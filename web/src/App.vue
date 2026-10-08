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
          <div class="h-3 w-3 rounded-full" :class="statusColor"></div>
          <h1 class="whitespace-nowrap text-lg font-bold text-white">AI Studio Proxy</h1>
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
      <LogsPanel v-if="currentTab === 'logs'" :logs="logs" @clear="clearLogs" />
      <AccountsPanel
        v-else-if="currentTab === 'accounts'"
        :accounts="accounts"
        :loading="loading.accounts"
        :error="errors.accounts"
        :worker-sets="workerSets"
        :worker-counts="status?.workers ?? null"
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
        @saved="config = $event"
        @notice="showNotice"
      />
      <PlaygroundPanel v-else :models="models" :api-key="config?.proxy_api_key ?? ''" />
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
