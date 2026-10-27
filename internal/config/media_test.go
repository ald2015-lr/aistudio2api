package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMediaConfigLoad 媒体设置的默认值、解析与校验：默认 inline、保存 7 天、总量 20GB；空值沿用默认，0 表示不清理或不限；
// 服务地址去掉末尾的 /；期限与大小有下限，地址只接受 http/https 且不能带账号密码、查询参数与片段
func TestMediaConfigLoad(t *testing.T) {
	clearConfigEnv(t)
	defaults := Default().Media
	if defaults.Output != MediaOutputInline || defaults.BaseURL != "" || defaults.Retention != 7*24*time.Hour || defaults.MaxBytes != 20<<30 {
		t.Fatalf("默认媒体设置 = %+v", defaults)
	}
	for _, test := range []struct {
		content   string
		output    string
		base      string
		retention time.Duration
		maxBytes  int64
		fail      string
	}{
		{content: "", output: "inline", retention: 7 * 24 * time.Hour, maxBytes: 20 << 30},
		{content: "MEDIA_OUTPUT=\nMEDIA_RETENTION=\nMEDIA_MAX_SIZE=\n", output: "inline", retention: 7 * 24 * time.Hour, maxBytes: 20 << 30},
		{
			content: "MEDIA_OUTPUT=URL\nMEDIA_BASE_URL=https://ai.example.com/\nMEDIA_RETENTION=72h\nMEDIA_MAX_SIZE=500mb\n",
			output:  "url", base: "https://ai.example.com", retention: 72 * time.Hour, maxBytes: 500 << 20,
		},
		{
			content: "MEDIA_OUTPUT=url\nMEDIA_BASE_URL=http://192.0.2.10:8080/aistudio\nMEDIA_RETENTION=3d\nMEDIA_MAX_SIZE=1 TB\n",
			output:  "url", base: "http://192.0.2.10:8080/aistudio", retention: 3 * 24 * time.Hour, maxBytes: 1 << 40,
		},
		{content: "MEDIA_RETENTION=0\nMEDIA_MAX_SIZE=0\n", output: "inline"},
		{content: "MEDIA_OUTPUT=link\n", fail: "MEDIA_OUTPUT"},
		{content: "MEDIA_BASE_URL=ftp://ai.example.com\n", fail: "MEDIA_BASE_URL"},
		{content: "MEDIA_BASE_URL=https://user:secret-value@ai.example.com\n", fail: "MEDIA_BASE_URL"},
		{content: "MEDIA_BASE_URL=https://ai.example.com/?a=1\n", fail: "MEDIA_BASE_URL"},
		{content: "MEDIA_BASE_URL=https://ai.example.com/a(b)\n", fail: "MEDIA_BASE_URL"},
		{content: "MEDIA_BASE_URL=ai.example.com\n", fail: "MEDIA_BASE_URL"},
		{content: "MEDIA_RETENTION=30m\n", fail: "MEDIA_RETENTION"},
		{content: "MEDIA_RETENTION=forever\n", fail: "MEDIA_RETENTION"},
		{content: "MEDIA_MAX_SIZE=20\n", fail: "MEDIA_MAX_SIZE"},
		{content: "MEDIA_MAX_SIZE=20XB\n", fail: "MEDIA_MAX_SIZE"},
		{content: "MEDIA_MAX_SIZE=-1GB\n", fail: "MEDIA_MAX_SIZE"},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.fail != "" {
			if err == nil || !strings.Contains(err.Error(), test.fail) {
				t.Fatalf("%q 应报 %s 错误，得到 %v", test.content, test.fail, err)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("错误不应包含密码: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", test.content, err)
		}
		media := cfg.Media
		if media.Output != test.output || media.BaseURL != test.base || media.Retention != test.retention || media.MaxBytes != test.maxBytes {
			t.Fatalf("%q: 媒体设置 = %+v", test.content, media)
		}
	}
}

// TestMediaConfigSaveRoundTrip 保存时写规范写法（7d、20GB），读回不变；进程环境变量优先于 .env
func TestMediaConfigSaveRoundTrip(t *testing.T) {
	clearConfigEnv(t)
	cfg := Default()
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"MEDIA_OUTPUT=inline\n", "MEDIA_BASE_URL=\n", "MEDIA_RETENTION=7d\n", "MEDIA_MAX_SIZE=20GB\n"} {
		if !strings.Contains(string(data), line) {
			t.Fatalf(".env 缺少 %q:\n%s", line, data)
		}
	}
	cfg.Media = MediaSettings{Output: "url", BaseURL: "https://ai.example.com/", Retention: 36 * time.Hour, MaxBytes: 1536 << 20}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	for _, line := range []string{"MEDIA_OUTPUT=url\n", "MEDIA_BASE_URL=https://ai.example.com\n", "MEDIA_RETENTION=36h\n", "MEDIA_MAX_SIZE=1536MB\n"} {
		if !strings.Contains(string(data), line) {
			t.Fatalf(".env 缺少 %q:\n%s", line, data)
		}
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := MediaSettings{Output: "url", BaseURL: "https://ai.example.com", Retention: 36 * time.Hour, MaxBytes: 1536 << 20}
	if loaded.Media != want {
		t.Fatalf("读回 %+v，期望 %+v", loaded.Media, want)
	}

	t.Setenv("MEDIA_OUTPUT", "inline")
	overridden, err := Load(path)
	if err != nil || overridden.Media.Output != MediaOutputInline || overridden.Media.BaseURL != want.BaseURL {
		t.Fatalf("进程环境变量应覆盖 .env: %+v err=%v", overridden.Media, err)
	}
}

// TestMediaConfigJSON JSON 写法为 media 对象（期限与大小为文本）；旧数据没有该字段时取默认设置，无效值报错
func TestMediaConfigJSON(t *testing.T) {
	cfg := Default()
	cfg.Media = MediaSettings{Output: MediaOutputURL, BaseURL: "https://ai.example.com", Retention: 0, MaxBytes: 0}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"media":{"output":"url","base_url":"https://ai.example.com","retention":"0","max_size":"0"}`) {
		t.Fatalf("JSON 中的媒体设置: %s", data)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Media != cfg.Media {
		t.Fatalf("JSON 往返 %+v，期望 %+v", decoded.Media, cfg.Media)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "media")
	legacy, _ := json.Marshal(fields)
	if err := json.Unmarshal(legacy, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Media != DefaultMediaSettings() {
		t.Fatalf("旧数据的媒体设置 = %+v，期望默认值", decoded.Media)
	}
	fields["media"] = map[string]any{"output": "url", "retention": "10m"}
	invalid, _ := json.Marshal(fields)
	if err := json.Unmarshal(invalid, &decoded); err == nil || !strings.Contains(err.Error(), "MEDIA_RETENTION") {
		t.Fatalf("过短的保存期限应报错: %v", err)
	}
}

// TestMediaSizeAndRetentionFormat 大小与期限的写法：取能整除的最大单位，Parse 与 Format 往返不变
func TestMediaSizeAndRetentionFormat(t *testing.T) {
	for _, test := range []struct {
		size int64
		text string
	}{
		{0, "0"}, {20 << 30, "20GB"}, {500 << 20, "500MB"}, {1 << 40, "1TB"}, {1536 << 20, "1536MB"}, {100<<20 + 1, "104857601B"},
	} {
		if got := FormatMediaSize(test.size); got != test.text {
			t.Fatalf("FormatMediaSize(%d) = %q，期望 %q", test.size, got, test.text)
		}
		if parsed, err := ParseMediaSize(test.text); err != nil || parsed != test.size {
			t.Fatalf("ParseMediaSize(%q) = %d %v", test.text, parsed, err)
		}
	}
	for _, test := range []struct {
		duration time.Duration
		text     string
	}{
		{0, "0"}, {7 * 24 * time.Hour, "7d"}, {36 * time.Hour, "36h"}, {90 * time.Minute, "90m"}, {90 * time.Second, "1m30s"},
	} {
		if got := FormatMediaRetention(test.duration); got != test.text {
			t.Fatalf("FormatMediaRetention(%s) = %q，期望 %q", test.duration, got, test.text)
		}
		if parsed, err := ParseMediaRetention(test.text); err != nil || parsed != test.duration {
			t.Fatalf("ParseMediaRetention(%q) = %s %v", test.text, parsed, err)
		}
	}
	for _, value := range []string{"", "abc", "1.5GB", "GB", "9223372036854775807TB"} {
		if _, err := ParseMediaSize(value); err == nil {
			t.Fatalf("ParseMediaSize(%q) 应报错", value)
		}
	}
	for _, value := range []string{"", "-1h", "1.5d", "d", "abc"} {
		if _, err := ParseMediaRetention(value); err == nil {
			t.Fatalf("ParseMediaRetention(%q) 应报错", value)
		}
	}
}
