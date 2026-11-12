package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// deviceBoundTestAccount 返回认证状态带或不带 __Secure-1PSIDRTS 的账户（还没进账户池）
func deviceBoundTestAccount(id string, bound bool) *aistudio.Account {
	cookies := []aistudio.StateCookie{{Name: "SAPISID", Value: "sapisid-value", Domain: ".google.com", Path: "/"}}
	if bound {
		cookies = append(cookies, aistudio.StateCookie{Name: deviceBoundCookie, Value: "bound-secret-value", Domain: ".google.com", Path: "/"})
	}
	return &aistudio.Account{
		ID: id, Config: aistudio.AccountConfig{Label: id},
		StorageState: aistudio.StorageState{Cookies: cookies, Origins: []aistudio.StorageOrigin{}},
	}
}

// TestLogDeviceBoundSessions 载入认证文件时统计带 __Secure-1PSIDRTS 的账户（设备绑定会话，复制出来约 2 小时失效），写一行 WARN：
// 只写数量与最多 5 个账户名，不写 Cookie 的值；没有这类账户时不写
func TestLogDeviceBoundSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	accounts := []*aistudio.Account{deviceBoundTestAccount("plain@example.com", false), nil}
	logDeviceBoundSessions(requests, accounts, "启动时")
	if logCount(requests, "设备绑定会话") != 0 {
		t.Fatal("没有设备绑定会话时不应写日志")
	}
	for index := range 7 {
		accounts = append(accounts, deviceBoundTestAccount(fmt.Sprintf("bound%d@example.com", index), true))
	}
	logDeviceBoundSessions(requests, accounts, "启动时")
	if logCount(requests, "启动时载入的认证文件中有 7 个账户是设备绑定会话（带 __Secure-1PSIDRTS") != 1 {
		t.Fatalf("应写一行带数量的 WARN：%v", serviceLogMessages(requests))
	}
	requests.mu.Lock()
	defer requests.mu.Unlock()
	for _, entry := range requests.logs {
		if !strings.Contains(entry.Message, "设备绑定会话") {
			continue
		}
		if entry.Level != "WARN" || !strings.Contains(entry.Message, "bound0@example.com, bound1@example.com, bound2@example.com, bound3@example.com, bound4@example.com 等 7 个") {
			t.Fatalf("应列出前 5 个账户名与总数：%s %s", entry.Level, entry.Message)
		}
		if strings.Contains(entry.Message, "bound5@example.com") || strings.Contains(entry.Message, "plain@example.com") {
			t.Fatalf("最多列出 5 个、只列设备绑定的账户：%s", entry.Message)
		}
		if strings.Contains(entry.Message, "secret") || strings.Contains(entry.Message, "sapisid-value") {
			t.Fatalf("日志不应含 Cookie 的值：%s", entry.Message)
		}
	}
}

// deviceBoundRenewableAccount 返回带 __Secure-1PSIDRTS、认证扩展里保存了 Chrome OAuth 续签材料的账户（本服务 Chrome 导入的账户）
func deviceBoundRenewableAccount(t *testing.T, id string) *aistudio.Account {
	t.Helper()
	account := deviceBoundTestAccount(id, true)
	extension := aistudio.AuthExtension{
		Source: aistudio.AuthSource{Browser: "chrome", Email: id},
		OAuth:  &aistudio.ChromeOAuthMaterial{GaiaID: "1", RefreshToken: "refresh-secret-value"},
	}
	if err := account.StorageState.SetAuthExtension(extension); err != nil {
		t.Fatal(err)
	}
	return account
}

// deviceBoundLogs 返回设备绑定会话的日志
func deviceBoundLogs(requests *requestRegistry) []string {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	var messages []string
	for _, entry := range requests.logs {
		if strings.Contains(entry.Message, "设备绑定会话") {
			messages = append(messages, entry.Level+" "+entry.Message)
		}
	}
	return messages
}

// TestDeviceBoundRenewableExcluded 带 Chrome 续签材料的设备绑定会话（本服务 Chrome 导入、强制设备绑定的账户）在能原地续签的平台上
// 不计入 WARN 的数量与名单，单独写明“另有 N 个可原地续签”；全是这类账户时不写。不能原地续签的平台上照常计入，写明只有 Windows 能续签，
// 提示语只给浏览器登录。日志不含续签材料的值
func TestDeviceBoundRenewableExcluded(t *testing.T) {
	previous := deviceBoundRenewSupported
	t.Cleanup(func() { deviceBoundRenewSupported = previous })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceBoundRenewSupported = func() bool { return true }
	requests := newRequestRegistry(ctx)
	logDeviceBoundSessions(requests, []*aistudio.Account{deviceBoundRenewableAccount(t, "chrome@example.com")}, "启动时")
	if logs := deviceBoundLogs(requests); len(logs) != 0 {
		t.Fatalf("全是可原地续签的设备绑定会话时不应写 WARN：%v", logs)
	}
	logDeviceBoundSessions(requests, []*aistudio.Account{
		deviceBoundRenewableAccount(t, "chrome@example.com"), deviceBoundTestAccount("copied@example.com", true),
	}, "启动时")
	logs := deviceBoundLogs(requests)
	if len(logs) != 1 || !strings.Contains(logs[0], "有 1 个账户是设备绑定会话") ||
		!strings.Contains(logs[0], "另有 1 个带 Chrome 续签材料、可原地续签，不计入") ||
		!strings.Contains(logs[0], "可改用本服务的浏览器登录或 Chrome 导入") || !strings.Contains(logs[0], "账户=copied@example.com") ||
		strings.Contains(logs[0], "chrome@example.com") {
		t.Fatalf("可原地续签的应单独计数、不进名单：%v", logs)
	}

	deviceBoundRenewSupported = func() bool { return false }
	requests = newRequestRegistry(ctx)
	logDeviceBoundSessions(requests, []*aistudio.Account{deviceBoundRenewableAccount(t, "chrome@example.com")}, "启动时")
	logs = deviceBoundLogs(requests)
	if len(logs) != 1 || !strings.Contains(logs[0], "有 1 个账户是设备绑定会话") ||
		!strings.Contains(logs[0], "其中 1 个带 Chrome 续签材料，但只有 Windows 能原地续签") ||
		strings.Contains(logs[0], "Chrome 导入") || !strings.Contains(logs[0], "账户=chrome@example.com") {
		t.Fatalf("不能原地续签的平台上应照常计入并说明：%v", logs)
	}
	for _, message := range logs {
		if strings.Contains(message, "secret") {
			t.Fatalf("日志不应含 Cookie 或续签材料的值：%s", message)
		}
	}
}

// TestDeviceBoundCountedOnCredentialUpdate 认证文件更新（批量覆盖已有账户的 storage-state.json）载入的新认证文件同样统计：
// 一批载入完写一行“认证文件更新时载入的认证文件中有 N 个账户是设备绑定会话”
func TestDeviceBoundCountedOnCredentialUpdate(t *testing.T) {
	previous := deviceBoundRenewSupported
	deviceBoundRenewSupported = func() bool { return false }
	t.Cleanup(func() { deviceBoundRenewSupported = previous })
	admin, _, accounts := keepCatalogAdmin(t, "a@example.com", "b@example.com")
	watcher := newTestAccountWatcher(admin)
	for _, account := range accounts {
		state := authTestState(t, "NEW-"+account.ID, false)
		state.Cookies = append(state.Cookies, aistudio.StateCookie{Name: deviceBoundCookie, Value: "bound-secret-value", Domain: ".google.com", Path: "/"})
		if err := aistudio.WriteStorageState(account.StoragePath, state); err != nil {
			t.Fatal(err)
		}
		watcher.loginPending[account.ID] = struct{}{}
	}
	known := map[string]aistudio.AccountLoginSummary{}
	for _, summary := range admin.pool.LoginSummaries() {
		known[summary.ID] = summary
	}
	watcher.startLoginRefresh(admin.lifecycle, known)
	waitFor(t, "认证文件更新载入完成", func() bool { return !watcher.refreshing.Load() })
	logs := deviceBoundLogs(admin.requests)
	if len(logs) != 1 || !strings.Contains(logs[0], "认证文件更新时载入的认证文件中有 2 个账户是设备绑定会话") {
		t.Fatalf("认证文件更新载入的设备绑定会话应汇总写一行：%v", logs)
	}
	if strings.Contains(logs[0], "secret") {
		t.Fatalf("日志不应含 Cookie 的值：%s", logs[0])
	}
}
