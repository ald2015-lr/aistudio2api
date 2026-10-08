import { computed, ref, watch } from 'vue'

import { readStorage, writeStorage } from '@/storage'

export const PAGE_SIZES = [20, 50, 100, 200] as const

// readPageSize 读取保存的每页条数，无效时使用默认值
function readPageSize(storageKey: string, fallback: number): number {
  const stored = Number(readStorage(storageKey))
  return PAGE_SIZES.some((size) => size === stored) ? stored : fallback
}

// usePaging 为列表提供页码、每页条数与当前页数据；每页条数按 storageKey 记忆
export function usePaging<T>(source: () => readonly T[], storageKey: string, fallback = 50) {
  const page = ref(1)
  const pageSize = ref(readPageSize(storageKey, fallback))
  const pageCount = computed(() => Math.max(1, Math.ceil(source().length / pageSize.value)))
  const pageItems = computed(() => {
    const current = Math.min(page.value, pageCount.value)
    const start = (current - 1) * pageSize.value
    return source().slice(start, start + pageSize.value)
  })

  watch(pageSize, (size) => {
    writeStorage(storageKey, String(size))
    page.value = 1
  })
  watch(pageCount, (count) => {
    if (page.value > count) page.value = count
  })

  return { page, pageSize, pageCount, pageItems }
}
