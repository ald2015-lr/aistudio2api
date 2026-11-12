package aistudio

import (
	"math/rand/v2"
	"strings"
	"time"
)

// ModelListedIn 判断号池里有没有账户的目录列出这个模型（含目录别名与 Build 目录），与选号时“目录中没有请求模型”的判断相同
func (p *AccountPool) ModelListedIn(scope PoolScope, modelID string) bool {
	if p == nil {
		return false
	}
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if modelID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hasModelLocked(modelID, scope)
}

// benefitProbeRank 为缺少模型时读目录的权益顺序：Ultra、Pro、Plus、Free（与上游 v0.3.5 的 ByBenefitTier 相同）。
// 新模型多半先对高权益账户开放，先读它们最可能读到
func benefitProbeRank(tier BenefitTier) int {
	switch tier {
	case BenefitTierUltra:
		return 3
	case BenefitTierPro:
		return 2
	case BenefitTierPlus:
		return 1
	default:
		return 0
	}
}

// CatalogProbeCandidates 返回号池内可以马上读一次目录的账户，最多 limit 个：启用、就绪（没有租约、不在冷却、不需要登录、
// 组织没有停用）、已经有目录，按权益从高到低（Ultra、Pro、Plus、Free），同一权益内随机（不总打同几个账户）。
// 一次加锁遍历全部账户，只在缺少模型的读目录时调用（app 层全局限频）
func (p *AccountPool) CatalogProbeCandidates(scope PoolScope, limit int) []string {
	if p == nil || limit <= 0 {
		return nil
	}
	var tiers [4][]string
	now := time.Now()
	p.mu.Lock()
	for _, account := range p.accounts {
		if account == nil || len(account.Models) == 0 || !p.inPoolLocked(scope, account) {
			continue
		}
		if p.accountStateLocked(account, now) != AccountReady || account.orgDisabled {
			continue
		}
		rank := benefitProbeRank(account.BenefitTier)
		tiers[rank] = append(tiers[rank], account.ID)
	}
	p.mu.Unlock()
	result := make([]string, 0, limit)
	for rank := len(tiers) - 1; rank >= 0 && len(result) < limit; rank-- {
		accountIDs := tiers[rank]
		rand.Shuffle(len(accountIDs), func(left, right int) {
			accountIDs[left], accountIDs[right] = accountIDs[right], accountIDs[left]
		})
		result = append(result, accountIDs[:min(len(accountIDs), limit-len(result))]...)
	}
	return result
}
