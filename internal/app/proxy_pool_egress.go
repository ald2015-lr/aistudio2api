package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// IP 池（proxy_pool.go）与 429 治理（egress_guard.go、rate_limit_guard.go）的接线：
//   - 出口键随绑定：账户池在写绑定的同一次加锁内重挂出口通道（aistudio.AccountPool.SetProxyBindings），静态每个端口、动态每个会话
//     是独立出口；会话号写在密码里时出口键带 #会话号（aistudio egressKeyOfLocked）；
//   - 冷启动闸门：IP 池开启、账户没有自己的代理时不经闸门（egressColdStartGate）：闸门看的是账户现在的默认出口，真正启动时池会另分端口；
//   - 端口冷却撤回误伤（proxyPortCooled）：规则 A 冷却时，计入规则 A 的账号（窗口里在这个端口上返回裸 429、自最近一次成功以来只在
//     这个端口上被拒、第一次被拒前还不是坏号，见 proxyPool.strikeLocked）被逐级冷却、被标坏号是 IP 的问题，撤回这些冷却与坏号记忆；
//     在别处也被拒、早已是坏号的账号不计入，也不撤回（坏号记忆的依据：裸 429 多半跟着账号走）。规则 A 与规则 B 都让正用这个端口的
//     Worker 换端口重启；
//   - 动态换会话：出口判定成立、判定的出口就是账户当前的会话时换会话（applyRateLimitCooldown 调 rotateSession，10 分钟最短间隔）。
//     每个会话各自是独立出口、只有一个账号，凑不够判定门槛，实际走不到：单个会话被上游限流（裸 429）不会自动换会话；
//   - 日志带出口：裸 429 的“账号冷却”行与“WAA Worker 启动失败”行末尾追加端口号或会话号（proxyPoolLogSuffix）

// proxyPortRelaxMinLevel 为端口冷却时撤回的逐级冷却的最低档位：第 1 档起全部撤回（端口换手后新账号也被拒，说明被拒的是 IP）
const proxyPortRelaxMinLevel = 1

// proxyPortCooled 为 IP 池端口冷却的回调（newRuntime 装配成功后挂到进程级 IP 池上）：
//   - 规则 A（不同账号的裸 429）：对计入规则 A 的账号（cooling.RateLimitedAccounts）撤回逐级冷却（RelaxRateLimitCooldowns，缩到现在 + 判定期间的
//     短冷却 egressLimitedCooldown，留出让 Worker 换端口的时间；档位一起清除，登记的淘汰与排空随 stillCooling 为假自动撤销），
//     并撤回坏号记忆（badAccountMemory.pardon：清零连续裸限流次数，规则 A 窗口内标上的坏号清除，账号回到预热、扩容与替补的候选）；
//   - 规则 A 与规则 B（连不上上游代理）：对正用这个端口服务的账户登记后台关闭 Worker（scheduleWorkerReset：等同账号进行中的请求结束，
//     不切断输出中的流），之后补齐预热时换到别的端口；登记前核对账户仍在这个端口上服务（servesOnPort），日志的“重启 Worker”
//     只计真正登记的，ResetAll、Close 期间或独占等待登记失败的计入“未登记”。
//
// 回调在 IP 池锁外、可能持有某个账户的 startupMu（Worker 启动失败时）调用：撤回要写运行态文件，放到后台执行；
// scheduleWorkerReset 只登记、不等关闭。生成服务已停止时不做任何事
func (service *trackedService) proxyPortCooled(cooling proxyPoolCooling) {
	if service == nil || service.pool == nil || len(cooling.RateLimitedAccounts) == 0 && len(cooling.Workers) == 0 {
		return
	}
	if service.lifecycle != nil && service.lifecycle.Err() != nil {
		return
	}
	run := func() { service.relaxProxyPort(cooling) }
	guard := service.egress
	if guard == nil {
		run()
		return
	}
	// 与进入出口判定时的撤回共用 relaxWG（测试等它结束）
	guard.relaxWG.Add(1)
	go func() {
		defer guard.relaxWG.Done()
		run()
	}()
}

// relaxProxyPort 为 proxyPortCooled 的执行部分：撤回规则 A 账号的逐级冷却与坏号记忆、登记重启正用端口的 Worker，写一行持久日志
func (service *trackedService) relaxProxyPort(cooling proxyPoolCooling) {
	startedAt := time.Now()
	relaxed, pardoned, failed := 0, 0, 0
	if cooling.Rule == proxyPoolRuleRateLimited {
		until := time.Now().Add(service.egress.limitedCooldown())
		memory := service.workers.badAccountMemory()
		for _, accountID := range cooling.RateLimitedAccounts {
			if memory.pardon(accountID, proxyPoolFailureWindow) {
				pardoned++
			}
			changed, err := service.pool.RelaxRateLimitCooldowns(accountID, until, proxyPortRelaxMinLevel)
			switch {
			case errors.Is(err, aistudio.ErrAccountNotFound):
				// 账户已删除：没有可撤回的
			case err != nil:
				failed++
				service.requests.log(accountID, "WARN", "IP 池端口冷却后撤回逐级冷却失败 | 端口="+fmt.Sprint(cooling.Port)+" | 错误="+strings.TrimSpace(err.Error()))
			case changed > 0:
				relaxed++
			}
		}
	}
	restarted, unregistered := 0, 0
	if service.workers != nil {
		ipPool := service.workers.ipPool()
		for _, accountID := range cooling.Workers {
			// 先取版本再核对端口：回调在后台、撤回写完运行态之后才走到这里，期间账户可能已换成别的端口上的新 Worker
			// （先启后停刚激活）。核对时仍在这个端口上，取到的就是这个端口上的 Worker 的版本；核对之后才换掉的，按版本登记的关闭
			// 发现版本已变，不会关掉新 Worker
			generation := service.workers.WorkerGeneration(accountID)
			if !service.workers.hasWorker(accountID) || !ipPool.servesOnPort(accountID, cooling.Port) {
				continue
			}
			pending, _ := service.workers.scheduleWorkerReset(accountID, generation, nil, workerResetProxyPort)
			if workerResetRegistered(pending) {
				restarted++
			} else {
				unregistered++
			}
		}
	}
	if relaxed == 0 && pardoned == 0 && failed == 0 && restarted == 0 && unregistered == 0 {
		return
	}
	rule := "连续出口连不通"
	if cooling.Rule == proxyPoolRuleRateLimited {
		rule = "不同账号返回限流"
	}
	skipped := ""
	if unregistered > 0 {
		skipped = fmt.Sprintf(" | 未登记=%d", unregistered)
	}
	service.requests.logDurable("service", "INFO", fmt.Sprintf(
		"IP 池端口冷却：撤回逐级冷却并换端口重启 Worker | 端口=%d | 规则=%s | 撤回账号=%d | 撤回坏号=%d | 失败=%d | 重启 Worker=%d%s | 耗时=%s",
		cooling.Port, rule, relaxed, pardoned, failed, restarted, skipped, time.Since(startedAt).Round(time.Millisecond),
	))
}

// workerResetRegistered 判断 scheduleWorkerReset 的结果是登记了关闭（还在后台进行，或已经结束且关闭了这个版本）：
// ResetAll、Close 期间不登记、独占等待登记失败、版本已经换过时返回的是已结束且没有关闭的结果
func workerResetRegistered(pending *workerReset) bool {
	if pending == nil {
		return false
	}
	select {
	case <-pending.done:
		return pending.reset
	default:
		return true
	}
}

// hasWorker 返回账户当前是否有 Worker（运行中或停在关闭中）；账户不存在时为假
func (manager *accountWorkerManager) hasWorker(accountID string) bool {
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		return false
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.worker != nil
}
