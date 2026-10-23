package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 出口限流期间冷启动闸门与排队的边界情况（第 1 轮复查）：补覆盖拿不到槽位、被闸门拒掉的替换不占名额、
// 固定账户的选号被暂停、转写等数据路径经 Worker() 的冷启动

// TestEgressCoverageNoSlotBounded 补覆盖选中的备用账号拿不到槽位（分区已满、没有可替换的空闲 Worker）时，非主用模型按
// “驻留账号该模型额度已用完”排队，受出口排队上限约束；原先按没有空闲的 Worker 槽位一直排到请求期限
func TestEgressCoverageNoSlotBounded(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com"}, 1, 1, 0)
	addCoverageModel(t, env.pool, "b@example.com")
	forceEgressLimited(env.service, 7*time.Second)
	// a 忙碌：分区已满（1/1）又没有可替换的空闲 Worker
	env.pool.SetPerAccountConcurrency(1)
	release := holdLease(t, env.pool, "a@example.com")
	defer release()
	env.service.egressQueueWait = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
	defer cancel()
	startedAt := time.Now()
	_, err := env.service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: egressCoverageModel, Method: "generateContent"})
	elapsed := time.Since(startedAt)
	var busy *aistudio.CapacityBusyError
	if !errors.As(err, &busy) || busy.Reason != waitReasonResidentExhausted {
		t.Fatalf("err=%v，期望按驻留账号该模型额度已用完排队超限", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("用时 %s，应受出口排队上限（300ms）约束", elapsed)
	}
	if launched := env.launches(); len(launched) != 0 {
		t.Fatalf("没有槽位时冷启动了 %v", launched)
	}
}

// TestEgressReplacementCountedAfterGate 按需替换的每分钟次数在冷启动闸门放行后才记：出口限流期间被闸门拒掉的替换不占名额，
// 闸门放行的补覆盖照常计一次
func TestEgressReplacementCountedAfterGate(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com", "c@example.com"}, 1, 1, 0)
	env.manager.replacements.limit.Store(replacementsPerMinute)
	forceEgressLimited(env.service, 7*time.Second)
	ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
	defer cancel()
	count := func() int {
		env.manager.replacements.mu.Lock()
		defer env.manager.replacements.mu.Unlock()
		return len(env.manager.replacements.times[aistudio.PoolScopeNormal])
	}
	for range replacementsPerMinute + 2 {
		if _, err := env.manager.promote(ctx, "b@example.com", testRuntimeModel); !errors.Is(err, errEgressColdStartPaused) {
			t.Fatalf("出口限流期间的替换应被闸门暂停，实际 %v", err)
		}
	}
	if got := count(); got != 0 {
		t.Fatalf("被闸门拒掉的替换记了 %d 次", got)
	}
	if !env.manager.replacementAllowed(ctx, aistudio.PoolScopeNormal) {
		t.Fatal("被拒的尝试不应用掉替换名额")
	}
	if _, err := env.manager.promote(withWorkerStartSource(ctx, workerStartCoverage), "c@example.com", testRuntimeModel); err != nil {
		t.Fatalf("补覆盖应按配额放行：%v", err)
	}
	if got := count(); got != 1 {
		t.Fatalf("闸门放行的补覆盖替换应记 1 次，实际 %d", got)
	}
}

// TestEgressFixedAccountPausedReturns 固定账户的选号要冷启动、被出口闸门暂停时直接返回（Worker 重建后的重放据此换号），
// 不按“Worker 容量已满”每秒重试到请求期限
func TestEgressFixedAccountPausedReturns(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com"}, 1, 1, 0)
	forceEgressLimited(env.service, 7*time.Second)
	ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
	defer cancel()
	startedAt := time.Now()
	lease, err := env.service.acquireWarmLease(ctx, aistudio.AccountSelection{
		ModelID: testRuntimeModel, Method: "generateContent", AccountID: "b@example.com",
	})
	if lease != nil || !errors.Is(err, errEgressColdStartPaused) {
		t.Fatalf("lease=%v err=%v，期望冷启动被暂停的错误", lease, err)
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("用时 %s，应立即返回", elapsed)
	}
	if launched := env.launches(); len(launched) != 0 {
		t.Fatalf("冷启动被暂停时启动了 %v", launched)
	}
}

// TestEgressDataPathWorkerGated 转写、Live、Veo 经 Worker() 的冷启动按“按需”过冷启动闸门：出口限流期间超出常驻的扩容暂停，
// 按号池繁忙 429 返回（带 Retry-After，errors.Is 仍认 errEgressColdStartPaused），不冷启动；生成请求标为重开的照常启动
func TestEgressDataPathWorkerGated(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com", "c@example.com"}, 1, 3, 0)
	forceEgressLimited(env.service, 7*time.Second)
	ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
	defer cancel()
	_, err := env.manager.Worker(ctx, "b@example.com", testRuntimeModel)
	var busy *aistudio.CapacityBusyError
	if !errors.As(err, &busy) || !errors.Is(err, errEgressColdStartPaused) || busy.Reason != egressColdStartPausedReason {
		t.Fatalf("err=%v，期望冷启动被闸门暂停、按号池繁忙返回", err)
	}
	if until := time.Until(busy.Until); until <= 0 || until > egressRetryMin+time.Second {
		t.Fatalf("建议重试 %s 后，期望约 %s", until, egressRetryMin)
	}
	if launched := env.launches(); len(launched) != 0 {
		t.Fatalf("数据路径绕过闸门冷启动了 %v", launched)
	}
	if _, err := env.manager.Worker(withWorkerStartSource(ctx, workerStartReopen), "c@example.com", testRuntimeModel); err != nil {
		t.Fatalf("重开不经闸门：%v", err)
	}
	if launched := env.launches(); len(launched) != 1 || launched[0] != "c@example.com" {
		t.Fatalf("冷启动 %v，期望只有重开的 c", launched)
	}
}

// 第 2 轮复查：转写、Live、Veo 的候选与出口限流、固定账户的选号被闸门暂停

// TestEgressDataPathCandidatesWarmOnly 出口限流期间转写、Live、Veo 的候选去掉受限出口上的备用账户（还有热 Worker 时）：这些路径由账户池
// 按 ID 顺序在候选里选号，原先几乎总选中要冷启动的备用账号，被冷启动闸门暂停后按号池繁忙 429 返回，空闲的热 Worker 用不上。
// 没有热候选时照旧保留备用账户，交给冷启动闸门；没有出口受限时候选不变、Veo 不限定候选
func TestEgressDataPathCandidatesWarmOnly(t *testing.T) {
	env := newEgressEnv(t, []string{"z@example.com"}, []string{"a@example.com", "b@example.com"}, 1, 3, 0)
	env.manager.accounts["z@example.com"].bootstrapModel = testRuntimeModel
	for _, accountID := range []string{"z@example.com", "a@example.com", "b@example.com"} {
		if err := env.pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{{
			ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens", "bidiGenerateContent", "predictLongRunning"},
			Capabilities: map[string]bool{"chat_model": true, "transcription_output": true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
	defer cancel()
	check := func(stage string, want []string, wantVideo []string) {
		t.Helper()
		transcription, err := env.service.transcriptionCandidates(ctx, testRuntimeModel)
		if err != nil || !slices.Equal(transcription, want) {
			t.Fatalf("%s：转写候选 %v err=%v，期望 %v", stage, transcription, err, want)
		}
		bidi, err := env.service.bidiCandidates(ctx, testRuntimeModel, "")
		if err != nil || !slices.Equal(bidi, want) {
			t.Fatalf("%s：Live 候选 %v err=%v，期望 %v", stage, bidi, err, want)
		}
		if video := env.service.videoCandidates(ctx, testRuntimeModel); !slices.Equal(video, wantVideo) {
			t.Fatalf("%s：Veo 候选 %v，期望 %v", stage, video, wantVideo)
		}
	}
	check("出口正常", []string{"z@example.com", "a@example.com", "b@example.com"}, nil)
	forceEgressLimited(env.service, 7*time.Second)
	check("出口限流", []string{"z@example.com"}, []string{"z@example.com"})
	// 按收窄后的候选选号拿到热 Worker 的账号，经 Worker() 取 preparer 不冷启动（原先拿到 a、被闸门暂停后 429）
	candidates, _ := env.service.transcriptionCandidates(ctx, testRuntimeModel)
	lease, err := env.pool.AcquireFor(ctx, aistudio.AccountSelection{
		ModelID: testRuntimeModel, ModelAccessScope: testRuntimeModel, Method: "generateContent",
		Capability: "transcription_output", AllowedAccountIDs: candidates,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := lease.Account().ID; got != "z@example.com" {
		t.Fatalf("选中 %s，期望热 Worker 的 z", got)
	}
	if _, err := env.manager.Worker(ctx, lease.Account().ID, testRuntimeModel); err != nil {
		t.Fatalf("热 Worker 应直接可用：%v", err)
	}
	_ = lease.Release()
	if launched := env.launches(); len(launched) != 0 {
		t.Fatalf("冷启动了 %v", launched)
	}
	// 热 Worker 冷却后没有热候选：照旧保留备用账户，交给冷启动闸门（Veo 不限定）
	coolModel(t, env.pool, "z@example.com", "", 10*time.Minute, "测试")
	check("没有热候选", []string{"a@example.com", "b@example.com"}, nil)
}

// TestEgressPinnedGenerationPausedBusy 出口限流期间指定账户（没有文件绑定）的生成请求要冷启动、被闸门暂停：按号池繁忙 429 返回
// （带 Retry-After，原因为出口限流中、冷启动按配额进行），不再以裸错误按 502 结束；不冷启动
func TestEgressPinnedGenerationPausedBusy(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com"}, 1, 1, 0)
	forceEgressLimited(env.service, 7*time.Second)
	text, err, _ := runModelRequest(t, env.service, "req-egress-pinned", "b@example.com", testRuntimeModel)
	var busy *aistudio.CapacityBusyError
	if text != "" || !errors.As(err, &busy) || busy.Reason != egressColdStartPausedReason {
		t.Fatalf("text=%q err=%v，期望按号池繁忙（出口限流中，冷启动按配额进行）返回", text, err)
	}
	if !errors.Is(err, errEgressColdStartPaused) {
		t.Fatalf("err=%v 应仍认 errEgressColdStartPaused", err)
	}
	var status interface{ HTTPStatus() int }
	if !errors.As(err, &status) || status.HTTPStatus() != http.StatusTooManyRequests {
		t.Fatalf("err=%v，期望按 HTTP 429 返回", err)
	}
	if until := time.Until(busy.Until); until <= 0 || until > egressRetryMin+time.Second {
		t.Fatalf("建议重试 %s 后，期望约 %s", until, egressRetryMin)
	}
	if launched := env.launches(); len(launched) != 0 {
		t.Fatalf("冷启动被暂停时启动了 %v", launched)
	}
}
