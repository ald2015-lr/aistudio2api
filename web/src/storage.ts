// readStorage 读取浏览器本地存储；浏览器禁用站点存储（隐私模式、策略限制）时访问会抛异常，按未保存处理
export function readStorage(key: string): string | null {
  try {
    return window.localStorage.getItem(key)
  } catch {
    return null
  }
}

// writeStorage 写入浏览器本地存储；不可用时忽略，只是不记住这项偏好
export function writeStorage(key: string, value: string): void {
  try {
    window.localStorage.setItem(key, value)
  } catch {
    // 本地存储不可用或已满
  }
}
