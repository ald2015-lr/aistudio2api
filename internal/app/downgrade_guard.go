package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 降级判定（拒绝被上游降级的回复）。
//
// 上游会把部分 gemini-3.1-pro-preview 请求改由 3.1-flash-lite 生成（触发原因是对话内容），响应照常返回：
// Build 通道只有最后一块的 modelVersion 标明实际模型，Playground 通道的响应里没有模型名。两者出字速度相差约一倍
// （Pro 约 90～150 tok/s，Flash-Lite 约 230～270），所以在把正文交给客户端之前按正文速度判定：
//   - Playground：正文阶段每块都带累计正文数（用量数组第 1 项），速度是精确的；
//   - Build：中间块只有输入数，用"第一块的输入 token ÷ 本次输入字数"的比例估算正文 token，估算速度落在模糊区间时
//     在同一账号上调用 CountTokens 精确计算；最后一块标明的模型作为最终确认。
// 判定窗口从第一块正文之后算起（第一块在到达前就已生成，用时不可知）：窗口内新增正文达到设定的 token 数、且窗口达到
// 最短时长时判定。判定为降级就取消上游、按各协议官方格式返回 400，不换号重试，并记住这段对话。
//
// 缓存方式：流式严格模式（默认）判定前缓存全部事件、不写响应头，Generate 等到判定出结果才返回（内容最多延后设定的时长：到时限还没判定就先返回 200 并发出思考，正文继续缓存，正文缓存也不超过这个时长）；流式快速模式实时转发
// 思考、只缓存正文，判定为降级时在流中发送错误事件；非流式请求判定前缓存，判定正常后继续收集并扣住结束事件，
// 上游结束时再复核一次（Build 看最后一块标明的模型，其余看整段正文速度）。

const (
	gateModeStrict = "strict"
	gateModeFast   = "fast"
	gateModeUnary  = "unary"

	verdictRejected = "rejected"
	verdictPassed   = "passed"
	verdictUnjudged = "unjudged"

	// downgradeBurstWindow 内与第一块正文一起到达的块视为同一批：它们在到达前就已生成，并入起点，不计入速度窗口
	downgradeBurstWindow = 30 * time.Millisecond
	// downgradeFinalMinWindow 为上游结束后按整段速度判定所需的最短窗口：判定窗口设得较长时，较短的回复在窗口凑满前就结束了，
	// 这时数据已经全部到齐，按这个较短的下限判定，避免短回复因"数据不足"漏判
	downgradeFinalMinWindow = 800 * time.Millisecond
	// downgradeMemoryPerKey、downgradeMemoryLimit 为对话记录的条数上限（同一开头的对话 / 全部）
	downgradeMemoryPerKey = 16
	downgradeMemoryLimit  = 20000
	// downgradeMemoryMinMessages 为记录被降级对话所需的最少消息条数
	downgradeMemoryMinMessages = 2
)

const (
	runPending = iota
	runPassed
	runStopped
)

// downgradeSettings 为降级判定的生效设置；服务配置页修改后整体替换，进行中的请求沿用开始时的设置
type downgradeSettings struct {
	enabled      bool
	models       map[string]struct{}
	threshold    float64
	minTokens    int64
	minWindow    time.Duration
	fuzzyLow     float64
	fuzzyHigh    float64
	countTimeout time.Duration
	fastMode     bool
	memory       time.Duration
	maxHold      time.Duration
	rejectStatus int
}

func newDowngradeSettings(guard config.DowngradeGuard) *downgradeSettings {
	settings := &downgradeSettings{
		enabled: guard.Enabled, models: make(map[string]struct{}, len(guard.Models)),
		threshold: guard.SpeedThreshold, minTokens: int64(guard.MinTokens),
		minWindow: time.Duration(guard.MinWindowMS) * time.Millisecond,
		fuzzyLow:  guard.FuzzyLow, fuzzyHigh: guard.FuzzyHigh,
		countTimeout: time.Duration(guard.CountTimeoutMS) * time.Millisecond,
		fastMode:     guard.FastMode, memory: time.Duration(guard.MemoryMinutes) * time.Minute,
		maxHold:      time.Duration(guard.MaxHoldMS) * time.Millisecond,
		rejectStatus: config.NormalizeDowngradeRejectStatus(guard.RejectStatus),
	}
	for _, model := range guard.Models {
		if name := normalizeGuardModel(model); name != "" {
			settings.models[name] = struct{}{}
		}
	}
	return settings
}

func normalizeGuardModel(model string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(model), "models/"))
}

// covers 判断请求的模型（别名已解析为规范 ID）是否在拦截列表里
func (settings *downgradeSettings) covers(model string) bool {
	if settings == nil || !settings.enabled {
		return false
	}
	_, ok := settings.models[normalizeGuardModel(model)]
	return ok
}

// setDowngradeGuard 应用降级判定设置（启动时与服务配置页保存后，立即生效）
func (service *trackedService) setDowngradeGuard(guard config.DowngradeGuard) {
	service.downgradeGuard.Store(newDowngradeSettings(guard))
}

func downgradeGuardToAPI(guard config.DowngradeGuard) *api.DowngradeGuardConfig {
	return &api.DowngradeGuardConfig{
		Enabled: guard.Enabled, Models: append([]string{}, guard.Models...),
		SpeedThreshold: guard.SpeedThreshold, MinTokens: guard.MinTokens, MinWindowMS: guard.MinWindowMS,
		FuzzyLow: guard.FuzzyLow, FuzzyHigh: guard.FuzzyHigh, CountTimeoutMS: guard.CountTimeoutMS,
		FastMode: guard.FastMode, MemoryMinutes: guard.MemoryMinutes, MaxHoldMS: guard.MaxHoldMS,
		RejectStatus: config.NormalizeDowngradeRejectStatus(guard.RejectStatus),
	}
}

func downgradeGuardFromAPI(value api.DowngradeGuardConfig) config.DowngradeGuard {
	return config.DowngradeGuard{
		Enabled: value.Enabled, Models: config.NormalizeModelList(value.Models),
		SpeedThreshold: value.SpeedThreshold, MinTokens: value.MinTokens, MinWindowMS: value.MinWindowMS,
		FuzzyLow: value.FuzzyLow, FuzzyHigh: value.FuzzyHigh, CountTimeoutMS: value.CountTimeoutMS,
		FastMode: value.FastMode, MemoryMinutes: value.MemoryMinutes, MaxHoldMS: value.MaxHoldMS,
		RejectStatus: config.NormalizeDowngradeRejectStatus(value.RejectStatus),
	}
}

// downgradeGuardSummary 为热更新日志里的一句话概况
func downgradeGuardSummary(guard config.DowngradeGuard) string {
	if !guard.Enabled {
		return "关闭"
	}
	mode := "严格模式"
	if guard.FastMode {
		mode = "快速模式"
	}
	hold := "内容延后不限时"
	if guard.MaxHoldMS > 0 {
		hold = fmt.Sprintf("内容最多延后 %.1f 秒", float64(guard.MaxHoldMS)/1000)
	}
	return fmt.Sprintf("%s（%s，%s，%.0f tok/s，拒绝返回 %d）", strings.Join(guard.Models, ","), mode, hold, guard.SpeedThreshold,
		config.NormalizeDowngradeRejectStatus(guard.RejectStatus))
}

// downgradeGate 为一次被拦截模型请求的降级判定
type downgradeGate struct {
	service    *trackedService
	settings   *downgradeSettings
	requestID  string
	model      string
	family     string
	mode       string
	clientCtx  context.Context
	cancel     context.CancelFunc
	system     string
	contents   []aistudio.Content
	memoryKey  string
	inputChars int64
	hasMedia   bool

	// 以下由 forwardEvents 在拿到账号后写入（attach），判定 goroutine 读取
	mu           sync.Mutex
	lease        *aistudio.AccountLease
	countCtx     context.Context
	channel      string
	accountLabel string
	rejected     *aistudio.ModelDowngradedError
}

// prepareDowngradeGate 为被拦截的模型准备判定；不拦截时返回 nil。同一段对话近期被判定为降级、且当时的消息原样都在时
// 返回拒绝错误（发送前直接拒绝）。contents 为加随机后缀之前的原始内容
func (service *trackedService) prepareDowngradeGate(ctx context.Context, request aistudio.GenerateRequest, contents []aistudio.Content) (*downgradeGate, *aistudio.ModelDowngradedError) {
	settings := service.downgradeGuard.Load()
	if !settings.covers(request.Model) || !randomSeedApplicable(request.Model, request.Config) {
		return nil, nil
	}
	gate := &downgradeGate{
		service: service, settings: settings, requestID: request.ID, model: request.Model,
		family: aistudio.ModelFamily(request.Model), mode: gateModeUnary, clientCtx: ctx,
		system: request.System, contents: contents,
	}
	if request.Stream {
		gate.mode = gateModeStrict
		if settings.fastMode {
			gate.mode = gateModeFast
		}
	}
	// 字数按实际发往上游的内容（含随机后缀）计算：上游输入 token 数包含后缀，分母不含后缀会让短提示词的比例被放大数倍
	gate.inputChars, gate.hasMedia = guardInputSize(request.System, request.Contents, request.Tools)
	if settings.memory <= 0 || len(contents) == 0 {
		return gate, nil
	}
	gate.memoryKey = conversationHash(request.Model, request.System, contents[:1])
	// 走 /trace/ 排查路由的请求（测试工具反复发送同一段对话）不查历史记录，照常判定与记录
	if aistudio.TraceFromContext(ctx).Active() {
		return gate, nil
	}
	age, messages, ok := conversationMemory.lookup(gate.memoryKey, request.Model, request.System, contents, time.Now(), settings.memory)
	if !ok {
		return gate, nil
	}
	decision := aistudio.DowngradeDecision{
		Verdict: verdictRejected, Reason: "memory", Mode: gate.mode, Memory: true,
		Basis: fmt.Sprintf("同一段对话 %s前被判定为降级，当时的 %d 条消息原样都在，发送前直接拒绝（记录保留 %s，改过其中任何一条即重新判定）",
			guardDuration(age), messages, guardDuration(settings.memory)),
	}
	return nil, &aistudio.ModelDowngradedError{Model: request.Model, Decision: decision, Status: settings.rejectStatus}
}

// guard 返回本次请求的降级判定（没有时为 nil；nil 的方法都是空操作）
func (diag *generationDiagnostics) guard() *downgradeGate {
	if diag == nil {
		return nil
	}
	return diag.downgrade
}

// attach 记下本次使用的账号与通道：Build 通道的 CountTokens 在同一账号上计数，判定依据里写明通道
func (gate *downgradeGate) attach(lease *aistudio.AccountLease, requestCtx context.Context) {
	if gate == nil || lease == nil {
		return
	}
	label := lease.Account().Config.Label
	gate.mu.Lock()
	gate.lease, gate.countCtx = lease, requestCtx
	gate.channel, gate.accountLabel = string(lease.Channel()), label
	gate.mu.Unlock()
}

// rejection 返回判定为降级时的拒绝错误（forwardEvents 结束时据此把请求记为失败）
func (gate *downgradeGate) rejection() error {
	if gate == nil {
		return nil
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.rejected == nil {
		return nil
	}
	return gate.rejected
}

func (gate *downgradeGate) setRejected(err *aistudio.ModelDowngradedError) {
	gate.mu.Lock()
	gate.rejected = err
	gate.mu.Unlock()
}

func (gate *downgradeGate) countTarget() (*aistudio.AccountLease, context.Context) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.lease, gate.countCtx
}

func (gate *downgradeGate) channelName() string {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.channel
}

func (gate *downgradeGate) label() string {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.accountLabel == "" {
		return "service"
	}
	return gate.accountLabel
}

func (gate *downgradeGate) channelTitle() string {
	switch gate.channelName() {
	case string(aistudio.ChannelPlayground):
		return "Playground"
	case string(aistudio.ChannelBuild):
		return "Build"
	default:
		return "未知通道"
	}
}

// tokenRatio 返回"字数 → token"的估算比例。upper 为真时比例只是上限（输入含附件，或没有上游输入数时按 1 token/字），
// 按它估算出的速度偏高，只能作为放行依据，不能单独作为拒绝依据
func (gate *downgradeGate) tokenRatio(promptTokens int64) (float64, bool) {
	if promptTokens > 0 && gate.inputChars > 0 {
		return float64(promptTokens) / float64(gate.inputChars), gate.hasMedia
	}
	return 1, true
}

func (gate *downgradeGate) record(decision aistudio.DowngradeDecision, final bool) {
	gate.service.recordDowngradeDecision(gate.clientCtx, gate.requestID, gate.label(), gate.model, decision, final)
}

// remember 记住被判定为降级的对话：同一段对话在保留期内再次请求、且这些消息原样都在时发送前直接拒绝。
// 只有一条消息的对话不记录：记录以"系统提示 + 第一条消息"识别，单条消息的记录会拒绝之后所有以同一句话开头的对话
// （例如角色卡固定的开场白），一次误判就会波及其他用户
func (gate *downgradeGate) remember() {
	if gate.settings.memory <= 0 || gate.memoryKey == "" || len(gate.contents) < downgradeMemoryMinMessages {
		return
	}
	if gate.clientCtx != nil && gate.clientCtx.Err() != nil {
		// 客户端已断开时的判定基于截断的数据，不作为记录依据
		return
	}
	conversationMemory.remember(gate.memoryKey, len(gate.contents),
		conversationHash(gate.model, gate.system, gate.contents), time.Now(), gate.settings.memory)
}

// start 启动判定 goroutine，返回交给协议层的事件流；流式严格模式还返回 wait：调用方在不持有锁时调用，
// 等到判定出结果（正常返回 nil；降级返回拒绝错误；判定前上游出错时返回该错误）
func (gate *downgradeGate) start(ctx context.Context, cancel context.CancelFunc, upstream <-chan aistudio.Event) (<-chan aistudio.Event, func() error) {
	gate.cancel = cancel
	out := make(chan aistudio.Event, 8)
	ready := make(chan error, 1)
	run := &downgradeRun{gate: gate, ctx: ctx, out: out, ready: ready, live: gate.mode == gateModeFast}
	go run.loop(upstream)
	if gate.mode != gateModeStrict {
		return out, nil
	}
	return out, func() error {
		select {
		case err := <-ready:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// downgradeEstimate 为按字数估算的一次判定快照（交给 CountTokens 精确计算时保留）
type downgradeEstimate struct {
	chars  int64
	tokens float64
	window time.Duration
	speed  float64
	ratio  float64
	upper  bool
}

type downgradeCountResult struct {
	tokens  int64
	err     error
	elapsed time.Duration
}

// downgradeTokenCounter 为在已持有的账户租约上计数的能力（PooledService.CountTokensForLease）
type downgradeTokenCounter interface {
	CountTokensForLease(context.Context, *aistudio.AccountLease, aistudio.TokenCountRequest) (aistudio.TokenCount, error)
}

// downgradeMeter 记录判定所需的计数，只在判定 goroutine 内使用
type downgradeMeter struct {
	firstEventAt time.Time
	firstTextAt  time.Time
	lastTextAt   time.Time
	// 上游累计正文数（精确）：起点为第一块带累计数的正文（同一批到达的并入起点）
	exactFirstAt time.Time
	exactBaseAt  time.Time
	exactBase    int64
	exactTokens  int64
	exactLastAt  time.Time
	// 按字数：起点之后的正文字数与正文（估算，或交给 CountTokens）
	charBaseAt   time.Time
	windowChars  int64
	totalChars   int64
	windowText   strings.Builder
	promptTokens int64
	servedModel  string
	finalOutput  int64
	// nonTextOutput 为回复包含正文以外的输出（函数调用、代码、媒体）：最终用量里的输出 token 包含这些部分，
	// 不能再除以只按正文计算的时间窗口
	nonTextOutput bool
}

func (meter *downgradeMeter) observe(event aistudio.Event, now time.Time, collectText bool) {
	if meter.firstEventAt.IsZero() {
		meter.firstEventAt = now
	}
	if event.ProviderModel != "" {
		meter.servedModel = event.ProviderModel
	}
	if meter.promptTokens == 0 && event.UpstreamPromptTokens > 0 {
		meter.promptTokens = event.UpstreamPromptTokens
	}
	if event.Kind == aistudio.EventUsage && event.Usage != nil {
		meter.finalOutput = event.Usage.OutputTokens
	}
	switch event.Kind {
	case aistudio.EventToolCall, aistudio.EventExecutableCode, aistudio.EventCodeExecutionResult, aistudio.EventMedia:
		meter.nonTextOutput = true
	}
	if event.Kind != aistudio.EventText || event.Text == "" {
		return
	}
	chars := int64(utf8.RuneCountInString(event.Text))
	meter.totalChars += chars
	meter.lastTextAt = now
	switch {
	case meter.firstTextAt.IsZero():
		meter.firstTextAt, meter.charBaseAt = now, now
	case now.Sub(meter.firstTextAt) <= downgradeBurstWindow:
		// 与第一块同一批到达：在到达前就已生成，并入起点
		meter.charBaseAt = now
	default:
		meter.windowChars += chars
		if collectText {
			meter.windowText.WriteString(event.Text)
		}
	}
	if cumulative := event.UpstreamOutputTokens; cumulative > 0 {
		if meter.exactFirstAt.IsZero() {
			meter.exactFirstAt = now
		}
		if now.Sub(meter.exactFirstAt) <= downgradeBurstWindow {
			meter.exactBaseAt, meter.exactBase = now, cumulative
		}
		if cumulative > meter.exactTokens {
			meter.exactTokens = cumulative
		}
		meter.exactLastAt = now
	}
}

// finalWindow 返回上游结束时第一块之后的正文 token 与时长：有累计数时用累计数，否则按最终用量与字数占比折算
func (meter *downgradeMeter) finalWindow() (int64, time.Duration, bool) {
	if !meter.exactBaseAt.IsZero() {
		total := meter.exactTokens
		if meter.finalOutput > total && !meter.nonTextOutput {
			total = meter.finalOutput
		}
		return total - meter.exactBase, meter.lastTextAt.Sub(meter.exactBaseAt), true
	}
	if !meter.charBaseAt.IsZero() && meter.finalOutput > 0 && meter.totalChars > 0 && !meter.nonTextOutput {
		tokens := int64(float64(meter.finalOutput) * float64(meter.windowChars) / float64(meter.totalChars))
		return tokens, meter.lastTextAt.Sub(meter.charBaseAt), true
	}
	return 0, 0, false
}

// downgradeRun 为一次请求判定期间的状态，只在判定 goroutine 内使用
type downgradeRun struct {
	gate     *downgradeGate
	ctx      context.Context
	out      chan<- aistudio.Event
	ready    chan<- error
	signaled bool
	gone     bool
	state    int
	buffer   []aistudio.Event
	// bufferAt 为缓存事件各自的到达时间（与 buffer 一一对应），用于最长延后时限
	bufferAt []time.Time
	tail     []aistudio.Event
	// live 为判定前就可以向客户端转发思考：快速模式一开始就是，严格模式到了最长延后时限之后才是
	live bool
	// capped 为严格模式到了最长延后时限，已先返回 200 并发出思考（此后判定为降级只能在流中报错）
	capped    bool
	holdTimer *time.Timer
	holdC     <-chan time.Time
	// releasedAt 为判定前第一次把内容发给客户端的时间
	releasedAt time.Time
	meter      downgradeMeter
	counting   chan downgradeCountResult
	snapshot   downgradeEstimate
	counted    bool
	exhausted  bool
	countNote  string
	decision   aistudio.DowngradeDecision
}

func (run *downgradeRun) loop(upstream <-chan aistudio.Event) {
	defer close(run.out)
	defer run.signal(nil)
	defer run.stopHold()
	for {
		select {
		case event, ok := <-upstream:
			if !ok {
				run.finish()
				return
			}
			run.handle(event)
		case result := <-run.counting:
			run.counting = nil
			run.countDone(result)
		case <-run.holdC:
			run.holdC = nil
			run.holdExpired()
		}
	}
}

// signal 通知严格模式下等待的 Generate：nil 表示放行（之后才向 out 写事件），否则为要返回的错误
func (run *downgradeRun) signal(err error) {
	if run.signaled {
		return
	}
	run.signaled = true
	run.ready <- err
}

func (run *downgradeRun) send(event aistudio.Event) {
	if run.gone {
		return
	}
	select {
	case run.out <- event:
	case <-run.ctx.Done():
		run.gone = true
	}
}

func (run *downgradeRun) handle(event aistudio.Event) {
	now := time.Now()
	run.meter.observe(event, now, run.state == runPending)
	switch run.state {
	case runStopped:
		return
	case runPassed:
		run.forward(event)
		return
	}
	if event.Kind == aistudio.EventError {
		run.upstreamFailed(event)
		return
	}
	if served := event.ProviderModel; served != "" && aistudio.ModelFamily(served) != run.gate.family {
		run.reject(run.modelDecision("model", served))
		return
	}
	run.hold(event, now)
	if event.Kind == aistudio.EventText {
		run.evaluate(now)
	}
}

// hold 判定前缓存事件；可以转发思考时（快速模式，或严格模式到了最长延后时限），缓存为空时思考与签名直接转发
func (run *downgradeRun) hold(event aistudio.Event, now time.Time) {
	if run.live && len(run.buffer) == 0 && downgradeThinkingEvent(event) {
		run.release(now)
		run.send(event)
		return
	}
	run.buffer = append(run.buffer, event)
	run.bufferAt = append(run.bufferAt, now)
	if len(run.buffer) == 1 {
		run.armHold()
	}
}

func downgradeThinkingEvent(event aistudio.Event) bool {
	return event.Kind == aistudio.EventReasoning || event.Kind == aistudio.EventThoughtSignature
}

// release 记下判定前第一次把内容发给客户端的时间（统计延后时长用）
func (run *downgradeRun) release(now time.Time) {
	if run.releasedAt.IsZero() {
		run.releasedAt = now
	}
}

// armHold 按缓存里最早事件的到达时间设定最长延后时限（只用于流式请求；0 表示不限）
func (run *downgradeRun) armHold() {
	run.stopHold()
	maxHold := run.gate.settings.maxHold
	if maxHold <= 0 || run.gate.mode == gateModeUnary || len(run.bufferAt) == 0 {
		return
	}
	wait := run.bufferAt[0].Add(maxHold).Sub(time.Now())
	if wait < 0 {
		wait = 0
	}
	run.holdTimer = time.NewTimer(wait)
	run.holdC = run.holdTimer.C
}

func (run *downgradeRun) stopHold() {
	if run.holdTimer != nil {
		run.holdTimer.Stop()
		run.holdTimer = nil
	}
	run.holdC = nil
}

// holdExpired 到了最长延后时限还没判定：严格模式先返回 200 并发出缓存开头的思考（正文继续缓存，之后判定为降级时在流中报错）；
// 正文缓存到时限仍未判定时放行，结束时照常复核并统计漏判
func (run *downgradeRun) holdExpired() {
	if run.state != runPending || len(run.buffer) == 0 {
		return
	}
	now := time.Now()
	if !run.live {
		run.live, run.capped = true, true
		run.signal(nil)
		run.flushThinking(now)
		if len(run.buffer) > 0 {
			run.armHold()
		}
		return
	}
	decision := run.newDecision("timeout")
	decision.Verdict = verdictUnjudged
	decision.Basis = fmt.Sprintf("%s · 正文已缓存 %.1f 秒（最长延后 %.1f 秒）仍未能判定，按时限放行",
		run.gate.channelTitle(), now.Sub(run.bufferAt[0]).Seconds(), run.gate.settings.maxHold.Seconds())
	if run.countNote != "" {
		decision.Basis += "（" + run.countNote + "）"
	}
	run.pass(decision, now)
}

// flushThinking 发出缓存开头的思考与思考签名，正文及之后的事件保持缓存
func (run *downgradeRun) flushThinking(now time.Time) {
	index := 0
	for index < len(run.buffer) && downgradeThinkingEvent(run.buffer[index]) {
		index++
	}
	if index == 0 {
		return
	}
	run.release(now)
	for _, event := range run.buffer[:index] {
		run.send(event)
	}
	run.buffer = append([]aistudio.Event(nil), run.buffer[index:]...)
	run.bufferAt = append([]time.Time(nil), run.bufferAt[index:]...)
}

// forward 判定正常之后转发事件；非流式请求扣住结束事件及之后的事件，等上游结束复核后再发
func (run *downgradeRun) forward(event aistudio.Event) {
	if run.gate.mode == gateModeUnary && (event.Kind == aistudio.EventFinish || len(run.tail) > 0) {
		run.tail = append(run.tail, event)
		return
	}
	run.send(event)
}

func (run *downgradeRun) flushTail() {
	tail := run.tail
	run.tail = nil
	for _, event := range tail {
		run.send(event)
	}
}

func (run *downgradeRun) newDecision(reason string) aistudio.DowngradeDecision {
	return aistudio.DowngradeDecision{Reason: reason, Mode: run.gate.mode, Channel: run.gate.channelName()}
}

func (run *downgradeRun) modelDecision(reason, served string) aistudio.DowngradeDecision {
	decision := run.newDecision(reason)
	decision.ServedModel = served
	decision.Basis = fmt.Sprintf("%s · 上游标明由 %s 生成（请求 %s）", run.gate.channelTitle(), served, run.gate.model)
	return decision
}

func (run *downgradeRun) estimateBasis(estimate downgradeEstimate) string {
	source := "按本次输入校准"
	if estimate.upper {
		source = "比例为上限估计"
	}
	return fmt.Sprintf("%s · 第一块之后正文 %d 字 ≈ %.0f token（%.2f token/字，%s）/ %.2f 秒 = %.0f tok/s",
		run.gate.channelTitle(), estimate.chars, estimate.tokens, estimate.ratio, source, estimate.window.Seconds(), estimate.speed)
}

// evaluate 每到一块正文判定一次：有上游累计正文数时按精确数，否则按字数估算（模糊区间内交给 CountTokens）
func (run *downgradeRun) evaluate(now time.Time) {
	settings, meter := run.gate.settings, &run.meter
	if !meter.exactBaseAt.IsZero() {
		tokens := meter.exactTokens - meter.exactBase
		window := meter.exactLastAt.Sub(meter.exactBaseAt)
		if tokens < settings.minTokens || window < settings.minWindow || window <= 0 {
			return
		}
		speed := float64(tokens) / window.Seconds()
		decision := run.newDecision("speed")
		decision.Speed, decision.Tokens, decision.WindowMS = speed, tokens, guardMS(window)
		decision.Basis = fmt.Sprintf("%s · 第一块之后正文 %d token / %.2f 秒 = %.0f tok/s %s 阈值 %.0f（上游累计正文数）",
			run.gate.channelTitle(), tokens, window.Seconds(), speed, guardCompare(speed, settings.threshold), settings.threshold)
		run.conclude(decision, speed >= settings.threshold, now)
		return
	}
	if run.counting != nil || run.exhausted || meter.windowChars == 0 {
		return
	}
	window := meter.lastTextAt.Sub(meter.charBaseAt)
	ratio, upper := run.gate.tokenRatio(meter.promptTokens)
	tokens := float64(meter.windowChars) * ratio
	if tokens < float64(settings.minTokens) || window < settings.minWindow || window <= 0 {
		return
	}
	estimate := downgradeEstimate{chars: meter.windowChars, tokens: tokens, window: window, speed: tokens / window.Seconds(), ratio: ratio, upper: upper}
	switch {
	case estimate.speed < settings.fuzzyLow:
		run.concludeEstimate(estimate, fmt.Sprintf("，低于模糊区间下限 %.0f", settings.fuzzyLow), false, now)
	case estimate.speed > settings.fuzzyHigh && !upper:
		// 估算比例受系统提示与回复语言差异影响很大（中文约 0.6 token/字，英文约 0.17），偏高的估算先用 CountTokens 确认
		if run.startCount(estimate) {
			return
		}
		run.concludeEstimate(estimate, fmt.Sprintf("，高于模糊区间上限 %.0f（没有调用 CountTokens）", settings.fuzzyHigh), true, now)
	default:
		if run.startCount(estimate) {
			return
		}
		if upper {
			// 不能精确计数、比例又只是上限：不凭偏高的估算拒绝，等上游结束后按标明的模型或整段速度判定
			run.exhausted = true
			run.countNote = "没有调用 CountTokens，估算比例只是上限"
			return
		}
		run.concludeEstimate(estimate, fmt.Sprintf("，落在模糊区间 %.0f～%.0f（没有调用 CountTokens），按估算 %s 阈值 %.0f",
			settings.fuzzyLow, settings.fuzzyHigh, guardCompare(estimate.speed, settings.threshold), settings.threshold),
			estimate.speed >= settings.threshold, now)
	}
}

func (run *downgradeRun) concludeEstimate(estimate downgradeEstimate, suffix string, downgraded bool, now time.Time) {
	decision := run.newDecision("speed")
	decision.Estimated = true
	decision.EstimatedSpeed, decision.Speed = estimate.speed, estimate.speed
	decision.Tokens, decision.WindowMS = int64(estimate.tokens+0.5), guardMS(estimate.window)
	decision.Basis = run.estimateBasis(estimate) + suffix
	run.conclude(decision, downgraded, now)
}

// startCount 在后台用同一账号对窗口内的正文调用 CountTokens（每个请求最多一次）；不能调用时返回 false
func (run *downgradeRun) startCount(estimate downgradeEstimate) bool {
	gate := run.gate
	if run.counted || gate.settings.countTimeout <= 0 {
		return false
	}
	counter, ok := gate.service.service.(downgradeTokenCounter)
	lease, countCtx := gate.countTarget()
	text := run.meter.windowText.String()
	if !ok || lease == nil || countCtx == nil || text == "" {
		return false
	}
	run.counted = true
	run.snapshot = estimate
	results := make(chan downgradeCountResult, 1)
	run.counting = results
	timeout, model := gate.settings.countTimeout, gate.model
	go func() {
		started := time.Now()
		ctx, cancel := context.WithTimeout(countCtx, timeout)
		defer cancel()
		count, err := counter.CountTokensForLease(ctx, lease, aistudio.TokenCountRequest{
			Model: model, Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: text}}}},
		})
		results <- downgradeCountResult{tokens: count.InputTokens, err: err, elapsed: time.Since(started)}
	}()
	return true
}

func (run *downgradeRun) countDone(result downgradeCountResult) {
	downgradeStats.countCall(result.elapsed, result.err)
	if run.state != runPending {
		return
	}
	now := time.Now()
	settings, estimate := run.gate.settings, run.snapshot
	if result.err == nil && result.tokens > 0 {
		downgradeStats.countSample(estimate.tokens, result.tokens)
		speed := float64(result.tokens) / estimate.window.Seconds()
		decision := run.newDecision("speed")
		decision.Estimated, decision.CountTokens = true, true
		decision.EstimatedSpeed, decision.Speed = estimate.speed, speed
		decision.Tokens, decision.WindowMS = result.tokens, guardMS(estimate.window)
		decision.CountTokensMS = guardMS(result.elapsed)
		decision.Basis = run.estimateBasis(estimate) + fmt.Sprintf(" → CountTokens 精确 %d token（用时 %d ms）= %.0f tok/s %s 阈值 %.0f",
			result.tokens, result.elapsed.Milliseconds(), speed, guardCompare(speed, settings.threshold), settings.threshold)
		run.conclude(decision, speed >= settings.threshold, now)
		return
	}
	reason := "返回 0"
	if result.err != nil {
		reason = guardShorten(result.err.Error(), 80)
	}
	if estimate.upper {
		// 比例只是上限（输入含附件或没有输入数）：偏高的估算不能单独作为拒绝依据，等上游结束后再判定
		run.exhausted = true
		run.countNote = "CountTokens 失败：" + reason
		return
	}
	decision := run.newDecision("speed")
	decision.Estimated, decision.CountTokens = true, true
	decision.EstimatedSpeed, decision.Speed = estimate.speed, estimate.speed
	decision.Tokens, decision.WindowMS = int64(estimate.tokens+0.5), guardMS(estimate.window)
	decision.CountTokensMS, decision.CountTokensError = guardMS(result.elapsed), reason
	decision.Basis = run.estimateBasis(estimate) + fmt.Sprintf(" → CountTokens 失败（%s），按估算 %s 阈值 %.0f",
		reason, guardCompare(estimate.speed, settings.threshold), settings.threshold)
	run.conclude(decision, estimate.speed >= settings.threshold, now)
}

func (run *downgradeRun) conclude(decision aistudio.DowngradeDecision, downgraded bool, now time.Time) {
	if downgraded {
		run.reject(decision)
		return
	}
	decision.Verdict = verdictPassed
	run.pass(decision, now)
}

// pass 放行：记录判定、通知等待的 Generate，再把缓存的事件按顺序发出
func (run *downgradeRun) pass(decision aistudio.DowngradeDecision, now time.Time) {
	gate := run.gate
	run.state = runPassed
	run.stopHold()
	if gate.mode != gateModeUnary {
		if !run.meter.firstEventAt.IsZero() {
			release := now
			if !run.releasedAt.IsZero() {
				release = run.releasedAt
			}
			decision.HeldMS = guardMS(release.Sub(run.meter.firstEventAt))
		}
		if !run.meter.firstTextAt.IsZero() {
			decision.TextHeldMS = guardMS(now.Sub(run.meter.firstTextAt))
		}
		decision.Capped = run.capped
		decision.Basis += fmt.Sprintf("；首个内容延后 %.1f 秒，正文延后 %.1f 秒", decision.HeldMS/1000, decision.TextHeldMS/1000)
		if run.capped {
			decision.Basis += "（到最长延后时限先发出了思考）"
		}
	}
	run.decision = decision
	// 非流式请求要等上游结束复核后才是最终结论，届时再计入统计
	gate.record(decision, gate.mode != gateModeUnary)
	run.signal(nil)
	buffered := run.buffer
	run.buffer, run.bufferAt = nil, nil
	for _, event := range buffered {
		run.forward(event)
	}
}

// reject 判定为降级：取消上游（不换号重试）、记住这段对话；严格模式让 Generate 返回拒绝错误，其余模式在流中发送错误事件
func (run *downgradeRun) reject(decision aistudio.DowngradeDecision) {
	gate := run.gate
	run.state = runStopped
	run.stopHold()
	decision.Verdict = verdictRejected
	decision.Capped = run.capped
	err := &aistudio.ModelDowngradedError{Model: gate.model, Decision: decision, Status: gate.settings.rejectStatus}
	gate.setRejected(err)
	if gate.cancel != nil {
		gate.cancel()
	}
	gate.record(decision, true)
	gate.remember()
	run.decision = decision
	run.buffer, run.bufferAt, run.tail = nil, nil, nil
	if gate.mode == gateModeStrict && !run.signaled {
		run.signal(err)
		return
	}
	run.signal(nil)
	run.send(aistudio.Event{Kind: aistudio.EventError, Err: err})
}

// upstreamFailed 判定前上游出错：丢弃尚未判定的缓存（可能是降级内容），只转发错误
func (run *downgradeRun) upstreamFailed(event aistudio.Event) {
	run.state = runStopped
	run.stopHold()
	run.buffer, run.bufferAt = nil, nil
	if event.Err == nil {
		event.Err = errors.New("AI Studio stream returned an empty error event")
	}
	if run.gate.mode == gateModeStrict && !run.signaled {
		// 严格模式此时还没有写出任何内容：直接作为 HTTP 错误返回
		run.signal(event.Err)
		return
	}
	run.send(event)
}

// finish 上游结束：还没判定的按结束时的数据判定；非流式已放行的再复核一次；流式已放行的只统计漏判
func (run *downgradeRun) finish() {
	gate := run.gate
	now := time.Now()
	if run.ctx.Err() != nil {
		// 客户端已断开（例如用户点了停止）：上游流是被取消的，数据是截断的，不判定也不记录对话
		if run.state == runPending {
			decision := run.newDecision("client_gone")
			decision.Verdict = verdictUnjudged
			decision.Basis = gate.channelTitle() + " · 客户端在判定前断开，回复被截断，不判定"
			run.pass(decision, now)
		}
		return
	}
	switch run.state {
	case runPending:
		decision, downgraded, judged := run.finalDecision()
		if judged {
			run.conclude(decision, downgraded, now)
		} else {
			decision.Verdict = verdictUnjudged
			run.pass(decision, now)
		}
		if run.state == runPassed && gate.mode == gateModeUnary {
			downgradeStats.add(run.decision)
			run.flushTail()
		}
	case runPassed:
		decision, downgraded, judged := run.finalDecision()
		switch {
		case gate.mode == gateModeUnary && judged && downgraded:
			run.reject(decision)
		case gate.mode == gateModeUnary:
			downgradeStats.add(run.decision)
			run.flushTail()
		case judged && downgraded:
			// 流式已经放行，只能统计漏判，供调整阈值参考
			downgradeStats.missedOne(decision.Basis)
			gate.service.requests.logRequestProgress(gate.requestID, gate.label(), "WARN", "降级判定漏判：放行后结束时复核为降级 | "+decision.Basis)
		}
	}
	run.sampleBuildEstimate()
}

// finalDecision 上游结束后的判定：Build 以最后一块标明的模型为准，其余按整段正文速度；数据不足时 judged 为 false
func (run *downgradeRun) finalDecision() (aistudio.DowngradeDecision, bool, bool) {
	gate, settings, meter := run.gate, run.gate.settings, &run.meter
	if served := meter.servedModel; served != "" && aistudio.ModelFamily(served) != gate.family {
		return run.modelDecision("final_model", served), true, true
	}
	if gate.channelName() == string(aistudio.ChannelBuild) && meter.servedModel != "" {
		decision := run.newDecision("final_model")
		decision.ServedModel = meter.servedModel
		decision.Basis = fmt.Sprintf("Build · 最后一块标明由 %s 生成，与请求一致", meter.servedModel)
		return decision, false, true
	}
	tokens, window, ok := meter.finalWindow()
	decision := run.newDecision("final_speed")
	minWindow := settings.minWindow
	if minWindow > downgradeFinalMinWindow {
		minWindow = downgradeFinalMinWindow
	}
	if !ok || tokens < settings.minTokens || window < minWindow || window <= 0 {
		decision.Basis = fmt.Sprintf("%s · 第一块之后正文不足 %d token 或不足 %.1f 秒，无法按速度判定，放行",
			gate.channelTitle(), settings.minTokens, minWindow.Seconds())
		if run.countNote != "" {
			decision.Basis += "（" + run.countNote + "）"
		}
		return decision, false, false
	}
	speed := float64(tokens) / window.Seconds()
	decision.Speed, decision.Tokens, decision.WindowMS = speed, tokens, guardMS(window)
	decision.Basis = fmt.Sprintf("%s · 结束时复核：第一块之后正文 %d token / %.2f 秒 = %.0f tok/s %s 阈值 %.0f",
		gate.channelTitle(), tokens, window.Seconds(), speed, guardCompare(speed, settings.threshold), settings.threshold)
	return decision, speed >= settings.threshold, true
}

// sampleBuildEstimate 统计 Build 通道按比例估算的正文 token 与最终真实数的偏差（判断估算是否可靠）
func (run *downgradeRun) sampleBuildEstimate() {
	gate, meter := run.gate, &run.meter
	if gate.channelName() != string(aistudio.ChannelBuild) || gate.hasMedia || meter.promptTokens <= 0 || gate.inputChars <= 0 ||
		meter.finalOutput <= 0 || meter.totalChars <= 0 {
		return
	}
	ratio := float64(meter.promptTokens) / float64(gate.inputChars)
	downgradeStats.buildSample(float64(meter.totalChars)*ratio, meter.finalOutput)
}

// recordDowngradeDecision 把判定依据写进管理日志、排查记录与请求时间线；final 为该请求的最终结论时计入统计，
// 拒绝时另写一条运行日志
func (service *trackedService) recordDowngradeDecision(ctx context.Context, requestID, accountLabel, model string, decision aistudio.DowngradeDecision, final bool) {
	if accountLabel == "" {
		accountLabel = "service"
	}
	api.SetAccessLogDowngrade(ctx, decision)
	title := downgradeVerdictTitle(decision.Verdict)
	aistudio.TraceFromContext(ctx).Note(title + "：" + decision.Basis)
	level := "INFO"
	if decision.Verdict == verdictRejected {
		level = "WARN"
	}
	service.requests.logRequestProgress(requestID, accountLabel, level, title+" | "+decision.Basis)
	if !final {
		return
	}
	total := downgradeStats.add(decision)
	if decision.Verdict == verdictRejected {
		service.requests.log(accountLabel, "WARN", fmt.Sprintf("%s | 模型=%s | %s | 启动以来第 %d 次", title, model, decision.Basis, total))
	}
}

func downgradeVerdictTitle(verdict string) string {
	switch verdict {
	case verdictRejected:
		return "因降级拒绝"
	case verdictPassed:
		return "降级判定：正常"
	default:
		return "降级判定：数据不足，放行"
	}
}

// playgroundUnavailable 判断 Playground 优先选号失败是不是因为没有可用的 Playground 账号（不支持该模型或全部冷却）
func playgroundUnavailable(err error) bool {
	var cooling *aistudio.AllCoolingError
	return errors.Is(err, aistudio.ErrNoEligibleAccount) || errors.As(err, &cooling)
}

// downgradeCounters 为启动以来的降级判定统计（./start.sh diag 与 /admin/api/perf 显示）
type downgradeCounters struct {
	mu                 sync.Mutex
	judged             int64
	passed             int64
	unjudged           int64
	rejected           int64
	reasons            map[string]int64
	missed             int64
	capped             int64
	lastMissed         string
	last               string
	heldCount          int64
	heldTotal          float64
	textHeldTotal      float64
	countCalls         int64
	countFailures      int64
	countTotal         float64
	countSamples       int64
	countErrorTotal    float64
	countAbsErrorTotal float64
	buildSamples       int64
	buildErrorTotal    float64
	buildAbsErrorTotal float64
}

var downgradeStats = &downgradeCounters{reasons: make(map[string]int64)}

// add 计入一个请求的最终结论，返回启动以来因降级拒绝的次数
func (stats *downgradeCounters) add(decision aistudio.DowngradeDecision) int64 {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.judged++
	switch decision.Verdict {
	case verdictRejected:
		stats.rejected++
		stats.reasons[decision.Reason]++
		stats.last = time.Now().Format("01-02 15:04:05") + " " + decision.Basis
	case verdictPassed:
		stats.passed++
	default:
		stats.unjudged++
	}
	if decision.Capped {
		stats.capped++
	}
	if decision.Verdict != verdictRejected && (decision.Mode == gateModeStrict || decision.Mode == gateModeFast) {
		stats.heldCount++
		stats.heldTotal += decision.HeldMS
		stats.textHeldTotal += decision.TextHeldMS
	}
	return stats.rejected
}

func (stats *downgradeCounters) countCall(elapsed time.Duration, err error) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.countCalls++
	stats.countTotal += guardMS(elapsed)
	if err != nil {
		stats.countFailures++
	}
}

// countSample 记录估算 token 与 CountTokens 精确值的偏差
func (stats *downgradeCounters) countSample(estimated float64, exact int64) {
	if exact <= 0 {
		return
	}
	deviation := (estimated - float64(exact)) / float64(exact)
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.countSamples++
	stats.countErrorTotal += deviation
	if deviation < 0 {
		deviation = -deviation
	}
	stats.countAbsErrorTotal += deviation
}

// buildSample 记录 Build 通道整段正文的估算 token 与上游最终用量的偏差
func (stats *downgradeCounters) buildSample(estimated float64, actual int64) {
	if actual <= 0 {
		return
	}
	deviation := (estimated - float64(actual)) / float64(actual)
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.buildSamples++
	stats.buildErrorTotal += deviation
	if deviation < 0 {
		deviation = -deviation
	}
	stats.buildAbsErrorTotal += deviation
}

func (stats *downgradeCounters) missedOne(basis string) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.missed++
	stats.lastMissed = time.Now().Format("01-02 15:04:05") + " " + basis
}

// downgradeGuardStats 返回降级判定统计（性能统计接口的 downgrade_guard 字段）
func downgradeGuardStats() map[string]any {
	stats := downgradeStats
	stats.mu.Lock()
	defer stats.mu.Unlock()
	reasons := make(map[string]int64, len(stats.reasons))
	for key, value := range stats.reasons {
		reasons[key] = value
	}
	result := map[string]any{
		"judged": stats.judged, "passed": stats.passed, "unjudged": stats.unjudged, "rejected": stats.rejected,
		"rejected_by": reasons, "missed": stats.missed, "last_missed": stats.lastMissed, "last_rejected": stats.last,
		"held_samples": stats.heldCount, "capped": stats.capped, "count_tokens_calls": stats.countCalls, "count_tokens_failures": stats.countFailures,
		"memory_entries": conversationMemory.size(),
	}
	if stats.heldCount > 0 {
		result["avg_held_ms"] = stats.heldTotal / float64(stats.heldCount)
		result["avg_text_held_ms"] = stats.textHeldTotal / float64(stats.heldCount)
	}
	if stats.countCalls > 0 {
		result["count_tokens_avg_ms"] = stats.countTotal / float64(stats.countCalls)
	}
	if stats.countSamples > 0 {
		result["count_estimate_samples"] = stats.countSamples
		result["count_estimate_error_pct"] = stats.countErrorTotal * 100 / float64(stats.countSamples)
		result["count_estimate_abs_error_pct"] = stats.countAbsErrorTotal * 100 / float64(stats.countSamples)
	}
	if stats.buildSamples > 0 {
		result["build_estimate_samples"] = stats.buildSamples
		result["build_estimate_error_pct"] = stats.buildErrorTotal * 100 / float64(stats.buildSamples)
		result["build_estimate_abs_error_pct"] = stats.buildAbsErrorTotal * 100 / float64(stats.buildSamples)
	}
	return result
}

// downgradeMemory 记录最近被判定为降级的对话。键为"模型 + 系统提示 + 第一条消息"的哈希；每条记录还保存被拒绝时
// 已有的消息条数与这些消息的哈希。同一段对话再次请求时，这些消息原样都在（重新生成、接着往下聊）就直接拒绝；
// 其中任何一条被改过（用户按提示修改了内容）就放行重新判定。只保存哈希，不保存内容
type downgradeMemory struct {
	mu      sync.Mutex
	entries map[string][]downgradeMemoryEntry
	total   int
}

type downgradeMemoryEntry struct {
	messages int
	hash     string
	at       time.Time
}

var conversationMemory = &downgradeMemory{entries: make(map[string][]downgradeMemoryEntry)}

// lookup 查找保留期内的记录，返回距被拒绝的时长与当时的消息条数
func (memory *downgradeMemory) lookup(key, model, system string, contents []aistudio.Content, now time.Time, retention time.Duration) (time.Duration, int, bool) {
	memory.mu.Lock()
	candidates := memory.liveLocked(key, now, retention)
	memory.mu.Unlock()
	hashes := make(map[int]string, len(candidates))
	for index := len(candidates) - 1; index >= 0; index-- {
		entry := candidates[index]
		if entry.messages <= 0 || entry.messages > len(contents) {
			continue
		}
		digest, ok := hashes[entry.messages]
		if !ok {
			digest = conversationHash(model, system, contents[:entry.messages])
			hashes[entry.messages] = digest
		}
		if digest == entry.hash {
			return now.Sub(entry.at), entry.messages, true
		}
	}
	return 0, 0, false
}

// liveLocked 清掉 key 下过期的记录（保留期按当前设置算，改短立即生效），返回仍有效记录的副本
func (memory *downgradeMemory) liveLocked(key string, now time.Time, retention time.Duration) []downgradeMemoryEntry {
	entries := memory.entries[key]
	live := make([]downgradeMemoryEntry, 0, len(entries))
	for _, entry := range entries {
		if now.Sub(entry.at) < retention {
			live = append(live, entry)
		}
	}
	memory.total -= len(entries) - len(live)
	if len(live) == 0 {
		delete(memory.entries, key)
		return nil
	}
	memory.entries[key] = live
	return append([]downgradeMemoryEntry(nil), live...)
}

func (memory *downgradeMemory) remember(key string, messages int, digest string, now time.Time, retention time.Duration) {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	live := memory.liveLocked(key, now, retention)
	kept := make([]downgradeMemoryEntry, 0, len(live)+1)
	for _, entry := range live {
		if entry.messages != messages || entry.hash != digest {
			kept = append(kept, entry)
		}
	}
	kept = append(kept, downgradeMemoryEntry{messages: messages, hash: digest, at: now})
	if len(kept) > downgradeMemoryPerKey {
		kept = kept[len(kept)-downgradeMemoryPerKey:]
	}
	memory.total += len(kept) - len(live)
	memory.entries[key] = kept
	if memory.total > downgradeMemoryLimit {
		memory.pruneLocked(now, retention)
	}
}

// pruneLocked 记录过多时先清掉全部过期记录，仍超出上限就丢掉最早的
func (memory *downgradeMemory) pruneLocked(now time.Time, retention time.Duration) {
	for key := range memory.entries {
		memory.liveLocked(key, now, retention)
	}
	for memory.total > downgradeMemoryLimit {
		oldestKey := ""
		var oldest time.Time
		for key, entries := range memory.entries {
			if len(entries) > 0 && (oldestKey == "" || entries[0].at.Before(oldest)) {
				oldestKey, oldest = key, entries[0].at
			}
		}
		if oldestKey == "" {
			return
		}
		memory.total -= len(memory.entries[oldestKey])
		delete(memory.entries, oldestKey)
	}
}

func (memory *downgradeMemory) size() int {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	return memory.total
}

// conversationHash 计算模型、系统提示与给定消息的哈希：只看角色、文字、思考标记、附件与工具调用内容，不看思考签名
// （签名缓存命中与否会让同一段历史的签名不同）；随机后缀加在副本上，这里拿到的是客户端原始内容
func conversationHash(model, system string, contents []aistudio.Content) string {
	digest := sha256.New()
	writeGuardField(digest, model)
	writeGuardField(digest, system)
	for _, content := range contents {
		writeGuardField(digest, "role:"+string(content.Role))
		for _, part := range content.Parts {
			writeGuardField(digest, part.Text)
			if part.Thought {
				writeGuardField(digest, "thought")
			}
			if part.FunctionCall != nil {
				writeGuardField(digest, "call:"+part.FunctionCall.Name)
				writeGuardField(digest, string(part.FunctionCall.Arguments))
			}
			for _, extra := range []any{part.InlineData, part.File, part.ExternalMedia, part.FunctionResult, part.ExecutableCode, part.CodeExecutionResult} {
				if data, err := json.Marshal(extra); err == nil {
					writeGuardField(digest, string(data))
				}
			}
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeGuardField(digest hash.Hash, value string) {
	digest.Write([]byte(strconv.Itoa(len(value)) + ":"))
	digest.Write([]byte(value))
}

// guardInputSize 统计本次输入的字数（系统提示、文字、工具调用与结果、函数声明）及是否含附件，用于校准"字数 → token"的比例
func guardInputSize(system string, contents []aistudio.Content, tools aistudio.Tools) (int64, bool) {
	chars := int64(utf8.RuneCountInString(system))
	media := false
	for _, content := range contents {
		for _, part := range content.Parts {
			chars += int64(utf8.RuneCountInString(part.Text))
			if part.InlineData != nil || part.File != nil || part.ExternalMedia != nil {
				media = true
			}
			if part.FunctionCall != nil {
				chars += int64(len(part.FunctionCall.Name) + len(part.FunctionCall.Arguments))
			}
			if part.FunctionResult != nil {
				if data, err := json.Marshal(part.FunctionResult); err == nil {
					chars += int64(len(data))
				}
			}
		}
	}
	if len(tools.Functions) > 0 {
		if data, err := json.Marshal(tools.Functions); err == nil {
			chars += int64(len(data))
		}
	}
	return chars, media
}

func guardMS(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func guardCompare(value, threshold float64) string {
	if value >= threshold {
		return "≥"
	}
	return "<"
}

func guardDuration(duration time.Duration) string {
	if duration < time.Minute {
		return fmt.Sprintf("%d 秒", int(duration.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(duration.Minutes()))
}

func guardShorten(text string, limit int) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}
