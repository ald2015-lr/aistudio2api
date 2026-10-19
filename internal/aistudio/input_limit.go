package aistudio

import "strings"

// InputTokenLimitError 判断上游错误是否为"输入 token 超过上限"（提示词过长）。
//
// 这类错误与账号无关：换号、冷却都不会让同一段输入变短。上游有时按 400 INVALID_ARGUMENT 返回
// （"The input token count (N) exceeds the maximum number of tokens allowed (M)."），
// 部分账号的 Playground 与 Build 对输入另有上限（如 131072 token），超过时按 429 RESOURCE_EXHAUSTED 返回
// （"The input token count exceeds the maximum number of tokens allowed 131072"）。两种写法都按文案识别，
// 不看状态码：调度不冷却账号、不换号（Playground 超限仍可改走 Build 再试一次），对外一律按 400 返回
func InputTokenLimitError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "input token count") && strings.Contains(message, "exceed") {
		return true
	}
	return strings.Contains(message, "exceeds the maximum number of tokens allowed")
}

// InputTokenCapError 判断是否为部分账号的输入上限错误（"The input token count exceeds the maximum number of tokens allowed 131072"）：
// 这是账号权益带来的上限，换通道（Playground 改走 Build）或换更高权益的账号可能有更高的上限。
// 带具体计数的模型本身上限（"The input token count (N) exceeds the maximum number of tokens allowed (M)."）不算：任何账号都放不下
func InputTokenCapError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "input token count exceeds the maximum number of tokens allowed")
}

// benefitTierRank 按账号输入上限大致由低到高给权益排序：Free < Plus < Pro < Ultra（未知权益按 Free）
func benefitTierRank(tier BenefitTier) int {
	switch tier {
	case BenefitTierPlus:
		return 1
	case BenefitTierPro:
		return 2
	case BenefitTierUltra:
		return 3
	default:
		return 0
	}
}

// EnabledAccountsAboveTierOf 返回号池内启用、权益高于 accountID 所属账号权益的账户 ID（按候选索引顺序）。
// 输入 token 超过账号上限时用它找一次更高权益的账号再试；accountID 不存在或没有更高权益的账号时返回 nil。
// 号池按成员关系（含 ULTRA_INCLUDE_PRO）过滤，权益高低仍按真实权益比较：输入上限由真实权益决定
func (p *AccountPool) EnabledAccountsAboveTierOf(scope PoolScope, accountID string) []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return nil
	}
	rank := benefitTierRank(account.BenefitTier)
	index := p.schedulingLocked()
	var ids []string
	for position := range index.ids {
		if !index.enabled[position] || !p.tierInPoolLocked(scope, index.tier[position]) || benefitTierRank(index.tier[position]) <= rank {
			continue
		}
		ids = append(ids, index.ids[position])
	}
	return ids
}
