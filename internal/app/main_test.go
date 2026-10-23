package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain 把模型目录缓存（含坏号记忆，见 catalogCacheDir、badAccountPath）整个测试进程改到临时目录：默认是相对工作目录的
// runtime/model-catalogs，go test 时就是包目录，.gitignore 只忽略根目录的 /runtime/。没有调 useTempCatalogCacheDir 的测试
// （包括以后其他分支新加的、直接调用 newRuntime 的集成测试）也不会在包目录留下 index、blob 与 bad-accounts.json，
// 不会从上一次 go test 留下的文件“来自磁盘”恢复目录。需要与其他测试隔离的仍用 useTempCatalogCacheDir（每个测试一个目录）
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aistudio2api-app-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建测试临时目录:", err)
		os.Exit(1)
	}
	catalogCacheDir = filepath.Join(dir, "model-catalogs")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
