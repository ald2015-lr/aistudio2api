package app

import (
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// PerformanceStats 返回账户池锁与浏览器命令的累计统计（管理接口 GET /api/debug/perf）。
// 诊断脚本间隔几秒取两次，按差值算出每秒加锁次数、平均等锁时间、锁占用率与每秒浏览器命令数
func (manager *runtimeManager) PerformanceStats() any {
	return map[string]any{
		"time":      time.Now(),
		"pool_lock": aistudio.PoolLockStats(),
		"browser":   camoufoxnative.RuntimeStats(),
		"models":    servedModelStats(),
		// downgrade_guard 为降级判定统计（见 downgrade_guard.go）
		"downgrade_guard": downgradeGuardStats(),
	}
}
