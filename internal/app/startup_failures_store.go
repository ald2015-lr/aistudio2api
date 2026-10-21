package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 确定失效账户的持久表：启动 Worker 时确认登录失效（auth）或认证文件无法用于启动浏览器（files）、并因此处于需要重新登录状态的账户
// （有续签材料留给带租约的请求恢复、标记被认证代际拒绝的不记），按账户记下类别、登录跳转地址、错误原文（截断）、阶段、次数、
// 首次与最近一次的时间，以及记录时的登录签名指纹（aistudio.LoginIdentity）。原因不确定的失败不持久化。
// 需要重新登录的状态不会持久化（runtime-state 没有认证字段，重启后按 Cookie 推导为就绪），重启时对照这张表（reconcileStartupFailures）：
// 同一登录签名下连续两次以上确认登录失效、没有续签材料的重新标为需要重新登录；只有一次的保留展示，让账户重试一次（一次登录页跳转可能是偶发）。
// 列表只把当前处于需要重新登录状态的账户列为确定失效，其余记录（重启后待重试的等）列为未确认、不能一键删除（见 StartupFailures）。
// 认证文件更新、验证通过、删除或移走账户、启动成功时清除；续签换掉登录签名时更新记下的指纹（不当作重新上传）。
// 保存在账户目录下的 .startup-failures.json，变化在几秒内合并后原子写入一次，生成服务关闭时立即写入（flush）
const (
	startupFailureFileName  = ".startup-failures.json"
	startupFailureSaveDelay = 3 * time.Second
	// startupFailureConfirmCount 为重启时重新标为需要重新登录所需的同一登录签名下的连续次数
	startupFailureConfirmCount = 2
)

// startupFailureRecord 为一个账户的确定失效记录
type startupFailureRecord struct {
	Kind     string    `json:"kind"`
	URL      string    `json:"url,omitempty"`
	Message  string    `json:"message,omitempty"`
	Stage    int       `json:"stage,omitempty"`
	Count    int       `json:"count"`
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
	Identity string    `json:"identity,omitempty"`
}

type startupFailureFile struct {
	Accounts map[string]*startupFailureRecord `json:"accounts"`
}

// startupFailureRegistry 为确定失效账户的持久表；零值不可用，nil 时各方法什么都不做（测试与没有账户目录的管理器）
type startupFailureRegistry struct {
	mu        sync.RWMutex
	path      string
	requests  *requestRegistry
	accounts  map[string]*startupFailureRecord
	dirty     bool
	saveTimer *time.Timer
	// saveMu 让写文件按快照的先后进行：定时写入与关闭时的写入同时发生时，较旧的快照不会最后落盘
	saveMu sync.Mutex
}

// startupFailureRegistries 按文件路径共用持久表：重启生成服务时新实例先装配、旧实例后关闭，各自读写同一个文件会丢失旧实例最后几秒的变化
// （同 permissionDeniedRegistries）
var startupFailureRegistries = struct {
	mu     sync.Mutex
	byPath map[string]*startupFailureRegistry
}{byPath: make(map[string]*startupFailureRegistry)}

// openStartupFailureRegistry 返回账户目录对应的持久表，同一进程内同一目录只读取一次文件；directory 为空时只保存在内存中
func openStartupFailureRegistry(directory string, requests *requestRegistry) *startupFailureRegistry {
	if strings.TrimSpace(directory) == "" {
		return newStartupFailureRegistry("", requests)
	}
	path := filepath.Join(directory, startupFailureFileName)
	startupFailureRegistries.mu.Lock()
	defer startupFailureRegistries.mu.Unlock()
	if registry := startupFailureRegistries.byPath[path]; registry != nil {
		return registry
	}
	registry := newStartupFailureRegistry(directory, requests)
	startupFailureRegistries.byPath[path] = registry
	return registry
}

// newStartupFailureRegistry 创建持久表并读取账户目录下保存的记录
func newStartupFailureRegistry(directory string, requests *requestRegistry) *startupFailureRegistry {
	registry := &startupFailureRegistry{requests: requests, accounts: make(map[string]*startupFailureRecord)}
	if strings.TrimSpace(directory) == "" {
		return registry
	}
	registry.path = filepath.Join(directory, startupFailureFileName)
	if err := registry.load(); err != nil && requests != nil {
		requests.log("service", "WARN", fmt.Sprintf("失效账户记录读取失败，重新开始记录 | 错误=%v", err))
	}
	return registry
}

func (registry *startupFailureRegistry) load() error {
	data, err := os.ReadFile(registry.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored startupFailureFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	for accountID, record := range stored.Accounts {
		if record == nil || record.Count < 1 {
			continue
		}
		if kind := startupFailureKind(record.Kind); kind != startupFailureAuth && kind != startupFailureFiles {
			continue
		}
		registry.accounts[accountID] = record
	}
	return nil
}

// record 记下一次确定失效，返回同一登录签名下同一类别的连续次数。登录签名或类别与之前不同时从头计；identity 为空时不比较、保留之前的
func (registry *startupFailureRegistry) record(
	accountID string, kind startupFailureKind, url string, message string, stage int, identity string, now time.Time,
) int {
	if registry == nil || accountID == "" || kind != startupFailureAuth && kind != startupFailureFiles {
		return 0
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record := registry.accounts[accountID]
	if record == nil || record.Kind != string(kind) || identity != "" && record.Identity != "" && record.Identity != identity {
		record = &startupFailureRecord{Kind: string(kind), FirstAt: now}
		registry.accounts[accountID] = record
	}
	if identity != "" {
		record.Identity = identity
	}
	record.URL = url
	record.Message = message
	record.Stage = stage
	record.Count++
	record.LastAt = now
	registry.scheduleSaveLocked()
	return record.Count
}

// remove 按精确 ID 清除账户的记录，返回是否有记录
func (registry *startupFailureRegistry) remove(accountID string) bool {
	if registry == nil {
		return false
	}
	registry.mu.RLock()
	_, exists := registry.accounts[accountID]
	registry.mu.RUnlock()
	if !exists {
		return false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.accounts[accountID]; !exists {
		return false
	}
	delete(registry.accounts, accountID)
	registry.scheduleSaveLocked()
	return true
}

// clear 清除指定账户（all 为真时为全部账户）的记录，返回清除的账户。账户 ID 先精确匹配，匹配不到的再按 strings.EqualFold 比较
// （管理页的清除操作，同 permissionDeniedRegistry.clear；删除与认证文件更新的清除钩子一律用 remove 精确匹配）
func (registry *startupFailureRegistry) clear(accountIDs []string, all bool) []string {
	if registry == nil {
		return nil
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	cleared := make([]string, 0)
	if all {
		for accountID := range registry.accounts {
			cleared = append(cleared, accountID)
		}
		clear(registry.accounts)
	} else {
		unmatched := make([]string, 0)
		for _, accountID := range accountIDs {
			if _, exists := registry.accounts[accountID]; exists {
				delete(registry.accounts, accountID)
				cleared = append(cleared, accountID)
			} else {
				unmatched = append(unmatched, accountID)
			}
		}
		if len(unmatched) > 0 {
			for accountID := range registry.accounts {
				if slices.ContainsFunc(unmatched, func(requested string) bool { return strings.EqualFold(requested, accountID) }) {
					delete(registry.accounts, accountID)
					cleared = append(cleared, accountID)
				}
			}
		}
	}
	if len(cleared) > 0 {
		registry.scheduleSaveLocked()
	}
	return cleared
}

// setIdentity 更新账户记下的登录签名指纹（本服务自己续签换掉了登录签名 Cookie）；账户不在表里时什么都不做
func (registry *startupFailureRegistry) setIdentity(accountID string, identity string) {
	if registry == nil || identity == "" {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record := registry.accounts[accountID]
	if record == nil || record.Identity == identity {
		return
	}
	record.Identity = identity
	registry.scheduleSaveLocked()
}

// startupFailureEntry 为列表中的一个账户
type startupFailureEntry struct {
	accountID string
	record    startupFailureRecord
}

// list 返回全部记录（副本），按最近一次从新到旧排列
func (registry *startupFailureRegistry) list() []startupFailureEntry {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	entries := make([]startupFailureEntry, 0, len(registry.accounts))
	for accountID, record := range registry.accounts {
		entries = append(entries, startupFailureEntry{accountID: accountID, record: *record})
	}
	registry.mu.RUnlock()
	sort.Slice(entries, func(left int, right int) bool {
		if !entries[left].record.LastAt.Equal(entries[right].record.LastAt) {
			return entries[left].record.LastAt.After(entries[right].record.LastAt)
		}
		return entries[left].accountID < entries[right].accountID
	})
	return entries
}

func (registry *startupFailureRegistry) scheduleSaveLocked() {
	registry.dirty = true
	if registry.path == "" || registry.saveTimer != nil {
		return
	}
	registry.saveTimer = time.AfterFunc(startupFailureSaveDelay, registry.save)
}

// save 合并几秒内的多次变化后写一次文件：锁内只复制一份快照，编码与写盘在锁外
func (registry *startupFailureRegistry) save() {
	registry.saveMu.Lock()
	defer registry.saveMu.Unlock()
	registry.mu.Lock()
	registry.saveTimer = nil
	if !registry.dirty || registry.path == "" {
		registry.mu.Unlock()
		return
	}
	// 先清除标记再复制：两步之间的变化会被这份快照带上，并重新标记、稍后再写一次，不会丢失
	registry.dirty = false
	path := registry.path
	snapshot := make(map[string]*startupFailureRecord, len(registry.accounts))
	for accountID, record := range registry.accounts {
		copied := *record
		snapshot[accountID] = &copied
	}
	registry.mu.Unlock()
	data, err := json.MarshalIndent(startupFailureFile{Accounts: snapshot}, "", "  ")
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	if err != nil && registry.requests != nil {
		registry.requests.log("service", "WARN", fmt.Sprintf("失效账户记录保存失败 | 错误=%v", err))
	}
}

// flush 立即写入还没保存的变化（生成服务关闭时调用）
func (registry *startupFailureRegistry) flush() {
	if registry == nil {
		return
	}
	registry.mu.Lock()
	if registry.saveTimer != nil {
		registry.saveTimer.Stop()
		registry.saveTimer = nil
	}
	registry.mu.Unlock()
	registry.save()
}

// reconcileStartupFailures 在启动时（账户池与持久表都已载入后）对照持久表与账户池：
//   - 账户已不在池里（服务停止期间删除了目录）：删除记录；
//   - 登录签名与记录时不同（服务停止期间重新上传了认证文件）：删除记录；
//   - 登录失效记录、同一登录签名下连续 startupFailureConfirmCount 次以上、没有续签材料、当前不是需要重新登录：重新标为需要重新登录；
//   - 只有一次的只保留展示，让账户重试一次
func (manager *accountWorkerManager) reconcileStartupFailures() {
	registry := manager.startupFailures
	entries := registry.list()
	if len(entries) == 0 {
		return
	}
	accountIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		accountIDs = append(accountIDs, entry.accountID)
	}
	states := manager.pool.States(accountIDs)
	missing, replaced, marked := 0, 0, 0
	for _, entry := range entries {
		current, exists := manager.pool.LoginIdentity(entry.accountID)
		switch {
		case !exists:
			if registry.remove(entry.accountID) {
				missing++
			}
		case entry.record.Identity != "" && current != entry.record.Identity:
			if registry.remove(entry.accountID) {
				replaced++
			}
		case entry.record.Kind == string(startupFailureAuth) && entry.record.Count >= startupFailureConfirmCount:
			if state := states[entry.accountID]; state == aistudio.AccountAuthRequired || state == aistudio.AccountDisabled {
				continue
			}
			if storage, ok := manager.pool.StorageStateOf(entry.accountID); ok && authRefreshMaterial(storage) {
				continue
			}
			reason := fmt.Sprintf("启动 Worker 时连续 %d 次确认登录失效（重启前记录）：%s", entry.record.Count, entry.record.Message)
			if err := manager.pool.MarkAuthRequired(entry.accountID, reason); err != nil {
				manager.requests.log(entry.accountID, "WARN", "失效账户重新标记失败 | 错误="+strings.TrimSpace(err.Error()))
				continue
			}
			marked++
		}
	}
	if missing+replaced+marked > 0 {
		manager.requests.log("service", "INFO", fmt.Sprintf(
			"失效账户记录对账 | 重新标为需要重新登录=%d | 清除已不在账户池中=%d | 清除认证文件已更新=%d", marked, missing, replaced,
		))
	}
}
