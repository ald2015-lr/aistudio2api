package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 不可调度的驻留 Worker：账号需要登录、已停用、不可用，或目录加载过却没有可初始化的模型时，它的 Worker 不是任何模型的候选，
// 却一直占着驻留槽位（既不算死槽位也不算冷却中，淘汰与按需替换都换不掉）。线上第二份诊断：150 个驻留里 138–140 个不是任何模型的
// 候选，生成 401 把账号标成需要登录却不关它的 Worker。这里登记释放（与冷却淘汰共用登记表与淘汰循环，见 worker_retire.go）：
//   - 生成、流内、续签重放、启动阶段确认需要登录后立即登记（releaseUnschedulable）；
//   - keepWarm 每 5 秒对驻留 Worker 补登记其他路径造成的不可调度（registerUnschedulableWorkers）：aistudio 内部的 CountTokens、
//     上传、视频的 401，管理页停用，认证文件变化，目录被同步成空等；
//   - 淘汰循环对它空闲即关，忙碌等请求结束；不排空、不要求可接替账户、不走排空超时，关闭后只补齐预热，不按冷却模型挑替补。
// 淘汰选号（pickIdleVictimFor）把它排在死槽位之前，按需替换、现场回收、空闲回收都先关它。
// “账号需要登录”日志由 noteAuthRequired 统一写（持久行，同账号 1 分钟最多一条），是第 5 项（需要登录误判）的诊断依据

// 不可调度的原因（与 aistudio.WarmWorkerState.Unschedulable 一致），写进“WAA Worker 释放 | 原因=”
const (
	unschedulableAuthRequired = aistudio.UnschedulableAuthRequired
	unschedulableDisabled     = aistudio.UnschedulableDisabled
	unschedulableUnavailable  = aistudio.UnschedulableUnavailable
	unschedulableNoCatalog    = aistudio.UnschedulableNoCatalog
	unschedulableOther        = aistudio.UnschedulableNotReady
	// unschedulableOrgDisabled 为账号所属组织停用了 AI Studio（见 org_disabled.go）
	unschedulableOrgDisabled = aistudio.UnschedulableOrgDisabled
)

// unschedulableReasonOrder 为补登记日志里各原因的顺序
var unschedulableReasonOrder = []string{
	unschedulableAuthRequired, unschedulableOrgDisabled, unschedulableDisabled, unschedulableUnavailable, unschedulableNoCatalog,
	unschedulableOther,
}

const (
	// authRequiredNoteInterval 为同一账号两条“账号需要登录”日志的最短间隔：线上约 4.8 个账号/分钟被标需要登录，
	// 同一请求的续签重放、生成与流内路径可能先后确认同一次失效，1 分钟一条不会挤满 256 条的持久队列
	authRequiredNoteInterval = time.Minute
	// authRequiredNotePrune 为去重记录达到这么多条时清掉过期记录
	authRequiredNotePrune = 256
	// authRequiredCauseLimit 为日志里错误摘要的最大字数
	authRequiredCauseLimit = 200
)

// authRequiredSource 为账号被标为需要登录的来源，写进“账号需要登录 | 来源=”
type authRequiredSource uint8

const (
	authFromGeneration authRequiredSource = iota + 1 // 生成请求首事件前的 401
	authFromStream                                   // 流式输出中的 401
	authFromReplay                                   // 经认证续签传输确认（续签后重放仍 401，或没有续签材料、续签失败）
	authFromStartup                                  // Worker 启动阶段确认登录失效
	authFromCatalog                                  // 模型目录同步的 401
	authFromVerify                                   // 管理页验证判定登录已失效
	authFromOther                                    // 其他请求（CountTokens、文件、视频、Live 等）的 401，经登录复核确认
)

// String 返回来源在日志中的写法
func (source authRequiredSource) String() string {
	switch source {
	case authFromGeneration:
		return "生成"
	case authFromStream:
		return "流内"
	case authFromReplay:
		return "续签重放"
	case authFromStartup:
		return "启动"
	case authFromCatalog:
		return "目录同步"
	case authFromVerify:
		return "管理验证"
	case authFromOther:
		return "其他请求"
	default:
		return "未知"
	}
}

type authRequiredSourceKey struct{}

// withAuthRequiredSource 标记 ctx 内经认证续签传输（authRuntimeRefresher）确认的登录失效来自哪条路径：
// Worker 启动阶段的恢复与重启经同一个续签器，没有标记时都会记成续签重放
func withAuthRequiredSource(ctx context.Context, source authRequiredSource) context.Context {
	return context.WithValue(ctx, authRequiredSourceKey{}, source)
}

// authRequiredSourceFrom 返回 withAuthRequiredSource 标记的来源，没有标记时为续签重放
func authRequiredSourceFrom(ctx context.Context) authRequiredSource {
	if ctx != nil {
		if source, ok := ctx.Value(authRequiredSourceKey{}).(authRequiredSource); ok && source != 0 {
			return source
		}
	}
	return authFromReplay
}

// authRequiredHook 为 Worker 管理器在账户确认需要登录后调用的生成服务回调（写“账号需要登录”，见 noteAuthRequired）
type authRequiredHook func(accountID, label string, source authRequiredSource, cause error)

// authRequiredNotes 为“账号需要登录”日志的去重状态：每个账号最近一次写日志的时间；零值可用
type authRequiredNotes struct {
	mu     sync.Mutex
	logged map[string]time.Time
}

// allow 判断账号现在能否写一条，能写时记下时间：距上一次不足 authRequiredNoteInterval 时不能写。
// 记录达到 authRequiredNotePrune 条时先清掉过期的，记录数不超过最近 1 分钟被标需要登录的账号数加这个余量
func (notes *authRequiredNotes) allow(accountID string, now time.Time) bool {
	notes.mu.Lock()
	defer notes.mu.Unlock()
	if last, seen := notes.logged[accountID]; seen && now.Sub(last) < authRequiredNoteInterval {
		return false
	}
	if notes.logged == nil {
		notes.logged = make(map[string]time.Time)
	}
	if len(notes.logged) >= authRequiredNotePrune {
		for other, at := range notes.logged {
			if now.Sub(at) >= authRequiredNoteInterval {
				delete(notes.logged, other)
			}
		}
	}
	notes.logged[accountID] = now
	return true
}

// releaseUnschedulable 登记释放账号的驻留 Worker（reason 为调用方确认的不可调度原因）：空闲即关，忙碌等它的请求释放租约后关闭。
// 生成路径确认需要登录后立即调用；manager 为 nil 或账号没有驻留 Worker 时不做任何事。
// 登记以账户池当前的原因为准：登录在这之前已被替换（认证代际变化）时需要登录的标记不会写入，账号仍可调度，不登记
func (manager *accountWorkerManager) releaseUnschedulable(accountID string, reason string) {
	if manager == nil || manager.pool == nil {
		return
	}
	current := manager.pool.UnschedulableSet([]string{accountID})[accountID]
	if current == "" {
		return
	}
	manager.registerUnschedulable(accountID, current)
}

// confirmAuthRequired 在账户确认需要登录、标记已写入后调用：登记释放它的驻留 Worker，并经生成服务写“账号需要登录”
// （服务没有挂上回调时只登记释放）。认证续签器与 Worker 启动路径用它，manager 为 nil 时不做任何事
func (manager *accountWorkerManager) confirmAuthRequired(accountID, label string, source authRequiredSource, cause error) {
	manager.confirmAuthRequiredWith(accountID, label, source, cause, "")
}

// confirmAuthRequiredWith 与 confirmAuthRequired 相同，extra 非空时追加在“账号需要登录”日志末尾（如登录复核的“复核=仍失效”）
func (manager *accountWorkerManager) confirmAuthRequiredWith(accountID, label string, source authRequiredSource, cause error, extra string) {
	if manager == nil {
		return
	}
	manager.releaseUnschedulable(accountID, unschedulableAuthRequired)
	if note := manager.authRequired.Load(); note != nil {
		(*note)(accountID, label, source, withAuthNoteExtra(cause, extra))
	}
}

// authNoteExtraError 携带“账号需要登录”日志末尾的附加字段（经 authRequiredHook 传给 noteAuthRequired，不改回调签名）
type authNoteExtraError struct {
	err   error
	extra string
}

func (err *authNoteExtraError) Error() string { return err.err.Error() }
func (err *authNoteExtraError) Unwrap() error { return err.err }

// withAuthNoteExtra 给错误附上日志附加字段；extra 为空或没有错误时原样返回
func withAuthNoteExtra(cause error, extra string) error {
	if cause == nil || strings.TrimSpace(extra) == "" {
		return cause
	}
	return &authNoteExtraError{err: cause, extra: extra}
}

// registerUnschedulableWorkers 为 keepWarm 每个周期的补登记：驻留 Worker 中已不可调度、还没有登记释放的账号
// （aistudio 内部的 401、管理页停用、认证文件变化、启动阶段登录失效、目录被同步成空等），返回新增登记数。
// 驻留 Worker 不超过峰值数（线上 150），账户池只加锁一次
func (service *trackedService) registerUnschedulableWorkers() int {
	workers := service.workers
	if workers == nil || service.pool == nil {
		return 0
	}
	warm := workers.WarmAccountIDs()
	if len(warm) == 0 {
		return 0
	}
	found := service.pool.UnschedulableSet(warm)
	if len(found) == 0 {
		return 0
	}
	accountIDs := make([]string, 0, len(found))
	for accountID := range found {
		accountIDs = append(accountIDs, accountID)
	}
	slices.Sort(accountIDs)
	added := 0
	reasons := make(map[string]int, len(unschedulableReasonOrder))
	for _, accountID := range accountIDs {
		reasons[found[accountID]]++
		if _, ok := workers.registerUnschedulable(accountID, found[accountID]); ok {
			added++
		}
	}
	if added > 0 {
		// 持久行：控制台队列满时也不丢，诊断脚本按“新增=”求和
		service.requests.logDurable("service", "INFO", fmt.Sprintf(
			"WAA Worker 补登记释放 | 新增=%d | 驻留中不可调度=%d/%d | 原因=%s",
			added, len(found), len(warm), unschedulableReasonsText(reasons),
		))
	}
	return added
}

// unschedulableReasonsText 把各原因的账号数写成“需要登录 3、已停用 1”（只写非零的，按固定顺序）
func unschedulableReasonsText(reasons map[string]int) string {
	parts := make([]string, 0, len(reasons))
	for _, reason := range unschedulableReasonOrder {
		if count := reasons[reason]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", reason, count))
		}
	}
	for reason, count := range reasons {
		if count > 0 && !slices.Contains(unschedulableReasonOrder, reason) {
			parts = append(parts, fmt.Sprintf("%s %d", reason, count))
		}
	}
	return strings.Join(parts, "、")
}

// noteAuthRequired 统一写“账号需要登录”日志（持久行，同账号 1 分钟最多一条），把需要登录的时间记进模型目录缓存
// （catalogSyncer.noteAuthRequired，供疑似失效账号的预热排序与探测顺序，并移出疑似失效），并给登录复核记下证据来源。只在账号此刻确实需要登录（停用账号的认证结果写成停用，同样算）
// 时写：登录在这之前已被替换、标记没有写入时不写。带账号的出口与出口是否判定限流，第 5 项（需要登录误判）按 来源 × 出口限流中 统计
func (service *trackedService) noteAuthRequired(accountID, label string, source authRequiredSource, cause error) {
	if service == nil || service.pool == nil || service.requests == nil {
		return
	}
	switch service.pool.UnschedulableSet([]string{accountID})[accountID] {
	case unschedulableAuthRequired, unschedulableDisabled:
	default:
		return
	}
	now := time.Now()
	// 证据来源（登录复核只复核接口证据，见 auth_recheck.go）与目录缓存的需要登录记录每次都记，不受下面日志去重的限制。
	// 管理验证另按浏览器结论重记（见 verifyLogin）
	service.authRecheck.noteMarked(accountID, source == authFromStartup)
	service.catalogSync.noteAuthRequired(accountID, now)
	if !service.authNotes.allow(accountID, now) {
		return
	}
	// 状态接口里“最近 10 分钟新增需要登录”按这条日志计数（同账号 1 分钟最多一次）
	service.authMarks.add(now)
	extra := ""
	var noted *authNoteExtraError
	if errors.As(cause, &noted) {
		extra, cause = " | "+noted.extra, noted.err
	}
	key, lane := service.pool.EgressKeyOf(accountID)
	limited := "否"
	if service.egress.verdict(key, lane == aistudio.EgressLaneUltra, now).Limited {
		limited = "是"
	}
	if strings.TrimSpace(label) == "" {
		label = accountID
	}
	service.requests.logDurable(label, "WARN", fmt.Sprintf(
		"账号需要登录 | 来源=%s | 出口=%s | 出口限流中=%s | 错误=%s%s",
		source, aistudio.EgressLabel(key), limited, authRequiredCauseSummary(cause), extra,
	))
}

// authRequiredCauseSummary 把错误压成一行（连续空白合成一个空格），超过 authRequiredCauseLimit 字时截断；没有错误时为 -
func authRequiredCauseSummary(cause error) string {
	if cause == nil {
		return "-"
	}
	text := strings.Join(strings.Fields(cause.Error()), " ")
	if runes := []rune(text); len(runes) > authRequiredCauseLimit {
		text = string(runes[:authRequiredCauseLimit]) + "…"
	}
	if text == "" {
		return "-"
	}
	return text
}
