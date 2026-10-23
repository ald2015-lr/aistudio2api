package proxyrelay

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// maxHeadBytes、maxHeadLines 为请求头与上游响应头的上限
	maxHeadBytes = 64 << 10
	maxHeadLines = 100
	// plainResponseTimeout 为明文请求等上游响应头的上限，与 Firefox network.http.response.timeout 的 300 秒一致；
	// 只防 goroutine 与 fd 无限挂住，到期安静关闭
	plainResponseTimeout = 5 * time.Minute
	// lingerTimeout、lingerBytes 为写完失败回复后读掉对端剩余数据的上限，避免内核因未读数据发 RST
	lingerTimeout = time.Second
	lingerBytes   = 4 << 10
)

// errHeadTooLarge 表示头部超过 64 KiB 或 100 行
var errHeadTooLarge = errors.New("头部过长")

// errHeadMalformed 表示上游响应头无法解析
var errHeadMalformed = errors.New("响应头无法解析")

// httpSession 为一条浏览器连接：第一个请求上懒建上游连接，之后这条连接上的所有请求都走同一条上游连接，
// 不复用、不合并，“每条连接换 IP”的代理语义不变
type httpSession struct {
	entry    *relayEntry
	client   net.Conn
	reader   *bufio.Reader
	upstream *upstreamConn
	upReader *bufio.Reader
}

// serveHTTP 为 http/https 上游提供本地 HTTP 中转：CONNECT 与 absolute-form 明文请求
func (entry *relayEntry) serveHTTP(client net.Conn) {
	reader := bufio.NewReaderSize(client, 4096)
	ok, scope, limit := entry.acquireSlot()
	if !ok {
		// 不等待，直接回协议层错误：Firefox 把 CONNECT 的 503 映射成 NS_ERROR_CONNECTION_REFUSED，不在回退直连的列表里
		entry.report(entry.failureBusy(scope, limit))
		// 回复前先读完请求头，避免对端数据没读完就关闭、内核发 RST；第一个字节也按 HeaderTimeout 等（整个请求头共用这一个期限），
		// 不按 FirstByteTimeout：被拒的连接不计入上限，浏览器预连接又不发字节时不能各自占着 fd 等几分钟
		_ = client.SetReadDeadline(time.Now().Add(entry.registry.options.HeaderTimeout))
		if _, err := readHeadLines(reader); err == nil || errors.Is(err, errHeadTooLarge) {
			lingerClose(client, responseBusy)
		}
		return
	}
	defer entry.releaseSlot()
	session := &httpSession{entry: entry, client: client, reader: reader}
	defer session.closeUpstream()
	session.run()
}

func (session *httpSession) run() {
	options := session.entry.registry.options
	idle := options.FirstByteTimeout
	for {
		head, err := session.entry.readHead(session.client, session.reader, idle)
		if err != nil {
			if errors.Is(err, errHeadTooLarge) {
				lingerClose(session.client, responseHeaderTooLarge)
			}
			return
		}
		method, target, ok := parseRequestLine(head)
		if !ok {
			lingerClose(session.client, responseBadRequest)
			return
		}
		if method == http.MethodConnect {
			session.connect(head, target)
			return
		}
		if !session.forward(head, target) {
			return
		}
		if !session.waitNext() {
			return
		}
		idle = options.KeepAliveIdle
	}
}

func (session *httpSession) closeUpstream() {
	if session.upstream != nil {
		session.entry.closeUpstream(session.upstream)
	}
}

// fail 记下失败，按需要给浏览器回复后关闭
func (session *httpSession) fail(failure *relayFailure) {
	session.entry.report(failure)
	if failure.kind != "" && failure.response != "" {
		lingerClose(session.client, failure.response)
	}
}

// ensureUpstream 在第一个请求上建上游连接；返回是否新建
func (session *httpSession) ensureUpstream() (bool, *relayFailure) {
	if session.upstream != nil {
		return false, nil
	}
	upstream, failure := session.entry.dialUpstream()
	if failure != nil {
		return false, failure
	}
	session.upstream = upstream
	session.upReader = bufio.NewReaderSize(upstream.connection, 4096)
	return true, nil
}

// connect 处理 CONNECT：写入改写后的请求头，手工读响应头，只有严格等于 200 才建隧道
func (session *httpSession) connect(head []byte, target string) {
	entry := session.entry
	host, ok := validConnectTarget(target)
	if !ok {
		lingerClose(session.client, responseBadRequest)
		return
	}
	fresh, failure := session.ensureUpstream()
	if failure != nil {
		session.fail(failure)
		return
	}
	upstream := session.upstream
	deadline := upstream.deadline
	if !fresh {
		deadline = time.Now().Add(entry.registry.options.UpstreamTimeout)
	}
	_ = upstream.connection.SetDeadline(deadline)
	if _, err := upstream.connection.Write(rewriteHead(head, entry.upstream.authorization)); err != nil {
		session.fail(session.lostBeforeResponse(fresh, err))
		return
	}
	if _, err := session.upReader.Peek(1); err != nil {
		session.fail(session.lostBeforeResponse(fresh, err))
		return
	}
	code, reason, err := readResponseHead(session.upReader)
	if err != nil {
		if entry.registry.ctx.Err() != nil {
			return
		}
		if isTimeout(err) {
			session.fail(entry.failureTimeout())
			return
		}
		session.fail(entry.failureMalformed())
		return
	}
	switch {
	case code == http.StatusOK:
	case code == http.StatusProxyAuthRequired:
		// 不能把 407 原样转给浏览器：有界面的登录浏览器会弹认证框，无头 Worker 会卡在 BiDi authRequired
		session.fail(entry.failureAuth(text407, false))
		return
	case code < 200:
		// CONNECT 不应有 101 或其他 1xx 最终响应
		session.fail(entry.failureMalformed())
		return
	default:
		session.fail(entry.failureTargetHTTP(code, reason, host, target))
		return
	}
	_ = upstream.connection.SetDeadline(time.Time{})
	// 固定的 200 行之后、客户端开口之前不多发任何字节；上游在响应头之后已经发来的隧道字节紧跟着转发
	reply := []byte("HTTP/1.1 200 Connection established\r\n\r\n")
	if buffered := session.upReader.Buffered(); buffered > 0 {
		pending, _ := session.upReader.Peek(buffered)
		reply = append(reply, pending...)
	}
	if _, err := session.client.Write(reply); err != nil {
		return
	}
	// 客户端流水线发来的 ClientHello 开头
	if buffered := session.reader.Buffered(); buffered > 0 {
		pending, _ := session.reader.Peek(buffered)
		if _, err := upstream.connection.Write(pending); err != nil {
			return
		}
	}
	_ = session.client.SetDeadline(time.Time{})
	pipe(session.client, upstream.connection, entry.registry.options.HalfCloseGrace)
}

// lostBeforeResponse 处理收到上游任何响应字节之前的失败：上游刚建好时照常回 502/504；
// 长连接上的非首个请求只关闭两侧、不回 502，Firefox 在复用连接上遇到“没有任何响应就断开”会重试
func (session *httpSession) lostBeforeResponse(fresh bool, err error) *relayFailure {
	entry := session.entry
	if entry.registry.ctx.Err() != nil {
		return failureSilent()
	}
	if isTimeout(err) {
		return entry.failureTimeout()
	}
	if !fresh {
		return failureSilent()
	}
	return entry.failureMalformed()
}

// forward 转发一个 absolute-form 明文请求；返回 false 时这条连接结束
func (session *httpSession) forward(head []byte, target string) bool {
	entry := session.entry
	if !hasPrefixFold(target, "http://") {
		// origin-form 与 OPTIONS * 挡住本机网页与 DNS rebinding；https:// 绝对 URI 转发的话上游会替我们和目标握 TLS，指纹就变了
		entry.logThrottled("reject", "WARN", fmt.Sprintf("本机代理中转拒绝请求 | 本机=%s | 原因=不是 http:// 绝对地址的请求", entry.local))
		lingerClose(session.client, responseBadRequest)
		return false
	}
	// 只做解析：重建会按字母排序头部、调整 Content-Length 位置、自动补 Cache-Control，这些会经上游传给目标
	parsed, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		lingerClose(session.client, responseBadRequest)
		return false
	}
	fresh, failure := session.ensureUpstream()
	if failure != nil {
		session.fail(failure)
		return false
	}
	upstream := session.upstream
	_ = upstream.connection.SetDeadline(time.Time{})
	writer := &trackedWriter{writer: upstream.connection}
	if _, err := writer.Write(rewriteHead(head, entry.upstream.authorization)); err != nil {
		session.fail(session.lostBeforeResponse(fresh, err))
		return false
	}
	if err := copyRequestBody(writer, session.reader, parsed); err != nil {
		if writer.err != nil {
			session.fail(session.lostBeforeResponse(fresh, writer.err))
		}
		return false
	}
	return session.relayResponse(parsed, fresh)
}

// relayResponse 读上游响应并转给浏览器；这些字节只发给本机浏览器，允许标准库重写
func (session *httpSession) relayResponse(parsed *http.Request, fresh bool) bool {
	entry := session.entry
	upstream := session.upstream
	for {
		_ = upstream.connection.SetReadDeadline(time.Now().Add(plainResponseTimeout))
		if _, err := session.upReader.Peek(1); err != nil {
			if isTimeout(err) {
				return false
			}
			session.fail(session.lostBeforeResponse(fresh, err))
			return false
		}
		response, err := http.ReadResponse(session.upReader, parsed)
		if err != nil {
			if entry.registry.ctx.Err() == nil && !isTimeout(err) {
				session.fail(entry.failureMalformed())
			}
			return false
		}
		_ = upstream.connection.SetReadDeadline(time.Time{})
		switch {
		case response.StatusCode == http.StatusProxyAuthRequired:
			// 不调 Body.Close：标准库在不关连接时会把响应体读到 EOF，上游发得慢或停在半路时 502 就一直发不出去；
			// 上游连接马上由 closeUpstream 关闭
			session.fail(entry.failureAuth(text407, false))
			return false
		case response.StatusCode == http.StatusSwitchingProtocols:
			if writeInterim(session.client, response) != nil {
				return false
			}
			session.switchProtocols()
			return false
		case response.StatusCode >= 100 && response.StatusCode < 200:
			if writeInterim(session.client, response) != nil {
				return false
			}
			continue
		}
		response.Header.Del("Proxy-Authenticate")
		// 写浏览器失败时 Response.Write 会关闭响应体，Body.Close 同样会把上游剩余响应体读到 EOF；
		// 写失败的同时让上游读立即超时，连接不再被一条已经断开的浏览器连接占着。
		// 上游停在响应体半路、不再发字节时根本不会再写浏览器，浏览器是否断开由 watchClient 发现
		stopWatch := session.watchClient()
		err = response.Write(&abortReadWriter{writer: session.client, upstream: upstream.connection})
		if gone := stopWatch(); err != nil || gone {
			return false
		}
		_ = response.Body.Close()
		return !parsed.Close && !response.Close
	}
}

// watchClient 在转发明文响应体期间盯住浏览器（这期间没有别的代码读浏览器这一侧）：浏览器断开（EOF、RST）就把上游读期限
// 设为现在，上游停在响应体半路、不再发字节时也立即放手，不再长期占着 goroutine、fd、上游连接，监听器也能按活动关闭；
// 浏览器提前发来的下一个请求只留在缓冲区里，照常处理。返回的函数打断后台读、等它返回，并报告浏览器是否已断开
func (session *httpSession) watchClient() func() bool {
	upstream := session.upstream.connection
	var stopped atomic.Bool
	watched := make(chan bool, 1)
	go func() {
		_, err := session.reader.Peek(1)
		gone := err != nil && !(stopped.Load() && isTimeout(err))
		if gone {
			_ = upstream.SetReadDeadline(time.Now())
		}
		watched <- gone
	}()
	return func() bool {
		stopped.Store(true)
		// bufio.Reader 不保留超时错误，之后读下一个请求时 readHead 与 waitNext 会重新设读期限
		_ = session.client.SetReadDeadline(time.Now())
		return <-watched
	}
}

// abortReadWriter 写出错时把上游读期限设为现在，打断标准库关闭响应体时读到 EOF 的过程
type abortReadWriter struct {
	writer   io.Writer
	upstream net.Conn
}

func (writer *abortReadWriter) Write(data []byte) (int, error) {
	written, err := writer.writer.Write(data)
	if err != nil {
		_ = writer.upstream.SetReadDeadline(time.Now())
	}
	return written, err
}

// switchProtocols 在 101 之后转发两侧缓冲区里的字节并进入对拷
func (session *httpSession) switchProtocols() {
	upstream := session.upstream
	if buffered := session.upReader.Buffered(); buffered > 0 {
		pending, _ := session.upReader.Peek(buffered)
		if _, err := session.client.Write(pending); err != nil {
			return
		}
	}
	if buffered := session.reader.Buffered(); buffered > 0 {
		pending, _ := session.reader.Peek(buffered)
		if _, err := upstream.connection.Write(pending); err != nil {
			return
		}
	}
	_ = session.client.SetDeadline(time.Time{})
	_ = upstream.connection.SetDeadline(time.Time{})
	pipe(session.client, upstream.connection, session.entry.registry.options.HalfCloseGrace)
}

// writeInterim 写出 1xx 响应头（不经 Response.Write，免得给 1xx 补 Content-Length）
func writeInterim(client net.Conn, response *http.Response) error {
	var buffer bytes.Buffer
	fmt.Fprintf(&buffer, "HTTP/%d.%d %s\r\n", response.ProtoMajor, response.ProtoMinor, response.Status)
	if err := response.Header.Write(&buffer); err != nil {
		return err
	}
	buffer.WriteString("\r\n")
	_, err := client.Write(buffer.Bytes())
	return err
}

// waitNext 等下一个请求时同时盯住上游（与 net/http persistConn 的后台读同理）：上游先有动静（EOF、RST、
// 空闲期主动发来的字节，例如 408 后断开）就立即关闭两侧、不给浏览器写任何字节，Firefox 会像直连时一样重开连接重发；
// 浏览器先发来下一个请求就打断后台读，继续处理
func (session *httpSession) waitNext() bool {
	options := session.entry.registry.options
	upstream := session.upstream.connection
	_ = upstream.SetReadDeadline(time.Time{})
	var interrupted atomic.Bool
	watched := make(chan error, 1)
	go func() {
		_, err := session.upReader.Peek(1)
		if !(interrupted.Load() && isTimeout(err)) {
			// 上游先有动静：关闭两侧，丢弃上游发来的字节
			_ = session.client.Close()
			_ = upstream.Close()
		}
		watched <- err
	}()
	_ = session.client.SetReadDeadline(time.Now().Add(options.KeepAliveIdle))
	if _, err := session.reader.Peek(1); err != nil {
		interrupted.Store(true)
		_ = upstream.Close()
		<-watched
		return false
	}
	interrupted.Store(true)
	_ = upstream.SetReadDeadline(time.Now())
	err := <-watched
	if !isTimeout(err) {
		return false
	}
	// bufio.Reader 不保留超时错误；tls.Conn 的读超时也不会污染连接状态
	_ = upstream.SetReadDeadline(time.Time{})
	return true
}

// readHead 读一个请求头，保留原始字节（含原样的换行符）：先按 idle 等第一个字节，收到后按 HeaderTimeout 读完，
// 读完立刻清掉读期限，请求体与隧道都不受 HeaderTimeout 限制
func (entry *relayEntry) readHead(client net.Conn, reader *bufio.Reader, idle time.Duration) ([]byte, error) {
	_ = client.SetReadDeadline(time.Now().Add(idle))
	if _, err := reader.Peek(1); err != nil {
		return nil, err
	}
	_ = client.SetReadDeadline(time.Now().Add(entry.registry.options.HeaderTimeout))
	head, err := readHeadLines(reader)
	if err != nil {
		return nil, err
	}
	_ = client.SetReadDeadline(time.Time{})
	return head, nil
}

// readHeadLines 逐行读到空行，总长不超过 64 KiB、不超过 100 行
func readHeadLines(reader *bufio.Reader) ([]byte, error) {
	var head []byte
	lines := 0
	for {
		line, err := readLimitedLine(reader, maxHeadBytes-len(head))
		if err != nil {
			return nil, err
		}
		head = append(head, line...)
		if isBlankLine(line) {
			return head, nil
		}
		lines++
		if lines > maxHeadLines {
			return nil, errHeadTooLarge
		}
	}
}

// readLimitedLine 读一行（含换行符），超过 limit 时返回 errHeadTooLarge
func readLimitedLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(line)+len(chunk) > limit {
			return nil, errHeadTooLarge
		}
		line = append(line, chunk...)
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func isBlankLine(line []byte) bool {
	return len(line) == 1 && line[0] == '\n' || len(line) == 2 && line[0] == '\r' && line[1] == '\n'
}

// parseRequestLine 只接受恰好三段的 METHOD SP TARGET SP HTTP/1.0|HTTP/1.1
func parseRequestLine(head []byte) (string, string, bool) {
	end := bytes.IndexByte(head, '\n')
	if end < 0 {
		return "", "", false
	}
	line := strings.TrimSuffix(string(head[:end]), "\r")
	parts := strings.Split(line, " ")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	if parts[2] != "HTTP/1.1" && parts[2] != "HTTP/1.0" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// validConnectTarget 校验 CONNECT 目标：host 非空，不含 @ / ? # 与空白，端口 1-65535；主机名原样转发，不解析、不改大小写
func validConnectTarget(target string) (string, bool) {
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" || strings.ContainsAny(host, "@/?#") {
		return "", false
	}
	for _, character := range host {
		if character <= ' ' || character == 0x7f {
			return "", false
		}
	}
	if number, err := strconv.ParseUint(port, 10, 16); err != nil || number == 0 {
		return "", false
	}
	return host, true
}

// rewriteHead 删掉所有 Proxy-Authorization 行（名称不区分大小写，连同折行），在空行之前追加注入的一行；
// 其余行的顺序、大小写、内容一律不动
func rewriteHead(head []byte, authorization string) []byte {
	output := make([]byte, 0, len(head)+len(authorization)+24)
	rest := head
	first := true
	dropping := false
	for len(rest) > 0 {
		end := bytes.IndexByte(rest, '\n')
		if end < 0 {
			end = len(rest) - 1
		}
		line := rest[:end+1]
		rest = rest[end+1:]
		switch {
		case first:
			first = false
			output = append(output, line...)
		case isBlankLine(line):
			output = append(output, "Proxy-Authorization: "...)
			output = append(output, authorization...)
			output = append(output, "\r\n"...)
			output = append(output, line...)
		case line[0] == ' ' || line[0] == '\t':
			if !dropping {
				output = append(output, line...)
			}
		default:
			dropping = isProxyAuthorization(line)
			if !dropping {
				output = append(output, line...)
			}
		}
	}
	return output
}

func isProxyAuthorization(line []byte) bool {
	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		return false
	}
	return strings.EqualFold(string(bytes.TrimRight(line[:colon], " \t")), "Proxy-Authorization")
}

// readResponseHead 手工读 CONNECT 的响应头：不用 http.ReadResponse，也绝不读或关闭响应体（上游回 200 加
// Transfer-Encoding: chunked 时 ReadResponse 会吞掉隧道数据）；1xx（101 除外）跳过
func readResponseHead(reader *bufio.Reader) (int, string, error) {
	for {
		statusLine, err := readLimitedLine(reader, maxHeadBytes)
		if err != nil {
			return 0, "", err
		}
		code, reason, ok := parseStatusLine(statusLine)
		if !ok {
			return 0, "", errHeadMalformed
		}
		total := len(statusLine)
		lines := 0
		for {
			line, err := readLimitedLine(reader, maxHeadBytes-total)
			if err != nil {
				if errors.Is(err, errHeadTooLarge) {
					return 0, "", errHeadMalformed
				}
				return 0, "", err
			}
			total += len(line)
			if isBlankLine(line) {
				break
			}
			lines++
			if lines > maxHeadLines {
				return 0, "", errHeadMalformed
			}
		}
		if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
			continue
		}
		return code, reason, nil
	}
}

// parseStatusLine 解析 HTTP/1.x SSS reason
func parseStatusLine(line []byte) (int, string, bool) {
	text := strings.TrimRight(string(line), "\r\n")
	if len(text) < 12 || !strings.HasPrefix(text, "HTTP/1.") || text[7] < '0' || text[7] > '9' || text[8] != ' ' {
		return 0, "", false
	}
	code := 0
	for _, digit := range text[9:12] {
		if digit < '0' || digit > '9' {
			return 0, "", false
		}
		code = code*10 + int(digit-'0')
	}
	if code < 100 {
		return 0, "", false
	}
	reason := ""
	if len(text) > 12 {
		if text[12] != ' ' {
			return 0, "", false
		}
		reason = text[13:]
	}
	return code, reason, true
}

// copyRequestBody 按原始字节拷贝请求体：chunked 逐块原样拷贝、不重新分块；有 Content-Length 就拷贝这么多；都没有就视为没有请求体
func copyRequestBody(writer io.Writer, reader *bufio.Reader, parsed *http.Request) error {
	if len(parsed.TransferEncoding) > 0 && strings.EqualFold(parsed.TransferEncoding[0], "chunked") {
		return copyChunked(writer, reader)
	}
	if parsed.ContentLength > 0 {
		_, err := io.CopyN(writer, reader, parsed.ContentLength)
		return err
	}
	return nil
}

// copyChunked 逐块拷贝原始的块大小行、数据和 CRLF，直到 0 块，再拷贝 trailer 到空行
func copyChunked(writer io.Writer, reader *bufio.Reader) error {
	for {
		line, err := readLimitedLine(reader, 4096)
		if err != nil {
			return err
		}
		size, err := parseChunkSize(line)
		if err != nil {
			return err
		}
		if _, err := writer.Write(line); err != nil {
			return err
		}
		if size == 0 {
			break
		}
		if _, err := io.CopyN(writer, reader, size); err != nil {
			return err
		}
		ending, err := readLimitedLine(reader, 2)
		if err != nil {
			return err
		}
		if !isBlankLine(ending) {
			return errHeadMalformed
		}
		if _, err := writer.Write(ending); err != nil {
			return err
		}
	}
	total := 0
	for {
		line, err := readLimitedLine(reader, maxHeadBytes-total)
		if err != nil {
			return err
		}
		total += len(line)
		if _, err := writer.Write(line); err != nil {
			return err
		}
		if isBlankLine(line) {
			return nil
		}
	}
}

// parseChunkSize 解析块大小行（可带扩展）
func parseChunkSize(line []byte) (int64, error) {
	text := strings.TrimRight(string(line), "\r\n")
	if index := strings.IndexByte(text, ';'); index >= 0 {
		text = text[:index]
	}
	text = strings.TrimRight(text, " \t")
	if text == "" || len(text) > 16 {
		return 0, errHeadMalformed
	}
	size, err := strconv.ParseInt(text, 16, 64)
	if err != nil || size < 0 {
		return 0, errHeadMalformed
	}
	return size, nil
}

// trackedWriter 记下写上游时的错误，用来区分“读浏览器失败”与“写上游失败”
type trackedWriter struct {
	writer io.Writer
	err    error
}

func (writer *trackedWriter) Write(data []byte) (int, error) {
	written, err := writer.writer.Write(data)
	if err != nil && writer.err == nil {
		writer.err = err
	}
	return written, err
}

// lingerClose 写完失败回复后半关闭，再最多读 1 秒或 4 KiB 把对端剩余数据读掉，然后由调用方关闭
func lingerClose(client net.Conn, response string) {
	_ = client.SetWriteDeadline(time.Now().Add(lingerTimeout))
	if response != "" {
		if _, err := io.WriteString(client, response); err != nil {
			return
		}
	}
	if closeWrite(client) != nil {
		return
	}
	_ = client.SetReadDeadline(time.Now().Add(lingerTimeout))
	_, _ = io.CopyN(io.Discard, client, lingerBytes)
}

func hasPrefixFold(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}
