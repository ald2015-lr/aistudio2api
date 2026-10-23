package app

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 按需替换的速率上限（N6）：每个分区每分钟最多 replacementsPerMinute 次按需替换（换掉一个空闲 Worker、冷启动另一个账号）。
// 线上 30 分钟 700–1000 次启停、10 分钟“按需替换 540 次”：替补也经同一个出口，首个请求即被拒，又被换掉，冷启动风暴本身占满了浏览器启动。
// 超过上限时排队等待（等现有 Worker 空出或冷却恢复），不等待的选号按容量已满返回；预热补位不受限（它只补到常驻数）

const (
	// replacementsPerMinute 为每个分区每分钟的按需替换上限
	replacementsPerMinute = 10
	// replacementWindow 为计数窗口
	replacementWindow = time.Minute
)

// replacementRate 为各分区最近一分钟的按需替换时间；零值可用（不限）
type replacementRate struct {
	mu    sync.Mutex
	times map[aistudio.PoolScope][]time.Time
	// limit 为每分区每分钟的上限：newRuntime 设为 replacementsPerMinute，0 为不限（测试直接构造的管理器）
	limit atomic.Int64
}

// pruneLocked 去掉窗口外的记录；调用方持有 mu
func (rate *replacementRate) pruneLocked(partition aistudio.PoolScope, now time.Time) []time.Time {
	times := rate.times[partition]
	kept := times[:0]
	for _, at := range times {
		if now.Sub(at) < replacementWindow {
			kept = append(kept, at)
		}
	}
	if rate.times == nil {
		rate.times = make(map[aistudio.PoolScope][]time.Time)
	}
	rate.times[partition] = kept
	return kept
}

// replacementAllowed 判断分区现在能否再发起一次按需替换：预热补位不受限
func (manager *accountWorkerManager) replacementAllowed(ctx context.Context, partition aistudio.PoolScope) bool {
	if manager == nil || workerStartSourceFrom(ctx) == workerStartPrewarm {
		return true
	}
	rate := &manager.replacements
	limit := int(rate.limit.Load())
	if limit <= 0 {
		return true
	}
	rate.mu.Lock()
	defer rate.mu.Unlock()
	return len(rate.pruneLocked(partition, time.Now())) < limit
}

// noteReplacement 记一次按需替换（选中淘汰对象、冷启动闸门放行后记，冷启动失败也算：限制的是启动次数；闸门拒掉的不算）
func (manager *accountWorkerManager) noteReplacement(ctx context.Context, partition aistudio.PoolScope) {
	if manager == nil || workerStartSourceFrom(ctx) == workerStartPrewarm || manager.replacements.limit.Load() <= 0 {
		return
	}
	rate := &manager.replacements
	now := time.Now()
	rate.mu.Lock()
	times := rate.pruneLocked(partition, now)
	rate.times[partition] = append(times, now)
	rate.mu.Unlock()
}
