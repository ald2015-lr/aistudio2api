package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// forbiddenUpstream 让 denied 中的账号对每次生成返回 HTTP 403 / 协议错误码 7（对该模型没有权限），quota 中的账号返回每日限额 429，
// 其余账号正常返回；delay 为返回前的上游耗时
type forbiddenUpstream struct {
	mu     sync.Mutex
	denied map[string]bool
	quota  map[string]bool
	delay  time.Duration
	calls  []string
}

func (upstream *forbiddenUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *forbiddenUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *forbiddenUpstream) called() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

func (upstream *forbiddenUpstream) setDenied(accountID string, denied bool) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	upstream.denied[accountID] = denied
}

func (upstream *forbiddenUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID)
	denied, quota, delay := upstream.denied[request.AccountID], upstream.quota[request.AccountID], upstream.delay
	upstream.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if denied {
		return nil, &aistudio.RPCError{
			Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7,
			Message: "The caller does not have permission",
		}
	}
	if quota {
		return nil, &aistudio.RPCError{
			Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
			Message: "You exceeded your current quota", RetryDelay: 2 * time.Hour,
			Metadata: map[string]string{"quota_limit": "GenerateRequestsPerDayPerProjectPerModel-FreeTier"},
		}
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

const (
	forbiddenBad  = "a-bad@example.com"
	forbiddenGood = "b-good@example.com"
)

// runAccountRequest 执行一次 testRuntimeModel 的生成请求（accountID 不为空时固定账号），返回正文、错误与用时
func runAccountRequest(t *testing.T, service *trackedService, requestID string, accountID string) (string, error, time.Duration) {
	t.Helper()
	return runModelRequest(t, service, requestID, accountID, testRuntimeModel)
}

// runModelRequest 与 runAccountRequest 相同，但请求指定的模型
func runModelRequest(t *testing.T, service *trackedService, requestID string, accountID string, modelID string) (string, error, time.Duration) {
	t.Helper()
	request := aistudio.GenerateRequest{
		ID: requestID, Model: modelID, AccountID: accountID,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	startedAt := time.Now()
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, startedAt, request, "", events, newGenerationDiagnostics(request, "未发送"))
	var text string
	var err error
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventError:
			err = event.Err
		}
	}
	return text, err, time.Since(startedAt)
}

// modelCooldownUntil 返回账号在该作用域上的冷却到期时间与原因；没有冷却时 ok 为 false
func modelCooldownUntil(t *testing.T, pool *aistudio.AccountPool, accountID string, scope string) (time.Time, string, bool) {
	t.Helper()
	status, exists := pool.StatusOf(accountID)
	if !exists {
		t.Fatalf("账号 %s 不存在", accountID)
	}
	cooldown, ok := status.Cooldowns[scope]
	if !ok || !cooldown.Until.After(time.Now()) {
		return time.Time{}, "", false
	}
	return cooldown.Until, cooldown.Reason, true
}

// expireModelCooldown 模拟单模型 403 冷却到期：清除账号在该冷却作用域（Playground 为模型 ID）上的冷却，并把冷却档位记录的
// 到期时间移到过去（档位按距上一次 403 的时间计，记录本身保留）
func expireModelCooldown(t *testing.T, service *trackedService, accountID string, scope string) {
	t.Helper()
	pool := service.pool
	if err := pool.ClearCooldownIfGeneration(accountID, scope, pool.ModelAccessGeneration(accountID), time.Now()); err != nil {
		t.Fatal(err)
	}
	tracker := service.forbidden
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	key := forbiddenModelKey{accountID: accountID, scope: scope}
	if strike, exists := tracker.models[key]; exists {
		strike.until = time.Now().Add(-time.Second)
		tracker.models[key] = strike
	}
}

// assertCooldownAround 断言冷却到期时间约为 now + want（允许几秒误差）
func assertCooldownAround(t *testing.T, until time.Time, want time.Duration, label string) {
	t.Helper()
	if got := time.Until(until); got < want-5*time.Second || got > want+5*time.Second {
		t.Fatalf("%s：冷却剩余 %s，期望约 %s", label, got.Round(time.Second), want)
	}
}

// TestModelForbiddenCoolsAccount 账号对某个模型返回 403 无权限时只被白耗一次：该通道该模型写入 30 分钟冷却（状态里可见，
// 原因写明无权限），之后的请求直接选别的账号；这次换号不退避，驻留 Worker 成了该模型的死槽位时登记淘汰，并写冷却日志
func TestModelForbiddenCoolsAccount(t *testing.T) {
	upstream := &forbiddenUpstream{denied: map[string]bool{forbiddenBad: true}}
	service, pool := primaryBusyService(t, upstream, forbiddenBad, forbiddenGood)
	preverifyRuntimeModelAccess(t, pool, forbiddenGood)
	const requests = 4
	for index := range requests {
		text, err, elapsed := runAccountRequest(t, service, fmt.Sprintf("req-forbidden-%d", index), "")
		if err != nil || text != "hello" {
			t.Fatalf("请求 %d：text=%q err=%v", index, text, err)
		}
		if index == 0 && elapsed >= 180*time.Millisecond {
			t.Fatalf("单模型 403 换号用时 %s，期望不再退避（原先固定退避 200ms）", elapsed)
		}
	}
	calls := upstream.called()
	want := []string{forbiddenBad, forbiddenGood, forbiddenGood, forbiddenGood, forbiddenGood}
	if !slices.Equal(calls, want) {
		t.Fatalf("尝试顺序 = %v，期望 403 的账号只被尝试一次：%v", calls, want)
	}
	until, reason, cooling := modelCooldownUntil(t, pool, forbiddenBad, testRuntimeModel)
	if !cooling || !strings.Contains(reason, "无权限（模型）") || !strings.Contains(reason, "The caller does not have permission") {
		t.Fatalf("403 的账号应有该模型的冷却：cooling=%v reason=%q", cooling, reason)
	}
	assertCooldownAround(t, until, forbiddenModelFirstCooldown, "第一次 403")
	if !pool.AccountCoolingDown(forbiddenBad, testRuntimeModel) {
		t.Fatal("403 的账号应对该模型处于冷却")
	}
	if pool.AccountCoolingDown(forbiddenBad, "") {
		t.Fatal("单模型 403 不应暂停整个账号")
	}
	if !hasLogContaining(service.requests, "账号冷却", "类型=无权限", "范围="+testRuntimeModel) {
		t.Fatalf("应记录无权限冷却日志，日志 = %v", serviceLogMessages(service.requests))
	}
	if got := service.workers.retirementCount(); got != 1 {
		t.Fatalf("唯一启用通道冷却后驻留 Worker 成了死槽位，应登记淘汰，登记数 = %d", got)
	}
}

// TestModelForbiddenCountsTowardFailureLimit 多个账号对该模型都返回 403：每个都写入冷却、换号不退避，但仍计入连续失败上限，
// 一个请求最多试 generateFailureLimit 个账号后按 403 结束，不会把号池逐个试完；下一个请求跳过已冷却的账号，在没有 403 的账号上完成
func TestModelForbiddenCountsTowardFailureLimit(t *testing.T) {
	ids := make([]string, 0, 7)
	denied := make(map[string]bool)
	for index := range 6 {
		id := fmt.Sprintf("a-bad-%d@example.com", index)
		ids = append(ids, id)
		denied[id] = true
	}
	ids = append(ids, forbiddenGood)
	upstream := &forbiddenUpstream{denied: denied}
	service, pool := primaryBusyService(t, upstream, ids...)
	preverifyRuntimeModelAccess(t, pool, forbiddenGood)
	// 号池里该模型刚成功过：这些 403 是各账号没有权限，不是号池级 403
	service.forbidden.modelSucceeded(aistudio.PoolScopeAll, testRuntimeModel, time.Now())
	_, err, elapsed := runAccountRequest(t, service, "req-forbidden-many", "")
	var rpc *aistudio.RPCError
	if !errors.As(err, &rpc) || rpc.StatusCode != http.StatusForbidden {
		t.Fatalf("连续 %d 次 403 后应按 403 结束，err=%v calls=%v", generateFailureLimit, err, upstream.called())
	}
	calls := upstream.called()
	if len(calls) != generateFailureLimit {
		t.Fatalf("尝试 = %v，期望最多 %d 次", calls, generateFailureLimit)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("请求用时 %s，期望单模型 403 换号不退避", elapsed)
	}
	for _, id := range calls {
		if _, _, cooling := modelCooldownUntil(t, pool, id, testRuntimeModel); !cooling {
			t.Fatalf("账号 %s 返回 403 后应冷却该模型", id)
		}
	}
	text, err, _ := runAccountRequest(t, service, "req-forbidden-many-2", "")
	if err != nil || text != "hello" {
		t.Fatalf("下一个请求应跳过已冷却的账号并完成：text=%q err=%v calls=%v", text, err, upstream.called())
	}
	if calls := upstream.called(); len(calls) != 7 || calls[6] != forbiddenGood {
		t.Fatalf("尝试 = %v，期望第二个请求只试剩下的 403 账号后到 %s", calls, forbiddenGood)
	}
}

// TestSystemicForbiddenNotCooled 号池级 403（纯 Go proof、请求形态等失效时号池里所有账号都 403）：同一号池同一模型 10 分钟内
// 第 5 个不同账号的 403 起、这段时间又没有任何成功时不再写冷却、不升档，按一般失败计入连续失败；故障持续到第一档冷却到期后
// 不会升到 6 小时，修复后下一个请求立即成功。号池里该模型成功之后，单个账号的 403 照常按账号冷却
func TestSystemicForbiddenNotCooled(t *testing.T) {
	ids := make([]string, 0, 8)
	denied := make(map[string]bool)
	for index := range 8 {
		id := fmt.Sprintf("a-outage-%d@example.com", index)
		ids = append(ids, id)
		denied[id] = true
	}
	upstream := &forbiddenUpstream{denied: denied}
	service, pool := primaryBusyService(t, upstream, ids...)
	var rpc *aistudio.RPCError
	if _, err, _ := runAccountRequest(t, service, "req-outage-1", ""); !errors.As(err, &rpc) || rpc.StatusCode != http.StatusForbidden {
		t.Fatalf("故障期间应按 403 结束，err=%v", err)
	}
	calls := upstream.called()
	if len(calls) != generateFailureLimit {
		t.Fatalf("尝试 = %v，期望 %d 次", calls, generateFailureLimit)
	}
	for index, id := range calls {
		_, _, cooling := modelCooldownUntil(t, pool, id, testRuntimeModel)
		if want := index < forbiddenSystemicAccounts-1; cooling != want {
			t.Fatalf("第 %d 个 403 的账号 %s 冷却=%v，期望 %v（第 %d 个不同账号起按号池级 403）", index+1, id, cooling, want, forbiddenSystemicAccounts)
		}
	}
	// 故障持续到第一档冷却到期：再 403 不升档、不冷却
	for _, id := range calls[:forbiddenSystemicAccounts-1] {
		expireModelCooldown(t, service, id, testRuntimeModel)
	}
	if _, err, _ := runAccountRequest(t, service, "req-outage-2", calls[0]); !errors.As(err, &rpc) {
		t.Fatalf("故障期间应按 403 结束，err=%v", err)
	}
	if _, _, cooling := modelCooldownUntil(t, pool, calls[0], testRuntimeModel); cooling {
		t.Fatal("号池级 403 不应写冷却、升档")
	}
	// 修复上线：下一个请求立即成功
	for _, id := range ids {
		upstream.setDenied(id, false)
	}
	if text, err, _ := runAccountRequest(t, service, "req-outage-fixed", ""); err != nil || text != "hello" {
		t.Fatalf("修复后应立即成功：text=%q err=%v", text, err)
	}
	for _, id := range ids {
		if until, _, cooling := modelCooldownUntil(t, pool, id, testRuntimeModel); cooling && time.Until(until) > forbiddenModelFirstCooldown {
			t.Fatalf("账号 %s 不应留下超过第一档的冷却：%s", id, until)
		}
	}
	// 成功之后单个账号的 403 照常按账号冷却
	upstream.setDenied(calls[1], true)
	if _, err, _ := runAccountRequest(t, service, "req-single-denied", calls[1]); !errors.As(err, &rpc) {
		t.Fatalf("应按 403 结束，err=%v", err)
	}
	if _, _, cooling := modelCooldownUntil(t, pool, calls[1], testRuntimeModel); !cooling {
		t.Fatal("号池里该模型成功之后，单个账号的 403 应冷却")
	}
}

// TestForbiddenSystemicTracker 号池级 403 的判定：同一号池同一模型 10 分钟内不同账号达到 5 个、这段时间没有成功；同一账号重复只算一个，
// 超过窗口的不算；成功一次即清空；号池与模型分开计
func TestForbiddenSystemicTracker(t *testing.T) {
	tracker := newForbiddenTracker()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	observe := func(at time.Time, pool aistudio.PoolScope, accountID string, modelID string, want bool) {
		t.Helper()
		if got := tracker.observeModelDenied(pool, accountID, modelID, at); got != want {
			t.Fatalf("%s %s/%s/%s：号池级=%v，期望 %v", at.Format(time.RFC3339), pool.String(), accountID, modelID, got, want)
		}
	}
	for index := range forbiddenSystemicAccounts - 1 {
		observe(start, aistudio.PoolScopeNormal, fmt.Sprintf("a%d", index), "pro", false)
	}
	observe(start, aistudio.PoolScopeNormal, "a0", "pro", false)
	observe(start, aistudio.PoolScopeUltra, "u0", "pro", false)
	observe(start, aistudio.PoolScopeNormal, "b0", "flash", false)
	observe(start.Add(time.Minute), aistudio.PoolScopeNormal, "a4", "pro", true)
	// 超过窗口的记录不再算：只剩 a4 与新的两个
	later := start.Add(forbiddenWindow + 2*time.Minute)
	observe(later, aistudio.PoolScopeNormal, "a5", "pro", false)
	observe(later, aistudio.PoolScopeNormal, "a6", "pro", false)
	// 成功一次即清空，且成功后窗口内不判为号池级
	tracker.modelSucceeded(aistudio.PoolScopeNormal, "pro", later)
	for index := range forbiddenSystemicAccounts {
		observe(later.Add(time.Minute), aistudio.PoolScopeNormal, fmt.Sprintf("c%d", index), "pro", false)
	}
	observe(later.Add(forbiddenWindow+2*time.Minute), aistudio.PoolScopeNormal, "c9", "pro", false)
}

// TestModelForbiddenEscalatesAndResets 同一账号同一模型的 403 冷却逐次加长：第一次 30 分钟，冷却到期后再 403 为 6 小时；
// 该账号对该模型成功一次后从头计，再 403 又是 30 分钟。固定账号的请求同样写入冷却
func TestModelForbiddenEscalatesAndResets(t *testing.T) {
	upstream := &forbiddenUpstream{denied: map[string]bool{forbiddenBad: true}}
	service, pool := primaryBusyService(t, upstream, forbiddenBad, forbiddenGood)
	var rpc *aistudio.RPCError
	if _, err, _ := runAccountRequest(t, service, "req-escalate-1", forbiddenBad); !errors.As(err, &rpc) || rpc.StatusCode != http.StatusForbidden {
		t.Fatalf("固定账号的 403 应原样返回，err=%v", err)
	}
	until, _, cooling := modelCooldownUntil(t, pool, forbiddenBad, testRuntimeModel)
	if !cooling {
		t.Fatal("第一次 403 后应冷却该模型")
	}
	assertCooldownAround(t, until, forbiddenModelFirstCooldown, "第一次 403")

	expireModelCooldown(t, service, forbiddenBad, testRuntimeModel)
	if _, err, _ := runAccountRequest(t, service, "req-escalate-2", forbiddenBad); !errors.As(err, &rpc) {
		t.Fatalf("第二次请求应返回 403，err=%v", err)
	}
	until, _, cooling = modelCooldownUntil(t, pool, forbiddenBad, testRuntimeModel)
	if !cooling {
		t.Fatal("第二次 403 后应冷却该模型")
	}
	assertCooldownAround(t, until, forbiddenModelRepeatCooldown, "24 小时内第二次 403")

	// 成功一次后从头计
	expireModelCooldown(t, service, forbiddenBad, testRuntimeModel)
	upstream.setDenied(forbiddenBad, false)
	if text, err, _ := runAccountRequest(t, service, "req-escalate-ok", forbiddenBad); err != nil || text != "hello" {
		t.Fatalf("恢复权限后应成功：text=%q err=%v", text, err)
	}
	upstream.setDenied(forbiddenBad, true)
	if _, err, _ := runAccountRequest(t, service, "req-escalate-3", forbiddenBad); !errors.As(err, &rpc) {
		t.Fatalf("再次 403 应原样返回，err=%v", err)
	}
	until, _, cooling = modelCooldownUntil(t, pool, forbiddenBad, testRuntimeModel)
	if !cooling {
		t.Fatal("成功后再 403 应冷却该模型")
	}
	assertCooldownAround(t, until, forbiddenModelFirstCooldown, "成功后再 403")
}

// TestForbiddenModelTrackerLevels 单模型 403 的冷却档位：30 分钟、6 小时、到太平洋时间次日零点（至少 6 小时）；
// 同一通道本档冷却未到期时再次 403（在途请求）不升档；距上一次超过 24 小时从第一档重新计；成功后清零；不同模型、不同账号分开计
func TestForbiddenModelTrackerLevels(t *testing.T) {
	tracker := newForbiddenTracker()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	check := func(at time.Time, accountID string, modelID string, wantLevel int, wantUntil time.Time) {
		t.Helper()
		until, level := tracker.recordModel(accountID, modelID, at)
		if level != wantLevel || !until.Equal(wantUntil) {
			t.Fatalf("%s 记录 %s/%s：档位=%d 到期=%s，期望档位=%d 到期=%s", at.Format(time.RFC3339), accountID, modelID,
				level, until.Format(time.RFC3339), wantLevel, wantUntil.Format(time.RFC3339))
		}
	}
	check(start, "a", "pro", 1, start.Add(30*time.Minute))
	// 冷却期内同一通道的在途请求又 403：不升档，从现在起重新计
	check(start.Add(time.Minute), "a", "pro", 1, start.Add(31*time.Minute))
	// 不同模型、不同账号分开计
	check(start.Add(2*time.Minute), "a", "flash", 1, start.Add(32*time.Minute))
	check(start.Add(2*time.Minute), "b", "pro", 1, start.Add(32*time.Minute))
	// 冷却到期后 24 小时内再 403：6 小时
	second := start.Add(time.Hour)
	check(second, "a", "pro", 2, second.Add(6*time.Hour))
	// 第三次：到太平洋时间次日零点，不足 6 小时按 6 小时
	third := second.Add(7 * time.Hour)
	wantThird := aistudio.NextQuotaDay(third)
	if wantThird.Before(third.Add(6 * time.Hour)) {
		wantThird = third.Add(6 * time.Hour)
	}
	check(third, "a", "pro", 3, wantThird)
	late := time.Date(2026, 10, 14, 5, 0, 0, 0, time.UTC) // 太平洋时间 10 月 13 日 22 点，距次日零点 2 小时
	if next := aistudio.NextQuotaDay(late); next.Sub(late) != 2*time.Hour {
		t.Fatalf("NextQuotaDay(%s) = %s，期望 2 小时后", late, next)
	}
	if until := forbiddenModelCooldownUntil(3, late); !until.Equal(late.Add(6 * time.Hour)) {
		t.Fatalf("第三档在次日零点前 2 小时：到期 %s，期望至少 6 小时", until)
	}
	if until := forbiddenModelCooldownUntil(4, start); !until.Equal(aistudio.NextQuotaDay(start)) || start.Sub(until) > -6*time.Hour {
		t.Fatalf("第三档以后到次日零点：%s", until)
	}
	// 成功后清零
	tracker.resetModel("a", "pro")
	reset := wantThird.Add(time.Hour)
	check(reset, "a", "pro", 1, reset.Add(30*time.Minute))
	// 距上一次超过 24 小时从第一档重新计
	check(reset.Add(time.Hour), "a", "pro", 2, reset.Add(7*time.Hour))
	far := reset.Add(7*time.Hour + 25*time.Hour)
	check(far, "a", "pro", 1, far.Add(30*time.Minute))
	// 过期记录在下一次记录时清理
	tracker.mu.Lock()
	_, kept := tracker.models[forbiddenModelKey{accountID: "b", scope: "pro"}]
	tracker.mu.Unlock()
	if kept {
		t.Fatal("超过 24 小时且冷却已到期的记录应被清理")
	}
}

// TestForbiddenModelLevelsPerChannel 两个通道分别计档：Playground 与 Build 的 403 错开 10 分钟时，各自冷却到期后再 403 都升档
// （共用一份档位时，一个通道到期后的 403 总落在另一通道刚续上的到期时间之内，档位永远停在 30 分钟）；一个通道成功只清除该通道的档位
func TestForbiddenModelLevelsPerChannel(t *testing.T) {
	tracker := newForbiddenTracker()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	playground, build := "pro", aistudio.ChannelCooldownScope(aistudio.ChannelBuild, "pro")
	check := func(at time.Time, scope string, wantLevel int) {
		t.Helper()
		if _, level := tracker.recordModel("a", scope, at); level != wantLevel {
			t.Fatalf("%s 记录 %s：档位=%d，期望 %d", at.Format(time.RFC3339), scope, level, wantLevel)
		}
	}
	check(start, playground, 1)
	check(start.Add(10*time.Minute), build, 1)
	check(start.Add(30*time.Minute+5*time.Second), playground, 2)
	check(start.Add(40*time.Minute+5*time.Second), build, 2)
	tracker.resetModel("a", playground)
	check(start.Add(7*time.Hour), build, 3)
	check(start.Add(7*time.Hour), playground, 1)
}

// playgroundForbiddenUpstream 对 Playground 通道的每次生成返回 403 无权限，Build 通道正常返回；记录"账号/通道"
type playgroundForbiddenUpstream struct {
	channelRecorder
}

func (upstream *playgroundForbiddenUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events, err := upstream.channelRecorder.Generate(ctx, request)
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok && lease.Channel() == aistudio.ChannelPlayground {
		for range events {
		}
		return nil, &aistudio.RPCError{
			Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7,
			Message: "The caller does not have permission",
		}
	}
	return events, err
}

// TestModelForbiddenFallsBackToOtherChannel 单模型 403 只冷却返回 403 的通道：同一账号的另一通道可用时在那里再试一次
// （不算换号、不退避），另一通道仍能服务该模型时不登记淘汰
func TestModelForbiddenFallsBackToOtherChannel(t *testing.T) {
	upstream := &playgroundForbiddenUpstream{}
	service, pool := primaryBusyService(t, upstream, forbiddenBad)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	target := aistudio.Model{ID: streamTestModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	if err := pool.SetCatalog(forbiddenBad, aistudio.BenefitTierFree, []aistudio.Model{target}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetBuildCatalog(forbiddenBad, []aistudio.Model{target}); err != nil {
		t.Fatal(err)
	}
	text, err, elapsed := runModelRequest(t, service, "req-forbidden-channel", "", streamTestModel)
	if err != nil || text != "hello" {
		t.Fatalf("应在同一账号的 Build 通道完成：text=%q err=%v calls=%v", text, err, upstream.called())
	}
	want := []string{forbiddenBad + "/playground", forbiddenBad + "/build"}
	if calls := upstream.called(); !slices.Equal(calls, want) {
		t.Fatalf("尝试 = %v，期望 %v", calls, want)
	}
	if elapsed >= 180*time.Millisecond {
		t.Fatalf("换通道用时 %s，期望不退避", elapsed)
	}
	if _, _, cooling := modelCooldownUntil(t, pool, forbiddenBad, streamTestModel); !cooling {
		t.Fatal("Playground 通道应冷却该模型")
	}
	if _, _, cooling := modelCooldownUntil(t, pool, forbiddenBad, aistudio.ChannelCooldownScope(aistudio.ChannelBuild, streamTestModel)); cooling {
		t.Fatal("Build 通道没有返回 403，不应冷却")
	}
	if got := service.workers.retirementCount(); got != 0 {
		t.Fatalf("另一通道仍能服务该模型时不应登记淘汰，登记数 = %d", got)
	}
}

// TestModelForbiddenStrikesPerChannel 生成请求按通道记档：同一请求先后在 Playground 与 Build 上 403，两个通道各记一份第一档
// （同一请求换通道不会直接升到 6 小时）；Build 上成功只清除 Build 的档位，Playground 的档位保留
func TestModelForbiddenStrikesPerChannel(t *testing.T) {
	upstream := &forbiddenUpstream{denied: map[string]bool{forbiddenBad: true}}
	service, pool := primaryBusyService(t, upstream, forbiddenBad)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	target := aistudio.Model{ID: streamTestModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	if err := pool.SetCatalog(forbiddenBad, aistudio.BenefitTierFree, []aistudio.Model{target}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetBuildCatalog(forbiddenBad, []aistudio.Model{target}); err != nil {
		t.Fatal(err)
	}
	playground, build := streamTestModel, aistudio.ChannelCooldownScope(aistudio.ChannelBuild, streamTestModel)
	strike := func(scope string) (forbiddenModelStrike, bool) {
		service.forbidden.mu.Lock()
		defer service.forbidden.mu.Unlock()
		value, ok := service.forbidden.models[forbiddenModelKey{accountID: forbiddenBad, scope: scope}]
		return value, ok
	}
	if _, err, _ := runModelRequest(t, service, "req-strike-both", forbiddenBad, streamTestModel); !isPermissionDenied(err) {
		t.Fatalf("两个通道都 403 时应返回 403，err=%v calls=%v", err, upstream.called())
	}
	for _, scope := range []string{playground, build} {
		value, ok := strike(scope)
		if !ok || value.level != 1 {
			t.Fatalf("%s 应记第一档：%+v ok=%v", scope, value, ok)
		}
		until, _, cooling := modelCooldownUntil(t, pool, forbiddenBad, scope)
		if !cooling {
			t.Fatalf("%s 应冷却", scope)
		}
		assertCooldownAround(t, until, forbiddenModelFirstCooldown, scope)
	}
	// Build 冷却到期、恢复权限后在 Build 上成功：只清除 Build 的档位
	expireModelCooldown(t, service, forbiddenBad, build)
	upstream.setDenied(forbiddenBad, false)
	if text, err, _ := runModelRequest(t, service, "req-strike-build-ok", forbiddenBad, streamTestModel); err != nil || text != "hello" {
		t.Fatalf("Build 上应成功：text=%q err=%v", text, err)
	}
	if _, ok := strike(build); ok {
		t.Fatal("Build 成功后应清除 Build 的档位")
	}
	if _, ok := strike(playground); !ok {
		t.Fatal("Build 成功不应清除 Playground 的档位")
	}
}

// TestModelForbiddenKeepsMultiModelPause 单模型 403 冷却之外，原有的多模型暂停照常：同一账号 10 分钟内对 2 个以上模型
// 连续 3 次 403 时暂停整个账号（每个模型先各自冷却，第三次来自第三个模型）
func TestModelForbiddenKeepsMultiModelPause(t *testing.T) {
	upstream := &forbiddenUpstream{denied: map[string]bool{forbiddenBad: true}}
	service, pool := primaryBusyService(t, upstream, forbiddenBad)
	models := []string{"model-one", "model-two", "model-three"}
	catalog := make([]aistudio.Model, 0, len(models))
	for _, modelID := range models {
		catalog = append(catalog, aistudio.Model{ID: modelID, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}})
	}
	if err := pool.SetCatalog(forbiddenBad, aistudio.BenefitTierFree, catalog); err != nil {
		t.Fatal(err)
	}
	for index, modelID := range models {
		if _, err, _ := runModelRequest(t, service, fmt.Sprintf("req-pause-%d", index), forbiddenBad, modelID); err == nil {
			t.Fatalf("模型 %s 应返回 403", modelID)
		}
		if _, _, cooling := modelCooldownUntil(t, pool, forbiddenBad, modelID); !cooling {
			t.Fatalf("模型 %s 403 后应冷却", modelID)
		}
		if paused := pool.AccountCoolingDown(forbiddenBad, ""); paused != (index == len(models)-1) {
			t.Fatalf("第 %d 次 403 后账号暂停 = %v", index+1, paused)
		}
	}
	if !hasLogContaining(service.requests, "账号自动暂停", "3 个模型连续 3 次返回 403 无权限") {
		t.Fatalf("应记录账号自动暂停，日志 = %v", serviceLogMessages(service.requests))
	}
}
