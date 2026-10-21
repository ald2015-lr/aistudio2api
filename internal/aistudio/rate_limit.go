package aistudio

import (
	"math"
	"sort"
	"strings"
	"time"
)

// 每账号每分钟限速（ACCOUNT_RPM_LIMIT）
//
// 上游对每个 Google 账号每分钟最多 12 次生成请求（所有模型、Playground 与 Build 两个通道共用，见 MinuteQuotaKind），
// 用满后整个账号冷却到窗口结束。这里在号池内按账号记最近一个窗口的发送时刻，提前避开：
//   - 只计 Method=="generateContent" 的选号（Playground、Build、Interactions、转写），Live 与 Veo 不拦不计，交给 429 兜底
//   - 取得生成租约时先“预占”一个名额（leaseCandidateLocked），真正发往上游时记一次发送（beginGenerationSend）并抵掉预占，
//     没有发出的租约在释放时自动退还（releaseLocked）；准入条件为 已发出 + 预占 < 上限，同账号的并发租约加起来也不会超出
//   - 满窗的账户分类时按忙碌处理（热账户进 WarmBusy、备用账户进 StandbyBusy），请求换到其他账户，热账户都满时由 app 层扩容；
//     满窗不写冷却表，不影响全部冷却、退休、淘汰与对外状态
//   - 文件绑定的选号不拦（所属账户不可用时要走复制文件的路径），但照常预占与记账；上限为 0 时同样只记账不拦
//   - 窗口按时间释放时由号池唯一的计时器（rateWake，指向满窗账户中最早的释放时刻）notifyLocked，经 app 层 forwardPoolChanges
//     及时唤醒各队首，与租约释放一样按到达序号排队；窗口释放时刻同时并入 AcquireFor 的 earliest 定时复查。只因预占而满时等租约
//     发出或释放：释放时 releaseLocked 唤醒；最后一个预占经发送转成有限的截止时刻时 rateChangedLocked 唤醒，等待方改按新的截止时刻定时
//   - 只统计本进程，记录只在内存中，重启或重建号池后从空窗口开始，429 冷却仍然兜底
//
// 全部状态受账户池 mu 保护
const (
	// accountRateWindow 为滑动窗口
	accountRateWindow = time.Minute
	// accountRateMaxSent 为每个账户最多保留的发送时刻（不小于配置上限 1000，防止关闭限速期间无限增长）
	accountRateMaxSent = 1024
	// accountRateNoteInterval 为同一账户“每分钟请求已满”日志的最短间隔
	accountRateNoteInterval = 10 * time.Minute
	// rateUntilPending 表示只因进行中的预占而满：要等租约发出或释放，不随时间恢复
	rateUntilPending = int64(math.MaxInt64)
)

// accountRate 为账户最近一个窗口的生成请求记录，受账户池 mu 保护；第一次取得生成租约或记发送时创建，只在内存中
type accountRate struct {
	// sent 为发出时刻（UnixNano，升序、不减）
	sent []int64
	// pending 为已取得生成租约、还没发出的预占数
	pending int
	// until 为按当前上限算出的窗口已满截止时刻（缓存）：0 为未满，rateUntilPending 见上
	until int64
	// notedAt 为上次写“已满”日志的时间（只由 ClaimRateWindowNote 写入）
	notedAt time.Time
}

// rateGate 为一次分类或选号内不变的限速判断
type rateGate struct {
	enabled bool
	now     int64
}

// full 判断按缓存的截止时刻账户当前是否满窗
func (g rateGate) full(until int64) bool {
	return g.enabled && until > g.now
}

// generationSelection 判断选择会发出生成请求并计入每分钟上限
func generationSelection(selection AccountSelection) bool {
	return strings.TrimSpace(selection.Method) == "generateContent"
}

// rateGateLocked 返回选择的限速判断：上限大于 0、是生成选择、且不是文件绑定时拦截；调用方持有 p.mu
func (p *AccountPool) rateGateLocked(selection AccountSelection, now time.Time) rateGate {
	return rateGate{
		enabled: p.accountRateLimit > 0 && generationSelection(selection) && selection.ResourceID == "",
		now:     now.UnixNano(),
	}
}

// rateWindowLocked 返回滑动窗口长度：rateWindow 只给测试改小（包内直接改，其他包经 SetRateWindowForTest），为 0 时取 accountRateWindow；调用方持有 p.mu
func (p *AccountPool) rateWindowLocked() time.Duration {
	if p.rateWindow > 0 {
		return p.rateWindow
	}
	return accountRateWindow
}

// rateUntilOf 返回账户缓存的窗口已满截止时刻；没有记录时为 0。调用方持有 p.mu
func rateUntilOf(account *Account) int64 {
	if account == nil || account.rate == nil {
		return 0
	}
	return account.rate.until
}

// computeRateUntilLocked 按当前上限计算窗口已满的截止时刻，不修剪记录（核对索引时也用它）：
// 窗口内已发出数加预占数不少于上限时，第 excess+1 条发送过期后才降到上限以下；预占本身不少于上限时为 rateUntilPending。
// 没有新事件时，随时间修剪只移动窗口起点，不改变第 excess+1 条的时刻，缓存值一直有效。调用方持有 p.mu
func (p *AccountPool) computeRateUntilLocked(rate *accountRate, nowNanos int64) int64 {
	limit := p.accountRateLimit
	if limit <= 0 || rate == nil {
		return 0
	}
	window := int64(p.rateWindowLocked())
	start := sort.Search(len(rate.sent), func(i int) bool { return rate.sent[i] > nowNanos-window })
	live := len(rate.sent) - start
	excess := live + rate.pending - limit
	switch {
	case excess < 0:
		return 0
	case excess < live:
		return rate.sent[start+excess] + window
	default:
		return rateUntilPending
	}
}

// rateChangedLocked 为修改 until 的唯一入口：修剪窗口外的记录，重算截止时刻，写回候选索引；截止时刻为有限值时登记到窗口释放计时器。
// 只因预占而满（rateUntilPending）转成有限的截止时刻时（最后一个预占经发送抵掉，上限小于单账户并发或热改小上限时会出现）唤醒等待方：
// 这时 AcquireFor 的等待方没有定时（earliest 为空），只等调度信号。只在这个转换上唤醒，普通发送不唤醒。调用方持有 p.mu
func (p *AccountPool) rateChangedLocked(account *Account, now time.Time) {
	rate := account.rate
	if rate == nil {
		return
	}
	nowNanos := now.UnixNano()
	cutoff := nowNanos - int64(p.rateWindowLocked())
	if start := sort.Search(len(rate.sent), func(i int) bool { return rate.sent[i] > cutoff }); start > 0 {
		rate.sent = rate.sent[:copy(rate.sent, rate.sent[start:])]
	}
	old := rate.until
	rate.until = p.computeRateUntilLocked(rate, nowNanos)
	if p.sched != nil && p.sched.positionValid(p, account) {
		p.sched.rateUntil[account.position] = rate.until
	}
	if rate.until > nowNanos && rate.until != rateUntilPending {
		p.armRateWakeLocked(account, rate.until, nowNanos)
	}
	if old == rateUntilPending && rate.until != rateUntilPending {
		p.notifyLocked()
	}
}

// armRateWakeLocked 把截止时刻为有限值的满窗账户登记到 rateFull，截止时刻早于计时器当前的触发时刻（或没有设）时把计时器提前到它；
// 调用方持有 p.mu
func (p *AccountPool) armRateWakeLocked(account *Account, until int64, nowNanos int64) {
	if p.rateFull == nil {
		p.rateFull = make(map[*Account]struct{})
	}
	p.rateFull[account] = struct{}{}
	if p.rateWakeAt != 0 && p.rateWakeAt <= until {
		return
	}
	p.rateWakeAt = until
	delay := time.Duration(until - nowNanos)
	if p.rateWake == nil {
		p.rateWake = time.AfterFunc(delay, p.rateWindowReleased)
		return
	}
	p.rateWake.Reset(delay)
}

// rateWindowReleased 为窗口释放计时器的回调：有账户的窗口已经释放时唤醒等待方（app 层经 forwardPoolChanges 唤醒各队首，
// 先排队的请求先取得名额），再按 rateFull 里剩下的满窗账户中最早的截止时刻重设计时器。没有新事件时截止时刻不变，扫描结果准确；
// 已移除、已不满或改为只因预占而满的账户移出 rateFull。号池没有关闭操作，丢弃的号池最多再按剩下的截止时刻触发到窗口结束，
// 回调只操作自己的号池，没有影响
func (p *AccountPool) rateWindowReleased() {
	p.mu.Lock()
	defer p.mu.Unlock()
	nowNanos := time.Now().UnixNano()
	p.rateWakeAt = 0
	released := false
	next := int64(0)
	for account := range p.rateFull {
		until := rateUntilOf(account)
		if p.byID[account.ID] != account || until <= nowNanos || until == rateUntilPending {
			released = released || until <= nowNanos
			delete(p.rateFull, account)
			continue
		}
		if next == 0 || until < next {
			next = until
		}
	}
	if next != 0 {
		p.rateWakeAt = next
		p.rateWake.Reset(time.Duration(next - nowNanos))
	}
	if released {
		p.notifyLocked()
	}
}

// reserveRateLocked 为刚取得的生成租约预占一个名额；预占后账户满窗时标记这个租约占满了窗口（见 ClaimRateWindowNote，
// 是否写日志由写日志的一侧判断）。调用方持有 p.mu
func (p *AccountPool) reserveRateLocked(account *Account, lease *AccountLease, now time.Time) {
	if account.rate == nil {
		account.rate = &accountRate{}
	}
	account.rate.pending++
	lease.ratePending = true
	p.rateChangedLocked(account, now)
	if p.accountRateLimit > 0 && account.rate.until > now.UnixNano() {
		lease.rateFilled = true
	}
}

// recordGenerationSentLocked 记一次发送：抵掉本租约的预占（只抵一次），追加不减的发送时刻；调用方持有 p.mu
func (p *AccountPool) recordGenerationSentLocked(lease *AccountLease, now time.Time) {
	account := lease.account
	if account.rate == nil {
		account.rate = &accountRate{}
	}
	rate := account.rate
	if lease.ratePending {
		lease.ratePending = false
		if rate.pending > 0 {
			rate.pending--
		}
	}
	stamp := now.UnixNano()
	if n := len(rate.sent); n > 0 && rate.sent[n-1] > stamp {
		// 墙钟回拨时保持有序，最多多算
		stamp = rate.sent[n-1]
	}
	rate.sent = append(rate.sent, stamp)
	if extra := len(rate.sent) - accountRateMaxSent; extra > 0 {
		rate.sent = rate.sent[:copy(rate.sent, rate.sent[extra:])]
	}
	p.rateChangedLocked(account, now)
}

// releaseRateLocked 在 releaseLocked 里退还没发出的预占（调用方持有 l.operation 与 p.mu）
func (l *AccountLease) releaseRateLocked() {
	if !l.ratePending {
		return
	}
	l.ratePending = false
	if rate := l.account.rate; rate != nil && rate.pending > 0 {
		rate.pending--
		l.pool.rateChangedLocked(l.account, time.Now())
	}
}

// beginGenerationSend 在生成请求发往上游前一次加锁完成两件事：租约账户已进入冷却时（多半是同账号别的请求刚遇到额度 429）
// 返回 *AccountCoolingDownError、不记发送；否则记一次发送（抵掉本租约的预占）。冷却按租约所在通道与全局判断（CooldownScope）。
// 账户已被移除时不拦不记；账户被替换时只看新账户的冷却，不给新账户记账
func (l *AccountLease) beginGenerationSend(modelID string) error {
	if l == nil || l.pool == nil || l.account == nil {
		return nil
	}
	scope := l.CooldownScope(strings.TrimPrefix(strings.TrimSpace(modelID), "models/"))
	now := time.Now()
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.byID[l.account.ID]
	if current == nil {
		return nil
	}
	if cooldown, active := accountCooldown(current, scope, now); active {
		return &AccountCoolingDownError{AccountID: current.ID, ModelID: modelID, Until: cooldown.Until, Reason: cooldown.Reason}
	}
	if current == l.account {
		p.recordGenerationSentLocked(l, now)
	}
	return nil
}

// RecordGenerationSent 为租约记一次发往上游的生成请求（抵掉本租约的预占）。供不经 WorkerProtectedTransport 发送的调用方
// 与测试替身使用；账户已不在池中时不记
func (l *AccountLease) RecordGenerationSent() {
	if l == nil || l.pool == nil || l.account == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID[l.account.ID] == l.account {
		p.recordGenerationSentLocked(l, time.Now())
	}
}

// ClaimRateWindowNote 报告取得这个租约时账户的每分钟窗口被它占满、且现在该写一条“账号每分钟请求已满”日志（同一账户 10 分钟最多一次），
// 返回真时在账户上记下这次日志的时间。只由真正写日志的调用方调用：转写、启动失败后释放的租约等不写日志的路径占满窗口时不占用这
// 10 分钟的名额。rateFilled 只在取得租约时写入；notedAt 在 p.mu 下判断与写入。账户已不在池中时返回假
func (l *AccountLease) ClaimRateWindowNote() bool {
	if l == nil || !l.rateFilled || l.pool == nil || l.account == nil {
		return false
	}
	p := l.pool
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	rate := l.account.rate
	if p.byID[l.account.ID] != l.account || rate == nil || now.Sub(rate.notedAt) < accountRateNoteInterval {
		return false
	}
	rate.notedAt = now
	return true
}

// SetAccountRateLimit 运行中调整单账户每分钟生成请求上限（0 表示关闭，负数按 0），按新上限重算各账户的满窗截止时刻，
// 并唤醒等待的请求（调大或关闭后可能立即可用）
func (p *AccountPool) SetAccountRateLimit(limit int) {
	limit = max(limit, 0)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accountRateLimit == limit {
		return
	}
	p.accountRateLimit = limit
	now := time.Now()
	for _, account := range p.accounts {
		if account != nil && account.rate != nil {
			p.rateChangedLocked(account, now)
		}
	}
	p.notifyLocked()
}

// SetRateWindowForTest 把每分钟限速的滑动窗口改为 window（不大于 0 时恢复为 60 秒），只供其他包的测试缩短窗口，
// 按新窗口重算各账户的满窗截止时刻
func (p *AccountPool) SetRateWindowForTest(window time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rateWindow = max(window, 0)
	now := time.Now()
	for _, account := range p.accounts {
		if account != nil && account.rate != nil {
			p.rateChangedLocked(account, now)
		}
	}
}

// AccountRateLimit 返回当前的单账户每分钟生成请求上限（0 表示关闭）
func (p *AccountPool) AccountRateLimit() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accountRateLimit
}
