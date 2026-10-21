<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { api } from '@/api'
import {
  emptyBulkState,
  runBulk,
  runChunkedBulk,
  type BulkAction,
  type BulkChunkResult,
  type BulkFailure,
  type BulkState,
  type BulkTarget,
} from '@/bulk'
import { copyText } from '@/clipboard'
import { confirmAction } from '@/confirm'
import { channelLabelKey, useI18n, type TranslationKey } from '@/i18n'
import { usePaging } from '@/paging'
import type {
  Account,
  AccountDraft,
  AccountState,
  ChromeImportProfile,
  Cooldown,
  PermissionDeniedAccount,
  StartupFailureAccount,
  StartupFailureKind,
  StartupStatus,
  UltraWorkerCounters,
  WorkerCounters,
  WorkerState,
} from '@/types'
import OnboardingPanel from './OnboardingPanel.vue'
import UiIcon from './UiIcon.vue'
import UiPager from './UiPager.vue'

const props = defineProps<{
  accounts: Account[]
  loading: boolean
  error: string
  workerSets: { warm: Set<string>; starting: Set<string> }
  workerCounts: WorkerCounters | null
  ultraWorkerCounts: UltraWorkerCounters | null
  cooldowns: Cooldown[]
  // startup 为状态接口的冷启动统计，暂停中与失效账户数变化时重新读取启动失败列表；旧版服务端没有
  startup: StartupStatus | null
}>()

const emit = defineEmits<{
  refresh: []
  notice: [message: string, tone: 'success' | 'error']
}>()

const { t, tf } = useI18n()
const defaultAccountLocale = navigator.language || 'en-US'
const defaultAccountTimezone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'

// ---------- 模型冷却概况 ----------

interface CooldownGroup {
  key: string
  model: string
  channel: Cooldown['channel']
  accounts: number
  earliest: number
  latest: number
  reason: string
}

// 每秒刷新一次倒计时
const clock = ref(Date.now())
let clockTimer: number | undefined
onMounted(() => {
  clockTimer = window.setInterval(() => {
    clock.value = Date.now()
  }, 1000)
})
onBeforeUnmount(() => {
  if (clockTimer !== undefined) window.clearInterval(clockTimer)
})

const parsedCooldowns = computed(() =>
  props.cooldowns
    .map((cooldown) => ({ cooldown, until: Date.parse(cooldown.until) }))
    .filter((item) => Number.isFinite(item.until)),
)

// cooldownSummary 按模型与通道汇总仍在冷却中的账号：账号数、最早与最晚恢复时间、最常见原因
const cooldownSummary = computed(() => {
  const now = clock.value
  const accountIDs = new Set<string>()
  const groups = new Map<string, { group: CooldownGroup; ids: Set<string>; reasons: Map<string, number> }>()
  let entries = 0
  let earliest = Number.POSITIVE_INFINITY
  let latest = 0
  for (const { cooldown, until } of parsedCooldowns.value) {
    if (until <= now) continue
    entries += 1
    accountIDs.add(cooldown.account_id)
    earliest = Math.min(earliest, until)
    latest = Math.max(latest, until)
    const key = `${cooldown.channel}:${cooldown.model_id}`
    let entry = groups.get(key)
    if (!entry) {
      entry = {
        group: { key, model: cooldown.model_id, channel: cooldown.channel, accounts: 0, earliest: until, latest: until, reason: '' },
        ids: new Set<string>(),
        reasons: new Map<string, number>(),
      }
      groups.set(key, entry)
    }
    entry.ids.add(cooldown.account_id)
    entry.group.earliest = Math.min(entry.group.earliest, until)
    entry.group.latest = Math.max(entry.group.latest, until)
    const reason = (cooldown.reason ?? '').trim()
    if (reason) entry.reasons.set(reason, (entry.reasons.get(reason) ?? 0) + 1)
  }
  const list: CooldownGroup[] = []
  for (const { group, ids, reasons } of groups.values()) {
    let topReason = ''
    let topCount = 0
    for (const [reason, count] of reasons) {
      if (count > topCount) {
        topReason = reason
        topCount = count
      }
    }
    list.push({ ...group, accounts: ids.size, reason: topReason })
  }
  list.sort((left, right) => right.accounts - left.accounts || left.earliest - right.earliest)
  return { accounts: accountIDs.size, entries, earliest, latest, groups: list }
})

const showAllCooldowns = ref(false)
const visibleCooldownGroups = computed(() =>
  showAllCooldowns.value ? cooldownSummary.value.groups : cooldownSummary.value.groups.slice(0, 6),
)

// formatRemaining 把距恢复的剩余时间格式化为"X 小时 Y 分" / "X 分 Y 秒" / "X 秒"
function formatRemaining(until: number): string {
  const seconds = Math.max(0, Math.ceil((until - clock.value) / 1000))
  if (seconds <= 0) return t('accounts.cooldownSoon')
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const rest = seconds % 60
  if (hours > 0) return tf('accounts.cooldownHours', { h: hours, m: minutes })
  if (minutes > 0) return tf('accounts.cooldownMinutes', { m: minutes, s: rest })
  return tf('accounts.cooldownSeconds', { s: rest })
}

function formatClock(until: number): string {
  return new Date(until).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false })
}

// ---------- 403 无权限账户 ----------

// 列表的最短刷新间隔：账户与冷却数据在每个请求结束后都会推送，列表不随它们高频请求，最多每 15 秒一次
const PERMISSION_DENIED_REFRESH_MS = 15000
// 收起时显示的账户数
const PERMISSION_DENIED_COLLAPSED = 20

const permissionDenied = ref<PermissionDeniedAccount[]>([])
const showAllPermissionDenied = ref(false)
const permissionDeniedClearing = ref(false)
let permissionDeniedLoadedAt = 0
let permissionDeniedLoading = false
let permissionDeniedTimer: number | undefined
// permissionDeniedVersion 在清除记录或一键删除后递增：之前发出、之后才返回的读取结果不再覆盖列表
let permissionDeniedVersion = 0

const visiblePermissionDenied = computed(() =>
  showAllPermissionDenied.value
    ? permissionDenied.value
    : permissionDenied.value.slice(0, PERMISSION_DENIED_COLLAPSED),
)

// loadPermissionDenied 读取 403 无权限账户列表；失败时保留上一次的列表（旧版服务没有这个接口时不显示）
async function loadPermissionDenied(): Promise<void> {
  if (permissionDeniedLoading) return
  permissionDeniedLoading = true
  permissionDeniedLoadedAt = Date.now()
  const version = permissionDeniedVersion
  try {
    const accounts = await api.permissionDenied()
    if (version === permissionDeniedVersion) permissionDenied.value = accounts
  } catch {
    // 保留上一次的列表
  } finally {
    permissionDeniedLoading = false
  }
}

// schedulePermissionDeniedLoad 账户或冷却数据变化后刷新列表；距上次刷新不到 15 秒时推迟到满 15 秒，期间的变化合并为一次
function schedulePermissionDeniedLoad(): void {
  if (permissionDeniedTimer !== undefined) return
  const wait = Math.max(0, permissionDeniedLoadedAt + PERMISSION_DENIED_REFRESH_MS - Date.now())
  permissionDeniedTimer = window.setTimeout(() => {
    permissionDeniedTimer = undefined
    void loadPermissionDenied()
  }, wait)
}

watch([() => props.accounts, () => props.cooldowns], schedulePermissionDeniedLoad)
onMounted(() => {
  void loadPermissionDenied()
})
onBeforeUnmount(() => {
  if (permissionDeniedTimer !== undefined) window.clearTimeout(permissionDeniedTimer)
})

// permissionDeniedModels 把账户的 403 模型格式化为"模型（通道）"，按最近一次从新到旧
function permissionDeniedModels(account: PermissionDeniedAccount): string {
  return account.models
    .map((model) => {
      const channels = model.channels.map((channel) => t(channelLabelKey(channel))).join('/')
      return channels === '' ? model.model : `${model.model} (${channels})`
    })
    .join(', ')
}

// permissionDeniedDetail 为模型一栏的悬停提示：每个模型的次数与上游原文
function permissionDeniedDetail(account: PermissionDeniedAccount): string {
  return account.models
    .map((model) => `${model.model} ×${model.count}${model.message ? `: ${model.message}` : ''}`)
    .join('\n')
}

function formatDateTime(value: string): string {
  const time = Date.parse(value)
  if (!Number.isFinite(time)) return '—'
  return new Date(time).toLocaleString([], {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })
}

// downloadEmails 下载账户邮箱列表（txt，每行一个，与复制的内容相同），文件名为“前缀-年月日-时分.txt”
function downloadEmails(labels: readonly string[], prefix: string): void {
  if (labels.length === 0) return
  const text = `${labels.join('\n')}\n`
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }))
  const now = new Date()
  const pad = (value: number) => String(value).padStart(2, '0')
  const stamp = `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}`
  const link = document.createElement('a')
  link.href = url
  link.download = `${prefix}-${stamp}.txt`
  document.body.appendChild(link)
  link.click()
  link.remove()
  window.setTimeout(() => URL.revokeObjectURL(url), 1000)
}

// downloadPermissionDenied 下载 403 无权限账户的邮箱列表
function downloadPermissionDenied(): void {
  downloadEmails(
    permissionDenied.value.map((account) => account.label),
    'permission-denied-accounts',
  )
}

// clearPermissionDenied 清除全部 403 无权限账户记录（只清除列表，不解除冷却）
async function clearPermissionDenied(): Promise<void> {
  if (
    !(await confirmAction(
      t('accounts.permissionDeniedClearConfirm'),
      t('accounts.permissionDeniedClear'),
    ))
  )
    return
  permissionDeniedClearing.value = true
  try {
    const result = await api.clearPermissionDenied({ all: true })
    permissionDeniedVersion += 1
    permissionDenied.value = []
    showAllPermissionDenied.value = false
    emit('notice', tf('accounts.permissionDeniedCleared', { count: result.cleared }), 'success')
  } catch (error) {
    actionError(error)
  } finally {
    permissionDeniedClearing.value = false
  }
}

// ---------- 启动失败账户 ----------

// 列表的最短刷新间隔与 403 列表相同：账户或启动统计变化时最多每 15 秒读取一次
const STARTUP_FAILURES_REFRESH_MS = 15000
// 收起时显示的账户数
const STARTUP_FAILURES_COLLAPSED = 20

const startupFailures = ref<StartupFailureAccount[]>([])
const showAllStartupDead = ref(false)
const showAllStartupPaused = ref(false)
const startupDeadClearing = ref(false)
// startupPauseClearing 为正在清除暂停的账户 ID
const startupPauseClearing = ref('')
let startupFailuresLoadedAt = 0
let startupFailuresLoading = false
let startupFailuresTimer: number | undefined
// startupFailuresVersion 在清除记录、清除暂停或一键删除后递增：之前发出、之后才返回的读取结果不再覆盖列表
let startupFailuresVersion = 0

// startupDead 为确定失效的账户（登录失效、认证文件无法使用，或号池健康时连续页面受阻），可以一键删除；
// startupPaused 为启动反复失败、原因不确定、指纹文件损坏或页面受阻还没确认的账户，只暂停调度，只读，不提供删除
const startupDead = computed(() => startupFailures.value.filter((account) => account.confirmed))
const startupPaused = computed(() => startupFailures.value.filter((account) => !account.confirmed))
// startupDeadPageBlocked 为确认为页面受阻的失效账户数：账户状态仍是就绪，卡片另给检查与删除的建议
const startupDeadPageBlocked = computed(
  () => startupDead.value.filter((account) => account.kind === 'page_blocked').length,
)
// startupPageBlockedBurst 为大量账户同时页面受阻（可能是官网页面改版）：服务端暂不确认新的页面受阻失效，
// 卡片顶部显示黄色提示，一键删除的确认框另加警告；旧版服务端没有这个字段
const startupPageBlockedBurst = computed(() => props.startup?.page_blocked_burst === true)
const visibleStartupDead = computed(() =>
  showAllStartupDead.value
    ? startupDead.value
    : startupDead.value.slice(0, STARTUP_FAILURES_COLLAPSED),
)
const visibleStartupPaused = computed(() =>
  showAllStartupPaused.value
    ? startupPaused.value
    : startupPaused.value.slice(0, STARTUP_FAILURES_COLLAPSED),
)
const accountsByID = computed(() => new Map(props.accounts.map((account) => [account.id, account])))

// pauseClock 为判断暂停是否到期用的时钟，每 10 秒变化一次：账户表不随每秒的倒计时重新渲染
const pauseClock = computed(() => Math.floor(clock.value / 10000) * 10000)

// startupPauses 为仍在暂停中的启动失败（账户 ID → 记录与到期时间），供账户表 Worker 列显示；已到期的不再显示。
// 确认为页面受阻的账户状态仍是就绪、继续按 30 分钟暂停重试，带 until 时同样显示；登录失效等持久记录没有 until，不显示
const startupPauses = computed(() => {
  const now = pauseClock.value
  const pauses = new Map<string, { entry: StartupFailureAccount; until: number }>()
  for (const entry of startupFailures.value) {
    const until = Date.parse(entry.until ?? '')
    if (Number.isFinite(until) && until > now) pauses.set(entry.id, { entry, until })
  }
  return pauses
})

// loadStartupFailures 读取启动失败账户列表；失败时保留上一次的列表（旧版服务没有这个接口时不显示）
async function loadStartupFailures(): Promise<void> {
  if (startupFailuresLoading) return
  startupFailuresLoading = true
  startupFailuresLoadedAt = Date.now()
  const version = startupFailuresVersion
  try {
    const accounts = await api.startupFailures()
    if (version === startupFailuresVersion) startupFailures.value = accounts
  } catch {
    // 保留上一次的列表
  } finally {
    startupFailuresLoading = false
  }
}

// readStartupFailuresNow 立即重新读取启动失败账户列表（不受 15 秒刷新间隔限制）；读取失败时保留当前列表并抛出错误
async function readStartupFailuresNow(): Promise<void> {
  const accounts = await api.startupFailures()
  // 之前发出、之后才返回的读取结果不再覆盖这次读取
  startupFailuresVersion += 1
  startupFailures.value = accounts
  startupFailuresLoadedAt = Date.now()
}

// refreshStartupFailures 在清除或验证之后立即刷新列表；读取失败时保留当前列表，等下一次定时刷新
function refreshStartupFailures(): void {
  readStartupFailuresNow().catch(() => {})
}

// scheduleStartupFailuresLoad 账户或启动统计变化后刷新列表；距上次刷新不到 15 秒时推迟到满 15 秒，期间的变化合并为一次
function scheduleStartupFailuresLoad(): void {
  if (startupFailuresTimer !== undefined) return
  const wait = Math.max(0, startupFailuresLoadedAt + STARTUP_FAILURES_REFRESH_MS - Date.now())
  startupFailuresTimer = window.setTimeout(() => {
    startupFailuresTimer = undefined
    void loadStartupFailures()
  }, wait)
}

// 原因不确定与页面受阻的启动失败不改变账户状态，不会推送账户事件，所以也跟随状态接口的暂停中、失效账户数与页面受阻爆发刷新
watch(
  [
    () => props.accounts,
    () => props.startup?.paused,
    () => props.startup?.dead,
    () => props.startup?.page_blocked_burst,
  ],
  scheduleStartupFailuresLoad,
)
onMounted(() => {
  void loadStartupFailures()
})
onBeforeUnmount(() => {
  if (startupFailuresTimer !== undefined) window.clearTimeout(startupFailuresTimer)
})

const startupKindKeys: Record<StartupFailureKind, TranslationKey> = {
  auth: 'accounts.startupFailureKindAuth',
  files: 'accounts.startupFailureKindFiles',
  fingerprint: 'accounts.startupFailureKindFingerprint',
  page_blocked: 'accounts.startupFailureKindPageBlocked',
  ambiguous: 'accounts.startupFailureKindAmbiguous',
}

// startupKindLabel 返回启动失败类别的名称；旧版或更新的服务端返回未知类别时原样显示
function startupKindLabel(entry: StartupFailureAccount): string {
  const key = startupKindKeys[entry.kind] as TranslationKey | undefined
  return key === undefined ? entry.kind : t(key)
}

// startupFailureReason 为原因一栏：类别名加原因摘要（没有摘要时用错误原文），过长时截断，完整内容见悬停提示。
// 确认为页面受阻的原因说明各账户相同（卡片另有说明），改为优先显示错误摘要（填不进提示词还是 Run 按钮不可用）；
// 未确认的页面受阻仍先显示原因（带失效确认的进度）
function startupFailureReason(entry: StartupFailureAccount): string {
  const reasonText = (entry.reason ?? '').trim()
  const messageText = (entry.message ?? '').trim()
  const reason =
    entry.kind === 'page_blocked' && entry.confirmed
      ? messageText || reasonText
      : reasonText || messageText
  const kind = startupKindLabel(entry)
  return reason === '' ? kind : `${kind} · ${reason}`
}

// startupFailureDetail 为原因一栏的悬停提示：类别、原因、错误原文、登录跳转地址与首次失败时间
function startupFailureDetail(entry: StartupFailureAccount): string {
  const lines = [startupKindLabel(entry)]
  const reason = (entry.reason ?? '').trim()
  const message = (entry.message ?? '').trim()
  if (reason !== '') lines.push(reason)
  if (message !== '' && message !== reason) lines.push(message)
  if (entry.url) lines.push(tf('accounts.startupFailureURL', { url: entry.url }))
  if (entry.first_at) {
    lines.push(tf('accounts.startupFailureFirst', { time: formatDateTime(entry.first_at) }))
  }
  return lines.join('\n')
}

// startupStage 把失败时的启动阶段格式化为“n/7”，未知时为“—”
function startupStage(entry: StartupFailureAccount): string {
  const stage = entry.stage ?? 0
  return stage > 0 ? `${stage}/7` : '—'
}

// startupPausedUntil 为未确认列表的暂停到期时间；已到期的显示“已到期”（下次启动失败前不再暂停）
function startupPausedUntil(entry: StartupFailureAccount): string {
  const until = Date.parse(entry.until ?? '')
  if (!Number.isFinite(until)) return '—'
  if (until <= pauseClock.value) return t('accounts.startupPausedExpired')
  return formatClock(until)
}

function formatHourMinute(until: number): string {
  return new Date(until).toLocaleTimeString([], {
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  })
}

// startupPauseText 为账户表 Worker 列的“启动失败×N（暂停至 hh:mm）”；账户不在暂停中时为空
function startupPauseText(account: Account): string {
  const pause = startupPauses.value.get(account.id)
  if (pause === undefined) return ''
  return tf('accounts.workerStartPaused', {
    count: pause.entry.count,
    time: formatHourMinute(pause.until),
  })
}

function startupPauseDetail(account: Account): string {
  const pause = startupPauses.value.get(account.id)
  return pause === undefined ? '' : startupFailureDetail(pause.entry)
}

// clearStartupDead 清除列表中全部失效账户的记录：只清除记录，不删除账户，也不改变账户状态，
// 仍处于需要重新登录状态的账户会继续列出。只提交确认时列出的确定失效账户，不用 all：all 会同时清除下方未确认账户的暂停
async function clearStartupDead(): Promise<void> {
  const ids = startupDead.value.map((account) => account.id)
  if (ids.length === 0) return
  if (
    !(await confirmAction(
      tf('accounts.startupDeadClearConfirm', { count: ids.length }),
      t('accounts.startupDeadClear'),
    ))
  )
    return
  startupDeadClearing.value = true
  try {
    const result = await api.clearStartupFailures({ account_ids: ids, all: false })
    const cleared = new Set(ids)
    startupFailuresVersion += 1
    startupFailures.value = startupFailures.value.filter((account) => !cleared.has(account.id))
    showAllStartupDead.value = false
    emit('notice', tf('accounts.startupDeadCleared', { count: result.cleared }), 'success')
  } catch (error) {
    actionError(error)
  } finally {
    startupDeadClearing.value = false
  }
  refreshStartupFailures()
}

// clearStartupPause 清除一个账户的启动失败暂停：服务端同时唤醒调度，账户立即重新参与调度，再次启动失败时重新计数
async function clearStartupPause(entry: StartupFailureAccount): Promise<void> {
  if (startupPauseClearing.value !== '') return
  startupPauseClearing.value = entry.id
  try {
    await api.clearStartupFailures({ account_ids: [entry.id], all: false })
    startupFailuresVersion += 1
    startupFailures.value = startupFailures.value.filter((account) => account.id !== entry.id)
    emit('notice', tf('accounts.startupPausedCleared', { label: entry.label }), 'success')
  } catch (error) {
    actionError(error)
  } finally {
    startupPauseClearing.value = ''
  }
  refreshStartupFailures()
}

// verifyStartupPaused 验证一个启动反复失败的账户，与账户列表里的“验证”相同：验证会另开浏览器，通过时服务端清除它的暂停
async function verifyStartupPaused(entry: StartupFailureAccount): Promise<void> {
  const account = accountsByID.value.get(entry.id)
  if (account === undefined) return
  await runAccountAction(account, 'verify')
  refreshStartupFailures()
}

// ---------- 弹窗与单账户操作 ----------

const showEditor = ref(false)
const showBrowserLogin = ref(false)
const showChromeImport = ref(false)
const editingAccountID = ref('')
const pendingAction = ref('')
const chromeProfiles = ref<ChromeImportProfile[]>([])
const selectedChromeProfiles = ref<string[]>([])
const allChromeProfilesSelected = computed(
  () =>
    chromeProfiles.value.length > 0 &&
    selectedChromeProfiles.value.length === chromeProfiles.value.length,
)
const accountEnvironment = reactive({
  proxy: '',
  locale: defaultAccountLocale,
  timezone: defaultAccountTimezone,
})
const draft = reactive<AccountDraft>({
  label: '',
  enabled: true,
  proxy: '',
  locale: defaultAccountLocale,
  timezone: defaultAccountTimezone,
})

const stateKeys: Record<AccountState, TranslationKey> = {
  ready: 'state.ready',
  busy: 'state.busy',
  cooldown: 'state.cooldown',
  auth_required: 'state.auth_required',
  unavailable: 'state.unavailable',
  disabled: 'state.disabled',
}

const stateDotClass: Record<AccountState, string> = {
  ready: 'bg-green-500',
  busy: 'bg-blue-500',
  cooldown: 'bg-yellow-500',
  auth_required: 'bg-red-500',
  unavailable: 'bg-red-500',
  disabled: 'bg-gray-600',
}

const stateTextClass: Record<AccountState, string> = {
  ready: 'text-green-400',
  busy: 'text-blue-400',
  cooldown: 'text-yellow-400',
  auth_required: 'text-red-400',
  unavailable: 'text-red-400',
  disabled: 'text-gray-500',
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : t('common.error')
}

// actionError 将账户写操作错误发送到全局通知
function actionError(error: unknown): void {
  emit('notice', messageOf(error), 'error')
}

function toggleAllChromeProfiles(): void {
  selectedChromeProfiles.value = allChromeProfilesSelected.value
    ? []
    : chromeProfiles.value.map((profile) => profile.id)
}

function beginEdit(account: Account): void {
  editingAccountID.value = account.id
  draft.label = account.label
  draft.enabled = account.enabled
  draft.proxy = account.proxy
  draft.locale = account.locale
  draft.timezone = account.timezone
  showEditor.value = true
}

function closeEditor(): void {
  showEditor.value = false
  editingAccountID.value = ''
}

function openBrowserLogin(): void {
  showBrowserLogin.value = true
}

function closeBrowserLogin(): void {
  if (pendingAction.value === 'browser-login') return
  showBrowserLogin.value = false
}

// beginBrowserLogin 启动浏览器登录并使用返回身份创建账户
async function beginBrowserLogin(): Promise<void> {
  pendingAction.value = 'browser-login'
  try {
    await api.createAccount({ ...accountEnvironment })
    showBrowserLogin.value = false
    emit('refresh')
    emit('notice', t('accounts.loginComplete'), 'success')
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

// openChromeImport 读取本机可导入的 Chrome 账户
async function openChromeImport(): Promise<void> {
  pendingAction.value = 'chrome-discover'
  try {
    chromeProfiles.value = await api.chromeImportProfiles()
    selectedChromeProfiles.value = []
    showChromeImport.value = true
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

function closeChromeImport(): void {
  if (pendingAction.value === 'chrome-import') return
  showChromeImport.value = false
  chromeProfiles.value = []
  selectedChromeProfiles.value = []
}

// importChromeAccounts 导入用户选中的 Chrome 账户
async function importChromeAccounts(): Promise<void> {
  pendingAction.value = 'chrome-import'
  try {
    const result = await api.importChromeAccounts({
      account_ids: [...selectedChromeProfiles.value],
      ...accountEnvironment,
    })
    showChromeImport.value = false
    chromeProfiles.value = []
    selectedChromeProfiles.value = []
    emit('refresh')
    emit('notice', tf('accounts.importComplete', { count: result.accounts.length }), 'success')
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

// saveAccount 保存已有账户配置并刷新产品数据
async function saveAccount(): Promise<void> {
  if (editingAccountID.value === '') return
  pendingAction.value = `edit:${editingAccountID.value}`
  try {
    await api.updateAccount(editingAccountID.value, { ...draft })
    closeEditor()
    emit('refresh')
    emit('notice', t('accounts.saved'), 'success')
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

// setEnabled 以账户现有配置切换启用状态
function setEnabled(account: Account, enabled: boolean): Promise<void> {
  return api.updateAccount(account.id, {
    label: account.label,
    enabled,
    proxy: account.proxy,
    locale: account.locale,
    timezone: account.timezone,
  })
}

// toggleAccount 切换账户是否参与请求
async function toggleAccount(account: Account): Promise<void> {
  pendingAction.value = `toggle:${account.id}`
  try {
    await setEnabled(account, !account.enabled)
    emit('refresh')
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

// runAccountAction 执行登录或会话验证
async function runAccountAction(account: Account, action: 'login' | 'verify'): Promise<void> {
  pendingAction.value = `${action}:${account.id}`
  try {
    if (action === 'login') {
      await api.loginAccount(account.id)
    } else {
      await api.verifyAccount(account.id)
    }
    emit('refresh')
    emit(
      'notice',
      t(action === 'login' ? 'accounts.loginComplete' : 'accounts.verified'),
      'success',
    )
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

// removeAccount 删除用户确认的账户
async function removeAccount(account: Account): Promise<void> {
  if (!(await confirmAction(t('accounts.deleteConfirm'), t('common.delete')))) return
  pendingAction.value = `delete:${account.id}`
  try {
    await api.deleteAccount(account.id)
    emit('refresh')
    emit('notice', t('accounts.deleted'), 'success')
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

// ---------- 筛选、排序、分页与选择 ----------

type StateFilter = 'all' | AccountState
type WorkerFilter = 'all' | WorkerState
// AccountTier 为筛选与显示用的权益：unknown 表示权益尚未从官网读取
type AccountTier = 'Ultra' | 'Pro' | 'Plus' | 'Free' | 'unknown'
type TierFilter = 'all' | AccountTier
type SortKey = 'default' | 'state' | 'label' | 'models' | 'worker'

const filterStates: AccountState[] = [
  'ready',
  'busy',
  'cooldown',
  'auth_required',
  'unavailable',
  'disabled',
]
const stateOrder: Record<AccountState, number> = {
  auth_required: 0,
  unavailable: 1,
  cooldown: 2,
  busy: 3,
  ready: 4,
  disabled: 5,
}
const sortOptions: { value: SortKey; label: TranslationKey }[] = [
  { value: 'default', label: 'accounts.sortDefault' },
  { value: 'state', label: 'accounts.sortState' },
  { value: 'label', label: 'accounts.sortLabel' },
  { value: 'models', label: 'accounts.sortModels' },
  { value: 'worker', label: 'accounts.sortWorker' },
]
const workerFilterOptions: { value: WorkerFilter; label: TranslationKey }[] = [
  { value: 'all', label: 'accounts.workerFilterAll' },
  { value: 'warm', label: 'accounts.workerFilterWarm' },
  { value: 'starting', label: 'accounts.workerFilterStarting' },
  { value: 'none', label: 'accounts.workerFilterNone' },
]
const tierFilterOptions: { value: TierFilter; label: TranslationKey }[] = [
  { value: 'all', label: 'accounts.tierFilterAll' },
  { value: 'Ultra', label: 'accounts.tierFilterUltra' },
  { value: 'Pro', label: 'accounts.tierFilterPro' },
  { value: 'Plus', label: 'accounts.tierFilterPlus' },
  { value: 'Free', label: 'accounts.tierFilterFree' },
  { value: 'unknown', label: 'accounts.tierFilterUnknown' },
]
const workerOrder: Record<WorkerState, number> = { warm: 0, starting: 1, none: 2 }
const workerLabelKeys: Record<WorkerState, TranslationKey> = {
  warm: 'accounts.workerWarm',
  starting: 'accounts.workerStarting',
  none: 'accounts.workerNone',
}
const workerTextClass: Record<WorkerState, string> = {
  warm: 'text-green-400',
  starting: 'text-cyan-300',
  none: 'text-gray-600',
}
const workerDotClass: Record<WorkerState, string> = {
  warm: 'bg-green-500',
  starting: 'bg-cyan-400 animate-pulse',
  none: 'bg-gray-700',
}

// accountTier 返回账户的真实权益，不随号池变化：开启 ULTRA_INCLUDE_PRO 后 Pro 账户属于 Ultra 号池，仍显示为 Pro，
// 也仍能用“权益：Pro”筛出来。权益尚未读取时为 unknown（服务端暂按 Free 显示，计入普通号池）；
// 已经属于 Ultra 号池的账户不按 unknown 显示，这样有权益但还没有目录指纹的已入池账户仍显示它的权益
function accountTier(account: Account): AccountTier {
  if (account.benefit_tier_known === false && account.pool !== 'ultra') return 'unknown'
  const tier = account.benefit_tier
  return tier === 'Ultra' || tier === 'Pro' || tier === 'Plus' ? tier : 'Free'
}

// inUltraPool 判断账户是否属于 Ultra 号池（成员关系，不是权益）：经 /ultra 前缀的请求只用这些账户，
// 开启 ULTRA_INCLUDE_PRO 时其中含权益为 Pro 的账户，所以号池计数不能按权益统计
function inUltraPool(account: Account): boolean {
  return account.pool === 'ultra'
}

// workerState 账户 Worker 的实时状态：运行中、启动中或未启动
function workerState(account: Account): WorkerState {
  if (props.workerSets.warm.has(account.id)) return 'warm'
  if (props.workerSets.starting.has(account.id)) return 'starting'
  return 'none'
}

const query = ref('')
const stateFilter = ref<StateFilter>('all')
const workerFilter = ref<WorkerFilter>('all')
const tierFilter = ref<TierFilter>('all')
const sortKey = ref<SortKey>('default')
const selected = ref<Set<string>>(new Set())

const stateCounts = computed(() => {
  const counts: Record<AccountState, number> = {
    ready: 0,
    busy: 0,
    cooldown: 0,
    auth_required: 0,
    unavailable: 0,
    disabled: 0,
  }
  for (const account of props.accounts) counts[account.state] += 1
  return counts
})

// tierCounts 按真实权益统计账户数，供权益筛选显示数量；ultraPoolCount 与 ultraReady 按号池成员关系统计
// Ultra 号池的账户数与其中就绪的账户数，开启 ULTRA_INCLUDE_PRO 时含 Pro 账户
const tierCounts = computed(() => {
  const counts: Record<TierFilter, number> = {
    all: props.accounts.length,
    Ultra: 0,
    Pro: 0,
    Plus: 0,
    Free: 0,
    unknown: 0,
  }
  for (const account of props.accounts) counts[accountTier(account)] += 1
  return counts
})
const ultraPoolCount = computed(() => props.accounts.filter(inUltraPool).length)
// poolTagShown 表示有账户要在权益旁另加 Ultra 号池标记（权益不是 Ultra 却属于 Ultra 号池）。原来的权益列放不下
// “权益 + 标记”，只在这时加宽权益列并同样加大表格最小宽度，账户列不被挤窄；开关关闭时表格布局与原来一致
const poolTagShown = computed(() =>
  props.accounts.some((account) => inUltraPool(account) && accountTier(account) !== 'Ultra'),
)
const ultraReady = computed(
  () =>
    props.accounts.filter((account) => inUltraPool(account) && account.state === 'ready').length,
)
// startupPauseShown 表示有账户在 Worker 列显示“启动失败×N（暂停至 hh:mm）”：原来的 Worker 列放不下，
// 只在这时加宽 Worker 列并同样加大表格最小宽度；没有暂停中的账户时表格布局与原来一致
const startupPauseShown = computed(() => startupPauses.value.size > 0)
const tableMinWidth = computed(() => {
  if (poolTagShown.value) return startupPauseShown.value ? 'min-w-[1188px]' : 'min-w-[1108px]'
  return startupPauseShown.value ? 'min-w-[1140px]' : 'min-w-[1060px]'
})

const filtered = computed(() => {
  const term = query.value.trim().toLowerCase()
  const list = props.accounts.filter((account) => {
    if (stateFilter.value !== 'all' && account.state !== stateFilter.value) return false
    if (workerFilter.value !== 'all' && workerState(account) !== workerFilter.value) return false
    if (tierFilter.value !== 'all' && accountTier(account) !== tierFilter.value) return false
    if (term === '') return true
    return [account.label, account.proxy, account.message, account.benefit_tier].some((value) =>
      value.toLowerCase().includes(term),
    )
  })
  if (sortKey.value === 'state') {
    list.sort(
      (left, right) =>
        stateOrder[left.state] - stateOrder[right.state] || left.label.localeCompare(right.label),
    )
  } else if (sortKey.value === 'label') {
    list.sort((left, right) => left.label.localeCompare(right.label))
  } else if (sortKey.value === 'worker') {
    list.sort(
      (left, right) =>
        workerOrder[workerState(left)] - workerOrder[workerState(right)] ||
        left.label.localeCompare(right.label),
    )
  } else if (sortKey.value === 'models') {
    list.sort(
      (left, right) =>
        right.models.length - left.models.length || left.label.localeCompare(right.label),
    )
  }
  return list
})

const { page, pageSize, pageItems } = usePaging(
  () => filtered.value,
  'aistudio2api_accounts_page_size',
  50,
)

watch([query, stateFilter, workerFilter, tierFilter, sortKey], () => {
  page.value = 1
})

const selectedAccounts = computed(() =>
  props.accounts.filter((account) => selected.value.has(account.id)),
)
const pageAllSelected = computed(
  () =>
    pageItems.value.length > 0 &&
    pageItems.value.every((account) => selected.value.has(account.id)),
)

function isSelected(account: Account): boolean {
  return selected.value.has(account.id)
}

function toggleSelected(account: Account): void {
  const next = new Set(selected.value)
  if (next.has(account.id)) next.delete(account.id)
  else next.add(account.id)
  selected.value = next
}

function togglePageSelection(): void {
  const next = new Set(selected.value)
  const selectAll = !pageAllSelected.value
  for (const account of pageItems.value) {
    if (selectAll) next.add(account.id)
    else next.delete(account.id)
  }
  selected.value = next
}

function selectAllFiltered(): void {
  selected.value = new Set(filtered.value.map((account) => account.id))
}

function clearSelection(): void {
  selected.value = new Set()
}

// 账户被删除后从选择集中移除
watch(
  () => props.accounts,
  (accounts) => {
    if (selected.value.size === 0) return
    const ids = new Set(accounts.map((account) => account.id))
    const next = new Set([...selected.value].filter((id) => ids.has(id)))
    if (next.size !== selected.value.size) selected.value = next
  },
)

// ---------- 批量操作 ----------

const bulk = reactive<BulkState>(emptyBulkState())
const bulkPanelOpen = ref(false)
const showVerifyDialog = ref(false)
const verifyScope = ref<'problem' | 'enabled' | 'selection'>('problem')
const verifyConcurrency = ref(2)

const bulkActionKeys: Record<BulkAction, TranslationKey> = {
  enable: 'common.enable',
  disable: 'common.disable',
  verify: 'common.verify',
  delete: 'common.delete',
}

const bulkLabel = computed(() => (bulk.action === '' ? '' : t(bulkActionKeys[bulk.action])))
const bulkPercent = computed(() =>
  bulk.total === 0 ? 0 : Math.round((bulk.done / bulk.total) * 100),
)
const busy = computed(() => bulk.running || pendingAction.value !== '')

const disabledAccounts = computed(() => props.accounts.filter((account) => !account.enabled))
const problemAccounts = computed(() =>
  props.accounts.filter(
    (account) =>
      account.enabled && (account.state === 'auth_required' || account.state === 'unavailable'),
  ),
)
const enabledAccounts = computed(() => props.accounts.filter((account) => account.enabled))
const selectionScopeAccounts = computed(() =>
  (selected.value.size > 0 ? selectedAccounts.value : filtered.value).filter(
    (account) => account.enabled,
  ),
)
const verifyTargets = computed(() => {
  if (verifyScope.value === 'enabled') return enabledAccounts.value
  if (verifyScope.value === 'selection') return selectionScopeAccounts.value
  return problemAccounts.value
})

// startBulk 执行批量任务，完成后刷新数据并给出汇总通知
async function startBulk(
  action: BulkAction,
  accounts: readonly Account[],
  concurrency: number,
  task: (account: Account) => Promise<void>,
): Promise<void> {
  if (bulk.running || accounts.length === 0) return
  bulkPanelOpen.value = true
  await runBulk(bulk, action, accounts, concurrency, task)
  finishBulk(action)
}

// finishBulk 批量任务结束后刷新数据并给出汇总通知
function finishBulk(action: BulkAction): void {
  emit('refresh')
  emit(
    'notice',
    tf('accounts.bulkFinished', {
      action: t(bulkActionKeys[action]),
      succeeded: bulk.succeeded,
      failed: bulk.failures.length,
    }),
    bulk.failures.length === 0 ? 'success' : 'error',
  )
}

// ENABLE_CHUNK 为批量启用每次请求提交的账户数：服务端每次最多处理 1000 个，500 个一批几秒内返回，
// 不会被反向代理的请求超时打断，两批之间也可以停止
const ENABLE_CHUNK = 500

// enableTargets 分批调用批量启用接口：原先逐个账户发请求，每启用一个服务端都要推送一次全部账户，上万个账户要几个小时。
// 原本已启用的账户计为成功，没有启用的列出原因
async function enableTargets(targets: readonly Account[]): Promise<void> {
  if (bulk.running || targets.length === 0) return
  bulkPanelOpen.value = true
  await runChunkedBulk(bulk, 'enable', targets, ENABLE_CHUNK, enableChunk)
  finishBulk('enable')
}

// enableChunk 一次请求启用一批账户，把服务端的失败项换成面板的失败明细
async function enableChunk(chunk: readonly Account[]): Promise<BulkChunkResult> {
  const result = await api.enableAccounts({ account_ids: chunk.map((account) => account.id) })
  const labels = new Map(chunk.map((account) => [account.id, account.label]))
  return {
    succeeded: result.enabled + result.skipped,
    failures: (result.failed ?? []).map((failure) => ({
      id: failure.id,
      label: labels.get(failure.id) ?? failure.id,
      message: failure.code === 'account_busy' ? t('accounts.bulkEnableBusy') : failure.message,
    })),
  }
}

// enableAll 一键启用全部已停用账户
async function enableAll(): Promise<void> {
  const targets = disabledAccounts.value
  if (targets.length === 0) {
    emit('notice', t('accounts.noneDisabled'), 'success')
    return
  }
  if (!(await confirmAction(tf('accounts.enableAllConfirm', { count: targets.length }), t('common.enable'))))
    return
  await enableTargets(targets)
}

// PERMISSION_DENIED_DELETE_CHUNK 为 403 无权限账户与失效账户一键删除每次请求提交的账户数（服务端每次最多 500 个），两批之间可以停止
const PERMISSION_DENIED_DELETE_CHUNK = 500
// 一键删除的结果提示最多列出的失败账户数，其余见批量进度面板的失败明细
const PERMISSION_DENIED_DELETE_FAILURES_SHOWN = 3

const permissionDeniedDeleting = ref(false)
const startupDeadDeleting = ref(false)
// oneClickDeleting 为 403 无权限账户或失效账户的一键删除正在进行（含确认后重新读取列表、尚未开始分批的阶段）：两者共用批量进度面板，不能同时进行
const oneClickDeleting = computed(() => permissionDeniedDeleting.value || startupDeadDeleting.value)

// OneClickDeleteTotals 为一键删除的累计结果；skipped 为确认后已移出列表、没有删除的账户数
interface OneClickDeleteTotals {
  deleted: number
  missing: number
  remaining: number
  skipped: number
}

// bulkDeleteChunk 用批量删除接口删除一批账户：累计结果，把已删除或号池里已经没有的账户记入 removed（服务端已清除它们的记录），
// 并把服务端的失败项换成面板的失败明细，account_busy 单独提示
async function bulkDeleteChunk(
  listed: readonly BulkTarget[],
  totals: OneClickDeleteTotals,
  removed: Set<string>,
): Promise<BulkChunkResult> {
  const result = await api.bulkDeleteAccounts(listed.map((account) => account.id))
  const failed = result.failed ?? []
  totals.deleted += result.deleted
  totals.missing += result.missing
  totals.remaining += result.remaining
  // 有本次没有处理的账户时不知道是哪几个，这一批只靠重新读取列表；账户 ID 按不区分大小写对照
  if (result.remaining === 0) {
    const failedIDs = new Set(failed.map((failure) => failure.id.toLowerCase()))
    for (const account of listed) {
      if (!failedIDs.has(account.id.toLowerCase())) removed.add(account.id)
    }
  }
  const labels = new Map(listed.map((account) => [account.id.toLowerCase(), account.label]))
  return {
    succeeded: result.deleted + result.missing,
    failures: failed.map((failure) => ({
      id: failure.id,
      label: labels.get(failure.id.toLowerCase()) ?? failure.id,
      message:
        failure.code === 'account_busy'
          ? t('accounts.permissionDeniedDeleteBusy')
          : failure.message,
    })),
  }
}

// notifyOneClickDelete 给出一键删除的结果提示：已删除、不存在、失败（最多列出 3 个）、本次没有处理与确认后跳过的账户数
function notifyOneClickDelete(
  totals: OneClickDeleteTotals,
  failures: readonly BulkFailure[],
  skippedKey: TranslationKey,
): void {
  let message = tf('accounts.permissionDeniedDeleteResult', {
    deleted: totals.deleted,
    missing: totals.missing,
    failed: failures.length,
  })
  if (failures.length > 0) {
    const shown = failures
      .slice(0, PERMISSION_DENIED_DELETE_FAILURES_SHOWN)
      .map((failure) => `${failure.label}: ${failure.message}`)
    if (failures.length > PERMISSION_DENIED_DELETE_FAILURES_SHOWN) {
      shown.push(
        tf('accounts.permissionDeniedDeleteMore', {
          count: failures.length - PERMISSION_DENIED_DELETE_FAILURES_SHOWN,
        }),
      )
    }
    message += tf('accounts.permissionDeniedDeleteFailures', { list: shown.join('; ') })
  }
  if (totals.remaining > 0) {
    message += tf('accounts.permissionDeniedDeleteRemaining', { count: totals.remaining })
  }
  if (totals.skipped > 0) {
    message += tf(skippedKey, { count: totals.skipped })
  }
  emit('notice', message, failures.length === 0 && totals.remaining === 0 ? 'success' : 'error')
}

// readPermissionDeniedNow 立即重新读取 403 无权限账户列表（不受 15 秒刷新间隔限制）；读取失败时保留当前列表
async function readPermissionDeniedNow(): Promise<void> {
  try {
    const accounts = await api.permissionDenied()
    // 之前发出、之后才返回的读取结果不再覆盖这次读取
    permissionDeniedVersion += 1
    permissionDenied.value = accounts
    permissionDeniedLoadedAt = Date.now()
  } catch {
    // 保留当前列表
  }
}

// stillPermissionDenied 返回仍在当前 403 无权限账户列表里的账户（账户 ID 按原样对照，与服务端一致）
function stillPermissionDenied<T extends BulkTarget>(accounts: readonly T[]): T[] {
  const listed = new Set(permissionDenied.value.map((account) => account.id))
  return accounts.filter((account) => listed.has(account.id))
}

// deletePermissionDenied 一键删除 403 无权限账户：删除完整列表（包括折叠未显示的）中的账户及其认证目录，
// 与单个“删除”效果相同，服务端同时清除它们的 403 记录。按 500 个一批调用批量删除接口，进度与失败明细显示在
// 批量进度面板；正在处理请求的账户由服务端等它空闲后删除，等不到的列为失败。完成后刷新 403 列表与账户列表。
// 确认框打开期间列表可能已经变化：重新上传了认证文件的账户会被服务端移出列表，这时再删除就会连同新的认证文件一起删掉。
// 所以确认后先重新读取一次列表，每批提交前再与当前列表对照，只删除确认时列出、提交时仍在列表里的账户；
// 确认后才列出的账户没有经过确认，也不删除
async function deletePermissionDenied(): Promise<void> {
  const confirmed = permissionDenied.value.map(({ id, label }) => ({ id, label }))
  if (busy.value || oneClickDeleting.value || confirmed.length === 0) return
  if (
    !(await confirmAction(
      tf('accounts.permissionDeniedDeleteConfirm', { count: confirmed.length }),
      t('accounts.permissionDeniedDelete'),
    ))
  )
    return
  if (busy.value || oneClickDeleting.value) return
  permissionDeniedDeleting.value = true
  const totals: OneClickDeleteTotals = { deleted: 0, missing: 0, remaining: 0, skipped: 0 }
  // removed 为已删除或号池里已经没有的账户，服务端已清除它们的 403 记录
  const removed = new Set<string>()
  let failures: BulkFailure[] = []
  try {
    await readPermissionDeniedNow()
    const targets = stillPermissionDenied(confirmed)
    totals.skipped = confirmed.length - targets.length
    if (targets.length > 0) {
      bulkPanelOpen.value = true
      await runChunkedBulk(bulk, 'delete', targets, PERMISSION_DENIED_DELETE_CHUNK, async (chunk) => {
        // 前面几批删除期间列表可能又刷新过
        const listed = stillPermissionDenied(chunk)
        totals.skipped += chunk.length - listed.length
        if (listed.length === 0) return { succeeded: 0, failures: [] }
        return bulkDeleteChunk(listed, totals, removed)
      })
      failures = bulk.failures
    }
  } finally {
    permissionDeniedDeleting.value = false
  }
  // 删除前发出、删除后才返回的读取结果不再覆盖列表；先移除已删除的账户，再重新读取
  permissionDeniedVersion += 1
  permissionDenied.value = permissionDenied.value.filter((account) => !removed.has(account.id))
  if (permissionDenied.value.length <= PERMISSION_DENIED_COLLAPSED) {
    showAllPermissionDenied.value = false
  }
  void loadPermissionDenied()
  emit('refresh')
  notifyOneClickDelete(totals, failures, 'accounts.permissionDeniedDeleteSkipped')
}

// stillStartupDead 返回仍在当前启动失败列表里、且仍确定失效的账户（账户 ID 按原样对照，与服务端一致）
function stillStartupDead<T extends BulkTarget>(accounts: readonly T[]): T[] {
  const listed = new Set(startupDead.value.map((account) => account.id))
  return accounts.filter((account) => listed.has(account.id))
}

// deleteStartupDead 一键删除失效账户：做法与 403 无权限账户的一键删除相同，删除完整列表（包括折叠未显示的）中的账户及其认证目录，
// 按 500 个一批调用批量删除接口，服务端同时清除它们的记录；完成后刷新列表与账户列表。只删除确定失效（confirmed）的账户：
// 原因不确定、只暂停调度的账户永远不删除，机器负载高或网络慢时健康账户也会这样启动失败。
// 确认框打开期间列表可能已经变化：重新上传了认证文件或验证通过的账户会被服务端移出列表，这时再删除就会连同新的认证文件一起删掉。
// 所以确认后先重新读取一次列表（读取失败时不删除），每批提交前再与当前列表对照，只删除确认时列出、提交时仍确定失效的账户；
// 确认后才列出的账户没有经过确认，也不删除
async function deleteStartupDead(): Promise<void> {
  const confirmed = startupDead.value.map(({ id, label }) => ({ id, label }))
  if (busy.value || oneClickDeleting.value || confirmed.length === 0) return
  // 页面受阻确认的账户状态仍是就绪、登录可能正常，确认框写明个数；大量账户同时页面受阻时另加警告，不建议批量删除
  let message = tf('accounts.startupDeadDeleteConfirm', { count: confirmed.length })
  const pageBlocked = startupDeadPageBlocked.value
  if (pageBlocked > 0) {
    message += tf('accounts.startupDeadDeletePageBlocked', { count: pageBlocked })
  }
  if (startupPageBlockedBurst.value) message += t('accounts.startupDeadDeleteBurst')
  if (!(await confirmAction(message, t('accounts.startupDeadDelete')))) return
  if (busy.value || oneClickDeleting.value) return
  startupDeadDeleting.value = true
  const totals: OneClickDeleteTotals = { deleted: 0, missing: 0, remaining: 0, skipped: 0 }
  // removed 为已删除或号池里已经没有的账户，服务端已清除它们的记录
  const removed = new Set<string>()
  let failures: BulkFailure[] = []
  try {
    try {
      await readStartupFailuresNow()
    } catch (error) {
      // 读不到当前列表就无法确认账户仍然失效，不删除
      emit('notice', tf('accounts.startupDeadReadFailed', { error: messageOf(error) }), 'error')
      return
    }
    const targets = stillStartupDead(confirmed)
    totals.skipped = confirmed.length - targets.length
    if (targets.length > 0) {
      bulkPanelOpen.value = true
      await runChunkedBulk(
        bulk,
        'delete',
        targets,
        PERMISSION_DENIED_DELETE_CHUNK,
        async (chunk) => {
          // 前面几批删除期间列表可能又刷新过
          const listed = stillStartupDead(chunk)
          totals.skipped += chunk.length - listed.length
          if (listed.length === 0) return { succeeded: 0, failures: [] }
          return bulkDeleteChunk(listed, totals, removed)
        },
      )
      failures = bulk.failures
    }
  } finally {
    startupDeadDeleting.value = false
  }
  // 删除前发出、删除后才返回的读取结果不再覆盖列表；先移除已删除的账户，再重新读取
  startupFailuresVersion += 1
  startupFailures.value = startupFailures.value.filter((account) => !removed.has(account.id))
  if (startupDead.value.length <= STARTUP_FAILURES_COLLAPSED) {
    showAllStartupDead.value = false
  }
  void loadStartupFailures()
  emit('refresh')
  notifyOneClickDelete(totals, failures, 'accounts.startupDeadDeleteSkipped')
}

function openVerifyDialog(): void {
  verifyScope.value = selected.value.size > 0 ? 'selection' : problemAccounts.value.length > 0 ? 'problem' : 'enabled'
  showVerifyDialog.value = true
}

// startVerify 按所选范围与并发批量验证账户
async function startVerify(): Promise<void> {
  const targets = verifyTargets.value
  showVerifyDialog.value = false
  if (targets.length === 0) {
    emit('notice', t('accounts.noneToVerify'), 'success')
    return
  }
  await startBulk('verify', targets, verifyConcurrency.value, (account) =>
    api.verifyAccount(account.id),
  )
}

// bulkSelected 对选中账户执行启用、停用或删除
async function bulkSelected(action: 'enable' | 'disable' | 'delete'): Promise<void> {
  const targets = selectedAccounts.value
  if (targets.length === 0) return
  if (action === 'disable') {
    if (!(await confirmAction(tf('accounts.bulkDisableConfirm', { count: targets.length }), t('common.disable'))))
      return
    await startBulk('disable', targets, 4, (account) => setEnabled(account, false))
    return
  }
  if (action === 'delete') {
    if (!(await confirmAction(tf('accounts.bulkDeleteConfirm', { count: targets.length }), t('common.delete'))))
      return
    await startBulk('delete', targets, 2, (account) => api.deleteAccount(account.id))
    clearSelection()
    return
  }
  await enableTargets(targets.filter((account) => !account.enabled))
}

// copyEmails 复制账户邮箱（每行一个），便于在本地批量重新登录
async function copyEmails(labels: readonly string[]): Promise<void> {
  if (labels.length === 0) return
  const copied = await copyText(labels.join('\n'))
  emit(
    'notice',
    copied ? tf('accounts.copiedEmails', { count: labels.length }) : t('common.error'),
    copied ? 'success' : 'error',
  )
}

function stopBulk(): void {
  bulk.cancelled = true
}

function closeBulkPanel(): void {
  if (bulk.running) return
  bulkPanelOpen.value = false
}
</script>

<template>
  <section class="min-h-0 flex-1 overflow-auto">
    <div class="page space-y-4">
      <!-- 标题与主要操作 -->
      <div class="flex flex-wrap items-center justify-between gap-3 border-b border-[#30363d] pb-3">
        <div class="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <h2 class="page-title">{{ t('section.accounts.title') }}</h2>
          <span class="font-mono text-xs text-gray-500">
            {{ tf('app.readyOf', { ready: stateCounts.ready, total: accounts.length }) }}
          </span>
          <span v-if="workerCounts" class="font-mono text-xs text-green-400">
            {{ tf('app.workers', { warm: workerCounts.warm, target: workerCounts.target }) }}
          </span>
          <span v-if="workerCounts && workerCounts.starting > 0" class="font-mono text-xs text-cyan-300">
            {{ tf('app.workersStarting', { count: workerCounts.starting }) }}
          </span>
          <span
            v-tooltip="t('accounts.ultraCountHelp')"
            class="tag tag-ultra"
            tabindex="0"
          >
            {{ tf('accounts.ultraCount', { ready: ultraReady, total: ultraPoolCount }) }}
          </span>
          <span
            v-if="ultraWorkerCounts && ultraPoolCount > 0"
            class="font-mono text-xs text-fuchsia-300"
          >
            {{
              tf('accounts.ultraWorkers', {
                warm: ultraWorkerCounts.warm,
                target: ultraWorkerCounts.target,
              })
            }}
          </span>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <button
            class="btn btn-success"
            type="button"
            :disabled="busy || disabledAccounts.length === 0"
            @click="enableAll"
          >
            <UiIcon name="play" :size="14" />
            {{ t('accounts.enableAll') }}
            <span class="seg-count">{{ disabledAccounts.length }}</span>
          </button>
          <button class="btn btn-primary" type="button" :disabled="busy" @click="openVerifyDialog">
            <UiIcon name="verify" :size="14" />
            {{ t('accounts.verifyAll') }}
          </button>
          <span class="mx-1 hidden h-5 w-px bg-[#30363d] sm:block"></span>
          <button
            class="btn"
            type="button"
            :disabled="busy"
            :aria-busy="pendingAction === 'chrome-discover'"
            @click="openChromeImport"
          >
            <UiIcon name="accounts" :size="14" />
            {{ t('accounts.importChrome') }}
          </button>
          <button class="btn" type="button" :disabled="busy" @click="openBrowserLogin">
            <UiIcon name="login" :size="14" />
            {{ t('accounts.browserLogin') }}
          </button>
        </div>
      </div>

      <p class="-mt-2 text-xs text-gray-500">{{ t('accounts.autoImportHint') }}</p>
      <OnboardingPanel :disabled-count="stateCounts.disabled" />

      <!-- 403 无权限账户：多半未同意 Google 政策，重新获取认证文件上传后自动清除 -->
      <div
        v-if="permissionDenied.length > 0"
        class="rounded-lg border border-red-500/30 bg-red-500/5 p-3 text-sm"
      >
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div class="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1">
            <span
              v-tooltip="t('accounts.permissionDeniedHelp')"
              class="font-bold text-red-300"
              tabindex="0"
            >
              {{ t('accounts.permissionDeniedTitle') }}
            </span>
            <span class="text-gray-300">{{ t('accounts.permissionDeniedHint') }}</span>
            <span class="font-mono text-red-300">
              {{ tf('accounts.permissionDeniedCount', { count: permissionDenied.length }) }}
            </span>
          </div>
          <div class="flex flex-wrap gap-2">
            <button
              class="btn btn-sm"
              type="button"
              @click="copyEmails(permissionDenied.map((account) => account.label))"
            >
              {{ t('accounts.copyAllEmails') }}
            </button>
            <button class="btn btn-sm" type="button" @click="downloadPermissionDenied">
              {{ t('accounts.permissionDeniedDownload') }}
            </button>
            <button
              class="btn btn-sm btn-danger"
              type="button"
              :disabled="permissionDeniedClearing || permissionDeniedDeleting"
              :aria-busy="permissionDeniedClearing"
              @click="clearPermissionDenied"
            >
              {{ t('accounts.permissionDeniedClear') }}
            </button>
            <button
              v-tooltip="t('accounts.permissionDeniedDeleteHelp')"
              class="btn btn-sm btn-danger"
              type="button"
              :disabled="
                busy ||
                oneClickDeleting ||
                permissionDeniedClearing ||
                permissionDenied.length === 0
              "
              :aria-busy="permissionDeniedDeleting"
              @click="deletePermissionDenied"
            >
              {{
                permissionDeniedDeleting
                  ? tf('accounts.progress', {
                      action: t('common.delete'),
                      done: bulk.done,
                      total: bulk.total,
                    })
                  : t('accounts.permissionDeniedDelete')
              }}
            </button>
          </div>
        </div>
        <div class="mt-2 overflow-x-auto">
          <table class="w-full text-xs">
            <thead>
              <tr class="text-left text-gray-500">
                <th class="py-1 pr-3 font-normal">{{ t('accounts.permissionDeniedEmail') }}</th>
                <th class="py-1 pr-3 font-normal">{{ t('accounts.permissionDeniedModels') }}</th>
                <th class="py-1 pr-3 font-normal">{{ t('accounts.permissionDeniedLast') }}</th>
                <th class="py-1 text-right font-normal">
                  {{ t('accounts.permissionDeniedTimes') }}
                </th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="account in visiblePermissionDenied"
                :key="account.id"
                class="border-t border-[#30363d]"
              >
                <td class="py-1 pr-3 font-mono text-gray-200">{{ account.label }}</td>
                <td class="max-w-md py-1 pr-3">
                  <div class="cell-truncate text-gray-300" :title="permissionDeniedDetail(account)">
                    {{ permissionDeniedModels(account) }}
                  </div>
                </td>
                <td class="whitespace-nowrap py-1 pr-3 font-mono text-gray-300">
                  {{ formatDateTime(account.last_at) }}
                </td>
                <td class="py-1 text-right font-mono text-red-300">{{ account.count }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <button
          v-if="permissionDenied.length > PERMISSION_DENIED_COLLAPSED"
          class="mt-2 text-xs text-blue-400 hover:underline"
          type="button"
          @click="showAllPermissionDenied = !showAllPermissionDenied"
        >
          {{
            showAllPermissionDenied
              ? t('accounts.cooldownShowLess')
              : tf('accounts.permissionDeniedShowAll', { count: permissionDenied.length })
          }}
        </button>
      </div>

      <!-- 失效账户：启动 Worker 时登录已失效、认证文件无法使用或号池健康时连续页面受阻（确定失效），可以一键删除 -->
      <div
        v-if="startupDead.length > 0"
        class="rounded-lg border border-red-500/30 bg-red-500/5 p-3 text-sm"
      >
        <!-- 大量账户同时页面受阻：可能是官网页面改版，服务端暂不确认新的页面受阻失效 -->
        <p
          v-if="startupPageBlockedBurst"
          class="mb-2 rounded border border-yellow-500/40 bg-yellow-500/10 px-2 py-1 text-xs leading-5 text-yellow-300"
          role="status"
        >
          {{ t('accounts.startupPageBlockedBurst') }}
        </p>
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div class="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1">
            <span
              v-tooltip="t('accounts.startupDeadHelp')"
              class="font-bold text-red-300"
              tabindex="0"
            >
              {{ t('accounts.startupDeadTitle') }}
            </span>
            <span class="text-gray-300">{{ t('accounts.startupDeadHint') }}</span>
            <span class="font-mono text-red-300">
              {{ tf('accounts.startupDeadCount', { count: startupDead.length }) }}
            </span>
          </div>
          <div class="flex flex-wrap gap-2">
            <button
              class="btn btn-sm"
              type="button"
              @click="copyEmails(startupDead.map((account) => account.label))"
            >
              {{ t('accounts.copyAllEmails') }}
            </button>
            <button
              class="btn btn-sm"
              type="button"
              @click="
                downloadEmails(
                  startupDead.map((account) => account.label),
                  'startup-dead-accounts',
                )
              "
            >
              {{ t('accounts.startupDeadDownload') }}
            </button>
            <button
              class="btn btn-sm btn-danger"
              type="button"
              :disabled="startupDeadClearing || startupDeadDeleting"
              :aria-busy="startupDeadClearing"
              @click="clearStartupDead"
            >
              {{ t('accounts.startupDeadClear') }}
            </button>
            <button
              v-tooltip="t('accounts.startupDeadDeleteHelp')"
              class="btn btn-sm btn-danger"
              type="button"
              :disabled="
                busy || oneClickDeleting || startupDeadClearing || startupDead.length === 0
              "
              :aria-busy="startupDeadDeleting"
              @click="deleteStartupDead"
            >
              {{
                startupDeadDeleting
                  ? tf('accounts.progress', {
                      action: t('common.delete'),
                      done: bulk.done,
                      total: bulk.total,
                    })
                  : t('accounts.startupDeadDelete')
              }}
            </button>
          </div>
        </div>
        <!-- 页面受阻确认的账户：账户状态仍是就绪，说明怎么检查、什么时候不要批量删除 -->
        <p v-if="startupDeadPageBlocked > 0" class="mt-2 text-xs leading-5 text-gray-400">
          {{ tf('accounts.startupPageBlockedHelp', { count: startupDeadPageBlocked }) }}
        </p>
        <div class="mt-2 overflow-x-auto">
          <table class="w-full text-xs">
            <thead>
              <tr class="text-left text-gray-500">
                <th class="py-1 pr-3 font-normal">{{ t('accounts.startupFailureEmail') }}</th>
                <th class="py-1 pr-3 font-normal">{{ t('accounts.startupFailureReason') }}</th>
                <th class="py-1 pr-3 font-normal">
                  <span v-tooltip="t('accounts.startupFailureStageHelp')" tabindex="0">
                    {{ t('accounts.startupFailureStage') }}
                  </span>
                </th>
                <th class="py-1 pr-3 font-normal">{{ t('accounts.startupFailureLast') }}</th>
                <th class="py-1 text-right font-normal">
                  {{ t('accounts.startupFailureTimes') }}
                </th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="account in visibleStartupDead"
                :key="account.id"
                class="border-t border-[#30363d]"
              >
                <td class="py-1 pr-3 font-mono text-gray-200">{{ account.label }}</td>
                <td class="max-w-md py-1 pr-3">
                  <div class="cell-truncate text-gray-300" :title="startupFailureDetail(account)">
                    {{ startupFailureReason(account) }}
                  </div>
                </td>
                <td class="whitespace-nowrap py-1 pr-3 font-mono text-gray-300">
                  {{ startupStage(account) }}
                </td>
                <td class="whitespace-nowrap py-1 pr-3 font-mono text-gray-300">
                  {{ formatDateTime(account.last_at ?? '') }}
                </td>
                <td class="py-1 text-right font-mono text-red-300">{{ account.count }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <button
          v-if="startupDead.length > STARTUP_FAILURES_COLLAPSED"
          class="mt-2 text-xs text-blue-400 hover:underline"
          type="button"
          @click="showAllStartupDead = !showAllStartupDead"
        >
          {{
            showAllStartupDead
              ? t('accounts.cooldownShowLess')
              : tf('accounts.startupFailureShowAll', { count: startupDead.length })
          }}
        </button>
      </div>

      <!-- 没有失效账户时，页面受阻爆发的提示单独显示（这时服务端不确认新的页面受阻失效，账户在下方未确认列表里） -->
      <p
        v-if="startupDead.length === 0 && startupPageBlockedBurst"
        class="rounded-lg border border-yellow-500/40 bg-yellow-500/10 p-3 text-xs leading-5 text-yellow-300"
        role="status"
      >
        {{ t('accounts.startupPageBlockedBurst') }}
      </p>

      <!-- 启动反复失败（未确认）：原因不确定，机器负载高或网络慢时健康账户也会这样，只暂停调度，只能验证或清除暂停，不能删除 -->
      <details
        v-if="startupPaused.length > 0"
        class="rounded-lg border border-yellow-500/20 bg-yellow-500/5 p-3 text-sm"
      >
        <summary class="cursor-pointer">
          <span class="font-bold text-yellow-300">{{ t('accounts.startupPausedTitle') }}</span>
          <span class="ml-2 font-mono text-yellow-300">
            {{ tf('accounts.startupPausedCount', { count: startupPaused.length }) }}
          </span>
        </summary>
        <p class="mt-2 text-xs leading-5 text-gray-400">{{ t('accounts.startupPausedHint') }}</p>
        <div class="mt-2 overflow-x-auto">
          <table class="w-full text-xs">
            <thead>
              <tr class="text-left text-gray-500">
                <th class="py-1 pr-3 font-normal">{{ t('accounts.startupFailureEmail') }}</th>
                <th class="py-1 pr-3 font-normal">{{ t('accounts.startupFailureReason') }}</th>
                <th class="py-1 pr-3 font-normal">
                  <span v-tooltip="t('accounts.startupFailureStageHelp')" tabindex="0">
                    {{ t('accounts.startupFailureStage') }}
                  </span>
                </th>
                <th class="py-1 pr-3 text-right font-normal">
                  {{ t('accounts.startupFailureTimes') }}
                </th>
                <th class="py-1 pr-3 font-normal">{{ t('accounts.startupFailureLast') }}</th>
                <th class="py-1 pr-3 font-normal">{{ t('accounts.startupFailureUntil') }}</th>
                <th class="py-1 text-right font-normal">{{ t('accounts.actions') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="account in visibleStartupPaused"
                :key="account.id"
                class="border-t border-[#30363d]"
              >
                <td class="py-1 pr-3 font-mono text-gray-200">{{ account.label }}</td>
                <td class="max-w-md py-1 pr-3">
                  <div class="cell-truncate text-gray-300" :title="startupFailureDetail(account)">
                    {{ startupFailureReason(account) }}
                  </div>
                </td>
                <td class="whitespace-nowrap py-1 pr-3 font-mono text-gray-300">
                  {{ startupStage(account) }}
                </td>
                <td class="py-1 pr-3 text-right font-mono text-yellow-300">{{ account.count }}</td>
                <td class="whitespace-nowrap py-1 pr-3 font-mono text-gray-300">
                  {{ formatDateTime(account.last_at ?? '') }}
                </td>
                <td class="whitespace-nowrap py-1 pr-3 font-mono text-gray-300">
                  {{ startupPausedUntil(account) }}
                </td>
                <td class="py-1">
                  <div class="flex justify-end gap-1">
                    <button
                      class="btn btn-sm"
                      type="button"
                      :disabled="
                        busy ||
                        account.kind === 'fingerprint' ||
                        accountsByID.get(account.id)?.enabled !== true
                      "
                      :aria-busy="pendingAction === `verify:${account.id}`"
                      @click="verifyStartupPaused(account)"
                    >
                      {{ t('common.verify') }}
                    </button>
                    <button
                      class="btn btn-sm"
                      type="button"
                      :disabled="startupPauseClearing !== ''"
                      :aria-busy="startupPauseClearing === account.id"
                      @click="clearStartupPause(account)"
                    >
                      {{ t('accounts.startupPausedClear') }}
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <button
          v-if="startupPaused.length > STARTUP_FAILURES_COLLAPSED"
          class="mt-2 text-xs text-blue-400 hover:underline"
          type="button"
          @click="showAllStartupPaused = !showAllStartupPaused"
        >
          {{
            showAllStartupPaused
              ? t('accounts.cooldownShowLess')
              : tf('accounts.startupFailureShowAll', { count: startupPaused.length })
          }}
        </button>
      </details>

      <!-- 模型冷却概况：按模型统计冷却中的账号与恢复倒计时 -->
      <div v-if="cooldownSummary.entries > 0" class="rounded-lg border border-yellow-500/30 bg-yellow-500/5 p-3 text-sm">
        <div class="flex flex-wrap items-baseline gap-x-4 gap-y-1">
          <span class="font-bold text-yellow-300">{{ t('accounts.cooldownTitle') }}</span>
          <span class="text-gray-300">
            {{ tf('accounts.cooldownSummary', { accounts: cooldownSummary.accounts, entries: cooldownSummary.entries }) }}
          </span>
          <span class="font-mono text-xs text-gray-400">
            {{
              tf('accounts.cooldownEarliestAll', {
                time: formatRemaining(cooldownSummary.earliest),
                clock: formatClock(cooldownSummary.earliest),
              })
            }}
          </span>
          <span class="font-mono text-xs text-gray-400">
            {{
              tf('accounts.cooldownLatestAll', {
                time: formatRemaining(cooldownSummary.latest),
                clock: formatClock(cooldownSummary.latest),
              })
            }}
          </span>
        </div>
        <div class="mt-2 overflow-x-auto">
          <table class="w-full text-xs">
            <thead>
              <tr class="text-left text-gray-500">
                <th class="py-1 pr-3 font-normal">{{ t('accounts.cooldownModel') }}</th>
                <th class="py-1 pr-3 text-right font-normal">{{ t('accounts.cooldownAccounts') }}</th>
                <th class="py-1 pr-3 font-normal">{{ t('accounts.cooldownFirst') }}</th>
                <th class="py-1 pr-3 font-normal">{{ t('accounts.cooldownLast') }}</th>
                <th class="py-1 font-normal">{{ t('accounts.cooldownReason') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="group in visibleCooldownGroups" :key="group.key" class="border-t border-[#30363d]">
                <td class="py-1 pr-3 text-gray-200">
                  {{ group.model }}
                  <span class="text-gray-500">· {{ t(channelLabelKey(group.channel)) }}</span>
                </td>
                <td class="py-1 pr-3 text-right font-mono text-yellow-300">{{ group.accounts }}</td>
                <td class="py-1 pr-3 font-mono text-gray-300">
                  {{ formatRemaining(group.earliest) }}
                  <span class="text-gray-500">({{ formatClock(group.earliest) }})</span>
                </td>
                <td class="py-1 pr-3 font-mono text-gray-300">
                  {{ formatRemaining(group.latest) }}
                  <span class="text-gray-500">({{ formatClock(group.latest) }})</span>
                </td>
                <td class="max-w-xs py-1">
                  <div class="cell-truncate text-gray-400" :title="group.reason">{{ group.reason || '—' }}</div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <button
          v-if="cooldownSummary.groups.length > 6"
          class="mt-2 text-xs text-blue-400 hover:underline"
          type="button"
          @click="showAllCooldowns = !showAllCooldowns"
        >
          {{
            showAllCooldowns
              ? t('accounts.cooldownShowLess')
              : tf('accounts.cooldownShowAll', { count: cooldownSummary.groups.length })
          }}
        </button>
      </div>

      <!-- 批量进度 -->
      <div v-if="bulkPanelOpen" class="panel p-3">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div class="flex flex-wrap items-center gap-3 text-sm">
            <strong class="text-gray-200">
              {{ tf('accounts.progress', { action: bulkLabel, done: bulk.done, total: bulk.total }) }}
            </strong>
            <span class="text-xs text-green-400">
              {{ tf('accounts.progressSucceeded', { count: bulk.succeeded }) }}
            </span>
            <span class="text-xs" :class="bulk.failures.length > 0 ? 'text-red-400' : 'text-gray-500'">
              {{ tf('accounts.progressFailed', { count: bulk.failures.length }) }}
            </span>
            <span v-if="!bulk.running" class="text-xs text-gray-500">
              {{ bulk.cancelled ? t('accounts.progressStopped') : t('accounts.progressDone') }}
            </span>
          </div>
          <div class="flex gap-2">
            <button
              v-if="bulk.running"
              class="btn btn-sm btn-danger"
              type="button"
              :disabled="bulk.cancelled"
              :aria-busy="bulk.cancelled"
              @click="stopBulk"
            >
              {{ t('accounts.progressStop') }}
            </button>
            <button
              v-else
              class="btn btn-sm btn-ghost"
              type="button"
              :aria-label="t('common.close')"
              @click="closeBulkPanel"
            >
              <UiIcon name="close" :size="14" />
            </button>
          </div>
        </div>
        <div class="progress-track mt-2">
          <div
            class="progress-bar"
            :class="bulk.failures.length > 0 ? 'bg-yellow-500' : ''"
            :style="{ width: `${bulkPercent}%` }"
          ></div>
        </div>
        <p v-if="bulk.active.length > 0" class="cell-truncate mt-2 font-mono text-xs text-gray-500">
          {{ tf('accounts.progressActive', { names: bulk.active.join(', ') }) }}
        </p>
        <details v-if="bulk.failures.length > 0" class="mt-2 text-xs">
          <summary class="cursor-pointer text-red-400">
            {{ tf('accounts.progressFailures', { count: bulk.failures.length }) }}
            <button
              class="btn btn-sm ml-2"
              type="button"
              @click.prevent="copyEmails(bulk.failures.map((failure) => failure.label))"
            >
              {{ t('accounts.copyEmails') }}
            </button>
          </summary>
          <ul class="mt-2 max-h-48 space-y-1 overflow-auto">
            <li v-for="failure in bulk.failures" :key="failure.id" class="flex gap-2">
              <span class="shrink-0 font-mono text-gray-300">{{ failure.label }}</span>
              <span class="min-w-0 break-words text-red-300">{{ failure.message }}</span>
            </li>
          </ul>
        </details>
      </div>

      <!-- 状态筛选、搜索、排序 -->
      <div class="flex flex-wrap items-center gap-2">
        <div class="seg">
          <button
            class="seg-item"
            :class="{ active: stateFilter === 'all' }"
            type="button"
            @click="stateFilter = 'all'"
          >
            {{ t('accounts.filterAll') }}
            <span class="seg-count">{{ accounts.length }}</span>
          </button>
          <button
            v-for="state in filterStates"
            :key="state"
            class="seg-item"
            :class="{ active: stateFilter === state }"
            type="button"
            @click="stateFilter = state"
          >
            <span class="dot" :class="stateDotClass[state]"></span>
            {{ t(stateKeys[state]) }}
            <span class="seg-count">{{ stateCounts[state] }}</span>
          </button>
        </div>
        <input
          v-model="query"
          class="input min-w-40 flex-1 sm:max-w-80"
          type="search"
          :placeholder="t('accounts.search')"
          :aria-label="t('accounts.search')"
        />
        <select v-model="workerFilter" class="input" :aria-label="t('accounts.worker')">
          <option v-for="option in workerFilterOptions" :key="option.value" :value="option.value">
            {{ t(option.label) }}
          </option>
        </select>
        <select v-model="tierFilter" class="input" :aria-label="t('accounts.benefitTier')">
          <option v-for="option in tierFilterOptions" :key="option.value" :value="option.value">
            {{ t(option.label) }} ({{ tierCounts[option.value] }})
          </option>
        </select>
        <select v-model="sortKey" class="input" :aria-label="t('accounts.sortDefault')">
          <option v-for="option in sortOptions" :key="option.value" :value="option.value">
            {{ t(option.label) }}
          </option>
        </select>
      </div>

      <!-- 选择工具条 -->
      <div
        v-if="selected.size > 0"
        class="flex flex-wrap items-center gap-2 rounded-md border border-blue-500/40 bg-blue-500/10 px-3 py-2 text-xs"
      >
        <strong class="text-blue-300">{{ tf('accounts.selected', { count: selected.size }) }}</strong>
        <button
          v-if="selected.size < filtered.length"
          class="text-blue-400 hover:text-blue-300"
          type="button"
          @click="selectAllFiltered"
        >
          {{ tf('accounts.selectFiltered', { count: filtered.length }) }}
        </button>
        <span class="flex-1"></span>
        <button class="btn btn-sm" type="button" :disabled="busy" @click="bulkSelected('enable')">
          {{ t('common.enable') }}
        </button>
        <button class="btn btn-sm" type="button" :disabled="busy" @click="bulkSelected('disable')">
          {{ t('common.disable') }}
        </button>
        <button class="btn btn-sm" type="button" :disabled="busy" @click="openVerifyDialog">
          {{ t('common.verify') }}
        </button>
        <button
          class="btn btn-sm btn-danger"
          type="button"
          :disabled="busy"
          @click="bulkSelected('delete')"
        >
          {{ t('common.delete') }}
        </button>
        <button
          class="btn btn-sm"
          type="button"
          @click="copyEmails(selectedAccounts.map((account) => account.label))"
        >
          {{ t('accounts.copyEmails') }}
        </button>
        <button class="btn btn-sm btn-ghost" type="button" @click="clearSelection">
          {{ t('accounts.clearSelection') }}
        </button>
      </div>

      <!-- 列表 -->
      <div v-if="error" class="rounded border border-red-500/40 bg-red-500/10 p-4 text-red-300">
        {{ error }}
      </div>
      <div v-else-if="loading" class="py-12 text-center text-gray-500">
        {{ t('common.loading') }}
      </div>
      <div v-else-if="accounts.length === 0" class="panel py-10 text-center text-gray-500">
        <p class="mb-2">{{ t('accounts.empty') }}</p>
        <p class="text-xs">{{ t('accounts.emptyHint') }}</p>
      </div>
      <div v-else class="panel">
        <div class="overflow-x-auto">
          <table class="data-table" :class="tableMinWidth">
            <colgroup>
              <col class="w-10" />
              <col class="w-28" />
              <col :class="startupPauseShown ? 'w-44' : 'w-24'" />
              <col />
              <col :class="poolTagShown ? 'w-32' : 'w-20'" />
              <col class="w-16" />
              <col class="w-44" />
              <col class="w-44" />
              <col class="w-[17.5rem]" />
            </colgroup>
            <thead>
              <tr>
                <th>
                  <input
                    type="checkbox"
                    :checked="pageAllSelected"
                    :aria-label="t('accounts.selectPage')"
                    @change="togglePageSelection"
                  />
                </th>
                <th>{{ t('accounts.state') }}</th>
                <th>{{ t('accounts.worker') }}</th>
                <th>{{ t('accounts.account') }}</th>
                <th>{{ t('accounts.benefitTier') }}</th>
                <th>{{ t('accounts.models') }}</th>
                <th>{{ t('accounts.proxy') }}</th>
                <th>{{ t('accounts.region') }}</th>
                <th class="text-right">{{ t('accounts.actions') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="filtered.length === 0">
                <td colspan="9" class="py-8 text-center text-gray-500">{{ t('accounts.noMatch') }}</td>
              </tr>
              <tr
                v-for="account in pageItems"
                :key="account.id"
                :class="{ 'is-selected': isSelected(account), 'opacity-60': !account.enabled }"
              >
                <td>
                  <input
                    type="checkbox"
                    :checked="isSelected(account)"
                    :aria-label="account.label"
                    @change="toggleSelected(account)"
                  />
                </td>
                <td>
                  <span class="flex items-center gap-2 text-xs" :class="stateTextClass[account.state]">
                    <span class="dot" :class="stateDotClass[account.state]"></span>
                    {{ t(stateKeys[account.state]) }}
                  </span>
                </td>
                <td>
                  <span
                    class="flex items-center gap-2 text-xs"
                    :class="workerTextClass[workerState(account)]"
                  >
                    <span class="dot" :class="workerDotClass[workerState(account)]"></span>
                    {{ t(workerLabelKeys[workerState(account)]) }}
                  </span>
                  <!-- 启动反复失败、暂停调度中的账户 -->
                  <div
                    v-if="startupPauses.has(account.id)"
                    class="mt-0.5 text-[11px] leading-4 text-yellow-400"
                    :title="startupPauseDetail(account)"
                  >
                    {{ startupPauseText(account) }}
                  </div>
                </td>
                <td>
                  <div v-tooltip="account.label" class="cell-truncate font-mono text-gray-200">
                    {{ account.label }}
                  </div>
                  <div
                    v-if="account.message"
                    v-tooltip="account.message"
                    class="cell-truncate text-[11px] text-red-400"
                  >
                    {{ account.message }}
                  </div>
                </td>
                <td class="cell-truncate text-xs text-gray-300">
                  <span
                    v-if="accountTier(account) === 'Ultra'"
                    v-tooltip="t('accounts.ultraCountHelp')"
                    class="tag tag-ultra"
                    >{{ t('pool.ultra') }}</span
                  >
                  <span
                    v-else-if="accountTier(account) === 'unknown'"
                    v-tooltip="t('accounts.tierUnknownHelp')"
                    class="text-gray-500"
                    >{{ t('accounts.tierUnknown') }}</span
                  >
                  <template v-else>
                    {{ accountTier(account) }}
                    <!-- 权益不是 Ultra 却属于 Ultra 号池（开启 ULTRA_INCLUDE_PRO 的 Pro 账户）：权益照常显示，另加号池标记 -->
                    <span
                      v-if="inUltraPool(account)"
                      v-tooltip="t('accounts.proInUltraHelp')"
                      class="tag tag-ultra"
                      tabindex="0"
                      >{{ t('accounts.poolUltraTag') }}</span
                    >
                  </template>
                </td>
                <td class="font-mono text-xs text-gray-300">
                  {{ account.models.length === 0 ? '—' : account.models.length }}
                </td>
                <td>
                  <div v-tooltip="account.proxy" class="cell-truncate text-xs text-gray-400">
                    {{ account.proxy || t('accounts.direct') }}
                  </div>
                </td>
                <td>
                  <div class="cell-truncate text-xs text-gray-400">
                    {{ account.locale }} · {{ account.timezone }}
                  </div>
                </td>
                <td>
                  <div class="flex justify-end gap-1">
                    <button
                      class="btn btn-sm"
                      type="button"
                      :disabled="busy"
                      @click="beginEdit(account)"
                    >
                      {{ t('common.edit') }}
                    </button>
                    <button
                      class="btn btn-sm"
                      type="button"
                      :disabled="busy"
                      :aria-busy="pendingAction === `toggle:${account.id}`"
                      @click="toggleAccount(account)"
                    >
                      {{ t(account.enabled ? 'common.disable' : 'common.enable') }}
                    </button>
                    <button
                      v-if="account.state === 'auth_required'"
                      class="btn btn-sm btn-success"
                      type="button"
                      :disabled="busy || !account.enabled"
                      :aria-busy="pendingAction === `login:${account.id}`"
                      @click="runAccountAction(account, 'login')"
                    >
                      {{ t('common.relogin') }}
                    </button>
                    <button
                      class="btn btn-sm"
                      type="button"
                      :disabled="busy || !account.enabled"
                      :aria-busy="pendingAction === `verify:${account.id}`"
                      @click="runAccountAction(account, 'verify')"
                    >
                      {{ t('common.verify') }}
                    </button>
                    <button
                      class="btn btn-sm btn-danger"
                      type="button"
                      :disabled="busy"
                      :aria-busy="pendingAction === `delete:${account.id}`"
                      @click="removeAccount(account)"
                    >
                      {{ t('common.delete') }}
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <div class="border-t border-[#30363d] px-3 py-2">
          <UiPager v-model:page="page" v-model:page-size="pageSize" :total="filtered.length" />
        </div>
      </div>
    </div>

    <Teleport to="body">
      <!-- 批量验证 -->
      <div v-if="showVerifyDialog" class="modal-overlay" @click.self="showVerifyDialog = false">
        <form
          class="mx-4 w-full max-w-md rounded-lg border border-[#30363d] bg-[#161b22] p-6 shadow-xl"
          @submit.prevent="startVerify"
        >
          <div class="mb-4 flex items-center justify-between">
            <h3 class="text-lg font-bold text-white">{{ t('accounts.verifyTitle') }}</h3>
            <button
              class="rounded p-1 text-gray-500 hover:bg-[#30363d] hover:text-white"
              type="button"
              :aria-label="t('common.close')"
              @click="showVerifyDialog = false"
            >
              <UiIcon name="close" :size="16" />
            </button>
          </div>
          <div class="space-y-2 text-sm text-gray-300">
            <label class="flex cursor-pointer items-center gap-2">
              <input v-model="verifyScope" type="radio" value="problem" class="accent-blue-600" />
              {{ tf('accounts.verifyScopeProblem', { count: problemAccounts.length }) }}
            </label>
            <label class="flex cursor-pointer items-center gap-2">
              <input v-model="verifyScope" type="radio" value="enabled" class="accent-blue-600" />
              {{ tf('accounts.verifyScopeEnabled', { count: enabledAccounts.length }) }}
            </label>
            <label class="flex cursor-pointer items-center gap-2">
              <input v-model="verifyScope" type="radio" value="selection" class="accent-blue-600" />
              {{ tf('accounts.verifyScopeSelection', { count: selectionScopeAccounts.length }) }}
            </label>
          </div>
          <label class="mt-4 flex items-center justify-between gap-3 text-sm text-gray-400">
            {{ t('accounts.verifyConcurrency') }}
            <select v-model.number="verifyConcurrency" class="input w-24">
              <option :value="1">1</option>
              <option :value="2">2</option>
              <option :value="3">3</option>
              <option :value="4">4</option>
            </select>
          </label>
          <p class="mt-3 text-xs leading-5 text-gray-500">{{ t('accounts.verifyHint') }}</p>
          <div class="mt-6 flex gap-2">
            <button class="btn flex-1" type="button" @click="showVerifyDialog = false">
              {{ t('common.cancel') }}
            </button>
            <button
              class="btn btn-primary flex-1"
              type="submit"
              :disabled="verifyTargets.length === 0"
            >
              {{ t('accounts.start') }} ({{ verifyTargets.length }})
            </button>
          </div>
        </form>
      </div>

      <!-- 编辑账户 -->
      <div v-if="showEditor" class="modal-overlay" @click.self="closeEditor">
        <form
          class="mx-4 w-full max-w-md rounded-lg border border-[#30363d] bg-[#161b22] p-6 shadow-xl"
          @submit.prevent="saveAccount"
        >
          <div class="mb-4 flex items-center justify-between">
            <h3 class="text-lg font-bold text-white">{{ t('accounts.editTitle') }}</h3>
            <button
              class="rounded p-1 text-gray-500 hover:bg-[#30363d] hover:text-white"
              type="button"
              :aria-label="t('common.close')"
              @click="closeEditor"
            >
              <UiIcon name="close" :size="16" />
            </button>
          </div>
          <p class="mb-4 truncate font-mono text-sm text-gray-300">{{ draft.label }}</p>
          <div class="space-y-4">
            <label class="flex items-center justify-between">
              <span class="text-sm font-medium text-gray-400">{{ t('common.enable') }}</span>
              <input v-model="draft.enabled" class="h-4 w-4 accent-blue-600" type="checkbox" />
            </label>
            <label class="block">
              <span class="mb-1 block text-sm font-medium text-gray-400">{{
                t('accounts.proxy')
              }}</span>
              <input
                v-model.trim="draft.proxy"
                class="input w-full"
                placeholder="socks5://127.0.0.1:1080"
              />
            </label>
            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.locale')
                }}</span>
                <input v-model.trim="draft.locale" class="input w-full" required />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.timezone')
                }}</span>
                <input v-model.trim="draft.timezone" class="input w-full" required />
              </label>
            </div>
          </div>
          <div class="mt-6 flex gap-2">
            <button class="btn flex-1" type="button" @click="closeEditor">
              {{ t('common.cancel') }}
            </button>
            <button
              class="btn btn-primary flex-1"
              type="submit"
              :disabled="pendingAction !== ''"
              :aria-busy="pendingAction.startsWith('edit:')"
            >
              {{ t('common.save') }}
            </button>
          </div>
        </form>
      </div>

      <!-- 浏览器登录 -->
      <div v-if="showBrowserLogin" class="modal-overlay" @click.self="closeBrowserLogin">
        <form
          class="mx-4 w-full max-w-md rounded-lg border border-[#30363d] bg-[#161b22] p-6 shadow-xl"
          @submit.prevent="beginBrowserLogin"
        >
          <div class="mb-4 flex items-center justify-between">
            <h3 class="text-lg font-bold text-white">{{ t('accounts.browserLogin') }}</h3>
            <button
              class="rounded p-1 text-gray-500 hover:bg-[#30363d] hover:text-white disabled:opacity-50"
              type="button"
              :disabled="pendingAction === 'browser-login'"
              :aria-label="t('common.close')"
              @click="closeBrowserLogin"
            >
              <UiIcon name="close" :size="16" />
            </button>
          </div>
          <div class="space-y-4">
            <label class="block">
              <span class="mb-1 block text-sm font-medium text-gray-400">{{
                t('accounts.proxy')
              }}</span>
              <input
                v-model.trim="accountEnvironment.proxy"
                class="input w-full"
                placeholder="socks5://127.0.0.1:1080"
              />
            </label>
            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.locale')
                }}</span>
                <input v-model.trim="accountEnvironment.locale" class="input w-full" required />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.timezone')
                }}</span>
                <input v-model.trim="accountEnvironment.timezone" class="input w-full" required />
              </label>
            </div>
          </div>
          <div class="mt-6 flex gap-2">
            <button
              class="btn flex-1"
              type="button"
              :disabled="pendingAction === 'browser-login'"
              @click="closeBrowserLogin"
            >
              {{ t('common.cancel') }}
            </button>
            <button
              class="btn btn-primary flex-1"
              type="submit"
              :disabled="pendingAction !== ''"
              :aria-busy="pendingAction === 'browser-login'"
            >
              {{ t('accounts.browserLogin') }}
            </button>
          </div>
        </form>
      </div>

      <!-- Chrome 导入 -->
      <div v-if="showChromeImport" class="modal-overlay" @click.self="closeChromeImport">
        <form
          class="mx-4 w-full max-w-lg rounded-lg border border-[#30363d] bg-[#161b22] p-6 shadow-xl"
          @submit.prevent="importChromeAccounts"
        >
          <div class="mb-4 flex items-center justify-between">
            <h3 class="text-lg font-bold text-white">{{ t('accounts.chromeTitle') }}</h3>
            <button
              class="rounded p-1 text-gray-500 hover:bg-[#30363d] hover:text-white disabled:opacity-50"
              type="button"
              :disabled="pendingAction === 'chrome-import'"
              :aria-label="t('common.close')"
              @click="closeChromeImport"
            >
              <UiIcon name="close" :size="16" />
            </button>
          </div>
          <div class="mb-4 space-y-4">
            <label class="block">
              <span class="mb-1 block text-sm font-medium text-gray-400">{{
                t('accounts.proxy')
              }}</span>
              <input
                v-model.trim="accountEnvironment.proxy"
                class="input w-full"
                placeholder="socks5://127.0.0.1:1080"
              />
            </label>
            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.locale')
                }}</span>
                <input v-model.trim="accountEnvironment.locale" class="input w-full" required />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.timezone')
                }}</span>
                <input v-model.trim="accountEnvironment.timezone" class="input w-full" required />
              </label>
            </div>
          </div>
          <div v-if="chromeProfiles.length === 0" class="py-8 text-center text-sm text-gray-500">
            {{ t('accounts.chromeEmpty') }}
          </div>
          <div v-else class="mb-2 flex justify-end">
            <button
              class="text-xs text-blue-400 transition hover:text-blue-300"
              type="button"
              @click="toggleAllChromeProfiles"
            >
              {{ allChromeProfilesSelected ? t('accounts.deselectAll') : t('accounts.selectAll') }}
            </button>
          </div>
          <div v-if="chromeProfiles.length > 0" class="max-h-[50vh] space-y-2 overflow-auto">
            <label
              v-for="profile in chromeProfiles"
              :key="profile.id"
              class="flex cursor-pointer items-start gap-3 rounded border border-[#30363d] bg-[#0d1117] p-3 transition hover:border-[#4b5563]"
            >
              <input
                v-model="selectedChromeProfiles"
                class="mt-1 h-4 w-4 shrink-0 accent-blue-600"
                type="checkbox"
                :value="profile.id"
              />
              <span class="min-w-0 flex-1">
                <strong class="block truncate text-sm text-white">{{ profile.email }}</strong>
                <span class="block truncate text-xs text-gray-400">{{ profile.display_name }}</span>
                <span class="block truncate font-mono text-xs text-gray-500">{{
                  profile.profile
                }}</span>
              </span>
            </label>
          </div>
          <div class="mt-6 flex gap-2">
            <button
              class="btn flex-1"
              type="button"
              :disabled="pendingAction === 'chrome-import'"
              @click="closeChromeImport"
            >
              {{ t('common.cancel') }}
            </button>
            <button
              class="btn btn-primary flex-1"
              type="submit"
              :disabled="pendingAction !== '' || selectedChromeProfiles.length === 0"
              :aria-busy="pendingAction === 'chrome-import'"
            >
              {{ t('accounts.importSelected') }}
            </button>
          </div>
        </form>
      </div>
    </Teleport>
  </section>
</template>
