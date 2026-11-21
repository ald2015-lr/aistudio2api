package app

import (
	"fmt"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// proxyPoolCapacity 返回可能同时运行的 Worker 浏览器数：Σ各分区（峰值 + 排空上限，见 drainLimitFor），不含登录与验证。
// Pro 分区只在 PRO_POOL=true 且有 Pro 号池账户（proAccounts）时计入：否则 Pro 分区为空、不占端口，默认开启不会让没有 Pro 账户的部署
// 多需要 PRO_MAX_ACTIVE_WORKERS + 排空上限个端口。IP 池按它判断端口够不够（静态 IP 的端口数应不少于它再加上验证并发），
// 设置页用同一公式（web/src/proxyPool.ts，Pro 账户按状态接口的 pro_accounts 判断）。装配与保存配置时按配置计算；之后 Pro 分区
// 有没有账户的变化（读到 Pro 权益、新增或删除 Pro 账户）由后台周期按当前生效的 Worker 设置重算（见 syncProxyPoolCapacity）
func proxyPoolCapacity(cfg config.Config, proAccounts bool) int {
	pro := 0
	if cfg.ProPool && proAccounts {
		pro = cfg.ProMaxActiveWorkers
	}
	return browserCapacity(cfg.WarmStartupConcurrency, cfg.MaxActiveWorkers, cfg.UltraMaxActiveWorkers, pro)
}

// browserCapacity 为 proxyPoolCapacity 的公式：各分区峰值（不大于 0 的不计）加上它的排空上限之和
func browserCapacity(warmConcurrency int, peaks ...int) int {
	total := 0
	for _, peak := range peaks {
		if peak > 0 {
			total += peak + drainLimitFor(peak, warmConcurrency)
		}
	}
	return total
}

// liveProxyPoolCapacity 按当前生效的 Worker 设置（热更新后的各分区峰值与启动预热并发）计算 proxyPoolCapacity；
// proPartition 为 Pro 分区是否有账户（只在 PRO_POOL=true 时可能为真）
func (manager *accountWorkerManager) liveProxyPoolCapacity(proPartition bool) int {
	pro := 0
	if proPartition {
		pro = manager.maxActiveFor(aistudio.PoolScopePro)
	}
	return browserCapacity(manager.warmConcurrencyValue(),
		manager.maxActiveFor(aistudio.PoolScopeNormal), manager.maxActiveFor(aistudio.PoolScopeUltra), pro)
}

// syncProxyPoolCapacity 在后台周期里按 Pro 分区当前有没有账户重算 IP 池的浏览器上限：装配时权益尚未读取的账户按 Free 计，之后才读到
// Pro 权益，或在管理页新增、删除 Pro 账户时，上限随之计入或去掉 Pro 分区，端口不够与占用超出的 WARN、状态接口的 capacity 与设置页一致。
// 上限不变时什么也不做；不在请求路径上
func (service *trackedService) syncProxyPoolCapacity() {
	if service == nil || service.pool == nil || service.workers == nil || service.workers.proxyPool == nil {
		return
	}
	proPartition := service.pool.PartitionMembers().Count(aistudio.PoolScopePro) > 0
	service.workers.proxyPool.setCapacity(service.workers.liveProxyPoolCapacity(proPartition))
}

// hasProAccounts 判断账户里有没有权益为 Pro 的账户（运行时装配时，账户池建立之前按载入的权益估计 Pro 分区是否为空）
func hasProAccounts(accounts []*aistudio.Account) bool {
	for _, account := range accounts {
		if account != nil && account.BenefitTier == aistudio.BenefitTierPro {
			return true
		}
	}
	return false
}

// ipPool 返回 Worker 管理器持有的 IP 池；管理器为 nil（测试构造的服务）时为 nil，IP 池的方法对 nil 安全
func (manager *accountWorkerManager) ipPool() *proxyPool {
	if manager == nil {
		return nil
	}
	return manager.proxyPool
}

// workerProxyLease 为一次 Worker 启动申请出口：账户有自己的代理（ownProxy）或没有 IP 池时返回 nil，不改 options；
// IP 池分配时把 options.Proxy 改成分到的上游（只改这次启动的局部参数，accountWorker.config 不变）；IP 池关闭时为直通租约，
// options.Proxy 仍是 PROXY。返回的租约在启动失败时 launchFailed、启动成功后 attach 到浏览器（方法对 nil 安全）
func (manager *accountWorkerManager) workerProxyLease(
	accountID string, label string, ownProxy bool, options *camoufoxnative.Options,
) *proxyPoolLease {
	if manager == nil || manager.proxyPool == nil || ownProxy || options == nil {
		return nil
	}
	lease := manager.proxyPool.acquire(accountID, accountID, proxyPoolKindWorker, label, options.Proxy)
	if lease.Pooled() {
		options.Proxy = lease.URL()
	}
	return lease
}

// errFollowDefaultBusy 表示账户的 Worker 正在启动或关闭，这次不改为跟随默认代理
var errFollowDefaultBusy = fmt.Errorf("账户的 Worker 正在启动或关闭")

// followDefaultProxy 把账户的 Worker 配置改为跟随默认代理（“改为跟随默认代理”用）：save 写回 account.json（清空账户代理），
// 成功后账户启动 Worker 时向 IP 池申请出口。Worker 配置里的代理本来就等于 PROXY，运行中的 Worker 不重启，只给它
// （与正在清理的旧 Worker）补登记直通租约：Go 侧在浏览器关闭前仍走 PROXY，关闭后改用 IP 池。
// Worker 正在启动或关闭（startupMu 被占）时不等待、不调用 save，返回 errFollowDefaultBusy。返回是否有运行中的 Worker
func (manager *accountWorkerManager) followDefaultProxy(accountID string, save func() error) (bool, error) {
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		return false, fmt.Errorf("WAA worker 账户不存在: %s", accountID)
	}
	// 持有 startupMu 期间不会有新的启动读到旧的 ownProxy（启动读它之后一直持有 startupMu 到启动结束）
	if !account.startupMu.TryLock() {
		return false, errFollowDefaultBusy
	}
	defer account.startupMu.Unlock()
	if err := save(); err != nil {
		return false, err
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	account.ownProxy = false
	running := false
	for _, worker := range []*aistudio.NativeWorker{account.worker, account.cleanupWorker} {
		if worker == nil {
			continue
		}
		running = true
		lease := manager.proxyPool.passthrough(accountID, accountID, proxyPoolKindWorker, account.label, strings.TrimSpace(account.config.Proxy))
		worker.OnClosed(lease.release)
	}
	return running, nil
}
