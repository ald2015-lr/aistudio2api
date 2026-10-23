<script setup lang="ts">
// ProxyPoolSettings 设置页的 IP 池分组：模式、地址（带账号密码时掩码显示）、静态 IP 的端口段与容量提示、抽测出口、
// 检查全部端口、实时状态与“改为跟随默认代理”。改动经 commit 通知设置页保存（与失焦保存的字段相同）：模式与地址的组合
// 不合法时设置页不提交，错误显示在对应输入框旁
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { ApiError, api } from '@/api'
import { confirmAction } from '@/confirm'
import { useI18n, type TranslationKey } from '@/i18n'
import { proxyParts } from '@/proxy'
import {
  DEFAULT_VERIFY_CONCURRENCY,
  cloneProxyPool,
  formatClock,
  proxyPoolCapacity,
  proxyPoolPortCount,
  proxyPoolProblem,
  proxyPoolProblemKey,
  slotLabel,
} from '@/proxyPool'
import type {
  ProxyPoolConfig,
  ProxyPoolMode,
  ProxyPoolStatus,
  ProxyTestProbe,
  ProxyTestResult,
} from '@/types'
import ProxyInput from './ProxyInput.vue'
import UiIcon from './UiIcon.vue'
import UiSelect from './UiSelect.vue'

const model = defineModel<ProxyPoolConfig>({ required: true })
const props = defineProps<{
  status: ProxyPoolStatus | null // 状态接口的 proxy_pool：已保存并生效的设置的实时情况
  // 设置页表单里的峰值与启动并发（可能是输入到一半的值），用于估算端口需求
  maxActive: number | string
  ultraMaxActive: number | string
  warmConcurrency: number | string
  hasDefaultProxy: boolean
  // serverError 为服务端拒绝表单里当前这份设置时的错误（前端组合校验查不出的地址格式问题），显示在地址框旁
  serverError?: string
}>()
// testing 在抽测开始与结束时通知设置页（passed 为全部出口两条路径都拿到 IP）：设置页据此在抽测期间不自动应用
const emit = defineEmits<{
  commit: []
  testing: [running: boolean, passed: boolean]
  notice: [message: string, tone: 'success' | 'error']
}>()

const { t, tf, errorText, locale } = useI18n()

const INPUT_CLASS =
  'w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none'
// SAMPLE_COUNT 为抽测的出口数：静态为首、中、尾三个端口，动态为三个临时会话号
const SAMPLE_COUNT = 3
// CHECK_PENDING_MS 为发起检查后等状态接口出现新检查的最长时间，超过后按钮恢复可用
const CHECK_PENDING_MS = 15_000
// RELAY_CONNECTIONS_PER_BROWSER 与服务端开启 IP 池时的检查一致：中转连接上限低于 浏览器数 × 16 时提示调高 ulimit -n
const RELAY_CONNECTIONS_PER_BROWSER = 16

type ProxyWarning = NonNullable<ProxyTestResult['warnings']>[number]

const warningKeys: Record<ProxyWarning, TranslationKey> = {
  rotating_ip: 'proxy.warnRotating',
  https_plain_browser: 'proxy.warnHttpsPlain',
  duplicate_ip: 'proxy.warnDuplicateIP',
  pool_partial: 'proxy.warnPoolPartial',
}

// update 整体替换设置对象：表单与已保存的配置不共用对象，未保存的修改才能被识别
function update(patch: Partial<ProxyPoolConfig>): void {
  model.value = { ...model.value, ...patch }
}

function setMode(value: unknown): void {
  if (value !== 'off' && value !== 'static' && value !== 'dynamic') return
  const mode: ProxyPoolMode = value
  if (mode === model.value.mode) return
  update({ mode })
  // 下拉框没有失焦时机：选完立即提交，组合不合法时设置页不保存，错误显示在下面
  emit('commit')
}

const url = computed({
  get: () => model.value.url,
  set: (value: string) => update({ url: value }),
})
const portStart = computed({
  get: () => model.value.port_start,
  set: (value: number) => update({ port_start: value }),
})
const portEnd = computed({
  get: () => model.value.port_end,
  set: (value: number) => update({ port_end: value }),
})

const problem = computed(() => proxyPoolProblem(model.value))
const problemText = computed(() => {
  const key = proxyPoolProblemKey(problem.value)
  return key === null ? '' : t(key)
})
const portsError = computed(() =>
  problem.value === 'ports' || problem.value === 'ports_too_many' ? problemText.value : '',
)
const urlError = computed(() =>
  portsError.value === '' ? problemText.value || (props.serverError ?? '') : '',
)
// invalid 为组合不合法或被服务端拒绝：抽测与检查全部端口都按这份设置进行，先改好再测
const invalid = computed(() => problem.value !== '' || (props.serverError ?? '') !== '')

const urlHelp = computed(() => {
  if (model.value.mode === 'static') return t('settings.proxyPoolURLHelpStatic')
  if (model.value.mode === 'dynamic') return t('settings.proxyPoolURLHelpDynamic')
  return t('settings.proxyPoolURLHelpOff')
})
const urlPlaceholder = computed(() =>
  model.value.mode === 'dynamic'
    ? t('settings.proxyPoolURLPlaceholderDynamic')
    : t('settings.proxyPoolURLPlaceholderStatic'),
)

// ---------- 端口需求 ----------

const portCount = computed(() => proxyPoolPortCount(model.value))
const capacity = computed(() =>
  proxyPoolCapacity(props.maxActive, props.ultraMaxActive, props.warmConcurrency),
)
const spare = computed(() => (capacity.value === null ? 0 : portCount.value - capacity.value.total))
const capacityShown = computed(
  () => model.value.mode === 'static' && portCount.value > 0 && capacity.value !== null,
)

// ---------- 实时状态 ----------

const relayLow = computed(() => {
  const status = props.status
  if (status === null || status.mode === 'off' || status.relay_limit === undefined) return false
  return (
    status.relay_limit > 0 && status.relay_limit < status.capacity * RELAY_CONNECTIONS_PER_BROWSER
  )
})
const pinned = computed(() =>
  props.status !== null && props.status.mode !== 'off' ? props.status.pinned_default : 0,
)

// ---------- 抽测出口 ----------

const sampling = ref(false)
const sample = ref<ProxyTestResult | null>(null)
const sampleError = ref('')
// sampleSeq 在设置改动时递增：进行中的抽测返回时已经对应旧设置，结果丢弃
let sampleSeq = 0

// sampleKey 为抽测结果对应的设置：地址只取协议、host:port 与是否带认证，保存后同步规范值（写法不同、上游相同）时不清空结果
const sampleKey = computed(() => {
  const parts = proxyParts(model.value.url)
  const address =
    parts === null ? model.value.url.trim() : `${parts.scheme}://${parts.hostPort}|${parts.auth}`
  return JSON.stringify([model.value.mode, model.value.port_start, model.value.port_end, address])
})
watch(sampleKey, () => {
  sampleSeq += 1
  sample.value = null
  sampleError.value = ''
})

const sampleDisabled = computed(
  () => sampling.value || model.value.mode === 'off' || invalid.value,
)
const samples = computed(() => sample.value?.samples ?? [])
const sampleWarnings = computed(() =>
  (sample.value?.warnings ?? []).filter((warning) => warning in warningKeys),
)

function probeOK(probe: ProxyTestProbe): probe is ProxyTestProbe & { ip: string } {
  return probe.ip !== undefined && probe.ip !== ''
}

function probeText(probe: ProxyTestProbe): string {
  if (probeOK(probe)) return tf('proxy.testOK', { ip: probe.ip, ms: probe.elapsed_ms })
  return tf('proxy.testFailed', { error: probe.error || t('common.unknown') })
}

function probeClass(probe: ProxyTestProbe): string {
  return probeOK(probe) ? 'text-green-400' : 'text-red-400'
}

// runSample 用输入框里当前的设置抽测几个出口的 IP（不必先保存）；同一时间只跑一次
async function runSample(): Promise<void> {
  if (sampleDisabled.value) return
  sampleSeq += 1
  const seq = sampleSeq
  sample.value = null
  sampleError.value = ''
  sampling.value = true
  emit('testing', true, false)
  let passed = false
  try {
    const response = await api.testProxyPool(cloneProxyPool(model.value), SAMPLE_COUNT)
    if (seq === sampleSeq) {
      sample.value = response
      const items = response.samples ?? []
      passed =
        items.length > 0 && items.every((item) => probeOK(item.browser) && probeOK(item.direct))
    }
  } catch (error) {
    if (seq === sampleSeq) sampleError.value = errorText(error)
  } finally {
    sampling.value = false
    emit('testing', false, passed)
  }
}

// ---------- 检查全部端口 ----------

const check = computed(() => props.status?.check ?? null)
const checkError = ref('')
const checkStarting = ref(false)
// pendingCheck 为发起检查时上一次检查的开始时间（没有时为空串）：状态接口出现新检查之前按进行中显示
const pendingCheck = ref<string | null>(null)
let pendingTimer: number | undefined

function clearPending(): void {
  pendingCheck.value = null
  if (pendingTimer !== undefined) {
    window.clearTimeout(pendingTimer)
    pendingTimer = undefined
  }
}

watch(
  () => check.value?.started_at ?? '',
  (startedAt) => {
    if (pendingCheck.value !== null && startedAt !== pendingCheck.value) clearPending()
  },
)

const checkRunning = computed(
  () => checkStarting.value || pendingCheck.value !== null || check.value?.running === true,
)
const checkDisabled = computed(
  () => checkRunning.value || model.value.mode !== 'static' || invalid.value,
)
const checkProgress = computed(() => {
  const value = check.value
  if (value === null || value.total <= 0) return 0
  return Math.min(100, Math.round((value.done / value.total) * 100))
})
const checkAllDistinct = computed(() => {
  const value = check.value
  return (
    value !== null &&
    !value.running &&
    value.total > 0 &&
    value.done === value.total &&
    value.ok === value.total &&
    value.distinct_ips === value.ok
  )
})

// startCheck 让服务端在后台逐个检查端口段里每个端口的出口 IP；进度与结果经状态接口更新
async function startCheck(): Promise<void> {
  if (checkDisabled.value) return
  checkError.value = ''
  checkStarting.value = true
  const previous = check.value?.started_at ?? ''
  try {
    await api.checkProxyPool(cloneProxyPool(model.value))
    clearPending()
    // 状态接口可能在请求返回前就已经带上了新的检查
    if ((check.value?.started_at ?? '') === previous) {
      pendingCheck.value = previous
      pendingTimer = window.setTimeout(clearPending, CHECK_PENDING_MS)
    }
  } catch (error) {
    checkError.value =
      error instanceof ApiError && error.status === 409
        ? t('settings.proxyPoolCheckBusy')
        : errorText(error)
  } finally {
    checkStarting.value = false
  }
}

// ---------- 改为跟随默认代理 ----------

const following = ref(false)

async function followDefault(): Promise<void> {
  const count = pinned.value
  if (count === 0 || following.value) return
  const confirmed = await confirmAction(
    tf('settings.proxyPoolFollowConfirm', { count }),
    t('settings.proxyPoolFollowAction'),
  )
  if (!confirmed) return
  following.value = true
  try {
    const result = await api.followDefaultProxy()
    emit(
      'notice',
      tf('settings.proxyPoolFollowDone', {
        changed: result.changed,
        running: result.running,
        skipped: result.skipped,
      }),
      'success',
    )
  } catch (error) {
    emit('notice', errorText(error), 'error')
  } finally {
    following.value = false
  }
}

onBeforeUnmount(clearPending)
</script>

<template>
  <!-- 地址输入里有掩码按钮，标题不用 label 包裹，避免点说明文字时触发掩码按钮 -->
  <fieldset class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
    <legend class="sr-only">{{ t('settings.proxyPoolTitle') }}</legend>
    <span class="mb-1 block text-sm font-medium text-gray-300">{{
      t('settings.proxyPoolTitle')
    }}</span>
    <p class="mb-4 text-xs leading-5 text-gray-500">{{ t('settings.proxyPoolHelp') }}</p>

    <label class="block">
      <span class="mb-1 block text-sm font-medium text-gray-400">{{
        t('settings.proxyPoolMode')
      }}</span>
      <UiSelect :model-value="model.mode" :class="INPUT_CLASS" @update:model-value="setMode">
        <option value="off">{{ t('settings.proxyPoolModeOff') }}</option>
        <option value="static">{{ t('settings.proxyPoolModeStatic') }}</option>
        <option value="dynamic">{{ t('settings.proxyPoolModeDynamic') }}</option>
      </UiSelect>
    </label>

    <div class="mt-4">
      <span class="mb-1 block text-sm font-medium text-gray-400">{{
        t('settings.proxyPoolURL')
      }}</span>
      <ProxyInput
        v-model="url"
        :input-class="INPUT_CLASS"
        :help="urlHelp"
        :allow-empty-test="false"
        :testable="false"
        :label="t('settings.proxyPoolURL')"
        :placeholder="urlPlaceholder"
        @commit="emit('commit')"
      />
      <p v-if="urlError !== ''" class="mt-1 text-xs text-red-400" role="alert">{{ urlError }}</p>
    </div>

    <!-- 端口任何模式下都校验：端口不合法时其他模式也显示输入框，否则错误提示无处可改 -->
    <div v-if="model.mode === 'static' || portsError !== ''" class="mt-4">
      <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.proxyPoolPortStart')
          }}</span>
          <input
            v-model.number="portStart"
            :class="INPUT_CLASS"
            type="number"
            min="1"
            max="65535"
            required
            @change="emit('commit')"
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.proxyPoolPortEnd')
          }}</span>
          <input
            v-model.number="portEnd"
            :class="INPUT_CLASS"
            type="number"
            min="1"
            max="65535"
            required
            @change="emit('commit')"
          />
        </label>
      </div>
      <span class="mt-2 block text-xs text-gray-500">{{ t('settings.proxyPoolPortsHelp') }}</span>
      <template v-if="capacityShown && capacity !== null">
        <span class="mt-2 block text-xs text-gray-400">
          {{
            tf('settings.proxyPoolCapacity', {
              ports: portCount,
              need: capacity.total,
              normal: capacity.normal,
              normalDrain: capacity.normalDrain,
              ultra: capacity.ultra,
              ultraDrain: capacity.ultraDrain,
              spare: Math.max(spare, 0),
            })
          }}
        </span>
        <p
          v-if="spare < 0"
          class="mt-2 rounded border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs leading-5 text-amber-200"
          role="note"
        >
          {{ tf('settings.proxyPoolShortage', { need: capacity.total }) }}
        </p>
        <p v-else-if="spare < DEFAULT_VERIFY_CONCURRENCY" class="mt-2 text-xs text-yellow-400">
          {{ tf('settings.proxyPoolTight', { spare }) }}
        </p>
      </template>
    </div>
    <p v-if="portsError !== ''" class="mt-2 text-xs text-red-400" role="alert">{{ portsError }}</p>

    <p v-if="model.mode !== 'off' && hasDefaultProxy" class="mt-3 text-xs leading-5 text-gray-500">
      {{ t('settings.proxyPoolDefaultProxyHint') }}
    </p>
    <p class="mt-3 text-xs leading-5 text-gray-500">{{ t('settings.proxyPoolLiveHint') }}</p>

    <div v-if="model.mode !== 'off'" class="mt-4 border-t border-[#30363d] pt-4">
      <div class="flex flex-wrap items-center gap-2">
        <!-- mousedown.prevent：点按钮时地址输入框不失焦，不会先触发失焦保存，抽测的是输入框里当前的设置 -->
        <button
          class="btn btn-sm"
          type="button"
          :disabled="sampleDisabled"
          :aria-busy="sampling"
          @mousedown.prevent
          @click="runSample"
        >
          <UiIcon v-if="sampling" name="spinner" :size="12" />
          {{ sampling ? t('settings.proxyPoolSampling') : t('settings.proxyPoolSample') }}
        </button>
        <button
          v-if="model.mode === 'static'"
          class="btn btn-sm"
          type="button"
          :disabled="checkDisabled"
          :aria-busy="checkRunning"
          @mousedown.prevent
          @click="startCheck"
        >
          <UiIcon v-if="checkRunning" name="spinner" :size="12" />
          {{ t('settings.proxyPoolCheck') }}
        </button>
      </div>
      <span class="mt-2 block text-xs text-gray-500">{{ t('settings.proxyPoolSampleHint') }}</span>

      <div
        v-if="sample !== null || sampleError !== ''"
        class="mt-3 space-y-1 text-xs"
        aria-live="polite"
      >
        <template v-if="sample !== null">
          <div v-for="item in samples" :key="item.slot" class="break-words">
            <span class="mr-2 font-mono text-gray-300">{{ slotLabel(item.slot, tf) }}</span>
            <span class="mr-1 text-gray-400">{{
              sample.relayed ? t('proxy.testBrowserRelay') : t('proxy.testBrowser')
            }}</span>
            <span class="mr-3" :class="probeClass(item.browser)">{{
              probeText(item.browser)
            }}</span>
            <span class="mr-1 text-gray-400">{{ t('proxy.testDirect') }}</span>
            <span :class="probeClass(item.direct)">{{ probeText(item.direct) }}</span>
          </div>
          <div v-if="sample.distinct_ips !== undefined" class="text-gray-400">
            {{
              tf('settings.proxyPoolSampleSummary', {
                count: samples.length,
                distinct: sample.distinct_ips,
              })
            }}
          </div>
          <div v-for="warning in sampleWarnings" :key="warning" class="text-yellow-400">
            {{ t(warningKeys[warning]) }}
          </div>
        </template>
        <div v-if="sampleError !== ''" class="break-words text-red-400">{{ sampleError }}</div>
      </div>

      <div v-if="model.mode === 'static' && (check !== null || checkError !== '')" class="mt-3">
        <div v-if="checkError !== ''" class="text-xs break-words text-red-400" role="alert">
          {{ checkError }}
        </div>
        <template v-if="check !== null">
          <template v-if="check.running">
            <div class="text-xs text-cyan-300" aria-live="polite">
              {{ tf('settings.proxyPoolCheckRunning', { done: check.done, total: check.total }) }}
            </div>
            <div class="progress-track mt-1">
              <div class="progress-bar" :style="{ width: `${checkProgress}%` }"></div>
            </div>
          </template>
          <div v-else class="text-xs text-gray-300">
            {{
              tf('settings.proxyPoolCheckDone', {
                time: formatClock(check.finished_at ?? check.started_at, locale),
                total: check.total,
                ok: check.ok,
                distinct: check.distinct_ips,
                failed: Math.max(0, check.done - check.ok),
              })
            }}
          </div>
          <div v-if="checkAllDistinct" class="mt-1 text-xs text-green-400">
            {{ t('settings.proxyPoolCheckAllDistinct') }}
          </div>
          <div v-if="(check.duplicates ?? []).length > 0" class="mt-2 text-xs">
            <div class="text-yellow-400">
              {{
                tf('settings.proxyPoolCheckDuplicates', { count: (check.duplicates ?? []).length })
              }}
            </div>
            <ul class="mt-1 max-h-32 space-y-0.5 overflow-auto font-mono text-gray-300">
              <li v-for="group in check.duplicates" :key="group.ip" class="break-words">
                {{ group.ip }}：{{ group.ports.join(', ') }}
              </li>
            </ul>
          </div>
          <div v-if="(check.failed ?? []).length > 0" class="mt-2 text-xs">
            <div class="text-red-400">
              {{ tf('settings.proxyPoolCheckFailed', { count: (check.failed ?? []).length }) }}
            </div>
            <ul class="mt-1 max-h-32 space-y-0.5 overflow-auto text-gray-300">
              <li v-for="failure in check.failed" :key="failure.port" class="break-words">
                <span class="font-mono">{{ failure.port }}</span
                >：{{ failure.error }}
              </li>
            </ul>
          </div>
        </template>
      </div>
    </div>

    <div
      v-if="status !== null"
      class="mt-4 space-y-1 border-t border-[#30363d] pt-4 text-xs text-gray-400"
    >
      <div v-if="status.mode === 'static'">
        {{
          tf('settings.proxyPoolStatusStatic', {
            inUse: status.in_use,
            total: status.total,
            leases: status.leases,
            cooling: status.cooling,
            shared: status.shared,
          })
        }}
      </div>
      <div v-else-if="status.mode === 'dynamic'">
        {{
          tf('settings.proxyPoolStatusDynamic', {
            inUse: status.in_use,
            leases: status.leases,
            rotations: status.rotations ?? 0,
          })
        }}
      </div>
      <div v-if="status.mode === 'static'" class="text-gray-500">
        {{
          tf('settings.proxyPoolAffinity', {
            hits: status.affinity_hits,
            misses: status.affinity_misses,
          })
        }}
      </div>
      <div v-if="status.unpooled > 0" class="text-blue-300">
        {{ tf('settings.proxyPoolUnpooled', { count: status.unpooled }) }}
      </div>
      <div v-if="status.stale > 0">
        {{ tf('settings.proxyPoolStale', { count: status.stale }) }}
      </div>
      <div v-if="status.external > 0" class="text-yellow-400">
        {{ tf('settings.proxyPoolExternal', { count: status.external }) }}
      </div>
      <div v-if="status.orphaned > 0" class="text-yellow-400">
        {{ tf('settings.proxyPoolOrphaned', { count: status.orphaned }) }}
      </div>
      <div v-if="relayLow" class="text-yellow-400">
        {{ tf('settings.proxyPoolRelayLimit', { limit: status.relay_limit ?? 0 }) }}
      </div>
    </div>

    <div
      v-if="pinned > 0"
      class="mt-4 flex flex-wrap items-center gap-3 rounded border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs leading-5 text-amber-200"
      role="note"
    >
      <span class="min-w-0 flex-1">{{ tf('settings.proxyPoolPinned', { count: pinned }) }}</span>
      <button
        class="btn btn-sm"
        type="button"
        :disabled="following"
        :aria-busy="following"
        @click="followDefault"
      >
        {{ t('settings.proxyPoolFollow') }}
      </button>
    </div>
  </fieldset>
</template>
