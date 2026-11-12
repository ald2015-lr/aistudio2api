package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 官网停用了 Build 通道的生成，而 Playground 只是冷却或额度刚用完（见 runtime.go 的 buildDisabled 与 build_pause.go）：默认上游通道顺序
// （Playground 在前），支持该模型的账号的 Playground 在冷却、Build 还没被本服务暂停（刚重启、按账号暂停到期、全局暂停还没建立）时，
// 请求先落到它们的 Build 上遇到 PROXY_GENERATION_DISABLED。账号的 Build 随即暂停、仍留在候选里，之后按 Playground 的冷却给出
// 全部冷却或额度不足（429，带 Retry-After；交给 MODEL_FALLBACKS），不以“Playground 接不了”的 503 结束。
// 真正接不了的情形（Playground 目录不列该模型、UPSTREAM_CHANNELS 只有 build）仍按原来的 503 结束

// coolingUpstream 在 buildOffUpstream 之上按账号给 Playground、Build 各排一串错误（依次返回，用完后照常：Build 照常为
// PROXY_GENERATION_DISABLED），并按顺序记下每次调用的模型
type coolingUpstream struct {
	buildOffUpstream
	playgroundErrors map[string][]error
	buildErrors      map[string][]error
	models           []string
}

func (upstream *coolingUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	channel := ""
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		channel = string(lease.Channel())
	}
	upstream.mu.Lock()
	upstream.models = append(upstream.models, strings.TrimPrefix(request.Model, "models/"))
	var queued error
	queues := upstream.playgroundErrors
	if channel == string(aistudio.ChannelBuild) {
		queues = upstream.buildErrors
	}
	if queue := queues[request.AccountID]; channel != "" && len(queue) > 0 {
		queued, queues[request.AccountID] = queue[0], queue[1:]
		upstream.calls = append(upstream.calls, request.AccountID+"/"+channel)
	}
	upstream.mu.Unlock()
	if queued != nil {
		return nil, queued
	}
	return upstream.buildOffUpstream.Generate(ctx, request)
}

// calledModels 返回上游按顺序收到的生成请求的模型
func (upstream *coolingUpstream) calledModels() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.models)
}

// coolingModel 返回测试目录里的模型：argon、carbon 取改用测试的目录（支持思考与函数声明），其余为只会对话的模型
func coolingModel(id string) aistudio.Model {
	for _, model := range fallbackCatalog() {
		if model.ID == id {
			return model
		}
	}
	return aistudio.Model{ID: id, Name: id, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
}

// coolingService 建立运行中的生成服务（经公开 API 处理链发请求）：上游通道为 channels，按 fill-first、账号按 ID 顺序选号，
// 权益为 tier；playground[账号]、build[账号] 为该账号两个通道目录里的模型（Playground 目录另有 testRuntimeModel）。
// 预写未来的模型资格（请求结束后的后台写入不改动账户状态文件）；Build 暂停的时钟可调、没有抖动；
// MODEL_FALLBACKS 为默认规则、范围为 ultra，Ultra 主用模型为 gemini-4-argon
func coolingService(
	t *testing.T, upstream aistudio.Service, tier aistudio.BenefitTier, channels []aistudio.Channel, accounts []string,
	playground map[string][]string, build map[string][]string,
) (*trackedService, *aistudio.AccountPool, *time.Time) {
	t.Helper()
	service, pool := runningGenerateService(t, upstream, context.Background(), time.Minute, accounts...)
	pool.SetUpstreamChannels(channels)
	pool.SetRoutingStrategy("fill-first")
	snapshot := []aistudio.Model{coolingModel(testRuntimeModel)}
	seen := map[string]bool{testRuntimeModel: true}
	models := func(ids []string) []aistudio.Model {
		list := make([]aistudio.Model, 0, len(ids))
		for _, id := range ids {
			list = append(list, coolingModel(id))
			if !seen[id] {
				seen[id] = true
				snapshot = append(snapshot, coolingModel(id))
			}
		}
		return list
	}
	for _, accountID := range accounts {
		if err := pool.SetCatalog(accountID, tier, append([]aistudio.Model{coolingModel(testRuntimeModel)}, models(playground[accountID])...)); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetBuildCatalog(accountID, models(build[accountID])); err != nil {
			t.Fatal(err)
		}
	}
	service.replaceModelSnapshot(snapshot)
	setUltraPrimaryModels(service, []string{fallbackSource}, 10*time.Second)
	service.setModelFallbacks(config.DefaultModelFallbacks, config.ModelFallbackScopeUltra)
	for _, accountID := range accounts {
		verifyModelAccessAhead(t, pool, accountID, seenModels(seen), "")
	}
	now := time.Now()
	tracker := newBuildPauseTracker(pool, service.requests)
	tracker.now = func() time.Time { return now }
	tracker.jitter = func(time.Duration) time.Duration { return 0 }
	service.buildPause = tracker
	return service, pool, &now
}

func seenModels(seen map[string]bool) []string {
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// verifyModelAccessAhead 给账号的模型在两个通道上预写一小时后的模型资格（skip 为不预写的作用域）：之后检查时间更早的冷却与资格都不再写入
func verifyModelAccessAhead(t *testing.T, pool *aistudio.AccountPool, accountID string, models []string, skip string) {
	t.Helper()
	generation := pool.ModelAccessGeneration(accountID)
	future := time.Now().Add(time.Hour)
	for _, model := range models {
		for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
			scope := aistudio.ChannelCooldownScope(channel, model)
			if scope == skip {
				continue
			}
			if _, err := pool.MarkModelAccessVerifiedIfGeneration(accountID, scope, generation, future); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// coolPlayground 让账号的 Playground 在 model 上冷却 d（每日限额）；检查时间晚于预写的模型资格，冷却照常写入。返回恢复时间
func coolPlayground(t *testing.T, pool *aistudio.AccountPool, accountID string, model string, d time.Duration) time.Time {
	t.Helper()
	until := time.Now().Add(d)
	if err := pool.MarkCooldownIfGeneration(
		accountID, aistudio.ChannelCooldownScope(aistudio.ChannelPlayground, model), pool.ModelAccessGeneration(accountID),
		time.Now().Add(2*time.Hour), until, "每日限额",
	); err != nil {
		t.Fatal(err)
	}
	return until
}

// coolingResult 为经公开 API 处理链发出的一次请求的结果：响应、这次请求的上游尝试（“账号/通道”）与模型
type coolingResult struct {
	recorder *httptest.ResponseRecorder
	entries  []api.AccessLog
	calls    []string
	models   []string
}

// postCooling 经公开 API 处理链发出非流式请求（path 带 /ultra 前缀时走 Ultra 号池）
func postCooling(t *testing.T, service *trackedService, upstream *coolingUpstream, path string, model string, stream bool) coolingResult {
	t.Helper()
	calls, models := len(upstream.called()), len(upstream.calledModels())
	body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, model, stream)
	recorder, entries := postFallback(t, service, path, body)
	return coolingResult{recorder: recorder, entries: entries, calls: upstream.called()[calls:], models: upstream.calledModels()[models:]}
}

// assertCooling429 断言客户端收到额度类 429：官方文案、Retry-After 为 until 距现在的秒数（只往后加最多 3 秒的抖动），
// 不是 Build 不可用的 503；访问日志的错误含 internal（内部原因）
func assertCooling429(t *testing.T, result coolingResult, until time.Time, internal string) {
	t.Helper()
	text := result.recorder.Body.String()
	if result.recorder.Code != http.StatusTooManyRequests || !strings.Contains(text, "Resource has been exhausted") {
		t.Fatalf("状态 %d，响应 %s，期望额度类 429", result.recorder.Code, text)
	}
	if strings.Contains(text, "Build channel") {
		t.Fatalf("响应不应说 Build 通道不可用：%s", text)
	}
	retryAfter, err := strconv.Atoi(result.recorder.Header().Get("Retry-After"))
	want := int(math.Ceil(time.Until(until).Seconds()))
	if err != nil || retryAfter < want-2 || retryAfter > want+4 {
		t.Fatalf("Retry-After = %q，期望约 %d 秒（最早恢复的 Playground）", result.recorder.Header().Get("Retry-After"), want)
	}
	if len(result.entries) != 1 || !strings.Contains(result.entries[0].Error, internal) {
		t.Fatalf("访问日志 = %+v，期望错误含 %q", result.entries, internal)
	}
}

// assertBuild503 断言客户端收到 Build 不可用的 503（public 为对外说明的片段），没有 Retry-After
func assertBuild503(t *testing.T, result coolingResult, public string) {
	t.Helper()
	text := result.recorder.Body.String()
	if result.recorder.Code != http.StatusServiceUnavailable || !strings.Contains(text, public) {
		t.Fatalf("状态 %d，响应 %s，期望 503 与 %q", result.recorder.Code, text, public)
	}
	if value := result.recorder.Header().Get("Retry-After"); value != "" {
		t.Fatalf("Build 不可用的 503 不应带 Retry-After：%s", value)
	}
}

const (
	// coolingNoPlayground 为对外说明里 Playground 接不了的片段，coolingBuildOnly 为只能经 Build 调用的片段
	coolingNoPlayground = "the Playground channel cannot serve this request right now"
	coolingBuildOnly    = "is only available through the AI Studio Build channel"
	// coolingSwitchNote 为同账号的 Playground 现在不可用、账号留在候选里的进度片段，coolingEndNote 为按 Build 不可用结束的进度片段
	coolingSwitchNote = "改走 Playground 重试 | 同账号=否 | 该账号的 Playground 现在不可用"
	coolingEndNote    = "没有其他通道能接这个请求"
	coolingAllCooling = "可用账户均在冷却"
	// coolingRecentQuotaNote 为本请求刚在该账号的 Playground 上遇到额度 429、冷却很快结束，不在同账号上等它恢复的进度片段
	coolingRecentQuotaNote = "改走 Playground 重试 | 同账号=否 | 本请求刚在该账号的 Playground 上遇到额度 429"
)

// coolingMinuteQuota 为 Playground 的分钟限额 429，上游给出 3 秒后重试（冷却 3 秒，短于排队上限 cooldownQueueLimit）
func coolingMinuteQuota() error {
	return coolingMinuteQuotaAfter(3 * time.Second)
}

// coolingMinuteQuotaAfter 为上游给出 delay 后重试的 Playground 分钟限额 429
func coolingMinuteQuotaAfter(delay time.Duration) error {
	return &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
		Message: "Quota exceeded for quota metric 'Generate Content API requests per minute'", RetryDelay: delay,
	}
}

var coolingDefaultChannels = []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild}

// coolingCatalogs 返回 accounts 两个通道目录都有 models 的目录
func coolingCatalogs(accounts []string, models ...string) map[string][]string {
	catalogs := make(map[string][]string, len(accounts))
	for _, accountID := range accounts {
		catalogs[accountID] = slices.Clone(models)
	}
	return catalogs
}

// TestBuildDisabledPlaygroundCooling 支持该模型的账号的 Playground 都在冷却、Build 还没被本服务暂停时，请求在它们的 Build 上
// 依次遇到 PROXY_GENERATION_DISABLED：每个账号只白试一次 Build（暂停后不再选它的 Build），之后按全部冷却返回 429，Retry-After 为
// 最早恢复的 Playground；不以“Playground 接不了”的 503 结束。k 为支持该模型的账号数，部分账号支持时同样如此。
// 冷却短于排队上限时等它恢复，在同账号的 Playground 完成
func TestBuildDisabledPlaygroundCooling(t *testing.T) {
	t.Run("k=1：暂停到期后每次都是 429", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := []string{buildOffA}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, _, now := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		until := coolPlayground(t, service.pool, buildOffA, buildOffShared, 3*time.Hour)
		for round := range 2 {
			result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
			assertCooling429(t, result, until, coolingAllCooling)
			if want := []string{buildOffA + "/build"}; !slices.Equal(result.calls, want) {
				t.Fatalf("第 %d 次：尝试 = %v，期望 %v", round+1, result.calls, want)
			}
			// 暂停中：不再经 Build，直接按全部冷却返回
			result = postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
			assertCooling429(t, result, until, coolingAllCooling)
			if len(result.calls) != 0 {
				t.Fatalf("第 %d 次暂停中：尝试 = %v，期望不发上游请求", round+1, result.calls)
			}
			// 按账号暂停到期（30 分钟），下一次又在 Build 上试探一次
			*now = now.Add(buildPauseInterval + time.Second)
		}
		if got := progressCountAll(service, coolingSwitchNote); got != 2 {
			t.Fatalf("进度“%s”写了 %d 行，期望 2 行", coolingSwitchNote, got)
		}
		if progressLogged(service, coolingEndNote) {
			t.Fatal("不应按 Build 不可用结束")
		}
	})
	t.Run("冷却短于排队上限：等它恢复后在同账号的 Playground 完成", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := []string{buildOffA}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, _, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		coolPlayground(t, service.pool, buildOffA, buildOffShared, 1500*time.Millisecond)
		startedAt := time.Now()
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		if text := result.recorder.Body.String(); result.recorder.Code != http.StatusOK || !strings.Contains(text, "hello from playground") {
			t.Fatalf("状态 %d，响应 %s，期望等冷却结束后经 Playground 成功", result.recorder.Code, text)
		}
		if want := []string{buildOffA + "/build", buildOffA + "/playground"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
		if elapsed := time.Since(startedAt); elapsed < time.Second {
			t.Fatalf("用时 %s，应等到 Playground 冷却结束", elapsed)
		}
		if !progressLogged(service, coolingSwitchNote) {
			t.Fatal("进度应写明账号留在候选里")
		}
	})
	t.Run("k=1 流式", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := []string{buildOffA}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, _, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		until := coolPlayground(t, service.pool, buildOffA, buildOffShared, 3*time.Hour)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, true)
		assertCooling429(t, result, until, coolingAllCooling)
		if want := []string{buildOffA + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
	})
	t.Run("部分账号支持该模型", func(t *testing.T) {
		upstream := &coolingUpstream{}
		// 3 个可调度账号（换号次数按 3 计），只有 A 的两个通道目录有该模型
		accounts := []string{buildOffA, buildOffB, buildOffC}
		catalogs := map[string][]string{buildOffA: {buildOffShared}}
		service, _, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		until := coolPlayground(t, service.pool, buildOffA, buildOffShared, 2*time.Hour)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		assertCooling429(t, result, until, coolingAllCooling)
		if want := []string{buildOffA + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
		if !progressLogged(service, coolingSwitchNote) || progressLogged(service, coolingEndNote) {
			t.Fatal("进度应写明账号留在候选里，不应按 Build 不可用结束")
		}
	})
	t.Run("k=2：暂停同时到期", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := []string{buildOffA, buildOffB}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, _, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		coolPlayground(t, service.pool, buildOffA, buildOffShared, 2*time.Hour)
		until := coolPlayground(t, service.pool, buildOffB, buildOffShared, time.Hour)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		assertCooling429(t, result, until, coolingAllCooling)
		if want := []string{buildOffA + "/build", buildOffB + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v（每个账号的 Build 只试一次）", result.calls, want)
		}
		if got := progressCountAll(service, coolingSwitchNote); got != 2 {
			t.Fatalf("进度“%s”写了 %d 行，期望 2 行", coolingSwitchNote, got)
		}
	})
	t.Run("k=2：暂停到期时间错开", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := []string{buildOffA, buildOffB}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, _, now := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		// A 先恢复（40 分钟），B 晚恢复（2 小时）；A 的按账号暂停已到期、B 的还在暂停中
		untilA := coolPlayground(t, service.pool, buildOffA, buildOffShared, 40*time.Minute)
		coolPlayground(t, service.pool, buildOffB, buildOffShared, 2*time.Hour)
		tracker := service.buildPauses()
		tracker.disabled(buildOffA, buildOffA, false)
		*now = now.Add(20 * time.Minute)
		tracker.disabled(buildOffB, buildOffB, false)
		*now = now.Add(15 * time.Minute)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		// Retry-After 取最早恢复的 A，不只看还在候选里的 B
		assertCooling429(t, result, untilA, coolingAllCooling)
		if want := []string{buildOffA + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v（B 的 Build 还在暂停中）", result.calls, want)
		}
	})
	t.Run("k=3：同一个请求里进入全局暂停", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := []string{buildOffA, buildOffB, buildOffC}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		until := coolPlayground(t, pool, buildOffA, buildOffShared, time.Hour)
		coolPlayground(t, pool, buildOffB, buildOffShared, 2*time.Hour)
		coolPlayground(t, pool, buildOffC, buildOffShared, 3*time.Hour)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		assertCooling429(t, result, until, coolingAllCooling)
		if want := []string{buildOffA + "/build", buildOffB + "/build", buildOffC + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
		if !pool.BuildGenerationPaused() || warningCount(service, "全部账号") != 1 {
			t.Fatal("3 个账号遇到后应进入全局暂停并写一行 WARN")
		}
		result = postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		assertCooling429(t, result, until, coolingAllCooling)
		if len(result.calls) != 0 {
			t.Fatalf("全局暂停后尝试 = %v，期望不发上游请求", result.calls)
		}
	})
}

// allowQuotaCooldown 去掉账号 Playground 上 buildOffShared 预写的资格：本请求里额度 429 的冷却要能写入
func allowQuotaCooldown(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	allowModelQuotaCooldown(t, pool, accountID, buildOffShared, testRuntimeModel)
}

// allowModelQuotaCooldown 去掉账号 Playground 上 model 预写的资格（model 的 Build 与 others 的两个通道照常预写）
func allowModelQuotaCooldown(t *testing.T, pool *aistudio.AccountPool, accountID string, model string, others ...string) {
	t.Helper()
	if err := pool.ResetModelAccess(accountID); err != nil {
		t.Fatal(err)
	}
	verifyModelAccessAhead(t, pool, accountID, append([]string{model}, others...), aistudio.ChannelCooldownScope(aistudio.ChannelPlayground, model))
}

// markCoolingAuthRequired 把账号标成需要重新登录（目录照旧，暂时不可调度，选号没有候选时报 AccountsNotReadyError）
func markCoolingAuthRequired(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	if err := pool.MarkAuthRequired(accountID, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
}

// markCoolingDisabled 停用账号（目录照旧，不参与调度，选号没有候选时报 AccountsNotReadyError）
func markCoolingDisabled(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	if _, _, err := pool.SetEnabled(accountID, false); err != nil {
		t.Fatal(err)
	}
}

// playgroundUntil 返回账号的 Playground 在 buildOffShared 上的冷却恢复时间（本请求里额度 429 写入的）
func playgroundUntil(t *testing.T, pool *aistudio.AccountPool, accountID string) time.Time {
	t.Helper()
	status, _ := pool.StatusOf(accountID)
	cooldown, ok := status.Cooldowns[buildOffShared]
	if !ok || !cooldown.Active(time.Now()) {
		t.Fatalf("账号 %s 的 Playground 应在额度 429 后冷却：%+v", accountID, status.Cooldowns)
	}
	return cooldown.Until
}

// TestBuildDisabledAfterPlaygroundFailure Playground 在本请求里当场失败后改走 Build 被拒：额度 429 后同账号换到 Build（一个与两个账号）、
// Playground 先报码 4 后换到下一个账号的 Build、全局暂停中的试探被拒，都按额度类 429 结束（带 Retry-After），不盖成 Build 不可用的 503
func TestBuildDisabledAfterPlaygroundFailure(t *testing.T) {
	t.Run("当场额度 429 后同账号 Build 被拒", func(t *testing.T) {
		upstream := &coolingUpstream{buildOffUpstream: buildOffUpstream{playgroundQuota: true}}
		accounts := []string{buildOffA}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		allowQuotaCooldown(t, pool, buildOffA)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		assertCooling429(t, result, playgroundUntil(t, pool, buildOffA), "额度不足")
		if want := []string{buildOffA + "/playground", buildOffA + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
		if !progressLogged(service, coolingSwitchNote) || progressLogged(service, coolingEndNote) {
			t.Fatal("进度应写明账号留在候选里，不应按 Build 不可用结束")
		}
	})
	t.Run("当场额度 429、同账号 Build 被拒后换号到下一个账号的 Build 也被拒", func(t *testing.T) {
		upstream := &coolingUpstream{buildOffUpstream: buildOffUpstream{playgroundQuota: true}}
		accounts := []string{buildOffA, buildOffB}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		allowQuotaCooldown(t, pool, buildOffA)
		untilB := coolPlayground(t, pool, buildOffB, buildOffShared, 90*time.Minute)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		until := playgroundUntil(t, pool, buildOffA)
		if untilB.Before(until) {
			until = untilB
		}
		assertCooling429(t, result, until, "额度不足")
		if want := []string{buildOffA + "/playground", buildOffA + "/build", buildOffB + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
	})
	t.Run("当场分钟限额 429（短冷却）后同账号 Build 被拒：不在同账号上等它恢复", func(t *testing.T) {
		// 与只启用 Playground 时相同：限流类失败后不在同账号上再试，没有其他账号时立即按额度不足返回 429，Retry-After 取 5 秒与
		// 这次的冷却中较早者（这里是 3 秒的冷却；冷却更长时见下文“冷却超过 5 秒”）
		upstream := &coolingUpstream{playgroundErrors: map[string][]error{buildOffA: {coolingMinuteQuota(), coolingMinuteQuota()}}}
		accounts := []string{buildOffA}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		allowQuotaCooldown(t, pool, buildOffA)
		startedAt := time.Now()
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		elapsed := time.Since(startedAt)
		assertCooling429(t, result, playgroundUntil(t, pool, buildOffA), "没有其他可换的账号")
		// Retry-After 为这次的冷却（3 秒，早于 5 秒），不落到 1 秒下限
		if retryAfter, _ := strconv.Atoi(result.recorder.Header().Get("Retry-After")); retryAfter < 2 || retryAfter > 4 {
			t.Fatalf("Retry-After = %q，期望约 3 秒（这次分钟限额的冷却）", result.recorder.Header().Get("Retry-After"))
		}
		if want := []string{buildOffA + "/playground", buildOffA + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v（不在同账号的 Playground 上等冷却结束再试）", result.calls, want)
		}
		if elapsed >= 2*time.Second {
			t.Fatalf("用时 %s，不应等同账号的短冷却", elapsed)
		}
		if !progressLogged(service, coolingRecentQuotaNote) || progressLogged(service, coolingSwitchNote) || progressLogged(service, coolingEndNote) {
			t.Fatal("进度应写明本请求刚在该账号的 Playground 上遇到额度 429、不在同账号上等它恢复，不应写账号留在候选里或按 Build 不可用结束")
		}
	})
	t.Run("当场分钟限额 429 后同账号 Build 被拒：换到其他账号的 Playground 完成", func(t *testing.T) {
		upstream := &coolingUpstream{playgroundErrors: map[string][]error{buildOffA: {coolingMinuteQuota(), coolingMinuteQuota()}}}
		accounts := []string{buildOffA, buildOffB}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		allowQuotaCooldown(t, pool, buildOffA)
		startedAt := time.Now()
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		if text := result.recorder.Body.String(); result.recorder.Code != http.StatusOK || !strings.Contains(text, "hello from playground") {
			t.Fatalf("状态 %d，响应 %s，期望换到 B 的 Playground 成功", result.recorder.Code, text)
		}
		if want := []string{buildOffA + "/playground", buildOffA + "/build", buildOffB + "/playground"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
		if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
			t.Fatalf("用时 %s，不应等 A 的短冷却", elapsed)
		}
	})
	// 号池另有一个目录相同、却暂时不可调度的账号（需要重新登录或已停用）：排除 A 后的选号返回 AccountsNotReadyError，不是没有候选，
	// 仍按额度不足立即返回 429（Retry-After 为这次 3 秒的冷却，早于 5 秒），与只启用 Playground 时相同，不以 Build 不可用的 503 结束
	for _, notReady := range []struct {
		name string
		mark func(*testing.T, *aistudio.AccountPool, string)
	}{
		{"需要重新登录", markCoolingAuthRequired},
		{"已停用", markCoolingDisabled},
	} {
		for _, channels := range []struct {
			name string
			list []aistudio.Channel
		}{
			{"默认通道", coolingDefaultChannels},
			{"只启用 Playground", []aistudio.Channel{aistudio.ChannelPlayground}},
		} {
			t.Run("当场分钟限额 429 后同账号 Build 被拒、另一账号"+notReady.name+"："+channels.name, func(t *testing.T) {
				upstream := &coolingUpstream{playgroundErrors: map[string][]error{buildOffA: {coolingMinuteQuota(), coolingMinuteQuota()}}}
				accounts := []string{buildOffA, buildOffB}
				catalogs := coolingCatalogs(accounts, buildOffShared)
				service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, channels.list, accounts, catalogs, catalogs)
				allowQuotaCooldown(t, pool, buildOffA)
				notReady.mark(t, pool, buildOffB)
				startedAt := time.Now()
				result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
				elapsed := time.Since(startedAt)
				assertCooling429(t, result, playgroundUntil(t, pool, buildOffA), "没有其他可换的账号")
				if retryAfter, _ := strconv.Atoi(result.recorder.Header().Get("Retry-After")); retryAfter < 2 || retryAfter > 4 {
					t.Fatalf("Retry-After = %q，期望约 3 秒（这次分钟限额的冷却）", result.recorder.Header().Get("Retry-After"))
				}
				want := []string{buildOffA + "/playground", buildOffA + "/build"}
				if len(channels.list) == 1 {
					want = want[:1]
				}
				if !slices.Equal(result.calls, want) {
					t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
				}
				if elapsed >= 2*time.Second {
					t.Fatalf("用时 %s，不应等 A 的短冷却", elapsed)
				}
				if progressLogged(service, coolingEndNote) {
					t.Fatal("不应按 Build 不可用结束")
				}
				if len(channels.list) == 2 && !progressLogged(service, coolingRecentQuotaNote) {
					t.Fatal("进度应写明本请求刚在该账号的 Playground 上遇到额度 429、不在同账号上等它恢复")
				}
			})
		}
	}
	t.Run("当场分钟限额 429 后同账号 Build 被拒，换到只在 Build 目录列出该模型的账号也被拒、另一账号需要重新登录", func(t *testing.T) {
		// B 的 Playground 目录不列该模型：只启用 Playground 时它不是候选，请求在 A 的 429 后按额度不足结束；默认通道下 B 的 Build 被拒
		// 同样只是换通道，不把本请求遇到的额度 429 盖成 Build 不可用的 503
		upstream := &coolingUpstream{playgroundErrors: map[string][]error{buildOffA: {coolingMinuteQuota(), coolingMinuteQuota()}}}
		accounts := []string{buildOffA, buildOffB, buildOffC}
		playground := map[string][]string{buildOffA: {buildOffShared}, buildOffC: {buildOffShared}}
		build := coolingCatalogs(accounts, buildOffShared)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, playground, build)
		allowQuotaCooldown(t, pool, buildOffA)
		markCoolingAuthRequired(t, pool, buildOffC)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		assertCooling429(t, result, playgroundUntil(t, pool, buildOffA), "没有其他可换的账号")
		if retryAfter, _ := strconv.Atoi(result.recorder.Header().Get("Retry-After")); retryAfter < 2 || retryAfter > 4 {
			t.Fatalf("Retry-After = %q，期望约 3 秒（这次分钟限额的冷却）", result.recorder.Header().Get("Retry-After"))
		}
		if want := []string{buildOffA + "/playground", buildOffA + "/build", buildOffB + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
		if progressLogged(service, coolingEndNote) {
			t.Fatal("不应按 Build 不可用结束")
		}
	})
	t.Run("Playground 先报码 4，换到下一个账号的 Build 被拒", func(t *testing.T) {
		upstream := &coolingUpstream{playgroundErrors: map[string][]error{buildOffA: {&aistudio.RPCError{
			Method: "GenerateContent", StatusCode: http.StatusGatewayTimeout, Code: 4, Message: "Deadline expired before operation could complete.",
		}}}}
		accounts := []string{buildOffA, buildOffB}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		until := coolPlayground(t, pool, buildOffB, buildOffShared, 2*time.Hour)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		assertCooling429(t, result, until, coolingAllCooling)
		if want := []string{buildOffA + "/playground", buildOffB + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
	})
	t.Run("全局暂停中单个支持账号的试探被拒", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := []string{buildOffA, buildOffB, buildOffC}
		catalogs := map[string][]string{buildOffA: {buildOffShared}}
		service, pool, now := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		until := coolPlayground(t, pool, buildOffA, buildOffShared, 2*time.Hour)
		pauseBuildGlobally(t, service, accounts...)
		*now = now.Add(buildPauseInterval + time.Minute)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		assertCooling429(t, result, until, coolingAllCooling)
		if want := []string{buildOffA + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v（试探一次）", result.calls, want)
		}
		if !progressLogged(service, "这次请求作为试探改走 Build") || !pool.BuildGenerationPaused() {
			t.Fatal("应作为试探改走 Build，被拒后仍全局暂停")
		}
	})
}

// coolingChannelConfigs 为对照用的两种上游通道：默认（Playground 在前、启用 Build）与只启用 Playground
var coolingChannelConfigs = []struct {
	name string
	list []aistudio.Channel
}{
	{"默认通道", coolingDefaultChannels},
	{"只启用 Playground", []aistudio.Channel{aistudio.ChannelPlayground}},
}

// coolingSwitchCooldowns 为同账号改走 Build 被拒时它的 Playground 冷却的两种长度：短于排队上限时账号留在排除名单里，更长时放回候选
var coolingSwitchCooldowns = []struct {
	name  string
	delay time.Duration
}{
	{"冷却 3 秒、留在排除名单里", 3 * time.Second},
	{"冷却 2 分钟、放回候选", 2 * time.Minute},
}

// coolingColdStandby 把账号改成没有已启动 Worker 的备用账号，返回冷启动次数（冷启动照常成功，用桩 Worker）
func coolingColdStandby(t *testing.T, service *trackedService, accountID string) *atomic.Int32 {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	account := service.workers.accounts[accountID]
	account.worker = nil
	account.warm.Store(false)
	var launches atomic.Int32
	service.workers.launch = func(_ context.Context, id string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		launches.Add(1)
		worker, _ := aistudio.NewStubWorker(id, 0)
		return worker, nil
	}
	return &launches
}

// TestBuildDisabledQuotaSwitchAsPlaygroundOnly 本请求在 Playground 上遇到额度 429、同账号改走 Build 又被官网停用：Build 被拒只是换通道，
// 之后与只启用 Playground 时那次额度 429 之后的处理相同（两种通道配置对照）：下一次选号只用已启动的 Worker、第 2 次起的限流类换号前
// 等待照常进行（等待中首事件前期限到期时按那次额度 429 结束）、没有其他账号时 Retry-After 取 5 秒与这次冷却中较早者
func TestBuildDisabledQuotaSwitchAsPlaygroundOnly(t *testing.T) {
	for _, channels := range coolingChannelConfigs {
		playgroundOnly := len(channels.list) == 1
		t.Run("热账号分钟限额 429、另有备用账号：不现场冷启动备用账号（"+channels.name+"）", func(t *testing.T) {
			// A 的 Worker 已启动，B 没有：限流类额度失败后的选号只用已启动的 Worker，只为 B 发起一次后台扩容，按额度不足立即返回 429；
			// 不因同账号换到 Build 时取得租约清掉了这个约束而在请求路径上现场冷启动 B
			upstream := &coolingUpstream{playgroundErrors: map[string][]error{buildOffA: {coolingMinuteQuota(), coolingMinuteQuota()}}}
			accounts := []string{buildOffA, buildOffB}
			catalogs := coolingCatalogs(accounts, buildOffShared)
			service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, channels.list, accounts, catalogs, catalogs)
			allowQuotaCooldown(t, pool, buildOffA)
			launches := coolingColdStandby(t, service, buildOffB)
			startedAt := time.Now()
			result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
			assertCooling429(t, result, startedAt.Add(primaryRetryFallback), primaryQuotaReasonNoWarm)
			want := []string{buildOffA + "/playground", buildOffA + "/build"}
			if playgroundOnly {
				want = want[:1]
			}
			if !slices.Equal(result.calls, want) {
				t.Fatalf("尝试 = %v，期望 %v（不在 B 上现场冷启动后再试）", result.calls, want)
			}
			// 后台扩容不等它：请求结束后只有这一次冷启动
			deadline := time.Now().Add(3 * time.Second)
			for launches.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(100 * time.Millisecond)
			if got := launches.Load(); got != 1 {
				t.Fatalf("冷启动 %d 次，期望只有一次后台扩容", got)
			}
		})
		for _, cooldown := range coolingSwitchCooldowns {
			t.Run("两个账号先后分钟限额 429：第 2 次换号前照常等待（"+cooldown.name+"，"+channels.name+"）", func(t *testing.T) {
				// A、B 的 Playground 先后分钟限额 429（同账号改走 Build 都被拒），C 的 Playground 成功：本请求第 2 次限流类失败后换号前
				// 等待一次，与只启用 Playground 时相同；同账号换通道不算换号，等待挪到 Build 被拒、真正换走时
				upstream := &coolingUpstream{playgroundErrors: map[string][]error{
					buildOffA: {coolingMinuteQuotaAfter(cooldown.delay), coolingMinuteQuotaAfter(cooldown.delay)},
					buildOffB: {coolingMinuteQuotaAfter(cooldown.delay), coolingMinuteQuotaAfter(cooldown.delay)},
				}}
				accounts := []string{buildOffA, buildOffB, buildOffC}
				catalogs := coolingCatalogs(accounts, buildOffShared)
				service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, channels.list, accounts, catalogs, catalogs)
				allowQuotaCooldown(t, pool, buildOffA)
				allowQuotaCooldown(t, pool, buildOffB)
				var jitters atomic.Int32
				service.quotaJitter = func() time.Duration {
					jitters.Add(1)
					return 0
				}
				result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
				if text := result.recorder.Body.String(); result.recorder.Code != http.StatusOK || !strings.Contains(text, "hello from playground") {
					t.Fatalf("状态 %d，响应 %s，期望换到 C 的 Playground 成功", result.recorder.Code, text)
				}
				want := []string{buildOffA + "/playground", buildOffA + "/build", buildOffB + "/playground", buildOffB + "/build", buildOffC + "/playground"}
				if playgroundOnly {
					want = []string{buildOffA + "/playground", buildOffB + "/playground", buildOffC + "/playground"}
				}
				if !slices.Equal(result.calls, want) {
					t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
				}
				if got := jitters.Load(); got != 1 {
					t.Fatalf("限流类换号前等待 %d 次，期望 1 次（第 2 次限流类失败之后）", got)
				}
			})
			t.Run("第 2 次换号前等待中首事件前期限到期：按那次额度 429 结束（"+cooldown.name+"，"+channels.name+"）", func(t *testing.T) {
				// 换号前等待（2 秒）中本服务首事件前期限（300ms）到期，请求按上一次的错误结束：对外照旧是那次额度 429，与只启用 Playground 时
				// 相同，不因最后一次尝试是 B 的 Build 被拒而换成“Playground 接不了”的 503
				const limit = 300 * time.Millisecond
				upstream := &coolingUpstream{playgroundErrors: map[string][]error{
					buildOffA: {coolingMinuteQuotaAfter(cooldown.delay), coolingMinuteQuotaAfter(cooldown.delay)},
					buildOffB: {coolingMinuteQuotaAfter(cooldown.delay), coolingMinuteQuotaAfter(cooldown.delay)},
				}}
				accounts := []string{buildOffA, buildOffB, buildOffC}
				catalogs := coolingCatalogs(accounts, buildOffShared)
				service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, channels.list, accounts, catalogs, catalogs)
				allowQuotaCooldown(t, pool, buildOffA)
				allowQuotaCooldown(t, pool, buildOffB)
				service.timeout.Store(int64(limit))
				service.quotaJitter = func() time.Duration { return 2 * time.Second }
				result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
				if text := result.recorder.Body.String(); result.recorder.Code != http.StatusTooManyRequests ||
					!strings.Contains(text, "Resource has been exhausted") || strings.Contains(text, "Build channel") {
					t.Fatalf("状态 %d，响应 %s，期望按那次额度 429 结束", result.recorder.Code, text)
				}
				if len(result.entries) != 1 || !strings.Contains(result.entries[0].Error, "停在 限流换号前等待") ||
					!strings.Contains(result.entries[0].Error, "Quota exceeded for quota metric") {
					t.Fatalf("访问日志 = %+v，期望停在限流换号前等待、原因为那次额度 429", result.entries)
				}
				want := []string{buildOffA + "/playground", buildOffA + "/build", buildOffB + "/playground", buildOffB + "/build"}
				if playgroundOnly {
					want = []string{buildOffA + "/playground", buildOffB + "/playground"}
				}
				if !slices.Equal(result.calls, want) {
					t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
				}
				if progressLogged(service, coolingEndNote) {
					t.Fatal("不应按 Build 不可用结束")
				}
			})
		}
		t.Run("分钟限额的冷却超过 5 秒：Retry-After 为 5 秒（"+channels.name+"）", func(t *testing.T) {
			// 没有其他可换的账号时 Retry-After 取 5 秒与本请求遇到的额度冷却中较早者（primaryQuotaRetryAt），不是这次 30 秒的冷却
			upstream := &coolingUpstream{playgroundErrors: map[string][]error{buildOffA: {coolingMinuteQuotaAfter(30 * time.Second)}}}
			accounts := []string{buildOffA}
			catalogs := coolingCatalogs(accounts, buildOffShared)
			service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, channels.list, accounts, catalogs, catalogs)
			allowQuotaCooldown(t, pool, buildOffA)
			startedAt := time.Now()
			result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
			assertCooling429(t, result, startedAt.Add(primaryRetryFallback), primaryQuotaReasonNoOther)
			if retryAfter, _ := strconv.Atoi(result.recorder.Header().Get("Retry-After")); retryAfter < 4 || retryAfter > 5 {
				t.Fatalf("Retry-After = %q，期望 5 秒", result.recorder.Header().Get("Retry-After"))
			}
			if remaining := time.Until(playgroundUntil(t, pool, buildOffA)); remaining < 20*time.Second {
				t.Fatalf("A 的冷却剩余 %s，期望约 30 秒", remaining)
			}
			want := []string{buildOffA + "/playground", buildOffA + "/build"}
			if playgroundOnly {
				want = want[:1]
			}
			if !slices.Equal(result.calls, want) {
				t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
			}
		})
	}
	// 额度 429 之后中间换到的账号返回 5xx，最后在只在 Build 目录列出该模型的账号上被拒，号池只剩需要重新登录的账号：默认通道按选号没有
	// 候选时的规则（本请求有过额度失败）以额度不足结束；只启用 Playground 时 C 不是候选，选号返回 AccountsNotReadyError，按 B 的 5xx 结束。
	// 两者不同是有意的：Build 被拒的原文对客户端没有意义，不追溯被拒前最后一次真正的失败
	for _, channels := range coolingChannelConfigs {
		t.Run("额度 429 后换到的账号 5xx、只在 Build 目录列出该模型的账号被拒、另一账号需要重新登录（"+channels.name+"）", func(t *testing.T) {
			upstream := &coolingUpstream{playgroundErrors: map[string][]error{
				buildOffA: {coolingMinuteQuotaAfter(30 * time.Second)},
				buildOffB: {&aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusInternalServerError, Code: 13, Message: "Internal error encountered."}},
			}}
			accounts := []string{buildOffA, buildOffB, buildOffC, buildOffD}
			playground := map[string][]string{buildOffA: {buildOffShared}, buildOffB: {buildOffShared}, buildOffD: {buildOffShared}}
			build := coolingCatalogs(accounts, buildOffShared)
			service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, channels.list, accounts, playground, build)
			allowQuotaCooldown(t, pool, buildOffA)
			markCoolingAuthRequired(t, pool, buildOffD)
			result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
			if progressLogged(service, coolingEndNote) {
				t.Fatal("不应按 Build 不可用结束")
			}
			if playgroundOnly := len(channels.list) == 1; playgroundOnly {
				if text := result.recorder.Body.String(); result.recorder.Code == http.StatusTooManyRequests ||
					result.recorder.Code == http.StatusServiceUnavailable || len(result.entries) != 1 ||
					!strings.Contains(result.entries[0].Error, "Internal error encountered") {
					t.Fatalf("状态 %d，响应 %s，访问日志 %+v，期望按 B 的 5xx 结束", result.recorder.Code, text, result.entries)
				}
				if want := []string{buildOffA + "/playground", buildOffB + "/playground"}; !slices.Equal(result.calls, want) {
					t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
				}
				return
			}
			assertCooling429(t, result, time.Now().Add(primaryRetryFallback), primaryQuotaReasonNoOther)
			want := []string{buildOffA + "/playground", buildOffA + "/build", buildOffB + "/playground", buildOffC + "/build"}
			if !slices.Equal(result.calls, want) {
				t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
			}
		})
	}
}

// ceilingQuotaAccounts 为硬上限测试的号池：total 个账号按 ID 顺序，前 6 个的 Playground 返回额度 429（4 个每日限额、2 个冷却 30 秒的
// 分钟限额，都没到各自的换号上限，不终止请求），第 7 个到第 generateSwitchCeiling 个只在 Build 目录列出该模型（ID 前缀 prefix）
func ceilingQuotaAccounts(prefix string, total int) (accounts []string, playground map[string][]string, build map[string][]string, errs map[string][]error) {
	accounts = make([]string, total)
	playground, build, errs = map[string][]string{}, map[string][]string{}, map[string][]error{}
	for index := range accounts {
		accountID := fmt.Sprintf("%s-%d@example.com", prefix, index+1)
		accounts[index] = accountID
		switch {
		case index < normalQuotaSwitchLimit:
			playground[accountID] = []string{buildOffShared}
			errs[accountID] = []error{buildOffQuotaError()}
		case index < normalQuotaSwitchLimit+normalRateLimitSwitchLimit:
			playground[accountID] = []string{buildOffShared}
			errs[accountID] = []error{coolingMinuteQuotaAfter(30 * time.Second)}
		case index < generateSwitchCeiling:
			build[accountID] = []string{buildOffShared}
		default:
			playground[accountID] = []string{buildOffShared}
		}
	}
	return accounts, playground, build, errs
}

// TestBuildDisabledSwitchCeiling 每请求换号硬上限（generateSwitchCeiling）遇上 Build 被官网停用：本请求第一次用到、只在 Build 上被拒的
// 账号不占硬上限，与只启用 Playground 时（它们不是候选）一样，号池里还有能经 Playground 接的账号时换到它完成，没有时按额度不足返回 429
// （两种通道配置对照，原先默认通道在第 8 个不同账号的 Build 被拒时到硬上限，以“Playground 接不了”的 503 结束）；本请求之前已试过的
// 账号在 Build 上被拒时正好到硬上限（Worker 重建后的重放），按额度不足结束，不以 503 结束
func TestBuildDisabledSwitchCeiling(t *testing.T) {
	quotaAccounts := normalQuotaSwitchLimit + normalRateLimitSwitchLimit
	for _, extra := range []bool{false, true} {
		for _, channels := range coolingChannelConfigs {
			playgroundOnly := len(channels.list) == 1
			name := fmt.Sprintf("%d 个额度 429、%d 个只在 Build 目录列出该模型的账号被拒", quotaAccounts, generateSwitchCeiling-quotaAccounts)
			if extra {
				name += "，另有一个 Playground 可用的账号"
			}
			t.Run(name+"（"+channels.name+"）", func(t *testing.T) {
				total := generateSwitchCeiling
				if extra {
					total++
				}
				accounts, playground, build, errs := ceilingQuotaAccounts("ceiling", total)
				upstream := &coolingUpstream{playgroundErrors: errs}
				service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, channels.list, accounts, playground, build)
				for _, accountID := range accounts[:quotaAccounts] {
					allowQuotaCooldown(t, pool, accountID)
				}
				service.quotaJitter = func() time.Duration { return 0 }
				startedAt := time.Now()
				result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
				want := make([]string, 0, total+1)
				for _, accountID := range accounts[:quotaAccounts] {
					want = append(want, accountID+"/playground")
				}
				if !playgroundOnly {
					for _, accountID := range accounts[quotaAccounts:generateSwitchCeiling] {
						want = append(want, accountID+"/build")
					}
				}
				if extra {
					want = append(want, accounts[generateSwitchCeiling]+"/playground")
				}
				if !slices.Equal(result.calls, want) {
					t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
				}
				if progressLogged(service, "不再换号") || progressLogged(service, coolingEndNote) {
					t.Fatal("只在 Build 上被拒的账号不应占每请求换号硬上限，也不应按 Build 不可用结束")
				}
				if extra {
					if text := result.recorder.Body.String(); result.recorder.Code != http.StatusOK || !strings.Contains(text, "hello from playground") {
						t.Fatalf("状态 %d，响应 %s，期望换到最后一个账号的 Playground 成功", result.recorder.Code, text)
					}
					return
				}
				// 没有其他可换的账号：Retry-After 取 5 秒与本请求遇到的额度冷却中较早者；尝试账号不算只在 Build 上被拒的账号，与只启用 Playground 时相同
				assertCooling429(t, result, startedAt.Add(primaryRetryFallback), primaryQuotaReasonNoOther)
				if accountsNote := fmt.Sprintf("尝试账号 %d 个", quotaAccounts); !strings.Contains(result.entries[0].Error, accountsNote) {
					t.Fatalf("访问日志错误 = %s，期望含 %q", result.entries[0].Error, accountsNote)
				}
			})
		}
	}
	// 第 7 个账号的 Playground 返回 5xx（换号、计一次连续失败），第 8 个账号（只在 Build 目录列出该模型）的 Worker 已被替换、重建后在原账号重放，
	// 重放在它的 Build 上被拒：它已算过一次，正好到硬上限。默认通道按硬上限的额度不足结束（429，Retry-After 5 秒）；只启用 Playground 时
	// 第 8 个账号不是候选，选号没有候选，按没有其他可换的账号结束。两者都不是“Playground 接不了”的 503
	for _, channels := range coolingChannelConfigs {
		t.Run("之前试过的账号重放时在 Build 上被拒、正好到硬上限：按额度不足结束（"+channels.name+"）", func(t *testing.T) {
			accounts, playground, build, errs := ceilingQuotaAccounts("replay", generateSwitchCeiling)
			fivexx, replaced := accounts[quotaAccounts], accounts[generateSwitchCeiling-1]
			playground[fivexx], build[fivexx] = []string{buildOffShared}, nil
			errs[fivexx] = []error{&aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusInternalServerError, Code: 13, Message: "Internal error encountered."}}
			upstream := &coolingUpstream{playgroundErrors: errs, buildErrors: map[string][]error{replaced: {errAccountWorkerReplaced}}}
			service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, channels.list, accounts, playground, build)
			for _, accountID := range accounts[:quotaAccounts] {
				allowQuotaCooldown(t, pool, accountID)
			}
			service.quotaJitter = func() time.Duration { return 0 }
			startedAt := time.Now()
			result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
			want := make([]string, 0, generateSwitchCeiling+1)
			for _, accountID := range accounts[:quotaAccounts+1] {
				want = append(want, accountID+"/playground")
			}
			if progressLogged(service, coolingEndNote) {
				t.Fatal("不应按 Build 不可用结束")
			}
			if len(channels.list) == 1 {
				assertCooling429(t, result, startedAt.Add(primaryRetryFallback), primaryQuotaReasonNoOther)
				if !slices.Equal(result.calls, want) {
					t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
				}
				return
			}
			want = append(want, replaced+"/build", replaced+"/build")
			if !slices.Equal(result.calls, want) {
				t.Fatalf("尝试 = %v，期望 %v（第 8 个账号重建后重放）", result.calls, want)
			}
			if !progressLogged(service, "WAA Worker 重建，重放当前请求") ||
				!progressLogged(service, fmt.Sprintf("已尝试 %d 个账号，不再换号", generateSwitchCeiling)) {
				t.Fatal("应在第 8 个账号重建后重放，重放被拒后到每请求换号硬上限")
			}
			assertCooling429(t, result, time.Now().Add(primaryRetryFallback), primaryQuotaReasonCeiling)
			// 原因取本请求最后一次额度失败（分钟限额），不是 Build 的拒绝
			if entry := result.entries[0].Error; !strings.Contains(entry, "Quota exceeded for quota metric") || strings.Contains(entry, "PROXY_GENERATION_DISABLED") {
				t.Fatalf("访问日志错误 = %s，期望原因为那次分钟限额 429", entry)
			}
		})
	}
}

// TestBuildDisabledUltraFallback Ultra 号池的 gemini-4-argon（Ultra 主用模型）在 Playground 上冷却、Build 被官网停用：
// 按 MODEL_FALLBACKS 改用 gemini-4-carbon 成功；没有改用规则时客户端收到 429 且带 Retry-After，不是 503
func TestBuildDisabledUltraFallback(t *testing.T) {
	setup := func(t *testing.T, upstream *coolingUpstream, accounts []string) (*trackedService, time.Time) {
		t.Helper()
		catalogs := coolingCatalogs(accounts, fallbackSource, fallbackTarget)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierUltra, coolingDefaultChannels, accounts, catalogs, catalogs)
		var until time.Time
		for index, accountID := range accounts {
			cooled := coolPlayground(t, pool, accountID, fallbackSource, time.Duration(index+2)*time.Hour)
			if until.IsZero() {
				until = cooled
			}
		}
		return service, until
	}
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d 个账号：改用 carbon", count), func(t *testing.T) {
			upstream := &coolingUpstream{}
			accounts := ultraTestIDs(count)
			service, _ := setup(t, upstream, accounts)
			result := postCooling(t, service, upstream, "/ultra/v1/chat/completions", fallbackSource, false)
			text := result.recorder.Body.String()
			if result.recorder.Code != http.StatusOK || !strings.Contains(text, "hello from playground") ||
				!strings.Contains(text, `"model":"`+fallbackTarget+`"`) {
				t.Fatalf("状态 %d，响应 %s，期望改用 carbon 经 Playground 成功", result.recorder.Code, text)
			}
			var want []string
			for _, accountID := range accounts {
				want = append(want, accountID+"/build")
			}
			want = append(want, accounts[0]+"/playground")
			wantModels := append(slices.Repeat([]string{fallbackSource}, count), fallbackTarget)
			if !slices.Equal(result.calls, want) || !slices.Equal(result.models, wantModels) {
				t.Fatalf("尝试 = %v 模型 = %v，期望 %v 与 %v", result.calls, result.models, want, wantModels)
			}
			if progressCountAll(service, "模型 "+fallbackSource+" 额度用完（候选全部冷却", "按 MODEL_FALLBACKS 改用 "+fallbackTarget) != 1 {
				t.Fatal("请求日志应写一行候选全部冷却后改用 carbon")
			}
			if progressLogged(service, coolingEndNote) {
				t.Fatal("不应按 Build 不可用结束")
			}
		})
	}
	t.Run("当场分钟限额 429 后同账号 Build 被拒：不等短冷却，改用 carbon", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := ultraTestIDs(1)
		upstream.playgroundErrors = map[string][]error{accounts[0]: {coolingMinuteQuota()}}
		catalogs := coolingCatalogs(accounts, fallbackSource, fallbackTarget)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierUltra, coolingDefaultChannels, accounts, catalogs, catalogs)
		allowModelQuotaCooldown(t, pool, accounts[0], fallbackSource, testRuntimeModel, fallbackTarget)
		startedAt := time.Now()
		result := postCooling(t, service, upstream, "/ultra/v1/chat/completions", fallbackSource, false)
		text := result.recorder.Body.String()
		if result.recorder.Code != http.StatusOK || !strings.Contains(text, "hello from playground") ||
			!strings.Contains(text, `"model":"`+fallbackTarget+`"`) {
			t.Fatalf("状态 %d，响应 %s，期望改用 carbon 经 Playground 成功", result.recorder.Code, text)
		}
		want := []string{accounts[0] + "/playground", accounts[0] + "/build", accounts[0] + "/playground"}
		wantModels := []string{fallbackSource, fallbackSource, fallbackTarget}
		if !slices.Equal(result.calls, want) || !slices.Equal(result.models, wantModels) {
			t.Fatalf("尝试 = %v 模型 = %v，期望 %v 与 %v", result.calls, result.models, want, wantModels)
		}
		if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
			t.Fatalf("用时 %s，不应等 argon 的短冷却", elapsed)
		}
		if !progressLogged(service, coolingRecentQuotaNote) || progressCountAll(service, "按 MODEL_FALLBACKS 改用 "+fallbackTarget) != 1 {
			t.Fatal("进度应写明不在同账号上等短冷却，并按 MODEL_FALLBACKS 改用 carbon")
		}
	})
	for _, channels := range []struct {
		name string
		list []aistudio.Channel
	}{
		{"默认通道", coolingDefaultChannels},
		{"只启用 Playground", []aistudio.Channel{aistudio.ChannelPlayground}},
	} {
		t.Run("当场分钟限额 429 后同账号 Build 被拒、另一账号需要重新登录：改用 carbon（"+channels.name+"）", func(t *testing.T) {
			// 2 个 Ultra 账号里 1 个 Cookie 失效：排除 argon 遇到分钟限额的账号后选号返回 AccountsNotReadyError，按额度不足结束并改用 carbon
			upstream := &coolingUpstream{}
			accounts := ultraTestIDs(2)
			upstream.playgroundErrors = map[string][]error{accounts[0]: {coolingMinuteQuota()}}
			catalogs := coolingCatalogs(accounts, fallbackSource, fallbackTarget)
			service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierUltra, channels.list, accounts, catalogs, catalogs)
			allowModelQuotaCooldown(t, pool, accounts[0], fallbackSource, testRuntimeModel, fallbackTarget)
			markCoolingAuthRequired(t, pool, accounts[1])
			result := postCooling(t, service, upstream, "/ultra/v1/chat/completions", fallbackSource, false)
			text := result.recorder.Body.String()
			if result.recorder.Code != http.StatusOK || !strings.Contains(text, "hello from playground") ||
				!strings.Contains(text, `"model":"`+fallbackTarget+`"`) {
				t.Fatalf("状态 %d，响应 %s，期望改用 carbon 经 Playground 成功", result.recorder.Code, text)
			}
			want := []string{accounts[0] + "/playground", accounts[0] + "/build", accounts[0] + "/playground"}
			wantModels := []string{fallbackSource, fallbackSource, fallbackTarget}
			if len(channels.list) == 1 {
				want = []string{accounts[0] + "/playground", accounts[0] + "/playground"}
				wantModels = []string{fallbackSource, fallbackTarget}
			}
			if !slices.Equal(result.calls, want) || !slices.Equal(result.models, wantModels) {
				t.Fatalf("尝试 = %v 模型 = %v，期望 %v 与 %v", result.calls, result.models, want, wantModels)
			}
			if progressCountAll(service, "没有其他可换的账号", "按 MODEL_FALLBACKS 改用 "+fallbackTarget) != 1 || progressLogged(service, coolingEndNote) {
				t.Fatal("进度应写明没有其他可换的账号并按 MODEL_FALLBACKS 改用 carbon，不应按 Build 不可用结束")
			}
		})
	}
	t.Run("没有改用规则：429 带 Retry-After", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := ultraTestIDs(2)
		service, until := setup(t, upstream, accounts)
		service.setModelFallbacks("", config.ModelFallbackScopeOff)
		result := postCooling(t, service, upstream, "/ultra/v1/chat/completions", fallbackSource, false)
		assertCooling429(t, result, until, coolingAllCooling)
		if want := []string{accounts[0] + "/build", accounts[1] + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
		if !progressLogged(service, "Ultra 主用模型候选全部冷却") {
			t.Fatal("进度应写明 Ultra 主用模型候选全部冷却")
		}
	})
}

// TestBuildDisabledPinnedAccountCooling 指定账户的请求：账户的 Playground 冷却、Build 被官网停用时按全部冷却结束（429，带恢复时间），
// 不按 Build 不可用的 503；账户的 Playground 目录没有该模型时仍是 Playground 接不了的 503
func TestBuildDisabledPinnedAccountCooling(t *testing.T) {
	run := func(t *testing.T, service *trackedService, upstream *coolingUpstream) (error, []string) {
		t.Helper()
		request := aistudio.GenerateRequest{
			ID: "req-pinned", Model: buildOffShared, AccountID: buildOffA,
			Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
		}
		_, err, calls, _ := runBuildOffRequestWith(t, service, &upstream.buildOffUpstream, request)
		return err, calls
	}
	t.Run("Playground 冷却", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := []string{buildOffA, buildOffB}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		until := coolPlayground(t, pool, buildOffA, buildOffShared, time.Hour)
		err, calls := run(t, service, upstream)
		var cooling *aistudio.AllCoolingError
		if !errors.As(err, &cooling) || !cooling.Until.Equal(until) {
			t.Fatalf("err = %v，期望全部冷却（恢复时间 %s）", err, until)
		}
		if want := []string{buildOffA + "/build"}; !slices.Equal(calls, want) {
			t.Fatalf("尝试 = %v，期望 %v（只在指定账户上）", calls, want)
		}
	})
	t.Run("本请求 Playground 分钟限额 429 后同账号 Build 被拒：按那次 429 结束", func(t *testing.T) {
		// 与只启用 Playground 时相同：指定账户的请求在它的 Playground 上遇到限流类 429 即结束，不等它的短冷却恢复后再试
		upstream := &coolingUpstream{playgroundErrors: map[string][]error{buildOffA: {coolingMinuteQuota(), coolingMinuteQuota()}}}
		accounts := []string{buildOffA, buildOffB}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, catalogs, catalogs)
		allowQuotaCooldown(t, pool, buildOffA)
		startedAt := time.Now()
		err, calls := run(t, service, upstream)
		var rpcErr *aistudio.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.StatusCode != http.StatusTooManyRequests || rpcErr.RetryDelay != 3*time.Second {
			t.Fatalf("err = %v，期望 Playground 那次分钟限额 429", err)
		}
		if aistudio.BuildGenerationDisabledFailure(err) {
			t.Fatalf("err = %v，不应带 Build 的 PROXY_GENERATION_DISABLED", err)
		}
		if want := []string{buildOffA + "/playground", buildOffA + "/build"}; !slices.Equal(calls, want) {
			t.Fatalf("尝试 = %v，期望 %v（不在同账号的 Playground 上等冷却结束再试）", calls, want)
		}
		if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
			t.Fatalf("用时 %s，不应等指定账户的短冷却", elapsed)
		}
		if !progressLogged(service, "指定账户的 Playground 本请求刚遇到额度 429") {
			t.Fatal("进度应写明按本请求那次额度 429 结束")
		}
	})
	t.Run("Ultra 主用模型：本请求 Playground 分钟限额 429 后同账号 Build 被拒，按额度不足结束", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := ultraTestIDs(2)
		upstream.playgroundErrors = map[string][]error{accounts[0]: {coolingMinuteQuota(), coolingMinuteQuota()}}
		catalogs := coolingCatalogs(accounts, fallbackSource, fallbackTarget)
		service, pool, _ := coolingService(t, upstream, aistudio.BenefitTierUltra, coolingDefaultChannels, accounts, catalogs, catalogs)
		allowModelQuotaCooldown(t, pool, accounts[0], fallbackSource, testRuntimeModel, fallbackTarget)
		startedAt := time.Now()
		result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: fallbackSource, AccountID: accounts[0]})
		var quotaErr *aistudio.PrimaryQuotaError
		if !errors.As(result.err, &quotaErr) || quotaErr.Kind != aistudio.QuotaFailUltraPrimary || quotaErr.Reason != primaryQuotaReasonNoOther {
			t.Fatalf("err = %v，期望 Ultra 主用模型额度不足（没有其他可换的账号）", result.err)
		}
		if retryAt := time.Until(quotaErr.Until); retryAt <= 0 || retryAt > 4*time.Second {
			t.Fatalf("建议重试 = %s 后，期望约 3 秒（这次 3 秒的冷却，早于 5 秒）", retryAt)
		}
		if want := []string{accounts[0] + "/playground", accounts[0] + "/build"}; !slices.Equal(upstream.called(), want) {
			t.Fatalf("尝试 = %v，期望 %v（只在指定账户上，不改用）", upstream.called(), want)
		}
		if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
			t.Fatalf("用时 %s，不应等指定账户的短冷却", elapsed)
		}
		if !progressLogged(service, "指定账户的 Playground 本请求刚遇到额度 429") {
			t.Fatal("进度应写明按本请求那次额度 429 结束")
		}
	})
	t.Run("Playground 目录没有该模型", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := []string{buildOffA, buildOffB}
		playground := map[string][]string{buildOffB: {buildOffShared}}
		build := coolingCatalogs(accounts, buildOffShared)
		service, _, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, playground, build)
		err, calls := run(t, service, upstream)
		var unavailable *aistudio.BuildGenerationUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Reason != aistudio.BuildUnavailableNoPlayground {
			t.Fatalf("err = %v，期望 Playground 接不了的 503", err)
		}
		if want := []string{buildOffA + "/build"}; !slices.Equal(calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", calls, want)
		}
	})
}

// runBuildOffRequestWith 与 runBuildOffRequest 相同，按给定的请求执行
func runBuildOffRequestWith(t *testing.T, service *trackedService, upstream *buildOffUpstream, request aistudio.GenerateRequest) (string, error, []string, time.Duration) {
	t.Helper()
	before := len(upstream.called())
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
	return text, err, upstream.called()[before:], time.Since(startedAt)
}

// TestBuildDisabledTrulyUnavailable 真正接不了的情形仍按原来的 503 结束：Playground 目录不列该模型（只能经 Build 调用），
// UPSTREAM_CHANNELS 只有 build（Playground 接不了）；每个账号的 Build 只试一次
func TestBuildDisabledTrulyUnavailable(t *testing.T) {
	t.Run("Playground 目录不列该模型", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := []string{buildOffA, buildOffB}
		build := coolingCatalogs(accounts, buildOffOnly)
		service, _, _ := coolingService(t, upstream, aistudio.BenefitTierFree, coolingDefaultChannels, accounts, nil, build)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffOnly, false)
		assertBuild503(t, result, coolingBuildOnly)
		if want := []string{buildOffA + "/build", buildOffB + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
		if !progressLogged(service, coolingEndNote) {
			t.Fatal("进度应写明没有其他通道能接这个请求")
		}
	})
	t.Run("只启用 Build", func(t *testing.T) {
		upstream := &coolingUpstream{}
		accounts := []string{buildOffA, buildOffB}
		catalogs := coolingCatalogs(accounts, buildOffShared)
		service, _, _ := coolingService(t, upstream, aistudio.BenefitTierFree, []aistudio.Channel{aistudio.ChannelBuild}, accounts, catalogs, catalogs)
		result := postCooling(t, service, upstream, "/v1/chat/completions", buildOffShared, false)
		assertBuild503(t, result, coolingNoPlayground)
		if want := []string{buildOffA + "/build", buildOffB + "/build"}; !slices.Equal(result.calls, want) {
			t.Fatalf("尝试 = %v，期望 %v", result.calls, want)
		}
		if !progressLogged(service, coolingEndNote) {
			t.Fatal("进度应写明没有其他通道能接这个请求")
		}
	})
}
