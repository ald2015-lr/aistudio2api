import type { AdminLog } from './types'

// LogRow 保存请求的最新状态与完整事件时间线
export interface LogRow {
  key: string
  entry: AdminLog
  events: AdminLog[]
}

// logKeys 为每条非请求日志分配稳定的键。原先用 时间:下标 作键，缓冲区满后从前面丢弃日志，
// 每来一条新日志所有下标都会变，选中的日志随之消失、详情面板关闭
const logKeys = new WeakMap<AdminLog, string>()
let nextLogKey = 0

function logKey(entry: AdminLog): string {
  let key = logKeys.get(entry)
  if (key === undefined) {
    nextLogKey += 1
    key = `log:${nextLogKey}`
    logKeys.set(entry, key)
  }
  return key
}

// groupLogs 按请求标识合并生命周期并保持服务事件的原始顺序
export function groupLogs(logs: readonly AdminLog[]): LogRow[] {
  const rows: LogRow[] = []
  const requests = new Map<string, LogRow>()
  for (const entry of logs) {
    const id = entry.request?.id
    const row = id ? requests.get(id) : undefined
    if (row) {
      row.events.push(entry)
      row.entry = { ...entry, request: { ...row.entry.request!, ...entry.request! } }
    } else {
      const next = { key: id || logKey(entry), entry, events: [entry] }
      rows.push(next)
      if (id) requests.set(id, next)
    }
  }
  return rows
}

export interface MessageField {
  key: string
  value: string
}

// ParsedMessage 服务端日志形如 "标题 | 键=值 | 键=值 | 说明"
export interface ParsedMessage {
  title: string
  fields: MessageField[]
  notes: string[]
}

// parseMessage 拆出标题、键值字段与其余说明
export function parseMessage(message: string): ParsedMessage {
  const parts = message.split(' | ')
  const title = (parts.shift() ?? '').trim()
  const fields: MessageField[] = []
  const notes: string[] = []
  for (const raw of parts) {
    const part = raw.trim()
    if (part === '') continue
    const separator = part.indexOf('=')
    if (separator > 0 && separator < 24) {
      fields.push({ key: part.slice(0, separator), value: part.slice(separator + 1) })
    } else {
      notes.push(part)
    }
  }
  return { title, fields, notes }
}

export type LogCategory = 'request' | 'account' | 'service'

// rowCategory 区分请求、账户与服务三类日志
export function rowCategory(row: LogRow): LogCategory {
  if (row.entry.request !== undefined) return 'request'
  if (row.entry.source === 'service') return 'service'
  return 'account'
}

// 精简模式隐藏的过程日志：Worker 启动的逐步进度、运行时装配步骤等
const NOISE_PATTERNS: readonly RegExp[] = [
  /^WAA Worker 启动 \| \d+\/\d+/,
  /^运行时装配 \| \d+\/\d+/,
  /^账户验证 \| 访问 AI Studio/,
]

// isNoise 判断日志行是否为可隐藏的过程日志；请求行与警告、错误始终保留
export function isNoise(row: LogRow): boolean {
  if (row.entry.request !== undefined) return false
  if (row.entry.level.toUpperCase() !== 'INFO') return false
  return NOISE_PATTERNS.some((pattern) => pattern.test(row.entry.message))
}

// LogItem 列表中的一行：单条日志，或一分钟内连续出现的同类信息合并成的一组
export interface LogItem {
  key: string
  row: LogRow
  members: LogRow[]
}

// groupKey 返回可合并日志的分组键；请求、警告、错误与非结构化消息不合并
function groupKey(row: LogRow): string | null {
  if (row.entry.request !== undefined) return null
  if (row.entry.level.toUpperCase() !== 'INFO') return null
  if (!row.entry.message.includes(' | ')) return null
  return `${rowCategory(row)}:${parseMessage(row.entry.message).title}`
}

// aggregateRows 把相邻间隔不超过 windowMs 的同类信息合并为一组，组显示在最新一条的位置
export function aggregateRows(rows: readonly LogRow[], windowMs = 60_000): LogItem[] {
  const anchors: (LogItem | undefined)[] = new Array<LogItem | undefined>(rows.length)
  const open = new Map<string, { item: LogItem; index: number; time: number }>()
  rows.forEach((row, index) => {
    const key = groupKey(row)
    if (key === null) {
      anchors[index] = { key: row.key, row, members: [row] }
      return
    }
    const time = Date.parse(row.entry.time)
    const current = open.get(key)
    if (current !== undefined && Math.abs(time - current.time) <= windowMs) {
      anchors[current.index] = undefined
      current.item.members.push(row)
      current.item.row = row
      anchors[index] = current.item
      current.index = index
      current.time = time
      return
    }
    const item: LogItem = { key: row.key, row, members: [row] }
    anchors[index] = item
    open.set(key, { item, index, time })
  })
  return anchors.filter((item): item is LogItem => item !== undefined)
}

// singleItems 不合并时把每行包装成列表项
export function singleItems(rows: readonly LogRow[]): LogItem[] {
  return rows.map((row) => ({ key: row.key, row, members: [row] }))
}

// rowSummary 生成单行摘要：请求行显示状态、模型、耗时与用量，其余显示消息原文
export function rowSummary(row: LogRow): string {
  const request = row.entry.request
  if (request === undefined) return row.entry.message
  const parts: string[] = [
    String(request.status ?? '…'),
    request.model || `${request.method ?? ''} ${request.path ?? ''}`.trim(),
  ]
  if (request.duration_ms !== undefined) parts.push(`${(request.duration_ms / 1000).toFixed(2)}s`)
  if (request.usage !== undefined) parts.push(`${request.usage.total_tokens} tok`)
  if (request.channel !== undefined) parts.push(request.channel)
  if (request.error) parts.push(request.error)
  return parts.join(' · ')
}
