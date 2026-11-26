import { readonly, ref } from 'vue'
import { legacyLocales } from '@/legacy-locales'
import type { Locale, UpstreamChannel } from '@/types'
import { readStorage, writeStorage } from '@/storage'

const zhCN = {
  'app.title': 'AI Studio 控制台',
  'app.console': '控制台',
  'app.gateway': '协议网关',
  'app.online': '服务在线',
  'app.offline': '服务离线',
  'app.events': '实时事件',
  'app.reconnecting': '正在重连',
  'app.refresh': '刷新数据',
  'app.version': '版本',
  'app.status': '服务状态',
  'app.start': '启动服务',
  'app.stop': '停止服务',
  'app.eventsReconnecting': '实时连接已断开，正在重连…',
  'app.eventsClosed': '实时连接已断开（可能是管理令牌失效），请刷新页面',
  'app.statusUnreachable': '无法连接服务，显示的是上一次的状态',
  'app.noticeClose': '关闭提示',
  'app.noticeExpand': '展开全文',
  'app.noticeCollapse': '收起',
  'app.assetReloadBlocked':
    '管理页已经更新，部分页面文件载入失败。当前有未保存的输入或还没完成的保存与账户操作（服务配置、账户或 API 试用），没有自动刷新：请等操作完成，保存或记下这些内容后，再手动刷新浏览器',
  'nav.logs': '日志',
  'nav.accounts': '账户',
  'nav.models': '模型目录',
  'nav.requests': '冷却与请求',
  'nav.usage': '用量',
  'nav.settings': '服务配置',
  'nav.playground': 'API 试用',
  'section.accounts.title': '账户与登录',
  'section.accounts.description': '管理登录状态、账户出口与模型可用性',
  'section.models.title': '实时模型目录',
  'section.models.description': '来自健康账户的模型、方法与能力并集',
  'section.requests.title': '冷却与请求',
  'section.requests.description': '查看账户模型冷却状态与当前请求',
  'section.settings.title': '服务配置',
  'section.settings.description': '本地监听、鉴权、代理与超时',
  'section.playground.title': 'API Playground',
  'section.playground.description': '通过公开协议发送一次真实请求',
  'metric.readyAccounts': '就绪账户',
  'metric.activeRequests': '活动请求',
  'metric.models': '可用模型',
  'metric.totalAccounts': '账户总数',
  'common.loading': '加载中',
  'common.save': '保存',
  'common.cancel': '取消',
  'common.close': '关闭',
  'common.delete': '删除',
  'common.verify': '验证',
  'common.relogin': '重新登录',
  'common.search': '搜索',
  'common.add': '添加账户',
  'common.edit': '编辑',
  'common.enable': '启用',
  'common.disable': '停用',
  'common.empty': '暂无数据',
  'common.copy': '复制',
  'common.copied': '已复制',
  'common.error': '请求失败',
  'common.unknown': '未知',
  'accounts.empty': '暂无账户',
  'accounts.emptyHint': '通过浏览器登录或从 Chrome 导入账户',
  'accounts.proxy': '账户代理',
  'accounts.proxyHelp':
    '留空：IP 池开启时使用 IP 池（每个浏览器一个 IP），否则使用服务配置的默认代理。留空点“测试代理”：编辑已有账户时测这个账户当前实际的出口，账户原来有自己的代理（在这里清空了）时测清空后 IP 池会给它的出口（IP 池关闭时测默认代理）；添加新账户时测 IP 池此刻会分给登录的出口（IP 池关闭时测默认代理）。写法同默认代理；动态 IP 建议在代理商处开启固定会话（粘性 IP），否则浏览器页面与后台请求可能从不同 IP 出网。',
  'accounts.locale': '语言区域',
  'accounts.timezone': '时区',
  'accounts.models': '模型',
  'accounts.benefitTier': '账户权益',
  'accounts.direct': '直连',
  'accounts.browserLogin': '浏览器登录',
  'accounts.importChrome': 'Chrome 批量导入',
  'accounts.chromeTitle': '选择 Chrome 账户',
  'accounts.chromeEmpty': '没有可导入的 Chrome 账户',
  'accounts.importSelected': '导入所选账户',
  'accounts.selectAll': '全选',
  'accounts.deselectAll': '取消全选',
  'accounts.loginComplete': '账户登录完成',
  'accounts.verified': '账户验证通过',
  'accounts.verifyInvalid': '登录已失效：{reason}',
  'accounts.verifyOrgDisabled':
    '验证未通过：账号所属组织已停用 AI Studio（不是登录失效，重新登录也没用；已停止调度）',
  'accounts.verifyOrgDisabledReason':
    '账号所属组织已停用 AI Studio（不是登录失效，重新登录也没用）',
  'accounts.verifyResult': '验证完成：有效 {ok}，失效 {invalid}，出错 {failed}',
  'accounts.verifyResultOrgDisabled':
    '验证完成：有效 {ok}，失效 {invalid}，组织停用或受限 {org}，出错 {failed}',
  'accounts.verifyResultSkippedPaused': '；跳过 {count} 个 IP 暂停的账户（固定端口恢复后再验证）',
  'accounts.verifyRestricted':
    '验证未通过：{reason}（不是登录失效，重新登录也没用；已停止调度）',
  'accounts.verifyRestrictedReason': '{reason}（不是登录失效，重新登录也没用）',
  'accounts.saved': '账户已保存',
  'accounts.deleted': '账户已删除',
  'accounts.importComplete': '已导入 {count} 个账户',
  'accounts.accountExists':
    '该账户已存在，没有重复添加：在账户列表里状态为“需要登录”时对它使用“重新登录”，停用的先启用，其他状态不需要再添加；列表里没有它时是账户目录已在 auth 目录下，等目录扫描自动载入',
  'accounts.accountExistsReady': '{email} 已在号池中，没有重复添加，不需要再添加',
  'accounts.accountExistsRelogin':
    '{email} 已在号池中、需要登录，没有重复添加：请在账户列表里对它使用“重新登录”',
  'accounts.accountExistsDisabled':
    '{email} 已在号池中、已停用，没有重复添加：在账户列表里启用它即可，启用后需要登录时再对它使用“重新登录”',
  'accounts.accountDirectoryExists':
    '{email} 的账户目录已在 auth 目录下，没有重复添加：目录扫描会自动载入它，载入后需要登录时再对它使用“重新登录”',
  'accounts.invalidTimezone':
    '时区无效：请填写 IANA 时区名（如 Asia/Shanghai），不能含空白或不可见字符',
  'accounts.transitionDraining': '已停用，等 {count} 个请求结束后关闭 Worker',
  'accounts.transitionWaiting':
    '已停用，等账户上的其他操作（登录、验证、Worker 启动等）结束后关闭 Worker',
  'accounts.transitionClosing': '已停用，正在关闭 Worker',
  'accounts.transitionSyncing': '已启用，正在同步模型目录',
  'accounts.transitionHelp':
    '启用或停用已经生效：停用的账户不再接新请求，进行中的请求照常结束；等请求结束、关闭 Worker、同步模型目录在后台进行',
  'accounts.chromeExisting': '已导入 · 就地重新导入',
  'accounts.chromeExistingHelp':
    '导入时只覆盖这个账户的 Cookie 与续签材料，保留账户设置、浏览器指纹、资源绑定与运行态；上面的代理、语言区域、时区只用于新账户',
  'accounts.chromeSelectAllHint':
    '全选只选新账号与需要重新登录的账户；状态正常的已导入账户请单独勾选（重新导入会关闭它正在服务的 Worker）',
  'accounts.chromeImportPartial':
    '已导入 {count} 个账户，{failed} 个已有账户没有重新导入：{details}',
  'accounts.editTitle': '编辑账户',
  'accounts.deleteConfirm': '确认删除此账户？',
  'logs.level': '日志等级',
  'logs.source': '来源',
  'logs.allSources': '全部',
  'logs.clear': '清空日志',
  'logs.autoScroll': '自动滚动',
  'logs.waiting': '等待日志输出...',
  'logs.search': '搜索模型、请求 ID、账户或消息',
  'logs.running': '处理中',
  'logs.tools': '工具调用',
  'logs.loadOlder': '加载更早日志 ({count})',
  'logs.showingRecent': '展示最近 {count} 条',
  'logs.reasoning': '思考',
  'logs.reply': '回复',
  'logs.outputTotal': '输出合计',
  'logs.averageSpeed': '平均速度',
  'logs.averageSpeedHelp':
    '输出 token 除以首个上游分块到达之后到请求结束的时长：排队、WAA、等待首个分块与被放弃的重新生成都不算；工具约束要求必须调用时从扣住第一个上游事件起算',
  'logs.averageSpeedTotalHelp':
    '输出 token 除以请求总耗时（含排队与等待首个分块）：只有一个分块、思考不对外而用量带思考 token 时测不出生成速度，或记录来自旧版本',
  'logs.details': '详情',
  'logs.endpoint': '接口',
  'logs.inputTokens': '输入用量',
  'logs.totalTokens': '总用量',
  'logs.inputMessages': '输入消息',
  'logs.finishReason': '终止原因',
  'logs.downgrade': '降级判定',
  'logs.all': '全部',
  'logs.info': '信息',
  'logs.warn': '警告',
  'logs.error': '错误',
  'models.search': '搜索模型、方法或能力',
  'models.allMethods': '全部方法',
  'models.model': '模型',
  'models.methods': '方法',
  'models.capabilities': '能力',
  'models.context': '输入上限',
  'models.output': '输出上限',
  'models.paid': 'Paid',
  'models.empty': '当前没有可用模型',
  'requests.live': '进行中',
  'requests.history': '请求摘要',
  'requests.id': '请求 ID',
  'requests.account': '账户',
  'requests.model': '模型',
  'requests.started': '开始时间',
  'requests.action': '操作',
  'requests.stop': '停止',
  'requests.empty': '当前没有请求',
  'cooldowns.title': '模型冷却',
  'cooldowns.until': '恢复时间',
  'cooldowns.empty': '当前没有冷却账号',
  'accounts.cooldownTitle': '模型冷却',
  'accounts.cooldownSummary': '{accounts} 个账号冷却中，共 {entries} 条模型冷却',
  'accounts.cooldownEarliestAll': '最早 {time} 后恢复（{clock}）',
  'accounts.cooldownLatestAll': '全部恢复还需 {time}（{clock}）',
  'accounts.cooldownModel': '模型 · 通道',
  'accounts.cooldownAccounts': '冷却账号',
  'accounts.cooldownFirst': '最早恢复',
  'accounts.cooldownLast': '全部恢复',
  'accounts.cooldownReason': '主要原因',
  'accounts.cooldownVerifyExcluded': '另有 {count} 个需要验证账号的账户整户暂停中，未计入',
  'accounts.cooldownVerifyExcludedOther':
    '另有 {count} 个需要验证账号的账户整户暂停中，未计入（含 {other} 个需要登录、组织已停用或不可用的账户，它们不在需要验证账号汇总块里）',
  'accounts.cooldownVerifyExcludedHelp':
    '按需要验证账号整户暂停仍生效的账户计，不看账户状态：它们的全部冷却条目（整户暂停按每个模型展开的条目与单模型冷却）都不计入这里的账号数、条目数、恢复时间与主要原因。需要登录、组织已停用、不可用的账户也算在这个数里，但不计入需要验证账号汇总块（与“出问题（403）”一致），所以这里可能多于汇总块里整户暂停中的个数；暂停已到期的账户照常计入冷却。',
  'accounts.cooldownVerifyModelExcluded': '另有 {count} 个账户的单个模型需要验证暂停未计入',
  'accounts.cooldownVerifyModelExcludedHelp':
    '单个模型需要验证账号的暂停只挡住那一个模型（最长 30 天），这些条目不计入这里的账号数、条目数、恢复时间与主要原因（该账户的其他冷却照常计入）；它们另见“403 单个模型需要验证”。',
  'accounts.cooldownShowAll': '展开全部 {count} 个模型',
  'accounts.cooldownShowLess': '收起',
  'accounts.cooldownSoon': '即将恢复',
  'accounts.cooldownSeconds': '{s} 秒',
  'accounts.cooldownMinutes': '{m} 分 {s} 秒',
  'accounts.cooldownHours': '{h} 小时 {m} 分',
  'settings.authPath': '账户状态目录',
  'settings.listen': '监听地址',
  'settings.apiKey': '本地 API 密钥',
  'settings.proxy': '默认代理',
  'settings.proxyHelp':
    '支持 http、https、socks5，可带账号密码：http://用户名:密码@IP:端口、socks5://用户名:密码@IP:端口、IP:端口:用户名:密码、用户名:密码@IP:端口；不写协议按 http。保存时统一改写成 URL 形式，密码里的特殊字符自动编码。URL 写法与 用户名:密码@ 写法里的 %XX 按百分号编码解码，密码里要保留 %41 这类字面内容时请用 IP:端口:用户名:密码 写法。带账号密码时，浏览器经本机中转连接代理。',
  'proxy.placeholder': 'http://用户名:密码@1.2.3.4:8080',
  'proxy.editHint': '点击显示并编辑完整代理（含账号密码）',
  'proxy.authTag': '带认证',
  'proxy.test': '测试代理',
  'proxy.testing': '测试中…',
  'proxy.testHint': '经代理访问 api.ipify.org 查询出口 IP，不访问 Google。',
  'proxy.testBrowser': '浏览器路径',
  'proxy.testBrowserRelay': '浏览器路径（经本机中转）',
  'proxy.testDirect': '后台请求路径',
  'proxy.testOK': '出口 IP {ip} · {ms} 毫秒',
  'proxy.testFailed': '失败：{error}',
  'proxy.warnRotating':
    '两次连接的出口 IP 不同：这是按连接轮换的动态代理，浏览器页面与后台请求会从不同 IP 出网。建议在代理商处使用固定会话并设置较长的会话时长。',
  'proxy.warnHttpsPlain':
    'https:// 且不带账号密码的代理，浏览器（Camoufox）会以明文 HTTP 代理的方式连接，登录与 WAA 多半不可用；请改用 http:// 写法，或使用带账号密码的写法（由本机中转负责 TLS）。',
  'settings.initTimeout': '初始化超时',
  'settings.requestTimeout': '请求超时',
  'settings.requestTimeoutHelp':
    '生成请求在首个上游事件之前的总期限：排队、冷启动、WAA proof、附件上传、等首字与首事件前的换号都计入，到时返回 504。开始输出后改为空闲上限：上游连续这么久既没有事件也没有网络字节才以 504 结束，持续出字的长回复不会被切断；整条生成请求最长 1 小时（本项更长时按本项）。计数、视频与转写等其他请求整段受它限制。修改后立即生效，只影响新请求。',
  'settings.firstEventTimeout': '首事件超时',
  'settings.firstEventTimeoutHelp':
    '每次尝试从向上游发送起等待第一个上游事件的上限：超时只放弃这一次尝试并换号重试（不额外冷却账号），不能再换号时返回 504；客户端还没收到内容时的同账号重新生成每次发送重新起算。0 或 off 表示关闭（默认）；可以写整数秒（60 即 60 秒，与上游 Mag1cFall/AIStudio2API 的写法相同）或时长（90s、2m）；思考很长的模型可能很久才有第一个事件，开启时要留足余量。',
  'settings.streamDataHeartbeat': '数据型流式心跳',
  'settings.streamDataHeartbeatHelp':
    '每 10 秒没有输出时写一块空数据（Gemini SSE 为空文本或空思考的候选块，OpenAI Chat 为空 delta 块），首个事件还没到就提交响应头时提交时也写一块，代替只写换行或 SSE 注释。new-api 等中转只转发 data 行，开启后中转后面的客户端在等待首字和长思考时也能持续收到字节，不会因长时间零字节被手机系统、VPN 或代理断开。官方 Gemini SDK 直连时仍只写换行（直连不经过中转，不再多写一种 SDK 判为无效的块）；开启后 new-api 的首字时间统计反映的是写出第一块心跳的时刻，且客户端断开或流被截断、没有用量数据时 new-api 按估算的输入 token 计费（原先计 0）。关闭时与原先完全一致。修改后立即生效，只影响新请求。',
  'settings.warmWorkerLimit': '常驻 Worker 数',
  'settings.maxActiveWorkers': '峰值 Worker 上限',
  'settings.warmStartupConcurrency': '启动预热并发',
  'settings.perAccountConcurrency': '单账户请求并发',
  'settings.proTitle': 'Pro 号池',
  'settings.proHelp':
    '权益为 Pro 的账户组成 Pro 号池（开启下方“Pro 独立号池”时）。客户端把 Base URL 换成 /pro 前缀（OpenAI 用 http://主机:端口/pro/v1，Anthropic 与 Gemini 用 http://主机:端口/pro）即只用 Pro 号池的账户处理请求；/pro 的全部接口与默认地址相同，排查路由为 /trace/pro。修改后立即生效，不需要重启服务。',
  'settings.proPool': 'Pro 独立号池',
  'settings.proPoolHelp':
    '开启（默认）时权益为 Pro 的账户只服务 /pro 前缀：默认地址与 /ultra 都不再使用它们（下方 Ultra 号池的“Pro 账户并入 Ultra 号池”不生效），/pro 也不借用 Ultra 或普通号池的账户——Pro 号池没有可用账户时返回 503，候选全部冷却、额度用完或排队超限时返回 429 并带 Retry-After；开启补充池时例外：/pro 也借用补充池里其他号池的成员，补充池里的 Pro 成员也借给默认地址与 /ultra（权益规则照常）。Pro 不能用只限 Ultra 的模型（gemini-4-carbon 不在 /pro 的模型列表里，请求返回 404），这一点靠下方的“仅 Ultra 可用的模型”（ULTRA_ONLY_MODELS），清空它会让 Pro 号池也能选到 carbon；gemini-4-argon、gemini-3.1-pro-preview 照常可用。/pro 沿用 Ultra/Pro 主用模型列表与排队上限。资源不迁移：Pro 账户上已有的文件与视频任务要改从 /pro 引用（否则返回 400 并说明改用哪个入口）。关闭后与没有这项设置之前相同（Pro 账户留在默认地址，开启“Pro 账户并入 Ultra 号池”时并入 /ultra），新的 /pro 请求返回 404，切换时仍在进行的 /pro 请求返回 503。修改后立即生效，已运行的 Worker 改按新的分区计数，不会重启；建议在同一次保存里调好下方两个 Worker 数，避免多出的 Pro Worker 被逐步回收。',
  'settings.proWarmWorkerLimit': 'Pro 常驻 Worker 数',
  'settings.proMaxActiveWorkers': 'Pro 峰值 Worker 上限',
  'settings.proWorkersHelp':
    'Pro 号池账户的 Worker 单独计数：预热、空闲回收与淘汰都只在 Pro 分区内进行，不会挤占普通号池或 Ultra 号池的 Worker，反之亦然。默认 2 与 5 偏小，原来由普通分区或 Ultra 分区承担 Pro 流量时，建议把那边的 Worker 数挪一部分过来，浏览器总数保持不变。常驻数可以为 0（只在有请求时启动），峰值至少为 1 且不小于常驻数。',
  'settings.workerPartitionHelp':
    '常驻 Worker 数与峰值 Worker 上限只约束普通号池（不含 Pro 与 Ultra）；Pro 号池与 Ultra 号池的 Worker 在下方单独设置，浏览器总数最多为三个分区的峰值之和，开启补充池时再加补充池峰值（关闭 Pro 独立号池或没有 Pro 账户时 Pro 分区为空）。启动预热并发与单账户请求并发各号池共用，冷启动按普通、Ultra、Pro 的顺序分配启动预热并发。单账户请求并发是硬上限：同一账号同时进行的请求不超过此数（所有请求类型，含流式与非流式、Playground 与 Build、countTokens 与文件操作）；修改后立即生效，调小时已在进行的请求照常跑完。',
  'settings.extraTitle': '补充池（额外浏览器号池）',
  'settings.extraHelp':
    '有模型今天用完、其他模型还能用的账户（例如 gemini-4-argon 的每日额度用完、flash 还能用）单独开一批浏览器，补给默认地址、/pro 与 /ultra。账户所属的号池不变：完全没冷却的账户留在原号池，需要登录、已停用、整户冷却或初始化模型全都在冷却的账户不会进来。默认关闭，关闭时一切与没有这项设置之前相同。修改后立即生效，不需要重启服务。',
  'settings.extraPool': '开启补充池',
  'settings.extraPoolHelp':
    '有模型冷却、剩余不少于 15 分钟，而且还有别的模型能用的账户成为成员；冷却结束（太平洋时间零点额度恢复时会成批结束）、整户不可用或权益不在范围时离开：原分区有空位就原地归还 Worker，否则在空闲时关闭。分钟限额等短冷却、只在单个模型上需要验证账号的不算。Free（含 Plus 与权益尚未读取的，按 Free 计）与 Pro 账户可以进入，Ultra 看下方开关。开启后补充池按常驻数预热，主分区换下成员的空闲 Worker 时原地接手，不关浏览器。',
  'settings.extraPoolMode': '使用方式',
  'settings.extraModeShare': '一起分担（默认）',
  'settings.extraModeOverflow': '主号池不够时才补',
  'settings.extraModeHelp':
    '一起分担：补充池里已就绪的 Worker 与主号池的 Worker 轮流接请求。主号池不够时才补：主号池的热 Worker 都忙、候选全部冷却、额度 429 之后只能用已启动的 Worker 时立即借补充池；主号池的 Worker 正在启动或出口限速时排队满 3 秒再借；主号池还能现场冷启动备用号时，由这个请求先冷启动主号池（约 10–30 秒），期间不借。两种方式都只补请求入口本来就有的模型（下方默认地址的付费模型除外）。',
  'settings.extraDefaultPaid': '默认地址可借补充池的付费账户',
  'settings.extraDefaultPaidHelp':
    '开启（默认）时，默认地址能借补充池里的 Pro 账户（开启下方 Ultra 开关时还有 Ultra 账户）跑默认地址模型列表之外的模型，例如 gemini-4-argon 与 PAID_ONLY_MODELS 里的模型，前提是那个号这个模型还没用完：只有“别的模型已冷却、这个付费模型还能用”的 Pro（或 Ultra）成员借得到，gemini-4-argon 用完的 Pro 号在默认地址上借给 flash 等模型用，不是借给 argon；完全没冷却的健康 Pro 号留在 /pro。这些模型随之出现在默认地址的 /v1/models 里，只能由补充池服务：补充池范围内能跑它的账户全部冷却时返回 429 并带 Retry-After；没有这样的成员或此刻借不到 Worker 时模型仍在列表里，请求返回 503。要稳定使用 gemini-4-argon 请走 /pro。权益规则照常，Free 永远跑不了付费模型，Pro 跑不了只限 Ultra 的模型。关闭时各入口只补充自己原本就有的模型；/pro 与 /ultra 不受这项影响。',
  'settings.extraIncludeUltra': 'Ultra 账户也进补充池',
  'settings.extraIncludeUltraHelp':
    '默认关闭：权益为 Ultra 的账户不会成为成员。打开后有模型冷却的 Ultra 账户也补给默认地址与 /pro（只跑这些入口允许的模型；开启上方付费开关时默认地址还能借它跑只限 Ultra 的模型）。借用 Ultra 账户会消耗 Ultra 出口通道的令牌，可能挤占 /ultra。',
  'settings.extraWarmWorkerLimit': '补充池常驻 Worker 数',
  'settings.extraMaxActiveWorkers': '补充池峰值 Worker 上限',
  'settings.extraWorkersHelp':
    '补充池的 Worker 单独计数，不占普通、Pro、Ultra 分区的名额。常驻数（默认 40）是预热与接手补到的数量，可以为 0（只在请求需要时扩容）；峰值（默认 50）至少为 1 且不小于常驻数，超出常驻、空闲满 5 分钟的逐个回收。补充池预热排在主分区之后，共用启动预热并发。每个浏览器约 0.6GB 内存：常驻 40 个约 24GB，峰值 50 个加替换时多出的 4 个约 32GB；开启 IP 池时端口要多“峰值 + 替换余量”个（默认 54）；浏览器多时进程的打开文件数上限建议不低于 65536（ulimit -n）。',
  'settings.extraResourceHint':
    '按当前峰值，补充池最多同时 {browsers} 个浏览器（峰值 {max} + 替换 {drain}），约多占 {memory}GB 内存，开启 IP 池时多需要 {browsers} 个端口。',
  'settings.extraOffNote': '补充池关闭：下面的设置照常保存，开启后才生效。',
  'settings.invalidExtraWarmWorkers': '补充池常驻 Worker 数必须是 0 或正整数',
  'settings.invalidExtraMaxWorkers':
    '补充池峰值 Worker 上限必须是正整数，且不能小于补充池常驻 Worker 数',
  'settings.accountRpmLimit': '单账号每分钟请求上限',
  'settings.accountRpmLimitHelp':
    '每个账号最近 60 秒最多发出的生成请求数（上游每账号每分钟 12 次，所有模型、两个通道共用）；满了就算忙碌，请求换到其他账号，热 Worker 都满时启动新的 Worker；只统计本服务，上游的 429 冷却仍然兜底；默认 10，比 12 留出余量；0 表示关闭；修改后立即生效。',
  'settings.ultraTitle': 'Ultra 号池',
  'settings.ultraHelp':
    '权益为 Ultra 的账户组成 Ultra 号池（付费号池）；关闭上方“Pro 独立号池”、开启下方“Pro 账户并入 Ultra 号池”时，权益为 Pro 的账户也属于它。客户端把 Base URL 换成 /ultra 前缀（OpenAI 用 http://主机:端口/ultra/v1，Anthropic 与 Gemini 用 http://主机:端口/ultra）即只用 Ultra 号池的账户处理请求。修改后立即生效，不需要重启服务。',
  'settings.ultraExclusive': 'Ultra 号池的账户只服务 /ultra 请求',
  'settings.ultraExclusiveHelp':
    '开启（默认）时普通路径只用其余账户，Ultra 号池的账户留给 /ultra；关闭时普通路径也可以用 Ultra 号池的账户，/ultra 仍只用 Ultra 号池的账户；开启 Pro 独立号池时普通路径仍不使用 Pro 账户，所以 gemini-4-argon 等付费模型在默认地址仍可经 Ultra 账户使用，不是只有 /pro 与 /ultra 才有。开启补充池时各入口还会借用补充池里其他号池的成员（权益规则照常）。',
  'settings.ultraIncludePro': 'Pro 账户并入 Ultra 号池（付费号池）',
  'settings.ultraIncludeProHelp':
    '只在关闭上方“Pro 独立号池”（PRO_POOL=false）时生效。开启后权益为 Pro 的账户与 Ultra 账户同属 Ultra 号池，按 Ultra 号池的全部设置调度：经 /ultra 前缀服务并列入 /ultra 的模型列表，受上方“Ultra 号池的账户只服务 /ultra 请求”约束，Worker 计入下方 Ultra 常驻数与峰值上限，适用 Ultra 主用模型列表，也计入侧栏与账户页的 Ultra 号池计数。账户的真实权益不变：账户页仍显示 Pro（另加 Ultra 号池标记），模型资格、输入上限与额度共享仍按 Pro 计。上方设置开启时普通路径不再使用 Pro 账户，普通号池容量随之变小，gemini-4-argon 与需要订阅权益的模型会从普通路径的模型列表（/v1/models）消失，客户端要改用 /ultra 前缀。Pro 与 Ultra 共用下方两个 Worker 上限，默认值偏小，建议调大；Ultra 常驻 Worker 数为 1（ULTRA_WARM_WORKER_LIMIT=1）时常驻的可能是 Pro 账户而不是 Ultra 账户，建议不小于 2。资源不迁移，Pro 账户上已有的文件与视频任务切换后可能要换入口引用：开启本项后，上方设置开启时经普通路径引用会返回 400，要改用 /ultra 前缀（上方设置关闭时不受影响）；关闭本项后经 /ultra 引用会返回 400，要改用普通路径。建议在没有进行中的视频任务时切换。默认关闭；修改后立即生效，不需要重启服务，已运行的 Worker 改按新的分区计数，不会重启。',
  'settings.ultraIncludeProSharedHint':
    '上方“Ultra 号池的账户只服务 /ultra 请求”（ULTRA_EXCLUSIVE）已关闭：普通路径仍会使用 Pro 与 Ultra 账户，并入 Ultra 号池的 Pro 账户照常服务普通路径的请求。要让 Pro 账户只服务 /ultra 请求，请同时开启上方设置。',
  'settings.ultraIncludeProIgnoredHint':
    'Pro 独立号池（PRO_POOL）开启时本项不生效：Pro 账户只服务 /pro。关闭 Pro 独立号池后按本项设置并入 Ultra 号池。',
  'settings.ultraOnlyModels': '仅 Ultra 可用的模型',
  'settings.ultraOnlyModelsHelp':
    '列表中的模型只分给权益为 Ultra 的账户：Pro 号池与并入 Ultra 号池的 Pro 账户都不会被选去服务它们，也不计入它们的预热与供给；“Pro 不能用 gemini-4-carbon”只靠这份列表，从中去掉 carbon 后 /pro 也能用它（服务会写一条 WARN）。逗号分隔的模型 ID，默认 gemini-4-carbon；清空即没有规则。',
  'settings.paidOnlyModels': '仅付费账号可用的模型（Pro 与 Ultra）',
  'settings.paidOnlyModelsHelp':
    '列表中的模型只分给权益为 Pro 或 Ultra 的账户，Free 与 Plus 账户不会被选去服务它们。逗号分隔的模型 ID，默认 gemini-3.1-pro-preview；清空即没有规则。',
  'settings.tierRulesHelp':
    '账户的模型目录不一定标注哪些模型只限付费或只限 Ultra，这两份列表补上这一层：只收窄、不放宽（目录本身限制的模型仍按目录），按账户的真实权益判断，权益尚未读取的账户按 Free 处理，读到后自动恢复。写基础模型 ID（如 gemini-4-carbon，不要带 -high、-nothinking 等后缀，也不要带 long- 前缀），覆盖它的全部后缀写法与目录别名，大小写不限。号池里没有账户能用的模型会从模型列表消失，请求按模型不可用返回 404。Google 以后放开时请从这里移除：目录标注了被排除的权益可用时，对应输入框下方会醒目提示；目录没有标注权益限制时，那里只用不醒目的说明列出靠规则挡住的账号，不必因此移除。修改后立即生效，进行中的请求不受影响。',
  'settings.tierRuleHintMarked':
    '{tier} 账号（{count} 个）的目录已标注 {model} 可用，确认 {tier} 能用后可从这里移除',
  'settings.tierRuleHintUnmarked':
    '{model}：{tiers}账号的目录没有标注权益限制，只靠这条规则不被选中。目录没有标注不代表这些账号能用，不必因此移除',
  'settings.tierRuleHintTier': '{tier}（{count} 个）',
  'settings.tierRuleHintTierSeparator': '、',
  'settings.ultraWarmWorkerLimit': 'Ultra 常驻 Worker 数',
  'settings.ultraMaxActiveWorkers': 'Ultra 峰值 Worker 上限',
  'settings.ultraWorkersHelp':
    'Ultra 号池账户的 Worker 与普通号池分开计数：预热、空闲回收与淘汰都只在 Ultra 分区内进行，不会挤占普通号池的 Worker；关闭 Pro 独立号池、开启“Pro 账户并入 Ultra 号池”时，Pro 账户的 Worker 也计入这里（开启 Pro 独立号池时计入上方 Pro 分区）。常驻数可以为 0（只在有请求时启动），峰值至少为 1 且不小于常驻数。',
  'settings.routingStrategy': '账户选择策略',
  'settings.routingRoundRobin': '轮询',
  'settings.routingFillFirst': '账号粘性优先',
  'settings.routingBalanced': '平均调用',
  'settings.routingHelp':
    '轮询依次分配请求；粘性优先持续使用首个可用账户，达到并发上限或不可用时切换。平均调用让高并发的流量尽量平均落在所有账号上：按每个账号今天（太平洋时间，北京时间 15 点或 16 点清零）发出的请求数，运行中的 Worker 之间先给手上没有请求的号，预热、替补与扩容优先启动今天用得少的号；在跑的号比接替它的备用号多发 50 次（且至少已服务 5 分钟）就换下，先启后停：先在后台启动接替的号，旧号接完手上的请求再关，不打断任何请求。代价是浏览器冷启动会增加（默认每个分区每分钟最多换 1 次，峰值不小于 20 时 2 次；出口被限流时暂停）。排空名额只有 1 个的分区（默认的 Ultra 分区、峰值小于 8 的普通分区）只能在号空闲时换，高负载时基本不轮换，要轮换就把该分区的峰值调到 8 或以上。同时在线的号仍只有峰值 Worker 那么多，其余账号轮流上岗。修改后立即生效，改回轮询也立即停止。',
  'settings.waaBackend': 'WAA 后端',
  'settings.waaBackendCamoufox': 'Camoufox 浏览器',
  'settings.waaBackendGo': '纯 Go',
  'settings.waaBackendHelp':
    'Camoufox 在后台浏览器页面中运行 WAA；纯 Go 在服务进程内运行 WAA，不下载也不启动 Camoufox。登录账户始终需要浏览器。切换后重启生成服务生效。',
  'settings.upstreamChannels': '上游通道',
  'settings.upstreamChannelsHelp':
    'Playground 与 Build 是同一账户的两份独立额度，同时启用时按账户与通道组合调度，一个通道冷却后由另一个通道继续；至少启用一个。官网停用 Build 通道的生成（PROXY_GENERATION_DISABLED）时自动暂停 Build、改走 Playground，并定期试探恢复。修改后立即生效。',
  'settings.buildChannelNormal': 'Build 通道生成：正常',
  'settings.buildChannelPaused':
    'Build 通道生成：官网已停用（PROXY_GENERATION_DISABLED），已暂停，生成改走 Playground | 开始 {since} | 下次试探 {next}',
  'settings.buildChannelProbing': '（试探中）',
  'settings.buildChannelAccountsPaused':
    'Build 通道生成：{count} 个账号暂停（官网对它们停用了 Build 生成，改走 Playground），其余账号照常',
  'settings.buildChannelAwaitingMore': '；另有 {count} 个账号暂停已到期，等待试探',
  'settings.buildChannelAwaiting':
    'Build 通道生成：{count} 个账号的暂停已到期，等待试探（它们在 Build 上的下一次请求成功即恢复，再遇到就再暂停）',
  'settings.streamPlaygroundModels': '流式优先 Playground 的模型',
  'settings.streamPlaygroundModelsHelp':
    '需要订阅权益的模型在 Build 通道只能一次性返回整段回复，流式输出会在最后一次性出现。列表中的模型的流式请求先用 Playground（逐块返回）；Playground 账号都在忙时立即改用 Build，不排队等待 Playground。逗号分隔的模型 ID；留空时 Playground 与 Build 照常轮询（默认）。非流式请求不受影响，降级判定拦截的模型按降级判定处理。修改后立即生效。',
  'settings.streamPlaygroundModelsHint':
    '目录中需要订阅权益、可经 Playground 调用的模型（点击加入）：',
  'settings.primaryTitle': '主用模型',
  'settings.primaryModels': '主用模型列表',
  'settings.primaryQueueWait': '排队上限',
  'settings.primaryHelp':
    '这份列表只作用于普通号池（不带 /pro、/ultra 前缀的请求），/pro 与 /ultra 请求按下方的 Ultra/Pro 主用模型列表。列表中的模型的请求最多等这么久空闲的账号或 Worker，超过即返回 429 并带 Retry-After，让客户端稍后重试，而不是在这里排队到请求超时。同时生成数约为 峰值 Worker 上限 × 单账户请求并发，并发超过它时请求就会排队，每分钟生成次数也不超过 Worker 数 × 单账号每分钟请求上限；这只是对症状的处理，长期不够用应增加 Worker 数；单账号并发受每分钟次数上限约束，不宜调高。逗号分隔的模型 ID；留空时所有模型照常排队（默认）。排队上限为 0 表示不排队，没有立即可用的账号就返回 429；上限整个请求共用一份，按实际经过的时间计，换号重试的尝试与退避都算在内（请求自己现场冷启动的浏览器启动时间不计），不能超过请求超时。候选全部冷却仍按原规则处理。修改后立即生效。',
  'settings.ultraPrimaryModels': 'Ultra/Pro 主用模型列表（/ultra 与 /pro）',
  'settings.ultraPrimaryQueueWait': 'Ultra/Pro 排队上限',
  'settings.ultraPrimaryHelp':
    'Ultra/Pro 主用模型列表作用于 /ultra 与 /pro 请求（Pro 号池跳过只限 Ultra 的模型）；关闭“Ultra 号池的账户只服务 /ultra 请求”时，普通路径上不在上方普通列表里、但在这份列表里的模型（通常是只有 Ultra 号池的账户能服务的模型）也按这里的规则。这些模型没有额度时立即返回 429 并带 Retry-After，而不是在服务里一直换号重试：候选账户全部冷却时立即返回；请求中遇到额度 429 后，每日额度用完时最多再换 2 次账户，分钟限额与限流最多再换 5 次，每次额度失败后的那次换号只用已启动的 Worker，不为它现场冷启动备用账户。排队上限只限制等空闲账户或 Worker 的时间，规则与上方相同（0 表示不排队，不能超过请求超时）。逗号分隔的模型 ID，默认 gemini-4-argon；清空即关闭。修改后立即生效。',
  'channel.playground': 'Playground',
  'channel.build': 'Build',
  'logs.channel': '通道',
  'logs.pool': '号池',
  'logs.fallback': '改用',
  'logs.fallbackHelp': '模型 {model} 额度用完，已按 MODEL_FALLBACKS 改用这个模型',
  'settings.modelFallbackTitle': '额度用完自动改用其他模型',
  'settings.modelFallbacks': '改用规则',
  'settings.modelFallbackScope': '生效范围',
  'settings.modelFallbackScopeUltra': '只对 /ultra 请求（不含 /pro）',
  'settings.modelFallbackScopeAll': '默认地址、/pro 与 /ultra',
  'settings.modelFallbackScopeOff': '关闭',
  'settings.modelFallbackHelp':
    '请求的模型在这个号池里没有可用额度（候选账户全部冷却、额度不足立即返回、换号到最后仍是每日或分钟额度 429、账户发出前已冷却），而且还没有向客户端交出任何内容时，按规则改用另一个模型，响应里的模型名写实际使用的模型。每日限额按模型分开计，gemini-4-argon 用完时 gemini-4-carbon 往往还能用。每条规则写成 源模型=目标1|目标2，规则之间用逗号分隔，目标按顺序尝试，不再套用目标自己的规则；模型名写目录 ID（也可以写目录别名），请求的模型名带的后缀与 long- 前缀照常生效。目标要在这个号池的模型目录里、支持请求带的工具，不支持或在降级判定里就跳过。改用后思考等级、输出上限、自动续写与排队规则都按目标模型，历史里别的模型签发的思考签名换成跳过校验的占位值或去掉（目标模型在改用期间自己签发的保留）。号池繁忙排队超限、参数错误、内容拦截、上游 5xx、超时等不改用；指定账户与带文件的请求不改用。所有目标也都不可用时按原模型的 429 返回，Retry-After 取原模型与各目标建议重试中最早的时刻。生效范围：只对 /ultra 请求（默认，不含 /pro）、默认地址、/pro 与 /ultra（要让 /pro 也改用请选这一项）、关闭。默认 gemini-4-argon=gemini-4-carbon；清空规则或选关闭即停用。修改后立即生效。',
  'settings.invalidModelFallbacks':
    '改用规则无效：每条写成 源模型=目标模型（多个目标用 | 分隔，规则之间用逗号分隔），源与目标不能相同，同一个源模型只能写一条，同一条里的目标不能重复，模型名不能为空或包含空白',
  'pool.ultra': 'Ultra',
  'pool.normal': '普通号池',
  'pool.ultraHelp': '经 /ultra 前缀进入，只用 Ultra 号池的账户',
  'pool.pro': 'Pro',
  'pool.proHelp': '经 /pro 前缀进入，只用 Pro 号池的账户',
  'pool.extra': '补充池',
  'pool.extraHelp': '由补充池服务：最后一次尝试借用了补充池的 Worker（号池标记仍按入口）',
  'models.channels': '通道',
  'settings.mediaTitle': '生成媒体输出',
  'settings.mediaHelp':
    'Chat Completions、Messages 与 Gemini 原生接口（generateContent、streamGenerateContent，含 /pro、/ultra 前缀）回复里视频与音频的输出方式；图片（画图模型的输出）总是按官网原样以 Base64 内联返回，不受这里影响；Responses、Interactions、Live 等其他接口不受影响。修改后立即生效。',
  'settings.mediaOutput': '输出方式',
  'settings.mediaOutputInline': '内联 data URI / inlineData（默认）',
  'settings.mediaOutputURL': '本服务链接（保存到 runtime/media/）',
  'settings.mediaOutputHelp':
    '经 new-api 等中转使用、或生成视频时选“本服务链接”：中转把 Gemini 回复转成 OpenAI 格式时会丢掉视频与音频，几 MB 的 data URI 也很难用。链接模式只对视频与音频生效：按内容保存，正文写成 Markdown 链接（视频 [video](…)、音频 [audio](…)），Gemini 原生接口另附 fileData；图片照旧内联。/media/ 链接不校验 API 密钥，拿到链接的人都能打开。',
  'settings.mediaBaseURL': '媒体链接地址',
  'settings.mediaBaseURLHelp':
    '链接使用的服务地址，填用户能访问到的公网地址，例如 https://ai.example.com（可带路径前缀）；经 new-api、nginx 等转发时必须填写，并让反向代理放行 /media/。留空时按每个请求访问的地址（X-Forwarded-Proto、X-Forwarded-Host 或 Host），经中转时多半是内网地址，用户打不开。',
  'settings.mediaRetention': '保存期限',
  'settings.mediaMaxSize': '总量上限',
  'settings.mediaCleanupHelp':
    '后台每小时清理一次 runtime/media/：删除超过保存期限的文件（例如 7d、72h；0 表示不清理，不为 0 时至少 1 小时），总量超过上限时从最旧的文件开始删除（例如 20GB、500MB；0 表示不限，不为 0 时至少 100MB；最近 10 分钟内生成的不删）。只删除以内容哈希命名的媒体文件，删除后对应链接返回 404。',
  'settings.invalidMediaBaseURL':
    '媒体链接地址必须是 http 或 https 地址，不能带账号密码、查询参数或片段',
  'settings.invalidMediaRetention': '媒体保存期限必须是 0 或不少于 1 小时，例如 7d、72h',
  'settings.invalidMediaMaxSize': '媒体总量上限必须是 0 或不小于 100MB，例如 20GB、500MB',
  'settings.omniIgnoreSystemPrompt': '视频模型忽略系统提示',
  'settings.omniIgnoreSystemPromptHelp':
    '发给能输出视频的 Omni 模型（如 gemini-omni-1.1-flash，含带后缀的名称与目录别名）的请求去掉客户端的系统提示（各协议的 system、systemInstruction、instructions、system_instruction 与 system 角色消息），不发给上游，请求日志记一行去掉的字数。默认开启：Chatbox 等聊天客户端每次都自动加 “You are a helpful assistant.” 一类的系统提示，Omni 带上它时常常只回一段文字、说自己无法生成视频。开启时客户端设置的系统提示（如 Chatbox 的角色设定）对 Omni 不生效，要规定视频风格请写进用户消息；开启前已被拒绝过的对话可能仍只回文字，请新建对话再试。关闭后系统提示照常发送（与其他模型相同）；这是全局设置，对所有客户端生效，Chatbox 等客户端又会只回文字。除系统提示外没有其他消息时保留系统提示；文本、图片、Veo、Lyria 等其他模型不受影响。修改后立即生效，只影响新请求。',
  'settings.omniImageBinding': '视频模型带图请求的校验值计入图片',
  'settings.omniImageBindingHelp':
    '发给 Omni 模型（如 gemini-omni-1.1-flash）、带内联图片的请求，把图片按文字模型带图的同一写法（图片原数据的 Base64，按它在请求里的位置）计入 WAA 校验值；Drive 上传的视频等文件不计入，纯文字请求与其他模型不受影响。默认开启：线上带图请求在多个账户上都被官网以 403（没有权限）拒绝、纯文字正常，推断官网算校验值时计入了图片。关闭后只有校验值回到只含文本（与原先相同）；无论开关取值，带图请求被 403 拒绝时都不冷却、不登记账户，最多试 2 个账户后按 400 返回说明（可以去掉图片重试）。修改后立即生效，只影响新请求。',
  'settings.temporaryChat': 'WAA 预热使用临时对话',
  'settings.ignoreClientSeed': '忽略客户端传入的 seed',
  'settings.ignoreClientSeedHelp':
    '客户端固定 seed 时，相同提示词会得到几乎一模一样的回复（重新生成也一样）。开启后不把客户端的 seed 发给上游；请求日志的参数里可以看到客户端是否传了 seed。',
  'settings.repeatPromptNonce': '给请求加不可见随机后缀',
  'settings.repeatPromptNonceHelp':
    '上游对相同、甚至只差几个字的请求会返回完全相同的回复，换 seed、调高温度都没有用。开启后在最后一条用户消息末尾加入不可见的零宽字符，让每次生成都不同。客户端指定了 seed 或温度设为 0（需要可复现）时不加，带工具结果的轮次不改动。',
  'settings.abnormalFinishError': '重新生成用完仍异常结束时返回 502',
  'settings.abnormalFinishErrorHelp':
    '上游以 MALFORMED_FUNCTION_CALL（函数调用格式错误）、UNEXPECTED_TOOL_CALL（意外的工具调用）或 MALFORMED_RESPONSE（响应格式异常）结束、客户端还没收到任何内容时，本服务先同账号重新生成（每请求最多 2 次）。默认关闭：重新生成用完（或不能重新生成，例如请求没有随机后缀）仍这样结束时，照上游结果返回一个空回复，结束原因原样。开启后改为返回 502 与英文说明（写明结束原因与共生成次数），客户端与 new-api 等中转可以按网关错误重试（流式请求的每次重新生成让写出响应头前的等待重新计、合计不超过 50 秒；第一次生成就超过这个等待（默认 10 秒）或合计超过 50 秒才判定时响应头已经提交，只能以流内错误结束，中转一般不会重试）；已经输出过正文、思考或函数调用的回复不受影响，Gemini 原生接口照官方形状保留结束原因。不算账户的问题：不冷却、不换号、不改用其他模型，用量照旧记入用量账本。修改后立即生效，只影响新请求。',
  'settings.minOutputTokens': '最大输出 token 下限',
  'settings.minOutputTokensHelp':
    '客户端设置的最大输出低于这个值时提高到它（不超过模型上限），避免思考把输出额度用完、回复被截断或为空；客户端没设置时沿用模型上限。图片、语音等模型不调整，0 表示不调整。',
  'settings.longOutputEnabled': '模型名带 long- 时自动续写',
  'settings.longOutputEnabledHelp':
    '模型名前加 long-（如 long-gemini-4-argon、long-gemini-4-argon-low）时，输出达到单轮上限后在同一账号上自动续写下一轮，客户端收到的是一条连续的回复。官网支持自动续写的模型照官网分轮续写，其他模型把已输出的正文作为模型消息再发一轮接着写。每轮占一次请求额度，会更慢。只在 Playground 通道进行；关闭后 long- 前缀照常识别，但按普通请求处理。保存后立即生效。',
  'settings.longOutputMaxTokens': '自动续写默认总上限（token）',
  'settings.longOutputMaxTokensHelp':
    '带 long- 的请求没有给出最大输出时，不支持官网续写的模型最多输出这么多 token（正文与思考合计，默认 262144，约为常见单轮上限的 4 倍）；支持官网续写的模型按官网总量（约 100 万）。客户端给出最大输出时以客户端为准。0 表示不模拟续写。轮数约为总上限除以模型单轮上限：单轮 65536 的模型最多约 4 轮，单轮更小的模型轮数更多（一个请求最多 64 轮），每轮占一次请求额度并计入每账号每分钟上限。模拟续写把已输出的正文发回上游，输入上限为 131072 的账号到不了更大的值，超过时按 max_tokens 结束。',
  'settings.invalidLongOutputMaxTokens': '自动续写默认总上限必须是 0 到 1048576 的整数',
  'settings.activeValue': '当前生效',
  'settings.pendingService': '已保存值将在下次生成服务启动时生效',
  'settings.pendingManagement': '监听地址需重启管理进程生效（./start.sh restart 或重新运行 start.bat）',
  'settings.invalidConfigTitle': '以下配置项取值无效，已按默认值生效',
  'settings.invalidConfigEnvironment': '来自进程环境变量，要在启动环境里修改',
  'settings.invalidConfigHint':
    '在本页修改后保存即写入有效值；REQUEST_BODY_LOG 不在本页编辑，保存其他设置时保留 .env 里的原行，要在 .env 里改正；代理（PROXY、PROXY_POOL_*）、WAA_BACKEND、UPSTREAM_CHANNELS、TEMPORARY_CHAT 与 AUTO_START 无效时服务不会启动，不会出现在这里；凭据类配置不显示原值。',
  'settings.saved': '配置已保存',
  'settings.savedService': '配置已保存，在左下角停止并重新启动生成服务后生效',
  'settings.savedManagement': '配置已保存，请重启管理进程（./start.sh restart 或重新运行 start.bat）',
  'settings.savedBoth': '配置已保存；重启生成服务后生效；监听地址或 API 密钥需重启管理进程',
  'settings.reveal': '显示密钥',
  'settings.hide': '隐藏密钥',
  'playground.protocol': '协议',
  'playground.mode': '输出类型',
  'playground.modeText': '文本',
  'playground.modeImage': '图片',
  'playground.modeSpeech': '语音',
  'playground.modeMusic': 'Lyria 音乐',
  'playground.model': '模型',
  'playground.system': '系统指令',
  'playground.prompt': '用户消息',
  'playground.stream': '流式响应',
  'playground.ultraHelp': '请求经 /ultra 前缀发送，只由 Ultra 号池的账户处理',
  'playground.ultraNoModels': '还没有读到 Ultra 号池账户的模型，列表显示全部模型',
  'playground.pool': '号池',
  'playground.poolNormal': '默认',
  'playground.poolPro': 'Pro',
  'playground.poolUltra': 'Ultra',
  'playground.poolNormalHelp':
    '请求经默认地址发送，只由普通号池（Free、Plus）的账户处理（开启补充池时还可能借用补充池里其他号池的成员）；列表只显示它们能用的模型，Pro、Ultra 才能用的模型请选 Pro 或 Ultra',
  'playground.poolNormalSharedHelp':
    '请求经默认地址发送，由普通号池与 Ultra 号池的账户处理（不含 Pro 账户，ULTRA_EXCLUSIVE=false）',
  'playground.poolNormalProOffHelp': '请求经默认地址发送；Pro 独立号池已关闭，Pro 不可选',
  'playground.poolProHelp': '请求经 /pro 前缀发送，只由 Pro 号池的账户处理',
  'playground.poolProNoModels': '还没有读到 Pro 号池账户的模型，列表显示全部模型',
  'playground.reasoning': '思考强度',
  'playground.reasoningOff': '默认',
  'playground.reasoningLow': '低',
  'playground.reasoningMedium': '中',
  'playground.reasoningHigh': '高',
  'playground.reasoningExtraHigh': '超高（模型不支持时按高）',
  'playground.tool': '工具',
  'playground.toolOff': '无',
  'playground.imageSize': '图片尺寸',
  'playground.imageQuality': '图片质量',
  'playground.voice': '声音',
  'playground.reasoningOutput': '思考',
  'playground.toolOutput': '工具调用',
  'playground.clear': '清空对话',
  'playground.send': '发送请求',
  'playground.stop': '停止生成',
  'playground.output': '输出',
  'playground.raw': '原始响应',
  'playground.placeholder': '输入一条消息',
  'playground.systemPlaceholder': '可选系统指令',
  'playground.waiting': '等待请求',
  'playground.elapsed': '耗时',
  'playground.status': 'HTTP 状态',
  'playground.waitingResponse': '等待响应 {seconds}s',
  'playground.generating': '生成中',
  'playground.stopped': '已停止',
  'playground.errorTitle': '请求失败',
  'playground.errorInStream': '已连接后失败',
  'playground.errorCode': '错误码',
  'playground.retryAfter': '约 {seconds} 秒后可重试',
  'playground.retry': '重试',
  'playground.streamIncomplete': '响应流在协议的结束事件之前断开',
  'playground.streamInterrupted': '连接在响应结束之前中断：{reason}',
  'playground.emptyThinkingOnly': '只有思考、没有正文（结束原因 {reason}）',
  'playground.emptyReply': '模型没有返回正文（结束原因 {reason}）',
  'playground.finishUnknown': '未知',
  'state.ready': '就绪',
  'state.busy': '忙碌',
  'state.cooldown': '冷却',
  'state.auth_required': '需要登录',
  'state.unavailable': '不可用',
  'state.disabled': '已停用',
  'state.org_disabled': '组织停用或受限',
  'state.proxy_paused': 'IP 暂停',
  'restriction.org_disabled': '组织已停用',
  'restriction.service_restricted': '服务受限',
  'restriction.age_requirement': '年龄或资格不符',
  'restriction.workspace_disabled': 'Workspace 未开放',
  'restrictionReason.service_restricted': 'Google 已限制该账号使用 AI Studio',
  'restrictionReason.age_requirement': '账号不满足 AI Studio 的年龄或使用资格要求',
  'restrictionReason.workspace_disabled': '账号所属 Google Workspace 没有开放 AI Studio',
  'state.queued': '排队',
  'state.running': '运行中',
  'state.completed': '已完成',
  'state.cancelled': '已取消',
  'state.failed': '失败',
  'app.readyOf': '就绪 {ready} / {total}',
  'app.activeRequests': '活动请求 {count}',
  'pager.total': '共 {count} 条',
  'pager.perPage': '{count} 条/页',
  'pager.prev': '上一页',
  'pager.next': '下一页',
  'pager.pageOf': '第 {page} / {count} 页',
  'accounts.search': '搜索邮箱、代理、权益或错误信息',
  'accounts.filterAll': '全部',
  'accounts.filterProblem': '出问题（403）',
  'accounts.problemHelp':
    '只汇总返回 403 无权限的账户（如需要验证账号），每个账户写明原因；需要登录、组织已停用、不可用的账户各有单独的状态筛选，即使有 403 记录也不计入这里（重新登录或更新认证文件后 403 记录一并清除，上方 403 列表照常列出其中的普通 403）。403 的每个模型带最近一次的时间与次数，只涉及一个模型的归为“403 单个模型无权限”；需要验证账号只在一个模型上的归为“403 单个模型需要验证”（只暂停那个模型），2 个及以上模型或整户暂停仍生效的归为“403 需要验证账号”；不是账户级、超过 {hours} 小时没有再出现、账户也不在 403 冷却里的 403 记录视为陈旧，不计入（上方 403 列表里灰显，可逐行清除）',
  'accounts.problemCount': '{count} 个返回 403 的账户',
  'accounts.problemCopyWithReason': '复制邮箱与原因',
  'accounts.problemCopied': '已复制 {count} 个账户',
  'accounts.problemPermissionDenied': '403 无权限',
  'accounts.problemVerifyRequired': '需要验证账号',
  'accounts.problemKind': '问题类型',
  'accounts.problemKindAll': '全部 403',
  'accounts.problemKindVerify': '403 需要验证账号',
  'accounts.problemKindForbidden': '403 无权限（多个模型）',
  'accounts.problemKindModelForbidden': '403 单个模型无权限',
  'accounts.problemKindVerifyModel': '403 单个模型需要验证',
  'accounts.problemVerifyModel': '单个模型需要验证账号',
  'accounts.problemVerifyModelOrphan':
    '单个模型需要验证账号（{models}，403 记录已清除，该模型仍暂停；到官网验证后点“验证”，或在上方“403 单个模型需要验证”里点“清除”）',
  'accounts.problemVerifyPaused':
    '需要验证账号，整户暂停调度（403 记录已清除；到官网验证后点“验证”，或在上方“需要验证账号”汇总块点“清除记录并解除暂停”）',
  'accounts.problemStaleMore': '；另有 {count} 个陈旧记录不计',
  'accounts.sortDefault': '默认排序',
  'accounts.sortState': '按状态',
  'accounts.sortLabel': '按邮箱',
  'accounts.sortModels': '按模型数',
  'accounts.enableAll': '一键启用',
  'accounts.verifyAll': '一键验证',
  'accounts.enableAllConfirm':
    '启用全部 {count} 个已停用账户？每 500 个一批提交，没有模型目录的账户启用后在后台同步，登录已失效的会显示为需要登录。',
  'accounts.noneDisabled': '没有已停用的账户',
  'accounts.noneToVerify': '没有需要验证的账户',
  'accounts.selected': '已选 {count} 个',
  'accounts.selectFiltered': '选择全部筛选结果（{count}）',
  'accounts.clearSelection': '取消选择',
  'accounts.bulkDisableConfirm': '停用选中的 {count} 个账户？',
  'accounts.bulkDeleteConfirm': '删除选中的 {count} 个账户？账户目录会被移除，无法恢复。',
  'accounts.verifyTitle': '批量验证账户',
  'accounts.verifyScopeProblem': '需要登录或不可用的账户（{count}）',
  'accounts.verifyScopeEnabled': '全部已启用账户（{count}）',
  'accounts.verifyScopeSelection': '选中或当前筛选出的已启用账户（{count}）',
  'accounts.verifyConcurrency': '并发数',
  'accounts.verifyHint':
    '每次验证会启动一个无头 Camoufox 浏览器，约占 300–600 MB 内存。并发越高越快，也越占资源；验证期间请保持此页面打开。',
  'accounts.start': '开始',
  'accounts.progress': '{action} {done}/{total}',
  'accounts.progressSucceeded': '成功 {count}',
  'accounts.progressFailed': '失败 {count}',
  'accounts.progressValid': '有效 {count}',
  'accounts.progressInvalid': '登录失效 {count}',
  'accounts.progressErrored': '出错 {count}',
  'accounts.progressInvalidList': '登录失效明细（{count}）',
  'accounts.progressOrgDisabled': '组织停用或受限 {count}',
  'accounts.progressSkippedPaused': '跳过 {count}（IP 暂停）',
  'accounts.progressSkippedPausedList': '跳过明细：IP 暂停（{count}）',
  'accounts.progressNotPassedList': '未通过明细（{count}）',
  'accounts.progressActive': '处理中：{names}',
  'accounts.progressStop': '停止',
  'accounts.progressStopped': '已停止',
  'accounts.progressDone': '已完成',
  'accounts.progressFailures': '失败明细（{count}）',
  'accounts.bulkBusy': '批量操作进行中，请稍候',
  'accounts.bulkFinished': '{action}完成：成功 {succeeded}，失败 {failed}',
  'accounts.bulkEnableBusy': '账户正忙（正在验证、登录或处理请求），稍后重试',
  'accounts.bulkEnableLocked':
    '另一个进程正在使用这个账户的文件（共用账户目录的另一个实例），请先停掉它再重试',
  'accounts.state': '状态',
  'accounts.account': '账户',
  'accounts.region': '区域 / 时区',
  'accounts.actions': '操作',
  'accounts.noMatch': '没有符合条件的账户',
  'accounts.selectPage': '选择本页',
  'accounts.copyEmails': '复制邮箱',
  'studioKey.title': 'AI Studio API Key',
  'studioKey.fetch': '获取 Key',
  'studioKey.fetchSelected': '获取选中账户的 Key（{count}）',
  'studioKey.idle': '空闲',
  'studioKey.running': '正在处理 {current}，排队 {queued} 个，间隔 {delay} 秒',
  'studioKey.expand': '展开',
  'studioKey.collapse': '收起',
  'studioKey.help':
    '用账户已保存的登录态在后台打开 AI Studio 的 API Keys 页面，从页面自己的请求结果里读取 Key，并且只认与页面 Key 列表里某一行对得上的 Key（页面自己的公共 Key 一律不认；列表只给打码的 Key 时打开这一行的详情读完整 Key）；页面上没有 Key 时点页面自己的“Create API key”创建一个（账户还没有 Google Cloud 项目时先经页面新建一个项目，每次最多建一个项目、一个 Key）。账户逐个处理，每个之后等 {delay} 秒（设置页 STUDIO_KEY_DELAY），处理期间该账户不接新请求；未登录、停用、组织停用的账户跳过，需要验证账号（403）而暂停的账户照常获取、获取后仍暂停。列表只显示末四位，点“复制”才读取完整 Key。与其他账户相同的 Key、浏览器认出的页面公共 Key，以及旧版本保存、没有核对过的 Key 都作废，列在“失败”里，不能复制、不导出，点“全部获取”会重新获取。缺项目编号的 Key 可以点“补项目信息”（只读打开页面，从不创建 Key 或项目）；失败、跳过的记录可以删除（只是从列表里去掉，不删除账户的 Key；作废的 Key 不当作账户的，随记录删掉）。',
  'studioKey.empty': '还没有记录。在账户行点“获取 Key”，或选中账户后批量获取。',
  'studioKey.account': '账户',
  'studioKey.key': 'Key',
  'studioKey.projectName': '项目名',
  'studioKey.projectNumber': '项目编号',
  'studioKey.state': '状态',
  'studioKey.time': '时间',
  'studioKey.created': '（本次新建）',
  'studioKey.projectCreated': '（新建的项目）',
  'studioKey.fetchedAt': '读取',
  'studioKey.createdAt': '创建',
  'studioKey.attemptAt': '最近尝试',
  'studioKey.copy': '复制',
  'studioKey.copied': 'Key 已复制',
  'studioKey.copyFailed': '复制失败，请检查浏览器剪贴板权限',
  'studioKey.queued': '已加入获取队列：{count} 个账户',
  'studioKey.fetchAll': '全部获取',
  'studioKey.fetchAllConfirm':
    '把所有还没有 Key 的账户（含 Key 已作废的）加入获取队列？会一个一个慢慢处理，号与号之间隔 {delay} 秒；没有 Key 的号会自动创建一个（没有项目时先新建一个项目）。中途可以点“停止”，之后再点“全部获取”只补没拿到的。',
  'studioKey.queuedAll': '已开始：所有还没有 Key 的账户逐个获取，进度看上方队列',
  'studioKey.allDone': '所有账户都已经有 Key，没有要获取的',
  'studioKey.stop': '停止',
  'studioKey.stopped': '已停止：清空了排队中的账户，正在处理的那个会做完',
  'studioKey.copyAll': '复制全部 Key（{count}）',
  'studioKey.copiedAll': '已复制 {count} 个 Key，一行一个',
  'studioKey.noKeys': '还没有获取到任何 Key',
  'studioKey.fetchSelectedConfirm':
    '只给选中的 {count} 个账户获取 Key（未选中的不处理），逐个处理、号与号之间隔 {delay} 秒，没有 Key 的会自动创建；可以在 AI Studio API Key 卡片上点“停止”。',
  'studioKey.tab.ok': '成功',
  'studioKey.tab.failed': '失败',
  'studioKey.tab.skipped': '跳过',
  'studioKey.emptyOk': '还没有拿到 Key 的账户',
  'studioKey.emptyFailed': '没有失败的账户',
  'studioKey.emptySkipped': '没有被跳过的账户',
  'studioKey.reason': '原因（点击展开全文）',
  'studioKey.keptPrevious': '保留着上次的 Key',
  'studioKey.retry': '重试',
  'studioKey.retryFailed': '重试失败的（{count}）',
  'studioKey.retryFailedConfirm':
    '把“失败”里的 {count} 个账户重新加入获取队列？逐个处理、号与号之间隔 {delay} 秒，没有 Key 的会自动创建；可以随时点“停止”。',
  'studioKey.copyFailedEmails': '复制失败账户邮箱',
  'studioKey.copiedFailedEmails': '已复制 {count} 个失败账户的邮箱，一行一个',
  'studioKey.status.ok': '成功',
  'studioKey.status.none': '没有 Key',
  'studioKey.status.failed': '失败',
  'studioKey.status.skipped': '已跳过',
  'studioKey.status.invalid': '作废',
  'studioKey.invalidCount': '（含作废 {count}）',
  'studioKey.invalidHint':
    '其中 {count} 条是作废的 Key：与其他账户保存的相同或是页面自己的公共 Key（不是这个账户的），或是旧版本保存、没有与页面的 Key 列表核对过的。它们不能复制、不会导出；点“全部获取”或“重试失败的”会重新获取，也可以清除。',
  'studioKey.fillMark': '（补项目信息）',
  'studioKey.projectIdTitle': '项目 ID：{id}',
  'studioKey.delete': '删除',
  'studioKey.clear': '清除',
  'studioKey.clearTab': '清除这一栏的记录（{count}）',
  'studioKey.clearFailure': '清除失败信息',
  'studioKey.deleteRecordConfirm':
    '删除 {label} 的这条“{tab}”记录？记录只是从列表里消失，不会删除账户的 Key（作废的 Key 不当作账户的，随记录删掉），也不会动 AI Studio 上的任何东西；之后再获取会重新生成记录。',
  'studioKey.clearTabConfirm':
    '清除“{tab}”这一栏的全部 {count} 条记录（包括其他页的）？这些记录只是从列表里消失：账户的 Key 不删除（作废的 Key 不当作账户的，随记录删掉），也不会动 AI Studio 上的任何东西；正在处理的账户不动。之后再获取会重新生成记录。',
  'studioKey.clearFailureConfirm':
    '清除 {label} 最近一次没成功的信息？保存着的 Key 与项目信息原样保留，只是不再显示失败原因（这条仍在“成功”里）。',
  'studioKey.cleared': '已清除：{parts}（账户的 Key 都没动）',
  'studioKey.clearedDeleted': '删掉 {count} 条记录',
  'studioKey.clearedInvalid': '删掉 {count} 条作废的 Key 记录',
  'studioKey.clearedReset': '清掉 {count} 条失败信息',
  'studioKey.clearedJoin': '，',
  'studioKey.clearedNothing': '没有需要清除的记录',
  'studioKey.clearedBusy': '正在处理的 {busy} 个账户没有动',
  'studioKey.clearedUnknown': '{count} 个账户已不在账户列表里',
  'studioKey.clearedErrors': '{count} 个账户的记录读写失败（文件损坏或没有读写权限），没有动',
  'studioKey.clearedSentence': '；',
  'studioKey.fillProjects': '补项目信息（{count}）',
  'studioKey.fillProjectsAction': '补项目信息',
  'studioKey.fillProjectsConfirm':
    '给 {count} 个缺项目编号的 Key 补项目名与项目编号？只读打开 AI Studio 的 API Keys 页面，找到已保存的那条 Key 读它的项目（必要时点开这条 Key 的详情读完再关掉），从不点创建、从不新建 Key 或项目；页面上找不到这条 Key 时记为失败，Key 照样保留。逐个处理、号与号之间隔 {delay} 秒，可以随时点“停止”。',
  'studioKey.fillQueued': '已开始补项目信息：{count} 个账户逐个处理，进度看上方队列',
  'studioKey.fillNone': '没有缺项目编号的 Key',
  'settings.studioKeyDelay': '获取 API Key 的间隔（秒）',
  'settings.studioKeyDelayHelp':
    '批量获取 AI Studio API Key 时，相邻两个账户之间等待的秒数（STUDIO_KEY_DELAY，默认 30，0 到 3600）。保存后立即生效。',
  'accounts.copiedEmails': '已复制 {count} 个邮箱',
  'accounts.permissionDeniedTitle': '403 无权限账户',
  'accounts.permissionDeniedHint': '（多半未同意 Google 政策，重新获取认证文件后上传即可）',
  'accounts.permissionDeniedCount': '{count} 个',
  'accounts.permissionDeniedHelp':
    '生成请求返回 HTTP 403 PERMISSION_DENIED（The caller does not have permission）的账户，每个模型记下最近一次的时间、次数与请求来源。以下情况清除记录：认证文件更新（上传新的 storage-state.json、浏览器登录或重新导入目录，同时解除它的 403 冷却）；该模型在该通道上成功一次（只清这个模型这个通道）；需要验证账号的账户通过验证（2 个及以上模型要求验证时任一模型成功，只有一个模型时这个模型本身成功）；管理页“验证”通过；启动时对账（账户已不在号池，或登录签名变了）；号池级 403 撤销；每行的“清除”与“清除记录”（只清这张表里列出的账户，不解除单模型 403 冷却）。超过 {hours} 小时没有再出现、账户也不在 403 冷却里的记录灰显为陈旧，多半是很早以前或别人（new-api 渠道测试、下游用户、模型别名、共用账户目录的另一个实例）调用留下的。号池级 403 不计入。需要验证账号（账户级 403：2 个及以上模型时整户暂停调度，只有一个模型时只暂停那个模型，都最长 30 天）的账户不列在这里，见下方“需要验证账号”汇总块与“403 单个模型需要验证”。',
  'accounts.permissionDeniedEmail': '邮箱',
  'accounts.permissionDeniedModels': '模型 · 通道',
  'accounts.permissionDeniedLast': '最近一次',
  'accounts.permissionDeniedTimes': '次数',
  'accounts.copyAllEmails': '复制全部邮箱',
  'accounts.permissionDeniedDownload': '下载列表',
  'accounts.permissionDeniedClear': '清除记录',
  'accounts.permissionDeniedClearConfirm':
    '清除列表中 {count} 个账户的 403 记录？不解除单模型 403 冷却与多模型 403 自动暂停；账户再次返回 403 时会重新列出。',
  'accounts.permissionDeniedClearKeepVerify':
    '不含需要验证账号的 {count} 个账户（整户与单个模型）：它们的 403 记录与暂停不动（见“需要验证账号”汇总块与“403 单个模型需要验证”）。',
  'accounts.permissionDeniedClearPartial':
    '已清除 {cleared} 个账户的 403 记录、解除 {lifted} 个账户的整户暂停后出错：{error}',
  'accounts.permissionDeniedCleared': '已清除 {count} 个账户的记录',
  'accounts.permissionDeniedLiftedMore':
    '；解除 {count} 个账户需要验证账号的暂停（整户或单个模型）',
  'accounts.permissionDeniedDelete': '一键删除',
  'accounts.permissionDeniedDeleteHelp':
    '从号池删除列表中的全部账户（包括折叠未显示的）并删除它们的认证目录，与账户列表里的“删除”相同，不能恢复。',
  'accounts.permissionDeniedDeleteConfirm':
    '删除列表中的全部 {count} 个账户及其认证文件（账户目录会被移除）？删除后不能恢复，要重新上传认证文件才能再用；正在处理请求的账户会等它空闲后删除。',
  'accounts.permissionDeniedDeleteConfirmFresh':
    '删除列表中 403 记录不陈旧的 {count} 个账户及其认证文件（账户目录会被移除）？另有 {stale} 个账户的 403 记录全部陈旧（灰显，多半没有问题），不删除，可以按行清除。删除后不能恢复，要重新上传认证文件才能再用；正在处理请求的账户会等它空闲后删除。',
  'accounts.permissionDeniedDeleteStaleHelp':
    '403 记录全部陈旧的账户（灰显，多半没有问题）不删除。',
  'accounts.permissionDeniedDeleteStaleKept': '；{count} 个 403 记录全部陈旧的账户没有删除',
  'accounts.permissionDeniedDeleteAllStale':
    '列表里 {count} 个账户的 403 记录全部陈旧（多半没有问题），一键删除不删除它们；可以按行清除记录，确实要删除时到账户列表里逐个删除',
  'accounts.permissionDeniedDeleteResult':
    '已删除 {deleted} 个，不存在 {missing} 个，失败 {failed} 个',
  'accounts.permissionDeniedDeleteFailures': '；失败：{list}',
  'accounts.permissionDeniedDeleteMore': '等 {count} 个，完整列表见“失败明细”',
  'accounts.permissionDeniedDeleteRemaining': '；另有 {count} 个本次没有处理，再点一次继续',
  'accounts.permissionDeniedDeleteBusy': '账户一直在处理请求，等不到空闲，稍后再点一次',
  'accounts.permissionDeniedDeleteSkipped':
    '；{count} 个在确认后已不在列表里（例如重新上传了认证文件），没有删除',
  'accounts.permissionDeniedShowAll': '展开全部 {count} 个',
  'accounts.permissionDeniedSeen': '最近 {time} · {count} 次',
  'accounts.permissionDeniedLastRequest': '最近一次请求：{detail}',
  'accounts.permissionDeniedRequestModel': '请求名 {model}',
  'accounts.permissionDeniedRequestMessages': '{count} 条消息',
  'accounts.permissionDeniedRequestImages': '图片 {count}',
  'accounts.permissionDeniedRequestFiles': '文件 {count}',
  'accounts.permissionDeniedStale': '陈旧（{days} 天前）',
  'accounts.permissionDeniedStaleShort': '陈旧',
  'accounts.permissionDeniedStaleCount': '其中 {count} 个陈旧',
  'accounts.permissionDeniedStaleHelp':
    '超过 {hours} 小时没有再出现、账户也不在 403 冷却里，多半是很早以前或别人调用留下的，可以清除；不计入“出问题（403）”',
  'accounts.permissionDeniedClearRow': '清除',
  'accounts.permissionDeniedClearRowHelp': '只清除这个账户的 403 记录，不解除单模型 403 冷却',
  'accounts.permissionDeniedRowCleared': '已清除 {label} 的 403 记录',
  'accounts.permissionDeniedRowMissing': '{label} 的 403 记录已经不在（可能刚被清除）',
  'accounts.verifyRequiredTitle': '需要验证账号 {count} 个：',
  'accounts.verifyRequiredState':
    '整户暂停调度中（不占 Worker、不预热），不计入 403 无权限账户列表与模型冷却。',
  'accounts.verifyRequiredStateLatest':
    '整户暂停调度中（不占 Worker、不预热），不计入 403 无权限账户列表与模型冷却；最近一次 {time}。',
  'accounts.verifyRequiredStatePartial':
    '{paused} 个整户暂停调度中（不占 Worker、不预热）、{expired} 个暂停已到期；都不计入 403 列表，模型冷却只不计暂停中的。',
  'accounts.verifyRequiredStatePartialLatest':
    '{paused} 个整户暂停调度中（不占 Worker、不预热）、{expired} 个暂停已到期；都不计入 403 列表，模型冷却只不计暂停中的；最近一次 {time}。',
  'accounts.verifyRequiredAdvice': '到官网验证后在管理页点“验证”；不处理的话暂停 30 天后到期再试。',
  'accounts.verifyRequiredHelp':
    '返回 403 “Your account must be verified … verify-account”（账户级）的账户，以及需要验证账号整户暂停仍生效的账户，与“出问题（403）→ 403 需要验证账号”同一口径。在 2 个及以上模型上返回过，或整户暂停仍生效时整户暂停调度 30 天：不参与调度、不占 Worker、不预热，所以不列在 403 无权限账户列表里，模型冷却也不计它们；只在一个模型上要求验证的只暂停那个模型，另见“403 单个模型需要验证”。暂停从第一次 403 起算 30 天，到期后（或旧版本留下、已超过 30 天的登记）账户重新参与调度，登记还在、仍计在这里，状态说明分开写暂停中与已到期的个数；仍未验证的下一次请求会再返回 403 并重新暂停。需要登录、组织已停用、不可用的账户不计入这里（与“出问题（403）”一致），它们各有单独的状态筛选。到 https://aistudio.google.com/verify-account 验证后在账户列表点“验证”即可解除；“清除记录并解除暂停”清除这些账户的 403 记录并解除整户暂停，没在官网验证的账户下一次请求会再返回 403 并重新暂停。',
  'accounts.verifyRequiredView': '在账户列表查看',
  'accounts.verifyRequiredLift': '清除记录并解除暂停',
  'accounts.verifyRequiredLiftConfirm':
    '清除这 {count} 个需要验证账号的账户的 403 记录并解除整户暂停？它们重新参与调度；没在官网验证的账户下一次请求会再返回 403 并重新暂停（那次请求换号重试）。单模型 403 冷却不变。',
  'accounts.verifyRequiredLifted':
    '已清除 {cleared} 个账户的 403 记录，解除 {lifted} 个账户需要验证账号的整户暂停',
  'accounts.verifyModelTitle': '403 单个模型需要验证 {count} 个：',
  'accounts.verifyModelHint':
    '只暂停所列模型（最长 30 天），其他模型照常调度、Worker 不释放；到官网 https://aistudio.google.com/verify-account 验证后点“验证”，或点“清除”解除该模型的暂停。',
  'accounts.verifyModelHelp':
    '只在一个模型上返回 403 “Your account must be verified … verify-account” 的账户（与“出问题（403）→ 403 单个模型需要验证”同一口径，需要登录、组织已停用、不可用的不计）。只暂停这个模型的 Playground 与 Build（最长 30 天），账户其他模型照常调度，Worker 不释放、不淘汰，账户行照常显示就绪或忙碌，模型冷却概况不计这条暂停。其他模型成功不解除它；在官网验证后点“验证”、更新认证文件或“清除”都会解除，没在官网验证的话下一次请求这个模型时会再返回 403 并重新暂停。同一账户在第二个模型上也要求验证时改为整户暂停，移到上方“需要验证账号”汇总块。',
  'accounts.verifyModelColumn': '需要验证的模型',
  'accounts.verifyModelPauseColumn': '该模型的暂停',
  'accounts.verifyModelPaused': '暂停中（最迟 {time}）',
  'accounts.verifyModelPauseExpired': '未生效（该模型照常调度）',
  'accounts.verifyModelPauseExpiredHelp':
    '这个模型的暂停已到期，或生成路径没有写入暂停（目录在这期间变过）：该模型照常调度，仍未验证的下一次请求会再返回 403 并重新暂停',
  'accounts.verifyModelState': '{paused} 个该模型暂停中。',
  'accounts.verifyModelStatePartial':
    '{paused} 个该模型暂停中、{expired} 个暂停未生效（已到期或没有写入，该模型照常调度，仍未验证的下一次请求会再返回 403 并重新暂停）。',
  'accounts.verifyModelNoRecord': '403 记录已清除',
  'accounts.verifyModelVerifyHelp':
    '用隔离浏览器检查这个账户的登录是否有效：有效即清除它的 403 记录并解除该模型的暂停（不检查是否已在官网验证）；没在官网验证的话，下一次请求这个模型时会再返回 403 并重新暂停',
  'accounts.verifyModelClearRowHelp':
    '清除这个账户的 403 记录并解除该模型的暂停；没在官网验证的话，下一次请求这个模型时会再返回 403 并重新暂停',
  'accounts.verifyModelRowCleared': '已清除 {label} 的 403 记录',
  'accounts.orgDisabledTitle': '组织停用或受限、用不了 AI Studio 的账户',
  'accounts.orgDisabledHint':
    '（账号所属组织关闭了 AI Studio，或 Google 限制了账号、年龄或资格不符、Workspace 未开放，重新登录也没用；已停止调度，换号或等对方重新开放）',
  'accounts.orgDisabledHelp':
    '载入 AI Studio 被跳到 access.workspace.google.com 的 ServiceNotAllowed 页，或接口返回 HTTP 403 “AI Studio app is disabled” 的账户（Google Workspace 账号所属组织或 Google 为它关闭了 AI Studio）；被跳到 accounts.google.com/info/servicerestricted（Google 限制了该账号）、AI Studio 的年龄要求说明页或接口返回 “Account not supported.”（年龄或使用资格不符）、Workspace 说明页或接口返回 “Early Access Apps disabled.”（所属 Workspace 没有开放 AI Studio）的账户同样列在这里，“原因”一栏区分。被跳到地区说明页或接口返回 “Region not supported.” 是出口地区的问题，不会列在这里。这些账户不再参与预热、替补、扩容与选号，驻留 Worker 空闲即关；模型目录每 24 小时复查一次，读取成功时自动恢复。认证文件更新（上传新的 storage-state.json、浏览器登录或重新导入目录）或点“恢复调度”也会立即恢复，仍被拒绝时会重新列出。',
  'accounts.orgDisabledReason': '原因',
  'accounts.orgDisabledSource': '来源',
  'accounts.orgDisabledNextCheck': '下次复查',
  'accounts.orgDisabledClear': '恢复调度',
  'accounts.orgDisabledClearConfirm':
    '清除全部 {count} 个账户的组织停用或受限状态、立即恢复调度？仍被拒绝的账户再次启动或同步时会重新列出（启动时立即失败，不再干等）。',
  'accounts.orgDisabledCleared': '已恢复 {count} 个账户的调度',
  'accounts.orgDisabledSourceStartup': '启动',
  'accounts.orgDisabledSourceCatalog': '模型目录',
  'accounts.orgDisabledSourceVerify': '验证',
  'accounts.orgDisabledSourceLogin': '登录',
  'accounts.orgDisabledSourceRequest': '请求',
  'accounts.orgDisabledSourceRecheck': '登录复核',
  'models.allChannels': '全部通道',
  'models.pricingAll': '免费与付费',
  'models.pricingFree': '仅免费',
  'models.pricingPaid': '仅付费',
  'models.expandPage': '展开本页',
  'models.collapseAll': '全部收起',
  'models.description': '说明',
  'models.options': '能力选项',
  'logs.compact': '精简',
  'logs.compactHint': '隐藏 Worker 启动等过程日志，并把一分钟内连续出现的同类信息合并为一行',
  'logs.extraFilter': '只看补充池',
  'logs.extraFilterHelp':
    '只看由补充池服务的请求与补充池的运行日志（成员变化、预热、接手、归还、释放）',
  'logs.shown': '显示 {shown} / {total}',
  'logs.jumpLatest': '跳到最新',
  'logs.detailTitle': '日志详情',
  'logs.time': '时间',
  'logs.message': '消息',
  'logs.event': '事件',
  'requests.filterAll': '全部',
  'requests.filterExtra': '补充池',
  'requests.filterExtraHelp': '只看由补充池服务的请求（这次或最后一次尝试借用了补充池的 Worker）',
  'cooldowns.search': '搜索模型',
  'cooldowns.reason': '原因',
  'app.workers': 'Worker {warm} / {target}',
  'app.workersStarting': '启动中 {count}',
  'app.workerCapacity': '槽位 {occupied} / 峰值 {max}',
  'app.workersDead': '冷却占位 {count} · 待淘汰',
  'app.workersDeadDetail':
    '账号已长时间冷却（如每日限额，或主用、热门模型的当天额度已用完）的常驻 Worker：占着名额却服务不了这些请求。空闲时立即关闭，忙碌时先排空再关闭，并换上没有冷却的账号',
  'app.workersDraining': '排空中 {count}',
  'app.workersDrainingDetail':
    '冷却占位的 Worker 不再接新请求，正在服务的请求结束后关闭（开始输出的长流会让排空保持到流结束）；有空槽时替补已先启动，否则旧 Worker 关闭后补上',
  'app.workersUnschedulable': '不可调度 {count}',
  'app.workersUnschedulableDetail':
    '驻留 Worker 的账号需要登录、已停用、没有模型目录或需要验证账号（整户暂停中），已不能服务任何模型：空闲后立即释放，槽位由预热补上',
  'app.primarySupplyModel': '主用 {model}：',
  'app.primarySupply': '热 {warm} · 冷却 {cooling} · 备用 {standby}',
  'app.primarySupplyDetail':
    '主用模型 {model}：热为能服务它的常驻 Worker，冷却为对它冷却中的常驻 Worker（含 2 分钟内到期的短冷却，冷却到期前服务不了它），备用为还能服务它的空闲账号（最多统计 1000 个），约每 30 秒更新。备用为 0 表示没有还能服务该模型的空闲账号（当天额度可能已用完，太平洋时间零点重置）',
  'app.ultraPrimarySupplyModel': 'Ultra 主用 {model}：',
  'app.ultraPrimarySupplyDetail':
    'Ultra 主用模型 {model}：热为 Ultra 分区能服务它的常驻 Worker，冷却为 Ultra 分区对它冷却中的常驻 Worker（含 2 分钟内到期的短冷却，冷却到期前服务不了它），备用为 Ultra 号池里还能服务它的空闲账号（关闭 Pro 独立号池、开启“Pro 账户并入 Ultra 号池”时含 Pro 账户，最多统计 1000 个），约每 30 秒更新。备用为 0 表示 Ultra 号池里没有还能服务该模型的空闲账号（当天额度可能已用完，太平洋时间零点重置）；额度不足时该模型的请求立即返回 429，不在服务里排队重试',
  'app.proPrimarySupplyModel': 'Pro 主用 {model}：',
  'app.proPrimarySupplyDetail':
    'Ultra/Pro 主用模型 {model} 在 Pro 分区的供给：热为 Pro 分区能服务它的常驻 Worker，冷却为 Pro 分区对它冷却中的常驻 Worker（含 2 分钟内到期的短冷却），备用为 Pro 号池里还能服务它的空闲账号（最多统计 1000 个），约每 30 秒更新。备用为 0 表示 Pro 号池里没有还能服务该模型的空闲账号（当天额度可能已用完，太平洋时间零点重置）；额度不足时 /pro 上该模型的请求立即返回 429，不借用 Ultra 号池',
  'app.prewarmActive': '预热中 · 进行 {inflight} · 本轮 {launched}',
  'app.prewarmStalled': '预热循环 {seconds}s 无进展',
  'app.prewarmIdle': '预热空闲',
  'app.stepPrepareCatalog': '准备模型目录',
  'app.stepWaitCatalog': '等待首个账户读取模型目录',
  'app.stepStartPrewarm': '开始后台预热',
  'app.stepCancelRequests': '停止在途请求',
  'app.stepWaitCatalogStop': '等待模型目录同步停止',
  'app.stepCloseWorkers': '关闭 WAA Worker',
  'app.stepFlushWrites': '写出暂存记录',
  'app.stepPrepareCatalogDetail':
    '对照账户池排好模型目录任务，从写盘缓存恢复目录，开始后台目录同步与登录复核',
  'app.stepWaitCatalogDetail':
    '没有模型目录缓存（首次启动或缓存损坏）时，要等第一个账户读到模型目录才能进入运行，在此之前还不能处理生成请求；其余账户在后台继续同步',
  'app.stepStartPrewarmDetail': '首轮预热在后台进行，不等它的结果，服务随即进入运行',
  'app.stepCancelRequestsDetail': '中止进行中的请求与后台扩容中的 Worker 启动',
  'app.stepWaitCatalogStopDetail': '模型目录同步退出前把缓存写盘，最多等 2 秒',
  'app.stepCloseWorkersDetail': '等进行中的预热结束，关闭全部 WAA Worker 的浏览器',
  'app.stepFlushWritesDetail':
    '写出暂存的认证 Cookie 与推迟的模型资格时间；停止运行中的服务时另写出坏号记忆、Drive 授权状态与今日发送数',
  'app.stepClockNote': '（已用时间按服务端时钟计算）',
  'app.elapsedSeconds': '{seconds} 秒',
  'app.elapsedMinutes': '{minutes} 分 {seconds} 秒',
  'app.firstWarm': '首轮预热中',
  'app.firstWarmPools': '等首个 Worker：{pools}',
  'app.firstWarmDetail':
    '生成服务已在运行、接收请求，首轮预热在后台进行。列出的分区还没有就绪的 Worker：期间到达、不指定账户的请求排队等该分区首轮预热的第一个 Worker，不另起冷启动；该分区没有可启动的账户时立即放行，等待受请求期限约束。有账户启动失败时预热换下一个候选继续（日志逐个写“WAA Worker 启动失败”），所有候选都试过仍没有 Worker 时才写一行 WARN“WAA Worker 首轮预热失败”，服务保持运行，之后每 5 秒自动补齐',
  'app.partitionNormal': '普通',
  'app.ultraWorkers': 'Ultra Worker {occupied} / {max}',
  'app.ultraWorkersDetail':
    'Ultra 分区：运行中 {warm} · 启动中 {starting} · 冷却占位 {dead} · 排空中 {draining} · 不可调度 {unschedulable} · 预热目标 {target} · 峰值 {max}',
  'app.proWorkers': 'Pro Worker {occupied} / {max}',
  'app.proWorkersDetail':
    'Pro 分区：运行中 {warm} · 启动中 {starting} · 冷却占位 {dead} · 排空中 {draining} · 不可调度 {unschedulable} · 预热目标 {target} · 峰值 {max}',
  'app.extraWorkers':
    '补充池 Worker {workers} / {warm} · 峰值 {max} · 可借出 {lendable} · 成员 {members}',
  'app.extraWorkersOff': '补充池已关闭 · 归还中 {workers}',
  'app.extraWorkersDetail':
    '补充池（{state}）：方式 {mode} · 含 Ultra {ultra} · 默认地址借付费模型 {paid} · 运行中 {running} · 启动中 {starting} · 预热目标 {target} · 冷成员 {cold} · 成员 Free {free} / Pro {pro} / Ultra {ultraMembers} · 归还中 {returning} · 本进程借出 {borrows} 次',
  'app.extraStateOn': '已开启',
  'app.extraStateOff': '已关闭，等待归还',
  'app.extraModeShare': '一起分担',
  'app.extraModeOverflow': '不够时才补',
  'app.extraYes': '是',
  'app.extraNo': '否',
  'app.proPoolNoticeTitle': 'Pro 独立号池已默认开启',
  'app.proPoolNotice': '{count} 个 Pro 账户只服务 /pro。建议 Worker：Pro {proWarm}/{proMax}，普通 {warm}/{max}。',
  'app.proPoolNoticeUltra': '{count} 个 Pro 账户只服务 /pro。建议 Worker：Pro {proWarm}/{proMax}，Ultra {warm}/{max}。',
  'app.proPoolNoticeDetail':
    '.env 与环境变量都没有写 PRO_POOL，开关按默认开启：{count} 个 Pro 账户只服务 /pro 前缀（Pro 可用账户 {proAvailable}，普通可用账户 {normalAvailable}）。当前 PRO_WARM_WORKER_LIMIT/PRO_MAX_ACTIVE_WORKERS={currentProWarm}/{currentProMax}，建议按 Pro 可用账户的占比分出 {proWarm}/{proMax}（不少于默认值）；WARM_WORKER_LIMIT/MAX_ACTIVE_WORKERS={currentWarm}/{currentMax} 可减去分走的份额，为 {warm}/{max}（不少于默认值与启动预热并发）。客户端要用 Pro 账户请改用 /pro 前缀；要保持升级前的行为请关闭“Pro 独立号池”。在设置页改动 Pro 号池的设置（开关或两个 Worker 数）并保存后 .env 写明 PRO_POOL，不再提示；只保存其他设置不影响',
  'app.proPoolNoticeDetailUltra':
    '.env 与环境变量都没有写 PRO_POOL，开关按默认开启：{count} 个 Pro 账户只服务 /pro 前缀，ULTRA_INCLUDE_PRO=true 不再生效（升级前它们在 Ultra 分区，用 ULTRA_* 的容量；Pro 可用账户 {proAvailable}，Ultra 可用账户 {ultraAvailable}）。当前 PRO_WARM_WORKER_LIMIT/PRO_MAX_ACTIVE_WORKERS={currentProWarm}/{currentProMax}，建议按 Pro 可用账户的占比分出 {proWarm}/{proMax}（不少于默认值）；ULTRA_WARM_WORKER_LIMIT/ULTRA_MAX_ACTIVE_WORKERS={currentWarm}/{currentMax} 可减去分走的份额，为 {warm}/{max}（不少于默认值），普通分区不变。客户端要用 Pro 账户请改用 /pro 前缀；要保持升级前的行为请关闭“Pro 独立号池”。在设置页改动 Pro 号池的设置（开关或两个 Worker 数）并保存后 .env 写明 PRO_POOL，不再提示；只保存其他设置不影响',
  'app.proPoolNoticeDismiss': '关闭提示',
  'app.proPoolNoticeOpen': '去设置',
  'app.authRecheckLine': '登录复核 · 疑似失效 {suspects} · 10分钟新增需要登录 {recent} · 坏号 {bad}',
  'app.balancedTitle': '平均调用',
  'app.balancedUltraTitle': 'Ultra 平均调用',
  'app.balancedProTitle': 'Pro 平均调用',
  'app.balancedSpread': '今日 最少/中位/最多 {min}/{median}/{max}',
  'app.balancedUsed': '已用 {used}/{accounts} 个号',
  'app.balancedRotations': '近 1 小时轮换 {count} 次',
  'app.balancedRotating': '轮换中 {count}',
  'app.balancedPaused': '暂停：{reason}',
  'app.balancedDetail':
    '平均调用（ROUTING_STRATEGY=balanced）：今日（太平洋时间 {day}，北京时间 15 点或 16 点清零）每个可调度账号发往上游的生成请求数，最少 {min} · 中位 {median} · p90 {p90} · 最多 {max}；已用为今天发过请求的账号数 / 可调度账号数（需要验证账号的整户暂停、需要登录等不算）。在跑的号比接替它的备用号多发 {gap} 次就换下（先启后停，不打断请求）；近 1 小时与本额度日的轮换次数为两个分区合计，本额度日已轮换 {today} 次。每 30 秒统计一次，只统计本服务。每个账号的明细见用量页“分项排行”里按“账户”排的一栏：那边按服务器本地日、每个请求记一行，这里按太平洋日、每次发送都计（含换号重试、续写的每一轮与重新生成），两边的数不一样',
  'app.driveLine': 'Drive 授权 · 可用 {available} · 不可用 {unavailable} · 未知 {unknown}',
  'app.driveLineStorageFull': 'Drive 空间已满 · {storageFull}',
  'app.driveDetail':
    '启用账户的 Google Drive 授权状态：可用为换到过 Drive token 或经本服务登录时完成了 Drive 授权，不可用为没有 Drive 授权（多半是只导入 Cookie 的账户），空间已满为有 Drive 授权、但上传时 Drive 返回存储空间已满（storageQuotaExceeded，有这类账户时另起一行显示），未知为还没试过。没有 Drive 授权或空间已满的账户，普通模型的附件直接内联发送，不冷却、不换号。gemini-omni 的视频输入（及不能内联的图片）要先上传到账户的 Drive，只交给可用或未知的账户，只剩不可用的账户时返回 400；PNG、JPEG、WebP、HEIC、HEIF、BMP、TIFF 图片按官网直接内联发送（GIF 转为 PNG），不需要 Drive，任何账户都能处理。经本服务的登录流程登录（账户页“登录”或“添加账户”）会完成官网的 Drive 授权，随即记为可用。可用、不可用、空间已满各记 24 小时，写在 runtime/model-catalogs/drive-accounts.json，重启后沿用；换到 Drive token 不清除空间已满、上传成功时清除，更新认证文件后重新探测，管理验证通过时清除不可用与空间已满的记录',
  'app.authRecheckDetail':
    '上游 401 先记疑似登录失效、冷却 5 分钟，用磁盘 Cookie 复核仍是 401 才标需要登录；按接口判定需要登录的账户每 15 分钟低速复核一轮，通过即恢复；浏览器判定未登录的（管理验证判定未登录、Worker 启动时跳到登录页）不复核，需要重新登录或更新认证文件。最近一小时：排除 {cleared}、确认 {confirmed}、恢复 {recovered}、仍失效 {invalid}。坏号为连续裸限流的账号，2 小时内不参与预热与替补',
  'app.catalogSyncPaused': '目录同步暂停到 {time}（{reason}）',
  'app.catalogPauseUpstream': '上游 429',
  'app.catalogPauseEgress': '出口限流',
  'app.catalogSyncPending': '目录待同步 {count} · 疑似失效待探测 {suspects}',
  'app.catalogSyncDetail':
    '模型目录保存在 runtime/model-catalogs，重启直接载入；缺失或超过 24 小时的账户在后台低速刷新（只刷空闲账户），遇到上游 429 或出口限流时暂停',
  'app.egressLimited':
    '出口限速 {label} · 上游拒绝 {ratio}% · 本地限速拒绝 {local}/分钟 · 排队 {queued} · 限速 {limit}',
  'app.egressLimitRPM': '{limit}/分钟',
  'app.egressLimitUltra': 'Ultra {limit}/分钟',
  'app.egressLimitLanes': '普通 {normal}/分钟 · Ultra {ultra}/分钟',
  'app.egressLimitOff': '未启用',
  'app.egressSuspended': '出口限流 {label} · 限速无效已停止 · 上游拒绝 {ratio}% · 本地拒绝 {local}/分钟',
  'app.egressLimitedDetail':
    '经同一出口的多个账号同时被上游限流（新换上的账号第一次请求就被拒）：已停止逐级冷却与按需冷启动，按出口限速发送，主用模型优先。“上游拒绝”是发往上游的请求被拒的比例；“本地限速拒绝”是排队超过上限、在本地直接返回的 429。限流解除后自动恢复。给账号设置不同代理或开启 IP 池（增加出口）才能提高上限',
  'app.egressRejected': '出口 {label} 判定已撤销（{by}）',
  'app.egressRejectedDetail':
    '被拒的账号再试都不成功（更像账号额度用完），或已手动撤销：已交回逐级冷却，{minutes} 分钟内不再判定',
  'app.egressRejectedByValve': '安全阀',
  'app.egressRejectedByAdmin': '管理操作',
  'app.egressDirect': '直连',
  'app.egressWithdraw': '撤销判定',
  'app.egressResume': '恢复判定',
  'onboard.title': '新账户自动处理',
  'onboard.off': '已关闭',
  'onboard.idle': '队列为空',
  'onboard.pending': '待处理 {count}',
  'onboard.processing': '处理中 {count}',
  'onboard.nextSoon': '即将开始',
  'onboard.nextIn': '约 {seconds} 秒后开始',
  'onboard.settings': '设置与记录',
  'onboard.collapse': '收起',
  'onboard.autoVerify':
    '自动验证新账户与重新上传认证文件的已有账户（启动浏览器验证登录；账户默认启用，验证未通过的显示为需要登录，仍为停用的账户在停用状态下验证）',
  'onboard.autoEnable': '自动启用仍为停用的账户（同时开启验证时，只启用验证通过的账户）',
  'onboard.batchSize': '每批数量',
  'onboard.concurrency': '验证并发',
  'onboard.hint':
    '新目录放进 auth/ 后约 10 秒自动导入并排队；待处理达到每批数量，或最早的账户已等待 2 分钟，就开始处理。验证会启动浏览器，每个账户约 10～30 秒。首次开启时已有账户不做改动；管理页手动添加的账户不自动处理；删除后重新上传的账户会再次处理。已有账户重新上传认证文件（覆盖 storage-state.json，或 Chrome 重新导入）后，清除这个账户原来的需要登录、403 无权限、需要验证账号、单个模型需要验证、组织停用或受限、疑似登录失效等判定，关闭旧浏览器、重新认证（载入后即恢复调度）并立即加入队列验证一次，结果在下面“最近处理”里写“重新上传后：…”；处于这些状态的账户即使上传的内容与原来完全相同（例如在官网解开 403 后原样上传）也照样处理，只有本来就正常的账户内容没变时才只提示“认证文件未变化”；手动停用的只换认证、保持停用；关闭自动验证时只重新载入。服务正常停止期间重新上传的，启动时同样处理。批量上传时每轮扫描最多载入 40 个，没载入的计入“等待载入”。原样重新上传时不要保留原文件的修改时间（WinSCP“保留时间戳”、rsync -a、scp -p），否则看不出文件被重新写入；只差 Google 轮换 Cookie 的重新导出按其他进程回写处理，这两种情况计入“未按重新上传处理”（日志有提示），都可以对账户点“验证”。',
  'onboard.reauth': '其中重新上传 {count}',
  'onboard.reauthTip': '已有账户重新上传认证文件后加入的重新验证（已计入待处理与处理中）',
  'onboard.sourceReauthTip': '已有账户重新上传认证文件后的处理结果',
  'onboard.loading': '等待载入 {count}',
  'onboard.loadingTip':
    '账户目录扫描已检测到认证文件更新、还没载入的账户：每轮扫描（约 10 秒）最多载入 40 个，正在处理请求的账户下一轮再试；载入前账户列表仍显示原来的状态，载入后才加入验证队列',
  'onboard.notReuploaded': '未按重新上传处理 {count}',
  'onboard.notReuploadedTip':
    '处于需要登录、403 等失效状态的账户换上的认证文件，被当成本服务自己的文件（上传时保留了原文件的修改时间）或共用账户目录的其他进程回写（只差 Google 轮换的 Cookie 或 AI Studio 的 localStorage），没有清除旧判定、没有重新认证（本次运行的次数，日志里有每个账户）。如果是你重新上传的：对账户点“验证”（通过即清除 403 与冷却），或关闭保留时间戳后重新上传、上传后 touch 一下',
  'onboard.unchanged': '认证文件未变化 {count}',
  'onboard.unchangedTip':
    '本来就正常的账户（或只是手动停用的账户）重新上传了与当前登录状态相同的认证文件，没有重新认证（本次运行的次数，日志里有每个账户）。处于需要登录、403 等失效状态的账户不会计在这里，照样按重新上传处理',
  'onboard.sameLogin': '登录未变、未重新载入 {count}',
  'onboard.sameLoginTip':
    '状态正常的启用账户换上了内容有变化、但登录签名 Cookie 没变的认证文件（同一会话重新导出，或共用账户目录的其他进程回写），按原口径没有重新载入、没有重新验证；换了登录的文件才会重新认证（本次运行的次数，同一账户每小时最多计一次，日志里有每个账户）',
  'onboard.sourceReauth': '重新上传',
  'onboard.reauthAfter': '重新上传后：{outcome}',
  'onboard.reauthOutcomesTip':
    '各账户最近一次重新上传认证文件后的结果：已恢复为验证通过、恢复调度；验证通过后 24 小时内又返回 403 的改记为仍需验证（需要验证账号）或仍有 403',
  'onboard.reauthOutcome.restored': '已恢复',
  'onboard.reauthOutcome.auth_required': '仍需登录',
  'onboard.reauthOutcome.verify_required': '仍需验证',
  'onboard.reauthOutcome.forbidden': '仍有 403',
  'onboard.reauthOutcome.restricted': '仍受限',
  'onboard.reauthOutcome.manual_disabled': '跳过（手动停用）',
  'onboard.reauthOutcome.verify_off': '已重新载入，未验证',
  'onboard.reauthOutcome.error': '验证出错',
  'onboard.reauthOutcome.skipped': '跳过',
  'onboard.invalid': '每批数量 1～200，验证并发 1～8',
  'onboard.saving': '保存中…',
  'onboard.saved': '已保存并生效',
  'onboard.recent': '最近处理',
  'onboard.none': '暂无处理记录',
  'onboard.result.enabled': '已启用',
  'onboard.result.verified': '验证通过',
  'onboard.result.verify_failed': '验证未通过',
  'onboard.result.error': '出错',
  'onboard.result.skipped': '跳过',
  'onboard.result.manual': '手动添加',
  'onboard.result.baseline': '基线',
  'onboard.queueDisabled': '处理现有停用账户（{count}）',
  'onboard.queueDisabledConfirm':
    '将把 {count} 个已停用的账户加入自动处理队列：先验证登录，验证通过的会被启用，未通过的保持停用。你手动停用的账户如果验证通过也会被启用。管理页手动添加的账户不受影响。',
  'onboard.queueDisabledAction': '加入队列',
  'accounts.worker': 'Worker',
  'accounts.workerWarm': '运行中',
  'accounts.workerStarting': '启动中',
  'accounts.workerNone': '未启动',
  'accounts.workerFilterAll': 'Worker：全部',
  'accounts.workerFilterWarm': 'Worker：运行中',
  'accounts.workerFilterStarting': 'Worker：启动中',
  'accounts.workerFilterNone': 'Worker：未启动',
  'accounts.sortWorker': '按 Worker',
  'accounts.ultraCount': 'Ultra 号池就绪 {ready} / {total}',
  'accounts.ultraCountHelp':
    'Ultra 号池的账户数（关闭 Pro 独立号池、开启“Pro 账户并入 Ultra 号池”时含 Pro 账户）：经 /ultra 前缀的请求只用这些账户',
  'accounts.ultraWorkers': 'Ultra Worker {warm} / {target}',
  'accounts.proCount': 'Pro 号池就绪 {ready} / {total}',
  'accounts.proCountHelp':
    'Pro 号池的账户数（开启 Pro 独立号池时权益为 Pro 的账户）：经 /pro 前缀的请求只用这些账户，默认地址与 /ultra 不用它们',
  'accounts.proWorkers': 'Pro Worker {warm} / {target}',
  'accounts.poolUltraTag': 'Ultra 号池',
  'accounts.proInUltraHelp':
    '已开启“Pro 账户并入 Ultra 号池”：这个账户的权益仍是 Pro，但属于 Ultra 号池，按 Ultra 号池的设置调度，经 /ultra 前缀的请求会用它；服务配置里的“Ultra 号池的账户只服务 /ultra 请求”开启时，它不再服务普通路径的请求',
  'accounts.tierFilterAll': '权益：全部',
  'accounts.tierFilterUltra': '权益：Ultra',
  'accounts.poolFilter': '号池',
  'accounts.poolFilterHelp':
    '按账户当前所属的号池筛选（成员关系，不是权益）：开启 Pro 独立号池时 Pro 账户属于 Pro 号池，关闭时留在普通号池或并入 Ultra 号池',
  'accounts.poolFilterAll': '号池：全部',
  'accounts.poolFilterNormal': '号池：普通',
  'accounts.poolFilterPro': '号池：Pro',
  'accounts.poolFilterUltra': '号池：Ultra',
  'accounts.extraWorker': '补充池',
  'accounts.extraWorkerHelp':
    '这个 Worker 计入补充池：账户有模型冷却、其他模型可用，补充池借它补给各入口',
  'accounts.extraMember': '可补充',
  'accounts.extraMemberHelp':
    '补充池成员：有模型冷却、其他模型可用（冷却的模型与结束时刻见请求页的冷却列表）。账户列表按最近一次对账，不随成员变化推送',
  'accounts.extraCount': '补充池 Worker {workers} / {warm} · 成员 {members}',
  'accounts.extraCountOff': '补充池已关闭 · 归还中 {workers}',
  'accounts.extraCountHelp':
    '补充池（EXTRA_POOL）：Worker 为计入补充池的 Worker 数与常驻数，成员为有模型冷却、其他模型可用的账户（含 Worker 驻留在主分区的）',
  'accounts.poolFilterExtra': '号池：补充池',
  'accounts.tierFilterPro': '权益：Pro',
  'accounts.tierFilterPlus': '权益：Plus',
  'accounts.tierFilterFree': '权益：Free',
  'accounts.tierFilterUnknown': '权益：未读取',
  'accounts.tierUnknown': '未读取',
  'accounts.tierUnknownHelp': '尚未从官网读取权益，暂按 Free 计入普通号池',
  'settings.helpExpand': '展开说明',
  'settings.helpCollapse': '收起说明',
  'settings.liveHint':
    'Worker 数、并发、单账号每分钟请求上限、账户选择策略、超时、上游通道、临时对话、IP 池、API 密钥等保存后立即生效；账户目录、默认代理、WAA 后端需要重启生成服务；监听地址需要重启管理进程。',
  'logs.categoryAll': '全部',
  'logs.categoryRequest': '请求',
  'logs.categoryAccount': '账户',
  'logs.categoryService': '服务',
  'logs.groupSources': '{count} 个来源',
  'logs.groupMembers': '合并了 {count} 条同类日志',
  'logs.raw': '原始消息',
  'settings.autoApply': '修改后自动应用',
  'settings.stateSaving': '保存中…',
  'settings.stateSaved': '已保存并生效',
  'settings.stateSavedPending': '已保存，待应用',
  'settings.stateDirty': '有未保存的修改（输入框失去焦点后保存）',
  'settings.stateError': '未保存：{message}',
  'settings.applyPending': '新配置需要重启生成服务才能生效；重启前会等待进行中的请求结束（最多 30 秒）。',
  'settings.applyCountdown': '{seconds} 秒后自动应用',
  'settings.applyNow': '立即应用',
  'settings.applying': '正在应用新配置…',
  'settings.applied': '新配置已生效',
  'settings.savedStopped': '已保存，下次启动生成服务时生效',
  'settings.saveAndApply': '保存并应用',
  'settings.apiKeyHot': '保存后立即生效，无需重启',
  'settings.apiKeyDefault':
    '调用 API 必须携带此密钥；留空保存即使用默认密钥 {key}。默认密钥随源码公开，对外监听时请改为自定义密钥',
  'settings.invalidRequired': '{field}不能为空',
  'settings.invalidNumber': '{field}必须是正整数',
  'settings.invalidAccountRpmLimit': '单账号每分钟请求上限应为 0（关闭）到 1000 的整数',
  'settings.invalidDuration': '{field}格式应为 30s、2m、1h30m 这类时长',
  'settings.invalidFirstEventTimeout':
    '首事件超时应为 0 或 off（关闭），或小于请求超时的整数秒或时长，例如 60 或 90s',
  'settings.invalidPrimaryQueueWait': '主用模型排队上限应为 0（不排队）或不超过请求超时的时长，例如 10s',
  'settings.invalidTierRuleModels': '{field}不能写 long- 开头的名称，请写基础模型 ID',
  'settings.invalidUltraPrimaryQueueWait':
    'Ultra 主用模型排队上限应为 0（不排队）或不超过请求超时的时长，例如 10s',
  'settings.invalidListen': '监听地址格式应为 主机:端口',
  'settings.invalidMaxWorkers': '峰值 Worker 上限不能小于常驻 Worker 数',
  'settings.invalidWarmConcurrency': '启动预热并发应在 1 到常驻 Worker 数之间',
  'settings.invalidUltraWarmWorkers': 'Ultra 常驻 Worker 数必须是 0 或正整数',
  'settings.invalidUltraMaxWorkers': 'Ultra 峰值 Worker 上限必须是正整数，且不能小于 Ultra 常驻 Worker 数',
  'settings.invalidProWarmWorkers': 'Pro 常驻 Worker 数必须是 0 或正整数',
  'settings.invalidProMaxWorkers':
    'Pro 峰值 Worker 上限必须是正整数，且不能小于 Pro 常驻 Worker 数',
  'settings.downgradeGuard': '拒绝被上游降级的回复',
  'settings.downgradeGuardHelp':
    '请求下面列出的模型时，在把正文交给客户端之前按出字速度与上游标明的模型判断这次是否被换成了其他模型（例如 3.1 Pro 被换成 3.1 Flash-Lite），判定为降级就按各协议官方格式返回 HTTP 400，不把降级的内容发给用户。其他模型不受影响。',
  'settings.downgradeModels': '拦截的模型',
  'settings.downgradeModelsHelp': '逗号分隔的模型 ID；-nothinking、-128 等别名会自动对应到这里的模型。',
  'settings.downgradeSpeed': '降级速度阈值（tok/s）',
  'settings.downgradeMinTokens': '判定所需正文 token',
  'settings.downgradeMinWindow': '最短判定窗口（毫秒）',
  'settings.downgradeSpeedHelp':
    '从第一块正文之后算起，新增正文达到所需 token 数、且经过最短窗口后按速度判定，达到阈值即为降级。正常 Pro 约 90～150 tok/s，被降级时约 230～270。数值越大判定越准，正文首字也越晚。',
  'settings.downgradeFuzzyLow': '模糊区间下限（tok/s）',
  'settings.downgradeFuzzyHigh': '模糊区间上限（tok/s）',
  'settings.downgradeCountTimeout': 'CountTokens 超时（毫秒）',
  'settings.downgradeFuzzyHelp':
    'Playground 通道用上游的累计正文数，速度是精确的（开启后被拦截的模型优先走 Playground）；Build 通道只能按字数估算，估算速度落在模糊区间内时用同一账号调用 CountTokens 精确计算后再判定，超时或失败时按估算判定。CountTokens 超时填 0 表示不调用。',
  'settings.downgradeFastMode': '快速模式',
  'settings.downgradeFastModeHelp':
    '默认是严格模式：流式请求在判定前不发出任何内容（包括思考），最多扣住响应头 50 秒（与首事件前扣住响应头的上限相同），在这之内判定为降级时直接返回 HTTP 400；到 50 秒（设了最长延后时为该时限）还没判定就先返回 200，之后判定为降级时在流中发送错误事件。快速模式从一开始就实时转发思考、只缓存正文，被降级时在流中发送官方格式的错误事件。',
  'settings.downgradeRejectStatus': '拒绝时的返回码',
  'settings.downgradeReject400': '400：按内容策略拦截返回（默认）',
  'settings.downgradeReject503': '503：按服务暂时不可用返回',
  'settings.downgradeRejectStatusHelp':
    '400 使用 Google 输入被内容策略拦截（PROHIBITED_CONTENT）的官方格式，客户端与中转通常不会重试；503 如实说明上游换用了其他模型、回复已丢弃，客户端与中转可以重试或切换渠道。修改后立即生效。',
  'settings.downgradeMemory': '记住被降级的对话（分钟）',
  'settings.downgradeMemoryHelp':
    '同一段对话在这段时间内再次请求、且当时的消息原样都在时（重新生成、接着往下聊），发送前直接拒绝；用户改过其中任何一条就重新判定。只有一条消息的对话、以及客户端中途断开的请求不记录。只保存哈希，0 表示不记录。',
  'settings.downgradeMaxHold': '最长延后（毫秒）',
  'settings.downgradeMaxHoldHelp':
    '默认 0 表示扣到判定出结果或 50 秒为止：判定前扣住思考，50 秒内判定为降级时返回 HTTP 400，首字要晚整个思考时长；到 50 秒还没判定时先返回 200 并开始写心跳，之后判定为降级时在流中发送错误事件。设为正数（例如 3000）时，内容最多被延后这么久：到时限还没判定就先返回 200 并发出已缓存的思考，正文继续缓存，之后判定为降级只能在流中发送错误事件；正文缓存到时限仍未判定则放行，结束时照常复核并统计漏判。',
  'settings.invalidDowngradeModels': '开启降级判定时至少要填一个拦截的模型',
  'settings.invalidDowngradeFuzzy': '模糊区间下限不能大于上限',
  'accounts.autoImportHint':
    '账户目录放进 auth/ 后约 10 秒自动导入；删除目录会自动移出；需要登录的账户替换认证文件后会自动重新载入。',
  'usage.title': '用量',
  'usage.description': '通过 API key 校验的公开 API 请求的数量、结果、Token 与耗时，token 计数请求不计入，记录保留 90 天',
  'usage.unavailable': '用量账本未启用：本次启动时账本打开失败，原因见服务日志',
  'usage.panelLoading': '正在加载用量页面',
  'usage.panelFailed': '用量页面加载失败，请刷新浏览器重试',
  'usage.range': '时间范围',
  'usage.range.1h': '近 1 小时',
  'usage.range.6h': '近 6 小时',
  'usage.range.24h': '近 24 小时',
  'usage.range.7d': '近 7 天',
  'usage.range.30d': '近 30 天',
  'usage.range.90d': '近 90 天',
  'usage.range.today': '今天',
  'usage.range.yesterday': '昨天',
  'usage.range.week': '本周',
  'usage.range.month': '本月',
  'usage.range.lastMonth': '上月',
  'usage.range.custom': '自定义',
  'usage.rangeRecent': '最近',
  'usage.rangeCalendar': '日历',
  'usage.rangeCustom': '自定义范围',
  'usage.rangeStart': '开始',
  'usage.rangeEnd': '结束',
  'usage.rangeApply': '应用',
  'usage.rangeInvalid': '开始时间必须早于结束时间',
  'usage.rangeTooLong': '范围不能超过 93 天',
  'usage.rangeRetention': '记录保留 90 天，更早的时间没有数据',
  'usage.filters': '筛选',
  'usage.filtersClear': '清除筛选',
  'usage.filterSearch': '搜索取值',
  'usage.filterNone': '没有可选的取值',
  'usage.dimension.model': '模型',
  'usage.dimension.account': '账户',
  'usage.dimension.channel': '通道',
  'usage.dimension.protocol': '协议',
  'usage.dimension.state': '结果',
  'usage.dimension.pool': '号池',
  'usage.poolAll': '全部号池',
  'usage.dimension.status': '状态码',
  'usage.autoRefresh': '自动刷新',
  'usage.autoRefresh.0': '不自动刷新',
  'usage.autoRefresh.15': '每 15 秒',
  'usage.autoRefresh.30': '每 30 秒',
  'usage.autoRefresh.60': '每 60 秒',
  'usage.updatedAt': '更新于 {time}',
  'usage.nextRefresh': '{seconds} 秒后刷新',
  'usage.fixedRange': '固定范围，不自动刷新',
  'usage.latest': '最近请求：{time}',
  'usage.latestNone': '还没有请求记录',
  'usage.latestHelp': '账本中最新一条请求的完成时间，不受时间范围与筛选影响',
  'usage.justNow': '刚刚',
  'usage.secondsAgo': '{n} 秒前',
  'usage.minutesAgo': '{n} 分钟前',
  'usage.hoursAgo': '{n} 小时前',
  'usage.daysAgo': '{n} 天前',
  'usage.requests': '请求数',
  'usage.requestsHelp': '范围内完成的公开 API 请求；成功包含工具调用、达到输出上限与上游终止',
  'usage.succeeded': '成功',
  'usage.failed': '失败',
  'usage.canceled': '取消',
  'usage.rateLimited': '限流 429',
  'usage.otherErrors': '其他错误',
  'usage.successRate': '成功率',
  'usage.successRateHelp': '成功 ÷（请求 − 客户端取消）。工具调用、达到输出上限与上游终止都计为成功',
  'usage.quality': '降级拦截率',
  'usage.qualityHelp': '因降级被拒绝的请求 ÷ 经过降级判定的请求；重复回复率为回复与 2 小时内某次回复完全相同的请求 ÷ 回复不少于 50 token 的请求',
  'usage.downgradeRejected': '拦截 / 判定',
  'usage.duplicateReplies': '重复回复',
  'usage.tokens': 'Token',
  'usage.input': '输入',
  'usage.reasoning': '推理',
  'usage.reply': '回复',
  'usage.throughput': '吞吐',
  'usage.throughputHelp': '平均值为范围内的请求数或 Token 除以范围分钟数；当前值取最近 {minutes} 分钟',
  'usage.current': '当前',
  'usage.duration': '耗时',
  'usage.durationHelp': '成功请求从进入服务到响应结束的耗时',
  'usage.firstEvent': '首个事件',
  'usage.firstEventHelp': '成功请求从进入服务到收到首个上游语义事件的耗时',
  'usage.average': '平均',
  'usage.queue': '排队',
  'usage.vsPrevious': '相比上一周期 {range}',
  'usage.points': '{value} 个百分点',
  'usage.chartTraffic': '流量',
  'usage.chartTrafficHelp': '按维度堆叠，前 8 项以外的取值合并为“其他”',
  'usage.metricRequests': '请求',
  'usage.metricTokens': 'Token',
  'usage.stackBy': '堆叠维度',
  'usage.other': '其他',
  'usage.unassigned': '未分配',
  'usage.chartOutcomes': '结果与错误',
  'usage.chartLatency': '耗时分位',
  'usage.chartTokens': 'Token 构成',
  'usage.total': '合计',
  'usage.ranking': '分项排行',
  'usage.lastRequest': '最近请求',
  'usage.onlyThis': '只看此项',
  'usage.showBreakdown': '展开明细',
  'usage.hideBreakdown': '收起明细',
  'usage.pairsLoading': '正在读取组合明细…',
  'usage.pairsFailed': '组合明细读取失败：{error}',
  'usage.pairsEmpty': '当前范围内这一行没有组合明细',
  'usage.pairsMore': '再显示 {count} 项（还有 {hidden} 项未显示）',
  'usage.records': '请求记录',
  'usage.recordsCount': '共 {count} 条，按完成时间倒序',
  'usage.recordsSearch': '搜索请求 ID、错误或回复指纹',
  'usage.recordsAllStatus': '全部状态码',
  'usage.extraFilter': '只看由补充池服务',
  'usage.extraFilterHelp':
    '只列出最后一次尝试借用了补充池的请求（号池列仍按入口）；导出 CSV 同样只含这些行，列不变',
  'usage.export': '导出 CSV',
  'usage.columns': '显示列',
  'usage.pageSize': '每页 {count} 条',
  'usage.previousPage': '上一页',
  'usage.nextPage': '下一页',
  'usage.page': '第 {page} 页',
  'usage.recordsEmpty': '没有符合条件的请求记录',
  'usage.time': '完成时间',
  'usage.result': '结果',
  'usage.attempts': '重试',
  'usage.openDetail': '查看请求详情',
  'usage.state.completed': '完成',
  'usage.state.tool_calls': '工具调用',
  'usage.state.limited': '达到输出上限',
  'usage.state.blocked': '上游终止',
  'usage.state.failed': '失败',
  'usage.state.cancelled': '已取消',
  'usage.downgrade': '降级判定',
  'usage.downgrade.rejected': '因降级拒绝',
  'usage.downgrade.passed': '判定正常',
  'usage.downgrade.unjudged': '无法判定，已放行',
  'usage.downgradeShort': '降级',
  'usage.duplicate': '与近期某次回复完全相同',
  'usage.duplicateShort': '重复',
  'usage.servedModel': '实际服务模型',
  'usage.replyHash': '回复指纹',
  'usage.searchReply': '查找同一回复',
  'usage.detailTitle': '请求详情',
  'usage.detailSummary': '概要',
  'usage.detailError': '错误',
  'usage.detailAttempts': '上游尝试',
  'usage.detailAttemptsNone': '第一次尝试即得到结果',
  'usage.detailBody': '正文',
  'usage.detailBodyNone': '这条请求完成时正文记录未开启（REQUEST_BODY_LOG）',
  'usage.detailRequest': '请求',
  'usage.detailResponse': '响应',
  'usage.detailTruncated': '已截断，保存 {saved}，原始 {size}',
  'usage.path': '路径',
  'usage.toolCalls': '工具调用',
  'usage.empty': '这段时间没有请求',
  'usage.emptyHelp': '公开 API 请求完成后计入用量。',
  'usage.emptyFiltered': '没有符合筛选条件的请求',
  'usage.emptyFilteredHelp': '调整筛选或时间范围后再试。',
  'usage.retry': '重试',
  'usage.protocol.images': '图片',
  'usage.protocol.audio': '音频',
  'usage.protocol.videos': '视频',
  'usage.protocol.files': '文件',
  'usage.protocol.other': '其他',
  // ---------- IP 池 ----------
  'proxy.warnDuplicateIP':
    '有出口的 IP 相同：这些端口或会话可能对应同一个 IP，账号会共用 IP。请检查代理商的分配。',
  'proxy.warnPoolPartial': '部分出口测试失败，原因见上面各行。',
  'proxy.slotPort': '端口 {port}',
  'proxy.slotSession': '会话 {session}',
  'proxy.testAccountSlot': '这个账户当前的出口：{slot}',
  'proxy.testAccountClearedSlot':
    '清空账户代理后由 IP 池分配，例如：{slot}（之后启动的 Worker 可能分到别的端口）',
  'proxy.testPoolSlot': 'IP 池此刻会分给登录的出口：{slot}（实际以登录时分到的为准）',
  'settings.proxyPoolTitle': 'IP 池（每个浏览器一个 IP）',
  'settings.proxyPoolTitleFixed': 'IP 池（每个账户固定一个 IP）',
  'settings.proxyPoolHelp':
    '没有自己代理的账户改用 IP 池出网。静态 IP：端口段里每个端口是一个固定 IP，每个运行中的浏览器（Worker、登录、验证）独占一个端口，账号尽量回到上次用过的端口；动态 IP：地址里的 {session} 按账户换成固定会话号，一个会话号一个粘性 IP。优先级：账户自己的代理 > IP 池 > 默认代理。“一个账号一个 IP”指同一时刻：账号多于端口时，一个 IP 会先后服务多个账号。',
  'settings.proxyPoolHelpFixed':
    '没有自己代理的账户改用 IP 池出网。已开启固定分配：静态 IP 下每个账户固定一个端口（端口段里每个端口是一个固定 IP），同一端口的几个账户共用这个 IP、可以同时开浏览器，不再每个浏览器独占一个端口；动态 IP 下地址里的 {session} 按账户换成固定会话号，一直用这个会话号。少数 IP（会话）坏了时自动换，30 分钟内坏得超过设置值就停止自动换、暂停等你手动处理（设置为 0 时不自动换）。优先级：账户自己的代理 > IP 池 > 默认代理。',
  'settings.proxyPoolMode': '模式',
  'settings.proxyPoolModeOff': '关闭（使用默认代理）',
  'settings.proxyPoolModeStatic': '静态 IP（端口段，每个浏览器独占一个端口）',
  'settings.proxyPoolModeStaticFixed': '静态 IP（端口段，每个账户固定一个端口）',
  'settings.proxyPoolModeDynamic': '动态 IP（会话号，每个账户一个固定会话）',
  'settings.proxyPoolURL': '代理地址',
  'settings.proxyPoolURLHelpOff': '关闭时保留地址，方便再次开启。',
  'settings.proxyPoolURLHelpStatic':
    '填代理商给的任意一个端口的地址，例如 http://用户名:密码@isp.decodo.com:10000；端口会自动换成下面端口段里的端口（一个端口一个 IP）。写法同默认代理，保存时统一改写成 URL 形式。',
  'settings.proxyPoolURLHelpDynamic':
    '在用户名或密码里会话号的位置写 {session}（不区分大小写），例如 http://customer-账号-cc-US-sessid-{session}:密码@gate.ipfoxy.io:58688；程序按账户换成固定会话号（只含小写字母和数字，最长 15 个字符）。只在代理连不通时换会话号；裸 429 不换（每个会话只有一个账号，分不清是账号还是 IP 的问题），代理能连通、但被上游限制的会话不会自动换。建议在代理商处设置较长的会话时长。',
  'settings.proxyPoolURLHelpDynamicFixed':
    '在用户名或密码里会话号的位置写 {session}（不区分大小写），例如 http://customer-账号-cc-US-sessid-{session}:密码@gate.ipfoxy.io:58688；程序按账户换成固定会话号（只含小写字母和数字，最长 15 个字符）。已开启固定会话：每个账户一直用它的会话号；少数会话连续出口连不通时自动换会话号，30 分钟内超过设置值就停止自动换、等你手动换会话（设置为 0 时不换）。建议在代理商处设置较长的会话时长。',
  'settings.proxyPoolURLPlaceholderStatic': 'http://用户名:密码@isp.decodo.com:10000',
  'settings.proxyPoolURLPlaceholderDynamic':
    'http://customer-账号-sessid-{session}:密码@gate.ipfoxy.io:58688',
  'settings.proxyPoolPortStart': '起始端口',
  'settings.proxyPoolPortEnd': '结束端口',
  'settings.proxyPoolPortsHelp':
    '端口段含起止两端，最多 5000 个端口。每台服务器（每个进程）用不重叠的端口段，例如这台 10001–10200、另一台 10201–10400；同一台机器上的多个进程按端口加锁，不会分到同一端口。',
  'settings.proxyPoolCapacity':
    '共 {ports} 个端口；最多同时 {need} 个浏览器（普通 {normal}+{normalDrain}、Ultra {ultra}+{ultraDrain}，加号后为排空时先启动的替补），余量 {spare} 个，登录与验证另占（默认验证并发 3）。',
  'settings.proxyPoolCapacityPro':
    '共 {ports} 个端口；最多同时 {need} 个浏览器（普通 {normal}+{normalDrain}、Ultra {ultra}+{ultraDrain}、Pro {pro}+{proDrain}，加号后为排空时先启动的替补），余量 {spare} 个，登录与验证另占（默认验证并发 3）。',
  'settings.proxyPoolCapacityExtra':
    '共 {ports} 个端口；最多同时 {need} 个浏览器（普通 {normal}+{normalDrain}、Ultra {ultra}+{ultraDrain}、补充池 {extra}+{extraDrain}，加号后为排空或替换时先启动的浏览器），余量 {spare} 个，登录与验证另占（默认验证并发 3）。',
  'settings.proxyPoolCapacityProExtra':
    '共 {ports} 个端口；最多同时 {need} 个浏览器（普通 {normal}+{normalDrain}、Ultra {ultra}+{ultraDrain}、Pro {pro}+{proDrain}、补充池 {extra}+{extraDrain}，加号后为排空或替换时先启动的浏览器），余量 {spare} 个，登录与验证另占（默认验证并发 3）。',
  'settings.proxyPoolShortage':
    '端口少于可能同时运行的浏览器数（需要 {need} 个）：超出的浏览器会与其他账号共用端口（同一个 IP）。请扩大端口段，或调低峰值 Worker 上限、启动预热并发。',
  'settings.proxyPoolTight':
    '余量只有 {spare} 个：登录与验证同时进行时可能与其他账号共用端口。建议端口数 ≥ 最多同时运行的浏览器数 + 验证并发。',
  'settings.proxyPoolDefaultProxyHint':
    '开启后，没有自己代理的账户不再使用默认代理；只有端口全被占用时，没有浏览器的账户的少量后台请求才暂时退回默认代理，端口空出来后自动回到 IP 池。',
  'settings.proxyPoolLiveHint':
    '保存后立即生效，不重启生成服务（还有没应用的、需要重启的改动时随重启生效，例如默认代理或账户目录）：运行中的 Worker 继续用原来的出口，关闭后才切换；要立即全部切换，在左下角停止服务后再启动。',
  'settings.proxyPoolInvalidURL': '请填写代理地址',
  'settings.proxyPoolInvalidURLPort':
    '不写协议时必须写端口：填代理商给的任意一个端口的地址，例如 isp.example.com:10000 或 http://用户名:密码@isp.example.com:10000（静态 IP 的端口按端口段自动替换）',
  'settings.proxyPoolInvalidStaticSession':
    '静态 IP 的地址不能包含 {session}：端口按端口段自动替换',
  'settings.proxyPoolInvalidDynamicSession':
    '动态 IP 的地址必须包含 {session}：写在用户名或密码里会话号的位置，例如 customer-账号-sessid-{session}',
  'settings.proxyPoolInvalidSessionHost': '{session} 只能写在用户名或密码里',
  'settings.proxyPoolInvalidPorts': '端口必须是 1 到 65535 的整数，且起始端口不大于结束端口',
  'settings.proxyPoolInvalidPortsTooMany': '端口段最多 5000 个端口',
  'settings.proxyPoolSample': '抽测出口',
  'settings.proxyPoolSampling': '抽测中…',
  'settings.proxyPoolSampleHint':
    '“抽测出口”用输入框里当前的设置（不必先保存）测 3 个端口（首、中、尾）或 3 个临时会话号的出口 IP；“检查全部端口”逐个查询端口段里每个端口的出口 IP（后台请求路径，并发 10，最长 5 分钟），确认互不相同。都只访问 api.ipify.org，不访问 Google。',
  'settings.proxyPoolSampleSummary': '{count} 个出口，不同 IP {distinct} 个',
  'settings.proxyPoolCheck': '检查全部端口',
  'settings.proxyPoolCheckRunning': '检查中 {done} / {total}',
  'settings.proxyPoolCheckBusy': '上一次检查还没有结束，进度见下方',
  'settings.proxyPoolCheckDone':
    '检查完成（{time}）：{total} 个端口，成功 {ok}，不同 IP {distinct}，失败 {failed}',
  'settings.proxyPoolCheckAllDistinct': '全部端口的出口 IP 互不相同',
  'settings.proxyPoolCheckDuplicates': '出口 IP 相同的端口（{count} 组）：',
  'settings.proxyPoolCheckFailed': '失败的端口（{count} 个）：',
  'settings.proxyPoolStatusStatic':
    '当前：使用中 {inUse} / {total} · 租约 {leases} · 冷却 {cooling} · 共用 {shared}',
  'settings.proxyPoolStatusDynamic': '当前：使用中的会话 {inUse} · 租约 {leases} · 换会话 {rotations}',
  'settings.proxyPoolAffinity': '本次运行回到上次端口 {hits} 次，未能回到 {misses} 次',
  'settings.proxyPoolUnpooled':
    '{count} 个运行中的 Worker 仍走默认代理（例如开启 IP 池或改为跟随默认代理之前就在运行），Worker 重启后切换到 IP 池；要立即切换，停止服务后再启动。',
  'settings.proxyPoolStale': '{count} 个浏览器仍在用改配置前的出口，关闭后归还。',
  'settings.proxyPoolExternal':
    '{count} 个端口被账户自己的代理、默认代理指向或被其他进程占用，不会分配。',
  'settings.proxyPoolOrphaned': '{count} 个已删除账户的浏览器关闭失败，仍占着出口。',
  'settings.proxyPoolRelayLimit':
    '本机代理中转的连接上限只有 {limit}，浏览器多时可能出现“连接数已满”：建议把 ulimit -n 调到 65536 以上后重启程序。',
  'settings.proxyPoolPinned':
    '{count} 个账户的账户代理与默认代理相同（多半是 setup 导入时写进去的），不会使用 IP 池。',
  'settings.proxyPoolFollow': '改为跟随默认代理',
  'settings.proxyPoolFollowConfirm':
    '清空这 {count} 个账户的账户代理（改为跟随默认代理），之后它们按 IP 池出网。运行中的 Worker 不重启，继续走默认代理，关闭后才改用 IP 池；正在被其他操作使用的账户会跳过，稍后可以再点一次。',
  'settings.proxyPoolFollowAction': '改为跟随',
  'settings.proxyPoolFollowDone':
    '已改为跟随默认代理：{changed} 个账户（其中 {running} 个的 Worker 关闭后改用 IP 池），跳过 {skipped} 个',
  'app.proxyPoolStatic': 'IP 池 {inUse}/{total} · 冷却 {cooling} · 共用 {shared}',
  'app.proxyPoolDynamic': 'IP 池（动态）· 会话 {inUse} · 换会话 {rotations}',
  'app.proxyPoolOff': 'IP 池（已关闭）',
  'app.proxyPoolStale': '旧出口 {count}',
  'app.proxyPoolUnpooled': '仍走默认代理 {count}',
  'app.proxyPoolExternal': '外部占用 {count}',
  'app.proxyPoolOrphaned': '已删账户占用 {count}',
  'app.proxyPoolDetailStatic':
    '{endpoint} 端口 {start}-{end}：租约 {leases}（Worker 与登录、验证）· 最多同时 {capacity} 个浏览器 · 回到上次端口 {hits} 次 / 未回到 {misses} 次',
  'app.proxyPoolDetailDynamic': '{endpoint}：租约 {leases}（Worker 与登录、验证）· 换会话 {rotations} 次',
  'app.proxyPoolDetailCooling': '冷却：{ports}',
  'app.proxyPoolDetailCoolingPort': '{port}（至 {until}，第 {level} 档）',
  'app.proxyPoolDetailHot': '最近 10 分钟裸 429 最多：{ports}',
  'app.proxyPoolDetailHotPort': '{port}（{count} 次 / {accounts} 个账号）',
  'app.proxyPoolDetailShortage': '端口少于可能同时运行的 {capacity} 个浏览器，超出的与其他账号共用端口',
  'app.proxyPoolDetailEgress':
    '出口限流判定按端口或会话分别统计：共 {total} 个出口，判定成立 {limited} 个、已撤销 {rejected} 个（判定成立或撤销的出口在上方单独列出）',
  'accounts.proxyPoolSlot': 'IP 池 · {slot}',
  'accounts.proxyPoolActive': '使用中',
  'accounts.proxyPoolLogin': '登录中',
  'accounts.proxyPoolBackground': '后台',
  'accounts.proxyPoolActiveHelp':
    '这个账户的浏览器正在独占这个出口（静态 IP 时端口归它独占），后台请求与浏览器同一出口',
  'accounts.proxyPoolLoginHelp': '这个账户正在登录或验证，浏览器用这个出口',
  'accounts.proxyPoolBackgroundHelp':
    '没有运行中的浏览器：后台请求（目录同步、复核、续签）暂用这个出口，启动 Worker 时重新分配（尽量回到上次的端口）',
  'accounts.proxyPoolUnpooled': '默认代理（Worker 重启后切换到 IP 池）',
  'accounts.proxyPoolUnpooledHelp':
    '这个 Worker 仍走默认代理（例如开启 IP 池或改为跟随默认代理之前就在运行）；Worker 重启后切换到 IP 池。要立即切换，停止服务后再启动',
  'accounts.proxyPoolDefault': '默认代理',
  'accounts.proxyPoolDefaultHelp': 'IP 池端口都被占用：没有浏览器的账户的后台请求暂用默认代理',
  'accounts.proxyPoolPinned': '固定为默认代理（不使用 IP 池）',
  'accounts.proxyPoolPinnedHelp':
    '账户代理与默认代理相同，因此不使用 IP 池；可在“服务配置 → IP 池”改为跟随默认代理',
  'settings.proxyPoolFixed': '固定分配（每个账户固定一个端口）',
  'settings.proxyPoolFixedHelpStatic':
    '开启后，每个走 IP 池的账户（没有自己代理的）固定一个端口，按账户数平均分到端口段的端口上，例如 2000 个账户、500 个端口 → 每个端口 4 个账户（除不尽时各端口相差不超过 1）。同一端口的几个账户共用这个 IP、可以同时开浏览器；账户的 Worker、登录、验证、Chrome 导入与后台请求都只用它的固定端口。之后重启、更新、停止再启动、改其他设置都不变。端口冷却（IP 坏了：连续出口连不通、不同账号返回限流）时少数自动换 IP：这个端口上的账户挪到其他账户最少、没有暂停的端口，坏端口停用；30 分钟内坏得超过下面的设置值就停止自动换，之后坏掉的端口上的账户暂停使用、等你手动换 IP；被其他进程占用时只暂停、等它释放（都不走默认代理、不直连）。分配记在 runtime/proxy-pool-fixed.json：首次开启时尽量保留账户上次用的端口，之后新增的账户分到账户最少的端口，删除账户只释放它自己的位置。',
  'settings.proxyPoolFixedDynamic': '固定会话（每个账户一个固定会话号）',
  'settings.proxyPoolFixedHelpDynamic':
    '开启后，每个账户一直用它的会话号（同一个 IP），会话号记在 runtime/proxy-pool.json，重启、更新都不变。少数会话连续出口连不通时自动换这个账户的会话号；30 分钟内超过下面的设置值就停止自动换，这些账户一直用原会话（Worker 启动会失败）、等你在 IP 池状态里手动换会话。设置为 0 时不自动换会话。',
  'settings.proxyPoolFixedEnableConfirm':
    '开启固定分配：{accounts} 个账户分到 {ports} 个端口，每个端口约 {perPort} 个账户（同一端口的账户共用这个 IP，可以同时开浏览器；以前开启过时沿用原来的分配表、不重新平均，需要均匀时开启后点“重新平均分配”）。之后重启、更新都不变；端口冷却（IP 坏了）时少数自动换 IP，30 分钟内坏得超过设置值就停止自动换、暂停等你手动换（设置为 0 时只暂停）；被其他进程占用时只暂停。运行中的浏览器关闭后改用固定端口（要立即切换，停止服务后再启动）。',
  'settings.proxyPoolFixedEnableConfirmDynamic':
    '开启固定会话：每个账户一直用它的会话号；少数会话连续出口连不通时自动换会话，30 分钟内超过设置值就停止自动换、等你手动换会话（设置为 0 时不换，这个账户的 Worker 启动会失败直到会话恢复）。',
  'settings.proxyPoolFixedEnableAction': '开启',
  'settings.proxyPoolFixedDisableConfirm':
    '关闭固定分配：分配表保留，再次开启时沿用；之后启动的浏览器按原方式分配端口（每个浏览器独占一个端口，账户可能换 IP），运行中的浏览器关闭后改用原方式。',
  'settings.proxyPoolFixedDisableConfirmDynamic': '关闭固定会话：连续出口连不通时恢复自动换会话。',
  'settings.proxyPoolFixedDisableAction': '关闭',
  'settings.proxyPoolFixedShrinkConfirm':
    '固定分配已开启：端口 {ports} 不在新的端口段内，这些端口上约 {accounts} 个账户会立即挪到其他账户最少的端口、换 IP；之后改回端口段也不会回到原端口。确定要改端口段吗？',
  'settings.proxyPoolFixedShrinkAction': '改端口段',
  'settings.proxyPoolFixedCapacity':
    '共 {ports} 个端口、{accounts} 个账户，每端口 {perPort} 个（同一端口的账户共用这个 IP，可以同时开浏览器；固定分配下不要求浏览器数不超过端口数）。',
  'settings.proxyPoolFixedDefaultProxyHint':
    '开启后，没有自己代理的账户不再使用默认代理；固定分配下也不会退回默认代理或直连：端口暂停使用时，这个端口上的账户暂停、等它恢复。',
  'settings.proxyPoolFixedStatus': '固定分配：{ports} 个端口 · {accounts} 个账户 · 每端口 {perPort} 个',
  'settings.proxyPoolFixedEmpty': '没有账户的端口 {count} 个',
  'settings.proxyPoolFixedReserved': '登录、导入中预留 {count} 个',
  'settings.proxyPoolFixedStray':
    '{count} 个记录的账户已不在账户池（例如目录暂时移走或载入失败）：位置保留，账户回来时回到原端口；“重新平均分配”会清理它们。',
  'settings.proxyPoolFixedUnassigned': '{count} 个走 IP 池的账户还没有分到端口（下次用到时分到账户最少的端口）。',
  'settings.proxyPoolFixedTransitional':
    '{count} 个运行中的 Worker 还不在固定端口上（开启固定分配或挪动之前就在运行），关闭后改用固定端口；要立即切换，停止服务后再启动。',
  'settings.proxyPoolFixedExternal':
    '{count} 个有固定账户的端口同时被账户自己的代理或默认代理指向：照常使用（与它们共用 IP），需要时可以给这些端口换 IP。',
  'settings.proxyPoolFixedPaused':
    '暂停使用的端口 {ports} 个（账户 {accounts} 个）：端口恢复后这些账户自动恢复；IP 被限流或一直不通时可以给端口换 IP。',
  'settings.proxyPoolFixedPausedUntil': '至 {time}',
  'settings.proxyPoolFixedPausedRecheck': '下次检查 {time}',
  'settings.proxyPoolFixedPausedAccounts': '{count} 个账户',
  'settings.proxyPoolFixedListMore': '另有 {count} 个未列出',
  'settings.proxyPoolFixedExcluded': '已停用的端口 {count} 个（换 IP 后不再分配；恢复后新增账户与重新平均分配会用到它）：',
  'settings.proxyPoolFixedRestore': '恢复',
  'settings.proxyPoolFixedRestoreDone': '端口 {port} 已恢复分配（不挪动任何账户）',
  'settings.proxyPoolFixedRestoreNoop': '端口 {port} 本来就没有停用',
  'settings.proxyPoolFixedReassign': '换 IP',
  'settings.proxyPoolFixedReassignHelp': '把这个端口上的固定账户挪到其他账户最少的端口，这个端口停用（可以恢复）',
  'settings.proxyPoolFixedReassignConfirm':
    '给端口 {port} 换 IP：这个端口上的 {moved} 个账户挪到其他账户最少的端口（{targets}），端口 {port} 停用、之后不再分配（可以恢复）。',
  'settings.proxyPoolFixedReassignClosing':
    '此刻用这个端口的 {closing} 个浏览器在进行中的请求结束后关闭，下次启动用各自的固定端口。',
  'settings.proxyPoolFixedReassignEmpty': '端口 {port} 上没有固定账户：停用这个端口，之后不再分配（可以恢复）。',
  'settings.proxyPoolFixedReassignDone':
    '已给端口 {port} 换 IP：挪动 {moved} 个账户，后台关闭 {restarted} 个 Worker，端口 {port} 已停用',
  'settings.proxyPoolFixedRebalance': '重新平均分配',
  'settings.proxyPoolFixedRebalanceHint':
    '加了端口（IP）或删了很多账户之后，用“重新平均分配”让每个端口的账户数重新均匀：尽量少挪，先显示挪动数再确认；被挪的账户会换 IP。',
  'settings.proxyPoolFixedRebalanceConfirm':
    '重新平均分配：将挪动 {moved} 个账户（其中 {running} 个有运行中的浏览器，关闭后改用新端口），清理 {pruned} 个已不在账户池的记录；之后 {accounts} 个账户分到 {ports} 个端口，每端口 {perPort} 个。被挪的账户会换 IP。',
  'settings.proxyPoolFixedRebalanceNone': '已经是平均分配，不需要挪动（每端口 {perPort} 个）',
  'settings.proxyPoolFixedRebalanceDone': '已重新平均分配：挪动 {moved} 个账户，清理 {pruned} 个记录，每端口 {perPort} 个',
  'settings.proxyPoolFixedDynamicStatus': '固定会话：{accounts} 个账户一直用各自的会话号',
  'settings.proxyPoolFixedNotFixed': '固定分配没有开启（需要静态 IP 且开启固定分配）',
  'settings.proxyPoolFixedNoTarget': '没有其他可分配的端口',
  'settings.proxyPoolFixedOutOfRange': '端口不在当前端口段内',
  'settings.proxyPoolAutoReplace': '坏 IP 自动换（30 分钟内最多几个）',
  'settings.proxyPoolAutoReplaceDynamic': '坏会话自动换（30 分钟内最多几个）',
  'settings.proxyPoolAutoReplaceHelp':
    '端口冷却（连续出口连不通、不同账号返回限流）时，把这个端口上的账户挪到其他账户最少、没有暂停的端口，坏端口停用（可以恢复），用它的浏览器在进行中的请求结束后关闭；30 分钟内坏掉的端口超过这个数、再换就会让端口段里能用的端口（没有停用、不在冷却；被其他进程占用的不计入）不到一半、换过去的新 IP 又坏了、或 IP 坏掉的账户超过一半时停止自动换（像是代理商整体出了问题），之后坏掉的端口暂停、等你手动换 IP，处理后点“恢复自动换 IP”。被其他进程占用不算坏。0 为不自动换（只暂停）。0 到 5000，保存后立即生效（PROXY_POOL_FIXED_AUTO_REPLACE）。',
  'settings.proxyPoolAutoReplaceHelpDynamic':
    '账户的会话连续出口连不通时自动换这个账户的会话号；30 分钟内连不通的会话超过这个数、换出的新会话又连不通、或会话连不通的账户超过一半时停止自动换（像是代理商整体出了问题），之后的账户一直用原会话、等你在下面手动换会话，处理后点“恢复自动换会话”。0 为不自动换。0 到 5000，保存后立即生效（PROXY_POOL_FIXED_AUTO_REPLACE）。',
  'settings.invalidProxyPoolAutoReplace': '坏 IP 自动换的上限必须是 0 到 5000 的整数',
  'settings.proxyPoolAutoStatus': '自动换坏 IP：30 分钟内最多 {limit} 个 · 最近 30 分钟坏了 {recent} 个',
  'settings.proxyPoolAutoStatusDynamic': '自动换坏会话：30 分钟内最多 {limit} 个 · 最近 30 分钟连不通 {recent} 个',
  'settings.proxyPoolAutoHaltedTitle': '坏 IP 过多，已停止自动换 IP，请手动处理',
  'settings.proxyPoolAutoHaltedTitleDynamic': '坏会话过多，已停止自动换会话，请手动处理',
  'settings.proxyPoolAutoHaltedDetail':
    '{time} 停止（30 分钟内坏了 {count} 个，上限 {limit}）。暂停的端口可以逐个“换 IP”，已经自动换过的不撤回；处理完后点“恢复自动换 IP”。',
  'settings.proxyPoolAutoHaltedDetailHalf':
    '{time} 停止（30 分钟内坏了 {count} 个；再自动换的话，端口段里能用的端口会不到一半，账户会越挤越少的 IP 上）。暂停的端口可以逐个“换 IP”，已经自动换过的不撤回；停用的端口多时先在“自动换过的端口”里恢复，处理完后点“恢复自动换 IP”。',
  'settings.proxyPoolAutoHaltedDetailChain':
    '{time} 停止（换过去的新 IP 30 分钟内又坏了，像是代理商整体出了问题；30 分钟内坏了 {count} 个，上限 {limit}）。等代理商恢复，或逐个给暂停的端口“换 IP”，已经自动换过的不撤回；处理完后点“恢复自动换 IP”。',
  'settings.proxyPoolAutoHaltedDetailAccounts':
    '{time} 停止（30 分钟内 IP 坏掉的账户超过一半，像是代理商整体出了问题；30 分钟内坏了 {count} 个，上限 {limit}）。等代理商恢复，或逐个给暂停的端口“换 IP”，已经自动换过的不撤回；处理完后点“恢复自动换 IP”。',
  'settings.proxyPoolAutoHaltedSkipped': '停止之后又坏了 {skipped} 个，同样按暂停处理。',
  'settings.proxyPoolAutoHaltedDetailDynamic':
    '{time} 停止（30 分钟内 {count} 个会话连不通，上限 {limit}）。可以在下面逐个“换会话”，已经自动换过的不撤回；处理完后点“恢复自动换会话”。',
  'settings.proxyPoolAutoHaltedDetailDynamicChain':
    '{time} 停止（换出的新会话 30 分钟内又连不通，像是代理商整体出了问题；30 分钟内 {count} 个会话连不通，上限 {limit}）。等代理商恢复，或在下面逐个“换会话”，已经自动换过的不撤回；处理完后点“恢复自动换会话”。',
  'settings.proxyPoolAutoHaltedDetailDynamicAccounts':
    '{time} 停止（30 分钟内会话连不通的账户超过一半，像是代理商整体出了问题；30 分钟内 {count} 个会话连不通，上限 {limit}）。等代理商恢复，或在下面逐个“换会话”，已经自动换过的不撤回；处理完后点“恢复自动换会话”。',
  'settings.proxyPoolAutoHaltedSkippedDynamic': '停止之后又有 {skipped} 个会话连不通、没有换。',
  'settings.proxyPoolAutoResume': '恢复自动换 IP',
  'settings.proxyPoolAutoResumeDynamic': '恢复自动换会话',
  'settings.proxyPoolAutoResumeConfirm':
    '恢复自动换 IP：30 分钟计数从 0 开始，之后新坏的端口会自动换；此刻暂停的端口不补做自动换（冷却到期后恢复使用，再坏时按新的计数自动换），需要时手动换 IP。停用与冷却中的端口仍占端口段一半以上时，下一个坏的端口会再次停止自动换，先在“自动换过的端口”里恢复一些。',
  'settings.proxyPoolAutoResumeConfirmDynamic':
    '恢复自动换会话：30 分钟计数从 0 开始，之后连不通的会话会自动换；此刻连不通的账户不立刻换，它的浏览器再启动失败时按新的计数自动换（超过上限会再次停止），也可以先手动换会话。',
  'settings.proxyPoolAutoResumeDone': '已恢复自动换，30 分钟计数从 0 开始（停止期间没有自动换的仍按原样，需要时手动处理）',
  'settings.proxyPoolAutoResumeNoop': '自动换本来就没有停止',
  'settings.proxyPoolAutoTagHalted': '已停止自动换',
  'settings.proxyPoolAutoTagNoTarget': '没有可挪的端口',
  'settings.proxyPoolAutoReplaced': '自动换过的端口 {count} 个（已停用、账户已挪走；恢复后账户不回原端口）：',
  'settings.proxyPoolAutoReplacedItem': '{time} · {rule} · 挪走 {moved} 个账户',
  'settings.proxyPoolAutoRuleUpstream': '连续出口连不通',
  'settings.proxyPoolAutoRuleRateLimited': '不同账号返回限流',
  'settings.proxyPoolAutoStuck': '会话连不通、没有自动换的账户 {count} 个：',
  'settings.proxyPoolAutoStuckSince': '会话 {session} · 自 {time}',
  'settings.proxyPoolAutoRotate': '换会话',
  'settings.proxyPoolAutoRotateConfirm':
    '给 {account} 换会话：之后用新的会话号（代理商会给新的 IP），运行中的浏览器在进行中的请求结束后关闭。',
  'settings.proxyPoolAutoRotateDone': '已换会话：{from} → {to}',
  'settings.proxyPoolAutoNotDynamic': '固定会话没有开启（需要动态 IP 且开启固定会话）',
  'settings.proxyPoolAutoNotPooled': '这个账户有自己的代理，不用 IP 池',
  'app.proxyPoolAutoHalted': '坏 IP 过多，已停止自动换 IP，请手动处理',
  'app.proxyPoolAutoHaltedDynamic': '坏会话过多，已停止自动换会话，请手动处理',
  'app.proxyPoolAutoHaltedDetail': '{time} 停止：30 分钟内坏了 {count} 个（上限 {limit}）',
  'app.proxyPoolAutoHaltedDetailHalf': '{time} 停止：30 分钟内坏了 {count} 个，再自动换的话能用的端口会不到一半',
  'app.proxyPoolAutoHaltedDetailChain': '{time} 停止：换过去的新 IP 30 分钟内又坏了（共坏了 {count} 个），像是代理商整体出了问题',
  'app.proxyPoolAutoHaltedDetailChainDynamic': '{time} 停止：换出的新会话 30 分钟内又连不通（共 {count} 个），像是代理商整体出了问题',
  'app.proxyPoolAutoHaltedDetailAccounts': '{time} 停止：30 分钟内 IP 坏掉的账户超过一半（共坏了 {count} 个），像是代理商整体出了问题',
  'app.proxyPoolAutoHaltedDetailAccountsDynamic': '{time} 停止：30 分钟内会话连不通的账户超过一半（共 {count} 个），像是代理商整体出了问题',
  'app.proxyPoolAutoHaltedSkipped': '，停止之后又坏了 {skipped} 个',
  'app.proxyPoolAutoHaltedOpen': '去处理',
  'app.proxyPoolAutoStopped': '已停止自动换 IP',
  'app.proxyPoolAutoStoppedDynamic': '已停止自动换会话',
  'app.proxyPoolDetailAuto': '自动换坏 IP：30 分钟内最多 {limit} 个，最近坏了 {recent} 个，自动换过的端口 {replaced} 个',
  'app.proxyPoolDetailAutoDynamic': '自动换坏会话：30 分钟内最多 {limit} 个，最近连不通 {recent} 个',
  'proxyPoolPause.cooling_upstream': '冷却中（连续出口连不通）',
  'proxyPoolPause.cooling_rate_limited': '冷却中（不同账号返回限流）',
  'proxyPoolPause.cooling': '冷却中',
  'proxyPoolPause.locked_elsewhere': '被其他进程占用',
  'app.proxyPoolFixed': 'IP 池 · 固定 · 每端口 {perPort} · 暂停 {paused}',
  'app.proxyPoolFixedDynamic': 'IP 池（动态）· 会话 {inUse} · 固定会话',
  'app.proxyPoolDetailFixed':
    '固定分配：{ports} 个端口 · {accounts} 个账户 · 每端口 {perPort} 个（同一端口的账户共用这个 IP）· 有浏览器的端口 {inUse}/{total}',
  'app.proxyPoolDetailFixedPaused': '暂停使用：{ports}',
  'app.proxyPoolDetailFixedPausedPort': '{port}（{reason}，{accounts} 个账户）',
  'app.proxyPoolDetailFixedTransitional': '还不在固定端口上的 Worker {count} 个（关闭后改用固定端口）',
  'app.proxyPoolDetailFixedDynamic': '固定会话：{accounts} 个账户各用固定会话号',
  'accounts.proxyPoolFixedSlot': 'IP 池 · 端口 {port}（固定）',
  'accounts.proxyPoolFixedHelp': '固定分配：这个账户的 Worker、登录、验证与后台请求一直用这个端口（同一端口的几个账户共用这个 IP），重启、更新都不变',
  'accounts.proxyPoolFixedPaused': '暂停使用',
  'accounts.proxyPoolFixedTransitionalHelp': '这个浏览器在开启固定分配或挪动之前就在这个出口上运行，关闭后改用固定端口 {port}',
  'proxy.testAccountClearedFixedSlot': '清空账户代理后分到的固定端口，例如：{slot}（账户最少的端口；之后一直用它）',
  'proxy.testPoolFixedSlot': '固定分配下新账户登录此刻会分到：{slot}（账户最少、没有暂停的端口；登录用的端口就是之后的固定端口）',
  'logs.midWarn': '中途有警告',
  'logs.midError': '中途有错误',
  'logs.midHelp': '这个请求中途写过{level}（如换号、冷却），左侧是最终级别；点开看事件时间线',
  'requests.accountFilter': '筛选账户（邮箱或账户 ID）',
  'requests.noMatch': '没有符合筛选的请求',
  'cooldowns.noMatch': '没有符合筛选的冷却',
  'usage.range.quotaDay': '配额日 · {time} 起',
  'usage.range.quotaDayHelp':
    '从最近一次太平洋时间零点起算：Google 每日额度在这时重置，平均调用的今日发送数与额度冷却到次日零点也按这个边界',
  'accounts.deviceBound': '2 小时内失效',
  'accounts.deviceBoundHelp':
    '设备绑定会话（带 __Secure-1PSIDRTS）：复制出来的认证文件约 2 小时后失效、变成需要登录。改用本服务的浏览器登录重新登录；Windows 上也可以用 Chrome 导入（带续签材料，失效时原地续签）',
} as const

export type TranslationKey = keyof typeof zhCN

const en: Record<TranslationKey, string> = {
  'app.title': 'AI Studio Console',
  'app.console': 'Console',
  'app.gateway': 'Protocol gateway',
  'app.online': 'Service online',
  'app.offline': 'Service offline',
  'app.events': 'Live events',
  'app.reconnecting': 'Reconnecting',
  'app.refresh': 'Refresh data',
  'app.version': 'Version',
  'app.status': 'Service status',
  'app.start': 'Start service',
  'app.stop': 'Stop service',
  'app.eventsReconnecting': 'Live connection lost, reconnecting…',
  'app.eventsClosed': 'Live connection closed (the admin token may have expired). Please reload the page.',
  'app.statusUnreachable': 'Cannot reach the service; showing the last known status',
  'app.noticeClose': 'Dismiss notification',
  'app.noticeExpand': 'Show full message',
  'app.noticeCollapse': 'Show less',
  'app.assetReloadBlocked':
    'The management page has been updated and some page files failed to load. There is unsaved input or a save or account operation still in progress (service configuration, accounts or the API playground), so the page was not reloaded automatically: wait for the operation to finish, save or note your input, then reload the browser',
  'nav.logs': 'Logs',
  'nav.accounts': 'Accounts',
  'nav.models': 'Model catalog',
  'nav.requests': 'Cooldowns & requests',
  'nav.usage': 'Usage',
  'nav.settings': 'Service config',
  'nav.playground': 'API playground',
  'section.accounts.title': 'Accounts & login',
  'section.accounts.description': 'Manage login state, account egress, and model availability',
  'section.models.title': 'Live model catalog',
  'section.models.description': 'Models, methods, and capabilities from healthy accounts',
  'section.requests.title': 'Cooldowns & requests',
  'section.requests.description': 'Inspect account model cooldowns and active requests',
  'section.settings.title': 'Service config',
  'section.settings.description': 'Local listener, authentication, proxy, and timeouts',
  'section.playground.title': 'API Playground',
  'section.playground.description': 'Send one live request through a public protocol',
  'metric.readyAccounts': 'Ready accounts',
  'metric.activeRequests': 'Active requests',
  'metric.models': 'Available models',
  'metric.totalAccounts': 'Total accounts',
  'common.loading': 'Loading',
  'common.save': 'Save',
  'common.cancel': 'Cancel',
  'common.close': 'Close',
  'common.delete': 'Delete',
  'common.verify': 'Verify',
  'common.relogin': 'Log in again',
  'common.search': 'Search',
  'common.add': 'Add account',
  'common.edit': 'Edit',
  'common.enable': 'Enable',
  'common.disable': 'Disable',
  'common.empty': 'No data',
  'common.copy': 'Copy',
  'common.copied': 'Copied',
  'common.error': 'Request failed',
  'common.unknown': 'Unknown',
  'accounts.empty': 'No accounts',
  'accounts.emptyHint': 'Log in with a browser or import accounts from Chrome',
  'accounts.proxy': 'Account proxy',
  'accounts.proxyHelp':
    'Leave empty to use the IP pool when it is on (one IP per browser), otherwise the default proxy. "Test proxy" with an empty value: when editing an account it tests the exit the account actually uses now, or, if the account had its own proxy that you cleared here, the exit the IP pool would give it after clearing (the default proxy when the IP pool is off); when adding an account it tests the IP pool exit a login would get right now (the default proxy when the IP pool is off). Same formats as the default proxy; for rotating IPs, enable a sticky session at your provider, otherwise browser pages and background requests may leave from different IPs.',
  'accounts.locale': 'Locale',
  'accounts.timezone': 'Timezone',
  'accounts.models': 'Models',
  'accounts.benefitTier': 'Account tier',
  'accounts.direct': 'Direct',
  'accounts.browserLogin': 'Browser login',
  'accounts.importChrome': 'Import Chrome accounts',
  'accounts.chromeTitle': 'Select Chrome accounts',
  'accounts.chromeEmpty': 'No Chrome accounts are available to import',
  'accounts.importSelected': 'Import selected',
  'accounts.selectAll': 'Select all',
  'accounts.deselectAll': 'Deselect all',
  'accounts.loginComplete': 'Account login complete',
  'accounts.verified': 'Account verified',
  'accounts.verifyInvalid': 'Login expired: {reason}',
  'accounts.verifyOrgDisabled':
    'Verification failed: the organization that manages this account has turned AI Studio off (not an expired login, signing in again does not help; scheduling has stopped)',
  'accounts.verifyOrgDisabledReason':
    'The organization that manages this account has turned AI Studio off (not an expired login, signing in again does not help)',
  'accounts.verifyResult': 'Verification finished: {ok} valid, {invalid} invalid, {failed} failed',
  'accounts.verifyResultOrgDisabled':
    'Verification finished: {ok} valid, {invalid} invalid, {org} disabled or restricted, {failed} failed',
  'accounts.verifyResultSkippedPaused':
    '; skipped {count} accounts whose IP is paused (verify them once their fixed port recovers)',
  'accounts.verifyRestricted':
    'Verification failed: {reason} (not an expired login, signing in again does not help; scheduling has stopped)',
  'accounts.verifyRestrictedReason': '{reason} (not an expired login, signing in again does not help)',
  'accounts.saved': 'Account saved',
  'accounts.deleted': 'Account deleted',
  'accounts.importComplete': 'Imported {count} accounts',
  'accounts.accountExists':
    'This account already exists and was not added again: if the account list shows it as "Login required", use "Log in again" on it; enable it first if it is disabled; in any other state nothing more is needed. If it is not in the list, its folder is already in the auth directory and the directory scan loads it automatically',
  'accounts.accountExistsReady':
    '{email} is already in the pool and was not added again; nothing more is needed',
  'accounts.accountExistsRelogin':
    '{email} is already in the pool and needs login, so it was not added again: use "Log in again" on it in the account list',
  'accounts.accountExistsDisabled':
    '{email} is already in the pool and disabled, so it was not added again: enable it in the account list, then use "Log in again" on it if it needs login',
  'accounts.accountDirectoryExists':
    'The folder for {email} is already in the auth directory, so it was not added again: the directory scan loads it automatically; use "Log in again" on it once loaded if it needs login',
  'accounts.invalidTimezone':
    'Invalid time zone: enter an IANA time zone name such as Asia/Shanghai, without spaces or invisible characters',
  'accounts.transitionDraining':
    'Disabled; the Worker closes after {count} in-flight requests finish',
  'accounts.transitionWaiting':
    'Disabled; the Worker closes after other operations on this account (login, verification, Worker startup, etc.) finish',
  'accounts.transitionClosing': 'Disabled; closing the Worker',
  'accounts.transitionSyncing': 'Enabled; syncing the model catalog',
  'accounts.transitionHelp':
    'The change is already in effect: a disabled account takes no new requests and in-flight requests finish normally. Waiting for them, closing the Worker and syncing the model catalog happen in the background',
  'accounts.chromeExisting': 'Already imported · re-import in place',
  'accounts.chromeExistingHelp':
    'Importing replaces only the cookies and renewal material of this account. Its settings, browser fingerprint, resource bindings and runtime state are kept; the proxy, locale and time zone above apply to new accounts only',
  'accounts.chromeSelectAllHint':
    'Select all picks new accounts and accounts that need to log in again. Check healthy imported accounts individually (re-importing closes the Worker it is serving)',
  'accounts.chromeImportPartial':
    'Imported {count} accounts; {failed} existing accounts were not re-imported: {details}',
  'accounts.editTitle': 'Edit account',
  'accounts.deleteConfirm': 'Delete this account?',
  'logs.level': 'Log level',
  'logs.source': 'Source',
  'logs.allSources': 'All',
  'logs.clear': 'Clear logs',
  'logs.autoScroll': 'Auto scroll',
  'logs.waiting': 'Waiting for logs...',
  'logs.search': 'Search model, request ID, account or message',
  'logs.running': 'Running',
  'logs.tools': 'Tool calls',
  'logs.loadOlder': 'Load older logs ({count})',
  'logs.showingRecent': 'Showing recent {count}',
  'logs.reasoning': 'Reasoning',
  'logs.reply': 'Reply',
  'logs.outputTotal': 'Output total',
  'logs.averageSpeed': 'Average speed',
  'logs.averageSpeedHelp':
    'Output tokens divided by the time from the first upstream chunk to the end of the request: queueing, WAA, waiting for the first chunk and discarded regenerations are not counted; when a tool call is required, timing starts when the first upstream event is held back',
  'logs.averageSpeedTotalHelp':
    'Output tokens divided by the total request time (including queueing and waiting for the first chunk): the generation speed cannot be measured with a single chunk or when thinking is hidden but the usage includes thinking tokens, or the record comes from an older version',
  'logs.details': 'Details',
  'logs.endpoint': 'Endpoint',
  'logs.inputTokens': 'Input usage',
  'logs.totalTokens': 'Total usage',
  'logs.inputMessages': 'Input messages',
  'logs.finishReason': 'Finish reason',
  'logs.downgrade': 'Downgrade check',
  'logs.all': 'ALL',
  'logs.info': 'INFO',
  'logs.warn': 'WARN',
  'logs.error': 'ERROR',
  'models.search': 'Search models, methods, or capabilities',
  'models.allMethods': 'All methods',
  'models.model': 'Model',
  'models.methods': 'Methods',
  'models.capabilities': 'Capabilities',
  'models.context': 'Input limit',
  'models.output': 'Output limit',
  'models.paid': 'Paid',
  'models.empty': 'No models are currently available',
  'requests.live': 'In progress',
  'requests.history': 'Request summary',
  'requests.id': 'Request ID',
  'requests.account': 'Account',
  'requests.model': 'Model',
  'requests.started': 'Started',
  'requests.action': 'Action',
  'requests.stop': 'Stop',
  'requests.empty': 'No current requests',
  'cooldowns.title': 'Model cooldowns',
  'cooldowns.until': 'Available at',
  'cooldowns.empty': 'No accounts are cooling down',
  'accounts.cooldownTitle': 'Model cooldowns',
  'accounts.cooldownSummary': '{accounts} accounts cooling down, {entries} model cooldowns in total',
  'accounts.cooldownEarliestAll': 'first recovery in {time} ({clock})',
  'accounts.cooldownLatestAll': 'all recovered in {time} ({clock})',
  'accounts.cooldownModel': 'Model · channel',
  'accounts.cooldownAccounts': 'Accounts',
  'accounts.cooldownFirst': 'First recovery',
  'accounts.cooldownLast': 'All recovered',
  'accounts.cooldownReason': 'Main reason',
  'accounts.cooldownVerifyExcluded':
    '{count} more accounts paused as a whole for account verification are not counted',
  'accounts.cooldownVerifyExcludedOther':
    '{count} more accounts paused as a whole for account verification are not counted (including {other} that need sign-in, have AI Studio disabled by their organization or are unavailable, which the verification summary leaves out)',
  'accounts.cooldownVerifyExcludedHelp':
    'Counts the accounts whose whole-account pause for account verification is still in effect, whatever their state: none of their cooldown entries (the pause expanded to every model, and per-model cooldowns) count toward the accounts, entries, recovery times or main reasons here. Accounts that need sign-in, have AI Studio disabled by their organization or are unavailable are included in this number but not in the "Account verification required" summary (as under "Problems (403)"), so this number can be larger than the paused accounts in the summary; accounts whose pause has expired are counted in the cooldowns as usual.',
  'accounts.cooldownVerifyModelExcluded':
    '{count} more accounts with a pause for verification on one model are not counted',
  'accounts.cooldownVerifyModelExcludedHelp':
    'A pause for account verification on one model only blocks that model (up to 30 days): these entries do not count toward the accounts, entries, recovery times or main reasons here (the other cooldowns of the account are counted as usual); see "403 verification required on one model".',
  'accounts.cooldownShowAll': 'Show all {count} models',
  'accounts.cooldownShowLess': 'Show less',
  'accounts.cooldownSoon': 'any moment',
  'accounts.cooldownSeconds': '{s}s',
  'accounts.cooldownMinutes': '{m}m {s}s',
  'accounts.cooldownHours': '{h}h {m}m',
  'settings.authPath': 'Account state path',
  'settings.listen': 'Listen address',
  'settings.apiKey': 'Local API key',
  'settings.proxy': 'Default proxy',
  'settings.proxyHelp':
    'Supports http, https and socks5, with optional credentials: http://user:pass@IP:port, socks5://user:pass@IP:port, IP:port:user:pass or user:pass@IP:port; http is assumed when no scheme is given. Saved in URL form with special characters encoded. In the URL and user:pass@ forms, %XX sequences are percent-decoded; to keep a literal sequence such as %41 in the password, use the IP:port:user:pass form. With credentials, the browser connects through a local relay.',
  'proxy.placeholder': 'http://user:pass@1.2.3.4:8080',
  'proxy.editHint': 'Click to show and edit the full proxy (including credentials)',
  'proxy.authTag': 'auth',
  'proxy.test': 'Test proxy',
  'proxy.testing': 'Testing…',
  'proxy.testHint':
    'Queries api.ipify.org through the proxy for the exit IP; Google is not contacted.',
  'proxy.testBrowser': 'Browser path',
  'proxy.testBrowserRelay': 'Browser path (via local relay)',
  'proxy.testDirect': 'Background request path',
  'proxy.testOK': 'Exit IP {ip} · {ms} ms',
  'proxy.testFailed': 'Failed: {error}',
  'proxy.warnRotating':
    'The two connections left from different IPs: this proxy rotates per connection, so browser pages and background requests will use different IPs. Use a sticky session with a long duration.',
  'proxy.warnHttpsPlain':
    'For https:// proxies without credentials, the browser (Camoufox) connects as a plain HTTP proxy, so login and WAA will likely fail; use http:// or a credentialed URL (the local relay handles TLS).',
  'settings.initTimeout': 'Init timeout',
  'settings.requestTimeout': 'Request timeout',
  'settings.requestTimeoutHelp':
    'Deadline before the first upstream event of a generation request: queuing, cold starts, WAA proof, attachment uploads, waiting for the first token and account switches before it all count, and a 504 is returned when it expires. Once output starts it becomes an idle limit: the request ends with 504 only when upstream sends no event and no bytes for this long, so a long reply that keeps streaming is not cut off; a whole generation request is capped at 1 hour (or this value if longer). Token counting, video and transcription requests are limited by it as a whole. Changes apply immediately to new requests only.',
  'settings.firstEventTimeout': 'First event timeout',
  'settings.firstEventTimeoutHelp':
    'How long each attempt waits for its first upstream event after sending upstream. On timeout only that attempt is abandoned and the request retries on another account (no extra cooldown); when no retry is possible a 504 is returned. It restarts for each same-account regeneration before any output reaches the client. 0 or off disables it (default); write whole seconds (60 means 60 seconds, as in upstream Mag1cFall/AIStudio2API) or a duration (90s, 2m); models that think for a long time may take a while to emit their first event, so leave plenty of headroom.',
  'settings.streamDataHeartbeat': 'Data stream heartbeat',
  'settings.streamDataHeartbeatHelp':
    'Write an empty data chunk after every 10 seconds without output (an empty text or empty thought candidate for Gemini SSE, an empty delta chunk for OpenAI Chat), and one more when the streaming response header is sent before the first event, instead of only a newline or an SSE comment. Relays such as new-api forward only data lines, so with this on the clients behind them keep receiving bytes while waiting for the first token or during long thinking and are not dropped by the phone OS, a VPN or a proxy after a long silence. Official Gemini SDKs connecting directly still get newline heartbeats (no relay in between, and no extra chunk the SDK treats as invalid); with this on, new-api reports the first heartbeat as the first-response time, and bills the estimated input tokens when the client disconnects or the stream is cut without usage data (0 before). When off, behavior is exactly as before. Changes apply immediately to new requests only.',
  'settings.warmWorkerLimit': 'Resident workers',
  'settings.maxActiveWorkers': 'Active worker limit',
  'settings.warmStartupConcurrency': 'Startup prewarm concurrency',
  'settings.perAccountConcurrency': 'Concurrent requests per account',
  'settings.proTitle': 'Pro pool',
  'settings.proHelp':
    'Accounts with the Pro tier form the Pro pool (while "Separate Pro pool" below is on). Point a client at the /pro prefix (OpenAI: http://host:port/pro/v1; Anthropic and Gemini: http://host:port/pro) to have its requests served only by Pro-pool accounts; /pro offers every endpoint of the default URL, and its trace route is /trace/pro. Changes apply immediately without a service restart.',
  'settings.proPool': 'Separate Pro pool',
  'settings.proPoolHelp':
    'When on (default), accounts with the Pro tier only serve the /pro prefix: neither the default URL nor /ultra uses them any more ("Put Pro accounts in the Ultra pool" in the Ultra pool below has no effect), and /pro never borrows Ultra or normal-pool accounts: it returns 503 when no Pro-pool account is ready, and 429 with Retry-After when every candidate is cooling down, out of quota or the queue is full. The exception is the extra pool: /pro also borrows extra-pool members of other pools, and Pro members in the extra pool are lent to the default URL and /ultra too (tier rules still apply). Pro cannot use Ultra-only models (gemini-4-carbon is not in the /pro model list and requests for it return 404); this relies on "Ultra-only models" (ULTRA_ONLY_MODELS) below, and clearing that list lets the Pro pool pick carbon too. gemini-4-argon and gemini-3.1-pro-preview work as usual. /pro follows the Ultra/Pro primary model list and queue limit. Resources do not move: files and video jobs that already live on Pro accounts must be referenced through /pro (otherwise the request returns 400 naming the right entry point). When off, behaviour is exactly as before this setting existed (Pro accounts stay on the default URL, or join /ultra when "Put Pro accounts in the Ultra pool" is on), new /pro requests return 404, and /pro requests still in flight during the switch return 503. Changes apply immediately; running workers are recounted in their new partition instead of being restarted. Set the two worker counts below in the same save, otherwise surplus Pro workers are reaped gradually.',
  'settings.proWarmWorkerLimit': 'Pro resident workers',
  'settings.proMaxActiveWorkers': 'Pro active worker limit',
  'settings.proWorkersHelp':
    'Workers of Pro-pool accounts are counted on their own: prewarming, idle reaping and eviction stay inside the Pro partition and never take workers from the normal or Ultra pool, nor the other way round. The defaults of 2 and 5 are small; if the normal or Ultra partition carried the Pro traffic before, move part of its workers here and keep the total number of browsers unchanged. The resident count may be 0 (start on demand only); the limit must be at least 1 and not lower than the resident count.',
  'settings.workerPartitionHelp':
    'Resident workers and the active worker limit apply to the normal pool only (Pro and Ultra excluded). Pro-pool and Ultra-pool workers are configured separately below, so the total number of browsers can reach the sum of the three partition limits, plus the extra pool limit while the extra pool is on (the Pro partition is empty while the separate Pro pool is off or there are no Pro accounts). Startup prewarm concurrency and per-account concurrency are shared by all pools, and cold starts get prewarm slots in the order normal, Ultra, Pro. Per-account concurrency is a hard cap: an account never has more requests in flight than this (every request type, streaming or not, Playground or Build, countTokens and file operations included). Changes apply immediately; lowering it lets requests already in flight finish.',
  'settings.extraTitle': 'Extra pool (additional browser pool)',
  'settings.extraHelp':
    'Accounts that used up one model today while other models still work (for example gemini-4-argon is out of daily quota but flash is fine) get browsers of their own that help the default URL, /pro and /ultra. The pool an account belongs to does not change: accounts with nothing cooling down stay in their own pool, and accounts that need a login, are disabled, are cooling down as a whole or have every bootstrap model cooling down never join. Off by default; while off everything behaves exactly as before this setting existed. Changes apply immediately without a service restart.',
  'settings.extraPool': 'Enable the extra pool',
  'settings.extraPoolHelp':
    'An account becomes a member when a model has at least 15 minutes of cooldown left and some other model still works; it leaves when the cooldown ends (many end together when quotas reset at midnight Pacific time), when the whole account becomes unusable or when its tier is out of scope: its worker is handed back in place if its home partition has room, or closed once idle. Short cooldowns such as per-minute limits and a verification required on a single model do not count. Free (including Plus and tiers not read yet, counted as Free) and Pro accounts may join; Ultra depends on the switch below. While on, the extra pool prewarms up to its resident count and takes over idle member workers in place (without closing the browser) when a main partition lets them go.',
  'settings.extraPoolMode': 'Mode',
  'settings.extraModeShare': 'Share the load (default)',
  'settings.extraModeOverflow': 'Only when the main pool runs short',
  'settings.extraModeHelp':
    'Share the load: ready extra-pool workers take turns with the main pool workers. Only when the main pool runs short: the extra pool is borrowed at once when every running main-pool worker is busy, every candidate is cooling down or only started workers may be used after a quota 429; after 3 seconds of queueing while a main-pool worker is starting or egress is rate limited; while the main pool can still cold-start a spare account, the request cold-starts it first (about 10–30 seconds) and does not borrow meanwhile. Either way the extra pool only serves models the entry point already offers (except the paid models of the default URL below).',
  'settings.extraDefaultPaid': 'Default URL may borrow paid accounts',
  'settings.extraDefaultPaidHelp':
    'When on (default), the default URL may borrow Pro accounts of the extra pool (and Ultra accounts while the Ultra switch below is on) for models outside the default URL model list, such as gemini-4-argon and the models in PAID_ONLY_MODELS, as long as that account still has quota for the model: only Pro (or Ultra) members whose other models are cooling down while this paid model still works can be borrowed, so a Pro account that used up gemini-4-argon is lent to the default URL for flash and similar models, not for argon, and healthy Pro accounts with nothing cooling down stay on /pro. These models then appear in the default URL /v1/models and can only be served by the extra pool: when every account within the scope of the extra pool that can run one is cooling down the request returns 429 with Retry-After; without such a member, or when no worker can be borrowed right now, the model stays listed and the request returns 503. Use /pro for steady access to gemini-4-argon. Tier rules still apply: Free never runs paid models and Pro never runs Ultra-only models. When off, each entry point is only helped with the models it already offers; /pro and /ultra are not affected by this switch.',
  'settings.extraIncludeUltra': 'Include Ultra accounts',
  'settings.extraIncludeUltraHelp':
    'Off by default: accounts with the Ultra tier never become members. When on, Ultra accounts with a cooling model also help the default URL and /pro (only with models those entry points allow; with the paid switch above on, the default URL may also borrow them for Ultra-only models). Borrowing Ultra accounts spends tokens of the Ultra egress lane and may squeeze /ultra.',
  'settings.extraWarmWorkerLimit': 'Extra pool resident workers',
  'settings.extraMaxActiveWorkers': 'Extra pool active worker limit',
  'settings.extraWorkersHelp':
    'Extra-pool workers are counted on their own and never take slots of the normal, Pro or Ultra partitions. The resident count (default 40) is what prewarming and takeovers fill up to and may be 0 (expand on demand only); the limit (default 50) must be at least 1 and not lower than the resident count, and workers above the resident count are reaped one by one after 5 idle minutes. Extra-pool prewarming comes after the main partitions and shares the startup prewarm concurrency. Each browser takes about 0.6 GB of memory: 40 resident take about 24 GB, and 50 plus the 4 started during replacements about 32 GB; with the IP pool on you need "limit + replacement headroom" more ports (54 by default); with many browsers keep the open-file limit of the process at 65536 or more (ulimit -n).',
  'settings.extraResourceHint':
    'With the current limit the extra pool runs at most {browsers} browsers at once ({max} + {drain} replacements), about {memory} GB more memory, and {browsers} more ports while the IP pool is on.',
  'settings.extraOffNote':
    'The extra pool is off: the settings below are saved and take effect once it is on.',
  'settings.invalidExtraWarmWorkers': 'Extra pool resident workers must be 0 or a positive integer',
  'settings.invalidExtraMaxWorkers':
    'The extra pool active worker limit must be a positive integer and not lower than its resident workers',
  'settings.accountRpmLimit': 'Requests per minute per account',
  'settings.accountRpmLimitHelp':
    'The most generation requests each account sends in the last 60 seconds (upstream allows 12 per account per minute, shared by all models and both channels). An account that reaches it counts as busy and requests go to other accounts; when every running worker is full a new worker starts. Only requests from this service are counted, and upstream 429 cooldowns still act as the fallback. Defaults to 10, which leaves headroom below 12; 0 turns it off. Changes apply immediately.',
  'settings.ultraTitle': 'Ultra pool',
  'settings.ultraHelp':
    'Accounts with the Ultra tier form the Ultra pool (the paid pool); with "Separate Pro pool" above off and "Put Pro accounts in the Ultra pool" below on, accounts with the Pro tier belong to it too. Point a client at the /ultra prefix (OpenAI: http://host:port/ultra/v1; Anthropic and Gemini: http://host:port/ultra) to have its requests served only by Ultra-pool accounts. Changes apply immediately without a service restart.',
  'settings.ultraExclusive': 'Ultra-pool accounts only serve /ultra requests',
  'settings.ultraExclusiveHelp':
    'When on (default), normal paths only use the other accounts and Ultra-pool accounts are kept for /ultra. When off, normal paths may also use Ultra-pool accounts; /ultra still uses Ultra-pool accounts only. With the separate Pro pool on, normal paths still never use Pro accounts, so paid models such as gemini-4-argon stay available on the default URL through Ultra accounts instead of only on /pro and /ultra. With the extra pool on, every entry point also borrows extra-pool members of other pools (tier rules still apply).',
  'settings.ultraIncludePro': 'Put Pro accounts in the Ultra pool (paid pool)',
  'settings.ultraIncludeProHelp':
    'Only takes effect while "Separate Pro pool" above is off (PRO_POOL=false). When on, accounts with the Pro tier join Ultra accounts in the Ultra pool and are scheduled by every Ultra-pool setting: they serve the /ultra prefix and appear in its model list, follow "Ultra-pool accounts only serve /ultra requests" above, count toward the Ultra resident and active worker limits below, follow the Ultra primary model list, and count toward the Ultra-pool figures in the sidebar and on the accounts page. Their real tier does not change: the accounts page still shows Pro (with an Ultra pool tag), and model eligibility, input limits and quota sharing still follow Pro. While the setting above is on, normal paths no longer use Pro accounts, the normal pool shrinks accordingly, and gemini-4-argon and the models that need a subscription disappear from the normal-path model list (/v1/models), so clients have to switch to the /ultra prefix. Pro and Ultra share the two worker limits below, whose defaults are small, so raise them; with 1 Ultra resident worker (ULTRA_WARM_WORKER_LIMIT=1) the resident one may be a Pro account rather than an Ultra account, so keep it at 2 or more. Resources do not move, so files and video jobs that already live on a Pro account may need a different entry point after a switch: after turning this on, referencing them through normal paths returns 400 while the setting above is on, so use the /ultra prefix instead (nothing changes while it is off); after turning this off, referencing them through /ultra returns 400, so use normal paths instead. Switch while no video job is in progress. Off by default; changes apply immediately without a service restart, and running workers are recounted in their new partition instead of being restarted.',
  'settings.ultraIncludeProSharedHint':
    '"Ultra-pool accounts only serve /ultra requests" (ULTRA_EXCLUSIVE) above is off, so normal paths still use Pro and Ultra accounts, and Pro accounts in the Ultra pool keep serving normal-path requests. Turn that setting on as well to keep Pro accounts for /ultra requests only.',
  'settings.ultraIncludeProIgnoredHint':
    'This has no effect while the separate Pro pool (PRO_POOL) is on: Pro accounts only serve /pro. Turn the separate Pro pool off to put Pro accounts in the Ultra pool by this setting.',
  'settings.ultraOnlyModels': 'Ultra-only models',
  'settings.ultraOnlyModelsHelp':
    'Models in this list are only given to accounts with the Ultra tier: neither the Pro pool nor Pro accounts put in the Ultra pool are picked for them or count towards their prewarming or supply. Keeping gemini-4-carbon off /pro relies on this list alone; removing carbon from it lets /pro use it (the service logs a WARN). Comma-separated model IDs, default gemini-4-carbon; clear the list to remove the rule.',
  'settings.paidOnlyModels': 'Paid-only models (Pro and Ultra)',
  'settings.paidOnlyModelsHelp':
    'Models in this list are only given to accounts with the Pro or Ultra tier; Free and Plus accounts are never picked for them. Comma-separated model IDs, default gemini-3.1-pro-preview; clear the list to remove the rule.',
  'settings.tierRulesHelp':
    'Account model catalogs do not always mark which models are paid-only or Ultra-only, so these two lists add that layer. They only narrow, never widen (a model the catalog itself restricts stays restricted), use the account\'s real tier, and treat accounts whose tier has not been read yet as Free until it is read. Write the base model ID (for example gemini-4-carbon, without suffixes such as -high or -nothinking and without the long- prefix); it covers every suffixed name and catalog alias, case-insensitively. A model no account in the pool can use disappears from the model list and requests for it return 404 model not available. Remove a model here once Google opens it up: a highlighted hint appears under the matching list when the catalogs mark an excluded tier as allowed; when the catalogs merely do not restrict it, a plain note there lists the accounts the rule keeps off it, which is no reason to remove it. Changes apply immediately and do not affect requests in progress.',
  'settings.tierRuleHintMarked':
    '{tier} account catalogs ({count}) mark {model} as available. Once you confirm {tier} accounts can use it, remove it here.',
  'settings.tierRuleHintUnmarked':
    '{model}: {tiers} account catalogs do not restrict it by tier, so only this rule keeps those accounts off it. An unmarked catalog does not mean they can use it, so this is no reason to remove it.',
  'settings.tierRuleHintTier': '{tier} ({count})',
  'settings.tierRuleHintTierSeparator': ', ',
  'settings.ultraWarmWorkerLimit': 'Ultra resident workers',
  'settings.ultraMaxActiveWorkers': 'Ultra active worker limit',
  'settings.ultraWorkersHelp':
    'Workers of Ultra-pool accounts are counted separately from the normal pool: prewarming, idle reaping and eviction stay inside the Ultra partition and never take workers from the normal pool. With the separate Pro pool off and "Put Pro accounts in the Ultra pool" on, workers of Pro accounts count here too (with the separate Pro pool on they count in the Pro partition above). The resident count may be 0 (start on demand only); the limit must be at least 1 and not lower than the resident count.',
  'settings.routingStrategy': 'Account selection strategy',
  'settings.routingRoundRobin': 'Round robin',
  'settings.routingFillFirst': 'Fill first',
  'settings.routingBalanced': 'Balanced',
  'settings.routingHelp':
    'Round robin rotates requests. Fill first keeps using the first available account until it is unavailable or reaches its concurrency limit. Balanced spreads heavy traffic as evenly as possible over all accounts by how many requests each account has sent today (Pacific time, the day the daily quota resets on): running workers without a request in flight get new requests first, prewarming, replacements and expansion start the accounts used least today, and a running account that has sent 50 more requests than the spare taking its place (and has served for at least 5 minutes) is rotated out start-then-stop: the replacement starts in the background first and the old account closes after finishing its requests, so no request is interrupted. The cost is more browser cold starts (by default at most one rotation per partition per minute, two with a peak of 20 or more; paused while an egress is rate limited). A partition with a single drain slot (the default Ultra partition, or a normal partition whose peak is below 8) only rotates idle accounts and hardly rotates at all under heavy load; raise the peak of that partition to 8 or more for it to rotate. Only as many accounts as the active worker limit are online at once; the rest take turns. Changes apply immediately, and switching back to round robin stops it immediately.',
  'settings.waaBackend': 'WAA backend',
  'settings.waaBackendCamoufox': 'Camoufox browser',
  'settings.waaBackendGo': 'Pure Go',
  'settings.waaBackendHelp':
    'Camoufox runs WAA in a background browser page. Pure Go runs WAA inside the service process and neither downloads nor starts Camoufox. Account login always needs a browser. Restart the generation service to apply a change.',
  'settings.upstreamChannels': 'Upstream channels',
  'settings.upstreamChannelsHelp':
    'Playground and Build are two independent quotas of the same account. With both enabled, requests are scheduled per account and channel, and the other channel continues when one cools down. Enable at least one. When AI Studio disables generation through the Build channel (PROXY_GENERATION_DISABLED), Build is paused automatically, generation uses Playground, and recovery is probed periodically. Changes apply immediately.',
  'settings.buildChannelNormal': 'Build generation: normal',
  'settings.buildChannelPaused':
    'Build generation: disabled by AI Studio (PROXY_GENERATION_DISABLED), paused; generation uses Playground | since {since} | next probe {next}',
  'settings.buildChannelProbing': ' (probing)',
  'settings.buildChannelAccountsPaused':
    'Build generation: {count} account(s) paused (AI Studio disabled Build generation for them; they use Playground), other accounts as usual',
  'settings.buildChannelAwaitingMore':
    '; the pause of {count} more account(s) has expired, awaiting a probe',
  'settings.buildChannelAwaiting':
    'Build generation: the pause of {count} account(s) has expired, awaiting a probe (their next request on Build clears it on success or pauses them again)',
  'settings.streamPlaygroundModels': 'Models that stream via Playground first',
  'settings.streamPlaygroundModelsHelp':
    'Models that need a subscription return the whole reply at once on the Build channel, so streamed output appears all at once at the end. Streaming requests for the listed models use Playground first (streamed chunk by chunk) and switch to Build immediately when every Playground account is busy, without waiting for Playground. Comma-separated model IDs; leave empty to let Playground and Build rotate as usual (default). Non-streaming requests are not affected, and models covered by the downgrade guard follow the downgrade guard. Changes apply immediately.',
  'settings.streamPlaygroundModelsHint':
    'Catalog models that need a subscription and are available on Playground (click to add):',
  'settings.primaryTitle': 'Primary models',
  'settings.primaryModels': 'Primary model list',
  'settings.primaryQueueWait': 'Queue limit',
  'settings.primaryHelp':
    'This list applies to the normal pool only (requests without the /pro or /ultra prefix); /pro and /ultra requests use the Ultra/Pro primary model list below. Requests for the listed models wait at most this long for a free account or worker, then get a 429 with Retry-After so the client retries later instead of queuing here until the request timeout. Concurrent generations are roughly the active worker limit × concurrent requests per account; beyond that, requests queue, and generations per minute never exceed the number of workers × requests per minute per account. This only treats the symptom: when capacity is short for long, add workers; per-account concurrency is bound by the per-minute request limit of each account, so do not raise it. Comma-separated model IDs; empty keeps every model queuing as usual (default). A limit of 0 never queues and returns 429 when nothing is free right away; the limit is shared by the whole request and counts wall-clock time, including retries on another account and their backoff (the browser start-up of a cold start the request runs for itself does not count), and it cannot exceed the request timeout. When every candidate is cooling down the usual rules still apply. Changes apply immediately.',
  'settings.ultraPrimaryModels': 'Ultra/Pro primary model list (/ultra and /pro)',
  'settings.ultraPrimaryQueueWait': 'Ultra/Pro queue limit',
  'settings.ultraPrimaryHelp':
    'The Ultra/Pro primary model list applies to /ultra and /pro requests (the Pro pool skips Ultra-only models). When "Ultra-pool accounts only serve /ultra requests" is off, models on normal paths that are not in the normal list above but are in this list (usually models only Ultra-pool accounts can serve) follow these rules too. When these models are out of quota the request gets a 429 with Retry-After right away instead of switching accounts inside the service over and over: it returns at once when every candidate account is cooling down, and after a quota 429 during a request it switches accounts at most 2 more times when the daily quota is used up and at most 5 more times for per-minute limits and rate limits; the switch right after each quota failure uses only workers that are already running and never cold-starts a standby account for it. The queue limit only bounds the wait for a free account or worker and follows the same rules as above (0 never queues; it cannot exceed the request timeout). Comma-separated model IDs; defaults to gemini-4-argon; clear it to turn this off. Changes apply immediately.',
  'channel.playground': 'Playground',
  'channel.build': 'Build',
  'logs.channel': 'Channel',
  'logs.pool': 'Pool',
  'logs.fallback': 'Fallback',
  'logs.fallbackHelp':
    'Model {model} ran out of quota; MODEL_FALLBACKS switched the request to this model',
  'settings.modelFallbackTitle': 'Switch models when quota runs out',
  'settings.modelFallbacks': 'Fallback rules',
  'settings.modelFallbackScope': 'Scope',
  'settings.modelFallbackScopeUltra': 'Only /ultra requests (not /pro)',
  'settings.modelFallbackScopeAll': 'Default URL, /pro and /ultra',
  'settings.modelFallbackScopeOff': 'Off',
  'settings.modelFallbackHelp':
    'When the requested model has no quota left in this pool (every candidate account is cooling down, the quota fail-fast returns, the last account switch still gets a daily or per-minute quota 429, or the account cooled down before sending) and nothing has been sent to the client yet, the request switches to another model by these rules, and the response reports the model actually used. Daily limits are counted per model, so gemini-4-carbon often still works when gemini-4-argon is used up. Write each rule as source=target1|target2 and separate rules with commas; targets are tried in order, and the rule of a target is not applied again. Use catalog model IDs (catalog aliases also work); suffixes and the long- prefix in the requested name still apply. A target must be in the model catalog of this pool and support the tools the request uses, and is skipped otherwise or when it is under the downgrade guard. After switching, the thinking level, output limits, automatic continuation and queuing follow the target model, and thought signatures in the history issued by other models are replaced with the skip-validation placeholder or dropped (signatures the target issued while switched are kept). A full pool queue, invalid arguments, blocked content, upstream 5xx errors and timeouts do not switch; requests pinned to an account or carrying files do not switch either. When every target is unavailable too, the 429 of the original model is returned with Retry-After set to the earliest suggested retry among the original model and the targets. Scope: only /ultra requests (default; /pro is not included), the default URL, /pro and /ultra (choose this to cover /pro), or off. Defaults to gemini-4-argon=gemini-4-carbon; clear the rules or choose off to turn this off. Changes apply immediately.',
  'settings.invalidModelFallbacks':
    'Invalid fallback rules: write each rule as source=target (separate several targets with | and rules with commas); the source and a target cannot be the same, each source model can have only one rule, targets in a rule cannot repeat, and model names cannot be empty or contain spaces',
  'pool.ultra': 'Ultra',
  'pool.normal': 'Normal pool',
  'pool.ultraHelp': 'Sent through the /ultra prefix and served only by Ultra-pool accounts',
  'pool.pro': 'Pro',
  'pool.proHelp': 'Sent through the /pro prefix and served only by Pro-pool accounts',
  'pool.extra': 'Extra',
  'pool.extraHelp':
    'Served by the extra pool: the last attempt borrowed an extra-pool worker (the pool tag still follows the entry point)',
  'models.channels': 'Channels',
  'settings.mediaTitle': 'Generated media output',
  'settings.mediaHelp':
    'How video and audio appear in replies from Chat Completions, Messages and the native Gemini API (generateContent, streamGenerateContent, including the /pro and /ultra prefixes). Images (image model output) are always returned inline as Base64 like the official API and are not affected. Responses, Interactions, Live and other APIs are unchanged. Changes apply immediately.',
  'settings.mediaOutput': 'Output',
  'settings.mediaOutputInline': 'Inline data URI / inlineData (default)',
  'settings.mediaOutputURL': 'Link to this service (saved under runtime/media/)',
  'settings.mediaOutputHelp':
    'Choose links when clients go through new-api or similar gateways, or when generating video: those gateways drop video and audio when converting Gemini replies to the OpenAI format, and data URIs of several MB are hard to use. Link mode applies to video and audio only: they are saved by content and the reply carries a Markdown link (video [video](…), audio [audio](…)); the native Gemini API also adds a fileData part. Images stay inline. /media/ links do not check the API key, so anyone with a link can open it.',
  'settings.mediaBaseURL': 'Media link address',
  'settings.mediaBaseURLHelp':
    'Service address used in links. Enter the public address users can reach, such as https://ai.example.com (a path prefix is allowed). It is required behind new-api, nginx and other proxies, and the reverse proxy must pass /media/ through. When empty, the address of each request is used (X-Forwarded-Proto, X-Forwarded-Host or Host), which behind a gateway is usually an internal address users cannot open.',
  'settings.mediaRetention': 'Retention',
  'settings.mediaMaxSize': 'Total size limit',
  'settings.mediaCleanupHelp':
    'runtime/media/ is cleaned every hour: files older than the retention are deleted (such as 7d or 72h; 0 keeps them, otherwise at least 1 hour), and when the total exceeds the limit the oldest files go first (such as 20GB or 500MB; 0 means no limit, otherwise at least 100MB; files from the last 10 minutes are kept). Only media files named by content hash are deleted; their links then return 404.',
  'settings.invalidMediaBaseURL':
    'Media link address must be an http or https address without credentials, query or fragment',
  'settings.invalidMediaRetention':
    'Media retention must be 0 or at least 1 hour, such as 7d or 72h',
  'settings.invalidMediaMaxSize':
    'Media size limit must be 0 or at least 100MB, such as 20GB or 500MB',
  'settings.omniIgnoreSystemPrompt': 'Ignore system prompts for video models',
  'settings.omniIgnoreSystemPromptHelp':
    'Requests to Omni models that output video (such as gemini-omni-1.1-flash, including suffixed names and catalog aliases) drop the client system prompt (system, systemInstruction, instructions, system_instruction and system-role messages in every protocol) instead of sending it upstream, and the request log records how many characters were dropped. On by default: chat clients such as Chatbox add a system prompt like "You are a helpful assistant." to every request, and with it Omni often replies with text only, saying it cannot generate videos. While on, system prompts set in the client (such as a Chatbox persona) have no effect on Omni, so put style instructions in the user message; a conversation that was already refused before may keep replying with text only, so start a new one. When off, system prompts are sent as usual (as for other models); this is a global setting that applies to every client, and Chatbox-style requests go back to text-only. A system prompt is kept when the request has no other messages; text, image, Veo, Lyria and other models are not affected. Changes apply immediately to new requests only.',
  'settings.omniImageBinding': 'Include images in the check value of Omni image requests',
  'settings.omniImageBindingHelp':
    'Requests to Omni models (such as gemini-omni-1.1-flash) with inline images include those images in the WAA check value the same way text models do (the Base64 of the original image data, at its position in the request); videos and other files uploaded to Drive are not included, and text-only requests and other models are not affected. On by default: in production, requests with images were refused with 403 (no permission) on several accounts while text-only requests worked, which suggests the website includes images in the check value. When off, only the check value goes back to text only (as before); whatever this setting, when a request with images is refused with 403, accounts are not cooled down or listed, at most 2 accounts are tried, and the request ends with a 400 explanation (remove the images and retry). Changes apply immediately to new requests only.',
  'settings.temporaryChat': 'Use temporary chat for WAA prewarming',
  'settings.ignoreClientSeed': 'Ignore seed sent by clients',
  'settings.ignoreClientSeedHelp':
    'With a fixed seed the same prompt returns nearly identical replies (regenerating too). When enabled the client seed is not sent upstream; request log parameters show whether the client sent a seed.',
  'settings.repeatPromptNonce': 'Add an invisible random suffix to requests',
  'settings.repeatPromptNonceHelp':
    'The upstream returns the exact same reply for identical or near-identical requests, even with a different seed or a higher temperature. When enabled, invisible zero-width characters are appended to the last user message so every generation differs. Requests with a client seed or temperature 0 (reproducibility) and tool-result turns are left unchanged.',
  'settings.abnormalFinishError': 'Return 502 when regeneration still ends abnormally',
  'settings.abnormalFinishErrorHelp':
    'When the upstream ends with MALFORMED_FUNCTION_CALL, UNEXPECTED_TOOL_CALL or MALFORMED_RESPONSE before the client has received any content, the service first regenerates on the same account (at most 2 times per request). Off by default: if the response still ends this way after regeneration is used up (or cannot be used, for example when the request has no random suffix), an empty reply is returned with the finish reason unchanged, as the upstream gave it. When on, a 502 with an English explanation (naming the finish reason and how many times it was generated) is returned instead, so clients and relays such as new-api can retry it as a gateway error (for streaming requests each regeneration restarts the wait before the response headers are written, capped at 50 seconds in total; when the first generation alone exceeds that wait (10 seconds by default) or the total exceeds 50 seconds, the headers are already sent and the request ends with an in-stream error that relays usually do not retry). Replies that already produced text, thinking or function calls are not affected, and the native Gemini API keeps the finish reason in the official shape. It is not treated as an account problem: no cooldown, no account switch and no model fallback; usage is still recorded in the usage ledger. Changes apply immediately to new requests only.',
  'settings.minOutputTokens': 'Minimum max output tokens',
  'settings.minOutputTokensHelp':
    'When a client sets max output tokens below this value it is raised to it (capped at the model limit), so thinking does not use up the budget and truncate or empty the reply. Requests without a value use the model limit. Image and speech models are not changed; 0 disables.',
  'settings.longOutputEnabled': 'Continue automatically for long- model names',
  'settings.longOutputEnabledHelp':
    'When the model name starts with long- (such as long-gemini-4-argon or long-gemini-4-argon-low), output that reaches the per-round limit continues automatically in further rounds on the same account, and the client receives one continuous reply. Models with official automatic continuation continue in rounds as AI Studio does; other models get the text written so far sent back as a model message and continue from there. Every round uses one request of quota and takes longer. Playground channel only; when disabled the long- prefix is still recognized but the request is handled normally. Applies immediately after saving.',
  'settings.longOutputMaxTokens': 'Default total limit for continuation (tokens)',
  'settings.longOutputMaxTokensHelp':
    'When a long- request does not set max output tokens, models without official continuation output at most this many tokens (text and thinking combined; default 262144, about 4 times a common per-round limit); models with official continuation use the official total (about 1 million). A max output set by the client takes precedence. 0 disables simulated continuation. The number of rounds is about the total divided by the per-round limit of the model: at most about 4 rounds at 65536 per round, more for models with smaller per-round limits (at most 64 rounds per request); every round uses one request of quota and counts toward the per-account per-minute limit. Simulated continuation sends the text written so far back upstream, so accounts with a 131072-token input limit cannot reach larger values and end with max_tokens once it is exceeded.',
  'settings.invalidLongOutputMaxTokens':
    'Default total limit for continuation must be an integer from 0 to 1048576',
  'settings.activeValue': 'Active',
  'settings.pendingService': 'Saved values apply the next time the generation service starts',
  'settings.pendingManagement':
    'Restart the management process (./start.sh restart or start.bat) to apply the listen address',
  'settings.invalidConfigTitle':
    'These settings have invalid values and are running with their defaults',
  'settings.invalidConfigEnvironment':
    'set by a process environment variable; change it where the service is started',
  'settings.invalidConfigHint':
    'Edit and save them here to write valid values. REQUEST_BODY_LOG is not edited on this page: saving other settings keeps its .env line as it is, so fix it in .env. An invalid proxy (PROXY, PROXY_POOL_*), WAA_BACKEND, UPSTREAM_CHANNELS, TEMPORARY_CHAT or AUTO_START stops the service from starting instead, so it never appears here. Credential values are not shown.',
  'settings.saved': 'Configuration saved',
  'settings.savedService':
    'Configuration saved. Stop and start the generation service at bottom left.',
  'settings.savedManagement':
    'Configuration saved. Restart the management process (./start.sh restart or start.bat).',
  'settings.savedBoth':
    'Configuration saved. Restart the generation service; restart the management process for the listen address or API key.',
  'settings.reveal': 'Reveal key',
  'settings.hide': 'Hide key',
  'playground.protocol': 'Protocol',
  'playground.mode': 'Output type',
  'playground.modeText': 'Text',
  'playground.modeImage': 'Image',
  'playground.modeSpeech': 'Speech',
  'playground.modeMusic': 'Lyria music',
  'playground.model': 'Model',
  'playground.system': 'System instruction',
  'playground.prompt': 'User message',
  'playground.stream': 'Stream response',
  'playground.ultraHelp': 'Requests go through the /ultra prefix and are served only by Ultra-pool accounts',
  'playground.ultraNoModels': 'No models known for Ultra-pool accounts yet; showing all models',
  'playground.pool': 'Pool',
  'playground.poolNormal': 'Default',
  'playground.poolPro': 'Pro',
  'playground.poolUltra': 'Ultra',
  'playground.poolNormalHelp':
    'Requests go through the default URL and are served only by normal-pool accounts (Free, Plus; with the extra pool on, extra-pool members of other pools may be borrowed too); the list only shows models they can use, choose Pro or Ultra for models that need those tiers',
  'playground.poolNormalSharedHelp':
    'Requests go through the default URL and are served by normal-pool and Ultra-pool accounts (no Pro accounts, ULTRA_EXCLUSIVE=false)',
  'playground.poolNormalProOffHelp':
    'Requests go through the default URL; the separate Pro pool is off, so Pro cannot be chosen',
  'playground.poolProHelp':
    'Requests go through the /pro prefix and are served only by Pro-pool accounts',
  'playground.poolProNoModels': 'No models known for Pro-pool accounts yet; showing all models',
  'playground.reasoning': 'Reasoning effort',
  'playground.reasoningOff': 'Default',
  'playground.reasoningLow': 'Low',
  'playground.reasoningMedium': 'Medium',
  'playground.reasoningHigh': 'High',
  'playground.reasoningExtraHigh': 'Extra high (else High)',
  'playground.tool': 'Tool',
  'playground.toolOff': 'None',
  'playground.imageSize': 'Image size',
  'playground.imageQuality': 'Image quality',
  'playground.voice': 'Voice',
  'playground.reasoningOutput': 'Reasoning',
  'playground.toolOutput': 'Tool calls',
  'playground.clear': 'Clear conversation',
  'playground.send': 'Send request',
  'playground.stop': 'Stop generation',
  'playground.output': 'Output',
  'playground.raw': 'Raw response',
  'playground.placeholder': 'Enter a message',
  'playground.systemPlaceholder': 'Optional system instruction',
  'playground.waiting': 'Waiting for a request',
  'playground.elapsed': 'Elapsed',
  'playground.status': 'HTTP status',
  'playground.waitingResponse': 'Waiting for response {seconds}s',
  'playground.generating': 'Generating',
  'playground.stopped': 'Stopped',
  'playground.errorTitle': 'Request failed',
  'playground.errorInStream': 'Failed after connecting',
  'playground.errorCode': 'Error code',
  'playground.retryAfter': 'Retry in about {seconds}s',
  'playground.retry': 'Retry',
  'playground.streamIncomplete': 'The response stream ended before the protocol completion event',
  'playground.streamInterrupted': 'The connection was interrupted before the response finished: {reason}',
  'playground.emptyThinkingOnly': 'Reasoning only, no reply text (finish reason {reason})',
  'playground.emptyReply': 'The model returned no reply text (finish reason {reason})',
  'playground.finishUnknown': 'unknown',
  'state.ready': 'Ready',
  'state.busy': 'Busy',
  'state.cooldown': 'Cooldown',
  'state.auth_required': 'Login required',
  'state.unavailable': 'Unavailable',
  'state.disabled': 'Disabled',
  'state.org_disabled': 'Disabled or restricted',
  'state.proxy_paused': 'Proxy paused',
  'restriction.org_disabled': 'Disabled by org',
  'restriction.service_restricted': 'Service restricted',
  'restriction.age_requirement': 'Age or eligibility',
  'restriction.workspace_disabled': 'Not enabled by Workspace',
  'restrictionReason.service_restricted': 'Google has restricted this account from using AI Studio',
  'restrictionReason.age_requirement':
    'The account does not meet the AI Studio age or eligibility requirements',
  'restrictionReason.workspace_disabled':
    'The Google Workspace that manages this account has not enabled AI Studio',
  'state.queued': 'Queued',
  'state.running': 'Running',
  'state.completed': 'Completed',
  'state.cancelled': 'Cancelled',
  'state.failed': 'Failed',
  'app.readyOf': 'Ready {ready} / {total}',
  'app.activeRequests': 'Active requests {count}',
  'pager.total': '{count} total',
  'pager.perPage': '{count} / page',
  'pager.prev': 'Previous',
  'pager.next': 'Next',
  'pager.pageOf': 'Page {page} of {count}',
  'accounts.search': 'Search email, proxy, tier or error',
  'accounts.filterAll': 'All',
  'accounts.filterProblem': 'Problems (403)',
  'accounts.problemHelp':
    'Only accounts with 403 permission denied (such as account verification required), each with its reason; accounts that need sign-in, have AI Studio disabled by their organization or are unavailable have their own state filters and are not counted here even with 403 records (signing in again or updating the auth file clears those records; the 403 list above still shows their ordinary 403s). Each 403 model shows the time and count of its latest 403, and an account with a 403 on only one model is grouped as "403 permission denied (single model)"; account verification required on one model only is grouped as "403 verification required on one model" (only that model is paused), and on 2 or more models, or with the whole-account pause still in effect, as "403 verification required". A 403 record that is not account-level, has not been seen for more than {hours} hours and whose account is not in a 403 cooldown counts as stale and is left out (it stays greyed out in the 403 list above and can be cleared per row)',
  'accounts.problemCount': '{count} accounts with 403',
  'accounts.problemCopyWithReason': 'Copy emails and reasons',
  'accounts.problemCopied': 'Copied {count} accounts',
  'accounts.problemPermissionDenied': '403 permission denied',
  'accounts.problemVerifyRequired': 'Account verification required',
  'accounts.problemKind': 'Problem type',
  'accounts.problemKindAll': 'All 403',
  'accounts.problemKindVerify': '403 verification required',
  'accounts.problemKindForbidden': '403 permission denied (multiple models)',
  'accounts.problemKindModelForbidden': '403 permission denied (single model)',
  'accounts.problemKindVerifyModel': '403 verification required on one model',
  'accounts.problemVerifyModel': 'Account verification required on one model',
  'accounts.problemVerifyModelOrphan':
    'Account verification required on one model ({models}; the 403 record was cleared, the model is still paused; verify on the official site and click "Verify", or click "Clear" under "403 verification required on one model" above)',
  'accounts.problemVerifyPaused':
    'Account verification required, whole account paused (403 record already cleared; verify on the official site and click "Verify", or click "Clear records and lift pause" in the "Account verification required" summary above)',
  'accounts.problemStaleMore': '; {count} more stale records not counted',
  'accounts.sortDefault': 'Default order',
  'accounts.sortState': 'By state',
  'accounts.sortLabel': 'By email',
  'accounts.sortModels': 'By models',
  'accounts.enableAll': 'Enable all',
  'accounts.verifyAll': 'Verify all',
  'accounts.enableAllConfirm':
    'Enable all {count} disabled accounts? They are submitted 500 at a time; accounts without a model catalog sync it in the background after enabling, and accounts whose login has expired show as needing login.',
  'accounts.noneDisabled': 'No disabled accounts',
  'accounts.noneToVerify': 'No accounts to verify',
  'accounts.selected': '{count} selected',
  'accounts.selectFiltered': 'Select all {count} matching',
  'accounts.clearSelection': 'Clear selection',
  'accounts.bulkDisableConfirm': 'Disable {count} selected accounts?',
  'accounts.bulkDeleteConfirm':
    'Delete {count} selected accounts? Their account directories will be removed permanently.',
  'accounts.verifyTitle': 'Verify accounts',
  'accounts.verifyScopeProblem': 'Accounts that need login or are unavailable ({count})',
  'accounts.verifyScopeEnabled': 'All enabled accounts ({count})',
  'accounts.verifyScopeSelection': 'Selected or currently filtered enabled accounts ({count})',
  'accounts.verifyConcurrency': 'Concurrency',
  'accounts.verifyHint':
    'Each verification launches a headless Camoufox browser using about 300–600 MB of memory. Higher concurrency is faster but heavier; keep this page open while verifying.',
  'accounts.start': 'Start',
  'accounts.progress': '{action} {done}/{total}',
  'accounts.progressSucceeded': '{count} succeeded',
  'accounts.progressFailed': '{count} failed',
  'accounts.progressValid': '{count} valid',
  'accounts.progressInvalid': '{count} login expired',
  'accounts.progressErrored': '{count} failed',
  'accounts.progressInvalidList': 'Expired logins ({count})',
  'accounts.progressOrgDisabled': '{count} disabled or restricted',
  'accounts.progressSkippedPaused': '{count} skipped (proxy paused)',
  'accounts.progressSkippedPausedList': 'Skipped: proxy paused ({count})',
  'accounts.progressNotPassedList': 'Not passed ({count})',
  'accounts.progressActive': 'Working on: {names}',
  'accounts.progressStop': 'Stop',
  'accounts.progressStopped': 'Stopped',
  'accounts.progressDone': 'Done',
  'accounts.progressFailures': 'Failures ({count})',
  'accounts.bulkBusy': 'A bulk operation is running',
  'accounts.bulkFinished': '{action} finished: {succeeded} succeeded, {failed} failed',
  'accounts.bulkEnableBusy':
    'Account is busy (verifying, logging in, or serving a request); try again later',
  'accounts.bulkEnableLocked':
    'Another process is using the files of this account (another instance sharing the account directory); stop it and try again',
  'accounts.state': 'State',
  'accounts.account': 'Account',
  'accounts.region': 'Locale / Time zone',
  'accounts.actions': 'Actions',
  'accounts.noMatch': 'No matching accounts',
  'accounts.selectPage': 'Select page',
  'accounts.copyEmails': 'Copy emails',
  'studioKey.title': 'AI Studio API Key',
  'studioKey.fetch': 'Get key',
  'studioKey.fetchSelected': 'Get keys for selected ({count})',
  'studioKey.idle': 'Idle',
  'studioKey.running': 'Processing {current}, {queued} queued, {delay}s between accounts',
  'studioKey.expand': 'Expand',
  'studioKey.collapse': 'Collapse',
  'studioKey.help':
    'Opens the AI Studio API Keys page in the background with the account\'s saved sign-in and reads the key from the responses of the page\'s own requests, accepting only a key that matches a row of the page\'s key list (the page\'s own public key is never accepted; when the list only shows masked keys, that row\'s details are opened to read the full key); when the page has no key, it clicks the page\'s own "Create API key" to create one (if the account has no Google Cloud project yet, it first creates one through the page; at most one project and one key per run). Accounts are processed one at a time with {delay}s between them (STUDIO_KEY_DELAY in Settings); the account takes no new requests while it is processed. Signed-out, disabled and organization-disabled accounts are skipped; accounts paused for account verification (403) are processed as usual and stay paused afterwards. Only the last 4 characters are listed; the full key is read only when you click Copy. A key shared with other accounts, the public key of the page recognized by the browser, and a key saved by an older version without being checked are invalid: they are listed under "Failed", cannot be copied or exported, and "Get all" fetches them again. Keys without a project number can be completed with "Fill project info" (read-only: never creates a key or project); failed and skipped records can be deleted (they only disappear from the list; the account\'s key is not deleted, but an invalid key is not taken as the account\'s and goes with its record).',
  'studioKey.empty': 'No records yet. Click "Get key" on an account row, or select accounts and get keys in bulk.',
  'studioKey.account': 'Account',
  'studioKey.key': 'Key',
  'studioKey.projectName': 'Project name',
  'studioKey.projectNumber': 'Project number',
  'studioKey.state': 'Status',
  'studioKey.time': 'Time',
  'studioKey.created': '(created this time)',
  'studioKey.projectCreated': '(project created)',
  'studioKey.fetchedAt': 'Read',
  'studioKey.createdAt': 'Created',
  'studioKey.attemptAt': 'Last attempt',
  'studioKey.copy': 'Copy',
  'studioKey.copied': 'Key copied',
  'studioKey.copyFailed': 'Copy failed; check the browser clipboard permission',
  'studioKey.queued': 'Queued {count} account(s)',
  'studioKey.fetchAll': 'Get all',
  'studioKey.fetchAllConfirm':
    'Queue every account that has no key yet (including invalid keys)? They are processed one by one, {delay} seconds apart; an account without a key gets one created (and a project first if it has none). You can click "Stop" at any time; clicking "Get all" again only fills in the missing ones.',
  'studioKey.queuedAll': 'Started: every account without a key is fetched one by one; see the queue above',
  'studioKey.allDone': 'Every account already has a key; nothing to fetch',
  'studioKey.stop': 'Stop',
  'studioKey.stopped': 'Stopped: queued accounts were cleared; the one in progress will finish',
  'studioKey.copyAll': 'Copy all keys ({count})',
  'studioKey.copiedAll': 'Copied {count} key(s), one per line',
  'studioKey.noKeys': 'No keys fetched yet',
  'studioKey.fetchSelectedConfirm':
    'Get keys only for the {count} selected account(s); unselected accounts are left alone. They are processed one by one, {delay} seconds apart, and an account without a key gets one created. You can click "Stop" on the AI Studio API Key card.',
  'studioKey.tab.ok': 'OK',
  'studioKey.tab.failed': 'Failed',
  'studioKey.tab.skipped': 'Skipped',
  'studioKey.emptyOk': 'No account has a key yet',
  'studioKey.emptyFailed': 'No failed accounts',
  'studioKey.emptySkipped': 'No skipped accounts',
  'studioKey.reason': 'Reason (click to expand)',
  'studioKey.keptPrevious': 'Previous key kept',
  'studioKey.retry': 'Retry',
  'studioKey.retryFailed': 'Retry failed ({count})',
  'studioKey.retryFailedConfirm':
    'Queue the {count} account(s) under "Failed" again? They are processed one by one, {delay} seconds apart, and an account without a key gets one created. You can click "Stop" at any time.',
  'studioKey.copyFailedEmails': 'Copy failed account emails',
  'studioKey.copiedFailedEmails': 'Copied {count} failed account email(s), one per line',
  'studioKey.status.ok': 'OK',
  'studioKey.status.none': 'No key',
  'studioKey.status.failed': 'Failed',
  'studioKey.status.skipped': 'Skipped',
  'studioKey.status.invalid': 'Invalid',
  'studioKey.invalidCount': ' (incl. {count} invalid)',
  'studioKey.invalidHint':
    '{count} of these are invalid keys: shared with other accounts or the page\'s own public key (not this account\'s key), or saved by an older version without being checked against the page\'s key list. They cannot be copied and are not exported; "Get all" or "Retry failed" fetches them again, or you can clear them.',
  'studioKey.fillMark': ' (filling project info)',
  'studioKey.projectIdTitle': 'Project ID: {id}',
  'studioKey.delete': 'Delete',
  'studioKey.clear': 'Clear',
  'studioKey.clearTab': 'Clear records in this tab ({count})',
  'studioKey.clearFailure': 'Clear failure info',
  'studioKey.deleteRecordConfirm':
    'Delete the "{tab}" record of {label}? The record only disappears from the list: the account\'s key is not deleted (an invalid key is not taken as the account\'s and goes with the record) and nothing on AI Studio is touched. Fetching again creates a new record.',
  'studioKey.clearTabConfirm':
    'Clear all {count} records in the "{tab}" tab (including other pages)? They only disappear from the list: the accounts\' keys are not deleted (invalid keys are not taken as the accounts\' and go with their records), nothing on AI Studio is touched, and the account in progress is left alone. Fetching again creates new records.',
  'studioKey.clearFailureConfirm':
    'Clear the failure info of {label}\'s latest attempt? The saved key and project info stay as they are; only the failure reason is no longer shown (the record stays under "OK").',
  'studioKey.cleared': "Cleared: {parts} (no account's key was touched)",
  'studioKey.clearedDeleted': '{count} record(s) deleted',
  'studioKey.clearedInvalid': '{count} invalid key record(s) deleted',
  'studioKey.clearedReset': 'failure info cleared on {count} record(s)',
  'studioKey.clearedJoin': ', ',
  'studioKey.clearedNothing': 'Nothing to clear',
  'studioKey.clearedBusy': '{busy} account(s) in progress were left alone',
  'studioKey.clearedUnknown': '{count} account(s) are no longer in the account list',
  'studioKey.clearedErrors':
    'Records of {count} account(s) could not be read or written (damaged file or no permission) and were left alone',
  'studioKey.clearedSentence': '; ',
  'studioKey.fillProjects': 'Fill project info ({count})',
  'studioKey.fillProjectsAction': 'Fill project info',
  'studioKey.fillProjectsConfirm':
    'Fill in the project name and number for {count} key(s) without a project number? The AI Studio API Keys page is opened read-only, the saved key is found and its project is read (opening that key\'s details and closing them again if needed); nothing is ever created, neither keys nor projects. If the saved key is not on the page, the record is marked failed and the key is kept. Accounts are processed one by one, {delay} seconds apart; you can click "Stop" at any time.',
  'studioKey.fillQueued': 'Started filling project info: {count} account(s), one by one; see the queue above',
  'studioKey.fillNone': 'No key is missing a project number',
  'settings.studioKeyDelay': 'API key fetch interval (seconds)',
  'settings.studioKeyDelayHelp':
    'Seconds to wait between two accounts when getting AI Studio API keys in bulk (STUDIO_KEY_DELAY, default 30, 0 to 3600). Takes effect as soon as it is saved.',
  'accounts.copiedEmails': 'Copied {count} emails',
  'accounts.permissionDeniedTitle': '403 permission denied accounts',
  'accounts.permissionDeniedHint':
    "(usually the account has not accepted Google's terms; fetch a new auth file and upload it)",
  'accounts.permissionDeniedCount': '{count}',
  'accounts.permissionDeniedHelp':
    'Accounts whose generation requests returned HTTP 403 PERMISSION_DENIED (The caller does not have permission); each model keeps the time and count of its latest 403 and where that request came from. A record is cleared when the auth file is updated (a new storage-state.json is uploaded, a browser login, or the directory is imported again; its 403 cooldowns are lifted too); when the model succeeds once on that channel (only that model and channel); when an account that needs verification passes it (any model succeeds when 2 or more models asked for verification, that model itself when only one did); when "Verify" passes on this page; by the startup reconciliation (the account left the pool or its login signature changed); when a pool-wide 403 is revoked; and by the "Clear" button of each row or "Clear records" (only the accounts listed in this table; per-model 403 cooldowns stay). Records not seen for more than {hours} hours while the account is not in a 403 cooldown are greyed out as stale: most likely left over from long ago or from someone else (new-api channel tests, downstream users, a model alias, another instance sharing the account directory). Pool-wide 403s are not counted. Accounts that need account verification (an account-level 403: paused as a whole with 2 or more models, only that model paused with one, either for at most 30 days) are not listed here; see the "Account verification required" summary and "403 verification required on one model" below.',
  'accounts.permissionDeniedEmail': 'Email',
  'accounts.permissionDeniedModels': 'Model · channel',
  'accounts.permissionDeniedLast': 'Last seen',
  'accounts.permissionDeniedTimes': 'Times',
  'accounts.copyAllEmails': 'Copy all emails',
  'accounts.permissionDeniedDownload': 'Download list',
  'accounts.permissionDeniedClear': 'Clear records',
  'accounts.permissionDeniedClearConfirm':
    'Clear the 403 records of the {count} listed accounts? Per-model 403 cooldowns and multi-model 403 auto-pauses stay; an account that returns 403 again is listed again.',
  'accounts.permissionDeniedClearKeepVerify':
    ' The {count} accounts that need account verification (whole account or one model) are not included: their 403 records and pauses stay (see the "Account verification required" summary and "403 verification required on one model").',
  'accounts.permissionDeniedClearPartial':
    'Cleared the 403 records of {cleared} accounts and lifted the whole-account pause on {lifted} accounts, then failed: {error}',
  'accounts.permissionDeniedCleared': 'Cleared records of {count} accounts',
  'accounts.permissionDeniedLiftedMore':
    '; lifted the pause for account verification (whole account or one model) on {count} accounts',
  'accounts.permissionDeniedDelete': 'Delete all',
  'accounts.permissionDeniedDeleteHelp':
    'Removes every listed account (including collapsed ones) from the pool and deletes its auth directory, the same as "Delete" in the account list. This cannot be undone.',
  'accounts.permissionDeniedDeleteConfirm':
    'Delete all {count} listed accounts and their auth files (the account directories are removed)? This cannot be undone; an account can be used again only after its auth file is uploaded again. Accounts serving a request are deleted once they become idle.',
  'accounts.permissionDeniedDeleteConfirmFresh':
    'Delete the {count} listed accounts whose 403 records are not stale, and their auth files (the account directories are removed)? The other {stale} accounts have only stale 403 records (greyed out, most likely fine) and are not deleted; they can be cleared per row. This cannot be undone; an account can be used again only after its auth file is uploaded again. Accounts serving a request are deleted once they become idle.',
  'accounts.permissionDeniedDeleteStaleHelp':
    ' Accounts whose 403 records are all stale (greyed out, most likely fine) are not deleted.',
  'accounts.permissionDeniedDeleteStaleKept':
    '; {count} accounts with only stale 403 records were not deleted',
  'accounts.permissionDeniedDeleteAllStale':
    'All {count} listed accounts have only stale 403 records (most likely fine), so "Delete all" leaves them alone; clear their records per row, or delete them one by one in the account list if you really mean to',
  'accounts.permissionDeniedDeleteResult':
    'Deleted {deleted}, {missing} no longer existed, {failed} failed',
  'accounts.permissionDeniedDeleteFailures': '; failed: {list}',
  'accounts.permissionDeniedDeleteMore': 'and {count} more, see "Failures" for the full list',
  'accounts.permissionDeniedDeleteRemaining':
    '; {count} were not processed this time, click again to continue',
  'accounts.permissionDeniedDeleteBusy':
    'Account kept serving requests and never became idle; click again later',
  'accounts.permissionDeniedDeleteSkipped':
    '; {count} were no longer listed after confirming (for example, the auth file was uploaded again) and were not deleted',
  'accounts.permissionDeniedShowAll': 'Show all {count}',
  'accounts.permissionDeniedSeen': 'last {time} · {count}×',
  'accounts.permissionDeniedLastRequest': 'Last request: {detail}',
  'accounts.permissionDeniedRequestModel': 'requested as {model}',
  'accounts.permissionDeniedRequestMessages': '{count} messages',
  'accounts.permissionDeniedRequestImages': 'images {count}',
  'accounts.permissionDeniedRequestFiles': 'files {count}',
  'accounts.permissionDeniedStale': 'Stale ({days} days ago)',
  'accounts.permissionDeniedStaleShort': 'stale',
  'accounts.permissionDeniedStaleCount': '{count} stale',
  'accounts.permissionDeniedStaleHelp':
    'Not seen again for more than {hours} hours and the account is not in a 403 cooldown: most likely left over from long ago or from calls made by someone else, and safe to clear. Not counted under "Problems (403)".',
  'accounts.permissionDeniedClearRow': 'Clear',
  'accounts.permissionDeniedClearRowHelp':
    'Clears only the 403 record of this account; per-model 403 cooldowns stay',
  'accounts.permissionDeniedRowCleared': 'Cleared the 403 record of {label}',
  'accounts.permissionDeniedRowMissing':
    'The 403 record of {label} no longer exists (it may have just been cleared)',
  'accounts.verifyRequiredTitle': 'Account verification required: {count}.',
  'accounts.verifyRequiredState':
    'Paused as a whole (no Worker, no prewarm), not counted in the 403 list or model cooldowns.',
  'accounts.verifyRequiredStateLatest':
    'Paused as a whole (no Worker, no prewarm), not counted in the 403 list or model cooldowns; latest {time}.',
  'accounts.verifyRequiredStatePartial':
    '{paused} paused as a whole (no Worker, no prewarm), {expired} with the pause expired; none is in the 403 list, model cooldowns leave out only the paused ones.',
  'accounts.verifyRequiredStatePartialLatest':
    '{paused} paused as a whole (no Worker, no prewarm), {expired} with the pause expired; none is in the 403 list, model cooldowns leave out only the paused ones; latest {time}.',
  'accounts.verifyRequiredAdvice':
    'Verify on the official site, then click "Verify" here; otherwise the pause expires 30 days after it started and the account is tried again.',
  'accounts.verifyRequiredHelp':
    'Accounts that returned the account-level 403 "Your account must be verified … verify-account", plus accounts whose whole-account pause for account verification is still in effect: the same set as "Problems (403)" → "403 verification required". When it was returned on 2 or more models, or the whole-account pause is still in effect, the whole account is paused for 30 days: it is not scheduled, holds no Worker and is not prewarmed, so it is not listed in the 403 permission denied list and the model cooldowns leave it out; an account that asks for verification on one model only has just that model paused, see "403 verification required on one model". The pause runs 30 days from the first 403; once it expires (or for a record left by an older version that is already more than 30 days old) the account is scheduled again while its record stays and it is still counted here, and the state text gives the paused and expired counts separately; an account that is still unverified returns 403 on its next request and is paused again. Accounts that need sign-in, have AI Studio disabled by their organization or are unavailable are not counted here (as under "Problems (403)"); they have their own state filters. Verify at https://aistudio.google.com/verify-account and click "Verify" in the account list to lift the pause; "Clear records and lift pause" clears the 403 records of these accounts and lifts the pause, and an account that is still unverified returns 403 on its next request and is paused again.',
  'accounts.verifyRequiredView': 'View in account list',
  'accounts.verifyRequiredLift': 'Clear records and lift pause',
  'accounts.verifyRequiredLiftConfirm':
    'Clear the 403 records of these {count} accounts that need account verification and lift their whole-account pause? They are scheduled again; an account not verified on the official site returns 403 on its next request and is paused again (that request switches to another account). Per-model 403 cooldowns stay.',
  'accounts.verifyRequiredLifted':
    'Cleared the 403 records of {cleared} accounts and lifted the whole-account pause for account verification on {lifted} accounts',
  'accounts.verifyModelTitle': '403 verification required on one model: {count}.',
  'accounts.verifyModelHint':
    'Only the listed model is paused (up to 30 days); the other models are scheduled as usual and the Worker is kept. Verify at https://aistudio.google.com/verify-account, then click "Verify", or click "Clear" to lift the pause on that model.',
  'accounts.verifyModelHelp':
    'Accounts that returned the 403 "Your account must be verified … verify-account" on one model only (the same set as "Problems (403)" → "403 verification required on one model"; accounts that need sign-in, have AI Studio disabled by their organization or are unavailable are left out). Only that model is paused, on Playground and Build (up to 30 days); the other models of the account are scheduled as usual, its Worker is neither released nor retired, the account row keeps showing ready or busy, and the model cooldown summary leaves this pause out. A success on another model does not lift it; verifying on the official site and clicking "Verify", updating the auth file or "Clear" lifts it, and an account that is still unverified returns 403 the next time that model is requested and is paused again. When the same account also asks for verification on a second model it is paused as a whole and moves to the "Account verification required" summary above.',
  'accounts.verifyModelColumn': 'Model needing verification',
  'accounts.verifyModelPauseColumn': 'Pause on the model',
  'accounts.verifyModelPaused': 'Paused (until {time} at the latest)',
  'accounts.verifyModelPauseExpired': 'Not in effect (model scheduled as usual)',
  'accounts.verifyModelPauseExpiredHelp':
    'The pause on this model has expired, or the generation path did not write it (the catalog changed meanwhile): the model is scheduled as usual, and an account still unverified returns 403 on its next request and is paused again',
  'accounts.verifyModelState': '{paused} with the model paused.',
  'accounts.verifyModelStatePartial':
    '{paused} with the model paused, {expired} with the pause not in effect (expired or never written; the model is scheduled as usual, and an account still unverified returns 403 on its next request and is paused again).',
  'accounts.verifyModelNoRecord': '403 record cleared',
  'accounts.verifyModelVerifyHelp':
    'Check in an isolated browser whether this account is still signed in: if it is, its 403 records are cleared and the pause on the model is lifted (whether it was verified on the official site is not checked); if it was not verified, the next request for the model returns 403 and pauses it again',
  'accounts.verifyModelClearRowHelp':
    'Clear the 403 record of this account and lift the pause on the model; if it is still unverified on the official site, the next request for the model returns 403 and pauses it again',
  'accounts.verifyModelRowCleared': 'Cleared the 403 record of {label}',
  'accounts.orgDisabledTitle': 'Accounts that cannot use AI Studio (disabled by organization or restricted)',
  'accounts.orgDisabledHint':
    '(the organization has turned AI Studio off, or Google restricted the account, it does not meet the age or eligibility requirements, or its Workspace has not enabled AI Studio; signing in again does not help. Scheduling has stopped: replace the account or wait until it is enabled again)',
  'accounts.orgDisabledHelp':
    'Accounts whose AI Studio page is redirected to the ServiceNotAllowed page on access.workspace.google.com, or whose RPCs return HTTP 403 "AI Studio app is disabled" (the Google Workspace organization or Google has turned AI Studio off for them). Accounts redirected to accounts.google.com/info/servicerestricted (Google restricted the account), to the AI Studio age requirements page or whose RPCs return "Account not supported." (age or eligibility), and to the Workspace page or whose RPCs return "Early Access Apps disabled." (the Workspace has not enabled AI Studio) are listed here too; the Reason column tells them apart. A redirect to the available regions page or "Region not supported." is a problem with the egress region and is not listed here. These accounts no longer take part in prewarm, replacement, scale-out or account selection, and their resident Workers are closed once idle. The model catalog is rechecked every 24 hours and the account is restored as soon as a read succeeds. Updating the auth file (uploading a new storage-state.json, a browser login or importing the directory again) or clicking "Resume scheduling" also restores it at once; it is listed again if it is still denied.',
  'accounts.orgDisabledReason': 'Reason',
  'accounts.orgDisabledSource': 'Source',
  'accounts.orgDisabledNextCheck': 'Next recheck',
  'accounts.orgDisabledClear': 'Resume scheduling',
  'accounts.orgDisabledClearConfirm':
    'Clear the disabled or restricted state of all {count} accounts and resume scheduling now? An account that is still denied is listed again the next time it starts or syncs (startup fails at once instead of waiting).',
  'accounts.orgDisabledCleared': 'Resumed scheduling for {count} accounts',
  'accounts.orgDisabledSourceStartup': 'Startup',
  'accounts.orgDisabledSourceCatalog': 'Model catalog',
  'accounts.orgDisabledSourceVerify': 'Verify',
  'accounts.orgDisabledSourceLogin': 'Login',
  'accounts.orgDisabledSourceRequest': 'Request',
  'accounts.orgDisabledSourceRecheck': 'Login recheck',
  'models.allChannels': 'All channels',
  'models.pricingAll': 'Free & paid',
  'models.pricingFree': 'Free only',
  'models.pricingPaid': 'Paid only',
  'models.expandPage': 'Expand page',
  'models.collapseAll': 'Collapse all',
  'models.description': 'Description',
  'models.options': 'Options',
  'logs.compact': 'Compact',
  'logs.compactHint': 'Hide worker startup steps and merge repeated messages within a minute into one row',
  'logs.extraFilter': 'Extra pool only',
  'logs.extraFilterHelp':
    'Show only requests served by the extra pool and extra-pool runtime logs (membership, prewarming, takeovers, hand-backs, releases)',
  'logs.shown': 'Showing {shown} / {total}',
  'logs.jumpLatest': 'Jump to latest',
  'logs.detailTitle': 'Log detail',
  'logs.time': 'Time',
  'logs.message': 'Message',
  'logs.event': 'Event',
  'requests.filterAll': 'All',
  'requests.filterExtra': 'Extra pool',
  'requests.filterExtraHelp':
    'Show only requests served by the extra pool (the current or last attempt borrowed an extra-pool worker)',
  'cooldowns.search': 'Search model',
  'cooldowns.reason': 'Reason',
  'app.workers': 'Workers {warm} / {target}',
  'app.workersStarting': '{count} starting',
  'app.workerCapacity': 'Slots {occupied} / peak {max}',
  'app.workersDead': '{count} dead slots · retiring',
  'app.workersDeadDetail':
    "Warm workers whose account is in a long cooldown (e.g. daily quota, or the day's quota of a primary or hot model is used up): they hold a slot but cannot serve those requests. Closed at once when idle, drained first when busy, and replaced with an uncooled account",
  'app.workersDraining': '{count} draining',
  'app.workersDrainingDetail':
    'These dead-slot workers take no new requests and close once their in-flight requests finish (a long stream that has started output keeps the drain going until it ends); their replacements have already been started when a slot was free, otherwise they start once the old worker closes',
  'app.workersUnschedulable': '{count} unschedulable',
  'app.workersUnschedulableDetail':
    'Resident workers whose accounts need login, are disabled, have no model catalog or are paused until account verification; released when idle and refilled by prewarm',
  'app.primarySupplyModel': 'Primary {model}:',
  'app.primarySupply': '{warm} warm · {cooling} cooling · {standby} standby',
  'app.primarySupplyDetail':
    "Primary model {model}: warm is the number of resident workers that can serve it, cooling the number of resident workers cooling down for it (including short cooldowns ending within 2 minutes; they cannot serve it until the cooldown ends), standby the number of idle accounts that still can (counted up to 1000), refreshed about every 30 seconds. Standby 0 means no idle account can serve this model any more (the day's quota is probably used up; it resets at midnight Pacific time)",
  'app.ultraPrimarySupplyModel': 'Ultra primary {model}:',
  'app.ultraPrimarySupplyDetail':
    "Ultra primary model {model}: warm is the number of resident workers in the Ultra partition that can serve it, cooling the number of resident workers in the Ultra partition cooling down for it (including short cooldowns ending within 2 minutes; they cannot serve it until the cooldown ends), standby the number of idle Ultra-pool accounts that still can (including Pro accounts when they are put in the Ultra pool with the separate Pro pool off; counted up to 1000), refreshed about every 30 seconds. Standby 0 means no idle Ultra-pool account can serve this model any more (the day's quota is probably used up; it resets at midnight Pacific time). When quota runs out, requests for this model get a 429 right away instead of queuing and retrying inside the service",
  'app.proPrimarySupplyModel': 'Pro primary {model}:',
  'app.proPrimarySupplyDetail':
    "Supply of the Ultra/Pro primary model {model} in the Pro partition: warm is the number of resident workers in the Pro partition that can serve it, cooling the number of resident workers in the Pro partition cooling down for it (including short cooldowns ending within 2 minutes), standby the number of idle Pro-pool accounts that still can (counted up to 1000), refreshed about every 30 seconds. Standby 0 means no idle Pro-pool account can serve this model any more (the day's quota is probably used up; it resets at midnight Pacific time). When quota runs out, /pro requests for this model get a 429 right away and never borrow the Ultra pool",
  'app.prewarmActive': 'Prewarming · {inflight} running · {launched} this round',
  'app.prewarmStalled': 'Prewarm loop stalled {seconds}s',
  'app.prewarmIdle': 'Prewarm idle',
  'app.stepPrepareCatalog': 'Preparing the model catalog',
  'app.stepWaitCatalog': 'Waiting for the first account to load models',
  'app.stepStartPrewarm': 'Starting background prewarm',
  'app.stepCancelRequests': 'Cancelling in-flight requests',
  'app.stepWaitCatalogStop': 'Waiting for model sync to stop',
  'app.stepCloseWorkers': 'Closing WAA Workers',
  'app.stepFlushWrites': 'Writing pending records',
  'app.stepPrepareCatalogDetail':
    'Queues model catalog work against the account pool, restores catalogs from the disk cache, and starts background catalog sync and sign-in rechecks',
  'app.stepWaitCatalogDetail':
    'With no cached model catalog (first start or a damaged cache), the service starts running only after the first account loads its models and cannot serve generation requests before that; the other accounts keep syncing in the background',
  'app.stepStartPrewarmDetail':
    'The first prewarm runs in the background; the service starts running right away without waiting for its result',
  'app.stepCancelRequestsDetail':
    'Aborts in-progress requests and Worker launches for background expansion',
  'app.stepWaitCatalogStopDetail':
    'Model sync writes its cache to disk before exiting; waits at most 2 seconds',
  'app.stepCloseWorkersDetail':
    'Waits for the running prewarm to finish and closes the browsers of all WAA Workers',
  'app.stepFlushWritesDetail':
    "Writes buffered sign-in cookies and deferred model eligibility times; when a running service stops, also writes bad-account memory, Drive authorization state and today's send counts",
  'app.stepClockNote': ' (elapsed time follows the server clock)',
  'app.elapsedSeconds': '{seconds}s',
  'app.elapsedMinutes': '{minutes}m {seconds}s',
  'app.firstWarm': 'First prewarm in progress',
  'app.firstWarmPools': 'Waiting for the first Worker: {pools}',
  'app.firstWarmDetail':
    'The service is already running and accepting requests; the first prewarm continues in the background. The listed partitions have no ready Worker yet: requests that arrive meanwhile without a pinned account queue for the first Worker of that partition instead of cold-starting another one; a partition with no account left to start is released at once, and the wait is bounded by the request deadline. When an account fails to start, the prewarm moves on to the next candidate (each failure is logged as "WAA Worker 启动失败"); only after every candidate was tried without a Worker is one WARN line "WAA Worker 首轮预热失败" written, the service keeps running, and Workers are topped up automatically every 5 seconds',
  'app.partitionNormal': 'Normal',
  'app.ultraWorkers': 'Ultra workers {occupied} / {max}',
  'app.ultraWorkersDetail':
    'Ultra partition: {warm} running · {starting} starting · {dead} dead slots · {draining} draining · {unschedulable} unschedulable · prewarm target {target} · limit {max}',
  'app.proWorkers': 'Pro workers {occupied} / {max}',
  'app.proWorkersDetail':
    'Pro partition: {warm} running · {starting} starting · {dead} dead slots · {draining} draining · {unschedulable} unschedulable · prewarm target {target} · limit {max}',
  'app.extraWorkers':
    'Extra workers {workers} / {warm} · limit {max} · lendable {lendable} · members {members}',
  'app.extraWorkersOff': 'Extra pool off · handing back {workers}',
  'app.extraWorkersDetail':
    'Extra pool ({state}): mode {mode} · Ultra included {ultra} · paid models on the default URL {paid} · running {running} · starting {starting} · prewarm target {target} · cold members {cold} · members Free {free} / Pro {pro} / Ultra {ultraMembers} · handing back {returning} · borrowed {borrows} times by this process',
  'app.extraStateOn': 'on',
  'app.extraStateOff': 'off, handing back',
  'app.extraModeShare': 'share',
  'app.extraModeOverflow': 'overflow',
  'app.extraYes': 'yes',
  'app.extraNo': 'no',
  'app.proPoolNoticeTitle': 'Separate Pro pool is on by default',
  'app.proPoolNotice':
    '{count} Pro accounts only serve /pro. Suggested workers: Pro {proWarm}/{proMax}, normal {warm}/{max}.',
  'app.proPoolNoticeUltra':
    '{count} Pro accounts only serve /pro. Suggested workers: Pro {proWarm}/{proMax}, Ultra {warm}/{max}.',
  'app.proPoolNoticeDetail':
    'PRO_POOL is set in neither .env nor the environment, so it is on by default: {count} Pro accounts only serve the /pro prefix ({proAvailable} Pro accounts and {normalAvailable} normal accounts available). PRO_WARM_WORKER_LIMIT/PRO_MAX_ACTIVE_WORKERS are {currentProWarm}/{currentProMax}; the suggestion is {proWarm}/{proMax}, the share of available Pro accounts (never below the defaults); WARM_WORKER_LIMIT/MAX_ACTIVE_WORKERS {currentWarm}/{currentMax} can drop by that share to {warm}/{max} (never below the defaults or the startup prewarm concurrency). Point clients that need Pro accounts at the /pro prefix; turn "Separate Pro pool" off to keep the behaviour from before the upgrade. Changing a Pro pool setting (the switch or either worker count) on the settings page and saving writes PRO_POOL to .env and this notice stops; saving other settings does not',
  'app.proPoolNoticeDetailUltra':
    'PRO_POOL is set in neither .env nor the environment, so it is on by default: {count} Pro accounts only serve the /pro prefix and ULTRA_INCLUDE_PRO=true has no effect (before the upgrade they were in the Ultra partition and used the ULTRA_* capacity; {proAvailable} Pro accounts and {ultraAvailable} Ultra accounts available). PRO_WARM_WORKER_LIMIT/PRO_MAX_ACTIVE_WORKERS are {currentProWarm}/{currentProMax}; the suggestion is {proWarm}/{proMax}, the share of available Pro accounts (never below the defaults); ULTRA_WARM_WORKER_LIMIT/ULTRA_MAX_ACTIVE_WORKERS {currentWarm}/{currentMax} can drop by that share to {warm}/{max} (never below the defaults), and the normal partition stays as it is. Point clients that need Pro accounts at the /pro prefix; turn "Separate Pro pool" off to keep the behaviour from before the upgrade. Changing a Pro pool setting (the switch or either worker count) on the settings page and saving writes PRO_POOL to .env and this notice stops; saving other settings does not',
  'app.proPoolNoticeDismiss': 'Dismiss',
  'app.proPoolNoticeOpen': 'Open settings',
  'app.authRecheckLine': 'Login recheck · {suspects} suspected · {recent} new login-required in 10 min · {bad} bad accounts',
  'app.balancedTitle': 'Balanced',
  'app.balancedUltraTitle': 'Ultra balanced',
  'app.balancedProTitle': 'Pro balanced',
  'app.balancedSpread': 'today min/median/max {min}/{median}/{max}',
  'app.balancedUsed': '{used}/{accounts} accounts used',
  'app.balancedRotations': '{count} rotations in 1 h',
  'app.balancedRotating': '{count} rotating',
  'app.balancedPaused': 'paused: {reason}',
  'app.balancedDetail':
    "Balanced routing (ROUTING_STRATEGY=balanced): generation requests each schedulable account sent upstream today (Pacific time {day}; the daily quota resets at midnight Pacific time), min {min} · median {median} · p90 {p90} · max {max}. Used is the number of accounts that sent a request today out of the schedulable accounts (accounts paused for verification, accounts that need a login and the like are not counted). A running account that has sent {gap} more requests than the spare taking its place is rotated out (start-then-stop, no request is interrupted); rotation counts cover both partitions, {today} so far this quota day. Refreshed every 30 seconds, counting this service only. Per-account details are on the Usage page under Breakdown, ranked by Account: that view uses the server's local day and one row per request, while this counts every send by Pacific day (including retries on other accounts, every continuation round and regenerations), so the numbers differ",
  'app.driveLine':
    'Drive access · {available} available · {unavailable} unavailable · {unknown} unknown',
  'app.driveLineStorageFull': 'Drive storage full · {storageFull}',
  'app.driveDetail':
    "Google Drive authorization of enabled accounts: available accounts have obtained a Drive token or completed Drive authorization when signing in through this service, unavailable accounts have no Drive authorization (usually accounts imported with cookies only), storage full accounts are authorized but Drive rejected an upload because their storage is full (storageQuotaExceeded; shown on a line of its own only when there are such accounts), unknown accounts have not been tried yet. For accounts without Drive authorization or with full storage, attachments for regular models are sent inline, with no cooldown and no account switch. Video input for gemini-omni (and images in formats that cannot be sent inline) is uploaded to the account's Drive first, so it only goes to available or unknown accounts, and returns 400 when only unavailable accounts are left; PNG, JPEG, WebP, HEIC, HEIF, BMP and TIFF images are sent inline like the website does (GIF is converted to PNG), need no Drive and work on any account. Signing an account in through this service's login flow (Login or Add account on the Accounts page) completes the website's Drive authorization and marks the account available. All three states are kept for 24 hours in runtime/model-catalogs/drive-accounts.json and survive a restart; obtaining a Drive token does not clear the storage full state but a successful upload does, updating the auth file probes the account again, and a passing admin verification clears the unavailable and storage full records",
  'app.authRecheckDetail':
    'An upstream 401 first marks the login as suspected (5-minute cooldown); it is rechecked with the saved cookies and only marked as login-required if it is still 401. Accounts marked login-required by the API are rechecked every 15 minutes and restored when the recheck passes; accounts the browser found logged out (an admin verification found no login, or a worker start landed on the sign-in page) are not rechecked and need a new login or an updated auth file. Last hour: {cleared} cleared, {confirmed} confirmed, {recovered} recovered, {invalid} still invalid. Bad accounts kept getting bare rate limits and are left out of prewarm and replacements for 2 hours',
  'app.catalogSyncPaused': 'Catalog sync paused until {time} ({reason})',
  'app.catalogPauseUpstream': 'upstream 429',
  'app.catalogPauseEgress': 'egress rate limited',
  'app.catalogSyncPending': '{count} catalogs pending · {suspects} suspected logins to probe',
  'app.catalogSyncDetail':
    'Model catalogs are saved in runtime/model-catalogs and loaded directly on restart; accounts whose catalog is missing or older than 24 hours are refreshed slowly in the background (idle accounts only), pausing on upstream 429 or while the egress is rate limited',
  'app.egressLimited':
    'Egress limited {label} · upstream rejected {ratio}% · local 429 {local}/min · queued {queued} · {limit}',
  'app.egressLimitRPM': '{limit}/min',
  'app.egressLimitUltra': 'Ultra {limit}/min',
  'app.egressLimitLanes': 'normal {normal}/min · Ultra {ultra}/min',
  'app.egressLimitOff': 'not throttled',
  'app.egressSuspended':
    'Egress limited {label} · throttling stopped (ineffective) · upstream rejected {ratio}% · local 429 {local}/min',
  'app.egressLimitedDetail':
    'Many accounts behind this egress are rate limited at once (freshly switched-in accounts are rejected on their first request): escalating cooldowns and on-demand cold starts are paused, and requests are sent at the egress rate limit with primary models first. "Upstream rejected" is the share of requests sent upstream that were rejected; "local 429" counts requests that queued past the limit and were rejected locally. Recovers automatically once the limiting ends. Only more egresses (different proxies for some accounts, or the IP pool) can raise the ceiling',
  'app.egressRejected': 'Egress {label} verdict withdrawn ({by})',
  'app.egressRejectedDetail':
    'Rejected accounts never succeeded when retried (more like used-up account quota), or the verdict was withdrawn manually: escalating cooldowns are back, and no new verdict for {minutes} minutes',
  'app.egressRejectedByValve': 'safety valve',
  'app.egressRejectedByAdmin': 'manual',
  'app.egressDirect': 'direct',
  'app.egressWithdraw': 'Withdraw',
  'app.egressResume': 'Resume',
  'onboard.title': 'New account automation',
  'onboard.off': 'Off',
  'onboard.idle': 'Queue empty',
  'onboard.pending': '{count} pending',
  'onboard.processing': '{count} processing',
  'onboard.nextSoon': 'starting soon',
  'onboard.nextIn': 'starts in ~{seconds}s',
  'onboard.settings': 'Settings & history',
  'onboard.collapse': 'Collapse',
  'onboard.autoVerify':
    'Verify new accounts and existing accounts whose auth file is re-uploaded (launches a browser; accounts are enabled by default and show as needing login if verification fails, accounts still disabled are verified while disabled)',
  'onboard.autoEnable':
    'Enable accounts that are still disabled (with verification on, only verified accounts)',
  'onboard.batchSize': 'Batch size',
  'onboard.concurrency': 'Verify concurrency',
  'onboard.hint':
    'New folders in auth/ are imported within ~10s and queued; processing starts when the batch size is reached or the oldest account has waited 2 minutes. Verification launches a browser, ~10-30s per account. Existing accounts are left untouched on first use; accounts added from this page are not processed; re-uploaded accounts are processed again. When an existing account\'s auth file is re-uploaded (storage-state.json overwritten, or re-imported from Chrome), the account\'s old needs-login, 403, account-verification, single-model verification, organization disabled or restricted and suspected-logout states are cleared, its old browser is closed, it is re-authenticated (scheduled again as soon as it is loaded) and queued for one verification right away, and the result shows below as "After re-upload: …". Accounts in any of these states are processed even if the uploaded file is identical (e.g. re-uploaded as-is after clearing a 403 on the website); only healthy accounts with an unchanged file just get an "auth file unchanged" note. Manually disabled accounts only get the new auth and stay disabled; with verification off the file is only reloaded. Files re-uploaded while the service was stopped normally are handled the same way at startup. A bulk upload is loaded at most 40 accounts per scan; the rest count as "waiting to load". When re-uploading an identical file, do not preserve its original modification time (WinSCP "preserve timestamp", rsync -a, scp -p), or the rewrite cannot be seen; a re-export that only changes Google\'s rotating cookies is treated as another process\'s write-back. Both count as "not treated as re-uploads" (the log says so); in both cases click Verify on the account.',
  'onboard.reauth': 'incl. {count} re-uploaded',
  'onboard.reauthTip': 'Existing accounts re-verified after their auth file was re-uploaded (included in pending and processing)',
  'onboard.sourceReauthTip': 'Result of processing an existing account after its auth file was re-uploaded',
  'onboard.loading': '{count} waiting to load',
  'onboard.loadingTip':
    'The directory scan has seen updated auth files for these accounts but has not loaded them yet: at most 40 are loaded per scan (about every 10 seconds) and accounts busy with requests are retried next scan; until then the account list still shows the old state, and they join the verification queue once loaded',
  'onboard.notReuploaded': 'Not treated as re-uploads: {count}',
  'onboard.notReuploadedTip':
    'Accounts needing login, hitting 403 or in other failed states got an auth file that was taken for this service\'s own copy (the upload kept the original modification time) or for a write-back by another process sharing the account folder (only Google\'s rotating cookies or the AI Studio localStorage differ), so the old states were not cleared and nothing was re-authenticated (number of times in this run; the log lists each account). If it was your re-upload, click Verify on the account (passing clears the 403 records and cooldowns), or upload again without preserving timestamps, or touch the file after uploading',
  'onboard.unchanged': 'Auth file unchanged: {count}',
  'onboard.unchangedTip':
    'Healthy (or merely manually disabled) accounts got an auth file identical to their current login, so nothing was re-authenticated (number of times in this run; the log lists each account). Accounts needing login, hitting 403 or in other failed states are not counted here and are processed as re-uploads',
  'onboard.sameLogin': 'Same login, not reloaded: {count}',
  'onboard.sameLoginTip':
    'Healthy enabled accounts got an auth file whose content changed but whose login-signature cookies did not (a re-export from the same session, or a write-back by another process sharing the account folder), so as before it was not reloaded or re-verified; only a file with a different login is re-authenticated (number of times in this run, at most once per account per hour; the log lists each account)',
  'onboard.sourceReauth': 'Re-upload',
  'onboard.reauthAfter': 'After re-upload: {outcome}',
  'onboard.reauthOutcomesTip':
    'Result of each account\'s latest auth file re-upload: Restored means verified and scheduled again; accounts that hit a 403 within 24 hours after verifying are changed to Still needs verification (account verification) or Still 403',
  'onboard.reauthOutcome.restored': 'Restored',
  'onboard.reauthOutcome.auth_required': 'Still needs login',
  'onboard.reauthOutcome.verify_required': 'Still needs verification',
  'onboard.reauthOutcome.forbidden': 'Still 403',
  'onboard.reauthOutcome.restricted': 'Still restricted',
  'onboard.reauthOutcome.manual_disabled': 'Skipped (manually disabled)',
  'onboard.reauthOutcome.verify_off': 'Reloaded, not verified',
  'onboard.reauthOutcome.error': 'Verification error',
  'onboard.reauthOutcome.skipped': 'Skipped',
  'onboard.invalid': 'Batch size 1-200, concurrency 1-8',
  'onboard.saving': 'Saving…',
  'onboard.saved': 'Saved and applied',
  'onboard.recent': 'Recent',
  'onboard.none': 'No records yet',
  'onboard.result.enabled': 'Enabled',
  'onboard.result.verified': 'Verified',
  'onboard.result.verify_failed': 'Verification failed',
  'onboard.result.error': 'Error',
  'onboard.result.skipped': 'Skipped',
  'onboard.result.manual': 'Added manually',
  'onboard.result.baseline': 'Baseline',
  'onboard.queueDisabled': 'Process disabled accounts ({count})',
  'onboard.queueDisabledConfirm':
    'Queue {count} disabled accounts: each is verified first; verified ones are enabled, the rest stay disabled. Accounts you disabled on purpose will also be enabled if they verify. Accounts added from this page are not affected.',
  'onboard.queueDisabledAction': 'Queue',
  'accounts.worker': 'Worker',
  'accounts.workerWarm': 'Running',
  'accounts.workerStarting': 'Starting',
  'accounts.workerNone': 'Not started',
  'accounts.workerFilterAll': 'Worker: all',
  'accounts.workerFilterWarm': 'Worker: running',
  'accounts.workerFilterStarting': 'Worker: starting',
  'accounts.workerFilterNone': 'Worker: not started',
  'accounts.sortWorker': 'By worker',
  'accounts.ultraCount': 'Ultra pool ready {ready} / {total}',
  'accounts.ultraCountHelp':
    'Accounts in the Ultra pool (including Pro accounts when the separate Pro pool is off and "Put Pro accounts in the Ultra pool" is on): requests under the /ultra prefix only use these accounts',
  'accounts.ultraWorkers': 'Ultra workers {warm} / {target}',
  'accounts.proCount': 'Pro pool ready {ready} / {total}',
  'accounts.proCountHelp':
    'Accounts in the Pro pool (accounts with the Pro tier while the separate Pro pool is on): requests under the /pro prefix only use these accounts, and the default URL and /ultra do not',
  'accounts.proWorkers': 'Pro workers {warm} / {target}',
  'accounts.poolUltraTag': 'Ultra pool',
  'accounts.proInUltraHelp':
    '"Put Pro accounts in the Ultra pool" is on: this account keeps its Pro tier but belongs to the Ultra pool, is scheduled by the Ultra-pool settings and serves requests under the /ultra prefix. While "Ultra-pool accounts only serve /ultra requests" is on in Service config, it no longer serves normal-path requests',
  'accounts.tierFilterAll': 'Tier: all',
  'accounts.tierFilterUltra': 'Tier: Ultra',
  'accounts.poolFilter': 'Pool',
  'accounts.poolFilterHelp':
    'Filter by the pool an account belongs to now (membership, not tier): with the separate Pro pool on, Pro accounts belong to the Pro pool; with it off they stay in the normal pool or join the Ultra pool',
  'accounts.poolFilterAll': 'Pool: all',
  'accounts.poolFilterNormal': 'Pool: normal',
  'accounts.poolFilterPro': 'Pool: Pro',
  'accounts.poolFilterUltra': 'Pool: Ultra',
  'accounts.extraWorker': 'Extra',
  'accounts.extraWorkerHelp':
    'This worker counts toward the extra pool: the account has a cooling model while other models work, and the extra pool lends it to every entry point',
  'accounts.extraMember': 'Member',
  'accounts.extraMemberHelp':
    'Extra pool member: a model is cooling down while other models work (the cooling models and their end times are in the cooldown list on the Requests page). The account list follows the latest reconciliation and is not pushed on membership changes',
  'accounts.extraCount': 'Extra workers {workers} / {warm} · members {members}',
  'accounts.extraCountOff': 'Extra pool off · handing back {workers}',
  'accounts.extraCountHelp':
    'Extra pool (EXTRA_POOL): workers counted in the extra pool against its resident count, and members, the accounts with a cooling model while other models work (including those whose worker stays in a main partition)',
  'accounts.poolFilterExtra': 'Pool: Extra',
  'accounts.tierFilterPro': 'Tier: Pro',
  'accounts.tierFilterPlus': 'Tier: Plus',
  'accounts.tierFilterFree': 'Tier: Free',
  'accounts.tierFilterUnknown': 'Tier: not read yet',
  'accounts.tierUnknown': 'Not read',
  'accounts.tierUnknownHelp': 'The tier has not been read from AI Studio yet; the account counts as Free in the normal pool for now',
  'settings.helpExpand': 'Show more',
  'settings.helpCollapse': 'Show less',
  'settings.liveHint':
    'Worker counts, concurrency, requests per minute per account, routing strategy, timeouts, upstream channels, temporary chat, the IP pool, the API key and the like apply immediately; the account directory, default proxy and WAA backend need a generation service restart; the listen address needs a restart of the management process.',
  'logs.categoryAll': 'All',
  'logs.categoryRequest': 'Requests',
  'logs.categoryAccount': 'Accounts',
  'logs.categoryService': 'Service',
  'logs.groupSources': '{count} sources',
  'logs.groupMembers': '{count} similar entries merged',
  'logs.raw': 'Raw message',
  'settings.autoApply': 'Apply automatically',
  'settings.stateSaving': 'Saving…',
  'settings.stateSaved': 'Saved and active',
  'settings.stateSavedPending': 'Saved, waiting to apply',
  'settings.stateDirty': 'Unsaved changes (saved when the field loses focus)',
  'settings.stateError': 'Not saved: {message}',
  'settings.applyPending':
    'The new configuration needs a generation service restart; active requests get up to 30 seconds to finish first.',
  'settings.applyCountdown': 'Applying in {seconds}s',
  'settings.applyNow': 'Apply now',
  'settings.applying': 'Applying new configuration…',
  'settings.applied': 'New configuration is active',
  'settings.savedStopped': 'Saved; applies the next time the generation service starts',
  'settings.saveAndApply': 'Save and apply',
  'settings.apiKeyHot': 'Takes effect immediately after saving, no restart needed',
  'settings.apiKeyDefault':
    'Every API call must send this key; saving it empty uses the default key {key}. The default key is public in the source code, so set your own key before listening on a public address',
  'settings.invalidRequired': '{field} is required',
  'settings.invalidNumber': '{field} must be a positive integer',
  'settings.invalidAccountRpmLimit':
    'Requests per minute per account must be an integer from 0 (off) to 1000',
  'settings.invalidDuration': '{field} must be a duration such as 30s, 2m or 1h30m',
  'settings.invalidFirstEventTimeout':
    'First event timeout must be 0 or off (disabled), or whole seconds or a duration shorter than the request timeout, such as 60 or 90s',
  'settings.invalidPrimaryQueueWait':
    'Primary model queue limit must be 0 (never queue) or a duration not longer than the request timeout, such as 10s',
  'settings.invalidTierRuleModels': '{field} cannot contain names starting with long-; write the base model ID',
  'settings.invalidUltraPrimaryQueueWait':
    'Ultra primary model queue limit must be 0 (never queue) or a duration not longer than the request timeout, such as 10s',
  'settings.invalidListen': 'Listen address must be host:port',
  'settings.invalidMaxWorkers': 'Peak worker limit cannot be lower than the resident worker count',
  'settings.invalidWarmConcurrency': 'Warm-up concurrency must be between 1 and the resident worker count',
  'settings.invalidUltraWarmWorkers': 'Ultra resident workers must be 0 or a positive integer',
  'settings.invalidUltraMaxWorkers':
    'Ultra active worker limit must be a positive integer and cannot be lower than the Ultra resident worker count',
  'settings.invalidProWarmWorkers': 'Pro resident workers must be 0 or a positive integer',
  'settings.invalidProMaxWorkers':
    'Pro active worker limit must be a positive integer and cannot be lower than the Pro resident worker count',
  'settings.downgradeGuard': 'Reject downgraded replies',
  'settings.downgradeGuardHelp':
    'For the models listed below, check before any reply text reaches the client whether the upstream switched the request to another model (for example 3.1 Pro to 3.1 Flash-Lite), using output speed and the model the upstream reports. Downgraded requests get an HTTP 400 in the official format of each protocol, and the downgraded text is never sent. Other models are unaffected.',
  'settings.downgradeModels': 'Guarded models',
  'settings.downgradeModelsHelp': 'Comma-separated model IDs; aliases such as -nothinking and -128 map to these models automatically.',
  'settings.downgradeSpeed': 'Downgrade speed threshold (tok/s)',
  'settings.downgradeMinTokens': 'Reply tokens needed',
  'settings.downgradeMinWindow': 'Minimum window (ms)',
  'settings.downgradeSpeedHelp':
    'Measured after the first text chunk: once enough new reply tokens have arrived and the minimum window has passed, a speed at or above the threshold counts as a downgrade. Normal Pro runs at about 90-150 tok/s, downgraded replies at about 230-270. Larger values judge more reliably but delay the first reply text more.',
  'settings.downgradeFuzzyLow': 'Uncertain range low (tok/s)',
  'settings.downgradeFuzzyHigh': 'Uncertain range high (tok/s)',
  'settings.downgradeCountTimeout': 'CountTokens timeout (ms)',
  'settings.downgradeFuzzyHelp':
    'The Playground channel reports cumulative reply tokens, so its speed is exact (guarded models prefer Playground). The Build channel can only estimate from characters; when the estimate falls inside the uncertain range, CountTokens is called on the same account to measure exactly, and the estimate is used on timeout or failure. A CountTokens timeout of 0 disables the call.',
  'settings.downgradeFastMode': 'Fast mode',
  'settings.downgradeFastModeHelp':
    'Strict mode (default): streaming requests send nothing, including thinking, until judged, and hold the response headers for at most 50 seconds (the same cap as holding the headers before the first event); a downgrade judged within that time gets an HTTP 400. Without a verdict after 50 seconds (or the maximum delay, when set) the 200 response starts, and a later downgrade verdict is reported as an error event in the stream. Fast mode streams thinking in real time from the start and holds only the reply text; a downgrade is reported as an official error event in the stream.',
  'settings.downgradeRejectStatus': 'Status code when rejecting',
  'settings.downgradeReject400': '400: report as blocked by content policy (default)',
  'settings.downgradeReject503': '503: report as temporarily unavailable',
  'settings.downgradeRejectStatusHelp':
    '400 uses the official format of a Google content-policy block (PROHIBITED_CONTENT), which clients and relays usually do not retry; 503 states that upstream served a different model and the reply was discarded, so clients and relays can retry or switch channels. Takes effect immediately.',
  'settings.downgradeMemory': 'Remember downgraded conversations (minutes)',
  'settings.downgradeMemoryHelp':
    'Within this time, a request for the same conversation that still contains the earlier messages unchanged (regenerate, continue) is rejected before sending; editing any of those messages makes it judged again. Single-message conversations and requests the client abandoned are not remembered. Only hashes are stored; 0 disables this.',
  'settings.downgradeMaxHold': 'Maximum delay (ms)',
  'settings.downgradeMaxHoldHelp':
    'The default 0 holds until the verdict or for 50 seconds: thinking is held, a downgrade judged within 50 seconds gets an HTTP 400, and the first token arrives only after the whole thinking phase; without a verdict after 50 seconds the 200 response starts with heartbeats, and a later downgrade verdict is reported as an error event in the stream. With a positive value (for example 3000), content is held at most this long: if there is no verdict in time, the 200 response starts and the held thinking is sent while the reply text stays held, and a later downgrade verdict can only be reported as an error event in the stream. Reply text held this long without a verdict is released, then rechecked at the end and counted as a miss if it was downgraded.',
  'settings.invalidDowngradeModels': 'Enter at least one guarded model when the downgrade check is on',
  'settings.invalidDowngradeFuzzy': 'The uncertain range low value cannot exceed the high value',
  'accounts.autoImportHint':
    'Account folders placed in auth/ are imported within about 10 seconds; deleted folders are removed; accounts needing login reload automatically when their credential file is replaced.',
  'usage.title': 'Usage',
  'usage.description': 'Counts, outcomes, tokens and latency of public API requests that passed the API key check; token counting requests are not recorded, records are kept for 90 days',
  'usage.unavailable': 'The usage ledger is not available: it failed to open at startup, see the service log for the reason',
  'usage.panelLoading': 'Loading the usage page',
  'usage.panelFailed': 'The usage page failed to load. Reload the browser and try again',
  'usage.range': 'Time range',
  'usage.range.1h': 'Last hour',
  'usage.range.6h': 'Last 6 hours',
  'usage.range.24h': 'Last 24 hours',
  'usage.range.7d': 'Last 7 days',
  'usage.range.30d': 'Last 30 days',
  'usage.range.90d': 'Last 90 days',
  'usage.range.today': 'Today',
  'usage.range.yesterday': 'Yesterday',
  'usage.range.week': 'This week',
  'usage.range.month': 'This month',
  'usage.range.lastMonth': 'Last month',
  'usage.range.custom': 'Custom',
  'usage.rangeRecent': 'Recent',
  'usage.rangeCalendar': 'Calendar',
  'usage.rangeCustom': 'Custom range',
  'usage.rangeStart': 'Start',
  'usage.rangeEnd': 'End',
  'usage.rangeApply': 'Apply',
  'usage.rangeInvalid': 'The start must be earlier than the end',
  'usage.rangeTooLong': 'The range cannot exceed 93 days',
  'usage.rangeRetention': 'Records are kept for 90 days; earlier times have no data',
  'usage.filters': 'Filters',
  'usage.filtersClear': 'Clear filters',
  'usage.filterSearch': 'Search values',
  'usage.filterNone': 'No values to choose',
  'usage.dimension.model': 'Model',
  'usage.dimension.account': 'Account',
  'usage.dimension.channel': 'Channel',
  'usage.dimension.protocol': 'Protocol',
  'usage.dimension.state': 'Outcome',
  'usage.dimension.pool': 'Pool',
  'usage.poolAll': 'All pools',
  'usage.dimension.status': 'Status code',
  'usage.autoRefresh': 'Auto refresh',
  'usage.autoRefresh.0': 'No auto refresh',
  'usage.autoRefresh.15': 'Every 15 seconds',
  'usage.autoRefresh.30': 'Every 30 seconds',
  'usage.autoRefresh.60': 'Every 60 seconds',
  'usage.updatedAt': 'Updated {time}',
  'usage.nextRefresh': 'Refreshing in {seconds}s',
  'usage.fixedRange': 'Fixed range; no auto refresh',
  'usage.latest': 'Latest request: {time}',
  'usage.latestNone': 'No requests recorded yet',
  'usage.latestHelp': 'Completion time of the newest request in the ledger, regardless of range and filters',
  'usage.justNow': 'just now',
  'usage.secondsAgo': '{n}s ago',
  'usage.minutesAgo': '{n} min ago',
  'usage.hoursAgo': '{n} h ago',
  'usage.daysAgo': '{n} d ago',
  'usage.requests': 'Requests',
  'usage.requestsHelp': 'Public API requests finished in the range; succeeded includes tool calls, output limits and upstream stops',
  'usage.succeeded': 'Succeeded',
  'usage.failed': 'Failed',
  'usage.canceled': 'Canceled',
  'usage.rateLimited': 'Rate limited 429',
  'usage.otherErrors': 'Other errors',
  'usage.successRate': 'Success rate',
  'usage.successRateHelp': 'Succeeded ÷ (requests − client cancellations). Tool calls, output limits and upstream stops count as succeeded',
  'usage.quality': 'Downgrade rejections',
  'usage.qualityHelp': 'Requests rejected as downgraded ÷ requests checked by the downgrade guard; the duplicate rate is replies identical to another reply within 2 hours ÷ replies of at least 50 tokens',
  'usage.downgradeRejected': 'Rejected / checked',
  'usage.duplicateReplies': 'Duplicate replies',
  'usage.tokens': 'Tokens',
  'usage.input': 'Input',
  'usage.reasoning': 'Reasoning',
  'usage.reply': 'Reply',
  'usage.throughput': 'Throughput',
  'usage.throughputHelp': 'Averages divide requests or tokens in the range by its minutes; current values use the last {minutes} minutes',
  'usage.current': 'Current',
  'usage.duration': 'Latency',
  'usage.durationHelp': 'Time from arrival to the end of the response for succeeded requests',
  'usage.firstEvent': 'First event',
  'usage.firstEventHelp': 'Time from arrival to the first upstream semantic event for succeeded requests',
  'usage.average': 'Avg',
  'usage.queue': 'Queue',
  'usage.vsPrevious': 'Compared with the previous period {range}',
  'usage.points': '{value} pp',
  'usage.chartTraffic': 'Traffic',
  'usage.chartTrafficHelp': 'Stacked by dimension; values beyond the top 8 are merged into “Other”',
  'usage.metricRequests': 'Requests',
  'usage.metricTokens': 'Tokens',
  'usage.stackBy': 'Stack by',
  'usage.other': 'Other',
  'usage.unassigned': 'Unassigned',
  'usage.chartOutcomes': 'Outcomes and errors',
  'usage.chartLatency': 'Latency percentiles',
  'usage.chartTokens': 'Token composition',
  'usage.total': 'Total',
  'usage.ranking': 'Breakdown',
  'usage.lastRequest': 'Last request',
  'usage.onlyThis': 'Filter to this',
  'usage.showBreakdown': 'Show details',
  'usage.hideBreakdown': 'Hide details',
  'usage.pairsLoading': 'Loading the breakdown…',
  'usage.pairsFailed': 'Failed to load the breakdown: {error}',
  'usage.pairsEmpty': 'No breakdown for this row in the current range',
  'usage.pairsMore': 'Show {count} more ({hidden} not shown)',
  'usage.records': 'Request records',
  'usage.recordsCount': '{count} records, newest first',
  'usage.recordsSearch': 'Search request ID, error or reply fingerprint',
  'usage.recordsAllStatus': 'All status codes',
  'usage.extraFilter': 'Served by the extra pool',
  'usage.extraFilterHelp':
    'List only requests whose last attempt borrowed the extra pool (the pool column still follows the entry point); the CSV export keeps the same columns and only these rows',
  'usage.export': 'Export CSV',
  'usage.columns': 'Columns',
  'usage.pageSize': '{count} per page',
  'usage.previousPage': 'Previous',
  'usage.nextPage': 'Next',
  'usage.page': 'Page {page}',
  'usage.recordsEmpty': 'No matching request records',
  'usage.time': 'Finished',
  'usage.result': 'Outcome',
  'usage.attempts': 'Retries',
  'usage.openDetail': 'Open request details',
  'usage.state.completed': 'Completed',
  'usage.state.tool_calls': 'Tool calls',
  'usage.state.limited': 'Output limit reached',
  'usage.state.blocked': 'Stopped upstream',
  'usage.state.failed': 'Failed',
  'usage.state.cancelled': 'Canceled',
  'usage.downgrade': 'Downgrade check',
  'usage.downgrade.rejected': 'Rejected as downgraded',
  'usage.downgrade.passed': 'Passed',
  'usage.downgrade.unjudged': 'Not judged, let through',
  'usage.downgradeShort': 'Downgrade',
  'usage.duplicate': 'Identical to a recent reply',
  'usage.duplicateShort': 'Duplicate',
  'usage.servedModel': 'Served model',
  'usage.replyHash': 'Reply fingerprint',
  'usage.searchReply': 'Find the same reply',
  'usage.detailTitle': 'Request details',
  'usage.detailSummary': 'Summary',
  'usage.detailError': 'Error',
  'usage.detailAttempts': 'Upstream attempts',
  'usage.detailAttemptsNone': 'The first attempt produced the result',
  'usage.detailBody': 'Bodies',
  'usage.detailBodyNone': 'Body logging (REQUEST_BODY_LOG) was off when this request finished',
  'usage.detailRequest': 'Request',
  'usage.detailResponse': 'Response',
  'usage.detailTruncated': 'Truncated: {saved} saved of {size}',
  'usage.path': 'Path',
  'usage.toolCalls': 'Tool calls',
  'usage.empty': 'No requests in this period',
  'usage.emptyHelp': 'Public API requests count toward usage when they finish.',
  'usage.emptyFiltered': 'No requests match the filters',
  'usage.emptyFilteredHelp': 'Adjust the filters or the time range and try again.',
  'usage.retry': 'Retry',
  'usage.protocol.images': 'Images',
  'usage.protocol.audio': 'Audio',
  'usage.protocol.videos': 'Videos',
  'usage.protocol.files': 'Files',
  'usage.protocol.other': 'Other',
  // ---------- IP pool ----------
  'proxy.warnDuplicateIP':
    'Some exits share the same IP: these ports or sessions may map to one IP, so accounts would share it. Check the allocation at your provider.',
  'proxy.warnPoolPartial': 'Some exits failed; see the lines above for the reasons.',
  'proxy.slotPort': 'port {port}',
  'proxy.slotSession': 'session {session}',
  'proxy.testAccountSlot': 'Current exit of this account: {slot}',
  'proxy.testAccountClearedSlot':
    'After clearing the account proxy the IP pool assigns, for example: {slot} (workers started later may get another port)',
  'proxy.testPoolSlot':
    'IP pool exit a login would get right now: {slot} (the login itself may get another)',
  'settings.proxyPoolTitle': 'IP pool (one IP per browser)',
  'settings.proxyPoolTitleFixed': 'IP pool (one fixed IP per account)',
  'settings.proxyPoolHelp':
    'Accounts without their own proxy go out through the IP pool. Static IPs: each port in the range is one fixed IP, every running browser (worker, login, verification) gets a port of its own, and an account returns to the port it used last when possible. Rotating IPs: {session} in the address is replaced with a fixed session ID per account, one sticky IP per session. Priority: the account proxy > the IP pool > the default proxy. "One IP per account" holds at any one moment: with more accounts than ports, one IP serves several accounts over time.',
  'settings.proxyPoolHelpFixed':
    'Accounts without their own proxy go out through the IP pool. Fixed assignment is on: with static IPs every account has one fixed port (each port in the range is one fixed IP), and the accounts on one port share its IP and can run browsers at the same time instead of one port per browser; with rotating IPs, {session} in the address is replaced with a fixed session ID per account, which is kept. When a few IPs (sessions) go bad they are replaced automatically; if more go bad within 30 minutes than the limit allows, automatic replacement stops and they pause until you handle them (a limit of 0 never replaces). Priority: the account proxy > the IP pool > the default proxy.',
  'settings.proxyPoolMode': 'Mode',
  'settings.proxyPoolModeOff': 'Off (use the default proxy)',
  'settings.proxyPoolModeStatic': 'Static IPs (port range, one port per browser)',
  'settings.proxyPoolModeStaticFixed': 'Static IPs (port range, one fixed port per account)',
  'settings.proxyPoolModeDynamic': 'Rotating IPs (session IDs, one fixed session per account)',
  'settings.proxyPoolURL': 'Proxy address',
  'settings.proxyPoolURLHelpOff': 'The address is kept while off so the pool can be turned on again.',
  'settings.proxyPoolURLHelpStatic':
    'Enter the address of any port from your provider, for example http://user:pass@isp.decodo.com:10000; the port is replaced with each port of the range below (one IP per port). Same formats as the default proxy; saved in URL form.',
  'settings.proxyPoolURLHelpDynamic':
    'Write {session} (any case) where the session ID goes in the username or password, for example http://customer-account-cc-US-sessid-{session}:pass@gate.ipfoxy.io:58688; it is replaced with a fixed session ID per account (lowercase letters and digits, at most 15 characters), which changes only when the proxy cannot be reached; bare 429s never change it (each session has a single account, so they cannot tell the account from the IP), and a session that the upstream limits while the proxy still works is not changed automatically. Set a long session duration at your provider.',
  'settings.proxyPoolURLHelpDynamicFixed':
    'Write {session} (any case) where the session ID goes in the username or password, for example http://customer-account-cc-US-sessid-{session}:pass@gate.ipfoxy.io:58688; it is replaced with a fixed session ID per account (lowercase letters and digits, at most 15 characters). Fixed sessions are on: every account keeps its session ID; when a few sessions keep failing to connect they are changed automatically, and if more than the limit fail within 30 minutes, automatic changes stop until you change sessions by hand (a limit of 0 never changes them). Set a long session duration at your provider.',
  'settings.proxyPoolURLPlaceholderStatic': 'http://user:pass@isp.decodo.com:10000',
  'settings.proxyPoolURLPlaceholderDynamic':
    'http://customer-account-sessid-{session}:pass@gate.ipfoxy.io:58688',
  'settings.proxyPoolPortStart': 'First port',
  'settings.proxyPoolPortEnd': 'Last port',
  'settings.proxyPoolPortsHelp':
    'Both ends are included, at most 5000 ports. Give each server (each process) a range of its own, for example 10001–10200 here and 10201–10400 on another; processes on the same machine lock ports and never get the same one.',
  'settings.proxyPoolCapacity':
    '{ports} ports; at most {need} browsers at once (normal {normal}+{normalDrain}, Ultra {ultra}+{ultraDrain}; the second number is replacements started before draining workers close), {spare} spare, and logins and verifications take ports too (default verification concurrency 3).',
  'settings.proxyPoolCapacityPro':
    '{ports} ports; at most {need} browsers at once (normal {normal}+{normalDrain}, Ultra {ultra}+{ultraDrain}, Pro {pro}+{proDrain}; the second number is replacements started before draining workers close), {spare} spare, and logins and verifications take ports too (default verification concurrency 3).',
  'settings.proxyPoolCapacityExtra':
    '{ports} ports; at most {need} browsers at once (normal {normal}+{normalDrain}, Ultra {ultra}+{ultraDrain}, extra pool {extra}+{extraDrain}; the second number is browsers started before draining or replaced workers close), {spare} spare, and logins and verifications take ports too (default verification concurrency 3).',
  'settings.proxyPoolCapacityProExtra':
    '{ports} ports; at most {need} browsers at once (normal {normal}+{normalDrain}, Ultra {ultra}+{ultraDrain}, Pro {pro}+{proDrain}, extra pool {extra}+{extraDrain}; the second number is browsers started before draining or replaced workers close), {spare} spare, and logins and verifications take ports too (default verification concurrency 3).',
  'settings.proxyPoolShortage':
    'Fewer ports than browsers that may run at once ({need} needed): the extra browsers share ports (the same IP) with other accounts. Widen the range or lower the peak worker limit or startup concurrency.',
  'settings.proxyPoolTight':
    'Only {spare} spare: logins and verifications running at the same time may share ports with other accounts. Aim for ports ≥ the most browsers at once + the verification concurrency.',
  'settings.proxyPoolDefaultProxyHint':
    'Once on, accounts without their own proxy stop using the default proxy; only when every port is taken do the few background requests of accounts without a browser fall back to it for a while, returning to the IP pool once a port frees up.',
  'settings.proxyPoolLiveHint':
    'Applies immediately without restarting the generation service (if other saved changes still need a restart, such as the default proxy or the account directory, it applies with that restart): running workers keep their exit and switch after they close; to switch everything now, stop the service at the bottom left and start it again.',
  'settings.proxyPoolInvalidURL': 'Enter the proxy address',
  'settings.proxyPoolInvalidURLPort':
    'Without a scheme the address must include a port: enter the address of any port from your provider, for example isp.example.com:10000 or http://user:password@isp.example.com:10000 (static IP ports are replaced from the port range)',
  'settings.proxyPoolInvalidStaticSession':
    'A static-IP address cannot contain {session}: the port is replaced from the port range',
  'settings.proxyPoolInvalidDynamicSession':
    'A rotating-IP address must contain {session} where the session ID goes in the username or password, for example customer-account-sessid-{session}',
  'settings.proxyPoolInvalidSessionHost': '{session} can only appear in the username or password',
  'settings.proxyPoolInvalidPorts':
    'Ports must be integers from 1 to 65535, and the first port cannot be greater than the last',
  'settings.proxyPoolInvalidPortsTooMany': 'The port range can hold at most 5000 ports',
  'settings.proxyPoolSample': 'Sample exits',
  'settings.proxyPoolSampling': 'Sampling…',
  'settings.proxyPoolSampleHint':
    '"Sample exits" uses the settings currently in the inputs (no need to save first) to test the exit IP of 3 ports (first, middle, last) or 3 temporary session IDs; "Check all ports" queries the exit IP of every port in the range one by one (background request path, 10 at a time, up to 5 minutes) to confirm they all differ. Both contact only api.ipify.org, never Google.',
  'settings.proxyPoolSampleSummary': '{count} exits, {distinct} distinct IPs',
  'settings.proxyPoolCheck': 'Check all ports',
  'settings.proxyPoolCheckRunning': 'Checking {done} / {total}',
  'settings.proxyPoolCheckBusy': 'The previous check is still running; see its progress below',
  'settings.proxyPoolCheckDone':
    'Check finished ({time}): {total} ports, {ok} succeeded, {distinct} distinct IPs, {failed} failed',
  'settings.proxyPoolCheckAllDistinct': 'Every port has a different exit IP',
  'settings.proxyPoolCheckDuplicates': 'Ports sharing an exit IP ({count} groups):',
  'settings.proxyPoolCheckFailed': 'Failed ports ({count}):',
  'settings.proxyPoolStatusStatic':
    'Now: {inUse} / {total} in use · {leases} leases · {cooling} cooling · {shared} shared',
  'settings.proxyPoolStatusDynamic':
    'Now: {inUse} sessions in use · {leases} leases · {rotations} session changes',
  'settings.proxyPoolAffinity': 'This run: returned to the last port {hits} times, could not {misses} times',
  'settings.proxyPoolUnpooled':
    '{count} running workers still use the default proxy (for example, they were running before the IP pool was turned on or before following the default proxy) and switch to the pool after they restart; to switch now, stop the service and start it again.',
  'settings.proxyPoolStale': '{count} browsers still use exits from before the change and return them when they close.',
  'settings.proxyPoolExternal':
    '{count} ports are taken by account proxies, the default proxy, or another process, and are not assigned.',
  'settings.proxyPoolOrphaned': '{count} browsers of deleted accounts failed to close and still hold exits.',
  'settings.proxyPoolRelayLimit':
    'The local proxy relay allows only {limit} connections, which many browsers can exhaust ("connection limit reached"): raise ulimit -n to 65536 or more and restart the program.',
  'settings.proxyPoolPinned':
    '{count} accounts have the default proxy as their account proxy (usually written by setup) and do not use the IP pool.',
  'settings.proxyPoolFollow': 'Follow the default proxy',
  'settings.proxyPoolFollowConfirm':
    'Clear the account proxy of these {count} accounts (follow the default proxy) so that they use the IP pool. Running workers are not restarted: they keep the default proxy and switch to the pool after they close. Accounts busy with another operation are skipped; click again later.',
  'settings.proxyPoolFollowAction': 'Follow',
  'settings.proxyPoolFollowDone':
    'Now following the default proxy: {changed} accounts ({running} of them switch to the pool after their worker closes), {skipped} skipped',
  'app.proxyPoolStatic': 'IP pool {inUse}/{total} · {cooling} cooling · {shared} shared',
  'app.proxyPoolDynamic': 'IP pool (rotating) · {inUse} sessions · {rotations} changes',
  'app.proxyPoolOff': 'IP pool (off)',
  'app.proxyPoolStale': '{count} old exits',
  'app.proxyPoolUnpooled': '{count} on default proxy',
  'app.proxyPoolExternal': '{count} external',
  'app.proxyPoolOrphaned': '{count} held by deleted accounts',
  'app.proxyPoolDetailStatic':
    '{endpoint} ports {start}-{end}: {leases} leases (workers, logins, verifications) · up to {capacity} browsers at once · returned to the last port {hits} times / missed {misses}',
  'app.proxyPoolDetailDynamic':
    '{endpoint}: {leases} leases (workers, logins, verifications) · {rotations} session changes',
  'app.proxyPoolDetailCooling': 'Cooling: {ports}',
  'app.proxyPoolDetailCoolingPort': '{port} (until {until}, level {level})',
  'app.proxyPoolDetailHot': 'Most bare 429s in the last 10 minutes: {ports}',
  'app.proxyPoolDetailHotPort': '{port} ({count} times / {accounts} accounts)',
  'app.proxyPoolDetailShortage':
    'Fewer ports than the {capacity} browsers that may run at once; the extra ones share ports with other accounts',
  'app.proxyPoolDetailEgress':
    'Egress rate limiting counts each port or session as its own egress: {total} egresses, {limited} limited, {rejected} withdrawn (limited or withdrawn egresses are listed above)',
  'accounts.proxyPoolSlot': 'IP pool · {slot}',
  'accounts.proxyPoolActive': 'in use',
  'accounts.proxyPoolLogin': 'logging in',
  'accounts.proxyPoolBackground': 'background',
  'accounts.proxyPoolActiveHelp':
    'A browser of this account holds this exit (with static IPs the port is its own); background requests use the same exit',
  'accounts.proxyPoolLoginHelp': 'This account is logging in or being verified; the browser uses this exit',
  'accounts.proxyPoolBackgroundHelp':
    'No browser is running: background requests (catalog sync, rechecks, renewal) use this exit for now, and a new one is assigned when a worker starts (the last port when possible)',
  'accounts.proxyPoolUnpooled': 'Default proxy (switches to the IP pool after the worker restarts)',
  'accounts.proxyPoolUnpooledHelp':
    'This worker still uses the default proxy (for example, it was running before the IP pool was turned on or before following the default proxy); it switches to the pool after it restarts. To switch now, stop the service and start it again',
  'accounts.proxyPoolDefault': 'Default proxy',
  'accounts.proxyPoolDefaultHelp':
    'Every IP pool port is taken: background requests of accounts without a browser use the default proxy for now',
  'accounts.proxyPoolPinned': 'Pinned to the default proxy (no IP pool)',
  'accounts.proxyPoolPinnedHelp':
    'The account proxy equals the default proxy, so this account does not use the IP pool; switch it under Service Configuration → IP pool with "Follow the default proxy"',
  'settings.proxyPoolFixed': 'Fixed assignment (one fixed port per account)',
  'settings.proxyPoolFixedHelpStatic':
    "Once on, every account that goes through the IP pool (no proxy of its own) gets one fixed port, spread evenly over the ports of the range by account count, for example 2000 accounts on 500 ports → 4 accounts per port (when it does not divide evenly, ports differ by at most 1). The accounts on one port share its IP and can run browsers at the same time; an account's worker, logins, verifications, Chrome imports and background requests all use only its fixed port. It then stays the same across restarts, updates, stopping and starting the service and changes to other settings. When a port cools down (the IP went bad: the exit kept failing, or several accounts were rate limited) a few bad IPs are replaced automatically: the accounts on that port move to the port with the fewest accounts that is not paused, and the bad port is disabled; if more ports go bad within 30 minutes than the limit below, automatic replacement stops and the accounts on ports that go bad afterwards pause until you change the IP by hand; a port held by another process only pauses until it is released (never the default proxy, never a direct connection). The assignment is kept in runtime/proxy-pool-fixed.json: the first time it is turned on, accounts keep the port they used last where possible, accounts added later go to the port with the fewest accounts, and deleting an account frees only its own place.",
  'settings.proxyPoolFixedDynamic': 'Fixed sessions (one fixed session ID per account)',
  'settings.proxyPoolFixedHelpDynamic':
    'Once on, every account keeps its session ID (the same IP); session IDs are kept in runtime/proxy-pool.json and survive restarts and updates. When a few sessions keep failing to connect, the session of that account is changed automatically; if more than the limit below fail within 30 minutes, automatic changes stop and those accounts keep their session (their workers fail to start) until you change sessions by hand in the IP pool status. A limit of 0 never changes sessions.',
  'settings.proxyPoolFixedEnableConfirm':
    'Turn on fixed assignment: {accounts} accounts on {ports} ports, about {perPort} accounts per port (accounts on one port share its IP and can run browsers at the same time; if it was on before, the earlier assignment is kept rather than rebalanced, so use Rebalance after turning it on to even it out). It then stays the same across restarts and updates; when a port cools down (the IP went bad) a few are replaced automatically, and if more go bad within 30 minutes than the limit, automatic replacement stops and they pause until you change them by hand (a limit of 0 only pauses); a port held by another process only pauses. Running browsers switch to their fixed port after they close (to switch now, stop the service and start it again).',
  'settings.proxyPoolFixedEnableConfirmDynamic':
    'Turn on fixed sessions: every account keeps its session ID; when a few sessions keep failing to connect they are changed automatically, and if more than the limit fail within 30 minutes, automatic changes stop until you change sessions by hand (with a limit of 0 sessions are never changed and the worker of that account fails to start until the session recovers).',
  'settings.proxyPoolFixedEnableAction': 'Turn on',
  'settings.proxyPoolFixedDisableConfirm':
    'Turn off fixed assignment: the assignment is kept and used again when it is turned back on; browsers started from now on get ports the usual way (one port per browser, so accounts may change IP), and running browsers switch after they close.',
  'settings.proxyPoolFixedDisableConfirmDynamic':
    'Turn off fixed sessions: sessions are changed automatically again when the exit keeps failing.',
  'settings.proxyPoolFixedDisableAction': 'Turn off',
  'settings.proxyPoolFixedShrinkConfirm':
    'Fixed assignment is on: ports {ports} are not in the new range, so about {accounts} accounts on them move to the ports with the fewest accounts right away and change IP; changing the range back later does not return them to their old ports. Change the port range?',
  'settings.proxyPoolFixedShrinkAction': 'Change range',
  'settings.proxyPoolFixedCapacity':
    '{ports} ports, {accounts} accounts, {perPort} per port (accounts on one port share its IP and can run browsers at the same time; with fixed assignment, browsers may outnumber ports).',
  'settings.proxyPoolFixedDefaultProxyHint':
    'Once on, accounts without their own proxy stop using the default proxy; with fixed assignment they never fall back to the default proxy or a direct connection either: while a port is paused, the accounts on it pause until it recovers.',
  'settings.proxyPoolFixedStatus':
    'Fixed assignment: {ports} ports · {accounts} accounts · {perPort} per port',
  'settings.proxyPoolFixedEmpty': '{count} ports without accounts',
  'settings.proxyPoolFixedReserved': '{count} reserved for logins and imports',
  'settings.proxyPoolFixedStray':
    '{count} recorded accounts are no longer in the account pool (for example, their directory was moved away for a while or failed to load): their places are kept and they return to their port when they come back; "Rebalance" clears them.',
  'settings.proxyPoolFixedUnassigned':
    '{count} accounts that use the IP pool have no port yet (they get the port with the fewest accounts when next used).',
  'settings.proxyPoolFixedTransitional':
    '{count} running workers are not on their fixed port yet (they were running before fixed assignment was turned on or before a move) and switch after they close; to switch now, stop the service and start it again.',
  'settings.proxyPoolFixedExternal':
    '{count} ports with fixed accounts are also used by account proxies or the default proxy: they are used as usual (sharing the IP with those), and you can change the IP of those ports if needed.',
  'settings.proxyPoolFixedPaused':
    '{ports} ports paused ({accounts} accounts): the accounts resume when the port recovers; if the IP is rate limited or keeps failing, change the IP of that port.',
  'settings.proxyPoolFixedPausedUntil': 'until {time}',
  'settings.proxyPoolFixedPausedRecheck': 'next check {time}',
  'settings.proxyPoolFixedPausedAccounts': '{count} accounts',
  'settings.proxyPoolFixedListMore': '{count} more not listed',
  'settings.proxyPoolFixedExcluded':
    '{count} ports disabled (not assigned after an IP change; once restored, new accounts and rebalancing use them again):',
  'settings.proxyPoolFixedRestore': 'Restore',
  'settings.proxyPoolFixedRestoreDone':
    'Port {port} can be assigned again (no accounts were moved)',
  'settings.proxyPoolFixedRestoreNoop': 'Port {port} was not disabled',
  'settings.proxyPoolFixedReassign': 'Change IP',
  'settings.proxyPoolFixedReassignHelp':
    'Move the fixed accounts of this port to the ports with the fewest accounts and disable this port (it can be restored)',
  'settings.proxyPoolFixedReassignConfirm':
    'Change the IP of port {port}: its {moved} accounts move to the ports with the fewest accounts ({targets}), and port {port} is disabled and no longer assigned (it can be restored).',
  'settings.proxyPoolFixedReassignClosing':
    ' The {closing} browsers using this port now close once their requests finish and start on their own fixed ports next time.',
  'settings.proxyPoolFixedReassignEmpty':
    'Port {port} has no fixed accounts: disable it so it is no longer assigned (it can be restored).',
  'settings.proxyPoolFixedReassignDone':
    'Changed the IP of port {port}: moved {moved} accounts, closing {restarted} workers in the background; port {port} is disabled',
  'settings.proxyPoolFixedRebalance': 'Rebalance',
  'settings.proxyPoolFixedRebalanceHint':
    'After adding ports (IPs) or deleting many accounts, "Rebalance" evens out the accounts per port again with as few moves as possible, showing the number of moves before you confirm; moved accounts change IP.',
  'settings.proxyPoolFixedRebalanceConfirm':
    'Rebalance: {moved} accounts will move ({running} of them have a running browser and switch after it closes), and {pruned} records of accounts no longer in the account pool will be cleared; afterwards {accounts} accounts are on {ports} ports, {perPort} per port. Moved accounts change IP.',
  'settings.proxyPoolFixedRebalanceNone': 'Already balanced, nothing to move ({perPort} per port)',
  'settings.proxyPoolFixedRebalanceDone':
    'Rebalanced: moved {moved} accounts, cleared {pruned} records, {perPort} per port',
  'settings.proxyPoolFixedDynamicStatus':
    'Fixed sessions: {accounts} accounts keep their session IDs',
  'settings.proxyPoolFixedNotFixed':
    'Fixed assignment is not on (it needs static IPs with fixed assignment turned on)',
  'settings.proxyPoolFixedNoTarget': 'No other port to assign to',
  'settings.proxyPoolFixedOutOfRange': 'The port is not in the current port range',
  'settings.proxyPoolAutoReplace': 'Replace bad IPs automatically (at most per 30 minutes)',
  'settings.proxyPoolAutoReplaceDynamic':
    'Change bad sessions automatically (at most per 30 minutes)',
  'settings.proxyPoolAutoReplaceHelp':
    'When a port cools down (the exit kept failing, or several accounts were rate limited), the accounts on it move to the port with the fewest accounts that is not paused, the bad port is disabled (it can be restored), and browsers using it close after their current requests finish; once more ports than this go bad within 30 minutes, replacing one more would leave fewer than half of the ports in the range usable (not disabled, not cooling down; ports held by another process are left out), a new IP that accounts were moved to goes bad again, or the IPs of more than half of the accounts went bad, automatic replacement stops (it looks like a provider-wide problem), ports that go bad afterwards pause until you change their IP by hand, and after handling them click "Resume automatic IP replacement". A port held by another process does not count as bad. 0 never replaces (only pauses). 0 to 5000, applied as soon as it is saved (PROXY_POOL_FIXED_AUTO_REPLACE).',
  'settings.proxyPoolAutoReplaceHelpDynamic':
    'When the session of an account keeps failing to connect, that account gets a new session ID automatically; once more sessions than this fail within 30 minutes, a new session fails again, or the sessions of more than half of the accounts fail, automatic changes stop (it looks like a provider-wide problem), later accounts keep their session until you change it below, and after handling them click "Resume automatic session changes". 0 never changes sessions. 0 to 5000, applied as soon as it is saved (PROXY_POOL_FIXED_AUTO_REPLACE).',
  'settings.invalidProxyPoolAutoReplace':
    'The limit for replacing bad IPs must be an integer from 0 to 5000',
  'settings.proxyPoolAutoStatus':
    'Automatic IP replacement: at most {limit} per 30 minutes · {recent} went bad in the last 30 minutes',
  'settings.proxyPoolAutoStatusDynamic':
    'Automatic session changes: at most {limit} per 30 minutes · {recent} failed in the last 30 minutes',
  'settings.proxyPoolAutoHaltedTitle':
    'Too many bad IPs: automatic IP replacement stopped, please handle them',
  'settings.proxyPoolAutoHaltedTitleDynamic':
    'Too many bad sessions: automatic session changes stopped, please handle them',
  'settings.proxyPoolAutoHaltedDetail':
    'Stopped at {time} ({count} went bad within 30 minutes, limit {limit}). Change the IP of paused ports one by one; ports already replaced are not moved back. When done, click "Resume automatic IP replacement".',
  'settings.proxyPoolAutoHaltedDetailHalf':
    'Stopped at {time} ({count} went bad within 30 minutes; replacing more would leave fewer than half of the ports in the range usable and crowd accounts onto fewer and fewer IPs). Change the IP of paused ports one by one; ports already replaced are not moved back. If many ports are disabled, restore some under "ports replaced automatically" first; when done, click "Resume automatic IP replacement".',
  'settings.proxyPoolAutoHaltedDetailChain':
    'Stopped at {time} (a new IP that accounts were moved to went bad again within 30 minutes, which looks like a provider-wide problem; {count} went bad within 30 minutes, limit {limit}). Wait for the provider to recover or change the IP of paused ports one by one; ports already replaced are not moved back. When done, click "Resume automatic IP replacement".',
  'settings.proxyPoolAutoHaltedDetailAccounts':
    'Stopped at {time} (the IPs of more than half of the accounts went bad within 30 minutes, which looks like a provider-wide problem; {count} went bad within 30 minutes, limit {limit}). Wait for the provider to recover or change the IP of paused ports one by one; ports already replaced are not moved back. When done, click "Resume automatic IP replacement".',
  'settings.proxyPoolAutoHaltedSkipped': '{skipped} more went bad since then and are paused as well.',
  'settings.proxyPoolAutoHaltedDetailDynamic':
    'Stopped at {time} ({count} sessions failed within 30 minutes, limit {limit}). Change sessions below one by one; sessions already changed are not reverted. When done, click "Resume automatic session changes".',
  'settings.proxyPoolAutoHaltedDetailDynamicChain':
    'Stopped at {time} (a new session failed again within 30 minutes, which looks like a provider-wide problem; {count} sessions failed within 30 minutes, limit {limit}). Wait for the provider to recover or change sessions below one by one; sessions already changed are not reverted. When done, click "Resume automatic session changes".',
  'settings.proxyPoolAutoHaltedDetailDynamicAccounts':
    'Stopped at {time} (the sessions of more than half of the accounts failed within 30 minutes, which looks like a provider-wide problem; {count} sessions failed within 30 minutes, limit {limit}). Wait for the provider to recover or change sessions below one by one; sessions already changed are not reverted. When done, click "Resume automatic session changes".',
  'settings.proxyPoolAutoHaltedSkippedDynamic': '{skipped} more sessions failed since then and were not changed.',
  'settings.proxyPoolAutoResume': 'Resume automatic IP replacement',
  'settings.proxyPoolAutoResumeDynamic': 'Resume automatic session changes',
  'settings.proxyPoolAutoResumeConfirm':
    'Resume automatic IP replacement: the 30-minute count starts again from 0 and ports that go bad from now on are replaced automatically; ports paused right now are not replaced after the fact (they are used again when their cooldown ends and replaced under the new count if they go bad again), change their IP by hand if needed. While disabled and cooling ports still make up more than half of the range, the next port that goes bad stops automatic replacement again, so restore some under "ports replaced automatically" first.',
  'settings.proxyPoolAutoResumeConfirmDynamic':
    'Resume automatic session changes: the 30-minute count starts again from 0 and sessions that fail from now on are changed automatically; accounts failing right now are not changed at once, their session is changed under the new count when their browser fails to start again (going over the limit stops it again), or change their session by hand first.',
  'settings.proxyPoolAutoResumeDone':
    'Automatic replacement resumed, the 30-minute count starts again from 0 (what was not replaced while stopped stays as it is; handle it by hand if needed)',
  'settings.proxyPoolAutoResumeNoop': 'Automatic replacement was not stopped',
  'settings.proxyPoolAutoTagHalted': 'auto replacement stopped',
  'settings.proxyPoolAutoTagNoTarget': 'no port to move to',
  'settings.proxyPoolAutoReplaced':
    '{count} ports replaced automatically (disabled, accounts moved away; restoring a port does not move accounts back):',
  'settings.proxyPoolAutoReplacedItem': '{time} · {rule} · {moved} accounts moved',
  'settings.proxyPoolAutoRuleUpstream': 'exit kept failing',
  'settings.proxyPoolAutoRuleRateLimited': 'rate limited across accounts',
  'settings.proxyPoolAutoStuck':
    '{count} accounts whose session keeps failing and was not changed:',
  'settings.proxyPoolAutoStuckSince': 'session {session} · since {time}',
  'settings.proxyPoolAutoRotate': 'Change session',
  'settings.proxyPoolAutoRotateConfirm':
    'Change the session of {account}: it uses a new session ID from now on (the provider gives a new IP), and a running browser closes after its current requests finish.',
  'settings.proxyPoolAutoRotateDone': 'Session changed: {from} → {to}',
  'settings.proxyPoolAutoNotDynamic':
    'Fixed sessions are not on (they need rotating IPs with fixed sessions turned on)',
  'settings.proxyPoolAutoNotPooled': 'This account has its own proxy and does not use the IP pool',
  'app.proxyPoolAutoHalted':
    'Too many bad IPs: automatic IP replacement stopped, please handle them',
  'app.proxyPoolAutoHaltedDynamic':
    'Too many bad sessions: automatic session changes stopped, please handle them',
  'app.proxyPoolAutoHaltedDetail': 'Stopped at {time}: {count} went bad within 30 minutes (limit {limit})',
  'app.proxyPoolAutoHaltedDetailHalf':
    'Stopped at {time}: {count} went bad within 30 minutes, replacing more would leave fewer than half of the ports usable',
  'app.proxyPoolAutoHaltedDetailChain':
    'Stopped at {time}: a new IP went bad again within 30 minutes ({count} in all), which looks like a provider-wide problem',
  'app.proxyPoolAutoHaltedDetailChainDynamic':
    'Stopped at {time}: a new session failed again within 30 minutes ({count} in all), which looks like a provider-wide problem',
  'app.proxyPoolAutoHaltedDetailAccounts':
    'Stopped at {time}: the IPs of more than half of the accounts went bad within 30 minutes ({count} in all), which looks like a provider-wide problem',
  'app.proxyPoolAutoHaltedDetailAccountsDynamic':
    'Stopped at {time}: the sessions of more than half of the accounts failed within 30 minutes ({count} in all), which looks like a provider-wide problem',
  'app.proxyPoolAutoHaltedSkipped': ', {skipped} more since then',
  'app.proxyPoolAutoHaltedOpen': 'Handle',
  'app.proxyPoolAutoStopped': 'auto IP replacement stopped',
  'app.proxyPoolAutoStoppedDynamic': 'auto session changes stopped',
  'app.proxyPoolDetailAuto':
    'Automatic IP replacement: at most {limit} per 30 minutes, {recent} went bad recently, {replaced} ports replaced',
  'app.proxyPoolDetailAutoDynamic':
    'Automatic session changes: at most {limit} per 30 minutes, {recent} failed recently',
  'proxyPoolPause.cooling_upstream': 'cooling down (exit kept failing)',
  'proxyPoolPause.cooling_rate_limited': 'cooling down (rate limited across accounts)',
  'proxyPoolPause.cooling': 'cooling down',
  'proxyPoolPause.locked_elsewhere': 'held by another process',
  'app.proxyPoolFixed': 'IP pool · fixed · {perPort} per port · {paused} paused',
  'app.proxyPoolFixedDynamic': 'IP pool (rotating) · {inUse} sessions · fixed',
  'app.proxyPoolDetailFixed':
    'Fixed assignment: {ports} ports · {accounts} accounts · {perPort} per port (accounts on one port share its IP) · ports with browsers {inUse}/{total}',
  'app.proxyPoolDetailFixedPaused': 'Paused: {ports}',
  'app.proxyPoolDetailFixedPausedPort': '{port} ({reason}, {accounts} accounts)',
  'app.proxyPoolDetailFixedTransitional':
    '{count} workers not on their fixed port yet (they switch after closing)',
  'app.proxyPoolDetailFixedDynamic': 'Fixed sessions: {accounts} accounts keep their sessions',
  'accounts.proxyPoolFixedSlot': 'IP pool · port {port} (fixed)',
  'accounts.proxyPoolFixedHelp':
    'Fixed assignment: the worker, logins, verifications and background requests of this account always use this port (the accounts on one port share its IP), across restarts and updates',
  'accounts.proxyPoolFixedPaused': 'paused',
  'accounts.proxyPoolFixedTransitionalHelp':
    'This browser was already running on this exit before fixed assignment was turned on or before a move; it switches to fixed port {port} after it closes',
  'proxy.testAccountClearedFixedSlot':
    'Fixed port after clearing the account proxy, for example: {slot} (the port with the fewest accounts; kept from then on)',
  'proxy.testPoolFixedSlot':
    'With fixed assignment a new login would get: {slot} (the port with the fewest accounts that is not paused; the login port becomes its fixed port)',
  'logs.midWarn': 'warned midway',
  'logs.midError': 'error midway',
  'logs.midHelp':
    'This request logged a {level} midway (such as an account switch or a cooldown); the badge on the left is its final level. Open it to see the timeline',
  'requests.accountFilter': 'Filter by account (email or account ID)',
  'requests.noMatch': 'No requests match the filters',
  'cooldowns.noMatch': 'No cooldowns match the filters',
  'usage.range.quotaDay': 'Quota day · since {time}',
  'usage.range.quotaDayHelp':
    "Starts at the latest midnight Pacific time, when Google's daily quotas reset; balanced routing's daily send counts and quota cooldowns until the next day use the same boundary",
  'accounts.deviceBound': 'Expires ~2h',
  'accounts.deviceBoundHelp':
    "Device-bound session (has __Secure-1PSIDRTS): a copied auth file stops working after about 2 hours and the account then needs a login. Log in again with this service's browser login; on Windows, Chrome import also works (it keeps renewal material and renews in place)",
}

const legacyKeyMap: Partial<Record<TranslationKey, string>> = {
  'app.title': 'pageTitle',
  'app.console': 'pageTitle',
  'app.status': 'status.title',
  'app.start': 'status.start',
  'app.stop': 'status.stop',
  'nav.logs': 'logs.title',
  'nav.accounts': 'worker.title',
  'nav.models': 'chat.model',
  'nav.settings': 'nav.config',
  'nav.playground': 'nav.playground',
  'section.accounts.title': 'worker.title',
  'section.models.title': 'chat.model',
  'section.settings.title': 'config.title',
  'section.playground.title': 'nav.playground',
  'common.loading': 'status.loading',
  'common.save': 'config.save',
  'common.cancel': 'modal.cancel',
  'common.delete': 'worker.delete',
  'common.edit': 'account.edit',
  'common.enable': 'account.enable',
  'common.disable': 'account.disable',
  'common.relogin': 'auth.relogin',
  'accounts.editTitle': 'account.editTitle',
  'logs.level': 'logs.level',
  'logs.source': 'logs.source',
  'logs.allSources': 'logs.allSources',
  'logs.clear': 'logs.clear',
  'logs.autoScroll': 'logs.autoScroll',
  'logs.waiting': 'logs.waiting',
  'logs.all': 'logs.all',
  'logs.info': 'logs.info',
  'logs.warn': 'logs.warn',
  'logs.error': 'logs.error',
  'models.model': 'chat.model',
  'settings.proxy': 'config.proxyAddress',
  'playground.model': 'chat.model',
  'playground.system': 'chat.systemPrompt',
  'playground.prompt': 'chat.placeholder',
  'playground.send': 'chat.send',
  'playground.stop': 'chat.stop',
  'playground.clear': 'chat.clear',
  'playground.placeholder': 'chat.placeholder',
}

export const availableLocales = (Object.keys(legacyLocales) as Locale[]).map((code) => ({
  code,
  label: legacyLocales[code].label,
}))

const savedLocale = readStorage('user_lang') as Locale | null
const locale = ref<Locale>(
  savedLocale !== null && availableLocales.some((item) => item.code === savedLocale)
    ? savedLocale
    : 'zh-CN',
)

// legacyTranslation 读取旧控制台的多语言文本
function legacyTranslation(value: Locale, key: string): string | undefined {
  let current: unknown = legacyLocales[value]
  for (const part of key.split('.')) {
    if (typeof current !== 'object' || current === null || !(part in current)) return undefined
    current = (current as Record<string, unknown>)[part]
  }
  return typeof current === 'string' ? current : undefined
}

// channelLabelKey 返回上游通道的翻译键
export function channelLabelKey(channel: UpstreamChannel): TranslationKey {
  return channel === 'build' ? 'channel.build' : 'channel.playground'
}

// poolTagged 判断请求或用量记录是否带号池标记（经 /ultra 或 /pro 进入）；普通路径没有标记
export function poolTagged(pool: string | undefined): pool is 'ultra' | 'pro' {
  return pool === 'ultra' || pool === 'pro'
}

// poolTagLabelKey、poolTagHelpKey、poolTagClass 为号池标记的文字、悬停说明与样式：pro 为 Pro，其余带标记的为 Ultra
export function poolTagLabelKey(pool: string | undefined): TranslationKey {
  return pool === 'pro' ? 'pool.pro' : 'pool.ultra'
}

export function poolTagHelpKey(pool: string | undefined): TranslationKey {
  return pool === 'pro' ? 'pool.proHelp' : 'pool.ultraHelp'
}

export function poolTagClass(pool: string | undefined): string {
  return pool === 'pro' ? 'tag-pro' : 'tag-ultra'
}

// useI18n 提供旧控制台语言与新增字段翻译
export function useI18n() {
  const t = (key: TranslationKey): string => {
    if (locale.value === 'zh-CN') return zhCN[key]
    if (locale.value === 'en') return en[key]
    const legacyKey = legacyKeyMap[key]
    return legacyKey === undefined
      ? en[key]
      : (legacyTranslation(locale.value, legacyKey) ?? en[key])
  }
  // tf 翻译并替换 {name} 形式的占位符
  const tf = (key: TranslationKey, values: Record<string, string | number>): string =>
    t(key).replace(/\{(\w+)\}/g, (match: string, name: string) => {
      const value = values[name]
      return value === undefined ? match : String(value)
    })
  const setLocale = (value: Locale): void => {
    locale.value = value
    document.documentElement.lang = value
    writeStorage('user_lang', value)
  }
  // errorText 返回界面显示的错误文本：请求没能连到服务（fetch 抛出 TypeError）时显示服务离线
  const errorText = (reason: unknown): string => {
    if (reason instanceof TypeError) return t('app.offline')
    return reason instanceof Error ? reason.message : t('common.error')
  }

  return { availableLocales, locale: readonly(locale), setLocale, t, tf, errorText }
}
