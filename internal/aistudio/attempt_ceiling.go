package aistudio

import (
	"context"
	"sync/atomic"
)

// 不固定账户的数据请求（CountTokens、Live、转写、Veo）的换号上限。原先上限是请求号池内可调度的账户数（accountAttemptLimit），
// 线上 1.15 万个账户时等于没有上限：上游对整个出口限流时，一个请求会在同一个出口上把账户挨个试下去，每次失败都冷却一个账户。
// app 层在这些请求的入口设 WithAttemptCeiling（1 次加换 3 个），小号池（账户数不超过上限）时行为不变。
// 这些路径的 429 仍由 markRetryableFailure 按 30 秒冷却，不逐级，也不作为出口判定的证据

type attemptCeilingKey struct{}

// WithAttemptCeiling 给不固定账户的数据请求（CountTokens、Live、转写、Veo）设换号上限：accountAttemptLimit 返回 min(可调度账户数, n)；
// n 不大于 0 时原样返回
func WithAttemptCeiling(ctx context.Context, n int) context.Context {
	if n <= 0 {
		return ctx
	}
	return context.WithValue(ctx, attemptCeilingKey{}, n)
}

// attemptCeilingFrom 返回 WithAttemptCeiling 设置的上限，没有设置时为 0（不限）
func attemptCeilingFrom(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	ceiling, _ := ctx.Value(attemptCeilingKey{}).(int)
	return ceiling
}

// attemptLimitScans 为 accountAttemptLimit 统计可调度账户的次数（在账户池锁内扫描全部账户），只给包内测试核对每个请求只算一次
var attemptLimitScans atomic.Int64
