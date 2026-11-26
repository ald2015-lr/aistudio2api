<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from '@/i18n'
import { famClass, groupSize, type FamilyMembership } from '@/vault'
import UiIcon from '../UiIcon.vue'

// FamilyBadge 为家庭组的“主/子”徽标：颜色取组的色板编号（fam-c{n}），主邮箱带皇冠图标；悬停说明写组名、主邮箱、人数与其中
// 不在账户里的个数（missing 由调用方按账户列表算好传入，不知道时不写）。“主/子”文字本身能区分身份，不只靠颜色
const props = defineProps<{
  membership: FamilyMembership
  missing?: number | undefined
}>()

const { t, tf } = useI18n()

const tooltip = computed(() => {
  const group = props.membership.group
  const missing = props.missing ?? 0
  return tf('family.tooltip', {
    name: group.name || t('family.unnamed'),
    primary: group.primary,
    count: groupSize(group),
    missing: missing > 0 ? tf('family.tooltipMissing', { count: missing }) : '',
  })
})
</script>

<template>
  <span
    v-tooltip="tooltip"
    class="tag tag-fam shrink-0"
    :class="[famClass(membership.group.color), { 'tag-fam-member': membership.role === 'member' }]"
    tabindex="0"
    :aria-label="t(membership.role === 'primary' ? 'family.primaryAria' : 'family.memberAria')"
    :data-family-role="membership.role"
    data-testid="family-badge"
  >
    <UiIcon v-if="membership.role === 'primary'" name="crown" :size="11" />
    {{ t(membership.role === 'primary' ? 'family.primary' : 'family.member') }}
  </span>
</template>
