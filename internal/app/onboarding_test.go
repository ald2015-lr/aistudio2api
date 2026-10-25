package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// verifyTestLogin 为测试用隔离浏览器：验证返回固定结果并计数；onVerify 非空时在验证中（仍持有账户租约）调用；
// onboarding 非空时在验证中自动完成了首次使用页（调用请求里的 OnboardingCompleted）
type verifyTestLogin struct {
	verification aistudio.LoginVerification
	calls        atomic.Int32
	onVerify     func()
	onboarding   *camoufoxnative.OnboardingResult
}

func (login *verifyTestLogin) Login(context.Context, aistudio.IsolatedLoginRequest) (aistudio.IsolatedLoginResult, error) {
	return aistudio.IsolatedLoginResult{}, errors.New("测试中不登录")
}

func (login *verifyTestLogin) Verify(
	_ context.Context, request aistudio.IsolatedLoginRequest, _ aistudio.StorageState,
) (aistudio.LoginVerification, error) {
	login.calls.Add(1)
	if login.onVerify != nil {
		login.onVerify()
	}
	if login.onboarding != nil && request.OnboardingCompleted != nil {
		request.OnboardingCompleted(*login.onboarding)
	}
	verification := login.verification
	verification.VerifiedAt = time.Now()
	return verification, nil
}

// newTestOnboarder 创建不读取已有记录的新账户自动处理队列，记录保存在 directory
func newTestOnboarder(admin *runtimeAdmin, directory string) *accountOnboarder {
	return &accountOnboarder{
		admin: admin, path: filepath.Join(directory, onboardingFileName), policy: defaultOnboardingPolicy(),
		records: make(map[string]onboardRecord), pending: make(map[string]*onboardPending),
		processing: make(map[string]*onboardPending), wake: make(chan struct{}, 1),
	}
}

// readOnboardingFile 读取 .onboarding.json 中的处理记录
func readOnboardingFile(t *testing.T, path string) map[string]onboardRecord {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file onboardFile
	if err := json.Unmarshal(content, &file); err != nil {
		t.Fatal(err)
	}
	return file.Accounts
}

// TestOnboardVerifyFailure 验证是否通过看浏览器的验证结果：停用账户的状态始终是 disabled，不能据此判断
func TestOnboardVerifyFailure(t *testing.T) {
	disabled := aistudio.AccountStatus{State: aistudio.AccountDisabled}
	for _, test := range []struct {
		name         string
		verification aistudio.LoginVerification
		status       aistudio.AccountStatus
		failed       bool
		reason       string
	}{
		{"停用账户浏览器未认证", aistudio.LoginVerification{Reason: "跳转到登录页"}, disabled, true, "跳转到登录页"},
		{"停用账户浏览器已认证", aistudio.LoginVerification{Authenticated: true}, disabled, false, ""},
		{"启用账户目录确认登录失效", aistudio.LoginVerification{Authenticated: true},
			aistudio.AccountStatus{Enabled: true, State: aistudio.AccountAuthRequired, Message: "HTTP 401"}, true, "HTTP 401"},
		{"启用账户已认证", aistudio.LoginVerification{Authenticated: true},
			aistudio.AccountStatus{Enabled: true, State: aistudio.AccountReady}, false, ""},
		{"没有原因", aistudio.LoginVerification{}, disabled, true, "AI Studio 登录已失效"},
	} {
		failed, reason := onboardVerifyFailure(test.verification, test.status)
		if failed != test.failed || reason != test.reason {
			t.Fatalf("%s：failed=%t reason=%q，期望 %t %q", test.name, failed, reason, test.failed, test.reason)
		}
	}
}

// TestOnboardingKeepsDisabledAccountWhenVerifyFails 停用账户浏览器验证未通过时记为验证未通过、保持停用并记下自动停用原因；
// 原先只看账户状态（停用账户始终是 disabled），验证未通过的账户也会被启用
func TestOnboardingKeepsDisabledAccountWhenVerifyFails(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "a@example.com")
	login := &verifyTestLogin{verification: aistudio.LoginVerification{Reason: "跳转到登录页"}}
	admin.login = login
	accountID := accounts[0].ID
	disableTestAccount(t, admin.pool, accountID)
	onboarder := newTestOnboarder(admin, t.TempDir())
	admin.onboard = onboarder

	result, detail, retry := onboarder.process(context.Background(), accountID, defaultOnboardingPolicy())
	if result != onboardVerifyFailed || detail != "跳转到登录页" || retry {
		t.Fatalf("结果 = %s %q %t，期望验证未通过", result, detail, retry)
	}
	if login.calls.Load() != 1 {
		t.Fatal("应启动一次浏览器验证")
	}
	if status := testStatus(t, admin.pool, accountID); status.Enabled {
		t.Fatal("验证未通过的停用账户不应被启用")
	}
	onboarder.processing[accountID] = &onboardPending{queuedAt: time.Now()}
	if final := onboarder.finish(accountID, result, detail, retry); final != onboardVerifyFailed {
		t.Fatalf("最终结果 = %q", final)
	}
	onboarder.flush()
	if onboarder.records[accountID].Result != onboardVerifyFailed || !notes.automatic(accountID) {
		t.Fatal("应记为验证未通过并记下自动停用原因")
	}
}

// TestOnboardingEnablesVerifiedDisabledAccount 停用账户浏览器验证通过后启用，验证时同步的模型目录保留
func TestOnboardingEnablesVerifiedDisabledAccount(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "a@example.com")
	admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	accountID := accounts[0].ID
	disableTestAccount(t, admin.pool, accountID)
	onboarder := newTestOnboarder(admin, t.TempDir())
	admin.onboard = onboarder

	result, _, _ := onboarder.process(context.Background(), accountID, defaultOnboardingPolicy())
	if result != onboardEnabled {
		t.Fatalf("结果 = %s，期望已启用", result)
	}
	status := testStatus(t, admin.pool, accountID)
	if !status.Enabled || status.State != aistudio.AccountReady || len(status.Models) == 0 {
		t.Fatalf("enabled=%t state=%s 模型=%d，期望启用、就绪且有模型目录", status.Enabled, status.State, len(status.Models))
	}
}

// TestOnboardingSaveDebounced 处理结果合并几秒后写一次 .onboarding.json（原先每处理完一个账户就完整重写一次），
// flush 与停止处理时立即写出
func TestOnboardingSaveDebounced(t *testing.T) {
	previous := onboardingSaveDelay
	onboardingSaveDelay = 200 * time.Millisecond
	t.Cleanup(func() { onboardingSaveDelay = previous })
	onboarder := newTestOnboarder(nil, t.TempDir())
	finish := func(index int) {
		id := fmt.Sprintf("u%03d@example.com", index)
		onboarder.mu.Lock()
		onboarder.processing[id] = &onboardPending{queuedAt: time.Now()}
		onboarder.mu.Unlock()
		if result := onboarder.finish(id, onboardVerified, "", false); result != onboardVerified {
			t.Fatalf("结果 = %q", result)
		}
	}
	for index := range 50 {
		finish(index)
	}
	if _, err := os.Stat(onboarder.path); !os.IsNotExist(err) {
		t.Fatal("处理结果不应立即写文件")
	}
	waitFor(t, "合并保存", func() bool {
		_, err := os.Stat(onboarder.path)
		return err == nil
	})
	if records := readOnboardingFile(t, onboarder.path); len(records) != 50 {
		t.Fatalf("文件中有 %d 条记录，期望 50", len(records))
	}

	finish(50)
	onboarder.flush()
	if records := readOnboardingFile(t, onboarder.path); len(records) != 51 {
		t.Fatalf("flush 后文件中有 %d 条记录，期望 51", len(records))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		onboarder.run(ctx)
	}()
	finish(51)
	cancel()
	<-done
	if records := readOnboardingFile(t, onboarder.path); len(records) != 52 {
		t.Fatalf("停止处理后文件中有 %d 条记录，期望 52", len(records))
	}
}
