package app

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/vault"
)

// 账号资料与家庭组（见 internal/vault）：只用于管理页的查看、复制与分类显示，不碰生成服务、号池、Worker，
// 服务停止时照常可用。数据目录为配置文件所在目录下的 runtime/account-vault/（与 runtime/proxy-pool.json 同一推导），
// 删除账户与各种清理都不动它。运行日志只写计数与字段名，不写邮箱、来源名与任何内容；预览（dry_run）与取出不写日志

var _ api.VaultService = (*runtimeManager)(nil)

// vaultHolder 懒打开的 Store（零值可用）
type vaultHolder struct {
	once  sync.Once
	store *vault.Store
}

// vaultStore 第一次调用时按 configPath 打开 Store（Open 不做 I/O）；configPath 为空（测试直接构造的管理器）时只在内存
func (manager *runtimeManager) vaultStore() *vault.Store {
	manager.vault.once.Do(func() {
		dir := ""
		if manager.configPath != "" {
			dir = filepath.Join(filepath.Dir(manager.configPath), "runtime", "account-vault")
		}
		manager.vault.store = vault.Open(dir, vault.Options{
			Warn: func(message string) { manager.vaultLog("WARN", message) },
			Info: func(message string) { manager.vaultLog("INFO", message) },
		})
	})
	return manager.vault.store
}

// vaultLog 写运行日志（来源 auth）；WARN 很少出现又必须留下，走持久队列
func (manager *runtimeManager) vaultLog(level string, message string) {
	if manager.requests == nil {
		return
	}
	if level == "WARN" {
		manager.requests.logDurable("auth", level, message)
		return
	}
	manager.requests.log("auth", level, message)
}

var vaultFieldNames = map[string]string{"password": "密码", "recovery_email": "辅助邮箱", "totp": "2FA", "note": "备注"}

// vaultFieldList 为字段名列表的中文写法（“密码、2FA”），空时为 -
func vaultFieldList(fields []string) string {
	if len(fields) == 0 {
		return "-"
	}
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, vaultFieldNames[field])
	}
	return strings.Join(names, "、")
}

// vaultCounts 为“密码 N、辅助邮箱 N、2FA N、备注 N”中不为 0 的项，全为 0 时为 -
func vaultCounts(counts vault.FieldCounts) string {
	var parts []string
	for _, item := range []struct {
		name  string
		count int
	}{{"密码", counts.Password}, {"辅助邮箱", counts.RecoveryEmail}, {"2FA", counts.TOTP}, {"备注", counts.Note}} {
		if item.count > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", item.name, item.count))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "、")
}

// Vault 返回账号资料（打码）与家庭组
func (manager *runtimeManager) Vault(ctx context.Context, since string) (vault.View, error) {
	return manager.vaultStore().View(ctx, since)
}

// ImportProfiles 预览或确认导入账号资料
func (manager *runtimeManager) ImportProfiles(ctx context.Context, input vault.ProfileImportInput) (vault.ProfileImportResult, error) {
	result, err := manager.vaultStore().ImportProfiles(ctx, input)
	if err != nil || input.DryRun {
		return result, err
	}
	mode := "合并"
	if result.Mode == "fill_empty" {
		mode = "只补空字段"
	}
	summary := result.Summary
	manager.vaultLog("INFO", fmt.Sprintf("账号资料导入 | 模式=%s | 行=%d | 新增=%d | 更新=%d | 未变化=%d | 无法识别=%d | 重复行=%d | 覆盖已有=%s | 已核对可疑提示=%s",
		mode, summary.Lines, summary.New, summary.Updated, summary.Unchanged, summary.Invalid, summary.DuplicateLines,
		vaultCounts(summary.Overwrites), yesNo(len(summary.Suspect) > 0)))
	return result, nil
}

// UpdateProfile 编辑或新增一条账号资料
func (manager *runtimeManager) UpdateProfile(ctx context.Context, input vault.ProfileUpdate) (vault.ProfileUpdateResult, error) {
	result, err := manager.vaultStore().UpdateProfile(ctx, input)
	if err != nil || (!result.Created && len(result.Changed) == 0) {
		return result, err
	}
	var modified []string
	for _, field := range result.Changed {
		if !slices.Contains(result.Cleared, field) {
			modified = append(modified, field)
		}
	}
	manager.vaultLog("INFO", fmt.Sprintf("账号资料已修改 | 新建=%s | 修改=%s | 清空=%s", yesNo(result.Created), vaultFieldList(modified), vaultFieldList(result.Cleared)))
	return result, nil
}

// DeleteProfiles 删除账号资料（不动账户与家庭组）
func (manager *runtimeManager) DeleteProfiles(ctx context.Context, input vault.ProfileDeleteInput) (vault.ProfileDeleteResult, error) {
	result, err := manager.vaultStore().DeleteProfiles(ctx, input)
	if err == nil {
		manager.vaultLog("INFO", fmt.Sprintf("账号资料已删除 | 删除=%d | 不存在=%d", result.Deleted, result.Missing))
	}
	return result, err
}

// RevealProfile 取出明文（每次点复制都会调用，不写日志）
func (manager *runtimeManager) RevealProfile(ctx context.Context, input vault.RevealInput) (vault.ProfileSecret, error) {
	return manager.vaultStore().RevealProfile(ctx, input)
}

// ExportProfiles 导出整行（复制选中与导出 TXT 共用），写一行计数
func (manager *runtimeManager) ExportProfiles(ctx context.Context, input vault.ExportInput) (vault.ProfileExport, error) {
	export, err := manager.vaultStore().ExportProfiles(ctx, input)
	if err == nil {
		manager.vaultLog("INFO", fmt.Sprintf("账号资料导出 | 条数=%d | 不存在=%d | 含备注=%s | Tab 分隔=%d | 不能完全还原=%d",
			export.Count, len(export.Missing), yesNo(input.IncludeNote), export.TabLines, len(export.Lossy)))
	}
	return export, err
}

// ImportFamilies 预览或确认录入家庭组
func (manager *runtimeManager) ImportFamilies(ctx context.Context, input vault.FamilyImportInput) (vault.FamilyImportResult, error) {
	result, err := manager.vaultStore().ImportFamilies(ctx, input)
	if err != nil || input.DryRun {
		return result, err
	}
	mode := "按块"
	if result.Mode == "lines" {
		mode = "每行一组"
	}
	summary := result.Summary
	manager.vaultLog("INFO", fmt.Sprintf("家庭组录入 | 模式=%s | 块=%d | 新建=%d | 替换成员=%d | 换主=%d | 未变化=%d | 拒绝=%d | 无法识别=%d | 移动邮箱=%d | 移出=%d | 自动解散=%d",
		mode, summary.Blocks, summary.Created, summary.Replaced, summary.PrimaryChanged, summary.Unchanged, summary.Rejected, summary.Invalid,
		summary.Moved, summary.Removed, summary.Dissolved))
	return result, nil
}

// CreateFamily 新建家庭组（主邮箱已是某组主邮箱时按替换处理，日志写“已修改”）
func (manager *runtimeManager) CreateFamily(ctx context.Context, input vault.FamilyCreate) (vault.FamilyBlockResult, error) {
	result, err := manager.vaultStore().CreateFamily(ctx, input)
	if err != nil || input.DryRun {
		return result, err
	}
	switch result.Status {
	case "created":
		manager.vaultLog("INFO", fmt.Sprintf("家庭组已新建 | 成员=%d | 移动邮箱=%d | 自动解散=%d", 1+len(result.Members), len(result.Moved), len(result.Dissolved)))
	case "replaced", "primary_changed":
		manager.logFamilyChange(result)
	}
	return result, nil
}

// UpdateFamily 编辑家庭组（改名、改色、换主、改成员）
func (manager *runtimeManager) UpdateFamily(ctx context.Context, id string, input vault.FamilyUpdate) (vault.FamilyBlockResult, error) {
	result, err := manager.vaultStore().UpdateFamily(ctx, id, input)
	if err != nil || input.DryRun {
		return result, err
	}
	switch result.Status {
	case "replaced", "primary_changed", "updated":
		manager.logFamilyChange(result)
	}
	return result, nil
}

func (manager *runtimeManager) logFamilyChange(result vault.FamilyBlockResult) {
	manager.vaultLog("INFO", fmt.Sprintf("家庭组已修改 | 新增成员=%d | 移出成员=%d | 换主=%s | 改名=%s | 改色=%s | 移动邮箱=%d | 自动解散=%d",
		len(result.Added), len(result.Removed), yesNo(result.Status == "primary_changed"), yesNo(result.Renamed), yesNo(result.Recolored),
		len(result.Moved), len(result.Dissolved)))
}

// DissolveFamily 解散家庭组
func (manager *runtimeManager) DissolveFamily(ctx context.Context, id string, rev *int64) (vault.FamilyDissolveResult, error) {
	result, err := manager.vaultStore().DissolveFamily(ctx, id, rev)
	if err == nil {
		manager.vaultLog("INFO", fmt.Sprintf("家庭组已解散 | 成员=%d", result.Members))
	}
	return result, err
}

// StartEmpty 主文件缺失而有备份时“从空开始”
func (manager *runtimeManager) StartEmpty(ctx context.Context, file string) (vault.StartEmptyResult, error) {
	result, err := manager.vaultStore().StartEmpty(ctx, file)
	if err != nil {
		return result, err
	}
	label := "账号资料"
	if result.File == "families" {
		label = "家庭组"
	}
	moved := "没有"
	if result.MovedBackup != "" {
		moved = "已移到 snapshots/" + filepath.Base(result.MovedBackup)
	}
	manager.vaultLog("INFO", fmt.Sprintf("%s从空开始 | 原备份=%s", label, moved))
	return result, nil
}
