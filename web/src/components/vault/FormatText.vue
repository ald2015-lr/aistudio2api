<script setup lang="ts">
import { computed } from 'vue'

// FormatText 显示格式说明：文字里用反引号标出的片段（如 `----`、`p@ss--word`）显示成不折行、保留空格的等宽片段，
// 免得 ---- 在行尾被拆成“-”和“---”，全角“－－－－”被拆成单个字
const props = defineProps<{ text: string }>()

const parts = computed(() =>
  props.text
    .split('`')
    .map((text, index) => ({ text, code: index % 2 === 1 }))
    .filter((part) => part.text !== ''),
)
</script>

<template>
  <template v-for="(part, index) in parts" :key="index"
    ><code
      v-if="part.code"
      class="rounded bg-raised px-1 font-mono text-[11px] whitespace-pre text-gray-200"
      translate="no"
      >{{ part.text }}</code
    ><template v-else>{{ part.text }}</template></template
  >
</template>
