import type { Account } from '@/types'

export type BulkAction = 'enable' | 'disable' | 'verify' | 'delete'

// BulkFailure 为一个账户的否定结论或出错；orgDisabled 为批量验证时账号所属组织停用了 AI Studio（不是登录失效，单独计数）
export interface BulkFailure {
  id: string
  label: string
  message: string
  orgDisabled?: boolean
}

// BulkTaskResult 为单个账户操作的结论：invalid 为操作做完了、但结论是否定的（批量验证的“登录失效”），message 为原因，
// orgDisabled 为否定的原因是账号所属组织停用了 AI Studio；不返回（undefined）表示成功。操作本身出错仍以抛出错误表示（计为出错）
export interface BulkTaskResult {
  invalid: true
  message: string
  orgDisabled?: boolean
}

// BulkState 描述一次批量账户操作的进度：succeeded 为成功（验证时为“有效”），invalid 为结论否定（验证时为“登录失效”），
// failures 为操作出错
export interface BulkState {
  action: BulkAction | ''
  running: boolean
  cancelled: boolean
  total: number
  done: number
  succeeded: number
  invalid: BulkFailure[]
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
    invalid: [],
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
  task: (account: Account) => Promise<BulkTaskResult | void>,
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
        const result = await task(account)
        if (result?.invalid) {
          state.invalid = [
            ...state.invalid,
            {
              id: account.id,
              label: account.label,
              message: result.message,
              ...(result.orgDisabled ? { orgDisabled: true } : {}),
            },
          ]
        } else {
          state.succeeded += 1
        }
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

// BulkChunkResult 为一批账户的处理结果：succeeded 为成功数，failures 只列出失败的账户
export interface BulkChunkResult {
  succeeded: number
  failures: BulkFailure[]
}

// BulkTarget 为分批操作需要的账户字段：账户列表与 403 无权限账户列表的条目都满足
export type BulkTarget = Pick<Account, 'id' | 'label'>

// runChunkedBulk 把账户按 chunkSize 分批，依次交给 task 一次处理一批（例如一次请求启用 500 个账户）；
// cancelled 置位后不再提交下一批，已提交的一批会等待完成；task 抛出错误时这一批全部计为失败
export async function runChunkedBulk<T extends BulkTarget>(
  state: BulkState,
  action: BulkAction,
  accounts: readonly T[],
  chunkSize: number,
  task: (chunk: readonly T[]) => Promise<BulkChunkResult>,
): Promise<void> {
  const queue = [...accounts]
  Object.assign(state, emptyBulkState(), { action, running: true, total: queue.length })
  const size = Math.max(1, Math.floor(chunkSize))
  try {
    for (let start = 0; start < queue.length && !state.cancelled; start += size) {
      const chunk = queue.slice(start, start + size)
      const first = chunk[0]
      if (first === undefined) break
      state.active = [chunk.length > 1 ? `${first.label} +${chunk.length - 1}` : first.label]
      try {
        const result = await task(chunk)
        state.succeeded += result.succeeded
        state.failures = [...state.failures, ...result.failures]
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error)
        state.failures = [
          ...state.failures,
          ...chunk.map((account) => ({ id: account.id, label: account.label, message })),
        ]
      } finally {
        state.active = []
        state.done += chunk.length
      }
    }
  } finally {
    state.running = false
  }
}
