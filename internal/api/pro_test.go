package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// proTestHandler 建立带 PRO_POOL 与 ULTRA_EXCLUSIVE 读取函数的公开 API
func proTestHandler(service aistudio.Service, admin AdminService, exclusive *atomic.Bool, proPool *atomic.Bool, traceDir string) http.Handler {
	return NewHandler(service, Config{APIKey: "sk-test", Admin: admin, UltraExclusive: exclusive.Load, ProPool: proPool.Load, TraceDir: traceDir})
}

func proFlags(exclusive bool, proPool bool) (*atomic.Bool, *atomic.Bool) {
	exclusiveFlag, proFlag := &atomic.Bool{}, &atomic.Bool{}
	exclusiveFlag.Store(exclusive)
	proFlag.Store(proPool)
	return exclusiveFlag, proFlag
}

// TestProRouting /pro/v1 与 /pro/v1beta 走与主路由相同的处理链并标记 Pro 号池（与 /ultra 的挂载方式相同）；/trace/pro 为 Pro 号池的排查路由；
// 其余 /pro/* 路径（含 /pro/trace）返回 404
func TestProRouting(t *testing.T) {
	service := &poolRecordingService{}
	exclusive, proPool := proFlags(true, true)
	handler := proTestHandler(service, nil, exclusive, proPool, t.TempDir())
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/pro/v1/models"},
		{method: http.MethodGet, path: "/pro/v1beta/models"},
		{method: http.MethodGet, path: "/pro/v1/models/pro-model"},
		{method: http.MethodPost, path: "/pro/v1/chat/completions", body: `{"model":"pro-model","messages":[{"role":"user","content":"hi"}]}`},
		{method: http.MethodPost, path: "/pro/v1/messages", body: `{"model":"pro-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
		{method: http.MethodPost, path: "/pro/v1/messages/count_tokens", body: `{"model":"pro-model","messages":[{"role":"user","content":"hi"}]}`},
		{method: http.MethodPost, path: "/pro/v1beta/models/pro-model:generateContent", body: `{"contents":[{"parts":[{"text":"hi"}]}]}`},
		{method: http.MethodGet, path: "/trace/pro/v1/models"},
		{method: http.MethodPost, path: "/trace/pro/v1/chat/completions", body: `{"model":"pro-model","messages":[{"role":"user","content":"hi"}]}`},
	} {
		recorder := serve(handler, authorizedRequest(test.method, test.path, test.body))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s %s 状态码 = %d，期望 200: %s", test.method, test.path, recorder.Code, recorder.Body.String())
		}
		if got := service.lastScope(t); got != aistudio.PoolScopePro {
			t.Fatalf("%s %s 的号池 = %q，期望 pro", test.method, test.path, got.String())
		}
	}
	for _, path := range []string{"/pro/", "/pro/api/status", "/pro/trace/v1/models", "/trace/pro/", "/trace/pro/api/status", "/pro/health", "/pro/v2/models", "/pro/ultra/v1/models", "/ultra/pro/v1/models"} {
		if recorder := serve(handler, authorizedRequest(http.MethodGet, path, "")); recorder.Code != http.StatusNotFound {
			t.Fatalf("%s 状态码 = %d，期望 404", path, recorder.Code)
		}
	}
	// 未知接口与 /ultra 一样按协议返回 404
	if recorder := serve(handler, authorizedRequest(http.MethodGet, "/pro/v1/unknown", "")); recorder.Code != http.StatusNotFound ||
		!strings.Contains(recorder.Body.String(), "Invalid URL (GET /v1/unknown)") {
		t.Fatalf("/pro 未知接口：%d %s", recorder.Code, recorder.Body.String())
	}
}

// TestDefaultPathScopeWithProPool 普通路径的号池：独占时为普通号池（与 PRO_POOL 无关）；不独占时 PRO_POOL=true 为普通号池与 Ultra 号池
// （不用 Pro 账户），PRO_POOL=false 为不限号池（与没有这项设置之前相同）。开关每个请求读取一次
func TestDefaultPathScopeWithProPool(t *testing.T) {
	service := &poolRecordingService{}
	exclusive, proPool := proFlags(true, true)
	handler := proTestHandler(service, nil, exclusive, proPool, t.TempDir())
	for _, test := range []struct {
		exclusive, proPool bool
		want               aistudio.PoolScope
	}{
		{exclusive: true, proPool: true, want: aistudio.PoolScopeNormal},
		{exclusive: true, proPool: false, want: aistudio.PoolScopeNormal},
		{exclusive: false, proPool: true, want: aistudio.PoolScopeNormalUltra},
		{exclusive: false, proPool: false, want: aistudio.PoolScopeAll},
	} {
		exclusive.Store(test.exclusive)
		proPool.Store(test.proPool)
		for _, path := range []string{"/v1/models", "/v1beta/models", "/trace/v1/models"} {
			if recorder := serve(handler, authorizedRequest(http.MethodGet, path, "")); recorder.Code != http.StatusOK {
				t.Fatalf("%s: %d", path, recorder.Code)
			}
			if got := service.lastScope(t); got != test.want {
				t.Fatalf("%s（独占=%t，PRO_POOL=%t）的号池 = %q，期望 %q", path, test.exclusive, test.proPool, got.String(), test.want.String())
			}
		}
		// /ultra 不受这两个开关影响
		serve(handler, authorizedRequest(http.MethodGet, "/ultra/v1/models", ""))
		if got := service.lastScope(t); got != aistudio.PoolScopeUltra {
			t.Fatalf("/ultra（独占=%t，PRO_POOL=%t）的号池 = %q", test.exclusive, test.proPool, got.String())
		}
	}
	// 没有配置读取函数时按默认开启
	if !(Config{}).proPool() {
		t.Fatal("Config.ProPool 为 nil 时应按开启")
	}
}

// TestProRoutingKeepsHandlerChain /pro 与主路由共用密钥校验、来源检查、CORS 与各协议的错误格式
func TestProRoutingKeepsHandlerChain(t *testing.T) {
	exclusive, proPool := proFlags(true, true)
	handler := proTestHandler(&poolRecordingService{}, nil, exclusive, proPool, t.TempDir())
	for _, test := range []struct {
		method, path, want string
	}{
		{method: http.MethodGet, path: "/pro/v1/models", want: "invalid_api_key"},
		{method: http.MethodPost, path: "/pro/v1/messages", want: "authentication_error"},
		{method: http.MethodPost, path: "/pro/v1beta/models/pro-model:generateContent", want: "UNAUTHENTICATED"},
	} {
		recorder := serve(handler, httptest.NewRequest(test.method, test.path, strings.NewReader("{}")))
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), test.want) {
			t.Fatalf("%s 缺少密钥：%d %s，期望含 %s 的 401", test.path, recorder.Code, recorder.Body.String(), test.want)
		}
	}
	options := httptest.NewRequest(http.MethodOptions, "/pro/v1/chat/completions", nil)
	if recorder := serve(handler, options); recorder.Code != http.StatusNoContent || recorder.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS 预检：%d %v", recorder.Code, recorder.Header())
	}
	defaultKey := NewHandler(&poolRecordingService{}, Config{})
	browser := httptest.NewRequest(http.MethodGet, "/pro/v1/models", nil)
	browser.Header.Set("Authorization", "Bearer sk-onechat-fun-fun")
	browser.Header.Set("Origin", "https://evil.example.com")
	if recorder := serve(defaultKey, browser); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("外部网页来源：%d，期望 401", recorder.Code)
	}
}

// openAINotFound 判断 OpenAI 错误体与未知路由的 404 相同（type 为 invalid_request_error，code 为 null，见 writeRouteNotFound）
func openAINotFound(body proRouteErrorBody) bool {
	return body.Error.Type == "invalid_request_error" && body.Error.Code == nil
}

type proRouteErrorBody struct {
	Error struct {
		Code    any    `json:"code"`
		Type    string `json:"type"`
		Status  string `json:"status"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestProRouteDisabled PRO_POOL=false 时 /pro 与 /trace/pro 按协议返回 404，处理函数不被调用；404 在密钥校验、CORS 与请求日志之后：
// 没有密钥照常 401，响应带 CORS 头，请求日志路径为 /pro/...、号池为 pro。重新开启后立即恢复
func TestProRouteDisabled(t *testing.T) {
	service := &poolRecordingService{}
	admin := &capturingAdmin{}
	dir := t.TempDir()
	exclusive, proPool := proFlags(true, false)
	handler := proTestHandler(service, admin, exclusive, proPool, dir)
	for _, test := range []struct {
		method, path, body string
		check              func(proRouteErrorBody) bool
		message            string
	}{
		{
			method: http.MethodPost, path: "/pro/v1/chat/completions", body: `{"model":"pro-model","messages":[{"role":"user","content":"hi"}]}`,
			check:   openAINotFound,
			message: "Invalid URL (POST /pro/v1/chat/completions): the /pro endpoints are disabled on this server.",
		},
		{
			method: http.MethodPost, path: "/pro/v1/messages", body: `{"model":"pro-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
			check:   func(body proRouteErrorBody) bool { return body.Error.Type == "not_found_error" },
			message: "Invalid URL (POST /pro/v1/messages): the /pro endpoints are disabled on this server.",
		},
		{
			method: http.MethodGet, path: "/pro/v1beta/models",
			check:   func(body proRouteErrorBody) bool { return body.Error.Status == "NOT_FOUND" },
			message: "Invalid URL (GET /pro/v1beta/models): the /pro endpoints are disabled on this server.",
		},
		{
			method: http.MethodGet, path: "/pro/v1/models",
			check:   openAINotFound,
			message: "Invalid URL (GET /pro/v1/models): the /pro endpoints are disabled on this server.",
		},
		{
			method: http.MethodPost, path: "/trace/pro/v1/chat/completions", body: `{"model":"pro-model","messages":[{"role":"user","content":"hi"}]}`,
			check:   openAINotFound,
			message: "Invalid URL (POST /trace/pro/v1/chat/completions): the /pro endpoints are disabled on this server.",
		},
	} {
		recorder := serve(handler, authorizedRequest(test.method, test.path, test.body))
		var body proRouteErrorBody
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 错误体不是 JSON: %s", test.path, recorder.Body.String())
		}
		if recorder.Code != http.StatusNotFound || !test.check(body) || body.Error.Message != test.message {
			t.Fatalf("%s %s：%d %s，期望按协议的 404 与 %q", test.method, test.path, recorder.Code, recorder.Body.String(), test.message)
		}
		if recorder.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s 的 404 缺少 CORS 头：%v", test.path, recorder.Header())
		}
	}
	service.mu.Lock()
	called := len(service.scopes)
	service.mu.Unlock()
	if called != 0 {
		t.Fatalf("PRO_POOL=false 时处理函数不应被调用，实际 %d 次", called)
	}
	if files, err := os.ReadDir(dir); err != nil || len(files) != 0 {
		t.Fatalf("关闭时 /trace/pro 不应写排查记录：%v err=%v", files, err)
	}
	entries := admin.finished()
	if len(entries) != 5 {
		t.Fatalf("请求日志条数 = %d，期望 5", len(entries))
	}
	if entry := entries[0]; entry.Path != "/pro/v1/chat/completions" || entry.Pool != "pro" || entry.Status != http.StatusNotFound || !entry.Authorized {
		t.Fatalf("关闭时 /pro 的请求日志：path=%q pool=%q status=%d authorized=%t", entry.Path, entry.Pool, entry.Status, entry.Authorized)
	}
	if entry := entries[4]; entry.Path != "/trace/pro/v1/chat/completions" || entry.Pool != "pro" {
		t.Fatalf("关闭时 /trace/pro 的请求日志：path=%q pool=%q", entry.Path, entry.Pool)
	}
	// 没有密钥：密钥校验在 404 之前，照常 401
	if recorder := serve(handler, httptest.NewRequest(http.MethodGet, "/pro/v1/models", nil)); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("关闭时没有密钥：%d %s，期望 401", recorder.Code, recorder.Body.String())
	}
	// CORS 预检照常
	if recorder := serve(handler, httptest.NewRequest(http.MethodOptions, "/pro/v1/chat/completions", nil)); recorder.Code != http.StatusNoContent {
		t.Fatalf("关闭时 CORS 预检：%d", recorder.Code)
	}
	// 普通路径与 /ultra 不受影响
	for _, path := range []string{"/v1/models", "/ultra/v1/models"} {
		if recorder := serve(handler, authorizedRequest(http.MethodGet, path, "")); recorder.Code != http.StatusOK {
			t.Fatalf("关闭时 %s：%d", path, recorder.Code)
		}
	}
	// 重新开启后 /pro 立即恢复
	proPool.Store(true)
	if recorder := serve(handler, authorizedRequest(http.MethodGet, "/pro/v1/models", "")); recorder.Code != http.StatusOK ||
		!strings.Contains(recorder.Body.String(), "pro-model") {
		t.Fatalf("重新开启后 /pro/v1/models：%d %s", recorder.Code, recorder.Body.String())
	}
}

// TestProModelsAndAliasCachePerPool /pro 的模型列表与别名解析只看 Pro 号池的目录，别名缓存与不限号池、Ultra 号池分开
func TestProModelsAndAliasCachePerPool(t *testing.T) {
	service := &poolRecordingService{}
	exclusive, proPool := proFlags(false, true)
	handler := proTestHandler(service, nil, exclusive, proPool, t.TempDir())
	listed := func(request *http.Request) string {
		t.Helper()
		recorder := serve(handler, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d", request.URL.Path, recorder.Code)
		}
		return recorder.Body.String()
	}
	if body := listed(authorizedRequest(http.MethodGet, "/pro/v1/models", "")); !strings.Contains(body, "pro-model") ||
		strings.Contains(body, "normal-model") || strings.Contains(body, "ultra-model") {
		t.Fatalf("/pro/v1/models 应只列 Pro 号池的模型: %s", body)
	}
	if body := listed(authorizedRequest(http.MethodGet, "/pro/v1beta/models", "")); !strings.Contains(body, "models/pro-model") {
		t.Fatalf("/pro/v1beta/models 应列 Pro 号池的模型: %s", body)
	}
	anthropic := authorizedRequest(http.MethodGet, "/pro/v1/models", "")
	anthropic.Header.Set("Anthropic-Version", "2023-06-01")
	if body := listed(anthropic); !strings.Contains(body, "pro-model") {
		t.Fatalf("Anthropic 模型列表应列 Pro 号池的模型: %s", body)
	}
	chat := func(path string, model string) string {
		t.Helper()
		recorder := serve(handler, authorizedRequest(http.MethodPost, path, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", path, model, recorder.Code, recorder.Body.String())
		}
		service.mu.Lock()
		defer service.mu.Unlock()
		return service.generated[len(service.generated)-1]
	}
	// 普通路径（不独占、PRO_POOL=true 时为普通号池与 Ultra 号池）先建立自己的快照，/pro 的别名不能用它解析
	if got := chat("/v1/chat/completions", "normal-model-nothinking"); got != "normal-model" {
		t.Fatalf("普通路径别名解析为 %q", got)
	}
	if got := chat("/pro/v1/chat/completions", "pro-model-nothinking"); got != "pro-model" {
		t.Fatalf("/pro 别名解析为 %q，期望 pro-model", got)
	}
	if got := chat("/pro/v1/chat/completions", "normal-model-nothinking"); got != "normal-model-nothinking" {
		t.Fatalf("/pro 不应解析其他号池的别名，实际 %q", got)
	}
	if got := chat("/v1/chat/completions", "pro-model-nothinking"); got != "pro-model-nothinking" {
		t.Fatalf("普通路径不应解析 Pro 号池的别名，实际 %q", got)
	}
	before := service.modelCall.Load()
	chat("/pro/v1/chat/completions", "pro-model-nothinking")
	if service.modelCall.Load() != before {
		t.Fatal("/pro 的别名目录应按号池缓存")
	}
	// 每个号池都有自己的缓存格；不认识的取值不借用不限号池的缓存
	s := &server{}
	if len(s.aliasCaches) != int(aistudio.PoolScopeCount) {
		t.Fatalf("别名缓存格数 = %d，期望 %d", len(s.aliasCaches), aistudio.PoolScopeCount)
	}
	for scope := aistudio.PoolScope(0); scope < aistudio.PoolScopeCount; scope++ {
		cache, got := s.aliasCache(aistudio.ContextWithPoolScope(t.Context(), scope))
		if cache != &s.aliasCaches[scope] || got != scope {
			t.Fatalf("号池 %d 的别名缓存不是自己的格", scope)
		}
	}
	unknown, got := s.aliasCache(aistudio.ContextWithPoolScope(t.Context(), aistudio.PoolScopeCount+3))
	if unknown == &s.aliasCaches[aistudio.PoolScopeAll] || got != aistudio.PoolScopeCount+3 {
		t.Fatal("不认识的号池不应落到不限号池的别名缓存")
	}
}

// TestProAccessLogErrorsAndVideoURI /pro 的请求日志带 /pro 路径与 pool=pro；Pro 号池没有可用账户按 503 并说明 Pro 号池（各协议）；
// 资源属于其他号池时按 400 返回改用入口的英文说明；Gemini 视频任务的下载地址保留 /pro 前缀
func TestProAccessLogErrorsAndVideoURI(t *testing.T) {
	admin := &capturingAdmin{}
	service := &poolRecordingService{}
	exclusive, proPool := proFlags(true, true)
	handler := proTestHandler(service, admin, exclusive, proPool, t.TempDir())
	serve(handler, authorizedRequest(http.MethodPost, "/pro/v1/chat/completions", `{"model":"pro-model","messages":[{"role":"user","content":"hi"}]}`))
	serve(handler, authorizedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"normal-model","messages":[{"role":"user","content":"hi"}]}`))
	entries := admin.finished()
	if len(entries) != 2 || entries[0].Path != "/pro/v1/chat/completions" || entries[0].Pool != UsagePoolPro || !entries[0].Authorized {
		t.Fatalf("Pro 请求日志: %+v", entries)
	}
	if entries[1].Path != "/v1/chat/completions" || entries[1].Pool != "" {
		t.Fatalf("普通请求日志: path=%q pool=%q", entries[1].Path, entries[1].Pool)
	}

	for _, reason := range []string{"没有权益为 Pro 的账户", "PRO_POOL=false，Pro 号池未启用"} {
		service.genErr = &aistudio.AccountsNotReadyError{Reasons: []string{reason}, Pool: aistudio.PoolScopePro}
		recorder := serve(handler, authorizedRequest(http.MethodPost, "/pro/v1/chat/completions", `{"model":"pro-model","messages":[{"role":"user","content":"hi"}]}`))
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), officialProUnavailableMessage) ||
			strings.Contains(recorder.Body.String(), "PRO_POOL") {
			t.Fatalf("Pro 号池没有账户（%s）：%d %s，期望说明 Pro 号池的 503", reason, recorder.Code, recorder.Body.String())
		}
	}
	recorder := serve(handler, authorizedRequest(http.MethodPost, "/pro/v1/messages", `{"model":"pro-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	var anthropic struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &anthropic)
	if recorder.Code != http.StatusServiceUnavailable || anthropic.Error.Message != officialProUnavailableMessage {
		t.Fatalf("Anthropic Pro 号池没有账户：%d %s", recorder.Code, recorder.Body.String())
	}
	// Ultra 号池的文案不变
	if public := publicErrorFor(&aistudio.AccountsNotReadyError{Pool: aistudio.PoolScopeUltra}, ""); public.Message != officialUltraUnavailableMessage {
		t.Fatalf("Ultra 号池的 503 文案 = %q", public.Message)
	}
	service.genErr = nil

	for _, test := range []struct {
		err  error
		want string
	}{
		{err: &aistudio.ResourcePoolMismatchError{ResourceID: "files/abc", Owner: aistudio.PoolScopeNormal, Request: aistudio.PoolScopePro},
			want: "resource files/abc belongs to an account of the normal pool; request it without the /pro prefix"},
		{err: &aistudio.ResourcePoolMismatchError{ResourceID: "files/abc", Owner: aistudio.PoolScopeUltra, Request: aistudio.PoolScopePro},
			want: "resource files/abc belongs to an account of the Ultra pool; request it through the /ultra endpoints"},
	} {
		service.fileErr = test.err
		recorder := serve(handler, authorizedRequest(http.MethodGet, "/pro/v1/files/files-abc", ""))
		body, _ := io.ReadAll(recorder.Body)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(string(body), test.want) {
			t.Fatalf("跨号池文件：%d %s，期望含 %q 的 400", recorder.Code, body, test.want)
		}
		if service.lastScope(t) != aistudio.PoolScopePro {
			t.Fatal("/pro 文件接口应标记 Pro 号池")
		}
	}
	service.fileErr = &aistudio.ResourcePoolMismatchError{ResourceID: "files/abc", Owner: aistudio.PoolScopePro, Request: aistudio.PoolScopeNormal}
	recorder = serve(handler, authorizedRequest(http.MethodGet, "/v1/files/files-abc", ""))
	if body := recorder.Body.String(); recorder.Code != http.StatusBadRequest ||
		!strings.Contains(body, "belongs to an account of the Pro pool; request it through the /pro endpoints") {
		t.Fatalf("普通路径引用 Pro 账户的文件：%d %s", recorder.Code, body)
	}

	video := &proVideoService{}
	videoHandler := proTestHandler(video, nil, exclusive, proPool, t.TempDir())
	recorder = serve(videoHandler, authorizedRequest(http.MethodGet, "/pro/v1beta/operations/op1", ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("查询视频任务：%d %s", recorder.Code, recorder.Body.String())
	}
	var operation struct {
		Response struct {
			GenerateVideoResponse struct {
				GeneratedSamples []struct {
					Video struct {
						URI string `json:"uri"`
					} `json:"video"`
				} `json:"generatedSamples"`
			} `json:"generateVideoResponse"`
		} `json:"response"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &operation); err != nil || len(operation.Response.GenerateVideoResponse.GeneratedSamples) != 1 {
		t.Fatalf("视频任务结果：%s err=%v", recorder.Body.String(), err)
	}
	parsed, err := url.Parse(operation.Response.GenerateVideoResponse.GeneratedSamples[0].Video.URI)
	if err != nil || parsed.Path != "/pro/v1/videos/op1/content" {
		t.Fatalf("/pro 下返回的视频地址 = %q，期望带 /pro 前缀", operation.Response.GenerateVideoResponse.GeneratedSamples[0].Video.URI)
	}
	follow := serve(videoHandler, authorizedRequest(http.MethodGet, parsed.Path, ""))
	if follow.Code != http.StatusOK || follow.Body.String() != "video" || video.lastScope(t) != aistudio.PoolScopePro {
		t.Fatalf("按返回地址下载：%d %s", follow.Code, follow.Body.String())
	}
}

// proVideoService 的视频任务属于 Pro 账户：非 Pro 号池的请求按资源号池不符拒绝
type proVideoService struct {
	poolRecordingService
}

func (service *proVideoService) GenerateVideo(ctx context.Context, _ aistudio.VideoRequest) (aistudio.VideoOperation, error) {
	service.record(ctx)
	return aistudio.VideoOperation{ID: "op1"}, nil
}

func (service *proVideoService) GetGenerateVideoOperation(ctx context.Context, operationID string) (aistudio.VideoOperation, error) {
	service.record(ctx)
	if scope := aistudio.PoolScopeFromContext(ctx); scope != aistudio.PoolScopePro {
		return aistudio.VideoOperation{}, &aistudio.ResourcePoolMismatchError{ResourceID: operationID, Owner: aistudio.PoolScopePro, Request: scope}
	}
	return aistudio.VideoOperation{ID: operationID, Done: true, File: &aistudio.FileRef{Name: "files/video"}}, nil
}

func (service *proVideoService) DownloadFile(ctx context.Context, _ string) (aistudio.MediaStream, error) {
	service.record(ctx)
	if scope := aistudio.PoolScopeFromContext(ctx); scope != aistudio.PoolScopePro {
		return aistudio.MediaStream{}, &aistudio.ResourcePoolMismatchError{ResourceID: "op1", Owner: aistudio.PoolScopePro, Request: scope}
	}
	return aistudio.MediaStream{Body: io.NopCloser(strings.NewReader("video")), MIME: "video/mp4", Size: 5}, nil
}

// TestTraceProRoute /trace/pro 的 POST 请求走 Pro 号池并写排查记录，管理日志路径为 /trace/pro/...、号池为 pro
func TestTraceProRoute(t *testing.T) {
	admin := &capturingAdmin{}
	service := &poolRecordingService{}
	dir := t.TempDir()
	exclusive, proPool := proFlags(true, true)
	handler := proTestHandler(service, admin, exclusive, proPool, dir)
	recorder := serve(handler, authorizedRequest(http.MethodPost, "/trace/pro/v1/chat/completions", `{"model":"pro-model","messages":[{"role":"user","content":"hi"}]}`))
	if recorder.Code != http.StatusOK || service.lastScope(t) != aistudio.PoolScopePro {
		t.Fatalf("状态码 %d: %s", recorder.Code, recorder.Body.String())
	}
	entries := admin.finished()
	if len(entries) != 1 || entries[0].Path != "/trace/pro/v1/chat/completions" || entries[0].Pool != "pro" {
		t.Fatalf("请求日志 %+v", entries)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("排查记录 %v err=%v", files, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil || !strings.Contains(string(raw), `"/trace/pro/v1/chat/completions"`) {
		t.Fatalf("排查记录没有带 /trace/pro 的路径: %s err=%v", raw, err)
	}
}

// TestOtherPoolHintInPublicError 普通路径的模型不存在错误带改用入口时（aistudio.ModelInOtherPoolError），对外 404 说明末尾追加英文提示，
// 错误代码仍是 model_not_found；模型在列（503）与其他错误不加
func TestOtherPoolHintInPublicError(t *testing.T) {
	hint := "This model is available through the /pro or /ultra endpoints."
	pools := []aistudio.PoolScope{aistudio.PoolScopePro, aistudio.PoolScopeUltra}
	notFound := &aistudio.ModelInOtherPoolError{Err: fmt.Errorf("%w: gemini-4-argon", aistudio.ErrModelNotFound), Model: "gemini-4-argon", Pools: pools}
	public := publicErrorFor(notFound, "gemini-4-argon")
	if public.Status != http.StatusNotFound || public.Kind != publicKindModelNotFound ||
		public.Message != modelNotFoundPublicError("gemini-4-argon").Message+" "+hint {
		t.Fatalf("模型不存在加提示：%+v", public)
	}
	unlisted := &aistudio.NoServingAccountError{Model: "gemini-3.1-pro-preview"}
	public = publicErrorFor(&aistudio.ModelInOtherPoolError{Err: unlisted, Model: "gemini-3.1-pro-preview", Pools: pools[:1]}, "gemini-3.1-pro-preview")
	if public.Status != http.StatusNotFound || public.Kind != publicKindModelNotFound ||
		public.Message != unlisted.PublicMessage()+" This model is available through the /pro endpoints." {
		t.Fatalf("不在模型列表加提示：%+v", public)
	}
	listed := &aistudio.NoServingAccountError{Model: "gemini-3.1-pro-preview", Listed: true}
	if public := publicErrorFor(&aistudio.ModelInOtherPoolError{Err: listed, Pools: pools}, "gemini-3.1-pro-preview"); strings.Contains(public.Message, "endpoints") {
		t.Fatalf("模型在列时不应加提示：%+v", public)
	}
	if public := publicErrorFor(fmt.Errorf("%w: x", aistudio.ErrModelNotFound), "x"); strings.Contains(public.Message, "endpoints") {
		t.Fatalf("没有改用入口时不应加提示：%+v", public)
	}
	// 经处理链返回：OpenAI 错误体的 code 为 model_not_found，消息带提示
	service := &poolRecordingService{genErr: notFound}
	exclusive, proPool := proFlags(true, true)
	handler := proTestHandler(service, nil, exclusive, proPool, t.TempDir())
	recorder := serve(handler, authorizedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`))
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if recorder.Code != http.StatusNotFound || body.Error.Code != "model_not_found" || !strings.HasSuffix(body.Error.Message, hint) {
		t.Fatalf("普通路径请求只有 Pro/Ultra 能服务的模型：%d %s", recorder.Code, recorder.Body.String())
	}
	if !errors.Is(notFound, aistudio.ErrModelNotFound) {
		t.Fatal("带提示的错误仍应是模型不存在")
	}
}

// TestUsagePoolDimensionPro 用量接口与记录查询接受 pool=pro 的筛选，原样交给账本
func TestUsagePoolDimensionPro(t *testing.T) {
	ledger := &fakeLedger{}
	handler := ledgerTestHandler(ledger)
	if recorder := adminGet(handler, "/api/usage?"+usageRange()+"&stack=pool&pool=pro"); recorder.Code != http.StatusOK {
		t.Fatalf("按号池 pro 筛选：%d %s", recorder.Code, recorder.Body.String())
	}
	if got := strings.Join(ledger.usage[0].Filters["pool"], ","); got != UsagePoolPro {
		t.Fatalf("号池筛选 = %q，期望 pro", got)
	}
	if recorder := adminGet(handler, "/api/usage/records?"+usageRange()+"&pool=normal,pro,ultra"); recorder.Code != http.StatusOK {
		t.Fatalf("记录按号池筛选：%d %s", recorder.Code, recorder.Body.String())
	}
	if got := strings.Join(ledger.records[0].Filters["pool"], ","); got != "normal,pro,ultra" {
		t.Fatalf("记录的号池筛选 = %q", got)
	}
}
