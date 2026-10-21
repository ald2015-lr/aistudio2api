package aistudio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	appconfig "github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/gofrs/flock"
)

const (
	accountConfigName = "account.json"
	storageStateName  = "storage-state.json"
	runtimeStateName  = "runtime-state.json"
	globalCooldownKey = "*"
	externalLeasePoll = 100 * time.Millisecond
	runtimeLockPoll   = 25 * time.Millisecond
	runtimeLockLimit  = 2 * time.Second
)

// AccountState 表示账户当前是否可调度
type AccountState string

const (
	// AccountReady 表示账户可以接收请求
	AccountReady AccountState = "ready"
	// AccountBusy 表示账户存在活动请求
	AccountBusy AccountState = "busy"
	// AccountCooldown 表示账户或模型处于冷却期
	AccountCooldown AccountState = "cooldown"
	// AccountAuthRequired 表示账户需要重新登录
	AccountAuthRequired AccountState = "auth_required"
	// AccountUnavailable 表示账户初始化或运行失败
	AccountUnavailable AccountState = "unavailable"
	// AccountDisabled 表示账户已停用
	AccountDisabled AccountState = "disabled"
)

var (
	// ErrInvalidArgument 表示请求参数在发送前已确定无效
	ErrInvalidArgument = errors.New("AI Studio 请求参数无效")
	// ErrModelNotFound 表示实时目录中不存在请求模型
	ErrModelNotFound = errors.New("AI Studio 实时目录中没有请求模型")
	// ErrNoEligibleAccount 表示号池中没有任何账户能处理请求的模型、方法、能力、通道或绑定资源，属于请求本身的原因，
	// 对外按 4xx 返回。号池一侧的暂时性原因（账户需要重新登录、已停用、被占用，号池为空或模型目录尚未加载）
	// 返回 AccountsNotReadyError，全部冷却返回 AllCoolingError；前者同样满足 errors.Is(err, ErrNoEligibleAccount)
	ErrNoEligibleAccount = errors.New("没有符合条件的 AI Studio 账户")
	// ErrAccountNotFound 表示稳定账户 ID 不存在
	ErrAccountNotFound = errors.New("账户不存在")
	// ErrAccountLeased 表示账户当前存在进程内或跨进程租约
	ErrAccountLeased = errors.New("账户正在使用")
	// ErrAccountCoolingDown 表示租约账户在请求发出前进入冷却
	ErrAccountCoolingDown = errors.New("账户已进入冷却")
	// ErrResourceNotFound 表示资源没有创建账户映射
	ErrResourceNotFound = errors.New("资源账户映射不存在")
	errAccountLeaseBusy = ErrAccountLeased
)

// AccountCoolingDownError 表示租约账户在请求发出前进入冷却（多半是同账号别的请求刚遇到额度 429）：这次请求没有发到上游。
// errors.Is(err, ErrAccountCoolingDown) 成立；对外与全部冷却一样按 429 返回，Retry-After 为拦下它的冷却的恢复时间
type AccountCoolingDownError struct {
	AccountID string
	ModelID   string
	// Until 与 Reason 为拦下请求的冷却（全局 * 与该通道该模型中恢复较晚的一个，见 accountCooldown）
	Until  time.Time
	Reason string
}

func (e *AccountCoolingDownError) Error() string {
	// 冷却原因形如“分钟限额: <上游原文>”，文案只写冷却类型
	kind, _, _ := strings.Cut(e.Reason, ":")
	if kind = strings.TrimSpace(kind); kind != "" {
		kind += "，"
	}
	return fmt.Sprintf("%s（%s恢复时间 %s，请求没有发到上游）", ErrAccountCoolingDown.Error(), kind, e.Until.Local().Format(time.RFC3339))
}

// Is 让 errors.Is(err, ErrAccountCoolingDown) 成立：调用方按哨兵判断“发出前已冷却”
func (e *AccountCoolingDownError) Is(target error) bool {
	return target == ErrAccountCoolingDown
}

// RetryAt 返回拦下请求的冷却的恢复时间，供对外错误写 Retry-After
func (e *AccountCoolingDownError) RetryAt() time.Time {
	return e.Until
}

// HTTPStatus 返回发出前已冷却对应的公开状态码：与全部冷却一样按限流 429 返回
func (e *AccountCoolingDownError) HTTPStatus() int {
	return http.StatusTooManyRequests
}

// ErrorCode 返回 OpenAI 兼容的限流错误代码
func (e *AccountCoolingDownError) ErrorCode() string {
	return "rate_limit_exceeded"
}

// AllCoolingError 表示请求的全部候选账户都在冷却且不会很快恢复
type AllCoolingError struct {
	ModelID string
	Until   time.Time
}

func (e *AllCoolingError) Error() string {
	return fmt.Sprintf("模型 %s 的可用账户均在冷却，最早恢复时间 %s", e.ModelID, e.Until.Local().Format(time.RFC3339))
}

// RetryAt 返回最早恢复时间，供对外错误写 Retry-After
func (e *AllCoolingError) RetryAt() time.Time {
	return e.Until
}

// HTTPStatus 返回额度耗尽对应的公开状态码
func (e *AllCoolingError) HTTPStatus() int {
	return http.StatusTooManyRequests
}

// ErrorCode 返回 OpenAI 兼容的额度错误代码
func (e *AllCoolingError) ErrorCode() string {
	return "rate_limit_exceeded"
}

// CapacityBusyError 表示主用模型（PRIMARY_MODELS 或 ULTRA_PRIMARY_MODELS）的请求在排队上限（PRIMARY_QUEUE_WAIT 或
// ULTRA_PRIMARY_QUEUE_WAIT）内没有等到空闲的账号或 Worker 槽位：
// 号池容量（峰值 Worker 数 × 单账户并发）低于当前并发时，请求不再排队到请求超时，立即按 429 返回并带 Retry-After，
// 由客户端稍后重试。只在等待原因是账号忙碌、没有空闲槽位、Worker 启动或扩容中时产生；候选全部冷却仍按 AllCoolingError 处理
type CapacityBusyError struct {
	ModelID string
	// Pool 为请求限定的号池；Ultra 号池的错误写明 Ultra 号池
	Pool PoolScope
	// Reason 为最后一次等待的原因（账号全部忙碌、没有空闲的 Worker 槽位等）
	Reason string
	// Waited 为计入排队上限的时间（整个请求共用一份预算：总耗时减去请求自己现场冷启动与候选冷却的等待），Limit 为排队上限
	Waited time.Duration
	Limit  time.Duration
	// Elapsed 为请求从第一次选号起的总耗时（含失败的尝试、换号退避与现场冷启动），ColdStart 为其中请求自己现场冷启动的时长，
	// Attempt 为返回时是第几次尝试；为 0 的项不写进错误文案
	Elapsed   time.Duration
	ColdStart time.Duration
	Attempt   int
	// Until 为建议客户端重试的时间，供对外错误写 Retry-After
	Until time.Time
}

func (e *CapacityBusyError) Error() string {
	pool := ""
	if e.Pool == PoolScopeUltra {
		pool = e.Pool.Label() + "，"
	}
	detail := ""
	if e.Elapsed > 0 {
		detail = fmt.Sprintf("，总耗时 %s", roundBusyDuration(e.Elapsed))
		if e.ColdStart > 0 {
			detail += fmt.Sprintf("（含现场冷启动 %s）", roundBusyDuration(e.ColdStart))
		}
	}
	if e.Attempt > 0 {
		detail += fmt.Sprintf("，第 %d 次尝试", e.Attempt)
	}
	return fmt.Sprintf("号池繁忙：模型 %s 暂时没有空闲的账号或 Worker，请稍后重试（%s已排队 %s，上限 %s%s，最后等待原因：%s）",
		e.ModelID, pool, roundBusyDuration(e.Waited), e.Limit, detail, e.Reason)
}

// roundBusyDuration 为号池繁忙文案里的时长取整：1 秒以上到 0.1 秒，以下到毫秒
func roundBusyDuration(value time.Duration) time.Duration {
	if value >= time.Second {
		return value.Round(100 * time.Millisecond)
	}
	return value.Round(time.Millisecond)
}

// RetryAt 返回建议重试时间，供对外错误写 Retry-After
func (e *CapacityBusyError) RetryAt() time.Time {
	return e.Until
}

// HTTPStatus 返回号池繁忙对应的公开状态码：与全部冷却一样按限流 429 返回
func (e *CapacityBusyError) HTTPStatus() int {
	return http.StatusTooManyRequests
}

// ErrorCode 返回 OpenAI 兼容的限流错误代码
func (e *CapacityBusyError) ErrorCode() string {
	return "rate_limit_exceeded"
}

// PrimaryQuotaError 表示 Ultra 主用模型（ULTRA_PRIMARY_MODELS）的请求因额度不足立即结束：额度 429 的换号次数已达上限
// （每日类与限流类各有上限，见 QuotaHits、RateHits）、其余候选全部冷却、额度 429 后号池里没有已启动的 Worker（只剩要现场冷启动的备用账号），
// 或没有其他可换的账号。对外与全部冷却一样按 429 返回并带 Retry-After，由客户端稍后重试，不在号池里挨个换号、现场冷启动直到超时。
// Cause 为最后一次上游额度错误，只写进内部日志：刻意不实现 Unwrap，否则会被当成上游 RPCError（对外原文、没有 Retry-After）
// 或输入超限等错误处理
type PrimaryQuotaError struct {
	ModelID string
	// Pool 为请求限定的号池
	Pool PoolScope
	// Reason 为结束的原因（额度失败已达换号上限、其余候选全部冷却、额度 429 后没有已启动的 Worker 等）
	Reason string
	// QuotaHits 为本请求遇到的每日类额度失败的次数，RateHits 为限流类额度失败（分钟限额、限流）的次数，
	// Elapsed 为请求从第一次选号起的总耗时
	QuotaHits int
	RateHits  int
	Elapsed   time.Duration
	// Until 为建议客户端重试的时间，供对外错误写 Retry-After
	Until time.Time
	// Cause 为最后一次上游额度错误（可为空）
	Cause error
}

func (e *PrimaryQuotaError) Error() string {
	pool := e.Pool.Label()
	if pool == "" {
		pool = "不限号池"
	}
	message := fmt.Sprintf("额度不足：%s的 Ultra 主用模型 %s 没有可用额度，立即返回（原因：%s，额度失败 %d 次",
		pool, e.ModelID, e.Reason, e.QuotaHits)
	if e.RateHits > 0 {
		message += fmt.Sprintf("，限流失败 %d 次", e.RateHits)
	}
	if e.Elapsed > 0 {
		message += fmt.Sprintf("，总耗时 %s", roundBusyDuration(e.Elapsed))
	}
	if !e.Until.IsZero() {
		message += "，建议重试时间 " + e.Until.Local().Format(time.RFC3339)
	}
	message += "）"
	if e.Cause != nil {
		message += "：" + e.Cause.Error()
	}
	return message
}

// RetryAt 返回建议重试时间，供对外错误写 Retry-After
func (e *PrimaryQuotaError) RetryAt() time.Time {
	return e.Until
}

// HTTPStatus 返回额度不足对应的公开状态码：与全部冷却一样按限流 429 返回
func (e *PrimaryQuotaError) HTTPStatus() int {
	return http.StatusTooManyRequests
}

// ErrorCode 返回 OpenAI 兼容的限流错误代码
func (e *PrimaryQuotaError) ErrorCode() string {
	return "rate_limit_exceeded"
}

// AccountsNotReadyError 表示号池暂时无法调度请求：支持请求的账户都需要重新登录、不可用、已停用或被其他进程占用，
// 或者号池为空、模型目录尚未加载、候选账户在调度期间被移除。这些是号池一侧的原因，对外按 503 返回，
// 不能让客户端当成自己的请求错误
type AccountsNotReadyError struct {
	Reasons []string
	// Cause 为触发该错误的内部原因（如账户 runtime 被其他进程占用），可为空
	Cause error
	// Pool 为请求限定的号池；Ultra 号池的错误原因前写明 Ultra 号池（普通号池的措辞与不分号池时相同）
	Pool PoolScope
}

func (e *AccountsNotReadyError) Error() string {
	message := ErrNoEligibleAccount.Error() + "："
	if e.Pool == PoolScopeUltra {
		message += e.Pool.Label() + "："
	}
	message += strings.Join(e.Reasons, "；")
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

// Unwrap 同时保留无账户哨兵与内部原因，调用方仍可用 errors.Is(err, ErrNoEligibleAccount) 判断“没有可用账户”
func (e *AccountsNotReadyError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrNoEligibleAccount}
	}
	return []error{ErrNoEligibleAccount, e.Cause}
}

// HTTPStatus 返回号池暂时不可调度对应的公开状态码
func (e *AccountsNotReadyError) HTTPStatus() int {
	return http.StatusServiceUnavailable
}

// ErrorCode 返回 OpenAI 兼容的账户不可用错误代码
func (e *AccountsNotReadyError) ErrorCode() string {
	return "account_unavailable"
}

// PoolNotReady 返回号池一侧暂时无法调度的无账户错误
func PoolNotReady(reason string, cause error) *AccountsNotReadyError {
	return &AccountsNotReadyError{Reasons: []string{reason}, Cause: cause}
}

// PoolNotReadyIn 返回限定号池的请求暂时无法调度的无账户错误：请求路径上号池已知时使用，/ultra 请求的错误写明 Ultra 号池
func PoolNotReadyIn(scope PoolScope, reason string, cause error) *AccountsNotReadyError {
	notReady := PoolNotReady(reason, cause)
	notReady.Pool = scope
	return notReady
}

const (
	// poolMissingReason 为账户池未初始化时的原因
	poolMissingReason = "账户池未初始化"
	// leaseReplacedReason 为候选账户在调度期间被移除或替换时的原因
	leaseReplacedReason = "候选账户在调度期间被移除或替换"
	// emptyPoolReason 为账户池中没有任何账户时的原因
	emptyPoolReason = "账户池中没有账户"
	// emptyUltraPoolReason 为 Ultra 号池中没有任何账户时的原因
	emptyUltraPoolReason = "没有权益为 Ultra 的账户"
	// emptyNormalPoolReason 为账户都是 Ultra 账户、独占模式下普通路径没有可用账户时的原因
	emptyNormalPoolReason = "账户都是 Ultra 账户，ULTRA_EXCLUSIVE=true 时只服务 /ultra 前缀的请求"
	// emptyUltraPoolWithProReason 为 ULTRA_INCLUDE_PRO=true 时 Ultra 号池中没有任何账户的原因：开关关闭时仍用 emptyUltraPoolReason，
	// 现有客户端与测试可能按原文判断
	emptyUltraPoolWithProReason = "没有权益为 Ultra 或 Pro 的账户"
	// emptyNormalPoolWithProReason 为 ULTRA_INCLUDE_PRO=true 时账户都属于 Ultra 号池、独占模式下普通路径没有可用账户的原因
	emptyNormalPoolWithProReason = "账户都属于 Ultra 号池（权益为 Ultra，或 ULTRA_INCLUDE_PRO=true 时的 Pro），ULTRA_EXCLUSIVE=true 时只服务 /ultra 前缀的请求"
)

// accountStateLabels 为不可调度账户状态的中文说明
var accountStateLabels = map[AccountState]string{
	AccountAuthRequired: "需要重新登录",
	AccountUnavailable:  "不可用",
	AccountDisabled:     "已停用",
}

// AccountConfig 表示账户目录中的固定最小配置
type AccountConfig struct {
	Label    string `json:"label"`
	Enabled  bool   `json:"enabled"`
	Proxy    string `json:"proxy"`
	Locale   string `json:"locale"`
	Timezone string `json:"timezone"`
}

// ResourceBinding 记录上游资源的创建账户
type ResourceBinding struct {
	Kind      string                 `json:"kind,omitempty"`
	Name      string                 `json:"name,omitempty"`
	MIME      string                 `json:"mime,omitempty"`
	Size      int64                  `json:"size,omitempty"`
	Purpose   string                 `json:"purpose,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	Video     *VideoResourceMetadata `json:"video,omitempty"`
}

// VideoResourceMetadata 保存 OpenAI 视频对象的持久字段
type VideoResourceMetadata struct {
	Model   string `json:"model"`
	Seconds string `json:"seconds"`
	Size    string `json:"size"`
}

type accountRuntimeState struct {
	Cooldowns          map[string]CooldownState   `json:"cooldowns,omitempty"`
	Resources          map[string]ResourceBinding `json:"resources,omitempty"`
	ModelAccess        map[string]ModelAccess     `json:"model_access,omitempty"`
	BenefitTier        BenefitTier                `json:"benefit_tier,omitempty"`
	CatalogFingerprint string                     `json:"catalog_fingerprint,omitempty"`
}

// ModelAccessState 表示账户对单个模型的实测调用资格
type ModelAccessState string

const (
	// ModelAccessVerified 表示账户已成功调用模型
	ModelAccessVerified ModelAccessState = "verified"
)

// ModelAccess 保存账户模型资格的实测结果
type ModelAccess struct {
	State     ModelAccessState `json:"state"`
	CheckedAt time.Time        `json:"checked_at"`
	Reason    string           `json:"reason,omitempty"`
}

// Account 表示一个稳定目录对应的 AI Studio 账户
type Account struct {
	ID                    string        `json:"id"`
	Directory             string        `json:"-"`
	ConfigPath            string        `json:"-"`
	StoragePath           string        `json:"-"`
	RuntimePath           string        `json:"-"`
	Config                AccountConfig `json:"config"`
	StorageState          StorageState  `json:"-"`
	Models                []Model       `json:"models,omitempty"`
	BenefitTier           BenefitTier   `json:"benefit_tier"`
	State                 AccountState  `json:"state"`
	LastUsed              time.Time     `json:"last_used,omitempty"`
	runtime               accountRuntimeState
	active                int
	exclusive             bool
	exclusiveWaiters      int
	authRefreshers        int
	leaseLock             *flock.Flock
	leasePath             string
	storageMu             sync.Mutex
	runtimeMu             sync.Mutex
	persistenceLocked     bool
	authGeneration        uint64
	authCheckedAt         time.Time
	modelAccessGeneration uint64
	stateMessage          string
	initializedAt         time.Time
	buildModels           []Model
	// modelIndex 按模型 ID 与别名索引 Models（取第一个匹配的位置）；buildGenerates 为 Build 目录中可生成的模型 ID。
	// 目录切片被整体替换时按切片身份判断并重建；均受账户池 mu 保护
	modelIndex          map[string]int
	modelIndexSource    []Model
	buildGenerates      map[string]struct{}
	buildGenerateSource []Model
	// position 为账户在账户池 accounts 中的下标，候选索引按它存放成员状态，账户增删后由索引重建时重新赋值。
	// 以下目录派生缓存（预热模型、可生成模型数、排序后的模型 ID、目录内容键）按目录切片身份与权益判断是否重算。
	// 均受账户池 mu 保护
	position           int
	bootstrapModels    []string
	bootstrapSource    []Model
	bootstrapTier      BenefitTier
	warmCoverage       int
	warmCoverageSource []Model
	warmCoverageTier   BenefitTier
	statusModels       []string
	statusModelsSource []Model
	catalogKey         uint64
	catalogKeySource   []Model
	// draining 为排空中（见 SetDraining）：不固定账户的选号不再给它新租约，分类时按忙碌处理；
	// 指定账户与文件绑定的请求照常使用。只影响调度，不改变对外状态、Activity 与预热概况。受账户池 mu 保护
	draining bool
	// rate 为最近一个窗口的生成请求记录（每账号每分钟限速，见 rate_limit.go），第一次取得生成租约时创建；受账户池 mu 保护
	rate *accountRate
}

// AccountStatus 表示管理界面使用的脱敏账户状态
type AccountStatus struct {
	ID          string       `json:"id"`
	Label       string       `json:"label"`
	State       AccountState `json:"state"`
	Enabled     bool         `json:"enabled"`
	Proxy       string       `json:"proxy"`
	Locale      string       `json:"locale"`
	Timezone    string       `json:"timezone"`
	Models      []string     `json:"models"`
	BenefitTier string       `json:"benefit_tier"`
	// BenefitTierKnown 表示账户权益已经从官网读取过（运行态里有目录指纹）；尚未读取的账户按 Free 显示，属于普通号池
	BenefitTierKnown bool `json:"benefit_tier_known"`
	// Pool 为账户当前所属的号池（成员关系，随 ULTRA_INCLUDE_PRO 变化）：ultra 或 normal；BenefitTier 始终是真实权益
	Pool      string                   `json:"pool"`
	Cooldowns map[string]CooldownState `json:"-"`
	LastUsed  *time.Time               `json:"last_used,omitempty"`
	Message   string                   `json:"message,omitempty"`
}

// AccountSelection 描述账户调度所需的能力或粘性条件
type AccountSelection struct {
	ModelID           string
	ModelAccessScope  string
	Method            string
	Capability        string
	AccountID         string
	ResourceID        string
	AllowedAccountIDs []string
	// ExcludedAccountIDs 为不能使用的账户，换号重试时排除已尝试过的账户（通常只有几个）。与 AllowedAccountIDs 不同，
	// 只有排除列表时仍按候选索引分类与选号，只跳过这些账户的下标，不为整个号池建允许集合；
	// 排除列表不进候选集的键，也不进调度等待队列的键（见 app/dispatch.go），换号重试的请求与同模型的请求同队排队
	ExcludedAccountIDs []string
	PlaygroundOnly     bool
	// BuildOnly 只使用 Build 通道（Playground 对输入长度另有上限时改走 Build）
	BuildOnly bool
	// PlaygroundFirst 只用 Playground 通道选号（降级判定：该通道每块都带累计正文数，判定最准）；
	// 没有可用的 Playground 账号时由调用方去掉该标记重新选号，退回其他通道
	PlaygroundFirst bool
	// PreferPlayground 只调整候选顺序，不限定通道：每个账户先 Playground 后其他通道，轮询只在账户之间进行
	// （流式优先 Playground 的请求改为不限通道选号时使用：两个通道共用账户并发，账户空闲时它的 Playground 同样空闲）
	PreferPlayground bool
	// Pool 为可以使用的号池；不限号池时由 AcquireFor 等入口取 context 中请求的号池（见 PoolScope）
	Pool PoolScope
}

const preferredBootstrapModelID = "gemini-flash-latest"

// ModelAccessKey 返回关联真实目录模型的独立资格键
func ModelAccessKey(scope string, modelID string) string {
	scope = strings.TrimSpace(scope)
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if scope == "" || modelID == "" {
		return modelID
	}
	return scope + ":" + modelID
}

// AccountCandidateGroups 表示 warm 与 standby 账户的实时可调度状态
type AccountCandidateGroups struct {
	WarmReady        []string
	WarmAvailable    []string
	WarmBusy         []string
	StandbyReady     []string
	StandbyBusy      []string
	EarliestCooldown time.Time
	Eligible         bool
	// RateLimited 为因每分钟请求已满（ACCOUNT_RPM_LIMIT）而按忙碌分组的候选数，冷却中与被排除的不算；WarmRateLimited 为其中的热账户数
	RateLimited     int
	WarmRateLimited int
}

// AccountCandidateState 表示账户候选的实时调度指标
type AccountCandidateState struct {
	ModelAccess   ModelAccessState
	Active        int
	AvailableSlot int
}

// AccountStore 从一个或多个账户文件或目录加载账户
type AccountStore struct {
	paths []string
}

// AccountPool 在账户之间执行能力与并发槽位调度
type AccountPool struct {
	mu                    timedMutex
	accounts              []*Account
	byID                  map[string]*Account
	resources             map[string]string
	perAccountConcurrency int
	routingStrategy       string
	lastPicked            map[string]string
	lastPickedChannel     map[string]Channel
	channels              []Channel
	changed               chan struct{}
	// selectionRefreshAt 记录各模型最近一次从磁盘刷新运行态的时间，用于限频
	selectionRefreshMu sync.Mutex
	selectionRefreshAt map[string]time.Time
	// catalogVersion 在账户增删或 Playground 目录变化时递增；playgroundIndex 是按模型 ID 与别名
	// 建立的 Playground 目录索引（取账户顺序中第一个匹配的模型），版本变化时才重建。均受 mu 保护
	catalogVersion         uint64
	playgroundIndex        map[string]Model
	playgroundIndexVersion uint64
	playgroundIndexBuiltAt time.Time
	// accountsVersion 在账户增删时递增；sortedIndices 为按账户 ID 排序的 accounts 下标，版本变化时才重建
	accountsVersion      uint64
	sortedIndices        []int
	sortedIndicesVersion uint64
	// sched 为候选索引（见 scheduling_index.go），nil 表示下次调度时重建；eligibilityVersion 在影响"号池内哪些账户
	// 支持哪些模型"的变化时递增（账户增删、目录或权益变化、启用状态、Build 目录、上游通道、号池划分规则），按版本缓存的目录汇总、
	// 别名解析与模型通道（见 pool_catalog_cache.go）在版本变化后重建；ultraAccounts 为当前属于 Ultra 号池的账户（见 ultraPoolTier）。
	// statusCache 为账户多时缓存的状态列表
	sched              *schedulingIndex
	eligibilityVersion uint64
	catalogSummaries   map[PoolScope]*catalogSummary
	canonicalIndexes   map[PoolScope]*canonicalIndex
	modelChannelsMemos map[PoolScope]*modelChannelsMemo
	ultraAccounts      map[string]struct{}
	// ultraIncludePro 为 ULTRA_INCLUDE_PRO：为真时权益为 Pro 的账户也属于 Ultra 号池（Worker 分区）；受 mu 保护
	ultraIncludePro bool
	// ultraSnapshot 为 UltraAccountIDs 返回的只读副本，nil 表示成员变化后尚未重建；受 mu 保护
	ultraSnapshot map[string]struct{}
	// poolRuleVersion 在号池划分规则每次实际切换时加一，无锁读取，供 app 层按版本作废派生缓存
	poolRuleVersion atomic.Uint64
	statusCache     []AccountStatus
	statusCacheAt   time.Time
	// background 记录后台运行态写入（见 pool_background.go），WaitBackground 等待它们结束
	background backgroundWrites
	// accountRateLimit 为单账户每分钟生成请求上限（ACCOUNT_RPM_LIMIT，0 关闭，构造时为 0，由 app 层 SetAccountRateLimit 设置）；
	// rateWindow 为滑动窗口，只给测试改小，为 0 时取 accountRateWindow。rateFull 为截止时刻为有限值的满窗账户，rateWake 为指向
	// 其中最早窗口释放时刻的计时器，rateWakeAt 为它的触发时刻（UnixNano，0 为没有设），见 rate_limit.go。均受 mu 保护
	accountRateLimit int
	rateWindow       time.Duration
	rateFull         map[*Account]struct{}
	rateWake         *time.Timer
	rateWakeAt       int64
}

// AccountLease 表示一个账户请求槽位
type AccountLease struct {
	pool                  *AccountPool
	account               *Account
	exclusive             bool
	authGeneration        uint64
	modelAccessGeneration uint64
	checkedAt             time.Time
	refreshRuntime        bool
	channel               Channel
	operation             sync.Mutex
	released              bool
	once                  sync.Once
	err                   error
	// ratePending 为本租约预占的每分钟名额还没被发送或释放抵掉，受账户池 mu 保护；rateFilled 为取得租约时账户的窗口被它占满
	// （是否写“已满”日志见 ClaimRateWindowNote），只在创建时写入
	ratePending bool
	rateFilled  bool
}

// AccountRuntimeLease 保证同一邮箱只有一个 WAA runtime
type AccountRuntimeLease struct {
	lock *flock.Flock
	once sync.Once
	err  error
}

// AccountPublishLease 保护新账户从稳定目录发布到运行时
type AccountPublishLease struct {
	account     *Account
	requestLock *flock.Flock
	runtimeLock *flock.Flock
	once        sync.Once
	err         error
}

// DefaultAccountConfig 返回新账户的最小配置
func DefaultAccountConfig(label string) AccountConfig {
	return AccountConfig{
		Label:    strings.TrimSpace(label),
		Enabled:  true,
		Locale:   DefaultAccountLocale(),
		Timezone: DefaultAccountTimezone(),
	}
}

// NewAccountStore 创建账户目录存储
func NewAccountStore(paths ...string) *AccountStore {
	if len(paths) == 0 {
		paths = []string{"auth"}
	}
	cleaned := make([]string, 0, len(paths))
	for _, value := range paths {
		value = strings.TrimSpace(value)
		if value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return &AccountStore{paths: cleaned}
}

// Load 扫描账户目录并恢复冷却与资源粘性
func (s *AccountStore) Load() ([]*Account, error) {
	if s == nil || len(s.paths) == 0 {
		return nil, fmt.Errorf("账户路径为空")
	}
	directories := make([]string, 0)
	for _, source := range s.paths {
		absolute, err := filepath.Abs(source)
		if err != nil {
			return nil, fmt.Errorf("解析账户路径 %q: %w", source, err)
		}
		info, err := os.Stat(absolute)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读取账户路径 %q: %w", source, err)
		}
		if !info.IsDir() {
			if filepath.Base(absolute) != storageStateName {
				return nil, fmt.Errorf("账户文件必须命名为 %s", storageStateName)
			}
			directories = append(directories, filepath.Dir(absolute))
			continue
		}
		if fileExists(filepath.Join(absolute, storageStateName)) || fileExists(filepath.Join(absolute, accountConfigName)) {
			directories = append(directories, absolute)
			continue
		}
		entries, err := os.ReadDir(absolute)
		if err != nil {
			return nil, fmt.Errorf("扫描账户目录 %q: %w", source, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if strings.HasPrefix(entry.Name(), ".account-") && strings.HasSuffix(entry.Name(), ".tmp") {
				continue
			}
			directory := filepath.Join(absolute, entry.Name())
			if fileExists(filepath.Join(directory, storageStateName)) || fileExists(filepath.Join(directory, accountConfigName)) {
				directories = append(directories, directory)
			}
		}
	}
	sort.Strings(directories)

	accounts := make([]*Account, 0, len(directories))
	ids := make(map[string]struct{}, len(directories))
	resources := make(map[string]string)
	for _, directory := range directories {
		account, err := loadAccount(directory)
		if err != nil {
			return nil, err
		}
		if _, exists := ids[account.ID]; exists {
			return nil, fmt.Errorf("账户 ID 重复: %s", account.ID)
		}
		ids[account.ID] = struct{}{}
		for resourceID := range account.runtime.Resources {
			if owner, exists := resources[resourceID]; exists {
				return nil, fmt.Errorf("资源 %s 同时绑定账户 %s 和 %s", resourceID, owner, account.ID)
			}
			resources[resourceID] = account.ID
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

// Create 创建并锁定以认证邮箱命名的账户目录
func (s *AccountStore) Create(accountConfig AccountConfig, state StorageState) (*Account, *AccountPublishLease, error) {
	if s == nil || len(s.paths) != 1 {
		return nil, nil, fmt.Errorf("创建账户需要一个账户根目录")
	}
	if err := accountConfig.Validate(); err != nil {
		return nil, nil, err
	}
	if err := state.Validate(); err != nil {
		return nil, nil, err
	}
	root, err := filepath.Abs(s.paths[0])
	if err != nil {
		return nil, nil, fmt.Errorf("解析账户根目录: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, nil, fmt.Errorf("创建账户根目录: %w", err)
	}
	id, err := accountEmailID(accountConfig, state)
	if err != nil {
		return nil, nil, err
	}
	accountConfig.Label = id
	temporary, err := os.MkdirTemp(root, ".account-*.tmp")
	if err != nil {
		return nil, nil, fmt.Errorf("创建临时账户目录: %w", err)
	}
	defer os.RemoveAll(temporary)
	if err := writeAccountConfig(filepath.Join(temporary, accountConfigName), accountConfig); err != nil {
		return nil, nil, err
	}
	if err := WriteStorageState(filepath.Join(temporary, storageStateName), state); err != nil {
		return nil, nil, err
	}
	directory := filepath.Join(root, id)
	account, err := loadAccount(temporary)
	if err != nil {
		return nil, nil, err
	}
	account.ID = id
	account.Directory = directory
	account.ConfigPath = filepath.Join(directory, accountConfigName)
	account.StoragePath = filepath.Join(directory, storageStateName)
	account.RuntimePath = filepath.Join(directory, runtimeStateName)
	publishLease, err := acquireAccountPublishLease(account, false)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Rename(temporary, directory); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("保存账户目录: %w", err), publishLease.Release())
	}
	if err := validatePersistentAccountFiles(account); err != nil {
		return nil, nil, errors.Join(err, os.RemoveAll(directory), publishLease.Release())
	}
	return account, publishLease, nil
}

// Delete 删除属于当前存储的稳定账户目录
func (s *AccountStore) Delete(account *Account) error {
	if account == nil || strings.TrimSpace(account.ID) == "" || strings.TrimSpace(account.Directory) == "" {
		return fmt.Errorf("账户未初始化")
	}
	directory, err := filepath.Abs(account.Directory)
	if err != nil {
		return fmt.Errorf("解析账户目录: %w", err)
	}
	if filepath.Base(directory) != account.ID {
		return fmt.Errorf("账户目录与稳定 ID 不匹配")
	}
	owned, err := s.ownsDirectory(directory)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("账户目录不属于当前 AccountStore: %s", directory)
	}
	info, err := os.Stat(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取账户目录: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("账户路径不是目录: %s", directory)
	}
	account.storageMu.Lock()
	lockedByPool := account.persistenceLocked
	account.storageMu.Unlock()
	if lockedByPool {
		if err := os.RemoveAll(directory); err != nil {
			return fmt.Errorf("删除账户目录: %w", err)
		}
		return nil
	}
	leaseLock, _, err := acquireAccountFileLease(account.StoragePath)
	if errors.Is(err, errAccountLeaseBusy) {
		return fmt.Errorf("%w: %s", ErrAccountLeased, account.ID)
	}
	if err != nil {
		return err
	}
	account.runtimeMu.Lock()
	runtimeLock, runtimeErr := lockRuntimeState(context.Background(), account)
	if runtimeErr != nil {
		account.runtimeMu.Unlock()
		if leaseLock != nil {
			_ = leaseLock.Unlock()
		}
		return runtimeErr
	}
	deleteErr := os.RemoveAll(directory)
	if runtimeLock != nil {
		deleteErr = errors.Join(deleteErr, runtimeLock.Unlock())
	}
	account.runtimeMu.Unlock()
	if leaseLock != nil {
		deleteErr = errors.Join(deleteErr, leaseLock.Unlock())
	}
	if deleteErr != nil {
		return fmt.Errorf("删除账户目录: %w", deleteErr)
	}
	return nil
}

func (s *AccountStore) ownsDirectory(directory string) (bool, error) {
	if s == nil || len(s.paths) == 0 {
		return false, fmt.Errorf("账户路径为空")
	}
	for _, source := range s.paths {
		absolute, err := filepath.Abs(source)
		if err != nil {
			return false, fmt.Errorf("解析账户路径 %q: %w", source, err)
		}
		info, err := os.Stat(absolute)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("读取账户路径 %q: %w", source, err)
		}
		root := absolute
		if !info.IsDir() {
			root = filepath.Dir(absolute)
		}
		relative, err := filepath.Rel(root, directory)
		if err != nil {
			return false, fmt.Errorf("比较账户路径 %q: %w", source, err)
		}
		if relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true, nil
		}
	}
	return false, nil
}

// Validate 校验账户固定配置
func (c AccountConfig) Validate() error {
	if _, err := normalizeAccountEmail(c.Label); err != nil {
		return err
	}
	if strings.TrimSpace(c.Locale) == "" {
		return fmt.Errorf("账户 locale 不能为空")
	}
	if strings.TrimSpace(c.Timezone) == "" {
		return fmt.Errorf("账户 timezone 不能为空")
	}
	if err := appconfig.ValidateProxy(c.Proxy); err != nil {
		return fmt.Errorf("账户 proxy 无效: %w", err)
	}
	return nil
}

// EffectiveProxy 返回账户固定代理或全局代理
func (a *Account) EffectiveProxy(globalProxy string) string {
	if a != nil && strings.TrimSpace(a.Config.Proxy) != "" {
		return strings.TrimSpace(a.Config.Proxy)
	}
	return strings.TrimSpace(globalProxy)
}

// AcceptLanguage 返回账户 locale 对应的请求语言头
func (a *Account) AcceptLanguage() string {
	if a == nil {
		return ""
	}
	locale := strings.TrimSpace(a.Config.Locale)
	language, _, _ := strings.Cut(locale, "-")
	if language == "" || strings.EqualFold(language, locale) {
		return locale
	}
	return locale + "," + strings.ToLower(language) + ";q=0.9"
}

// SupportsModel 判断账户实时目录是否包含模型
func (a *Account) SupportsModel(modelID string) bool {
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if modelID == "" {
		return true
	}
	for _, model := range a.Models {
		if modelMatchesID(model, modelID) && modelAllowedByTier(model, a.BenefitTier) {
			return true
		}
	}
	return false
}

// SupportsMethod 判断账户模型是否声明目标方法
func (a *Account) SupportsMethod(modelID string, method string) bool {
	if strings.TrimSpace(method) == "" {
		return a.SupportsModel(modelID)
	}
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	for _, model := range a.Models {
		if !modelMatchesID(model, modelID) {
			continue
		}
		if !modelAllowedByTier(model, a.BenefitTier) {
			return false
		}
		for _, candidate := range model.Methods {
			if candidate == method {
				return true
			}
		}
	}
	return false
}

func accountSupportsSelection(account *Account, selection AccountSelection) bool {
	modelID := strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	if modelID == "" {
		return true
	}
	model, exists := accountModelLocked(account, modelID)
	if !exists {
		return false
	}
	if !modelAllowedByTier(model, account.BenefitTier) {
		return false
	}
	if strings.TrimSpace(selection.Method) != "" && !hasMethod(model, selection.Method) {
		return false
	}
	return strings.TrimSpace(selection.Capability) == "" || model.Capabilities[selection.Capability]
}

// sameModelSlice 判断两个目录切片是否为同一份（长度相同且指向同一底层数组）
func sameModelSlice(left []Model, right []Model) bool {
	return len(left) == len(right) && (len(left) == 0 || &left[0] == &right[0])
}

// accountModelLocked 返回账户 Playground 目录中按 ID 或别名匹配的第一个模型，与逐个 modelMatchesID 的结果相同。
// 调度时每个账户都要检查一次，原先每次都线性扫描目录；调用方持有账户池 mu
func accountModelLocked(account *Account, modelID string) (Model, bool) {
	if account.modelIndex == nil || !sameModelSlice(account.modelIndexSource, account.Models) {
		index := make(map[string]int, len(account.Models))
		for position, model := range account.Models {
			if _, exists := index[model.ID]; !exists {
				index[model.ID] = position
			}
			for _, alias := range model.CapabilityOptions["aliases"] {
				if _, exists := index[alias]; !exists {
					index[alias] = position
				}
			}
		}
		account.modelIndex = index
		account.modelIndexSource = account.Models
	}
	position, exists := account.modelIndex[modelID]
	if !exists {
		return Model{}, false
	}
	return account.Models[position], true
}

// accountBuildGeneratesLocked 返回账户 Build 目录中声明 generateContent 的模型 ID 集合；调用方持有账户池 mu
func accountBuildGeneratesLocked(account *Account) map[string]struct{} {
	if account.buildGenerates == nil || !sameModelSlice(account.buildGenerateSource, account.buildModels) {
		generates := make(map[string]struct{}, len(account.buildModels))
		for _, model := range account.buildModels {
			if hasMethod(model, "generateContent") {
				generates[model.ID] = struct{}{}
			}
		}
		account.buildGenerates = generates
		account.buildGenerateSource = account.buildModels
	}
	return account.buildGenerates
}

// sortedAccountIndicesLocked 返回按账户 ID 排序的 accounts 下标，账户增删后才重建；调用方持有账户池 mu，不得修改返回值
func (p *AccountPool) sortedAccountIndicesLocked() []int {
	if p.sortedIndices == nil || p.sortedIndicesVersion != p.accountsVersion {
		indices := make([]int, 0, len(p.accounts))
		for index, account := range p.accounts {
			if account != nil {
				indices = append(indices, index)
			}
		}
		sort.Slice(indices, func(left, right int) bool {
			return p.accounts[indices[left]].ID < p.accounts[indices[right]].ID
		})
		p.sortedIndices = indices
		p.sortedIndicesVersion = p.accountsVersion
	}
	return p.sortedIndices
}

// CatalogModels 返回完整目录，每项带有启用账户可以调用的通道
func (p *AccountPool) CatalogModels(models []Model) []Model {
	p.mu.Lock()
	defer p.mu.Unlock()
	catalog := make([]Model, len(models))
	for index, model := range models {
		model.Channels = p.modelChannelsInLocked(model, PoolScopeAll)
		catalog[index] = model
	}
	return catalog
}

// EligibleModels 返回至少一个启用账户具有模型资格的目录项
func (p *AccountPool) EligibleModels(models []Model) []Model {
	return p.EligibleModelsIn(PoolScopeAll, models)
}

// CanonicalModelID 把实时目录中的模型别名换成上游接受的正式模型 ID
func (p *AccountPool) CanonicalModelID(modelID string) string {
	return p.CanonicalModelIDIn(PoolScopeAll, modelID)
}

func modelMatchesID(model Model, modelID string) bool {
	if model.ID == modelID {
		return true
	}
	for _, alias := range model.CapabilityOptions["aliases"] {
		if alias == modelID {
			return true
		}
	}
	return false
}

// NewAccountPool 创建账户独占调度池
func NewAccountPool(accounts []*Account, perAccountConcurrency int) *AccountPool {
	p := &AccountPool{
		accounts: append([]*Account(nil), accounts...), byID: make(map[string]*Account, len(accounts)),
		resources: make(map[string]string), perAccountConcurrency: perAccountConcurrency, changed: make(chan struct{}),
		routingStrategy: "round-robin", lastPicked: make(map[string]string), lastPickedChannel: make(map[string]Channel),
		catalogSummaries: make(map[PoolScope]*catalogSummary), canonicalIndexes: make(map[PoolScope]*canonicalIndex),
		modelChannelsMemos: make(map[PoolScope]*modelChannelsMemo), ultraAccounts: make(map[string]struct{}),
	}
	for position, account := range p.accounts {
		if account == nil {
			continue
		}
		account.position = position
		// 构造时开关为 false；ULTRA_INCLUDE_PRO=true 由随后的 SetUltraIncludePro 重算成员（见 app 的 configurePool）
		if ultraPoolTier(account.BenefitTier, false) {
			p.ultraAccounts[account.ID] = struct{}{}
		}
		if account.runtime.Cooldowns == nil {
			account.runtime.Cooldowns = make(map[string]CooldownState)
		}
		if account.runtime.Resources == nil {
			account.runtime.Resources = make(map[string]ResourceBinding)
		}
		if account.runtime.ModelAccess == nil {
			account.runtime.ModelAccess = make(map[string]ModelAccess)
		}
		p.byID[account.ID] = account
		for resourceID := range account.runtime.Resources {
			p.resources[resourceID] = account.ID
		}
	}
	return p
}

// Account 返回稳定 ID 对应的账户
func (p *AccountPool) Account(accountID string) (*Account, error) {
	if p == nil {
		return nil, ErrAccountNotFound
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	return account, nil
}

// Add 将新账户加入当前调度池
func (p *AccountPool) Add(account *Account) (resultErr error) {
	if p == nil || account == nil || strings.TrimSpace(account.ID) == "" {
		return fmt.Errorf("账户未初始化")
	}
	if account.ConfigPath != "" {
		account.storageMu.Lock()
		lockedExternally := account.persistenceLocked
		account.storageMu.Unlock()
		if !lockedExternally {
			leaseLock, _, err := acquireAccountFileLease(account.StoragePath)
			if errors.Is(err, errAccountLeaseBusy) {
				return fmt.Errorf("%w: %s", ErrAccountLeased, account.ID)
			}
			if err != nil {
				return err
			}
			defer func() {
				if leaseLock != nil {
					resultErr = errors.Join(resultErr, leaseLock.Unlock())
				}
			}()
			account.runtimeMu.Lock()
			defer account.runtimeMu.Unlock()
			runtimeLock, err := lockRuntimeState(context.Background(), account)
			if err != nil {
				return err
			}
			defer func() {
				if runtimeLock != nil {
					resultErr = errors.Join(resultErr, runtimeLock.Unlock())
				}
			}()
		}
		if err := validatePersistentAccountFiles(account); err != nil {
			return err
		}
		runtimeState, err := readRuntimeOrEmpty(account.RuntimePath)
		if err != nil {
			return err
		}
		account.runtime = runtimeState
		account.BenefitTier = runtimeState.BenefitTier
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.byID[account.ID]; exists {
		return fmt.Errorf("账户已存在: %s", account.ID)
	}
	if account.runtime.Cooldowns == nil {
		account.runtime.Cooldowns = make(map[string]CooldownState)
	}
	if account.runtime.Resources == nil {
		account.runtime.Resources = make(map[string]ResourceBinding)
	}
	if account.runtime.ModelAccess == nil {
		account.runtime.ModelAccess = make(map[string]ModelAccess)
	}
	for resourceID := range account.runtime.Resources {
		if owner, exists := p.resources[resourceID]; exists {
			return fmt.Errorf("资源 %s 已绑定账户 %s", resourceID, owner)
		}
	}
	p.accounts = append(p.accounts, account)
	p.byID[account.ID] = account
	p.catalogVersion++
	p.accountsVersion++
	p.eligibilityVersion++
	p.indexAccountAddedLocked(account)
	p.invalidateStatusLocked()
	for resourceID := range account.runtime.Resources {
		p.resources[resourceID] = account.ID
	}
	p.notifyLocked()
	return nil
}

// Remove 在账户空闲时删除持久目录并移出调度池
func (p *AccountPool) Remove(accountID string, deleteDirectory func(*Account) error) (*Account, error) {
	if p == nil {
		return nil, ErrAccountNotFound
	}
	p.mu.Lock()
	accountID = strings.TrimSpace(accountID)
	account := p.byID[accountID]
	if account == nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	if account.exclusive || account.exclusiveWaiters > 0 || account.active > 0 {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAccountLeased, accountID)
	}
	if deleteDirectory == nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("账户目录删除函数为空")
	}
	account.exclusive = true
	p.busyChangedLocked(account)
	p.notifyLocked()
	p.mu.Unlock()
	leaseLock, _, err := acquireAccountFileLease(account.StoragePath)
	if err != nil {
		p.clearRemoveExclusive(account)
		if errors.Is(err, errAccountLeaseBusy) {
			return nil, fmt.Errorf("%w: %s", ErrAccountLeased, accountID)
		}
		return nil, err
	}
	return p.removeExclusive(account, leaseLock, deleteDirectory)
}

// clearRemoveExclusive 删除没有完成时撤销删除置上的独占，账户照常调度
func (p *AccountPool) clearRemoveExclusive(account *Account) {
	p.mu.Lock()
	account.exclusive = false
	p.busyChangedLocked(account)
	p.notifyLocked()
	p.mu.Unlock()
}

// removeExclusive 删除已经置为独占、持有账户文件锁（leaseLock，可为 nil）的账户：锁定运行状态后删除持久目录，
// 成功后移出调度池；失败时撤销独占，账户照常调度。返回前释放 leaseLock。Remove 与 AccountReservation.Remove 共用
func (p *AccountPool) removeExclusive(account *Account, leaseLock *flock.Flock, deleteDirectory func(*Account) error) (*Account, error) {
	accountID := account.ID
	account.runtimeMu.Lock()
	runtimeLock, err := lockRuntimeState(context.Background(), account)
	if err != nil {
		account.runtimeMu.Unlock()
		if leaseLock != nil {
			_ = leaseLock.Unlock()
		}
		p.clearRemoveExclusive(account)
		return nil, err
	}
	account.storageMu.Lock()
	account.persistenceLocked = true
	account.storageMu.Unlock()
	deleteErr := deleteDirectory(account)
	account.storageMu.Lock()
	account.persistenceLocked = false
	account.storageMu.Unlock()
	var releaseErr error
	if runtimeLock != nil {
		releaseErr = runtimeLock.Unlock()
	}
	account.runtimeMu.Unlock()
	if leaseLock != nil {
		releaseErr = errors.Join(releaseErr, leaseLock.Unlock())
	}
	if deleteErr != nil {
		p.clearRemoveExclusive(account)
		return nil, errors.Join(deleteErr, releaseErr)
	}
	p.mu.Lock()
	for resourceID, owner := range p.resources {
		if owner == accountID {
			delete(p.resources, resourceID)
		}
	}
	delete(p.byID, accountID)
	delete(p.ultraAccounts, accountID)
	p.ultraSnapshot = nil
	// 移出号池的账户不再排空：调用方另建同 ID 账户时从未排空开始
	account.draining = false
	for index, candidate := range p.accounts {
		if candidate != nil && candidate.ID == accountID {
			p.accounts = append(p.accounts[:index], p.accounts[index+1:]...)
			p.catalogVersion++
			p.accountsVersion++
			break
		}
	}
	// 后续账户的下标都变了，候选索引整体重建
	p.eligibilityVersion++
	p.invalidateSchedulingLocked()
	p.invalidateStatusLocked()
	p.notifyLocked()
	p.mu.Unlock()
	return account, releaseErr
}

// Acquire 为模型轮询获取一个账户槽位
func (p *AccountPool) Acquire(ctx context.Context, model string) (*AccountLease, error) {
	return p.AcquireFor(ctx, AccountSelection{ModelID: model})
}

// AcquireAccount 为管理操作获取不受调度状态限制的指定账户租约
func (p *AccountPool) AcquireAccount(ctx context.Context, accountID string) (*AccountLease, error) {
	reservation, err := p.ReserveAccount(accountID)
	if err != nil {
		return nil, err
	}
	return reservation.Wait(ctx)
}

// AccountReservation 为已登记的独占等待（AcquireAccount 的前半段）：登记期间账户不再发出新租约，
// 进行中的租约照常使用；Wait 等它们都释放后取得独占租约，Cancel 撤销登记
type AccountReservation struct {
	pool    *AccountPool
	account *Account
	once    sync.Once
}

// ReserveAccount 立即为指定账户登记独占等待，不等待进行中的租约
func (p *AccountPool) ReserveAccount(accountID string) (*AccountReservation, error) {
	if p == nil {
		return nil, ErrAccountNotFound
	}
	accountID = strings.TrimSpace(accountID)
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[accountID]
	if account == nil {
		return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	return p.reserveLocked(account), nil
}

// reserveLocked 登记独占等待（调用方持有 p.mu）
func (p *AccountPool) reserveLocked(account *Account) *AccountReservation {
	account.exclusiveWaiters++
	p.busyChangedLocked(account)
	p.notifyLocked()
	return &AccountReservation{pool: p, account: account}
}

// Cancel 撤销独占等待登记；已撤销（含 Wait 返回后）时不做任何事
func (r *AccountReservation) Cancel() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.pool.mu.Lock()
		r.account.exclusiveWaiters--
		r.pool.busyChangedLocked(r.account)
		r.pool.notifyLocked()
		r.pool.mu.Unlock()
	})
}

// Wait 等账户上进行中的租约都释放后取得独占租约（与 AcquireAccount 相同），返回时撤销登记：
// 取得租约时账户已处于独占状态，撤销登记与取得租约之间没有空档
func (r *AccountReservation) Wait(ctx context.Context) (*AccountLease, error) {
	if r == nil {
		return nil, fmt.Errorf("账户独占等待未初始化")
	}
	defer r.Cancel()
	p := r.pool
	accountID := r.account.ID
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		account := p.byID[accountID]
		if account == nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
		}
		if !account.exclusive && account.authRefreshers == 0 && account.active == 0 {
			checkedAt := time.Now().UTC()
			leaseLock, leasePath, err := acquireAccountFileLease(account.StoragePath)
			if err == nil {
				account.exclusive = true
				account.leaseLock = leaseLock
				account.leasePath = leasePath
				p.busyChangedLocked(account)
				// 代际在锁内读取：目录同步与认证结果在锁内递增它们，解锁后再读会与之竞争
				authGeneration, modelAccessGeneration := account.authGeneration, account.modelAccessGeneration
				p.mu.Unlock()
				lease := &AccountLease{
					pool: p, account: account, exclusive: true, authGeneration: authGeneration,
					modelAccessGeneration: modelAccessGeneration, checkedAt: checkedAt,
				}
				if err := p.refreshAccountRuntime(ctx, account); err != nil {
					releaseErr := lease.Release()
					if errors.Is(err, ErrAccountNotFound) {
						p.markStaleAccountUnavailable(account)
					}
					return nil, errors.Join(err, releaseErr)
				}
				p.mu.Lock()
				lease.modelAccessGeneration = account.modelAccessGeneration
				p.mu.Unlock()
				return lease, nil
			}
			if !errors.Is(err, errAccountLeaseBusy) {
				p.mu.Unlock()
				return nil, err
			}
		}
		changed := p.changed
		p.mu.Unlock()

		timer := time.NewTimer(externalLeasePoll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-changed:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

// Remove 等账户上进行中的租约、独占操作与认证刷新都结束后删除持久目录并移出调度池（删除步骤与 AccountPool.Remove 相同），
// 返回时撤销登记。登记期间账户不再发出新租约，进行中的请求照常结束；空闲时在同一次加锁内转为独占删除，与 Wait 一样没有空档：
// AccountPool.Remove 遇到进行中的请求直接返回 ErrAccountLeased，先 Wait 取得租约、释放后再 Remove 时，释放唤醒的排队请求
// 又可能在两者之间取得账户。账户已经空闲时立即删除、不看 ctx；需要等待时 ctx 结束即返回它的错误，登记随之撤销，账户恢复原状。
// 登记的账户已被移出号池（或换成了同 ID 的新账户）时返回 ErrAccountNotFound，不删除新账户
func (r *AccountReservation) Remove(ctx context.Context, deleteDirectory func(*Account) error) (*Account, error) {
	if r == nil {
		return nil, fmt.Errorf("账户独占等待未初始化")
	}
	defer r.Cancel()
	if deleteDirectory == nil {
		return nil, fmt.Errorf("账户目录删除函数为空")
	}
	p := r.pool
	account := r.account
	for {
		p.mu.Lock()
		if p.byID[account.ID] != account {
			p.mu.Unlock()
			return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, account.ID)
		}
		if !account.exclusive && account.authRefreshers == 0 && account.active == 0 {
			leaseLock, _, err := acquireAccountFileLease(account.StoragePath)
			if err == nil {
				account.exclusive = true
				p.busyChangedLocked(account)
				p.notifyLocked()
				p.mu.Unlock()
				return p.removeExclusive(account, leaseLock, deleteDirectory)
			}
			if !errors.Is(err, errAccountLeaseBusy) {
				p.mu.Unlock()
				return nil, err
			}
		}
		changed := p.changed
		p.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		timer := time.NewTimer(externalLeasePoll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-changed:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

// AcquireFor 按模型方法账户或资源粘性获取账户槽位
func (p *AccountPool) AcquireFor(ctx context.Context, selection AccountSelection) (*AccountLease, error) {
	if p == nil {
		return nil, PoolNotReady(poolMissingReason, nil)
	}
	selection = ScopedSelection(ctx, selection)
	if selection.ResourceID != "" {
		if err := p.refreshResource(ctx, selection.ResourceID); err != nil {
			return nil, err
		}
	}
	refreshedCandidates := false
	for {
		p.mu.Lock()
		if err := p.validateSelectionLocked(selection); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		lease, earliest, waitable, err := p.tryAcquireLocked(selection, time.Now())
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if lease != nil {
			p.mu.Unlock()
			eligible, refreshErr := p.refreshAndValidateLease(ctx, lease, selection)
			if refreshErr != nil {
				releaseErr := lease.Release()
				if errors.Is(refreshErr, ErrAccountNotFound) {
					p.markStaleAccountUnavailable(lease.account)
					if selection.AccountID == "" && selection.ResourceID == "" {
						if releaseErr != nil {
							return nil, releaseErr
						}
						continue
					}
				}
				return nil, errors.Join(refreshErr, releaseErr)
			}
			if eligible {
				return lease, nil
			}
			if err := lease.Release(); err != nil {
				return nil, err
			}
			continue
		}
		if !refreshedCandidates && (!waitable || !earliest.IsZero()) {
			p.mu.Unlock()
			if err := p.refreshSelectionRuntimesThrottled(ctx, selection); err != nil {
				return nil, err
			}
			refreshedCandidates = true
			continue
		}
		if !waitable {
			err := p.noEligibleErrorLocked(selection)
			p.mu.Unlock()
			return nil, err
		}
		changed := p.changed
		p.mu.Unlock()

		if earliest.IsZero() {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-changed:
			}
			continue
		}
		delay := time.Until(earliest)
		if delay <= 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-changed:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

// TryAcquireFor 尝试获取账户槽位并立即返回当前结果
func (p *AccountPool) TryAcquireFor(ctx context.Context, selection AccountSelection) (*AccountLease, bool, error) {
	if p == nil {
		return nil, false, PoolNotReady(poolMissingReason, nil)
	}
	selection = ScopedSelection(ctx, selection)
	if selection.ResourceID != "" {
		if err := p.refreshResource(ctx, selection.ResourceID); err != nil {
			return nil, false, err
		}
	}
	refreshedCandidates := false
	for {
		p.mu.Lock()
		if err := p.validateSelectionLocked(selection); err != nil {
			p.mu.Unlock()
			return nil, false, err
		}
		lease, earliest, waitable, err := p.tryAcquireLocked(selection, time.Now())
		p.mu.Unlock()
		if err != nil {
			return nil, waitable, err
		}
		if lease == nil && !refreshedCandidates && (!waitable || !earliest.IsZero()) {
			if err := p.refreshSelectionRuntimesThrottled(ctx, selection); err != nil {
				return nil, false, err
			}
			refreshedCandidates = true
			continue
		}
		if lease == nil {
			return lease, waitable, err
		}
		eligible, refreshErr := p.refreshAndValidateLease(ctx, lease, selection)
		if refreshErr != nil {
			releaseErr := lease.Release()
			if errors.Is(refreshErr, ErrAccountNotFound) {
				p.markStaleAccountUnavailable(lease.account)
				if selection.AccountID == "" && selection.ResourceID == "" {
					if releaseErr != nil {
						return nil, false, releaseErr
					}
					continue
				}
			}
			return nil, false, errors.Join(refreshErr, releaseErr)
		}
		if eligible {
			return lease, waitable, nil
		}
		if err := lease.Release(); err != nil {
			return nil, false, err
		}
	}
}

func (p *AccountPool) refreshAndValidateLease(
	ctx context.Context,
	lease *AccountLease,
	selection AccountSelection,
) (bool, error) {
	if lease == nil || lease.account == nil {
		return false, PoolNotReadyIn(selection.Pool, leaseReplacedReason, nil)
	}
	if lease.refreshRuntime {
		if err := p.refreshAccountRuntime(ctx, lease.account); err != nil {
			return false, err
		}
		lease.refreshRuntime = false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := lease.account
	if p.byID[account.ID] != account {
		return false, PoolNotReadyIn(selection.Pool, leaseReplacedReason, nil)
	}
	lease.modelAccessGeneration = account.modelAccessGeneration
	if resourceID := strings.TrimSpace(selection.ResourceID); resourceID != "" {
		if owner, exists := p.resources[resourceID]; !exists || owner != account.ID {
			return false, ErrResourceNotFound
		}
		if err := p.resourcePoolErrorLocked(selection.Pool, resourceID, account.ID); err != nil {
			return false, err
		}
	} else if !p.inPoolLocked(selection.Pool, account) {
		// 刷新运行态期间账户权益或 ULTRA_INCLUDE_PRO 变化、已不属于请求的号池：放弃这个账户重新选号
		return false, nil
	}
	if selection.ModelID != "" && !p.channelSupportsLocked(account, lease.Channel(), selection) {
		return false, nil
	}
	if selection.ResourceID == "" {
		if _, active := accountCooldown(account, lease.CooldownScope(selectionAccessScope(selection)), time.Now()); active {
			return false, nil
		}
	}
	return true, nil
}

// NoEligibleError 返回没有可调度账户时的错误：支持请求的账户都不可调度（或号池为空）时为列出原因的
// AccountsNotReadyError（503）；号池中没有任何账户支持请求时为 ErrNoEligibleAccount（请求本身的原因）
func (p *AccountPool) NoEligibleError(selection AccountSelection) error {
	if p == nil {
		return PoolNotReady(poolMissingReason, nil)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.noEligibleErrorLocked(selection)
}

func (p *AccountPool) noEligibleErrorLocked(selection AccountSelection) error {
	selection.ModelID = strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	var reasons []string
	for _, account := range p.accounts {
		if account == nil || account.Config.Enabled && account.State == AccountReady || !p.inPoolLocked(selection.Pool, account) {
			continue
		}
		if selection.ModelID != "" && !p.accountSupportsAnyChannelLocked(account, selection) {
			continue
		}
		label, ok := accountStateLabels[account.State]
		if !ok {
			label = string(account.State)
		}
		if !account.Config.Enabled {
			label = accountStateLabels[AccountDisabled]
		}
		reason := account.ID + " " + label
		if message := strings.TrimSpace(account.stateMessage); message != "" {
			reason += "（" + message + "）"
		}
		reasons = append(reasons, reason)
	}
	if len(reasons) > 0 {
		return &AccountsNotReadyError{Reasons: reasons, Pool: selection.Pool}
	}
	if !p.hasAccountLocked(selection.Pool) {
		return p.emptyPoolErrorLocked(selection.Pool)
	}
	return ErrNoEligibleAccount
}

// emptyPoolErrorLocked 返回号池中没有任何账户时的号池暂时不可调度错误：Ultra 号池为空说明没有属于 Ultra 号池的账户；
// 普通号池为空而账户池不为空，说明账户都属于 Ultra 号池、独占模式下普通路径不能使用。ULTRA_INCLUDE_PRO=true 时原因写明
// Pro 也属于 Ultra 号池，关闭时保持原文。调用方持有 p.mu
func (p *AccountPool) emptyPoolErrorLocked(scope PoolScope) *AccountsNotReadyError {
	reason := emptyPoolReason
	switch {
	case scope == PoolScopeUltra && p.ultraIncludePro:
		reason = emptyUltraPoolWithProReason
	case scope == PoolScopeUltra:
		reason = emptyUltraPoolReason
	case scope == PoolScopeNormal && p.hasAccountLocked(PoolScopeAll) && p.ultraIncludePro:
		reason = emptyNormalPoolWithProReason
	case scope == PoolScopeNormal && p.hasAccountLocked(PoolScopeAll):
		reason = emptyNormalPoolReason
	}
	return &AccountsNotReadyError{Reasons: []string{reason}, Pool: scope}
}

// NoCandidateError 返回候选分组为空时的错误：有账户在冷却时按全部冷却（429，带最早恢复时间），否则同 NoEligibleError
func (p *AccountPool) NoCandidateError(selection AccountSelection, groups AccountCandidateGroups) error {
	if !groups.EarliestCooldown.IsZero() {
		return &AllCoolingError{ModelID: strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/"), Until: groups.EarliestCooldown}
	}
	return p.NoEligibleError(selection)
}

func (p *AccountPool) hasAccountLocked(scope PoolScope) bool {
	for _, account := range p.accounts {
		if account != nil && p.inPoolLocked(scope, account) {
			return true
		}
	}
	return false
}

// catalogMissingErrorLocked 号池里还没有任何账户的模型目录：号池为空、启动阶段目录仍在同步，或账户都需要重新登录。
// 这时无法判断模型是否存在，属于号池一侧的暂时性原因，不能按模型不存在返回
func (p *AccountPool) catalogMissingErrorLocked(scope PoolScope) error {
	if !p.hasAccountLocked(scope) {
		return p.emptyPoolErrorLocked(scope)
	}
	reasons := []string{"账户模型目录尚未加载"}
	var notReady *AccountsNotReadyError
	if errors.As(p.noEligibleErrorLocked(AccountSelection{Pool: scope}), &notReady) {
		reasons = append(reasons, notReady.Reasons...)
	}
	return &AccountsNotReadyError{Reasons: reasons, Pool: scope}
}

func (p *AccountPool) markStaleAccountUnavailable(account *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if account != nil && p.byID[account.ID] == account {
		account.State = AccountUnavailable
		account.stateMessage = ErrAccountNotFound.Error()
		for resourceID, owner := range p.resources {
			if owner == account.ID {
				delete(p.resources, resourceID)
			}
		}
		clear(account.runtime.Resources)
		p.accountChangedLocked(account)
		p.notifyLocked()
	}
}

func (p *AccountPool) validateSelectionLocked(selection AccountSelection) error {
	modelID := strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	if modelID == "" {
		return nil
	}
	if !p.hasModelCatalogLocked(selection.Pool) {
		return p.catalogMissingErrorLocked(selection.Pool)
	}
	if !p.hasModelLocked(modelID, selection.Pool) {
		return fmt.Errorf("%w: %s", ErrModelNotFound, modelID)
	}
	if selection.Method != "" && !p.hasModelMethodLocked(modelID, selection.Method, selection.Pool) {
		return fmt.Errorf("%w: 模型 %s 不支持 %s", ErrModelNotFound, modelID, selection.Method)
	}
	if selection.Capability != "" && !p.hasModelCapabilityLocked(modelID, selection.Capability, selection.Pool) {
		return fmt.Errorf("%w: 模型 %s 不支持 %s", ErrModelNotFound, modelID, selection.Capability)
	}
	return nil
}

// AcquireResource 获取创建资源的固定账户
func (p *AccountPool) AcquireResource(ctx context.Context, resourceID string) (*AccountLease, error) {
	return p.AcquireFor(ctx, AccountSelection{ResourceID: resourceID})
}

// Account 返回当前租约持有的账户
func (l *AccountLease) Account() *Account {
	if l == nil {
		return nil
	}
	return l.account
}

// ModelAccessGeneration 返回租约开始时的模型资格目录代际
func (l *AccountLease) ModelAccessGeneration() uint64 {
	if l == nil {
		return 0
	}
	return l.modelAccessGeneration
}

// CheckedAt 返回当前账户请求取得租约的时间
func (l *AccountLease) CheckedAt() time.Time {
	if l == nil {
		return time.Time{}
	}
	return l.checkedAt
}

// MarkAuthenticationValid 保存当前租约确认的认证成功状态
func (l *AccountLease) MarkAuthenticationValid() error {
	return l.markAuthenticationStateAt(false, "", l.CheckedAt())
}

// MarkAuthenticationRequired 保存当前租约确认的认证失败状态
func (l *AccountLease) MarkAuthenticationRequired(reason string) error {
	return l.markAuthenticationStateAt(true, reason, l.CheckedAt())
}

// markAuthenticationValidAt 保存长连接中指定轮次的认证成功状态
func (l *AccountLease) markAuthenticationValidAt(checkedAt time.Time) error {
	return l.markAuthenticationStateAt(false, "", checkedAt)
}

// markAuthenticationRequiredAt 保存长连接中指定轮次的认证失败状态
func (l *AccountLease) markAuthenticationRequiredAt(reason string, checkedAt time.Time) error {
	return l.markAuthenticationStateAt(true, reason, checkedAt)
}

// CoolingDown 返回租约账户当前是否处于全局或指定模型的冷却
func (l *AccountLease) CoolingDown(modelID string) bool {
	if l == nil || l.account == nil {
		return false
	}
	return l.pool.AccountCoolingDown(l.account.ID, l.CooldownScope(strings.TrimPrefix(strings.TrimSpace(modelID), "models/")))
}

// AccountCoolingDown 返回账户当前是否处于全局或指定模型的冷却
func (p *AccountPool) AccountCoolingDown(accountID string, modelID string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return false
	}
	_, active := accountCooldown(account, strings.TrimPrefix(strings.TrimSpace(modelID), "models/"), time.Now())
	return active
}

// markAuthenticationStateAt 写回指定顺序时间的账户认证状态
func (l *AccountLease) markAuthenticationStateAt(required bool, reason string, checkedAt time.Time) error {
	if l == nil || l.account == nil || l.pool == nil {
		return fmt.Errorf("账户租约未初始化")
	}
	l.operation.Lock()
	authGeneration := l.authGeneration
	l.operation.Unlock()
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	l.pool.markAuthenticationLocked(l.account, authGeneration, required, reason, checkedAt)
	return nil
}

// markAuthenticationLocked 按认证代际与顺序时间写回认证结果，返回是否写入；调用方持有 p.mu。
// 登录已被替换（代际变化）或已有更晚的结果时忽略。同一时间先确认有效、后确认失效时以失效为准：
// 同一请求先输出正文、随后才返回认证失败，账户不能继续显示为就绪
func (p *AccountPool) markAuthenticationLocked(
	account *Account,
	authGeneration uint64,
	required bool,
	reason string,
	checkedAt time.Time,
) bool {
	if p.byID[account.ID] != account || authGeneration != account.authGeneration ||
		checkedAt.Before(account.authCheckedAt) {
		return false
	}
	account.authCheckedAt = checkedAt
	previous := account.State
	if !account.Config.Enabled {
		account.State = AccountDisabled
	} else if required {
		account.State = AccountAuthRequired
	} else if account.State == AccountAuthRequired {
		account.State = AccountReady
	}
	if required {
		account.stateMessage = strings.TrimSpace(reason)
	} else if account.State == AccountReady {
		account.stateMessage = ""
	}
	if account.State != previous {
		p.accountChangedLocked(account)
	}
	p.notifyLocked()
	return true
}

// AuthGeneration 返回账户当前的认证代际；账户不存在时返回 0
func (p *AccountPool) AuthGeneration(accountID string) uint64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return 0
	}
	return account.authGeneration
}

// MarkAuthenticationRequiredIfGeneration 在没有请求租约的路径（预热启动 Worker）写回认证失败：
// 与租约写回使用同一顺序规则，generation 为开始前读取的认证代际，期间保存过新登录时不写入。返回是否写入
func (p *AccountPool) MarkAuthenticationRequiredIfGeneration(
	accountID string,
	generation uint64,
	checkedAt time.Time,
	reason string,
) (bool, error) {
	if p == nil {
		return false, ErrAccountNotFound
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return false, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	return p.markAuthenticationLocked(account, generation, true, reason, checkedAt.UTC()), nil
}

// ModelAccessGeneration 返回账户当前模型资格目录代际
func (p *AccountPool) ModelAccessGeneration(accountID string) uint64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return 0
	}
	return account.modelAccessGeneration
}

// SaveStorageState 在租约内原子写回认证状态
func (l *AccountLease) SaveStorageState(state StorageState) error {
	if l == nil || l.account == nil || l.pool == nil {
		return fmt.Errorf("账户租约未初始化")
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return fmt.Errorf("账户租约已释放")
	}
	l.account.storageMu.Lock()
	defer l.account.storageMu.Unlock()
	if err := WriteStorageState(l.account.StoragePath, state); err != nil {
		return err
	}
	l.pool.mu.Lock()
	l.account.StorageState = state
	// 保存新的登录状态（管理端重新登录、目录监视载入新文件）与续签一样推进认证代际：
	// 在此之前开始的租约导出的旧 Cookie 不能再写回覆盖新登录（见 authRefreshedSinceLease）
	l.account.authGeneration++
	l.authGeneration = l.account.authGeneration
	l.account.authCheckedAt = time.Time{}
	l.pool.mu.Unlock()
	return nil
}

// WaitForAuthRefresh 在 BeginAuthRefresh 之后等待同账户的其他正常请求结束，或复用其他租约已经提交的新认证：
// 续签会重置账户的 WAA runtime，正在流式输出的请求不能被打断。调用方必须给 ctx 设上限，
// 同账户的请求可能正等待本次续签所在的 Worker 启动结束
func (l *AccountLease) WaitForAuthRefresh(ctx context.Context) error {
	if l == nil || l.account == nil || l.pool == nil {
		return fmt.Errorf("账户租约未初始化")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.operation.Lock()
		generation := l.authGeneration
		l.operation.Unlock()
		l.pool.mu.Lock()
		ready := l.exclusive || generation != l.account.authGeneration || l.account.active <= l.account.authRefreshers
		changed := l.pool.changed
		l.pool.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// RefreshStorageState 保证并发认证失效只提交一次
func (l *AccountLease) RefreshStorageState(
	update func(*StorageState) error,
	prepareCommit func() (func(bool), error),
) error {
	if l == nil || l.account == nil || l.pool == nil || update == nil || prepareCommit == nil {
		return fmt.Errorf("账户租约未初始化")
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return fmt.Errorf("账户租约已释放")
	}
	l.account.storageMu.Lock()
	defer l.account.storageMu.Unlock()
	l.pool.mu.Lock()
	currentGeneration := l.account.authGeneration
	l.pool.mu.Unlock()
	if l.authGeneration != currentGeneration {
		l.authGeneration = currentGeneration
		return nil
	}
	state, err := LoadStorageState(l.account.StoragePath)
	if err != nil {
		return err
	}
	if err := update(&state); err != nil {
		return err
	}
	finishCommit, err := prepareCommit()
	if err != nil {
		return err
	}
	if err := WriteStorageState(l.account.StoragePath, state); err != nil {
		finishCommit(false)
		return err
	}
	l.pool.mu.Lock()
	l.account.StorageState = state
	l.account.authGeneration++
	l.authGeneration = l.account.authGeneration
	l.account.authCheckedAt = time.Time{}
	l.pool.mu.Unlock()
	finishCommit(true)
	return nil
}

// BeginAuthRefresh 为当前账户取得认证刷新独占窗口
func (l *AccountLease) BeginAuthRefresh() (func(), bool) {
	if l == nil || l.account == nil || l.pool == nil {
		return nil, false
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	l.pool.mu.Lock()
	if l.released {
		l.pool.mu.Unlock()
		return nil, false
	}
	if l.exclusive {
		l.pool.mu.Unlock()
		return func() {}, true
	}
	if l.account.exclusive {
		l.pool.mu.Unlock()
		return nil, false
	}
	l.account.authRefreshers++
	l.pool.busyChangedLocked(l.account)
	l.pool.notifyLocked()
	l.pool.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.pool.mu.Lock()
			if l.account.authRefreshers > 0 {
				l.account.authRefreshers--
			}
			l.pool.busyChangedLocked(l.account)
			l.pool.notifyLocked()
			l.pool.mu.Unlock()
		})
	}, true
}

// SaveConfig 在租约内原子写回账户固定配置
func (l *AccountLease) SaveConfig(value AccountConfig) error {
	if l == nil || l.account == nil || l.pool == nil {
		return fmt.Errorf("账户租约未初始化")
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return fmt.Errorf("账户租约已释放")
	}
	l.account.storageMu.Lock()
	defer l.account.storageMu.Unlock()
	if err := writeAccountConfig(l.account.ConfigPath, value); err != nil {
		return err
	}
	l.pool.mu.Lock()
	wasDisabled := l.account.State == AccountDisabled
	l.account.Config = value
	if !value.Enabled {
		l.account.State = AccountDisabled
		l.account.stateMessage = ""
	} else if wasDisabled {
		l.account.State = initialAccountState(value, l.account.StorageState)
		l.account.stateMessage = ""
	}
	// 启用状态影响模型可用通道的统计与候选资格
	l.pool.eligibilityVersion++
	l.pool.accountChangedLocked(l.account)
	l.pool.notifyLocked()
	l.pool.mu.Unlock()
	return nil
}

// ReloadStorageState 在租约内重新读取认证状态
func (l *AccountLease) ReloadStorageState() (StorageState, error) {
	if l == nil || l.account == nil || l.pool == nil {
		return StorageState{}, fmt.Errorf("账户租约未初始化")
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return StorageState{}, fmt.Errorf("账户租约已释放")
	}
	l.account.storageMu.Lock()
	defer l.account.storageMu.Unlock()
	state, err := LoadStorageState(l.account.StoragePath)
	if err != nil {
		return StorageState{}, err
	}
	l.pool.mu.Lock()
	l.account.StorageState = state
	l.pool.mu.Unlock()
	return state, nil
}

// ReplaceCookies 以固定指纹浏览器当前 Cookie 替换账户最新持久状态
func (l *AccountLease) ReplaceCookies(cookies []StateCookie) error {
	if l == nil || l.account == nil || l.pool == nil {
		return fmt.Errorf("账户租约未初始化")
	}
	if len(cookies) == 0 {
		return nil
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return fmt.Errorf("账户租约已释放")
	}
	l.account.storageMu.Lock()
	defer l.account.storageMu.Unlock()
	if l.authRefreshedSinceLease() {
		// 本租约开始后账户已续签：这些 Cookie 来自续签前的浏览器会话，写回会覆盖刚续签的登录态，
		// 让账户卡在需要登录。旧浏览器会随续签被重建，直接丢弃
		return nil
	}
	state, err := LoadStorageState(l.account.StoragePath)
	if err != nil {
		return err
	}
	state.Cookies = append([]StateCookie(nil), cookies...)
	if err := WriteStorageState(l.account.StoragePath, state); err != nil {
		return err
	}
	l.pool.mu.Lock()
	l.account.StorageState = state
	l.pool.mu.Unlock()
	return nil
}

// BindResource 将资源固定到当前租约账户
func (l *AccountLease) BindResource(resourceID string, kind string) error {
	if l == nil || l.account == nil || l.pool == nil {
		return fmt.Errorf("账户租约未初始化")
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return fmt.Errorf("账户租约已释放")
	}
	l.account.storageMu.Lock()
	defer l.account.storageMu.Unlock()
	return l.pool.BindResourceKind(resourceID, l.account.ID, kind)
}

// BindVideoOperation 保存视频任务账户与公开对象元数据
func (l *AccountLease) BindVideoOperation(
	ctx context.Context,
	resourceID string,
	metadata VideoResourceMetadata,
) (ResourceBinding, error) {
	if l == nil || l.account == nil || l.pool == nil {
		return ResourceBinding{}, fmt.Errorf("账户租约未初始化")
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return ResourceBinding{}, fmt.Errorf("账户租约已释放")
	}
	l.account.storageMu.Lock()
	defer l.account.storageMu.Unlock()
	return l.pool.bindVideoOperation(ctx, resourceID, l.account.ID, metadata)
}

// VideoOperationBinding 返回当前账户持有的视频任务元数据
func (l *AccountLease) VideoOperationBinding(resourceID string) (ResourceBinding, error) {
	if l == nil || l.account == nil || l.pool == nil {
		return ResourceBinding{}, fmt.Errorf("账户租约未初始化")
	}
	resourceID = strings.TrimSpace(resourceID)
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if l.pool.byID[l.account.ID] != l.account || l.pool.resources[resourceID] != l.account.ID {
		return ResourceBinding{}, fmt.Errorf("%w: %s", ErrResourceNotFound, resourceID)
	}
	binding, exists := l.account.runtime.Resources[resourceID]
	if !exists || binding.Kind != "video-operation" || binding.Video == nil {
		return ResourceBinding{}, fmt.Errorf("%w: %s", ErrResourceNotFound, resourceID)
	}
	metadata := *binding.Video
	binding.Video = &metadata
	return binding, nil
}

// ReplaceResource 原子替换当前租约账户的单个资源绑定
func (l *AccountLease) ReplaceResource(previousResourceID string, resourceID string, kind string) error {
	if l == nil || l.account == nil || l.pool == nil {
		return fmt.Errorf("账户租约未初始化")
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return fmt.Errorf("账户租约已释放")
	}
	l.account.storageMu.Lock()
	defer l.account.storageMu.Unlock()
	return l.pool.replaceResource(previousResourceID, resourceID, l.account.ID, kind)
}

// authRefreshedSinceLease 判断本租约开始（或最近一次采用续签结果）之后账户是否又完成了续签；调用方持有 storageMu
func (l *AccountLease) authRefreshedSinceLease() bool {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	return l.authGeneration != l.account.authGeneration
}

// MergeSetCookieHeaders 将响应 Cookie 合并到账户最新持久状态
func (l *AccountLease) MergeSetCookieHeaders(headers []string, requestURL string, now time.Time) error {
	if l == nil || l.account == nil || l.pool == nil {
		return fmt.Errorf("账户租约未初始化")
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return fmt.Errorf("账户租约已释放")
	}
	l.account.storageMu.Lock()
	defer l.account.storageMu.Unlock()
	if l.authRefreshedSinceLease() {
		// 响应属于续签前的会话，合并它的 Set-Cookie 会用旧会话的轮换值覆盖新登录态
		return nil
	}
	state, err := LoadStorageState(l.account.StoragePath)
	if err != nil {
		return err
	}
	// 磁盘上的登录 Cookie 已被外部更新（与内存不同）：记下来，由账户目录扫描在账户空闲时重启它的浏览器，
	// 让浏览器与签名用同一份新 Cookie。只比较几个 Cookie 值，不加锁
	if LoginIdentity(state) != LoginIdentity(l.account.StorageState) {
		externalLoginChanges.Store(l.account.ID, struct{}{})
	}
	if err := state.MergeSetCookieHeaders(headers, requestURL, now); err != nil {
		return err
	}
	if err := WriteStorageState(l.account.StoragePath, state); err != nil {
		return err
	}
	l.pool.mu.Lock()
	l.account.StorageState = state
	l.pool.mu.Unlock()
	return nil
}

// Release 释放账户文件和进程内租约
func (l *AccountLease) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.operation.Lock()
		defer l.operation.Unlock()
		l.released = true
		l.pool.mu.Lock()
		l.releaseLocked()
		l.pool.mu.Unlock()
	})
	return l.err
}

// ReleaseToExclusiveReservation 释放租约，并在同一次加锁内为该账户登记独占等待（见 AccountReservation）。
// 用于关闭出错的 Worker：请求释放自己的租约后要等账户上其他请求结束才能关闭，原先先释放、再由 AcquireAccount 另行登记，
// 释放唤醒的排队请求可能在两者之间取得这个账户，落到坏掉的 Worker 上（Worker 已失败时还会原地替换浏览器、切断同账户进行中的流）；
// 现在排队请求被唤醒时看到的已经是登记后的账户。租约已释放时返回错误、不登记；释放账户文件锁失败时已经登记，与错误一起返回
func (l *AccountLease) ReleaseToExclusiveReservation() (*AccountReservation, error) {
	if l == nil || l.pool == nil || l.account == nil {
		return nil, fmt.Errorf("账户租约未初始化")
	}
	var reservation *AccountReservation
	l.once.Do(func() {
		l.operation.Lock()
		defer l.operation.Unlock()
		l.released = true
		l.pool.mu.Lock()
		reservation = l.pool.reserveLocked(l.account)
		l.releaseLocked()
		l.pool.mu.Unlock()
	})
	if reservation == nil {
		return nil, fmt.Errorf("账户租约已释放")
	}
	return reservation, l.err
}

// releaseLocked 归还租约占用的槽位或独占状态，账户空闲时释放账户文件锁（调用方持有 l.operation 与 p.mu）
func (l *AccountLease) releaseLocked() {
	if l.exclusive {
		l.account.exclusive = false
	} else if l.account.active > 0 {
		l.account.active--
	}
	l.pool.busyChangedLocked(l.account)
	if l.account.active == 0 && !l.account.exclusive && l.account.authRefreshers == 0 && l.account.leaseLock != nil {
		if err := l.account.leaseLock.Unlock(); err != nil {
			l.err = err
		}
		l.account.leaseLock = nil
		l.account.leasePath = ""
	}
	// 没有发出的生成租约退还每分钟名额的预占（独占租约没有预占）
	l.releaseRateLocked()
	l.pool.notifyLocked()
}

// SetCatalog 替换账户的权益等级与实时模型目录
func (p *AccountPool) SetCatalog(accountID string, tier BenefitTier, models []Model) error {
	fingerprint, err := accountCatalogFingerprint(tier, models)
	if err != nil {
		return err
	}
	_, err = p.updateRuntime(accountID, func(account *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		previousGeneration := account.modelAccessGeneration
		firstCatalog := account.initializedAt.IsZero()
		tierChanged := account.BenefitTier != tier
		catalogChanged := !firstCatalog && catalogEntriesChanged(account.Models, models)
		runtimeChanged := runtimeState.CatalogFingerprint != fingerprint || runtimeState.BenefitTier != tier
		persistedCatalogChanged := firstCatalog && runtimeState.CatalogFingerprint != fingerprint
		if tierChanged || persistedCatalogChanged {
			runtimeChanged = runtimeChanged || len(runtimeState.ModelAccess) > 0
			clear(runtimeState.ModelAccess)
		} else if !firstCatalog {
			for modelID := range runtimeState.ModelAccess {
				catalogModelID := modelAccessCatalogModelID(account.Models, models, modelID)
				if modelCatalogEntryChanged(account.Models, models, catalogModelID) && catalogChanged {
					delete(runtimeState.ModelAccess, modelID)
					runtimeChanged = true
				}
			}
		}
		runtimeState.BenefitTier = tier
		runtimeState.CatalogFingerprint = fingerprint
		return runtimeChanged, func(account *Account) {
			if (firstCatalog || tierChanged || catalogChanged) && account.modelAccessGeneration == previousGeneration {
				account.modelAccessGeneration++
			}
			account.BenefitTier = tier
			account.Models = cloneAccountModels(models)
			setCatalogKeyLocked(account, fingerprint)
			p.catalogVersion++
			if firstCatalog || tierChanged || catalogChanged {
				// 目录内容或权益确实变了才让按版本缓存的目录派生结果重建；目录同步大多数时候返回相同内容
				p.eligibilityVersion++
			}
			account.initializedAt = time.Now()
			if account.State == AccountUnavailable {
				account.State = AccountReady
				account.stateMessage = ""
			}
			p.accountChangedLocked(account)
		}, nil
	})
	if err != nil {
		return err
	}
	return nil
}

// MarkModelAccessVerifiedIfGeneration 保存当前目录代际中的模型生成成功记录
func (p *AccountPool) MarkModelAccessVerifiedIfGeneration(
	accountID string,
	modelID string,
	generation uint64,
	checkedAt time.Time,
) (bool, error) {
	return p.markModelAccessVerified(accountID, modelID, generation, checkedAt)
}

// ForgetModelAccessVerifiedIfGeneration 删除当前目录代际中过期的模型成功记录
func (p *AccountPool) ForgetModelAccessVerifiedIfGeneration(
	accountID string,
	modelID string,
	generation uint64,
	checkedAt time.Time,
) (bool, error) {
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if modelID == "" {
		return false, fmt.Errorf("模型 ID 不能为空")
	}
	checkedAt = checkedAt.UTC()
	if checkedAt.IsZero() {
		return false, fmt.Errorf("模型资格检查时间不能为空")
	}
	forgotten := false
	_, err := p.updateRuntime(accountID, func(account *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		if generation != account.modelAccessGeneration {
			return false, nil, nil
		}
		canonicalModelID := canonicalAccountModelID(account, modelID)
		current := runtimeState.ModelAccess[canonicalModelID]
		if !current.CheckedAt.Before(checkedAt) {
			return false, nil, nil
		}
		forgotten = current.State == ModelAccessVerified
		runtimeState.ModelAccess[canonicalModelID] = ModelAccess{CheckedAt: checkedAt}
		return true, nil, nil
	})
	return forgotten, err
}

func (p *AccountPool) markModelAccessVerified(
	accountID string,
	modelID string,
	generation uint64,
	checkedAt time.Time,
) (bool, error) {
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if modelID == "" {
		return false, fmt.Errorf("模型 ID 不能为空")
	}
	checkedAt = checkedAt.UTC()
	if checkedAt.IsZero() {
		return false, fmt.Errorf("模型资格检查时间不能为空")
	}
	p.mu.Lock()
	account := p.byID[accountID]
	if account == nil {
		p.mu.Unlock()
		return false, fmt.Errorf("账户不存在: %s", accountID)
	}
	canonicalModelID := canonicalAccountModelID(account, modelID)
	current := account.runtime.ModelAccess[canonicalModelID]
	_, cooling := account.runtime.Cooldowns[canonicalModelID]
	globalAccess := account.runtime.ModelAccess[globalCooldownKey]
	// 账号全局 * 上短的分钟限额或限流冷却不会被这次成功清除，不算要写回的变化（见 shortCooldownActive）
	globalCooldown, globalCooling := account.runtime.Cooldowns[globalCooldownKey]
	globalBlocks := globalCooling && !shortCooldownActive(globalCooldown, time.Now())
	unchanged := generation == account.modelAccessGeneration && current.State == ModelAccessVerified &&
		!cooling && !current.CheckedAt.Before(checkedAt) && !globalBlocks &&
		!globalAccess.CheckedAt.Before(checkedAt)
	p.mu.Unlock()
	if unchanged {
		return false, nil
	}
	changed := false
	_, err := p.updateRuntime(accountID, func(account *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		if generation != account.modelAccessGeneration {
			return false, nil, nil
		}
		canonicalModelID := canonicalAccountModelID(account, modelID)
		current := runtimeState.ModelAccess[canonicalModelID]
		runtimeChanged := false
		if !current.CheckedAt.After(checkedAt) {
			if current.CheckedAt.Before(checkedAt) || current.State != ModelAccessVerified {
				runtimeChanged = true
			}
			if _, exists := runtimeState.Cooldowns[canonicalModelID]; exists {
				runtimeChanged = true
			}
			changed = modelAccessState(account, canonicalModelID) != ModelAccessVerified
			runtimeState.ModelAccess[canonicalModelID] = ModelAccess{
				State: ModelAccessVerified, CheckedAt: checkedAt.UTC(),
			}
			delete(runtimeState.Cooldowns, canonicalModelID)
		}
		globalAccess := runtimeState.ModelAccess[globalCooldownKey]
		if !globalAccess.CheckedAt.After(checkedAt) {
			if globalAccess.CheckedAt.Before(checkedAt) {
				runtimeChanged = true
			}
			// 全局短冷却只推进 CheckedAt、等它自行到期：取得租约更晚的请求先成功，证明不了这一分钟的额度已经恢复
			if cooldown, exists := runtimeState.Cooldowns[globalCooldownKey]; exists && !shortCooldownActive(cooldown, time.Now()) {
				runtimeChanged = true
				delete(runtimeState.Cooldowns, globalCooldownKey)
			}
			globalAccess.CheckedAt = checkedAt.UTC()
			runtimeState.ModelAccess[globalCooldownKey] = globalAccess
		}
		return runtimeChanged, nil, nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// ResetModelAccess 清空账户的实测模型资格
func (p *AccountPool) ResetModelAccess(accountID string) error {
	_, err := p.updateRuntime(accountID, func(account *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		if len(runtimeState.ModelAccess) == 0 {
			return false, func(account *Account) { account.modelAccessGeneration++ }, nil
		}
		clear(runtimeState.ModelAccess)
		return true, func(account *Account) { account.modelAccessGeneration++ }, nil
	})
	if err != nil {
		return err
	}
	return nil
}

// CandidateStates 返回候选账户的权益与实时负载
func (p *AccountPool) CandidateStates(accountIDs []string, modelID string) map[string]AccountCandidateState {
	return p.CandidateStatesForScope(accountIDs, modelID, "")
}

// CandidateStatesForScope 返回指定资格范围的候选账户状态
func (p *AccountPool) CandidateStatesForScope(
	accountIDs []string,
	modelID string,
	modelAccessScope string,
) map[string]AccountCandidateState {
	result := make(map[string]AccountCandidateState, len(accountIDs))
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	modelAccessScope = strings.TrimSpace(modelAccessScope)
	if modelAccessScope == "" {
		modelAccessScope = modelID
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, accountID := range accountIDs {
		account := p.byID[accountID]
		if account == nil {
			continue
		}
		result[accountID] = AccountCandidateState{
			ModelAccess:   modelAccessState(account, modelAccessScope),
			Active:        account.active,
			AvailableSlot: max(0, p.perAccountConcurrency-account.active),
		}
	}
	return result
}

// PreferWarmPool 按权益分层轮选初始热账户
func (p *AccountPool) PreferWarmPool(accountIDs []string) []string {
	type warmCandidate struct {
		id       string
		coverage int
	}
	p.mu.Lock()
	groups := make(map[int][]warmCandidate)
	for _, accountID := range accountIDs {
		account := p.byID[accountID]
		if account == nil {
			continue
		}
		coverage := accountWarmCoverageLocked(account)
		priority := benefitTierPriority(account.BenefitTier)
		groups[priority] = append(groups[priority], warmCandidate{id: accountID, coverage: coverage})
	}
	p.mu.Unlock()
	priorities := make([]int, 0, len(groups))
	for priority, candidates := range groups {
		priorities = append(priorities, priority)
		sort.SliceStable(candidates, func(left int, right int) bool {
			return candidates[left].coverage > candidates[right].coverage
		})
		groups[priority] = candidates
	}
	sort.Ints(priorities)
	result := make([]string, 0, len(accountIDs))
	for len(result) < len(accountIDs) {
		added := false
		for _, priority := range priorities {
			candidates := groups[priority]
			if len(candidates) == 0 {
				continue
			}
			result = append(result, candidates[0].id)
			groups[priority] = candidates[1:]
			added = true
		}
		if !added {
			break
		}
	}
	return result
}

// CooldownWrite 为写入冷却的结果（见 TryMarkCooldownIfGeneration）
type CooldownWrite uint8

const (
	// CooldownWritten 表示已写入
	CooldownWritten CooldownWrite = iota + 1
	// CooldownSkippedGeneration 表示模型目录代际已变化，旧租约的结果不再写回
	CooldownSkippedGeneration
	// CooldownSkippedStale 表示该作用域已有不早于本次租约的记录（同账号取得租约更晚的请求已写回）
	CooldownSkippedStale
)

// String 返回写入结果的中文说明，供冷却日志的“原因”字段使用
func (w CooldownWrite) String() string {
	switch w {
	case CooldownWritten:
		return "已写入"
	case CooldownSkippedGeneration:
		return "模型目录已更新，旧租约的结果不再写回"
	case CooldownSkippedStale:
		return "同账号取得租约更晚的请求已写回结果"
	default:
		return "未知"
	}
}

// MarkCooldownIfGeneration 保存当前目录代际中的作用域冷却
func (p *AccountPool) MarkCooldownIfGeneration(
	accountID string,
	modelAccessScope string,
	generation uint64,
	checkedAt time.Time,
	until time.Time,
	reason string,
) error {
	_, err := p.TryMarkCooldownIfGeneration(accountID, modelAccessScope, generation, checkedAt, until, reason)
	return err
}

// TryMarkCooldownIfGeneration 与 MarkCooldownIfGeneration 相同，另外返回冷却是否真正写入：
// 目录代际已变化、或该作用域已有不早于 checkedAt 的记录时不写，调用方据此如实记录日志、不登记淘汰
func (p *AccountPool) TryMarkCooldownIfGeneration(
	accountID string,
	modelAccessScope string,
	generation uint64,
	checkedAt time.Time,
	until time.Time,
	reason string,
) (CooldownWrite, error) {
	if !until.After(time.Now()) {
		return 0, fmt.Errorf("冷却期限必须在未来")
	}
	checkedAt = checkedAt.UTC()
	if checkedAt.IsZero() {
		return 0, fmt.Errorf("冷却检查时间不能为空")
	}
	modelAccessScope = strings.TrimSpace(modelAccessScope)
	if modelAccessScope == "" {
		modelAccessScope = globalCooldownKey
	}
	var write CooldownWrite
	_, err := p.updateRuntime(accountID, func(account *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		if generation != account.modelAccessGeneration {
			write = CooldownSkippedGeneration
			return false, nil, nil
		}
		modelAccessScope = canonicalAccountModelID(account, modelAccessScope)
		currentAccess := runtimeState.ModelAccess[modelAccessScope]
		if !currentAccess.CheckedAt.Before(checkedAt) {
			write = CooldownSkippedStale
			return false, nil, nil
		}
		next := CooldownState{Until: until.UTC(), Reason: reason}
		currentAccess.CheckedAt = checkedAt
		runtimeState.ModelAccess[modelAccessScope] = currentAccess
		runtimeState.Cooldowns[modelAccessScope] = next
		write = CooldownWritten
		return true, nil, nil
	})
	if err != nil {
		return 0, err
	}
	return write, nil
}

// ClearCooldownIfGeneration 清除当前目录代际中的作用域冷却
func (p *AccountPool) ClearCooldownIfGeneration(
	accountID string,
	modelAccessScope string,
	generation uint64,
	checkedAt time.Time,
) error {
	checkedAt = checkedAt.UTC()
	if checkedAt.IsZero() {
		return fmt.Errorf("冷却检查时间不能为空")
	}
	modelAccessScope = strings.TrimSpace(modelAccessScope)
	if modelAccessScope == "" {
		modelAccessScope = globalCooldownKey
	}
	_, err := p.updateRuntime(accountID, func(account *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		if generation != account.modelAccessGeneration {
			return false, nil, nil
		}
		modelAccessScope = canonicalAccountModelID(account, modelAccessScope)
		changed := false
		now := time.Now()
		// clearScope 推进作用域的 CheckedAt 并清除它的冷却；账号全局 * 上的短的分钟限额或限流冷却只推进 CheckedAt、等它自行到期（见 shortCooldownActive）
		clearScope := func(scope string) {
			access := runtimeState.ModelAccess[scope]
			if access.CheckedAt.After(checkedAt) {
				return
			}
			cooldown, cooling := runtimeState.Cooldowns[scope]
			keep := scope == globalCooldownKey && shortCooldownActive(cooldown, now)
			if !access.CheckedAt.Before(checkedAt) && (!cooling || keep) {
				return
			}
			changed = true
			access.CheckedAt = checkedAt
			runtimeState.ModelAccess[scope] = access
			if !keep {
				delete(runtimeState.Cooldowns, scope)
			}
		}
		clearScope(modelAccessScope)
		if modelAccessScope != globalCooldownKey {
			clearScope(globalCooldownKey)
		}
		return changed, nil, nil
	})
	return err
}

// ClearCooldownsMatching 清除账户上满足 match 的冷却（作用域与冷却状态，不论目录代际），返回清除的条数。
// 认证文件更新后用它按原因清除 403 无权限的冷却：新的登录状态可能已经解决了权限问题，冷却最长要到次日零点才到期，
// 不能让重新上传的账户一直等；额度等其他原因的冷却与登录无关，保持不变
func (p *AccountPool) ClearCooldownsMatching(accountID string, match func(string, CooldownState) bool) (int, error) {
	// 内存中没有符合的冷却时不读写运行态文件：重新导入一批账户时每个都会调用一次
	p.mu.Lock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		p.mu.Unlock()
		return 0, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	found := false
	for scope, cooldown := range account.runtime.Cooldowns {
		if match(scope, cooldown) {
			found = true
			break
		}
	}
	p.mu.Unlock()
	if !found {
		return 0, nil
	}
	cleared := 0
	_, err := p.updateRuntime(accountID, func(_ *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		cleared = 0
		for scope, cooldown := range runtimeState.Cooldowns {
			if match(scope, cooldown) {
				delete(runtimeState.Cooldowns, scope)
				cleared++
			}
		}
		return cleared > 0, nil, nil
	})
	if err != nil {
		return 0, err
	}
	return cleared, nil
}

// BindResource 将资源 ID 固定到创建账户
func (p *AccountPool) BindResource(resourceID string, accountID string) error {
	return p.BindResourceKind(resourceID, accountID, "")
}

// BindResourceKind 将带类型的资源 ID 固定到创建账户
func (p *AccountPool) BindResourceKind(resourceID string, accountID string, kind string) error {
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" {
		return fmt.Errorf("资源 ID 不能为空")
	}
	_, err := p.updateRuntime(accountID, func(_ *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		if owner, exists := p.resources[resourceID]; exists && owner != accountID {
			return false, nil, fmt.Errorf("资源 %s 已绑定账户 %s", resourceID, owner)
		}
		if _, exists := runtimeState.Resources[resourceID]; exists {
			return false, nil, nil
		}
		runtimeState.Resources[resourceID] = ResourceBinding{Kind: strings.TrimSpace(kind), CreatedAt: time.Now().UTC()}
		return true, nil, nil
	})
	if err != nil {
		return err
	}
	return nil
}

func (p *AccountPool) bindVideoOperation(
	ctx context.Context,
	resourceID string,
	accountID string,
	metadata VideoResourceMetadata,
) (ResourceBinding, error) {
	resourceID = strings.TrimSpace(resourceID)
	metadata.Model = strings.TrimSpace(metadata.Model)
	metadata.Seconds = strings.TrimSpace(metadata.Seconds)
	metadata.Size = strings.TrimSpace(metadata.Size)
	if resourceID == "" || metadata.Model == "" || metadata.Seconds == "" || metadata.Size == "" {
		return ResourceBinding{}, fmt.Errorf("视频任务元数据不完整")
	}
	var bound ResourceBinding
	_, err := p.updateRuntimeContext(ctx, accountID, func(_ *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		if owner, exists := p.resources[resourceID]; exists && owner != accountID {
			return false, nil, fmt.Errorf("资源 %s 已绑定账户 %s", resourceID, owner)
		}
		if existing, exists := runtimeState.Resources[resourceID]; exists {
			if existing.Kind != "video-operation" || existing.Video == nil {
				return false, nil, fmt.Errorf("资源 %s 不是视频任务", resourceID)
			}
			bound = existing
			return false, nil, nil
		}
		bound = ResourceBinding{
			Kind: "video-operation", CreatedAt: time.Now().UTC(), Video: &metadata,
		}
		runtimeState.Resources[resourceID] = bound
		return true, nil, nil
	})
	if err != nil {
		return ResourceBinding{}, err
	}
	return bound, nil
}

func (p *AccountPool) replaceResource(previousResourceID string, resourceID string, accountID string, kind string) error {
	previousResourceID = strings.TrimSpace(previousResourceID)
	resourceID = strings.TrimSpace(resourceID)
	kind = strings.TrimSpace(kind)
	if resourceID == "" {
		return fmt.Errorf("资源 ID 不能为空")
	}
	_, err := p.updateRuntime(accountID, func(_ *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		for _, candidate := range []string{previousResourceID, resourceID} {
			if candidate == "" {
				continue
			}
			if owner, exists := p.resources[candidate]; exists && owner != accountID {
				return false, nil, fmt.Errorf("资源 %s 已绑定账户 %s", candidate, owner)
			}
		}
		changed := false
		if previousResourceID != "" && previousResourceID != resourceID {
			if _, exists := runtimeState.Resources[previousResourceID]; exists {
				delete(runtimeState.Resources, previousResourceID)
				changed = true
			}
		}
		binding, exists := runtimeState.Resources[resourceID]
		if !exists {
			runtimeState.Resources[resourceID] = ResourceBinding{Kind: kind, CreatedAt: time.Now().UTC()}
			return true, nil, nil
		}
		if binding.Kind == "" && kind != "" {
			binding.Kind = kind
			runtimeState.Resources[resourceID] = binding
			changed = true
		}
		return changed, nil, nil
	})
	if err != nil {
		return err
	}
	return nil
}

// UnbindResource 删除终态资源的账户映射
func (p *AccountPool) UnbindResource(resourceID string) error {
	return p.unbindResourceContext(context.Background(), resourceID)
}

func (p *AccountPool) unbindResourceContext(ctx context.Context, resourceID string) error {
	p.mu.Lock()
	accountID, exists := p.resources[resourceID]
	p.mu.Unlock()
	if !exists {
		if err := p.refreshResource(ctx, resourceID); err != nil {
			return err
		}
		p.mu.Lock()
		accountID, exists = p.resources[resourceID]
		p.mu.Unlock()
	}
	if !exists {
		return ErrResourceNotFound
	}
	_, err := p.updateRuntimeContext(ctx, accountID, func(_ *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		if _, exists := runtimeState.Resources[resourceID]; !exists {
			return false, nil, ErrResourceNotFound
		}
		delete(runtimeState.Resources, resourceID)
		return true, nil, nil
	})
	if err != nil {
		return err
	}
	return nil
}

// MarkAuthRequired 将账户标记为需要重新登录
func (p *AccountPool) MarkAuthRequired(accountID string, reason string) error {
	return p.setAccountState(accountID, AccountAuthRequired, reason)
}

// MarkUnavailable 将账户标记为初始化或运行失败
func (p *AccountPool) MarkUnavailable(accountID string, reason string) error {
	return p.setAccountState(accountID, AccountUnavailable, reason)
}

// MarkReady 将账户恢复为可调度状态
func (p *AccountPool) MarkReady(accountID string) error {
	return p.setAccountState(accountID, AccountReady, "")
}

// Status 返回账户池的脱敏状态
func (p *AccountPool) Status() []AccountStatus {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	now := time.Now()
	if cached, ok := p.cachedStatusLocked(now); ok {
		p.mu.Unlock()
		return cloneStatuses(cached)
	}
	// 先让候选索引给账户编号：缓存的列表按账户下标就地更新单个账户（见 refreshStatusEntryLocked）
	p.schedulingLocked()
	statuses := make([]AccountStatus, 0, len(p.accounts))
	for _, account := range p.accounts {
		if account == nil {
			continue
		}
		statuses = append(statuses, p.accountStatusLocked(account, now))
	}
	p.storeStatusLocked(statuses, now)
	shared := p.statusCache != nil
	p.mu.Unlock()
	if shared {
		return cloneStatuses(statuses)
	}
	return statuses
}

// accountStateLocked 推导账户对外状态：停用、忙碌、冷却或原始状态；调用方持有 p.mu。
// 只有就绪账户会显示为忙碌：需要登录或不可用的账户即使还有未结束的租约也显示原状态，不被“忙碌”盖住。
// 冷却按调度上是否还能用来判断：全局 "*" 冷却仍有效，或至少有一个模型在账户可用的每个启用通道上都在冷却
// （见 accountCoolingLocked）。原先只看全局冷却：每日限额按模型与通道记录，账户在主用模型上两个通道都冷却到次日时
// 状态页仍显示就绪、冷却计数为 0，上万条冷却被摘要掩盖
func (p *AccountPool) accountStateLocked(account *Account, now time.Time) AccountState {
	state := account.State
	if !account.Config.Enabled {
		state = AccountDisabled
	} else if state == AccountReady &&
		(account.exclusive || account.exclusiveWaiters > 0 || account.authRefreshers > 0 || account.active > 0) {
		state = AccountBusy
	} else if state == AccountReady && p.accountCoolingLocked(account, now) {
		state = AccountCooldown
	}
	return state
}

// accountCoolingLocked 判断账户是否处于冷却状态：全局 "*" 冷却仍有效，或任一有效冷却所在的模型在账户可用的每个启用通道上
// 都在冷却。只遍历账户自己的冷却表（通常几条），每条按目录索引判断通道支持，账户上万时状态统计仍很快；调用方持有 p.mu
func (p *AccountPool) accountCoolingLocked(account *Account, now time.Time) bool {
	if _, active := accountCooldown(account, "", now); active {
		return true
	}
	for key, cooldown := range account.runtime.Cooldowns {
		if key == globalCooldownKey || !cooldown.Active(now) {
			continue
		}
		if p.accountModelCoolingLocked(account, cooldownModelID(key), now) {
			return true
		}
	}
	return false
}

// accountModelCoolingLocked 判断账户在模型上的每个可用通道是否都在冷却：Playground 要求通道启用、目录含该模型且权益允许，
// 冷却键为模型 ID；Build 要求通道启用且 Build 目录支持，冷却键为 build:<模型>。没有可用通道时按不冷却处理
// （目录已不含该模型，冷却记录不再影响调度）；只有一个通道冷却、另一个通道还能用时同样不算冷却；调用方持有 p.mu
func (p *AccountPool) accountModelCoolingLocked(account *Account, modelID string, now time.Time) bool {
	usable := false
	if p.channelEnabledLocked(ChannelPlayground) {
		if model, exists := accountModelLocked(account, modelID); exists && modelAllowedByTier(model, account.BenefitTier) {
			usable = true
			if _, active := accountCooldown(account, modelID, now); !active {
				return false
			}
		}
	}
	if p.channelEnabledLocked(ChannelBuild) && p.buildSupportsModelLocked(account, modelID) {
		usable = true
		if _, active := accountCooldown(account, ChannelCooldownScope(ChannelBuild, modelID), now); !active {
			return false
		}
	}
	return usable
}

// accountCoolingUntilLocked 返回账户按状态口径冷却到的时刻（UnixNano，0 为没有），与 accountCoolingLocked 同口径：
// 全局 "*" 冷却的期限，与每个有冷却记录的模型在账户可用的每个启用通道上都冷却到的时刻，取其中最晚者；
// 时刻之前账户为冷却状态、之后不是。候选索引按下标保存这个时刻（见 schedulingIndex.coolUntil），状态统计与换号上限
// 只比较数组，不用逐个账户遍历冷却表；调用方持有 p.mu
func (p *AccountPool) accountCoolingUntilLocked(account *Account) int64 {
	until := cooldownNanos(account.runtime.Cooldowns[globalCooldownKey])
	for key := range account.runtime.Cooldowns {
		if key == globalCooldownKey {
			continue
		}
		until = max(until, p.accountModelCoolingUntilLocked(account, cooldownModelID(key)))
	}
	return until
}

// accountModelCoolingUntilLocked 返回模型在账户每个可用通道上都冷却到的时刻：各可用通道冷却期限中最早的；
// 没有可用通道或任一可用通道没有冷却为 0。通道是否可用与 accountModelCoolingLocked 相同；调用方持有 p.mu
func (p *AccountPool) accountModelCoolingUntilLocked(account *Account, modelID string) int64 {
	until := int64(-1)
	if p.channelEnabledLocked(ChannelPlayground) {
		if model, exists := accountModelLocked(account, modelID); exists && modelAllowedByTier(model, account.BenefitTier) {
			until = cooldownNanos(account.runtime.Cooldowns[modelID])
			if until <= 0 {
				return 0
			}
		}
	}
	if p.channelEnabledLocked(ChannelBuild) && p.buildSupportsModelLocked(account, modelID) {
		channelUntil := cooldownNanos(account.runtime.Cooldowns[ChannelCooldownScope(ChannelBuild, modelID)])
		if channelUntil <= 0 {
			return 0
		}
		if until < 0 || channelUntil < until {
			until = channelUntil
		}
	}
	return max(until, 0)
}

// accountStatusLocked 构建单个账户的状态；调用方持有 p.mu
func (p *AccountPool) accountStatusLocked(account *Account, now time.Time) AccountStatus {
	models := accountStatusModelsLocked(account)
	status := AccountStatus{
		ID:          account.ID,
		Label:       account.Config.Label,
		State:       p.accountStateLocked(account, now),
		Enabled:     account.Config.Enabled,
		Proxy:       account.Config.Proxy,
		Locale:      account.Config.Locale,
		Timezone:    account.Config.Timezone,
		Models:      models,
		BenefitTier: account.BenefitTier.String(),
		// 权益来自目录同步：保存过目录指纹才算已经读取
		BenefitTierKnown: account.runtime.CatalogFingerprint != "",
		Pool:             p.poolOfTierLocked(account.BenefitTier).String(),
		Cooldowns:        cloneCooldowns(account.runtime.Cooldowns),
		Message:          account.stateMessage,
	}
	if !account.LastUsed.IsZero() {
		status.LastUsed = timePointer(account.LastUsed)
	}
	return status
}

// ClassifyCandidates 按 warm 集合分类目标请求的候选账户
func (p *AccountPool) ClassifyCandidates(
	ctx context.Context,
	selection AccountSelection,
	warmAccountIDs []string,
) (AccountCandidateGroups, error) {
	if p == nil {
		return AccountCandidateGroups{}, PoolNotReady(poolMissingReason, nil)
	}
	selection = ScopedSelection(ctx, selection)
	selection.ModelID = strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	if selection.ResourceID != "" {
		if err := p.refreshResource(ctx, selection.ResourceID); err != nil {
			return AccountCandidateGroups{}, err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		p.mu.Lock()
		groups, err := p.classifyCandidatesLocked(selection, warmAccountIDs)
		p.mu.Unlock()
		if err != nil {
			return AccountCandidateGroups{}, err
		}
		available := len(groups.WarmReady)+len(groups.WarmAvailable)+len(groups.WarmBusy)+
			len(groups.StandbyReady)+len(groups.StandbyBusy) > 0
		if attempt == 0 && (!groups.Eligible || !available && !groups.EarliestCooldown.IsZero()) {
			if err := p.refreshSelectionRuntimesThrottled(ctx, selection); err != nil {
				return AccountCandidateGroups{}, err
			}
			continue
		}
		return groups, nil
	}
	return AccountCandidateGroups{}, ErrNoEligibleAccount
}

// classifyCandidatesLocked 按候选索引分类候选账户（见 scheduling_index.go）；调用方持有 p.mu
func (p *AccountPool) classifyCandidatesLocked(
	selection AccountSelection,
	warmAccountIDs []string,
) (AccountCandidateGroups, error) {
	return p.classifyCandidatesAtLocked(selection, warmAccountIDs, time.Now())
}

// classifyCandidatesAtLocked 按 now 时刻分类候选账户：限定账户或资源的选择只涉及一个账户，直接按该账户判断；
// 其余选择用候选集，限定账户集合时只看集合内的账户，排除列表中的账户跳过。结果与 classifyCandidatesBruteLocked 相同
func (p *AccountPool) classifyCandidatesAtLocked(
	selection AccountSelection,
	warmAccountIDs []string,
	now time.Time,
) (AccountCandidateGroups, error) {
	if err := p.validateCandidateSelectionLocked(selection); err != nil {
		return AccountCandidateGroups{}, err
	}
	if strings.TrimSpace(selection.AccountID) != "" || selection.ResourceID != "" {
		indices, err := p.selectionIndicesLocked(selection)
		if err != nil {
			return AccountCandidateGroups{}, err
		}
		groups := AccountCandidateGroups{}
		warm := warmAccountSet(warmAccountIDs)
		channels := p.selectionChannelsLocked(selection)
		for _, index := range indices {
			p.classifyAccountBruteLocked(&groups, p.accounts[index], selection, channels, warm, now)
		}
		return groups, nil
	}
	set := p.candidateSetLocked(selection, now)
	exclusion := p.exclusionLocked(selection)
	if selection.AllowedAccountIDs != nil {
		allowed := make(map[string]struct{}, len(selection.AllowedAccountIDs))
		for _, accountID := range selection.AllowedAccountIDs {
			if accountID = strings.TrimSpace(accountID); accountID != "" {
				allowed[accountID] = struct{}{}
			}
		}
		return p.classifyAllowedLocked(set, allowed, warmAccountIDs, now, exclusion), nil
	}
	return p.classifyIndexedLocked(set, warmAccountIDs, now, exclusion), nil
}

// validateCandidateSelectionLocked 检查号池内存在请求的模型与方法；调用方持有 p.mu，ModelID 已去掉前缀
func (p *AccountPool) validateCandidateSelectionLocked(selection AccountSelection) error {
	modelID := selection.ModelID
	if modelID == "" {
		return nil
	}
	if !p.hasModelCatalogLocked(selection.Pool) {
		return p.catalogMissingErrorLocked(selection.Pool)
	}
	if !p.hasModelLocked(modelID, selection.Pool) {
		return fmt.Errorf("%w: %s", ErrModelNotFound, modelID)
	}
	if selection.Method != "" && !p.hasModelMethodLocked(modelID, selection.Method, selection.Pool) {
		return fmt.Errorf("%w: 模型 %s 不支持 %s", ErrModelNotFound, modelID, selection.Method)
	}
	return nil
}

func warmAccountSet(warmAccountIDs []string) map[string]struct{} {
	warm := make(map[string]struct{}, len(warmAccountIDs))
	for _, accountID := range warmAccountIDs {
		if accountID = strings.TrimSpace(accountID); accountID != "" {
			warm[accountID] = struct{}{}
		}
	}
	return warm
}

// classifyCandidatesBruteLocked 为逐个账户判断的参考实现：遍历选择范围内的全部账户，每个账户判断启用、就绪、
// 各通道支持与冷却后按忙闲分组。只供测试核对候选索引的结果，请求路径不再使用
func (p *AccountPool) classifyCandidatesBruteLocked(
	selection AccountSelection,
	warmAccountIDs []string,
	now time.Time,
) (AccountCandidateGroups, error) {
	if err := p.validateCandidateSelectionLocked(selection); err != nil {
		return AccountCandidateGroups{}, err
	}
	indices, err := p.selectionIndicesBruteLocked(selection)
	if err != nil {
		return AccountCandidateGroups{}, err
	}
	warm := warmAccountSet(warmAccountIDs)
	groups := AccountCandidateGroups{}
	channels := p.selectionChannelsLocked(selection)
	for _, index := range indices {
		p.classifyAccountBruteLocked(&groups, p.accounts[index], selection, channels, warm, now)
	}
	return groups, nil
}

// classifyAccountBruteLocked 逐项判断一个账户并分组：启用、就绪、至少一个通道支持，再看支持通道是否都在冷却
func (p *AccountPool) classifyAccountBruteLocked(
	groups *AccountCandidateGroups,
	account *Account,
	selection AccountSelection,
	channels []Channel,
	warm map[string]struct{},
	now time.Time,
) {
	if account == nil || !account.Config.Enabled || account.State != AccountReady {
		return
	}
	// 每个账户只判断一次各通道是否支持，结果同时用于资格与冷却判断
	var supportedBuffer [4]Channel
	supported := supportedBuffer[:0]
	for _, channel := range channels {
		if p.channelSupportsLocked(account, channel, selection) {
			supported = append(supported, channel)
		}
	}
	if selection.ModelID != "" && len(supported) == 0 {
		return
	}
	groups.Eligible = true
	if until, active := accountChannelsCooldown(account, supported, selection, now); active {
		if groups.EarliestCooldown.IsZero() || until.Before(groups.EarliestCooldown) {
			groups.EarliestCooldown = until
		}
		return
	}
	classifyCandidate(groups, account, warm, p.perAccountConcurrency, sharedSelection(selection), p.rateGateLocked(selection, now))
}

// sharedSelection 判断选择不固定账户：既没有指定账户，也没有绑定文件资源。排空标记只对这类选择生效
func sharedSelection(selection AccountSelection) bool {
	return strings.TrimSpace(selection.AccountID) == "" && selection.ResourceID == ""
}

func (p *AccountPool) tryAcquireLocked(selection AccountSelection, now time.Time) (*AccountLease, time.Time, bool, error) {
	if strings.TrimSpace(selection.AccountID) == "" && selection.ResourceID == "" && selection.AllowedAccountIDs == nil {
		return p.tryAcquireIndexedLocked(selection, now)
	}
	indices, err := p.selectionIndicesLocked(selection)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	waitable := false
	var earliest time.Time
	for _, candidate := range p.channelCandidatesLocked(indices, selection) {
		account := p.accounts[candidate.index]
		if account == nil || !account.Config.Enabled || account.State != AccountReady {
			continue
		}
		if selection.ModelID != "" && !p.channelSupportsLocked(account, candidate.channel, selection) {
			continue
		}
		waitable = true
		lease, err := p.leaseCandidateLocked(account, candidate.channel, selection, now, &earliest)
		if lease != nil || err != nil {
			return lease, time.Time{}, true, err
		}
	}
	return nil, earliest, waitable, nil
}

// leaseCandidateLocked 尝试在一个已确认支持选择的账户通道上取得租约：账户忙碌、通道冷却、每分钟请求已满或文件租约被其他进程占用时
// 返回 nil 并把冷却恢复、窗口释放或轮询时间并入 earliest；排空中的账户不给不固定账户的选择新租约（排空结束时唤醒等待方）。
// 生成选择取得的租约预占一个每分钟名额（见 rate_limit.go）。调用方持有 p.mu
func (p *AccountPool) leaseCandidateLocked(
	account *Account, channel Channel, selection AccountSelection, now time.Time, earliest *time.Time,
) (*AccountLease, error) {
	if account.exclusive || account.exclusiveWaiters > 0 || account.authRefreshers > 0 || account.active >= p.perAccountConcurrency {
		return nil, nil
	}
	if account.draining && sharedSelection(selection) {
		return nil, nil
	}
	if selection.ResourceID == "" {
		if cooldown, active := accountCooldown(account, ChannelCooldownScope(channel, selectionAccessScope(selection)), now); active {
			if earliest.IsZero() || cooldown.Until.Before(*earliest) {
				*earliest = cooldown.Until
			}
			return nil, nil
		}
	}
	// 每分钟请求已满：按忙碌处理，窗口释放时刻并入 earliest 由 AcquireFor 定时复查（窗口释放计时器另发调度信号）；
	// 只因预占而满时等租约发出或释放的唤醒（见 rateChangedLocked）
	gated := generationSelection(selection)
	if until := rateUntilOf(account); p.rateGateLocked(selection, now).full(until) {
		if until != rateUntilPending {
			if at := time.Unix(0, until).UTC(); earliest.IsZero() || at.Before(*earliest) {
				*earliest = at
			}
		}
		return nil, nil
	}
	refreshRuntime := false
	if account.active == 0 {
		leaseLock, leasePath, err := acquireAccountFileLease(account.StoragePath)
		if errors.Is(err, errAccountLeaseBusy) {
			pollAt := now.Add(externalLeasePoll)
			if earliest.IsZero() || pollAt.Before(*earliest) {
				*earliest = pollAt
			}
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		account.leaseLock = leaseLock
		account.leasePath = leasePath
		refreshRuntime = leaseLock != nil
	}
	account.active++
	p.busyChangedLocked(account)
	account.LastUsed = now
	p.lastPicked[selectionAccessScope(selection)] = account.ID
	p.lastPickedChannel[selectionAccessScope(selection)] = channel
	lease := &AccountLease{
		pool: p, account: account, authGeneration: account.authGeneration,
		modelAccessGeneration: account.modelAccessGeneration, refreshRuntime: refreshRuntime, checkedAt: now.UTC(),
		channel: channel,
	}
	// 上限为 0 或文件绑定时不拦但照常预占，重新打开限速时窗口仍然准确
	if gated {
		p.reserveRateLocked(account, lease, now)
	}
	return lease, nil
}

// tryAcquireIndexedLocked 为不限账户的选择在候选集中取租约：只遍历未冷却的候选，账户与通道顺序和
// channelCandidatesLocked 相同（按账户 ID 轮询，上次选中账户在上次通道之后的通道排在最前）。
// 全部通道都在冷却的候选不再逐个检查，最早恢复时间取自冷却堆（含忙碌账户，等待方至多提前醒来复查一次）。
// 排除列表中的账户跳过，不计入可等待与最早恢复时间
func (p *AccountPool) tryAcquireIndexedLocked(selection AccountSelection, now time.Time) (*AccountLease, time.Time, bool, error) {
	set := p.candidateSetLocked(selection, now)
	set.expireLocked(p, now.UnixNano())
	exclusion := p.exclusionLocked(selection)
	waitable := set.liveMembersLocked(exclusion)
	var earliest time.Time
	if until, cooling := set.earliestCoolingLocked(exclusion); cooling {
		earliest = time.Unix(0, until).UTC()
	}
	channels := p.selectionChannelsLocked(selection)
	preferPlayground := selection.PreferPlayground && len(channels) > 1 && slices.Contains(channels, ChannelPlayground)
	if preferPlayground && channels[0] != ChannelPlayground {
		ordered := make([]Channel, 0, len(channels))
		ordered = append(ordered, ChannelPlayground)
		for _, channel := range channels {
			if channel != ChannelPlayground {
				ordered = append(ordered, channel)
			}
		}
		channels = ordered
	}
	rank := func(channel Channel) int { return slices.Index(channels, channel) }
	available := set.available
	start := set.rotationStartLocked(p)
	lastPosition := -1
	var lastChannel Channel
	if p.routingStrategy == "round-robin" && !preferPlayground {
		key := selectionAccessScope(selection)
		if lastChannel = p.lastPickedChannel[key]; lastChannel != "" {
			if at := set.availableSearchLocked(p, p.lastPicked[key]); at < len(available) && p.accounts[available[at]].ID == p.lastPicked[key] {
				lastPosition = available[at]
			}
		}
	}
	try := func(position int, channel Channel) (*AccountLease, error) {
		account := p.accounts[position]
		if account == nil || !account.Config.Enabled || account.State != AccountReady || exclusion.has(position) {
			return nil, nil
		}
		if selection.ModelID != "" && !p.channelSupportsLocked(account, channel, selection) {
			return nil, nil
		}
		return p.leaseCandidateLocked(account, channel, selection, now, &earliest)
	}
	if lastPosition >= 0 {
		for _, channel := range channels {
			if rank(channel) > rank(lastChannel) {
				if lease, err := try(lastPosition, channel); lease != nil || err != nil {
					return lease, time.Time{}, true, err
				}
			}
		}
	}
	for offset := range available {
		position := available[(start+offset)%len(available)]
		if position == lastPosition {
			continue
		}
		for _, channel := range channels {
			if lease, err := try(position, channel); lease != nil || err != nil {
				return lease, time.Time{}, true, err
			}
		}
	}
	if lastPosition >= 0 {
		for _, channel := range channels {
			if rank(channel) <= rank(lastChannel) {
				if lease, err := try(lastPosition, channel); lease != nil || err != nil {
					return lease, time.Time{}, true, err
				}
			}
		}
	}
	return nil, earliest, waitable, nil
}

// hasModelLocked 等按号池目录汇总判断模型存在性（见 pool_catalog_cache.go），原先每次扫描全部账户的全部模型
func (p *AccountPool) hasModelLocked(modelID string, scope PoolScope) bool {
	if _, exists := p.catalogSummaryLocked(scope).models[modelID]; exists {
		return true
	}
	return p.hasBuildModelLocked(modelID, "", scope)
}

func (p *AccountPool) hasModelCatalogLocked(scope PoolScope) bool {
	return p.catalogSummaryLocked(scope).accounts > 0
}

func (p *AccountPool) hasModelMethodLocked(modelID string, method string, scope PoolScope) bool {
	if entry := p.catalogSummaryLocked(scope).models[modelID]; entry != nil {
		if _, exists := entry.methods[method]; exists {
			return true
		}
	}
	return p.hasBuildModelLocked(modelID, method, scope)
}

func (p *AccountPool) hasModelCapabilityLocked(modelID string, capability string, scope PoolScope) bool {
	entry := p.catalogSummaryLocked(scope).models[modelID]
	if entry == nil {
		return false
	}
	_, exists := entry.capabilities[capability]
	return exists
}

// BootstrapModels 返回账户实时目录中的 WAA 初始化模型
func (p *AccountPool) BootstrapModels(accountID string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return nil, fmt.Errorf("账户不存在: %s", accountID)
	}
	models := accountBootstrapModelsLocked(account)
	if len(models) == 0 {
		return nil, fmt.Errorf("账户 %s 的实时目录没有可用 WAA 初始化模型", account.ID)
	}
	return slices.Clone(models), nil
}

// BootstrapModel 返回账户使用的通用 WAA 初始化模型
func (p *AccountPool) BootstrapModel(accountID string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return "", fmt.Errorf("账户不存在: %s", accountID)
	}
	models := accountBootstrapModelsLocked(account)
	if len(models) > 0 {
		return models[0], nil
	}
	return "", fmt.Errorf("账户 %s 的实时目录没有可用 WAA 初始化模型", account.ID)
}

// selectionIndicesLocked 返回选择范围内的账户下标（号池、指定账户或资源、限定账户集合、排除列表），按轮询起点旋转
func (p *AccountPool) selectionIndicesLocked(selection AccountSelection) ([]int, error) {
	var allowed map[string]struct{}
	if selection.AllowedAccountIDs != nil {
		allowed = make(map[string]struct{}, len(selection.AllowedAccountIDs))
		for _, accountID := range selection.AllowedAccountIDs {
			if accountID = strings.TrimSpace(accountID); accountID != "" {
				allowed[accountID] = struct{}{}
			}
		}
	}
	excluded := excludedAccountSet(selection)
	accountID := strings.TrimSpace(selection.AccountID)
	if selection.ResourceID != "" {
		owner, exists := p.resources[selection.ResourceID]
		if !exists {
			return nil, ErrResourceNotFound
		}
		if accountID != "" && accountID != owner {
			return nil, fmt.Errorf("资源 %s 绑定账户 %s", selection.ResourceID, owner)
		}
		// 资源绑定在号池之外的账户上：直接拒绝，不能改用池外账户
		if err := p.resourcePoolErrorLocked(selection.Pool, selection.ResourceID, owner); err != nil {
			return nil, err
		}
		accountID = owner
	}
	if accountID != "" {
		// 指定账户按账户表定位，下标由候选索引维护（账户增删后重建时重新编号）；原先逐个比对全部账户的 ID，
		// 账户上万时每次备用账户晋升为 Worker 的选号都要在锁内扫一遍
		account := p.byID[accountID]
		if account == nil {
			return nil, ErrNoEligibleAccount
		}
		if allowed != nil {
			if _, exists := allowed[accountID]; !exists {
				return nil, ErrNoEligibleAccount
			}
		}
		if _, exists := excluded[accountID]; exists {
			return nil, ErrNoEligibleAccount
		}
		if !p.inPoolLocked(selection.Pool, account) {
			return nil, ErrNoEligibleAccount
		}
		p.schedulingLocked()
		return []int{account.position}, nil
	}
	var indices []int
	if allowed != nil && len(allowed)*4 < len(p.accounts) {
		// 限定少量账户时只查这些账户的下标再排序；原先过滤全部有序账户，账户上万时每次选号都要扫一遍
		p.schedulingLocked()
		indices = make([]int, 0, len(allowed))
		for accountID := range allowed {
			account := p.byID[accountID]
			if account == nil || !p.inPoolLocked(selection.Pool, account) {
				continue
			}
			if _, exists := excluded[accountID]; exists {
				continue
			}
			indices = append(indices, account.position)
		}
		sort.Slice(indices, func(left, right int) bool { return p.accounts[indices[left]].ID < p.accounts[indices[right]].ID })
	} else {
		// 按缓存的有序下标过滤，结果与每次收集后按 ID 排序相同；原先每次调度都要对全部账户排序
		sorted := p.sortedAccountIndicesLocked()
		indices = make([]int, 0, len(sorted))
		for _, index := range sorted {
			account := p.accounts[index]
			if account == nil || !p.inPoolLocked(selection.Pool, account) {
				continue
			}
			if allowed != nil {
				if _, exists := allowed[account.ID]; !exists {
					continue
				}
			}
			if _, exists := excluded[account.ID]; exists {
				continue
			}
			indices = append(indices, index)
		}
	}
	return p.rotateIndicesLocked(indices, selection), nil
}

// rotateIndicesLocked 轮询策略下把有序下标旋转到上次选中账户之后开始
func (p *AccountPool) rotateIndicesLocked(indices []int, selection AccountSelection) []int {
	if p.routingStrategy == "round-robin" {
		last := p.lastPicked[selectionAccessScope(selection)]
		start := sort.Search(len(indices), func(index int) bool { return p.accounts[indices[index]].ID > last })
		indices = append(indices[start:], indices[:start]...)
	}
	return indices
}

// selectionIndicesBruteLocked 为 selectionIndicesLocked 的原实现：过滤全部有序账户，按 ID 逐个判断限定集合与排除列表。
// 只供测试核对候选索引的结果
func (p *AccountPool) selectionIndicesBruteLocked(selection AccountSelection) ([]int, error) {
	if strings.TrimSpace(selection.AccountID) != "" || selection.ResourceID != "" {
		return p.selectionIndicesLocked(selection)
	}
	var allowed map[string]struct{}
	if selection.AllowedAccountIDs != nil {
		allowed = make(map[string]struct{}, len(selection.AllowedAccountIDs))
		for _, accountID := range selection.AllowedAccountIDs {
			if accountID = strings.TrimSpace(accountID); accountID != "" {
				allowed[accountID] = struct{}{}
			}
		}
	}
	excluded := make(map[string]struct{}, len(selection.ExcludedAccountIDs))
	for _, accountID := range selection.ExcludedAccountIDs {
		excluded[strings.TrimSpace(accountID)] = struct{}{}
	}
	sorted := p.sortedAccountIndicesLocked()
	indices := make([]int, 0, len(sorted))
	for _, index := range sorted {
		account := p.accounts[index]
		if account == nil || !p.inPoolLocked(selection.Pool, account) {
			continue
		}
		if _, exists := allowed[account.ID]; allowed != nil && !exists {
			continue
		}
		if _, exists := excluded[account.ID]; exists {
			continue
		}
		indices = append(indices, index)
	}
	return p.rotateIndicesLocked(indices, selection), nil
}

// SetRoutingStrategy 设置账户轮询或优先填满策略
func (p *AccountPool) SetRoutingStrategy(strategy string) {
	p.mu.Lock()
	p.routingStrategy = strategy
	p.mu.Unlock()
}

// OrderCandidates 按当前策略排列候选账户而不推进轮询位置
func (p *AccountPool) OrderCandidates(accountIDs []string, modelAccessScope string) []string {
	if len(accountIDs) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	indices, _ := p.selectionIndicesLocked(AccountSelection{AllowedAccountIDs: accountIDs, ModelAccessScope: modelAccessScope})
	ordered := make([]string, 0, len(indices))
	for _, index := range indices {
		ordered = append(ordered, p.accounts[index].ID)
	}
	return ordered
}

func (p *AccountPool) setAccountState(accountID string, state AccountState, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[accountID]
	if account == nil {
		return fmt.Errorf("账户不存在: %s", accountID)
	}
	if !account.Config.Enabled {
		account.State = AccountDisabled
	} else {
		account.State = state
	}
	account.stateMessage = strings.TrimSpace(reason)
	if state == AccountReady || state == AccountAuthRequired {
		// 管理操作（重新登录、验证）写入的认证结果同样参与顺序判断：此前开始的请求较晚返回的结果不能覆盖它
		account.authCheckedAt = time.Now().UTC()
	}
	p.accountChangedLocked(account)
	p.notifyLocked()
	return nil
}

// EnabledAccounts 返回已启用账户 ID 与其中 ready 或 busy 的账户数量
func (p *AccountPool) EnabledAccounts() ([]string, int) {
	return p.EnabledAccountsIn(PoolScopeAll)
}

// SetDraining 设置账户是否排空，返回标记是否变化（账户不存在时为 false）。排空中的账户不再给不固定账户的选择
// （AccountID 与 ResourceID 都为空）新租约，分类时热账户分进 WarmBusy、备用账户分进 StandbyBusy，进行中的租约照常结束；
// 指定账户与文件绑定的请求照常使用。排空不改变对外状态、Activity 与预热概况，由调用方在 Worker 关闭或放弃淘汰时清除；
// 账户移出号池时自动清除。标记变化时唤醒等待槽位的请求
func (p *AccountPool) SetDraining(accountID string, on bool) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil || account.draining == on {
		return false
	}
	account.draining = on
	p.busyChangedLocked(account)
	p.notifyLocked()
	return true
}

// Draining 返回账户是否在排空中
func (p *AccountPool) Draining(accountID string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	return account != nil && account.draining
}

// Activity 返回账户是否存在活动租约或独占操作及最近使用时间
func (p *AccountPool) Activity(accountID string) (bool, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[accountID]
	if account == nil {
		return false, time.Time{}
	}
	return account.exclusive || account.exclusiveWaiters > 0 || account.authRefreshers > 0 || account.active > 0, account.LastUsed
}

// Changed 返回账户池下一次租约或状态变化时关闭的通道
func (p *AccountPool) Changed() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

func (p *AccountPool) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func loadAccount(directory string) (*Account, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("解析账户目录: %w", err)
	}
	id := filepath.Base(directory)
	if id == "." || id == string(filepath.Separator) || strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("账户目录缺少稳定 ID")
	}
	configPath := filepath.Join(directory, accountConfigName)
	storagePath := filepath.Join(directory, storageStateName)
	accountConfig, err := readAccountConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("账户 %s: %w", id, err)
	}
	state, err := LoadStorageState(storagePath)
	if err != nil {
		return nil, fmt.Errorf("账户 %s: %w", id, err)
	}
	runtimePath := filepath.Join(directory, runtimeStateName)
	runtimeState, err := readRuntimeOrEmpty(runtimePath)
	if err != nil {
		return nil, fmt.Errorf("账户 %s: %w", id, err)
	}
	return &Account{
		ID:           id,
		Directory:    directory,
		ConfigPath:   configPath,
		StoragePath:  storagePath,
		RuntimePath:  runtimePath,
		Config:       accountConfig,
		StorageState: state,
		BenefitTier:  runtimeState.BenefitTier,
		State:        initialAccountState(accountConfig, state),
		runtime:      runtimeState,
	}, nil
}

func initialAccountState(accountConfig AccountConfig, state StorageState) AccountState {
	if !accountConfig.Enabled {
		return AccountDisabled
	}
	now := time.Now()
	for _, item := range signatureCookies {
		if _, ok := state.CookieValue(item.name, aiStudioOrigin+"/", now); !ok {
			return AccountAuthRequired
		}
	}
	return AccountReady
}

func readAccountConfig(filePath string) (AccountConfig, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return AccountConfig{}, fmt.Errorf("读取 %s: %w", accountConfigName, err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	// 缺少 enabled 字段时按启用（与 DefaultAccountConfig 一致）：外部程序写入的 account.json 常常不带这一项，
	// 原先按零值读成停用，重启后这些账户全部显示停用；显式写了 false 的仍按停用读取
	value := AccountConfig{Enabled: true}
	if err := decoder.Decode(&value); err != nil {
		return AccountConfig{}, fmt.Errorf("解析 %s: %w", accountConfigName, err)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return AccountConfig{}, fmt.Errorf("解析 %s: %w", accountConfigName, err)
	}
	if err := value.Validate(); err != nil {
		return AccountConfig{}, err
	}
	return value, nil
}

func writeAccountConfig(filePath string, value AccountConfig) error {
	if err := value.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 %s: %w", accountConfigName, err)
	}
	return atomicWriteFile(filePath, append(data, '\n'), 0o600)
}

// readRuntime 读取账户运行态（冷却、资源绑定、模型资格等缓存）。
//
// 运行态只是缓存，不能因为它让账户不可用：未知字段（新版本写入或回退版本）直接忽略；
// 文件损坏时返回 errRuntimeCorrupt，由 readRuntimeOrEmpty / readRuntimeLocked 决定按空运行态继续还是备份并用内存运行态重写。
// 原先严格解析，一个账户的坏文件会让轮询到它的请求和全部候选的分类都失败，启动时还会直接丢掉该账户
func readRuntime(filePath string) (accountRuntimeState, error) {
	value := emptyRuntimeState()
	data, err := os.ReadFile(filePath)
	if os.IsNotExist(err) {
		return value, nil
	}
	if err != nil {
		return accountRuntimeState{}, fmt.Errorf("读取 %s: %w", runtimeStateName, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decodeErr := decoder.Decode(&value)
	if decodeErr == nil {
		decodeErr = ensureJSONEnd(decoder)
	}
	if decodeErr != nil {
		return accountRuntimeState{}, fmt.Errorf("解析 %s: %w: %w", runtimeStateName, errRuntimeCorrupt, decodeErr)
	}
	if value.Cooldowns == nil {
		value.Cooldowns = make(map[string]CooldownState)
	}
	if value.Resources == nil {
		value.Resources = make(map[string]ResourceBinding)
	}
	if value.ModelAccess == nil {
		value.ModelAccess = make(map[string]ModelAccess)
	}
	return value, nil
}

// errRuntimeCorrupt 表示运行态文件无法解析（手工编辑出错或其他版本写入了不兼容的字段）
var errRuntimeCorrupt = errors.New("运行态文件损坏")

// readRuntimeOrEmpty 用于加载账户（还没有内存运行态、也未持有运行态文件锁）：文件损坏时按空运行态继续，不让账户失败；
// 不改名备份，避免与其他进程持锁写入竞争，之后第一次持锁读写时再备份并重写
func readRuntimeOrEmpty(filePath string) (accountRuntimeState, error) {
	value, err := readRuntime(filePath)
	if errors.Is(err, errRuntimeCorrupt) {
		slog.Warn("账户运行态文件损坏，按空运行态继续", "file", filePath, "error", err)
		return emptyRuntimeState(), nil
	}
	return value, err
}

// readRuntimeLocked 在持有运行态文件锁时读取：文件损坏时备份为 .corrupt 并用内存中的运行态重写，
// 不用空运行态替换内存里仍然正确的资源绑定与冷却
func readRuntimeLocked(filePath string, memory accountRuntimeState) (accountRuntimeState, error) {
	value, err := readRuntime(filePath)
	if !errors.Is(err, errRuntimeCorrupt) {
		return value, err
	}
	backup := filePath + ".corrupt"
	if renameErr := os.Rename(filePath, backup); renameErr != nil {
		return accountRuntimeState{}, fmt.Errorf("%w（备份失败: %v）", err, renameErr)
	}
	if writeErr := writeRuntime(filePath, memory); writeErr != nil {
		return accountRuntimeState{}, writeErr
	}
	slog.Warn("账户运行态文件损坏，已备份并按内存中的运行态重写", "file", filePath, "backup", backup, "error", err)
	return memory, nil
}

func emptyRuntimeState() accountRuntimeState {
	return accountRuntimeState{
		Cooldowns:   make(map[string]CooldownState),
		Resources:   make(map[string]ResourceBinding),
		ModelAccess: make(map[string]ModelAccess),
	}
}

func writeRuntime(filePath string, value accountRuntimeState) error {
	if filePath == "" {
		return nil
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 %s: %w", runtimeStateName, err)
	}
	return atomicWriteFile(filePath, append(data, '\n'), 0o600)
}

type runtimeStateMutation func(*Account, *accountRuntimeState) (bool, func(*Account), error)

func (p *AccountPool) updateRuntime(accountID string, mutate runtimeStateMutation) (changed bool, resultErr error) {
	return p.updateRuntimeContext(context.Background(), accountID, mutate)
}

func (p *AccountPool) updateRuntimeContext(
	ctx context.Context,
	accountID string,
	mutate runtimeStateMutation,
) (changed bool, resultErr error) {
	p.mu.Lock()
	account := p.byID[strings.TrimSpace(accountID)]
	p.mu.Unlock()
	if account == nil {
		return false, fmt.Errorf("账户不存在: %s", accountID)
	}

	account.runtimeMu.Lock()
	defer account.runtimeMu.Unlock()
	p.mu.Lock()
	currentAccount := p.byID[account.ID]
	p.mu.Unlock()
	if currentAccount != account {
		return false, fmt.Errorf("账户不存在: %s", account.ID)
	}
	runtimeLock, err := lockRuntimeState(ctx, account)
	if err != nil {
		return false, err
	}
	defer func() {
		if runtimeLock != nil {
			resultErr = errors.Join(resultErr, runtimeLock.Unlock())
		}
	}()
	if err := validatePersistentAccountFiles(account); err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			p.markStaleAccountUnavailable(account)
		}
		return false, err
	}

	current := cloneRuntime(account.runtime)
	if account.RuntimePath == "" {
		current.BenefitTier = account.BenefitTier
	} else {
		current, err = readRuntimeLocked(account.RuntimePath, current)
		if err != nil {
			return false, err
		}
	}
	p.mu.Lock()
	if p.byID[account.ID] != account {
		p.mu.Unlock()
		return false, fmt.Errorf("账户不存在: %s", account.ID)
	}
	refreshed, err := p.syncAccountRuntimeLocked(account, current)
	if err != nil {
		p.mu.Unlock()
		return false, err
	}
	working := cloneRuntime(current)
	changed, apply, err := mutate(account, &working)
	p.mu.Unlock()
	if err != nil {
		return false, err
	}
	if changed {
		if err := writeRuntime(account.RuntimePath, working); err != nil {
			return false, err
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID[account.ID] != account {
		return false, fmt.Errorf("账户不存在: %s", account.ID)
	}
	synced, err := p.syncAccountRuntimeLocked(account, working)
	if err != nil {
		return false, err
	}
	if apply != nil {
		apply(account)
	}
	if changed || refreshed || synced || apply != nil {
		p.notifyLocked()
	}
	return changed, nil
}

func (p *AccountPool) syncAccountRuntimeLocked(account *Account, runtimeState accountRuntimeState) (bool, error) {
	for resourceID := range runtimeState.Resources {
		if owner, exists := p.resources[resourceID]; exists && owner != account.ID {
			return false, fmt.Errorf("资源 %s 已绑定账户 %s", resourceID, owner)
		}
	}
	changed := account.BenefitTier != runtimeState.BenefitTier || !reflect.DeepEqual(account.runtime, runtimeState)
	if !changed {
		for resourceID := range runtimeState.Resources {
			if p.resources[resourceID] != account.ID {
				changed = true
				break
			}
		}
	}
	if !changed {
		for resourceID, owner := range p.resources {
			if owner == account.ID {
				if _, exists := runtimeState.Resources[resourceID]; !exists {
					changed = true
					break
				}
			}
		}
	}
	if !changed {
		return false, nil
	}
	for resourceID, owner := range p.resources {
		if owner == account.ID {
			delete(p.resources, resourceID)
		}
	}
	catalogChanged := account.runtime.BenefitTier != runtimeState.BenefitTier ||
		account.runtime.CatalogFingerprint != runtimeState.CatalogFingerprint
	if account.BenefitTier != runtimeState.BenefitTier {
		// 权益变化会把账户换到另一个号池
		p.eligibilityVersion++
	}
	account.runtime = cloneRuntime(runtimeState)
	account.BenefitTier = runtimeState.BenefitTier
	for resourceID := range runtimeState.Resources {
		p.resources[resourceID] = account.ID
	}
	if catalogChanged {
		account.modelAccessGeneration++
	}
	// 冷却表或权益变化后更新候选集
	p.accountChangedLocked(account)
	return true, nil
}

func lockRuntimeState(ctx context.Context, account *Account) (*flock.Flock, error) {
	if account == nil || account.RuntimePath == "" {
		return nil, nil
	}
	accountDirectory := account.Directory
	if accountDirectory == "" {
		accountDirectory = filepath.Dir(account.RuntimePath)
	}
	leaseDirectory := filepath.Join(filepath.Dir(accountDirectory), ".leases")
	if err := os.MkdirAll(leaseDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("创建账户状态锁目录: %w", err)
	}
	lock := flock.New(filepath.Join(leaseDirectory, filepath.Base(accountDirectory)+".runtime.lock"))
	lockCtx, cancel := context.WithTimeout(ctx, runtimeLockLimit)
	defer cancel()
	_, err := lock.TryLockContext(lockCtx, runtimeLockPoll)
	if err != nil {
		return nil, fmt.Errorf("锁定账户运行状态: %w", err)
	}
	return lock, nil
}

func validatePersistentAccountFiles(account *Account) error {
	if account == nil || account.ConfigPath == "" {
		return nil
	}
	for _, filePath := range []string{account.ConfigPath, account.StoragePath} {
		if strings.TrimSpace(filePath) == "" {
			return fmt.Errorf("%w: %s", ErrAccountNotFound, account.ID)
		}
		info, err := os.Stat(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: %s", ErrAccountNotFound, account.ID)
			}
			return fmt.Errorf("读取账户持久文件: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s", ErrAccountNotFound, account.ID)
		}
	}
	return nil
}

func (p *AccountPool) refreshAccountRuntime(ctx context.Context, account *Account) (resultErr error) {
	account.runtimeMu.Lock()
	defer account.runtimeMu.Unlock()
	p.mu.Lock()
	currentAccount := p.byID[account.ID]
	p.mu.Unlock()
	if currentAccount != account {
		return fmt.Errorf("%w: %s", ErrAccountNotFound, account.ID)
	}
	runtimeLock, err := lockRuntimeState(ctx, account)
	if err != nil {
		return err
	}
	defer func() {
		if runtimeLock != nil {
			resultErr = errors.Join(resultErr, runtimeLock.Unlock())
		}
	}()
	if err := validatePersistentAccountFiles(account); err != nil {
		return err
	}
	current := cloneRuntime(account.runtime)
	if account.RuntimePath == "" {
		current.BenefitTier = account.BenefitTier
	} else {
		current, err = readRuntimeLocked(account.RuntimePath, current)
		if err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID[account.ID] != account {
		return fmt.Errorf("%w: %s", ErrAccountNotFound, account.ID)
	}
	changed, err := p.syncAccountRuntimeLocked(account, current)
	if changed {
		p.notifyLocked()
	}
	return err
}

// accountRuntimeRefreshResult 保存单账户运行态刷新结果
type accountRuntimeRefreshResult struct {
	account *Account
	err     error
}

// runtimeRefreshConcurrency 为同时从磁盘刷新运行态的账户数上限：原先每个账户一个 goroutine，
// 上万账户一次刷新就是上万个 goroutine 同时抢文件锁与账户池锁
const runtimeRefreshConcurrency = 32

// refreshAccountRuntimes 并发刷新独立账户运行态
func (p *AccountPool) refreshAccountRuntimes(ctx context.Context, accounts []*Account) []accountRuntimeRefreshResult {
	results := make([]accountRuntimeRefreshResult, len(accounts))
	var refreshes sync.WaitGroup
	slots := make(chan struct{}, runtimeRefreshConcurrency)
	for index, account := range accounts {
		if account == nil {
			continue
		}
		slots <- struct{}{}
		refreshes.Add(1)
		go func(index int, account *Account) {
			defer refreshes.Done()
			defer func() { <-slots }()
			results[index] = accountRuntimeRefreshResult{account: account, err: p.refreshAccountRuntime(ctx, account)}
		}(index, account)
	}
	refreshes.Wait()
	return results
}

func (p *AccountPool) refreshResource(ctx context.Context, resourceID string) error {
	resourceID = strings.TrimSpace(resourceID)
	p.mu.Lock()
	ownerID, exists := p.resources[resourceID]
	owner := p.byID[ownerID]
	if exists && owner != nil {
		p.mu.Unlock()
		if err := p.refreshAccountRuntime(ctx, owner); err != nil {
			if errors.Is(err, ErrAccountNotFound) {
				p.markStaleAccountUnavailable(owner)
				return fmt.Errorf("%w: %s", ErrResourceNotFound, resourceID)
			}
			return err
		}
		return nil
	}
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	var failures []error
	for _, result := range p.refreshAccountRuntimes(ctx, accounts) {
		if result.err == nil {
			continue
		}
		if errors.Is(result.err, ErrAccountNotFound) {
			p.markStaleAccountUnavailable(result.account)
			continue
		}
		failures = append(failures, result.err)
	}
	p.mu.Lock()
	_, found := p.resources[resourceID]
	p.mu.Unlock()
	if found {
		return nil
	}
	return errors.Join(failures...)
}

func (p *AccountPool) refreshSelectionRuntimes(ctx context.Context, selection AccountSelection) error {
	allowed := make(map[string]struct{}, len(selection.AllowedAccountIDs))
	for _, accountID := range selection.AllowedAccountIDs {
		if accountID = strings.TrimSpace(accountID); accountID != "" {
			allowed[accountID] = struct{}{}
		}
	}
	excluded := excludedAccountSet(selection)
	requestedAccountID := strings.TrimSpace(selection.AccountID)
	modelID := strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	supports := func(account *Account) bool {
		if account == nil || !account.Config.Enabled || account.State != AccountReady || !p.inPoolLocked(selection.Pool, account) {
			return false
		}
		if modelID == "" {
			return true
		}
		for _, model := range account.Models {
			if modelMatchesID(model, modelID) && (strings.TrimSpace(selection.Method) == "" || hasMethod(model, selection.Method)) {
				return true
			}
		}
		return false
	}
	p.mu.Lock()
	var accounts []*Account
	if requestedAccountID != "" {
		if account := p.byID[requestedAccountID]; supports(account) {
			accounts = append(accounts, account)
		}
	} else if selection.ResourceID == "" {
		// 不限账户时只刷新候选集中冷却中的成员：刷新是为了感知其他进程清除的冷却，未冷却的候选没有可刷新的；
		// 原先在锁内扫描全部账户的目录，并刷新全部支持该模型的账户
		now := time.Now()
		set := p.candidateSetLocked(selection, now)
		set.expireLocked(p, now.UnixNano())
		for _, position := range p.candidatePositionsLocked(set) {
			if set.until[position] <= 0 {
				continue
			}
			account := p.accounts[position]
			if selection.AllowedAccountIDs != nil {
				if _, exists := allowed[account.ID]; !exists {
					continue
				}
			}
			if _, exists := excluded[account.ID]; exists {
				continue
			}
			accounts = append(accounts, account)
		}
	}
	p.mu.Unlock()
	var failures []error
	for _, result := range p.refreshAccountRuntimes(ctx, accounts) {
		if result.err == nil {
			continue
		}
		if errors.Is(result.err, ErrAccountNotFound) {
			p.markStaleAccountUnavailable(result.account)
			continue
		}
		failures = append(failures, result.err)
	}
	return errors.Join(failures...)
}

func cloneRuntime(value accountRuntimeState) accountRuntimeState {
	result := accountRuntimeState{
		Cooldowns:          make(map[string]CooldownState, len(value.Cooldowns)),
		Resources:          make(map[string]ResourceBinding, len(value.Resources)),
		ModelAccess:        make(map[string]ModelAccess, len(value.ModelAccess)),
		BenefitTier:        value.BenefitTier,
		CatalogFingerprint: value.CatalogFingerprint,
	}
	for key, cooldown := range value.Cooldowns {
		result.Cooldowns[key] = cooldown
	}
	for key, binding := range value.Resources {
		if binding.Video != nil {
			metadata := *binding.Video
			binding.Video = &metadata
		}
		result.Resources[key] = binding
	}
	for key, access := range value.ModelAccess {
		result.ModelAccess[key] = access
	}
	return result
}

func accountCatalogFingerprint(tier BenefitTier, models []Model) (string, error) {
	catalog := cloneAccountModels(models)
	sort.Slice(catalog, func(left int, right int) bool {
		return catalog[left].ID < catalog[right].ID
	})
	data, err := json.Marshal(struct {
		Tier   BenefitTier `json:"tier"`
		Models []Model     `json:"models"`
	}{Tier: tier, Models: catalog})
	if err != nil {
		return "", fmt.Errorf("编码账户模型目录指纹: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func accountCooldown(account *Account, modelID string, now time.Time) (CooldownState, bool) {
	var selected CooldownState
	for _, key := range []string{globalCooldownKey, modelID} {
		if key == "" {
			continue
		}
		cooldown, exists := account.runtime.Cooldowns[key]
		if !exists || !cooldown.Active(now) {
			continue
		}
		if selected.Until.IsZero() || cooldown.Until.After(selected.Until) {
			selected = cooldown
		}
	}
	return selected, !selected.Until.IsZero()
}

// shortCooldownActive 判断冷却是仍生效、剩余不到 ShortCooldownMax 的分钟限额或限流冷却（rateCooldownReason）：账号全局 * 上的
// 这类冷却只等到期，同账号其他请求或非生成操作的成功不提前清除（取得租约更晚的请求先到上游并成功，证明不了这一分钟的额度已经恢复）。
// 其他原因的冷却（转录上传等暂时故障的 30 秒冷却、长冷却的最后一段）照旧被成功清除
func shortCooldownActive(cooldown CooldownState, now time.Time) bool {
	return cooldown.Active(now) && cooldown.Until.Sub(now) < ShortCooldownMax && rateCooldownReason(cooldown.Reason)
}

func selectionAccessScope(selection AccountSelection) string {
	if scope := strings.TrimSpace(selection.ModelAccessScope); scope != "" {
		return scope
	}
	return strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
}

func acquireAccountFileLease(storagePath string) (*flock.Flock, string, error) {
	if storagePath == "" {
		return nil, "", nil
	}
	accountDirectory := filepath.Dir(storagePath)
	leaseDirectory := filepath.Join(filepath.Dir(accountDirectory), ".leases")
	if err := os.MkdirAll(leaseDirectory, 0o700); err != nil {
		return nil, "", fmt.Errorf("创建账户租约目录: %w", err)
	}
	leasePath := filepath.Join(leaseDirectory, filepath.Base(accountDirectory)+".lock")
	leaseLock := flock.New(leasePath)
	locked, err := leaseLock.TryLock()
	if err != nil {
		return nil, leasePath, fmt.Errorf("锁定账户租约: %w", err)
	}
	if !locked {
		return nil, leasePath, errAccountLeaseBusy
	}
	return leaseLock, leasePath, nil
}

func acquireAccountPublishLease(account *Account, validate bool) (*AccountPublishLease, error) {
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return nil, fmt.Errorf("账户未初始化")
	}
	requestLock, _, err := acquireAccountFileLease(account.StoragePath)
	if errors.Is(err, errAccountLeaseBusy) {
		return nil, fmt.Errorf("%w: %s", ErrAccountLeased, account.ID)
	}
	if err != nil {
		return nil, err
	}
	account.runtimeMu.Lock()
	runtimeLock, err := lockRuntimeState(context.Background(), account)
	if err != nil {
		account.runtimeMu.Unlock()
		if requestLock != nil {
			_ = requestLock.Unlock()
		}
		return nil, err
	}
	if validate {
		if err := validatePersistentAccountFiles(account); err != nil {
			if runtimeLock != nil {
				_ = runtimeLock.Unlock()
			}
			account.runtimeMu.Unlock()
			if requestLock != nil {
				_ = requestLock.Unlock()
			}
			return nil, err
		}
	}
	account.storageMu.Lock()
	account.persistenceLocked = true
	account.storageMu.Unlock()
	return &AccountPublishLease{account: account, requestLock: requestLock, runtimeLock: runtimeLock}, nil
}

// Release 结束新账户运行时发布窗口
func (lease *AccountPublishLease) Release() error {
	if lease == nil || lease.account == nil {
		return nil
	}
	lease.once.Do(func() {
		lease.account.storageMu.Lock()
		lease.account.persistenceLocked = false
		lease.account.storageMu.Unlock()
		if lease.runtimeLock != nil {
			lease.err = lease.runtimeLock.Unlock()
		}
		lease.account.runtimeMu.Unlock()
		if lease.requestLock != nil {
			lease.err = errors.Join(lease.err, lease.requestLock.Unlock())
		}
	})
	return lease.err
}

// AcquireAccountRuntimeLease 锁定当前用户下的账户 WAA runtime
func AcquireAccountRuntimeLease(accountID string) (*AccountRuntimeLease, error) {
	accountID, err := normalizeAccountEmail(accountID)
	if err != nil {
		return nil, err
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("读取用户缓存目录: %w", err)
	}
	directory := filepath.Join(cacheRoot, "AIStudio2API", "runtime-leases")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("创建 WAA runtime 租约目录: %w", err)
	}
	lock := flock.New(filepath.Join(directory, accountID+".lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("锁定账户 WAA runtime: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("%w: %s 已由另一个 AIStudio2API runtime 使用", ErrAccountLeased, accountID)
	}
	return &AccountRuntimeLease{lock: lock}, nil
}

// Release 释放账户 WAA runtime 锁
func (lease *AccountRuntimeLease) Release() error {
	if lease == nil || lease.lock == nil {
		return nil
	}
	lease.once.Do(func() {
		lease.err = lease.lock.Unlock()
	})
	return lease.err
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("文件包含多个 JSON 值")
	}
	return err
}

func accountEmailID(accountConfig AccountConfig, state StorageState) (string, error) {
	candidate := strings.TrimSpace(accountConfig.Label)
	if extension, exists, err := state.AuthExtension(); err != nil {
		return "", err
	} else if exists && strings.TrimSpace(extension.Source.Email) != "" {
		candidate = strings.TrimSpace(extension.Source.Email)
	}
	return normalizeAccountEmail(candidate)
}

func normalizeAccountEmail(candidate string) (string, error) {
	candidate = strings.TrimSpace(candidate)
	address, err := mail.ParseAddress(candidate)
	if err != nil || !strings.EqualFold(strings.TrimSpace(address.Address), candidate) {
		return "", fmt.Errorf("账户必须填写 Google 邮箱")
	}
	id := strings.ToLower(strings.TrimSpace(address.Address))
	if id == "." || id == ".." || strings.ContainsAny(id, `<>:"/\|?*`) {
		return "", fmt.Errorf("账户邮箱不能用作目录名: %s", id)
	}
	return id, nil
}

func cloneAccountModels(models []Model) []Model {
	result := make([]Model, len(models))
	for index, model := range models {
		result[index] = model
		result[index].Methods = append([]string(nil), model.Methods...)
		result[index].AccessModes = append([]int64(nil), model.AccessModes...)
		if model.Capabilities != nil {
			result[index].Capabilities = make(map[string]bool, len(model.Capabilities))
			for key, value := range model.Capabilities {
				result[index].Capabilities[key] = value
			}
		}
		if model.CapabilityOptions != nil {
			result[index].CapabilityOptions = make(map[string][]string, len(model.CapabilityOptions))
			for key, value := range model.CapabilityOptions {
				result[index].CapabilityOptions[key] = append([]string(nil), value...)
			}
		}
	}
	return result
}

func canonicalAccountModelID(account *Account, modelID string) string {
	for _, model := range account.Models {
		if modelMatchesID(model, modelID) {
			return model.ID
		}
	}
	return modelID
}

func modelAccessState(account *Account, modelID string) ModelAccessState {
	return account.runtime.ModelAccess[canonicalAccountModelID(account, modelID)].State
}

func modelCatalogEntryChanged(current []Model, next []Model, modelID string) bool {
	currentModel, currentFound := findCatalogModel(current, modelID)
	nextModel, nextFound := findCatalogModel(next, modelID)
	return currentFound != nextFound || !reflect.DeepEqual(currentModel, nextModel)
}

func modelAccessCatalogModelID(current []Model, next []Model, modelAccessScope string) string {
	if _, found := findCatalogModel(current, modelAccessScope); found {
		return modelAccessScope
	}
	if _, found := findCatalogModel(next, modelAccessScope); found {
		return modelAccessScope
	}
	separator := strings.LastIndex(modelAccessScope, ":")
	if separator < 0 || separator+1 >= len(modelAccessScope) {
		return modelAccessScope
	}
	modelID := modelAccessScope[separator+1:]
	if _, found := findCatalogModel(current, modelID); found {
		return modelID
	}
	if _, found := findCatalogModel(next, modelID); found {
		return modelID
	}
	return modelAccessScope
}

func catalogEntriesChanged(current []Model, next []Model) bool {
	for _, model := range current {
		if modelCatalogEntryChanged(current, next, model.ID) {
			return true
		}
	}
	for _, model := range next {
		if modelCatalogEntryChanged(current, next, model.ID) {
			return true
		}
	}
	return false
}

func findCatalogModel(models []Model, modelID string) (Model, bool) {
	for _, model := range models {
		if modelMatchesID(model, modelID) {
			return model, true
		}
	}
	return Model{}, false
}

func benefitTierPriority(tier BenefitTier) int {
	switch tier {
	case BenefitTierFree:
		return 0
	case BenefitTierPlus:
		return 1
	case BenefitTierPro:
		return 2
	case BenefitTierUltra:
		return 3
	default:
		return 4
	}
}

func cloneCooldowns(cooldowns map[string]CooldownState) map[string]CooldownState {
	if len(cooldowns) == 0 {
		return nil
	}
	result := make(map[string]CooldownState, len(cooldowns))
	for key, cooldown := range cooldowns {
		result[key] = cooldown
	}
	return result
}

func fileExists(filePath string) bool {
	info, err := os.Stat(filePath)
	return err == nil && !info.IsDir()
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}
