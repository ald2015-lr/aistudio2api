<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { api } from '@/api'
import { copyText } from '@/clipboard'
import { useI18n } from '@/i18n'
import type { RequestBody, UsageRecord } from '@/types'
import UiIcon from '../UiIcon.vue'
import { formatBytes, useUsageFormat } from './format'
import { latencyTone, stateTone } from './model'

const props = defineProps<{ record: UsageRecord | null }>()
const emit = defineEmits<{ close: []; search: [value: string] }>()

const { t, tf, errorText } = useI18n()
const { integer, duration, dateTime, dimensionLabel, downgradeLabel } = useUsageFormat()
const body = ref<RequestBody | null>(null)
const bodyState = ref<'idle' | 'loading' | 'ready' | 'error'>('idle')
const bodyError = ref('')
const copied = ref('')
const closeButton = ref<HTMLButtonElement>()
const titleId = `usage-drawer-${Math.random().toString(36).slice(2)}`
let restoreFocus: HTMLElement | null = null

// onKeydown 按 Esc 关闭抽屉
function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') {
    event.preventDefault()
    emit('close')
  }
}

watch(
  () => props.record,
  async (record, previous) => {
    body.value = null
    bodyError.value = ''
    bodyState.value = 'idle'
    if (record !== null && previous === null) {
      // 打开时记下原焦点并聚焦关闭按钮，关闭后还原
      restoreFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
      document.addEventListener('keydown', onKeydown)
      void nextTick(() => closeButton.value?.focus())
    } else if (record === null && previous !== null) {
      document.removeEventListener('keydown', onKeydown)
      restoreFocus?.focus()
      restoreFocus = null
    }
    if (record === null || !record.has_body) return
    bodyState.value = 'loading'
    try {
      const value = await api.requestBody(record.id)
      if (props.record?.id !== record.id) return
      body.value = value
      bodyState.value = 'ready'
    } catch (error) {
      if (props.record?.id !== record.id) return
      bodyError.value = errorText(error)
      bodyState.value = 'error'
    }
  },
)

onBeforeUnmount(() => document.removeEventListener('keydown', onKeydown))

const summary = computed(() => {
  const record = props.record
  if (record === null) return []
  const items = [
    { label: t('usage.dimension.model'), value: record.model || '—' },
    { label: t('usage.dimension.account'), value: record.account || '—' },
    { label: t('usage.dimension.channel'), value: dimensionLabel('channel', record.channel) },
    { label: t('usage.dimension.protocol'), value: dimensionLabel('protocol', record.protocol) },
    { label: t('usage.path'), value: record.path },
    { label: t('usage.dimension.status'), value: String(record.status) },
    { label: t('usage.duration'), value: duration(record.duration_ms) },
    { label: t('usage.firstEvent'), value: duration(record.first_event_ms) },
    { label: t('usage.queue'), value: duration(record.queue_ms) },
    { label: t('usage.toolCalls'), value: integer(record.tool_calls) },
    { label: t('usage.input'), value: integer(record.input_tokens) },
    { label: t('usage.reasoning'), value: integer(record.reasoning_tokens) },
    { label: t('usage.reply'), value: integer(record.reply_tokens) },
    { label: t('usage.tokens'), value: integer(record.total_tokens) },
  ]
  if (record.served_model) items.push({ label: t('usage.servedModel'), value: record.served_model })
  if (record.downgrade)
    items.push({ label: t('usage.downgrade'), value: downgradeLabel(record.downgrade) })
  return items
})

// byteLength 返回文本的 UTF-8 字节数
function byteLength(text: string): number {
  return new TextEncoder().encode(text).length
}

// pretty 格式化 JSON 正文，无法解析时原样返回
function pretty(text: string): string {
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

// copy 复制文本并短暂显示已复制
async function copy(kind: string, text: string): Promise<void> {
  if (!(await copyText(text))) return
  copied.value = kind
  window.setTimeout(() => {
    if (copied.value === kind) copied.value = ''
  }, 1200)
}
</script>

<template>
  <Teleport to="body">
    <div v-if="record" class="fixed inset-0 z-[70] bg-black/60" @click="emit('close')"></div>
    <aside
      v-if="record"
      role="dialog"
      aria-modal="true"
      :aria-labelledby="titleId"
      class="fixed inset-y-0 right-0 z-[71] flex w-full max-w-2xl flex-col border-l border-line bg-panel shadow-2xl"
    >
      <header class="flex items-start gap-3 border-b border-line px-5 py-4">
        <div class="min-w-0 flex-1">
          <h2 :id="titleId" class="text-base font-semibold text-white">
            {{ t('usage.detailTitle') }}
          </h2>
          <div class="mt-1 flex flex-wrap items-center gap-2 text-xs text-gray-400">
            <code class="font-mono text-gray-300">{{ record.id }}</code>
            <button type="button" class="btn btn-sm btn-ghost" @click="copy('id', record.id)">
              {{ copied === 'id' ? t('common.copied') : t('common.copy') }}
            </button>
            <span class="tabular-nums">{{ dateTime(record.time, true) }}</span>
          </div>
        </div>
        <button
          ref="closeButton"
          type="button"
          class="rounded p-1 text-gray-500 hover:bg-raised hover:text-white"
          :aria-label="t('common.close')"
          @click="emit('close')"
        >
          <UiIcon name="close" :size="16" />
        </button>
      </header>
      <div class="flex-1 space-y-5 overflow-y-auto px-5 py-4 text-sm">
        <section>
          <h3 class="mb-2 text-xs font-medium tracking-wide text-gray-500 uppercase">
            {{ t('usage.detailSummary') }}
          </h3>
          <div class="mb-3 flex flex-wrap items-center gap-2">
            <span class="rounded px-2 py-0.5 text-xs" :class="stateTone(record.state)">{{
              dimensionLabel('state', record.state)
            }}</span>
            <span class="flex items-center gap-1.5 text-xs text-gray-400">
              <span
                class="h-1.5 w-1.5 rounded-full"
                :class="latencyTone(record.duration_ms, 'duration')"
              ></span>
              {{ duration(record.duration_ms) }}
            </span>
            <span v-if="record.duplicate" class="tag text-yellow-300">{{
              t('usage.duplicate')
            }}</span>
          </div>
          <dl class="grid grid-cols-1 gap-x-6 gap-y-2 text-xs sm:grid-cols-2">
            <div
              v-for="item in summary"
              :key="item.label"
              class="flex min-w-0 justify-between gap-3"
            >
              <dt class="shrink-0 text-gray-500">{{ item.label }}</dt>
              <dd v-tooltip="item.value" class="truncate text-right text-gray-200 tabular-nums">
                {{ item.value }}
              </dd>
            </div>
          </dl>
          <div v-if="record.reply_hash" class="mt-3 flex flex-wrap items-center gap-2 text-xs">
            <span class="text-gray-500">{{ t('usage.replyHash') }}</span>
            <code class="font-mono text-gray-300">{{ record.reply_hash }}</code>
            <button
              type="button"
              class="btn btn-sm"
              @click="emit('search', record.reply_hash ?? '')"
            >
              {{ t('usage.searchReply') }}
            </button>
          </div>
        </section>
        <section v-if="record.error">
          <h3 class="mb-2 text-xs font-medium tracking-wide text-gray-500 uppercase">
            {{ t('usage.detailError') }}
          </h3>
          <pre
            class="rounded-md border border-red-500/30 bg-red-500/5 p-3 font-mono text-xs leading-5 break-words whitespace-pre-wrap text-red-200"
            >{{ record.error }}</pre>
        </section>
        <section>
          <h3 class="mb-2 text-xs font-medium tracking-wide text-gray-500 uppercase">
            {{ t('usage.detailAttempts') }}
          </h3>
          <p v-if="record.attempts.length === 0" class="text-xs text-gray-500">
            {{ t('usage.detailAttemptsNone') }}
          </p>
          <ol v-else class="space-y-2 border-l border-line pl-4">
            <li v-for="(attempt, index) in record.attempts" :key="index" class="relative text-xs">
              <span
                class="absolute top-1.5 -left-[1.3rem] h-2 w-2 rounded-full bg-red-500"
                aria-hidden="true"
              ></span>
              <div class="flex flex-wrap gap-x-3 text-gray-300">
                <span>{{ attempt.account || '—' }}</span>
                <span class="text-gray-500">{{ dimensionLabel('channel', attempt.channel) }}</span>
                <span class="text-gray-500 tabular-nums">{{ duration(attempt.duration_ms) }}</span>
              </div>
              <p class="mt-0.5 font-mono break-words text-red-300/90">{{ attempt.error }}</p>
            </li>
          </ol>
        </section>
        <section>
          <h3 class="mb-2 text-xs font-medium tracking-wide text-gray-500 uppercase">
            {{ t('usage.detailBody') }}
          </h3>
          <p v-if="!record.has_body" class="text-xs text-gray-500">
            {{ t('usage.detailBodyNone') }}
          </p>
          <p v-else-if="bodyState === 'loading'" class="text-xs text-gray-500">
            {{ t('common.loading') }}
          </p>
          <p v-else-if="bodyState === 'error'" class="text-xs text-red-300" role="alert">
            {{ bodyError }}
          </p>
          <template v-else-if="body">
            <div
              v-for="part in [
                {
                  kind: 'request',
                  label: t('usage.detailRequest'),
                  text: body.request,
                  size: body.request_size,
                },
                {
                  kind: 'response',
                  label: t('usage.detailResponse'),
                  text: body.response,
                  size: body.response_size,
                },
              ]"
              :key="part.kind"
              class="mb-3"
            >
              <div class="mb-1 flex items-center gap-2 text-xs text-gray-400">
                <span class="font-medium text-gray-300">{{ part.label }}</span>
                <span v-if="part.size > byteLength(part.text)" class="text-yellow-400/80">{{
                  tf('usage.detailTruncated', {
                    saved: formatBytes(byteLength(part.text)),
                    size: formatBytes(part.size),
                  })
                }}</span>
                <button
                  type="button"
                  class="btn btn-sm btn-ghost ml-auto"
                  @click="copy(part.kind, part.text)"
                >
                  {{ copied === part.kind ? t('common.copied') : t('common.copy') }}
                </button>
              </div>
              <pre
                class="max-h-80 overflow-auto rounded-md border border-line bg-canvas p-3 font-mono text-xs leading-5 break-words whitespace-pre-wrap text-gray-300"
                >{{ pretty(part.text) }}</pre>
            </div>
          </template>
        </section>
      </div>
    </aside>
  </Teleport>
</template>
