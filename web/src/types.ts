export type Locale = 'zh-CN' | 'zh-TW' | 'en' | 'ja' | 'ko' | 'fr' | 'de'

export type TabID =
  | 'logs'
  | 'accounts'
  | 'models'
  | 'requests'
  | 'usage'
  | 'settings'
  | 'playground'

// org_disabled 为账号所属组织停用了 AI Studio（不参与任何调度，认证文件更新、管理员清除或长间隔复查通过后恢复）
export type AccountState =
  'ready' | 'busy' | 'cooldown' | 'auth_required' | 'unavailable' | 'disabled' | 'org_disabled'

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
  // 开启 ULTRA_INCLUDE_PRO 时权益为 Pro 的账户也属于 ultra；其余属于 normal
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
  // org_disabled 表示账户所属组织停用了 AI Studio（不区分是否启用）：启用账户的 state 为 org_disabled，停用账户仍为 disabled、
  // message 接上原因；验证接口的返回在验证遇到组织停用时同样为 true
  org_disabled?: boolean
}

// AccountPool 为账户所属的号池；经 /ultra 前缀的请求只用 ultra 号池的账户
export type AccountPool = 'ultra' | 'normal'

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
}

export interface ChromeImportInput extends AccountLoginInput {
  account_ids: string[]
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

// PrewarmState 为预热循环的实时状态；loop_age_seconds 持续变大说明循环被卡住
export interface PrewarmState {
  active: boolean
  inflight: number
  launched: number
  round_seconds: number
  loop_age_seconds: number
  reason?: string
}

// WorkerCounters 为 WAA Worker 的实时数量；warm_ids、starting_ids 为对应账户；
// dead 为 warm 中账号长时间冷却（如每日限额）、占着名额却服务不了请求、等待淘汰的死槽位；
// draining 为其中排空中的 Worker（不再接新请求，正在服务的请求结束后关闭，替补已先启动），retiring 为待淘汰登记数；
// unschedulable 为驻留 Worker 中账号已不能服务任何模型的（需要登录、已停用、没有模型目录），等待释放；旧版服务端不返回
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
}

// UltraWorkerCounters 为 Ultra 分区的 Worker 数量与上限；occupied 为占用的槽位（含正在关闭的），
// dead 为其中账号长时间冷却、等待淘汰的死槽位，draining 为其中排空中的，retiring 为待淘汰登记数，
// unschedulable 为其中账号已不能服务任何模型、等待释放的，warm_limit 与 max 为 Ultra 常驻数与峰值数设置
export interface UltraWorkerCounters {
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
// pool 为 "ultra" 时是 Ultra 主用模型（ULTRA_PRIMARY_MODELS），只统计 Ultra 分区的 Worker 与 Ultra 号池的账号；
// 没有该字段时是普通主用模型（PRIMARY_MODELS），只统计普通分区。旧版服务端不返回 pool
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

export interface ServiceStatus {
  state: 'STOPPED' | 'LAUNCHING' | 'RUNNING'
  running: boolean
  ready: boolean
  version: string
  active_requests: number
  accounts: AccountCounters
  // workers 的数量与上限只含普通分区，warm_ids、starting_ids 包含两个分区
  workers?: WorkerCounters
  // ultra_accounts 为 Ultra 号池的账户计数（开启 ULTRA_INCLUDE_PRO 时含并入的 Pro 账户），ultra_workers 为 Ultra 分区的 Worker 计数
  ultra_accounts?: AccountCounters
  ultra_workers?: UltraWorkerCounters
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
  // path 保留 /ultra 前缀；pool 为 ultra 表示请求经 /ultra 进入，只用 Ultra 号池的账户
  path?: string
  pool?: 'ultra'
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
  usage?: {
    input_tokens: number
    reasoning_tokens: number
    reply_tokens: number
    output_tokens: number
    total_tokens: number
    average_tokens_per_second: number
  }
  downgrade?: DowngradeDecision
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
}

// PermissionDeniedModel 为账户在一个模型上的 403 无权限记录；channels 为返回过 403、之后还没有成功过的通道
export interface PermissionDeniedModel {
  model: string
  channels: UpstreamChannel[]
  count: number
  first_at: string
  last_at: string
  message?: string
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
// message 为最近一次的原文（服务端已打码、截短）
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
  // pool 为 ultra 表示请求经 /ultra 进入
  pool?: 'ultra'
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
  // warm_worker_limit 与 max_active_workers 只约束普通分区的 Worker
  warm_worker_limit: number
  max_active_workers: number
  // ultra_exclusive 为真时 Ultra 号池的账户只服务 /ultra 请求；ultra_warm_worker_limit 可以为 0（只按需启动），
  // ultra_max_active_workers 至少为 1 且不小于 Ultra 常驻数
  ultra_exclusive: boolean
  // ultra_include_pro 为真时权益为 Pro 的账户并入 Ultra 号池，按 Ultra 号池的全部设置调度，账户的真实权益不变。
  // 服务端总是返回该字段，因此这里不是可选字段：缺了它，设置页的快照对比会一直显示未保存
  ultra_include_pro: boolean
  ultra_warm_worker_limit: number
  ultra_max_active_workers: number
  warm_startup_concurrency: number
  per_account_concurrency: number
  // account_rpm_limit 为单账号最近 60 秒最多发出的生成请求数，0 表示关闭。服务端总是返回该字段，因此这里不是可选字段
  account_rpm_limit: number
  routing_strategy: 'round-robin' | 'fill-first'
  upstream_channels: UpstreamChannel[]
  // stream_playground_models 为流式请求优先走 Playground 的模型；旧版服务端不返回该字段
  stream_playground_models?: string[]
  // primary_models 为主用模型，primary_queue_wait 为它们等空闲账号或 Worker 的上限（0s 表示不排队）；旧版服务端不返回这两个字段。
  // 这份列表只作用于普通号池
  primary_models?: string[]
  primary_queue_wait?: string
  // ultra_primary_models 为 Ultra 主用模型（空数组表示关闭），只作用于 /ultra 请求（以及 ULTRA_EXCLUSIVE 关闭时普通路径上
  // 只有 Ultra 号池的账户能服务的模型），这些模型没有额度时立即返回 429；ultra_primary_queue_wait 为它们的排队上限，
  // 规则与 primary_queue_wait 相同。旧版服务端不返回这两个字段
  ultra_primary_models?: string[]
  ultra_primary_queue_wait?: string
  waa_backend: 'camoufox' | 'go'
  temporary_chat: boolean
  ignore_client_seed: boolean
  repeat_prompt_nonce: boolean
  min_output_tokens: number
  downgrade_guard: DowngradeGuardConfig
  // proxy_pool 为 IP 池设置；旧版服务端不返回，此时设置页不显示 IP 池分组，保存时也不提交（服务端沿用现值）
  proxy_pool?: ProxyPoolConfig
  // media_output 为生成媒体的输出方式：inline 内联 data URI（Gemini 原生接口为 inlineData），url 保存到 runtime/media/ 并以本服务
  // /media/<文件名> 链接引用；media_base_url 为链接使用的服务地址（为空时按请求地址）；media_retention 为保存期限（7d、72h，0 不清理），
  // media_max_size 为媒体目录总量上限（20GB、500MB，0 不限）。服务端总是返回这几个字段，保存后立即生效
  media_output: 'inline' | 'url'
  media_base_url: string
  media_retention: string
  media_max_size: string
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

export type PlaygroundReasoning = '' | 'low' | 'medium' | 'high'

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
  // ultra 为真时请求经 /ultra 前缀发送，只由 Ultra 号池的账户处理
  ultra: boolean
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

// UsageDimension 为用量可筛选与分组的维度；pool 为号池，取值为 normal 或 ultra
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
  pairs: UsagePair[]
  options: Record<UsageDimension, string[]>
}

export interface RequestAttempt {
  account: string
  channel: string
  error: string
  duration_ms: number
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
  // pool 为请求的号池：经 /ultra 进入的请求为 ultra，其余为 normal
  pool: 'normal' | 'ultra'
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
