export type Locale = 'zh-CN' | 'zh-TW' | 'en' | 'ja' | 'ko' | 'fr' | 'de'

export type TabID =
  | 'logs'
  | 'accounts'
  | 'models'
  | 'requests'
  | 'usage'
  | 'settings'
  | 'playground'

export type AccountState =
  'ready' | 'busy' | 'cooldown' | 'auth_required' | 'unavailable' | 'disabled'

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
// draining 为其中排空中的 Worker（不再接新请求，正在服务的请求结束后关闭，替补已先启动），retiring 为待淘汰登记数
export interface WorkerCounters {
  warm: number
  starting: number
  target: number
  max: number
  occupied?: number
  dead?: number
  draining?: number
  retiring?: number
  prewarm?: PrewarmState
  warm_ids?: string[]
  starting_ids?: string[]
}

// UltraWorkerCounters 为 Ultra 分区的 Worker 数量与上限；occupied 为占用的槽位（含正在关闭的），
// dead 为其中账号长时间冷却、等待淘汰的死槽位，draining 为其中排空中的，retiring 为待淘汰登记数，
// warm_limit 与 max 为 Ultra 常驻数与峰值数设置
export interface UltraWorkerCounters {
  warm: number
  starting: number
  occupied: number
  dead?: number
  draining?: number
  retiring?: number
  target: number
  warm_limit: number
  max: number
}

// PrimarySupply 为一个主用模型的可用供给（约每 30 秒统计一次）：warm 为能服务它的常驻 Worker，
// standby 为还能服务它的空闲备用账号，最多统计 1000 个，达到上限时 standby_capped 为 true。
// pool 为 "ultra" 时是 Ultra 主用模型（ULTRA_PRIMARY_MODELS），只统计 Ultra 分区的 Worker 与 Ultra 号池的账号；
// 没有该字段时是普通主用模型（PRIMARY_MODELS），只统计普通分区。旧版服务端不返回 pool
export interface PrimarySupply {
  model: string
  pool?: string
  warm: number
  standby: number
  standby_capped?: boolean
}

// StartupStatus 为 Worker 冷启动的实时状态（普通与 Ultra 号池共用冷启动名额）：launching 为正在启动浏览器的数量
// （占用的冷启动名额），limit 为同时启动的上限（WARM_STARTUP_CONCURRENCY 至少按 1 计，另加 1 个请求现场名额），
// queued_on_demand 与 queued_background 为在名额队列里等待的按需启动（请求现场）与后台启动（预热、扩容、替补）数；
// paused 为启动失败暂停中的账户数，dead 为确定失效的账户数（含确认为页面受阻的），last_success 为最近一次 Worker 启动成功的时间
// （还没有成功过时为空），env_failures_10m 为近 10 分钟环境类启动失败数（浏览器没能启动或连接，多半是机器资源不够，不计入账户）；
// page_blocked_burst 为真时大量账户同时页面受阻（可能是官网页面改版），暂不把新的页面受阻确认为失效，旧版服务端不返回
export interface StartupStatus {
  launching: number
  limit: number
  queued_on_demand: number
  queued_background: number
  paused: number
  dead: number
  last_success?: string
  env_failures_10m: number
  page_blocked_burst?: boolean
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
  // startup 为冷启动名额与启动失败的统计；旧版服务端不返回
  startup?: StartupStatus
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

// StartupFailureKind 为启动 Worker 失败的类别：auth 为登录失效（启动时被跳转到 Google 登录页，或账户处于需要重新登录），
// files 为认证文件无法用于启动浏览器，fingerprint 为账户 Camoufox 指纹文件损坏（与登录无关，重新上传认证文件修不好），
// page_blocked 为页面受阻（页面打开后填不进 bootstrap 提示词或 Run 按钮一直不可用，多半是官网有需要账户处理的提示），
// ambiguous 为启动反复失败、原因不确定（页面未就绪、WAA 失败、超时，机器负载高或网络慢时健康账户也会这样）
export type StartupFailureKind = 'auth' | 'files' | 'fingerprint' | 'page_blocked' | 'ambiguous'

// StartupFailureAccount 为启动 Worker 失败的账户。confirmed 为 true 时确定失效（kind 为 auth、files，或号池健康时连续受阻确认的
// page_blocked：账户状态仍是就绪，按 30 分钟暂停重试，可能带 until），可以删除；为 false 时只是暂停调度到 until（kind 为 fingerprint、
// page_blocked 或 ambiguous），不改变账户状态，不能据此删除。stage 为失败时的启动阶段 1–7
// （未知为 0），url 为登录跳转的地址（只用于展示），reason 为原因摘要，message 为错误原文（截断）
export interface StartupFailureAccount {
  id: string
  label: string
  kind: StartupFailureKind
  confirmed: boolean
  reason?: string
  message?: string
  stage?: number
  url?: string
  count: number
  first_at?: string
  last_at?: string
  until?: string
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

export interface PlaygroundResult {
  text: string
  reasoning: string
  tools: string
  media: PlaygroundMedia[]
  raw: string
  durationMs: number
  status: number
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
