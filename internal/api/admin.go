package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// AdminService 定义管理端需要的权威状态能力
type AdminService interface {
	Status(context.Context) (AdminStatus, error)
	Models(context.Context) ([]aistudio.Model, error)
	Accounts(context.Context) ([]AdminAccount, error)
	CreateAccount(context.Context, AccountCreateInput) (AdminAccount, error)
	ChromeImportProfiles(context.Context) ([]ChromeImportProfile, error)
	ImportChromeAccounts(context.Context, ChromeImportInput) ([]AdminAccount, error)
	UpdateAccount(context.Context, string, AccountInput) (AdminAccount, error)
	DeleteAccount(context.Context, string) error
	LoginAccount(context.Context, string) (AdminAccount, error)
	VerifyAccount(context.Context, string) (AdminAccount, error)
	StartService(context.Context) (AdminStatus, error)
	StopService(context.Context) (AdminStatus, error)
	ClearLogs(context.Context) error
	RuntimeConfig(context.Context) (RuntimeConfig, error)
	UpdateRuntimeConfig(context.Context, RuntimeConfig) (RuntimeConfig, error)
	Cooldowns(context.Context) ([]AdminCooldown, error)
	PermissionDenied(context.Context) ([]AdminPermissionDenied, error)
	ClearPermissionDenied(context.Context, PermissionDeniedClearInput) (int, error)
	Requests(context.Context) ([]AdminRequest, error)
	CancelRequest(context.Context, string) error
	Events(context.Context) (<-chan AdminEvent, error)
	RecordAccessStart(AccessLog)
	RecordAccessLog(AccessLog)
}

// AdminStatus 表示管理端运行状态
type AdminStatus struct {
	State          string             `json:"state"`
	Running        bool               `json:"running"`
	Ready          bool               `json:"ready"`
	Version        string             `json:"version"`
	ActiveRequests int                `json:"active_requests"`
	Accounts       AdminAccountCounts `json:"accounts"`
	Workers        AdminWorkerCounts  `json:"workers"`
	// UltraAccounts 为 Ultra 号池成员（权益为 Ultra 的账户，ULTRA_INCLUDE_PRO=true 时也含权益为 Pro 的账户）的状态计数，
	// Accounts 包含全部账户
	UltraAccounts AdminAccountCounts `json:"ultra_accounts"`
	// UltraWorkers 为 Ultra 分区的 Worker 计数；Workers 的计数与上限只含普通分区（WarmIDs、StartingIDs 包含两个分区）
	UltraWorkers AdminWorkerPartition `json:"ultra_workers"`
	// PrimarySupply 为每个主用模型的可用供给：普通主用模型（PRIMARY_MODELS）统计普通分区，Ultra 主用模型（ULTRA_PRIMARY_MODELS）
	// 统计 Ultra 分区（Pool 为 ultra），keepWarm 约每 30 秒统计一次并缓存；没有主用模型或服务还没统计过时为空
	PrimarySupply []AdminPrimarySupply `json:"primary_supply,omitempty"`
	// Egress 为出口（账号代理、IP 池端口或会话规范化后的值，空为直连）的限流判定与限速：出口不超过 16 个时全部列出（含正常的，
	// 诊断要看门槛附近的计数），否则只列出判定成立（limited）或已撤销（rejected）的，正常的只计入 EgressSummary
	// （IP 池开启后出口多达几百个：静态每个端口、动态每个会话一个）。服务未运行、没有出口或出口多且全部正常时为空
	Egress []AdminEgress `json:"egress,omitempty"`
	// EgressSummary 为全部出口的汇总计数；服务运行时总有（没有出口时各项为 0），服务未运行时为空
	EgressSummary *AdminEgressSummary `json:"egress_summary,omitempty"`
	// CatalogSync 为模型目录缓存与后台同步的状态；服务未运行时为空
	CatalogSync *AdminCatalogSync `json:"catalog_sync,omitempty"`
	// AuthRecheck 为登录复核（疑似登录失效的复核与需要登录账户的定期复核）的状态；服务未运行时为空
	AuthRecheck *AdminAuthRecheck `json:"auth_recheck,omitempty"`
	// BadAccounts 为现在标为坏号（连续裸限流、期间没有成功）、移出预热、扩容与替补候选的账号数
	BadAccounts int `json:"bad_accounts,omitempty"`
	// ProxyPool 为 IP 池的实时状态（见 AdminProxyPool）：关闭且没有浏览器在用旧出口时省略
	ProxyPool *AdminProxyPool `json:"proxy_pool,omitempty"`
	// BuildChannel 为 Build 通道生成的暂停状态（官网停用经 Build 代理的生成时，见 AdminBuildChannel）：服务运行且启用了 Build 通道时才有
	BuildChannel *AdminBuildChannel `json:"build_channel,omitempty"`
	// Drive 为启用账户的 Google Drive 授权状态计数（见 AdminDriveCounts）：生成服务不报告时省略
	Drive *AdminDriveCounts `json:"drive,omitempty"`
}

// AdminDriveCounts 表示启用账户的 Google Drive 授权状态：Available 为换到过 Drive token、或经本服务登录时完成了 Drive 授权的账户，
// Unavailable 为 GenerateAccessToken 返回 401 / Code 16（没有 Drive 授权，多半是只导入 Cookie 的账户）的账户，Unknown 为还没试过、
// 认证代际变了或记录过期的账户。gemini-omni 的视频输入（及无法转为 PNG 的图片）只交给可用或未知的账户。可用、不可用各记 24 小时，
// 写在 runtime/model-catalogs/drive-accounts.json，服务重启后沿用（认证文件换成另一个登录时作废）
type AdminDriveCounts struct {
	Available   int `json:"available"`
	Unavailable int `json:"unavailable"`
	Unknown     int `json:"unknown"`
}

// AdminBuildChannel 表示 Build 通道生成的暂停状态：Build 代理调用返回 PROXY_GENERATION_DISABLED（官网停用了经 Build 代理的生成）时
// 先按账号暂停，短时间内 3 个不同账号都遇到即全局暂停（所有账号都不再走 Build，生成改走 Playground），之后定期放行一个真实请求试探，
// 试探成功即解除。Paused 为全局暂停，Since 为开始时间，NextProbe 为下次试探时间（两者只在全局暂停时输出），Probing 为试探进行中，
// PausedAccounts 为按账号暂停中的账号数（到期等待试探的不算），AwaitingProbe 为按账号暂停已到期、等它在 Build 上的下一次结果
// （试探）的账号数（没有时省略）；全局暂停时所有账号都不走 Build、各账号也不单独试探，PausedAccounts 为遇到过的全部账号数
// （含各自暂停已到期的），AwaitingProbe 为 0。进程内状态，服务重启后从头判定
type AdminBuildChannel struct {
	Paused         bool      `json:"paused"`
	Since          time.Time `json:"since,omitzero"`
	NextProbe      time.Time `json:"next_probe,omitzero"`
	Probing        bool      `json:"probing,omitempty"`
	PausedAccounts int       `json:"paused_accounts"`
	AwaitingProbe  int       `json:"awaiting_probe,omitempty"`
}

// AdminAuthRecheck 表示登录复核：上游 401 先记疑似登录失效、写短冷却并用磁盘 Cookie 复核，仍是 401 才标需要登录；
// 需要登录的账户每 15 分钟一轮低速复核，复核通过即恢复
type AdminAuthRecheck struct {
	Suspects int `json:"suspects"` // 疑似登录失效、等待复核的账户
	Queued   int `json:"queued"`   // 本轮还没复核的需要登录账户
	Running  int `json:"running"`
	// 以下为最近一小时的计数：Cleared 为疑似失效复核通过（排除），Confirmed 为疑似失效复核仍是 401（标需要登录），
	// Recovered 为需要登录账户复核通过（恢复就绪），StillInvalid 为仍是 401，Skipped 为账户忙、出口限流或复核出错跳过
	Cleared1h      int       `json:"cleared_1h"`
	Confirmed1h    int       `json:"confirmed_1h"`
	Recovered1h    int       `json:"recovered_1h"`
	StillInvalid1h int       `json:"still_invalid_1h"`
	Skipped1h      int       `json:"skipped_1h"`
	NextRound      time.Time `json:"next_round,omitempty"`
	PauseReason    string    `json:"pause_reason,omitempty"` // 出口限流
}

// AdminPrimarySupply 表示一个主用模型在它所在分区的可用供给：Pool 为 ultra 时是 Ultra 主用模型、统计 Ultra 分区，为空时是普通主用模型、
// 统计普通分区。Warm 为能服务它的驻留 Worker（对它没有冷却、不在排空中），Standby 为能服务它的空闲备用账号（没有驻留 Worker、
// 不在启动中、对它没有冷却），最多统计 1000 个，达到上限时 StandbyCapped 为真。
// Standby 为 0 表示没有还能服务该模型的空闲账号（多半是当天额度已用完，太平洋时间零点重置）
type AdminPrimarySupply struct {
	Model   string `json:"model"`
	Pool    string `json:"pool,omitempty"`
	Warm    int    `json:"warm"`
	Standby int    `json:"standby"`
	// Cooling 为对它冷却中的驻留 Worker：不在排空中、是该模型的候选、但所有支持的启用通道都在冷却（含剩余不到 2 分钟的短冷却，
	// 与调度分类口径一致），既不计入 Warm 也不计入 Standby；驻留 Worker 大多在这里时，主用模型的请求只能等冷却到期或换上新号
	Cooling       int  `json:"cooling"`
	StandbyCapped bool `json:"standby_capped,omitempty"`
}

// AdminEgressSummary 为出口汇总：Total 为有账户、有活动或有判定记录的出口数，其余为按判定状态的分项（出口多于 16 个时 Normal 不在 Egress 里列出）
type AdminEgressSummary struct {
	Total    int `json:"total"`
	Normal   int `json:"normal"`
	Limited  int `json:"limited"`
	Rejected int `json:"rejected"`
}

// AdminEgress 表示一个出口的限流判定与限速
type AdminEgress struct {
	ID             string            `json:"id"` // 出口编号（管理动作用），不含代理原文
	Label          string            `json:"label"`
	Mode           string            `json:"mode"`    // normal | limited | rejected
	Limiter        string            `json:"limiter"` // off | active | suspended
	Since          time.Time         `json:"since,omitempty"`
	Accounts       int               `json:"accounts"`
	Lanes          []AdminEgressLane `json:"lanes"`
	Success1m      int               `json:"success_1m"`
	Bare1m         int               `json:"bare_429_1m"`
	Other1m        int               `json:"other_1m"`
	Ratio2m        float64           `json:"ratio_2m"`              // 裸 429 占 2 分钟内尝试结果的比例（0–1，不含已降级账号）
	RatioEntry     float64           `json:"ratio_entry,omitempty"` // 判定成立时的比例
	FreshRejects2m int               `json:"fresh_rejects_2m"`
	Queued         int               `json:"queued"`              // 正在按“出口限速”排队的请求
	LocalRejects1m int               `json:"local_429_1m"`        // 本地返回的 429：出口限速排队超限与出口限流换号上限
	Retried        int               `json:"retried,omitempty"`   // 本次判定期间裸 429 后再被尝试的账号数
	Recovered      int               `json:"recovered,omitempty"` // 其中后来成功的
	Demoted        int               `json:"demoted,omitempty"`   // 按账号证据降级的账号数
	Relaxed        int               `json:"relaxed,omitempty"`   // 判定成立时撤回的逐级冷却账号数
	RetryAt        time.Time         `json:"retry_at,omitempty"`
	RejectedUntil  time.Time         `json:"rejected_until,omitempty"`
	RejectedBy     string            `json:"rejected_by,omitempty"` // 安全阀 | 管理操作
}

// AdminEgressLane 表示出口内一个分区通道的令牌桶与冷启动配额
type AdminEgressLane struct {
	Lane             string `json:"lane"` // normal | ultra
	Accounts         int    `json:"accounts"`
	LimitRPM         int    `json:"limit_rpm"` // 0 为不限
	Burst            int    `json:"burst,omitempty"`
	Sent1m           int    `json:"sent_1m"`
	PrimarySent1m    int    `json:"primary_sent_1m"`
	OtherSent1m      int    `json:"other_sent_1m"`
	Pending          int    `json:"pending"`
	ColdStarting     int    `json:"cold_starting"`      // 闸门放行、正在启动的冷启动
	ColdStarted1m    int    `json:"cold_started_1m"`    // 最近一分钟闸门放行的冷启动
	RefillDeferred1m int    `json:"refill_deferred_1m"` // 最近一分钟因补位并发已满被推迟的补位
}

// AdminCatalogSync 表示模型目录缓存与后台同步
type AdminCatalogSync struct {
	Cached        int       `json:"cached"`        // 有目录的账户（含来自磁盘的）
	FromDisk      int       `json:"from_disk"`     // 本代运行时启动时从磁盘恢复的账户
	Missing       int       `json:"missing"`       // 启用、就绪却没有目录的账户
	Expired       int       `json:"expired"`       // 目录超过有效期、等待刷新的账户
	AuthSuspects  int       `json:"auth_suspects"` // 疑似登录失效、等待探测的账户
	Queued        int       `json:"queued"`
	Running       int       `json:"running"`
	PausedUntil   time.Time `json:"paused_until,omitempty"`
	PauseReason   string    `json:"pause_reason,omitempty"` // 上游 429 | 出口限流
	Synced1h      int       `json:"synced_1h"`
	Failed1h      int       `json:"failed_1h"`
	RateLimited1h int       `json:"rate_limited_1h"`
	SkippedBusy1h int       `json:"skipped_busy_1h"` // 账户有进行中的请求、推后刷新的次数
}

// AdminWorkerPartition 表示一个 Worker 分区（Ultra 分区）的实时数量与上限
type AdminWorkerPartition struct {
	// Warm 为已就绪的 Worker，Starting 为正在启动的，Occupied 为占用的槽位（含正在关闭的，排空中的按已让出计），
	// Dead 为 Warm 中账号长时间冷却、等待淘汰的死槽位；Draining 为其中排空中的 Worker（不再接不固定账户的新请求，
	// 进行中的请求结束后关闭，替补已先启动），Retiring 为待淘汰登记数
	Warm     int `json:"warm"`
	Starting int `json:"starting"`
	Occupied int `json:"occupied"`
	Dead     int `json:"dead"`
	Draining int `json:"draining"`
	Retiring int `json:"retiring"`
	// Unschedulable 为驻留 Worker 中账号已不能服务任何模型的（需要登录、已停用、不可用、没有可初始化模型），等待释放
	Unschedulable int `json:"unschedulable"`
	// Target 为预热目标（常驻数与分区内可预热账户数取小），WarmLimit 与 Max 为分区的常驻数与峰值数设置
	Target    int `json:"target"`
	WarmLimit int `json:"warm_limit"`
	Max       int `json:"max"`
}

// AdminPrewarmState 表示预热循环的实时状态
type AdminPrewarmState struct {
	Active         bool   `json:"active"`
	Inflight       int    `json:"inflight"`
	Launched       int    `json:"launched"`
	RoundSeconds   int64  `json:"round_seconds"`
	LoopAgeSeconds int64  `json:"loop_age_seconds"`
	Reason         string `json:"reason,omitempty"`
}

// AdminWorkerCounts 表示 WAA Worker 的实时数量与对应账户：数量与上限只含普通分区（Ultra 分区见 AdminWorkerPartition），
// WarmIDs、StartingIDs 包含两个分区；预热循环两个分区共用
type AdminWorkerCounts struct {
	Warm     int `json:"warm"`
	Starting int `json:"starting"`
	Target   int `json:"target"`
	Max      int `json:"max"`
	Occupied int `json:"occupied"`
	// Dead 为 Warm 中账号长时间冷却（如每日限额）、占着名额却服务不了请求、等待淘汰的死槽位；
	// Draining 为其中排空中的 Worker（不再接不固定账户的新请求，进行中的请求结束后关闭，替补已先启动），Retiring 为待淘汰登记数
	Dead        int               `json:"dead"`
	Draining    int               `json:"draining"`
	Retiring    int               `json:"retiring"`
	Prewarm     AdminPrewarmState `json:"prewarm"`
	WarmIDs     []string          `json:"warm_ids,omitempty"`
	StartingIDs []string          `json:"starting_ids,omitempty"`
	// Unschedulable 为驻留 Worker 中账号已不能服务任何模型的（需要登录、已停用、不可用、没有可初始化模型），等待释放
	Unschedulable int `json:"unschedulable"`
}

// AdminLog 表示管理页面展示的一条运行日志
type AdminLog struct {
	Time    time.Time   `json:"time"`
	Level   string      `json:"level"`
	Source  string      `json:"source"`
	Message string      `json:"message"`
	Event   string      `json:"event"`
	Request *RequestLog `json:"request,omitempty"`
}

// RequestLog 保存可关联的请求状态、用量与诊断字段
type RequestLog struct {
	ID           string           `json:"id"`
	State        string           `json:"state"`
	Model        string           `json:"model,omitempty"`
	Method       string           `json:"method,omitempty"`
	Path         string           `json:"path,omitempty"`
	Pool         string           `json:"pool,omitempty"`
	Status       int              `json:"status,omitempty"`
	DurationMS   float64          `json:"duration_ms,omitempty"`
	Usage        *RequestLogUsage `json:"usage,omitempty"`
	ToolCalls    int              `json:"tool_calls,omitempty"`
	FinishReason string           `json:"finish_reason,omitempty"`
	ReplyHash    string           `json:"reply_hash,omitempty"`
	// ServedModel 为上游标明的实际服务模型，只在与请求的模型系列不同时出现
	ServedModel string `json:"served_model,omitempty"`
	// Downgrade 为降级判定的依据（只有被拦截的模型才有）
	Downgrade       *aistudio.DowngradeDecision `json:"downgrade,omitempty"`
	Error           string                      `json:"error,omitempty"`
	InputMessages   int                         `json:"input_messages,omitempty"`
	InputTextChars  int                         `json:"input_text_chars,omitempty"`
	InputMedia      int                         `json:"input_media,omitempty"`
	InputMediaBytes int64                       `json:"input_media_bytes,omitempty"`
	InputFiles      int                         `json:"input_files,omitempty"`
	Parameters      map[string]string           `json:"parameters,omitempty"`
	FirstEventMS    float64                     `json:"first_event_ms,omitempty"`
	// QueueMS 为进入服务到取得最终执行账户的时间，ProofMS 为等待并生成 WAA proof 的累计时间
	QueueMS       float64 `json:"queue_ms,omitempty"`
	ProofMS       float64 `json:"proof_ms,omitempty"`
	UpstreamBytes int64   `json:"upstream_bytes,omitempty"`
	Channel       string  `json:"channel,omitempty"`
}

// RequestLogUsage 区分输入、思考、回复与端到端输出速率
type RequestLogUsage struct {
	InputTokens            int64   `json:"input_tokens"`
	ReasoningTokens        int64   `json:"reasoning_tokens"`
	ReplyTokens            int64   `json:"reply_tokens"`
	OutputTokens           int64   `json:"output_tokens"`
	TotalTokens            int64   `json:"total_tokens"`
	AverageTokensPerSecond float64 `json:"average_tokens_per_second"`
}

// AccessLog 表示一次公开 API 请求的最终访问记录
type AccessLog struct {
	Status          int
	Latency         time.Duration
	FirstEvent      time.Duration
	QueueWait       time.Duration
	Proof           time.Duration
	UpstreamBytes   int64
	Usage           *aistudio.Usage
	ToolCalls       int
	InputMessages   int
	InputTextChars  int
	InputMedia      int
	InputMediaBytes int64
	InputFiles      int
	Temperature     string
	TopP            string
	TopK            string
	Thinking        string
	MaxOutputTokens string
	Seed            string
	RequestID       string
	Method          string
	Path            string
	Model           string
	Account         string
	Channel         string
	FinishReason    string
	ReplyHash       string
	ServedModel     string
	Downgrade       *aistudio.DowngradeDecision
	Error           string
	Canceled        bool
	Generation      bool
	// Attempts 为最终结果之前未成功的上游尝试
	Attempts []RequestAttempt
	// Authorized 表示请求通过了公开 API key 校验
	Authorized bool
	// Pool 为请求的号池：经 /ultra 进入的请求为 ultra，其余为空
	Pool string
}

// AdminAccountCounts 表示账户状态计数
type AdminAccountCounts struct {
	Total        int `json:"total"`
	Ready        int `json:"ready"`
	Busy         int `json:"busy"`
	Cooldown     int `json:"cooldown"`
	AuthRequired int `json:"auth_required"`
	// AuthRequired10m 为最近 10 分钟新被标为需要登录的账户数（“账号需要登录”日志的条数，同账号 1 分钟最多一条）；
	// 只在全部账户的计数里填写，持续增长说明运行期 401 在累积
	AuthRequired10m int `json:"auth_required_10m,omitempty"`
	// OrgDisabled 为所属组织停用了 AI Studio 的启用账户数（状态 org_disabled，不参与任何调度，见 AdminOrgDisabled）
	OrgDisabled int `json:"org_disabled"`
}

// AdminAccount 表示管理端账户摘要
type AdminAccount struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Enabled     bool     `json:"enabled"`
	State       string   `json:"state"`
	Proxy       string   `json:"proxy"`
	Locale      string   `json:"locale"`
	Timezone    string   `json:"timezone"`
	Models      []string `json:"models"`
	BenefitTier string   `json:"benefit_tier"`
	Message     string   `json:"message"`
	// BenefitTierKnown 表示权益已经从官网读取过；为 false 时 benefit_tier 按 Free 显示，账户属于普通号池
	BenefitTierKnown bool `json:"benefit_tier_known"`
	// Pool 为账户当前所属的号池（成员关系）：ultra（权益为 Ultra，ULTRA_INCLUDE_PRO=true 时也含 Pro）或 normal；
	// benefit_tier 始终是账户的真实权益，并入 Ultra 号池的 Pro 账户仍显示 Pro
	Pool string `json:"pool"`
	// Authenticated 与 VerifyReason 只在验证接口的返回里填写：验证结论（浏览器已登录且接口复核通过才为 true）与它的原因。
	// 停用账户的状态始终是 disabled，前端按 Authenticated 计“有效 / 登录失效”，不能看状态或 HTTP 200
	Authenticated *bool  `json:"authenticated,omitempty"`
	VerifyReason  string `json:"verify_reason,omitempty"`
	// ProxyPoolSlot 为账户没有自己的代理时 IP 池给它的端口号或会话号，ProxyPoolActive 表示有浏览器正在用它；
	// 账户列表不随 Worker 启停推送，只作兜底，实时端口看状态接口的 proxy_pool.assignments
	ProxyPoolSlot   string `json:"proxy_pool_slot,omitempty"`
	ProxyPoolActive bool   `json:"proxy_pool_active,omitempty"`
	// ProxyPoolPinned 表示 IP 池开启、账户代理与当前生效的默认代理（PROXY）规范值相同，因此不使用 IP 池；与状态接口的
	// proxy_pool.pinned_default、“改为跟随默认代理”同一份判断
	ProxyPoolPinned bool `json:"proxy_pool_pinned,omitempty"`
	// OrgDisabled 表示账户所属组织停用了 AI Studio（不区分是否启用）：启用账户的 state 为 org_disabled，停用账户的 state 仍为 disabled、
	// message 接上这个原因；验证接口的返回在验证遇到组织停用时同样为 true，前端据此提示组织停用而不是登录失效
	OrgDisabled bool `json:"org_disabled,omitempty"`
}

// AccountInput 表示已有账户配置
type AccountInput struct {
	Label    string `json:"label"`
	Enabled  bool   `json:"enabled"`
	Proxy    string `json:"proxy"`
	Locale   string `json:"locale"`
	Timezone string `json:"timezone"`
}

// AccountCreateInput 表示浏览器登录的账户环境
type AccountCreateInput struct {
	Proxy    string `json:"proxy"`
	Locale   string `json:"locale"`
	Timezone string `json:"timezone"`
}

// ChromeImportProfile 表示可从本机 Chrome 导入的账号
type ChromeImportProfile struct {
	ID          string `json:"id"`
	Profile     string `json:"profile"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Locale      string `json:"locale"`
}

// ChromeImportInput 表示批量导入的 Chrome 账号与账户环境
type ChromeImportInput struct {
	AccountIDs []string `json:"account_ids"`
	Proxy      string   `json:"proxy"`
	Locale     string   `json:"locale"`
	Timezone   string   `json:"timezone"`
}

// RuntimeConfig 表示全局运行配置
type RuntimeConfig struct {
	AuthStates                string   `json:"auth_states"`
	ListenAddr                string   `json:"listen_addr"`
	APIKey                    string   `json:"proxy_api_key"`
	ActiveListenAddr          string   `json:"active_listen_addr"`
	ActiveAPIKey              string   `json:"active_proxy_api_key"`
	ManagementRestartRequired bool     `json:"management_restart_required"`
	ServiceRestartRequired    bool     `json:"service_restart_required"`
	Proxy                     string   `json:"proxy"`
	InitTimeout               string   `json:"init_timeout"`
	RequestTimeout            string   `json:"request_timeout"`
	FirstEventTimeout         string   `json:"first_event_timeout"`
	WarmWorkerLimit           int      `json:"warm_worker_limit"`
	MaxActiveWorkers          int      `json:"max_active_workers"`
	WarmStartupConcurrency    int      `json:"warm_startup_concurrency"`
	PerAccountConcurrency     int      `json:"per_account_concurrency"`
	RoutingStrategy           string   `json:"routing_strategy"`
	UpstreamChannels          []string `json:"upstream_channels"`
	TemporaryChat             bool     `json:"temporary_chat"`
	IgnoreClientSeed          bool     `json:"ignore_client_seed"`
	RepeatPromptNonce         bool     `json:"repeat_prompt_nonce"`
	MinOutputTokens           int      `json:"min_output_tokens"`
	WAABackend                string   `json:"waa_backend"`
	// AccountRPMLimit 为单账户每分钟生成请求上限（ACCOUNT_RPM_LIMIT，0 表示关闭），读取时总是返回；保存时没有该字段（旧版页面）
	// 则沿用现值。用指针是因为 0 表示关闭，不能用零值判断是否缺省
	AccountRPMLimit *int `json:"account_rpm_limit,omitempty"`
	// DowngradeGuard 为降级判定设置；保存时没有该字段（旧版页面）则沿用现值
	DowngradeGuard *DowngradeGuardConfig `json:"downgrade_guard,omitempty"`
	// UltraExclusive、UltraIncludePro、UltraWarmWorkerLimit、UltraMaxActiveWorkers 为 Ultra 号池设置（ULTRA_EXCLUSIVE、
	// ULTRA_INCLUDE_PRO、ULTRA_WARM_WORKER_LIMIT、ULTRA_MAX_ACTIVE_WORKERS），读取时总是返回；保存时没有这些字段（旧版页面）则沿用现值
	UltraExclusive        *bool `json:"ultra_exclusive,omitempty"`
	UltraIncludePro       *bool `json:"ultra_include_pro,omitempty"`
	UltraWarmWorkerLimit  *int  `json:"ultra_warm_worker_limit,omitempty"`
	UltraMaxActiveWorkers *int  `json:"ultra_max_active_workers,omitempty"`
	// StreamPlaygroundModels 为流式请求优先走 Playground 的模型（STREAM_PLAYGROUND_MODELS），读取时总是返回（可以为空数组）；
	// 保存时没有该字段（旧版页面）则沿用现值
	StreamPlaygroundModels *[]string `json:"stream_playground_models,omitempty"`
	// PrimaryModels 为主用模型（PRIMARY_MODELS），读取时总是返回（可以为空数组）；PrimaryQueueWait 为主用模型的排队上限
	// （PRIMARY_QUEUE_WAIT，时长文本，0s 表示不排队）；保存时没有这些字段（旧版页面）则沿用现值
	PrimaryModels    *[]string `json:"primary_models,omitempty"`
	PrimaryQueueWait string    `json:"primary_queue_wait,omitempty"`
	// UltraPrimaryModels 为 Ultra 主用模型（ULTRA_PRIMARY_MODELS，只作用于 Ultra 号池，没有额度时立即返回 429），读取时总是返回
	// （可以为空数组）；UltraPrimaryQueueWait 为它的排队上限（ULTRA_PRIMARY_QUEUE_WAIT，时长文本）；保存时没有这些字段（旧版页面）则沿用现值
	UltraPrimaryModels    *[]string `json:"ultra_primary_models,omitempty"`
	UltraPrimaryQueueWait string    `json:"ultra_primary_queue_wait,omitempty"`
	// ProxyPool 为 IP 池设置（PROXY_POOL_*，保存后立即生效），读取时总是返回；保存时没有该字段（旧版页面）则沿用现值
	ProxyPool *ProxyPoolConfig `json:"proxy_pool,omitempty"`
	// MediaOutput、MediaBaseURL、MediaRetention、MediaMaxSize 为生成媒体的输出与保存设置（MEDIA_OUTPUT、MEDIA_BASE_URL、
	// MEDIA_RETENTION、MEDIA_MAX_SIZE，保存后立即生效），读取时总是返回；保存时没有这些字段（旧版页面）则沿用现值。
	// 期限与大小为文本（7d、72h、20GB，0 表示不清理或不限）；服务地址可以清空，所以用指针区分缺省
	MediaOutput    string  `json:"media_output,omitempty"`
	MediaBaseURL   *string `json:"media_base_url,omitempty"`
	MediaRetention string  `json:"media_retention,omitempty"`
	MediaMaxSize   string  `json:"media_max_size,omitempty"`
}

// DowngradeGuardConfig 为"拒绝被上游降级的回复"的设置（管理页 → 服务配置，修改后立即生效）
type DowngradeGuardConfig struct {
	Enabled        bool     `json:"enabled"`
	Models         []string `json:"models"`
	SpeedThreshold float64  `json:"speed_threshold"`
	MinTokens      int      `json:"min_tokens"`
	MinWindowMS    int      `json:"min_window_ms"`
	FuzzyLow       float64  `json:"fuzzy_low"`
	FuzzyHigh      float64  `json:"fuzzy_high"`
	CountTimeoutMS int      `json:"count_timeout_ms"`
	FastMode       bool     `json:"fast_mode"`
	MemoryMinutes  int      `json:"memory_minutes"`
	MaxHoldMS      int      `json:"max_hold_ms"`
	RejectStatus   int      `json:"reject_status"`
}

// AdminCooldown 表示账户模型冷却
type AdminCooldown struct {
	AccountID    string    `json:"account_id"`
	AccountLabel string    `json:"account_label"`
	Channel      string    `json:"channel"`
	ModelID      string    `json:"model_id"`
	Until        time.Time `json:"until"`
	Reason       string    `json:"reason,omitempty"`
}

// AdminPermissionDenied 表示一个返回过 HTTP 403 无权限（PERMISSION_DENIED）的账户：多半没有同意 Google 的政策，
// 重新获取认证文件上传后清除。Models 按最近一次 403 从新到旧排列，Count 为各模型次数之和
type AdminPermissionDenied struct {
	ID      string                       `json:"id"`
	Label   string                       `json:"label"`
	Models  []AdminPermissionDeniedModel `json:"models"`
	Count   int                          `json:"count"`
	FirstAt time.Time                    `json:"first_at"`
	LastAt  time.Time                    `json:"last_at"`
}

// AdminPermissionDeniedModel 表示账户在一个模型上的 403 记录；Channels 为返回过 403、之后还没有成功过的通道
type AdminPermissionDeniedModel struct {
	Model    string    `json:"model"`
	Channels []string  `json:"channels"`
	Count    int       `json:"count"`
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
	Message  string    `json:"message,omitempty"`
}

// PermissionDeniedClearInput 表示要清除记录的账户；All 为真时清除全部（组织停用账户的清除接口共用）
type PermissionDeniedClearInput struct {
	AccountIDs []string `json:"account_ids"`
	All        bool     `json:"all"`
}

// AdminOrgDisabled 表示一个所属组织停用了 AI Studio 的账户（Google Workspace 账号的组织或 Google 关闭了这项服务）：载入 AI Studio
// 被跳到 access.workspace.google.com 的 ServiceNotAllowed 页，或 RPC 返回 HTTP 403 “AI Studio app is disabled”。重新登录也没用，
// 账户不参与任何调度，认证文件更新、管理员清除或长间隔复查通过后恢复。Source 为第一次进入的来源（启动、模型目录、验证、登录、请求、
// 登录复核：疑似登录失效或需要登录账户的复核请求返回 app is disabled），LastSource 为最近一次；CheckAt 为下一次长间隔复查的时间；
// Message 为最近一次的原文（已打码、截短）
type AdminOrgDisabled struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`
	Source     string    `json:"source"`
	LastSource string    `json:"last_source"`
	Count      int       `json:"count"`
	FirstAt    time.Time `json:"first_at"`
	LastAt     time.Time `json:"last_at"`
	CheckAt    time.Time `json:"check_at"`
	Message    string    `json:"message,omitempty"`
}

// OrgDisabledService 为管理服务可选实现的组织停用账户能力：实现时 /api/accounts/permission-denied 的返回另带 org_disabled 列表，
// /api/accounts/org-disabled/clear 清除状态；没有实现时列表省略、清除接口返回 404（旧的管理服务与测试替身不受影响）
type OrgDisabledService interface {
	OrgDisabled(context.Context) ([]AdminOrgDisabled, error)
	ClearOrgDisabled(context.Context, PermissionDeniedClearInput) (int, error)
}

// AdminRequest 表示活动请求摘要
type AdminRequest struct {
	ID           string    `json:"id"`
	Model        string    `json:"model"`
	AccountID    string    `json:"account_id"`
	AccountLabel string    `json:"account_label"`
	Channel      string    `json:"channel,omitempty"`
	State        string    `json:"state"`
	StartedAt    time.Time `json:"started_at"`
	// Pool 为请求的号池：经 /ultra 进入的请求为 ultra，其余为空
	Pool string `json:"pool,omitempty"`
}

// AdminEvent 表示管理端增量事件
type AdminEvent struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

func (s *server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/accounts", s.handleAccounts)
	mux.HandleFunc("POST /api/accounts", s.handleCreateAccount)
	mux.HandleFunc("GET /api/accounts/import/chrome", s.handleChromeImportProfiles)
	mux.HandleFunc("POST /api/accounts/import/chrome", s.handleImportChromeAccounts)
	mux.HandleFunc("POST /api/accounts/bulk-enable", s.handleBulkEnableAccounts)
	mux.HandleFunc("PUT /api/accounts/{id}", s.handleUpdateAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	mux.HandleFunc("POST /api/accounts/bulk-delete", s.handleBulkDeleteAccounts)
	mux.HandleFunc("POST /api/accounts/{id}/login", s.handleLoginAccount)
	mux.HandleFunc("POST /api/accounts/{id}/verify", s.handleVerifyAccount)
	mux.HandleFunc("POST /api/control/start", s.handleStartService)
	mux.HandleFunc("POST /api/control/stop", s.handleStopService)
	mux.HandleFunc("POST /api/control/restart", s.handleRestartService)
	mux.HandleFunc("GET /api/debug/goroutines", handleDebugGoroutines)
	mux.HandleFunc("GET /api/onboarding", s.handleGetOnboarding)
	mux.HandleFunc("PUT /api/onboarding", s.handleUpdateOnboarding)
	mux.HandleFunc("POST /api/onboarding/queue-disabled", s.handleQueueDisabledOnboarding)
	mux.HandleFunc("DELETE /api/logs", s.handleClearLogs)
	mux.HandleFunc("GET /api/config", s.handleRuntimeConfig)
	mux.HandleFunc("PUT /api/config", s.handleUpdateRuntimeConfig)
	mux.HandleFunc("POST /api/proxy/test", s.handleTestProxy)
	mux.HandleFunc("POST /api/proxy/pool/check", s.handleCheckProxyPool)
	mux.HandleFunc("POST /api/accounts/proxy/follow-default", s.handleFollowDefaultProxy)
	mux.HandleFunc("GET /api/cooldowns", s.handleCooldowns)
	mux.HandleFunc("GET /api/accounts/permission-denied", s.handlePermissionDenied)
	mux.HandleFunc("POST /api/accounts/permission-denied/clear", s.handleClearPermissionDenied)
	mux.HandleFunc("POST /api/accounts/org-disabled/clear", s.handleClearOrgDisabled)
	mux.HandleFunc("GET /api/requests", s.handleRequests)
	mux.HandleFunc("POST /api/requests/{id}/cancel", s.handleCancelRequest)
	mux.HandleFunc("GET /api/events", s.handleAdminEvents)
	s.registerEgress(mux)
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.config.Admin != nil {
		status, err := s.config.Admin.Status(r.Context())
		if err != nil {
			writeAdminUpstreamError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
		return
	}
	if _, err := s.service.Models(r.Context()); err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ready": true})
}

func (s *server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.config.Admin.Models(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]aistudio.Model{"models": models})
}

func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.config.Admin.Accounts(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]AdminAccount{"accounts": accounts})
}

func (s *server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var input AccountCreateInput
	if err := decodeJSON(r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	account, err := s.config.Admin.CreateAccount(r.Context(), input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]AdminAccount{"account": account})
}

func (s *server) handleChromeImportProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.config.Admin.ChromeImportProfiles(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]ChromeImportProfile{"profiles": profiles})
}

func (s *server) handleImportChromeAccounts(w http.ResponseWriter, r *http.Request) {
	var input ChromeImportInput
	if err := decodeJSON(r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	accounts, err := s.config.Admin.ImportChromeAccounts(r.Context(), input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string][]AdminAccount{"accounts": accounts})
}

func (s *server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	var input AccountInput
	if err := decodeJSON(r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := context.WithValue(r.Context(), adminCallerKey{}, adminCallerText(r))
	account, err := s.config.Admin.UpdateAccount(ctx, r.PathValue("id"), input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]AdminAccount{"account": account})
}

func (s *server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.config.Admin.DeleteAccount(r.Context(), r.PathValue("id")); err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleLoginAccount(w http.ResponseWriter, r *http.Request) {
	account, err := s.config.Admin.LoginAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]AdminAccount{"account": account})
}

func (s *server) handleVerifyAccount(w http.ResponseWriter, r *http.Request) {
	account, err := s.config.Admin.VerifyAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]AdminAccount{"account": account})
}

func (s *server) handleStartService(w http.ResponseWriter, r *http.Request) {
	status, err := s.config.Admin.StartService(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *server) handleStopService(w http.ResponseWriter, r *http.Request) {
	status, err := s.config.Admin.StopService(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *server) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	if err := s.config.Admin.ClearLogs(r.Context()); err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	config, err := s.config.Admin.RuntimeConfig(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (s *server) handleUpdateRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	var config RuntimeConfig
	if err := decodeJSON(r, &config); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	updated, err := s.config.Admin.UpdateRuntimeConfig(r.Context(), config)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *server) handleCooldowns(w http.ResponseWriter, r *http.Request) {
	cooldowns, err := s.config.Admin.Cooldowns(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]AdminCooldown{"cooldowns": cooldowns})
}

// handlePermissionDenied 返回 403 无权限账户列表（accounts，格式不变）；管理服务实现了 OrgDisabledService 时另带
// 所属组织停用了 AI Studio 的账户（org_disabled，没有时为空数组），旧前端只读 accounts，不受影响
func (s *server) handlePermissionDenied(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.config.Admin.PermissionDenied(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	body := map[string]any{"accounts": accounts}
	if orgDisabled, ok := s.config.Admin.(OrgDisabledService); ok {
		entries, err := orgDisabled.OrgDisabled(r.Context())
		if err != nil {
			writeAdminUpstreamError(w, err)
			return
		}
		if entries == nil {
			entries = []AdminOrgDisabled{}
		}
		body["org_disabled"] = entries
	}
	writeJSON(w, http.StatusOK, body)
}

// handleClearOrgDisabled 清除组织停用账户的状态（立即恢复调度），返回清除数；请求体同 403 无权限的清除接口
func (s *server) handleClearOrgDisabled(w http.ResponseWriter, r *http.Request) {
	orgDisabled, ok := s.config.Admin.(OrgDisabledService)
	if !ok {
		writeAdminError(w, http.StatusNotFound, "not_found", "不支持组织停用账户")
		return
	}
	var input PermissionDeniedClearInput
	if err := decodeJSON(r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cleared, err := orgDisabled.ClearOrgDisabled(r.Context(), input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"cleared": cleared})
}

func (s *server) handleClearPermissionDenied(w http.ResponseWriter, r *http.Request) {
	var input PermissionDeniedClearInput
	if err := decodeJSON(r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cleared, err := s.config.Admin.ClearPermissionDenied(r.Context(), input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"cleared": cleared})
}

func (s *server) handleRequests(w http.ResponseWriter, r *http.Request) {
	requests, err := s.config.Admin.Requests(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]AdminRequest{"requests": requests})
}

func (s *server) handleCancelRequest(w http.ResponseWriter, r *http.Request) {
	if err := s.config.Admin.CancelRequest(r.Context(), r.PathValue("id")); err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.config.Admin.Events(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	if err := streamHeaders(w); err != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.config.Stopping:
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := writeSSE(w, "", event); err != nil {
				return
			}
		}
	}
}

func writeAdminUpstreamError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	writeAdminError(w, statusFromError(err), codeFromError(err, "upstream_error"), err.Error())
}

type adminCallerKey struct{}

// AdminCaller 返回管理接口调用方的地址与客户端，用于记录停用原因
func AdminCaller(ctx context.Context) string {
	value, _ := ctx.Value(adminCallerKey{}).(string)
	return value
}

func adminCallerText(r *http.Request) string {
	address := r.RemoteAddr
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
		address = forwarded
	}
	agent := strings.TrimSpace(r.UserAgent())
	if len(agent) > 60 {
		agent = agent[:60]
	}
	if agent == "" {
		return address
	}
	return address + "，" + agent
}
