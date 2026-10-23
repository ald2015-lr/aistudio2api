package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

// relayAddressPattern 为本机中转地址
var relayAddressPattern = regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+$`)

// recordingProxyProbe 为假的出口 IP 探测：记下每条路径拿到的代理地址，按 answer 返回结果
type recordingProxyProbe struct {
	mu      sync.Mutex
	proxies []string
	answer  func(ctx context.Context, proxy string) api.ProxyTestProbe
}

func (probe *recordingProxyProbe) probe(ctx context.Context, proxy string) api.ProxyTestProbe {
	probe.mu.Lock()
	probe.proxies = append(probe.proxies, proxy)
	probe.mu.Unlock()
	return probe.answer(ctx, proxy)
}

// split 返回本机中转路径与直连上游路径分别拿到的代理地址
func (probe *recordingProxyProbe) split() ([]string, []string) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	var relayed, direct []string
	for _, proxy := range probe.proxies {
		if relayAddressPattern.MatchString(proxy) {
			relayed = append(relayed, proxy)
		} else {
			direct = append(direct, proxy)
		}
	}
	return relayed, direct
}

func (probe *recordingProxyProbe) reset() {
	probe.mu.Lock()
	probe.proxies = nil
	probe.mu.Unlock()
}

// newProxyTestManager 创建只用于“测试代理”的管理器：中转注册表与探测都换成测试替身
func newProxyTestManager(t *testing.T, defaultProxy string, probe *recordingProxyProbe) (*runtimeManager, *proxyrelay.Registry) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	requests := newRequestRegistry(ctx)
	// 中转日志与生产一样写进管理页日志（service 来源），assertNoProxySecretInLogs 一并检查
	registry := proxyrelay.NewRegistry(proxyrelay.Options{
		UpstreamTimeout: 200 * time.Millisecond, IdleGrace: 50 * time.Millisecond,
		Log: func(level, message string) { requests.log("service", level, message) },
	})
	t.Cleanup(func() { _ = registry.Close() })
	manager := &runtimeManager{
		requests:    requests,
		current:     &runtimeGeneration{config: config.Config{Proxy: defaultProxy}},
		probeProxy:  probe.probe,
		proxyRelays: registry,
	}
	return manager, registry
}

func fixedProbe(ip string) func(context.Context, string) api.ProxyTestProbe {
	return func(context.Context, string) api.ProxyTestProbe {
		return api.ProxyTestProbe{IP: ip, ElapsedMS: 12}
	}
}

// TestTestProxy 带认证时浏览器路径拿到本机中转地址、后台请求路径拿到上游；不带认证时两者相同且不经中转；
// 出口 IP 不同时提示轮换；https 无认证提示明文；并发时第二个返回 409；留空时测试生成服务的默认代理，两者都为空时 400；
// 结果、错误与日志都不含账号密码
func TestTestProxy(t *testing.T) {
	probe := &recordingProxyProbe{answer: fixedProbe("203.0.113.7")}
	manager, _ := newProxyTestManager(t, "", probe)
	ctx := context.Background()
	input := "1.2.3.4:8080:" + testProxyUser + ":" + testProxyPassword
	upstream := "http://" + testProxyUser + ":" + testProxyPassword + "@1.2.3.4:8080"

	result, err := manager.TestProxy(ctx, api.ProxyTestInput{Proxy: input})
	if err != nil {
		t.Fatal(err)
	}
	relayed, direct := probe.split()
	if len(relayed) != 1 || len(direct) != 1 || direct[0] != upstream {
		t.Fatalf("浏览器路径应拿到本机中转地址、后台请求路径应拿到上游，得到 中转=%v 直连=%v", relayed, direct)
	}
	if result.Proxy != "http://1.2.3.4:8080" || !result.Authenticated || !result.Relayed || !result.SameIP ||
		result.Browser.IP != "203.0.113.7" || result.Direct.IP != "203.0.113.7" || len(result.Warnings) != 0 {
		t.Fatalf("结果 = %+v", result)
	}
	encoded, _ := json.Marshal(result)
	assertNoProxySecret(t, "结果", string(encoded))
	logs := proxyTestLogs(manager.requests)
	if len(logs) != 1 || logs[0].Level != "INFO" || logs[0].Source != "service" ||
		logs[0].Message != "代理测试 | 代理=http://1.2.3.4:8080 | 认证=是 | 浏览器路径=203.0.113.7（12ms） | 后台请求路径=203.0.113.7（12ms）" {
		t.Fatalf("日志 = %+v", logs)
	}

	// 不带认证：两条路径都直连上游，不经中转
	probe.reset()
	result, err = manager.TestProxy(ctx, api.ProxyTestInput{Proxy: " http://10.1.2.3:7890 "})
	if err != nil {
		t.Fatal(err)
	}
	relayed, direct = probe.split()
	if len(relayed) != 0 || len(direct) != 2 || direct[0] != "http://10.1.2.3:7890" || direct[1] != "http://10.1.2.3:7890" {
		t.Fatalf("不带认证时两条路径都应拿到原值，得到 中转=%v 直连=%v", relayed, direct)
	}
	if result.Relayed || result.Authenticated || result.Proxy != "http://10.1.2.3:7890" {
		t.Fatalf("不带认证的结果 = %+v", result)
	}

	// 两条连接出口 IP 不同：按连接轮换的动态代理
	probe.answer = func(_ context.Context, proxy string) api.ProxyTestProbe {
		if relayAddressPattern.MatchString(proxy) {
			return api.ProxyTestProbe{IP: "203.0.113.7"}
		}
		return api.ProxyTestProbe{IP: "198.51.100.9"}
	}
	result, err = manager.TestProxy(ctx, api.ProxyTestInput{Proxy: input})
	if err != nil {
		t.Fatal(err)
	}
	if result.SameIP || len(result.Warnings) != 1 || result.Warnings[0] != "rotating_ip" {
		t.Fatalf("出口 IP 不同时应提示 rotating_ip，得到 %+v", result)
	}

	// https 不带认证：浏览器以明文 HTTP 代理方式连接
	probe.answer = fixedProbe("203.0.113.7")
	result, err = manager.TestProxy(ctx, api.ProxyTestInput{Proxy: "https://proxy.example:443"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Relayed || len(result.Warnings) != 1 || result.Warnings[0] != "https_plain_browser" {
		t.Fatalf("https 无认证应提示 https_plain_browser，得到 %+v", result)
	}
	// https 带认证经本机中转，由中转负责 TLS，不提示
	result, err = manager.TestProxy(ctx, api.ProxyTestInput{Proxy: "https://u:p@proxy.example:443"})
	if err != nil || !result.Relayed || len(result.Warnings) != 0 {
		t.Fatalf("https 带认证的结果 = %+v (%v)", result, err)
	}

	// 一边失败记 WARN
	probe.answer = func(_ context.Context, proxy string) api.ProxyTestProbe {
		if relayAddressPattern.MatchString(proxy) {
			return api.ProxyTestProbe{IP: "203.0.113.7", ElapsedMS: 5}
		}
		return api.ProxyTestProbe{Error: "上游代理拒绝了账号密码（407），请检查用户名和密码", ElapsedMS: 6}
	}
	if _, err := manager.TestProxy(ctx, api.ProxyTestInput{Proxy: input}); err != nil {
		t.Fatal(err)
	}
	logs = proxyTestLogs(manager.requests)
	last := logs[len(logs)-1]
	if last.Level != "WARN" || !strings.HasSuffix(last.Message, "| 浏览器路径=203.0.113.7（5ms） | 后台请求路径=失败：上游代理拒绝了账号密码（407），请检查用户名和密码") {
		t.Fatalf("一边失败的日志 = %+v", last)
	}

	// 并发：第二个返回 409
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	probe.answer = func(context.Context, string) api.ProxyTestProbe {
		entered <- struct{}{}
		<-release
		return api.ProxyTestProbe{IP: "203.0.113.7"}
	}
	first := make(chan error, 1)
	go func() {
		_, err := manager.TestProxy(ctx, api.ProxyTestInput{Proxy: input})
		first <- err
	}()
	<-entered
	_, err = manager.TestProxy(ctx, api.ProxyTestInput{Proxy: input})
	assertAdminError(t, err, http.StatusConflict, "proxy_test_busy")
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("第一个测试应成功: %v", err)
	}

	// 留空：测试生成服务的默认代理
	probe.answer = fixedProbe("203.0.113.7")
	probe.reset()
	manager.current.config.Proxy = "http://10.0.0.1:3128"
	result, err = manager.TestProxy(ctx, api.ProxyTestInput{Proxy: "  "})
	if err != nil {
		t.Fatal(err)
	}
	if _, direct := probe.split(); len(direct) != 2 || direct[0] != "http://10.0.0.1:3128" || result.Proxy != "http://10.0.0.1:3128" {
		t.Fatalf("留空时应测试默认代理，得到 %v / %+v", direct, result)
	}
	manager.current.config.Proxy = ""
	_, err = manager.TestProxy(ctx, api.ProxyTestInput{})
	assertAdminError(t, err, http.StatusBadRequest, "proxy_required")

	// 不合法：400 invalid_config，文案不含账号密码
	_, err = manager.TestProxy(ctx, api.ProxyTestInput{Proxy: "ftp://" + testProxyUser + ":" + testProxyPassword + "@h:1"})
	message := assertAdminError(t, err, http.StatusBadRequest, "invalid_config")
	assertNoProxySecret(t, "错误", message)

	assertNoProxySecretInLogs(t, manager.requests)
}

// TestTestProxyUsesRelayFailure 上游接受连接却不回复：浏览器路径等到超过中转的 UpstreamTimeout 才放弃时，
// 结果里是中转记下的原因（连接上游代理超时），而不是浏览器路径看到的期限错误
func TestTestProxyUsesRelayFailure(t *testing.T) {
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		for {
			connection, err := upstream.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				// 读到中转关闭为止，不回任何字节
				buffer := make([]byte, 4096)
				for {
					if _, err := connection.Read(buffer); err != nil {
						return
					}
				}
			}()
		}
	}()
	address := upstream.Addr().String()
	probe := &recordingProxyProbe{}
	probe.answer = func(ctx context.Context, proxy string) api.ProxyTestProbe {
		if !relayAddressPattern.MatchString(proxy) {
			return api.ProxyTestProbe{Error: "后台请求路径失败"}
		}
		// 浏览器路径：经中转 CONNECT 出口 IP 服务，一直等到中转回复（上游超时后中转回 504），再按期限错误返回
		parsed, err := url.Parse(proxy)
		if err != nil {
			return api.ProxyTestProbe{Error: err.Error()}
		}
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", parsed.Host)
		if err != nil {
			return api.ProxyTestProbe{Error: err.Error()}
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(connection, "CONNECT api.ipify.org:443 HTTP/1.1\r\nHost: api.ipify.org:443\r\n\r\n")
		response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodConnect})
		if err != nil || response.StatusCode != http.StatusGatewayTimeout {
			return api.ProxyTestProbe{Error: fmt.Sprintf("中转应回 504，得到 %v (%v)", response, err)}
		}
		return api.ProxyTestProbe{Error: context.DeadlineExceeded.Error()}
	}
	manager, _ := newProxyTestManager(t, "", probe)
	result, err := manager.TestProxy(context.Background(), api.ProxyTestInput{
		Proxy: "http://" + testProxyUser + ":" + testProxyPassword + "@" + address,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "连接上游代理 " + address + " 超时"; result.Browser.Error != want {
		t.Fatalf("浏览器路径错误 = %q，期望中转记下的 %q", result.Browser.Error, want)
	}
	if result.Direct.Error != "后台请求路径失败" || !result.Relayed {
		t.Fatalf("结果 = %+v", result)
	}
	// 中转自己的日志也进了管理页：启动行与超时的失败行，只写 scheme://host:port 与使用者“代理测试”
	manager.requests.mu.Lock()
	var relayLogs []string
	for _, entry := range manager.requests.logs {
		if entry.Source == "service" && !strings.HasPrefix(entry.Message, "代理测试 | ") {
			relayLogs = append(relayLogs, entry.Level+" "+entry.Message)
		}
	}
	manager.requests.mu.Unlock()
	joined := strings.Join(relayLogs, "\n")
	if !strings.Contains(joined, "INFO 本机代理中转启动 | 上游=http://"+address+" | ") ||
		!strings.Contains(joined, "WARN 上游代理连接失败 | 上游=http://"+address+" | ") ||
		!strings.Contains(joined, "使用者=代理测试") {
		t.Fatalf("中转日志 = %s", joined)
	}
	assertNoProxySecretInLogs(t, manager.requests)
}

// TestProbeBodyParsing 出口 IP 服务的响应：{"ip":"203.0.113.7"} 解析成功；非 200、非 JSON、IP 不合法、超过 4 KiB 都给出说明，
// 说明是固定文案，不会被当成代理错误归类（例如 HTTP 407 不说成上游代理拒绝账号密码）
func TestProbeBodyParsing(t *testing.T) {
	ip, err := parseProxyProbeBody(http.StatusOK, strings.NewReader(`{"ip":"203.0.113.7"}`))
	if err != nil || ip != "203.0.113.7" {
		t.Fatalf("解析 = %q (%v)", ip, err)
	}
	if ip, err := parseProxyProbeBody(http.StatusOK, strings.NewReader(`{"ip":" 2001:db8::1 "}`)); err != nil || ip != "2001:db8::1" {
		t.Fatalf("IPv6 解析 = %q (%v)", ip, err)
	}
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"非 200", http.StatusBadGateway, `{"ip":"203.0.113.7"}`, "出口 IP 服务返回 HTTP 502"},
		{"407 不当成代理认证失败", http.StatusProxyAuthRequired, ``, "出口 IP 服务返回 HTTP 407"},
		{"非 JSON", http.StatusOK, `<html>blocked</html>`, "出口 IP 服务的响应不是 JSON"},
		{"IP 不合法", http.StatusOK, `{"ip":"not-an-ip"}`, "出口 IP 服务返回的 IP 无效"},
		{"缺少 IP", http.StatusOK, `{}`, "出口 IP 服务返回的 IP 无效"},
		{"超过 4 KiB", http.StatusOK, `{"ip":"203.0.113.7","pad":"` + strings.Repeat("x", 5000) + `"}`, "出口 IP 服务的响应超过 4 KiB"},
	}
	for _, testCase := range cases {
		_, err := parseProxyProbeBody(testCase.status, strings.NewReader(testCase.body))
		if err == nil {
			t.Fatalf("%s: 应失败", testCase.name)
		}
		if got := describeProbeError(err, "http://u:p@1.2.3.4:8080"); got != testCase.want {
			t.Fatalf("%s: 说明 = %q，期望 %q", testCase.name, got, testCase.want)
		}
	}
	if got := limitProbeError(strings.Repeat("长", 300)); len([]rune(got)) != proxyProbeErrorLimit || !strings.HasSuffix(got, "…") {
		t.Fatalf("说明应截到 %d 个字符，得到 %d", proxyProbeErrorLimit, len([]rune(got)))
	}
}

// TestProbeProxyExitDescribes407 真实的探测函数经假 HTTP 代理（只在回环上，回 407）：说明是 407 的中文原因，不含账号密码；
// 假代理收到的 CONNECT 带着原文密码的 Basic 认证
func TestProbeProxyExitDescribes407(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	authorization := make(chan string, 16)
	go func() {
		// 每条连接都回 407：客户端若重试，也不会卡在没人应答的连接上
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				request, err := http.ReadRequest(bufio.NewReader(connection))
				if err != nil {
					authorization <- "读取请求失败: " + err.Error()
					return
				}
				authorization <- request.Method + " " + request.Host + " " + request.Header.Get("Proxy-Authorization")
				_, _ = connection.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"x\"\r\nContent-Length: 0\r\n\r\n"))
			}()
		}
	}()
	proxy, err := config.NormalizeProxy(listener.Addr().String() + ":" + testProxyUser + ":" + testProxyPassword + ":x")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	probe := probeProxyExit(ctx, proxy)
	if probe.IP != "" || probe.Error != "上游代理拒绝了账号密码（407），请检查用户名和密码" {
		t.Fatalf("探测结果 = %+v", probe)
	}
	assertNoProxySecret(t, "探测说明", probe.Error)
	select {
	case got := <-authorization:
		want := "CONNECT api.ipify.org:443 Basic " + basicCredentials(testProxyUser, testProxyPassword+":x")
		if got != want {
			t.Fatalf("假代理收到 %q，期望 %q", got, want)
		}
	case <-ctx.Done():
		t.Fatal(errors.New("假代理没有收到 CONNECT"))
	}
}

// proxyTestLogs 返回管理页日志里“代理测试”的那些行
func proxyTestLogs(registry *requestRegistry) []api.AdminLog {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	var logs []api.AdminLog
	for _, entry := range registry.logs {
		if strings.HasPrefix(entry.Message, "代理测试 | ") {
			logs = append(logs, entry)
		}
	}
	return logs
}

// basicCredentials 为 Basic 认证的 base64(用户名:密码)
func basicCredentials(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}

// TestProbeDoesNotFollowRedirect 探测不跟随跳转：出口 IP 服务回 3xx 时报“出口 IP 服务返回 HTTP 302”，不会访问跳转目标
func TestProbeDoesNotFollowRedirect(t *testing.T) {
	var followed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/elsewhere" {
			followed.Store(true)
			_, _ = writer.Write([]byte(`{"ip":"203.0.113.7"}`))
			return
		}
		http.Redirect(writer, request, "/elsewhere", http.StatusFound)
	}))
	defer server.Close()
	probeClient, err := newProxyProbeClient("http://u:p@127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	// 只借用探测 client 的跳转策略，连接走回环上的假服务
	client := &http.Client{CheckRedirect: probeClient.CheckRedirect}
	ip, err := fetchProbeURL(context.Background(), client, server.URL+"/?format=json")
	if ip != "" || err == nil {
		t.Fatalf("跳转后应失败，得到 %q (%v)", ip, err)
	}
	if got := describeProbeError(err, "http://u:p@127.0.0.1:1"); got != "出口 IP 服务返回 HTTP 302" {
		t.Fatalf("说明 = %q", got)
	}
	if followed.Load() {
		t.Fatal("探测跟随了跳转")
	}
}
