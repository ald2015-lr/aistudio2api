package app

import (
	"context"
	"sync/atomic"
	"time"
)

// egressQueueWaitMax 为非主用模型整个请求按“出口限速”累计排队的上限：没有上限时会一直排到首事件前期限（REQUEST_TIMEOUT），
// 线上表现为“超时 504（排队中）”
const egressQueueWaitMax = 30 * time.Second

// egressWaitBudget 为一个非主用模型生成请求按“出口限速”累计排队的时间（整个请求共用，换号重试不重新计时）
type egressWaitBudget struct{ waited atomic.Int64 }

type egressWaitBudgetKey struct{}

// withEgressWaitBudget 给请求挂上出口排队预算；已有时沿用（同一请求只计一份）
func withEgressWaitBudget(ctx context.Context) context.Context {
	if egressWaitBudgetFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, egressWaitBudgetKey{}, &egressWaitBudget{})
}

// egressWaitBudgetFrom 返回请求的出口排队预算，没有时为 nil（调用方按单次选号累计）
func egressWaitBudgetFrom(ctx context.Context) *egressWaitBudget {
	if ctx == nil {
		return nil
	}
	budget, _ := ctx.Value(egressWaitBudgetKey{}).(*egressWaitBudget)
	return budget
}

// add 计入一次等待并返回累计；budget 为 nil 时只返回本次
func (budget *egressWaitBudget) add(wait time.Duration) time.Duration {
	if wait < 0 {
		wait = 0
	}
	if budget == nil {
		return wait
	}
	return time.Duration(budget.waited.Add(int64(wait)))
}

// total 返回已累计的等待，budget 为 nil 时为 0
func (budget *egressWaitBudget) total() time.Duration {
	if budget == nil {
		return 0
	}
	return time.Duration(budget.waited.Load())
}

// egressQueueWaitFor 返回非主用模型按“出口限速”累计排队的上限：egressQueueWait 为 0 时为默认 egressQueueWaitMax
func (service *trackedService) egressQueueWaitFor() time.Duration {
	if service != nil && service.egressQueueWait > 0 {
		return service.egressQueueWait
	}
	return egressQueueWaitMax
}
