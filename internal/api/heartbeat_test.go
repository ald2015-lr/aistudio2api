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
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// withoutGeminiHeartbeats 去掉数据型心跳块（唯一的 part 为空文本或空思考，没有签名、finishReason 与 usageMetadata），只留内容块
func withoutGeminiHeartbeats(t *testing.T, chunks []map[string]any) []map[string]any {
	t.Helper()
	kept := make([]map[string]any, 0, len(chunks))
	for _, chunk := range chunks {
		if !geminiHeartbeatChunk(chunk) {
			kept = append(kept, chunk)
		}
	}
	return kept
}

func geminiHeartbeatChunk(chunk map[string]any) bool {
	if _, ok := chunk["usageMetadata"]; ok {
		return false
	}
	candidates, _ := chunk["candidates"].([]any)
	if len(candidates) != 1 {
		return false
	}
	candidate, _ := candidates[0].(map[string]any)
	if _, ok := candidate["finishReason"]; ok {
		return false
	}
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 1 {
		return false
	}
	part, _ := parts[0].(map[string]any)
	if text, ok := part["text"].(string); !ok || text != "" {
		return false
	}
	_, signed := part["thoughtSignature"]
	return !signed && (len(part) == 1 || len(part) == 2 && part["thought"] == true)
}

// withHeartbeatInterval 在测试期间把流式心跳间隔缩短为 interval
func withHeartbeatInterval(t *testing.T, interval time.Duration) {
	t.Helper()
	heartbeatIntervalOverride.Store(int64(interval))
	t.Cleanup(func() { heartbeatIntervalOverride.Store(0) })
}

// withStreamStartHold 在测试期间把 server.generate 扣住流式响应头的上限缩短为 limit（写出响应头前的等待随之不超过它）
func withStreamStartHold(t *testing.T, limit time.Duration) {
	t.Helper()
	streamStartHoldMaxOverride.Store(int64(limit))
	t.Cleanup(func() { streamStartHoldMaxOverride.Store(0) })
}

// TestGeminiDataHeartbeatBytes 数据型心跳块的确切字节：首个事件还没到就提交响应头时立刻写一块空文本候选块，responseId、modelVersion
// 与本流其他块一致，不带 finishReason 与 usageMetadata；首个事件已到时提交响应头不写这一块；上一块是思考（含独立签名的空思考 part）时
// 心跳用空思考 part，搜索来源、引用块不改变，正文、函数调用之后回到空文本
func TestGeminiDataHeartbeatBytes(t *testing.T) {
	const textBeat = `data: {"candidates":[{"content":{"parts":[{"text":""}],"role":"model"},"index":0}],"modelVersion":"gemini-3.8-flash","responseId":"resp_1"}` + "\n\n"
	const thoughtBeat = `data: {"candidates":[{"content":{"parts":[{"text":"","thought":true}],"role":"model"},"index":0}],"modelVersion":"gemini-3.8-flash","responseId":"resp_1"}` + "\n\n"
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse", nil)
	ready := httptest.NewRecorder()
	if _, err := newGeminiChunkWriter(ready, request, "resp_1", "gemini-3.8-flash", true, true); err != nil {
		t.Fatal(err)
	}
	if got := ready.Body.String(); got != "" || ready.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("首个事件已到时提交响应头不应写心跳块: %q", got)
	}
	writer, err := newGeminiChunkWriter(recorder, request, "resp_1", "gemini-3.8-flash", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.Body.String(); got != textBeat {
		t.Fatalf("响应头提交时的心跳块 = %q，期望 %q", got, textBeat)
	}
	if recorder.Header().Get("Content-Type") != "text/event-stream" || recorder.Code != http.StatusOK {
		t.Fatalf("响应头 = %d %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	beat := func() string {
		t.Helper()
		recorder.Body.Reset()
		if err := writer.heartbeat(); err != nil {
			t.Fatal(err)
		}
		return recorder.Body.String()
	}
	steps := []struct {
		kind aistudio.EventKind
		want string
	}{
		{aistudio.EventReasoning, thoughtBeat},
		{aistudio.EventGrounding, thoughtBeat},
		{aistudio.EventCitation, thoughtBeat},
		{aistudio.EventText, textBeat},
		{aistudio.EventThoughtSignature, thoughtBeat},
		{aistudio.EventToolCall, textBeat},
		{aistudio.EventMedia, textBeat},
	}
	for _, step := range steps {
		writer.observe(step.kind)
		if got := beat(); got != step.want {
			t.Fatalf("上一块为 %s 时心跳 = %q，期望 %q", step.kind, got, step.want)
		}
	}
}

// TestGeminiHeartbeatDisabledUnchanged 关闭数据型心跳时与原先逐字节一致：响应头提交时不写任何数据，心跳只写一个换行；
// JSON 数组模式不论开关都只写换行
func TestGeminiHeartbeatDisabledUnchanged(t *testing.T) {
	for _, test := range []struct {
		name     string
		query    string
		data     bool
		start    string
		heartbit string
	}{
		{name: "SSE 关闭", query: "?alt=sse", data: false, start: "", heartbit: "\n"},
		{name: "JSON 数组开启", query: "", data: true, start: "[", heartbit: "\n"},
		{name: "JSON 数组关闭", query: "", data: false, start: "[", heartbit: "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:streamGenerateContent"+test.query, nil)
			writer, err := newGeminiChunkWriter(recorder, request, "resp_1", "m", test.data, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := recorder.Body.String(); got != test.start {
				t.Fatalf("响应头提交后 = %q，期望 %q", got, test.start)
			}
			writer.observe(aistudio.EventReasoning)
			recorder.Body.Reset()
			if err := writer.heartbeat(); err != nil {
				t.Fatal(err)
			}
			if got := recorder.Body.String(); got != test.heartbit {
				t.Fatalf("心跳 = %q，期望 %q", got, test.heartbit)
			}
		})
	}
}

// pacedEventsService 按 steps 依次发出事件，每一步之前先等 delay；ctx 结束时停止
type pacedEventsService struct {
	modelListService
	steps []pacedStep
}

type pacedStep struct {
	delay time.Duration
	event aistudio.Event
}

func (service *pacedEventsService) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events := make(chan aistudio.Event)
	go func() {
		defer close(events)
		for _, step := range service.steps {
			select {
			case <-time.After(step.delay):
			case <-ctx.Done():
				return
			}
			select {
			case events <- step.event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, nil
}

// geminiStreamKinds 把 Gemini SSE 的块归类：hb-text、hb-thought（心跳）、thought、text、finish
func geminiStreamKinds(t *testing.T, body string) []string {
	t.Helper()
	var kinds []string
	for _, chunk := range sseData(t, body) {
		part := geminiChunkPart(chunk)
		candidates, _ := chunk["candidates"].([]any)
		candidate, _ := candidates[0].(map[string]any)
		switch {
		case candidate["finishReason"] != nil:
			kinds = append(kinds, "finish")
		case geminiHeartbeatChunk(chunk) && part["thought"] == true:
			kinds = append(kinds, "hb-thought")
		case geminiHeartbeatChunk(chunk):
			kinds = append(kinds, "hb-text")
		case part["thought"] == true:
			kinds = append(kinds, "thought")
		default:
			kinds = append(kinds, "text")
		}
	}
	return kinds
}

// collapseRuns 把连续相同的项合并成一项（心跳次数受调度影响，只看顺序）
func collapseRuns(items []string) []string {
	var collapsed []string
	for _, item := range items {
		if len(collapsed) == 0 || collapsed[len(collapsed)-1] != item {
			collapsed = append(collapsed, item)
		}
	}
	return collapsed
}

// TestGeminiStreamDataHeartbeatDuringSilence 完整流程：首个事件很快到达时流的开头没有心跳块；思考期间静默时心跳为空思考块，
// 正文开始后静默时为空文本块；首个事件晚于响应头等待时提交响应头立刻有一块空文本心跳；关闭时流里没有任何心跳块（只有换行）
func TestGeminiStreamDataHeartbeatDuringSilence(t *testing.T) {
	withHeartbeatInterval(t, 30*time.Millisecond)
	steps := []pacedStep{
		{delay: 0, event: aistudio.Event{Kind: aistudio.EventReasoning, Text: "想一想"}},
		{delay: 120 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventText, Text: "答案"}},
		{delay: 120 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}},
	}
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	path := "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse"

	handler := NewHandler(&pacedEventsService{steps: steps}, Config{APIKey: "sk-test"})
	recorder := postJSON(t, handler, path, body, nil)
	got := collapseRuns(geminiStreamKinds(t, recorder.Body.String()))
	want := []string{"thought", "hb-thought", "text", "hb-text", "finish"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("块顺序 = %v，期望 %v\n%s", got, want, recorder.Body.String())
	}
	// 首个事件晚于响应头等待（这里把扣住响应头的上限缩短到 20ms，首个事件 200ms 后才到）：提交响应头时先写一块空文本心跳
	withStreamStartHold(t, 20*time.Millisecond)
	late := append([]pacedStep{{delay: 200 * time.Millisecond, event: steps[0].event}}, steps[1:]...)
	lateRecorder := postJSON(t, NewHandler(&pacedEventsService{steps: late}, Config{APIKey: "sk-test"}), path, body, nil)
	if got := collapseRuns(geminiStreamKinds(t, lateRecorder.Body.String())); strings.Join(got, ",") != "hb-text,thought,hb-thought,text,hb-text,finish" {
		t.Fatalf("首个事件晚到时块顺序 = %v\n%s", got, lateRecorder.Body.String())
	}
	streamStartHoldMaxOverride.Store(0)
	// 心跳块与本流其他块的 responseId、modelVersion 一致
	chunks := sseData(t, recorder.Body.String())
	for _, chunk := range chunks {
		if chunk["responseId"] != chunks[len(chunks)-1]["responseId"] || chunk["modelVersion"] != "gemini-3.8-flash" {
			t.Fatalf("块的 responseId/modelVersion 不一致: %v", chunk)
		}
	}

	off := NewHandler(&pacedEventsService{steps: steps}, Config{APIKey: "sk-test", StreamDataHeartbeat: func() bool { return false }})
	recorder = postJSON(t, off, path, body, nil)
	got = geminiStreamKinds(t, recorder.Body.String())
	if strings.Join(got, ",") != "thought,text,finish" {
		t.Fatalf("关闭时块顺序 = %v，期望只有内容块\n%q", got, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "\n\n\n") {
		t.Fatalf("关闭时静默期间应照旧写换行心跳: %q", recorder.Body.String())
	}
}

// TestOpenAIChatDataHeartbeat OpenAI Chat 流的心跳：开启时为与本流 id、model、created 一致的空 delta 块（finish_reason 为 null，
// include_usage 时带 usage:null）；关闭时为原先的 ": ping" 注释
func TestOpenAIChatDataHeartbeat(t *testing.T) {
	recorder := httptest.NewRecorder()
	if err := chatHeartbeat(recorder, true, "chatcmpl_1", 1700000000, "gemini-3.8-flash", false)(); err != nil {
		t.Fatal(err)
	}
	const want = `data: {"choices":[{"delta":{},"finish_reason":null,"index":0}],"created":1700000000,"id":"chatcmpl_1","model":"gemini-3.8-flash","object":"chat.completion.chunk"}` + "\n\n"
	if got := recorder.Body.String(); got != want {
		t.Fatalf("空 delta 心跳 = %q，期望 %q", got, want)
	}
	recorder = httptest.NewRecorder()
	if err := chatHeartbeat(recorder, true, "chatcmpl_1", 1700000000, "gemini-3.8-flash", true)(); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Body.String(); !strings.HasSuffix(got, `"object":"chat.completion.chunk","usage":null}`+"\n\n") {
		t.Fatalf("include_usage 时心跳 = %q，期望与其他块一样带 usage:null", got)
	}
	recorder = httptest.NewRecorder()
	if err := chatHeartbeat(recorder, false, "chatcmpl_1", 1700000000, "gemini-3.8-flash", false)(); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Body.String(); got != ": ping\n\n" {
		t.Fatalf("关闭时心跳 = %q，期望原先的 SSE 注释", got)
	}

	// 完整流程：静默期间写空 delta 块，id、model、created 与本流其他块一致，没有 ": ping"
	withHeartbeatInterval(t, 30*time.Millisecond)
	service := &pacedEventsService{steps: []pacedStep{
		{delay: 100 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventText, Text: "hi"}},
		{delay: 100 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}},
	}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	stream := postJSON(t, handler, "/v1/chat/completions", `{"model":"gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if strings.Contains(stream.Body.String(), ": ping") {
		t.Fatalf("开启数据型心跳时不应再写 SSE 注释: %s", stream.Body.String())
	}
	chunks := sseData(t, stream.Body.String())
	empty := 0
	for _, chunk := range chunks {
		if chunk["id"] != chunks[0]["id"] || chunk["created"] != chunks[0]["created"] || chunk["model"] != "gemini-3.8-flash" {
			t.Fatalf("块的 id/created/model 不一致: %v", chunk)
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) != 1 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if delta, _ := choice["delta"].(map[string]any); len(delta) == 0 && choice["finish_reason"] == nil {
			empty++
		}
	}
	if empty == 0 {
		t.Fatalf("静默期间应有空 delta 心跳块: %s", stream.Body.String())
	}
	off := NewHandler(service, Config{APIKey: "sk-test", StreamDataHeartbeat: func() bool { return false }})
	stream = postJSON(t, off, "/v1/chat/completions", `{"model":"gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if !strings.Contains(stream.Body.String(), ": ping\n\n") {
		t.Fatalf("关闭时静默期间应照旧写 \": ping\": %s", stream.Body.String())
	}
}

// ---------- new-api 的转换规则（按 /tmp/vidinv_newapi/src@e449cc7 手写的等价实现，只用于断言） ----------

// newAPIGeminiChunk 对应 relaykit/dto/gemini.go 的 GeminiChatResponse（只取转换用到的字段；没有 error 字段）
type newAPIGeminiChunk struct {
	Candidates []struct {
		Index   int64 `json:"index"`
		Content struct {
			Parts []struct {
				Text         string          `json:"text"`
				Thought      bool            `json:"thought"`
				FunctionCall json.RawMessage `json:"functionCall"`
				InlineData   json.RawMessage `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason *string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// newAPIDelta 为 new-api 写给它的客户端的 choices[0].delta（指针区分“没有该字段”与空串）
type newAPIDelta struct {
	Content          *string
	ReasoningContent *string
	ToolCalls        int
	FinishReason     *string
	HasUsage         bool
	Stop             bool
}

// newAPIForward 按 new-api 的规则处理上游的一行：
//   - relay/helper/stream_scanner.go:301-307：去掉 "data:" 前缀再 TrimSpace，不带前缀的行与空数据直接跳过（不写给客户端）；
//   - relay/channel/gemini/relay-gemini.go:200-206：按 GeminiChatResponse 解析，失败时整个流按 500 结束；
//   - relaykit/relayconvert/internal/gemini_chat/to_oai_chat_resp.go:186-277（StreamResponseGeminiChat2OpenAI）：finishReason 为 STOP 时
//     isStop 并清掉；有 finishReason 才设 choice.FinishReason；thought part 进 reasoning_content，其余文本（"\n" 除外）进 content，
//     两者都用 Set 写入（空串也写出字段）；有 functionCall 时 finish_reason 为 tool_calls；只有带 token 数的 usageMetadata 才附 usage；
//   - relay/channel/openai/relay-openai.go:23-29（sendStreamData）：只有整段数据为空串才不写，其余经 StringData 写出 "data: ..."。
//
// forwarded 为 false 表示这一行不会让 new-api 给它的客户端写任何字节
func newAPIForward(t *testing.T, line string) (delta newAPIDelta, forwarded bool) {
	t.Helper()
	data, hasPrefix := strings.CutPrefix(line, "data:")
	data = strings.TrimSpace(data)
	if !hasPrefix || data == "" {
		return newAPIDelta{}, false
	}
	var chunk newAPIGeminiChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		t.Fatalf("new-api 无法解析这一块，会按 500 bad_response_body 结束: %v %s", err, data)
	}
	if len(chunk.Candidates) != 1 {
		t.Fatalf("测试只处理单个候选: %s", data)
	}
	candidate := chunk.Candidates[0]
	if candidate.FinishReason != nil && *candidate.FinishReason == "STOP" {
		delta.Stop = true
	} else if candidate.FinishReason != nil {
		reason := "content_filter"
		delta.FinishReason = &reason
	}
	var content strings.Builder
	appended, thought := 0, false
	for _, part := range candidate.Content.Parts {
		switch {
		case len(part.FunctionCall) > 0:
			delta.ToolCalls++
		case part.Thought:
			thought = true
			if appended > 0 {
				content.WriteByte('\n')
			}
			appended++
			content.WriteString(part.Text)
		case part.Text != "\n":
			if appended > 0 {
				content.WriteByte('\n')
			}
			appended++
			content.WriteString(part.Text)
		}
	}
	text := content.String()
	if thought {
		delta.ReasoningContent = &text
	} else {
		delta.Content = &text
	}
	if delta.ToolCalls > 0 {
		reason := "tool_calls"
		delta.FinishReason = &reason
	}
	delta.HasUsage = chunk.UsageMetadata != nil && chunk.UsageMetadata.TotalTokenCount > 0
	return delta, true
}

// TestNewAPIForwardsDataHeartbeat 按 new-api 的转换规则：原先的换行心跳被扫描器丢弃，不会给 new-api 的客户端写任何字节；
// 空文本心跳块被转成 delta.content="" 的空增量（与 new-api 自己在流开头写的 role 块同形），空思考心跳块被转成 delta.reasoning_content=""；
// 两者都没有 finish_reason、不会触发 isStop（new-api 不会补结束块），也没有 usage
func TestNewAPIForwardsDataHeartbeat(t *testing.T) {
	// 原先的心跳：SSE 下的空行
	if _, forwarded := newAPIForward(t, ""); forwarded {
		t.Fatal("空行心跳不应被转发")
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:streamGenerateContent?alt=sse", nil)
	writer, err := newGeminiChunkWriter(recorder, request, "resp_1", "gemini-3.8-flash", true, false)
	if err != nil {
		t.Fatal(err)
	}
	writer.observe(aistudio.EventReasoning)
	if err := writer.heartbeat(); err != nil {
		t.Fatal(err)
	}
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(recorder.Body.String()))
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("期望两行 data（提交时一块、思考后一块）: %q", recorder.Body.String())
	}
	text, forwarded := newAPIForward(t, lines[0])
	if !forwarded || text.Content == nil || *text.Content != "" || text.ReasoningContent != nil {
		t.Fatalf("空文本心跳应转成 delta.content=\"\" 的增量: %+v forwarded=%v", text, forwarded)
	}
	thought, forwarded := newAPIForward(t, lines[1])
	if !forwarded || thought.ReasoningContent == nil || *thought.ReasoningContent != "" || thought.Content != nil {
		t.Fatalf("空思考心跳应转成 delta.reasoning_content=\"\" 的增量: %+v forwarded=%v", thought, forwarded)
	}
	for _, delta := range []newAPIDelta{text, thought} {
		if delta.FinishReason != nil || delta.Stop || delta.HasUsage || delta.ToolCalls != 0 {
			t.Fatalf("心跳不应带结束原因、触发结束或带用量: %+v", delta)
		}
	}
	// 对照：最后一块带 finishReason STOP，new-api 会补结束块
	final, _ := newAPIForward(t, `data: {"candidates":[{"index":0,"finishReason":"STOP"}],"responseId":"resp_1"}`)
	if !final.Stop {
		t.Fatal("对照组：STOP 块应触发 isStop")
	}
}

// TestOpenAIHeartbeatForwardedByNewAPI OpenAI 渠道：new-api 的扫描器同样只认 data 行（stream_scanner.go:301-307），": ping" 被丢弃；
// 空 delta 块是一行 data，经 OaiStreamHandler 原样写给客户端
func TestOpenAIHeartbeatForwardedByNewAPI(t *testing.T) {
	scan := func(body string) []string {
		var forwarded []string
		for _, line := range strings.Split(body, "\n") {
			data, hasPrefix := strings.CutPrefix(line, "data:")
			if data = strings.TrimSpace(data); hasPrefix && data != "" {
				forwarded = append(forwarded, data)
			}
		}
		return forwarded
	}
	recorder := httptest.NewRecorder()
	_ = chatHeartbeat(recorder, false, "id", 1, "m", false)()
	if forwarded := scan(recorder.Body.String()); len(forwarded) != 0 {
		t.Fatalf("\": ping\" 不应被转发: %v", forwarded)
	}
	recorder = httptest.NewRecorder()
	_ = chatHeartbeat(recorder, true, "id", 1, "m", false)()
	forwarded := scan(recorder.Body.String())
	if len(forwarded) != 1 || !json.Valid([]byte(forwarded[0])) {
		t.Fatalf("空 delta 块应作为一行 data 被转发: %v", forwarded)
	}
}

// TestGeminiDataHeartbeatOverTCP 经真实 TCP 读原始字节：首事件前的等待到时、事件流还没有任何事件时（streamGemini 照常提交响应头），
// 客户端立刻收到一块空文本心跳，不必等下一次心跳；原先这时只有响应头，中转在收到第一条 data 之前不给它的客户端写任何字节
func TestGeminiDataHeartbeatOverTCP(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := &server{config: Config{}, thoughtSignatures: newThoughtSignatureStore()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events := make(chan aistudio.Event)
		go func() {
			defer close(events)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}()
		s.streamGemini(w, r, aistudio.GenerateRequest{ID: "resp_7", Model: "gemini-3.8-flash"}, events, false)
	}))
	defer server.Close()
	startedAt := time.Now()
	response, err := http.Post(server.URL+"/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	const want = `data: {"candidates":[{"content":{"parts":[{"text":""}],"role":"model"},"index":0}],"modelVersion":"gemini-3.8-flash","responseId":"resp_7"}` + "\n"
	if line != want {
		t.Fatalf("提交响应头后的第一行 = %q，期望空文本心跳块 %q", line, want)
	}
	if elapsed := time.Since(startedAt); elapsed >= streamHeartbeatInterval {
		t.Fatalf("用时 %s，期望提交响应头后立刻收到心跳块", elapsed)
	}
}

// TestGeminiHeartbeatOfficialSDKFallsBack 官方 SDK 直连（x-goog-api-client 带 google-genai-sdk/、genai-py/、gl-python/ 等）时 Gemini SSE 的心跳
// 退回只写换行，提交响应头时也不写心跳块（直连不经过只转发 data 行的中转，换行就能保活，不再多写 google-genai chats 判为无效的空文本块）。
// 其他客户端照常写数据型心跳
func TestGeminiHeartbeatOfficialSDKFallsBack(t *testing.T) {
	withHeartbeatInterval(t, 30*time.Millisecond)
	withStreamStartHold(t, 20*time.Millisecond)
	steps := []pacedStep{
		{delay: 120 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventReasoning, Text: "想一想"}},
		{delay: 120 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventText, Text: "答案"}},
		{delay: 0, event: aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}},
	}
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	path := "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse"
	for _, client := range []string{"google-genai-sdk/1.55.0 gl-python/3.12.3", "genai-py/0.8.5 gl-python/3.11.9 grpc/1.66.1", "gl-python/3.10.0", "google-genai-sdk/1.30.0 gl-node/v22.11.0"} {
		recorder := postJSON(t, NewHandler(&pacedEventsService{steps: steps}, Config{APIKey: "sk-test"}), path, body, map[string]string{"X-Goog-Api-Client": client})
		got := geminiStreamKinds(t, recorder.Body.String())
		if strings.Join(got, ",") != "thought,text,finish" {
			t.Fatalf("%s：块顺序 = %v，期望官方 SDK 直连时没有心跳块\n%q", client, got, recorder.Body.String())
		}
		if !strings.HasPrefix(recorder.Body.String(), "\n") || !strings.Contains(recorder.Body.String(), "\n\n\n") {
			t.Fatalf("%s：静默期间应写换行心跳: %q", client, recorder.Body.String())
		}
	}
	recorder := postJSON(t, NewHandler(&pacedEventsService{steps: steps}, Config{APIKey: "sk-test"}), path, body, map[string]string{"X-Goog-Api-Client": "my-relay/1.0"})
	if got := collapseRuns(geminiStreamKinds(t, recorder.Body.String())); strings.Join(got, ",") != "hb-text,thought,hb-thought,text,finish" {
		t.Fatalf("其他客户端应照常写数据型心跳: %v\n%s", got, recorder.Body.String())
	}
}

// newAPILagForwarded 按 new-api OpenAI 渠道的转发规则（relay/channel/openai/relay-openai.go 的 OaiStreamHandler）：扫描器每读到一条 data，
// 先把上一条写给它的客户端，再记下这一条，所以已读到的 data 里最后一条还扣在 new-api 手里。返回此刻已写给它的客户端的 data
func newAPILagForwarded(body string) []string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		data, hasPrefix := strings.CutPrefix(line, "data:")
		if data = strings.TrimSpace(data); hasPrefix && data != "" {
			lines = append(lines, data)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return lines[:len(lines)-1]
}

// TestOpenAIRolePushedOutBehindNewAPI 首个事件还没到就提交响应头时，OpenAI Chat 在 role 块之后立刻再写一块空 delta，把 role 块“顶”出 new-api：
// 它落后一块转发，只写 role 块时 role 块要等到下一次心跳（10 秒后）或首个事件才到它的客户端。首个事件已到时不另写（首个事件的块紧随其后）；
// 关闭数据型心跳时与原先一致，只有 role 块
func TestOpenAIRolePushedOutBehindNewAPI(t *testing.T) {
	withStreamStartHold(t, 20*time.Millisecond)
	release := make(chan struct{})
	service := &gatedEventsService{release: release, events: []aistudio.Event{{Kind: aistudio.EventText, Text: "hi"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}}
	body := `{"model":"gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	for _, test := range []struct {
		name      string
		data      bool
		forwarded int
	}{
		{name: "开启", data: true, forwarded: 1},
		{name: "关闭", data: false, forwarded: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := test.data
			server := httptest.NewServer(NewHandler(service, Config{APIKey: "sk-test", StreamDataHeartbeat: func() bool { return data }}))
			defer server.Close()
			response, cancel, err := startStream(t, server, "/v1/chat/completions", body)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			defer response.Body.Close()
			// 首个事件被扣住：读到提交响应头时写出的全部字节（role 块，开启时另有一块空 delta）。要在提交响应头时立刻读到，
			// 不能等到 10 秒后的下一次心跳
			sentAt := time.Now()
			reader := bufio.NewReader(response.Body)
			var head strings.Builder
			wantBlocks := 1
			if data {
				wantBlocks = 2
			}
			for blocks := 0; blocks < wantBlocks; {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				head.WriteString(line)
				if strings.HasPrefix(line, "data: ") {
					blocks++
				}
			}
			if elapsed := time.Since(sentAt); elapsed >= streamHeartbeatInterval/2 {
				t.Fatalf("用时 %s 才读到提交响应头时的块，期望立刻读到", elapsed)
			}
			forwarded := newAPILagForwarded(head.String())
			if len(forwarded) != test.forwarded {
				t.Fatalf("首个事件前 new-api 已写给客户端 %d 块，期望 %d: %q", len(forwarded), test.forwarded, head.String())
			}
			if test.forwarded == 1 && !strings.Contains(forwarded[0], `"role":"assistant"`) {
				t.Fatalf("顶出去的应是 role 块: %s", forwarded[0])
			}
			release <- struct{}{}
			rest, _ := io.ReadAll(reader)
			if !strings.Contains(string(rest), `"content":"hi"`) {
				t.Fatalf("放行后应收到正文: %q", rest)
			}
		})
	}
	// 首个事件很快到达：role 块之后直接是首个事件的块，没有多余的空 delta
	quick := postJSON(t, NewHandler(&scriptedService{events: service.events}, Config{APIKey: "sk-test"}), "/v1/chat/completions", body, nil)
	chunks := sseData(t, quick.Body.String())
	if len(chunks) < 2 {
		t.Fatalf("块太少: %s", quick.Body.String())
	}
	second, _ := chunks[1]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if second["content"] != "hi" {
		t.Fatalf("首个事件已到时 role 块之后应直接是正文块: %s", quick.Body.String())
	}
}

// gatedEventsService 每次生成都先等 release 放行，再发出 events
type gatedEventsService struct {
	modelListService
	release <-chan struct{}
	events  []aistudio.Event
}

func (service *gatedEventsService) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events := make(chan aistudio.Event, len(service.events))
	go func() {
		defer close(events)
		select {
		case <-service.release:
		case <-ctx.Done():
			return
		}
		for _, event := range service.events {
			events <- event
		}
	}()
	return events, nil
}

// TestServerGenerateCapsStreamHold 经 server.generate：生成服务一直不返回（降级判定严格模式在判定出结果前不返回）时，扣住响应头到上限就照常提交
// 响应头开始推流（之后有心跳），之后才返回的错误在流内返回
func TestServerGenerateCapsStreamHold(t *testing.T) {
	withStreamStartHold(t, 60*time.Millisecond)
	blocking := &delayedService{delay: 300 * time.Millisecond, err: aistudio.ErrInvalidArgument}
	s := &server{service: blocking}
	startedAt := time.Now()
	events, eventReady, err := s.generateStream(context.Background(), aistudio.GenerateRequest{Stream: true})
	elapsed := time.Since(startedAt)
	if err != nil || events == nil || eventReady || elapsed < 50*time.Millisecond || elapsed > 250*time.Millisecond {
		t.Fatalf("events=%v eventReady=%v err=%v 用时 %s，期望约 60ms 时开始推流", events, eventReady, err, elapsed)
	}
	event, ok := <-events
	if !ok || event.Kind != aistudio.EventError || !errors.Is(event.Err, aistudio.ErrInvalidArgument) {
		t.Fatalf("上限之后返回的错误应在流内返回: %+v ok=%v", event, ok)
	}
	// 首个事件在上限内到达：按首个事件返回，eventReady 为真
	quick := &server{service: &delayedService{events: []aistudio.Event{{Kind: aistudio.EventText, Text: "a"}, {Kind: aistudio.EventFinish}}}}
	if events, eventReady, err := quick.generateStream(context.Background(), aistudio.GenerateRequest{Stream: true}); err != nil || !eventReady {
		t.Fatalf("首个事件已到时 eventReady 应为真: err=%v eventReady=%v", err, eventReady)
	} else {
		drainEvents(events)
	}
}

// headStartService 每次生成先立刻发出 head，等 release 放行后再发出 rest
type headStartService struct {
	modelListService
	release <-chan struct{}
	head    []aistudio.Event
	rest    []aistudio.Event
}

func (service *headStartService) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events := make(chan aistudio.Event, len(service.head)+len(service.rest))
	for _, event := range service.head {
		events <- event
	}
	go func() {
		defer close(events)
		select {
		case <-service.release:
		case <-ctx.Done():
			return
		}
		for _, event := range service.rest {
			events <- event
		}
	}()
	return events, nil
}

// TestHiddenReasoningHeartbeatAtCommit 隐藏思维链（-nothinking）且首个事件是思考时：首个事件在等待内就到了，响应头随即提交，但这条思考在过滤中被丢掉；
// 提交响应头时仍要立刻写一块心跳（Gemini 为空文本候选块，OpenAI Chat 为 role 块之后的空 delta），不能等下一个可见事件或一个心跳间隔。
// 不隐藏思维链时首个帧就是这条思考，没有多余的心跳块
func TestHiddenReasoningHeartbeatAtCommit(t *testing.T) {
	withHeartbeatInterval(t, 3*time.Second)
	catalog := modelListService{models: []aistudio.Model{{
		ID: "gemini-3.8-flash", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "thinking_level": true},
	}}}
	for _, test := range []struct {
		name, path, body string
		// blocks 为提交响应头时要立刻读到的 data 块数；check 核对最后一块
		blocks int
		check  func(data string) bool
	}{
		{
			name: "Gemini 隐藏思维链", path: "/v1beta/models/gemini-3.8-flash-nothinking:streamGenerateContent?alt=sse",
			body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, blocks: 1,
			check: func(data string) bool {
				var chunk map[string]any
				return json.Unmarshal([]byte(data), &chunk) == nil && geminiHeartbeatChunk(chunk) && !strings.Contains(data, `"thought"`)
			},
		},
		{
			name: "Gemini 显示思考", path: "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse",
			body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, blocks: 1,
			check: func(data string) bool {
				return strings.Contains(data, `"text":"先想想"`) && strings.Contains(data, `"thought":true`)
			},
		},
		{
			name: "OpenAI Chat 隐藏思维链", path: "/v1/chat/completions",
			body: `{"model":"gemini-3.8-flash-nothinking","stream":true,"messages":[{"role":"user","content":"hi"}]}`, blocks: 2,
			check: func(data string) bool {
				return strings.Contains(data, `"delta":{}`) && !strings.Contains(data, `"finish_reason":"`)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			release := make(chan struct{})
			service := &headStartService{
				modelListService: catalog, release: release,
				head: []aistudio.Event{{Kind: aistudio.EventReasoning, Text: "先想想"}},
				rest: []aistudio.Event{{Kind: aistudio.EventText, Text: "答案"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}},
			}
			server := httptest.NewServer(NewHandler(service, Config{APIKey: "sk-test"}))
			defer server.Close()
			sentAt := time.Now()
			response, cancel, err := startStream(t, server, test.path, test.body)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			defer response.Body.Close()
			reader := bufio.NewReader(response.Body)
			var last string
			for blocks := 0; blocks < test.blocks; {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if data, ok := strings.CutPrefix(line, "data: "); ok {
					blocks++
					last = strings.TrimSpace(data)
				}
			}
			if elapsed := time.Since(sentAt); elapsed >= time.Second {
				t.Fatalf("用时 %s 才读到提交响应头时的块，期望立刻读到（心跳间隔 3s）", elapsed)
			}
			if !test.check(last) {
				t.Fatalf("提交响应头时的块不对: %s", last)
			}
			close(release)
			rest, _ := io.ReadAll(reader)
			if !strings.Contains(string(rest), "答案") {
				t.Fatalf("放行后应收到正文: %q", rest)
			}
			if strings.Contains(test.path, "nothinking") && strings.Contains(string(rest), "先想想") {
				t.Fatalf("隐藏思维链时不应输出思考: %q", rest)
			}
		})
	}
}
