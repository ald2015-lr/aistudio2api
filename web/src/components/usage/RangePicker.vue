<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from '@/i18n'
import UiIcon from '../UiIcon.vue'
import UiPopover from '../UiPopover.vue'
import { useUsageFormat } from './format'
import {
  calendarPresets,
  localInput,
  relativePresets,
  retention,
  validRange,
  type RangePreset,
} from './model'

const props = defineProps<{
  range: RangePreset
  resolved: { from: Date; to: Date } | null
}>()
const emit = defineEmits<{ apply: [range: RangePreset, from: string, to: string] }>()

const { t } = useI18n()
const { dateTime } = useUsageFormat()
const open = ref(false)
const customFrom = ref('')
const customTo = ref('')
const now = ref(new Date())

const summary = computed(() =>
  props.resolved === null
    ? ''
    : `${dateTime(props.resolved.from)} – ${dateTime(props.resolved.to)}`,
)
const customError = computed(() => {
  const error = validRange(new Date(customFrom.value), new Date(customTo.value))
  return error === '' ? '' : t(error)
})
const beforeRetention = computed(
  () => new Date(customFrom.value).getTime() < now.value.getTime() - retention,
)
const groups = [
  { label: 'usage.rangeRecent' as const, items: relativePresets },
  { label: 'usage.rangeCalendar' as const, items: calendarPresets },
]

// 打开浮层时用当前范围填充自定义输入框
watch(open, (value) => {
  if (!value) return
  now.value = new Date()
  const range = props.resolved ?? {
    from: new Date(now.value.getTime() - 86_400_000),
    to: now.value,
  }
  customFrom.value = localInput(range.from)
  customTo.value = localInput(range.to)
})

// choose 应用快捷范围并关闭浮层
function choose(range: RangePreset): void {
  emit('apply', range, '', '')
  open.value = false
}

// applyCustom 应用校验通过的自定义范围
function applyCustom(): void {
  if (customError.value !== '') return
  emit('apply', 'custom', customFrom.value, customTo.value)
  open.value = false
}
</script>

<template>
  <UiPopover
    v-model:open="open"
    :label="`${t('usage.range')}: ${t(`usage.range.${range}`)} ${summary}`"
    content-class="w-[min(36rem,calc(100vw-24px))] p-3 text-sm"
    trigger-class="flex h-8 min-w-0 items-center gap-2 rounded-md border border-line bg-canvas px-3 text-left text-sm text-gray-200 transition-colors hover:border-line-strong data-[state=open]:border-blue-500"
  >
    <template #trigger>
      <span class="font-medium whitespace-nowrap">{{ t(`usage.range.${range}`) }}</span>
      <span class="hidden truncate text-xs text-gray-500 tabular-nums sm:inline">{{
        summary
      }}</span>
      <UiIcon name="chevronDown" :size="12" class="text-gray-500" />
    </template>
    <div class="grid gap-4 sm:grid-cols-[1fr_1.1fr]">
      <div class="space-y-3">
        <section v-for="group in groups" :key="group.label">
          <h4 class="mb-1.5 text-[11px] font-medium tracking-wide text-gray-500 uppercase">
            {{ t(group.label) }}
          </h4>
          <div class="grid grid-cols-2 gap-1">
            <button
              v-for="item in group.items"
              :key="item"
              type="button"
              :aria-pressed="range === item"
              class="rounded px-2 py-1.5 text-left text-xs transition-colors"
              :class="
                range === item
                  ? 'bg-blue-600/20 text-blue-300'
                  : 'text-gray-300 hover:bg-raised hover:text-white'
              "
              @click="choose(item)"
            >
              {{ t(`usage.range.${item}`) }}
            </button>
          </div>
        </section>
      </div>
      <form class="space-y-2 border-line sm:border-l sm:pl-4" @submit.prevent="applyCustom">
        <h4 class="mb-1.5 text-[11px] font-medium tracking-wide text-gray-500 uppercase">
          {{ t('usage.rangeCustom') }}
        </h4>
        <label class="block text-xs text-gray-400">
          {{ t('usage.rangeStart') }}
          <input
            v-model="customFrom"
            type="datetime-local"
            :max="localInput(now)"
            class="input mt-1 w-full"
          />
        </label>
        <label class="block text-xs text-gray-400">
          {{ t('usage.rangeEnd') }}
          <input v-model="customTo" type="datetime-local" class="input mt-1 w-full" />
        </label>
        <p v-if="customError" class="text-xs text-red-400" role="alert">{{ customError }}</p>
        <p v-else-if="beforeRetention" class="text-xs text-yellow-400">
          {{ t('usage.rangeRetention') }}
        </p>
        <button type="submit" :disabled="customError !== ''" class="btn btn-sm btn-primary w-full">
          {{ t('usage.rangeApply') }}
        </button>
      </form>
    </div>
  </UiPopover>
</template>
