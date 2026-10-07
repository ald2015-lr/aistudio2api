package app

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadAdminTokenPersists 首次生成随机令牌并以 0600 保存，之后读取同一个令牌
func TestLoadAdminTokenPersists(t *testing.T) {
	path := adminTokenPath(filepath.Join(t.TempDir(), ".env"))
	first, err := loadAdminToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if !validAdminToken(first) || len(first) != 64 {
		t.Fatalf("令牌格式无效: %q", first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("令牌文件权限 = %v", info.Mode().Perm())
	}
	second, err := loadAdminToken(path)
	if err != nil || second != first {
		t.Fatalf("重新读取得到 %q, %v", second, err)
	}
	if err := os.WriteFile(path, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := loadAdminToken(path)
	if err != nil || third == first || !validAdminToken(third) {
		t.Fatalf("无效内容应重新生成，得到 %q, %v", third, err)
	}
}
