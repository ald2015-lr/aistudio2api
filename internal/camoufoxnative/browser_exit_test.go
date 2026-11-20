package camoufoxnative

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeCamoufox 写一个代替 Camoufox 的 shell 脚本（忽略参数）并返回路径；Windows 上跳过
func fakeCamoufox(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("假 Camoufox 是 shell 脚本")
	}
	path := filepath.Join(t.TempDir(), "camoufox")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// lockedBuffer 为并发安全的日志转写目标
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

// TestBrowserExitBeforeEndpoint Camoufox 在报告 BiDi 端点前退出：立即失败（不等启动期限），错误写明退出码与最近 20 行输出，
// 输出去掉敏感内容（代理账号密码、查询参数、邮箱、密码）、不留网页脚本的控制台消息；设置了 Options.Log 时输出照常逐行原样转写过去（服务不设置它）
func TestBrowserExitBeforeEndpoint(t *testing.T) {
	executable := fakeCamoufox(t, `i=1
while [ $i -le 25 ]; do printf 'line-%02d\n' $i; i=$((i+1)); done
echo "JavaScript error: https://aistudio.google.com/app.js?authuser=0, line 1: boom"
echo "proxy http://user:secret@proxy.example.com:8080/path?token=abc"
echo "account lisa@example.com password=hunter2 cookie: SID=xyz"
echo "stderr-note" >&2
exit 3
`)
	mirror := &lockedBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	process, endpoint, err := launchBrowser(ctx, Options{ExecutablePath: executable, Headless: true, ReadyTimeout: 20 * time.Second, Log: mirror}, map[string]any{})
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("应在进程退出后立即失败，用了 %s", elapsed)
	}
	if process != nil || endpoint != "" {
		t.Fatalf("不应返回进程与端点: %v %q", process, endpoint)
	}
	var exited *BrowserExitedError
	if !errors.As(err, &exited) || exited.Stage != browserExitBeforeEndpoint || exited.Code != "退出码 3" || exited.Clean {
		t.Fatalf("应写明在报告端点前以退出码 3 退出: %#v %v", exited, err)
	}
	message := err.Error()
	if !strings.HasPrefix(message, "Camoufox 在报告 BiDi 端点前退出，退出码 3 | 最近输出=") || strings.Contains(message, "\n") {
		t.Fatalf("错误文案 = %q", message)
	}
	lines := strings.Split(exited.Output, browserOutputSeparator)
	if len(lines) != browserOutputLines {
		t.Fatalf("应留最近 %d 行，实际 %d 行: %q", browserOutputLines, len(lines), exited.Output)
	}
	for _, want := range []string{"line-25", "proxy http://***@proxy.example.com:8080/path?…", "account li***@*** password=*** cookie: ***"} {
		if !strings.Contains(exited.Output, want) {
			t.Fatalf("最近输出应包含 %q: %q", want, exited.Output)
		}
	}
	for _, leaked := range []string{"line-01", "line-06", "secret", "token=abc", "lisa@", "hunter2", "SID=xyz", "JavaScript error", "authuser"} {
		if strings.Contains(message, leaked) {
			t.Fatalf("错误里不应有 %q: %q", leaked, message)
		}
	}
	// 转写照旧：每行原样写到日志（包括最早的行与网页脚本消息）
	logged := mirror.String()
	for _, want := range []string{"line-01\n", "JavaScript error: https://aistudio.google.com/app.js?authuser=0", "stderr-note\n"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("日志转写应包含 %q: %q", want, logged)
		}
	}
}

// bidiTestServer 为只接受连接、不回复命令的假 BiDi 服务端：hangup 关闭后服务端断开连接（模拟浏览器退出时连接断开）
func bidiTestServer(t *testing.T) (*websocket.Conn, chan struct{}) {
	t.Helper()
	hangup := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		go func() {
			for {
				if _, _, err := connection.ReadMessage(); err != nil {
					return
				}
			}
		}()
		<-hangup
		_ = connection.Close()
	}))
	t.Cleanup(server.Close)
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection, hangup
}

// TestBrowserExitWhileRunning Camoufox 运行中被信号结束：BiDi 连接随之断开，断开的原因（Worker.Broken、进行中的命令）写明信号与
// 退出前最后几行输出，仍以“BiDi 连接已断开: ”开头
func TestBrowserExitWhileRunning(t *testing.T) {
	executable := fakeCamoufox(t, `echo "WebDriver BiDi listening on ws://127.0.0.1:9"
sleep 1
echo "Crash Annotation GraphicsCriticalError: |[0][GFX1-]: fake failure"
echo "Exiting due to channel error." >&2
kill -SEGV $$
`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	process, endpoint, err := launchBrowser(ctx, Options{ExecutablePath: executable, Headless: true, ReadyTimeout: 20 * time.Second}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Close() })
	if endpoint != "ws://127.0.0.1:9/session" {
		t.Fatalf("端点 = %q", endpoint)
	}
	connection, hangup := bidiTestServer(t)
	client := newBrowserBiDiClient(connection, process)
	pending := make(chan error, 1)
	go func() {
		_, err := client.command(ctx, "session.status", map[string]any{})
		pending <- err
	}()
	select {
	case <-process.exited():
	case <-time.After(15 * time.Second):
		t.Fatal("假 Camoufox 没有退出")
	}
	close(hangup)
	var commandErr error
	select {
	case commandErr = <-pending:
	case <-time.After(15 * time.Second):
		t.Fatal("连接断开后进行中的命令应结束")
	}
	broken := client.broken()
	for _, err := range []error{broken, commandErr} {
		var exited *BrowserExitedError
		if !errors.As(err, &exited) || exited.Stage != browserExitRunning || !strings.HasPrefix(exited.Code, "信号 ") || exited.Clean {
			t.Fatalf("应写明运行中被信号结束: %v", err)
		}
		message := err.Error()
		if !strings.HasPrefix(message, "BiDi 连接已断开: Camoufox 运行中退出，信号 ") ||
			!strings.Contains(message, "Exiting due to channel error.") || !strings.Contains(message, "GraphicsCriticalError") {
			t.Fatalf("错误文案 = %q", message)
		}
	}
}

// TestBrowserClosedByServiceNotReported 服务自己关闭浏览器（Close 已开始）时连接断开不算浏览器退出：原因照旧，不等进程退出
func TestBrowserClosedByServiceNotReported(t *testing.T) {
	executable := fakeCamoufox(t, `echo "WebDriver BiDi listening on ws://127.0.0.1:9"
exec sleep 30
`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	process, _, err := launchBrowser(ctx, Options{ExecutablePath: executable, Headless: true, ReadyTimeout: 20 * time.Second}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	connection, hangup := bidiTestServer(t)
	client := newBrowserBiDiClient(connection, process)
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	closed := time.Now()
	close(hangup)
	for client.broken() == nil {
		if time.Since(closed) > browserExitWait {
			t.Fatal("连接断开后应立即报告")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if broken := client.broken(); browserExitFailure(broken) != nil || strings.Contains(broken.Error(), "Camoufox") {
		t.Fatalf("服务关闭的浏览器不应报退出: %v", broken)
	}
	// 没有进程（测试的假连接）同样照旧
	if (*browserProcess)(nil).exitFailure(time.Second) != nil {
		t.Fatal("没有进程时不应报退出")
	}
}

// TestLoginBrowserExit 登录期间浏览器退出：退出码为 0（关了登录窗口）时说明登录没有完成，其余写明意外退出、退出码与输出；
// 其他错误不改
func TestLoginBrowserExit(t *testing.T) {
	clean := &BrowserExitedError{Stage: browserExitRunning, Code: "退出码 0", Clean: true}
	if err := loginBrowserExit(errors.Join(errors.New("读取隔离登录页面"), clean)); err == nil || err.Error() != "登录窗口已关闭，登录没有完成（Camoufox 退出码 0）" {
		t.Fatalf("关闭登录窗口 = %v", err)
	}
	crashed := &BrowserExitedError{Stage: browserExitRunning, Code: "信号 killed", Output: "Exiting due to channel error."}
	err := loginBrowserExit(errors.Join(errors.New("读取隔离登录页面"), crashed))
	if err == nil || !strings.HasPrefix(err.Error(), "登录浏览器意外退出: Camoufox 运行中退出，信号 killed | 最近输出=Exiting due to channel error.") ||
		browserExitFailure(err) != crashed {
		t.Fatalf("意外退出 = %v", err)
	}
	if loginBrowserExit(errors.New("其他")) != nil || loginBrowserExit(nil) != nil {
		t.Fatal("不是浏览器退出时不改")
	}
}

// TestSanitizeBrowserOutput 一行输出写进错误前去掉敏感内容：URL 与不带 scheme 的 user:password@host 的账号密码、查询参数与片段，
// 邮箱，Authorization、Cookie 等凭据头之后的整段，Bearer 令牌，密码、token、API Key 的值（含 JSON 写法）；控制字符换成空格，过长截断
func TestSanitizeBrowserOutput(t *testing.T) {
	cases := map[string]string{
		"socks5://alice:p4ss@127.0.0.1:1080":                      "socks5://***@127.0.0.1:1080",
		"GET https://accounts.google.com/x?email=a@example.com#f": "GET https://accounts.google.com/x?…",
		"ws://127.0.0.1:9222/session":                             "ws://127.0.0.1:9222/session",
		"user bob.smith@example.com logged":                       "user bo***@*** logged",
		"proxy_password=abc token: \"q w\" api_key=k1, ok":        "proxy_password=*** token: *** api_key=***, ok",
		"tab\there": "tab here",
		// 凭据头冒号之后整段遮住（Bearer 之后的令牌、Cookie 的第二项起原先原样保留）
		"V/nsHttp   Authorization: Bearer ya29.fakeTokenValue123": "V/nsHttp   Authorization: ***",
		"Cookie: SID=fakesid; HSID=fakehsid; SSID=fakessid":       "Cookie: ***",
		"set-cookie: __Secure-1PSID=fakepsid; Path=/; Secure":     "set-cookie: ***",
		"Proxy-Authorization: Basic ZmFrZTpmYWtl":                 "Proxy-Authorization: ***",
		// JSON 写法的键（键名之后先是引号）、带前缀的键名、不在 Authorization 头里的 Bearer 令牌
		`{"password":"fakepass","user":"x"}`:                       `{"password":***,"user":"x"}`,
		`{"cookie": "SID=fake", "n": 1}`:                           `{"cookie": ***`,
		"refresh_token=fakerefresh x-goog-api-key: AIzaFakeKey ok": "refresh_token=*** x-goog-api-key: *** ok",
		"sending Bearer ya29.fakeToken to host":                    "sending Bearer *** to host",
		// 不带 scheme 的 user:password@host（代理地址）
		"proxy user:fakepass@10.0.0.1:8080 failed":      "proxy ***@10.0.0.1:8080 failed",
		"connect alice:fakepass@proxy.example.com:3128": "connect ***@proxy.example.com:3128",
	}
	for input, want := range cases {
		if got := sanitizeBrowserOutput(input); got != want {
			t.Errorf("sanitizeBrowserOutput(%q) = %q，期望 %q", input, got, want)
		}
	}
	if got := sanitizeBrowserOutput(strings.Repeat("x", 500)); len([]rune(got)) != browserOutputLineLimit+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("过长的行应截到 %d 字: %d", browserOutputLineLimit, len([]rune(got)))
	}
	var output browserOutput
	for _, line := range []string{"", "   ", "JavaScript warning: https://aistudio.google.com/a.js, line 2: x", "JavaScript error: resource://gre/modules/x.sys.mjs, line 1: y"} {
		output.add(line)
	}
	if output.text() != "JavaScript error: resource://gre/modules/x.sys.mjs, line 1: y" {
		t.Fatalf("空行与网页脚本消息不留，浏览器自身脚本的照留: %q", output.text())
	}
}

// TestLoginBrowserExitedDuringStartup 浏览器登录时 Camoufox 还没报告端点就退出：退出码 0（窗口刚打开就被关掉）报登录窗口已关闭，
// 其他退出码报登录浏览器意外退出并带退出码与最近输出
func TestLoginBrowserExitedDuringStartup(t *testing.T) {
	for _, item := range []struct {
		name   string
		script string
		want   string
	}{
		{name: "关闭窗口", script: "exit 0\n", want: "登录窗口已关闭，登录没有完成（Camoufox 退出码 0）"},
		{name: "意外退出", script: "echo 'Gtk-WARNING: cannot open display' >&2\nexit 1\n",
			want: "登录浏览器意外退出: Camoufox 在报告 BiDi 端点前退出，退出码 1 | 最近输出=Gtk-WARNING: cannot open display"},
	} {
		t.Run(item.name, func(t *testing.T) {
			executable := fakeCamoufox(t, item.script)
			_, err := Login(context.Background(), LoginOptions{ExecutablePath: executable, Directory: t.TempDir(), Timeout: 20 * time.Second})
			if err == nil || err.Error() != item.want {
				t.Fatalf("错误 = %v，期望 %s", err, item.want)
			}
		})
	}
}

// waitBroken 等 client.broken() 报告断开，返回用时；limit 内没有报告时测试失败
func waitBroken(t *testing.T, client *bidiClient, limit time.Duration) time.Duration {
	t.Helper()
	started := time.Now()
	for client.broken() == nil {
		if time.Since(started) > limit {
			t.Fatalf("连接断开 %s 后 broken() 仍报告正常", limit)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return time.Since(started)
}

// TestBrowserBrokenReportedImmediately BiDi 连接一断开 broken() 就报告失败、新命令立即失败（不等 Camoufox 退出）：浏览器进程仍在运行
// 时原先要等满 browserExitWait（5 秒）才报告，这段时间 Worker 仍显示就绪、照常接新请求，命令超时还被当成浏览器暂时繁忙。
// 进行中的命令照旧等浏览器退出的诊断：进程随后退出的，断开原因换成写明退出码与最近输出的错误；等进程退出与等输出读完合计不超过
// browserExitWait（浏览器的子进程还占着输出管道时，已读到的行照样写进错误）
func TestBrowserBrokenReportedImmediately(t *testing.T) {
	t.Run("浏览器仍在运行", func(t *testing.T) {
		executable := fakeCamoufox(t, `echo "WebDriver BiDi listening on ws://127.0.0.1:9"
exec sleep 30
`)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		process, _, err := launchBrowser(ctx, Options{ExecutablePath: executable, Headless: true, ReadyTimeout: 20 * time.Second}, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = process.Close() })
		connection, hangup := bidiTestServer(t)
		client := newBrowserBiDiClient(connection, process)
		close(hangup)
		waitBroken(t, client, time.Second)
		started := time.Now()
		if _, err := client.command(ctx, "session.status", map[string]any{}); err == nil || !strings.HasPrefix(err.Error(), "BiDi 连接已断开: ") {
			t.Fatalf("断开后的新命令应以断开原因失败: %v", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("断开后的新命令应立即失败，用了 %s", elapsed)
		}
	})
	t.Run("主进程退出而子进程占着输出", func(t *testing.T) {
		// 连接断开 2 秒后主进程才被信号结束，它启动的子进程一直占着 stdout（输出读不完）
		signal := filepath.Join(t.TempDir(), "hangup")
		executable := fakeCamoufox(t, `echo "WebDriver BiDi listening on ws://127.0.0.1:9"
sleep 30 &
while [ ! -f '`+signal+`' ]; do sleep 0.05; done
sleep 2
echo "Exiting due to channel error." >&2
kill -SEGV $$
`)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		process, _, err := launchBrowser(ctx, Options{ExecutablePath: executable, Headless: true, ReadyTimeout: 20 * time.Second}, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = process.Close() })
		connection, hangup := bidiTestServer(t)
		client := newBrowserBiDiClient(connection, process)
		pending := make(chan error, 1)
		go func() {
			_, err := client.command(ctx, "session.status", map[string]any{})
			pending <- err
		}()
		time.Sleep(100 * time.Millisecond)
		if err := os.WriteFile(signal, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		close(hangup)
		started := time.Now()
		waitBroken(t, client, time.Second)
		var commandErr error
		select {
		case commandErr = <-pending:
		case <-time.After(15 * time.Second):
			t.Fatal("连接断开后进行中的命令应结束")
		}
		if elapsed := time.Since(started); elapsed > browserExitWait+time.Second {
			t.Fatalf("等进程退出与等输出读完合计不应超过 %s，用了 %s", browserExitWait, elapsed)
		}
		message := commandErr.Error()
		if !strings.HasPrefix(message, "BiDi 连接已断开: Camoufox 运行中退出，信号 ") || !strings.Contains(message, "Exiting due to channel error.") {
			t.Fatalf("进行中的命令应写明退出信号与最近输出: %q", message)
		}
		if broken := client.broken(); broken == nil || broken.Error() != message {
			t.Fatalf("broken() 随后应换成同一个写明退出的原因: %v", broken)
		}
	})
}
