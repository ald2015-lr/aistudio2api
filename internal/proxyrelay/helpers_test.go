package proxyrelay

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testUser = "user-session-XYZ"
	// testPassword 带上需要百分号编码的字符，覆盖编码后的形式
	testPassword = "S3cr3t-PW/@:%x+"
	testTimeout  = 5 * time.Second
)

// testAuthorization 为测试凭据注入的 Proxy-Authorization 值
func testAuthorization() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(testUser+":"+testPassword))
}

// testUserinfo 为规范化写法的 用户名:密码
func testUserinfo() string {
	return escapeStrict(testUser) + ":" + escapeStrict(testPassword)
}

func httpUpstreamURL(address string) string {
	return "http://" + testUserinfo() + "@" + address
}

func socksUpstreamURL(address string) string {
	return "socks5://" + testUserinfo() + "@" + address
}

// logRecorder 收集注册表日志
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (recorder *logRecorder) log(level, message string) {
	recorder.mu.Lock()
	recorder.lines = append(recorder.lines, level+" "+message)
	recorder.mu.Unlock()
}

func (recorder *logRecorder) snapshot() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.lines...)
}

func (recorder *logRecorder) matching(substring string) []string {
	var lines []string
	for _, line := range recorder.snapshot() {
		if strings.Contains(line, substring) {
			lines = append(lines, line)
		}
	}
	return lines
}

// fakeClock 为可拨动的时钟
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

func newTestRegistry(t *testing.T, options Options) (*Registry, *logRecorder) {
	t.Helper()
	recorder := &logRecorder{}
	if options.Log == nil {
		options.Log = recorder.log
	}
	registry := NewRegistry(options)
	t.Cleanup(func() { _ = registry.Close() })
	return registry, recorder
}

func acquireLease(t *testing.T, registry *Registry, upstream string) *Lease {
	t.Helper()
	lease, err := registry.Acquire(upstream, "a@example.com")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !lease.Relayed() {
		t.Fatalf("带账号密码的上游没有经过中转：%q", lease.Address())
	}
	return lease
}

func (registry *Registry) entryCount() int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return len(registry.entries)
}

func (registry *Registry) hasEntry(entry *relayEntry) bool {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	_, ok := registry.entries[entry]
	return ok
}

// fakeUpstream 为回环上的假上游，每条连接交给 handler
type fakeUpstream struct {
	listener net.Listener
	accepted atomic.Int32
	mu       sync.Mutex
	conns    []net.Conn
	handlers sync.WaitGroup
}

func startUpstream(t *testing.T, handler func(net.Conn)) *fakeUpstream {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动假上游: %v", err)
	}
	upstream := &fakeUpstream{listener: listener}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			upstream.accepted.Add(1)
			upstream.mu.Lock()
			upstream.conns = append(upstream.conns, connection)
			upstream.mu.Unlock()
			upstream.handlers.Add(1)
			go func() {
				defer upstream.handlers.Done()
				defer connection.Close()
				handler(connection)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		upstream.mu.Lock()
		for _, connection := range upstream.conns {
			_ = connection.Close()
		}
		upstream.mu.Unlock()
		upstream.handlers.Wait()
	})
	return upstream
}

func (upstream *fakeUpstream) address() string {
	return upstream.listener.Addr().String()
}

// closedPort 返回一个当前没有监听的回环端口
func closedPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

// readHeadRaw 读到空行，返回原始字节
func readHeadRaw(reader *bufio.Reader) (string, error) {
	var head strings.Builder
	for {
		line, err := reader.ReadString('\n')
		head.WriteString(line)
		if err != nil {
			return head.String(), err
		}
		if line == "\r\n" || line == "\n" {
			return head.String(), nil
		}
	}
}

// serveConnectEcho 为假 HTTP 代理：校验注入的认证，回 200，之后回显到对端半关闭
func serveConnectEcho(t *testing.T) func(net.Conn) {
	return func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		head, err := readHeadRaw(reader)
		if err != nil {
			return
		}
		if !strings.Contains(head, "\r\nProxy-Authorization: "+testAuthorization()+"\r\n") {
			t.Errorf("上游收到的请求头没有注入的认证：%q", head)
			_, _ = io.WriteString(connection, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
			return
		}
		if _, err := io.WriteString(connection, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
			return
		}
		echoUntilEOF(connection, reader)
	}
}

// echoUntilEOF 把读到的数据原样写回，读到 EOF 后半关闭
func echoUntilEOF(connection net.Conn, reader io.Reader) {
	if _, err := io.Copy(connection, reader); err != nil {
		return
	}
	_ = closeWrite(connection)
}

// respondConnect 为假 HTTP 代理：读完请求头后写 response 并关闭
func respondConnect(response string) func(net.Conn) {
	return func(connection net.Conn) {
		reader := bufio.NewReader(connection)
		if _, err := readHeadRaw(reader); err != nil {
			return
		}
		_, _ = io.WriteString(connection, response)
	}
}

func dialLease(t *testing.T, lease *Lease) net.Conn {
	t.Helper()
	address := lease.Address()
	address = address[strings.Index(address, "://")+3:]
	connection, err := net.DialTimeout("tcp", address, testTimeout)
	if err != nil {
		t.Fatalf("连接本机中转: %v", err)
	}
	_ = connection.SetDeadline(time.Now().Add(testTimeout))
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

// connectRequest 为 Firefox 形态的 CONNECT 请求头
func connectRequest(target string) string {
	return "CONNECT " + target + " HTTP/1.1\r\n" +
		"User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:152.0) Gecko/20100101 Firefox/152.0\r\n" +
		"Proxy-Connection: keep-alive\r\n" +
		"Connection: keep-alive\r\n" +
		"Host: " + target + "\r\n\r\n"
}

// openTunnel 经中转建一条 CONNECT 隧道
func openTunnel(t *testing.T, lease *Lease, target string) net.Conn {
	t.Helper()
	connection := dialLease(t, lease)
	if _, err := io.WriteString(connection, connectRequest(target)); err != nil {
		t.Fatalf("写 CONNECT: %v", err)
	}
	expected := "HTTP/1.1 200 Connection established\r\n\r\n"
	buffer := make([]byte, len(expected))
	if _, err := io.ReadFull(connection, buffer); err != nil {
		t.Fatalf("读 CONNECT 响应: %v", err)
	}
	if string(buffer) != expected {
		t.Fatalf("CONNECT 响应 = %q", buffer)
	}
	return connection
}

// requireEcho 写一段数据，读回同样的字节
func requireEcho(t *testing.T, connection net.Conn, payload string) {
	t.Helper()
	if _, err := io.WriteString(connection, payload); err != nil {
		t.Fatalf("写隧道: %v", err)
	}
	buffer := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, buffer); err != nil {
		t.Fatalf("读隧道: %v", err)
	}
	if string(buffer) != payload {
		t.Fatalf("回显 = %q，期望 %q", buffer, payload)
	}
}

// readAllResponse 读到 EOF，要求是干净的 EOF 而不是 RST
func readAllResponse(t *testing.T, connection net.Conn) string {
	t.Helper()
	data, err := io.ReadAll(connection)
	if err != nil {
		t.Fatalf("读到 EOF 之前出错（%q）: %v", data, err)
	}
	return string(data)
}

// sendAndReadAll 发一段原始请求后读到 EOF
func sendAndReadAll(t *testing.T, lease *Lease, request string) string {
	t.Helper()
	connection := dialLease(t, lease)
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatalf("写请求: %v", err)
	}
	return readAllResponse(t, connection)
}

// waitFor 轮询直到条件成立
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// listenerClosed 判断中转端口是否已经连不上
func listenerClosed(lease *Lease) bool {
	address := lease.Address()
	address = address[strings.Index(address, "://")+3:]
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return true
	}
	_ = connection.Close()
	return false
}

// isEOF 判断读到的是干净的 EOF
func isEOF(err error) bool {
	return errors.Is(err, io.EOF)
}

// socksBehavior 为假 SOCKS5 上游的行为
type socksBehavior struct {
	method       byte // 方法选择，默认 02
	badAuth      bool // RFC1929 回 STATUS=01
	reply        byte // CONNECT 的 REP
	replyVersion byte // CONNECT 回复的 VER，默认 05
	silent       bool // 读完问候后不回复
}

// socksRecord 为假 SOCKS5 上游收到的字节
type socksRecord struct {
	greeting []byte
	username string
	password string
	request  []byte // ATYP 起的地址与端口
}

// serveSOCKSUpstream 为假 SOCKS5 上游：要求账号密码，成功后回显
func serveSOCKSUpstream(t *testing.T, behavior socksBehavior, records chan<- socksRecord) func(net.Conn) {
	return func(connection net.Conn) {
		_ = connection.SetDeadline(time.Now().Add(testTimeout))
		var record socksRecord
		defer func() {
			if records != nil {
				records <- record
			}
		}()
		header := make([]byte, 2)
		if _, err := io.ReadFull(connection, header); err != nil {
			return
		}
		methods := make([]byte, header[1])
		if _, err := io.ReadFull(connection, methods); err != nil {
			return
		}
		record.greeting = append(header, methods...)
		if behavior.silent {
			_, _ = io.Copy(io.Discard, connection)
			return
		}
		method := behavior.method
		if method == 0 {
			method = 0x02
		}
		if _, err := connection.Write([]byte{0x05, method}); err != nil {
			return
		}
		if method == 0xFF {
			return
		}
		if method == 0x02 {
			prefix := make([]byte, 2)
			if _, err := io.ReadFull(connection, prefix); err != nil {
				return
			}
			username := make([]byte, prefix[1])
			if _, err := io.ReadFull(connection, username); err != nil {
				return
			}
			length := make([]byte, 1)
			if _, err := io.ReadFull(connection, length); err != nil {
				return
			}
			password := make([]byte, length[0])
			if _, err := io.ReadFull(connection, password); err != nil {
				return
			}
			record.username, record.password = string(username), string(password)
			status := byte(0x00)
			if behavior.badAuth || record.username != testUser || record.password != testPassword {
				status = 0x01
			}
			if _, err := connection.Write([]byte{0x01, status}); err != nil || status != 0 {
				return
			}
		}
		request := make([]byte, 4)
		if _, err := io.ReadFull(connection, request); err != nil {
			return
		}
		var address []byte
		switch request[3] {
		case 0x01, 0x04:
			address = make([]byte, 4)
			if request[3] == 0x04 {
				address = make([]byte, 16)
			}
			if _, err := io.ReadFull(connection, address); err != nil {
				return
			}
		case 0x03:
			length := make([]byte, 1)
			if _, err := io.ReadFull(connection, length); err != nil {
				return
			}
			domain := make([]byte, length[0])
			if _, err := io.ReadFull(connection, domain); err != nil {
				return
			}
			address = append(length, domain...)
		default:
			return
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(connection, port); err != nil {
			return
		}
		record.request = append(append([]byte{request[3]}, address...), port...)
		version := behavior.replyVersion
		if version == 0 {
			version = 0x05
		}
		if _, err := connection.Write([]byte{version, behavior.reply, 0x00, 0x01, 10, 0, 0, 1, 0x1f, 0x90}); err != nil {
			return
		}
		if behavior.reply != 0 || version != 0x05 {
			return
		}
		_ = connection.SetDeadline(time.Time{})
		echoUntilEOF(connection, connection)
	}
}

// socksGreet 完成 Firefox 形态的问候（05 01 00）
func socksGreet(t *testing.T, connection net.Conn) {
	t.Helper()
	if _, err := connection.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("写问候: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(connection, reply); err != nil {
		t.Fatalf("读方法选择: %v", err)
	}
	if !bytes.Equal(reply, []byte{0x05, 0x00}) {
		t.Fatalf("方法选择 = % x", reply)
	}
}

// socksDomainRequest 为 ATYP=3 的 CONNECT
func socksDomainRequest(host string, port uint16) []byte {
	request := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	request = append(request, host...)
	return append(request, byte(port>>8), byte(port))
}

// socksRequestReply 发一个 SOCKS5 请求，读 10 字节回复
func socksRequestReply(t *testing.T, connection net.Conn, request []byte) []byte {
	t.Helper()
	if _, err := connection.Write(request); err != nil {
		t.Fatalf("写请求: %v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(connection, reply); err != nil {
		t.Fatalf("读回复: %v", err)
	}
	return reply
}
