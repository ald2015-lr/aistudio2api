package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

const (
	// proxyProbeURL 为第三方出口 IP 服务；绝不能换成 Google 的任何域名
	proxyProbeURL = "https://api.ipify.org?format=json"
	// proxyProbeHost 为出口 IP 服务的主机名：中转记下的按目标失败只认这个主机
	proxyProbeHost = "api.ipify.org"
	// proxyProbeTimeout 不小于中转的 UpstreamTimeout（25 秒）加 TLS 与一次请求的余量，否则上游慢时浏览器路径先超时、中转还没记下失败原因
	proxyProbeTimeout = 40 * time.Second
	// proxyProbeBodyLimit 为出口 IP 服务响应体的读取上限
	proxyProbeBodyLimit = 4 << 10
	// proxyProbeErrorLimit 为单条路径失败说明的长度上限（字符数）
	proxyProbeErrorLimit = 200
)

// 测试结果里的提示码，前端按码显示文案
const (
	proxyWarningRotatingIP        = "rotating_ip"
	proxyWarningHTTPSPlainBrowser = "https_plain_browser"
)

// runtimeManager 实现 api 包按可选接口识别的“测试代理”（POST /api/proxy/test），不改 AdminService
var _ interface {
	TestProxy(context.Context, api.ProxyTestInput) (api.ProxyTestResult, error)
} = (*runtimeManager)(nil)

// TestProxy 分别经 Camoufox 的路径（带账号密码时经本机中转）与 Go 后台请求的路径访问一次出口 IP 服务，
// 检查账号密码是否可用、两条连接的出口 IP 是否相同；不访问 Google。服务停止时也能用
func (manager *runtimeManager) TestProxy(ctx context.Context, input api.ProxyTestInput) (api.ProxyTestResult, error) {
	if !manager.proxyTestMu.TryLock() {
		return api.ProxyTestResult{}, &adminOperationError{
			status: http.StatusConflict, code: "proxy_test_busy", message: "上一次代理测试还没有结束",
		}
	}
	defer manager.proxyTestMu.Unlock()
	value := input.Proxy
	if strings.TrimSpace(value) == "" {
		// 留空时测试登录与 Worker 实际会用的默认代理：当前生成服务的配置快照，与 runtimeAdmin.effectiveProxy 一致
		value = manager.generation().config.Proxy
	}
	if strings.TrimSpace(value) == "" {
		return api.ProxyTestResult{}, &adminOperationError{
			status: http.StatusBadRequest, code: "proxy_required", message: "没有填写代理，服务配置的默认代理也为空",
		}
	}
	normalized, err := config.NormalizeProxy(value)
	if err != nil {
		// 规范化的错误是固定文案，不含输入内容
		return api.ProxyTestResult{}, invalidConfigError(err)
	}
	relay, err := manager.acquireTestRelay(normalized)
	if err != nil {
		// 这里的错误只有“代理地址无效”“本机代理中转已关闭”与监听 127.0.0.1:0 的失败，不含上游地址
		return api.ProxyTestResult{}, &adminOperationError{
			status: http.StatusInternalServerError, code: "proxy_relay_failed", message: err.Error(),
		}
	}
	defer relay.Release()

	probe := manager.probeProxy
	if probe == nil {
		probe = probeProxyExit
	}
	probeCtx, cancel := context.WithTimeout(ctx, proxyProbeTimeout)
	defer cancel()
	startedAt := time.Now()
	var browser, direct api.ProxyTestProbe
	var wait sync.WaitGroup
	wait.Add(2)
	// 两条路径并发、各用一条新连接：按连接轮换 IP 的代理会给出不同的出口 IP
	go func() {
		defer wait.Done()
		browser = probe(probeCtx, relay.Address())
	}()
	go func() {
		defer wait.Done()
		direct = probe(probeCtx, normalized)
	}()
	wait.Wait()
	if browser.Error != "" {
		// 中转记下的原因（407、连不上上游、超时等）比浏览器路径看到的 502/504 或期限错误更准确；
		// 这个租约独占监听器，失败记录里不会混进 Worker 的失败。按目标的失败只认出口 IP 服务
		if failure := relay.FailureSince(startedAt, func(host string) bool { return host == proxyProbeHost }); failure != "" {
			browser.Error = failure
		}
	}
	// 再按上游的账号密码兜底脱敏：说明文字来自外部错误时也不会带出账号密码
	browser.Error = limitProbeError(proxyrelay.Scrub(browser.Error, normalized))
	direct.Error = limitProbeError(proxyrelay.Scrub(direct.Error, normalized))

	authenticated := config.ProxyHasCredentials(normalized)
	result := api.ProxyTestResult{
		Proxy: config.RedactProxy(normalized), Authenticated: authenticated, Relayed: relay.Relayed(),
		Browser: browser, Direct: direct,
		SameIP: browser.IP != "" && browser.IP == direct.IP,
	}
	if browser.IP != "" && direct.IP != "" && browser.IP != direct.IP {
		result.Warnings = append(result.Warnings, proxyWarningRotatingIP)
	}
	if !authenticated && proxyScheme(normalized) == "https" {
		// 现状：Camoufox 用明文连接不带账号密码的 https:// 代理（prefs 只有 HTTP 代理）
		result.Warnings = append(result.Warnings, proxyWarningHTTPSPlainBrowser)
	}
	manager.logProxyTest(result)
	return result, nil
}

// acquireTestRelay 为“测试代理”申请本机中转租约，owner 固定为“代理测试”
func (manager *runtimeManager) acquireTestRelay(normalized string) (*proxyrelay.Lease, error) {
	if manager.proxyRelays != nil {
		return manager.proxyRelays.Acquire(normalized, "代理测试")
	}
	return proxyrelay.Acquire(normalized, "代理测试")
}

// logProxyTest 记一行测试结果：两条路径都成功记 INFO，否则记 WARN；代理只写 scheme://host:port
func (manager *runtimeManager) logProxyTest(result api.ProxyTestResult) {
	if manager.requests == nil {
		return
	}
	level := "INFO"
	if result.Browser.IP == "" || result.Direct.IP == "" {
		level = "WARN"
	}
	authenticated := "否"
	if result.Authenticated {
		authenticated = "是"
	}
	manager.requests.log("service", level, fmt.Sprintf("代理测试 | 代理=%s | 认证=%s | 浏览器路径=%s | 后台请求路径=%s",
		result.Proxy, authenticated, proxyProbeText(result.Browser), proxyProbeText(result.Direct)))
}

// proxyProbeText 为日志里一条路径的结果：成功写出口 IP 与耗时，失败写说明
func proxyProbeText(probe api.ProxyTestProbe) string {
	if probe.IP != "" {
		return fmt.Sprintf("%s（%dms）", probe.IP, probe.ElapsedMS)
	}
	return "失败：" + probe.Error
}

// proxyScheme 返回规范化代理的协议（小写）
func proxyScheme(normalized string) string {
	parsed, err := url.Parse(normalized)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Scheme)
}

// limitProbeError 把失败说明截到 proxyProbeErrorLimit 个字符
func limitProbeError(text string) string {
	text = strings.TrimSpace(text)
	if runes := []rune(text); len(runes) > proxyProbeErrorLimit {
		return string(runes[:proxyProbeErrorLimit-1]) + "…"
	}
	return text
}

// probeProxyExit 经指定代理访问出口 IP 服务；使用与 Go 后台请求相同的 aistudio.NewProxyHTTPClient（浏览器 TLS 指纹）
func probeProxyExit(ctx context.Context, proxyURL string) api.ProxyTestProbe {
	startedAt := time.Now()
	ip, err := fetchProxyExit(ctx, proxyURL)
	probe := api.ProxyTestProbe{IP: ip, ElapsedMS: time.Since(startedAt).Milliseconds()}
	if err != nil {
		probe.IP = ""
		probe.Error = limitProbeError(describeProbeError(err, proxyURL))
	}
	return probe
}

// proxyProbeResponseError 为出口 IP 服务的响应不合要求：文案是固定文本，不经过 proxyrelay.Describe
type proxyProbeResponseError struct {
	message string
}

func (err *proxyProbeResponseError) Error() string {
	return err.message
}

// describeProbeError 把探测错误转成中文说明：网络与代理错误按 proxyrelay.Describe 归类，响应不合要求时用固定文案
func describeProbeError(err error, proxyURL string) string {
	var response *proxyProbeResponseError
	if errors.As(err, &response) {
		return response.message
	}
	return proxyrelay.Describe(err, proxyURL)
}

// fetchProxyExit 发一次 GET 并解析出口 IP；结束时关闭空闲连接，下一次探测用新连接
func fetchProxyExit(ctx context.Context, proxyURL string) (string, error) {
	client, err := newProxyProbeClient(proxyURL)
	if err != nil {
		return "", err
	}
	defer client.CloseIdleConnections()
	return fetchProbeURL(ctx, client, proxyProbeURL)
}

// newProxyProbeClient 建探测用的 client：不跟随跳转，只访问出口 IP 服务本身，3xx 按“出口 IP 服务返回 HTTP 30x”报告
func newProxyProbeClient(proxyURL string) (*http.Client, error) {
	client, err := aistudio.NewProxyHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client, nil
}

// fetchProbeURL 用 client 向 target 发一次 GET 并解析出口 IP
func fetchProbeURL(ctx context.Context, client *http.Client, target string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	return parseProxyProbeBody(response.StatusCode, response.Body)
}

// parseProxyProbeBody 解析出口 IP 服务的响应：状态码必须是 200，响应体最多 4 KiB，形如 {"ip":"203.0.113.7"}
func parseProxyProbeBody(status int, body io.Reader) (string, error) {
	if status != http.StatusOK {
		return "", &proxyProbeResponseError{message: fmt.Sprintf("出口 IP 服务返回 HTTP %d", status)}
	}
	data, err := io.ReadAll(io.LimitReader(body, proxyProbeBodyLimit+1))
	if err != nil {
		return "", fmt.Errorf("读取出口 IP 服务的响应: %w", err)
	}
	if len(data) > proxyProbeBodyLimit {
		return "", &proxyProbeResponseError{message: "出口 IP 服务的响应超过 4 KiB"}
	}
	var payload struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", &proxyProbeResponseError{message: "出口 IP 服务的响应不是 JSON"}
	}
	ip := net.ParseIP(strings.TrimSpace(payload.IP))
	if ip == nil {
		return "", &proxyProbeResponseError{message: "出口 IP 服务返回的 IP 无效"}
	}
	return ip.String(), nil
}
