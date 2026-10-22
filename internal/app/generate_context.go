package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// 生成请求的上下文（generateContext）把原来整条请求一个 REQUEST_TIMEOUT 拆成三段：
//   - 首事件前总期限 = REQUEST_TIMEOUT：从 startGenerate 建立上下文算起，排队、选号、冷启动、文件复制、附件上传、WAA proof、
//     等首个上游事件与首事件前的换号重试都计入；到期时 Err() 为 context.DeadlineExceeded，对外 504、换号规则与原来相同；
//   - 收到首个非错误上游事件、转发之前 generateWithRetry 调用 beginStreaming：期限在原对象上延长到总时长上限，
//     之前派生的上下文（本次尝试、上游连接、响应体）随之延长，不需要逐个通知；
//   - 首事件后空闲期限 = REQUEST_TIMEOUT：由 forwardEvents 的看门狗判断，到期调用 expire；
//   - 总时长上限 = max(generateMaxDuration, REQUEST_TIMEOUT)：从建立上下文算起。
//
// 三种期限到期时 Err() 都是 context.DeadlineExceeded 本身（不包装），deadlineCause 给出是哪一段；
// 父上下文取消、管理端取消、生成服务停止时为父上下文的错误或 context.Canceled。三段期限都在创建时取值，热更新只影响新请求。
//
// 实现 AfterFunc(func()) func() bool：标准库的 WithCancel、WithCancelCause、context.AfterFunc 直接登记在这里，不起 goroutine
// （context.propagateCancel 的 afterFuncer 分支）。done 通道是自己的，不能复用内部标准 cancelCtx 的通道：否则标准库会把子上下文
// 直接挂到那个 cancelCtx 上，到期时子上下文拿到的是它的取消错误（Canceled），对外变成 503。
//
// 注意：首事件前 Deadline() 返回首事件前期限。不要在首事件前把 Deadline() 快照成绝对时间，用在首事件后仍在使用的连接或计时器上；
// 首事件前派生的 WithTimeout/WithDeadline 若期限晚于当时的首事件前期限，标准库直接返回 WithCancel（不带自己的计时器），
// 延长后它跟着延长、不再有自己的期限。现有的这类派生都在首事件前同步结束

// generateMaxDuration 为生成请求总时长上限的默认值：从建立请求上下文算起，含排队、冷启动、首事件前的换号与整段输出。
// 正常的长回复不受它影响，它只防上游以极低速率一直输出（空闲期限截不断）时请求长期占着账号与 Worker。
// 暂不做成配置项：需要更长时调大 REQUEST_TIMEOUT，总上限随之变大
const generateMaxDuration = time.Hour

// generateMaxDurationFor 返回 REQUEST_TIMEOUT 为 requestTimeout 时生成请求的总时长上限（不短于首事件前期限）。
// Worker 层“进行中的请求最长持续多久”的估计（排空超时、后台关闭失败 Worker 的上限）同样按它
func generateMaxDurationFor(requestTimeout time.Duration) time.Duration {
	return max(generateMaxDuration, requestTimeout)
}

// serviceDeadlineStage 为本服务自己的哪一段期限到期
type serviceDeadlineStage uint8

const (
	serviceDeadlineStart serviceDeadlineStage = iota + 1 // 首事件前总期限
	serviceDeadlineIdle                                  // 首事件后空闲期限
	serviceDeadlineTotal                                 // 总时长上限
)

// label 为写进进度日志的阶段名
func (stage serviceDeadlineStage) label() string {
	switch stage {
	case serviceDeadlineStart:
		return "首事件前"
	case serviceDeadlineIdle:
		return "首事件后空闲"
	default:
		return "总时长上限"
	}
}

// serviceDeadlineError 为本服务自己的期限到期（不是上游的“协议错误码 4”，也不是上游断流 “stream closed before finish”）：
// 只解包为 context.DeadlineExceeded（或包装的、本身满足 DeadlineExceeded 的等待错误），对外仍是 504 官方文案、状态记 failed，
// 首事件前照常按超时处理、不换号。错误链里绝不能有 context.Canceled：statusFromError、访问日志与 finalRequestState 都先判断取消
type serviceDeadlineError struct {
	stage   serviceDeadlineStage
	limit   time.Duration
	elapsed time.Duration // 从建立请求上下文到期限到期
	// reasoning、content 为首事件后已输出的推理与正文事件数
	reasoning, content int
	// where 为首事件前到期时本次尝试停在哪一步（见 startCutStage），不在尝试中时为空
	where string
	// inner 为首事件前到期时原来的错误（带等待原因的 ctx 错误，如“排队 … 后请求超时（最后原因=…）”）；只在它满足
	// DeadlineExceeded 且不含 context.Canceled 时保留
	inner error
}

func (e *serviceDeadlineError) Error() string {
	var head string
	switch e.stage {
	case serviceDeadlineStart:
		head = fmt.Sprintf("本服务首事件前期限 %s 到期", e.limit)
		if e.where != "" {
			head += "（停在 " + e.where + "）"
		}
	case serviceDeadlineIdle:
		head = fmt.Sprintf("本服务首事件后空闲期限 %s 到期（已输出 推理=%d 正文=%d）", e.limit, e.reasoning, e.content)
	default:
		head = fmt.Sprintf("本服务总时长上限 %s 到期（已输出 推理=%d 正文=%d）", e.limit, e.reasoning, e.content)
	}
	return head + ": " + e.Unwrap().Error()
}

func (e *serviceDeadlineError) Unwrap() error {
	if e.inner != nil {
		return e.inner
	}
	return context.DeadlineExceeded
}

// withOutput 返回带已输出事件数的副本
func (e *serviceDeadlineError) withOutput(reasoning, content int) *serviceDeadlineError {
	cut := *e
	cut.reasoning, cut.content = reasoning, content
	return &cut
}

type generateStage uint8

const (
	generateStarting  generateStage = iota // 首事件前：期限为 REQUEST_TIMEOUT
	generateStreaming                      // 首事件后：期限为总时长上限，空闲由 forwardEvents 的看门狗判断
)

type generateContextKey struct{}

type generateAfterFunc struct{ f func() }

// generateContext 为一次生成请求的上下文（见文件开头说明）。可变字段只在 mu 内读写；done 只在 finishLocked 里关闭一次；
// parent、startedAt 与三个期限构造后不再改变。锁顺序固定为“子上下文的锁 → mu”：标准库 propagateCancel 持有子上下文的锁调用
// AfterFunc，这里持有 mu 时从不调用子上下文（结束时的收尾都在锁外执行）
type generateContext struct {
	parent      context.Context
	done        chan struct{}
	startedAt   time.Time
	startLimit  time.Duration // 首事件前总期限（创建时的 REQUEST_TIMEOUT）
	idleLimit   time.Duration // 首事件后空闲期限（同一个值）
	maxDuration time.Duration // 总时长上限（不小于 startLimit）

	mu         sync.Mutex
	stage      generateStage
	deadline   time.Time
	timer      *time.Timer
	timerGen   uint64 // 计时器代数：延长、结束时加一，旧计时器的回调作废
	err        error
	cause      *serviceDeadlineError
	funcs      map[*generateAfterFunc]struct{} // 结束时置 nil
	stopParent func() bool
}

var _ context.Context = (*generateContext)(nil)

// newGenerateContext 建立生成请求的上下文：首事件前期限与首事件后空闲期限为 requestTimeout，总时长上限为 maxDuration
// （不小于 requestTimeout），都从 startedAt 算起；父上下文结束时随之结束
func newGenerateContext(parent context.Context, startedAt time.Time, requestTimeout, maxDuration time.Duration) *generateContext {
	maxDuration = max(maxDuration, requestTimeout)
	c := &generateContext{
		parent: parent, done: make(chan struct{}), startedAt: startedAt,
		startLimit: requestTimeout, idleLimit: requestTimeout, maxDuration: maxDuration,
		funcs: make(map[*generateAfterFunc]struct{}),
	}
	c.mu.Lock()
	c.armLocked(startedAt.Add(requestTimeout))
	// 父上下文已结束时 context.AfterFunc 在新 goroutine 里调用 finish，不会在这里持锁重入
	c.stopParent = context.AfterFunc(parent, func() { c.finish(parent.Err(), nil) })
	c.mu.Unlock()
	if err := parent.Err(); err != nil {
		c.finish(err, nil)
	}
	return c
}

// Deadline 返回当前期限：首事件前为首事件前期限，首事件后为总时长上限；父上下文的期限更早时按父上下文
func (c *generateContext) Deadline() (time.Time, bool) {
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	if parentDeadline, ok := c.parent.Deadline(); ok && parentDeadline.Before(deadline) {
		return parentDeadline, true
	}
	return deadline, true
}

func (c *generateContext) Done() <-chan struct{} {
	return c.done
}

// Err 只可能是 nil、context.DeadlineExceeded、context.Canceled 或父上下文的错误本身（不包装）
func (c *generateContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *generateContext) Value(key any) any {
	if _, ok := key.(generateContextKey); ok {
		return c
	}
	return c.parent.Value(key)
}

// AfterFunc 登记结束时调用的 f（标准库 context.propagateCancel 与 context.AfterFunc 经它登记子上下文，不起 goroutine）；
// 返回的 stop 在 f 还没被调用时取消登记并返回 true
func (c *generateContext) AfterFunc(f func()) func() bool {
	c.mu.Lock()
	if c.funcs == nil {
		c.mu.Unlock()
		// 已结束：不能同步调用 f。propagateCancel 持有子上下文的锁调用这里，f 又要取这把锁（取消子上下文），同步调用会死锁
		go f()
		return func() bool { return false }
	}
	entry := &generateAfterFunc{f: f}
	c.funcs[entry] = struct{}{}
	c.mu.Unlock()
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.funcs[entry]; !ok {
			return false
		}
		delete(c.funcs, entry)
		return true
	}
}

// cancel 以 context.Canceled 结束（管理端取消、生成服务停止、降级判定拒绝、转发结束）
func (c *generateContext) cancel() {
	c.finish(context.Canceled, nil)
}

// finish 以 err 结束，cause 为本服务期限到期时是哪一段（其他原因为 nil）；已结束时什么都不做并返回 false
func (c *generateContext) finish(err error, cause *serviceDeadlineError) bool {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return false
	}
	after := c.finishLocked(err, cause)
	c.mu.Unlock()
	after()
	return true
}

// finishLocked 在 mu 内记下结束原因、停止计时器、关闭 done，返回要在锁外执行的收尾（解除父上下文的登记、调用登记的 f）
func (c *generateContext) finishLocked(err error, cause *serviceDeadlineError) func() {
	c.err = err
	c.cause = cause
	c.timerGen++
	if c.timer != nil {
		c.timer.Stop()
	}
	funcs := c.funcs
	c.funcs = nil
	stopParent := c.stopParent
	c.stopParent = nil
	close(c.done)
	return func() {
		if stopParent != nil {
			stopParent()
		}
		for entry := range funcs {
			entry.f()
		}
	}
}

// armLocked 把期限设为 deadline 并重设计时器；旧计时器的回调因代数改变而作废
func (c *generateContext) armLocked(deadline time.Time) {
	c.deadline = deadline
	c.timerGen++
	gen := c.timerGen
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = time.AfterFunc(time.Until(deadline), func() { c.deadlineReached(gen) })
}

// deadlineReached 为第 gen 代计时器到时：首事件前为首事件前期限到期，首事件后为总时长上限到期
func (c *generateContext) deadlineReached(gen uint64) {
	c.mu.Lock()
	if c.err != nil || gen != c.timerGen {
		c.mu.Unlock()
		return
	}
	cause := &serviceDeadlineError{stage: serviceDeadlineStart, limit: c.startLimit, elapsed: time.Since(c.startedAt)}
	if c.stage == generateStreaming {
		cause.stage, cause.limit = serviceDeadlineTotal, c.maxDuration
	}
	after := c.finishLocked(context.DeadlineExceeded, cause)
	c.mu.Unlock()
	after()
}

// beginStreaming 在收到首个非错误上游事件、转发之前调用：切到首事件后，期限延长到总时长上限。与首事件前期限到期在同一把锁内互斥：
// 已经结束（期限到期或被取消）时返回 false，这个首事件不能再转发。nil（测试直接传普通 context）返回 true
func (c *generateContext) beginStreaming() bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false
	}
	if c.stage == generateStarting {
		c.stage = generateStreaming
		c.armLocked(c.startedAt.Add(c.maxDuration))
	}
	return true
}

// expire 以本服务期限到期结束（forwardEvents 的空闲看门狗使用）；已结束或为 nil 时返回 false
func (c *generateContext) expire(cause *serviceDeadlineError) bool {
	if c == nil {
		return false
	}
	return c.finish(context.DeadlineExceeded, cause)
}

// deadlineCause 返回本服务哪一段期限到期：只在计时器或 expire 结束时非空，nil 安全
func (c *generateContext) deadlineCause() *serviceDeadlineError {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cause
}

// startExpired 判断是否因首事件前期限到期而结束，nil 安全
func (c *generateContext) startExpired() bool {
	cause := c.deadlineCause()
	return cause != nil && cause.stage == serviceDeadlineStart
}

// idleTimeout 返回首事件后空闲期限；nil 时为 0，表示不检查空闲
func (c *generateContext) idleTimeout() time.Duration {
	if c == nil {
		return 0
	}
	return c.idleLimit
}

// startDeadlineError 在首事件前期限到期时把请求的最终错误 err 标注为本服务的首事件前期限（where 为停在哪一步）：
// err 满足 context.Canceled 时（首事件超时或期限取消的尝试）不保留原错误，错误链里不能有 Canceled；满足 DeadlineExceeded 时
// 保留原错误（带等待原因）；其他错误（上游先返回的真实错误）、已经标注过的、不是首事件前期限到期时返回 false，保留原样
func (c *generateContext) startDeadlineError(err error, where string) (*serviceDeadlineError, bool) {
	if err == nil {
		return nil, false
	}
	cause := c.deadlineCause()
	if cause == nil || cause.stage != serviceDeadlineStart {
		return nil, false
	}
	var marked *serviceDeadlineError
	if errors.As(err, &marked) {
		return nil, false
	}
	cut := *cause
	cut.where = where
	switch {
	case errors.Is(err, context.Canceled):
		return &cut, true
	case errors.Is(err, context.DeadlineExceeded):
		cut.inner = err
		return &cut, true
	}
	return nil, false
}

// generateContextFrom 返回 ctx 所属的生成请求上下文（经 WithValue 等包装也能取到）；不是生成请求时为 nil
func generateContextFrom(ctx context.Context) *generateContext {
	c, _ := ctx.Value(generateContextKey{}).(*generateContext)
	return c
}
