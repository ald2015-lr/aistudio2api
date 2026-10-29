package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/mediastore"
)

// 测试用的媒体内容：视频以 ftypmp42 开头，图片为 PNG 文件头，音频为两段 24kHz 的 PCM
var (
	testVideo = append([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), bytes.Repeat([]byte{0x5a, 0xa5}, 4096)...)
	testImage = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x11}, 512)...)
	testPCM1  = bytes.Repeat([]byte{0x01, 0x02}, 300)
	testPCM2  = bytes.Repeat([]byte{0x03, 0x04}, 200)
)

const testPCMMIME = "audio/L16;codec=pcm;rate=24000"

// mediaEvents 为一段正文后依次生成视频、图片与两段 PCM 音频的事件流；视频带签名
func mediaEvents() []aistudio.Event {
	return []aistudio.Event{
		{Kind: aistudio.EventReasoning, Text: "先想一想"},
		{Kind: aistudio.EventText, Text: "Here:"},
		{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: "video/mp4", Data: testVideo}, ThoughtSignature: "sig-video"},
		{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: "image/png", Data: testImage}},
		{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: testPCMMIME, Data: testPCM1}},
		{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: testPCMMIME, Data: testPCM2}},
		{Kind: aistudio.EventFinish, FinishReason: "stop"},
	}
}

// mediaSettingsStub 为可在测试中切换的媒体设置
type mediaSettingsStub struct {
	enabled bool
	base    string
}

func (stub *mediaSettingsStub) links() (bool, string) { return stub.enabled, stub.base }

// newMediaHandler 返回带媒体目录的公开 API：service 按预设事件生成，settings 为媒体设置
func newMediaHandler(t *testing.T, service aistudio.Service, settings *mediaSettingsStub) (http.Handler, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "media")
	return NewHandler(service, Config{APIKey: "sk-test", MediaDir: dir, MediaLinks: settings.links}), dir
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// expectedWAV 为两段 PCM 合并后包装成的 WAV
func expectedWAV() []byte {
	return pcmWAV(append(append([]byte(nil), testPCM1...), testPCM2...), 24000, 1)
}

// mediaLinks 返回 base 下三个媒体的链接
func mediaLinks(base string) (video string, image string, audio string) {
	return base + "/media/" + sha256Hex(testVideo) + ".mp4", base + "/media/" + sha256Hex(testImage) + ".png",
		base + "/media/" + sha256Hex(expectedWAV()) + ".wav"
}

// fetchMedia 不带 API Key 读取链接，返回状态、响应头与正文
func fetchMedia(t *testing.T, handler http.Handler, link string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, link, nil)
	for name, value := range header {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// assertSavedMedia 确认三个链接都能不带 API Key 取回与生成内容相同的字节
func assertSavedMedia(t *testing.T, handler http.Handler, base string) {
	t.Helper()
	video, image, audio := mediaLinks(base)
	for _, item := range []struct {
		link        string
		contentType string
		data        []byte
	}{{video, "video/mp4", testVideo}, {image, "image/png", testImage}, {audio, "audio/wav", expectedWAV()}} {
		recorder := fetchMedia(t, handler, item.link, nil)
		if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != item.contentType ||
			!bytes.Equal(recorder.Body.Bytes(), item.data) {
			t.Fatalf("%s: 状态=%d 类型=%q 长度=%d", item.link, recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Len())
		}
	}
}

// sseData 返回 SSE 正文里每个 data 行解析出的 JSON（跳过 [DONE]）
func sseData(t *testing.T, body string) []map[string]any {
	t.Helper()
	var values []map[string]any
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			t.Fatalf("SSE 数据不是 JSON: %q", data)
		}
		values = append(values, value)
	}
	return values
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// TestChatMediaOutput Chat Completions：inline 与原先一样内联 data URI、不写文件；url 模式下正文为本服务链接
// （图片 ![image]、视频 [video]、音频 [audio]，PCM 合并为一个 WAV），流式与非流式正文一致，链接不带 API Key 能取回相同字节
func TestChatMediaOutput(t *testing.T) {
	body := `{"model":"gemini-omni-1.1-flash","messages":[{"role":"user","content":"做个视频"}]}`
	streamBody := `{"model":"gemini-omni-1.1-flash","stream":true,"messages":[{"role":"user","content":"做个视频"}]}`

	t.Run("inline", func(t *testing.T) {
		handler, dir := newMediaHandler(t, &scriptedService{events: mediaEvents()}, &mediaSettingsStub{})
		recorder := postJSON(t, handler, "/v1/chat/completions", body, nil)
		var response struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || len(response.Choices) != 1 {
			t.Fatalf("响应: %s", recorder.Body.String())
		}
		content := response.Choices[0].Message.Content
		want := "Here:\n[media](data:video/mp4;base64," + base64.StdEncoding.EncodeToString(testVideo) + ")\n![media](data:image/png;base64," +
			base64.StdEncoding.EncodeToString(testImage) + ")\n[media](data:" + testPCMMIME + ";base64," +
			base64.StdEncoding.EncodeToString(append(append([]byte(nil), testPCM1...), testPCM2...)) + ")"
		if content != want {
			t.Fatalf("inline 正文与原先不同:\n%.300s", content)
		}
		if countFiles(t, dir) != 0 {
			t.Fatal("inline 模式不应写媒体文件")
		}
	})

	for _, test := range []struct {
		name   string
		base   string
		header map[string]string
		want   string
	}{
		{name: "设置的服务地址", base: "https://ai.example.com/", want: "https://ai.example.com"},
		{name: "请求地址", want: "http://example.com"},
		{name: "反向代理头", header: map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "public.example.org"}, want: "https://public.example.org"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := newMediaHandler(t, &scriptedService{events: mediaEvents()}, &mediaSettingsStub{enabled: true, base: test.base})
			video, image, audio := mediaLinks(test.want)
			want := "Here:\n[video](" + video + ")\n![image](" + image + ")\n[audio](" + audio + ")"
			recorder := postJSON(t, handler, "/v1/chat/completions", body, test.header)
			var response struct {
				Choices []struct {
					Message struct {
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || len(response.Choices) != 1 {
				t.Fatalf("响应: %s", recorder.Body.String())
			}
			if got := response.Choices[0].Message.Content; got != want || response.Choices[0].Message.ReasoningContent != "先想一想" {
				t.Fatalf("非流式正文 = %q，期望 %q", got, want)
			}
			assertSavedMedia(t, handler, test.want)

			stream := postJSON(t, handler, "/v1/chat/completions", streamBody, test.header)
			var content strings.Builder
			for _, chunk := range sseData(t, stream.Body.String()) {
				for _, choice := range chunk["choices"].([]any) {
					if text, ok := choice.(map[string]any)["delta"].(map[string]any)["content"].(string); ok {
						content.WriteString(text)
					}
				}
			}
			if content.String() != want {
				t.Fatalf("流式正文 = %q，期望 %q", content.String(), want)
			}
		})
	}
}

// TestAnthropicMediaOutput Messages：url 模式下媒体各成一个 text block，内容为本服务链接；流式 text_delta 同样是链接；inline 不变
func TestAnthropicMediaOutput(t *testing.T) {
	header := map[string]string{"Anthropic-Version": "2023-06-01"}
	body := `{"model":"gemini-omni-1.1-flash","max_tokens":64,"messages":[{"role":"user","content":"做个视频"}]}`
	streamBody := `{"model":"gemini-omni-1.1-flash","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"做个视频"}]}`

	inlineHandler, inlineDir := newMediaHandler(t, &scriptedService{events: mediaEvents()}, &mediaSettingsStub{})
	recorder := postJSON(t, inlineHandler, "/v1/messages", body, header)
	if !strings.Contains(recorder.Body.String(), `[media](data:video/mp4;base64,`) || countFiles(t, inlineDir) != 0 {
		t.Fatalf("inline 应与原先一样内联: %.300s", recorder.Body.String())
	}

	handler, _ := newMediaHandler(t, &scriptedService{events: mediaEvents()}, &mediaSettingsStub{enabled: true, base: "https://ai.example.com"})
	video, image, audio := mediaLinks("https://ai.example.com")
	recorder = postJSON(t, handler, "/v1/messages", body, header)
	var response struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("响应: %s", recorder.Body.String())
	}
	var texts []string
	for _, block := range response.Content {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	want := []string{"Here:", "[video](" + video + ")", "![image](" + image + ")", "[audio](" + audio + ")"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("text block = %q，期望 %q", texts, want)
	}
	assertSavedMedia(t, handler, "https://ai.example.com")

	stream := postJSON(t, handler, "/v1/messages", streamBody, header)
	var deltas []string
	for _, event := range sseData(t, stream.Body.String()) {
		if event["type"] != "content_block_delta" {
			continue
		}
		if delta := event["delta"].(map[string]any); delta["type"] == "text_delta" {
			deltas = append(deltas, delta["text"].(string))
		}
	}
	if strings.Join(deltas, "|") != strings.Join(want, "|") {
		t.Fatalf("流式 text_delta = %q，期望 %q", deltas, want)
	}
}

// geminiParts 返回 Gemini 响应（或流式块）里全部 part
func geminiResponseParts(t *testing.T, response map[string]any) []map[string]any {
	t.Helper()
	var parts []map[string]any
	candidates, _ := response["candidates"].([]any)
	for _, candidate := range candidates {
		content, _ := candidate.(map[string]any)["content"].(map[string]any)
		values, _ := content["parts"].([]any)
		for _, value := range values {
			parts = append(parts, value.(map[string]any))
		}
	}
	return parts
}

// expectedGeminiLinkParts 为 url 模式下媒体对应的 part：链接文本在前、fileData 在后，签名在 fileData 上
func expectedGeminiLinkParts(base string) []map[string]any {
	video, image, audio := mediaLinks(base)
	return []map[string]any{
		{"text": "\n[video](" + video + ")\n"},
		{"fileData": map[string]any{"mimeType": "video/mp4", "fileUri": video}, "thoughtSignature": "sig-video"},
		{"text": "![image](" + image + ")\n"},
		{"fileData": map[string]any{"mimeType": "image/png", "fileUri": image}},
		{"text": "[audio](" + audio + ")\n"},
		{"fileData": map[string]any{"mimeType": "audio/wav", "fileUri": audio}},
	}
}

// TestGeminiMediaOutput Gemini 原生接口（含 /ultra 前缀）：inline 与原先一样输出 inlineData；url 模式下每个媒体为链接文本与
// fileData 两个 part，流式时两者在同一块里、文本在前；链接能取回相同字节
func TestGeminiMediaOutput(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"text":"做个视频"}]}]}`

	inlineHandler, inlineDir := newMediaHandler(t, &scriptedService{events: mediaEvents()}, &mediaSettingsStub{})
	recorder := postJSON(t, inlineHandler, "/v1beta/models/gemini-omni-1.1-flash:generateContent", body, nil)
	var inline map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &inline); err != nil {
		t.Fatalf("响应: %s", recorder.Body.String())
	}
	parts := geminiResponseParts(t, inline)
	if len(parts) != 5 || parts[2]["inlineData"].(map[string]any)["data"] != base64.StdEncoding.EncodeToString(testVideo) ||
		parts[2]["thoughtSignature"] != "sig-video" || parts[3]["inlineData"] == nil || parts[4]["inlineData"] == nil {
		t.Fatalf("inline parts 与原先不同: %v", recorder.Body.String()[:300])
	}
	if countFiles(t, inlineDir) != 0 {
		t.Fatal("inline 模式不应写媒体文件")
	}

	for _, path := range []string{"/v1beta/models/gemini-omni-1.1-flash", "/ultra/v1beta/models/gemini-omni-1.1-flash"} {
		handler, _ := newMediaHandler(t, &scriptedService{events: mediaEvents()}, &mediaSettingsStub{enabled: true, base: "https://ai.example.com"})
		recorder := postJSON(t, handler, path+":generateContent", body, nil)
		var response map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("%s 响应: %s", path, recorder.Body.String())
		}
		want := append([]map[string]any{{"text": "先想一想", "thought": true}, {"text": "Here:"}}, expectedGeminiLinkParts("https://ai.example.com")...)
		if got := mustJSONText(t, geminiResponseParts(t, response)); got != mustJSONText(t, want) {
			t.Fatalf("%s 非流式 parts:\n%s\n期望:\n%s", path, got, mustJSONText(t, want))
		}
		assertSavedMedia(t, handler, "https://ai.example.com")

		stream := postJSON(t, handler, path+":streamGenerateContent?alt=sse", body, nil)
		var streamed []map[string]any
		mediaChunks := 0
		for _, chunk := range sseData(t, stream.Body.String()) {
			chunkParts := geminiResponseParts(t, chunk)
			if len(chunkParts) == 2 {
				mediaChunks++
				if _, ok := chunkParts[0]["text"]; !ok || chunkParts[1]["fileData"] == nil {
					t.Fatalf("%s 媒体块应文本在前、fileData 在后: %v", path, chunkParts)
				}
			}
			streamed = append(streamed, chunkParts...)
		}
		if mediaChunks != 3 || mustJSONText(t, streamed) != mustJSONText(t, want) {
			t.Fatalf("%s 流式 parts:\n%s", path, mustJSONText(t, streamed))
		}
	}
}

func mustJSONText(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestMediaSaveFailureFallsBackInline 媒体目录不可写时本次改为内联输出，请求照常成功
func TestMediaSaveFailureFallsBackInline(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(&scriptedService{events: mediaEvents()}, Config{
		APIKey: "sk-test", MediaDir: filepath.Join(blocker, "media"), MediaLinks: (&mediaSettingsStub{enabled: true}).links,
	})
	recorder := postJSON(t, handler, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "data:video/mp4;base64,") {
		t.Fatalf("保存失败应改为内联: %d %.200s", recorder.Code, recorder.Body.String())
	}
}

// TestMediaRoute /media/ 路由：不需要 API Key；只提供名字合法的普通文件，Content-Type 按扩展名、支持 Range 与 HEAD、
// 带 immutable 缓存头；路径穿越、不合法的名字、目录、符号链接与写方法都拒绝；不是 url 模式或没有设置函数时一律 404
func TestMediaRoute(t *testing.T) {
	settings := &mediaSettingsStub{enabled: true}
	handler, dir := newMediaHandler(t, modelListService{}, settings)
	store := mediastore.New(dir)
	name, err := store.Save(testVideo, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	link := "/media/" + name

	recorder := fetchMedia(t, handler, link, nil)
	header := recorder.Header()
	if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), testVideo) || header.Get("Content-Type") != "video/mp4" ||
		header.Get("Cache-Control") != "public, max-age=31536000, immutable" || header.Get("Accept-Ranges") != "bytes" ||
		header.Get("ETag") != `"`+sha256Hex(testVideo)+`"` || header.Get("X-Content-Type-Options") != "nosniff" ||
		header.Get("Content-Disposition") != "" {
		t.Fatalf("GET: %d %v", recorder.Code, header)
	}

	recorder = fetchMedia(t, handler, link, map[string]string{"Range": "bytes=4-11"})
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != "ftypmp42" ||
		recorder.Header().Get("Content-Range") != "bytes 4-11/"+strconv.Itoa(len(testVideo)) {
		t.Fatalf("Range: %d %q %v", recorder.Code, recorder.Body.String(), recorder.Header())
	}
	recorder = fetchMedia(t, handler, link, map[string]string{"If-None-Match": `"` + sha256Hex(testVideo) + `"`})
	if recorder.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", recorder.Code)
	}

	head := httptest.NewRequest(http.MethodHead, link, nil)
	headRecorder := httptest.NewRecorder()
	handler.ServeHTTP(headRecorder, head)
	if headRecorder.Code != http.StatusOK || headRecorder.Body.Len() != 0 || headRecorder.Header().Get("Content-Length") != strconv.Itoa(len(testVideo)) {
		t.Fatalf("HEAD: %d %d %v", headRecorder.Code, headRecorder.Body.Len(), headRecorder.Header())
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		request := httptest.NewRequest(method, link, strings.NewReader("x"))
		methodRecorder := httptest.NewRecorder()
		handler.ServeHTTP(methodRecorder, request)
		if methodRecorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s 应返回 405，得到 %d", method, methodRecorder.Code)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(dir, name)); !bytes.Equal(data, testVideo) {
		t.Fatal("写方法不应改动文件")
	}

	binName, err := store.Save([]byte("<svg onload=alert(1)>"), "image/svg+xml")
	if err != nil {
		t.Fatal(err)
	}
	recorder = fetchMedia(t, handler, "/media/"+binName, nil)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/octet-stream" ||
		recorder.Header().Get("Content-Disposition") != "attachment" {
		t.Fatalf("未知类型应按下载提供: %d %v", recorder.Code, recorder.Header())
	}

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("secret-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkName := strings.Repeat("ab", 32) + ".png"
	symlinked := os.Symlink(secret, filepath.Join(dir, linkName)) == nil
	directory := strings.Repeat("cd", 32) + ".mp4"
	if err := os.Mkdir(filepath.Join(dir, directory), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("secret-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	invalid := []string{
		"/media/", "/media/notes.txt", "/media/" + strings.ToUpper(name), "/media/" + strings.TrimSuffix(name, ".mp4") + ".exe",
		"/media/" + name + "/x", "/media/../media/notes.txt", "/media/%2e%2e/notes.txt", "/media/..%2fnotes.txt",
		"/media/" + directory, "/media/" + strings.Repeat("0", 64) + ".mp4",
	}
	if symlinked {
		invalid = append(invalid, "/media/"+linkName)
	}
	for _, path := range invalid {
		invalidRecorder := httptest.NewRecorder()
		handler.ServeHTTP(invalidRecorder, httptest.NewRequest(http.MethodGet, path, nil))
		if invalidRecorder.Code == http.StatusOK || strings.Contains(invalidRecorder.Body.String(), "secret-content") {
			t.Fatalf("%s 不应提供: %d %q", path, invalidRecorder.Code, invalidRecorder.Body.String())
		}
	}

	settings.enabled = false
	if recorder := fetchMedia(t, handler, link, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("不是 url 模式时应返回 404，得到 %d", recorder.Code)
	}
	noSettings := NewHandler(modelListService{}, Config{APIKey: "sk-test", MediaDir: dir})
	if recorder := fetchMedia(t, noSettings, link, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("没有设置函数时应返回 404，得到 %d", recorder.Code)
	}
	noDir := NewHandler(modelListService{}, Config{APIKey: "sk-test"})
	if recorder := fetchMedia(t, noDir, link, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("没有媒体目录时应返回 404，得到 %d", recorder.Code)
	}
}

// recordingService 记录最后一次生成请求，生成一段正文
type recordingService struct {
	modelListService
	last *aistudio.GenerateRequest
}

func (service *recordingService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	copied := request
	service.last = &copied
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "ok"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}
	close(events)
	return events, nil
}

// describeParts 把 part 写成便于比较的文本：text:内容、inline:类型:sha256 前 8 位、file:ID、external:URL
func describeParts(parts []aistudio.Part) []string {
	var described []string
	for _, part := range parts {
		switch {
		case part.InlineData != nil:
			described = append(described, "inline:"+part.InlineData.MIME+":"+sha256Hex(part.InlineData.Data)[:8])
		case part.File != nil:
			described = append(described, "file:"+part.File.ID)
		case part.ExternalMedia != nil:
			described = append(described, "external:"+part.ExternalMedia.URL)
		case part.Text != "":
			described = append(described, "text:"+part.Text)
		}
	}
	return described
}

// TestRestoreMediaLinks 回传历史里本服务的媒体（inline 模式下同样恢复）：assistant 正文里的图片链接恢复为图片（视频链接保持文本），
// 用户消息里的链接文字不动；用户消息的 image_url 与 fileData 指向已保存的媒体时恢复为内联媒体（含视频）；模型消息的 fileData 只恢复图片，
// 同一条消息里的同一文件只恢复一次；文件已清理时图片链接保持文本、fileData 改为文本链接；不是本服务的地址不动
func TestRestoreMediaLinks(t *testing.T) {
	settings := &mediaSettingsStub{}
	service := &recordingService{}
	handler, dir := newMediaHandler(t, service, settings)
	store := mediastore.New(dir)
	secondImage := append(append([]byte(nil), testImage...), 0x22)
	imageName, _ := store.Save(testImage, "image/png")
	secondName, _ := store.Save(secondImage, "image/png")
	videoName, _ := store.Save(testVideo, "video/mp4")
	imageLink := "https://ai.example.com/media/" + imageName
	secondLink := "https://ai.example.com/media/" + secondName
	videoLink := "https://ai.example.com/media/" + videoName
	missingLink := "https://ai.example.com/media/" + strings.Repeat("e", 64) + ".png"
	imageHash, secondHash, videoHash := sha256Hex(testImage)[:8], sha256Hex(secondImage)[:8], sha256Hex(testVideo)[:8]

	chat := map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": "看看 ![image](" + imageLink + ")"},
		map[string]any{"role": "assistant", "content": "图在这：\n![image](" + imageLink + ")\n视频：[video](" + videoLink + ")\n旧图 ![image](" + missingLink + ")"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "再改一下"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": secondLink}},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://images.example.net/cat.png"}},
		}},
	}}
	recorder := postJSON(t, handler, "/v1/chat/completions", mustJSONText(t, chat), nil)
	if recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("Chat: %d %s", recorder.Code, recorder.Body.String())
	}
	contents := service.last.Contents
	if got := describeParts(contents[0].Parts); strings.Join(got, "|") != "text:看看 ![image]("+imageLink+")" {
		t.Fatalf("用户消息里的链接文字不应改动: %q", got)
	}
	want := []string{"text:图在这：\n", "inline:image/png:" + imageHash, "text:\n视频：[video](" + videoLink + ")\n旧图 ![image](" + missingLink + ")"}
	if got := describeParts(contents[1].Parts); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("assistant 正文恢复 = %q，期望 %q", got, want)
	}
	want = []string{"text:再改一下", "inline:image/png:" + secondHash, "external:https://images.example.net/cat.png"}
	if got := describeParts(contents[2].Parts); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("image_url 恢复 = %q，期望 %q", got, want)
	}

	gemini := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "做个视频"}}},
		map[string]any{"role": "model", "parts": []any{
			map[string]any{"text": "![image](" + imageLink + ")\n"},
			map[string]any{"fileData": map[string]any{"mimeType": "image/png", "fileUri": imageLink}, "thoughtSignature": "sig-image"},
			map[string]any{"text": "\n[video](" + videoLink + ")\n"},
			map[string]any{"fileData": map[string]any{"mimeType": "video/mp4", "fileUri": videoLink}, "thoughtSignature": "sig-video"},
			map[string]any{"fileData": map[string]any{"mimeType": "image/png", "fileUri": missingLink}},
		}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "再来一个"}}},
		map[string]any{"role": "model", "parts": []any{
			map[string]any{"fileData": map[string]any{"mimeType": "video/mp4", "fileUri": videoLink}},
		}},
		map[string]any{"role": "user", "parts": []any{
			map[string]any{"text": "描述这个视频"},
			map[string]any{"fileData": map[string]any{"mimeType": "video/mp4", "fileUri": "/media/" + videoName}},
		}},
	}}
	recorder = postJSON(t, handler, "/v1beta/models/m:generateContent", mustJSONText(t, gemini), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("Gemini: %d %s", recorder.Code, recorder.Body.String())
	}
	contents = service.last.Contents
	// 模型消息：图片 fileData 恢复为图片（同一文件的文本链接不再重复恢复）；视频 fileData 不恢复（模型不需要把自己生成的视频再当输入），
	// 已有文本链接时只留签名；已清理的图片 fileData 改为文本链接
	want = []string{
		"text:![image](" + imageLink + ")\n", "inline:image/png:" + imageHash, "text:\n[video](" + videoLink + ")\n",
		"text:![image](" + missingLink + ")",
	}
	if got := describeParts(contents[1].Parts); strings.Join(got, "|") != strings.Join(want, "|") || len(contents[1].Parts) != 5 {
		t.Fatalf("Gemini 模型消息恢复 = %q，期望 %q", got, want)
	}
	if contents[1].Parts[1].ThoughtSignature != "sig-image" {
		t.Fatal("恢复的 part 应保留签名")
	}
	if signature := contents[1].Parts[3]; signature.ThoughtSignature != "sig-video" || signature.Text != "" || signature.File != nil || signature.InlineData != nil {
		t.Fatalf("去掉的视频 fileData 应只留签名: %+v", signature)
	}
	if got := describeParts(contents[3].Parts); strings.Join(got, "|") != "text:[video]("+videoLink+")" {
		t.Fatalf("没有文本链接的视频 fileData 应改为文本链接: %q", got)
	}
	if got := describeParts(contents[4].Parts); strings.Join(got, "|") != "text:描述这个视频|inline:video/mp4:"+videoHash {
		t.Fatalf("用户 fileData 恢复 = %q", got)
	}

	anthropic := map[string]any{"model": "m", "max_tokens": 16, "messages": []any{
		map[string]any{"role": "user", "content": "画一张"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "![image](" + imageLink + ")"}}},
		map[string]any{"role": "user", "content": "再来"},
	}}
	recorder = postJSON(t, handler, "/v1/messages", mustJSONText(t, anthropic), map[string]string{"Anthropic-Version": "2023-06-01"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("Messages: %d %s", recorder.Code, recorder.Body.String())
	}
	if got := describeParts(service.last.Contents[1].Parts); strings.Join(got, "|") != "inline:image/png:"+imageHash {
		t.Fatalf("Messages assistant 恢复 = %q", got)
	}
}

// TestRestoreMediaLinksModelVideo 只有模型消息引用生成的视频（没有用户消息引用同一文件）：模型消息里的视频 fileData 不恢复为内联，
// 同一条消息里已有文本链接时只留签名，没有时改为文本链接；视频不占恢复额度
func TestRestoreMediaLinksModelVideo(t *testing.T) {
	service := &recordingService{}
	handler, dir := newMediaHandler(t, service, &mediaSettingsStub{})
	videoName, err := mediastore.New(dir).Save(testVideo, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	videoLink := "https://ai.example.com/media/" + videoName
	for _, test := range []struct {
		name  string
		parts []any
		want  []string
	}{
		{
			name: "有文本链接",
			parts: []any{
				map[string]any{"text": "\n[video](" + videoLink + ")\n"},
				map[string]any{"fileData": map[string]any{"mimeType": "video/mp4", "fileUri": videoLink}, "thoughtSignature": "sig-video"},
			},
			want: []string{"text:\n[video](" + videoLink + ")\n"},
		},
		{
			name: "没有文本链接",
			parts: []any{
				map[string]any{"fileData": map[string]any{"mimeType": "video/mp4", "fileUri": videoLink}, "thoughtSignature": "sig-video"},
			},
			want: []string{"text:[video](" + videoLink + ")"},
		},
	} {
		gemini := map[string]any{"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "做个视频"}}},
			map[string]any{"role": "model", "parts": test.parts},
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "再长一点"}}},
		}}
		service.last = nil
		recorder := postJSON(t, handler, "/v1beta/models/m:generateContent", mustJSONText(t, gemini), nil)
		if recorder.Code != http.StatusOK || service.last == nil {
			t.Fatalf("%s: %d %s", test.name, recorder.Code, recorder.Body.String())
		}
		contents := service.last.Contents
		if total, count := inlineBytes(contents); total != 0 || count != 0 {
			t.Fatalf("%s: 模型消息里的视频不应恢复为内联（%d 个、%d 字节）", test.name, count, total)
		}
		if got := describeParts(contents[1].Parts); strings.Join(got, "|") != strings.Join(test.want, "|") {
			t.Fatalf("%s: 模型消息 = %q，期望 %q", test.name, got, test.want)
		}
		signed := 0
		for _, part := range contents[1].Parts {
			if part.File != nil || part.ExternalMedia != nil {
				t.Fatalf("%s: 视频 fileData 应去掉或改为文本链接: %+v", test.name, part)
			}
			if part.ThoughtSignature == "sig-video" {
				signed++
			}
		}
		if signed != 1 {
			t.Fatalf("%s: 签名应保留一次，实际 %d 次", test.name, signed)
		}
	}
}

// countingService 记录最后一次计数请求
type countingService struct {
	modelListService
	last *aistudio.TokenCountRequest
}

func (service *countingService) CountTokens(_ context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	copied := request
	service.last = &copied
	return aistudio.TokenCount{InputTokens: 42}, nil
}

// TestCountTokensRestoreMediaLinks Messages 的 count_tokens 与 Gemini 的 countTokens 同样恢复回传历史里本服务的图片链接，
// 计数与实际生成时发送的内容一致
func TestCountTokensRestoreMediaLinks(t *testing.T) {
	service := &countingService{}
	handler, dir := newMediaHandler(t, service, &mediaSettingsStub{})
	imageName, err := mediastore.New(dir).Save(testImage, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	imageLink := "https://ai.example.com/media/" + imageName
	want := "inline:image/png:" + sha256Hex(testImage)[:8]

	anthropic := map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": "画一张"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "![image](" + imageLink + ")"}}},
		map[string]any{"role": "user", "content": "再来"},
	}}
	recorder := postJSON(t, handler, "/v1/messages/count_tokens", mustJSONText(t, anthropic), map[string]string{"Anthropic-Version": "2023-06-01"})
	if recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("count_tokens: %d %s", recorder.Code, recorder.Body.String())
	}
	if got := describeParts(service.last.Contents[1].Parts); strings.Join(got, "|") != want {
		t.Fatalf("count_tokens 应恢复 assistant 图片链接: %q", got)
	}

	service.last = nil
	gemini := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "画一张"}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"text": "![image](" + imageLink + ")"}}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "再来"}}},
	}}
	recorder = postJSON(t, handler, "/v1beta/models/m:countTokens", mustJSONText(t, gemini), nil)
	if recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("countTokens: %d %s", recorder.Code, recorder.Body.String())
	}
	if got := describeParts(service.last.Contents[1].Parts); strings.Join(got, "|") != want {
		t.Fatalf("countTokens 应恢复模型消息里的图片链接: %q", got)
	}
}

// inlineBytes 返回 contents 里内联媒体的合计字节数与 part 个数
func inlineBytes(contents []aistudio.Content) (int64, int) {
	var total int64
	count := 0
	for _, content := range contents {
		for _, part := range content.Parts {
			if part.InlineData != nil {
				total += int64(len(part.InlineData.Data))
				count++
			}
		}
	}
	return total, count
}

// TestRestoreMediaLinksOncePerRequest 同一文件一次请求只恢复一处：几 KB 的请求里引用同一个视频 150 次，只展开一份（其余改为文本链接），
// 恢复总量不超过 maxRestoredMediaBytes；assistant 正文的图片链接与随后用户消息的 image_url 指向同一张图时只恢复较新的用户附件
func TestRestoreMediaLinksOncePerRequest(t *testing.T) {
	service := &recordingService{}
	handler, dir := newMediaHandler(t, service, &mediaSettingsStub{})
	store := mediastore.New(dir)
	video := bytes.Repeat([]byte("ftypmp42"), 450_000)
	videoName, err := store.Save(video, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	imageName, _ := store.Save(testImage, "image/png")
	videoLink := "https://ai.example.com/media/" + videoName
	imageLink := "https://ai.example.com/media/" + imageName

	blocks := []any{map[string]any{"type": "text", "text": "看这些"}}
	for range 150 {
		blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": videoLink}})
	}
	chat := map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": "画一张"},
		map[string]any{"role": "assistant", "content": "![image](" + imageLink + ")"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "改一下"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageLink}},
		}},
		map[string]any{"role": "user", "content": blocks},
	}}
	body := mustJSONText(t, chat)
	if len(body) > 64<<10 {
		t.Fatalf("请求体应只有几 KB，实际 %d 字节", len(body))
	}
	recorder := postJSON(t, handler, "/v1/chat/completions", body, nil)
	if recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("Chat: %d %s", recorder.Code, recorder.Body.String())
	}
	contents := service.last.Contents
	total, count := inlineBytes(contents)
	if total > maxRestoredMediaBytes || total != int64(len(video)+len(testImage)) || count != 2 {
		t.Fatalf("恢复总量 %d 字节、%d 个内联 part，期望只展开一份视频与一张图片（%d 字节）", total, count, len(video)+len(testImage))
	}
	var videoParts []aistudio.Part
	for _, content := range contents {
		for _, part := range content.Parts {
			if part.InlineData != nil && part.InlineData.MIME == "video/mp4" || strings.Contains(part.Text, videoLink) {
				videoParts = append(videoParts, part)
			}
		}
	}
	if len(videoParts) != 150 || videoParts[0].InlineData == nil {
		t.Fatalf("150 处引用应为 1 个内联视频加文本链接，实际 %d 处", len(videoParts))
	}
	for _, part := range videoParts[1:] {
		if part.InlineData != nil || part.ExternalMedia != nil || part.Text != "[video]("+videoLink+")" {
			t.Fatalf("重复引用应改为文本链接: %+v", part)
		}
	}
	if got := describeParts(contents[1].Parts); strings.Join(got, "|") != "text:![image]("+imageLink+")" {
		t.Fatalf("较早的 assistant 图片链接应保持文本: %q", got)
	}
	if got := describeParts(contents[2].Parts); strings.Join(got, "|") != "text:改一下|inline:image/png:"+sha256Hex(testImage)[:8] {
		t.Fatalf("较新的用户附件应恢复为图片: %q", got)
	}
}

// TestRestoreMediaLinksBudget 恢复总量超过上限时从最新的消息往前恢复，较早的引用不恢复：文本链接保持文本、fileData 改为文本链接、
// 外部地址保持原样；大于剩余总量的文件不读取
func TestRestoreMediaLinksBudget(t *testing.T) {
	store := mediastore.New(t.TempDir())
	images := make([][]byte, 4)
	links := make([]string, 4)
	for index := range images {
		images[index] = append(append([]byte(nil), testImage...), bytes.Repeat([]byte{byte(index + 1)}, 1000)...)
		name, err := store.Save(images[index], "image/png")
		if err != nil {
			t.Fatal(err)
		}
		links[index] = "https://ai.example.com/media/" + name
	}
	large := append(append([]byte(nil), testImage...), bytes.Repeat([]byte{0x7f}, 8000)...)
	largeName, _ := store.Save(large, "image/png")
	largeLink := "https://ai.example.com/media/" + largeName
	contents := []aistudio.Content{
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "最早 ![image](" + links[0] + ")"}}},
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{ExternalMedia: &aistudio.ExternalMedia{MIME: "image/png", URL: links[1]}}}},
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{File: &aistudio.FileRef{ID: links[2]}}}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{File: &aistudio.FileRef{ID: largeLink}}, {Text: "![image](" + links[3] + ")"}}},
	}
	limit := int64(len(images[3]) + len(images[2]) + len(images[1]) - 1)
	restoreMediaLinksWithin(store, contents, limit, false)
	if total, count := inlineBytes(contents); total > limit || count != 2 {
		t.Fatalf("恢复 %d 字节、%d 个 part，上限 %d", total, count, limit)
	}
	want := [][]string{
		{"text:最早 ![image](" + links[0] + ")"},
		{"external:" + links[1]},
		{"inline:image/png:" + sha256Hex(images[2])[:8]},
		{"text:![image](" + largeLink + ")", "inline:image/png:" + sha256Hex(images[3])[:8]},
	}
	for index, content := range contents {
		if got := describeParts(content.Parts); strings.Join(got, "|") != strings.Join(want[index], "|") {
			t.Fatalf("contents[%d] = %q，期望 %q", index, got, want[index])
		}
	}
}

// interactionEncodingService 像 Interaction 模型的生成一样先整理图片（GIF 转 PNG，见 aistudio.PrepareInteractionImages），PNG、JPEG 图片
// 保持内联，视频等需要 Drive 的内联附件上传成 Drive 引用（noDrive 为真时像没有 Drive 授权的账户一样保留 inline data），再编码
// CreateInteractionStream 请求，编码失败时按参数错误返回；记下收到的请求、编码出的请求体与 binding
type interactionEncodingService struct {
	modelListService
	noDrive bool
	// events 为编码成功后返回的生成事件，为空时返回一段正文
	events  []aistudio.Event
	last    *aistudio.GenerateRequest
	body    []byte
	binding []aistudio.Content
}

func (service *interactionEncodingService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	copied := request
	service.last = &copied
	uploaded := request
	prepared, _, err := aistudio.PrepareInteractionImages(context.Background(), request.Contents)
	if err != nil {
		return nil, err
	}
	uploaded.Contents = make([]aistudio.Content, len(prepared))
	files := 0
	for index, content := range prepared {
		content.Parts = append([]aistudio.Part(nil), content.Parts...)
		for partIndex, part := range content.Parts {
			if aistudio.InteractionDriveMedia(part) && !service.noDrive {
				files++
				content.Parts[partIndex] = aistudio.Part{
					File:             &aistudio.FileRef{ID: fmt.Sprintf("drive-%d", files), MIME: part.InlineData.MIME},
					ThoughtSignature: part.ThoughtSignature,
				}
			}
		}
		uploaded.Contents[index] = content
	}
	body, binding, err := aistudio.EncodeCreateInteractionStreamRequest(uploaded, aistudio.GenerationDefaults{MaxOutputTokens: 8192, DefaultThinkingLevel: 3})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", aistudio.ErrInvalidArgument, err)
	}
	service.body, service.binding = body, binding
	script := service.events
	if len(script) == 0 {
		script = []aistudio.Event{{Kind: aistudio.EventText, Text: "好的"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}
	}
	events := make(chan aistudio.Event, len(script))
	for _, event := range script {
		events <- event
	}
	close(events)
	return events, nil
}

// interactionModelMediaIntro 与 aistudio 里移到用户 step 的模型附件前的说明一致
const interactionModelMediaIntro = "以下是上文模型回复里的图片或视频："

// TestInteractionHistoryWithModelImage 先用图片模型出图（url 模式下历史里是本服务的图片链接，inline 模式下是 data URI），
// 再在同一对话里让 Interaction 模型（Omni）“让它动起来”：模型消息里恢复出的图片连同说明移到随后的用户 step，按官网内联为
// Content field 2 [1, Base64]（不上传 Drive），模型能看到这张图；模型 step 只带同一条消息里的文本，binding 与发送的文本一致。
// 账户没有 Drive 授权时同样内联发送成功，不再返回 400
func TestInteractionHistoryWithModelImage(t *testing.T) {
	service := &interactionEncodingService{}
	handler, dir := newMediaHandler(t, service, &mediaSettingsStub{enabled: true, base: "https://ai.example.com"})
	imageName, err := mediastore.New(dir).Save(testImage, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	imageLink := "https://ai.example.com/media/" + imageName
	encodedImage := base64.StdEncoding.EncodeToString(testImage)
	movedImage := `[[["` + interactionModelMediaIntro + `"]],[null,[1,"` + encodedImage + `"]],[["让它动起来"]]]`
	wantBinding := `[{"role":"user","parts":[{"text":"画一只猫"}]},{"role":"assistant","parts":[{"text":"\n"}]},` +
		`{"role":"user","parts":[{"text":"` + interactionModelMediaIntro + `"},{"text":"让它动起来"}]}]`

	gemini := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "画一只猫"}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"text": "![image](" + imageLink + ")\n"}}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "让它动起来"}}},
	}}
	for _, method := range []string{"generateContent", "streamGenerateContent?alt=sse"} {
		service.body, service.binding = nil, nil
		recorder := postJSON(t, handler, "/v1beta/models/gemini-omni-1.1-flash:"+method, mustJSONText(t, gemini), nil)
		if recorder.Code != http.StatusOK || service.body == nil {
			t.Fatalf("%s: %d %s", method, recorder.Code, recorder.Body.String())
		}
		if got := describeParts(service.last.Contents[1].Parts); strings.Join(got, "|") != "inline:image/png:"+sha256Hex(testImage)[:8]+"|text:\n" {
			t.Fatalf("%s: 模型消息里的图片链接应照常恢复: %q", method, got)
		}
		if !strings.Contains(string(service.body), movedImage) || strings.Contains(string(service.body), "drive-") {
			t.Fatalf("%s: 模型消息里的图片应内联放在随后的用户 step 最前面、不上传 Drive: %s", method, service.body)
		}
		if got := mustJSONText(t, service.binding); got != wantBinding {
			t.Fatalf("%s: binding = %s", method, got)
		}
	}

	chat := map[string]any{"model": "gemini-omni-1.1-flash", "messages": []any{
		map[string]any{"role": "user", "content": "画一只猫"},
		map[string]any{"role": "assistant", "content": "![image](data:image/png;base64," + encodedImage + ")"},
		map[string]any{"role": "user", "content": "让它动起来"},
	}}
	service.body = nil
	recorder := postJSON(t, handler, "/v1/chat/completions", mustJSONText(t, chat), nil)
	if recorder.Code != http.StatusOK || service.body == nil {
		t.Fatalf("Chat inline 图片历史: %d %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(string(service.body), movedImage) {
		t.Fatalf("Chat: 模型消息里的图片应内联放在随后的用户 step 最前面: %s", service.body)
	}
	if got := mustJSONText(t, service.binding); got != `[{"role":"user","parts":[{"text":"画一只猫"}]},`+
		`{"role":"user","parts":[{"text":"`+interactionModelMediaIntro+`"},{"text":"让它动起来"}]}]` {
		t.Fatalf("只有图片的模型消息应整条省略、图片移到随后的用户 step，binding = %s", got)
	}

	// 账户没有 Drive 授权：图片照样内联发送成功
	service.noDrive = true
	for _, request := range []struct{ path, body string }{
		{path: "/v1beta/models/gemini-omni-1.1-flash:generateContent", body: mustJSONText(t, gemini)},
		{path: "/v1/chat/completions", body: mustJSONText(t, chat)},
	} {
		service.body = nil
		recorder = postJSON(t, handler, request.path, request.body, nil)
		if recorder.Code != http.StatusOK || !strings.Contains(string(service.body), movedImage) {
			t.Fatalf("%s: 没有 Drive 授权时图片应照样内联发送: %d %s", request.path, recorder.Code, recorder.Body.String())
		}
	}
}

// TestInteractionContinuationWithModelVideo Interactions 接口经 previous_interaction_id 续接 Omni 的对话：续接历史里模型生成的视频
// 上传 Drive 后连同说明移到这一轮用户 step 的最前面，模型能看到自己上一轮生成的视频；模型 step 只带正文
func TestInteractionContinuationWithModelVideo(t *testing.T) {
	service := &interactionEncodingService{events: []aistudio.Event{
		{Kind: aistudio.EventText, Text: "视频好了"},
		{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: "video/mp4", Data: testVideo}, ThoughtSignature: "sig-video"},
		{Kind: aistudio.EventFinish, FinishReason: "stop"},
	}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	first := decodeInteraction(t, postJSON(t, handler, "/v1beta/interactions", `{"model":"gemini-omni-1.1-flash","input":"做个视频"}`, nil))
	service.events = nil
	recorder := postJSON(t, handler, "/v1beta/interactions",
		fmt.Sprintf(`{"model":"gemini-omni-1.1-flash","previous_interaction_id":%q,"input":"让它更长"}`, first["id"]), nil)
	decodeInteraction(t, recorder)
	moved := `[[["` + interactionModelMediaIntro + `"]],[null,null,null,null,null,null,null,null,["drive-1"]],[["让它更长"]]]`
	if !strings.Contains(string(service.body), moved) || !strings.Contains(string(service.body), `[null,[[[["视频好了"]]]]]`) {
		t.Fatalf("续接历史里的模型视频应移到这一轮用户 step 的最前面: %s", service.body)
	}
	if got := mustJSONText(t, service.binding); got != `[{"role":"user","parts":[{"text":"做个视频"}]},{"role":"assistant","parts":[{"text":"视频好了"}]},`+
		`{"role":"user","parts":[{"text":"`+interactionModelMediaIntro+`"},{"text":"让它更长"}]}]` {
		t.Fatalf("binding = %s", got)
	}

	// 账户没有 Drive 授权：视频仍要先上传 Drive（官网发送视频同样先传 Drive），不内联，按参数错误返回 400 的英文说明
	service.noDrive = true
	recorder = postJSON(t, handler, "/v1beta/interactions",
		fmt.Sprintf(`{"model":"gemini-omni-1.1-flash","previous_interaction_id":%q,"input":"再长一点"}`, first["id"]), nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "video and file input for this model must be uploaded to Google Drive first") {
		t.Fatalf("没有 Drive 授权时视频应按需要 Drive 返回 400: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestRequestOrigin 请求地址：协议只认 http 与 https，多级代理取第一个值；主机含不能写进链接的字符时为空（链接改为相对地址）
func TestRequestOrigin(t *testing.T) {
	for _, test := range []struct {
		host    string
		headers map[string]string
		want    string
	}{
		{host: "example.com:2048", want: "http://example.com:2048"},
		{host: "example.com", headers: map[string]string{"X-Forwarded-Proto": "HTTPS, http"}, want: "https://example.com"},
		{host: "example.com", headers: map[string]string{"X-Forwarded-Proto": "javascript"}, want: "http://example.com"},
		{host: "127.0.0.1:2048", headers: map[string]string{"X-Forwarded-Host": "ai.example.com, proxy.local"}, want: "http://ai.example.com"},
		{host: "[2001:db8::1]:2048", want: "http://[2001:db8::1]:2048"},
		{host: "example.com", headers: map[string]string{"X-Forwarded-Host": "evil.example.com)(x"}, want: ""},
	} {
		request := httptest.NewRequest(http.MethodGet, "http://placeholder/", nil)
		request.Host = test.host
		for name, value := range test.headers {
			request.Header.Set(name, value)
		}
		if got := requestOrigin(request); got != test.want {
			t.Fatalf("%s %v: requestOrigin = %q，期望 %q", test.host, test.headers, got, test.want)
		}
	}
}

// interactionStreamBody 构造一段 CreateInteractionStream 响应：思考摘要（content field 6）、正文（field 1）、
// 视频（field 5：MIME 枚举 1 与 Base64 的 MP4）与完成状态、用量
func interactionStreamBody(t *testing.T, video []byte) []byte {
	t.Helper()
	delta := func(content []any) []any {
		event := make([]any, 11)
		event[10] = []any{nil, content}
		return event
	}
	interaction := make([]any, 13)
	interaction[1] = 3
	interaction[12] = []any{12, nil, nil, nil, 19310, nil, nil, nil, 120, 19442}
	final := make([]any, 20)
	final[19] = []any{interaction}
	body, err := json.Marshal([]any{
		[]any{
			delta([]any{nil, nil, nil, nil, nil, []any{[]any{"**规划镜头**\n先写分镜"}}}),
			delta([]any{[]any{"视频已生成："}}),
			delta([]any{nil, nil, nil, nil, []any{1, base64.StdEncoding.EncodeToString(video)}}),
			final,
		},
		[]any{0},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// interactionStreamService 把 CreateInteractionStream 响应按官网解码器解码后作为生成事件
type interactionStreamService struct {
	modelListService
	body []byte
}

func (service *interactionStreamService) Generate(_ context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	var decoded []aistudio.Event
	err := aistudio.DecodeInteractionStream(bytes.NewReader(service.body), func(event aistudio.Event) error {
		decoded = append(decoded, event)
		return nil
	})
	if err != nil {
		return nil, err
	}
	events := make(chan aistudio.Event, len(decoded))
	for _, event := range decoded {
		events <- event
	}
	close(events)
	return events, nil
}

// TestGeminiInteractionVideoLinkEndToEnd 端到端：CreateInteractionStream 返回思考摘要与 field 5 的 MP4，经 Gemini 原生接口
// streamGenerateContent（url 模式）输出思考、正文与链接文本 + fileData；视频按内容保存，链接不带 API Key 能取回相同字节
func TestGeminiInteractionVideoLinkEndToEnd(t *testing.T) {
	video := append(append([]byte(nil), testVideo...), []byte("Google C2PA manifest")...)
	settings := &mediaSettingsStub{enabled: true, base: "https://ai.example.com"}
	handler, dir := newMediaHandler(t, &interactionStreamService{body: interactionStreamBody(t, video)}, settings)
	link := "https://ai.example.com/media/" + sha256Hex(video) + ".mp4"
	body := `{"contents":[{"role":"user","parts":[{"text":"生成一段海浪的视频"}]}]}`

	recorder := postJSON(t, handler, "/v1beta/models/gemini-omni-1.1-flash:streamGenerateContent?alt=sse", body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
	}
	var parts []map[string]any
	var usage map[string]any
	for _, chunk := range sseData(t, recorder.Body.String()) {
		parts = append(parts, geminiResponseParts(t, chunk)...)
		if value, ok := chunk["usageMetadata"].(map[string]any); ok {
			usage = value
		}
	}
	want := []map[string]any{
		{"text": "**规划镜头**\n先写分镜", "thought": true},
		{"text": "视频已生成："},
		{"text": "\n[video](" + link + ")\n"},
		{"fileData": map[string]any{"mimeType": "video/mp4", "fileUri": link}},
	}
	if mustJSONText(t, parts) != mustJSONText(t, want) {
		t.Fatalf("流式 parts:\n%s\n期望:\n%s", mustJSONText(t, parts), mustJSONText(t, want))
	}
	if usage == nil || usage["candidatesTokenCount"] != float64(19310) {
		t.Fatalf("用量 = %v", usage)
	}
	if strings.Contains(recorder.Body.String(), base64.StdEncoding.EncodeToString(video[:64])) {
		t.Fatal("url 模式下不应再内联视频 Base64")
	}
	saved, err := os.ReadFile(filepath.Join(dir, sha256Hex(video)+".mp4"))
	if err != nil || !bytes.Equal(saved, video) {
		t.Fatalf("视频应按内容保存: %v", err)
	}
	fetched := fetchMedia(t, handler, link, nil)
	if fetched.Code != http.StatusOK || !bytes.Equal(fetched.Body.Bytes(), video) || fetched.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("链接取回: %d %q", fetched.Code, fetched.Header().Get("Content-Type"))
	}
	if links := regexp.MustCompile(`\[video\]\(([^)]+)\)`).FindAllStringSubmatch(recorder.Body.String(), -1); len(links) != 1 || links[0][1] != link {
		t.Fatalf("正文里的链接 = %v", links)
	}

	recorder = postJSON(t, handler, "/v1beta/models/gemini-omni-1.1-flash:generateContent", body, nil)
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if mustJSONText(t, geminiResponseParts(t, response)) != mustJSONText(t, want) {
		t.Fatalf("非流式 parts: %s", mustJSONText(t, geminiResponseParts(t, response)))
	}
	if countFiles(t, dir) != 1 {
		t.Fatalf("同一视频只保存一份，目录里有 %d 个文件", countFiles(t, dir))
	}
}

// TestInlineStreamMediaUnchanged inline 模式下三种接口的流式输出与原先一样内联：Chat 与 Messages 为 data URI 的 Markdown
// （PCM 分片各自一段），Gemini 为 inlineData（签名在 inlineData 上），不写媒体文件
func TestInlineStreamMediaUnchanged(t *testing.T) {
	handler, dir := newMediaHandler(t, &scriptedService{events: mediaEvents()}, &mediaSettingsStub{})
	videoURI := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(testVideo)
	imageURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(testImage)

	stream := postJSON(t, handler, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	var content strings.Builder
	for _, chunk := range sseData(t, stream.Body.String()) {
		for _, choice := range chunk["choices"].([]any) {
			if text, ok := choice.(map[string]any)["delta"].(map[string]any)["content"].(string); ok {
				content.WriteString(text)
			}
		}
	}
	want := "Here:\n[media](" + videoURI + ")\n![media](" + imageURI + ")\n[media](data:" + testPCMMIME + ";base64," +
		base64.StdEncoding.EncodeToString(testPCM1) + ")\n[media](data:" + testPCMMIME + ";base64," + base64.StdEncoding.EncodeToString(testPCM2) + ")"
	if content.String() != want {
		t.Fatalf("Chat inline 流式正文与原先不同:\n%.300s", content.String())
	}

	stream = postJSON(t, handler, "/v1/messages", `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Anthropic-Version": "2023-06-01"})
	var deltas []string
	for _, event := range sseData(t, stream.Body.String()) {
		if delta, ok := event["delta"].(map[string]any); ok && delta["type"] == "text_delta" {
			deltas = append(deltas, delta["text"].(string))
		}
	}
	if len(deltas) != 5 || deltas[1] != "[media]("+videoURI+")" || deltas[2] != "![media]("+imageURI+")" {
		t.Fatalf("Messages inline 流式 text_delta 与原先不同: %d 段", len(deltas))
	}

	stream = postJSON(t, handler, "/v1beta/models/m:streamGenerateContent?alt=sse", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, nil)
	var inline []map[string]any
	for _, chunk := range sseData(t, stream.Body.String()) {
		for _, part := range geminiResponseParts(t, chunk) {
			if part["fileData"] != nil {
				t.Fatalf("inline 模式不应输出 fileData: %v", part)
			}
			if part["inlineData"] != nil {
				inline = append(inline, part)
			}
		}
	}
	if len(inline) != 4 || inline[0]["inlineData"].(map[string]any)["data"] != base64.StdEncoding.EncodeToString(testVideo) ||
		inline[0]["thoughtSignature"] != "sig-video" || inline[2]["inlineData"].(map[string]any)["mimeType"] != testPCMMIME {
		t.Fatalf("Gemini inline 流式 inlineData 与原先不同: %d 个", len(inline))
	}
	if countFiles(t, dir) != 0 {
		t.Fatal("inline 模式不应写媒体文件")
	}
}
