package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestWriteFileAtomicConcurrent 并发写入时目标文件始终是某一次完整写入的内容，不留下临时文件
func TestWriteFileAtomicConcurrent(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, ".onboarding.json")
	var wg sync.WaitGroup
	for index := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			content := fmt.Sprintf(`{"writer":%d,"padding":"%0512d"}`, index, index)
			if err := writeFileAtomic(path, []byte(content), 0o600); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != len(`{"writer":0,"padding":"`)+512+2 && len(data) != len(`{"writer":10,"padding":"`)+512+2 {
		t.Fatalf("文件内容不完整: %d 字节", len(data))
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("权限 = %v, %v", info.Mode().Perm(), err)
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 1 {
		t.Fatalf("目录中应只剩目标文件，得到 %d 个", len(entries))
	}
}
