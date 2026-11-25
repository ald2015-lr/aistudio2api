export type Locale = 'zh-CN' | 'zh-TW' | 'en' | 'ja' | 'ko' | 'fr' | 'de'

export type TabID =
  | 'logs'
  | 'accounts'
  | 'models'
  | 'requests'
  | 'usage'
  | 'settings'
  | 'playground'

// org_disabled 为账号所属组织停用了 AI Studio（不参与任何调度，认证文件更新、管理员清除或长间隔复查通过后恢复）；
// proxy_paused 为 IP 池固定分配下账户的固定端口暂停使用（端口冷却中或被其他进程占用；不参与调度，端口恢复后自动恢复，message 为原因）
export type AccountState =
  | 'ready'
  | 'busy'
  | 'cooldown'
  | 'auth_required'
  | 'unavailable'
  | 'disabled'
  | 'org_disabled'
  | 'proxy_paused'

// AccountRestriction 为 org_disabled 状态的原因种类：org_disabled（组织停用）、service_restricted（Google 限制了该账号使用 AI Studio）、
// age_requirement（年龄或使用资格）、workspace_disabled（所属 Workspace 没有开放 AI Studio）；旧版服务没有这个字段，按组织停用显示
export type AccountRestriction =
  'org_disabled' | 'service_restricted' | 'age_requirement' | 'workspace_disabled'

export interface Account {
  id: string
  label: string
  enabled: boolean
  state: AccountState
  proxy: string
  locale: string
  timezone: string
  models: string[]
  // benefit_tier 始终是账户的真实权益：Pro 账户并入 Ultra 号池后仍为 Pro，号池成员关系看 pool
  benefit_tier: string
  message: string
  // benefit_tier_known 为 false 表示权益尚未从官网读取：benefit_tier 按 Free 显示，账户属于普通号池
  benefit_tier_known: boolean
  // pool 为账户当前所属的号池，表示成员关系而不是权益：权益为 Ultra 的账户属于 ultra；
  // 开启 PRO_POOL（默认）时权益为 Pro 的账户属于 pro，关闭 PRO_POOL、开启 ULTRA_INCLUDE_PRO 时属于 ultra；其余属于 normal
  pool: AccountPool
  // authenticated 与 verify_reason 只在验证接口的返回里有：验证结论（浏览器已登录且接口复核通过才为 true）与原因。
  // 停用账户的状态始终是 disabled，验证结果按 authenticated 判断，不能看状态或 HTTP 200
  authenticated?: boolean
  verify_reason?: string
  // proxy_pool_slot 为 IP 池分给账户的端口号或会话号（账户有自己的代理、IP 池关闭或沿用默认代理时没有），
  // proxy_pool_active 表示有浏览器正在用它。账户列表不随 Worker 启停推送，这两个字段只作兜底，实时端口看 ServiceStatus.proxy_pool.assignments
  proxy_pool_slot?: string
  proxy_pool_active?: boolean
  // proxy_pool_pinned 表示 IP 池开启、账户代理与当前生效的默认代理相同，因此不使用 IP 池（服务端判断，与 pinned_default 一致）
  proxy_pool_pinned?: boolean
  // proxy_pool_fixed_port 为 IP 池固定分配（静态 IP、PROXY_POOL_FIXED=true）下账户的固定端口（只有端口号）；没有自己代理、分到端口的
  // 账户才有。端口暂停使用时 state 为 proxy_paused、message 为原因
  proxy_pool_fixed_port?: number
  // org_disabled 表示账户所属组织停用了 AI Studio（不区分是否启用）：启用账户的 state 为 org_disabled，停用账户仍为 disabled、
  // message 接上原因；验证接口的返回在验证遇到组织停用时同样为 true
  org_disabled?: boolean
  // restriction 为 org_disabled 时的原因种类（见 AccountRestriction），没有标记时没有这个字段
  restriction?: AccountRestriction
  // transition 为管理页启停之后的后台收尾阶段：draining（已停用，等进行中的请求与账户上的其他操作结束）、closing（已停用，
  // 正在关闭 Worker）或 syncing（已启用，合并模型目录、没有目录时同步一次），没有收尾时没有这个字段；
  // active_requests 为收尾期间进行中的请求数（登录、验证等独占操作不计入，draining 而它为 0 时在等这些操作结束）
  transition?: AccountTransition
  active_requests?: number
  // device_bound_session 表示认证文件是设备绑定会话（带 __Secure-1PSIDRTS），复制出来约 2 小时后失效：服务端所在平台不能用 Chrome
  // 续签材料原地续签（与启动、导入时“设备绑定会话”那行 WARN 同一口径）；不是时没有这个字段
  device_bound_session?: boolean
  // extra 为账户与补充池（EXTRA_POOL）的关系：worker 为 Worker 计入补充池（含启动中），member 为补充池成员（有模型冷却、其他模型可用；
  // Worker 驻留在主分区或没有 Worker）。按最近一次对账的成员缓存，账户列表不随成员变化推送；补充池关闭且没有补充池 Worker 时没有
  extra?: AccountExtra
}

// AccountExtra 为账户与补充池的关系
export type AccountExtra = 'worker' | 'member'

// AccountTransition 为账户启停之后的后台收尾阶段
export type AccountTransition = 'draining' | 'closing' | 'syncing'

// AccountPool 为账户所属的号池；经 /ultra 前缀的请求只用 ultra 号池的账户，经 /pro 前缀的请求只用 pro 号池的账户
export type AccountPool = 'ultra' | 'pro' | 'normal'

// RequestPool 为请求日志里的号池标记：经 /ultra 进入为 ultra，经 /pro 进入为 pro，普通路径没有标记
export type RequestPool = 'ultra' | 'pro'

export interface AccountDraft {
  label: string
  enabled: boolean
  proxy: string
  locale: string
  timezone: string
}

export interface AccountLoginInput {
  proxy: string
  locale: string
  timezone: string
}

export interface ChromeImportProfile {
  id: string
  profile: string
  display_name: string
  email: string
  locale: string
  // existing 表示邮箱已是号池里的账户：导入时就地重新导入（只覆盖 Cookie 与续签材料，保留设置、指纹、资源绑定与运行态）；
  // state 为该账户当前的状态
  existing?: boolean
  state?: AccountState
}

export interface ChromeImportInput extends AccountLoginInput {
  account_ids: string[]
}

// ChromeImportFailure 为一个没有就地重新导入的已有账户：id 为 Chrome 账号 ID，email 为账户邮箱，code 为 account_busy 等错误码
export interface ChromeImportFailure {
  id: string
  email: string
  code?: string
  message: string
}

// ChromeImportResult 为 Chrome 导入的结果：accounts 为新增与就地重新导入成功的账户，failed 为重新导入失败的已有账户
// （逐个处理，一个失败不打断后面的；旧版服务没有这个字段）
export interface ChromeImportResult {
  accounts: Account[]
  failed?: ChromeImportFailure[] | null
}

export interface AccountCounters {
  total: number
  ready: number
  busy: number
  cooldown: number
  auth_required: number
  // auth_required_10m 为最近 10 分钟新被标为需要登录的账户数（只在全部账户的计数里有），持续增长说明运行期 401 在累积
  auth_required_10m?: number
  // org_disabled 为所属组织停用了 AI Studio 的启用账户数（旧版服务没有这个字段）
  org_disabled?: number
}

// OnboardingPolicy 为新账户自动处理设置
export interface OnboardingPolicy {
  auto_verify: boolean
  auto_enable: boolean
  batch_size: number
  concurrency: number
}

export type OnboardingResult =
  | 'enabled'
  | 'verified'
  | 'verify_failed'
  | 'error'
  | 'skipped'
  | 'manual'
  | 'baseline'

export interface OnboardingEvent {
  account_id: string
  result: OnboardingResult
  detail?: string
  at: string
}

// BulkEnableInput 为批量启用请求：account_ids 为要启用的账户，all_disabled 为 true 时启用全部停用账户
export interface BulkEnableInput {
  account_ids?: string[]
  all_disabled?: boolean
}

// BulkEnableFailure 为批量启用中没有启用的账户；code 为 account_busy（正在验证、登录或处理请求）、account_not_found 等
export interface BulkEnableFailure {
  id: string
  code?: string
  message: string
}

// BulkEnableResult 为一次批量启用的结果：skipped 为原本已启用的账户数，remaining 为超过单次上限、本次没有处理的账户数
export interface BulkEnableResult {
  enabled: number
  skipped: number
  failed: BulkEnableFailure[] | null
  remaining: number
}

// BulkDeleteFailure 为批量删除中没有删掉的账户；code 为 account_busy（账户一直在处理请求，等不到空闲）等错误码
export interface BulkDeleteFailure {
  id: string
  code?: string
  message: string
}

// BulkDeleteResult 为一次批量删除的结果：deleted 为本次删除的账户数，missing 为号池里已经没有的账户数
// （同时清除它的 403 记录，不算失败，不占单次上限），remaining 为号池里还有、超过单次上限本次没有处理的账户数
export interface BulkDeleteResult {
  deleted: number
  missing: number
  failed: BulkDeleteFailure[] | null
  remaining: number
}

// OnboardingStatus 为新账户自动处理队列状态；next_batch_seconds 为 -1 表示队列为空
export interface OnboardingStatus {
  policy: OnboardingPolicy
  pending: number
  processing: string[] | null
  next_batch_seconds: number
  counts: Partial<Record<OnboardingResult, number>> | null
  recent: OnboardingEvent[] | null
}

// WorkerPartition 为 Worker 分区：普通、Ultra 与 Pro
export type WorkerPartition = 'normal' | 'ultra' | 'pro'

// PrewarmState 为预热循环的实时状态；loop_age_seconds 持续变大说明循环被卡住，round_seconds 为当前这一轮已进行的秒数。
// first_warm_pending 为首轮预热还没出结果的分区：生成服务已在运行，这些分区还没有就绪的 Worker，期间不指定账户的请求排队等
// 首轮预热的第一个 Worker、不另起冷启动；都已出结果时省略，旧版服务端不返回
export interface PrewarmState {
  active: boolean
  inflight: number
  launched: number
  round_seconds: number
  loop_age_seconds: number
  reason?: string
  first_warm_pending?: WorkerPartition[]
}

// WorkerCounters 为 WAA Worker 的实时数量；warm_ids、starting_ids 为对应账户；
// dead 为 warm 中账号长时间冷却（如每日限额）、占着名额却服务不了请求、等待淘汰的死槽位；
// draining 为其中排空中的 Worker（不再接新请求，正在服务的请求结束后关闭，替补已先启动），retiring 为待淘汰登记数；
// unschedulable 为驻留 Worker 中账号已不能服务任何模型的（需要登录、已停用、没有模型目录、需要验证账号的整户暂停中），等待释放；旧版服务端不返回
export interface WorkerCounters {
  warm: number
  starting: number
  target: number
  max: number
  occupied?: number
  dead?: number
  draining?: number
  retiring?: number
  unschedulable?: number
  prewarm?: PrewarmState
  warm_ids?: string[]
  starting_ids?: string[]
  // extra_ids 为 warm_ids、starting_ids 中计入补充池的账户（带补充池标记）；没有时省略，旧版服务端不返回
  extra_ids?: string[]
}

// PartitionWorkerCounters 为 Ultra 分区或 Pro 分区的 Worker 数量与上限；occupied 为占用的槽位（含正在关闭的），
// dead 为其中账号长时间冷却、等待淘汰的死槽位，draining 为其中排空中的，retiring 为待淘汰登记数，
// unschedulable 为其中账号已不能服务任何模型、等待释放的，warm_limit 与 max 为该分区的常驻数与峰值数设置
export interface PartitionWorkerCounters {
  warm: number
  starting: number
  occupied: number
  dead?: number
  draining?: number
  retiring?: number
  unschedulable?: number
  target: number
  warm_limit: number
  max: number
}

// PrimarySupply 为一个主用模型的可用供给（约每 30 秒统计一次）：warm 为能服务它的常驻 Worker，
// standby 为还能服务它的空闲备用账号，最多统计 1000 个，达到上限时 standby_capped 为 true。
// cooling 为对它冷却中的常驻 Worker（不在排空中、所有支持的启用通道都在冷却，含剩余不到 2 分钟的短冷却），
// 既不计入 warm 也不计入 standby；旧版服务端不返回 cooling。
// pool 为 "ultra" 时是 Ultra/Pro 主用模型（ULTRA_PRIMARY_MODELS），只统计 Ultra 分区的 Worker 与 Ultra 号池的账号；
// 为 "pro" 时是同一份列表在 Pro 分区的供给（只在开启 PRO_POOL 时有）；没有该字段时是普通主用模型（PRIMARY_MODELS），
// 只统计普通分区。旧版服务端不返回 pool
export interface PrimarySupply {
  model: string
  pool?: string
  warm: number
  cooling?: number
  standby: number
  standby_capped?: boolean
}

// AuthRecheckStatus 为登录复核：上游 401 先记疑似登录失效、用磁盘 Cookie 复核，仍是 401 才标需要登录；
// 按接口证据标记的需要登录账户每 15 分钟一轮低速复核，复核通过即恢复。*_1h 为最近一小时的计数
export interface AuthRecheckStatus {
  suspects?: number
  queued?: number
  running?: number
  cleared_1h?: number
  confirmed_1h?: number
  recovered_1h?: number
  still_invalid_1h?: number
  skipped_1h?: number
  next_round?: string
  pause_reason?: string
}

// CatalogSyncStatus 为模型目录写盘缓存与后台同步的状态（服务运行时才有；旧版服务端不返回）：
// cached 为有目录的账户（含来自磁盘的），from_disk 为本代运行时启动时从磁盘恢复的，missing 为启用、就绪却没有目录的，
// expired 为超过有效期等待刷新的，auth_suspects 为疑似登录失效、等待探测的；queued、running 为排队与进行中的同步；
// paused_until 与 pause_reason（上游 429 | 出口限流）为暂停；*_1h 为最近一小时的计数，skipped_busy_1h 为账户有进行中的请求、推后刷新的次数
export interface CatalogSyncStatus {
  cached?: number
  from_disk?: number
  missing?: number
  expired?: number
  auth_suspects?: number
  queued?: number
  running?: number
  paused_until?: string
  pause_reason?: string
  synced_1h?: number
  failed_1h?: number
  rate_limited_1h?: number
  skipped_busy_1h?: number
}

// EgressLane 为出口内一个分区通道（普通、Ultra）的令牌桶与冷启动配额：limit_rpm 为每分钟生成请求上限（0 为不限），
// sent_1m 为最近 60 秒发往上游的生成请求，cold_starting 为冷启动闸门放行、正在启动的，refill_deferred_1m 为最近一分钟因补位并发已满被推迟的补位
export interface EgressLane {
  lane: 'normal' | 'ultra'
  accounts?: number
  limit_rpm?: number
  burst?: number
  sent_1m?: number
  primary_sent_1m?: number
  other_sent_1m?: number
  pending?: number
  cold_starting?: number
  cold_started_1m?: number
  refill_deferred_1m?: number
}

// EgressStatus 为一个出口（账号代理、IP 池端口或会话规范化后的值，直连为“直连”）的限流判定与限速：mode 为 normal（没有判定）、limited（判定为出口限流）
// 或 rejected（判定已撤销，期间不再判定）；limiter 为 off、active（限速生效）或 suspended（限速无效已停止）。
// ratio_2m 为发往上游的请求被裸 429 拒绝的比例（0–1），local_429_1m 为本地返回的出口限速 429；id 用于撤销与恢复判定。
// 字段均可选，兼容旧版服务端
export interface EgressStatus {
  id: string
  label: string
  mode: 'normal' | 'limited' | 'rejected'
  limiter?: 'off' | 'active' | 'suspended'
  since?: string
  accounts?: number
  lanes?: EgressLane[]
  success_1m?: number
  bare_429_1m?: number
  other_1m?: number
  ratio_2m?: number
  ratio_entry?: number
  fresh_rejects_2m?: number
  queued?: number
  local_429_1m?: number
  retried?: number
  recovered?: number
  demoted?: number
  relaxed?: number
  retry_at?: string
  rejected_until?: string
  rejected_by?: string
}

// EgressSummary 为全部出口的汇总：IP 池开启后出口多达几百个（静态每个端口、动态每个会话一个），出口多于 16 个时 egress 只列出
// 非 normal 的、normal 只计数；total 为有账户、有活动或有判定记录的出口数
export interface EgressSummary {
  total: number
  normal: number
  limited: number
  rejected: number
}

// ServiceTransition 为生成服务的启动步骤（prepare_catalog、wait_catalog、start_prewarm）与停止步骤（其余四个，启动失败的清理
// 同样经过后三个）
export type ServiceTransition =
  | 'prepare_catalog'
  | 'wait_catalog'
  | 'start_prewarm'
  | 'cancel_requests'
  | 'wait_catalog_stop'
  | 'close_workers'
  | 'flush_writes'

export interface ServiceStatus {
  // 停止期间 state 已是 STOPPED，transition 为停止步骤；启动失败的清理期间 state 仍是 LAUNCHING
  state: 'STOPPED' | 'LAUNCHING' | 'RUNNING'
  running: boolean
  ready: boolean
  version: string
  active_requests: number
  accounts: AccountCounters
  // workers 的数量与上限只含普通分区，warm_ids、starting_ids 包含全部分区
  workers?: WorkerCounters
  // ultra_accounts 为 Ultra 号池的账户计数（关闭 PRO_POOL、开启 ULTRA_INCLUDE_PRO 时含并入的 Pro 账户），ultra_workers 为 Ultra 分区的 Worker 计数
  ultra_accounts?: AccountCounters
  ultra_workers?: PartitionWorkerCounters
  // pro_accounts 为 Pro 号池（开启 PRO_POOL 时权益为 Pro 的账户）的账户计数，pro_workers 为 Pro 分区的 Worker 计数；
  // 关闭 PRO_POOL 时各项为 0。旧版服务端不返回
  pro_accounts?: AccountCounters
  pro_workers?: PartitionWorkerCounters
  // pro_pool_notice 为 PRO_POOL 默认开启的升级提示：.env 与环境变量都没有写 PRO_POOL、又有 Pro 号池账户时才有
  pro_pool_notice?: ProPoolNotice
  // extra_pool 为补充池（EXTRA_POOL）的概况：开启，或关闭后仍有补充池 Worker 在归还或释放时才有；旧版服务端不返回
  extra_pool?: ExtraPoolStatus
  // primary_supply 为每个主用模型（PRIMARY_MODELS 与 ULTRA_PRIMARY_MODELS）的可用供给，没有主用模型或服务未运行时没有；
  // 同一个模型同时在两份列表里时有两个条目，用 pool 区分
  primary_supply?: PrimarySupply[]
  // catalog_sync 为模型目录缓存与后台同步的状态，服务未运行时没有
  catalog_sync?: CatalogSyncStatus
  // egress 为出口的限流判定与限速：出口不超过 16 个时全部列出（含 normal 的），更多时只列出判定成立（limited）或已撤销（rejected）的，
  // 正常的只计入 egress_summary；服务未运行时没有。侧栏只显示非 normal 的
  egress?: EgressStatus[]
  // egress_summary 为全部出口的汇总计数，服务运行时总有；旧版服务端不返回
  egress_summary?: EgressSummary
  // auth_recheck 为登录复核的状态，服务未运行时没有
  auth_recheck?: AuthRecheckStatus
  // bad_accounts 为标为坏号（连续裸限流、期间没有成功）、移出预热与替补候选的账号数
  bad_accounts?: number
  // proxy_pool 为 IP 池的实时状态：IP 池开启，或关闭后仍有浏览器在用旧出口时才有；旧版服务端不返回
  proxy_pool?: ProxyPoolStatus
  // build_channel 为 Build 通道生成的暂停状态（官网停用经 Build 代理的生成时）：服务运行且启用了 Build 时才有；旧版服务端不返回
  build_channel?: BuildChannelStatus
  // drive 为启用账户的 Google Drive 授权状态计数（gemini-omni 的视频输入及不能内联的图片只交给可用或未知的账户，PNG、JPEG、WebP 等能内联的图片不需要 Drive）；旧版服务端不返回
  drive?: DriveCounters
  // balanced 为平均调用（ROUTING_STRATEGY=balanced）的概况：开启且服务运行时才有；旧版服务端不返回
  balanced?: BalancedStatus
  // transition 为正在进行的启动或停止步骤，transition_since 为进入它的服务端时刻，没有步骤时都省略；
  // time 为生成这份状态的服务端时刻，管理页据此换算已用时间（不受浏览器时钟偏差影响）。旧版服务端都不返回
  transition?: ServiceTransition
  transition_since?: string
  time?: string
}

// ExtraPoolMode 为补充池的使用方式：share 与主号池一起分担（默认），overflow 只在主号池不够时补充
export type ExtraPoolMode = 'share' | 'overflow'

// ExtraPoolStatus 为补充池（额外浏览器号池）的概况。members 为最近一次对账（约每 5 秒）按进入口径的成员数（含已有 Worker 的），
// free（含 Plus 与权益尚未识别的）、pro、ultra 为其中各权益的个数，cold_members 为其中没有 Worker、也不在启动中的；
// lendable 为补充池 Worker 中仍是成员、可以借出的个数，returning 为已不是成员、等待归还或释放的个数，borrows 为本进程借出次数；
// workers 为补充池 Worker 的计数（target 为常驻数与成员数取小，warm_limit、max 为常驻与峰值设置；dead、draining、retiring 恒为 0）
export interface ExtraPoolStatus {
  enabled: boolean
  mode: ExtraPoolMode
  default_paid: boolean
  include_ultra: boolean
  members: number
  free: number
  pro: number
  ultra: number
  cold_members: number
  lendable: number
  returning: number
  borrows: number
  workers: PartitionWorkerCounters
}

// ProPoolNotice 为 PRO_POOL 默认开启的升级提示：升级前 Pro 账户服务默认地址（或经 ULTRA_INCLUDE_PRO 并入 /ultra），默认开启后
// 只服务 /pro。pro_accounts 为 Pro 号池账户数，pro_available、normal_available、ultra_available 为 Pro、普通与 Ultra 号池里可预热的账户数；
// source 为升级前 Pro 账户所在、建议从中分出 Pro 容量的分区（ULTRA_INCLUDE_PRO 写为 true 时为 ultra，否则为 normal）；
// suggested_* 为按 Pro 可用账户占它与来源分区可用账户之和的比例分来源分区常驻与峰值数得到的建议值（Pro 不少于默认值，来源分区减去分走的份额，
// 不少于该分区默认值），另一个分区的建议值与当前值相同。旧版服务端没有 source 与 Ultra 字段，按 normal
export interface ProPoolNotice {
  pro_accounts: number
  source?: 'normal' | 'ultra'
  pro_available: number
  normal_available: number
  ultra_available?: number
  warm_worker_limit: number
  max_active_workers: number
  ultra_warm_worker_limit?: number
  ultra_max_active_workers?: number
  pro_warm_worker_limit: number
  pro_max_active_workers: number
  suggested_pro_warm_worker_limit: number
  suggested_pro_max_active_workers: number
  suggested_warm_worker_limit: number
  suggested_max_active_workers: number
  suggested_ultra_warm_worker_limit?: number
  suggested_ultra_max_active_workers?: number
}

// BalancedStatus 为平均调用的概况，服务端每 30 秒统计一次：quota_day 为当前太平洋时间额度日（北京时间 15 点或 16 点清零），
// gap 为轮换门槛（在跑的号比接替它的备用号多发这么多次才换下），normal、ultra 为两个分区的今日发送数分布，
// pro 为 Pro 分区的分布（只在 Pro 号池有可调度账号时有），
// rotations_1h、rotations_today 为最近 1 小时与本额度日完成（旧号已关闭）的轮换次数，还在进行、取消或失败的不计。
// 发送数为本服务真正发往上游的生成请求数（每次发送都计），与用量页按本地日、每个请求一行的口径不同
export interface BalancedStatus {
  quota_day: string
  gap: number
  normal: BalancedPartition
  ultra: BalancedPartition
  pro?: BalancedPartition
  rotations_1h: number
  rotations_today: number
}

// BalancedPartition 为一个分区的概况：accounts 为可调度账号数（需要验证账号的整户暂停、需要登录等不算），used 为其中今天发送过的账号数，
// min、median、p90、max 为它们今日发送数的分布；rotating 为进行中的轮换数，paused_reason 为轮换暂停的原因（没有暂停时省略）
export interface BalancedPartition {
  accounts: number
  used: number
  min: number
  median: number
  p90: number
  max: number
  rotating: number
  paused_reason?: string
}

// DriveCounters 为启用账户的 Google Drive 授权状态：available 为换到过 Drive token 或经本服务登录时完成了 Drive 授权，unavailable 为
// 没有 Drive 授权（GenerateAccessToken 返回 401），storage_full 为有授权但 Drive 存储空间已满（上传返回 403 storageQuotaExceeded，
// 为 0 或旧版服务端时省略），unknown 为还没试过、更新认证文件后或记录过期。各记 24 小时，写在 runtime/model-catalogs/drive-accounts.json，
// 服务重启后沿用
export interface DriveCounters {
  available: number
  unavailable: number
  storage_full?: number
  unknown: number
}

// BuildChannelStatus 为 Build 通道生成的暂停状态：Build 代理返回 PROXY_GENERATION_DISABLED 时先按账号暂停，短时间内 3 个账号都遇到即
// 全局暂停（paused，生成改走 Playground），之后定期放行一个真实请求试探（since、next_probe 只在全局暂停时有，probing 为试探进行中），
// 成功即解除；paused_accounts 为按账号暂停中的账号数，awaiting_probe 为按账号暂停已到期、等待试探的账号数（没有时省略）；
// 全局暂停时 paused_accounts 为遇到过的账号数，awaiting_probe 为 0。进程内状态，服务重启后从头判定
export interface BuildChannelStatus {
  paused: boolean
  since?: string
  next_probe?: string
  probing?: boolean
  paused_accounts: number
  awaiting_probe?: number
}

// ProxyPoolMode 为 IP 池模式：off 关闭（没有自己代理的账户用默认代理），static 静态 IP（端口段里每个浏览器独占一个端口），
// dynamic 动态 IP（地址里的 {session} 按账户换成固定会话号）
export type ProxyPoolMode = 'off' | 'static' | 'dynamic'

// ProxyPoolConfig 为 IP 池设置（PROXY_POOL_*，保存后立即生效）。url 可能带账号密码，只在 /api/config 返回给管理页编辑，
// 页面按掩码显示；port_start、port_end 为静态 IP 的端口段（含两端），任何模式下都要合法
export interface ProxyPoolConfig {
  mode: ProxyPoolMode
  url: string
  port_start: number
  port_end: number
}

// ProxyPoolCoolingPort 为冷却中的端口：until 为恢复时间，level 为冷却档位（第 n 档 15 分钟 × 2^(n-1)，最长 2 小时）
export interface ProxyPoolCoolingPort {
  port: number
  until: string
  level: number
}

// ProxyPoolHotPort 为最近 10 分钟裸 429 最多的端口：rate_limited 为次数，accounts 为返回过裸 429 的不同账号数
export interface ProxyPoolHotPort {
  port: number
  rate_limited: number
  accounts: number
}

// ProxyPoolAssignment 为一个活动的 IP 池租约：slot 为端口号或会话号；kind 为 worker（Worker）或 login（登录、验证、导入）；
// 新账户登录中还没有账户 ID 时 account_id 为空
export interface ProxyPoolAssignment {
  account_id: string
  slot: string
  kind: 'worker' | 'login'
}

// ProxyPoolDuplicate 为“检查全部端口”中出口 IP 相同的一组端口
export interface ProxyPoolDuplicate {
  ip: string
  ports: number[]
}

// ProxyPoolFailure 为“检查全部端口”中失败的端口；error 已去掉账号密码
export interface ProxyPoolFailure {
  port: number
  error: string
}

// ProxyPoolCheck 为最近一次“检查全部端口”（只走后台请求路径）：done 为已检查的端口数，ok 为拿到出口 IP 的端口数，
// distinct_ips 为其中不同 IP 的个数；duplicates 最多 20 组，failed 最多 50 个
export interface ProxyPoolCheck {
  running: boolean
  started_at: string
  finished_at?: string
  total: number
  done: number
  ok: number
  distinct_ips: number
  duplicates?: ProxyPoolDuplicate[]
  failed?: ProxyPoolFailure[]
}

// ProxyPoolPauseReason 为固定分配下端口暂停使用的原因：cooling_upstream（连续出口连不通的冷却）、cooling_rate_limited（不同账号返回
// 限流的冷却）、cooling（冷却中、规则未知，例如重启后读回）、locked_elsewhere（被其他进程占用）
export type ProxyPoolPauseReason =
  | 'cooling_upstream'
  | 'cooling_rate_limited'
  | 'cooling'
  | 'locked_elsewhere'

// ProxyPoolPausedPort 为固定分配下暂停使用的端口：until 为冷却恢复时间或下次检查时间，accounts 为这个端口上的固定账户数
export interface ProxyPoolPausedPort {
  port: number
  reason: ProxyPoolPauseReason | string
  until?: string
  accounts: number
}

// ProxyPoolFixedStatus 为 IP 池固定分配（PROXY_POOL_FIXED=true）的状态，模式为 static 或 dynamic（不含地址）。静态：ports 为参与分配的端口数
// （端口段内、没有停用的），accounts 为分配表里的账户数，min_per_port / max_per_port 为参与分配的端口上的最少 / 最多账户数，empty 为没有
// 账户的端口，stray 为分配表里已不在账户池的账户（保留位置），unassigned 为走 IP 池却没有分到端口的账户（正常为 0），reserved 为登录、
// 导入中预留的位置，transitional 为运行中、还不在固定端口上的 Worker，external_ports 为有固定账户、同时被账户固定代理或 PROXY 指向的端口，
// excluded 为“换 IP”后停用的端口（最多 50 个，excluded_total 为总数），paused_ports / paused_accounts 为暂停使用的端口与其上的账户数，
// paused 为暂停的端口（最多 50 个，按恢复时间）。动态：accounts 为走 IP 池的账户数，其余为 0
export interface ProxyPoolFixedStatus {
  mode: 'static' | 'dynamic'
  since?: string
  ports?: number
  accounts: number
  min_per_port: number
  max_per_port: number
  empty?: number
  stray?: number
  unassigned?: number
  reserved?: number
  transitional?: number
  external_ports?: number
  excluded?: number[]
  excluded_total?: number
  paused_ports: number
  paused_accounts: number
  paused?: ProxyPoolPausedPort[]
}

// ProxyPoolStatus 为 IP 池的实时状态（不含账号密码）：
// total 为静态 IP 的端口数（动态为 0），in_use 为有浏览器占用的端口（动态为有浏览器在用的会话），leases 为 IP 池分配的活动租约
// （Worker 与登录、验证），capacity 为可能同时运行的浏览器数 Σ(峰值 + 排空上限)（不含登录与验证）；shared 为被两个及以上账号
// 同时占用的端口，external 为账户固定代理、默认代理指向或被其他进程锁住、不再分配的端口，stale 为改配置前分出、浏览器还没关闭的
// 出口，unpooled 为 IP 池开启时仍走默认代理的 Worker，orphaned 为账户已删除、浏览器关闭失败仍占着的租约，pinned_default 为
// 账户代理与默认代理相同、因此不用 IP 池的账户数；relay_limit 为本机代理中转的进程连接上限
export interface ProxyPoolStatus {
  mode: ProxyPoolMode
  endpoint?: string
  port_start?: number
  port_end?: number
  total: number
  in_use: number
  leases: number
  capacity: number
  shared: number
  cooling: number
  external: number
  stale: number
  unpooled: number
  orphaned: number
  pinned_default: number
  rotations?: number
  affinity_hits: number
  affinity_misses: number
  cooling_ports?: ProxyPoolCoolingPort[]
  hot_ports?: ProxyPoolHotPort[]
  assignments?: ProxyPoolAssignment[]
  check?: ProxyPoolCheck
  relay_limit?: number
  // fixed 为固定分配的状态：开启 PROXY_POOL_FIXED 且模式不为关闭时才有；旧版服务端不返回
  fixed?: ProxyPoolFixedStatus
}

// ProxyPoolFixedRebalanceResult 为“重新平均分配”的结果：dry_run 为只计算、没有改分配；accounts 为分配表里的账户数（清理之后），ports 为参与
// 分配的端口数，moved 为换了端口的账户数，pruned 为清理掉的已不在账户池的记录（服务停止时不清理），running 为被挪账户里有运行中 Worker 的
// （关闭后改用新端口），min_per_port / max_per_port 为之后每个端口的最少 / 最多账户数
export interface ProxyPoolFixedRebalanceResult {
  dry_run: boolean
  accounts: number
  ports: number
  moved: number
  pruned: number
  running: number
  min_per_port: number
  max_per_port: number
}

// ProxyPoolFixedReassignResult 为“给端口换 IP”的结果：moved 为挪走的账户数，targets 为挪到的端口（去重升序，最多 20 个），running 为被挪
// 账户里有运行中 Worker 的（不论在哪个端口），closing 为此刻用这个端口服务、服务运行中时登记后台关闭的 Worker 数（含固定端口已在别处的
// 过渡 Worker，预演与执行同一口径；旧版服务没有），restarted 为执行时真正登记了后台关闭的 Worker（等进行中的请求结束，下次启动用各自的
// 固定端口），excluded 为这个端口已停用
export interface ProxyPoolFixedReassignResult {
  dry_run: boolean
  port: number
  moved: number
  targets?: number[]
  running: number
  closing?: number
  restarted: number
  excluded: boolean
}

// ProxyPoolFixedRestoreResult 为恢复停用端口的结果：restored 为假时这个端口本来就没有停用
export interface ProxyPoolFixedRestoreResult {
  port: number
  restored: boolean
}

// FollowDefaultProxyResult 为“改为跟随默认代理”的结果：changed 为清空了账户代理的账户数，running 为其中仍有运行中 Worker
// （关闭后才改用 IP 池）的账户数，skipped 为正在被其他操作使用、本次跳过的账户数
export interface FollowDefaultProxyResult {
  changed: number
  running: number
  skipped: number
}

export type WorkerState = 'warm' | 'starting' | 'none'

export interface AdminLog {
  time: string
  level: string
  source: string
  message: string
  event: string
  request?: RequestLog
}

// RequestLog 对应请求日志的结构化载荷
export interface RequestLog {
  id: string
  state: 'running' | 'completed' | 'tool_calls' | 'limited' | 'blocked' | 'failed' | 'cancelled'
  model?: string
  method?: string
  // path 保留 /ultra、/pro 前缀；pool 为 ultra 表示请求经 /ultra 进入，只用 Ultra 号池的账户，pro 表示经 /pro 进入，只用 Pro 号池的账户
  path?: string
  pool?: RequestPool
  status?: number
  duration_ms?: number
  tool_calls?: number
  finish_reason?: string
  error?: string
  input_messages?: number
  input_text_chars?: number
  input_media?: number
  input_media_bytes?: number
  input_files?: number
  parameters?: Record<string, string>
  first_event_ms?: number
  upstream_bytes?: number
  channel?: UpstreamChannel
  // disconnect 为请求取消（cancelled）时的现场：谁断开、断开阶段、断开前写出的字节与块数、最后一次写出与上游最近一次事件距断开多久
  disconnect?: RequestDisconnect
  usage?: {
    input_tokens: number
    reasoning_tokens: number
    reply_tokens: number
    output_tokens: number
    total_tokens: number
    // average_tokens_per_second 为输出除以请求总耗时（原口径）；generation_tokens_per_second 为输出除以首个上游分块之后到结束的时长，
    // 只有一个分块或旧版服务端时没有
    average_tokens_per_second: number
    generation_tokens_per_second?: number
  }
  downgrade?: DowngradeDecision
  // fallback_from 为额度用完改用其他模型（MODEL_FALLBACKS）前的模型，model 为改用后实际使用的模型；没有改用时不出现
  fallback_from?: string
  // extra 为请求由补充池服务（最后一次尝试的租约借自补充池）；与 pool（入口）相互独立，其余请求不出现
  extra?: boolean
}

// RequestDisconnect 对应请求日志里取消请求的断开现场
export interface RequestDisconnect {
  initiator: 'client' | 'service'
  phase: 'before_first_event' | 'mid_stream'
  headers_sent: boolean
  written_bytes: number
  written_chunks: number
  // 最后一次写出（含换行与 ": ping" 心跳、响应头）到断开的时长
  since_last_write_ms: number
  // 最后一次写出中转会转发的数据（data 行、非 SSE 响应体，含数据型心跳）到断开的时长；一块数据都没写过时省略
  since_last_data_write_ms?: number
  // 提交响应头到断开的时长；没提交时省略
  since_headers_sent_ms?: number
  // 提交响应头后 1 秒内断开（与阶段无关，经中转时多为下游在响应头之前已断开）；为假时省略
  right_after_headers?: boolean
  since_upstream_event_ms?: number
  elapsed_ms: number
}

export interface Model {
  id: string
  name: string
  description?: string
  methods: string[]
  input_token_limit?: number
  output_token_limit?: number
  capabilities?: Record<string, boolean>
  capability_options?: Record<string, string[]>
  access_modes?: number[]
  paid?: boolean
  channels?: UpstreamChannel[]
  // build_unary 为真表示模型需要订阅权益，在 Build 通道只能一次性返回整段回复
  build_unary?: boolean
}

export type UpstreamChannel = 'playground' | 'build'

export interface Cooldown {
  account_id: string
  account_label: string
  channel: UpstreamChannel
  model_id: string
  until: string
  reason?: string
  // verify 为需要验证账号的冷却种类：account 为整户暂停（按目录模型展开的每一条），model 为单个模型需要验证（只暂停这个模型）；
  // 其他冷却与旧版服务省略
  verify?: 'account' | 'model'
}

// PermissionDeniedRequest 为一个模型最近一次 403 的请求来源（不含 API 密钥、IP、User-Agent 与提示词）：id 为请求 ID，path 为接口路径，
// requested_model 为客户端请求的模型名（与登记的模型相同时省略）；messages、images、files 为消息条数、内联图片数与文件引用数（为 0 时省略）
export interface PermissionDeniedRequest {
  id?: string
  path?: string
  requested_model?: string
  messages?: number
  images?: number
  files?: number
}

// PermissionDeniedModel 为账户在一个模型上的 403 无权限记录；channels 为返回过 403、之后还没有成功过的通道；
// last_request 为最近一次 403 的请求来源（旧版本服务与旧记录没有）
export interface PermissionDeniedModel {
  model: string
  channels: UpstreamChannel[]
  count: number
  first_at: string
  last_at: string
  message?: string
  last_request?: PermissionDeniedRequest
}

// PermissionDeniedAccount 为返回过 403 无权限的账户（多半未同意 Google 政策），重新获取认证文件上传后清除
export interface PermissionDeniedAccount {
  id: string
  label: string
  models: PermissionDeniedModel[]
  count: number
  first_at: string
  last_at: string
}

// OrgDisabledAccount 为所属组织停用了 AI Studio 的账户（Google Workspace 账号的组织关闭了这项服务，重新登录也没用）：
// source 为第一次进入的来源（启动、模型目录、验证、登录、请求、登录复核），last_source 为最近一次；check_at 为下一次长间隔复查的时间；
// message 为最近一次的原文（服务端已打码、截短）。服务受限、年龄或使用资格、Workspace 未开放的账户同样列在这里：
// restriction 为原因种类、reason 为服务端的说明（旧版服务没有这两个字段，按组织停用显示）
export interface OrgDisabledAccount {
  id: string
  label: string
  source: string
  last_source: string
  count: number
  first_at: string
  last_at: string
  check_at: string
  message?: string
  restriction?: AccountRestriction
  reason?: string
}

export type RequestState = 'queued' | 'running' | 'completed' | 'cancelled' | 'failed'

export interface RequestSummary {
  id: string
  model: string
  account_id: string
  account_label: string
  channel?: UpstreamChannel
  state: RequestState
  started_at: string
  // pool 为 ultra 表示请求经 /ultra 进入，pro 表示经 /pro 进入
  pool?: RequestPool
  // fallback_from 为额度用完改用其他模型（MODEL_FALLBACKS）前的模型，model 为改用后的模型；没有改用时不出现
  fallback_from?: string
  // extra 为这次尝试的租约借自补充池（正在由补充池服务）；其余请求不出现
  extra?: boolean
}

// DowngradeGuardConfig 为"拒绝被上游降级的回复"的设置（服务配置页，修改后立即生效）
export interface DowngradeGuardConfig {
  enabled: boolean
  models: string[]
  speed_threshold: number
  min_tokens: number
  min_window_ms: number
  fuzzy_low: number
  fuzzy_high: number
  count_timeout_ms: number
  fast_mode: boolean
  memory_minutes: number
  max_hold_ms: number
  // reject_status 为因降级拒绝时返回的 HTTP 状态码：400 按内容策略拦截返回，503 按服务暂时不可用返回
  reject_status: 400 | 503
}

// DowngradeDecision 为一次降级判定的依据（请求日志的 downgrade 字段）
export interface DowngradeDecision {
  verdict: 'rejected' | 'passed' | 'unjudged'
  reason?: string
  mode?: string
  channel?: string
  served_model?: string
  speed?: number
  estimated_speed?: number
  tokens?: number
  window_ms?: number
  estimated?: boolean
  count_tokens?: boolean
  count_tokens_ms?: number
  count_tokens_error?: string
  memory?: boolean
  held_ms?: number
  text_held_ms?: number
  capped?: boolean
  basis: string
}

// TierRuleHint 为一条比账户目录更严的权益规则：key（ULTRA_ONLY_MODELS 或 PAID_ONLY_MODELS）里的模型 model 把权益 tier 排除在外，
// 但 accounts 个这一权益的启用账户的目录显示能用它；marked 为这些账户的目录明确标注了这一权益可用，否则它们的目录只是没有标注
// 权益限制。同一权益两种账户都有时分成两条，各自计数
export interface TierRuleHint {
  key: 'ULTRA_ONLY_MODELS' | 'PAID_ONLY_MODELS'
  model: string
  tier: string
  marked?: boolean
  accounts: number
}

export interface ServiceConfig {
  auth_states: string
  listen_addr: string
  proxy_api_key: string
  active_listen_addr: string
  active_proxy_api_key: string
  management_restart_required: boolean
  service_restart_required: boolean
  proxy: string
  init_timeout: string
  request_timeout: string
  // first_event_timeout 为每次尝试等待首个上游事件的上限，0s 表示关闭
  first_event_timeout: string
  // stream_data_heartbeat 为数据型心跳（STREAM_DATA_HEARTBEAT）：流式心跳写中转会转发的空数据块，保存后立即生效。
  // 服务端总是返回该字段，因此这里不是可选字段
  stream_data_heartbeat: boolean
  // warm_worker_limit 与 max_active_workers 只约束普通分区的 Worker（不含 Pro 与 Ultra）
  warm_worker_limit: number
  max_active_workers: number
  // ultra_exclusive 为真时 Ultra 号池的账户只服务 /ultra 请求；ultra_warm_worker_limit 可以为 0（只按需启动），
  // ultra_max_active_workers 至少为 1 且不小于 Ultra 常驻数
  ultra_exclusive: boolean
  // ultra_include_pro 为真时权益为 Pro 的账户并入 Ultra 号池，按 Ultra 号池的全部设置调度，账户的真实权益不变；
  // 开启 pro_pool 时不生效（保存原值）。服务端总是返回该字段，因此这里不是可选字段：缺了它，设置页的快照对比会一直显示未保存
  ultra_include_pro: boolean
  // pro_pool 为 Pro 独立号池（PRO_POOL，默认开启，保存后立即生效）：权益为 Pro 的账户只服务 /pro 前缀，不借用其他号池；
  // pro_warm_worker_limit 可以为 0，pro_max_active_workers 至少为 1 且不小于 Pro 常驻数。服务端总是返回这三个字段
  pro_pool: boolean
  pro_warm_worker_limit: number
  pro_max_active_workers: number
  // extra_pool 为补充池（EXTRA_POOL，默认关闭，保存后立即生效）：有模型冷却、其他模型还能用的账户单独开浏览器，补给默认地址、/pro 与 /ultra；
  // extra_pool_mode 为 share（一起分担）或 overflow（不够时才补）；extra_pool_default_paid 为默认地址能否借补充池里的 Pro（含 Ultra 时还有 Ultra）
  // 账户跑本路由列表之外的模型；extra_pool_include_ultra 为 Ultra 账户能否进补充池；extra_warm_worker_limit 可以为 0，
  // extra_max_active_workers 至少为 1 且不小于常驻数。服务端总是返回这六个字段，因此这里不是可选字段
  extra_pool: boolean
  extra_pool_mode: ExtraPoolMode
  extra_pool_default_paid: boolean
  extra_pool_include_ultra: boolean
  extra_warm_worker_limit: number
  extra_max_active_workers: number
  // ultra_only_models 为只有 Ultra 权益的账户能用的模型（ULTRA_ONLY_MODELS），paid_only_models 为只有 Pro 与 Ultra 能用的模型
  // （PAID_ONLY_MODELS）；空数组表示没有规则，保存后立即生效。旧版服务端不返回这两个字段
  ultra_only_models?: string[]
  paid_only_models?: string[]
  // tier_rule_hints 为比账户目录更严的权益规则（只读，保存时服务端忽略）：某个权益的账户目录显示能用规则排除的模型
  tier_rule_hints?: TierRuleHint[]
  ultra_warm_worker_limit: number
  ultra_max_active_workers: number
  warm_startup_concurrency: number
  per_account_concurrency: number
  // account_rpm_limit 为单账号最近 60 秒最多发出的生成请求数，0 表示关闭。服务端总是返回该字段，因此这里不是可选字段
  account_rpm_limit: number
  // studio_key_delay 为批量获取 AI Studio API Key 时相邻两个账户之间的间隔（秒），服务端总是返回该字段
  studio_key_delay: number
  routing_strategy: 'round-robin' | 'fill-first' | 'balanced'
  upstream_channels: UpstreamChannel[]
  // stream_playground_models 为流式请求优先走 Playground 的模型；旧版服务端不返回该字段
  stream_playground_models?: string[]
  // primary_models 为主用模型，primary_queue_wait 为它们等空闲账号或 Worker 的上限（0s 表示不排队）；旧版服务端不返回这两个字段。
  // 这份列表只作用于普通号池
  primary_models?: string[]
  primary_queue_wait?: string
  // ultra_primary_models 为 Ultra/Pro 主用模型（空数组表示关闭），作用于 /ultra 与 /pro 请求（以及 ULTRA_EXCLUSIVE 关闭时普通路径上
  // 只有 Ultra 号池的账户能服务的模型），这些模型没有额度时立即返回 429；ultra_primary_queue_wait 为它们的排队上限，
  // 规则与 primary_queue_wait 相同。旧版服务端不返回这两个字段
  ultra_primary_models?: string[]
  ultra_primary_queue_wait?: string
  // model_fallbacks 为额度用完时改用的模型（MODEL_FALLBACKS，规则 源模型=目标1|目标2，逗号分隔，空字符串表示没有规则），
  // model_fallback_scope 为生效范围（MODEL_FALLBACK_SCOPE：ultra 只对 /ultra 请求、all 普通路径、/pro 与 /ultra、off 关闭）；都保存后立即生效。
  // 服务端总是返回这两个字段，因此这里不是可选字段
  model_fallbacks: string
  model_fallback_scope: 'ultra' | 'all' | 'off'
  waa_backend: 'camoufox' | 'go'
  temporary_chat: boolean
  ignore_client_seed: boolean
  repeat_prompt_nonce: boolean
  // abnormal_finish_error 为重新生成用完仍异常结束且没有输出时按 502 返回（ABNORMAL_FINISH_ERROR，默认关闭，保存后立即生效）。
  // 服务端总是返回该字段，因此这里不是可选字段
  abnormal_finish_error: boolean
  min_output_tokens: number
  // long_output_enabled 为模型名带 long- 前缀时自动续写（LONG_OUTPUT_ENABLED），long_output_max_tokens 为没有官网续写能力的模型
  // 在客户端没有给出输出上限时的输出总上限（LONG_OUTPUT_MAX_TOKENS，0 表示不模拟续写）；都保存后立即生效。
  // 服务端总是返回这两个字段，因此这里不是可选字段
  long_output_enabled: boolean
  long_output_max_tokens: number
  downgrade_guard: DowngradeGuardConfig
  // proxy_pool 为 IP 池设置；旧版服务端不返回，此时设置页不显示 IP 池分组，保存时也不提交（服务端沿用现值）
  proxy_pool?: ProxyPoolConfig
  // proxy_pool_fixed 为 IP 池固定分配（PROXY_POOL_FIXED，默认关闭，保存后立即生效）：静态 IP 时每个走 IP 池的账户固定一个端口、按账户数
  // 平均分到端口段的端口上，之后不自动改变；动态 IP 时不再自动换会话。旧版服务端不返回，此时不显示开关、保存时也不提交（服务端沿用现值）
  proxy_pool_fixed?: boolean
  // media_output 为生成媒体的输出方式：inline 内联 data URI（Gemini 原生接口为 inlineData），url 保存到 runtime/media/ 并以本服务
  // /media/<文件名> 链接引用；media_base_url 为链接使用的服务地址（为空时按请求地址）；media_retention 为保存期限（7d、72h，0 不清理），
  // media_max_size 为媒体目录总量上限（20GB、500MB，0 不限）。服务端总是返回这几个字段，保存后立即生效
  media_output: 'inline' | 'url'
  media_base_url: string
  media_retention: string
  media_max_size: string
  // omni_ignore_system_prompt 为视频模型忽略系统提示（OMNI_IGNORE_SYSTEM_PROMPT）：发给能输出视频的 Omni 模型的请求去掉客户端的系统提示，
  // 保存后立即生效。服务端总是返回该字段，因此这里不是可选字段
  omni_ignore_system_prompt: boolean
  // omni_image_binding 为视频模型带图校验值（OMNI_IMAGE_BINDING）：开启时 Omni 带内联图片的请求把图片按 GenerateContent 的写法计入 WAA 校验值，
  // 关闭时校验值只含文本（与原先相同），保存后立即生效。服务端总是返回该字段，因此这里不是可选字段
  omni_image_binding: boolean
  // invalid_config 为 .env 或进程环境变量里取值无效、已按默认值生效的配置项，只读；没有时服务端不返回
  invalid_config?: InvalidConfigValue[]
}

// InvalidConfigValue 为一项取值无效、已按默认值生效的配置：value 为原值（凭据类配置不返回），reason 为服务端的说明，
// environment 为取值来自进程环境变量（在设置页保存改不了它）
export interface InvalidConfigValue {
  key: string
  value?: string
  reason: string
  environment?: boolean
}

// ProxyTestProbe 为“测试代理”一条路径的结果：成功时 ip 为出口 IP，失败时 error 为说明（不含账号密码）
export interface ProxyTestProbe {
  ip?: string
  elapsed_ms: number
  error?: string
}

// ProxyTestSample 为抽测 IP 池时一个出口的结果：slot 为端口号或临时会话号，其余同 ProxyTestResult
export interface ProxyTestSample {
  slot: string
  browser: ProxyTestProbe
  direct: ProxyTestProbe
  same_ip: boolean
}

// ProxyTestResult 为“测试代理”的结果：browser 为 Camoufox 的路径（带账号密码时经本机中转），direct 为后台请求的路径
// （直连上游代理）；same_ip 在两边都拿到 IP 且相同时为真。抽测 IP 池时 samples 为每个出口的结果、distinct_ips 为不同出口 IP 数；
// 测试账户实际出口时 slot 为它当前的端口号或会话号（不在 IP 池时为空）
export interface ProxyTestResult {
  proxy: string // 去掉账号密码的 scheme://host:port
  authenticated: boolean
  relayed: boolean
  browser: ProxyTestProbe
  direct: ProxyTestProbe
  same_ip: boolean
  warnings?: Array<'rotating_ip' | 'https_plain_browser' | 'duplicate_ip' | 'pool_partial'>
  samples?: ProxyTestSample[]
  distinct_ips?: number
  slot?: string
}

export type AdminEvent =
  | { type: 'status'; data: ServiceStatus }
  | { type: 'log'; data: AdminLog }
  | { type: 'accounts'; data: { accounts: Account[] } }
  | { type: 'models'; data: { models: Model[] } }
  | { type: 'cooldowns'; data: Cooldown[] }
  | { type: 'request'; data: RequestSummary }

export type PlaygroundProtocol = 'openai-chat' | 'openai-responses' | 'anthropic' | 'gemini'

export type PlaygroundMode = 'text' | 'image' | 'speech' | 'music' | 'video'

export type PlaygroundReasoning = '' | 'low' | 'medium' | 'high' | 'xhigh'

export type PlaygroundTool =
  '' | 'web_search' | 'image_search' | 'code_interpreter' | 'url_context' | 'google_maps'

export interface PlaygroundInput {
  mode: PlaygroundMode
  protocol: PlaygroundProtocol
  model: string
  prompt: string
  system: string
  stream: boolean
  reasoning: PlaygroundReasoning
  tool: PlaygroundTool
  imageSize: 'auto' | '1024x1024' | '1536x1024' | '1024x1536'
  imageQuality: 'auto' | 'low' | 'medium' | 'high'
  voice: string
  apiKey: string
  // pool 为请求经哪个入口发送：normal 为默认地址，pro 经 /pro 前缀（只由 Pro 号池的账户处理），ultra 经 /ultra 前缀
  pool: AccountPool
}

export interface PlaygroundMedia {
  mime: string
  url: string
}

// PlaygroundError 为试用请求失败的详情：status 为 HTTP 状态码（流已建立后失败时 inStream 为 true，status 为建流时的 200），
// code 为错误外壳中的错误码或类型，retryAfter 为响应头 Retry-After 的秒数
export interface PlaygroundError {
  status: number
  code: string
  message: string
  retryAfter?: number
  inStream: boolean
}

// PlaygroundResult 为一次试用的结果：finish 为最后一个非空的结束原因（各协议原文），
// completed 为请求正常结束，stopped 为用户点了停止，error 为失败详情（已收到的内容保留）
export interface PlaygroundResult {
  text: string
  reasoning: string
  tools: string
  media: PlaygroundMedia[]
  raw: string
  durationMs: number
  status: number
  finish: string
  completed: boolean
  stopped: boolean
  error?: PlaygroundError
}

// UsageDimension 为用量可筛选与分组的维度；pool 为号池，取值为 normal、pro 或 ultra
export type UsageDimension = 'model' | 'account' | 'channel' | 'protocol' | 'state' | 'pool'

export type UsageFilters = Partial<Record<UsageDimension, string[]>>

export interface UsageLatency {
  avg_ms: number
  p50_ms: number
  p95_ms: number
  p99_ms: number
}

// UsageStats 为一组请求的结果、token 与耗时；成功包含工具调用、达到输出上限与上游终止
export interface UsageStats {
  requests: number
  succeeded: number
  failed: number
  canceled: number
  rate_limited: number
  input_tokens: number
  reasoning_tokens: number
  reply_tokens: number
  total_tokens: number
  duration: UsageLatency
  first_event: UsageLatency
  queue_avg_ms: number
  // downgrade_checked 为经过降级判定的请求数，downgrade_rejected 为其中因降级被拒绝的请求数
  downgrade_checked: number
  downgrade_rejected: number
  // replies 为参与重复回复统计的请求数，duplicate_replies 为其中与近期某次回复完全相同的请求数
  replies: number
  duplicate_replies: number
  last_at?: string
}

export interface UsageBucket extends UsageStats {
  at: string
}

export interface UsageGroup extends UsageStats {
  key: string
}

export interface UsagePair extends UsageStats {
  account: string
  model: string
}

export interface UsageSeries {
  key: string
  other?: boolean
  requests: number[]
  tokens: number[]
}

export interface UsageReport {
  from: string
  to: string
  bucket_seconds: number
  generated_at: string
  latest_at?: string
  totals: UsageStats
  previous: UsageStats
  recent: { minutes: number; requests: number; tokens: number }
  buckets: UsageBucket[]
  stack_by: UsageDimension
  series: UsageSeries[]
  groups: Record<UsageDimension | 'status', UsageGroup[]>
  // pairs 为账户与模型组合明细：用量页的主报告带 pairs=0 不读组合（为空），展开排行行时按 pairs_account 或 pairs_model 另行读取
  pairs: UsagePair[]
  options: Record<UsageDimension, string[]>
}

export interface RequestAttempt {
  account: string
  channel: string
  error: string
  duration_ms: number
  // 这次尝试所用的模型：只在额度用完改用了其他模型（MODEL_FALLBACKS）的请求里有，源模型与目标模型的尝试各记各的
  model?: string
  // extra 为这次尝试的租约借自补充池；其余尝试不出现
  extra?: boolean
}

export interface UsageRecord {
  id: string
  time: string
  protocol: string
  path: string
  model: string
  account: string
  channel: string
  status: number
  state: string
  duration_ms: number
  first_event_ms: number
  queue_ms: number
  input_tokens: number
  reasoning_tokens: number
  reply_tokens: number
  total_tokens: number
  tool_calls: number
  error: string
  attempts: RequestAttempt[]
  served_model?: string
  downgrade?: string
  reply_hash?: string
  duplicate?: boolean
  has_body: boolean
  // pool 为请求的号池：经 /ultra 进入的请求为 ultra，经 /pro 进入的为 pro，其余为 normal
  pool: AccountPool
  // extra 为由补充池服务（与 pool 相互独立，pool 仍按入口）；其余记录不出现
  extra?: boolean
}

export interface UsageRecordPage {
  items: UsageRecord[]
  next_cursor?: string
}

export interface RequestBody {
  id: string
  time: string
  request: string
  request_size: number
  response: string
  response_size: number
}

// StudioKeyRecord 为一个账户的 AI Studio API Key 记录（列表只有末四位，没有完整 Key）
export interface StudioKeyRecord {
  account_id: string
  label?: string
  // status：ok 读到 Key；none 页面上没有 Key 也不能创建；failed 失败；skipped 账户未登录、停用等被跳过；
  // invalid 保存着的 Key 与其他账户相同、已作废（has_key 为假，没有末四位与项目，reason 写明原因）
  status: 'ok' | 'none' | 'failed' | 'skipped' | 'invalid' | string
  reason?: string
  has_key: boolean
  last4?: string
  project_name?: string
  project_number?: string
  project_display?: string
  // project_id：项目 ID（如 gen-lang-client-0123456789）
  project_id?: string
  created?: boolean
  // project_created：Key 所在的项目是获取时经页面新建的（账户原先没有 Google Cloud 项目）
  project_created?: boolean
  key_count?: number
  fetched_at?: string
  created_at?: string
  attempt_at?: string
}

// StudioKeyStatus 为获取任务状态与全部记录
export interface StudioKeyStatus {
  delay_seconds: number
  running: boolean
  current?: string
  // current_fill：正在处理的账户是补项目信息（只读，不创建 Key 与项目）
  current_fill?: boolean
  queue: string[]
  next_at?: string
  records: StudioKeyRecord[]
}

// StudioKeySecret 为完整 Key（只由复制按钮读取）
// StudioKeyExport 为全部保存着的完整 Key（“复制全部 Key”）
export interface StudioKeyExport {
  keys: StudioKeySecret[]
}

export interface StudioKeySecret {
  account_id: string
  key: string
  project_name?: string
  project_number?: string
  project_display?: string
  project_id?: string
}

// StudioKeyClearCounts 为清除记录的计数：deleted 删掉的没有 Key 的记录；invalid 删掉的作废 Key 的记录（与其他账户相同，不是账户的 Key）；
// reset 清掉失败信息的保存着 Key 的记录（Key 不动）；busy 正在处理、没有动的账户；unknown 不认识的账户；unchanged 不用清的；
// errors 读不出来、没有动的
export interface StudioKeyClearCounts {
  deleted: number
  invalid: number
  reset: number
  busy: number
  unknown: number
  unchanged: number
  errors: number
}

// StudioKeyClearResult 为清除之后的状态与计数
export interface StudioKeyClearResult extends StudioKeyStatus {
  cleared: StudioKeyClearCounts
}
