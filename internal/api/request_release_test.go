package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// releaseMediaBytes 为释放测试里内联媒体解码后的大小；base64 原文多三分之一。取得足够大，堆上的其他分配（含之前测试留下的后台
// goroutine）远小于判定用的余量（原文的一半，4 MiB）
const releaseMediaBytes = 6 << 20

// releaseWait 为等生成服务发出第一个事件、等请求结束的上限：-race 下解析与扫描几 MB 的请求体要慢十倍以上
const releaseWait = 2 * time.Minute

// heldRequestService 发出一个文本事件后停住，直到 release 关闭才结束，用来在请求还没结束时测量常驻内存。
// retain 为真时像真实生成服务一样在生成期间持有生成请求（解码后的媒体一定常驻，测量只看原文有没有放掉）；
// 为假时不持有，测量的是协议层自己还留着的内容
type heldRequestService struct {
	modelListService
	retain  bool
	held    []aistudio.Content
	started chan struct{}
	release chan struct{}
}

func newHeldRequestService(retain bool) *heldRequestService {
	return &heldRequestService{retain: retain, started: make(chan struct{}), release: make(chan struct{})}
}

func (service *heldRequestService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	if service.retain {
		service.held = request.Contents
	}
	events := make(chan aistudio.Event)
	go func() {
		defer close(events)
		select {
		case events <- aistudio.Event{Kind: aistudio.EventText, Text: "ok"}:
		case <-ctx.Done():
			return
		}
		close(service.started)
		select {
		case <-service.release:
		case <-ctx.Done():
			return
		}
		select {
		case events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}:
		case <-ctx.Done():
		}
	}()
	return events, nil
}

// repeatReader 读出 n 个相同的字节，拼出很大的请求体而不在测试里另外持有一份
type repeatReader struct {
	value byte
	n     int
}

func (reader *repeatReader) Read(buffer []byte) (int, error) {
	if reader.n == 0 {
		return 0, io.EOF
	}
	count := min(len(buffer), reader.n)
	for index := range buffer[:count] {
		buffer[index] = reader.value
	}
	reader.n -= count
	return count, nil
}

// liveHeap 返回两次完整 GC 之后仍然存活的堆内存
func liveHeap() int64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return int64(stats.HeapAlloc)
}

// heldDuringRequest 发出一次请求体为 prefix + base64 媒体（releaseMediaBytes 字节的 0）+ suffix 的请求，在生成服务发出第一个事件、
// 请求还没结束时测量比请求前多常驻的堆内存，之后放行请求并确认它成功结束
func heldDuringRequest(t *testing.T, service *heldRequestService, path string, prefix string, suffix string, header map[string]string) int64 {
	t.Helper()
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	body := io.MultiReader(strings.NewReader(prefix), &repeatReader{value: 'A', n: releaseMediaBytes / 3 * 4}, strings.NewReader(suffix))
	request := httptest.NewRequest(http.MethodPost, path, body)
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("Content-Type", "application/json")
	for name, value := range header {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	before := liveHeap()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(recorder, request)
	}()
	select {
	case <-service.started:
	case <-done:
		t.Fatalf("请求没有走到生成：status=%d body=%.300s", recorder.Code, recorder.Body.String())
	case <-time.After(releaseWait):
		t.Fatal("等不到生成服务发出第一个事件")
	}
	held := liveHeap() - before
	close(service.release)
	select {
	case <-done:
	case <-time.After(releaseWait):
		t.Fatal("请求没有结束")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%.300s", recorder.Code, recorder.Body.String())
	}
	t.Logf("请求进行中多常驻 %.1f MiB（媒体解码后 %.1f MiB，base64 原文 %.1f MiB）", float64(held)/(1<<20),
		float64(releaseMediaBytes)/(1<<20), float64(releaseMediaBytes/3*4)/(1<<20))
	return held
}

// TestConvertedRequestReleasesOriginal 各协议把客户端请求转成生成请求之后放掉原始消息与输入：生成期间（流式请求在流写出期间，Anthropic
// 非流式在等生成结束期间）不再同时常驻内联媒体的 base64 原文。生成服务持有生成请求，解码后的媒体一定常驻；原文还在时多出三分之一以上
func TestConvertedRequestReleasesOriginal(t *testing.T) {
	limit := int64(releaseMediaBytes + releaseMediaBytes/3*4/2)
	for _, test := range []struct {
		name, path, prefix, suffix string
		header                     map[string]string
	}{
		{
			name: "Chat", path: "/v1/chat/completions",
			prefix: `{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,`,
			suffix: `"}}]}]}`,
		},
		{
			name: "ChatNonStream", path: "/v1/chat/completions",
			prefix: `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,`,
			suffix: `"}}]}]}`,
		},
		{
			// 回传历史里 assistant 正文内联的 Markdown 图片拆成图片 part（PNG 文件头之后全是 0），拆出的文本不再引用整段原文
			name: "ChatAssistantImage", path: "/v1/chat/completions",
			prefix: `{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":"draw"},{"role":"assistant","content":"here ![image](data:image/png;base64,iVBORw0KGgoA`,
			suffix: `) done"},{"role":"user","content":"again"}]}`,
		},
		{
			name: "Anthropic", path: "/v1/messages", header: map[string]string{"Anthropic-Version": "2023-06-01"},
			prefix: `{"model":"gemini-2.5-flash","max_tokens":16,"system":"be brief","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`,
			suffix: `"}},{"type":"text","text":"look"}]}]}`,
		},
		{
			name: "Responses", path: "/v1/responses",
			prefix: `{"model":"gemini-2.5-flash","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,`,
			suffix: `"}]}]}`,
		},
		{
			name: "Interactions", path: "/v1beta/interactions",
			prefix: `{"model":"test-model","stream":true,"input":[{"type":"user_input","content":[{"type":"text","text":"look"},{"type":"image","mime_type":"image/png","data":"`,
			suffix: `"}]}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newHeldRequestService(true)
			if held := heldDuringRequest(t, service, test.path, test.prefix, test.suffix, test.header); held > limit {
				t.Fatalf("生成期间多常驻 %d 字节，超过 %d：原始请求里的 base64 原文没有放掉", held, limit)
			}
			// 转换结果不变：媒体照样解码成内联附件交给生成服务
			if !hasInlineMedia(service.held, releaseMediaBytes) {
				t.Fatal("生成请求里没有解码后的内联媒体")
			}
		})
	}
}

// hasInlineMedia 判断 contents 里有不小于 size 字节的内联附件
func hasInlineMedia(contents []aistudio.Content, size int) bool {
	for _, content := range contents {
		for _, part := range content.Parts {
			if part.InlineData != nil && len(part.InlineData.Data) >= size {
				return true
			}
		}
	}
	return false
}

// TestResponsesStoreFalseKeepsNoCopy store:false（Codex 的默认写法）时 Responses 不再复制本轮输入：生成服务不持有请求时，流写出期间
// 协议层不再留着引用内联媒体的副本；store 缺省（保存续接状态）时照旧复制，流结束后存进续接状态
func TestResponsesStoreFalseKeepsNoCopy(t *testing.T) {
	const prefix = `{"model":"gemini-2.5-flash","stream":true,%s"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,`
	const suffix = `"}]}]}`
	notStored := heldDuringRequest(t, newHeldRequestService(false), "/v1/responses", strings.Replace(prefix, "%s", `"store":false,`, 1), suffix, nil)
	stored := heldDuringRequest(t, newHeldRequestService(false), "/v1/responses", strings.Replace(prefix, "%s", "", 1), suffix, nil)
	if notStored > releaseMediaBytes/2 {
		t.Fatalf("store:false 时流写出期间多常驻 %d 字节：本轮输入的副本还在", notStored)
	}
	// 对照：保存续接状态时副本引用解码后的媒体，常驻量接近媒体大小（说明上面的测量确实测得到副本）
	if stored < releaseMediaBytes*3/4 {
		t.Fatalf("保存续接状态时流写出期间只多常驻 %d 字节，测量没有测到本轮输入的副本", stored)
	}
}

// TestGeminiInlineDataReleasedAfterDecode Gemini 原生请求转换时解码 inlineData 之后放掉原始请求里的 base64 原文（含函数结果附带的媒体），
// 转换结果不变
func TestGeminiInlineDataReleasedAfterDecode(t *testing.T) {
	var request geminiRequest
	body := `{"contents":[{"role":"user","parts":[{"text":"look"},{"inlineData":{"mimeType":"image/png","data":"AAECAw=="}}]},
		{"role":"model","parts":[{"functionCall":{"id":"c1","name":"shot","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"shot","response":{"ok":true},"parts":[{"inlineData":{"mimeType":"image/png","data":"BAUGBw=="}}]}}]}]}`
	if err := decodeJSON(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), &request); err != nil {
		t.Fatal(err)
	}
	generate, err := request.toGenerateRequest("resp_test", "gemini-2.5-flash")
	if err != nil {
		t.Fatal(err)
	}
	if data := request.Contents[0].Parts[1].InlineData.Data; data != "" {
		t.Fatalf("inlineData.data 解码后没有放掉：%q", data)
	}
	if data := request.Contents[2].Parts[0].FunctionResponse.Parts[0].InlineData.Data; data != "" {
		t.Fatalf("functionResponse.parts 的 inlineData.data 解码后没有放掉：%q", data)
	}
	image := generate.Contents[0].Parts[1].InlineData
	if image == nil || image.MIME != "image/png" || string(image.Data) != "\x00\x01\x02\x03" {
		t.Fatalf("内联图片转换结果变了：%+v", image)
	}
	var attached *aistudio.Blob
	for _, content := range generate.Contents {
		for _, part := range content.Parts {
			if part.FromToolResult && part.InlineData != nil {
				attached = part.InlineData
			}
		}
	}
	if attached == nil || string(attached.Data) != "\x04\x05\x06\x07" {
		t.Fatalf("函数结果附带的媒体转换结果变了：%+v", generate.Contents)
	}
}
