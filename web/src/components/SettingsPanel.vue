<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { ApiError, api } from '@/api'
import { channelLabelKey, useI18n, type TranslationKey } from '@/i18n'
import {
  DEFAULT_PROXY_POOL_PORT_END,
  DEFAULT_PROXY_POOL_PORT_START,
  cloneProxyPool,
  formatClock,
  proxyPoolProblem,
  proxyPoolProblemKey,
} from '@/proxyPool'
import type {
  BuildChannelStatus,
  DowngradeGuardConfig,
  Model,
  ProxyPoolConfig,
  ProxyPoolStatus,
  ServiceConfig,
  UpstreamChannel,
} from '@/types'
import ProxyInput from './ProxyInput.vue'
import ProxyPoolSettings from './ProxyPoolSettings.vue'
import UiIcon from './UiIcon.vue'
import UiSelect from './UiSelect.vue'
import { readStorage, writeStorage } from '@/storage'

const props = defineProps<{
  config: ServiceConfig | null
  loading: boolean
  error: string
  running: boolean
  models: Model[]
  // proxyPoolStatus 为状态接口的 IP 池实时状态（没有开启或旧版服务端时为 null）
  proxyPoolStatus: ProxyPoolStatus | null
  // buildChannelStatus 为状态接口的 Build 通道生成暂停状态（没有启用 Build、服务未运行或旧版服务端时为 null）
  buildChannelStatus?: BuildChannelStatus | null
}>()

const emit = defineEmits<{
  saved: [config: ServiceConfig]
  notice: [message: string, tone: 'success' | 'error']
}>()

type EditableKey =
  | 'auth_states'
  | 'listen_addr'
  | 'proxy_api_key'
  | 'proxy'
  | 'init_timeout'
  | 'request_timeout'
  | 'first_event_timeout'
  | 'warm_worker_limit'
  | 'max_active_workers'
  | 'ultra_exclusive'
  | 'ultra_include_pro'
  | 'ultra_warm_worker_limit'
  | 'ultra_max_active_workers'
  | 'warm_startup_concurrency'
  | 'per_account_concurrency'
  | 'account_rpm_limit'
  | 'routing_strategy'
  | 'upstream_channels'
  | 'stream_playground_models'
  | 'primary_models'
  | 'primary_queue_wait'
  | 'ultra_primary_models'
  | 'ultra_primary_queue_wait'
  | 'waa_backend'
  | 'temporary_chat'
  | 'ignore_client_seed'
  | 'repeat_prompt_nonce'
  | 'min_output_tokens'
  | 'downgrade_guard'
  | 'proxy_pool'
  | 'media_output'
  | 'media_base_url'
  | 'media_retention'
  | 'media_max_size'

const EDITABLE_KEYS: readonly EditableKey[] = [
  'auth_states',
  'listen_addr',
  'proxy_api_key',
  'proxy',
  'init_timeout',
  'request_timeout',
  'first_event_timeout',
  'warm_worker_limit',
  'max_active_workers',
  'ultra_exclusive',
  'ultra_include_pro',
  'ultra_warm_worker_limit',
  'ultra_max_active_workers',
  'warm_startup_concurrency',
  'per_account_concurrency',
  'account_rpm_limit',
  'routing_strategy',
  'upstream_channels',
  'stream_playground_models',
  'primary_models',
  'primary_queue_wait',
  'ultra_primary_models',
  'ultra_primary_queue_wait',
  'waa_backend',
  'temporary_chat',
  'ignore_client_seed',
  'repeat_prompt_nonce',
  'min_output_tokens',
  'downgrade_guard',
  'proxy_pool',
  'media_output',
  'media_base_url',
  'media_retention',
  'media_max_size',
]
// 这几个字段边输入边保存会出问题（例如密钥只输入了一半），改为输入框失去焦点时保存；
// IP 池的模式、地址与端口段要组合合法才能保存（例如先选静态 IP、再填地址），同样只在失焦或选完模式时提交；
// 媒体链接地址、保存期限与总量上限输入到一半时不合法（例如期限只写了 7、地址只写了 https://），同样失焦时保存
const BLUR_KEYS: readonly EditableKey[] = [
  'auth_states',
  'listen_addr',
  'proxy_api_key',
  'proxy',
  'proxy_pool',
  'media_base_url',
  'media_retention',
  'media_max_size',
]
const AUTO_KEYS: readonly EditableKey[] = EDITABLE_KEYS.filter((key) => !BLUR_KEYS.includes(key))

// defaultDowngradeGuard 与服务端默认值一致
function defaultDowngradeGuard(): DowngradeGuardConfig {
  return {
    enabled: true,
    models: ['gemini-3.1-pro-preview'],
    speed_threshold: 190,
    min_tokens: 150,
    min_window_ms: 2500,
    fuzzy_low: 160,
    fuzzy_high: 220,
    count_timeout_ms: 1500,
    fast_mode: false,
    memory_minutes: 30,
    max_hold_ms: 0,
    reject_status: 400,
  }
}

// cloneDowngradeGuard 深拷贝降级判定设置：表单与已保存的配置不能共用同一个对象，否则修改不会被识别为未保存
function cloneDowngradeGuard(value: DowngradeGuardConfig | undefined): DowngradeGuardConfig {
  const source = value ?? defaultDowngradeGuard()
  // 旧版服务端不返回 reject_status，按默认 400 处理
  return { ...source, models: [...(source.models ?? [])], reject_status: source.reject_status ?? 400 }
}

// DEFAULT_ULTRA_PRIMARY_MODELS 与服务端 ULTRA_PRIMARY_MODELS 的默认值一致；旧版服务端不返回该字段时按它显示
const DEFAULT_ULTRA_PRIMARY_MODELS: readonly string[] = ['gemini-4-argon']

// parseModelList 与服务端一致：逗号或空白分隔，去掉 models/ 前缀并去重
function parseModelList(value: string): string[] {
  const models: string[] = []
  for (const item of value.split(/[\s,，]+/)) {
    const name = item.trim().replace(/^models\//, '')
    if (name !== '' && !models.includes(name)) models.push(name)
  }
  return models
}
const SAVE_DELAY_MS = 800
// DEFAULT_API_KEY 与服务端 config.DefaultProxyAPIKey 一致：留空保存时服务端使用该默认密钥
const DEFAULT_API_KEY = 'sk-onechat-fun-fun'
const APPLY_DELAY_SECONDS = 5
const AUTO_APPLY_KEY = 'aistudio2api_settings_auto_apply'
const DURATION_PATTERN = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/
// MEDIA_* 的下限与服务端一致：保存期限不为 0 时至少 1 小时，总量上限不为 0 时至少 100MB
const MIN_MEDIA_RETENTION_MS = 3_600_000
const MIN_MEDIA_MAX_BYTES = 100 * 1024 * 1024
const MEDIA_SIZE_UNITS: Record<string, number> = {
  '': 1,
  b: 1,
  k: 1024,
  kb: 1024,
  kib: 1024,
  m: 1024 ** 2,
  mb: 1024 ** 2,
  mib: 1024 ** 2,
  g: 1024 ** 3,
  gb: 1024 ** 3,
  gib: 1024 ** 3,
  t: 1024 ** 4,
  tb: 1024 ** 4,
  tib: 1024 ** 4,
}

const { t, tf, locale } = useI18n()
const revealKey = ref(false)
const saving = ref(false)
const saveError = ref('')
const applying = ref(false)
const countdown = ref(0)
const autoApply = ref(readStorage(AUTO_APPLY_KEY) !== 'false')
const savedConfig = ref<ServiceConfig | null>(null)
const form = reactive<ServiceConfig>({
  auth_states: 'auth',
  listen_addr: '127.0.0.1:2048',
  proxy_api_key: '',
  active_listen_addr: '127.0.0.1:2048',
  active_proxy_api_key: '',
  management_restart_required: false,
  service_restart_required: false,
  proxy: '',
  init_timeout: '2m',
  request_timeout: '5m',
  first_event_timeout: '0s',
  warm_worker_limit: 5,
  max_active_workers: 10,
  ultra_exclusive: true,
  ultra_include_pro: false,
  ultra_warm_worker_limit: 2,
  ultra_max_active_workers: 5,
  warm_startup_concurrency: 2,
  per_account_concurrency: 2,
  account_rpm_limit: 10,
  routing_strategy: 'round-robin',
  upstream_channels: ['playground', 'build'],
  stream_playground_models: [],
  primary_models: [],
  primary_queue_wait: '10s',
  ultra_primary_models: [...DEFAULT_ULTRA_PRIMARY_MODELS],
  ultra_primary_queue_wait: '10s',
  waa_backend: 'camoufox',
  temporary_chat: false,
  ignore_client_seed: false,
  repeat_prompt_nonce: true,
  min_output_tokens: 60000,
  downgrade_guard: defaultDowngradeGuard(),
  media_output: 'inline',
  media_base_url: '',
  media_retention: '7d',
  media_max_size: '20GB',
})

const upstreamChannelOptions: UpstreamChannel[] = ['playground', 'build']
// 拦截模型列表的输入框单独保存文字，避免输入逗号时被立即重新格式化
const guardModelsText = ref(form.downgrade_guard.models.join(', '))

function onGuardModelsInput(event: Event): void {
  const value = (event.target as HTMLInputElement).value
  guardModelsText.value = value
  form.downgrade_guard.models = parseModelList(value)
}

// 流式优先 Playground 的模型输入框同样单独保存文字
const streamModelsText = ref('')

function onStreamModelsInput(event: Event): void {
  const value = (event.target as HTMLInputElement).value
  streamModelsText.value = value
  form.stream_playground_models = parseModelList(value)
}

// 主用模型的输入框同样单独保存文字
const primaryModelsText = ref('')

function onPrimaryModelsInput(event: Event): void {
  const value = (event.target as HTMLInputElement).value
  primaryModelsText.value = value
  form.primary_models = parseModelList(value)
}

// Ultra 主用模型的输入框同样单独保存文字；清空即关闭 Ultra 主用模型
const ultraPrimaryModelsText = ref((form.ultra_primary_models ?? []).join(', '))

function onUltraPrimaryModelsInput(event: Event): void {
  const value = (event.target as HTMLInputElement).value
  ultraPrimaryModelsText.value = value
  form.ultra_primary_models = parseModelList(value)
}

// subscriptionModels 为目录中需要订阅权益、可经 Build 调用（在 Build 通道一次性返回整段回复）且可经 Playground 流式调用的模型，
// 提示可以加入列表；只有 Build 能调用的模型加入列表也不会改走 Playground，不列出
const subscriptionModels = computed(() =>
  parseModelList(
    props.models
      .filter((model) => {
        const channels = model.channels ?? []
        return (
          model.build_unary === true &&
          channels.includes('build') &&
          channels.includes('playground')
        )
      })
      .map((model) => model.id)
      .join(','),
  ),
)

// 服务端匹配列表时不分大小写，这里同样不分大小写，避免加入只差大小写的重复项
function streamModelListed(model: string): boolean {
  const name = model.toLowerCase()
  return (form.stream_playground_models ?? []).some((item) => item.toLowerCase() === name)
}

function addStreamModel(model: string): void {
  if (streamModelListed(model)) return
  form.stream_playground_models = [...(form.stream_playground_models ?? []), model]
  streamModelsText.value = form.stream_playground_models.join(', ')
}
let saveTimer: number | undefined
let countdownTimer: number | undefined
// proxyTesting 为“测试代理”进行中：期间不开始自动应用的倒计时，免得没测完的代理先重启生效（键盘 Tab 到测试按钮时
// 输入框先失焦保存）；applyDeferred 记下期间被推迟的倒计时，测试通过后再开始，没通过就留给用户点“立即应用”
const proxyTesting = ref(false)
let applyDeferred = false
let saveChain: Promise<unknown> = Promise.resolve()

// snapshot 序列化可编辑字段，用于判断是否有未保存的修改；IP 池设置按固定的字段顺序序列化，不受服务端返回顺序影响
function snapshot(source: ServiceConfig, keys: readonly EditableKey[] = EDITABLE_KEYS): string {
  return JSON.stringify(
    keys.map((key) =>
      key === 'proxy_pool' && source.proxy_pool !== undefined
        ? cloneProxyPool(source.proxy_pool)
        : source[key],
    ),
  )
}

const dirty = computed(
  () => savedConfig.value !== null && snapshot(form) !== snapshot(savedConfig.value),
)

const statusText = computed(() => {
  if (saving.value) return t('settings.stateSaving')
  if (saveError.value !== '') return tf('settings.stateError', { message: saveError.value })
  if (dirty.value) return t('settings.stateDirty')
  if (props.config?.service_restart_required) return t('settings.stateSavedPending')
  return t('settings.stateSaved')
})

const statusClass = computed(() => {
  if (saveError.value !== '') return 'text-red-400'
  if (saving.value || dirty.value || props.config?.service_restart_required) return 'text-yellow-400'
  return 'text-green-400'
})

// buildChannelText 为“上游通道”旁的 Build 通道状态：全局暂停时写开始与下次试探时间，只有部分账号暂停时写账号数（另有暂停到期、
// 等待试探的账号时一并写出），只有等待试探的账号时写等待试探，正常时写正常；没有启用 Build 或服务未运行时不显示
const buildChannelText = computed(() => {
  const status = props.buildChannelStatus
  if (!status) return ''
  if (status.paused) {
    const text = tf('settings.buildChannelPaused', {
      since: formatClock(status.since, locale.value) || '-',
      next: formatClock(status.next_probe, locale.value) || '-',
    })
    return status.probing ? text + t('settings.buildChannelProbing') : text
  }
  const awaiting = status.awaiting_probe ?? 0
  if (status.paused_accounts > 0) {
    const text = tf('settings.buildChannelAccountsPaused', { count: status.paused_accounts })
    return awaiting > 0 ? text + tf('settings.buildChannelAwaitingMore', { count: awaiting }) : text
  }
  if (awaiting > 0) {
    return tf('settings.buildChannelAwaiting', { count: awaiting })
  }
  return t('settings.buildChannelNormal')
})

// buildChannelWarn 为 Build 通道有暂停（全局、按账号或等待试探）时用提示色
const buildChannelWarn = computed(() => {
  const status = props.buildChannelStatus
  return (
    !!status && (status.paused || status.paused_accounts > 0 || (status.awaiting_probe ?? 0) > 0)
  )
})

// toggleUpstreamChannel 切换通道并保持配置顺序，至少保留一个通道
function toggleUpstreamChannel(channel: UpstreamChannel, event: Event): void {
  const target = event.target
  const enabled = target instanceof HTMLInputElement && target.checked
  const selected = new Set(form.upstream_channels)
  if (enabled) selected.add(channel)
  else if (selected.size > 1) selected.delete(channel)
  form.upstream_channels = upstreamChannelOptions.filter((option) => selected.has(option))
}

function isPositiveInteger(value: unknown): boolean {
  return typeof value === 'number' && Number.isInteger(value) && value > 0
}

function isDuration(value: string): boolean {
  return DURATION_PATTERN.test(value) && /[1-9]/.test(value)
}

const DURATION_UNITS: Record<string, number> = {
  ns: 1e-6,
  us: 1e-3,
  µs: 1e-3,
  ms: 1,
  s: 1000,
  m: 60_000,
  h: 3_600_000,
}

// durationMillis 把 Go 时长写法（如 3m、1m30s、90s）换算为毫秒；无法解析时返回 NaN。单独的 0 与 Go 一致按 0 处理
function durationMillis(value: string): number {
  if (value.trim() === '0') return 0
  const parts = value.trim().matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)
  let total = 0
  let matched = ''
  for (const [text, amount, unit] of parts) {
    total += Number(amount) * DURATION_UNITS[unit!]!
    matched += text
  }
  return matched !== '' && matched === value.trim() ? total : Number.NaN
}

// mediaRetentionMillis 与服务端 ParseMediaRetention 一致：Go 时长写法或整数天数加 d（7d），单独的 0 表示不清理；无法解析时返回 NaN
function mediaRetentionMillis(value: string): number {
  const text = value.trim().toLowerCase()
  const days = /^(\d+)d$/.exec(text)
  if (days !== null) return Number(days[1]) * 86_400_000
  return durationMillis(text)
}

// mediaSizeBytes 与服务端 ParseMediaSize 一致：整数加单位（B、KB、MB、GB、TB，按 1024 进位），不写单位按字节；无法解析时返回 NaN
function mediaSizeBytes(value: string): number {
  const match = /^(\d+)\s*([a-z]*)$/.exec(value.trim().toLowerCase())
  if (match === null) return Number.NaN
  const unit = MEDIA_SIZE_UNITS[match[2]!]
  return unit === undefined ? Number.NaN : Number(match[1]) * unit
}

// isMediaBaseURL 与服务端一致：空值，或不带账号密码、查询参数与片段的 http/https 地址（可带路径前缀）
function isMediaBaseURL(value: string): boolean {
  const text = value.trim()
  if (text === '') return true
  if (/[\s()<>"'`\\?#]/.test(text)) return false
  try {
    const parsed = new URL(text)
    return (
      (parsed.protocol === 'http:' || parsed.protocol === 'https:') &&
      parsed.host !== '' &&
      parsed.username === '' &&
      parsed.password === ''
    )
  } catch {
    return false
  }
}

function isListenAddress(value: string): boolean {
  const match = /^(\[[^\]]+\]|[^:]*):(\d{1,5})$/.exec(value)
  if (match === null) return false
  const port = Number(match[2])
  return port >= 1 && port <= 65535
}

// validate 与服务端校验规则一致，返回第一条错误；通过时返回空字符串
function validate(value: ServiceConfig): string {
  const label = (key: TranslationKey): string => t(key)
  if (value.auth_states.trim() === '') {
    return tf('settings.invalidRequired', { field: label('settings.authPath') })
  }
  if (!isListenAddress(value.listen_addr)) return t('settings.invalidListen')
  const durations: [string, TranslationKey][] = [
    [value.init_timeout, 'settings.initTimeout'],
    [value.request_timeout, 'settings.requestTimeout'],
  ]
  for (const [duration, key] of durations) {
    if (!isDuration(duration)) return tf('settings.invalidDuration', { field: label(key) })
  }
  // 首事件超时可以为 0（关闭），开启时必须小于请求超时
  const firstEvent = durationMillis(value.first_event_timeout)
  if (
    Number.isNaN(firstEvent) ||
    (firstEvent > 0 && firstEvent >= durationMillis(value.request_timeout))
  ) {
    return t('settings.invalidFirstEventTimeout')
  }
  // 主用模型的排队上限可以为 0（不排队），不能超过请求超时
  const primaryWait = durationMillis(value.primary_queue_wait ?? '0s')
  if (Number.isNaN(primaryWait) || primaryWait > durationMillis(value.request_timeout)) {
    return t('settings.invalidPrimaryQueueWait')
  }
  // Ultra 主用模型的排队上限规则相同
  const ultraPrimaryWait = durationMillis(value.ultra_primary_queue_wait ?? '0s')
  if (Number.isNaN(ultraPrimaryWait) || ultraPrimaryWait > durationMillis(value.request_timeout)) {
    return t('settings.invalidUltraPrimaryQueueWait')
  }
  const numbers: [unknown, TranslationKey][] = [
    [value.warm_worker_limit, 'settings.warmWorkerLimit'],
    [value.max_active_workers, 'settings.maxActiveWorkers'],
    [value.warm_startup_concurrency, 'settings.warmStartupConcurrency'],
    [value.per_account_concurrency, 'settings.perAccountConcurrency'],
  ]
  for (const [number, key] of numbers) {
    if (!isPositiveInteger(number)) return tf('settings.invalidNumber', { field: label(key) })
  }
  // 单账号每分钟上限可以为 0（关闭），最大 1000
  const rpm = value.account_rpm_limit
  if (typeof rpm !== 'number' || !Number.isInteger(rpm) || rpm < 0 || rpm > 1000) {
    return t('settings.invalidAccountRpmLimit')
  }
  const floor = value.min_output_tokens
  if (typeof floor !== 'number' || !Number.isInteger(floor) || floor < 0) {
    return tf('settings.invalidNumber', { field: label('settings.minOutputTokens') })
  }
  const guard = value.downgrade_guard
  if (guard !== undefined && guard.enabled) {
    if (guard.models.length === 0) return t('settings.invalidDowngradeModels')
    const positives: [unknown, TranslationKey][] = [
      [guard.speed_threshold, 'settings.downgradeSpeed'],
      [guard.fuzzy_low, 'settings.downgradeFuzzyLow'],
      [guard.fuzzy_high, 'settings.downgradeFuzzyHigh'],
    ]
    for (const [number, key] of positives) {
      if (typeof number !== 'number' || !Number.isFinite(number) || number <= 0) {
        return tf('settings.invalidNumber', { field: label(key) })
      }
    }
    if (!isPositiveInteger(guard.min_tokens)) {
      return tf('settings.invalidNumber', { field: label('settings.downgradeMinTokens') })
    }
    const bounded: [unknown, TranslationKey, number][] = [
      [guard.min_window_ms, 'settings.downgradeMinWindow', 60000],
      [guard.count_timeout_ms, 'settings.downgradeCountTimeout', 30000],
      [guard.memory_minutes, 'settings.downgradeMemory', Number.MAX_SAFE_INTEGER],
      [guard.max_hold_ms, 'settings.downgradeMaxHold', 600000],
    ]
    for (const [number, key, max] of bounded) {
      if (typeof number !== 'number' || !Number.isInteger(number) || number < 0 || number > max) {
        return tf('settings.invalidNumber', { field: label(key) })
      }
    }
    if (guard.fuzzy_high < guard.fuzzy_low) return t('settings.invalidDowngradeFuzzy')
  }
  if (value.max_active_workers < value.warm_worker_limit) return t('settings.invalidMaxWorkers')
  if (value.warm_startup_concurrency > value.warm_worker_limit) {
    return t('settings.invalidWarmConcurrency')
  }
  // Ultra 常驻数可以为 0（只按需启动），峰值至少为 1 且不小于常驻数
  const ultraWarm = value.ultra_warm_worker_limit
  if (typeof ultraWarm !== 'number' || !Number.isInteger(ultraWarm) || ultraWarm < 0) {
    return t('settings.invalidUltraWarmWorkers')
  }
  if (!isPositiveInteger(value.ultra_max_active_workers) || value.ultra_max_active_workers < ultraWarm) {
    return t('settings.invalidUltraMaxWorkers')
  }
  if (!isMediaBaseURL(value.media_base_url)) return t('settings.invalidMediaBaseURL')
  const retention = mediaRetentionMillis(value.media_retention)
  if (Number.isNaN(retention) || (retention > 0 && retention < MIN_MEDIA_RETENTION_MS)) {
    return t('settings.invalidMediaRetention')
  }
  const maxSize = mediaSizeBytes(value.media_max_size)
  if (Number.isNaN(maxSize) || (maxSize > 0 && maxSize < MIN_MEDIA_MAX_BYTES)) {
    return t('settings.invalidMediaMaxSize')
  }
  return ''
}

// proxyPoolRejected 为服务端拒绝的 IP 池设置（前端组合校验查不出的地址格式问题，例如冒号写法的主机里写了 {session}）：pool 为被拒绝的
// 设置的快照，message 为服务端的错误（固定文案，不含地址）。表单里的 IP 池设置还是这一份时，错误与组合校验的错误一样显示在 IP 池
// 地址框旁、提交时沿用已保存的值；IP 池字段一改就清掉
const proxyPoolRejected = ref<{ pool: string; message: string } | null>(null)

function poolSnapshot(pool: ProxyPoolConfig): string {
  return JSON.stringify(cloneProxyPool(pool))
}

watch(
  () => (form.proxy_pool === undefined ? '' : poolSnapshot(form.proxy_pool)),
  (value) => {
    if (proxyPoolRejected.value !== null && proxyPoolRejected.value.pool !== value) {
      proxyPoolRejected.value = null
    }
  },
)

// proxyPoolInvalid 返回 IP 池模式、地址与端口段组合的错误（已翻译），或服务端拒绝这份设置时的错误；旧版服务端没有 IP 池设置时为空
function proxyPoolInvalid(value: ServiceConfig): string {
  if (value.proxy_pool === undefined) return ''
  const key = proxyPoolProblemKey(proxyPoolProblem(value.proxy_pool))
  if (key !== null) return t(key)
  const rejected = proxyPoolRejected.value
  return rejected !== null && rejected.pool === poolSnapshot(value.proxy_pool) ? rejected.message : ''
}

// proxyPoolServerError 为表单里当前的 IP 池设置被服务端拒绝时的错误（显示在 IP 池地址框旁）
const proxyPoolServerError = computed(() => {
  const rejected = proxyPoolRejected.value
  if (rejected === null || form.proxy_pool === undefined) return ''
  return rejected.pool === poolSnapshot(form.proxy_pool) ? rejected.message : ''
})

// proxyPoolForm 为 IP 池分组的 v-model：分组只在服务端返回了 IP 池设置时显示，此时 form.proxy_pool 一定存在
const proxyPoolForm = computed<ProxyPoolConfig>({
  get: () =>
    form.proxy_pool ?? {
      mode: 'off',
      url: '',
      port_start: DEFAULT_PROXY_POOL_PORT_START,
      port_end: DEFAULT_PROXY_POOL_PORT_END,
    },
  set: (value) => {
    form.proxy_pool = value
  },
})

// ultraMaxFloor 为 Ultra 峰值输入框的下限：至少为 1，且不小于 Ultra 常驻数
const ultraMaxFloor = computed(() =>
  typeof form.ultra_warm_worker_limit === 'number' ? Math.max(1, form.ultra_warm_worker_limit) : 1,
)

// payload 构造保存内容；自动保存时所有失焦字段沿用已保存的值，避免把输入到一半的内容写进去
function payload(includeBlurFields: boolean): ServiceConfig {
  const base = savedConfig.value
  if (base === null) return { ...form }
  if (!includeBlurFields) {
    return { ...form, ...Object.fromEntries(BLUR_KEYS.map((key) => [key, base[key]])) }
  }
  // IP 池组合还不合法（例如选了静态 IP、地址还空着）时沿用已保存的值，其余失焦字段照常保存；错误只显示在 IP 池输入框旁
  if (proxyPoolInvalid(form) !== '' && base.proxy_pool !== undefined) {
    return { ...form, proxy_pool: base.proxy_pool }
  }
  return { ...form }
}

function cancelCountdown(): void {
  if (countdownTimer !== undefined) {
    window.clearInterval(countdownTimer)
    countdownTimer = undefined
  }
  countdown.value = 0
}

function startCountdown(): void {
  cancelCountdown()
  countdown.value = APPLY_DELAY_SECONDS
  countdownTimer = window.setInterval(() => {
    countdown.value -= 1
    if (countdown.value <= 0) {
      cancelCountdown()
      void applyNow()
    }
  }, 1000)
}

async function doSave(includeBlurFields: boolean): Promise<ServiceConfig | null> {
  // IP 池组合不合法时提交已保存的值（见 payload），表单里还没填完的组合不拦住其他字段的保存
  const invalid = validate(form)
  if (invalid !== '') {
    saveError.value = invalid
    return null
  }
  saving.value = true
  try {
    let submitted = payload(includeBlurFields)
    let saved: ServiceConfig
    try {
      saved = await api.saveConfig(submitted)
    } catch (error) {
      const base = savedConfig.value?.proxy_pool
      const rejectedPool = submitted.proxy_pool
      if (
        !(error instanceof ApiError) ||
        error.status !== 400 ||
        error.code !== 'invalid_config' ||
        base === undefined ||
        rejectedPool === undefined ||
        poolSnapshot(rejectedPool) === poolSnapshot(base)
      ) {
        throw error
      }
      // 服务端拒绝了这次提交、其中 IP 池设置有改动：沿用已保存的 IP 池设置再提交一次。这次成功说明是 IP 池设置的问题，错误记到
      // IP 池地址框旁，同一次失焦保存里的密钥、默认代理、监听地址与账户目录照常保存；再失败时按普通的保存错误显示
      submitted = { ...submitted, proxy_pool: base }
      saved = await api.saveConfig(submitted)
      proxyPoolRejected.value = { pool: poolSnapshot(rejectedPool), message: error.message }
    }
    savedConfig.value = saved
    // 代理由服务端规范化（host:port:用户名:密码 保存为 http://用户名:密码@host:port），提交后没有再改动时表单同步显示，
    // 避免一直提示未保存、每次失焦重复保存；自动保存时 proxy 取的是已保存的值，form.proxy 可能是还没提交的编辑，不能覆盖
    if (includeBlurFields && form.proxy === submitted.proxy) form.proxy = saved.proxy
    // IP 池地址同样由服务端规范化（{SESSION} 保存为 {session}、冒号写法改写成 URL 形式），提交后没有再改动时同步显示
    if (
      includeBlurFields &&
      saved.proxy_pool !== undefined &&
      form.proxy_pool !== undefined &&
      JSON.stringify(form.proxy_pool) === JSON.stringify(submitted.proxy_pool)
    ) {
      form.proxy_pool = cloneProxyPool(saved.proxy_pool)
    }
    // 留空的密钥由服务端换成默认密钥，表单同步显示，避免一直提示未保存
    if (form.proxy_api_key.trim() === '') form.proxy_api_key = saved.proxy_api_key
    // 服务端把时长写成规范形式（3m 保存为 3m0s、90s 保存为 1m30s），时长相同时表单同步显示，避免一直提示未保存
    for (const key of ['init_timeout', 'request_timeout', 'first_event_timeout'] as const) {
      if (form[key] !== saved[key] && durationMillis(form[key]) === durationMillis(saved[key])) {
        form[key] = saved[key]
      }
    }
    // 媒体设置同样由服务端写成规范形式（168h 保存为 7d、20gb 保存为 20GB、地址去掉末尾的 /），含义相同时表单同步显示
    if (
      form.media_retention !== saved.media_retention &&
      mediaRetentionMillis(form.media_retention) === mediaRetentionMillis(saved.media_retention)
    ) {
      form.media_retention = saved.media_retention
    }
    if (
      form.media_max_size !== saved.media_max_size &&
      mediaSizeBytes(form.media_max_size) === mediaSizeBytes(saved.media_max_size)
    ) {
      form.media_max_size = saved.media_max_size
    }
    if (
      form.media_base_url !== saved.media_base_url &&
      form.media_base_url.trim().replace(/\/+$/, '') === saved.media_base_url
    ) {
      form.media_base_url = saved.media_base_url
    }
    // 两个主用模型排队上限在旧版服务端可能没有返回，只在两边都有值时同步
    for (const key of ['primary_queue_wait', 'ultra_primary_queue_wait'] as const) {
      const local = form[key]
      const remote = saved[key]
      if (
        local !== undefined &&
        remote !== undefined &&
        local !== remote &&
        durationMillis(local) === durationMillis(remote)
      ) {
        form[key] = remote
      }
    }
    saveError.value = ''
    emit('saved', saved)
    if (saved.service_restart_required && props.running && autoApply.value && !applying.value) {
      if (proxyTesting.value) applyDeferred = true
      else startCountdown()
    }
    return saved
  } catch (error) {
    saveError.value = error instanceof Error ? error.message : t('common.error')
    return null
  } finally {
    saving.value = false
  }
}

// save 串行执行保存，避免并发请求乱序覆盖
function save(includeBlurFields: boolean): Promise<ServiceConfig | null> {
  const run = saveChain.then(() => doSave(includeBlurFields))
  saveChain = run.catch(() => undefined)
  return run
}

function scheduleSave(): void {
  cancelCountdown()
  if (saveTimer !== undefined) window.clearTimeout(saveTimer)
  saveTimer = window.setTimeout(() => {
    saveTimer = undefined
    void save(false)
  }, SAVE_DELAY_MS)
}

// commitBlurField 失焦字段在输入框失去焦点或按回车时保存；按实际会提交的内容比较（IP 池组合不合法时为已保存的值），没有变化时不保存
function commitBlurField(): void {
  const base = savedConfig.value
  if (base === null || snapshot(payload(true), BLUR_KEYS) === snapshot(base, BLUR_KEYS)) return
  void save(true)
}

// commitProxyPool IP 池的模式选完、地址或端口失焦时保存；组合不合法时不提交（不弹提示），错误显示在 IP 池分组的输入框旁
function commitProxyPool(): void {
  if (proxyPoolInvalid(form) !== '') return
  commitBlurField()
}

// onProxyTesting 测试开始时暂停进行中的倒计时，结束后只在测试通过、仍需要应用时重新开始
function onProxyTesting(running: boolean, passed: boolean): void {
  proxyTesting.value = running
  if (running) {
    if (countdownTimer !== undefined) {
      cancelCountdown()
      applyDeferred = true
    }
    return
  }
  const deferred = applyDeferred
  applyDeferred = false
  if (
    deferred &&
    passed &&
    savedConfig.value?.service_restart_required === true &&
    props.running &&
    autoApply.value &&
    !applying.value
  ) {
    startCountdown()
  }
}

// applyNow 等待进行中的请求结束后重启生成服务，让已保存的配置生效
async function applyNow(): Promise<void> {
  cancelCountdown()
  if (applying.value) return
  applying.value = true
  try {
    await api.restartService()
    const fresh = await api.config()
    savedConfig.value = fresh
    emit('saved', fresh)
    emit('notice', t('settings.applied'), 'success')
  } catch (error) {
    emit('notice', error instanceof Error ? error.message : t('common.error'), 'error')
  } finally {
    applying.value = false
  }
}

// saveAndApply 立即保存全部字段，需要时马上应用
async function saveAndApply(): Promise<void> {
  if (saveTimer !== undefined) {
    window.clearTimeout(saveTimer)
    saveTimer = undefined
  }
  const saved = await save(true)
  if (saved === null) {
    if (saveError.value !== '') emit('notice', saveError.value, 'error')
    return
  }
  // IP 池组合不合法时其余字段照常保存、IP 池沿用已保存的值：不提示“已保存”，改为提示 IP 池的错误
  const poolProblem = proxyPoolInvalid(form)
  if (saved.service_restart_required && props.running) {
    await applyNow()
  } else if (poolProblem === '') {
    emit(
      'notice',
      t(saved.service_restart_required ? 'settings.savedStopped' : 'settings.saved'),
      'success',
    )
  }
  if (poolProblem !== '') emit('notice', poolProblem, 'error')
}

watch(
  () => props.config,
  (config) => {
    if (config === null) return
    const base = savedConfig.value
    const hasLocalEdits = base !== null && snapshot(form) !== snapshot(base)
    if (!hasLocalEdits) {
      Object.assign(form, config)
      form.downgrade_guard = cloneDowngradeGuard(config.downgrade_guard)
      guardModelsText.value = form.downgrade_guard.models.join(', ')
      form.stream_playground_models = [...(config.stream_playground_models ?? [])]
      streamModelsText.value = form.stream_playground_models.join(', ')
      // 旧版服务端不返回主用模型字段时按默认值显示；payload 保存时总会带上这几个键，旧版服务端忽略未知字段，
      // 新版服务端读取时总是返回它们（Ultra 列表关闭时为空数组），不存在“新版服务端 + 字段缺失”的组合，不会重置服务端现值
      form.primary_models = [...(config.primary_models ?? [])]
      primaryModelsText.value = form.primary_models.join(', ')
      form.primary_queue_wait = config.primary_queue_wait ?? '10s'
      form.ultra_primary_models = [...(config.ultra_primary_models ?? DEFAULT_ULTRA_PRIMARY_MODELS)]
      ultraPrimaryModelsText.value = form.ultra_primary_models.join(', ')
      form.ultra_primary_queue_wait = config.ultra_primary_queue_wait ?? '10s'
      // 旧版服务端不返回 IP 池设置：表单里也去掉，保存时不提交，服务端沿用现值
      if (config.proxy_pool !== undefined) form.proxy_pool = cloneProxyPool(config.proxy_pool)
      else delete form.proxy_pool
    }
    savedConfig.value = config
  },
  { immediate: true },
)

// 普通字段修改后自动保存
watch(
  () => snapshot(form, AUTO_KEYS),
  (value) => {
    const base = savedConfig.value
    if (base === null || value === snapshot(base, AUTO_KEYS)) return
    scheduleSave()
  },
)

// 输出方式切回 inline 时媒体链接地址框随之隐藏：地址还是没能保存的不合法输入时恢复为已保存的值，
// 否则看不见、改不了的地址错误会拦住这次切换和之后的每次保存
watch(
  () => form.media_output,
  (value) => {
    if (value !== 'inline' || isMediaBaseURL(form.media_base_url)) return
    form.media_base_url = savedConfig.value?.media_base_url ?? ''
  },
)

watch(autoApply, (value) => {
  writeStorage(AUTO_APPLY_KEY, String(value))
  if (!value) cancelCountdown()
})

onBeforeUnmount(() => {
  if (saveTimer !== undefined) {
    window.clearTimeout(saveTimer)
    void save(false)
  }
  cancelCountdown()
})
</script>

<template>
  <section class="mx-auto w-full max-w-3xl flex-1 overflow-auto p-4 md:p-8">
    <div
      class="mb-6 flex flex-wrap items-center justify-between gap-3 border-b border-[#30363d] pb-2"
    >
      <h2 class="text-2xl font-bold text-white">{{ t('section.settings.title') }}</h2>
      <div v-if="config !== null" class="flex flex-wrap items-center gap-3 text-xs">
        <span :class="statusClass">{{ statusText }}</span>
        <label class="flex cursor-pointer items-center gap-1.5 text-gray-400">
          <input v-model="autoApply" type="checkbox" />
          {{ t('settings.autoApply') }}
        </label>
      </div>
    </div>

    <div v-if="error" class="rounded border border-red-500/40 bg-red-500/10 p-4 text-red-300">
      {{ error }}
    </div>
    <div v-else-if="loading || config === null" class="py-12 text-center text-gray-500">
      {{ t('common.loading') }}
    </div>
    <form v-else class="space-y-6" @submit.prevent="saveAndApply">
      <p class="text-xs leading-5 text-gray-500">{{ t('settings.liveHint') }}</p>
      <div
        v-if="applying"
        class="flex items-center gap-2 rounded border border-cyan-500/30 bg-cyan-500/10 px-4 py-3 text-sm text-cyan-200"
      >
        <UiIcon name="spinner" :size="14" />
        {{ t('settings.applying') }}
      </div>
      <div
        v-else-if="config.service_restart_required"
        class="flex flex-wrap items-center gap-3 rounded border border-blue-500/30 bg-blue-500/10 px-4 py-3 text-sm text-blue-200"
      >
        <span class="min-w-0 flex-1">
          {{ running ? t('settings.applyPending') : t('settings.savedStopped') }}
        </span>
        <template v-if="running">
          <span v-if="countdown > 0" class="font-mono text-xs text-blue-300">
            {{ tf('settings.applyCountdown', { seconds: countdown }) }}
          </span>
          <button class="btn btn-sm btn-primary" type="button" @click="applyNow">
            {{ t('settings.applyNow') }}
          </button>
          <button v-if="countdown > 0" class="btn btn-sm" type="button" @click="cancelCountdown">
            {{ t('common.cancel') }}
          </button>
        </template>
      </div>
      <div
        v-if="config.management_restart_required"
        class="rounded border border-amber-500/30 bg-amber-500/10 px-4 py-3 text-sm text-amber-200"
      >
        {{ t('settings.pendingManagement') }}
      </div>

      <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.authPath')
          }}</span>
          <input
            v-model.trim="form.auth_states"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            required
            autocomplete="off"
            @change="commitBlurField"
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.listen')
          }}</span>
          <input
            v-model.trim="form.listen_addr"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            required
            autocomplete="off"
            @change="commitBlurField"
          />
          <span
            v-if="form.listen_addr !== config.active_listen_addr"
            class="mt-1 block text-xs text-gray-500"
          >
            {{ t('settings.activeValue') }}: {{ config.active_listen_addr }}
          </span>
        </label>
      </div>

      <div class="rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-300">{{
            t('settings.apiKey')
          }}</span>
          <div class="flex gap-2">
            <input
              v-model="form.proxy_api_key"
              :type="revealKey ? 'text' : 'password'"
              :placeholder="DEFAULT_API_KEY"
              class="min-w-0 flex-1 rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              autocomplete="new-password"
              @change="commitBlurField"
            />
            <button
              class="rounded border border-[#30363d] bg-[#21262d] px-3 text-xs text-gray-300 transition hover:bg-[#30363d]"
              type="button"
              @click="revealKey = !revealKey"
            >
              {{ revealKey ? t('settings.hide') : t('settings.reveal') }}
            </button>
          </div>
          <span class="mt-1 block text-xs text-gray-500">{{ t('settings.apiKeyHot') }}</span>
          <span class="mt-1 block text-xs text-gray-500">{{
            tf('settings.apiKeyDefault', { key: DEFAULT_API_KEY })
          }}</span>
        </label>
      </div>

      <!-- 代理输入里有掩码按钮与“测试代理”按钮，标题不用 label 包裹，避免点说明文字或测试结果时触发掩码按钮 -->
      <div class="rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <span class="mb-1 block text-sm font-medium text-gray-300">{{ t('settings.proxy') }}</span>
        <ProxyInput
          v-model="form.proxy"
          input-class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
          :help="t('settings.proxyHelp')"
          :allow-empty-test="false"
          :label="t('settings.proxy')"
          @commit="commitBlurField"
          @testing="onProxyTesting"
        />
      </div>

      <ProxyPoolSettings
        v-if="form.proxy_pool !== undefined"
        v-model="proxyPoolForm"
        :status="proxyPoolStatus"
        :max-active="form.max_active_workers"
        :ultra-max-active="form.ultra_max_active_workers"
        :warm-concurrency="form.warm_startup_concurrency"
        :has-default-proxy="form.proxy.trim() !== ''"
        :server-error="proxyPoolServerError"
        @commit="commitProxyPool"
        @testing="onProxyTesting"
        @notice="(message, tone) => emit('notice', message, tone)"
      />

      <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.initTimeout')
          }}</span>
          <input
            v-model.trim="form.init_timeout"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            required
            autocomplete="off"
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.requestTimeout')
          }}</span>
          <input
            v-model.trim="form.request_timeout"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            required
            autocomplete="off"
          />
          <span class="mt-2 block text-xs text-gray-500">{{ t('settings.requestTimeoutHelp') }}</span>
        </label>
      </div>

      <label class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.firstEventTimeout') }}</span>
        <input
          v-model.trim="form.first_event_timeout"
          class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
          placeholder="0s"
          required
          autocomplete="off"
        />
        <span class="mt-2 block text-xs text-gray-500">{{ t('settings.firstEventTimeoutHelp') }}</span>
      </label>

      <div class="grid grid-cols-1 gap-4 md:grid-cols-4">
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.warmWorkerLimit')
          }}</span>
          <input
            v-model.number="form.warm_worker_limit"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            type="number"
            min="1"
            required
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.maxActiveWorkers')
          }}</span>
          <input
            v-model.number="form.max_active_workers"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            type="number"
            :min="form.warm_worker_limit"
            required
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.warmStartupConcurrency')
          }}</span>
          <input
            v-model.number="form.warm_startup_concurrency"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            type="number"
            min="1"
            :max="form.warm_worker_limit"
            required
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.perAccountConcurrency')
          }}</span>
          <input
            v-model.number="form.per_account_concurrency"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            type="number"
            min="1"
            required
          />
        </label>
      </div>
      <p class="-mt-4 text-xs leading-5 text-gray-500">{{ t('settings.workerPartitionHelp') }}</p>

      <label class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.accountRpmLimit') }}</span>
        <input
          v-model.number="form.account_rpm_limit"
          class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
          type="number"
          min="0"
          max="1000"
          required
        />
        <span class="mt-2 block text-xs text-gray-500">{{ t('settings.accountRpmLimitHelp') }}</span>
      </label>

      <fieldset class="block rounded-lg border border-fuchsia-500/30 bg-[#161b22] p-4">
        <legend class="sr-only">{{ t('settings.ultraTitle') }}</legend>
        <div class="mb-1 flex items-center gap-2">
          <span class="tag tag-ultra">{{ t('pool.ultra') }}</span>
          <span class="text-sm font-medium text-gray-300">{{ t('settings.ultraTitle') }}</span>
        </div>
        <p class="mb-4 text-xs leading-5 text-gray-500">{{ t('settings.ultraHelp') }}</p>
        <label class="flex items-start gap-3">
          <input v-model="form.ultra_exclusive" class="mt-0.5 h-4 w-4 accent-blue-500" type="checkbox" />
          <span>
            <span class="block text-sm font-medium text-gray-300">{{ t('settings.ultraExclusive') }}</span>
            <span class="mt-1 block text-xs text-gray-500">{{ t('settings.ultraExclusiveHelp') }}</span>
          </span>
        </label>
        <label class="mt-3 flex items-start gap-3">
          <input
            v-model="form.ultra_include_pro"
            class="mt-0.5 h-4 w-4 accent-blue-500"
            type="checkbox"
          />
          <span>
            <span class="block text-sm font-medium text-gray-300">{{
              t('settings.ultraIncludePro')
            }}</span>
            <span class="mt-1 block text-xs text-gray-500">{{
              t('settings.ultraIncludeProHelp')
            }}</span>
          </span>
        </label>
        <!-- 并入开关开启而独占关闭时，普通路径仍会用到 Pro，用户想要的“Pro 不服务普通路径”没有达成，这里明确提示 -->
        <p
          v-if="form.ultra_include_pro && !form.ultra_exclusive"
          class="mt-3 rounded border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs leading-5 text-amber-200"
          role="note"
        >
          {{ t('settings.ultraIncludeProSharedHint') }}
        </p>
        <div class="mt-4 grid grid-cols-1 gap-4 border-t border-[#30363d] pt-4 md:grid-cols-2">
          <label class="block">
            <span class="mb-1 block text-sm font-medium text-gray-400">{{
              t('settings.ultraWarmWorkerLimit')
            }}</span>
            <input
              v-model.number="form.ultra_warm_worker_limit"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              type="number"
              min="0"
              required
            />
          </label>
          <label class="block">
            <span class="mb-1 block text-sm font-medium text-gray-400">{{
              t('settings.ultraMaxActiveWorkers')
            }}</span>
            <input
              v-model.number="form.ultra_max_active_workers"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              type="number"
              :min="ultraMaxFloor"
              required
            />
          </label>
        </div>
        <span class="mt-2 block text-xs text-gray-500">{{ t('settings.ultraWorkersHelp') }}</span>
      </fieldset>

      <label class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <span class="mb-2 block text-sm font-medium text-gray-300">{{
          t('settings.routingStrategy')
        }}</span>
        <UiSelect
          v-model="form.routing_strategy"
          class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
        >
          <option value="round-robin">{{ t('settings.routingRoundRobin') }}</option>
          <option value="fill-first">{{ t('settings.routingFillFirst') }}</option>
        </UiSelect>
        <span class="mt-2 block text-xs text-gray-500">{{ t('settings.routingHelp') }}</span>
      </label>

      <label class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <span class="mb-2 block text-sm font-medium text-gray-300">{{
          t('settings.waaBackend')
        }}</span>
        <UiSelect
          v-model="form.waa_backend"
          class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
        >
          <option value="camoufox">{{ t('settings.waaBackendCamoufox') }}</option>
          <option value="go">{{ t('settings.waaBackendGo') }}</option>
        </UiSelect>
        <span class="mt-2 block text-xs text-gray-500">{{ t('settings.waaBackendHelp') }}</span>
      </label>

      <fieldset class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <legend class="sr-only">{{ t('settings.mediaTitle') }}</legend>
        <span class="mb-1 block text-sm font-medium text-gray-300">{{
          t('settings.mediaTitle')
        }}</span>
        <p class="mb-4 text-xs leading-5 text-gray-500">{{ t('settings.mediaHelp') }}</p>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.mediaOutput')
          }}</span>
          <UiSelect
            v-model="form.media_output"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
          >
            <option value="inline">{{ t('settings.mediaOutputInline') }}</option>
            <option value="url">{{ t('settings.mediaOutputURL') }}</option>
          </UiSelect>
          <span class="mt-2 block text-xs text-gray-500">{{ t('settings.mediaOutputHelp') }}</span>
        </label>
        <label v-if="form.media_output === 'url'" class="mt-4 block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.mediaBaseURL')
          }}</span>
          <input
            v-model.trim="form.media_base_url"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            placeholder="https://ai.example.com"
            autocomplete="off"
            spellcheck="false"
            @change="commitBlurField"
          />
          <span class="mt-2 block text-xs text-gray-500">{{ t('settings.mediaBaseURLHelp') }}</span>
        </label>
        <div class="mt-4 grid grid-cols-1 gap-4 border-t border-[#30363d] pt-4 md:grid-cols-2">
          <label class="block">
            <span class="mb-1 block text-sm font-medium text-gray-400">{{
              t('settings.mediaRetention')
            }}</span>
            <input
              v-model.trim="form.media_retention"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              placeholder="7d"
              autocomplete="off"
              spellcheck="false"
              @change="commitBlurField"
            />
          </label>
          <label class="block">
            <span class="mb-1 block text-sm font-medium text-gray-400">{{
              t('settings.mediaMaxSize')
            }}</span>
            <input
              v-model.trim="form.media_max_size"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              placeholder="20GB"
              autocomplete="off"
              spellcheck="false"
              @change="commitBlurField"
            />
          </label>
        </div>
        <span class="mt-2 block text-xs text-gray-500">{{ t('settings.mediaCleanupHelp') }}</span>
      </fieldset>

      <fieldset class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <legend class="sr-only">{{ t('settings.upstreamChannels') }}</legend>
        <span class="mb-2 block text-sm font-medium text-gray-300">{{
          t('settings.upstreamChannels')
        }}</span>
        <div class="flex flex-wrap gap-4">
          <label
            v-for="channel in upstreamChannelOptions"
            :key="channel"
            class="flex items-center gap-2 text-sm text-gray-300"
          >
            <input
              class="h-4 w-4 accent-blue-500"
              type="checkbox"
              :checked="form.upstream_channels.includes(channel)"
              :disabled="
                form.upstream_channels.length === 1 && form.upstream_channels.includes(channel)
              "
              @change="toggleUpstreamChannel(channel, $event)"
            />
            {{ t(channelLabelKey(channel)) }}
          </label>
        </div>
        <span class="mt-2 block text-xs text-gray-500">{{
          t('settings.upstreamChannelsHelp')
        }}</span>
        <span
          v-if="buildChannelText"
          class="mt-1 block text-xs"
          :class="buildChannelWarn ? 'text-yellow-400' : 'text-gray-400'"
          >{{ buildChannelText }}</span
        >
      </fieldset>

      <div class="rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <label class="block">
          <span class="mb-2 block text-sm font-medium text-gray-300">{{
            t('settings.streamPlaygroundModels')
          }}</span>
          <input
            :value="streamModelsText"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            type="text"
            spellcheck="false"
            autocomplete="off"
            @input="onStreamModelsInput"
          />
        </label>
        <span class="mt-2 block text-xs text-gray-500">{{
          t('settings.streamPlaygroundModelsHelp')
        }}</span>
        <div v-if="subscriptionModels.length > 0" class="mt-3 flex flex-wrap items-center gap-2">
          <span class="text-xs text-gray-500">{{ t('settings.streamPlaygroundModelsHint') }}</span>
          <button
            v-for="model in subscriptionModels"
            :key="model"
            class="btn btn-sm font-mono"
            type="button"
            :disabled="streamModelListed(model)"
            @click="addStreamModel(model)"
          >
            <UiIcon v-if="streamModelListed(model)" name="check" :size="12" />
            {{ model }}
          </button>
        </div>
      </div>

      <div class="rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.primaryTitle') }}</span>
        <div class="grid grid-cols-1 gap-4 md:grid-cols-3">
          <label class="block md:col-span-2">
            <span class="mb-1 block text-sm font-medium text-gray-400">{{
              t('settings.primaryModels')
            }}</span>
            <input
              :value="primaryModelsText"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              type="text"
              spellcheck="false"
              autocomplete="off"
              @input="onPrimaryModelsInput"
            />
          </label>
          <label class="block">
            <span class="mb-1 block text-sm font-medium text-gray-400">{{
              t('settings.primaryQueueWait')
            }}</span>
            <input
              v-model.trim="form.primary_queue_wait"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              placeholder="10s"
              required
              autocomplete="off"
            />
          </label>
        </div>
        <span class="mt-2 block text-xs text-gray-500">{{ t('settings.primaryHelp') }}</span>
        <div class="mt-4 border-t border-[#30363d] pt-4">
          <div class="grid grid-cols-1 gap-4 md:grid-cols-3">
            <label class="block md:col-span-2">
              <span class="mb-1 flex items-center gap-2 text-sm font-medium text-gray-400">
                <span class="tag tag-ultra">{{ t('pool.ultra') }}</span>
                {{ t('settings.ultraPrimaryModels') }}
              </span>
              <input
                :value="ultraPrimaryModelsText"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="text"
                spellcheck="false"
                autocomplete="off"
                @input="onUltraPrimaryModelsInput"
              />
            </label>
            <label class="block">
              <span class="mb-1 block text-sm font-medium text-gray-400">{{
                t('settings.ultraPrimaryQueueWait')
              }}</span>
              <input
                v-model.trim="form.ultra_primary_queue_wait"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                placeholder="10s"
                required
                autocomplete="off"
              />
            </label>
          </div>
          <span class="mt-2 block text-xs text-gray-500">{{ t('settings.ultraPrimaryHelp') }}</span>
        </div>
      </div>

      <label class="flex items-center gap-3 rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <input v-model="form.temporary_chat" class="h-4 w-4 accent-blue-500" type="checkbox" />
        <span class="text-sm font-medium text-gray-300">{{ t('settings.temporaryChat') }}</span>
      </label>

      <label class="flex items-start gap-3 rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <input v-model="form.ignore_client_seed" class="mt-0.5 h-4 w-4 accent-blue-500" type="checkbox" />
        <span>
          <span class="block text-sm font-medium text-gray-300">{{ t('settings.ignoreClientSeed') }}</span>
          <span class="mt-1 block text-xs text-gray-500">{{ t('settings.ignoreClientSeedHelp') }}</span>
        </span>
      </label>

      <label class="flex items-start gap-3 rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <input v-model="form.repeat_prompt_nonce" class="mt-0.5 h-4 w-4 accent-blue-500" type="checkbox" />
        <span>
          <span class="block text-sm font-medium text-gray-300">{{ t('settings.repeatPromptNonce') }}</span>
          <span class="mt-1 block text-xs text-gray-500">{{ t('settings.repeatPromptNonceHelp') }}</span>
        </span>
      </label>

      <label class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.minOutputTokens') }}</span>
        <input
          v-model.number="form.min_output_tokens"
          class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
          type="number"
          min="0"
          step="1000"
          required
        />
        <span class="mt-2 block text-xs text-gray-500">{{ t('settings.minOutputTokensHelp') }}</span>
      </label>

      <div class="rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <label class="flex items-start gap-3">
          <input v-model="form.downgrade_guard.enabled" class="mt-0.5 h-4 w-4 accent-blue-500" type="checkbox" />
          <span>
            <span class="block text-sm font-medium text-gray-300">{{ t('settings.downgradeGuard') }}</span>
            <span class="mt-1 block text-xs text-gray-500">{{ t('settings.downgradeGuardHelp') }}</span>
          </span>
        </label>
        <div v-if="form.downgrade_guard.enabled" class="mt-4 space-y-4 border-t border-[#30363d] pt-4">
          <label class="block">
            <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeModels') }}</span>
            <input
              :value="guardModelsText"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              type="text"
              spellcheck="false"
              autocomplete="off"
              @input="onGuardModelsInput"
            />
            <span class="mt-2 block text-xs text-gray-500">{{ t('settings.downgradeModelsHelp') }}</span>
          </label>
          <div class="grid gap-4 sm:grid-cols-3">
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeSpeed') }}</span>
              <input
                v-model.number="form.downgrade_guard.speed_threshold"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="1"
                step="5"
                required
              />
            </label>
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeMinTokens') }}</span>
              <input
                v-model.number="form.downgrade_guard.min_tokens"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="1"
                step="10"
                required
              />
            </label>
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeMinWindow') }}</span>
              <input
                v-model.number="form.downgrade_guard.min_window_ms"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="0"
                step="100"
                required
              />
            </label>
          </div>
          <span class="block text-xs text-gray-500">{{ t('settings.downgradeSpeedHelp') }}</span>
          <div class="grid gap-4 sm:grid-cols-3">
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeFuzzyLow') }}</span>
              <input
                v-model.number="form.downgrade_guard.fuzzy_low"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="1"
                step="5"
                required
              />
            </label>
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeFuzzyHigh') }}</span>
              <input
                v-model.number="form.downgrade_guard.fuzzy_high"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="1"
                step="5"
                required
              />
            </label>
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeCountTimeout') }}</span>
              <input
                v-model.number="form.downgrade_guard.count_timeout_ms"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="0"
                step="100"
                required
              />
            </label>
          </div>
          <span class="block text-xs text-gray-500">{{ t('settings.downgradeFuzzyHelp') }}</span>
          <label class="block">
            <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeMaxHold') }}</span>
            <input
              v-model.number="form.downgrade_guard.max_hold_ms"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              type="number"
              min="0"
              step="500"
              required
            />
            <span class="mt-2 block text-xs text-gray-500">{{ t('settings.downgradeMaxHoldHelp') }}</span>
          </label>
          <label class="flex items-start gap-3">
            <input v-model="form.downgrade_guard.fast_mode" class="mt-0.5 h-4 w-4 accent-blue-500" type="checkbox" />
            <span>
              <span class="block text-sm font-medium text-gray-300">{{ t('settings.downgradeFastMode') }}</span>
              <span class="mt-1 block text-xs text-gray-500">{{ t('settings.downgradeFastModeHelp') }}</span>
            </span>
          </label>
          <label class="block">
            <span class="mb-2 block text-sm font-medium text-gray-300">{{
              t('settings.downgradeRejectStatus')
            }}</span>
            <UiSelect
              v-model="form.downgrade_guard.reject_status"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            >
              <option :value="400">{{ t('settings.downgradeReject400') }}</option>
              <option :value="503">{{ t('settings.downgradeReject503') }}</option>
            </UiSelect>
            <span class="mt-2 block text-xs text-gray-500">{{ t('settings.downgradeRejectStatusHelp') }}</span>
          </label>
          <label class="block">
            <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeMemory') }}</span>
            <input
              v-model.number="form.downgrade_guard.memory_minutes"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              type="number"
              min="0"
              step="5"
              required
            />
            <span class="mt-2 block text-xs text-gray-500">{{ t('settings.downgradeMemoryHelp') }}</span>
          </label>
        </div>
      </div>

      <div class="flex items-center justify-end gap-3 pt-4">
        <span class="text-xs" :class="statusClass">{{ statusText }}</span>
        <button
          class="btn btn-primary h-9 px-5"
          type="submit"
          :disabled="saving || applying"
          :aria-busy="saving || applying"
        >
          <UiIcon name="check" :size="16" />
          {{ t('settings.saveAndApply') }}
        </button>
      </div>
    </form>
  </section>
</template>
