package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestNoServingAccountError 没有候选的错误保留无账户哨兵：模型在列按 503 UNAVAILABLE 与 Retry-After，不在列按 404 NOT_FOUND 与写明原因类别的
// 英文说明（不带 Retry-After）；对外说明不含内部摘要。只有不是号池暂时不可调度、Build 暂停或已整理过的无账户错误才算“没有候选”
func TestNoServingAccountError(t *testing.T) {
	now := time.Now()
	listed := NewNoServingAccountError(NoCandidateReport{Model: "gemini-omni-1.1-flash", ModelListed: true, Listed: 3, Servable: 3}, now)
	if !errors.Is(listed, ErrNoEligibleAccount) || listed.HTTPStatus() != http.StatusServiceUnavailable || listed.RPCStatus() != "UNAVAILABLE" ||
		!listed.RetryAt().Equal(now.Add(noServingRetryAfter)) || !strings.Contains(listed.Error(), "目录列出=3") ||
		listed.PublicMessage() != "Model gemini-omni-1.1-flash is listed, but no account in the pool can serve this request right now. Please try again later." {
		t.Fatalf("模型在列: %v %q %v", listed, listed.PublicMessage(), listed.RetryAt())
	}
	unlisted := NewNoServingAccountError(NoCandidateReport{Model: "gemini-omni-1.1-flash", Listed: 3, Tier: 3}, now)
	if !errors.Is(unlisted, ErrNoEligibleAccount) || unlisted.HTTPStatus() != http.StatusNotFound || unlisted.RPCStatus() != "NOT_FOUND" ||
		!unlisted.RetryAt().IsZero() || strings.Contains(unlisted.PublicMessage(), "目录") ||
		unlisted.PublicMessage() != "models/gemini-omni-1.1-flash is not available to the accounts in this pool: "+
			"no account in this pool has a subscription tier that allows this model. Call ListModels to see the list of available models." {
		t.Fatalf("模型不在列: %v %q", unlisted, unlisted.PublicMessage())
	}
	for _, test := range []struct {
		report NoCandidateReport
		want   string
	}{
		{report: NoCandidateReport{}, want: "no account in this pool lists this model in its catalog"},
		{report: NoCandidateReport{Listed: 2, Method: 2}, want: "no account in this pool supports generating content with this model"},
		{report: NoCandidateReport{Listed: 2, Unsupported: 2, EnabledChannels: []string{"build"}}, want: "the enabled upstream channels (build) cannot serve this model"},
		{report: NoCandidateReport{Listed: 2, Tier: 1, Unsupported: 1}, want: "no account in this pool can serve this model with its subscription tier and the enabled upstream channels"},
	} {
		if got := test.report.UnlistedReason(); got != test.want {
			t.Fatalf("UnlistedReason(%+v) = %q，期望 %q", test.report, got, test.want)
		}
	}
	for _, test := range []struct {
		err  error
		want bool
	}{
		{err: ErrNoEligibleAccount, want: true},
		{err: fmt.Errorf("选号: %w", ErrNoEligibleAccount), want: true},
		{err: &AccountsNotReadyError{Reasons: []string{"a 需要重新登录"}}, want: false},
		{err: &BuildGenerationUnavailableError{Model: "m", NoCandidate: true}, want: false},
		{err: listed, want: false},
		{err: ErrModelNotFound, want: false},
		{err: nil, want: false},
	} {
		if got := NoCandidateFailure(test.err); got != test.want {
			t.Fatalf("NoCandidateFailure(%v) = %v，期望 %v", test.err, got, test.want)
		}
	}
}

// noCandidateTestPool 建立这些账户的账户池（disabled 中的账户停用），每个账户的目录由 catalog 给出
func noCandidateTestPool(t *testing.T, accounts []string, disabled string, catalog func(accountID string) []Model) *AccountPool {
	t.Helper()
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, 1)
	t.Cleanup(pool.WaitBackground)
	for _, accountID := range accounts {
		config := DefaultAccountConfig(accountID)
		config.Enabled = accountID != disabled
		account, publish, err := store.Create(config, testStorageState("S"))
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetCatalog(accountID, BenefitTierFree, catalog(accountID)); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

// TestNoCandidateReport 核对按目录列出该模型的账号统计原因：停用、不可调度、被排除、权益不符、所选通道不支持分别计数，写明可用通道、选择条件、
// 模型列表与候选索引；没有列出该模型的账号不计。权益不符、只启用 Build 时模型不在模型列表里（与 GET /v1/models 一致），不值得重试；
// 能接的账号是候选时索引与逐个核对一致，不作废索引
func TestNoCandidateReport(t *testing.T) {
	proOnly := Model{ID: "pro-only", Methods: []string{"generateContent"}, AccessModes: []int64{3}}
	plain := Model{ID: "plain", Methods: []string{"generateContent"}}
	accounts := []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com", "e@example.com"}
	pool := noCandidateTestPool(t, accounts, "a@example.com", func(accountID string) []Model {
		if accountID == "e@example.com" {
			return []Model{plain}
		}
		return []Model{proOnly}
	})
	report := pool.NoCandidateReport(AccountSelection{ModelID: "models/pro-only", Method: "generateContent", ExcludedAccountIDs: []string{"b@example.com"}})
	summary := report.Text()
	for _, fragment := range []string{
		"模型=pro-only", "可用通道=playground", "条件=无", "排除=1", "目录列出=4", "停用=1", "不在选号范围=1", "权益不符=2", "可接=0",
		"模型列表=不列出", "核对候选=索引 0、逐个账户 0", "Playground=启用", "Build=未启用",
	} {
		if !strings.Contains(summary, fragment) {
			t.Fatalf("摘要 %q 缺少 %q", summary, fragment)
		}
	}
	if report.ModelListed || report.Recheckable() || report.Rebuilt || report.UnlistedReason() != "no account in this pool has a subscription tier that allows this model" {
		t.Fatalf("权益不符: %+v", report)
	}
	report = pool.NoCandidateReport(AccountSelection{ModelID: "plain", Method: "generateContent", PlaygroundFirst: true})
	if summary := report.Text(); !strings.Contains(summary, "目录列出=1") || !strings.Contains(summary, "条件=PlaygroundFirst") ||
		!strings.Contains(summary, "可接=1") || !strings.Contains(summary, "模型列表=列出") || !strings.Contains(summary, "核对候选=索引 1、逐个账户 1") ||
		!report.ModelListed || !report.Recheckable() || report.Rebuilt {
		t.Fatalf("能接的账号是候选时索引与逐个核对一致: %+v %q", report, summary)
	}

	// 只启用 Build：Playground 才能调用的模型没有可用通道，不在模型列表里
	pool.SetUpstreamChannels([]Channel{ChannelBuild})
	report = pool.NoCandidateReport(AccountSelection{ModelID: "plain", Method: "generateContent"})
	if report.ModelListed || report.Recheckable() || report.Unsupported != 1 ||
		report.UnlistedReason() != "the enabled upstream channels (build) cannot serve this model" || !strings.Contains(report.Text(), "可用通道=build") {
		t.Fatalf("只启用 Build: %+v %q", report, report.Text())
	}
}

// desyncCandidateIndex 模拟候选索引滞后（只供本包测试；没有找到会造成这种状态的生产代码路径，见 no_serving.go 开头）：清空候选索引里
// 该模型各候选集的成员（不改账户，建立时间记为现在，不会因过期重建），返回清空的候选集数。之后按候选索引的选号没有候选，逐个账户核对仍有
func desyncCandidateIndex(p *AccountPool, modelID string) int {
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sched == nil {
		return 0
	}
	cleared := 0
	now := time.Now()
	for key, set := range p.sched.sets {
		if key.modelID != modelID {
			continue
		}
		clear(set.until)
		set.available, set.cooling, set.members, set.builtAt = set.available[:0], set.cooling[:0], 0, now
		cleared++
	}
	return cleared
}

// TestNoCandidateReportRebuildsStaleIndex 假设的候选索引滞后（候选集里没有成员、账户本身能接，由 desyncCandidateIndex 人为造成）时，
// 选号按候选索引没有候选；核对时逐个账户还有候选，作废候选索引，之后的选号重建索引、能取到账号。缓存期内同一选择条件沿用核对结果、
// 不再遍历账户，目录版本变化后重新核对。CandidateIndexSummary 与核对摘要里的候选索引概况一致
func TestNoCandidateReportRebuildsStaleIndex(t *testing.T) {
	plain := Model{ID: "plain", Methods: []string{"generateContent"}}
	pool := noCandidateTestPool(t, []string{"a@example.com", "b@example.com"}, "", func(string) []Model { return []Model{plain} })
	selection := AccountSelection{ModelID: "plain", Method: "generateContent"}
	count := func() int {
		groups, err := pool.ClassifyCandidatesCached(selection, nil)
		if err != nil {
			t.Fatal(err)
		}
		return candidateGroupCount(groups)
	}
	if got := count(); got != 2 {
		t.Fatalf("候选数 = %d", got)
	}
	if cleared := desyncCandidateIndex(pool, "models/plain"); cleared != 1 {
		t.Fatalf("清空的候选集 = %d", cleared)
	}
	if got := count(); got != 0 {
		t.Fatalf("候选索引滞后时候选数 = %d，期望 0", got)
	}
	if lease, _, err := pool.TryAcquireFor(context.Background(), selection); lease != nil || err != nil {
		t.Fatalf("候选索引滞后时不应取到租约: %v %v", lease, err)
	}
	if err := pool.NoEligibleError(selection); !NoCandidateFailure(err) {
		t.Fatalf("候选索引滞后时应为裸的没有候选: %v", err)
	}
	report := pool.NoCandidateReport(selection)
	summary := report.Text()
	if !report.Recheckable() || !report.Rebuilt || !report.ModelListed || report.IndexCandidates != 0 || report.BruteCandidates != 2 ||
		!report.IndexFound || report.IndexMembers != 0 || report.Servable != 2 ||
		!strings.Contains(summary, "候选索引=建立于") || !strings.Contains(summary, "成员 0（可用 0、冷却 0）") ||
		!strings.Contains(summary, "核对候选=索引 0、逐个账户 2") || !strings.Contains(summary, "已作废候选索引") {
		t.Fatalf("核对 = %+v %q", report, summary)
	}
	if got := count(); got != 2 {
		t.Fatalf("作废后重建的候选索引候选数 = %d，期望 2", got)
	}
	// 重建后的候选集概况（只读，不建立）：成员 2、可用 2；指定账户的选择不用候选集
	if index := pool.CandidateIndexSummary(AccountSelection{ModelID: "models/plain", Method: "generateContent"}); !index.Found ||
		index.Members != 2 || index.Available != 2 || index.Cooling != 0 || !strings.Contains(index.Text(), "成员 2（可用 2、冷却 0）") {
		t.Fatalf("候选集概况 = %+v %q", index, index.Text())
	}
	if index := pool.CandidateIndexSummary(AccountSelection{ModelID: "plain", Method: "generateContent", AccountID: "a@example.com"}); index.Found ||
		index.Text() != "无" {
		t.Fatalf("指定账户不应有候选集概况: %+v", index)
	}
	if index := pool.CandidateIndexSummary(AccountSelection{ModelID: "plain", Method: "generateContent", PlaygroundFirst: true}); index.Found {
		t.Fatalf("没建立过的候选集不应建立: %+v", index)
	}
	if got := SelectionFlagsText(AccountSelection{PlaygroundFirst: true, BuildProbe: true}); got != "PlaygroundFirst,BuildProbe" ||
		SelectionFlagsText(AccountSelection{}) != "无" {
		t.Fatalf("选择条件标记 = %q", got)
	}
	lease, _, err := pool.TryAcquireFor(context.Background(), selection)
	if err != nil || lease == nil {
		t.Fatalf("重建后应取到租约: %v %v", lease, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	// 缓存期内沿用：候选索引已一致时不再作废；候选索引又滞后时只按候选索引再分类一次，仍没有就再作废
	cached := pool.NoCandidateReport(selection)
	if cached.CachedAge <= 0 || cached.Rebuilt || cached.BruteCandidates != 2 || cached.IndexCandidates != 2 || !strings.Contains(cached.Text(), "沿用") {
		t.Fatalf("缓存期内应沿用核对结果: %+v", cached)
	}
	desyncCandidateIndex(pool, "plain")
	if again := pool.NoCandidateReport(selection); again.CachedAge <= 0 || !again.Rebuilt || again.IndexCandidates != 0 {
		t.Fatalf("沿用的核对遇到候选索引又滞后时应再作废: %+v", again)
	}
	if got := count(); got != 2 {
		t.Fatalf("再次作废后重建的候选索引候选数 = %d，期望 2", got)
	}
	if err := pool.SetBuildCatalog("a@example.com", []Model{plain}); err != nil {
		t.Fatal(err)
	}
	if fresh := pool.NoCandidateReport(selection); fresh.CachedAge != 0 || fresh.Rebuilt || fresh.IndexCandidates != 2 {
		t.Fatalf("目录版本变化后应重新核对，索引一致时不作废: %+v", fresh)
	}
}

// TestOmniCatalogRejectsFunctionTools 线上形状的 Omni 目录项（能力码 1、25、52、59，没有 10 function_declarations）带函数声明时，
// 发送前的工具校验返回中文原因（对外换成通用的 “Request contains an invalid argument.”）：app 层对 Interaction 模型去掉工具的原因；
// InteractionToolsNote 只在带工具时给出说明
func TestOmniCatalogRejectsFunctionTools(t *testing.T) {
	raw, _ := json.Marshal([]any{[]any{omniCatalogRow([]any{nil, nil, nil, nil, []int{1, 2}, nil, nil, nil, []int{5, 1}}, []int{1, 2})}})
	catalog, err := parseModelCatalog(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	model := catalog.entries["gemini-omni-1.1-flash"].model
	if !model.Capabilities["interactions_api"] || model.Capabilities["function_declarations"] {
		t.Fatalf("Omni 目录能力 = %v", model.Capabilities)
	}
	tools := Tools{Functions: []FunctionDeclaration{{Name: "save_memory"}, {Name: "delete_memory"}}}
	if err := validateRequestedTools(tools, model); err == nil || !strings.Contains(err.Error(), "不支持 function declarations") {
		t.Fatalf("带函数声明应被目录校验拒绝，得到 %v", err)
	}
	if note := InteractionToolsNote(tools); note != "CreateInteractionStream 不带工具，已忽略 2 个函数声明与 0 个 Google 工具（Omni 模型不调用工具）" {
		t.Fatalf("说明 = %q", note)
	}
	if note := InteractionToolsNote(Tools{GoogleSearch: &GoogleSearchOptions{WebSearch: true}}); !strings.Contains(note, "0 个函数声明与 1 个 Google 工具") {
		t.Fatalf("说明 = %q", note)
	}
	if note := InteractionToolsNote(Tools{ToolConfig: ToolConfig{Mode: "auto"}}); note != "" {
		t.Fatalf("没有工具时不应有说明: %q", note)
	}
}
