package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 启动失败记忆：启动 Worker 失败的账户原先只写一行日志，同一个排队的请求会反复挑同一个失败的号来启动（一个失败号 2 秒被启动 97 次，
// 排在后面的健康账户一次都轮不到），死号也一直显示就绪。现在每次启动失败都按类别处理（见 classifyStartupFailure）：
//   - 登录失效（auth）：按原有流程处理（无租约且没有续签材料时标为需要重新登录，带租约时恢复一次），标为需要重新登录的记入确定失效的
//     持久表；有续签材料、没有租约的账户状态不变，留给带租约的请求恢复，不记；
//   - 认证文件损坏（files）：storage state 的内容无法用于启动浏览器，按启动前的认证代际标为需要重新登录，标记成功的记入持久表；
//   - 指纹文件损坏（fingerprint）：账户的 Camoufox 指纹文件（camoufox-fingerprint.json）内容坏了（如写到一半时磁盘满或进程被杀）。
//     指纹不是登录凭据，登录可能仍然有效，重新上传认证文件、验证与浏览器登录都修不好它（都要先读这个文件），标为需要重新登录会把
//     登录正常的账户列为确定失效、只剩一键删除。所以不改变账户状态、不记入持久表、不列为确定失效，只在内存里按连续失败暂停调度
//     （2m、10m、30m 封顶，它与机器负载无关，不按系统性问题处理），日志与列表写明指纹文件路径，由管理员从备份恢复或删除该文件；
//   - 环境问题（environment）：浏览器没能启动起来（启动 Camoufox、连接 BiDi、取得 runtime 租约、读写文件的系统错误等），多半是机器资源
//     或部署问题，不是号的问题，只计入近 10 分钟的环境类失败数，不写账户记录；其中认证文件不存在的（整体替换账户目录的中途）
//     按文件缺失标为不可用，文件恢复后自动恢复（noteFilesMissing）；
//   - 页面受阻（page_blocked）：页面已经打开、输入框也在，但 bootstrap 提示词填不进去，或填好后官网 Run 按钮一直不可点（阶段 7）。
//     多半是账户本身的问题（例如官网弹出需要同意的条款或通知，挡住了输入框）。暂停与原因不确定相同；同一认证代际下连续
//     startupPageBlockedConfirmCount 次、每次失败时号池都健康（见 startup_page_blocked.go）时确认为失效：账户状态不变（仍是就绪，
//     问题消失时会自己恢复），之后每次失败都暂停 30 分钟，记入确定失效的持久表（kind=page_blocked），列为确定失效、可一键删除；
//   - 原因不确定（ambiguous）：页面未就绪、WAA 失败、超时。机器过载或代理慢时健康账号报的是同一串错误，只在内存里短暂暂停调度
//     （2m、10m、30m 封顶；近 5 分钟没有任何 Worker 启动成功时按系统性问题只暂停 1m、不计次数），永不改变账户状态，不能据此删号。
//
// 暂停只影响不固定账户的选号、预热与替补（startupBackoffAvailable）：固定账户、文件绑定与 Worker()（受保护传输的现场重建）不受影响。
// 启动成功、验证通过、清除记录时清零；认证代际变化（重新上传、续签）后记录自动失效
type startupFailureKind string

const (
	startupFailureNone        startupFailureKind = ""
	startupFailureAuth        startupFailureKind = "auth"
	startupFailureFiles       startupFailureKind = "files"
	startupFailureFingerprint startupFailureKind = "fingerprint"
	startupFailureEnvironment startupFailureKind = "environment"
	startupFailurePageBlocked startupFailureKind = "page_blocked"
	startupFailureAmbiguous   startupFailureKind = "ambiguous"
)

// label 返回日志与列表里的类别名称
func (kind startupFailureKind) label() string {
	switch kind {
	case startupFailureAuth:
		return "登录失效"
	case startupFailureFiles:
		return "认证文件损坏"
	case startupFailureFingerprint:
		return "指纹文件损坏"
	case startupFailureEnvironment:
		return "环境问题"
	case startupFailurePageBlocked:
		return "页面受阻"
	case startupFailureAmbiguous:
		return "原因不确定"
	}
	return "不计入"
}

const (
	// startupSystemicWindow 内没有任何 Worker 启动成功时，原因不确定的失败按系统性问题处理：只暂停 startupSystemicPause，不计次数
	startupSystemicWindow = 5 * time.Minute
	startupSystemicPause  = time.Minute
	// startupProbeInterval 为保底试探的全局间隔：候选都在暂停、又没有热 Worker 时，每个间隔最多提前放行一个最早到期的账户
	startupProbeInterval = time.Minute
	// startupEnvironmentWindow 为状态接口统计环境类启动失败的时间窗，startupEnvironmentLimit 为保留的时间点上限
	startupEnvironmentWindow = 10 * time.Minute
	startupEnvironmentLimit  = 4096
	// accountFingerprintFile 为账户目录里 Camoufox 指纹文件的文件名，与 camoufoxnative 读写账户指纹的文件相同（只用于日志与列表提示）
	accountFingerprintFile = "camoufox-fingerprint.json"
	// startupFailureMessageLimit 为保存与展示的错误原文的最大字符数
	startupFailureMessageLimit = 300
	// waitReasonStartupPaused 为候选账户都在启动失败暂停中时的排队原因
	waitReasonStartupPaused = "候选账户启动失败暂停中"
)

// startupAmbiguousPauses 为原因不确定（以及指纹文件损坏、页面受阻）的连续失败第 1、2、3 次起的暂停时长
var startupAmbiguousPauses = [...]time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// startupMaxPause 为启动失败暂停的上限，确认为页面受阻失效的账户之后每次失败都暂停这么久
var startupMaxPause = startupAmbiguousPauses[len(startupAmbiguousPauses)-1]

// startupPageBlockedTexts 为阶段 7 页面受阻的错误文本：Camoufox 填写 bootstrap 提示词失败（“填写 bootstrap 提示词失败 value=%q err=%v”）、
// 等待官网 Run 按钮启用超时（“等待官网 Run 按钮启用: %w”），以及纯 Go 后端填写 bootstrap 提示词失败（“填写 bootstrap 提示词失败: %w”）
var startupPageBlockedTexts = []string{"填写 bootstrap 提示词失败", "等待官网 Run 按钮启用"}

// startupBrowserBrokenTexts 为错误链里浏览器或运行时已经断开、退出的迹象：同时出现时页面受阻的文本只是连接断开的结果，
// 不是账户的问题，仍按原因不确定处理
var startupBrowserBrokenTexts = []string{
	"BiDi 连接已断开", "websocket: close", "use of closed network connection", "connection reset by peer", "broken pipe",
	"WAA runtime 已关闭",
}

// errStartupSlotsLate 表示后台启动取得冷启动名额时剩余期限已不够一次完整启动（INIT_TIMEOUT），或等名额一直等到期限：名额被
// 按需启动占满，后台启动等了太久。按容量已满返回（预热本轮不再启动新的，稍后重试），不记为预热失败、也不计入账户
var errStartupSlotsLate = fmt.Errorf("%w: 取得冷启动名额时剩余期限不足", errAccountWorkerCapacity)

// ambiguousStartupPause 返回原因不确定的第 count 次连续失败后的暂停时长
func ambiguousStartupPause(count int) time.Duration {
	if count < 1 {
		count = 1
	}
	return startupAmbiguousPauses[min(count, len(startupAmbiguousPauses))-1]
}

// classifyStartupFailure 按顺序取第一条匹配的规则给启动失败分类，stage 为失败时的启动阶段（1–7，runtime 租约错误为 0）：
//  1. 不计入：不是启动错误（*accountWorkerInitError），或属于容量已满、正在启动、被其他进程占用、清理未完成（ctx 已结束由调用方判断）；
//  2. 登录失效：aistudio.DefinitiveAuthenticationFailure；
//  3. 指纹文件损坏：阶段 ≤2 且错误文本含 Camoufox 指纹（解析失败、内容为空）；
//  4. 认证文件损坏：阶段 ≤2 且错误文本含 storage state（3、4 两条里读写文件的系统错误都不算，如文件不存在、打开文件过多、磁盘满，按环境问题）；
//  5. 环境问题：阶段 ≤4，或取得 runtime 租约失败；
//  6. 页面受阻：阶段 7，错误文本含 startupPageBlockedTexts，且错误链里没有浏览器断开、退出的迹象（startupBrowserBrokenTexts），
//     整个启动的期限（INIT_TIMEOUT）也没有到（期限到了说明页面加载得太慢，提示词与 Run 按钮只是没来得及）；
//  7. 其余（阶段 ≥5 的页面未就绪、WAA 失败、超时）都是原因不确定
func classifyStartupFailure(err error, stage int) startupFailureKind {
	var initErr *accountWorkerInitError
	if err == nil || !errors.As(err, &initErr) {
		return startupFailureNone
	}
	if errors.Is(err, errAccountWorkerCapacity) || errors.Is(err, errAccountWorkerOpening) ||
		errors.Is(err, errAccountRuntimeBusy) || errors.Is(err, errAccountWorkerCleanupPending) {
		return startupFailureNone
	}
	if aistudio.DefinitiveAuthenticationFailure(err) {
		return startupFailureAuth
	}
	if text := err.Error(); stage <= 2 && !fileSystemError(err) {
		// 指纹先于 storage state 判断：指纹文件坏了不说明登录失效，不能按认证文件损坏标为需要重新登录
		if strings.Contains(text, "Camoufox 指纹") {
			return startupFailureFingerprint
		}
		if strings.Contains(text, "storage state") {
			return startupFailureFiles
		}
	}
	if stage <= 4 || initErr.leaseFailed {
		return startupFailureEnvironment
	}
	if text := err.Error(); stage == 7 && !initErr.initExpired && containsAny(text, startupPageBlockedTexts) &&
		!containsAny(text, startupBrowserBrokenTexts) {
		return startupFailurePageBlocked
	}
	return startupFailureAmbiguous
}

// containsAny 判断 text 是否包含 parts 中的任意一个
func containsAny(text string, parts []string) bool {
	for _, part := range parts {
		if strings.Contains(text, part) {
			return true
		}
	}
	return false
}

// fileSystemError 判断错误是否来自读写文件的系统错误（文件不存在、打开文件过多、权限、磁盘等）：这些不说明认证文件的内容坏了。
// 文件不存在多半是整体替换账户目录的中途（账户目录监视等约 1 分钟才移出账户，文件复制回来后登录状态不变），按需要重新登录处理
// 会把健康账户列为确定失效、能被一键删除，由 noteFilesMissing 按文件缺失标为不可用、文件恢复后自动恢复
func fileSystemError(err error) bool {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	return errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.Is(err, fs.ErrNotExist)
}

// noteFilesMissing 处理读取认证文件或指纹时文件不存在的启动失败（阶段 ≤2）：账户的配置或 storage state 确实不存在时，
// 与取得租约时发现文件缺失一样标为不可用（aistudio.AccountPool.MarkFilesMissing），退出调度，文件恢复后由账户目录监视重新同步恢复，
// 目录一直不在时按原有规则移出账户池。文件又已存在（复制完成）时什么都不做，下次启动照常进行。不改登录状态、不写失效账户记录
func (manager *accountWorkerManager) noteFilesMissing(accountID string, stage int, err error) {
	if stage > 2 || !errors.Is(err, fs.ErrNotExist) || !manager.pool.MarkFilesMissing(accountID) {
		return
	}
	if manager.refresher != nil && manager.refresher.publish != nil {
		manager.refresher.publish()
	}
	label := accountID
	manager.mu.RLock()
	if account := manager.accounts[accountID]; account != nil {
		label = account.label
	}
	manager.mu.RUnlock()
	manager.requests.log(label, "WARN", fmt.Sprintf(
		"WAA Worker 启动失败 | 账户文件缺失，标为不可用（文件恢复后自动恢复，不计入失效账户） | 阶段=%s | 错误=%s",
		startupStepText(stage), startupFailureMessage(err),
	))
}

// startupStageOf 返回启动错误记下的启动阶段，不是启动错误时为 0
func startupStageOf(err error) int {
	var initErr *accountWorkerInitError
	if errors.As(err, &initErr) {
		return initErr.stage
	}
	return 0
}

// startupFailureMessage 返回截断后的错误原文
func startupFailureMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	if runes := []rune(message); len(runes) > startupFailureMessageLimit {
		message = string(runes[:startupFailureMessageLimit]) + "…"
	}
	return message
}

// startupFailureURL 返回错误文本里的页面地址（登录页跳转优先），只用于展示，没有时为空
func startupFailureURL(err error) string {
	if redirect := aistudio.NativeLoginRedirectURL(err); redirect != "" {
		return truncateRunes(redirect, startupFailureMessageLimit)
	}
	text := err.Error()
	index := strings.Index(text, "url=")
	if index < 0 {
		return ""
	}
	raw := text[index+len("url="):]
	if end := strings.IndexAny(raw, " \t\r\n"); end >= 0 {
		raw = raw[:end]
	}
	return truncateRunes(strings.TrimSuffix(raw, ":"), startupFailureMessageLimit)
}

func truncateRunes(text string, limit int) string {
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

// accountFingerprintPath 返回账户 Camoufox 指纹文件的路径（与 storage state 在同一目录），没有认证文件路径时为空
func accountFingerprintPath(storageStatePath string) string {
	if strings.TrimSpace(storageStatePath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(storageStatePath), accountFingerprintFile)
}

// startupStepText 返回日志里的阶段字段（阶段=n/7）
func startupStepText(stage int) string {
	return fmt.Sprintf("%d/7", stage)
}

// noteStartupFailure 在 startAuthenticatedWorker 返回启动失败前调用一次，覆盖预热、后台扩容、替补、请求现场启动与 Worker()。
// generation 为启动前读取的认证代际（带租约恢复认证后为恢复后的代际），startedAt 为开始时间：记录前代际已变化的（启动排队或启动途中
// 替换了认证文件、续签提交了新认证）不记，旧凭据的失败不能记到新凭据头上。返回原错误不变，这里只做记录
func (manager *accountWorkerManager) noteStartupFailure(
	ctx context.Context, accountID string, generation uint64, startedAt time.Time, err error,
) {
	if err == nil || ctx.Err() != nil {
		return
	}
	stage := startupStageOf(err)
	kind := classifyStartupFailure(err, stage)
	if kind == startupFailureNone || manager.pool.AuthGeneration(accountID) != generation {
		return
	}
	if kind == startupFailureEnvironment {
		manager.noteEnvironmentFailure(time.Now())
		manager.noteFilesMissing(accountID, stage, err)
		return
	}
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		return
	}
	now := time.Now()
	switch kind {
	case startupFailureAuth, startupFailureFiles:
		if kind == startupFailureFiles {
			reason := "认证文件无法用于启动浏览器：" + startupFailureMessage(err)
			marked, markErr := manager.pool.MarkAuthenticationRequiredIfGeneration(accountID, generation, startedAt, reason)
			if markErr != nil {
				manager.requests.log(account.label, "WARN", "WAA Worker 标记需要重新登录失败 | 错误="+strings.TrimSpace(markErr.Error()))
			}
			if marked && manager.refresher != nil && manager.refresher.publish != nil {
				manager.refresher.publish()
			}
		}
		// 登录失效与认证文件损坏都不设暂停：已标为需要重新登录的账户由账户状态退出调度。只有账户此刻确实处于需要重新登录状态时
		// 才记入确定失效的持久表：有续签材料、没有租约的登录失效保持原来的行为，留给带租约的请求现场启动恢复（已知限制：这期间它仍可能
		// 被后台扩容反复选中）；启动期间验证通过或认证已更新、标记被认证代际与检查时间拒绝的也不记。这些账户仍可调度、可能恢复，
		// 记进表里会被列为确定失效、能被一键删除
		if states := manager.pool.States([]string{accountID}); states[accountID] != aistudio.AccountAuthRequired {
			manager.requests.log(account.label, "WARN", fmt.Sprintf(
				"WAA Worker 启动失败计入账户 | 类别=%s | 阶段=%s | 暂停调度=否 | 账户状态不变，不计入失效账户（有续签材料时留给带租约的请求恢复；启动期间验证通过或认证已更新时不标记）",
				kind.label(), startupStepText(stage),
			))
			return
		}
		identity, _ := manager.pool.LoginIdentity(accountID)
		count := manager.startupFailures.record(accountID, kind, startupFailureURL(err), startupFailureMessage(err), stage, identity, now)
		manager.requests.log(account.label, "WARN", fmt.Sprintf(
			"WAA Worker 启动失败计入账户 | 类别=%s | 阶段=%s | 连续失败=%d | 暂停调度=否 | 已标为需要重新登录",
			kind.label(), startupStepText(stage), max(count, 1),
		))
	case startupFailureAmbiguous, startupFailureFingerprint, startupFailurePageBlocked:
		manager.notePausedFailure(account, kind, generation, stage, err, now)
	}
}

// notePausedFailure 记下一次原因不确定、指纹文件损坏或页面受阻的启动失败并暂停调度，不改变账户状态。写入顺序同 markRuntimeBusy：
// 先写原因与代际再写期限，读到期限未到的一方总能拿到原因与代际。原因不确定与页面受阻的失败按系统性问题处理：指纹文件损坏与机器负载无关，
// 每次都计次数。页面受阻另按号池是否健康累计失效确认的连续次数，达到 startupPageBlockedConfirmCount 时记入确定失效的持久表
// （见 startup_page_blocked.go），已确认的之后每次失败都暂停 startupMaxPause（按系统性问题处理时也是，只是不计次数），
// 也不再计入页面受阻的爆发统计
func (manager *accountWorkerManager) notePausedFailure(
	account *accountWorker, kind startupFailureKind, generation uint64, stage int, err error, now time.Time,
) {
	systemic := (kind == startupFailureAmbiguous || kind == startupFailurePageBlocked) &&
		now.Sub(time.Unix(0, manager.lastStartupSuccess.Load())) >= startupSystemicWindow
	var verdict pageBlockedVerdict
	identity, confirmedBefore := "", false
	if kind == startupFailurePageBlocked {
		identity, _ = manager.pool.LoginIdentity(account.id)
		confirmedBefore = manager.startupFailures.pageBlocked(account.id, identity)
		verdict = manager.startupHealth.notePageBlocked(account.id, !confirmedBefore, now)
		if verdict.warnBurst {
			manager.requests.log("service", "WARN", fmt.Sprintf(
				"大量账户页面受阻，可能是官网页面改版，暂不确认为失效 | 账户数=%d | 启动尝试账户数=%d",
				verdict.blockedAccounts, verdict.attemptedAccounts,
			))
		}
	}
	account.mu.Lock()
	if account.startFailGen.Load() != generation || account.startFailKind != kind {
		// 上一段连续失败属于旧认证或另一类原因，从头计
		account.startFailCount, account.startFailTotal, account.startFailEvidence = 0, 0, 0
	}
	if account.startFailTotal == 0 {
		account.startFailFirst = now
	}
	account.startFailTotal++
	delay := startupSystemicPause
	if !systemic {
		account.startFailCount++
		delay = ambiguousStartupPause(account.startFailCount)
		if kind == startupFailurePageBlocked {
			// 号池不健康的那次失败只计入暂停，失效确认的连续次数不增加、也不清零
			if verdict.healthy() {
				account.startFailEvidence++
			}
			if confirmedBefore || account.startFailEvidence >= startupPageBlockedConfirmCount {
				delay = startupMaxPause
			}
		}
	} else if confirmedBefore {
		// 已确认的账户按系统性问题处理时同样不计次数，但仍暂停 startupMaxPause（确认后每 30 分钟重试一次）：服务刚启动还没有任何
		// Worker 启动成功、或整个号池都启动不了时，不每分钟重新启动它一次、占用冷启动名额
		delay = startupMaxPause
	}
	account.startFailKind = kind
	account.startFailStage = stage
	account.startFailLast = now
	stored := err
	account.startFailErr.Store(&stored)
	account.startFailGen.Store(generation)
	account.startFailUntil.Store(now.Add(delay).UnixNano())
	count := account.startFailCount
	evidence := account.startFailEvidence
	firstAt := account.startFailFirst
	label := account.label
	fingerprintPath := accountFingerprintPath(account.config.StorageStatePath)
	account.mu.Unlock()
	confirmed, created, recordCount := confirmedBefore, false, 0
	if kind == startupFailurePageBlocked && !systemic && verdict.healthy() &&
		(confirmedBefore || evidence >= startupPageBlockedConfirmCount) &&
		manager.pool.AuthGeneration(account.id) == generation {
		// 记录前再核对一次认证代际：途中替换了认证文件的，旧凭据的失败不记到新凭据头上
		recordCount, created = manager.startupFailures.recordPageBlocked(
			account.id, evidence, startupFailureURL(err), startupFailureMessage(err), stage, identity, firstAt, now,
		)
		confirmed = confirmed || created
	}
	note := ""
	switch {
	case kind == startupFailurePageBlocked && confirmed:
		// 在系统性问题之前判断：已确认的账户按系统性问题处理时同样暂停 30 分钟（只是不计次数、不更新记录）
		note = " | 已确认为失效账户（账户状态不变，每 30 分钟重试一次）"
	case systemic:
		note = " | 近 5 分钟没有任何 Worker 启动成功，按系统性问题处理，不计次数"
	case kind == startupFailureFingerprint:
		note = fmt.Sprintf(
			" | 指纹文件=%s | 与登录无关，账户状态不变、不计入失效账户（重新上传认证文件、验证与浏览器登录都修不好）："+
				"从备份恢复该文件，或删除它（下次启动按当前版本重新生成指纹）后清除暂停",
			fingerprintPath,
		)
	case kind == startupFailurePageBlocked && verdict.burst:
		note = fmt.Sprintf(" | 大量账户页面受阻（可能是官网页面改版），不计入失效确认 | 失效确认=%d/%d",
			evidence, startupPageBlockedConfirmCount)
	case kind == startupFailurePageBlocked && !verdict.otherSuccess:
		note = fmt.Sprintf(" | 近 10 分钟没有其他账户启动成功，不计入失效确认 | 失效确认=%d/%d",
			evidence, startupPageBlockedConfirmCount)
	case kind == startupFailurePageBlocked:
		note = fmt.Sprintf(" | 失效确认=%d/%d", evidence, startupPageBlockedConfirmCount)
	}
	manager.requests.log(label, "WARN", fmt.Sprintf(
		"WAA Worker 启动失败计入账户 | 类别=%s | 阶段=%s | 连续失败=%d | 暂停调度=%s%s",
		kind.label(), startupStepText(stage), count, delay, note,
	))
	if created {
		manager.requests.log(label, "WARN", fmt.Sprintf(
			"WAA Worker 页面受阻已确认为失效账户 | 连续失败=%d | 最近错误=%s | 可在账户页删除，或到官网登录检查是否有需要处理的提示",
			recordCount, startupFailureMessage(err),
		))
	}
	time.AfterFunc(delay, manager.notifyScheduling)
}

// clearStartupFailureLocked 清除账户的启动失败记忆；调用方持有 account.mu
func clearStartupFailureLocked(account *accountWorker) bool {
	had := account.startFailTotal > 0 || account.startFailUntil.Load() != 0
	account.startFailUntil.Store(0)
	account.startFailErr.Store(nil)
	account.startFailGen.Store(0)
	account.startFailKind = startupFailureNone
	account.startFailCount, account.startFailTotal, account.startFailEvidence, account.startFailStage = 0, 0, 0, 0
	account.startFailFirst, account.startFailLast = time.Time{}, time.Time{}
	return had
}

// clearStartupFailures 清除指定账户（all 为真时为全部账户）的启动失败暂停，返回清除的账户；有清除时唤醒调度。
// 账户 ID 先精确匹配，匹配不到的再按 strings.EqualFold 比较（同 permissionDeniedRegistry.clear）
func (manager *accountWorkerManager) clearStartupFailures(accountIDs []string, all bool) []string {
	manager.mu.RLock()
	accounts := make([]*accountWorker, 0, len(accountIDs))
	if all {
		for _, account := range manager.accounts {
			accounts = append(accounts, account)
		}
	} else {
		unmatched := make([]string, 0)
		for _, accountID := range accountIDs {
			if account := manager.accounts[accountID]; account != nil {
				accounts = append(accounts, account)
			} else {
				unmatched = append(unmatched, accountID)
			}
		}
		if len(unmatched) > 0 {
			for accountID, account := range manager.accounts {
				for _, requested := range unmatched {
					if strings.EqualFold(requested, accountID) {
						accounts = append(accounts, account)
						break
					}
				}
			}
		}
	}
	manager.mu.RUnlock()
	cleared := make([]string, 0)
	for _, account := range accounts {
		account.mu.Lock()
		if clearStartupFailureLocked(account) {
			cleared = append(cleared, account.id)
		}
		account.mu.Unlock()
	}
	if len(cleared) > 0 {
		manager.notifyScheduling()
	}
	return cleared
}

// startupBackoffAvailable 过滤仍在启动失败暂停期内的账户，保持原有顺序。写法同 runtimeAvailable：只读原子值、不取账户锁；
// 只对暂停中的账户（在 manager.mu 之外，一次取号池锁：选号每次重新检查都要调用两次，暂停的账户可能上千）核对认证代际，
// 代际已变化的记录视为无。
// allowProbe 为真时（调用方没有热候选）过滤后为空、原列表非空的，全局每 startupProbeInterval 最多放行一个最早到期的账户（保底试探）
func (manager *accountWorkerManager) startupBackoffAvailable(accountIDs []string, allowProbe bool) []string {
	if len(accountIDs) == 0 {
		return accountIDs
	}
	type pausedAccount struct {
		id         string
		until      int64
		generation uint64
	}
	nowNanos := time.Now().UnixNano()
	var paused []pausedAccount
	manager.mu.RLock()
	for _, accountID := range accountIDs {
		account := manager.accounts[accountID]
		if account == nil {
			continue
		}
		if until := account.startFailUntil.Load(); until != 0 && nowNanos < until {
			paused = append(paused, pausedAccount{id: accountID, until: until, generation: account.startFailGen.Load()})
		}
	}
	manager.mu.RUnlock()
	if len(paused) == 0 {
		return accountIDs
	}
	pausedIDs := make([]string, 0, len(paused))
	for _, entry := range paused {
		pausedIDs = append(pausedIDs, entry.id)
	}
	generations := manager.pool.AuthGenerations(pausedIDs)
	still := make(map[string]int64, len(paused))
	for _, entry := range paused {
		if generation, exists := generations[entry.id]; exists && generation == entry.generation {
			still[entry.id] = entry.until
		}
	}
	if len(still) == 0 {
		return accountIDs
	}
	available := make([]string, 0, len(accountIDs)-len(still)+1)
	for _, accountID := range accountIDs {
		if _, pausedNow := still[accountID]; !pausedNow {
			available = append(available, accountID)
		}
	}
	if len(available) == 0 && allowProbe {
		if accountID := manager.startupProbe(still, nowNanos); accountID != "" {
			available = append(available, accountID)
		}
	}
	return available
}

// startupProbe 在距上次保底试探满 startupProbeInterval 时（CAS 更新 lastProbe，并发的选号只有一个成功）返回暂停中最早到期的账户
func (manager *accountWorkerManager) startupProbe(paused map[string]int64, nowNanos int64) string {
	last := manager.lastProbe.Load()
	if last != 0 && nowNanos-last < int64(startupProbeInterval) {
		return ""
	}
	chosen := ""
	var earliest int64
	for accountID, until := range paused {
		if chosen == "" || until < earliest || until == earliest && accountID < chosen {
			chosen, earliest = accountID, until
		}
	}
	if chosen == "" || !manager.lastProbe.CompareAndSwap(last, nowNanos) {
		return ""
	}
	manager.requests.log(chosen, "INFO", fmt.Sprintf(
		"WAA Worker 保底试探 | 候选都在启动失败暂停中，提前放行最早到期的账户 | 原暂停至=%s",
		time.Unix(0, earliest).Format(time.RFC3339),
	))
	return chosen
}

// noteEnvironmentFailure 计入一次环境类启动失败（只计数，不写账户记录）
func (manager *accountWorkerManager) noteEnvironmentFailure(now time.Time) {
	manager.envFailMu.Lock()
	defer manager.envFailMu.Unlock()
	manager.envFailures = append(pruneStartupTimes(manager.envFailures, now), now)
	if extra := len(manager.envFailures) - startupEnvironmentLimit; extra > 0 {
		manager.envFailures = append(manager.envFailures[:0], manager.envFailures[extra:]...)
	}
}

// environmentFailures 返回近 startupEnvironmentWindow 的环境类启动失败数
func (manager *accountWorkerManager) environmentFailures(now time.Time) int {
	manager.envFailMu.Lock()
	defer manager.envFailMu.Unlock()
	manager.envFailures = pruneStartupTimes(manager.envFailures, now)
	return len(manager.envFailures)
}

// pruneStartupTimes 去掉早于时间窗的时间点（按时间先后排列）
func pruneStartupTimes(times []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-startupEnvironmentWindow)
	index := sort.Search(len(times), func(position int) bool { return times[position].After(cutoff) })
	if index == 0 {
		return times
	}
	return append(times[:0], times[index:]...)
}

// startupPause 为一个启动失败暂停中的账户（列表与状态接口用）
type startupPause struct {
	accountID string
	label     string
	kind      startupFailureKind
	until     time.Time
	total     int
	stage     int
	firstAt   time.Time
	lastAt    time.Time
	err       error
	// fingerprintPath 为账户 Camoufox 指纹文件的路径（指纹文件损坏时列表提示用）
	fingerprintPath string
	// evidence 为页面受阻时计入失效确认的连续次数
	evidence int
}

// startupPauses 返回启动失败暂停中的账户（认证代际已变化的不算），按暂停到期时间从早到晚排列
func (manager *accountWorkerManager) startupPauses(now time.Time) []startupPause {
	nowNanos := now.UnixNano()
	manager.mu.RLock()
	accounts := make([]*accountWorker, 0)
	for _, account := range manager.accounts {
		if until := account.startFailUntil.Load(); until != 0 && nowNanos < until {
			accounts = append(accounts, account)
		}
	}
	manager.mu.RUnlock()
	accountIDs := make([]string, 0, len(accounts))
	for _, account := range accounts {
		accountIDs = append(accountIDs, account.id)
	}
	generations := manager.pool.AuthGenerations(accountIDs)
	pauses := make([]startupPause, 0, len(accounts))
	for _, account := range accounts {
		account.mu.Lock()
		until := account.startFailUntil.Load()
		pause := startupPause{
			accountID: account.id, label: account.label, kind: account.startFailKind, until: time.Unix(0, until),
			total: account.startFailTotal, stage: account.startFailStage, firstAt: account.startFailFirst, lastAt: account.startFailLast,
			fingerprintPath: accountFingerprintPath(account.config.StorageStatePath), evidence: account.startFailEvidence,
		}
		if stored := account.startFailErr.Load(); stored != nil {
			pause.err = *stored
		}
		generation := account.startFailGen.Load()
		account.mu.Unlock()
		// 代际在读取账户之前一次取得：之后代际只会增加，读到的记录代际与它不同的（含读取之后才记下的新失败）本次不列，下次读取时列出
		if current, exists := generations[account.id]; until == 0 || nowNanos >= until || !exists || current != generation {
			continue
		}
		pauses = append(pauses, pause)
	}
	sort.Slice(pauses, func(left int, right int) bool {
		if !pauses[left].until.Equal(pauses[right].until) {
			return pauses[left].until.Before(pauses[right].until)
		}
		return pauses[left].accountID < pauses[right].accountID
	})
	return pauses
}

// pausedStartupCount 返回启动失败暂停中的账户数（认证代际已变化的不算）。状态接口与预热等待原因用它：只读原子值、不取账户锁
// （取账户锁会让 occupiedWorkersIn 把正被读取的账户误算成占用的浏览器槽位），只对暂停中的账户一次取号池锁核对认证代际
func (manager *accountWorkerManager) pausedStartupCount(now time.Time) int {
	type pausedAccount struct {
		id         string
		generation uint64
	}
	nowNanos := now.UnixNano()
	manager.mu.RLock()
	paused := make([]pausedAccount, 0)
	for accountID, account := range manager.accounts {
		if until := account.startFailUntil.Load(); until != 0 && nowNanos < until {
			paused = append(paused, pausedAccount{id: accountID, generation: account.startFailGen.Load()})
		}
	}
	manager.mu.RUnlock()
	if len(paused) == 0 {
		return 0
	}
	pausedIDs := make([]string, 0, len(paused))
	for _, entry := range paused {
		pausedIDs = append(pausedIDs, entry.id)
	}
	generations := manager.pool.AuthGenerations(pausedIDs)
	count := 0
	for _, entry := range paused {
		if generation, exists := generations[entry.id]; exists && generation == entry.generation {
			count++
		}
	}
	return count
}

// startupFailureTime 把时间格式化为 RFC3339，零值为空
func startupFailureTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// startupStatus 返回状态接口的 startup 字段：authRequired 为当前处于需要重新登录状态的账户数。确定失效的账户数与失效账户列表的
// confirmed 一致：需要重新登录的账户，加上持久表里确认为页面受阻、账户还在池里且不是需要重新登录状态的账户（见 StartupFailures）。
// PageBlockedBurst 为此刻处于页面受阻的大面积爆发（见 startup_page_blocked.go）
func (manager *accountWorkerManager) startupStatus(authRequired int) api.AdminStartupStatus {
	now := time.Now()
	launching, queued, deferred := manager.startupSlots.usage()
	status := api.AdminStartupStatus{
		Launching: launching, Limit: max(manager.warmConcurrencyValue(), 1) + 1,
		QueuedOnDemand: queued, QueuedBackground: deferred,
		Paused:           manager.pausedStartupCount(now),
		Dead:             authRequired + manager.pageBlockedDead(),
		EnvFailures10m:   manager.environmentFailures(now),
		PageBlockedBurst: manager.startupHealth.burst(now),
	}
	if last := manager.lastStartupSuccess.Load(); last != 0 {
		status.LastSuccess = startupFailureTime(time.Unix(0, last))
	}
	return status
}

// pageBlockedDead 返回持久表里确认为页面受阻、账户还在池里且不是需要重新登录状态的账户数（需要重新登录的已计入 authRequired）
func (manager *accountWorkerManager) pageBlockedDead() int {
	accountIDs := manager.startupFailures.pageBlockedIDs()
	if len(accountIDs) == 0 {
		return 0
	}
	count := 0
	for _, state := range manager.pool.States(accountIDs) {
		if state != aistudio.AccountAuthRequired {
			count++
		}
	}
	return count
}

// startupFailureAdmin 为管理接口 GET /api/accounts/startup-failures 与 POST /api/accounts/startup-failures/clear 需要的能力；
// 管理器把请求交给当前生成服务的管理端
type startupFailureAdmin interface {
	StartupFailures(context.Context) ([]api.AdminStartupFailure, error)
	ClearStartupFailures(context.Context, api.StartupFailureClearInput) (int, error)
}

var (
	_ startupFailureAdmin = (*runtimeManager)(nil)
	_ startupFailureAdmin = (*runtimeAdmin)(nil)
)

// StartupFailures 返回启动失败的账户。确定失效（confirmed，可一键删除）：先列有持久记录的（登录失效与认证文件损坏只列当前处于需要重新登录
// 状态的账户，页面受阻列账户还在池里的，带原因、阶段与次数，按最近一次从新到旧；页面受阻的账户状态不变，带暂停到期时间），再列其余需要重新登录
// 的账户。未确认的只读：内存里原因不确定、指纹文件损坏或尚未确认的页面受阻的暂停（按暂停到期从早到晚），以及持久表里登录失效与认证文件损坏
// 的账户当前不是需要重新登录状态的记录（重启后重试一次、验证通过或续签之前留下的，账户仍可调度、可能恢复，不能据此删号；下次启动再失败、
// 标为需要重新登录时才列为确定失效）。同一账户只列一次
func (admin *runtimeAdmin) StartupFailures(context.Context) ([]api.AdminStartupFailure, error) {
	workers := admin.workers
	records := workers.startupFailures.list()
	pauses := workers.startupPauses(time.Now())
	pauseUntil := make(map[string]time.Time, len(pauses))
	for _, pause := range pauses {
		pauseUntil[pause.accountID] = pause.until
	}
	listed := make(map[string]struct{}, len(records))
	recordIDs := make([]string, 0, len(records))
	for _, entry := range records {
		recordIDs = append(recordIDs, entry.accountID)
	}
	labels := admin.pool.Labels(recordIDs)
	states := admin.pool.States(recordIDs)
	result := make([]api.AdminStartupFailure, 0, len(records))
	pending := make([]api.AdminStartupFailure, 0)
	for _, entry := range records {
		label := entry.accountID
		if value := labels[entry.accountID]; value != "" {
			label = value
		}
		kind := startupFailureKind(entry.record.Kind)
		state, exists := states[entry.accountID]
		confirmed := state == aistudio.AccountAuthRequired
		reason := "认证文件无法用于启动浏览器"
		switch kind {
		case startupFailureAuth:
			reason = "启动 Worker 时确认登录失效"
			if entry.record.URL != "" {
				reason = "启动 Worker 时被跳转到 Google 登录页"
			}
		case startupFailurePageBlocked:
			// 页面受阻不改变账户状态：账户还在池里就是确定失效
			confirmed = exists
			reason = "页面打开后填不进提示词或 Run 按钮一直不可用，多半是账户在官网有需要处理的提示（例如未同意条款）；" +
				"账户状态不变，每 30 分钟重试一次"
		}
		item := api.AdminStartupFailure{
			ID: entry.accountID, Label: label, Kind: string(kind), Confirmed: confirmed,
			Reason: reason, Message: entry.record.Message, Stage: entry.record.Stage, URL: entry.record.URL, Count: entry.record.Count,
			FirstAt: startupFailureTime(entry.record.FirstAt), LastAt: startupFailureTime(entry.record.LastAt),
		}
		if until, paused := pauseUntil[entry.accountID]; paused && kind == startupFailurePageBlocked {
			item.Until = startupFailureTime(until)
		}
		if !item.Confirmed {
			if kind == startupFailurePageBlocked {
				item.Reason += "（账户已不在账户池中）"
			} else {
				item.Reason += "（账户当前不是需要重新登录状态，下次启动再失败才确认）"
			}
			pending = append(pending, item)
			continue
		}
		listed[entry.accountID] = struct{}{}
		result = append(result, item)
	}
	authRequired := make([]api.AdminStartupFailure, 0)
	for _, status := range admin.pool.Status() {
		if status.State != aistudio.AccountAuthRequired {
			continue
		}
		if _, exists := listed[status.ID]; exists {
			continue
		}
		listed[status.ID] = struct{}{}
		authRequired = append(authRequired, api.AdminStartupFailure{
			ID: status.ID, Label: status.Label, Kind: string(startupFailureAuth), Confirmed: true,
			Reason: "账户需要重新登录", Message: truncateRunes(strings.TrimSpace(status.Message), startupFailureMessageLimit),
		})
	}
	sort.Slice(authRequired, func(left int, right int) bool { return authRequired[left].ID < authRequired[right].ID })
	result = append(result, authRequired...)
	// 暂停先于未确认的持久记录列出：同一账户两者都有时暂停是当前的情况（账户表 Worker 列按它显示暂停到期时间）
	for _, pause := range pauses {
		if _, exists := listed[pause.accountID]; exists {
			continue
		}
		listed[pause.accountID] = struct{}{}
		kind, reason := startupFailureAmbiguous, "启动反复失败，原因不确定（机器负载高或网络慢时健康账户也会这样）"
		switch pause.kind {
		case startupFailureFingerprint:
			kind = startupFailureFingerprint
			reason = fmt.Sprintf(
				"账户 Camoufox 指纹文件损坏，与登录无关（重新上传认证文件、验证与浏览器登录都修不好）：从备份恢复 %s，"+
					"或删除它（下次启动按当前版本重新生成指纹）后清除暂停",
				pause.fingerprintPath,
			)
		case startupFailurePageBlocked:
			kind = startupFailurePageBlocked
			reason = fmt.Sprintf(
				"页面打开后填不进提示词或 Run 按钮一直不可用，尚未确认为失效（号池健康时连续 %d 次才确认，当前 %d/%d）",
				startupPageBlockedConfirmCount, pause.evidence, startupPageBlockedConfirmCount,
			)
		}
		entry := api.AdminStartupFailure{
			ID: pause.accountID, Label: pause.label, Kind: string(kind), Confirmed: false,
			Reason: reason, Stage: pause.stage, Count: pause.total,
			FirstAt: startupFailureTime(pause.firstAt), LastAt: startupFailureTime(pause.lastAt), Until: startupFailureTime(pause.until),
		}
		if pause.err != nil {
			entry.Message = startupFailureMessage(pause.err)
			entry.URL = startupFailureURL(pause.err)
		}
		if entry.Label == "" {
			entry.Label = entry.ID
		}
		result = append(result, entry)
	}
	for _, item := range pending {
		if _, exists := listed[item.ID]; !exists {
			result = append(result, item)
		}
	}
	return result, nil
}

// ClearStartupFailures 按管理页的操作清除启动失败记录：持久表里的条目与内存里的暂停一起清除，并唤醒调度；不改变账户状态
// （需要重新登录的账户仍需重新上传认证文件或验证）。返回清除的账户数；account_ids 为空且 all 为假时返回 400
func (admin *runtimeAdmin) ClearStartupFailures(_ context.Context, input api.StartupFailureClearInput) (int, error) {
	accountIDs := make([]string, 0, len(input.AccountIDs))
	for _, accountID := range input.AccountIDs {
		if accountID = strings.TrimSpace(accountID); accountID != "" {
			accountIDs = append(accountIDs, accountID)
		}
	}
	if !input.All && len(accountIDs) == 0 {
		return 0, &adminOperationError{status: http.StatusBadRequest, code: "invalid_request", message: "需要 account_ids 或 all"}
	}
	cleared := make(map[string]struct{})
	for _, accountID := range admin.workers.startupFailures.clear(accountIDs, input.All) {
		cleared[accountID] = struct{}{}
	}
	for _, accountID := range admin.workers.clearStartupFailures(accountIDs, input.All) {
		cleared[accountID] = struct{}{}
	}
	if len(cleared) > 0 {
		admin.requests.log("auth", "INFO", fmt.Sprintf("已清除启动失败记录 | 账户=%d", len(cleared)))
	}
	return len(cleared), nil
}

// StartupFailures 返回当前生成服务的启动失败账户
func (manager *runtimeManager) StartupFailures(ctx context.Context) ([]api.AdminStartupFailure, error) {
	admin, ok := manager.generation().admin.(startupFailureAdmin)
	if !ok {
		return nil, fmt.Errorf("启动失败列表尚未就绪")
	}
	return admin.StartupFailures(ctx)
}

// ClearStartupFailures 清除当前生成服务的启动失败记录
func (manager *runtimeManager) ClearStartupFailures(ctx context.Context, input api.StartupFailureClearInput) (int, error) {
	admin, ok := manager.generation().admin.(startupFailureAdmin)
	if !ok {
		return 0, fmt.Errorf("启动失败列表尚未就绪")
	}
	return admin.ClearStartupFailures(ctx, input)
}

// clearStartupFailure 清除一个账户的启动失败暂停（账户 ID 精确匹配），返回是否有记录；有清除时唤醒调度
func (manager *accountWorkerManager) clearStartupFailure(accountID string) bool {
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		return false
	}
	account.mu.Lock()
	had := clearStartupFailureLocked(account)
	account.mu.Unlock()
	if had {
		manager.notifyScheduling()
	}
	return had
}

// forgetStartupFailure 清除账户的启动失败记录（持久表与内存暂停），有清除时写日志并唤醒调度。认证文件更新、验证通过时调用；
// 账户 ID 精确匹配
func (service *trackedService) forgetStartupFailure(accountID string, source string) {
	if service == nil || service.workers == nil {
		return
	}
	recorded := service.workers.startupFailures.remove(accountID)
	paused := service.workers.clearStartupFailure(accountID)
	if (recorded || paused) && service.requests != nil {
		service.requests.log(accountID, "INFO", "清除启动失败记录 | 来源="+source)
	}
}

// authFilesUpdated 账户的认证文件更新（新增账户、浏览器登录、重新导入、外部更新）后清除 403 无权限状态与启动失败记录
func (service *trackedService) authFilesUpdated(accountID string, source string) {
	service.forgetPermissionDenied(accountID, source)
	service.forgetStartupFailure(accountID, source)
}
