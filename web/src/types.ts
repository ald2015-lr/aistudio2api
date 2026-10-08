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
  benefit_tier: string
  message: string
  // benefit_tier_known 为 false 表示权益尚未从官网读取：benefit_tier 按 Free 显示，账户属于普通号池
  benefit_tier_known: boolean
  // pool 为账户当前所属的号池：权益为 Ultra 的账户属于 ultra，其余属于 normal
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

// WorkerCounters 为 WAA Worker 的实时数量；warm_ids、starting_ids 为对应账户
export interface WorkerCounters {
  warm: number
  starting: number
  target: number
  max: number
  occupied?: number
  prewarm?: PrewarmState
  warm_ids?: string[]
  starting_ids?: string[]
}

// UltraWorkerCounters 为 Ultra 分区的 Worker 数量与上限；occupied 为占用的槽位（含正在关闭的），
// warm_limit 与 max 为 Ultra 常驻数与峰值数设置
export interface UltraWorkerCounters {
  warm: number
  starting: number
  occupied: number
  target: number
  warm_limit: number
  max: number
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
  // ultra_accounts 为 Ultra 号池的账户计数，ultra_workers 为 Ultra 分区的 Worker 计数
  ultra_accounts?: AccountCounters
  ultra_workers?: UltraWorkerCounters
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
  // ultra_exclusive 为真时 Ultra 账户只服务 /ultra 请求；ultra_warm_worker_limit 可以为 0（只按需启动），
  // ultra_max_active_workers 至少为 1 且不小于 Ultra 常驻数
  ultra_exclusive: boolean
  ultra_warm_worker_limit: number
  ultra_max_active_workers: number
  warm_startup_concurrency: number
  per_account_concurrency: number
  routing_strategy: 'round-robin' | 'fill-first'
  upstream_channels: UpstreamChannel[]
  // stream_playground_models 为流式请求优先走 Playground 的模型；旧版服务端不返回该字段
  stream_playground_models?: string[]
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
