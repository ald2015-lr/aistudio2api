package proxyrelay

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

// secretAcceptError 模拟一个把凭据带进文案的 Accept 错误
type secretAcceptError struct{}

func (secretAcceptError) Error() string {
	return "accept failed for " + testUser + ":" + testPassword + " " + url.QueryEscape(testPassword)
}
func (secretAcceptError) Timeout() bool   { return false }
func (secretAcceptError) Temporary() bool { return true }

type secretListener struct {
	net.Listener
	failed bool
}

func (listener *secretListener) Accept() (net.Conn, error) {
	if !listener.failed {
		listener.failed = true
		return nil, secretAcceptError{}
	}
	return listener.Listener.Accept()
}

func TestNoCredentialsInLogsOrErrors(t *testing.T) {
	recorder := &logRecorder{}
	var collected []string
	collect := func(values ...string) { collected = append(collected, values...) }
	cause := errors.New("加载 AI Studio 页面: NS_ERROR_PROXY_BAD_GATEWAY")
	newRegistry := func(options Options) *Registry {
		options.Log = recorder.log
		registry, _ := newTestRegistry(t, options)
		return registry
	}
	since := time.Now()
	finish := func(lease *Lease) {
		collect(lease.FailureSince(since, GoogleTarget), lease.FailureSince(since, nil))
		if wrapped := lease.WrapFailure(cause, since, GoogleTarget); wrapped != nil {
			collect(wrapped.Error())
		}
	}
	httpCase := func(options Options, handler func(net.Conn), request string) {
		registry := newRegistry(options)
		address := closedPort(t)
		if handler != nil {
			address = startUpstream(t, handler).address()
		}
		lease := acquireLease(t, registry, httpUpstreamURL(address))
		collect(lease.Address(), sendAndReadAll(t, lease, request))
		finish(lease)
	}
	connect := connectRequest("aistudio.google.com:443")
	plain := "GET http://o.pki.goog/x HTTP/1.1\r\nHost: o.pki.goog\r\n\r\n"
	httpCase(Options{}, respondConnect("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\""+testUser+"\"\r\n\r\n"), connect)
	httpCase(Options{}, respondConnect("HTTP/1.1 403 Forbidden for "+testUser+" "+testPassword+"\r\nContent-Length: 0\r\n\r\n"), connect)
	httpCase(Options{}, nil, connect)
	httpCase(Options{UpstreamTimeout: 100 * time.Millisecond}, func(connection net.Conn) { _, _ = io.Copy(io.Discard, connection) }, connect)
	httpCase(Options{}, respondConnect("garbage "+testPassword+"\r\n\r\n"), connect)
	httpCase(Options{}, respondConnect("HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n"), plain)
	httpCase(Options{}, func(connection net.Conn) {
		if _, err := readHeadRaw(bufio.NewReader(connection)); err == nil {
			_ = connection.(*net.TCPConn).SetLinger(0)
		}
	}, plain)
	httpCase(Options{}, respondConnect(""), "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")

	// https 上游证书不受信任
	tlsRegistry := newRegistry(Options{RootCAs: x509.NewCertPool()})
	certificate, _ := selfSignedCertificate(t, "proxy.test")
	tlsUpstream := startUpstream(t, func(connection net.Conn) {
		_ = tls.Server(connection, &tls.Config{Certificates: []tls.Certificate{certificate}}).Handshake()
	})
	tlsLease := acquireLease(t, tlsRegistry, "https://"+testUserinfo()+"@"+tlsUpstream.address())
	collect(sendAndReadAll(t, tlsLease, connect))
	finish(tlsLease)

	// 连接数已满
	busyRegistry := newRegistry(Options{MaxConnections: 1})
	busyLease := acquireLease(t, busyRegistry, httpUpstreamURL(startUpstream(t, serveConnectEcho(t)).address()))
	requireEcho(t, openTunnel(t, busyLease, "a.example:443"), "x")
	collect(sendAndReadAll(t, busyLease, connect))
	finish(busyLease)

	// SOCKS5 的各类失败
	for _, behavior := range []*socksBehavior{{badAuth: true}, {method: 0xFF}, {reply: 0x02}, {replyVersion: 0x04}, nil} {
		registry := newRegistry(Options{})
		address := closedPort(t)
		if behavior != nil {
			address = startUpstream(t, serveSOCKSUpstream(t, *behavior, nil)).address()
		}
		lease := acquireLease(t, registry, socksUpstreamURL(address))
		connection := dialLease(t, lease)
		socksGreet(t, connection)
		if _, err := connection.Write(socksDomainRequest("aistudio.google.com", 443)); err != nil {
			t.Fatal(err)
		}
		collect(readAllResponse(t, connection))
		finish(lease)
	}

	// Accept 错误的文案
	acceptRegistry := newRegistry(Options{})
	acceptRegistry.listen = func(ctx context.Context) (net.Listener, error) {
		listener, err := listenLoopback(ctx)
		return &secretListener{Listener: listener}, err
	}
	acceptLease := acquireLease(t, acceptRegistry, httpUpstreamURL(closedPort(t)))
	sendAndReadAll(t, acceptLease, connect)
	if len(recorder.matching("本机代理中转接受连接失败")) != 1 {
		t.Fatalf("没有记下接受连接失败：%q", recorder.snapshot())
	}

	// 释放与关闭
	quietRegistry := newRegistry(Options{IdleGrace: 20 * time.Millisecond})
	quietLease := acquireLease(t, quietRegistry, socksUpstreamURL(closedPort(t)))
	quietLease.Release()
	waitFor(t, "监听器关闭", func() bool { return quietRegistry.entryCount() == 0 })

	// Acquire 的错误
	for _, value := range []string{
		"ftp://" + testUserinfo() + "@h:1",
		"http://" + testUserinfo() + "@:1",
		"http://" + testUserinfo() + "@h:99999",
		"socks5://" + testUserinfo() + "@h",
		"http://" + testUser + ":" + testPassword + "@h:1",
		"http://" + testUser + ":%zz" + testPassword + "@h:1",
	} {
		if _, err := acceptRegistry.Acquire(value, "无效"); err != nil {
			collect(err.Error())
		}
	}
	closedRegistry := NewRegistry(Options{})
	_ = closedRegistry.Close()
	if _, err := closedRegistry.Acquire(httpUpstreamURL("h:1"), "关闭后"); err != nil {
		collect(err.Error())
	}

	// Describe 的输出
	upstream := httpUpstreamURL("h:1")
	for _, err := range []error{
		fmt.Errorf("proxyconnect tcp: dial %s: refused", upstream),
		errors.New("Proxy-Authorization: " + testAuthorization()),
		&net.OpError{Op: "dial", Net: "tcp", Addr: testAddr("h:1"), Err: errors.New(testPassword)},
		errors.New("unknown error " + testUser),
		errors.New(url.PathEscape(testPassword) + " " + escapeStrict(testPassword)),
	} {
		collect(Describe(err, upstream))
	}

	collect(recorder.snapshot()...)
	credentials := []byte(testUser + ":" + testPassword)
	forbidden := []string{
		testUser,
		testPassword,
		escapeStrict(testPassword),
		url.PathEscape(testPassword),
		url.QueryEscape(testPassword),
		base64.StdEncoding.EncodeToString(credentials),
		base64.RawStdEncoding.EncodeToString(credentials),
	}
	if len(collected) < 40 {
		t.Fatalf("收集的文本太少：%d", len(collected))
	}
	for _, text := range collected {
		for _, secret := range forbidden {
			if strings.Contains(text, secret) {
				t.Fatalf("文本泄露了凭据 %q：%q", secret, text)
			}
		}
	}
}

// 上游原因短语与目标里的凭据要先脱敏再截断与过滤：截断后只剩凭据的一段、过滤后只剩非 ASCII 凭据的残片、
// 被控制字符隔开的凭据，都不能漏出去
func TestTargetTextScrubbedBeforeTruncate(t *testing.T) {
	const unicodePassword = "pässwörd-Zeta9"
	cases := []struct {
		name     string
		raw      string
		reason   string
		fragment string
	}{
		{"密码跨过截断位置", httpUpstreamURL("h:1"), strings.Repeat("a", targetReasonLimit-4) + testPassword, testPassword[:4]},
		{"用户名跨过截断位置", httpUpstreamURL("h:1"), strings.Repeat("b", targetReasonLimit-6) + " " + testUser, testUser[:5]},
		{"控制字符隔开密码", httpUpstreamURL("h:1"), "denied " + testPassword[:6] + "\x01" + testPassword[6:], testPassword},
		{"非 ASCII 密码", "http://" + testUser + ":" + escapeStrict(unicodePassword) + "@h:1", "denied " + unicodePassword, "sswrd-Zeta9"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			entry := &relayEntry{upstream: upstreamTarget{raw: item.raw, display: "http://h:1"}}
			failure := entry.failureTargetHTTP(403, item.reason, "a.example", "a.example:443")
			for _, text := range []string{failure.text, failure.response, failure.targetStatus} {
				if strings.Contains(text, item.fragment) {
					t.Fatalf("文本漏出凭据片段 %q：%q", item.fragment, text)
				}
			}
			if len(failure.targetStatus) > len("403 ")+targetReasonLimit {
				t.Fatalf("状态没有截断：%q", failure.targetStatus)
			}
		})
	}
	entry := &relayEntry{upstream: upstreamTarget{raw: httpUpstreamURL("h:1")}}
	target := strings.Repeat("c", 296) + testPassword + ".example"
	if shown := entry.displayTarget(target); strings.Contains(shown, testPassword[:4]) || len(shown) > 300 {
		t.Fatalf("目标显示 = %q", shown)
	}
}
