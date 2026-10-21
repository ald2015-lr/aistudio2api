package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// normalPrimaryWait 返回模型在普通号池是否为主用模型及其排队上限
func normalPrimaryWait(service *trackedService, model string) (time.Duration, bool) {
	policy, listed := service.primaryPolicyFor(aistudio.PoolScopeNormal, model)
	return policy.queueWait, listed
}

// normalPoolContext 返回普通路径（ULTRA_EXCLUSIVE=true）请求的 context
func normalPoolContext() context.Context {
	return aistudio.ContextWithPoolScope(context.Background(), aistudio.PoolScopeNormal)
}

// normalSupplyTargets 返回只统计普通主用模型供给的目标
func normalSupplyTargets(models ...string) []primarySupplyTarget {
	return primarySupplyTargets(models, nil)
}

// setUltraPrimaryModels 只替换 Ultra 主用模型列表，普通列表保持不变
func setUltraPrimaryModels(service *trackedService, models []string, queueWait time.Duration) {
	current := service.primaryModels.Load()
	normal := primaryModelList{}
	if current != nil {
		normal = current.normal
	}
	service.storePrimaryModels(&primaryModelSettings{normal: normal, ultra: newPrimaryModelList(models, queueWait)})
}

// markUltra 把账户的权益改为 Ultra（目录只有 testRuntimeModel），账户随即属于 Ultra 号池与 Ultra 分区
func markUltra(t *testing.T, pool *aistudio.AccountPool, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierUltra, []aistudio.Model{{
			ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
}

// runScopedPrimaryRequest 在号池 scope 内执行一次生成（与 runPrimaryRequest 相同，请求 context 带号池）
func runScopedPrimaryRequest(t *testing.T, service *trackedService, scope aistudio.PoolScope, requestID string, resourceID string) (string, error, time.Duration) {
	t.Helper()
	return runPrimaryRequestWith(t, service, aistudio.ContextWithPoolScope(service.lifecycle, scope), requestID, resourceID)
}

// runPrimaryRequestWith 以 clientCtx（带号池、写出响应头前的等待延长记录等请求标记）为客户端 context 执行一次生成，
// 请求 context 由它派生
func runPrimaryRequestWith(t *testing.T, service *trackedService, clientCtx context.Context, requestID string, resourceID string) (string, error, time.Duration) {
	t.Helper()
	request := aistudio.GenerateRequest{
		ID: requestID, Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	requestCtx, requestCancel := context.WithTimeout(clientCtx, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	startedAt := time.Now()
	go service.generateWithRetry(clientCtx, requestCtx, requestCancel, startedAt, request, resourceID, events, newGenerationDiagnostics(request, "未发送"))
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

// ultraPrimaryUpstream 是 Ultra 主用模型测试的上游：quota 中的账号返回额度 429（quotaErr 非 nil 时返回它给出的错误，否则为每日限额），
// inputLimit 中的返回以 429 写的输入超限，coolingDown 中的先执行 beforeCoolingDown、再返回租约账号已进入冷却的裸哨兵，
// runtimeFailures 中账号的前几次调用先执行 beforeRuntimeFailure、再返回协议 Code 5（要重建 Worker），其余正常返回；
// 记录调用的账号与复制文件的目标账号
type ultraPrimaryUpstream struct {
	mu                   sync.Mutex
	quota                map[string]bool
	quotaErr             func(accountID string) error
	inputLimit           map[string]bool
	coolingDown          map[string]bool
	beforeCoolingDown    func(accountID string)
	runtimeFailures      map[string]int
	beforeRuntimeFailure func(accountID string)
	allQuota             bool
	calls                []string
	copies               []string
}

// ultraDailyQuotaError 构造按模型的每日限额 429（另给 2 小时的重试建议）
func ultraDailyQuotaError() *aistudio.RPCError {
	return &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
		Message: "You exceeded your current quota", RetryDelay: 2 * time.Hour,
		Metadata: map[string]string{"quota_limit": "GenerateRequestsPerDayPerProjectPerModel-FreeTier"},
	}
}

// globalMinuteQuotaError 构造用户遇到的真实分钟限额 429（AI Studio 网页后端每账号每分钟 12 次，冷却整个账号）：
// 只有 ErrorInfo，没有 RetryInfo，冷却到 windowStart 所在窗口结束
func globalMinuteQuotaError(windowStart time.Time) *aistudio.RPCError {
	return &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
		Message: "Quota exceeded for quota metric 'Generate requests' and limit 'Generate requests per minute per user' of service " +
			"'alkalimakersuite-pa.googleapis.com' for consumer 'project_number:823511539352'.",
		Metadata: map[string]string{
			"quota_limit_value": "12", "quota_unit": "1/min/{project}/{user}",
			"window_start_time": strconv.FormatInt(windowStart.Unix(), 10), "consumer": "projects/823511539352",
			"quota_metric": "alkalimakersuite-pa.googleapis.com/generate_requests_global", "service": "alkalimakersuite-pa.googleapis.com",
			"quota_limit": "GeneratePerMinutePerProjectPerUser", "quota_location": "global",
		},
	}
}

func (upstream *ultraPrimaryUpstream) Models(context.Context) ([]aistudio.Model, error) {
	return nil, nil
}

func (upstream *ultraPrimaryUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *ultraPrimaryUpstream) called() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

func (upstream *ultraPrimaryUpstream) copied() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.copies)
}

func (upstream *ultraPrimaryUpstream) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID)
	quota := upstream.allQuota || upstream.quota[request.AccountID]
	inputLimit, coolingDown := upstream.inputLimit[request.AccountID], upstream.coolingDown[request.AccountID]
	runtimeFailure := upstream.runtimeFailures[request.AccountID] > 0
	if runtimeFailure {
		upstream.runtimeFailures[request.AccountID]--
	}
	hook, quotaErr, coolingHook := upstream.beforeRuntimeFailure, upstream.quotaErr, upstream.beforeCoolingDown
	upstream.mu.Unlock()
	switch {
	case quota:
		if quotaErr != nil {
			return nil, quotaErr(request.AccountID)
		}
		return nil, ultraDailyQuotaError()
	case inputLimit:
		return nil, &aistudio.RPCError{
			Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
			Message: "The input token count (2000000) exceeds the maximum number of tokens allowed (1048576).",
		}
	case coolingDown:
		if coolingHook != nil {
			coolingHook(request.AccountID)
		}
		return nil, aistudio.ErrAccountCoolingDown
	case runtimeFailure:
		if hook != nil {
			hook(request.AccountID)
		}
		return nil, waaRuntimeFailure("GenerateContent")
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

// CopyFileReferencesToLease 模拟把文件引用复制到目标账号：只记录目标账号，内容原样返回
func (upstream *ultraPrimaryUpstream) CopyFileReferencesToLease(
	_ context.Context, lease *aistudio.AccountLease, contents []aistudio.Content,
) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error) {
	upstream.mu.Lock()
	upstream.copies = append(upstream.copies, lease.Account().ID)
	upstream.mu.Unlock()
	return contents, nil, nil
}

// assertNoBusyAccounts 断言请求结束后没有账户仍持有租约（额度 429 的账号会进入冷却，不要求就绪）
func assertNoBusyAccounts(t *testing.T, pool *aistudio.AccountPool) {
	t.Helper()
	for _, status := range pool.Status() {
		if status.State == aistudio.AccountBusy {
			t.Fatalf("账户 %s 请求结束后仍持有租约", status.ID)
		}
	}
}

func ultraTestIDs(count int) []string {
	ids := make([]string, count)
	for index := range ids {
		ids[index] = fmt.Sprintf("ultra-p%d@example.com", index)
	}
	return ids
}

// TestPrimaryListsPerPool 两份主用模型列表按号池生效：普通列表只作用于普通号池，Ultra 列表只作用于 Ultra 号池且额度快速失败；
// 不限号池先查普通列表（命中按普通规则），没有再查 Ultra 列表（命中按 Ultra 规则）；模型名按降级判定的规则整理
func TestPrimaryListsPerPool(t *testing.T) {
	service := &trackedService{}
	service.setPrimaryModelSettings([]string{"model-a"}, 3*time.Second, []string{"models/Model-B"}, 7*time.Second)
	for _, test := range []struct {
		model    string
		pool     aistudio.PoolScope
		listed   bool
		wait     time.Duration
		failFast bool
	}{
		{model: "model-a", pool: aistudio.PoolScopeNormal, listed: true, wait: 3 * time.Second},
		{model: "model-a", pool: aistudio.PoolScopeUltra},
		{model: "model-b", pool: aistudio.PoolScopeUltra, listed: true, wait: 7 * time.Second, failFast: true},
		{model: "models/MODEL-B", pool: aistudio.PoolScopeUltra, listed: true, wait: 7 * time.Second, failFast: true},
		{model: "model-b", pool: aistudio.PoolScopeNormal},
		{model: "model-b", pool: aistudio.PoolScopeAll, listed: true, wait: 7 * time.Second, failFast: true},
		{model: "model-a", pool: aistudio.PoolScopeAll, listed: true, wait: 3 * time.Second},
		{model: "model-c", pool: aistudio.PoolScopeAll},
	} {
		policy, listed := service.primaryPolicyFor(test.pool, test.model)
		if listed != test.listed || listed && (policy.queueWait != test.wait || policy.quotaFailFast != test.failFast) {
			t.Fatalf("(%s, %s) = %+v listed=%v，期望 listed=%v wait=%s failFast=%v",
				test.model, test.pool, policy, listed, test.listed, test.wait, test.failFast)
		}
		budget := service.primaryBudget(test.model, test.pool)
		if (budget != nil) != test.listed || budget.failFastOnQuota() != test.failFast {
			t.Fatalf("(%s, %s) 的排队预算 = %+v，期望有预算=%v、额度快速失败=%v", test.model, test.pool, budget, test.listed, test.failFast)
		}
		if budget != nil && (budget.limit != test.wait || budget.pool != test.pool) {
			t.Fatalf("(%s, %s) 的排队预算 = %+v", test.model, test.pool, budget)
		}
	}
	// 同一个模型在两份列表里：不限号池按普通规则
	service.setPrimaryModelSettings([]string{"model-b"}, time.Second, []string{"model-b"}, 2*time.Second)
	if policy, listed := service.primaryPolicyFor(aistudio.PoolScopeAll, "model-b"); !listed || policy.quotaFailFast || policy.queueWait != time.Second {
		t.Fatalf("两份列表都有时不限号池应按普通规则: %+v", policy)
	}
	// 只改普通列表时 Ultra 列表保持不变
	service.setPrimaryModels([]string{"model-c"}, 4*time.Second)
	if policy, listed := service.primaryPolicyFor(aistudio.PoolScopeUltra, "model-b"); !listed || !policy.quotaFailFast || policy.queueWait != 2*time.Second {
		t.Fatalf("setPrimaryModels 不应改动 Ultra 列表: %+v listed=%v", policy, listed)
	}
	if _, listed := service.primaryPolicyFor(aistudio.PoolScopeNormal, "model-b"); listed {
		t.Fatal("setPrimaryModels 应整体替换普通列表")
	}
	var none *leaseWaitBudget
	if none.failFastOnQuota() {
		t.Fatal("没有预算时不应快速失败")
	}
}

// TestStreamStartWaitPerPool 流式首事件等待下限按请求 context 的号池取对应列表的排队上限（含经 runtimeManager 的路径）：
// 默认配置下 /ultra 上的 gemini-4-argon 为 Ultra 排队上限加余量，普通路径上为 0；普通列表的模型在 /ultra 上为 0
func TestStreamStartWaitPerPool(t *testing.T) {
	clearAppConfigEnv(t)
	clearPrimaryConfigEnv(t)
	cfg := config.Default()
	cfg.PrimaryModels = []string{testRuntimeModel}
	cfg.PrimaryQueueWait = 3 * time.Second
	cfg.UltraPrimaryQueueWait = 4 * time.Second
	service := &trackedService{}
	service.setPrimaryModelSettings(cfg.PrimaryModels, cfg.PrimaryQueueWait, cfg.UltraPrimaryModels, cfg.UltraPrimaryQueueWait)
	manager := &runtimeManager{current: &runtimeGeneration{service: service, config: cfg}}
	ultraCtx := aistudio.ContextWithPoolScope(context.Background(), aistudio.PoolScopeUltra)
	allCtx := aistudio.ContextWithPoolScope(context.Background(), aistudio.PoolScopeAll)
	for _, test := range []struct {
		name  string
		ctx   context.Context
		model string
		want  time.Duration
	}{
		{name: "Ultra 号池的 argon", ctx: ultraCtx, model: "gemini-4-argon", want: 4*time.Second + primaryStreamStartGrace},
		{name: "普通号池的 argon", ctx: normalPoolContext(), model: "gemini-4-argon"},
		{name: "不限号池的 argon 按 Ultra 列表", ctx: allCtx, model: "gemini-4-argon", want: 4*time.Second + primaryStreamStartGrace},
		{name: "没有号池标记的后台调用", ctx: context.Background(), model: "gemini-4-argon", want: 4*time.Second + primaryStreamStartGrace},
		{name: "普通列表的模型在 Ultra 号池", ctx: ultraCtx, model: testRuntimeModel},
		{name: "普通列表的模型在普通号池", ctx: normalPoolContext(), model: testRuntimeModel, want: 3*time.Second + primaryStreamStartGrace},
		{name: "不在列表里", ctx: ultraCtx, model: "other-model"},
	} {
		if got := service.StreamStartWait(test.ctx, test.model); got != test.want {
			t.Fatalf("%s：trackedService.StreamStartWait = %s，期望 %s", test.name, got, test.want)
		}
		if got := manager.StreamStartWait(test.ctx, test.model); got != test.want {
			t.Fatalf("%s：runtimeManager.StreamStartWait = %s，期望 %s", test.name, got, test.want)
		}
	}
}

// TestUpdateRuntimeConfigUltraPrimaryModels 管理页面保存 Ultra 主用模型与排队上限：整理后写入 .env；旧版页面不带这些字段时沿用现值；
// 空数组表示关闭；无效或超过请求超时的排队上限返回 invalid_config；读取与保存结果总是返回这些字段；生效配置比较包含它们
func TestUpdateRuntimeConfigUltraPrimaryModels(t *testing.T) {
	clearAppConfigEnv(t)
	clearPrimaryConfigEnv(t)
	for _, key := range []string{"ULTRA_PRIMARY_MODELS", "ULTRA_PRIMARY_QUEUE_WAIT"} {
		t.Setenv(key, "")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := t.TempDir() + "/.env"
	if err := config.Default().Save(path); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}
	load := func() ([]string, time.Duration) {
		t.Helper()
		saved, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return saved.UltraPrimaryModels, saved.UltraPrimaryQueueWait
	}
	// 旧版页面：没有这些字段，沿用现值（默认列表）
	legacy := validRuntimeConfig()
	legacy.UltraPrimaryModels, legacy.UltraPrimaryQueueWait = nil, ""
	if _, err := admin.UpdateRuntimeConfig(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if models, wait := load(); !slices.Equal(models, []string{"gemini-4-argon"}) || wait != 10*time.Second {
		t.Fatalf("旧版页面不带这些字段时应沿用现值：%v / %s", models, wait)
	}
	value := validRuntimeConfig()
	models := []string{" models/model-u ", "model-v", "model-u"}
	value.UltraPrimaryModels = &models
	value.UltraPrimaryQueueWait = "6s"
	saved, err := admin.UpdateRuntimeConfig(ctx, value)
	want := []string{"model-u", "model-v"}
	if err != nil || saved.UltraPrimaryModels == nil || !slices.Equal(*saved.UltraPrimaryModels, want) || saved.UltraPrimaryQueueWait != "6s" {
		t.Fatalf("保存返回 %v / %q err=%v，期望 %v / 6s", saved.UltraPrimaryModels, saved.UltraPrimaryQueueWait, err, want)
	}
	if got, wait := load(); !slices.Equal(got, want) || wait != 6*time.Second {
		t.Fatalf("保存后 .env = %v / %s", got, wait)
	}
	empty := []string{}
	value.UltraPrimaryModels = &empty
	value.UltraPrimaryQueueWait = "0s"
	if saved, err = admin.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatal(err)
	}
	if got, wait := load(); len(got) != 0 || wait != 0 {
		t.Fatalf("空数组与 0s 应关闭并不排队：%v / %s", got, wait)
	}
	if saved.UltraPrimaryModels == nil || len(*saved.UltraPrimaryModels) != 0 || saved.UltraPrimaryQueueWait != "0s" {
		t.Fatalf("为空时也应返回空数组与 0s：%v / %q", saved.UltraPrimaryModels, saved.UltraPrimaryQueueWait)
	}
	read, err := admin.RuntimeConfig(ctx)
	if err != nil || read.UltraPrimaryModels == nil || read.UltraPrimaryQueueWait != "0s" {
		t.Fatalf("读取配置应返回这些字段: %+v err=%v", read, err)
	}
	var operation *adminOperationError
	for _, wait := range []string{"abc", "-1s", "10m"} {
		value.UltraPrimaryQueueWait = wait
		if _, err := admin.UpdateRuntimeConfig(ctx, value); !errors.As(err, &operation) || operation.status != http.StatusBadRequest || operation.code != "invalid_config" {
			t.Fatalf("排队上限 %q 应返回 invalid_config，实际 err=%v", wait, err)
		}
	}
	// 旧版页面把请求超时调到 Ultra 排队上限以下：按请求超时封顶，不报页面上没有的字段无效
	value.UltraPrimaryModels, value.UltraPrimaryQueueWait = nil, ""
	if err := config.Default().Save(path); err != nil {
		t.Fatal(err)
	}
	value.RequestTimeout = "8s"
	value.PrimaryQueueWait = "5s"
	if _, err := admin.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatalf("旧版页面调小请求超时应能保存: %v", err)
	}
	if _, wait := load(); wait != 8*time.Second {
		t.Fatalf("Ultra 排队上限应按请求超时封顶为 8s，实际 %s", wait)
	}

	active := config.Default()
	changed := active
	changed.UltraPrimaryModels = []string{"model-u"}
	if liveFieldsEqual(changed, active) || requiresRebuild(changed, active) {
		t.Fatal("Ultra 主用模型列表变化应按热更新处理")
	}
	waitChanged := active
	waitChanged.UltraPrimaryQueueWait = 3 * time.Second
	if liveFieldsEqual(waitChanged, active) || requiresRebuild(waitChanged, active) {
		t.Fatal("Ultra 排队上限变化应按热更新处理")
	}
	dto := runtimeConfigDTO(changed)
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含 Ultra 主用模型列表")
	}
	dto.UltraPrimaryModels = nil
	dto.UltraPrimaryQueueWait = ""
	if !sameDataConfig(dto, active, dataConfigOverrides{}) {
		t.Fatal("旧版页面不带这些字段时按生效值比较")
	}
	dto.UltraPrimaryQueueWait = "3s"
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, waitChanged, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含 Ultra 排队上限")
	}
}

// TestUltraPrimaryModelsHotReload 保存配置后 Ultra 主用模型列表与排队上限立即生效（不重启生成服务），Worker 管理器同步收到两份列表；
// 旧版页面保存时沿用现值；清空后 Ultra 号池的模型恢复默认排队
func TestUltraPrimaryModelsHotReload(t *testing.T) {
	clearAppConfigEnv(t)
	clearPrimaryConfigEnv(t)
	for _, key := range []string{"ULTRA_PRIMARY_MODELS", "ULTRA_PRIMARY_QUEUE_WAIT"} {
		t.Setenv(key, "")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := t.TempDir() + "/.env"
	cfg := config.Default()
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	requests := newRequestRegistry(ctx)
	pool, accounts := testAccountPool(t, ultraTestNormalA)
	workers := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.WarmStartupConcurrency, false)
	t.Cleanup(func() { _ = workers.Close() })
	service := &trackedService{
		lifecycle: ctx, catalog: emptyCatalog{}, pool: pool, requests: requests, workers: workers,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	service.setPrimaryModelSettings(cfg.PrimaryModels, cfg.PrimaryQueueWait, cfg.UltraPrimaryModels, cfg.UltraPrimaryQueueWait)
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: workers, configPath: path, config: cfg}
	manager := &runtimeManager{
		lifecycle: ctx, configPath: path, activeManagement: cfg, requests: requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{}, current: &runtimeGeneration{service: service, admin: admin, config: cfg},
	}
	ultraCtx := aistudio.ContextWithPoolScope(ctx, aistudio.PoolScopeUltra)
	if _, ultra := workers.primaryModelLists(); !slices.Equal(ultra, []string{"gemini-4-argon"}) {
		t.Fatalf("启动时 Worker 管理器的 Ultra 列表 = %v，期望默认列表", ultra)
	}

	value := runtimeConfigDTO(cfg)
	models := []string{"models/" + testRuntimeModel}
	value.UltraPrimaryModels = &models
	value.UltraPrimaryQueueWait = "2s"
	updated, err := manager.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServiceRestartRequired {
		t.Fatal("Ultra 主用模型热更新后不应要求重启生成服务")
	}
	if policy, listed := service.primaryPolicyFor(aistudio.PoolScopeUltra, testRuntimeModel); !listed || !policy.quotaFailFast || policy.queueWait != 2*time.Second {
		t.Fatalf("保存后 Ultra 列表应立即生效: %+v listed=%v", policy, listed)
	}
	if _, listed := service.primaryPolicyFor(aistudio.PoolScopeUltra, "gemini-4-argon"); listed {
		t.Fatal("移出列表的模型应立即不再是 Ultra 主用模型")
	}
	if got := manager.StreamStartWait(ultraCtx, testRuntimeModel); got != 2*time.Second+primaryStreamStartGrace {
		t.Fatalf("Ultra 主用模型的流式首事件等待下限 = %s", got)
	}
	if _, ultra := workers.primaryModelLists(); !slices.Equal(ultra, []string{testRuntimeModel}) {
		t.Fatalf("Worker 管理器的 Ultra 列表 = %v，期望随保存更新", ultra)
	}
	if !slices.Equal(manager.current.config.UltraPrimaryModels, []string{testRuntimeModel}) || manager.current.config.UltraPrimaryQueueWait != 2*time.Second {
		t.Fatalf("热更新后生效配置 = %v / %s", manager.current.config.UltraPrimaryModels, manager.current.config.UltraPrimaryQueueWait)
	}
	if !hasLogContaining(requests, "配置已热更新", "Ultra 主用模型="+testRuntimeModel+"（排队上限 2s）") {
		t.Fatalf("热更新日志应写明 Ultra 主用模型，日志 = %v", serviceLogMessages(requests))
	}

	// 旧版页面：沿用现值
	value.UltraPrimaryModels, value.UltraPrimaryQueueWait = nil, ""
	if _, err := manager.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatal(err)
	}
	if _, listed := service.primaryPolicyFor(aistudio.PoolScopeUltra, testRuntimeModel); !listed {
		t.Fatal("旧版页面保存后 Ultra 列表应保持不变")
	}
	empty := []string{}
	value.UltraPrimaryModels = &empty
	if _, err := manager.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatal(err)
	}
	if _, listed := service.primaryPolicyFor(aistudio.PoolScopeUltra, testRuntimeModel); listed {
		t.Fatal("清空后应立即恢复默认排队")
	}
	if got := manager.StreamStartWait(ultraCtx, testRuntimeModel); got != 0 {
		t.Fatalf("清空后流式首事件等待下限 = %s，期望 0", got)
	}
}

// TestDeadSlotsPerPartition 死槽位与预热排序按分区的主用模型判断：Ultra Worker 只在普通列表的模型上长时间冷却不算死槽位
// （它还能服务 Ultra 列表的模型），在 Ultra 列表的模型上算；普通 Worker 反之。补登记、状态页计数与空闲回收口径一致，
// 不限号池的选号用两份列表的并集
func TestDeadSlotsPerPartition(t *testing.T) {
	const ultraModel = "ultra-primary-model"
	manager, pool, requests := partitionTestManager(t,
		[]string{partitionNormalA, partitionNormalB}, []string{partitionUltraA, partitionUltraB}, 2, 2, 2, 2)
	catalog := []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: drainProModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: ultraModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, catalog); err != nil {
			t.Fatal(err)
		}
	}
	for _, accountID := range []string{partitionUltraA, partitionUltraB} {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierUltra, catalog); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	manager.afterRetire.Store(nil)
	service.setPrimaryModelSettings([]string{drainProModel}, 10*time.Second, []string{ultraModel}, 10*time.Second)
	if got := manager.deadSlotModelsFor(aistudio.PoolScopeUltra); !slices.Equal(got, []string{ultraModel}) {
		t.Fatalf("Ultra 分区的死槽位模型 = %v", got)
	}
	if got := manager.deadSlotModelsFor(aistudio.PoolScopeNormal); !slices.Equal(got, []string{drainProModel}) {
		t.Fatalf("普通分区的死槽位模型 = %v", got)
	}
	if got := manager.deadSlotModelsForScope(aistudio.PoolScopeAll); !slices.Equal(got, []string{drainProModel, ultraModel}) {
		t.Fatalf("不限号池的死槽位模型 = %v，期望两份列表的并集", got)
	}
	for _, accountID := range []string{partitionNormalA, partitionUltraA, partitionUltraB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	// Ultra A 只在普通列表的模型上冷却，Ultra B 在 Ultra 列表的模型上冷却，普通 A 在 Ultra 列表的模型上冷却
	coolModelFor(t, pool, partitionUltraA, drainProModel, 2*time.Hour)
	coolModelFor(t, pool, partitionUltraB, ultraModel, 2*time.Hour)
	coolModelFor(t, pool, partitionNormalA, ultraModel, 2*time.Hour)
	ultra := pool.UltraAccountIDs()
	normalDead, ultraDead := manager.deadWarmCounts(manager.WarmAccountIDs(), ultra)
	if normalDead != 0 || ultraDead != 1 {
		t.Fatalf("死槽位 = 普通 %d · Ultra %d，期望 0 · 1（只有在 Ultra 列表模型上冷却的 Ultra Worker）", normalDead, ultraDead)
	}
	// 普通分区的死槽位模型里没有 ultraModel，Ultra 分区里没有 drainProModel：只登记 Ultra B
	release := holdLease(t, pool, partitionUltraB)
	defer release()
	if added := service.registerDeadWorkers(); added != 1 {
		t.Fatalf("补登记 = %d，期望只登记 Ultra B", added)
	}
	manager.retireMu.Lock()
	_, ultraBRegistered := manager.retirements[partitionUltraB]
	_, ultraARegistered := manager.retirements[partitionUltraA]
	_, normalRegistered := manager.retirements[partitionNormalA]
	manager.retireMu.Unlock()
	if !ultraBRegistered || ultraARegistered || normalRegistered {
		t.Fatalf("登记 = Ultra B %v · Ultra A %v · 普通 A %v，期望只有 Ultra B", ultraBRegistered, ultraARegistered, normalRegistered)
	}
	// 预热排序按分区：Ultra 分区按 Ultra 列表排，普通列表的冷却不影响 Ultra 账号的先后
	if got := manager.preferUncooled(aistudio.PoolScopeUltra, []string{partitionUltraB, partitionUltraA}); !slices.Equal(got, []string{partitionUltraA, partitionUltraB}) {
		t.Fatalf("Ultra 分区的预热排序 = %v，期望在 Ultra 列表模型上没冷却的 Ultra A 在前", got)
	}
	if got := manager.preferUncooled(aistudio.PoolScopeNormal, []string{partitionNormalA, partitionNormalB}); !slices.Equal(got, []string{partitionNormalA, partitionNormalB}) {
		t.Fatalf("普通分区的预热排序 = %v，Ultra 列表模型的冷却不应影响普通账号", got)
	}
	// 备用账号排序（preferred）同样按候选账户的分区取列表
	capacity := manager.standbyCapacity(manager.WarmAccountIDs(), testRuntimeModel)
	if got := capacity.preferred([]string{partitionUltraB, partitionUltraA})[0]; !slices.Equal(got, []string{partitionUltraA, partitionUltraB}) {
		t.Fatalf("备用账号排序 = %v，期望按 Ultra 列表排", got)
	}
	// 其他模型的请求优先用死槽位：Ultra 号池里死槽位是 Ultra B，Ultra 列表的模型本身不挑死槽位
	warmUltra := []string{partitionUltraA, partitionUltraB}
	if got := service.deadSlotWorkers(aistudio.AccountSelection{ModelID: testRuntimeModel, Pool: aistudio.PoolScopeUltra}, false, warmUltra); !slices.Equal(got, []string{partitionUltraB}) {
		t.Fatalf("Ultra 号池其他模型的死槽位 = %v，期望 Ultra B", got)
	}
	if got := service.deadSlotWorkers(aistudio.AccountSelection{ModelID: ultraModel, Pool: aistudio.PoolScopeUltra}, false, warmUltra); got != nil {
		t.Fatalf("Ultra 列表的模型不应优先用死槽位: %v", got)
	}
	// 普通号池里 Ultra 列表的模型只是其他模型：可以用死槽位（这里普通号池只有一个驻留 Worker，不挑）
	if got := service.deadSlotWorkers(aistudio.AccountSelection{ModelID: drainProModel, Pool: aistudio.PoolScopeNormal}, false, warmUltra); got != nil {
		t.Fatalf("普通列表的模型在普通号池不应优先用死槽位: %v", got)
	}
}

// TestPrimarySupplyUltraEntries 主用模型供给按列表分开统计：Ultra 列表的条目 pool 为 ultra、只计 Ultra 分区的驻留 Worker 与备用账号，
// 普通列表的条目 pool 为空、只计普通分区；状态只保留仍在对应列表里的（号池、模型），Ultra 列表变化同样立即重新统计
func TestPrimarySupplyUltraEntries(t *testing.T) {
	manager, pool, requests := partitionTestManager(t,
		[]string{partitionNormalA, partitionNormalB}, []string{partitionUltraA, partitionUltraB, partitionUltraC}, 2, 2, 2, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	for _, accountID := range []string{partitionNormalA, partitionUltraA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	service.setPrimaryModelSettings([]string{testRuntimeModel}, 10*time.Second, []string{testRuntimeModel, partitionUltraModel}, 10*time.Second)
	start := time.Now()
	if !manager.refreshPrimarySupply(start) {
		t.Fatal("第一次应统计")
	}
	entries := manager.primarySupplyStatus()
	find := func(model string, poolLabel string) (got primarySupplyEntry, ok bool) {
		for _, entry := range entries {
			if entry.Model == model && entry.Pool == poolLabel {
				return primarySupplyEntry{warm: entry.Warm, standby: entry.Standby}, true
			}
		}
		return primarySupplyEntry{}, false
	}
	if len(entries) != 3 {
		t.Fatalf("供给条目 = %+v，期望普通 1 条、Ultra 2 条", entries)
	}
	if got, ok := find(testRuntimeModel, ""); !ok || got != (primarySupplyEntry{warm: 1, standby: 1}) {
		t.Fatalf("普通列表的供给 = %+v ok=%v，期望只计普通分区：热 1 · 备用 1", got, ok)
	}
	if got, ok := find(testRuntimeModel, "ultra"); !ok || got != (primarySupplyEntry{warm: 1, standby: 2}) {
		t.Fatalf("Ultra 列表的供给 = %+v ok=%v，期望只计 Ultra 分区：热 1 · 备用 2", got, ok)
	}
	// partitionUltraModel 只在 Ultra B 的目录里：Ultra 分区备用 1，驻留的 Ultra A 不能服务它
	if got, ok := find(partitionUltraModel, "ultra"); !ok || got != (primarySupplyEntry{warm: 0, standby: 1}) {
		t.Fatalf("只有 Ultra B 能服务的模型供给 = %+v ok=%v", got, ok)
	}
	// Ultra 列表变化后立即重新统计；移出 Ultra 列表的条目不再显示，普通列表的同名条目不受影响
	setUltraPrimaryModels(service, []string{partitionUltraModel}, 10*time.Second)
	if entries = manager.primarySupplyStatus(); len(entries) != 2 {
		t.Fatalf("移出 Ultra 列表的条目应立即不显示: %+v", entries)
	}
	if _, ok := find(testRuntimeModel, ""); !ok {
		t.Fatalf("普通列表的同名条目应保留: %+v", entries)
	}
	if !manager.refreshPrimarySupply(start.Add(time.Second)) {
		t.Fatal("Ultra 主用模型列表变化后应立即重新统计")
	}
	service.setPrimaryModelSettings(nil, 0, nil, 0)
	if manager.refreshPrimarySupply(start.Add(2*time.Second)) || manager.primarySupplyStatus() != nil {
		t.Fatal("两份列表都为空时应清空缓存")
	}
	// 只有 Ultra 列表时也统计
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
	if !manager.refreshPrimarySupply(start.Add(3 * time.Second)) {
		t.Fatal("只有 Ultra 列表时也应统计")
	}
	if entries = manager.primarySupplyStatus(); len(entries) != 1 || entries[0].Pool != "ultra" {
		t.Fatalf("只有 Ultra 列表时的供给 = %+v", entries)
	}
}

type primarySupplyEntry struct {
	warm    int
	standby int
}

// TestUltraPrimaryAllCoolingFailsFast Ultra 主用模型候选全部冷却时立即返回全部冷却（不论 400ms 还是 30s 后恢复），Retry-After
// 为剩余冷却时间，进度写明立即返回；普通号池的主用模型与 Ultra 号池的非主用模型遇到 400ms 的冷却仍然原地等待
func TestUltraPrimaryAllCoolingFailsFast(t *testing.T) {
	for _, cooldown := range []time.Duration{400 * time.Millisecond, 30 * time.Second} {
		t.Run(cooldown.String(), func(t *testing.T) {
			service, pool := primaryBusyService(t, nil, primaryTestA)
			markUltra(t, pool, primaryTestA)
			setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
			coolModelFor(t, pool, primaryTestA, testRuntimeModel, cooldown)
			until := time.Now().Add(cooldown)
			ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), 10*time.Second)
			defer cancel()
			lease, err, elapsed := acquirePrimary(ctx, service, aistudio.PoolScopeUltra)
			var cooling *aistudio.AllCoolingError
			if lease != nil || !errors.As(err, &cooling) || elapsed > 200*time.Millisecond {
				t.Fatalf("lease=%v err=%v 用时 %s，期望约 200ms 内返回全部冷却", lease, err, elapsed)
			}
			if diff := cooling.RetryAt().Sub(until); diff < -100*time.Millisecond || diff > 100*time.Millisecond {
				t.Fatalf("最早恢复 = %s，期望约 %s", cooling.RetryAt(), until)
			}
			if waiters := dispatchWaiters(service); waiters != 0 {
				t.Fatalf("立即返回后仍有 %d 个等待者", waiters)
			}
			// 经 generateWithRetry：进度写明候选全部冷却立即返回，不向上游发送
			_, err, elapsed = runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-cooling", "")
			if !errors.As(err, &cooling) || elapsed > 500*time.Millisecond {
				t.Fatalf("请求 err=%v 用时 %s，期望立即返回全部冷却", err, elapsed)
			}
			if !progressLogged(service, "Ultra 主用模型候选全部冷却，立即返回 429 | 最早恢复=") {
				t.Fatal("请求进度应写明 Ultra 主用模型候选全部冷却")
			}
		})
	}
	t.Run("普通号池与非主用模型仍等待", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			ultra bool
			scope aistudio.PoolScope
		}{
			{name: "普通号池的主用模型", scope: aistudio.PoolScopeNormal},
			{name: "Ultra 号池的非主用模型", ultra: true, scope: aistudio.PoolScopeUltra},
		} {
			service, pool := primaryBusyService(t, nil, primaryTestA)
			if test.ultra {
				markUltra(t, pool, primaryTestA)
				setUltraPrimaryModels(service, []string{"other-model"}, 0)
			} else {
				service.setPrimaryModelSettings([]string{testRuntimeModel}, 0, []string{testRuntimeModel}, 0)
			}
			coolModelFor(t, pool, primaryTestA, testRuntimeModel, 400*time.Millisecond)
			ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(service.lifecycle, test.scope), 10*time.Second)
			lease, err, elapsed := acquirePrimary(ctx, service, test.scope)
			cancel()
			if err != nil || elapsed < 300*time.Millisecond {
				t.Fatalf("%s：lease=%v err=%v 用时 %s，期望等到冷却结束后取得租约", test.name, lease, err, elapsed)
			}
			_ = lease.Release()
		}
	})
}

// TestUltraPrimaryNoSlotStillCapacity Ultra 主用模型（不限号池按 Ultra 规则）遇到“还有没冷却的备用账号只是没有槽位”仍是容量原因：
// 排队到上限后返回号池繁忙，不按没额度立即返回全部冷却
func TestUltraPrimaryNoSlotStillCapacity(t *testing.T) {
	env := newDrainEnv(t, slotWaitAccounts, 2, 2)
	fullPartitionCooledWarm(t, env, slotWaitAccounts, 30*time.Second)
	env.service.setPrimaryModelSettings(nil, 0, []string{testRuntimeModel}, 300*time.Millisecond)
	ctx, cancel := context.WithTimeout(env.ctx, 3*time.Second)
	defer cancel()
	lease, err, elapsed := acquirePrimary(ctx, env.service, aistudio.PoolScopeAll)
	if lease != nil {
		_ = lease.Release()
	}
	var busy *aistudio.CapacityBusyError
	if !errors.As(err, &busy) || busy.Reason != waitReasonNoSlot || elapsed < 300*time.Millisecond {
		t.Fatalf("err=%v 用时 %s，期望排队约 300ms 后按没有空闲的 Worker 槽位返回号池繁忙", err, elapsed)
	}
}

// TestUltraPrimaryQuotaSwitchLimit 6 个热账号都返回额度 429：Ultra 主用模型额度失败 3 次后立即返回额度不足（429，Retry-After 约 5s，
// 对外不带上游原文），不退避；普通号池的主用模型与 Ultra 号池的非主用模型照旧把 6 个账号都试一遍
func TestUltraPrimaryQuotaSwitchLimit(t *testing.T) {
	accounts := ultraTestIDs(6)
	t.Run("Ultra 主用模型", func(t *testing.T) {
		upstream := &ultraPrimaryUpstream{allQuota: true}
		service, pool := primaryBusyService(t, upstream, accounts...)
		markUltra(t, pool, accounts...)
		setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
		_, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-quota", "")
		var quota *aistudio.PrimaryQuotaError
		if !errors.As(err, &quota) {
			t.Fatalf("err=%v，期望额度不足", err)
		}
		if calls := upstream.called(); len(calls) != primaryQuotaSwitchLimit+1 {
			t.Fatalf("上游调用 = %v，期望 %d 次", calls, primaryQuotaSwitchLimit+1)
		}
		if quota.Reason != primaryQuotaReasonLimit || quota.QuotaHits != 3 || quota.Pool != aistudio.PoolScopeUltra || quota.ModelID != testRuntimeModel {
			t.Fatalf("额度不足错误 = %+v", quota)
		}
		if retry := time.Until(quota.RetryAt()); retry < 4*time.Second || retry > 6*time.Second {
			t.Fatalf("建议重试间隔 = %s，期望约 5s", retry)
		}
		if quota.HTTPStatus() != http.StatusTooManyRequests || quota.ErrorCode() != "rate_limit_exceeded" || quota.Cause == nil {
			t.Fatalf("公开状态 = %d %s cause=%v", quota.HTTPStatus(), quota.ErrorCode(), quota.Cause)
		}
		var rpcError *aistudio.RPCError
		if errors.As(err, &rpcError) {
			t.Fatal("额度不足错误不应被当成上游 RPCError（不实现 Unwrap）")
		}
		if elapsed > time.Second {
			t.Fatalf("用时 %s，期望不退避、立即返回", elapsed)
		}
		for _, fragment := range []string{
			"额度 429，换号重试（只用已启动的 Worker）| 额度失败=1/2",
			"额度 429，换号重试（只用已启动的 Worker）| 额度失败=2/2",
			"Ultra 主用模型额度不足，立即返回 429 | 原因=额度失败已达换号上限 | 额度失败=3 | 总耗时=",
		} {
			if !progressLogged(service, fragment) {
				t.Fatalf("请求进度应写明 %q", fragment)
			}
		}
		if count := service.requests.count(); count != 0 {
			t.Fatalf("请求结束后仍有 %d 个活动请求", count)
		}
		assertNoBusyAccounts(t, pool)
	})
	for _, test := range []struct {
		name  string
		ultra bool
		scope aistudio.PoolScope
	}{
		{name: "普通号池的主用模型", scope: aistudio.PoolScopeNormal},
		{name: "Ultra 号池的非主用模型", ultra: true, scope: aistudio.PoolScopeUltra},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &ultraPrimaryUpstream{allQuota: true}
			service, pool := primaryBusyService(t, upstream, accounts...)
			if test.ultra {
				markUltra(t, pool, accounts...)
			} else {
				service.setPrimaryModelSettings([]string{testRuntimeModel}, 10*time.Second, []string{testRuntimeModel}, 10*time.Second)
			}
			_, err, _ := runScopedPrimaryRequest(t, service, test.scope, "req-quota-all", "")
			var quota *aistudio.PrimaryQuotaError
			var rpcError *aistudio.RPCError
			if errors.As(err, &quota) || !errors.As(err, &rpcError) {
				t.Fatalf("err=%v，期望照旧以上游额度错误结束", err)
			}
			if calls := upstream.called(); len(calls) != len(accounts) {
				t.Fatalf("上游调用 = %v，期望 6 个账号都试一遍", calls)
			}
		})
	}
}

// TestUltraPrimaryQuotaSwitchesToWarm 额度 429 后换到另一个已启动的 Worker 能照常完成
func TestUltraPrimaryQuotaSwitchesToWarm(t *testing.T) {
	upstream := &ultraPrimaryUpstream{quota: map[string]bool{primaryTestA: true}}
	service, pool := primaryBusyService(t, upstream, primaryTestA, primaryTestB)
	markUltra(t, pool, primaryTestA, primaryTestB)
	preverifyRuntimeModelAccess(t, pool, primaryTestB)
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 0)
	text, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-switch", "")
	if err != nil || text != "hello" {
		t.Fatalf("应换号后完成：text=%q err=%v", text, err)
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{primaryTestA, primaryTestB}) {
		t.Fatalf("尝试顺序 = %v", calls)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("换号用时 %s", elapsed)
	}
	if !progressLogged(service, "额度 429，换号重试（只用已启动的 Worker）| 额度失败=1/2") || progressLogged(service, "立即返回 429") {
		t.Fatal("请求进度应写明额度 429 换号，且不按额度不足结束")
	}
}

// TestUltraPrimaryRecoveryReplayCooledSwitches 额度 429 之后换到的账号 Worker 失败、同步重建成功，重放前它又进入冷却（同时有其他请求
// 在它上面遇到额度 429）：固定到它的重放选号失败只排除它、照常换号，不按“其余候选全部冷却”结束（那次选号只看了这一个账号）
func TestUltraPrimaryRecoveryReplayCooledSwitches(t *testing.T) {
	accounts := ultraTestIDs(3)
	upstream := &ultraPrimaryUpstream{
		quota:           map[string]bool{accounts[0]: true},
		runtimeFailures: map[string]int{accounts[1]: 1},
	}
	service, _ := workerResetTestService(t, upstream, accounts...)
	// 使用默认等待（主用模型最多 1 秒）：-race 下关闭也来得及完成，重建在原账号同步完成
	service.resetWait.Store(nil)
	pool := service.pool
	markUltra(t, pool, accounts...)
	coolErrs := make(chan error, 1)
	upstream.beforeRuntimeFailure = func(accountID string) {
		coolErrs <- pool.MarkCooldownIfGeneration(accountID, testRuntimeModel, pool.ModelAccessGeneration(accountID),
			time.Now(), time.Now().Add(2*time.Hour), "每日限额")
	}
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
	text, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-recovery-cooled", "")
	select {
	case coolErr := <-coolErrs:
		if coolErr != nil {
			t.Fatal(coolErr)
		}
	default:
		t.Fatal("第二个账号应在 Worker 失败前进入冷却")
	}
	if err != nil || text != "hello" {
		t.Fatalf("重放选号失败后应换到第三个账号完成：text=%q err=%v", text, err)
	}
	if calls := upstream.called(); !slices.Equal(calls, accounts) {
		t.Fatalf("上游调用 = %v，期望 %v", calls, accounts)
	}
	if !progressLogged(service, "WAA Worker 重建，重放当前请求") {
		t.Fatal("应在第二个账号同步重建 Worker 并安排重放")
	}
	if progressLogged(service, "Ultra 主用模型额度不足") {
		t.Fatal("固定到重建账号的重放选号失败不应按额度不足结束")
	}
	assertNoBusyAccounts(t, pool)
}

// TestUltraPrimaryQuotaRemainingNotReady 额度 429 之后号池里剩下能服务该模型的账号都暂时不可调度（需要重新登录），换号上限
// （可调度账号数，不看模型）却还没用完：选号得到 AccountsNotReadyError 时按“没有其他可换的账号”返回额度不足（429，Retry-After 约 5s），
// 不以上一次不带 Retry-After 的上游额度 429 结束
func TestUltraPrimaryQuotaRemainingNotReady(t *testing.T) {
	accounts := ultraTestIDs(4)
	upstream := &ultraPrimaryUpstream{quota: map[string]bool{accounts[0]: true, accounts[1]: true}}
	service, pool := primaryBusyService(t, upstream, accounts...)
	markUltra(t, pool, accounts[0], accounts[1], accounts[3])
	// 第三个账号是 Ultra 账号、就绪，但目录里没有这个模型：计入换号上限，却不是候选
	if err := pool.SetCatalog(accounts[2], aistudio.BenefitTierUltra, []aistudio.Model{{
		ID: drainProModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	// 第四个账号能服务这个模型，但需要重新登录：选号的无账户错误为 AccountsNotReadyError
	if err := pool.MarkAuthRequired(accounts[3], "Cookie 失效"); err != nil {
		t.Fatal(err)
	}
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
	_, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-not-ready", "")
	var quota *aistudio.PrimaryQuotaError
	if !errors.As(err, &quota) || quota.Reason != primaryQuotaReasonNoOther || quota.QuotaHits != 2 {
		t.Fatalf("err=%v，期望额度不足（没有其他可换的账号）", err)
	}
	if retry := time.Until(quota.RetryAt()); retry < 4*time.Second || retry > 6*time.Second {
		t.Fatalf("建议重试间隔 = %s，期望约 5s", retry)
	}
	if calls := upstream.called(); len(calls) != 2 || slices.Contains(calls, accounts[2]) || slices.Contains(calls, accounts[3]) {
		t.Fatalf("上游调用 = %v，期望只有前两个账号", calls)
	}
	if elapsed > time.Second {
		t.Fatalf("用时 %s，期望立即返回", elapsed)
	}
	if !progressLogged(service, "Ultra 主用模型额度不足，立即返回 429 | 原因=没有其他可换的账号 | 额度失败=2") {
		t.Fatal("请求进度应写明没有其他可换的账号")
	}
	assertNoBusyAccounts(t, pool)
}

// ultraColdStartService 返回浏览器后端的生成服务：两个 Ultra 账号，A 已有驻留 Worker，B 是没冷却的备用账号，extra 为排在 B 之后的
// 其余备用账号（Ultra 分区峰值为默认的 5，B 与它们可以同时启动）；每次冷启动耗时 launchDelay，launched 收到每次冷启动的账号
func ultraColdStartService(
	t *testing.T, upstream aistudio.Service, launchDelay time.Duration, extra ...string,
) (*trackedService, <-chan string) {
	t.Helper()
	accounts := append([]string{primaryTestA, primaryTestB}, extra...)
	service := coldStartTestService(t, launchDelay, nil, accounts...)
	service.service = upstream
	markUltra(t, service.pool, accounts...)
	ctx, cancel := context.WithTimeout(service.lifecycle, 10*time.Second)
	defer cancel()
	if _, err := service.workers.promote(ctx, primaryTestA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	launched := make(chan string, 8)
	inner := service.workers.launch
	service.workers.launch = func(ctx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		launched <- accountID
		return inner(ctx, accountID, options)
	}
	return service, launched
}

// TestUltraPrimaryQuotaNoColdStart A 是热 Worker 且返回额度 429、B 是没冷却的备用账号：Ultra 主用模型不现场冷启动 B，立即返回额度不足
// （Retry-After 约 5s），只为 B 发起后台扩容；非主用模型照常现场冷启动 B 并完成
func TestUltraPrimaryQuotaNoColdStart(t *testing.T) {
	const launch = 1500 * time.Millisecond
	t.Run("Ultra 主用模型", func(t *testing.T) {
		upstream := &ultraPrimaryUpstream{quota: map[string]bool{primaryTestA: true}}
		service, launched := ultraColdStartService(t, upstream, launch)
		setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
		_, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-standby", "")
		var quota *aistudio.PrimaryQuotaError
		if !errors.As(err, &quota) || quota.Reason != primaryQuotaReasonNoWarm || quota.QuotaHits != 1 {
			t.Fatalf("err=%v，期望额度 429 后没有已启动的 Worker", err)
		}
		if elapsed >= launch {
			t.Fatalf("用时 %s，不应同步等待 B 冷启动（%s）", elapsed, launch)
		}
		if retry := time.Until(quota.RetryAt()); retry < 4*time.Second || retry > 6*time.Second {
			t.Fatalf("建议重试间隔 = %s，期望约 5s", retry)
		}
		if calls := upstream.called(); !slices.Equal(calls, []string{primaryTestA}) {
			t.Fatalf("上游调用 = %v，期望只有 A", calls)
		}
		select {
		case accountID := <-launched:
			if accountID != primaryTestB {
				t.Fatalf("后台扩容启动了 %s，期望 B", accountID)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("应为备用账号 B 发起后台扩容")
		}
		if !progressLogged(service, "Ultra 主用模型额度不足，立即返回 429 | 原因=额度 429 后没有已启动的 Worker | 额度失败=1") {
			t.Fatal("请求进度应写明没有已启动的 Worker")
		}
	})
	t.Run("非主用模型", func(t *testing.T) {
		upstream := &ultraPrimaryUpstream{quota: map[string]bool{primaryTestA: true}}
		service, launched := ultraColdStartService(t, upstream, launch)
		preverifyRuntimeModelAccess(t, service.pool, primaryTestB)
		text, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-standby-other", "")
		if err != nil || text != "hello" {
			t.Fatalf("非主用模型应现场冷启动 B 后完成：text=%q err=%v", text, err)
		}
		if elapsed < launch {
			t.Fatalf("用时 %s，期望包含 B 的冷启动", elapsed)
		}
		if calls := upstream.called(); !slices.Equal(calls, []string{primaryTestA, primaryTestB}) {
			t.Fatalf("上游调用 = %v", calls)
		}
		select {
		case accountID := <-launched:
			if accountID != primaryTestB {
				t.Fatalf("冷启动了 %s，期望 B", accountID)
			}
		default:
			t.Fatal("非主用模型应冷启动 B")
		}
	})
}

// TestUltraPrimaryWarmOnlySelection 只用已启动 Worker 的选号：没有热候选时不现场冷启动，立即返回没有立即可用的账号并发起后台扩容；
// 热 Worker 都忙时照常排队、受排队预算限制；标记只作用于带它的那次选号
func TestUltraPrimaryWarmOnlySelection(t *testing.T) {
	service, launched := ultraColdStartService(t, &ultraPrimaryUpstream{}, 300*time.Millisecond)
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 400*time.Millisecond)
	ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), 10*time.Second)
	defer cancel()
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent", Pool: aistudio.PoolScopeUltra}
	budgetCtx := func() context.Context {
		return withLeaseWaitBudget(ctx, service.primaryBudget(testRuntimeModel, aistudio.PoolScopeUltra))
	}
	// A 忙碌：热 Worker 都忙，排队到预算用完后按号池繁忙返回，不冷启动 B
	release := holdLease(t, service.pool, primaryTestA)
	startedAt := time.Now()
	lease, err := service.acquireWarmLease(withoutColdStart(budgetCtx()), selection)
	var busy *aistudio.CapacityBusyError
	if lease != nil || !errors.As(err, &busy) || time.Since(startedAt) < 400*time.Millisecond {
		t.Fatalf("热 Worker 忙碌时 lease=%v err=%v 用时 %s，期望排队到预算用完", lease, err, time.Since(startedAt))
	}
	select {
	case accountID := <-launched:
		t.Fatalf("热 Worker 忙碌时不应启动备用账号，实际启动了 %s", accountID)
	default:
	}
	release()
	// 排除 A：没有热候选，只剩备用账号 B，立即返回并发起后台扩容
	excluded := selection
	excluded.ExcludedAccountIDs = []string{primaryTestA}
	startedAt = time.Now()
	lease, err = service.acquireWarmLease(withoutColdStart(budgetCtx()), excluded)
	var noImmediate *noImmediateLeaseError
	if lease != nil || !errors.As(err, &noImmediate) || noImmediate.reason != primaryQuotaReasonNoWarm || time.Since(startedAt) > 200*time.Millisecond {
		t.Fatalf("只剩备用账号时 lease=%v err=%v 用时 %s，期望立即返回没有已启动的 Worker", lease, err, time.Since(startedAt))
	}
	select {
	case accountID := <-launched:
		if accountID != primaryTestB {
			t.Fatalf("后台扩容启动了 %s，期望 B", accountID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("应为 B 发起后台扩容")
	}
	// 后台扩容完成后 B 是已启动的 Worker，再选号直接取得
	if !waitUntil(3*time.Second, func() bool { return slices.Contains(service.workers.ReadyWarmAccountIDs(), primaryTestB) }) {
		t.Fatal("后台扩容应让 B 就绪")
	}
	lease, err = service.acquireWarmLease(withoutColdStart(budgetCtx()), excluded)
	if err != nil || lease.Account().ID != primaryTestB {
		t.Fatalf("B 就绪后应取得 B：lease=%v err=%v", lease, err)
	}
	_ = lease.Release()
	if coldStartBlocked(ctx) || coldStartBlocked(budgetCtx()) {
		t.Fatal("标记不应出现在没有加它的 context 上")
	}
}

// TestUltraPrimaryStreamHoldDuringColdStart Ultra 主用模型的流式请求第一次尝试现场冷启动备用账号：启动期间写出响应头前的等待一直保持
// （API 层不写出响应头），启动结束后按启动时长延后，启动后才遇到的额度不足仍以 HTTP 429 返回；普通号池的主用模型不推迟（行为不变）
func TestUltraPrimaryStreamHoldDuringColdStart(t *testing.T) {
	const launch = 600 * time.Millisecond
	// watchLaunch 统计冷启动开始时写出响应头前的等待保持着的次数（只有请求自己的现场冷启动会保持）
	watchLaunch := func(service *trackedService) *atomic.Int32 {
		var held atomic.Int32
		inner := service.workers.launch
		service.workers.launch = func(ctx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
			if _, starting := aistudio.StreamStartHoldFromContext(ctx).State(); starting {
				held.Add(1)
			}
			return inner(ctx, accountID, options)
		}
		return &held
	}
	t.Run("Ultra 主用模型", func(t *testing.T) {
		upstream := &ultraPrimaryUpstream{allQuota: true}
		service := coldStartTestService(t, launch, nil, primaryTestA, primaryTestB)
		service.service = upstream
		markUltra(t, service.pool, primaryTestA, primaryTestB)
		setUltraPrimaryModels(service, []string{testRuntimeModel}, 200*time.Millisecond)
		held := watchLaunch(service)
		hold := aistudio.NewStreamStartHold()
		clientCtx := aistudio.WithStreamStartHold(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), hold)
		_, err, elapsed := runPrimaryRequestWith(t, service, clientCtx, "req-ultra-stream-hold", "")
		var quota *aistudio.PrimaryQuotaError
		if !errors.As(err, &quota) || quota.Reason != primaryQuotaReasonNoWarm || quota.QuotaHits != 1 {
			t.Fatalf("err=%v，期望冷启动的账号额度 429 后没有已启动的 Worker", err)
		}
		if held.Load() != 1 {
			t.Fatalf("请求自己的现场冷启动期间等待保持了 %d 次，期望 1 次（后台扩容不算）", held.Load())
		}
		extension, starting := hold.State()
		if starting || extension < launch || extension > elapsed {
			t.Fatalf("等待延长 %s（进行中=%v），期望约冷启动时长 %s、不超过总耗时 %s", extension, starting, launch, elapsed)
		}
	})
	t.Run("普通号池的主用模型", func(t *testing.T) {
		service := coldStartTestService(t, launch, nil, primaryTestA)
		service.service = &ultraPrimaryUpstream{}
		preverifyRuntimeModelAccess(t, service.pool, primaryTestA)
		service.setPrimaryModels([]string{testRuntimeModel}, 200*time.Millisecond)
		held := watchLaunch(service)
		hold := aistudio.NewStreamStartHold()
		clientCtx := aistudio.WithStreamStartHold(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeNormal), hold)
		text, err, elapsed := runPrimaryRequestWith(t, service, clientCtx, "req-normal-stream-hold", "")
		if err != nil || text != "hello" || elapsed < launch {
			t.Fatalf("text=%q err=%v 用时 %s，期望现场冷启动 A 后完成", text, err, elapsed)
		}
		if extension, starting := hold.State(); held.Load() != 0 || extension != 0 || starting {
			t.Fatalf("普通号池的主用模型不应推迟写出响应头：保持 %d 次，延长 %s，进行中=%v", held.Load(), extension, starting)
		}
		select {
		case <-hold.Changed():
			t.Fatal("普通号池的主用模型不应改动等待延长记录")
		default:
		}
	})
}

// TestUltraPrimaryStreamHoldBoundedByCredit Ultra 主用模型的流式请求连着现场冷启动几个备用账号（每次启动都失败）：写出响应头前的
// 等待合计最多推迟排队预算能抵扣的启动时长（INIT_TIMEOUT），抵扣用完后的启动不再推迟。原先每次现场冷启动都把响应头压到启动结束，
// 第一次启动挂到 INIT_TIMEOUT 失败后再冷启动一个，客户端最长约 等待 + 2×INIT_TIMEOUT 收不到任何字节，多等的时间却抵扣不了，
// 之后的额度不足照样是 200 之后的流内错误
func TestUltraPrimaryStreamHoldBoundedByCredit(t *testing.T) {
	const launch = 600 * time.Millisecond
	const initTimeout = 700 * time.Millisecond
	const primaryTestC = "primary-c@example.com"
	accounts := []string{primaryTestA, primaryTestB, primaryTestC}
	service := coldStartTestService(t, launch, map[string]bool{primaryTestA: true, primaryTestB: true, primaryTestC: true}, accounts...)
	service.service = &ultraPrimaryUpstream{}
	markUltra(t, service.pool, accounts...)
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 200*time.Millisecond)
	service.workers.initTimeout.Store(int64(initTimeout))
	// 记下每次请求自己的现场冷启动开始时，写出响应头前的等待是否推迟、要延长多久
	type launchState struct {
		held      bool
		extension time.Duration
	}
	var mu sync.Mutex
	var launches []launchState
	inner := service.workers.launch
	service.workers.launch = func(ctx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		if hold := aistudio.StreamStartHoldFromContext(ctx); hold != nil {
			_, starting := hold.State()
			mu.Lock()
			launches = append(launches, launchState{held: starting, extension: hold.Extension()})
			mu.Unlock()
		}
		return inner(ctx, accountID, options)
	}
	hold := aistudio.NewStreamStartHold()
	clientCtx := aistudio.WithStreamStartHold(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), hold)
	if _, err, _ := runPrimaryRequestWith(t, service, clientCtx, "req-ultra-hold-credit", ""); err == nil {
		t.Fatal("备用账号全部启动失败时请求应以错误结束")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(launches) < 3 {
		t.Fatalf("请求自己现场冷启动了 %d 次，测试要求至少 3 次：%+v", len(launches), launches)
	}
	// 第一次启动：最多推迟 INIT_TIMEOUT；第二次只剩约 100ms 可抵扣，进行中合计仍是 INIT_TIMEOUT；第三次抵扣已用完，不推迟
	if !launches[0].held || !launches[1].held {
		t.Fatalf("还有抵扣时启动期间应推迟写出响应头：%+v", launches)
	}
	for index, state := range launches {
		if state.extension > initTimeout {
			t.Fatalf("第 %d 次启动期间要延长 %s，超过 INIT_TIMEOUT %s：%+v", index+1, state.extension, initTimeout, launches)
		}
		if index >= 2 && state.held {
			t.Fatalf("第 %d 次启动时抵扣已用完，不应再推迟写出响应头：%+v", index+1, launches)
		}
	}
	if extension, starting := hold.State(); starting || extension != initTimeout {
		t.Fatalf("结束后延长 %s（进行中=%v），期望抵扣上限 %s", extension, starting, initTimeout)
	}
}

// TestUltraPrimaryStreamedQuotaAfterColdStart 流式的 Ultra 主用模型请求：没有已启动的 Worker，第一次尝试现场冷启动备用账号，
// 启动时长超过写出响应头前的等待（至少一个心跳间隔 10s），启动后上游返回额度 429、额度失败后的选号没有已启动的 Worker：
// 仍以 HTTP 429（带 Retry-After）返回，不是先回 200 再在流里报错（原先客户端与中转拿不到 429，用量账本也记 200）
func TestUltraPrimaryStreamedQuotaAfterColdStart(t *testing.T) {
	if testing.Short() {
		t.Skip("需要约 11 秒")
	}
	const launch = 10500 * time.Millisecond
	upstream := &ultraPrimaryUpstream{allQuota: true}
	service := coldStartTestService(t, launch, nil, primaryTestA, primaryTestB)
	service.service = upstream
	markUltra(t, service.pool, primaryTestA, primaryTestB)
	setUltraPrimaryModels(service, []string{testRuntimeModel}, time.Second)
	service.state.Store(serviceRunning)
	service.dataContext = service.lifecycle
	if wait := service.StreamStartWait(aistudio.ContextWithPoolScope(context.Background(), aistudio.PoolScopeUltra), testRuntimeModel); wait >= launch {
		t.Fatalf("StreamStartWait = %s，测试要求冷启动比它长", wait)
	}
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test"})
	body := `{"model":"` + testRuntimeModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/ultra/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	startedAt := time.Now()
	handler.ServeHTTP(recorder, request)
	elapsed := time.Since(startedAt)
	if recorder.Code != http.StatusTooManyRequests || strings.Contains(recorder.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("status=%d content-type=%q 用时 %s body=%s，期望以 HTTP 429 返回", recorder.Code,
			recorder.Header().Get("Content-Type"), elapsed, recorder.Body.String())
	}
	if seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After")); err != nil || seconds < 4 || seconds > 6 {
		t.Fatalf("Retry-After = %q，期望约 5 秒", recorder.Header().Get("Retry-After"))
	}
	if elapsed < launch {
		t.Fatalf("用时 %s，期望包含第一次尝试的现场冷启动 %s", elapsed, launch)
	}
	if calls := upstream.called(); len(calls) != 1 {
		t.Fatalf("上游调用 = %v，期望只有冷启动的那个账号", calls)
	}
}

// TestUltraPrimaryPlaygroundFirstFallsBack 降级判定的 Playground 优先选号在 Playground 候选全部冷却（不到一分钟）时立即返回全部冷却，
// 调用方照原逻辑改为不限通道选号，在同一账号的 Build 通道完成，不对外返回 429
func TestUltraPrimaryPlaygroundFirstFallsBack(t *testing.T) {
	upstream := &ultraPrimaryUpstream{}
	service, pool := primaryBusyService(t, upstream, primaryTestA)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	markUltra(t, pool, primaryTestA)
	if err := pool.SetBuildCatalog(primaryTestA, []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
	// 只冷却 Playground 通道（冷却键为模型名），Build 通道照常可用
	coolModelFor(t, pool, primaryTestA, testRuntimeModel, 20*time.Second)
	guard := config.DefaultDowngradeGuard()
	guard.Enabled, guard.Models = true, []string{testRuntimeModel}
	service.setDowngradeGuard(guard)
	service.state.Store(serviceRunning)
	service.dataContext = service.lifecycle
	ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), 10*time.Second)
	defer cancel()
	// 选号本身：Playground 优先在冷却不到一分钟时也立即返回全部冷却，调用方据此改为不限通道选号
	playground := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent", Pool: aistudio.PoolScopeUltra, PlaygroundFirst: true}
	startedAt := time.Now()
	_, err := service.acquireWarmLease(withLeaseWaitBudget(ctx, service.primaryBudget(testRuntimeModel, aistudio.PoolScopeUltra)), playground)
	if !playgroundUnavailable(err) || time.Since(startedAt) > 200*time.Millisecond {
		t.Fatalf("Playground 优先选号 err=%v 用时 %s，期望立即返回全部冷却", err, time.Since(startedAt))
	}
	request := aistudio.GenerateRequest{
		ID: "req-ultra-playground-first", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	startedAt = time.Now()
	events, err := service.Generate(ctx, request)
	if err != nil {
		t.Fatalf("Generate err=%v，期望改为不限通道选号后完成", err)
	}
	var text string
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventError:
			t.Fatalf("不应以错误结束: %v", event.Err)
		}
	}
	if text != "hello" || time.Since(startedAt) > 3*time.Second {
		t.Fatalf("text=%q 用时 %s，期望在 Build 通道立即完成", text, time.Since(startedAt))
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{primaryTestA}) {
		t.Fatalf("上游调用 = %v", calls)
	}
}

// TestUltraPrimaryInputLimitNotQuota 以 429 返回的输入超限不算额度失败：按 400 的原错误结束，不换号、不按额度不足返回
func TestUltraPrimaryInputLimitNotQuota(t *testing.T) {
	upstream := &ultraPrimaryUpstream{inputLimit: map[string]bool{primaryTestA: true, primaryTestB: true}}
	service, pool := primaryBusyService(t, upstream, primaryTestA, primaryTestB)
	markUltra(t, pool, primaryTestA, primaryTestB)
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
	_, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-input", "")
	var quota *aistudio.PrimaryQuotaError
	if errors.As(err, &quota) || !aistudio.InputTokenLimitError(err) {
		t.Fatalf("err=%v，期望以输入超限的原错误结束", err)
	}
	attempts := upstream.called()
	if len(attempts) != 1 {
		t.Fatalf("上游调用 = %v，期望不换号", attempts)
	}
	if progressLogged(service, "额度 429") {
		t.Fatal("输入超限不应记为额度失败")
	}
	if status, _ := pool.StatusOf(attempts[0]); status.State == aistudio.AccountCooldown {
		t.Fatal("输入超限不应冷却账号")
	}
}

// TestUltraPrimaryFileBoundCopiesFiles 文件绑定的 Ultra 主用请求：所属账号额度 429 后仍先把文件复制到其他账号再试（不按额度不足结束），
// 在已启动的另一个 Worker 上完成
func TestUltraPrimaryFileBoundCopiesFiles(t *testing.T) {
	const resourceID = "files/ultra-primary"
	upstream := &ultraPrimaryUpstream{quota: map[string]bool{primaryTestA: true}}
	service, pool := primaryBusyService(t, upstream, primaryTestA, primaryTestB)
	markUltra(t, pool, primaryTestA, primaryTestB)
	preverifyRuntimeModelAccess(t, pool, primaryTestB)
	if err := pool.BindResource(resourceID, primaryTestA); err != nil {
		t.Fatal(err)
	}
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
	text, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-file", resourceID)
	if err != nil || text != "hello" {
		t.Fatalf("所属账号额度 429 后应复制文件换号完成：text=%q err=%v", text, err)
	}
	if got := upstream.called(); !slices.Equal(got, []string{primaryTestA, primaryTestB}) {
		t.Fatalf("上游调用 = %v", got)
	}
	if copies := upstream.copied(); !slices.Equal(copies, []string{primaryTestA, primaryTestB}) {
		t.Fatalf("文件复制目标 = %v，期望先在所属账号、再复制到 B", copies)
	}
	if progressLogged(service, "Ultra 主用模型额度不足") {
		t.Fatal("文件所属账号不可用时应先复制文件再试，不按额度不足结束")
	}
}

// TestUltraPrimaryAccountCoolingDownCounts 租约账号在发出前进入冷却（ErrAccountCoolingDown，请求没有发到上游）：
// 任何模型都按已冷却处理，不计入连续失败上限、不退避；Ultra 主用模型只在拦下它的冷却剩余不少于 rotationMinRemaining 时记一次每日类额度失败
func TestUltraPrimaryAccountCoolingDownCounts(t *testing.T) {
	// coolingUpstream 让每个账号都返回裸哨兵
	coolingUpstream := func(accounts []string) *ultraPrimaryUpstream {
		cooling := make(map[string]bool, len(accounts))
		for _, accountID := range accounts {
			cooling[accountID] = true
		}
		return &ultraPrimaryUpstream{coolingDown: cooling}
	}
	// coolBeforeBlocked 让账号返回裸哨兵前先写入 cooldown 时长的全局冷却（模拟同账号别的请求刚遇到额度 429）
	coolBeforeBlocked := func(upstream *ultraPrimaryUpstream, pool *aistudio.AccountPool, cooldown time.Duration) {
		upstream.beforeCoolingDown = func(accountID string) {
			_ = pool.MarkCooldownIfGeneration(accountID, "", pool.ModelAccessGeneration(accountID), time.Now(), time.Now().Add(cooldown), "每日限额")
		}
	}
	t.Run("Ultra 主用模型长冷却计数", func(t *testing.T) {
		accounts := ultraTestIDs(4)
		upstream := coolingUpstream(accounts)
		service, pool := primaryBusyService(t, upstream, accounts...)
		coolBeforeBlocked(upstream, pool, 2*time.Hour)
		markUltra(t, pool, accounts...)
		setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
		_, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-cooling-long", "")
		var quota *aistudio.PrimaryQuotaError
		if !errors.As(err, &quota) || quota.Reason != primaryQuotaReasonLimit || quota.QuotaHits != 3 || quota.RateHits != 0 ||
			!errors.Is(quota.Cause, aistudio.ErrAccountCoolingDown) {
			t.Fatalf("err=%v，期望每日类额度失败达到换号上限", err)
		}
		if got := upstream.called(); len(got) != primaryQuotaSwitchLimit+1 {
			t.Fatalf("上游调用 = %v，期望 3 次", got)
		}
		// 裸哨兵没有恢复时间：建议重试取 5s
		if retry := time.Until(quota.RetryAt()); retry < 4*time.Second || retry > 6*time.Second {
			t.Fatalf("建议重试间隔 = %s，期望约 5s", retry)
		}
		// 一般失败的退避为 200ms + 400ms：不退避时远小于它
		if elapsed > 400*time.Millisecond {
			t.Fatalf("用时 %s，期望不退避", elapsed)
		}
		if !progressLogged(service, "额度 429，换号重试（只用已启动的 Worker）| 额度失败=1/2 | 类型=发出前已冷却") {
			t.Fatal("请求进度应写明发出前已冷却记一次每日类额度失败")
		}
		assertNoBusyAccounts(t, pool)
	})
	t.Run("Ultra 主用模型短冷却不计数", func(t *testing.T) {
		accounts := ultraTestIDs(4)
		upstream := coolingUpstream(accounts)
		service, pool := primaryBusyService(t, upstream, accounts...)
		coolBeforeBlocked(upstream, pool, 30*time.Second)
		markUltra(t, pool, accounts...)
		setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
		_, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-cooling-short", "")
		var quota *aistudio.PrimaryQuotaError
		if errors.As(err, &quota) || !errors.Is(err, aistudio.ErrAccountCoolingDown) {
			t.Fatalf("err=%v，期望换号次数用完后以账户已进入冷却结束、不按额度不足结束", err)
		}
		if got := upstream.called(); len(got) != len(accounts) {
			t.Fatalf("上游调用 = %v，期望 4 个账号都试一遍", got)
		}
		if elapsed > 400*time.Millisecond {
			t.Fatalf("用时 %s，期望不退避", elapsed)
		}
		if progressLogged(service, "额度 429") || progressLogged(service, "限流 429") {
			t.Fatal("短冷却的连带拦截不应记额度失败")
		}
		assertNoBusyAccounts(t, pool)
	})
	t.Run("非主用模型不计入连续失败上限", func(t *testing.T) {
		accounts := ultraTestIDs(6)
		upstream := coolingUpstream(accounts)
		service, pool := primaryBusyService(t, upstream, accounts...)
		markUltra(t, pool, accounts...)
		_, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-other-cooling-down", "")
		var quota *aistudio.PrimaryQuotaError
		if errors.As(err, &quota) || !errors.Is(err, aistudio.ErrAccountCoolingDown) {
			t.Fatalf("非主用模型 err=%v，期望以账户已进入冷却结束", err)
		}
		// 6 次超过连续失败上限 generateFailureLimit=5：发出前已冷却不计入
		if got := upstream.called(); len(got) != len(accounts) {
			t.Fatalf("上游调用 = %v，期望 6 个账号都试一遍", got)
		}
		if elapsed > 400*time.Millisecond {
			t.Fatalf("用时 %s，期望不退避", elapsed)
		}
		if progressLogged(service, "已连续 5 次失败，不再换号") {
			t.Fatal("发出前已冷却不应计入连续失败上限")
		}
		assertNoBusyAccounts(t, pool)
	})
}

// TestPrimaryQuotaClassOf 额度 429 的类别：每日限额与剩余不少于 rotationMinRemaining 的冷却按每日类，分钟限额与限流按限流类
func TestPrimaryQuotaClassOf(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name     string
		cooldown aistudio.QuotaCooldown
		want     primaryQuotaClass
	}{
		{name: "每日限额", cooldown: aistudio.QuotaCooldown{Kind: aistudio.DailyQuotaKind, Until: now.Add(time.Hour)}, want: primaryQuotaDaily},
		{name: "分钟限额剩余 50s", cooldown: aistudio.QuotaCooldown{Kind: aistudio.MinuteQuotaKind, Until: now.Add(50 * time.Second)}, want: primaryQuotaRate},
		{name: "限流剩余 30s", cooldown: aistudio.QuotaCooldown{Kind: aistudio.RateLimitKind, Until: now.Add(30 * time.Second)}, want: primaryQuotaRate},
		{name: "分钟限额剩余 20m", cooldown: aistudio.QuotaCooldown{Kind: aistudio.MinuteQuotaKind, Until: now.Add(20 * time.Minute)}, want: primaryQuotaDaily},
	} {
		if got := primaryQuotaClassOf(test.cooldown, now); got != test.want {
			t.Fatalf("%s：类别 = %d，期望 %d", test.name, got, test.want)
		}
	}
	// 用户样本：窗口开始于 10 秒前，冷却到窗口结束，按限流类
	cooldown, ok := aistudio.QuotaCooldownForError(globalMinuteQuotaError(now.Add(-10*time.Second)), now)
	if !ok || cooldown.Kind != aistudio.MinuteQuotaKind || !cooldown.Global {
		t.Fatalf("用户样本的冷却 = %+v ok=%t", cooldown, ok)
	}
	if got := primaryQuotaClassOf(cooldown, now); got != primaryQuotaRate {
		t.Fatalf("用户样本的类别 = %d，期望限流类", got)
	}
}

// TestUltraPrimaryMinuteQuotaWiderLimit 8 个热账号都返回分钟限额 429：限流类另有更宽的换号上限，6 次后按限流失败已达换号上限结束
// （Retry-After 为本请求遇到的限流冷却中最早的恢复，约 50s），每个试过的账号整个冷却，不退避
func TestUltraPrimaryMinuteQuotaWiderLimit(t *testing.T) {
	accounts := ultraTestIDs(8)
	windowStart := time.Now().Add(-10 * time.Second)
	upstream := &ultraPrimaryUpstream{
		allQuota: true, quotaErr: func(string) error { return globalMinuteQuotaError(windowStart) },
	}
	service, pool := primaryBusyService(t, upstream, accounts...)
	markUltra(t, pool, accounts...)
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
	_, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-minute", "")
	var quota *aistudio.PrimaryQuotaError
	if !errors.As(err, &quota) || quota.Reason != primaryQuotaReasonRateLimit || quota.RateHits != primaryRateLimitSwitchLimit+1 || quota.QuotaHits != 0 {
		t.Fatalf("err=%v，期望限流失败已达换号上限", err)
	}
	calls := upstream.called()
	if len(calls) != primaryRateLimitSwitchLimit+1 {
		t.Fatalf("上游调用 = %v，期望 %d 次", calls, primaryRateLimitSwitchLimit+1)
	}
	// window_start_time 只到秒：窗口结束为取整后的窗口开始加 1 分钟，距现在 49 到 50 秒之间（减去请求用时），按它精确比较
	if want := time.Unix(windowStart.Unix(), 0).Add(time.Minute); !quota.RetryAt().Equal(want) {
		t.Fatalf("建议重试时间 = %s（%s 后），期望分钟窗口结束 %s", quota.RetryAt(), time.Until(quota.RetryAt()), want)
	}
	// 一般失败的退避为 200ms + 400ms + 800ms + …：不退避时远小于它
	if elapsed > time.Second {
		t.Fatalf("用时 %s，期望不退避", elapsed)
	}
	for _, fragment := range []string{
		"限流 429，换号重试（只用已启动的 Worker）| 限流失败=1/5 | 类型=分钟限额",
		"限流失败=5/5",
		"Ultra 主用模型额度不足，立即返回 429 | 原因=限流失败已达换号上限 | 额度失败=0 | 限流失败=6 | 总耗时=",
	} {
		if !progressLogged(service, fragment) {
			t.Fatalf("请求进度应写明 %q", fragment)
		}
	}
	for _, accountID := range calls {
		if !pool.AccountCoolingDown(accountID, "") {
			t.Fatalf("试过的账号 %s 应整个冷却", accountID)
		}
	}
	if !strings.Contains(quota.Error(), "限流失败 6 次") {
		t.Fatalf("错误文案 %q 应写明限流失败次数", quota.Error())
	}
	assertNoBusyAccounts(t, pool)
}

// TestUltraPrimaryMixedQuotaClasses 两类额度失败分开计数：前 3 次分钟限额（不到限流类上限）、后 3 次每日限额，
// 第 6 次尝试时每日类超过换号上限，按额度失败已达换号上限结束
func TestUltraPrimaryMixedQuotaClasses(t *testing.T) {
	accounts := ultraTestIDs(6)
	windowStart := time.Now().Add(-10 * time.Second)
	var calls atomic.Int64
	upstream := &ultraPrimaryUpstream{allQuota: true, quotaErr: func(string) error {
		if calls.Add(1) <= 3 {
			return globalMinuteQuotaError(windowStart)
		}
		return ultraDailyQuotaError()
	}}
	service, pool := primaryBusyService(t, upstream, accounts...)
	markUltra(t, pool, accounts...)
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
	_, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-mixed", "")
	var quota *aistudio.PrimaryQuotaError
	if !errors.As(err, &quota) || quota.Reason != primaryQuotaReasonLimit || quota.QuotaHits != 3 || quota.RateHits != 3 {
		t.Fatalf("err=%v，期望每日类达到换号上限（额度失败 3、限流失败 3）", err)
	}
	if got := upstream.called(); len(got) != 6 {
		t.Fatalf("上游调用 = %v，期望 6 次", got)
	}
	for _, fragment := range []string{
		"限流 429，换号重试（只用已启动的 Worker）| 限流失败=3/5 | 类型=分钟限额 | 额度失败=0/2",
		"额度 429，换号重试（只用已启动的 Worker）| 额度失败=2/2 | 类型=每日限额",
		"原因=额度失败已达换号上限 | 额度失败=3 | 限流失败=3",
	} {
		if !progressLogged(service, fragment) {
			t.Fatalf("请求进度应写明 %q", fragment)
		}
	}
	assertNoBusyAccounts(t, pool)
}

// TestUltraPrimaryConcurrentQuota 多个 Ultra 主用请求同时遇到额度耗尽：都按 429（额度不足或全部冷却，带 Retry-After）结束，
// 不留下租约、等待者与活动请求（配合 -race 检查数据竞争）
func TestUltraPrimaryConcurrentQuota(t *testing.T) {
	accounts := ultraTestIDs(6)
	upstream := &ultraPrimaryUpstream{allQuota: true}
	service, pool := primaryBusyService(t, upstream, accounts...)
	pool.SetPerAccountConcurrency(2)
	markUltra(t, pool, accounts...)
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 2*time.Second)
	const requests = 8
	var wait sync.WaitGroup
	var limited atomic.Int64
	failures := make(chan error, requests)
	for index := range requests {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, fmt.Sprintf("req-ultra-concurrent-%d", index), "")
			var quota *aistudio.PrimaryQuotaError
			var cooling *aistudio.AllCoolingError
			var busy *aistudio.CapacityBusyError
			switch {
			case errors.As(err, &quota) && !quota.RetryAt().IsZero():
				limited.Add(1)
			case errors.As(err, &cooling) && !cooling.RetryAt().IsZero():
				limited.Add(1)
			case errors.As(err, &busy):
				limited.Add(1)
			default:
				failures <- err
			}
		}(index)
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("并发请求应按 429 结束，实际 err=%v", err)
	}
	if limited.Load() != requests {
		t.Fatalf("按 429 结束的请求 = %d，期望 %d", limited.Load(), requests)
	}
	if got := len(upstream.called()); got > requests*(primaryQuotaSwitchLimit+1) {
		t.Fatalf("上游调用 %d 次，超过每个请求 %d 次的上限", got, primaryQuotaSwitchLimit+1)
	}
	if count := service.requests.count(); count != 0 {
		t.Fatalf("请求结束后仍有 %d 个活动请求", count)
	}
	if waiters := dispatchWaiters(service); waiters != 0 {
		t.Fatalf("请求结束后仍有 %d 个等待者", waiters)
	}
	assertNoBusyAccounts(t, pool)
}

// TestPrimaryQuotaRetryAt 额度不足的建议重试时间：没有已启动的 Worker 为 5s；全部冷却取最早恢复与本请求遇到的冷却中较早者，不设上限；
// 换号上限与没有其他可换的账号取 5s 与本请求遇到的冷却中较早者；都不早于 1s
func TestPrimaryQuotaRetryAt(t *testing.T) {
	now := time.Now()
	far := now.Add(3 * time.Hour)
	for _, test := range []struct {
		name    string
		reason  string
		quota   time.Time
		cooling *aistudio.AllCoolingError
		want    time.Time
	}{
		{name: "没有已启动的 Worker", reason: primaryQuotaReasonNoWarm, quota: now.Add(time.Second), want: now.Add(primaryRetryFallback)},
		{name: "全部冷却取最早恢复", reason: primaryQuotaReasonCooling, quota: far, cooling: &aistudio.AllCoolingError{Until: now.Add(2 * time.Hour)}, want: now.Add(2 * time.Hour)},
		{name: "全部冷却时本请求的冷却更早", reason: primaryQuotaReasonCooling, quota: now.Add(40 * time.Second), cooling: &aistudio.AllCoolingError{Until: far}, want: now.Add(40 * time.Second)},
		{name: "全部冷却不早于 1s", reason: primaryQuotaReasonCooling, cooling: &aistudio.AllCoolingError{Until: now.Add(100 * time.Millisecond)}, want: now.Add(primaryRetryMin)},
		{name: "全部冷却没有时间时按 5s", reason: primaryQuotaReasonCooling, want: now.Add(primaryRetryFallback)},
		{name: "换号上限", reason: primaryQuotaReasonLimit, quota: far, want: now.Add(primaryRetryFallback)},
		{name: "换号上限时本请求的冷却更早", reason: primaryQuotaReasonLimit, quota: now.Add(2 * time.Second), want: now.Add(2 * time.Second)},
		{name: "没有其他可换的账号", reason: primaryQuotaReasonNoOther, want: now.Add(primaryRetryFallback)},
		{name: "限流上限取限流冷却的恢复", reason: primaryQuotaReasonRateLimit, quota: now.Add(40 * time.Second), want: now.Add(40 * time.Second)},
		{name: "限流上限没有时间时按 5s", reason: primaryQuotaReasonRateLimit, want: now.Add(primaryRetryFallback)},
		{name: "限流上限不超过一分钟", reason: primaryQuotaReasonRateLimit, quota: now.Add(90 * time.Second), want: now.Add(cooldownQueueLimit)},
		{name: "限流上限不早于 1s", reason: primaryQuotaReasonRateLimit, quota: now.Add(100 * time.Millisecond), want: now.Add(primaryRetryMin)},
	} {
		if got := primaryQuotaRetryAt(test.reason, test.quota, test.cooling, now); !got.Equal(test.want) {
			t.Fatalf("%s：建议重试 = %s，期望 %s", test.name, got.Sub(now), test.want.Sub(now))
		}
	}
	quota := &aistudio.PrimaryQuotaError{
		ModelID: testRuntimeModel, Pool: aistudio.PoolScopeUltra, Reason: primaryQuotaReasonLimit, QuotaHits: 3, RateHits: 2,
		Elapsed: 1500 * time.Millisecond, Until: now.Add(5 * time.Second), Cause: errors.New("上游原文"),
	}
	message := quota.Error()
	for _, fragment := range []string{"Ultra 号池", testRuntimeModel, primaryQuotaReasonLimit, "额度失败 3 次", "限流失败 2 次", "总耗时 1.5s", "建议重试时间", "上游原文"} {
		if !strings.Contains(message, fragment) {
			t.Fatalf("错误文案 %q 缺少 %q", message, fragment)
		}
	}
	if !quota.RetryAt().Equal(quota.Until) || errors.Unwrap(quota) != nil {
		t.Fatal("RetryAt 应返回 Until，且不实现 Unwrap")
	}
}

// TestUltraPrimaryWarmOnlyRateLimitedExpands 只用已启动 Worker 的选号：热 Worker A 每分钟请求已满（最长一分钟才释放）、B、C 是没冷却的
// 备用账号且分区有空槽时，照常按账号全部忙碌排队到预算用完返回号池繁忙，同时只为 B 发起一次后台扩容（不等它）；A 只是并发忙碌时不扩容。
// 预算长于兜底复查（schedulingRecheck）、短于冷启动：选号期间至少复查一次，复查时 B 还在启动、C 仍是能扩容的备用账号，不再扩容 C
func TestUltraPrimaryWarmOnlyRateLimitedExpands(t *testing.T) {
	const primaryTestC = "primary-c@example.com"
	const launch = 3 * time.Second
	const budget = 1500 * time.Millisecond
	if budget <= schedulingRecheck || budget >= launch {
		t.Fatalf("预算 %s 应长于兜底复查 %s、短于冷启动 %s", budget, schedulingRecheck, launch)
	}
	service, launched := ultraColdStartService(t, &ultraPrimaryUpstream{}, launch, primaryTestC)
	setUltraPrimaryModels(service, []string{testRuntimeModel}, budget)
	ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), 10*time.Second)
	defer cancel()
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent", Pool: aistudio.PoolScopeUltra}
	warmOnlyCtx := func() context.Context {
		return withoutColdStart(withLeaseWaitBudget(ctx, service.primaryBudget(testRuntimeModel, aistudio.PoolScopeUltra)))
	}
	// A 并发忙碌：排队到预算用完（期间至少复查一次），不扩容。后台扩容是异步的，等一段时间再看有没有启动
	release := holdLease(t, service.pool, primaryTestA)
	lease, err := service.acquireWarmLease(warmOnlyCtx(), selection)
	var busy *aistudio.CapacityBusyError
	if lease != nil || !errors.As(err, &busy) || busy.Reason != "账号全部忙碌" {
		t.Fatalf("A 并发忙碌时 lease=%v err=%v，期望按账号全部忙碌返回号池繁忙", lease, err)
	}
	select {
	case accountID := <-launched:
		t.Fatalf("A 只是并发忙碌时不应扩容，实际启动了 %s", accountID)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	// 第二阶段只看自己发起的启动：排空 launched，前面晚到的启动记录不会被当成 B 的扩容
	for drained := false; !drained; {
		select {
		case accountID := <-launched:
			t.Fatalf("A 只是并发忙碌时不应扩容，实际启动了 %s", accountID)
		default:
			drained = true
		}
	}

	// A 每分钟请求已满
	service.pool.SetAccountRateLimit(1)
	fillRateWindow(t, service.pool, primaryTestA, 1)
	startedAt := time.Now()
	lease, err = service.acquireWarmLease(warmOnlyCtx(), selection)
	elapsed := time.Since(startedAt)
	if lease != nil || !errors.As(err, &busy) || busy.Reason != "账号全部忙碌" || elapsed < budget {
		t.Fatalf("A 每分钟已满时 lease=%v err=%v 用时 %s，期望按账号全部忙碌排队约 %s 后返回号池繁忙", lease, err, elapsed, budget)
	}
	if elapsed >= launch {
		t.Fatalf("用时 %s，不应等 B 冷启动完成", elapsed)
	}
	select {
	case accountID := <-launched:
		if accountID != primaryTestB {
			t.Fatalf("后台扩容启动了 %s，期望 B", accountID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("热 Worker 每分钟已满时应为备用账号 B 发起后台扩容")
	}
	select {
	case accountID := <-launched:
		t.Fatalf("一次选号只应扩容一次，又启动了 %s", accountID)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestUltraPrimaryWarmOnlyOpeningKeepsBusyReason 只用已启动 Worker 的选号满窗时为唯一的备用账号 B 发起扩容后，B 进入启动中、
// 没有别的备用账号：之后的复查不按 Worker 正在启动等待（它不排队等后台扩容），仍按账号全部忙碌排队到预算用完。
// 预算长于兜底复查，选号期间一定复查过；原先复查后最后的等待原因变成 Worker 正在启动
func TestUltraPrimaryWarmOnlyOpeningKeepsBusyReason(t *testing.T) {
	const launch = 3 * time.Second
	const budget = 1500 * time.Millisecond
	service, launched := ultraColdStartService(t, &ultraPrimaryUpstream{}, launch)
	setUltraPrimaryModels(service, []string{testRuntimeModel}, budget)
	ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), 10*time.Second)
	defer cancel()
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent", Pool: aistudio.PoolScopeUltra}
	service.pool.SetAccountRateLimit(1)
	fillRateWindow(t, service.pool, primaryTestA, 1)
	startedAt := time.Now()
	lease, err := service.acquireWarmLease(
		withoutColdStart(withLeaseWaitBudget(ctx, service.primaryBudget(testRuntimeModel, aistudio.PoolScopeUltra))), selection)
	elapsed := time.Since(startedAt)
	var busy *aistudio.CapacityBusyError
	if lease != nil || !errors.As(err, &busy) || busy.Reason != "账号全部忙碌" || elapsed < budget || elapsed >= launch {
		t.Fatalf("lease=%v err=%v 用时 %s，期望按账号全部忙碌排队约 %s 后返回号池繁忙", lease, err, elapsed, budget)
	}
	select {
	case accountID := <-launched:
		if accountID != primaryTestB {
			t.Fatalf("后台扩容启动了 %s，期望 B", accountID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("热 Worker 每分钟已满时应为备用账号 B 发起后台扩容")
	}
}

// TestUltraPrimaryWarmOnlyNoWarmExpandsOnce 只用已启动 Worker 的选号满窗时为 B 发起扩容、排队期间 A 被写入冷却（例如同账号
// 别的请求刚遇到额度 429）：复查时没有热候选，B 还在启动、C 是能扩容的备用账号，立即返回额度 429 后没有已启动的 Worker，
// 不再为 C 扩容（原先同一次选号会冷启动 B、C 两个浏览器）
func TestUltraPrimaryWarmOnlyNoWarmExpandsOnce(t *testing.T) {
	const primaryTestC = "primary-c@example.com"
	const launch = 3 * time.Second
	service, launched := ultraColdStartService(t, &ultraPrimaryUpstream{}, launch, primaryTestC)
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 5*time.Second)
	ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), 10*time.Second)
	defer cancel()
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent", Pool: aistudio.PoolScopeUltra}
	service.pool.SetAccountRateLimit(1)
	fillRateWindow(t, service.pool, primaryTestA, 1)
	type result struct {
		lease *aistudio.AccountLease
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lease, err := service.acquireWarmLease(
			withoutColdStart(withLeaseWaitBudget(ctx, service.primaryBudget(testRuntimeModel, aistudio.PoolScopeUltra))), selection)
		done <- result{lease, err}
	}()
	select {
	case accountID := <-launched:
		if accountID != primaryTestB {
			t.Fatalf("后台扩容启动了 %s，期望 B", accountID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("热 Worker 每分钟已满时应为备用账号 B 发起后台扩容")
	}
	now := time.Now()
	if err := service.pool.MarkCooldownIfGeneration(primaryTestA, "", service.pool.ModelAccessGeneration(primaryTestA),
		now, now.Add(time.Hour), "每日限额"); err != nil {
		t.Fatal(err)
	}
	// 写入冷却后的复查最晚在兜底复查（1s）时发生，B 的冷启动（3s）还没结束
	select {
	case got := <-done:
		var noWarm *noImmediateLeaseError
		if got.lease != nil || !errors.As(got.err, &noWarm) || noWarm.reason != primaryQuotaReasonNoWarm {
			t.Fatalf("A 冷却后 lease=%v err=%v，期望额度 429 后没有已启动的 Worker", got.lease, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A 冷却后选号应立即返回没有已启动的 Worker")
	}
	select {
	case accountID := <-launched:
		t.Fatalf("B 还在启动时同一次选号不应再扩容，又启动了 %s", accountID)
	case <-time.After(300 * time.Millisecond):
	}
}
