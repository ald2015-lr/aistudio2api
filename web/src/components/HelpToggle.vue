<script setup lang="ts">
import { useI18n } from '@/i18n'

// HelpToggle 是设置项说明的“展开说明/收起说明”按钮（HelpText 与 HelpField 共用）。说明常在设置项的 label 附近，
// 点击时阻止默认动作，不会因此勾选或聚焦输入框。按下鼠标时也阻止默认动作：说明常紧挨着失焦保存的输入框
// （代理、本地 API 密钥、媒体链接地址等），点按钮时输入框不失焦，不会把输入了一半的值先保存（保存后可能自动重启生效）；
// 键盘用 Tab 移到按钮上时照常失焦，与切到下一个字段相同
defineProps<{ expanded: boolean; controls: string }>()
const emit = defineEmits<{ toggle: [] }>()

const { t } = useI18n()
</script>

<template>
  <button
    type="button"
    class="mt-0.5 rounded text-blue-400/90 underline-offset-2 hover:text-blue-300 hover:underline focus-visible:outline focus-visible:outline-1 focus-visible:outline-blue-500"
    :aria-expanded="expanded"
    :aria-controls="controls"
    @mousedown.prevent
    @click.prevent="emit('toggle')"
  >
    {{ expanded ? t('settings.helpCollapse') : t('settings.helpExpand') }}
  </button>
</template>
