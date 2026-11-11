package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// memoryStatePersister 是按调用顺序立即生效的状态库：saved 同 ID 替换内容、保留原来的位置，removed 删除
type memoryStatePersister struct {
	mu     sync.Mutex
	ids    []string
	data   map[string][]byte
	logs   []string
	failed error
	// written 为收到过写入的节点 ID（按收到的顺序，含之后又删除的）
	written []string
}

func newMemoryStatePersister() *memoryStatePersister {
	return &memoryStatePersister{data: make(map[string][]byte)}
}

func (persister *memoryStatePersister) LoadResponseStates() ([]SavedResponseState, error) {
	persister.mu.Lock()
	defer persister.mu.Unlock()
	if persister.failed != nil {
		return nil, persister.failed
	}
	states := make([]SavedResponseState, 0, len(persister.ids))
	for _, id := range persister.ids {
		states = append(states, SavedResponseState{ID: id, Data: persister.data[id]})
	}
	return states, nil
}

func (persister *memoryStatePersister) SaveResponseStates(saved []SavedResponseState, removed []string) {
	persister.mu.Lock()
	defer persister.mu.Unlock()
	for _, state := range saved {
		persister.written = append(persister.written, state.ID)
		if _, exists := persister.data[state.ID]; !exists {
			persister.ids = append(persister.ids, state.ID)
		}
		persister.data[state.ID] = append([]byte(nil), state.Data...)
	}
	for _, id := range removed {
		if _, exists := persister.data[id]; !exists {
			continue
		}
		delete(persister.data, id)
		for index, value := range persister.ids {
			if value == id {
				persister.ids = append(persister.ids[:index:index], persister.ids[index+1:]...)
				break
			}
		}
	}
}

func (persister *memoryStatePersister) LogResponseStates(level string, message string) {
	persister.mu.Lock()
	defer persister.mu.Unlock()
	persister.logs = append(persister.logs, level+" "+message)
}

// postResponse 发一个非流式 Responses 请求并返回响应 ID
func postResponse(t *testing.T, handler http.Handler, body string) string {
	t.Helper()
	recorder := postJSON(t, handler, "/v1/responses", body, nil)
	var response struct {
		ID string `json:"id"`
	}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil || response.ID == "" {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	return response.ID
}

// TestResponseStatesSurviveRestart 续接状态写进状态库后，重新创建的处理器（服务重启）仍能按 previous_response_id 与
// previous_interaction_id 续接，带上重启前的对话与 input 里的系统指令；不带状态库时（原先的行为）重启后返回 not found
func TestResponseStatesSurviveRestart(t *testing.T) {
	persister := newMemoryStatePersister()
	service := &recordingService{}
	first := NewHandler(service, Config{APIKey: "sk-test", ResponseStates: persister})
	one := postResponse(t, first, `{"model":"gemini-2.5-flash","input":[{"role":"developer","content":"be brief"},{"role":"user","content":"hi"}]}`)
	two := postResponse(t, first, fmt.Sprintf(`{"model":"gemini-2.5-flash","input":"again","previous_response_id":%q}`, one))
	interaction := decodeInteraction(t, postJSON(t, first, "/v1/interactions", `{"model":"gemini-2.5-flash","input":"ping"}`, nil))["id"].(string)

	restarted := NewHandler(service, Config{APIKey: "sk-test", ResponseStates: persister})
	postResponse(t, restarted, fmt.Sprintf(`{"model":"gemini-2.5-flash","input":"third","previous_response_id":%q}`, two))
	if got := strings.Join(describeTurns(t, service.last.Contents), " / "); got != "user text:hi / assistant text:ok / user text:again / assistant text:ok / user text:third" {
		t.Fatalf("重启后续接的对话 = %s", got)
	}
	if service.last.System != "be brief" {
		t.Fatalf("重启后续接应带上 input 里的系统指令: %q", service.last.System)
	}
	decodeInteraction(t, postJSON(t, restarted, "/v1/interactions", fmt.Sprintf(`{"model":"gemini-2.5-flash","input":"pong","previous_interaction_id":%q}`, interaction), nil))
	if got := strings.Join(describeTurns(t, service.last.Contents), " / "); got != "user text:ping / assistant text:ok / user text:pong" {
		t.Fatalf("重启后续接的交互 = %s", got)
	}
	if !strings.Contains(strings.Join(persister.logs, "\n"), "已读回 Responses 与 Interactions 的续接状态（状态库以明文保存这些节点的对话内容）| 节点=3") {
		t.Fatalf("读回时应记一行节点数: %q", persister.logs)
	}

	memoryOnly := NewHandler(service, Config{APIKey: "sk-test"})
	lost := postJSON(t, memoryOnly, "/v1/responses", fmt.Sprintf(`{"model":"gemini-2.5-flash","input":"x","previous_response_id":%q}`, two), nil)
	if lost.Code != http.StatusBadRequest || !strings.Contains(lost.Body.String(), "was not found") {
		t.Fatalf("没有状态库时重启后应返回 not found: status=%d body=%s", lost.Code, lost.Body.String())
	}
}

// TestResponseStateEncodingRoundTrip 写进状态库的节点读回后与内存里的逐字段相同：包括 JSON 里不序列化的 FromSystem、FromToolResult、
// RestoredLink、RestoredText，内联数据、函数调用参数与思考签名；存入前去掉的 SourceGIF 不写进状态库
func TestResponseStateEncodingRoundTrip(t *testing.T) {
	persister := newMemoryStatePersister()
	store := newResponseStateStore()
	store.restore(persister)
	contents := []aistudio.Content{
		{Role: aistudio.RoleUser, FromSystem: true, Parts: []aistudio.Part{{Text: "<system-reminder>\nrule\n</system-reminder>"}}},
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{Text: "look"},
			{InlineData: &aistudio.Blob{MIME: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}, SourceGIF: []byte("GIF89a")}},
			{InlineData: &aistudio.Blob{MIME: "image/png", Data: []byte{1, 2}}, RestoredLink: true, RestoredText: "https://host/media/a.png"},
		}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
			{ThoughtSignature: "sig-only"},
			{FunctionCall: &aistudio.FunctionCall{ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"path": "a.txt"}`), ThoughtSignature: "sig-call"}},
		}},
		{Role: aistudio.RoleTool, Parts: []aistudio.Part{
			{FunctionResult: &aistudio.FunctionResult{ID: "call_1", Name: "read", Content: json.RawMessage(`{"ok":true}`)}},
			{InlineData: &aistudio.Blob{MIME: "image/jpeg", Data: []byte{0xff, 0xd8}}, FromToolResult: true},
		}},
	}
	store.Store("resp_1", responseState{Contents: contents, InlineInstructions: []string{"be brief"}})
	restored := newResponseStateStore()
	restored.restore(persister)
	gotContents, gotInstructions, ok := restored.Load("resp_1")
	wantContents, wantInstructions, _ := store.Load("resp_1")
	if !ok || !reflect.DeepEqual(gotContents, wantContents) || !reflect.DeepEqual(gotInstructions, wantInstructions) {
		t.Fatalf("读回的节点与内存不同:\n%+v\n%+v", gotContents, wantContents)
	}
	if gotContents[1].Parts[1].InlineData.SourceGIF != nil || !gotContents[0].FromSystem || !gotContents[3].Parts[1].FromToolResult ||
		!gotContents[1].Parts[2].RestoredLink || gotContents[1].Parts[2].RestoredText == "" {
		t.Fatalf("标记或 SourceGIF 不对: %+v", gotContents)
	}
}

// TestResponseStatePersistEviction 超过 256 个节点时照内存规则淘汰最早的节点并把上下文并入子节点：状态库同步删除淘汰的节点、
// 写入并入后的子节点；重启读回后续接得到的上下文与重启前相同
func TestResponseStatePersistEviction(t *testing.T) {
	persister := newMemoryStatePersister()
	store := newResponseStateStore()
	store.restore(persister)
	parent := ""
	for index := range responseStateCapacity + 2 {
		id := fmt.Sprintf("resp_%03d", index)
		store.Store(id, responseState{ParentID: parent, Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: id}}}}})
		parent = id
	}
	if len(persister.ids) != responseStateCapacity || persister.ids[0] != "resp_002" {
		t.Fatalf("状态库应只留最近 256 个节点: %d 个，最早 %s", len(persister.ids), persister.ids[0])
	}
	before, _, ok := store.Load(parent)
	if !ok || len(before) != responseStateCapacity+2 {
		t.Fatalf("内存续接的上下文 = %d 轮", len(before))
	}
	restored := newResponseStateStore()
	restored.restore(persister)
	after, _, ok := restored.Load(parent)
	if !ok || !reflect.DeepEqual(after, before) {
		t.Fatalf("重启后续接的上下文与重启前不同: %d 轮 / %d 轮", len(after), len(before))
	}
}

// smallContents 返回只有一段文本的用户轮
func smallContents(text string) []aistudio.Content {
	return []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: text}}}}
}

// mediaContents 返回带 size 字节内联视频的用户轮
func mediaContents(size int) []aistudio.Content {
	return []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{InlineData: &aistudio.Blob{MIME: "video/mp4", Data: make([]byte, size)}}}}}
}

// countLogs 返回包含 text 的日志行数
func (persister *memoryStatePersister) countLogs(text string) int {
	persister.mu.Lock()
	defer persister.mu.Unlock()
	count := 0
	for _, line := range persister.logs {
		if strings.Contains(line, text) {
			count++
		}
	}
	return count
}

// TestResponseStatePersistLimit 超过 4 MiB 的节点不写进状态库（只留在内存）：粗估已超限的不编码，记一行 INFO；以它为续接起点保存的节点
// 继承“不入库”、也不写（重启后反正接不上），不再记日志
func TestResponseStatePersistLimit(t *testing.T) {
	persister := newMemoryStatePersister()
	store := newResponseStateStore()
	store.restore(persister)
	store.Store("resp_root", responseState{Contents: smallContents("root")})
	store.Store("resp_big", responseState{ParentID: "resp_root", Contents: mediaContents(responseStatePersistLimit + 1)})
	store.Store("resp_child", responseState{ParentID: "resp_big", Contents: smallContents("child")})
	store.Store("resp_grandchild", responseState{ParentID: "resp_child", Contents: smallContents("grandchild")})
	if _, _, ok := store.Load("resp_grandchild"); !ok {
		t.Fatal("内存里照常能续接")
	}
	if got := strings.Join(persister.ids, ","); got != "resp_root" {
		t.Fatalf("大节点与它的后代都不应写入: %s", got)
	}
	if persister.countLogs("INFO 续接状态节点内容至少") != 1 || persister.countLogs("不写入状态库") != 1 {
		t.Fatalf("一条链只记一行 INFO（粗估超限、不编码）: %q", persister.logs)
	}
}

// TestResponseStatePersistMergedOverLimit 淘汰时并入上文后超过 4 MiB 的子节点不再写库：它与它当时的后代一起从状态库删除（重启后都接不上），
// 只记一行；同一次改动里先处理到的后代（触发淘汰的新节点）也不写入；之后以它们为续接起点的新节点与再次并入上文的子节点不编码、
// 不写库、不再记日志，淘汰时去掉标记；内存里照常续接
func TestResponseStatePersistMergedOverLimit(t *testing.T) {
	persister := newMemoryStatePersister()
	store := newResponseStateStore()
	store.restore(persister)
	store.Store("resp_a", responseState{Contents: mediaContents(3 << 20)})
	store.Store("resp_b", responseState{ParentID: "resp_a", Contents: mediaContents(2 << 20)})
	store.Store("resp_c", responseState{ParentID: "resp_b", Contents: smallContents("c")})
	for index := range responseStateCapacity - 3 {
		store.Store(fmt.Sprintf("resp_fill_%03d", index), responseState{Contents: smallContents("fill")})
	}
	if _, exists := persister.data["resp_c"]; !exists || len(persister.ids) != responseStateCapacity {
		t.Fatalf("淘汰之前三个节点都应在状态库: %d 个", len(persister.ids))
	}
	// 第 257 个节点以 resp_c 为续接起点：淘汰 resp_a，resp_b 并入上文后超限，连同 resp_c 与这个新节点一起不入库
	store.Store("resp_d", responseState{ParentID: "resp_c", Contents: smallContents("d")})
	for _, id := range []string{"resp_a", "resp_b", "resp_c", "resp_d"} {
		if _, exists := persister.data[id]; exists {
			t.Fatalf("%s 应已从状态库删除: %v", id, persister.ids[:3])
		}
	}
	if slices.Contains(persister.written, "resp_d") {
		t.Fatalf("同一次改动里随续接起点不入库的新节点不应写入: %v", persister.written[len(persister.written)-3:])
	}
	if persister.countLogs("续接状态节点内容至少") != 1 || persister.countLogs("一并从状态库删除以它为续接起点的节点=2") != 1 {
		t.Fatalf("并入上文后超限应记一行并带上删除的后代数: %q", persister.logs)
	}
	store.Store("resp_e", responseState{ParentID: "resp_d", Contents: smallContents("e")})
	store.Store("resp_fill_more", responseState{Contents: smallContents("fill")})
	if slices.Contains(persister.written, "resp_e") {
		t.Fatal("续接起点不入库的新节点不应写入")
	}
	if persister.countLogs("不写入状态库") != 1 {
		t.Fatalf("同一条链不应重复记日志: %q", persister.logs)
	}
	contents, _, ok := store.Load("resp_e")
	if !ok || len(contents) != 5 {
		t.Fatalf("内存里照常续接: ok=%v 轮数=%d", ok, len(contents))
	}
	// resp_b、resp_c 已淘汰（上文并入 resp_d），“不入库”标记随淘汰去掉，只剩 resp_d、resp_e
	if len(store.unpersisted) != 2 || !store.unpersisted["resp_d"] || !store.unpersisted["resp_e"] {
		t.Fatalf("不入库标记应随淘汰去掉: %v", store.unpersisted)
	}
}

// droppingStatePersister 丢弃第一次写入 dropID 的那次改动（相当于账本排队已满），其余照 memoryStatePersister
type droppingStatePersister struct {
	*memoryStatePersister
	dropID  string
	dropped bool
}

func (persister *droppingStatePersister) SaveResponseStates(saved []SavedResponseState, removed []string) {
	for _, state := range saved {
		if state.ID == persister.dropID && !persister.dropped {
			persister.dropped = true
			return
		}
	}
	persister.memoryStatePersister.SaveResponseStates(saved, removed)
}

// TestResponseStateRestoreParentSavedLater 续接起点的首次写入被丢弃、之后在淘汰时作为改动过的子节点重新写入（排在它的子节点之后）：
// 读回时按整条续接链判定，子节点不被误判为孤儿，重启后照常续接
func TestResponseStateRestoreParentSavedLater(t *testing.T) {
	persister := &droppingStatePersister{memoryStatePersister: newMemoryStatePersister(), dropID: "resp_p"}
	store := newResponseStateStore()
	store.restore(persister)
	store.Store("resp_g", responseState{Contents: smallContents("g")})
	store.Store("resp_p", responseState{ParentID: "resp_g", Contents: smallContents("p")})
	store.Store("resp_c", responseState{ParentID: "resp_p", Contents: smallContents("c")})
	for index := range responseStateCapacity - 2 {
		store.Store(fmt.Sprintf("resp_fill_%03d", index), responseState{Contents: smallContents("fill")})
	}
	if !persister.dropped || persister.ids[0] != "resp_c" || persister.ids[len(persister.ids)-1] != "resp_p" {
		t.Fatalf("前提：resp_p 重新写入后排在 resp_c 之后: %v … %v", persister.ids[:1], persister.ids[len(persister.ids)-1:])
	}
	before, _, ok := store.Load("resp_c")
	if !ok {
		t.Fatal("内存里应能从 resp_c 续接")
	}
	restored := newResponseStateStore()
	restored.restore(persister)
	after, _, ok := restored.Load("resp_c")
	if !ok || !reflect.DeepEqual(after, before) {
		t.Fatalf("重启后应能从 resp_c 续接、上下文与重启前相同: ok=%v %d 轮 / %d 轮 | %q", ok, len(after), len(before), persister.logs)
	}
	if persister.countLogs("丢弃续接起点已不在的节点") != 0 {
		t.Fatalf("不应丢弃节点: %q", persister.logs)
	}
}

// TestResponseStateRestoreOrphans 读回时续接链上有节点不在库里（或成环）的节点与读不出的节点丢弃并从状态库删除；读回失败时从空状态开始
func TestResponseStateRestoreOrphans(t *testing.T) {
	persister := newMemoryStatePersister()
	encode := func(state responseState) []byte {
		t.Helper()
		data, err := encodeResponseState(state)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	persister.SaveResponseStates([]SavedResponseState{
		{ID: "resp_root", Data: encode(responseState{Contents: smallContents("root")})},
		{ID: "resp_orphan", Data: encode(responseState{ParentID: "resp_missing", Contents: smallContents("orphan")})},
		{ID: "resp_orphan_child", Data: encode(responseState{ParentID: "resp_orphan", Contents: smallContents("orphan child")})},
		{ID: "resp_loop_a", Data: encode(responseState{ParentID: "resp_loop_b", Contents: smallContents("a")})},
		{ID: "resp_loop_b", Data: encode(responseState{ParentID: "resp_loop_a", Contents: smallContents("b")})},
		{ID: "resp_broken", Data: []byte("not a state")},
		{ID: "resp_leaf", Data: encode(responseState{ParentID: "resp_root", Contents: smallContents("leaf")})},
	}, nil)
	restored := newResponseStateStore()
	restored.restore(persister)
	if _, _, ok := restored.Load("resp_leaf"); !ok {
		t.Fatal("重启后应能从 resp_leaf 续接")
	}
	if got := strings.Join(persister.ids, ","); got != "resp_root,resp_leaf" {
		t.Fatalf("丢弃的节点应从状态库删除: %s", got)
	}
	if persister.countLogs("丢弃读不出的节点=1 | 丢弃续接起点已不在的节点=4") != 1 {
		t.Fatalf("读回日志: %q", persister.logs)
	}

	persister.failed = fmt.Errorf("disk error")
	empty := newResponseStateStore()
	empty.restore(persister)
	if len(empty.states) != 0 || !strings.Contains(persister.logs[len(persister.logs)-1], "读取续接状态失败") {
		t.Fatalf("读回失败时应从空状态开始并记 WARN: %q", persister.logs)
	}
}

// TestEstimatedResponseStateSizeIsLowerBound 粗估不大于编码后的实际大小（粗估超限时不编码，必须不会把能写入的节点误判为超限）
func TestEstimatedResponseStateSizeIsLowerBound(t *testing.T) {
	state := responseState{ParentID: "resp_parent", InlineInstructions: []string{"be brief"}, Contents: []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "look"}, {InlineData: &aistudio.Blob{MIME: "image/png", Data: make([]byte, 1000)}}}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
			{Text: "thinking", Thought: true, ThoughtSignature: "sig"},
			{FunctionCall: &aistudio.FunctionCall{ID: "call_1", Name: "run", Arguments: json.RawMessage(`{"cmd":"ls"}`), ThoughtSignature: "sig-call"}},
			{ExecutableCode: &aistudio.ExecutableCode{Language: "python", Code: "print(1)"}},
			{CodeExecutionResult: &aistudio.CodeExecutionResult{Outcome: "ok", Output: "1", Error: "none"}},
		}},
		{Role: aistudio.RoleTool, Parts: []aistudio.Part{{FunctionResult: &aistudio.FunctionResult{ID: "call_1", Name: "run", Content: json.RawMessage(`{"out":"a.txt"}`)}}}},
	}}
	data, err := encodeResponseState(state)
	if err != nil {
		t.Fatal(err)
	}
	if estimate := estimatedResponseStateSize(state); estimate > len(data) || estimate < 1000 {
		t.Fatalf("粗估 %d 字节，编码后 %d 字节", estimate, len(data))
	}
}

// pacedRecordingService 记下收到的每个生成请求：第一个请求按 paced 的间隔送出事件，之后的请求立即回复 ok
type pacedRecordingService struct {
	pacedService
	mu       sync.Mutex
	requests []aistudio.GenerateRequest
}

func (service *pacedRecordingService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.mu.Lock()
	index := len(service.requests)
	service.requests = append(service.requests, request)
	service.mu.Unlock()
	if index == 0 {
		return service.pacedService.Generate(ctx, request)
	}
	return (&recordingService{}).Generate(ctx, request)
}

func (service *pacedRecordingService) request(t *testing.T, index int) aistudio.GenerateRequest {
	t.Helper()
	service.mu.Lock()
	defer service.mu.Unlock()
	if index >= len(service.requests) {
		t.Fatalf("生成服务只收到 %d 个请求，期望第 %d 个", len(service.requests), index+1)
	}
	return service.requests[index]
}

// TestResponseStatesAgentMessageSurviveRestart Codex 多智能体会话的 agent_message 输入项与超过 63 字节的函数名经续接状态入库、
// 重启读回后不变：流式请求在静默期间改发 response.in_progress，结束后保存的节点带 agent_message 转成的 user 轮（全部为空的那一轮
// 没有 Part）与原名的函数调用及其签名；重启后续接与不重启续接发往生成服务的对话逐字段相同，发往上游时函数名照旧截短
// （状态库里存原名，截短只在编码层）
func TestResponseStatesAgentMessageSurviveRestart(t *testing.T) {
	withHeartbeatInterval(t, 20*time.Millisecond)
	responsesProgressAfterOverride.Store(int64(60 * time.Millisecond))
	t.Cleanup(func() { responsesProgressAfterOverride.Store(0) })
	longName := strings.Repeat("codex_apps_document_", 4) + "execute"
	if len(longName) <= 63 {
		t.Fatalf("测试用的函数名应超过 63 字节: %d", len(longName))
	}
	call := aistudio.Event{Kind: aistudio.EventToolCall, ThoughtSignature: "sig-call", ToolCall: &aistudio.FunctionCall{
		ID: "call_1", Name: longName, Arguments: json.RawMessage(`{}`), ThoughtSignature: "sig-call",
	}}
	service := &pacedRecordingService{pacedService: pacedService{events: []pacedEvent{
		{event: aistudio.Event{Kind: aistudio.EventText, Text: "working"}},
		{delay: 250 * time.Millisecond, event: call},
		{event: aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}},
	}}}
	persister := newMemoryStatePersister()
	first := NewHandler(service, Config{APIKey: "sk-test", ResponseStates: persister})
	body := `{"model":"gemini-2.5-flash","stream":true,` +
		`"tools":[{"type":"function","name":"` + longName + `","parameters":{"type":"object"}}],"input":[` +
		`{"type":"message","role":"user","content":"plan the task"},` +
		`{"type":"agent_message","content":[{"type":"input_text","text":"worker report"},{"type":"encrypted_content","encrypted_content":"model wrote this"}]},` +
		`{"type":"agent_message","content":[{"type":"input_text","text":""}]}]}`
	recorder := postJSON(t, first, "/v1/responses", body, nil)
	names, _, _ := responsesStreamEvents(t, recorder.Body.String())
	if countName(names, "response.in_progress") < 2 || names[len(names)-1] != "response.completed" {
		t.Fatalf("静默期间应改发 response.in_progress、流以 response.completed 结束: %v", names)
	}
	responseID := ""
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		var payload struct {
			Type     string `json:"type"`
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if strings.HasPrefix(line, "data: ") && json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload) == nil && payload.Type == "response.created" {
			responseID = payload.Response.ID
		}
	}
	if responseID == "" {
		t.Fatalf("没有 response.created: %s", recorder.Body.String())
	}

	persister.mu.Lock()
	saved, ok := persister.data[responseID]
	persister.mu.Unlock()
	if !ok {
		t.Fatalf("流式响应的节点应写进状态库: %v", persister.ids)
	}
	state, err := decodeResponseState(saved)
	if err != nil {
		t.Fatal(err)
	}
	want := []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "plan the task"}}},
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "worker report"}, {Text: "model wrote this"}}},
		{Role: aistudio.RoleUser},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
			{Text: "working"},
			{FunctionCall: &aistudio.FunctionCall{ID: "call_1", Name: longName, Arguments: json.RawMessage(`{}`), ThoughtSignature: "sig-call"}, ThoughtSignature: "sig-call"},
		}},
	}
	if !reflect.DeepEqual(state.Contents, want) {
		t.Fatalf("状态库里的节点:\n%+v\n期望:\n%+v", state.Contents, want)
	}

	continuation := fmt.Sprintf(`{"model":"gemini-2.5-flash","previous_response_id":%q,`+
		`"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`, responseID)
	postResponse(t, first, continuation)
	restarted := NewHandler(service, Config{APIKey: "sk-test", ResponseStates: persister})
	postResponse(t, restarted, continuation)
	inMemory, afterRestart := service.request(t, 1), service.request(t, 2)
	if !reflect.DeepEqual(afterRestart.Contents, inMemory.Contents) {
		t.Fatalf("重启后续接的对话与不重启时不同:\n%+v\n%+v", afterRestart.Contents, inMemory.Contents)
	}
	agentTurn := false
	for _, content := range afterRestart.Contents {
		if content.Role == aistudio.RoleUser && reflect.DeepEqual(describeParts(content.Parts), []string{"text:worker report", "text:model wrote this"}) {
			agentTurn = true
		}
	}
	if !agentTurn {
		t.Fatalf("重启后续接应带上 agent_message 的 user 轮: %v", describeTurns(t, afterRestart.Contents))
	}
	encoded, err := aistudio.EncodeGenerateContentRequest(aistudio.GenerateRequest{Model: "gemini-2.5-flash", Contents: afterRestart.Contents},
		aistudio.GenerationDefaults{MaxOutputTokens: 1024}, aistudio.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), longName) || !strings.Contains(string(encoded), `"`+longName[:50]+`_`) {
		t.Fatalf("读回的历史调用发往上游时应为截短名: %s", encoded)
	}
}
