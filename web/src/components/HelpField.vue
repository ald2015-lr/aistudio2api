<script setup lang="ts">
import { computed, provide, reactive, ref } from 'vue'
import { expandedHelpTexts, helpFieldKey, toggleHelpText, type HelpFieldState } from '@/helpText'
import HelpToggle from './HelpToggle.vue'

// HelpField 包住一个说明在 label 里的设置项：label 里的 HelpText 只显示正文，展开按钮放在 label 之后、与说明文字左对齐。
// 按钮在 label 里时，它的文字会并入输入框、勾选框的可访问名称（读屏软件每次聚焦都读出“展开说明”），
// label 里也不能有它所标注的控件以外的按钮
const root = ref<HTMLElement | null>(null)
const state = reactive<HelpFieldState>({ text: '', id: '', clipped: false, indent: 0 })
provide(helpFieldKey, { state, root: () => root.value })

const expanded = computed(() => state.text !== '' && expandedHelpTexts.has(state.text))
</script>

<template>
  <div ref="root">
    <slot />
    <div
      v-if="state.text !== '' && (state.clipped || expanded)"
      class="text-xs"
      :style="{ paddingLeft: `${state.indent}px` }"
    >
      <HelpToggle :expanded="expanded" :controls="state.id" @toggle="toggleHelpText(state.text)" />
    </div>
  </div>
</template>
