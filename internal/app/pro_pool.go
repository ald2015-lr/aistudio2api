package app

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// Pro 独立号池（PRO_POOL，默认开启，见 aistudio.PoolScope）：为真时权益为 Pro 的账户单独组成 Pro 号池与 Pro Worker 分区，只服务 /pro 前缀，
// 不借用其他号池；为假时与没有这项设置之前相同（Pro 留在普通号池，ULTRA_INCLUDE_PRO=true 时并入 Ultra 号池），/pro 返回 404。
// 成员规则在账户池里与 ULTRA_INCLUDE_PRO 一起切换（SetPoolRules），/pro 的开关（runtimeManager.proPool）只从账户池读取，
// 与成员规则在同一段 manager.mu 内更新（热更新见 applyLiveFields，构造与启动见 installGeneration）。
// 这里另有三件只在 PRO_POOL 开启时才会出现的事：
//   - ULTRA_INCLUDE_PRO=true 在 PRO_POOL=true 时不生效：启动时与两项之一变化时写一行 WARN（logPoolRuleNotes）；
//   - 默认开启的升级提示：.env 与进程环境变量都没有写 PRO_POOL、开关为默认开启且有 Pro 号池账户时，状态接口给出 pro_pool_notice，
//     运行日志写一行 WARN（变成需要提示时、提示中的设置变化时各写一次，见 checkProPoolNotice）；
//   - 普通路径请求的模型只有 Pro 或 Ultra 号池能服务时，错误带上改用 /pro 或 /ultra 的提示，不为它在后台读目录（见 missingModelError）

// poolRuleState 为号池划分规则生效的开关：includePro 为生效的 ULTRA_INCLUDE_PRO（PRO_POOL=true 时为假），proPool 为 PRO_POOL
type poolRuleState struct {
	includePro bool
	proPool    bool
}

// activeProPool 返回 /pro 前缀当前是否启用（PRO_POOL）：公开 API 在每个 /pro 请求进入时读取，关闭时按 404 返回。
// 与账户池的成员规则一致：读到开启时 Pro 账户已在 Pro 号池
func (manager *runtimeManager) activeProPool() bool {
	return manager.proPool.Load()
}

// generationProPool 返回生成服务实例的账户池是否启用了 Pro 独立号池；不是完整生成服务的实例（测试替身）为否
func generationProPool(generation *runtimeGeneration) bool {
	if generation == nil {
		return false
	}
	admin, ok := generation.admin.(*runtimeAdmin)
	return ok && admin.pool.ProPool()
}

// installGeneration 换上新的生成服务实例（构造 runtimeManager 与每次启动生成服务时都经这里）：在 manager.mu 内替换当前实例，
// 按它的账户池同步 /pro 的开关，并记下 PRO_POOL 是否写在 .env 或进程环境变量里（锁外读取）；返回被换下的实例
func (manager *runtimeManager) installGeneration(next *runtimeGeneration) *runtimeGeneration {
	configured := config.KeyConfigured(manager.configPath, "PRO_POOL")
	manager.mu.Lock()
	defer manager.mu.Unlock()
	previous := manager.current
	manager.current = next
	manager.syncGenerationPoolRules(next, configured)
	return previous
}

// syncGenerationPoolRules 在换上新的生成服务实例后（见 installGeneration，调用方持有 manager.mu）按它的账户池同步 /pro 的开关，
// 并记下 PRO_POOL 是否写在 .env 或进程环境变量里（configured，调用方在锁外读取）
func (manager *runtimeManager) syncGenerationPoolRules(generation *runtimeGeneration, configured bool) {
	manager.proPool.Store(generationProPool(generation))
	if generation == nil {
		return
	}
	if admin, ok := generation.admin.(*runtimeAdmin); ok && admin.service != nil {
		admin.service.proPoolConfigured.Store(configured)
	}
}

// logPoolRuleNotes 写号池开关组合的提示：ULTRA_INCLUDE_PRO=true 在 PRO_POOL=true 时不生效（启动时与两项之一变化时调用，不是每次保存都写）
func logPoolRuleNotes(requests *requestRegistry, cfg config.Config) {
	if requests != nil && cfg.ProPool && cfg.UltraIncludePro {
		requests.log("service", "WARN", "ULTRA_INCLUDE_PRO=true 在 PRO_POOL=true 时不生效：Pro 账户只服务 /pro")
	}
}

// proPoolNoticeMonitor 记下已写进运行日志的升级提示（按开关与 Worker 设置去重，账户可用数的波动不重复写）
type proPoolNoticeMonitor struct {
	mu     sync.Mutex
	logged string
}

// proPoolNotice 返回 PRO_POOL 默认开启的升级提示（见 api.AdminProPoolNotice）；不需要提示时为 nil。proAccounts 为 Pro 号池账户数
func (service *trackedService) proPoolNotice(proAccounts int) *api.AdminProPoolNotice {
	if service == nil || service.pool == nil || service.workers == nil || proAccounts <= 0 ||
		service.proPoolConfigured.Load() || !service.pool.ProPool() {
		return nil
	}
	workers := service.workers
	return newProPoolNotice(proAccounts, workers.bootstrapSummary(), service.pool.UltraIncludeProSetting(), proNoticeCapacity{
		warm: workers.warmTargetFor(aistudio.PoolScopeNormal), maxActive: workers.maxActiveFor(aistudio.PoolScopeNormal),
		ultraWarm: workers.warmTargetFor(aistudio.PoolScopeUltra), ultraMax: workers.maxActiveFor(aistudio.PoolScopeUltra),
		proWarm: workers.warmTargetFor(aistudio.PoolScopePro), proMax: workers.maxActiveFor(aistudio.PoolScopePro),
		startup: workers.warmConcurrencyValue(),
	})
}

// proNoticeCapacity 为升级提示用到的各分区当前常驻与峰值数（WARM_*、ULTRA_*、PRO_*）与 WARM_STARTUP_CONCURRENCY
type proNoticeCapacity struct {
	warm, maxActive, ultraWarm, ultraMax, proWarm, proMax, startup int
}

// newProPoolNotice 给出升级提示的建议值。来源分区为升级前 Pro 账户所在的分区：fromUltra（ULTRA_INCLUDE_PRO 写为 true）时为 Ultra 分区，
// 否则为普通分区。按 Pro 可用账户占它与来源分区可用账户之和的比例分来源分区的常驻与峰值数：Pro 的建议值为分到的份额（不少于默认值），
// 来源分区的建议值为原值减去分到的份额，不少于它的默认值（当前值更小时为当前值），普通分区常驻另不少于 WARM_STARTUP_CONCURRENCY
// （照建议保存能通过校验），峰值不少于常驻数；另一个分区不变
func newProPoolNotice(proAccounts int, summary aistudio.BootstrapSummary, fromUltra bool, capacity proNoticeCapacity) *api.AdminProPoolNotice {
	defaults := config.Default()
	notice := &api.AdminProPoolNotice{
		ProAccounts: proAccounts, Source: aistudio.PoolScopeNormal.String(),
		ProAvailable: summary.AvailableIn(aistudio.PoolScopePro), NormalAvailable: summary.AvailableIn(aistudio.PoolScopeNormal),
		UltraAvailable:  summary.AvailableIn(aistudio.PoolScopeUltra),
		WarmWorkerLimit: capacity.warm, MaxActiveWorkers: capacity.maxActive,
		UltraWarmWorkerLimit: capacity.ultraWarm, UltraMaxActiveWorkers: capacity.ultraMax,
		ProWarmWorkerLimit: capacity.proWarm, ProMaxActiveWorkers: capacity.proMax,
		SuggestedWarmWorkerLimit: capacity.warm, SuggestedMaxActiveWorkers: capacity.maxActive,
		SuggestedUltraWarmWorkerLimit: capacity.ultraWarm, SuggestedUltraMaxActiveWorkers: capacity.ultraMax,
	}
	sourceAvailable, sourceWarm, sourceMax := notice.NormalAvailable, capacity.warm, capacity.maxActive
	floorWarm, floorMax := max(1, capacity.startup, min(capacity.warm, defaults.WarmWorkerLimit)), min(capacity.maxActive, defaults.MaxActiveWorkers)
	suggestedWarm, suggestedMax := &notice.SuggestedWarmWorkerLimit, &notice.SuggestedMaxActiveWorkers
	if fromUltra {
		notice.Source = aistudio.PoolScopeUltra.String()
		sourceAvailable, sourceWarm, sourceMax = notice.UltraAvailable, capacity.ultraWarm, capacity.ultraMax
		floorWarm, floorMax = min(capacity.ultraWarm, defaults.UltraWarmWorkerLimit), max(1, min(capacity.ultraMax, defaults.UltraMaxActiveWorkers))
		suggestedWarm, suggestedMax = &notice.SuggestedUltraWarmWorkerLimit, &notice.SuggestedUltraMaxActiveWorkers
	}
	share := 0.0
	if total := notice.ProAvailable + sourceAvailable; total > 0 {
		share = float64(notice.ProAvailable) / float64(total)
	}
	warmShare, maxShare := int(math.Round(share*float64(sourceWarm))), int(math.Round(share*float64(sourceMax)))
	notice.SuggestedProWarmWorkerLimit = max(defaults.ProWarmWorkerLimit, warmShare)
	notice.SuggestedProMaxActiveWorkers = max(defaults.ProMaxActiveWorkers, maxShare, notice.SuggestedProWarmWorkerLimit)
	*suggestedWarm = min(sourceWarm, max(floorWarm, sourceWarm-warmShare))
	*suggestedMax = max(*suggestedWarm, floorMax, sourceMax-maxShare)
	return notice
}

// proPoolNoticeLine 返回升级提示的运行日志（WARN）：来源分区为 Ultra 时建议减 ULTRA_*，否则减 WARM_WORKER_LIMIT、MAX_ACTIVE_WORKERS
func proPoolNoticeLine(notice *api.AdminProPoolNotice) string {
	source := fmt.Sprintf("Pro 可用账户 %d，普通可用账户 %d），普通分区可相应减为 WARM_WORKER_LIMIT=%d、MAX_ACTIVE_WORKERS=%d",
		notice.ProAvailable, notice.NormalAvailable, notice.SuggestedWarmWorkerLimit, notice.SuggestedMaxActiveWorkers)
	if notice.Source == aistudio.PoolScopeUltra.String() {
		source = fmt.Sprintf("ULTRA_INCLUDE_PRO=true，升级前 Pro 账户在 Ultra 分区；Pro 可用账户 %d，Ultra 可用账户 %d），Ultra 分区可相应减为 "+
			"ULTRA_WARM_WORKER_LIMIT=%d、ULTRA_MAX_ACTIVE_WORKERS=%d",
			notice.ProAvailable, notice.UltraAvailable, notice.SuggestedUltraWarmWorkerLimit, notice.SuggestedUltraMaxActiveWorkers)
	}
	return fmt.Sprintf(
		"PRO_POOL 默认开启：%d 个 Pro 账户只服务 /pro 前缀，默认地址与 /ultra 不再使用它们 | Pro 常驻/峰值 Worker=%d/%d | "+
			"建议 PRO_WARM_WORKER_LIMIT=%d、PRO_MAX_ACTIVE_WORKERS=%d（%s | 要保持升级前的行为请设置 PRO_POOL=false，写明 PRO_POOL 后不再提示",
		notice.ProAccounts, notice.ProWarmWorkerLimit, notice.ProMaxActiveWorkers,
		notice.SuggestedProWarmWorkerLimit, notice.SuggestedProMaxActiveWorkers, source,
	)
}

// checkProPoolNotice 在需要升级提示时写一行 WARN：变成需要提示时写一次，提示中的 Worker 设置或来源分区变化时再写一次；不再需要时清掉记录，
// 之后再出现时重新写。由后台周期（keepWarm）与配置热更新调用，不在请求路径上
func (service *trackedService) checkProPoolNotice() {
	if service == nil || service.pool == nil || service.requests == nil {
		return
	}
	notice := service.proPoolNotice(service.pool.PartitionMembers().Count(aistudio.PoolScopePro))
	key := ""
	if notice != nil {
		key = fmt.Sprintf("%s/%d/%d/%d/%d/%d/%d", notice.Source, notice.WarmWorkerLimit, notice.MaxActiveWorkers,
			notice.UltraWarmWorkerLimit, notice.UltraMaxActiveWorkers, notice.ProWarmWorkerLimit, notice.ProMaxActiveWorkers)
	}
	monitor := &service.proPoolNotes
	monitor.mu.Lock()
	changed := key != monitor.logged
	monitor.logged = key
	monitor.mu.Unlock()
	if changed && notice != nil {
		service.requests.log("service", "WARN", proPoolNoticeLine(notice))
	}
}

// missingModelError 在选号以“目录中没有请求模型”或“不在号池的模型列表里”失败后调用，返回给调用方的错误：
// 普通路径（普通号池，或 PRO_POOL=true、ULTRA_EXCLUSIVE=false 时的普通号池与 Ultra 号池）请求的模型只有 Pro 或 Ultra 号池能服务时
// 包成 aistudio.ModelInOtherPoolError（对外仍是 404，说明里加一句改用哪个入口），不为它在后台读目录（其他号池已经列出，读了也不会出现在
// 这个号池）；否则照旧交给 noteMissingModel 限频地读目录。只在错误路径上对账户池多加锁
func (service *trackedService) missingModelError(selection aistudio.AccountSelection, err error) error {
	if hinted := service.otherPoolHint(selection, err); hinted != nil {
		return hinted
	}
	service.noteMissingModel(selection, err)
	return err
}

// otherPoolHint 判断普通路径上没有账户能服务的请求是否由 Pro 或 Ultra 号池服务，是则返回带提示的错误，否则为 nil。
// 按选择的号池（selection.Pool）、模型、方法与能力判断：模型在目录里但不支持请求的方法或能力时其他号池同样接不了，不提示。
// 默认地址的别名目录属于普通号池，带思考等级、-nothinking、-online 后缀或 long- 前缀的付费模型名（如 gemini-4-argon-medium）在 api 层
// 解析不了、原样进入选号，这里按同一套写法去掉后缀再找（api.RequestModelBases）。
// 只在 PRO_POOL=true 时提示（无锁读取）：PRO_POOL=false 时错误文案与后台读目录都与没有这项设置之前相同
func (service *trackedService) otherPoolHint(selection aistudio.AccountSelection, err error) error {
	scope := selection.Pool
	if service == nil || service.pool == nil || err == nil || scope != aistudio.PoolScopeNormal && scope != aistudio.PoolScopeNormalUltra ||
		!service.pool.ProPool() {
		return nil
	}
	var noServing *aistudio.NoServingAccountError
	unlisted := errors.As(err, &noServing) && !noServing.Listed
	if !unlisted && !errors.Is(err, aistudio.ErrModelNotFound) {
		return nil
	}
	modelID := strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	if modelID == "" {
		return nil
	}
	pools := service.pool.OtherPoolsServing(selection, api.RequestModelBases(modelID))
	if len(pools) == 0 {
		return nil
	}
	return &aistudio.ModelInOtherPoolError{Err: err, Model: modelID, Pools: pools}
}
