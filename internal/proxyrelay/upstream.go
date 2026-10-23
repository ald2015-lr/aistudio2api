package proxyrelay

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// upstreamTarget 为解析后的上游代理；display 之外的字段都不能进日志
type upstreamTarget struct {
	scheme        string // http、https、socks5
	address       string // host:port，http 默认补 80、https 补 443
	serverName    string // https 上游的 TLS ServerName（主机名，IPv6 不带方括号）
	username      string
	password      string
	authorization string // "Basic " + base64(user:pass)，只用于 http/https 上游
	display       string // scheme://host:port，日志里只用它
	raw           string // 去掉首尾空白的原值，只用于 Scrub（由 Acquire 填入）
}

// parseUpstream 从带账号密码的上游 URL 得到 upstreamTarget；错误不含任何输入内容
func parseUpstream(parsed *url.URL) (upstreamTarget, error) {
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https", "socks5":
	default:
		return upstreamTarget{}, ErrInvalidUpstream
	}
	host := parsed.Hostname()
	if host == "" || parsed.User == nil {
		return upstreamTarget{}, ErrInvalidUpstream
	}
	port := parsed.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return upstreamTarget{}, ErrInvalidUpstream
		}
	}
	if number, err := strconv.ParseUint(port, 10, 16); err != nil || number == 0 {
		return upstreamTarget{}, ErrInvalidUpstream
	}
	username := parsed.User.Username()
	password, _ := parsed.User.Password()
	if username == "" {
		return upstreamTarget{}, ErrInvalidUpstream
	}
	if scheme == "socks5" && (len(username) > 255 || len(password) > 255) {
		// RFC1929 的 ULEN、PLEN 各只有 1 字节
		return upstreamTarget{}, ErrInvalidUpstream
	}
	address := net.JoinHostPort(host, port)
	target := upstreamTarget{
		scheme:     scheme,
		address:    address,
		serverName: host,
		username:   username,
		password:   password,
		display:    scheme + "://" + address,
	}
	if scheme != "socks5" {
		// 与 Go 侧 transport_browser.go 拼法一致：用户名:密码
		target.authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
	}
	return target, nil
}

// upstreamConn 为建好的上游连接：connection 用于读写（https 上游时是 *tls.Conn），raw 为底层 TCP 连接
type upstreamConn struct {
	connection net.Conn
	raw        net.Conn
	deadline   time.Time // 本次连上游的总期限，后续 CONNECT 或 SOCKS 握手沿用
}

// dialUpstream 在 UpstreamTimeout 总期限内连上游（TCP 或 TLS）。不感知客户端在握手期间断开：要感知就得读客户端，
// 会和缓冲区里待转发的流水线字节冲突；浏览器放弃后，上游握手最多再跑 UpstreamTimeout
func (entry *relayEntry) dialUpstream() (*upstreamConn, *relayFailure) {
	registry := entry.registry
	ctx, cancel := context.WithTimeout(registry.ctx, registry.options.UpstreamTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	raw, err := registry.options.DialContext(ctx, "tcp", entry.upstream.address)
	if err != nil {
		if registry.ctx.Err() != nil {
			return nil, failureSilent()
		}
		return nil, entry.dialFailure(err)
	}
	if !entry.addUpstream(raw) {
		_ = raw.Close()
		return nil, failureSilent()
	}
	result := &upstreamConn{connection: raw, raw: raw, deadline: deadline}
	if entry.upstream.scheme != "https" {
		return result, nil
	}
	client := tls.Client(raw, &tls.Config{
		ServerName: entry.upstream.serverName,
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
		RootCAs:    registry.options.RootCAs,
	})
	if err := client.HandshakeContext(ctx); err != nil {
		entry.closeUpstream(result)
		if registry.ctx.Err() != nil {
			return nil, failureSilent()
		}
		if isTimeout(err) {
			return nil, entry.failureTimeout()
		}
		return nil, entry.failureTLS()
	}
	result.connection = client
	return result, nil
}

// closeUpstream 关闭上游连接并注销
func (entry *relayEntry) closeUpstream(upstream *upstreamConn) {
	if upstream == nil {
		return
	}
	_ = upstream.connection.Close()
	if upstream.raw != upstream.connection {
		_ = upstream.raw.Close()
	}
	entry.removeUpstream(upstream.raw)
}

// dialFailure 把拨号错误归为 unreachable 或 timeout
func (entry *relayEntry) dialFailure(err error) *relayFailure {
	if isTimeout(err) {
		return entry.failureTimeout()
	}
	return entry.failureUnreachable(dialReason(err))
}

// dialReason 为拨号失败的简短中文原因，不含错误原文
func dialReason(err error) string {
	var dnsError *net.DNSError
	switch {
	case errors.As(err, &dnsError):
		return "主机名无法解析"
	case isConnectionRefused(err):
		return "连接被拒绝"
	case isNetworkUnreachable(err):
		return "网络不可达"
	case isConnectionReset(err):
		return "连接被重置"
	default:
		return "连接失败"
	}
}

// isTimeout 判断是否为超时类错误
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netError net.Error
	return errors.As(err, &netError) && netError.Timeout()
}

// socksConnect 在上游 SOCKS5 原始连接上做客户端握手（自写，不用 x/net）：问候与 Go 侧 x/net 相同，
// CONNECT 原样使用客户端发来的 ATYP、地址与端口字节；全部用 io.ReadFull 精确读取，不会多读隧道字节
func (entry *relayEntry) socksConnect(upstream *upstreamConn, request socksRequest) *relayFailure {
	connection := upstream.raw
	if err := connection.SetDeadline(upstream.deadline); err != nil {
		return entry.socksIOFailure(err)
	}
	// 问候：05 02 00 02，与 Go 侧 x/net 的问候相同（同一个上游看到的两条路径一致）
	if _, err := connection.Write([]byte{0x05, 0x02, 0x00, 0x02}); err != nil {
		return entry.socksIOFailure(err)
	}
	var selection [2]byte
	if _, err := io.ReadFull(connection, selection[:]); err != nil {
		return entry.socksIOFailure(err)
	}
	if selection[0] != 0x05 {
		return entry.failureMalformed()
	}
	switch selection[1] {
	case 0x00:
	case 0x02:
		message := make([]byte, 0, 3+len(entry.upstream.username)+len(entry.upstream.password))
		message = append(message, 0x01, byte(len(entry.upstream.username)))
		message = append(message, entry.upstream.username...)
		message = append(message, byte(len(entry.upstream.password)))
		message = append(message, entry.upstream.password...)
		if _, err := connection.Write(message); err != nil {
			return entry.socksIOFailure(err)
		}
		var status [2]byte
		if _, err := io.ReadFull(connection, status[:]); err != nil {
			return entry.socksIOFailure(err)
		}
		if status[1] != 0x00 {
			return entry.failureAuth("上游 SOCKS5 代理拒绝了账号密码，请检查用户名和密码", true)
		}
	case 0xFF:
		return entry.failureAuth("上游 SOCKS5 代理不接受账号密码认证", true)
	default:
		return entry.failureMalformed()
	}
	message := make([]byte, 0, 3+len(request.raw))
	message = append(message, 0x05, 0x01, 0x00)
	message = append(message, request.raw...)
	if _, err := connection.Write(message); err != nil {
		return entry.socksIOFailure(err)
	}
	var header [4]byte
	if _, err := io.ReadFull(connection, header[:]); err != nil {
		return entry.socksIOFailure(err)
	}
	if header[0] != 0x05 {
		return entry.failureMalformed()
	}
	if header[1] != 0x00 {
		return entry.failureTargetSOCKS(header[1], request.host, request.display())
	}
	var bound int
	switch header[3] {
	case 0x01:
		bound = net.IPv4len
	case 0x04:
		bound = net.IPv6len
	case 0x03:
		var length [1]byte
		if _, err := io.ReadFull(connection, length[:]); err != nil {
			return entry.socksIOFailure(err)
		}
		bound = int(length[0])
	default:
		return entry.failureMalformed()
	}
	if _, err := io.ReadFull(connection, make([]byte, bound+2)); err != nil {
		return entry.socksIOFailure(err)
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return entry.socksIOFailure(err)
	}
	return nil
}

// socksIOFailure 把握手阶段的读写错误归为 timeout 或 malformed
func (entry *relayEntry) socksIOFailure(err error) *relayFailure {
	if entry.registry.ctx.Err() != nil {
		return failureSilent()
	}
	if isTimeout(err) {
		return entry.failureTimeout()
	}
	return entry.failureMalformed()
}
