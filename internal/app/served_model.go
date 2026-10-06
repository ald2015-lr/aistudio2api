package app

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 上游实际服务的模型与请求不同（例如请求 Pro 实际由 3.1-flash-lite 生成）时计数并记录。
// Build 通道响应的每一块都有 modelVersion：前面的内容块沿用请求的模型名，最后一块报告实际服务的模型；
// Playground 通道的响应里没有模型名，无法核对
var servedModelMismatch struct {
	count  atomic.Int64
	mu     sync.Mutex
	logged map[string]time.Time
	last   string
}

// servedModelLogInterval 内同一种不一致只写一次运行日志（每个请求的时间线里仍会记下）
const servedModelLogInterval = 10 * time.Minute

// servedModelDiffers 核对上游标明的模型系列是否与请求一致；不一致时计数、记录并写进管理日志的 served_model
func (service *trackedService) servedModelDiffers(ctx context.Context, requestID, accountLabel, requested, served, channel string) bool {
	if requested == "" || served == "" || aistudio.ModelFamily(requested) == aistudio.ModelFamily(served) {
		return false
	}
	servedModelMismatch.count.Add(1)
	key := requested + " → " + served
	servedModelMismatch.mu.Lock()
	if servedModelMismatch.logged == nil {
		servedModelMismatch.logged = make(map[string]time.Time)
	}
	servedModelMismatch.last = key + "（" + time.Now().Format("01-02 15:04:05") + "）"
	shouldLog := time.Since(servedModelMismatch.logged[key]) >= servedModelLogInterval
	if shouldLog {
		servedModelMismatch.logged[key] = time.Now()
	}
	servedModelMismatch.mu.Unlock()
	api.SetAccessLogServedModel(ctx, served)
	service.requests.logRequestProgress(requestID, accountLabel, "WARN", "上游实际服务的模型与请求不同 | 请求="+requested+" | 上游="+served)
	if shouldLog {
		service.requests.log(accountLabel, "WARN", fmt.Sprintf(
			"上游实际服务的模型与请求不同 | 请求=%s | 上游=%s | 通道=%s（同一种情况 10 分钟内只记一次）", requested, served, channel,
		))
	}
	return true
}

// servedModelStats 返回模型核对统计（管理接口 /api/debug/perf）
func servedModelStats() map[string]any {
	servedModelMismatch.mu.Lock()
	last := servedModelMismatch.last
	servedModelMismatch.mu.Unlock()
	return map[string]any{"mismatches": servedModelMismatch.count.Load(), "last": last}
}
