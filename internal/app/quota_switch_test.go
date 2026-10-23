package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// 普通号池额度换号规则的测试（见 quota_switch.go）：每日类、限流类各有换号上限，限流类失败后只用已启动的 Worker，
// 出口正常时第 2 次限流类换号前短暂等待，出口限流时按出口判定调整，每请求最多尝试 generateSwitchCeiling 个不同账号

// switchUpstream 让 fail 中的账号按给出的错误失败（每次调用都失败），其余账号正常返回；记录“账号/通道”
type switchUpstream struct {
	mu    sync.Mutex
	fail  map[string]func() error
	calls []string
}

func newSwitchUpstream() *switchUpstream {
	return &switchUpstream{fail: make(map[string]func() error)}
}

// failAll 让 accountIDs 中的账号都按 failure 失败
func (upstream *switchUpstream) failAll(failure func() error, accountIDs ...string) *switchUpstream {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	for _, accountID := range accountIDs {
		upstream.fail[accountID] = failure
	}
	return upstream
}

func (upstream *switchUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *switchUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *switchUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	channel := ""
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		channel = string(lease.Channel())
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID+"/"+channel)
	failure := upstream.fail[request.AccountID]
	upstream.mu.Unlock()
	if failure != nil {
		return nil, failure()
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

// called 返回调用过的“账号/通道”
func (upstream *switchUpstream) called() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

// accounts 返回调用过的账号（按调用顺序，含重复）
func (upstream *switchUpstream) accounts() []string {
	calls := upstream.called()
	accounts := make([]string, 0, len(calls))
	for _, call := range calls {
		accountID, _, _ := strings.Cut(call, "/")
		accounts = append(accounts, accountID)
	}
	return accounts
}

func dailyQuotaFailure() error { return ultraDailyQuotaError() }
func bareRateFailure() error   { return bareRateLimitError() }
func coolingDownFailure() error {
	return aistudio.ErrAccountCoolingDown
}
func serverFailure() error {
	return &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusInternalServerError, Code: 13, Message: "Internal error encountered."}
}
func forbiddenFailure() error {
	return &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission"}
}

// switchTestIDs 返回 count 个测试账号
func switchTestIDs(count int) []string {
	ids := make([]string, count)
	for index := range ids {
		ids[index] = fmt.Sprintf("switch-%02d@example.com", index)
	}
	return ids
}

// countJitter 让服务的限流类换号前等待记下调用次数、不实际等待
func countJitter(service *trackedService) *atomic.Int32 {
	var calls atomic.Int32
	service.quotaJitter = func() time.Duration {
		calls.Add(1)
		return 0
	}
	return &calls
}

// forceEgress 给服务挂上出口判定器，并让直连出口（骨架阶段全部账户都是直连）的判定为 verdict
func forceEgress(service *trackedService, verdict egressVerdict) {
	service.egress = newEgressGuard(service.pool, service.requests)
	service.egress.forceVerdictForTest("", verdict)
}

// expectQuotaError 断言 err 为额度不足（不是上游 RPCError），返回它
func expectQuotaError(t *testing.T, err error) *aistudio.PrimaryQuotaError {
	t.Helper()
	var quota *aistudio.PrimaryQuotaError
	if !errors.As(err, &quota) {
		t.Fatalf("err=%v，期望额度不足", err)
	}
	var rpcError *aistudio.RPCError
	if errors.As(err, &rpcError) {
		t.Fatal("额度不足错误不应被当成上游 RPCError（不实现 Unwrap）")
	}
	if quota.HTTPStatus() != http.StatusTooManyRequests || quota.ErrorCode() != "rate_limit_exceeded" || quota.Cause == nil {
		t.Fatalf("公开状态 = %d %s cause=%v", quota.HTTPStatus(), quota.ErrorCode(), quota.Cause)
	}
	return quota
}

// expectRetryAfter 断言建议重试间隔在 [low, high] 内
func expectRetryAfter(t *testing.T, quota *aistudio.PrimaryQuotaError, low, high time.Duration) {
	t.Helper()
	if retry := time.Until(quota.RetryAt()); retry < low || retry > high {
		t.Fatalf("建议重试间隔 = %s，期望 %s 到 %s", retry, low, high)
	}
}

// expectProgress 断言请求进度里有包含这些文字的记录
func expectProgress(t *testing.T, service *trackedService, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !progressLogged(service, fragment) {
			t.Fatalf("请求进度应写明 %q", fragment)
		}
	}
}

// TestNormalQuotaRateLimitSwitchLimit 6 个热账号都返回裸限流、非主用模型：限流类失败 3 次（换号 2 次）后立即按换号上限返回额度不足
// （429，Retry-After 约 5s，不是上游 RPCError）；限流类失败后只用已启动的 Worker
func TestNormalQuotaRateLimitSwitchLimit(t *testing.T) {
	accounts := switchTestIDs(6)
	upstream := newSwitchUpstream().failAll(bareRateFailure, accounts...)
	service, pool := primaryBusyService(t, upstream, accounts...)
	countJitter(service)
	_, err, elapsed := runModelRequest(t, service, "req-normal-rate", "", testRuntimeModel)
	quota := expectQuotaError(t, err)
	if calls := upstream.called(); len(calls) != normalRateLimitSwitchLimit+1 {
		t.Fatalf("上游调用 = %v，期望 %d 次", calls, normalRateLimitSwitchLimit+1)
	}
	if quota.Kind != aistudio.QuotaFailSwitchLimit || quota.Reason != primaryQuotaReasonRateLimit || quota.RateHits != 3 ||
		quota.QuotaHits != 0 || quota.Accounts != 3 || quota.ModelID != testRuntimeModel {
		t.Fatalf("额度不足错误 = %+v", quota)
	}
	expectRetryAfter(t, quota, 4*time.Second, 6*time.Second)
	if elapsed > time.Second {
		t.Fatalf("用时 %s，期望不退避、立即返回", elapsed)
	}
	expectProgress(t, service,
		"限流 429，换号重试（只用已启动的 Worker）| 限流失败=1/2 | 类型=限流 | 额度失败=0/4",
		"限流 429，换号重试（只用已启动的 Worker）| 限流失败=2/2",
		"换号上限，立即返回 429 | 原因=限流失败已达换号上限 | 额度失败=0 | 限流失败=3 | 尝试账号=3 | 总耗时=",
	)
	if message := err.Error(); !strings.HasPrefix(message, "额度不足：模型 "+testRuntimeModel+" 换号重试已达上限") ||
		strings.Contains(message, "主用模型") {
		t.Fatalf("错误文案 = %q", message)
	}
	if count := service.requests.count(); count != 0 {
		t.Fatalf("请求结束后仍有 %d 个活动请求", count)
	}
	assertNoBusyAccounts(t, pool)
}

// TestNormalQuotaDailySwitchLimit 6 个热账号都返回每日限额：每日类失败 5 个账号后立即返回额度不足，不退避
func TestNormalQuotaDailySwitchLimit(t *testing.T) {
	accounts := switchTestIDs(6)
	upstream := newSwitchUpstream().failAll(dailyQuotaFailure, accounts...)
	service, pool := primaryBusyService(t, upstream, accounts...)
	_, err, elapsed := runModelRequest(t, service, "req-normal-daily", "", testRuntimeModel)
	quota := expectQuotaError(t, err)
	if calls := upstream.called(); len(calls) != normalQuotaSwitchLimit+1 {
		t.Fatalf("上游调用 = %v，期望 %d 次", calls, normalQuotaSwitchLimit+1)
	}
	if quota.Kind != aistudio.QuotaFailSwitchLimit || quota.Reason != primaryQuotaReasonLimit || quota.QuotaHits != 5 || quota.RateHits != 0 {
		t.Fatalf("额度不足错误 = %+v", quota)
	}
	expectRetryAfter(t, quota, 4*time.Second, 6*time.Second)
	if elapsed > time.Second {
		t.Fatalf("用时 %s，期望每日类换号不退避", elapsed)
	}
	expectProgress(t, service, "额度 429，换号重试 | 额度失败=1/4 | 类型=每日限额", "额度 429，换号重试 | 额度失败=4/4")
	assertNoBusyAccounts(t, pool)
}

// TestNormalQuotaPrimaryModelSameLimits 普通号池的主用模型（带排队预算、不快速失败）与非主用模型的换号上限相同
func TestNormalQuotaPrimaryModelSameLimits(t *testing.T) {
	accounts := switchTestIDs(6)
	for _, test := range []struct {
		name    string
		failure func() error
		calls   int
		reason  string
	}{
		{name: "每日类", failure: dailyQuotaFailure, calls: normalQuotaSwitchLimit + 1, reason: primaryQuotaReasonLimit},
		{name: "限流类", failure: bareRateFailure, calls: normalRateLimitSwitchLimit + 1, reason: primaryQuotaReasonRateLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := newSwitchUpstream().failAll(test.failure, accounts...)
			service, _ := primaryBusyService(t, upstream, accounts...)
			countJitter(service)
			service.setPrimaryModelSettings([]string{testRuntimeModel}, 10*time.Second, nil, 0)
			_, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeNormal, "req-normal-primary", "")
			quota := expectQuotaError(t, err)
			if calls := upstream.called(); len(calls) != test.calls {
				t.Fatalf("上游调用 = %v，期望 %d 次", calls, test.calls)
			}
			if quota.Kind != aistudio.QuotaFailSwitchLimit || quota.Reason != test.reason || quota.Pool != aistudio.PoolScopeNormal {
				t.Fatalf("额度不足错误 = %+v", quota)
			}
			if !strings.HasPrefix(err.Error(), "额度不足：普通号池的模型 ") {
				t.Fatalf("错误文案 = %q", err.Error())
			}
			expectRetryAfter(t, quota, 4*time.Second, 6*time.Second)
			if elapsed > time.Second {
				t.Fatalf("用时 %s，期望立即返回", elapsed)
			}
		})
	}
}

// TestNormalQuotaJitterBeforeSecondRateSwitch 出口正常时只在第 2 次限流类换号前等待一次；主用模型的排队预算已过时不等待；
// 出口限速生效（令牌桶已错开重试）时不等待
func TestNormalQuotaJitterBeforeSecondRateSwitch(t *testing.T) {
	accounts := switchTestIDs(6)
	for _, test := range []struct {
		name    string
		prepare func(service *trackedService)
		want    int32
	}{
		{name: "出口正常", want: 1},
		{name: "排队预算已过", prepare: func(service *trackedService) {
			service.setPrimaryModelSettings([]string{testRuntimeModel}, 0, nil, 0)
		}},
		{name: "出口限速生效", prepare: func(service *trackedService) {
			forceEgress(service, egressVerdict{
				Mode: egressLimited, Limited: true, Throttled: true, Limit: 24, Label: "直连", RetryAt: time.Now().Add(7 * time.Second),
			})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := newSwitchUpstream().failAll(bareRateFailure, accounts...)
			service, _ := primaryBusyService(t, upstream, accounts...)
			jitter := countJitter(service)
			if test.prepare != nil {
				test.prepare(service)
			}
			_, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeNormal, "req-normal-jitter", "")
			expectQuotaError(t, err)
			if calls := upstream.called(); len(calls) != normalRateLimitSwitchLimit+1 {
				t.Fatalf("上游调用 = %v，期望 %d 次", calls, normalRateLimitSwitchLimit+1)
			}
			if got := jitter.Load(); got != test.want {
				t.Fatalf("换号前等待 %d 次，期望 %d 次", got, test.want)
			}
		})
	}
	t.Run("默认等待 300–800ms", func(t *testing.T) {
		service := &trackedService{}
		for range 50 {
			if delay := service.quotaJitterDelay(); delay < quotaJitterMin || delay >= quotaJitterMin+quotaJitterSpread {
				t.Fatalf("默认等待 = %s", delay)
			}
		}
		budget := &leaseWaitBudget{limit: 50 * time.Millisecond, startedAt: time.Now()}
		startedAt := time.Now()
		if err := waitQuotaJitter(context.Background(), time.Second, budget); err != nil || time.Since(startedAt) > 500*time.Millisecond {
			t.Fatalf("等待应截到排队预算剩余：err=%v 用时 %s", err, time.Since(startedAt))
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := waitQuotaJitter(ctx, time.Second, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("请求结束时应返回它的错误: %v", err)
		}
	})
}

// warmOnlyColdStartService 返回浏览器后端的生成服务：热账号已启动 Worker，备用账号没有；launches 记下冷启动次数
func warmOnlyColdStartService(t *testing.T, upstream aistudio.Service, warm string, standby string) (*trackedService, *atomic.Int32) {
	t.Helper()
	service := coldStartTestService(t, 10*time.Millisecond, nil, warm, standby)
	service.service = upstream
	worker, _ := aistudio.NewStubWorker(warm, 0)
	service.workers.accounts[warm].worker = worker
	service.workers.accounts[warm].warm.Store(true)
	var launches atomic.Int32
	launch := service.workers.launch
	service.workers.launch = func(ctx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		launches.Add(1)
		return launch(ctx, accountID, options)
	}
	return service, &launches
}

// TestNormalQuotaWarmOnlyAfterRateLimit 热账号 A 裸限流、备用账号 B 健康：限流类失败后的选号只用已启动的 Worker，
// 不现场冷启动 B（只为它发起一次后台扩容），以“额度 429 后没有已启动的 Worker”结束，Retry-After 约 5s
func TestNormalQuotaWarmOnlyAfterRateLimit(t *testing.T) {
	const warm, standby = "warm-a@example.com", "standby-b@example.com"
	upstream := newSwitchUpstream().failAll(bareRateFailure, warm)
	service, launches := warmOnlyColdStartService(t, upstream, warm, standby)
	_, err, elapsed := runModelRequest(t, service, "req-normal-warm-only", "", testRuntimeModel)
	quota := expectQuotaError(t, err)
	if quota.Kind != aistudio.QuotaFailSwitchLimit || quota.Reason != primaryQuotaReasonNoWarm || quota.RateHits != 1 {
		t.Fatalf("额度不足错误 = %+v", quota)
	}
	expectRetryAfter(t, quota, 4*time.Second, 6*time.Second)
	if accounts := upstream.accounts(); !slices.Equal(accounts, []string{warm}) {
		t.Fatalf("上游调用 = %v，期望只有热账号", accounts)
	}
	if elapsed > time.Second {
		t.Fatalf("用时 %s，期望不等冷启动、立即返回", elapsed)
	}
	// 后台扩容不等它：请求结束后只有它这一次冷启动
	deadline := time.Now().Add(3 * time.Second)
	for launches.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := launches.Load(); got != 1 {
		t.Fatalf("冷启动 %d 次，期望只有一次后台扩容", got)
	}
	expectProgress(t, service, "换号上限，立即返回 429 | 原因="+primaryQuotaReasonNoWarm)
}

// TestNormalQuotaDailyAllowsColdStart 热账号 A 每日限额、备用账号 B 健康：每日类失败后照常可以现场冷启动 B 完成请求
func TestNormalQuotaDailyAllowsColdStart(t *testing.T) {
	const warm, standby = "warm-a@example.com", "standby-b@example.com"
	upstream := newSwitchUpstream().failAll(dailyQuotaFailure, warm)
	service, launches := warmOnlyColdStartService(t, upstream, warm, standby)
	text, err, _ := runModelRequest(t, service, "req-normal-daily-cold", "", testRuntimeModel)
	if err != nil || text != "hello" {
		t.Fatalf("应现场冷启动备用账号完成：text=%q err=%v", text, err)
	}
	if accounts := upstream.accounts(); !slices.Equal(accounts, []string{warm, standby}) {
		t.Fatalf("上游调用 = %v", accounts)
	}
	if got := launches.Load(); got != 1 {
		t.Fatalf("冷启动 %d 次，期望 1 次", got)
	}
	expectProgress(t, service, "额度 429，换号重试 | 额度失败=1/4 | 类型=每日限额")
}

// TestNormalQuotaCoolingDownNotCounted 租约账号都在发出前已冷却（请求没有发到上游）：不计入额度上限、不退避，
// 只计入每请求硬上限，试满 generateSwitchCeiling 个账号后按原错误结束
func TestNormalQuotaCoolingDownNotCounted(t *testing.T) {
	accounts := switchTestIDs(10)
	upstream := newSwitchUpstream().failAll(coolingDownFailure, accounts...)
	service, pool := primaryBusyService(t, upstream, accounts...)
	_, err, elapsed := runModelRequest(t, service, "req-normal-cooling-down", "", testRuntimeModel)
	var quota *aistudio.PrimaryQuotaError
	if errors.As(err, &quota) || !errors.Is(err, aistudio.ErrAccountCoolingDown) {
		t.Fatalf("err=%v，期望按发出前已冷却结束、不按额度不足", err)
	}
	if calls := upstream.accounts(); len(calls) != generateSwitchCeiling || len(sortedUnique(calls)) != generateSwitchCeiling {
		t.Fatalf("上游调用 = %v，期望 %d 个不同账号", calls, generateSwitchCeiling)
	}
	if elapsed > time.Second {
		t.Fatalf("用时 %s，期望不退避", elapsed)
	}
	expectProgress(t, service, fmt.Sprintf("已尝试 %d 个账号，不再换号 | 原因=", generateSwitchCeiling))
	if progressLogged(service, "额度失败=") || progressLogged(service, "限流失败=") {
		t.Fatal("发出前已冷却不应计入额度失败")
	}
	assertNoBusyAccounts(t, pool)
}

// TestGenerateSwitchCeiling 20 个账号混合 5xx、单模型 403 与发出前已冷却（硬失败不到连续失败上限）：最多在 8 个不同账号上尝试
func TestGenerateSwitchCeiling(t *testing.T) {
	accounts := switchTestIDs(20)
	upstream := newSwitchUpstream().failAll(coolingDownFailure, accounts[2:]...)
	upstream.failAll(serverFailure, accounts[0])
	upstream.failAll(forbiddenFailure, accounts[1])
	service, pool := primaryBusyService(t, upstream, accounts...)
	_, err, _ := runModelRequest(t, service, "req-switch-ceiling", "", testRuntimeModel)
	if err == nil {
		t.Fatal("期望失败")
	}
	if calls := upstream.accounts(); len(sortedUnique(calls)) != generateSwitchCeiling || len(calls) != generateSwitchCeiling {
		t.Fatalf("上游调用 = %v，期望 %d 个不同账号各一次", calls, generateSwitchCeiling)
	}
	expectProgress(t, service, fmt.Sprintf("已尝试 %d 个账号，不再换号", generateSwitchCeiling))
	if count := service.requests.count(); count != 0 {
		t.Fatalf("请求结束后仍有 %d 个活动请求", count)
	}
	assertNoBusyAccounts(t, pool)
}

// TestGenerateSwitchCeilingQuotaFailure 最后一次是额度失败时，硬上限按“换号次数已达上限”返回额度不足（Retry-After 约 5s）
func TestGenerateSwitchCeilingQuotaFailure(t *testing.T) {
	accounts := switchTestIDs(12)
	upstream := newSwitchUpstream().failAll(coolingDownFailure, accounts...)
	service, _ := primaryBusyService(t, upstream, accounts...)
	// 发出前已冷却的账号试过 7 个后，第 8 个返回每日限额
	var cooled atomic.Int32
	upstream.mu.Lock()
	for _, accountID := range accounts {
		upstream.fail[accountID] = func() error {
			if cooled.Add(1) >= generateSwitchCeiling {
				return dailyQuotaFailure()
			}
			return coolingDownFailure()
		}
	}
	upstream.mu.Unlock()
	_, err, _ := runModelRequest(t, service, "req-switch-ceiling-quota", "", testRuntimeModel)
	quota := expectQuotaError(t, err)
	if quota.Kind != aistudio.QuotaFailSwitchLimit || quota.Reason != primaryQuotaReasonCeiling || quota.Accounts != generateSwitchCeiling ||
		quota.QuotaHits != 1 {
		t.Fatalf("额度不足错误 = %+v", quota)
	}
	expectRetryAfter(t, quota, 4*time.Second, 6*time.Second)
	expectProgress(t, service, "换号上限，立即返回 429 | 原因="+primaryQuotaReasonCeiling)
}

// independentChannelService 返回 Playground 与 Build 都启用、两个通道判定为独立额度的生成服务（裸限流只冷却本通道，
// 同账号另一通道还能再试）
func independentChannelService(t *testing.T, upstream aistudio.Service, accountIDs ...string) (*trackedService, *aistudio.AccountPool) {
	t.Helper()
	service, pool := rateLadderService(t, upstream, accountIDs...)
	service.quota.mu.Lock()
	service.quota.models[quotaStatsKey(rateLadderModel, aistudio.PoolScopeNormal)] = &quotaSharingStats{Independent: 1}
	service.quota.mu.Unlock()
	return service, pool
}

// TestNormalQuotaEgressLimitedSwitchesViaQueue 出口限流且限速生效：限流类换 2 次（每次经出口限速排队）后按上游出口限流结束，
// Retry-After 为判定给出的时间；不做同账号换通道（对照：出口正常时同账号另一通道先再试一次），不抖动
func TestNormalQuotaEgressLimitedSwitchesViaQueue(t *testing.T) {
	accounts := switchTestIDs(4)
	t.Run("出口限流", func(t *testing.T) {
		upstream := newSwitchUpstream().failAll(bareRateFailure, accounts...)
		service, pool := independentChannelService(t, upstream, accounts...)
		jitter := countJitter(service)
		retryAt := time.Now().Add(7 * time.Second)
		forceEgress(service, egressVerdict{Mode: egressLimited, Limited: true, Throttled: true, Limit: 24, Label: "直连", RetryAt: retryAt})
		_, err, _ := runModelRequest(t, service, "req-egress-limited", "", rateLadderModel)
		quota := expectQuotaError(t, err)
		calls := upstream.accounts()
		if len(calls) != normalRateLimitSwitchLimit+1 || len(sortedUnique(calls)) != len(calls) {
			t.Fatalf("上游调用 = %v，期望 %d 个不同账号各一次（不做同账号换通道）", upstream.called(), normalRateLimitSwitchLimit+1)
		}
		if quota.Kind != aistudio.QuotaFailEgress || quota.Reason != primaryQuotaReasonEgress || quota.RateHits != 3 ||
			quota.Egress != "直连" || quota.EgressLimit != 24 || quota.Accounts != 3 {
			t.Fatalf("额度不足错误 = %+v", quota)
		}
		if !quota.Until.Equal(retryAt) {
			t.Fatalf("建议重试时间 = %s，期望判定给出的 %s", quota.Until, retryAt)
		}
		if got := jitter.Load(); got != 0 {
			t.Fatalf("限速生效时不应抖动，实际等待 %d 次", got)
		}
		if message := err.Error(); !strings.HasPrefix(message, "上游限流：模型 "+rateLadderModel+" 所在出口 直连 正在限流") ||
			!strings.Contains(message, "限速 24 次/分钟") || strings.Contains(message, "主用模型") {
			t.Fatalf("错误文案 = %q", message)
		}
		expectProgress(t, service,
			"限流 429，经出口限速换号重试 | 限流失败=1/2 | 出口=直连 | 限速=24/分钟 | 原因=",
			"限流 429，经出口限速换号重试 | 限流失败=2/2",
			"上游出口限流，换号已达上限，立即返回 429 | 出口=直连 | 限速=24/分钟 | 限流失败=3 | 尝试账号=3 | 总耗时=",
		)
		assertNoBusyAccounts(t, pool)
	})
	t.Run("出口正常时同账号换通道", func(t *testing.T) {
		upstream := newSwitchUpstream().failAll(bareRateFailure, accounts...)
		service, _ := independentChannelService(t, upstream, accounts...)
		countJitter(service)
		_, err, _ := runModelRequest(t, service, "req-egress-normal", "", rateLadderModel)
		quota := expectQuotaError(t, err)
		calls := upstream.called()
		if len(calls) != 3 {
			t.Fatalf("上游调用 = %v，期望 3 次", calls)
		}
		first, _, _ := strings.Cut(calls[0], "/")
		second, _, _ := strings.Cut(calls[1], "/")
		if first != second || calls[0] == calls[1] {
			t.Fatalf("上游调用 = %v，期望先在同账号另一通道再试", calls)
		}
		if quota.Kind != aistudio.QuotaFailSwitchLimit || quota.Reason != primaryQuotaReasonRateLimit {
			t.Fatalf("额度不足错误 = %+v", quota)
		}
		expectProgress(t, service, "限流 429，同账号换通道重试 | 限流失败=1/2")
	})
}

// TestNormalQuotaEgressSuspendedSwitchLimit 出口限流期间限速已判为无效（停止限速）：限流类上限放宽到 egressSuspendedRateSwitches，
// 上游调用 5 次，结束时按上游出口限流（限速已停止）返回；出口正常的规则下第 2 次起每次限流类换号前都抖动
func TestNormalQuotaEgressSuspendedSwitchLimit(t *testing.T) {
	accounts := switchTestIDs(6)
	upstream := newSwitchUpstream().failAll(bareRateFailure, accounts...)
	service, pool := primaryBusyService(t, upstream, accounts...)
	jitter := countJitter(service)
	forceEgress(service, egressVerdict{Mode: egressLimited, Limited: true, Suspended: true, Label: "直连", RetryAt: time.Now().Add(5 * time.Second)})
	_, err, _ := runModelRequest(t, service, "req-egress-suspended", "", testRuntimeModel)
	quota := expectQuotaError(t, err)
	if calls := upstream.called(); len(calls) != egressSuspendedRateSwitches+1 {
		t.Fatalf("上游调用 = %v，期望 %d 次", calls, egressSuspendedRateSwitches+1)
	}
	if quota.Kind != aistudio.QuotaFailEgress || quota.RateHits != egressSuspendedRateSwitches+1 || quota.EgressLimit != 0 {
		t.Fatalf("额度不足错误 = %+v", quota)
	}
	if got := jitter.Load(); got != egressSuspendedRateSwitches-1 {
		t.Fatalf("换号前等待 %d 次，期望 %d 次", got, egressSuspendedRateSwitches-1)
	}
	if !strings.Contains(err.Error(), "限速已停止") {
		t.Fatalf("错误文案 = %q", err.Error())
	}
	expectProgress(t, service,
		"限流 429，换号重试（只用已启动的 Worker）| 限流失败=1/4",
		"上游出口限流，换号已达上限，立即返回 429 | 出口=直连 | 限速=已停止 | 限流失败=5",
	)
	assertNoBusyAccounts(t, pool)
}

// TestFileBoundOwnerColdStartPausedCopies 文件绑定请求的所属账号不可用时改为复制文件到其他账号：没有符合条件的账号、全部冷却，
// 以及所属账号是同出口的备用账号、出口限流期间冷启动被闸门暂停（errEgressColdStartPaused）；其余容量原因照常在选号里等待
func TestFileBoundOwnerColdStartPausedCopies(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "冷启动被闸门暂停", err: errEgressColdStartPaused, want: true},
		{name: "包装后的冷启动暂停", err: fmt.Errorf("提升备用账号: %w", errEgressColdStartPaused), want: true},
		{name: "与其他错误合并的冷启动暂停", err: errors.Join(errors.New("释放租约失败"), errEgressColdStartPaused), want: true},
		{name: "没有符合条件的账号", err: fmt.Errorf("%w: 文件所属账号已停用", aistudio.ErrNoEligibleAccount), want: true},
		{name: "全部冷却", err: &aistudio.AllCoolingError{ModelID: testRuntimeModel, Until: time.Now().Add(time.Hour)}, want: true},
		{name: "一般的 Worker 槽位已满", err: errAccountWorkerCapacity},
		{name: "号池繁忙", err: &aistudio.CapacityBusyError{ModelID: testRuntimeModel}},
		{name: "其他错误", err: errors.New("boom")},
	} {
		if got := fileOwnerUnavailable(test.err); got != test.want {
			t.Fatalf("%s：fileOwnerUnavailable = %v，期望 %v", test.name, got, test.want)
		}
	}
	if !errors.Is(errEgressColdStartPaused, errAccountWorkerCapacity) {
		t.Fatal("冷启动暂停在其他调用方仍应按容量处理")
	}
}

// TestFileBoundOwnerColdStartPausedCopiesRequest 走完整请求：文件所属账号 B 是同出口的备用账号，出口限流期间它的冷启动被闸门暂停
// （选号返回 errEgressColdStartPaused，见 acquireLease），请求改为把文件复制到已启动的 A 上完成，不冷启动 B、不等冷启动配额
func TestFileBoundOwnerColdStartPausedCopiesRequest(t *testing.T) {
	const resourceID = "files/egress-paused-owner"
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com"}, 1, 2, 0)
	upstream := &ultraPrimaryUpstream{}
	env.service.service = upstream
	if err := env.pool.BindResource(resourceID, "b@example.com"); err != nil {
		t.Fatal(err)
	}
	forceEgressLimited(env.service, 7*time.Second)
	text, err, elapsed := runPrimaryRequestWith(t, env.service, env.service.lifecycle, "req-file-owner-paused", resourceID)
	if err != nil || text != "hello" || elapsed > 3*time.Second {
		t.Fatalf("text=%q err=%v 用时 %s，期望复制文件到已启动的 A 后立即完成", text, err, elapsed)
	}
	if got := upstream.called(); !slices.Equal(got, []string{"a@example.com"}) {
		t.Fatalf("上游调用 = %v，期望只在 A 上发送", got)
	}
	if copies := upstream.copied(); !slices.Equal(copies, []string{"a@example.com"}) {
		t.Fatalf("文件复制目标 = %v，期望复制到 A", copies)
	}
	if launched := env.launches(); len(launched) != 0 {
		t.Fatalf("出口限流期间为文件所属的备用账号冷启动了 %v", launched)
	}
	assertNoBusyAccounts(t, env.pool)
}

// TestEgressOtherFailureExclusions 交给出口判定器的“其他失败”只算已发往上游、与出口有关的失败：输入超限、参数错误、上游 400、
// 内容拦截、请求取消、发出前已冷却、裸限流与文件复制或附件上传失败都不算；5xx、首事件超时、403、带指标的限额算
func TestEgressOtherFailureExclusions(t *testing.T) {
	live := context.Background()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	inputLimit := &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
		Message: "The input token count (2000000) exceeds the maximum number of tokens allowed (1048576).",
	}
	badRequest := &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: "Request contains an invalid argument."}
	for _, test := range []struct {
		name        string
		ctx         context.Context
		generated   bool
		coolingDown bool
		bare        bool
		err         error
		want        bool
	}{
		{name: "5xx", ctx: live, generated: true, err: serverFailure(), want: true},
		{name: "首事件超时", ctx: live, generated: true, err: &firstEventTimeoutError{timeout: time.Second}, want: true},
		{name: "403", ctx: live, generated: true, err: forbiddenFailure(), want: true},
		{name: "带指标的每日限额", ctx: live, generated: true, err: dailyQuotaFailure(), want: true},
		{name: "输入超限", ctx: live, generated: true, err: inputLimit},
		{name: "参数错误", ctx: live, generated: true, err: fmt.Errorf("%w: 模型不能为空", aistudio.ErrInvalidArgument)},
		{name: "上游 400", ctx: live, generated: true, err: badRequest},
		{name: "内容拦截", ctx: live, generated: true, err: &aistudio.PromptFeedbackError{Reason: "SAFETY"}},
		{name: "取消", ctx: live, generated: true, err: context.Canceled},
		{name: "客户端已取消", ctx: canceled, generated: true, err: serverFailure()},
		{name: "本地 Worker 替换", ctx: live, generated: true, err: errAccountWorkerReplaced},
		{name: "发出前已冷却", ctx: live, generated: true, coolingDown: true, err: aistudio.ErrAccountCoolingDown},
		{name: "裸限流", ctx: live, generated: true, bare: true, err: bareRateFailure()},
		{name: "文件复制或附件上传失败", ctx: live, err: serverFailure()},
	} {
		if got := egressOtherOutcome(test.ctx, test.generated, test.coolingDown, test.bare, test.err); got != test.want {
			t.Fatalf("%s：egressOtherOutcome = %v，期望 %v", test.name, got, test.want)
		}
	}
}

// TestNormalQuotaPinnedKeepsRPCError 指定账号的请求只能用这个账号：额度 429 不计数，按上游原错误结束
func TestNormalQuotaPinnedKeepsRPCError(t *testing.T) {
	accounts := switchTestIDs(3)
	for _, failure := range []func() error{bareRateFailure, dailyQuotaFailure} {
		upstream := newSwitchUpstream().failAll(failure, accounts...)
		service, _ := primaryBusyService(t, upstream, accounts...)
		_, err, _ := runModelRequest(t, service, "req-pinned-quota", accounts[0], testRuntimeModel)
		var quota *aistudio.PrimaryQuotaError
		var rpcError *aistudio.RPCError
		if errors.As(err, &quota) || !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("err=%v，期望按上游 429 原错误结束", err)
		}
		if calls := upstream.accounts(); !slices.Equal(calls, []string{accounts[0]}) {
			t.Fatalf("上游调用 = %v，期望只用指定账号", calls)
		}
	}
	if policy := (&trackedService{}).quotaPolicyFor(nil, false); policy.counted || policy.ultra {
		t.Fatalf("指定账号的请求不应计数：%+v", policy)
	}
}

// TestNormalQuotaClassOf 普通策略的额度失败分类：裸限流不论写入后的冷却多长都是限流类；每日限额、剩余很长的 RetryInfo 为每日类；
// 分钟限额为限流类。Ultra 主用模型沿用原分类（裸限流升到 30 分钟按每日类）
func TestNormalQuotaClassOf(t *testing.T) {
	now := time.Now()
	normal := (&trackedService{}).quotaPolicyFor(nil, true)
	ultra := (&trackedService{}).quotaPolicyFor(&leaseWaitBudget{quotaFailFast: true}, true)
	if !normal.counted || normal.ultra || normal.dailyLimit != normalQuotaSwitchLimit || normal.rateLimit != normalRateLimitSwitchLimit ||
		normal.warmAfterDaily || !normal.warmAfterRate || !normal.jitter {
		t.Fatalf("普通策略 = %+v", normal)
	}
	if !ultra.counted || !ultra.ultra || ultra.dailyLimit != primaryQuotaSwitchLimit || ultra.rateLimit != primaryRateLimitSwitchLimit ||
		!ultra.warmAfterDaily || !ultra.warmAfterRate || ultra.jitter {
		t.Fatalf("Ultra 主用模型策略 = %+v", ultra)
	}
	for _, test := range []struct {
		name     string
		cooldown aistudio.QuotaCooldown
		normal   primaryQuotaClass
		ultra    primaryQuotaClass
	}{
		{name: "裸限流第 3 档", cooldown: aistudio.QuotaCooldown{Kind: aistudio.RateLimitKind, Escalate: true, Until: now.Add(30 * time.Minute)},
			normal: primaryQuotaRate, ultra: primaryQuotaDaily},
		{name: "裸限流第 1 档", cooldown: aistudio.QuotaCooldown{Kind: aistudio.RateLimitKind, Escalate: true, Until: now.Add(30 * time.Second)},
			normal: primaryQuotaRate, ultra: primaryQuotaRate},
		{name: "每日限额", cooldown: aistudio.QuotaCooldown{Kind: aistudio.DailyQuotaKind, Until: now.Add(time.Hour)},
			normal: primaryQuotaDaily, ultra: primaryQuotaDaily},
		{name: "分钟限额", cooldown: aistudio.QuotaCooldown{Kind: aistudio.MinuteQuotaKind, Until: now.Add(40 * time.Second)},
			normal: primaryQuotaRate, ultra: primaryQuotaRate},
		{name: "RetryInfo 20 分钟的非裸限流", cooldown: aistudio.QuotaCooldown{Kind: aistudio.RateLimitKind, Until: now.Add(20 * time.Minute)},
			normal: primaryQuotaDaily, ultra: primaryQuotaDaily},
	} {
		if got := normal.classOf(test.cooldown, now); got != test.normal {
			t.Fatalf("%s：普通策略类别 = %d，期望 %d", test.name, got, test.normal)
		}
		if got := ultra.classOf(test.cooldown, now); got != test.ultra {
			t.Fatalf("%s：Ultra 主用模型类别 = %d，期望 %d", test.name, got, test.ultra)
		}
	}
	// 限流类上限按出口判定调整：限速无效时放宽，Ultra 主用模型不调整
	suspended := egressVerdict{Mode: egressLimited, Limited: true, Suspended: true}
	throttled := egressVerdict{Mode: egressLimited, Limited: true, Throttled: true, Limit: 24}
	if normal.rateLimitFor(egressVerdict{}) != normalRateLimitSwitchLimit || normal.rateLimitFor(throttled) != normalRateLimitSwitchLimit ||
		normal.rateLimitFor(suspended) != egressSuspendedRateSwitches || ultra.rateLimitFor(suspended) != primaryRateLimitSwitchLimit {
		t.Fatal("限流类上限没有按出口判定调整")
	}
}

// TestNormalQuotaRetryAt 普通号池额度不足的建议重试时间：限流类上限、没有已启动的 Worker、硬上限为 5s；每日类上限与没有其他账号
// 取 5s 与本请求遇到的冷却中较早者；出口限流取判定给出的时间；全部冷却同 Ultra；都不早于 1s
func TestNormalQuotaRetryAt(t *testing.T) {
	now := time.Now()
	kind := aistudio.QuotaFailSwitchLimit
	for _, test := range []struct {
		name    string
		kind    aistudio.QuotaFailKind
		reason  string
		quota   time.Time
		cooling *aistudio.AllCoolingError
		want    time.Time
	}{
		{name: "限流类上限不取限流冷却", kind: kind, reason: primaryQuotaReasonRateLimit, quota: now.Add(40 * time.Second), want: now.Add(primaryRetryFallback)},
		{name: "没有已启动的 Worker", kind: kind, reason: primaryQuotaReasonNoWarm, want: now.Add(primaryRetryFallback)},
		{name: "硬上限", kind: kind, reason: primaryQuotaReasonCeiling, quota: now.Add(2 * time.Second), want: now.Add(primaryRetryFallback)},
		{name: "每日类上限", kind: kind, reason: primaryQuotaReasonLimit, quota: now.Add(3 * time.Hour), want: now.Add(primaryRetryFallback)},
		{name: "没有其他账号时本请求的冷却更早", kind: kind, reason: primaryQuotaReasonNoOther, quota: now.Add(2 * time.Second), want: now.Add(2 * time.Second)},
		{name: "全部冷却", kind: kind, reason: primaryQuotaReasonCooling, cooling: &aistudio.AllCoolingError{Until: now.Add(time.Hour)}, quota: now.Add(2 * time.Hour), want: now.Add(time.Hour)},
		{name: "出口限流", kind: aistudio.QuotaFailEgress, reason: primaryQuotaReasonEgress, quota: now.Add(17 * time.Second), want: now.Add(17 * time.Second)},
		{name: "出口限流没有时间时按 5s", kind: aistudio.QuotaFailEgress, reason: primaryQuotaReasonEgress, want: now.Add(primaryRetryFallback)},
		{name: "出口限流不早于 1s", kind: aistudio.QuotaFailEgress, reason: primaryQuotaReasonEgress, quota: now.Add(100 * time.Millisecond), want: now.Add(primaryRetryMin)},
		{name: "Ultra 主用模型在出口限流时同样取判定的时间", kind: aistudio.QuotaFailUltraPrimary, reason: primaryQuotaReasonEgress, quota: now.Add(9 * time.Second), want: now.Add(9 * time.Second)},
	} {
		if got := primaryQuotaRetryAt(test.kind, test.reason, test.quota, test.cooling, now); !got.Equal(test.want) {
			t.Fatalf("%s：建议重试 = %s，期望 %s", test.name, got.Sub(now), test.want.Sub(now))
		}
	}
}

// TestNormalQuotaConcurrentBounded 20 个并发请求、10 个账号都裸限流：每个请求最多 3 次上游发送，结束后没有活动请求、等待者与租约
func TestNormalQuotaConcurrentBounded(t *testing.T) {
	accounts := switchTestIDs(10)
	upstream := newSwitchUpstream().failAll(bareRateFailure, accounts...)
	service, pool := rateLadderService(t, upstream, accounts...)
	service.quotaJitter = func() time.Duration { return time.Millisecond }
	const requests = 20
	var wait sync.WaitGroup
	failures := make(chan error, requests)
	for index := range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err, _ := runModelRequest(t, service, fmt.Sprintf("req-concurrent-%d", index), "", rateLadderModel)
			var quota *aistudio.PrimaryQuotaError
			if !errors.As(err, &quota) {
				failures <- err
			}
		}()
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("并发请求应按额度不足（429）结束，实际 err=%v", err)
	}
	if got := len(upstream.called()); got > requests*(normalRateLimitSwitchLimit+1) {
		t.Fatalf("上游调用 %d 次，超过每个请求 %d 次的上限", got, normalRateLimitSwitchLimit+1)
	}
	if count := service.requests.count(); count != 0 {
		t.Fatalf("请求结束后仍有 %d 个活动请求", count)
	}
	if waiters := dispatchWaiters(service); waiters != 0 {
		t.Fatalf("请求结束后仍有 %d 个等待者", waiters)
	}
	assertNoBusyAccounts(t, pool)
}
