package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// armedContext 在 canceled 成立之后报告已取消；Done 为 nil，派生的 context（取租约的等待等）不随它取消。用来让管理请求恰好在
// prepareUpdate 关闭旧 Worker 之后被取消
type armedContext struct {
	context.Context
	canceled func() bool
}

func (ctx armedContext) Err() error {
	if ctx.canceled() {
		return context.Canceled
	}
	return nil
}

// diskLocale 读取账户 account.json 里的 locale
func diskLocale(t *testing.T, directory string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(directory, "account.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Locale string `json:"locale"`
	}
	if err := json.Unmarshal(content, &value); err != nil {
		t.Fatal(err)
	}
	return value.Locale
}

// TestUpdateAccountCanceledAfterPrepare 改账户语言的管理请求在 prepareUpdate（已关闭旧 Worker）之后被取消（参照上游 c77dd89）：
// 返回取消错误，不写 account.json、账户池里的配置不变；被关掉的 Worker 之后按原配置按需重建，再次保存照常生效。
// 原先取消后照样 SaveConfig，调用方看到失败、配置却已改了
func TestUpdateAccountCanceledAfterPrepare(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := keepCatalogAdmin(t, "update-cancel@example.com")
	account := accounts[0]
	var mu sync.Mutex
	var closedCounters []func() int
	var launchedLocales []string
	admin.workers.launch = func(_ context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, closed := aistudio.NewStubWorker(accountID, 0)
		mu.Lock()
		closedCounters = append(closedCounters, closed)
		launchedLocales = append(launchedLocales, options.Locale)
		mu.Unlock()
		return worker, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.workers.promote(ctx, account.ID, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	original := diskLocale(t, account.Directory)
	changed := "zh-CN"
	if original == changed {
		changed = "en-US"
	}
	firstClosed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(closedCounters) > 0 && closedCounters[0]() > 0
	}
	input := manualDisableInput(account.ID, true)
	input.Locale = changed
	_, err := admin.UpdateAccount(armedContext{Context: context.Background(), canceled: firstClosed}, account.ID, input)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("准备之后被取消应返回取消错误，得到 %v", err)
	}
	if !firstClosed() {
		t.Fatal("前提：旧 Worker 已在 prepareUpdate 里关闭")
	}
	if got := diskLocale(t, account.Directory); got != original {
		t.Fatalf("取消后不应写 account.json：locale=%q，期望 %q", got, original)
	}
	if status := testStatus(t, admin.pool, account.ID); status.Locale != original {
		t.Fatalf("取消后账户池里的配置不应改变：locale=%q", status.Locale)
	}
	if slices.Contains(admin.workers.WarmAccountIDs(), account.ID) {
		t.Fatal("被关掉的 Worker 不应还算驻留")
	}
	// 启动锁已放开：按原配置按需重建
	if _, err := admin.workers.promote(ctx, account.ID, testRuntimeModel); err != nil {
		t.Fatalf("取消后应能按需重建 Worker：%v", err)
	}
	mu.Lock()
	rebuilt := slices.Clone(launchedLocales)
	mu.Unlock()
	if len(rebuilt) != 2 || rebuilt[1] != original {
		t.Fatalf("重建的 Worker 应按原配置启动：启动的语言 %v", rebuilt)
	}
	if _, err := admin.UpdateAccount(ctx, account.ID, input); err != nil {
		t.Fatalf("再次保存应照常生效：%v", err)
	}
	if got := diskLocale(t, account.Directory); got != changed {
		t.Fatalf("再次保存后 locale=%q，期望 %q", got, changed)
	}
}
