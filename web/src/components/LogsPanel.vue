<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { channelLabelKey, useI18n, type TranslationKey } from '@/i18n'
import {
  aggregateRows,
  groupLogs,
  isNoise,
  parseMessage,
  rowCategory,
  rowSummary,
  singleItems,
  type LogCategory,
  type LogItem,
  type LogRow,
} from '@/logs'
import type { AdminLog, RequestLog } from '@/types'
import RequestLogCard from './RequestLogCard.vue'
import UiIcon from './UiIcon.vue'
import { readStorage, writeStorage } from '@/storage'

const props = defineProps<{
  logs: AdminLog[]
}>()

defineEmits<{
  clear: []
}>()

type Level = 'ALL' | 'INFO' | 'WARN' | 'ERROR'
type Category = 'all' | LogCategory

// 固定行高 + 只渲染可视区域：几千条日志也只保留几十个 DOM 节点
const ROW_HEIGHT = 30
const OVERSCAN = 10
const COMPACT_KEY = 'aistudio2api_logs_compact'

const { t, tf, locale } = useI18n()
const levels: readonly Level[] = ['ALL', 'INFO', 'WARN', 'ERROR']
const categories: readonly Category[] = ['all', 'request', 'account', 'service']
const levelKeys: Record<Level, TranslationKey> = {
  ALL: 'logs.all',
  INFO: 'logs.info',
  WARN: 'logs.warn',
  ERROR: 'logs.error',
}
const categoryKeys: Record<Category, TranslationKey> = {
  all: 'logs.categoryAll',
  request: 'logs.categoryRequest',
  account: 'logs.categoryAccount',
  service: 'logs.categoryService',
}
const requestStateClass: Record<RequestLog['state'], string> = {
  running: 'badge-blue',
  completed: 'badge-green',
  tool_calls: 'badge-green',
  limited: 'badge-amber',
  blocked: 'badge-amber',
  cancelled: 'badge-amber',
  failed: 'badge-red',
}

const level = ref<Level>('ALL')
const category = ref<Category>('all')
const source = ref('ALL')
const search = ref('')
const autoScroll = ref(true)
const compact = ref(readStorage(COMPACT_KEY) !== 'false')
const selectedKey = ref('')
const viewport = ref<HTMLElement>()
const scrollTop = ref(0)
const viewportHeight = ref(600)
let resizeObserver: ResizeObserver | undefined
let scrollFrame = 0

watch(compact, (value) => {
  writeStorage(COMPACT_KEY, String(value))
})

const rows = computed(() => groupLogs(props.logs))
const sources = computed(() =>
  Array.from(new Set(props.logs.map((entry) => entry.source).filter(Boolean))).sort(),
)

function normalizedLevel(row: LogRow): Level {
  const value = row.entry.level.toUpperCase()
  return value === 'WARN' || value === 'ERROR' ? value : 'INFO'
}

function matches(row: LogRow, query: string): boolean {
  const entry = row.entry
  if (rowSummary(row).toLowerCase().includes(query)) return true
  if (entry.source.toLowerCase().includes(query)) return true
  if (entry.event.toLowerCase().includes(query)) return true
  const request = entry.request
  if (request === undefined) return false
  return [request.id, request.path ?? '', request.finish_reason ?? ''].some((value) =>
    value.toLowerCase().includes(query),
  )
}

const categoryCounts = computed(() => {
  const counts: Record<Category, number> = { all: 0, request: 0, account: 0, service: 0 }
  for (const row of rows.value) {
    counts.all += 1
    counts[rowCategory(row)] += 1
  }
  return counts
})

// base 为除等级以外的全部筛选结果，等级按钮上的数量基于它计算
const base = computed(() => {
  const query = search.value.trim().toLowerCase()
  return rows.value.filter((row) => {
    if (category.value !== 'all' && rowCategory(row) !== category.value) return false
    if (source.value !== 'ALL' && !row.events.some((event) => event.source === source.value))
      return false
    if (compact.value && isNoise(row)) return false
    return query === '' || matches(row, query)
  })
})

const levelCounts = computed(() => {
  const counts: Record<Level, number> = { ALL: 0, INFO: 0, WARN: 0, ERROR: 0 }
  for (const row of base.value) {
    counts.ALL += 1
    counts[normalizedLevel(row)] += 1
  }
  return counts
})

const filtered = computed(() =>
  level.value === 'ALL'
    ? base.value
    : base.value.filter((row) => normalizedLevel(row) === level.value),
)
const items = computed(() =>
  compact.value ? aggregateRows(filtered.value) : singleItems(filtered.value),
)

const totalHeight = computed(() => items.value.length * ROW_HEIGHT)
const range = computed(() => {
  const start = Math.max(0, Math.floor(scrollTop.value / ROW_HEIGHT) - OVERSCAN)
  const end = Math.min(
    items.value.length,
    Math.ceil((scrollTop.value + viewportHeight.value) / ROW_HEIGHT) + OVERSCAN,
  )
  return { start, end }
})
const visibleItems = computed(() => items.value.slice(range.value.start, range.value.end))
const offsetY = computed(() => range.value.start * ROW_HEIGHT)

const selectedItem = computed(() =>
  selectedKey.value === '' ? undefined : items.value.find((item) => item.key === selectedKey.value),
)
const selectedParsed = computed(() =>
  selectedItem.value === undefined ? undefined : parseMessage(selectedItem.value.row.entry.message),
)

function levelLabel(value: Level): string {
  return t(levelKeys[value])
}

function levelBadge(row: LogRow): string {
  const value = normalizedLevel(row)
  if (value === 'ERROR') return 'badge-red'
  if (value === 'WARN') return 'badge-amber'
  return 'badge-muted'
}

function rowTone(row: LogRow): string {
  const value = normalizedLevel(row)
  if (value === 'ERROR') return 'tone-error'
  if (value === 'WARN') return 'tone-warn'
  return ''
}

function sourceClass(row: LogRow): string {
  if (row.entry.source === 'service') return 'source-service'
  if (row.entry.source === 'auth') return 'source-auth'
  if (row.entry.source === 'request') return 'source-request'
  return 'source-account'
}

function requestBadge(row: LogRow): string {
  const request = row.entry.request
  return request === undefined ? 'badge-muted' : requestStateClass[request.state]
}

function requestStatus(row: LogRow): string {
  const request = row.entry.request
  if (request === undefined) return ''
  return request.status === undefined ? t('logs.running') : String(request.status)
}

function requestTitle(row: LogRow): string {
  const request = row.entry.request
  if (request === undefined) return ''
  return request.model || `${request.method ?? ''} ${request.path ?? ''}`.trim()
}

// requestChips 请求行右侧的耗时、用量、通道
function requestChips(row: LogRow): string[] {
  const request = row.entry.request
  if (request === undefined) return []
  const chips: string[] = []
  if (request.duration_ms !== undefined) chips.push(`${(request.duration_ms / 1000).toFixed(2)}s`)
  if (request.usage !== undefined)
    chips.push(`${request.usage.total_tokens.toLocaleString(locale.value)} tok`)
  if (request.usage !== undefined && request.usage.average_tokens_per_second > 0) {
    chips.push(`${request.usage.average_tokens_per_second.toFixed(1)} tok/s`)
  }
  if (request.channel !== undefined) chips.push(t(channelLabelKey(request.channel)))
  if (request.tool_calls) chips.push(`${t('logs.tools')} ${request.tool_calls}`)
  return chips
}

// itemSource 合并组显示来源数量，单条显示来源本身
function itemSource(item: LogItem): string {
  if (item.members.length === 1) return item.row.entry.source
  const distinct = new Set(item.members.map((member) => member.entry.source))
  return distinct.size === 1
    ? item.row.entry.source
    : tf('logs.groupSources', { count: distinct.size })
}

function displayTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleTimeString('en-GB', { hour12: false })
}

function select(item: LogItem): void {
  selectedKey.value = selectedKey.value === item.key ? '' : item.key
}

function closeDetail(): void {
  selectedKey.value = ''
}

// syncScroll 读取滚动位置；离开底部时暂停自动滚动，回到底部时恢复
function syncScroll(): void {
  scrollFrame = 0
  const element = viewport.value
  if (element === undefined) return
  scrollTop.value = element.scrollTop
  const atBottom = element.scrollTop + element.clientHeight >= element.scrollHeight - ROW_HEIGHT * 2
  if (autoScroll.value !== atBottom) autoScroll.value = atBottom
}

function onScroll(): void {
  if (scrollFrame !== 0) return
  scrollFrame = window.requestAnimationFrame(syncScroll)
}

function scrollToBottom(): void {
  void nextTick(() => {
    const element = viewport.value
    if (element === undefined) return
    element.scrollTop = element.scrollHeight
    scrollTop.value = element.scrollTop
  })
}

function jumpLatest(): void {
  autoScroll.value = true
  scrollToBottom()
}

function toggleAutoScroll(): void {
  if (autoScroll.value) autoScroll.value = false
  else jumpLatest()
}

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape' && selectedKey.value !== '') closeDetail()
}

watch(items, () => {
  if (autoScroll.value) scrollToBottom()
})

onMounted(() => {
  const element = viewport.value
  if (element !== undefined) {
    viewportHeight.value = element.clientHeight
    resizeObserver = new ResizeObserver(() => {
      if (viewport.value !== undefined) viewportHeight.value = viewport.value.clientHeight
    })
    resizeObserver.observe(element)
  }
  window.addEventListener('keydown', onKeydown)
  scrollToBottom()
})

onBeforeUnmount(() => {
  resizeObserver?.disconnect()
  if (scrollFrame !== 0) window.cancelAnimationFrame(scrollFrame)
  window.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <section class="flex min-h-0 flex-1 flex-col bg-[#0d1117]">
    <!-- 工具栏 -->
    <div class="space-y-2 border-b border-[#30363d] bg-[#161b22] px-4 py-2.5">
      <div class="flex flex-wrap items-center gap-2">
        <div class="seg">
          <button
            v-for="item in categories"
            :key="item"
            class="seg-item"
            :class="{ active: category === item }"
            type="button"
            @click="category = item"
          >
            {{ t(categoryKeys[item]) }}
            <span class="seg-count">{{ categoryCounts[item] }}</span>
          </button>
        </div>
        <div class="seg">
          <button
            v-for="item in levels"
            :key="item"
            class="seg-item"
            :class="{
              active: level === item,
              'text-amber-300': item === 'WARN' && level !== item && levelCounts.WARN > 0,
              'text-red-300': item === 'ERROR' && level !== item && levelCounts.ERROR > 0,
            }"
            type="button"
            @click="level = item"
          >
            {{ levelLabel(item) }}
            <span class="seg-count">{{ levelCounts[item] }}</span>
          </button>
        </div>
        <span class="flex-1"></span>
        <label
          v-tooltip="t('logs.compactHint')"
          class="flex cursor-pointer items-center gap-1.5 text-xs text-gray-400"
        >
          <input v-model="compact" type="checkbox" />
          {{ t('logs.compact') }}
        </label>
        <button
          class="btn btn-sm"
          :class="autoScroll ? 'border-blue-500/40 text-blue-300' : ''"
          type="button"
          @click="toggleAutoScroll"
        >
          <span class="dot" :class="autoScroll ? 'bg-green-500' : 'bg-gray-600'"></span>
          {{ t('logs.autoScroll') }}
        </button>
        <button
          v-tooltip="t('logs.clear')"
          class="btn btn-sm btn-ghost"
          type="button"
          :aria-label="t('logs.clear')"
          @click="$emit('clear')"
        >
          <UiIcon name="trash" :size="14" />
        </button>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <input
          v-model="search"
          type="search"
          :placeholder="t('logs.search')"
          :aria-label="t('logs.search')"
          class="input h-[1.75rem] min-w-0 flex-1 text-xs sm:max-w-80"
        />
        <select
          v-model="source"
          class="input h-[1.75rem] max-w-64 text-xs"
          :aria-label="t('logs.source')"
        >
          <option value="ALL">{{ t('logs.source') }}: {{ t('logs.allSources') }}</option>
          <option v-for="item in sources" :key="item" :value="item">{{ item }}</option>
        </select>
        <span class="flex-1"></span>
        <span class="font-mono text-[11px] text-gray-500">
          {{ tf('logs.shown', { shown: filtered.length, total: rows.length }) }}
        </span>
      </div>
    </div>

    <div class="flex min-h-0 flex-1 flex-col xl:flex-row">
      <!-- 列表 -->
      <div class="relative flex min-h-0 min-w-0 flex-1 flex-col">
        <div ref="viewport" class="min-h-0 flex-1 overflow-auto" @scroll="onScroll">
          <div
            v-if="items.length === 0"
            class="mt-16 flex flex-col items-center gap-2 text-sm text-gray-600"
          >
            <UiIcon name="chatBubble" :size="40" />
            {{ t('logs.waiting') }}
          </div>
          <div v-else class="relative" :style="{ height: `${totalHeight}px` }">
            <div
              class="absolute inset-x-0 top-0"
              :style="{ transform: `translateY(${offsetY}px)` }"
            >
              <div
                v-for="item in visibleItems"
                :key="item.key"
                class="log-row"
                :class="[rowTone(item.row), { 'is-selected': item.key === selectedKey }]"
                @click="select(item)"
              >
                <span class="log-time">{{ displayTime(item.row.entry.time) }}</span>
                <span class="badge" :class="levelBadge(item.row)">
                  {{ levelLabel(normalizedLevel(item.row)) }}
                </span>
                <span class="log-source" :class="sourceClass(item.row)" :title="itemSource(item)">
                  {{ itemSource(item) }}
                </span>
                <span class="log-body">
                  <template v-if="item.row.entry.request">
                    <span class="badge" :class="requestBadge(item.row)">{{
                      requestStatus(item.row)
                    }}</span>
                    <span
                      v-if="item.row.entry.request.pool === 'ultra'"
                      v-tooltip="t('pool.ultraHelp')"
                      class="tag tag-ultra shrink-0"
                      >{{ t('pool.ultra') }}</span
                    >
                    <strong class="log-title">{{ requestTitle(item.row) }}</strong>
                    <span
                      v-for="(chip, index) in requestChips(item.row)"
                      :key="index"
                      class="log-chip"
                    >
                      {{ chip }}
                    </span>
                    <span v-if="item.row.entry.request.error" class="log-error">
                      {{ item.row.entry.request.error }}
                    </span>
                  </template>
                  <template v-else>
                    <strong class="log-title">{{
                      parseMessage(item.row.entry.message).title
                    }}</strong>
                    <span v-if="item.members.length > 1" class="badge badge-blue">
                      ×{{ item.members.length }}
                    </span>
                    <span
                      v-for="(note, index) in parseMessage(item.row.entry.message).notes"
                      :key="`n:${index}`"
                      class="log-note"
                    >
                      {{ note }}
                    </span>
                    <span
                      v-for="(field, index) in parseMessage(item.row.entry.message).fields"
                      :key="`f:${index}`"
                      class="log-chip"
                    >
                      <span class="log-chip-key">{{ field.key }}</span
                      >{{ field.value }}
                    </span>
                  </template>
                </span>
              </div>
            </div>
          </div>
        </div>
        <button
          v-if="!autoScroll && items.length > 0"
          class="btn btn-sm btn-primary absolute right-5 bottom-4 shadow-lg"
          type="button"
          @click="jumpLatest"
        >
          ↓ {{ t('logs.jumpLatest') }}
        </button>
      </div>

      <!-- 详情 -->
      <aside
        v-if="selectedItem && selectedParsed"
        class="flex max-h-[45%] min-h-48 flex-col border-t border-[#30363d] bg-[#161b22] xl:max-h-none xl:w-[460px] xl:border-t-0 xl:border-l"
      >
        <div class="flex items-center gap-2 border-b border-[#30363d] px-4 py-2">
          <span class="badge" :class="levelBadge(selectedItem.row)">
            {{ levelLabel(normalizedLevel(selectedItem.row)) }}
          </span>
          <strong class="cell-truncate text-sm text-gray-100">
            {{
              selectedItem.row.entry.request
                ? requestTitle(selectedItem.row)
                : selectedParsed.title || t('logs.detailTitle')
            }}
          </strong>
          <span v-if="selectedItem.members.length > 1" class="badge badge-blue">
            ×{{ selectedItem.members.length }}
          </span>
          <span class="flex-1"></span>
          <button
            class="btn btn-sm btn-ghost"
            type="button"
            :aria-label="t('common.close')"
            @click="closeDetail"
          >
            <UiIcon name="close" :size="14" />
          </button>
        </div>

        <div class="min-h-0 flex-1 space-y-4 overflow-auto px-4 py-3 text-xs">
          <dl class="detail-grid">
            <dt>{{ t('logs.time') }}</dt>
            <dd class="font-mono">{{ displayTime(selectedItem.row.entry.time) }}</dd>
            <dt>{{ t('logs.source') }}</dt>
            <dd class="font-mono break-all">{{ itemSource(selectedItem) }}</dd>
            <template v-if="selectedItem.row.entry.event">
              <dt>{{ t('logs.event') }}</dt>
              <dd class="font-mono">{{ selectedItem.row.entry.event }}</dd>
            </template>
          </dl>

          <RequestLogCard v-if="selectedItem.row.entry.request" :row="selectedItem.row" />

          <template v-else-if="selectedItem.members.length > 1">
            <p class="text-gray-500">
              {{ tf('logs.groupMembers', { count: selectedItem.members.length }) }}
            </p>
            <ul class="space-y-1">
              <li
                v-for="member in selectedItem.members"
                :key="member.key"
                class="rounded border border-[#21262d] bg-[#0d1117] px-2 py-1.5"
              >
                <div class="flex items-center gap-2">
                  <span class="font-mono text-gray-500">{{ displayTime(member.entry.time) }}</span>
                  <span class="cell-truncate font-mono text-gray-300">{{
                    member.entry.source
                  }}</span>
                </div>
                <div class="mt-1 flex flex-wrap gap-1">
                  <span
                    v-for="(field, index) in parseMessage(member.entry.message).fields"
                    :key="`f:${index}`"
                    class="log-chip"
                  >
                    <span class="log-chip-key">{{ field.key }}</span
                    >{{ field.value }}
                  </span>
                  <span
                    v-for="(note, index) in parseMessage(member.entry.message).notes"
                    :key="`n:${index}`"
                    class="log-note"
                  >
                    {{ note }}
                  </span>
                </div>
              </li>
            </ul>
          </template>

          <template v-else>
            <dl v-if="selectedParsed.fields.length > 0" class="detail-grid">
              <template v-for="(field, index) in selectedParsed.fields" :key="index">
                <dt>{{ field.key }}</dt>
                <dd class="font-mono break-all">{{ field.value }}</dd>
              </template>
            </dl>
            <div v-if="selectedParsed.notes.length > 0" class="flex flex-wrap gap-1">
              <span v-for="(note, index) in selectedParsed.notes" :key="index" class="log-note">{{
                note
              }}</span>
            </div>
            <div>
              <div class="mb-1 text-gray-500">{{ t('logs.raw') }}</div>
              <pre
                class="rounded border border-[#21262d] bg-[#0d1117] p-2 font-mono break-words whitespace-pre-wrap text-gray-300"
                >{{ selectedItem.row.entry.message }}</pre>
            </div>
          </template>
        </div>
      </aside>
    </div>
  </section>
</template>

<style scoped>
.log-row {
  display: grid;
  height: 30px;
  grid-template-columns: 4.5rem 3.25rem 13.5rem minmax(0, 1fr);
  align-items: center;
  column-gap: 0.75rem;
  padding: 0 1rem;
  border-bottom: 1px solid #161b22;
  border-left: 2px solid transparent;
  cursor: pointer;
  font-size: 12.5px;
  white-space: nowrap;
}

.log-row:hover {
  background-color: #161b22;
}

.log-row.is-selected {
  border-left-color: #3b82f6;
  background-color: rgb(37 99 235 / 14%);
}

.log-row.tone-warn {
  border-left-color: #d29922;
  background-color: rgb(210 153 34 / 6%);
}

.log-row.tone-error {
  border-left-color: #f85149;
  background-color: rgb(248 81 73 / 8%);
}

.log-time {
  color: #6e7681;
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 11.5px;
  font-variant-numeric: tabular-nums;
}

.log-source {
  overflow: hidden;
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 11.5px;
  text-overflow: ellipsis;
}

.source-account {
  color: #8b949e;
}

.source-service {
  color: #bc8cff;
}

.source-auth {
  color: #56d4a0;
}

.source-request {
  color: #79c0ff;
}

.log-body {
  display: flex;
  overflow: hidden;
  min-width: 0;
  align-items: center;
  gap: 0.375rem;
}

.log-title {
  flex-shrink: 0;
  color: #e6edf3;
  font-weight: 500;
}

.log-note {
  flex-shrink: 0;
  color: #8b949e;
}

.log-chip {
  display: inline-flex;
  height: 18px;
  flex-shrink: 0;
  align-items: center;
  gap: 0.3rem;
  padding: 0 0.4rem;
  border: 1px solid #21262d;
  border-radius: 4px;
  background: #161b22;
  color: #c9d1d9;
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 11px;
}

.log-chip-key {
  color: #6e7681;
}

.log-error {
  overflow: hidden;
  color: #ff7b72;
  text-overflow: ellipsis;
}

.badge {
  display: inline-flex;
  height: 18px;
  flex-shrink: 0;
  align-items: center;
  justify-content: center;
  padding: 0 0.4rem;
  border-radius: 4px;
  font-size: 11px;
  font-weight: 500;
}

.badge-muted {
  background: rgb(110 118 129 / 15%);
  color: #8b949e;
}

.badge-blue {
  background: rgb(56 139 253 / 15%);
  color: #79c0ff;
}

.badge-green {
  background: rgb(46 160 67 / 15%);
  color: #56d4a0;
}

.badge-amber {
  background: rgb(210 153 34 / 18%);
  color: #e3b341;
}

.badge-red {
  background: rgb(248 81 73 / 15%);
  color: #ff7b72;
}

.detail-grid {
  display: grid;
  grid-template-columns: max-content minmax(0, 1fr);
  gap: 0.35rem 1rem;
}

.detail-grid dt {
  color: #6e7681;
}

.detail-grid dd {
  margin: 0;
  color: #c9d1d9;
}

@media (max-width: 767px) {
  .log-row {
    grid-template-columns: 4.25rem 2.75rem minmax(0, 1fr);
    padding: 0 0.5rem;
  }

  .log-source {
    display: none;
  }
}
</style>
