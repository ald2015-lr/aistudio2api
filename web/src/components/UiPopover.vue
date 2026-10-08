<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'

// UiPopover 是点击触发器展开的浮层：挂到 body 下按触发器位置显示，点击外部、Esc、滚动或缩放窗口时关闭
const props = withDefaults(
  defineProps<{
    align?: 'start' | 'end'
    triggerClass?: string
    contentClass?: string
    label?: string
  }>(),
  { align: 'start', triggerClass: '', contentClass: '', label: '' },
)
const open = defineModel<boolean>('open', { default: false })

const trigger = ref<HTMLButtonElement>()
const content = ref<HTMLDivElement>()
const position = ref({ top: 0, left: 0 })
const contentId = `ui-popover-${Math.random().toString(36).slice(2)}`

// place 按触发器位置计算浮层，下方空间不足时向上展开，左右不超出窗口
function place(): void {
  const rect = trigger.value?.getBoundingClientRect()
  if (!rect) return
  const width = content.value?.offsetWidth ?? 0
  const height = content.value?.offsetHeight ?? 0
  const preferred = props.align === 'end' ? rect.right - width : rect.left
  const left = Math.max(8, Math.min(preferred, window.innerWidth - width - 8))
  const below = rect.bottom + 6
  const upward = below + height > window.innerHeight - 8 && rect.top - height - 6 > 8
  position.value = { top: upward ? rect.top - height - 6 : below, left }
}

function close(): void {
  open.value = false
}

function onOutside(event: PointerEvent): void {
  const target = event.target as Node
  if (!trigger.value?.contains(target) && !content.value?.contains(target)) close()
}

function onViewportChange(event: Event): void {
  if (event.type === 'scroll' && content.value?.contains(event.target as Node)) return
  close()
}

function onKeydown(event: KeyboardEvent): void {
  if (event.key !== 'Escape') return
  event.preventDefault()
  close()
  trigger.value?.focus()
}

function listen(active: boolean): void {
  if (active) {
    document.addEventListener('pointerdown', onOutside, true)
    document.addEventListener('keydown', onKeydown)
    window.addEventListener('scroll', onViewportChange, true)
    window.addEventListener('resize', onViewportChange)
    return
  }
  document.removeEventListener('pointerdown', onOutside, true)
  document.removeEventListener('keydown', onKeydown)
  window.removeEventListener('scroll', onViewportChange, true)
  window.removeEventListener('resize', onViewportChange)
}

watch(open, async (value) => {
  listen(value)
  if (!value) return
  place()
  await nextTick()
  place()
})

onBeforeUnmount(() => listen(false))
</script>

<template>
  <button
    ref="trigger"
    type="button"
    :class="triggerClass"
    :aria-expanded="open"
    :aria-controls="contentId"
    :aria-label="label || undefined"
    aria-haspopup="dialog"
    :data-state="open ? 'open' : 'closed'"
    @click="open = !open"
  >
    <slot name="trigger" :open="open" />
  </button>
  <Teleport to="body">
    <div
      v-if="open"
      :id="contentId"
      ref="content"
      role="dialog"
      :aria-label="label || undefined"
      class="ui-popover fixed z-[60] rounded-lg border border-line bg-panel shadow-xl shadow-black/40"
      :class="contentClass"
      :style="{ top: `${position.top}px`, left: `${position.left}px` }"
    >
      <slot :close="close" />
    </div>
  </Teleport>
</template>
