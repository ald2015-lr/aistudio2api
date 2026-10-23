package aistudio

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadAccountConfigNormalizesProxy account.json 里手写的冒号写法能直接载入：内存里是规范值，读取时不改写文件，
// 下次保存时写入规范值；不合法时报“账户 proxy 无效: ”，文案不含账号密码
func TestReadAccountConfigNormalizesProxy(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, accountConfigName)
	write := func(proxy string) []byte {
		t.Helper()
		content, err := json.Marshal(map[string]any{
			"label": "a@example.com", "enabled": true, "proxy": proxy,
			"locale": DefaultAccountLocale(), "timezone": DefaultAccountTimezone(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		return content
	}

	original := write("1.2.3.4:8080:user-session-XYZ:S3cr3t:PW")
	value, err := readAccountConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "http://user-session-XYZ:S3cr3t%3APW@1.2.3.4:8080"
	if value.Proxy != want {
		t.Fatalf("Proxy = %q，期望 %q", value.Proxy, want)
	}
	if content, err := os.ReadFile(path); err != nil || !bytes.Equal(content, original) {
		t.Fatalf("读取时不应改写 account.json: %s (%v)", content, err)
	}
	if err := writeAccountConfig(path, value); err != nil {
		t.Fatal(err)
	}
	if again, err := readAccountConfig(path); err != nil || again.Proxy != want {
		t.Fatalf("保存后读回 %q (%v)", again.Proxy, err)
	}

	write(" HTTP://H:1/ ")
	if value, err := readAccountConfig(path); err != nil || value.Proxy != "HTTP://H:1/" {
		t.Fatalf("不带认证的旧值应只去首尾空白，得到 %q (%v)", value.Proxy, err)
	}

	write("ftp://user-session-XYZ:S3cr3t-PW@h:1")
	_, err = readAccountConfig(path)
	if err == nil || !strings.HasPrefix(err.Error(), "账户 proxy 无效: ") {
		t.Fatalf("不合法的代理应报账户 proxy 无效，得到 %v", err)
	}
	if strings.Contains(err.Error(), "user-session-XYZ") || strings.Contains(err.Error(), "S3cr3t-PW") {
		t.Fatalf("错误含代理账号密码: %v", err)
	}
}
