<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { VaultApiError, api } from '@/api'
import { useI18n, type TranslationKey } from '@/i18n'
import type {
  Account,
  ProfileImportInput,
  ProfileImportItem,
  ProfileImportResult,
  ProfileImportSource,
  VaultField,
} from '@/types'
import {
  DEFAULT_LIMITS,
  REQUEST_BUDGET,
  SECRET_INPUT_ATTRS,
  formatBytes,
  jsonBytes,
  profileDraft,
  profileImportDone,
  profileImportKept,
  profilePreview,
  vaultData,
  vaultErrorText,
  vaultNotice,
  vaultWrite,
  type ImportFile,
  type ProfileListFilter,
  type VaultSubTab,
} from '@/vault'
import FormatText from './FormatText.vue'
import UiIcon from '../UiIcon.vue'
import UiPager from '../UiPager.vue'

// ProfileImport 为批量导入账号资料：大文本框（可粘贴几千行）与多选 .txt 文件 → 服务端解析预览（dry-run，只回打码信息）→ 确认导入。
// 草稿（文本、文件、预览）只在 vault.ts 的内存里，不写本地存储，导入成功后清空；预览之后改了文本、文件或导入选项，预览作废。
// 确认时原样再发预览时的来源，并带预览的版本与覆盖计数：期间资料被别处改过、覆盖条数变了，服务端 409 并附新预览
const props = defineProps<{
  byEmail: ReadonlyMap<string, Account[]>
  // accountsLoading 为账户列表还不知道（载入中或载入失败）：这时不标“未存在”、不给“查看不在账户里的”入口
  accountsLoading?: boolean
}>()
const emit = defineEmits<{ goto: [tab: VaultSubTab, filter?: ProfileListFilter] }>()

const { t, tf } = useI18n()
const PREVIEW_PAGE = 100
const helpKeys: TranslationKey[] = [
  'vault.help.line',
  'vault.help.fields',
  'vault.help.separator',
  'vault.help.tab',
  'vault.help.noPassword',
  'vault.help.totp',
  'vault.help.merge',
]
const fieldKeys: Record<VaultField, TranslationKey> = {
  password: 'vault.fieldPassword',
  recovery_email: 'vault.fieldRecovery',
  totp: 'vault.fieldTotp',
  note: 'vault.fieldNote',
}
const overwriteFields: VaultField[] = ['password', 'recovery_email', 'totp', 'note']

const limits = computed(() => vaultData.value?.limits ?? DEFAULT_LIMITS)
const area = ref<HTMLTextAreaElement | null>(null)
const fileInput = ref<HTMLInputElement | null>(null)
const busy = ref('')
const error = ref('')
const previewFilter = ref('all')
const previewPage = ref(1)
const previewPageSize = ref(PREVIEW_PAGE)
let fileSeq = 0

// ---------- 计量（输入后 200ms 去抖再算，每次按键都对几 MB 的文本 split + 编码会卡） ----------

const encoder = new TextEncoder()
function countLines(text: string): number {
  let count = 0
  for (const line of text.split('\n')) if (line.trim() !== '') count += 1
  return count
}

const pasteMeter = ref({ lines: 0, bytes: 0 })
let meterTimer: number | undefined
function measurePaste(): void {
  const text = profileDraft.text
  pasteMeter.value = { lines: countLines(text), bytes: encoder.encode(text).length }
}
measurePaste()
watch(
  () => profileDraft.text,
  () => {
    window.clearTimeout(meterTimer)
    meterTimer = window.setTimeout(measurePaste, 200)
  },
)
onBeforeUnmount(() => window.clearTimeout(meterTimer))

const usableFiles = computed(() => profileDraft.files.filter((file) => file.error === ''))
const meter = computed(() => {
  let lines = pasteMeter.value.lines
  let bytes = pasteMeter.value.bytes
  for (const file of usableFiles.value) {
    lines += file.lines
    bytes += file.bytes
  }
  const sources = usableFiles.value.length + (profileDraft.text.trim() === '' ? 0 : 1)
  return { lines, bytes, sources }
})
const tooLarge = computed(
  () =>
    meter.value.lines > limits.value.import_lines ||
    meter.value.bytes > limits.value.import_bytes ||
    meter.value.sources > limits.value.import_sources,
)
const empty = computed(() => meter.value.sources === 0)

// bump 记一次草稿改动：已有的预览作废
function bump(): void {
  profileDraft.version += 1
}

function onInput(): void {
  bump()
  profileImportDone.value = null
}

// ---------- 文件 ----------

function decode(bytes: Uint8Array): string {
  if (bytes[0] === 0xef && bytes[1] === 0xbb && bytes[2] === 0xbf)
    return new TextDecoder('utf-8').decode(bytes)
  if (bytes[0] === 0xff && bytes[1] === 0xfe) return new TextDecoder('utf-16le').decode(bytes)
  if (bytes[0] === 0xfe && bytes[1] === 0xff) return new TextDecoder('utf-16be').decode(bytes)
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    return new TextDecoder('gb18030').decode(bytes)
  }
}

// looksBinary 判断解码后的内容不像文本：含 U+0000，或控制字符（Tab、CR、LF 除外）与替换字符合计超过 1%
function looksBinary(text: string): boolean {
  let bad = 0
  for (let index = 0; index < text.length; index += 1) {
    const code = text.charCodeAt(index)
    if (code === 0) return true
    if ((code < 32 && code !== 9 && code !== 10 && code !== 13) || code === 0xfffd) bad += 1
  }
  return text.length > 0 && bad / text.length > 0.01
}

async function addFiles(list: FileList | null): Promise<void> {
  if (list === null) return
  for (const file of Array.from(list)) {
    const entry: ImportFile = {
      id: (fileSeq += 1),
      name: file.name,
      size: file.size,
      text: '',
      lines: 0,
      error: '',
      bytes: 0,
    }
    try {
      const text = decode(new Uint8Array(await file.arrayBuffer()))
      if (looksBinary(text)) entry.error = 'not_text'
      else {
        entry.text = text
        entry.lines = countLines(text)
        entry.bytes = encoder.encode(text).length
      }
    } catch {
      entry.error = 'read'
    }
    profileDraft.files.push(entry)
  }
  if (fileInput.value !== null) fileInput.value.value = ''
  onInput()
}

function removeFile(id: number): void {
  profileDraft.files = profileDraft.files.filter((file) => file.id !== id)
  onInput()
}

function clearDraft(): void {
  profileDraft.text = ''
  profileDraft.files = []
  profileDraft.ack = false
  profilePreview.value = null
  error.value = ''
  bump()
  measurePaste()
}

// ---------- 预览与确认 ----------

const pasteName = computed(() => t('vault.pasteSource'))
function buildSources(): ProfileImportSource[] {
  const sources: ProfileImportSource[] = []
  if (profileDraft.text.trim() !== '')
    sources.push({ name: pasteName.value, text: profileDraft.text })
  for (const file of usableFiles.value) sources.push({ name: file.name, text: file.text })
  return sources
}

const stale = computed(
  () => profilePreview.value !== null && profilePreview.value.version !== profileDraft.version,
)
const result = computed<ProfileImportResult | null>(() => profilePreview.value?.result ?? null)

async function runPreview(): Promise<void> {
  if (empty.value || tooLarge.value || busy.value !== '') return
  const sources = buildSources()
  const input: ProfileImportInput = { dry_run: true, mode: profileDraft.mode, sources }
  if (jsonBytes(input) > REQUEST_BUDGET) {
    error.value = t('vault.requestTooLarge')
    return
  }
  busy.value = 'preview'
  error.value = ''
  try {
    const preview = await api.importProfiles(input)
    profilePreview.value = { result: preview, version: profileDraft.version, sources }
    profileDraft.ack = false
    previewFilter.value = 'all'
    previewPage.value = 1
    profileImportDone.value = null
  } catch (reason) {
    error.value = vaultErrorText(reason)
  } finally {
    busy.value = ''
  }
}

const suspect = computed(() => result.value?.summary.suspect ?? [])
const canConfirm = computed(
  () =>
    result.value !== null &&
    !stale.value &&
    busy.value === '' &&
    !result.value.summary.over_limit &&
    result.value.summary.valid > 0 &&
    (suspect.value.length === 0 || profileDraft.ack),
)

async function confirmImport(): Promise<void> {
  const preview = profilePreview.value
  if (preview === null || !canConfirm.value) return
  const input: ProfileImportInput = {
    dry_run: false,
    mode: profileDraft.mode,
    sources: preview.sources,
    base_revision: preview.result.revision,
    expect_overwrites: preview.result.summary.overwrites,
  }
  if (profileDraft.ack) input.acknowledge_suspect = true
  busy.value = 'confirm'
  error.value = ''
  try {
    const done = await vaultWrite(api.importProfiles(input))
    profileImportDone.value = done
    profilePreview.value = null
    // 已导入的行从输入框清掉；粘贴内容里无法识别的行按原顺序留下，方便就地改正后重新预览（文件里的按“文件名:行号”列在结果里）
    const kept = invalidPasteLines(done, preview.sources)
    profileImportKept.value = kept.length
    profileDraft.text = kept.join('\n')
    profileDraft.files = []
    profileDraft.ack = false
    bump()
    measurePaste()
    const summary = done.summary
    vaultNotice(
      tf('vault.importDone', {
        new: summary.new,
        updated: summary.updated,
        unchanged: summary.unchanged,
        invalid: summary.invalid,
      }),
    )
  } catch (reason) {
    if (reason instanceof VaultApiError && reason.code === 'vault_conflict' && reason.preview) {
      const fresh = reason.preview as ProfileImportResult
      const sameBase = fresh.revision === preview.result.revision
      profilePreview.value = {
        result: fresh,
        version: profileDraft.version,
        sources: preview.sources,
      }
      profileDraft.ack = false
      previewFilter.value = 'all'
      previewPage.value = 1
      error.value = t(sameBase ? 'vault.optionsMismatch' : 'vault.conflictRefreshed')
    } else {
      error.value = vaultErrorText(reason)
    }
  } finally {
    busy.value = ''
  }
}

// invalidPasteLines 取出粘贴内容（第 0 个来源）里无法识别的行的原文，按行号顺序
function invalidPasteLines(done: ProfileImportResult, sources: ProfileImportSource[]): string[] {
  const paste = sources[0]
  if (paste === undefined || paste.name !== pasteName.value) return []
  const numbers = new Set<number>()
  for (const item of done.items ?? []) {
    if (item.status === 'invalid' && item.source === 0) numbers.add(item.line)
  }
  if (numbers.size === 0) return []
  const lines = paste.text.split('\n')
  return [...numbers]
    .sort((left, right) => left - right)
    .map((line) => (lines[line - 1] ?? '').replace(/\r$/, ''))
    .filter((line) => line.trim() !== '')
}

// ---------- 预览表 ----------

function itemMissing(item: ProfileImportItem): boolean {
  return (
    !props.accountsLoading &&
    item.status !== 'invalid' &&
    item.email !== undefined &&
    !props.byEmail.has(item.email)
  )
}

const derived = computed(() => {
  const items = result.value?.items ?? []
  let warned = 0
  let duplicate = 0
  let kept = 0
  const missing = new Set<string>()
  for (const item of items) {
    if ((item.warnings?.length ?? 0) > 0) warned += 1
    if ((item.also?.length ?? 0) > 0) duplicate += 1
    kept += item.kept?.length ?? 0
    if (itemMissing(item)) missing.add(item.email!)
  }
  return { warned, duplicate, kept, missing: missing.size }
})

function matches(item: ProfileImportItem, filter: string): boolean {
  if (filter === 'all') return true
  if (['new', 'update', 'unchanged', 'invalid'].includes(filter)) return item.status === filter
  if (filter === 'duplicate') return (item.also?.length ?? 0) > 0
  if (filter === 'warnings') return (item.warnings?.length ?? 0) > 0
  if (filter === 'missing') return itemMissing(item)
  if (filter === 'kept') return (item.kept?.length ?? 0) > 0
  if (filter.startsWith('overwrite:'))
    return (item.overwrites ?? []).includes(filter.slice('overwrite:'.length))
  if (filter.startsWith('warn:'))
    return (item.warnings ?? []).includes(filter.slice('warn:'.length))
  return true
}

const filteredItems = computed(() =>
  (result.value?.items ?? []).filter((item) => matches(item, previewFilter.value)),
)
const pageItems = computed(() =>
  filteredItems.value.slice(
    (previewPage.value - 1) * PREVIEW_PAGE,
    previewPage.value * PREVIEW_PAGE,
  ),
)
watch(previewFilter, () => {
  previewPage.value = 1
})

function setFilter(filter: string): void {
  previewFilter.value = previewFilter.value === filter ? 'all' : filter
}

const sourceNames = computed(() => profilePreview.value?.sources.map((source) => source.name) ?? [])
// labelIn 为行的位置：只有粘贴内容一个来源时写“第 N 行”，否则“来源:行号”（预览表与导入结果共用，写法一致）
function labelIn(names: readonly string[], item: { source: number; line: number }): string {
  const name = names[item.source]
  if (name === pasteName.value && names.length <= 1) return tf('vault.lineNo', { line: item.line })
  return `${name ?? '?'}:${item.line}`
}
function lineLabel(item: ProfileImportItem): string {
  return labelIn(sourceNames.value, item)
}

const statusKeys: Record<string, TranslationKey> = {
  new: 'vault.status.new',
  update: 'vault.status.update',
  unchanged: 'vault.status.unchanged',
  invalid: 'vault.status.invalid',
}
const statusClass: Record<string, string> = {
  new: 'text-green-300',
  update: 'text-blue-300',
  unchanged: 'text-gray-400',
  invalid: 'text-red-300',
}
const reasonKeys: Record<string, TranslationKey> = {
  first_not_email: 'vault.reason.first_not_email',
  no_fields: 'vault.reason.no_fields',
  line_too_long: 'vault.reason.line_too_long',
  control_char: 'vault.reason.control_char',
  field_too_long: 'vault.reason.field_too_long',
}
const hintKeys: Record<string, TranslationKey> = {
  dash_too_short: 'vault.hint.dash_too_short',
  unsupported_separator: 'vault.hint.unsupported_separator',
  mixed_separators: 'vault.hint.mixed_separators',
}
const warnKeys: Record<string, TranslationKey> = {
  dash_run: 'vault.warn.dash_run',
  mixed_separators: 'vault.warn.mixed_separators',
  empty_password: 'vault.warn.empty_password',
  password_looks_email: 'vault.warn.password_looks_email',
  password_looks_totp: 'vault.warn.password_looks_totp',
  extra_email: 'vault.warn.extra_email',
  extra_totp: 'vault.warn.extra_totp',
  looks_app_password: 'vault.warn.looks_app_password',
  totp_letters_only: 'vault.warn.totp_letters_only',
  otpauth_params: 'vault.warn.otpauth_params',
  otpauth_invalid: 'vault.warn.otpauth_invalid',
  recovery_same: 'vault.warn.recovery_same',
  repeated: 'vault.warn.repeated',
}
const warnShortKeys: Record<string, TranslationKey> = {
  dash_run: 'vault.warnShort.dash_run',
  mixed_separators: 'vault.warnShort.mixed_separators',
  empty_password: 'vault.warnShort.empty_password',
  password_looks_email: 'vault.warnShort.password_looks_email',
  password_looks_totp: 'vault.warnShort.password_looks_totp',
  extra_email: 'vault.warnShort.extra_email',
  extra_totp: 'vault.warnShort.extra_totp',
  looks_app_password: 'vault.warnShort.looks_app_password',
  totp_letters_only: 'vault.warnShort.totp_letters_only',
  otpauth_params: 'vault.warnShort.otpauth_params',
  otpauth_invalid: 'vault.warnShort.otpauth_invalid',
  recovery_same: 'vault.warnShort.recovery_same',
  repeated: 'vault.warnShort.repeated',
}
const suspectKeys: Record<string, TranslationKey> = {
  password_looks_email: 'vault.suspect.password_looks_email',
  password_looks_totp: 'vault.suspect.password_looks_totp',
  empty_password: 'vault.suspect.empty_password',
}

function reasonText(item: ProfileImportItem): string {
  const key = item.reason ? reasonKeys[item.reason] : undefined
  let text = key
    ? tf(key, {
        field: item.field ? t(fieldKeys[item.field as VaultField] ?? 'vault.fieldNote') : '',
      })
    : (item.reason ?? '')
  const hint = item.hint ? hintKeys[item.hint] : undefined
  if (hint) text += ` · ${t(hint)}`
  return text
}

function changesText(item: ProfileImportItem): string {
  return (item.changes ?? [])
    .map((field) => t(fieldKeys[field as VaultField] ?? 'vault.fieldNote'))
    .join(t('common.listSep'))
}

function warnTitle(item: ProfileImportItem): string {
  const parts = (item.warnings ?? []).map((code) => (warnKeys[code] ? t(warnKeys[code]) : code))
  if ((item.also?.length ?? 0) > 0)
    parts.push(
      tf('vault.alsoLines', {
        lines: (item.also ?? [])
          .map((ref) => lineLabel({ ...item, source: ref.source, line: ref.line }))
          .join(t('common.listSep')),
      }),
    )
  return parts.join('\n')
}

function suspectText(code: string): string {
  const summary = result.value?.summary
  const count = summary?.warnings?.[code] ?? 0
  const valid = Math.max(1, summary?.valid ?? 1)
  const key = suspectKeys[code]
  return key ? tf(key, { count, percent: Math.round((count / valid) * 100) }) : code
}

// previewTags 为预览汇总的标签（点了按它筛预览表，再点一次取消）；为 0 的可选项不显示
const previewTags = computed(() => {
  const preview = result.value
  if (preview === null) return []
  const summary = preview.summary
  const tags = [
    {
      key: 'new',
      label: tf('vault.tagNew', { count: summary.new }),
      cls: 'text-green-300',
      show: true,
    },
    {
      key: 'update',
      label: tf('vault.tagUpdated', { count: summary.updated }),
      cls: 'text-blue-300',
      show: true,
    },
    {
      key: 'unchanged',
      label: tf('vault.tagUnchanged', { count: summary.unchanged }),
      cls: 'text-gray-300',
      show: true,
    },
    {
      key: 'invalid',
      label: tf('vault.tagInvalid', { count: summary.invalid }),
      cls: 'text-red-300',
      show: true,
    },
    {
      key: 'duplicate',
      label: tf('vault.tagDuplicate', { count: summary.duplicate_lines }),
      cls: 'text-amber-300',
      show: summary.duplicate_lines > 0,
    },
    {
      key: 'warnings',
      label: tf('vault.warningsTag', { count: derived.value.warned }),
      cls: 'text-amber-300',
      show: derived.value.warned > 0,
    },
    {
      key: 'overwrite:password',
      label: tf('vault.overwriteTag', {
        field: t('vault.fieldPassword'),
        count: summary.overwrites.password,
      }),
      cls: 'text-red-300 font-semibold',
      show: summary.overwrites.password > 0,
    },
    {
      key: 'kept',
      label: tf('vault.keptTag', { count: derived.value.kept }),
      cls: 'text-gray-300',
      show: preview.mode === 'fill_empty' && derived.value.kept > 0,
    },
    {
      key: 'missing',
      label: tf('vault.notInAccounts', { count: derived.value.missing }),
      cls: 'text-amber-300',
      show: derived.value.missing > 0,
    },
  ]
  return tags.filter((tag) => tag.show)
})

const otherOverwrites = computed(() =>
  overwriteFields
    .filter((field) => field !== 'password' && (result.value?.summary.overwrites[field] ?? 0) > 0)
    .map((field) => ({ field, count: result.value?.summary.overwrites[field] ?? 0 })),
)

const confirmLabel = computed(() => {
  const summary = result.value?.summary
  if (!summary) return t('vault.confirmImportIdle')
  const values = {
    new: summary.new,
    updated: summary.updated,
    password: summary.overwrites.password,
  }
  return summary.overwrites.password > 0
    ? tf('vault.confirmImportOverwrite', values)
    : tf('vault.confirmImport', values)
})

// locate 在文本框里选中粘贴内容的第 line 行并滚动过去（不在表格里显示原文）
function locate(line: number): void {
  const element = area.value
  if (element === null) return
  const text = profileDraft.text
  let start = 0
  for (let current = 1; current < line; current += 1) {
    const next = text.indexOf('\n', start)
    if (next < 0) return
    start = next + 1
  }
  let end = text.indexOf('\n', start)
  if (end < 0) end = text.length
  element.focus()
  element.setSelectionRange(start, end)
  const lineHeight = Number.parseFloat(getComputedStyle(element).lineHeight) || 20
  element.scrollTop = Math.max(0, (line - 1) * lineHeight - element.clientHeight / 3)
}

const isPasteItem = (item: ProfileImportItem) =>
  sourceNames.value[item.source] === pasteName.value && item.source === 0

const done = computed(() => profileImportDone.value)
const doneInvalid = computed(() =>
  (done.value?.items ?? []).filter((item) => item.status === 'invalid'),
)
// doneInvalidText 为无法识别的行（写法与预览表相同，最多列 30 个；不显示原文）
const doneInvalidText = computed(() => {
  const names = done.value?.sources.map((source) => source.name) ?? []
  const list = doneInvalid.value.slice(0, 30).map((item) => labelIn(names, item))
  return list.join(t('common.listSep')) + (doneInvalid.value.length > 30 ? ' …' : '')
})
const doneKept = computed(() => profileImportKept.value)
const doneMissing = computed(() => {
  if (props.accountsLoading) return 0
  const emails = new Set<string>()
  for (const item of done.value?.items ?? []) {
    if (item.status !== 'invalid' && item.email && !props.byEmail.has(item.email))
      emails.add(item.email)
  }
  return emails.size
})

function modeChanged(): void {
  bump()
}
</script>

<template>
  <div class="space-y-4">
    <div class="grid gap-4 lg:grid-cols-[minmax(0,1fr)_22rem]">
      <section class="panel space-y-3 p-4">
        <label class="block">
          <span class="mb-1.5 flex items-center justify-between gap-2 text-xs text-gray-400">
            {{ t('vault.importPaste') }}
            <span
              class="font-mono"
              :class="tooLarge ? 'text-red-300' : 'text-gray-500'"
              data-testid="vault-import-meter"
              >{{
                tf('vault.importSize', {
                  lines: meter.lines.toLocaleString(),
                  size: formatBytes(meter.bytes),
                  maxLines: limits.import_lines.toLocaleString(),
                  maxSize: formatBytes(limits.import_bytes),
                })
              }}</span
            >
          </span>
          <textarea
            ref="area"
            v-model="profileDraft.text"
            class="input h-auto min-h-[16rem] w-full resize-y py-2 font-mono text-xs leading-5"
            rows="14"
            :placeholder="t('vault.importPlaceholder')"
            v-bind="SECRET_INPUT_ATTRS"
            data-testid="vault-import-text"
            @input="onInput"
          ></textarea>
        </label>
        <p v-if="tooLarge" class="text-xs text-red-300">{{ t('vault.importTooLarge') }}</p>
        <div class="flex flex-wrap items-center gap-2">
          <input
            ref="fileInput"
            class="hidden"
            type="file"
            accept=".txt,text/plain"
            multiple
            data-testid="vault-import-files"
            @change="addFiles(($event.target as HTMLInputElement).files)"
          />
          <button class="btn btn-sm" type="button" @click="fileInput?.click()">
            <UiIcon name="upload" :size="12" />
            {{ t('vault.importFiles') }}
          </button>
          <span
            v-for="file in profileDraft.files"
            :key="file.id"
            class="tag gap-1.5 font-sans"
            :class="file.error ? 'border-red-500/50 text-red-300' : ''"
            data-testid="vault-import-file"
          >
            <span class="max-w-[12rem] truncate">{{ file.name }}</span>
            <span v-if="file.error === 'not_text'">{{ t('vault.notText') }}</span>
            <span v-else-if="file.error === 'read'">{{ t('vault.fileReadFailed') }}</span>
            <span v-else class="text-gray-500"
              >{{ tf('vault.fileLines', { count: file.lines }) }} ·
              {{ formatBytes(file.size) }}</span
            >
            <button
              class="text-gray-500 hover:text-white"
              type="button"
              :aria-label="t('common.delete')"
              @click="removeFile(file.id)"
            >
              ×
            </button>
          </span>
        </div>
        <div class="flex flex-wrap items-center gap-3 border-t border-line pt-3">
          <fieldset class="flex flex-wrap items-center gap-3 text-xs text-gray-300">
            <legend class="sr-only">{{ t('vault.importMode') }}</legend>
            <span class="text-gray-500">{{ t('vault.importMode') }}</span>
            <label class="flex items-center gap-1.5">
              <input
                v-model="profileDraft.mode"
                type="radio"
                value="merge"
                data-mode="merge"
                @change="modeChanged"
              />
              {{ t('vault.modeMerge') }}
            </label>
            <label v-tooltip="t('vault.modeFillEmptyHelp')" class="flex items-center gap-1.5">
              <input
                v-model="profileDraft.mode"
                type="radio"
                value="fill_empty"
                data-mode="fill_empty"
                @change="modeChanged"
              />
              {{ t('vault.modeFillEmpty') }}
            </label>
          </fieldset>
          <span class="flex-1"></span>
          <button
            class="btn"
            type="button"
            :disabled="empty && !profilePreview"
            @click="clearDraft"
          >
            {{ t('vault.clearDraft') }}
          </button>
          <button
            class="btn btn-primary"
            type="button"
            data-testid="vault-preview"
            :disabled="empty || tooLarge || busy !== ''"
            :aria-busy="busy === 'preview'"
            @click="runPreview"
          >
            {{ t('vault.preview') }}
          </button>
        </div>
        <p
          v-if="error"
          class="rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-xs text-red-300"
          role="alert"
          data-testid="vault-import-error"
        >
          {{ error }}
        </p>
      </section>

      <aside class="panel p-4 text-xs leading-5 text-gray-400">
        <details open class="group/help">
          <summary
            class="mb-2 flex cursor-pointer items-center gap-2 text-sm font-semibold text-gray-200"
          >
            <UiIcon name="info" :size="14" class="text-blue-300" />
            {{ t('vault.formatTitle') }}
          </summary>
          <ul class="list-disc space-y-1.5 pl-4">
            <li v-for="key in helpKeys" :key="key"><FormatText :text="t(key)" /></li>
          </ul>
          <p class="mt-2 text-gray-500">{{ t('vault.help.examples') }}</p>
          <pre
            class="mt-1 overflow-x-auto rounded border border-line bg-canvas p-2 font-mono text-[11px] leading-5 text-gray-300"
          >
a@example.com----Fake-Pass-1----r@example.com----JBSWY3DPEHPK3PXP
b@example.com----Fake-Pass-2----JBSWY3DPEHPK3PXP----r@example.com
c@example.com----Fake-Pass-3----r@example.com
d@example.com——Fake-Pass-4——{{ t('vault.help.noteExample') }}
# {{ t('vault.help.commentExample') }}</pre>
        </details>
      </aside>
    </div>

    <!-- 导入完成 -->
    <section
      v-if="done"
      class="panel space-y-2 border-green-500/40 p-4 text-sm"
      data-testid="vault-import-done"
    >
      <p class="flex items-center gap-2 font-semibold text-green-300">
        <UiIcon name="check" :size="14" />
        {{
          tf('vault.importDone', {
            new: done.summary.new,
            updated: done.summary.updated,
            unchanged: done.summary.unchanged,
            invalid: done.summary.invalid,
          })
        }}
      </p>
      <p v-if="doneInvalid.length > 0" class="text-xs text-red-300">
        {{ tf('vault.doneInvalid', { count: doneInvalid.length }) }}
        <span class="font-mono text-gray-400">{{ doneInvalidText }}</span>
      </p>
      <p v-if="doneKept > 0" class="text-xs text-gray-400" data-testid="vault-invalid-kept">
        {{ tf('vault.invalidKept', { count: doneKept }) }}
      </p>
      <p class="flex flex-wrap items-center gap-3 text-xs text-gray-400">
        <button
          v-if="doneMissing > 0"
          class="text-amber-300 hover:text-amber-200"
          type="button"
          @click="emit('goto', 'profiles', 'missing')"
        >
          {{ tf('vault.viewMissing', { count: doneMissing }) }} ›
        </button>
        <button
          class="text-blue-300 hover:text-blue-200"
          type="button"
          @click="emit('goto', 'profiles')"
        >
          {{ t('vault.viewProfiles') }} ›
        </button>
        <span>{{ t('vault.snapshotSaved') }}</span>
      </p>
    </section>

    <!-- 预览 -->
    <section v-if="result" class="space-y-3" data-testid="vault-import-preview">
      <div
        v-if="stale"
        class="rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs text-amber-200"
        data-testid="vault-preview-stale"
      >
        {{ t('vault.previewStale') }}
      </div>
      <div
        v-if="suspect.length > 0"
        class="space-y-2 rounded-md border border-red-500/50 bg-red-500/10 px-3 py-2.5 text-xs text-red-200"
        data-testid="vault-suspect"
      >
        <p class="flex items-center gap-1.5 font-semibold text-red-300">
          <UiIcon name="warning" :size="14" />
          {{ t('vault.suspectTitle') }}
        </p>
        <p v-for="code in suspect" :key="code" class="flex flex-wrap items-center gap-2">
          <span>{{ suspectText(code) }}</span>
          <button
            class="text-red-300 underline-offset-2 hover:underline"
            type="button"
            @click="previewFilter = `warn:${code}`"
          >
            {{ t('vault.suspectOnly') }}
          </button>
        </p>
        <label class="flex items-center gap-2 font-medium text-red-100">
          <input v-model="profileDraft.ack" type="checkbox" data-testid="vault-ack" />
          {{ t('vault.ackSuspect') }}
        </label>
      </div>
      <div
        v-if="(result.summary.hints ?? []).includes('looks_like_family_list')"
        class="flex flex-wrap items-center gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs text-amber-200"
      >
        {{ t('vault.hint.looks_like_family_list') }}
        <button class="btn btn-sm" type="button" @click="emit('goto', 'familyImport')">
          {{ t('vault.tabFamilyImport') }} ›
        </button>
      </div>

      <div class="panel space-y-3 p-3">
        <div class="flex flex-wrap items-center gap-1.5 text-xs" data-testid="vault-preview-tags">
          <button
            v-for="tag in previewTags"
            :key="tag.key"
            class="tag font-sans transition hover:border-line-strong"
            :class="[tag.cls, previewFilter === tag.key ? 'border-blue-500 bg-blue-500/15' : '']"
            type="button"
            :aria-pressed="previewFilter === tag.key"
            :data-tag="tag.key"
            @click="setFilter(tag.key)"
          >
            {{ tag.label }}
          </button>
          <span class="ml-auto text-gray-500">{{
            tf('vault.previewLines', {
              lines: result.summary.lines,
              blank: result.summary.blank,
              comments: result.summary.comments,
            })
          }}</span>
        </div>
        <p
          v-if="otherOverwrites.length > 0"
          class="flex flex-wrap items-center gap-1.5 text-xs text-gray-400"
        >
          {{ t('vault.overwriteOthers') }}
          <button
            v-for="entry in otherOverwrites"
            :key="entry.field"
            class="tag font-sans text-red-300"
            :class="previewFilter === `overwrite:${entry.field}` ? 'border-blue-500' : ''"
            type="button"
            @click="setFilter(`overwrite:${entry.field}`)"
          >
            {{ tf('vault.overwriteTag', { field: t(fieldKeys[entry.field]), count: entry.count }) }}
          </button>
        </p>
        <p v-if="result.summary.over_limit" class="text-xs text-red-300">
          {{
            tf('vault.overLimit', {
              max: limits.profiles.toLocaleString(),
              total: result.summary.total_after.toLocaleString(),
            })
          }}
        </p>

        <div class="overflow-x-auto">
          <!-- 邮箱列不定宽、其余列合计 52rem：表格至少 66rem（窄屏横向滚动），邮箱列至少 14rem；长邮箱换行显示全文，不截断 -->
          <table class="data-table min-w-[66rem] text-xs" data-testid="vault-preview-table">
            <colgroup>
              <col class="w-24" />
              <col />
              <col class="w-60" />
              <col class="w-36" />
              <col class="w-28" />
              <col class="w-20" />
              <col class="w-40" />
            </colgroup>
            <thead>
              <tr>
                <th>{{ t('vault.colLine') }}</th>
                <th>{{ t('vault.colEmail') }}</th>
                <th>{{ t('vault.colFields') }}</th>
                <th>{{ t('vault.colResult') }}</th>
                <th>{{ t('vault.colOverwrite') }}</th>
                <th>{{ t('vault.colAccount') }}</th>
                <th>{{ t('vault.colWarnings') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="pageItems.length === 0">
                <td colspan="7" class="py-6 text-center text-gray-500">
                  {{ t('vault.previewEmpty') }}
                </td>
              </tr>
              <tr
                v-for="item in pageItems"
                :key="`${item.source}:${item.line}`"
                :data-status="item.status"
                :data-line="item.line"
              >
                <td class="font-mono text-gray-400">
                  <div v-tooltip="lineLabel(item)" class="cell-truncate">{{ lineLabel(item) }}</div>
                </td>
                <td>
                  <div
                    v-tooltip="item.email ?? ''"
                    class="font-mono wrap-anywhere text-gray-200"
                    data-testid="vault-preview-email"
                  >
                    {{ item.email ?? '—' }}
                  </div>
                </td>
                <td>
                  <div v-if="item.fields" class="flex flex-wrap items-center gap-1">
                    <span
                      v-if="item.fields.password"
                      class="tag font-sans"
                      :title="t('vault.fieldPassword')"
                      >{{ t('vault.fieldPassword') }} ●</span
                    >
                    <span
                      v-if="item.fields.recovery_email"
                      class="tag max-w-[11rem] truncate"
                      :title="item.fields.recovery_email"
                      >{{ item.fields.recovery_email }}</span
                    >
                    <span
                      v-if="item.fields.totp"
                      class="tag"
                      :class="
                        (item.fields.totp_flags ?? []).includes('letters_only')
                          ? 'border-amber-500/50 text-amber-300'
                          : ''
                      "
                      >2FA …{{ item.fields.totp_last4 }}</span
                    >
                    <span v-if="item.fields.note" class="tag font-sans">{{
                      t('vault.fieldNote')
                    }}</span>
                  </div>
                  <span v-else class="text-gray-600">—</span>
                </td>
                <td>
                  <span :class="statusClass[item.status] ?? 'text-gray-300'" class="font-medium">{{
                    t(statusKeys[item.status] ?? 'vault.status.invalid')
                  }}</span
                  ><span
                    v-if="item.status === 'update' && (item.changes?.length ?? 0) > 0"
                    class="text-gray-400"
                    >{{ t('common.colon') }}{{ changesText(item) }}</span
                  >
                  <div v-if="item.status === 'invalid'" class="text-[11px] text-red-300/90">
                    {{ reasonText(item) }}
                    <button
                      v-if="isPasteItem(item) && !stale"
                      class="ml-1 text-blue-300 hover:text-blue-200"
                      type="button"
                      data-testid="vault-locate"
                      @click="locate(item.line)"
                    >
                      {{ t('vault.locate') }}
                    </button>
                  </div>
                </td>
                <td>
                  <span
                    v-for="field in item.overwrites ?? []"
                    :key="field"
                    class="tag mr-1 border-red-500/50 font-sans text-red-300"
                    >{{
                      tf('vault.overwriteShort', {
                        field: t(fieldKeys[field as VaultField] ?? 'vault.fieldNote'),
                      })
                    }}</span
                  >
                  <span v-if="(item.kept?.length ?? 0) > 0" class="text-gray-500">
                    {{
                      tf('vault.keptShort', {
                        fields: (item.kept ?? [])
                          .map((field) => t(fieldKeys[field as VaultField] ?? 'vault.fieldNote'))
                          .join(t('common.listSep')),
                      })
                    }}
                  </span>
                </td>
                <td>
                  <template v-if="item.status !== 'invalid' && item.email">
                    <span v-if="accountsLoading" class="text-gray-600">…</span>
                    <span v-else-if="byEmail.has(item.email)" class="text-green-300">{{
                      t('vault.exists')
                    }}</span>
                    <span v-else class="tag tag-missing">{{ t('vault.missingTag') }}</span>
                  </template>
                </td>
                <td>
                  <div
                    v-if="(item.warnings?.length ?? 0) > 0 || (item.also?.length ?? 0) > 0"
                    v-tooltip="warnTitle(item)"
                    class="flex flex-wrap gap-1"
                    tabindex="0"
                  >
                    <span
                      v-for="code in item.warnings ?? []"
                      :key="code"
                      class="tag border-amber-500/40 font-sans text-amber-300"
                      >{{ warnShortKeys[code] ? t(warnShortKeys[code]) : code }}</span
                    >
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <UiPager
          v-if="filteredItems.length > PREVIEW_PAGE"
          v-model:page="previewPage"
          v-model:page-size="previewPageSize"
          :total="filteredItems.length"
          fixed-size
        />
        <div class="flex flex-wrap items-center justify-end gap-2 border-t border-line pt-3">
          <span v-if="result.mode === 'fill_empty'" class="text-xs text-gray-400">{{
            t('vault.modeFillEmptyActive')
          }}</span>
          <button
            class="btn btn-primary"
            type="button"
            data-testid="vault-confirm-import"
            :disabled="!canConfirm"
            :aria-busy="busy === 'confirm'"
            @click="confirmImport"
          >
            {{ confirmLabel }}
          </button>
        </div>
      </div>
    </section>
  </div>
</template>
