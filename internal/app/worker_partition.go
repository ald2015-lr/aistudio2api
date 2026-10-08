package app

import (
	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// Worker 分区：每个 Worker 计入其账户当前权益所属的分区（aistudio.PoolScopeNormal 或 aistudio.PoolScopeUltra）。
// WARM_WORKER_LIMIT、MAX_ACTIVE_WORKERS 只约束普通分区，ULTRA_WARM_WORKER_LIMIT、ULTRA_MAX_ACTIVE_WORKERS 约束 Ultra 分区，
// 浏览器总数最多为两个分区峰值之和。冷启动名额（WARM_STARTUP_CONCURRENCY）与单账户并发仍是全局共用的

// defaultUltraCapacity 返回 Ultra 分区的默认常驻数与峰值数（与 config.Default 一致）
func defaultUltraCapacity() (int, int) {
	defaults := config.Default()
	return defaults.UltraWarmWorkerLimit, defaults.UltraMaxActiveWorkers
}

// setUltraCapacity 更新 Ultra 分区的常驻数与峰值数
func (manager *accountWorkerManager) setUltraCapacity(warm int, maxActive int) {
	manager.ultraWarmTarget.Store(int64(warm))
	manager.ultraMaxActive.Store(int64(maxActive))
}

// warmTargetFor 返回分区的常驻 Worker 数
func (manager *accountWorkerManager) warmTargetFor(partition aistudio.PoolScope) int {
	if partition == aistudio.PoolScopeUltra {
		return int(manager.ultraWarmTarget.Load())
	}
	return manager.warmTargetValue()
}

// maxActiveFor 返回分区的峰值 Worker 数
func (manager *accountWorkerManager) maxActiveFor(partition aistudio.PoolScope) int {
	if partition == aistudio.PoolScopeUltra {
		return int(manager.ultraMaxActive.Load())
	}
	return manager.maxActiveValue()
}
