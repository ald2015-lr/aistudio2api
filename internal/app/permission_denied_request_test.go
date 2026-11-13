package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// permissionDeniedAliasModel 为测试用的目录别名：不是目录 ID，请求它时生成服务换成 testRuntimeModel（与线上只作为 Omni 别名存在的名字相同）
const permissionDeniedAliasModel = "alias-only-chat"

// TestPermissionDeniedRecordsLastRequest 经公开 API 的请求被 403 时，登记里记下最近一次的请求来源：请求 ID、接口路径、客户端请求的模型名
// （目录别名，换成正式 ID 之前）与消息条数；管理接口照样返回。请求结果日志在请求名与最终模型不同时带 requested_model
func TestPermissionDeniedRecordsLastRequest(t *testing.T) {
	upstream := &forbiddenUpstream{denied: map[string]bool{forbiddenBad: true, forbiddenGood: true}}
	service, pool := runningGenerateService(t, upstream, context.Background(), time.Minute, forbiddenBad, forbiddenGood)
	registry := withPermissionDenied(t, service)
	catalog := []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
		CapabilityOptions: map[string][]string{"aliases": {permissionDeniedAliasModel}},
	}}
	for _, accountID := range []string{forbiddenBad, forbiddenGood} {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, catalog); err != nil {
			t.Fatal(err)
		}
	}
	service.replaceModelSnapshot(catalog)
	capture := &accessLogCapture{}
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: capture})
	// 内联图片数见 TestPermissionDeniedRequestOf（测试上游没有附件上传，带图的普通模型请求在发出前失败）
	body := `{"model":"` + permissionDeniedAliasModel + `","max_tokens":16,"messages":[` +
		`{"role":"user","content":"look"},{"role":"assistant","content":"ok"},{"role":"user","content":"again"}]}`
	recorder, entries := postFallbackTo(t, handler, capture, "/v1/chat/completions", body)
	if recorder.Code < http.StatusBadRequest {
		t.Fatalf("两个账户都 403，请求应失败：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(entries) != 1 {
		t.Fatalf("访问日志 = %+v，期望 1 条", entries)
	}
	requestID := entries[0].RequestID
	for _, accountID := range []string{forbiddenBad, forbiddenGood} {
		entry, ok := permissionDeniedEntryOf(registry, accountID)
		if !ok {
			t.Fatalf("%s 应登记为 403 无权限账户", accountID)
		}
		model, ok := permissionDeniedModelOf(entry, testRuntimeModel)
		if !ok || model.LastRequest == nil {
			t.Fatalf("%s 的登记应带最近一次请求：%+v", accountID, model)
		}
		want := permissionDeniedRequest{
			ID: requestID, Path: "/v1/chat/completions", RequestedModel: permissionDeniedAliasModel, Messages: 3,
		}
		if *model.LastRequest != want {
			t.Fatalf("%s 最近一次请求 = %+v，期望 %+v", accountID, *model.LastRequest, want)
		}
	}
	admin := &runtimeAdmin{pool: pool, service: service, requests: service.requests}
	listed, err := admin.PermissionDenied(context.Background())
	if err != nil || len(listed) != 2 {
		t.Fatalf("管理接口返回 %+v err=%v", listed, err)
	}
	last := listed[0].Models[0].LastRequest
	if last == nil || last.ID != requestID || last.Path != "/v1/chat/completions" || last.RequestedModel != permissionDeniedAliasModel ||
		last.Messages != 3 || last.Images != 0 || last.Files != 0 {
		t.Fatalf("管理接口的最近一次请求 = %+v", last)
	}

	// 请求结果日志：请求名与最终模型不同时带 requested_model（与 request.started 的 model 相同含义）
	if entries[0].Model != testRuntimeModel || entries[0].RequestedModel != permissionDeniedAliasModel {
		t.Fatalf("访问日志的模型 = %q、请求名 = %q", entries[0].Model, entries[0].RequestedModel)
	}
	data, _, _ := finishedRequestLog(entries[0])
	encoded, _ := json.Marshal(data)
	if data.RequestedModel != permissionDeniedAliasModel || !strings.Contains(string(encoded), `"requested_model":"`+permissionDeniedAliasModel+`"`) {
		t.Fatalf("request.finished 应带 requested_model：%s", encoded)
	}
}

// TestRequestLogRequestedModelOnlyWhenDifferent request.finished 只在请求名与最终模型不同时带 requested_model；request.started 不带
func TestRequestLogRequestedModelOnlyWhenDifferent(t *testing.T) {
	same, _, _ := finishedRequestLog(api.AccessLog{RequestID: "req-same", Model: "gemini-x", RequestedModel: "gemini-x", Status: 200})
	if same.RequestedModel != "" {
		t.Fatalf("请求名与最终模型相同时不带 requested_model：%+v", same)
	}
	empty, _, _ := finishedRequestLog(api.AccessLog{RequestID: "req-empty", Model: "gemini-x", Status: 200})
	if encoded, _ := json.Marshal(empty); strings.Contains(string(encoded), "requested_model") {
		t.Fatalf("没有请求名时不输出 requested_model：%s", encoded)
	}
	fallback, _, _ := finishedRequestLog(api.AccessLog{RequestID: "req-fallback", Model: "gemini-y", RequestedModel: "gemini-x", FallbackFrom: "gemini-x", Status: 200})
	if fallback.RequestedModel != "gemini-x" || fallback.FallbackFrom != "gemini-x" {
		t.Fatalf("改用其他模型时 requested_model 为客户端请求的名字：%+v", fallback)
	}
}

// TestPermissionDeniedRequestOf 请求来源按生成请求计：消息条数、image/* 内联数据数与文件引用数；没有请求日志（不经公开 API）时
// 没有接口路径与请求名；请求名与登记的模型相同时省略
func TestPermissionDeniedRequestOf(t *testing.T) {
	request := aistudio.GenerateRequest{ID: "req-shape", Model: "models/gemini-x", Contents: []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{Text: "hi"}, {InlineData: &aistudio.Blob{MIME: "image/png", Data: []byte("png")}},
			{InlineData: &aistudio.Blob{MIME: "IMAGE/JPEG", Data: []byte("jpg")}}, {InlineData: &aistudio.Blob{MIME: "audio/wav", Data: []byte("wav")}},
			{File: &aistudio.FileRef{}},
		}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "ok"}}},
	}}
	got := permissionDeniedRequestOf(context.Background(), request, "gemini-x")
	want := permissionDeniedRequest{ID: "req-shape", Messages: 2, Images: 2, Files: 1}
	if got == nil || *got != want {
		t.Fatalf("请求来源 = %+v，期望 %+v", got, want)
	}
}

// TestPermissionDeniedLastRequestPersistence 最近一次请求随登记一起保存、重新打开后仍在；旧版本保存的文件（没有 last_request）照常读入；
// 之后一次拿不到来源的登记清空之前记下的，不留下更早那次的来源；文件里没有请求来源时不写 last_request
func TestPermissionDeniedLastRequestPersistence(t *testing.T) {
	directory := t.TempDir()
	at := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	first := newPermissionDeniedRegistry(directory, nil)
	last := &permissionDeniedRequest{ID: "req-1", Path: "/v1beta/interactions", RequestedModel: "veo-alias", Messages: 1, Files: 2}
	first.recordRequest("a@example.com", "gemini-omni-1.1-flash", aistudio.ChannelPlayground, "The caller does not have permission", "", at, last)
	first.record("b@example.com", "pro", aistudio.ChannelPlayground, "The caller does not have permission", "", at)
	first.flush()
	data, err := os.ReadFile(filepath.Join(directory, permissionDeniedFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"last_request"`) || strings.Count(string(data), `"last_request"`) != 1 {
		t.Fatalf("只有带来源的条目写 last_request：%s", data)
	}
	second := newPermissionDeniedRegistry(directory, nil)
	entry, _ := permissionDeniedEntryOf(second, "a@example.com")
	model, _ := permissionDeniedModelOf(entry, "gemini-omni-1.1-flash")
	if model.LastRequest == nil || *model.LastRequest != *last {
		t.Fatalf("重新打开后的最近一次请求 = %+v，期望 %+v", model.LastRequest, last)
	}
	second.record("a@example.com", "gemini-omni-1.1-flash", aistudio.ChannelPlayground, "The caller does not have permission", "", at.Add(time.Hour))
	entry, _ = permissionDeniedEntryOf(second, "a@example.com")
	if model, _ := permissionDeniedModelOf(entry, "gemini-omni-1.1-flash"); model.LastRequest != nil || model.Count != 2 {
		t.Fatalf("拿不到来源的登记应清空之前的来源：%+v", model)
	}

	legacy := t.TempDir()
	old := `{"accounts":{"c@example.com":{"identity":"id-c","models":{"pro":{"channels":["playground"],` +
		`"first_at":"2026-10-01T00:00:00Z","last_at":"2026-10-02T00:00:00Z","count":4,"message":"The caller does not have permission"}}}}}`
	if err := os.WriteFile(filepath.Join(legacy, permissionDeniedFileName), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := newPermissionDeniedRegistry(legacy, nil)
	entry, ok := permissionDeniedEntryOf(loaded, "c@example.com")
	model, _ = permissionDeniedModelOf(entry, "pro")
	if !ok || model.Count != 4 || model.LastRequest != nil || !model.LastAt.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("旧格式文件读入 = %+v ok=%v", model, ok)
	}
}
