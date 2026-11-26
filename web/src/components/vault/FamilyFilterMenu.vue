<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { useI18n, type TranslationKey } from '@/i18n'
import type { FamilyGroup } from '@/types'
import {
  famClass,
  familiesBySeq,
  familyFilterGroup,
  groupById,
  groupSize,
  vaultData,
  type FamilyFilter,
} from '@/vault'
import UiIcon from '../UiIcon.vue'
import UiPopover from '../UiPopover.vue'

// FamilyFilterMenu 为家庭组下拉（账户页筛选栏、资料列表共用）：触发按钮与 select.input 同高同边框；弹层里是固定选项（带计数）与
// “按组选择”（搜索主邮箱或名称，色点 + 名称 · 主邮箱 + 计数 + 人数，最多渲染 100 项）。原生 select 与 UiSelect 画不了色点，
// 几百个组也要能搜索，所以自绘。键盘：↑↓ 移动、Enter 选中、Esc 关闭
type Mode = 'all' | 'grouped' | 'in' | 'primary' | 'member' | 'none'

const props = withDefaults(
  defineProps<{
    // counts 为各固定选项的计数（与权益、号池下拉同口径：对全部行计，不受其他筛选影响）；groupCounts 为每个组的行数
    counts: Record<Exclude<Mode, 'grouped'>, number>
    groupCounts: ReadonlyMap<string, number>
    // grouped 为有“按家庭组显示”这一项（账户页）；kind 决定“仅家庭组账户/邮箱”的说法
    grouped?: boolean
    kind?: 'accounts' | 'profiles'
    // unavailable 为家庭组文件出错或拉取失败时的说明（下拉里灰字显示，计数为空）
    unavailable?: string
    triggerClass?: string
  }>(),
  { grouped: false, kind: 'accounts', unavailable: '', triggerClass: 'input input-trigger' },
)
const model = defineModel<FamilyFilter>({ required: true })

const GROUP_LIMIT = 100
const { t, tf } = useI18n()
const open = ref(false)
const term = ref('')
const active = ref(0)
const search = ref<HTMLInputElement | null>(null)

const modeLabels = computed<Record<Mode, TranslationKey>>(() => ({
  all: 'accounts.familyFilterAll',
  grouped: 'accounts.familyGrouped',
  in: props.kind === 'accounts' ? 'accounts.familyInFamily' : 'vault.familyInFamily',
  primary: 'accounts.familyPrimary',
  member: 'accounts.familyMember',
  none: 'accounts.familyNone',
}))

const modes = computed<Mode[]>(() =>
  props.grouped
    ? ['all', 'grouped', 'in', 'primary', 'member', 'none']
    : ['all', 'in', 'primary', 'member', 'none'],
)

function modeCount(mode: Mode): number {
  return mode === 'grouped' ? props.counts.all : props.counts[mode]
}

const matchedGroups = computed(() => {
  const query = term.value.trim().toLowerCase()
  const list =
    query === ''
      ? familiesBySeq.value
      : familiesBySeq.value.filter(
          (group) =>
            group.primary.includes(query) || (group.name ?? '').toLowerCase().includes(query),
        )
  return { total: list.length, shown: list.slice(0, GROUP_LIMIT) }
})

type Item = { kind: 'mode'; mode: Mode } | { kind: 'group'; group: FamilyGroup }
const items = computed<Item[]>(() => [
  ...modes.value.map((mode) => ({ kind: 'mode' as const, mode })),
  ...matchedGroups.value.shown.map((group) => ({ kind: 'group' as const, group })),
])

// currentMode 为当前选中的固定选项（选中某个组时不用）
const currentMode = computed<Mode>(() =>
  (modes.value as string[]).includes(model.value) ? (model.value as Mode) : 'all',
)

const selectedGroup = computed(() => {
  const id = familyFilterGroup(model.value)
  return id === '' ? undefined : groupById.value.get(id)
})

// 选中的组在别处被解散（或在别的页签、别的进程里删掉）后：家庭组数据已载入时把筛选置回“全部”，
// 免得下拉显示“全部”、列表却按一个不存在的组筛成空的。家庭组文件出错时组也查不到，同样回到“全部”（下拉里有灰字说明）
watch(
  [model, groupById],
  () => {
    const id = familyFilterGroup(model.value)
    if (id !== '' && vaultData.value !== null && !groupById.value.has(id)) model.value = 'all'
  },
  { immediate: true },
)

function select(item: Item | undefined): void {
  if (item === undefined) return
  model.value = item.kind === 'mode' ? item.mode : `group:${item.group.id}`
  open.value = false
}

function isSelected(item: Item): boolean {
  return item.kind === 'mode' ? model.value === item.mode : model.value === `group:${item.group.id}`
}

function onKeydown(event: KeyboardEvent): void {
  const count = items.value.length
  if (count === 0) return
  if (event.key === 'ArrowDown') {
    event.preventDefault()
    active.value = (active.value + 1) % count
  } else if (event.key === 'ArrowUp') {
    event.preventDefault()
    active.value = (active.value - 1 + count) % count
  } else if (event.key === 'Enter') {
    event.preventDefault()
    select(items.value[active.value])
  } else {
    return
  }
  void nextTick(() => {
    document
      .querySelector(`[data-family-option="${active.value}"]`)
      ?.scrollIntoView({ block: 'nearest' })
  })
}

watch(open, async (value) => {
  if (!value) return
  term.value = ''
  const index = items.value.findIndex(isSelected)
  active.value = index < 0 ? 0 : index
  await nextTick()
  search.value?.focus()
})

watch(term, () => {
  active.value = modes.value.length
})
</script>

<template>
  <UiPopover
    v-model:open="open"
    :label="t('accounts.familyFilter')"
    :trigger-class="triggerClass"
    content-class="w-80 max-w-[calc(100vw-16px)] p-1.5"
  >
    <template #trigger>
      <span class="flex min-w-0 flex-1 items-center gap-1.5" data-testid="family-filter-trigger">
        <template v-if="selectedGroup">
          <span class="fam-dot" :class="famClass(selectedGroup.color)"></span>
          <span class="cell-truncate">{{ selectedGroup.name || selectedGroup.primary }}</span>
          <span class="shrink-0 text-gray-400">({{ groupCounts.get(selectedGroup.id) ?? 0 }})</span>
        </template>
        <span v-else class="cell-truncate"
          >{{ t(modeLabels[currentMode]) }}
          <span v-if="unavailable === ''" class="text-gray-400"
            >({{ modeCount(currentMode) }})</span
          ></span
        >
      </span>
      <UiIcon name="chevronDownSm" :size="12" class="ml-auto" />
    </template>
    <div data-testid="family-filter-menu" class="text-xs" @keydown="onKeydown">
      <p v-if="unavailable" class="px-2 py-1.5 text-gray-500">{{ unavailable }}</p>
      <div role="listbox" :aria-label="t('accounts.familyFilter')">
        <button
          v-for="(mode, index) in modes"
          :key="mode"
          type="button"
          role="option"
          class="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left transition"
          :class="[
            index === active ? 'bg-raised text-white' : 'text-gray-300 hover:bg-raised',
            isSelected({ kind: 'mode', mode }) ? 'font-semibold text-blue-300' : '',
          ]"
          :aria-selected="isSelected({ kind: 'mode', mode })"
          :data-family-option="index"
          :data-mode="mode"
          @mouseenter="active = index"
          @click="select({ kind: 'mode', mode })"
        >
          <UiIcon v-if="mode === 'grouped'" name="users" :size="12" class="text-gray-400" />
          <span class="flex-1">{{ t(modeLabels[mode]) }}</span>
          <span v-if="unavailable === ''" class="font-mono text-gray-500">{{
            modeCount(mode)
          }}</span>
        </button>
      </div>
      <template v-if="unavailable === '' && familiesBySeq.length > 0">
        <div class="mx-1 my-1.5 flex items-center gap-2 text-[11px] text-gray-500">
          <span class="h-px flex-1 bg-line"></span>
          {{ t('accounts.familyPick') }}
          <span class="h-px flex-1 bg-line"></span>
        </div>
        <input
          ref="search"
          v-model="term"
          class="input mb-1 h-7 w-full text-xs"
          type="search"
          :placeholder="t('accounts.familySearch')"
          :aria-label="t('accounts.familySearch')"
          autocomplete="off"
          spellcheck="false"
        />
        <div class="max-h-64 overflow-y-auto" role="listbox" :aria-label="t('accounts.familyPick')">
          <button
            v-for="(group, offset) in matchedGroups.shown"
            :key="group.id"
            type="button"
            role="option"
            class="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left transition"
            :class="[
              offset + modes.length === active
                ? 'bg-raised text-white'
                : 'text-gray-300 hover:bg-raised',
              famClass(group.color),
            ]"
            :aria-selected="isSelected({ kind: 'group', group })"
            :data-family-option="offset + modes.length"
            :data-group="group.id"
            @mouseenter="active = offset + modes.length"
            @click="select({ kind: 'group', group })"
          >
            <span class="fam-dot"></span>
            <span class="cell-truncate min-w-0 flex-1">
              <span v-if="group.name" class="text-gray-100">{{ group.name }} · </span>
              <span class="font-mono">{{ group.primary }}</span>
            </span>
            <span class="font-mono text-gray-500">({{ groupCounts.get(group.id) ?? 0 }})</span>
            <span
              class="shrink-0 text-[11px]"
              :class="groupSize(group) > 6 ? 'text-amber-300' : 'text-gray-500'"
              >{{ tf('family.people', { count: groupSize(group) }) }}</span
            >
          </button>
          <p v-if="matchedGroups.total === 0" class="px-2 py-1.5 text-gray-500">
            {{ t('accounts.familyNoMatch') }}
          </p>
          <p v-else-if="matchedGroups.total > GROUP_LIMIT" class="px-2 py-1.5 text-gray-500">
            {{ tf('accounts.familyMore', { count: matchedGroups.total }) }}
          </p>
        </div>
      </template>
    </div>
  </UiPopover>
</template>
