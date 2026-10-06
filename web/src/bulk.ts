import type { Account } from '@/types'

export type BulkAction = 'enable' | 'disable' | 'verify' | 'delete'

export interface BulkFailure {
  id: string
  label: string
  message: string
}

// BulkState 描述一次批量账户操作的进度
export interface BulkState {
  action: BulkAction | ''
  running: boolean
  cancelled: boolean
  total: number
  done: number
  succeeded: number
  failures: BulkFailure[]
  active: string[]
}

// emptyBulkState 返回空闲状态
export function emptyBulkState(): BulkState {
  return {
    action: '',
    running: false,
    cancelled: false,
    total: 0,
    done: 0,
    succeeded: 0,
    failures: [],
    active: [],
  }
}

// runBulk 以固定并发依次处理账户；cancelled 置位后不再领取新账户，已开始的操作会等待完成
export async function runBulk(
  state: BulkState,
  action: BulkAction,
  accounts: readonly Account[],
  concurrency: number,
  task: (account: Account) => Promise<void>,
): Promise<void> {
  const queue = [...accounts]
  Object.assign(state, emptyBulkState(), { action, running: true, total: queue.length })
  let cursor = 0

  const worker = async (): Promise<void> => {
    while (!state.cancelled) {
      const account = queue[cursor]
      cursor += 1
      if (account === undefined) return
      state.active = [...state.active, account.label]
      try {
        await task(account)
        state.succeeded += 1
      } catch (error) {
        state.failures = [
          ...state.failures,
          {
            id: account.id,
            label: account.label,
            message: error instanceof Error ? error.message : String(error),
          },
        ]
      } finally {
        state.active = state.active.filter((label) => label !== account.label)
        state.done += 1
      }
    }
  }

  const size = Math.max(1, Math.min(Math.floor(concurrency), queue.length))
  try {
    await Promise.all(Array.from({ length: size }, () => worker()))
  } finally {
    state.running = false
  }
}
