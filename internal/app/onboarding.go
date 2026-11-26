package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 新账户自动处理：持续上传的新账户攒够一批（或等待超过上限）后验证登录；停用的账户先在停用状态下验证，
// 验证通过才启用，失效的保持停用，不会接到任何请求。账户默认启用：启动与导入时没有手动停用记录的停用账户
// 已经改为启用（见 account_enable.go），这里多数账户已是启用状态，验证未通过的会被标为需要登录。
// 处理记录与设置保存在账户目录的 .onboarding.json，重启后继续有效：
//   - 首次启用本功能时，已有账户全部记为基线，不做任何改动（不会重新启用你手动停用的账户）
//   - 之后新增的目录（包括服务停止期间上传的）都会进入队列
//   - 目录被删除后记录随之移除，同一账户重新上传会再次处理
//   - 管理页浏览器登录、Chrome 导入创建的账户记为手动添加，不自动处理
//   - 手动停用的账户（有手动停用记录）不自动启用：导入时直接记为跳过，排队或验证期间被手动停用的也保持停用；
//     只有管理页“处理现有停用账户”（确认框写明手动停用的也会启用）加入的例外，重启后尚未处理完的同样例外
//   - 已有账户重新上传了认证文件（目录扫描发现内容变了，或处于失效状态的账户原样重新上传，见 detectLoginUpdates；
//     Chrome 就地重新导入）并载入后仍是启用的，同样加入队列验证一次（见 queueReauth）：不管之前有没有处理记录，复用同一并发上限，
//     日志写“认证文件更新后自动验证”；结果按“重新上传后：已恢复/仍需登录/仍需验证…”记下（见 reauthOutcome）
const (
	onboardingFileName = ".onboarding.json"
	// onboardingTick 为检查队列的周期
	onboardingTick = 5 * time.Second
	// onboardingMaxWait 为不满一批时的最长等待，避免零散上传的账户一直不处理
	onboardingMaxWait = 2 * time.Minute
	// onboardingRetryLimit 与 onboardingRetryDelay：验证过程出错（网络、浏览器异常）时的重试
	onboardingRetryLimit = 3
	onboardingRetryDelay = 2 * time.Minute
	// onboardingRecentLimit 为管理页显示的最近处理记录数
	onboardingRecentLimit = 50
	// onboardingMaxBatch 为单批最多处理的账户数
	onboardingMaxBatch = 200
	// onboardingMaxBatchSize 与 onboardingMaxConcurrency 为设置项上限
	onboardingMaxBatchSize   = 200
	onboardingMaxConcurrency = 8
)

// onboardingSaveDelay 为处理结果合并保存的间隔：账户上万时 .onboarding.json 有几 MB，原先每处理完一个账户就完整重写
// 并同步一次文件，且全程持有队列锁。服务异常退出时最多丢失这几秒的结果，这些账户下次启动时重新进入队列
var onboardingSaveDelay = 2 * time.Second

// 处理结果
const (
	onboardBaseline     = "baseline"
	onboardManual       = "manual"
	onboardSkipped      = "skipped"
	onboardEnabled      = "enabled"
	onboardVerified     = "verified"
	onboardVerifyFailed = "verify_failed"
	onboardError        = "error"
	// onboardDeferred 为暂缓（IP 池固定分配下账户的固定端口暂停使用）：不是结果，不记录、不计重试次数，放回队列等端口恢复
	onboardDeferred = "deferred"
)

type onboardRecord struct {
	Result string    `json:"result"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
	// Source 为 onboardSourceReauth 时是已有账户重新上传认证文件后的处理，Outcome 为重新上传后的结果（见 reauthOutcome）；
	// 旧版本保存的文件里没有，照常读入
	Source  string `json:"source,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

type onboardFile struct {
	Version  int                      `json:"version"`
	Policy   api.OnboardingPolicy     `json:"policy"`
	Accounts map[string]onboardRecord `json:"accounts"`
	// Explicit 为“处理现有停用账户”加入、尚未处理完的账户及加入时间（见 onboardPending.explicitAt）
	Explicit map[string]time.Time `json:"explicit,omitempty"`
	// Reauth 为换上新认证文件后加入、尚未验证完的账户及加入时间（见 onboardPending.reauthAt）
	Reauth map[string]time.Time `json:"reauth,omitempty"`
}

type onboardPending struct {
	queuedAt  time.Time
	attempts  int
	notBefore time.Time
	// explicitAt 非零表示由管理页“处理现有停用账户”在这一时间加入：在这之前手动停用的账户验证通过后也启用
	// （见 disableNotes.blocksAutoEnable）。随处理记录保存（onboardFile.Explicit）：原先只在内存中，账户上千个时
	// 队列要处理几个小时，这期间停止服务或重启，重新排队的账户按普通新账户处理，手动停用的全部记为跳过，
	// 管理员的要求被悄悄丢掉
	explicitAt time.Time
	// reauthAt 非零表示已有账户在这一时间换上了新的认证文件、加入队列重新验证（见 queueReauth），随处理记录保存（onboardFile.Reauth）
	reauthAt time.Time
	// started 为真时这一项已开始验证（见 markStarted）：之后再换认证文件，这次结果属于上一份文件，结束后重新排队（requeue）。
	// 还没开始验证（取进本批、在等并发名额）时验证用的就是内存里新的认证状态，只更新 reauthAt，不重复验证
	started bool
	// requeue 为真时账户在验证开始后又换了一次认证文件：这次的结果写进最近处理与日志后按新的认证文件重新排队
	requeue bool
}

// 重新验证的来源（日志与面板显示）
const (
	reauthSourceAuthFile = "认证文件更新"
	reauthSourceChrome   = "Chrome 重新导入"
	// reauthSourceSameFile 为失效账户重新上传了与当前登录状态相同的认证文件（见 detectLoginUpdates）
	reauthSourceSameFile = "认证文件重新上传（内容相同）"
	// reauthSourceStopped 为进程停止期间被重新写入的认证文件（启动时对照上次关闭时的记录发现，见 reconcileAuthFiles）
	reauthSourceStopped = "服务停止期间重新上传"
	// onboardSourceReauth 为面板最近处理记录里标出的重新验证（api.OnboardingEvent.Source）
	onboardSourceReauth = "reauth"
	// reauthVerifyOffDetail 为自动验证关闭时重新上传的账户只重新载入、不验证的说明
	reauthVerifyOffDetail = "自动验证已关闭"
	// reauthStillDisabledDetail 为换上新认证文件后账户仍是停用（按默认启用失败或账户正忙）、没有验证的说明
	reauthStillDisabledDetail = "账户仍是停用（按默认启用未成功），未验证"
	// reauthWatchWindow 为重新上传后验证通过（已恢复）的账户之后又返回 403 时改记为“仍需验证”的期限（见 noteForbidden）
	reauthWatchWindow = 24 * time.Hour
)

// 重新上传认证文件后的结果（api.OnboardingEvent.Outcome 与处理记录的 Outcome，见 reauthOutcome）
const (
	// reauthRestored 已恢复：验证通过，恢复调度
	reauthRestored = "restored"
	// reauthAuthRequired 仍需登录：验证未通过（浏览器判定未登录或接口复核 401），显示这次的原因
	reauthAuthRequired = "auth_required"
	// reauthVerifyRequired 仍需验证：验证通过，但排队期间或之后又返回需要验证账号（verify-account）的 403
	reauthVerifyRequired = "verify_required"
	// reauthForbidden 仍有 403：验证通过，但排队期间或之后又返回 403 无权限
	reauthForbidden = "forbidden"
	// reauthRestricted 仍受限：组织停用、服务受限等（以验证结果为准）
	reauthRestricted = "restricted"
	// reauthManualDisabled 跳过（手动停用）：只换了认证，保持停用，不验证
	reauthManualDisabled = "manual_disabled"
	// reauthVerifyOff 已重新载入，未验证（自动验证已关闭）
	reauthVerifyOff = "verify_off"
	// reauthError 验证出错（重试用完）
	reauthError = "error"
	// reauthSkipped 其他跳过
	reauthSkipped = "skipped"
)

// reauthOutcomeLabels 为重新上传后结果写进日志的说法
var reauthOutcomeLabels = map[string]string{
	reauthRestored:       "已恢复",
	reauthAuthRequired:   "仍需登录",
	reauthVerifyRequired: "仍需验证",
	reauthForbidden:      "仍有 403",
	reauthRestricted:     "仍受限",
	reauthManualDisabled: "跳过（手动停用）",
	reauthVerifyOff:      "已重新载入，未验证（自动验证已关闭）",
	reauthError:          "验证出错",
	reauthSkipped:        "跳过",
}

// onboardManuallyDisabled 为手动停用的账户不自动启用时记下的说明
const onboardManuallyDisabled = "账户是手动停用的，保持停用"

// accountOnboarder 维护新账户队列并按批处理
type accountOnboarder struct {
	admin      *runtimeAdmin
	path       string
	mu         sync.Mutex
	policy     api.OnboardingPolicy
	records    map[string]onboardRecord
	pending    map[string]*onboardPending
	processing map[string]*onboardPending
	recent     []api.OnboardingEvent
	wake       chan struct{}
	// savePending 为真时已有一次合并保存在等待（见 scheduleSaveLocked）
	savePending bool
	// unchanged 为本次运行中健康账户换上了内容相同的认证文件、没有重新认证的次数（见 noteUnchanged）；sameLogin 为就绪账户换上了
	// 内容有变化、登录签名没变的认证文件，按原口径没有重新载入的次数（见 noteSameLogin）；notReuploaded 为失效账户换上的认证文件被当成
	// 本服务自己的文件（保留了原文件的修改时间）或共用账户目录的其他进程回写、没有按重新上传处理的次数（见 noteNotReuploaded）
	unchanged     int
	sameLogin     int
	notReuploaded int
	// restored 为最近一次重新上传后验证通过（已恢复）的账户与那次的时间（reauthWatchWindow 之内），noteForbidden 先在这里查：
	// 它在生成请求的 403 路径上，不在这张表里的账户不取 mu（mu 在保存处理记录时持有，要整份编码并写盘）
	restored sync.Map
}

func defaultOnboardingPolicy() api.OnboardingPolicy {
	return api.OnboardingPolicy{AutoVerify: true, AutoEnable: true, BatchSize: 10, Concurrency: 3}
}

// validateOnboardingPolicy 校验设置范围
func validateOnboardingPolicy(policy api.OnboardingPolicy) error {
	if policy.BatchSize < 1 || policy.BatchSize > onboardingMaxBatchSize {
		return fmt.Errorf("每批数量必须在 1 到 %d 之间", onboardingMaxBatchSize)
	}
	if policy.Concurrency < 1 || policy.Concurrency > onboardingMaxConcurrency {
		return fmt.Errorf("验证并发必须在 1 到 %d 之间", onboardingMaxConcurrency)
	}
	return nil
}

func newAccountOnboarder(admin *runtimeAdmin, directory string) *accountOnboarder {
	onboarder := &accountOnboarder{
		admin:      admin,
		path:       filepath.Join(directory, onboardingFileName),
		policy:     defaultOnboardingPolicy(),
		records:    make(map[string]onboardRecord),
		pending:    make(map[string]*onboardPending),
		processing: make(map[string]*onboardPending),
		wake:       make(chan struct{}, 1),
	}
	onboarder.load()
	return onboarder
}

// load 读取处理记录；文件不存在时把现有账户全部记为基线
func (onboarder *accountOnboarder) load() {
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	existing := false
	var explicit, reauth map[string]time.Time
	if content, err := os.ReadFile(onboarder.path); err == nil {
		var file onboardFile
		if err := json.Unmarshal(content, &file); err == nil {
			existing = true
			explicit = file.Explicit
			reauth = file.Reauth
			if validateOnboardingPolicy(file.Policy) == nil {
				onboarder.policy = file.Policy
			}
			// 记录按账户 ID（目录名）原样保存与查找；原先读取时转成小写，与原样的账户 ID 对不上，
			// 大小写混合的目录每次重启都会被重新加入队列
			for id, record := range file.Accounts {
				onboarder.records[strings.TrimSpace(id)] = record
			}
		} else {
			onboarder.admin.requests.log("auth", "WARN", "新账户自动处理记录损坏，已重新建立基线 | 错误="+err.Error())
		}
	}
	now := time.Now()
	present := make(map[string]struct{})
	queued, resumed, reauthQueued := 0, 0, 0
	var latestExplicit time.Time
	for _, status := range onboarder.admin.pool.Status() {
		present[status.ID] = struct{}{}
		if _, done := onboarder.records[status.ID]; done {
			continue
		}
		if !existing {
			onboarder.records[status.ID] = onboardRecord{Result: onboardBaseline, At: now}
			continue
		}
		// “处理现有停用账户”加入、重启前没有处理完的账户仍按管理员当时的要求处理；换上新认证文件后待验证的仍按重新验证处理
		explicitAt := explicit[status.ID]
		onboarder.pending[status.ID] = &onboardPending{queuedAt: now, explicitAt: explicitAt, reauthAt: reauth[status.ID]}
		queued++
		if !reauth[status.ID].IsZero() {
			reauthQueued++
		}
		if !explicitAt.IsZero() {
			resumed++
			if explicitAt.After(latestExplicit) {
				latestExplicit = explicitAt
			}
		}
	}
	if !latestExplicit.IsZero() {
		accountDisableNotes.noteExplicitRequest(latestExplicit)
	}
	// 目录已不存在的账户移除记录，重新上传时会再次处理
	for id := range onboarder.records {
		if _, exists := present[id]; !exists {
			delete(onboarder.records, id)
		}
	}
	if !existing {
		onboarder.admin.requests.log("auth", "INFO", fmt.Sprintf(
			"新账户自动处理已开启 | 现有 %d 个账户记为基线不做改动，之后新增的账户将自动验证、通过后启用", len(present),
		))
	} else if queued > 0 {
		message := fmt.Sprintf("新账户自动处理 | 发现服务停止期间新增的账户 %d 个，已加入队列", queued)
		switch {
		case reauthQueued > 0:
			// 有认证文件更新后待验证的（已有账户，不是新增的）：分项写明，不写成“服务停止期间新增的账户”
			parts := make([]string, 0, 3)
			if added := queued - resumed - reauthQueued; added > 0 {
				parts = append(parts, fmt.Sprintf("服务停止期间新增 %d 个", added))
			}
			if resumed > 0 {
				parts = append(parts, fmt.Sprintf("“处理现有停用账户”加入、尚未处理完的 %d 个（仍按当时的要求处理）", resumed))
			}
			parts = append(parts, fmt.Sprintf("认证文件更新后待验证的 %d 个", reauthQueued))
			message = fmt.Sprintf("新账户自动处理 | 重新加入队列 %d 个账户：%s", queued, strings.Join(parts, "、"))
		case resumed > 0:
			message = fmt.Sprintf(
				"新账户自动处理 | 重新加入队列 %d 个账户，其中 %d 个是“处理现有停用账户”加入、尚未处理完的，仍按当时的要求处理", queued, resumed,
			)
		}
		onboarder.admin.requests.log("auth", "INFO", message)
	}
	for id, record := range onboarder.records {
		if record.Source == onboardSourceReauth && record.Outcome == reauthRestored && now.Sub(record.At) <= reauthWatchWindow {
			onboarder.restored.Store(id, record.At)
		}
	}
	onboarder.saveLocked()
}

// saveLocked 原子写入处理记录；调用方持有 mu。等待中的合并保存随之完成
func (onboarder *accountOnboarder) saveLocked() {
	onboarder.savePending = false
	explicit := make(map[string]time.Time)
	reauth := make(map[string]time.Time)
	for _, items := range []map[string]*onboardPending{onboarder.pending, onboarder.processing} {
		for id, item := range items {
			if !item.explicitAt.IsZero() {
				explicit[id] = item.explicitAt
			}
			if !item.reauthAt.IsZero() {
				reauth[id] = item.reauthAt
			}
		}
	}
	content, err := json.MarshalIndent(onboardFile{
		Version: 1, Policy: onboarder.policy, Accounts: onboarder.records, Explicit: explicit, Reauth: reauth,
	}, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(onboarder.path, content, 0o600); err != nil {
		if onboarder.admin != nil && onboarder.admin.requests != nil {
			onboarder.admin.requests.log("service", "WARN", "新账户自动处理记录保存失败 | "+err.Error())
		}
	}
}

// scheduleSaveLocked 在 onboardingSaveDelay 后保存一次，期间的多次变化合并写入；调用方持有 mu
func (onboarder *accountOnboarder) scheduleSaveLocked() {
	if onboarder.savePending {
		return
	}
	onboarder.savePending = true
	time.AfterFunc(onboardingSaveDelay, onboarder.flush)
}

// flush 写出等待合并保存的处理结果；生成服务停止与关闭时也调用。没有待保存的变化时什么都不做
func (onboarder *accountOnboarder) flush() {
	if onboarder == nil {
		return
	}
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	if onboarder.savePending {
		onboarder.saveLocked()
	}
}

func (onboarder *accountOnboarder) signal() {
	select {
	case onboarder.wake <- struct{}{}:
	default:
	}
}

// enqueue 把目录扫描新导入的账户加入队列
func (onboarder *accountOnboarder) enqueue(accountIDs []string) {
	if onboarder == nil || len(accountIDs) == 0 {
		return
	}
	onboarder.mu.Lock()
	now := time.Now()
	changed := false
	for _, id := range accountIDs {
		if _, done := onboarder.records[id]; done {
			continue
		}
		if _, queued := onboarder.pending[id]; queued {
			continue
		}
		if _, active := onboarder.processing[id]; active {
			continue
		}
		if !onboarder.policy.AutoVerify && !onboarder.policy.AutoEnable {
			onboarder.records[id] = onboardRecord{Result: onboardSkipped, At: now}
			changed = true
			continue
		}
		onboarder.pending[id] = &onboardPending{queuedAt: now}
	}
	if changed {
		onboarder.saveLocked()
	}
	onboarder.mu.Unlock()
	onboarder.signal()
}

// skipManuallyDisabled 把目录扫描导入时保持停用的账户（有手动停用记录）直接记为跳过，不进入队列：
// 原先与其他新账户一起排队，验证通过后被启用，手动停用记录也被清除
func (onboarder *accountOnboarder) skipManuallyDisabled(accountIDs []string) {
	if onboarder == nil || len(accountIDs) == 0 {
		return
	}
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	now := time.Now()
	changed := false
	for _, id := range accountIDs {
		if _, done := onboarder.records[id]; done {
			continue
		}
		if _, active := onboarder.processing[id]; active {
			continue
		}
		delete(onboarder.pending, id)
		onboarder.records[id] = onboardRecord{Result: onboardSkipped, Detail: onboardManuallyDisabled, At: now}
		changed = true
	}
	if changed {
		onboarder.saveLocked()
	}
}

// queueReauth 把换上了新认证文件的已有账户（调用方已载入新文件、账户是启用的）加入队列，用新的 Cookie 验证一次：
// 通过的恢复就绪、照常参与预热与调度，未通过的按这次的原因显示需要登录。不管之前有没有处理记录（基线、已验证、手动添加……）
// 都加入：enqueue 会跳过已有记录的账户，原先换了认证文件的账户不会再验证。加入时间往前拨，下一轮立即开批，不等攒够一批；
// 并发与每批上限照旧（policy.Concurrency，最多 8），上千个账户一起换认证文件时按批依次验证，不阻塞目录扫描。
//   - 自动验证关闭：只载入新的认证文件，不加入（写一行说明）
//   - 已在队列中：重新验证的项更新加入时间（立即可开批），在等出错重试的清零重试次数、不再等重试间隔（上一份文件出的错），
//     因固定端口暂停而暂缓的照旧等端口恢复；新账户等其他项照原样（重试次数、暂缓时间都不动），验证时用的就是内存里新的认证状态
//   - 已取进本批、还没开始验证：只更新加入时间，验证时用的就是新的认证状态
//   - 正在验证：这次结果写进最近处理与日志后按新的认证文件重新排队（见 finish）
//
// source 为来源（认证文件更新、Chrome 重新导入），写进日志
func (onboarder *accountOnboarder) queueReauth(accountID string, source string) {
	if onboarder == nil || accountID == "" {
		return
	}
	onboarder.mu.Lock()
	if !onboarder.policy.AutoVerify {
		onboarder.mu.Unlock()
		onboarder.admin.requests.log("auth", "INFO", fmt.Sprintf(
			"认证文件更新后自动验证 | 自动验证已关闭，只重新载入 | 账户=%s | 来源=%s", accountID, source,
		))
		onboarder.noteReauthResult(accountID, source, onboardSkipped, reauthVerifyOffDetail)
		return
	}
	now := time.Now()
	outcome := "已加入队列"
	switch {
	case onboarder.processing[accountID] != nil && !onboarder.processing[accountID].started:
		// 已取进本批、还没开始验证：验证用的就是内存里新的认证状态，只更新加入时间（之后的 403 从这时算起），不重复验证
		onboarder.processing[accountID].reauthAt = now
		outcome = "已在本批中、尚未开始验证，按新的认证文件验证"
	case onboarder.processing[accountID] != nil:
		onboarder.processing[accountID].requeue = true
		outcome = "正在验证，结束后按新的认证文件再验证一次"
	case onboarder.pending[accountID] != nil:
		item := onboarder.pending[accountID]
		if !item.reauthAt.IsZero() {
			item.reauthAt, item.queuedAt = now, now.Add(-onboardingMaxWait)
			if item.attempts > 0 {
				// 在等出错重试（attempts 只在出错时增加）：出错的是上一份认证文件，按新的立即重试；只是暂缓（attempts 为 0）的照旧等端口恢复
				item.attempts, item.notBefore = 0, time.Time{}
			}
		}
		outcome = "已在队列中，按新的认证文件验证"
	default:
		delete(onboarder.records, accountID)
		onboarder.pending[accountID] = &onboardPending{queuedAt: now.Add(-onboardingMaxWait), reauthAt: now}
	}
	onboarder.scheduleSaveLocked()
	onboarder.mu.Unlock()
	onboarder.admin.requests.log("auth", "INFO", fmt.Sprintf(
		"认证文件更新后自动验证 | %s | 账户=%s | 来源=%s", outcome, accountID, source,
	))
	onboarder.signal()
}

// noteReauthResult 记下重新上传认证文件后没有进队列的账户的结果（手动停用的只换认证、自动验证关闭时只重新载入）：
// 写进处理记录与面板的最近处理（标为认证文件更新），日志写“重新上传后：…”。已在队列或处理中的不记（它们的结果由处理记下）
func (onboarder *accountOnboarder) noteReauthResult(accountID string, source string, result string, detail string) {
	if onboarder == nil || accountID == "" {
		return
	}
	outcome := reauthSkipped
	switch detail {
	case onboardManuallyDisabled:
		outcome = reauthManualDisabled
	case reauthVerifyOffDetail:
		outcome = reauthVerifyOff
	}
	onboarder.mu.Lock()
	if onboarder.pending[accountID] != nil || onboarder.processing[accountID] != nil {
		onboarder.mu.Unlock()
		return
	}
	onboarder.recordReauthLocked(accountID, result, detail, outcome, time.Now())
	onboarder.scheduleSaveLocked()
	onboarder.mu.Unlock()
	onboarder.logReauthOutcome(accountID, outcome, detail, source)
}

// recordReauthLocked 写下重新上传后的结果：处理记录与面板的最近处理（标为认证文件更新），已恢复的记进 restored（见 noteForbidden）。
// 调用方持有 mu
func (onboarder *accountOnboarder) recordReauthLocked(accountID string, result string, detail string, outcome string, now time.Time) {
	onboarder.records[accountID] = onboardRecord{
		Result: result, Detail: detail, At: now, Source: onboardSourceReauth, Outcome: outcome,
	}
	onboarder.noteRestoredLocked(accountID, outcome, now)
	onboarder.pushRecentLocked(api.OnboardingEvent{
		AccountID: accountID, Result: result, Detail: detail, At: now, Source: onboardSourceReauth, Outcome: outcome,
	})
}

// noteRestoredLocked 按账户这次重新上传后的结果维护 restored：已恢复的记下时间，其他结果移除。调用方持有 mu
func (onboarder *accountOnboarder) noteRestoredLocked(accountID string, outcome string, at time.Time) {
	if outcome == reauthRestored {
		onboarder.restored.Store(accountID, at)
		return
	}
	onboarder.restored.Delete(accountID)
}

// pushRecentLocked 把一条处理结果放到面板最近处理的最前面，超过 onboardingRecentLimit 的丢掉。调用方持有 mu
func (onboarder *accountOnboarder) pushRecentLocked(event api.OnboardingEvent) {
	onboarder.recent = append([]api.OnboardingEvent{event}, onboarder.recent...)
	if len(onboarder.recent) > onboardingRecentLimit {
		onboarder.recent = onboarder.recent[:onboardingRecentLimit]
	}
}

// logReauthOutcome 写一行重新上传后的结果：“认证文件更新后 | 重新上传后：已恢复 | 账户=… | 原因=… | 来源=…”（原因、来源为空时不写）
func (onboarder *accountOnboarder) logReauthOutcome(accountID string, outcome string, detail string, source string) {
	level := "INFO"
	switch outcome {
	case reauthAuthRequired, reauthVerifyRequired, reauthForbidden, reauthRestricted, reauthError:
		level = "WARN"
	}
	message := "认证文件更新后 | 重新上传后：" + reauthOutcomeLabels[outcome] + " | 账户=" + accountID
	if detail = strings.TrimSpace(detail); detail != "" {
		message += " | 原因=" + detail
	}
	if source != "" {
		message += " | 来源=" + source
	}
	onboarder.admin.requests.log("auth", level, message)
}

// reauthOutcome 按处理结果归类重新上传后的结果（在锁外调用：要查账户池与登记表），返回结果与显示的原因：
//   - 验证通过（或启用）：排队之后（since 之后）又返回过 403 时为仍需验证（需要验证账号）或仍有 403，原因为那次 403 的原文；否则已恢复
//   - 验证未通过：组织停用或受限时为仍受限，否则为仍需登录，原因为这次验证的原因
//   - 跳过：手动停用、自动验证已关闭、组织停用分别归类；出错为验证出错
func (onboarder *accountOnboarder) reauthOutcome(accountID string, result string, detail string, since time.Time) (string, string) {
	switch result {
	case onboardVerified, onboardEnabled:
		if outcome, reason := onboarder.admin.service.reauthForbiddenSince(accountID, since); outcome != "" {
			return outcome, reason
		}
		return reauthRestored, detail
	case onboardVerifyFailed:
		if onboarder.admin.service.orgDisabledListed(accountID) {
			return reauthRestricted, detail
		}
		return reauthAuthRequired, detail
	case onboardError:
		return reauthError, detail
	}
	switch {
	case detail == onboardManuallyDisabled:
		return reauthManualDisabled, detail
	case detail == reauthVerifyOffDetail:
		return reauthVerifyOff, detail
	case onboarder.admin.service.orgDisabledListed(accountID):
		return reauthRestricted, detail
	}
	return reauthSkipped, detail
}

// noteForbidden 账户返回了一次 403 无权限（已登记，见 recordPermissionDenied）：它最近一次重新上传后的结果是已恢复、而且在
// reauthWatchWindow 之内时，改记为仍需验证（需要验证账号）或仍有 403，原因为这次 403 的原文，最近处理里加一条、写一行日志。
// 验证只看登录是否有效，看不出账号还要不要在官网验证；在官网解开 403 之后重新上传、实际还没解开的账户，第一次请求时在这里看出来。
// 在生成请求的 403 路径上：先查 restored（不取 mu），不是最近已恢复的账户直接返回，不等 mu（保存处理记录时持有，要整份编码并写盘）
func (onboarder *accountOnboarder) noteForbidden(accountID string, message string, at time.Time) {
	if onboarder == nil {
		return
	}
	value, listed := onboarder.restored.Load(accountID)
	if !listed {
		return
	}
	restoredAt, _ := value.(time.Time)
	if at.Sub(restoredAt) > reauthWatchWindow {
		onboarder.restored.CompareAndDelete(accountID, value)
		return
	}
	if !at.After(restoredAt) {
		return
	}
	outcome := reauthForbidden
	if accountLevelPermissionDenied(message) {
		outcome = reauthVerifyRequired
	}
	reason := authRequiredCauseSummary(errors.New(message))
	onboarder.mu.Lock()
	record, exists := onboarder.records[accountID]
	if !exists || record.Source != onboardSourceReauth || record.Outcome != reauthRestored ||
		!at.After(record.At) || at.Sub(record.At) > reauthWatchWindow {
		// 记录已经变了（重新排队、改记过、账户移除）：表里的这一项过时了
		onboarder.restored.CompareAndDelete(accountID, value)
		onboarder.mu.Unlock()
		return
	}
	onboarder.recordReauthLocked(accountID, record.Result, reason, outcome, at)
	onboarder.scheduleSaveLocked()
	onboarder.mu.Unlock()
	onboarder.logReauthOutcome(accountID, outcome, reason, "验证通过后的请求")
}

// noteUnchanged 记一次“认证文件未变化”（健康账户换上了内容相同的认证文件，见 noteAuthUnchanged），在面板上显示
func (onboarder *accountOnboarder) noteUnchanged() {
	if onboarder == nil {
		return
	}
	onboarder.mu.Lock()
	onboarder.unchanged++
	onboarder.mu.Unlock()
}

// noteSameLogin 记一次“登录签名没变，未重新载入”（就绪账户换上了内容有变化、登录签名没变的认证文件，见 noteSameLoginAuthFile），
// 在面板上显示
func (onboarder *accountOnboarder) noteSameLogin() {
	if onboarder == nil {
		return
	}
	onboarder.mu.Lock()
	onboarder.sameLogin++
	onboarder.mu.Unlock()
}

// noteNotReuploaded 记一次“失效账户的上传未按重新上传处理”（失效账户换上的认证文件被当成本服务自己的文件或其他进程回写，
// 见 noteOwnedRewrite、noteWriteBackAuthFile），在面板上显示：原先只有一行日志，面板上什么都看不出来，看起来“上传后没变化”
func (onboarder *accountOnboarder) noteNotReuploaded() {
	if onboarder == nil {
		return
	}
	onboarder.mu.Lock()
	onboarder.notReuploaded++
	onboarder.mu.Unlock()
}

// noteManualRestored 账户在管理页验证通过或重新登录成功（detail 为怎样恢复的，写进最近处理与日志）：它最近一次重新上传后的结果是
// 仍需登录、仍需验证、仍有 403、仍受限或验证出错时改记为已恢复（最近处理加一条，写一行日志）。原先这条记录一直留着（并随处理记录保存），
// 账户已经就绪，面板仍计“重新上传后：仍需登录”。之后 24 小时内的请求又返回 403 时照常改记（见 noteForbidden）。没有这种记录时什么都不做
func (onboarder *accountOnboarder) noteManualRestored(accountID string, detail string) {
	if onboarder == nil || accountID == "" {
		return
	}
	onboarder.mu.Lock()
	record, exists := onboarder.records[accountID]
	stale := exists && record.Source == onboardSourceReauth
	switch record.Outcome {
	case reauthAuthRequired, reauthVerifyRequired, reauthForbidden, reauthRestricted, reauthError:
	default:
		stale = false
	}
	if !stale {
		onboarder.mu.Unlock()
		return
	}
	onboarder.recordReauthLocked(accountID, onboardVerified, detail, reauthRestored, time.Now())
	onboarder.scheduleSaveLocked()
	onboarder.mu.Unlock()
	onboarder.logReauthOutcome(accountID, reauthRestored, detail, "管理页")
}

// markManual 记录管理页手动添加的账户，不做自动处理
func (onboarder *accountOnboarder) markManual(accountID string) {
	if onboarder == nil {
		return
	}
	onboarder.mu.Lock()
	onboarder.records[accountID] = onboardRecord{Result: onboardManual, At: time.Now()}
	delete(onboarder.pending, accountID)
	onboarder.saveLocked()
	onboarder.mu.Unlock()
}

// forget 账户被删除或目录消失时移除记录，重新上传后会再次处理
func (onboarder *accountOnboarder) forget(accountID string) {
	onboarder.forgetMany([]string{accountID})
}

// forgetMany 一次移除多个账户的记录，只写一次文件：账户上万时 .onboarding.json 有几 MB，批量删除几百个账户时
// 逐个 forget 每次都要完整重写并同步一次文件
func (onboarder *accountOnboarder) forgetMany(accountIDs []string) {
	if onboarder == nil || len(accountIDs) == 0 {
		return
	}
	onboarder.mu.Lock()
	for _, accountID := range accountIDs {
		delete(onboarder.records, accountID)
		delete(onboarder.pending, accountID)
	}
	onboarder.saveLocked()
	onboarder.mu.Unlock()
}

// run 在生成服务实例存活期间处理队列
func (onboarder *accountOnboarder) run(ctx context.Context) {
	ticker := time.NewTicker(onboardingTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			onboarder.flush()
			return
		case <-ticker.C:
		case <-onboarder.wake:
		}
		batch, policy := onboarder.takeBatch(time.Now())
		if len(batch) > 0 {
			onboarder.processBatch(ctx, batch, policy)
		}
	}
}

// takeBatch 判断是否开始一批：待处理数达到每批数量，或最早的账户已等待超过上限，或有到期的重试
func (onboarder *accountOnboarder) takeBatch(now time.Time) ([]string, api.OnboardingPolicy) {
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	policy := onboarder.policy
	if !policy.AutoVerify && !policy.AutoEnable {
		if len(onboarder.pending) > 0 {
			for id := range onboarder.pending {
				onboarder.records[id] = onboardRecord{Result: onboardSkipped, At: now}
			}
			onboarder.pending = make(map[string]*onboardPending)
			onboarder.saveLocked()
		}
		return nil, policy
	}
	eligible := make([]string, 0, len(onboarder.pending))
	trigger := false
	for id, item := range onboarder.pending {
		if now.Before(item.notBefore) {
			continue
		}
		eligible = append(eligible, id)
		if item.attempts > 0 || now.Sub(item.queuedAt) >= onboardingMaxWait {
			trigger = true
		}
	}
	if len(eligible) == 0 || !trigger && len(eligible) < policy.BatchSize {
		return nil, policy
	}
	sort.Slice(eligible, func(left, right int) bool {
		return onboarder.pending[eligible[left]].queuedAt.Before(onboarder.pending[eligible[right]].queuedAt)
	})
	if len(eligible) > onboardingMaxBatch {
		eligible = eligible[:onboardingMaxBatch]
	}
	for _, id := range eligible {
		onboarder.processing[id] = onboarder.pending[id]
		delete(onboarder.pending, id)
	}
	return eligible, policy
}

// processBatch 按并发上限处理一批账户
func (onboarder *accountOnboarder) processBatch(ctx context.Context, batch []string, policy api.OnboardingPolicy) {
	startedAt := time.Now()
	// 本批全是重新上传的（认证文件更新后的重新验证）时日志开头写“认证文件更新后自动验证”；混有新账户时照原句、末尾写其中的个数。
	// 没有重新上传的与原来逐字相同
	reauth := onboarder.countReauth(batch)
	subject := onboardLogPrefix(reauth > 0 && reauth == len(batch))
	message := fmt.Sprintf(
		"%s开始 | 本批=%d | 并发=%d | 自动验证=%t | 自动启用=%t",
		subject, len(batch), policy.Concurrency, policy.AutoVerify, policy.AutoEnable,
	)
	if reauth > 0 && reauth < len(batch) {
		message += fmt.Sprintf(" | 其中认证文件更新=%d", reauth)
	}
	onboarder.admin.requests.log("auth", "INFO", message)
	var countsMu sync.Mutex
	counts := make(map[string]int)
	limiter := make(chan struct{}, policy.Concurrency)
	var work sync.WaitGroup
	for _, id := range batch {
		work.Add(1)
		go func(id string) {
			defer work.Done()
			select {
			case limiter <- struct{}{}:
			case <-ctx.Done():
				onboarder.finish(id, "", "", false)
				return
			}
			defer func() { <-limiter }()
			result, detail, retry := onboarder.process(ctx, id, policy)
			if ctx.Err() != nil {
				// 服务正在停止：不写结果，下次启动时会重新进入队列
				onboarder.finish(id, "", "", false)
				return
			}
			if finalResult := onboarder.finish(id, result, detail, retry); finalResult != "" {
				countsMu.Lock()
				counts[finalResult]++
				countsMu.Unlock()
			}
		}(id)
	}
	work.Wait()
	if ctx.Err() != nil {
		return
	}
	message = fmt.Sprintf(
		"%s完成 | 已启用=%d | 验证通过=%d | 验证未通过=%d | 出错=%d",
		subject, counts[onboardEnabled], counts[onboardVerified], counts[onboardVerifyFailed], counts[onboardError],
	)
	if reauth > 0 && reauth < len(batch) {
		message += fmt.Sprintf(" | 其中认证文件更新=%d", reauth)
	}
	onboarder.admin.requests.log("auth", "INFO", message+" | 耗时="+time.Since(startedAt).Round(time.Second).String())
}

// deferUntil 让处理中的账户放回队列后不早于 at 再处理（暂缓，见 onboardDeferred）
func (onboarder *accountOnboarder) deferUntil(accountID string, at time.Time) {
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	if item := onboarder.processing[accountID]; item != nil {
		item.notBefore = at
	}
}

// explicitAt 返回处理中的账户由“处理现有停用账户”加入的时间；普通新账户为零
func (onboarder *accountOnboarder) explicitAt(accountID string) time.Time {
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	if item := onboarder.processing[accountID]; item != nil {
		return item.explicitAt
	}
	return time.Time{}
}

// markStarted 记下处理中的账户已开始验证（见 onboardPending.started）
func (onboarder *accountOnboarder) markStarted(accountID string) {
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	if item := onboarder.processing[accountID]; item != nil {
		item.started = true
	}
}

// reauthItem 返回处理中的账户是不是换上新认证文件后的重新验证（见 queueReauth）
func (onboarder *accountOnboarder) reauthItem(accountID string) bool {
	return !onboarder.reauthSince(accountID).IsZero()
}

// reauthSince 返回处理中的重新验证项加入队列的时间（换上新认证文件、清除旧判定的时间）；不是重新验证时为零
func (onboarder *accountOnboarder) reauthSince(accountID string) time.Time {
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	if item := onboarder.processing[accountID]; item != nil {
		return item.reauthAt
	}
	return time.Time{}
}

// countReauth 返回这一批（已在处理中）里重新验证的账户数
func (onboarder *accountOnboarder) countReauth(batch []string) int {
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	count := 0
	for _, id := range batch {
		if item := onboarder.processing[id]; item != nil && !item.reauthAt.IsZero() {
			count++
		}
	}
	return count
}

// onboardLogPrefix 为处理日志的开头：重新验证写“认证文件更新后自动验证”，其余写“新账户自动处理”
func onboardLogPrefix(reauth bool) string {
	if reauth {
		return "认证文件更新后自动验证"
	}
	return "新账户自动处理"
}

// process 处理单个账户：先验证（账户仍保持原状态），通过后再启用。手动停用的账户不启用：处理开始时就是手动停用的
// 直接跳过（不启动浏览器验证）；处理开始时是启用的、验证期间被停用的也不启用（只有管理页或管理接口会停用账户）；
// 启用时在账户租约内再看一次停用记录（见 updateAccount）。原先验证后只看账户是否停用，排队等待（最长 2 分钟，
// 批量导入时可达几个小时）或验证期间被手动停用的账户也会被启用，手动停用记录随之清除
func (onboarder *accountOnboarder) process(
	ctx context.Context,
	accountID string,
	policy api.OnboardingPolicy,
) (result string, detail string, retry bool) {
	status, ok := onboarder.accountStatus(accountID)
	if !ok {
		return "", "", false
	}
	label := status.Label
	explicitAt := onboarder.explicitAt(accountID)
	// 换上新认证文件后的重新验证（见 queueReauth）：流程相同，日志开头写“认证文件更新后自动验证”
	reauth := onboarder.reauthItem(accountID)
	prefix := onboardLogPrefix(reauth)
	if !status.Enabled && accountDisableNotes.blocksAutoEnable(accountID, explicitAt) {
		onboarder.admin.requests.log(label, "INFO", prefix+" | "+onboardManuallyDisabled)
		return onboardSkipped, onboardManuallyDisabled, false
	}
	initiallyEnabled := status.Enabled
	// 重新上传后的重新验证不按组织停用跳过：重新上传时已清除组织停用与受限的旧判定，以这次验证的结果为准（仍受限时验证会重新登记）
	if !reauth && onboarder.admin.service.orgDisabledListed(accountID) {
		// 账号所属组织停用了 AI Studio（或服务受限等，见 org_disabled.go）：验证与启用都没有意义，不启动浏览器，记为跳过并写明原因
		reason := aistudio.RestrictionReason(onboarder.admin.service.orgDisabledKind(accountID))
		onboarder.admin.requests.log(label, "INFO", prefix+" | 跳过 | 原因="+reason)
		return onboardSkipped, reason, false
	}
	if reauth && !policy.AutoVerify {
		// 排队之后关闭了自动验证：新的认证文件已经载入，只是不再验证
		onboarder.admin.requests.log(label, "INFO", prefix+" | 跳过 | 原因=自动验证已关闭")
		return onboardSkipped, "自动验证已关闭", false
	}
	verified := false
	if policy.AutoVerify {
		onboarder.markStarted(accountID)
		verification, err := onboarder.admin.verifyLogin(ctx, accountID)
		var paused *proxyPoolPausedError
		if errors.As(err, &paused) {
			// IP 池固定分配下账户的固定端口暂停使用（冷却中或被其他进程占用，固定分配不换端口）：不是验证出错，不计重试次数、
			// 不写停用原因，等端口恢复后再处理
			notBefore := time.Now().Add(onboardingRetryDelay)
			if paused.Until.After(notBefore) {
				notBefore = paused.Until
			}
			onboarder.deferUntil(accountID, notBefore)
			onboarder.admin.requests.log(label, "INFO", fmt.Sprintf(
				"%s暂缓 | 原因=IP 池固定端口暂停使用 | 端口=%d | 下次=%s | 不计重试次数，端口恢复后再处理",
				prefix, paused.Port, notBefore.UTC().Format(time.RFC3339)))
			return onboardDeferred, strings.TrimSpace(err.Error()), false
		}
		if err != nil {
			return onboardError, strings.TrimSpace(err.Error()), true
		}
		if status, ok = onboarder.accountStatus(accountID); !ok {
			return "", "", false
		}
		if failed, reason := onboardVerifyFailure(verification, status); failed {
			outcome := "验证未通过，保持停用"
			switch {
			case verification.OrgDisabled:
				outcome = "验证未通过，" + restrictionSubject(verification.Restriction) + "，停止调度"
			case status.Enabled:
				outcome = "验证未通过，账户需要重新登录"
			}
			onboarder.admin.requests.log(label, "WARN", prefix+" | "+outcome+" | 原因="+reason)
			return onboardVerifyFailed, reason, false
		}
		verified = true
	}
	if policy.AutoEnable && !status.Enabled {
		if initiallyEnabled {
			onboarder.admin.requests.log(label, "INFO", prefix+" | 处理期间被停用，"+onboardManuallyDisabled)
			return onboardSkipped, onboardManuallyDisabled, false
		}
		// 启用后还没有模型目录时（未经验证直接启用、验证时目录同步失败），updateAccount 会在后台补一次同步
		dto := adminAccountDTO(status)
		_, err := onboarder.admin.updateAccount(ctx, accountID, api.AccountInput{
			Label: dto.Label, Enabled: true, Proxy: dto.Proxy, Locale: dto.Locale, Timezone: dto.Timezone,
		}, &automaticEnable{explicitAt: explicitAt})
		if errors.Is(err, errAccountManuallyDisabled) {
			onboarder.admin.requests.log(label, "INFO", prefix+" | "+onboardManuallyDisabled)
			return onboardSkipped, onboardManuallyDisabled, false
		}
		if err != nil {
			return onboardError, strings.TrimSpace(err.Error()), true
		}
		onboarder.admin.requests.log(label, "INFO", prefix+" | 已启用")
		return onboardEnabled, "", false
	}
	if verified {
		if reauth {
			onboarder.admin.requests.log(label, "INFO", prefix+" | 通过，恢复调度")
		}
		return onboardVerified, "", false
	}
	return onboardSkipped, "账户已是启用状态", false
}

// onboardVerifyFailure 判断验证是否未通过并给出原因：浏览器验证未认证，或验证后账户被标为需要登录（启用账户的模型目录
// 同步确认登录失效）。停用账户的对外状态始终是 disabled，原先只看状态，停用账户验证未通过也被当成通过并启用，
// 面板上“验证未通过”一直是 0
func onboardVerifyFailure(verification aistudio.LoginVerification, status aistudio.AccountStatus) (bool, string) {
	if verification.Authenticated && status.State != aistudio.AccountAuthRequired {
		return false, ""
	}
	reason := ""
	if !verification.Authenticated {
		reason = strings.TrimSpace(verification.Reason)
	}
	if reason == "" {
		reason = strings.TrimSpace(status.Message)
	}
	if reason == "" {
		reason = "AI Studio 登录已失效"
	}
	return true, reason
}

// finish 记录处理结果；出错且未超过重试次数时稍后重试。返回最终结果（仍在重试或暂缓时为空）
func (onboarder *accountOnboarder) finish(accountID string, result string, detail string, retry bool) string {
	// 先在锁外查询账户是否仍存在，避免持有本锁时再去拿账户池锁；重新验证的账户同样先在锁外归类重新上传后的结果（查账户池与登记表）
	exists := true
	if result == "" || result == onboardDeferred {
		_, exists = onboarder.accountStatus(accountID)
	}
	outcome, outcomeDetail := "", detail
	if result != "" && result != onboardDeferred {
		if since := onboarder.reauthSince(accountID); !since.IsZero() {
			outcome, outcomeDetail = onboarder.reauthOutcome(accountID, result, detail, since)
		}
	}
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	item := onboarder.processing[accountID]
	delete(onboarder.processing, accountID)
	now := time.Now()
	// 处理期间又换了一次认证文件（见 queueReauth）：这次的结果属于上一份文件，记下后按新的认证文件重新排队、立即开批
	requeue := item != nil && item.requeue && exists
	if item != nil {
		item.requeue = false
	}
	if item != nil {
		// 放回队列的下次重新开始验证（见 markStarted）
		item.started = false
	}
	if result == "" || result == onboardDeferred {
		// 服务停止时放回队列，暂缓的按 deferUntil 记下的时间再处理（不计重试次数）；账户已被删除时直接丢弃
		if item != nil && exists {
			onboarder.pending[accountID] = item
		}
		return ""
	}
	reauth := item != nil && !item.reauthAt.IsZero()
	if requeue && retry {
		// 出错的是上一份认证文件：不计这次的重试次数，按新的认证文件从头验证
		onboarder.pending[accountID] = &onboardPending{queuedAt: now.Add(-onboardingMaxWait), reauthAt: now}
		onboarder.scheduleSaveLocked()
		return ""
	}
	if retry && item != nil && item.attempts+1 < onboardingRetryLimit {
		item.attempts++
		item.notBefore = now.Add(onboardingRetryDelay)
		onboarder.pending[accountID] = item
		onboarder.admin.requests.log(accountID, "WARN", fmt.Sprintf(
			"%s出错，%s 后重试 | 第 %d 次 | 错误=%s", onboardLogPrefix(reauth), onboardingRetryDelay, item.attempts, detail,
		))
		return ""
	}
	switch {
	case requeue:
		// 这次的结果属于上一份认证文件、马上按新的重新验证：只写最近处理（与日志），不写处理记录。原先写了处理记录，重启后载入时
		// 因已有记录跳过了待验证的这一项，按新认证文件的验证丢失，面板一直显示上一份文件的结果
		delete(onboarder.records, accountID)
		onboarder.restored.Delete(accountID)
		event := api.OnboardingEvent{AccountID: accountID, Result: result, Detail: detail, At: now}
		if reauth && outcome != "" {
			event.Detail, event.Source, event.Outcome = outcomeDetail, onboardSourceReauth, outcome
			onboarder.logReauthOutcome(accountID, outcome, outcomeDetail, "上一份认证文件（已按新的重新排队）")
		}
		onboarder.pushRecentLocked(event)
	case reauth && outcome != "":
		// 重新上传后的结果：处理记录与最近处理都标为认证文件更新，写一行“重新上传后：…”
		onboarder.recordReauthLocked(accountID, result, outcomeDetail, outcome, now)
		onboarder.logReauthOutcome(accountID, outcome, outcomeDetail, "")
	default:
		onboarder.records[accountID] = onboardRecord{Result: result, Detail: detail, At: now}
		onboarder.pushRecentLocked(api.OnboardingEvent{AccountID: accountID, Result: result, Detail: detail, At: now})
	}
	// 停用原因：验证未通过或出错的账户保持停用，在管理页显示原因（已有的手动停用记录不被改为自动停用）。
	// 启用时已在账户租约内清除记录（见 updateAccount）；原先在这里再清除一次，启用后、这里之前刚被手动停用的账户
	// 记录会被抹掉。重新验证的写明是认证文件更新后的验证，不写“新账户”
	subject := "新账户自动处理"
	if reauth {
		subject = "认证文件更新后"
	}
	switch result {
	case onboardVerifyFailed:
		accountDisableNotes.set(accountID, subject+"验证未通过（"+detail+"），认证文件更新或重启后按默认启用", true)
	case onboardError:
		accountDisableNotes.set(accountID, subject+"验证出错（"+detail+"），认证文件更新或重启后按默认启用", true)
	}
	if requeue {
		onboarder.pending[accountID] = &onboardPending{queuedAt: now.Add(-onboardingMaxWait), reauthAt: now}
	}
	onboarder.scheduleSaveLocked()
	return result
}

// accountStatus 只查询这一个账户，而不是构建全部账户的状态再查找
func (onboarder *accountOnboarder) accountStatus(accountID string) (aistudio.AccountStatus, bool) {
	return onboarder.admin.pool.StatusOf(accountID)
}

// status 返回管理页显示的队列状态
func (onboarder *accountOnboarder) status() api.OnboardingStatus {
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	now := time.Now()
	result := api.OnboardingStatus{
		Policy:           onboarder.policy,
		Pending:          len(onboarder.pending),
		Processing:       make([]string, 0, len(onboarder.processing)),
		NextBatchSeconds: -1,
		Counts:           make(map[string]int),
		Recent:           append([]api.OnboardingEvent(nil), onboarder.recent...),
	}
	for id, item := range onboarder.processing {
		result.Processing = append(result.Processing, id)
		if !item.reauthAt.IsZero() {
			result.Reauth++
		}
	}
	for _, item := range onboarder.pending {
		if !item.reauthAt.IsZero() {
			result.Reauth++
		}
	}
	result.Unchanged, result.SameLogin, result.NotReuploaded = onboarder.unchanged, onboarder.sameLogin, onboarder.notReuploaded
	if onboarder.admin != nil {
		result.Loading = int(onboarder.admin.loginBacklog.Load())
	}
	sort.Strings(result.Processing)
	for _, record := range onboarder.records {
		// 重新上传后的记录只计入 ReauthOutcomes：原先同时计入处理结果，同一账户既算“验证通过”又算“重新上传后：已恢复”，
		// 2403 个重新上传后看起来像多了上千个新账户
		if record.Source == onboardSourceReauth && record.Outcome != "" {
			if result.ReauthOutcomes == nil {
				result.ReauthOutcomes = make(map[string]int)
			}
			result.ReauthOutcomes[record.Outcome]++
			continue
		}
		result.Counts[record.Result]++
	}
	if len(onboarder.pending) > 0 {
		eligible := 0
		var next time.Time
		for _, item := range onboarder.pending {
			ready := item.queuedAt.Add(onboardingMaxWait)
			if item.attempts > 0 {
				ready = item.notBefore
			}
			if next.IsZero() || ready.Before(next) {
				next = ready
			}
			if !now.Before(item.notBefore) {
				eligible++
			}
		}
		if eligible >= onboarder.policy.BatchSize || !now.Before(next) {
			result.NextBatchSeconds = 0
		} else {
			result.NextBatchSeconds = int(next.Sub(now).Round(time.Second) / time.Second)
		}
	}
	return result
}

// queueDisabled 把当前停用的账户加入队列（管理页手动添加的除外），返回加入数量。
// 用于处理功能开启前就已上传、被记为基线的停用账户；验证通过的会被启用。这是管理员的明确操作，确认框写明
// 手动停用的账户验证通过也会启用：记下加入时间（随处理记录保存，重启后继续有效），之前的手动停用不挡住启用，
// 之后才手动停用的仍保持停用
func (onboarder *accountOnboarder) queueDisabled() int {
	statuses := onboarder.admin.pool.Status()
	onboarder.mu.Lock()
	now := time.Now()
	accountDisableNotes.noteExplicitRequest(now)
	added := 0
	for _, status := range statuses {
		if status.Enabled {
			continue
		}
		if record, done := onboarder.records[status.ID]; done && record.Result == onboardManual {
			continue
		}
		if item, queued := onboarder.pending[status.ID]; queued {
			// 已在队列中的（例如重启后重新排队的）同样按管理员这次的要求处理
			item.explicitAt = now
			continue
		}
		if _, active := onboarder.processing[status.ID]; active {
			continue
		}
		delete(onboarder.records, status.ID)
		// 手动触发的立即开始，不等攒批
		onboarder.pending[status.ID] = &onboardPending{queuedAt: now.Add(-onboardingMaxWait), explicitAt: now}
		added++
	}
	onboarder.saveLocked()
	onboarder.mu.Unlock()
	if added > 0 {
		onboarder.admin.requests.log("auth", "INFO", fmt.Sprintf("新账户自动处理 | 手动加入停用账户 %d 个", added))
		onboarder.signal()
	}
	return added
}

// updatePolicy 保存新设置并立即生效
func (onboarder *accountOnboarder) updatePolicy(policy api.OnboardingPolicy) error {
	if err := validateOnboardingPolicy(policy); err != nil {
		return invalidAccount(err)
	}
	onboarder.mu.Lock()
	onboarder.policy = policy
	onboarder.saveLocked()
	onboarder.mu.Unlock()
	onboarder.admin.requests.log("auth", "INFO", fmt.Sprintf(
		"新账户自动处理设置已更新 | 自动验证=%t | 自动启用=%t | 每批=%d | 并发=%d",
		policy.AutoVerify, policy.AutoEnable, policy.BatchSize, policy.Concurrency,
	))
	onboarder.signal()
	return nil
}
