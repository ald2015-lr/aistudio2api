package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 权益规则（ULTRA_ONLY_MODELS、PAID_ONLY_MODELS，判断见 aistudio/tier_rules.go）在 app 层的测试：额度用完改用其他模型的目标、
// 主用模型供给、热更新与提示、规则排除的请求对外结果、Build 独有模型不触发目录补拉

const (
	tierRulePro   = "tier-rule-pro@example.com"
	tierRuleUltra = "tier-rule-ultra@example.com"
)

// TestFallbackArgonToCarbonSkipsPro ULTRA_INCLUDE_PRO=true、目录没有给 carbon 标注 [4]、carbon 在 ULTRA_ONLY_MODELS 里：/ultra 上 argon
// 全部冷却后按 MODEL_FALLBACKS 改用的 carbon 只落到 Ultra 账户；Ultra 号池只有 Pro 时 carbon 不是可用的目标，按 argon 原来的 429 结束、
// 不向上游发送
func TestFallbackArgonToCarbonSkipsPro(t *testing.T) {
	t.Run("Pro 与 Ultra", func(t *testing.T) {
		accounts := []string{tierRulePro, tierRuleUltra}
		upstream := &fallbackUpstream{}
		service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
		if err := pool.SetCatalog(tierRulePro, aistudio.BenefitTierPro, fallbackCatalog()); err != nil {
			t.Fatal(err)
		}
		pool.SetUltraIncludePro(true)
		pool.SetModelTierRules([]string{fallbackTarget}, nil)
		coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
		for round := range 4 {
			result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{ID: fmt.Sprintf("req-tier-%d", round), Model: fallbackSource})
			if result.err != nil || result.served != fallbackTarget {
				t.Fatalf("第 %d 次：err=%v served=%q，期望改用 carbon 成功", round, result.err, result.served)
			}
		}
		upstream.mu.Lock()
		calls := slices.Clone(upstream.calls)
		upstream.mu.Unlock()
		if len(calls) != 4 {
			t.Fatalf("上游收到 %d 次请求，期望 4 次 carbon", len(calls))
		}
		for _, call := range calls {
			if call.model != fallbackTarget || call.account != tierRuleUltra {
				t.Fatalf("改用的 carbon 落到 %s（模型 %s），期望只有 Ultra 账户", call.account, call.model)
			}
		}
		assertNoBusyAccounts(t, pool)
	})
	t.Run("只有 Pro", func(t *testing.T) {
		upstream := &fallbackUpstream{}
		service, pool := fallbackService(t, upstream, aistudio.BenefitTierPro, config.ModelFallbackScopeUltra, tierRulePro)
		pool.SetUltraIncludePro(true)
		pool.SetModelTierRules([]string{fallbackTarget}, nil)
		coolAll(t, pool, fallbackSource, 3*time.Hour, tierRulePro)
		result := runFallbackRequest(t, service, aistudio.PoolScopeUltra, aistudio.GenerateRequest{Model: fallbackSource})
		var cooling *aistudio.AllCoolingError
		if !errors.As(result.err, &cooling) || cooling.ModelID != fallbackSource || len(upstream.models()) != 0 {
			t.Fatalf("err=%v 上游=%v，期望按 argon 的全部冷却结束、不发送", result.err, upstream.models())
		}
		if !progressLogged(service, "MODEL_FALLBACKS 的目标 "+fallbackTarget+" 不在这个号池的模型目录里，跳过") {
			t.Fatal("日志应写明 carbon 不在这个号池的模型目录里")
		}
	})
}

// tierRuleEnv 为热更新与主用模型供给的测试环境：Free、Pro（并入 Ultra 号池）、Ultra 各一个账户，目录都只有 testRuntimeModel（没有标注
// 访问模式），Ultra 主用模型为 testRuntimeModel；Pro 与 Ultra 各有一个驻留 Worker。.env 与生效配置一开始都没有权益规则
func tierRuleEnv(t *testing.T) *includeProRuntime {
	t.Helper()
	for _, key := range []string{"ULTRA_ONLY_MODELS", "PAID_ONLY_MODELS"} {
		t.Setenv(key, "")
	}
	env := newIncludeProRuntime(t)
	env.cfg.UltraIncludePro, env.cfg.UltraOnlyModels, env.cfg.PaidOnlyModels = true, []string{}, []string{}
	env.cfg.UltraWarmWorkerLimit, env.cfg.UltraMaxActiveWorkers = 2, 2
	if err := env.cfg.Save(env.path); err != nil {
		t.Fatal(err)
	}
	env.manager.current.config = env.cfg
	env.admin.config = env.cfg
	env.workers.setUltraCapacity(2, 2)
	env.pool.SetUltraIncludePro(true)
	for _, accountID := range []string{includeProAccount, partitionUltraA} {
		if _, err := env.workers.promote(env.ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

// saveTierRules 经管理接口保存权益规则（nil 表示旧版页面不带这个字段），按页面的方式立即热更新
func (env *includeProRuntime) saveTierRules(t *testing.T, ultraOnly *[]string, paidOnly *[]string) (api.RuntimeConfig, error) {
	t.Helper()
	value := runtimeConfigDTO(env.manager.current.config)
	value.UltraOnlyModels, value.PaidOnlyModels = ultraOnly, paidOnly
	return env.manager.UpdateRuntimeConfig(env.ctx, value)
}

// ultraSupplyWarm 返回 Ultra 主用模型 testRuntimeModel 的驻留供给
func ultraSupplyWarm(t *testing.T, workers *accountWorkerManager) int {
	t.Helper()
	for _, entry := range workers.primarySupplyStatus() {
		if entry.Pool == "ultra" && entry.Model == testRuntimeModel {
			return entry.Warm
		}
	}
	t.Fatalf("缺少 Ultra 主用模型供给: %+v", workers.primarySupplyStatus())
	return 0
}

// TestHotConfigAppliesTierRules 管理页保存权益规则后立即生效、不要求重启：账户池的规则、生效配置与 .env 一致，运行日志写一行生效与
// 热更新摘要；主用模型供给在下一个 keepWarm 周期按新规则重算（不等满 30 秒），Pro 的驻留 Worker 对规则排除的模型不再计入；
// 旧版页面不带这些字段时沿用现值；long- 开头的名称按 400 invalid_config 拒绝、不改动生效的规则；清空后恢复
func TestHotConfigAppliesTierRules(t *testing.T) {
	env := tierRuleEnv(t)
	start := time.Now()
	if !env.workers.refreshPrimarySupply(start) || ultraSupplyWarm(t, env.workers) != 2 {
		t.Fatalf("没有规则时 Ultra 主用模型的驻留供给 = %d，期望 2（Pro 与 Ultra）", ultraSupplyWarm(t, env.workers))
	}

	rules := []string{testRuntimeModel}
	updated, err := env.saveTierRules(t, &rules, &[]string{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServiceRestartRequired || updated.UltraOnlyModels == nil || !slices.Equal(*updated.UltraOnlyModels, rules) {
		t.Fatalf("保存结果 restart=%t ultra_only_models=%v", updated.ServiceRestartRequired, updated.UltraOnlyModels)
	}
	if ultraOnly, paidOnly := env.pool.ModelTierRules(); !slices.Equal(ultraOnly, rules) || len(paidOnly) != 0 {
		t.Fatalf("账户池的规则 = %v / %v", ultraOnly, paidOnly)
	}
	if saved, err := config.Load(env.path); err != nil || !slices.Equal(saved.UltraOnlyModels, rules) ||
		!slices.Equal(env.manager.current.config.UltraOnlyModels, rules) {
		t.Fatalf(".env = %v（%v），生效配置 = %v", saved.UltraOnlyModels, err, env.manager.current.config.UltraOnlyModels)
	}
	if !env.workers.refreshPrimarySupply(start.Add(time.Second)) || ultraSupplyWarm(t, env.workers) != 1 {
		t.Fatalf("规则生效后下一次 keepWarm 应重算主用模型供给，Ultra 驻留数 = %d，期望 1", ultraSupplyWarm(t, env.workers))
	}
	for _, fragments := range [][]string{
		{"权益规则已更新并立即生效 | 仅 Ultra " + testRuntimeModel + "；仅付费 无"},
		{"配置已热更新", "权益规则=仅 Ultra " + testRuntimeModel + "；仅付费 无"},
	} {
		if !hasLogContaining(env.requests, fragments...) {
			t.Fatalf("缺少日志 %q，日志 = %v", fragments, serviceLogMessages(env.requests))
		}
	}

	// 旧版页面不带这两个字段：沿用现值
	if _, err := env.saveTierRules(t, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ultraOnly, _ := env.pool.ModelTierRules(); !slices.Equal(ultraOnly, rules) {
		t.Fatalf("旧版页面保存后规则 = %v，期望沿用 %v", ultraOnly, rules)
	}
	// long- 开头的名称：400 invalid_config，生效的规则不变
	_, err = env.saveTierRules(t, &[]string{"long-" + testRuntimeModel}, nil)
	var operation *adminOperationError
	if !errors.As(err, &operation) || operation.status != http.StatusBadRequest || operation.code != "invalid_config" ||
		!strings.Contains(operation.message, "ULTRA_ONLY_MODELS") {
		t.Fatalf("long- 开头的名称应按 400 invalid_config 拒绝：%v", err)
	}
	if ultraOnly, _ := env.pool.ModelTierRules(); !slices.Equal(ultraOnly, rules) {
		t.Fatalf("拒绝保存后规则 = %v，期望不变", ultraOnly)
	}

	if _, err := env.saveTierRules(t, &[]string{}, &[]string{}); err != nil {
		t.Fatal(err)
	}
	if ultraOnly, paidOnly := env.pool.ModelTierRules(); len(ultraOnly)+len(paidOnly) != 0 {
		t.Fatalf("清空后规则 = %v / %v", ultraOnly, paidOnly)
	}
	if !env.workers.refreshPrimarySupply(start.Add(2*time.Second)) || ultraSupplyWarm(t, env.workers) != 2 {
		t.Fatalf("清空后 Ultra 驻留数 = %d，期望 2", ultraSupplyWarm(t, env.workers))
	}
	if got := countLogs(env.requests, "权益规则已更新并立即生效"); got != 2 {
		t.Fatalf("规则实际变化两次，生效日志 %d 条", got)
	}
}

// TestHotConfigAppliesPaidOnlyRule 只改 PAID_ONLY_MODELS（ULTRA_ONLY_MODELS 不变）的保存同样立即生效、不要求重启：保存结果、账户池、
// 生效配置与 .env 都是新值，运行日志写一行生效；Free 账户立即不再能服务列表里的模型（Pro 与 Ultra 照常），清空后恢复。
// 热更新判断（liveFieldsEqual）与生效配置比较（sameDataConfig）分别包含两份列表，只改其中一份也算变化
func TestHotConfigAppliesPaidOnlyRule(t *testing.T) {
	env := tierRuleEnv(t)
	accounts := []string{partitionNormalA, includeProAccount, partitionUltraA}
	serveable := func() map[string]int {
		return env.pool.ServeableCounts(accounts, []string{testRuntimeModel}, 0)
	}
	if counts := serveable(); counts[partitionNormalA] != 1 || counts[includeProAccount] != 1 || counts[partitionUltraA] != 1 {
		t.Fatalf("没有规则时可服务数 = %v，期望三个账户都是 1", counts)
	}
	rules := []string{testRuntimeModel}
	updated, err := env.saveTierRules(t, &[]string{}, &rules)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServiceRestartRequired || updated.PaidOnlyModels == nil || !slices.Equal(*updated.PaidOnlyModels, rules) ||
		updated.UltraOnlyModels == nil || len(*updated.UltraOnlyModels) != 0 {
		t.Fatalf("保存结果 restart=%t paid_only_models=%v ultra_only_models=%v", updated.ServiceRestartRequired, updated.PaidOnlyModels, updated.UltraOnlyModels)
	}
	if ultraOnly, paidOnly := env.pool.ModelTierRules(); len(ultraOnly) != 0 || !slices.Equal(paidOnly, rules) {
		t.Fatalf("账户池的规则 = %v / %v，期望只有付费规则 %v", ultraOnly, paidOnly, rules)
	}
	if saved, err := config.Load(env.path); err != nil || !slices.Equal(saved.PaidOnlyModels, rules) ||
		!slices.Equal(env.manager.current.config.PaidOnlyModels, rules) {
		t.Fatalf(".env = %v（%v），生效配置 = %v", saved.PaidOnlyModels, err, env.manager.current.config.PaidOnlyModels)
	}
	if counts := serveable(); counts[partitionNormalA] != 0 || counts[includeProAccount] != 1 || counts[partitionUltraA] != 1 {
		t.Fatalf("付费规则生效后可服务数 = %v，期望 Free 0、Pro 与 Ultra 1", counts)
	}
	if !hasLogContaining(env.requests, "权益规则已更新并立即生效 | 仅 Ultra 无；仅付费 "+testRuntimeModel) {
		t.Fatalf("缺少生效日志，日志 = %v", serviceLogMessages(env.requests))
	}
	if _, err := env.saveTierRules(t, &[]string{}, &[]string{}); err != nil {
		t.Fatal(err)
	}
	if _, paidOnly := env.pool.ModelTierRules(); len(paidOnly) != 0 || serveable()[partitionNormalA] != 1 {
		t.Fatalf("清空付费规则后账户池的规则 = %v，Free 可服务数 = %d", paidOnly, serveable()[partitionNormalA])
	}

	active := config.Default()
	for name, change := range map[string]func(*config.Config){
		"ULTRA_ONLY_MODELS": func(cfg *config.Config) { cfg.UltraOnlyModels = []string{"gemini-other"} },
		"PAID_ONLY_MODELS":  func(cfg *config.Config) { cfg.PaidOnlyModels = []string{"gemini-other"} },
	} {
		changed := active
		change(&changed)
		if liveFieldsEqual(changed, active) || requiresRebuild(changed, active) {
			t.Fatalf("只改 %s 应按热更新处理", name)
		}
		dto := runtimeConfigDTO(changed)
		if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
			t.Fatalf("生效配置比较应包含 %s", name)
		}
		dto.UltraOnlyModels, dto.PaidOnlyModels = nil, nil
		if !sameDataConfig(dto, active, dataConfigOverrides{}) {
			t.Fatalf("旧版页面不带 %s 时按生效值比较", name)
		}
	}
}

// countLogsAt 返回级别为 level、包含全部 fragments 的服务运行日志条数
func countLogsAt(requests *requestRegistry, level string, fragments ...string) int {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	count := 0
	for _, entry := range requests.logs {
		matched := entry.Level == level
		for _, fragment := range fragments {
			matched = matched && strings.Contains(entry.Message, fragment)
		}
		if matched {
			count++
		}
	}
	return count
}

// TestTierRuleHintsSurfaced 规则与账户目录的对照：保存后的结果与读取设置都带只读提示。目录没有给规则模型标注权益限制（Free、Pro）时
// 只按配置项与模型合并写一行 INFO“权益规则在起作用”、不建议移除（规则本来就是为这种目录设的）；Pro 账户的目录标注了 Pro 可用（[3]）后
// 写一行 WARN“权益规则比账户目录更严”并建议确认后移除；都不含账户，同样的内容不重复写；规则里不在任何目录里的名称写一行 WARN
func TestTierRuleHintsSurfaced(t *testing.T) {
	env := tierRuleEnv(t)
	rules := []string{testRuntimeModel, "missing-model-high"}
	updated, err := env.saveTierRules(t, &rules, &[]string{})
	if err != nil {
		t.Fatal(err)
	}
	want := []api.TierRuleHint{
		{Key: "ULTRA_ONLY_MODELS", Model: testRuntimeModel, Tier: "Free", Accounts: 1},
		{Key: "ULTRA_ONLY_MODELS", Model: testRuntimeModel, Tier: "Pro", Accounts: 1},
	}
	if !slices.Equal(updated.TierRuleHints, want) {
		t.Fatalf("保存结果的提示 = %+v，期望 %+v", updated.TierRuleHints, want)
	}
	read, err := env.manager.RuntimeConfig(env.ctx)
	if err != nil || !slices.Equal(read.TierRuleHints, want) {
		t.Fatalf("读取设置的提示 = %+v（%v）", read.TierRuleHints, err)
	}
	unmarkedLine := "权益规则在起作用 | 配置项=ULTRA_ONLY_MODELS | 模型=" + testRuntimeModel + " | 目录没有标注权益限制=Free 1 个账户、Pro 1 个账户 | "
	unmatchedLine := "规则模型目前不在任何账户目录里（带后缀的名称要写成基础模型 ID） | 配置项=ULTRA_ONLY_MODELS | 模型=missing-model-high"
	// 保存后的热更新立即对照（不等 keepWarm）：没有标注的合并成一行 INFO，不写“比目录更严”的 WARN；不匹配的名称一行 WARN
	check := func(stage string, unmarked int, marked int) {
		t.Helper()
		if countLogsAt(env.requests, "INFO", unmarkedLine) != unmarked || countLogsAt(env.requests, "WARN", unmatchedLine) != 1 ||
			countLogs(env.requests, "权益规则比账户目录更严") != marked || countLogs(env.requests, "权益规则在起作用") != unmarked {
			t.Fatalf("%s：日志 = %v", stage, serviceLogMessages(env.requests))
		}
	}
	check("保存后", 1, 0)
	for range 3 {
		env.service.checkTierRules()
	}
	check("重复对照", 1, 0)
	// Pro 账户的目录标注了 Pro 可用：多半是 Google 已开放，写一行 WARN 建议确认后移除；Free 仍没有标注，合并成新的一行 INFO
	if err := env.pool.SetCatalog(includeProAccount, aistudio.BenefitTierPro, []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}, AccessModes: []int64{3},
	}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		env.service.checkTierRules()
	}
	markedLine := "权益规则比账户目录更严 | 配置项=ULTRA_ONLY_MODELS | 模型=" + testRuntimeModel + " | 权益=Pro | 账户数=1 | 目录标注了 Pro 可用 | 确认 Pro 账户能用后可从 ULTRA_ONLY_MODELS 移除"
	freeOnly := "权益规则在起作用 | 配置项=ULTRA_ONLY_MODELS | 模型=" + testRuntimeModel + " | 目录没有标注权益限制=Free 1 个账户 | "
	if countLogsAt(env.requests, "WARN", markedLine) != 1 || countLogsAt(env.requests, "INFO", freeOnly) != 1 || countLogs(env.requests, "权益规则比账户目录更严") != 1 {
		t.Fatalf("Pro 目录标注可用后：日志 = %v", serviceLogMessages(env.requests))
	}
	read, err = env.manager.RuntimeConfig(env.ctx)
	wantMarked := []api.TierRuleHint{want[0], {Key: "ULTRA_ONLY_MODELS", Model: testRuntimeModel, Tier: "Pro", Marked: true, Accounts: 1}}
	if err != nil || !slices.Equal(read.TierRuleHints, wantMarked) {
		t.Fatalf("Pro 目录标注可用后读取设置的提示 = %+v（%v）", read.TierRuleHints, err)
	}
	for _, message := range serviceLogMessages(env.requests) {
		if strings.Contains(message, "权益规则") && strings.Contains(message, "@example.com") {
			t.Fatalf("规则提示不应写账户：%s", message)
		}
	}
	// Ultra 账户的目录不产生提示；去掉规则后提示消失，读取设置不再带
	if _, err := env.saveTierRules(t, &[]string{}, &[]string{}); err != nil {
		t.Fatal(err)
	}
	if read, err := env.manager.RuntimeConfig(env.ctx); err != nil || len(read.TierRuleHints) != 0 {
		t.Fatalf("清空规则后提示 = %+v（%v）", read.TierRuleHints, err)
	}
}

// countingModelsService 记下 Models 的调用次数（别名目录的同步重算都经它）
type countingModelsService struct {
	*trackedService
	models atomic.Int64
}

func (service *countingModelsService) Models(ctx context.Context) ([]aistudio.Model, error) {
	service.models.Add(1)
	return service.trackedService.Models(ctx)
}

// tierRuleOutcome 为一次规则或目录排除的请求在各入口的结果
type tierRuleOutcome struct {
	status    int
	body      string
	listed    string
	pinned    string
	bound     string
	modelSync int64
}

// runTierRuleOutcome 只有一个 Pro 账户（并入 Ultra 号池）、它的目录有 carbon 时，按 carbon 经 /ultra 发出：公开 API 的对话请求（三次，
// 同一个处理链）、/ultra/v1/models、指定账户的请求、绑定该账户文件的请求，记下各自的结果
func runTierRuleOutcome(t *testing.T, carbon aistudio.Model, rules []string) tierRuleOutcome {
	t.Helper()
	service, pool := runningGenerateService(t, &fallbackUpstream{}, context.Background(), time.Minute, tierRulePro)
	catalog := []aistudio.Model{carbon, fallbackCatalog()[0]}
	if err := pool.SetCatalog(tierRulePro, aistudio.BenefitTierPro, catalog); err != nil {
		t.Fatal(err)
	}
	service.replaceModelSnapshot(catalog)
	pool.SetUltraIncludePro(true)
	pool.SetModelTierRules(rules, nil)
	counting := &countingModelsService{trackedService: service}
	admin := &accessLogCapture{}
	handler := api.NewHandler(counting, api.Config{APIKey: "sk-test", Admin: admin})
	var outcome tierRuleOutcome
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, fallbackTarget)
	for round := range 3 {
		recorder, _ := postFallbackTo(t, handler, admin, "/ultra/v1/chat/completions", body)
		if round == 0 {
			outcome.status, outcome.body = recorder.Code, recorder.Body.String()
		} else if recorder.Code != outcome.status || recorder.Body.String() != outcome.body {
			t.Fatalf("第 %d 次结果与第一次不同：%d %s", round, recorder.Code, recorder.Body.String())
		}
	}
	outcome.modelSync = counting.models.Load()
	ids := make([]string, 0)
	for _, model := range pool.EligibleModelsIn(aistudio.PoolScopeUltra, service.modelSnapshot()) {
		ids = append(ids, model.ID)
	}
	outcome.listed = strings.Join(ids, ",")
	finalError := func(request aistudio.GenerateRequest) string {
		ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), 10*time.Second)
		defer cancel()
		events, err := service.Generate(ctx, request)
		final := err
		if err == nil {
			for event := range events {
				if event.Kind == aistudio.EventError {
					final = event.Err
				}
			}
		}
		if final == nil {
			t.Fatalf("%s：请求应失败", request.ID)
		}
		var noServing *aistudio.NoServingAccountError
		return fmt.Sprintf("%t|%t|%s", aistudio.NoCandidateFailure(final), errors.As(final, &noServing), final.Error())
	}
	contents := []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}}
	outcome.pinned = finalError(aistudio.GenerateRequest{ID: "req-pinned", Model: fallbackTarget, AccountID: tierRulePro, Contents: contents})
	if err := pool.BindResourceKind("files/tier-rule", tierRulePro, "drive-file"); err != nil {
		t.Fatal(err)
	}
	outcome.bound = finalError(aistudio.GenerateRequest{ID: "req-bound", Model: fallbackTarget, Contents: []aistudio.Content{{
		Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}, {File: &aistudio.FileRef{ID: "files/tier-rule"}}},
	}}})
	return outcome
}

// TestTierRuleExcludedRequestMirrorsCatalogTier 规则排除了号池里全部账户的请求，对外结果与目录标注 [4]、号池里没有 Ultra 账户时完全相同：
// 公开 API 立即 404 NOT_FOUND / model_not_found（写明权益原因、不说稍后重试），/ultra/v1/models 不列出；指定账户、绑定文件的请求照原来的
// 无账户错误返回；别名目录的同步重算次数相同（不在列的名称同一号池每 10 秒最多重算一次）。不向上游发送
func TestTierRuleExcludedRequestMirrorsCatalogTier(t *testing.T) {
	marked := fallbackCatalog()[1]
	marked.AccessModes = []int64{4}
	catalogTier := runTierRuleOutcome(t, marked, nil)
	ruled := runTierRuleOutcome(t, fallbackCatalog()[1], []string{fallbackTarget})
	t.Logf("结果：%+v", ruled)
	if catalogTier != ruled {
		t.Fatalf("规则排除的结果与目录 [4] 不同：\n目录 [4]：%+v\n规则：%+v", catalogTier, ruled)
	}
	want := "models/" + fallbackTarget + " is not available to the accounts in this pool: no account in this pool has a subscription tier that allows this model."
	if ruled.status != http.StatusNotFound || !strings.Contains(ruled.body, want) || !strings.Contains(ruled.body, `"code":"model_not_found"`) ||
		strings.Contains(ruled.body, "try again later") {
		t.Fatalf("对外结果 %d %s", ruled.status, ruled.body)
	}
	if ruled.listed != fallbackSource {
		t.Fatalf("/ultra 模型列表 = %q，期望只有 argon", ruled.listed)
	}
	for _, final := range []string{ruled.pinned, ruled.bound} {
		if !strings.HasPrefix(final, "true|false|") {
			t.Fatalf("指定账户与绑定文件的请求应照原来的无账户错误返回：%s", final)
		}
	}
	if ruled.modelSync > 2 {
		t.Fatalf("三次请求同步重算别名目录 %d 次", ruled.modelSync)
	}
}

// TestTierRuleExcludedBuildOnlyNoMissingProbe 规则排除的 Build 独有模型不按“目录中没有这个模型”处理，不触发后台读目录
func TestTierRuleExcludedBuildOnlyNoMissingProbe(t *testing.T) {
	env, catalog, _ := newProbeTestEnv(t)
	pool := env.gen.pool
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	if err := pool.SetBuildCatalog("pro@example.com", []aistudio.Model{{
		ID: fallbackTarget, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	pool.SetModelTierRules([]string{fallbackTarget}, nil)
	manager := newAccountWorkerManager(pool, nil, env.service.requests, "", "", time.Minute, 1, 1, 1, false)
	t.Cleanup(func() { _ = manager.Close() })
	env.service.workers = manager
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := env.service.acquireWarmLease(ctx, missingSelection(fallbackTarget))
	if err == nil || errors.Is(err, aistudio.ErrModelNotFound) || !errors.Is(err, aistudio.ErrNoEligibleAccount) {
		t.Fatalf("规则排除的 Build 独有模型应按没有符合条件的账户返回，实际 %v", err)
	}
	env.syncer.waitProbe()
	if reads := catalog.readAccounts(); len(reads) != 0 || len(probeLogLines(env.service)) != 0 {
		t.Fatalf("不应读目录：%v", reads)
	}
}

// TestConfigurePoolSetsTierRules 启动时 configurePool 在建立候选集之前把权益规则写进账户池（默认 carbon 只限 Ultra、pro-preview 只限付费）
func TestConfigurePoolSetsTierRules(t *testing.T) {
	pool, _ := testAccountPool(t, tierRulePro)
	configurePool(pool, config.Default())
	if ultraOnly, paidOnly := pool.ModelTierRules(); !slices.Equal(ultraOnly, []string{"gemini-4-carbon"}) ||
		!slices.Equal(paidOnly, []string{"gemini-3.1-pro-preview"}) {
		t.Fatalf("启动后账户池的规则 = %v / %v，期望默认列表", ultraOnly, paidOnly)
	}
}

// TestKeepWarmLogsTierRuleHints 不经配置保存、只是账户目录变化时，keepWarm 的周期同样对照规则与目录：Free 账户的目录没有给规则模型
// 标注权益限制时写一行 INFO，之后的周期不重复，只是账户数变化（停用后再启用）也不重复；目录改成标注 Ultra 专用（[4]）后提示消失，
// 再改回来时重新写
func TestKeepWarmLogsTierRuleHints(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com"}
	env := newDrainEnv(t, accounts, 1, 2)
	env.pool.SetModelTierRules([]string{drainProModel}, nil)
	line := "权益规则在起作用 | 配置项=ULTRA_ONLY_MODELS | 模型=" + drainProModel + " | 目录没有标注权益限制=Free 2 个账户"
	for range 2 {
		env.service.keepWarmPass(env.ctx, time.Now())
	}
	if got := countLogsAt(env.requests, "INFO", line); got != 1 {
		t.Fatalf("keepWarm 两个周期后提示 %d 行，期望 1，日志 = %v", got, serviceLogMessages(env.requests))
	}
	// 只是账户数变化（停用、启用账户，目录陆续载入）：同一配置项、模型与权益的 INFO 不重复写
	for _, enabled := range []bool{false, true} {
		if _, _, err := env.pool.SetEnabled(accounts[1], enabled); err != nil {
			t.Fatal(err)
		}
		env.service.keepWarmPass(env.ctx, time.Now())
	}
	prefix := "权益规则在起作用 | 配置项=ULTRA_ONLY_MODELS | 模型=" + drainProModel + " | "
	if got := countLogsAt(env.requests, "INFO", prefix); got != 1 {
		t.Fatalf("只是账户数变化后提示 %d 行，期望仍为 1，日志 = %v", got, serviceLogMessages(env.requests))
	}
	setCatalog := func(modes ...int64) {
		t.Helper()
		for _, accountID := range accounts {
			if err := env.pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{
				{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
				{ID: drainProModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}, AccessModes: modes},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	setCatalog(4)
	env.service.keepWarmPass(env.ctx, time.Now())
	setCatalog()
	env.service.keepWarmPass(env.ctx, time.Now())
	if got := countLogsAt(env.requests, "INFO", line); got != 2 {
		t.Fatalf("目录先标注 [4] 再去掉后提示 %d 行，期望再写一次（共 2 行）", got)
	}
}

// TestTierRuleClearSuffixedNameImmediate 规则排除 carbon 时 /ultra 的别名快照里没有它；清除规则后（与热更新同一个调用）带后缀的名称
// 立即可用，不等快照建立满 10 秒：快照记下号池规则版本，规则变过后解析不到的名称当场重算（生产中公开 API 经 runtimeManager 读到版本）
func TestTierRuleClearSuffixedNameImmediate(t *testing.T) {
	const accountID = "tier-rule-suffix@example.com"
	service, pool := runningGenerateService(t, &fallbackUpstream{}, context.Background(), time.Minute, accountID)
	if err := pool.SetCatalog(accountID, aistudio.BenefitTierPro, fallbackCatalog()); err != nil {
		t.Fatal(err)
	}
	service.replaceModelSnapshot(fallbackCatalog())
	pool.SetUltraIncludePro(true)
	pool.SetModelTierRules([]string{fallbackTarget}, nil)
	manager := &runtimeManager{current: &runtimeGeneration{service: service}}
	admin := &accessLogCapture{}
	handler := api.NewHandler(manager, api.Config{APIKey: "sk-test", Admin: admin})
	post := func(model string) int {
		recorder, _ := postFallbackTo(t, handler, admin, "/ultra/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model))
		return recorder.Code
	}
	if low, base := post(fallbackTarget+"-low"), post(fallbackTarget); low != http.StatusNotFound || base != http.StatusNotFound {
		t.Fatalf("规则排除时：%s-low=%d、%s=%d，期望都是 404", fallbackTarget, low, fallbackTarget, base)
	}
	version := manager.PoolRuleVersion()
	pool.SetModelTierRules(nil, nil)
	if manager.PoolRuleVersion() != version+1 {
		t.Fatalf("清除规则后 runtimeManager 读到的号池规则版本 = %d，期望 %d", manager.PoolRuleVersion(), version+1)
	}
	if low := post(fallbackTarget + "-low"); low != http.StatusOK {
		t.Fatalf("清除规则后带后缀的名称应立即可用：%s-low=%d", fallbackTarget, low)
	}
}
