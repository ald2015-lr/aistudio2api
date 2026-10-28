package aistudio

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// 官网停用了经 Build 应用代理的生成：ProxyStreamedCall（streamGenerateContent）与 ProxyUnaryCall（generateContent）都返回
// HTTP 400、文案以 “PROXY_GENERATION_DISABLED: streamGenerateContent is not supported” 或 “… generateContent is not supported” 开头，
// 可能直接作为错误响应，也可能作为流尾的 google.rpc 状态。这与请求本身（参数错误）、账号额度（429）和权限（403）都无关，
// 换一个通道（Playground）就能完成。这里只负责识别与账户池里的暂停标记；暂停的判定、试探与日志在 app 层（见 app/build_pause.go）

// ErrBuildGenerationDisabled 为官网停用了 Build 通道生成的哨兵：Build 代理调用返回的 PROXY_GENERATION_DISABLED（*RPCError）
// 与 Build 暂停期间只能经 Build 处理的请求（*BuildGenerationUnavailableError）都满足 errors.Is(err, ErrBuildGenerationDisabled)
var ErrBuildGenerationDisabled = errors.New("官网停用了 Build 通道的生成（PROXY_GENERATION_DISABLED）")

// buildGenerationDisabledMarker 为上游文案开头的标记（区分大小写）
const buildGenerationDisabledMarker = "PROXY_GENERATION_DISABLED"

// buildGenerationDisabled 判断上游错误是否为官网停用了 Build 通道的生成：只认 Build 代理调用（ProxyStreamedCall、ProxyUnaryCall）
// 的错误，文案去掉内部标记（"[original: …]"、"generic::…:"、"beyond::…:" 前缀与 "(qos=…)"，与 API 层对外返回上游文案时的清洗相同，
// 见 CleanUpstreamMessage）与首尾空白后以 PROXY_GENERATION_DISABLED 开头，其后是冒号、空白或文案结束；不看状态码与协议错误码
// （直接返回时为 HTTP 400，流尾状态按协议错误码换算）。线上客户端看到的是清洗后的文案，分不出原文带不带这些标记，不清洗时带标记的原文
// 整套机制都不触发、原样返回 400。只认开头、区分大小写：参数错误的 400 会回显客户端请求里的字段名与取值（如工具参数里名为
// proxy_generation_disabled 的属性），按“包含”匹配会把这类请求本身的错误当成官网停用 Build，暂停账号、改按 503 返回
func (e *RPCError) buildGenerationDisabled() bool {
	if e == nil || e.Method != buildProxyStreamedMethod && e.Method != buildProxyUnaryMethod {
		return false
	}
	rest, found := strings.CutPrefix(CleanUpstreamMessage(e.Message), buildGenerationDisabledMarker)
	return found && (rest == "" || rest[0] == ':' || unicode.IsSpace(rune(rest[0])))
}

// BuildGenerationDisabledFailure 判断错误是否说明官网停用了 Build 通道的生成（Build 代理调用返回 PROXY_GENERATION_DISABLED）；
// 经 errors.Join 或 %w 包装后同样成立。Build 暂停期间无法处理的请求（*BuildGenerationUnavailableError）不算：它已经是给客户端的结论
func BuildGenerationDisabledFailure(err error) bool {
	var rpcError *RPCError
	if err == nil || !errors.As(err, &rpcError) {
		return false
	}
	var unavailable *BuildGenerationUnavailableError
	if errors.As(err, &unavailable) {
		return false
	}
	return errors.Is(err, ErrBuildGenerationDisabled)
}

// BuildUnavailableReason 为 Build 暂停期间无法处理请求的原因
type BuildUnavailableReason int

const (
	// BuildUnavailableModel 为只能经 Build 通道调用的模型（Build 目录有、Playground 目录没有，如 Ultra 号池的 gemini-4-argon）
	BuildUnavailableModel BuildUnavailableReason = iota
	// BuildUnavailableInputLimit 为输入超过 Playground 的上限、只能改走 Build 的请求
	BuildUnavailableInputLimit
	// BuildUnavailableNoPlayground 为 Playground 接不了的请求：UPSTREAM_CHANNELS 没有启用 Playground，或换到 Playground 后仍没有可用账号
	BuildUnavailableNoPlayground
)

// BuildGenerationUnavailableError 表示官网停用了 Build 通道的生成（Build 已暂停或这次请求刚遇到 PROXY_GENERATION_DISABLED），
// 而请求只能经 Build 处理。对外给出官方风格的说明，不把 PROXY_GENERATION_DISABLED 原文交给客户端：输入超过 Playground 上限为 400
// （按输入过长返回，客户端可以压缩上下文后重试），其余为 503（暂时无法处理，稍后重试或换模型）
type BuildGenerationUnavailableError struct {
	Reason BuildUnavailableReason
	Model  string
	// InputLimit 为 Playground 的输入上限（token），从上游的输入超限文案里取，取不到时为 0（只在 BuildUnavailableInputLimit 时有）
	InputLimit int64
	// Cause 为内部原因（Playground 的输入超限错误、Build 的 PROXY_GENERATION_DISABLED、选号错误），只写管理日志
	Cause error
	// NoCandidate 为账户池选号没有候选时返回（见 noEligibleErrorLocked）：同时满足 errors.Is(err, ErrNoEligibleAccount)，
	// 换号重试据此照原规则处理“没有可换的账号”（保留上一次尝试的错误、额度失败后按额度不足结束、回到原账号等 Worker 关闭等）；
	// 对外仍按本类型的状态码与说明返回（API 层先判断本类型）
	NoCandidate bool
}

func (e *BuildGenerationUnavailableError) Error() string {
	var message string
	switch e.Reason {
	case BuildUnavailableInputLimit:
		limit := "Playground 上限"
		if e.InputLimit > 0 {
			limit = fmt.Sprintf("Playground 上限（%d token）", e.InputLimit)
		}
		message = "输入超过" + limit + "，" + ErrBuildGenerationDisabled.Error() + "，无法改走 Build 处理更长的输入"
	case BuildUnavailableNoPlayground:
		message = ErrBuildGenerationDisabled.Error() + "，Playground 通道接不了模型 " + e.Model + " 的这个请求（没有启用或没有可用账号）"
	default:
		message = "模型 " + e.Model + " 只能经 Build 通道调用，" + ErrBuildGenerationDisabled.Error() + "，暂时无法处理"
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

// Unwrap 保留官网停用 Build 生成的哨兵与内部原因；选号没有候选时另带无账户哨兵
func (e *BuildGenerationUnavailableError) Unwrap() []error {
	errs := []error{ErrBuildGenerationDisabled}
	if e.NoCandidate {
		errs = append(errs, ErrNoEligibleAccount)
	}
	if e.Cause != nil {
		errs = append(errs, e.Cause)
	}
	return errs
}

// HTTPStatus 返回对外状态码：输入超过 Playground 上限为 400，其余为 503
func (e *BuildGenerationUnavailableError) HTTPStatus() int {
	if e.Reason == BuildUnavailableInputLimit {
		return http.StatusBadRequest
	}
	return http.StatusServiceUnavailable
}

// PublicMessage 返回给客户端的英文说明（官方措辞风格，不含 PROXY_GENERATION_DISABLED 原文与账号信息）
func (e *BuildGenerationUnavailableError) PublicMessage() string {
	switch e.Reason {
	case BuildUnavailableInputLimit:
		limit := "the maximum number of tokens allowed on the AI Studio Playground channel"
		if e.InputLimit > 0 {
			limit = fmt.Sprintf("the maximum number of tokens allowed (%d) on the AI Studio Playground channel", e.InputLimit)
		}
		return "The input token count exceeds " + limit +
			", and longer inputs cannot be processed because AI Studio has disabled generation through the Build channel."
	case BuildUnavailableNoPlayground:
		return "AI Studio has disabled generation through the Build channel, and the Playground channel cannot serve this request right now. Please try again later."
	default:
		model := strings.TrimSpace(e.Model)
		if model == "" {
			model = "The requested model"
		} else {
			model = "Model " + model
		}
		return model + " is only available through the AI Studio Build channel, and AI Studio has disabled generation through the Build channel. Please try again later or use another model."
	}
}

// inputCapLimitPattern 从账号输入上限的文案（"… maximum number of tokens allowed 131072"）里取上限
var inputCapLimitPattern = regexp.MustCompile(`(?i)maximum number of tokens allowed\s*\(?\s*(\d+)`)

// InputTokenLimitValue 返回输入超限错误文案里的上限（token），取不到时为 0
func InputTokenLimitValue(err error) int64 {
	if err == nil {
		return 0
	}
	match := inputCapLimitPattern.FindStringSubmatch(inputLimitMessage(err))
	if match == nil {
		return 0
	}
	value, parseErr := strconv.ParseInt(match[1], 10, 64)
	if parseErr != nil || value <= 0 {
		return 0
	}
	return value
}

// SetBuildGenerationPaused 设置 Build 生成的全局暂停，返回状态是否变化。暂停期间生成请求的选号把 Build 通道当作没有启用
// （试探请求 BuildProbe 除外），Playground 照常；只启用 Build 时生成请求没有候选。变化时候选索引整个重建并唤醒等待调度的请求
func (p *AccountPool) SetBuildGenerationPaused(paused bool) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buildPaused == paused {
		return false
	}
	p.buildPaused = paused
	// 暂停与否决定每个生成候选集的通道顺序与按状态口径的冷却（见 buildGenerationOpenLocked）：与改变上游通道相同，整个候选索引重建，
	// 状态缓存同时作废
	p.eligibilityVersion++
	p.invalidateSchedulingLocked()
	p.invalidateStatusLocked()
	p.notifyLocked()
	return true
}

// BuildGenerationPaused 返回 Build 生成是否全局暂停
func (p *AccountPool) BuildGenerationPaused() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buildPaused
}

// SetAccountBuildPaused 设置账户的 Build 生成暂停，返回状态是否变化（账户不存在时为假）。暂停的账户不再经 Build 通道接生成请求
// （试探请求除外），Playground 照常；不写冷却，不影响账户状态
func (p *AccountPool) SetAccountBuildPaused(accountID string, paused bool) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil || account.buildPaused == paused {
		return false
	}
	account.buildPaused = paused
	p.accountChangedLocked(account)
	p.notifyLocked()
	return true
}

// ClearBuildPauses 解除全局与全部账户的 Build 生成暂停（试探成功后），返回解除的账户数
func (p *AccountPool) ClearBuildPauses() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cleared := 0
	for _, account := range p.accounts {
		if account != nil && account.buildPaused {
			account.buildPaused = false
			cleared++
			if !p.buildPaused {
				p.accountChangedLocked(account)
			}
		}
	}
	if p.buildPaused {
		p.buildPaused = false
		p.eligibilityVersion++
		p.invalidateSchedulingLocked()
		p.invalidateStatusLocked()
	}
	p.notifyLocked()
	return cleared
}

// buildPauseActiveLocked 判断这个选择受 Build 暂停影响：全局暂停，或账户暂停；试探请求不受影响。调用方持有 p.mu
func (p *AccountPool) buildPauseActiveLocked(account *Account, selection AccountSelection) bool {
	return !selection.BuildProbe && (p.buildPaused || account != nil && account.buildPaused)
}

// buildPauseBlocksLocked 判断没有候选只是因为 Build 暂停（官网停用了 Build 通道的生成）：生成选择、Build 已启用、不看暂停时这个选择
// 会经 Build 选号（降级判定的 Playground 优先不经 Build，暂停不是原因），这次选择范围内（号池、指定账户、限定账户集合）有启用的账户
// 能经 Build 调用该模型而 Build 暂停中（全局或该账户）；并且除只走 Build 的选择（输入超过 Playground 上限）外，范围内没有任何启用的
// 账户能经 Playground 接这个选择。Playground 能否接不看换号排除、冷却与 Worker：有账户能经 Playground 接时，没有候选是因为它们
// 已被换号排除、额度用尽或不可调度，按原来的 ErrNoEligibleAccount 处理（额度失败后按额度不足 429 结束、保留上一次尝试的错误），
// 不说成“只能经 Build 调用”。只在没有候选时调用：没有任何暂停时只扫一遍暂停标记，否则遍历范围内的账户；调用方持有 p.mu
func (p *AccountPool) buildPauseBlocksLocked(selection AccountSelection) bool {
	if selection.BuildProbe || strings.TrimSpace(selection.ModelID) == "" || !generationChannelSelection(selection) ||
		!p.channelEnabledLocked(ChannelBuild) || !p.buildPauseEngagedLocked() {
		return false
	}
	probe := selection
	probe.BuildProbe = true
	if !slices.Contains(p.selectionChannelsLocked(probe), ChannelBuild) {
		return false
	}
	accountID := strings.TrimSpace(selection.AccountID)
	var allowed map[string]struct{}
	if selection.AllowedAccountIDs != nil {
		allowed = make(map[string]struct{}, len(selection.AllowedAccountIDs))
		for _, id := range selection.AllowedAccountIDs {
			allowed[strings.TrimSpace(id)] = struct{}{}
		}
	}
	paused := false
	for _, account := range p.accounts {
		if account == nil || !account.Config.Enabled || !p.inPoolLocked(selection.Pool, account) ||
			accountID != "" && account.ID != accountID {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[account.ID]; !ok {
				continue
			}
		}
		if !selection.BuildOnly && p.channelSupportsLocked(account, ChannelPlayground, selection) {
			return false
		}
		if !paused && p.buildPauseActiveLocked(account, selection) && p.channelSupportsLocked(account, ChannelBuild, probe) {
			paused = true
			if selection.BuildOnly {
				return true
			}
		}
	}
	return paused
}

// buildPauseEngagedLocked 判断有没有任何 Build 生成暂停（全局或任一账户）；调用方持有 p.mu
func (p *AccountPool) buildPauseEngagedLocked() bool {
	if p.buildPaused {
		return true
	}
	for _, account := range p.accounts {
		if account != nil && account.buildPaused {
			return true
		}
	}
	return false
}

// buildUnavailableErrorLocked 返回没有候选、原因是 Build 暂停时给客户端的错误（同时满足 ErrNoEligibleAccount，见 NoCandidate）：
// 只走 Build 的选择（输入超过 Playground 上限，上限由 app 层补上）为输入超限；没有启用 Playground，或 Playground 目录列出了该模型
// （只是这次选择范围内的账户经 Playground 接不了）为 Playground 接不了；其余为只能经 Build 调用的模型。调用方持有 p.mu
func (p *AccountPool) buildUnavailableErrorLocked(selection AccountSelection) error {
	modelID := strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	reason := BuildUnavailableModel
	switch {
	case selection.BuildOnly:
		reason = BuildUnavailableInputLimit
	case !p.channelEnabledLocked(ChannelPlayground) || p.hasPlaygroundModelLocked(modelID):
		reason = BuildUnavailableNoPlayground
	}
	return &BuildGenerationUnavailableError{Reason: reason, Model: modelID, NoCandidate: true}
}

// buildGenerationOpenLocked 判断账户的 Build 通道按死槽位与冷却状态的口径能接生成请求：通道启用，且没有全局或该账户的 Build 生成暂停。
// 暂停期间生成只走 Playground，Playground 长时间冷却的账户在该模型上已不可用（与只启用 Playground 时一样算死槽位、显示冷却），
// 不看暂停的剩余时间：到期后由试探决定是否恢复。只启用 Build 时暂停不算：没有别的通道，暂停期间全部账户都会被算作死槽位，
// 驻留 Worker 被全部淘汰，试探也就没有热 Worker 可用。调用方持有 p.mu
func (p *AccountPool) buildGenerationOpenLocked(account *Account) bool {
	if !p.channelEnabledLocked(ChannelBuild) {
		return false
	}
	if !p.channelEnabledLocked(ChannelPlayground) {
		return true
	}
	return !p.buildPaused && (account == nil || !account.buildPaused)
}

// BuildServesModel 判断号池内有启用账户的 Build 通道能生成该模型（只看通道配置与目录，不看暂停、冷却与 Worker；按目录版本记忆，
// 见 modelChannelsInLocked）。Build 生成暂停中挑选试探请求时用它：Build 接不了的模型不拿来试探，免得它们每分钟认领一次、
// 取不到 Build 账号又把下次试探往后推，真正能试探 Build 的请求一直轮不到
func (p *AccountPool) BuildServesModel(modelID string, scope PoolScope) bool {
	if p == nil {
		return false
	}
	model := Model{ID: strings.TrimPrefix(strings.TrimSpace(modelID), "models/"), Methods: []string{"generateContent"}}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.modelChannelsInLocked(model, scope), string(ChannelBuild))
}

// PlaygroundListsModel 判断号池里有账户的 Playground 目录列出了模型（按 ID 或别名，不看账户是否可调度）。
// 只在 Build 暂停后决定改走 Playground 还是按“只能经 Build 调用”结束时使用
func (p *AccountPool) PlaygroundListsModel(modelID string) bool {
	if p == nil {
		return false
	}
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hasPlaygroundModelLocked(modelID)
}
