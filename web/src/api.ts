import { EventSourceParserStream } from 'eventsource-parser/stream'
import type {
  Account,
  AccountDraft,
  AccountLoginInput,
  AdminEvent,
  BulkDeleteResult,
  BulkEnableInput,
  BulkEnableResult,
  ChromeImportInput,
  ChromeImportProfile,
  Model,
  OnboardingPolicy,
  OnboardingStatus,
  PlaygroundInput,
  PlaygroundMedia,
  Cooldown,
  FollowDefaultProxyResult,
  OrgDisabledAccount,
  PermissionDeniedAccount,
  ProxyPoolConfig,
  ProxyTestResult,
  RequestBody,
  RequestSummary,
  ServiceConfig,
  ServiceStatus,
  UsageRecordPage,
  UsageReport,
} from '@/types'

interface AccountsResponse {
  accounts: Account[]
}

interface AccountResponse {
  account: Account
}

interface ChromeProfilesResponse {
  profiles: ChromeImportProfile[]
}

interface ModelsResponse {
  models: Model[]
}

interface CooldownsResponse {
  cooldowns: Cooldown[]
}

interface RequestsResponse {
  requests: RequestSummary[]
}

// PermissionDeniedResponse 为 403 无权限账户列表；org_disabled 为所属组织停用了 AI Studio 的账户（旧版服务没有这个字段）
interface PermissionDeniedResponse {
  accounts: PermissionDeniedAccount[] | null
  org_disabled?: OrgDisabledAccount[] | null
}

// AccountProblemLists 为账户页上方单独列出的两类账户
export interface AccountProblemLists {
  permissionDenied: PermissionDeniedAccount[]
  orgDisabled: OrgDisabledAccount[]
}

interface PermissionDeniedClearResponse {
  cleared: number
}

export interface EventConnection {
  close: () => void
}

// ApiErrorDetails 为试用请求错误的补充信息：code 为错误外壳中的错误码或类型，retryAfter 为 Retry-After 的秒数，
// inStream 表示流已建立（HTTP 200）后在事件里返回的错误
export interface ApiErrorDetails {
  code?: string
  retryAfter?: number
  inStream?: boolean
}

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly retryAfter: number | undefined
  readonly inStream: boolean

  constructor(message: string, status: number, details: ApiErrorDetails = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = details.code ?? ''
    this.retryAfter = details.retryAfter
    this.inStream = details.inStream ?? false
  }
}

// StreamIncompleteError 表示流式响应在协议的结束事件之前断开（或没有响应体），界面按本地化文案显示；
// status 为建流时的 HTTP 状态码；reason 为读响应体时连接中断的浏览器原因（正常读到流末尾时为空）
export class StreamIncompleteError extends Error {
  readonly status: number
  readonly reason: string

  constructor(message: string, status: number, reason = '') {
    super(message)
    this.name = 'StreamIncompleteError'
    this.status = status
    this.reason = reason
  }
}

// readResponseBody 读取已建立响应的响应体：读的过程中连接中断时浏览器抛 TypeError（如 network error），与 fetch
// 连不上服务时相同；这里改抛带 HTTP 状态与原因的 StreamIncompleteError，界面按“已连接后失败”显示，不误报服务离线
async function readResponseBody<T>(response: Response, read: () => Promise<T>): Promise<T> {
  try {
    return await read()
  } catch (error) {
    if (error instanceof TypeError) {
      throw new StreamIncompleteError(error.message, response.status, error.message)
    }
    throw error
  }
}

// errorCode 读取错误外壳中的错误码：OpenAI 为 error.code，Anthropic 为 error.type，Gemini 与 Interactions 为 error.status
// 或字符串 error.code。Gemini 的 error.code 是与 HTTP 状态相同的数字，排在字符串之后，避免盖住 RESOURCE_EXHAUSTED 这类状态名
function errorCode(error: unknown): string {
  if (!isRecord(error)) return ''
  for (const value of [error.code, error.type, error.status]) {
    if (typeof value === 'string' && value !== '') return value
  }
  return typeof error.code === 'number' ? String(error.code) : ''
}

// responseErrorBody 提取四套公开协议共享的错误消息与错误码；声明为 JSON 却无法解析时按原文返回。
// 读错误响应体时连接中断（浏览器抛 TypeError）按状态行返回，调用方照常得到带 HTTP 状态的 ApiError，不误报服务离线；
// 用户中止（AbortError 为 DOMException）照常抛出
async function responseErrorBody(response: Response): Promise<{ message: string; code: string }> {
  let body: string
  try {
    body = await response.text()
  } catch (error) {
    if (error instanceof TypeError) return { message: response.statusText || error.message, code: '' }
    throw error
  }
  if (body === '') return { message: response.statusText, code: '' }
  if (!response.headers.get('content-type')?.includes('application/json')) {
    return { message: body, code: '' }
  }

  let value: unknown
  try {
    value = JSON.parse(body)
  } catch {
    return { message: body, code: '' }
  }
  const message = pathValue(value, ['error', 'message'])
  return {
    message: typeof message === 'string' ? message : body,
    code: errorCode(pathValue(value, ['error'])),
  }
}

// responseErrorMessage 提取四套公开协议共享的错误消息
async function responseErrorMessage(response: Response): Promise<string> {
  return (await responseErrorBody(response)).message
}

// retryAfterSeconds 读取 Retry-After 响应头的秒数；HTTP 日期等非数字写法忽略
function retryAfterSeconds(response: Response): number | undefined {
  const value = response.headers.get('retry-after')?.trim() ?? ''
  if (!/^\d+$/.test(value)) return undefined
  return Number(value)
}

// playgroundResponseError 把试用请求的非 2xx 响应转为带错误码与 Retry-After 的 ApiError
async function playgroundResponseError(response: Response): Promise<ApiError> {
  const { message, code } = await responseErrorBody(response)
  const retryAfter = retryAfterSeconds(response)
  return new ApiError(message, response.status, {
    code,
    ...(retryAfter === undefined ? {} : { retryAfter }),
  })
}

// requestJSON 执行管理端 JSON 请求并保留服务端错误语义
async function requestJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  if (init?.body !== undefined) {
    headers.set('Content-Type', 'application/json')
  }

  const response = await fetch(path, { ...init, headers })
  if (!response.ok) {
    throw new ApiError(await responseErrorMessage(response), response.status)
  }

  return (await response.json()) as T
}

// requestCommand 执行无需响应体的管理写操作
async function requestCommand(path: string, init: RequestInit): Promise<void> {
  const headers = new Headers(init.headers)
  if (init.body !== undefined) {
    headers.set('Content-Type', 'application/json')
  }

  const response = await fetch(path, { ...init, headers })
  if (!response.ok) {
    throw new ApiError(await responseErrorMessage(response), response.status)
  }
}

// parseAdminEvent 校验单一事件流的事件外壳
function parseAdminEvent(raw: string): AdminEvent | undefined {
  const value: unknown = JSON.parse(raw)
  if (typeof value !== 'object' || value === null || !('type' in value) || !('data' in value)) {
    return undefined
  }

  const type = value.type
  if (
    type !== 'status' &&
    type !== 'log' &&
    type !== 'accounts' &&
    type !== 'models' &&
    type !== 'cooldowns' &&
    type !== 'request'
  ) {
    return undefined
  }

  return value as AdminEvent
}

export const api = {
  status: () => requestJSON<ServiceStatus>('/api/status'),
  accounts: async () => (await requestJSON<AccountsResponse>('/api/accounts')).accounts,
  models: async () => (await requestJSON<ModelsResponse>('/api/models')).models,
  cooldowns: async () => (await requestJSON<CooldownsResponse>('/api/cooldowns')).cooldowns,
  permissionDenied: async () =>
    (await requestJSON<PermissionDeniedResponse>('/api/accounts/permission-denied')).accounts ?? [],
  // accountProblemLists 一次读取 403 无权限账户与所属组织停用了 AI Studio 的账户（同一个接口）
  accountProblemLists: async (): Promise<AccountProblemLists> => {
    const body = await requestJSON<PermissionDeniedResponse>('/api/accounts/permission-denied')
    return { permissionDenied: body.accounts ?? [], orgDisabled: body.org_disabled ?? [] }
  },
  clearPermissionDenied: (input: { account_ids?: string[]; all?: boolean }) =>
    requestJSON<PermissionDeniedClearResponse>('/api/accounts/permission-denied/clear', {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  // clearOrgDisabled 清除组织停用账户的状态：立即恢复调度（仍是组织停用时会重新列出）
  clearOrgDisabled: (input: { account_ids?: string[]; all?: boolean }) =>
    requestJSON<PermissionDeniedClearResponse>('/api/accounts/org-disabled/clear', {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  requests: async () => (await requestJSON<RequestsResponse>('/api/requests')).requests,
  config: () => requestJSON<ServiceConfig>('/api/config'),
  createAccount: (input: AccountLoginInput) =>
    requestJSON<AccountResponse>('/api/accounts', {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  chromeImportProfiles: async () =>
    (await requestJSON<ChromeProfilesResponse>('/api/accounts/import/chrome')).profiles,
  importChromeAccounts: (input: ChromeImportInput) =>
    requestJSON<AccountsResponse>('/api/accounts/import/chrome', {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  updateAccount: (id: string, draft: AccountDraft) =>
    requestCommand(`/api/accounts/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify(draft),
    }),
  // enableAccounts 一次请求启用一批账户（服务端每次最多 1000 个）
  enableAccounts: (input: BulkEnableInput) =>
    requestJSON<BulkEnableResult>('/api/accounts/bulk-enable', {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  deleteAccount: (id: string) =>
    requestCommand(`/api/accounts/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  // bulkDeleteAccounts 一次请求删除一批账户及其认证目录（服务端每次最多 500 个）
  bulkDeleteAccounts: (ids: string[]) =>
    requestJSON<BulkDeleteResult>('/api/accounts/bulk-delete', {
      method: 'POST',
      body: JSON.stringify({ account_ids: ids }),
    }),
  loginAccount: (id: string) =>
    requestCommand(`/api/accounts/${encodeURIComponent(id)}/login`, { method: 'POST' }),
  // verifyAccount 返回带验证结论的账户（authenticated 与 verify_reason）：HTTP 200 只表示验证做完了，不表示登录有效。
  // 服务端返回 {"account": …}，取出里面的账户（原先把外壳当成账户，authenticated 永远读不到）
  verifyAccount: async (id: string) =>
    (await requestJSON<AccountResponse>(`/api/accounts/${encodeURIComponent(id)}/verify`, { method: 'POST' })).account,
  startService: () => requestJSON<ServiceStatus>('/api/control/start', { method: 'POST' }),
  stopService: () => requestJSON<ServiceStatus>('/api/control/stop', { method: 'POST' }),
  restartService: () => requestJSON<ServiceStatus>('/api/control/restart', { method: 'POST' }),
  // egressAction 撤销（withdraw，30 分钟内不再判定）或恢复（resume）出口限流判定，返回最新状态；只改服务端内存，不落盘
  egressAction: (id: string, action: 'withdraw' | 'resume') =>
    requestJSON<ServiceStatus>(`/api/egress/${encodeURIComponent(id)}/${action}`, { method: 'POST' }),
  onboarding: () => requestJSON<OnboardingStatus>('/api/onboarding'),
  queueDisabledOnboarding: () =>
    requestJSON<OnboardingStatus>('/api/onboarding/queue-disabled', { method: 'POST' }),
  updateOnboarding: (policy: OnboardingPolicy) =>
    requestJSON<OnboardingStatus>('/api/onboarding', {
      method: 'PUT',
      body: JSON.stringify(policy),
    }),
  clearLogs: () => requestCommand('/api/logs', { method: 'DELETE' }),
  saveConfig: (config: ServiceConfig) =>
    requestJSON<ServiceConfig>('/api/config', {
      method: 'PUT',
      body: JSON.stringify(config),
    }),
  // testProxy 测试代理的出口 IP；proxy 为空时测试服务配置的默认代理
  testProxy: (proxy: string) =>
    requestJSON<ProxyTestResult>('/api/proxy/test', {
      method: 'POST',
      body: JSON.stringify({ proxy }),
    }),
  // testProxyPool 抽测 IP 池的出口 IP：用输入框里当前的设置（不必先保存），静态抽首、中、尾等端口，动态用临时会话号；
  // samples 为抽测的出口数（服务端默认 3，最多 10）
  testProxyPool: (pool: ProxyPoolConfig, samples: number) =>
    requestJSON<ProxyTestResult>('/api/proxy/test', {
      method: 'POST',
      body: JSON.stringify({ pool, samples }),
    }),
  // testAccountProxy 测试账户实际使用的出口（账户代理、IP 池分配或默认代理），结果的 slot 为端口号或会话号
  testAccountProxy: (accountID: string) =>
    requestJSON<ProxyTestResult>('/api/proxy/test', {
      method: 'POST',
      body: JSON.stringify({ account_id: accountID }),
    }),
  // checkProxyPool 在后台逐个检查静态 IP 端口段里每个端口的出口 IP（返回 202，已在进行时 409）；pool 省略时用当前生效的设置。
  // 进度与结果在状态接口的 proxy_pool.check
  checkProxyPool: (pool?: ProxyPoolConfig) =>
    requestCommand('/api/proxy/pool/check', {
      method: 'POST',
      body: JSON.stringify(pool === undefined ? {} : { pool }),
    }),
  // followDefaultProxy 把账户代理与默认代理相同的账户改为跟随默认代理（清空账户代理），之后它们按 IP 池出网；
  // 运行中的 Worker 不重启，关闭后才改用 IP 池
  followDefaultProxy: () =>
    requestJSON<FollowDefaultProxyResult>('/api/accounts/proxy/follow-default', {
      method: 'POST',
    }),
  cancelRequest: (id: string) =>
    requestCommand(`/api/requests/${encodeURIComponent(id)}/cancel`, {
      method: 'POST',
    }),
  usage: (query: URLSearchParams, signal?: AbortSignal) =>
    requestJSON<UsageReport>(`/api/usage?${query.toString()}`, { signal: signal ?? null }),
  usageRecords: (query: URLSearchParams, signal?: AbortSignal) =>
    requestJSON<UsageRecordPage>(`/api/usage/records?${query.toString()}`, {
      signal: signal ?? null,
    }),
  // usageExportURL 返回 CSV 下载地址：同源链接自动携带管理令牌 Cookie
  usageExportURL: (query: URLSearchParams) => `/api/usage/records.csv?${query.toString()}`,
  requestBody: (id: string) =>
    requestJSON<RequestBody>(`/api/requests/${encodeURIComponent(id)}/body`),
}

// openAdminEvents 建立唯一的管理状态 SSE 连接
// EventsState 为管理事件流的连接状态：closed 表示浏览器不再自动重连（例如返回 401/403）
export type EventsState = 'open' | 'reconnecting' | 'closed'

export function openAdminEvents(
  onEvent: (event: AdminEvent) => void,
  onOpen: () => void,
  onState: (state: EventsState) => void = () => {},
): EventConnection {
  const source = new EventSource('/api/events')
  source.onopen = () => {
    onState('open')
    onOpen()
  }
  source.onerror = () => {
    onState(source.readyState === EventSource.CLOSED ? 'closed' : 'reconnecting')
  }
  source.onmessage = (message) => {
    const event = parseAdminEvent(message.data)
    if (event !== undefined) {
      onEvent(event)
    }
  }

  return {
    close: () => source.close(),
  }
}

interface PlaygroundRequest {
  path: string
  headers: Headers
  body: string
  responseType: 'json' | 'audio'
}

// PlaygroundChunk 为一次事件或完整响应解析出的内容；finish 为其中的结束原因（各协议原文），没有时为空
export interface PlaygroundChunk {
  text: string
  reasoning: string
  tools: string
  media: PlaygroundMedia[]
  finish: string
}

function toolPayload(input: PlaygroundInput): unknown[] | undefined {
  if (input.tool === '') return undefined

  if (input.protocol === 'gemini') {
    const names: Record<Exclude<PlaygroundInput['tool'], ''>, string> = {
      web_search: 'googleSearch',
      image_search: 'imageSearch',
      code_interpreter: 'codeExecution',
      url_context: 'urlContext',
      google_maps: 'googleMaps',
    }
    return [{ [names[input.tool]]: {} }]
  }

  if (input.protocol === 'anthropic') {
    const definitions: Record<
      Exclude<PlaygroundInput['tool'], ''>,
      { type: string; name: string }
    > = {
      web_search: { type: 'web_search_20250305', name: 'web_search' },
      image_search: { type: 'image_search', name: 'image_search' },
      code_interpreter: {
        type: 'code_execution_20250825',
        name: 'code_execution',
      },
      url_context: { type: 'web_fetch_20250910', name: 'web_fetch' },
      google_maps: { type: 'google_maps', name: 'google_maps' },
    }
    return [definitions[input.tool]]
  }

  return [{ type: input.tool }]
}

// playgroundPrefix 返回试用请求的路径前缀：使用 Ultra 账户时经 /ultra 发送，只由 Ultra 号池的账户处理
export function playgroundPrefix(input: Pick<PlaygroundInput, 'ultra'>): string {
  return input.ultra ? '/ultra' : ''
}

// buildPlaygroundRequest 将输出类型和协议映射为公开 API 请求（路径不含号池前缀）
function buildPlaygroundRequest(input: PlaygroundInput): PlaygroundRequest {
  const headers = new Headers({ 'Content-Type': 'application/json' })
  if (input.apiKey !== '') {
    headers.set('Authorization', `Bearer ${input.apiKey}`)
  }

  if (input.mode === 'video') {
    return {
      path: '/v1/videos',
      headers,
      responseType: 'json',
      body: JSON.stringify({ model: input.model, prompt: input.prompt }),
    }
  }

  if (input.mode === 'image') {
    return {
      path: '/v1/images/generations',
      headers,
      responseType: 'json',
      body: JSON.stringify({
        model: input.model,
        prompt: input.prompt,
        n: 1,
        size: input.imageSize,
        quality: input.imageQuality,
      }),
    }
  }

  if (input.mode === 'speech') {
    return {
      path: '/v1/audio/speech',
      headers,
      responseType: 'audio',
      body: JSON.stringify({
        model: input.model,
        input: input.prompt,
        instructions: input.system || undefined,
        voice: input.voice || undefined,
        response_format: 'wav',
      }),
    }
  }

  if (input.mode === 'music') {
    return {
      path: `/v1beta/models/${encodeURIComponent(input.model)}:generateContent`,
      headers,
      responseType: 'json',
      body: JSON.stringify({
        contents: [{ role: 'user', parts: [{ text: input.prompt }] }],
        generationConfig: { responseModalities: ['AUDIO'] },
      }),
    }
  }

  const tools = toolPayload(input)

  if (input.protocol === 'openai-chat') {
    const messages = input.system
      ? [
          { role: 'system', content: input.system },
          { role: 'user', content: input.prompt },
        ]
      : [{ role: 'user', content: input.prompt }]
    return {
      path: '/v1/chat/completions',
      headers,
      responseType: 'json',
      body: JSON.stringify({
        model: input.model,
        messages,
        stream: input.stream,
        reasoning_effort: input.reasoning || undefined,
        tools,
      }),
    }
  }

  if (input.protocol === 'openai-responses') {
    return {
      path: '/v1/responses',
      headers,
      responseType: 'json',
      body: JSON.stringify({
        model: input.model,
        input: input.prompt,
        instructions: input.system || undefined,
        stream: input.stream,
        reasoning: input.reasoning ? { effort: input.reasoning } : undefined,
        tools,
      }),
    }
  }

  if (input.protocol === 'anthropic') {
    headers.set('anthropic-version', '2023-06-01')
    if (input.apiKey !== '') {
      headers.set('x-api-key', input.apiKey)
    }
    return {
      path: '/v1/messages',
      headers,
      responseType: 'json',
      body: JSON.stringify({
        model: input.model,
        max_tokens: 1024,
        messages: [{ role: 'user', content: input.prompt }],
        system: input.system || undefined,
        stream: input.stream,
        output_config: input.reasoning ? { effort: input.reasoning } : undefined,
        tools,
      }),
    }
  }

  // 流式必须带 alt=sse：不带时服务端与官方一致返回逐块写出的 JSON 数组，事件流解析器读不出任何事件
  const suffix = input.stream ? 'streamGenerateContent?alt=sse' : 'generateContent'
  return {
    path: `/v1beta/models/${encodeURIComponent(input.model)}:${suffix}`,
    headers,
    responseType: 'json',
    body: JSON.stringify({
      contents: [{ role: 'user', parts: [{ text: input.prompt }] }],
      systemInstruction: input.system
        ? { role: 'system', parts: [{ text: input.system }] }
        : undefined,
      generationConfig: input.reasoning
        ? { thinkingConfig: { thinkingLevel: input.reasoning } }
        : undefined,
      tools,
    }),
  }
}

// isRecord 收窄公开协议响应中的动态对象
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

// pathValue 读取公开协议中已知的嵌套字段
function pathValue(value: unknown, path: readonly (string | number)[]): unknown {
  let current = value
  for (const part of path) {
    if (typeof part === 'number') {
      if (!Array.isArray(current)) return undefined
      current = current[part]
      continue
    }
    if (!isRecord(current)) return undefined
    current = current[part]
  }
  return current
}

interface VideoState {
  id: string
  status: 'queued' | 'completed' | 'failed'
}

function videoState(value: unknown): VideoState {
  const id = isRecord(value) && typeof value.id === 'string' ? value.id : ''
  const status = isRecord(value) && typeof value.status === 'string' ? value.status : ''
  if (id === '' || (status !== 'queued' && status !== 'completed' && status !== 'failed')) {
    throw new Error('Invalid video response')
  }
  return { id, status }
}

function waitForVideoPoll(signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(new DOMException('Aborted', 'AbortError'))
      return
    }
    const onAbort = () => {
      window.clearTimeout(timer)
      reject(new DOMException('Aborted', 'AbortError'))
    }
    const timer = window.setTimeout(() => {
      signal.removeEventListener('abort', onAbort)
      resolve()
    }, 2000)
    signal.addEventListener('abort', onAbort, { once: true })
  })
}

// completeVideo 轮询视频任务并下载结果；prefix 为创建任务时的号池前缀，任务只能在同一号池查询。
// 各响应体都经 readResponseBody 读取：读到一半连接中断（下载视频时最常见）按已连接后失败显示，不误报服务离线
async function completeVideo(
  response: Response,
  headers: Headers,
  signal: AbortSignal,
  prefix: string,
): Promise<{ status: number; chunk: PlaygroundChunk; raw: string }> {
  let value: unknown = await readResponseBody(response, () => response.json())
  let state = videoState(value)
  while (state.status === 'queued') {
    await waitForVideoPoll(signal)
    const poll = await fetch(`${prefix}/v1/videos/${encodeURIComponent(state.id)}`, {
      headers,
      signal,
    })
    if (!poll.ok) {
      throw await playgroundResponseError(poll)
    }
    value = await readResponseBody(poll, () => poll.json())
    state = videoState(value)
  }
  if (state.status === 'failed') {
    throw new ApiError('Veo generation failed', response.status)
  }
  const content = await fetch(`${prefix}/v1/videos/${encodeURIComponent(state.id)}/content`, {
    headers,
    signal,
  })
  if (!content.ok) {
    throw await playgroundResponseError(content)
  }
  const blob = await readResponseBody(content, () => content.blob())
  return {
    status: content.status,
    chunk: {
      ...emptyChunk(),
      media: [{ mime: blob.type || 'video/mp4', url: URL.createObjectURL(blob) }],
    },
    raw: JSON.stringify(value, null, 2),
  }
}

function emptyChunk(): PlaygroundChunk {
  return { text: '', reasoning: '', tools: '', media: [], finish: '' }
}

function mediaFrom(value: unknown): PlaygroundMedia | undefined {
  if (!isRecord(value)) return undefined
  const mime = typeof value.mimeType === 'string' ? value.mimeType : ''
  const data = typeof value.data === 'string' ? value.data : ''
  const url = typeof value.fileUri === 'string' ? value.fileUri : ''
  if (mime === '' || (data === '' && url === '')) return undefined
  return { mime, url: data === '' ? url : `data:${mime};base64,${data}` }
}

function toolLine(value: unknown): string {
  if (!isRecord(value)) return ''
  const name = typeof value.name === 'string' ? value.name : ''
  const argumentsValue = value.arguments ?? value.args ?? value.input
  const argumentsText =
    typeof argumentsValue === 'string' ? argumentsValue : JSON.stringify(argumentsValue ?? {})
  return name === '' ? argumentsText : `${name} ${argumentsText}`
}

// geminiFinish 读取 Gemini 的结束原因：有 finishMessage 时拼在后面；没有候选时为输入被拦截的 blockReason
function geminiFinish(value: unknown): string {
  const candidates = pathValue(value, ['candidates'])
  if (!Array.isArray(candidates) || candidates.length === 0) {
    const blocked = pathValue(value, ['promptFeedback', 'blockReason'])
    return typeof blocked === 'string' ? blocked : ''
  }
  const reason = pathValue(candidates, [0, 'finishReason'])
  if (typeof reason !== 'string' || reason === '') return ''
  const message = pathValue(candidates, [0, 'finishMessage'])
  return typeof message === 'string' && message !== '' ? `${reason}: ${message}` : reason
}

function geminiChunk(value: unknown): PlaygroundChunk {
  const chunk = emptyChunk()
  chunk.finish = geminiFinish(value)
  const parts = pathValue(value, ['candidates', 0, 'content', 'parts'])
  if (!Array.isArray(parts)) return chunk
  for (const part of parts) {
    if (!isRecord(part)) continue
    if (typeof part.text === 'string') {
      if (part.thought === true) chunk.reasoning += part.text
      else chunk.text += part.text
    }
    if (part.functionCall !== undefined) chunk.tools += `${toolLine(part.functionCall)}\n`
    if (part.executableCode !== undefined) {
      chunk.tools += `${JSON.stringify(part.executableCode, null, 2)}\n`
    }
    if (part.codeExecutionResult !== undefined) {
      chunk.tools += `${JSON.stringify(part.codeExecutionResult, null, 2)}\n`
    }
    const media = mediaFrom(part.inlineData ?? part.fileData)
    if (media !== undefined) chunk.media.push(media)
  }
  return chunk
}

function chatChunk(value: unknown): PlaygroundChunk {
  const chunk = emptyChunk()
  const finish = pathValue(value, ['choices', 0, 'finish_reason'])
  if (typeof finish === 'string') chunk.finish = finish
  const message =
    pathValue(value, ['choices', 0, 'delta']) ?? pathValue(value, ['choices', 0, 'message'])
  if (!isRecord(message)) return chunk
  if (typeof message.content === 'string') chunk.text = message.content
  if (typeof message.reasoning_content === 'string') chunk.reasoning = message.reasoning_content
  if (Array.isArray(message.tool_calls)) {
    for (const call of message.tool_calls) {
      const value = pathValue(call, ['function'])
      chunk.tools += `${toolLine(value)}\n`
    }
  }
  return chunk
}

// responsesFinish 读取 Responses 的结束原因：status 为 incomplete 时取 incomplete_details.reason，否则为 status
function responsesFinish(response: unknown): string {
  const status = pathValue(response, ['status'])
  if (status === 'incomplete') {
    const reason = pathValue(response, ['incomplete_details', 'reason'])
    if (typeof reason === 'string' && reason !== '') return reason
  }
  return typeof status === 'string' ? status : ''
}

function responsesChunk(value: unknown): PlaygroundChunk {
  const chunk = emptyChunk()
  const type = isRecord(value) && typeof value.type === 'string' ? value.type : ''
  const delta = isRecord(value) && typeof value.delta === 'string' ? value.delta : ''
  if (type === 'response.output_text.delta') chunk.text = delta
  if (type === 'response.reasoning_summary_text.delta') chunk.reasoning = delta
  if (type.includes('function_call') && delta !== '') chunk.tools = delta
  if (type === 'response.output_item.added') {
    const item = pathValue(value, ['item'])
    if (isRecord(item) && item.type === 'function_call') chunk.tools = `${toolLine(item)}\n`
  }
  const response = isRecord(value) && isRecord(value.response) ? value.response : value
  // 结束原因只看完整响应与完成事件：response.created 等中间事件的 status 为 in_progress
  if (type === '' || type === 'response.completed' || type === 'response.incomplete') {
    chunk.finish = responsesFinish(response)
  }
  if (type !== '') return chunk
  if (!isRecord(response)) return chunk
  // output_text 是 output 中全部消息正文的合并，两者都累加会把正文显示两遍；只在没有 output 时使用
  if (!Array.isArray(response.output)) {
    if (typeof response.output_text === 'string') chunk.text = response.output_text
    return chunk
  }
  for (const item of response.output) {
    if (!isRecord(item)) continue
    if (item.type === 'reasoning' && Array.isArray(item.summary)) {
      for (const summary of item.summary) {
        const text = pathValue(summary, ['text'])
        if (typeof text === 'string') chunk.reasoning += text
      }
    }
    if (item.type === 'message' && Array.isArray(item.content)) {
      for (const content of item.content) {
        const text = pathValue(content, ['text'])
        if (typeof text === 'string') chunk.text += text
      }
    }
    if (item.type === 'function_call') chunk.tools += `${toolLine(item)}\n`
    if (item.type === 'image_generation_call' && typeof item.result === 'string') {
      chunk.media.push({ mime: 'image/png', url: `data:image/png;base64,${item.result}` })
    }
  }
  return chunk
}

function anthropicChunk(value: unknown): PlaygroundChunk {
  const chunk = emptyChunk()
  const deltaText = pathValue(value, ['delta', 'text'])
  const thinking = pathValue(value, ['delta', 'thinking'])
  const partialJSON = pathValue(value, ['delta', 'partial_json'])
  if (typeof deltaText === 'string') chunk.text = deltaText
  if (typeof thinking === 'string') chunk.reasoning = thinking
  if (typeof partialJSON === 'string') chunk.tools = partialJSON
  // 流式的结束原因在 message_delta 的 delta.stop_reason，非流式在顶层 stop_reason
  const stopReason = pathValue(value, ['delta', 'stop_reason']) ?? pathValue(value, ['stop_reason'])
  if (typeof stopReason === 'string') chunk.finish = stopReason
  const blocks = isRecord(value) && Array.isArray(value.content) ? value.content : []
  for (const block of blocks) {
    if (!isRecord(block)) continue
    if (block.type === 'text' && typeof block.text === 'string') chunk.text += block.text
    if (block.type === 'thinking' && typeof block.thinking === 'string') {
      chunk.reasoning += block.thinking
    }
    if (block.type === 'tool_use') chunk.tools += `${toolLine(block)}\n`
  }
  const startBlock = pathValue(value, ['content_block'])
  if (isRecord(startBlock) && startBlock.type === 'tool_use') {
    chunk.tools += `${toolLine(startBlock)}\n`
  }
  return chunk
}

function imageChunk(value: unknown): PlaygroundChunk {
  const chunk = emptyChunk()
  const data = isRecord(value) && Array.isArray(value.data) ? value.data : []
  for (const item of data) {
    if (!isRecord(item)) continue
    if (typeof item.revised_prompt === 'string') chunk.text += item.revised_prompt
    if (typeof item.url === 'string') {
      const mime = /^data:([^;,]+)/.exec(item.url)?.[1] ?? 'image/png'
      chunk.media.push({ mime, url: item.url })
    }
    if (typeof item.b64_json === 'string') {
      chunk.media.push({ mime: 'image/png', url: `data:image/png;base64,${item.b64_json}` })
    }
  }
  return chunk
}

// responseChunk 提取公开协议的正文、思考、工具与媒体结果
function responseChunk(input: PlaygroundInput, value: unknown): PlaygroundChunk {
  if (input.mode === 'image') return imageChunk(value)
  if (input.mode === 'music' || input.protocol === 'gemini') return geminiChunk(value)
  if (input.protocol === 'openai-chat') return chatChunk(value)
  if (input.protocol === 'openai-responses') return responsesChunk(value)
  return anthropicChunk(value)
}

// throwStreamError 检查流式事件中的错误（含 Responses 的 response.failed）：流已建立后返回的错误标为 inStream
function throwStreamError(value: unknown, raw: string, status: number): void {
  const error = pathValue(value, ['error']) ?? pathValue(value, ['response', 'error'])
  if (error == null) return
  const message = pathValue(error, ['message'])
  throw new ApiError(typeof message === 'string' ? message : raw, status, {
    code: errorCode(error),
    inStream: true,
  })
}

// readEventStream 逐帧读取公开协议流式响应
async function readEventStream(
  response: Response,
  input: PlaygroundInput,
  onDelta: (chunk: PlaygroundChunk, raw: string) => void,
): Promise<void> {
  if (response.body === null) throw new StreamIncompleteError('Empty event stream', response.status)

  const events = response.body
    .pipeThrough(new TextDecoderStream())
    .pipeThrough(new EventSourceParserStream())
  const finished = await readResponseBody(response, async () => {
    for await (const { data } of events) {
      if (input.protocol === 'openai-chat' && data === '[DONE]') return true
      const value: unknown = JSON.parse(data)
      throwStreamError(value, data, response.status)
      onDelta(responseChunk(input, value), data)
      if (streamFinished(input, value)) return true
    }
    return false
  })
  if (finished) return
  throw new StreamIncompleteError(
    'Event stream ended before the protocol completion event',
    response.status,
  )
}

// readJSONStream 兜底读取以 JSON 返回的流式响应（Gemini 不带 alt=sse 时为 JSON 数组）：数组逐项、对象按一项交给 onDelta
async function readJSONStream(
  response: Response,
  input: PlaygroundInput,
  onDelta: (chunk: PlaygroundChunk, raw: string) => void,
): Promise<void> {
  const value: unknown = await readResponseBody(response, () => response.json())
  for (const item of Array.isArray(value) ? value : [value]) {
    const raw = JSON.stringify(item)
    throwStreamError(item, raw, response.status)
    onDelta(responseChunk(input, item), raw)
  }
}

// streamFinished 按各公开协议的结束事件确认请求完成
function streamFinished(input: PlaygroundInput, value: unknown): boolean {
  const type = pathValue(value, ['type'])
  switch (input.protocol) {
    case 'openai-chat':
      return false
    case 'openai-responses':
      return type === 'response.completed' || type === 'response.incomplete'
    case 'anthropic':
      return type === 'message_stop'
    case 'gemini': {
      const reason = pathValue(value, ['candidates', 0, 'finishReason'])
      return typeof reason === 'string' && reason !== ''
    }
  }
}

// runPlayground 执行一次公开协议试用并返回取消控制器
export async function runPlayground(
  input: PlaygroundInput,
  signal: AbortSignal,
  onDelta: (chunk: PlaygroundChunk, raw: string) => void,
): Promise<{ status: number; chunk?: PlaygroundChunk; raw?: string }> {
  let request: PlaygroundRequest
  try {
    request = buildPlaygroundRequest(input)
  } catch (error) {
    // 构造请求头失败（如 API Key 含非 Latin-1 字符）同样抛 TypeError：改为普通错误，界面显示浏览器给出的原因而不是服务离线
    if (error instanceof TypeError) throw new Error(error.message, { cause: error })
    throw error
  }
  const prefix = playgroundPrefix(input)
  const response = await fetch(`${prefix}${request.path}`, {
    method: 'POST',
    headers: request.headers,
    body: request.body,
    signal,
  })

  if (!response.ok) {
    throw await playgroundResponseError(response)
  }

  if (input.mode === 'video') {
    return completeVideo(response, request.headers, signal, prefix)
  }

  if (request.responseType === 'audio') {
    const blob = await readResponseBody(response, () => response.blob())
    return {
      status: response.status,
      chunk: {
        ...emptyChunk(),
        media: [{ mime: blob.type || 'audio/wav', url: URL.createObjectURL(blob) }],
      },
      raw: JSON.stringify({ type: blob.type || 'audio/wav', size: blob.size }, null, 2),
    }
  }

  if (input.mode === 'text' && input.stream) {
    if (response.headers.get('content-type')?.includes('application/json')) {
      await readJSONStream(response, input, onDelta)
    } else {
      await readEventStream(response, input, onDelta)
    }
    return { status: response.status }
  }

  const value: unknown = await readResponseBody(response, () => response.json())
  return {
    status: response.status,
    chunk: responseChunk(input, value),
    raw: JSON.stringify(value, null, 2),
  }
}
