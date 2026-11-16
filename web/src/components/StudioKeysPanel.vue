<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api } from '@/api'
import { copyText } from '@/clipboard'
import { confirmAction } from '@/confirm'
import { useI18n, type TranslationKey } from '@/i18n'
import { readStorage, writeStorage } from '@/storage'
import type { StudioKeyRecord, StudioKeyStatus } from '@/types'
import UiPager from './UiPager.vue'

// AI Studio API Key：显示获取队列与每个账户的记录（Key 只显示末四位），复制按钮按需读取完整 Key、不在页面里保存。
// 获取由账户行的“获取 Key”按钮或选中账户后的批量按钮触发（见 AccountsPanel），这里只轮询状态。
// 记录按成功、失败、跳过分标签列出，各自按最近一次尝试从新到旧排序、每页 10 条：账户多时失败原因不再整列铺开
const { t, tf, locale } = useI18n()
const emit = defineEmits<{ notice: [message: string, kind: 'success' | 'error'] }>()

const POLL_RUNNING = 3000
const POLL_IDLE = 20000
const PAGE_SIZE = 10
const TAB_STORAGE_KEY = 'aistudio2api_studio_keys_tab'

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
    current: value.current || '-',
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
          <template v-if="tab === 'failed' && grouped.failed.length > 0">
            <button class="btn btn-sm ml-auto" type="button" :disabled="busy" @click="retryFailed">
              {{ tf('studioKey.retryFailed', { count: grouped.failed.length }) }}
            </button>
            <button class="btn btn-sm" type="button" @click="copyFailedEmails">
              {{ t('studioKey.copyFailedEmails') }}
            </button>
          </template>
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
                    <th class="w-40 py-1">{{ t('studioKey.attemptAt') }}</th>
                  </template>
                </tr>
              </thead>
              <tbody class="@max-xl:block">
                <tr
                  v-for="record in pageRecords"
                  :key="record.account_id"
                  class="border-t border-gray-800 align-top text-gray-300 @max-xl:grid @max-xl:gap-x-2 @max-xl:gap-y-0.5 @max-xl:py-1.5"
                  :class="
                    tab === 'ok'
                      ? '@max-xl:grid-cols-[minmax(0,1fr)_auto_auto]'
                      : '@max-xl:grid-cols-[minmax(0,1fr)_auto]'
                  "
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
                      <div
                        class="cell-truncate"
                        :title="
                          record.project_display
                            ? `${record.project_name || '-'}（${record.project_display}）`
                            : record.project_name || '-'
                        "
                      >
                        {{ record.project_name || '-' }}
                        <span v-if="record.project_display" class="text-gray-500"
                          >（{{ record.project_display }}）</span
                        >
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
                    <td v-if="tab === 'failed'" class="whitespace-nowrap py-1 pr-3 @max-xl:p-0">
                      <span :class="statusClass[record.status] ?? 'text-gray-400'">
                        {{ statusLabel(record.status) }}
                      </span>
                    </td>
                    <td class="py-1 pr-3 @max-xl:col-span-2 @max-xl:p-0">
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
                      class="whitespace-nowrap py-1 font-mono text-gray-500 @max-xl:p-0 @max-xl:text-[11px]"
                      :class="
                        tab === 'failed'
                          ? '@max-xl:col-span-2'
                          : '@max-xl:col-start-2 @max-xl:row-start-1'
                      "
                    >
                      <div class="cell-truncate" :title="formatTime(latestTime(record))">
                        {{ formatTime(latestTime(record)) }}
                      </div>
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
