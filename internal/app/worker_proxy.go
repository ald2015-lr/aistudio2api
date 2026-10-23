package app

import (
	"fmt"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// proxyPoolCapacity 返回可能同时运行的 Worker 浏览器数：Σ各分区（峰值 + 排空上限，见 drainLimitFor），不含登录与验证。
// IP 池按它判断端口够不够（静态 IP 的端口数应不少于它再加上验证并发），设置页用同一公式
func proxyPoolCapacity(cfg config.Config) int {
	total := 0
	for _, peak := range []int{cfg.MaxActiveWorkers, cfg.UltraMaxActiveWorkers} {
		if peak > 0 {
			total += peak + drainLimitFor(peak, cfg.WarmStartupConcurrency)
		}
	}
	return total
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
