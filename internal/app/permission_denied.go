package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 403 无权限账户登记：生成请求返回 HTTP 403 PERMISSION_DENIED（The caller does not have permission，协议错误码 7）的账户
// 按模型记下通道、首次与最近一次的时间、次数与上游原文，管理页账户列表上方单独列出。这类账户多半是没有同意 Google 的政策，
// 冷却到期后仍然 403，只有重新获取认证文件上传才能恢复；冷却列表按模型汇总、到期即消失，看不出是哪些账户。
// 认证文件更新（外部更新、浏览器登录、重新导入）时清除该账户的登记，并清除它的 403 冷却与暂停，让它立即重新参与调度；
// 之后在某个模型的某个通道上成功时清除该模型这个通道的登记。号池级 403（见 forbiddenSystemicAccounts）不是账户的问题，不登记。
// 保存在账户目录下的 .permission-denied.json，不写入各账户的 account.json 与 runtime-state.json
const (
	permissionDeniedFileName  = ".permission-denied.json"
	permissionDeniedSaveDelay = 3 * time.Second
	// permissionDeniedMessageLimit 为保存的上游原文的最大字符数
	permissionDeniedMessageLimit = 300
)

// permissionDeniedModel 为账户在一个模型上的 403 记录；Channels 为返回过 403、之后还没有成功过的通道
type permissionDeniedModel struct {
	Channels []string  `json:"channels"`
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
	Count    int       `json:"count"`
	Message  string    `json:"message,omitempty"`
}

type permissionDeniedAccount struct {
	Models map[string]*permissionDeniedModel `json:"models"`
}

type permissionDeniedFile struct {
	Accounts map[string]*permissionDeniedAccount `json:"accounts"`
}

// permissionDeniedRegistry 为 403 无权限账户的登记表。变化在几秒内合并后原子写入一次，生成服务关闭时立即写入（flush）
type permissionDeniedRegistry struct {
	mu        sync.Mutex
	path      string
	requests  *requestRegistry
	accounts  map[string]*permissionDeniedAccount
	dirty     bool
	saveTimer *time.Timer
	// saveMu 让写文件按快照的先后进行：定时写入与关闭时的写入同时发生时，较旧的快照不会最后落盘
	saveMu sync.Mutex
}

// permissionDeniedRegistries 按文件路径共用登记表：重启生成服务时新实例先装配、旧实例后关闭，各自读写同一个文件时，
// 新实例读到的是旧实例还没写入的旧内容，旧实例最后几秒登记的账户会丢失，刚清除的登记也会重新出现
var permissionDeniedRegistries = struct {
	mu     sync.Mutex
	byPath map[string]*permissionDeniedRegistry
}{byPath: make(map[string]*permissionDeniedRegistry)}

// openPermissionDeniedRegistry 返回账户目录对应的登记表，同一进程内同一目录只读取一次文件
func openPermissionDeniedRegistry(directory string, requests *requestRegistry) *permissionDeniedRegistry {
	if strings.TrimSpace(directory) == "" {
		return newPermissionDeniedRegistry("", requests)
	}
	path := filepath.Join(directory, permissionDeniedFileName)
	permissionDeniedRegistries.mu.Lock()
	defer permissionDeniedRegistries.mu.Unlock()
	if registry := permissionDeniedRegistries.byPath[path]; registry != nil {
		return registry
	}
	registry := newPermissionDeniedRegistry(directory, requests)
	permissionDeniedRegistries.byPath[path] = registry
	return registry
}

// newPermissionDeniedRegistry 创建登记表并读取账户目录下保存的记录；directory 为空时只保存在内存中
func newPermissionDeniedRegistry(directory string, requests *requestRegistry) *permissionDeniedRegistry {
	registry := &permissionDeniedRegistry{requests: requests, accounts: make(map[string]*permissionDeniedAccount)}
	if strings.TrimSpace(directory) == "" {
		return registry
	}
	registry.path = filepath.Join(directory, permissionDeniedFileName)
	if err := registry.load(); err != nil && requests != nil {
		requests.log("service", "WARN", fmt.Sprintf("403 无权限账户记录读取失败，重新开始记录 | 错误=%v", err))
	}
	return registry
}

func (registry *permissionDeniedRegistry) load() error {
	data, err := os.ReadFile(registry.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored permissionDeniedFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	for accountID, account := range stored.Accounts {
		if account == nil {
			continue
		}
		for modelID, model := range account.Models {
			if model == nil || len(model.Channels) == 0 {
				delete(account.Models, modelID)
			}
		}
		if len(account.Models) > 0 {
			registry.accounts[accountID] = account
		}
	}
	return nil
}

// record 登记一次 403 无权限：账户在该模型该通道上返回了 403
func (registry *permissionDeniedRegistry) record(accountID string, modelID string, channel aistudio.Channel, message string, now time.Time) {
	if registry == nil || accountID == "" || modelID == "" {
		return
	}
	message = strings.TrimSpace(message)
	if runes := []rune(message); len(runes) > permissionDeniedMessageLimit {
		message = string(runes[:permissionDeniedMessageLimit]) + "…"
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	account := registry.accounts[accountID]
	if account == nil {
		account = &permissionDeniedAccount{Models: make(map[string]*permissionDeniedModel)}
		registry.accounts[accountID] = account
	}
	model := account.Models[modelID]
	if model == nil {
		model = &permissionDeniedModel{FirstAt: now}
		account.Models[modelID] = model
	}
	if !slices.Contains(model.Channels, string(channel)) {
		model.Channels = append(model.Channels, string(channel))
		slices.Sort(model.Channels)
	}
	model.LastAt = now
	model.Count++
	model.Message = message
	registry.scheduleSaveLocked()
}

// succeeded 账户在该模型该通道上请求成功：清除这个通道的登记，模型的通道都清除后清除该模型。在每个成功的请求上调用，
// 没有登记时只查一次表
func (registry *permissionDeniedRegistry) succeeded(accountID string, modelID string, channel aistudio.Channel) {
	if registry == nil {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	account := registry.accounts[accountID]
	if account == nil {
		return
	}
	model := account.Models[modelID]
	if model == nil {
		return
	}
	index := slices.Index(model.Channels, string(channel))
	if index < 0 {
		return
	}
	model.Channels = slices.Delete(model.Channels, index, index+1)
	if len(model.Channels) == 0 {
		delete(account.Models, modelID)
	}
	if len(account.Models) == 0 {
		delete(registry.accounts, accountID)
	}
	registry.scheduleSaveLocked()
}

// dropSystemic 号池级 403 判定成立后撤销之前几个账号在该模型上新登记的条目（首次登记不早于 since）：这些 403 是号池一侧的问题，
// 不是账户没有权限。更早就登记过的保留
func (registry *permissionDeniedRegistry) dropSystemic(accountIDs []string, modelID string, since time.Time) {
	if registry == nil || len(accountIDs) == 0 {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	changed := false
	for _, accountID := range accountIDs {
		account := registry.accounts[accountID]
		if account == nil {
			continue
		}
		model := account.Models[modelID]
		if model == nil || model.FirstAt.Before(since) {
			continue
		}
		delete(account.Models, modelID)
		if len(account.Models) == 0 {
			delete(registry.accounts, accountID)
		}
		changed = true
	}
	if changed {
		registry.scheduleSaveLocked()
	}
}

// clearAccount 清除账户的全部登记，返回清除的模型数
func (registry *permissionDeniedRegistry) clearAccount(accountID string) int {
	if registry == nil {
		return 0
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	account := registry.accounts[accountID]
	if account == nil {
		return 0
	}
	delete(registry.accounts, accountID)
	registry.scheduleSaveLocked()
	return len(account.Models)
}

// clear 清除指定账户（all 为真时为全部账户）的登记，返回清除的账户数
func (registry *permissionDeniedRegistry) clear(accountIDs []string, all bool) int {
	if registry == nil {
		return 0
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	cleared := 0
	if all {
		cleared = len(registry.accounts)
		clear(registry.accounts)
	} else {
		for _, accountID := range accountIDs {
			if _, exists := registry.accounts[accountID]; exists {
				delete(registry.accounts, accountID)
				cleared++
			}
		}
	}
	if cleared > 0 {
		registry.scheduleSaveLocked()
	}
	return cleared
}

// permissionDeniedEntry 为列表中的一个账户；Models 按最近一次 403 从新到旧排列
type permissionDeniedEntry struct {
	accountID string
	models    []permissionDeniedModelEntry
	count     int
	firstAt   time.Time
	lastAt    time.Time
}

type permissionDeniedModelEntry struct {
	modelID string
	model   permissionDeniedModel
}

// list 返回全部登记，按最近一次 403 从新到旧排列
func (registry *permissionDeniedRegistry) list() []permissionDeniedEntry {
	if registry == nil {
		return nil
	}
	registry.mu.Lock()
	entries := make([]permissionDeniedEntry, 0, len(registry.accounts))
	for accountID, account := range registry.accounts {
		entry := permissionDeniedEntry{accountID: accountID, models: make([]permissionDeniedModelEntry, 0, len(account.Models))}
		for modelID, model := range account.Models {
			copied := *model
			copied.Channels = slices.Clone(model.Channels)
			entry.models = append(entry.models, permissionDeniedModelEntry{modelID: modelID, model: copied})
			entry.count += model.Count
			if entry.firstAt.IsZero() || model.FirstAt.Before(entry.firstAt) {
				entry.firstAt = model.FirstAt
			}
			if model.LastAt.After(entry.lastAt) {
				entry.lastAt = model.LastAt
			}
		}
		entries = append(entries, entry)
	}
	registry.mu.Unlock()
	for _, entry := range entries {
		sort.Slice(entry.models, func(left int, right int) bool {
			if !entry.models[left].model.LastAt.Equal(entry.models[right].model.LastAt) {
				return entry.models[left].model.LastAt.After(entry.models[right].model.LastAt)
			}
			return entry.models[left].modelID < entry.models[right].modelID
		})
	}
	sort.Slice(entries, func(left int, right int) bool {
		if !entries[left].lastAt.Equal(entries[right].lastAt) {
			return entries[left].lastAt.After(entries[right].lastAt)
		}
		return entries[left].accountID < entries[right].accountID
	})
	return entries
}

func (registry *permissionDeniedRegistry) scheduleSaveLocked() {
	registry.dirty = true
	if registry.path == "" || registry.saveTimer != nil {
		return
	}
	registry.saveTimer = time.AfterFunc(permissionDeniedSaveDelay, registry.save)
}

// save 合并几秒内的多次变化后写一次文件
func (registry *permissionDeniedRegistry) save() {
	registry.saveMu.Lock()
	defer registry.saveMu.Unlock()
	registry.mu.Lock()
	registry.saveTimer = nil
	if !registry.dirty || registry.path == "" {
		registry.mu.Unlock()
		return
	}
	registry.dirty = false
	path := registry.path
	data, err := json.MarshalIndent(permissionDeniedFile{Accounts: registry.accounts}, "", "  ")
	registry.mu.Unlock()
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	if err != nil && registry.requests != nil {
		registry.requests.log("service", "WARN", fmt.Sprintf("403 无权限账户记录保存失败 | 错误=%v", err))
	}
}

// flush 立即写入还没保存的变化（生成服务关闭时调用）
func (registry *permissionDeniedRegistry) flush() {
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

// forgetPermissionDenied 认证文件更新（外部更新、浏览器登录、重新导入）后清除账户的 403 无权限状态：登记表里的条目、
// 单模型 403 冷却与多模型 403 暂停（按冷却原因识别，额度等其他冷却不动）、冷却档位与连续 403 计数。没有同意 Google 政策的账户
// 重新获取认证文件上传后立即重新参与调度，不必等冷却到期（第三档要到太平洋时间次日零点）；新的认证文件仍然 403 时会重新登记
func (service *trackedService) forgetPermissionDenied(accountID string, source string) {
	if service == nil {
		return
	}
	models := service.permissionDenied.clearAccount(accountID)
	service.forbidden.forgetAccount(accountID)
	cooldowns := 0
	if service.pool != nil {
		cleared, err := service.pool.ClearCooldownsMatching(accountID, forbiddenCooldown)
		if err != nil && service.requests != nil {
			service.requests.log(accountID, "WARN", fmt.Sprintf("清除 403 无权限冷却失败 | 来源=%s | 错误=%s", source, strings.TrimSpace(err.Error())))
		}
		cooldowns = cleared
	}
	if models == 0 && cooldowns == 0 {
		return
	}
	if service.requests != nil {
		service.requests.log(accountID, "INFO", fmt.Sprintf(
			"认证文件已更新，清除 403 无权限记录与冷却 | 来源=%s | 记录模型=%d | 冷却=%d", source, models, cooldowns,
		))
	}
	if cooldowns > 0 {
		service.publishModelAccess()
	}
}

// PermissionDenied 返回 403 无权限账户列表，按最近一次 403 从新到旧排列
func (admin *runtimeAdmin) PermissionDenied(context.Context) ([]api.AdminPermissionDenied, error) {
	entries := admin.service.permissionDeniedEntries()
	result := make([]api.AdminPermissionDenied, 0, len(entries))
	for _, entry := range entries {
		label := entry.accountID
		if admin.pool != nil {
			if status, ok := admin.pool.StatusOf(entry.accountID); ok && status.Label != "" {
				label = status.Label
			}
		}
		models := make([]api.AdminPermissionDeniedModel, 0, len(entry.models))
		for _, model := range entry.models {
			models = append(models, api.AdminPermissionDeniedModel{
				Model: model.modelID, Channels: model.model.Channels, Count: model.model.Count,
				FirstAt: model.model.FirstAt, LastAt: model.model.LastAt, Message: model.model.Message,
			})
		}
		result = append(result, api.AdminPermissionDenied{
			ID: entry.accountID, Label: label, Models: models, Count: entry.count,
			FirstAt: entry.firstAt, LastAt: entry.lastAt,
		})
	}
	return result, nil
}

// ClearPermissionDenied 按管理页的操作清除 403 无权限账户的登记（不改变冷却），返回清除的账户数
func (admin *runtimeAdmin) ClearPermissionDenied(_ context.Context, input api.PermissionDeniedClearInput) (int, error) {
	accountIDs := make([]string, 0, len(input.AccountIDs))
	for _, accountID := range input.AccountIDs {
		if accountID = strings.ToLower(strings.TrimSpace(accountID)); accountID != "" {
			accountIDs = append(accountIDs, accountID)
		}
	}
	if !input.All && len(accountIDs) == 0 {
		return 0, &adminOperationError{status: http.StatusBadRequest, code: "invalid_request", message: "需要 account_ids 或 all"}
	}
	var registry *permissionDeniedRegistry
	if admin.service != nil {
		registry = admin.service.permissionDenied
	}
	cleared := registry.clear(accountIDs, input.All)
	if cleared > 0 {
		admin.requests.log("auth", "INFO", fmt.Sprintf("已清除 403 无权限账户记录 | 账户=%d", cleared))
	}
	return cleared, nil
}

// permissionDeniedEntries 返回登记表的列表；没有登记表时为空
func (service *trackedService) permissionDeniedEntries() []permissionDeniedEntry {
	if service == nil {
		return nil
	}
	return service.permissionDenied.list()
}
