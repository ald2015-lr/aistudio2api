<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api } from '@/api'
import { confirmAction } from '@/confirm'
import { useI18n, type TranslationKey } from '@/i18n'
import type { OnboardingPolicy, OnboardingResult, OnboardingStatus } from '@/types'

// 新账户自动处理：显示队列状态，修改设置后自动保存并立即生效
const props = defineProps<{
  disabledCount: number
}>()
const { t, tf } = useI18n()

const POLL_INTERVAL = 5000
const SAVE_DELAY = 600

const status = ref<OnboardingStatus | null>(null)
const draft = ref<OnboardingPolicy | null>(null)
const expanded = ref(false)
const error = ref('')
const saving = ref(false)
const saved = ref(false)
let pollTimer: number | undefined
let saveTimer: number | undefined
let savedTimer: number | undefined

const resultKeys: Record<OnboardingResult, TranslationKey> = {
  enabled: 'onboard.result.enabled',
  verified: 'onboard.result.verified',
  verify_failed: 'onboard.result.verify_failed',
  error: 'onboard.result.error',
  skipped: 'onboard.result.skipped',
  manual: 'onboard.result.manual',
  baseline: 'onboard.result.baseline',
}
const resultClass: Record<OnboardingResult, string> = {
  enabled: 'text-green-400',
  verified: 'text-green-400',
  verify_failed: 'text-red-400',
  error: 'text-yellow-400',
  skipped: 'text-gray-400',
  manual: 'text-gray-400',
  baseline: 'text-gray-500',
}
// 顶部汇总只显示与新账户处理相关的结果
const summaryResults: OnboardingResult[] = ['enabled', 'verified', 'verify_failed', 'error']

const active = computed(() => {
  const policy = status.value?.policy
  return policy !== undefined && (policy.auto_verify || policy.auto_enable)
})
const processing = computed(() => status.value?.processing ?? [])
const recent = computed(() => status.value?.recent ?? [])

const dirty = computed(() => {
  if (draft.value === null || status.value === null) return false
  const current = status.value.policy
  return (
    draft.value.auto_verify !== current.auto_verify ||
    draft.value.auto_enable !== current.auto_enable ||
    draft.value.batch_size !== current.batch_size ||
    draft.value.concurrency !== current.concurrency
  )
})

const valid = computed(() => {
  const value = draft.value
  if (value === null) return false
  return (
    Number.isInteger(value.batch_size) &&
    value.batch_size >= 1 &&
    value.batch_size <= 200 &&
    Number.isInteger(value.concurrency) &&
    value.concurrency >= 1 &&
    value.concurrency <= 8
  )
})

const queueText = computed(() => {
  const value = status.value
  if (value === null) return ''
  if (!active.value) return t('onboard.off')
  const parts: string[] = []
  if (value.pending > 0) parts.push(tf('onboard.pending', { count: value.pending }))
  if (processing.value.length > 0) parts.push(tf('onboard.processing', { count: processing.value.length }))
  if (parts.length === 0) return t('onboard.idle')
  if (value.pending > 0 && processing.value.length === 0) {
    parts.push(
      value.next_batch_seconds <= 0
        ? t('onboard.nextSoon')
        : tf('onboard.nextIn', { seconds: value.next_batch_seconds }),
    )
  }
  return parts.join(' · ')
})

function countOf(result: OnboardingResult): number {
  return status.value?.counts?.[result] ?? 0
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleTimeString()
}

async function load(): Promise<void> {
  try {
    const next = await api.onboarding()
    status.value = next
    // 用户正在编辑或保存时不覆盖草稿
    if (draft.value === null || (!dirty.value && !saving.value)) {
      draft.value = { ...next.policy }
    }
    error.value = ''
  } catch (err) {
    error.value = err instanceof Error ? err.message : String(err)
  }
}

async function save(): Promise<void> {
  if (draft.value === null || !dirty.value) return
  if (!valid.value) {
    error.value = t('onboard.invalid')
    return
  }
  saving.value = true
  try {
    const next = await api.updateOnboarding({ ...draft.value })
    status.value = next
    draft.value = { ...next.policy }
    error.value = ''
    saved.value = true
    window.clearTimeout(savedTimer)
    savedTimer = window.setTimeout(() => {
      saved.value = false
    }, 2500)
  } catch (err) {
    error.value = err instanceof Error ? err.message : String(err)
  } finally {
    saving.value = false
  }
}

const queueing = ref(false)

// queueDisabled 把功能开启前就已存在的停用账户交给同一流程处理
async function queueDisabled(): Promise<void> {
  const confirmed = await confirmAction(
    tf('onboard.queueDisabledConfirm', { count: props.disabledCount }),
    t('onboard.queueDisabledAction'),
  )
  if (!confirmed) return
  queueing.value = true
  try {
    status.value = await api.queueDisabledOnboarding()
    error.value = ''
  } catch (err) {
    error.value = err instanceof Error ? err.message : String(err)
  } finally {
    queueing.value = false
  }
}

function scheduleSave(): void {
  window.clearTimeout(saveTimer)
  saveTimer = window.setTimeout(() => void save(), SAVE_DELAY)
}

onMounted(() => {
  void load()
  pollTimer = window.setInterval(() => {
    if (document.visibilityState === 'visible') void load()
  }, POLL_INTERVAL)
})

onUnmounted(() => {
  window.clearInterval(pollTimer)
  window.clearTimeout(saveTimer)
  window.clearTimeout(savedTimer)
})
</script>

<template>
  <section class="panel px-4 py-3">
    <div class="flex flex-wrap items-center gap-x-3 gap-y-2">
      <span class="dot" :class="active ? 'bg-green-500' : 'bg-gray-600'"></span>
      <span class="font-bold text-gray-100">{{ t('onboard.title') }}</span>
      <span v-tooltip="processing.join('\n')" class="text-xs text-gray-400">{{ queueText }}</span>
      <span class="flex flex-wrap gap-2 text-xs">
        <template v-for="result in summaryResults" :key="result">
          <span v-if="countOf(result) > 0" class="tag" :class="resultClass[result]">
            {{ t(resultKeys[result]) }} {{ countOf(result) }}
          </span>
        </template>
      </span>
      <span v-if="saving" class="text-xs text-gray-400">{{ t('onboard.saving') }}</span>
      <span v-else-if="saved" class="text-xs text-green-400">{{ t('onboard.saved') }}</span>
      <button
        v-if="active && disabledCount > 0"
        class="btn btn-sm ml-auto"
        type="button"
        :disabled="queueing"
        @click="queueDisabled"
      >
        {{ tf('onboard.queueDisabled', { count: disabledCount }) }}
      </button>
      <button
        class="btn btn-sm btn-ghost"
        :class="active && disabledCount > 0 ? '' : 'ml-auto'"
        type="button"
        @click="expanded = !expanded"
      >
        {{ expanded ? t('onboard.collapse') : t('onboard.settings') }}
      </button>
    </div>

    <div v-if="expanded && draft" class="mt-3 space-y-3 border-t border-gray-800 pt-3">
      <div class="flex flex-col gap-2 text-sm text-gray-200">
        <label class="flex items-start gap-2">
          <input v-model="draft.auto_verify" class="mt-1" type="checkbox" @change="scheduleSave" />
          <span>{{ t('onboard.autoVerify') }}</span>
        </label>
        <label class="flex items-start gap-2">
          <input v-model="draft.auto_enable" class="mt-1" type="checkbox" @change="scheduleSave" />
          <span>{{ t('onboard.autoEnable') }}</span>
        </label>
      </div>
      <div class="flex flex-wrap items-center gap-4 text-sm text-gray-300">
        <label class="flex items-center gap-2">
          <span>{{ t('onboard.batchSize') }}</span>
          <input
            v-model.number="draft.batch_size"
            class="input w-24"
            type="number"
            min="1"
            max="200"
            @input="scheduleSave"
          />
        </label>
        <label class="flex items-center gap-2">
          <span>{{ t('onboard.concurrency') }}</span>
          <input
            v-model.number="draft.concurrency"
            class="input w-20"
            type="number"
            min="1"
            max="8"
            @input="scheduleSave"
          />
        </label>
      </div>
      <p class="text-xs leading-relaxed text-gray-500">{{ t('onboard.hint') }}</p>

      <div>
        <div class="mb-1 text-xs font-bold text-gray-400">{{ t('onboard.recent') }}</div>
        <p v-if="recent.length === 0" class="text-xs text-gray-500">{{ t('onboard.none') }}</p>
        <div v-else class="max-h-64 overflow-auto">
          <table class="data-table">
            <tbody>
              <tr v-for="event in recent" :key="`${event.account_id}-${event.at}`">
                <td class="w-24 whitespace-nowrap font-mono text-xs text-gray-500">{{ formatTime(event.at) }}</td>
                <td class="font-mono text-xs text-gray-200">{{ event.account_id }}</td>
                <td class="w-28 whitespace-nowrap text-xs" :class="resultClass[event.result]">
                  {{ t(resultKeys[event.result]) }}
                </td>
                <td>
                  <div v-tooltip="event.detail || ''" class="cell-truncate text-xs text-gray-500">
                    {{ event.detail }}
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>
    <p v-if="error" class="mt-2 text-xs text-red-400">{{ error }}</p>
  </section>
</template>
