package app

import (
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 已有账户重新上传认证文件（覆盖 storage-state.json）时的判断，见 accountWatcher.detectLoginUpdates：
//   - 账户处于失效类状态（见 reauthFailure）时，认证文件被外部重新写入就按重新上传处理：内容变了载入新文件；内容与当前登录状态
//     相同（例如在官网解开了 403、Cookie 没变就重新上传了同一份）也清除这个账户的旧判定、重新认证并加入验证队列，原先这种情况
//     什么都不做，账户一直显示原来的失效原因
//   - 账户本来就健康、内容又没变：不重新认证，只写一行“内容与当前登录状态相同”的提示
//   - 只多了、换了或缺了 Google 轮换的 Cookie、AI Studio 的 localStorage（共用账户目录的另一个进程回写）：不算重新上传
//     （见 compareAuthFile）；文件逐字节就是本服务读入或写过的那一份时除外（原样重新上传）

// reauthFailure 返回账户当前的失效类状态（重新上传认证文件时要清除的旧判定），健康时为空：
//   - 需要登录的启用账户；停用但不是手动停用的账户（新账户自动处理或重新验证未通过、出错后自动停用，或没有停用记录）
//   - 组织停用或受限（登记表或账户池的标记），403 无权限登记（含需要验证账号的账户级登记）
//   - 冷却里仍生效的：需要验证账号的整户暂停、单个模型需要验证、403 无权限的单模型冷却与多模型自动暂停、疑似登录失效
//
// 手动停用本身不算（只换认证、保持停用，见 applyLoginUpdateMode），手动停用的账户同时有上面的旧判定时照样算。
// 只在认证文件有变化时调用（一次账户状态查询），不在请求路径上
func (admin *runtimeAdmin) reauthFailure(summary aistudio.AccountLoginSummary) string {
	if summary.Enabled && summary.State == aistudio.AccountAuthRequired {
		return "需要登录"
	}
	if summary.OrgDisabled || admin.service.orgDisabledListed(summary.ID) {
		return aistudio.RestrictionReason(admin.service.orgDisabledKind(summary.ID))
	}
	if !summary.Enabled && !accountDisableNotes.keepDisabled(summary.ID) {
		return "自动停用"
	}
	if status, ok := admin.pool.StatusOf(summary.ID); ok {
		if label := failureCooldownLabel(status.Cooldowns, time.Now()); label != "" {
			return label
		}
	}
	if _, listed := admin.service.permissionDeniedIdentity(summary.ID); listed {
		return "403 无权限"
	}
	if _, listed := admin.service.orgDisabledIdentity(summary.ID); listed {
		return "组织停用"
	}
	return ""
}

// failureCooldownLabel 返回冷却里仍生效的失效类冷却（按 reauthFailure 的顺序取最重的一种），没有时为空
func failureCooldownLabel(cooldowns map[string]aistudio.CooldownState, now time.Time) string {
	labels := [...]string{"需要验证账号（整户暂停）", "单个模型需要验证", "403 无权限冷却", "疑似登录失效"}
	best := len(labels)
	for scope, cooldown := range cooldowns {
		if !cooldown.Active(now) {
			continue
		}
		rank := len(labels)
		switch {
		case verifyRequiredPauseCooldown(scope, cooldown):
			rank = 0
		case verifyCooldown(scope, cooldown):
			rank = 1
		case forbiddenCooldown(scope, cooldown):
			rank = 2
		case strings.HasPrefix(cooldown.Reason, authSuspectReasonPrefix):
			rank = 3
		}
		best = min(best, rank)
	}
	if best == len(labels) {
		return ""
	}
	return labels[best]
}

// authFileChange 为磁盘上的认证文件与内存中登录状态的关系（见 compareAuthFile）
type authFileChange int

const (
	// authFileChanged 内容变了：登录签名或摘要（不计轮换 Cookie 与 AI Studio 的 localStorage，见 storageStateDigest）不同
	authFileChanged authFileChange = iota
	// authFileSame 与当前登录状态相同：完全相同，或文件逐字节就是本服务读入或写过的那一份（见 aistudio.AccountPool.StorageFileKnown）
	authFileSame
	// authFileWriteBack 只多了或换了轮换 Cookie、AI Studio 的 localStorage：共用账户目录的另一个进程回写，不算重新上传
	authFileWriteBack
	// authFileOlder 只是轮换 Cookie 或 AI Studio 的 localStorage 比当前登录状态旧（或缺了几项），又不是本服务读入或写过的那一份：
	// 共用账户目录的另一个进程也会这样回写（它的浏览器里还是较早的轮换 Cookie、localStorage 整份替换，本进程更新的值还暂存在内存），
	// 与重新上传较早导出的文件分不开，按回写处理（失效账户写一行提示，见 detectLoginUpdates）
	authFileOlder
)

// compareAuthFile 判断磁盘上的认证文件 disk 与内存中的登录状态 memory 的关系；known 为磁盘上的文件逐字节就是本服务读入或写过的那一份
// （见 aistudio.AccountPool.StorageFileKnown）。摘要相同时只差轮换 Cookie 与 AI Studio 的 localStorage：
//   - 完全相同，或 known：authFileSame（原样重新上传。known 时不管轮换 Cookie 新旧：例如本服务载入之后又收到并写回了更新的轮换
//     Cookie，再原样上传最初那一份）
//   - 磁盘多出内存没有的轮换 Cookie 或 localStorage 项，或同一个轮换 Cookie 的值变了、又看不出磁盘上的更旧（过期时间不早于内存里的）：
//     回写只会增加或更新这些内容（MergeSetCookieHeaders、MergeBrowserLocalStorage），按回写处理（authFileWriteBack）
//   - 否则（磁盘缺少内存里有的项、轮换 Cookie 的过期时间更早）：authFileOlder。原先按相同处理，共用账户目录的另一个进程回写较早的
//     轮换 Cookie（它的那次响应早于本进程最近一次）时，失效账户被当成重新上传：清除 403 登记与冷却、关闭 Worker、排队验证，
//     每次回写都这样，冷却档位从头计；健康账户每次都写一行“内容相同”的提示
//   - 只有 localStorage 的值不同（看不出新旧）：按回写处理
func compareAuthFile(disk aistudio.StorageState, memory aistudio.StorageState, known bool) authFileChange {
	if aistudio.LoginIdentity(disk) != aistudio.LoginIdentity(memory) || storageStateDigest(disk) != storageStateDigest(memory) {
		return authFileChanged
	}
	if known {
		return authFileSame
	}
	diskCookies, memoryCookies := rotatingCookieSet(disk), rotatingCookieSet(memory)
	newer, older, changed := false, false, false
	for key, cookie := range diskCookies {
		previous, exists := memoryCookies[key]
		switch {
		case !exists:
			newer = true
		case cookie == previous:
		case cookie.Expires > 0 && previous.Expires > 0 && cookie.Expires < previous.Expires:
			older = true
		default:
			newer = true
		}
	}
	for key := range memoryCookies {
		if _, exists := diskCookies[key]; !exists {
			older = true
		}
	}
	diskItems, memoryItems := browserLocalStorageItems(disk), browserLocalStorageItems(memory)
	for name, value := range diskItems {
		previous, exists := memoryItems[name]
		switch {
		case !exists:
			newer = true
		case previous != value:
			changed = true
		}
	}
	for name := range memoryItems {
		if _, exists := diskItems[name]; !exists {
			older = true
		}
	}
	switch {
	case !newer && !older && !changed:
		return authFileSame
	case newer:
		return authFileWriteBack
	case older:
		return authFileOlder
	default:
		return authFileWriteBack
	}
}

// rotatingCookieSet 返回登录状态里的轮换 Cookie（见 rotatingCookies），按名称、域（不分大小写）与路径区分
func rotatingCookieSet(state aistudio.StorageState) map[string]aistudio.StateCookie {
	result := make(map[string]aistudio.StateCookie)
	for _, cookie := range state.Cookies {
		if _, rotating := rotatingCookies[cookie.Name]; rotating {
			result[cookie.Name+"\x00"+strings.ToLower(cookie.Domain)+"\x00"+cookie.Path] = cookie
		}
	}
	return result
}

// browserLocalStorageItems 返回 AI Studio 的 localStorage 项（见 aistudio.BrowserLocalStorageOrigin）
func browserLocalStorageItems(state aistudio.StorageState) map[string]string {
	result := make(map[string]string)
	for _, origin := range state.Origins {
		if origin.Origin != aistudio.BrowserLocalStorageOrigin {
			continue
		}
		for _, item := range origin.LocalStorage {
			result[item.Name] = item.Value
		}
	}
	return result
}
