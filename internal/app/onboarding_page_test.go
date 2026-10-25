package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// TestStartupOnboardingCompletedLog 启动时自动完成 AI Studio 首次使用页后写一行 INFO（勾选数与各次勾选的标签、按钮、按顺序做过的每一步、
// 耗时，没有点击按钮时按钮写 -，没有标签时不写括号，没有进度记录时不写已做），并把完成之后页面的 localStorage 写回认证文件（这次启动随后失败也写：
// 浏览器关闭后就没有机会了），解析失败只写警告；之后的启动失败照常记录
func TestStartupOnboardingCompletedLog(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	onboarded := `{"origin":"https://aistudio.google.com","localStorage":[{"name":"onboarded","value":"1"}]}`
	manager.launch = func(_ context.Context, _ string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		if options.OnboardingCompleted == nil {
			return nil, errors.New("启动参数缺少首次使用页回调")
		}
		options.OnboardingCompleted(camoufoxnative.OnboardingResult{
			Checked: 2, Labels: []string{"* 私は 18 歳以上です", "[补]Google 利用規約に同意します"}, Button: "开始使用", Skipped: 1,
			Steps:   []string{"勾选:* 私は 18 歳以上です", "勾选:[补]Google 利用規約に同意します", "点击:続行", "跳过:跳过", "点击:开始使用"},
			Elapsed: 1500 * time.Millisecond, LocalStorage: []byte(onboarded),
		})
		options.OnboardingCompleted(camoufoxnative.OnboardingResult{Checked: 1, Elapsed: 2 * time.Second, LocalStorage: []byte("{")})
		return nil, errors.New("官网初始化页面模型不匹配 expected=a actual=b")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err == nil {
		t.Fatal("启动应失败")
	}
	want := []string{
		"完成 AI Studio 首次使用页 | 勾选=2（* 私は 18 歳以上です；[补]Google 利用規約に同意します） | 按钮=开始使用 | " +
			"已做=勾选:* 私は 18 歳以上です；勾选:[补]Google 利用規約に同意します；点击:続行；跳过:跳过；点击:开始使用 | 耗时=1.5s",
		"完成 AI Studio 首次使用页 | 勾选=1 | 按钮=- | 耗时=2s",
	}
	if got := levelLogs(requests, "INFO", "完成 AI Studio 首次使用页"); !slices.Equal(got, want) {
		t.Fatalf("首次使用页日志\n得到 %v\n期望 %v", got, want)
	}
	if got := levelLogs(requests, "ERROR", "WAA Worker 启动失败", "官网初始化页面模型不匹配"); len(got) != 1 {
		t.Fatalf("之后的启动失败应照常记录: %v", serviceLogMessages(requests))
	}
	state, _ := pool.StorageStateOf(partitionNormalA)
	written := false
	for _, origin := range state.Origins {
		written = written || origin.Origin == aistudio.BrowserLocalStorageOrigin &&
			slices.Equal(origin.LocalStorage, []aistudio.StorageItem{{Name: "onboarded", Value: "1"}})
	}
	if !written {
		t.Fatalf("完成后的 localStorage 应写回认证状态: %+v", state.Origins)
	}
	if got := levelLogs(requests, "WARN", "完成 AI Studio 首次使用页后写回 localStorage 失败 | 错误=解析浏览器 localStorage 失败"); len(got) != 1 {
		t.Fatalf("解析失败应写一条警告: %v", serviceLogMessages(requests))
	}
}

// TestVerifyOnboardingCompletedLog 账户验证中自动完成首次使用页时写“账户验证 | 完成 AI Studio 首次使用页 | …”，格式同启动时那一行
func TestVerifyOnboardingCompletedLog(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "verify-onboarding@example.com")
	admin.login = &verifyTestLogin{
		verification: aistudio.LoginVerification{Authenticated: true},
		onboarding: &camoufoxnative.OnboardingResult{Checked: 1, Labels: []string{"* 私は 18 歳以上です"}, Button: "続行",
			Steps: []string{"勾选:* 私は 18 歳以上です", "点击:続行"}, Elapsed: 1500 * time.Millisecond},
	}
	if _, err := admin.VerifyAccount(context.Background(), accounts[0].ID); err != nil {
		t.Fatal(err)
	}
	want := []string{"账户验证 | 完成 AI Studio 首次使用页 | 勾选=1（* 私は 18 歳以上です） | 按钮=続行 | 已做=勾选:* 私は 18 歳以上です；点击:続行 | 耗时=1.5s"}
	if got := levelLogs(admin.requests, "INFO", "完成 AI Studio 首次使用页"); !slices.Equal(got, want) {
		t.Fatalf("验证时的首次使用页日志\n得到 %v\n期望 %v", got, want)
	}
}
