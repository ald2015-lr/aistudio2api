package app

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// dispatchQueue 按账户选择条件分组，按请求到达的先后唤醒等待账户槽位的请求
type dispatchQueue struct {
	mu     sync.Mutex
	queues map[string][]*dispatchWaiter
}

// dispatchWaiter 表示一个等待账户槽位的请求；order 为请求的到达序号，队列按它从小到大排列
type dispatchWaiter struct {
	key   string
	order uint64
	wake  chan struct{}
}

// dispatchOrders 为请求的到达序号：generateWithRetry 在第一次选号前取一个，换号重试共用，重新排队时按它插回原来的位置
var dispatchOrders atomic.Uint64

type dispatchOrderKey struct{}

// withDispatchOrder 为整个请求取一个到达序号放进选号上下文
func withDispatchOrder(ctx context.Context) context.Context {
	return context.WithValue(ctx, dispatchOrderKey{}, dispatchOrders.Add(1))
}

// dispatchOrderFromContext 返回选号上下文中的到达序号，没有时为 0
func dispatchOrderFromContext(ctx context.Context) uint64 {
	order, _ := ctx.Value(dispatchOrderKey{}).(uint64)
	return order
}

func newDispatchQueue() *dispatchQueue {
	return &dispatchQueue{queues: make(map[string][]*dispatchWaiter)}
}

// dispatchKey 返回可互相替代的请求共用的队列键；号池不同的请求分开排队，Ultra 请求不会排在普通请求之后，反之亦然。
// 排除列表（ExcludedAccountIDs，换号重试排除已尝试的账户）不进键：换号重试的请求与同模型的请求同队先到先得
// （按请求到达的先后，换号重试排回原来的位置，见 joinAt），不再各占一个队列、每次号池变化都被唤醒
func dispatchKey(selection aistudio.AccountSelection) string {
	allowed := slices.Clone(selection.AllowedAccountIDs)
	slices.Sort(allowed)
	return strings.Join([]string{
		selection.ModelID, selection.Method, selection.Capability, selection.AccountID, selection.ResourceID,
		strings.Join(allowed, ","), strconv.FormatBool(selection.PlaygroundOnly),
		strconv.FormatBool(selection.BuildOnly), strconv.FormatBool(selection.PlaygroundFirst),
		strconv.FormatBool(selection.PreferPlayground), selection.Pool.String(),
	}, "\x00")
}

// join 把请求排到同条件队列末尾（取新的到达序号），见 joinAt
func (queue *dispatchQueue) join(key string) *dispatchWaiter {
	return queue.joinAt(key, 0)
}

// joinAt 按到达序号排队：换号重试排回请求到达时的位置（排在之后到达的请求前面），否则它的主用模型截止时间照常从请求开始计，
// 却排在重试期间新到的请求之后，最早到的请求反而最容易 429。order 为 0 时取新序号排到末尾。排好后唤醒队首以复查空闲槽位
func (queue *dispatchQueue) joinAt(key string, order uint64) *dispatchWaiter {
	if order == 0 {
		order = dispatchOrders.Add(1)
	}
	waiter := &dispatchWaiter{key: key, order: order, wake: make(chan struct{}, 1)}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	waiters := queue.queues[key]
	index := len(waiters)
	for index > 0 && waiters[index-1].order > order {
		index--
	}
	waiters = slices.Insert(waiters, index, waiter)
	queue.queues[key] = waiters
	waiters[0].signal()
	return waiter
}

// leave 移除请求并在其位于队首时唤醒下一个请求
func (queue *dispatchQueue) leave(waiter *dispatchWaiter) {
	if waiter == nil {
		return
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	waiters := queue.queues[waiter.key]
	index := slices.Index(waiters, waiter)
	if index < 0 {
		return
	}
	waiters = slices.Delete(waiters, index, index+1)
	if len(waiters) == 0 {
		delete(queue.queues, waiter.key)
		return
	}
	queue.queues[waiter.key] = waiters
	if index == 0 {
		waiters[0].signal()
	}
}

// passOn 唤醒同队里排在 waiter 之后的下一个请求。带排除列表的请求（换号重试）被唤醒复查后仍要等待时调用：
// 刚空出的槽位可能在它排除的账户上，而号池变化只唤醒队首，不传下去时后面能用这个槽位的请求要等到兜底复查
func (queue *dispatchQueue) passOn(waiter *dispatchWaiter) {
	if waiter == nil {
		return
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	waiters := queue.queues[waiter.key]
	if index := slices.Index(waiters, waiter); index >= 0 && index+1 < len(waiters) {
		waiters[index+1].signal()
	}
}

// wakeHeads 在账户或 Worker 状态变化后唤醒每个队列的队首
func (queue *dispatchQueue) wakeHeads() {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for _, waiters := range queue.queues {
		waiters[0].signal()
	}
}

func (waiter *dispatchWaiter) signal() {
	select {
	case waiter.wake <- struct{}{}:
	default:
	}
}
