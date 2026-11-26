<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { api } from '@/api'
import { stateDotClass, stateKeys, stateTextClass } from '@/accountState'
import { copyText } from '@/clipboard'
import { confirmAction } from '@/confirm'
import { useI18n } from '@/i18n'
import { usePaging } from '@/paging'
import type { Account, ProfileExport, VaultProfile } from '@/types'
import {
  DEFAULT_LIMITS,
  downloadText,
  famClass,
  familyByEmail,
  familyFilterMatches,
  formatShortTime,
  matchFamilyIds,
  openManualCopy,
  openProfileEditor,
  vaultData,
  vaultErrorText,
  vaultNotice,
  vaultWrite,
  type FamilyFilter,
  type ProfileListFilter,
} from '@/vault'
import FamilyBadge from './FamilyBadge.vue'
import FamilyFilterMenu from './FamilyFilterMenu.vue'
import SecretMenu from './SecretMenu.vue'
import UiIcon from '../UiIcon.vue'
import UiPager from '../UiPager.vue'

// ProfileList 为账号资料列表：筛选（全部 / 已存在账户 / 未存在邮箱 / 有 2FA）、家庭组下拉、搜索（邮箱、辅助邮箱、组名、主邮箱）、
// 排序、分页（每页条数记在本地，只是条数）、多选（复制选中整行、导出 TXT、删除）。列表只有打码信息；“未存在”就是资料里有、
// 账户列表里没有的邮箱（删掉了账户或还没添加），按账户列表现算
// 显式命名：VaultPanel 的 KeepAlive 按名称只保留这个列表（切子页签回来筛选、搜索、页码与选择不变）
defineOptions({ name: 'ProfileList' })
const props = defineProps<{
  byEmail: ReadonlyMap<string, Account[]>
  // accountsLoading 为账户列表还不知道（载入中或载入失败）：这时“已存在账户/未存在邮箱”的计数显示“…”，这两个筛选先不出结果
  accountsLoading: boolean
  initialFilter?: ProfileListFilter | undefined
  initialFamily?: string | undefined
}>()

const { t, tf } = useI18n()

type SortKey = 'email' | 'updated' | 'family'
const listFilter = ref<ProfileListFilter>(props.initialFilter ?? 'all')
const familyFilter = ref<FamilyFilter>(props.initialFamily ? `group:${props.initialFamily}` : 'all')
const queryInput = ref('')
const query = ref('')
const sortKey = ref<SortKey>('email')
const selected = ref<Set<string>>(new Set())
const includeNote = ref(false)
const working = ref('')

let searchTimer: number | undefined
watch(queryInput, (value) => {
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => (query.value = value), 150)
})

// 表格列宽：面板（容器）不到 60rem 时改成卡片排列（模板里的 @max-[60rem]）；“更新时间”列只在面板至少 UPDATED_MIN_REM 宽时出现，
// col、th、td 一起用 v-if（只把单元格设成 display:none 时 col 还在，后面的列会错位、操作按钮压到 2FA 与备注上）。
// 其余列定宽合计约 47.5rem（加更新时间约 54.5rem），这样表格排列时邮箱列至少约 12rem
const UPDATED_MIN_REM = 70
const tablePanel = ref<HTMLElement | null>(null)
const panelRem = ref(0)
let panelObserver: ResizeObserver | undefined
function measurePanel(): void {
  const element = tablePanel.value
  if (element === null) return
  const rootSize = Number.parseFloat(getComputedStyle(document.documentElement).fontSize) || 16
  panelRem.value = element.clientWidth / rootSize
}
watch(
  tablePanel,
  (element) => {
    panelObserver?.disconnect()
    panelObserver = undefined
    if (element === null) return
    if (typeof ResizeObserver !== 'undefined') {
      panelObserver = new ResizeObserver(measurePanel)
      panelObserver.observe(element)
    }
    measurePanel()
  },
  { flush: 'post' },
)
onBeforeUnmount(() => panelObserver?.disconnect())
const showUpdated = computed(() => panelRem.value >= UPDATED_MIN_REM)
const columnCount = computed(() => (showUpdated.value ? 9 : 8))

const profiles = computed(() => vaultData.value?.profiles ?? [])
const limits = computed(() => vaultData.value?.limits ?? DEFAULT_LIMITS)

const counts = computed(() => {
  const result = { all: 0, exists: 0, missing: 0, totp: 0 }
  for (const profile of profiles.value) {
    result.all += 1
    if (props.byEmail.has(profile.email)) result.exists += 1
    else result.missing += 1
    if (profile.has_totp) result.totp += 1
  }
  return result
})

// accountFilterPending 为选了“已存在账户/未存在邮箱”而账户列表还不知道：列表先空着并写明原因，不按空的账户列表把全部算成未存在
const accountFilterPending = computed(
  () => props.accountsLoading && (listFilter.value === 'exists' || listFilter.value === 'missing'),
)

const familyCounts = computed(() => {
  const result = { all: 0, in: 0, primary: 0, member: 0, none: 0 }
  const groups = new Map<string, number>()
  for (const profile of profiles.value) {
    result.all += 1
    const membership = familyByEmail.value.get(profile.email)
    if (membership === undefined) {
      result.none += 1
      continue
    }
    result.in += 1
    result[membership.role] += 1
    groups.set(membership.group.id, (groups.get(membership.group.id) ?? 0) + 1)
  }
  return { result, groups }
})

const familiesFailed = computed(() =>
  (vaultData.value?.errors ?? []).some((entry) => entry.file === 'families'),
)

const filtered = computed(() => {
  if (accountFilterPending.value) return []
  const term = query.value.trim().toLowerCase()
  const groupIds = matchFamilyIds(term)
  const families = familyByEmail.value
  const list = profiles.value.filter((profile) => {
    const exists = props.byEmail.has(profile.email)
    if (listFilter.value === 'exists' && !exists) return false
    if (listFilter.value === 'missing' && exists) return false
    if (listFilter.value === 'totp' && !profile.has_totp) return false
    const membership = families.get(profile.email)
    if (!familyFilterMatches(familyFilter.value, membership)) return false
    if (term === '') return true
    return (
      profile.email.includes(term) ||
      (profile.recovery_email ?? '').includes(term) ||
      (membership !== undefined && groupIds.has(membership.group.id))
    )
  })
  if (sortKey.value === 'updated') {
    list.sort((left, right) => right.updated_at.localeCompare(left.updated_at))
  } else if (sortKey.value === 'family') {
    const rank = (profile: VaultProfile): [number, number] => {
      const membership = families.get(profile.email)
      return membership === undefined
        ? [Number.MAX_SAFE_INTEGER, 0]
        : [membership.group.seq, membership.position]
    }
    list.sort((left, right) => {
      const [leftSeq, leftPos] = rank(left)
      const [rightSeq, rightPos] = rank(right)
      return leftSeq - rightSeq || leftPos - rightPos || left.email.localeCompare(right.email)
    })
  }
  return list
})

const { page, pageSize, pageItems } = usePaging(
  () => filtered.value,
  'aistudio2api_vault_page_size',
  50,
)
watch([listFilter, familyFilter, query, sortKey], () => {
  page.value = 1
})
watch([listFilter, familyFilter], () => {
  selected.value = new Set()
})
// 资料被删掉后从选择里去掉
watch(profiles, (list) => {
  if (selected.value.size === 0) return
  const present = new Set(list.map((profile) => profile.email))
  const next = new Set([...selected.value].filter((email) => present.has(email)))
  if (next.size !== selected.value.size) selected.value = next
})

const pageAllSelected = computed(
  () =>
    pageItems.value.length > 0 &&
    pageItems.value.every((profile) => selected.value.has(profile.email)),
)

function toggle(email: string): void {
  const next = new Set(selected.value)
  if (next.has(email)) next.delete(email)
  else next.add(email)
  selected.value = next
}

function togglePage(): void {
  const next = new Set(selected.value)
  const all = !pageAllSelected.value
  for (const profile of pageItems.value) {
    if (all) next.add(profile.email)
    else next.delete(profile.email)
  }
  selected.value = next
}

function selectAllFiltered(): void {
  selected.value = new Set(filtered.value.map((profile) => profile.email))
}

function chunks<T>(list: T[], size: number): T[][] {
  const result: T[][] = []
  for (let start = 0; start < list.length; start += size)
    result.push(list.slice(start, start + size))
  return result
}

// exportSelected 按选中顺序取整行（每批不超过单次邮箱上限）；note 为带上备注
async function exportSelected(): Promise<ProfileExport> {
  const total: ProfileExport = { lines: [], count: 0, missing: [], tab_lines: 0, lossy: [] }
  for (const batch of chunks([...selected.value], limits.value.emails_per_request)) {
    const part = await api.exportProfiles({ emails: batch, include_note: includeNote.value })
    total.lines.push(...part.lines)
    total.count += part.count
    total.missing.push(...(part.missing ?? []))
    total.tab_lines += part.tab_lines
    total.lossy.push(...(part.lossy ?? []))
  }
  return total
}

// exportReport 为整行的补充说明：改用 Tab 的行数、重新导入不能完全还原的行（邮箱不是秘密，列出前几个）
function exportReport(result: ProfileExport): string {
  if (result.tab_lines === 0 && result.lossy.length === 0) return ''
  const emails = result.lossy.slice(0, 5).map((entry) => entry.email)
  const more = result.lossy.length > emails.length ? ' …' : ''
  return (
    ' ' +
    tf('vault.exportReport', { tab: result.tab_lines, lossy: result.lossy.length }) +
    (emails.length > 0 ? `${t('common.colon')}${emails.join(t('common.listSep'))}${more}` : '')
  )
}

async function copySelected(): Promise<void> {
  working.value = 'copy'
  try {
    const result = await exportSelected()
    const text = result.lines.join('\n')
    if (await copyText(text)) {
      vaultNotice(tf('vault.copiedLines', { count: result.count }) + exportReport(result))
    } else {
      openManualCopy(text, tf('vault.copiedLinesTitle', { count: result.count }))
    }
  } catch (error) {
    vaultNotice(vaultErrorText(error), 'error')
  } finally {
    working.value = ''
  }
}

async function exportTxt(): Promise<void> {
  const count = selected.value.size
  const message = tf(includeNote.value ? 'vault.exportConfirmNote' : 'vault.exportConfirm', {
    count,
  })
  if (!(await confirmAction(message, t('vault.exportTxt')))) return
  working.value = 'export'
  try {
    const result = await exportSelected()
    downloadText(result.lines.map((line) => `${line}\r\n`).join(''), 'account-profiles')
    vaultNotice(tf('vault.exported', { count: result.count }) + exportReport(result))
  } catch (error) {
    vaultNotice(vaultErrorText(error), 'error')
  } finally {
    working.value = ''
  }
}

async function removeEmails(emails: string[], message: string): Promise<void> {
  if (!(await confirmAction(message, t('common.delete')))) return
  working.value = 'delete'
  try {
    let deleted = 0
    for (const batch of chunks(emails, limits.value.emails_per_request)) {
      const result = await vaultWrite(api.deleteProfiles(batch))
      deleted += result.deleted
    }
    selected.value = new Set([...selected.value].filter((email) => !emails.includes(email)))
    vaultNotice(tf('vault.deleted', { count: deleted }))
  } catch (error) {
    vaultNotice(vaultErrorText(error), 'error')
  } finally {
    working.value = ''
  }
}

function removeSelected(): void {
  void removeEmails(
    [...selected.value],
    tf('vault.bulkDeleteConfirm', { count: selected.value.size }),
  )
}

function removeOne(profile: VaultProfile): void {
  void removeEmails([profile.email], tf('vault.deleteConfirm', { email: profile.email }))
}

async function copyEmail(email: string): Promise<void> {
  if (await copyText(email)) vaultNotice(tf('vault.copied', { what: t('vault.fieldEmail') }))
  else openManualCopy(email, t('vault.fieldEmail'))
}

function accountsOf(email: string): Account[] {
  return props.byEmail.get(email) ?? []
}

const filterOptions = computed(() => [
  { value: 'all' as const, label: t('vault.filterAll'), count: counts.value.all },
  {
    value: 'exists' as const,
    label: t('vault.filterExists'),
    count: props.accountsLoading ? '…' : counts.value.exists,
  },
  {
    value: 'missing' as const,
    label: t('vault.filterMissing'),
    count: props.accountsLoading ? '…' : counts.value.missing,
  },
  { value: 'totp' as const, label: t('vault.filterTotp'), count: counts.value.totp },
])
</script>

<template>
  <div class="space-y-3">
    <div class="flex flex-wrap items-center gap-2">
      <div class="seg" role="tablist" :aria-label="t('vault.listFilter')">
        <button
          v-for="option in filterOptions"
          :key="option.value"
          class="seg-item"
          :class="{ active: listFilter === option.value }"
          type="button"
          role="tab"
          :aria-selected="listFilter === option.value"
          :data-filter="option.value"
          @click="listFilter = option.value"
        >
          <span v-if="option.value === 'missing'" class="dot bg-amber-400"></span>
          {{ option.label }}
          <span class="seg-count">{{ option.count }}</span>
        </button>
      </div>
      <FamilyFilterMenu
        v-model="familyFilter"
        kind="profiles"
        :counts="familyCounts.result"
        :group-counts="familyCounts.groups"
        :unavailable="familiesFailed ? t('accounts.familyFilterUnavailable') : ''"
        trigger-class="input input-trigger w-full sm:w-auto sm:min-w-[10rem]"
      />
      <input
        v-model="queryInput"
        class="input min-w-40 flex-1 sm:max-w-80"
        type="search"
        :placeholder="t('vault.search')"
        :aria-label="t('vault.search')"
        autocomplete="off"
        spellcheck="false"
      />
      <select v-model="sortKey" class="input" :aria-label="t('vault.sort')">
        <option value="email">{{ t('vault.sortEmail') }}</option>
        <option value="updated">{{ t('vault.sortUpdated') }}</option>
        <option value="family">{{ t('vault.sortFamily') }}</option>
      </select>
      <span class="flex-1"></span>
      <button class="btn btn-primary" type="button" @click="openProfileEditor('', 'add')">
        <UiIcon name="plusLine" :size="14" />
        {{ t('vault.add') }}
      </button>
    </div>

    <div
      v-if="selected.size > 0"
      class="flex flex-wrap items-center gap-2 rounded-md border border-blue-500/40 bg-blue-500/10 px-3 py-2 text-xs"
      data-testid="vault-selection"
    >
      <strong class="text-blue-300">{{ tf('vault.selected', { count: selected.size }) }}</strong>
      <button
        v-if="selected.size < filtered.length"
        class="text-blue-400 hover:text-blue-300"
        type="button"
        @click="selectAllFiltered"
      >
        {{ tf('vault.selectAllFiltered', { count: filtered.length }) }}
      </button>
      <span class="flex-1"></span>
      <label class="flex items-center gap-1.5 text-gray-300">
        <input v-model="includeNote" type="checkbox" data-testid="vault-include-note" />
        {{ t('vault.includeNote') }}
      </label>
      <button
        class="btn btn-sm"
        type="button"
        :disabled="working !== ''"
        :aria-busy="working === 'copy'"
        data-testid="vault-copy-selected"
        @click="copySelected"
      >
        <UiIcon name="copy" :size="12" />
        {{ t('vault.copySelected') }}
      </button>
      <button
        class="btn btn-sm"
        type="button"
        :disabled="working !== ''"
        :aria-busy="working === 'export'"
        data-testid="vault-export"
        @click="exportTxt"
      >
        <UiIcon name="download" :size="12" />
        {{ t('vault.exportTxt') }}
      </button>
      <button
        class="btn btn-sm btn-danger"
        type="button"
        :disabled="working !== ''"
        :aria-busy="working === 'delete'"
        data-testid="vault-delete-selected"
        @click="removeSelected"
      >
        {{ t('common.delete') }}
      </button>
      <button class="btn btn-sm btn-ghost" type="button" @click="selected = new Set()">
        {{ t('accounts.clearSelection') }}
      </button>
    </div>

    <div v-if="profiles.length === 0" class="panel px-6 py-10 text-center text-sm text-gray-500">
      <p class="mb-2">{{ t('vault.empty') }}</p>
      <p class="text-xs">{{ t('vault.emptyHint') }}</p>
    </div>
    <div v-else ref="tablePanel" class="panel @container">
      <table class="data-table @max-[60rem]:block" data-testid="vault-profile-table">
        <colgroup class="@max-[60rem]:hidden">
          <col class="w-10" />
          <col />
          <col class="w-32" />
          <col class="w-20" />
          <col class="w-44" />
          <col class="w-28" />
          <col class="w-16" />
          <col v-if="showUpdated" class="w-28" />
          <col class="w-40" />
        </colgroup>
        <thead class="@max-[60rem]:hidden">
          <tr>
            <th>
              <input
                type="checkbox"
                :checked="pageAllSelected"
                :aria-label="t('vault.selectPage')"
                @change="togglePage"
              />
            </th>
            <th>{{ t('vault.colEmail') }}</th>
            <th>{{ t('vault.colAccount') }}</th>
            <th>{{ t('vault.colPassword') }}</th>
            <th>{{ t('vault.colRecovery') }}</th>
            <th>{{ t('vault.colTotp') }}</th>
            <th>{{ t('vault.colNote') }}</th>
            <th v-if="showUpdated">{{ t('vault.colUpdated') }}</th>
            <th class="text-right">{{ t('vault.colActions') }}</th>
          </tr>
        </thead>
        <tbody class="@max-[60rem]:block">
          <tr v-if="filtered.length === 0" class="@max-[60rem]:block">
            <td :colspan="columnCount" class="py-8 text-center text-gray-500 @max-[60rem]:block">
              {{ accountFilterPending ? t('vault.accountsPending') : t('vault.noMatch') }}
            </td>
          </tr>
          <tr
            v-for="profile in pageItems"
            :key="profile.email"
            :class="{ 'is-selected': selected.has(profile.email) }"
            class="@max-[60rem]:grid @max-[60rem]:grid-cols-[auto_minmax(0,1fr)_auto] @max-[60rem]:gap-x-2 @max-[60rem]:gap-y-0.5 @max-[60rem]:border-b @max-[60rem]:border-[#21262d] @max-[60rem]:px-3 @max-[60rem]:py-2"
            :data-email="profile.email"
          >
            <td
              class="@max-[60rem]:row-span-6 @max-[60rem]:border-0 @max-[60rem]:p-0 @max-[60rem]:pt-0.5"
              :class="
                familyByEmail.get(profile.email)
                  ? ['fam-bar', famClass(familyByEmail.get(profile.email)?.group.color)]
                  : ''
              "
            >
              <input
                type="checkbox"
                :checked="selected.has(profile.email)"
                :aria-label="profile.email"
                @change="toggle(profile.email)"
              />
            </td>
            <td class="@max-[60rem]:border-0 @max-[60rem]:p-0">
              <div class="flex min-w-0 items-center gap-1.5">
                <FamilyBadge
                  v-if="familyByEmail.get(profile.email)"
                  :membership="familyByEmail.get(profile.email)!"
                />
                <span v-tooltip="profile.email" class="cell-truncate font-mono text-gray-200">{{
                  profile.email
                }}</span>
                <button
                  class="shrink-0 rounded p-0.5 text-gray-500 hover:bg-raised hover:text-gray-200"
                  type="button"
                  :aria-label="t('vault.copyEmail')"
                  @click="copyEmail(profile.email)"
                >
                  <UiIcon name="copy" :size="12" />
                </button>
              </div>
            </td>
            <td class="text-xs @max-[60rem]:col-start-2 @max-[60rem]:border-0 @max-[60rem]:p-0">
              <span class="hidden text-gray-500 @max-[60rem]:inline"
                >{{ t('vault.colAccount') }}{{ t('common.colon') }}</span
              >
              <span
                v-if="accountsLoading && accountsOf(profile.email).length === 0"
                class="text-gray-500"
                >…</span
              >
              <span
                v-else-if="accountsOf(profile.email).length > 0"
                class="inline-flex items-center gap-1.5"
                :class="stateTextClass[accountsOf(profile.email)[0]!.state]"
              >
                <span
                  class="dot"
                  :class="stateDotClass[accountsOf(profile.email)[0]!.state]"
                ></span>
                <span class="cell-truncate">{{
                  t(stateKeys[accountsOf(profile.email)[0]!.state])
                }}</span>
                <span v-if="accountsOf(profile.email).length > 1" class="font-mono text-gray-500"
                  >×{{ accountsOf(profile.email).length }}</span
                >
              </span>
              <span
                v-else
                v-tooltip="t('vault.missingHelp')"
                class="tag tag-missing"
                tabindex="0"
                data-testid="vault-missing-tag"
                >{{ t('vault.missingTag') }}</span
              >
            </td>
            <td
              class="font-mono text-xs @max-[60rem]:col-start-2 @max-[60rem]:border-0 @max-[60rem]:p-0"
            >
              <span class="hidden font-sans text-gray-500 @max-[60rem]:inline"
                >{{ t('vault.colPassword') }}{{ t('common.colon') }}</span
              >
              <span :class="profile.has_password ? 'text-gray-300' : 'text-gray-600'">{{
                profile.has_password ? '●●●●●●' : t('vault.none')
              }}</span>
            </td>
            <td class="text-xs @max-[60rem]:col-start-2 @max-[60rem]:border-0 @max-[60rem]:p-0">
              <div class="cell-truncate">
                <span class="text-gray-500 @max-[60rem]:inline hidden"
                  >{{ t('vault.colRecovery') }}{{ t('common.colon') }}</span
                >
                <span
                  v-if="profile.recovery_email"
                  v-tooltip="profile.recovery_email"
                  class="font-mono text-gray-300"
                  >{{ profile.recovery_email }}</span
                >
                <span v-else class="text-gray-600">{{ t('vault.none') }}</span>
              </div>
            </td>
            <td class="text-xs @max-[60rem]:col-start-2 @max-[60rem]:border-0 @max-[60rem]:p-0">
              <span class="hidden text-gray-500 @max-[60rem]:inline"
                >{{ t('vault.colTotp') }}{{ t('common.colon') }}</span
              >
              <template v-if="profile.has_totp">
                <span v-if="profile.totp_ok" class="font-mono text-green-300"
                  >✓ …{{ profile.totp_last4 }}</span
                >
                <span v-else class="text-red-300">{{ t('vault.totpBad') }}</span>
                <span
                  v-if="(profile.totp_flags ?? []).includes('letters_only')"
                  class="block text-[11px] text-amber-300 @max-[60rem]:ml-1 @max-[60rem]:inline"
                  >{{ t('vault.totpUnsure') }}</span
                >
                <span
                  v-if="(profile.totp_flags ?? []).includes('otpauth_params')"
                  class="block text-[11px] text-amber-300 @max-[60rem]:ml-1 @max-[60rem]:inline"
                  >{{ t('vault.totpParams') }}</span
                >
              </template>
              <span v-else class="text-gray-600">{{ t('vault.none') }}</span>
            </td>
            <td class="text-xs @max-[60rem]:col-start-2 @max-[60rem]:border-0 @max-[60rem]:p-0">
              <span class="hidden text-gray-500 @max-[60rem]:inline"
                >{{ t('vault.colNote') }}{{ t('common.colon') }}</span
              >
              <span :class="profile.has_note ? 'text-gray-300' : 'text-gray-600'">{{
                profile.has_note ? t('vault.recorded') : t('vault.none')
              }}</span>
            </td>
            <td v-if="showUpdated" class="text-xs text-gray-500">
              {{ formatShortTime(profile.updated_at) }}
            </td>
            <td
              class="@max-[60rem]:col-start-3 @max-[60rem]:row-start-1 @max-[60rem]:border-0 @max-[60rem]:p-0"
            >
              <div class="flex items-center justify-end gap-1">
                <SecretMenu
                  :email="profile.email"
                  :profile="profile"
                  :membership="familyByEmail.get(profile.email)"
                  variant="button"
                />
                <button
                  class="btn btn-sm px-1.5"
                  type="button"
                  :aria-label="t('common.edit')"
                  :title="t('common.edit')"
                  @click="openProfileEditor(profile.email, 'edit')"
                >
                  <UiIcon name="pencil" :size="12" />
                </button>
                <button
                  class="btn btn-sm btn-danger px-1.5"
                  type="button"
                  :aria-label="t('common.delete')"
                  :title="t('common.delete')"
                  :disabled="working !== ''"
                  data-testid="vault-delete-one"
                  @click="removeOne(profile)"
                >
                  <UiIcon name="close" :size="12" />
                </button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
      <div class="border-t border-line px-3 py-2">
        <UiPager v-model:page="page" v-model:page-size="pageSize" :total="filtered.length" />
      </div>
    </div>
  </div>
</template>
