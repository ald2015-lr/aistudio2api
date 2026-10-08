package app

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// restartDrainTimeout 为应用新配置前等待进行中请求结束的最长时间
const restartDrainTimeout = 30 * time.Second

// apiKeyHolder 保存当前生效的公开 API 密钥，可在运行中原子替换
type apiKeyHolder struct {
	value atomic.Pointer[string]
}

func newAPIKeyHolder(key string) *apiKeyHolder {
	holder := &apiKeyHolder{}
	key = config.EffectiveProxyAPIKey(key)
	holder.value.Store(&key)
	return holder
}

func (holder *apiKeyHolder) get() string {
	if holder == nil {
		return ""
	}
	if value := holder.value.Load(); value != nil {
		return *value
	}
	return ""
}

func (holder *apiKeyHolder) set(key string) {
	key = config.EffectiveProxyAPIKey(key)
	holder.value.Store(&key)
}

// activeAPIKey 返回公开 API 当前使用的密钥
func (manager *runtimeManager) activeAPIKey() string {
	return manager.apiKey.get()
}

// applyAPIKey 立即切换公开 API 密钥，无需重启管理进程
func (manager *runtimeManager) applyAPIKey(key string) {
	key = config.EffectiveProxyAPIKey(key)
	if key == manager.apiKey.get() {
		return
	}
	manager.apiKey.set(key)
	manager.mu.Lock()
	manager.activeManagement.ProxyAPIKey = key
	manager.mu.Unlock()
	manager.requests.log("service", "INFO", "API 密钥已更新并立即生效")
}

// RestartService 等待进行中的请求结束（最多 restartDrainTimeout）后重建生成服务，
// 使已保存的配置与账户目录的变化立即生效；服务未运行时只返回当前状态
func (manager *runtimeManager) RestartService(ctx context.Context) (api.AdminStatus, error) {
	status, err := manager.Status(ctx)
	if err != nil {
		return api.AdminStatus{}, err
	}
	if status.State == "STOPPED" {
		return status, nil
	}
	// 等待期间用户按了停止或进程开始退出时不再重启：原先等完之后照常停止再启动，把用户的停止覆盖掉
	stops := manager.intent.stops.Load()
	if active := manager.requests.count(); active > 0 {
		manager.requests.log("service", "INFO", "应用新配置 | 等待进行中的请求结束")
	}
	deadline := time.Now().Add(restartDrainTimeout)
	for manager.requests.count() > 0 && time.Now().Before(deadline) {
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.AdminStatus{}, ctx.Err()
		case <-timer.C:
		}
	}
	manager.mu.RLock()
	abort := manager.shuttingDown || manager.intent.stops.Load() != stops || !manager.intent.running.Load()
	manager.mu.RUnlock()
	if abort {
		return manager.Status(ctx)
	}
	// 进入重启后不再受页面请求取消影响，避免停在已停止状态
	detached := context.WithoutCancel(ctx)
	manager.requests.log("service", "INFO", "应用新配置 | 重启生成服务")
	// 重启内部的停止不改变用户期望的运行状态；随后的启动不会覆盖期间用户按下的停止
	if _, err := manager.stopCurrent(detached); err != nil {
		return api.AdminStatus{}, err
	}
	return manager.startService(detached, false)
}

// ---------- 配置热更新 ----------

func (manager *accountWorkerManager) warmTargetValue() int {
	return int(manager.warmTarget.Load())
}

func (manager *accountWorkerManager) maxActiveValue() int {
	return int(manager.maxActive.Load())
}

func (manager *accountWorkerManager) warmConcurrencyValue() int {
	return int(manager.warmConcurrency.Load())
}

// applyLiveSettings 更新 Worker 容量与启动参数；调小的常驻数由空闲回收逐步收敛，
// 冷启动名额立即按新的启动预热并发放行排队的启动
func (manager *accountWorkerManager) applyLiveSettings(cfg config.Config) {
	manager.warmTarget.Store(int64(cfg.WarmWorkerLimit))
	manager.maxActive.Store(int64(cfg.MaxActiveWorkers))
	manager.warmConcurrency.Store(int64(cfg.WarmStartupConcurrency))
	manager.startupSlots.resized()
	manager.initTimeout.Store(int64(cfg.InitTimeout))
	manager.temporaryChat.Store(cfg.TemporaryChat)
}

// prewarmIfRunning 服务运行中时按最新常驻数补齐预热 Worker
func (service *trackedService) prewarmIfRunning() {
	service.lifecycleMu.Lock()
	dataContext := service.dataContext
	running := service.state.Load() == serviceRunning && dataContext != nil && dataContext.Err() == nil
	service.lifecycleMu.Unlock()
	if running {
		service.workers.StartPrewarm(dataContext)
	}
}

// requiresRebuild 判断配置差异是否只能通过重建生成服务生效：
// 账户目录、默认代理、WAA 后端与上游通道决定了运行时的装配方式，其余字段都可热更新
func requiresRebuild(saved config.Config, active config.Config) bool {
	return saved.AuthStates != active.AuthStates || saved.Proxy != active.Proxy ||
		saved.WAABackend != active.WAABackend || !slices.Equal(saved.UpstreamChannels, active.UpstreamChannels)
}

// liveFieldsEqual 判断可热更新的字段是否一致
func liveFieldsEqual(saved config.Config, active config.Config) bool {
	return saved.InitTimeout == active.InitTimeout && saved.RequestTimeout == active.RequestTimeout &&
		saved.FirstEventTimeout == active.FirstEventTimeout &&
		saved.WarmWorkerLimit == active.WarmWorkerLimit && saved.MaxActiveWorkers == active.MaxActiveWorkers &&
		saved.WarmStartupConcurrency == active.WarmStartupConcurrency &&
		saved.PerAccountConcurrency == active.PerAccountConcurrency &&
		saved.RoutingStrategy == active.RoutingStrategy && saved.TemporaryChat == active.TemporaryChat &&
		saved.IgnoreClientSeed == active.IgnoreClientSeed && saved.RepeatPromptNonce == active.RepeatPromptNonce &&
		saved.MinOutputTokens == active.MinOutputTokens && saved.DowngradeGuard.Equal(active.DowngradeGuard)
}

// applyLiveConfig 读取已保存配置，把可热更新的字段直接应用到当前生成服务，不中断任何请求
func (manager *runtimeManager) applyLiveConfig() {
	saved, err := config.Load(manager.configPath)
	if err != nil {
		return
	}
	manager.overrides.Apply(&saved)

	manager.mu.Lock()
	generation := manager.current
	if generation == nil || requiresRebuild(saved, generation.config) || liveFieldsEqual(saved, generation.config) {
		manager.mu.Unlock()
		return
	}
	admin, ok := generation.admin.(*runtimeAdmin)
	if !ok {
		manager.mu.Unlock()
		return
	}
	admin.pool.SetPerAccountConcurrency(saved.PerAccountConcurrency)
	admin.pool.SetRoutingStrategy(saved.RoutingStrategy)
	admin.workers.applyLiveSettings(saved)
	admin.service.timeout.Store(int64(saved.RequestTimeout))
	admin.service.firstEventTimeout.Store(int64(saved.FirstEventTimeout))
	admin.service.ignoreSeed.Store(saved.IgnoreClientSeed)
	admin.service.repeatNonce.Store(saved.RepeatPromptNonce)
	admin.service.minOutputTokens.Store(int64(saved.MinOutputTokens))
	admin.service.setDowngradeGuard(saved.DowngradeGuard)
	generation.config = saved
	manager.mu.Unlock()

	manager.requests.log("service", "INFO", fmt.Sprintf(
		"配置已热更新 | 常驻 Worker=%d | 峰值 Worker=%d | 预热并发=%d | 单账户并发=%d | 策略=%s | 请求超时=%s | 首事件超时=%s | 忽略客户端 seed=%t | 降级判定=%s",
		saved.WarmWorkerLimit, saved.MaxActiveWorkers, saved.WarmStartupConcurrency,
		saved.PerAccountConcurrency, saved.RoutingStrategy, saved.RequestTimeout, saved.FirstEventTimeout, saved.IgnoreClientSeed,
		downgradeGuardSummary(saved.DowngradeGuard),
	))
	admin.service.prewarmIfRunning()
	admin.syncModelCache()
}
