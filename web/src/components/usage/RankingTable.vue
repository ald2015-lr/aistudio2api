<script setup lang="ts">
import { computed, onBeforeUnmount, ref, shallowRef, useId, watch } from 'vue'
import { useI18n } from '@/i18n'
import { usePaging } from '@/paging'
import type { UsageDimension, UsagePair, UsageReport, UsageStats } from '@/types'
import UiIcon from '../UiIcon.vue'
import UiPager from '../UiPager.vue'
import { useUsageFormat } from './format'
import { rateTone, readPreference, successRate, writePreference } from './model'

type Tab = UsageDimension | 'status'
type SortKey = 'key' | 'requests' | 'rate' | 'tokens' | 'p50' | 'p95' | 'first' | 'last'
// Breakdown 为能展开账户×模型组合明细的维度
type Breakdown = 'account' | 'model'

// PairDetail 为一行已展开的组合明细：report 为发起读取时的报告，与当前报告不同即为过期；
// 重新读取期间保留上一次的明细，避免自动刷新时整段闪烁
interface PairDetail {
  state: 'loading' | 'ready' | 'error'
  items: UsagePair[]
  error: string
  report: UsageReport
  controller?: AbortController
}

// CHILD_STEP 为组合明细每次显示的行数：一个模型的组合可达上万个账户，先显示一部分，按需再显示
const CHILD_STEP = 50

const props = defineProps<{
  report: UsageReport
  now: number
  // loadPairs 读取当前报告中一行的组合明细（主报告不带组合，见 UsagePanel.vue）
  loadPairs: (dimension: Breakdown, key: string, signal: AbortSignal) => Promise<UsagePair[]>
}>()
const emit = defineEmits<{
  filter: [dimension: UsageDimension, key: string]
  status: [code: string]
}>()

const { t, tf, errorText } = useI18n()
const { compact, integer, duration, percent, ago, dimensionLabel } = useUsageFormat()
const id = useId()
const tabs: Tab[] = ['account', 'model', 'channel', 'protocol', 'state', 'pool', 'status']
const tab = ref<Tab>(
  readPreference('aistudio2api_usage_ranking', 'account', (value) => tabs.includes(value as Tab)),
)
const sort = ref<{ key: SortKey; descending: boolean }>({ key: 'requests', descending: true })
const expanded = ref(new Set<string>())
// details 为已展开行的组合明细，limits 为各行当前显示的明细行数，键都是行取值
const details = shallowRef(new Map<string, PairDetail>())
const limits = ref(new Map<string, number>())

const columns: { key: SortKey; label: () => string; numeric: boolean }[] = [
  { key: 'key', label: () => t(`usage.dimension.${tab.value}`), numeric: false },
  { key: 'requests', label: () => t('usage.requests'), numeric: true },
  { key: 'rate', label: () => t('usage.successRate'), numeric: true },
  { key: 'tokens', label: () => t('usage.tokens'), numeric: true },
  { key: 'p50', label: () => `${t('usage.duration')} P50`, numeric: true },
  { key: 'p95', label: () => `${t('usage.duration')} P95`, numeric: true },
  { key: 'first', label: () => `${t('usage.firstEvent')} P95`, numeric: true },
  { key: 'last', label: () => t('usage.lastRequest'), numeric: true },
]

// sortValue 返回行在排序列上的比较值
function sortValue(key: string, stats: UsageStats, column: SortKey): number | string {
  switch (column) {
    case 'key':
      return dimensionLabel(tab.value, key).toLowerCase()
    case 'requests':
      return stats.requests
    case 'rate':
      return successRate(stats) ?? -1
    case 'tokens':
      return stats.total_tokens
    case 'p50':
      return stats.duration.p50_ms
    case 'p95':
      return stats.duration.p95_ms
    case 'first':
      return stats.first_event.p95_ms
    case 'last':
      return stats.last_at ? new Date(stats.last_at).getTime() : 0
  }
}

const rows = computed(() => {
  const { key, descending } = sort.value
  return [...(props.report.groups[tab.value] ?? [])].sort((a, b) => {
    const left = sortValue(a.key, a, key)
    const right = sortValue(b.key, b, key)
    const order = left < right ? -1 : left > right ? 1 : 0
    return descending ? -order : order
  })
})

// 排行每页显示的行数沿用其他列表的分页（默认 50 行，每页条数记在本机）；切换维度或排序时回到第一页，报告刷新时停在当前页
const { page, pageSize, pageItems } = usePaging(
  () => rows.value,
  'aistudio2api_usage_ranking_page_size',
  50,
)
watch(sort, () => {
  page.value = 1
})
watch(tab, (value) => {
  writePreference('aistudio2api_usage_ranking', value)
  expanded.value = new Set()
  clearDetails()
  page.value = 1
})

// breakdown 为当前维度能否展开组合明细：只有账户与模型行可以展开
const breakdown = computed<Breakdown | null>(() =>
  tab.value === 'account' || tab.value === 'model' ? tab.value : null,
)

// setDetail 替换一行的明细（整体替换 Map，触发更新）
function setDetail(key: string, detail: PairDetail | undefined): void {
  const next = new Map(details.value)
  if (detail === undefined) next.delete(key)
  else next.set(key, detail)
  details.value = next
}

// dropDetail 丢弃一行的明细并中止进行中的读取
function dropDetail(key: string): void {
  details.value.get(key)?.controller?.abort()
  setDetail(key, undefined)
}

// clearDetails 丢弃全部明细
function clearDetails(): void {
  for (const detail of details.value.values()) detail.controller?.abort()
  details.value = new Map()
  limits.value = new Map()
}

// fetchPairs 读取一行在当前报告范围内的组合明细；读取期间报告更新或行被收起时中止，结果丢弃
async function fetchPairs(dimension: Breakdown, key: string): Promise<void> {
  const report = props.report
  const previous = details.value.get(key)
  previous?.controller?.abort()
  const controller = new AbortController()
  setDetail(key, { state: 'loading', items: previous?.items ?? [], error: '', report, controller })
  try {
    const items = await props.loadPairs(dimension, key, controller.signal)
    if (controller.signal.aborted) return
    setDetail(key, { state: 'ready', items, error: '', report })
  } catch (reason) {
    if (controller.signal.aborted) return
    setDetail(key, { state: 'error', items: [], error: errorText(reason), report })
  }
}

// syncDetails 让明细跟上报告与分页：收起的行与不在当前页的展开行丢弃明细（翻回来时再读），当前页展开的行
// 没有明细或明细属于旧报告时读取。报告自动刷新后只重读仍展开且在当前页的行
function syncDetails(): void {
  const dimension = breakdown.value
  if (dimension === null) return
  const visible = new Set(pageItems.value.map((row) => row.key))
  for (const key of details.value.keys()) {
    if (!expanded.value.has(key) || !visible.has(key)) dropDetail(key)
  }
  for (const key of expanded.value) {
    if (!visible.has(key)) continue
    const detail = details.value.get(key)
    if (detail === undefined || detail.report !== props.report) void fetchPairs(dimension, key)
  }
}

watch([() => props.report, pageItems, expanded], syncDetails)
onBeforeUnmount(clearDetails)

// children 返回一行已读到的组合明细（按请求数降序），只取当前显示的行数
function children(key: string): { key: string; label: string; stats: UsageStats }[] {
  const detail = details.value.get(key)
  if (detail === undefined) return []
  const other: Breakdown = tab.value === 'account' ? 'model' : 'account'
  return detail.items
    .slice(0, limits.value.get(key) ?? CHILD_STEP)
    .map((pair) => ({ key: pair[other], label: dimensionLabel(other, pair[other]), stats: pair }))
}

// hiddenChildren 返回一行还没显示的明细行数
function hiddenChildren(key: string): number {
  const total = details.value.get(key)?.items.length ?? 0
  return Math.max(0, total - (limits.value.get(key) ?? CHILD_STEP))
}

// showMoreChildren 多显示一批明细行
function showMoreChildren(key: string): void {
  const next = new Map(limits.value)
  next.set(key, (next.get(key) ?? CHILD_STEP) + CHILD_STEP)
  limits.value = next
}

// toggleSort 切换排序列，再次点击同一列时反转方向
function toggleSort(key: SortKey): void {
  sort.value =
    sort.value.key === key
      ? { key, descending: !sort.value.descending }
      : { key, descending: key !== 'key' }
}

// toggleRow 展开或收起一行的组合明细：展开时读取（见 syncDetails），收起时丢弃
function toggleRow(key: string): void {
  const next = new Set(expanded.value)
  if (next.has(key)) {
    next.delete(key)
    const limit = new Map(limits.value)
    limit.delete(key)
    limits.value = limit
  } else next.add(key)
  expanded.value = next
}

// retryPairs 重新读取一行读取失败的明细
function retryPairs(key: string): void {
  if (breakdown.value !== null) void fetchPairs(breakdown.value, key)
}

// applyFilter 把行取值加入筛选；状态码行筛选请求记录
function applyFilter(key: string): void {
  if (tab.value === 'status') emit('status', key)
  else emit('filter', tab.value, key)
}

// moveTab 用左右方向键切换分项
function moveTab(step: number): void {
  const index = tabs.indexOf(tab.value)
  tab.value = tabs[(index + step + tabs.length) % tabs.length] ?? tab.value
}

const total = computed(() => props.report.totals.requests)
</script>

<template>
  <section class="panel min-w-0">
    <header class="flex flex-wrap items-center gap-3 border-b border-line px-4 pt-3">
      <h3 class="mr-auto pb-3 text-sm font-medium text-gray-200">{{ t('usage.ranking') }}</h3>
      <div role="tablist" :aria-label="t('usage.ranking')" class="flex gap-1 overflow-x-auto">
        <button
          v-for="item in tabs"
          :id="`${id}-tab-${item}`"
          :key="item"
          type="button"
          role="tab"
          :aria-selected="tab === item"
          :aria-controls="`${id}-panel`"
          :tabindex="tab === item ? 0 : -1"
          class="border-b-2 px-2.5 pb-2.5 text-xs whitespace-nowrap transition-colors"
          :class="
            tab === item
              ? 'border-blue-500 text-white'
              : 'border-transparent text-gray-400 hover:text-gray-200'
          "
          @click="tab = item"
          @keydown.right.prevent="moveTab(1)"
          @keydown.left.prevent="moveTab(-1)"
        >
          {{ t(`usage.dimension.${item}`) }}
          <span class="ml-1 text-gray-500 tabular-nums">{{
            (report.groups[item] ?? []).length
          }}</span>
        </button>
      </div>
    </header>
    <div
      :id="`${id}-panel`"
      role="tabpanel"
      :aria-labelledby="`${id}-tab-${tab}`"
      class="relative overflow-x-auto"
    >
      <table class="w-full min-w-[56rem] text-left text-xs">
        <thead class="text-gray-500">
          <tr>
            <th
              v-for="column in columns"
              :key="column.key"
              scope="col"
              class="px-4 py-2 font-normal"
              :class="column.numeric ? 'text-right' : ''"
              :aria-sort="
                sort.key === column.key ? (sort.descending ? 'descending' : 'ascending') : 'none'
              "
            >
              <button
                type="button"
                class="inline-flex items-center gap-1 hover:text-gray-200"
                :class="sort.key === column.key ? 'text-gray-200' : ''"
                @click="toggleSort(column.key)"
              >
                {{ column.label() }}
                <UiIcon
                  v-if="sort.key === column.key"
                  name="chevronDown"
                  :size="11"
                  :class="sort.descending ? '' : 'rotate-180'"
                />
              </button>
            </th>
            <th scope="col" class="w-24 px-2">
              <span class="sr-only">{{ t('usage.onlyThis') }}</span>
            </th>
          </tr>
        </thead>
        <tbody>
          <template v-for="row in pageItems" :key="row.key">
            <tr class="border-t border-line/60 text-gray-300 hover:bg-raised/40">
              <th scope="row" class="max-w-72 px-4 py-2 font-normal">
                <div class="flex items-center gap-1.5">
                  <button
                    v-if="breakdown !== null"
                    type="button"
                    class="rounded p-0.5 text-gray-500 hover:bg-raised hover:text-white"
                    :aria-expanded="expanded.has(row.key)"
                    :aria-label="`${
                      expanded.has(row.key) ? t('usage.hideBreakdown') : t('usage.showBreakdown')
                    }: ${dimensionLabel(tab, row.key)}`"
                    @click="toggleRow(row.key)"
                  >
                    <!-- 读取中换成转圈图标；不用 aria-busy：全局样式会给 aria-busy 的按钮再画一个转圈 -->
                    <UiIcon
                      v-if="details.get(row.key)?.state === 'loading'"
                      name="spinner"
                      :size="12"
                    />
                    <UiIcon
                      v-else
                      name="chevronRight"
                      :size="12"
                      class="transition-transform"
                      :class="expanded.has(row.key) ? 'rotate-90' : ''"
                    />
                  </button>
                  <span v-else class="w-4"></span>
                  <span v-tooltip="dimensionLabel(tab, row.key)" class="truncate text-gray-200">{{
                    dimensionLabel(tab, row.key)
                  }}</span>
                </div>
                <div class="mt-1 ml-5 h-1 rounded-full bg-raised">
                  <div
                    class="h-1 rounded-full bg-blue-500/70"
                    :style="{ width: `${Math.max((row.requests / Math.max(total, 1)) * 100, 1)}%` }"
                  ></div>
                </div>
              </th>
              <td class="px-4 py-2 text-right tabular-nums">
                {{ integer(row.requests) }}
                <span class="ml-1 text-gray-500">{{
                  percent(row.requests / Math.max(total, 1), 0)
                }}</span>
              </td>
              <td class="px-4 py-2 text-right tabular-nums" :class="rateTone(successRate(row))">
                {{ percent(successRate(row)) }}
              </td>
              <td class="px-4 py-2 text-right tabular-nums">{{ compact(row.total_tokens) }}</td>
              <td class="px-4 py-2 text-right tabular-nums">{{ duration(row.duration.p50_ms) }}</td>
              <td class="px-4 py-2 text-right tabular-nums">{{ duration(row.duration.p95_ms) }}</td>
              <td class="px-4 py-2 text-right tabular-nums">
                {{ duration(row.first_event.p95_ms) }}
              </td>
              <td class="px-4 py-2 text-right whitespace-nowrap text-gray-400">
                {{ row.last_at ? ago(row.last_at, now) : '—' }}
              </td>
              <td class="px-2 py-2 text-right">
                <!-- 未分配（空取值）不能作为筛选条件：服务端忽略空筛选值，筛选后仍显示全部请求 -->
                <button
                  v-if="row.key !== ''"
                  type="button"
                  class="btn btn-sm btn-ghost"
                  :aria-label="`${t('usage.onlyThis')}: ${dimensionLabel(tab, row.key)}`"
                  @click="applyFilter(row.key)"
                >
                  {{ t('usage.onlyThis') }}
                </button>
              </td>
            </tr>
            <template v-if="expanded.has(row.key)">
              <!-- 明细状态：第一次读取中、读取失败（可重试）、没有组合；重新读取期间照旧显示上一次的明细 -->
              <tr
                v-if="
                  details.get(row.key)?.state === 'loading' &&
                  (details.get(row.key)?.items.length ?? 0) === 0
                "
                class="bg-canvas/60 text-gray-500"
              >
                <!-- role 放在单元格里面的一层：放在 td 上会覆盖单元格自身的 cell 角色，读屏软件按表格导航时这一行没有单元格 -->
                <td colspan="9" class="py-1.5 pr-4 pl-11">
                  <div role="status">{{ t('usage.pairsLoading') }}</div>
                </td>
              </tr>
              <tr v-else-if="details.get(row.key)?.state === 'error'" class="bg-canvas/60">
                <td colspan="9" class="py-1.5 pr-4 pl-11 text-red-300">
                  <div role="alert">
                    <span class="break-words">{{
                      tf('usage.pairsFailed', { error: details.get(row.key)?.error ?? '' })
                    }}</span>
                    <button
                      type="button"
                      class="btn btn-sm btn-ghost ml-2"
                      @click="retryPairs(row.key)"
                    >
                      {{ t('usage.retry') }}
                    </button>
                  </div>
                </td>
              </tr>
              <tr
                v-else-if="
                  details.get(row.key)?.state === 'ready' &&
                  (details.get(row.key)?.items.length ?? 0) === 0
                "
                class="bg-canvas/60 text-gray-500"
              >
                <td colspan="9" class="py-1.5 pr-4 pl-11">{{ t('usage.pairsEmpty') }}</td>
              </tr>
              <tr
                v-for="child in children(row.key)"
                :key="`${row.key}/${child.key}`"
                class="bg-canvas/60 text-gray-400"
              >
                <th scope="row" class="max-w-72 py-1.5 pr-4 pl-11 font-normal">
                  <span v-tooltip="child.label" class="block truncate">{{ child.label }}</span>
                </th>
                <td class="px-4 py-1.5 text-right tabular-nums">
                  {{ integer(child.stats.requests) }}
                </td>
                <td
                  class="px-4 py-1.5 text-right tabular-nums"
                  :class="rateTone(successRate(child.stats))"
                >
                  {{ percent(successRate(child.stats)) }}
                </td>
                <td class="px-4 py-1.5 text-right tabular-nums">
                  {{ compact(child.stats.total_tokens) }}
                </td>
                <td class="px-4 py-1.5 text-right tabular-nums">
                  {{ duration(child.stats.duration.p50_ms) }}
                </td>
                <td class="px-4 py-1.5 text-right tabular-nums">
                  {{ duration(child.stats.duration.p95_ms) }}
                </td>
                <td class="px-4 py-1.5 text-right tabular-nums">
                  {{ duration(child.stats.first_event.p95_ms) }}
                </td>
                <td class="px-4 py-1.5 text-right whitespace-nowrap">
                  {{ child.stats.last_at ? ago(child.stats.last_at, now) : '—' }}
                </td>
                <td></td>
              </tr>
              <tr v-if="hiddenChildren(row.key) > 0" class="bg-canvas/60">
                <td colspan="9" class="py-1.5 pr-4 pl-11">
                  <button
                    type="button"
                    class="btn btn-sm btn-ghost"
                    @click="showMoreChildren(row.key)"
                  >
                    {{
                      tf('usage.pairsMore', {
                        count: Math.min(CHILD_STEP, hiddenChildren(row.key)),
                        hidden: hiddenChildren(row.key),
                      })
                    }}
                  </button>
                </td>
              </tr>
            </template>
          </template>
        </tbody>
      </table>
    </div>
    <div class="border-t border-line px-3 py-2">
      <UiPager v-model:page="page" v-model:page-size="pageSize" :total="rows.length" />
    </div>
  </section>
</template>
