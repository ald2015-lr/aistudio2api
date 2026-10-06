<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from '@/i18n'
import { PAGE_SIZES } from '@/paging'

const props = defineProps<{
  total: number
}>()

const page = defineModel<number>('page', { required: true })
const pageSize = defineModel<number>('pageSize', { required: true })

const { tf, t } = useI18n()

type PagerItem = { kind: 'page'; value: number } | { kind: 'gap'; value: number }

const pageCount = computed(() => Math.max(1, Math.ceil(props.total / pageSize.value)))
const current = computed(() => Math.min(Math.max(1, page.value), pageCount.value))

// items 生成紧凑页码：首页、当前页前后各一页、末页，其余用省略号
const items = computed<PagerItem[]>(() => {
  const count = pageCount.value
  const result: PagerItem[] = [{ kind: 'page', value: 1 }]
  const start = Math.max(2, current.value - 1)
  const end = Math.min(count - 1, current.value + 1)
  if (start > 2) result.push({ kind: 'gap', value: -1 })
  for (let value = start; value <= end; value += 1) result.push({ kind: 'page', value })
  if (end < count - 1) result.push({ kind: 'gap', value: -2 })
  if (count > 1) result.push({ kind: 'page', value: count })
  return result
})

function go(value: number): void {
  page.value = Math.min(Math.max(1, value), pageCount.value)
}

function changeSize(event: Event): void {
  const target = event.target
  if (target instanceof HTMLSelectElement) pageSize.value = Number(target.value)
}
</script>

<template>
  <div class="flex flex-wrap items-center justify-between gap-2 text-xs text-gray-400">
    <span class="font-mono">{{ tf('pager.total', { count: total }) }}</span>
    <div class="flex flex-wrap items-center gap-1">
      <button
        class="btn btn-sm"
        type="button"
        :disabled="current <= 1"
        :aria-label="t('pager.prev')"
        @click="go(current - 1)"
      >
        ‹
      </button>
      <template v-for="item in items" :key="`${item.kind}:${item.value}`">
        <span v-if="item.kind === 'gap'" class="px-1 text-gray-600">…</span>
        <button
          v-else
          class="btn btn-sm min-w-[1.75rem] font-mono"
          :class="item.value === current ? 'btn-primary' : ''"
          type="button"
          @click="go(item.value)"
        >
          {{ item.value }}
        </button>
      </template>
      <button
        class="btn btn-sm"
        type="button"
        :disabled="current >= pageCount"
        :aria-label="t('pager.next')"
        @click="go(current + 1)"
      >
        ›
      </button>
      <select class="input ml-1 h-[1.625rem] text-xs" :value="pageSize" @change="changeSize">
        <option v-for="size in PAGE_SIZES" :key="size" :value="size">
          {{ tf('pager.perPage', { count: size }) }}
        </option>
      </select>
    </div>
  </div>
</template>
