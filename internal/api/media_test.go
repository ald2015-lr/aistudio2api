package api

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestGeminiInlineDataCompatibility 验证不同 SDK 的内联媒体字段与 Base64 形式
func TestGeminiInlineDataCompatibility(t *testing.T) {
	want := []byte{0xfb, 0xff, 0xef}
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "camel case", raw: `[{"inlineData":{"mimeType":"application/octet-stream","data":"+//v"}}]`},
		{name: "snake case", raw: `[{"inline_data":{"mime_type":"application/octet-stream","data":"-__v"}}]`},
		{name: "data URL", raw: `[{"inlineData":{"mimeType":"application/octet-stream","data":"data:application/octet-stream;base64,+//v"}}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var input []geminiPart
			if err := json.Unmarshal([]byte(test.raw), &input); err != nil {
				t.Fatal(err)
			}
			parts, hasResult, err := mapGeminiParts(input)
			if err != nil {
				t.Fatal(err)
			}
			if hasResult || len(parts) != 1 || parts[0].InlineData == nil {
				t.Fatalf("mapped parts=%#v hasResult=%v", parts, hasResult)
			}
			if parts[0].InlineData.MIME != "application/octet-stream" || !bytes.Equal(parts[0].InlineData.Data, want) {
				t.Fatalf("inline data=%#v", parts[0].InlineData)
			}
		})
	}
}

// gifWithCanvas 构造逻辑画布为 width×height、首帧为 1×1 的最小 GIF
func gifWithCanvas(width, height uint16) []byte {
	return []byte{'G', 'I', 'F', '8', '9', 'a',
		byte(width), byte(width >> 8), byte(height), byte(height >> 8), 0x80, 0, 0,
		0, 0, 0, 0xff, 0xff, 0xff,
		',', 0, 0, 0, 0, 1, 0, 1, 0, 0,
		2, 2, 0x44, 0x01, 0,
		';'}
}

// TestNormalizeImagePayloadRejectsHugeGIFCanvas 头部声明超大画布的 GIF 原样返回，不按画布分配内存
func TestNormalizeImagePayloadRejectsHugeGIFCanvas(t *testing.T) {
	data := gifWithCanvas(65535, 65535)
	mime, output := normalizeImagePayload("image/gif", data)
	if mime != "image/gif" || !bytes.Equal(output, data) {
		t.Fatalf("超大画布 GIF 应原样返回，得到 mime=%s len=%d", mime, len(output))
	}
}

// TestNormalizeImagePayloadConvertsSmallGIF 正常尺寸的 GIF 仍按逻辑画布转为 PNG
func TestNormalizeImagePayloadConvertsSmallGIF(t *testing.T) {
	mime, output := normalizeImagePayload("image/gif", gifWithCanvas(4, 3))
	if mime != "image/png" || !bytes.HasPrefix(output, []byte("\x89PNG")) {
		t.Fatalf("应转换为 PNG，得到 mime=%s", mime)
	}
}
