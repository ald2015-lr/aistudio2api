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

// expireModelCooldown 模拟单模型 403 冷却到期：清除账号在该模型上的冷却，并把冷却档位记录的到期时间移到过去
// （档位按距上一次 403 的时间计，记录本身保留）
func expireModelCooldown(t *testing.T, service *trackedService, accountID string, modelID string) {
	t.Helper()
	pool := service.pool
	if err := pool.ClearCooldownIfGeneration(accountID, modelID, pool.ModelAccessGeneration(accountID), time.Now()); err != nil {
		t.Fatal(err)
	}
	tracker := service.forbidden
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	key := forbiddenModelKey{accountID: accountID, modelID: modelID}
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

// TestModelForbiddenNoHardFailureLimit 多个账号对该模型都返回 403：每个都写入冷却、不计入连续失败上限、不退避，
// 请求在第 7 个账号上完成（原先 5 次后放弃并返回 403，期间退避合计 3s）
func TestModelForbiddenNoHardFailureLimit(t *testing.T) {
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
	text, err, elapsed := runAccountRequest(t, service, "req-forbidden-many", "")
	if err != nil || text != "hello" {
		t.Fatalf("应在没有 403 的账号上完成：text=%q err=%v calls=%v", text, err, upstream.called())
	}
	if calls := upstream.called(); len(calls) != 7 || calls[6] != forbiddenGood {
		t.Fatalf("尝试 = %v，期望 6 个 403 账号各一次后到 %s", calls, forbiddenGood)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("请求用时 %s，期望单模型 403 换号不退避", elapsed)
	}
	for id := range denied {
		if _, _, cooling := modelCooldownUntil(t, pool, id, testRuntimeModel); !cooling {
			t.Fatalf("账号 %s 返回 403 后应冷却该模型", id)
		}
	}
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
// 本档冷却未到期时再次 403（另一通道或在途请求）不升档；距上一次超过 24 小时从第一档重新计；成功后清零；不同模型、不同账号分开计
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
	// 冷却期内另一通道又 403：不升档，从现在起重新计
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
	_, kept := tracker.models[forbiddenModelKey{accountID: "b", modelID: "pro"}]
	tracker.mu.Unlock()
	if kept {
		t.Fatal("超过 24 小时且冷却已到期的记录应被清理")
	}
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
