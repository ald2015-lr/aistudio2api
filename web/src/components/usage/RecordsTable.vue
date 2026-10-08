<script setup lang="ts">
import { computed, onUnmounted, ref, shallowRef, useId, watch } from 'vue'
import { api } from '@/api'
import { useI18n } from '@/i18n'
import type { UsageRecord } from '@/types'
import UiPopover from '../UiPopover.vue'
import UiSelect from '../UiSelect.vue'
import RecordDrawer from './RecordDrawer.vue'
import { useUsageFormat } from './format'
import { latencyTone, readPreference, stateTone, writePreference } from './model'

type Column =
  'model' | 'account' | 'channel' | 'protocol' | 'tokens' | 'latency' | 'queue' | 'attempts'

const props = defineProps<{
  scope: string
  baseQuery: () => URLSearchParams | null
  status: string
  search: string
  total: number
  statusOptions: string[]
  refreshKey: number
}>()
const emit = defineEmits<{ 'update:status': [value: string]; 'update:search': [value: string] }>()

const { t, tf, errorText } = useI18n()
const { compact, integer, duration, dateTime, dimensionLabel } = useUsageFormat()
const id = useId()
const pageSizes = [20, 50, 100]
const allColumns: Column[] = [
  'model',
  'account',
  'channel',
  'protocol',
  'tokens',
  'latency',
  'queue',
  'attempts',
]
const columnLabels: Record<Column, () => string> = {
  model: () => t('usage.dimension.model'),
  account: () => t('usage.dimension.account'),
  channel: () => t('usage.dimension.channel'),
  protocol: () => t('usage.dimension.protocol'),
  tokens: () => t('usage.tokens'),
  latency: () => t('usage.duration'),
  queue: () => t('usage.queue'),
  attempts: () => t('usage.attempts'),
}
const pageSize = ref(
  readPreference('aistudio2api_usage_page_size', 20, (value) =>
    pageSizes.includes(value as number),
  ),
)
const visible = ref<Column[]>(
  readPreference<Column[]>(
    'aistudio2api_usage_columns',
    ['model', 'account', 'channel', 'tokens', 'latency', 'attempts'],
    (value) => Array.isArray(value) && value.every((item) => allColumns.includes(item as Column)),
  ),
)
watch(pageSize, (value) => writePreference('aistudio2api_usage_page_size', value))
watch(visible, (value) => writePreference('aistudio2api_usage_columns', value), { deep: true })
const shownColumns = computed(() => allColumns.filter((item) => visible.value.includes(item)))

const items = shallowRef<UsageRecord[]>([])
const cursors = ref<(string | undefined)[]>([undefined])
const page = ref(0)
const nextCursor = ref<string | undefined>()
const state = ref<'loading' | 'ready' | 'error'>('loading')
const error = ref('')
const selected = ref<UsageRecord | null>(null)
const searchText = ref(props.search)
let controller: AbortController | undefined
let searchTimer: number | undefined

const filtered = computed(() => props.status !== '' || props.search !== '')

// parameters 在调用时解析时间范围并组合筛选、状态码、关键字与分页参数，范围无效时返回 null
function parameters(cursor?: string): URLSearchParams | null {
  const query = props.baseQuery()
  if (query === null) return null
  if (props.status !== '') query.set('status', props.status)
  if (props.search !== '') query.set('q', props.search)
  if (cursor !== undefined) query.set('cursor', cursor)
  return query
}

// load 读取当前页，较早发出的请求被取消
async function load(): Promise<void> {
  controller?.abort()
  const current = new AbortController()
  controller = current
  state.value = items.value.length === 0 ? 'loading' : state.value
  error.value = ''
  const query = parameters(cursors.value[page.value])
  if (query === null) {
    // 自定义范围无效（如本机保存的范围被改坏）：显示原因，不停在加载中
    error.value = t('usage.rangeInvalid')
    state.value = 'error'
    return
  }
  query.set('limit', String(pageSize.value))
  try {
    const result = await api.usageRecords(query, current.signal)
    items.value = result.items
    nextCursor.value = result.next_cursor
    state.value = 'ready'
  } catch (reason) {
    if (current.signal.aborted) return
    error.value = errorText(reason)
    state.value = 'error'
  }
}

// restart 回到第一页重新读取
function restart(): void {
  cursors.value = [undefined]
  page.value = 0
  void load()
}

// move 前后翻页，向后翻页时记录下一页的位置
function move(step: 1 | -1): void {
  if (step === 1) {
    if (nextCursor.value === undefined) return
    cursors.value = [...cursors.value.slice(0, page.value + 1), nextCursor.value]
  }
  page.value = Math.max(0, page.value + step)
  void load()
}

watch(() => [props.scope, props.status, props.search, pageSize.value], restart, { immediate: true })
watch(
  () => props.refreshKey,
  () => void load(),
)
watch(
  () => props.search,
  (value) => {
    searchText.value = value
  },
)
watch(searchText, (value) => {
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => emit('update:search', value.trim()), 300)
})
onUnmounted(() => {
  controller?.abort()
  window.clearTimeout(searchTimer)
})

// searchReply 关闭详情并列出同一回复指纹的全部请求
function searchReply(hash: string): void {
  selected.value = null
  searchText.value = hash
  emit('update:search', hash)
}

// download 按当前范围与筛选下载 CSV
function download(): void {
  const query = parameters()
  if (query === null) return
  const link = document.createElement('a')
  link.href = api.usageExportURL(query)
  link.download = ''
  link.click()
}

// toggleColumn 切换一列的显示
function toggleColumn(column: Column, checked: boolean): void {
  visible.value = checked
    ? allColumns.filter((item) => item === column || visible.value.includes(item))
    : visible.value.filter((item) => item !== column)
}

// latencyWidth 把耗时映射到 0 到 100 的条形宽度，3 分钟为满格
function latencyWidth(milliseconds: number): number {
  return Math.min(100, Math.max(4, (milliseconds / 180_000) * 100))
}
</script>

<template>
  <section class="panel min-w-0">
    <header class="flex flex-wrap items-center gap-2 border-b border-line px-4 py-3">
      <div class="mr-auto min-w-0">
        <h3 :id="`${id}-title`" class="text-sm font-medium text-gray-200">
          {{ t('usage.records') }}
        </h3>
        <p v-if="!filtered" class="text-xs text-gray-500">
          {{ tf('usage.recordsCount', { count: integer(total) }) }}
        </p>
      </div>
      <input
        v-model="searchText"
        type="search"
        :placeholder="t('usage.recordsSearch')"
        :aria-label="t('usage.recordsSearch')"
        class="input h-8 w-60 text-xs"
      />
      <UiSelect
        :model-value="status"
        :aria-label="t('usage.dimension.status')"
        class="input h-8 w-36 text-xs"
        @update:model-value="emit('update:status', String($event))"
      >
        <option value="">{{ t('usage.recordsAllStatus') }}</option>
        <option v-for="code in statusOptions" :key="code" :value="code">{{ code }}</option>
      </UiSelect>
      <UiPopover align="end" content-class="w-48 p-2" trigger-class="btn btn-sm h-8">
        <template #trigger>{{ t('usage.columns') }}</template>
        <fieldset>
          <legend class="sr-only">{{ t('usage.columns') }}</legend>
          <label
            v-for="column in allColumns"
            :key="column"
            :for="`${id}-column-${column}`"
            class="flex cursor-pointer items-center gap-2 rounded px-2 py-1.5 text-xs text-gray-300 hover:bg-raised"
          >
            <input
              :id="`${id}-column-${column}`"
              type="checkbox"
              :checked="visible.includes(column)"
              @change="toggleColumn(column, ($event.target as HTMLInputElement).checked)"
            />
            {{ columnLabels[column]() }}
          </label>
        </fieldset>
      </UiPopover>
      <button type="button" class="btn btn-sm h-8" @click="download">
        {{ t('usage.export') }}
      </button>
    </header>

    <div
      v-if="state === 'error'"
      class="flex items-center gap-3 px-4 py-6 text-sm text-red-300"
      role="alert"
    >
      <span class="flex-1">{{ error }}</span>
      <button type="button" class="btn btn-sm btn-danger" @click="load">
        {{ t('usage.retry') }}
      </button>
    </div>
    <div v-else-if="state === 'loading'" class="space-y-2 p-4" aria-hidden="true">
      <div v-for="index in 6" :key="index" class="h-8 animate-pulse rounded bg-raised/60"></div>
    </div>
    <div v-else-if="items.length === 0" class="px-4 py-12 text-center text-sm text-gray-500">
      {{ t('usage.recordsEmpty') }}
    </div>
    <template v-else>
      <div class="hidden overflow-x-auto md:block">
        <table class="w-full text-left text-xs" :aria-labelledby="`${id}-title`">
          <thead class="text-gray-500">
            <tr>
              <th scope="col" class="px-4 py-2 font-normal">{{ t('usage.time') }}</th>
              <th scope="col" class="px-4 py-2 font-normal">{{ t('usage.result') }}</th>
              <th
                v-for="column in shownColumns"
                :key="column"
                scope="col"
                class="px-4 py-2 font-normal"
                :class="['tokens', 'queue', 'attempts'].includes(column) ? 'text-right' : ''"
              >
                {{ columnLabels[column]() }}
              </th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="record in items"
              :key="record.id"
              class="cursor-pointer border-t border-line/60 text-gray-300 hover:bg-raised/50"
              :class="record.state === 'failed' ? 'bg-red-500/[0.06]' : ''"
              @click="selected = record"
            >
              <td class="px-4 py-2 whitespace-nowrap">
                <button
                  type="button"
                  class="text-left text-gray-200 tabular-nums hover:text-blue-300"
                  :aria-label="`${t('usage.openDetail')} ${record.id}`"
                  @click.stop="selected = record"
                >
                  {{ dateTime(record.time, true) }}
                </button>
              </td>
              <td class="px-4 py-2 whitespace-nowrap">
                <span class="rounded px-1.5 py-0.5" :class="stateTone(record.state)">{{
                  dimensionLabel('state', record.state)
                }}</span>
                <span class="ml-1.5 text-gray-500 tabular-nums">{{ record.status }}</span>
                <span
                  v-if="record.pool === 'ultra'"
                  v-tooltip="t('pool.ultraHelp')"
                  class="tag tag-ultra ml-1.5"
                  >{{ t('pool.ultra') }}</span
                >
                <span
                  v-if="record.downgrade === 'rejected'"
                  v-tooltip="t('usage.downgrade.rejected')"
                  class="tag ml-1.5 text-red-300"
                  >{{ t('usage.downgradeShort') }}</span
                >
                <span
                  v-if="record.duplicate"
                  v-tooltip="t('usage.duplicate')"
                  class="tag ml-1.5 text-yellow-300"
                  >{{ t('usage.duplicateShort') }}</span
                >
              </td>
              <template v-for="column in shownColumns" :key="column">
                <td v-if="column === 'model'" class="max-w-48 px-4 py-2">
                  <span v-tooltip="record.model" class="block truncate">{{
                    record.model || '—'
                  }}</span>
                </td>
                <td v-else-if="column === 'account'" class="max-w-56 px-4 py-2">
                  <span v-tooltip="record.account" class="block truncate">{{
                    record.account || '—'
                  }}</span>
                </td>
                <td v-else-if="column === 'channel'" class="px-4 py-2 whitespace-nowrap">
                  {{ record.channel ? dimensionLabel('channel', record.channel) : '—' }}
                </td>
                <td v-else-if="column === 'protocol'" class="px-4 py-2 whitespace-nowrap">
                  {{ dimensionLabel('protocol', record.protocol) }}
                </td>
                <td
                  v-else-if="column === 'tokens'"
                  class="px-4 py-2 text-right whitespace-nowrap tabular-nums"
                >
                  <div class="text-gray-200">{{ compact(record.total_tokens) }}</div>
                  <div class="text-[11px] text-gray-500">
                    {{ compact(record.input_tokens) }} / {{ compact(record.reasoning_tokens) }} /
                    {{ compact(record.reply_tokens) }}
                  </div>
                </td>
                <td v-else-if="column === 'latency'" class="w-44 px-4 py-2">
                  <div class="flex items-center justify-between gap-2 tabular-nums">
                    <span class="text-gray-200">{{ duration(record.duration_ms) }}</span>
                    <span class="text-[11px] text-gray-500">{{
                      duration(record.first_event_ms)
                    }}</span>
                  </div>
                  <div class="mt-1 h-1 rounded-full bg-raised">
                    <div
                      class="h-1 rounded-full"
                      :class="latencyTone(record.duration_ms, 'duration')"
                      :style="{ width: `${latencyWidth(record.duration_ms)}%` }"
                    ></div>
                  </div>
                </td>
                <td v-else-if="column === 'queue'" class="px-4 py-2 text-right tabular-nums">
                  {{ duration(record.queue_ms) }}
                </td>
                <td v-else-if="column === 'attempts'" class="px-4 py-2 text-right tabular-nums">
                  <span :class="record.attempts.length > 0 ? 'text-yellow-400' : 'text-gray-600'">{{
                    record.attempts.length
                  }}</span>
                </td>
              </template>
            </tr>
          </tbody>
        </table>
      </div>
      <ul class="divide-y divide-line/60 md:hidden">
        <li v-for="record in items" :key="record.id">
          <button
            type="button"
            class="w-full px-4 py-3 text-left text-xs"
            :class="record.state === 'failed' ? 'bg-red-500/[0.06]' : ''"
            :aria-label="`${t('usage.openDetail')} ${record.id}`"
            @click="selected = record"
          >
            <div class="flex items-center gap-2">
              <span class="rounded px-1.5 py-0.5" :class="stateTone(record.state)">{{
                dimensionLabel('state', record.state)
              }}</span>
              <span class="text-gray-500 tabular-nums">{{ record.status }}</span>
              <span v-if="record.pool === 'ultra'" class="tag tag-ultra">{{
                t('pool.ultra')
              }}</span>
              <span class="ml-auto text-gray-400 tabular-nums">{{
                dateTime(record.time, true)
              }}</span>
            </div>
            <div class="mt-1.5 truncate text-gray-200">{{ record.model || '—' }}</div>
            <div class="mt-0.5 flex gap-3 text-gray-500 tabular-nums">
              <span class="truncate">{{ record.account || '—' }}</span>
              <span class="ml-auto shrink-0">{{ duration(record.duration_ms) }}</span>
              <span class="shrink-0">{{ compact(record.total_tokens) }}</span>
            </div>
          </button>
        </li>
      </ul>
    </template>

    <footer
      class="flex flex-wrap items-center gap-2 border-t border-line px-4 py-2 text-xs text-gray-400"
    >
      <UiSelect
        :model-value="pageSize"
        :aria-label="tf('usage.pageSize', { count: pageSize })"
        class="input h-7 w-32 text-xs"
        @update:model-value="pageSize = Number($event)"
      >
        <option v-for="size in pageSizes" :key="size" :value="size">
          {{ tf('usage.pageSize', { count: size }) }}
        </option>
      </UiSelect>
      <span class="ml-auto tabular-nums">{{ tf('usage.page', { page: page + 1 }) }}</span>
      <button type="button" class="btn btn-sm" :disabled="page === 0" @click="move(-1)">
        ‹ {{ t('usage.previousPage') }}
      </button>
      <button
        type="button"
        class="btn btn-sm"
        :disabled="nextCursor === undefined"
        @click="move(1)"
      >
        {{ t('usage.nextPage') }} ›
      </button>
    </footer>
    <RecordDrawer :record="selected" @close="selected = null" @search="searchReply" />
  </section>
</template>
