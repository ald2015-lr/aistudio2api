package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 额度用完自动改用其他模型（MODEL_FALLBACKS、MODEL_FALLBACK_SCOPE，见 model_fallback.go）的测试

const (
	fallbackSource = "gemini-4-argon"
	fallbackTarget = "gemini-4-carbon"
	// fallbackPlain 为只会对话、不支持函数声明与思考的模型；fallbackBudget 为只支持思考预算的模型；fallbackOmni 为经 Interaction 通道
	// 生成的 Omni 模型（只在个别测试的目录里）
	fallbackPlain  = "gemini-plain-chat"
	fallbackBudget = "gemini-budget-chat"
	fallbackOmni   = "gemini-omni-chat"
)

// fallbackCatalog 为测试账户的模型目录：argon、carbon 支持思考等级、函数声明与 Google 搜索
func fallbackCatalog() []aistudio.Model {
	full := func(id string) aistudio.Model {
		return aistudio.Model{
			ID: id, Name: id, Methods: []string{"generateContent", "countTokens"},
			Capabilities: map[string]bool{
				"chat_model": true, "thinking": true, "thinking_level": true, "function_declarations": true, "google_search": true,
			},
		}
	}
	source, target := full(fallbackSource), full(fallbackTarget)
	source.CapabilityOptions = map[string][]string{"aliases": {"gemini-argon-latest"}}
	target.CapabilityOptions = map[string][]string{"aliases": {"gemini-carbon-latest"}}
	return []aistudio.Model{
		source, target,
		{ID: fallbackPlain, Name: fallbackPlain, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{
			ID: fallbackBudget, Name: fallbackBudget, Methods: []string{"generateContent"},
			Capabilities: map[string]bool{"chat_model": true, "thinking": true, "thinking_budget": true, "function_declarations": true},
		},
	}
}

// fallbackCall 为上游收到的一次生成请求
type fallbackCall struct {
	model   string
	account string
	request aistudio.GenerateRequest
}

// fallbackUpstream 为改用测试的上游：fail 按模型给出这次调用的错误（返回 nil 时正常回复），midStream 为在首个事件之后才返回错误的模型；
// 正常回复为一段写明模型的正文，replies 按模型给出另外的回复事件（结束事件之前）；encode 为发送前按 Build 通道编码请求（与真实上游
// 相同，编码失败时按那次的错误结束）；block 为调用时一直阻塞到那次的 ctx 结束、按 ctx 的错误返回的模型，blocked 在阻塞开始时收到模型名
type fallbackUpstream struct {
	mu        sync.Mutex
	calls     []fallbackCall
	fail      map[string]func(accountID string) error
	midStream map[string]error
	replies   map[string][]aistudio.Event
	encode    bool
	block     map[string]bool
	blocked   chan string
}

func (upstream *fallbackUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *fallbackUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *fallbackUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, fallbackCall{model: request.Model, account: request.AccountID, request: request})
	fail, midStream, replies := upstream.fail[request.Model], upstream.midStream[request.Model], upstream.replies[request.Model]
	block := upstream.block[request.Model]
	upstream.mu.Unlock()
	if block {
		if upstream.blocked != nil {
			upstream.blocked <- request.Model
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if fail != nil {
		if err := fail(request.AccountID); err != nil {
			return nil, err
		}
	}
	if upstream.encode {
		if _, _, err := aistudio.EncodeBuildGenerateRequest(request, aistudio.GenerationDefaults{}, false, false); err != nil {
			return nil, err
		}
	}
	events := make(chan aistudio.Event, 3+len(replies))
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "reply from " + request.Model}
	for _, reply := range replies {
		events <- reply
	}
	if midStream != nil {
		events <- aistudio.Event{Kind: aistudio.EventError, Err: midStream}
	} else {
		events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}
	}
	close(events)
	return events, nil
}

// models 返回上游按顺序收到的生成请求的模型
func (upstream *fallbackUpstream) models() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	models := make([]string, 0, len(upstream.calls))
	for _, call := range upstream.calls {
		models = append(models, call.model)
	}
	return models
}

// lastCall 返回上游收到的最后一个指定模型的请求
func (upstream *fallbackUpstream) lastCall(t *testing.T, model string) aistudio.GenerateRequest {
	t.Helper()
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	for index := len(upstream.calls) - 1; index >= 0; index-- {
		if upstream.calls[index].model == model {
			return upstream.calls[index].request
		}
	}
	t.Fatalf("上游没有收到 %s 的请求，收到 %v", model, upstream.calls)
	return aistudio.GenerateRequest{}
}

// fallbackService 创建运行中的生成服务：账户按 tier 设置权益与 fallbackCatalog，Ultra 主用模型为默认的 gemini-4-argon，
// 改用规则为默认值、范围为 scope
func fallbackService(t *testing.T, upstream aistudio.Service, tier aistudio.BenefitTier, scope string, accounts ...string) (*trackedService, *aistudio.AccountPool) {
	t.Helper()
	service, pool := runningGenerateService(t, upstream, context.Background(), time.Minute, accounts...)
	for _, accountID := range accounts {
		if err := pool.SetCatalog(accountID, tier, fallbackCatalog()); err != nil {
			t.Fatal(err)
		}
	}
	service.replaceModelSnapshot(fallbackCatalog())
	setUltraPrimaryModels(service, []string{fallbackSource}, 10*time.Second)
	service.setModelFallbacks(config.DefaultModelFallbacks, scope)
	return service, pool
}

// coolAll 让模型在全部账户上冷却 d
func coolAll(t *testing.T, pool *aistudio.AccountPool, model string, d time.Duration, accounts ...string) {
	t.Helper()
	for _, accountID := range accounts {
		coolModelFor(t, pool, accountID, model, d)
	}
}

// progressCountAll 返回请求进度里包含全部片段的行数
func progressCountAll(service *trackedService, fragments ...string) int {
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	count := 0
	for _, entry := range service.requests.logs {
		if entry.Event != "request.progress" {
			continue
		}
		matched := true
		for _, fragment := range fragments {
			matched = matched && strings.Contains(entry.Message, fragment)
		}
		if matched {
			count++
		}
	}
	return count
}

// fallbackResult 为一次经 Generate（startGenerate）发起的请求读完事件流的结果
type fallbackResult struct {
	text   string
	err    error
	served string
}

// runFallbackRequest 以号池 scope 经 Generate 发起请求并读完事件流；返回正文、错误与改用记录报告的模型
func runFallbackRequest(t *testing.T, service *trackedService, scope aistudio.PoolScope, request aistudio.GenerateRequest) fallbackResult {
	t.Helper()
	record := aistudio.NewModelSwitch()
	ctx, cancel := context.WithTimeout(aistudio.ContextWithModelSwitch(aistudio.ContextWithPoolScope(service.lifecycle, scope), record), 20*time.Second)
	defer cancel()
	if request.ID == "" {
		request.ID = "req-fallback"
	}
	if len(request.Contents) == 0 {
		request.Contents = []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}}
	}
	events, err := service.Generate(ctx, request)
	if err != nil {
		return fallbackResult{err: err, served: record.Model(request.Model)}
	}
	var result fallbackResult
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			result.text += event.Text
		case aistudio.EventError:
			result.err = event.Err
		}
	}
	result.served = record.Model(request.Model)
	if count := service.requests.count(); count != 0 {
		t.Fatalf("请求结束后仍有 %d 个活动请求", count)
	}
	return result
}

// postFallback 经公开 API 处理链发出请求（path 带 /ultra 前缀时走 Ultra 号池），返回响应与访问日志
func postFallback(t *testing.T, service *trackedService, path string, body string) (*httptest.ResponseRecorder, []api.AccessLog) {
	t.Helper()
	admin := &accessLogCapture{}
	return postFallbackTo(t, api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin}), admin, path, body)
}

// postFallbackTo 与 postFallback 相同，经同一个处理链（多轮请求共用协议层的思考签名登记）发出；返回响应与 admin 记下的全部访问日志
func postFallbackTo(t *testing.T, handler http.Handler, admin *accessLogCapture, path string, body string) (*httptest.ResponseRecorder, []api.AccessLog) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("X-Goog-Api-Key", "sk-test")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	admin.mu.Lock()
	defer admin.mu.Unlock()
	return recorder, append([]api.AccessLog(nil), admin.entries...)
}

// TestModelFallbackUltraAllCooling /ultra 上 argon 在全部账户上冷却（Ultra 主用模型立即 429）：流式与非流式都改用 carbon 成功，
// 响应的模型名为 carbon，上游只收到 carbon 的请求；请求日志写一行“额度用完，按 MODEL_FALLBACKS 改用”，访问日志与用量账本记实际模型，
// 请求日志另记 fallback_from，账本的上游尝试里有改用备注
func TestModelFallbackUltraAllCooling(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			accounts := ultraTestIDs(2)
			upstream := &fallbackUpstream{}
			service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
			coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
			body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, fallbackSource, stream)
			recorder, entries := postFallback(t, service, "/ultra/v1/chat/completions", body)
			text := recorder.Body.String()
			if recorder.Code != http.StatusOK || !strings.Contains(text, "reply from "+fallbackTarget) {
				t.Fatalf("状态 %d，响应 %s，期望 200 与 carbon 的回复", recorder.Code, text)
			}
			if !strings.Contains(text, `"model":"`+fallbackTarget+`"`) || strings.Contains(text, `"model":"`+fallbackSource+`"`) {
				t.Fatalf("响应的模型名应为 %s：%s", fallbackTarget, text)
			}
			if models := upstream.models(); !slices.Equal(models, []string{fallbackTarget}) {
				t.Fatalf("上游收到 %v，期望只有 carbon（argon 全部冷却，不向上游发送）", models)
			}
			if count := progressCountAll(service, "模型 "+fallbackSource+" 额度用完（候选全部冷却，最早恢复 ", "按 MODEL_FALLBACKS 改用 "+fallbackTarget); count != 1 {
				t.Fatalf("改用的日志应恰好一行，实际 %d 行", count)
			}
			// 选号日志不写“立即返回 429”（这次请求不返回 429），写交给 MODEL_FALLBACKS
			if progressLogged(service, "立即返回 429") || !progressLogged(service, "Ultra 主用模型候选全部冷却，结束这个模型的选号，交给 MODEL_FALLBACKS 改用其他模型") {
				t.Fatal("改用时选号日志不应写“立即返回 429”")
			}
			if len(entries) != 1 {
				t.Fatalf("访问日志 %d 条，期望 1", len(entries))
			}
			entry := entries[0]
			if entry.Model != fallbackTarget || entry.FallbackFrom != fallbackSource || entry.Error != "" {
				t.Fatalf("访问日志 model=%q fallback_from=%q error=%q，期望记实际模型与改用前的模型", entry.Model, entry.FallbackFrom, entry.Error)
			}
			if data := requestLogData(entry); data.Model != fallbackTarget || data.FallbackFrom != fallbackSource {
				t.Fatalf("请求日志 model=%q fallback_from=%q", data.Model, data.FallbackFrom)
			}
			row := requestRow(entry, time.Now())
			if row.Model != fallbackTarget || row.State != "completed" {
				t.Fatalf("用量账本 model=%q state=%q，期望 %s 成功", row.Model, row.State, fallbackTarget)
			}
			if !slices.ContainsFunc(row.Attempts, func(attempt api.RequestAttempt) bool {
				return strings.Contains(attempt.Error, "按 MODEL_FALLBACKS 改用 "+fallbackTarget)
			}) {
				t.Fatalf("用量账本的上游尝试里应有改用备注：%+v", row.Attempts)
			}
			assertNoBusyAccounts(t, pool)
		})
	}
}

// TestModelFallbackPrimaryQuota argon 在每个账户上都返回每日限额 429（Ultra 主用模型额度不足，PrimaryQuotaError）：改用 carbon 成功，
// 改用记录报告 carbon；日志的原因写明额度不足与最早恢复时间
func TestModelFallbackPrimaryQuota(t *testing.T) {
	accounts := ultraTestIDs(4)
	upstream := &fallbackUpstream{fail: map[string]func(string) error{
		fallbackSource: func(string) error { return ultraDailyQuotaError() },
	}}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: fallbackSource})
	if result.err != nil || result.text != "reply from "+fallbackTarget || result.served != fallbackTarget {
		t.Fatalf("text=%q err=%v served=%q，期望改用 carbon 成功", result.text, result.err, result.served)
	}
	models := upstream.models()
	if len(models) != primaryQuotaSwitchLimit+2 || models[len(models)-1] != fallbackTarget ||
		slices.Contains(models[:len(models)-1], fallbackTarget) {
		t.Fatalf("上游收到 %v，期望 argon %d 次额度失败后 carbon 一次", models, primaryQuotaSwitchLimit+1)
	}
	if progressCountAll(service, "模型 "+fallbackSource+" 额度用完（额度不足：额度失败已达换号上限，最早恢复 ", "按 MODEL_FALLBACKS 改用 "+fallbackTarget) != 1 {
		t.Fatal("请求日志应写一行额度不足后改用 carbon")
	}
	if progressLogged(service, "立即返回 429") || !progressLogged(service, "Ultra 主用模型额度不足，结束这个模型的选号，交给 MODEL_FALLBACKS 改用其他模型 | 原因=额度失败已达换号上限") {
		t.Fatal("改用时额度不足的日志不应写“立即返回 429”")
	}
	assertNoBusyAccounts(t, pool)
}

// TestModelFallbackTargetsExhausted 源模型与目标都在全部账户上冷却：按源模型原来的 429 返回（全部冷却错误的模型仍是 argon），
// Retry-After 取两者中最早恢复的时刻；日志写明试过的目标；访问日志与账本记回源模型
func TestModelFallbackTargetsExhausted(t *testing.T) {
	for _, test := range []struct {
		name           string
		source, target time.Duration
		want           time.Duration
	}{
		{name: "目标先恢复", source: 3 * time.Hour, target: time.Hour, want: time.Hour},
		{name: "源模型先恢复", source: 30 * time.Minute, target: 2 * time.Hour, want: 30 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			accounts := ultraTestIDs(2)
			upstream := &fallbackUpstream{}
			service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
			coolAll(t, pool, fallbackSource, test.source, accounts...)
			coolAll(t, pool, fallbackTarget, test.target, accounts...)
			result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: fallbackSource})
			var cooling *aistudio.AllCoolingError
			if !errors.As(result.err, &cooling) || cooling.ModelID != fallbackSource || result.served != fallbackSource {
				t.Fatalf("err=%v served=%q，期望按 argon 的全部冷却返回", result.err, result.served)
			}
			var provider interface{ RetryAt() time.Time }
			if !errors.As(result.err, &provider) {
				t.Fatal("错误应带最早恢复时间")
			}
			if diff := time.Until(provider.RetryAt()) - test.want; diff < -time.Minute || diff > time.Minute {
				t.Fatalf("最早恢复 = %s 后，期望约 %s 后", time.Until(provider.RetryAt()), test.want)
			}
			if !progressLoggedAll(service, "按 MODEL_FALLBACKS 改用其他模型也没有成功，按源模型的错误返回",
				"试过="+fallbackTarget+" 额度用完（候选全部冷却") {
				t.Fatal("日志应写明试过的目标与结果")
			}
			if models := upstream.models(); len(models) != 0 {
				t.Fatalf("全部冷却不应向上游发送，实际 %v", models)
			}
			// 经公开 API：429，Retry-After 按最早恢复（另加最多 3 秒的抖动），账本记源模型
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, fallbackSource)
			recorder, entries := postFallback(t, service, "/ultra/v1/chat/completions", body)
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("状态 %d，期望 429：%s", recorder.Code, recorder.Body.String())
			}
			seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
			if want := int(test.want.Seconds()); err != nil || seconds < want-60 || seconds > want+5 {
				t.Fatalf("Retry-After = %q，期望约 %d", recorder.Header().Get("Retry-After"), want)
			}
			if len(entries) != 1 || entries[0].Model != fallbackSource || entries[0].FallbackFrom != "" ||
				!strings.HasSuffix(entries[0].Error, "（已按 MODEL_FALLBACKS 改用其他模型："+fallbackTarget+" 额度用完）") ||
				strings.Count(entries[0].Error, "可用账户均在冷却") != 1 {
				t.Fatalf("访问日志 = %+v，期望记源模型并写明试过的目标与结果类别（不嵌入目标错误的原文）", entries)
			}
		})
	}
}

// TestModelFallbackScope 生效范围：ultra 时普通路径不改用；all 时普通路径也改用；off 时 /ultra 也不改用（与没有这项设置时相同）
func TestModelFallbackScope(t *testing.T) {
	for _, test := range []struct {
		name     string
		scope    string
		tier     aistudio.BenefitTier
		pool     aistudio.PoolScope
		switched bool
	}{
		{name: "ultra 范围的普通路径", scope: config.ModelFallbackScopeUltra, tier: aistudio.BenefitTierFree, pool: aistudio.PoolScopeNormal},
		{name: "ultra 范围的不限号池路径", scope: config.ModelFallbackScopeUltra, tier: aistudio.BenefitTierFree, pool: aistudio.PoolScopeAll},
		{name: "all 范围的普通路径", scope: config.ModelFallbackScopeAll, tier: aistudio.BenefitTierFree, pool: aistudio.PoolScopeNormal, switched: true},
		{name: "all 范围的 /ultra", scope: config.ModelFallbackScopeAll, tier: aistudio.BenefitTierUltra, pool: aistudio.PoolScopeUltra, switched: true},
		{name: "off 范围的 /ultra", scope: config.ModelFallbackScopeOff, tier: aistudio.BenefitTierUltra, pool: aistudio.PoolScopeUltra},
	} {
		t.Run(test.name, func(t *testing.T) {
			accounts := ultraTestIDs(2)
			upstream := &fallbackUpstream{}
			service, pool := fallbackService(t, upstream, test.tier, test.scope, accounts...)
			coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
			result := runFallbackRequest(t, service, test.pool, aistudio.GenerateRequest{Model: fallbackSource})
			if test.switched {
				if result.err != nil || result.served != fallbackTarget || !slices.Equal(upstream.models(), []string{fallbackTarget}) {
					t.Fatalf("err=%v served=%q 上游=%v，期望改用 carbon", result.err, result.served, upstream.models())
				}
				return
			}
			var cooling *aistudio.AllCoolingError
			if !errors.As(result.err, &cooling) || cooling.ModelID != fallbackSource || result.served != fallbackSource {
				t.Fatalf("err=%v served=%q，期望照原样按 argon 的全部冷却返回", result.err, result.served)
			}
			var exhausted *modelFallbackExhaustedError
			if errors.As(result.err, &exhausted) || len(upstream.models()) != 0 || progressLogged(service, "MODEL_FALLBACKS") {
				t.Fatalf("不改用时错误、上游与日志都应与原先相同：err=%v 上游=%v", result.err, upstream.models())
			}
			if test.pool == aistudio.PoolScopeUltra && !progressLogged(service, "Ultra 主用模型候选全部冷却，立即返回 429 | 最早恢复=") {
				t.Fatal("不改用时选号日志应与原先相同")
			}
		})
	}
}

// TestModelFallbackNonQuotaNotTriggered 非额度原因不改用：上游 5xx、参数错误、号池繁忙排队超限、请求指定账户；已交出事件后的额度错误也不改用
func TestModelFallbackNonQuotaNotTriggered(t *testing.T) {
	accounts := ultraTestIDs(2)
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "上游 503", err: &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusServiceUnavailable, Code: 14, Message: "overloaded"}},
		{name: "参数错误", err: &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: "bad argument"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &fallbackUpstream{fail: map[string]func(string) error{fallbackSource: func(string) error { return test.err }}}
			service, _ := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
			result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: fallbackSource})
			if result.err == nil || slices.Contains(upstream.models(), fallbackTarget) || result.served != fallbackSource {
				t.Fatalf("err=%v 上游=%v served=%q，非额度原因不应改用", result.err, upstream.models(), result.served)
			}
		})
	}
	t.Run("号池繁忙", func(t *testing.T) {
		upstream := &fallbackUpstream{}
		service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
		pool.SetPerAccountConcurrency(1)
		setUltraPrimaryModels(service, []string{fallbackSource}, 100*time.Millisecond)
		for _, accountID := range accounts {
			release := holdLease(t, pool, accountID)
			defer release()
		}
		result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: fallbackSource})
		var busy *aistudio.CapacityBusyError
		if !errors.As(result.err, &busy) || len(upstream.models()) != 0 || progressLogged(service, "MODEL_FALLBACKS") {
			t.Fatalf("err=%v 上游=%v，排队超限应照原样按号池繁忙返回", result.err, upstream.models())
		}
	})
	t.Run("指定账户", func(t *testing.T) {
		upstream := &fallbackUpstream{}
		service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
		coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
		result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: fallbackSource, AccountID: accounts[0]})
		if result.err == nil || len(upstream.models()) != 0 || result.served != fallbackSource {
			t.Fatalf("err=%v 上游=%v，指定账户的请求不应改用", result.err, upstream.models())
		}
		if !progressLogged(service, "但请求指定了账户，不按 MODEL_FALLBACKS 改用") {
			t.Fatal("日志应写明指定账户不改用")
		}
	})
	t.Run("已交出事件", func(t *testing.T) {
		upstream := &fallbackUpstream{midStream: map[string]error{fallbackSource: ultraDailyQuotaError()}}
		service, _ := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
		result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: fallbackSource})
		if result.text != "reply from "+fallbackSource || result.err == nil || slices.Contains(upstream.models(), fallbackTarget) {
			t.Fatalf("text=%q err=%v 上游=%v，首个事件之后的额度错误不应改用", result.text, result.err, upstream.models())
		}
	})
}

// TestModelFallbackBlockedRequests 额度用完但不能改用的请求：文件绑定、降级判定、非文本生成、源模型是 Interaction（Omni）模型；
// 按原错误返回并写明原因
func TestModelFallbackBlockedRequests(t *testing.T) {
	accounts := ultraTestIDs(1)
	service, pool := fallbackService(t, &fallbackUpstream{}, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	// 目录里另有一个经 Interaction 通道生成的 Omni 模型，并给它配上改用规则：额度用完也不改用到文本模型（请求方式不同）
	omni := aistudio.Model{
		ID: fallbackOmni, Name: fallbackOmni, Methods: []string{"generateContent"},
		Capabilities: map[string]bool{"chat_model": true, "interactions_api": true},
	}
	if err := pool.SetCatalog(accounts[0], aistudio.BenefitTierUltra, append(fallbackCatalog(), omni)); err != nil {
		t.Fatal(err)
	}
	service.setModelFallbacks(config.DefaultModelFallbacks+","+fallbackOmni+"="+fallbackTarget, config.ModelFallbackScopeUltra)
	ctx := aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra)
	cooling := &aistudio.AllCoolingError{ModelID: fallbackSource, Until: time.Now().Add(time.Hour)}
	base := aistudio.GenerateRequest{ID: "req-blocked", Model: fallbackSource, Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}}}
	speech := base
	speech.Config.SpeechConfig = &aistudio.SpeechConfig{VoiceName: "Zephyr"}
	omniRequest := base
	omniRequest.Model = fallbackOmni
	guarded := newGenerationDiagnostics(base, "未发送")
	guarded.downgrade = &downgradeGate{}
	for _, test := range []struct {
		name     string
		request  aistudio.GenerateRequest
		resource string
		diag     *generationDiagnostics
		reason   string
	}{
		{name: "文件绑定", request: base, resource: "file-abc", reason: "文件绑定"},
		{name: "降级判定", request: base, diag: guarded, reason: "降级判定"},
		{name: "语音", request: speech, reason: "不是文本对话生成"},
		{name: "Interaction 源模型", request: omniRequest, reason: "源模型是 Interaction（Omni）模型"},
	} {
		next, run, err := service.continueWithModelFallback(ctx, test.request, test.resource, test.diag, time.Now(), nil, cooling)
		if next != nil || run != nil || err != cooling {
			t.Fatalf("%s：next=%v run=%v err=%v，期望不改用、错误不变", test.name, next, run, err)
		}
		if !progressLoggedAll(service, "额度用完", test.reason, "不按 MODEL_FALLBACKS 改用") {
			t.Fatalf("%s：日志应写明不改用的原因", test.name)
		}
	}
	// 出口限流与号池繁忙不算额度用完
	for _, err := range []error{
		&aistudio.PrimaryQuotaError{ModelID: fallbackSource, Kind: aistudio.QuotaFailEgress, Reason: aistudio.QuotaReasonEgress},
		&aistudio.CapacityBusyError{ModelID: fallbackSource},
		context.DeadlineExceeded,
		&aistudio.RPCError{StatusCode: http.StatusTooManyRequests, Code: 8, Message: "Resource has been exhausted"},
	} {
		if _, _, ok := modelQuotaEnd(err, time.Now()); ok {
			t.Fatalf("%v 不应算额度用完", err)
		}
	}
	minuteQuota := &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "Quota exceeded",
		Metadata: map[string]string{"quota_limit": "GenerateRequestsPerMinutePerProjectPerModel"},
	}
	for _, err := range []error{
		cooling, &aistudio.PrimaryQuotaError{ModelID: fallbackSource, Reason: "额度失败已达换号上限"},
		&aistudio.AccountCoolingDownError{ModelID: fallbackSource, Until: time.Now().Add(time.Hour), Reason: "每日限额: x"},
		aistudio.ErrAccountCoolingDown, ultraDailyQuotaError(), minuteQuota,
	} {
		if _, _, ok := modelQuotaEnd(err, time.Now()); !ok {
			t.Fatalf("%v 应算额度用完", err)
		}
	}
	// 整个账号的全局分钟限额不是这个模型的额度用完：改用其他模型也落在同一批账号上
	if _, _, ok := modelQuotaEnd(globalMinuteQuotaError(time.Now()), time.Now()); ok {
		t.Fatal("全局分钟限额不应算这个模型的额度用完")
	}
}

// TestModelFallbackSkipsTargets 目标按顺序检查：不在号池目录里、接不了请求的工具时跳过并记一行原因，改用下一个能接的目标；
// 目标不递归套用它自己的规则
func TestModelFallbackSkipsTargets(t *testing.T) {
	accounts := ultraTestIDs(2)
	upstream := &fallbackUpstream{}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	// carbon 自己的规则指回 argon：改用到 carbon 后不再套用
	service.setModelFallbacks(fallbackSource+"=gemini-missing|"+fallbackPlain+"|"+fallbackTarget+","+fallbackTarget+"="+fallbackSource, config.ModelFallbackScopeUltra)
	coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
	request := aistudio.GenerateRequest{Model: fallbackSource, Tools: aistudio.Tools{Functions: []aistudio.FunctionDeclaration{{Name: "lookup"}}}}
	result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, request)
	if result.err != nil || result.served != fallbackTarget || !slices.Equal(upstream.models(), []string{fallbackTarget}) {
		t.Fatalf("err=%v served=%q 上游=%v，期望跳过前两个目标后改用 carbon", result.err, result.served, upstream.models())
	}
	for _, fragment := range []string{
		"MODEL_FALLBACKS 的目标 gemini-missing 不在这个号池的模型目录里，跳过",
		"MODEL_FALLBACKS 的目标 " + fallbackPlain + " 不支持请求的工具（",
	} {
		if !progressLogged(service, fragment) {
			t.Fatalf("日志应写明 %q", fragment)
		}
	}
	// carbon 也全部冷却：不会再按 carbon 的规则改回 argon（不成环、不把 argon 当成下一个目标），按 argon 的错误结束
	coolAll(t, pool, fallbackTarget, time.Hour, accounts...)
	request.ID = "req-loop"
	result = runFallbackRequest(t, service, aistudio.PoolScopeUltra, request)
	var cooling *aistudio.AllCoolingError
	if !errors.As(result.err, &cooling) || cooling.ModelID != fallbackSource || progressCountAll(service, "按 MODEL_FALLBACKS 改用 ") != 2 {
		t.Fatalf("err=%v，期望只改用一次 carbon 后按 argon 的错误结束", result.err)
	}
	if progressLogged(service, "MODEL_FALLBACKS 的目标 "+fallbackSource) || progressLogged(service, "改用 "+fallbackSource) {
		t.Fatal("目标不应递归套用它自己的规则")
	}
	if models := upstream.models(); !slices.Equal(models, []string{fallbackTarget}) {
		t.Fatalf("上游收到 %v，第二个请求全部冷却不应发送", models)
	}
}

// TestModelFallbackCatalogAliases 规则的源模型与目标也接受目录别名（按请求号池的目录解析）：别名写的源模型照常匹配；
// 两个目标解析成同一个模型时第二个按已经试过跳过
func TestModelFallbackCatalogAliases(t *testing.T) {
	accounts := ultraTestIDs(2)
	upstream := &fallbackUpstream{}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	service.setModelFallbacks("gemini-argon-latest=gemini-carbon-latest", config.ModelFallbackScopeUltra)
	coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
	// 客户端请求写目录别名：startGenerate 解析成目录 ID 后与规则（同样按目录解析）匹配
	result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: "gemini-argon-latest"})
	if result.err != nil || !slices.Equal(upstream.models(), []string{fallbackTarget}) {
		t.Fatalf("err=%v 上游=%v，别名写的规则应改用 carbon", result.err, upstream.models())
	}
	service.setModelFallbacks(fallbackSource+"="+fallbackTarget+"|gemini-carbon-latest", config.ModelFallbackScopeUltra)
	coolAll(t, pool, fallbackTarget, time.Hour, accounts...)
	result = runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{ID: "req-alias-twice", Model: fallbackSource})
	var cooling *aistudio.AllCoolingError
	if !errors.As(result.err, &cooling) || cooling.ModelID != fallbackSource {
		t.Fatalf("err=%v，期望按 argon 的错误结束", result.err)
	}
	if !progressLogged(service, "MODEL_FALLBACKS 的目标 gemini-carbon-latest 已经试过，跳过") {
		t.Fatal("解析成同一个模型的第二个目标应按已经试过跳过")
	}
}

// TestModelFallbackRetargetsRequest 改用时按目标模型重新整理：long- 前缀与 -low 后缀的组合按目标的能力续写与映射思考强度，
// 只支持预算的目标换成预算；long- 的默认总上限（LONG_OUTPUT_MAX_TOKENS）按目标重新填，自动续写关闭时目标同样不续写；
// 历史里的思考签名换成跳过校验的占位值；最大输出下限按目标重新判断
func TestModelFallbackRetargetsRequest(t *testing.T) {
	accounts := ultraTestIDs(2)
	for _, test := range []struct {
		name, target string
		effort       string
		budget       int64
		longOutput   bool
		// longDisabled 为自动续写已关闭（LONG_OUTPUT_ENABLED=false）
		longDisabled bool
	}{
		{name: "等级", target: fallbackTarget, effort: "low", longOutput: true},
		{name: "预算", target: fallbackBudget, budget: 1024, longOutput: true},
		{name: "自动续写关闭", target: fallbackTarget, effort: "low", longDisabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &fallbackUpstream{}
			service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
			service.setModelFallbacks(fallbackSource+"="+test.target, config.ModelFallbackScopeUltra)
			service.minOutputTokens.Store(4096)
			service.setLongOutput(!test.longDisabled, 262144)
			coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
			body := `{"model":"long-` + fallbackSource + `-low","messages":[` +
				`{"role":"user","content":"first"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"},` +
				`"extra_content":{"google":{"thought_signature":"c291cmNlLXNpZw=="}}}]},` +
				`{"role":"tool","tool_call_id":"call-1","content":"ok"}],` +
				`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
			recorder, _ := postFallback(t, service, "/ultra/v1/chat/completions", body)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"model":"`+test.target+`"`) {
				t.Fatalf("状态 %d，响应 %s", recorder.Code, recorder.Body.String())
			}
			sent := upstream.lastCall(t, test.target)
			if sent.Model != test.target || sent.LongOutput != test.longOutput || sent.Config.ReasoningEffort != test.effort {
				t.Fatalf("发往目标的请求 model=%s long=%v effort=%q，期望 %s long=%v effort=%q",
					sent.Model, sent.LongOutput, sent.Config.ReasoningEffort, test.target, test.longOutput, test.effort)
			}
			// long- 的默认总上限按目标模型重新填（RetargetGenerateRequest 先清零）：没有它时 long- 按单轮上限处理，不再模拟续写
			if want := map[bool]int64{true: 262144, false: 0}[test.longOutput]; sent.LongOutputDefaultTokens != want {
				t.Fatalf("long- 默认总上限 = %d，期望 %d", sent.LongOutputDefaultTokens, want)
			}
			// 自动续写关闭：源模型与目标模型各按开关检查一次，各写一行
			if test.longDisabled && progressCountAll(service, "模型名带 long- 前缀，但自动续写已关闭") != 2 {
				t.Fatal("改用后应按目标模型再检查一次自动续写开关并写明已关闭")
			}
			if test.budget > 0 && (sent.Config.ThinkingBudget == nil || *sent.Config.ThinkingBudget != test.budget) {
				t.Fatalf("只支持预算的目标应按 -low 换成预算 %d，实际 %v", test.budget, sent.Config.ThinkingBudget)
			}
			if test.budget == 0 && sent.Config.ThinkingBudget != nil {
				t.Fatalf("支持等级的目标不应带预算：%v", *sent.Config.ThinkingBudget)
			}
			if sent.MinOutputTokens != 4096 {
				t.Fatalf("最大输出下限 = %d，期望 4096", sent.MinOutputTokens)
			}
			var call *aistudio.Part
			for contentIndex := range sent.Contents {
				for partIndex := range sent.Contents[contentIndex].Parts {
					if part := &sent.Contents[contentIndex].Parts[partIndex]; part.FunctionCall != nil {
						call = part
					}
				}
			}
			if call == nil || call.ThoughtSignature != "skip_thought_signature_validator" || call.FunctionCall.ThoughtSignature != "skip_thought_signature_validator" {
				t.Fatalf("历史函数调用的签名应换成跳过校验的占位值：%+v", call)
			}
			if !progressLogged(service, "历史里的 1 个思考签名换成跳过校验的占位值") {
				t.Fatal("改用的日志应写明替换的签名数")
			}
		})
	}
}

// TestModelFallbackProtocols 四套协议与 Interactions 入口都改用并报告实际使用的模型（Gemini 原生为 modelVersion）
func TestModelFallbackProtocols(t *testing.T) {
	accounts := ultraTestIDs(2)
	for _, test := range []struct {
		name, path, body, field string
	}{
		{name: "OpenAI", path: "/ultra/v1/chat/completions", body: `{"model":"` + fallbackSource + `","messages":[{"role":"user","content":"hi"}]}`, field: "model"},
		{name: "Responses", path: "/ultra/v1/responses", body: `{"model":"` + fallbackSource + `","input":"hi"}`, field: "model"},
		{name: "Anthropic", path: "/ultra/v1/messages", body: `{"model":"` + fallbackSource + `","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`, field: "model"},
		{name: "Gemini", path: "/ultra/v1beta/models/" + fallbackSource + ":generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, field: "modelVersion"},
		{name: "Interactions", path: "/ultra/v1beta/interactions", body: `{"model":"` + fallbackSource + `","input":"hi"}`, field: "model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &fallbackUpstream{}
			service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
			coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
			recorder, entries := postFallback(t, service, test.path, test.body)
			var response map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != http.StatusOK {
				t.Fatalf("状态 %d，响应 %s", recorder.Code, recorder.Body.String())
			}
			if response[test.field] != fallbackTarget {
				t.Fatalf("%s = %v，期望 %s：%s", test.field, response[test.field], fallbackTarget, recorder.Body.String())
			}
			if len(entries) != 1 || entries[0].Model != fallbackTarget || entries[0].FallbackFrom != fallbackSource {
				t.Fatalf("访问日志 = %+v", entries)
			}
		})
	}
	// Gemini 原生流式：每一块的 modelVersion 都是实际使用的模型
	upstream := &fallbackUpstream{}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
	recorder, _ := postFallback(t, service, "/ultra/v1beta/models/"+fallbackSource+":streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if text := recorder.Body.String(); recorder.Code != http.StatusOK || !strings.Contains(text, `"modelVersion":"`+fallbackTarget+`"`) ||
		strings.Contains(text, `"modelVersion":"`+fallbackSource+`"`) {
		t.Fatalf("状态 %d，流式响应 %s", recorder.Code, text)
	}
	// Anthropic、Responses、Interactions 流式：响应头前改用时开头的 message_start、response.created 与 response.in_progress、
	// interaction.created 就报告实际使用的模型，整个流里不出现源模型
	for _, test := range []struct {
		name, path, body string
	}{
		{name: "Anthropic 流式", path: "/ultra/v1/messages", body: `{"model":"` + fallbackSource + `","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "Responses 流式", path: "/ultra/v1/responses", body: `{"model":"` + fallbackSource + `","input":"hi","stream":true}`},
		{name: "Interactions 流式", path: "/ultra/v1beta/interactions", body: `{"model":"` + fallbackSource + `","input":"hi","stream":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &fallbackUpstream{}
			service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
			coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
			recorder, entries := postFallback(t, service, test.path, test.body)
			text := recorder.Body.String()
			if recorder.Code != http.StatusOK || !strings.Contains(text, "reply from "+fallbackTarget) {
				t.Fatalf("状态 %d，流式响应 %s，期望 200 与 carbon 的回复", recorder.Code, text)
			}
			if !strings.Contains(text, `"model":"`+fallbackTarget+`"`) || strings.Contains(text, fallbackSource) {
				t.Fatalf("流式响应应只报告 %s：%s", fallbackTarget, text)
			}
			if len(entries) != 1 || entries[0].Model != fallbackTarget || entries[0].FallbackFrom != fallbackSource {
				t.Fatalf("访问日志 = %+v", entries)
			}
		})
	}
}

// TestModelFallbackHotReload 管理页保存改用规则与范围后下一个请求立即按新值；旧版页面不带这些字段时沿用现值；写法无效时拒绝保存
func TestModelFallbackHotReload(t *testing.T) {
	clearAppConfigEnv(t)
	clearPrimaryConfigEnv(t)
	for _, key := range []string{"MODEL_FALLBACKS", "MODEL_FALLBACK_SCOPE"} {
		t.Setenv(key, "")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := t.TempDir() + "/.env"
	cfg := config.Default()
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	accounts := ultraTestIDs(2)
	upstream := &fallbackUpstream{}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, cfg.ModelFallbackScope, accounts...)
	requests := service.requests
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: service.workers, configPath: path, config: cfg}
	manager := &runtimeManager{
		lifecycle: ctx, configPath: path, activeManagement: cfg, requests: requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{}, current: &runtimeGeneration{service: service, admin: admin, config: cfg},
	}
	coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
	if result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{ID: "req-before", Model: fallbackSource}); result.served != fallbackTarget {
		t.Fatalf("默认规则应改用 carbon：%+v", result)
	}

	value := runtimeConfigDTO(cfg)
	rules := " models/" + fallbackSource + " = " + fallbackPlain
	value.ModelFallbacks = &rules
	updated, err := manager.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServiceRestartRequired || updated.ModelFallbacks == nil || *updated.ModelFallbacks != fallbackSource+"="+fallbackPlain {
		t.Fatalf("保存结果 = %v restart=%v，期望规范写法并立即生效", updated.ModelFallbacks, updated.ServiceRestartRequired)
	}
	if result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{ID: "req-after", Model: fallbackSource}); result.served != fallbackPlain {
		t.Fatalf("保存后下一个请求应按新规则改用 %s：%+v", fallbackPlain, result)
	}
	if !hasLogContaining(requests, "额度用完改用其他模型的设置已更新并立即生效", "MODEL_FALLBACKS="+fallbackSource+"="+fallbackPlain) {
		t.Fatalf("热更新应写一行设置已生效，日志 = %v", serviceLogMessages(requests))
	}
	// 旧版页面不带这些字段：沿用现值；只改范围为 off 时立即不再改用
	value.ModelFallbacks, value.ModelFallbackScope = nil, config.ModelFallbackScopeOff
	if _, err := manager.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatal(err)
	}
	if saved, err := config.Load(path); err != nil || saved.ModelFallbacks != fallbackSource+"="+fallbackPlain || saved.ModelFallbackScope != config.ModelFallbackScopeOff {
		t.Fatalf(".env = %q / %q err=%v", saved.ModelFallbacks, saved.ModelFallbackScope, err)
	}
	if result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{ID: "req-off", Model: fallbackSource}); result.served != fallbackSource || result.err == nil {
		t.Fatalf("范围改为 off 后不应改用：%+v", result)
	}
	var operation *adminOperationError
	for _, invalid := range []string{fallbackSource + "=" + fallbackSource, "a=b,a=c", "a=", "a b=c", "a=b|b"} {
		value.ModelFallbacks = &invalid
		if _, err := manager.UpdateRuntimeConfig(ctx, value); !errors.As(err, &operation) || operation.code != "invalid_config" {
			t.Fatalf("规则 %q 应拒绝保存，实际 err=%v", invalid, err)
		}
	}
	value.ModelFallbacks, value.ModelFallbackScope = nil, "sometimes"
	if _, err := manager.UpdateRuntimeConfig(ctx, value); !errors.As(err, &operation) || operation.code != "invalid_config" {
		t.Fatalf("范围 sometimes 应拒绝保存，实际 err=%v", err)
	}
	// 热更新判断与生效配置比较都包含这两项
	active := config.Default()
	changed := active
	changed.ModelFallbackScope = config.ModelFallbackScopeAll
	if liveFieldsEqual(changed, active) || requiresRebuild(changed, active) {
		t.Fatal("改用范围变化应按热更新处理")
	}
	dto := runtimeConfigDTO(changed)
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含改用设置")
	}
	dto.ModelFallbacks, dto.ModelFallbackScope = nil, ""
	if !sameDataConfig(dto, active, dataConfigOverrides{}) {
		t.Fatal("旧版页面不带这些字段时按生效值比较")
	}
}

// TestModelFallbackSignatureOnlyHistory 历史里有只带签名的部分（Anthropic 的 thinking 与 redacted_thinking 块、Responses 经 previous_response_id
// 保存的只带签名的思考）：改用时整个去掉，目标模型的请求能编码（上游发送前按 Build 通道编码），返回 200 与 carbon 的回复。原先只清掉签名，
// 留下空部分，编码失败，最后按 argon 的 429 返回
func TestModelFallbackSignatureOnlyHistory(t *testing.T) {
	accounts := ultraTestIDs(2)
	for _, test := range []struct {
		name, path, body string
		// previous 非空时先按它发一轮（直接请求不冷却的 gemini-budget-chat，回复里带只有签名的思考），再按 body 带 previous_response_id 发第二轮
		previous string
	}{
		{
			name: "Anthropic thinking", path: "/ultra/v1/messages",
			body: `{"model":"` + fallbackSource + `","max_tokens":100,"messages":[{"role":"user","content":"q"},` +
				`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"c2lnLWFyZ29u"},{"type":"text","text":"answer"}]},` +
				`{"role":"assistant","content":[{"type":"redacted_thinking","data":"cmVkYWN0ZWQ="}]},` +
				`{"role":"user","content":"again"}]}`,
		},
		{
			name: "Responses 保存的历史", path: "/ultra/v1/responses",
			previous: `{"model":"` + fallbackBudget + `","input":"q","max_output_tokens":100}`,
			body:     `{"model":"` + fallbackSource + `","input":"again","max_output_tokens":100,"previous_response_id":"%s"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &fallbackUpstream{encode: true, replies: map[string][]aistudio.Event{
				fallbackBudget: {{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "QlVER0VULVNJRw=="}},
			}}
			service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
			coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
			admin := &accessLogCapture{}
			handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
			body := test.body
			if test.previous != "" {
				recorder, _ := postFallbackTo(t, handler, admin, test.path, test.previous)
				var previous struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &previous); err != nil || recorder.Code != http.StatusOK || previous.ID == "" {
					t.Fatalf("第一轮状态 %d，响应 %s", recorder.Code, recorder.Body.String())
				}
				body = fmt.Sprintf(test.body, previous.ID)
			}
			recorder, entries := postFallbackTo(t, handler, admin, test.path, body)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "reply from "+fallbackTarget) {
				t.Fatalf("状态 %d，响应 %s，期望改用 carbon 成功", recorder.Code, recorder.Body.String())
			}
			if last := entries[len(entries)-1]; last.Model != fallbackTarget {
				t.Fatalf("访问日志 = %+v", last)
			}
			sent := upstream.lastCall(t, fallbackTarget)
			for _, content := range sent.Contents {
				if len(content.Parts) == 0 {
					t.Fatalf("不应把空消息发给目标模型：%+v", sent.Contents)
				}
				for _, part := range content.Parts {
					if part.ThoughtSignature != "" {
						t.Fatalf("别的模型的签名不应发给 carbon：%+v", part)
					}
				}
			}
			if !progressLogged(service, "思考签名换成跳过校验的占位值或去掉") {
				t.Fatal("改用的日志应写明处理的签名")
			}
		})
	}
}

// fallbackToolCall 为 carbon 在改用期间签发的带签名的函数调用
func fallbackToolCall(signature string) aistudio.Event {
	return aistudio.Event{Kind: aistudio.EventToolCall, ToolCall: &aistudio.FunctionCall{
		ID: "call_fb_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`), ThoughtSignature: signature,
	}}
}

// sentCallSignature 返回发往上游的请求里第一个函数调用的签名（Part 上的优先）
func sentCallSignature(t *testing.T, request aistudio.GenerateRequest) string {
	t.Helper()
	for _, content := range request.Contents {
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				if part.ThoughtSignature != "" {
					return part.ThoughtSignature
				}
				return part.FunctionCall.ThoughtSignature
			}
		}
	}
	t.Fatalf("请求里没有函数调用：%+v", request.Contents)
	return ""
}

// TestModelFallbackTargetOwnSignatures 改用期间目标模型自己签发的思考签名：下一轮再改用时原样发给目标（原生客户端带回的真实签名保留；
// new-api 丢掉签名写占位值时按目标模型的作用域按调用 ID 还原），只有源模型签发的签名换成占位值；之后发给别的模型（源模型恢复等）时
// 目标模型的签名换成占位值，不原样发过去。原先改用时一律换成占位值，整个改用期间每一步工具调用都丢了前几步的思考上下文
func TestModelFallbackTargetOwnSignatures(t *testing.T) {
	const carbonSignature = "REAL-SIG-gemini-4-carbon"
	accounts := ultraTestIDs(2)
	upstream := &fallbackUpstream{replies: map[string][]aistudio.Event{fallbackTarget: {fallbackToolCall(carbonSignature)}}}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
	admin := &accessLogCapture{}
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
	post := func(path string, body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder, _ := postFallbackTo(t, handler, admin, path, body)
		return recorder
	}
	tools := `"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`
	// 第一轮：argon 全部冷却，改用 carbon，carbon 返回带真实签名的函数调用
	recorder := post("/ultra/v1/chat/completions",
		`{"model":"`+fallbackSource+`","messages":[{"role":"user","content":"q"}],`+tools+`}`)
	var first struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &first); err != nil || recorder.Code != http.StatusOK ||
		len(first.Choices) != 1 || len(first.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("第一轮状态 %d，响应 %s", recorder.Code, recorder.Body.String())
	}
	callID := first.Choices[0].Message.ToolCalls[0].ID
	if issuer := service.fallbackSignatures.issuer(carbonSignature); issuer != fallbackTarget {
		t.Fatalf("改用期间 carbon 交出的签名应登记为 carbon 签发，实际 %q", issuer)
	}
	// 第二轮经 new-api：函数调用的签名被换成占位值；argon 仍冷却，再改用 carbon 时按 carbon 的作用域还原出它自己的签名
	history := `{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":[{"id":"` + callID +
		`","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"},"extra_content":{"google":{"thought_signature":"context_engineering_is_the_way_to_go"}}}]},` +
		`{"role":"tool","tool_call_id":"` + callID + `","content":"ok"}`
	recorder = post("/ultra/v1/chat/completions", `{"model":"`+fallbackSource+`","messages":[`+history+`],`+tools+`}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("第二轮状态 %d，响应 %s", recorder.Code, recorder.Body.String())
	}
	if got := sentCallSignature(t, upstream.lastCall(t, fallbackTarget)); got != carbonSignature {
		t.Fatalf("经 new-api 的第二轮发给 carbon 的签名 = %q，期望还原为 carbon 自己的签名", got)
	}
	if !progressLogged(service, "按 "+fallbackTarget+" 签发过的签名还原了 1 个函数调用的签名") {
		t.Fatal("改用的日志应写明还原的签名数")
	}
	// new-api 写的占位签名不是真实签名：照常换掉，但不算进“换掉的思考签名”（否则每次改用都像丢了源模型的思考上下文）
	if progressLogged(service, "思考签名换成跳过校验的占位值或去掉") {
		t.Fatal("new-api 的占位签名不应计为换掉的思考签名")
	}
	// 原生 Gemini 客户端原样带回 carbon 的真实签名（不带调用 ID）：再改用 carbon 时原样保留，argon 的签名仍换成占位值
	native := `{"contents":[{"role":"user","parts":[{"text":"q"}]},` +
		`{"role":"model","parts":[{"text":"earlier","thoughtSignature":"ARGON-SIG"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"` + carbonSignature + `"}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"ok":true}}}]}],` +
		`"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}]}`
	recorder = post("/ultra/v1beta/models/"+fallbackSource+":generateContent", native)
	if recorder.Code != http.StatusOK {
		t.Fatalf("原生客户端状态 %d，响应 %s", recorder.Code, recorder.Body.String())
	}
	sent := upstream.lastCall(t, fallbackTarget)
	if got := sentCallSignature(t, sent); got != carbonSignature {
		t.Fatalf("原生客户端带回的 carbon 签名发给 carbon 时 = %q，期望原样保留", got)
	}
	for _, content := range sent.Contents {
		for _, part := range content.Parts {
			if part.ThoughtSignature == "ARGON-SIG" {
				t.Fatal("argon 的签名不应发给 carbon")
			}
		}
	}
	// 之后发给别的模型（这里是没有冷却的 gemini-budget-chat，与 argon 恢复后相同）：carbon 的签名换成占位值，写一行说明
	recorder = post("/ultra/v1beta/models/"+fallbackBudget+":generateContent", native)
	if recorder.Code != http.StatusOK {
		t.Fatalf("发给其他模型状态 %d，响应 %s", recorder.Code, recorder.Body.String())
	}
	sent = upstream.lastCall(t, fallbackBudget)
	if got := sentCallSignature(t, sent); got != "skip_thought_signature_validator" {
		t.Fatalf("carbon 的签名发给 %s 时 = %q，期望换成跳过校验的占位值", fallbackBudget, got)
	}
	if sent.Contents[1].Parts[0].ThoughtSignature != "ARGON-SIG" {
		t.Fatalf("不是改用期间签发的签名应原样发送：%+v", sent.Contents[1].Parts[0])
	}
	if !progressLogged(service, "历史里的 1 个思考签名是额度用完改用其他模型时由 "+fallbackTarget+" 签发的，发给 "+fallbackBudget+" 前换成跳过校验的占位值或去掉") {
		t.Fatal("发给其他模型前应写一行说明")
	}
	// 发给签发它的 carbon 本身时原样发送
	recorder = post("/ultra/v1beta/models/"+fallbackTarget+":generateContent", native)
	if recorder.Code != http.StatusOK || sentCallSignature(t, upstream.lastCall(t, fallbackTarget)) != carbonSignature {
		t.Fatalf("直接发给 carbon 时签名应原样：状态 %d", recorder.Code)
	}
	// 范围改为 off 之后，之前改用期间登记的 carbon 签名发给别的模型时照样换掉（它们不是那个模型签发的）
	service.setModelFallbacks(config.DefaultModelFallbacks, config.ModelFallbackScopeOff)
	recorder = post("/ultra/v1beta/models/"+fallbackBudget+":generateContent", native)
	if recorder.Code != http.StatusOK || sentCallSignature(t, upstream.lastCall(t, fallbackBudget)) != "skip_thought_signature_validator" {
		t.Fatalf("范围为 off 时 carbon 的签名发给 %s 也应换掉：状态 %d", fallbackBudget, recorder.Code)
	}
}

// TestModelFallbackToolCallSignatureToOtherModel OpenAI 格式的历史（tool_calls 的 extra_content.google.thought_signature）里带改用期间
// gemini-4-carbon 签发的签名，之后发给别的模型：协议层只把签名写在函数调用（FunctionCall）上、部分（Part）上没有，照样认出是 carbon 签发的，
// 换成跳过校验的占位值并写一行说明。Anthropic 的 tool_use 同样只写在函数调用上；只看部分上的签名时 carbon 的签名会原样发给别的模型
func TestModelFallbackToolCallSignatureToOtherModel(t *testing.T) {
	const carbonSignature = "REAL-SIG-carbon-openai-tool-call"
	accounts := ultraTestIDs(2)
	upstream := &fallbackUpstream{replies: map[string][]aistudio.Event{fallbackTarget: {fallbackToolCall(carbonSignature)}}}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
	admin := &accessLogCapture{}
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
	tools := `"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`
	// 第一轮：argon 全部冷却，改用 carbon，carbon 返回带真实签名的函数调用（登记为 carbon 签发）
	recorder, _ := postFallbackTo(t, handler, admin, "/ultra/v1/chat/completions",
		`{"model":"`+fallbackSource+`","messages":[{"role":"user","content":"q"}],`+tools+`}`)
	if recorder.Code != http.StatusOK || service.fallbackSignatures.issuer(carbonSignature) != fallbackTarget {
		t.Fatalf("第一轮状态 %d，响应 %s，期望改用 carbon 并登记它的签名", recorder.Code, recorder.Body.String())
	}
	// 第二轮发给 gemini-budget-chat（与 argon 恢复后相同）：历史里 carbon 的签名只在 tool_calls 的 extra_content 上
	history := `{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_fb_1","type":"function",` +
		`"function":{"name":"lookup","arguments":"{\"q\":\"x\"}"},"extra_content":{"google":{"thought_signature":"` + carbonSignature + `"}}}]},` +
		`{"role":"tool","tool_call_id":"call_fb_1","content":"ok"}`
	recorder, _ = postFallbackTo(t, handler, admin, "/ultra/v1/chat/completions",
		`{"model":"`+fallbackBudget+`","messages":[`+history+`],`+tools+`}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("第二轮状态 %d，响应 %s", recorder.Code, recorder.Body.String())
	}
	if got := sentCallSignature(t, upstream.lastCall(t, fallbackBudget)); got != "skip_thought_signature_validator" {
		t.Fatalf("carbon 的签名发给 %s 时 = %q，期望换成跳过校验的占位值", fallbackBudget, got)
	}
	if !progressLogged(service, "历史里的 1 个思考签名是额度用完改用其他模型时由 "+fallbackTarget+" 签发的，发给 "+fallbackBudget+" 前换成跳过校验的占位值或去掉") {
		t.Fatal("发给其他模型前应写一行说明")
	}
}

// TestModelFallbackOnlineTarget 请求带 -online、目标不支持搜索：按目标模型整理后再检查工具（协议层追加的 google_search 去掉），改用成功，
// 发往目标的请求不带 google_search；原先拿源模型的请求检查，误判为不支持请求的工具而跳过，按 argon 的 429 返回
func TestModelFallbackOnlineTarget(t *testing.T) {
	accounts := ultraTestIDs(2)
	upstream := &fallbackUpstream{}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	service.setModelFallbacks(fallbackSource+"="+fallbackBudget, config.ModelFallbackScopeUltra)
	coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
	recorder, _ := postFallback(t, service, "/ultra/v1/chat/completions",
		`{"model":"`+fallbackSource+`-online","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "reply from "+fallbackBudget) {
		t.Fatalf("状态 %d，响应 %s，期望改用 %s 成功", recorder.Code, recorder.Body.String(), fallbackBudget)
	}
	if sent := upstream.lastCall(t, fallbackBudget); slices.Contains(sent.Tools.Google, "google_search") {
		t.Fatalf("不支持搜索的目标不应带 google_search：%+v", sent.Tools)
	}
	if progressLogged(service, "不支持请求的工具") {
		t.Fatal("-online 追加的搜索不应让目标被跳过")
	}
	// 客户端自己声明了搜索工具时照常按不支持跳过
	upstream.mu.Lock()
	upstream.calls = nil
	upstream.mu.Unlock()
	recorder, _ = postFallback(t, service, "/ultra/v1beta/models/"+fallbackSource+":generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"googleSearch":{}}]}`)
	if recorder.Code != http.StatusTooManyRequests || len(upstream.models()) != 0 || !progressLogged(service, "不支持请求的工具") {
		t.Fatalf("客户端声明搜索时状态 %d，上游 %v，期望跳过目标后按 429 返回", recorder.Code, upstream.models())
	}
}

// TestModelFallbackDowngradeGuardTarget 目标在降级判定里（DOWNGRADE_GUARD_MODELS）：改用后没有按它建立的判定，跳过并写明原因，
// 不绕过判定交出它的回复
func TestModelFallbackDowngradeGuardTarget(t *testing.T) {
	accounts := ultraTestIDs(2)
	upstream := &fallbackUpstream{}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	guard := config.Default().DowngradeGuard
	guard.Enabled, guard.Models = true, []string{fallbackTarget}
	service.setDowngradeGuard(guard)
	coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
	result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: fallbackSource})
	var cooling *aistudio.AllCoolingError
	if !errors.As(result.err, &cooling) || cooling.ModelID != fallbackSource || len(upstream.models()) != 0 {
		t.Fatalf("err=%v 上游=%v，目标在降级判定里应跳过、按 argon 的错误返回", result.err, upstream.models())
	}
	if !progressLogged(service, "MODEL_FALLBACKS 的目标 "+fallbackTarget+" 在降级判定里（DOWNGRADE_GUARD_MODELS）") {
		t.Fatal("日志应写明目标在降级判定里")
	}
}

// TestModelFallbackBusyTargetRetryAfter 源模型全部冷却 3 小时、目标只是号池繁忙（排队超限）：按源模型的 429 返回，Retry-After 取目标号池繁忙
// 建议的重试时间（几秒），不按源模型的 3 小时；原因响应头仍按源模型的错误（local_cooling）
func TestModelFallbackBusyTargetRetryAfter(t *testing.T) {
	accounts := ultraTestIDs(2)
	upstream := &fallbackUpstream{}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	pool.SetPerAccountConcurrency(1)
	setUltraPrimaryModels(service, []string{fallbackSource, fallbackTarget}, 100*time.Millisecond)
	coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
	for _, accountID := range accounts {
		release := holdLease(t, pool, accountID)
		defer release()
	}
	body := `{"model":"` + fallbackSource + `","messages":[{"role":"user","content":"hi"}]}`
	recorder, entries := postFallback(t, service, "/ultra/v1/chat/completions", body)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("状态 %d，响应 %s", recorder.Code, recorder.Body.String())
	}
	seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
	if err != nil || seconds > 60 {
		t.Fatalf("Retry-After = %q，期望目标号池繁忙建议的几秒，而不是源模型的 3 小时", recorder.Header().Get("Retry-After"))
	}
	if reason := recorder.Header().Get(api.LimitReasonHeader); reason != api.LimitReasonLocalCooling {
		t.Fatalf("原因响应头 = %q，期望仍按源模型的错误 %s", reason, api.LimitReasonLocalCooling)
	}
	// 账本的错误只写目标与结果类别，不嵌入目标错误的原文（诊断脚本按源模型的错误分类）
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Error, "（已按 MODEL_FALLBACKS 改用其他模型："+fallbackTarget+" 不可用）") ||
		strings.Contains(entries[0].Error, "号池繁忙") {
		t.Fatalf("账本错误 = %+v", entries)
	}
	if !progressLoggedAll(service, "试过="+fallbackTarget+" 不可用（号池繁忙") {
		t.Fatal("日志应写明目标不可用的原因")
	}
}

// TestModelFallbackStreamStartRestart 流式请求改用时 API 层写出响应头前的等待从改用时刻起按目标模型重新计（目标是主用模型时为排队上限加余量），
// 目标模型首事件前的错误照常以 HTTP 状态返回
func TestModelFallbackStreamStartRestart(t *testing.T) {
	accounts := ultraTestIDs(2)
	upstream := &fallbackUpstream{}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	setUltraPrimaryModels(service, []string{fallbackSource, fallbackTarget}, 10*time.Second)
	coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
	hold := aistudio.NewStreamStartHold()
	startedAt := time.Now()
	record := aistudio.NewModelSwitch()
	ctx := aistudio.WithStreamStartHold(aistudio.ContextWithModelSwitch(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), record), hold)
	events, err := service.Generate(ctx, aistudio.GenerateRequest{
		ID: "req-hold", Model: fallbackSource, Stream: true,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if record.Model(fallbackSource) != fallbackTarget {
		t.Fatal("应改用 carbon")
	}
	// 源模型的等待按 0 算：改用后从改用时刻起至少再等 carbon 的排队上限加余量
	if limit := hold.Limit(startedAt, 0); limit < 10*time.Second+primaryStreamStartGrace-time.Second {
		t.Fatalf("改用后写出响应头前的等待 = %s，期望按 carbon 的 %s", limit, 10*time.Second+primaryStreamStartGrace)
	}
}

// TestModelFallbackEndsWithRequest 改用之后请求超时（REQUEST_TIMEOUT 的首事件前期限）或被取消：按那次的错误结束（超时为 504 的
// DeadlineExceeded，取消为 499 的 Canceled），不再试规则里后面的目标，也不换成源模型的 429；日志写“请求已结束”，改用记录恢复为源模型
func TestModelFallbackEndsWithRequest(t *testing.T) {
	for _, test := range []struct {
		name string
		want error
	}{
		{name: "超时", want: context.DeadlineExceeded},
		{name: "取消", want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			accounts := ultraTestIDs(2)
			upstream := &fallbackUpstream{block: map[string]bool{fallbackTarget: true}, blocked: make(chan string, 4)}
			service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
			// 两个目标：第一个 carbon 在上游阻塞到请求结束，第二个 gemini-budget-chat 能正常回复，但请求已结束，不应再试
			service.setModelFallbacks(fallbackSource+"="+fallbackTarget+"|"+fallbackBudget, config.ModelFallbackScopeUltra)
			coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
			request := aistudio.GenerateRequest{ID: "req-ended-" + test.name, Model: fallbackSource}
			if test.want == context.DeadlineExceeded {
				service.timeout.Store(int64(300 * time.Millisecond))
			} else {
				go func() {
					select {
					case <-upstream.blocked:
						if err := service.requests.cancel(request.ID); err != nil {
							t.Error(err)
						}
					case <-time.After(10 * time.Second):
					}
				}()
			}
			result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, request)
			if !errors.Is(result.err, test.want) {
				t.Fatalf("err = %v，期望按请求结束的错误（%v）返回", result.err, test.want)
			}
			var exhausted *modelFallbackExhaustedError
			var cooling *aistudio.AllCoolingError
			if errors.As(result.err, &exhausted) || errors.As(result.err, &cooling) {
				t.Fatalf("err = %v，请求已结束时不应换成源模型的 429", result.err)
			}
			if models := upstream.models(); !slices.Equal(models, []string{fallbackTarget}) {
				t.Fatalf("上游收到 %v，期望只有 carbon（请求已结束，不再试后面的目标）", models)
			}
			if result.served != fallbackSource {
				t.Fatalf("改用记录报告 %q，请求以错误结束时应恢复为源模型", result.served)
			}
			if !progressLogged(service, "按 MODEL_FALLBACKS 改用其他模型也没有成功，请求已结束（") ||
				progressLogged(service, "按 MODEL_FALLBACKS 改用 "+fallbackBudget) {
				t.Fatal("日志应写明请求已结束，且不再改用后面的目标")
			}
			assertNoBusyAccounts(t, pool)
		})
	}
}

// TestModelFallbackLedgerAttempts 用量账本的账户、通道与上游尝试：
//   - argon 每个账户都每日限额 429 后改用 carbon 成功：argon 的尝试记 argon，改用备注没有账户与模型，账户与通道为 carbon 成功的那次
//   - argon 全部冷却（一次也没有发出）、carbon 在每个账户上都每日限额 429：按 argon 的错误结束，账户与通道恢复为空（不记成 carbon 最后用过的），
//     carbon 的尝试记 carbon
//   - argon 在每个账户上都发出过并每日限额 429、carbon 只在 p0 上发出也 429：账户与通道恢复为 argon 最后那次尝试的（不是 carbon 的 p0）
//   - 改用两次（carbon 也用完，再改用 gemini-budget-chat）：两条改用备注都不带模型，argon 与 carbon 的尝试各记各的
//   - 目标都被跳过（不在号池目录里）、没有改用：尝试不带模型，没有改用备注
//   - 范围为 off 时与没有这项设置时相同：尝试不带模型
func TestModelFallbackLedgerAttempts(t *testing.T) {
	accounts := ultraTestIDs(3)
	body := `{"model":"` + fallbackSource + `","messages":[{"role":"user","content":"hi"}]}`
	t.Run("改用成功", func(t *testing.T) {
		upstream := &fallbackUpstream{fail: map[string]func(string) error{fallbackSource: func(string) error { return ultraDailyQuotaError() }}}
		service, _ := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
		recorder, entries := postFallback(t, service, "/ultra/v1/chat/completions", body)
		if recorder.Code != http.StatusOK || len(entries) != 1 {
			t.Fatalf("状态 %d，访问日志 %d 条：%s", recorder.Code, len(entries), recorder.Body.String())
		}
		entry := entries[0]
		upstream.mu.Lock()
		served := upstream.calls[len(upstream.calls)-1]
		upstream.mu.Unlock()
		if entry.Model != fallbackTarget || served.model != fallbackTarget || entry.Account != served.account || entry.Channel == "" {
			t.Fatalf("访问日志 model=%q account=%q channel=%q，期望 carbon 成功的那次（%s）", entry.Model, entry.Account, entry.Channel, served.account)
		}
		sourceAttempts, notes := 0, 0
		for _, attempt := range entry.Attempts {
			switch {
			case attempt.Account == "":
				notes++
				if attempt.Model != "" || !strings.Contains(attempt.Error, "按 MODEL_FALLBACKS 改用 "+fallbackTarget) {
					t.Fatalf("改用备注 = %+v", attempt)
				}
			case attempt.Model == fallbackSource:
				sourceAttempts++
			default:
				t.Fatalf("argon 的尝试应记 argon：%+v", attempt)
			}
		}
		if sourceAttempts != primaryQuotaSwitchLimit+1 || notes != 1 {
			t.Fatalf("尝试 = %+v，期望 %d 次 argon 与一条改用备注", entry.Attempts, primaryQuotaSwitchLimit+1)
		}
	})
	t.Run("目标也用完", func(t *testing.T) {
		upstream := &fallbackUpstream{fail: map[string]func(string) error{fallbackTarget: func(string) error { return ultraDailyQuotaError() }}}
		service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
		coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
		recorder, entries := postFallback(t, service, "/ultra/v1/chat/completions", body)
		if recorder.Code != http.StatusTooManyRequests || len(entries) != 1 {
			t.Fatalf("状态 %d，访问日志 %d 条：%s", recorder.Code, len(entries), recorder.Body.String())
		}
		entry := entries[0]
		if entry.Model != fallbackSource || entry.Account != "" || entry.Channel != "" {
			t.Fatalf("访问日志 model=%q account=%q channel=%q，期望记 argon、账户与通道为空（argon 一次也没有发出）",
				entry.Model, entry.Account, entry.Channel)
		}
		targetAttempts := 0
		for _, attempt := range entry.Attempts {
			if attempt.Account == "" {
				continue
			}
			if attempt.Model != fallbackTarget {
				t.Fatalf("carbon 的尝试应记 carbon：%+v", attempt)
			}
			targetAttempts++
		}
		if targetAttempts == 0 || targetAttempts != len(upstream.models()) {
			t.Fatalf("尝试 = %+v，上游 %v，期望每次 carbon 的上游尝试都记 carbon", entry.Attempts, upstream.models())
		}
		if row := requestRow(entry, time.Now()); row.Model != fallbackSource || row.Account != "" {
			t.Fatalf("用量账本 model=%q account=%q", row.Model, row.Account)
		}
	})
	t.Run("源模型发出过、目标也用完", func(t *testing.T) {
		// argon 在每个账户上都每日限额 429（三个账户都发出过）；carbon 预先在 p1、p2 上冷却，只在 p0 上发出并每日限额 429
		upstream := &fallbackUpstream{fail: map[string]func(string) error{
			fallbackSource: func(string) error { return ultraDailyQuotaError() },
			fallbackTarget: func(string) error { return ultraDailyQuotaError() },
		}}
		service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
		coolAll(t, pool, fallbackTarget, 3*time.Hour, accounts[1:]...)
		recorder, entries := postFallback(t, service, "/ultra/v1/chat/completions", body)
		if recorder.Code != http.StatusTooManyRequests || len(entries) != 1 {
			t.Fatalf("状态 %d，访问日志 %d 条：%s", recorder.Code, len(entries), recorder.Body.String())
		}
		upstream.mu.Lock()
		calls := slices.Clone(upstream.calls)
		upstream.mu.Unlock()
		var sourceAccounts, targetAccounts []string
		for _, call := range calls {
			if call.model == fallbackSource {
				sourceAccounts = append(sourceAccounts, call.account)
			} else {
				targetAccounts = append(targetAccounts, call.account)
			}
		}
		sorted := slices.Sorted(slices.Values(sourceAccounts))
		if !slices.Equal(sorted, accounts) || !slices.Equal(targetAccounts, accounts[:1]) {
			t.Fatalf("argon 发往 %v，carbon 发往 %v，期望 argon 每个账户各一次、carbon 只发往 %s", sourceAccounts, targetAccounts, accounts[0])
		}
		// argon 最后用的账户不是 carbon 的 p0，才能区分“恢复为源模型的账户”与“留着目标模型最后用过的账户”
		sourceLast := sourceAccounts[len(sourceAccounts)-1]
		if sourceLast == accounts[0] {
			t.Fatalf("argon 最后发往 %s，与 carbon 的账户相同，测试区分不了", sourceLast)
		}
		entry := entries[0]
		sourceChannel := ""
		counts := map[string]int{}
		for _, attempt := range entry.Attempts {
			switch {
			case attempt.Account == "":
				counts["备注"]++
			case attempt.Model == fallbackSource:
				counts[fallbackSource]++
				if attempt.Account == sourceLast {
					sourceChannel = attempt.Channel
				}
			case attempt.Model == fallbackTarget && attempt.Account == accounts[0]:
				counts[fallbackTarget]++
			default:
				t.Fatalf("上游尝试应各记各的模型：%+v", attempt)
			}
		}
		if counts[fallbackSource] != len(accounts) || counts[fallbackTarget] != 1 || counts["备注"] != 1 || sourceChannel == "" {
			t.Fatalf("尝试 = %+v，期望 argon %d 次、carbon 1 次与一条改用备注", entry.Attempts, len(accounts))
		}
		// 按 argon 的错误结束：账户与通道恢复为 argon 运行结束时的值（最后那次尝试），不记成 carbon 最后用过的 p0
		if entry.Model != fallbackSource || entry.FallbackFrom != "" || entry.Account != sourceLast || entry.Channel != sourceChannel {
			t.Fatalf("访问日志 model=%q fallback_from=%q account=%q channel=%q，期望 argon、账户 %s、通道 %q",
				entry.Model, entry.FallbackFrom, entry.Account, entry.Channel, sourceLast, sourceChannel)
		}
		if row := requestRow(entry, time.Now()); row.Model != fallbackSource || row.Account != sourceLast || row.Channel == "" {
			t.Fatalf("用量账本 model=%q account=%q channel=%q", row.Model, row.Account, row.Channel)
		}
	})
	t.Run("改用两次", func(t *testing.T) {
		// argon 与 carbon 在每个账户上都每日限额 429，再改用 gemini-budget-chat 成功：源模型的尝试只在第一次改用时补记，
		// 第一条改用备注在第二次改用时不被记成 argon
		upstream := &fallbackUpstream{fail: map[string]func(string) error{
			fallbackSource: func(string) error { return ultraDailyQuotaError() },
			fallbackTarget: func(string) error { return ultraDailyQuotaError() },
		}}
		service, _ := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
		service.setModelFallbacks(fallbackSource+"="+fallbackTarget+"|"+fallbackBudget, config.ModelFallbackScopeUltra)
		recorder, entries := postFallback(t, service, "/ultra/v1/chat/completions", body)
		if recorder.Code != http.StatusOK || len(entries) != 1 || !strings.Contains(recorder.Body.String(), "reply from "+fallbackBudget) {
			t.Fatalf("状态 %d，访问日志 %d 条：%s", recorder.Code, len(entries), recorder.Body.String())
		}
		entry := entries[0]
		if entry.Model != fallbackBudget || entry.FallbackFrom != fallbackSource {
			t.Fatalf("访问日志 model=%q fallback_from=%q", entry.Model, entry.FallbackFrom)
		}
		counts := map[string]int{}
		for _, attempt := range entry.Attempts {
			if attempt.Account == "" {
				if attempt.Model != "" {
					t.Fatalf("改用备注不应带模型：%+v", attempt)
				}
				counts["备注"]++
				continue
			}
			counts[attempt.Model]++
		}
		targetCalls := 0
		for _, model := range upstream.models() {
			if model == fallbackTarget {
				targetCalls++
			}
		}
		if counts["备注"] != 2 || counts[fallbackSource] != primaryQuotaSwitchLimit+1 || targetCalls == 0 || counts[fallbackTarget] != targetCalls ||
			len(counts) != 3 {
			t.Fatalf("尝试 = %+v，期望两条改用备注、argon %d 次、carbon %d 次", entry.Attempts, primaryQuotaSwitchLimit+1, targetCalls)
		}
	})
	t.Run("目标都被跳过", func(t *testing.T) {
		// 目标不在号池目录里，一次也没有改用：与没有改用的请求相同，argon 的尝试不带模型，也没有改用备注
		upstream := &fallbackUpstream{fail: map[string]func(string) error{fallbackSource: func(string) error { return ultraDailyQuotaError() }}}
		service, _ := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
		service.setModelFallbacks(fallbackSource+"=gemini-not-in-catalog", config.ModelFallbackScopeUltra)
		recorder, entries := postFallback(t, service, "/ultra/v1/chat/completions", body)
		if recorder.Code != http.StatusTooManyRequests || len(entries) != 1 || len(entries[0].Attempts) == 0 {
			t.Fatalf("状态 %d，访问日志 %+v", recorder.Code, entries)
		}
		if !progressLogged(service, "MODEL_FALLBACKS 的目标 gemini-not-in-catalog 不在这个号池的模型目录里，跳过") {
			t.Fatal("日志应写明跳过的目标")
		}
		entry := entries[0]
		if entry.Model != fallbackSource || entry.FallbackFrom != "" {
			t.Fatalf("访问日志 model=%q fallback_from=%q", entry.Model, entry.FallbackFrom)
		}
		for _, attempt := range entry.Attempts {
			if attempt.Model != "" || attempt.Account == "" {
				t.Fatalf("没有改用时尝试不应带模型（也没有改用备注）：%+v", attempt)
			}
		}
	})
	t.Run("范围为 off", func(t *testing.T) {
		upstream := &fallbackUpstream{fail: map[string]func(string) error{fallbackSource: func(string) error { return ultraDailyQuotaError() }}}
		service, _ := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeOff, accounts...)
		recorder, entries := postFallback(t, service, "/ultra/v1/chat/completions", body)
		if recorder.Code != http.StatusTooManyRequests || len(entries) != 1 || len(entries[0].Attempts) == 0 {
			t.Fatalf("状态 %d，访问日志 %+v", recorder.Code, entries)
		}
		for _, attempt := range entries[0].Attempts {
			if attempt.Model != "" || attempt.Account == "" {
				t.Fatalf("不改用时尝试应与原先相同（不带模型）：%+v", attempt)
			}
		}
	})
}
