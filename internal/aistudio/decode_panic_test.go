package aistudio

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// panickingBody 为读取时 panic 的上游响应体，记下是否被关闭
type panickingBody struct {
	closed atomic.Bool
}

func (body *panickingBody) Read([]byte) (int, error) {
	panic("解码中 panic")
}

func (body *panickingBody) Close() error {
	body.closed.Store(true)
	return nil
}

// panicClient 返回上游响应体读取时 panic 的 Client；interaction 为真时模型走 CreateInteractionStream
func panicClient(t *testing.T, body *panickingBody, interaction bool) *Client {
	t.Helper()
	client, err := NewClient(ClientOptions{
		Transport: ceilingTransport{},
		Protected: ProtectedTransportFunc(func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error) {
			return &RPCResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {JSONProtobufContentType}}, Body: body}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	model := Model{ID: "test", Methods: []string{"generateContent"}, OutputTokenLimit: 65536, Capabilities: map[string]bool{"chat_model": true}}
	entry := modelEntry{model: model, defaults: GenerationDefaults{MaxOutputTokens: 65536}}
	if interaction {
		entry.defaults = omniTestDefaults()
	}
	client.catalogs["alice@example.com"] = modelCatalog{models: []Model{model}, entries: map[string]modelEntry{"test": entry}}
	return client
}

func panicRequest() GenerateRequest {
	return GenerateRequest{
		ID: "req-decode-panic", Model: "test", AccountID: "alice@example.com",
		Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}},
	}
}

// TestClientGenerateDecodePanic 经 Client.Generate：解码上游事件流时 panic，以错误事件结束这次尝试并关闭事件流，上游响应体被关闭，进程不退出。
// 原先恢复时没有关闭响应体（关闭响应体的 AfterFunc 已先被 defer 停掉，正常路径的显式关闭被 panic 跳过），到 AI Studio 的连接一直占着
func TestClientGenerateDecodePanic(t *testing.T) {
	body := &panickingBody{}
	events, err := panicClient(t, body, false).Generate(context.Background(), panicRequest())
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	timeout := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case event, ok := <-events:
			if !ok {
				done = true
				break
			}
			got = append(got, event)
		case <-timeout:
			t.Fatal("事件流没有关闭")
		}
	}
	if len(got) != 1 || got[0].Kind != EventError || !strings.Contains(got[0].Err.Error(), "panic") {
		t.Fatalf("事件 = %+v，期望 panic 转成的一个错误事件", got)
	}
	if !body.closed.Load() {
		t.Fatal("panic 恢复后应关闭上游响应体")
	}
}

// TestInteractionDecodePanic Interaction 流（gemini-omni）解码时 panic：还没交出事件流时按错误返回（调用方照旧换号或返回错误），
// 上游响应体被关闭，进程不退出；原先这里没有恢复，panic 会让整个进程退出
func TestInteractionDecodePanic(t *testing.T) {
	body := &panickingBody{}
	done := make(chan struct{})
	var events <-chan Event
	var err error
	go func() {
		defer close(done)
		events, err = panicClient(t, body, true).Generate(context.Background(), panicRequest())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Generate 没有返回")
	}
	if events != nil || err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("events=%v err=%v，期望 panic 转成的错误", events, err)
	}
	if !body.closed.Load() {
		t.Fatal("panic 恢复后应关闭上游响应体")
	}
}

// TestToolContractForwardPanic 核对工具调用契约时 panic：以错误事件结束本请求、关闭事件流，并在后台读完上游，进程不退出
func TestToolContractForwardPanic(t *testing.T) {
	// 手工拼出的残缺 Schema（属性的子 Schema 为空）在校验时 panic：真实请求里 Schema 都经过编译，这里只用来让核对这一步出错
	broken := &jsonschema.Schema{Properties: map[string]*jsonschema.Schema{"q": nil}}
	contract := &toolContract{names: map[string]bool{"lookup": true}, schemas: map[string]*jsonschema.Schema{"lookup": broken}}
	source := make(chan Event, 3)
	source <- Event{Kind: EventText, Text: "先说一句"}
	source <- Event{Kind: EventToolCall, ToolCall: &FunctionCall{ID: "c1", Name: "lookup", Arguments: []byte(`{"q":1}`)}}
	source <- Event{Kind: EventFinish, FinishReason: "stop"}
	close(source)
	var kinds []EventKind
	var failure error
	for event := range contract.forward(context.Background(), source) {
		kinds = append(kinds, event.Kind)
		if event.Kind == EventError {
			failure = event.Err
		}
	}
	if len(kinds) != 2 || kinds[0] != EventText || kinds[1] != EventError || failure == nil || !strings.Contains(failure.Error(), "核对工具调用契约时发生 panic") {
		t.Fatalf("事件 = %v err=%v，期望正文之后是 panic 转成的错误事件", kinds, failure)
	}
	var contractErr *ToolContractError
	if errors.As(failure, &contractErr) {
		t.Fatal("panic 不应报成契约违规")
	}
}
