<script setup lang="ts">
import { computed, ref, shallowRef, watch } from 'vue'
import { api } from '@/api'
import { stateDotClass, stateKeys } from '@/accountState'
import { copyText } from '@/clipboard'
import { confirmAction } from '@/confirm'
import { useI18n } from '@/i18n'
import type { Account, FamilyGroup } from '@/types'
import {
  famClass,
  familiesBySeq,
  groupEmails,
  groupMembers,
  groupSize,
  openAccountsView,
  openManualCopy,
  vaultErrorText,
  vaultNotice,
  vaultWrite,
} from '@/vault'
import FamilyEditor from './FamilyEditor.vue'
import UiIcon from '../UiIcon.vue'
import UiPager from '../UiPager.vue'
import UiPopover from '../UiPopover.vue'

// FamilyCards 为家庭组卡片：按创建顺序、每页 24 张；卡片顶边为组色，主邮箱带皇冠与“主”标识，每个邮箱显示在账户列表里的状态点
// 或“未存在”，可复制。菜单：编辑、在账户页查看本组、解散；成员：设为主邮箱、移出（移出最后一个成员 = 解散，要确认）
const props = defineProps<{
  byEmail: ReadonlyMap<string, Account[]>
  accountsLoading: boolean
}>()
const emit = defineEmits<{ import: [] }>()

const { t, tf } = useI18n()
const PAGE_SIZE = 24

type CardFilter = 'all' | 'missing' | 'over'
const filter = ref<CardFilter>('all')
const query = ref('')
const page = ref(1)
const pageSize = ref(PAGE_SIZE)
const editing = shallowRef<FamilyGroup | 'new' | null>(null)
const working = ref('')
const menuOpen = ref('')

function missingOf(group: FamilyGroup): number {
  if (props.accountsLoading) return 0
  return groupEmails(group).filter((email) => !props.byEmail.has(email)).length
}

const counts = computed(() => {
  const result = { all: 0, missing: 0, over: 0 }
  for (const group of familiesBySeq.value) {
    result.all += 1
    if (missingOf(group) > 0) result.missing += 1
    if (groupSize(group) > 6) result.over += 1
  }
  return result
})

const filtered = computed(() => {
  const term = query.value.trim().toLowerCase()
  return familiesBySeq.value.filter((group) => {
    if (filter.value === 'missing' && missingOf(group) === 0) return false
    if (filter.value === 'over' && groupSize(group) <= 6) return false
    if (term === '') return true
    return (
      (group.name ?? '').toLowerCase().includes(term) ||
      groupEmails(group).some((email) => email.includes(term))
    )
  })
})

const pageCards = computed(() =>
  filtered.value.slice((page.value - 1) * PAGE_SIZE, page.value * PAGE_SIZE),
)
watch([filter, query], () => {
  page.value = 1
})
watch(
  () => filtered.value.length,
  (count) => {
    const last = Math.max(1, Math.ceil(count / PAGE_SIZE))
    if (page.value > last) page.value = last
  },
)

function statusOf(email: string): { dot: string; label: string } | null {
  const accounts = props.byEmail.get(email)
  if (accounts === undefined || accounts.length === 0) return null
  const first = accounts[0]!
  return {
    dot: stateDotClass[first.state],
    label: t(stateKeys[first.state]) + (accounts.length > 1 ? ` ×${accounts.length}` : ''),
  }
}

async function copy(text: string, what: string): Promise<void> {
  if (await copyText(text)) vaultNotice(tf('vault.copied', { what }))
  else openManualCopy(text, what)
}

function copyAll(group: FamilyGroup): void {
  void copy(groupEmails(group).join('\n'), tf('family.copyAllWhat', { count: groupSize(group) }))
}

async function run(key: string, work: () => Promise<unknown>, done: string): Promise<void> {
  working.value = key
  try {
    await vaultWrite(work())
    vaultNotice(done)
  } catch (error) {
    vaultNotice(vaultErrorText(error), 'error')
  } finally {
    working.value = ''
  }
}

async function dissolve(group: FamilyGroup): Promise<void> {
  menuOpen.value = ''
  const message = tf('family.dissolveConfirm', {
    group: group.name || group.primary,
    count: groupSize(group),
  })
  if (!(await confirmAction(message, t('family.dissolve')))) return
  await run(
    `dissolve:${group.id}`,
    () => api.dissolveFamily(group.id, group.rev),
    t('family.dissolvedNotice'),
  )
}

async function setPrimary(group: FamilyGroup, email: string): Promise<void> {
  menuOpen.value = ''
  const members = [group.primary, ...groupMembers(group).filter((member) => member !== email)]
  await run(
    `primary:${group.id}`,
    () => api.updateFamily(group.id, { dry_run: false, rev: group.rev, primary: email, members }),
    tf('family.primarySet', { email }),
  )
}

async function removeMember(group: FamilyGroup, email: string): Promise<void> {
  menuOpen.value = ''
  const rest = groupMembers(group).filter((member) => member !== email)
  if (rest.length === 0) {
    if (!(await confirmAction(t('family.lastMemberConfirm'), t('family.dissolve')))) return
    await run(
      `dissolve:${group.id}`,
      () => api.dissolveFamily(group.id, group.rev),
      t('family.dissolvedNotice'),
    )
    return
  }
  const message = tf('family.removeConfirm', { email, group: group.name || group.primary })
  if (!(await confirmAction(message, t('family.removeMember')))) return
  await run(
    `remove:${group.id}`,
    () => api.updateFamily(group.id, { dry_run: false, rev: group.rev, members: rest }),
    tf('family.removed', { email }),
  )
}

function viewInAccounts(group: FamilyGroup): void {
  menuOpen.value = ''
  openAccountsView({ family: group.id })
}

const filterOptions = computed(() => [
  { value: 'all' as const, label: t('family.filterAll'), count: counts.value.all },
  { value: 'missing' as const, label: t('family.filterMissing'), count: counts.value.missing },
  { value: 'over' as const, label: t('family.filterOverSix'), count: counts.value.over },
])
</script>

<template>
  <div class="space-y-3">
    <div class="flex flex-wrap items-center gap-2">
      <input
        v-model="query"
        class="input min-w-40 flex-1 sm:max-w-80"
        type="search"
        :placeholder="t('family.search')"
        :aria-label="t('family.search')"
        autocomplete="off"
        spellcheck="false"
      />
      <div class="seg">
        <button
          v-for="option in filterOptions"
          :key="option.value"
          class="seg-item"
          :class="{ active: filter === option.value }"
          type="button"
          :data-filter="option.value"
          @click="filter = option.value"
        >
          {{ option.label }}
          <span class="seg-count">{{ option.count }}</span>
        </button>
      </div>
      <span class="flex-1"></span>
      <button class="btn" type="button" @click="emit('import')">
        <UiIcon name="upload" :size="14" />
        {{ t('vault.tabFamilyImport') }}
      </button>
      <button class="btn btn-primary" type="button" @click="editing = 'new'">
        <UiIcon name="plusLine" :size="14" />
        {{ t('family.create') }}
      </button>
    </div>

    <div
      v-if="familiesBySeq.length === 0"
      class="panel px-6 py-10 text-center text-sm text-gray-500"
    >
      <p class="mb-2">{{ t('family.empty') }}</p>
      <p class="text-xs">{{ t('family.emptyHint') }}</p>
    </div>
    <p v-else-if="filtered.length === 0" class="panel px-6 py-8 text-center text-sm text-gray-500">
      {{ t('family.noMatch') }}
    </p>
    <div v-else class="grid grid-cols-1 gap-3 md:grid-cols-2 2xl:grid-cols-3">
      <article
        v-for="group in pageCards"
        :key="group.id"
        class="panel fam-top flex min-w-0 flex-col"
        :class="famClass(group.color)"
        data-testid="family-card"
        :data-group="group.id"
      >
        <header class="flex items-center gap-2 border-b border-line px-3 pt-3 pb-2">
          <span class="fam-dot h-2.5 w-2.5"></span>
          <h4 class="cell-truncate min-w-0 flex-1 text-sm font-semibold text-gray-100">
            <span v-if="group.name">{{ group.name }}</span>
            <span v-else class="font-normal text-gray-500">{{ t('family.unnamed') }}</span>
          </h4>
          <span
            class="shrink-0 text-xs"
            :class="groupSize(group) > 6 ? 'text-amber-300' : 'text-gray-400'"
            >{{ tf('family.people', { count: groupSize(group) }) }}</span
          >
          <span v-if="missingOf(group) > 0" class="tag tag-missing shrink-0">{{
            tf('accounts.familyHeadMissing', { count: missingOf(group) })
          }}</span>
          <UiPopover
            :open="menuOpen === `card:${group.id}`"
            :label="t('family.menu')"
            trigger-class="rounded p-1 text-gray-400 hover:bg-raised hover:text-white"
            content-class="min-w-44 p-1"
            align="end"
            @update:open="menuOpen = $event ? `card:${group.id}` : ''"
          >
            <template #trigger><UiIcon name="dots" :size="14" /></template>
            <div class="flex flex-col text-xs">
              <button
                class="rounded px-2 py-1.5 text-left text-gray-200 hover:bg-raised"
                type="button"
                @click="((menuOpen = ''), (editing = group))"
              >
                {{ t('family.edit') }}
              </button>
              <button
                class="rounded px-2 py-1.5 text-left text-gray-200 hover:bg-raised"
                type="button"
                data-testid="family-view-accounts"
                @click="viewInAccounts(group)"
              >
                {{ t('family.viewInAccounts') }}
              </button>
              <button
                class="rounded px-2 py-1.5 text-left text-red-300 hover:bg-raised"
                type="button"
                data-testid="family-dissolve"
                @click="dissolve(group)"
              >
                {{ t('family.dissolve') }}
              </button>
            </div>
          </UiPopover>
        </header>
        <ul class="flex-1 space-y-0.5 px-2 py-2 text-xs">
          <li
            v-for="(email, index) in groupEmails(group)"
            :key="email"
            class="group/row flex min-w-0 items-center gap-1.5 rounded px-1 py-1 hover:bg-raised/60"
            :data-email="email"
          >
            <span class="tag tag-fam shrink-0" :class="index === 0 ? '' : 'tag-fam-member'">
              <UiIcon v-if="index === 0" name="crown" :size="11" />
              {{ t(index === 0 ? 'family.primary' : 'family.member') }}</span
            >
            <span
              class="cell-truncate min-w-0 flex-1 font-mono"
              :class="index === 0 ? 'text-gray-100' : 'text-gray-300'"
              >{{ email }}</span
            >
            <span v-if="accountsLoading" class="text-gray-600">…</span>
            <span
              v-else-if="statusOf(email)"
              v-tooltip="statusOf(email)?.label"
              class="dot"
              :class="statusOf(email)?.dot"
              tabindex="0"
              :aria-label="statusOf(email)?.label"
            ></span>
            <span v-else v-tooltip="t('family.missingHelp')" class="tag tag-missing" tabindex="0">{{
              t('vault.missingTag')
            }}</span>
            <button
              class="shrink-0 rounded p-0.5 text-gray-500 hover:bg-raised hover:text-gray-200"
              type="button"
              :aria-label="t('vault.copyEmail')"
              @click="copy(email, t('vault.fieldEmail'))"
            >
              <UiIcon name="copy" :size="12" />
            </button>
            <UiPopover
              v-if="index > 0"
              :open="menuOpen === `member:${group.id}:${email}`"
              :label="t('family.memberMenu')"
              trigger-class="shrink-0 rounded p-0.5 text-gray-500 hover:bg-raised hover:text-gray-200"
              content-class="min-w-40 p-1"
              align="end"
              @update:open="menuOpen = $event ? `member:${group.id}:${email}` : ''"
            >
              <template #trigger><UiIcon name="dots" :size="12" /></template>
              <div class="flex flex-col text-xs">
                <button
                  class="rounded px-2 py-1.5 text-left text-gray-200 hover:bg-raised"
                  type="button"
                  :disabled="working !== ''"
                  @click="setPrimary(group, email)"
                >
                  {{ t('family.setPrimary') }}
                </button>
                <button
                  class="rounded px-2 py-1.5 text-left text-red-300 hover:bg-raised"
                  type="button"
                  :disabled="working !== ''"
                  data-testid="family-remove-member"
                  @click="removeMember(group, email)"
                >
                  {{ t('family.removeMember') }}
                </button>
              </div>
            </UiPopover>
            <span v-else class="w-[18px] shrink-0"></span>
          </li>
          <li v-if="groupMembers(group).length === 0" class="px-1 py-1 text-gray-500">
            {{ t('family.onlyPrimary') }}
          </li>
        </ul>
        <footer class="flex items-center justify-between gap-2 border-t border-line px-3 py-2">
          <span class="font-mono text-[11px] text-gray-600">#{{ group.seq }}</span>
          <button class="btn btn-sm" type="button" @click="copyAll(group)">
            <UiIcon name="copy" :size="12" />
            {{ t('family.copyAll') }}
          </button>
        </footer>
      </article>
    </div>
    <div v-if="filtered.length > PAGE_SIZE" class="panel px-3 py-2">
      <UiPager
        v-model:page="page"
        v-model:page-size="pageSize"
        :total="filtered.length"
        fixed-size
      />
    </div>
    <FamilyEditor
      v-if="editing !== null"
      :group="editing === 'new' ? undefined : editing"
      @close="editing = null"
    />
  </div>
</template>
