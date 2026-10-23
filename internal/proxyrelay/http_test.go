package proxyrelay

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHTTPConnectInjectsAuthorization(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	heads := make(chan string, 1)
	upstream := startUpstream(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		head, err := readHeadRaw(reader)
		heads <- head
		if err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\n\r\n")
		echoUntilEOF(connection, reader)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(upstream.address()))
	request := "CONNECT a.example:443 HTTP/1.1\r\n" +
		"Host: a.example:443\r\n" +
		"User-Agent: Mozilla/5.0 (X11; Linux x86_64; rv:152.0) Gecko/20100101 Firefox/152.0\r\n" +
		"Proxy-Connection: keep-alive\r\n" +
		"proxy-authorization: Basic Zm9yZ2VkOmZvcmdlZA==\r\n" +
		"Connection: keep-alive\r\n" +
		"\r\n"
	connection := dialLease(t, lease)
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatal(err)
	}
	expected := "CONNECT a.example:443 HTTP/1.1\r\n" +
		"Host: a.example:443\r\n" +
		"User-Agent: Mozilla/5.0 (X11; Linux x86_64; rv:152.0) Gecko/20100101 Firefox/152.0\r\n" +
		"Proxy-Connection: keep-alive\r\n" +
		"Connection: keep-alive\r\n" +
		"Proxy-Authorization: " + testAuthorization() + "\r\n" +
		"\r\n"
	if head := <-heads; head != expected {
		t.Fatalf("上游收到的请求头：\n%q\n期望：\n%q", head, expected)
	}
	status := make([]byte, len("HTTP/1.1 200 Connection established\r\n\r\n"))
	if _, err := io.ReadFull(connection, status); err != nil || string(status) != "HTTP/1.1 200 Connection established\r\n\r\n" {
		t.Fatalf("CONNECT 响应 = %q: %v", status, err)
	}
	requireEcho(t, connection, "\x16\x03\x01 hello")
	requireEcho(t, connection, strings.Repeat("z", 100000))
}

func TestHTTPConnectPipelinedBytes(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	clientHello := strings.Repeat("C", 517)
	serverBytes := strings.Repeat("S", 1400)
	received := make(chan string, 1)
	upstream := startUpstream(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\n\r\n"+serverBytes)
		buffer := make([]byte, len(clientHello))
		_, _ = io.ReadFull(reader, buffer)
		received <- string(buffer)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(upstream.address()))
	connection := dialLease(t, lease)
	if _, err := io.WriteString(connection, connectRequest("a.example:443")+clientHello); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got != clientHello {
		t.Fatalf("上游收到的流水线字节 %d 个", len(got))
	}
	expected := "HTTP/1.1 200 Connection established\r\n\r\n" + serverBytes
	buffer := make([]byte, len(expected))
	if _, err := io.ReadFull(connection, buffer); err != nil || string(buffer) != expected {
		t.Fatalf("客户端收到 %q: %v", buffer[:min(len(buffer), 80)], err)
	}
}

func TestHTTPConnectChunked200(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	tunnelBytes := "\x16\x03\x03 server hello bytes, not a chunk"
	upstream := startUpstream(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"+tunnelBytes)
		_ = closeWrite(connection)
		_, _ = io.Copy(io.Discard, reader)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(upstream.address()))
	connection := dialLease(t, lease)
	if _, err := io.WriteString(connection, connectRequest("a.example:443")); err != nil {
		t.Fatal(err)
	}
	expected := "HTTP/1.1 200 Connection established\r\n\r\n" + tunnelBytes
	buffer := make([]byte, len(expected))
	if _, err := io.ReadFull(connection, buffer); err != nil || string(buffer) != expected {
		t.Fatalf("客户端收到 %q: %v", buffer, err)
	}
}

func TestHTTPConnect407BecomesBadGateway(t *testing.T) {
	clock := newFakeClock()
	registry, recorder := newTestRegistry(t, Options{Now: clock.Now})
	upstream := startUpstream(t, respondConnect("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"proxy\"\r\nContent-Length: 12\r\n\r\nauth needed!"))
	lease := acquireLease(t, registry, httpUpstreamURL(upstream.address()))
	for attempt := 0; attempt < 5; attempt++ {
		response := sendAndReadAll(t, lease, connectRequest("aistudio.google.com:443"))
		if response != "HTTP/1.1 502 Upstream Proxy Authentication Failed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n" {
			t.Fatalf("第 %d 次响应 = %q", attempt+1, response)
		}
		if strings.Contains(strings.ToLower(response), "proxy-authenticate") {
			t.Fatal("响应带了 Proxy-Authenticate")
		}
	}
	if failure := lease.FailureSince(time.Time{}, nil); failure != text407 {
		t.Fatalf("失败说明 = %q", failure)
	}
	lines := recorder.matching("上游代理认证失败")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN 上游代理认证失败 | 上游=http://"+upstream.address()+" | 本机=127.0.0.1:") ||
		!strings.HasSuffix(lines[0], "| 使用者=a@example.com | 已向浏览器返回 502，请检查代理用户名和密码") {
		t.Fatalf("5 次失败的日志 = %q", lines)
	}
	clock.Advance(61 * time.Second)
	sendAndReadAll(t, lease, connectRequest("aistudio.google.com:443"))
	lines = recorder.matching("上游代理认证失败")
	if len(lines) != 2 || !strings.HasSuffix(lines[1], "| 60 秒内另有 4 次") {
		t.Fatalf("窗口过后的日志 = %q", lines)
	}
}

func TestHTTPConnectStatusPassthrough(t *testing.T) {
	registry, recorder := newTestRegistry(t, Options{})
	upstream := startUpstream(t, respondConnect("HTTP/1.1 403 Forbidden\x01\r\nContent-Type: text/html\r\nContent-Length: 18\r\n\r\n<h1>blocked!</h1>\n"))
	lease := acquireLease(t, registry, httpUpstreamURL(upstream.address()))
	response := sendAndReadAll(t, lease, connectRequest("aistudio.google.com:443"))
	if response != "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n" {
		t.Fatalf("响应 = %q", response)
	}
	if failure := lease.FailureSince(time.Time{}, GoogleTarget); failure != "上游代理拒绝了隧道：403 Forbidden（目标 aistudio.google.com）" {
		t.Fatalf("失败说明 = %q", failure)
	}
	if lines := recorder.matching("上游代理拒绝隧道"); len(lines) != 1 || !strings.Contains(lines[0], "| 状态=403 Forbidden | 目标=aistudio.google.com:443 | 使用者=a@example.com") {
		t.Fatalf("日志 = %q", lines)
	}
	bare := startUpstream(t, respondConnect("HTTP/1.1 429\r\n\r\n"))
	bareLease := acquireLease(t, registry, httpUpstreamURL(bare.address()))
	if response := sendAndReadAll(t, bareLease, connectRequest("blocked.example:443")); response != "HTTP/1.1 429 Too Many Requests\r\nContent-Length: 0\r\nConnection: close\r\n\r\n" {
		t.Fatalf("没有原因短语时 = %q", response)
	}
}

func TestHTTPConnectUpstreamUnreachable(t *testing.T) {
	registry, recorder := newTestRegistry(t, Options{})
	address := closedPort(t)
	lease := acquireLease(t, registry, httpUpstreamURL(address))
	if response := sendAndReadAll(t, lease, connectRequest("aistudio.google.com:443")); response != responseUnreachable {
		t.Fatalf("响应 = %q", response)
	}
	if failure := lease.FailureSince(time.Time{}, nil); failure != "连不上上游代理 "+address+"（连接被拒绝）" {
		t.Fatalf("失败说明 = %q", failure)
	}
	if lines := recorder.matching("上游代理连接失败"); len(lines) != 1 || !strings.Contains(lines[0], "| 原因=连接被拒绝 |") {
		t.Fatalf("日志 = %q", lines)
	}

	resolver, _ := newTestRegistry(t, Options{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: network, Err: &net.DNSError{Err: "no such host", Name: "proxy.invalid"}}
	}})
	dnsLease := acquireLease(t, resolver, httpUpstreamURL("proxy.invalid:8080"))
	if response := sendAndReadAll(t, dnsLease, connectRequest("aistudio.google.com:443")); response != responseUnreachable {
		t.Fatalf("主机名无法解析时 = %q", response)
	}
	if failure := dnsLease.FailureSince(time.Time{}, nil); failure != "连不上上游代理 proxy.invalid:8080（主机名无法解析）" {
		t.Fatalf("失败说明 = %q", failure)
	}
}

func TestHTTPConnectUpstreamTimeout(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{UpstreamTimeout: 100 * time.Millisecond})
	upstream := startUpstream(t, func(connection net.Conn) {
		_, _ = io.Copy(io.Discard, connection)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(upstream.address()))
	started := time.Now()
	if response := sendAndReadAll(t, lease, connectRequest("aistudio.google.com:443")); response != responseTimeout {
		t.Fatalf("响应 = %q", response)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("超时用了 %s", elapsed)
	}
	if failure := lease.FailureSince(time.Time{}, nil); failure != "连接上游代理 "+upstream.address()+" 超时" {
		t.Fatalf("失败说明 = %q", failure)
	}

	// 拨号本身超时
	slow, _ := newTestRegistry(t, Options{UpstreamTimeout: 100 * time.Millisecond, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: network, Err: ctx.Err()}
	}})
	slowLease := acquireLease(t, slow, httpUpstreamURL("203.0.113.1:8080"))
	if response := sendAndReadAll(t, slowLease, connectRequest("aistudio.google.com:443")); response != responseTimeout {
		t.Fatalf("拨号超时时 = %q", response)
	}
}

// selfSignedCertificate 为 host 生成自签证书
func selfSignedCertificate(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestHTTPSUpstreamTLS(t *testing.T) {
	certificate, pool := selfSignedCertificate(t, "proxy.test")
	hellos := make(chan *tls.ClientHelloInfo, 4)
	serverConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"http/1.1"},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			hellos <- hello
			return nil, nil
		},
	}
	echo := serveConnectEcho(t)
	upstream := startUpstream(t, func(connection net.Conn) {
		server := tls.Server(connection, serverConfig)
		if err := server.Handshake(); err != nil {
			return
		}
		echo(server)
	})
	_, port, _ := net.SplitHostPort(upstream.address())
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "proxy.test:"+port {
			t.Errorf("拨号地址 = %q", address)
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, upstream.address())
	}
	value := "https://" + testUserinfo() + "@proxy.test:" + port
	registry, _ := newTestRegistry(t, Options{DialContext: dial, RootCAs: pool})
	lease := acquireLease(t, registry, value)
	if !strings.HasPrefix(lease.Address(), "http://127.0.0.1:") {
		t.Fatalf("https 上游的中转地址 = %q", lease.Address())
	}
	tunnel := openTunnel(t, lease, "aistudio.google.com:443")
	requireEcho(t, tunnel, "through tls upstream")
	hello := <-hellos
	if hello.ServerName != "proxy.test" || len(hello.SupportedProtos) != 1 || hello.SupportedProtos[0] != "http/1.1" {
		t.Fatalf("ClientHello ServerName=%q ALPN=%q", hello.ServerName, hello.SupportedProtos)
	}

	untrusted, _ := newTestRegistry(t, Options{DialContext: dial, RootCAs: x509.NewCertPool()})
	untrustedLease := acquireLease(t, untrusted, value)
	if response := sendAndReadAll(t, untrustedLease, connectRequest("aistudio.google.com:443")); response != "HTTP/1.1 502 Upstream Proxy TLS Failed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n" {
		t.Fatalf("证书不受信任时 = %q", response)
	}
	if failure := untrustedLease.FailureSince(time.Time{}, nil); failure != "与上游代理 proxy.test:"+port+" 的 TLS 握手失败" {
		t.Fatalf("失败说明 = %q", failure)
	}
}

// plainUpstream 为假的明文 HTTP 代理：逐个读请求头并按 responses 回复
type plainUpstream struct {
	mu    sync.Mutex
	heads []string
}

func (upstream *plainUpstream) record(head string) {
	upstream.mu.Lock()
	upstream.heads = append(upstream.heads, head)
	upstream.mu.Unlock()
}

func (upstream *plainUpstream) snapshot() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return append([]string(nil), upstream.heads...)
}

func TestPlainHTTPForwardRawHeaders(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	recorded := &plainUpstream{}
	responses := []string{
		"HTTP/1.1 200 OK\r\nContent-Type: application/ocsp-response\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok",
	}
	fake := startUpstream(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		for _, response := range responses {
			head, err := readHeadRaw(reader)
			if err != nil {
				return
			}
			recorded.record(head)
			if _, err := io.WriteString(connection, response); err != nil {
				return
			}
		}
		_, _ = io.Copy(io.Discard, reader)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	first := "GET http://o.pki.goog/x HTTP/1.1\r\n" +
		"Host: o.pki.goog\r\n" +
		"User-Agent: Mozilla/5.0 (X11; Linux x86_64; rv:152.0) Gecko/20100101 Firefox/152.0\r\n" +
		"Pragma: no-cache\r\n" +
		"Accept: */*\r\n" +
		"Zzz-Last: 1\r\n" +
		"accept-language: en-US\r\n" +
		"Proxy-Connection: keep-alive\r\n" +
		"\r\n"
	second := "GET http://c.pki.goog/y HTTP/1.1\r\nHost: c.pki.goog\r\nProxy-Connection: keep-alive\r\n\r\n"
	connection := dialLease(t, lease)
	reader := bufio.NewReader(connection)
	if _, err := io.WriteString(connection, first); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	if string(body) != "hello world" {
		t.Fatalf("chunked 响应体 = %q", body)
	}
	if _, err := io.WriteString(connection, second); err != nil {
		t.Fatal(err)
	}
	response, err = http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	if string(body) != "ok" {
		t.Fatalf("第二个响应体 = %q", body)
	}
	heads := recorded.snapshot()
	injected := "Proxy-Authorization: " + testAuthorization() + "\r\n\r\n"
	if len(heads) != 2 || heads[0] != strings.TrimSuffix(first, "\r\n")+injected || heads[1] != strings.TrimSuffix(second, "\r\n")+injected {
		t.Fatalf("上游收到的请求头 = %q", heads)
	}
	if strings.Contains(heads[0], "Cache-Control") {
		t.Fatal("上游收到了多出来的 Cache-Control")
	}
	if fake.accepted.Load() != 1 {
		t.Fatalf("上游连接数 = %d，期望同一条连接", fake.accepted.Load())
	}
}

func TestPlainHTTPRequestBodies(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	contentLength := "POST http://o.pki.goog/ HTTP/1.1\r\nHost: o.pki.goog\r\nContent-Type: application/ocsp-request\r\nContent-Length: 11\r\n\r\nhello world"
	chunked := "POST http://o.pki.goog/ HTTP/1.1\r\nHost: o.pki.goog\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"5;ext=1\r\nhello\r\n6\r\n world\r\n0\r\nTrailer-X: y\r\n\r\n"
	for _, request := range []string{contentLength, chunked} {
		received := make(chan string, 1)
		head, body, _ := strings.Cut(request, "\r\n\r\n")
		expected := head + "\r\nProxy-Authorization: " + testAuthorization() + "\r\n\r\n" + body
		fake := startUpstream(t, func(connection net.Conn) {
			buffer := make([]byte, len(expected))
			_, err := io.ReadFull(connection, buffer)
			received <- string(buffer)
			if err == nil {
				_, _ = io.WriteString(connection, "HTTP/1.1 204 No Content\r\n\r\n")
			}
			_, _ = io.Copy(io.Discard, connection)
		})
		lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
		connection := dialLease(t, lease)
		if _, err := io.WriteString(connection, request); err != nil {
			t.Fatal(err)
		}
		if got := <-received; got != expected {
			t.Fatalf("上游收到：\n%q\n期望：\n%q", got, expected)
		}
		response, err := http.ReadResponse(bufio.NewReader(connection), nil)
		if err != nil || response.StatusCode != http.StatusNoContent {
			t.Fatalf("响应 %v: %v", response, err)
		}
	}
}

func TestPlainHTTPRejects(t *testing.T) {
	registry, recorder := newTestRegistry(t, Options{})
	fake := startUpstream(t, func(connection net.Conn) {})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	for _, request := range []string{
		"GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n",
		"GET https://aistudio.google.com/ HTTP/1.1\r\nHost: aistudio.google.com\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n",
		"GET  http://o.pki.goog/ HTTP/1.1\r\n\r\n",
		"GET http://o.pki.goog/ HTTP/2.0\r\n\r\n",
		"CONNECT a.example HTTP/1.1\r\n\r\n",
		"CONNECT user@a.example:443 HTTP/1.1\r\n\r\n",
		"CONNECT a.example:0 HTTP/1.1\r\n\r\n",
		"GET http://o.pki.goog/ HTTP/1.1\r\nBad Header Line\r\n\r\n",
	} {
		if response := sendAndReadAll(t, lease, request); response != responseBadRequest {
			t.Fatalf("%q 的响应 = %q", request, response)
		}
	}
	if fake.accepted.Load() != 0 {
		t.Fatalf("被拒绝的请求连了上游 %d 次", fake.accepted.Load())
	}
	if lines := recorder.matching("本机代理中转拒绝请求"); len(lines) != 1 || !strings.Contains(lines[0], "原因=不是 http:// 绝对地址的请求") {
		t.Fatalf("拒绝日志 = %q", lines)
	}
}

func TestPlainHTTP407(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	fake := startUpstream(t, respondConnect("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"x\"\r\nContent-Length: 0\r\n\r\n"))
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	if response := sendAndReadAll(t, lease, "GET http://o.pki.goog/x HTTP/1.1\r\nHost: o.pki.goog\r\n\r\n"); response != responseAuthFailed {
		t.Fatalf("响应 = %q", response)
	}
	if failure := lease.FailureSince(time.Time{}, nil); failure != text407 {
		t.Fatalf("失败说明 = %q", failure)
	}
}

// 上游回 407 而响应体没发完（发得慢或停在半路）时，浏览器要立即收到 502，不能等上游把响应体发完
func TestPlainHTTP407BodyStalled(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	upstreamDone := make(chan struct{})
	fake := startUpstream(t, func(connection net.Conn) {
		defer close(upstreamDone)
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"x\"\r\nContent-Length: 100000\r\n\r\npartial")
		// 停在半路，直到中转关闭上游连接
		_, _ = io.Copy(io.Discard, reader)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	connection := dialLease(t, lease)
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(connection, "GET http://o.pki.goog/x HTTP/1.1\r\nHost: o.pki.goog\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if response := readAllResponse(t, connection); response != responseAuthFailed {
		t.Fatalf("响应 = %q", response)
	}
	select {
	case <-upstreamDone:
	case <-time.After(testTimeout):
		t.Fatal("中转没有关闭上游连接")
	}
	if failure := lease.FailureSince(time.Time{}, nil); failure != text407 {
		t.Fatalf("失败说明 = %q", failure)
	}
}

// 浏览器在响应体中途断开时，中转要立即关闭上游，不能替已经断开的浏览器把剩余响应体读完
func TestPlainHTTPClientGoneMidBody(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	clientClosed := make(chan struct{})
	upstreamDone := make(chan struct{})
	fake := startUpstream(t, func(connection net.Conn) {
		defer close(upstreamDone)
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		if _, err := io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 100000\r\n\r\nhead"); err != nil {
			return
		}
		<-clientClosed
		// 浏览器断开后继续发一部分，然后停在半路：中转写浏览器失败后要立即放手
		chunk := strings.Repeat("x", 4096)
		for range 22 {
			if _, err := io.WriteString(connection, chunk); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		_, _ = io.Copy(io.Discard, reader)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	connection := dialLease(t, lease)
	if _, err := io.WriteString(connection, "GET http://o.pki.goog/x HTTP/1.1\r\nHost: o.pki.goog\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := http.ReadResponse(bufio.NewReader(connection), nil); err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	close(clientClosed)
	select {
	case <-upstreamDone:
	case <-time.After(testTimeout):
		t.Fatal("浏览器断开后中转没有关闭上游连接")
	}
}

// 浏览器断开时上游停在响应体半路、之后一个字节都不再发：转发响应体期间没有写浏览器的机会，要靠盯住浏览器发现断开，
// 立即关闭上游；连接结束后，释放了租约的监听器照常按活动关闭
func TestPlainHTTPClientGoneUpstreamStalled(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{IdleGrace: 100 * time.Millisecond})
	upstreamDone := make(chan struct{})
	fake := startUpstream(t, func(connection net.Conn) {
		defer close(upstreamDone)
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		if _, err := io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 100000\r\n\r\nhead"); err != nil {
			return
		}
		// 停在半路，直到中转关闭上游连接
		_, _ = io.Copy(io.Discard, reader)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	connection := dialLease(t, lease)
	if _, err := io.WriteString(connection, "GET http://o.pki.goog/x HTTP/1.1\r\nHost: o.pki.goog\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := http.ReadResponse(bufio.NewReader(connection), nil); err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	select {
	case <-upstreamDone:
	case <-time.After(testTimeout):
		t.Fatal("浏览器断开、上游停在响应体半路时，中转没有关闭上游连接")
	}
	waitFor(t, "连接结束", func() bool {
		lease.entry.mu.Lock()
		defer lease.entry.mu.Unlock()
		return lease.entry.active == 0
	})
	lease.Release()
	waitFor(t, "监听器按活动关闭", func() bool { return !registry.hasEntry(lease.entry) })
	if failure := lease.FailureSince(time.Time{}, nil); failure != "" {
		t.Fatalf("浏览器自己断开不应记失败：%q", failure)
	}
}

// 转发响应体期间浏览器发来下一个请求（流水线）：盯住浏览器的后台读不能吞掉这些字节，也不能当成浏览器断开
func TestPlainHTTPPipelinedDuringResponseBody(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	bodyRest := make(chan struct{})
	fake := startUpstream(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		for index := 0; ; index++ {
			if _, err := readHeadRaw(reader); err != nil {
				return
			}
			if index == 0 {
				_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 8\r\n\r\nfirs")
				<-bodyRest
				_, _ = io.WriteString(connection, "t-ok")
				continue
			}
			_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nsecond-ok")
		}
	})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	connection := dialLease(t, lease)
	reader := bufio.NewReader(connection)
	request := "GET http://o.pki.goog/x HTTP/1.1\r\nHost: o.pki.goog\r\n\r\n"
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatal(err)
	}
	first, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 响应体还没发完时就发下一个请求
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	close(bodyRest)
	if body, err := io.ReadAll(first.Body); err != nil || string(body) != "first-ok" {
		t.Fatalf("第一个响应体 = %q: %v", body, err)
	}
	second, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("流水线发来的第二个请求没有得到响应: %v", err)
	}
	if body, err := io.ReadAll(second.Body); err != nil || string(body) != "second-ok" {
		t.Fatalf("第二个响应体 = %q: %v", body, err)
	}
	if accepted := fake.accepted.Load(); accepted != 1 {
		t.Fatalf("两个请求应走同一条上游连接，上游收到 %d 条连接", accepted)
	}
}

func TestPlainHTTPUpstreamIdleClose(t *testing.T) {
	for name, idle := range map[string]string{"上游空闲断开": "", "上游先发 408": "HTTP/1.1 408 Request Timeout\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"} {
		t.Run(name, func(t *testing.T) {
			registry, _ := newTestRegistry(t, Options{})
			fake := startUpstream(t, func(connection net.Conn) {
				reader := bufio.NewReader(connection)
				if _, err := readHeadRaw(reader); err != nil {
					return
				}
				_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
				time.Sleep(50 * time.Millisecond)
				if idle != "" {
					_, _ = io.WriteString(connection, idle)
				}
			})
			lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
			connection := dialLease(t, lease)
			reader := bufio.NewReader(connection)
			if _, err := io.WriteString(connection, "GET http://o.pki.goog/x HTTP/1.1\r\nHost: o.pki.goog\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(response.Body)
			rest, err := io.ReadAll(reader)
			if err != nil || len(rest) != 0 {
				t.Fatalf("上游空闲断开后客户端收到 %q: %v", rest, err)
			}
			if failure := lease.FailureSince(time.Time{}, nil); failure != "" {
				t.Fatalf("空闲断开记了失败：%q", failure)
			}
		})
	}
}

func TestPlainHTTPReusedRequestFailure(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	reset := func(connection net.Conn) {
		if tcp, ok := connection.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = connection.Close()
	}
	fake := startUpstream(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		reset(connection)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	connection := dialLease(t, lease)
	reader := bufio.NewReader(connection)
	request := "GET http://o.pki.goog/x HTTP/1.1\r\nHost: o.pki.goog\r\n\r\n"
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(reader); err != nil || len(rest) != 0 {
		t.Fatalf("复用连接上的请求失败后客户端收到 %q: %v", rest, err)
	}
	if failure := lease.FailureSince(time.Time{}, nil); failure != "" {
		t.Fatalf("复用连接上的失败记了失败：%q", failure)
	}

	first := startUpstream(t, func(connection net.Conn) {
		if _, err := readHeadRaw(bufio.NewReader(connection)); err != nil {
			return
		}
		reset(connection)
	})
	firstLease := acquireLease(t, registry, httpUpstreamURL(first.address()))
	if response := sendAndReadAll(t, firstLease, request); response != "HTTP/1.1 502 Bad Upstream Response\r\nContent-Length: 0\r\nConnection: close\r\n\r\n" {
		t.Fatalf("首个请求失败时 = %q", response)
	}
	if failure := firstLease.FailureSince(time.Time{}, nil); failure != textMalformed {
		t.Fatalf("失败说明 = %q", failure)
	}
}

func TestPlainHTTPBodyNotLimitedByHeaderTimeout(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{HeaderTimeout: 50 * time.Millisecond})
	received := make(chan string, 1)
	fake := startUpstream(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		body := make([]byte, 5)
		_, err := io.ReadFull(reader, body)
		received <- string(body)
		if err == nil {
			_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
		}
		_, _ = io.Copy(io.Discard, reader)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	connection := dialLease(t, lease)
	if _, err := io.WriteString(connection, "POST http://o.pki.goog/ HTTP/1.1\r\nHost: o.pki.goog\r\nContent-Length: 5\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := io.WriteString(connection, "hello"); err != nil {
		t.Fatal(err)
	}
	if body := <-received; body != "hello" {
		t.Fatalf("上游收到的请求体 = %q", body)
	}
	if response, err := http.ReadResponse(bufio.NewReader(connection), nil); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("响应 %v: %v", response, err)
	}
}

func TestPlainThenConnectSameUpstream(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	fake := startUpstream(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		head, err := readHeadRaw(reader)
		if err != nil || !strings.HasPrefix(head, "CONNECT a.example:443 HTTP/1.1\r\n") {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\n\r\n")
		echoUntilEOF(connection, reader)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	connection := dialLease(t, lease)
	reader := bufio.NewReader(connection)
	if _, err := io.WriteString(connection, "GET http://o.pki.goog/x HTTP/1.1\r\nHost: o.pki.goog\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	if _, err := io.WriteString(connection, connectRequest("a.example:443")); err != nil {
		t.Fatal(err)
	}
	status, err := readHeadRaw(reader)
	if err != nil || status != "HTTP/1.1 200 Connection established\r\n\r\n" {
		t.Fatalf("CONNECT 响应 = %q: %v", status, err)
	}
	if _, err := io.WriteString(connection, "tunnel"); err != nil {
		t.Fatal(err)
	}
	echoed := make([]byte, 6)
	if _, err := io.ReadFull(reader, echoed); err != nil || string(echoed) != "tunnel" {
		t.Fatalf("回显 = %q: %v", echoed, err)
	}
	if fake.accepted.Load() != 1 {
		t.Fatalf("上游连接数 = %d", fake.accepted.Load())
	}
}

func TestHeaderLimits(t *testing.T) {
	registry, recorder := newTestRegistry(t, Options{FirstByteTimeout: 100 * time.Millisecond})
	fake := startUpstream(t, func(connection net.Conn) {})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	large := "CONNECT a.example:443 HTTP/1.1\r\nX-Large: " + strings.Repeat("a", 65<<10) + "\r\n\r\n"
	connection := dialLease(t, lease)
	go func() { _, _ = io.WriteString(connection, large) }()
	if response := readAllResponse(t, connection); response != responseHeaderTooLarge {
		t.Fatalf("超长请求头的响应 = %q", response)
	}
	many := "CONNECT a.example:443 HTTP/1.1\r\n" + strings.Repeat("X-Line: 1\r\n", 101) + "\r\n"
	if response := sendAndReadAll(t, lease, many); response != responseHeaderTooLarge {
		t.Fatalf("超过 100 行的响应 = %q", response)
	}
	before := len(recorder.snapshot())
	idle := dialLease(t, lease)
	started := time.Now()
	if data, err := io.ReadAll(idle); err != nil || len(data) != 0 {
		t.Fatalf("没有首字节时 = %q: %v", data, err)
	}
	if elapsed := time.Since(started); elapsed < 80*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("首字节超时用了 %s", elapsed)
	}
	if lines := recorder.snapshot(); len(lines) != before {
		t.Fatalf("安静关闭记了日志：%q", lines[before:])
	}
	if fake.accepted.Load() != 0 {
		t.Fatal("被拒绝的请求连了上游")
	}
}

func TestHalfClose(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	received := make(chan string, 1)
	fake := startUpstream(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\n\r\n")
		data, _ := io.ReadAll(reader)
		received <- string(data)
		_, _ = io.WriteString(connection, "reply after client EOF")
	})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	tunnel := openTunnel(t, lease, "a.example:443")
	if _, err := io.WriteString(tunnel, "request then half close"); err != nil {
		t.Fatal(err)
	}
	if err := tunnel.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got != "request then half close" {
		t.Fatalf("上游收到 %q", got)
	}
	if reply := readAllResponse(t, tunnel); reply != "reply after client EOF" {
		t.Fatalf("客户端收到 %q", reply)
	}
}

func TestHalfCloseGrace(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{HalfCloseGrace: 100 * time.Millisecond})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	gotEOF := make(chan struct{})
	fake := startUpstream(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\n\r\n")
		_, _ = io.Copy(io.Discard, reader)
		close(gotEOF)
		// 上游一直不关
		<-release
	})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	baseline := runtime.NumGoroutine()
	tunnel := openTunnel(t, lease, "a.example:443")
	if err := tunnel.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	<-gotEOF
	started := time.Now()
	if data := readAllResponse(t, tunnel); data != "" {
		t.Fatalf("客户端收到 %q", data)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("兜底计时用了 %s", elapsed)
	}
	_ = tunnel.Close()
	close(release)
	waitFor(t, "goroutine 回落", func() bool { return runtime.NumGoroutine() <= baseline })
}

func TestPlainHTTPInterimAndUpgrade(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	fake := startUpstream(t, func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 103 Early Hints\r\nLink: </style.css>\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nWELCOME")
		echoUntilEOF(connection, reader)
	})
	lease := acquireLease(t, registry, httpUpstreamURL(fake.address()))
	connection := dialLease(t, lease)
	reader := bufio.NewReader(connection)
	if _, err := io.WriteString(connection, "GET http://o.example/ HTTP/1.1\r\nHost: o.example\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if head, err := readHeadRaw(reader); err != nil || head != "HTTP/1.1 103 Early Hints\r\nLink: </style.css>\r\n\r\n" {
		t.Fatalf("1xx 响应 = %q: %v", head, err)
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("最终响应 %v: %v", response, err)
	}
	_, _ = io.ReadAll(response.Body)
	if _, err := io.WriteString(connection, "GET http://ws.example/ HTTP/1.1\r\nHost: ws.example\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	head, err := readHeadRaw(reader)
	if err != nil || !strings.HasPrefix(head, "HTTP/1.1 101 Switching Protocols\r\n") || strings.Contains(head, "Content-Length") {
		t.Fatalf("101 响应 = %q: %v", head, err)
	}
	welcome := make([]byte, len("WELCOME"))
	if _, err := io.ReadFull(reader, welcome); err != nil || string(welcome) != "WELCOME" {
		t.Fatalf("101 之后的字节 = %q: %v", welcome, err)
	}
	if _, err := io.WriteString(connection, "frame"); err != nil {
		t.Fatal(err)
	}
	echoed := make([]byte, 5)
	if _, err := io.ReadFull(reader, echoed); err != nil || string(echoed) != "frame" {
		t.Fatalf("升级后回显 = %q: %v", echoed, err)
	}
}
