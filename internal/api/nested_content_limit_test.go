package api

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// nestedJSON 生成 depth 层嵌套的 JSON：每层为 open + 内层 + close
func nestedJSON(depth int, open string, inner string, close string) string {
	var builder strings.Builder
	builder.Grow(depth*(len(open)+len(close)) + len(inner))
	for range depth {
		builder.WriteString(open)
	}
	builder.WriteString(inner)
	for range depth {
		builder.WriteString(close)
	}
	return builder.String()
}

// allocatedBytes 返回 fn 执行期间累计分配的字节数
func allocatedBytes(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestAnthropicDeepNestingRejectedCheaply 深层嵌套的 search_result、document content source 与 web_fetch 结果不再逐层递归转换：
// 原先每层重新解析剩余 JSON，约 5000 层、300 KB 的请求体就要分配 5 GB 内存。现在内层不合规范的块直接返回错误，开销与请求体大小成线性
func TestAnthropicDeepNestingRejectedCheaply(t *testing.T) {
	const depth = 3000
	const limit = 64 << 20
	for name, content := range map[string]string{
		"search_result":    nestedJSON(depth, `[{"type":"search_result","source":"s","title":"t","content":`, `"x"`, `}]`),
		"document content": nestedJSON(depth, `[{"type":"document","source":{"type":"content","content":`, `"x"`, `}}]`),
		"web_fetch": nestedJSON(depth,
			`[{"type":"web_fetch_tool_result","tool_use_id":"t","content":{"type":"web_fetch_result","url":"u","content":{"type":"document","source":{"type":"content","content":`,
			`"x"`, `}}}}]`),
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			allocated := allocatedBytes(func() { _, err = anthropicParts(json.RawMessage(content)) })
			if allocated > limit {
				t.Fatalf("%d 字节的请求体分配了 %d 字节", len(content), allocated)
			}
			if err == nil {
				t.Fatal("嵌套的同类块应返回错误")
			}
		})
	}
}

// TestAnthropicNestedContentFollowsSpec search_result 与 document content source 的内容按规范接受字符串、text 与 image 块，
// 其他块返回错误；web_fetch 结果只接受 document 块
func TestAnthropicNestedContentFollowsSpec(t *testing.T) {
	parts, err := anthropicParts(json.RawMessage(`[
		{"type":"search_result","source":"https://kb.example/1","title":"KB","content":[{"type":"text","text":"kb body","citations":[]},{"type":"text","text":""}]},
		{"type":"search_result","source":"https://kb.example/2","title":"KB2","content":"plain body"},
		{"type":"document","source":{"type":"content","content":[{"type":"text","text":"chunk"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}},
		{"type":"web_fetch_tool_result","tool_use_id":"t","content":{"type":"web_fetch_result","url":"https://go.dev",
			"content":{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERg=="}}}}]`))
	if err != nil {
		t.Fatalf("合规范的嵌套内容应被接受: %v", err)
	}
	if len(parts) != 8 || parts[0].Text != "Search result: KB\nhttps://kb.example/1" || parts[1].Text != "kb body" ||
		parts[3].Text != "plain body" || parts[4].Text != "chunk" || parts[5].InlineData == nil ||
		parts[6].Text != "Web fetch: https://go.dev" || parts[7].InlineData == nil || parts[7].InlineData.MIME != "application/pdf" {
		t.Fatalf("parts=%+v", parts)
	}
	for name, block := range map[string]string{
		"search_result 里的 tool_use":    `[{"type":"search_result","source":"s","title":"t","content":[{"type":"tool_use","id":"x","name":"f","input":{}}]}]`,
		"search_result 里的 document":    `[{"type":"search_result","source":"s","title":"t","content":[{"type":"document","source":{"type":"text","data":"x"}}]}]`,
		"document 里的 document":         `[{"type":"document","source":{"type":"content","content":[{"type":"document","source":{"type":"text","data":"x"}}]}}]`,
		"search_result 内容不是数组":         `[{"type":"search_result","source":"s","title":"t","content":{"type":"text"}}]`,
		"web_fetch 结果不是 document":      `[{"type":"web_fetch_tool_result","tool_use_id":"t","content":{"type":"web_fetch_result","url":"u","content":{"type":"text","text":"x"}}}]`,
		"web_fetch document 缺少 source": `[{"type":"web_fetch_tool_result","tool_use_id":"t","content":{"type":"web_fetch_result","url":"u","content":{"type":"document"}}}]`,
	} {
		if _, err := anthropicParts(json.RawMessage(block)); err == nil {
			t.Fatalf("%s 应返回错误", name)
		}
	}
}

// TestFunctionResultMediaDepthLimit 工具结果沿 {"content": ...} 查找附件最多 functionResultMediaMaxDepth 层：
// 常见的一两层照常取出图片，更深的嵌套原样按 JSON 发送，不再按层数平方耗费内存
func TestFunctionResultMediaDepthLimit(t *testing.T) {
	image := `[{"type":"image","data":"iVBORw0KGgo=","mimeType":"image/png"}]`
	cleaned, media, err := splitFunctionResultMedia(json.RawMessage(nestedJSON(2, `{"content":`, image, `}`)))
	if err != nil || len(media) != 1 || strings.Contains(string(cleaned), "iVBORw0KGgo") {
		t.Fatalf("两层嵌套应取出图片: media=%d cleaned=%s err=%v", len(media), cleaned, err)
	}
	_, media, err = splitFunctionResultMedia(json.RawMessage(nestedJSON(functionResultMediaMaxDepth, `{"content":`, image, `}`)))
	if err != nil || len(media) != 1 {
		t.Fatalf("上限以内应取出图片: media=%d err=%v", len(media), err)
	}
	tooDeep := json.RawMessage(nestedJSON(functionResultMediaMaxDepth+1, `{"content":`, image, `}`))
	cleaned, media, err = splitFunctionResultMedia(tooDeep)
	if err != nil || len(media) != 0 || string(cleaned) != string(tooDeep) {
		t.Fatalf("超过上限应原样返回: media=%d err=%v", len(media), err)
	}
	deep := json.RawMessage(nestedJSON(5000, `{"content":`, image, `}`))
	allocated := allocatedBytes(func() { cleaned, media, err = splitFunctionResultMedia(deep) })
	if err != nil || len(media) != 0 || string(cleaned) != string(deep) {
		t.Fatalf("深层嵌套应原样返回: media=%d err=%v", len(media), err)
	}
	if allocated > 64<<20 {
		t.Fatalf("%d 字节的工具结果分配了 %d 字节", len(deep), allocated)
	}
}
