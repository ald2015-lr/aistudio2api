import { onScopeDispose, ref, watch, type Ref } from 'vue'

// 服务端时钟（移植自上游 c77dd89 的 elapsed.ts）：状态接口的 time 为生成这份状态的服务端时刻，管理页据此估计服务端时钟与本机时钟之差，
// 把服务端给出的时刻（启动、停止步骤的 transition_since 等）换算成本机时刻再算已用时间，不受浏览器时钟偏差影响。
// 约束：差值取最近 OFFSET_SAMPLES 份状态中最大的一个：状态在途中耽搁只会让差值偏小；只取最近几份，本机时钟被校准后差值随之更新

// OFFSET_SAMPLES 为参与估计的最近状态份数（状态每 3 秒轮询一次，另有推送，约覆盖最近半分钟）
const OFFSET_SAMPLES = 10
const offsets: number[] = []

// syncServerClock 以一份状态的服务端时刻 time 校准时钟差；旧版服务端没有 time 时不变（没有样本时按本机时钟）
export function syncServerClock(time: string | undefined): void {
  if (time === undefined) return
  const server = Date.parse(time)
  if (Number.isNaN(server)) return
  offsets.push(server - Date.now())
  if (offsets.length > OFFSET_SAMPLES) offsets.splice(0, offsets.length - OFFSET_SAMPLES)
}

// serverTime 把服务端时刻换算成本机时钟的毫秒时刻
export function serverTime(time: string): number {
  return Date.parse(time) - (offsets.length === 0 ? 0 : Math.max(...offsets))
}

// elapsedSecondsSince 返回从服务端时刻 since 到本机时刻 now 的已用整秒数，since 晚于 now（时钟差估计偏小）时为 0
export function elapsedSecondsSince(since: string, now: number): number {
  const start = serverTime(since)
  return Number.isNaN(start) ? 0 : Math.floor(Math.max(0, now - start) / 1000)
}

// useSecondClock 返回 active 为真期间每秒刷新的本机毫秒时刻，active 变化时立即刷新一次
export function useSecondClock(active: () => boolean): Readonly<Ref<number>> {
  const now = ref(Date.now())
  let timer: number | undefined
  watch(
    active,
    (running) => {
      window.clearInterval(timer)
      now.value = Date.now()
      timer = running ? window.setInterval(() => (now.value = Date.now()), 1000) : undefined
    },
    { immediate: true },
  )
  onScopeDispose(() => window.clearInterval(timer))
  return now
}
