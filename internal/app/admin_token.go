package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// adminTokenFileName 为管理令牌文件名，与 .env 放在同一目录
const adminTokenFileName = ".admin-token"

// adminTokenPath 返回与配置文件同目录的管理令牌文件路径
func adminTokenPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), adminTokenFileName)
}

// loadAdminToken 读取管理令牌，不存在或内容无效时生成新的随机令牌并以 0600 权限保存。
// 令牌跨重启保持不变，浏览器记住的登录 Cookie 在重启后继续有效；删除文件即可让旧令牌失效。
// 保存失败时仍返回只在本次运行有效的令牌，管理面不会因此退回免认证
func loadAdminToken(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(data)); validAdminToken(token) {
			return token, nil
		}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("生成管理令牌: %w", err)
	}
	token := hex.EncodeToString(raw[:])
	if err := writeAdminToken(path, token); err != nil {
		return token, err
	}
	return token, nil
}

func validAdminToken(token string) bool {
	if len(token) < 32 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

// writeAdminToken 以临时文件加改名写入令牌，避免并发启动读到半截内容
func writeAdminToken(path string, token string) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".admin-token-*.tmp")
	if err != nil {
		return fmt.Errorf("保存管理令牌: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("保存管理令牌: %w", err)
	}
	if _, err := temporary.WriteString(token + "\n"); err != nil {
		temporary.Close()
		return fmt.Errorf("保存管理令牌: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("保存管理令牌: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("保存管理令牌: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("保存管理令牌: %w", err)
	}
	return nil
}
