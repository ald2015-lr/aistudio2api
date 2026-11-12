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
//   - 总时长上限 = max(generateMaxDuration, REQUEST_TIMEOUT)：从建立上下文算起；long- 自动续写每开始一轮从此刻重新计算，
//     整段不超过总时长上限的 longOutputDurationFactor 倍（见 renewTotal）。long- 自动续写第二轮起登记了这一轮的结束函数
//     （beginLongRound）时，首事件后空闲期限与总时长上限（含绝对总时长上限）到期只结束这一轮，续写按这一轮失败体面结束
//     （已输出的内容完整返回、按 max_tokens 结束），longRoundGrace 后整个请求仍没有结束时才按期限到期结束。
//   - 扣住上游事件（holdUpstream）：工具约束要求必须调用时生成服务把上游已经返回的正文、思考扣住到调用出现（见 aistudio/tool_guard.go），
//     app 层收不到首事件，但上游正在输出。这时仍在首事件前（换号、首事件判定不变），只是退出首事件前的固定期限：期限（Deadline）改为
//     总时长上限，上游连续 REQUEST_TIMEOUT 既没有网络字节也没有新的发送（同一尝试内的重新生成，touchHold）时按扣住期间空闲到期；
//     long- 续写的各轮照常重新计算总时长上限，登记的这一轮结束函数同样生效。扣住的尝试没有交出首事件就结束（换号再试）时
//     退出扣住（endHold），首事件前期限恢复，扣住的时间不计入（期限顺延扣住的时长，不超过总时长上限）。
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
// Worker 层“进行中的请求最长持续多久”的估计（排空超时、后台关闭失败 Worker 的上限）同样按它；账号上有 long- 自动续写的租约时
// 按 longOutputMaxDurationFor
func generateMaxDurationFor(requestTimeout time.Duration) time.Duration {
	return max(generateMaxDuration, requestTimeout)
}

// longOutputDurationFactor 为 long- 自动续写的绝对总时长上限相对总时长上限的倍数：续写每开始一轮从此刻重新计算总时长上限，
// 但从建立请求上下文算起不超过总时长上限的这个倍数（默认 6 小时）。按官网续写约 100 万 token、每秒几十到一百多 token 估计，
// 多数长输出在这之内写完；没有这个上限时一个请求最多 64 轮、每轮各 1 小时，可以持续几十小时，排空与后台关闭等不到它结束
const longOutputDurationFactor = 6

// longRoundGrace 为 long- 自动续写第二轮起总时长上限到期、只结束当前这一轮之后，等整个请求体面结束（交出扣住的正文、用量与
// max_tokens 结束）的时间：过后仍没有结束时按总时长上限到期结束整个请求
const longRoundGrace = 30 * time.Second

// longOutputMaxDurationFor 返回 REQUEST_TIMEOUT 为 requestTimeout 时 long- 自动续写请求最长持续的时间：绝对总时长上限加上
// 到期后体面结束的等待（longRoundGrace）
func longOutputMaxDurationFor(requestTimeout time.Duration) time.Duration {
	return generateMaxDurationFor(requestTimeout)*longOutputDurationFactor + longRoundGrace
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
// 首事件前照常按超时处理、不换号。首事件前期限在两次尝试之间的等待中到期时例外（见 prior）：解包为上一次尝试的错误，对外照旧按它返回。
// 错误链里绝不能有 context.Canceled：statusFromError、访问日志与 finalRequestState 都先判断取消
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
	// prior 为首事件前期限在两次尝试之间的等待（换号退避、限流换号前等待）中到期时上一次尝试的错误（见 betweenAttemptsDeadlineError）：
	// 原文写在前缀之后；不含 context.Canceled 时同时是错误链的下一环，对外的状态码与文案、Retry-After、MODEL_FALLBACKS 的判断都与
	// 原来一样按它（协议错误码 4 与超时类仍是 504 官方原句，上游 503、429 照旧是 503、429，其他 5xx（如 500 INTERNAL）与断流
	// 照旧是 500，见 api 包的 upstreamPublicError）。含 context.Canceled 时（上一次的尝试被取消）错误链只到 context.DeadlineExceeded，
	// 与原来 startDeadlineError 的处理一致，请求状态不会记成 cancelled
	prior error
	// held 为期限在生成服务扣住上游事件期间到期（还没有交出首事件，见 holdUpstream）：stage 为空闲或总时长上限
	held bool
	// heldFor 为首事件前期限到期时此前扣住上游事件的累计时长（不计入首事件前期限，见 endHold）
	heldFor time.Duration
}

func (e *serviceDeadlineError) Error() string {
	if e.prior != nil {
		return e.head() + ": " + e.prior.Error()
	}
	return e.head() + ": " + e.Unwrap().Error()
}

// head 为错误文本里原错误之前的部分：哪一段期限、多长，首事件前期限到期时停在哪一步（扣住期间到期时文本已写明，不另写）
func (e *serviceDeadlineError) head() string {
	var head string
	switch {
	case e.held && e.stage == serviceDeadlineIdle:
		head = fmt.Sprintf("本服务扣住期间空闲期限 %s 到期（生成服务扣住上游事件等待工具调用，上游没有动静）", e.limit)
	case e.held:
		head = fmt.Sprintf("本服务总时长上限 %s 到期（生成服务扣住上游事件等待工具调用）", e.limit)
	case e.stage == serviceDeadlineStart:
		head = fmt.Sprintf("本服务首事件前期限 %s 到期", e.limit)
		if e.heldFor > 0 {
			head += fmt.Sprintf("（扣住上游事件的 %s 不计入）", e.heldFor.Round(time.Millisecond))
		}
		if e.where != "" {
			head += "（停在 " + e.where + "）"
		}
	case e.stage == serviceDeadlineIdle:
		head = fmt.Sprintf("本服务首事件后空闲期限 %s 到期（已输出 推理=%d 正文=%d）", e.limit, e.reasoning, e.content)
	default:
		head = fmt.Sprintf("本服务总时长上限 %s 到期（已输出 推理=%d 正文=%d）", e.limit, e.reasoning, e.content)
	}
	return head
}

func (e *serviceDeadlineError) Unwrap() error {
	if e.inner != nil {
		return e.inner
	}
	if e.prior != nil && !errors.Is(e.prior, context.Canceled) {
		return e.prior
	}
	return context.DeadlineExceeded
}

// label 为写进进度日志的阶段名：扣住上游事件期间到期时另标出
func (e *serviceDeadlineError) label() string {
	if !e.held {
		return e.stage.label()
	}
	if e.stage == serviceDeadlineIdle {
		return "扣住期间空闲"
	}
	return "扣住期间总时长上限"
}

// beforeFirstEvent 判断期限在交出首事件之前到期：首事件前期限，或扣住上游事件期间的空闲、总时长上限
func (e *serviceDeadlineError) beforeFirstEvent() bool {
	return e != nil && (e.stage == serviceDeadlineStart || e.held)
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
	longLimit   time.Duration // long- 自动续写的绝对总时长上限（maxDuration × longOutputDurationFactor，见 renewTotal）

	mu         sync.Mutex
	stage      generateStage
	deadline   time.Time
	totalLimit time.Duration // 首事件后当前期限对应的上限（总时长上限，或 renewTotal 截到绝对总时长上限时的 longLimit），写进到期原因
	timer      *time.Timer
	timerGen   uint64 // 计时器代数：延长、结束时加一，旧计时器的回调作废
	err        error
	cause      *serviceDeadlineError
	funcs      map[*generateAfterFunc]struct{} // 结束时置 nil
	stopParent func() bool
	// longRound 为 long- 自动续写第二轮起当前这一轮的结束函数（见 beginLongRound，没有时为 nil）；longRoundFired 为已经为这一轮
	// 调用过（之后到期按原规则结束整个请求）
	longRound      func(error)
	longRoundFired bool
	// held 为首事件前生成服务扣住了上游已经返回的事件（见 holdUpstream）：期限（deadline）为总时长上限，计时器按上游活动判空闲；
	// heldSince 为这次扣住的开始时刻，heldProgress 为最近一次新的发送（扣住开始、同一尝试内重新生成，见 touchHold），
	// heldQuiet 返回上游从 progress 起连续没有活动的时间（本次尝试的网络字节，long- 两轮之间暂停计时）；
	// heldFor 为此前各次扣住的累计时长（退出扣住后不计入首事件前期限）
	held         bool
	heldSince    time.Time
	heldProgress time.Time
	heldQuiet    func(now, progress time.Time) time.Duration
	heldFor      time.Duration
}

var _ context.Context = (*generateContext)(nil)

// newGenerateContext 建立生成请求的上下文：首事件前期限与首事件后空闲期限为 requestTimeout，总时长上限为 maxDuration
// （不小于 requestTimeout），都从 startedAt 算起；父上下文结束时随之结束
func newGenerateContext(parent context.Context, startedAt time.Time, requestTimeout, maxDuration time.Duration) *generateContext {
	maxDuration = max(maxDuration, requestTimeout)
	c := &generateContext{
		parent: parent, done: make(chan struct{}), startedAt: startedAt,
		startLimit: requestTimeout, idleLimit: requestTimeout, maxDuration: maxDuration,
		longLimit: maxDuration * longOutputDurationFactor, totalLimit: maxDuration,
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

// Deadline 返回当前期限：首事件前为首事件前期限（扣住上游事件期间为总时长上限，见 holdUpstream），首事件后为总时长上限；
// 父上下文的期限更早时按父上下文
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
	c.armTimerLocked(deadline)
}

// armTimerLocked 只重设计时器（到 at 时检查期限），不改期限
func (c *generateContext) armTimerLocked(at time.Time) {
	c.timerGen++
	gen := c.timerGen
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = time.AfterFunc(time.Until(at), func() { c.deadlineReached(gen) })
}

// heldQuietLocked 返回扣住期间上游从最近一次新的发送起连续没有活动的时间
func (c *generateContext) heldQuietLocked(now time.Time) time.Duration {
	if c.heldQuiet != nil {
		return c.heldQuiet(now, c.heldProgress)
	}
	return now.Sub(c.heldProgress)
}

// armHeldLocked 扣住期间重设计时器：到空闲期限（按上游实际活动算出的剩余）与总时长上限中较早的一个时检查
func (c *generateContext) armHeldLocked(now time.Time) {
	at := c.deadline
	if idleAt := now.Add(max(c.idleLimit-c.heldQuietLocked(now), 0)); idleAt.Before(at) {
		at = idleAt
	}
	c.armTimerLocked(at)
}

// deadlineReached 为第 gen 代计时器到时：首事件前为首事件前期限到期（扣住上游事件期间为空闲或总时长上限，见 holdUpstream），
// 首事件后为总时长上限到期
func (c *generateContext) deadlineReached(gen uint64) {
	c.mu.Lock()
	if c.err != nil || gen != c.timerGen {
		c.mu.Unlock()
		return
	}
	now := time.Now()
	cause := &serviceDeadlineError{stage: serviceDeadlineStart, limit: c.startLimit, elapsed: now.Sub(c.startedAt), heldFor: c.heldFor}
	if c.stage == generateStarting && c.held {
		// 扣住上游事件期间：上游仍有活动时只把检查推后；空闲或到总时长上限时到期
		cause.held, cause.heldFor = true, 0
		if now.Before(c.deadline) {
			if c.heldQuietLocked(now) < c.idleLimit {
				c.armHeldLocked(now)
				c.mu.Unlock()
				return
			}
			cause.stage, cause.limit = serviceDeadlineIdle, c.idleLimit
		} else {
			cause.stage, cause.limit = serviceDeadlineTotal, c.totalLimit
		}
		if interrupt := c.longRound; interrupt != nil && !c.longRoundFired {
			// long- 自动续写第二轮起：只结束当前这一轮（与首事件后相同），之后仍没有结束时再按原规则结束整个请求
			c.longRoundFired = true
			if cause.stage == serviceDeadlineTotal {
				c.deadline = now.Add(longRoundGrace)
			}
			c.heldProgress = now
			c.armHeldLocked(now)
			c.mu.Unlock()
			interrupt(errors.New(cause.head()))
			return
		}
		after := c.finishLocked(context.DeadlineExceeded, cause)
		c.mu.Unlock()
		after()
		return
	}
	if c.stage == generateStreaming {
		cause.stage, cause.limit = serviceDeadlineTotal, c.totalLimit
		if interrupt := c.longRound; interrupt != nil && !c.longRoundFired {
			// long- 自动续写第二轮起：只结束当前这一轮，续写体面结束；longRoundGrace 后仍没有结束时再按总时长上限结束整个请求
			c.longRoundFired = true
			c.armLocked(time.Now().Add(longRoundGrace))
			c.mu.Unlock()
			interrupt(fmt.Errorf("本服务总时长上限 %s 到期", cause.limit))
			return
		}
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
		if c.held {
			// 扣住上游事件之后交出首事件：期限已是总时长上限（long- 续写的各轮可能已重新计算过），照用
			c.held, c.heldQuiet = false, nil
			c.armLocked(c.deadline)
		} else {
			c.armLocked(c.startedAt.Add(c.maxDuration))
		}
	}
	return true
}

// holdUpstream 在生成服务扣住上游已经返回的事件时调用（app 层经 firstEventDeadline.PauseFirstEvent，见 aistudio/tool_guard.go）：
// 仍在首事件前，但退出首事件前的固定期限，期限改为总时长上限，上游连续一个 REQUEST_TIMEOUT 没有活动（quiet 返回从 progress 起
// 连续没有活动的时间）时按扣住期间空闲到期。同一尝试内再次扣住（重新生成之后）只刷新空闲计时。首事件后、已结束或为 nil 时
// 不做任何事并返回 false
func (c *generateContext) holdUpstream(quiet func(now, progress time.Time) time.Duration) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || c.stage != generateStarting {
		return false
	}
	now := time.Now()
	if !c.held {
		c.held, c.heldSince = true, now
		c.deadline, c.totalLimit = c.startedAt.Add(c.maxDuration), c.maxDuration
	}
	c.heldQuiet, c.heldProgress = quiet, now
	c.armHeldLocked(now)
	return true
}

// touchHold 在扣住上游事件的尝试内重新发送时调用（重新生成、续写回退的重发，见 firstEventDeadline.RestartFirstEvent）：
// 空闲计时从此刻重新计（准备 WAA proof、等响应头与首个字节合计仍受 REQUEST_TIMEOUT 约束）。没有扣住或为 nil 时不做任何事
func (c *generateContext) touchHold() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || c.stage != generateStarting || !c.held {
		return
	}
	c.heldProgress = time.Now()
}

// endHold 在扣住过上游事件的尝试没有交出首事件就结束时调用（以错误结束、换号再试）：退出扣住，首事件前期限恢复并顺延扣住的时长
// （扣住期间上游在正常输出，不算等首事件的时间），不超过总时长上限；已到时立即按首事件前期限到期。没有扣住或为 nil 时不做任何事
func (c *generateContext) endHold() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || c.stage != generateStarting || !c.held {
		return
	}
	c.heldFor += time.Since(c.heldSince)
	c.held, c.heldQuiet = false, nil
	c.totalLimit = c.maxDuration
	deadline := c.startedAt.Add(c.startLimit + c.heldFor)
	if ceiling := c.startedAt.Add(c.maxDuration); deadline.After(ceiling) {
		deadline = ceiling
	}
	c.armLocked(deadline)
}

// renewTotal 在 long- 自动续写开始新一轮时调用（见 long_output.go）：首事件后的总时长上限从此刻重新计算（照上游每轮重新计时的做法），
// 多轮续写的长输出不会被按整段计算的总时长上限截断；每一轮仍受总时长上限约束，整段从建立上下文算起不超过绝对总时长上限
// （longLimit，到期按总时长上限截断，原因里的期限为它）。首事件前、已结束或为 nil 时不做任何事并返回 false
func (c *generateContext) renewTotal() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.renewTotalLocked()
}

// renewTotalLocked 为 renewTotal 在 mu 内的部分；扣住上游事件期间（见 holdUpstream）同样重新计算总时长上限
func (c *generateContext) renewTotalLocked() bool {
	held := c.stage == generateStarting && c.held
	if c.err != nil || c.stage != generateStreaming && !held {
		return false
	}
	now := time.Now()
	deadline, limit := now.Add(c.maxDuration), c.maxDuration
	if ceiling := c.startedAt.Add(c.longLimit); deadline.After(ceiling) {
		deadline, limit = ceiling, c.longLimit
	}
	c.totalLimit = limit
	if held {
		c.deadline = deadline
		c.armHeldLocked(now)
		return true
	}
	c.armLocked(deadline)
	return true
}

// beginLongRound 在 long- 自动续写决定发下一轮（第二轮起）时调用（见 long_output.go）：登记这一轮的结束函数 interrupt（之后本服务的
// 首事件后空闲期限（interruptLongRound）与总时长上限、绝对总时长上限到期时只结束这一轮），首事件后再从此刻重新计算总时长上限
// （同 renewTotal，等号池准入的时间不占下一轮的总时长）。第一轮很短时解码可能先于转发首个事件就决定续写：这时还在首事件前，
// 只登记、不改期限（转发首个事件时 beginStreaming 照常设总时长上限），之后的期限到期同样只结束这一轮。
// 返回离绝对总时长上限的剩余时间；已结束或为 nil 时不做任何事，ok 为假
func (c *generateContext) beginLongRound(interrupt func(error)) (remaining time.Duration, ok bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, false
	}
	// 先登记再重设计时器（同一把锁内）：已过绝对总时长上限时计时器立即到时，到时的回调看得到这一轮的登记，只结束这一轮
	c.longRound, c.longRoundFired = interrupt, false
	c.renewTotalLocked()
	return time.Until(c.startedAt.Add(c.longLimit)), true
}

// endLongRound 在 long- 自动续写的一轮结束（或续写失败）时清除登记的结束函数：之后的期限到期按原规则结束整个请求。nil 安全
func (c *generateContext) endLongRound() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.longRound, c.longRoundFired = nil, false
	c.mu.Unlock()
}

// interruptLongRound 在首事件后空闲期限到期时调用（forwardEvents 的看门狗）：long- 自动续写第二轮起的这一轮还没有被结束过时
// 只结束这一轮并返回 true；没有登记（第一轮、普通请求）、这一轮已经结束过一次（之后仍没有动静）或请求已结束时返回 false，
// 由调用方按原规则结束整个请求。nil 安全
func (c *generateContext) interruptLongRound(reason error) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	interrupt := c.longRound
	if c.err != nil || interrupt == nil || c.longRoundFired {
		c.mu.Unlock()
		return false
	}
	c.longRoundFired = true
	c.mu.Unlock()
	interrupt(reason)
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

// startExpired 判断是否在交出首事件之前因本服务期限到期而结束（首事件前期限，或扣住上游事件期间的空闲、总时长上限），nil 安全
func (c *generateContext) startExpired() bool {
	return c.deadlineCause().beforeFirstEvent()
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
// 保留原错误（带等待原因）；其他错误（上游先返回的真实错误）、已经标注过的、不是首事件前期限到期时返回 false，保留原样。
// 期限在两次尝试之间的等待中到期时 err 是上一次尝试的错误，改用 betweenAttemptsDeadlineError
func (c *generateContext) startDeadlineError(err error, where string) (*serviceDeadlineError, bool) {
	if err == nil {
		return nil, false
	}
	cause := c.deadlineCause()
	if !cause.beforeFirstEvent() {
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

// betweenAttemptsDeadlineError 在两次尝试之间的等待（换号退避、限流换号前等待）因首事件前期限到期而结束时，把请求的最终错误标注为
// 本服务的首事件前期限：where 为停在哪一步，err 为上一次尝试的错误（上游 RPC 错误如协议错误码 4、5xx，429，网络错误，超时类错误等），
// 原文写在前缀之后（“本服务首事件前期限 <期限> 到期（停在 换号退避）: <上一次错误>”），请求日志与账本据此分清是本服务的期限。
// 只改文本，客户端看到的不变：错误链接着上一次的错误（含 context.Canceled 时只到 context.DeadlineExceeded，见 prior），对外按它原来的
// 状态码与文案返回，码 4 与超时类仍是 504 官方原句。原先只有超时类的上一次错误加了前缀，上游 RPC 错误只留原文：码 4 时请求日志与
// 账本看起来像 Google 侧期限。不是首事件前期限到期（客户端断开、生成服务停止）、已经标注过或为 nil 时返回 false，保留原样
func (c *generateContext) betweenAttemptsDeadlineError(err error, where string) (*serviceDeadlineError, bool) {
	if err == nil {
		return nil, false
	}
	cause := c.deadlineCause()
	if !cause.beforeFirstEvent() {
		return nil, false
	}
	var marked *serviceDeadlineError
	if errors.As(err, &marked) {
		return nil, false
	}
	cut := *cause
	cut.where, cut.inner, cut.prior = where, nil, err
	return &cut, true
}

// generateContextFrom 返回 ctx 所属的生成请求上下文（经 WithValue 等包装也能取到）；不是生成请求时为 nil
func generateContextFrom(ctx context.Context) *generateContext {
	c, _ := ctx.Value(generateContextKey{}).(*generateContext)
	return c
}
