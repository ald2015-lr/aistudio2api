<script setup lang="ts">
import { computed, onUnmounted, reactive, ref, watch } from 'vue'
import {
  ApiError,
  StreamIncompleteError,
  playgroundPrefix,
  runPlayground,
  type PlaygroundChunk,
} from '@/api'
import { copyText } from '@/clipboard'
import { useI18n, type TranslationKey } from '@/i18n'
import type {
  Account,
  Model,
  PlaygroundError,
  PlaygroundInput,
  PlaygroundMode,
  PlaygroundProtocol,
  PlaygroundResult,
  PlaygroundTool,
} from '@/types'
import UiIcon from './UiIcon.vue'
import UiSelect from './UiSelect.vue'

// 显式命名：App.vue 的 KeepAlive 按名称只缓存这个面板，切到其他页时进行中的请求不中止、结果保留
defineOptions({ name: 'PlaygroundPanel' })

const props = defineProps<{
  models: Model[]
  accounts: Account[]
  apiKey: string
}>()

const { t, tf, errorText } = useI18n()
const protocols: { id: PlaygroundProtocol; label: string }[] = [
  { id: 'openai-chat', label: 'OpenAI Chat' },
  { id: 'openai-responses', label: 'OpenAI Responses' },
  { id: 'anthropic', label: 'Anthropic Messages' },
  { id: 'gemini', label: 'Gemini GenerateContent' },
]
const modes: { id: PlaygroundMode; label?: TranslationKey }[] = [
  { id: 'text', label: 'playground.modeText' },
  { id: 'image', label: 'playground.modeImage' },
  { id: 'speech', label: 'playground.modeSpeech' },
  { id: 'music', label: 'playground.modeMusic' },
  { id: 'video' },
]
const tools: { id: Exclude<PlaygroundTool, ''>; label: string; capability: string }[] = [
  { id: 'web_search', label: 'Google Search', capability: 'google_search' },
  { id: 'image_search', label: 'Image Search', capability: 'image_search' },
  { id: 'code_interpreter', label: 'Code Execution', capability: 'code_execution' },
  { id: 'url_context', label: 'URL Context', capability: 'browse' },
  { id: 'google_maps', label: 'Google Maps', capability: 'google_maps' },
]

const form = reactive<PlaygroundInput>({
  mode: 'text',
  protocol: 'openai-chat',
  model: '',
  prompt: '',
  system: '',
  stream: true,
  reasoning: '',
  tool: '',
  imageSize: 'auto',
  imageQuality: 'auto',
  voice: '',
  apiKey: props.apiKey,
  ultra: false,
})
const result = reactive<PlaygroundResult>({
  text: '',
  reasoning: '',
  tools: '',
  media: [],
  raw: '',
  durationMs: 0,
  status: 0,
  finish: '',
  completed: false,
  stopped: false,
})
const isRunning = ref(false)
const outputMode = ref<'output' | 'raw'>('output')
const copied = ref(false)
const hasRun = ref(false)
const submittedPrompt = ref('')
const submittedModel = ref('')
// received 为本次请求已收到事件；之前显示“等待响应 Ns”（waitSeconds 每秒刷新），之后显示“生成中”
const received = ref(false)
const waitSeconds = ref(0)
let waitTimer: number | undefined
let controller: AbortController | undefined
// stopRequested 为用户点了停止：中止后显示“已停止”；清空对话引起的中止不显示
let stopRequested = false
// lastInput 为上一次实际发出的输入快照：重试按它重发，输出类型、协议、号池与模型保持一致，不与之后改动的表单拼在一起
let lastInput: PlaygroundInput | undefined

// ultraModelIDs 为 Ultra 号池账户的模型并集：使用 Ultra 号池时模型列表只显示这些模型，还没有读到时显示全部模型。
// 账户的 models 只含 Playground 目录，只在 Build 通道提供的模型（如 gemini-4-argon）不会出现在这里，过滤时要放过它们
const ultraModelIDs = computed(
  () =>
    new Set(
      props.accounts
        .filter((account) => account.pool === 'ultra')
        .flatMap((account) => account.models),
    ),
)
const availableModels = computed(() =>
  props.models.filter((model) => {
    // 只在 Build 通道提供的模型无法按账户的 Playground 目录判断 Ultra 号池能否服务，照常列出，由服务端按号池选号；
    // 否则勾选“使用 Ultra 号池”后这类模型会被滤掉，没法在这里验证 Ultra 号池（含并入的 Pro 账户）能否使用它们
    const channels = model.channels ?? []
    const buildOnly = channels.includes('build') && !channels.includes('playground')
    if (
      form.ultra &&
      ultraModelIDs.value.size > 0 &&
      !buildOnly &&
      !ultraModelIDs.value.has(model.id)
    ) {
      return false
    }
    const capabilities = model.capabilities ?? {}
    if (form.mode === 'image') return capabilities.image_route === true
    if (form.mode === 'speech') return capabilities.speech_route === true
    if (form.mode === 'music') return capabilities.music_route === true
    if (form.mode === 'video') return capabilities.video_route === true
    return (
      capabilities.chat_model === true ||
      (model.methods.includes('generateContent') &&
        capabilities.image_route !== true &&
        capabilities.speech_route !== true &&
        capabilities.music_route !== true &&
        capabilities.video_route !== true)
    )
  }),
)
const selectedModel = computed(() => availableModels.value.find((model) => model.id === form.model))
const supportsThinking = computed(() => {
  const capabilities = selectedModel.value?.capabilities ?? {}
  return (
    capabilities.thinking === true ||
    capabilities.thinking_budget === true ||
    capabilities.thinking_level === true
  )
})
const availableTools = computed(() => {
  const capabilities = selectedModel.value?.capabilities ?? {}
  return tools.filter((tool) => capabilities[tool.capability] === true)
})
const voices = computed(() => selectedModel.value?.capability_options?.voices ?? [])

function modeLabel(mode: (typeof modes)[number]): string {
  return mode.label === undefined ? 'Veo' : t(mode.label)
}

watch(
  [() => props.models, () => form.mode, () => form.ultra, ultraModelIDs],
  () => {
    if (!availableModels.value.some((model) => model.id === form.model)) {
      form.model = availableModels.value[0]?.id ?? ''
    }
  },
  { immediate: true },
)

watch(selectedModel, () => {
  if (!supportsThinking.value) form.reasoning = ''
  if (!availableTools.value.some((tool) => tool.id === form.tool)) form.tool = ''
  if (!voices.value.includes(form.voice)) form.voice = voices.value[0] ?? ''
})

watch(
  () => props.apiKey,
  (apiKey) => {
    form.apiKey = apiKey
  },
)

// endpointPath 为当前输出类型与协议的公开 API 路径，endpoint 加上号池前缀
const endpointPath = computed(() => {
  if (form.mode === 'image') return '/v1/images/generations'
  if (form.mode === 'speech') return '/v1/audio/speech'
  if (form.mode === 'music') return `/v1beta/models/${form.model || '{model}'}:generateContent`
  if (form.mode === 'video') return '/v1/videos'
  if (form.protocol === 'openai-chat') return '/v1/chat/completions'
  if (form.protocol === 'openai-responses') return '/v1/responses'
  if (form.protocol === 'anthropic') return '/v1/messages'
  const method = form.stream ? 'streamGenerateContent?alt=sse' : 'generateContent'
  return `/v1beta/models/${form.model || '{model}'}:${method}`
})
const endpoint = computed(() => `${playgroundPrefix(form)}${endpointPath.value}`)

const visibleOutput = computed(() => {
  if (outputMode.value === 'raw') return result.raw
  return [
    result.text,
    result.reasoning,
    result.tools,
    ...result.media.map((media) => media.url),
    result.error?.message ?? '',
  ]
    .filter(Boolean)
    .join('\n')
})
// emptyNotice 为正常结束却没有正文、工具调用与媒体时的提示：只有思考时单独说明，没有结束原因时显示“未知”
const emptyNotice = computed(() => {
  if (!result.completed || result.error !== undefined || result.stopped) return ''
  if (result.text.trim() !== '' || result.tools !== '' || result.media.length > 0) return ''
  const reason = result.finish || t('playground.finishUnknown')
  return tf(result.reasoning === '' ? 'playground.emptyReply' : 'playground.emptyThinkingOnly', {
    reason,
  })
})
// showReply 为回复框有内容可显示：原始响应、正文、等待或生成中、空回复提示、已停止；只有错误或只有媒体时不显示空框
const showReply = computed(
  () =>
    outputMode.value === 'raw' ||
    result.text !== '' ||
    isRunning.value ||
    emptyNotice.value !== '' ||
    result.stopped,
)

// clearMedia 释放浏览器创建的媒体资源
function clearMedia(): void {
  for (const media of result.media) {
    if (media.url.startsWith('blob:')) URL.revokeObjectURL(media.url)
  }
  result.media = []
}

// resetResult 清空上一次的结果、结束原因、错误与停止标记
function resetResult(): void {
  clearMedia()
  result.text = ''
  result.reasoning = ''
  result.tools = ''
  result.raw = ''
  result.durationMs = 0
  result.status = 0
  result.finish = ''
  result.completed = false
  result.stopped = false
  delete result.error
}

// appendChunk 追加一次协议解析结果，记录最后一个非空的结束原因
function appendChunk(chunk: PlaygroundChunk): void {
  result.text += chunk.text
  result.reasoning += chunk.reasoning
  result.tools += chunk.tools
  result.media.push(...chunk.media)
  if (chunk.finish !== '') result.finish = chunk.finish
}

// startWaiting 开始“等待响应”计时：每秒刷新，收到首个事件或请求结束时停止
function startWaiting(started: number): void {
  stopWaiting()
  received.value = false
  waitSeconds.value = 0
  waitTimer = window.setInterval(() => {
    waitSeconds.value = Math.floor((performance.now() - started) / 1000)
  }, 1000)
}

function stopWaiting(): void {
  if (waitTimer !== undefined) window.clearInterval(waitTimer)
  waitTimer = undefined
}

// playgroundError 把请求异常整理为错误卡片的内容：流在结束事件前断开、读响应体时连接中断都按“已连接后失败”显示本地化说明
// （中断时带上浏览器给出的原因）；只有 fetch 本身连不上服务时显示服务离线
function playgroundError(error: unknown): PlaygroundError {
  if (error instanceof ApiError) {
    return {
      status: error.status,
      code: error.code,
      message: error.message || t('common.error'),
      inStream: error.inStream,
      ...(error.retryAfter === undefined ? {} : { retryAfter: error.retryAfter }),
    }
  }
  if (error instanceof StreamIncompleteError) {
    return {
      status: error.status,
      code: '',
      message:
        error.reason === ''
          ? t('playground.streamIncomplete')
          : tf('playground.streamInterrupted', { reason: error.reason }),
      inStream: true,
    }
  }
  return { status: 0, code: '', message: errorText(error), inStream: false }
}

// execute 发送输入框中的消息
function execute(): void {
  if (isRunning.value || form.model === '' || form.prompt.trim() === '') return
  const input = { ...form }
  form.prompt = ''
  void run(input)
}

// retry 重发上一次实际发出的请求（只换成当前填写的 API Key）：之后切换输出类型等不影响重试，免得把模型发到不匹配的接口
function retry(): void {
  if (isRunning.value || lastInput === undefined) return
  void run({ ...lastInput, apiKey: form.apiKey })
}

// run 发送公开协议请求并实时追加结果；失败时保留已收到的内容并显示错误卡片
async function run(input: PlaygroundInput): Promise<void> {
  const current = new AbortController()
  controller = current
  stopRequested = false
  lastInput = { ...input }
  submittedPrompt.value = input.prompt
  submittedModel.value = input.model
  hasRun.value = true
  resetResult()
  outputMode.value = 'output'
  isRunning.value = true
  const started = performance.now()
  startWaiting(started)

  try {
    const response = await runPlayground(input, current.signal, (chunk, raw) => {
      if (!received.value) {
        received.value = true
        stopWaiting()
      }
      appendChunk(chunk)
      result.raw += `${raw}\n`
    })
    result.status = response.status
    if (response.chunk !== undefined) appendChunk(response.chunk)
    result.raw += response.raw ?? ''
    result.completed = true
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') {
      result.stopped = stopRequested
      return
    }
    result.error = playgroundError(error)
    result.status = result.error.status
  } finally {
    stopWaiting()
    result.durationMs = Math.round(performance.now() - started)
    isRunning.value = false
  }
}

// clearConversation 清空当前试用结果
function clearConversation(): void {
  stopRequested = false
  controller?.abort()
  hasRun.value = false
  lastInput = undefined
  submittedPrompt.value = ''
  submittedModel.value = ''
  resetResult()
}

// stop 中止当前浏览器请求并触发服务端取消
function stop(): void {
  stopRequested = true
  controller?.abort()
}

// copyOutput 复制当前可见响应；非 HTTPS 页面没有 Clipboard API，由 copyText 退回兼容方式
async function copyOutput(): Promise<void> {
  if (!(await copyText(visibleOutput.value))) return
  copied.value = true
  window.setTimeout(() => {
    copied.value = false
  }, 1200)
}

// 面板由 KeepAlive 缓存，切页不会触发这里；只在真正卸载时中止请求并释放媒体
onUnmounted(() => {
  stopWaiting()
  controller?.abort()
  clearMedia()
})
</script>

<template>
  <section class="flex min-h-0 flex-1 flex-col overflow-hidden md:flex-row">
    <div class="flex min-w-0 flex-1 flex-col bg-[#0d1117]">
      <div class="flex-1 space-y-4 overflow-auto p-4">
        <div v-if="!hasRun" class="flex h-full flex-col items-center justify-center text-gray-500">
          <UiIcon name="chatBubble" :size="64" />
          <p>{{ t('playground.waiting') }}</p>
        </div>

        <div v-else class="mx-auto flex max-w-3xl gap-4">
          <div
            class="flex h-8 w-8 shrink-0 select-none items-center justify-center rounded-full bg-blue-600"
          >
            <span class="text-xs font-bold">U</span>
          </div>
          <div class="min-w-0 flex-1 space-y-2">
            <div
              class="rounded-lg border border-[#30363d] bg-[#161b22] p-3 text-sm leading-relaxed text-gray-200 shadow-sm"
            >
              <div class="whitespace-pre-wrap">{{ submittedPrompt }}</div>
            </div>
          </div>
        </div>

        <div v-if="hasRun" class="mx-auto flex max-w-3xl gap-4">
          <div
            class="flex h-8 w-8 shrink-0 select-none items-center justify-center rounded-full bg-green-600"
            :class="{ 'animate-pulse': isRunning }"
          >
            <span class="text-xs font-bold">AI</span>
          </div>
          <div class="min-w-0 flex-1 space-y-2">
            <details
              v-if="result.reasoning"
              class="group/thinking overflow-hidden rounded-lg border border-blue-900/30 bg-[#0d1117]"
              open
            >
              <summary
                class="flex cursor-pointer list-none items-center gap-2 bg-[#161b22] px-3 py-2 text-xs font-medium text-gray-400 transition select-none hover:bg-[#1c2128]"
              >
                <UiIcon name="chevronRight" :size="12" />
                <span class="text-blue-400">{{ t('playground.reasoningOutput') }}</span>
                <span v-if="isRunning" class="animate-pulse">...</span>
              </summary>
              <pre
                class="border-t border-[#30363d] bg-[#0d1117]/50 p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap text-gray-400"
                >{{ result.reasoning }}</pre>
            </details>

            <div
              v-if="showReply"
              class="relative rounded-lg border border-[#30363d] bg-[#161b22] p-3 text-sm leading-relaxed text-gray-200 shadow-sm"
            >
              <pre
                v-if="outputMode === 'raw'"
                class="overflow-x-auto font-mono text-xs whitespace-pre-wrap text-gray-300"
                >{{ result.raw }}</pre>
              <template v-else>
                <pre v-if="result.text" class="font-sans whitespace-pre-wrap text-gray-200">{{
                  result.text
                }}</pre>
                <!-- 还没有收到任何事件时显示已等待的秒数（排队、冷启动），收到首个事件后显示生成中 -->
                <span v-else-if="isRunning" class="animate-pulse text-gray-500" role="status">
                  {{
                    received
                      ? t('playground.generating')
                      : tf('playground.waitingResponse', { seconds: waitSeconds })
                  }}
                </span>
                <p v-if="emptyNotice" class="text-xs text-yellow-300" role="status">
                  {{ emptyNotice }}
                </p>
                <p
                  v-if="result.stopped"
                  class="text-xs text-gray-500"
                  :class="{ 'mt-2': result.text !== '' }"
                >
                  {{ t('playground.stopped') }}
                </p>
              </template>
            </div>

            <!-- 错误卡片：已收到的正文保留在上面；流已建立后失败时不显示 HTTP 状态码（建流时为 200） -->
            <div
              v-if="result.error"
              class="rounded-lg border border-red-500/40 bg-red-950/30 p-3 text-sm shadow-sm"
              role="alert"
            >
              <div class="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-red-300">
                <UiIcon name="info" :size="14" />
                <span class="font-bold">{{ t('playground.errorTitle') }}</span>
                <span
                  v-if="result.error.inStream || result.error.status > 0"
                  class="rounded bg-red-900/40 px-1.5 font-mono"
                >
                  {{
                    result.error.inStream
                      ? t('playground.errorInStream')
                      : `HTTP ${result.error.status}`
                  }}
                </span>
                <span v-if="result.error.code" class="font-mono text-red-300/80">
                  {{ t('playground.errorCode') }} {{ result.error.code }}
                </span>
              </div>
              <pre class="mt-2 font-mono text-xs break-words whitespace-pre-wrap text-red-200">{{
                result.error.message
              }}</pre>
              <div class="mt-3 flex items-center gap-3 text-xs">
                <span v-if="result.error.retryAfter !== undefined" class="text-red-300/80">
                  {{ tf('playground.retryAfter', { seconds: result.error.retryAfter }) }}
                </span>
                <button
                  class="ml-auto flex items-center gap-1 rounded border border-red-500/40 px-2 py-1 text-red-200 transition hover:bg-red-900/40 disabled:cursor-not-allowed disabled:opacity-50"
                  type="button"
                  :disabled="isRunning"
                  @click="retry"
                >
                  <UiIcon name="refresh" :size="12" />
                  {{ t('playground.retry') }}
                </button>
              </div>
            </div>

            <details
              v-if="result.tools"
              class="overflow-hidden rounded-lg border border-[#30363d] bg-[#0d1117]"
            >
              <summary
                class="cursor-pointer bg-[#161b22] px-3 py-2 text-xs font-medium text-gray-400 hover:bg-[#1c2128]"
              >
                {{ t('playground.toolOutput') }}
              </summary>
              <pre
                class="border-t border-[#30363d] p-3 font-mono text-xs whitespace-pre-wrap text-gray-400"
                >{{ result.tools }}</pre>
            </details>

            <div v-if="result.media.length" class="space-y-3">
              <template v-for="(media, index) in result.media" :key="`${media.url}:${index}`">
                <img
                  v-if="media.mime.startsWith('image/')"
                  :src="media.url"
                  alt=""
                  class="max-h-[560px] max-w-full rounded-md border border-[#30363d] object-contain"
                />
                <audio
                  v-else-if="media.mime.startsWith('audio/')"
                  :src="media.url"
                  class="w-full"
                  controls
                ></audio>
                <video
                  v-else-if="media.mime.startsWith('video/')"
                  :src="media.url"
                  class="max-h-[560px] w-full rounded-md border border-[#30363d]"
                  controls
                ></video>
              </template>
            </div>

            <div class="flex items-center justify-between gap-3 text-xs text-gray-600">
              <span>
                {{ submittedModel }}
                <template v-if="result.status"> · HTTP {{ result.status }}</template>
                <template v-if="result.durationMs"> · {{ result.durationMs }} ms</template>
              </span>
              <div class="flex gap-2">
                <button
                  class="rounded px-2 py-1 text-gray-500 hover:bg-[#30363d] hover:text-white"
                  type="button"
                  @click="outputMode = outputMode === 'output' ? 'raw' : 'output'"
                >
                  {{ outputMode === 'output' ? t('playground.raw') : t('playground.output') }}
                </button>
                <button
                  class="rounded px-2 py-1 text-gray-500 hover:bg-[#30363d] hover:text-white"
                  type="button"
                  :disabled="visibleOutput === ''"
                  @click="copyOutput"
                >
                  {{ copied ? t('common.copied') : t('common.copy') }}
                </button>
              </div>
            </div>
          </div>
        </div>
      </div>

      <div class="border-t border-[#30363d] bg-[#161b22] p-4">
        <div class="mx-auto flex max-w-3xl gap-2">
          <textarea
            v-model="form.prompt"
            class="h-[42px] max-h-[200px] flex-1 resize-none rounded border border-[#30363d] bg-[#0d1117] p-2 text-sm text-white focus:border-blue-500 focus:outline-none"
            :placeholder="t('playground.placeholder')"
            @keydown.ctrl.enter.prevent="execute"
          ></textarea>
          <button
            v-if="isRunning"
            class="flex items-center gap-2 rounded bg-red-600 px-4 py-2 font-medium text-white transition hover:bg-red-500"
            type="button"
            @click="stop"
          >
            <UiIcon name="stopAnim" :size="16" />
            {{ t('playground.stop') }}
          </button>
          <button
            v-else
            class="rounded bg-blue-600 px-4 py-2 font-medium text-white transition hover:bg-blue-500 disabled:cursor-not-allowed disabled:opacity-50"
            type="button"
            :disabled="form.model === '' || form.prompt.trim() === ''"
            @click="execute"
          >
            {{ t('playground.send') }}
          </button>
        </div>
      </div>
    </div>

    <aside
      class="flex max-h-[45%] w-full shrink-0 flex-col space-y-5 overflow-auto border-t border-[#30363d] bg-[#161b22] p-4 md:max-h-none md:w-64 md:border-t-0 md:border-l"
    >
      <div>
        <label class="mb-2 block text-xs font-bold text-gray-500 uppercase">{{
          t('playground.mode')
        }}</label>
        <UiSelect
          v-model="form.mode"
          class="w-full appearance-none rounded border border-[#30363d] bg-[#0d1117] px-2 py-1 text-xs text-white focus:border-blue-500 focus:outline-none"
        >
          <option v-for="mode in modes" :key="mode.id" :value="mode.id">
            {{ modeLabel(mode) }}
          </option>
        </UiSelect>
      </div>

      <div>
        <label class="mb-2 block text-xs font-bold text-gray-500 uppercase">{{
          t('playground.model')
        }}</label>
        <UiSelect
          v-model="form.model"
          class="w-full appearance-none rounded border border-[#30363d] bg-[#0d1117] px-2 py-1 text-xs text-white focus:border-blue-500 focus:outline-none"
        >
          <option v-for="model in availableModels" :key="model.id" :value="model.id">
            {{ model.name }}
          </option>
        </UiSelect>
      </div>

      <template v-if="form.mode === 'text'">
        <div>
          <label class="mb-2 block text-xs font-bold text-gray-500 uppercase">{{
            t('playground.protocol')
          }}</label>
          <UiSelect
            v-model="form.protocol"
            class="w-full appearance-none rounded border border-[#30363d] bg-[#0d1117] px-2 py-1 text-xs text-white focus:border-blue-500 focus:outline-none"
          >
            <option v-for="protocol in protocols" :key="protocol.id" :value="protocol.id">
              {{ protocol.label }}
            </option>
          </UiSelect>
        </div>

        <div>
          <label class="mb-2 block text-xs font-bold text-gray-500 uppercase">{{
            t('playground.reasoning')
          }}</label>
          <UiSelect
            v-model="form.reasoning"
            class="w-full appearance-none rounded border border-[#30363d] bg-[#0d1117] px-2 py-1 text-xs text-white focus:border-blue-500 focus:outline-none disabled:opacity-50"
            :disabled="!supportsThinking"
          >
            <option value="">{{ t('playground.reasoningOff') }}</option>
            <option value="low">{{ t('playground.reasoningLow') }}</option>
            <option value="medium">{{ t('playground.reasoningMedium') }}</option>
            <option value="high">{{ t('playground.reasoningHigh') }}</option>
          </UiSelect>
        </div>

        <div>
          <label class="mb-2 block text-xs font-bold text-gray-500 uppercase">{{
            t('playground.tool')
          }}</label>
          <UiSelect
            v-model="form.tool"
            class="w-full appearance-none rounded border border-[#30363d] bg-[#0d1117] px-2 py-1 text-xs text-white focus:border-blue-500 focus:outline-none"
          >
            <option value="">{{ t('playground.toolOff') }}</option>
            <option v-for="tool in availableTools" :key="tool.id" :value="tool.id">
              {{ tool.label }}
            </option>
          </UiSelect>
        </div>
      </template>

      <div v-else-if="form.mode === 'image'" class="space-y-4">
        <label class="block">
          <span class="mb-2 block text-xs font-bold text-gray-500 uppercase">{{
            t('playground.imageSize')
          }}</span>
          <UiSelect
            v-model="form.imageSize"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-2 py-1 text-xs text-white"
          >
            <option value="auto">Auto</option>
            <option value="1024x1024">1024 × 1024</option>
            <option value="1536x1024">1536 × 1024</option>
            <option value="1024x1536">1024 × 1536</option>
          </UiSelect>
        </label>
        <label class="block">
          <span class="mb-2 block text-xs font-bold text-gray-500 uppercase">{{
            t('playground.imageQuality')
          }}</span>
          <UiSelect
            v-model="form.imageQuality"
            class="w-full rounded border border-[#30363d] bg-[#0d1117] px-2 py-1 text-xs text-white"
          >
            <option value="auto">Auto</option>
            <option value="low">1K</option>
            <option value="medium">2K</option>
            <option value="high">4K</option>
          </UiSelect>
        </label>
      </div>

      <div v-else-if="form.mode === 'speech'">
        <label class="mb-2 block text-xs font-bold text-gray-500 uppercase">{{
          t('playground.voice')
        }}</label>
        <UiSelect
          v-model="form.voice"
          class="w-full rounded border border-[#30363d] bg-[#0d1117] px-2 py-1 text-xs text-white"
        >
          <option v-for="voice in voices" :key="voice" :value="voice">{{ voice }}</option>
        </UiSelect>
      </div>

      <div v-if="form.mode === 'text' || form.mode === 'speech'">
        <label class="mb-2 block text-xs font-bold text-gray-500 uppercase">{{
          t('playground.system')
        }}</label>
        <textarea
          v-model="form.system"
          class="h-24 w-full resize-none rounded border border-[#30363d] bg-[#0d1117] px-2 py-1 text-xs text-white focus:border-blue-500 focus:outline-none"
          :placeholder="t('playground.systemPlaceholder')"
        ></textarea>
      </div>

      <div>
        <label class="flex items-center justify-between gap-2">
          <span class="text-xs font-bold text-gray-500 uppercase">{{ t('playground.ultra') }}</span>
          <input v-model="form.ultra" class="h-4 w-4 accent-blue-600" type="checkbox" />
        </label>
        <p class="mt-1 text-[11px] leading-4 text-gray-500">
          {{
            form.ultra && ultraModelIDs.size === 0
              ? t('playground.ultraNoModels')
              : t('playground.ultraHelp')
          }}
        </p>
      </div>

      <div>
        <label class="mb-2 block text-xs font-bold text-gray-500 uppercase">API</label>
        <code class="block break-all text-xs text-blue-400">{{ endpoint }}</code>
      </div>

      <div>
        <label class="mb-2 block text-xs font-bold text-gray-500 uppercase">API Key</label>
        <input
          v-model="form.apiKey"
          class="w-full rounded border border-[#30363d] bg-[#0d1117] px-2 py-1 text-xs text-white"
          type="password"
        />
      </div>

      <label v-if="form.mode === 'text'" class="flex items-center justify-between">
        <span class="text-xs font-bold text-gray-500 uppercase">{{ t('playground.stream') }}</span>
        <input v-model="form.stream" class="h-4 w-4 accent-blue-600" type="checkbox" />
      </label>

      <div class="border-t border-[#30363d] pt-4">
        <button
          class="w-full rounded border border-[#30363d] py-2 text-xs text-red-400 transition hover:bg-red-900/20"
          type="button"
          @click="clearConversation"
        >
          {{ t('playground.clear') }}
        </button>
      </div>
    </aside>
  </section>
</template>
