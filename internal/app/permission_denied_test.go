package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// withPermissionDenied 给测试服务挂上保存在临时目录的 403 无权限账户登记表
func withPermissionDenied(t *testing.T, service *trackedService) *permissionDeniedRegistry {
	t.Helper()
	registry := newPermissionDeniedRegistry(t.TempDir(), service.requests)
	t.Cleanup(registry.flush)
	service.permissionDenied = registry
	return registry
}

// permissionDeniedEntryOf 返回登记表中账户的条目
func permissionDeniedEntryOf(registry *permissionDeniedRegistry, accountID string) (permissionDeniedEntry, bool) {
	for _, entry := range registry.list() {
		if entry.accountID == accountID {
			return entry, true
		}
	}
	return permissionDeniedEntry{}, false
}

// permissionDeniedModelOf 返回账户条目中模型的记录
func permissionDeniedModelOf(entry permissionDeniedEntry, modelID string) (permissionDeniedModel, bool) {
	for _, model := range entry.models {
		if model.modelID == modelID {
			return model.model, true
		}
	}
	return permissionDeniedModel{}, false
}

// TestPermissionDeniedRecordedOnModelForbidden 生成请求对某个模型返回 403 无权限时登记该账户：模型、通道、次数、首次与最近时间、上游原文；
// 没有 403 的账户不登记；管理接口按账户返回，带邮箱与各模型记录
func TestPermissionDeniedRecordedOnModelForbidden(t *testing.T) {
	upstream := &forbiddenUpstream{denied: map[string]bool{forbiddenBad: true}}
	service, pool := primaryBusyService(t, upstream, forbiddenBad, forbiddenGood)
	registry := withPermissionDenied(t, service)
	preverifyRuntimeModelAccess(t, pool, forbiddenGood)
	before := time.Now()
	if text, err, _ := runAccountRequest(t, service, "req-denied-record", ""); err != nil || text != "hello" {
		t.Fatalf("应换到没有 403 的账户完成：text=%q err=%v", text, err)
	}
	entries := registry.list()
	if len(entries) != 1 || entries[0].accountID != forbiddenBad {
		t.Fatalf("登记 = %+v，期望只有 %s", entries, forbiddenBad)
	}
	model, ok := permissionDeniedModelOf(entries[0], testRuntimeModel)
	if !ok || !slices.Equal(model.Channels, []string{string(aistudio.ChannelPlayground)}) || model.Count != 1 {
		t.Fatalf("模型记录 = %+v ok=%v，期望 Playground 通道 1 次", model, ok)
	}
	if model.FirstAt.Before(before) || !model.LastAt.Equal(model.FirstAt) || !strings.Contains(model.Message, "The caller does not have permission") {
		t.Fatalf("模型记录的时间或原文不对：%+v", model)
	}
	// 冷却到期后再 403：次数累加，首次时间不变
	expireModelCooldown(t, service, forbiddenBad, testRuntimeModel)
	var rpc *aistudio.RPCError
	if _, err, _ := runAccountRequest(t, service, "req-denied-record-2", forbiddenBad); !errors.As(err, &rpc) {
		t.Fatalf("固定账户的 403 应原样返回，err=%v", err)
	}
	entry, _ := permissionDeniedEntryOf(registry, forbiddenBad)
	again, _ := permissionDeniedModelOf(entry, testRuntimeModel)
	if again.Count != 2 || !again.FirstAt.Equal(model.FirstAt) || !again.LastAt.After(model.LastAt) {
		t.Fatalf("再次 403 后记录 = %+v，期望次数 2、首次时间不变、最近时间更新", again)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin := &runtimeAdmin{pool: pool, service: service, requests: newRequestRegistry(ctx)}
	listed, err := admin.PermissionDenied(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("管理接口返回 %+v err=%v", listed, err)
	}
	got := listed[0]
	if got.ID != forbiddenBad || got.Label != forbiddenBad || got.Count != 2 || len(got.Models) != 1 ||
		got.Models[0].Model != testRuntimeModel || got.Models[0].Count != 2 || !got.LastAt.Equal(again.LastAt) ||
		!got.FirstAt.Equal(again.FirstAt) || !slices.Equal(got.Models[0].Channels, []string{"playground"}) {
		t.Fatalf("管理接口条目 = %+v", got)
	}
}

// TestPermissionDeniedSkipsSystemicForbidden 号池级 403（号池内多个账户对同一模型连续 403、这段时间没有任何成功）不是账户没有权限：
// 判定成立后不再登记，判定成立之前几个账户刚登记的条目一并撤销；号池里该模型成功之后，单个账户的 403 照常登记
func TestPermissionDeniedSkipsSystemicForbidden(t *testing.T) {
	ids := make([]string, 0, 8)
	denied := make(map[string]bool)
	for index := range 8 {
		id := fmt.Sprintf("a-outage-%d@example.com", index)
		ids = append(ids, id)
		denied[id] = true
	}
	upstream := &forbiddenUpstream{denied: denied}
	service, _ := primaryBusyService(t, upstream, ids...)
	registry := withPermissionDenied(t, service)
	// 判定之前更早就登记过的账户保留
	earlier := time.Now().Add(-2 * forbiddenWindow)
	registry.record(ids[0], testRuntimeModel, aistudio.ChannelPlayground, "earlier", "", earlier)
	var rpc *aistudio.RPCError
	if _, err, _ := runAccountRequest(t, service, "req-denied-outage", ""); !errors.As(err, &rpc) || rpc.StatusCode != http.StatusForbidden {
		t.Fatalf("故障期间应按 403 结束，err=%v", err)
	}
	calls := upstream.called()
	if len(calls) < forbiddenSystemicAccounts {
		t.Fatalf("尝试 = %v，期望至少 %d 个账户", calls, forbiddenSystemicAccounts)
	}
	for _, entry := range registry.list() {
		model, ok := permissionDeniedModelOf(entry, testRuntimeModel)
		if entry.accountID != ids[0] || !ok || !model.FirstAt.Equal(earlier) {
			t.Fatalf("号池级 403 不应登记：%+v", registry.list())
		}
	}
	if _, ok := permissionDeniedEntryOf(registry, ids[0]); !ok {
		t.Fatal("判定之前更早登记的条目应保留")
	}
	// 修复后成功一次，单个账户的 403 照常登记
	for _, id := range ids {
		upstream.setDenied(id, false)
	}
	if text, err, _ := runAccountRequest(t, service, "req-denied-fixed", ""); err != nil || text != "hello" {
		t.Fatalf("修复后应成功：text=%q err=%v", text, err)
	}
	// 判定成立的那个账户没有冷却，固定它再请求一次
	single := calls[forbiddenSystemicAccounts-1]
	upstream.setDenied(single, true)
	if _, err, _ := runAccountRequest(t, service, "req-denied-single", single); !errors.As(err, &rpc) {
		t.Fatalf("应按 403 结束，err=%v", err)
	}
	if _, ok := permissionDeniedEntryOf(registry, single); !ok {
		t.Fatalf("号池里该模型成功之后单个账户的 403 应登记：%+v", registry.list())
	}
}

// TestPermissionDeniedClearedOnSuccessPerChannel 账户之后在该模型的某个通道上成功时只清除这个通道的登记：两个通道都 403 时登记两个通道，
// Build 上成功后只剩 Playground；Playground 也成功后整个账户从列表中移除
func TestPermissionDeniedClearedOnSuccessPerChannel(t *testing.T) {
	upstream := &forbiddenUpstream{denied: map[string]bool{forbiddenBad: true}}
	service, pool := primaryBusyService(t, upstream, forbiddenBad)
	registry := withPermissionDenied(t, service)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	target := aistudio.Model{ID: streamTestModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	if err := pool.SetCatalog(forbiddenBad, aistudio.BenefitTierFree, []aistudio.Model{target}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetBuildCatalog(forbiddenBad, []aistudio.Model{target}); err != nil {
		t.Fatal(err)
	}
	if _, err, _ := runModelRequest(t, service, "req-denied-both", forbiddenBad, streamTestModel); !isPermissionDenied(err) {
		t.Fatalf("两个通道都 403 时应返回 403，err=%v", err)
	}
	channels := func() []string {
		entry, ok := permissionDeniedEntryOf(registry, forbiddenBad)
		if !ok {
			return nil
		}
		model, _ := permissionDeniedModelOf(entry, streamTestModel)
		return model.Channels
	}
	if got := channels(); !slices.Equal(got, []string{"build", "playground"}) {
		t.Fatalf("登记的通道 = %v，期望两个通道", got)
	}
	build := aistudio.ChannelCooldownScope(aistudio.ChannelBuild, streamTestModel)
	expireModelCooldown(t, service, forbiddenBad, build)
	upstream.setDenied(forbiddenBad, false)
	if text, err, _ := runModelRequest(t, service, "req-denied-build-ok", forbiddenBad, streamTestModel); err != nil || text != "hello" {
		t.Fatalf("Build 上应成功：text=%q err=%v", text, err)
	}
	if got := channels(); !slices.Equal(got, []string{"playground"}) {
		t.Fatalf("Build 成功后登记的通道 = %v，期望只剩 Playground", got)
	}
	expireModelCooldown(t, service, forbiddenBad, streamTestModel)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground})
	if text, err, _ := runModelRequest(t, service, "req-denied-playground-ok", forbiddenBad, streamTestModel); err != nil || text != "hello" {
		t.Fatalf("Playground 上应成功：text=%q err=%v", text, err)
	}
	if entries := registry.list(); len(entries) != 0 {
		t.Fatalf("两个通道都成功后应移出列表：%+v", entries)
	}
}

// TestPermissionDeniedClearedOnAuthUpdate 认证文件被外部更新（重新获取后上传）时清除账户的登记、全部 403 无权限冷却（单模型冷却与
// 多模型 403 暂停）与冷却档位，账户立即重新参与调度；额度等其他原因的冷却保持不变；之后再 403 从第一档（30 分钟）重新计
func TestPermissionDeniedClearedOnAuthUpdate(t *testing.T) {
	upstream := &forbiddenUpstream{denied: map[string]bool{forbiddenBad: true}}
	service, pool := primaryBusyService(t, upstream, forbiddenBad)
	registry := withPermissionDenied(t, service)
	models := []string{"model-one", "model-two", "model-three"}
	catalog := make([]aistudio.Model, 0, len(models)+1)
	for _, modelID := range append(slices.Clone(models), testRuntimeModel) {
		catalog = append(catalog, aistudio.Model{ID: modelID, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}})
	}
	if err := pool.SetCatalog(forbiddenBad, aistudio.BenefitTierFree, catalog); err != nil {
		t.Fatal(err)
	}
	for index, modelID := range models {
		if _, err, _ := runModelRequest(t, service, fmt.Sprintf("req-denied-auth-%d", index), forbiddenBad, modelID); err == nil {
			t.Fatalf("模型 %s 应返回 403", modelID)
		}
	}
	if !pool.AccountCoolingDown(forbiddenBad, "") {
		t.Fatal("3 个模型连续 403 后账户应自动暂停")
	}
	// 另有一条每日限额冷却：与登录无关，认证文件更新后保持
	quotaUntil := time.Now().Add(2 * time.Hour)
	if err := pool.MarkCooldownIfGeneration(
		forbiddenBad, testRuntimeModel, pool.ModelAccessGeneration(forbiddenBad), time.Now(), quotaUntil, "每日限额: You exceeded your current quota",
	); err != nil {
		t.Fatal(err)
	}
	if entry, ok := permissionDeniedEntryOf(registry, forbiddenBad); !ok || len(entry.models) != len(models) {
		t.Fatalf("登记 = %+v，期望 3 个模型", registry.list())
	}

	account, err := pool.Account(forbiddenBad)
	if err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteStorageState(account.StoragePath, authTestState(t, "NEW", false)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin := &runtimeAdmin{pool: pool, service: service, workers: service.workers, requests: service.requests}
	var summary aistudio.AccountLoginSummary
	for _, item := range pool.LoginSummaries() {
		if item.ID == forbiddenBad {
			summary = item
		}
	}
	if !admin.applyLoginUpdate(ctx, summary) {
		t.Fatal("账户空闲时应载入新的认证文件")
	}
	if _, ok := permissionDeniedEntryOf(registry, forbiddenBad); ok {
		t.Fatalf("认证文件更新后应清除登记：%+v", registry.list())
	}
	if pool.AccountCoolingDown(forbiddenBad, "") {
		t.Fatal("认证文件更新后应解除多模型 403 暂停")
	}
	for _, modelID := range models {
		if _, reason, cooling := modelCooldownUntil(t, pool, forbiddenBad, modelID); cooling {
			t.Fatalf("认证文件更新后应解除 %s 的 403 冷却，仍有：%s", modelID, reason)
		}
	}
	if _, reason, cooling := modelCooldownUntil(t, pool, forbiddenBad, testRuntimeModel); !cooling || !strings.HasPrefix(reason, "每日限额") {
		t.Fatalf("每日限额冷却应保持：cooling=%v reason=%q", cooling, reason)
	}
	if !hasLogContaining(service.requests, "清除 403 无权限记录与冷却", "来源=认证文件更新") {
		t.Fatalf("应记录清除日志，日志 = %v", serviceLogMessages(service.requests))
	}
	// 载入新的认证文件时关闭了浏览器：换回不启动浏览器的测试 Worker
	restoreStubWorker(t, service.workers, forbiddenBad)
	// 新的认证文件仍然 403：重新登记，冷却从第一档重新计
	if _, err, _ := runModelRequest(t, service, "req-denied-auth-again", forbiddenBad, models[0]); err == nil {
		t.Fatal("应返回 403")
	}
	until, _, cooling := modelCooldownUntil(t, pool, forbiddenBad, models[0])
	if !cooling {
		t.Fatal("再次 403 应冷却")
	}
	assertCooldownAround(t, until, forbiddenModelFirstCooldown, "认证文件更新后再 403")
	if _, ok := permissionDeniedEntryOf(registry, forbiddenBad); !ok {
		t.Fatal("新的认证文件仍然 403 时应重新登记")
	}
}

// restoreStubWorker 给账户装回不启动浏览器的测试 Worker（认证文件更新时 Worker 被关闭）
func restoreStubWorker(t *testing.T, workers *accountWorkerManager, accountID string) {
	t.Helper()
	worker, _ := aistudio.NewStubWorker(accountID, 0)
	workers.mu.RLock()
	account := workers.accounts[accountID]
	workers.mu.RUnlock()
	account.mu.Lock()
	account.worker = worker
	account.mu.Unlock()
	account.warm.Store(true)
}

// TestPermissionDeniedPersistence 登记保存在账户目录的 .permission-denied.json：关闭时立即写入，重新打开后原样恢复；
// 同一进程内同一目录共用一份登记表（重启生成服务时新实例先装配、旧实例后关闭）；文件损坏时从空表开始
func TestPermissionDeniedPersistence(t *testing.T) {
	directory := t.TempDir()
	first := newPermissionDeniedRegistry(directory, nil)
	at := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	first.record("a@example.com", "pro", aistudio.ChannelBuild, "The caller does not have permission", "identity-a", at)
	first.record("a@example.com", "pro", aistudio.ChannelPlayground, "The caller does not have permission", "", at.Add(time.Minute))
	first.record("b@example.com", "flash", aistudio.ChannelPlayground, strings.Repeat("x", permissionDeniedMessageLimit+50), "", at.Add(2*time.Minute))
	first.flush()
	path := filepath.Join(directory, permissionDeniedFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("关闭时应写入 %s：%v", permissionDeniedFileName, err)
	}
	second := newPermissionDeniedRegistry(directory, nil)
	entries := second.list()
	if len(entries) != 2 || entries[0].accountID != "b@example.com" || entries[1].accountID != "a@example.com" {
		t.Fatalf("重新打开后 = %+v，期望按最近一次从新到旧", entries)
	}
	model, ok := permissionDeniedModelOf(entries[1], "pro")
	if !ok || model.Count != 2 || !slices.Equal(model.Channels, []string{"build", "playground"}) ||
		!model.FirstAt.Equal(at) || !model.LastAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("重新打开后的记录 = %+v", model)
	}
	if message := entries[0].models[0].model.Message; len([]rune(message)) != permissionDeniedMessageLimit+1 {
		t.Fatalf("上游原文应截断到 %d 字符，长度 %d", permissionDeniedMessageLimit, len([]rune(message)))
	}
	// 登记时记下的登录签名一并保存（启动时据此判断服务停止期间是否替换了认证文件）；之后没有给出时保留之前的
	if identity, _ := second.identityOf("a@example.com"); identity != "identity-a" {
		t.Fatalf("重新打开后的登录签名 = %q，期望 identity-a", identity)
	}
	// 清除后同样持久化
	if cleared := second.clear([]string{"a@example.com", "missing@example.com"}, false); cleared != 1 {
		t.Fatalf("清除数 = %d，期望 1", cleared)
	}
	second.flush()
	if entries := newPermissionDeniedRegistry(directory, nil).list(); len(entries) != 1 || entries[0].accountID != "b@example.com" {
		t.Fatalf("清除后重新打开 = %+v", entries)
	}

	shared := t.TempDir()
	if openPermissionDeniedRegistry(shared, nil) != openPermissionDeniedRegistry(shared, nil) {
		t.Fatal("同一目录应共用一份登记表")
	}

	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, permissionDeniedFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	if entries := newPermissionDeniedRegistry(broken, requests).list(); len(entries) != 0 {
		t.Fatalf("文件损坏时应从空表开始：%+v", entries)
	}
	if !hasLogContaining(requests, "403 无权限账户记录读取失败") {
		t.Fatal("文件损坏时应记录警告")
	}
}

// TestClearPermissionDeniedAdmin 管理接口按账户或全部清除登记（只清除列表，不改变冷却）；两者都没有给时返回 400
func TestClearPermissionDeniedAdmin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	registry := newPermissionDeniedRegistry("", requests)
	now := time.Now()
	for _, accountID := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		registry.record(accountID, "pro", aistudio.ChannelPlayground, "denied", "", now)
	}
	admin := &runtimeAdmin{service: &trackedService{permissionDenied: registry, requests: requests}, requests: requests}
	var operation *adminOperationError
	if _, err := admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{}); !errors.As(err, &operation) || operation.status != http.StatusBadRequest {
		t.Fatalf("没有账户也没有 all 时应返回 400，err=%v", err)
	}
	cleared, err := admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{AccountIDs: []string{" A@example.com "}})
	if err != nil || cleared != 1 {
		t.Fatalf("按账户清除：cleared=%d err=%v", cleared, err)
	}
	if _, ok := permissionDeniedEntryOf(registry, "a@example.com"); ok {
		t.Fatal("指定的账户应被清除")
	}
	cleared, err = admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{All: true})
	if err != nil || cleared != 2 || len(registry.list()) != 0 {
		t.Fatalf("全部清除：cleared=%d err=%v 剩余=%+v", cleared, err, registry.list())
	}
}

// TestClearPermissionDeniedMixedCaseIDs 从磁盘导入的账户以目录名为 ID，不一定是小写：按 account_ids 清除时只去掉首尾空白、不转小写，
// 原样传入的混合大小写 ID 能清除；大小写不同的写法同样能清除（不区分大小写）
func TestClearPermissionDeniedMixedCaseIDs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	registry := newPermissionDeniedRegistry("", requests)
	now := time.Now()
	for _, accountID := range []string{"John.Doe@Gmail.com", "Jane.Roe@Gmail.com", "other@example.com"} {
		registry.record(accountID, "pro", aistudio.ChannelPlayground, "denied", "", now)
	}
	admin := &runtimeAdmin{service: &trackedService{permissionDenied: registry, requests: requests}, requests: requests}
	cleared, err := admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{AccountIDs: []string{" John.Doe@Gmail.com "}})
	if err != nil || cleared != 1 {
		t.Fatalf("按列表返回的 ID 清除：cleared=%d err=%v，期望 1", cleared, err)
	}
	cleared, err = admin.ClearPermissionDenied(ctx, api.PermissionDeniedClearInput{AccountIDs: []string{"jane.roe@gmail.com"}})
	if err != nil || cleared != 1 {
		t.Fatalf("大小写不同的 ID 清除：cleared=%d err=%v，期望 1", cleared, err)
	}
	if entries := registry.list(); len(entries) != 1 || entries[0].accountID != "other@example.com" {
		t.Fatalf("清除后剩余 = %+v，期望只剩 other@example.com", entries)
	}
}

// runScopedRequest 执行一次固定账户的 testRuntimeModel 生成请求，请求 context 带号池（号池级 403 按请求的号池判断），返回错误
func runScopedRequest(t *testing.T, service *trackedService, requestID string, accountID string, scope aistudio.PoolScope) error {
	t.Helper()
	request := aistudio.GenerateRequest{
		ID: requestID, Model: testRuntimeModel, AccountID: accountID,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	requestCtx, requestCancel := context.WithTimeout(aistudio.ContextWithPoolScope(service.lifecycle, scope), 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	var err error
	for event := range events {
		if event.Kind == aistudio.EventError {
			err = event.Err
		}
	}
	return err
}

// TestPermissionDeniedSystemicDropsWholeWindow 故障紧接着一次成功开始时，号池级 403 判定要等号池里该模型 10 分钟没有成功才成立，
// 这之前每个 403 都按账户级登记（这里是成功之后约 9 分钟内的 40 个账户）。判定成立后撤销号池里该模型自那次成功以来登记的全部条目，
// 不只是判定记着的几个账户；成功之前就登记的、其他号池（Ultra）账户的、其他模型的保留；判定成立之后才写入的登记
// （与判定同时进行的请求）在下一次号池级 403 时同样撤销
func TestPermissionDeniedSystemicDropsWholeWindow(t *testing.T) {
	const trigger, ultra = "z-trigger@example.com", "u-ultra@example.com"
	upstream := &forbiddenUpstream{denied: map[string]bool{trigger: true}}
	service, pool := primaryBusyService(t, upstream, trigger, ultra)
	registry := withPermissionDenied(t, service)
	ultraCatalog := []aistudio.Model{{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}}
	if err := pool.SetCatalog(ultra, aistudio.BenefitTierUltra, ultraCatalog); err != nil {
		t.Fatal(err)
	}
	scope := aistudio.PoolScopeNormal
	success := time.Now().Add(-forbiddenWindow - 30*time.Second)
	service.forbidden.modelSucceeded(scope, testRuntimeModel, success)
	registry.record("before@example.com", testRuntimeModel, aistudio.ChannelPlayground, "denied", "", success.Add(-time.Minute))
	healthy := make([]string, 0, 40)
	for index := range 40 {
		accountID := fmt.Sprintf("a-healthy-%02d@example.com", index)
		at := success.Add(time.Duration(index) * 14 * time.Second)
		if service.forbidden.observeModelDenied(scope, accountID, testRuntimeModel, at) {
			t.Fatalf("第 %d 个 403 距成功不到 %s，不应按号池级 403", index, forbiddenWindow)
		}
		registry.record(accountID, testRuntimeModel, aistudio.ChannelPlayground, "The caller does not have permission", "", at)
		healthy = append(healthy, accountID)
	}
	registry.record(healthy[0], "other-model", aistudio.ChannelPlayground, "denied", "", success.Add(time.Minute))
	registry.record(ultra, testRuntimeModel, aistudio.ChannelPlayground, "denied", "", success.Add(time.Minute))
	if entries := registry.list(); len(entries) != 42 {
		t.Fatalf("判定之前登记 %d 个账户，期望 42", len(entries))
	}

	var rpc *aistudio.RPCError
	if err := runScopedRequest(t, service, "req-systemic-window", trigger, scope); !errors.As(err, &rpc) || rpc.StatusCode != http.StatusForbidden {
		t.Fatalf("应按 403 结束，err=%v", err)
	}
	for _, accountID := range append(slices.Clone(healthy), trigger) {
		entry, listed := permissionDeniedEntryOf(registry, accountID)
		if _, ok := permissionDeniedModelOf(entry, testRuntimeModel); listed && ok {
			t.Fatalf("号池级 403 判定成立后 %s 在该模型上的登记应撤销", accountID)
		}
	}
	remaining := make([]string, 0)
	for _, entry := range registry.list() {
		remaining = append(remaining, entry.accountID)
	}
	slices.Sort(remaining)
	if want := []string{healthy[0], "before@example.com", ultra}; !slices.Equal(remaining, want) {
		t.Fatalf("剩余登记 = %v，期望 %v（成功之前的、其他模型的、Ultra 号池的保留）", remaining, want)
	}
	if entry, _ := permissionDeniedEntryOf(registry, healthy[0]); len(entry.models) != 1 || entry.models[0].modelID != "other-model" {
		t.Fatalf("其他模型的登记应保留：%+v", entry)
	}
	if !hasLogContaining(service.requests, "撤销 403 无权限账户记录", "账户=40") {
		t.Fatalf("应记录撤销了 40 个账户，日志 = %v", serviceLogMessages(service.requests))
	}

	registry.record("late@example.com", testRuntimeModel, aistudio.ChannelPlayground, "denied", "", time.Now())
	service.dropSystemicPermissionDenied(scope, testRuntimeModel, time.Now())
	if _, listed := permissionDeniedEntryOf(registry, "late@example.com"); listed {
		t.Fatal("判定成立之后才写入的登记应在下一次号池级 403 时撤销")
	}
}

// TestPermissionDeniedReconciledAtStartup 按说明在服务停止期间覆盖认证文件后启动：启动时登录签名 Cookie 的指纹与登记时不同的账户
// 移出列表，单模型 403 冷却（持久化在 runtime-state.json）一并解除；认证文件没变的账户仍然列着、仍在冷却；
// 服务停止期间删除了目录的账户的登记直接清除
func TestPermissionDeniedReconciledAtStartup(t *testing.T) {
	isolateDisableNotes(t)
	root := filepath.Join(t.TempDir(), "auth")
	const replaced, kept, gone = "replaced@example.com", "kept@example.com", "gone@example.com"
	replacedDirectory := writeEnableTestAccount(t, root, replaced, true)
	writeEnableTestAccount(t, root, kept, true)
	reason := forbiddenModelReasonPrefix + "The caller does not have permission"
	until := time.Now().Add(forbiddenModelRepeatCooldown)
	{
		// 停止之前的一代：两个账户都返回过 403，登记并写入单模型 403 冷却
		accounts, failures := aistudio.NewAccountStore(root).LoadTolerant()
		if len(failures) > 0 {
			t.Fatal(failures)
		}
		previous := aistudio.NewAccountPool(accounts, 1)
		registry := newPermissionDeniedRegistry(root, nil)
		for _, accountID := range []string{replaced, kept} {
			if err := previous.MarkCooldownIfGeneration(
				accountID, testRuntimeModel, previous.ModelAccessGeneration(accountID), time.Now(), until, reason,
			); err != nil {
				t.Fatal(err)
			}
			identity, _ := previous.LoginIdentity(accountID)
			registry.record(accountID, testRuntimeModel, aistudio.ChannelPlayground, "The caller does not have permission", identity, time.Now())
		}
		identity, _ := previous.LoginIdentity(kept)
		registry.record(gone, testRuntimeModel, aistudio.ChannelPlayground, "The caller does not have permission", identity, time.Now())
		registry.flush()
		previous.WaitBackground()
	}
	// 服务停止期间：重新获取 replaced 的认证文件覆盖上传
	if err := aistudio.WriteStorageState(aistudio.StorageStatePath(replacedDirectory), authTestState(t, "NEW", false)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	cfg := config.Default()
	cfg.AuthStates = root
	cfg.WAABackend = config.WAABackendGo
	lifecycle, stop := context.WithCancel(ctx)
	useTempCatalogCacheDir(t)
	_, admin, closeRuntime, err := newRuntime(ctx, lifecycle, cfg, requests, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.pool.WaitBackground)
	defer func() {
		stop()
		if err := closeRuntime(); err != nil {
			t.Error(err)
		}
	}()
	registry := admin.service.permissionDenied
	if entries := registry.list(); len(entries) != 1 || entries[0].accountID != kept {
		t.Fatalf("启动后登记 = %+v，期望只剩认证文件没变的 %s", entries, kept)
	}
	if _, reason, cooling := modelCooldownUntil(t, admin.pool, replaced, testRuntimeModel); cooling {
		t.Fatalf("认证文件已更新的账户启动后应解除 403 冷却，仍有：%s", reason)
	}
	if _, cooldownReason, cooling := modelCooldownUntil(t, admin.pool, kept, testRuntimeModel); !cooling || cooldownReason != reason {
		t.Fatalf("认证文件没变的账户应保持 403 冷却：cooling=%v reason=%q", cooling, cooldownReason)
	}
	if !hasLogContaining(requests, "清除 403 无权限记录与冷却", "来源=启动时发现认证文件已更新") ||
		!hasLogContaining(requests, "已不在账户池中的账户", "账户=1") {
		t.Fatalf("应记录启动时的清除，日志 = %v", serviceLogMessages(requests))
	}
}

// deniedWatcherAdmin 返回带 403 无权限登记表的管理端；账户都已登记（记下当前登录签名指纹）并有单模型 403 冷却
func deniedWatcherAdmin(t *testing.T, listed []string, accountIDs ...string) (*runtimeAdmin, *permissionDeniedRegistry) {
	t.Helper()
	admin, _, _ := enableTestAdmin(t, append(slices.Clone(listed), accountIDs...)...)
	registry := newPermissionDeniedRegistry("", admin.requests)
	admin.service.permissionDenied = registry
	until := time.Now().Add(forbiddenModelRepeatCooldown)
	for _, accountID := range listed {
		if err := admin.pool.MarkCooldownIfGeneration(
			accountID, testRuntimeModel, admin.pool.ModelAccessGeneration(accountID), time.Now(), until,
			forbiddenModelReasonPrefix+"The caller does not have permission",
		); err != nil {
			t.Fatal(err)
		}
		admin.service.recordPermissionDenied(accountID, testRuntimeModel, aistudio.ChannelPlayground, "The caller does not have permission", time.Now())
	}
	return admin, registry
}

// newTestAccountWatcher 返回基准为空的账户目录扫描器（与生成服务刚启动时一样）
func newTestAccountWatcher(admin *runtimeAdmin) *accountWatcher {
	return &accountWatcher{
		admin: admin, failures: map[string]string{}, missing: map[string]int{},
		authFiles: map[string]authFileStat{}, loginPending: map[string]struct{}{},
	}
}

// settleAuthFile 把账户认证文件的修改时间改到 at，模拟文件已经写完、不再变化
func settleAuthFile(t *testing.T, pool *aistudio.AccountPool, accountID string, at time.Time) {
	t.Helper()
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(account.StoragePath, at, at); err != nil {
		t.Fatal(err)
	}
}

// scanAndWait 扫描一轮账户目录，并等后台载入登录状态更新结束
func scanAndWait(t *testing.T, watcher *accountWatcher) {
	t.Helper()
	watcher.scan(watcher.admin.lifecycle)
	waitFor(t, "后台载入认证文件更新结束", func() bool { return !watcher.refreshing.Load() })
}

// withCookie 返回加上（或替换）一个 Cookie 后的登录状态，登录签名 Cookie 不变
func withCookie(state aistudio.StorageState, name string, value string) aistudio.StorageState {
	cookies := make([]aistudio.StateCookie, 0, len(state.Cookies)+1)
	for _, cookie := range state.Cookies {
		if cookie.Name != name {
			cookies = append(cookies, cookie)
		}
	}
	state.Cookies = append(cookies, aistudio.StateCookie{
		Name: name, Value: value, Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None",
	})
	return state
}

// memoryDigest 返回账户内存中登录状态的摘要
func memoryDigest(t *testing.T, pool *aistudio.AccountPool, accountID string) string {
	t.Helper()
	state, ok := pool.StorageStateOf(accountID)
	if !ok {
		t.Fatalf("账户 %s 不存在", accountID)
	}
	return storageStateDigest(state)
}

// TestPermissionDeniedWatcherSameSessionReexport 在已登录的浏览器里同意政策后重新导出认证文件时登录签名 Cookie 不变：403 无权限账户的
// 认证文件内容与内存不同就按认证文件更新载入，移出列表并解除 403 冷却。本服务自己写回 Cookie（内存随之更新）不算更新；
// 不在列表里的账户仍只比较登录签名 Cookie，内容变化不重新载入
func TestPermissionDeniedWatcherSameSessionReexport(t *testing.T) {
	const listed, plain = "listed@example.com", "plain@example.com"
	admin, registry := deniedWatcherAdmin(t, []string{listed}, plain)
	pool := admin.pool
	base := time.Now().Add(-time.Minute)
	settleAuthFile(t, pool, listed, base)
	settleAuthFile(t, pool, plain, base)
	watcher := newTestAccountWatcher(admin)
	scanAndWait(t, watcher)
	if _, ok := registry.identityOf(listed); !ok {
		t.Fatal("认证文件没有变化时第一次扫描不应清除登记")
	}

	// 本服务自己写回 Cookie：磁盘与内存一起变化
	lease, err := pool.AcquireAccount(context.Background(), listed)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.MergeSetCookieHeaders(
		[]string{"__Secure-1PSIDTS=service-rotated; Domain=.google.com; Path=/; Secure"}, "https://aistudio.google.com/", time.Now(),
	); err != nil {
		t.Fatal(errors.Join(err, lease.Release()))
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	settleAuthFile(t, pool, listed, base.Add(10*time.Second))
	scanAndWait(t, watcher)
	if _, ok := registry.identityOf(listed); !ok {
		t.Fatal("本服务自己写回 Cookie 不应当作认证文件更新")
	}
	if _, _, cooling := modelCooldownUntil(t, pool, listed, testRuntimeModel); !cooling {
		t.Fatal("本服务自己写回 Cookie 后 403 冷却应保持")
	}

	// 本服务自己续签：登录签名 Cookie 换了，磁盘与内存一起变化。不算重新上传，只更新记下的登录签名，下次启动时也不会被当作替换了认证文件
	renewed := authTestState(t, "RENEWED", false)
	lease, err = pool.AcquireAccount(context.Background(), listed)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lease.SaveStorageState(renewed), lease.Release()); err != nil {
		t.Fatal(err)
	}
	settleAuthFile(t, pool, listed, base.Add(15*time.Second))
	scanAndWait(t, watcher)
	if identity, ok := registry.identityOf(listed); !ok || identity != aistudio.LoginIdentity(renewed) {
		t.Fatalf("本服务自己续签后应保留登记并更新登录签名：listed=%v identity=%q", ok, identity)
	}
	admin.service.reconcilePermissionDenied()
	if _, ok := registry.identityOf(listed); !ok {
		t.Fatal("本服务自己续签之后重启，不应当作服务停止期间替换了认证文件")
	}

	// 外部在同一登录会话里重新导出：登录签名 Cookie 不变，轮换 Cookie 与其他 Cookie 变了
	plainBefore := memoryDigest(t, pool, plain)
	for _, accountID := range []string{listed, plain} {
		account, err := pool.Account(accountID)
		if err != nil {
			t.Fatal(err)
		}
		state, err := aistudio.LoadStorageState(account.StoragePath)
		if err != nil {
			t.Fatal(err)
		}
		reexported := withCookie(withCookie(state, "__Secure-1PSIDTS", "browser-reexport"), "NID", "browser-reexport")
		if err := aistudio.WriteStorageState(account.StoragePath, reexported); err != nil {
			t.Fatal(err)
		}
		settleAuthFile(t, pool, accountID, base.Add(20*time.Second))
	}
	scanAndWait(t, watcher)
	if _, ok := registry.identityOf(listed); ok {
		t.Fatalf("同一会话重新导出的认证文件应清除登记：%+v", registry.list())
	}
	if _, reason, cooling := modelCooldownUntil(t, pool, listed, testRuntimeModel); cooling {
		t.Fatalf("同一会话重新导出的认证文件应解除 403 冷却，仍有：%s", reason)
	}
	account, err := pool.Account(listed)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if memoryDigest(t, pool, listed) != storageStateDigest(disk) {
		t.Fatal("应载入重新导出的认证文件")
	}
	if memoryDigest(t, pool, plain) != plainBefore {
		t.Fatal("不在列表里的账户只比较登录签名 Cookie，内容变化不应重新载入")
	}
}

// TestPermissionDeniedWatcherBaseline 账户目录扫描第一次看到的认证文件原先只记作基准：载入账户池之后、第一次扫描之前被替换的认证文件
// （例如重启生成服务时正好在等待文件稳定）再也发现不了。403 无权限账户第一次看到时就对照：磁盘与内存不同时载入新的认证文件；
// 磁盘与内存相同、只是登录签名与登记时记下的不同（本服务自己写回换掉了登录签名 Cookie）时不算重新上传，保留登记与冷却、
// 只更新记下的登录签名；都没变的保留
func TestPermissionDeniedWatcherBaseline(t *testing.T) {
	const replaced, relogged, kept = "replaced@example.com", "relogged@example.com", "kept@example.com"
	admin, registry := deniedWatcherAdmin(t, []string{replaced, relogged, kept})
	pool := admin.pool
	// relogged 登记时用的是之前的登录状态，之后本服务自己换掉了登录签名 Cookie（磁盘与内存一起变）
	registry.record(relogged, testRuntimeModel, aistudio.ChannelPlayground, "The caller does not have permission", "previous-identity", time.Now())
	account, err := pool.Account(replaced)
	if err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteStorageState(account.StoragePath, authTestState(t, "NEW", false)); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Minute)
	for _, accountID := range []string{replaced, relogged, kept} {
		settleAuthFile(t, pool, accountID, base)
	}
	scanAndWait(t, newTestAccountWatcher(admin))
	if _, ok := registry.identityOf(replaced); ok {
		t.Fatalf("%s 的认证文件已更新，第一次扫描应清除登记：%+v", replaced, registry.list())
	}
	if _, reason, cooling := modelCooldownUntil(t, pool, replaced, testRuntimeModel); cooling {
		t.Fatalf("%s 的认证文件已更新，应解除 403 冷却，仍有：%s", replaced, reason)
	}
	if identity, _ := pool.LoginIdentity(replaced); identity != aistudio.LoginIdentity(authTestState(t, "NEW", false)) {
		t.Fatal("应载入替换后的认证文件")
	}
	current, _ := pool.LoginIdentity(relogged)
	if identity, ok := registry.identityOf(relogged); !ok || identity != current {
		t.Fatalf("磁盘与内存相同时不算重新上传，应保留登记并更新登录签名：listed=%v identity=%q", ok, identity)
	}
	for _, accountID := range []string{relogged, kept} {
		if _, ok := registry.identityOf(accountID); !ok {
			t.Fatalf("%s 应保留登记", accountID)
		}
		if _, _, cooling := modelCooldownUntil(t, pool, accountID, testRuntimeModel); !cooling {
			t.Fatalf("%s 的 403 冷却应保持", accountID)
		}
	}
}

// TestPermissionDeniedRegistryLocking 每个成功的请求都要查一次登记表：没有这个账户这个模型的登记时只取读锁，别的请求持有读锁时
// （列出登记、复制快照）不必等待。写文件时编码整张表在锁外进行（登记表可能有上万个账户），编码期间的新登记不混进这份快照、
// 稍后另写一次；管理接口一次取得账户的显示名称，不在账户池里的账户显示 ID
func TestPermissionDeniedRegistryLocking(t *testing.T) {
	directory := t.TempDir()
	registry := newPermissionDeniedRegistry(directory, nil)
	registry.record("listed@example.com", "pro", aistudio.ChannelPlayground, "denied", "", time.Now())

	registry.mu.RLock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		registry.succeeded("other@example.com", "pro", aistudio.ChannelPlayground)
		registry.succeeded("listed@example.com", "flash", aistudio.ChannelPlayground)
		registry.succeeded("listed@example.com", "pro", aistudio.ChannelBuild)
	}()
	select {
	case <-done:
		registry.mu.RUnlock()
	case <-time.After(2 * time.Second):
		registry.mu.RUnlock()
		<-done
		t.Fatal("没有登记的成功请求不应等待其他读者")
	}

	registry.mu.Lock()
	registry.encode = func(file permissionDeniedFile) ([]byte, error) {
		if !registry.mu.TryLock() {
			t.Error("编码登记时不应持有登记表的锁")
		} else {
			registry.mu.Unlock()
			registry.record("late@example.com", "pro", aistudio.ChannelPlayground, "denied", "", time.Now())
		}
		return json.MarshalIndent(file, "", "  ")
	}
	registry.mu.Unlock()
	saved := func() []string {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(directory, permissionDeniedFileName))
		if err != nil {
			t.Fatal(err)
		}
		var stored permissionDeniedFile
		if err := json.Unmarshal(content, &stored); err != nil {
			t.Fatal(err)
		}
		accountIDs := make([]string, 0, len(stored.Accounts))
		for accountID := range stored.Accounts {
			accountIDs = append(accountIDs, accountID)
		}
		slices.Sort(accountIDs)
		return accountIDs
	}
	registry.flush()
	if got := saved(); !slices.Equal(got, []string{"listed@example.com"}) {
		t.Fatalf("写入的登记 = %v，编码期间的新登记不应混进这份快照", got)
	}
	registry.mu.Lock()
	registry.encode = nil
	registry.mu.Unlock()
	registry.flush()
	if got := saved(); !slices.Equal(got, []string{"late@example.com", "listed@example.com"}) {
		t.Fatalf("再次写入的登记 = %v，编码期间的新登记应稍后写入", got)
	}

	pool, _ := testAccountPool(t, "listed@example.com")
	lease, err := pool.AcquireAccount(context.Background(), "listed@example.com")
	if err != nil {
		t.Fatal(err)
	}
	accountConfig := lease.Account().Config
	accountConfig.Label = "Listed.Account@Example.com"
	if err := errors.Join(lease.SaveConfig(accountConfig), lease.Release()); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{pool: pool, service: &trackedService{permissionDenied: registry}}
	listedEntries, err := admin.PermissionDenied(context.Background())
	if err != nil || len(listedEntries) != 2 {
		t.Fatalf("管理接口返回 %+v err=%v", listedEntries, err)
	}
	labels := map[string]string{}
	for _, entry := range listedEntries {
		labels[entry.ID] = entry.Label
	}
	if labels["listed@example.com"] != "Listed.Account@Example.com" || labels["late@example.com"] != "late@example.com" {
		t.Fatalf("显示名称 = %v，期望账户池里的名称，不在账户池里的显示 ID", labels)
	}
}

// TestPermissionDeniedRenewalKeepsListing 本服务自己续签换掉了登录签名 Cookie，不是重新上传。原先启动对照保留登记之后、账户目录扫描
// 第一次看到认证文件之前（启动后几十秒内）续签的账户，第一次扫描时磁盘与内存相同、登录签名与登记时不同，按重新上传清除：移出列表、
// 解除 403 冷却、冷却档位从头计，永远升不到后两档；停止前十几秒内续签、扫描还没更新记下的登录签名时，下次启动的对照同样清除。
// 续签提交时更新记下的登录签名；扫描第一次看到磁盘与内存相同的文件也不再清除（续签已提交、还没更新登记时被扫描到的同样保留）
func TestPermissionDeniedRenewalKeepsListing(t *testing.T) {
	isolateDisableNotes(t)
	root := filepath.Join(t.TempDir(), "auth")
	const renewed, committed = "renewed@example.com", "committed@example.com"
	reason := forbiddenModelReasonPrefix + "The caller does not have permission"
	for _, accountID := range []string{renewed, committed} {
		directory := writeEnableTestAccount(t, root, accountID, true)
		// 带 Chrome 续签材料，登录签名 Cookie 与 writeEnableTestAccount 写入的相同
		if err := aistudio.WriteStorageState(aistudio.StorageStatePath(directory), authTestState(t, "V", true)); err != nil {
			t.Fatal(err)
		}
	}
	{
		// 停止之前的一代：两个账户都返回过 403，登记并写入单模型 403 冷却
		accounts, failures := aistudio.NewAccountStore(root).LoadTolerant()
		if len(failures) > 0 {
			t.Fatal(failures)
		}
		previous := aistudio.NewAccountPool(accounts, 1)
		registry := newPermissionDeniedRegistry(root, nil)
		until := time.Now().Add(forbiddenModelRepeatCooldown)
		for _, accountID := range []string{renewed, committed} {
			if err := previous.MarkCooldownIfGeneration(
				accountID, testRuntimeModel, previous.ModelAccessGeneration(accountID), time.Now(), until, reason,
			); err != nil {
				t.Fatal(err)
			}
			identity, _ := previous.LoginIdentity(accountID)
			registry.record(accountID, testRuntimeModel, aistudio.ChannelPlayground, "The caller does not have permission", identity, time.Now())
		}
		registry.flush()
		previous.WaitBackground()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	cfg := config.Default()
	cfg.AuthStates = root
	cfg.WAABackend = config.WAABackendGo
	lifecycle, stop := context.WithCancel(ctx)
	useTempCatalogCacheDir(t)
	_, admin, closeRuntime, err := newRuntime(ctx, lifecycle, cfg, requests, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.pool.WaitBackground)
	defer func() {
		stop()
		if err := closeRuntime(); err != nil {
			t.Error(err)
		}
	}()
	registry := admin.service.permissionDenied
	if entries := registry.list(); len(entries) != 2 {
		t.Fatalf("启动对照不应清除认证文件没变的账户：%+v", entries)
	}

	// renewed：请求遇到登录失效，经生成服务的续签器续签（与请求路径相同，只替换向 Google 续签的那一步）
	refresher := admin.workers.refresher
	refresher.refresh = func(context.Context, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error) {
		return authTestState(t, "RENEWED", false).Cookies, nil
	}
	lease, err := admin.pool.AcquireAccount(ctx, renewed)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(refresher.Refresh(aistudio.ContextWithAccountLease(ctx, lease)), lease.Release()); err != nil {
		t.Fatal(err)
	}
	renewedIdentity := aistudio.LoginIdentity(authTestState(t, "RENEWED", false))
	if identity, _ := admin.pool.LoginIdentity(renewed); identity != renewedIdentity {
		t.Fatal("续签应换掉登录签名 Cookie")
	}
	if identity, ok := registry.identityOf(renewed); !ok || identity != renewedIdentity {
		t.Fatalf("续签提交时应更新记下的登录签名：listed=%v identity=%q", ok, identity)
	}
	// 扫描看到这个文件之前停止、再次启动：对照不应当作停止期间替换了认证文件
	admin.service.reconcilePermissionDenied()
	if _, ok := registry.identityOf(renewed); !ok {
		t.Fatal("续签之后重启，启动对照不应清除登记")
	}

	// committed：续签已经提交、还没更新登记时被扫描看到（直接在租约内提交，不经续签器）
	lease, err = admin.pool.AcquireAccount(ctx, committed)
	if err != nil {
		t.Fatal(err)
	}
	err = lease.RefreshStorageState(func(state *aistudio.StorageState) error {
		state.Cookies = authTestState(t, "COMMITTED", false).Cookies
		return nil
	}, func() (func(bool), error) { return func(bool) {}, nil })
	if err := errors.Join(err, lease.Release()); err != nil {
		t.Fatal(err)
	}

	// 账户目录扫描第一次看到这两个认证文件
	base := time.Now().Add(-time.Minute)
	for _, accountID := range []string{renewed, committed} {
		settleAuthFile(t, admin.pool, accountID, base)
	}
	scanAndWait(t, newTestAccountWatcher(admin))
	for _, accountID := range []string{renewed, committed} {
		current, _ := admin.pool.LoginIdentity(accountID)
		if identity, ok := registry.identityOf(accountID); !ok || identity != current {
			t.Fatalf("%s 自己续签后第一次扫描应保留登记并记下新的登录签名：listed=%v identity=%q", accountID, ok, identity)
		}
		if _, cooldownReason, cooling := modelCooldownUntil(t, admin.pool, accountID, testRuntimeModel); !cooling || cooldownReason != reason {
			t.Fatalf("%s 自己续签后 403 冷却应保持：cooling=%v reason=%q", accountID, cooling, cooldownReason)
		}
	}
	if hasLogContaining(requests, "清除 403 无权限记录与冷却") {
		t.Fatalf("自己续签不应清除 403 无权限记录，日志 = %v", serviceLogMessages(requests))
	}
}

// TestPermissionDeniedSharedDirectoryWriteBack 多个进程共用一个账户目录时，另一个进程回写 Google 轮换的 Cookie（__Secure-1PSIDTS、SIDCC 等）
// 或 Worker 启动成功后写回 AI Studio 的 localStorage 后，磁盘与本进程内存不同。原先按重新上传处理：移出列表、解除 403 冷却、
// 冷却档位从头计，另一个进程每回写一次就这样一次。这两项不计入内容比较；外部重新导出改了其他内容时仍按重新上传清除
func TestPermissionDeniedSharedDirectoryWriteBack(t *testing.T) {
	const listed = "listed@example.com"
	admin, registry := deniedWatcherAdmin(t, []string{listed})
	pool := admin.pool
	account, err := pool.Account(listed)
	if err != nil {
		t.Fatal(err)
	}
	// 另一个进程：同一账户目录上的另一个账户池
	accounts, failures := aistudio.NewAccountStore(filepath.Dir(account.Directory)).LoadTolerant()
	if len(failures) > 0 {
		t.Fatal(failures)
	}
	other := aistudio.NewAccountPool(accounts, 1)
	t.Cleanup(other.WaitBackground)
	base := time.Now().Add(-time.Minute)
	settleAuthFile(t, pool, listed, base)
	watcher := newTestAccountWatcher(admin)
	scanAndWait(t, watcher)
	if _, ok := registry.identityOf(listed); !ok {
		t.Fatal("认证文件没有变化时第一次扫描不应清除登记")
	}

	lease, err := other.AcquireAccount(context.Background(), listed)
	if err != nil {
		t.Fatal(err)
	}
	rotated := make([]string, 0, 5)
	for _, name := range []string{"__Secure-1PSIDTS", "__Secure-3PSIDTS", "SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDCC"} {
		rotated = append(rotated, name+"=other-process; Domain=.google.com; Path=/; Secure")
	}
	if err := lease.MergeSetCookieHeaders(rotated, "https://aistudio.google.com/", time.Now()); err != nil {
		t.Fatal(errors.Join(err, lease.Release()))
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	disk, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if memory, _ := pool.StorageStateOf(listed); len(disk.Cookies) != len(memory.Cookies)+len(rotated) {
		t.Fatalf("另一个进程应把轮换 Cookie 写进认证文件：磁盘 %d 个，内存 %d 个", len(disk.Cookies), len(memory.Cookies))
	}
	settleAuthFile(t, pool, listed, base.Add(10*time.Second))
	scanAndWait(t, watcher)
	if _, ok := registry.identityOf(listed); !ok {
		t.Fatalf("另一个进程回写轮换 Cookie 不应当作重新上传：%+v", registry.list())
	}
	if _, _, cooling := modelCooldownUntil(t, pool, listed, testRuntimeModel); !cooling {
		t.Fatal("另一个进程回写轮换 Cookie 后 403 冷却应保持")
	}
	if hasLogContaining(admin.requests, "清除 403 无权限记录与冷却") {
		t.Fatalf("另一个进程回写轮换 Cookie 不应清除记录，日志 = %v", serviceLogMessages(admin.requests))
	}

	// 另一个进程的 Worker 启动成功后写回 AI Studio 的 localStorage：同样是本服务自己写回的内容，不算重新上传
	written, err := other.MergeBrowserLocalStorage(listed, other.AuthGeneration(listed), aistudio.StorageOrigin{
		Origin: aistudio.BrowserLocalStorageOrigin, LocalStorage: []aistudio.StorageItem{{Name: "aiStudioUserPreference", Value: "other-process"}},
	})
	if err != nil || !written {
		t.Fatalf("另一个进程应写回 localStorage: written=%t err=%v", written, err)
	}
	disk, err = aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if memory, _ := pool.StorageStateOf(listed); len(disk.Origins) == len(memory.Origins) {
		t.Fatalf("另一个进程应把 localStorage 写进认证文件：磁盘 %d 个 origin，内存 %d 个", len(disk.Origins), len(memory.Origins))
	}
	settleAuthFile(t, pool, listed, base.Add(15*time.Second))
	scanAndWait(t, watcher)
	if _, ok := registry.identityOf(listed); !ok {
		t.Fatalf("另一个进程写回 localStorage 不应当作重新上传：%+v", registry.list())
	}
	if _, _, cooling := modelCooldownUntil(t, pool, listed, testRuntimeModel); !cooling {
		t.Fatal("另一个进程写回 localStorage 后 403 冷却应保持")
	}
	if hasLogContaining(admin.requests, "清除 403 无权限记录与冷却") {
		t.Fatalf("另一个进程写回 localStorage 不应清除记录，日志 = %v", serviceLogMessages(admin.requests))
	}

	// 外部在同一登录会话里重新导出：登录签名 Cookie 不变，轮换 Cookie 之外的 Cookie 也变了
	if err := aistudio.WriteStorageState(
		account.StoragePath, withCookie(withCookie(disk, "__Secure-1PSIDTS", "browser-reexport"), "NID", "browser-reexport"),
	); err != nil {
		t.Fatal(err)
	}
	settleAuthFile(t, pool, listed, base.Add(20*time.Second))
	scanAndWait(t, watcher)
	if _, ok := registry.identityOf(listed); ok {
		t.Fatalf("重新导出的认证文件应清除登记：%+v", registry.list())
	}
	if _, reason, cooling := modelCooldownUntil(t, pool, listed, testRuntimeModel); cooling {
		t.Fatalf("重新导出的认证文件应解除 403 冷却，仍有：%s", reason)
	}
}

// TestStorageStateDigestIgnoresWrittenBack 摘要不计本服务自己写回的内容（轮换 Cookie、AI Studio 的 localStorage），
// 其他 origin 的 localStorage 与其他 Cookie 仍计入；磁盘与内存只差 AI Studio 的 localStorage 时 403 账户不按重新上传载入
func TestStorageStateDigestIgnoresWrittenBack(t *testing.T) {
	const listed = "listed@example.com"
	admin, _ := deniedWatcherAdmin(t, []string{listed})
	memory, ok := admin.pool.StorageStateOf(listed)
	if !ok {
		t.Fatal("账户不存在")
	}
	withOrigin := func(state aistudio.StorageState, origin string, value string) aistudio.StorageState {
		state.Origins = append(slices.Clone(state.Origins), aistudio.StorageOrigin{
			Origin: origin, LocalStorage: []aistudio.StorageItem{{Name: "k", Value: value}},
		})
		return state
	}
	written := withOrigin(withCookie(memory, "SIDCC", "rotated"), aistudio.BrowserLocalStorageOrigin, "upgraded")
	if storageStateDigest(written) != storageStateDigest(memory) {
		t.Fatal("轮换 Cookie 与 AI Studio 的 localStorage 不应计入摘要")
	}
	if storageStateDigest(withOrigin(memory, "https://accounts.google.com", "changed")) == storageStateDigest(memory) {
		t.Fatal("其他 origin 的 localStorage 应计入摘要")
	}
	if storageStateDigest(withCookie(memory, "NID", "changed")) == storageStateDigest(memory) {
		t.Fatal("轮换 Cookie 之外的 Cookie 应计入摘要")
	}
	account, err := admin.pool.Account(listed)
	if err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteStorageState(account.StoragePath, written); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	watcher := newTestAccountWatcher(admin)
	summary := aistudio.AccountLoginSummary{ID: listed, StoragePath: account.StoragePath}
	refresh, settled := watcher.deniedAuthUpdated(summary, written, authFileStat{modTime: info.ModTime(), size: info.Size()})
	if refresh || !settled {
		t.Fatalf("磁盘与内存只差 AI Studio 的 localStorage 时 deniedAuthUpdated = (%t, %t)，期望 (false, true)", refresh, settled)
	}
}
