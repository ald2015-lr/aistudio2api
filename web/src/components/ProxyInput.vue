<script setup lang="ts">
// ProxyInput 代理输入框：带账号密码且不在编辑时显示掩码按钮，点击后切换成明文输入框；始终不用密码框，避免浏览器密码管理器介入；可测试出口 IP
import { computed, nextTick, ref, watch } from 'vue'
import { api } from '@/api'
import { useI18n, type TranslationKey } from '@/i18n'
import { maskProxy, proxyHasAuth, proxyParts } from '@/proxy'
import { slotLabel } from '@/proxyPool'
import type { ProxyTestProbe, ProxyTestResult } from '@/types'

const model = defineModel<string>({ required: true })
const props = withDefaults(
  defineProps<{
    inputClass: string // 沿用调用处现有样式（设置页的长 class、账户页的 "input w-full"）
    help: string // 已翻译的说明文字
    // 账户对话框为 true：留空时测试新账户登录实际会用的出口（IP 池开启时为此刻会分到的池出口，否则为服务配置的默认代理）
    allowEmptyTest: boolean
    label?: string // 输入框的无障碍名称，即调用处的字段标题
    // testable 为 false 时不显示“测试代理”按钮（IP 池地址由设置页的“抽测出口”测试）
    testable?: boolean
    placeholder?: string // 输入框的占位示例，为空时用通用的代理示例
    // accountId 为编辑中的已有账户：留空时改测这个账户实际使用的出口（IP 池分配或默认代理），结果带端口号或会话号；
    // accountCleared 为这个账户原来有自己的代理（对话框里清空了）：服务端改测清空后 IP 池会给它的出口
    accountId?: string
    accountCleared?: boolean
  }>(),
  { label: '', testable: true, placeholder: '', accountId: '', accountCleared: false },
)
// testing 在测试开始与结束时通知调用处（passed 为两条路径都拿到出口 IP）：设置页据此在测试期间不自动应用
const emit = defineEmits<{ commit: []; testing: [running: boolean, passed: boolean] }>()

const { t, tf, errorText } = useI18n()

type ProxyWarning = NonNullable<ProxyTestResult['warnings']>[number]

const warningKeys: Record<ProxyWarning, TranslationKey> = {
  rotating_ip: 'proxy.warnRotating',
  https_plain_browser: 'proxy.warnHttpsPlain',
  duplicate_ip: 'proxy.warnDuplicateIP',
  pool_partial: 'proxy.warnPoolPartial',
}

const editing = ref(false)
const testing = ref(false)
const result = ref<ProxyTestResult | null>(null)
const testError = ref('')
const inputRef = ref<HTMLInputElement | null>(null)
// testSeq 在输入改动时递增：进行中的测试返回时已经对应旧值，结果丢弃
let testSeq = 0
// testedKey 为当前结果（或进行中的测试）对应代理的协议、host:port 与是否带认证
let testedKey = ''

const masked = computed(() => proxyHasAuth(model.value) && !editing.value)
const testDisabled = computed(
  () => testing.value || (!props.allowEmptyTest && model.value.trim() === ''),
)
// slotKey 为结果里端口号或会话号的说明：编辑已有账户时为它当前的出口，清空了它原来的代理时为清空后的出口（之后的 Worker 可能另分），
// 添加新账户时为此刻会分给登录的出口
const slotKey = computed<TranslationKey>(() => {
  if (props.accountId === '') return 'proxy.testPoolSlot'
  return props.accountCleared ? 'proxy.testAccountClearedSlot' : 'proxy.testAccountSlot'
})
const warnings = computed(() =>
  (result.value?.warnings ?? []).filter((warning) => warning in warningKeys),
)

// proxyKey 返回代理的协议、host:port 与是否带认证；服务端规范化前后不变（例如 host:port:用户名:密码 与
// http://用户名:密码@host:port），用来区分“保存后同步规范值”与真正换了代理
function proxyKey(value: string): string {
  const parts = proxyParts(value)
  if (parts === null) return value.trim()
  let hostPort = parts.hostPort
  // 服务端给带认证、没写端口的 http/https 补默认端口（http://u:p@h 保存后是 http://u:p@h:80），比较前同样补上
  const defaultPort = { http: '80', https: '443' }[parts.scheme]
  const hasPort = hostPort.startsWith('[') ? hostPort.includes(']:') : hostPort.includes(':')
  if (parts.auth && defaultPort !== undefined && !hasPort) hostPort += `:${defaultPort}`
  return `${parts.scheme}://${hostPort}|${parts.auth}`
}

// invalidate 清空测试结果，进行中的测试作废
function invalidate(): void {
  testSeq += 1
  result.value = null
  testError.value = ''
}

function startEdit(): void {
  if (editing.value) return
  editing.value = true
  void nextTick(() => inputRef.value?.focus())
}

// probeOK 判断这条路径是否拿到了出口 IP
function probeOK(probe: ProxyTestProbe): probe is ProxyTestProbe & { ip: string } {
  return probe.ip !== undefined && probe.ip !== ''
}

function probeText(probe: ProxyTestProbe): string {
  if (probeOK(probe)) {
    return tf('proxy.testOK', { ip: probe.ip, ms: probe.elapsed_ms })
  }
  return tf('proxy.testFailed', { error: probe.error || t('common.unknown') })
}

function probeClass(probe: ProxyTestProbe): string {
  return probeOK(probe) ? 'text-green-400' : 'text-red-400'
}

// runTest 经服务端分别测试浏览器路径与后台请求路径的出口 IP；同一时间只跑一次
async function runTest(): Promise<void> {
  if (testDisabled.value) return
  const value = model.value.trim()
  invalidate()
  const seq = testSeq
  testedKey = proxyKey(value)
  testing.value = true
  emit('testing', true, false)
  let passed = false
  try {
    // 已有账户留空时测它实际使用的出口（IP 池开启时为分到的端口或会话）；否则测输入框里的值，留空时服务端测新账户登录会用的出口
    const response =
      value === '' && props.accountId !== ''
        ? await api.testAccountProxy(props.accountId)
        : await api.testProxy(value)
    if (seq === testSeq) {
      result.value = response
      passed = probeOK(response.browser) && probeOK(response.direct)
    }
  } catch (error) {
    if (seq === testSeq) testError.value = errorText(error)
  } finally {
    testing.value = false
    emit('testing', false, passed)
  }
}

// 用户输入会清空结果（见 @input）；外部改值（保存后同步规范值、表单按服务端重置）只在换了代理时清空，
// 避免点“测试代理”触发的失焦保存把正在测试的结果清掉
watch(model, (value) => {
  if (proxyKey(value) !== testedKey) invalidate()
})
</script>

<template>
  <div>
    <div class="flex gap-2">
      <button
        v-if="masked"
        type="button"
        :class="[inputClass, 'flex min-w-0 flex-1 items-center text-left']"
        :title="t('proxy.editHint')"
        :aria-label="t('proxy.editHint')"
        @click="startEdit"
        @focus="startEdit"
      >
        <span class="min-w-0 truncate">{{ maskProxy(model) }}</span>
        <span class="ml-1 shrink-0 rounded bg-[#21262d] px-1 text-[10px] text-gray-400">{{
          t('proxy.authTag')
        }}</span>
      </button>
      <input
        v-else
        ref="inputRef"
        v-model.trim="model"
        type="text"
        :class="[inputClass, 'min-w-0 flex-1']"
        :placeholder="placeholder || t('proxy.placeholder')"
        :aria-label="label || undefined"
        autocomplete="off"
        spellcheck="false"
        autocapitalize="off"
        autocorrect="off"
        data-lpignore="true"
        data-1p-ignore="true"
        data-bwignore="true"
        data-form-type="other"
        @focus="editing = true"
        @blur="editing = false"
        @input="invalidate"
        @change="emit('commit')"
      />
      <!-- mousedown.prevent：点测试时输入框不失焦，不会先触发失焦保存（设置页保存后可能自动重启生效），测的是当前输入的值 -->
      <button
        v-if="testable"
        class="btn h-auto shrink-0"
        type="button"
        :disabled="testDisabled"
        :aria-busy="testing"
        @mousedown.prevent
        @click="runTest"
      >
        {{ testing ? t('proxy.testing') : t('proxy.test') }}
      </button>
    </div>
    <span class="mt-1 block text-xs text-gray-500">{{ help }}</span>
    <span v-if="testable" class="mt-1 block text-xs text-gray-500">{{ t('proxy.testHint') }}</span>
    <div
      v-if="result !== null || testError !== ''"
      class="mt-2 space-y-1 text-xs"
      aria-live="polite"
    >
      <template v-if="result !== null">
        <div class="break-words">
          <span class="mr-2 text-gray-400">{{
            result.relayed ? t('proxy.testBrowserRelay') : t('proxy.testBrowser')
          }}</span>
          <span :class="probeClass(result.browser)">{{ probeText(result.browser) }}</span>
        </div>
        <div class="break-words">
          <span class="mr-2 text-gray-400">{{ t('proxy.testDirect') }}</span>
          <span :class="probeClass(result.direct)">{{ probeText(result.direct) }}</span>
        </div>
        <div v-if="result.slot" class="break-words text-gray-400">
          {{
            tf(slotKey, {
              slot: slotLabel(result.slot, tf),
            })
          }}
        </div>
        <div v-for="warning in warnings" :key="warning" class="text-yellow-400">
          {{ t(warningKeys[warning]) }}
        </div>
      </template>
      <div v-if="testError !== ''" class="break-words text-red-400">{{ testError }}</div>
    </div>
  </div>
</template>
