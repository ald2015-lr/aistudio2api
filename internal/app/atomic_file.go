package app

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic 以同目录唯一临时文件写入、同步到磁盘后改名替换目标文件。
// 固定的 .tmp 文件名在两个写入者同时保存时（例如重建生成服务期间新旧实例重叠）会互相覆盖，
// 不同步就改名在断电时可能留下空文件
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
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
