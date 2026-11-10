package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 额度用完自动改用其他模型（MODEL_FALLBACKS、MODEL_FALLBACK_SCOPE，保存后立即生效，写法见 config/model_fallback.go）
//
// 放在哪一层：generateWithRetry 外层（按一个模型换号重试的 generateForModel 以错误结束、还没有交出任何事件时，见 continueWithModelFallback）。
// 每日限额按模型分开计，“这个模型在号池里没有可用额度”只有走完该模型的选号、排队、冷却与换号才知道；按目标模型重新调用 generateForModel，
// 主用模型的排队上限与额度快速失败、换号上限、冷却与额度失败计数都按目标模型自己的设置从头进行，与客户端直接请求目标模型相同。
// 这些错误都发生在首个事件之前（事件只经 forwardEvents 交出，以错误结束时它没有开始），流式请求此前最多写过响应头与数据型心跳。
//
// 触发（同时满足）：
//   - MODEL_FALLBACK_SCOPE 为 all，或为 ultra 且请求经 /ultra 进入（PoolScopeUltra）；off 时所有请求与没有这项设置时相同
//   - 请求的模型（startGenerate 已解析成目录模型 ID：后缀与 long- 前缀在协议层去掉，目录别名按号池解析）有规则；规则的源模型与目标也接受目录别名
//   - 以额度类错误结束（modelQuotaEnd）：候选全部冷却（AllCoolingError，含 Ultra 主用模型的立即 429）、额度不足（PrimaryQuotaError，
//     出口限流除外）、换号到最后仍是每日或分钟额度 429（按模型的，不是整个账号的全局额度）、租约账号发出前已冷却又不能再换号（ErrAccountCoolingDown）
//   - 不固定账户、没有文件绑定（资源属于某个账户）、不在降级判定里（DOWNGRADE_GUARD_MODELS：事件流已交给按源模型建立的判定）、
//     是文本对话生成（图片、语音、转录、Interaction 即 Omni 等不触发）
//
// 不触发：参数错误、内容拦截、上游 5xx、超时、取消、模型不在目录（404/503 没有候选）、带图 403 等非额度原因；号池繁忙排队超限
// （CapacityBusyError：账号有额度只是都在忙，改用其他模型落在同一批账号与 Worker 上，只会多排一轮）；出口限流（同一出口上换模型照样被限）；
// 整个账号的全局额度（换模型也落在同一批账号上）。Live、Veo 视频、图片与语音生成不经这条路径（各自的入口或非文本请求），countTokens、
// 单模型查询与 /v1/models 不受影响。
//
// 改用目标：按规则顺序逐个检查，目标不在这个号池的模型目录里、与源模型相同、接不了这次请求（先按目标模型整理一份请求副本，再按
// aistudio.FallbackTargetUnsupported 检查方法、文本对话路由、Interaction 与工具：-online 让协议层追加的 google_search 在整理时按目标的
// 能力重新决定，不算客户端要求的工具）、在降级判定里（DOWNGRADE_GUARD_MODELS：判定在 startGenerate 按源模型建立，改用后没有判定，
// 直接请求目标会被判定或拦截）时跳过并记一行原因；目标不递归套用它自己的规则（不会成环）。按目标模型重新整理：思考等级与预算、
// -online 联网搜索、long- 续写与原生续写名单（api.RetargetGenerateRequest）、最大输出下限（MIN_OUTPUT_TOKENS）与 long- 默认总上限
// （prepareLongOutput）；输出上限、long- 续写方式（能力码 91）与工具校验在发送时按目标模型的目录进行。
//
// 思考签名（签名是签发模型在那一轮的加密思考上下文，另一个模型可能以 MISSING/INVALID thought signature 400 拒绝）：
//   - 改用时：历史里不是目标模型在改用期间签发的签名按 aistudio.ReplaceThoughtSignatures 处理（函数调用换成跳过校验的占位值，只有签名的
//     部分去掉，其余部分去掉签名）；目标模型在之前改用时签发的签名（fallbackSignatures）原样保留，签名缺失或为占位值的函数调用再按目标模型
//     的作用域按调用 ID 还原（协议层只按请求的模型还原过，new-api 丢掉签名、写占位值时靠这一步找回目标模型自己的签名）
//   - 改用之后：forwardEvents 把目标模型交出的签名记进 fallbackSignatures（只存摘要）；之后的请求发给别的模型（例如源模型恢复后）时，
//     这些签名同样换成占位值或去掉（generateForModel 开头），不把目标模型的签名发给源模型。只记改用期间签发的签名，没有发生过改用时
//     请求原样发送（与没有这项设置时相同）；范围改为 off 或清空规则之后，之前改用期间登记的签名照样换掉（它们不是源模型签发的，
//     原样发过去可能 400，换掉只少了那几步的思考上下文）。记录在进程内、按容量淘汰：重启或淘汰后认不出的签名原样发给别的模型，可能以思考签名无效 400 结束，
//     这时重开对话或让客户端去掉历史里的思考块（README 写明）
//
// 对外与记账：请求日志写一行 WARN（modelFallbackSwitchNote）；响应报告目标模型（aistudio.ModelSwitch，见 api.servedModel）；请求日志与
// 用量账本的模型记目标模型，请求日志另记 fallback_from，账本的上游尝试里多一条改用备注（账户为空），源模型与目标模型的上游尝试各带模型
// （RequestAttempt.Model）；活动请求列表的模型同样改为目标模型。
// 流式请求改用时 API 层写出响应头前的等待从改用时刻起按目标模型重新计（aistudio.StreamStartHold.RestartWait）：目标模型首事件前的错误
// 照常以 HTTP 状态返回，响应开头报告目标模型；改用前响应头已经提交（源模型很晚才额度用完）时，已写出的块保留源模型，之后的块按目标模型。
// 所有目标都额度用完或不可用时按源模型原来的错误返回（状态码、对外文案与原因响应头不变），Retry-After 取源模型与各目标的建议重试中
// 最早的时刻：额度用完的目标为它的恢复时间，目标只是暂时不可用（号池繁忙、出口限流等）时为它自己建议的重试时间（无论哪个先恢复，
// 下一次请求都能用上：源模型恢复就直接用，目标先恢复就再改用过去），日志写明试过哪些目标；请求日志与账本的模型恢复为源模型，
// 账户与通道恢复为源模型运行结束时的值。
// 改用之后请求超时或取消时按那次的错误结束（504、499）

// modelFallbackSettings 为生效的改用规则与范围（启动时与服务配置页保存后整份替换）
type modelFallbackSettings struct {
	scope string
	rules []config.ModelFallbackRule
	// text 为规则的规范写法（热更新日志与变化判断用）
	text string
}

// setModelFallbacks 应用 MODEL_FALLBACKS 与 MODEL_FALLBACK_SCOPE（启动时与热更新时调用），返回是否有变化。取值已在加载与保存时校验过，
// 规则仍解析不了时按没有规则处理
func (service *trackedService) setModelFallbacks(value string, scope string) bool {
	rules, err := config.ParseModelFallbacks(value)
	if err != nil {
		rules = nil
	}
	next := &modelFallbackSettings{scope: config.NormalizeModelFallbackScope(scope), rules: rules, text: config.FormatModelFallbacks(rules)}
	previous := service.modelFallbacks.Swap(next)
	return previous == nil || previous.scope != next.scope || previous.text != next.text
}

// fallbackSignatureCapacity 为改用期间签发的思考签名的保留条数（超过时从最早的淘汰；每条只存摘要与模型，约百字节）。
// 被淘汰或重启后认不出的签名会原样发给别的模型（见文件开头），容量按一天的改用流量留足
const fallbackSignatureCapacity = 32768

// fallbackSignatureRegistry 记下额度用完改用其他模型期间目标模型交出的思考签名（只存摘要），之后的请求据此判断历史里的签名是哪个模型
// 签发的：改用时保留目标模型自己的签名，发给别的模型时换成占位值或去掉（见文件开头）。零值可用，并发安全
type fallbackSignatureRegistry struct {
	mu sync.Mutex
	// issuers 以签名摘要为键，值为签发的目录模型 ID（小写）；order 为登记顺序
	issuers map[string]string
	order   []string
}

// fallbackSignatureDigest 返回签名的摘要（不存签名原文）
func fallbackSignatureDigest(signature string) string {
	digest := sha256.Sum256([]byte(signature))
	return hex.EncodeToString(digest[:16])
}

// rememberEvent 登记事件里的思考签名（正文、思考与函数调用上的）为 model 签发；占位值不登记
func (registry *fallbackSignatureRegistry) rememberEvent(model string, event aistudio.Event) {
	signatures := []string{event.ThoughtSignature}
	if event.ToolCall != nil {
		signatures = append(signatures, event.ToolCall.ThoughtSignature)
	}
	model = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(model), "models/"))
	for _, signature := range signatures {
		if model == "" || aistudio.PlaceholderThoughtSignature(signature) {
			continue
		}
		key := fallbackSignatureDigest(signature)
		registry.mu.Lock()
		if registry.issuers == nil {
			registry.issuers = make(map[string]string)
		}
		if _, exists := registry.issuers[key]; !exists {
			registry.order = append(registry.order, key)
		}
		registry.issuers[key] = model
		for len(registry.order) > fallbackSignatureCapacity {
			delete(registry.issuers, registry.order[0])
			registry.order = registry.order[1:]
		}
		registry.mu.Unlock()
	}
}

// issuer 返回签名是改用期间哪个模型签发的（目录模型 ID，小写），不是改用期间签发的为空串。摘要在锁外计算（签名常有几 KB），锁内只查表
func (registry *fallbackSignatureRegistry) issuer(signature string) string {
	if aistudio.PlaceholderThoughtSignature(signature) {
		return ""
	}
	key := fallbackSignatureDigest(signature)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.issuers[key]
}

// foreign 去掉对话里改用期间由别的模型签发、不能发给 model 的签名（见 aistudio.ReplaceThoughtSignatures），返回处理后的对话、处理的签名数
// 与签发的模型；没有这样的签名时原样返回（不复制）。登记表只按容量淘汰、不会变空：进程里发生过一次改用后，之后每个生成请求（所有模型、
// 所有号池，范围改为 off 之后也是：不把改用期间目标模型的签名发给别的模型）都要查一遍，所以每个签名只算一次摘要、查一次表，
// 结果记在局部的表里供替换时判断
func (registry *fallbackSignatureRegistry) foreign(contents []aistudio.Content, model string) ([]aistudio.Content, int, []string) {
	registry.mu.Lock()
	empty := len(registry.issuers) == 0
	registry.mu.Unlock()
	if empty {
		// 没有发生过改用：不逐个计算签名摘要，请求原样发送
		return contents, 0, nil
	}
	model = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(model), "models/"))
	var issuers []string
	// seen 为对话里每个签名的签发模型（不是改用期间签发的为空串）：ReplaceThoughtSignatures 的 keep 按部分取同样的签名，直接查这张表
	seen := make(map[string]string)
	for _, content := range contents {
		for _, part := range content.Parts {
			signature := part.ThoughtSignature
			if signature == "" && part.FunctionCall != nil {
				signature = part.FunctionCall.ThoughtSignature
			}
			if signature == "" {
				continue
			}
			issuer, done := seen[signature]
			if !done {
				issuer = registry.issuer(signature)
				seen[signature] = issuer
			}
			if issuer != "" && issuer != model && !slices.Contains(issuers, issuer) {
				issuers = append(issuers, issuer)
			}
		}
	}
	if len(issuers) == 0 {
		return contents, 0, nil
	}
	result, replaced := aistudio.ReplaceThoughtSignatures(contents, func(signature string) bool {
		issuer := seen[signature]
		return issuer == "" || issuer == model
	})
	return result, replaced, issuers
}

// modelFallbackSummary 为热更新日志里的改用设置
func modelFallbackSummary(value string, scope string) string {
	if strings.TrimSpace(value) == "" {
		value = "<空>"
	}
	return fmt.Sprintf("MODEL_FALLBACKS=%s | MODEL_FALLBACK_SCOPE=%s", value, scope)
}

// applies 判断规则对请求的号池生效：all 都生效，ultra 只对 /ultra 请求生效，off、未设置或没有规则时都不生效
func (settings *modelFallbackSettings) applies(scope aistudio.PoolScope) bool {
	if settings == nil || len(settings.rules) == 0 {
		return false
	}
	switch settings.scope {
	case config.ModelFallbackScopeAll:
		return true
	case config.ModelFallbackScopeUltra:
		return scope == aistudio.PoolScopeUltra
	default:
		return false
	}
}

// targetsFor 返回源模型（目录模型 ID）的改用目标：规则的源模型写目录别名时按号池的目录解析，比较不分大小写；没有规则时为 nil
func (settings *modelFallbackSettings) targetsFor(pool *aistudio.AccountPool, scope aistudio.PoolScope, modelID string) []string {
	for _, rule := range settings.rules {
		if strings.EqualFold(canonicalFallbackModel(pool, scope, rule.Source), modelID) {
			return rule.Targets
		}
	}
	return nil
}

// canonicalFallbackModel 把规则里的模型名按号池的目录解析成目录模型 ID（是目录别名时换成正式 ID），去掉 models/ 前缀
func canonicalFallbackModel(pool *aistudio.AccountPool, scope aistudio.PoolScope, name string) string {
	return strings.TrimPrefix(strings.TrimSpace(pool.CanonicalModelIDIn(scope, name)), "models/")
}

// modelQuotaEnd 判断生成请求以“这个模型在号池里没有可用额度”结束（触发条件见文件开头），返回原因与最早恢复时间（不知道时为零值）
func modelQuotaEnd(err error, now time.Time) (string, time.Time, bool) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", time.Time{}, false
	}
	var busy *aistudio.CapacityBusyError
	if errors.As(err, &busy) {
		return "", time.Time{}, false
	}
	var quota *aistudio.PrimaryQuotaError
	if errors.As(err, &quota) {
		if quota.Kind == aistudio.QuotaFailEgress || quota.Reason == aistudio.QuotaReasonEgress {
			return "", time.Time{}, false
		}
		return "额度不足：" + quota.Reason, quota.Until, true
	}
	var cooling *aistudio.AllCoolingError
	if errors.As(err, &cooling) {
		return "候选全部冷却", cooling.Until, true
	}
	var blocked *aistudio.AccountCoolingDownError
	if errors.As(err, &blocked) {
		reason := "租约账号发出前已冷却"
		if kind, _, _ := strings.Cut(blocked.Reason, ":"); strings.TrimSpace(kind) != "" {
			reason += "（" + strings.TrimSpace(kind) + "）"
		}
		return reason, blocked.Until, true
	}
	if errors.Is(err, aistudio.ErrAccountCoolingDown) {
		return "租约账号发出前已冷却", time.Time{}, true
	}
	if cooldown, ok := aistudio.QuotaCooldownForError(err, now); ok && !cooldown.Global &&
		(cooldown.Kind == aistudio.DailyQuotaKind || cooldown.Kind == aistudio.MinuteQuotaKind) {
		return cooldown.Kind + " 429", cooldown.Until, true
	}
	return "", time.Time{}, false
}

// quotaEndSummary 为日志里的额度用完原因：原因与最早恢复时间（本地时区 RFC3339 与距现在多久）
func quotaEndSummary(reason string, until time.Time) string {
	if until.IsZero() {
		return reason
	}
	return fmt.Sprintf("%s，最早恢复 %s（约 %s 后）", reason, until.Local().Format(time.RFC3339), retryAfterText(until))
}

// modelFallbackRun 为一个请求改用其他模型的进度：源模型额度用完时建立，之后各个目标模型的运行共用
type modelFallbackRun struct {
	// source 为源模型的请求（generateWithRetry 入口时的请求，不固定账户、没有文件绑定），sourceModel 为它的目录模型 ID
	source      aistudio.GenerateRequest
	sourceModel string
	// sourceErr 为源模型额度用完的错误：所有目标都不可用时按它返回；reason 为它的原因摘要（含最早恢复时间）
	sourceErr error
	reason    string
	// sourceAccount、sourceChannel 为源模型运行结束时请求日志记下的账户与通道（候选全部冷却、一次也没有发出时为空）：所有目标都不可用、
	// 按源模型的错误结束时恢复，不记成目标模型最后用过的账户与通道
	sourceAccount string
	sourceChannel string
	// earliest 为源模型与额度用完的目标中最早的恢复时间（Retry-After）
	earliest time.Time
	// targets 为规则里的目标，next 为下一个要检查的位置，current 为正在运行的目标
	targets []string
	next    int
	current string
	// tried 为已经运行或跳过的目标（目录模型 ID，小写）；results 为每个目标的结果与原因（写进最终日志），outcomes 为每个目标的结果类别
	// （额度用完、不可用、跳过，写进最终错误：不嵌入目标错误的原文，账本与诊断脚本按源模型的错误分类）
	tried    map[string]struct{}
	results  []string
	outcomes []string
	// catalog 为请求号池的模型目录（第一次检查目标时取一次）
	catalog []aistudio.Model
	// modelStartedAt 为当前模型开始运行的时刻（请求开始，或上一次改用）：改用备注的耗时为这个模型用掉的时间
	modelStartedAt time.Time
	// switched 为已经真正改用过一次（switchModelFallback）：只在第一次改用时给源模型的上游尝试补记模型。目标都被跳过、没有改用时
	// 尝试不带模型（与没有改用的请求相同）；第二次改用时不再补记，此前的改用备注不会被记成源模型
	switched bool
}

// modelFallbackExhaustedError 为改用其他模型之后所有目标也都额度用完或不可用时的最终错误：对外按源模型原来的错误返回（状态码、错误码、文案
// 与原因响应头不变，Unwrap 为源模型的错误），Retry-After 取源模型与各目标的建议重试中最早的时刻；Error() 在源模型的错误后写明试过的目标与
// 结果类别（目标的原因只写进请求日志）
type modelFallbackExhaustedError struct {
	err     error
	retryAt time.Time
	tried   string
}

func (e *modelFallbackExhaustedError) Error() string {
	return e.err.Error() + "（已按 MODEL_FALLBACKS 改用其他模型：" + e.tried + "）"
}

func (e *modelFallbackExhaustedError) Unwrap() error {
	return e.err
}

// RetryAt 返回源模型与各目标的建议重试中最早的时刻；都不知道时按源模型错误自己的恢复时间
func (e *modelFallbackExhaustedError) RetryAt() time.Time {
	if !e.retryAt.IsZero() {
		return e.retryAt
	}
	var provider interface{ RetryAt() time.Time }
	if errors.As(e.err, &provider) {
		return provider.RetryAt()
	}
	return time.Time{}
}

// modelFallbackSwitchNote 为改用时写进请求日志的一行（WARN）
func modelFallbackSwitchNote(source string, reason string, target string) string {
	return fmt.Sprintf("模型 %s 额度用完（%s），按 MODEL_FALLBACKS 改用 %s", source, reason, target)
}

// continueWithModelFallback 在按一个模型的换号重试以错误 err 结束、还没有交出任何事件时（generateForModel 的结束分支）判断是否改用其他模型。
// 要改用时返回目标模型的请求与改用进度，由调用方按目标模型重新走一遍；不改用时返回 nil 与最终错误：没有改用时就是 err，改用过、所有目标都
// 不可用时为按源模型原来的错误、Retry-After 取最早恢复的包装（请求已超时或取消时为这次的 err）。entry 为这次运行入口时的请求，
// startedAt 为生成请求开始的时刻，run 为改用进度（源模型的运行为 nil）
func (service *trackedService) continueWithModelFallback(
	requestCtx context.Context, entry aistudio.GenerateRequest, resourceID string, diag *generationDiagnostics,
	startedAt time.Time, run *modelFallbackRun, err error,
) (*aistudio.GenerateRequest, *modelFallbackRun, error) {
	now := time.Now()
	scope := aistudio.PoolScopeFromContext(requestCtx)
	if run == nil {
		settings := service.modelFallbacks.Load()
		if !settings.applies(scope) {
			return nil, nil, err
		}
		reason, until, quota := modelQuotaEnd(err, now)
		if !quota {
			return nil, nil, err
		}
		modelID := strings.TrimPrefix(strings.TrimSpace(entry.Model), "models/")
		targets := settings.targetsFor(service.pool, scope, modelID)
		if len(targets) == 0 {
			return nil, nil, err
		}
		summary := quotaEndSummary(reason, until)
		if why := service.modelFallbackBlocked(requestCtx, entry, modelID, resourceID, diag); why != "" {
			service.requests.logRequestProgress(entry.ID, "service", "WARN", fmt.Sprintf(
				"模型 %s 额度用完（%s），但%s，不按 MODEL_FALLBACKS 改用", modelID, summary, why))
			return nil, nil, err
		}
		run = &modelFallbackRun{
			source: entry, sourceModel: modelID, sourceErr: err, reason: summary, earliest: until,
			targets: targets, tried: make(map[string]struct{}, len(targets)), modelStartedAt: startedAt,
		}
		run.sourceAccount, run.sourceChannel = api.AccessLogTarget(requestCtx)
	} else {
		// 目标模型的运行以错误结束：记下结果。额度用完时它的恢复时间参与 Retry-After；只是暂时不可用（号池繁忙、出口限流、发出前冷却等）
		// 而错误带建议重试时间时同样参与：几秒后再发多半就能改用过去，不按源模型的十几小时
		target := run.current
		run.current = ""
		if reason, until, quota := modelQuotaEnd(err, now); quota {
			run.results = append(run.results, fmt.Sprintf("%s 额度用完（%s）", target, quotaEndSummary(reason, until)))
			run.outcomes = append(run.outcomes, target+" 额度用完")
			run.earliest = earlierTime(run.earliest, until)
		} else {
			run.results = append(run.results, fmt.Sprintf("%s 不可用（%s）", target, progressReason(err)))
			run.outcomes = append(run.outcomes, target+" 不可用")
			var provider interface{ RetryAt() time.Time }
			if requestCtx.Err() == nil && errors.As(err, &provider) {
				run.earliest = earlierTime(run.earliest, provider.RetryAt())
			}
		}
		if requestCtx.Err() != nil {
			// 请求已超时或取消：按这次的错误结束（504、499），不再试其他目标
			service.endModelFallback(requestCtx, run, "请求已结束（"+progressReason(requestCtx.Err())+"）")
			return nil, run, err
		}
	}
	for run.next < len(run.targets) {
		name := run.targets[run.next]
		run.next++
		target, skip := service.modelFallbackTarget(scope, run, name)
		if skip != "" {
			note := fmt.Sprintf("MODEL_FALLBACKS 的目标 %s %s，跳过", name, skip)
			run.results = append(run.results, fmt.Sprintf("%s 跳过（%s）", name, skip))
			run.outcomes = append(run.outcomes, name+" 跳过")
			service.requests.logRequestProgress(entry.ID, "service", "WARN", note)
			aistudio.TraceFromContext(requestCtx).Note(note)
			continue
		}
		request := service.switchModelFallback(requestCtx, run, target, diag)
		return &request, run, nil
	}
	final := &modelFallbackExhaustedError{err: run.sourceErr, retryAt: run.earliest, tried: strings.Join(run.outcomes, "；")}
	service.endModelFallback(requestCtx, run, "按源模型的错误返回")
	return nil, run, final
}

// modelFallbackBlocked 返回额度用完的请求不改用其他模型的原因（写进日志），能改用时返回空串
func (service *trackedService) modelFallbackBlocked(
	requestCtx context.Context, entry aistudio.GenerateRequest, modelID string, resourceID string, diag *generationDiagnostics,
) string {
	switch {
	case requestCtx.Err() != nil:
		return "请求已结束"
	case strings.TrimSpace(entry.AccountID) != "":
		return "请求指定了账户"
	case resourceID != "":
		return "请求带的文件属于某个账户（文件绑定）"
	case diag.guard() != nil:
		return "请求在降级判定里（DOWNGRADE_GUARD_MODELS），事件流已交给按源模型建立的判定"
	case service.pool.InteractionModel(modelID):
		return "源模型是 Interaction（Omni）模型"
	case !aistudio.TextGenerationRequest(entry):
		return "请求不是文本对话生成（图片、语音、转录等）"
	}
	return ""
}

// modelFallbackTarget 检查规则里的一个目标：返回目标在请求号池目录里的条目；不能改用时返回跳过的原因
func (service *trackedService) modelFallbackTarget(scope aistudio.PoolScope, run *modelFallbackRun, name string) (aistudio.Model, string) {
	canonical := canonicalFallbackModel(service.pool, scope, name)
	if strings.EqualFold(canonical, run.sourceModel) {
		return aistudio.Model{}, "与源模型 " + run.sourceModel + " 是同一个模型"
	}
	key := strings.ToLower(canonical)
	if _, done := run.tried[key]; done {
		return aistudio.Model{}, "已经试过"
	}
	run.tried[key] = struct{}{}
	if run.catalog == nil {
		run.catalog = service.pool.EligibleModelsIn(scope, service.modelSnapshot())
	}
	for _, model := range run.catalog {
		if !strings.EqualFold(model.ID, canonical) {
			continue
		}
		// 按目标模型整理过的请求副本检查：-online 让协议层追加的 google_search 在整理时按目标的能力重新决定（目标不支持搜索时不加，
		// 与直接请求目标模型带 -online 时相同），不当作客户端要求的工具而误跳过
		probe := run.source
		api.RetargetGenerateRequest(&probe, model)
		if why := aistudio.FallbackTargetUnsupported(model, probe); why != "" {
			return aistudio.Model{}, why
		}
		// 降级判定在 startGenerate 按源模型建立，改用后的目标模型没有判定；目标在 DOWNGRADE_GUARD_MODELS 里时直接请求它会被判定或拦截，
		// 不绕过判定交出它的回复
		if service.downgradeGuard.Load().covers(model.ID) && randomSeedApplicable(model.ID, probe.Config) {
			return aistudio.Model{}, "在降级判定里（DOWNGRADE_GUARD_MODELS），改用后没有按它建立的判定"
		}
		return model, ""
	}
	return aistudio.Model{}, "不在这个号池的模型目录里"
}

// switchModelFallback 按目标模型整理请求并记下改用：请求日志一行 WARN、排查时间线、请求日志与账本的模型与备注、活动请求列表、
// 响应报告的模型（aistudio.ModelSwitch）、流式响应头的等待（aistudio.StreamStartHold）与签名登记（diag）。返回目标模型的请求
func (service *trackedService) switchModelFallback(
	requestCtx context.Context, run *modelFallbackRun, target aistudio.Model, diag *generationDiagnostics,
) aistudio.GenerateRequest {
	request := run.source
	request.AccountID = ""
	request.Notes = nil
	notes := api.RetargetGenerateRequest(&request, target)
	// 思考签名：目标模型在之前改用时签发的原样保留，其余换成占位值或去掉（只有签名的部分）；签名缺失或为占位值的函数调用再按目标模型的作用域
	// 按调用 ID 还原（见文件开头）。还原按下标改写，先复制每条消息的部分，不改动源模型的请求
	targetKey := strings.ToLower(target.ID)
	contents, replaced := aistudio.ReplaceThoughtSignatures(request.Contents, func(signature string) bool {
		return service.fallbackSignatures.issuer(signature) == targetKey
	})
	restored := 0
	if restore := aistudio.ThoughtSignatureRestoreFromContext(requestCtx); restore != nil {
		for index := range contents {
			contents[index].Parts = slices.Clone(contents[index].Parts)
		}
		restored = restore(requestCtx, target.ID, contents)
	}
	request.Contents = contents
	// 最大输出下限按目标模型重新判断（图片、语音等模型不调整，与 startGenerate 相同）
	request.MinOutputTokens = 0
	if floor := service.minOutputTokens.Load(); floor > 0 && randomSeedApplicable(request.Model, request.Config) {
		request.MinOutputTokens = floor
	}
	run.current = target.ID
	note := modelFallbackSwitchNote(run.sourceModel, run.reason, target.ID)
	if replaced > 0 {
		note += fmt.Sprintf(" | 历史里的 %d 个思考签名换成跳过校验的占位值或去掉", replaced)
	}
	if restored > 0 {
		note += fmt.Sprintf(" | 按 %s 签发过的签名还原了 %d 个函数调用的签名", target.ID, restored)
	}
	aistudio.TraceFromContext(requestCtx).Note(note)
	service.requests.logRequestProgress(request.ID, "service", "WARN", note)
	for _, extra := range notes {
		service.requests.logRequestProgress(request.ID, "service", "WARN", extra)
	}
	// long- 前缀按目标模型重新判断后，开关与 Interaction 的检查、默认总上限与 startGenerate 相同
	if request.Alias != nil {
		service.prepareLongOutput(&request)
	}
	api.SetAccessLogModelFallback(requestCtx, run.sourceModel, target.ID)
	api.SetAccessLogGenerationConfig(requestCtx, request.Config)
	if !run.switched {
		// 账本的上游尝试：第一次真正改用时给改用前源模型的尝试记上源模型，之后目标模型的尝试记下时写目标模型（请求本身的模型只有一个，
		// 诊断脚本按尝试的模型分类）。要在追加改用备注之前，否则备注也被记成源模型
		run.switched = true
		api.TagAccessLogAttempts(requestCtx, run.sourceModel)
	}
	api.AddAccessLogAttempt(requestCtx, api.RequestAttempt{Error: note, DurationMS: time.Since(run.modelStartedAt).Milliseconds()})
	run.modelStartedAt = time.Now()
	aistudio.ModelSwitchFromContext(requestCtx).Switch(run.sourceModel, target.ID)
	service.requests.markModelFallback(request.ID, run.sourceModel, target.ID)
	// 流式请求：API 层写出响应头前的等待从现在起按目标模型重新计（目标是主用模型时为它的排队上限加余量），目标模型首事件前的错误照常以
	// HTTP 状态返回，响应开头报告目标模型；合计仍不超过 API 层的上限
	aistudio.StreamStartHoldFromContext(requestCtx).RestartWait(service.StreamStartWait(requestCtx, target.ID))
	if diag != nil {
		diag.fallbackModel = target.ID
	}
	return request
}

// endModelFallback 在改用之后仍以错误结束时写一行日志（试过哪些目标、结果与 Retry-After），并把请求日志、账本、活动请求列表与响应报告的
// 模型恢复为源模型，请求日志与账本的账户、通道恢复为源模型运行结束时的值（目标模型的尝试仍在账本的上游尝试里，各带模型）
func (service *trackedService) endModelFallback(requestCtx context.Context, run *modelFallbackRun, outcome string) {
	retry := "-"
	if !run.earliest.IsZero() {
		retry = run.earliest.Local().Format(time.RFC3339) + "（约 " + retryAfterText(run.earliest) + " 后）"
	}
	tried := strings.Join(run.results, "；")
	if tried == "" {
		tried = "-"
	}
	note := fmt.Sprintf("模型 %s 额度用完，按 MODEL_FALLBACKS 改用其他模型也没有成功，%s | 试过=%s | 最早恢复=%s",
		run.sourceModel, outcome, tried, retry)
	aistudio.TraceFromContext(requestCtx).Note(note)
	service.requests.logRequestProgress(run.source.ID, "service", "WARN", note)
	// 模型、改用标记、账户与通道在同一把锁内一起恢复：访问日志随时可能取快照（客户端断开 499 时），不记下一半是源模型、一半是目标模型的记录
	api.RestoreAccessLogSource(requestCtx, run.sourceModel, run.sourceAccount, run.sourceChannel)
	aistudio.ModelSwitchFromContext(requestCtx).Switch(run.sourceModel, "")
	service.requests.markModelFallback(run.source.ID, run.sourceModel, "")
}

// modelFallbackPending 判断源模型的运行以额度用完结束后还会交给 MODEL_FALLBACKS（生效范围内、模型有规则、请求能改用；目标能不能接到时才知道）：
// 这时主用模型的选号日志不写“立即返回 429”（见 primaryAllCoolingMessage、primaryQuotaMessage），结果看之后的改用日志
func (service *trackedService) modelFallbackPending(
	requestCtx context.Context, request aistudio.GenerateRequest, resourceID string, diag *generationDiagnostics,
) bool {
	settings := service.modelFallbacks.Load()
	scope := aistudio.PoolScopeFromContext(requestCtx)
	if !settings.applies(scope) {
		return false
	}
	modelID := strings.TrimPrefix(strings.TrimSpace(request.Model), "models/")
	if len(settings.targetsFor(service.pool, scope, modelID)) == 0 {
		return false
	}
	return service.modelFallbackBlocked(requestCtx, request, modelID, resourceID, diag) == ""
}

// markModelFallback 记下活动请求额度用完改用了其他模型（from 为改用前的模型）；model 为空时恢复为 from（改用之后仍以错误结束）
func (registry *requestRegistry) markModelFallback(id string, from string, model string) {
	registry.mu.Lock()
	if tracked, exists := registry.active[id]; exists {
		if model == "" {
			tracked.request.Model, tracked.request.FallbackFrom = from, ""
		} else {
			tracked.request.Model, tracked.request.FallbackFrom = model, from
		}
		registry.active[id] = tracked
		registry.publishLocked(api.AdminEvent{Type: "request", Data: tracked.request})
	}
	registry.mu.Unlock()
}
