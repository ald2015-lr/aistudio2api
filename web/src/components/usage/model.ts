import { reactive, watch } from 'vue'
import type { UsageDimension, UsageFilters, UsageStats } from '@/types'
import { readStorage, writeStorage } from '@/storage'

// relativePresets 与 calendarPresets 是相对时间与日历两类快捷范围
export const relativePresets = ['1h', '6h', '24h', '7d', '30d', '90d'] as const
export const calendarPresets = ['today', 'yesterday', 'week', 'month', 'lastMonth'] as const
export type RangePreset =
  (typeof relativePresets)[number] | (typeof calendarPresets)[number] | 'custom'

export const dimensions: UsageDimension[] = ['model', 'account', 'channel', 'protocol', 'state']

// maxSpan 与服务端校验一致；retention 为账本保留时长
export const maxSpan = 93 * 86_400_000
export const retention = 90 * 86_400_000

const relativeSpans: Record<(typeof relativePresets)[number], number> = {
  '1h': 3_600_000,
  '6h': 6 * 3_600_000,
  '24h': 86_400_000,
  '7d': 7 * 86_400_000,
  '30d': 30 * 86_400_000,
  '90d': 90 * 86_400_000,
}

// UsageViewState 是用量页的视图状态；时间范围与堆叠维度记在本机浏览器，筛选只在本次打开期间有效
export interface UsageViewState {
  range: RangePreset
  from: string
  to: string
  stack: UsageDimension
  filters: UsageFilters
  status: string
  search: string
}

const VIEW_STORAGE_KEY = 'aistudio2api_usage_view'

// readView 读取上次的时间范围与堆叠维度，非法取值使用默认值
function readView(): Pick<UsageViewState, 'range' | 'from' | 'to' | 'stack'> {
  const fallback = {
    range: '24h' as RangePreset,
    from: '',
    to: '',
    stack: 'model' as UsageDimension,
  }
  const saved = readPreference<Record<string, unknown>>(
    VIEW_STORAGE_KEY,
    {},
    (value) => typeof value === 'object' && value !== null,
  )
  const presets: readonly string[] = [...relativePresets, ...calendarPresets, 'custom']
  const range = typeof saved.range === 'string' && presets.includes(saved.range) ? saved.range : ''
  const stack = typeof saved.stack === 'string' ? saved.stack : ''
  return {
    range: range === '' ? fallback.range : (range as RangePreset),
    from: typeof saved.from === 'string' ? saved.from : '',
    to: typeof saved.to === 'string' ? saved.to : '',
    stack: dimensions.includes(stack as UsageDimension)
      ? (stack as UsageDimension)
      : fallback.stack,
  }
}

// useUsageState 返回用量页视图状态，时间范围与堆叠维度变化时写入本机浏览器
export function useUsageState(): UsageViewState {
  const state = reactive<UsageViewState>({ ...readView(), filters: {}, status: '', search: '' })
  watch(
    () => [state.range, state.from, state.to, state.stack],
    () =>
      writePreference(VIEW_STORAGE_KEY, {
        range: state.range,
        from: state.range === 'custom' ? state.from : '',
        to: state.range === 'custom' ? state.to : '',
        stack: state.stack,
      }),
  )
  return state
}

// localInput 把时间格式化为 datetime-local 输入框的本地时间文本
export function localInput(date: Date): string {
  const pad = (value: number): string => String(value).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

// resolveRange 把视图状态解析为具体的起止时间，自定义范围无效时返回 null
export function resolveRange(
  state: Pick<UsageViewState, 'range' | 'from' | 'to'>,
  now: Date,
): { from: Date; to: Date } | null {
  const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  switch (state.range) {
    case 'today':
      return { from: midnight, to: now }
    case 'yesterday':
      return {
        from: new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1),
        to: midnight,
      }
    case 'week': {
      const weekday = (now.getDay() + 6) % 7
      return {
        from: new Date(now.getFullYear(), now.getMonth(), now.getDate() - weekday),
        to: now,
      }
    }
    case 'month':
      return { from: new Date(now.getFullYear(), now.getMonth(), 1), to: now }
    case 'lastMonth':
      return {
        from: new Date(now.getFullYear(), now.getMonth() - 1, 1),
        to: new Date(now.getFullYear(), now.getMonth(), 1),
      }
    case 'custom': {
      const from = new Date(state.from)
      const to = new Date(state.to)
      return validRange(from, to) === '' ? { from, to } : null
    }
    default:
      return { from: new Date(now.getTime() - relativeSpans[state.range]), to: now }
  }
}

// validRange 校验自定义范围，返回错误的翻译键或空字符串
export function validRange(from: Date, to: Date): '' | 'usage.rangeInvalid' | 'usage.rangeTooLong' {
  if (Number.isNaN(from.getTime()) || Number.isNaN(to.getTime()) || from >= to)
    return 'usage.rangeInvalid'
  if (to.getTime() - from.getTime() > maxSpan) return 'usage.rangeTooLong'
  return ''
}

// liveRange 判断范围终点是否跟随当前时间，用于决定是否自动刷新
export function liveRange(range: RangePreset): boolean {
  return range !== 'yesterday' && range !== 'lastMonth' && range !== 'custom'
}

// successRate 返回排除客户端取消后的成功比例
export function successRate(stats: UsageStats): number | null {
  const counted = stats.requests - stats.canceled
  return counted > 0 ? stats.succeeded / counted : null
}

// ratio 返回比例，分母为 0 时返回 null
export function ratio(part: number, total: number): number | null {
  return total > 0 ? part / total : null
}

// palette 是分类序列使用的颜色，按取值哈希分配使同一取值在各图中颜色一致
export const palette = [
  '#60a5fa',
  '#34d399',
  '#f59e0b',
  '#a78bfa',
  '#f472b6',
  '#22d3ee',
  '#fb923c',
  '#a3e635',
  '#e879f9',
  '#2dd4bf',
]
export const otherColor = '#6b7280'

// assignColors 为一组取值分配颜色，哈希冲突时顺延到未使用的颜色
export function assignColors(keys: string[]): Map<string, string> {
  const result = new Map<string, string>()
  const used = new Set<number>()
  for (const key of keys) {
    let hash = 0
    for (const char of key) hash = (hash * 31 + char.charCodeAt(0)) >>> 0
    let index = hash % palette.length
    for (let probe = 0; probe < palette.length && used.has(index); probe++)
      index = (index + 1) % palette.length
    used.add(index)
    result.set(key, palette[index] ?? otherColor)
  }
  return result
}

// protocolLabels 是公开协议的产品名，各语言相同；图片、音频等端点类别的显示名随界面语言（见 format.ts）。
// interactions 为 /v1/interactions 与 /v1beta/interactions，账本单独归类，不并入 Gemini
export const protocolLabels: Record<string, string> = {
  'openai-chat': 'OpenAI Chat',
  'openai-responses': 'OpenAI Responses',
  anthropic: 'Anthropic',
  gemini: 'Gemini',
  interactions: 'Gemini Interactions',
}

// stateTone 返回请求结果徽标的色调：失败红、取消灰、完成与工具调用绿，达到输出上限与上游终止黄
export function stateTone(state: string): string {
  if (state === 'failed') return 'bg-red-500/15 text-red-300'
  if (state === 'cancelled') return 'bg-gray-500/15 text-gray-300'
  if (state === 'completed' || state === 'tool_calls') return 'bg-green-500/15 text-green-300'
  return 'bg-yellow-500/15 text-yellow-300'
}

// latencyTone 按耗时阈值返回色调，首个事件与总耗时使用不同阈值
export function latencyTone(milliseconds: number, kind: 'duration' | 'first'): string {
  const [warn, slow] = kind === 'first' ? [10_000, 30_000] : [60_000, 180_000]
  if (milliseconds >= slow) return 'bg-red-500'
  if (milliseconds >= warn) return 'bg-yellow-500'
  return 'bg-green-500'
}

// rateTone 按成功率阈值返回文字色调
export function rateTone(rate: number | null): string {
  if (rate === null) return 'text-gray-500'
  if (rate >= 0.95) return 'text-green-400'
  if (rate >= 0.8) return 'text-yellow-400'
  return 'text-red-400'
}

// readPreference 读取浏览器保存的界面偏好，存储不可用或内容无效时返回默认值
export function readPreference<T>(key: string, fallback: T, valid: (value: unknown) => boolean): T {
  const raw = readStorage(key)
  if (raw === null) return fallback
  try {
    const value: unknown = JSON.parse(raw)
    return valid(value) ? (value as T) : fallback
  } catch {
    return fallback
  }
}

// writePreference 保存界面偏好，存储不可用时忽略
export function writePreference(key: string, value: unknown): void {
  writeStorage(key, JSON.stringify(value))
}
