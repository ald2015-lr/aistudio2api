<script setup lang="ts">
import { computed, ref, useId, watch } from 'vue'
import { useI18n } from '@/i18n'
import UiIcon from '../UiIcon.vue'
import UiPopover from '../UiPopover.vue'

const props = defineProps<{
  label: string
  options: string[]
  selected: string[]
  format: (value: string) => string
}>()
const emit = defineEmits<{ change: [values: string[]] }>()

const { t } = useI18n()
const id = useId()
const open = ref(false)
const query = ref('')
// 打开时清空上次的搜索词
watch(open, (value) => {
  if (value) query.value = ''
})
const visible = computed(() => {
  const needle = query.value.trim().toLowerCase()
  const values = [...new Set([...props.selected, ...props.options])]
  return needle === ''
    ? values
    : values.filter((value) => props.format(value).toLowerCase().includes(needle))
})

// toggle 切换一个取值的选中状态
function toggle(value: string, checked: boolean): void {
  emit(
    'change',
    checked ? [...props.selected, value] : props.selected.filter((item) => item !== value),
  )
}
</script>

<template>
  <UiPopover
    v-model:open="open"
    :label="label"
    content-class="w-72 p-2"
    :trigger-class="
      [
        'flex h-8 items-center gap-1.5 rounded-md border px-2.5 text-xs transition-colors data-[state=open]:border-blue-500',
        selected.length > 0
          ? 'border-blue-500/50 bg-blue-600/10 text-blue-200'
          : 'border-line bg-canvas text-gray-300 hover:border-line-strong',
      ].join(' ')
    "
  >
    <template #trigger>
      {{ label }}
      <span
        v-if="selected.length > 0"
        class="rounded bg-blue-600/30 px-1 text-[10px] text-blue-100 tabular-nums"
        >{{ selected.length }}</span
      >
      <UiIcon name="chevronDown" :size="12" class="text-gray-500" />
    </template>
    <label class="mb-2 block">
      <span class="sr-only">{{ t('usage.filterSearch') }}</span>
      <input
        v-model="query"
        type="search"
        :placeholder="t('usage.filterSearch')"
        class="input h-7 w-full text-xs"
      />
    </label>
    <fieldset class="max-h-64 overflow-y-auto">
      <legend class="sr-only">{{ label }}</legend>
      <p v-if="visible.length === 0" class="px-2 py-3 text-center text-xs text-gray-500">
        {{ t('usage.filterNone') }}
      </p>
      <label
        v-for="(value, index) in visible"
        :key="value"
        :for="`${id}-${index}`"
        class="flex cursor-pointer items-center gap-2 rounded px-2 py-1.5 text-xs text-gray-300 hover:bg-raised"
      >
        <input
          :id="`${id}-${index}`"
          type="checkbox"
          :checked="selected.includes(value)"
          @change="toggle(value, ($event.target as HTMLInputElement).checked)"
        />
        <span class="min-w-0 flex-1 truncate">{{ format(value) }}</span>
      </label>
    </fieldset>
    <div v-if="selected.length > 0" class="mt-2 border-t border-line pt-2">
      <button
        type="button"
        class="w-full rounded px-2 py-1 text-left text-xs text-gray-400 hover:bg-raised hover:text-white"
        @click="emit('change', [])"
      >
        {{ t('usage.filtersClear') }}
      </button>
    </div>
  </UiPopover>
</template>
