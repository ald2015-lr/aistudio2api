package aistudio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"
)

// TestInputTokenLimitError 按文案识别输入 token 超过上限：400 带计数的写法与部分账号 131072 上限的 429 写法都算，
// 额度文案不算；包装后的错误同样识别
func TestInputTokenLimitError(t *testing.T) {
	for _, test := range []struct {
		message string
		want    bool
	}{
		{"The input token count exceeds the maximum number of tokens allowed 131072", true},
		{"The input token count (2000000) exceeds the maximum number of tokens allowed (1048576).", true},
		{"Input token count exceeds limit", true},
		{"You exceeded your current quota, please check your plan and billing details.", false},
		{"Resource has been exhausted (e.g. check quota).", false},
		{"Too Many Requests", false},
	} {
		err := &RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: test.message}
		if got := InputTokenLimitError(err); got != test.want {
			t.Fatalf("%q: InputTokenLimitError = %v，期望 %v", test.message, got, test.want)
		}
		if got := InputTokenLimitError(fmt.Errorf("换号重试: %w", err)); got != test.want {
			t.Fatalf("%q（包装后）: InputTokenLimitError = %v，期望 %v", test.message, got, test.want)
		}
	}
	if InputTokenLimitError(nil) {
		t.Fatal("nil 不应识别为输入超限")
	}
}

// TestInputTokenLimitNotQuota 上游按 429 返回的输入超限不是额度问题：不冷却账号、不换号；普通额度 429 的处理不变
func TestInputTokenLimitNotQuota(t *testing.T) {
	now := time.Now()
	for _, method := range []string{"GenerateContent", "ProxyStreamedCall"} {
		limit := &RPCError{
			Method: method, StatusCode: http.StatusTooManyRequests, Code: 8,
			Message: "The input token count exceeds the maximum number of tokens allowed 131072", RetryDelay: time.Minute,
		}
		if cooldown, ok := QuotaCooldownForError(limit, now); ok {
			t.Fatalf("%s：输入超限不应冷却账号，得到 %+v", method, cooldown)
		}
		if retryableAccountError(limit) {
			t.Fatalf("%s：输入超限不应换号", method)
		}
	}
	quota := &RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "You exceeded your current quota"}
	if _, ok := QuotaCooldownForError(quota, now); !ok {
		t.Fatal("额度 429 仍应冷却账号")
	}
	if !retryableAccountError(quota) {
		t.Fatal("额度 429 仍应换号")
	}
}

// TestInputTokenCapError 只有账号上限的写法算账号上限；带计数的模型上限不算（任何账号都放不下）
func TestInputTokenCapError(t *testing.T) {
	cap429 := &RPCError{Method: "ProxyStreamedCall", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "The input token count exceeds the maximum number of tokens allowed 131072"}
	model400 := &RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: "The input token count (2000000) exceeds the maximum number of tokens allowed (1048576)."}
	if !InputTokenCapError(cap429) || !InputTokenCapError(fmt.Errorf("包装: %w", cap429)) {
		t.Fatal("131072 写法应识别为账号上限")
	}
	if InputTokenCapError(model400) || !InputTokenLimitError(model400) {
		t.Fatal("带计数的模型上限不是账号上限，但仍是输入超限")
	}
	if InputTokenCapError(nil) {
		t.Fatal("nil 不应识别为账号上限")
	}
}

// TestEnabledAccountsAboveTierOf 返回号池内权益高于指定账号的启用账号：Free < Plus < Pro < Ultra，受号池范围限制
func TestEnabledAccountsAboveTierOf(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{
		"free@example.com": BenefitTierFree, "plus@example.com": BenefitTierPlus,
		"pro@example.com": BenefitTierPro, "ultra@example.com": BenefitTierUltra,
	})
	sorted := func(ids []string) []string { slices.Sort(ids); return ids }
	if got := sorted(pool.EnabledAccountsAboveTierOf(PoolScopeAll, "free@example.com")); !slices.Equal(got, []string{"plus@example.com", "pro@example.com", "ultra@example.com"}) {
		t.Fatalf("高于 Free = %v", got)
	}
	if got := sorted(pool.EnabledAccountsAboveTierOf(PoolScopeAll, "plus@example.com")); !slices.Equal(got, []string{"pro@example.com", "ultra@example.com"}) {
		t.Fatalf("高于 Plus = %v", got)
	}
	if got := sorted(pool.EnabledAccountsAboveTierOf(PoolScopeNormal, "free@example.com")); !slices.Equal(got, []string{"plus@example.com", "pro@example.com"}) {
		t.Fatalf("普通号池内高于 Free = %v（不含 Ultra）", got)
	}
	if got := pool.EnabledAccountsAboveTierOf(PoolScopeAll, "ultra@example.com"); len(got) != 0 {
		t.Fatalf("高于 Ultra = %v，期望为空", got)
	}
	if got := pool.EnabledAccountsAboveTierOf(PoolScopeAll, "missing@example.com"); got != nil {
		t.Fatalf("不存在的账号 = %v，期望 nil", got)
	}
}

// TestBuildOnlyModelThinkingLevel 只在 Build 可用的思考模型（默认值按 buildModelDefaults）按请求的思考等级发送 thinkingConfig.thinkingLevel：
// gemini-4-argon-low、-medium 的后缀在 API 层换成 reasoning_effort 后，发给上游的是 LOW、MEDIUM；没有指定时不发等级（由上游默认）
func TestBuildOnlyModelThinkingLevel(t *testing.T) {
	model := Model{ID: "gemini-4-argon", OutputTokenLimit: 65536, Capabilities: map[string]bool{"chat_model": true, "thinking": true}}
	defaults := buildModelDefaults(model)
	for effort, want := range map[string]string{"low": "LOW", "medium": "MEDIUM", "high": "HIGH", "": ""} {
		request := GenerateRequest{
			Model:    "gemini-4-argon",
			Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}},
			Config:   GenerationConfig{ReasoningEffort: effort},
		}
		_, body, err := EncodeBuildGenerateRequest(request, defaults, false, false)
		if err != nil {
			t.Fatalf("%q: %v", effort, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		config, _ := decoded["generationConfig"].(map[string]any)
		thinking, _ := config["thinkingConfig"].(map[string]any)
		got, _ := thinking["thinkingLevel"].(string)
		if got != want {
			t.Fatalf("reasoning_effort=%q 发出的 thinkingLevel = %q，期望 %q（请求体 %s）", effort, got, want, body)
		}
	}
}

// TestBuildOnlyModelEntrySkipsCatalogReload Playground 目录已缓存但没有该模型时，Build 请求直接按 Build 目录推导，
// 不再每次都重新拉取 Playground 目录；目录里有的模型照常用 Playground 目录的条目
func TestBuildOnlyModelEntrySkipsCatalogReload(t *testing.T) {
	const accountID = "alice@example.com"
	playground := Model{ID: "pg-model", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	buildOnly := Model{ID: "build-only", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "thinking": true}}
	pool := testPoolWithAccount(t, accountID)
	if err := pool.SetCatalog(accountID, BenefitTierFree, []Model{playground}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetBuildCatalog(accountID, []Model{buildOnly}); err != nil {
		t.Fatal(err)
	}
	client, transport := truncateClient(t, 0)
	client.catalogs[accountID] = modelCatalog{models: []Model{playground}, entries: map[string]modelEntry{"pg-model": {model: playground}}}
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	entry, err := client.buildModelEntry(context.Background(), GenerateRequest{Model: "models/build-only", AccountID: accountID}, lease)
	if err != nil || entry.model.ID != "build-only" || transport.calls != 0 {
		t.Fatalf("Build 独有模型 entry=%+v err=%v ListModels 调用 %d 次", entry.model, err, transport.calls)
	}
	entry, err = client.buildModelEntry(context.Background(), GenerateRequest{Model: "pg-model", AccountID: accountID}, lease)
	if err != nil || entry.model.ID != "pg-model" || transport.calls != 0 {
		t.Fatalf("Playground 模型 entry=%+v err=%v ListModels 调用 %d 次", entry.model, err, transport.calls)
	}
}
