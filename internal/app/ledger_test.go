package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/requestdb"
)

// TestRequestProtocol 按路径归类协议：排查路由去掉 /trace 前缀，token 计数单独归类
func TestRequestProtocol(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/chat/completions":                                "openai-chat",
		"/trace/v1/chat/completions":                          "openai-chat",
		"/v1/responses":                                       "openai-responses",
		"/trace/v1/responses":                                 "openai-responses",
		"/v1/messages":                                        "anthropic",
		"/v1/messages/count_tokens":                           "count_tokens",
		"/trace/v1/messages/count_tokens":                     "count_tokens",
		"/v1beta/models/gemini-x:generateContent":             "gemini",
		"/trace/v1beta/models/gemini-x:streamGenerateContent": "gemini",
		"/v1beta/models/gemini-x:countTokens":                 "count_tokens",
		"/v1/images/generations":                              "images",
		"/v1/audio/speech":                                    "audio",
		"/v1/videos":                                          "videos",
		"/v1/files":                                           "files",
		"/v1/unknown":                                         "other",
	} {
		if got := requestProtocol(path); got != want {
			t.Fatalf("requestProtocol(%q) = %q，期望 %q", path, got, want)
		}
	}
}

// TestLedgerEntrySelection 只有通过密钥校验的 POST 写入账本，token 计数请求不计入
func TestLedgerEntrySelection(t *testing.T) {
	for _, test := range []struct {
		name  string
		entry api.AccessLog
		want  bool
	}{
		{name: "生成请求", entry: api.AccessLog{Method: http.MethodPost, Path: "/v1/chat/completions", Authorized: true}, want: true},
		{name: "排查路由", entry: api.AccessLog{Method: http.MethodPost, Path: "/trace/v1/messages", Authorized: true}, want: true},
		{name: "未通过校验", entry: api.AccessLog{Method: http.MethodPost, Path: "/v1/chat/completions"}},
		{name: "GET 请求", entry: api.AccessLog{Method: http.MethodGet, Path: "/v1/models", Authorized: true}},
		{name: "Anthropic 计数", entry: api.AccessLog{Method: http.MethodPost, Path: "/v1/messages/count_tokens", Authorized: true}},
		{name: "Gemini 计数", entry: api.AccessLog{Method: http.MethodPost, Path: "/trace/v1beta/models/x:countTokens", Authorized: true}},
	} {
		if got := ledgerEntry(test.entry); got != test.want {
			t.Fatalf("%s: ledgerEntry = %v，期望 %v", test.name, got, test.want)
		}
	}
}

// TestRequestRowProjection 账本记录与请求日志的结果、token 和本地诊断字段一致
func TestRequestRowProjection(t *testing.T) {
	finished := time.Now().UTC()
	row := requestRow(api.AccessLog{
		RequestID: "chatcmpl_1", Method: http.MethodPost, Path: "/trace/v1/chat/completions", Status: http.StatusBadRequest,
		Latency: 3 * time.Second, FirstEvent: time.Second, QueueWait: 200 * time.Millisecond,
		Usage: &aistudio.Usage{InputTokens: 10, ToolTokens: 2, ReasoningTokens: 5, OutputTokens: 7, TotalTokens: 24},
		Model: "gemini-test", Account: "a@example.com", Channel: "playground", ServedModel: "other-model", ReplyHash: "abcdef",
		Downgrade: &aistudio.DowngradeDecision{Verdict: "rejected"},
		Attempts:  []api.RequestAttempt{{Account: "b@example.com", Error: "429"}},
	}, finished)
	if row.ID != "chatcmpl_1" || row.Protocol != "openai-chat" || row.Path != "/trace/v1/chat/completions" || row.State != "failed" {
		t.Fatalf("记录身份不对: %+v", row)
	}
	if row.Error != "HTTP 400" || row.Queue != 200*time.Millisecond || len(row.Attempts) != 1 {
		t.Fatalf("错误或排队不对: error=%q queue=%s attempts=%d", row.Error, row.Queue, len(row.Attempts))
	}
	if row.InputTokens != 12 || row.ReasoningTokens != 5 || row.ReplyTokens != 7 || row.TotalTokens != 24 {
		t.Fatalf("token 投影与请求日志不一致: %+v", row)
	}
	if row.ServedModel != "other-model" || row.Downgrade != "rejected" || row.ReplyHash != "abcdef" {
		t.Fatalf("本地诊断字段丢失: served=%q downgrade=%q reply=%q", row.ServedModel, row.Downgrade, row.ReplyHash)
	}
}

// TestRequestLedgerNilInterface 账本没有打开时交给路由的是 nil 接口：不注册用量接口，请求返回 404 而不是崩溃
func TestRequestLedgerNilInterface(t *testing.T) {
	manager := &runtimeManager{}
	if ledger := manager.requestLedger(); ledger != nil {
		t.Fatalf("没有账本时返回了非 nil 接口: %#v", ledger)
	}
	handler := api.NewHandler(stoppedService{}, api.Config{APIKey: "sk-test", AdminToken: "token", Ledger: manager.requestLedger()})
	request := httptest.NewRequest(http.MethodGet, "/api/usage?from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z", nil)
	request.Header.Set("X-Admin-Token", "token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("没有账本时 status = %d，期望 404", recorder.Code)
	}
}

// TestOpenRequestLedgerFailureContinues 账本打开失败只写 WARN，管理器不持有账本，记录请求时不崩溃
func TestOpenRequestLedgerFailureContinues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &runtimeManager{requests: newRequestRegistry(ctx), current: &runtimeGeneration{admin: recordingAdmin{}}}
	// 父路径是普通文件，无法创建目录
	if ledger := openRequestLedger(manager, filepath.Join(blocker, "runtime", "requests.db"), false); ledger != nil {
		t.Fatal("打开失败时不应返回账本")
	}
	if manager.ledger != nil || manager.requestLedger() != nil {
		t.Fatal("打开失败时管理器不应持有账本")
	}
	manager.requests.mu.Lock()
	logs := append([]api.AdminLog(nil), manager.requests.logs...)
	manager.requests.mu.Unlock()
	found := false
	for _, entry := range logs {
		found = found || entry.Level == "WARN" && strings.Contains(entry.Message, "请求账本打开失败")
	}
	if !found {
		t.Fatalf("没有写 WARN 日志: %+v", logs)
	}
	manager.RecordAccessLog(api.AccessLog{Method: http.MethodPost, Path: "/v1/chat/completions", Authorized: true, RequestID: "x"})
}

// TestRecordAccessLogWritesLedger 通过密钥校验的生成请求写入账本，token 计数与未校验请求不写入
func TestRecordAccessLogWritesLedger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "runtime", "requests.db")
	manager := &runtimeManager{requests: newRequestRegistry(ctx), current: &runtimeGeneration{admin: recordingAdmin{}}}
	ledger := openRequestLedger(manager, path, true)
	if ledger == nil || manager.requestLedger() == nil || !ledger.BodyCapture() {
		t.Fatal("账本没有打开或正文记录没有按配置开启")
	}
	manager.RecordAccessLog(api.AccessLog{RequestID: "gen", Method: http.MethodPost, Path: "/v1/chat/completions", Status: 200, Authorized: true, Model: "m"})
	manager.RecordAccessLog(api.AccessLog{RequestID: "count", Method: http.MethodPost, Path: "/v1/messages/count_tokens", Status: 200, Authorized: true})
	manager.RecordAccessLog(api.AccessLog{RequestID: "anon", Method: http.MethodPost, Path: "/v1/chat/completions", Status: 401})
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := requestdb.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	now := time.Now().UTC()
	page, err := reopened.Records(ctx, api.UsageRecordQuery{From: now.Add(-time.Hour), To: now.Add(time.Minute), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "gen" || page.Items[0].Protocol != "openai-chat" {
		t.Fatalf("账本记录不对: %+v", page.Items)
	}
}

// TestUpdateRuntimeConfigKeepsRequestBodyLog 管理页面保存配置时沿用 .env 中的 REQUEST_BODY_LOG
func TestUpdateRuntimeConfigKeepsRequestBodyLog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("REQUEST_BODY_LOG=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}
	if _, err := admin.UpdateRuntimeConfig(ctx, validRuntimeConfig()); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(path)
	if err != nil || !saved.RequestBodyLog {
		t.Fatalf("保存后 REQUEST_BODY_LOG=%v err=%v，期望沿用 true", saved.RequestBodyLog, err)
	}
}
