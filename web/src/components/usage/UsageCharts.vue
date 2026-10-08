<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from '@/i18n'
import type { UsageDimension, UsageReport } from '@/types'
import UiSelect from '../UiSelect.vue'
import EChart from './EChart.vue'
import { useUsageFormat } from './format'
import {
  assignColors,
  dimensions,
  otherColor,
  readPreference,
  successRate,
  writePreference,
} from './model'

const props = defineProps<{ report: UsageReport; stack: UsageDimension }>()
const emit = defineEmits<{ 'update:stack': [value: UsageDimension] }>()

const { t } = useI18n()
const { compact, integer, duration, percent, bucketLabel, dimensionLabel } = useUsageFormat()
const metric = ref<'requests' | 'tokens'>(
  readPreference('aistudio2api_usage_metric', 'requests', (value) =>
    ['requests', 'tokens'].includes(value as string),
  ),
)
watch(metric, (value) => writePreference('aistudio2api_usage_metric', value))

// theme 与 style.css 的颜色令牌一致（画布无法读取 CSS 变量）
const theme = {
  axis: '#6b7280',
  grid: '#21262d',
  line: '#30363d',
  surface: '#1c2128',
  text: '#c9d1d9',
}

const labels = computed(() => {
  const { buckets, bucket_seconds: seconds, from, to } = props.report
  const multiDay = new Date(to).getTime() - new Date(from).getTime() > 86_400_000
  return buckets.map((bucket) => bucketLabel(bucket.at, seconds, multiDay))
})

// TooltipItem 是 ECharts 坐标轴提示框传入的单个序列值
interface TooltipItem {
  axisValueLabel: string
  seriesName: string
  value: number | null
  color: string
}

// tooltipRows 生成坐标轴提示框内容，可按数值降序并追加合计；序列名经过转义
function tooltipRows(
  items: TooltipItem[],
  format: (value: number, item: TooltipItem) => string,
  options: { sort?: boolean; total?: boolean } = {},
): string {
  const rows = items.filter((item) => item.value !== null && item.value !== undefined)
  if (options.sort) rows.sort((a, b) => (b.value ?? 0) - (a.value ?? 0))
  const escape = (value: string): string =>
    value.replace(/[&<>"']/g, (char) => `&#${char.charCodeAt(0)};`)
  const lines = rows.map(
    (item) =>
      `<div style="display:flex;gap:12px;justify-content:space-between"><span><span style="display:inline-block;width:8px;height:8px;border-radius:2px;margin-right:6px;background:${escape(item.color)}"></span>${escape(item.seriesName)}</span><span style="font-variant-numeric:tabular-nums">${escape(format(item.value ?? 0, item))}</span></div>`,
  )
  if (options.total && rows[0]) {
    const total = rows.reduce((sum, item) => sum + (item.value ?? 0), 0)
    lines.push(
      `<div style="display:flex;justify-content:space-between;margin-top:4px;padding-top:4px;border-top:1px solid ${theme.line}"><span>${escape(t('usage.total'))}</span><span style="font-variant-numeric:tabular-nums">${escape(format(total, rows[0]))}</span></div>`,
    )
  }
  return `<div style="min-width:180px"><div style="margin-bottom:4px;color:${theme.axis}">${escape(items[0]?.axisValueLabel ?? '')}</div>${lines.join('')}</div>`
}

// base 返回各图共用的坐标轴、网格与提示框配置
function base(format: (value: number) => string) {
  return {
    animationDuration: 250,
    aria: { enabled: true },
    grid: { left: 8, right: 8, top: 36, bottom: 4, containLabel: true },
    legend: {
      type: 'scroll',
      top: 0,
      left: 0,
      textStyle: { color: theme.axis },
      pageTextStyle: { color: theme.axis },
      itemWidth: 10,
      itemHeight: 10,
    },
    tooltip: {
      trigger: 'axis',
      confine: true,
      backgroundColor: theme.surface,
      borderColor: theme.line,
      textStyle: { color: theme.text, fontSize: 12 },
    },
    xAxis: {
      type: 'category',
      data: labels.value,
      axisLine: { lineStyle: { color: theme.line } },
      axisTick: { show: false },
      axisLabel: { color: theme.axis, hideOverlap: true },
    },
    yAxis: [
      {
        type: 'value',
        splitLine: { lineStyle: { color: theme.grid } },
        axisLabel: { color: theme.axis, formatter: format },
      },
    ],
  }
}

const trafficOption = computed(() => {
  const { series, stack_by: stackBy } = props.report
  const colors = assignColors(series.filter((item) => !item.other).map((item) => item.key))
  const shared = base(compact)
  return {
    ...shared,
    tooltip: {
      ...shared.tooltip,
      axisPointer: { type: 'shadow' },
      formatter: (items: TooltipItem[]) => tooltipRows(items, integer, { sort: true, total: true }),
    },
    series: series.map((item) => ({
      name: item.other ? t('usage.other') : dimensionLabel(stackBy, item.key),
      type: 'bar',
      stack: 'traffic',
      barMaxWidth: 22,
      emphasis: { focus: 'series' },
      color: item.other ? otherColor : colors.get(item.key),
      data: metric.value === 'tokens' ? item.tokens : item.requests,
    })),
  }
})

const outcomesOption = computed(() => {
  const buckets = props.report.buckets
  const rateName = t('usage.successRate')
  const rate = (value: number) => percent(value / 100, 0)
  const shared = base(compact)
  const bar = (name: string, color: string, data: number[]) => ({
    name,
    type: 'bar',
    stack: 'outcomes',
    barMaxWidth: 22,
    color,
    data,
  })
  return {
    ...shared,
    tooltip: {
      ...shared.tooltip,
      axisPointer: { type: 'shadow' },
      formatter: (items: TooltipItem[]) =>
        tooltipRows(items, (value, item) =>
          item.seriesName === rateName ? percent(value / 100) : integer(value),
        ),
    },
    yAxis: [
      ...shared.yAxis,
      {
        type: 'value',
        min: 0,
        max: 100,
        splitLine: { show: false },
        axisLabel: { color: theme.axis, formatter: rate },
      },
    ],
    series: [
      bar(
        t('usage.succeeded'),
        '#22c55e',
        buckets.map((item) => item.succeeded),
      ),
      bar(
        t('usage.rateLimited'),
        '#f59e0b',
        buckets.map((item) => item.rate_limited),
      ),
      bar(
        t('usage.otherErrors'),
        '#ef4444',
        buckets.map((item) => item.failed - item.rate_limited),
      ),
      bar(
        t('usage.canceled'),
        otherColor,
        buckets.map((item) => item.canceled),
      ),
      {
        name: rateName,
        type: 'line',
        yAxisIndex: 1,
        symbol: 'circle',
        symbolSize: 4,
        showSymbol: false,
        connectNulls: false,
        color: '#60a5fa',
        data: buckets.map((item) => {
          const value = successRate(item)
          return value === null ? null : Math.round(value * 1000) / 10
        }),
      },
    ],
  }
})

const latencyOption = computed(() => {
  const { buckets, totals } = props.report
  const zeroToNull = (value: number) => (value > 0 ? Math.round(value) : null)
  const shared = base(duration)
  const line = (name: string, color: string, data: (number | null)[]) => ({
    name,
    type: 'line',
    symbol: 'circle',
    symbolSize: 4,
    showSymbol: false,
    connectNulls: false,
    color,
    data,
  })
  return {
    ...shared,
    tooltip: {
      ...shared.tooltip,
      formatter: (items: TooltipItem[]) => tooltipRows(items, duration),
    },
    series: [
      line(
        `${t('usage.duration')} P50`,
        '#60a5fa',
        buckets.map((item) => zeroToNull(item.duration.p50_ms)),
      ),
      {
        ...line(
          `${t('usage.duration')} P95`,
          '#a78bfa',
          buckets.map((item) => zeroToNull(item.duration.p95_ms)),
        ),
        markLine:
          totals.duration.p95_ms > 0
            ? {
                silent: true,
                symbol: 'none',
                lineStyle: { type: 'dashed', color: '#a78bfa', opacity: 0.6 },
                label: {
                  color: theme.axis,
                  position: 'insideEndTop',
                  formatter: () => `P95 ${duration(totals.duration.p95_ms)}`,
                },
                data: [{ yAxis: Math.round(totals.duration.p95_ms) }],
              }
            : undefined,
      },
      line(
        `${t('usage.firstEvent')} P95`,
        '#34d399',
        buckets.map((item) => zeroToNull(item.first_event.p95_ms)),
      ),
    ],
  }
})

const tokensOption = computed(() => {
  const buckets = props.report.buckets
  const shared = base(compact)
  const area = (name: string, color: string, data: number[]) => ({
    name,
    type: 'line',
    stack: 'tokens',
    symbol: 'none',
    color,
    lineStyle: { width: 1 },
    areaStyle: { opacity: 0.35 },
    data,
  })
  return {
    ...shared,
    tooltip: {
      ...shared.tooltip,
      formatter: (items: TooltipItem[]) => tooltipRows(items, compact, { total: true }),
    },
    series: [
      area(
        t('usage.input'),
        '#60a5fa',
        buckets.map((item) => item.input_tokens),
      ),
      area(
        t('usage.reasoning'),
        '#a78bfa',
        buckets.map((item) => item.reasoning_tokens),
      ),
      area(
        t('usage.reply'),
        '#34d399',
        buckets.map((item) => item.reply_tokens),
      ),
    ],
  }
})
</script>

<template>
  <div class="grid grid-cols-1 gap-4 xl:grid-cols-2">
    <figure class="panel min-w-0 p-4 xl:col-span-2">
      <div class="mb-2 flex flex-wrap items-center gap-2">
        <figcaption class="mr-auto">
          <span class="text-sm font-medium text-gray-200">{{ t('usage.chartTraffic') }}</span>
          <span class="ml-2 text-xs text-gray-500">{{ t('usage.chartTrafficHelp') }}</span>
        </figcaption>
        <UiSelect
          :model-value="stack"
          :aria-label="t('usage.stackBy')"
          class="input h-7 w-32 text-xs"
          @update:model-value="emit('update:stack', $event as UsageDimension)"
        >
          <option v-for="item in dimensions" :key="item" :value="item">
            {{ t(`usage.dimension.${item}`) }}
          </option>
        </UiSelect>
        <div class="seg" role="radiogroup" :aria-label="t('usage.chartTraffic')">
          <button
            v-for="item in ['requests', 'tokens'] as const"
            :key="item"
            type="button"
            role="radio"
            :aria-checked="metric === item"
            class="seg-item"
            :class="{ active: metric === item }"
            @click="metric = item"
          >
            {{ t(item === 'requests' ? 'usage.metricRequests' : 'usage.metricTokens') }}
          </button>
        </div>
      </div>
      <div class="h-72"><EChart :option="trafficOption" /></div>
    </figure>
    <figure class="panel min-w-0 p-4">
      <figcaption class="mb-2 text-sm font-medium text-gray-200">
        {{ t('usage.chartOutcomes') }}
      </figcaption>
      <div class="h-60"><EChart :option="outcomesOption" /></div>
    </figure>
    <figure class="panel min-w-0 p-4">
      <figcaption class="mb-2 text-sm font-medium text-gray-200">
        {{ t('usage.chartLatency') }}
      </figcaption>
      <div class="h-60"><EChart :option="latencyOption" /></div>
    </figure>
    <figure class="panel min-w-0 p-4 xl:col-span-2">
      <figcaption class="mb-2 text-sm font-medium text-gray-200">
        {{ t('usage.chartTokens') }}
      </figcaption>
      <div class="h-56"><EChart :option="tokensOption" /></div>
    </figure>
  </div>
</template>
