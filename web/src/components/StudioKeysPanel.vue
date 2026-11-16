<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api } from '@/api'
import { copyText } from '@/clipboard'
import { confirmAction } from '@/confirm'
import { useI18n, type TranslationKey } from '@/i18n'
import { readStorage, writeStorage } from '@/storage'
import type { StudioKeyClearCounts, StudioKeyRecord, StudioKeyStatus } from '@/types'
import UiPager from './UiPager.vue'

// AI Studio API Key：显示获取队列与每个账户的记录（Key 只显示末四位），复制按钮按需读取完整 Key、不在页面里保存。
// 获取由账户行的“获取 Key”按钮或选中账户后的批量按钮触发（见 AccountsPanel），这里只轮询状态。
// 记录按成功、失败、跳过分标签列出，各自按最近一次尝试从新到旧排序、每页 10 条：账户多时失败原因不再整列铺开。
// 失败、跳过的记录可以逐条删除或整栏清除，成功标签里保留着上次 Key 的失败信息可以清掉（服务端从不删除已保存的 Key）；
// 缺项目编号的 Key 可以“补项目信息”（服务端只读打开页面，不创建 Key 与项目）
const { t, tf, locale } = useI18n()
const emit = defineEmits<{ notice: [message: string, kind: 'success' | 'error'] }>()

const POLL_RUNNING = 3000
const POLL_IDLE = 20000
const PAGE_SIZE = 10
const TAB_STORAGE_KEY = 'aistudio2api_studio_keys_tab'
// CLEAR_LIMIT 为一次清除请求最多的账户数（与服务端一致），更多时分批
const CLEAR_LIMIT = 5000

// KeyTab：ok 已有 Key（最近一次失败但保留着上次的 Key 也算）；failed 没有 Key 且失败或页面上没有 Key；skipped 没有 Key 且被跳过
type KeyTab = 'ok' | 'failed' | 'skipped'
const TABS: readonly KeyTab[] = ['ok', 'failed', 'skipped']
const tabKeys: Record<KeyTab, TranslationKey> = {
  ok: 'studioKey.tab.ok',
  failed: 'studioKey.tab.failed',
  skipped: 'studioKey.tab.skipped',
}
const tabDotClass: Record<KeyTab, string> = {
  ok: 'bg-green-500',
  failed: 'bg-red-500',
  skipped: 'bg-gray-500',
}
const emptyKeys: Record<KeyTab, TranslationKey> = {
  ok: 'studioKey.emptyOk',
  failed: 'studioKey.emptyFailed',
  skipped: 'studioKey.emptySkipped',
}

const status = ref<StudioKeyStatus | null>(null)
const error = ref('')
const expanded = ref(false)
const copying = ref('')
const busy = ref(false)
let pollTimer: number | undefined

const statusKeys: Record<string, TranslationKey> = {
  ok: 'studioKey.status.ok',
  none: 'studioKey.status.none',
  failed: 'studioKey.status.failed',
  skipped: 'studioKey.status.skipped',
}
const statusClass: Record<string, string> = {
  ok: 'text-green-400',
  none: 'text-yellow-400',
  failed: 'text-red-400',
  skipped: 'text-gray-400',
}

const records = computed<StudioKeyRecord[]>(() => status.value?.records ?? [])
const delaySeconds = computed(() => status.value?.delay_seconds ?? 30)

function recordTab(record: StudioKeyRecord): KeyTab {
  if (record.has_key) return 'ok'
  return record.status === 'skipped' ? 'skipped' : 'failed'
}

function labelOf(record: StudioKeyRecord): string {
  return record.label || record.account_id
}

// latestTime 为最近一次尝试的时间（旧记录没有 attempt_at 时用读取或创建时间）
function latestTime(record: StudioKeyRecord): string | undefined {
  return record.attempt_at || record.fetched_at || record.created_at
}

function timeValue(record: StudioKeyRecord): number {
  const value = latestTime(record)
  const time = value ? Date.parse(value) : Number.NaN
  return Number.isNaN(time) ? 0 : time
}

// grouped 把记录分到三个标签，各自按最近一次尝试从新到旧排序，新结果排在最前面
const grouped = computed<Record<KeyTab, StudioKeyRecord[]>>(() => {
  const groups: Record<KeyTab, StudioKeyRecord[]> = { ok: [], failed: [], skipped: [] }
  for (const record of records.value) groups[recordTab(record)].push(record)
  for (const list of Object.values(groups)) {
    list.sort(
      (left, right) =>
        timeValue(right) - timeValue(left) || labelOf(left).localeCompare(labelOf(right)),
    )
  }
  return groups
})
const withKey = computed(() => grouped.value.ok.length)
// missingProject 为保存着 Key 却没有项目编号的记录（“补项目信息”的对象）
const missingProject = computed(() => grouped.value.ok.filter((record) => !record.project_number))

// 标签：记住每个浏览器上次选的标签；第一次读到记录时，上次选的标签没有记录就按成功、失败、跳过的顺序停在第一个有记录的标签
const tab = ref<KeyTab>('ok')
let tabSettled = false
const storedTab = TABS.find((item) => item === readStorage(TAB_STORAGE_KEY))

function settleTab(): void {
  if (tabSettled || records.value.length === 0) return
  tabSettled = true
  if (storedTab !== undefined && grouped.value[storedTab].length > 0) tab.value = storedTab
  else tab.value = TABS.find((item) => grouped.value[item].length > 0) ?? 'ok'
}

function selectTab(value: KeyTab): void {
  tabSettled = true
  tab.value = value
  writeStorage(TAB_STORAGE_KEY, value)
}

// 分页：每页 10 条；切换标签回到第一页，轮询时列表变短则停在最后一页
const page = ref(1)
const tabRecords = computed(() => grouped.value[tab.value])
const pageCount = computed(() => Math.max(1, Math.ceil(tabRecords.value.length / PAGE_SIZE)))
const pageRecords = computed(() => {
  const start = (Math.min(page.value, pageCount.value) - 1) * PAGE_SIZE
  return tabRecords.value.slice(start, start + PAGE_SIZE)
})
watch(tab, () => {
  page.value = 1
})
watch(pageCount, (count) => {
  if (page.value > count) page.value = count
})

// 长账户名与失败原因只占一行（悬停看全文），点击展开、再点收起；选中文字时不切换
const opened = ref<Set<string>>(new Set())

function isOpen(key: string): boolean {
  return opened.value.has(key)
}

function toggleOpen(key: string): void {
  if ((window.getSelection()?.toString() ?? '') !== '') return
  const next = new Set(opened.value)
  if (next.has(key)) next.delete(key)
  else next.add(key)
  opened.value = next
}

function clipClass(key: string): string {
  return isOpen(key) ? 'whitespace-pre-wrap [overflow-wrap:anywhere]' : 'cell-truncate'
}

const queueText = computed(() => {
  const value = status.value
  if (value === null) return ''
  if (!value.running) return t('studioKey.idle')
  return tf('studioKey.running', {
    current: value.current
      ? value.current + (value.current_fill ? t('studioKey.fillMark') : '')
      : '-',
    queued: value.queue.length,
    delay: value.delay_seconds,
  })
})

function statusLabel(value: string): string {
  const key = statusKeys[value]
  return key === undefined ? value : t(key)
}

// formatTime 按界面语言显示到秒的 24 小时制时间：不带上午/下午，英文界面下“最近尝试”一行也放得进时间列
function formatTime(value?: string): string {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '-'
  return date.toLocaleString(locale.value, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hourCycle: 'h23',
  })
}

// projectText 为项目列的主文字：显示名优先，没有时为 projects/<编号> 或项目 ID；projectSecondary 为显示名后面灰色的 projects/<编号>
function projectText(record: StudioKeyRecord): string {
  return record.project_display || record.project_name || record.project_id || '-'
}

function projectSecondary(record: StudioKeyRecord): string {
  return record.project_display ? (record.project_name ?? '') : ''
}

// projectTitle 为项目列的悬停全文：显示名、projects/<编号> 与项目 ID
function projectTitle(record: StudioKeyRecord): string {
  const parts = [record.project_display, record.project_name].filter(Boolean) as string[]
  if (record.project_id) parts.push(tf('studioKey.projectIdTitle', { id: record.project_id }))
  return parts.length > 0 ? parts.join(' · ') : '-'
}

// timeLines 为成功标签时间列的各行（读取、创建、最近一次没成功的尝试），每行另作悬停全文
function timeLines(record: StudioKeyRecord): string[] {
  const lines = [`${t('studioKey.fetchedAt')} ${formatTime(record.fetched_at)}`]
  if (record.created_at) lines.push(`${t('studioKey.createdAt')} ${formatTime(record.created_at)}`)
  if (record.attempt_at && record.status !== 'ok') {
    lines.push(`${t('studioKey.attemptAt')} ${formatTime(record.attempt_at)}`)
  }
  return lines
}

// refresh 读取状态；任务进行中时更快轮询
async function refresh(): Promise<void> {
  try {
    status.value = await api.studioKeys()
    error.value = ''
    settleTab()
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause)
  }
  schedule()
}

function schedule(): void {
  if (pollTimer !== undefined) window.clearTimeout(pollTimer)
  pollTimer = window.setTimeout(
    () => void refresh(),
    status.value?.running ? POLL_RUNNING : POLL_IDLE,
  )
}

// fetchKeys 把账户加入获取队列（AccountsPanel 调用）
async function fetchKeys(ids: string[]): Promise<void> {
  if (ids.length === 0) return
  try {
    status.value = await api.fetchStudioKeys(ids)
    expanded.value = true
    settleTab()
    emit('notice', tf('studioKey.queued', { count: ids.length }), 'success')
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  }
  schedule()
}

// fetchAll 全部账户排队，只补还没有 Key 的；服务端逐个处理，号与号之间按间隔等待
async function fetchAll(): Promise<void> {
  if (!window.confirm(tf('studioKey.fetchAllConfirm', { delay: delaySeconds.value }))) return
  busy.value = true
  try {
    status.value = await api.fetchAllStudioKeys()
    expanded.value = true
    settleTab()
    emit(
      'notice',
      status.value.running ? t('studioKey.queuedAll') : t('studioKey.allDone'),
      'success',
    )
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  } finally {
    busy.value = false
  }
  schedule()
}

// retryFailed 把“失败”标签里的全部账户（包括其他页的）重新排队，确认后才提交
async function retryFailed(): Promise<void> {
  const ids = grouped.value.failed.map((record) => record.account_id)
  if (ids.length === 0) return
  const message = tf('studioKey.retryFailedConfirm', {
    count: ids.length,
    delay: delaySeconds.value,
  })
  if (!(await confirmAction(message, t('studioKey.retry')))) return
  busy.value = true
  try {
    await fetchKeys(ids)
  } finally {
    busy.value = false
  }
}

// copyFailedEmails 复制“失败”标签里全部账户的邮箱（包括其他页的），一行一个
async function copyFailedEmails(): Promise<void> {
  const labels = grouped.value.failed.map(labelOf)
  if (labels.length === 0) return
  const copied = await copyText(labels.join('\n'))
  emit(
    'notice',
    copied
      ? tf('studioKey.copiedFailedEmails', { count: labels.length })
      : t('studioKey.copyFailed'),
    copied ? 'success' : 'error',
  )
}

// clearRecords 清除这些账户的记录（服务端：没有 Key 的删掉，保存着 Key 的只清掉失败或跳过的信息，从不删除 Key；正在处理的不动），
// 确认后才提交；超过 CLEAR_LIMIT 个时分批。清除后用返回的最新状态刷新列表（页码由 watch(pageCount) 收回到最后一页）
async function clearRecords(ids: string[], message: string): Promise<void> {
  if (ids.length === 0 || busy.value) return
  if (!(await confirmAction(message, t('studioKey.clear')))) return
  busy.value = true
  const total: StudioKeyClearCounts = {
    deleted: 0,
    reset: 0,
    busy: 0,
    unknown: 0,
    unchanged: 0,
    errors: 0,
  }
  try {
    for (let start = 0; start < ids.length; start += CLEAR_LIMIT) {
      const result = await api.clearStudioKeys(ids.slice(start, start + CLEAR_LIMIT))
      status.value = result
      total.deleted += result.cleared.deleted
      total.reset += result.cleared.reset
      total.busy += result.cleared.busy
      total.unknown += result.cleared.unknown
      total.errors += result.cleared.errors
    }
    // 有账户的记录读写失败（没有动）时用错误样式，提示里写明个数
    emit('notice', clearedText(total), total.errors > 0 ? 'error' : 'success')
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  } finally {
    busy.value = false
  }
  schedule()
}

// processing 判断这条记录的账户正在处理（服务端清除时也不动它，按钮先禁用）
function processing(record: StudioKeyRecord): boolean {
  return status.value?.running === true && status.value.current === record.account_id
}

// clearedText 为清除之后的提示：只写不为零的计数；正在处理没动的、已不在账户列表里的、记录读写失败没动的账户各另写一句
// （有读写失败时不写“没有需要清除的记录”）
function clearedText(counts: StudioKeyClearCounts): string {
  const parts: string[] = []
  if (counts.deleted > 0) parts.push(tf('studioKey.clearedDeleted', { count: counts.deleted }))
  if (counts.reset > 0) parts.push(tf('studioKey.clearedReset', { count: counts.reset }))
  const sentences: string[] = []
  if (parts.length > 0) {
    sentences.push(tf('studioKey.cleared', { parts: parts.join(t('studioKey.clearedJoin')) }))
  } else if (counts.errors === 0) {
    sentences.push(t('studioKey.clearedNothing'))
  }
  if (counts.busy > 0) sentences.push(tf('studioKey.clearedBusy', { busy: counts.busy }))
  if (counts.unknown > 0) sentences.push(tf('studioKey.clearedUnknown', { count: counts.unknown }))
  if (counts.errors > 0) sentences.push(tf('studioKey.clearedErrors', { count: counts.errors }))
  return sentences.join(t('studioKey.clearedSentence'))
}

// deleteRecord 删除失败或跳过标签里的一条记录
function deleteRecord(record: StudioKeyRecord): void {
  void clearRecords(
    [record.account_id],
    tf('studioKey.deleteRecordConfirm', { label: labelOf(record), tab: t(tabKeys[tab.value]) }),
  )
}

// clearTab 清除当前标签（失败或跳过）里的全部记录（包括其他页的）
function clearTab(): void {
  const ids = tabRecords.value.map((record) => record.account_id)
  void clearRecords(
    ids,
    tf('studioKey.clearTabConfirm', { tab: t(tabKeys[tab.value]), count: ids.length }),
  )
}

// clearFailure 清掉成功标签里保留着上次 Key 的记录最近一次失败的信息（Key 与项目不动）
function clearFailure(record: StudioKeyRecord): void {
  void clearRecords(
    [record.account_id],
    tf('studioKey.clearFailureConfirm', { label: labelOf(record) }),
  )
}

// fillProjects 给保存着 Key 却没有项目编号的账户补项目信息（服务端只读打开页面，不创建），确认后才提交
async function fillProjects(): Promise<void> {
  const count = missingProject.value.length
  if (count === 0 || busy.value) return
  const message = tf('studioKey.fillProjectsConfirm', { count, delay: delaySeconds.value })
  if (!(await confirmAction(message, t('studioKey.fillProjectsAction')))) return
  busy.value = true
  try {
    status.value = await api.fillStudioKeyProjects()
    expanded.value = true
    emit(
      'notice',
      status.value.running ? tf('studioKey.fillQueued', { count }) : t('studioKey.fillNone'),
      'success',
    )
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  } finally {
    busy.value = false
  }
  schedule()
}

async function stopAll(): Promise<void> {
  busy.value = true
  try {
    status.value = await api.stopStudioKeys()
    emit('notice', t('studioKey.stopped'), 'success')
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  } finally {
    busy.value = false
  }
  schedule()
}

// copyAll 读取全部完整 Key，一行一个复制到剪贴板，不保存在页面状态里
async function copyAll(): Promise<void> {
  busy.value = true
  try {
    const exported = await api.exportStudioKeys()
    if (exported.keys.length === 0) {
      emit('notice', t('studioKey.noKeys'), 'error')
      return
    }
    const copied = await copyText(exported.keys.map((item) => item.key).join('\n'))
    emit(
      'notice',
      copied
        ? tf('studioKey.copiedAll', { count: exported.keys.length })
        : t('studioKey.copyFailed'),
      copied ? 'success' : 'error',
    )
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  } finally {
    busy.value = false
  }
}

// copyKey 读取完整 Key 并复制，不保存在页面状态里
async function copyKey(record: StudioKeyRecord): Promise<void> {
  copying.value = record.account_id
  try {
    const secret = await api.revealStudioKey(record.account_id)
    const copied = await copyText(secret.key)
    emit(
      'notice',
      copied ? t('studioKey.copied') : t('studioKey.copyFailed'),
      copied ? 'success' : 'error',
    )
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  } finally {
    copying.value = ''
  }
}

// delaySeconds 供账户页批量获取的确认框写明号与号之间的间隔
defineExpose({ fetchKeys, refresh, delaySeconds })

onMounted(() => void refresh())
onBeforeUnmount(() => {
  if (pollTimer !== undefined) window.clearTimeout(pollTimer)
})
</script>

<template>
  <section class="panel px-4 py-3">
    <div class="flex flex-wrap items-center gap-x-3 gap-y-2">
      <span class="dot" :class="status?.running ? 'bg-green-500' : 'bg-gray-600'"></span>
      <span class="font-bold text-gray-100">{{ t('studioKey.title') }}</span>
      <span class="min-w-0 text-xs text-gray-400 [overflow-wrap:anywhere]">{{ queueText }}</span>
      <span v-if="records.length > 0" class="tag gap-1 text-xs">
        <span class="text-green-400">{{ t('studioKey.tab.ok') }} {{ grouped.ok.length }}</span>
        <span class="text-gray-600">·</span>
        <span class="text-red-400"
          >{{ t('studioKey.tab.failed') }} {{ grouped.failed.length }}</span
        >
        <span class="text-gray-600">·</span>
        <span class="text-gray-400">
          {{ t('studioKey.tab.skipped') }} {{ grouped.skipped.length }}
        </span>
      </span>
      <span v-if="error" class="text-xs text-red-400">{{ error }}</span>
      <button class="btn btn-sm ml-auto" type="button" :disabled="busy" @click="fetchAll">
        {{ t('studioKey.fetchAll') }}
      </button>
      <button
        v-if="status?.running && (status.queue.length ?? 0) > 0"
        class="btn btn-sm"
        type="button"
        :disabled="busy"
        @click="stopAll"
      >
        {{ t('studioKey.stop') }}
      </button>
      <button class="btn btn-sm" type="button" :disabled="busy || withKey === 0" @click="copyAll">
        {{ tf('studioKey.copyAll', { count: withKey }) }}
      </button>
      <button class="btn btn-sm btn-ghost" type="button" @click="expanded = !expanded">
        {{ expanded ? t('studioKey.collapse') : t('studioKey.expand') }}
      </button>
    </div>
    <div v-if="expanded" class="mt-3 space-y-2 border-t border-gray-800 pt-3">
      <p class="text-xs text-gray-500">
        {{ tf('studioKey.help', { delay: delaySeconds }) }}
      </p>
      <p v-if="records.length === 0" class="text-sm text-gray-500">{{ t('studioKey.empty') }}</p>
      <template v-else>
        <div class="flex flex-wrap items-center gap-2">
          <div class="seg">
            <button
              v-for="item in TABS"
              :key="item"
              class="seg-item"
              :class="{ active: tab === item }"
              type="button"
              :aria-pressed="tab === item"
              @click="selectTab(item)"
            >
              <span class="dot" :class="tabDotClass[item]"></span>
              {{ t(tabKeys[item]) }}
              <span class="seg-count">{{ grouped[item].length }}</span>
            </button>
          </div>
          <!-- 标签上的批量按钮靠右，窄卡片换行时仍靠右对齐 -->
          <div
            v-if="tab === 'ok' ? missingProject.length > 0 : tabRecords.length > 0"
            class="ml-auto flex flex-wrap items-center justify-end gap-2"
          >
            <template v-if="tab === 'failed'">
              <button class="btn btn-sm" type="button" :disabled="busy" @click="retryFailed">
                {{ tf('studioKey.retryFailed', { count: grouped.failed.length }) }}
              </button>
              <button class="btn btn-sm" type="button" @click="copyFailedEmails">
                {{ t('studioKey.copyFailedEmails') }}
              </button>
            </template>
            <button
              v-if="tab === 'ok'"
              class="btn btn-sm"
              type="button"
              :disabled="busy"
              @click="fillProjects"
            >
              {{ tf('studioKey.fillProjects', { count: missingProject.length }) }}
            </button>
            <button
              v-else
              class="btn btn-sm btn-danger"
              type="button"
              :disabled="busy"
              @click="clearTab"
            >
              {{ tf('studioKey.clearTab', { count: tabRecords.length }) }}
            </button>
          </div>
        </div>
        <p v-if="tabRecords.length === 0" class="text-sm text-gray-500">{{ t(emptyKeys[tab]) }}</p>
        <template v-else>
          <!-- 列的显隐与布局按卡片里可用的宽度（容器查询），不按窗口宽度：窗口到 md 时侧栏出现、卡片反而变窄。
               宽度不到 576px 时每条记录改为两三列的小网格；更宽时为固定列宽的表格（长文本不会撑宽），
               到 768、896、1024px 再依次加上时间、项目名、项目编号列，成功标签的账户列至少约 190px -->
          <div class="@container">
            <table class="w-full table-fixed text-left text-xs @max-xl:block">
              <thead class="text-gray-500 @max-xl:hidden">
                <tr>
                  <th class="py-1 pr-3" :class="tab === 'ok' ? '' : 'w-[30%]'">
                    {{ t('studioKey.account') }}
                  </th>
                  <template v-if="tab === 'ok'">
                    <th class="w-20 py-1 pr-3">{{ t('studioKey.key') }}</th>
                    <th class="hidden py-1 pr-3 @4xl:table-cell">
                      {{ t('studioKey.projectName') }}
                    </th>
                    <th class="hidden w-28 py-1 pr-3 @5xl:table-cell">
                      {{ t('studioKey.projectNumber') }}
                    </th>
                    <th class="w-40 py-1 pr-3">{{ t('studioKey.state') }}</th>
                    <th class="hidden w-52 py-1 pr-3 @3xl:table-cell">{{ t('studioKey.time') }}</th>
                    <th class="w-16 py-1"></th>
                  </template>
                  <template v-else>
                    <th v-if="tab === 'failed'" class="w-20 py-1 pr-3">
                      {{ t('studioKey.state') }}
                    </th>
                    <th class="py-1 pr-3">{{ t('studioKey.reason') }}</th>
                    <th class="w-40 py-1 pr-3">{{ t('studioKey.attemptAt') }}</th>
                    <th class="w-16 py-1"></th>
                  </template>
                </tr>
              </thead>
              <tbody class="@max-xl:block">
                <tr
                  v-for="record in pageRecords"
                  :key="record.account_id"
                  class="border-t border-gray-800 align-top text-gray-300 @max-xl:grid @max-xl:grid-cols-[minmax(0,1fr)_auto_auto] @max-xl:gap-x-2 @max-xl:gap-y-0.5 @max-xl:py-1.5"
                >
                  <td class="py-1 pr-3 @max-xl:p-0">
                    <div
                      class="cursor-pointer font-mono text-gray-200"
                      :class="clipClass(`label:${record.account_id}`)"
                      role="button"
                      tabindex="0"
                      :title="labelOf(record)"
                      :aria-expanded="isOpen(`label:${record.account_id}`)"
                      @click="toggleOpen(`label:${record.account_id}`)"
                      @keydown.enter.prevent="toggleOpen(`label:${record.account_id}`)"
                      @keydown.space.prevent="toggleOpen(`label:${record.account_id}`)"
                    >
                      {{ labelOf(record) }}
                    </div>
                  </td>
                  <template v-if="tab === 'ok'">
                    <td class="whitespace-nowrap py-1 pr-3 font-mono @max-xl:p-0">
                      …{{ record.last4 }}
                    </td>
                    <td class="hidden py-1 pr-3 @4xl:table-cell">
                      <div class="cell-truncate" :title="projectTitle(record)">
                        {{ projectText(record) }}
                        <span v-if="projectSecondary(record)" class="text-gray-500">{{
                          projectSecondary(record)
                        }}</span>
                        <span v-if="record.project_created" class="text-gray-500">{{
                          t('studioKey.projectCreated')
                        }}</span>
                      </div>
                    </td>
                    <td class="hidden py-1 pr-3 font-mono @5xl:table-cell">
                      <div class="cell-truncate" :title="record.project_number || ''">
                        {{ record.project_number || '-' }}
                      </div>
                    </td>
                    <td
                      class="py-1 pr-3 @max-xl:col-span-3 @max-xl:p-0"
                      :class="{ '@max-xl:hidden': record.status === 'ok' }"
                    >
                      <div class="cell-truncate">
                        <span :class="statusClass[record.status] ?? 'text-gray-400'">
                          {{ statusLabel(record.status) }}
                        </span>
                        <span v-if="record.created" class="ml-1 text-gray-500">{{
                          t('studioKey.created')
                        }}</span>
                      </div>
                      <template v-if="record.status !== 'ok'">
                        <div
                          class="cell-truncate text-yellow-400/80"
                          :title="t('studioKey.keptPrevious')"
                        >
                          {{ t('studioKey.keptPrevious') }}
                        </div>
                        <div
                          v-if="record.reason"
                          class="cursor-pointer text-gray-500"
                          :class="clipClass(`reason:${record.account_id}`)"
                          role="button"
                          tabindex="0"
                          :title="record.reason"
                          :aria-expanded="isOpen(`reason:${record.account_id}`)"
                          @click="toggleOpen(`reason:${record.account_id}`)"
                          @keydown.enter.prevent="toggleOpen(`reason:${record.account_id}`)"
                          @keydown.space.prevent="toggleOpen(`reason:${record.account_id}`)"
                        >
                          {{ record.reason }}
                        </div>
                        <button
                          class="btn btn-sm mt-1"
                          type="button"
                          :disabled="busy || processing(record)"
                          @click="clearFailure(record)"
                        >
                          {{ t('studioKey.clearFailure') }}
                        </button>
                      </template>
                    </td>
                    <td class="hidden py-1 pr-3 text-gray-500 @3xl:table-cell">
                      <div
                        v-for="line in timeLines(record)"
                        :key="line"
                        class="cell-truncate"
                        :title="line"
                      >
                        {{ line }}
                      </div>
                    </td>
                    <td class="py-1 @max-xl:col-start-3 @max-xl:row-start-1 @max-xl:p-0">
                      <button
                        class="btn btn-sm"
                        type="button"
                        :disabled="copying === record.account_id"
                        @click="copyKey(record)"
                      >
                        {{ t('studioKey.copy') }}
                      </button>
                    </td>
                  </template>
                  <template v-else>
                    <td
                      v-if="tab === 'failed'"
                      class="whitespace-nowrap py-1 pr-3 @max-xl:col-start-2 @max-xl:row-start-1 @max-xl:p-0"
                    >
                      <span :class="statusClass[record.status] ?? 'text-gray-400'">
                        {{ statusLabel(record.status) }}
                      </span>
                    </td>
                    <td class="py-1 pr-3 @max-xl:col-span-3 @max-xl:p-0">
                      <div
                        class="cursor-pointer text-gray-400"
                        :class="clipClass(`reason:${record.account_id}`)"
                        role="button"
                        tabindex="0"
                        :title="record.reason || ''"
                        :aria-expanded="isOpen(`reason:${record.account_id}`)"
                        @click="toggleOpen(`reason:${record.account_id}`)"
                        @keydown.enter.prevent="toggleOpen(`reason:${record.account_id}`)"
                        @keydown.space.prevent="toggleOpen(`reason:${record.account_id}`)"
                      >
                        {{ record.reason || '-' }}
                      </div>
                    </td>
                    <td
                      class="whitespace-nowrap py-1 pr-3 font-mono text-gray-500 @max-xl:p-0 @max-xl:text-[11px]"
                      :class="
                        tab === 'failed'
                          ? '@max-xl:col-span-3'
                          : '@max-xl:col-start-2 @max-xl:row-start-1'
                      "
                    >
                      <div class="cell-truncate" :title="formatTime(latestTime(record))">
                        {{ formatTime(latestTime(record)) }}
                      </div>
                    </td>
                    <td class="py-1 @max-xl:col-start-3 @max-xl:row-start-1 @max-xl:p-0">
                      <button
                        class="btn btn-sm btn-danger"
                        type="button"
                        :disabled="busy || processing(record)"
                        @click="deleteRecord(record)"
                      >
                        {{ t('studioKey.delete') }}
                      </button>
                    </td>
                  </template>
                </tr>
              </tbody>
            </table>
          </div>
          <UiPager
            v-model:page="page"
            :page-size="PAGE_SIZE"
            :total="tabRecords.length"
            fixed-size
          />
        </template>
      </template>
    </div>
  </section>
</template>
