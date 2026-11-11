package api

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// attachmentPNG 为测试用的 PNG 文件头（不是 UTF-8 文本）
var attachmentPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

// TestAttachmentMIME 内容是文本、声明的类型不是 text/*、application/json 与 application/x-ipynb+json 时按 text/plain；
// 文本类声明、二进制内容（PNG、PDF、带 NUL 的数据）与空内容原样保留；512 字节截断处不完整的汉字不影响判断
func TestAttachmentMIME(t *testing.T) {
	code := []byte("export const answer: number = 42;\n")
	long := bytes.Repeat([]byte("汉"), 200) // 600 字节，第 512 字节落在一个汉字中间
	for _, test := range []struct {
		name, mime string
		data       []byte
		want       string
	}{
		{"octet-stream 代码", "application/octet-stream", code, "text/plain"},
		{"x-sh 脚本", "application/x-sh", []byte("#!/bin/sh\necho hi\n"), "text/plain"},
		{".ts 被认成 mp2t", "video/mp2t", code, "text/plain"},
		{"默认 PDF 的文本", "application/pdf", code, "text/plain"},
		{"没有类型的文本", "", code, "text/plain"},
		{"截断处的汉字", "application/octet-stream", long, "text/plain"},
		{"text 子类型原样", "text/markdown", code, "text/markdown"},
		{"带参数的 text 原样", "text/plain; charset=utf-8", code, "text/plain; charset=utf-8"},
		{"JSON 原样", "application/json", []byte(`{"a":1}`), "application/json"},
		{"ipynb 原样", "application/x-ipynb+json", []byte(`{"cells":[]}`), "application/x-ipynb+json"},
		{"PNG 原样", "image/png", attachmentPNG, "image/png"},
		{"PDF 原样", "application/pdf", []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n"), "application/pdf"},
		{"很短的 PDF 文件头", "application/pdf", []byte("%PDF"), "application/pdf"},
		{"标成 octet-stream 的 PDF 文件头", "application/octet-stream", []byte("%PDF-1.4\n"), "application/octet-stream"},
		{"带 NUL 的二进制", "application/octet-stream", []byte("abc\x00def"), "application/octet-stream"},
		{"空内容", "application/octet-stream", nil, "application/octet-stream"},
	} {
		if got := attachmentMIME(test.mime, test.data); got != test.want {
			t.Errorf("%s: attachmentMIME(%q) = %q，期望 %q", test.name, test.mime, got, test.want)
		}
	}
}

// TestTextAttachmentsSentAsPlainText 四个协议里内容是文本的附件按 text/plain 发给生成服务：OpenAI file_data（不带前缀、
// 文件名 .ts 或没有文件名）与 data URL（octet-stream），Anthropic base64 document（application/x-sh），Gemini inlineData；
// 图片与已经是文本类型的附件不变
func TestTextAttachmentsSentAsPlainText(t *testing.T) {
	code := base64.StdEncoding.EncodeToString([]byte("export const answer: number = 42;\n"))
	png := base64.StdEncoding.EncodeToString(attachmentPNG)
	// .ts 按系统的 MIME 表推断：Debian 系为 video/mp2t（改为 text/plain），有的系统为 text/vnd.trolltech.linguist（已是文本类型，原样）
	ts := mimeForFilename("index.ts", "application/pdf")
	if !strings.HasPrefix(ts, "text/") {
		ts = "text/plain"
	}
	for _, test := range []struct {
		name, path, body string
		header           map[string]string
		want             []string
	}{
		{name: "OpenAI file_data", path: "/v1/chat/completions",
			body: `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":[{"type":"text","text":"看看"},` +
				`{"type":"file","file":{"filename":"index.ts","file_data":"` + code + `"}},` +
				`{"type":"file","file":{"file_data":"` + code + `"}},` +
				`{"type":"file","file":{"filename":"a.txt","file_data":"data:application/octet-stream;base64,` + code + `"}},` +
				`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]}]}`,
			want: []string{ts, "text/plain", "text/plain", "image/png"}},
		{name: "Anthropic base64", path: "/v1/messages", header: map[string]string{"Anthropic-Version": "2023-06-01"},
			body: `{"model":"gemini-2.5-flash","max_tokens":16,"messages":[{"role":"user","content":[` +
				`{"type":"document","source":{"type":"base64","media_type":"application/x-sh","data":"` + code + `"}},` +
				`{"type":"document","source":{"type":"base64","media_type":"text/x-typescript","data":"` + code + `"}},` +
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}},` +
				`{"type":"text","text":"看看"}]}]}`,
			want: []string{"text/plain", "text/x-typescript", "image/png"}},
		{name: "Gemini inlineData", path: "/v1beta/models/gemini-2.5-flash:generateContent",
			body: `{"contents":[{"role":"user","parts":[{"text":"看看"},{"inlineData":{"mimeType":"application/x-sh","data":"` + code + `"}},` +
				`{"inlineData":{"mimeType":"image/png","data":"` + png + `"}}]}]}`,
			want: []string{"text/plain", "image/png"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &scriptedService{events: []aistudio.Event{
				{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "STOP"},
			}}
			recorder := postJSON(t, NewHandler(service, Config{APIKey: "sk-test"}), test.path, test.body, test.header)
			if recorder.Code != 200 || service.last == nil {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var got []string
			for _, content := range service.last.Contents {
				for _, part := range content.Parts {
					if part.InlineData != nil {
						got = append(got, part.InlineData.MIME)
					}
				}
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("内联附件的 MIME = %v，期望 %v", got, test.want)
			}
		})
	}
}

// TestMultipartStreamMIMEText 上传文件声明的类型不是文本类、内容却是文本时按 text/plain 上传；图片原样，octet-stream 仍按内容识别
func TestMultipartStreamMIMEText(t *testing.T) {
	for _, test := range []struct {
		declared string
		data     []byte
		want     string
	}{
		{"video/mp2t", []byte("const a = 1;\n"), "text/plain"},
		{"application/x-sh", []byte("#!/bin/sh\n"), "text/plain"},
		{"image/png", attachmentPNG, "image/png"},
		{"application/octet-stream", []byte("const a = 1;\n"), "text/plain; charset=utf-8"},
		{"text/x-python", []byte("print(1)\n"), "text/x-python"},
	} {
		mediaType, stream, err := multipartStreamMIME(bytes.NewReader(test.data), test.declared)
		if err != nil {
			t.Fatal(err)
		}
		var read bytes.Buffer
		if _, err := read.ReadFrom(stream); err != nil || !bytes.Equal(read.Bytes(), test.data) {
			t.Fatalf("%s: 上传内容被改动: %q", test.declared, read.Bytes())
		}
		if mediaType != test.want {
			t.Errorf("声明 %s: 上传 MIME = %q，期望 %q", test.declared, mediaType, test.want)
		}
	}
}
