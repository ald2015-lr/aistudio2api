<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '@/api'
import { copyText } from '@/clipboard'
import { useI18n, type TranslationKey } from '@/i18n'
import type { StudioKeyRecord, StudioKeyStatus } from '@/types'

// AI Studio API Key：显示获取队列与每个账户的记录（Key 只显示末四位），复制按钮按需读取完整 Key、不在页面里保存。
// 获取由账户行的“获取 Key”按钮或选中账户后的批量按钮触发（见 AccountsPanel），这里只轮询状态
const { t, tf } = useI18n()
const emit = defineEmits<{ notice: [message: string, kind: 'success' | 'error'] }>()

const POLL_RUNNING = 3000
const POLL_IDLE = 20000

const status = ref<StudioKeyStatus | null>(null)
const error = ref('')
const expanded = ref(false)
const copying = ref('')
const busy = ref(false)
let pollTimer: number | undefined

const statusKeys: Record<string, TranslationKey> = {
  ok: 'studioKey.status.ok',
  none: 'studioKey.status.none',
  failed: 'studioKey.status.failed',
  skipped: 'studioKey.status.skipped',
}
const statusClass: Record<string, string> = {
  ok: 'text-green-400',
  none: 'text-yellow-400',
  failed: 'text-red-400',
  skipped: 'text-gray-400',
}

const records = computed<StudioKeyRecord[]>(() => status.value?.records ?? [])
const withKey = computed(() => records.value.filter((record) => record.has_key).length)
const queueText = computed(() => {
  const value = status.value
  if (value === null) return ''
  if (!value.running) return t('studioKey.idle')
  return tf('studioKey.running', {
    current: value.current || '-',
    queued: value.queue.length,
    delay: value.delay_seconds,
  })
})

function statusLabel(value: string): string {
  const key = statusKeys[value]
  return key === undefined ? value : t(key)
}

function formatTime(value?: string): string {
  if (!value) return '-'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '-' : date.toLocaleString()
}

// refresh 读取状态；任务进行中时更快轮询
async function refresh(): Promise<void> {
  try {
    status.value = await api.studioKeys()
    error.value = ''
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause)
  }
  schedule()
}

function schedule(): void {
  if (pollTimer !== undefined) window.clearTimeout(pollTimer)
  pollTimer = window.setTimeout(
    () => void refresh(),
    status.value?.running ? POLL_RUNNING : POLL_IDLE,
  )
}

// fetchKeys 把账户加入获取队列（AccountsPanel 调用）
async function fetchKeys(ids: string[]): Promise<void> {
  if (ids.length === 0) return
  try {
    status.value = await api.fetchStudioKeys(ids)
    expanded.value = true
    emit('notice', tf('studioKey.queued', { count: ids.length }), 'success')
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  }
  schedule()
}

// fetchAll 全部账户排队，只补还没有 Key 的；服务端逐个处理，号与号之间按间隔等待
async function fetchAll(): Promise<void> {
  if (
    !window.confirm(tf('studioKey.fetchAllConfirm', { delay: status.value?.delay_seconds ?? 30 }))
  )
    return
  busy.value = true
  try {
    status.value = await api.fetchAllStudioKeys()
    expanded.value = true
    emit(
      'notice',
      status.value.running ? t('studioKey.queuedAll') : t('studioKey.allDone'),
      'success',
    )
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  } finally {
    busy.value = false
  }
  schedule()
}

async function stopAll(): Promise<void> {
  busy.value = true
  try {
    status.value = await api.stopStudioKeys()
    emit('notice', t('studioKey.stopped'), 'success')
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  } finally {
    busy.value = false
  }
  schedule()
}

// copyAll 读取全部完整 Key，一行一个复制到剪贴板，不保存在页面状态里
async function copyAll(): Promise<void> {
  busy.value = true
  try {
    const exported = await api.exportStudioKeys()
    if (exported.keys.length === 0) {
      emit('notice', t('studioKey.noKeys'), 'error')
      return
    }
    const copied = await copyText(exported.keys.map((item) => item.key).join('\n'))
    emit(
      'notice',
      copied
        ? tf('studioKey.copiedAll', { count: exported.keys.length })
        : t('studioKey.copyFailed'),
      copied ? 'success' : 'error',
    )
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  } finally {
    busy.value = false
  }
}

// copyKey 读取完整 Key 并复制，不保存在页面状态里
async function copyKey(record: StudioKeyRecord): Promise<void> {
  copying.value = record.account_id
  try {
    const secret = await api.revealStudioKey(record.account_id)
    const copied = await copyText(secret.key)
    emit(
      'notice',
      copied ? t('studioKey.copied') : t('studioKey.copyFailed'),
      copied ? 'success' : 'error',
    )
  } catch (cause) {
    emit('notice', cause instanceof Error ? cause.message : String(cause), 'error')
  } finally {
    copying.value = ''
  }
}

defineExpose({ fetchKeys, refresh })

onMounted(() => void refresh())
onBeforeUnmount(() => {
  if (pollTimer !== undefined) window.clearTimeout(pollTimer)
})
</script>

<template>
  <section class="panel px-4 py-3">
    <div class="flex flex-wrap items-center gap-x-3 gap-y-2">
      <span class="dot" :class="status?.running ? 'bg-green-500' : 'bg-gray-600'"></span>
      <span class="font-bold text-gray-100">{{ t('studioKey.title') }}</span>
      <span class="text-xs text-gray-400">{{ queueText }}</span>
      <span v-if="records.length > 0" class="tag text-xs text-green-400">
        {{ tf('studioKey.summary', { count: withKey, total: records.length }) }}
      </span>
      <span v-if="error" class="text-xs text-red-400">{{ error }}</span>
      <button class="btn btn-sm ml-auto" type="button" :disabled="busy" @click="fetchAll">
        {{ t('studioKey.fetchAll') }}
      </button>
      <button
        v-if="status?.running && (status.queue.length ?? 0) > 0"
        class="btn btn-sm"
        type="button"
        :disabled="busy"
        @click="stopAll"
      >
        {{ t('studioKey.stop') }}
      </button>
      <button class="btn btn-sm" type="button" :disabled="busy || withKey === 0" @click="copyAll">
        {{ tf('studioKey.copyAll', { count: withKey }) }}
      </button>
      <button class="btn btn-sm btn-ghost" type="button" @click="expanded = !expanded">
        {{ expanded ? t('studioKey.collapse') : t('studioKey.expand') }}
      </button>
    </div>
    <div v-if="expanded" class="mt-3 space-y-2 border-t border-gray-800 pt-3">
      <p class="text-xs text-gray-500">
        {{ tf('studioKey.help', { delay: status?.delay_seconds ?? 30 }) }}
      </p>
      <p v-if="records.length === 0" class="text-sm text-gray-500">{{ t('studioKey.empty') }}</p>
      <div v-else class="overflow-x-auto">
        <table class="w-full text-left text-xs">
          <thead class="text-gray-500">
            <tr>
              <th class="py-1 pr-3">{{ t('studioKey.account') }}</th>
              <th class="py-1 pr-3">{{ t('studioKey.key') }}</th>
              <th class="py-1 pr-3">{{ t('studioKey.projectName') }}</th>
              <th class="py-1 pr-3">{{ t('studioKey.projectNumber') }}</th>
              <th class="py-1 pr-3">{{ t('studioKey.state') }}</th>
              <th class="py-1 pr-3">{{ t('studioKey.time') }}</th>
              <th class="py-1"></th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="record in records"
              :key="record.account_id"
              class="border-t border-gray-800 text-gray-300"
            >
              <td class="py-1 pr-3 break-all">{{ record.label || record.account_id }}</td>
              <td class="py-1 pr-3 font-mono">{{ record.has_key ? `…${record.last4}` : '-' }}</td>
              <td class="py-1 pr-3">
                {{ record.project_name || '-' }}
                <span v-if="record.project_display" class="text-gray-500"
                  >（{{ record.project_display }}）</span
                >
              </td>
              <td class="py-1 pr-3 font-mono">{{ record.project_number || '-' }}</td>
              <td class="py-1 pr-3">
                <span :class="statusClass[record.status] ?? 'text-gray-400'">
                  {{ statusLabel(record.status) }}
                </span>
                <span v-if="record.created" class="ml-1 text-gray-500">{{
                  t('studioKey.created')
                }}</span>
                <div v-if="record.reason" class="text-gray-500 break-all">{{ record.reason }}</div>
              </td>
              <td class="py-1 pr-3 text-gray-500">
                <div>{{ t('studioKey.fetchedAt') }} {{ formatTime(record.fetched_at) }}</div>
                <div v-if="record.created_at">
                  {{ t('studioKey.createdAt') }} {{ formatTime(record.created_at) }}
                </div>
                <div v-if="record.attempt_at && record.status !== 'ok'">
                  {{ t('studioKey.attemptAt') }} {{ formatTime(record.attempt_at) }}
                </div>
              </td>
              <td class="py-1">
                <button
                  v-if="record.has_key"
                  class="btn btn-sm"
                  type="button"
                  :disabled="copying === record.account_id"
                  @click="copyKey(record)"
                >
                  {{ t('studioKey.copy') }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </section>
</template>
