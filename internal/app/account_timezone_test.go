package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// assertInvalidTimezone 断言错误为 400 invalid_timezone
func assertInvalidTimezone(t *testing.T, what string, err error) {
	t.Helper()
	var operation *adminOperationError
	if !errors.As(err, &operation) || operation.status != http.StatusBadRequest || operation.code != "invalid_timezone" {
		t.Fatalf("%s：应返回 400 invalid_timezone，得到 %v", what, err)
	}
}

// TestAccountTimezoneInput 管理端录入的时区用 time.LoadLocation 校验：拼错、带零宽或其他不可见字符、中间有空白、Local 都返回 400，
// 错误里的值按 %q 显示（零宽字符显示为转义）；首尾空白去掉，空值交给调用方取默认值
func TestAccountTimezoneInput(t *testing.T) {
	for _, value := range []string{"Asia/Shanghai", "UTC", "America/Los_Angeles", "Etc/GMT+8", "  Europe/Berlin\t"} {
		got, err := accountTimezoneInput(value)
		if err != nil || got != strings.TrimSpace(value) {
			t.Fatalf("%q 应有效：%q %v", value, got, err)
		}
	}
	if got, err := accountTimezoneInput("   "); err != nil || got != "" {
		t.Fatalf("空值应返回空串：%q %v", got, err)
	}
	for _, value := range []string{
		"Asia/Shanghia", "Asia/Shanghai\u200b", "Asia/\u200bShanghai", "\ufeffUTC", "Asia/ Shanghai", "Local", "Asia/Shanghai\x00",
	} {
		_, err := accountTimezoneInput(value)
		assertInvalidTimezone(t, "时区 "+strings.ToValidUTF8(value, "?"), err)
	}
	_, err := accountTimezoneInput("Asia/Shanghai\u200b")
	if !strings.Contains(err.Error(), `\u200b`) {
		t.Fatalf("零宽字符应在错误里显示为转义：%v", err)
	}
}

// countingLogin 为只计数的隔离浏览器：Login 不应被调用（时区无效时在打开浏览器之前返回）
type countingLogin struct {
	calls atomic.Int32
}

func (login *countingLogin) Login(context.Context, aistudio.IsolatedLoginRequest) (aistudio.IsolatedLoginResult, error) {
	login.calls.Add(1)
	return aistudio.IsolatedLoginResult{}, errors.New("不应打开浏览器")
}

func (login *countingLogin) Verify(context.Context, aistudio.IsolatedLoginRequest, aistudio.StorageState) (aistudio.LoginVerification, error) {
	return aistudio.LoginVerification{}, nil
}

// TestAccountTimezoneValidatedOnInput 添加账户、Chrome 导入与修改时区时校验（无效在打开浏览器、导入之前返回 400）；
// 已有账户原样保存的时区（早先存下的无效值）不校验，启停与改代理照常，不在读入时静默规范化
func TestAccountTimezoneValidatedOnInput(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "a@example.com")
	accountID := accounts[0].ID
	login := &countingLogin{}
	admin.login = login
	_, err := admin.CreateAccount(context.Background(), api.AccountCreateInput{Timezone: "Asia/Shanghai\u200b"})
	assertInvalidTimezone(t, "添加账户", err)
	if login.calls.Load() != 0 {
		t.Fatal("时区无效时不应打开浏览器")
	}
	_, err = admin.ImportChromeAccounts(context.Background(), api.ChromeImportInput{AccountIDs: []string{"Default/x"}, Timezone: "Mars/Olympus"})
	assertInvalidTimezone(t, "Chrome 导入", err)
	input := manualDisableInput(accountID, true)
	input.Timezone = "Asia/Shanghia"
	_, err = admin.UpdateAccount(context.Background(), accountID, input)
	assertInvalidTimezone(t, "修改时区", err)
	if status := testStatus(t, admin.pool, accountID); status.Timezone != aistudio.DefaultAccountTimezone() {
		t.Fatalf("无效时区不应写入：%q", status.Timezone)
	}

	// 早先存下的无效时区（account.json 只要求非空）：原样保存、启停都不校验
	lease, err := admin.pool.AcquireAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	stored := lease.Account().Config
	stored.Timezone = "Bad/Zone"
	if err := lease.SaveConfig(stored); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	keep := manualDisableInput(accountID, false)
	keep.Timezone = "Bad/Zone"
	if _, err := admin.UpdateAccount(context.Background(), accountID, keep); err != nil {
		t.Fatalf("原样保存早先的时区不应被拒：%v", err)
	}
	keep.Enabled = true
	if _, err := admin.UpdateAccount(context.Background(), accountID, keep); err != nil {
		t.Fatalf("原样保存早先的时区不应被拒：%v", err)
	}
	waitFor(t, "启用收尾结束", func() bool { return !admin.transitions.active(accountID) })
	if status := testStatus(t, admin.pool, accountID); status.Timezone != "Bad/Zone" || !status.Enabled {
		t.Fatalf("不应静默改写已有账户的时区：%+v", status)
	}
}
