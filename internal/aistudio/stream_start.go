package aistudio

import (
	"context"
	"sync"
	"time"
)

// StreamStartHold 让生成服务在请求自己现场冷启动 Worker 时推迟流式请求写出响应头：API 层写出响应头前最多等首个事件一段时间
// （StreamStartWait 给出的下限），到时还没有事件就先回 200 再推流，之后的错误只能放进流里。请求自己现场冷启动的浏览器启动
// 要 10–30 秒以上，比这个下限长：启动结束后上游才回的额度 429 原先成了 200 之后的流内错误，没有 Retry-After，用量账本也记 200。
// 生成服务在启动开始时调用 BeginColdStart 并给出这次启动最多还能延长的等待（排队预算还能抵扣的启动时长），结束时调用 EndColdStart
// 并给出按实测时长延长的等待（不超过开始时给出的上限）；API 层在启动进行中把等待截止时间先延后这个上限，启动结束后改为按实测时长延后
// （见 api.awaitStreamStart）：推迟的总时长不超过排队预算能抵扣的启动时长（INIT_TIMEOUT），抵扣用完后的启动不再推迟；API 层另把
// 推迟限制在开始等首个事件后 50 秒内（api.streamStartHoldMax），免得前面的反向代理按读超时断开。
// 只有需要这样做的请求（Ultra 主用模型，见 app 包 primary_models.go）由生成服务调用；API 层只为流式请求创建，同一时间只有一个等待方。
// nil 安全：方法都可以在 nil 上调用
type StreamStartHold struct {
	mu sync.Mutex
	// extension 为已结束的现场冷启动按实测时长延长的等待，starting 为进行中的现场冷启动数，
	// pending 为进行中的现场冷启动最多还能延长的等待（开始时给出的上限中最大的一个，全部结束后清零）
	extension time.Duration
	starting  int
	pending   time.Duration
	// changed 在状态变化时非阻塞地写入（容量 1），唤醒等待方复查
	changed chan struct{}
}

// NewStreamStartHold 创建一份写出响应头前的等待延长记录
func NewStreamStartHold() *StreamStartHold {
	return &StreamStartHold{changed: make(chan struct{}, 1)}
}

type streamStartHoldKey struct{}

// WithStreamStartHold 把等待延长记录放进请求 context，交给生成服务；hold 为 nil 时原样返回
func WithStreamStartHold(ctx context.Context, hold *StreamStartHold) context.Context {
	if hold == nil {
		return ctx
	}
	return context.WithValue(ctx, streamStartHoldKey{}, hold)
}

// StreamStartHoldFromContext 返回请求 context 里的等待延长记录；非流式请求与没有经过 API 层的调用为 nil
func StreamStartHoldFromContext(ctx context.Context) *StreamStartHold {
	if ctx == nil {
		return nil
	}
	hold, _ := ctx.Value(streamStartHoldKey{}).(*StreamStartHold)
	return hold
}

// BeginColdStart 标记请求自己的现场冷启动开始，之后必须调用 EndColdStart：结束前 API 层写出响应头前的等待最多再延长 limit
// （这次启动还能抵扣的时长；不大于 0 时不延长，调用方可以不调用）。上限不随启动时长增加：启动比它久时到时照常写出响应头，
// 不让一次抵扣不了的启动把响应头一直压着
func (hold *StreamStartHold) BeginColdStart(limit time.Duration) {
	if hold == nil {
		return
	}
	hold.mu.Lock()
	hold.starting++
	hold.pending = max(hold.pending, limit)
	hold.mu.Unlock()
	hold.notify()
}

// EndColdStart 标记现场冷启动结束（成功或失败都要调用），并把写出响应头前的等待延长 extend（不大于 0 时不延长；调用方保证不超过
// 开始时给出的 limit，结束后的截止时间因此不会晚于启动进行中的）
func (hold *StreamStartHold) EndColdStart(extend time.Duration) {
	if hold == nil {
		return
	}
	hold.mu.Lock()
	if hold.starting > 0 {
		hold.starting--
	}
	if hold.starting == 0 {
		hold.pending = 0
	}
	hold.extension += max(extend, 0)
	hold.mu.Unlock()
	hold.notify()
}

// Extension 返回写出响应头前的等待当前要延长多久：已结束的现场冷启动按实测时长延长的等待，有启动在进行中时另加它最多还能延长的等待
func (hold *StreamStartHold) Extension() time.Duration {
	if hold == nil {
		return 0
	}
	hold.mu.Lock()
	defer hold.mu.Unlock()
	if hold.starting > 0 {
		return hold.extension + hold.pending
	}
	return hold.extension
}

// State 返回已延长的等待与是否有现场冷启动在进行中
func (hold *StreamStartHold) State() (time.Duration, bool) {
	if hold == nil {
		return 0, false
	}
	hold.mu.Lock()
	defer hold.mu.Unlock()
	return hold.extension, hold.starting > 0
}

// Changed 返回状态变化的通知通道；nil 上为 nil 通道（永远不就绪）
func (hold *StreamStartHold) Changed() <-chan struct{} {
	if hold == nil {
		return nil
	}
	return hold.changed
}

func (hold *StreamStartHold) notify() {
	select {
	case hold.changed <- struct{}{}:
	default:
	}
}
