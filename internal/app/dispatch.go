package app

import (
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// dispatchQueue 按账户选择条件分组，先到先唤醒等待账户槽位的请求
type dispatchQueue struct {
	mu     sync.Mutex
	queues map[string][]*dispatchWaiter
}

// dispatchWaiter 表示一个等待账户槽位的请求
type dispatchWaiter struct {
	key  string
	wake chan struct{}
}

func newDispatchQueue() *dispatchQueue {
	return &dispatchQueue{queues: make(map[string][]*dispatchWaiter)}
}

// dispatchKey 返回可互相替代的请求共用的队列键；号池不同的请求分开排队，Ultra 请求不会排在普通请求之后，反之亦然。
// 排除列表（ExcludedAccountIDs，换号重试排除已尝试的账户）不进键：换号重试的请求与同模型的请求同队先到先得，
// 不再各占一个队列、每次号池变化都被唤醒
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

// join 把请求排到同条件队列末尾，队列原本非空时唤醒队首以复查空闲槽位
func (queue *dispatchQueue) join(key string) *dispatchWaiter {
	waiter := &dispatchWaiter{key: key, wake: make(chan struct{}, 1)}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.queues[key] = append(queue.queues[key], waiter)
	queue.queues[key][0].signal()
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
