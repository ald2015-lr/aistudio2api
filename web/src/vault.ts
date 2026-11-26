import { computed, onBeforeUnmount, onMounted, reactive, ref, shallowRef } from 'vue'
import { ApiError, api } from '@/api'
import { trackPendingWrite } from '@/assetRecovery'
import { copyText } from '@/clipboard'
import { useI18n, type TranslationKey } from '@/i18n'
import type {
  Account,
  FamilyGroup,
  FamilyImportResult,
  ProfileImportResult,
  ProfileImportSource,
  TabID,
  VaultBackupInfo,
  VaultFileError,
  VaultLimits,
  VaultProfile,
} from '@/types'

// 账号资料（密码、辅助邮箱、2FA、备注）与家庭组的前端数据：资料页与账户页共用同一份。
// 约定：
// - 数据只经 GET /api/vault 拉取（不走 SSE）：页面挂载时、60 秒轮询（有页面在用且窗口可见时）、窗口回到前台、每次写操作之后；
//   带 since，没变时服务端只回 unchanged
// - profiles、families 整体替换（shallowRef + Object.freeze），索引在 computed 里一次 O(n) 建 Map，组件里只做 O(1) 查找
// - 打码视图之外的明文（取出、导出的结果）不进这里：复制按钮拿到后立即复制、丢弃；手动复制框与“显示 30 秒”关掉即清空
// - 导入草稿（粘贴的文本、选的文件、预览）只在这个模块的内存里，不写本地存储；导入成功后清空

export const PALETTE_SIZE = 12
// VAULT_POLL_MS 为有页面在用时的轮询间隔
const VAULT_POLL_MS = 60_000
// REQUEST_BUDGET 为发送前按 JSON 转义后计的请求体上限（/api/ 的 8 MiB 减 64 KiB 余量）
export const REQUEST_BUDGET = 8 * 1024 * 1024 - 64 * 1024

// DEFAULT_LIMITS 为第一次拉取之前用的上限（与服务端常量相同）
export const DEFAULT_LIMITS: VaultLimits = {
  import_lines: 20000,
  import_bytes: 4 << 20,
  import_sources: 50,
  line_bytes: 4096,
  password_bytes: 512,
  note_bytes: 2048,
  profiles: 50000,
  groups: 20000,
  group_emails: 20,
  emails_per_request: 20000,
  request_bytes: 8 << 20,
  snapshots: 10,
}

// famClass 返回家庭组颜色的类名（fam-c0…fam-c11，定义在 style.css）；颜色只经类名取，不在组件里写色值
export function famClass(color: number | undefined): string {
  const index = Number.isInteger(color)
    ? (((color ?? 0) % PALETTE_SIZE) + PALETTE_SIZE) % PALETTE_SIZE
    : 0
  return `fam-c${index}`
}

// normEmail 为关联用的邮箱：去首尾空白、转小写（与服务端存的规范化邮箱一致；账户的 label 都是合法邮箱）
export function normEmail(value: string): string {
  return value.trim().toLowerCase()
}

// accountEmail 为账户关联资料与家庭组用的邮箱（账户 label；没有时用 ID）
export function accountEmail(account: Pick<Account, 'id' | 'label'>): string {
  return normEmail(account.label || account.id)
}

export interface VaultSnapshot {
  revision: string
  dir: string
  persistent: boolean
  profiles: readonly VaultProfile[]
  families: readonly FamilyGroup[]
  errors: readonly VaultFileError[]
  backups: readonly VaultBackupInfo[]
  limits: VaultLimits
}

// vaultData 为最近一次拉取到的资料与家庭组（还没拉取到时为 null）
export const vaultData = shallowRef<VaultSnapshot | null>(null)
// vaultStatus：loading 为第一次拉取进行中；error 为最近一次拉取失败的说明（成功后清空，旧数据照常显示）
export const vaultStatus = reactive({ loading: false, error: '' })

export interface FamilyMembership {
  group: FamilyGroup
  role: 'primary' | 'member'
  // position 为在组里的位置：主邮箱 0，成员按录入顺序从 1 起
  position: number
}

export const profileByEmail = computed(() => {
  const map = new Map<string, VaultProfile>()
  for (const profile of vaultData.value?.profiles ?? []) map.set(profile.email, profile)
  return map
})

export const familyByEmail = computed(() => {
  const map = new Map<string, FamilyMembership>()
  for (const group of vaultData.value?.families ?? []) {
    map.set(group.primary, { group, role: 'primary', position: 0 })
    groupMembers(group).forEach((email, index) => {
      map.set(email, { group, role: 'member', position: index + 1 })
    })
  }
  return map
})

export const groupById = computed(() => {
  const map = new Map<string, FamilyGroup>()
  for (const group of vaultData.value?.families ?? []) map.set(group.id, group)
  return map
})

// familiesBySeq 为按创建顺序排列的家庭组（服务端已按 seq 排序，这里再排一次保证顺序）
export const familiesBySeq = computed(() =>
  [...(vaultData.value?.families ?? [])].sort((left, right) => left.seq - right.seq),
)

// fileError 为某个文件的错误状态（没有时为 undefined）
export function fileError(file: 'profiles' | 'families'): VaultFileError | undefined {
  return vaultData.value?.errors.find((entry) => entry.file === file)
}

// groupMembers 为组的成员（人工编辑文件造出的空成员组为空数组）
export function groupMembers(group: FamilyGroup): readonly string[] {
  return group.members ?? []
}

// groupEmails 为组里的全部邮箱：主邮箱在前
export function groupEmails(group: FamilyGroup): string[] {
  return [group.primary, ...groupMembers(group)]
}

// groupSize 为组的人数（主邮箱 + 成员）
export function groupSize(group: FamilyGroup): number {
  return 1 + groupMembers(group).length
}

// useAccountIndex 按关联邮箱索引账户：一个邮箱可能对应多个账户行（两个目录的 label 规范化后相同）；emailOf 为账户 ID → 关联邮箱
export function useAccountIndex(accounts: () => readonly Account[]) {
  return computed(() => {
    const byEmail = new Map<string, Account[]>()
    const emailOf = new Map<string, string>()
    for (const account of accounts()) {
      const email = accountEmail(account)
      emailOf.set(account.id, email)
      const list = byEmail.get(email)
      if (list === undefined) byEmail.set(email, [account])
      else list.push(account)
    }
    return { byEmail, emailOf }
  })
}

// ---------- 拉取与轮询 ----------

let inflight: Promise<void> | null = null
let again = false
let users = 0
let pollTimer: number | undefined

async function fetchOnce(): Promise<void> {
  const { errorText } = useI18n()
  const current = vaultData.value
  if (current === null) vaultStatus.loading = true
  try {
    const view = await api.vault(current?.revision ?? '')
    if (view.unchanged !== true || current === null) {
      vaultData.value = Object.freeze({
        revision: view.revision,
        dir: view.dir ?? '',
        persistent: view.persistent ?? false,
        profiles: Object.freeze(view.profiles ?? []),
        families: Object.freeze(view.families ?? []),
        errors: Object.freeze(view.errors ?? []),
        backups: Object.freeze(view.backups ?? []),
        limits: view.limits ?? DEFAULT_LIMITS,
      })
    }
    vaultStatus.error = ''
  } catch (error) {
    vaultStatus.error = vaultErrorText(error, errorText)
  } finally {
    vaultStatus.loading = false
  }
}

// refreshVault 重新拉取：已有请求在途时并入它，并在它结束后再拉一次（写操作之后的刷新不会拿到写之前的结果）。从不抛错
export function refreshVault(): Promise<void> {
  if (inflight !== null) {
    again = true
    return inflight
  }
  inflight = (async () => {
    try {
      do {
        again = false
        await fetchOnce()
      } while (again)
    } finally {
      inflight = null
    }
  })()
  return inflight
}

function onVisibility(): void {
  if (document.visibilityState === 'visible') void refreshVault()
}

// useVaultData 在组件存续期间保持数据新鲜：挂载时拉取一次；有组件在用时每 60 秒（窗口可见时）与窗口回到前台时刷新
export function useVaultData(): void {
  onMounted(() => {
    users += 1
    if (users === 1) {
      pollTimer = window.setInterval(() => {
        if (document.visibilityState === 'visible') void refreshVault()
      }, VAULT_POLL_MS)
      document.addEventListener('visibilitychange', onVisibility)
    }
    void refreshVault()
  })
  onBeforeUnmount(() => {
    users -= 1
    if (users > 0) return
    window.clearInterval(pollTimer)
    pollTimer = undefined
    document.removeEventListener('visibilitychange', onVisibility)
  })
}

// vaultWrite 执行一次写操作（登记为在途写操作），成功后刷新数据；失败照常抛给调用方
export async function vaultWrite<T>(work: Promise<T>): Promise<T> {
  const result = await trackPendingWrite(work)
  await refreshVault()
  return result
}

// ---------- 错误文案 ----------

const VAULT_ERROR_KEYS: Record<string, TranslationKey> = {
  invalid_request: 'vault.err.invalid_request',
  vault_invalid_email: 'vault.err.vault_invalid_email',
  vault_invalid_field: 'vault.err.vault_invalid_field',
  vault_family_empty: 'vault.err.vault_family_empty',
  vault_too_large: 'vault.err.vault_too_large',
  vault_unreadable: 'vault.err.vault_unreadable',
  vault_full: 'vault.err.vault_full',
  vault_conflict: 'vault.err.vault_conflict',
  vault_suspect_unacked: 'vault.err.vault_suspect_unacked',
  vault_not_found: 'vault.err.vault_not_found',
  vault_family_not_found: 'vault.err.vault_family_not_found',
  vault_totp_missing: 'vault.err.vault_totp_missing',
  vault_totp_invalid: 'vault.err.vault_totp_invalid',
  vault_busy: 'vault.err.vault_busy',
  vault_write_failed: 'vault.err.vault_write_failed',
  vault_unsupported: 'vault.err.vault_unsupported',
}

// vaultErrorText 返回接口错误的界面文案：中文界面用服务端的固定中文说明（更具体，例如哪个字段格式不对）；
// 其他语言按错误码换成本地化文案；连不上服务时为服务离线
export function vaultErrorText(error: unknown, fallback?: (error: unknown) => string): string {
  const { t, locale, errorText } = useI18n()
  if (error instanceof ApiError && error.code !== '') {
    const key = VAULT_ERROR_KEYS[error.code]
    if (locale.value !== 'zh-CN' && key !== undefined) return t(key)
    if (error.message !== '') return error.message
    if (key !== undefined) return t(key)
  }
  return (fallback ?? errorText)(error)
}

// ---------- 跨页跳转（账户页、资料页都会在切页时卸载，靠这里的请求在挂载时消费） ----------

export type VaultSubTab = 'profiles' | 'families' | 'import' | 'familyImport'
export type ProfileListFilter = 'all' | 'exists' | 'missing' | 'totp'

export interface VaultViewRequest {
  sub: VaultSubTab
  filter?: ProfileListFilter
  // family 为资料列表按这个家庭组筛选（组编号）
  family?: string
}

export const vaultNav = reactive({
  // tab 为请求 App 切到的页签（App 切换后清空）
  tab: null as TabID | null,
  vault: null as VaultViewRequest | null,
  accounts: null as { family: string } | null,
})

export function openVaultView(request: VaultViewRequest): void {
  vaultNav.vault = request
  vaultNav.tab = 'vault'
}

export function openAccountsView(request: { family: string }): void {
  vaultNav.accounts = request
  vaultNav.tab = 'accounts'
}

// ---------- 导入草稿（只在内存） ----------

export interface ImportFile {
  id: number
  name: string
  size: number
  text: string
  lines: number
  // bytes 为解码后文本的 UTF-8 字节数（计量用，与服务端的上限同口径）
  bytes: number
  // error：not_text 为不像文本文件（二进制或编码不对），read 为读不出来；有错误的文件不参与导入
  error: '' | 'not_text' | 'read'
}

export const profileDraft = reactive({
  text: '',
  files: [] as ImportFile[],
  mode: 'merge' as 'merge' | 'fill_empty',
  ack: false,
  // version 每次改动文本、文件或导入选项时加一（见 ProfilePreview）
  version: 0,
})

// ProfilePreview 为一次预览：version 为预览时草稿的版本（文本、文件、导入选项每改一次加一，预览之后再改即作废）；
// sources 为预览时发送的来源（确认时原样再发一次，保证确认的内容就是预览的内容）
export interface ProfilePreview {
  result: ProfileImportResult
  version: number
  sources: ProfileImportSource[]
}

export const profilePreview = shallowRef<ProfilePreview | null>(null)
// profileImportDone 为最近一次确认导入的结果（结果卡用）；profileImportKept 为导入后留在输入框里的无法识别的粘贴行数
export const profileImportDone = shallowRef<ProfileImportResult | null>(null)
export const profileImportKept = ref(0)

export const familyDraft = reactive({ text: '', lines: false, ack: false, version: 0 })
// familyPreview 为一次录入预览：text、lines 为预览时的内容与开关（确认时原样再发一次），version 同 ProfilePreview
export const familyPreview = shallowRef<{
  result: FamilyImportResult
  version: number
  text: string
  lines: boolean
} | null>(null)
export const familyImportDone = shallowRef<FamilyImportResult | null>(null)

// vaultDraftDirty 为有没导入的草稿（App 登记为未保存的输入：页面文件更新后不自动刷新）
export function vaultDraftDirty(): boolean {
  return (
    profileDraft.text.trim() !== '' ||
    profileDraft.files.length > 0 ||
    familyDraft.text.trim() !== ''
  )
}

// ---------- 复制 ----------

// manualCopy 为手动复制对话框（复制失败时弹出，只读文本框里放要复制的内容；关闭即清空，2 分钟无操作自动关闭）
export const manualCopy = reactive({ open: false, text: '', title: '' })

export function openManualCopy(text: string, title = ''): void {
  Object.assign(manualCopy, { open: true, text, title })
}

export function closeManualCopy(): void {
  Object.assign(manualCopy, { open: false, text: '', title: '' })
}

// copyOrManual 复制文本：浏览器复制失败（HTTP 页面上取出等得太久时必然如此）时打开手动复制框，返回 manual
export async function copyOrManual(text: string, title = ''): Promise<'copied' | 'manual'> {
  if (await copyText(text)) return 'copied'
  openManualCopy(text, title)
  return 'manual'
}

// downloadText 把文本下载为 txt（文件名为 prefix 加本地时间到秒），1 秒后释放
export function downloadText(text: string, prefix: string): void {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }))
  const now = new Date()
  const pad = (value: number) => String(value).padStart(2, '0')
  const stamp = `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`
  const link = document.createElement('a')
  link.href = url
  link.download = `${prefix}-${stamp}.txt`
  document.body.appendChild(link)
  link.click()
  link.remove()
  window.setTimeout(() => URL.revokeObjectURL(url), 1000)
}

// ---------- 小工具 ----------

// jsonBytes 为请求体按 JSON 转义之后的字节数（/api/ 的 8 MiB 上限按这个口径）
export function jsonBytes(value: unknown): number {
  return new TextEncoder().encode(JSON.stringify(value)).length
}

// formatBytes 以 KB/MB 显示字节数
export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(bytes >= 10 * 1024 * 1024 ? 0 : 1)} MB`
}

// formatShortTime 以本地时间显示“月-日 时:分”
export function formatShortTime(value: string | undefined): string {
  if (value === undefined || value === '') return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  const pad = (part: number) => String(part).padStart(2, '0')
  return `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`
}

// lettersOnlyTOTP 判断输入的 2FA 是否是没有数字 2–7 的 Base32（编辑框里的提示，不拦）
export function lettersOnlyTOTP(value: string): boolean {
  const text = value.replace(/\s+/g, '').replace(/=+$/, '')
  if (text.length < 16 || /^otpauth:/i.test(text)) return false
  return /^[A-Za-z]+$/.test(text)
}

// hasControl 判断文本里有没有 Tab 或控制字符（资料的字段都不能有，导入时 Tab 是分隔符）
export function hasControl(value: string, allowNewline = false): boolean {
  for (const char of value) {
    const code = char.codePointAt(0) ?? 0
    if (allowNewline && (code === 10 || code === 13)) continue
    if (code < 32 || code === 127 || code === 0x2028 || code === 0x2029) return true
  }
  return false
}

// ---------- 家庭组筛选（账户页与资料页共用） ----------

// FamilyFilter 为家庭组下拉的取值：grouped 为按家庭组显示（不筛选，按组排列），in 为仅家庭组账户（分组显示），
// primary、member 为仅主邮箱、仅子成员，none 为非家庭组，group:<编号> 为只看这一组（分组显示）
export type FamilyFilter =
  'all' | 'grouped' | 'in' | 'primary' | 'member' | 'none' | `group:${string}`

// familyFilterGroup 返回“按组选择”选中的组编号，其他取值为空字符串
export function familyFilterGroup(filter: FamilyFilter): string {
  return filter.startsWith('group:') ? filter.slice('group:'.length) : ''
}

// familyFilterMatches 判断一个邮箱的家庭组身份是否符合筛选（all、grouped 不筛选）
export function familyFilterMatches(
  filter: FamilyFilter,
  membership: FamilyMembership | undefined,
): boolean {
  switch (filter) {
    case 'all':
    case 'grouped':
      return true
    case 'in':
      return membership !== undefined
    case 'primary':
      return membership?.role === 'primary'
    case 'member':
      return membership?.role === 'member'
    case 'none':
      return membership === undefined
    default:
      return membership?.group.id === familyFilterGroup(filter)
  }
}

// matchFamilyIds 返回按搜索词整组匹配的家庭组：搜索词是组名的子串，或与主邮箱完全相同（规范化后）。
// 不用主邮箱子串：否则搜 example.com 之类会把所有家庭组整组带出来。term 为已去首尾空白、转小写的搜索词
export function matchFamilyIds(term: string): Set<string> {
  const ids = new Set<string>()
  if (term === '') return ids
  for (const group of vaultData.value?.families ?? []) {
    if (group.primary === term || (group.name ?? '').toLowerCase().includes(term)) ids.add(group.id)
  }
  return ids
}

// groupTitle 为家庭组的显示名：有名称时“名称 · 主邮箱”，没有时只写主邮箱
export function groupTitle(group: Pick<FamilyGroup, 'name' | 'primary'>): string {
  return group.name ? `${group.name} · ${group.primary}` : group.primary
}

// ---------- 编辑框、提示 ----------

// profileEditor 为资料编辑框（App 里挂一次，资料页与账户页都经 openProfileEditor 打开）：add 为新增（邮箱可改，账户页从没有资料的
// 账户打开时预填账户邮箱），edit 为编辑已有资料（邮箱不可改）
export const profileEditor = reactive({ open: false, email: '', mode: 'add' as 'add' | 'edit' })

export function openProfileEditor(email: string, mode: 'add' | 'edit'): void {
  Object.assign(profileEditor, { open: true, email, mode })
}

export function closeProfileEditor(): void {
  Object.assign(profileEditor, { open: false, email: '' })
}

type NoticeHandler = (message: string, tone: 'success' | 'error') => void
let noticeHandler: NoticeHandler | null = null

// setVaultNoticeHandler 由 App 登记提示的显示函数；vaultNotice 在资料页、账户页、复制菜单、编辑框里显示一次操作结果。
// 提示里只写个数、字段名与邮箱，从不写密码、2FA、验证码、备注
export function setVaultNoticeHandler(handler: NoticeHandler | null): void {
  noticeHandler = handler
}

export function vaultNotice(message: string, tone: 'success' | 'error' = 'success'): void {
  noticeHandler?.(message, tone)
}

// formatCode 把 6 位验证码显示成“287 082”
export function formatCode(code: string): string {
  return code.length === 6 ? `${code.slice(0, 3)} ${code.slice(3)}` : code
}

// SECRET_INPUT_ATTRS 为输入密码、2FA、备注与导入文本的框共用的属性：关拼写检查（增强拼写检查会把文字发给服务商）、自动填充与
// 自动更正，并让 1Password、LastPass、Bitwarden 等扩展忽略它们（不抓取第三方密码）。配合不用 <form> 与 type=password
export const SECRET_INPUT_ATTRS = {
  autocomplete: 'off',
  spellcheck: 'false',
  autocapitalize: 'off',
  autocorrect: 'off',
  translate: 'no',
  'data-1p-ignore': '',
  'data-lpignore': 'true',
  'data-bwignore': '',
  'data-form-type': 'other',
  'data-gramm': 'false',
} as const

// passwordNeedsTab 判断密码会让整行改用 Tab 分隔：含 4 个单位以上的破折号串，或首尾是破折号类字符（与导入的分隔规则一致）
export function passwordNeedsTab(value: string): boolean {
  if (value === '') return false
  if (/^[-‐‑‒–−﹣－—―﹘]|[-‐‑‒–−﹣－—―﹘]$/.test(value)) return true
  let units = 0
  for (const char of value) {
    if ('—―﹘'.includes(char)) units += 2
    else if ('-‐‑‒–−﹣－'.includes(char)) units += 1
    else units = 0
    if (units >= 4) return true
  }
  return false
}
