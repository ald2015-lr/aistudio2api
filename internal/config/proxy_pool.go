package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	ProxyPoolOff     = "off"
	ProxyPoolStatic  = "static"
	ProxyPoolDynamic = "dynamic"

	DefaultProxyPoolPortStart = 10001
	DefaultProxyPoolPortEnd   = 10200
	// MaxProxyPoolPorts 为端口段最多的端口数：每个端口预先算好 URL，选端口是线性扫描，5000 时每次分配几十微秒
	MaxProxyPoolPorts = 5000

	// ProxyPoolSessionPlaceholder 为动态 IP 地址里会话号的占位符（输入不区分大小写，也认 %7Bsession%7D，保存为小写）
	ProxyPoolSessionPlaceholder = "{session}"
)

// proxyPoolSessionToken 为规范化时临时替换 {session} 的令牌：全是小写字母，escapeUserinfo 不会编码它，换回占位后结果幂等。
// 输入本身含令牌时按格式错误处理，避免把用户写的文字误换成占位
const proxyPoolSessionToken = "aistudio2apisessiontoken"

// proxyPoolPlaceholderPattern 匹配会话号占位的各种写法（不区分大小写）：{session} 与百分号编码的 %7Bsession%7D
var proxyPoolPlaceholderPattern = regexp.MustCompile(`(?i)\{session\}|%7Bsession%7D`)

// IP 池设置的错误文案：全部是固定文本，不含输入内容（地址里可能有账号密码）
var (
	errProxyPoolMode         = errors.New("PROXY_POOL_MODE 必须是 off、static 或 dynamic")
	errProxyPoolPortRange    = errors.New("PROXY_POOL_PORT_START 与 PROXY_POOL_PORT_END 必须是 1 到 65535 的端口，且起始端口不大于结束端口")
	errProxyPoolPortCount    = fmt.Errorf("PROXY_POOL_PORT_START 到 PROXY_POOL_PORT_END 最多 %d 个端口", MaxProxyPoolPorts)
	errProxyPoolStaticEmpty  = errors.New("static 模式的 PROXY_POOL_URL 不能为空：填代理商给的任意一个端口的地址，例如 http://用户名:密码@isp.example.com:10000，端口按 PROXY_POOL_PORT_START 到 PROXY_POOL_PORT_END 自动替换")
	errProxyPoolStaticToken  = errors.New("static 模式的 PROXY_POOL_URL 不能包含 {session}；端口按 PROXY_POOL_PORT_START 到 PROXY_POOL_PORT_END 自动替换")
	errProxyPoolDynamicEmpty = errors.New("dynamic 模式的 PROXY_POOL_URL 不能为空：写成 http://customer-账号-sessid-{session}:密码@gate.example.com:端口 这样的地址，程序按账户替换 {session}")
	errProxyPoolDynamicToken = errors.New("dynamic 模式的 PROXY_POOL_URL 必须包含 {session}：写在用户名里会话号的位置，例如 customer-账号-sessid-{session}，程序按账户替换成固定会话号")
	errProxyPoolTokenHost    = errors.New("PROXY_POOL_URL 的 {session} 只能写在用户名或密码里")
)

// ProxyPool 为 IP 池设置（PROXY_POOL_*）：static 时每个浏览器独占端口段里的一个端口（代理商一个端口一个固定 IP），
// dynamic 时每个账户一个固定会话号（地址里的 {session}）。零值等同关闭、端口取默认值；保存后立即生效
type ProxyPool struct {
	// Mode 为 off、static 或 dynamic（PROXY_POOL_MODE，不区分大小写，空值按 off）
	Mode string `json:"mode"`
	// URL 为代理地址（PROXY_POOL_URL）：static 时端口部分会被替换，dynamic 时用户名或密码里写 {session}。
	// 支持 NormalizeProxy 的全部写法，保存时统一成 URL 形式；可能带账号密码，不能写进日志、错误与状态接口
	URL string `json:"url"`
	// PortStart、PortEnd 为 static 的端口段（PROXY_POOL_PORT_START / PROXY_POOL_PORT_END，含两端，默认 10001-10200）
	PortStart int `json:"port_start"`
	PortEnd   int `json:"port_end"`
}

// DefaultProxyPool 返回默认设置：关闭，端口段 10001-10200
func DefaultProxyPool() ProxyPool {
	return ProxyPool{Mode: ProxyPoolOff, PortStart: DefaultProxyPoolPortStart, PortEnd: DefaultProxyPoolPortEnd}
}

// withDefaults 补上零值语义：模式去空白转小写、空值按 off，端口为 0 时取默认值。没有写 ProxyPool 的 config.Config{} 字面量
// 因此等同默认设置
func (p ProxyPool) withDefaults() ProxyPool {
	p.Mode = strings.ToLower(strings.TrimSpace(p.Mode))
	if p.Mode == "" {
		p.Mode = ProxyPoolOff
	}
	p.URL = strings.TrimSpace(p.URL)
	if p.PortStart == 0 {
		p.PortStart = DefaultProxyPoolPortStart
	}
	if p.PortEnd == 0 {
		p.PortEnd = DefaultProxyPoolPortEnd
	}
	return p
}

// Enabled 判断 IP 池是否开启（static 或 dynamic）
func (p ProxyPool) Enabled() bool {
	mode := p.withDefaults().Mode
	return mode == ProxyPoolStatic || mode == ProxyPoolDynamic
}

// Equal 判断两份设置是否一致（按零值语义补齐后比较；地址按规范值比较，写法不同但上游相同视为一致）
func (p ProxyPool) Equal(other ProxyPool) bool {
	left, right := p.withDefaults(), other.withDefaults()
	return left.Mode == right.Mode && CanonicalProxyPoolURL(left.URL) == CanonicalProxyPoolURL(right.URL) &&
		left.PortStart == right.PortStart && left.PortEnd == right.PortEnd
}

// Validate 校验设置：端口段在任何模式下都校验；static 要求地址非空且不含 {session}；dynamic 要求地址非空、含 {session}
// 且只写在用户名或密码里；off 只要求地址为空或能规范化（关掉时保留地址，方便再打开）。错误文案不含地址内容
func (p ProxyPool) Validate() error {
	p = p.withDefaults()
	switch p.Mode {
	case ProxyPoolOff, ProxyPoolStatic, ProxyPoolDynamic:
	default:
		return errProxyPoolMode
	}
	if p.PortStart < 1 || p.PortEnd > 65535 || p.PortStart > p.PortEnd {
		return errProxyPoolPortRange
	}
	if p.PortEnd-p.PortStart+1 > MaxProxyPoolPorts {
		return errProxyPoolPortCount
	}
	if p.URL == "" {
		switch p.Mode {
		case ProxyPoolStatic:
			return errProxyPoolStaticEmpty
		case ProxyPoolDynamic:
			return errProxyPoolDynamicEmpty
		}
		return nil
	}
	normalized, err := NormalizeProxyPoolURL(p.URL)
	if err != nil {
		return err
	}
	hasPlaceholder := strings.Contains(normalized, ProxyPoolSessionPlaceholder)
	switch p.Mode {
	case ProxyPoolStatic:
		if hasPlaceholder {
			return errProxyPoolStaticToken
		}
	case ProxyPoolDynamic:
		if !hasPlaceholder {
			return errProxyPoolDynamicToken
		}
	}
	if hasPlaceholder {
		parsed, err := url.Parse(withProxyPoolToken(normalized))
		if err != nil {
			return proxyPoolError(errProxyFormat)
		}
		if strings.Contains(strings.ToLower(parsed.Host), proxyPoolSessionToken) {
			return errProxyPoolTokenHost
		}
	}
	return nil
}

// Size 返回 static 的端口数；其他模式为 0
func (p ProxyPool) Size() int {
	p = p.withDefaults()
	if p.Mode != ProxyPoolStatic || p.PortStart > p.PortEnd {
		return 0
	}
	return p.PortEnd - p.PortStart + 1
}

// Ports 返回端口段（按零值语义补齐，含两端）
func (p ProxyPool) Ports() (start, end int) {
	p = p.withDefaults()
	return p.PortStart, p.PortEnd
}

// StaticURL 返回端口 port 的上游 URL（规范值：NormalizeProxy 处理后逐字节不变，同一端口每次都是同一个字符串）；
// p 必须已通过 Validate，地址无法解析时返回空串
func (p ProxyPool) StaticURL(port int) string {
	parts, ok := p.urlParts()
	if !ok {
		return ""
	}
	return parts.withPort(port)
}

// StaticURLs 返回端口段内全部端口的上游 URL：下标 i 为端口 PortStart+i（与逐个调用 StaticURL 的结果相同，只规范化一次）；
// 不是 static 或地址无法解析时返回 nil
func (p ProxyPool) StaticURLs() []string {
	size := p.Size()
	if size == 0 {
		return nil
	}
	parts, ok := p.urlParts()
	if !ok {
		return nil
	}
	start, _ := p.Ports()
	urls := make([]string, size)
	for index := range urls {
		urls[index] = parts.withPort(start + index)
	}
	return urls
}

// SessionURL 把地址里的 {session} 换成 session（只能是小写字母与数字，否则返回空串）；p 必须已通过 Validate
func (p ProxyPool) SessionURL(session string) string {
	if session == "" || !proxyPoolSessionValid(session) {
		return ""
	}
	normalized, err := NormalizeProxyPoolURL(p.withDefaults().URL)
	if err != nil || !strings.Contains(normalized, ProxyPoolSessionPlaceholder) {
		return ""
	}
	return strings.ReplaceAll(normalized, ProxyPoolSessionPlaceholder, session)
}

// SessionInPassword 判断 {session} 写在密码里：429 治理的出口键去掉密码、只留用户名，会话号在密码里时各会话会合成一个出口，
// 需要按会话号另外区分（见 aistudio.ProxyBinding.EgressTag）
func (p ProxyPool) SessionInPassword() bool {
	normalized, err := NormalizeProxyPoolURL(p.withDefaults().URL)
	if err != nil {
		return false
	}
	_, password, _ := splitProxyPoolUserinfo(normalized)
	return strings.Contains(password, ProxyPoolSessionPlaceholder)
}

// Endpoint 返回脱敏的 scheme://host（不含端口、账号密码与会话号），供日志与状态接口；地址为空或无法解析时返回空串
func (p ProxyPool) Endpoint() string {
	parts, ok := p.urlParts()
	if !ok {
		return ""
	}
	return parts.endpoint()
}

// EndpointKey 返回 sha256(scheme://host + "\n" + 用户名) 的前 16 个十六进制字符：持久化记录据此判断是否换了代理商或子账号，
// 跨进程端口锁的文件名也用它（不含任何明文）。动态模式的用户名含 {session} 模板，只用于区分模板；地址为空或无法解析时返回空串
func (p ProxyPool) EndpointKey() string {
	parts, ok := p.urlParts()
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(parts.endpoint() + "\n" + parts.user))
	return hex.EncodeToString(sum[:])[:16]
}

// Summary 返回热更新日志里的一段（脱敏）：关闭 / 静态 http://isp.example.com 端口 10001-10200（200 个）/ 动态 http://gate.example.com
func (p ProxyPool) Summary() string {
	p = p.withDefaults()
	switch p.Mode {
	case ProxyPoolStatic:
		return fmt.Sprintf("静态 %s 端口 %d-%d（%d 个）", p.Endpoint(), p.PortStart, p.PortEnd, p.Size())
	case ProxyPoolDynamic:
		return "动态 " + p.Endpoint()
	default:
		return "关闭"
	}
}

// HostMatches 判断代理 proxy 与 IP 池地址是不是同一协议与主机（不比较账号密码），是时返回 proxy 的端口；
// 用于识别账户固定代理或 PROXY 指向池内端口（外部占用）。proxy 为空、无法规范化或 IP 池地址无法解析时为 false
func (p ProxyPool) HostMatches(proxy string) (port int, ok bool) {
	parts, valid := p.urlParts()
	if !valid {
		return 0, false
	}
	normalized, err := NormalizeProxy(proxy)
	if err != nil || normalized == "" {
		return 0, false
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Hostname() == "" {
		return 0, false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != parts.scheme || !strings.EqualFold(parsed.Hostname(), parts.host) {
		return 0, false
	}
	return proxyURLPort(scheme, parsed.Port())
}

// NormalizeProxyPoolURL 规范化 IP 池地址：{session} 的各种写法统一成小写 {session}，其余与 NormalizeProxy 相同（幂等）。
// 错误文案与 NormalizeProxy 相同，只把开头的 PROXY 换成 PROXY_POOL_URL，不回显输入
func NormalizeProxyPoolURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !proxyPoolPlaceholderPattern.MatchString(value) {
		normalized, err := NormalizeProxy(value)
		if err != nil {
			return "", proxyPoolError(err)
		}
		return normalized, nil
	}
	if strings.Contains(strings.ToLower(value), proxyPoolSessionToken) {
		return "", proxyPoolError(errProxyFormat)
	}
	normalized, err := NormalizeProxy(proxyPoolPlaceholderPattern.ReplaceAllLiteralString(value, proxyPoolSessionToken))
	if err != nil {
		return "", proxyPoolError(err)
	}
	return strings.ReplaceAll(normalized, proxyPoolSessionToken, ProxyPoolSessionPlaceholder), nil
}

// CanonicalProxyPoolURL 返回规范化后的 IP 池地址；无法规范化时返回去掉首尾空白的原值，留给 Validate 报错
func CanonicalProxyPoolURL(value string) string {
	if normalized, err := NormalizeProxyPoolURL(value); err == nil {
		return normalized
	}
	return strings.TrimSpace(value)
}

// NormalizeProxyPool 规范化并校验设置：模式转小写（空值按 off）、地址取规范值、端口为 0 时取默认值；
// 不能通过 Validate 时返回错误（文案不含地址内容）
func NormalizeProxyPool(p ProxyPool) (ProxyPool, error) {
	p = p.withDefaults()
	normalized, err := NormalizeProxyPoolURL(p.URL)
	if err != nil {
		return ProxyPool{}, err
	}
	p.URL = normalized
	if err := p.Validate(); err != nil {
		return ProxyPool{}, err
	}
	return p, nil
}

// proxyPoolOrDefault 用于 JSON 解析：旧数据没有该字段（或为 null）时取默认设置；否则补齐零值并取规范地址，
// 无法规范化的地址原样留给 Validate 报错
func proxyPoolOrDefault(value *ProxyPool) ProxyPool {
	if value == nil {
		return DefaultProxyPool()
	}
	pool := value.withDefaults()
	pool.URL = CanonicalProxyPoolURL(pool.URL)
	return pool
}

// loadProxyPool 从 env 读取 IP 池设置：键存在且非空时解析，没有写的项沿用默认值；模式与地址的组合留给 Validate 校验
func loadProxyPool(values map[string]string, pool *ProxyPool) error {
	if value, ok := values["PROXY_POOL_MODE"]; ok && strings.TrimSpace(value) != "" {
		pool.Mode = strings.ToLower(strings.TrimSpace(value))
	}
	if value, ok := values["PROXY_POOL_URL"]; ok && strings.TrimSpace(value) != "" {
		pool.URL = CanonicalProxyPoolURL(value)
	}
	for _, field := range []struct {
		key    string
		target *int
	}{
		{key: "PROXY_POOL_PORT_START", target: &pool.PortStart},
		{key: "PROXY_POOL_PORT_END", target: &pool.PortEnd},
	} {
		value, ok := values[field.key]
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		port, err := parsePortValue(field.key, value)
		if err != nil {
			return err
		}
		*field.target = port
	}
	return nil
}

// envValues 返回写进 .env 的规范值：模式小写、地址规范、端口为具体数字
func (p ProxyPool) envValues() map[string]string {
	p = p.withDefaults()
	return map[string]string{
		"PROXY_POOL_MODE":       p.Mode,
		"PROXY_POOL_URL":        CanonicalProxyPoolURL(p.URL),
		"PROXY_POOL_PORT_START": strconv.Itoa(p.PortStart),
		"PROXY_POOL_PORT_END":   strconv.Itoa(p.PortEnd),
	}
}

// parsePortValue 解析 1 到 65535 的端口号
func parsePortValue(key string, value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 1 || parsed > 65535 {
		return 0, fmt.Errorf("%s 必须是 1 到 65535 的整数", key)
	}
	return parsed, nil
}

// proxyPoolError 把 NormalizeProxy 的固定文案改成指向 PROXY_POOL_URL：只替换开头的 PROXY（SOCKS5、HTTP 开头的文案不变）
func proxyPoolError(err error) error {
	message := err.Error()
	if rest, ok := strings.CutPrefix(message, "PROXY "); ok {
		return errors.New("PROXY_POOL_URL " + rest)
	}
	return errors.New(message)
}

// withProxyPoolToken 把规范地址里的 {session} 换成令牌，供 url.Parse 解析（{ } 不是合法的 userinfo 字符）
func withProxyPoolToken(normalized string) string {
	return strings.ReplaceAll(normalized, ProxyPoolSessionPlaceholder, proxyPoolSessionToken)
}

// proxyPoolSessionValid 判断会话号只含小写字母与数字
func proxyPoolSessionValid(session string) bool {
	for index := 0; index < len(session); index++ {
		character := session[index]
		if !('a' <= character && character <= 'z' || '0' <= character && character <= '9') {
			return false
		}
	}
	return true
}

// proxyPoolUserinfo 从规范地址里取出 :// 与最后一个 @ 之间的账号密码原文（百分号编码保持不变）；没有账号密码时 ok 为 false
func proxyPoolUserinfo(normalized string) (userinfo string, ok bool) {
	_, rest, found := strings.Cut(normalized, "://")
	if !found {
		return "", false
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return "", false
	}
	return rest[:at], true
}

// splitProxyPoolUserinfo 把规范地址的账号密码原文按第一个冒号拆成用户名与密码
func splitProxyPoolUserinfo(normalized string) (user, password string, ok bool) {
	userinfo, ok := proxyPoolUserinfo(normalized)
	if !ok {
		return "", "", false
	}
	user, password, _ = strings.Cut(userinfo, ":")
	return user, password, true
}

// proxyPoolURLParts 为拆开的 IP 池地址：userinfo 为账号密码原文（含 {session}），换端口时原样拼回
type proxyPoolURLParts struct {
	scheme      string
	userinfo    string
	hasUserinfo bool
	user        string
	host        string
}

// urlParts 规范化地址并拆成各部分；地址为空或无法解析时 ok 为 false
func (p ProxyPool) urlParts() (proxyPoolURLParts, bool) {
	normalized, err := NormalizeProxyPoolURL(p.withDefaults().URL)
	if err != nil || normalized == "" {
		return proxyPoolURLParts{}, false
	}
	parsed, err := url.Parse(withProxyPoolToken(normalized))
	if err != nil || parsed.Hostname() == "" {
		return proxyPoolURLParts{}, false
	}
	parts := proxyPoolURLParts{scheme: strings.ToLower(parsed.Scheme), host: parsed.Hostname()}
	parts.userinfo, parts.hasUserinfo = proxyPoolUserinfo(normalized)
	parts.user, _, _ = strings.Cut(parts.userinfo, ":")
	return parts, true
}

// withPort 返回换成端口 port 的规范 URL
func (parts proxyPoolURLParts) withPort(port int) string {
	hostPort := net.JoinHostPort(parts.host, strconv.Itoa(port))
	if !parts.hasUserinfo {
		return parts.scheme + "://" + hostPort
	}
	return parts.scheme + "://" + parts.userinfo + "@" + hostPort
}

// endpoint 返回 scheme://host（IPv6 加方括号）
func (parts proxyPoolURLParts) endpoint() string {
	host := parts.host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return parts.scheme + "://" + host
}

// proxyURLPort 返回代理端口：没写端口时 http 为 80、https 为 443
func proxyURLPort(scheme, port string) (int, bool) {
	if port == "" {
		switch scheme {
		case "http":
			return 80, true
		case "https":
			return 443, true
		}
		return 0, false
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return 0, false
	}
	return number, true
}
