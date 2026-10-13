package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

const (
	primaryTestA = "primary-a@example.com"
	primaryTestB = "primary-b@example.com"
)

// primaryBusyService 返回单账号并发为 1、Worker 都已就绪的生成服务，供排队预算测试占满账号
func primaryBusyService(t *testing.T, upstream aistudio.Service, accountIDs ...string) (*trackedService, *aistudio.AccountPool) {
	t.Helper()
	if upstream == nil {
		upstream = &channelRecorder{}
	}
	service, pool := firstEventTestService(t, upstream, 0, accountIDs...)
	pool.SetPerAccountConcurrency(1)
	return service, pool
}

// acquirePrimary 按 generateWithRetry 的方式为主用模型选号：选择条件带号池，上下文带排队预算
func acquirePrimary(ctx context.Context, service *trackedService, pool aistudio.PoolScope) (*aistudio.AccountLease, error, time.Duration) {
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent", Pool: pool}
	startedAt := time.Now()
	lease, err := service.acquireWarmLease(withLeaseWaitBudget(ctx, service.primaryBudget(selection.ModelID, selection.Pool)), selection)
	return lease, err, time.Since(startedAt)
}

// TestPrimaryQueueWaitBounded 主用模型的选号在账号全部忙碌时最多排队 PRIMARY_QUEUE_WAIT，超过即返回 CapacityBusyError
// （带排队原因、实际排队时间与 1s 到 30s 的建议重试时间）；排队上限为 0 时立即返回；退出后不留下等待者与租约；
// 不在列表里的模型照常一直排队，账号空闲后立即取得
func TestPrimaryQueueWaitBounded(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA)
	ctx, cancel := context.WithTimeout(service.lifecycle, 10*time.Second)
	defer cancel()
	release := holdLease(t, pool, primaryTestA)

	service.setPrimaryModels([]string{"models/" + strings.ToUpper(testRuntimeModel)}, 300*time.Millisecond)
	lease, err, elapsed := acquirePrimary(ctx, service, aistudio.PoolScopeNormal)
	var busy *aistudio.CapacityBusyError
	if lease != nil || !errors.As(err, &busy) {
		t.Fatalf("账号全部忙碌时 lease=%v err=%v，期望号池繁忙错误", lease, err)
	}
	if elapsed < 300*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("排队 %s 后返回，期望约 300ms（允许复查一次候选的余量）", elapsed)
	}
	if busy.ModelID != testRuntimeModel || busy.Limit != 300*time.Millisecond || busy.Waited < 250*time.Millisecond || busy.Reason == "" {
		t.Fatalf("号池繁忙错误 = %+v，期望带模型、上限 300ms、实际排队时间与原因", busy)
	}
	if retry := time.Until(busy.RetryAt()); retry < 500*time.Millisecond || retry > 1500*time.Millisecond {
		t.Fatalf("建议重试间隔 = %s，期望排队上限不足 1s 时按 1s", retry)
	}
	if !strings.Contains(err.Error(), "号池繁忙：模型 "+testRuntimeModel+" 暂时没有空闲的账号或 Worker，请稍后重试") ||
		strings.Contains(err.Error(), "Ultra") {
		t.Fatalf("错误文案 = %q", err.Error())
	}
	if busy.HTTPStatus() != http.StatusTooManyRequests || busy.ErrorCode() != "rate_limit_exceeded" {
		t.Fatalf("公开状态 = %d %s，期望 429 rate_limit_exceeded", busy.HTTPStatus(), busy.ErrorCode())
	}
	if waiters := dispatchWaiters(service); waiters != 0 {
		t.Fatalf("排队超限退出后仍有 %d 个等待者", waiters)
	}

	// 排队上限 0：不排队，立即返回，建议 5s 后重试
	service.setPrimaryModels([]string{testRuntimeModel}, 0)
	lease, err, elapsed = acquirePrimary(ctx, service, aistudio.PoolScopeNormal)
	if lease != nil || !errors.As(err, &busy) || elapsed > 500*time.Millisecond {
		t.Fatalf("排队上限为 0 时 lease=%v err=%v 用时 %s，期望立即返回号池繁忙", lease, err, elapsed)
	}
	if retry := time.Until(busy.RetryAt()); retry < 4*time.Second || retry > 6*time.Second {
		t.Fatalf("排队上限为 0 时建议重试间隔 = %s，期望约 5s", retry)
	}
	if waiters := dispatchWaiters(service); waiters != 0 {
		t.Fatalf("不排队退出后仍有 %d 个等待者", waiters)
	}

	// 不在列表里的模型照常排队：700ms 内不返回，账号空闲后立即取得
	service.setPrimaryModels([]string{"other-model"}, 0)
	type acquired struct {
		lease *aistudio.AccountLease
		err   error
	}
	result := make(chan acquired, 1)
	go func() {
		lease, err, _ := acquirePrimary(ctx, service, aistudio.PoolScopeNormal)
		result <- acquired{lease: lease, err: err}
	}()
	select {
	case got := <-result:
		t.Fatalf("不在列表里的模型不应提前返回：lease=%v err=%v", got.lease, got.err)
	case <-time.After(700 * time.Millisecond):
	}
	releasedAt := time.Now()
	release()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("账号空闲后选号失败: %v", got.err)
		}
		if waited := time.Since(releasedAt); waited >= schedulingRecheck {
			t.Fatalf("账号空闲后 %s 才取得租约，期望被立即唤醒", waited)
		}
		if err := got.lease.Release(); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("账号空闲后没有取得租约")
	}
	if waiters := dispatchWaiters(service); waiters != 0 {
		t.Fatalf("选号结束后仍有 %d 个等待者", waiters)
	}
	assertLeasesReleased(t, pool)
}

// TestPrimaryQueueWaitUltraScope /ultra 请求的排队预算只看 Ultra 号池：Ultra 账号忙碌时即使普通账号空闲也按号池繁忙返回，
// 错误写明 Ultra 号池
func TestPrimaryQueueWaitUltraScope(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA, primaryTestB)
	if err := pool.SetCatalog(primaryTestB, aistudio.BenefitTierUltra, []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(service.lifecycle, 10*time.Second)
	defer cancel()
	release := holdLease(t, pool, primaryTestB)
	defer release()
	service.setPrimaryModels([]string{testRuntimeModel}, 0)
	ultraCtx := aistudio.ContextWithPoolScope(ctx, aistudio.PoolScopeUltra)
	lease, err, _ := acquirePrimary(ultraCtx, service, aistudio.PoolScopeUltra)
	var busy *aistudio.CapacityBusyError
	if lease != nil || !errors.As(err, &busy) || busy.Pool != aistudio.PoolScopeUltra || !strings.Contains(err.Error(), "Ultra") {
		t.Fatalf("Ultra 账号忙碌时 lease=%v err=%v，期望写明 Ultra 号池的号池繁忙错误", lease, err)
	}
	// 普通路径不受 Ultra 账号忙碌影响
	lease, err, _ = acquirePrimary(ctx, service, aistudio.PoolScopeNormal)
	if err != nil || lease.Account().ID != primaryTestA {
		t.Fatalf("普通路径应取得空闲的普通账号：lease=%v err=%v", lease, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestPrimaryCoolingKeepsQueueing 候选全部冷却时沿用原规则：一分钟内恢复的冷却照常排队等待，不计入排队预算，
// 排队上限为 0 也不会按号池繁忙返回；冷却结束后取得租约
func TestPrimaryCoolingKeepsQueueing(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA)
	ctx, cancel := context.WithTimeout(service.lifecycle, 10*time.Second)
	defer cancel()
	until := time.Now().Add(400 * time.Millisecond)
	if err := pool.MarkCooldownIfGeneration(primaryTestA, testRuntimeModel, pool.ModelAccessGeneration(primaryTestA), time.Now(), until, "分钟限额"); err != nil {
		t.Fatal(err)
	}
	service.setPrimaryModels([]string{testRuntimeModel}, 0)
	lease, err, elapsed := acquirePrimary(ctx, service, aistudio.PoolScopeNormal)
	if err != nil {
		t.Fatalf("冷却结束后应取得租约，实际 err=%v", err)
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("冷却中 %s 就取得了租约，期望等到冷却结束", elapsed)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	// 一分钟内不恢复的冷却仍按全部冷却返回，不是号池繁忙
	if err := pool.MarkCooldownIfGeneration(primaryTestA, testRuntimeModel, pool.ModelAccessGeneration(primaryTestA), time.Now(), time.Now().Add(2*time.Hour), "每日限额"); err != nil {
		t.Fatal(err)
	}
	var cooling *aistudio.AllCoolingError
	if _, err, _ := acquirePrimary(ctx, service, aistudio.PoolScopeNormal); !errors.As(err, &cooling) {
		t.Fatalf("全部长时间冷却时 err=%v，期望全部冷却错误", err)
	}
}

// runPrimaryRequest 执行一次主用模型的生成请求，返回正文、错误与用时
func runPrimaryRequest(t *testing.T, service *trackedService, requestID string) (string, error, time.Duration) {
	t.Helper()
	request := aistudio.GenerateRequest{
		ID: requestID, Model: testRuntimeModel,
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

// progressLogged 判断请求进度日志里是否有包含该文字的记录
func progressLogged(service *trackedService, text string) bool {
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	for _, entry := range service.requests.logs {
		if entry.Event == "request.progress" && strings.Contains(entry.Message, text) {
			return true
		}
	}
	return false
}

// TestGeneratePrimaryBusyEndsRequest 主用模型的生成请求排队超限时立即以号池繁忙结束：不换号、不向上游发送、不冷却任何账号，
// 请求按失败结束并在进度里写明排队超过上限
func TestGeneratePrimaryBusyEndsRequest(t *testing.T) {
	recorder := &channelRecorder{}
	service, pool := primaryBusyService(t, recorder, primaryTestA, primaryTestB)
	releaseA := holdLease(t, pool, primaryTestA)
	releaseB := holdLease(t, pool, primaryTestB)
	service.setPrimaryModels([]string{testRuntimeModel}, 200*time.Millisecond)
	text, err, elapsed := runPrimaryRequest(t, service, "req-primary-busy")
	var busy *aistudio.CapacityBusyError
	if text != "" || !errors.As(err, &busy) {
		t.Fatalf("text=%q err=%v，期望号池繁忙错误", text, err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("请求 %s 才结束，期望排队 200ms 左右即返回", elapsed)
	}
	if calls := recorder.called(); len(calls) != 0 {
		t.Fatalf("排队超限不应向上游发送，实际 %v", calls)
	}
	if !progressLogged(service, "主用模型排队超过 200ms，立即返回 429") {
		t.Fatal("请求进度里应写明主用模型排队超过上限")
	}
	if count := service.requests.count(); count != 0 {
		t.Fatalf("请求结束后仍有 %d 个活动请求", count)
	}
	if waiters := dispatchWaiters(service); waiters != 0 {
		t.Fatalf("请求结束后仍有 %d 个等待者", waiters)
	}
	releaseA()
	releaseB()
	assertLeasesReleased(t, pool)
}

// quotaOnceUpstream 让指定账号返回每日限额 429，其余账号正常返回
type quotaOnceUpstream struct {
	mu     sync.Mutex
	failed string
	calls  []string
}

func (upstream *quotaOnceUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *quotaOnceUpstream) called() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

// preverifyRuntimeModelAccess 预先把 testRuntimeModel 的模型资格写成未来时间（见 preverifyModelAccess）
func preverifyRuntimeModelAccess(t *testing.T, pool *aistudio.AccountPool, accountIDs ...string) {
	t.Helper()
	future := time.Now().Add(time.Hour)
	for _, accountID := range accountIDs {
		lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
		if err != nil {
			t.Fatal(err)
		}
		generation := lease.ModelAccessGeneration()
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.MarkModelAccessVerifiedIfGeneration(accountID, testRuntimeModel, generation, future); err != nil {
			t.Fatal(err)
		}
	}
}

func (upstream *quotaOnceUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *quotaOnceUpstream) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	upstream.calls = append(upstream.calls, request.AccountID)
	if upstream.failed == request.AccountID {
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

// TestGeneratePrimaryQuotaSwitchesAccount 主用模型遇到上游额度 429 仍立即换号：排队上限为 0 也不影响换号重试，
// 失败的账号进入冷却，请求在另一个账号上完成（轮询按账号 ID 顺序先选 A）
func TestGeneratePrimaryQuotaSwitchesAccount(t *testing.T) {
	upstream := &quotaOnceUpstream{failed: primaryTestA}
	service, pool := primaryBusyService(t, upstream, primaryTestA, primaryTestB)
	// 成功账号的模型资格预先写成未来时间：请求结束后的后台写入不再改动账户状态文件，不与测试结束时删除临时目录竞争；
	// 失败账号不能预写，否则额度冷却会因检查时间早于资格记录而被忽略
	preverifyRuntimeModelAccess(t, pool, primaryTestB)
	service.setPrimaryModels([]string{testRuntimeModel}, 0)
	text, err, elapsed := runPrimaryRequest(t, service, "req-primary-quota")
	if err != nil || text != "hello" {
		t.Fatalf("应换号后完成：text=%q err=%v", text, err)
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{primaryTestA, primaryTestB}) {
		t.Fatalf("尝试顺序 = %v，期望先在 %s 上遇到额度 429 再换到 %s", calls, primaryTestA, primaryTestB)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("换号用时 %s，期望额度 429 后立即换号", elapsed)
	}
	if status, _ := pool.StatusOf(primaryTestA); status.State != aistudio.AccountCooldown {
		t.Fatalf("额度 429 的账号状态 = %s，期望 cooldown", status.State)
	}
	if progressLogged(service, "立即返回 429") {
		t.Fatal("额度 429 换号不应按排队超限结束")
	}
}
