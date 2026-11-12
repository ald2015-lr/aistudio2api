import { onBeforeUnmount, ref } from 'vue'

// 服务升级后，已打开的旧页面再去载入旧版的懒加载块（用量页等）会失败：服务端对 /assets/ 下已不存在的文件返回 404。
// 这里在确认服务端的入口已经换成新版本后自动刷新一次；30 秒内最多一次，时间记在 sessionStorage（只对当前标签页有效），
// 读写不了时不刷新，避免刷新后仍失败而反复刷新。有未保存的输入时不刷新，改为提示手动刷新；有在途的写操作时先等它结束，
// 几秒内没结束同样不刷新

// RELOAD_STORAGE_KEY 是上次自动刷新的时间（毫秒）
const RELOAD_STORAGE_KEY = 'aistudio2api_asset_reload_at'
// RELOAD_INTERVAL 是两次自动刷新的最短间隔
const RELOAD_INTERVAL = 30_000

// unsavedChecks 是各组件登记的未保存输入判定
const unsavedChecks = new Set<() => boolean>()

// pendingWrites 是在途的写操作（保存服务配置、应用配置、账户批量操作）。它们常在发起的组件卸载后仍在进行：切到用量页时
// 设置页卸载前会补发挂起的自动保存、失焦字段会先发出保存，账户面板的批量任务在面板卸载后照常跑完。这些登记不随组件卸载移除，
// 写操作结束（成功或失败）时才移除
const pendingWrites = new Set<Promise<unknown>>()
// PENDING_WRITE_WAIT 是自动刷新前等在途写操作结束的最长时间（毫秒）；到时仍有写操作没结束时不刷新，改为提示手动刷新
const PENDING_WRITE_WAIT = 5_000

// assetReloadBlocked 在因有未保存的输入而没有自动刷新时加一，App.vue 据此显示需要手动刷新的提示
export const assetReloadBlocked = ref(0)

// recovering 为正在检查与刷新：同一次失败常同时触发 vite:preloadError 与载入函数的异常，只处理一次
let recovering = false

// useUnsavedGuard 在组件存续期间登记“有未保存的输入”的判定（KeepAlive 缓存的组件切走后仍保留登记），卸载时移除
export function useUnsavedGuard(check: () => boolean): void {
  unsavedChecks.add(check)
  onBeforeUnmount(() => {
    unsavedChecks.delete(check)
  })
}

// trackPendingWrite 登记一个在途的写操作并原样返回它，页面文件载入失败后的自动刷新会先等它结束（见 waitPendingWrites）
export function trackPendingWrite<T>(work: Promise<T>): Promise<T> {
  pendingWrites.add(work)
  const remove = (): void => {
    pendingWrites.delete(work)
  }
  // 两个参数都处理，写操作失败时这里不再产生未处理的拒绝；失败照常由调用方处理
  work.then(remove, remove)
  return work
}

// waitPendingWrites 等在途的写操作全部结束，最多等 PENDING_WRITE_WAIT；等待期间又有新的写操作（例如串行保存的下一次）时一并等。
// 全部结束时返回 true，到时仍有没结束的返回 false
async function waitPendingWrites(): Promise<boolean> {
  const deadline = performance.now() + PENDING_WRITE_WAIT
  while (pendingWrites.size > 0) {
    const left = deadline - performance.now()
    if (left <= 0) return false
    let timer: number | undefined
    await Promise.race([
      Promise.allSettled([...pendingWrites]),
      new Promise<void>((resolve) => {
        timer = window.setTimeout(resolve, left)
      }),
    ])
    window.clearTimeout(timer)
  }
  return true
}

// hasUnsavedInput 判断是否有组件报告未保存的输入；判定本身出错时按有处理，宁可不刷新
function hasUnsavedInput(): boolean {
  for (const check of unsavedChecks) {
    try {
      if (check()) return true
    } catch {
      return true
    }
  }
  return false
}

// reloadAllowed 判断距上次自动刷新是否已超过间隔；sessionStorage 不可用时返回 false
function reloadAllowed(): boolean {
  try {
    const last = Number(window.sessionStorage.getItem(RELOAD_STORAGE_KEY) ?? 0)
    // 时钟回拨时同样按间隔判断，不把很久以前或未来的时间当成刚刷新过之外的情形
    return !(last > 0 && Math.abs(Date.now() - last) < RELOAD_INTERVAL)
  } catch {
    return false
  }
}

// markReload 记下这次自动刷新的时间，写入后读回一致才算成功
function markReload(): boolean {
  try {
    const now = String(Date.now())
    window.sessionStorage.setItem(RELOAD_STORAGE_KEY, now)
    return window.sessionStorage.getItem(RELOAD_STORAGE_KEY) === now
  } catch {
    return false
  }
}

// entryScript 返回页面入口脚本的地址：构建产物为 /assets/index-<哈希>.js，开发服务器下没有
function entryScript(root: ParentNode): string | null {
  for (const script of root.querySelectorAll<HTMLScriptElement>('script[type="module"][src]')) {
    const source = script.getAttribute('src') ?? ''
    if (source.includes('/assets/')) return source
  }
  return null
}

// buildChanged 重新读取入口页面，判断服务端是否已换成另一版构建：连不上服务、入口读不出或与当前相同时返回 false
// （网络抖动或服务离线时刷新只会换来浏览器的错误页）
async function buildChanged(): Promise<boolean> {
  const current = entryScript(document)
  if (current === null) return false
  try {
    const response = await fetch(window.location.pathname, {
      cache: 'no-store',
      credentials: 'same-origin',
    })
    if (!response.ok) return false
    const latest = entryScript(new DOMParser().parseFromString(await response.text(), 'text/html'))
    return latest !== null && latest !== current
  } catch {
    return false
  }
}

// recoverFromAssetError 在页面文件（懒加载块、样式或脚本）载入失败后调用：服务端已换成新版构建时自动刷新一次
export async function recoverFromAssetError(): Promise<void> {
  if (recovering || !reloadAllowed()) return
  recovering = true
  try {
    if (!(await buildChanged())) return
    // 刷新会把在途的写操作中途打断（保存配置的第二次提交还没发出、刷新后读到的是保存前的配置等），先等它们结束；
    // 几秒内没结束（例如上万个账户的批量验证）时与有未保存的输入一样不刷新
    if (!(await waitPendingWrites()) || hasUnsavedInput()) {
      assetReloadBlocked.value++
      return
    }
    if (!markReload()) return
    window.location.reload()
  } finally {
    recovering = false
  }
}

// assetElement 判断资源载入失败的元素是否为构建产物的脚本或样式
function assetElement(target: EventTarget | null): boolean {
  if (target instanceof HTMLScriptElement) return target.src.includes('/assets/')
  if (target instanceof HTMLLinkElement) return target.href.includes('/assets/')
  return false
}

// installAssetRecovery 监听 Vite 的预加载失败与脚本、样式元素的载入失败（元素的 error 事件不冒泡，在捕获阶段监听）。
// 不调用 preventDefault：不刷新时（间隔内、版本没变、有未保存的输入）原来的错误照常抛给调用方，页面显示载入失败
export function installAssetRecovery(): void {
  window.addEventListener('vite:preloadError', () => {
    void recoverFromAssetError()
  })
  window.addEventListener(
    'error',
    (event) => {
      if (assetElement(event.target)) void recoverFromAssetError()
    },
    true,
  )
}
