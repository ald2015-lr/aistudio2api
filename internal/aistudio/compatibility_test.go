package aistudio

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAgentToolSchemaCompatibility 验证常见 Agent 工具 Schema 的稳定转换规则
func TestAgentToolSchemaCompatibility(t *testing.T) {
	t.Run("string const", func(t *testing.T) {
		wire, err := encodeJSONSchema(json.RawMessage(`{
			"properties":{"mode":{"const":"agent_probe","title":"Mode","type":"string"}},
			"required":["mode"],"title":"record_literal_args","type":"object"
		}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(wire) <= 6 || wire[0] != int64(6) {
			t.Fatalf("schema wire=%#v", wire)
		}
		properties, ok := wire[6].([]any)
		if !ok || len(properties) != 1 {
			t.Fatalf("schema properties=%#v", wire[6])
		}
		entry, ok := properties[0].([]any)
		if !ok || len(entry) != 2 || entry[0] != "mode" {
			t.Fatalf("schema property=%#v", properties[0])
		}
		mode, ok := entry[1].([]any)
		if !ok || len(mode) <= 4 || mode[0] != int64(1) {
			t.Fatalf("mode schema=%#v", entry[1])
		}
		values, ok := mode[4].([]string)
		if !ok || len(values) != 1 || values[0] != "agent_probe" {
			t.Fatalf("mode enum=%#v", mode[4])
		}
	})

	t.Run("nested const", func(t *testing.T) {
		wire, err := encodeJSONSchema(json.RawMessage(`{"anyOf":[{"const":"fast"},{"const":"slow"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(wire) <= 17 || wire[0] != int64(1) {
			t.Fatalf("schema wire=%#v", wire)
		}
		variants, ok := wire[17].([]any)
		if !ok || len(variants) != 2 {
			t.Fatalf("schema variants=%#v", wire[17])
		}
		for index, want := range []string{"fast", "slow"} {
			variant, ok := variants[index].([]any)
			if !ok || len(variant) <= 4 || variant[0] != int64(1) {
				t.Fatalf("schema variant=%#v", variants[index])
			}
			values, ok := variant[4].([]string)
			if !ok || len(values) != 1 || values[0] != want {
				t.Fatalf("schema variant enum=%#v", variant[4])
			}
		}
	})

	t.Run("non-string const", func(t *testing.T) {
		for _, raw := range []string{`{"const":1}`, `{"const":true}`, `{"const":null}`} {
			if _, err := encodeJSONSchema(json.RawMessage(raw)); err == nil || !strings.Contains(err.Error(), "const") {
				t.Fatalf("schema=%s error=%v", raw, err)
			}
		}
	})
}

// TestFunctionCallThoughtSignature 验证历史工具调用的签名补齐与原值保留
func TestFunctionCallThoughtSignature(t *testing.T) {
	for _, test := range []struct {
		name      string
		signature string
		want      string
	}{
		{name: "missing", want: "skip_thought_signature_validator"},
		{name: "present", signature: "real-signature", want: "real-signature"},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire, err := encodePart(Part{FunctionCall: &FunctionCall{
				ID: "call_123", Name: "fixture", Arguments: json.RawMessage(`{"value":"ok"}`),
				ThoughtSignature: test.signature,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if len(wire) <= 14 || wire[14] != test.want {
				t.Fatalf("thought signature=%#v", wire)
			}
		})
	}
}

// TestSignatureOnlyPartWire 只带签名的 Part（Anthropic thinking/redacted_thinking、Responses reasoning、Gemini 原生只带 thoughtSignature
// 的部分）按官网写法编码为空正文加签名 [null,"",…,签名]；带正文的签名 Part 不变；binding 照旧为这个 Part 计一个空占位
func TestSignatureOnlyPartWire(t *testing.T) {
	wire, err := encodePart(Part{ThoughtSignature: "sig-only"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(wire)
	if want := `[null,"",null,null,null,null,null,null,null,null,null,null,null,null,"sig-only"]`; string(encoded) != want {
		t.Fatalf("只带签名的 Part = %s，期望 %s", encoded, want)
	}
	wire, err = encodePart(Part{Text: "answer", ThoughtSignature: "sig-text"})
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ = json.Marshal(wire); string(encoded) != `[null,"answer",null,null,null,null,null,null,null,null,null,null,null,null,"sig-text"]` {
		t.Fatalf("带正文的签名 Part = %s", encoded)
	}
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "hi"}}},
		{Role: RoleAssistant, Parts: []Part{{ThoughtSignature: "sig-only"}, {Text: "answer"}}},
		{Role: RoleUser, Parts: []Part{{Text: "next"}}},
	}
	if prompt, err := bindingPrompt(GenerateRequest{Contents: contents}); err != nil || prompt != "hi  answer next" {
		t.Fatalf("bindingPrompt = %q, %v；只带签名的 Part 应计一个空占位", prompt, err)
	}
}

// TestSignatureOnlyPartWithLongFunctionName 只带签名的 Part 与超过 63 字节的函数名（wireFunctionName）出现在同一段历史里时两条规则
// 各自成立：签名 Part 编码为空正文加签名 [null,"",…,签名]，同一轮后面的函数调用与之后的函数结果用截短名（Playground 与 Build 都一样，
// Build 的签名 Part 为 text:"" 加签名）；binding 对签名 Part 与函数调用、结果都只计空占位，与函数名长短无关
func TestSignatureOnlyPartWithLongFunctionName(t *testing.T) {
	name := longFunctionPrefix + "command"
	wireName := wireFunctionName(name)
	if wireName == name || len(wireName) != maxWireFunctionNameLength {
		t.Fatalf("测试用的函数名应被截短: %q", wireName)
	}
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "run"}}},
		{Role: RoleAssistant, Parts: []Part{
			{ThoughtSignature: "sig-only"},
			{FunctionCall: &FunctionCall{ID: "call-1", Name: name, Arguments: json.RawMessage(`{}`), ThoughtSignature: "sig-call"}},
		}},
		{Role: RoleUser, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-1", Name: name, Content: json.RawMessage(`{"ok":true}`)}}}},
	}
	wire, err := encodeContents(contents)
	if err != nil {
		t.Fatal(err)
	}
	playground, _ := json.Marshal(wire)
	if !strings.Contains(string(playground), `[null,"",null,null,null,null,null,null,null,null,null,null,null,null,"sig-only"]`) {
		t.Fatalf("只带签名的 Part 应为空正文加签名: %s", playground)
	}
	if strings.Count(string(playground), `"`+wireName+`"`) != 2 || strings.Contains(string(playground), name) ||
		!strings.Contains(string(playground), `"sig-call"`) {
		t.Fatalf("函数调用与结果应为截短名、调用保留自己的签名: %s", playground)
	}
	_, build, err := EncodeBuildGenerateRequest(GenerateRequest{Model: "test", Contents: contents}, GenerationDefaults{MaxOutputTokens: 8192}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(build), `{"text":"","thoughtSignature":"sig-only"}`) ||
		strings.Count(string(build), `"`+wireName+`"`) != 2 || strings.Contains(string(build), name) {
		t.Fatalf("Build 请求的签名 Part 应为 text:\"\" 加签名，函数名为截短名: %s", build)
	}
	if prompt, err := bindingPrompt(GenerateRequest{Contents: contents}); err != nil || prompt != "run   " {
		t.Fatalf("bindingPrompt = %q, %v；签名 Part、函数调用与结果各计一个空占位", prompt, err)
	}
}

// TestRPCErrorCompatibility 验证真实错误帧解码和 Drive 授权边界
func TestRPCErrorCompatibility(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		err := DecodeRPCError("GenerateAccessToken", http.StatusUnauthorized, []byte(
			`[16,"OAuth error: unauthorized_client.",[["type.googleapis.com/google.internal.alkali.applications.makersuite.v1.AiStudioErrorDetails",[null,[]]]]]`,
		))
		if err.Code != 16 || err.Message != "OAuth error: unauthorized_client." || !driveAuthorizationMissing(err) {
			t.Fatalf("RPC error=%+v", err)
		}
	})

	t.Run("wrapped", func(t *testing.T) {
		err := DecodeRPCError("GenerateContent", http.StatusForbidden, []byte(
			`[null,[7,"Permission denied",[["type.googleapis.com/google.rpc.ErrorInfo",["DENIED","google.com",[["reason","account_permission"]]]]]]]`,
		))
		if err.Code != 7 || err.Message != "Permission denied" || err.Metadata["reason"] != "account_permission" {
			t.Fatalf("RPC error=%+v", err)
		}
	})

	t.Run("login failure", func(t *testing.T) {
		err := DecodeRPCError("GenerateAccessToken", http.StatusUnauthorized, []byte(
			`[16,"Request had invalid authentication credentials."]`,
		))
		if driveAuthorizationMissing(err) {
			t.Fatalf("login error classified as Drive authorization: %+v", err)
		}
	})

	t.Run("different scope", func(t *testing.T) {
		for _, err := range []*RPCError{
			{Method: "GenerateContent", StatusCode: http.StatusUnauthorized, Code: 16, Message: "OAuth error: unauthorized_client."},
			{Method: "GenerateAccessToken", StatusCode: http.StatusForbidden, Code: 16, Message: "OAuth error: unauthorized_client."},
		} {
			if driveAuthorizationMissing(err) {
				t.Fatalf("unrelated error classified as Drive authorization: %+v", err)
			}
		}
	})
}

// TestEncodeContentsEmptyPartsFilter 验证多轮历史中空 parts 内容被自动忽略而不触发编码校验失败
func TestEncodeContentsEmptyPartsFilter(t *testing.T) {
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "hello"}}},
		{Role: RoleAssistant, Parts: nil},
		{Role: RoleAssistant, Parts: []Part{}},
		{Role: RoleUser, Parts: []Part{{Text: "world"}}},
	}
	wire, err := encodeContents(contents)
	if err != nil {
		t.Fatalf("encodeContents failed: %v", err)
	}
	if len(wire) != 2 {
		t.Fatalf("expected 2 valid wire contents, got %d: %#v", len(wire), wire)
	}
}

// TestAttachYouTubeMediaPreservesText 验证普通文本与非空白字符在附加 YouTube 媒体时不被截断为空
func TestAttachYouTubeMediaPreservesText(t *testing.T) {
	content := Content{
		Role: RoleUser,
		Parts: []Part{
			{Text: "  normal text with space  "},
			{Text: "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
		},
	}
	attached := attachYouTubeMedia(content)
	if len(attached.Parts) != 2 {
		t.Fatalf("expected 2 parts (1 text + 1 external media), got %d: %#v", len(attached.Parts), attached.Parts)
	}
	if attached.Parts[0].Text != "  normal text with space  " {
		t.Fatalf("unexpected text: %q", attached.Parts[0].Text)
	}
	if attached.Parts[1].ExternalMedia == nil {
		t.Fatalf("expected external media part")
	}
	for _, text := range []string{"  https://example.com/article  ", "  https://youtube.com/playlist?list=abc  "} {
		got := attachYouTubeMedia(Content{Role: RoleUser, Parts: []Part{{Text: text}}})
		if len(got.Parts) != 1 || got.Parts[0].Text != text {
			t.Errorf("ordinary text changed: %#v", got)
		}
	}
}

// TestAttachYouTubeMediaVideoIDs 文本里只有视频 ID 恰好 11 位的 YouTube 链接转为视频；链接在汉字、全角标点与反引号处结束，
// 后面的汉字保留在正文里；示例里的占位写法（VIDEO_ID、${videoId}）与太短的 ID 原样保留为文本
func TestAttachYouTubeMediaVideoIDs(t *testing.T) {
	for _, test := range []struct{ text, media, rest string }{
		{"用一句话总结https://youtu.be/jNQXAC9IVRw这个视频", "https://www.youtube.com/watch?v=jNQXAC9IVRw", "用一句话总结这个视频"},
		{"看下https://youtu.be/dQw4w9WgXcQ讲了什么", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", "看下讲了什么"},
		{"看看 https://www.youtube.com/watch?v=jNQXAC9IVRw，讲了什么", "https://www.youtube.com/watch?v=jNQXAC9IVRw", "看看 ，讲了什么"},
		{"视频（https://youtu.be/jNQXAC9IVRw）", "https://www.youtube.com/watch?v=jNQXAC9IVRw", "视频（）"},
		{"打开 `https://youtu.be/jNQXAC9IVRw` 看看", "https://www.youtube.com/watch?v=jNQXAC9IVRw", "打开 `` 看看"},
		{"Embed players with `https://www.youtube.com/embed/${videoId}`.", "", "Embed players with `https://www.youtube.com/embed/${videoId}`."},
		{"Template: https://www.youtube.com/watch?v=VIDEO_ID", "", "Template: https://www.youtube.com/watch?v=VIDEO_ID"},
		{"See https://youtu.be/abc", "", "See https://youtu.be/abc"},
	} {
		got := attachYouTubeMedia(Content{Role: RoleUser, Parts: []Part{{Text: test.text}}})
		var media, rest string
		for _, part := range got.Parts {
			if part.ExternalMedia != nil {
				media = part.ExternalMedia.URL
			}
			rest += part.Text
		}
		if media != test.media || rest != test.rest {
			t.Errorf("%q: media=%q text=%q，期望 media=%q text=%q", test.text, media, rest, test.media, test.rest)
		}
	}
}

// TestYouTubeBindingMatchesRequestBody 链接后紧跟汉字时，发送的正文与 WAA binding 用的是同一份整理结果：请求体里的文本与外部视频
// 与 bindingPrompt 的文本、占位一致（两边都保留链接后的汉字），binding 的组成规则不变（文本按顺序以空格连接，外部视频为空占位）
func TestYouTubeBindingMatchesRequestBody(t *testing.T) {
	request := GenerateRequest{Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "看下https://youtu.be/dQw4w9WgXcQ讲了什么"}}}}}
	prompt, err := bindingPrompt(request)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != " 看下讲了什么" {
		t.Fatalf("bindingPrompt = %q，期望 %q（外部视频为空占位，链接后的汉字保留）", prompt, " 看下讲了什么")
	}
	var pending []FunctionCall
	wire, err := encodeContent(request.Contents[0], &pending)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(wire)
	want := `[[[null,null,null,null,null,null,["video/*","https://www.youtube.com/watch?v=dQw4w9WgXcQ"]],[null,"看下讲了什么"]],"user"]`
	if string(encoded) != want {
		t.Fatalf("请求体 = %s\n期望 %s", encoded, want)
	}
}

// TestFunctionResultNameInference 验证工具结果的唯一关联与原始输入保留
func TestFunctionResultNameInference(t *testing.T) {
	call := func(id, name string) Part {
		return Part{FunctionCall: &FunctionCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}}
	}
	result := func(id string) Part {
		return Part{FunctionResult: &FunctionResult{ID: id, Content: json.RawMessage(`{"ok":true}`)}}
	}
	for _, test := range []struct {
		name     string
		contents []Content
		want     string
	}{
		{"matching id", []Content{{Role: RoleAssistant, Parts: []Part{call("one", "lookup"), call("two", "weather")}}, {Role: RoleTool, Parts: []Part{result("one")}}}, "lookup"},
		{"unique mismatched id", []Content{{Role: RoleAssistant, Parts: []Part{call("one", "lookup")}}, {Role: RoleTool, Parts: []Part{result("opaque-id")}}}, "lookup"},
		{"missing id", []Content{{Role: RoleAssistant, Parts: []Part{call("one", "lookup")}}, {Role: RoleTool, Parts: []Part{result("")}}}, "lookup"},
		{"remaining call", []Content{{Role: RoleAssistant, Parts: []Part{call("one", "lookup"), call("two", "weather")}}, {Role: RoleTool, Parts: []Part{result("one")}}, {Role: RoleTool, Parts: []Part{result("opaque-id")}}}, "weather"},
		{"split calls", []Content{{Role: RoleAssistant, Parts: []Part{call("one", "lookup")}}, {Role: RoleAssistant, Parts: []Part{call("two", "weather")}}, {Role: RoleTool, Parts: []Part{result("two")}}, {Role: RoleTool, Parts: []Part{result("one")}}}, "lookup"},
		{"consumed call", []Content{{Role: RoleAssistant, Parts: []Part{call("one", "lookup")}}, {Role: RoleTool, Parts: []Part{result("one")}}, {Role: RoleTool, Parts: []Part{result("call_other")}}}, ""},
		{"user result", []Content{{Role: RoleAssistant, Parts: []Part{call("one", "lookup")}}, {Role: RoleUser, Parts: []Part{result("one")}}}, "lookup"},
		{"ambiguous id", []Content{{Role: RoleAssistant, Parts: []Part{call("one", "lookup"), call("two", "weather")}}, {Role: RoleTool, Parts: []Part{result("call_other")}}}, ""},
		{"orphan id", []Content{{Role: RoleTool, Parts: []Part{result("opaque-id")}}}, ""},
		{"closed turn", []Content{{Role: RoleAssistant, Parts: []Part{call("one", "lookup")}}, {Role: RoleTool, Parts: []Part{result("one")}}, {Role: RoleAssistant, Parts: []Part{{Text: "done"}}}, {Role: RoleTool, Parts: []Part{result("call_other")}}}, ""},
		{"new turn", []Content{{Role: RoleAssistant, Parts: []Part{call("one", "lookup")}}, {Role: RoleUser, Parts: []Part{{Text: "new question"}}}, {Role: RoleTool, Parts: []Part{result("call_other")}}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire, err := encodeContents(test.contents)
			if test.want == "" {
				if err == nil {
					t.Fatalf("unresolved result accepted: %#v", wire)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			last := wire[len(wire)-1].([]any)[0].([]any)[0].([]any)[11].([]any)
			if last[0] != test.want {
				t.Fatalf("name=%v want=%s", last[0], test.want)
			}
			if test.contents[len(test.contents)-1].Parts[0].FunctionResult.Name != "" {
				t.Fatal("input mutated")
			}
		})
	}
}
