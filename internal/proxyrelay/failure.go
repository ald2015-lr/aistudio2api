package proxyrelay

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// failureKind 为失败类别：上游层面的失败总是相关，按目标的失败（target）要调用方认为目标相关才附加
type failureKind string

const (
	failureKindAuth        failureKind = "auth"
	failureKindUnreachable failureKind = "unreachable"
	failureKindTimeout     failureKind = "timeout"
	failureKindTLS         failureKind = "tls"
	failureKindMalformed   failureKind = "malformed"
	failureKindBusy        failureKind = "busy"
	failureKindTarget      failureKind = "target"
)

const (
	// failureRingSize 为每个监听器保留的失败记录条数
	failureRingSize = 8
	// logThrottleWindow 为日志节流窗口：同一个键窗口内只记第一条
	logThrottleWindow = 60 * time.Second
	// targetReasonLimit 为转发给浏览器的上游原因短语上限
	targetReasonLimit = 64
)

const (
	text407              = "上游代理拒绝了账号密码（407），请检查用户名和密码"
	textSOCKSAuthDenied  = "上游 SOCKS5 代理拒绝了账号密码，请检查用户名和密码"
	textSOCKSAuthMethod  = "上游 SOCKS5 代理不接受账号密码认证"
	textMalformed        = "上游代理的响应无法解析"
	textBusy             = "本机代理中转连接数已满"
	textSOCKSTargetLabel = "上游 SOCKS5 代理连接目标失败："
)

// failureRecord 为一条失败记录
type failureRecord struct {
	at     time.Time
	kind   failureKind
	target string // 按目标的失败才有：CONNECT 或 SOCKS 请求里的 host（不含端口）
	text   string // 已脱敏的中文说明
}

// failureRing 保留最近 failureRingSize 条失败记录
type failureRing struct {
	records [failureRingSize]failureRecord
	next    int
	count   int
}

func (ring *failureRing) add(record failureRecord) {
	ring.records[ring.next] = record
	ring.next = (ring.next + 1) % failureRingSize
	if ring.count < failureRingSize {
		ring.count++
	}
}

// relayFailure 为一次失败：kind 为空表示安静关闭（不回复、不记录、不记日志）
type relayFailure struct {
	kind          failureKind
	text          string // 失败记录里的说明
	reason        string // 日志“原因=”字段
	response      string // HTTP 中转回给浏览器的完整响应头
	hint          string // 认证失败日志的结尾提示
	target        string // 按目标的失败：目标主机（不含端口）
	targetAddress string // 按目标的失败：目标 host:port，只用于日志
	targetStatus  string // 按目标的失败：403 Forbidden 或 SOCKS 05 拒绝连接
	scope         string // 连接数已满：单个浏览器 / 整个进程
	limit         int    // 连接数已满：上限
}

// failureSilent 为不需要回复的失败（注册表关闭、长连接复用时上游先断开等）
func failureSilent() *relayFailure {
	return &relayFailure{}
}

func errorResponse(status string) string {
	return "HTTP/1.1 " + status + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
}

var (
	responseBadRequest     = errorResponse("400 Bad Request")
	responseHeaderTooLarge = errorResponse("431 Request Header Fields Too Large")
	responseAuthFailed     = errorResponse("502 Upstream Proxy Authentication Failed")
	responseUnreachable    = errorResponse("502 Upstream Proxy Unreachable")
	responseTimeout        = errorResponse("504 Upstream Proxy Timeout")
	responseTLSFailed      = errorResponse("502 Upstream Proxy TLS Failed")
	responseMalformed      = errorResponse("502 Bad Upstream Response")
	responseBusy           = errorResponse("503 Relay Busy")
)

func (entry *relayEntry) failureAuth(text string, socks bool) *relayFailure {
	hint := "已向浏览器返回 502，请检查代理用户名和密码"
	if socks {
		hint = "已向浏览器返回 SOCKS 失败，请检查代理用户名和密码"
		if text == textSOCKSAuthMethod {
			hint = "已向浏览器返回 SOCKS 失败，上游不接受账号密码认证"
		}
	}
	return &relayFailure{kind: failureKindAuth, text: text, response: responseAuthFailed, hint: hint}
}

func (entry *relayEntry) failureUnreachable(reason string) *relayFailure {
	return &relayFailure{
		kind:     failureKindUnreachable,
		text:     fmt.Sprintf("连不上上游代理 %s（%s）", entry.upstream.address, reason),
		reason:   reason,
		response: responseUnreachable,
	}
}

func (entry *relayEntry) failureTimeout() *relayFailure {
	return &relayFailure{
		kind:     failureKindTimeout,
		text:     fmt.Sprintf("连接上游代理 %s 超时", entry.upstream.address),
		reason:   "超时",
		response: responseTimeout,
	}
}

func (entry *relayEntry) failureTLS() *relayFailure {
	return &relayFailure{
		kind:     failureKindTLS,
		text:     fmt.Sprintf("与上游代理 %s 的 TLS 握手失败", entry.upstream.address),
		reason:   "TLS 握手失败",
		response: responseTLSFailed,
	}
}

func (entry *relayEntry) failureMalformed() *relayFailure {
	return &relayFailure{kind: failureKindMalformed, text: textMalformed, reason: "响应无法解析", response: responseMalformed}
}

func (entry *relayEntry) failureBusy(scope string, limit int) *relayFailure {
	return &relayFailure{kind: failureKindBusy, text: textBusy, response: responseBusy, scope: scope, limit: limit}
}

// failureTargetHTTP 为上游对 CONNECT 回非 200：状态码原样转给浏览器，原因短语只保留可打印 ASCII，不转发响应体
func (entry *relayEntry) failureTargetHTTP(code int, reason, host, address string) *relayFailure {
	reason = scrubbedASCII(reason, targetReasonLimit, entry.upstream.raw)
	if reason == "" {
		reason = http.StatusText(code)
	}
	status := fmt.Sprintf("%03d %s", code, reason)
	return &relayFailure{
		kind:          failureKindTarget,
		text:          fmt.Sprintf("上游代理拒绝了隧道：%s（目标 %s）", strings.TrimSpace(status), entry.displayTarget(host)),
		response:      errorResponse(status),
		target:        host,
		targetAddress: address,
		targetStatus:  strings.TrimSpace(status),
	}
}

// failureTargetSOCKS 为上游 SOCKS5 回 REP≠0：上游的 REP 码只写进说明，不转给浏览器
func (entry *relayEntry) failureTargetSOCKS(reply byte, host, address string) *relayFailure {
	status := socksReplyText(reply)
	return &relayFailure{
		kind:          failureKindTarget,
		text:          fmt.Sprintf("%s%s（目标 %s）", textSOCKSTargetLabel, status, entry.displayTarget(host)),
		target:        host,
		targetAddress: address,
		targetStatus:  "SOCKS " + status,
	}
}

// report 记下失败并按节流写日志；安静失败什么都不做
func (entry *relayEntry) report(failure *relayFailure) {
	if failure == nil || failure.kind == "" {
		return
	}
	entry.mu.Lock()
	entry.failures.add(failureRecord{at: entry.registry.now(), kind: failure.kind, target: failure.target, text: failure.text})
	entry.mu.Unlock()
	upstream := entry.upstream.display
	switch failure.kind {
	case failureKindAuth:
		entry.logThrottled("auth", "WARN", fmt.Sprintf("上游代理认证失败 | 上游=%s | 本机=%s | 使用者=%s | %s",
			upstream, entry.local, entry.owner, failure.hint))
	case failureKindTarget:
		entry.logThrottled("target", "WARN", fmt.Sprintf("上游代理拒绝隧道 | 上游=%s | 本机=%s | 状态=%s | 目标=%s | 使用者=%s",
			upstream, entry.local, failure.targetStatus, entry.displayTarget(failure.targetAddress), entry.owner))
	case failureKindBusy:
		entry.logThrottled("busy", "WARN", fmt.Sprintf("本机代理中转连接数已满 | 本机=%s | 范围=%s | 上限=%d",
			entry.local, failure.scope, failure.limit))
	default:
		entry.logThrottled("connect", "WARN", fmt.Sprintf("上游代理连接失败 | 上游=%s | 本机=%s | 原因=%s | 使用者=%s",
			upstream, entry.local, Scrub(failure.reason, entry.upstream.raw), entry.owner))
	}
}

// failureSince 返回 since 之后最近一次相关失败的说明
func (entry *relayEntry) failureSince(since time.Time, relevant func(host string) bool) string {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	ring := &entry.failures
	for offset := 0; offset < ring.count; offset++ {
		record := ring.records[(ring.next-1-offset+failureRingSize)%failureRingSize]
		if record.at.Before(since) {
			// 记录按时间先后写入，更早的都在 since 之前
			return ""
		}
		if record.kind == failureKindTarget && (relevant == nil || !relevant(record.target)) {
			continue
		}
		return record.text
	}
	return ""
}

// logThrottle 为日志节流：同一个键 60 秒内只记第一条，其余计数；窗口结束后再出现时附上计数
type logThrottle struct {
	mu      sync.Mutex
	windows map[string]*throttleWindow
}

type throttleWindow struct {
	start      time.Time
	suppressed int
}

// allow 判断这一条是否要记；要记时返回上一个窗口里被压下的条数
func (throttle *logThrottle) allow(key string, now time.Time) (bool, int) {
	throttle.mu.Lock()
	defer throttle.mu.Unlock()
	if throttle.windows == nil {
		throttle.windows = make(map[string]*throttleWindow)
	}
	window, ok := throttle.windows[key]
	if ok && now.Sub(window.start) < logThrottleWindow {
		window.suppressed++
		return false, 0
	}
	suppressed := 0
	if ok {
		suppressed = window.suppressed
	}
	throttle.windows[key] = &throttleWindow{start: now}
	return true, suppressed
}

func withSuppressed(message string, suppressed int) string {
	if suppressed <= 0 {
		return message
	}
	return fmt.Sprintf("%s | 60 秒内另有 %d 次", message, suppressed)
}

// logThrottled 按（监听器，文案名）节流写日志
func (entry *relayEntry) logThrottled(key, level, message string) {
	emit, suppressed := entry.throttle.allow(key, entry.registry.now())
	if !emit {
		return
	}
	entry.registry.logf(level, withSuppressed(message, suppressed))
}

// socksReplyMeanings 为 SOCKS5 回复码的中文含义
var socksReplyMeanings = map[byte]string{
	0x01: "一般性失败",
	0x02: "规则不允许连接",
	0x03: "网络不可达",
	0x04: "主机不可达",
	0x05: "拒绝连接",
	0x06: "TTL 已过期",
	0x07: "不支持的命令",
	0x08: "不支持的地址类型",
}

// socksReplyText 为 SOCKS5 回复码的中文含义，形如“05 拒绝连接”
func socksReplyText(reply byte) string {
	meaning, ok := socksReplyMeanings[reply]
	if !ok {
		meaning = "未知错误"
	}
	return fmt.Sprintf("%02X %s", reply, meaning)
}

// xnetSOCKSReplies 为 x/net 的 Reply.String() 到回复码的对照（x/net 的回复码错误固定是 "unknown error " + Reply.String()）
var xnetSOCKSReplies = map[string]byte{
	"general SOCKS server failure":      0x01,
	"connection not allowed by ruleset": 0x02,
	"network unreachable":               0x03,
	"host unreachable":                  0x04,
	"connection refused":                0x05,
	"TTL expired":                       0x06,
	"command not supported":             0x07,
	"address type not supported":        0x08,
}

// Describe 把 Go 侧代理错误转成简短中文说明（407、SOCKS5 认证失败、拒绝连接、超时、TLS、无法解析），其余返回脱敏后的原文。
// x/net 的 SOCKS 回复码错误只按 "unknown error " 前缀识别；拨号阶段的 net.OpError 内层错误按“连不上上游代理”处理（3.8）
func Describe(err error, upstream string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	address, scheme := describeUpstream(upstream)
	switch {
	case strings.Contains(message, "username/password authentication failed"):
		return textSOCKSAuthDenied
	case strings.Contains(message, "no acceptable authentication methods"):
		return textSOCKSAuthMethod
	}
	if index := strings.Index(message, "unknown error "); index >= 0 {
		reply := strings.TrimSpace(message[index+len("unknown error "):])
		if code, ok := xnetSOCKSReplies[reply]; ok {
			return textSOCKSTargetLabel + socksReplyText(code)
		}
		var number int
		if _, scanErr := fmt.Sscanf(reply, "unknown code: %d", &number); scanErr == nil && number >= 0 && number <= 255 {
			return textSOCKSTargetLabel + socksReplyText(byte(number))
		}
		return textSOCKSTargetLabel + Scrub(reply, upstream)
	}
	if reason, timeout, ok := dialStageFailure(err, message); ok {
		if timeout {
			return timeoutText(address)
		}
		return unreachableText(address, reason)
	}
	lower := strings.ToLower(message)
	switch {
	case statusCode407.MatchString(message):
		return text407
	case isTimeout(err) || strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		return timeoutText(address)
	case strings.Contains(lower, "x509") || strings.Contains(lower, "tls:") || strings.Contains(lower, "certificate"):
		if scheme == "https" {
			return fmt.Sprintf("与上游代理 %s 的 TLS 握手失败", address)
		}
		// 上游不是 https 时 TLS 错误只可能来自经隧道的目标站点
		return "TLS 握手失败"
	}
	return Scrub(message, upstream)
}

// statusCode407 匹配错误串里独立出现的 407 状态码（不匹配端口 :407 与 4070 之类的数字）
var statusCode407 = regexp.MustCompile(`(^|[^0-9.:])407([^0-9]|$)`)

// dialStageFailure 识别拨号阶段（连上游代理本身）的失败：*net.OpError 的 Op 为 dial，
// 或者外层 *net.OpError（如 x/net 的 socks connect）内层是拨号错误或 *net.DNSError；类型丢失时按 "dial tcp" 字样兜底
func dialStageFailure(err error, message string) (string, bool, bool) {
	current := err
	for current != nil {
		var opError *net.OpError
		if !errors.As(current, &opError) {
			break
		}
		if opError.Op == "dial" {
			return dialReason(opError), isTimeout(opError), true
		}
		var dnsError *net.DNSError
		if errors.As(opError.Err, &dnsError) {
			return "主机名无法解析", false, true
		}
		current = opError.Err
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return "主机名无法解析", false, true
	}
	lower := strings.ToLower(message)
	index := strings.Index(lower, "dial tcp")
	if index < 0 {
		return "", false, false
	}
	detail := lower[index:]
	switch {
	case strings.Contains(detail, "no such host"):
		return "主机名无法解析", false, true
	case strings.Contains(detail, "timeout") || strings.Contains(detail, "deadline exceeded"):
		return "", true, true
	case strings.Contains(detail, "refused"):
		return "连接被拒绝", false, true
	case strings.Contains(detail, "unreachable") || strings.Contains(detail, "no route to host"):
		return "网络不可达", false, true
	default:
		return "连接失败", false, true
	}
}

func timeoutText(address string) string {
	if address == "" {
		return "连接上游代理超时"
	}
	return fmt.Sprintf("连接上游代理 %s 超时", address)
}

func unreachableText(address, reason string) string {
	if address == "" {
		return fmt.Sprintf("连不上上游代理（%s）", reason)
	}
	return fmt.Sprintf("连不上上游代理 %s（%s）", address, reason)
}

// describeUpstream 取上游的 host:port（补默认端口）与协议，只用于说明文字
func describeUpstream(upstream string) (string, string) {
	parsed, err := url.Parse(strings.TrimSpace(upstream))
	if err != nil || parsed.Hostname() == "" {
		return "", ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	port := parsed.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return parsed.Hostname(), scheme
		}
	}
	return net.JoinHostPort(parsed.Hostname(), port), scheme
}

func isConnectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(strings.ToLower(err.Error()), "refused")
}

func isNetworkUnreachable(err error) bool {
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "unreachable") || strings.Contains(lower, "no route to host")
}

func isConnectionReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || strings.Contains(strings.ToLower(err.Error()), "reset")
}

// Scrub 从文本里去掉上游的用户名、密码（原文、百分号编码与 Basic base64 三种形式，长度不少于 4 时替换为 ***）
func Scrub(text string, upstream string) string {
	if text == "" {
		return text
	}
	user, password, encodedUser, encodedPassword, ok := upstreamSecrets(upstream)
	if !ok {
		return text
	}
	candidates := map[string]struct{}{}
	for _, secret := range []string{user, password} {
		for _, form := range []string{secret, url.PathEscape(secret), url.QueryEscape(secret), escapeStrict(secret)} {
			candidates[form] = struct{}{}
		}
	}
	candidates[encodedUser] = struct{}{}
	candidates[encodedPassword] = struct{}{}
	credentials := []byte(user + ":" + password)
	candidates[base64.StdEncoding.EncodeToString(credentials)] = struct{}{}
	candidates[base64.RawStdEncoding.EncodeToString(credentials)] = struct{}{}
	forms := make([]string, 0, len(candidates))
	for form := range candidates {
		if len(form) >= 4 {
			forms = append(forms, form)
		}
	}
	// 先替换长的，避免短的先替换后留下长形式的残片
	sort.Slice(forms, func(left, right int) bool {
		if len(forms[left]) != len(forms[right]) {
			return len(forms[left]) > len(forms[right])
		}
		return forms[left] < forms[right]
	})
	for _, form := range forms {
		text = strings.ReplaceAll(text, form, "***")
	}
	return text
}

// upstreamSecrets 取上游里的用户名与密码：解码后的值，以及原文里出现的写法
func upstreamSecrets(upstream string) (string, string, string, string, bool) {
	value := strings.TrimSpace(upstream)
	if value == "" {
		return "", "", "", "", false
	}
	rest := value
	if index := strings.Index(value, "://"); index >= 0 {
		rest = value[index+3:]
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		// host:port:用户名:密码 写法
		parts := strings.SplitN(rest, ":", 4)
		if len(parts) == 4 && !strings.Contains(value, "://") {
			return parts[2], parts[3], parts[2], parts[3], true
		}
		return "", "", "", "", false
	}
	encodedUser, encodedPassword, _ := strings.Cut(rest[:at], ":")
	user, password := encodedUser, encodedPassword
	if decoded, err := url.PathUnescape(encodedUser); err == nil {
		user = decoded
	}
	if decoded, err := url.PathUnescape(encodedPassword); err == nil {
		password = decoded
	}
	return user, password, encodedUser, encodedPassword, true
}

// escapeStrict 与规范化写法一致：只保留字母、数字与 - . _ ~
func escapeStrict(value string) string {
	const hex = "0123456789ABCDEF"
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if 'a' <= character && character <= 'z' || 'A' <= character && character <= 'Z' || '0' <= character && character <= '9' ||
			character == '-' || character == '.' || character == '_' || character == '~' {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hex[character>>4])
		builder.WriteByte(hex[character&15])
	}
	return builder.String()
}

// printableASCII 只保留可打印 ASCII，最多 limit 个字符
func printableASCII(value string, limit int) string {
	var builder strings.Builder
	for index := 0; index < len(value) && builder.Len() < limit; index++ {
		if character := value[index]; character >= 0x20 && character < 0x7f {
			builder.WriteByte(character)
		}
	}
	return strings.TrimSpace(builder.String())
}

// scrubbedASCII 先脱敏、再过滤与截断：截断后只剩凭据的一段时 Scrub 认不出来，非 ASCII 凭据过滤后也只剩残片；
// 过滤会去掉夹在凭据中间的不可打印字符、把两段拼起来，所以过滤后再脱敏一次
func scrubbedASCII(value string, limit int, upstream string) string {
	value = Scrub(value, upstream)
	value = Scrub(printableASCII(value, len(value)), upstream)
	return printableASCII(value, limit)
}

// displayTarget 为写进日志与说明的目标：先脱敏再截断并替换不可打印字符（替换成 ?，不会把两段拼起来）
func (entry *relayEntry) displayTarget(value string) string {
	return displayText(Scrub(value, entry.upstream.raw))
}

// displayText 把目标主机写进日志与说明前去掉控制字符等不可打印字符
func displayText(value string) string {
	if len(value) > 300 {
		value = value[:300]
	}
	var builder strings.Builder
	for len(value) > 0 {
		character, size := utf8.DecodeRuneInString(value)
		value = value[size:]
		if character == utf8.RuneError && size <= 1 || !unicode.IsPrint(character) {
			builder.WriteByte('?')
			continue
		}
		builder.WriteRune(character)
	}
	return builder.String()
}
