package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 历史中任何位置的孤立工具结果（orphanResultsAsText，上游 d94bf91）的测试

func orphanCall(id, name string) aistudio.Part {
	return aistudio.Part{FunctionCall: &aistudio.FunctionCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}}
}

func orphanResult(id, name, content string) aistudio.Part {
	return aistudio.Part{FunctionResult: &aistudio.FunctionResult{ID: id, Name: name, Content: json.RawMessage(content)}}
}

func orphanText(role aistudio.Role, text string) aistudio.Content {
	return aistudio.Content{Role: role, Parts: []aistudio.Part{{Text: text}}}
}

// orphanShape 把历史写成一行便于比对：每轮为 角色[部件,…]，部件为 call:名、result:名、text:正文、media
func orphanShape(contents []aistudio.Content) string {
	turns := make([]string, 0, len(contents))
	for _, content := range contents {
		parts := make([]string, 0, len(content.Parts))
		for _, part := range content.Parts {
			switch {
			case part.FunctionCall != nil:
				parts = append(parts, "call:"+part.FunctionCall.Name)
			case part.FunctionResult != nil:
				parts = append(parts, "result:"+part.FunctionResult.Name)
			case part.InlineData != nil:
				media := "media"
				if part.FromToolResult {
					media += "(tool)"
				}
				parts = append(parts, media)
			default:
				parts = append(parts, "text:"+part.Text)
			}
		}
		turns = append(turns, string(content.Role)+"["+strings.Join(parts, ",")+"]")
	}
	return strings.Join(turns, " ")
}

// orphanEncodes 确认整理后的历史能编码（函数结果都有名称），且每组调用与结果的数量一致
func orphanEncodes(t *testing.T, contents []aistudio.Content) {
	t.Helper()
	wire, err := aistudio.EncodeCountTokensRequest(aistudio.TokenCountRequest{Model: "gemini-3.8-flash", Contents: contents})
	if err != nil {
		t.Fatalf("整理后的历史应能编码: %v | %s", err, orphanShape(contents))
	}
	if len(wire) == 0 {
		t.Fatal("编码结果为空")
	}
}

// TestOrphanResultsAnywhereInHistory 整段历史里关联不到调用的工具结果改写成文字说明，放在这组工具结果之后的用户消息里，不插在调用与结果之间：
// 历史中段带名称与不带名称的孤立结果、夹在调用与它的结果之间的孤立结果、重复调用 ID、同名调用按顺序关联、同一个结果发了两次、压缩上下文后
// 留下的结果、整条都是孤立结果的消息（按原顺序改写）；关联上的无名结果补上调用的名称；没有孤立结果时历史逐项不变
func TestOrphanResultsAnywhereInHistory(t *testing.T) {
	for _, test := range []struct {
		name      string
		contents  []aistudio.Content
		want      string
		rewritten int
	}{
		{
			name: "中段带名称的孤立结果",
			contents: []aistudio.Content{
				orphanText(aistudio.RoleUser, "q1"),
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("a", "lookup")}},
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("a", "lookup", `{"ok":1}`)}},
				orphanText(aistudio.RoleUser, "q2"),
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("x", "read_file", `{"text":"old"}`)}},
				orphanText(aistudio.RoleUser, "q3"),
			},
			want:      `user[text:q1] assistant[call:lookup] tool[result:lookup] user[text:q2] user[text:Tool result read_file: {"text":"old"}] user[text:q3]`,
			rewritten: 1,
		},
		{
			name: "中段没有名称、关联不到的结果",
			contents: []aistudio.Content{
				orphanText(aistudio.RoleUser, "q1"),
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("a", "lookup"), orphanCall("b", "search")}},
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("a", "", `{"ok":1}`), orphanResult("zz", "", `{"stale":true}`), orphanResult("b", "", `{"ok":2}`)}},
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "done"}}},
			},
			want:      `user[text:q1] assistant[call:lookup,call:search] tool[result:lookup,result:search] user[text:Tool result (zz): {"stale":true}] assistant[text:done]`,
			rewritten: 1,
		},
		{
			name: "夹在调用与它的结果之间",
			contents: []aistudio.Content{
				orphanText(aistudio.RoleUser, "q1"),
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("a", "lookup")}},
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("x", "read_file", `{}`)}},
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("a", "lookup", `{}`)}},
				orphanText(aistudio.RoleUser, "q2"),
			},
			want:      `user[text:q1] assistant[call:lookup] tool[result:lookup] user[text:Tool result read_file: {}] user[text:q2]`,
			rewritten: 1,
		},
		{
			name: "重复调用 ID 与重复的结果",
			contents: []aistudio.Content{
				orphanText(aistudio.RoleUser, "q1"),
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("call_0", "lookup"), orphanCall("call_0", "search")}},
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("call_0", "search", `{"s":1}`)}},
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("call_0", "", `{"l":1}`)}},
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("call_0", "lookup", `{"l":1}`)}},
			},
			want:      `user[text:q1] assistant[call:lookup,call:search] tool[result:search,result:lookup] user[text:Tool result lookup: {"l":1}]`,
			rewritten: 1,
		},
		{
			name: "同名调用按顺序、剩下唯一的补名",
			contents: []aistudio.Content{
				orphanText(aistudio.RoleUser, "q1"),
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("1", "search"), orphanCall("2", "search"), orphanCall("3", "lookup")}},
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("", "search", `{"n":1}`), orphanResult("", "search", `{"n":2}`), orphanResult("", "", `{"n":3}`)}},
			},
			want:      `user[text:q1] assistant[call:search,call:search,call:lookup] tool[result:search,result:search,result:lookup]`,
			rewritten: 0,
		},
		{
			name: "压缩上下文后留下的结果",
			contents: []aistudio.Content{
				orphanText(aistudio.RoleUser, "Summary of the earlier conversation: the user asked to inspect the repository."),
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("call_old", "", `{"result":"README.md"}`)}},
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("call_new", "shell")}},
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("call_new", "", `{"result":"ok"}`)}},
				orphanText(aistudio.RoleUser, "继续"),
			},
			want: `user[text:Summary of the earlier conversation: the user asked to inspect the repository.] user[text:Tool result (call_old): {"result":"README.md"}] ` +
				`assistant[call:shell] tool[result:shell] user[text:继续]`,
			rewritten: 1,
		},
		{
			name: "整条都是孤立结果时保持部件顺序",
			contents: []aistudio.Content{
				orphanText(aistudio.RoleUser, "q1"),
				orphanText(aistudio.RoleAssistant, "ok"),
				{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "看结果"}, orphanResult("x", "", `{}`), {Text: "然后"}}},
			},
			want:      `user[text:q1] assistant[text:ok] user[text:看结果,text:Tool result (x): {},text:然后]`,
			rewritten: 1,
		},
		{
			// 编码时没有部件的消息整条跳过、不清空待返回调用：这里同样不清空，结果按 ID 关联（若把空消息当作普通用户消息清空待返回调用，结果会被改写成文字，调用之后没有结果）
			name: "调用与结果之间夹了空的用户消息",
			contents: []aistudio.Content{
				orphanText(aistudio.RoleUser, "q1"),
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("c1", "lookup")}},
				{Role: aistudio.RoleUser},
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("c1", "", `{"result":"ok"}`)}},
			},
			want:      `user[text:q1] assistant[call:lookup] tool[result:lookup]`,
			rewritten: 0,
		},
		{
			// 有名称的结果先按名称关联，排在前面的无名结果再按唯一待返回调用关联（逐个处理时无名结果先面对两个调用、被判为孤立，上游 d94bf91 即如此；原先编码层对它报 400）
			name: "无名结果排在有名称的结果之前",
			contents: []aistudio.Content{
				orphanText(aistudio.RoleUser, "q"),
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("", "a"), orphanCall("", "b")}},
				{Role: aistudio.RoleUser, Parts: []aistudio.Part{orphanResult("", "", `{"r":"a"}`), orphanResult("", "b", `{"r":"b"}`)}},
			},
			want:      `user[text:q] assistant[call:a,call:b] user[result:a,result:b]`,
			rewritten: 0,
		},
		{
			name: "有名称的结果排在无名结果之前",
			contents: []aistudio.Content{
				orphanText(aistudio.RoleUser, "q"),
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("", "a"), orphanCall("", "b")}},
				{Role: aistudio.RoleUser, Parts: []aistudio.Part{orphanResult("", "b", `{"r":"b"}`), orphanResult("", "", `{"r":"a"}`)}},
			},
			want:      `user[text:q] assistant[call:a,call:b] user[result:b,result:a]`,
			rewritten: 0,
		},
		{
			name: "调用之后插了普通用户消息",
			contents: []aistudio.Content{
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("a", "lookup")}},
				orphanText(aistudio.RoleUser, "先别管它"),
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("a", "lookup", `{}`)}},
			},
			want:      `user[text:Continue.] assistant[call:lookup] user[text:先别管它] user[text:Tool result lookup: {}]`,
			rewritten: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := orphanShape(test.contents)
			rewritten, count := orphanResultsAsText(test.contents)
			if count != test.rewritten {
				t.Fatalf("改写 %d 个结果，期望 %d: %s", count, test.rewritten, orphanShape(rewritten))
			}
			if orphanShape(test.contents) != original {
				t.Fatal("不应改动传入的历史")
			}
			normalized, _ := normalizeTurnSplits(test.contents)
			if got := orphanShape(normalized); got != test.want {
				t.Fatalf("整理后 =\n%s\n期望 =\n%s", got, test.want)
			}
			orphanEncodes(t, normalized)
		})
	}
	unchanged := []aistudio.Content{
		orphanText(aistudio.RoleUser, "q1"),
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("a", "lookup")}},
		{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("a", "lookup", `{}`)}},
	}
	if rewritten, count := orphanResultsAsText(unchanged); count != 0 || &rewritten[0] != &unchanged[0] {
		t.Fatal("没有孤立结果、也不用补名时应原样返回")
	}
}

// TestOrphanResultsResponsesEmptyUserMessage Responses 输入在函数调用与它的输出之间夹了一条空的用户消息（content 为空串，或只有一个空的
// input_text）：空消息丢掉，输出按 call_id 关联，调用之后紧跟函数结果，不改写成文字
func TestOrphanResultsResponsesEmptyUserMessage(t *testing.T) {
	for _, empty := range []string{
		`{"type":"message","role":"user","content":""}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`,
	} {
		input := `[{"type":"message","role":"user","content":"hi"},{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},` +
			empty + `,{"type":"function_call_output","call_id":"c1","output":"ok"}]`
		contents, _, err := responsesContents(json.RawMessage(input))
		if err != nil {
			t.Fatal(err)
		}
		normalized, _ := normalizeTurnSplits(contents)
		if got, want := orphanShape(normalized), `user[text:hi] assistant[call:lookup] tool[result:lookup]`; got != want {
			t.Fatalf("空消息 %s：整理后 = %s，期望 %s", empty, got, want)
		}
		orphanEncodes(t, normalized)
	}
}

// TestOrphanResultMediaFollowsText 孤立结果里取出的图片随文字说明一起放进用户消息（不再标注为哪个工具返回的图片）；同一条消息里关联上的结果
// 与它的图片照旧（图片移到这组工具结果之后的用户消息里，与孤立结果的说明并在同一条用户消息里）
func TestOrphanResultMediaFollowsText(t *testing.T) {
	image := func() aistudio.Part {
		return aistudio.Part{InlineData: &aistudio.Blob{MIME: "image/png", Data: []byte("png")}, FromToolResult: true}
	}
	contents := []aistudio.Content{
		orphanText(aistudio.RoleUser, "截图"),
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{orphanCall("a", "shot")}},
		{Role: aistudio.RoleTool, Parts: []aistudio.Part{orphanResult("x", "old_shot", `{}`), image(), orphanResult("a", "shot", `{}`), image()}},
	}
	normalized, _ := normalizeTurnSplits(contents)
	want := `user[text:截图] assistant[call:shot] tool[result:shot] ` +
		`user[text:以下是上面工具调用返回的图片或文件：,text:[工具 shot 返回的图片 1],media,text:Tool result old_shot: {},media]`
	if got := orphanShape(normalized); got != want {
		t.Fatalf("整理后 =\n%s\n期望 =\n%s", got, want)
	}
	orphanEncodes(t, normalized)
}

// TestOrphanResultsProtocols 各协议入口都经过同一步：OpenAI Chat 中段的 tool 消息、Responses 的 function_call_output、Anthropic 的 tool_result、
// Gemini 没有名称的 functionResponse 关联不到调用时改写成文字说明，生成服务收到的历史里调用与结果数量一致、能编码
func TestOrphanResultsProtocols(t *testing.T) {
	reply := []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}
	for _, test := range []struct {
		name, path, body, orphan string
	}{
		{
			name: "OpenAI Chat", path: "/v1/chat/completions", orphan: `Tool result read_file: {"result":"old"}`,
			body: `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"q1"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"call_x","name":"read_file","content":"old"},` +
				`{"role":"tool","tool_call_id":"call_a","content":"new"},{"role":"user","content":"q2"}]}`,
		},
		{
			name: "Responses", path: "/v1/responses", orphan: `Tool result (call_old): {"result":"old"}`,
			body: `{"model":"gemini-3.8-flash","input":[{"type":"message","role":"user","content":"q1"},` +
				`{"type":"function_call_output","call_id":"call_old","output":"old"},` +
				`{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{}"},` +
				`{"type":"function_call_output","call_id":"call_a","output":"new"},{"type":"message","role":"user","content":"q2"}]}`,
		},
		{
			name: "Anthropic", path: "/v1/messages", orphan: `Tool result (toolu_old): {"result":"old"}`,
			body: `{"model":"gemini-3.8-flash","max_tokens":100,"messages":[{"role":"user","content":"q1"},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"lookup","input":{}},{"type":"tool_use","id":"toolu_b","name":"search","input":{}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"a"},{"type":"tool_result","tool_use_id":"toolu_old","content":"old"},` +
				`{"type":"tool_result","tool_use_id":"toolu_b","content":"b"}]},{"role":"user","content":"q2"}]}`,
		},
		{
			name: "Gemini", path: "/v1beta/models/gemini-3.8-flash:generateContent", orphan: `Tool result (fc_old): {"content":"old"}`,
			body: `{"contents":[{"role":"user","parts":[{"text":"q1"}]},` +
				`{"role":"model","parts":[{"functionCall":{"id":"fc_a","name":"lookup","args":{}}},{"functionCall":{"id":"fc_b","name":"search","args":{}}}]},` +
				`{"role":"user","parts":[{"functionResponse":{"id":"fc_a","name":"","response":{"ok":1}}},{"functionResponse":{"id":"fc_old","name":"","response":{"content":"old"}}},` +
				`{"functionResponse":{"id":"fc_b","name":"","response":{"ok":2}}}]},{"role":"user","parts":[{"text":"q2"}]}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &sequenceService{replies: [][]aistudio.Event{reply}}
			handler := NewHandler(service, Config{APIKey: "sk-test"})
			if recorder := postJSON(t, handler, test.path, test.body, nil); recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			contents := service.request(t, 0).Contents
			calls, results, found := 0, 0, false
			for _, content := range contents {
				for _, part := range content.Parts {
					switch {
					case part.FunctionCall != nil:
						calls++
					case part.FunctionResult != nil:
						results++
						if part.FunctionResult.Name == "" {
							t.Fatalf("关联上的结果应补上名称: %s", orphanShape(contents))
						}
					case part.Text == test.orphan:
						found = content.Role == aistudio.RoleUser
					}
				}
			}
			if !found || calls != results {
				t.Fatalf("孤立结果应改写成用户消息里的 %q，调用 %d 个、结果 %d 个: %s", test.orphan, calls, results, orphanShape(contents))
			}
			orphanEncodes(t, contents)
		})
	}
}
