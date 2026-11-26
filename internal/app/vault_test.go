package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/vault"
)

const (
	vaultAppPassword = "Fake-Pass-App-0001"
	vaultAppNote     = "Fake-Note-App-0002"
	vaultAppKey      = "JBSWY3DPEHPK3PXP"
	vaultAppSource   = "来源名-Fake-Source.txt"
)

// vaultTestManager 为带临时 .env 与运行日志的管理器（数据目录为 <临时目录>/runtime/account-vault）
func vaultTestManager(t *testing.T) (*runtimeManager, string) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &runtimeManager{configPath: filepath.Join(dir, ".env"), requests: newRequestRegistry(ctx)}, filepath.Join(dir, "runtime", "account-vault")
}

// vaultLogEntries 返回来源为 auth、以账号资料或家庭组开头的运行日志
func vaultLogEntries(requests *requestRegistry) []api.AdminLog {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	var entries []api.AdminLog
	for _, entry := range requests.logs {
		if strings.HasPrefix(entry.Message, "账号资料") || strings.HasPrefix(entry.Message, "家庭组") {
			entries = append(entries, entry)
		}
	}
	return entries
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestVaultPathFromConfig 文件落在配置文件所在目录下的 runtime/account-vault/；configPath 为空时只在内存，包目录不出现新文件
func TestVaultPathFromConfig(t *testing.T) {
	manager, dir := vaultTestManager(t)
	ctx := context.Background()
	if _, err := manager.ImportProfiles(ctx, vault.ProfileImportInput{Sources: []vault.ImportSource{{Text: "a@example.com----" + vaultAppPassword}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles.json")); err != nil {
		t.Fatalf("资料文件应在 %s: %v", dir, err)
	}
	view, err := manager.Vault(ctx, "")
	if err != nil || view.Dir != dir || !view.Persistent {
		t.Fatalf("目录 = %q persistent=%v err=%v", view.Dir, view.Persistent, err)
	}
	cwd, _ := os.Getwd()
	before := listNames(t, cwd)
	memory := &runtimeManager{}
	if _, err := memory.ImportProfiles(ctx, vault.ProfileImportInput{Sources: []vault.ImportSource{{Text: "a@example.com----" + vaultAppPassword}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.ImportFamilies(ctx, vault.FamilyImportInput{Text: "a@example.com\nb@example.com"}); err != nil {
		t.Fatal(err)
	}
	if view, _ := memory.Vault(ctx, ""); view.Persistent || len(view.Profiles) != 1 {
		t.Fatalf("只在内存 = %+v", view)
	}
	if after := listNames(t, cwd); !slices.Equal(before, after) {
		t.Fatalf("包目录不应出现新文件: %v → %v", before, after)
	}
}

// TestVaultLogsOnlyCounts 写操作的运行日志只有计数与字段名（来源 auth），不含邮箱、密码、密钥、备注、验证码、来源名；预览与取出不写日志
func TestVaultLogsOnlyCounts(t *testing.T) {
	manager, _ := vaultTestManager(t)
	ctx := context.Background()
	text := "a@example.com----" + vaultAppPassword + "----r@example.com----" + vaultAppKey + "----" + vaultAppNote + "\nbad line\n"
	sources := []vault.ImportSource{{Name: vaultAppSource, Text: text}}
	if _, err := manager.ImportProfiles(ctx, vault.ProfileImportInput{DryRun: true, Sources: sources}); err != nil {
		t.Fatal(err)
	}
	if entries := vaultLogEntries(manager.requests); len(entries) != 0 {
		t.Fatalf("预览不应写日志: %+v", entries)
	}
	if _, err := manager.ImportProfiles(ctx, vault.ProfileImportInput{Sources: sources}); err != nil {
		t.Fatal(err)
	}
	secret, err := manager.RevealProfile(ctx, vault.RevealInput{Email: "a@example.com"})
	if err != nil || secret.TOTPCode == nil {
		t.Fatalf("取出 err=%v", err)
	}
	if _, err := manager.UpdateProfile(ctx, vault.ProfileUpdate{Email: "a@example.com", Password: &[]string{"Fake-Pass-App-0003"}[0], Clear: []string{"note"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.UpdateProfile(ctx, vault.ProfileUpdate{Email: "a@example.com", Password: &[]string{"Fake-Pass-App-0003"}[0]}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ExportProfiles(ctx, vault.ExportInput{All: true, IncludeNote: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.DeleteProfiles(ctx, vault.ProfileDeleteInput{Emails: []string{"a@example.com", "zz@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ImportFamilies(ctx, vault.FamilyImportInput{DryRun: true, Text: "main@example.com\nm1@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ImportFamilies(ctx, vault.FamilyImportInput{Text: "main@example.com\nm1@example.com\n\nx@example.com\ny@example.com"}); err != nil {
		t.Fatal(err)
	}
	created, err := manager.CreateFamily(ctx, vault.FamilyCreate{Primary: "n@example.com", Members: []string{"m1@example.com", "n1@example.com"}})
	if err != nil || created.Status != "created" {
		t.Fatalf("新建 = %+v %v", created, err)
	}
	view, _ := manager.Vault(ctx, "")
	updated, err := manager.UpdateFamily(ctx, view.Families[0].ID, vault.FamilyUpdate{Name: &[]string{"张家"}[0], Members: &[]string{"y@example.com", "z@example.com"}})
	if err != nil || updated.Status != "replaced" {
		t.Fatalf("编辑 = %+v %v", updated, err)
	}
	if _, err := manager.DissolveFamily(ctx, view.Families[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, entry := range vaultLogEntries(manager.requests) {
		if entry.Source != "auth" || entry.Level != "INFO" {
			t.Fatalf("来源与级别 = %s %s", entry.Source, entry.Level)
		}
		messages = append(messages, entry.Message)
	}
	want := []string{
		"账号资料导入 | 模式=合并 | 行=2 | 新增=1 | 更新=0 | 未变化=0 | 无法识别=1 | 重复行=0 | 覆盖已有=- | 已核对可疑提示=否",
		"账号资料已修改 | 新建=否 | 修改=密码 | 清空=备注",
		"账号资料导出 | 条数=1 | 不存在=0 | 含备注=是 | Tab 分隔=0 | 不能完全还原=0",
		"账号资料已删除 | 删除=1 | 不存在=1",
		"家庭组录入 | 模式=按块 | 块=2 | 新建=2 | 替换成员=0 | 换主=0 | 未变化=0 | 拒绝=0 | 无法识别=0 | 移动邮箱=0 | 移出=0 | 自动解散=0",
		"家庭组已新建 | 成员=3 | 移动邮箱=1 | 自动解散=1",
		"家庭组已修改 | 新增成员=1 | 移出成员=0 | 换主=否 | 改名=是 | 改色=否 | 移动邮箱=0 | 自动解散=0",
		"家庭组已解散 | 成员=3",
	}
	if !slices.Equal(messages, want) {
		t.Fatalf("日志 =\n%s\n期望\n%s", strings.Join(messages, "\n"), strings.Join(want, "\n"))
	}
	all := strings.Join(serviceLogMessages(manager.requests), "\n")
	for _, secret := range []string{"@example.com", vaultAppPassword, "Fake-Pass-App-0003", vaultAppKey, vaultAppNote, vaultAppSource, secret.TOTPCode.Code, "张家"} {
		if strings.Contains(all, secret) {
			t.Fatalf("日志不应含 %q: %s", secret, all)
		}
	}
}

// TestVaultSurvivesAccountDeletion 删除账户（单个、批量、目录被移走）不动账号资料与家庭组
func TestVaultSurvivesAccountDeletion(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, _ := deleteTestAdmin(t, "a@example.com", "b@example.com", "c@example.com")
	dir := t.TempDir()
	manager := &runtimeManager{configPath: filepath.Join(dir, ".env"), requests: admin.requests, current: &runtimeGeneration{admin: admin}}
	ctx := context.Background()
	text := "a@example.com----" + vaultAppPassword + "\nb@example.com----" + vaultAppPassword + "----" + vaultAppKey + "\nc@example.com----" + vaultAppPassword
	if _, err := manager.ImportProfiles(ctx, vault.ProfileImportInput{Sources: []vault.ImportSource{{Text: text}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ImportFamilies(ctx, vault.FamilyImportInput{Text: "a@example.com\nb@example.com\nc@example.com"}); err != nil {
		t.Fatal(err)
	}
	vaultDir := filepath.Join(dir, "runtime", "account-vault")
	profiles, _ := os.ReadFile(filepath.Join(vaultDir, "profiles.json"))
	families, _ := os.ReadFile(filepath.Join(vaultDir, "families.json"))
	// 管理接口实际调用的是管理器上的这两个方法（转给当前生成服务）；目录被移走走 detachAccount
	if err := manager.DeleteAccount(ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if result, err := manager.DeleteAccounts(ctx, api.BulkDeleteInput{AccountIDs: []string{"b@example.com"}}); err != nil || result.Deleted != 1 {
		t.Fatalf("批量删除 = %+v %v", result, err)
	}
	if err := admin.detachAccount("c@example.com"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(vaultDir, "profiles.json")); !bytes.Equal(got, profiles) {
		t.Fatal("删除账户不应改动账号资料")
	}
	if got, _ := os.ReadFile(filepath.Join(vaultDir, "families.json")); !bytes.Equal(got, families) {
		t.Fatal("删除账户不应改动家庭组")
	}
	view, err := manager.Vault(ctx, "")
	if err != nil || len(view.Profiles) != 3 || len(view.Families) != 1 || len(view.Families[0].Members) != 2 {
		t.Fatalf("删除账户后 = %+v %v", view, err)
	}
}

// TestVaultNoStartupIO 装配与启动不打开 Store：没有 runtime/account-vault/，没有以“账号资料”“家庭组”开头的日志
func TestVaultNoStartupIO(t *testing.T) {
	cfg := wiringConfig(t, false)
	cfg.WAABackend = config.WAABackendGo
	manager, _, path := newWiringManager(t, cfg, true)
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "runtime", "account-vault")); !os.IsNotExist(err) {
		t.Fatalf("启动不应建出资料目录: %v", err)
	}
	if entries := vaultLogEntries(manager.requests); len(entries) != 0 {
		t.Fatalf("启动不应写资料日志: %+v", entries)
	}
	if manager.vault.store != nil {
		t.Fatal("启动不应打开 Store")
	}
}

// TestVaultCorruptWarnOnce 坏文件连续读只有一行 WARN（auth、带路径、无内容）；主文件不见了而 .bak 在同样只有一行；从空开始后一行 INFO
func TestVaultCorruptWarnOnce(t *testing.T) {
	manager, dir := vaultTestManager(t)
	ctx := context.Background()
	if _, err := manager.ImportProfiles(ctx, vault.ProfileImportInput{Sources: []vault.ImportSource{{Text: "a@example.com----" + vaultAppPassword}}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"profiles":[{"email":"a@example.com","password":"`+vaultAppPassword+`"`), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if view, err := manager.Vault(ctx, ""); err != nil || len(view.Errors) != 1 {
			t.Fatalf("坏文件 = %+v %v", view.Errors, err)
		}
		// 写操作在锁内重读同一个坏文件，也不再写 WARN
		if _, err := manager.UpdateProfile(ctx, vault.ProfileUpdate{Email: "b@example.com", Password: &[]string{vaultAppPassword}[0]}); err == nil {
			t.Fatal("坏文件时写操作应 409")
		}
	}
	warns := func() []api.AdminLog {
		var list []api.AdminLog
		for _, entry := range vaultLogEntries(manager.requests) {
			if entry.Level == "WARN" {
				list = append(list, entry)
			}
		}
		return list
	}
	if list := warns(); len(list) != 1 || list[0].Source != "auth" || !strings.Contains(list[0].Message, path) || strings.Contains(list[0].Message, vaultAppPassword) ||
		!strings.HasPrefix(list[0].Message, "账号资料文件无法读取，已停止写入以免覆盖") {
		t.Fatalf("WARN = %+v", list)
	}
	// 恢复后再写一次（有了 .bak），删掉主文件：同样只有一行“不见了，但还有备份”
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ImportProfiles(ctx, vault.ProfileImportInput{Sources: []vault.ImportSource{{Text: "a@example.com----" + vaultAppPassword}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ImportProfiles(ctx, vault.ProfileImportInput{Sources: []vault.ImportSource{{Text: "a@example.com----Fake-Pass-App-0009"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_, _ = manager.Vault(ctx, "")
	}
	if list := warns(); len(list) != 2 || !strings.Contains(list[1].Message, "不见了，但还有备份") {
		t.Fatalf("WARN = %+v", list)
	}
	if _, err := manager.StartEmpty(ctx, "profiles"); err != nil {
		t.Fatal(err)
	}
	entries := vaultLogEntries(manager.requests)
	last := entries[len(entries)-1]
	if last.Level != "INFO" || !strings.HasPrefix(last.Message, "账号资料从空开始 | 原备份=已移到 snapshots/profiles-") {
		t.Fatalf("从空开始的日志 = %+v", last)
	}
}
