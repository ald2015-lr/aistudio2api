package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// poolRecordingService 记录每次调用 context 中的号池；模型目录按号池返回不同的模型，文件接口按 fileErr 返回
type poolRecordingService struct {
	mu        sync.Mutex
	scopes    []aistudio.PoolScope
	generated []string
	modelCall atomic.Int32
	fileErr   error
	genErr    error
}

func (service *poolRecordingService) record(ctx context.Context) {
	service.mu.Lock()
	service.scopes = append(service.scopes, aistudio.PoolScopeFromContext(ctx))
	service.mu.Unlock()
}

func (service *poolRecordingService) lastScope(t *testing.T) aistudio.PoolScope {
	t.Helper()
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.scopes) == 0 {
		t.Fatal("服务没有被调用")
	}
	return service.scopes[len(service.scopes)-1]
}

func (service *poolRecordingService) Models(ctx context.Context) ([]aistudio.Model, error) {
	service.record(ctx)
	service.modelCall.Add(1)
	model := aistudio.Model{
		ID: "normal-model", Name: "Normal", Methods: []string{"generateContent"},
		Capabilities: map[string]bool{"chat_model": true, "thinking": true},
	}
	if aistudio.PoolScopeFromContext(ctx) == aistudio.PoolScopeUltra {
		model.ID, model.Name = "ultra-model", "Ultra"
	}
	return []aistudio.Model{model}, nil
}

func (service *poolRecordingService) CountTokens(ctx context.Context, _ aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	service.record(ctx)
	return aistudio.TokenCount{InputTokens: 1}, nil
}

func (service *poolRecordingService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.record(ctx)
	service.mu.Lock()
	service.generated = append(service.generated, request.Model)
	service.mu.Unlock()
	if service.genErr != nil {
		return nil, service.genErr
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "ok"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

func (service *poolRecordingService) UploadFile(ctx context.Context, _ aistudio.UploadRequest) (aistudio.FileRef, error) {
	service.record(ctx)
	return aistudio.FileRef{}, service.fileErr
}

func (service *poolRecordingService) FileMetadata(ctx context.Context, _ string) (aistudio.FileMetadata, error) {
	service.record(ctx)
	return aistudio.FileMetadata{}, service.fileErr
}

func (service *poolRecordingService) DownloadFile(ctx context.Context, _ string) (aistudio.MediaStream, error) {
	service.record(ctx)
	return aistudio.MediaStream{}, service.fileErr
}

func (service *poolRecordingService) DeleteFile(ctx context.Context, _ string) error {
	service.record(ctx)
	return service.fileErr
}

func ultraTestHandler(service aistudio.Service, admin AdminService, exclusive *atomic.Bool) http.Handler {
	return NewHandler(service, Config{APIKey: "sk-test", Admin: admin, UltraExclusive: exclusive.Load})
}

func authorizedRequest(method string, target string, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("Content-Type", "application/json")
	return request
}

func serve(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestUltraRouting /ultra/v1 与 /ultra/v1beta 走与主路由相同的处理链并标记 Ultra 号池；普通路径按 ULTRA_EXCLUSIVE 标记
// 普通号池或不限号池；其余 /ultra/* 路径（含 /ultra/trace 与 /trace/ultra）返回 404
func TestUltraRouting(t *testing.T) {
	service := &poolRecordingService{}
	exclusive := &atomic.Bool{}
	exclusive.Store(true)
	handler := ultraTestHandler(service, nil, exclusive)
	for _, test := range []struct {
		path      string
		exclusive bool
		want      aistudio.PoolScope
	}{
		{path: "/ultra/v1/models", exclusive: true, want: aistudio.PoolScopeUltra},
		{path: "/ultra/v1beta/models", exclusive: true, want: aistudio.PoolScopeUltra},
		{path: "/ultra/v1/models", exclusive: false, want: aistudio.PoolScopeUltra},
		{path: "/v1/models", exclusive: true, want: aistudio.PoolScopeNormal},
		{path: "/v1beta/models", exclusive: true, want: aistudio.PoolScopeNormal},
		{path: "/v1/models", exclusive: false, want: aistudio.PoolScopeAll},
		{path: "/trace/v1/models", exclusive: true, want: aistudio.PoolScopeNormal},
	} {
		exclusive.Store(test.exclusive)
		recorder := serve(handler, authorizedRequest(http.MethodGet, test.path, ""))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s 状态码 = %d，期望 200: %s", test.path, recorder.Code, recorder.Body.String())
		}
		if got := service.lastScope(t); got != test.want {
			t.Fatalf("%s（独占=%t）的号池 = %q，期望 %q", test.path, test.exclusive, got.String(), test.want.String())
		}
	}
	for _, path := range []string{"/ultra/", "/ultra/api/status", "/ultra/trace/v1/models", "/trace/ultra/v1/models", "/ultra/health", "/ultra/v2/models"} {
		if recorder := serve(handler, authorizedRequest(http.MethodGet, path, "")); recorder.Code != http.StatusNotFound {
			t.Fatalf("%s 状态码 = %d，期望 404", path, recorder.Code)
		}
	}
}

// TestUltraRoutingKeepsHandlerChain /ultra 与主路由共用密钥校验、来源检查、CORS 与各协议的错误格式
func TestUltraRoutingKeepsHandlerChain(t *testing.T) {
	exclusive := &atomic.Bool{}
	handler := ultraTestHandler(&poolRecordingService{}, nil, exclusive)

	missingKey := httptest.NewRequest(http.MethodGet, "/ultra/v1/models", nil)
	if recorder := serve(handler, missingKey); recorder.Code != http.StatusUnauthorized ||
		!strings.Contains(recorder.Body.String(), "invalid_api_key") {
		t.Fatalf("缺少密钥：%d %s，期望 OpenAI 格式的 401", recorder.Code, recorder.Body.String())
	}
	anthropic := httptest.NewRequest(http.MethodPost, "/ultra/v1/messages", strings.NewReader("{}"))
	if recorder := serve(handler, anthropic); recorder.Code != http.StatusUnauthorized ||
		!strings.Contains(recorder.Body.String(), "authentication_error") {
		t.Fatalf("Anthropic 缺少密钥：%d %s，期望 Anthropic 格式的 401", recorder.Code, recorder.Body.String())
	}
	gemini := httptest.NewRequest(http.MethodPost, "/ultra/v1beta/models/normal-model:generateContent", strings.NewReader("{}"))
	if recorder := serve(handler, gemini); recorder.Code != http.StatusUnauthorized ||
		!strings.Contains(recorder.Body.String(), "UNAUTHENTICATED") {
		t.Fatalf("Gemini 缺少密钥：%d %s，期望 Gemini 格式的 401", recorder.Code, recorder.Body.String())
	}
	options := httptest.NewRequest(http.MethodOptions, "/ultra/v1/chat/completions", nil)
	if recorder := serve(handler, options); recorder.Code != http.StatusNoContent || recorder.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS 预检：%d %v", recorder.Code, recorder.Header())
	}

	// 默认密钥时拒绝外部网页来源的浏览器请求，与主路由一致
	defaultKey := NewHandler(&poolRecordingService{}, Config{})
	browser := httptest.NewRequest(http.MethodGet, "/ultra/v1/models", nil)
	browser.Header.Set("Authorization", "Bearer sk-onechat-fun-fun")
	browser.Header.Set("Origin", "https://evil.example.com")
	if recorder := serve(defaultKey, browser); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("外部网页来源：%d，期望 401", recorder.Code)
	}
}

// TestUltraModelsAndAliasCachePerPool 模型列表与别名解析按号池：/ultra 只看 Ultra 号池的目录，别名缓存按号池分开
func TestUltraModelsAndAliasCachePerPool(t *testing.T) {
	service := &poolRecordingService{}
	exclusive := &atomic.Bool{}
	exclusive.Store(true)
	handler := ultraTestHandler(service, nil, exclusive)
	listed := func(path string) string {
		t.Helper()
		recorder := serve(handler, authorizedRequest(http.MethodGet, path, ""))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, recorder.Code)
		}
		return recorder.Body.String()
	}
	if body := listed("/ultra/v1/models"); !strings.Contains(body, "ultra-model") || strings.Contains(body, "normal-model") {
		t.Fatalf("/ultra/v1/models 应只列 Ultra 号池的模型: %s", body)
	}
	if body := listed("/v1/models"); !strings.Contains(body, "normal-model") || strings.Contains(body, "ultra-model") {
		t.Fatalf("/v1/models 应只列普通号池的模型: %s", body)
	}
	if body := listed("/ultra/v1beta/models"); !strings.Contains(body, "models/ultra-model") {
		t.Fatalf("/ultra/v1beta/models 应列 Ultra 号池的模型: %s", body)
	}
	anthropicModels := authorizedRequest(http.MethodGet, "/ultra/v1/models", "")
	anthropicModels.Header.Set("Anthropic-Version", "2023-06-01")
	if body := serve(handler, anthropicModels).Body.String(); !strings.Contains(body, "ultra-model") {
		t.Fatalf("Anthropic 模型列表应列 Ultra 号池的模型: %s", body)
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
	// 后缀别名只在请求号池的目录里解析：Ultra 独有模型的别名经 /ultra 解析，普通路径保持原样（由上游按模型不存在处理）
	if got := chat("/ultra/v1/chat/completions", "ultra-model-nothinking"); got != "ultra-model" {
		t.Fatalf("/ultra 别名解析为 %q，期望 ultra-model", got)
	}
	if got := chat("/v1/chat/completions", "ultra-model-nothinking"); got != "ultra-model-nothinking" {
		t.Fatalf("普通路径不应解析 Ultra 号池的别名，实际 %q", got)
	}
	if got := chat("/v1/chat/completions", "normal-model-nothinking"); got != "normal-model" {
		t.Fatalf("普通路径别名解析为 %q，期望 normal-model", got)
	}
	if service.lastScope(t) != aistudio.PoolScopeNormal {
		t.Fatal("普通路径的生成请求应标记普通号池")
	}
	// 再次请求使用各自号池的缓存快照，不再重算目录
	before := service.modelCall.Load()
	chat("/ultra/v1/chat/completions", "ultra-model-nothinking")
	chat("/v1/chat/completions", "normal-model-nothinking")
	if service.modelCall.Load() != before {
		t.Fatal("别名目录应按号池缓存")
	}
}

// TestUltraAccessLogAndErrors 请求日志带 /ultra 路径与 pool=ultra；Ultra 号池没有可用账户按 503 并说明 Ultra 号池；
// 文件属于另一号池时按 400 返回英文说明
func TestUltraAccessLogAndErrors(t *testing.T) {
	admin := &capturingAdmin{}
	service := &poolRecordingService{}
	exclusive := &atomic.Bool{}
	exclusive.Store(true)
	handler := ultraTestHandler(service, admin, exclusive)
	serve(handler, authorizedRequest(http.MethodPost, "/ultra/v1/chat/completions", `{"model":"ultra-model","messages":[{"role":"user","content":"hi"}]}`))
	serve(handler, authorizedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"normal-model","messages":[{"role":"user","content":"hi"}]}`))
	entries := admin.finished()
	if len(entries) != 2 {
		t.Fatalf("请求日志条数 = %d", len(entries))
	}
	if entries[0].Path != "/ultra/v1/chat/completions" || entries[0].Pool != "ultra" || !entries[0].Authorized {
		t.Fatalf("Ultra 请求日志: path=%q pool=%q authorized=%t", entries[0].Path, entries[0].Pool, entries[0].Authorized)
	}
	if entries[1].Path != "/v1/chat/completions" || entries[1].Pool != "" {
		t.Fatalf("普通请求日志: path=%q pool=%q", entries[1].Path, entries[1].Pool)
	}

	service.genErr = &aistudio.AccountsNotReadyError{Reasons: []string{"没有权益为 Ultra 的账户"}, Pool: aistudio.PoolScopeUltra}
	recorder := serve(handler, authorizedRequest(http.MethodPost, "/ultra/v1/chat/completions", `{"model":"ultra-model","messages":[{"role":"user","content":"hi"}]}`))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "Ultra pool") {
		t.Fatalf("Ultra 号池没有账户：%d %s，期望说明 Ultra 号池的 503", recorder.Code, recorder.Body.String())
	}
	recorder = serve(handler, authorizedRequest(http.MethodPost, "/ultra/v1/messages", `{"model":"ultra-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	var anthropic struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &anthropic)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(anthropic.Error.Message, "Ultra pool") {
		t.Fatalf("Anthropic Ultra 号池没有账户：%d %s", recorder.Code, recorder.Body.String())
	}

	service.fileErr = &aistudio.ResourcePoolMismatchError{ResourceID: "files/abc", Owner: aistudio.PoolScopeNormal}
	recorder = serve(handler, authorizedRequest(http.MethodGet, "/ultra/v1/files/files-abc", ""))
	body, _ := io.ReadAll(recorder.Body)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(string(body), "normal pool") {
		t.Fatalf("跨号池文件：%d %s，期望说明文件属于普通号池的 400", recorder.Code, body)
	}
	if service.lastScope(t) != aistudio.PoolScopeUltra {
		t.Fatal("/ultra 文件接口应标记 Ultra 号池")
	}
}

// TestUltraBodyCapture 正文记录开启时 /ultra 的 POST 请求同样保存正文
func TestUltraBodyCapture(t *testing.T) {
	ledger := &fakeLedger{capture: true}
	handler := NewHandler(&poolRecordingService{}, Config{APIKey: "sk-test", Admin: &capturingAdmin{}, Ledger: ledger})
	serve(handler, authorizedRequest(http.MethodPost, "/ultra/v1/chat/completions", `{"model":"ultra-model","messages":[{"role":"user","content":"hi"}]}`))
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.bodies) != 1 || !strings.Contains(ledger.bodies[0].Request, "ultra-model") {
		t.Fatalf("/ultra 请求正文没有保存: %+v", ledger.bodies)
	}
}

// TestUsagePoolDimension 用量接口接受号池筛选，号池可以作为堆叠维度，记录查询同样接受号池筛选
func TestUsagePoolDimension(t *testing.T) {
	ledger := &fakeLedger{}
	handler := ledgerTestHandler(ledger)
	if recorder := adminGet(handler, "/api/usage?"+usageRange()+"&stack=pool&pool=ultra"); recorder.Code != http.StatusOK {
		t.Fatalf("按号池堆叠与筛选：%d %s", recorder.Code, recorder.Body.String())
	}
	query := ledger.usage[0]
	if query.Stack != "pool" || strings.Join(query.Filters["pool"], ",") != UsagePoolUltra {
		t.Fatalf("号池参数不对: stack=%s filters=%+v", query.Stack, query.Filters)
	}
	if recorder := adminGet(handler, "/api/usage/records?"+usageRange()+"&pool=normal,ultra"); recorder.Code != http.StatusOK {
		t.Fatalf("记录按号池筛选：%d %s", recorder.Code, recorder.Body.String())
	}
	if got := strings.Join(ledger.records[0].Filters["pool"], ","); got != "normal,ultra" {
		t.Fatalf("记录的号池筛选 = %q", got)
	}
}
