<script setup lang="ts">
import {
  computed,
  defineAsyncComponent,
  h,
  onMounted,
  onUnmounted,
  reactive,
  ref,
  watch,
} from 'vue'
import { api, openAdminEvents, type EventConnection, type EventsState } from '@/api'
import { useI18n, type TranslationKey } from '@/i18n'
import type {
  Account,
  AdminLog,
  AdminEvent,
  EgressStatus,
  Model,
  Cooldown,
  RequestSummary,
  ServiceConfig,
  ServiceStatus,
  TabID,
} from '@/types'
import AccountsPanel from '@/components/AccountsPanel.vue'
import LogsPanel from '@/components/LogsPanel.vue'
import ModelsTable from '@/components/ModelsTable.vue'
import PlaygroundPanel from '@/components/PlaygroundPanel.vue'
import RequestsPanel from '@/components/RequestsPanel.vue'
import SettingsPanel from '@/components/SettingsPanel.vue'
import UiConfirm from '@/components/UiConfirm.vue'
import UiIcon, { type IconName } from '@/components/UiIcon.vue'
import { formatClock } from '@/proxyPool'
import { readStorage, writeStorage } from '@/storage'

const { availableLocales, locale, setLocale, t, tf } = useI18n()
// 用量页按需加载：图表库只在打开用量页时下载，不进入主包
const UsagePanel = defineAsyncComponent({
  loader: () => import('@/components/UsagePanel.vue'),
  loadingComponent: {
    render: () =>
      h('p', { class: 'p-8 text-sm text-gray-500', role: 'status' }, t('usage.panelLoading')),
  },
  errorComponent: {
    render: () =>
      h('p', { class: 'p-8 text-sm text-red-300', role: 'alert' }, t('usage.panelFailed')),
  },
  delay: 150,
})
// 前端最多保留的日志条数；日志面板按可视区域渲染，这里只限制内存占用
const LOG_LIMIT = 1500
// 前端最多保留的请求条数；超出时先丢弃最早的已结束请求，进行中的请求始终保留
const REQUEST_LIMIT = 500
const TAB_STORAGE_KEY = 'aistudio2api_active_tab'
const validTabs: TabID[] = [
  'logs',
  'accounts',
  'models',
  'requests',
  'usage',
  'settings',
  'playground',
]
const savedTab = readStorage(TAB_STORAGE_KEY) as TabID | null
const currentTab = ref<TabID>(savedTab && validTabs.includes(savedTab) ? savedTab : 'logs')
watch(currentTab, (tab) => {
  writeStorage(TAB_STORAGE_KEY, tab)
})
const status = ref<ServiceStatus | null>(null)
const logs = ref<AdminLog[]>([])
const accounts = ref<Account[]>([])
const models = ref<Model[]>([])
const cooldowns = ref<Cooldown[]>([])
const requests = ref<RequestSummary[]>([])
const config = ref<ServiceConfig | null>(null)
const startPending = ref(false)
const stopPending = ref(false)
const launchCancellationRequested = ref(false)
const notice = reactive({ message: '', tone: 'success' as 'success' | 'error' })
const loading = reactive({
  accounts: true,
  models: true,
  requests: true,
  cooldowns: true,
  config: true,
})
const errors = reactive({ accounts: '', models: '', requests: '', cooldowns: '', config: '' })
let eventConnection: EventConnection | undefined
let noticeTimer: number | undefined

const navigation: { id: TabID; label: TranslationKey; icon: IconName }[] = [
  { id: 'logs', label: 'nav.logs', icon: 'dashboard' },
  { id: 'accounts', label: 'nav.accounts', icon: 'key' },
  { id: 'models', label: 'nav.models', icon: 'dashboard' },
  { id: 'requests', label: 'nav.requests', icon: 'info' },
  { id: 'usage', label: 'nav.usage', icon: 'usage' },
  { id: 'settings', label: 'nav.settings', icon: 'settings' },
  { id: 'playground', label: 'nav.playground', icon: 'chat' },
]

const serviceState = computed(() => {
  if (status.value === null) return 'unavailable'
  if (status.value.state === 'RUNNING') return 'running'
  if (status.value.state === 'LAUNCHING') return 'launching'
  if (startPending.value && !launchCancellationRequested.value) return 'launching'
  return 'stopped'
})
const statusColor = computed(() => {
  if (serviceState.value === 'running') return 'bg-green-500 shadow-[0_0_10px_rgba(34,197,94,0.5)]'
  if (serviceState.value === 'launching') return 'bg-cyan-400 animate-pulse'
  return 'bg-gray-600'
})
// workerSets 由状态接口的账户列表构造，账户页据此标记每个账户的 Worker 状态
const workerSets = computed(() => ({
  warm: new Set(status.value?.workers?.warm_ids ?? []),
  starting: new Set(status.value?.workers?.starting_ids ?? []),
}))

// catalogSyncLine 为侧栏的模型目录同步行：暂停中（上游 429 或出口限流）、有缺失或有疑似失效时才显示
// catalogPauseReason 为目录同步暂停原因的显示：服务端给的是固定的中文枚举（上游 429、出口限流），按界面语言显示
function catalogPauseReason(reason: string): string {
  if (reason === '上游 429') return t('app.catalogPauseUpstream')
  if (reason === '出口限流') return t('app.catalogPauseEgress')
  return reason
}

const catalogSyncLine = computed(() => {
  const sync = status.value?.catalog_sync
  if (sync === undefined) return null
  if (sync.pause_reason) {
    // 出口限流暂停没有恢复时间；服务端的零值时间（0001-01-01）同样按没有处理
    const until = sync.paused_until ? new Date(sync.paused_until) : null
    const time =
      until !== null && until.getTime() > 0
        ? until.toLocaleTimeString([], {
            hour: '2-digit',
            minute: '2-digit',
            second: '2-digit',
            hour12: false,
          })
        : '-'
    return {
      paused: true,
      text: tf('app.catalogSyncPaused', { time, reason: catalogPauseReason(sync.pause_reason) }),
    }
  }
  const missing = sync.missing ?? 0
  const suspects = sync.auth_suspects ?? 0
  if (missing === 0 && suspects === 0) return null
  return { paused: false, text: tf('app.catalogSyncPending', { count: missing, suspects }) }
})

// authRecheckLine 为侧栏的登录复核行：有疑似登录失效、最近 10 分钟有新增需要登录或有坏号时才显示
const authRecheckLine = computed(() => {
  const value = status.value
  if (value === null) return null
  const recheck = value.auth_recheck
  const suspects = recheck?.suspects ?? 0
  const recent = value.accounts?.auth_required_10m ?? 0
  const bad = value.bad_accounts ?? 0
  if (suspects === 0 && recent === 0 && bad === 0) return null
  return {
    text: tf('app.authRecheckLine', { suspects, recent, bad }),
    detail: tf('app.authRecheckDetail', {
      cleared: recheck?.cleared_1h ?? 0,
      confirmed: recheck?.confirmed_1h ?? 0,
      recovered: recheck?.recovered_1h ?? 0,
      invalid: recheck?.still_invalid_1h ?? 0,
    }),
    warn: suspects > 0 || recent > 0,
  }
})

// ultraWorkers 为侧栏显示的 Ultra 分区 Worker 计数；Ultra 号池没有账户（开启 ULTRA_INCLUDE_PRO 时 Pro 账户也计入）
// 也没有 Ultra Worker 时不显示
const ultraWorkers = computed(() => {
  const workers = status.value?.ultra_workers
  if (workers === undefined) return null
  const accounts = status.value?.ultra_accounts?.total ?? 0
  return accounts > 0 || workers.occupied > 0 || workers.warm + workers.starting > 0 ? workers : null
})

// proxyPoolLine 为侧栏 Worker 区下方的 IP 池一行：静态显示端口占用、冷却与共用，动态显示会话数与换会话次数，
// 有旧出口、仍走默认代理的 Worker、外部占用、已删账户占用时追加；悬停列出冷却端口与裸 429 最多的端口
const PROXY_POOL_TOOLTIP_PORTS = 5

// listPorts 列出提示里的端口，最多 PROXY_POOL_TOOLTIP_PORTS 个，超出时以省略号结尾
function listPorts<T>(items: T[] | undefined, format: (item: T) => string): string {
  const values = (items ?? []).slice(0, PROXY_POOL_TOOLTIP_PORTS).map(format)
  if ((items?.length ?? 0) > PROXY_POOL_TOOLTIP_PORTS) values.push('…')
  return values.join('、')
}

const proxyPoolLine = computed(() => {
  const pool = status.value?.proxy_pool
  if (pool === undefined) return null
  const parts: string[] = []
  const details: string[] = []
  const shortage = pool.mode === 'static' && pool.total < pool.capacity
  if (pool.mode === 'static') {
    parts.push(
      tf('app.proxyPoolStatic', {
        inUse: pool.in_use,
        total: pool.total,
        cooling: pool.cooling,
        shared: pool.shared,
      }),
    )
    details.push(
      tf('app.proxyPoolDetailStatic', {
        endpoint: pool.endpoint ?? '',
        start: pool.port_start ?? '',
        end: pool.port_end ?? '',
        leases: pool.leases,
        capacity: pool.capacity,
        hits: pool.affinity_hits,
        misses: pool.affinity_misses,
      }),
    )
    if (shortage) details.push(tf('app.proxyPoolDetailShortage', { capacity: pool.capacity }))
  } else if (pool.mode === 'dynamic') {
    parts.push(tf('app.proxyPoolDynamic', { inUse: pool.in_use, rotations: pool.rotations ?? 0 }))
    details.push(
      tf('app.proxyPoolDetailDynamic', {
        endpoint: pool.endpoint ?? '',
        leases: pool.leases,
        rotations: pool.rotations ?? 0,
      }),
    )
  } else {
    parts.push(t('app.proxyPoolOff'))
  }
  if (pool.stale > 0) parts.push(tf('app.proxyPoolStale', { count: pool.stale }))
  if (pool.unpooled > 0) parts.push(tf('app.proxyPoolUnpooled', { count: pool.unpooled }))
  if (pool.external > 0) parts.push(tf('app.proxyPoolExternal', { count: pool.external }))
  if (pool.orphaned > 0) parts.push(tf('app.proxyPoolOrphaned', { count: pool.orphaned }))
  if ((pool.cooling_ports ?? []).length > 0) {
    details.push(
      tf('app.proxyPoolDetailCooling', {
        ports: listPorts(pool.cooling_ports, (port) =>
          tf('app.proxyPoolDetailCoolingPort', {
            port: port.port,
            until: formatClock(port.until, locale.value),
            level: port.level,
          }),
        ),
      }),
    )
  }
  // 出口限流判定的汇总：IP 池开启后每个端口或会话是一个出口，正常的不在侧栏逐个列出（判定成立或撤销的在上方的出口行里）
  const egressSummary = status.value?.egress_summary
  if (pool.mode !== 'off' && egressSummary && egressSummary.total > 0) {
    details.push(
      tf('app.proxyPoolDetailEgress', {
        total: egressSummary.total,
        limited: egressSummary.limited,
        rejected: egressSummary.rejected,
      }),
    )
  }
  if ((pool.hot_ports ?? []).length > 0) {
    details.push(
      tf('app.proxyPoolDetailHot', {
        ports: listPorts(pool.hot_ports, (port) =>
          tf('app.proxyPoolDetailHotPort', {
            port: port.port,
            count: port.rate_limited,
            accounts: port.accounts,
          }),
        ),
      }),
    )
  }
  return {
    text: parts.join(' · '),
    detail: details.join('；'),
    warn: shortage || pool.shared > 0 || pool.orphaned > 0,
  }
})

// primarySupplyRows 为侧栏显示的主用模型供给；备用达到统计上限时显示为 1000+。
// 普通条目与 Ultra 条目（pool 为 ultra）分开：同一个模型可能同时在两份列表里，Ultra 条目显示在 Ultra Worker 行下面
const primarySupplyRows = computed(() =>
  (status.value?.primary_supply ?? []).map((supply) => ({
    ...supply,
    standbyLabel: supply.standby_capped ? `${supply.standby}+` : String(supply.standby),
  })),
)
const primarySupply = computed(() =>
  primarySupplyRows.value.filter((supply) => supply.pool !== 'ultra'),
)
// Ultra 条目与 Ultra Worker 行一样只在有 Ultra 号池时显示：ULTRA_PRIMARY_MODELS 默认非空，Ultra 号池没有账户的部署也会统计出
// 全为 0 的条目，显示出来会被误读为备用账号已经用完
const ultraPrimarySupply = computed(() =>
  ultraWorkers.value ? primarySupplyRows.value.filter((supply) => supply.pool === 'ultra') : [],
)

// limitedEgress 为侧栏的出口限流行：只显示判定成立（limited，黄色）或已撤销（rejected，灰色）的出口，正常的不显示
// （服务端已只返回非正常的出口，这里仍过滤一次以兼容旧版服务端）。IP 池的端口占用、冷却与裸 429 最多的端口在 Worker 区下方的 IP 池行里，
// 两处不重复：出口行按限流判定，IP 池行按端口与会话
const limitedEgress = computed(() =>
  (status.value?.egress ?? []).filter((egress) => egress.mode !== 'normal'),
)
// egressPending 为正在撤销或恢复判定的出口编号
const egressPending = ref('')

// egressLabelText 为出口的显示名：服务端的直连标签是中文固定值，按界面语言显示；代理出口照原样（已脱敏）
function egressLabelText(egress: EgressStatus): string {
  return egress.label === '直连' ? t('app.egressDirect') : egress.label
}

// egressLine 为一个出口的侧栏文字：上游拒绝为发往上游的请求被拒的比例，本地限速拒绝为排队超限在本地返回的 429
function egressLine(egress: EgressStatus): string {
  const ratio = Math.round((egress.ratio_2m ?? 0) * 100)
  const local = egress.local_429_1m ?? 0
  const label = egressLabelText(egress)
  if (egress.mode === 'rejected') {
    const by =
      egress.rejected_by === '安全阀'
        ? t('app.egressRejectedByValve')
        : egress.rejected_by === '管理操作'
          ? t('app.egressRejectedByAdmin')
          : (egress.rejected_by ?? '')
    return tf('app.egressRejected', { label, by })
  }
  if (egress.limiter === 'suspended') {
    return tf('app.egressSuspended', { label, ratio, local })
  }
  return tf('app.egressLimited', {
    label,
    ratio,
    local,
    queued: egress.queued ?? 0,
    limit: egressLimitText(egress),
  })
}

// egressLimitText 为出口生效的限速：只取限速生效的通道（只挂 Ultra 账号的出口、普通通道已停止而 Ultra 通道仍在限速时，
// 普通通道的 limit_rpm 为 0，原先显示“限速 0/分钟”像是完全不放行）；两条通道都在限速时分别写出
function egressLimitText(egress: EgressStatus): string {
  const normal = egress.lanes?.find((lane) => lane.lane === 'normal')?.limit_rpm ?? 0
  const ultra = egress.lanes?.find((lane) => lane.lane === 'ultra')?.limit_rpm ?? 0
  if (normal > 0 && ultra > 0) return tf('app.egressLimitLanes', { normal, ultra })
  if (ultra > 0) return tf('app.egressLimitUltra', { limit: ultra })
  if (normal > 0) return tf('app.egressLimitRPM', { limit: normal })
  return t('app.egressLimitOff')
}

function egressDetail(egress: EgressStatus): string {
  if (egress.mode !== 'rejected') return t('app.egressLimitedDetail')
  const until = egress.rejected_until ? Date.parse(egress.rejected_until) : NaN
  const minutes = Number.isNaN(until) ? 30 : Math.max(1, Math.ceil((until - Date.now()) / 60000))
  return tf('app.egressRejectedDetail', { minutes })
}

// toggleEgress 撤销（限流中）或恢复（已撤销）出口的判定，按返回的最新状态刷新侧栏
async function toggleEgress(egress: EgressStatus): Promise<void> {
  egressPending.value = egress.id
  try {
    status.value = await api.egressAction(egress.id, egress.mode === 'rejected' ? 'resume' : 'withdraw')
  } catch (error) {
    showNotice(messageOf(error), 'error')
  } finally {
    egressPending.value = ''
  }
}

// navBadge 在导航项旁显示账户就绪数与活动请求数
function navBadge(id: TabID): string {
  if (status.value === null) return ''
  if (id === 'accounts') return `${status.value.accounts.ready}/${status.value.accounts.total}`
  if (id === 'requests' && status.value.active_requests > 0) {
    return String(status.value.active_requests)
  }
  return ''
}

const statusTextColor = computed(() => {
  if (serviceState.value === 'running') return 'text-green-400'
  if (serviceState.value === 'launching') return 'text-cyan-300'
  return 'text-gray-500'
})

// messageOf 统一呈现服务端错误内容
function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : t('common.error')
}

// showNotice 显示一次短暂操作结果
function showNotice(message: string, tone: 'success' | 'error'): void {
  notice.message = message
  notice.tone = tone
  if (noticeTimer !== undefined) window.clearTimeout(noticeTimer)
  noticeTimer = window.setTimeout(() => {
    notice.message = ''
  }, 3200)
}

let statusTimer: number | undefined

// pollStatus 定时刷新状态（Worker 数量变化不会单独推送事件）；失败时保留上一次状态并提示连接异常
let statusPolling = false
const statusUnreachable = ref(false)
const eventsState = ref<EventsState>('open')

async function pollStatus(): Promise<void> {
  // 上一次还没返回时跳过本次，避免服务繁忙时请求越积越多
  if (document.visibilityState !== 'visible' || statusPolling) return
  statusPolling = true
  try {
    status.value = await api.status()
    statusUnreachable.value = false
  } catch {
    // 网络抖动时保持现有显示，同时提示显示的是上一次的状态
    statusUnreachable.value = true
  } finally {
    statusPolling = false
  }
}

async function loadStatus(): Promise<void> {
  try {
    status.value = await api.status()
  } catch {
    status.value = null
  }
}

async function loadAccounts(): Promise<void> {
  loading.accounts = accounts.value.length === 0
  errors.accounts = ''
  try {
    accounts.value = await api.accounts()
  } catch (error) {
    errors.accounts = messageOf(error)
  } finally {
    loading.accounts = false
  }
}

async function loadAccountData(): Promise<void> {
  await Promise.all([loadAccounts(), loadModels(), loadStatus()])
}

async function loadModels(): Promise<void> {
  loading.models = models.value.length === 0
  errors.models = ''
  try {
    models.value = await api.models()
  } catch (error) {
    errors.models = messageOf(error)
  } finally {
    loading.models = false
  }
}

async function loadCooldowns(): Promise<void> {
  loading.cooldowns = cooldowns.value.length === 0
  errors.cooldowns = ''
  try {
    cooldowns.value = await api.cooldowns()
  } catch (error) {
    errors.cooldowns = messageOf(error)
  } finally {
    loading.cooldowns = false
  }
}

async function loadRequests(): Promise<void> {
  loading.requests = requests.value.length === 0
  errors.requests = ''
  try {
    requests.value = await api.requests()
  } catch (error) {
    errors.requests = messageOf(error)
  } finally {
    loading.requests = false
  }
}

async function loadRequestData(): Promise<void> {
  await Promise.all([loadRequests(), loadCooldowns()])
}

async function loadConfig(): Promise<void> {
  loading.config = config.value === null
  errors.config = ''
  try {
    config.value = await api.config()
  } catch (error) {
    errors.config = messageOf(error)
  } finally {
    loading.config = false
  }
}

async function refreshAll(): Promise<void> {
  await Promise.all([
    loadStatus(),
    loadAccounts(),
    loadModels(),
    loadCooldowns(),
    loadRequests(),
    loadConfig(),
  ])
}

async function startService(): Promise<void> {
  startPending.value = true
  launchCancellationRequested.value = false
  try {
    status.value = await api.startService()
    if (launchCancellationRequested.value || status.value.state !== 'RUNNING') return
    showNotice(t('app.start'), 'success')
    await Promise.all([loadAccounts(), loadModels(), loadCooldowns()])
  } catch (error) {
    if (!launchCancellationRequested.value) showNotice(messageOf(error), 'error')
    await loadStatus()
  } finally {
    startPending.value = false
  }
}

async function stopService(): Promise<void> {
  launchCancellationRequested.value = serviceState.value === 'launching'
  stopPending.value = true
  try {
    status.value = await api.stopService()
    showNotice(t('app.stop'), 'success')
    await Promise.all([loadAccounts(), loadModels(), loadCooldowns(), loadRequests()])
  } catch (error) {
    showNotice(messageOf(error), 'error')
    await loadStatus()
  } finally {
    stopPending.value = false
  }
}

let pendingLogs: AdminLog[] = []
let logFlushTimer: number | undefined

function flushLogs(): void {
  logFlushTimer = undefined
  if (pendingLogs.length === 0) return
  logs.value.push(...pendingLogs)
  pendingLogs = []
  if (logs.value.length > LOG_LIMIT) logs.value.splice(0, logs.value.length - LOG_LIMIT)
}

async function clearLogs(): Promise<void> {
  try {
    await api.clearLogs()
    pendingLogs = []
    if (logFlushTimer !== undefined) {
      window.clearTimeout(logFlushTimer)
      logFlushTimer = undefined
    }
    logs.value = []
  } catch (error) {
    showNotice(messageOf(error), 'error')
  }
}

function replaceByID<T extends { id: string }>(items: T[], incoming: T): void {
  const index = items.findIndex((item) => item.id === incoming.id)
  if (index === -1) items.unshift(incoming)
  else items[index] = incoming
}

// trimRequests 请求事件包括已结束的请求，原先列表只增不减；超过上限时从最早的已结束请求开始丢弃
function trimRequests(items: RequestSummary[]): void {
  for (let index = items.length - 1; index >= 0 && items.length > REQUEST_LIMIT; index--) {
    const state = items[index]!.state
    if (state !== 'queued' && state !== 'running') items.splice(index, 1)
  }
}

function handleAdminEvent(event: AdminEvent): void {
  if (event.type === 'status') {
    status.value = event.data
    return
  }
  if (event.type === 'log') {
    pendingLogs.push(event.data)
    if (logFlushTimer === undefined) {
      logFlushTimer = window.setTimeout(flushLogs, 120)
    }
    return
  }
  if (event.type === 'accounts') {
    accounts.value = event.data.accounts
    return
  }
  if (event.type === 'models') {
    models.value = event.data.models
    return
  }
  if (event.type === 'cooldowns') {
    cooldowns.value = event.data
    return
  }
  replaceByID(requests.value, event.data)
  trimRequests(requests.value)
}

onMounted(async () => {
  document.title = t('app.title')
  statusTimer = window.setInterval(() => void pollStatus(), 3000)
  await refreshAll()
  eventConnection = openAdminEvents(
    handleAdminEvent,
    () => {
      pendingLogs = []
      if (logFlushTimer !== undefined) {
        window.clearTimeout(logFlushTimer)
        logFlushTimer = undefined
      }
      logs.value = []
      requests.value = []
    },
    (state) => {
      eventsState.value = state
    },
  )
})

watch(locale, () => {
  document.title = t('app.title')
})

onUnmounted(() => {
  eventConnection?.close()
  if (statusTimer !== undefined) window.clearInterval(statusTimer)
  if (logFlushTimer !== undefined) window.clearTimeout(logFlushTimer)
  if (noticeTimer !== undefined) window.clearTimeout(noticeTimer)
})
</script>

<template>
  <div class="flex h-full w-full flex-col md:flex-row">
    <aside
      class="flex min-w-0 w-full shrink-0 flex-col border-b border-[#30363d] bg-[#161b22] md:w-56 md:border-r md:border-b-0"
    >
      <div class="flex h-14 items-center justify-between gap-2 border-b border-[#30363d] px-4">
        <div class="flex min-w-0 items-center gap-2">
          <div class="h-3 w-3 shrink-0 rounded-full" :class="statusColor"></div>
          <h1 class="truncate text-lg font-bold text-white" title="AI Studio Proxy">AI Studio Proxy</h1>
        </div>
        <div class="group relative">
          <button
            class="flex shrink-0 items-center gap-1 whitespace-nowrap rounded border border-gray-700 px-1.5 py-0.5 font-mono text-xs text-gray-500 transition hover:text-white"
            type="button"
          >
            {{ locale }}
            <UiIcon name="chevronDown" :size="12" />
          </button>
          <div class="absolute top-full right-0 z-50 hidden pt-1 group-hover:block">
            <div class="overflow-hidden rounded border border-[#30363d] bg-[#161b22] shadow-xl">
              <button
                v-for="item in availableLocales"
                :key="item.code"
                class="block w-full px-4 py-2 text-left text-xs whitespace-nowrap text-gray-300 hover:bg-blue-600 hover:text-white"
                type="button"
                @click="setLocale(item.code)"
              >
                {{ item.label }}
              </button>
            </div>
          </div>
        </div>
      </div>

      <nav
        class="flex w-full min-w-0 flex-none gap-1 overflow-x-auto p-2 md:flex-1 md:flex-col md:space-y-1"
      >
        <button
          v-for="item in navigation"
          :key="item.id"
          :class="[
            'flex w-auto shrink-0 items-center gap-2 rounded-md px-3 py-2 text-left whitespace-nowrap transition md:w-full',
            currentTab === item.id
              ? 'bg-blue-600 text-white'
              : 'text-gray-400 hover:bg-[#21262d] hover:text-white',
          ]"
          type="button"
          @click="currentTab = item.id"
        >
          <UiIcon :name="item.icon" :size="16" />
          {{ t(item.label) }}
          <span
            v-if="navBadge(item.id) !== ''"
            class="ml-auto rounded bg-black/20 px-1.5 font-mono text-[11px] leading-5 opacity-80"
          >
            {{ navBadge(item.id) }}
          </span>
        </button>
      </nav>

      <div
        class="relative min-w-0 overflow-hidden border-t border-[#30363d] p-2 md:p-4"
        :class="serviceState === 'launching' ? 'launching-shell' : ''"
      >
        <div v-if="serviceState === 'launching'" class="launching-scan" aria-hidden="true"></div>
        <div class="mb-2 text-xs text-gray-500">{{ t('app.status') }}</div>
        <div class="mb-1 flex items-center justify-between">
          <span class="font-mono font-bold" :class="statusTextColor">
            {{ serviceState.toUpperCase() }}
          </span>
          <span
            v-tooltip="status?.version || ''"
            class="max-w-[65%] truncate font-mono text-xs text-gray-500"
          >
            {{ status?.version || '—' }}
          </span>
        </div>
        <div v-if="status" class="mb-3 space-y-0.5 font-mono text-[11px] text-gray-500">
          <div>
            {{ tf('app.readyOf', { ready: status.accounts.ready, total: status.accounts.total }) }}
          </div>
          <div>{{ tf('app.activeRequests', { count: status.active_requests }) }}</div>
          <div v-if="status.workers" class="flex items-center gap-2">
            <span>{{
              tf('app.workers', { warm: status.workers.warm, target: status.workers.target })
            }}</span>
            <span v-if="status.workers.starting > 0" class="text-cyan-300">
              {{ tf('app.workersStarting', { count: status.workers.starting }) }}
            </span>
          </div>
          <div
            v-if="status.workers"
            :class="(status.workers.occupied ?? 0) >= status.workers.max ? 'text-yellow-400' : ''"
          >
            {{
              tf('app.workerCapacity', {
                occupied: status.workers.occupied ?? 0,
                max: status.workers.max,
              })
            }}
          </div>
          <div
            v-if="
              status.workers &&
              ((status.workers.dead ?? 0) > 0 ||
                (status.workers.draining ?? 0) > 0 ||
                (status.workers.unschedulable ?? 0) > 0)
            "
            class="flex items-center gap-2 text-yellow-400"
          >
            <span
              v-if="(status.workers.dead ?? 0) > 0 || (status.workers.draining ?? 0) > 0"
              v-tooltip="t('app.workersDeadDetail')"
            >
              {{ tf('app.workersDead', { count: status.workers.dead ?? 0 }) }}
            </span>
            <span
              v-if="(status.workers.draining ?? 0) > 0"
              v-tooltip="t('app.workersDrainingDetail')"
              class="text-cyan-300"
            >
              {{ tf('app.workersDraining', { count: status.workers.draining ?? 0 }) }}
            </span>
            <span
              v-if="(status.workers.unschedulable ?? 0) > 0"
              v-tooltip="t('app.workersUnschedulableDetail')"
            >
              {{ tf('app.workersUnschedulable', { count: status.workers.unschedulable ?? 0 }) }}
            </span>
          </div>
          <!-- 模型名过长时数量换到下一行，不被截断 -->
          <div
            v-for="supply in primarySupply"
            :key="supply.model"
            v-tooltip="tf('app.primarySupplyDetail', { model: supply.model })"
            class="flex flex-wrap items-baseline gap-x-1"
            :class="supply.standby === 0 ? 'text-yellow-400' : ''"
          >
            <span class="max-w-full min-w-0 truncate">
              {{ tf('app.primarySupplyModel', { model: supply.model }) }}
            </span>
            <span class="whitespace-nowrap">
              {{
                tf('app.primarySupply', {
                  warm: supply.warm,
                  cooling: supply.cooling ?? 0,
                  standby: supply.standbyLabel,
                })
              }}
            </span>
          </div>
          <div
            v-for="egress in limitedEgress"
            :key="`egress:${egress.id}`"
            v-tooltip="egressDetail(egress)"
            class="flex flex-wrap items-baseline gap-x-1"
            :class="egress.mode === 'rejected' ? 'text-gray-500' : 'text-yellow-400'"
          >
            <span class="max-w-full min-w-0 break-all">{{ egressLine(egress) }}</span>
            <button
              type="button"
              class="rounded border border-gray-600 px-1 text-[10px] text-gray-300 transition hover:bg-gray-700 disabled:opacity-50"
              :disabled="egressPending === egress.id"
              @click="toggleEgress(egress)"
            >
              {{ egress.mode === 'rejected' ? t('app.egressResume') : t('app.egressWithdraw') }}
            </button>
          </div>
          <div
            v-if="ultraWorkers"
            v-tooltip="
              tf('app.ultraWorkersDetail', {
                warm: ultraWorkers.warm,
                starting: ultraWorkers.starting,
                dead: ultraWorkers.dead ?? 0,
                draining: ultraWorkers.draining ?? 0,
                unschedulable: ultraWorkers.unschedulable ?? 0,
                target: ultraWorkers.target,
                max: ultraWorkers.max,
              })
            "
            :class="ultraWorkers.occupied >= ultraWorkers.max ? 'text-yellow-400' : 'text-fuchsia-300'"
          >
            {{ tf('app.ultraWorkers', { occupied: ultraWorkers.occupied, max: ultraWorkers.max }) }}
          </div>
          <div
            v-for="supply in ultraPrimarySupply"
            :key="`ultra:${supply.model}`"
            v-tooltip="tf('app.ultraPrimarySupplyDetail', { model: supply.model })"
            class="flex flex-wrap items-baseline gap-x-1"
            :class="supply.standby === 0 ? 'text-yellow-400' : 'text-fuchsia-300'"
          >
            <span class="max-w-full min-w-0 truncate">
              {{ tf('app.ultraPrimarySupplyModel', { model: supply.model }) }}
            </span>
            <span class="whitespace-nowrap">
              {{
                tf('app.primarySupply', {
                  warm: supply.warm,
                  cooling: supply.cooling ?? 0,
                  standby: supply.standbyLabel,
                })
              }}
            </span>
          </div>
          <div
            v-if="authRecheckLine"
            v-tooltip="authRecheckLine.detail"
            class="cell-truncate"
            :class="authRecheckLine.warn ? 'text-yellow-400' : ''"
          >
            {{ authRecheckLine.text }}
          </div>
          <div
            v-if="catalogSyncLine"
            v-tooltip="t('app.catalogSyncDetail')"
            class="cell-truncate"
            :class="catalogSyncLine.paused ? 'text-yellow-400' : ''"
          >
            {{ catalogSyncLine.text }}
          </div>
          <template v-if="status.workers?.prewarm">
            <div v-if="status.workers.prewarm.active" class="text-cyan-300">
              {{
                tf('app.prewarmActive', {
                  inflight: status.workers.prewarm.inflight,
                  launched: status.workers.prewarm.launched,
                })
              }}
            </div>
            <div
              v-if="status.workers.prewarm.active && status.workers.prewarm.loop_age_seconds > 15"
              class="text-yellow-400"
            >
              {{ tf('app.prewarmStalled', { seconds: status.workers.prewarm.loop_age_seconds }) }}
            </div>
            <div
              v-if="!status.workers.prewarm.active && status.workers.warm < status.workers.target"
              v-tooltip="status.workers.prewarm.reason || ''"
              class="cell-truncate"
            >
              {{ t('app.prewarmIdle')
              }}<template v-if="status.workers.prewarm.reason">
                · {{ status.workers.prewarm.reason }}</template
              >
            </div>
          </template>
          <div v-if="status.workers && status.workers.target > 0" class="progress-track mt-1">
            <div
              class="progress-bar"
              :style="{
                width: `${Math.min(100, Math.round((status.workers.warm / status.workers.target) * 100))}%`,
              }"
            ></div>
          </div>
          <div
            v-if="proxyPoolLine"
            v-tooltip="proxyPoolLine.detail"
            class="mt-1"
            :class="proxyPoolLine.warn ? 'text-yellow-400' : 'text-sky-300'"
          >
            {{ proxyPoolLine.text }}
          </div>
        </div>
        <div v-else class="mb-3"></div>
        <button
          v-if="serviceState === 'stopped'"
          class="flex w-full items-center justify-center gap-2 rounded bg-green-600 py-2 font-bold text-white shadow transition hover:bg-green-500 disabled:opacity-50"
          type="button"
          :disabled="startPending || stopPending"
          @click="startService"
        >
          <UiIcon :name="startPending ? 'spinner' : 'play'" :size="16" />
          {{ t('app.start') }}
        </button>
        <button
          v-else-if="serviceState === 'launching' || serviceState === 'running'"
          class="flex w-full items-center justify-center gap-2 rounded bg-red-600 py-2 font-bold text-white shadow transition hover:bg-red-500 disabled:opacity-50"
          type="button"
          :disabled="stopPending"
          @click="stopService"
        >
          <UiIcon :name="stopPending ? 'spinner' : 'stop'" :size="16" />
          {{ t('app.stop') }}
        </button>
      </div>
    </aside>

    <main class="flex min-w-0 flex-1 flex-col bg-[#0d1117]">
      <div
        v-if="eventsState !== 'open' || statusUnreachable"
        class="flex items-center gap-2 border-b border-yellow-700/60 bg-yellow-900/30 px-4 py-2 text-sm text-yellow-200"
        role="status"
      >
        <UiIcon :name="eventsState === 'reconnecting' ? 'spinner' : 'info'" :size="14" />
        <span v-if="eventsState === 'closed'">{{ t('app.eventsClosed') }}</span>
        <span v-else-if="eventsState === 'reconnecting'">{{ t('app.eventsReconnecting') }}</span>
        <span v-else>{{ t('app.statusUnreachable') }}</span>
      </div>
      <!-- API 试用页切走后保留组件：进行中的请求不中止，回来时结果仍在；其他面板照旧卸载 -->
      <KeepAlive include="PlaygroundPanel">
        <LogsPanel v-if="currentTab === 'logs'" :logs="logs" @clear="clearLogs" />
        <AccountsPanel
          v-else-if="currentTab === 'accounts'"
          :accounts="accounts"
          :loading="loading.accounts"
          :error="errors.accounts"
          :worker-sets="workerSets"
          :worker-counts="status?.workers ?? null"
          :ultra-worker-counts="status?.ultra_workers ?? null"
          :proxy-pool="status?.proxy_pool ?? null"
          :cooldowns="cooldowns"
          @refresh="loadAccountData"
          @notice="showNotice"
        />
        <ModelsTable
          v-else-if="currentTab === 'models'"
          :models="models"
          :loading="loading.models"
          :error="errors.models"
        />
        <RequestsPanel
          v-else-if="currentTab === 'requests'"
          :accounts="accounts"
          :cooldowns="cooldowns"
          :requests="requests"
          :loading="loading.requests || loading.cooldowns"
          :cooldown-error="errors.cooldowns"
          :request-error="errors.requests"
          @refresh="loadRequestData"
          @notice="showNotice"
        />
        <UsagePanel v-else-if="currentTab === 'usage'" />
        <SettingsPanel
          v-else-if="currentTab === 'settings'"
          :config="config"
          :loading="loading.config"
          :error="errors.config"
          :running="serviceState === 'running' || serviceState === 'launching'"
          :models="models"
          :proxy-pool-status="status?.proxy_pool ?? null"
          @saved="config = $event"
          @notice="showNotice"
        />
        <PlaygroundPanel
          v-else
          :models="models"
          :accounts="accounts"
          :api-key="config?.proxy_api_key ?? ''"
        />
      </KeepAlive>
    </main>

    <Transition name="notice">
      <div
        v-if="notice.message"
        class="fixed right-5 bottom-5 z-50 max-w-md rounded border bg-[#161b22] px-4 py-3 text-sm shadow-xl"
        :class="
          notice.tone === 'error'
            ? 'border-red-500/50 text-red-300'
            : 'border-green-500/50 text-green-300'
        "
      >
        {{ notice.message }}
      </div>
    </Transition>
    <UiConfirm />
  </div>
</template>

<style scoped>
.launching-shell {
  background: radial-gradient(circle at 12% 0%, rgb(34 211 238 / 14%), transparent 55%), #161b22;
}

.launching-scan {
  position: absolute;
  top: 0;
  left: -45%;
  width: 45%;
  height: 1px;
  background: linear-gradient(90deg, transparent, rgb(103 232 249), transparent);
  animation: launching-scan 1.25s ease-in-out infinite;
}

@keyframes launching-scan {
  to {
    transform: translateX(320%);
  }
}
</style>
