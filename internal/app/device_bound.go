package app

import (
	"fmt"
	"strings"
	"sync"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 设备绑定会话（DBSC）：在开了设备绑定的浏览器里登录的 Google 会话带 __Secure-1PSIDRTS，会话要定期用浏览器设备里的密钥
// 证明自己才能续期，复制出来的认证文件约 2 小时后集中失效（上游在 Cookie 导入时同样按它标 device_bound）。载入认证文件时统计
// 带这个 Cookie 的账户、写一行 WARN，批量导入的账户一起变成需要登录时能看出原因。只读 Cookie 名与认证扩展里有没有 OAuth 续签材料，
// 不读也不写 Cookie 与续签材料的值。
// 本服务的 Chrome 导入（OAuthMultilogin 加设备绑定 assertion）在强制设备绑定的账户上签发的 Cookie 可能同样带 __Secure-1PSIDRTS，
// 但认证扩展里保存了 Chrome OAuth 续签材料，在能原地续签的平台上（authRenewPlatformSupported，目前只有 Windows）遇到 401 会原地续签，
// 不算“复制出来会失效”：这类账户单独计数、不计入 WARN 的数量与名单，提示语也按平台给出
const (
	// deviceBoundCookie 为设备绑定会话的 Cookie 名
	deviceBoundCookie = "__Secure-1PSIDRTS"
	// deviceBoundSampleLimit 为日志里最多列出的账户数
	deviceBoundSampleLimit = 5
)

// deviceBoundRenewSupported 为本平台能否用 Chrome OAuth 续签材料原地续签（authRenewPlatformSupported）；只给包内测试替换
var deviceBoundRenewSupported = authRenewPlatformSupported

// deviceBoundSession 判断认证状态是否带设备绑定会话的 Cookie（只看名字）
func deviceBoundSession(state aistudio.StorageState) bool {
	for _, cookie := range state.Cookies {
		if cookie.Name == deviceBoundCookie {
			return true
		}
	}
	return false
}

// chromeRenewalMaterial 判断认证状态的认证扩展里是否保存了 Chrome OAuth 续签材料（只看有没有，不读值）
func chromeRenewalMaterial(state aistudio.StorageState) bool {
	extension, _, err := state.AuthExtension()
	return err == nil && extension.OAuth != nil
}

// deviceBoundTally 统计一批认证文件里的设备绑定会话，可并发累加（认证文件更新在后台并发载入）
type deviceBoundTally struct {
	mu sync.Mutex
	// stale 为复制出来会失效（不能原地续签）的设备绑定会话数，sample 为其中最多 deviceBoundSampleLimit 个账户名；
	// unsupported 为其中带 Chrome 续签材料、但本平台不能原地续签的数量；renewable 为带续签材料、本平台能原地续签的数量（不计入 stale）
	stale       int
	sample      []string
	unsupported int
	renewable   int
}

// add 计入一个账户的认证状态：不是设备绑定会话时不计
func (tally *deviceBoundTally) add(label string, state aistudio.StorageState) {
	if tally == nil || !deviceBoundSession(state) {
		return
	}
	material := chromeRenewalMaterial(state)
	tally.mu.Lock()
	defer tally.mu.Unlock()
	if material && deviceBoundRenewSupported() {
		tally.renewable++
		return
	}
	if material {
		tally.unsupported++
	}
	tally.stale++
	if len(tally.sample) < deviceBoundSampleLimit {
		tally.sample = append(tally.sample, label)
	}
}

// log 写一行 WARN：复制出来会失效的设备绑定会话数与最多几个账户名，附带其中带续签材料却不能原地续签的数量、另有可原地续签的数量；
// 没有会失效的时不写。when 为日志开头的场合（“启动时”“导入时”“认证文件更新时”）
func (tally *deviceBoundTally) log(requests *requestRegistry, when string) {
	if tally == nil || requests == nil {
		return
	}
	tally.mu.Lock()
	stale, unsupported, renewable := tally.stale, tally.unsupported, tally.renewable
	names := strings.Join(tally.sample, ", ")
	tally.mu.Unlock()
	if stale == 0 {
		return
	}
	if stale > deviceBoundSampleLimit {
		names += fmt.Sprintf(" 等 %d 个", stale)
	}
	// Chrome 导入与原地续签只在 Windows 上可用（见 authRenewPlatformSupported），其他平台只提示浏览器登录
	hint := "可改用本服务的浏览器登录"
	if deviceBoundRenewSupported() {
		hint = "可改用本服务的浏览器登录或 Chrome 导入（带续签材料，可原地续签）"
	}
	detail := ""
	if unsupported > 0 {
		detail += fmt.Sprintf("；其中 %d 个带 Chrome 续签材料，但只有 Windows 能原地续签", unsupported)
	}
	if renewable > 0 {
		detail += fmt.Sprintf("；另有 %d 个带 Chrome 续签材料、可原地续签，不计入", renewable)
	}
	requests.log("auth", "WARN", fmt.Sprintf(
		"%s载入的认证文件中有 %d 个账户是设备绑定会话（带 %s，复制出来的认证文件约 2 小时后失效，%s%s） | 账户=%s",
		when, stale, deviceBoundCookie, hint, detail, names,
	))
}

// logDeviceBoundSessions 载入认证文件后写一行 WARN（见 deviceBoundTally.log）。只能用于还没加入账户池的账户
// （启动装配、目录扫描新载入的账户）：读认证状态时不加锁
func logDeviceBoundSessions(requests *requestRegistry, accounts []*aistudio.Account, when string) {
	tally := &deviceBoundTally{}
	for _, account := range accounts {
		if account == nil {
			continue
		}
		label := strings.TrimSpace(account.Config.Label)
		if label == "" {
			label = account.ID
		}
		tally.add(label, account.StorageState)
	}
	tally.log(requests, when)
}
