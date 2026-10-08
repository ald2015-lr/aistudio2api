<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { api } from '@/api'
import { channelLabelKey, useI18n, type TranslationKey } from '@/i18n'
import type { DowngradeGuardConfig, ServiceConfig, UpstreamChannel } from '@/types'
import UiIcon from './UiIcon.vue'
import UiSelect from './UiSelect.vue'
import { readStorage, writeStorage } from '@/storage'

const props = defineProps<{
  config: ServiceConfig | null
  loading: boolean
  error: string
  running: boolean
}>()

const emit = defineEmits<{
  saved: [config: ServiceConfig]
  notice: [message: string, tone: 'success' | 'error']
}>()

type EditableKey =
  | 'auth_states'
  | 'listen_addr'
  | 'proxy_api_key'
  | 'proxy'
  | 'init_timeout'
  | 'request_timeout'
  | 'warm_worker_limit'
  | 'max_active_workers'
  | 'warm_startup_concurrency'
  | 'per_account_concurrency'
  | 'routing_strategy'
  | 'upstream_channels'
  | 'waa_backend'
  | 'temporary_chat'
  | 'ignore_client_seed'
  | 'repeat_prompt_nonce'
  | 'min_output_tokens'
  | 'downgrade_guard'

const EDITABLE_KEYS: readonly EditableKey[] = [
  'auth_states',
  'listen_addr',
  'proxy_api_key',
  'proxy',
  'init_timeout',
  'request_timeout',
  'warm_worker_limit',
  'max_active_workers',
  'warm_startup_concurrency',
  'per_account_concurrency',
  'routing_strategy',
  'upstream_channels',
  'waa_backend',
  'temporary_chat',
  'ignore_client_seed',
  'repeat_prompt_nonce',
  'min_output_tokens',
  'downgrade_guard',
]
// 这几个字段边输入边保存会出问题（例如密钥只输入了一半），改为输入框失去焦点时保存
const BLUR_KEYS: readonly EditableKey[] = ['auth_states', 'listen_addr', 'proxy_api_key', 'proxy']
const AUTO_KEYS: readonly EditableKey[] = EDITABLE_KEYS.filter((key) => !BLUR_KEYS.includes(key))

// defaultDowngradeGuard 与服务端默认值一致
function defaultDowngradeGuard(): DowngradeGuardConfig {
  return {
    enabled: true,
    models: ['gemini-3.1-pro-preview'],
    speed_threshold: 190,
    min_tokens: 150,
    min_window_ms: 2500,
    fuzzy_low: 160,
    fuzzy_high: 220,
    count_timeout_ms: 1500,
    fast_mode: false,
    memory_minutes: 30,
    max_hold_ms: 0,
    reject_status: 400,
  }
}

// cloneDowngradeGuard 深拷贝降级判定设置：表单与已保存的配置不能共用同一个对象，否则修改不会被识别为未保存
function cloneDowngradeGuard(value: DowngradeGuardConfig | undefined): DowngradeGuardConfig {
  const source = value ?? defaultDowngradeGuard()
  // 旧版服务端不返回 reject_status，按默认 400 处理
  return { ...source, models: [...(source.models ?? [])], reject_status: source.reject_status ?? 400 }
}

// parseModelList 与服务端一致：逗号或空白分隔，去掉 models/ 前缀并去重
function parseModelList(value: string): string[] {
  const models: string[] = []
  for (const item of value.split(/[\s,，]+/)) {
    const name = item.trim().replace(/^models\//, '')
    if (name !== '' && !models.includes(name)) models.push(name)
  }
  return models
}
const SAVE_DELAY_MS = 800
// DEFAULT_API_KEY 与服务端 config.DefaultProxyAPIKey 一致：留空保存时服务端使用该默认密钥
const DEFAULT_API_KEY = 'sk-onechat-fun-fun'
const APPLY_DELAY_SECONDS = 5
const AUTO_APPLY_KEY = 'aistudio2api_settings_auto_apply'
const DURATION_PATTERN = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/

const { t, tf } = useI18n()
const revealKey = ref(false)
const saving = ref(false)
const saveError = ref('')
const applying = ref(false)
const countdown = ref(0)
const autoApply = ref(readStorage(AUTO_APPLY_KEY) !== 'false')
const savedConfig = ref<ServiceConfig | null>(null)
const form = reactive<ServiceConfig>({
  auth_states: 'auth',
  listen_addr: '127.0.0.1:2048',
  proxy_api_key: '',
  active_listen_addr: '127.0.0.1:2048',
  active_proxy_api_key: '',
  management_restart_required: false,
  service_restart_required: false,
  proxy: '',
  init_timeout: '2m',
  request_timeout: '5m',
  warm_worker_limit: 5,
  max_active_workers: 10,
  warm_startup_concurrency: 2,
  per_account_concurrency: 2,
  routing_strategy: 'round-robin',
  upstream_channels: ['playground', 'build'],
  waa_backend: 'camoufox',
  temporary_chat: false,
  ignore_client_seed: false,
  repeat_prompt_nonce: true,
  min_output_tokens: 60000,
  downgrade_guard: defaultDowngradeGuard(),
})

const upstreamChannelOptions: UpstreamChannel[] = ['playground', 'build']
// 拦截模型列表的输入框单独保存文字，避免输入逗号时被立即重新格式化
const guardModelsText = ref(form.downgrade_guard.models.join(', '))

function onGuardModelsInput(event: Event): void {
  const value = (event.target as HTMLInputElement).value
  guardModelsText.value = value
  form.downgrade_guard.models = parseModelList(value)
}
let saveTimer: number | undefined
let countdownTimer: number | undefined
let saveChain: Promise<unknown> = Promise.resolve()

// snapshot 序列化可编辑字段，用于判断是否有未保存的修改
function snapshot(source: ServiceConfig, keys: readonly EditableKey[] = EDITABLE_KEYS): string {
  return JSON.stringify(keys.map((key) => source[key]))
}

const dirty = computed(
  () => savedConfig.value !== null && snapshot(form) !== snapshot(savedConfig.value),
)

const statusText = computed(() => {
  if (saving.value) return t('settings.stateSaving')
  if (saveError.value !== '') return tf('settings.stateError', { message: saveError.value })
  if (dirty.value) return t('settings.stateDirty')
  if (props.config?.service_restart_required) return t('settings.stateSavedPending')
  return t('settings.stateSaved')
})

const statusClass = computed(() => {
  if (saveError.value !== '') return 'text-red-400'
  if (saving.value || dirty.value || props.config?.service_restart_required) return 'text-yellow-400'
  return 'text-green-400'
})

// toggleUpstreamChannel 切换通道并保持配置顺序，至少保留一个通道
function toggleUpstreamChannel(channel: UpstreamChannel, event: Event): void {
  const target = event.target
  const enabled = target instanceof HTMLInputElement && target.checked
  const selected = new Set(form.upstream_channels)
  if (enabled) selected.add(channel)
  else if (selected.size > 1) selected.delete(channel)
  form.upstream_channels = upstreamChannelOptions.filter((option) => selected.has(option))
}

function isPositiveInteger(value: unknown): boolean {
  return typeof value === 'number' && Number.isInteger(value) && value > 0
}

function isDuration(value: string): boolean {
  return DURATION_PATTERN.test(value) && /[1-9]/.test(value)
}

const DURATION_UNITS: Record<string, number> = {
  ns: 1e-6,
  us: 1e-3,
  µs: 1e-3,
  ms: 1,
  s: 1000,
  m: 60_000,
  h: 3_600_000,
}

// durationMillis 把 Go 时长写法（如 3m、1m30s、90s）换算为毫秒；无法解析时返回 NaN
function durationMillis(value: string): number {
  const parts = value.trim().matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)
  let total = 0
  let matched = ''
  for (const [text, amount, unit] of parts) {
    total += Number(amount) * DURATION_UNITS[unit!]!
    matched += text
  }
  return matched !== '' && matched === value.trim() ? total : Number.NaN
}

function isListenAddress(value: string): boolean {
  const match = /^(\[[^\]]+\]|[^:]*):(\d{1,5})$/.exec(value)
  if (match === null) return false
  const port = Number(match[2])
  return port >= 1 && port <= 65535
}

// validate 与服务端校验规则一致，返回第一条错误；通过时返回空字符串
function validate(value: ServiceConfig): string {
  const label = (key: TranslationKey): string => t(key)
  if (value.auth_states.trim() === '') {
    return tf('settings.invalidRequired', { field: label('settings.authPath') })
  }
  if (!isListenAddress(value.listen_addr)) return t('settings.invalidListen')
  const durations: [string, TranslationKey][] = [
    [value.init_timeout, 'settings.initTimeout'],
    [value.request_timeout, 'settings.requestTimeout'],
  ]
  for (const [duration, key] of durations) {
    if (!isDuration(duration)) return tf('settings.invalidDuration', { field: label(key) })
  }
  const numbers: [unknown, TranslationKey][] = [
    [value.warm_worker_limit, 'settings.warmWorkerLimit'],
    [value.max_active_workers, 'settings.maxActiveWorkers'],
    [value.warm_startup_concurrency, 'settings.warmStartupConcurrency'],
    [value.per_account_concurrency, 'settings.perAccountConcurrency'],
  ]
  for (const [number, key] of numbers) {
    if (!isPositiveInteger(number)) return tf('settings.invalidNumber', { field: label(key) })
  }
  const floor = value.min_output_tokens
  if (typeof floor !== 'number' || !Number.isInteger(floor) || floor < 0) {
    return tf('settings.invalidNumber', { field: label('settings.minOutputTokens') })
  }
  const guard = value.downgrade_guard
  if (guard !== undefined && guard.enabled) {
    if (guard.models.length === 0) return t('settings.invalidDowngradeModels')
    const positives: [unknown, TranslationKey][] = [
      [guard.speed_threshold, 'settings.downgradeSpeed'],
      [guard.fuzzy_low, 'settings.downgradeFuzzyLow'],
      [guard.fuzzy_high, 'settings.downgradeFuzzyHigh'],
    ]
    for (const [number, key] of positives) {
      if (typeof number !== 'number' || !Number.isFinite(number) || number <= 0) {
        return tf('settings.invalidNumber', { field: label(key) })
      }
    }
    if (!isPositiveInteger(guard.min_tokens)) {
      return tf('settings.invalidNumber', { field: label('settings.downgradeMinTokens') })
    }
    const bounded: [unknown, TranslationKey, number][] = [
      [guard.min_window_ms, 'settings.downgradeMinWindow', 60000],
      [guard.count_timeout_ms, 'settings.downgradeCountTimeout', 30000],
      [guard.memory_minutes, 'settings.downgradeMemory', Number.MAX_SAFE_INTEGER],
      [guard.max_hold_ms, 'settings.downgradeMaxHold', 600000],
    ]
    for (const [number, key, max] of bounded) {
      if (typeof number !== 'number' || !Number.isInteger(number) || number < 0 || number > max) {
        return tf('settings.invalidNumber', { field: label(key) })
      }
    }
    if (guard.fuzzy_high < guard.fuzzy_low) return t('settings.invalidDowngradeFuzzy')
  }
  if (value.max_active_workers < value.warm_worker_limit) return t('settings.invalidMaxWorkers')
  if (value.warm_startup_concurrency > value.warm_worker_limit) {
    return t('settings.invalidWarmConcurrency')
  }
  return ''
}

// payload 构造保存内容；自动保存时所有失焦字段沿用已保存的值，避免把输入到一半的内容写进去
function payload(includeBlurFields: boolean): ServiceConfig {
  const base = savedConfig.value
  if (includeBlurFields || base === null) return { ...form }
  return { ...form, ...Object.fromEntries(BLUR_KEYS.map((key) => [key, base[key]])) }
}

function cancelCountdown(): void {
  if (countdownTimer !== undefined) {
    window.clearInterval(countdownTimer)
    countdownTimer = undefined
  }
  countdown.value = 0
}

function startCountdown(): void {
  cancelCountdown()
  countdown.value = APPLY_DELAY_SECONDS
  countdownTimer = window.setInterval(() => {
    countdown.value -= 1
    if (countdown.value <= 0) {
      cancelCountdown()
      void applyNow()
    }
  }, 1000)
}

async function doSave(includeBlurFields: boolean): Promise<ServiceConfig | null> {
  const invalid = validate(form)
  if (invalid !== '') {
    saveError.value = invalid
    return null
  }
  saving.value = true
  try {
    const saved = await api.saveConfig(payload(includeBlurFields))
    savedConfig.value = saved
    // 留空的密钥由服务端换成默认密钥，表单同步显示，避免一直提示未保存
    if (form.proxy_api_key.trim() === '') form.proxy_api_key = saved.proxy_api_key
    // 服务端把时长写成规范形式（3m 保存为 3m0s、90s 保存为 1m30s），时长相同时表单同步显示，避免一直提示未保存
    for (const key of ['init_timeout', 'request_timeout'] as const) {
      if (form[key] !== saved[key] && durationMillis(form[key]) === durationMillis(saved[key])) {
        form[key] = saved[key]
      }
    }
    saveError.value = ''
    emit('saved', saved)
    if (saved.service_restart_required && props.running && autoApply.value && !applying.value) {
      startCountdown()
    }
    return saved
  } catch (error) {
    saveError.value = error instanceof Error ? error.message : t('common.error')
    return null
  } finally {
    saving.value = false
  }
}

// save 串行执行保存，避免并发请求乱序覆盖
function save(includeBlurFields: boolean): Promise<ServiceConfig | null> {
  const run = saveChain.then(() => doSave(includeBlurFields))
  saveChain = run.catch(() => undefined)
  return run
}

function scheduleSave(): void {
  cancelCountdown()
  if (saveTimer !== undefined) window.clearTimeout(saveTimer)
  saveTimer = window.setTimeout(() => {
    saveTimer = undefined
    void save(false)
  }, SAVE_DELAY_MS)
}

// commitBlurField 失焦字段在输入框失去焦点或按回车时保存
function commitBlurField(): void {
  const base = savedConfig.value
  if (base === null || snapshot(form, BLUR_KEYS) === snapshot(base, BLUR_KEYS)) return
  void save(true)
}

// applyNow 等待进行中的请求结束后重启生成服务，让已保存的配置生效
async function applyNow(): Promise<void> {
  cancelCountdown()
  if (applying.value) return
  applying.value = true
  try {
    await api.restartService()
    const fresh = await api.config()
    savedConfig.value = fresh
    emit('saved', fresh)
    emit('notice', t('settings.applied'), 'success')
  } catch (error) {
    emit('notice', error instanceof Error ? error.message : t('common.error'), 'error')
  } finally {
    applying.value = false
  }
}

// saveAndApply 立即保存全部字段，需要时马上应用
async function saveAndApply(): Promise<void> {
  if (saveTimer !== undefined) {
    window.clearTimeout(saveTimer)
    saveTimer = undefined
  }
  const saved = await save(true)
  if (saved === null) {
    if (saveError.value !== '') emit('notice', saveError.value, 'error')
    return
  }
  if (saved.service_restart_required && props.running) {
    await applyNow()
  } else if (saved.service_restart_required) {
    emit('notice', t('settings.savedStopped'), 'success')
  } else {
    emit('notice', t('settings.saved'), 'success')
  }
}

watch(
  () => props.config,
  (config) => {
    if (config === null) return
    const base = savedConfig.value
    const hasLocalEdits = base !== null && snapshot(form) !== snapshot(base)
    if (!hasLocalEdits) {
      Object.assign(form, config)
      form.downgrade_guard = cloneDowngradeGuard(config.downgrade_guard)
      guardModelsText.value = form.downgrade_guard.models.join(', ')
    }
    savedConfig.value = config
  },
  { immediate: true },
)

// 普通字段修改后自动保存
watch(
  () => snapshot(form, AUTO_KEYS),
  (value) => {
    const base = savedConfig.value
    if (base === null || value === snapshot(base, AUTO_KEYS)) return
    scheduleSave()
  },
)

watch(autoApply, (value) => {
  writeStorage(AUTO_APPLY_KEY, String(value))
  if (!value) cancelCountdown()
})

onBeforeUnmount(() => {
  if (saveTimer !== undefined) {
    window.clearTimeout(saveTimer)
    void save(false)
  }
  cancelCountdown()
})
</script>

<template>
  <section class="mx-auto w-full max-w-3xl flex-1 overflow-auto p-4 md:p-8">
    <div
      class="mb-6 flex flex-wrap items-center justify-between gap-3 border-b border-[#30363d] pb-2"
    >
      <h2 class="text-2xl font-bold text-white">{{ t('section.settings.title') }}</h2>
      <div v-if="config !== null" class="flex flex-wrap items-center gap-3 text-xs">
        <span :class="statusClass">{{ statusText }}</span>
        <label class="flex cursor-pointer items-center gap-1.5 text-gray-400">
          <input v-model="autoApply" type="checkbox" />
          {{ t('settings.autoApply') }}
        </label>
      </div>
    </div>

    <div v-if="error" class="rounded border border-red-500/40 bg-red-500/10 p-4 text-red-300">
      {{ error }}
    </div>
    <div v-else-if="loading || config === null" class="py-12 text-center text-gray-500">
      {{ t('common.loading') }}
    </div>
    <form v-else class="space-y-6" @submit.prevent="saveAndApply">
      <p class="text-xs leading-5 text-gray-500">{{ t('settings.liveHint') }}</p>
      <div
        v-if="applying"
        class="flex items-center gap-2 rounded border border-cyan-500/30 bg-cyan-500/10 px-4 py-3 text-sm text-cyan-200"
      >
        <UiIcon name="spinner" :size="14" />
        {{ t('settings.applying') }}
      </div>
      <div
        v-else-if="config.service_restart_required"
        class="flex flex-wrap items-center gap-3 rounded border border-blue-500/30 bg-blue-500/10 px-4 py-3 text-sm text-blue-200"
      >
        <span class="min-w-0 flex-1">
          {{ running ? t('settings.applyPending') : t('settings.savedStopped') }}
        </span>
        <template v-if="running">
          <span v-if="countdown > 0" class="font-mono text-xs text-blue-300">
            {{ tf('settings.applyCountdown', { seconds: countdown }) }}
          </span>
          <button class="btn btn-sm btn-primary" type="button" @click="applyNow">
            {{ t('settings.applyNow') }}
          </button>
          <button v-if="countdown > 0" class="btn btn-sm" type="button" @click="cancelCountdown">
            {{ t('common.cancel') }}
          </button>
        </template>
      </div>
      <div
        v-if="config.management_restart_required"
        class="rounded border border-amber-500/30 bg-amber-500/10 px-4 py-3 text-sm text-amber-200"
      >
        {{ t('settings.pendingManagement') }}
      </div>

      <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.authPath')
          }}</span>
          <input
            v-model.trim="form.auth_states"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            required
            autocomplete="off"
            @change="commitBlurField"
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.listen')
          }}</span>
          <input
            v-model.trim="form.listen_addr"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            required
            autocomplete="off"
            @change="commitBlurField"
          />
          <span
            v-if="form.listen_addr !== config.active_listen_addr"
            class="mt-1 block text-xs text-gray-500"
          >
            {{ t('settings.activeValue') }}: {{ config.active_listen_addr }}
          </span>
        </label>
      </div>

      <div class="rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-300">{{
            t('settings.apiKey')
          }}</span>
          <div class="flex gap-2">
            <input
              v-model="form.proxy_api_key"
              :type="revealKey ? 'text' : 'password'"
              :placeholder="DEFAULT_API_KEY"
              class="min-w-0 flex-1 rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              autocomplete="new-password"
              @change="commitBlurField"
            />
            <button
              class="rounded border border-[#30363d] bg-[#21262d] px-3 text-xs text-gray-300 transition hover:bg-[#30363d]"
              type="button"
              @click="revealKey = !revealKey"
            >
              {{ revealKey ? t('settings.hide') : t('settings.reveal') }}
            </button>
          </div>
          <span class="mt-1 block text-xs text-gray-500">{{ t('settings.apiKeyHot') }}</span>
          <span class="mt-1 block text-xs text-gray-500">{{
            tf('settings.apiKeyDefault', { key: DEFAULT_API_KEY })
          }}</span>
        </label>
      </div>

      <div class="rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-300">{{
            t('settings.proxy')
          }}</span>
          <input
            v-model.trim="form.proxy"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            placeholder="http://127.0.0.1:7890"
            autocomplete="off"
            @change="commitBlurField"
          />
        </label>
      </div>

      <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.initTimeout')
          }}</span>
          <input
            v-model.trim="form.init_timeout"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            required
            autocomplete="off"
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.requestTimeout')
          }}</span>
          <input
            v-model.trim="form.request_timeout"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            required
            autocomplete="off"
          />
        </label>
      </div>

      <div class="grid grid-cols-1 gap-4 md:grid-cols-4">
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.warmWorkerLimit')
          }}</span>
          <input
            v-model.number="form.warm_worker_limit"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            type="number"
            min="1"
            required
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.maxActiveWorkers')
          }}</span>
          <input
            v-model.number="form.max_active_workers"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            type="number"
            :min="form.warm_worker_limit"
            required
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.warmStartupConcurrency')
          }}</span>
          <input
            v-model.number="form.warm_startup_concurrency"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            type="number"
            min="1"
            :max="form.warm_worker_limit"
            required
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-sm font-medium text-gray-400">{{
            t('settings.perAccountConcurrency')
          }}</span>
          <input
            v-model.number="form.per_account_concurrency"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            type="number"
            min="1"
            required
          />
        </label>
      </div>

      <label class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <span class="mb-2 block text-sm font-medium text-gray-300">{{
          t('settings.routingStrategy')
        }}</span>
        <UiSelect
          v-model="form.routing_strategy"
          class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
        >
          <option value="round-robin">{{ t('settings.routingRoundRobin') }}</option>
          <option value="fill-first">{{ t('settings.routingFillFirst') }}</option>
        </UiSelect>
        <span class="mt-2 block text-xs text-gray-500">{{ t('settings.routingHelp') }}</span>
      </label>

      <label class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <span class="mb-2 block text-sm font-medium text-gray-300">{{
          t('settings.waaBackend')
        }}</span>
        <UiSelect
          v-model="form.waa_backend"
          class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
        >
          <option value="camoufox">{{ t('settings.waaBackendCamoufox') }}</option>
          <option value="go">{{ t('settings.waaBackendGo') }}</option>
        </UiSelect>
        <span class="mt-2 block text-xs text-gray-500">{{ t('settings.waaBackendHelp') }}</span>
      </label>

      <fieldset class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <legend class="sr-only">{{ t('settings.upstreamChannels') }}</legend>
        <span class="mb-2 block text-sm font-medium text-gray-300">{{
          t('settings.upstreamChannels')
        }}</span>
        <div class="flex flex-wrap gap-4">
          <label
            v-for="channel in upstreamChannelOptions"
            :key="channel"
            class="flex items-center gap-2 text-sm text-gray-300"
          >
            <input
              class="h-4 w-4 accent-blue-500"
              type="checkbox"
              :checked="form.upstream_channels.includes(channel)"
              :disabled="
                form.upstream_channels.length === 1 && form.upstream_channels.includes(channel)
              "
              @change="toggleUpstreamChannel(channel, $event)"
            />
            {{ t(channelLabelKey(channel)) }}
          </label>
        </div>
        <span class="mt-2 block text-xs text-gray-500">{{
          t('settings.upstreamChannelsHelp')
        }}</span>
      </fieldset>

      <label class="flex items-center gap-3 rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <input v-model="form.temporary_chat" class="h-4 w-4 accent-blue-500" type="checkbox" />
        <span class="text-sm font-medium text-gray-300">{{ t('settings.temporaryChat') }}</span>
      </label>

      <label class="flex items-start gap-3 rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <input v-model="form.ignore_client_seed" class="mt-0.5 h-4 w-4 accent-blue-500" type="checkbox" />
        <span>
          <span class="block text-sm font-medium text-gray-300">{{ t('settings.ignoreClientSeed') }}</span>
          <span class="mt-1 block text-xs text-gray-500">{{ t('settings.ignoreClientSeedHelp') }}</span>
        </span>
      </label>

      <label class="flex items-start gap-3 rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <input v-model="form.repeat_prompt_nonce" class="mt-0.5 h-4 w-4 accent-blue-500" type="checkbox" />
        <span>
          <span class="block text-sm font-medium text-gray-300">{{ t('settings.repeatPromptNonce') }}</span>
          <span class="mt-1 block text-xs text-gray-500">{{ t('settings.repeatPromptNonceHelp') }}</span>
        </span>
      </label>

      <label class="block rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.minOutputTokens') }}</span>
        <input
          v-model.number="form.min_output_tokens"
          class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
          type="number"
          min="0"
          step="1000"
          required
        />
        <span class="mt-2 block text-xs text-gray-500">{{ t('settings.minOutputTokensHelp') }}</span>
      </label>

      <div class="rounded-lg border border-[#30363d] bg-[#161b22] p-4">
        <label class="flex items-start gap-3">
          <input v-model="form.downgrade_guard.enabled" class="mt-0.5 h-4 w-4 accent-blue-500" type="checkbox" />
          <span>
            <span class="block text-sm font-medium text-gray-300">{{ t('settings.downgradeGuard') }}</span>
            <span class="mt-1 block text-xs text-gray-500">{{ t('settings.downgradeGuardHelp') }}</span>
          </span>
        </label>
        <div v-if="form.downgrade_guard.enabled" class="mt-4 space-y-4 border-t border-[#30363d] pt-4">
          <label class="block">
            <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeModels') }}</span>
            <input
              :value="guardModelsText"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              type="text"
              spellcheck="false"
              autocomplete="off"
              @input="onGuardModelsInput"
            />
            <span class="mt-2 block text-xs text-gray-500">{{ t('settings.downgradeModelsHelp') }}</span>
          </label>
          <div class="grid gap-4 sm:grid-cols-3">
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeSpeed') }}</span>
              <input
                v-model.number="form.downgrade_guard.speed_threshold"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="1"
                step="5"
                required
              />
            </label>
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeMinTokens') }}</span>
              <input
                v-model.number="form.downgrade_guard.min_tokens"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="1"
                step="10"
                required
              />
            </label>
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeMinWindow') }}</span>
              <input
                v-model.number="form.downgrade_guard.min_window_ms"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="0"
                step="100"
                required
              />
            </label>
          </div>
          <span class="block text-xs text-gray-500">{{ t('settings.downgradeSpeedHelp') }}</span>
          <div class="grid gap-4 sm:grid-cols-3">
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeFuzzyLow') }}</span>
              <input
                v-model.number="form.downgrade_guard.fuzzy_low"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="1"
                step="5"
                required
              />
            </label>
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeFuzzyHigh') }}</span>
              <input
                v-model.number="form.downgrade_guard.fuzzy_high"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="1"
                step="5"
                required
              />
            </label>
            <label class="block">
              <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeCountTimeout') }}</span>
              <input
                v-model.number="form.downgrade_guard.count_timeout_ms"
                class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                type="number"
                min="0"
                step="100"
                required
              />
            </label>
          </div>
          <span class="block text-xs text-gray-500">{{ t('settings.downgradeFuzzyHelp') }}</span>
          <label class="block">
            <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeMaxHold') }}</span>
            <input
              v-model.number="form.downgrade_guard.max_hold_ms"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              type="number"
              min="0"
              step="500"
              required
            />
            <span class="mt-2 block text-xs text-gray-500">{{ t('settings.downgradeMaxHoldHelp') }}</span>
          </label>
          <label class="flex items-start gap-3">
            <input v-model="form.downgrade_guard.fast_mode" class="mt-0.5 h-4 w-4 accent-blue-500" type="checkbox" />
            <span>
              <span class="block text-sm font-medium text-gray-300">{{ t('settings.downgradeFastMode') }}</span>
              <span class="mt-1 block text-xs text-gray-500">{{ t('settings.downgradeFastModeHelp') }}</span>
            </span>
          </label>
          <label class="block">
            <span class="mb-2 block text-sm font-medium text-gray-300">{{
              t('settings.downgradeRejectStatus')
            }}</span>
            <UiSelect
              v-model="form.downgrade_guard.reject_status"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
            >
              <option :value="400">{{ t('settings.downgradeReject400') }}</option>
              <option :value="503">{{ t('settings.downgradeReject503') }}</option>
            </UiSelect>
            <span class="mt-2 block text-xs text-gray-500">{{ t('settings.downgradeRejectStatusHelp') }}</span>
          </label>
          <label class="block">
            <span class="mb-2 block text-sm font-medium text-gray-300">{{ t('settings.downgradeMemory') }}</span>
            <input
              v-model.number="form.downgrade_guard.memory_minutes"
              class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
              type="number"
              min="0"
              step="5"
              required
            />
            <span class="mt-2 block text-xs text-gray-500">{{ t('settings.downgradeMemoryHelp') }}</span>
          </label>
        </div>
      </div>

      <div class="flex items-center justify-end gap-3 pt-4">
        <span class="text-xs" :class="statusClass">{{ statusText }}</span>
        <button
          class="btn btn-primary h-9 px-5"
          type="submit"
          :disabled="saving || applying"
          :aria-busy="saving || applying"
        >
          <UiIcon name="check" :size="16" />
          {{ t('settings.saveAndApply') }}
        </button>
      </div>
    </form>
  </section>
</template>
