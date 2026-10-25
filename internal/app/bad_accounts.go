package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// 坏号记忆（N2）：线上发出请求的 73 个账号里 40 个出现裸 Quota exceeded，出现之后只有 10% 还成功过，同账号下一次尝试 63% 仍被拒，
// 换 10 个住宅 IP 后照样出现（nonip_deep_result R4）。逐级冷却只在内存、按（账号，模型）记，预热与替补排序不看历史结果，
// 每遇到一个坏号都要白付一次冷启动加一次必败的尝试。这里按账号（不分模型）记：连续 badAccountStreak 次裸限流、期间没有成功，
// 就在 badAccountHold 内移出预热、扩容（含按需提升）与替补的候选（还有其他候选时；全是坏号时照常用，免得小号池没有可启动的账号），
// 并且预热与替补在能服务主用（与热门）模型的前提下优先选近期成功过的账号。与出口判定解耦：不要求出口判定成立
// （多出口时每出口的判定门槛很难达到）；出口判定的“冷号首发被拒”不计已标坏号的账号，免得把坏号误判成出口限流。
// 记录写进模型目录缓存同目录的 bad-accounts.json，重启后保留

const (
	// badAccountStreak 为标坏号要求的连续裸限流次数（期间没有成功）
	badAccountStreak = 2
	// badAccountHold 为坏号移出候选的时长：取较短的固定值而不是等到太平洋时间零点（最长近 24 小时），裸限流不一定是当天额度用完，
	// 2 小时后重新参与预热，成功一次即清除
	badAccountHold = 2 * time.Hour
	// badAccountSuccessKeep 为“近期成功”的有效期（预热与替补优先），也是写盘时保留成功记录的时长
	badAccountSuccessKeep = 24 * time.Hour
	// badAccountSaveInterval 为有变化时两次写盘的最短间隔
	badAccountSaveInterval = 30 * time.Second
	// badAccountFileName 为记录文件名（放在模型目录缓存的目录里）
	badAccountFileName = "bad-accounts.json"
	// badAccountFileVersion 为记录文件的格式版本，不符时忽略
	badAccountFileVersion = 1
)

// badAccountEntry 为一个账号的记录（unix 秒，字段取短名）
type badAccountEntry struct {
	// Until 为坏号到期时间，0 为不是坏号；Success 为最近一次成功
	Until   int64 `json:"u,omitempty"`
	Success int64 `json:"s,omitempty"`
}

// badAccountFile 为 bad-accounts.json
type badAccountFile struct {
	Version  int                        `json:"version"`
	SavedAt  time.Time                  `json:"saved_at"`
	Accounts map[string]badAccountEntry `json:"accounts"`
}

// badAccountMemory 为坏号记忆；方法全部 nil 安全（测试直接构造的服务没有记忆，候选顺序不变）
type badAccountMemory struct {
	mu       sync.Mutex
	path     string
	requests *requestRegistry
	entries  map[string]badAccountEntry
	// streaks 为连续裸限流次数（只在内存）
	streaks map[string]int
	dirty   bool
	timer   *time.Timer
	saveMu  sync.Mutex
	closed  bool
	// now 只给包内测试替换
	now func() time.Time
}

// openBadAccountMemory 打开坏号记忆：path 为空时只在内存；文件不存在、损坏或版本不符时从空开始（损坏时写一条警告）
func openBadAccountMemory(path string, requests *requestRegistry) *badAccountMemory {
	memory := &badAccountMemory{
		path: path, requests: requests, entries: make(map[string]badAccountEntry), streaks: make(map[string]int),
	}
	if path == "" {
		return memory
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return memory
	}
	var file badAccountFile
	if err == nil {
		err = json.Unmarshal(data, &file)
	}
	if err != nil {
		memory.log("WARN", "坏号记忆读取失败，从空开始 | 文件="+path+" | 错误="+err.Error())
		return memory
	}
	if file.Version != badAccountFileVersion {
		return memory
	}
	now := memory.clock().Unix()
	for accountID, entry := range file.Accounts {
		if entry.Until <= now {
			entry.Until = 0
		}
		if now-entry.Success >= int64(badAccountSuccessKeep/time.Second) {
			entry.Success = 0
		}
		if entry.Until > 0 || entry.Success > 0 {
			memory.entries[accountID] = entry
		}
	}
	return memory
}

func (memory *badAccountMemory) clock() time.Time {
	if memory.now != nil {
		return memory.now()
	}
	return time.Now()
}

func (memory *badAccountMemory) log(level string, message string) {
	if memory != nil && memory.requests != nil {
		memory.requests.log("service", level, message)
	}
}

// observe 记一次已发往上游的生成尝试的结果：成功清零连续次数并清除坏号；裸限流累计，达到 badAccountStreak 时标坏号
func (memory *badAccountMemory) observe(accountID string, label string, outcome egressOutcome) {
	if memory == nil || accountID == "" {
		return
	}
	now := memory.clock()
	memory.mu.Lock()
	entry := memory.entries[accountID]
	marked := false
	switch outcome {
	case egressSuccess:
		delete(memory.streaks, accountID)
		entry.Success = now.Unix()
		entry.Until = 0
		memory.entries[accountID] = entry
		memory.dirty = true
	case egressBare:
		memory.streaks[accountID]++
		if memory.streaks[accountID] >= badAccountStreak && entry.Until <= now.Unix() {
			entry.Until = now.Add(badAccountHold).Unix()
			memory.entries[accountID] = entry
			memory.dirty = true
			marked = true
		}
	default:
		memory.mu.Unlock()
		return
	}
	streak := memory.streaks[accountID]
	memory.scheduleSaveLocked()
	memory.mu.Unlock()
	if marked {
		if label == "" {
			label = accountID
		}
		if memory.requests != nil {
			memory.requests.log(label, "WARN", fmt.Sprintf(
				"账号连续裸限流，移出预热与替补候选 | 连续=%d | 时长=%s | 恢复=%s",
				streak, badAccountHold, now.Add(badAccountHold).Format(time.RFC3339),
			))
		}
	}
}

// pardon 撤回账号因 IP 被拒记下的裸限流（IP 池端口按规则 A 冷却时调用，见 relaxProxyPort：端口换手后别的账号在同一端口上也被拒，
// 被拒的是 IP 不是账号）：清零连续次数；坏号标记是最近 within 内（端口规则 A 的窗口）标上的就清除，更早标上的（证据来自这个窗口之前，
// 不一定是这个 IP）保留。不记成功（预热与替补的“近期成功”优先不受影响），清除时照常排队写盘。返回是否清除了坏号标记
func (memory *badAccountMemory) pardon(accountID string, within time.Duration) bool {
	if memory == nil || accountID == "" {
		return false
	}
	now := memory.clock()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	delete(memory.streaks, accountID)
	entry, ok := memory.entries[accountID]
	// 标上的时刻为 Until - badAccountHold（按秒取整，多留 1 秒）
	if !ok || entry.Until <= now.Unix() || entry.Until-int64(badAccountHold/time.Second) < now.Add(-within).Unix()-1 {
		return false
	}
	entry.Until = 0
	if entry.Success == 0 {
		delete(memory.entries, accountID)
	} else {
		memory.entries[accountID] = entry
	}
	memory.dirty = true
	memory.scheduleSaveLocked()
	return true
}

// bad 返回账号现在是否为坏号
func (memory *badAccountMemory) bad(accountID string) bool {
	if memory == nil {
		return false
	}
	now := memory.clock().Unix()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	return memory.entries[accountID].Until > now
}

// count 返回现在的坏号数（状态接口）
func (memory *badAccountMemory) count() int {
	if memory == nil {
		return 0
	}
	now := memory.clock().Unix()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	total := 0
	for _, entry := range memory.entries {
		if entry.Until > now {
			total++
		}
	}
	return total
}

// order 整理预热与替补的冷启动候选：还有不是坏号的候选时去掉坏号；再把 badAccountSuccessKeep 内成功过的账号
// 稳定地移到最前（其余保持原顺序）。全是坏号时原样返回（小号池没有别的账号可启动）。
// 之后还要按能服务的主用模型个数、其次热门模型个数稳定排序，近期成功只在同分时优先（见 rankColdCandidates）；扩容与按需提升不用它，
// 只去坏号（见 withoutBadAccounts）
func (memory *badAccountMemory) order(accountIDs []string) []string {
	if memory == nil || len(accountIDs) == 0 {
		return accountIDs
	}
	now := memory.clock().Unix()
	keep := int64(badAccountSuccessKeep / time.Second)
	memory.mu.Lock()
	if len(memory.entries) == 0 {
		memory.mu.Unlock()
		return accountIDs
	}
	recent := make([]string, 0, len(accountIDs))
	neutral := make([]string, 0, len(accountIDs))
	bad := 0
	for _, accountID := range accountIDs {
		entry := memory.entries[accountID]
		switch {
		case entry.Until > now:
			bad++
		case entry.Success > 0 && now-entry.Success < keep:
			recent = append(recent, accountID)
		default:
			neutral = append(neutral, accountID)
		}
	}
	memory.mu.Unlock()
	if bad == len(accountIDs) {
		return accountIDs
	}
	if bad == 0 && len(recent) == 0 {
		return accountIDs
	}
	return append(recent, neutral...)
}

// scheduleSaveLocked 有变化时安排一次写盘（最多每 badAccountSaveInterval 一次）；调用方持有 mu
func (memory *badAccountMemory) scheduleSaveLocked() {
	if memory.path == "" || !memory.dirty || memory.timer != nil || memory.closed {
		return
	}
	memory.timer = time.AfterFunc(badAccountSaveInterval, func() {
		memory.mu.Lock()
		memory.timer = nil
		memory.mu.Unlock()
		if err := memory.save(); err != nil {
			memory.log("WARN", "坏号记忆写入失败 | 错误="+err.Error())
		}
	})
}

// save 写盘（原子写）：过期的坏号与超过 badAccountSuccessKeep 的成功记录不写
func (memory *badAccountMemory) save() error {
	if memory == nil || memory.path == "" {
		return nil
	}
	memory.saveMu.Lock()
	defer memory.saveMu.Unlock()
	now := memory.clock()
	memory.mu.Lock()
	if !memory.dirty {
		memory.mu.Unlock()
		return nil
	}
	file := badAccountFile{Version: badAccountFileVersion, SavedAt: now.UTC(), Accounts: make(map[string]badAccountEntry, len(memory.entries))}
	keep := int64(badAccountSuccessKeep / time.Second)
	for accountID, entry := range memory.entries {
		if entry.Until <= now.Unix() {
			entry.Until = 0
		}
		if now.Unix()-entry.Success >= keep {
			entry.Success = 0
		}
		if entry.Until == 0 && entry.Success == 0 {
			delete(memory.entries, accountID)
			continue
		}
		file.Accounts[accountID] = entry
	}
	memory.dirty = false
	memory.mu.Unlock()
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(memory.path, data, 0o600); err != nil {
		memory.mu.Lock()
		memory.dirty = true
		memory.mu.Unlock()
		return err
	}
	return nil
}

// close 停止计时器并立即写出没有落盘的变化（关闭运行时调用）
func (memory *badAccountMemory) close() error {
	if memory == nil {
		return nil
	}
	memory.mu.Lock()
	memory.closed = true
	if memory.timer != nil {
		memory.timer.Stop()
		memory.timer = nil
	}
	memory.mu.Unlock()
	return memory.save()
}

// forget 移除账号的记录（账号删除后）
func (memory *badAccountMemory) forget(accountID string) {
	if memory == nil {
		return
	}
	memory.mu.Lock()
	if _, ok := memory.entries[accountID]; ok {
		delete(memory.entries, accountID)
		memory.dirty = true
		memory.scheduleSaveLocked()
	}
	delete(memory.streaks, accountID)
	memory.mu.Unlock()
}

// badAccountPath 返回坏号记忆文件的位置（模型目录缓存的目录里）
func badAccountPath() string {
	return filepath.Join(catalogCacheDir, badAccountFileName)
}

// badAccountMemory 返回管理器挂着的坏号记忆（manager 为 nil 或没有挂时为 nil，方法都 nil 安全）
func (manager *accountWorkerManager) badAccountMemory() *badAccountMemory {
	if manager == nil {
		return nil
	}
	return manager.badAccounts.Load()
}

// orderColdCandidates 为 Worker 管理器整理冷启动候选（见 badAccountMemory.order）；没有坏号记忆时原样返回
func (manager *accountWorkerManager) orderColdCandidates(accountIDs []string) []string {
	return manager.badAccountMemory().order(accountIDs)
}

// withoutBadAccounts 返回去掉坏号后的候选（全是坏号时原样返回），不改变顺序：按需提升与扩容的候选用它，顺序仍由偏好决定
func (manager *accountWorkerManager) withoutBadAccounts(accountIDs []string) []string {
	memory := manager.badAccountMemory()
	if memory == nil || len(accountIDs) == 0 {
		return accountIDs
	}
	kept := slices.DeleteFunc(slices.Clone(accountIDs), memory.bad)
	if len(kept) == 0 {
		return accountIDs
	}
	return kept
}
