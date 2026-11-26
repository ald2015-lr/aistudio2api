package app

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 服务停止期间重新上传的认证文件（见 reconcileAuthFiles）：生成服务启动时账户池直接载入磁盘上的认证文件，内存与磁盘一致，
// 账户目录扫描第一次看到的文件只记作基准，403 登记与组织停用登记的启动对账只比较登录签名。原先服务停止期间（例如停服、上传、
// 更新程序再启动）同一会话重新导出或原样重新上传的认证文件，启动后认不出来：403 登记与冷却、需要验证账号的暂停都留着，也不排队验证；
// 换了登录的只清除登记、不排队验证，面板与日志都没有“重新上传后”的结果。现在生成服务正常关闭时，把各账户认证文件的状态（本服务
// 最近一次读入或写入之后的修改时间、大小与原始内容指纹，以及当时内存里登录状态的登录签名与内容摘要）写进模型目录缓存同目录的
// auth-files.json，下次启动时对照：
//   - 修改时间或大小变了的，是服务停止期间被重新写入的。处于失效状态的账户（见 reauthFailure）内容变了或逐字节相同，都按重新上传处理：
//     清除这个账户的旧判定、加入验证队列，结果照常写“重新上传后：…”；只差轮换 Cookie 与 AI Studio 的 localStorage 的按共用账户目录的
//     其他进程回写处理（写一行提示，计入面板“未按重新上传处理”）。状态正常的账户换了登录的加入验证队列，内容相同或登录签名没变的
//     只计入面板（与运行中相同）
//   - 启动对账按登录签名清除了 403 或组织停用登记的账户（见 reconcilePermissionDenied、reconcileOrgDisabled）同样加入验证队列，
//     没有这份记录时也一样
//
// 记录在启动读取后删除：只有正常关闭时写出的才可信，进程被强杀、断电之后没有记录，启动时不对照（与原来相同，不会把本服务自己
// 写回的文件当成重新上传）。同一进程里重建生成服务（启停服务、改了需要重建的设置）时只由最新的实例写出（见 authFileStampsOwner）。
// 更新到有这份记录的版本之前，旧版本没有写过记录，第一次启动时同样不对照

const (
	// authFileStampsName 为记录文件名（放在模型目录缓存的目录里，每个进程一份）
	authFileStampsName = "auth-files.json"
	// authFileStampsVersion 为记录文件的格式版本，不符时忽略
	authFileStampsVersion = 1
)

// authFileStampsOwner 为最近一次装配的生成服务实例的编号：重建生成服务时新实例先装配、旧实例后关闭，只让最新的实例在关闭时写出记录，
// 旧实例关闭时写出的（之后新实例还会改写认证文件）进程被强杀后会被下次启动当成服务停止期间的重新上传。旧实例关闭时反而删除已有的
// 记录：新实例装配后启动被取消、先于旧实例关闭时写下的记录，旧实例之后还改写过认证文件，已经过时
var authFileStampsOwner atomic.Uint64

// authFileStampsPath 返回记录文件的路径
func authFileStampsPath() string {
	return filepath.Join(catalogCacheDir, authFileStampsName)
}

// authFileStamp 为一个账户认证文件的记录（字段取短名）
type authFileStamp struct {
	// ModTime 与 Size 为本服务最近一次读入或写入认证文件之后的修改时间（unix 纳秒）与大小
	ModTime int64 `json:"m"`
	Size    int64 `json:"s"`
	// Digest 为那份文件原始内容的 SHA-256（hex）
	Digest string `json:"d"`
	// Identity 与 Content 为当时内存里登录状态的登录签名（aistudio.LoginIdentity）与内容摘要（storageStateDigest，不计轮换 Cookie
	// 与 AI Studio 的 localStorage）
	Identity string `json:"i,omitempty"`
	Content  string `json:"c,omitempty"`
}

// authFileStampsFile 为 auth-files.json
type authFileStampsFile struct {
	Version  int                      `json:"version"`
	SavedAt  time.Time                `json:"saved_at"`
	Accounts map[string]authFileStamp `json:"accounts"`
}

// saveAuthFileStamps 在生成服务关闭时（Worker 都已关闭、暂存的 Cookie 已写盘之后）写出各账户认证文件的记录；token 不是最近一次装配的
// 实例时不写，并删除已有的记录（见 authFileStampsOwner）。账户没有记下文件状态（载入时文件正在被改写等）的不写进记录，下次启动不对照它
func saveAuthFileStamps(path string, pool *aistudio.AccountPool, token uint64, requests *requestRegistry) {
	if path == "" || pool == nil {
		return
	}
	if authFileStampsOwner.Load() != token {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && requests != nil {
			requests.log("service", "WARN", "认证文件状态记录删除失败 | 错误="+strings.TrimSpace(err.Error()))
		}
		return
	}
	accounts := make(map[string]authFileStamp)
	for _, summary := range pool.LoginSummaries() {
		stamp, synced := pool.StorageFileStampOf(summary.ID)
		state, exists := pool.StorageStateOf(summary.ID)
		if !synced || !exists {
			continue
		}
		accounts[summary.ID] = authFileStamp{
			ModTime: stamp.ModTime.UnixNano(), Size: stamp.Size, Digest: hex.EncodeToString(stamp.Digest[:]),
			Identity: aistudio.LoginIdentity(state), Content: storageStateDigest(state),
		}
	}
	data, err := json.Marshal(authFileStampsFile{Version: authFileStampsVersion, SavedAt: time.Now().UTC(), Accounts: accounts})
	if err == nil {
		err = writeFileAtomic(path, data, 0o600)
	}
	if err != nil && requests != nil {
		requests.log("service", "WARN", "认证文件状态记录写入失败，下次启动不对照服务停止期间重新上传的认证文件 | 错误="+strings.TrimSpace(err.Error()))
	}
}

// loadAuthFileStamps 读出上次正常关闭时写的记录并删除文件（见文件开头）；没有、读不出来或版本不符时返回 nil（读不出来时写一行警告）
func loadAuthFileStamps(path string, requests *requestRegistry) map[string]authFileStamp {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if removeErr := os.Remove(path); removeErr != nil && requests != nil {
		requests.log("service", "WARN", "认证文件状态记录删除失败 | 错误="+strings.TrimSpace(removeErr.Error()))
	}
	var file authFileStampsFile
	if err := json.Unmarshal(data, &file); err != nil || file.Version != authFileStampsVersion {
		if requests != nil {
			requests.log("service", "WARN", "认证文件状态记录损坏或版本不符，这次启动不对照服务停止期间重新上传的认证文件")
		}
		return nil
	}
	return file.Accounts
}

// authFileRewrite 为服务停止期间认证文件被重新写入的方式（见 classifyAuthFileRewrite）
type authFileRewrite int

const (
	// authFileNotRewritten 没有被重新写入（修改时间与大小都与记录相同），或没有记录、比不了
	authFileNotRewritten authFileRewrite = iota
	// authFileRewrittenChanged 内容变了：登录签名或内容摘要（不计轮换 Cookie 与 AI Studio 的 localStorage）与记录不同
	authFileRewrittenChanged
	// authFileRewrittenIdentical 逐字节与记录相同（原样重新上传、touch）
	authFileRewrittenIdentical
	// authFileRewrittenRotating 只差轮换 Cookie 与 AI Studio 的 localStorage：与共用账户目录的其他进程回写分不开
	authFileRewrittenRotating
)

// classifyAuthFileRewrite 对照账户认证文件上次正常关闭时的记录 previous 与这次启动载入时记下的文件状态 current（state 为载入的内容）
func classifyAuthFileRewrite(previous authFileStamp, current aistudio.StorageFileStamp, state aistudio.StorageState) authFileRewrite {
	if previous.ModTime == current.ModTime.UnixNano() && previous.Size == current.Size {
		return authFileNotRewritten
	}
	switch {
	case previous.Identity != aistudio.LoginIdentity(state) || previous.Content != storageStateDigest(state):
		return authFileRewrittenChanged
	case previous.Digest == hex.EncodeToString(current.Digest[:]):
		return authFileRewrittenIdentical
	default:
		return authFileRewrittenRotating
	}
}

// reconcileAuthFiles 在启动时（账户池与登记表都已载入、对账之后，新账户自动处理队列建好之后、开始处理与扫描之前）对照上次正常关闭时
// 写下的认证文件记录 previous（可为 nil），处理服务停止期间被重新写入的认证文件（见文件开头）；reconciled 为启动对账按登录签名清除了
// 403 或组织停用登记的账户。启动时还没有 Worker 与请求，不用账户租约；状态正常、内容相同或登录签名没变的只写一行汇总
func (admin *runtimeAdmin) reconcileAuthFiles(previous map[string]authFileStamp, reconciled []string) {
	if admin == nil || admin.pool == nil {
		return
	}
	cleared := make(map[string]struct{}, len(reconciled))
	for _, id := range reconciled {
		cleared[id] = struct{}{}
	}
	summaries := admin.pool.LoginSummaries()
	sort.Slice(summaries, func(left, right int) bool { return summaries[left].ID < summaries[right].ID })
	reuploaded, relogin, unchanged, sameLogin, writeBack := 0, 0, 0, 0, 0
	for _, summary := range summaries {
		stamp, recorded := previous[summary.ID]
		rewrite := authFileNotRewritten
		var state aistudio.StorageState
		if recorded {
			current, synced := admin.pool.StorageFileStampOf(summary.ID)
			loaded, exists := admin.pool.StorageStateOf(summary.ID)
			if synced && exists {
				state = loaded
				rewrite = classifyAuthFileRewrite(stamp, current, state)
			}
		}
		if _, ok := cleared[summary.ID]; ok {
			// 启动对账已按登录签名清除了 403 或组织停用登记：换了登录，按重新上传处理（其余旧判定一并清除，加入验证队列）
			admin.reuploadedWhileStopped(summary.ID, "403 无权限或组织停用（登录签名与登记时不同，启动对账已清除登记）")
			reuploaded++
			continue
		}
		if rewrite == authFileNotRewritten {
			continue
		}
		failure := admin.reauthFailure(summary)
		switch {
		case failure != "" && rewrite == authFileRewrittenRotating:
			admin.requests.log("auth", "INFO", fmt.Sprintf(
				"服务停止期间认证文件被重新写入，但只差 Google 轮换的 Cookie 或 AI Studio 的 localStorage：按共用账户目录的其他进程回写处理，"+
					"未清除旧判定、未重新认证 | 账户=%s | 状态=%s | 如果是你重新上传的，请在管理页对该账户点“验证”，或重新导出后再上传",
				summary.ID, failure,
			))
			admin.onboard.noteNotReuploaded()
			writeBack++
		case failure != "":
			admin.reuploadedWhileStopped(summary.ID, failure)
			reuploaded++
		case rewrite == authFileRewrittenChanged && stamp.Identity != aistudio.LoginIdentity(state):
			// 状态正常的账户换了登录：与运行中相同，加入验证队列用新的 Cookie 验证一次
			admin.queueReauthIfEnabled(summary.ID, reauthSourceStopped)
			relogin++
		case rewrite == authFileRewrittenChanged:
			admin.onboard.noteSameLogin()
			sameLogin++
		case rewrite == authFileRewrittenIdentical:
			admin.onboard.noteUnchanged()
			unchanged++
		}
	}
	if reuploaded+relogin+unchanged+sameLogin+writeBack == 0 {
		return
	}
	admin.requests.log("auth", "INFO", fmt.Sprintf(
		"启动时对照认证文件 | 服务停止期间重新上传、按重新上传处理 %d 个 | 状态正常换了登录、加入验证 %d 个 | 状态正常内容相同 %d 个 | "+
			"状态正常登录签名没变 %d 个 | 失效账户只差轮换 Cookie 或 localStorage %d 个",
		reuploaded, relogin, unchanged, sameLogin, writeBack,
	))
}

// reuploadedWhileStopped 把服务停止期间换上认证文件的失效账户按重新上传处理（failure 为它的失效状态，写进日志）：与运行中重新上传相同，
// 清除这个账户的 403 无权限登记与冷却、需要验证账号的暂停、组织停用或受限、疑似登录失效冷却与登录复核记录，再加入验证队列
// （手动停用的只记为跳过）。账户池载入的就是新文件、还没有 Worker，不用关闭浏览器与推进认证代际
func (admin *runtimeAdmin) reuploadedWhileStopped(accountID string, failure string) {
	admin.service.forgetPermissionDenied(accountID, reauthSourceStopped)
	admin.service.noteLoginRestored(accountID)
	admin.requests.log("auth", "INFO", fmt.Sprintf(
		"服务停止期间认证文件已重新上传，已清除旧判定，加入验证 | 账户=%s | 状态=%s", accountID, failure,
	))
	admin.queueReauthIfEnabled(accountID, reauthSourceStopped)
}
