package app

import (
	"context"
	"fmt"
	"sort"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// OpeningAccountIDs 返回正在启动 Worker 的账户；读取无锁镜像，不会被空闲回收等慢操作阻塞
func (manager *accountWorkerManager) OpeningAccountIDs() []string {
	ids := make([]string, 0)
	manager.openingSet.Range(func(key, _ any) bool {
		if id, ok := key.(string); ok {
			ids = append(ids, id)
		}
		return true
	})
	sort.Strings(ids)
	return ids
}

// occupiedSlotsApprox 统计占用的 Worker 槽位，供状态接口使用，不会阻塞：
// 关闭或替换 Worker 期间会持有账户锁（关闭浏览器可能几秒，WAA proof 在锁外生成），拿不到锁的账户按占用 1 个槽位计。
// 调度路径仍使用精确的 occupiedWorkers
func (manager *accountWorkerManager) occupiedSlotsApprox() int {
	manager.mu.RLock()
	accounts := make([]*accountWorker, 0, len(manager.accounts))
	for _, account := range manager.accounts {
		accounts = append(accounts, account)
	}
	manager.mu.RUnlock()
	slots := 0
	for _, account := range accounts {
		if !account.mu.TryLock() {
			slots++
			continue
		}
		if account.worker != nil {
			slots++
		}
		if account.cleanupWorker != nil {
			slots++
		}
		if account.worker == nil && account.cleanupWorker == nil && account.runtimeLease != nil {
			slots++
		}
		if account.cleanupWorker == nil && account.cleanupLease != nil {
			slots++
		}
		account.mu.Unlock()
	}
	return slots
}

// Onboarding 返回当前生成服务的新账户自动处理状态
func (manager *runtimeManager) Onboarding(context.Context) (api.OnboardingStatus, error) {
	onboarder := manager.currentOnboarder()
	if onboarder == nil {
		return api.OnboardingStatus{}, fmt.Errorf("新账户自动处理尚未就绪")
	}
	return onboarder.status(), nil
}

// UpdateOnboarding 保存新账户自动处理设置并立即生效
func (manager *runtimeManager) UpdateOnboarding(_ context.Context, policy api.OnboardingPolicy) (api.OnboardingStatus, error) {
	onboarder := manager.currentOnboarder()
	if onboarder == nil {
		return api.OnboardingStatus{}, fmt.Errorf("新账户自动处理尚未就绪")
	}
	if err := onboarder.updatePolicy(policy); err != nil {
		return api.OnboardingStatus{}, err
	}
	return onboarder.status(), nil
}

// QueueDisabledAccounts 把当前停用的账户加入新账户自动处理队列
func (manager *runtimeManager) QueueDisabledAccounts(context.Context) (api.OnboardingStatus, error) {
	onboarder := manager.currentOnboarder()
	if onboarder == nil {
		return api.OnboardingStatus{}, fmt.Errorf("新账户自动处理尚未就绪")
	}
	onboarder.queueDisabled()
	return onboarder.status(), nil
}

func (manager *runtimeManager) currentOnboarder() *accountOnboarder {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.current == nil {
		return nil
	}
	admin, ok := manager.current.admin.(*runtimeAdmin)
	if !ok {
		return nil
	}
	return admin.onboard
}
