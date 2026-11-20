package app

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 候选全部冷却时排队的累计上限与请求期限（上游 Mag1cFall/AIStudio2API cd3d4ab 1A、6d8ad7d 7C 期限部分）

// TestCoolingWaitExceeded 按固定时刻判断：距最早恢复超过 cooldownQueueLimit 减去已累计的冷却排队、或恢复晚于上下文期限时不再排队
func TestCoolingWaitExceeded(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	background := context.Background()
	var none *coolingWait
	if none.exceeded(background, now.Add(59*time.Second), now) || !none.exceeded(background, now.Add(61*time.Second), now) {
		t.Fatal("没有累计时以 cooldownQueueLimit（1 分钟）为界")
	}
	waited := &coolingWait{}
	waited.add(50 * time.Second)
	if !waited.exceeded(background, now.Add(15*time.Second), now) || waited.exceeded(background, now.Add(9*time.Second), now) {
		t.Fatal("已累计 50s 时只剩 10s 可等")
	}
	deadlineCtx, cancel := context.WithDeadline(background, now.Add(5*time.Second))
	defer cancel()
	if !none.exceeded(deadlineCtx, now.Add(10*time.Second), now) || none.exceeded(deadlineCtx, now.Add(3*time.Second), now) {
		t.Fatal("最早恢复晚于上下文期限时不应排队，早于期限时照常排队")
	}
}

// TestAcquireLeaseCoolingAfterDeadline 唯一账号冷却 20 秒（不到 1 分钟）、选号上下文 500ms 后到期：恢复晚于期限，立即按全部冷却返回
// （429 + Retry-After），不再等到期限以 504 结束
func TestAcquireLeaseCoolingAfterDeadline(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA)
	until := time.Now().Add(20 * time.Second)
	if err := pool.MarkCooldownIfGeneration(primaryTestA, testRuntimeModel, pool.ModelAccessGeneration(primaryTestA), time.Now(), until, "分钟限额"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(withCoolingWait(service.lifecycle), 500*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	lease, err := service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if lease != nil {
		_ = lease.Release()
	}
	var cooling *aistudio.AllCoolingError
	if !errors.As(err, &cooling) {
		t.Fatalf("err = %v，期望全部冷却", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= 300*time.Millisecond {
		t.Fatalf("%s 后才返回，期望立即返回", elapsed)
	}
	if cooling.Until.Sub(until).Abs() > time.Second {
		t.Fatalf("最早恢复 = %s，期望 %s", cooling.Until, until)
	}
}

// TestAcquireLeaseCoolingCumulative 冷却排队按整个请求累计：同一份累计里已等了 59.8 秒时，600ms 后恢复的冷却也不再等、立即按全部冷却返回；
// 新的一份累计照常等到恢复取得租约，等的时间计入累计
func TestAcquireLeaseCoolingCumulative(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA)
	cool := func() {
		t.Helper()
		if err := pool.MarkCooldownIfGeneration(primaryTestA, testRuntimeModel, pool.ModelAccessGeneration(primaryTestA), time.Now(),
			time.Now().Add(600*time.Millisecond), "分钟限额"); err != nil {
			t.Fatal(err)
		}
	}
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}

	cool()
	spent := withCoolingWait(service.lifecycle)
	coolingWaitFromContext(spent).add(cooldownQueueLimit - 200*time.Millisecond)
	startedAt := time.Now()
	lease, err := service.acquireWarmLease(spent, selection)
	if lease != nil {
		_ = lease.Release()
	}
	var cooling *aistudio.AllCoolingError
	if !errors.As(err, &cooling) || time.Since(startedAt) >= 300*time.Millisecond {
		t.Fatalf("累计已近上限时 err = %v（%s），期望立即按全部冷却返回", err, time.Since(startedAt))
	}

	fresh := withCoolingWait(service.lifecycle)
	startedAt = time.Now()
	lease, err = service.acquireWarmLease(fresh, selection)
	if err != nil {
		t.Fatalf("新的累计应等到冷却结束取得租约，实际 %v", err)
	}
	_ = lease.Release()
	if elapsed := time.Since(startedAt); elapsed < 500*time.Millisecond {
		t.Fatalf("%s 就取得了租约，期望等到冷却结束", elapsed)
	}
	if waited := coolingWaitFromContext(fresh).waited(); waited < 500*time.Millisecond {
		t.Fatalf("累计的冷却排队 = %s，期望计入这次约 600ms 的等待", waited)
	}
}

// TestGenerateCoolingAfterDeadlineFastFails 生成请求的唯一账号冷却 30 秒、首事件前期限 1 秒：选号立即按全部冷却结束，请求以 AllCoolingError
// 结束（对外 429），不等到期限以 504 结束
func TestGenerateCoolingAfterDeadlineFastFails(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA)
	if err := pool.MarkCooldownIfGeneration(primaryTestA, testRuntimeModel, pool.ModelAccessGeneration(primaryTestA), time.Now(),
		time.Now().Add(30*time.Second), "分钟限额"); err != nil {
		t.Fatal(err)
	}
	request := aistudio.GenerateRequest{
		ID: "req-cooling-deadline", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	startedAt := time.Now()
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, startedAt, request, "", events, newGenerationDiagnostics(request, "未发送"))
	var err error
	for event := range events {
		if event.Kind == aistudio.EventError {
			err = event.Err
		}
	}
	var cooling *aistudio.AllCoolingError
	if !errors.As(err, &cooling) {
		t.Fatalf("err = %v，期望全部冷却", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= 800*time.Millisecond {
		t.Fatalf("%s 后才结束，期望不等到期限", elapsed)
	}
}

// coolingSwapUpstream 第一次生成返回裸限流 429，同时让其余账号在 testRuntimeModel 上冷却 swap；之后照 bareRateUpstream 正常返回
type coolingSwapUpstream struct {
	*bareRateUpstream
	pool     *aistudio.AccountPool
	accounts []string
	swap     time.Duration
	once     sync.Once
}

func (upstream *coolingSwapUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	first := false
	upstream.once.Do(func() { first = true })
	if !first {
		return upstream.bareRateUpstream.Generate(ctx, request)
	}
	for _, accountID := range upstream.accounts {
		if accountID != request.AccountID {
			_ = upstream.pool.MarkCooldownIfGeneration(accountID, testRuntimeModel, upstream.pool.ModelAccessGeneration(accountID), time.Now(),
				time.Now().Add(upstream.swap), "分钟限额")
		}
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID)
	upstream.mu.Unlock()
	return nil, bareRateLimitError()
}

// TestGenerateCoolingWaitSharedAcrossAttempts 冷却排队按整个请求累计（经请求上下文注入 1 秒的上限）：第一次选号等了约 700ms 冷却，
// 额度失败换号后另一个账号又要冷却约 700ms，累计超过上限，立即结束，不再等第二次（原先每次选号各自从 0 计）
func TestGenerateCoolingWaitSharedAcrossAttempts(t *testing.T) {
	accounts := []string{"swap-a@example.com", "swap-b@example.com"}
	upstream := &coolingSwapUpstream{bareRateUpstream: &bareRateUpstream{limited: map[string]bool{}}, accounts: accounts, swap: 700 * time.Millisecond}
	service, pool := firstEventTestService(t, upstream, 0, accounts...)
	upstream.pool = pool
	for _, accountID := range accounts {
		if err := pool.MarkCooldownIfGeneration(accountID, testRuntimeModel, pool.ModelAccessGeneration(accountID), time.Now(),
			time.Now().Add(700*time.Millisecond), "分钟限额"); err != nil {
			t.Fatal(err)
		}
	}
	text, err, elapsed := runPrimaryRequestWith(t, service, withCoolingWaitLimit(service.lifecycle, time.Second), "req-cooling-shared", "")
	if text != "" || err == nil {
		t.Fatalf("累计超过上限后应立即结束：text=%q err=%v，调用 %v", text, err, upstream.called())
	}
	var cooling *aistudio.AllCoolingError
	var quota *aistudio.PrimaryQuotaError
	if !errors.As(err, &cooling) && !errors.As(err, &quota) {
		t.Fatalf("err = %v，期望按全部冷却或额度不足（429）结束", err)
	}
	if calls := upstream.called(); len(calls) != 1 {
		t.Fatalf("上游生成调用 = %v，期望只有第一次", calls)
	}
	if elapsed >= 1300*time.Millisecond {
		t.Fatalf("%s 后才结束，期望第一次等冷却后不再等第二次", elapsed)
	}
}

// TestModelFallbackCoolingQueueSpent 普通路径（MODEL_FALLBACKS 范围 all）argon 在全部账户上冷却 2 秒、这个请求的冷却排队上限只剩不到
// 2 秒：不再原地等 argon 恢复，按全部冷却结束 argon 的选号并照常改用 carbon，上游只收到 carbon，不等 2 秒
func TestModelFallbackCoolingQueueSpent(t *testing.T) {
	accounts := ultraTestIDs(2)
	upstream := &fallbackUpstream{}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierFree, config.ModelFallbackScopeAll, accounts...)
	coolAll(t, pool, fallbackSource, 2*time.Second, accounts...)
	record := aistudio.NewModelSwitch()
	ctx := aistudio.ContextWithModelSwitch(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeNormal), record)
	ctx, cancel := context.WithTimeout(withCoolingWaitLimit(ctx, 500*time.Millisecond), 20*time.Second)
	defer cancel()
	startedAt := time.Now()
	events, err := service.Generate(ctx, aistudio.GenerateRequest{
		ID: "req-fallback-cooling-spent", Model: fallbackSource,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventError:
			err = event.Err
		}
	}
	if err != nil || text != "reply from "+fallbackTarget || record.Model(fallbackSource) != fallbackTarget {
		t.Fatalf("text=%q err=%v served=%q，期望改用 carbon 成功", text, err, record.Model(fallbackSource))
	}
	if models := upstream.models(); !slices.Equal(models, []string{fallbackTarget}) {
		t.Fatalf("上游收到 %v，期望只有 carbon（argon 不等恢复）", models)
	}
	if elapsed := time.Since(startedAt); elapsed >= 1500*time.Millisecond {
		t.Fatalf("%s 后才结束，期望不原地等 argon 恢复", elapsed)
	}
}
