// Package proxyrelay 为带账号密码的上游代理提供本机中转：Camoufox 的 prefs 里没有账号密码字段，
// 中转在 127.0.0.1 随机端口上接受浏览器的连接，带上账号密码转给上游，隧道里的字节原样对拷
package proxyrelay

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultIdleGrace        = 5 * time.Minute
	defaultFirstByteTimeout = 3 * time.Minute
	defaultHeaderTimeout    = 30 * time.Second
	defaultUpstreamTimeout  = 25 * time.Second
	defaultKeepAliveIdle    = 3 * time.Minute
	defaultHalfCloseGrace   = 60 * time.Second
	defaultMaxConnections   = 1024
	// tcpKeepAlive 为监听器与上游拨号的 TCP keepalive 间隔，隧道阶段的死连接靠它发现
	tcpKeepAlive = 30 * time.Second
	// acceptBackoffMin、acceptBackoffMax 为 Accept 出错（如 EMFILE）后的退避区间
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

// ErrInvalidUpstream 表示上游代理地址无法解析（文案不含地址）
var ErrInvalidUpstream = errors.New("代理地址无效")

// ErrUpstream 表示本机中转记到了连上游代理的失败（连不上、超时、407 等）：WrapFailure 有失败说明时把它包进返回的错误，
// 调用方用 errors.Is 判断启动失败是不是代理造成的（IP 池据此判断端口或会话连不通）
var ErrUpstream = errors.New("上游代理失败")

// upstreamMarker 只用于让 WrapFailure 的结果 errors.Is 命中 ErrUpstream：Error() 为空串，拼进文本后与改动前逐字相同
// （管理页与 tools/diag_*.sh 按现有文案匹配）
type upstreamMarker struct{}

func (upstreamMarker) Error() string { return "" }

func (upstreamMarker) Is(target error) bool { return target == ErrUpstream }

// errRegistryClosed 表示注册表已经 Close
var errRegistryClosed = errors.New("本机代理中转已关闭")

// Options 为注册表参数；零值字段取默认值，测试可以注入拨号与 TLS 配置
type Options struct {
	IdleGrace           time.Duration                                                        // 租约释放后，监听器要“没有进行中的连接且连续这么久没有新连接”才关闭，默认 5 分钟
	FirstByteTimeout    time.Duration                                                        // 接受连接后等第一个字节的上限，默认 3 分钟（Firefox 会预先建连、先空闲一段）
	HeaderTimeout       time.Duration                                                        // 收到第一个字节后读完请求头或 SOCKS 握手的上限，默认 30 秒；读完请求头后清掉，不作用于请求体
	UpstreamTimeout     time.Duration                                                        // 连上游 + TLS + CONNECT 或 SOCKS 握手的总上限，默认 25 秒（小于 Go 侧 30 秒的握手期限）
	KeepAliveIdle       time.Duration                                                        // 明文请求之间的空闲上限，默认 3 分钟（大于 Firefox 长连接的 115 秒）
	HalfCloseGrace      time.Duration                                                        // 一个方向结束后等另一方向的上限，默认 60 秒
	MaxConnections      int                                                                  // 单个监听器（即单个浏览器）同时处理的连接数，默认 1024，大于 Firefox network.http.max-connections 的 900
	MaxTotalConnections int                                                                  // 整个注册表同时处理的连接数，默认按 RLIMIT_NOFILE 推算（Windows 为常量），防 fd 耗尽
	Log                 func(level, message string)                                          // 默认丢弃
	DialContext         func(ctx context.Context, network, address string) (net.Conn, error) // 默认 net.Dialer{KeepAlive: 30s}
	RootCAs             *x509.CertPool                                                       // https 上游的根证书，默认系统根证书（测试注入）
	Now                 func() time.Time
}

// withDefaults 为零值或非法字段填默认值
func (options Options) withDefaults() Options {
	if options.IdleGrace <= 0 {
		options.IdleGrace = defaultIdleGrace
	}
	if options.FirstByteTimeout <= 0 {
		options.FirstByteTimeout = defaultFirstByteTimeout
	}
	if options.HeaderTimeout <= 0 {
		options.HeaderTimeout = defaultHeaderTimeout
	}
	if options.UpstreamTimeout <= 0 {
		options.UpstreamTimeout = defaultUpstreamTimeout
	}
	if options.KeepAliveIdle <= 0 {
		options.KeepAliveIdle = defaultKeepAliveIdle
	}
	if options.HalfCloseGrace <= 0 {
		options.HalfCloseGrace = defaultHalfCloseGrace
	}
	if options.MaxConnections <= 0 {
		options.MaxConnections = defaultMaxConnections
	}
	if options.MaxTotalConnections <= 0 {
		options.MaxTotalConnections = defaultTotalConnections()
	}
	if options.DialContext == nil {
		dialer := &net.Dialer{KeepAlive: tcpKeepAlive}
		options.DialContext = dialer.DialContext
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return options
}

// Registry 管理本机中转监听器：每个租约一个监听器
type Registry struct {
	options  Options
	log      atomic.Pointer[func(level, message string)]
	listen   func(ctx context.Context) (net.Listener, error) // 测试可替换，默认只绑 127.0.0.1 随机端口
	ctx      context.Context                                 // Close 时取消，打断进行中的上游拨号与握手
	cancel   context.CancelFunc
	mu       sync.Mutex
	entries  map[*relayEntry]struct{} // 活着的监听器；每个租约一个
	total    atomic.Int64             // 全部监听器上进行中的连接数，对照 MaxTotalConnections
	closed   bool
	throttle logThrottle // 进程级日志的节流
}

// NewRegistry 创建注册表；进程内通常只用包级默认注册表
func NewRegistry(options Options) *Registry {
	options = options.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	registry := &Registry{
		options: options,
		ctx:     ctx,
		cancel:  cancel,
		entries: make(map[*relayEntry]struct{}),
	}
	registry.listen = listenLoopback
	registry.setLogger(options.Log)
	return registry
}

// listenLoopback 只用 127.0.0.1 字面量监听随机端口：不用 localhost 或 0.0.0.0
func listenLoopback(ctx context.Context) (net.Listener, error) {
	config := net.ListenConfig{KeepAlive: tcpKeepAlive}
	return config.Listen(ctx, "tcp4", "127.0.0.1:0")
}

// defaultRegistry 为包级默认注册表：主程序与 setup 各是独立进程，各用自己的
var defaultRegistry = NewRegistry(Options{})

// TotalConnectionLimit 返回包级默认注册表的进程连接上限（只读）：IP 池开启时按它检查浏览器多时会不会出现“连接数已满：整个进程”
func TotalConnectionLimit() int {
	return defaultRegistry.options.MaxTotalConnections
}

// Acquire 使用包级默认注册表
func Acquire(upstream string, owner string) (*Lease, error) {
	return defaultRegistry.Acquire(upstream, owner)
}

// SetLogger 设置包级默认注册表的日志出口（原子替换，可重复调用）；nil 表示丢弃
func SetLogger(log func(level, message string)) {
	defaultRegistry.setLogger(log)
}

func (registry *Registry) setLogger(log func(level, message string)) {
	if log == nil {
		registry.log.Store(nil)
		return
	}
	registry.log.Store(&log)
}

func (registry *Registry) logf(level, message string) {
	if log := registry.log.Load(); log != nil {
		(*log)(level, message)
	}
}

func (registry *Registry) now() time.Time {
	return registry.options.Now()
}

// Acquire 为上游代理申请一个本机中转租约：带账号密码时新起一个 127.0.0.1 随机端口的中转，只给这一个租约用
// （同一上游的多次申请也各自独立）；不带账号密码（含本机中转地址本身、空串）时原样返回、不起监听。
// owner 只用于日志（账户 ID 或操作名）
func (registry *Registry) Acquire(upstream string, owner string) (*Lease, error) {
	value := strings.TrimSpace(upstream)
	if value == "" {
		return &Lease{address: upstream, owner: owner}, nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		// url.Parse 的错误里有原文和密码，不能包装
		return nil, ErrInvalidUpstream
	}
	if parsed.User == nil {
		// 直通：原值交给 Camoufox，行为与不经中转时逐字节一致；中转地址本身也走这里，所以是幂等的
		return &Lease{address: upstream, owner: owner}, nil
	}
	target, err := parseUpstream(parsed)
	if err != nil {
		return nil, ErrInvalidUpstream
	}
	target.raw = value
	registry.mu.Lock()
	if registry.closed {
		registry.mu.Unlock()
		return nil, errRegistryClosed
	}
	listener, err := registry.listen(context.Background())
	if err != nil {
		registry.mu.Unlock()
		return nil, fmt.Errorf("启动本机代理中转: %w", err)
	}
	local := listener.Addr().String()
	address := "http://" + local
	if target.scheme == "socks5" {
		address = "socks5://" + local
	}
	entry := &relayEntry{
		registry:     registry,
		upstream:     target,
		listener:     listener,
		address:      address,
		local:        local,
		owner:        owner,
		lastActivity: registry.now(),
		conns:        make(map[net.Conn]bool),
		done:         make(chan struct{}),
	}
	registry.entries[entry] = struct{}{}
	registry.mu.Unlock()
	go entry.acceptLoop()
	registry.logf("INFO", fmt.Sprintf("本机代理中转启动 | 上游=%s | 本机=%s | 使用者=%s", target.display, local, owner))
	return &Lease{entry: entry, address: address, owner: owner}, nil
}

// Close 关闭全部监听器与进行中的连接；只用于测试与进程退出前清理
func (registry *Registry) Close() error {
	registry.mu.Lock()
	if registry.closed {
		registry.mu.Unlock()
		return nil
	}
	registry.closed = true
	entries := make([]*relayEntry, 0, len(registry.entries))
	for entry := range registry.entries {
		entries = append(entries, entry)
	}
	registry.entries = make(map[*relayEntry]struct{})
	registry.mu.Unlock()
	registry.cancel()
	for _, entry := range entries {
		entry.shutdown()
	}
	for _, entry := range entries {
		<-entry.done
		entry.handlers.Wait()
	}
	return nil
}

func (registry *Registry) remove(entry *relayEntry) {
	registry.mu.Lock()
	delete(registry.entries, entry)
	registry.mu.Unlock()
}

// Lease 为一次中转使用；方法对 nil 安全
type Lease struct {
	entry   *relayEntry // nil 表示直通
	address string
	owner   string
	once    sync.Once
}

// Address 返回交给 Camoufox 的代理地址：http://127.0.0.1:N、socks5://127.0.0.1:N，直通时为原值
func (lease *Lease) Address() string {
	if lease == nil {
		return ""
	}
	return lease.address
}

// Relayed 表示是否经本机中转
func (lease *Lease) Relayed() bool {
	return lease != nil && lease.entry != nil
}

// Release 释放租约，可重复调用；之后监听器在没有进行中的连接、且连续 IdleGrace 没有新连接时关闭
func (lease *Lease) Release() {
	if lease == nil || lease.entry == nil {
		return
	}
	lease.once.Do(lease.entry.release)
}

// FailureSince 返回 since 之后这个租约记录的最近一次相关失败说明（已脱敏），没有时为空。
// 上游层面的失败（认证、连不上、超时、TLS、响应无法解析、连接数已满）总是相关；
// 按目标的失败（上游对某个 CONNECT 回非 200、上游 SOCKS 回 REP≠0）只在 relevant(目标主机) 为真时相关，relevant 为 nil 时不相关
func (lease *Lease) FailureSince(since time.Time, relevant func(host string) bool) string {
	if lease == nil || lease.entry == nil {
		return ""
	}
	return lease.entry.failureSince(since, relevant)
}

// WrapFailure 在 err 后附上 FailureSince(since, relevant) 的说明，便于管理页直接看到 407 等原因，并让结果 errors.Is 命中
// ErrUpstream；没有失败说明时原样返回 err
func (lease *Lease) WrapFailure(err error, since time.Time, relevant func(host string) bool) error {
	if err == nil {
		return nil
	}
	failure := lease.FailureSince(since, relevant)
	if failure == "" {
		return err
	}
	// 第二个 %w 的文本为空串：文本与以前逐字相同，errors.Is 同时命中原错误与 ErrUpstream
	return fmt.Errorf("%w；本机代理中转：%s%w", err, failure, upstreamMarker{})
}

// relayEntry 为一个租约独占的监听器
type relayEntry struct {
	registry     *Registry
	upstream     upstreamTarget
	listener     net.Listener
	address      string // http://127.0.0.1:N 或 socks5://127.0.0.1:N
	local        string // 127.0.0.1:N，只用于日志
	owner        string // 只用于日志
	mu           sync.Mutex
	released     bool              // mu 保护
	releasedAt   time.Time         // mu 保护
	closed       bool              // mu 保护：监听器已关闭（按活动判定或 Registry.Close）
	active       int               // mu 保护：进行中的客户端连接数（含等首字节的预连接），决定能否关闭
	slots        int               // mu 保护：占用名额的连接数，对照 MaxConnections
	lastActivity time.Time         // mu 保护：最近一次接受连接或连接结束的时间
	lateWarned   bool              // mu 保护：释放后仍有新连接的 WARN 只记一次
	idle         *time.Timer       // mu 保护：释放后启动，到期时按活动判定
	conns        map[net.Conn]bool // mu 保护：只为 Registry.Close 关闭进行中的连接；true 为客户端，false 为上游原始连接
	failures     failureRing       // mu 保护：最近 8 条失败记录
	throttle     logThrottle
	handlers     sync.WaitGroup // 进行中的连接处理 goroutine，Registry.Close 等它们结束
	done         chan struct{}  // acceptLoop 退出时关闭
}

// acceptLoop 接受浏览器连接；监听器关闭时退出，EMFILE 等错误退避后重试，不空转也不退出
func (entry *relayEntry) acceptLoop() {
	defer close(entry.done)
	var backoff time.Duration
	for {
		connection, err := entry.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if backoff == 0 {
				backoff = acceptBackoffMin
			} else {
				backoff *= 2
			}
			if backoff > acceptBackoffMax {
				backoff = acceptBackoffMax
			}
			entry.logThrottled("accept", "WARN", fmt.Sprintf("本机代理中转接受连接失败 | 本机=%s | 错误=%s", entry.local, Scrub(err.Error(), entry.upstream.raw)))
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-entry.registry.ctx.Done():
				timer.Stop()
			}
			continue
		}
		backoff = 0
		if !entry.track(connection) {
			_ = connection.Close()
			continue
		}
		go entry.serve(connection)
	}
}

// track 登记新接受的客户端连接；监听器已关闭时返回 false
func (entry *relayEntry) track(connection net.Conn) bool {
	now := entry.registry.now()
	entry.mu.Lock()
	if entry.closed {
		entry.mu.Unlock()
		return false
	}
	entry.conns[connection] = true
	entry.active++
	entry.lastActivity = now
	entry.handlers.Add(1)
	late := entry.released && !entry.lateWarned
	var sinceRelease time.Duration
	if late {
		entry.lateWarned = true
		sinceRelease = now.Sub(entry.releasedAt)
	}
	entry.mu.Unlock()
	if late {
		entry.registry.logf("WARN", fmt.Sprintf("本机代理中转在租约释放后仍有新连接 | 本机=%s | 使用者=%s | 释放后=%s | 浏览器可能没有退出，中转继续保留",
			entry.local, entry.owner, formatDuration(sinceRelease)))
	}
	return true
}

// untrack 注销结束的客户端连接，记下活动时间
func (entry *relayEntry) untrack(connection net.Conn) {
	now := entry.registry.now()
	entry.mu.Lock()
	if _, ok := entry.conns[connection]; ok {
		delete(entry.conns, connection)
		entry.active--
	}
	entry.lastActivity = now
	entry.mu.Unlock()
	entry.handlers.Done()
}

// addUpstream 登记上游原始连接，只为 Registry.Close 能打断阻塞在上游上的读写；监听器已被 Close 时返回 false
func (entry *relayEntry) addUpstream(connection net.Conn) bool {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.registry.ctx.Err() != nil {
		return false
	}
	entry.conns[connection] = false
	return true
}

func (entry *relayEntry) removeUpstream(connection net.Conn) {
	entry.mu.Lock()
	if client, ok := entry.conns[connection]; ok && !client {
		delete(entry.conns, connection)
	}
	entry.mu.Unlock()
}

// acquireSlot 占一个名额，满了不等待：返回 false 与范围（单个浏览器 / 整个进程）及上限
func (entry *relayEntry) acquireSlot() (bool, string, int) {
	options := entry.registry.options
	entry.mu.Lock()
	if entry.slots >= options.MaxConnections {
		entry.mu.Unlock()
		return false, "单个浏览器", options.MaxConnections
	}
	limit := int64(options.MaxTotalConnections)
	var current int64
	for {
		current = entry.registry.total.Load()
		if current >= limit {
			entry.mu.Unlock()
			return false, "整个进程", options.MaxTotalConnections
		}
		if entry.registry.total.CompareAndSwap(current, current+1) {
			break
		}
	}
	entry.slots++
	entry.mu.Unlock()
	if (current+1)*5 >= limit*4 {
		registry := entry.registry
		if emit, suppressed := registry.throttle.allow("near-limit", registry.now()); emit {
			registry.logf("WARN", withSuppressed(fmt.Sprintf("本机代理中转连接数接近上限 | 进行中=%d | 上限=%d", current+1, limit), suppressed))
		}
	}
	return true, "", 0
}

func (entry *relayEntry) releaseSlot() {
	entry.mu.Lock()
	entry.slots--
	entry.mu.Unlock()
	entry.registry.total.Add(-1)
}

// release 释放租约：之后按活动判定关闭
func (entry *relayEntry) release() {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.released || entry.closed {
		return
	}
	entry.released = true
	entry.releasedAt = entry.registry.now()
	entry.idle = time.AfterFunc(entry.registry.options.IdleGrace, entry.checkIdle)
}

// checkIdle 按活动判定关闭：没有进行中的连接且连续 IdleGrace 没有新连接才关；不主动关闭进行中的隧道，
// 有进行中的连接说明对端浏览器还活着，监听器就一直留着
func (entry *relayEntry) checkIdle() {
	grace := entry.registry.options.IdleGrace
	now := entry.registry.now()
	entry.mu.Lock()
	if entry.closed {
		entry.mu.Unlock()
		return
	}
	quiet := now.Sub(entry.lastActivity)
	if entry.active > 0 || quiet < grace {
		wait := grace
		if entry.active == 0 {
			wait = grace - quiet
		}
		entry.idle.Reset(wait)
		entry.mu.Unlock()
		return
	}
	entry.closed = true
	sinceRelease := now.Sub(entry.releasedAt)
	_ = entry.listener.Close()
	entry.mu.Unlock()
	entry.registry.remove(entry)
	entry.registry.logf("INFO", fmt.Sprintf("本机代理中转关闭 | 上游=%s | 本机=%s | 使用者=%s | 释放后=%s",
		entry.upstream.display, entry.local, entry.owner, formatDuration(sinceRelease)))
}

// shutdown 供 Registry.Close 使用：关闭监听器与全部进行中的连接
func (entry *relayEntry) shutdown() {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.closed = true
	if entry.idle != nil {
		entry.idle.Stop()
	}
	_ = entry.listener.Close()
	for connection := range entry.conns {
		_ = connection.Close()
	}
}

// serve 处理一条浏览器连接
func (entry *relayEntry) serve(connection net.Conn) {
	defer entry.untrack(connection)
	defer connection.Close()
	// 只是防御：监听本来就只绑在回环上
	remote, ok := connection.RemoteAddr().(*net.TCPAddr)
	if !ok || !remote.IP.IsLoopback() {
		return
	}
	if entry.upstream.scheme == "socks5" {
		entry.serveSOCKS(connection)
		return
	}
	entry.serveHTTP(connection)
}

// GoogleTarget 判断目标主机是否属于 Google（google.com、googleapis.com、gstatic.com、googleusercontent.com 及其子域），
// 供 Worker 与登录路径作为 relevant 传入
func GoogleTarget(host string) bool {
	host = strings.TrimSpace(host)
	if split, _, err := net.SplitHostPort(host); err == nil {
		host = split
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, domain := range []string{"google.com", "googleapis.com", "gstatic.com", "googleusercontent.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// formatDuration 把时长写成 5m12s 这种形式
func formatDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	return duration.Round(time.Second).String()
}

const (
	// minTotalConnections、maxTotalConnections 为进程级连接上限默认值的夹取区间
	minTotalConnections = 1024
	maxTotalConnections = 65536
)

func clampTotalConnections(value uint64) int {
	if value < minTotalConnections {
		return minTotalConnections
	}
	if value > maxTotalConnections {
		return maxTotalConnections
	}
	return int(value)
}
