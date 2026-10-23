package proxyrelay

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

var socksFailureReply = []byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0}

// socksTunnel 经 SOCKS5 中转发一个请求，要求成功并回显
func socksTunnel(t *testing.T, lease *Lease, request []byte) net.Conn {
	t.Helper()
	connection := dialLease(t, lease)
	socksGreet(t, connection)
	reply := socksRequestReply(t, connection, request)
	if !bytes.Equal(reply, []byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("成功回复 = % x", reply)
	}
	return connection
}

func testSOCKS5Address(t *testing.T, request []byte) {
	registry, _ := newTestRegistry(t, Options{})
	records := make(chan socksRecord, 1)
	upstream := startUpstream(t, serveSOCKSUpstream(t, socksBehavior{}, records))
	lease := acquireLease(t, registry, socksUpstreamURL(upstream.address()))
	if !strings.HasPrefix(lease.Address(), "socks5://127.0.0.1:") {
		t.Fatalf("SOCKS5 中转地址 = %q", lease.Address())
	}
	connection := socksTunnel(t, lease, request)
	requireEcho(t, connection, "socks tunnel payload")
	_ = connection.Close()
	record := <-records
	if record.username != testUser || record.password != testPassword {
		t.Fatalf("上游收到的凭据 = %q / %q", record.username, record.password)
	}
	if !bytes.Equal(record.greeting, []byte{0x05, 0x02, 0x00, 0x02}) {
		t.Fatalf("上游收到的问候 = % x", record.greeting)
	}
	if !bytes.Equal(record.request, request[3:]) {
		t.Fatalf("上游收到的地址 = % x，客户端发的是 % x", record.request, request[3:])
	}
}

func TestSOCKS5Domain(t *testing.T) {
	testSOCKS5Address(t, socksDomainRequest("aistudio.google.com", 443))
}

func TestSOCKS5IPv4(t *testing.T) {
	testSOCKS5Address(t, []byte{0x05, 0x01, 0x00, 0x01, 142, 250, 1, 1, 0x01, 0xbb})
}

func TestSOCKS5IPv6(t *testing.T) {
	request := append([]byte{0x05, 0x01, 0x00, 0x04}, net.ParseIP("2001:db8::1")...)
	testSOCKS5Address(t, append(request, 0x01, 0xbb))
}

func TestSOCKS5PreservesATYP(t *testing.T) {
	// IP 字面量以 ATYP=3 发来时原样转发，不改成 ATYP=1；ATYP=1 也原样转发
	testSOCKS5Address(t, socksDomainRequest("203.0.113.7", 443))
	testSOCKS5Address(t, []byte{0x05, 0x01, 0x00, 0x01, 203, 0, 113, 7, 0x01, 0xbb})
}

func TestSOCKS5MethodRejected(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	upstream := startUpstream(t, func(connection net.Conn) {})
	lease := acquireLease(t, registry, socksUpstreamURL(upstream.address()))
	connection := dialLease(t, lease)
	if _, err := connection.Write([]byte{0x05, 0x01, 0x02}); err != nil {
		t.Fatal(err)
	}
	if reply := readAllResponse(t, connection); reply != "\x05\xff" {
		t.Fatalf("回复 = % x", reply)
	}
}

func TestSOCKS5UnsupportedCommand(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	upstream := startUpstream(t, func(connection net.Conn) {})
	lease := acquireLease(t, registry, socksUpstreamURL(upstream.address()))
	connection := dialLease(t, lease)
	socksGreet(t, connection)
	request := socksDomainRequest("aistudio.google.com", 443)
	request[1] = 0x02
	if _, err := connection.Write(request); err != nil {
		t.Fatal(err)
	}
	if reply := readAllResponse(t, connection); reply != "\x05\x07\x00\x01\x00\x00\x00\x00\x00\x00" {
		t.Fatalf("回复 = % x", reply)
	}
	if upstream.accepted.Load() != 0 {
		t.Fatal("不支持的命令连了上游")
	}
}

func TestSOCKS5UnknownATYP(t *testing.T) {
	registry, _ := newTestRegistry(t, Options{})
	upstream := startUpstream(t, func(connection net.Conn) {})
	lease := acquireLease(t, registry, socksUpstreamURL(upstream.address()))
	for _, request := range [][]byte{{0x05, 0x01, 0x00, 0x05}, {0x05, 0x01, 0x00, 0x03, 0x00, 0x01, 0xbb}} {
		connection := dialLease(t, lease)
		socksGreet(t, connection)
		if _, err := connection.Write(request); err != nil {
			t.Fatal(err)
		}
		if reply := readAllResponse(t, connection); reply != "\x05\x08\x00\x01\x00\x00\x00\x00\x00\x00" {
			t.Fatalf("% x 的回复 = % x", request, reply)
		}
	}
	connection := dialLease(t, lease)
	socksGreet(t, connection)
	if _, err := connection.Write(socksDomainRequest("aistudio.google.com", 0)); err != nil {
		t.Fatal(err)
	}
	if reply := readAllResponse(t, connection); reply != string(socksFailureReply) {
		t.Fatalf("端口为 0 的回复 = % x", reply)
	}
}

func TestSOCKS5FailureReplies(t *testing.T) {
	request := socksDomainRequest("aistudio.google.com", 443)
	cases := []struct {
		name     string
		behavior *socksBehavior // nil 表示上游没有监听
		options  Options
		expected string
		relevant func(string) bool
		logged   string
	}{
		{name: "上游认证失败", behavior: &socksBehavior{badAuth: true}, expected: textSOCKSAuthDenied, logged: "上游代理认证失败"},
		{name: "上游方法选择回 FF", behavior: &socksBehavior{method: 0xFF}, expected: textSOCKSAuthMethod, logged: "已向浏览器返回 SOCKS 失败"},
		{name: "上游连不上", expected: "连不上上游代理 %s（连接被拒绝）", logged: "原因=连接被拒绝"},
		{name: "上游超时", behavior: &socksBehavior{silent: true}, options: Options{UpstreamTimeout: 100 * time.Millisecond}, expected: "连接上游代理 %s 超时", logged: "原因=超时"},
		{name: "上游回 REP=05", behavior: &socksBehavior{reply: 0x05}, expected: "上游 SOCKS5 代理连接目标失败：05 拒绝连接（目标 aistudio.google.com）", relevant: GoogleTarget, logged: "状态=SOCKS 05 拒绝连接 | 目标=aistudio.google.com:443"},
		{name: "上游回复格式错误", behavior: &socksBehavior{replyVersion: 0x04}, expected: textMalformed, logged: "原因=响应无法解析"},
	}
	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) {
			registry, recorder := newTestRegistry(t, each.options)
			address := closedPort(t)
			if each.behavior != nil {
				address = startUpstream(t, serveSOCKSUpstream(t, *each.behavior, nil)).address()
			}
			lease := acquireLease(t, registry, socksUpstreamURL(address))
			connection := dialLease(t, lease)
			socksGreet(t, connection)
			if _, err := connection.Write(request); err != nil {
				t.Fatal(err)
			}
			if reply := readAllResponse(t, connection); reply != string(socksFailureReply) {
				t.Fatalf("回复 = % x", reply)
			}
			expected := each.expected
			if strings.Contains(expected, "%s") {
				expected = strings.ReplaceAll(expected, "%s", address)
			}
			if failure := lease.FailureSince(time.Time{}, each.relevant); failure != expected {
				t.Fatalf("失败说明 = %q，期望 %q", failure, expected)
			}
			if each.relevant != nil && lease.FailureSince(time.Time{}, nil) != "" {
				t.Fatal("按目标的失败在 relevant 为 nil 时被附加")
			}
			if lines := recorder.matching(each.logged); len(lines) != 1 {
				t.Fatalf("日志 = %q", recorder.snapshot())
			}
		})
	}
	t.Run("连接数已满", func(t *testing.T) {
		registry, _ := newTestRegistry(t, Options{MaxConnections: 1})
		upstream := startUpstream(t, serveSOCKSUpstream(t, socksBehavior{}, nil))
		lease := acquireLease(t, registry, socksUpstreamURL(upstream.address()))
		tunnel := socksTunnel(t, lease, request)
		requireEcho(t, tunnel, "occupied")
		connection := dialLease(t, lease)
		socksGreet(t, connection)
		if _, err := connection.Write(request); err != nil {
			t.Fatal(err)
		}
		if reply := readAllResponse(t, connection); reply != string(socksFailureReply) {
			t.Fatalf("回复 = % x", reply)
		}
		if failure := lease.FailureSince(time.Time{}, nil); failure != textBusy {
			t.Fatalf("失败说明 = %q", failure)
		}
	})
	t.Run("HTTP 请求打到 SOCKS 端口", func(t *testing.T) {
		registry, _ := newTestRegistry(t, Options{})
		lease := acquireLease(t, registry, socksUpstreamURL(closedPort(t)))
		connection := dialLease(t, lease)
		if _, err := io.WriteString(connection, "GET / HTTP/1.1\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 1)
		if read, _ := connection.Read(buffer); read != 0 {
			t.Fatal("非 SOCKS5 请求收到了回复")
		}
	})
}
