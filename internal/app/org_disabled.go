package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 账号所属组织停用了 AI Studio（Google Workspace 账号的组织或 Google 为它关闭了这项服务，见 aistudio/org_disabled.go）：
// 载入 AI Studio 被跳到 access.workspace.google.com 的 ServiceNotAllowed 页，RPC 返回 HTTP 403、协议错误码 7、“AI Studio app is disabled”。
// 重新登录也没用，只有组织重新开放或换号才能恢复。线上 2 小时 260 个这样的账户：每次启动干等 120 秒（INIT_TIMEOUT）才失败、占着启动名额，
// 失败后过一阵又被挑出来再启动（坏号记忆只暂缓 2 小时），模型目录每分钟重试一次，管理页看不出是这个原因。这里把它做成账户级状态：
//   - 进入：启动（页面停在组织停用页）、模型目录（目录同步的 RPC）、验证与登录（隔离浏览器停在组织停用页）、请求（生成与其他 RPC）
//     都经 enterOrgDisabled 进入同一状态：账户池记标记（对外状态 org_disabled，不参与任何调度），登记表记下来源、时间与原文，
//     驻留 Worker 登记释放（空闲即关，忙碌等请求结束、不再接新请求），模型目录改为长间隔复查；同一账户只写一行日志；
//   - 持久化：账户目录下的 .org-disabled.json（写法同 403 无权限登记表：几秒合并、原子写、关闭时 flush），重启后在载入账户池后
//     恢复标记（reconcileOrgDisabled）；
//   - 清除：认证文件更新（外部替换、重新导入、浏览器登录，以及服务停止期间替换，识别方式与 403 登记相同）、管理员在管理页清除、
//     长间隔复查（orgDisabledRecheck，±10%）时上游不再返回 app is disabled；清除后立即恢复调度，模型目录随即按常规同步。
//     复查遇到其他失败（5xx、网络、空目录、401 等）时仍是组织停用，复查时间按退避顺延并写回登记表（见 orgRecheckFailedLocked）
const (
	orgDisabledFileName  = ".org-disabled.json"
	orgDisabledSaveDelay = 3 * time.Second
	// orgDisabledMessageLimit 为保存与写进日志的原文的最大字符数
	orgDisabledMessageLimit = 200
	// orgDisabledRecheck 为模型目录长间隔复查的间隔（×U(0.9,1.1)）：组织重新开放 AI Studio 后最晚约一天恢复，
	// 原先失败后每分钟重试一次（2 小时 1391 条“模型目录同步失败”）
	orgDisabledRecheck = 24 * time.Hour
)

// orgDisabledEntry 为一个账户的组织停用记录：Identity 为最近一次登记时登录签名 Cookie 的指纹（aistudio.LoginIdentity），
// 服务停止期间替换了认证文件时启动后与内存不同，按认证文件更新清除；Source 为第一次进入的来源，LastSource 为最近一次；
// Message 为最近一次的原文（已打码、截短）；CheckAt 为下一次长间隔复查的时间；RecheckFailures 为复查连续没有结论
// （上游返回其他失败，见 catalogSyncer.orgRecheckFailedLocked）的次数，复查退避据此计算，再次确认组织停用时清零
type orgDisabledEntry struct {
	Identity        string    `json:"identity,omitempty"`
	Source          string    `json:"source"`
	LastSource      string    `json:"last_source,omitempty"`
	FirstAt         time.Time `json:"first_at"`
	LastAt          time.Time `json:"last_at"`
	Count           int       `json:"count"`
	Message         string    `json:"message,omitempty"`
	CheckAt         time.Time `json:"check_at"`
	RecheckFailures int       `json:"recheck_failures,omitempty"`
}

type orgDisabledFile struct {
	Accounts map[string]*orgDisabledEntry `json:"accounts"`
}

// orgDisabledRegistry 为组织停用账户的登记表。变化在几秒内合并后原子写入一次，生成服务关闭时立即写入（flush）
type orgDisabledRegistry struct {
	mu       sync.RWMutex
	path     string
	requests *requestRegistry
	accounts map[string]*orgDisabledEntry
	dirty    bool
	// saveTimer 为合并写入的计时器；saveMu 让写文件按快照的先后进行（同 permissionDeniedRegistry）
	saveTimer *time.Timer
	saveMu    sync.Mutex
	// random 返回 [0,1) 的随机数，为空时用 math/rand（只给包内测试固定）
	random func() float64
}

// orgDisabledRegistries 按文件路径共用登记表（原因同 permissionDeniedRegistries：重启生成服务时新旧实例读写同一个文件）
var orgDisabledRegistries = struct {
	mu     sync.Mutex
	byPath map[string]*orgDisabledRegistry
}{byPath: make(map[string]*orgDisabledRegistry)}

// openOrgDisabledRegistry 返回账户目录对应的登记表，同一进程内同一目录只读取一次文件；directory 为空时只保存在内存中
func openOrgDisabledRegistry(directory string, requests *requestRegistry) *orgDisabledRegistry {
	if strings.TrimSpace(directory) == "" {
		return newOrgDisabledRegistry("", requests)
	}
	path := filepath.Join(directory, orgDisabledFileName)
	orgDisabledRegistries.mu.Lock()
	defer orgDisabledRegistries.mu.Unlock()
	if registry := orgDisabledRegistries.byPath[path]; registry != nil {
		return registry
	}
	registry := newOrgDisabledRegistry(directory, requests)
	orgDisabledRegistries.byPath[path] = registry
	return registry
}

// newOrgDisabledRegistry 创建登记表并读取账户目录下保存的记录
func newOrgDisabledRegistry(directory string, requests *requestRegistry) *orgDisabledRegistry {
	registry := &orgDisabledRegistry{requests: requests, accounts: make(map[string]*orgDisabledEntry)}
	if strings.TrimSpace(directory) == "" {
		return registry
	}
	registry.path = filepath.Join(directory, orgDisabledFileName)
	if err := registry.load(); err != nil && requests != nil {
		requests.log("service", "WARN", fmt.Sprintf("组织停用账户记录读取失败，重新开始记录 | 错误=%v", err))
	}
	return registry
}

func (registry *orgDisabledRegistry) load() error {
	data, err := os.ReadFile(registry.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored orgDisabledFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	for accountID, entry := range stored.Accounts {
		if entry != nil && strings.TrimSpace(accountID) != "" {
			registry.accounts[accountID] = entry
		}
	}
	return nil
}

// nextCheck 返回下一次长间隔复查的时间：now + orgDisabledRecheck×U(0.9,1.1)，一批同时进入的账户不在同一时刻复查
func (registry *orgDisabledRegistry) nextCheck(now time.Time) time.Time {
	value := rand.Float64()
	if registry != nil && registry.random != nil {
		value = registry.random()
	}
	return now.Add(time.Duration(float64(orgDisabledRecheck) * (0.9 + 0.2*value)))
}

// record 登记一次组织停用，返回是否新登记的账户与下一次复查的时间。identity 为空时保留之前记下的；
// 已登记的账户（复查仍是组织停用、其他路径再次遇到）只更新最近一次的来源、时间、次数与原文，复查时间顺延
func (registry *orgDisabledRegistry) record(accountID, source, message, identity string, now time.Time) (bool, time.Time) {
	if registry == nil {
		return false, time.Time{}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry := registry.accounts[accountID]
	added := entry == nil
	if added {
		entry = &orgDisabledEntry{Source: source, FirstAt: now}
		registry.accounts[accountID] = entry
	}
	if identity != "" {
		entry.Identity = identity
	}
	entry.LastSource = source
	entry.LastAt = now
	entry.Count++
	entry.Message = message
	entry.CheckAt = registry.nextCheck(now)
	entry.RecheckFailures = 0
	registry.scheduleSaveLocked()
	return added, entry.CheckAt
}

// postpone 把账户下一次长间隔复查顺延到 at（只往后，不提前），记下复查连续没有结论的次数，返回顺延后的复查时间；
// 账户不在登记表里时什么都不做、返回 at（复查遇到其他失败时按退避顺延，见 catalogSyncer.orgRecheckFailedLocked）
func (registry *orgDisabledRegistry) postpone(accountID string, at time.Time, failures int) time.Time {
	if registry == nil {
		return at
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry := registry.accounts[accountID]
	if entry == nil {
		return at
	}
	entry.CheckAt = laterOf(entry.CheckAt, at)
	entry.RecheckFailures = failures
	registry.scheduleSaveLocked()
	return entry.CheckAt
}

// entryOf 返回账户的记录副本与是否登记
func (registry *orgDisabledRegistry) entryOf(accountID string) (orgDisabledEntry, bool) {
	if registry == nil {
		return orgDisabledEntry{}, false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	entry := registry.accounts[accountID]
	if entry == nil {
		return orgDisabledEntry{}, false
	}
	return *entry, true
}

// setIdentity 更新账户记下的登录签名指纹（本服务自己续签或写回换掉了登录签名 Cookie）；账户不在登记表里时什么都不做
func (registry *orgDisabledRegistry) setIdentity(accountID string, identity string) {
	if registry == nil || identity == "" {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry := registry.accounts[accountID]
	if entry == nil || entry.Identity == identity {
		return
	}
	entry.Identity = identity
	registry.scheduleSaveLocked()
}

// identities 返回登记表里的全部账户与登记时记下的登录签名指纹
func (registry *orgDisabledRegistry) identities() map[string]string {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	identities := make(map[string]string, len(registry.accounts))
	for accountID, entry := range registry.accounts {
		identities[accountID] = entry.Identity
	}
	return identities
}

// clearAccount 清除账户的记录，返回是否清除了
func (registry *orgDisabledRegistry) clearAccount(accountID string) bool {
	if registry == nil {
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

// matching 返回登记表里与给定账户 ID 对应的账户（all 为真时为全部）。账户 ID 不区分大小写（同 permissionDeniedRegistry.clear）
func (registry *orgDisabledRegistry) matching(accountIDs []string, all bool) []string {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	matched := make([]string, 0)
	for accountID := range registry.accounts {
		if all || slices.ContainsFunc(accountIDs, func(requested string) bool {
			return requested == accountID || strings.EqualFold(requested, accountID)
		}) {
			matched = append(matched, accountID)
		}
	}
	sort.Strings(matched)
	return matched
}

// orgDisabledListEntry 为列表中的一个账户
type orgDisabledListEntry struct {
	accountID string
	entry     orgDisabledEntry
}

// list 返回全部记录，按最近一次从新到旧排列
func (registry *orgDisabledRegistry) list() []orgDisabledListEntry {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	entries := make([]orgDisabledListEntry, 0, len(registry.accounts))
	for accountID, entry := range registry.accounts {
		entries = append(entries, orgDisabledListEntry{accountID: accountID, entry: *entry})
	}
	registry.mu.RUnlock()
	sort.Slice(entries, func(left int, right int) bool {
		if !entries[left].entry.LastAt.Equal(entries[right].entry.LastAt) {
			return entries[left].entry.LastAt.After(entries[right].entry.LastAt)
		}
		return entries[left].accountID < entries[right].accountID
	})
	return entries
}

// count 返回登记的账户数
func (registry *orgDisabledRegistry) count() int {
	if registry == nil {
		return 0
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return len(registry.accounts)
}

func (registry *orgDisabledRegistry) scheduleSaveLocked() {
	registry.dirty = true
	if registry.path == "" || registry.saveTimer != nil {
		return
	}
	registry.saveTimer = time.AfterFunc(orgDisabledSaveDelay, registry.save)
}

// save 合并几秒内的多次变化后写一次文件：锁内只复制一份快照，编码与写盘在锁外（同 permissionDeniedRegistry.save）
func (registry *orgDisabledRegistry) save() {
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
	snapshot := make(map[string]*orgDisabledEntry, len(registry.accounts))
	for accountID, entry := range registry.accounts {
		copied := *entry
		snapshot[accountID] = &copied
	}
	registry.mu.Unlock()
	data, err := json.MarshalIndent(orgDisabledFile{Accounts: snapshot}, "", "  ")
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	if err != nil && registry.requests != nil {
		registry.requests.log("service", "WARN", fmt.Sprintf("组织停用账户记录保存失败 | 错误=%v", err))
	}
}

// flush 立即写入还没保存的变化（生成服务关闭时调用）
func (registry *orgDisabledRegistry) flush() {
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

// orgDisabledEmailPattern 匹配原文里的邮箱，打码为前两位 + ***@***（与页面快照、tools/diag_*.sh 的打码规则一致）
var orgDisabledEmailPattern = regexp.MustCompile(`([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@(?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,}`)

// orgDisabledCauseSummary 把原错误压成一行（连续空白合成一个空格）、邮箱打码，超过 orgDisabledMessageLimit 字时截断；没有错误时为 -
func orgDisabledCauseSummary(cause error) string {
	if cause == nil {
		return "-"
	}
	text := orgDisabledEmailPattern.ReplaceAllString(strings.Join(strings.Fields(cause.Error()), " "), "${1}***@***")
	if runes := []rune(text); len(runes) > orgDisabledMessageLimit {
		text = string(runes[:orgDisabledMessageLimit]) + "…"
	}
	if text == "" {
		return "-"
	}
	return text
}

// reportOrgDisabled 为账户池的组织停用回调（aistudio 内部识别出组织停用、Worker 启动时页面停在组织停用页时调用，
// 见 aistudio.OrgDisabledReporter 与 accountWorkerManager.confirmOrgDisabled）
func (service *trackedService) reportOrgDisabled(accountID string, source string, cause error) {
	if service == nil || service.pool == nil {
		return
	}
	service.enterOrgDisabled(accountID, service.accountLabel(accountID), source, cause)
}

// accountLabel 返回账户的显示名称，账户不存在或没有名称时为账户 ID
func (service *trackedService) accountLabel(accountID string) string {
	if service != nil && service.pool != nil {
		if label := service.pool.Labels([]string{accountID})[accountID]; label != "" {
			return label
		}
	}
	return accountID
}

// enterOrgDisabled 让账户进入组织停用状态（各来源共用）：账户池记标记（不再参与任何调度），登记表记下来源、时间与原文（持久化），
// 驻留 Worker 登记释放（空闲即关，忙碌等请求结束；不再接新请求），模型目录改为长间隔复查。新进入时写一行持久日志
// “账号所属组织已停用 AI Studio，停止调度”，已在这个状态的账户再次遇到时不再写。账户不存在（已删除）时什么都不做。
// 调用方不能持有账户池锁：可能在目录同步、请求的租约内调用（只写标记、不等租约）
func (service *trackedService) enterOrgDisabled(accountID string, label string, source string, cause error) {
	if service == nil || service.pool == nil {
		return
	}
	identity, exists := service.pool.LoginIdentity(accountID)
	if !exists {
		return
	}
	now := time.Now()
	message := orgDisabledCauseSummary(cause)
	added, checkAt := service.orgDisabled.record(accountID, source, message, identity, now)
	marked, err := service.pool.MarkOrgDisabled(accountID, aistudio.OrgDisabledReason)
	if err != nil {
		return
	}
	if checkAt.IsZero() {
		// 没有登记表（测试直接构造的服务）：只在内存里排复查
		checkAt = now.Add(orgDisabledRecheck)
	}
	if (added || marked) && service.requests != nil {
		if strings.TrimSpace(label) == "" {
			label = accountID
		}
		// 持久行：控制台队列满时也不丢，诊断脚本按“来源=”分组
		service.requests.logDurable(label, "WARN", fmt.Sprintf(
			"账号所属组织已停用 AI Studio，停止调度 | 来源=%s | 下次复查=%s | 原文=%s",
			source, checkAt.UTC().Format(time.RFC3339), message,
		))
	}
	service.workers.releaseUnschedulable(accountID, unschedulableOrgDisabled)
	service.catalogSync.noteOrgDisabled(accountID, checkAt)
	if marked {
		service.publishModelAccess()
	}
}

// forgetOrgDisabled 清除账户的组织停用状态（认证文件更新、管理员清除）：登记表与账户池的标记都清除，账户立即恢复调度；
// 模型目录的长间隔复查任务改回常规优先级并立即同步（见 catalogSyncer.orgCleared）：经验证、新账户自动处理或首次目录同步进入的账户
// 本来就没有目录，原先要等原定的复查时间（最长约一天）才同步，期间仍不是任何模型的候选。返回是否清除了（账户没有这个状态时为假，不写日志）
func (service *trackedService) forgetOrgDisabled(accountID string, source string) bool {
	if !service.clearOrgDisabled(accountID, source) {
		return false
	}
	service.catalogSync.orgCleared(accountID)
	return true
}

// clearOrgDisabled 清除账户的组织停用标记与登记，不动模型目录任务（长间隔复查通过时由 afterRefresh 随后按成功排下次刷新）；
// 返回是否清除了（账户没有这个状态时为假，不写日志）
func (service *trackedService) clearOrgDisabled(accountID string, source string) bool {
	if service == nil {
		return false
	}
	removed := service.orgDisabled.clearAccount(accountID)
	cleared := false
	if service.pool != nil {
		cleared = service.pool.ClearOrgDisabled(accountID)
	}
	if !removed && !cleared {
		return false
	}
	if service.requests != nil {
		service.requests.logDurable(service.accountLabel(accountID), "INFO", "已清除组织停用标记，恢复调度 | 来源="+source)
	}
	if cleared {
		service.publishModelAccess()
		service.workers.notifySchedulingIfAny()
	}
	return true
}

// orDash 返回 value，为空时返回 -（日志字段）
func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

// orgDisabledIdentity 返回账户登记时记下的登录签名指纹，以及账户是否列为组织停用账户
func (service *trackedService) orgDisabledIdentity(accountID string) (string, bool) {
	if service == nil {
		return "", false
	}
	entry, listed := service.orgDisabled.entryOf(accountID)
	return entry.Identity, listed
}

// orgDisabledListed 返回账户是否处于组织停用状态（登记表或账户池的标记）
func (service *trackedService) orgDisabledListed(accountID string) bool {
	if service == nil {
		return false
	}
	if _, listed := service.orgDisabled.entryOf(accountID); listed {
		return true
	}
	return service.pool != nil && service.pool.OrgDisabled(accountID)
}

// orgDisabledCheckAt 返回账户下一次长间隔复查的时间；没有登记时为零
func (service *trackedService) orgDisabledCheckAt(accountID string) time.Time {
	checkAt, _ := service.orgDisabledRecheck(accountID)
	return checkAt
}

// orgDisabledRecheck 返回账户下一次长间隔复查的时间与复查连续没有结论的次数；没有登记时为零
func (service *trackedService) orgDisabledRecheck(accountID string) (time.Time, int) {
	if service == nil {
		return time.Time{}, 0
	}
	entry, _ := service.orgDisabled.entryOf(accountID)
	return entry.CheckAt, entry.RecheckFailures
}

// noteLoginIdentity 本服务自己续签或写回换掉了账户的登录签名 Cookie：更新 403 无权限与组织停用两张登记表里记下的指纹，
// 不当作重新上传（见 authRuntimeRefresher.renewed 与 deniedAuthUpdated）
func (service *trackedService) noteLoginIdentity(accountID string, identity string) {
	if service == nil {
		return
	}
	service.permissionDenied.setIdentity(accountID, identity)
	service.orgDisabled.setIdentity(accountID, identity)
}

// watchedLogin 返回账户是否在 403 无权限或组织停用登记表里（目录扫描对它们另按文件内容判断认证文件是否更新，见 deniedAuthUpdated）
func (service *trackedService) watchedLogin(accountID string) bool {
	if _, listed := service.permissionDeniedIdentity(accountID); listed {
		return true
	}
	_, listed := service.orgDisabledIdentity(accountID)
	return listed
}

// reconcileOrgDisabled 在启动时（账户池与登记表都已载入后）对照登记表与账户池：不在账户池里的账户（服务停止期间删除了目录）
// 的记录直接清除；登录签名指纹与登记时不同的账户（服务停止期间替换了认证文件）按认证文件更新清除；其余恢复账户池的标记，
// 重启后仍不参与调度（与 reconcilePermissionDenied 同一识别方式）
func (service *trackedService) reconcileOrgDisabled() {
	if service == nil || service.pool == nil || service.orgDisabled == nil {
		return
	}
	missing, restored := 0, 0
	for accountID, identity := range service.orgDisabled.identities() {
		current, exists := service.pool.LoginIdentity(accountID)
		switch {
		case !exists:
			if service.orgDisabled.clearAccount(accountID) {
				missing++
			}
		case identity != "" && current != identity:
			service.forgetOrgDisabled(accountID, "启动时发现认证文件已更新")
		default:
			if _, err := service.pool.MarkOrgDisabled(accountID, aistudio.OrgDisabledReason); err == nil {
				restored++
			}
		}
	}
	if service.requests == nil {
		return
	}
	if missing > 0 {
		service.requests.log("service", "INFO", fmt.Sprintf("清除已不在账户池中的账户的组织停用记录 | 账户=%d", missing))
	}
	if restored > 0 {
		service.requests.log("service", "INFO", fmt.Sprintf("恢复组织停用账户的标记，不参与调度 | 账户=%d", restored))
	}
}

// OrgDisabled 返回组织停用账户列表，按最近一次从新到旧排列
func (admin *runtimeAdmin) OrgDisabled(context.Context) ([]api.AdminOrgDisabled, error) {
	var entries []orgDisabledListEntry
	if admin.service != nil {
		entries = admin.service.orgDisabled.list()
	}
	result := make([]api.AdminOrgDisabled, 0, len(entries))
	var labels map[string]string
	if admin.pool != nil && len(entries) > 0 {
		accountIDs := make([]string, 0, len(entries))
		for _, entry := range entries {
			accountIDs = append(accountIDs, entry.accountID)
		}
		labels = admin.pool.Labels(accountIDs)
	}
	for _, item := range entries {
		label := item.accountID
		if value := labels[item.accountID]; value != "" {
			label = value
		}
		lastSource := item.entry.LastSource
		if lastSource == "" {
			lastSource = item.entry.Source
		}
		result = append(result, api.AdminOrgDisabled{
			ID: item.accountID, Label: label, Source: item.entry.Source, LastSource: lastSource,
			Count: item.entry.Count, FirstAt: item.entry.FirstAt, LastAt: item.entry.LastAt,
			CheckAt: item.entry.CheckAt, Message: item.entry.Message,
		})
	}
	return result, nil
}

// ClearOrgDisabled 按管理页的操作清除组织停用账户的状态，返回清除的账户数：清除后立即恢复调度，模型目录随即复查一次
// （forgetOrgDisabled 改回常规优先级、立即到期；仍是组织停用时重新进入）。账户 ID 只去掉首尾空白，匹配时不区分大小写（同 ClearPermissionDenied）
func (admin *runtimeAdmin) ClearOrgDisabled(_ context.Context, input api.PermissionDeniedClearInput) (int, error) {
	accountIDs := make([]string, 0, len(input.AccountIDs))
	for _, accountID := range input.AccountIDs {
		if accountID = strings.TrimSpace(accountID); accountID != "" {
			accountIDs = append(accountIDs, accountID)
		}
	}
	if !input.All && len(accountIDs) == 0 {
		return 0, &adminOperationError{status: http.StatusBadRequest, code: "invalid_request", message: "需要 account_ids 或 all"}
	}
	if admin.service == nil {
		return 0, nil
	}
	cleared := 0
	for _, accountID := range admin.service.orgDisabled.matching(accountIDs, input.All) {
		if admin.service.forgetOrgDisabled(accountID, "管理员清除") {
			cleared++
		}
	}
	if cleared > 0 {
		admin.requests.log("auth", "INFO", fmt.Sprintf("已清除组织停用账户的状态 | 账户=%d", cleared))
	}
	return cleared, nil
}
