<script setup lang="ts">
import { computed, inject, onBeforeUnmount, ref, useId, watch, watchEffect } from 'vue'
import { contentIndent, expandedHelpTexts, helpFieldKey, toggleHelpText } from '@/helpText'
import HelpToggle from './HelpToggle.vue'

// HelpText 显示设置项的说明：超过两行的长说明默认折叠成两行，带“展开说明/收起说明”按钮；两行放得下的短说明照常显示、
// 不带按钮。是否超过两行按实际排版判断，宽度变化（窗口缩放、窄屏）后重新判断。说明在设置项的 label 里时用 HelpField 包住
// 那个 label：这里只显示正文，按钮由 HelpField 放到 label 之后（见 HelpField.vue）
const props = defineProps<{ text: string }>()

const field = inject(helpFieldKey, null)
const id = useId()
const body = ref<HTMLElement | null>(null)
// clipped 为折叠时正文被截断（超过两行）；展开期间保留折叠时的判断
const clipped = ref(false)
const expanded = computed(() => expandedHelpTexts.has(props.text))

// measure 在折叠状态下判断正文是否超过两行；在 HelpField 里时同时量正文的缩进（宽度变化时勾选框的宽度也会变）
function measure(): void {
  const element = body.value
  if (element === null) return
  const root = field?.root() ?? null
  if (field !== null && root !== null) field.state.indent = contentIndent(element, root)
  if (expanded.value) return
  clipped.value = element.scrollHeight - element.clientHeight > 1
}

watch(() => props.text, measure, { flush: 'post' })
watch(expanded, measure, { flush: 'post' })
watch(body, (element, _previous, onCleanup) => {
  if (element === null) return
  measure()
  if (typeof ResizeObserver === 'undefined') return
  const observer = new ResizeObserver(measure)
  observer.observe(element)
  onCleanup(() => observer.disconnect())
})

// 在 HelpField 里时把正文的状态交给它显示按钮
if (field !== null) {
  const state = field.state
  watchEffect(() => {
    state.text = props.text
    state.id = id
    state.clipped = clipped.value
  })
  onBeforeUnmount(() => {
    state.text = ''
    state.clipped = false
  })
}
</script>

<template>
  <span class="block">
    <!-- line-clamp 自带 display:-webkit-box，折叠时不能再加 block（同层的 block 会覆盖它，截断失效） -->
    <span :id="id" ref="body" class="break-words" :class="expanded ? 'block' : 'line-clamp-2'">{{
      text
    }}</span>
    <HelpToggle
      v-if="field === null && (clipped || expanded)"
      :expanded="expanded"
      :controls="id"
      @toggle="toggleHelpText(text)"
    />
  </span>
</template>
