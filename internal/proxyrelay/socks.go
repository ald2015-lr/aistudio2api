package proxyrelay

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"time"
)

const (
	socksReplySucceeded          = 0x00
	socksReplyHostUnreachable    = 0x04
	socksReplyCommandUnsupported = 0x07
	socksReplyAddressUnsupported = 0x08
)

// socksRequest 为浏览器发来的 SOCKS5 CONNECT：raw 为 ATYP、地址与端口的原始字节，连上游时原样使用，域名不在本地解析
type socksRequest struct {
	raw  []byte
	host string // 只用于失败记录与日志
	port uint16
}

func (request socksRequest) display() string {
	return net.JoinHostPort(request.host, strconv.Itoa(int(request.port)))
}

// socksReply 为 10 字节的回复：05 REP 00 01 00 00 00 00 00 00。失败回复也写满 10 字节，
// Firefox 读回复时先要凑齐 5 字节，两字节的简写会让它凑不齐
func socksReply(reply byte) []byte {
	return []byte{0x05, reply, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
}

// serveSOCKS 为 socks5 上游提供本地 SOCKS5 中转（服务端最小实现）。所有失败都回 REP=04：
// Firefox 把它映射成 NS_ERROR_UNKNOWN_HOST，不在回退直连的错误列表里；真实原因只进失败记录和日志
func (entry *relayEntry) serveSOCKS(client net.Conn) {
	options := entry.registry.options
	reader := bufio.NewReaderSize(client, 4096)
	_ = client.SetReadDeadline(time.Now().Add(options.FirstByteTimeout))
	version, err := reader.ReadByte()
	if err != nil {
		return
	}
	if version != 0x05 {
		// 例如 HTTP 请求打到这个端口，第一个字节是 G
		return
	}
	_ = client.SetReadDeadline(time.Now().Add(options.HeaderTimeout))
	count, err := reader.ReadByte()
	if err != nil {
		return
	}
	methods := make([]byte, count)
	if _, err := io.ReadFull(reader, methods); err != nil {
		return
	}
	acceptable := false
	for _, method := range methods {
		if method == 0x00 {
			acceptable = true
			break
		}
	}
	if !acceptable {
		// 方法选择回复本来就是 2 字节，Firefox 也只读 2 字节
		lingerClose(client, "\x05\xff")
		return
	}
	if _, err := client.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	request, reply, ok := readSOCKSRequest(reader)
	if !ok {
		return
	}
	if reply != socksReplySucceeded {
		lingerClose(client, string(socksReply(reply)))
		return
	}
	_ = client.SetReadDeadline(time.Time{})
	acquired, scope, limit := entry.acquireSlot()
	if !acquired {
		entry.report(entry.failureBusy(scope, limit))
		lingerClose(client, string(socksReply(socksReplyHostUnreachable)))
		return
	}
	defer entry.releaseSlot()
	// 先连上游、再回复浏览器
	upstream, failure := entry.dialUpstream()
	if failure == nil {
		failure = entry.socksConnect(upstream, request)
	}
	if failure != nil {
		entry.closeUpstream(upstream)
		entry.report(failure)
		if failure.kind != "" {
			lingerClose(client, string(socksReply(socksReplyHostUnreachable)))
		}
		return
	}
	defer entry.closeUpstream(upstream)
	if _, err := client.Write(socksReply(socksReplySucceeded)); err != nil {
		return
	}
	if buffered := reader.Buffered(); buffered > 0 {
		pending, _ := reader.Peek(buffered)
		if _, err := upstream.raw.Write(pending); err != nil {
			return
		}
	}
	pipe(client, upstream.raw, options.HalfCloseGrace)
}

// readSOCKSRequest 先完整读完 VER CMD RSV ATYP DST.ADDR DST.PORT 再判断；返回要回的 REP（成功为 0），
// 读失败或 VER 不对时 ok 为 false，直接关闭
func readSOCKSRequest(reader *bufio.Reader) (socksRequest, byte, bool) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return socksRequest{}, 0, false
	}
	if header[0] != 0x05 {
		return socksRequest{}, 0, false
	}
	var address []byte
	var host string
	switch header[3] {
	case 0x01:
		address = make([]byte, net.IPv4len)
		if _, err := io.ReadFull(reader, address); err != nil {
			return socksRequest{}, 0, false
		}
		host = net.IP(address).String()
	case 0x04:
		address = make([]byte, net.IPv6len)
		if _, err := io.ReadFull(reader, address); err != nil {
			return socksRequest{}, 0, false
		}
		host = net.IP(address).String()
	case 0x03:
		length, err := reader.ReadByte()
		if err != nil {
			return socksRequest{}, 0, false
		}
		address = make([]byte, 1+int(length))
		address[0] = length
		if _, err := io.ReadFull(reader, address[1:]); err != nil {
			return socksRequest{}, 0, false
		}
		host = string(address[1:])
	default:
		// 地址长度未知，读不完整个请求，直接回 REP=08
		return socksRequest{}, socksReplyAddressUnsupported, true
	}
	var port [2]byte
	if _, err := io.ReadFull(reader, port[:]); err != nil {
		return socksRequest{}, 0, false
	}
	if header[1] != 0x01 {
		return socksRequest{}, socksReplyCommandUnsupported, true
	}
	if header[3] == 0x03 && host == "" {
		return socksRequest{}, socksReplyAddressUnsupported, true
	}
	number := uint16(port[0])<<8 | uint16(port[1])
	if number == 0 {
		return socksRequest{}, socksReplyHostUnreachable, true
	}
	raw := make([]byte, 0, 1+len(address)+2)
	raw = append(raw, header[3])
	raw = append(raw, address...)
	raw = append(raw, port[:]...)
	return socksRequest{raw: raw, host: host, port: number}, socksReplySucceeded, true
}
