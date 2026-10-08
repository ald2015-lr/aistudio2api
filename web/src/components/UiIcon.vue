<script setup lang="ts">
import { computed } from 'vue'
import { legacyIcons } from '@/legacy-icons'

export type IconName =
  | keyof typeof legacyIcons
  | 'accounts'
  | 'models'
  | 'requests'
  | 'playground'
  | 'plus'
  | 'login'
  | 'verify'
  | 'save'
  | 'send'
  | 'copy'
  | 'eye'
  | 'eye-off'
  | 'usage'
  | 'success'
  | 'quality'
  | 'tokens'
  | 'throughput'
  | 'latency'
  | 'firstEvent'

const props = withDefaults(
  defineProps<{
    name: IconName
    size?: number
  }>(),
  { size: 18 },
)

const aliases: Partial<Record<IconName, keyof typeof legacyIcons>> = {
  accounts: 'key',
  models: 'dashboard',
  requests: 'info',
  playground: 'chat',
  plus: 'info',
  login: 'key',
  verify: 'check',
  save: 'check',
  send: 'play',
  copy: 'dashboard',
  eye: 'info',
  'eye-off': 'close',
  // 用量页沿用已有图标，不引入新的图标库
  usage: 'dashboard',
  success: 'check',
  quality: 'info',
  tokens: 'dashboard',
  throughput: 'play',
  latency: 'refresh',
  firstEvent: 'play',
}

const source = computed(() => {
  const name = aliases[props.name] ?? (props.name as keyof typeof legacyIcons)
  return legacyIcons[name]
})
</script>

<template>
  <span
    class="app-icon inline-flex shrink-0 items-center justify-center"
    :style="{ '--icon-size': `${props.size}px` }"
    aria-hidden="true"
    v-html="source"
  ></span>
</template>
