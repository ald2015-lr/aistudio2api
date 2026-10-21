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
)

type onboardRecord struct {
	Result string    `json:"result"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

type onboardFile struct {
	Version  int                      `json:"version"`
	Policy   api.OnboardingPolicy     `json:"policy"`
	Accounts map[string]onboardRecord `json:"accounts"`
	// Explicit 为“处理现有停用账户”加入、尚未处理完的账户及加入时间（见 onboardPending.explicitAt）
	Explicit map[string]time.Time `json:"explicit,omitempty"`
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
	var explicit map[string]time.Time
	if content, err := os.ReadFile(onboarder.path); err == nil {
		var file onboardFile
		if err := json.Unmarshal(content, &file); err == nil {
			existing = true
			explicit = file.Explicit
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
	queued, resumed := 0, 0
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
		// “处理现有停用账户”加入、重启前没有处理完的账户仍按管理员当时的要求处理
		explicitAt := explicit[status.ID]
		onboarder.pending[status.ID] = &onboardPending{queuedAt: now, explicitAt: explicitAt}
		queued++
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
		if resumed > 0 {
			message = fmt.Sprintf(
				"新账户自动处理 | 重新加入队列 %d 个账户，其中 %d 个是“处理现有停用账户”加入、尚未处理完的，仍按当时的要求处理", queued, resumed,
			)
		}
		onboarder.admin.requests.log("auth", "INFO", message)
	}
	onboarder.saveLocked()
}

// saveLocked 原子写入处理记录；调用方持有 mu。等待中的合并保存随之完成
func (onboarder *accountOnboarder) saveLocked() {
	onboarder.savePending = false
	explicit := make(map[string]time.Time)
	for _, items := range []map[string]*onboardPending{onboarder.pending, onboarder.processing} {
		for id, item := range items {
			if !item.explicitAt.IsZero() {
				explicit[id] = item.explicitAt
			}
		}
	}
	content, err := json.MarshalIndent(onboardFile{
		Version: 1, Policy: onboarder.policy, Accounts: onboarder.records, Explicit: explicit,
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
	onboarder.admin.requests.log("auth", "INFO", fmt.Sprintf(
		"新账户自动处理开始 | 本批=%d | 并发=%d | 自动验证=%t | 自动启用=%t",
		len(batch), policy.Concurrency, policy.AutoVerify, policy.AutoEnable,
	))
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
	onboarder.admin.requests.log("auth", "INFO", fmt.Sprintf(
		"新账户自动处理完成 | 已启用=%d | 验证通过=%d | 验证未通过=%d | 出错=%d | 耗时=%s",
		counts[onboardEnabled], counts[onboardVerified], counts[onboardVerifyFailed], counts[onboardError],
		time.Since(startedAt).Round(time.Second),
	))
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
	if !status.Enabled && accountDisableNotes.blocksAutoEnable(accountID, explicitAt) {
		onboarder.admin.requests.log(label, "INFO", "新账户自动处理 | "+onboardManuallyDisabled)
		return onboardSkipped, onboardManuallyDisabled, false
	}
	initiallyEnabled := status.Enabled
	verified := false
	if policy.AutoVerify {
		verification, err := onboarder.admin.verifyLogin(ctx, accountID)
		if err != nil {
			return onboardError, strings.TrimSpace(err.Error()), true
		}
		if status, ok = onboarder.accountStatus(accountID); !ok {
			return "", "", false
		}
		if failed, reason := onboardVerifyFailure(verification, status); failed {
			outcome := "验证未通过，保持停用"
			if status.Enabled {
				outcome = "验证未通过，账户需要重新登录"
			}
			onboarder.admin.requests.log(label, "WARN", "新账户自动处理 | "+outcome+" | 原因="+reason)
			return onboardVerifyFailed, reason, false
		}
		verified = true
	}
	if policy.AutoEnable && !status.Enabled {
		if initiallyEnabled {
			onboarder.admin.requests.log(label, "INFO", "新账户自动处理 | 处理期间被停用，"+onboardManuallyDisabled)
			return onboardSkipped, onboardManuallyDisabled, false
		}
		// 启用后还没有模型目录时（未经验证直接启用、验证时目录同步失败），updateAccount 会在后台补一次同步
		dto := adminAccountDTO(status)
		_, err := onboarder.admin.updateAccount(ctx, accountID, api.AccountInput{
			Label: dto.Label, Enabled: true, Proxy: dto.Proxy, Locale: dto.Locale, Timezone: dto.Timezone,
		}, &automaticEnable{explicitAt: explicitAt})
		if errors.Is(err, errAccountManuallyDisabled) {
			onboarder.admin.requests.log(label, "INFO", "新账户自动处理 | "+onboardManuallyDisabled)
			return onboardSkipped, onboardManuallyDisabled, false
		}
		if err != nil {
			return onboardError, strings.TrimSpace(err.Error()), true
		}
		onboarder.admin.requests.log(label, "INFO", "新账户自动处理 | 已启用")
		return onboardEnabled, "", false
	}
	if verified {
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

// finish 记录处理结果；出错且未超过重试次数时稍后重试。返回最终结果（仍在重试时为空）
func (onboarder *accountOnboarder) finish(accountID string, result string, detail string, retry bool) string {
	// 先在锁外查询账户是否仍存在，避免持有本锁时再去拿账户池锁
	exists := true
	if result == "" {
		_, exists = onboarder.accountStatus(accountID)
	}
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	item := onboarder.processing[accountID]
	delete(onboarder.processing, accountID)
	if result == "" {
		// 服务停止时放回队列；账户已被删除时直接丢弃
		if item != nil && exists {
			onboarder.pending[accountID] = item
		}
		return ""
	}
	now := time.Now()
	if retry && item != nil && item.attempts+1 < onboardingRetryLimit {
		item.attempts++
		item.notBefore = now.Add(onboardingRetryDelay)
		onboarder.pending[accountID] = item
		onboarder.admin.requests.log(accountID, "WARN", fmt.Sprintf(
			"新账户自动处理出错，%s 后重试 | 第 %d 次 | 错误=%s", onboardingRetryDelay, item.attempts, detail,
		))
		return ""
	}
	onboarder.records[accountID] = onboardRecord{Result: result, Detail: detail, At: now}
	// 停用原因：验证未通过或出错的账户保持停用，在管理页显示原因（已有的手动停用记录不被改为自动停用）。
	// 启用时已在账户租约内清除记录（见 updateAccount）；原先在这里再清除一次，启用后、这里之前刚被手动停用的账户
	// 记录会被抹掉
	switch result {
	case onboardVerifyFailed:
		accountDisableNotes.set(accountID, "新账户自动处理验证未通过（"+detail+"），认证文件更新或重启后按默认启用", true)
	case onboardError:
		accountDisableNotes.set(accountID, "新账户自动处理验证出错（"+detail+"），认证文件更新或重启后按默认启用", true)
	}
	onboarder.recent = append([]api.OnboardingEvent{{
		AccountID: accountID, Result: result, Detail: detail, At: now,
	}}, onboarder.recent...)
	if len(onboarder.recent) > onboardingRecentLimit {
		onboarder.recent = onboarder.recent[:onboardingRecentLimit]
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
	for id := range onboarder.processing {
		result.Processing = append(result.Processing, id)
	}
	sort.Strings(result.Processing)
	for _, record := range onboarder.records {
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
