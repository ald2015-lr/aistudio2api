package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// 认证文件更新（外部更新、浏览器登录、重新导入，以及服务停止期间替换了认证文件，见 reconcilePermissionDenied）时清除该账户的登记，
// 并清除它的 403 冷却与暂停，让它立即重新参与调度；之后在某个模型的某个通道上成功时清除该模型这个通道的登记。
// 号池级 403（见 forbiddenSystemicAccounts）不是账户的问题，不登记，判定成立前登记的也撤销（见 dropSystemic）。
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
	// Identity 为最近一次登记时账户登录签名 Cookie 的指纹（aistudio.LoginIdentity）。服务停止期间按说明重新获取并覆盖了认证文件时，
	// 启动后内存里的指纹与它不同，按认证文件更新清除；旧版本保存的文件里没有，不比较
	Identity string                            `json:"identity,omitempty"`
	Models   map[string]*permissionDeniedModel `json:"models"`
}

type permissionDeniedFile struct {
	Accounts map[string]*permissionDeniedAccount `json:"accounts"`
}

// permissionDeniedRegistry 为 403 无权限账户的登记表。变化在几秒内合并后原子写入一次，生成服务关闭时立即写入（flush）
type permissionDeniedRegistry struct {
	// mu 为读写锁：每个成功的请求都要查一次表（succeeded），没有这个账户的登记时只取读锁，成功的请求之间不争用；
	// 写文件时只在锁内复制一份快照，编码与写盘在锁外进行
	mu       sync.RWMutex
	path     string
	requests *requestRegistry
	accounts map[string]*permissionDeniedAccount
	// created 为各模型新建登记的累计次数，revoked 为号池级 403 撤销登记时（按号池与模型）看到的次数与起点：号池级 403 期间
	// 每个 403 都会要求撤销一次，之后没有新建登记、起点也没有提前时不再扫描整个登记表
	created   map[string]uint64
	revoked   map[permissionDeniedRevokeKey]permissionDeniedRevoke
	dirty     bool
	saveTimer *time.Timer
	// saveMu 让写文件按快照的先后进行：定时写入与关闭时的写入同时发生时，较旧的快照不会最后落盘
	saveMu sync.Mutex
	// encode 编码要写入文件的登记，为空时用 json.MarshalIndent（测试替换它来检查编码时没有持有锁）
	encode func(permissionDeniedFile) ([]byte, error)
}

type permissionDeniedRevokeKey struct {
	pool    aistudio.PoolScope
	modelID string
}

type permissionDeniedRevoke struct {
	created uint64
	since   time.Time
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
	registry := &permissionDeniedRegistry{
		requests: requests, accounts: make(map[string]*permissionDeniedAccount),
		created: make(map[string]uint64), revoked: make(map[permissionDeniedRevokeKey]permissionDeniedRevoke),
	}
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
				continue
			}
			// 重启前刚登记的条目在号池级 403 判定成立后同样要能撤销
			registry.created[modelID]++
		}
		if len(account.Models) > 0 {
			registry.accounts[accountID] = account
		}
	}
	return nil
}

// record 登记一次 403 无权限：账户在该模型该通道上返回了 403。identity 为账户当前登录签名 Cookie 的指纹（见 permissionDeniedAccount.Identity），
// 为空时保留之前记下的
func (registry *permissionDeniedRegistry) record(
	accountID string, modelID string, channel aistudio.Channel, message string, identity string, now time.Time,
) {
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
	if identity != "" {
		account.Identity = identity
	}
	model := account.Models[modelID]
	if model == nil {
		model = &permissionDeniedModel{FirstAt: now}
		account.Models[modelID] = model
		registry.created[modelID]++
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
// 没有登记时只在读锁下查一次表，不与其他成功的请求争用
func (registry *permissionDeniedRegistry) succeeded(accountID string, modelID string, channel aistudio.Channel) {
	if registry == nil {
		return
	}
	registry.mu.RLock()
	_, listed := registry.channelIndexLocked(accountID, modelID, channel)
	registry.mu.RUnlock()
	if !listed {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	// 释放读锁后可能已被其他请求清除，重新查找
	index, listed := registry.channelIndexLocked(accountID, modelID, channel)
	if !listed {
		return
	}
	account := registry.accounts[accountID]
	model := account.Models[modelID]
	model.Channels = slices.Delete(model.Channels, index, index+1)
	if len(model.Channels) == 0 {
		delete(account.Models, modelID)
	}
	if len(account.Models) == 0 {
		delete(registry.accounts, accountID)
	}
	registry.scheduleSaveLocked()
}

// channelIndexLocked 返回账户在该模型上登记的通道下标；调用方持有 mu（读锁即可）
func (registry *permissionDeniedRegistry) channelIndexLocked(accountID string, modelID string, channel aistudio.Channel) (int, bool) {
	account := registry.accounts[accountID]
	if account == nil {
		return 0, false
	}
	model := account.Models[modelID]
	if model == nil {
		return 0, false
	}
	index := slices.Index(model.Channels, string(channel))
	return index, index >= 0
}

// dropSystemic 号池级 403 判定成立后撤销号池 scope 里该模型首次登记不早于 since 的全部条目，返回撤销的账户数：这些 403 是号池一侧的
// 问题，不是账户没有权限。原先只撤销号池级判定记着的最多 5 个账户：故障紧接着成功开始时，判定要等号池 forbiddenWindow 内没有成功
// 才成立，这之前每个 403 都按账户级登记，判定成立后列表里仍留下大批正常账户。更早就登记过的保留；账户是否属于号池按当前权益判断
// （一次取得 Ultra 账户集合，只在需要扫描时取）
func (registry *permissionDeniedRegistry) dropSystemic(
	pool *aistudio.AccountPool, scope aistudio.PoolScope, modelID string, since time.Time,
) int {
	if registry == nil {
		return 0
	}
	key := permissionDeniedRevokeKey{pool: scope, modelID: modelID}
	registry.mu.RLock()
	created := registry.created[modelID]
	previous, revoked := registry.revoked[key]
	registry.mu.RUnlock()
	if created == 0 || revoked && previous.created == created && !since.Before(previous.since) {
		return 0
	}
	inPool := poolMembership(pool, scope)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	dropped := 0
	for accountID, account := range registry.accounts {
		model := account.Models[modelID]
		if model == nil || model.FirstAt.Before(since) || inPool != nil && !inPool(accountID) {
			continue
		}
		delete(account.Models, modelID)
		if len(account.Models) == 0 {
			delete(registry.accounts, accountID)
		}
		dropped++
	}
	registry.revoked[key] = permissionDeniedRevoke{created: registry.created[modelID], since: since}
	if dropped > 0 {
		registry.scheduleSaveLocked()
	}
	return dropped
}

// poolMembership 返回判断账户当前是否属于号池的函数；不限号池或没有账户池时为空（不限）
func poolMembership(pool *aistudio.AccountPool, scope aistudio.PoolScope) func(string) bool {
	if pool == nil || scope == aistudio.PoolScopeAll {
		return nil
	}
	ultra := pool.UltraAccountIDs()
	wantUltra := scope == aistudio.PoolScopeUltra
	return func(accountID string) bool {
		_, isUltra := ultra[accountID]
		return isUltra == wantUltra
	}
}

// identityOf 返回账户登记时记下的登录签名指纹，以及账户是否在登记表里
func (registry *permissionDeniedRegistry) identityOf(accountID string) (string, bool) {
	if registry == nil {
		return "", false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	account := registry.accounts[accountID]
	if account == nil {
		return "", false
	}
	return account.Identity, true
}

// setIdentity 更新账户记下的登录签名指纹（本服务自己续签换掉了登录签名 Cookie）；账户不在登记表里时什么都不做
func (registry *permissionDeniedRegistry) setIdentity(accountID string, identity string) {
	if registry == nil || identity == "" {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	account := registry.accounts[accountID]
	if account == nil || account.Identity == identity {
		return
	}
	account.Identity = identity
	registry.scheduleSaveLocked()
}

// identities 返回登记表里的全部账户与登记时记下的登录签名指纹
func (registry *permissionDeniedRegistry) identities() map[string]string {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	identities := make(map[string]string, len(registry.accounts))
	for accountID, account := range registry.accounts {
		identities[accountID] = account.Identity
	}
	return identities
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

// clear 清除指定账户（all 为真时为全部账户）的登记，返回清除的账户数。账户 ID 不区分大小写：从磁盘导入的账户以目录名为 ID，
// 不一定是小写，按原样精确匹配不到时再与登记表里的 ID 逐个按 strings.EqualFold 比较
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
		unmatched := make([]string, 0)
		for _, accountID := range accountIDs {
			if _, exists := registry.accounts[accountID]; exists {
				delete(registry.accounts, accountID)
				cleared++
			} else {
				unmatched = append(unmatched, accountID)
			}
		}
		if len(unmatched) > 0 {
			for accountID := range registry.accounts {
				if slices.ContainsFunc(unmatched, func(requested string) bool { return strings.EqualFold(requested, accountID) }) {
					delete(registry.accounts, accountID)
					cleared++
				}
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
	registry.mu.RLock()
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
	registry.mu.RUnlock()
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

// save 合并几秒内的多次变化后写一次文件。锁内只复制一份快照（复制在读锁下进行，成功请求的查表不必等它），编码与写盘在锁外：
// 登记表可能有上万个账户，原先在锁内编码整张表，每次写文件都让所有成功的请求等上百毫秒
func (registry *permissionDeniedRegistry) save() {
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
	encode := registry.encode
	registry.mu.Unlock()
	registry.mu.RLock()
	snapshot := registry.snapshotLocked()
	registry.mu.RUnlock()
	if encode == nil {
		encode = func(file permissionDeniedFile) ([]byte, error) { return json.MarshalIndent(file, "", "  ") }
	}
	data, err := encode(permissionDeniedFile{Accounts: snapshot})
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	if err != nil && registry.requests != nil {
		registry.requests.log("service", "WARN", fmt.Sprintf("403 无权限账户记录保存失败 | 错误=%v", err))
	}
}

// snapshotLocked 复制全部登记；调用方持有 mu（读锁即可）
func (registry *permissionDeniedRegistry) snapshotLocked() map[string]*permissionDeniedAccount {
	snapshot := make(map[string]*permissionDeniedAccount, len(registry.accounts))
	for accountID, account := range registry.accounts {
		copied := &permissionDeniedAccount{Identity: account.Identity, Models: make(map[string]*permissionDeniedModel, len(account.Models))}
		for modelID, model := range account.Models {
			modelCopy := *model
			modelCopy.Channels = slices.Clone(model.Channels)
			copied.Models[modelID] = &modelCopy
		}
		snapshot[accountID] = copied
	}
	return snapshot
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

// recordPermissionDenied 登记账户在该模型该通道上的一次 403 无权限，同时记下账户当前登录签名 Cookie 的指纹
func (service *trackedService) recordPermissionDenied(accountID string, modelID string, channel aistudio.Channel, message string, now time.Time) {
	if service == nil || service.permissionDenied == nil {
		return
	}
	identity := ""
	if service.pool != nil {
		identity, _ = service.pool.LoginIdentity(accountID)
	}
	service.permissionDenied.record(accountID, modelID, channel, message, identity, now)
}

// dropSystemicPermissionDenied 号池级 403 判定成立后撤销号池里该模型的登记：起点为号池里该模型最近一次成功的时间
// （见 forbiddenTracker.systemicSince），之后登记的账户都没有别的成功能证明是它自己没有权限
func (service *trackedService) dropSystemicPermissionDenied(scope aistudio.PoolScope, modelID string, now time.Time) {
	if service == nil || service.permissionDenied == nil {
		return
	}
	since := service.forbidden.systemicSince(scope, modelID, now)
	if dropped := service.permissionDenied.dropSystemic(service.pool, scope, modelID, since); dropped > 0 && service.requests != nil {
		service.requests.log("service", "INFO", fmt.Sprintf(
			"号池级 403 判定成立，撤销 403 无权限账户记录 | 模型=%s | 账户=%d | 起点=%s", modelID, dropped, since.Format(time.RFC3339),
		))
	}
}

// reconcilePermissionDenied 在启动时（账户池与登记表都已载入后）对照登记表与账户池。认证文件更新原先只由运行中的账户目录扫描发现，
// 扫描第一次看到的文件只记作基准：服务停止期间按说明覆盖了认证文件，启动后账户仍然列着、403 冷却（会持久化，第三档到太平洋时间
// 次日零点）也还在。这里把登录签名 Cookie 的指纹与登记时不同的账户按认证文件更新处理（清除登记、403 冷却与暂停），
// 不在账户池里的账户（服务停止期间删除了目录）的登记直接清除
func (service *trackedService) reconcilePermissionDenied() {
	if service == nil || service.pool == nil || service.permissionDenied == nil {
		return
	}
	missing := 0
	for accountID, identity := range service.permissionDenied.identities() {
		current, exists := service.pool.LoginIdentity(accountID)
		switch {
		case !exists:
			if service.permissionDenied.clearAccount(accountID) > 0 {
				missing++
			}
		case identity != "" && current != identity:
			service.forgetPermissionDenied(accountID, "启动时发现认证文件已更新")
		}
	}
	if missing > 0 && service.requests != nil {
		service.requests.log("service", "INFO", fmt.Sprintf("清除已不在账户池中的账户的 403 无权限记录 | 账户=%d", missing))
	}
}

// rotatingCookies 为 Google 用 Set-Cookie 定期轮换的会话 Cookie：同一登录会话里一直在变，每个进程都会把它们回写到认证文件
// （AccountLease.MergeSetCookieHeaders）
var rotatingCookies = map[string]struct{}{
	"__Secure-1PSIDTS": {}, "__Secure-3PSIDTS": {},
	"SIDCC": {}, "__Secure-1PSIDCC": {}, "__Secure-3PSIDCC": {},
}

// storageStateDigest 返回登录状态内容（Cookie、本地存储与扩展字段）的摘要，不计 rotatingCookies。同一登录会话里重新导出认证文件时
// 登录签名 Cookie 不变，只能按内容判断；本服务写回认证文件时同时更新内存，磁盘上的内容与内存不同才是外部更新。
// 轮换 Cookie 不计入：多个进程共用一个账户目录时，另一个进程回写它们后磁盘与本进程内存不同，原先按重新上传处理，
// 清除登记与 403 冷却、冷却档位从头计，每次回写都这样，列表在进程之间来回出现。只有轮换 Cookie 不同的重新导出因此发现不了
func storageStateDigest(state aistudio.StorageState) string {
	cookies := make([]aistudio.StateCookie, 0, len(state.Cookies))
	for _, cookie := range state.Cookies {
		if _, rotating := rotatingCookies[cookie.Name]; !rotating {
			cookies = append(cookies, cookie)
		}
	}
	state.Cookies = cookies
	data, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// PermissionDenied 返回 403 无权限账户列表，按最近一次 403 从新到旧排列
func (admin *runtimeAdmin) PermissionDenied(context.Context) ([]api.AdminPermissionDenied, error) {
	entries := admin.service.permissionDeniedEntries()
	result := make([]api.AdminPermissionDenied, 0, len(entries))
	// 一次加锁取得全部账户的显示名称：原先每个账户调用一次 StatusOf（每次都抢账户池的锁并构建完整状态），登记表有上万个账户时
	// 每次刷新列表都要抢上万次锁
	var labels map[string]string
	if admin.pool != nil && len(entries) > 0 {
		accountIDs := make([]string, 0, len(entries))
		for _, entry := range entries {
			accountIDs = append(accountIDs, entry.accountID)
		}
		labels = admin.pool.Labels(accountIDs)
	}
	for _, entry := range entries {
		label := entry.accountID
		if value := labels[entry.accountID]; value != "" {
			label = value
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

// ClearPermissionDenied 按管理页的操作清除 403 无权限账户的登记（不改变冷却），返回清除的账户数。账户 ID 只去掉首尾空白，
// 不转小写（与其他管理接口一致；从磁盘导入的账户 ID 是目录名，可能带大写），匹配时不区分大小写（见 permissionDeniedRegistry.clear）
func (admin *runtimeAdmin) ClearPermissionDenied(_ context.Context, input api.PermissionDeniedClearInput) (int, error) {
	accountIDs := make([]string, 0, len(input.AccountIDs))
	for _, accountID := range input.AccountIDs {
		if accountID = strings.TrimSpace(accountID); accountID != "" {
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

// permissionDeniedIdentity 返回账户登记时记下的登录签名指纹，以及账户是否列为 403 无权限账户；没有登记表时为否
func (service *trackedService) permissionDeniedIdentity(accountID string) (string, bool) {
	if service == nil {
		return "", false
	}
	return service.permissionDenied.identityOf(accountID)
}

// permissionDeniedEntries 返回登记表的列表；没有登记表时为空
func (service *trackedService) permissionDeniedEntries() []permissionDeniedEntry {
	if service == nil {
		return nil
	}
	return service.permissionDenied.list()
}
