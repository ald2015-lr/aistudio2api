package proxyrelay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquirePassthrough(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	upstream := startUpstream(t, serveConnectEcho(t))
	relayed := acquireLease(t, registry, httpUpstreamURL(upstream.address()))
	before := registry.entryCount()
	for _, value := range []string{"", "  ", "http://127.0.0.1:7890", "HTTP://Proxy.example.com:8080/", "socks5://127.0.0.1:1080", "http://h", relayed.Address()} {
		lease, err := registry.Acquire(value, "直通")
		if err != nil {
			t.Fatalf("Acquire(%q): %v", value, err)
		}
		if lease.Address() != value || lease.Relayed() {
			t.Fatalf("Acquire(%q) = %q relayed=%v，期望原样直通", value, lease.Address(), lease.Relayed())
		}
		lease.Release()
		lease.Release()
		if failure := lease.FailureSince(time.Time{}, nil); failure != "" {
			t.Fatalf("直通租约有失败记录：%q", failure)
		}
	}
	if registry.entryCount() != before {
		t.Fatalf("直通申请起了新监听器：%d -> %d", before, registry.entryCount())
	}
	var empty *Lease
	empty.Release()
	cause := errors.New("原始错误")
	if empty.Address() != "" || empty.Relayed() || empty.FailureSince(time.Time{}, nil) != "" || empty.WrapFailure(cause, time.Time{}, nil) != cause {
		t.Fatal("nil 租约的方法不安全")
	}
	for _, value := range []string{"ftp://u:p@h:1", "http://u:p@:1", "http://u:p@h:99999", "http://u:p@h:0", "socks5://u:p@h", "http://:p@h:1", "http://%zz:p@h:1"} {
		if _, err := registry.Acquire(value, "无效"); !errors.Is(err, ErrInvalidUpstream) {
			t.Fatalf("Acquire(%q) 错误 = %v，期望 ErrInvalidUpstream", value, err)
		}
	}
}

func TestSetLoggerReplacesDefault(t *testing.T) {
	recorder := &logRecorder{}
	SetLogger(recorder.log)
	defer SetLogger(nil)
	lease, err := Acquire("http://127.0.0.1:7890", "直通")
	if err != nil || lease.Relayed() {
		t.Fatalf("默认注册表直通失败：%v", err)
	}
	defaultRegistry.logf("INFO", "测试")
	SetLogger(nil)
	defaultRegistry.logf("INFO", "丢弃")
	if lines := recorder.snapshot(); len(lines) != 1 || lines[0] != "INFO 测试" {
		t.Fatalf("日志 = %q", lines)
	}
}

func TestAcquireSeparateListenerPerLease(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{IdleGrace: 50 * time.Millisecond})
	upstream := startUpstream(t, serveConnectEcho(t))
	value := httpUpstreamURL(upstream.address())
	first := acquireLease(t, registry, value)
	second := acquireLease(t, registry, value)
	if first.Address() == second.Address() {
		t.Fatalf("同一上游的两个租约共用了监听器：%s", first.Address())
	}
	if !strings.HasPrefix(first.Address(), "http://127.0.0.1:") {
		t.Fatalf("中转地址 = %q", first.Address())
	}
	first.Release()
	first.Release()
	waitFor(t, "释放的监听器关闭", func() bool { return !registry.hasEntry(first.entry) })
	if !listenerClosed(first) {
		t.Fatal("释放的监听器关闭后端口仍能连上")
	}
	tunnel := openTunnel(t, second, "a.example:443")
	requireEcho(t, tunnel, "second lease still works")
	if registry.entryCount() != 1 {
		t.Fatalf("剩余监听器 = %d", registry.entryCount())
	}
}

func TestIdleCloseAfterQuietGrace(t *testing.T) {
	registry, recorder := newTestRegistry(t, Options{IdleGrace: 50 * time.Millisecond})
	lease := acquireLease(t, registry, httpUpstreamURL(closedPort(t)))
	if listenerClosed(lease) {
		t.Fatal("释放前监听器就连不上")
	}
	lease.Release()
	waitFor(t, "监听器关闭", func() bool { return registry.entryCount() == 0 })
	if !listenerClosed(lease) {
		t.Fatal("监听器关闭后端口仍能连上")
	}
	if lines := recorder.matching("本机代理中转关闭"); len(lines) != 1 || !strings.Contains(lines[0], "释放后=") {
		t.Fatalf("关闭日志 = %q", lines)
	}
}

func TestReleasedListenerStaysWhileActive(t *testing.T) {
	grace := 100 * time.Millisecond
	registry, recorder := newTestRegistry(t, Options{IdleGrace: grace})
	upstream := startUpstream(t, serveConnectEcho(t))
	lease := acquireLease(t, registry, httpUpstreamURL(upstream.address()))
	tunnel := openTunnel(t, lease, "a.example:443")
	lease.Release()
	time.Sleep(3 * grace)
	if !registry.hasEntry(lease.entry) {
		t.Fatal("有进行中的隧道时监听器被关闭")
	}
	requireEcho(t, tunnel, "still alive after grace")
	_ = tunnel.Close()
	var lastActivity time.Time
	waitFor(t, "隧道结束", func() bool {
		lease.entry.mu.Lock()
		defer lease.entry.mu.Unlock()
		lastActivity = lease.entry.lastActivity
		return lease.entry.active == 0
	})
	waitFor(t, "隧道结束后监听器关闭", func() bool { return !registry.hasEntry(lease.entry) })
	if quiet := time.Since(lastActivity); quiet < grace {
		t.Fatalf("隧道结束后 %s 就关闭，短于 IdleGrace", quiet)
	}

	// 释放后持续有新连接：监听器一直保留，WARN 只记一条
	grace = 200 * time.Millisecond
	registry, recorder = newTestRegistry(t, Options{IdleGrace: grace})
	lease = acquireLease(t, registry, httpUpstreamURL(upstream.address()))
	lease.Release()
	stop := time.Now().Add(4 * grace)
	for time.Now().Before(stop) {
		if listenerClosed(lease) {
			t.Fatal("释放后仍有新连接时监听器被关闭")
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !registry.hasEntry(lease.entry) {
		t.Fatal("释放后仍有新连接时监听器被移出注册表")
	}
	if lines := recorder.matching("本机代理中转在租约释放后仍有新连接"); len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN ") {
		t.Fatalf("释放后新连接的 WARN = %q", lines)
	}
	waitFor(t, "安静后监听器关闭", func() bool { return !registry.hasEntry(lease.entry) })
}

func TestFailureRecordsPerLease(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	upstream := startUpstream(t, respondConnect("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"x\"\r\nContent-Length: 0\r\n\r\n"))
	value := httpUpstreamURL(upstream.address())
	first := acquireLease(t, registry, value)
	second := acquireLease(t, registry, value)
	since := time.Now()
	if response := sendAndReadAll(t, first, connectRequest("aistudio.google.com:443")); response != responseAuthFailed {
		t.Fatalf("响应 = %q", response)
	}
	if failure := first.FailureSince(since, nil); failure != text407 {
		t.Fatalf("A 的失败说明 = %q", failure)
	}
	if failure := second.FailureSince(since, GoogleTarget); failure != "" {
		t.Fatalf("B 的失败说明 = %q，期望为空", failure)
	}
	if failure := first.FailureSince(time.Now().Add(time.Second), nil); failure != "" {
		t.Fatalf("since 之后没有失败时 = %q", failure)
	}
}

func TestFailureRelevance(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	blocked := startUpstream(t, respondConnect("HTTP/1.1 403 Forbidden\r\nContent-Length: 6\r\n\r\ndenied"))
	lease := acquireLease(t, registry, httpUpstreamURL(blocked.address()))
	cause := errors.New("加载 AI Studio 页面: NS_ERROR_PROXY_BAD_GATEWAY")
	since := time.Now()
	if response := sendAndReadAll(t, lease, connectRequest("blocked.example:443")); !strings.HasPrefix(response, "HTTP/1.1 403 Forbidden\r\n") {
		t.Fatalf("响应 = %q", response)
	}
	if wrapped := lease.WrapFailure(cause, since, GoogleTarget); wrapped != cause {
		t.Fatalf("第三方目标的失败被附加：%v", wrapped)
	}
	if failure := lease.FailureSince(since, func(host string) bool { return host == "blocked.example" }); !strings.Contains(failure, "blocked.example") {
		t.Fatalf("relevant 认为相关时 = %q", failure)
	}
	sendAndReadAll(t, lease, connectRequest("aistudio.google.com:443"))
	wrapped := lease.WrapFailure(cause, since, GoogleTarget)
	expected := "加载 AI Studio 页面: NS_ERROR_PROXY_BAD_GATEWAY；本机代理中转：上游代理拒绝了隧道：403 Forbidden（目标 aistudio.google.com）"
	if wrapped.Error() != expected || !errors.Is(wrapped, cause) {
		t.Fatalf("附加后 = %q", wrapped)
	}

	auth := startUpstream(t, respondConnect("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
	authLease := acquireLease(t, registry, httpUpstreamURL(auth.address()))
	sendAndReadAll(t, authLease, connectRequest("blocked.example:443"))
	if wrapped := authLease.WrapFailure(cause, since, nil); !strings.HasSuffix(wrapped.Error(), "；本机代理中转："+text407) {
		t.Fatalf("407 没有附加：%v", wrapped)
	}
	unreachable := acquireLease(t, registry, httpUpstreamURL(closedPort(t)))
	sendAndReadAll(t, unreachable, connectRequest("blocked.example:443"))
	if wrapped := unreachable.WrapFailure(cause, since, nil); !strings.Contains(wrapped.Error(), "连不上上游代理") {
		t.Fatalf("连不上上游没有附加：%v", wrapped)
	}
	if lease.WrapFailure(nil, since, GoogleTarget) != nil {
		t.Fatal("nil 错误被包装")
	}
}

func TestGoogleTarget(t *testing.T) {
	for host, expected := range map[string]bool{
		"aistudio.google.com":                     true,
		"alkalimakersuite-pa.clients6.google.com": true,
		"GOOGLE.COM":                              true,
		"www.googleapis.com.":                     true,
		"fonts.gstatic.com:443":                   true,
		"lh3.googleusercontent.com":               true,
		"api.ipify.org":                           false,
		"notgoogle.com":                           false,
		"google.com.evil.example":                 false,
		"":                                        false,
	} {
		if GoogleTarget(host) != expected {
			t.Fatalf("GoogleTarget(%q) != %v", host, expected)
		}
	}
}

func TestConcurrentTunnels(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	upstream := startUpstream(t, serveConnectEcho(t))
	lease := acquireLease(t, registry, httpUpstreamURL(upstream.address()))
	var wait sync.WaitGroup
	errs := make(chan error, 200)
	for index := 0; index < 200; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- concurrentEcho(lease)
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// 单个监听器满了：第 5 条连接不等待，立即收到 503
	limited, recorder := newTestRegistry(t, Options{MaxConnections: 4})
	limitedLease := acquireLease(t, limited, httpUpstreamURL(upstream.address()))
	for index := 0; index < 4; index++ {
		requireEcho(t, openTunnel(t, limitedLease, "a.example:443"), "x")
	}
	started := time.Now()
	if response := sendAndReadAll(t, limitedLease, connectRequest("a.example:443")); response != responseBusy {
		t.Fatalf("第 5 条连接 = %q", response)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("连接数已满时等待了 %s", elapsed)
	}
	if failure := limitedLease.FailureSince(started.Add(-time.Second), nil); failure != textBusy {
		t.Fatalf("失败说明 = %q", failure)
	}
	if lines := recorder.matching("本机代理中转连接数已满"); len(lines) != 1 || !strings.Contains(lines[0], "范围=单个浏览器 | 上限=4") {
		t.Fatalf("已满日志 = %q", lines)
	}

	// 进程级上限：两个租约各 3 条之后，任一租约的新连接都收到 503
	total, totalRecorder := newTestRegistry(t, Options{MaxTotalConnections: 6})
	leases := []*Lease{acquireLease(t, total, httpUpstreamURL(upstream.address())), acquireLease(t, total, httpUpstreamURL(upstream.address()))}
	for _, each := range leases {
		for index := 0; index < 3; index++ {
			requireEcho(t, openTunnel(t, each, "a.example:443"), "y")
		}
	}
	for _, each := range leases {
		if response := sendAndReadAll(t, each, connectRequest("a.example:443")); response != responseBusy {
			t.Fatalf("进程级上限后 = %q", response)
		}
	}
	if lines := totalRecorder.matching("本机代理中转连接数接近上限"); len(lines) == 0 || !strings.Contains(lines[0], "上限=6") {
		t.Fatalf("接近上限的 WARN = %q", lines)
	}
	if lines := totalRecorder.matching("范围=整个进程 | 上限=6"); len(lines) != 2 {
		t.Fatalf("进程级已满日志 = %q", lines)
	}
}

// 连接数已满时，被拒的连接等第一个字节也只按 HeaderTimeout：浏览器预连接又不发字节时不能占着 fd 等 FirstByteTimeout
func TestBusyConnectionWaitsHeaderTimeoutOnly(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{MaxConnections: 1, HeaderTimeout: 200 * time.Millisecond, FirstByteTimeout: time.Minute})
	lease := acquireLease(t, registry, httpUpstreamURL(closedPort(t)))
	// 第一条预连接不发字节，按 FirstByteTimeout 占着唯一的名额
	holder := dialLease(t, lease)
	defer holder.Close()
	waitFor(t, "第一条连接占住名额", func() bool {
		lease.entry.mu.Lock()
		defer lease.entry.mu.Unlock()
		return lease.entry.slots == 1
	})
	started := time.Now()
	rejected := dialLease(t, lease)
	data, err := io.ReadAll(rejected)
	if err != nil || len(data) != 0 {
		t.Fatalf("不发字节的被拒连接应被安静关闭，得到 %q: %v", data, err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("被拒连接等了 %s 才关闭，应按 HeaderTimeout", elapsed)
	}
	if failure := lease.FailureSince(started.Add(-time.Second), nil); failure != textBusy {
		t.Fatalf("失败说明 = %q", failure)
	}
	// 发了请求头的被拒连接照常收到 503
	if response := sendAndReadAll(t, lease, connectRequest("a.example:443")); response != responseBusy {
		t.Fatalf("已满时 = %q", response)
	}
}

// concurrentEcho 经隧道回显 64 KiB 随机数据并校验
func concurrentEcho(lease *Lease) error {
	address := strings.TrimPrefix(lease.Address(), "http://")
	connection, err := net.DialTimeout("tcp", address, testTimeout)
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(4 * testTimeout))
	if _, err := io.WriteString(connection, connectRequest("a.example:443")); err != nil {
		return err
	}
	expected := "HTTP/1.1 200 Connection established\r\n\r\n"
	head := make([]byte, len(expected))
	if _, err := io.ReadFull(connection, head); err != nil || string(head) != expected {
		return fmt.Errorf("CONNECT 响应 %q: %v", head, err)
	}
	payload := make([]byte, 64<<10)
	_, _ = rand.Read(payload)
	writeErr := make(chan error, 1)
	go func() {
		_, err := connection.Write(payload)
		if err == nil {
			err = connection.(*net.TCPConn).CloseWrite()
		}
		writeErr <- err
	}()
	echoed, err := io.ReadAll(connection)
	if err != nil {
		return err
	}
	if err := <-writeErr; err != nil {
		return err
	}
	if sha256.Sum256(echoed) != sha256.Sum256(payload) {
		return fmt.Errorf("回显校验和不一致：%d 字节", len(echoed))
	}
	return nil
}

// flakyListener 前几次 Accept 返回临时错误（模拟 EMFILE）
type flakyListener struct {
	net.Listener
	failures atomic.Int32
}

type temporaryError struct{}

func (temporaryError) Error() string   { return "accept: too many open files" }
func (temporaryError) Timeout() bool   { return false }
func (temporaryError) Temporary() bool { return true }

func (listener *flakyListener) Accept() (net.Conn, error) {
	if listener.failures.Add(-1) >= 0 {
		return nil, temporaryError{}
	}
	return listener.Listener.Accept()
}

func TestAcceptBackoffAndClose(t *testing.T) {
	registry, recorder := newTestRegistry(t, Options{})
	registry.listen = func(ctx context.Context) (net.Listener, error) {
		listener, err := listenLoopback(ctx)
		if err != nil {
			return nil, err
		}
		flaky := &flakyListener{Listener: listener}
		flaky.failures.Store(3)
		return flaky, nil
	}
	upstream := startUpstream(t, serveConnectEcho(t))
	lease := acquireLease(t, registry, httpUpstreamURL(upstream.address()))
	tunnel := openTunnel(t, lease, "a.example:443")
	requireEcho(t, tunnel, "after backoff")
	if lines := recorder.matching("本机代理中转接受连接失败"); len(lines) != 1 || !strings.Contains(lines[0], "too many open files") {
		t.Fatalf("接受失败日志 = %q", lines)
	}
	idle := dialLease(t, lease)
	waitFor(t, "预连接登记", func() bool {
		lease.entry.mu.Lock()
		defer lease.entry.mu.Unlock()
		return lease.entry.active == 2
	})
	if err := registry.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-lease.entry.done:
	default:
		t.Fatal("Close 返回时 acceptLoop 还没有退出")
	}
	for _, connection := range []net.Conn{tunnel, idle} {
		buffer := make([]byte, 1)
		if _, err := connection.Read(buffer); err == nil {
			t.Fatal("Close 之后进行中的连接仍可读")
		}
	}
	if !listenerClosed(lease) {
		t.Fatal("Close 之后监听器仍可连接")
	}
	if _, err := registry.Acquire(httpUpstreamURL(upstream.address()), "关闭后"); err == nil || err.Error() != "本机代理中转已关闭" {
		t.Fatalf("Close 之后 Acquire 错误 = %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	lease.Release()
}

func TestLoopbackOnly(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	lease := acquireLease(t, registry, socksUpstreamURL(closedPort(t)))
	if !strings.HasPrefix(lease.Address(), "socks5://127.0.0.1:") {
		t.Fatalf("SOCKS5 中转地址 = %q", lease.Address())
	}
	address, ok := lease.entry.listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("监听地址 = %v", lease.entry.listener.Addr())
	}
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		return
	}
	for _, each := range interfaces {
		network, ok := each.(*net.IPNet)
		if !ok || network.IP.IsLoopback() || network.IP.To4() == nil {
			continue
		}
		target := net.JoinHostPort(network.IP.String(), fmt.Sprint(address.Port))
		if connection, err := net.DialTimeout("tcp", target, 200*time.Millisecond); err == nil {
			_ = connection.Close()
			t.Fatalf("经非回环地址 %s 连上了中转", target)
		}
	}
}

func TestDescribeAndScrub(t *testing.T) {
	upstream := socksUpstreamURL("h:1")
	dialRefused := &net.OpError{Op: "dial", Net: "tcp", Addr: testAddr("h:1"), Err: errors.New("connect: connection refused")}
	cases := []struct {
		name     string
		err      error
		upstream string
		expected string
	}{
		{"拨号被拒", &net.OpError{Op: "socks connect", Net: "tcp", Source: testAddr("h:1"), Addr: testAddr("d:443"), Err: dialRefused}, upstream, "连不上上游代理 h:1（连接被拒绝）"},
		{"回复码被拒", &net.OpError{Op: "socks connect", Net: "tcp", Source: testAddr("h:1"), Addr: testAddr("d:443"), Err: errors.New("unknown error connection refused")}, upstream, "上游 SOCKS5 代理连接目标失败：05 拒绝连接"},
		{"未知回复码", errors.New("socks connect tcp h:1->d:443: unknown error unknown code: 9"), upstream, "上游 SOCKS5 代理连接目标失败：09 未知错误"},
		{"SOCKS 认证失败", errors.New("socks connect tcp h:1->d:443: username/password authentication failed"), upstream, textSOCKSAuthDenied},
		{"SOCKS 方法不接受", errors.New("socks connect tcp h:1->d:443: no acceptable authentication methods"), upstream, textSOCKSAuthMethod},
		{"HTTP 407", errors.New(`Get "https://api.ipify.org?format=json": 代理 CONNECT 返回 407 Proxy Authentication Required`), "http://u:p@h:8080", text407},
		{"tls-client 407", errors.New("Proxy responded with non 200 code: 407 Proxy Authentication Required"), "http://u:p@h:8080", text407},
		{"DNS", &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "h"}}, "http://u:p@h", "连不上上游代理 h:80（主机名无法解析）"},
		{"拨号超时", &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}, "http://u:p@h:8080", "连接上游代理 h:8080 超时"},
		{"类型丢失的拨号错误", errors.New("failed to connect: dial tcp 1.2.3.4:8080: connect: connection refused"), "http://u:p@1.2.3.4:8080", "连不上上游代理 1.2.3.4:8080（连接被拒绝）"},
		{"超时", context.DeadlineExceeded, "https://u:p@h:8443", "连接上游代理 h:8443 超时"},
		{"https 上游 TLS", errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), "https://u:p@h:8443", "与上游代理 h:8443 的 TLS 握手失败"},
		{"目标 TLS", errors.New("remote error: tls: handshake failure"), "http://u:p@h:8080", "TLS 握手失败"},
		{"其他脱敏", fmt.Errorf("奇怪的错误 %s %s", testUser, testPassword), upstream, "奇怪的错误 *** ***"},
		{"端口里的 407 不算状态码", errors.New("代理 CONNECT 返回 502 Bad Gateway（h:4070）"), "http://u:p@h:4070", "代理 CONNECT 返回 502 Bad Gateway（h:4070）"},
		{"nil", nil, upstream, ""},
	}
	for _, each := range cases {
		if got := Describe(each.err, each.upstream); got != each.expected {
			t.Fatalf("%s：Describe = %q，期望 %q", each.name, got, each.expected)
		}
	}
	if got := Describe(&urlError{err: cases[0].err}, upstream); got != cases[0].expected {
		t.Fatalf("包了一层的拨号错误 = %q", got)
	}

	encoded := "http://user%40corp:p%40ss%2Fw0rd@h:1"
	text := strings.Join([]string{"user@corp", "user%40corp", "p@ss/w0rd", "p%40ss%2Fw0rd", "p%40ss%2Fw0rd", basicCredentials("user@corp", "p@ss/w0rd")}, " | ")
	if scrubbed := Scrub(text, encoded); strings.Trim(strings.ReplaceAll(strings.ReplaceAll(scrubbed, "***", ""), " | ", ""), " ") != "" {
		t.Fatalf("Scrub 后仍有残留：%q", scrubbed)
	}
	if scrubbed := Scrub("u p ab", "http://u:p@h:1"); scrubbed != "u p ab" {
		t.Fatalf("短于 4 的值不应替换：%q", scrubbed)
	}
	if scrubbed := Scrub("token "+basicCredentials("u", "p"), "http://u:p@h:1"); scrubbed != "token ***" {
		t.Fatalf("Basic base64 没有替换：%q", scrubbed)
	}
	if scrubbed := Scrub("h:1:"+testUser+":pass:word", "h:1:"+testUser+":pass:word"); strings.Contains(scrubbed, testUser) || strings.Contains(scrubbed, "pass:word") {
		t.Fatalf("冒号写法没有脱敏：%q", scrubbed)
	}
	if scrubbed := Scrub("plain text", "http://h:1"); scrubbed != "plain text" {
		t.Fatalf("不带认证时改动了文本：%q", scrubbed)
	}
}

type testAddr string

func (address testAddr) Network() string { return "tcp" }
func (address testAddr) String() string  { return string(address) }

// urlError 模拟 net/http 在外层包的 *url.Error
type urlError struct{ err error }

func (wrapper *urlError) Error() string {
	return `Get "https://api.ipify.org?format=json": ` + wrapper.err.Error()
}
func (wrapper *urlError) Unwrap() error { return wrapper.err }

func basicCredentials(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}
