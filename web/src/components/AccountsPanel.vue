<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { api } from '@/api'
import { emptyBulkState, runBulk, type BulkAction, type BulkState } from '@/bulk'
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
  cooldowns: Cooldown[]
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

// workerState 账户 Worker 的实时状态：运行中、启动中或未启动
function workerState(account: Account): WorkerState {
  if (props.workerSets.warm.has(account.id)) return 'warm'
  if (props.workerSets.starting.has(account.id)) return 'starting'
  return 'none'
}

const query = ref('')
const stateFilter = ref<StateFilter>('all')
const workerFilter = ref<WorkerFilter>('all')
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

const filtered = computed(() => {
  const term = query.value.trim().toLowerCase()
  const list = props.accounts.filter((account) => {
    if (stateFilter.value !== 'all' && account.state !== stateFilter.value) return false
    if (workerFilter.value !== 'all' && workerState(account) !== workerFilter.value) return false
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

watch([query, stateFilter, workerFilter, sortKey], () => {
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

// enableAll 一键启用全部已停用账户
async function enableAll(): Promise<void> {
  const targets = disabledAccounts.value
  if (targets.length === 0) {
    emit('notice', t('accounts.noneDisabled'), 'success')
    return
  }
  if (!(await confirmAction(tf('accounts.enableAllConfirm', { count: targets.length }), t('common.enable'))))
    return
  await startBulk('enable', targets, 4, (account) => setEnabled(account, true))
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
  await startBulk(
    'enable',
    targets.filter((account) => !account.enabled),
    4,
    (account) => setEnabled(account, true),
  )
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
        <div class="flex items-baseline gap-3">
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
          class="input min-w-0 flex-1 sm:max-w-80"
          type="search"
          :placeholder="t('accounts.search')"
          :aria-label="t('accounts.search')"
        />
        <select v-model="workerFilter" class="input" :aria-label="t('accounts.worker')">
          <option v-for="option in workerFilterOptions" :key="option.value" :value="option.value">
            {{ t(option.label) }}
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
          <table class="data-table min-w-[1060px]">
            <colgroup>
              <col class="w-10" />
              <col class="w-28" />
              <col class="w-24" />
              <col />
              <col class="w-20" />
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
                <td class="cell-truncate text-xs text-gray-300">{{ account.benefit_tier || '—' }}</td>
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
