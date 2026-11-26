<script setup lang="ts">
import { useI18n } from '@/i18n'
import { PALETTE_SIZE, famClass } from '@/vault'

// ColorSwatches 为家庭组的 12 色选择：每个颜色显示正在用它的组数；与上一组 / 下一组（按创建顺序相邻，“按家庭组显示”里紧挨着）
// 相同的颜色加小圆环与说明，帮助避开相邻撞色。颜色只经类名 fam-c{编号} 取
const props = defineProps<{
  usage: readonly number[]
  prevColor?: number | undefined
  nextColor?: number | undefined
}>()
const model = defineModel<number>({ required: true })

const { t, tf } = useI18n()
const colors = Array.from({ length: PALETTE_SIZE }, (_, index) => index)

function note(color: number): string {
  const parts = [tf('family.colorUsed', { count: props.usage[color] ?? 0 })]
  if (color === props.prevColor) parts.push(t('family.colorPrev'))
  if (color === props.nextColor) parts.push(t('family.colorNext'))
  return parts.join(' · ')
}
</script>

<template>
  <div class="grid grid-cols-6 gap-1.5" role="radiogroup" :aria-label="t('family.color')">
    <button
      v-for="color in colors"
      :key="color"
      v-tooltip="note(color)"
      type="button"
      role="radio"
      class="relative flex h-9 flex-col items-center justify-center gap-0.5 rounded-md border transition"
      :class="[
        famClass(color),
        model === color
          ? 'border-[var(--fam)] bg-[color-mix(in_srgb,var(--fam-base)_18%,transparent)]'
          : 'border-line hover:border-line-strong',
      ]"
      :aria-checked="model === color"
      :aria-label="note(color)"
      :data-color="color"
      @click="model = color"
    >
      <span
        class="fam-dot h-3.5 w-3.5"
        :class="
          color === prevColor || color === nextColor
            ? 'ring-2 ring-white/70 ring-offset-1 ring-offset-[#161b22]'
            : ''
        "
      ></span>
      <span class="font-mono text-[10px] leading-none text-gray-500">{{ usage[color] ?? 0 }}</span>
    </button>
  </div>
</template>
