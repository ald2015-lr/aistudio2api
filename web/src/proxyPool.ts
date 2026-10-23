// IP 池的界面辅助：组合校验与容量估算只用于设置页提示与提交前的检查，最终以服务端 ProxyPool.Validate 为准
import type { TranslationKey } from '@/i18n'
import type { ProxyPoolConfig, ProxyPoolStatus } from '@/types'

// 与服务端 config.DefaultProxyPoolPortStart、DefaultProxyPoolPortEnd、MaxProxyPoolPorts 一致
export const DEFAULT_PROXY_POOL_PORT_START = 10001
export const DEFAULT_PROXY_POOL_PORT_END = 10200
export const MAX_PROXY_POOL_PORTS = 5000
// DEFAULT_VERIFY_CONCURRENCY 与新账户自动处理的默认验证并发一致：登录与验证也各占一个端口
export const DEFAULT_VERIFY_CONCURRENCY = 3

// sessionPlaceholderPattern 与服务端一致：{session} 与 %7Bsession%7D 都不区分大小写
const sessionPlaceholderPattern = /\{session\}|%7Bsession%7D/i

// ProxyPoolProblem 为模式与地址、端口组合的第一条错误；空串表示通过
export type ProxyPoolProblem =
  | ''
  | 'url_required'
  | 'url_port'
  | 'static_session'
  | 'dynamic_session'
  | 'session_host'
  | 'ports'
  | 'ports_too_many'

const problemKeys: Record<Exclude<ProxyPoolProblem, ''>, TranslationKey> = {
  url_required: 'settings.proxyPoolInvalidURL',
  url_port: 'settings.proxyPoolInvalidURLPort',
  static_session: 'settings.proxyPoolInvalidStaticSession',
  dynamic_session: 'settings.proxyPoolInvalidDynamicSession',
  session_host: 'settings.proxyPoolInvalidSessionHost',
  ports: 'settings.proxyPoolInvalidPorts',
  ports_too_many: 'settings.proxyPoolInvalidPortsTooMany',
}

// cloneProxyPool 复制 IP 池设置：表单与已保存的配置不能共用同一个对象；字段顺序与服务端一致，快照对比不受影响
export function cloneProxyPool(value: ProxyPoolConfig): ProxyPoolConfig {
  return {
    mode: value.mode,
    url: value.url,
    port_start: value.port_start,
    port_end: value.port_end,
  }
}

// hasSessionPlaceholder 判断地址里是否有 {session} 占位
export function hasSessionPlaceholder(value: string): boolean {
  return sessionPlaceholderPattern.test(value)
}

function isPort(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 1 && value <= 65535
}

// schemePattern 与服务端一致：:// 前面是合法的协议名才算写了协议（字母开头，后面是字母、数字与 + - .）
const schemePattern = /^[A-Za-z][A-Za-z0-9+.-]*:\/\//
// colonFormPattern 为 host:port:用户名:密码 写法的开头（与服务端 colonProxyPattern 相同）
const colonFormPattern = /^(\[[0-9A-Fa-f:.]+\]|[^:@[\]/]+):[0-9]{1,5}:/
// hostPortPattern 为 host:port 结尾
const hostPortPattern = /:[0-9]{1,5}$/

// missingPort 粗查服务端的“不写协议时必须写端口”：没写协议时，host:port:用户名:密码 写法之外，最后一个 @ 之后（没有 @ 时为整串）
// 要以 :端口 结尾。静态 IP 也要写端口（任意一个，会按端口段替换）；其余格式问题交给服务端
function missingPort(url: string): boolean {
  if (schemePattern.test(url) || colonFormPattern.test(url)) return false
  const at = url.lastIndexOf('@')
  const hostPort = (at >= 0 ? url.slice(at + 1) : url).replace(/\/$/, '')
  return !hostPortPattern.test(hostPort)
}

// proxyPoolProblem 与服务端校验规则一致（端口任何模式下都校验；关闭时地址可以为空或保留，保留的地址照样要能规范化）。
// {session} 写在主机里只在 @ 写法下能判断，其余写法与地址的其他格式问题交给服务端（设置页把服务端的错误显示在地址框旁）
export function proxyPoolProblem(pool: ProxyPoolConfig): ProxyPoolProblem {
  if (!isPort(pool.port_start) || !isPort(pool.port_end) || pool.port_start > pool.port_end) {
    return 'ports'
  }
  if (pool.port_end - pool.port_start + 1 > MAX_PROXY_POOL_PORTS) return 'ports_too_many'
  const url = pool.url.trim()
  if (url !== '' && missingPort(url)) return 'url_port'
  if (pool.mode === 'off') return ''
  if (url === '') return 'url_required'
  const placeholder = hasSessionPlaceholder(url)
  if (pool.mode === 'static') return placeholder ? 'static_session' : ''
  if (!placeholder) return 'dynamic_session'
  const at = url.lastIndexOf('@')
  if (at >= 0 && hasSessionPlaceholder(url.slice(at + 1))) return 'session_host'
  return ''
}

// proxyPoolProblemKey 返回错误的翻译键；通过时返回 null
export function proxyPoolProblemKey(problem: ProxyPoolProblem): TranslationKey | null {
  return problem === '' ? null : problemKeys[problem]
}

// proxyPoolPortCount 返回端口段的端口数；端口不合法时为 0
export function proxyPoolPortCount(pool: ProxyPoolConfig): number {
  if (!isPort(pool.port_start) || !isPort(pool.port_end) || pool.port_start > pool.port_end)
    return 0
  return pool.port_end - pool.port_start + 1
}

// drainLimitFor 与服务端 drainLimitFor 一致：分区同时排空的上限 min(2 × max(启动并发, 1), max(1, 峰值 / 4))，峰值为 0 时为 0
export function drainLimitFor(maxActive: number, warmConcurrency: number): number {
  if (maxActive <= 0) return 0
  return Math.min(Math.max(warmConcurrency, 1) * 2, Math.max(1, Math.floor(maxActive / 4)))
}

// ProxyPoolCapacity 为可能同时运行的浏览器数：每个分区为 峰值 + 排空上限（排空时替补先启动、旧 Worker 后关闭），不含登录与验证
export interface ProxyPoolCapacity {
  normal: number
  normalDrain: number
  ultra: number
  ultraDrain: number
  total: number
}

// proxyPoolCapacity 按设置页当前的峰值与启动并发估算端口需求；数值不合法时返回 null
export function proxyPoolCapacity(
  maxActive: unknown,
  ultraMaxActive: unknown,
  warmConcurrency: unknown,
): ProxyPoolCapacity | null {
  const values = [maxActive, ultraMaxActive, warmConcurrency]
  if (
    !values.every((value) => typeof value === 'number' && Number.isInteger(value) && value >= 0)
  ) {
    return null
  }
  const normal = maxActive as number
  const ultra = ultraMaxActive as number
  const warm = warmConcurrency as number
  const normalDrain = drainLimitFor(normal, warm)
  const ultraDrain = drainLimitFor(ultra, warm)
  return {
    normal,
    normalDrain,
    ultra,
    ultraDrain,
    total: normal + normalDrain + ultra + ultraDrain,
  }
}

// isPortSlot 区分端口号与会话号：会话号至少 12 个字符，端口号最多 5 位数字
export function isPortSlot(slot: string): boolean {
  return /^\d{1,5}$/.test(slot)
}

type Format = (key: TranslationKey, values: Record<string, string | number>) => string

// slotLabel 把端口号或会话号显示为“端口 10023”“会话 3fa9c01b7d2e”
export function slotLabel(slot: string, tf: Format): string {
  return isPortSlot(slot)
    ? tf('proxy.slotPort', { port: slot })
    : tf('proxy.slotSession', { session: slot })
}

// formatClock 把 RFC 3339 时间显示为本地的时:分；解析不了时返回空串
export function formatClock(value: string | undefined, locale: string): string {
  const time = Date.parse(value ?? '')
  if (!Number.isFinite(time)) return ''
  return new Date(time).toLocaleTimeString(locale, { hour: '2-digit', minute: '2-digit' })
}

// proxyPoolActive 判断 IP 池是否开启（关闭后仍有旧出口时状态接口照样返回，mode 为 off）
export function proxyPoolActive(status: ProxyPoolStatus | null | undefined): boolean {
  return status !== null && status !== undefined && status.mode !== 'off'
}
