package app

import (
	"slices"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 主用模型供给：keepWarm 约每 30 秒统计一次每个主用模型（PRIMARY_MODELS）在普通分区的可用供给并缓存，状态接口直接读缓存，
// 不在每次状态请求时对上万个账户分类：
//   - warm 为能服务它的驻留 Worker：普通分区的驻留 Worker 中，是该模型的候选、对它没有冷却、不在排空中的账号；
//   - standby 为能服务它的空闲备用账号：没有驻留 Worker、不在启动中、空闲、对它没有冷却的候选账号，最多统计 primarySupplyStandbyLimit 个。
// standby 为 0 表示分区内已经没有还能服务该模型的空闲账号（多半是当天额度已用完，太平洋时间零点重置）：
// 死槽位淘汰后换不上能服务它的替补，主用模型的请求只能等能服务它的驻留 Worker 空出

const (
	// primarySupplyInterval 为重新统计主用模型供给的周期
	primarySupplyInterval = 30 * time.Second
	// primarySupplyStandbyLimit 为每个主用模型最多统计的空闲备用账号数
	primarySupplyStandbyLimit = 1000
)

// primarySupplySnapshot 为一次统计的结果：models 为统计时的主用模型列表，at 为统计时间
type primarySupplySnapshot struct {
	models  []string
	at      time.Time
	entries []api.AdminPrimarySupply
}

// refreshPrimarySupply 在缓存过期（超过 primarySupplyInterval）或主用模型列表变化时重新统计（keepWarm 每个周期调用），
// 返回这次是否重新统计；没有主用模型时清空缓存
func (manager *accountWorkerManager) refreshPrimarySupply(now time.Time) bool {
	models := manager.primaryModelList()
	snapshot := manager.primarySupply.Load()
	if len(models) == 0 {
		if snapshot != nil {
			manager.primarySupply.Store(nil)
		}
		return false
	}
	if snapshot != nil && slices.Equal(snapshot.models, models) && now.Sub(snapshot.at) < primarySupplyInterval {
		return false
	}
	manager.primarySupply.Store(&primarySupplySnapshot{
		models: slices.Clone(models), at: now, entries: manager.computePrimarySupply(models, primarySupplyStandbyLimit),
	})
	return true
}

// clearPrimarySupply 清空缓存（生成服务停止时，Worker 已全部关闭，旧的统计不再成立）
func (manager *accountWorkerManager) clearPrimarySupply() {
	manager.primarySupply.Store(nil)
}

// primarySupplyStatus 返回缓存的主用模型供给，只保留仍在当前主用模型列表里的模型（列表刚改过、还没重新统计时新增的模型暂缺）
func (manager *accountWorkerManager) primarySupplyStatus() []api.AdminPrimarySupply {
	snapshot := manager.primarySupply.Load()
	if snapshot == nil {
		return nil
	}
	models := manager.primaryModelList()
	entries := make([]api.AdminPrimarySupply, 0, len(snapshot.entries))
	for _, entry := range snapshot.entries {
		if slices.Contains(models, entry.Model) {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	return entries
}

// computePrimarySupply 统计每个模型在普通分区的供给（口径见本文件开头）：每个模型按调度索引分类一次（只读内存状态，不读磁盘），
// 排空中的驻留 Worker 不传作热账户，分类时随排空标记落入忙碌分组，不计入 warm；正在启动的账号不计入 standby。
// 模型在号池里找不到（目录尚未同步、配置写错）时两项都为 0
func (manager *accountWorkerManager) computePrimarySupply(models []string, standbyLimit int) []api.AdminPrimarySupply {
	draining := manager.drainingAccounts()
	warm := excludeAccountIDs(manager.WarmAccountIDs(), draining)
	opening := make(map[string]struct{})
	for _, accountID := range manager.OpeningAccountIDs() {
		opening[accountID] = struct{}{}
	}
	entries := make([]api.AdminPrimarySupply, 0, len(models))
	for _, modelID := range models {
		entry := api.AdminPrimarySupply{Model: modelID}
		groups, err := manager.pool.ClassifyCandidatesCached(aistudio.AccountSelection{
			ModelID: modelID, Method: "generateContent", Pool: aistudio.PoolScopeNormal,
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
