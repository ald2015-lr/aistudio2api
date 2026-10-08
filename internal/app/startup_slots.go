package app

import (
	"context"
	"fmt"
	"sync"
)

// startupSlots 限制同时冷启动的浏览器 Worker 数（零值可用）。容量在每次判断时读取 WARM_STARTUP_CONCURRENCY 的当前值，
// 热更新后由 resized 唤醒排队者按新容量放行，不需要重启生成服务；调小时已占用的名额不收回，释放后按新容量收敛。
//
// 按需启动（有请求在等这个 Worker：请求现场启动、请求排队时的后台扩容）与后台启动（预热、冷却轮换后的补齐）共用名额，
// 按需启动额外保留 1 个：后台启动只在占用数小于容量、且没有按需启动在排队时才能取得名额，最后一个名额只给按需启动。
// 预热把容量占满时请求的冷启动仍能立即开始，不用排在一整轮浏览器启动之后；按需启动排队时，释放出的名额也先给它们
type startupSlots struct {
	mu sync.Mutex
	// active 为已占用的名额（按需与后台合计），queued 与 deferred 为正在申请（含排队）的按需与后台启动数
	active   int
	queued   int
	deferred int
	// changed 在名额释放、按需启动离开队列或容量变化时关闭，唤醒排队者重新检查；没有排队者时为 nil
	changed chan struct{}
}

// acquire 取得一个冷启动名额，返回释放函数（重复调用只释放一次）。capacity 返回当前容量，
// background 标记后台启动；需要排队时先调用一次 onWait（不持有锁）。ctx 结束时放弃排队并返回 ctx 的错误
func (slots *startupSlots) acquire(
	ctx context.Context,
	capacity func() int,
	background bool,
	onWait func(active int, limit int),
) (func(), error) {
	slots.mu.Lock()
	if background {
		slots.deferred++
	} else {
		slots.queued++
	}
	notified := false
	for {
		limit := max(capacity(), 1)
		if !background {
			limit++
		}
		if slots.active < limit && (!background || slots.queued == 0) {
			break
		}
		if slots.changed == nil {
			slots.changed = make(chan struct{})
		}
		changed, active := slots.changed, slots.active
		slots.mu.Unlock()
		if !notified && onWait != nil {
			notified = true
			onWait(active, limit)
		}
		select {
		case <-ctx.Done():
			slots.mu.Lock()
			slots.leaveLocked(background)
			slots.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
		}
		slots.mu.Lock()
	}
	slots.leaveLocked(background)
	slots.active++
	slots.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			slots.mu.Lock()
			slots.active--
			slots.broadcastLocked()
			slots.mu.Unlock()
		})
	}, nil
}

// leaveLocked 结束一次申请；按需启动全部离开时唤醒为它们让行的后台启动
func (slots *startupSlots) leaveLocked(background bool) {
	if background {
		slots.deferred--
		return
	}
	slots.queued--
	if slots.queued == 0 {
		slots.broadcastLocked()
	}
}

// resized 在容量热更新后唤醒排队者按新容量重新检查
func (slots *startupSlots) resized() {
	slots.mu.Lock()
	slots.broadcastLocked()
	slots.mu.Unlock()
}

// usage 返回已占用的名额与排队中的按需、后台启动数
func (slots *startupSlots) usage() (int, int, int) {
	slots.mu.Lock()
	defer slots.mu.Unlock()
	return slots.active, slots.queued, slots.deferred
}

func (slots *startupSlots) broadcastLocked() {
	if slots.changed != nil {
		close(slots.changed)
		slots.changed = nil
	}
}

type backgroundStartupKey struct{}

// withBackgroundStartup 标记由预热发起的 Worker 启动：冷启动名额让给按需启动
func withBackgroundStartup(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundStartupKey{}, true)
}

func backgroundStartup(ctx context.Context) bool {
	background, _ := ctx.Value(backgroundStartupKey{}).(bool)
	return background
}

// acquireStartupSlot 为一次浏览器冷启动取得名额。纯 Go 后端不启动浏览器，只受活动 Worker 上限约束，不占名额。
// 后台启动等待名额时到期按槽位已满返回（预热本轮不再启动新的，稍后重试），不记为账户预热失败
func (manager *accountWorkerManager) acquireStartupSlot(ctx context.Context, label string, executablePath string) (func(), error) {
	if executablePath == "" {
		return func() {}, nil
	}
	background := backgroundStartup(ctx)
	release, err := manager.startupSlots.acquire(ctx, manager.warmConcurrencyValue, background, func(active int, limit int) {
		kind := "按需"
		if background {
			kind = "预热"
		}
		manager.requests.log(label, "INFO", fmt.Sprintf(
			"WAA Worker 启动 | 等待冷启动名额 | 类型=%s | 占用=%d/%d", kind, active, limit,
		))
	})
	if err != nil && background {
		return nil, fmt.Errorf("%w: 等待冷启动名额: %w", errAccountWorkerCapacity, err)
	}
	return release, err
}
