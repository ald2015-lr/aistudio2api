package aistudio

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	appconfig "github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

// 测试用的代理账号密码：断言它们不出现在错误里
const (
	relayTestUser     = "user-session-XYZ"
	relayTestPassword = "S3cr3t-PW"
)

// relayHostPort 取中转地址里的 127.0.0.1:N
func relayHostPort(t *testing.T, address string) string {
	t.Helper()
	parsed, err := url.Parse(address)
	if err != nil || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
		t.Fatalf("中转地址 %q 不是 127.0.0.1:N", address)
	}
	return parsed.Host
}

// listenerAlive 判断中转监听器是否还在接受连接；连上后立即关闭
func listenerAlive(hostPort string) bool {
	connection, err := net.DialTimeout("tcp", hostPort, time.Second)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

// TestNativeWorkerReleasesRelayAfterClose Worker 的中转租约只在浏览器确认退出（Close 返回 nil）后释放：
// 第一次关闭失败时租约保留，超过空闲期限监听器仍在；第二次关闭成功后监听器按活动判定关闭；第三次关闭不重复释放
func TestNativeWorkerReleasesRelayAfterClose(t *testing.T) {
	const grace = 50 * time.Millisecond
	registry := proxyrelay.NewRegistry(proxyrelay.Options{IdleGrace: grace})
	t.Cleanup(func() { _ = registry.Close() })
	lease, err := registry.Acquire("http://"+relayTestUser+":"+relayTestPassword+"@127.0.0.1:9", "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Relayed() {
		t.Fatal("带认证的代理应经本机中转")
	}
	hostPort := relayHostPort(t, lease.Address())
	worker, closed := newStubWorker("a@example.com", &stubRuntime{closeFailures: 1})
	worker.relay = lease

	if err := worker.Close(); err == nil {
		t.Fatal("第一次关闭应失败")
	}
	time.Sleep(4 * grace)
	if !listenerAlive(hostPort) {
		t.Fatal("关闭失败时不应释放租约，监听器应保留")
	}
	time.Sleep(4 * grace)
	if !listenerAlive(hostPort) {
		t.Fatal("关闭失败后监听器不应进入空闲判定")
	}

	if err := worker.Close(); err != nil {
		t.Fatalf("第二次关闭应成功: %v", err)
	}
	if closed() != 1 {
		t.Fatalf("成功关闭次数 = %d", closed())
	}
	deadline := time.Now().Add(5 * time.Second)
	for listenerAlive(hostPort) {
		if time.Now().After(deadline) {
			t.Fatal("关闭成功后监听器应在空闲期限后关闭")
		}
		time.Sleep(grace)
	}
	if err := worker.Close(); err != nil {
		t.Fatalf("第三次关闭: %v", err)
	}
	if listenerAlive(hostPort) {
		t.Fatal("监听器关闭后不应再出现")
	}
}

// TestNewNativeWorkerRelayStartError 带认证的代理在启动 Camoufox 前申请中转（使用者为账户 ID），Camoufox 起不来时
// 原错误照常返回，错误与中转日志都不含账号密码
func TestNewNativeWorkerRelayStartError(t *testing.T) {
	recorder := &relayLogRecorder{}
	proxyrelay.SetLogger(recorder.log)
	t.Cleanup(func() { proxyrelay.SetLogger(nil) })
	options := camoufoxnative.Options{Proxy: "http://" + relayTestUser + ":" + relayTestPassword + "@127.0.0.1:9"}
	worker, err := NewNativeWorker(context.Background(), "a@example.com", options)
	if err == nil || worker != nil {
		t.Fatal("缺少认证状态时 Worker 应启动失败")
	}
	text := recorder.text()
	if !strings.Contains(text, "本机代理中转启动 | 上游=http://127.0.0.1:9 | 本机=127.0.0.1:") || !strings.Contains(text, "使用者=a@example.com") {
		t.Fatalf("启动 Camoufox 前应申请中转，日志: %s", text)
	}
	for _, secret := range []string{relayTestUser, relayTestPassword} {
		if strings.Contains(err.Error(), secret) || strings.Contains(text, secret) {
			t.Fatalf("错误或日志含账号密码: %v / %s", err, text)
		}
	}
}

// relayLogRecorder 收集包级默认中转注册表的日志
type relayLogRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (recorder *relayLogRecorder) log(level, message string) {
	recorder.mu.Lock()
	recorder.lines = append(recorder.lines, level+" "+message)
	recorder.mu.Unlock()
}

func (recorder *relayLogRecorder) text() string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return strings.Join(recorder.lines, "\n")
}

// TestNativeLoginRelayOptions 带认证的代理在登录参数里换成本机中转地址（http 上游给 http://127.0.0.1:N，socks5 上游给
// socks5://127.0.0.1:N）；不带认证时原样返回、不起监听；中转解析不了的值原样交给 Camoufox，由它按原规则报错；
// 使用者为“登录:新账户”“验证:账户 ID”
func TestNativeLoginRelayOptions(t *testing.T) {
	recorder := &relayLogRecorder{}
	proxyrelay.SetLogger(recorder.log)
	t.Cleanup(func() { proxyrelay.SetLogger(nil) })
	driver := &NativeLoginDriver{camoufox: "/opt/camoufox/camoufox", timeout: time.Minute}

	httpUpstream := "http://" + relayTestUser + ":" + relayTestPassword + "@127.0.0.1:9"
	options, relay, err := driver.relayOptions(IsolatedLoginRequest{AccountID: "new", Proxy: httpUpstream, Locale: "en-US"}, "登录")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Release()
	if !relay.Relayed() || !regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+$`).MatchString(options.Proxy) {
		t.Fatalf("http 上游应换成 http://127.0.0.1:N，得到 %q", options.Proxy)
	}
	if options.ExecutablePath != driver.camoufox || options.Locale != "en-US" || options.Timeout != time.Minute {
		t.Fatalf("其余登录参数应原样透传: %+v", options)
	}
	if !strings.Contains(recorder.text(), "使用者=登录:新账户") {
		t.Fatalf("新账户登录的使用者应为“登录:新账户”，日志: %s", recorder.text())
	}

	socksUpstream := "socks5://" + relayTestUser + ":" + relayTestPassword + "@127.0.0.1:9"
	options, socksRelay, err := driver.relayOptions(IsolatedLoginRequest{AccountID: "a@example.com", Proxy: socksUpstream}, "验证")
	if err != nil {
		t.Fatal(err)
	}
	defer socksRelay.Release()
	if !socksRelay.Relayed() || !regexp.MustCompile(`^socks5://127\.0\.0\.1:[0-9]+$`).MatchString(options.Proxy) {
		t.Fatalf("socks5 上游应换成 socks5://127.0.0.1:N，得到 %q", options.Proxy)
	}
	if !strings.Contains(recorder.text(), "使用者=验证:a@example.com") {
		t.Fatalf("验证的使用者应为“验证:账户 ID”，日志: %s", recorder.text())
	}

	for _, plain := range []string{"", "http://127.0.0.1:7890", "HTTP://Proxy.example.com:8080/", "socks5://[::1]:1080"} {
		options, plainRelay, err := driver.relayOptions(IsolatedLoginRequest{AccountID: "setup", Proxy: plain}, "登录")
		if err != nil {
			t.Fatal(err)
		}
		if plainRelay.Relayed() || options.Proxy != plain {
			t.Fatalf("不带认证的 %q 应原样交给 Camoufox，得到 %q（中转 %v）", plain, options.Proxy, plainRelay.Relayed())
		}
		plainRelay.Release()
	}

	// 中转不支持的写法（例如带账号密码的 socks4）不起中转、原值交给 Camoufox：它会按原规则以固定文案拒绝
	options, invalid, err := driver.relayOptions(IsolatedLoginRequest{AccountID: "a@example.com", Proxy: "socks4://u:p@h:1080"}, "登录")
	if err != nil || invalid.Relayed() || options.Proxy != "socks4://u:p@h:1080" {
		t.Fatalf("中转解析不了的值应原样交给 Camoufox，得到 %q（%v）", options.Proxy, err)
	}
	if text := recorder.text(); strings.Contains(text, relayTestUser) || strings.Contains(text, relayTestPassword) {
		t.Fatalf("中转日志含账号密码: %s", text)
	}
}

// fakeConnectProxy 为回环上的假 HTTP 代理：记录每个 CONNECT 请求，统一回 status
type fakeConnectProxy struct {
	listener net.Listener
	mu       sync.Mutex
	requests []*http.Request
}

func newFakeConnectProxy(t *testing.T, status string) *fakeConnectProxy {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &fakeConnectProxy{listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				request, err := http.ReadRequest(bufio.NewReader(connection))
				if err != nil {
					return
				}
				proxy.mu.Lock()
				proxy.requests = append(proxy.requests, request)
				proxy.mu.Unlock()
				_, _ = io.WriteString(connection, "HTTP/1.1 "+status+"\r\nProxy-Authenticate: Basic realm=\"proxy\"\r\nContent-Length: 0\r\n\r\n")
			}()
		}
	}()
	return proxy
}

func (proxy *fakeConnectProxy) first(t *testing.T) *http.Request {
	t.Helper()
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if len(proxy.requests) == 0 {
		t.Fatal("假代理没有收到请求")
	}
	return proxy.requests[0]
}

// TestProxyHTTPClientAuthenticatedConnect Go 侧直连带认证的 HTTP 上游（只写测试，不改 transport_browser.go）：
// CONNECT 带 Basic base64(用户名:原文密码)，目标为 host:443；上游回 407 时错误含“代理 CONNECT 返回 407”，不含账号密码
func TestProxyHTTPClientAuthenticatedConnect(t *testing.T) {
	fake := newFakeConnectProxy(t, "407 Proxy Authentication Required")
	password := relayTestPassword + ":p@ss/w?rd#x %y"
	upstream, err := appconfig.NormalizeProxy(fake.listener.Addr().String() + ":" + relayTestUser + ":" + password)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewProxyHTTPClient(upstream)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://alkalimakersuite-pa.clients6.google.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err == nil {
		response.Body.Close()
		t.Fatal("上游回 407 时请求应失败")
	}
	if !strings.Contains(err.Error(), "代理 CONNECT 返回 407") {
		t.Fatalf("错误应说明 407，得到 %v", err)
	}
	basic := base64.StdEncoding.EncodeToString([]byte(relayTestUser + ":" + password))
	for _, secret := range []string{relayTestUser, relayTestPassword, url.PathEscape(password), basic} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("错误含账号密码: %v", err)
		}
	}
	connect := fake.first(t)
	if connect.Method != http.MethodConnect || connect.Host != "alkalimakersuite-pa.clients6.google.com:443" {
		t.Fatalf("假代理收到 %s %s", connect.Method, connect.Host)
	}
	if got := connect.Header.Get("Proxy-Authorization"); got != "Basic "+basic {
		t.Fatalf("Proxy-Authorization = %q，期望原文密码的 Basic", got)
	}
}

// TestProxyHTTPClientAuthenticatedSOCKS5 Go 侧直连带认证的 SOCKS5 上游：假服务端校验 RFC1929 用户名和原文密码，
// 之后回 REP=05；错误里不含账号密码
func TestProxyHTTPClientAuthenticatedSOCKS5(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	password := relayTestPassword + ":p@ss/w?rd#x"
	credentials := make(chan string, 4)
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go serveFakeSOCKS5(connection, credentials)
		}
	}()
	upstream, err := appconfig.NormalizeProxy("socks5://" + listener.Addr().String() + ":" + relayTestUser + ":" + password)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewProxyHTTPClient(upstream)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://aistudio.google.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err == nil {
		response.Body.Close()
		t.Fatal("上游回 REP=05 时请求应失败")
	}
	for _, secret := range []string{relayTestUser, relayTestPassword, url.PathEscape(password)} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("错误含账号密码: %v", err)
		}
	}
	if got := proxyrelay.Describe(err, upstream); !strings.HasPrefix(got, "上游 SOCKS5 代理连接目标失败：05") {
		t.Fatalf("REP=05 应说成上游连目标被拒，得到 %q（%v）", got, err)
	}
	select {
	case got := <-credentials:
		if want := relayTestUser + "\x00" + password + "\x00aistudio.google.com:443"; got != want {
			t.Fatalf("假 SOCKS5 收到 %q，期望 %q", got, want)
		}
	default:
		t.Fatal("假 SOCKS5 没有收到认证")
	}
}

// serveFakeSOCKS5 为要求账号密码的最小 SOCKS5 服务端：认证通过后对 CONNECT 回 REP=05
func serveFakeSOCKS5(connection net.Conn, credentials chan<- string) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	header := make([]byte, 2)
	if _, err := io.ReadFull(connection, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, header[1])
	if _, err := io.ReadFull(connection, methods); err != nil || !strings.Contains(string(methods), "\x02") {
		_, _ = connection.Write([]byte{5, 0xff})
		return
	}
	_, _ = connection.Write([]byte{5, 2})
	readField := func() (string, bool) {
		length := make([]byte, 1)
		if _, err := io.ReadFull(connection, length); err != nil {
			return "", false
		}
		value := make([]byte, length[0])
		if _, err := io.ReadFull(connection, value); err != nil {
			return "", false
		}
		return string(value), true
	}
	version := make([]byte, 1)
	if _, err := io.ReadFull(connection, version); err != nil || version[0] != 1 {
		return
	}
	user, ok := readField()
	if !ok {
		return
	}
	password, ok := readField()
	if !ok {
		return
	}
	_, _ = connection.Write([]byte{1, 0})
	request := make([]byte, 4)
	if _, err := io.ReadFull(connection, request); err != nil || request[1] != 1 {
		return
	}
	var host string
	switch request[3] {
	case 3:
		name, ok := readField()
		if !ok {
			return
		}
		host = name
	case 1:
		address := make([]byte, 4)
		if _, err := io.ReadFull(connection, address); err != nil {
			return
		}
		host = net.IP(address).String()
	default:
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(connection, port); err != nil {
		return
	}
	credentials <- user + "\x00" + password + "\x00" + net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1])))
	_, _ = connection.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
}

// TestNormalizedCredentialProxyHasHost 规范化后带认证的代理一定有主机：tls-client（chromeauth 导入、续签）在 URL 没有 Host 时
// 会把整个代理 URL 拼进错误（connect.go:105），带出账号密码
func TestNormalizedCredentialProxyHasHost(t *testing.T) {
	inputs := []string{
		"1.2.3.4:8080:" + relayTestUser + ":" + relayTestPassword,
		relayTestUser + ":" + relayTestPassword + "@1.2.3.4:8080",
		"http://" + relayTestUser + ":" + relayTestPassword + "@h.example",
		"https://" + relayTestUser + "@h.example",
		"socks5://[2001:db8::1]:1080:" + relayTestUser + ":" + relayTestPassword,
		"socks5h://" + relayTestUser + ":" + relayTestPassword + "@h.example:1080",
		"http://1.2.3.4:8080:" + relayTestUser + ":12345",
	}
	for _, input := range inputs {
		normalized, err := appconfig.NormalizeProxy(input)
		if err != nil {
			t.Fatalf("%q: %v", input, err)
		}
		parsed, err := url.Parse(normalized)
		if err != nil || parsed.User == nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.Port() == "" {
			t.Fatalf("%q 规范化为 %q，应带认证、主机与端口", input, normalized)
		}
		if strings.Contains(parsed.Hostname(), ":") && !strings.Contains(parsed.Host, "[") {
			t.Fatalf("%q 规范化为 %q，主机名不应带冒号", input, normalized)
		}
	}
}
