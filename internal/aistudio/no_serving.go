package aistudio

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// 生成选号没有候选（NoCandidateFailure：裸 ErrNoEligibleAccount，不是号池暂时不可调度、Build 暂停或需要 Drive）时的核对与对外说明。
//
// 原先这种情况与“号池里没有该模型”一样按 404 model_not_found 返回（官方措辞 “Call ListModels to see the list of available models”），
// 请求日志只有“请求失败 | 错误=没有符合条件的 AI Studio 账户”，看不出原因。线上 Omni 只带图片的请求同一秒以这个错误结束、纯文字请求正常，
// 根因尚未查明：选号条件与请求内容无关（同一号池状态下文字与图片的选号结果相同，见 app 层 TestOmniImageOnlySelectsLikeTextInProduction），
// 能造成同一秒裸 ErrNoEligibleAccount 的只有两类状态：目录列出模型的账户在所选通道上都因权益、方法、号池或通道接不了（文字请求同样失败），
// 或候选索引与账户状态不一致（假设，没有找到会造成它的代码路径）。这里的核对与重试是诊断与兜底，不是根因修复：
// app 层只在这次没有候选会成为请求的最终错误时（之前没有失败的尝试）调用 NoCandidateReport 核对一次（同一选择条件与目录版本下的结果缓存几秒，
// 见 noCandidateCacheTTL：配置错误时每个请求都没有候选，不在账户池锁内一遍遍遍历全部账户；换号用尽时结果用不上，不核对），按核对结果处理：
//   - 逐个账户核对（classifyCandidatesBruteLocked，与候选索引同口径的参考实现）还有候选（Recheckable）：候选索引与账户状态不一致，或核对前
//     账户状态刚变化。候选索引没有而逐个核对有时作废候选索引（下次选号重建），app 层按同样的条件重试一次选号（输入超限后的再试除外），
//     不占换号次数，按 ERROR 写一行摘要；
//   - 模型不在这个号池的模型列表里（与 GET /v1/models 同口径，见 EligibleModelsIn）：目录列出它的账户都因权益、方法或启用的上游通道
//     接不了，属于长期状态，按 404 NOT_FOUND 返回写明原因类别的英文说明，客户端与中转不必重试；
//   - 模型在列、重试后仍没有候选：暂时性原因，按 503 UNAVAILABLE 与 Retry-After 返回。
//
// 指定账户或绑定文件的选号不经这里，照原来的错误返回。线上定位时在同一秒内先后发一次文字、一次图片请求，对照失败请求的核对摘要与
// 成功请求的“Omni 选号诊断”（见 CandidateIndexSummary）

const (
	// noCandidateReportTTL 为模型不在模型列表里时核对结果的缓存时间：同一选择条件（含限定与排除）、账户与目录版本不变时沿用，不重新遍历账户。
	// 是否在列只取决于目录、权益、启用状态与上游通道，变化时版本都会变
	noCandidateReportTTL = 5 * time.Second
	// noCandidateListedTTL 为模型在列时核对结果的缓存时间：候选还取决于账户是否就绪、冷却等不改版本的状态，只缓存很短的时间
	noCandidateListedTTL = time.Second
	// noCandidateReportMax 为缓存的核对结果数上限，超过时整个清空
	noCandidateReportMax = 64
	// noServingRetryAfter 为模型在列、暂时没有账户能接时建议客户端重试的间隔（写 Retry-After）
	noServingRetryAfter = 5 * time.Second
)

// NoCandidateReport 为生成选号没有候选时的核对结果（不含账户，写管理日志）
type NoCandidateReport struct {
	Model string
	// Pool 为号池中文名（不限号池时为空）；Channels 为这次选择可用的通道（受 Build 暂停影响），EnabledChannels 为配置启用的上游通道；
	// Flags 为选择条件；Allowed、Excluded 为限定与排除的账户数
	Pool            string
	Channels        []string
	EnabledChannels []string
	Flags           []string
	Allowed         int
	Excluded        int
	// Listed 为号池里目录列出该模型的账户数；其后为这些账户接不了的原因分布，按先后只计一项：停用、不可调度（需要登录等）、不在这次选号范围
	// （指定账户、限定或排除列表）、按目录与通道能接（Servable）、权益不符、方法不符、这次可用的通道都不支持
	Listed, Disabled, NotReady, Scoped, Servable, Tier, Method, Unsupported int
	// Draining 为目录列出的账户里排空中的（分类时按忙碌处理，不会造成没有候选），AccountPaused 为 Build 暂停的账户数
	Draining, AccountPaused int
	// PlaygroundEnabled、BuildEnabled 为两个上游通道是否启用，BuildPaused 为 Build 生成全局暂停
	PlaygroundEnabled, BuildEnabled, BuildPaused bool
	// ModelListed 为模型在这个号池的模型列表里（与 GET /v1/models 同口径：有启用的号池账户能经某个启用的通道调用，不看暂停与冷却）
	ModelListed bool
	// IndexFound 为候选索引里有这个选择的候选集，IndexAge 为它建立了多久，IndexMembers、IndexAvailable、IndexCooling 为成员、未冷却与
	// 冷却的数量，IndexVersion、Version 为它建立时与现在的目录版本
	IndexFound                                 bool
	IndexAge                                   time.Duration
	IndexMembers, IndexAvailable, IndexCooling int
	IndexVersion, Version                      uint64
	// IndexCandidates、BruteCandidates 为核对时按候选索引与逐个账户核对得到的候选数（未冷却的），IndexCoolingFound、BruteCoolingFound
	// 为是否另有冷却中的候选
	IndexCandidates, BruteCandidates     int
	IndexCoolingFound, BruteCoolingFound bool
	// Rebuilt 为这次核对发现候选索引没有、逐个账户核对还有候选，已作废候选索引（下次选号重建）
	Rebuilt bool
	// CachedAge 为沿用缓存时距那次核对的时间，没有沿用时为 0
	CachedAge time.Duration
}

// noCandidateCacheEntry 为缓存的核对结果与核对时的版本
type noCandidateCacheEntry struct {
	report                                              NoCandidateReport
	at                                                  time.Time
	accountsVersion, catalogVersion, eligibilityVersion uint64
}

// Recheckable 判断逐个账户核对还有候选（含冷却中的）：选号没有候选是候选索引与账户状态不一致或状态刚变化造成的，值得重试一次选号
func (report NoCandidateReport) Recheckable() bool {
	return report.BruteCandidates > 0 || report.BruteCoolingFound
}

// Text 返回写进管理日志的原因摘要（中文，不含账户）
func (report NoCandidateReport) Text() string {
	join := func(values []string) string {
		if len(values) == 0 {
			return "无"
		}
		return strings.Join(values, ",")
	}
	pool := report.Pool
	if pool == "" {
		pool = "不限"
	}
	listed := "不列出"
	if report.ModelListed {
		listed = "列出"
	}
	index := CandidateIndexSummary{
		Found: report.IndexFound, Age: report.IndexAge, Members: report.IndexMembers, Available: report.IndexAvailable,
		Cooling: report.IndexCooling, Version: report.IndexVersion, Current: report.Version,
	}.Text()
	cooling := func(found bool) string {
		if found {
			return "（另有冷却中的）"
		}
		return ""
	}
	playground := "未启用"
	if report.PlaygroundEnabled {
		playground = "启用"
	}
	build := "未启用"
	if report.BuildEnabled {
		build = "启用"
		switch {
		case report.BuildPaused:
			build += "、全局暂停"
		case report.AccountPaused > 0:
			build += fmt.Sprintf("、%d 个账户暂停", report.AccountPaused)
		}
	}
	text := fmt.Sprintf(
		"模型=%s | 号池=%s | 可用通道=%s | 条件=%s | 限定=%d | 排除=%d | 目录列出=%d | 停用=%d | 不可调度=%d | 不在选号范围=%d | "+
			"权益不符=%d | 方法不符=%d | 所选通道不支持=%d | 可接=%d | 排空中=%d | 模型列表=%s | 候选索引=%s | 核对候选=索引 %d%s、逐个账户 %d%s | "+
			"Playground=%s | Build=%s",
		report.Model, pool, join(report.Channels), join(report.Flags), report.Allowed, report.Excluded, report.Listed, report.Disabled,
		report.NotReady, report.Scoped, report.Tier, report.Method, report.Unsupported, report.Servable, report.Draining, listed, index,
		report.IndexCandidates, cooling(report.IndexCoolingFound), report.BruteCandidates, cooling(report.BruteCoolingFound), playground, build,
	)
	if report.Rebuilt {
		text += " | 已作废候选索引（下次选号重建）"
	}
	if report.CachedAge > 0 {
		text += fmt.Sprintf(" | 沿用 %s 前的核对", report.CachedAge.Round(time.Millisecond))
	}
	return text
}

// UnlistedReason 返回模型不在这个号池的模型列表里时给客户端的英文原因（不含账户）：只有一种原因时写明是权益、方法还是启用的上游通道
func (report NoCandidateReport) UnlistedReason() string {
	switch {
	case report.Listed == 0:
		return "no account in this pool lists this model in its catalog"
	case report.Tier > 0 && report.Method == 0 && report.Unsupported == 0:
		return "no account in this pool has a subscription tier that allows this model"
	case report.Method > 0 && report.Tier == 0 && report.Unsupported == 0:
		return "no account in this pool supports generating content with this model"
	case report.Unsupported > 0 && report.Tier == 0 && report.Method == 0:
		channels := "none"
		if len(report.EnabledChannels) > 0 {
			channels = strings.Join(report.EnabledChannels, ", ")
		}
		return "the enabled upstream channels (" + channels + ") cannot serve this model"
	}
	return "no account in this pool can serve this model with its subscription tier and the enabled upstream channels"
}

// NoServingAccountError 表示不固定账户的生成选号没有候选（见文件开头）：Summary 为核对摘要（中文，只写管理日志）；Listed 为模型在这个号池的
// 模型列表里——为真时是暂时性原因，按 503 UNAVAILABLE 与 Retry-After（Until）返回；为假时是长期状态（权益、方法或启用的上游通道），按 404
// NOT_FOUND 返回写明原因类别（Reason，英文）的说明，与 GET /v1/models 不列出它一致。
// 同时满足 errors.Is(err, ErrNoEligibleAccount)，换号、额度与文件复制等按“没有可用账户”的原规则处理不变
type NoServingAccountError struct {
	Model   string
	Summary string
	Listed  bool
	Reason  string
	Until   time.Time
}

// NewNoServingAccountError 按核对结果构造没有候选的错误；now 为结束请求的时刻（模型在列时据此给出建议的重试时间）
func NewNoServingAccountError(report NoCandidateReport, now time.Time) *NoServingAccountError {
	err := &NoServingAccountError{Model: report.Model, Summary: report.Text(), Listed: report.ModelListed}
	if report.ModelListed {
		err.Until = now.Add(noServingRetryAfter)
	} else {
		err.Reason = report.UnlistedReason()
	}
	return err
}

func (e *NoServingAccountError) Error() string {
	message := ErrNoEligibleAccount.Error() + "：模型 " + strings.TrimSpace(e.Model)
	if e.Listed {
		message += " 在号池的模型列表里，但这次选号没有账户能接这个请求"
	} else {
		message += " 不在号池的模型列表里（目录列出它的账户都因权益、方法或启用的上游通道接不了）"
	}
	if summary := strings.TrimSpace(e.Summary); summary != "" {
		message += "（" + summary + "）"
	}
	return message
}

// Unwrap 保留无账户哨兵
func (e *NoServingAccountError) Unwrap() error {
	return ErrNoEligibleAccount
}

// HTTPStatus 返回对外状态码：模型在列为 503，不在列为 404
func (e *NoServingAccountError) HTTPStatus() int {
	if e.Listed {
		return http.StatusServiceUnavailable
	}
	return http.StatusNotFound
}

// RPCStatus 返回对外的 Google RPC 状态名：模型在列为 UNAVAILABLE，不在列为 NOT_FOUND
func (e *NoServingAccountError) RPCStatus() string {
	if e.Listed {
		return "UNAVAILABLE"
	}
	return "NOT_FOUND"
}

// RetryAt 返回建议的重试时间（写 Retry-After）：只在模型在列时有
func (e *NoServingAccountError) RetryAt() time.Time {
	if !e.Listed {
		return time.Time{}
	}
	return e.Until
}

// PublicMessage 返回给客户端的英文说明（不含账户信息与内部摘要）
func (e *NoServingAccountError) PublicMessage() string {
	model := strings.TrimPrefix(strings.TrimSpace(e.Model), "models/")
	if e.Listed {
		if model == "" {
			return "The requested model is listed, but no account in the pool can serve this request right now. Please try again later."
		}
		return "Model " + model + " is listed, but no account in the pool can serve this request right now. Please try again later."
	}
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		reason = "no account in this pool can serve this model"
	}
	name := "The requested model"
	if model != "" {
		name = "models/" + model
	}
	return name + " is not available to the accounts in this pool: " + reason + ". Call ListModels to see the list of available models."
}

// NoCandidateFailure 判断选号错误只是“没有候选”：满足 ErrNoEligibleAccount，且不是号池暂时不可调度（AccountsNotReadyError，
// 已列出原因、按 503 返回）、Build 暂停造成的没有候选（BuildGenerationUnavailableError）或已整理过的 NoServingAccountError
func NoCandidateFailure(err error) bool {
	if !errors.Is(err, ErrNoEligibleAccount) {
		return false
	}
	var notReady *AccountsNotReadyError
	var buildUnavailable *BuildGenerationUnavailableError
	var noServing *NoServingAccountError
	return !errors.As(err, &notReady) && !errors.As(err, &buildUnavailable) && !errors.As(err, &noServing)
}

// NoCandidateReport 核对生成选号没有候选的原因（见文件开头）：同一选择条件（含限定、排除、指定账户与资源）在缓存时间内（模型不在列为
// noCandidateReportTTL，在列为 noCandidateListedTTL）、账户与目录版本不变时沿用上次的结果（CachedAge 为距那次核对的时间），沿用的结果逐个核对
// 还有候选时只按候选索引再分类一次，仍没有就再作废候选索引；否则在账户池锁内遍历一次全部账户，统计目录列出该模型的账户与接不了的原因，
// 按候选索引与逐个账户各分类一次核对候选，候选索引没有而逐个核对有时作废候选索引（Rebuilt）。只在选号没有候选时调用
func (p *AccountPool) NoCandidateReport(selection AccountSelection) NoCandidateReport {
	selection.ModelID = strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	if p == nil {
		return NoCandidateReport{Model: selection.ModelID}
	}
	key := noCandidateKey(selection)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.noCandidateReports[key]; ok && entry.accountsVersion == p.accountsVersion && entry.catalogVersion == p.catalogVersion &&
		entry.eligibilityVersion == p.eligibilityVersion && now.Sub(entry.at) < noCandidateCacheTTL(entry.report) {
		report := entry.report
		report.Rebuilt = false
		report.CachedAge = max(now.Sub(entry.at), time.Millisecond)
		if report.Recheckable() {
			// 沿用的核对说逐个账户还有候选：只按候选索引再分类一次（不遍历全部账户），候选索引仍没有就再作废，不让缓存挡住重建
			if groups, err := p.classifyCandidatesAtLocked(selection, nil, now); err == nil {
				report.IndexCandidates, report.IndexCoolingFound = candidateGroupCount(groups), !groups.EarliestCooldown.IsZero()
			}
			if report.IndexCandidates == 0 && !report.IndexCoolingFound {
				p.invalidateSchedulingLocked()
				report.Rebuilt = true
			}
		}
		return report
	}
	report := p.noCandidateReportLocked(selection, now)
	if p.noCandidateReports == nil || len(p.noCandidateReports) >= noCandidateReportMax {
		p.noCandidateReports = make(map[string]noCandidateCacheEntry)
	}
	// 作废候选索引不改变版本：同一选择条件在缓存期内不再重复作废
	p.noCandidateReports[key] = noCandidateCacheEntry{
		report: report, at: now,
		accountsVersion: p.accountsVersion, catalogVersion: p.catalogVersion, eligibilityVersion: p.eligibilityVersion,
	}
	return report
}

// noCandidateCacheTTL 返回核对结果的缓存时间：模型在列时很短，不在列时较长
func noCandidateCacheTTL(report NoCandidateReport) time.Duration {
	if report.ModelListed {
		return noCandidateListedTTL
	}
	return noCandidateReportTTL
}

// noCandidateKey 为核对结果缓存的键：影响候选的全部选择条件，限定与排除列表排序后参与
func noCandidateKey(selection AccountSelection) string {
	sorted := func(values []string) string {
		if values == nil {
			return "-"
		}
		copied := slices.Clone(values)
		slices.Sort(copied)
		return strings.Join(copied, ",")
	}
	return fmt.Sprintf("%+v|%s|%s|%s|%s", selectionKeyOf(selection), strings.TrimSpace(selection.AccountID), selection.ResourceID,
		sorted(selection.AllowedAccountIDs), sorted(selection.ExcludedAccountIDs))
}

// noCandidateReportLocked 为 NoCandidateReport 的一次核对；调用方持有 p.mu，ModelID 已去掉前缀
func (p *AccountPool) noCandidateReportLocked(selection AccountSelection, now time.Time) NoCandidateReport {
	report := NoCandidateReport{
		Model: selection.ModelID, Pool: selection.Pool.Label(), Allowed: len(selection.AllowedAccountIDs), Excluded: len(selection.ExcludedAccountIDs),
		PlaygroundEnabled: p.channelEnabledLocked(ChannelPlayground), BuildEnabled: p.channelEnabledLocked(ChannelBuild), BuildPaused: p.buildPaused,
		Version: p.eligibilityVersion,
	}
	for _, channel := range p.selectionChannelsLocked(selection) {
		report.Channels = append(report.Channels, string(channel))
	}
	for _, channel := range p.enabledChannelsLocked() {
		report.EnabledChannels = append(report.EnabledChannels, string(channel))
	}
	report.Flags = selectionFlags(selection)
	var allowed map[string]struct{}
	if selection.AllowedAccountIDs != nil {
		allowed = make(map[string]struct{}, len(selection.AllowedAccountIDs))
		for _, accountID := range selection.AllowedAccountIDs {
			allowed[strings.TrimSpace(accountID)] = struct{}{}
		}
	}
	excluded := excludedAccountSet(selection)
	accountID := strings.TrimSpace(selection.AccountID)
	for _, account := range p.accounts {
		if account == nil || !p.inPoolLocked(selection.Pool, account) {
			continue
		}
		model, inPlayground := accountModelLocked(account, selection.ModelID)
		_, inBuild := accountBuildGeneratesLocked(account)[selection.ModelID]
		if selection.ModelID != "" && !inPlayground && !inBuild {
			continue
		}
		report.Listed++
		if account.buildPaused {
			report.AccountPaused++
		}
		if account.draining {
			report.Draining++
		}
		_, inAllowed := allowed[account.ID]
		_, isExcluded := excluded[account.ID]
		switch {
		case !account.Config.Enabled:
			report.Disabled++
		case account.schedulingState() != AccountReady:
			report.NotReady++
		case accountID != "" && account.ID != accountID || allowed != nil && !inAllowed || isExcluded:
			report.Scoped++
		case p.accountSupportsAnyChannelLocked(account, selection):
			report.Servable++
		case inPlayground && !modelAllowedByTier(model, account.BenefitTier):
			report.Tier++
		case inPlayground && strings.TrimSpace(selection.Method) != "" && !hasMethod(model, selection.Method):
			report.Method++
		default:
			report.Unsupported++
		}
	}
	if selection.ModelID != "" {
		listing := Model{ID: selection.ModelID}
		if method := strings.TrimSpace(selection.Method); method != "" {
			listing.Methods = []string{method}
		}
		report.ModelListed = len(p.modelChannelsInLocked(listing, selection.Pool)) > 0
	}
	// 先记下候选索引里现有的候选集（不建立），再按候选索引与逐个账户各分类一次
	if accountID == "" && selection.ResourceID == "" {
		index := p.candidateIndexSummaryLocked(selection, now)
		report.IndexFound, report.IndexAge, report.IndexVersion = index.Found, index.Age, index.Version
		report.IndexMembers, report.IndexAvailable, report.IndexCooling = index.Members, index.Available, index.Cooling
	}
	if groups, err := p.classifyCandidatesAtLocked(selection, nil, now); err == nil {
		report.IndexCandidates, report.IndexCoolingFound = candidateGroupCount(groups), !groups.EarliestCooldown.IsZero()
	}
	if groups, err := p.classifyCandidatesBruteLocked(selection, nil, now); err == nil {
		report.BruteCandidates, report.BruteCoolingFound = candidateGroupCount(groups), !groups.EarliestCooldown.IsZero()
	}
	if report.Recheckable() && report.IndexCandidates == 0 && !report.IndexCoolingFound {
		p.invalidateSchedulingLocked()
		report.Rebuilt = true
	}
	return report
}

// candidateGroupCount 返回分组里未冷却的候选数
func candidateGroupCount(groups AccountCandidateGroups) int {
	return len(groups.WarmReady) + len(groups.WarmAvailable) + len(groups.WarmBusy) + len(groups.StandbyReady) + len(groups.StandbyBusy)
}

// selectionFlags 返回选择条件里影响候选的标记（写进核对摘要与 Omni 选号诊断）
func selectionFlags(selection AccountSelection) []string {
	var flags []string
	for _, flag := range []struct {
		on   bool
		name string
	}{
		{selection.PlaygroundOnly, "PlaygroundOnly"}, {selection.BuildOnly, "BuildOnly"}, {selection.PlaygroundFirst, "PlaygroundFirst"},
		{selection.PreferPlayground, "PreferPlayground"}, {selection.BuildProbe, "BuildProbe"},
		{strings.TrimSpace(selection.AccountID) != "", "指定账户"}, {strings.TrimSpace(selection.ResourceID) != "", "绑定文件"},
	} {
		if flag.on {
			flags = append(flags, flag.name)
		}
	}
	return flags
}

// SelectionFlagsText 返回选择条件里影响候选的标记（PlaygroundFirst、BuildProbe 等，以逗号分隔），没有时为“无”
func SelectionFlagsText(selection AccountSelection) string {
	if flags := selectionFlags(selection); len(flags) > 0 {
		return strings.Join(flags, ",")
	}
	return "无"
}

// CandidateIndexSummary 为一个选择条件在候选索引里的候选集概况（见 AccountPool.CandidateIndexSummary）：Found 为候选索引里有这个候选集，
// Age 为它建立了多久，Members、Available、Cooling 为成员、未冷却与冷却的数量，Version、Current 为它建立时与现在的目录版本
type CandidateIndexSummary struct {
	Found                       bool
	Age                         time.Duration
	Members, Available, Cooling int
	Version, Current            uint64
}

// Text 返回写进管理日志的概况，没有这个候选集时为“无”
func (summary CandidateIndexSummary) Text() string {
	if !summary.Found {
		return "无"
	}
	return fmt.Sprintf("建立于 %s 前、成员 %d（可用 %d、冷却 %d）、目录版本 %d/%d",
		summary.Age.Round(time.Millisecond), summary.Members, summary.Available, summary.Cooling, summary.Version, summary.Current)
}

// CandidateIndexSummary 返回选择条件在候选索引里的候选集概况：只读，不建立、不重建，也不分类候选；指定账户或绑定文件的选择不用候选集，
// 返回零值（Found 为假）。供 app 层 Omni 请求取得租约时写临时诊断，与没有候选时的核对摘要对照（线上只带图片的 Omni 请求没有候选、纯文字正常，
// 原因未查明，见文件开头）；在账户池锁内只查一次表
func (p *AccountPool) CandidateIndexSummary(selection AccountSelection) CandidateIndexSummary {
	if p == nil || strings.TrimSpace(selection.AccountID) != "" || selection.ResourceID != "" {
		return CandidateIndexSummary{}
	}
	selection.ModelID = strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.candidateIndexSummaryLocked(selection, now)
}

// candidateIndexSummaryLocked 为 CandidateIndexSummary 的实现；调用方持有 p.mu，ModelID 已去掉前缀
func (p *AccountPool) candidateIndexSummaryLocked(selection AccountSelection, now time.Time) CandidateIndexSummary {
	summary := CandidateIndexSummary{Current: p.eligibilityVersion}
	if p.sched == nil {
		return summary
	}
	set := p.sched.sets[selectionKeyOf(selection)]
	if set == nil {
		return summary
	}
	summary.Found, summary.Age, summary.Version = true, now.Sub(set.builtAt), set.eligibilityVersion
	summary.Members, summary.Available, summary.Cooling = set.members, len(set.available), len(set.cooling)
	return summary
}
