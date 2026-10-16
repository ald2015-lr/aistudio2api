package app

import (
	"context"
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
	registry.record(ids[0], testRuntimeModel, aistudio.ChannelPlayground, "earlier", earlier)
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
	first.record("a@example.com", "pro", aistudio.ChannelBuild, "The caller does not have permission", at)
	first.record("a@example.com", "pro", aistudio.ChannelPlayground, "The caller does not have permission", at.Add(time.Minute))
	first.record("b@example.com", "flash", aistudio.ChannelPlayground, strings.Repeat("x", permissionDeniedMessageLimit+50), at.Add(2*time.Minute))
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
		registry.record(accountID, "pro", aistudio.ChannelPlayground, "denied", now)
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
