package app

import (
	"slices"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 主用模型供给：keepWarm 约每 30 秒统计一次每个主用模型的可用供给并缓存，状态接口直接读缓存，不在每次状态请求时对上万个账户分类。
// 普通主用模型（PRIMARY_MODELS）统计普通分区，Ultra 主用模型（ULTRA_PRIMARY_MODELS）统计 Ultra 分区（条目 pool 为 ultra），
// 同一个模型在两份列表里时各有一条：
//   - warm 为能服务它的驻留 Worker：分区内的驻留 Worker 中，是该模型的候选、对它没有冷却、不在排空中的账号；
//   - standby 为能服务它的空闲备用账号：分区内没有驻留 Worker、不在启动中、空闲、对它没有冷却的候选账号，最多统计 primarySupplyStandbyLimit 个。
// standby 为 0 表示分区内已经没有还能服务该模型的空闲账号（多半是当天额度已用完，太平洋时间零点重置）：
// 死槽位淘汰后换不上能服务它的替补，主用模型的请求只能等能服务它的驻留 Worker 空出

const (
	// primarySupplyInterval 为重新统计主用模型供给的周期
	primarySupplyInterval = 30 * time.Second
	// primarySupplyStandbyLimit 为每个主用模型最多统计的空闲备用账号数
	primarySupplyStandbyLimit = 1000
)

// primarySupplySnapshot 为一次统计的结果：models、ultraModels 为统计时的普通与 Ultra 主用模型列表，at 为统计时间，
// ruleVersion 为统计前读到的号池规则版本（见 AccountPool.PoolRuleVersion）
type primarySupplySnapshot struct {
	models      []string
	ultraModels []string
	at          time.Time
	ruleVersion uint64
	entries     []api.AdminPrimarySupply
}

// primarySupplyTarget 为要统计供给的一个主用模型：Pool 为它所在列表的号池（同时也是统计的 Worker 分区）
type primarySupplyTarget struct {
	model string
	pool  aistudio.PoolScope
}

// primarySupplyTargets 返回两份列表要统计的模型：普通列表在前
func primarySupplyTargets(normal []string, ultra []string) []primarySupplyTarget {
	targets := make([]primarySupplyTarget, 0, len(normal)+len(ultra))
	for _, modelID := range normal {
		targets = append(targets, primarySupplyTarget{model: modelID, pool: aistudio.PoolScopeNormal})
	}
	for _, modelID := range ultra {
		targets = append(targets, primarySupplyTarget{model: modelID, pool: aistudio.PoolScopeUltra})
	}
	return targets
}

// supplyPoolLabel 返回供给条目的 pool 字段：Ultra 列表的条目为 ultra，普通列表的为空
func supplyPoolLabel(pool aistudio.PoolScope) string {
	if pool == aistudio.PoolScopeUltra {
		return pool.String()
	}
	return ""
}

// refreshPrimarySupply 在缓存过期（超过 primarySupplyInterval）、任一份主用模型列表变化或号池规则版本变化时重新统计
// （keepWarm 每个周期调用），返回这次是否重新统计；两份列表都为空时清空缓存。供给按分区统计，ULTRA_INCLUDE_PRO 切换后
// Pro 账户换了分区，版本不同就在下一个 keepWarm 周期重算，不等满 30 秒。先读版本再统计：统计期间发生的切换让快照带上旧版本，
// 下一个周期照样重算
func (manager *accountWorkerManager) refreshPrimarySupply(now time.Time) bool {
	normal, ultra := manager.primaryModelLists()
	version := manager.pool.PoolRuleVersion()
	snapshot := manager.primarySupply.Load()
	if len(normal) == 0 && len(ultra) == 0 {
		if snapshot != nil {
			manager.primarySupply.Store(nil)
		}
		return false
	}
	if snapshot != nil && slices.Equal(snapshot.models, normal) && slices.Equal(snapshot.ultraModels, ultra) &&
		now.Sub(snapshot.at) < primarySupplyInterval && snapshot.ruleVersion == version {
		return false
	}
	manager.primarySupply.Store(&primarySupplySnapshot{
		models: slices.Clone(normal), ultraModels: slices.Clone(ultra), at: now, ruleVersion: version,
		entries: manager.computePrimarySupply(primarySupplyTargets(normal, ultra), primarySupplyStandbyLimit),
	})
	return true
}

// clearPrimarySupply 清空缓存（生成服务停止时，Worker 已全部关闭，旧的统计不再成立）
func (manager *accountWorkerManager) clearPrimarySupply() {
	manager.primarySupply.Store(nil)
}

// primarySupplyStatus 返回缓存的主用模型供给，只保留仍在当前对应列表里的（号池、模型）（列表刚改过、还没重新统计时新增的模型暂缺）
func (manager *accountWorkerManager) primarySupplyStatus() []api.AdminPrimarySupply {
	snapshot := manager.primarySupply.Load()
	if snapshot == nil {
		return nil
	}
	normal, ultra := manager.primaryModelLists()
	entries := make([]api.AdminPrimarySupply, 0, len(snapshot.entries))
	for _, entry := range snapshot.entries {
		models := normal
		if entry.Pool == aistudio.PoolScopeUltra.String() {
			models = ultra
		}
		if slices.Contains(models, entry.Model) {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	return entries
}

// computePrimarySupply 统计每个模型在它所在列表对应分区的供给（口径见本文件开头）：每个模型按调度索引分类一次（只读内存状态，
// 不读磁盘），分类限定在该号池，另一分区的驻留 Worker 不计入；排空中的驻留 Worker 不传作热账户，分类时随排空标记落入忙碌分组，
// 不计入 warm；正在启动的账号不计入 standby。模型在号池里找不到（目录尚未同步、配置写错）时两项都为 0
func (manager *accountWorkerManager) computePrimarySupply(targets []primarySupplyTarget, standbyLimit int) []api.AdminPrimarySupply {
	draining := manager.drainingAccounts()
	warm := excludeAccountIDs(manager.WarmAccountIDs(), draining)
	opening := make(map[string]struct{})
	for _, accountID := range manager.OpeningAccountIDs() {
		opening[accountID] = struct{}{}
	}
	entries := make([]api.AdminPrimarySupply, 0, len(targets))
	for _, target := range targets {
		entry := api.AdminPrimarySupply{Model: target.model, Pool: supplyPoolLabel(target.pool)}
		groups, err := manager.pool.ClassifyCandidatesCached(aistudio.AccountSelection{
			ModelID: target.model, Method: "generateContent", Pool: target.pool,
		}, warm)
		if err == nil {
			entry.Warm = len(groups.WarmReady) + len(groups.WarmAvailable) + len(groups.WarmBusy)
			for _, accountID := range groups.StandbyReady {
				if _, starting := opening[accountID]; starting {
					continue
				}
				if entry.Standby >= standbyLimit {
					entry.StandbyCapped = true
					break
				}
				entry.Standby++
			}
		}
		entries = append(entries, entry)
	}
	return entries
}
