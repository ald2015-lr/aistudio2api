package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testLogs 收集 Store 的 WARN 与 INFO 回调
type testLogs struct {
	mu    sync.Mutex
	warns []string
	infos []string
}

func (logs *testLogs) warn(message string) {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	logs.warns = append(logs.warns, message)
}

func (logs *testLogs) info(message string) {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	logs.infos = append(logs.infos, message)
}

func (logs *testLogs) counts() (int, int) {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return len(logs.warns), len(logs.infos)
}

func (logs *testLogs) all() string {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return strings.Join(logs.warns, "\n") + "\n" + strings.Join(logs.infos, "\n")
}

// newTestStore 在临时目录下打开 Store（数据目录 <tmp>/runtime/account-vault，第一次写入时才创建）
func newTestStore(t *testing.T) (*Store, string, *testLogs) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runtime", "account-vault")
	logs := &testLogs{}
	return Open(dir, Options{Warn: logs.warn, Info: logs.info}), dir, logs
}

func openAgain(dir string, logs *testLogs) *Store {
	return Open(dir, Options{Warn: logs.warn, Info: logs.info})
}

var bg = context.Background()

// importLines 确认导入一段文本（带 acknowledge_suspect，不带 expect_overwrites）
func importLines(t *testing.T, store *Store, text string) ProfileImportResult {
	t.Helper()
	result, err := store.ImportProfiles(bg, ProfileImportInput{Sources: []ImportSource{{Name: "粘贴的内容", Text: text}}, AcknowledgeSuspect: true})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	return result
}

func previewLines(t *testing.T, store *Store, text string, mode string) ProfileImportResult {
	t.Helper()
	result, err := store.ImportProfiles(bg, ProfileImportInput{DryRun: true, Mode: mode, Sources: []ImportSource{{Text: text}}})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	return result
}

func reveal(t *testing.T, store *Store, email string) ProfileSecret {
	t.Helper()
	secret, err := store.RevealProfile(bg, RevealInput{Email: email})
	if err != nil {
		t.Fatalf("取出 %s 失败: %v", email, err)
	}
	return secret
}

func strPtr(value string) *string { return &value }

func intPtr(value int) *int { return &value }

func int64Ptr(value int64) *int64 { return &value }

// errorCode 返回 vault 错误的状态与码
func errorCode(err error) (int, string) {
	var vaultErr *Error
	if errors.As(err, &vaultErr) {
		return vaultErr.Status, vaultErr.Code
	}
	return 0, ""
}

func assertCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	if gotStatus, gotCode := errorCode(err); gotStatus != status || gotCode != code {
		t.Fatalf("期望 %d %s，得到 %v（%d %s）", status, code, err, gotStatus, gotCode)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
