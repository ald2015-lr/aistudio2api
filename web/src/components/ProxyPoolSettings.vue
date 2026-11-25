<script setup lang="ts">
// ProxyPoolSettings 设置页的 IP 池分组：模式、固定分配开关、地址（带账号密码时掩码显示）、静态 IP 的端口段与容量提示、抽测出口、
// 检查全部端口、实时状态、固定分配的状态与操作（重新平均分配、给端口换 IP、恢复停用的端口）与“改为跟随默认代理”。
// 改动经 commit 通知设置页保存（与失焦保存的字段相同）：模式与地址的组合不合法时设置页不提交，错误显示在对应输入框旁；
// 固定分配开关（v-model:fixed）确认后改表单，由设置页自动保存
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { ApiError, api } from '@/api'
import { trackPendingWrite } from '@/assetRecovery'
import { confirmAction } from '@/confirm'
import { useI18n, type TranslationKey } from '@/i18n'
import { proxyParts } from '@/proxy'
import {
  DEFAULT_VERIFY_CONCURRENCY,
  cloneProxyPool,
  fixedPerPort,
  formatClock,
  proxyPoolCapacity,
  proxyPoolFixedActive,
  proxyPoolPauseReasonKey,
  proxyPoolPortCount,
  proxyPoolProblem,
  proxyPoolProblemKey,
  slotLabel,
} from '@/proxyPool'
import type {
  ProxyPoolConfig,
  ProxyPoolMode,
  ProxyPoolPausedPort,
  ProxyPoolStatus,
  ProxyTestProbe,
  ProxyTestResult,
} from '@/types'
import HelpField from './HelpField.vue'
import HelpText from './HelpText.vue'
import ProxyInput from './ProxyInput.vue'
import UiIcon from './UiIcon.vue'
import UiSelect from './UiSelect.vue'

const model = defineModel<ProxyPoolConfig>({ required: true })
// fixed 为固定分配开关（PROXY_POOL_FIXED）；旧版服务端不返回时为 undefined，不显示开关
const fixed = defineModel<boolean | undefined>('fixed')
const props = defineProps<{
  status: ProxyPoolStatus | null // 状态接口的 proxy_pool：已保存并生效的设置的实时情况
  // 设置页表单里的峰值与启动并发（可能是输入到一半的值），用于估算端口需求
  maxActive: number | string
  ultraMaxActive: number | string
  // proMaxActive 为 Pro 峰值，proCounted 为开启 PRO_POOL 且有 Pro 号池账户（只有这时 Pro 分区才占端口）
  proMaxActive?: number | string
  proCounted?: boolean
  // extraMaxActive 为补充池峰值，extraCounted 为开启补充池（EXTRA_POOL，只有这时补充池才占端口）
  extraMaxActive?: number | string
  extraCounted?: boolean
  warmConcurrency: number | string
  hasDefaultProxy: boolean
  // poolAccounts 为走 IP 池的账户数（账户列表里没有自己代理的账户），开启固定分配前估算每个端口的账户数
  poolAccounts?: number
  // savedFixedMode 为已保存的设置里开启了固定分配的模式（static 为固定分配、dynamic 为固定会话；没有开启或 IP 池关闭时为空）：
  // 切换模式会让固定分配换一种生效方式时先确认
  savedFixedMode?: ProxyPoolMode | ''
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

async function setMode(value: unknown): Promise<void> {
  if (value !== 'off' && value !== 'static' && value !== 'dynamic') return
  const mode: ProxyPoolMode = value
  if (mode === model.value.mode || modeConfirming.value) return
  // 开着固定分配切到另一种模式（动态切静态、关闭切静态等）：固定分配随之按新模式生效（静态为首次分配端口、同端口共用 IP、端口
  // 暂停时整端口暂停），先给出与勾选开关相同的确认；取消时不切模式
  if (fixedModeNeedsAck(mode)) {
    modeConfirming.value = true
    try {
      const confirmed = await confirmAction(
        fixedEnableMessage(mode),
        t('settings.proxyPoolFixedEnableAction'),
      )
      if (!confirmed) return
      fixedAcked.value = mode
    } finally {
      modeConfirming.value = false
    }
  }
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
  if (model.value.mode === 'dynamic') {
    // 开启固定会话时不再“连不通时换会话号”
    return t(
      fixed.value === true
        ? 'settings.proxyPoolURLHelpDynamicFixed'
        : 'settings.proxyPoolURLHelpDynamic',
    )
  }
  return t('settings.proxyPoolURLHelpOff')
})
// fixedForm 为表单里开启了固定分配（静态为固定分配、动态为固定会话）：分组标题与说明改用固定分配的口径
const fixedForm = computed(() => fixed.value === true && model.value.mode !== 'off')
// staticModeLabel 为模式下拉里静态 IP 的选项：开关开着时是每个账户固定一个端口，不再是每个浏览器独占
const staticModeLabel = computed(() =>
  t(fixed.value === true ? 'settings.proxyPoolModeStaticFixed' : 'settings.proxyPoolModeStatic'),
)
const urlPlaceholder = computed(() =>
  model.value.mode === 'dynamic'
    ? t('settings.proxyPoolURLPlaceholderDynamic')
    : t('settings.proxyPoolURLPlaceholderStatic'),
)

// ---------- 端口需求 ----------

const portCount = computed(() => proxyPoolPortCount(model.value))
const capacity = computed(() =>
  proxyPoolCapacity(
    props.maxActive,
    props.ultraMaxActive,
    props.warmConcurrency,
    props.proMaxActive ?? 0,
    props.proCounted ?? false,
    props.extraMaxActive ?? 0,
    props.extraCounted ?? false,
  ),
)
// capacityKey 为端口需求一行的文案：按计入的分区选（没有 Pro、补充池时与原来同一句）
const capacityKey = computed<TranslationKey>(() => {
  if (props.extraCounted) {
    return props.proCounted
      ? 'settings.proxyPoolCapacityProExtra'
      : 'settings.proxyPoolCapacityExtra'
  }
  return props.proCounted ? 'settings.proxyPoolCapacityPro' : 'settings.proxyPoolCapacity'
})
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

// ---------- 固定分配 ----------

// fixedShown 为显示固定分配开关：服务端返回了开关、IP 池没有关闭（关闭时开关保留、不生效）
const fixedShown = computed(() => fixed.value !== undefined && model.value.mode !== 'off')
// fixedStaticForm 为表单里是静态 IP 且开启固定分配：端口需求一行改为每端口账户数，不再提示端口少于浏览器数
const fixedStaticForm = computed(() => fixed.value === true && model.value.mode === 'static')
// fixedLive 为状态接口里生效中的固定分配（已保存的设置）
const fixedLive = computed(() => proxyPoolFixedActive(props.status))
const fixedStaticLive = computed(() => fixedLive.value?.mode === 'static')

// perPortText 把每端口的最少、最多账户数显示为“4”或“4–5”
function perPortText(min: number, max: number): string {
  return min === max ? String(min) : `${min}–${max}`
}

// fixedCapacity 为静态固定分配下的端口与每端口账户数：表单端口段与生效的端口段相同时用状态接口的实际值，否则按账户数估算
const fixedCapacity = computed(() => {
  if (!fixedStaticForm.value || portCount.value <= 0) return null
  const live = fixedLive.value
  const status = props.status
  if (
    live?.mode === 'static' &&
    status?.port_start === model.value.port_start &&
    status?.port_end === model.value.port_end
  ) {
    return {
      ports: live.ports ?? portCount.value,
      accounts: live.accounts,
      perPort: perPortText(live.min_per_port, live.max_per_port),
    }
  }
  const accounts = props.poolAccounts ?? 0
  const spread = fixedPerPort(accounts, portCount.value)
  if (spread === null) return null
  return { ports: portCount.value, accounts, perPort: perPortText(spread.min, spread.max) }
})

// fixedEnableMessage 返回在 mode 下开启固定分配的确认文案：静态按账户数与表单端口数估算每端口约几个账户（再次开启时沿用原分配表、
// 不重新平均，文案里写明），动态为固定会话
function fixedEnableMessage(mode: ProxyPoolMode): string {
  if (mode === 'dynamic') return t('settings.proxyPoolFixedEnableConfirmDynamic')
  const accounts = props.poolAccounts ?? 0
  const spread = fixedPerPort(accounts, portCount.value) ?? { min: 0, max: 0 }
  return tf('settings.proxyPoolFixedEnableConfirm', {
    accounts,
    ports: portCount.value,
    perPort: perPortText(spread.min, spread.max),
  })
}

// fixedAcked 为这次编辑里已经确认过开启固定分配的模式（勾选开关、切换模式时的确认）：同一模式不再重复确认
const fixedAcked = ref<ProxyPoolMode | ''>('')
const modeConfirming = ref(false)

// fixedModeNeedsAck 判断切到 mode 时固定分配会换一种方式生效、需要先确认：开关开着、mode 不是关闭、已保存的设置里不是这种模式的
// 固定分配，且这次编辑里还没确认过
function fixedModeNeedsAck(mode: ProxyPoolMode): boolean {
  return (
    fixed.value === true &&
    mode !== 'off' &&
    (props.savedFixedMode ?? '') !== mode &&
    fixedAcked.value !== mode
  )
}

// onFixedChange 切换固定分配：先确认（开启时说明每端口约几个账户、之后不再自动换 IP），确认后才改表单（设置页随即自动保存）
const fixedConfirming = ref(false)
async function onFixedChange(event: Event): Promise<void> {
  const input = event.target as HTMLInputElement
  const next = input.checked
  // 确认之前勾选框保持原样
  input.checked = fixed.value === true
  if (fixedConfirming.value || next === (fixed.value === true)) return
  const mode = model.value.mode
  const dynamic = mode === 'dynamic'
  let message: string
  if (next) {
    message = fixedEnableMessage(mode)
  } else {
    message = t(
      dynamic
        ? 'settings.proxyPoolFixedDisableConfirmDynamic'
        : 'settings.proxyPoolFixedDisableConfirm',
    )
  }
  fixedConfirming.value = true
  try {
    const confirmed = await confirmAction(
      message,
      t(next ? 'settings.proxyPoolFixedEnableAction' : 'settings.proxyPoolFixedDisableAction'),
    )
    if (confirmed) {
      fixed.value = next
      fixedAcked.value = next ? mode : ''
    }
  } finally {
    fixedConfirming.value = false
  }
}

// droppedPortRanges 返回生效端口段 [liveStart, liveEnd] 里不在新端口段 [start, end] 内的端口（最多两段，按升序）与个数
function droppedPortRanges(
  liveStart: number,
  liveEnd: number,
  start: number,
  end: number,
): { ranges: [number, number][]; count: number } {
  const ranges: [number, number][] = []
  if (end < liveStart || start > liveEnd) {
    ranges.push([liveStart, liveEnd])
  } else {
    if (start > liveStart) ranges.push([liveStart, start - 1])
    if (end < liveEnd) ranges.push([end + 1, liveEnd])
  }
  return { ranges, count: ranges.reduce((sum, [from, to]) => sum + to - from + 1, 0) }
}

// commitPorts 端口段失焦时提交：静态固定分配生效中、新端口段（合法时）丢掉了生效段里的端口时先确认——这些端口上的账户会立即挪到其他
// 端口、换 IP，改回端口段也不会回到原端口；取消时把表单恢复成生效的端口段，不提交
const portsConfirming = ref(false)
async function commitPorts(): Promise<void> {
  const status = props.status
  const live = fixedLive.value
  if (
    portsConfirming.value ||
    !fixedStaticForm.value ||
    live?.mode !== 'static' ||
    status?.port_start === undefined ||
    status.port_end === undefined ||
    problem.value !== ''
  ) {
    if (!portsConfirming.value) emit('commit')
    return
  }
  const liveStart = status.port_start
  const liveEnd = status.port_end
  const dropped = droppedPortRanges(liveStart, liveEnd, model.value.port_start, model.value.port_end)
  if (dropped.count === 0) {
    emit('commit')
    return
  }
  const total = liveEnd - liveStart + 1
  const accounts =
    dropped.count >= total ? live.accounts : Math.round((live.accounts * dropped.count) / total)
  const separator = locale.value.startsWith('zh') ? '、' : ', '
  const ports = dropped.ranges
    .map(([from, to]) => (from === to ? String(from) : `${from}–${to}`))
    .join(separator)
  portsConfirming.value = true
  try {
    const confirmed = await confirmAction(
      tf('settings.proxyPoolFixedShrinkConfirm', { ports, accounts }),
      t('settings.proxyPoolFixedShrinkAction'),
    )
    if (confirmed) {
      emit('commit')
    } else {
      update({ port_start: liveStart, port_end: liveEnd })
    }
  } finally {
    portsConfirming.value = false
  }
}

// pauseReasonText 返回暂停原因的说明；不认识的原因码原样显示
function pauseReasonText(reason: string): string {
  const key = proxyPoolPauseReasonKey(reason)
  return key === null ? reason : t(key)
}

// pausedUntilText 返回暂停端口的恢复时间（冷却）或下次检查时间（被其他进程占用）
function pausedUntilText(item: ProxyPoolPausedPort): string {
  const time = formatClock(item.until, locale.value)
  if (time === '') return ''
  return tf(
    item.reason === 'locked_elsewhere'
      ? 'settings.proxyPoolFixedPausedRecheck'
      : 'settings.proxyPoolFixedPausedUntil',
    { time },
  )
}

const fixedPaused = computed(() => fixedLive.value?.paused ?? [])
const fixedPausedMore = computed(() =>
  Math.max(0, (fixedLive.value?.paused_ports ?? 0) - fixedPaused.value.length),
)
const fixedExcluded = computed(() => fixedLive.value?.excluded ?? [])
const fixedExcludedMore = computed(() =>
  Math.max(0, (fixedLive.value?.excluded_total ?? 0) - fixedExcluded.value.length),
)
// fixedInRange 判断端口在生效的端口段内（“检查全部端口”的失败端口旁显示“换 IP”）
function fixedInRange(port: number): boolean {
  const status = props.status
  return (
    status !== null &&
    status.port_start !== undefined &&
    status.port_end !== undefined &&
    port >= status.port_start &&
    port <= status.port_end
  )
}

const fixedErrorKeys: Record<string, TranslationKey> = {
  proxy_pool_not_fixed: 'settings.proxyPoolFixedNotFixed',
  proxy_pool_no_target: 'settings.proxyPoolFixedNoTarget',
  proxy_pool_port_out_of_range: 'settings.proxyPoolFixedOutOfRange',
}

// fixedErrorText 返回固定分配操作的错误：已知错误码按界面语言显示，其余显示服务端的说明
function fixedErrorText(error: unknown): string {
  if (error instanceof ApiError) {
    const key = fixedErrorKeys[error.code]
    if (key !== undefined) return t(key)
  }
  return errorText(error)
}

// listTargets 把挪到的端口列成一行，超过 10 个时以省略号结尾
function listTargets(ports: number[] | undefined): string {
  const values = (ports ?? []).slice(0, 10).map(String)
  if ((ports?.length ?? 0) > 10) values.push('…')
  return values.join(locale.value.startsWith('zh') ? '、' : ', ')
}

// fixedBusy 为进行中的固定分配操作（rebalance、reassign:端口、restore:端口）；同一时间只做一个
const fixedBusy = ref('')

// rebalanceFixed 重新平均分配：先只计算挪动数，确认后执行（登记为在途写操作）
async function rebalanceFixed(): Promise<void> {
  if (fixedBusy.value !== '') return
  fixedBusy.value = 'rebalance'
  try {
    const preview = await api.rebalanceProxyPool(true)
    if (preview.moved === 0 && preview.pruned === 0) {
      emit(
        'notice',
        tf('settings.proxyPoolFixedRebalanceNone', {
          perPort: perPortText(preview.min_per_port, preview.max_per_port),
        }),
        'success',
      )
      return
    }
    const confirmed = await confirmAction(
      tf('settings.proxyPoolFixedRebalanceConfirm', {
        moved: preview.moved,
        running: preview.running,
        pruned: preview.pruned,
        accounts: preview.accounts,
        ports: preview.ports,
        perPort: perPortText(preview.min_per_port, preview.max_per_port),
      }),
      t('settings.proxyPoolFixedRebalance'),
    )
    if (!confirmed) return
    const result = await trackPendingWrite(api.rebalanceProxyPool(false))
    emit(
      'notice',
      tf('settings.proxyPoolFixedRebalanceDone', {
        moved: result.moved,
        pruned: result.pruned,
        perPort: perPortText(result.min_per_port, result.max_per_port),
      }),
      'success',
    )
  } catch (error) {
    emit('notice', fixedErrorText(error), 'error')
  } finally {
    fixedBusy.value = ''
  }
}

// reassignFixed 给端口换 IP：先只计算挪动数与目标端口，确认后执行
async function reassignFixed(port: number): Promise<void> {
  if (fixedBusy.value !== '') return
  fixedBusy.value = `reassign:${port}`
  try {
    const preview = await api.reassignProxyPoolPort(port, true)
    // closing 为此刻用这个端口、执行后登记后台关闭的浏览器（含固定端口已在别处的过渡浏览器），与执行结果的 restarted 同一口径；
    // 旧版服务没有这个字段时不写这一句
    const closing = preview.closing ?? 0
    const message =
      (preview.moved === 0
        ? tf('settings.proxyPoolFixedReassignEmpty', { port })
        : tf('settings.proxyPoolFixedReassignConfirm', {
            port,
            moved: preview.moved,
            targets: listTargets(preview.targets),
          })) + (closing > 0 ? tf('settings.proxyPoolFixedReassignClosing', { closing }) : '')
    const confirmed = await confirmAction(message, t('settings.proxyPoolFixedReassign'))
    if (!confirmed) return
    const result = await trackPendingWrite(api.reassignProxyPoolPort(port, false))
    emit(
      'notice',
      tf('settings.proxyPoolFixedReassignDone', {
        port,
        moved: result.moved,
        restarted: result.restarted,
      }),
      'success',
    )
  } catch (error) {
    emit('notice', fixedErrorText(error), 'error')
  } finally {
    fixedBusy.value = ''
  }
}

// restoreFixed 恢复“换 IP”后停用的端口（不挪任何账户）
async function restoreFixed(port: number): Promise<void> {
  if (fixedBusy.value !== '') return
  fixedBusy.value = `restore:${port}`
  try {
    const result = await trackPendingWrite(api.restoreProxyPoolPort(port))
    emit(
      'notice',
      tf(
        result.restored
          ? 'settings.proxyPoolFixedRestoreDone'
          : 'settings.proxyPoolFixedRestoreNoop',
        { port },
      ),
      'success',
    )
  } catch (error) {
    emit('notice', fixedErrorText(error), 'error')
  } finally {
    fixedBusy.value = ''
  }
}

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
    // 改为跟随默认代理会改写账户配置：登记为在途写操作，切到用量页后页面文件载入失败时先等它结束再自动刷新
    const result = await trackPendingWrite(api.followDefaultProxy())
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
    <legend class="sr-only">
      {{ fixedForm ? t('settings.proxyPoolTitleFixed') : t('settings.proxyPoolTitle') }}
    </legend>
    <span class="mb-1 block text-sm font-medium text-gray-300">{{
      fixedForm ? t('settings.proxyPoolTitleFixed') : t('settings.proxyPoolTitle')
    }}</span>
    <HelpText
      class="mb-4 text-xs leading-5 text-gray-500"
      :text="fixedForm ? t('settings.proxyPoolHelpFixed') : t('settings.proxyPoolHelp')"
    />

    <label class="block">
      <span class="mb-1 block text-sm font-medium text-gray-400">{{
        t('settings.proxyPoolMode')
      }}</span>
      <UiSelect
        :model-value="model.mode"
        :class="INPUT_CLASS"
        :disabled="modeConfirming"
        @update:model-value="setMode"
      >
        <option value="off">{{ t('settings.proxyPoolModeOff') }}</option>
        <option value="static">{{ staticModeLabel }}</option>
        <option value="dynamic">{{ t('settings.proxyPoolModeDynamic') }}</option>
      </UiSelect>
    </label>

    <HelpField v-if="fixedShown" class="mt-4">
      <label class="flex items-start gap-3">
        <input
          :checked="fixed === true"
          class="mt-0.5 h-4 w-4 shrink-0 accent-blue-500"
          type="checkbox"
          :disabled="fixedConfirming"
          data-testid="proxy-pool-fixed-toggle"
          @change="onFixedChange"
        />
        <span class="min-w-0">
          <span class="block text-sm font-medium text-gray-300">{{
            model.mode === 'dynamic'
              ? t('settings.proxyPoolFixedDynamic')
              : t('settings.proxyPoolFixed')
          }}</span>
          <HelpText
            class="mt-1 block text-xs text-gray-500"
            :text="
              model.mode === 'dynamic'
                ? t('settings.proxyPoolFixedHelpDynamic')
                : t('settings.proxyPoolFixedHelpStatic')
            "
          />
        </span>
      </label>
    </HelpField>

    <div class="mt-4">
      <span class="mb-1 block text-sm font-medium text-gray-400">{{
        t('settings.proxyPoolURL')
      }}</span>
      <ProxyInput
        v-model="url"
        :input-class="INPUT_CLASS"
        :help="urlHelp"
        collapsible-help
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
            data-testid="proxy-pool-port-start"
            @change="commitPorts"
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
            data-testid="proxy-pool-port-end"
            @change="commitPorts"
          />
        </label>
      </div>
      <HelpText class="mt-2 block text-xs text-gray-500" :text="t('settings.proxyPoolPortsHelp')" />
      <span
        v-if="fixedCapacity !== null"
        class="mt-2 block text-xs text-gray-400"
        data-testid="proxy-pool-fixed-capacity"
      >
        {{ tf('settings.proxyPoolFixedCapacity', fixedCapacity) }}
      </span>
      <template v-else-if="capacityShown && capacity !== null">
        <span class="mt-2 block text-xs text-gray-400">
          {{
            tf(capacityKey, {
              ports: portCount,
              need: capacity.total,
              normal: capacity.normal,
              normalDrain: capacity.normalDrain,
              ultra: capacity.ultra,
              ultraDrain: capacity.ultraDrain,
              pro: capacity.pro,
              proDrain: capacity.proDrain,
              extra: capacity.extra,
              extraDrain: capacity.extraDrain,
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

    <HelpText
      v-if="model.mode !== 'off' && hasDefaultProxy"
      class="mt-3 text-xs leading-5 text-gray-500"
      :text="
        fixedStaticForm
          ? t('settings.proxyPoolFixedDefaultProxyHint')
          : t('settings.proxyPoolDefaultProxyHint')
      "
    />
    <HelpText
      class="mt-3 text-xs leading-5 text-gray-500"
      :text="t('settings.proxyPoolLiveHint')"
    />

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
      <HelpText
        class="mt-2 block text-xs text-gray-500"
        :text="t('settings.proxyPoolSampleHint')"
      />

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
                <button
                  v-if="fixedStaticLive && fixedInRange(failure.port)"
                  v-tooltip="t('settings.proxyPoolFixedReassignHelp')"
                  class="btn btn-sm ml-2"
                  type="button"
                  :disabled="fixedBusy !== ''"
                  :aria-busy="fixedBusy === `reassign:${failure.port}`"
                  @click="reassignFixed(failure.port)"
                >
                  {{ t('settings.proxyPoolFixedReassign') }}
                </button>
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
      <div v-if="status.mode === 'static' && !fixedStaticLive" class="text-gray-500">
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
      <div v-if="status.external > 0 && !fixedStaticLive" class="text-yellow-400">
        {{ tf('settings.proxyPoolExternal', { count: status.external }) }}
      </div>
      <div v-if="status.orphaned > 0" class="text-yellow-400">
        {{ tf('settings.proxyPoolOrphaned', { count: status.orphaned }) }}
      </div>
      <div v-if="relayLow" class="text-yellow-400">
        {{ tf('settings.proxyPoolRelayLimit', { limit: status.relay_limit ?? 0 }) }}
      </div>

      <!-- 固定分配（已保存并生效的设置）：统计、暂停与停用的端口、重新平均分配 -->
      <div
        v-if="fixedLive !== null && fixedLive.mode === 'static'"
        class="mt-3 space-y-1 border-t border-[#30363d] pt-3"
        data-testid="proxy-pool-fixed-status"
      >
        <div class="text-gray-300">
          {{
            tf('settings.proxyPoolFixedStatus', {
              ports: fixedLive.ports ?? 0,
              accounts: fixedLive.accounts,
              perPort: perPortText(fixedLive.min_per_port, fixedLive.max_per_port),
            })
          }}<template v-if="(fixedLive.empty ?? 0) > 0">
            · {{ tf('settings.proxyPoolFixedEmpty', { count: fixedLive.empty ?? 0 }) }}</template
          ><template v-if="(fixedLive.reserved ?? 0) > 0">
            · {{ tf('settings.proxyPoolFixedReserved', { count: fixedLive.reserved ?? 0 }) }}</template
          >
        </div>
        <div v-if="(fixedLive.transitional ?? 0) > 0" class="text-blue-300">
          {{ tf('settings.proxyPoolFixedTransitional', { count: fixedLive.transitional ?? 0 }) }}
        </div>
        <div v-if="(fixedLive.unassigned ?? 0) > 0" class="text-yellow-400">
          {{ tf('settings.proxyPoolFixedUnassigned', { count: fixedLive.unassigned ?? 0 }) }}
        </div>
        <div v-if="(fixedLive.stray ?? 0) > 0" class="text-yellow-400">
          {{ tf('settings.proxyPoolFixedStray', { count: fixedLive.stray ?? 0 }) }}
        </div>
        <div v-if="(fixedLive.external_ports ?? 0) > 0" class="text-yellow-400">
          {{ tf('settings.proxyPoolFixedExternal', { count: fixedLive.external_ports ?? 0 }) }}
        </div>

        <div
          v-if="fixedLive.paused_ports > 0"
          class="mt-2 rounded border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-amber-200"
          role="note"
          data-testid="proxy-pool-fixed-paused"
        >
          <div class="leading-5">
            {{
              tf('settings.proxyPoolFixedPaused', {
                ports: fixedLive.paused_ports,
                accounts: fixedLive.paused_accounts,
              })
            }}
          </div>
          <ul class="mt-1 max-h-48 space-y-1 overflow-auto">
            <li
              v-for="item in fixedPaused"
              :key="item.port"
              class="flex flex-wrap items-center gap-x-2 gap-y-1"
            >
              <span class="font-mono text-gray-100">{{ item.port }}</span>
              <span class="min-w-0 break-words">{{ pauseReasonText(item.reason) }}</span>
              <span v-if="pausedUntilText(item) !== ''" class="text-amber-300/80">{{
                pausedUntilText(item)
              }}</span>
              <span class="text-amber-300/80">{{
                tf('settings.proxyPoolFixedPausedAccounts', { count: item.accounts })
              }}</span>
              <button
                v-tooltip="t('settings.proxyPoolFixedReassignHelp')"
                class="btn btn-sm"
                type="button"
                :disabled="fixedBusy !== ''"
                :aria-busy="fixedBusy === `reassign:${item.port}`"
                @click="reassignFixed(item.port)"
              >
                {{ t('settings.proxyPoolFixedReassign') }}
              </button>
            </li>
          </ul>
          <div v-if="fixedPausedMore > 0" class="mt-1 text-amber-300/80">
            {{ tf('settings.proxyPoolFixedListMore', { count: fixedPausedMore }) }}
          </div>
        </div>

        <div v-if="fixedExcluded.length > 0" class="mt-2" data-testid="proxy-pool-fixed-excluded">
          <div>
            {{
              tf('settings.proxyPoolFixedExcluded', {
                count: fixedLive.excluded_total ?? fixedExcluded.length,
              })
            }}
          </div>
          <ul class="mt-1 flex max-h-32 flex-wrap gap-2 overflow-auto">
            <li
              v-for="port in fixedExcluded"
              :key="port"
              class="flex items-center gap-1 rounded border border-[#30363d] px-2 py-0.5"
            >
              <span class="font-mono text-gray-300">{{ port }}</span>
              <button
                class="btn btn-sm"
                type="button"
                :disabled="fixedBusy !== ''"
                :aria-busy="fixedBusy === `restore:${port}`"
                @click="restoreFixed(port)"
              >
                {{ t('settings.proxyPoolFixedRestore') }}
              </button>
            </li>
          </ul>
          <div v-if="fixedExcludedMore > 0" class="mt-1 text-gray-500">
            {{ tf('settings.proxyPoolFixedListMore', { count: fixedExcludedMore }) }}
          </div>
        </div>

        <div class="mt-2 flex flex-wrap items-center gap-2">
          <button
            class="btn btn-sm"
            type="button"
            :disabled="fixedBusy !== ''"
            :aria-busy="fixedBusy === 'rebalance'"
            data-testid="proxy-pool-fixed-rebalance"
            @click="rebalanceFixed"
          >
            <UiIcon v-if="fixedBusy === 'rebalance'" name="spinner" :size="12" />
            {{ t('settings.proxyPoolFixedRebalance') }}
          </button>
        </div>
        <HelpText
          class="block text-xs text-gray-500"
          :text="t('settings.proxyPoolFixedRebalanceHint')"
        />
      </div>
      <div
        v-else-if="fixedLive !== null && fixedLive.mode === 'dynamic'"
        class="text-gray-300"
        data-testid="proxy-pool-fixed-status"
      >
        {{ tf('settings.proxyPoolFixedDynamicStatus', { accounts: fixedLive.accounts }) }}
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
