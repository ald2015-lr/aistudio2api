<script setup lang="ts">
import { computed } from 'vue'
import { useI18n, type TranslationKey } from '@/i18n'
import type { FamilyBlockResult } from '@/types'
import { famClass, groupById } from '@/vault'

// FamilyChanges 显示一块（或一次新建、编辑）的结果：状态、拒绝或无法识别的原因与处理办法、增减的成员、从别的组移入的邮箱、
// 将自动解散的组、提示。录入预览与家庭组编辑框共用。邮箱不是秘密，原样列出
// omitWarnings 为不显示的提示（新建框落到已有家庭组时自己说明名称不采用，不再显示录入用的 name_ignored）
const props = withDefaults(defineProps<{ result: FamilyBlockResult; omitWarnings?: string[] }>(), {
  omitWarnings: () => [],
})

const { t, tf } = useI18n()

type FamilyStatus =
  'created' | 'replaced' | 'primary_changed' | 'updated' | 'unchanged' | 'rejected' | 'invalid'

const statusKeys: Record<FamilyStatus, TranslationKey> = {
  created: 'family.status.created',
  replaced: 'family.status.replaced',
  primary_changed: 'family.status.primary_changed',
  updated: 'family.status.updated',
  unchanged: 'family.status.unchanged',
  rejected: 'family.status.rejected',
  invalid: 'family.status.invalid',
}

const statusClass: Record<FamilyStatus, string> = {
  created: 'border-green-500/50 text-green-300',
  replaced: 'border-blue-500/50 text-blue-300',
  primary_changed: 'border-fuchsia-500/50 text-fuchsia-300',
  updated: 'border-blue-500/50 text-blue-300',
  unchanged: 'border-line text-gray-400',
  rejected: 'border-red-500/50 text-red-300',
  invalid: 'border-red-500/50 text-red-300',
}

const reasonKeys: Record<string, TranslationKey> = {
  single_email: 'family.reason.single_email',
  block_too_large: 'family.reason.block_too_large',
  token_not_email: 'family.reason.token_not_email',
  looks_like_profile: 'family.reason.looks_like_profile',
  member_is_primary: 'family.reason.member_is_primary',
  primary_is_other_primary: 'family.reason.primary_is_other_primary',
  limit_groups: 'family.reason.limit_groups',
  ambiguous_block: 'family.reason.ambiguous_block',
}

const warnKeys: Record<string, TranslationKey> = {
  over_five: 'family.warn.over_five',
  duplicate_in_block: 'family.warn.duplicate_in_block',
  primary_in_members: 'family.warn.primary_in_members',
  maybe_missing_blank: 'family.warn.maybe_missing_blank',
  name_ignored: 'family.warn.name_ignored',
  comment_split: 'family.warn.comment_split',
}

const status = computed(() => props.result.status as FamilyStatus)
const warnings = computed(() =>
  (props.result.warnings ?? []).filter((code) => !props.omitWarnings.includes(code)),
)

function groupLabel(id: string, primary: string, name?: string): string {
  const group = groupById.value.get(id)
  const groupName = name || group?.name || ''
  return groupName ? tf('family.groupNamed', { name: groupName, primary }) : primary
}

const reasonText = computed(() => {
  const result = props.result
  if (result.reason === undefined || result.reason === '') return ''
  const key = reasonKeys[result.reason]
  if (key === undefined) return result.reason
  const detail = result.detail ?? {}
  const primary = detail.group_primary ?? ''
  return tf(key, {
    email: detail.email ?? '',
    group: detail.group_name
      ? tf('family.groupNamed', { name: detail.group_name, primary })
      : primary,
    primary,
    line: detail.line ?? result.first_line ?? '',
    position: detail.position ?? '',
    count: (result.members?.length ?? 0) + (result.primary ? 1 : 0),
  })
})

const rejectHint = computed(() => {
  const reason = props.result.reason
  if (reason === 'member_is_primary') return t('family.rejectHint.member_is_primary')
  if (reason === 'primary_is_other_primary') return t('family.rejectHint.primary_is_other_primary')
  return ''
})

function warnText(code: string): string {
  const key = warnKeys[code]
  if (key === undefined) return code
  const size = (props.result.members?.length ?? 0) + 1
  return tf(key, {
    line: props.result.comment_line ?? '',
    count: size,
    groups: Math.max(1, Math.round(size / 6)),
  })
}
</script>

<template>
  <div class="space-y-1.5 text-xs" data-testid="family-changes" :data-status="result.status">
    <div class="flex flex-wrap items-center gap-1.5">
      <span class="tag font-sans" :class="statusClass[status] ?? 'text-gray-300'">{{
        t(statusKeys[status] ?? 'family.status.unchanged')
      }}</span>
      <span v-if="result.renamed" class="text-gray-400">{{ t('family.renamed') }}</span>
      <span v-if="result.recolored" class="text-gray-400">{{ t('family.recolored') }}</span>
    </div>
    <p v-if="reasonText" class="text-red-300" data-testid="family-reason">{{ reasonText }}</p>
    <p v-if="rejectHint" class="text-gray-400">{{ rejectHint }}</p>
    <p v-if="result.hint === 'looks_like_name'" class="text-amber-300">
      {{ t('family.hint.looks_like_name') }}
    </p>
    <div
      v-if="(result.added?.length ?? 0) > 0 || (result.removed?.length ?? 0) > 0"
      class="flex flex-wrap gap-1"
    >
      <span
        v-for="email in result.added ?? []"
        :key="`+${email}`"
        class="tag border-green-500/40 text-green-300"
        >+{{ email }}</span
      >
      <span
        v-for="email in result.removed ?? []"
        :key="`-${email}`"
        v-tooltip="t('family.removedHelp')"
        class="tag border-red-500/40 text-red-300"
        >−{{ email }}</span
      >
    </div>
    <ul v-if="(result.moved?.length ?? 0) > 0" class="space-y-0.5 text-gray-300">
      <li v-for="move in result.moved ?? []" :key="move.email">
        {{
          tf('family.moved', {
            email: move.email,
            group: groupLabel(move.from_group, move.from_primary),
          })
        }}
      </li>
    </ul>
    <ul v-if="(result.dissolved?.length ?? 0) > 0" class="space-y-0.5">
      <li
        v-for="group in result.dissolved ?? []"
        :key="group.group_id"
        class="flex items-center gap-1.5 text-amber-300"
        :class="famClass(group.color)"
      >
        <span class="fam-dot"></span>
        {{
          tf('family.dissolved', { group: groupLabel(group.group_id, group.primary, group.name) })
        }}
      </li>
    </ul>
    <ul v-if="warnings.length > 0" class="space-y-0.5 text-amber-300">
      <li v-for="code in warnings" :key="code">{{ warnText(code) }}</li>
    </ul>
  </div>
</template>
