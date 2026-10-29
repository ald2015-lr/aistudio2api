package aistudio

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// driveUnavailableTTL 为“Drive 不可用”记忆的有效期：期间这个账号的内联附件直接内联发送，不再先换 Drive token，Interaction 模型
// （gemini-omni）的视频输入（及无法转为 PNG 的图片）也不选它。账号获得 Drive 授权只发生在重新登录（推进认证代际，记录随之失效）或人工操作之后
// （管理验证通过时清除，见 ForgetDriveUnavailable），所以记一天：原先 6 小时，没有 Drive 授权的账号每 6 小时就要被用户请求重新试一遍
const driveUnavailableTTL = 24 * time.Hour

// driveAvailableTTL 为“Drive 可用”记忆的有效期：期间 Interaction 模型（gemini-omni）的视频输入（及无法转为 PNG 的图片）优先交给这个账号。
// 只影响选号先后，记错的代价是一次换号（发送前发现附件仍是 inline data 时换号，见 app 层）
const driveAvailableTTL = 24 * time.Hour

// driveRecordRefresh 为同一账号同一状态再次通知写盘（见 SetDriveObserver）的最短间隔：状态不变时每次换 token 都只在内存续期，
// 写盘的记下时间最多晚这么久，重启后的剩余有效期最多少这么久
const driveRecordRefresh = time.Hour

// DriveState 为账号的 Drive 授权状态（进程内记忆，见 driveMemory）：换到过 Drive token 为可用，GenerateAccessToken 返回 401 / Code 16
// 为不可用，其余（没试过、认证代际变了、记忆过期）为未知
type DriveState string

const (
	DriveStateUnknown     DriveState = "unknown"
	DriveStateAvailable   DriveState = "available"
	DriveStateUnavailable DriveState = "unavailable"
)

// DriveRecord 为一个账号的 Drive 授权状态记录，供写盘与重启后恢复（见 SetDriveObserver、RestoreDriveRecords 与 app 层 drive_state.go）。
// State 为 DriveStateUnknown 时表示清除这个账号的记录（管理验证通过后重新探测）
type DriveRecord struct {
	AccountID string
	State     DriveState
	// CheckedAt 为记下状态的时间（换 token 的结果），恢复时按它加有效期计算到期
	CheckedAt time.Time
	// Identity 为记下时账户认证文件的登录签名（LoginIdentity）：重启后认证文件已换成另一个登录时记录作废。
	// 只认登录签名 Cookie，Worker 启动后合并写回的普通 Cookie 不影响
	Identity string
}

// driveMemoryPruneMin 为记忆条目达到多少时才清理过期与失效的条目（之后按清理后条目数的两倍再清，摊销到每次写入是常数）
const driveMemoryPruneMin = 1024

// driveUnavailableEntry 为一个账号的 Drive 状态记录（“Drive 不可用”与“Drive 可用”共用这个结构）
type driveUnavailableEntry struct {
	// account 为记下时的账户对象：账户删除后以同一 ID 重新添加是另一个对象（认证代际从 0 重新计），不沿用旧记录
	account *Account
	// generation 为发出那次 GenerateAccessToken 之前账户的认证代际：重新登录、续签、更新认证文件推进代际后记录失效
	generation uint64
	until      time.Time
	// checkedAt 为最近一次记下的时间，notifiedAt 为最近一次通知写盘的时间（见 driveRecordRefresh）
	checkedAt  time.Time
	notifiedAt time.Time
}

// driveMemory 按账号记住 GenerateAccessToken 返回 401 / 协议 Code 16（账号没有授权 Google Drive，导入的账号多半没走过官网的 Drive 同意），
// 以及换到过 Drive token（Drive 可用）。进程内的记忆，app 层经 SetDriveObserver 写盘、装配运行时经 RestoreDriveRecords 恢复。
// 用自己的小锁，不在持有时取账户池的 mu（认证代际在取这把锁之前读好），不与账户池主锁形成新的锁顺序。
// 同一账号只在一边：记下一边时删掉另一边。零值可用
type driveMemory struct {
	mu sync.Mutex
	// entries 为记着 Drive 不可用的账号，available 为记着 Drive 可用的账号
	entries   map[string]driveUnavailableEntry
	available map[string]driveUnavailableEntry
	pruneAt   int
	// availablePruneAt 为 available 下一次清理的门槛（与 pruneAt 相同的摊销规则）
	availablePruneAt int
	// version 在账号的 Drive 状态变化（新记下、换到另一边、清除）时加一，DriveCandidates 的短时缓存据此失效；同一状态的续期不变
	version uint64
}

// driveSnapshot 为判断 Drive 状态用的账户对象与认证代际（在账户池 mu 下读好）
type driveSnapshot struct {
	account    *Account
	generation uint64
}

// validLocked 判断 entries 里账号的记录对快照有效：账户对象、认证代际都相同且没有过期。只删确实失效的记录：已过期，或同一账户对象、
// 记下时的代际比快照旧。记录比快照新（读快照之后账号重新登录、另一个请求按新代际记下）或账户对象不同（账户删除后同 ID 重新添加，
// 分不出哪个是现在的账户）时只当作不命中、不删，免得持旧快照的一方删掉更新的记录。调用方持有 memory.mu
func validLocked(entries map[string]driveUnavailableEntry, snapshot driveSnapshot, now time.Time) bool {
	account := snapshot.account
	entry, exists := entries[account.ID]
	if !exists {
		return false
	}
	if entry.account == account && entry.generation == snapshot.generation && now.Before(entry.until) {
		return true
	}
	if !now.Before(entry.until) || entry.account == account && entry.generation < snapshot.generation {
		delete(entries, account.ID)
	}
	return false
}

// unavailable 判断账号此刻是否记着 Drive 不可用（规则见 validLocked）
func (memory *driveMemory) unavailable(account *Account, generation uint64, now time.Time) bool {
	if account == nil {
		return false
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	return validLocked(memory.entries, driveSnapshot{account: account, generation: generation}, now)
}

// remember 记下账号 Drive 不可用（generation 为发出 GenerateAccessToken 之前读取的认证代际），有效期 driveUnavailableTTL；
// 同时删掉它的“Drive 可用”记录。返回是否要通知写盘（见 set）
func (memory *driveMemory) remember(account *Account, generation uint64, now time.Time) bool {
	return memory.set(account, generation, DriveStateUnavailable, now, now)
}

// rememberAvailable 记下账号 Drive 可用（换到了 Drive token；generation 为发出前读取的认证代际），有效期 driveAvailableTTL；
// 同时删掉它的“Drive 不可用”记录。返回是否要通知写盘（见 set）
func (memory *driveMemory) rememberAvailable(account *Account, generation uint64, now time.Time) bool {
	return memory.set(account, generation, DriveStateAvailable, now, now)
}

// set 记下账号的 Drive 状态，checkedAt 为记下的时间（恢复写盘的记录时早于 now），有效期按状态取。同一账户对象已有更新代际的记录
// （另一个请求在重新登录后记下的）时不写：这次结果来自旧的登录。返回是否要通知写盘：状态变了，或距上次通知超过 driveRecordRefresh
func (memory *driveMemory) set(account *Account, generation uint64, state DriveState, checkedAt time.Time, now time.Time) bool {
	if account == nil || state == DriveStateUnknown {
		return false
	}
	ttl, target, other := driveUnavailableTTL, &memory.entries, &memory.available
	pruneAt := &memory.pruneAt
	if state == DriveStateAvailable {
		ttl, target, other = driveAvailableTTL, &memory.available, &memory.entries
		pruneAt = &memory.availablePruneAt
	}
	if !now.Before(checkedAt.Add(ttl)) {
		return false
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	for _, entries := range []map[string]driveUnavailableEntry{memory.entries, memory.available} {
		if entry, exists := entries[account.ID]; exists && entry.account == account && entry.generation > generation && now.Before(entry.until) {
			return false
		}
	}
	if *target == nil {
		*target = make(map[string]driveUnavailableEntry)
	}
	pruneDriveEntries(*target, pruneAt, now)
	previous, same := (*target)[account.ID]
	same = same && previous.account == account && previous.generation == generation && now.Before(previous.until)
	if _, switched := (*other)[account.ID]; switched {
		delete(*other, account.ID)
	}
	entry := driveUnavailableEntry{account: account, generation: generation, until: checkedAt.Add(ttl), checkedAt: checkedAt, notifiedAt: now}
	notify := true
	if same {
		notify = !now.Before(previous.notifiedAt.Add(driveRecordRefresh))
		if !notify {
			entry.notifiedAt = previous.notifiedAt
		}
	} else {
		memory.version++
	}
	(*target)[account.ID] = entry
	return notify
}

// pruneDriveEntries 在条目数达到门槛时清理过期条目，门槛改为清理后条目数的两倍（不少于 driveMemoryPruneMin）；调用方持有 memory.mu
func pruneDriveEntries(entries map[string]driveUnavailableEntry, pruneAt *int, now time.Time) {
	if len(entries) < max(*pruneAt, driveMemoryPruneMin) {
		return
	}
	for accountID, entry := range entries {
		if !now.Before(entry.until) {
			delete(entries, accountID)
		}
	}
	*pruneAt = 2 * len(entries)
}

// forget 清除账号的“Drive 不可用”记录（管理验证通过后重新探测）；“Drive 可用”记录不动。返回是否清除了记录
func (memory *driveMemory) forget(accountID string) bool {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if _, exists := memory.entries[accountID]; !exists {
		return false
	}
	delete(memory.entries, accountID)
	memory.version++
	return true
}

// currentVersion 返回状态版本（见 driveMemory.version）
func (memory *driveMemory) currentVersion() uint64 {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	return memory.version
}

// stateLocked 返回账号此刻的 Drive 状态（规则见 validLocked）。调用方持有 memory.mu
func (memory *driveMemory) stateLocked(snapshot driveSnapshot, now time.Time) DriveState {
	if snapshot.account == nil {
		return DriveStateUnknown
	}
	switch {
	case validLocked(memory.entries, snapshot, now):
		return DriveStateUnavailable
	case validLocked(memory.available, snapshot, now):
		return DriveStateAvailable
	}
	return DriveStateUnknown
}

// states 一次加锁返回一批账号的 Drive 状态，与 snapshots 一一对应
func (memory *driveMemory) states(snapshots []driveSnapshot, now time.Time) []DriveState {
	result := make([]DriveState, len(snapshots))
	memory.mu.Lock()
	defer memory.mu.Unlock()
	for index, snapshot := range snapshots {
		result[index] = memory.stateLocked(snapshot, now)
	}
	return result
}

// count 返回“Drive 不可用”记忆中的条目数（含尚未清理的过期条目），只供测试
func (memory *driveMemory) count() int {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	return len(memory.entries)
}

// driveUnavailableError 判断 GenerateAccessToken 的错误是否说明这个账号的 Drive 不可用：401 / 协议 Code 16，
// 含明确的 unauthorized_client 与其他 401（线上原文 “Request is missing required authentication credential”）。
// 这些账号用 Cookie 认证的 RPC 与生成都正常，401 不是 Cookie 过期，而是没有 Drive 授权（登录流程里 camoufoxnative 的 authorizeDrive
// 同样把 GenerateAccessToken 的任何 401 当作 Drive 未授权）。登录失效（ErrAuthenticationRequired：签名 Cookie 缺失、登录页跳转）不算
func driveUnavailableError(err error) bool {
	if err == nil || errors.Is(err, ErrAuthenticationRequired) {
		return false
	}
	return AccessTokenAuthFailure(err) || driveAuthorizationMissing(err)
}

// driveAuthGeneration 返回租约账户当前的认证代际，在发出 GenerateAccessToken 之前读取（只取账户池 mu，与 driveMemory 的锁不嵌套）；
// 账户已不在池中时 ok 为假，这次结果不记
func (s *PooledService) driveAuthGeneration(lease *AccountLease) (uint64, bool) {
	if s == nil || s.pool == nil || lease == nil || lease.account == nil {
		return 0, false
	}
	p := s.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID[lease.account.ID] != lease.account {
		return 0, false
	}
	return lease.account.authGeneration, true
}

// driveKnownUnavailable 判断租约账户是否记着 Drive 不可用（认证代际变化或过期后不算）
func (s *PooledService) driveKnownUnavailable(lease *AccountLease) bool {
	generation, current := s.driveAuthGeneration(lease)
	return current && s.drive.unavailable(lease.account, generation, time.Now())
}

// noteAccessToken 按一次 GenerateAccessToken 的结果更新记忆：取得 token 时记为 Drive 可用（账户已不在池中时只清除不可用记录）；
// 返回 401 / Code 16（driveUnavailableError）时按发出前读取的认证代际记下不可用。其他错误（网络、5xx、登录失效）不改记忆。
// 状态变化（或同一状态距上次通知超过 driveRecordRefresh）时通知写盘（见 SetDriveObserver）
func (s *PooledService) noteAccessToken(lease *AccountLease, generation uint64, current bool, tokenOK bool, err error) {
	if s == nil || lease == nil || lease.account == nil {
		return
	}
	now := time.Now()
	switch {
	case tokenOK && current:
		if s.drive.rememberAvailable(lease.account, generation, now) {
			s.notifyDrive(lease.account, generation, DriveStateAvailable, now)
		}
	case tokenOK:
		if s.drive.forget(lease.account.ID) {
			s.notifyDrive(lease.account, generation, DriveStateUnknown, now)
		}
	case current && driveUnavailableError(err):
		if s.drive.remember(lease.account, generation, now) {
			s.notifyDrive(lease.account, generation, DriveStateUnavailable, now)
		}
	}
}

// accessTokenForLease 为租约账户换一次 Drive bearer token，并按结果更新“Drive 不可用”记忆
func (s *PooledService) accessTokenForLease(ctx context.Context, lease *AccountLease) (string, error) {
	generation, current := s.driveAuthGeneration(lease)
	token, err := s.client.GenerateAccessToken(ContextWithAccountLease(ctx, lease), lease.Account().ID)
	s.noteAccessToken(lease, generation, current, err == nil, err)
	return token, err
}

// uploadFileForLease 用租约账户上传 Drive 文件（request.AccountID 须为租约账户），并按其中 GenerateAccessToken 的结果更新记忆：
// 返回了 token 即换取成功（之后的 Drive 上传失败与 Drive 授权无关）
func (s *PooledService) uploadFileForLease(ctx context.Context, lease *AccountLease, request UploadRequest) (FileRef, string, error) {
	generation, current := s.driveAuthGeneration(lease)
	file, token, err := s.client.uploadFile(ContextWithAccountLease(ctx, lease), request)
	s.noteAccessToken(lease, generation, current, token != "", err)
	return file, token, err
}

// NoteDriveAuthorized 记下账号 Drive 可用：经本服务的登录流程登录、官网 Drive 授权完成（登录页面的 GenerateAccessToken 返回 200，
// camoufoxnative 的 authorizeDrive 没有报错）并保存了新的认证文件之后调用（app 层 LoginAccount、CreateAccount）。按账户当前的认证代际
// 记下（与换到 Drive token 时相同，写盘），gemini-omni 的视频输入（及无法转为 PNG 的图片）随即优先交给它；原先重新登录后状态回到未知，记着可用的账号
// 有空时不会选它，只能等用户请求偶然试到。账户不在池中时不记
func (s *PooledService) NoteDriveAuthorized(accountID string) {
	if s == nil || s.pool == nil {
		return
	}
	p := s.pool
	p.mu.Lock()
	account := p.byID[accountID]
	var generation uint64
	if account != nil {
		generation = account.authGeneration
	}
	p.mu.Unlock()
	if account == nil {
		return
	}
	now := time.Now()
	if s.drive.rememberAvailable(account, generation, now) {
		s.notifyDrive(account, generation, DriveStateAvailable, now)
	}
}

// ForgetDriveUnavailable 清除账号的“Drive 不可用”记忆：管理验证通过等人工恢复路径调用，下一次内联附件重新探测 Drive 授权
// （重新登录与更新认证文件推进认证代际，记录本来就会失效）；写盘的记录同时清除
func (s *PooledService) ForgetDriveUnavailable(accountID string) {
	if s == nil {
		return
	}
	if s.drive.forget(accountID) {
		s.notifyDrive(&Account{ID: accountID}, 0, DriveStateUnknown, time.Now())
	}
}

// SetDriveObserver 设置 Drive 状态写盘的回调（app 层 drive_state.go）：记下状态变化、同一状态距上次通知超过 driveRecordRefresh、
// 清除记录时调用，State 为 DriveStateUnknown 表示清除。回调在记忆与账户池的锁外调用，不能阻塞；记下时账号已重新登录
// （认证代际变了）或已不在池中时不调用。observer 为 nil 时取消
func (s *PooledService) SetDriveObserver(observer func(DriveRecord)) {
	if s == nil {
		return
	}
	if observer == nil {
		s.driveObserver.Store(nil)
		return
	}
	s.driveObserver.Store(&observer)
}

// notifyDrive 把一次记下的状态交给写盘回调：可用与不可用带上记下时认证文件的登录签名（账户池 mu 下核对代际并拷出认证状态，
// 锁外计算），账号已重新登录或不在池中时不通知
func (s *PooledService) notifyDrive(account *Account, generation uint64, state DriveState, checkedAt time.Time) {
	observer := s.driveObserver.Load()
	if observer == nil || account == nil {
		return
	}
	record := DriveRecord{AccountID: account.ID, State: state, CheckedAt: checkedAt}
	if state != DriveStateUnknown {
		p := s.pool
		p.mu.Lock()
		current := p.byID[account.ID] == account && account.authGeneration == generation
		storage := account.StorageState
		p.mu.Unlock()
		if !current {
			return
		}
		record.Identity = LoginIdentity(storage)
		if record.Identity == "" {
			return
		}
	}
	(*observer)(record)
}

// RestoreDriveRecords 把写盘的 Drive 状态记录恢复进记忆（装配运行时、开始调度之前调用，见 app 层 drive_state.go），返回恢复的条数。
// 只恢复账户还在池中、认证文件的登录签名与记下时相同、按记下时间还没过期的记录，按账户当前的认证代际记下；已有记录的账户不覆盖。
// 恢复不通知写盘
func (s *PooledService) RestoreDriveRecords(records []DriveRecord) int {
	if s == nil || s.pool == nil || len(records) == 0 {
		return 0
	}
	type restoring struct {
		record     DriveRecord
		account    *Account
		generation uint64
		storage    StorageState
	}
	items := make([]restoring, 0, len(records))
	p := s.pool
	p.mu.Lock()
	for _, record := range records {
		if record.State != DriveStateAvailable && record.State != DriveStateUnavailable || record.Identity == "" {
			continue
		}
		if account := p.byID[record.AccountID]; account != nil {
			items = append(items, restoring{record: record, account: account, generation: account.authGeneration, storage: account.StorageState})
		}
	}
	p.mu.Unlock()
	now := time.Now()
	restored := 0
	for _, item := range items {
		if LoginIdentity(item.storage) != item.record.Identity || item.record.CheckedAt.After(now) {
			continue
		}
		snapshot := driveSnapshot{account: item.account, generation: item.generation}
		if s.drive.states([]driveSnapshot{snapshot}, now)[0] != DriveStateUnknown {
			continue
		}
		s.drive.set(item.account, item.generation, item.record.State, item.record.CheckedAt, now)
		if s.drive.states([]driveSnapshot{snapshot}, now)[0] == item.record.State {
			restored++
		}
	}
	return restored
}

// preferDriveCandidates 把记着 Drive 不可用的账户移到候选末尾（其余保持原顺序）：自动选号的文件上传先试可能有 Drive 授权的账户，
// 记着不可用的账户仍留在候选里（记录可能已过时）。先在账户池 mu 下读好各账户的对象与认证代际，再取记忆的锁
func (s *PooledService) preferDriveCandidates(accountIDs []string) []string {
	if s == nil || s.pool == nil || len(accountIDs) < 2 || s.drive.count() == 0 {
		return accountIDs
	}
	type candidate struct {
		account    *Account
		generation uint64
	}
	snapshot := make([]candidate, len(accountIDs))
	s.pool.mu.Lock()
	for index, accountID := range accountIDs {
		if account := s.pool.byID[accountID]; account != nil {
			snapshot[index] = candidate{account: account, generation: account.authGeneration}
		}
	}
	s.pool.mu.Unlock()
	now := time.Now()
	preferred := make([]string, 0, len(accountIDs))
	var deferred []string
	for index, accountID := range accountIDs {
		if snapshot[index].account != nil && s.drive.unavailable(snapshot[index].account, snapshot[index].generation, now) {
			deferred = append(deferred, accountID)
			continue
		}
		preferred = append(preferred, accountID)
	}
	return append(preferred, deferred...)
}

// DriveCandidates 为一次选号范围内账号的 Drive 授权状态（见 PooledService.DriveCandidates）。切片只读（可能来自短时缓存，调用方不得修改）
type DriveCandidates struct {
	// Available 为记着 Drive 可用的账号，Unavailable 为记着 Drive 不可用的账号，Unknown 为状态未知的账号，均按账号 ID 排序
	Available   []string
	Unavailable []string
	Unknown     []string
}

// DriveCounts 为号池里启用账号的 Drive 授权状态计数（管理页与 /api/status）
type DriveCounts struct {
	Available   int
	Unavailable int
	Unknown     int
}

// driveCandidatesCacheTTL 为 DriveCandidates 结果的缓存时长：候选集的成员（启用、就绪、目录）最多晚这么久反映到 Drive 选号上，
// Drive 状态的变化（记忆版本）立即失效。原先每次尝试都在账户池 mu 下扫一遍候选集、在记忆的锁下逐个判断，1.5 万个账户时约 4 毫秒
const driveCandidatesCacheTTL = time.Second

// driveCandidatesCacheMax 为缓存的选择条件数上限，超过时清空重来（同时只有少数几种 Interaction 模型的选择条件）
const driveCandidatesCacheMax = 64

// driveCandidatesCache 为 DriveCandidates 的短时缓存，按候选集的键（selectionKey）存放
type driveCandidatesCache struct {
	mu      sync.Mutex
	entries map[selectionKey]driveCandidatesEntry
}

type driveCandidatesEntry struct {
	version uint64
	at      time.Time
	result  DriveCandidates
}

// DriveCandidates 返回选择条件对应候选集（启用、就绪、属于号池、至少一个通道支持该模型；冷却中的也算）里各账号的 Drive 授权状态。
// 指定账号、资源、限定与排除列表不参与（只看模型、方法、通道与号池）。Interaction 模型（gemini-omni）的视频输入（及无法转为 PNG 的图片）要先上传到账号的
// Drive：生成选号据此先试记着可用的账号、只在这些账号都在忙时试一次状态未知的账号、不选记着不可用的账号，候选里没有可用或未知的账号时
// 直接返回需要 Drive 授权的错误（见 app 层 drive_select.go）。先在账户池 mu 下读好各账号的对象与认证代际，再取记忆的锁；
// 结果按 driveCandidatesCacheTTL 缓存，记忆版本变化时失效
func (s *PooledService) DriveCandidates(ctx context.Context, selection AccountSelection) DriveCandidates {
	if s == nil || s.pool == nil {
		return DriveCandidates{}
	}
	selection = ScopedSelection(ctx, selection)
	key := selectionKeyOf(selection)
	version := s.drive.currentVersion()
	now := time.Now()
	cache := &s.driveCache
	cache.mu.Lock()
	if entry, ok := cache.entries[key]; ok && entry.version == version && now.Sub(entry.at) < driveCandidatesCacheTTL && !now.Before(entry.at) {
		cache.mu.Unlock()
		return entry.result
	}
	cache.mu.Unlock()
	snapshots := s.pool.candidateSnapshots(selection)
	var result DriveCandidates
	for index, state := range s.drive.states(snapshots, now) {
		switch state {
		case DriveStateAvailable:
			result.Available = append(result.Available, snapshots[index].account.ID)
		case DriveStateUnavailable:
			result.Unavailable = append(result.Unavailable, snapshots[index].account.ID)
		default:
			result.Unknown = append(result.Unknown, snapshots[index].account.ID)
		}
	}
	cache.mu.Lock()
	if cache.entries == nil || len(cache.entries) >= driveCandidatesCacheMax {
		cache.entries = make(map[selectionKey]driveCandidatesEntry)
	}
	cache.entries[key] = driveCandidatesEntry{version: version, at: now, result: result}
	cache.mu.Unlock()
	return result
}

// DriveSummary 返回全部启用账号的 Drive 授权状态计数（不分号池、不看就绪与冷却）
func (s *PooledService) DriveSummary() DriveCounts {
	if s == nil || s.pool == nil {
		return DriveCounts{}
	}
	p := s.pool
	p.mu.Lock()
	snapshots := make([]driveSnapshot, 0, len(p.accounts))
	for _, account := range p.accounts {
		if account != nil && account.Config.Enabled {
			snapshots = append(snapshots, driveSnapshot{account: account, generation: account.authGeneration})
		}
	}
	p.mu.Unlock()
	var counts DriveCounts
	for _, state := range s.drive.states(snapshots, time.Now()) {
		switch state {
		case DriveStateAvailable:
			counts.Available++
		case DriveStateUnavailable:
			counts.Unavailable++
		default:
			counts.Unknown++
		}
	}
	return counts
}

// candidateSnapshots 返回选择条件对应候选集的成员（按账号 ID 排序）及其认证代际，用候选索引，不逐个账号判断通道支持
func (p *AccountPool) candidateSnapshots(selection AccountSelection) []driveSnapshot {
	selection.ModelID = strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	selection.AccountID, selection.ResourceID = "", ""
	selection.AllowedAccountIDs, selection.ExcludedAccountIDs = nil, nil
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.validateCandidateSelectionLocked(selection) != nil {
		return nil
	}
	set := p.candidateSetLocked(selection, time.Now())
	positions := p.candidatePositionsLocked(set)
	snapshots := make([]driveSnapshot, 0, len(positions))
	for _, position := range positions {
		if account := p.accounts[position]; account != nil {
			snapshots = append(snapshots, driveSnapshot{account: account, generation: account.authGeneration})
		}
	}
	return snapshots
}
