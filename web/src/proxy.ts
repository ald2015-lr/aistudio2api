// 代理的显示辅助：只用于界面显示（掩码、带认证标记、搜索），校验与规范化以服务端 NormalizeProxy 为准

// colonProxyPattern 与服务端一致：host:port:用户名:密码（代理商面板最常见的写法），密码里可以有 : 和 @
const colonProxyPattern = /^(\[[0-9A-Fa-f:.]+\]|[^:@[\]/]+):([0-9]{1,5}):(.*)$/
// hostPortPattern 粗略判断拆出的部分是否像 host[:port]；不像时掩码显示为 ***，不把可能含密码的片段显示出来
const hostPortPattern = /^(\[[0-9A-Fa-f:.]+\]|[^\s:@[\]/?#]+)(:[0-9]{1,5})?$/
// hostPortLikePattern 与服务端一致：用户名:密码 段像 IPv4、带点域名或 [v6] 加端口（账号密码与 host:port 可能写反了）
const hostPortLikePattern =
  /^(?:\[[0-9A-Fa-f:.]+\]|(?:[A-Za-z0-9_-]|[\u0080-\uFFFF])+(?:\.(?:[A-Za-z0-9_-]|[\u0080-\uFFFF])+)+):[0-9]{1,5}\/?$/

// isIPv6 判断 IPv6 字面量（不带方括号，可以以 IPv4 结尾）
function isIPv6(value: string): boolean {
  let groups = value
  let tail = 0
  const lastColon = value.lastIndexOf(':')
  if (value.slice(lastColon + 1).includes('.')) {
    if (
      !/^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/.test(
        value.slice(lastColon + 1),
      )
    )
      return false
    groups = value.slice(0, lastColon + 1) + '0'
    tail = 1
  }
  const halves = groups.split('::')
  if (halves.length > 2) return false
  const parse = (part: string): string[] | null => {
    if (part === '') return []
    const items = part.split(':')
    return items.every((item) => /^[0-9A-Fa-f]{1,4}$/.test(item)) ? items : null
  }
  const head = parse(halves[0] ?? '')
  const rest = halves.length === 2 ? parse(halves[1] ?? '') : []
  if (head === null || rest === null) return false
  const count = head.length + rest.length + tail
  return halves.length === 2 ? count <= 7 : count === 8
}

// bareIPv6 与服务端一致：没加方括号的 IPv6 地址，可以带端口（2001:db8::1、2001:db8::1:8080）
function bareIPv6(hostPort: string): boolean {
  if ((hostPort.match(/:/g)?.length ?? 0) < 2 || /[[\]]/.test(hostPort)) return false
  return isIPv6(hostPort) || isIPv6(hostPort.slice(0, hostPort.lastIndexOf(':')))
}

// ambiguousProxyOrder 与服务端一致：host 段不是 IP、不含点、也不是 [v6]，而 用户名:密码 段像 host:port 时，
// 服务端报错（数字密码会被当成端口）；这里同样不把“端口”（其实是密码）显示出来
function ambiguousProxyOrder(host: string, credentials: string): boolean {
  return !host.startsWith('[') && !host.includes('.') && hostPortLikePattern.test(credentials)
}

// pathUnescape 与服务端 url.PathUnescape 一致：有不合法的 %XX 时整段按字面；只用于歧义判定，解码出的非 ASCII 字节一律
// 记成 \u0080（服务端的正则把它们当成非 ASCII 字符，判定结果相同）
function pathUnescape(value: string): string {
  if (/%(?![0-9A-Fa-f]{2})/.test(value)) return value
  return value.replace(/%([0-9A-Fa-f]{2})/g, (_match, hex: string) => {
    const code = Number.parseInt(hex, 16)
    return code < 0x80 ? String.fromCharCode(code) : '\u0080'
  })
}

// atCredentials 返回 @ 写法里解码后的 用户名[:密码]，与服务端 unescapeUserinfo 的拆分方式一致
function atCredentials(userinfo: string): string {
  const colon = userinfo.indexOf(':')
  if (colon < 0) return pathUnescape(userinfo)
  return `${pathUnescape(userinfo.slice(0, colon))}:${pathUnescape(userinfo.slice(colon + 1))}`
}

interface ProxySplit {
  scheme: string
  hostPort: string
  auth: boolean
}

// splitProxy 按服务端 NormalizeProxy 的优先级拆分：带 :// 先取协议；host:port:用户名:密码 优先；
// 否则按最后一个 @ 拆分；否则整体为 host:port
function splitProxy(value: string): ProxySplit | null {
  const raw = value.trim()
  if (raw === '') return null
  let scheme = 'http'
  let rest = raw
  // legacyScheme 为旧规则接受的协议（http、https、socks5，不含 socks5h），只用于判断是否按不带认证的旧值原样保留
  let legacyScheme = false
  const index = raw.indexOf('://')
  // :// 之前不像协议名时（例如密码里含 ://）整体按不写协议处理，宁可多掩码也不把密码显示出来
  if (index >= 0 && /^[A-Za-z][A-Za-z0-9+.-]*$/.test(raw.slice(0, index))) {
    scheme = raw.slice(0, index).toLowerCase()
    legacyScheme = scheme === 'http' || scheme === 'https' || scheme === 'socks5'
    if (scheme === 'socks5h') scheme = 'socks5'
    rest = raw.slice(index + 3)
    // 带协议、不加方括号的 IPv6（http://2001:db8::1:3128）：服务端按不带认证的旧值原样保留，这里也原样显示
    if (!rest.includes('@') && bareIPv6(rest.replace(/\/$/, '')))
      return { scheme, hostPort: rest, auth: false }
  }
  const colon = colonProxyPattern.exec(rest)
  const credentials = colon?.[3] ?? ''
  // 与服务端 colonProxyCredentials 及直通分支一致：写了旧规则接受的协议、host 不是 [v6]、第 3 段里没有冒号或用户名为空，
  // 而且最后一个冒号之后是数字或为空（http://h:1:2、http://h:1::2）时，旧规则把它读成主机名带冒号的无认证 URL，
  // 服务端原样保留，这里也不走冒号写法
  const legacy =
    legacyScheme &&
    !(colon?.[1] ?? '').startsWith('[') &&
    (!credentials.includes(':') || credentials.startsWith(':')) &&
    !rest.includes('@') &&
    /:\d*\/?$/.test(rest)
  if (colon !== null && !legacy) {
    const host = colon[1] ?? ''
    // 用户名:密码:host:port、密码是纯数字时服务端报错；这里不把“端口”（其实是密码）显示出来，掩码显示为 ***
    if (ambiguousProxyOrder(host, credentials)) return { scheme, hostPort: '', auth: true }
    return { scheme, hostPort: `${host}:${colon[2]}`, auth: true }
  }
  rest = rest.endsWith('/') ? rest.slice(0, -1) : rest
  const at = rest.lastIndexOf('@')
  if (at >= 0) {
    const hostPort = rest.slice(at + 1)
    const host = hostPort.includes(':') ? hostPort.slice(0, hostPort.lastIndexOf(':')) : hostPort
    // host:port@用户名:密码、密码是纯数字时服务端报错（按解码后的值判断），同样掩码显示为 ***
    if (ambiguousProxyOrder(host, atCredentials(rest.slice(0, at)))) {
      return { scheme, hostPort: '', auth: true }
    }
    return { scheme, hostPort, auth: true }
  }
  return { scheme, hostPort: rest, auth: false }
}

// proxyParts 粗略拆出代理的协议、host:port 与是否带账号密码；规则与服务端 NormalizeProxy 的优先级一致：
// 带 :// 先取协议；host:port:用户名:密码 优先；否则按最后一个 @ 拆分；否则整体为 host:port。拆不出 host:port 时返回 null
export function proxyParts(
  value: string,
): { scheme: string; hostPort: string; auth: boolean } | null {
  const split = splitProxy(value)
  if (split === null || !hostPortPattern.test(split.hostPort)) return null
  return split
}

// proxyHasAuth 判断代理是否带账号密码；拆不出 host:port 但带账号密码的值（例如 user:pass@）同样算带认证
export function proxyHasAuth(value: string): boolean {
  return splitProxy(value)?.auth ?? false
}

// maskProxy 只处理带账号密码的值：返回去掉账号密码的 scheme://host:port，拆不出 host:port 时返回 '***'；
// 不带账号密码（含空值）时原样返回，不改动现有显示（例如 HTTP://H:1/ 仍显示 HTTP://H:1/）
export function maskProxy(value: string): string {
  if (!proxyHasAuth(value)) return value
  const parts = proxyParts(value)
  return parts ? `${parts.scheme}://${parts.hostPort}` : '***'
}
