package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// fakeLedger 记录收到的查询与正文，可指定返回的错误
type fakeLedger struct {
	mu        sync.Mutex
	capture   bool
	bodies    []RequestBody
	usage     []UsageQuery
	records   []UsageRecordQuery
	exportErr error
	exportCSV string
}

func (ledger *fakeLedger) BodyCapture() bool { return ledger.capture }

func (ledger *fakeLedger) SaveBody(body RequestBody) {
	ledger.mu.Lock()
	ledger.bodies = append(ledger.bodies, body)
	ledger.mu.Unlock()
}

func (ledger *fakeLedger) Usage(_ context.Context, query UsageQuery) (UsageReport, error) {
	ledger.mu.Lock()
	ledger.usage = append(ledger.usage, query)
	ledger.mu.Unlock()
	return UsageReport{From: query.From, To: query.To, StackBy: query.Stack}, nil
}

func (ledger *fakeLedger) Records(_ context.Context, query UsageRecordQuery) (UsageRecordPage, error) {
	ledger.mu.Lock()
	ledger.records = append(ledger.records, query)
	ledger.mu.Unlock()
	return UsageRecordPage{Items: []UsageRecord{}}, nil
}

func (ledger *fakeLedger) ExportRecords(_ context.Context, _ UsageRecordQuery, output io.Writer) error {
	if ledger.exportCSV != "" {
		if _, err := io.WriteString(output, ledger.exportCSV); err != nil {
			return err
		}
	}
	return ledger.exportErr
}

func (ledger *fakeLedger) RequestBody(_ context.Context, id string) (RequestBody, error) {
	if id == "known" {
		return RequestBody{ID: id, Request: "in", Response: "out"}, nil
	}
	return RequestBody{}, ErrRequestBodyNotFound
}

// ledgerTestHandler 创建带管理令牌与账本的完整路由
func ledgerTestHandler(ledger RequestLedger) http.Handler {
	return NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Ledger: ledger})
}

// adminGet 携带管理令牌请求控制面
func adminGet(handler http.Handler, target string) *httptest.ResponseRecorder {
	request := localRequest(target)
	request.Header.Set("X-Admin-Token", testAdminToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// usageRange 返回最近一小时的 from 与 to 参数
func usageRange() string {
	now := time.Now().UTC().Truncate(time.Second)
	return "from=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(now.Format(time.RFC3339))
}

// TestLedgerRoutesRequireAdminToken 用量接口与其他控制面接口一样需要管理令牌，携带公开 API key 也不行
func TestLedgerRoutesRequireAdminToken(t *testing.T) {
	handler := ledgerTestHandler(&fakeLedger{})
	for _, target := range []string{
		"/api/usage?" + usageRange(), "/api/usage/records?" + usageRange(),
		"/api/usage/records.csv?" + usageRange(), "/api/requests/known/body",
	} {
		request := localRequest(target)
		request.Header.Set("Authorization", "Bearer sk-test")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s 没有管理令牌 status = %d，期望 401", target, recorder.Code)
		}
		if recorder := adminGet(handler, target); recorder.Code != http.StatusOK {
			t.Fatalf("%s 携带管理令牌 status = %d: %s", target, recorder.Code, recorder.Body.String())
		}
	}
}

// TestLedgerRoutesAbsentWithoutLedger 没有账本时不注册用量接口，返回 404 而不是解引用空账本
func TestLedgerRoutesAbsentWithoutLedger(t *testing.T) {
	handler := ledgerTestHandler(nil)
	if recorder := adminGet(handler, "/api/usage?"+usageRange()); recorder.Code != http.StatusNotFound {
		t.Fatalf("没有账本时 status = %d，期望 404", recorder.Code)
	}
}

// TestUsageQueryValidation 校验时间范围、时区、分桶与堆叠维度，筛选值按逗号拆分并去重
func TestUsageQueryValidation(t *testing.T) {
	ledger := &fakeLedger{}
	handler := ledgerTestHandler(ledger)
	now := time.Now().UTC().Truncate(time.Second)
	format := func(value time.Time) string { return url.QueryEscape(value.Format(time.RFC3339)) }
	for _, test := range []struct {
		name  string
		query string
	}{
		{name: "缺少时间", query: ""},
		{name: "先后颠倒", query: "from=" + format(now) + "&to=" + format(now.Add(-time.Hour))},
		{name: "超过 93 天", query: "from=" + format(now.Add(-94*24*time.Hour)) + "&to=" + format(now)},
		{name: "时区无效", query: usageRange() + "&tz=Mars/Base"},
		{name: "分桶无效", query: usageRange() + "&bucket=7"},
		{name: "分桶过多", query: "from=" + format(now.Add(-30*24*time.Hour)) + "&to=" + format(now) + "&bucket=60"},
		{name: "堆叠维度无效", query: usageRange() + "&stack=status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if recorder := adminGet(handler, "/api/usage?"+test.query); recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d，期望 400: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if len(ledger.usage) != 0 {
		t.Fatalf("无效查询不应到达账本: %d", len(ledger.usage))
	}
	recorder := adminGet(handler, "/api/usage?"+usageRange()+"&tz=Asia/Shanghai&stack=account&model=a,b,a&model=c&state=failed")
	if recorder.Code != http.StatusOK {
		t.Fatalf("有效查询 status = %d: %s", recorder.Code, recorder.Body.String())
	}
	query := ledger.usage[0]
	if query.Location.String() != "Asia/Shanghai" || query.Stack != "account" || query.Bucket != time.Minute {
		t.Fatalf("查询参数不对: location=%s stack=%s bucket=%s", query.Location, query.Stack, query.Bucket)
	}
	if strings.Join(query.Filters["model"], ",") != "a,b,c" || strings.Join(query.Filters["state"], ",") != "failed" {
		t.Fatalf("筛选值不对: %+v", query.Filters)
	}
}

// TestUsageRecordQueryParsing 记录查询解析游标、状态码、关键字与每页条数
func TestUsageRecordQueryParsing(t *testing.T) {
	ledger := &fakeLedger{}
	handler := ledgerTestHandler(ledger)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	cursor := EncodeUsageCursor(UsageCursor{Time: at, ID: "req_1"})
	recorder := adminGet(handler, "/api/usage/records?"+usageRange()+"&cursor="+cursor+"&status=429&q=%20timeout%20&limit=20&account=x")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	query := ledger.records[0]
	if query.Before == nil || !query.Before.Time.Equal(at) || query.Before.ID != "req_1" {
		t.Fatalf("游标没有往返: %+v", query.Before)
	}
	if query.Status != 429 || query.Search != "timeout" || query.Limit != 20 || query.Filters["account"][0] != "x" {
		t.Fatalf("查询参数不对: %+v", query)
	}
	for _, bad := range []string{"&cursor=!!", "&status=99", "&limit=500"} {
		if recorder := adminGet(handler, "/api/usage/records?"+usageRange()+bad); recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d，期望 400", bad, recorder.Code)
		}
	}
}

// TestUsageExportErrors 导出在写出之前失败时返回 JSON 错误；写出之后失败只能中断下载
func TestUsageExportErrors(t *testing.T) {
	failing := ledgerTestHandler(&fakeLedger{exportErr: errors.New("磁盘错误")})
	recorder := adminGet(failing, "/api/usage/records.csv?"+usageRange())
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "usage_export_failed") {
		t.Fatalf("写出之前失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Header().Get("Content-Type"), "text/csv") {
		t.Fatal("错误响应不应声明为 CSV")
	}
	partial := ledgerTestHandler(&fakeLedger{exportCSV: "time,id\n", exportErr: errors.New("中途失败")})
	recorder = adminGet(partial, "/api/usage/records.csv?"+usageRange())
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/csv") ||
		!strings.Contains(recorder.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("写出之后失败: status=%d header=%v", recorder.Code, recorder.Header())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("导出响应应禁止缓存")
	}
}

// TestRequestBodyEndpoint 正文不存在时返回 404
func TestRequestBodyEndpoint(t *testing.T) {
	handler := ledgerTestHandler(&fakeLedger{})
	if recorder := adminGet(handler, "/api/requests/missing/body"); recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404", recorder.Code)
	}
	recorder := adminGet(handler, "/api/requests/known/body")
	var body RequestBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Request != "in" || body.Response != "out" {
		t.Fatalf("正文不对: %s err=%v", recorder.Body.String(), err)
	}
}

// bodyEchoService 原样回复一段固定文本
type bodyEchoService struct{ modelListService }

func (bodyEchoService) Generate(context.Context, aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

// TestRequestBodyCapture 正文记录只在开启时保存通过密钥校验的 POST 请求，ID 与请求日志一致；token 计数请求与请求头不保存
func TestRequestBodyCapture(t *testing.T) {
	ledger := &fakeLedger{capture: true}
	admin := &capturingAdmin{}
	handler := NewHandler(bodyEchoService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin, Ledger: ledger})
	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`

	if recorder := postJSON(t, handler, "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer wrong"}); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("错误密钥 status = %d", recorder.Code)
	}
	if len(ledger.bodies) != 0 {
		t.Fatal("未通过密钥校验的请求不应保存正文")
	}
	recorder := postJSON(t, handler, "/v1/chat/completions", body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(ledger.bodies) != 1 {
		t.Fatalf("保存了 %d 条正文，期望 1", len(ledger.bodies))
	}
	saved := ledger.bodies[0]
	entries := admin.finished()
	if saved.ID == "" || saved.ID != entries[len(entries)-1].RequestID {
		t.Fatalf("正文 ID=%q 与请求日志 ID=%q 不一致", saved.ID, entries[len(entries)-1].RequestID)
	}
	if saved.Request != body || saved.RequestSize != int64(len(body)) || !strings.Contains(saved.Response, "hello") {
		t.Fatalf("正文内容不对: %+v", saved)
	}
	if strings.Contains(saved.Request, "sk-test") || strings.Contains(saved.Response, "sk-test") {
		t.Fatal("正文中不应出现 API key")
	}

	postJSON(t, handler, "/v1/messages/count_tokens", `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`, nil)
	if len(ledger.bodies) != 1 {
		t.Fatal("token 计数请求不应保存正文")
	}
	ledger.capture = false
	postJSON(t, handler, "/v1/chat/completions", body, nil)
	if len(ledger.bodies) != 1 {
		t.Fatal("关闭正文记录后不应再保存")
	}
}

// TestBodyCaptureTruncates 正文超过上限时只保存前缀，同时记下原始字节数
func TestBodyCaptureTruncates(t *testing.T) {
	capture := &bodyCapture{}
	capture.add([]byte(strings.Repeat("a", requestBodyLimit-10)))
	capture.add([]byte(strings.Repeat("b", 100)))
	text, size := capture.result()
	if len(text) != requestBodyLimit || size != requestBodyLimit+90 || !strings.HasSuffix(text, "bbbbbbbbbb") {
		t.Fatalf("截断不对: len=%d size=%d", len(text), size)
	}
}
