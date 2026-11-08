package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 工具约束要求必须调用时生成服务扣住事件（aistudio/tool_guard.go）：经真实的 PooledService 与 Client 确认流式扣住期间数据型心跳照常写出

const toolHoldModel = "hold-model"

// rejectRPC 为不应被调用的普通 RPC 传输（生成只走受保护传输）
type rejectRPC struct{}

func (rejectRPC) Do(context.Context, aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	return nil, errors.New("测试不应调用普通 RPC")
}

// pausedBody 先读出 head，等 pause 之后再读出 tail
type pausedBody struct {
	head, tail *strings.Reader
	pause      time.Duration
	once       sync.Once
}

func (body *pausedBody) Read(buffer []byte) (int, error) {
	if body.head.Len() > 0 {
		return body.head.Read(buffer)
	}
	body.once.Do(func() { time.Sleep(body.pause) })
	return body.tail.Read(buffer)
}

func (body *pausedBody) Close() error { return nil }

// toolHoldService 返回一个账号、模型支持函数调用的真实 PooledService，受保护传输按 respond 返回上游响应
func toolHoldService(t *testing.T, respond func(call int) io.ReadCloser) aistudio.Service {
	t.Helper()
	store := aistudio.NewAccountStore(t.TempDir())
	pool := aistudio.NewAccountPool(nil, 2)
	t.Cleanup(pool.WaitBackground)
	cookies := make([]aistudio.StateCookie, 0, 3)
	for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
		cookies = append(cookies, aistudio.StateCookie{Name: name, Value: "test", Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None"})
	}
	account, publish, err := store.Create(aistudio.DefaultAccountConfig("hold@example.com"), aistudio.StorageState{Cookies: cookies, Origins: []aistudio.StorageOrigin{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(account); err != nil {
		t.Fatal(err)
	}
	if err := publish.Release(); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetCatalog(account.ID, aistudio.BenefitTierFree, []aistudio.Model{{
		ID: toolHoldModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true, "function_declarations": true},
	}}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	client, err := aistudio.NewClient(aistudio.ClientOptions{
		Transport: rejectRPC{},
		Protected: aistudio.ProtectedTransportFunc(func(context.Context, aistudio.GenerateRequest, aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
			mu.Lock()
			calls++
			call := calls
			mu.Unlock()
			return &aistudio.RPCResponse{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {aistudio.JSONProtobufContentType}}, Body: respond(call),
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	row := make([]any, 83)
	row[0], row[3], row[5], row[6] = "models/"+toolHoldModel, strings.ToUpper(toolHoldModel), 1048576, 65536
	row[7], row[64], row[77], row[82] = []string{"generateContent", "countTokens"}, []int{1, 10}, 2, []int{3}
	raw, _ := json.Marshal([]any{[]any{row}})
	catalog, err := aistudio.ParseModelCatalogRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	client.PrimeAccountCatalog(account.ID, aistudio.BenefitTierFree, catalog)
	pooled, err := aistudio.NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	return pooled
}

// toolHoldChat 为强制调用 Kei_Workflow 的 Chat Completions 流式请求
const toolHoldChat = `{"model":"` + toolHoldModel + `","stream":true,"messages":[{"role":"user","content":"run the workflow"}],` +
	`"tools":[{"type":"function","function":{"name":"Kei_Workflow","parameters":{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]}}}],` +
	`"tool_choice":{"type":"function","function":{"name":"Kei_Workflow"}}}`

const (
	toolHoldText = `[[[[[[null,"plain answer"]],"model"]]]]`
	toolHoldCall = `[[[[[[null,null,null,null,null,null,null,null,null,null,["Kei_Workflow",[[["task",[null,null,"go"]]]]]]],"model"]]]]`
	toolHoldStop = `[[[null,1]]]`
)

// sseChunks 解析 Chat Completions SSE 的 data 块（[DONE] 除外）
func sseChunks(t *testing.T, body string) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("SSE 块不是 JSON: %s", data)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

// chatDelta 返回块的 delta（没有时为 nil）
func chatDelta(chunk map[string]any) map[string]any {
	choices, _ := chunk["choices"].([]any)
	if len(choices) != 1 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	return delta
}

// TestToolHoldKeepsDataHeartbeat 强制调用指定函数的流式请求：上游先出正文、过一会儿才出调用，扣住期间客户端只收到数据型心跳
// （空 delta 块），调用出现后扣住的正文紧跟着调用交出；第一次只回复文字时客户端只收到第二次的结果
func TestToolHoldKeepsDataHeartbeat(t *testing.T) {
	withHeartbeatInterval(t, 40*time.Millisecond)
	withStreamStartHold(t, 60*time.Millisecond)
	for _, test := range []struct {
		name    string
		respond func(call int) io.ReadCloser
		text    bool
	}{
		{name: "扣住到调用出现", text: true, respond: func(int) io.ReadCloser {
			return &pausedBody{
				head: strings.NewReader("[[" + toolHoldText + ","), tail: strings.NewReader(toolHoldCall + "," + toolHoldStop + "]]"),
				pause: 400 * time.Millisecond,
			}
		}},
		{name: "第一次只回复文字", respond: func(call int) io.ReadCloser {
			if call == 1 {
				return &pausedBody{
					head: strings.NewReader("[[" + toolHoldText + ","), tail: strings.NewReader(toolHoldStop + "]]"), pause: 300 * time.Millisecond,
				}
			}
			return io.NopCloser(strings.NewReader("[[" + toolHoldCall + "," + toolHoldStop + "]]"))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(toolHoldService(t, test.respond), Config{APIKey: "sk-test"})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(toolHoldChat))
			request.Header.Set("Authorization", "Bearer sk-test")
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("状态 %d：%s", recorder.Code, recorder.Body.String())
			}
			chunks := sseChunks(t, recorder.Body.String())
			heartbeats, contentAt, callAt := 0, -1, -1
			for index, chunk := range chunks {
				delta := chatDelta(chunk)
				switch {
				case delta == nil:
				case delta["tool_calls"] != nil:
					if callAt < 0 {
						callAt = index
					}
				case delta["content"] != nil && delta["content"] != "":
					if contentAt < 0 {
						contentAt = index
					}
				case len(delta) == 0 && callAt < 0 && contentAt < 0:
					heartbeats++
				}
			}
			if callAt < 0 {
				t.Fatalf("没有收到函数调用：%s", recorder.Body.String())
			}
			if heartbeats < 3 {
				t.Fatalf("扣住期间只写了 %d 个心跳块，期望照常每个间隔一块：%s", heartbeats, recorder.Body.String())
			}
			if test.text && contentAt != callAt-1 {
				t.Fatalf("扣住的正文应紧跟着调用交出（正文在第 %d 块，调用在第 %d 块）", contentAt, callAt)
			}
			if !test.text && strings.Contains(recorder.Body.String(), "plain answer") {
				t.Fatal("被放弃那次的正文不应交出")
			}
		})
	}
}
