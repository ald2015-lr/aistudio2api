<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, onUnmounted, ref } from 'vue'
import { api } from '@/api'
import { trackPendingWrite } from '@/assetRecovery'
import { confirmAction } from '@/confirm'
import { useI18n, type TranslationKey } from '@/i18n'
import type {
  OnboardingEvent,
  OnboardingPolicy,
  OnboardingResult,
  OnboardingStatus,
  ReauthOutcome,
} from '@/types'

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

// 重新上传认证文件后的结果（最近处理里重新上传的记录与顶部汇总）：绿色为恢复，红色为仍失效，黄色为验证出错，灰色为没有验证
const outcomeKeys: Record<ReauthOutcome, TranslationKey> = {
  restored: 'onboard.reauthOutcome.restored',
  auth_required: 'onboard.reauthOutcome.auth_required',
  verify_required: 'onboard.reauthOutcome.verify_required',
  forbidden: 'onboard.reauthOutcome.forbidden',
  restricted: 'onboard.reauthOutcome.restricted',
  manual_disabled: 'onboard.reauthOutcome.manual_disabled',
  verify_off: 'onboard.reauthOutcome.verify_off',
  error: 'onboard.reauthOutcome.error',
  skipped: 'onboard.reauthOutcome.skipped',
}
const outcomeClass: Record<ReauthOutcome, string> = {
  restored: 'text-green-400',
  auth_required: 'text-red-400',
  verify_required: 'text-red-400',
  forbidden: 'text-red-400',
  restricted: 'text-red-400',
  manual_disabled: 'text-gray-400',
  verify_off: 'text-gray-400',
  error: 'text-yellow-400',
  skipped: 'text-gray-400',
}
const outcomeOrder: ReauthOutcome[] = [
  'restored',
  'auth_required',
  'verify_required',
  'forbidden',
  'restricted',
  'error',
  'manual_disabled',
  'verify_off',
  'skipped',
]
// reauthOutcomeCounts 为各账户最近一次重新上传后的结果计数（旧版服务没有这个字段，按空处理），只列出有的
const reauthOutcomeCounts = computed(() => {
  const counts = status.value?.reauth_outcomes ?? {}
  return outcomeOrder
    .filter((outcome) => (counts[outcome] ?? 0) > 0)
    .map((outcome) => ({ outcome, count: counts[outcome] ?? 0 }))
})

// eventOutcome 为最近处理里重新上传记录的结果；新账户的记录、旧版服务没有结果时为空
function eventOutcome(event: OnboardingEvent): ReauthOutcome | null {
  if (event.source !== 'reauth' || event.outcome === undefined) return null
  return event.outcome in outcomeKeys ? event.outcome : null
}
// recentRows 为最近处理的每一行与它的重新上传结果（没有时按处理结果显示）
const recentRows = computed(() => recent.value.map((event) => ({ event, outcome: eventOutcome(event) })))

const active = computed(() => {
  const policy = status.value?.policy
  return policy !== undefined && (policy.auto_verify || policy.auto_enable)
})
const processing = computed(() => status.value?.processing ?? [])
const recent = computed(() => status.value?.recent ?? [])
// 已有账户换上新认证文件后的重新验证（计入待处理与处理中）与内容相同、没有重新认证的次数；旧版服务没有这两个字段
const reauthCount = computed(() => status.value?.reauth ?? 0)
const unchangedCount = computed(() => status.value?.unchanged ?? 0)
// 状态正常的账户换上了登录签名没变的认证文件、没有重新载入的次数；旧版服务没有这个字段
const sameLoginCount = computed(() => status.value?.same_login ?? 0)
// 失效账户的上传没有按重新上传处理的次数（保留了原文件的修改时间、只差轮换 Cookie）与检测到认证文件更新、还没载入的账户数；
// 旧版服务没有这两个字段
const notReuploadedCount = computed(() => status.value?.not_reuploaded ?? 0)
const loadingCount = computed(() => status.value?.loading ?? 0)

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
  // 等待载入的另起一个标签显示（见模板）：队列为空、只是还没载入时不写“队列为空”
  if (parts.length === 0) return loadingCount.value > 0 ? '' : t('onboard.idle')
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
    // 与账户的其他写请求一样登记为在途写操作：面板卸载后请求仍会完成，页面文件载入失败后的自动刷新先等它结束
    const next = await trackPendingWrite(api.updateOnboarding({ ...draft.value }))
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
    status.value = await trackPendingWrite(api.queueDisabledOnboarding())
    error.value = ''
  } catch (err) {
    error.value = err instanceof Error ? err.message : String(err)
  } finally {
    queueing.value = false
  }
}

// scheduleSave 在设置停止变化 SAVE_DELAY 后保存；saveTimer 只在有挂起的保存时有值（卸载前据此补发，见 onBeforeUnmount）
function scheduleSave(): void {
  window.clearTimeout(saveTimer)
  saveTimer = window.setTimeout(() => {
    saveTimer = undefined
    void save()
  }, SAVE_DELAY)
}

onMounted(() => {
  void load()
  pollTimer = window.setInterval(() => {
    if (document.visibilityState === 'visible') void load()
  }, POLL_INTERVAL)
})

// 卸载前补发挂起的保存（与设置页相同）：改完设置 SAVE_DELAY 内切到其他页时，原先卸载时直接清掉定时器，这次修改既没发出、
// 也没登记为在途写操作，静默丢失。save() 在第一个 await 之前就读完草稿并发出请求、登记在途，卸载后请求照常完成，自动刷新也会等它
onBeforeUnmount(() => {
  if (saveTimer === undefined) return
  window.clearTimeout(saveTimer)
  saveTimer = undefined
  void save()
})

onUnmounted(() => {
  window.clearInterval(pollTimer)
  window.clearTimeout(savedTimer)
})
</script>

<template>
  <section class="panel px-4 py-3">
    <div class="flex flex-wrap items-center gap-x-3 gap-y-2">
      <span class="dot" :class="active ? 'bg-green-500' : 'bg-gray-600'"></span>
      <span class="font-bold text-gray-100">{{ t('onboard.title') }}</span>
      <span v-if="queueText !== ''" v-tooltip="processing.join('\n')" class="text-xs text-gray-400">{{ queueText }}</span>
      <!-- 待处理与处理中里重新上传的个数（已计入两者），说明挂在这里 -->
      <span
        v-if="active && reauthCount > 0"
        v-tooltip="t('onboard.reauthTip')"
        class="text-xs text-sky-300"
      >
        {{ tf('onboard.reauth', { count: reauthCount }) }}
      </span>
      <span v-if="loadingCount > 0" v-tooltip="t('onboard.loadingTip')" class="text-xs text-sky-300">
        {{ tf('onboard.loading', { count: loadingCount }) }}
      </span>
      <span class="flex flex-wrap gap-2 text-xs">
        <template v-for="result in summaryResults" :key="result">
          <span v-if="countOf(result) > 0" class="tag" :class="resultClass[result]">
            {{ t(resultKeys[result]) }} {{ countOf(result) }}
          </span>
        </template>
        <span
          v-for="item in reauthOutcomeCounts"
          :key="item.outcome"
          v-tooltip="t('onboard.reauthOutcomesTip')"
          class="tag"
          :class="outcomeClass[item.outcome]"
        >
          {{ tf('onboard.reauthAfter', { outcome: t(outcomeKeys[item.outcome]) }) }} {{ item.count }}
        </span>
        <span v-if="unchangedCount > 0" v-tooltip="t('onboard.unchangedTip')" class="tag text-gray-400">
          {{ tf('onboard.unchanged', { count: unchangedCount }) }}
        </span>
        <span v-if="sameLoginCount > 0" v-tooltip="t('onboard.sameLoginTip')" class="tag text-gray-400">
          {{ tf('onboard.sameLogin', { count: sameLoginCount }) }}
        </span>
        <span v-if="notReuploadedCount > 0" v-tooltip="t('onboard.notReuploadedTip')" class="tag text-yellow-400">
          {{ tf('onboard.notReuploaded', { count: notReuploadedCount }) }}
        </span>
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
              <tr v-for="{ event, outcome } in recentRows" :key="`${event.account_id}-${event.at}`">
                <!-- 窄屏不显示时间列，账户与结果放得下 -->
                <td class="hidden w-24 whitespace-nowrap font-mono text-xs text-gray-500 sm:table-cell">
                  {{ formatTime(event.at) }}
                </td>
                <td class="font-mono text-xs text-gray-200">
                  <div v-tooltip="event.account_id" class="cell-truncate">{{ event.account_id }}</div>
                </td>
                <!-- 重新上传后的结果比处理结果长：结果列放宽，窄屏时换行，不压到相邻列 -->
                <td v-if="outcome !== null" class="w-32 break-words text-xs sm:w-40" :class="outcomeClass[outcome]">
                  {{ tf('onboard.reauthAfter', { outcome: t(outcomeKeys[outcome]) }) }}
                </td>
                <td v-else class="w-32 whitespace-nowrap text-xs sm:w-40" :class="resultClass[event.result]">
                  {{ t(resultKeys[event.result]) }}
                  <span v-if="event.source === 'reauth'" v-tooltip="t('onboard.sourceReauthTip')" class="tag ml-1 text-sky-300">
                    {{ t('onboard.sourceReauth') }}
                  </span>
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
