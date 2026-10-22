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

// paidUsageLimitPayload 为 pro/ultra 号每日限额（gemini-3.1-pro 每天 50 次）的现场 429 原文（流式封装 [,[8, message, details]]）：
// 文案是 “You've reached your usage limit”，没有恢复时间，详情只有 GeminiApiErrorDetails（下标 28 为 QuotaFailure 形状）
const paidUsageLimitPayload = `[,[8,"You've reached your usage limit. Retry with an alternative Gemini model or upgrade your Google AI plan for higher limits. To view plans head to https://one.google.com/ai.",` +
	`[["type.googleapis.com/google.internal.alkali.applications.makersuite.v1.GeminiApiErrorDetails",[` +
	`null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
	`[[[null,null,null,"generativelanguage.googleapis.com/generate_requests_per_model_per_user","GenerateRequestsPerProjectPerUserPerModelPerDay",` +
	`[["model","gemini-3.1-pro"],["location","global"]],"50"]]]]]]]]`

// usageLimitLogFields 为每日限额冷却日志里上游给出的上限、指标与额度计数的模型
const usageLimitLogFields = "| 上限=50 | 指标=generate_requests_per_model_per_user | 模型=gemini-3.1-pro"

// paidUsageLimitError 按 Playground 或 Build 的方法名解码 paidUsageLimitPayload
func paidUsageLimitError(method string) *aistudio.RPCError {
	return aistudio.DecodeRPCError(method, http.StatusTooManyRequests, []byte(paidUsageLimitPayload))
}

// markQuotaShared 让模型在普通权益分组内已判定为两个通道共用每日额度
func markQuotaShared(service *trackedService, modelID string) {
	service.quota.mu.Lock()
	defer service.quota.mu.Unlock()
	service.quota.models[modelID] = &quotaSharingStats{Shared: quotaSharedMinEvidence}
}

// rateLimitFailures 返回号池级限流判定里该模型记下的裸限流账号数
func rateLimitFailures(service *trackedService, modelID string) int {
	service.rateLimits.mu.Lock()
	defer service.rateLimits.mu.Unlock()
	return len(service.rateLimits.scopes[rateLimitScopeKey{pool: aistudio.PoolScopeNormal, modelID: modelID}].failed)
}

// TestUsageLimitAppliesDailyQuota pro/ultra 每日限额（C）经 applyQuotaCooldown 按每日限额冷却，不走裸限流的逐级冷却
// （号池级限流判定没有记录、日志没有档位），冷却日志带上限、指标与模型：
//   - 已判定两个通道共用额度：另一个通道同步冷却到同一时间，驻留 Worker 立即登记淘汰；
//   - 观察中：只冷却本通道，等同账号另一通道的下一次尝试作为证据，另一个通道仍可用时不登记淘汰
func TestUsageLimitAppliesDailyQuota(t *testing.T) {
	// 每日限额冷却到太平洋时间次日零点，剩余要不少于 rotationMinRemaining 才登记淘汰
	skipNearQuotaDay(t, rotationMinRemaining+time.Minute)
	for _, shared := range []bool{true, false} {
		name := "观察中"
		if shared {
			name = "已判定共用"
		}
		t.Run(name, func(t *testing.T) {
			manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 1, 0, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			service := retireTestService(ctx, manager, pool, requests)
			service.rateLimits = newRateLimitGuard()
			pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
			target := aistudio.Model{ID: rateLadderModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
			base := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
			for _, accountID := range []string{partitionNormalA, partitionNormalB} {
				if err := pool.SetCatalog(accountID, aistudio.BenefitTierPro, []aistudio.Model{base, target}); err != nil {
					t.Fatal(err)
				}
				if err := pool.SetBuildCatalog(accountID, []aistudio.Model{target}); err != nil {
					t.Fatal(err)
				}
			}
			if shared {
				markQuotaShared(service, rateLadderModel)
			}
			if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
				t.Fatal(err)
			}
			lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{
				AccountID: partitionNormalA, ModelID: rateLadderModel, Method: "generateContent", PlaygroundOnly: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lease.Release() }()
			reset := aistudio.NextQuotaDay(time.Now())
			cooldown, ok, stateErr := service.applyQuotaCooldown(
				lease, partitionNormalA, rateLadderModel, paidUsageLimitError("GenerateContent"), "", aistudio.PoolScopeNormal, true, true,
			)
			if !ok || stateErr != nil || cooldown.Kind != aistudio.DailyQuotaKind || cooldown.Global || cooldown.Escalate || !cooldown.Until.Equal(reset) {
				t.Fatalf("C 应按模型的每日限额冷却到次日零点 %s：%+v ok=%t err=%v", reset, cooldown, ok, stateErr)
			}
			if got := rateLimitFailures(service, rateLadderModel); got != 0 {
				t.Fatalf("每日限额不应计入号池级限流：记录 %d 个账号", got)
			}
			if countSourceLogs(requests, "", "连续限流") != 0 {
				t.Fatalf("每日限额不应走逐级冷却，日志 = %v", serviceLogMessages(requests))
			}
			if got := countSourceLogs(requests, partitionNormalA, "账号冷却 | 类型=每日限额 | 范围="+rateLadderModel+" | 恢复=", usageLimitLogFields); got != 1 {
				t.Fatalf("冷却日志应带上限、指标与模型，日志 = %v", serviceLogMessages(requests))
			}
			playground, build := rateCooldowns(t, pool, partitionNormalA)
			if !playground.Equal(cooldown.Until) {
				t.Fatalf("Playground 冷却到 %s，期望 %s", playground, cooldown.Until)
			}
			if !shared {
				if !build.IsZero() && build.After(time.Now()) {
					t.Fatalf("观察中不应同步冷却 Build：%s", build)
				}
				if got := manager.retirementCount(); got != 0 {
					t.Fatalf("另一个通道仍可用时不应登记淘汰，登记数 = %d", got)
				}
				service.quota.mu.Lock()
				_, pending := service.quota.pending[quotaSharingKey(partitionNormalA, rateLadderModel, aistudio.ChannelBuild)]
				service.quota.mu.Unlock()
				if !pending {
					t.Fatal("观察中应等待同账号 Build 通道的下一次尝试")
				}
				return
			}
			if !build.Equal(cooldown.Until) {
				t.Fatalf("已判定共用时 Build 应同步冷却到 %s，实际 %s", cooldown.Until, build)
			}
			if !hasLogContaining(requests, "账号冷却 | 类型=每日限额 | 范围=build:"+rateLadderModel, "与另一通道共用额度，同步冷却") {
				t.Fatalf("应记录另一通道的同步冷却，日志 = %v", serviceLogMessages(requests))
			}
			if !pool.AccountLongCooling(partitionNormalA, rateLadderModel, rotationMinRemaining) {
				t.Fatal("两个通道都冷却后该模型应成为死槽位")
			}
			if got := manager.retirementCount(); got != 1 {
				t.Fatalf("两个通道都冷却后应立即登记淘汰，登记数 = %d", got)
			}
		})
	}
}

// usageLimitUpstream 让 limited 中的账号对每次生成返回 C 的每日限额 429，其余账号正常返回；记录“账号/通道”
type usageLimitUpstream struct {
	mu      sync.Mutex
	limited map[string]bool
	calls   []string
}

func (upstream *usageLimitUpstream) Models(context.Context) ([]aistudio.Model, error) {
	return nil, nil
}

func (upstream *usageLimitUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *usageLimitUpstream) called() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

func (upstream *usageLimitUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	channel := ""
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		channel = string(lease.Channel())
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID+"/"+channel)
	limited := upstream.limited[request.AccountID]
	upstream.mu.Unlock()
	if limited {
		method := "GenerateContent"
		if channel == string(aistudio.ChannelBuild) {
			method = "ProxyStreamedCall"
		}
		return nil, paidUsageLimitError(method)
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

// TestUsageLimitSwitchesAccount 请求遇到 C 后立即换号、不退避：
//   - 已判定两个通道共用额度：同一账号两个通道一起冷却，不再去另一个通道白试，直接换到另一个账号完成，驻留 Worker 登记淘汰；
//   - 观察中：先在同一账号的另一个通道再试一次（记一次“共用”证据），两个通道都冷却后换号并登记淘汰
func TestUsageLimitSwitchesAccount(t *testing.T) {
	skipNearQuotaDay(t, rotationMinRemaining+time.Minute)
	const limited, healthy = "a-usage@example.com", "b-usage@example.com"
	for _, shared := range []bool{true, false} {
		name := "观察中"
		if shared {
			name = "已判定共用"
		}
		t.Run(name, func(t *testing.T) {
			upstream := &usageLimitUpstream{limited: map[string]bool{limited: true}}
			service, pool := rateLadderService(t, upstream, limited, healthy)
			if shared {
				markQuotaShared(service, rateLadderModel)
			}
			text, err, elapsed := runModelRequest(t, service, "req-usage-limit", "", rateLadderModel)
			if err != nil || text != "hello" {
				t.Fatalf("应换到另一个账号完成：text=%q err=%v calls=%v", text, err, upstream.called())
			}
			want := []string{limited + "/" + string(aistudio.ChannelPlayground)}
			if !shared {
				want = append(want, limited+"/"+string(aistudio.ChannelBuild))
			}
			calls := upstream.called()
			if len(calls) != len(want)+1 || !slices.Equal(calls[:len(want)], want) || !strings.HasPrefix(calls[len(want)], healthy+"/") {
				t.Fatalf("尝试 = %v，期望先 %v、再换到 %s", calls, want, healthy)
			}
			// 一般失败的第一次退避为 200ms：额度 429 换号不退避
			if shared && elapsed >= 180*time.Millisecond {
				t.Fatalf("换号用时 %s，期望不退避", elapsed)
			}
			playground, build := rateCooldowns(t, pool, limited)
			for label, until := range map[string]time.Time{"Playground": playground, "Build": build} {
				if remaining := time.Until(until); remaining < rotationMinRemaining {
					t.Fatalf("%s 冷却剩余 %s，期望冷却到太平洋时间次日零点", label, remaining)
				}
			}
			if got := service.workers.retirementCount(); got != 1 {
				t.Fatalf("两个通道都冷却后驻留 Worker 成了死槽位，应登记淘汰，登记数 = %d", got)
			}
			if got := rateLimitFailures(service, rateLadderModel); got != 0 || countSourceLogs(service.requests, "", "连续限流") != 0 {
				t.Fatalf("每日限额不应走逐级冷却：号池级限流记录 %d 个，日志 = %v", got, serviceLogMessages(service.requests))
			}
			if got := countSourceLogs(service.requests, limited, "账号冷却 | 类型=每日限额", usageLimitLogFields); got != len(want) {
				t.Fatalf("应有 %d 行带上限、指标与模型的每日限额冷却，日志 = %v", len(want), serviceLogMessages(service.requests))
			}
			service.quota.mu.Lock()
			stats := service.quota.models[rateLadderModel]
			service.quota.mu.Unlock()
			if wantShared := map[bool]int{true: quotaSharedMinEvidence, false: 1}[shared]; stats == nil || stats.Shared != wantShared {
				t.Fatalf("通道共用证据 = %+v，期望共用 %d 次", stats, wantShared)
			}
		})
	}
}

// TestQuotaCooldownFieldsModelAndDetailTypes 冷却日志字段：上限、指标之后是模型；没有指标时写出详情类型（只用于排查裸 429），
// 有指标或没有详情时不写
func TestQuotaCooldownFieldsModelAndDetailTypes(t *testing.T) {
	for _, test := range []struct {
		cooldown aistudio.QuotaCooldown
		want     string
	}{
		{aistudio.QuotaCooldown{LimitValue: "10", Metric: "generate_requests_per_model_per_user", Model: "gemini-3.1-pro",
			DetailTypes: "google.internal.alkali.applications.makersuite.v1.GeminiApiErrorDetails"},
			" | 上限=10 | 指标=generate_requests_per_model_per_user | 模型=gemini-3.1-pro"},
		{aistudio.QuotaCooldown{LimitValue: "12", Metric: "generate_requests_global", DetailTypes: "google.rpc.ErrorInfo,google.rpc.Help"},
			" | 上限=12 | 指标=generate_requests_global"},
		{aistudio.QuotaCooldown{DetailTypes: "google.internal.foo.v1.Unknown,google.rpc.Help"}, " | 详情类型=google.internal.foo.v1.Unknown,google.rpc.Help"},
		{aistudio.QuotaCooldown{}, ""},
	} {
		if got := quotaCooldownFields(test.cooldown); got != test.want {
			t.Fatalf("quotaCooldownFields(%+v)=%q，期望 %q", test.cooldown, got, test.want)
		}
	}
	// 裸 429 带着未识别的详情：逐级冷却的日志同样带上详情类型
	bare := aistudio.DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"Quota exceeded",[["type.googleapis.com/google.internal.foo.v1.Unknown",[1]]]]`))
	cooldown, ok := aistudio.QuotaCooldownForError(bare, time.Now())
	if !ok || !cooldown.Escalate || quotaCooldownFields(cooldown) != " | 详情类型=google.internal.foo.v1.Unknown" {
		t.Fatalf("带未识别详情的裸 429：%+v ok=%t 字段=%q", cooldown, ok, quotaCooldownFields(cooldown))
	}
}

// TestUltraPrimaryUsageLimitProbeNotSwitch Ultra 主用模型遇到 C、两个通道都开且通道判定还在观察中：同账号另一通道的那次探测
// 不算换号，不计入每日类的换号上限。号池全是 C 时照样换满 primaryQuotaSwitchLimit 次账号（碰到 3 个账号）才按额度不足结束，
// 额度失败按账号计 3 次；同账号换通道时进度写“同账号换通道重试”，不写“换号重试”
func TestUltraPrimaryUsageLimitProbeNotSwitch(t *testing.T) {
	skipNearQuotaDay(t, rotationMinRemaining+time.Minute)
	accounts := ultraTestIDs(6)
	limited := make(map[string]bool, len(accounts))
	for _, accountID := range accounts {
		limited[accountID] = true
	}
	upstream := &usageLimitUpstream{limited: limited}
	service, pool := primaryBusyService(t, upstream, accounts...)
	markUltra(t, pool, accounts...)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	for _, accountID := range accounts {
		if err := pool.SetBuildCatalog(accountID, []aistudio.Model{{
			ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
	_, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-usage-probe", "")
	var quota *aistudio.PrimaryQuotaError
	if !errors.As(err, &quota) || quota.Reason != primaryQuotaReasonLimit || quota.QuotaHits != primaryQuotaSwitchLimit+1 || quota.RateHits != 0 {
		t.Fatalf("err=%v，期望额度失败按账号计 %d 次后按换号上限结束，调用 = %v", err, primaryQuotaSwitchLimit+1, upstream.called())
	}
	// 每个账号先 Playground、再在同账号 Build 探测一次；第三个账号的第一次额度失败就到上限，不再探测
	calls := upstream.called()
	pg, build := string(aistudio.ChannelPlayground), string(aistudio.ChannelBuild)
	wantChannels := []string{pg, build, pg, build, pg}
	if len(calls) != len(wantChannels) {
		t.Fatalf("调用 = %v，期望通道依次为 %v", calls, wantChannels)
	}
	distinct := map[string]struct{}{}
	for index, call := range calls {
		accountID, channel, _ := strings.Cut(call, "/")
		distinct[accountID] = struct{}{}
		if channel != wantChannels[index] || index%2 == 1 && !strings.HasPrefix(calls[index-1], accountID+"/") {
			t.Fatalf("调用 = %v，期望每个账号先 Playground 再同账号 Build", calls)
		}
	}
	if len(distinct) != primaryQuotaSwitchLimit+1 {
		t.Fatalf("调用 = %v，碰到 %d 个账号，期望 %d 个", calls, len(distinct), primaryQuotaSwitchLimit+1)
	}
	for _, fragment := range []string{
		"额度 429，同账号换通道重试 | 额度失败=1/2",
		"额度 429，换号重试（只用已启动的 Worker）| 额度失败=1/2",
		"额度 429，同账号换通道重试 | 额度失败=2/2",
		"额度 429，换号重试（只用已启动的 Worker）| 额度失败=2/2",
		"Ultra 主用模型额度不足，立即返回 429 | 原因=额度失败已达换号上限 | 额度失败=3 | 总耗时=",
	} {
		if !progressLogged(service, fragment) {
			t.Fatalf("请求进度应写明 %q", fragment)
		}
	}
	assertNoBusyAccounts(t, pool)
}
