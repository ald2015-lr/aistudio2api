package aistudio

// UltraAccountIDs 返回当前属于 Ultra 分区的账户（只给测试用，由 PartitionMembers 推出、每次新建）：生产代码只用 PartitionMembers
func (p *AccountPool) UltraAccountIDs() map[string]struct{} {
	ids := make(map[string]struct{})
	for accountID, partition := range p.PartitionMembers() {
		if partition == PoolScopeUltra {
			ids[accountID] = struct{}{}
		}
	}
	return ids
}

// baseUltraPoolTier 为 56e69d9（只有普通与 Ultra 两个号池时）的 ultraPoolTier 原样副本，作为 PRO_POOL=false 时成员规则的对照
func baseUltraPoolTier(tier BenefitTier, includePro bool) bool {
	return tier == BenefitTierUltra || includePro && tier == BenefitTierPro
}

// bruteInPool 不经预先算好的表、直接按 poolOfTier 判断账户是否属于号池，供暴力核对
func bruteInPool(p *AccountPool, scope PoolScope, account *Account) bool {
	return account != nil && scope.Contains(poolOfTier(account.BenefitTier, p.rules.includePro, p.rules.proPool))
}

// bruteTierInPool 与 bruteInPool 相同，按权益判断
func bruteTierInPool(p *AccountPool, scope PoolScope, tier BenefitTier) bool {
	return scope.Contains(poolOfTier(tier, p.rules.includePro, p.rules.proPool))
}
