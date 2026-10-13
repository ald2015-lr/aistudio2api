package app

import (
	"context"
	"slices"
	"testing"
)

// TestDispatchJoinAtArrivalOrder 等待队列按请求的到达序号排列：换号重试带着请求到达时的序号重新排队，排回之后到达的请求前面；
// 没有序号的排到末尾；插到队首的请求被唤醒复查
func TestDispatchJoinAtArrivalOrder(t *testing.T) {
	queue := newDispatchQueue()
	retried := dispatchOrderFromContext(withDispatchOrder(context.Background()))
	if retried == 0 {
		t.Fatal("到达序号不应为 0")
	}
	if got := dispatchOrderFromContext(context.Background()); got != 0 {
		t.Fatalf("没有到达序号的上下文应返回 0，实际 %d", got)
	}
	const key = "model"
	first := queue.join(key)
	second := queue.join(key)
	<-first.wake
	order := func() []*dispatchWaiter {
		queue.mu.Lock()
		defer queue.mu.Unlock()
		return slices.Clone(queue.queues[key])
	}
	head := queue.joinAt(key, retried)
	if got := order(); !slices.Equal(got, []*dispatchWaiter{head, first, second}) {
		t.Fatal("换号重试应排回之后到达的请求前面")
	}
	select {
	case <-head.wake:
	default:
		t.Fatal("插到队首的请求应被唤醒复查")
	}
	tail := queue.joinAt(key, 0)
	if got := order(); got[len(got)-1] != tail {
		t.Fatal("没有到达序号的请求应排到末尾")
	}
	for _, waiter := range []*dispatchWaiter{head, first, second, tail} {
		queue.leave(waiter)
	}
	if len(order()) != 0 {
		t.Fatal("全部离开后队列应为空")
	}
}
