package vault

import (
	"fmt"
	"os"
	"path/filepath"
)

// atomicWrite 以同目录唯一临时文件写入、同步到磁盘后改名替换目标文件（与 internal/app 的 writeFileAtomic 语义相同：
// 每次唯一临时名，两个写入者同时写不会互相覆盖临时文件；不同步就改名在断电时可能留下空文件）。错误只含路径与系统错误，不含内容
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建目录 %s: %w", directory, err)
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("设置文件权限: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("写入临时文件: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("同步临时文件: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭临时文件: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("替换 %s: %w", path, err)
	}
	return nil
}
