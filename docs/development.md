# 开发与贡献

AIStudio2API 使用 Go 直接调用 Google AI Studio 的 MakerSuite 私有协议，由 Vue 3 管理端展示账户、模型、冷却和请求状态。业务请求、流式解码、账户调度和公开协议适配都在 Go 进程内完成；Camoufox 只保留官方 WAA 初始化与 fresh proof 生成流程，`WAA_BACKEND=go` 时这部分同样由 Go 进程承担，见 [WAA 实现](waa.md)。

管理监听器、公开 API、账户池、协议运行时和内嵌 Vue 管理端位于同一进程。生成服务是可停止、可重新创建的公开 API 服务实例；原始 JSON+protobuf、WebChannel 与 WAA 格式见 [protocol.md](protocol.md)。

## 1. 环境、首次配置与启动

| 场景 | 必需组件 | 说明 |
| --- | --- | --- |
| Release 运行 | `aistudio2api` | 首次启动自动准备 Camoufox，不需要 Python、Node.js 或 Playwright |
| 源码运行 | Go 1.25.0+、Node.js 22.13+ 或 24+、配套 npm | Node.js 只用于构建 Vue 管理端 |
| Windows Chrome 导入 | Windows amd64、稳定版 Chrome | Go 程序直接读取本机 Profile 的 OAuth/DBSC 材料 |

Windows 用户可以直接运行根目录的 `start.bat`。脚本优先启动已有的 `aistudio2api.exe`；源码目录缺少可执行文件时才执行 `npm ci`、前端构建与 Go 构建。程序启动后自动打开管理页面，生成服务初始保持停止；账户登录、日志查看和生成服务启停均在该页面完成。

`start.bat` 的执行顺序如下：

```text
aistudio2api.exe 已存在
  -> 直接运行现有二进制

aistudio2api.exe 不存在
  -> 检查 node、npm、go 与 web/package-lock.json
  -> web/npm ci
  -> web/npm run build
  -> go build -o aistudio2api.exe ./cmd/aistudio2api
  -> 运行新二进制
```

前端或 Go 源码变化需要重建时，先结束当前管理进程并移除旧二进制，再运行 `start.bat`。Windows 在替换运行中的输出文件时可能留下 `aistudio2api.exe~`，它是旧进程占用目标文件期间产生的构建副本。

源码首次启动：

```powershell
cd web
npm ci
npm run build
cd ..
go run ./cmd/aistudio2api
```

管理页面的“账户”页提供 Chrome 批量导入和浏览器登录，也可以重新登录、验证、编辑、启停和删除账户。浏览器登录会在隔离 Camoufox 中完成并自动读取邮箱。`setup` 保留以下四种命令行导入入口：

| 入口 | 命令 | 适用场景 |
| --- | --- | --- |
| 扫描本机 Chrome | `aistudio2api setup` | 交互选择可导入 Profile |
| 指定 Chrome 账户 | `aistudio2api setup --profile <PROFILE>` 或重复使用 `--email` | 批量、确定性导入 |
| 隔离登录 | `aistudio2api setup --login` | 可见 Camoufox 中手动完成 Google 登录 |
| 文件导入 | `aistudio2api setup --storage-state <file>` | 导入 Playwright storage state 结构 |

`setup` 还接受以下参数：

| 参数 | 作用 |
| --- | --- |
| `--chrome-root <DIR>` | 指定 Chrome User Data 根目录 |
| `--proxy <URL>` | 固定到账户初始化、WAA 与业务请求；可带账号密码，写法同 `.env` 的 `PROXY`，不指定时使用 `.env` 的 `PROXY` |
| `--locale <LOCALE>` | 设置账户语言 |
| `--timezone <IANA_ZONE>` | 设置账户时区 |

`--storage-state`、`--login` 与 Chrome 导入参数分别构成文件导入、隔离登录和浏览器导入模式。隔离登录使用 `--login`。`setup` 要求 `AISTUDIO_AUTH_STATES` 指向一个账户目录。

`--proxy` 会同时固定到新账户的初始化、WAA 与业务请求，接受带账号密码的写法（与 `.env` 的 `PROXY` 相同，见下文[代理写法与规范化](#代理写法与规范化)）；显式传入时按 `config.NormalizeProxy` 规范化，`--proxy ""` 表示新账户直连。没有传入时使用 `.env` 的 `PROXY`（`config.Load` 已规范化）：setup 会把全局 `PROXY` 固定写进每个新账户的 `account.json`（`parseSetupFlags` 取 `cfg.Proxy`，`setupAccountConfig` 写入），代理密码轮换后需要逐个账户修改，或清空账户代理改用全局代理。flag 默认值为空，`-h` 与参数解析出错时不会打印 `.env` 里的账号密码；写在命令行的账号密码同机用户能在 `ps` 里看到，建议写进 `.env`（0600）。`--locale` 和 `--timezone` 设置账户环境；Chrome 导入未显式指定语言时读取 Profile 的首选语言。Camoufox 按以下顺序定位：进程环境变量 `CAMOUFOX_PATH`、`runtime/camoufox/`、可执行文件旁的同名目录、Windows 本机 Camoufox 缓存。全部不存在时自动下载当前平台的固定版本。

日常启动只需运行二进制或 Go 入口，再从管理页面启动生成服务：

```powershell
./aistudio2api.exe
go run ./cmd/aistudio2api --listen 127.0.0.1:2048 --open-ui
```

日常服务入口接受以下参数：

| 参数 | 作用 | 默认来源 |
| --- | --- | --- |
| `--auth <PATHS>` | 覆盖本次进程使用的账户文件、目录或逗号分隔路径 | `AISTUDIO_AUTH_STATES` |
| `--listen <HOST:PORT>` | 覆盖管理页面与 API 的监听地址 | `LISTEN_ADDR` |
| `--proxy <URL>` | 覆盖每次新建生成服务实例使用的全局代理；可带账号密码，显式传入时规范化，`--proxy ""` 表示本次直连，flag 默认值为空（`-h` 不打印 `.env` 的值） | `PROXY` |
| `--open-ui` | 启动后打开管理页面 | 无参数启动时为 `true` |

管理页面与 `/api` 控制面在生成服务停止时继续运行。停止生成服务会取消活动生成请求并关闭 WAA worker；再次启动会重新读取账户模型目录。关闭启动窗口或按 `Ctrl+C` 才会退出整个管理进程。

## 2. 目录、组件和运行依赖

```text
cmd/aistudio2api/        薄入口，只调用 internal/app 的 app.Run
internal/app/            配置、账户装配、认证续签、信号、生成服务实例、调度和服务生命周期
internal/setup/          Chrome 导入、storage-state 导入与隔离登录命令
internal/aistudio/       账户、MakerSuite、WAA、模型、工具、上传、媒体和规范事件
internal/api/            OpenAI、Responses、Anthropic、Gemini 与管理端 HTTP 路由
internal/camoufoxnative/ 原生 WebDriver BiDi、WAA bootstrap 与隔离登录
internal/waa/            纯 Go WAA：BotGuard VM、Firefox 形状宿主与 goja 分叉
internal/chromeauth/     Windows Chrome OAuth/DBSC 发现、导入和续签
internal/config/         全局配置的读取、校验、代理规范化和原子写回
internal/proxyrelay/     带账号密码的代理的本机中转（只给 Camoufox 用）
internal/webui/          嵌入并提供 Vue 构建产物
web/                     Vue 3、TypeScript、Vite 和 Tailwind CSS 源码
docs/                    开发流程与私有协议说明
auth/                    每账户配置、认证状态和可恢复运行状态
runtime/camoufox/        Release 使用的 Camoufox 运行时
runtime/requests.db      用量账本（SQLite，纯 Go 驱动），保留 90 天
internal/requestdb/      用量账本的写入、汇总与查询
```

主依赖方向：

```text
cmd/aistudio2api
  -> internal/app
       -> internal/api
       -> internal/aistudio
       -> internal/setup
       -> internal/camoufoxnative
       -> internal/config
       -> internal/webui

internal/setup
  -> internal/aistudio
  -> internal/camoufoxnative
  -> internal/chromeauth
  -> internal/config
```

请求链保持单向：

```text
HTTP route
  -> client protocol decoder
  -> canonical request
  -> capability-aware account lease
  -> AI Studio array encoder
  -> per-account WAA proof
  -> fingerprinted Camoufox GenerateContent transport
  -> incremental response decoder
  -> canonical events
  -> client protocol response
```

WebSocket 入口沿用相同分层：`internal/api` 解码公开协议，`internal/app` 绑定账户和运行状态，`internal/aistudio` 执行 WebChannel 与规范事件转换。公开适配器只消费规范请求与事件；账户文件、WAA 对象、原始数组和资源粘性由 `internal/aistudio` 与 `internal/app` 管理。

Camoufox 由 Go 通过 WebDriver BiDi 直接管理。启动数据面时，服务按 `WARM_WORKER_LIMIT` 与 `WARM_STARTUP_CONCURRENCY` 准备隔离、无头、长驻的账户 runtime，并在需要其他账户能力时替换最久未用的空闲 runtime。同时冷启动的 Camoufox 数受 `WARM_STARTUP_CONCURRENCY` 限制：预热与冷却轮换后的补齐最多占用该数，有请求在等的冷启动（请求现场启动与排队时的后台扩容）另有 1 个保留名额，排队时也先于预热放行；名额只覆盖浏览器启动本身，容量热更新后排队中的启动立即按新值放行。WAA proof 在账户调度锁外生成，执行前后各核对一次 Worker 未被替换或关闭，同账户的状态读取与取 Worker 不再排在 proof 之后。无头 runtime 的页面刷新帧率为每秒 1 帧。每个 runtime 使用关闭时删除的临时 profile，HTTP 磁盘缓存写入账户目录的 `camoufox-cache/`，同一账户重启时复用官网静态资源；同一账户同时存在的第二个 runtime 使用临时 profile 内的缓存。创建临时 profile 的进程在 profile 内持有锁文件，运行时装配时删除锁已释放的遗留 profile。Windows 上每个 Camoufox 进程树加入服务进程持有的 Job，服务进程退出时由系统一并结束。每个 runtime 在官网触发 GenerateContent 并于网络发送前拦截请求，以取得官方 WAA service 与动态请求头；后续业务正文由 Go 编码，在同步官网 prompt 状态并生成 fresh proof 后，通过同一固定指纹页面的原生 `fetch` 发送，响应流由 WebDriver BiDi 分块交回 Go。其他 MakerSuite、Drive 与媒体控制面请求继续使用账户固定出口的 Go HTTP transport。

`WAA_BACKEND=go` 时不定位、不下载也不启动 Camoufox。每个账户 runtime 在服务进程内请求官网页面与 `GetLoggingContext` 得到公开请求头，调用 `Waa/Create` 取得 challenge，按 hash 下载并缓存解释器到 `auth/.waa-interpreters/`，在 `internal/waa` 的 goja 分叉与 Firefox 形状宿主中执行 program。受保护请求携带 Firefox 请求头与账户 Cookie，由账户固定出口的 Go HTTP transport 发送，响应 Cookie 写回账户状态。账户页的浏览器登录在首次使用时准备 Camoufox。完整链路、宿主、生命周期、数据文件与上游变化的定位方法见 [WAA 实现](waa.md)。

## 3. 配置、账户和持久状态

程序从当前目录读取可选的 `.env`，进程环境变量覆盖同名配置：

| 变量 | 作用 | 默认值 |
| --- | --- | --- |
| `AISTUDIO_AUTH_STATES` | 账户文件、目录或逗号分隔的多个路径 | `auth` |
| `LISTEN_ADDR` | HTTP 服务监听地址 | `127.0.0.1:2048` |
| `PROXY_API_KEY` | 公开 API 访问密钥，调用 API 必须携带；留空使用默认值 | `sk-onechat-fun-fun` |
| `PROXY` | setup 与未设置账户代理时使用的固定出口：`http`、`https` 或 `socks5`，可带账号密码（`http://用户名:密码@host:port`、`host:port:用户名:密码` 等，见[代理写法与规范化](#代理写法与规范化)）；`Load` 读入时规范化 | 空 |
| `PROXY_POOL_MODE` | IP 池模式（不区分大小写，空值按 `off`）：`off`、`static`（端口段里每个运行中的浏览器独占一个端口）或 `dynamic`（地址里的 `{session}` 按账户换成固定会话号）；开启后没有账户代理的账户按 IP 池出网，可热更新，见[IP 池](#ip-池) | `off` |
| `PROXY_POOL_URL` | IP 池的上游地址，写法同 `PROXY`（`config.NormalizeProxyPoolURL`：`{session}`、`%7Bsession%7D` 不区分大小写，先换成内部令牌再按 `NormalizeProxy` 规范化，保存为小写 `{session}`）；`static` 不能含 `{session}`、端口会被替换，`dynamic` 必须含且只能在用户名或密码里；`off` 时保留 | 空 |
| `PROXY_POOL_PORT_START`、`PROXY_POOL_PORT_END` | 静态 IP 的端口段（含两端）：任何模式下都要 `1 <= 起 <= 止 <= 65535`，最多 5000 个端口（`MaxProxyPoolPorts`） | `10001`、`10200` |
| `INIT_TIMEOUT` | 单账户初始化超时 | `2m` |
| `REQUEST_TIMEOUT` | 生成请求在首个上游事件之前的总期限（排队、冷启动、WAA proof、文件复制与附件上传、等首字与首事件前的换号都计入，到时 504）；开始输出后改为空闲上限（上游连续这么久既没有事件也没有网络字节才以 504 结束）；整条生成请求最长 `max(1h, REQUEST_TIMEOUT)`。三段都在请求开始时取值，可热更新、只影响新请求，见下文[长生成与中转重试](#长生成与中转重试)。计数、视频、转写等其他请求整段受它限制 | `5m` |
| `FIRST_EVENT_TIMEOUT` | 每次尝试从向上游发送起（WAA proof 之后）等待首个上游事件的上限：超时只取消这一次尝试的上下文、读完其事件流后释放租约，按可重试的上游超时换号（不额外冷却账号），不能再换号时返回 504；`0` 关闭，开启时必须小于 `REQUEST_TIMEOUT`，可热更新 | `0` |
| `WARM_WORKER_LIMIT` | 普通分区（不属于 Ultra 号池的账户）的常驻预热账户数，可热更新 | `5` |
| `MAX_ACTIVE_WORKERS` | 普通分区的活动 Worker 容量上限，必须不小于热池目标，可热更新；浏览器总数最多为两个分区上限之和 | `10` |
| `ULTRA_EXCLUSIVE` | `true` 时 Ultra 号池的账户只服务 `/ultra` 前缀的请求，普通路径只用其余账户；`false` 时普通路径也可以用 Ultra 号池的账户，`/ultra` 仍只用 Ultra 号池的账户；保存后立即生效，手动改 `.env` 后停止再启动生成服务时按重新读取的值生效（进程环境变量优先） | `true` |
| `ULTRA_INCLUDE_PRO` | `true` 时权益为 Pro 的账户也属于 Ultra 号池与 Ultra 分区（`AccountPool.SetUltraIncludePro`），按全部 `ULTRA_*` 设置调度；账户权益本身不变（权益请求头、模型资格、输入上限与更高权益重试仍按 Pro），见下文“号池成员规则”一段。保存后立即热更新（`applyLiveConfig`）；同一次保存里还有需要重建生成服务的字段时随重建生效（`newRuntime` 经 `configurePool` 设置）；进程环境变量优先 | `false` |
| `ULTRA_WARM_WORKER_LIMIT` | Ultra 分区（Ultra 号池的账户）的常驻预热账户数，`0` 表示只按需启动，可热更新 | `2` |
| `ULTRA_MAX_ACTIVE_WORKERS` | Ultra 分区的活动 Worker 容量上限，至少为 1 且不小于 `ULTRA_WARM_WORKER_LIMIT`，可热更新 | `5` |
| `WARM_STARTUP_CONCURRENCY` | 同时冷启动的 Camoufox Worker 数：预热最多占用该数，按需冷启动另保留 1 个名额并优先，可热更新；`WAA_BACKEND=go` 不受此限 | `2` |
| `PER_ACCOUNT_CONCURRENCY` | 单账号同时执行的请求数；一分钟内能发出的生成请求受 `ACCOUNT_RPM_LIMIT` 约束，扩容靠增加 Worker 数，不宜调高 | `2` |
| `ACCOUNT_RPM_LIMIT` | 单账号最近 60 秒最多发出的生成请求数（只计 `generateContent`；上游每账号每分钟 12 次，所有模型、两个通道共用）：满窗的账号按忙碌分类，请求换到其他账号，热 Worker 都满时走现有的后台扩容；只统计本进程，不写冷却表，分钟限额 429 的冷却仍兜底；`0` 关闭（不拦，照常记账），范围 `0..1000`，空值按默认值；可热更新（`AccountPool.SetAccountRateLimit`），见下文“每账号每分钟限速”一段 | `10` |
| `ROUTING_STRATEGY` | 账户轮询 `round-robin` 或粘性优先 `fill-first` | `round-robin` |
| `UPSTREAM_CHANNELS` | 生成请求的上游通道 `playground`、`build`，逗号分隔 | `playground,build` |
| `STREAM_PLAYGROUND_MODELS` | 流式请求优先走 Playground 的模型 ID，逗号分隔：列表中的模型的流式请求每次尝试先不等待地取 Playground 账号，没有立即可用的就按默认条件在全部启用通道中选号；为空时两个通道照常轮询，可热更新，规则见 [Build 通道](build.md) | 空 |
| `PRIMARY_MODELS` | 普通号池的主用模型 ID，逗号分隔，只作用于普通路径（`PoolScopeNormal`；`ULTRA_EXCLUSIVE=false` 时为 `PoolScopeAll`，先于 Ultra 列表查找）：列表中的模型的生成请求最多排队 `PRIMARY_QUEUE_WAIT`，超过即返回 `CapacityBusyError`（429 + `Retry-After`）；为空时所有模型照常排队，可热更新 | 空 |
| `PRIMARY_QUEUE_WAIT` | 主用模型的排队上限，整个请求共用一份、按墙钟计：截止时间为第一次选号前创建预算的时刻加上限，失败的尝试与换号退避都计入，请求自己现场冷启动的浏览器启动时长（最多 `INIT_TIMEOUT`）与候选全部冷却的等待延后截止时间；`0` 表示不排队，不能超过 `REQUEST_TIMEOUT`（未配置时取 `10s` 与 `REQUEST_TIMEOUT` 中较小者），可热更新 | `10s` |
| `ULTRA_PRIMARY_MODELS` | Ultra 号池的主用模型 ID，逗号分隔：作用于 `/ultra` 请求（`PoolScopeUltra`，只查这份列表），以及 `ULTRA_EXCLUSIVE=false` 时普通路径上不在 `PRIMARY_MODELS` 里的这些模型；排队规则与 `PRIMARY_MODELS` 相同，另外没有额度时立即返回 429（候选全部冷却立即返回 `AllCoolingError`，额度 429 后每日类最多再换 2 次账号、限流类（分钟限额与限流）最多再换 5 次，额度失败后紧接着的那次选号只用已启动的 Worker，结束时返回 `PrimaryQuotaError`，见下文“主用模型按号池”一段）。键存在时按写的值（空值表示关闭），键不存在时取默认；进程环境变量的空值与其他配置项一样被忽略，关闭只能在 `.env` 写空值或在管理页清空，可热更新 | `gemini-4-argon` |
| `ULTRA_PRIMARY_QUEUE_WAIT` | Ultra 主用模型的排队上限，规则与 `PRIMARY_QUEUE_WAIT` 相同（整个请求共用一份、按墙钟计；`0` 表示不排队，不能超过 `REQUEST_TIMEOUT`，未配置时取 `10s` 与 `REQUEST_TIMEOUT` 中较小者，保存时写具体值），可热更新 | `10s` |
| `WAA_BACKEND` | WAA 后端 `camoufox` 或 `go` | `camoufox` |
| `TEMPORARY_CHAT` | WAA 预热页是否使用临时对话 | `false` |
| `AUTO_START` | 管理进程启动后自动启动生成服务。生成服务期望运行却停在 STOPPED（启动失败或意外停止）时由监督协程自动重新启动：一般失败按 5 秒起、上限 1 分钟退避，没有可用账户时上限 5 分钟；手动停止后不再自动启动 | `true` |
| `REQUEST_BODY_LOG` | 用量账本同时保存通过 API key 校验的 POST 请求与响应正文，各截断到 64 KiB，只保留最近 1000 条；管理页面保存配置时沿用现值，修改后重启程序生效 | `false` |
| `ADMIN_PASSWORD` | 远程管理密码；非空时非回环请求经 HTTP Basic 认证后可访问管理页面与 `/api/`，同一 IP 10 分钟内错 10 次封禁 15 分钟；管理进程重启后生效 | 空 |

`LISTEN_ADDR` 使用 `host:port`，端口范围为 `1..65535`。时长和容量字段必须为正值（`FIRST_EVENT_TIMEOUT`、`PRIMARY_QUEUE_WAIT`、`ULTRA_PRIMARY_QUEUE_WAIT` 与 `ULTRA_WARM_WORKER_LIMIT` 可以为 `0`，两个主用模型排队上限都不超过 `REQUEST_TIMEOUT`），`WARM_STARTUP_CONCURRENCY` 的有效范围为 `1..WARM_WORKER_LIMIT`，`ULTRA_MAX_ACTIVE_WORKERS` 不小于 `ULTRA_WARM_WORKER_LIMIT`，`ACCOUNT_RPM_LIMIT` 为 `0..1000` 的整数（超出时报 `ACCOUNT_RPM_LIMIT 必须是 0 到 1000 的整数`）。管理页面保存时没有带四个 Ultra 字段、`account_rpm_limit`、`stream_playground_models`、`primary_models`、`primary_queue_wait`、`ultra_primary_models` 或 `ultra_primary_queue_wait`（旧版页面）则沿用 `.env` 中的现值（省略排队上限而把 `request_timeout` 调到现值以下时按 `request_timeout` 封顶）；`stream_playground_models`、`primary_models` 与 `ultra_primary_models` 为空数组表示清空，排队上限为 `0s` 表示不排队。保存的配置 JSON 缺少 `ultra_primary_models`（旧版数据）时取默认列表，显式的空数组为关闭；缺少 `ultra_include_pro` 时按关闭处理；缺少 `account_rpm_limit` 时取默认值 10，显式的 `0` 为关闭。全局代理与账户代理的写法和规范化规则见 `config.NormalizeProxy` 与下文[代理写法与规范化](#代理写法与规范化)。命令行 `--auth` 与 `--proxy` 会覆盖每次启动生成服务时读取的保存值。

`GET /api/config` 与 `PUT /api/config` 同时暴露保存值和当前生效值：

| 字段 | 语义 |
| --- | --- |
| `auth_states`、`proxy`、`init_timeout`、`request_timeout` | 下一次启动生成服务时使用的保存值；`proxy` 是规范化后的上游 URL（含账号密码，与 `proxy_api_key` 同级明文返回），`PUT` 时先规范化再写入 `.env`，响应里同样是规范值，不存本机中转地址 |
| `first_event_timeout` | 每次尝试的首事件超时，保存后立即生效；`0s` 为关闭，提交时省略则沿用现值 |
| `stream_playground_models` | 流式请求优先走 Playground 的模型（字符串数组，读取时总是返回），保存后立即生效；提交时省略则沿用现值 |
| `primary_models`、`primary_queue_wait` | 普通号池的主用模型（字符串数组，读取时总是返回）与它们的排队上限（时长文本，`0s` 为不排队，不超过 `request_timeout`），保存后立即生效；提交时省略则沿用现值（排队上限超过新的 `request_timeout` 时按它封顶） |
| `ultra_primary_models`、`ultra_primary_queue_wait` | Ultra 主用模型（字符串数组，读取时总是返回，关闭时为空数组）与它们的排队上限，规则同上，保存后立即生效（不需要重启生成服务）；提交时省略（`null` 或空串，旧版页面）则沿用现值 |
| `warm_worker_limit`、`max_active_workers`、`warm_startup_concurrency`、`per_account_concurrency` | 下一次启动生成服务时使用的容量参数 |
| `account_rpm_limit` | 单账号每分钟生成请求上限（整数，`0` 为关闭，读取时总是返回），保存后立即热更新到当前生成服务（`applyLiveFields` 调 `SetAccountRateLimit`）；提交时省略（旧版页面）则沿用现值。只改它不需要重启生成服务：`liveFieldsEqual` 与 `sameDataConfig` 都比较它，`requiresRebuild` 不含它 |
| `proxy_pool` | IP 池设置 `{mode, url, port_start, port_end}`（读取时总是返回；`url` 与 `proxy` 一样以规范全文返回，只在这一个接口出现），保存后立即热更新（`applyLiveFields` 调 `proxyPool.configure`），不需要重启生成服务；提交时省略（旧版页面）则沿用现值，端口为 0 或组合不合法返回 400 `invalid_config` |
| `ultra_exclusive`、`ultra_include_pro`、`ultra_warm_worker_limit`、`ultra_max_active_workers` | Ultra 号池设置（读取时总是返回），保存后立即热更新到当前生成服务；提交时省略（旧版页面）则沿用现值。只改这些字段不需要重启生成服务：`sameDataConfig` 与 `liveFieldsEqual` 都比较这四个字段，`requiresRebuild` 不含它们 |
| `temporary_chat`、`waa_backend`、`upstream_channels` | 下一次启动生成服务时使用的 WAA 与上游通道配置 |
| `listen_addr`、`proxy_api_key` | 保存的管理监听配置 |
| `active_listen_addr`、`active_proxy_api_key` | 当前管理进程固定使用的值 |
| `management_restart_required` | 保存的监听地址或 API key 与当前管理进程不同 |
| `service_restart_required` | 保存的生成服务配置与当前生成服务实例不同 |

配置保存使用临时文件、`Sync` 和原子替换。监听地址与本地 API key 由管理进程持有，进程重启后应用；其余配置在停止并再次启动生成服务后应用。

生成服务启动顺序如下。源码中的 `generation` 表示一次 Stop/Start 创建的生成服务实例：

```text
PUT /api/config
  -> 校验并原子写入 .env
  -> 返回 saved/active 差异

POST /api/control/stop
  -> 取消 LAUNCHING、活动请求与后台扩容中的 Worker 启动
  -> 等待模型目录刷新退出并关闭当前 Worker
  -> 管理监听器继续提供 /api 与页面

POST /api/control/start
  -> 完成当前已停止实例的清理
  -> 重新读取 .env 并应用命令行 --auth/--proxy 覆盖（PROXY 两边都已规范化，是上游 URL，不是本机中转地址）
  -> 创建并启用新的生成服务实例
  -> 从当前 generation 的 CachedModels 建立内存目录
  -> 并发刷新全部 enabled ready/busy 账户
  -> 冷 generation 等待首个非空真实目录
  -> 启动首个 WAA Worker并进入 RUNNING；首轮目录同步完成前没有可预热账户时，同步完成后再预热一次
  -> 剩余目录同步与热池预热继续在后台运行
```

配置读取、校验、生成服务实例创建失败或启用前取消时，当前已停止实例保持不变。切换到新实例后启动失败时，该实例进入 `STOPPED`，管理端返回结构化错误。模型目录保存在当前生成服务实例的内存中；正常 Stop/Start 创建的新实例从空 `CachedModels` 冷启动。当前实例已有真实缓存时，`trackedService.Start` 可直接使用该缓存预热 Worker，同时继续刷新全部账户。

模型目录刷新与当前生成服务共用 context（Go 取消信号）。启动失败、取消或 Stop 后等待刷新退出的上界为 2 秒；未完成的 lifecycle transition（启动或停止操作）最多等待 12 秒。超时与 Worker 清理错误通过 `errors.Join` 保留在同一错误链中。

每个账户目录包含：

| 文件 | 内容 | 生命周期 |
| --- | --- | --- |
| `account.json` | label、enabled、proxy、locale、timezone | 创建或编辑账户时写入，`proxy` 保存规范化后的上游 URL（含账号密码明文，文件 0600），不存本机中转地址；外部手写的冒号写法读入时规范化（`readAccountConfig`，读时不回写，下次保存时写入规范值）；缺少 `enabled` 字段按启用读取。账户默认启用：启动与目录扫描导入时，`enabled` 为 `false` 但 `.disable-reasons.json` 中没有手动停用记录的账户在加入账户池之前改为启用并原子写回（16 个并行，只写一条汇总日志）；只有管理页或管理接口停用的保持停用。手动停用记录立即写入，目录移出账户池时保留；`.disable-reasons.json` 无法读取时没有记录的停用账户保持停用并补记为手动停用 |
| `storage-state.json` | Cookie、localStorage 与可选 Chrome OAuth/DBSC 续签材料 | 合并 `Set-Cookie` 或认证续签后原子写回 |
| `camoufox-fingerprint.json` | 账户固定的浏览器指纹、语言与时区 | 首次运行生成，空值、窗口几何、字体、语音与媒体设备按 Camoufox 官方启动器规则规范化；重新登录和 WAA runtime 继续复用 |
| `runtime-state.json` | 权益等级、模型资格、冷却与资源账户绑定 | 权益同步、首次模型结果或资源变化后原子写回 |
| `camoufox-cache/` | 该账户 Camoufox 的 HTTP 磁盘缓存，上限 256 MB | WAA runtime 运行期间独占写入，随账户目录删除 |

`runtime-state.json` 的 `model_access` value 为 `{state,checked_at,reason?}`，成功状态为 `verified`；`cooldowns` value 为 `{until,reason?}`；`resources` value 保存 kind、name、mime、size、purpose、created_at 与可选 video 元数据。Drive file、Veo operation、视频产物和 Bidi 恢复 token 均保持创建账户粘性。Veo operation 额外保存公开 video object 的 model、seconds、size 与 UTC 创建时间，生成服务或进程重启后的轮询继续投影相同字段。

活动请求锁保护跨进程账户租约，短事务锁保护 `runtime-state.json` 合并写回。短事务锁位于 `auth/.leases/<账户>.runtime.lock`，以 25ms 间隔等待，最多 2 秒；取得锁后重读磁盘状态，只修改目标字段，原子替换文件，再同步内存与资源索引。接收 context 的资源事务会在取消或 deadline 到达时提前返回。

账户更新以 `account.json` 原子写入为持久提交点。`internal/app` 先准备 `pending`（尚未提交）的固定出口，关闭旧 Worker并锁定该账户的 Worker 配置，再调用 `AccountLease.SaveConfig`；保存成功后依次提交 Worker 配置与固定出口。准备、关闭或保存失败时丢弃 pending 更新；保存后的租约释放错误原样返回，已发布配置继续生效。

账户调度先按每个账户实时 `ListModels` 返回的模型和方法筛选，再选择已经就绪且有并发槽位的 Worker。`ROUTING_STRATEGY=round-robin` 在每个模型的候选账户间轮询，按上次选中的账户 ID 继续；`fill-first` 持续使用 ID 排序后的首个可用账户，并在并发槽位用满、冷却或不可用时切换。生成请求的候选为账户与 `UPSTREAM_CHANNELS` 所启用通道的组合：Playground 按 `ListModels` 与权益判断，Build 按 Build 代理返回的 Gemini API 模型目录与相同权益判断；轮询与粘性在组合间按账户 ID 与通道顺序推进，冷却按通道记录（Build 为 `build:<模型>`），一个通道冷却后同一账户可由另一通道继续，全部启用通道都冷却时按下文的冷却规则排队或返回 429，通道规格见 [Build 通道](build.md)。`STREAM_PLAYGROUND_MODELS` 中的模型的流式请求每次尝试先用 `acquireWarmLeaseNow` 不等待地取 Playground 账号（不加入调度等待队列，只在 Worker 分区有空槽、冷启动名额空闲时现场启动），没有立即可用的账号时返回 `noImmediateLeaseError`，随即按默认条件选号：选择条件带 `PreferPlayground`，只调整候选顺序（每个账户先 Playground，轮询只在账户之间推进），不限定通道；不等待阶段现场启动失败的账户经 `workerStartupFailures` 交给这一步，不再启动。主用模型（按请求的号池查 `PRIMARY_MODELS` 或 `ULTRA_PRIMARY_MODELS`，见下文“主用模型按号池”一段）的生成请求带一份排队预算（`leaseWaitBudget`，`generateWithRetry` 在第一次选号前创建，换号重试共用，经 context 交给 `acquireLease`，签名不变）：截止时间按墙钟计，为创建时刻加 `PRIMARY_QUEUE_WAIT`，选号的分类与复查、等待冷启动名额、失败的上游尝试（403、额度 429）与换号退避都计入；请求自己现场冷启动的浏览器启动本身（`startReservedWorker` 实测，合计不超过 `INIT_TIMEOUT`）与候选全部冷却的等待延后截止时间。截止时间已过后账号忙碌、没有空闲槽位、Worker 启动或扩容中这些容量原因的等待立即返回 `CapacityBusyError`（对外 429 与 `Retry-After`，建议间隔为排队上限限定在 1 到 30 秒，上限为 0 时 5 秒），失败后的下一次尝试只接受立即可用的账号（截止时间已过时也不再退避）；`generateWithRetry` 立即结束请求并在进度里写“主用模型排队超过 Xs，立即返回 429”（带已排队、总耗时、第几次尝试与超限这一刻的候选快照：`waitQueued` 在 `CapacityBusyError` 上填 `Detail`，即这次复查的 `queueSnapshot` 加主用模型供给缓存里的驻留冷却数，见下文“主用模型供给”一段；`Detail` 不进 `Error()`，账本与对外文案不变），不换号、不冷却、不退避；等待冷启动名额最多等到截止时间（名额被占满而截止时间已过时按容量已满返回，不在名额队列里等前面的浏览器启动完成）；预算不用 context 超时，不会取消进行中的启动；候选全部冷却的等待不受限，仍按下文的冷却规则（Ultra 主用模型除外：立即返回 429）。流式请求写出响应头前对主用模型多等排队上限加 5 秒的首事件（`StreamStartWait(ctx, model)`，按 context 中的号池取对应列表的上限），换号重试不再得到新预算，号池繁忙的 429 因此以 HTTP 状态返回；请求自己现场冷启动或候选冷却延后了截止时间时仍可能晚于这个等待，此时响应头已按 200 写出，错误在流内返回（Ultra 主用模型的请求自己现场冷启动时通常不会，见下文 `StreamStartHold`）。号池的同时生成数约为 `MAX_ACTIVE_WORKERS × PER_ACCOUNT_CONCURRENCY`，每分钟生成次数不超过 Worker 数 × `ACCOUNT_RPM_LIMIT`（见下一段），快速失败只是对症状的处理，扩容靠增加 Worker 数，不靠调高单账户并发。并发槽位、Worker 与 WAA 按账户共享；计数、Live、Veo、转录与 Drive 文件引用使用 Playground RPC。每个账号最多同时租用 `PER_ACCOUNT_CONCURRENCY` 个请求槽位；首个请求获取跨进程文件锁，最后一个请求释放。WAA proof 由账号 worker 串行生成，`GenerateContent` 由同一 Camoufox 页面并发发送并流式读取（`WAA_BACKEND=go` 时由账户 runtime 的 Go HTTP 发送）；请求前使用 Worker 当前 Cookie 生成 Authorization，响应头到达后把 Worker Cookie 原子同步到账户持久状态。其他 MakerSuite HTTP 响应的 Cookie 在响应头到达时与最新账户状态合并。未固定账户的请求遇到可重试的 401、403、404、429、5xx 或单账户初始化超时时，可以在首个上游语义事件前继续切换尚未尝试的同能力账户（`generateWithRetry` 以排除列表 `ExcludedAccountIDs` 排除已尝试的账户，选号留在候选索引的快路径上，换号重试的请求与同模型的请求同队排队；队列按请求的到达序号排列，`generateWithRetry` 在第一次选号前取一个序号，换号重试排回请求到达时的位置，见 `dispatchQueue.joinAt`；带排除列表的队首被唤醒却仍要等待时、以及现场冷启动备用账号之前，把唤醒交给下一个请求，冷启动前并离开队列，见 `dispatchQueue.passOn`；转录与 Live 的候选列表整个请求不变，同样用排除列表排除已尝试的账户）；租约账号在请求发出前进入冷却（`beginGenerationSend` 返回 `AccountCoolingDownError`，`errors.Is(err, ErrAccountCoolingDown)` 成立，请求没有发到上游，多半是同账号别的请求刚遇到额度 429）时按已冷却换号：任何模型都不计入连续失败上限（`generateFailureLimit`）、不退避，这次尝试照常记入已尝试的账户；普通模型不能再换号时对外 429（`Retry-After` 为拦下它的冷却的恢复时间），不再落到 500，Ultra 主用模型另按下文“Ultra 主用模型的额度快速失败”一段（被剩余不少于 15 分钟的冷却拦下时记一次每日类额度失败，超过换号上限或没有其他可换的账号时以 `PrimaryQuotaError` 结束，`Retry-After` 为它的建议重试；被更短的冷却拦下、此前也没有额度失败时同普通模型）；没有文件绑定的生成请求遇到单个模型的 403 无权限时，对该账号该通道的这个模型写入冷却（`forbidden_guard.go` 按账号与冷却作用域记档，两个通道分别计：第一次 30 分钟，24 小时内再次 6 小时，第三次起到太平洋时间次日零点、至少 6 小时；同一通道本档冷却未到期时在途请求的 403 不升档；该通道上该模型成功一次即清零），换号不退避，但仍计入连续失败上限（`generateFailureLimit`），同一账号另一通道可用时再试一次，所有启用通道都冷却时经 `retireCooledWorker` 登记淘汰驻留 Worker（只在其他模型上冷却的忙碌 Worker 不排空，空闲后淘汰）；同一号池 10 分钟内至少 5 个不同账号对同一模型返回 403、这段时间该模型又没有任何成功时，按号池级 403（proof、请求形态或 WAA binding 失效等）处理：不写冷却、不升档、不登记淘汰，按一般失败计入连续失败并退避（`observeModelDenied`）；同一账号 10 分钟内对 2 个以上模型连续 3 次 403 时另外暂停整个账号 30 分钟；Drive 引用随需要临时复制到生成账户，显式账户和 Veo operation 保持账户绑定。Chrome 导入状态保留续签材料，HTTP `401`、Code 16 或启动时跳转登录页时在同一固定出口续签一次、重建该账户 WAA runtime 并重放请求；无法续签的账户标为 `auth_required`。Worker 失败与协议 Code 5 要关闭当前 Worker 版本，关闭交给后台（`internal/app/worker_reset.go`）：`scheduleWorkerReset` 按（账户，Worker 版本）登记，同一版本只关闭一次，并发失败的请求与失败 Worker 巡检（`sweepFailedWorkers`，只登记不等待）共用；请求的租约经 `AccountLease.ReleaseToExclusiveReservation` 在账户池的同一次加锁内释放并登记独占等待（`AccountReservation`），排队的请求不会在两者之间取得这个账户，登记保持到关闭结束，期间账户不发出新租约；后台最多等排空超时（总时长上限 + 30 秒，总时长上限为 1 小时与 `REQUEST_TIMEOUT` 中较长者：开始输出的生成请求最长持续到它，见[长生成与中转重试](#长生成与中转重试)），请求取消后照常继续，`ResetAll` 与 `Close` 取消并等后台 goroutine 退出。`generateWithRetry` 中不固定账户、还能换号的请求只等 `defaultWorkerResetWait`（2 秒；主用模型最多 1 秒且不超过剩余排队预算的四分之一），等不到就把原账户重新记为已尝试并换号（计入连续失败上限、不退避），换号返回 `ErrNoEligibleAccount`、全部冷却或没有立即可用账号时回到原账户一直等关闭结束后重放；指定账户、文件绑定的请求以及 Veo 的恢复照旧等关闭结束后在原账户重放（Live 不固定账户、还有其他候选时同样只等 2 秒就换号，转录只在协议 Code 5 时如此，见[长生成与中转重试](#长生成与中转重试)）；失败时请求已取消或不再重试只登记、不等待。解释器更新后整个号池的驻留 Worker 都可能返回 Code 5，换号会落到另一个同样过时的 Worker，这是缩短等待的代价。

每账号每分钟限速（`ACCOUNT_RPM_LIMIT`，`internal/aistudio/rate_limit.go`）：上游每个 Google 账号每分钟最多 12 次生成请求（`quota_limit=GeneratePerMinutePerProjectPerUser`，指标 `generate_requests_global`，所有模型、两个通道共用），撞上后返回分钟限额 429 并冷却整个账号到窗口结束。账户池为每个账户在内存中保留最近 60 秒的发送时刻（`accountRate`，受 `p.mu` 保护，第一次取得生成租约时创建，最多保留 1024 条），不新增锁或常驻 goroutine，号池只有一个窗口释放计时器（见下文）。只计 `Method=="generateContent"` 的选号（Playground、Build、Interactions 与转写）：`countTokens`、方法为空的选号与 `AcquireAccount` 独占租约不拦也不预占；Live（`bidiGenerateContent`，租约持有整个会话）与 Veo（`predictLongRunning`）不拦不计，交给 429 兜底。记账分三步：`leaseCandidateLocked` 在冷却判断之后检查准入（窗口内已发出数 + 预占数小于上限），通过后在同一次加锁内为生成租约预占一个名额（`ratePending`），所以同账号的并发租约加起来也不会超出；`beginGenerationSend`（`WorkerProtectedTransport.doBrowserPrepared` 在 WAA proof 之后、发往上游之前调用，取代原来的 `CoolingDown` 检查，加锁次数不变）在同一次加锁内先看租约通道与全局冷却，冷却生效时返回 `AccountCoolingDownError` 且不记录，否则抵掉本租约的预占（只抵一次）并追加一条不减的发送时刻；`releaseLocked` 退还没发出的预占（独占租约与 `ReleaseToExclusiveReservation` 同样经过这里）。401 续签后的重放会再进入一次 `doBrowserPrepared`，再检查一次冷却、再记一次发送，但不再检查准入，最多比上限多 1 次（默认 10 比上游的 12 留了余量）；不经 `WorkerProtectedTransport` 发送的调用方用 `AccountLease.RecordGenerationSent`。满窗（窗口内已发出数 + 预占数不小于上限）的账号按忙碌分类：候选索引按下标缓存按当前上限算出的窗口已满截止时刻（`schedulingIndex.rateUntil`，`rateChangedLocked` 是修改它的唯一入口，修剪窗口外记录后重算并写回索引；`verifySchedulingIndexLocked` 核对缓存值与重新计算的结果），索引的三处分类与逐个判断的参考实现（`classifyCandidate`）都把满窗的热账户归入 `WarmBusy`、备用账户归入 `StandbyBusy`，并计入 `AccountCandidateGroups.RateLimited`（排队进度的 `每分钟已满=N`）。所以热账户全部满窗、又有备用账户且分区有空位时走现有的“热 Worker 全忙才后台扩容”；分区已满时按“账号全部忙碌”排队：主用模型受排队预算限制，到期返回 `CapacityBusyError`，其他模型一直等到窗口释放。满窗不写冷却表，不影响 `EarliestCooldown`、全部冷却、退休、淘汰、死槽位、主用模型供给与账户对外状态，也不计 Ultra 主用模型的额度失败。窗口按时间释放时号池发调度信号：账户池只有一个 `time.AfterFunc` 计时器（`rateWake`），指向所有截止时刻为有限值的满窗账户（`rateFull`）中最早的释放时刻，`rateChangedLocked` 算出更早的截止时刻时把它提前；回调在有账户的窗口已释放时 `notifyLocked`（经 `forwardPoolChanges` 唤醒各队首，与租约释放一样按到达序号排队，不再由兜底复查先到的请求或新到达的请求抢走名额），再扫描 `rateFull` 按剩下的最早截止时刻重设计时器（没有新事件时截止时刻不变，扫描结果准确；已移除、已不满或改为只因预占而满的账户移出集合）。号池没有关闭操作，丢弃的号池最多再按剩下的截止时刻触发到窗口结束，回调只操作自己的号池。窗口释放时刻同时并入 `leaseCandidateLocked` 的 `earliest`，`AcquireFor` 按它定时等待，`acquireLease` 的每种等待另有 1 秒兜底复查（`schedulingRecheck`）。只因预占而满（`rateUntilPending`）时不随时间恢复，等租约发出或释放：释放时 `releaseLocked` 唤醒；最后一个预占经发送抵掉、截止时刻转为有限值时（上限小于单账户并发或热改小上限时会出现）`rateChangedLocked` 唤醒一次，`AcquireFor` 的等待方改按新的截止时刻定时，普通发送不唤醒。文件绑定（`ResourceID` 非空）的选号不拦（拦下会让它原地等待，不再走“所属账号不可用就复制文件到其他账号”的路径），但照常预占与记账；指定账户（`AccountID`）的选号照常拦。上限为 0（关闭）时不拦但照常预占与记账，重新打开时窗口仍然准确。`SetAccountRateLimit` 与 `SetPerAccountConcurrency` 一样在 `applyLiveFields` 持有 `manager.mu` 时调用（锁顺序 `manager.mu` 再到 `p.mu`），值变化后对有记录的账户重算截止时刻并 `notifyLocked` 唤醒排队的请求（调大或关闭后可能立即可用）；`NewAccountPool` 构造时上限为 0，生产由 `configurePool` 设置。计数只统计本进程，不写运行态文件，进程重启或重建生成服务后从空窗口开始；多进程共用账号、网页手动使用或墙钟前跳时可能少算，由分钟限额 429 的冷却兜底（墙钟回拨时写入不减的时刻，最多多算）。某个租约刚好占满账号的窗口时只在租约上标记（`rateFilled`），`generateWithRetry` 写日志前调用 `AccountLease.ClaimRateWindowNote`，在 `p.mu` 下判断并记下 `notedAt`，同一账号 10 分钟最多写一条 INFO `账号每分钟请求已满`（见 [运行日志](logging.md)）；转写经 `AcquireFor` 自取的租约、启动失败后释放的租约等不写日志的路径占满窗口时不占用这 10 分钟的名额。

主用模型按号池（`internal/app/primary_models.go`）：`primaryModelSettings` 保存普通列表（`PRIMARY_MODELS`、`PRIMARY_QUEUE_WAIT`）与 Ultra 列表（`ULTRA_PRIMARY_MODELS`、`ULTRA_PRIMARY_QUEUE_WAIT`，默认 `gemini-4-argon`）两份，`setPrimaryModelSettings` 整体原子替换，启动时与服务配置保存后立即生效（热更新日志的主用模型概况带上 Ultra 列表）。`primaryPolicyFor(pool, model)` 按请求的号池查找：`PoolScopeUltra`（`/ultra`）只查 Ultra 列表，`PoolScopeNormal`（`ULTRA_EXCLUSIVE=true` 的普通路径）只查普通列表，`PoolScopeAll`（`ULTRA_EXCLUSIVE=false` 的普通路径）先查普通列表、命中按普通规则，没有再查 Ultra 列表；命中 Ultra 列表的预算带 `quotaFailFast`（`leaseWaitBudget.failFastOnQuota()`，没有预算时为假），普通列表的预算没有，行为与原来相同。只在普通列表里的模型在 `/ultra` 上不再有排队上限。`StreamStartWait(ctx, model)` 按 `aistudio.PoolScopeFromContext(ctx)` 用同一规则取排队上限加 `primaryStreamStartGrace`（5 秒），不在对应列表里时返回 0；API 层传来的已是规范模型 ID，这里不再解析别名（那会在热路径上拿全局账户池锁）。API 层（`api.StreamStartWaitProvider`）与 `runtimeManager` 的转发都靠接口断言取这个方法，类型断言本身不检查签名，签名不一致只会静默返回 0；`internal/app/lifecycle.go` 因此对 `trackedService` 与 `runtimeManager` 各加一条 `var _ api.StreamStartWaitProvider` 编译期断言，`runtimeManager` 的转发也断言这个导出接口，改签名时两边一起报错。流式请求另经 `aistudio.StreamStartHold` 推迟写出响应头：API 层（`server.generate`，五种协议的流式入口共用）为每个流式请求创建一份，放进交给 `Generate` 的 context，`awaitStreamStart` 读同一份；Ultra 主用模型（`quotaFailFast`）的预算在 `generateWithRetry` 里取它（`holdStreamStart`），请求自己现场冷启动时 `startReservedWorker` 在浏览器启动前调用 `beginColdStart`，把这次启动还能抵扣的时长（`INIT_TIMEOUT` 减去已抵扣的）交给 `BeginColdStart`，抵扣已用完时不推迟；启动后 `creditColdStart` 结束标记并把实际抵扣的时长交给它。启动进行中 `awaitStreamStart` 的截止时间延后这次启动还能抵扣的时长，结束后改为延后实际抵扣的时长，与排队截止时间同步，推迟合计不超过 `INIT_TIMEOUT`（原先启动进行中一直不写出响应头，第一次启动挂到 `INIT_TIMEOUT` 失败后再现场冷启动一个，响应头要压到 `StreamStartWait` 加约两倍 `INIT_TIMEOUT`，多等的时间抵扣不了，额度不足照样在流内返回）。`awaitStreamStart` 另把截止时间限制在开始等首个事件后 `streamStartHoldMax`（50 秒，`StreamStartWait` 更长时按它）以内：推迟期间连接上没有任何字节，前面的反向代理读不到响应头时按读超时断开（nginx 默认 60 秒），断开还会取消请求与进行中的启动。第一次尝试现场冷启动备用账号超过 `StreamStartWait`、随后额度 429 的 `PrimaryQuotaError` 因此仍以 HTTP 429 返回，不是 200 之后的流内错误；启动更久、超过这两个上限时照常先回 200，其后的错误在流内返回。普通号池的主用模型不取它，行为不变；预热与后台扩容没有预算，也不改动它。Worker 管理器同样按分区保存两份列表（`primaryModelListFor(partition)`）：Ultra 分区只用 Ultra 列表，其余只用普通列表，`ULTRA_EXCLUSIVE=false` 时也不合并，免得普通号池的主用模型去淘汰数量少的 Ultra Worker；死槽位模型（`deadSlotModelsFor`，热门模型并上该分区的主用列表）、替补挑选、预热排序（`preferUncooled`）、空闲回收与淘汰登记都按 Worker 所在分区取列表，按选号号池判断时用 `deadSlotModelsForScope`（Ultra、普通各取本分区，`PoolScopeAll` 取两者并集）。

Ultra 主用模型的额度快速失败只对带 `quotaFailFast` 的预算生效。选号时候选全部冷却：`acquireLease` 不再等待 1 分钟内恢复的冷却，也不延后截止时间，立即返回 `AllCoolingError`（`Retry-After` 为最早恢复），进度写“Ultra 主用模型候选全部冷却，立即返回 429”；降级判定的 `PlaygroundFirst` 选号得到全部冷却时调用方仍按原逻辑改为不限通道选号（`playgroundUnavailable`），不直接对外 429；还有没冷却的备用账号、只是没有槽位（`waitReasonNoSlot`）时仍是容量原因，计入排队上限。请求中遇到额度 429：`generateWithRetry` 另记额度失败次数（`primaryQuotaTracker`），与 `generateFailureLimit`、`maxAttempts`、`hardFailures` 分开，并按 `primaryQuotaClassOf` 分两类：每日类（`DailyQuotaKind`，或剩余不少于 `rotationMinRemaining` 即 15 分钟的冷却，例如 RetryInfo 很长的分钟限额或限流）与限流类（`MinuteQuotaKind` 分钟限额、`RateLimitKind` 没有额度周期信息的限流；连续裸限流升到第 3 档起冷却不少于 30 分钟，按剩余时间归入每日类，见下文“连续裸限流逐级冷却”一段）。`applyQuotaCooldown` 成立算一次（输入超过账号上限的 429 不算；冷却没有写入时同样算）；租约账号在请求发出前进入冷却（`ErrAccountCoolingDown`）只在拦下它的冷却剩余不少于 15 分钟时记一次每日类（`coolingDownLong`：`AccountCoolingDownError` 按它的 `Until`，不带恢复时间的裸哨兵按 `AccountLongCooling`），短冷却的连带拦截不计数；每日类按账号计（`record` 用 `primaryQuotaTracker.dailyAccounts` 去重），同一账号在另一通道的探测、或发出前被长冷却拦下时再遇到每日类不重复计，限流类每次都计；两类都不计入 `hardFailures`、不退避。最近一次额度失败所在类别超过它的换号上限时（`exhaustedReason`：每日类为 `primaryQuotaSwitchLimit`=2，即一个请求最多 3 个账号遇到每日类额度失败，原因“额度失败已达换号上限”；限流类为 `primaryRateLimitSwitchLimit`=5，与 Ultra 分区默认峰值 `ULTRA_MAX_ACTIVE_WORKERS=5` 对齐，原因“限流失败已达换号上限”）不再重试（租约照常由 `recoverWorkerOnce` 释放），按 `PrimaryQuotaError` 结束，不回到等待重置的原账户。限流类单独计数、上限更宽，是因为分钟窗口按账号计（每账号每分钟 12 次，所有模型、两个通道共用），一个账号用满不代表其他账号也满。额度失败后的下一次选号带“不现场冷启动”标记（只设在这一次选号的 context 上，不影响之后的选号；短冷却的连带拦截不计数，但此前已有额度失败时同样保持这个标记）：`acquireLease` 不回收死槽位、不启动备用账号，也不排队等后台扩容；有空闲的热 Worker 照常取，都在忙时在排队预算内等待（截止后 `CapacityBusyError`），其中有每分钟请求已满的（`AccountCandidateGroups.WarmRateLimited`，最长一分钟才释放，不像并发忙碌很快空出）、又有没冷却的备用账号且所在分区能扩容时，另为其中一个发起一次后台扩容（每次选号最多一次，不等它；扩容的账号进入启动中后，复查仍按“账号全部忙碌”在排队预算内等待，不按“Worker 正在启动”），只是并发忙碌时仍不扩容；没有任何热候选、只有没冷却的备用账号时，为其中一个触发一次后台扩容（所在分区能扩容时，不等待；本次选号已发起、还没结束的扩容时不再触发，例如满窗扩容后热 Worker 被写入冷却，一次选号最多冷启动一个，不同请求仍各扩容一个）并立即返回“额度 429 后没有已启动的 Worker”；候选全部冷却同上。额度失败之后选号失败为没有已启动的 Worker、全部冷却或 `ErrNoEligibleAccount`（不含 `AccountsNotReadyError`）时同样按 `PrimaryQuotaError` 结束（原因分别为“额度 429 后没有已启动的 Worker”“其余候选全部冷却”“没有其他可换的账号”）；文件绑定且不复制文件的请求遇到 `ErrNoEligibleAccount` 或全部冷却时先走原来的复制文件到其他账号再试；不固定账号的请求在 Worker 重建后固定到原账号的重放选号失败时（例如原账号在重建期间被其他请求的额度 429 冷却）照原来的规则只排除这个账号、继续换号，不按额度不足结束，那次选号只看了这一个账号；`CapacityBusyError` 原样返回。`AccountsNotReadyError`（剩下能服务该模型的账号需要重新登录、已停用或 runtime 被其他进程占用）照原来的规则先回到没等到关闭的原账号；回不去而上一次失败是额度失败时按“没有其他可换的账号”结束，不以上一次不带 `Retry-After` 的上游额度 429 结束（`maxAttempts` 按可调度账号数计、不看模型，可能比真正能服务该模型的账号多）。同一账号另一通道可用时照旧再试一次（不算换号，进度写“同账号换通道重试”，见 `quotaSameAccount`；限流类照常计一次，每日类同一账号只计一次）；循环因 `maxAttempts` 用完而结束、最后是额度错误时也换成 `PrimaryQuotaError`（原因为“没有其他可换的账号”，账号少的 Ultra 号池常见）。第一次尝试照常可以现场冷启动一个备用账号（这时还不知道它有没有额度）。

`PrimaryQuotaError`（`internal/aistudio/accounts.go`，紧挨 `CapacityBusyError`）记模型、号池、原因（“额度失败已达换号上限”“限流失败已达换号上限”“其余候选全部冷却”“额度 429 后没有已启动的 Worker”“没有其他可换的账号”）、遇到每日类额度失败的账号数（`QuotaHits`）与限流类次数（`RateHits`，大于 0 时 `Error()` 在“额度失败 N 次”之后写“限流失败 N 次”）、总耗时、建议重试时间（`Until`）与最后一次上游额度错误（`Cause`，不实现 `Unwrap`，免得被当成上游 `RPCError`、输入超限等处理）；`HTTPStatus` 为 429，`ErrorCode` 为 `rate_limit_exceeded`，`RetryAt` 写进 `Retry-After`。建议重试时间：其余候选全部冷却时取全部冷却的最早恢复与本请求遇到的最早额度恢复中较早者，不设上限（每日限额就写真实恢复时间）；每日类达到换号上限或没有其他可换的账号时取 5 秒后（`primaryRetryFallback`）与最早额度恢复中较早者（号池里可能还有没冷却的账号，判断要在账户池锁内分类全部候选）；限流失败已达换号上限时取本请求遇到的限流冷却中最早的恢复（已启动的 Worker 多半刚用满这一分钟；每日类的长冷却不参与），不超过 `cooldownQueueLimit`（1 分钟），不知道时 5 秒后；没有已启动的 Worker 时为 5 秒后；都不早于 1 秒后。HTTP 边界写 `Retry-After` 时（`internal/api/official_errors.go` 的 `retryAfterSeconds`，只此一处）在建议间隔向上取整的秒数上随机往后加 0 到 min(建议间隔 ÷ 5 秒, 3) 秒（`retryAfterJitterStep`、`retryAfterJitterMax`，`math/rand/v2`；建议间隔不到 5 秒或已过时不加，只往后加，不早于真实恢复时间），覆盖 `PrimaryQuotaError`、`AllCoolingError`、`CapacityBusyError` 与 `AccountCoolingDownError`，免得冷却一到期所有客户端同时涌上来；内部的 `Until`、`RetryAt()` 与进度里的“建议重试=”不加抖动。对外映射与 `AllCoolingError`、`CapacityBusyError` 相同：固定文案 `Resource has been exhausted (e.g. check quota).`，不把 `Cause` 或上游原文放进对外消息（见 [协议规范](protocol.md)）；请求记录、用量账本等对这两种错误做类型判断的地方同样把它记为限流而不是上游失败。进度日志的文案见 [运行日志](logging.md)。普通号池的 `PRIMARY_MODELS` 与不在列表里的模型行为不变。

账户池按选择条件（模型、方法、能力、通道限定、号池、资格范围）维护候选索引（`internal/aistudio/scheduling_index.go`）：每个候选集记录各账户的成员状态与冷却期限，未冷却的候选按账户 ID 排成可用列表，冷却中的候选放在按恢复时间排列的最小堆里，到期时移回可用列表、堆顶即最早恢复时间。目录、权益、状态、启用与运行态变化时由 `accountChangedLocked` 增量更新候选集，租约、独占与认证刷新的忙闲由 `busyChangedLocked` 写进按账户下标的紧凑数组，`ClassifyCandidates` 与不限账户的 `AcquireFor` 只扫描可用列表与这些数组，不再在账户池锁内遍历全部账户；只带排除列表（`ExcludedAccountIDs`，供换号重试排除已尝试的账户）的选择同样走候选索引，只跳过这几个账户的下标，不建立整个号池的允许集合，排除列表也不进候选集与调度等待队列的键；排空标记（`SetDraining`，与独占、认证刷新分开记录）让不固定账户的选择把该账户当作忙碌（热账户归入 `WarmBusy`、备用账户归入 `StandbyBusy`）且不再给它新租约，进行中的请求照常结束，指定账户与文件绑定的请求照常使用，账户对外状态、`Activity` 与预热概况不变，账户移出号池时清除；状态摘要（`PoolStateCounts`）、换号上限与预热概况也按这些数组推导账户状态，其中冷却按与账户状态表相同的口径（全局 `*` 冷却，或某个模型在账户可用的每个启用通道上都冷却）记录为到期时刻（`coolUntil`，见 `accountCoolingUntilLocked`），Build 通道对其他账户目录的依赖在目录版本变化后按 1 秒节流重算；冷却轮换与淘汰统计可接替账户（`SpareAccounts`）时同样先按这些数组排除停用、未就绪与全局长时间冷却的账户，指定模型时只计入是这些模型生成请求的候选、且支持的通道中至少一个没有长时间冷却的账户（目录里没有该模型的账户换上去同样是死槽位）；账户增删、上游通道变化或 `ULTRA_INCLUDE_PRO` 切换时整个索引重建，候选集另每分钟全量重建一次兜底。模型存在性检查、别名解析、模型目录的可用通道按目录版本（`eligibilityVersion`，账户增删、目录或权益变化、启用状态、Build 目录、上游通道与号池划分规则变化时递增）缓存并按目录内容去重重建（`pool_catalog_cache.go`）；预热模型、可生成模型数与状态页的模型列表按账户目录缓存；账户超过 200 个时 `Status` 的结果缓存 1 秒。候选都没有空位且有账户冷却时从磁盘重读运行态的刷新只刷新冷却中的候选、最多 32 个并发，间隔按账户数放大（每 1000 个账户加 3 秒，最长 30 秒）。`internal/aistudio` 的 `TestSchedulingIndexMatchesBruteForce` 在随机变更（含排空）后用逐个账户判断的参考实现核对索引（选择含随机排除列表）与可接替账户统计，`TestExclusionLeaseMatchesAllowList` 核对排除列表的选号与等价的限定账户集合一致，`TestClassifyCandidatesScale` 在 15k 账户的合成号池上压测分类耗时与锁持有时间（`POOL_SCALE_SECONDS`、`POOL_SCALE_ASSERT`）。

Worker 容量由热池目标、活动上限和单账户并发共同约束。活动数低于 `MAX_ACTIVE_WORKERS` 时直接启动并发布新 Worker。容量已满且存在空闲旧实例时，先启动 pending Worker（正在启动、尚未发布的替代 Worker），再关闭旧实例并发布替代 Worker；对请求模型处于冷却（全局或该模型限额）且剩余不少于 2 分钟（`ShortCooldownMax`）的空闲实例优先，同类中最久未用者优先，其次为最久未用实例；剩余不到 2 分钟的冷却（分钟限额最长一分钟、没有额度信息的限流 30 秒，或长冷却的最后一段）很快自行恢复，`WarmWorkerStates` 不把它算作冷却中，这样的空闲实例与健康空闲实例同档按最久未用排序，关掉它换一个要冷启动 10–30 秒的账号得不偿失。多个冷却实例可并行替换，每次替换预留独立的旧实例，启动期间新旧进程会短暂共存。启动失败或取消时现有 Worker 继续服务。请求只在取得容量槽位时占用冷账户；没有可立即使用的槽位时释放该账户并重新分类，由任一空闲的热 Worker 或新释放的槽位接收。存在可调度账户但暂时没有空闲槽位的请求按相同选择条件先到先服务排队，在租约释放或 Worker 状态变化时唤醒，直到请求超时。生成请求的选号带排队进度（`leaseWaitProgress`，与排队预算一样经 context 交给 `acquireLease`，见 `queue_progress.go`）：一次尝试排队满 2 秒仍没有取得账号时在请求时间线写明等待原因（`排队等待`，之后每个请求最多每 15 秒一条、全进程限流，主用模型容量原因的等待不写，见 [运行日志](logging.md)），请求在排队中超时或取消时选号错误带上最后的等待原因与已等待时间（`%w` 包装，504/499 映射不变）。候选账户全部处于冷却时，最早恢复时间在 1 分钟内的请求排队等待恢复（冷却到期不发调度信号，排队请求按各自的计时器复查，最多每秒一次；最后一次不足 1 秒的等待随机延后 0 到 500ms，`coolingRecheckDelay`，免得同一时刻一起在账户池锁上分类候选），更晚的请求直接返回 429，`Retry-After` 给出距最早恢复时间的秒数；Ultra 主用模型不等待，立即返回 429。热 Worker 都在冷却或忙于其他模型、而还有没冷却的备用账户，只是所在分区没有空槽也没有可回收或替换的空闲 Worker 时，原因是容量而不是冷却：请求按“没有空闲的 Worker 槽位”排队（主用模型受排队上限限制，其余模型等到请求超时），不返回全部冷却的 429（原先的 `Retry-After` 取候选集里最早恢复的账户，可能长达几小时），也不按冷却等待绕过排队上限；热候选取租约时被其他请求抢先同样按“账号全部忙碌”等待；每分钟请求已满的候选同样按忙碌处理（见上文“每账号每分钟限速”一段）。降级判定的 Playground 优先选号除外：它全部冷却时调用方立即改为不限通道选号。超出 `WARM_WORKER_LIMIT` 的 Worker 空闲 5 分钟后关闭，热池保持目标数量。旧 Worker 与 pending 回收同时失败时，两份进程与租约均保留为 cleanup pending（仍待关闭）并占用容量槽，后续 Stop 会重试关闭。账户的 WAA runtime 租约由其他进程持有时，该账户退出预热与调度候选，首次 5 秒后重新探测，每次仍被占用时间隔翻倍、上限 1 分钟，每段占用只记录一条日志；指定该账户或只剩该类账户的请求返回账户正在使用的错误（对外按 503 服务暂时不可用）。

驻留 Worker 的账号进入长时间冷却（剩余不少于 15 分钟：每日限额冻结整个账号的全局 `*` 冷却，或请求模型的所有启用通道都冷却）后，这个 Worker 成为“死槽位”：占着常驻名额却服务不了请求（`internal/app/worker_retire.go`）。处理分三层，都与按需替换共用 `tryReserveVictim`，都经账号独占租约淘汰，正在服务请求的 Worker 永远不会被关闭，并且只在分区内有可接替的账户时淘汰（全部账号都冷却的小规模部署保留 Worker，冷却到期后立即可用）：`applyQuotaCooldown` 写入冷却后立即登记淘汰（冷却没有写入时，即模型目录已更新、同账号取得租约更晚的请求已写回结果，或该键已有仍生效且更晚的冷却，`TryExtendCooldownIfGeneration` 返回 `CooldownSkippedGeneration`、`CooldownSkippedStale` 或 `CooldownKeptLater`，只如实记一行 INFO `账号冷却未写入`，不登记淘汰、不做通道共用额度的学习，调用方照常按已冷却换号），Worker 空闲就关闭、忙碌则等它的请求释放租约后关闭，冷却被清除或缩短时放弃，淘汰成功后立即补齐预热；`keepWarm` 每个周期清扫全局长时间冷却的空闲 Worker，不受按模型轮换的 5% / 最少 3 个阈值限制，每轮两个分区合计最多关闭 `WARM_STARTUP_CONCURRENCY` 个并并发关闭，接替的 Worker 由预热经冷启动名额补齐；请求选号时备用账户所在的分区已满而分区里有对该模型是死槽位的空闲 Worker，先现场回收一个（每轮一个，只在该分区内）再启动备用账户，不等轮换也不先冷启动替代 Worker，回收写入请求时间线。按模型的冷却轮换（每 30 秒、阈值与一轮替换数不变）继续处理只在个别模型上冷却的账号。淘汰选号（`idleWarmVictimFor`）改为账户池一次加锁取得全部候选的活动与冷却状态，死槽位优先于冷却剩余不少于 2 分钟（`ShortCooldownMax`）的账号，再优先于最久未用的账号；剩余不到 2 分钟的冷却（分钟限额、限流第 1 档）在 `WarmWorkerStates` 里不算冷却中：`Cooling` 为全局冷却剩余不少于 2 分钟，或该模型生成请求所有支持的启用通道都在冷却（`accountChannelCooldownLocked`，Build 键 `build:<模型>` 与 Playground 键分别取全局与该键中较晚的一个，全局短、模型长时仍算）且最早恢复的通道剩余不少于 2 分钟；只有一个通道冷却、另一个支持的通道还能服务该模型时不算，账户对该模型没有任何支持的通道时只看全局冷却，但账户目录里有该模型、只是它不支持 `generateContent`（视频、Live 等按模型取 Worker 的请求）时退回按 Playground 模型键的 `accountCooldown` 判断（原先用 `accountCooldown` 只看 Playground 键，Playground 冷却而 Build 可用的 Worker 也被当作冷却中优先替换，Build 冷却时反而不算）；`Dead` 口径不变（`longCoolingLocked`：全局冷却或所有启用通道都冷却，不看通道是否支持该模型，与淘汰、补登记、轮换的死槽位一致）。这样的短冷却与健康空闲同档按最久未用排序，`victimCooling` 不选它，分区已满时 `expandable` 也不为扩容替换只有这种短冷却的空闲 Worker。状态接口的 `workers.dead`、`ultra_workers.dead` 给出各分区的死槽位数（全局长时间冷却，或在热门冷却模型、所在分区的主用模型上长时间冷却：普通分区按 `PRIMARY_MODELS`，Ultra 分区按 `ULTRA_PRIMARY_MODELS`），`draining` 与 `retiring` 给出其中排空中的数量与待淘汰登记数（`drainCounts`、`retirementCounts`，各一次加锁）。

饱和时其他模型的请求一直在用死槽位，它永远等不到空闲，于是先排空再淘汰（`internal/app/worker_drain.go`）。淘汰循环遇到忙碌的登记（全局冷却，或在主用、热门模型上冷却；只在其他模型上冷却的 Worker 还能服务主用模型，忙碌时不排空，空闲后照常淘汰）时，在分区占名额（未超过 `drainSlotTimeout`）的排空数低于 `drainLimit`（`max(WARM_STARTUP_CONCURRENCY, 1) × 2`，且不超过分区峰值的 1/4、至少 1；峰值为 0 的分区不排空）、排空总数低于 2 × `drainLimit`（`drainTotalLimit`）、分区内又有按登记模型统计的可接替账户时，把账号标记排空（`AccountPool.SetDraining`，最老的登记先排空；`startDrain` 在 `drainMu` 内核对账号仍有驻留 Worker、没有被其他路径选中淘汰）：不固定账户的请求不再分给它，进行中的请求照常结束，最后一个租约释放后由原有的唤醒路径关闭；指定账户与文件绑定的请求照常使用。排空开始时立即在后台启动替补（先启后停；让出额度已满、没有空槽时替补不启动，旧 Worker 关闭后补上）：`ensureWorker`、请求调度的分区容量（`standbyCapacity`，与 `ensureWorker` 一样把启动中的 Worker 计入占用）与状态接口的 `occupied` 都把排空中的 Worker 按已让出槽位计，最多 `drainLimit` 个，分区浏览器总数最多为峰值加 `drainLimit`。排空在 Worker 关闭（任何关闭路径，含 `ResetAll` 与 `Close`）、登记被放弃（冷却被清除或缩短）、淘汰出错时清除；超过排空超时（`drainTimeout`：总时长上限 + 30 秒）仍有请求时清除标记、1 分钟后再试；排空超过 `REQUEST_TIMEOUT` + 30 秒（`drainSlotTimeout`）后不再计入 `drainLimit` 的名额（标记保留），长流占着旧 Worker 时同分区其他死槽位在总数上限内排空（见下文 `drainTotalLimit`）。替补（`startReplacement`）在排空开始、空闲淘汰、冷却清扫与冷却轮换之后启动（现场回收不启动替补：请求随后自己启动的备用账号补上空出的槽位）：在同一分区按调度索引挑选能服务登记模型（全局冷却的登记按主用模型，没有主用模型时按预热候选）、对热门与主用模型都没有长时间冷却的空闲备用账号（每批 64 个、最多核对 1024 个），经后台冷启动名额启动，只用空槽（含排空让出的），不替换其他账号的空闲 Worker；分区低于预热目标时由预热补齐（排空开始时除外），`WARM_WORKER_LIMIT` 低于驻留数时驻留数靠替补保持；排空开始时的替补启动成功或仍在启动时，旧 Worker 关闭后不再多补。淘汰统计可接替账户时按登记的模型统计（`SpareAccounts`），唯一可接替的账号对同一模型也已冷却时不淘汰、不排空。`keepWarm` 每个周期把驻留 Worker 中的死槽位补登记（`registerDeadWorkers`：一次 `LongCoolingSet` 找出死槽位，只对它们按全局与各模型分别核对范围），重启后、其他进程写入的冷却、冷却时还没驻留的账号同样会被排空淘汰，登记与状态接口的 `dead` 口径一致。放置与挑选也按模型：模型不在主用与热门模型里的不固定账户请求先在对这些模型是死槽位的热 Worker 里取租约（多一次 `LongCoolingSet` 与一次限定账户的 `TryAcquireFor`），取不到再用其余热 Worker，把还能服务主用模型的租约位留给主用模型；请求路径为后台扩容与现场启动挑备用账号时（只核对前 64 个候选）、预热排序时优先没有在热门或主用模型上长时间冷却的账号；超出常驻数的空闲回收只在空闲超时的 Worker 中挑选，其中先关闭这类死槽位（刚被其他模型的请求用过的死槽位不会让回收整轮放弃）。

额度 429 的识别（`internal/aistudio/client.go` 的 `DecodeRPCError`、`internal/aistudio/quota.go` 的 `QuotaCooldownForError`）：Playground、Build（非 200 与流尾）、Live 与 Interactions 的上游错误都经 `DecodeRPCError` 解析，修一处所有通道受益。`decodeRPCErrorMetadata` 读 `google.rpc.RetryInfo`、`google.rpc.QuotaFailure`、`google.rpc.ErrorInfo`，以及类型 URL 以 `.GeminiApiErrorDetails` 结尾的私有详情（按后缀匹配，包名变化时仍能识别；AI Studio 网页后端的每日限额只在这里给出额度信息）：它的值是数组，先试下标 28，不是 QuotaFailure 形状 `[[违规项...]]` 时扫描第一层其余元素，要求某个违规项的下标 3 是带 `/` 的指标名、或下标 4 是带 `Per` 的限额名（`quotaFailureShape`），免得把其他嵌套数组误认成违规项。违规项按 proto 字段号减一读取（`quotaViolationFields`，`google.rpc.QuotaFailure` 共用）：3=`quota_metric`、4=`quota_id`、5=维度 `[[键, 值]...]`（键的顺序不固定，写成 `Metadata["quota_dimension_<键>"]`，如 `quota_dimension_model`、`quota_dimension_location`）、6=`quota_value`（字符串或数字都接受，写成与 ErrorInfo 同名的 `quota_limit_value`）；有每日违规项时取它，否则取第一个，ErrorInfo 已给出的字段不覆盖，详情项、违规项或字段解析失败只跳过它。全部详情的类型（去掉 `type.googleapis.com/` 前缀、按出现顺序去重，含没有解析的类型）记进 `RPCError.DetailTypes`。没有 RetryInfo 时从文案解析 `retry in <Go 时长>`（`retryDelayFromMessage`：正则 `(?i)\bretry in ((?:\d+(?:\.\d+)?(?:h|ms|us|µs|ns|m|s))+)`，`time.ParseDuration`，超过 `maxRetryDelay`（24 小时）时截到 24 小时而不是丢弃），`validateRPCResponse` 只在两者都没有时读 `Retry-After` 头，所以优先级为 RetryInfo > 文案 > `Retry-After`；“Retry with an alternative Gemini model” 不匹配。`dailyQuotaEvidence` 另认 “reached your usage limit”（不收更宽的 “usage limit”，免得误伤）。三种现场 429 的结果（`internal/aistudio/quota_recognition_test.go` 用原文测独立状态、`[null,[...]]`、稀疏 `[,[...]]` 与 Build 流尾）：free 每日（`GenerateRequestsPerProjectPerUserPerModelPerDay`，上限 10，文案带 `Please retry in 16h…`）为按模型的每日限额（含 `per_model`，非全局），`Until` 为太平洋时间次日零点与 now+retry in 中较晚的一个，`Escalate=false`；pro/ultra 每日（文案 “You've reached your usage limit”，上限 50，没有恢复时间）同样为按模型的每日限额，冷却到次日零点，原先落到 `RateLimitKind` 30 秒并逐级冷却，同一账号要白试几次才成为死槽位；pro/ultra 每分钟 12 次（ErrorInfo `GeneratePerMinutePerProjectPerUser`）为全局分钟限额，不变。`QuotaCooldown` 带 `LimitValue`、`Metric`、`Model`（`quota_dimension_model`：上游计额度用的模型，如 `gemini-3.1-pro`，可能与请求的 `gemini-3.1-pro-preview` 不同；冷却仍按请求模型）与 `DetailTypes`（逗号分隔），只写进冷却日志（`quotaCooldownFields`：`上限`、`指标`、`模型`，没有指标时另写 `详情类型`，用来核实裸 429 是否带了没有解析的详情）。每日限额照常走 `quotaSharing.dailyLimitHit` 的通道共用学习（本批不按指标直接判共用）：已判定共用时同步冷却另一通道，`applyQuotaCooldown` 延后到函数结束的 `retireCooledWorker` 看到两个通道都冷却后立即登记淘汰，`generateWithRetry` 不计入连续失败、不退避地换号；观察中时同账号另一通道先再试一次。`RPCError` 不实现 `RetryAt`：单个账号的 `retry in 16h` 透成对外的 `Retry-After` 会让客户端或中转停用渠道 16 小时，号池级的 `Retry-After` 仍由 `AllCoolingError`、`CapacityBusyError` 等给出。

连续裸限流逐级冷却（`internal/app/rate_limit_guard.go`、`internal/aistudio/rate_strike.go`）：上游对额度已用完的账号常只返回 “Quota exceeded”，没有 ErrorInfo，也没有 RetryInfo、文案里的 `retry in` 与 `Retry-After`；`QuotaCooldownForError` 按限流冷却 30 秒（`unknownRateLimitCooldown`），这样的账号每 30 秒恢复、被选中、再 429，它的驻留 Worker 因为剩余不到 `ShortCooldownMax` 不算冷却中、剩余不到 `rotationMinRemaining` 成不了死槽位，分区满时换不上新号。`QuotaCooldown.Escalate`（`Kind == RateLimitKind && !Global && RetryDelay <= 0`）为真的 429 由 `applyQuotaCooldown` 转给 `applyRateLimitCooldown`，分钟限额、每日限额、全局冷却与带恢复时间的 429 照原路径冷却。只转错误来自生成本身的（参数 `fromGeneration`：`generateWithRetry` 传本次尝试的 `generated`，`forwardEvents` 传 true）：文件复制、附件上传（Drive）、取访问令牌返回的 429 与模型额度无关，同单模型 403 的 `generated` 守卫一样照原路径只冷却本通道 30 秒，不建档位、不同步另一通道、不计入号池级限流，之后照常在同账号另一通道再试（Build 通道直接携带 inlineData，不经 Drive）。判断用的是本次尝试本身的错误（`generateWithRetry` 的 `attemptErr`）：生成失败后删除临时 Drive 副本（`TemporaryFileCopies.Cleanup`）返回的错误照旧并入请求的错误写日志与返回，但不参与额度冷却与单模型 403 的判断，否则一次连接重置、首事件超时之类的非额度失败会因为清理时的 `DriveDelete` 429 沿 `errors.Join` 被当成生成本身的裸限流逐级冷却、同步冷却 Build，`DriveDelete` 403 也会被当成单模型 403。这条单键路径（以及分钟限额、每日限额、全局与带恢复时间的 429）用 `TryExtendCooldownIfGeneration` 写入：该键已有仍生效且更晚的冷却时保留它（原因也保留），只把 `CheckedAt` 推进到本次租约并返回 `CooldownKeptLater`，`applyQuotaCooldown` 照未写入记一行 INFO `账号冷却未写入`（`恢复` 为保留的冷却的恢复时间），不登记淘汰、不做通道共用额度的学习；原先用 `TryMarkCooldownIfGeneration` 无条件覆盖，同账号并发、租约稍晚的附件上传 429 会把刚写入的第 3 档缩短成 30 秒，驻留 Worker 的淘汰登记随之失去理由被丢掉。其他调用方（单模型 403、自动暂停等）仍用 `TryMarkCooldownIfGeneration`，语义不变。档位时长由 app 层给出（`rateLimitSteps`：30 秒、5 分钟、30 分钟、2 小时，超出各档后到 `NextQuotaDay`，每档都不超过太平洋时间次日零点），取值对应调度的阈值：第 2 档大于 `ShortCooldownMax`（2 分钟）加 1 分钟，剩余 5→2 分钟这段算冷却中，分区满时可被 `coolingVictim`、`victimCooling` 替换；第 3 档不小于 `rotationMinRemaining`（15 分钟）加 10 分钟，剩余 30→15 分钟这段是死槽位（登记淘汰、补登记、轮换），对不超过 `REQUEST_TIMEOUT` 的请求足够排空；更长的流由冷却登记在 15 分钟窗口结束时撤销、随之清除排空（`worker_retire.go`）；太平洋时间 23:30 之后各档被截短，可能不足 15 分钟，只算冷却中，零点后额度恢复。包内测试经 `trackedService.rateLimitLadder` 把前两档改为 150ms、300ms（实例字段，并行测试互不影响）：冷却写入后请求还要判断同一账号能否换通道，档位短到 20ms 时 -race 下的一次停顿就让冷却在判断前到期，同一请求换通道再试并升档，档位与调用次数都对不上。

档位记在 `Account.rateStrikes`（`map[rateStrikeKey]rateStrike`，受 `p.mu` 保护，只在内存中，重启后从第 1 档起，冷却本身照常落盘）：键为按账户目录归一的裸模型与通道（两个通道共用时通道为空），值为档位、本档冷却的实际到期时间、最近一次写入档位的失败租约的 `CheckedAt`（`failedAt`：取得租约晚于它的成功才清零）与写入时的 `NextQuotaDay`（不早于它时档位作废，跨额度日从第 1 档起）。`AccountPool.MarkRateLimitCooldown(RateLimitMark)` 在一次 `updateRuntime` 内完成，同账号的成功清零与其他冷却写入因此串行：mutate 里先像 `TryMarkCooldownIfGeneration` 一样核对目录代际（不符为 `CooldownSkippedGeneration`）与本通道冷却键的 `CheckedAt`（不早于本次租约为 `CooldownSkippedStale`；两种都由调用方记 `账号冷却未写入`，不登记淘汰），再算档位：没有记录或已跨额度日为第 1 档；租约的 `CheckedAt` 早于本档到期时间（冷却写入前已经发出的并发请求，或同一请求换通道再试）或不允许升档时档位不变，冷却续到本档到期与第 1 档时长中较晚的一个；否则升一档，最多到“次日零点”一档。随后写本通道键，`Shared` 且另一通道启用时在同一次更新里写另一通道键（该键的 `CheckedAt` 不早于本次租约时不写，`Synced` 为假）。每个键已有仍生效且更晚的冷却时保留它（原因也保留），只把 `ModelAccess` 的 `CheckedAt` 推进到本次租约；另一通道键保留了更晚的冷却时 `Synced` 仍为真（该通道确实在冷却）。原因写成 `限流: 连续限流第 N 档，<上游原文>`，`rateCooldownReason` 照样按限流识别。档位在 apply 里提交（运行态写盘成功后，仍在 `p.mu` 内），写盘失败时不变，提交时顺带删除这个账户已跨额度日的记录。参数：`CheckedAt` 非零（同 `TryMarkCooldownIfGeneration`），`ModelID` 去掉 `models/` 后非空，`Ladder` 非空且每档为正；不要求“期限在未来”，期限由函数自己算。返回的 `RateLimitMarkResult` 带本通道实际生效的 `Until`、写入后的 `Level` 与 `Synced`；`applyRateLimitCooldown` 把 `cooldown.Until` 换成实际写入的恢复时间再交给 `retireCooledWorker`（第 3 档起、所有启用通道都已冷却或只启用一个通道时即为死槽位）与 Ultra 主用模型的额度失败计数（`primaryQuotaClassOf`，剩余不少于 15 分钟归入每日类）。

成功清零：`markModelAccessVerified` 抽出 `markModelAccessVerifiedClearing`。成功记录的作用域以 `build:` 开头为 Build 通道，否则为 Playground（`verifiedStrikeKeys`），清两个通道共用的一份与该通道自己的一份（不共用时另一通道的档位与这次成功无关），条件是这次成功的租约 `CheckedAt` 晚于档位的 `failedAt`；删除放在 apply 里，目录代际不符时不清，已跨额度日的记录直接删除、不计入返回的最高档，也不让快路径判断失效。原先的快路径（资格已是 verified、没有冷却、`CheckedAt` 不早于本次时不写运行态）另要求没有可清的档位（`clearableRateStrikeLocked`）：另一通道成功、该键没有冷却且 `CheckedAt` 不早于本次时快路径会漏清。`MarkModelAccessVerifiedAsync` 签名不变，内部改调清零版本；`MarkGenerationVerifiedAsync` 的回调另带被清档位中的最高档 `clearedLevel`，app 层的 `markModelAccessVerifiedAsync` 改用它，不小于 2 时写 INFO `限流档位清零`。

共用判定与号池级保护：`quotaSharing.sharedFor(modelID, TierPoolOf(accountID))` 决定两个通道是否共用一份档位，只读、不改等待验证的观察与复核计数，只有已判定为独立（`Independent > 0` 且 `Independent × 20 > Shared`，与 `verdictLocked` 的“独立”一致）时返回 false；共用、观察中、没有记录与 nil 都按共用，免得同一账号在两个通道上轮流白试。`rateLimitGuard`（结构同号池级 403 的 `forbiddenTracker`，方法 nil 安全）按 `rateLimitScopeKey{号池, 模型}`（形状与 `forbiddenScopeKey` 相同，另行定义）记最近裸限流的不同账号（每个账号只留最近一次，最多 5 个）与最近一次成功：10 分钟内达到 5 个账号、这段时间又没有成功（启动以来没有成功过也算）时判定为号池级限流，这次不允许升档，冷却日志带 `号池级限流不升档`；判定成立、距上一次告警超过 10 分钟时另写一条 WARN `号池级限流`（`applyQuotaCooldown` 拿不到请求 ID，只写服务日志）。`generateWithRetry` 收到首个事件、尝试成功后调用 `rateLimits.succeeded` 清空该号池该模型的记录。`applyQuotaCooldown` 另有两个参数 `pool`、`escalate`：`generateWithRetry` 传请求的号池与 true；`forwardEvents`（已向客户端输出后才出现的 429，无法换号）传 `PoolScopeFromContext(requestCtx)` 与 false，照常冷却，但这次尝试已经拿到上游输出，不作为“冷却到期后仍失败”的证据。代价是常驻 Worker 全部用完额度、10 分钟内又没有任何成功的极端情况下仍停在第 1 档。`internal/aistudio/rate_strike_test.go`（档位、截短、跨额度日、并发、不升档、保留更晚的冷却、单键写入不缩短、独立通道、单通道、未写入时档位不变、成功清零）与 `internal/app/rate_limit_guard_test.go`（档位阈值、号池级判定、共用判定、升到淘汰、成功清零、号池级与已输出内容后不升档、附件上传的 429 不逐级、临时副本清理的 429/403 不参与判断、并发的短冷却不缩短逐级冷却、第 2 档可被替换）覆盖这些规则。

主用模型供给（`internal/app/primary_supply.go`）：`keepWarm` 的每个周期（`keepWarmPass`）调用 `refreshPrimarySupply`，缓存超过 30 秒、两份主用模型列表任一变化或号池规则版本（`AccountPool.PoolRuleVersion`，见下文“号池成员规则”一段）变化时重新统计，两份都为空时清空，`ResetAll` 时也清空；状态接口的 `primary_supply` 只读这份缓存（服务未运行时不返回），不在状态请求里对上万账户分类。统计时普通列表的每个模型在普通分区、Ultra 列表的每个模型在 Ultra 分区（`PoolScopeUltra`，只算 Ultra 分区的驻留 Worker，条目 `pool` 为 `ultra`，普通条目 `pool` 为空）各按调度索引分类一次（`ClassifyCandidatesCached`，只读内存状态）：排空中的驻留 Worker 不作为热账户传入，随排空标记落入忙碌分组，不计入 `warm`；`warm` 为热账户的三个分组之和，`standby` 为 `StandbyReady` 中不在启动中的账号，最多数到 1000 个（`standby_capped`）。冷却中的候选不进任何分组：`cooling` 用同一个选择条件与同一份热账户（不含排空中的）另数一次（`AccountPool.CoolingCandidateCount`：一次加锁，取候选集并 `expireLocked` 后，数候选集里冷却到期时刻晚于现在的账户，即是该模型的候选、但所有支持的启用通道都在冷却，含剩余不到 2 分钟的短冷却，口径与调度分类一致；不是候选的账户不计），这些驻留 Worker 既不计入 `warm` 也不计入 `standby`，`warm` 很少而 `cooling` 很多时主用模型的请求只能等冷却到期或换上新号。排队超限的进度里的 `驻留冷却=N` 由 `supplyCoolingField` 读这份缓存（Ultra 号池的请求找 Ultra 列表的条目，其余找普通列表的条目，没有条目时不写），不在请求路径上扫描账户。侧栏据此为每个主用模型显示一行“主用 <模型>：热 N · 冷却 C · 备用 M”，Ultra 条目在 Ultra Worker 一行下面显示为“Ultra 主用 <模型>：热 N · 冷却 C · 备用 M”，备用为 0 时标黄；死槽位一行在有排空时追加“排空中 N”。`TestPrimarySupplyCounts`、`TestPrimarySupplyCachedForStatus`、`TestKeepWarmRefreshesPrimarySupply` 与 `TestStatusShowsDrainingAndRetiring` 覆盖供给口径（含 `cooling`）、缓存与状态字段，`TestCoolingCandidateCount`、`TestWarmWorkerStatesChannelCooling`（`internal/aistudio/warm_cooling_test.go`）覆盖冷却计数与 `Cooling` 的按通道口径，`TestCapacityBusyDetailSnapshot`、`TestSupplyCoolingField` 覆盖排队超限的快照。

账户按当前权益与 `ULTRA_INCLUDE_PRO` 分为两个号池：权益为 Ultra 的账户，以及 `ULTRA_INCLUDE_PRO=true` 时权益为 Pro 的账户，组成 Ultra 号池（付费号池），其余账户（含权益尚未读取的账户）组成普通号池。请求的号池写在 context 中（`aistudio.ContextWithPoolScope`），`AccountSelection.Pool` 为空时 `AcquireFor`、`TryAcquireFor`、`ClassifyCandidates` 取 context 中的号池；`/ultra` 请求只用 Ultra 号池，`ULTRA_EXCLUSIVE=true` 时普通路径只用普通号池，为 `false` 时普通路径不限号池，预热、轮换与登录检查等后台任务不限号池。选号、模型存在性校验、模型目录与别名解析、换号、跨账户文件复制、上传候选与没有账户时的错误都只看号池内的账户；Ultra 号池为空或账户都不可调度时返回写明 Ultra 号池的 `AccountsNotReadyError`（503），全部冷却仍按 429。文件或视频绑定在号池之外的账户上时返回 `ResourcePoolMismatchError`（400，英文说明资源属于哪个号池），不会改用池外账户。账户权益变化后立即按新权益归入号池；`ULTRA_INCLUDE_PRO` 开着时 Pro 降为 Free 回到普通号池、升为 Ultra 留在 Ultra 号池，都经 `accountChangedLocked` 的增量路径完成。

号池成员规则（`internal/aistudio/pool_scope.go`）：成员只由 `ultraPoolTier(tier, includePro)` 决定（Ultra 总是属于 Ultra 号池，`includePro` 为真时 Pro 也属于；不叫 paid，`Model.Paid` 是模型属性），账户池内的号池判断都经 `inPoolLocked`、`tierInPoolLocked` 与 `poolOfTierLocked`，读取受 `p.mu` 保护的 `ultraIncludePro`，开关与它派生的成员集合、候选集在同一把锁下变化。候选索引按位置只存真实权益（`index.tier[]`），号池在读取时由权益加开关推导，`verifySchedulingIndexLocked` 对权益的核对因此不变。号池划分不改变权益：权益请求头（`HeaderValue`、`applyBenefitTier`，Pro 仍发 `TIER1`）、AccessModes 资格（`modelAllowedByTier`）、`SupportsModel`/`SupportsMethod`/`accountSupportsSelection`、冷却期限、`PreferWarmPool` 的分层（`benefitTierPriority`）、`buildSupportsModelLocked`、`buildUsesUnary`、输入上限与更高权益重试的等级比较（`benefitTierRank`）、预热模型与覆盖率缓存、`SetCatalog` 与 `syncAccountRuntimeLocked` 的权益比较都不读开关；`EnabledAccountsAboveTierOf` 只把号池过滤换成成员判断，权益高低仍按真实权益比较。`PoolOf` 返回成员关系（Worker 分区），`TierPoolOf` 返回按真实权益的号池（Ultra 权益为 `PoolScopeUltra`，其余为 `PoolScopeNormal`），只供每日额度通道判定分组：Pro 的额度与 Ultra 不同，证据记进 `模型|ultra` 后关掉开关也不会消失，`.quota-sharing.json` 因此不需要迁移。`Status` 的 `Pool` 为成员关系，`BenefitTier` 仍是真实权益。号池为空时的原因在开关关闭时保持原文，开启时 Ultra 号池为空写“没有权益为 Ultra 或 Pro 的账户”，独占模式下普通号池为空写明 Pro 也属于 Ultra 号池。

`SetUltraIncludePro` 的做法与 `Remove`、`SetUpstreamChannels` 相同，把号池划分规则变化当作与权益变化同类的事件：在 `p.mu` 内按新规则一次扫描重算 `ultraAccounts`，作废 `UltraAccountIDs` 的快照，`eligibilityVersion++` 让按版本缓存的目录汇总、别名解析与模型通道重建，整个候选索引作废（`invalidateSchedulingLocked`，下次调度时重建；候选集指针都在账户池锁内用完，没有留到锁外），作废状态缓存，`poolRuleVersion` 加一，最后关闭 changed 唤醒等待账户的请求与后台循环。不在这里手工清除各个按号池的缓存，以后新增按 `eligibilityVersion` 缓存的结构自动随之作废。开关值没有变化时什么都不做、返回 `false`。进行中的租约不受影响；按旧成员选中、尚未确认的租约由 `refreshAndValidateLease` 放弃并重新选号；固定账户的请求在账户已离开请求的号池时返回 `ErrNoEligibleAccount`。切换本身在 3.1 万账户上持锁 2.4～6.7 毫秒，之后首次使用多出约 0.3 秒的重建，分摊到随后的多次加锁里。

`UltraAccountIDs` 返回写时复制的共享快照，调用方只能读、不能修改：快照为 nil 时在 `p.mu` 内克隆一次 `ultraAccounts` 并保存，之后的调用直接返回同一份；`accountChangedLocked` 只在成员实际变化时修改集合并作废快照（它每秒被冷却、状态与运行态变化调用很多次，成员不变时作废等于没做写时复制），`Remove` 与 `SetUltraIncludePro` 也作废快照。Pro 并入后成员可达数千，每次调用都克隆会在账户池锁内的热路径上花去近 1 毫秒（4,077 项实测 0.84 毫秒）。

`PoolRuleVersion()` 无锁返回号池规则版本，开关每次实际切换时加一。app 层在锁外缓存、又按号池成员计算的派生结果用它作废：预热概况（`bootstrapSummary`，可预热账户超过 200 个时缓存 60 秒，`UltraAvailable` 按成员计数，沿用旧值会让 Ultra 分区的预热目标在缓存期内仍按旧成员计算）与主用模型供给（`primarySupplySnapshot.ruleVersion`）都在计算前先读版本、与结果一同保存，命中条件加上版本相等。先读版本再计算，切换前开始、切换后才写回的旧结果带着旧版本，下一次调用照样重算，没有竞态；主用模型供给因此在切换后的下一个 `keepWarm` 周期（5 秒内）重算，不等满 30 秒。

Worker 同样按账户当前所在的号池分为普通分区与 Ultra 分区（`internal/app/worker_partition.go`，成员每次调用都从 `UltraAccountIDs` 重新读取）。`WARM_WORKER_LIMIT`、`MAX_ACTIVE_WORKERS` 只约束普通分区，`ULTRA_WARM_WORKER_LIMIT`、`ULTRA_MAX_ACTIVE_WORKERS` 约束 Ultra 分区，浏览器总数最多为两个分区上限之和。预热把每个分区从自己的账户补齐到自己的常驻数（没有可预热的 Ultra 账户时不为 Ultra 分区分类候选；可预热的账户都是 Ultra 时同样不为普通分区分类候选，只有 Ultra 账户的部署按 Ultra 分区完成首轮预热，`ULTRA_WARM_WORKER_LIMIT=0` 时直接完成、Ultra Worker 按需启动），预热不足按分区判断并在日志中写明分区，超出分区常驻数的空闲 Worker 按分区回收；按需启动的容量判断、`idleWarmVictimFor` 的淘汰选号与冷却轮换（`tryReserveVictim`）都只在账户所在分区内进行，只有同分区正在启动的 Worker 才会让淘汰只选冷却中（剩余不少于 2 分钟）的 Worker、暂停空闲回收，Ultra 请求不会淘汰普通 Worker，反之亦然；请求调度按备用账户各自的分区判断能否扩容，等待队列按含号池的选择条件分组，同分区的 Worker 空闲或容量释放后唤醒排队请求。冷启动名额（`WARM_STARTUP_CONCURRENCY`）、冷却轮换一轮的替换数与单账户并发两个分区共用。账户权益在持有 Worker 期间变化、或 `ULTRA_INCLUDE_PRO` 切换时，该 Worker 从此按新分区计数，不强制重启。四个 Ultra 设置与普通分区设置一样随服务配置保存立即热更新。

`ULTRA_INCLUDE_PRO` 的热切换（`internal/app/hot_config.go` 的 `applyLiveConfig`）：`liveConfigMu` 让配置热更新逐次进行，最后一次应用读到的一定是最后写入的 `.env`，两次并发保存（一开一关）后账户池的开关、`current.config` 与 `.env` 一致；在 `manager.mu` 内先 `applyLiveSettings` 更新两个分区的容量，再 `SetUltraIncludePro` 改成员（锁顺序 `manager.mu` 再到账户池锁，与 `SetPerAccountConcurrency` 相同），同一次保存调大 `ULTRA_*` 峰值时并入的 Pro Worker 按新上限计数；解锁后开关实际变化时调用 `poolRuleChanged`，随后照常 `prewarmIfRunning` 补齐失去或得到 Pro Worker 的分区，`syncModelCache` 推送状态、模型与账户快照。解锁之后的步骤都从当前状态重新推导（成员、开关与生效的 `ULTRA_EXCLUSIVE`），不依赖解锁前捕获的配置。`poolRuleChanged`（`worker_partition.go`）可以重复执行：在 `drainMu` 内取成员快照，把排空中的每个 Worker 按当前成员改标分区（否则排空中的 Pro Worker 会在最长排空超时内继续给旧分区记让出额度），唤醒等待容量的请求，再记录生效日志，分区超过峰值或独占模式下开启时另记 WARN（文案见 [运行日志](logging.md)）。`retirePass` 一轮开始时按成员快照算出分区，并按它核对能否排空、统计可接替账户、启动替补；`startDrain` 在 `drainMu` 内按当前成员重算，切换恰好落在取快照与写入之间、账号已换了分区时不写入排空（这些判断都是对旧分区做的），登记不推迟，下一轮按新的成员快照重新判断。排空开始之后才切换的，排空开始时的替补在挑号前按排空当前记在的分区（`drainPartition`，`poolRuleChanged` 已改标）挑选；冷却淘汰关闭 Worker 后（`finishRetirement`）按当前成员重算分区再决定替补，替补总补在空出槽位、记着让出额度的那个分区。分区计数、预热目标、空闲回收与容量判断不需要迁移；超出峰值的空闲 Worker 由空闲回收收敛（空闲 5 分钟，每 30 秒检查一次），失去 Pro Worker 的分区由预热补齐，期间浏览器总数可能短暂超过两个分区峰值之和，多出的数量等于换分区的 Pro Worker 数；切换瞬间正在启动的那个 Worker 与 `fillWarm` 的一轮分区快照可能按旧计数放行。等待队列（`dispatchKey`）与主用模型规则（`primaryPolicyFor`）都按请求入口的号池，切换后队列身份不变、只需唤醒；降级记忆、号池级 403 跟踪、账本与访问日志的 `pool` 列同样按请求入口记录，不迁移，号池级 403 的旧记录在 `forbiddenWindow` 内自愈。资源不迁移：`resourcePoolErrorLocked` 按资源所属账户当前的号池判断，绑定在 Pro 账户上的文件、视频与 Veo 操作切换后可能要换入口引用。开启后，独占模式下普通路径（`PoolScopeNormal`）引用返回 `ResourcePoolMismatchError`（400，`Owner` 为 Ultra），要改用 `/ultra`；`ULTRA_EXCLUSIVE=false` 时普通路径为 `PoolScopeAll`、不检查号池，开启不会产生 400。关闭后 `/ultra` 引用返回 400（`Owner` 为普通号池），要改用普通路径。`PreferWarmPool` 仍按真实权益分层轮选、Pro 排在 Ultra 之前，`ULTRA_WARM_WORKER_LIMIT=1` 时唯一常驻的 Ultra 分区 Worker 可能是 Pro，只有 Ultra 订阅可用的模型（access mode 4）每次都要冷启动。同一次保存里还有需要重建生成服务的字段（`requiresRebuild`，如 `AISTUDIO_AUTH_STATES`）时 `applyLiveConfig` 照旧整体提前返回，开关在重建后由 `newRuntime` 生效：`configurePool` 在 `NewAccountPool` 之后、创建 Worker 管理器之前依次设置路由策略、上游通道与 `ULTRA_INCLUDE_PRO`，第一次 Worker 分区与预热目标就按正确的成员计算（`NewAccountPool` 构造时按开关关闭计算成员）。生成服务正在启动时保存，与其他热字段一样由新生成的服务使用启动时读到的配置。

故障重置先等待同账户的活动请求释放租约；等待期间该账户暂停接收新请求。客户端取消只结束自身请求。启动预热在官网 Run 按钮启用后提交，请求在发送前再次检查账户冷却状态。

账户状态：

| 状态 | 含义 |
| --- | --- |
| `ready` | 认证有效且存在可调度容量 |
| `busy` | 就绪账户存在独占操作、认证刷新或活动请求；调度仍按 `PER_ACCOUNT_CONCURRENCY` 判断剩余槽位。需要登录或不可用的账户有活动请求时仍显示原状态。每分钟请求已满（`ACCOUNT_RPM_LIMIT`）只影响候选分类，不改变账户状态 |
| `cooldown` | 账户的全局 `*` 冷却仍有效；模型 `scope`（状态记录范围）冷却只影响对应请求的候选分类 |
| `auth_required` | 账户级认证失败，需要重新登录或续签 |
| `unavailable` | 当前运行时无法使用账户 |
| `disabled` | 账户配置已停用 |

认证结果携带 `authGeneration`（认证状态版本）与 `checkedAt`（检查时间），仅在账户对象、authGeneration 和时间顺序均匹配时应用；同一时间点的成功结果优先于失败。模型成功与冷却写回使用独立 `modelAccessGeneration`（模型状态版本）和 `checked_at`，目录变化时 modelAccessGeneration 递增，同一时间点已有 `verified` 时保留成功状态。账号全局 `*` 上剩余不到 2 分钟（`ShortCooldownMax`）的分钟限额或限流冷却（`shortCooldownActive`，原因以 `分钟限额: ` 或 `限流: ` 开头，见 `rateCooldownReason`；`QuotaCooldownForError` 写的原因为“类型: 原文”，落盘后前缀照样保留）只等到期：同账号其他请求的成功写回（`markModelAccessVerified`）与非生成操作的成功清除（`ClearCooldownIfGeneration` 顺带清 `*`）只把 `*` 的 `checked_at` 推进到本次时间、不删除这条冷却，因为取得租约更晚的请求先到上游并成功，证明不了这一分钟的额度已经恢复；`checked_at` 推进后，租约早于它的请求再写这个作用域的冷却会被当作过旧跳过。其他冷却照旧被成功清除：剩余不少于 2 分钟的冷却（每日限额被成功纠正、403 暂停被上传成功解除都不变，最后 2 分钟同样），以及转录上传失败等暂时故障写的 30 秒全局冷却（原因为错误原文，不带额度前缀）；模型作用域自己的冷却清除规则不变。淘汰选号的 `WarmWorkerStates` 仍只看剩余时间，不看原因。冷却写入用 `TryMarkCooldownIfGeneration` 返回写入结果（`CooldownWritten`、`CooldownSkippedGeneration` 目录代际已变、`CooldownSkippedStale` 该作用域已有不早于本次租约的记录），`MarkCooldownIfGeneration` 签名不变、只返回错误。

模型访问 scope：

| 操作 | scope | 成功与失败语义 |
| --- | --- | --- |
| 普通 GenerateContent | `<modelID>` | 规范 `EventFinish` 到达后写 `verified`；Code 7 保留已有记录 |
| CountTokens | `count-tokens:<modelID>` | 成功清该 scope 冷却，模型 `verified` 保持原值 |
| Transcribe | `<modelID>` | 非空文本或 segments 写 `verified`；Code 7 保留已有记录 |
| Live 纯文本 | `<modelID>` | setup 成功写 `verified`；每次 `SendText` 开始一次模型资格检查，`turn_complete` 更新成功 |
| Live 音频或图像 | `bidi-media:<modelID>` | `SendMedia` 开始一次模型资格检查并更新媒体 scope |
| Robotics | `bidi-media:<modelID>` | `SendText` 开始一次模型资格检查，`turn_complete` 更新成功 |

`ModelAccessKey(scope, model)` 会移除 `models/` 前缀；空 scope 返回规范模型 ID，非空 scope 返回 `<scope>:<canonicalModelID>`。Bidi setup 使用 lease（账户租约）时间，会话内每个 qualifying turn（一次模型资格检查）分配严格递增的 attempt 时间，`turn_complete` 消费对应 attempt。普通流式生成在规范 `EventFinish` 到达时写 `verified`；此前的 text、reasoning、tool、usage 和首事件用于输出与性能统计，终态前断流、取消或错误保持原验证状态。

模型目录刷新为全部 enabled ready/busy 账户并发执行。同步报错或返回空目录的账户进入 generation 内的 pending ID 集合；每个非空结果立即更新公共目录、账户状态并在 RUNNING 期间预热更多 Worker。初次 fan-out（同时向全部符合条件的账户发出 `ListModels`）结束后，单个 30 秒 ticker（定时器）对可用的 pending 账户再次并发刷新。冷却中的任务保留到期再试；需要登录、停用、不可用或已删除的账户退出重试，重新登录后重新同步；管理页单个启用或批量启用的账户没有模型目录时，在后台按 16 个并发（两者共用）补同步，一批同步完合并应用一次目录；认证文件更新后启用的账户随即同步一次。失败日志记录账户与原因，恢复成功记录模型数量。`modelRevision` 跟踪账户和配置变化，生成服务开始接收请求前会确认已应用当前 revision。

认证状态包含长期凭证和设备绑定材料，保存在本机受控目录。提交、Issue、CI 和普通日志使用脱敏材料，保留字段形状并替换 Cookie、token、proof、邮箱、账户 ID、提示词、响应正文和完整原始帧。

### 429 治理：换号上限、出口限流、目录缓存与登录复核

线上单出口直连、1.2 万账号时出现过“无缘无故 429”：额度类 429 换号没有上限、号池级保护从不触发、逐级冷却把驻留 Worker 盖成死槽位再淘汰冷启动、每次重启全量同步模型目录、需要登录的驻留 Worker 一直占着槽位、一次 401 就判死且永不恢复。现在的设计：

- **换号上限**（`internal/app/quota_switch.go`、`primary_models.go`）：不固定账号与文件绑定的生成请求额度失败分两类计数，每日类最多再换 4 个账号、限流类最多再换 2 次（出口限速判为无效时放宽到 4 次），限流类失败后紧接着的选号只用已启动的 Worker；每请求最多 8 个不同账号（`generateSwitchCeiling`，单独的 `tried` 集合，不受 `maxAttempts` 补偿影响）；因登录失效换号最多 2 次（`authFailureSwitchLimit`）。达到上限按 `PrimaryQuotaError`（`Kind` 区分 Ultra 主用、普通换号上限、上游出口限流）返回 429 与 `Retry-After`。CountTokens、Live、转写、Veo 与自动选号的文件上传经 `aistudio.WithAttemptCeiling` 最多试 4 个账户（上传原先在认证失败时把就绪账户试个遍）。出口限流期间转写、Live、Veo 的候选去掉受限出口上的备用账户（还有热 Worker 时，`withoutLimitedStandby`；这些路径由账户池按 ID 顺序选号，不认调用方排好的热 Worker 优先），冷启动被闸门暂停时与指定账户的生成请求一样按号池繁忙 429 返回（`newEgressColdStartBusy`）。Ultra 主用模型的规则不变。
- **出口限流判定**（`internal/app/egress_guard.go`）：按出口键（账号代理或 IP 池出口的规范化值，空为直连，见 `aistudio` 的 `egressKeyOfLocked`；IP 池的接线见下文“IP 池”的“与 egress_guard 的关系”）统计已发往上游的生成尝试（成功、裸 429、其他失败），2 分钟窗口内裸 429 来自足够多的账号、比例足够高（或冷号首发被拒）时成立。成立期间裸 429 只短冷却 10 秒不升档，并撤回第 2 档以上的逐级冷却；判定期间连续被拒、与出口按比例拒绝区分得开的账号写 30 分钟账号冷却。判定器有独立的 5 秒计时器（不放进 `keepWarmPass`：清扫死槽位会同步等浏览器关闭），只在自己的锁内算出动作，解锁后再调账户池与写日志；计时里删除空闲的正常出口状态（`pruneIdleLocked`），状态接口在出口多于 `egressStatusFullLimit`（16）个时只列出判定成立或已撤销的出口（`adminStatus`，正常的计入 `egress_summary`），出口少时与 `status()` 相同、全部列出（诊断第 0 段要看正常出口在门槛附近的计数）。
- **每出口令牌桶**（`internal/aistudio/egress_rate.go`）：账户池按出口、按分区（普通、Ultra）各一条 GCRA 令牌桶，限速由判定器设下（初值为最近 60 秒成功 ×1.25），主用模型优先、非主用保底 25%；AIMD 降速后成功数下降就回退并锁定 10 分钟，限速 3 分钟仍不改善被拒比例就判为无效、停止限速（其余保护保留）。非主用模型按出口排队整个请求累计不超过 30 秒（`egressWaitBudget`），转写经 `WithEgressWaitLimit`。
- **冷启动闸门**（`coldStartGateFunc`，`admitColdStart`）：出口限流期间低于常驻数的补位只受并发约束（max(2, WARM_STARTUP_CONCURRENCY/4)），驻留里没有能服务该模型的热 Worker 时的“补覆盖”另受每分钟 6 次配额，按需替换与超出常驻的扩容暂停，账户自己重开不经闸门（生成请求的每次尝试把发送时的冷启动标为重开；转写、Live、Veo 经 `accountWorkerManager.Worker` 的冷启动按“按需”过闸门，暂停时按号池繁忙 429 返回）。按需替换的每分钟次数在闸门放行后才记。按需替换与扩容另有启动宽限：不换就绪不到 2 分钟的 Worker（不可调度与死槽位除外，`pickReplaceVictim`），每个分区每分钟最多 10 次按需替换（`replace_rate.go`）；两项都只在 `newRuntime` 打开。现场提升判断“能否替换”（`promotable`）与 `ensureWorker` 的淘汰选号用同一个函数 `replaceVictimFor`：同分区有 Worker 在启动时（`victimReplaceable`）除冷却、死槽位、不可调度的之外，对请求模型不可服务或空闲超过 2 分钟的健康 Worker 也可以换掉。主用模型等这次选号发起的后台扩容（等待原因 `Worker 正在扩容`）时，等待的时间与现场冷启动一样延后排队截止时间（`leaseWaitBudget.expansionCredit`，合计不超过 `INIT_TIMEOUT`），Ultra 主用模型的流式请求写出响应头前的等待同步推迟；号池繁忙文案把它写成“等后台扩容”，与现场冷启动分开。
- **坏号记忆**（`internal/app/bad_accounts.go`）：按账号（不分模型）连续 2 次裸限流、期间没有成功即标坏号 2 小时，移出预热、扩容与替补候选（还有其他候选时），成功一次清除；预热与替补优先最近 24 小时成功过的账号；记录写在 `runtime/model-catalogs/bad-accounts.json`。出口判定的冷号首发不计坏号，与出口判定解耦（多出口时每出口的判定门槛很难达到）。
- **模型目录写盘缓存**（`internal/app/catalog_cache.go`、`catalog_sync.go`、`internal/aistudio/catalog_restore.go`）：`runtime/model-catalogs/` 的 index 加内容寻址 blob，装配运行时一次加锁恢复账户池与 Client 的目录和权益（含 Pro/Ultra 的权益头），后台调度器只刷新缺失、不一致、疑似失效、失败待重试与过期的账户，只取空闲账户，按优先级的小顶堆调度；上游 429 全局指数暂停，出口限流时推后，其他失败按账户退避；空目录在账户池与 Client 两侧都不覆盖已有目录。后台刷新（`TryRefreshAccountModels`）成功不清账号的全局冷却（`modelsForLease` 的 `clearCooldown`），管理操作照旧清；第一轮只算启动时就到期的缺失任务，一个目录都没有恢复出来时缺失任务不等重启前的退避；清理删掉的条目同时从内存删掉。`internal/app` 的 `TestMain` 把 `catalogCacheDir` 指到进程级临时目录，测试不会在包目录写 `runtime/model-catalogs/`。
- **不可调度驻留 Worker 的释放**（`internal/app/worker_unschedulable.go`）：需要登录、已停用、不可用、目录加载过却没有可初始化模型的驻留 Worker 登记释放（与冷却淘汰共用登记表与淘汰循环），空闲即关，忙碌等请求结束，不排空；淘汰选号把它排在死槽位之前，`victimCooling`、`victimDead` 模式也可选它。验证、登录、认证文件更新不再清空目录。
- **登录失效的收紧与复核**（`internal/aistudio/auth_gate.go`、`internal/app/auth_recheck.go`）：`DefinitiveAuthenticationFailure` 只认 Cookie 认证的 MakerSuite RPC 的 401 / Code 16（`Drive*`、`GenerateAccessToken` 除外，后者经 `AccessTokenAuthFailure` 只写 2 分钟短冷却）。请求路径统一调 `AccountLease.MarkAuthenticationFailure`：上游 401 先交给账户池的 `AuthFailureGate`，运行时由登录复核器写“疑似登录失效”5 分钟冷却并排队，用 `PooledService.ProbeLogin`（`WithoutAuthRecovery` 的 `GetAiStudioBenefitTier`，Go HTTP、磁盘 Cookie）复核，仍是 401 才按疑似时的认证代际标记、释放驻留 Worker；本地证据（登录页跳转、签名 Cookie 缺失）不经 gate。同一个复核器每 15 分钟复核一轮按接口证据标记的需要登录账户（并发 2、每秒最多 1 个、按账户退避 15 分钟到 6 小时、出口限流期间暂停；浏览器证据——管理验证判定未登录、Worker 启动跳到登录页——不复核，`authRecheck.noteMarked`），通过即在独占租约内写回有效并放回目录队列。浏览器登录、认证文件更新、管理验证通过统一调 `trackedService.noteLoginRestored`，清疑似失效、登录复核记录与疑似登录失效冷却。非 Windows 上续签器视为不可用（`authRenewPlatformSupported`，不改 chromeauth）。Worker 启动完成后把浏览器 Cookie 合并写回认证文件（`MergeBrowserCookies`，按启动前的认证代际核对），`ReplaceCookies` 也改为按 (name, domain, path) 合并。管理验证在浏览器判定已登录后先用接口复核一次，401 时保留目录、标需要登录；返回带 `authenticated` 与 `verify_reason`，原因带跳转到的登录页 host+path（`aistudio.VerificationPagePath`）。验证不导出浏览器 Cookie（要改 camoufoxnative 的验证流程，不在本批）。
- **429 原因响应头**（`internal/api/official_errors.go`）：429 响应带 `X-AIStudio2API-Reason`（`local_queue`、`local_cooling`、`upstream_quota`、`egress_limited`），判断顺序与 `publicErrorFor` 一致，文案不变。

这些行为都没有新配置项：阈值是常量，测试经 `trackedService`、`accountWorkerManager`、`egressGuard`、`catalogSyncer`、`authRecheck` 上的实例字段改小。出口判定、令牌桶、冷启动配额、登录复核的状态只在内存，重启后从头开始；目录缓存与坏号记录写盘。一个代理键默认对应一个出口 IP，按连接轮换 IP 的代理会让判定与限速偏保守。日志见 [logging.md 的 429 治理](logging.md#429-治理)，诊断用 `bash tools/diag_busy.sh`（第 0、4b、9 段）。

### 长生成与中转重试

**三段期限**（`internal/app/generate_context.go`）：生成请求从 `startGenerate` 调用 `generateRequestContext` 起计时，期限分三段，都在这一刻按当时的 `REQUEST_TIMEOUT` 取值，热更新只影响新请求：

| 阶段 | 期限 | 计入 | 到期 |
| --- | --- | --- | --- |
| 首事件前 | `REQUEST_TIMEOUT` | 排队、选号、现场冷启动、回到原账号等 Worker 关闭、文件复制与附件上传、WAA proof、等首个上游事件与首事件前的换号重试 | 504，换号规则与原来相同；`request.error` 以 `本服务首事件前期限 <期限> 到期` 开头，在某次尝试中到期时带 `（停在 <位置>）`，文件资源刷新时到期带 `（停在 文件资源刷新）`。期限在换号退避中到期时按上一次尝试的错误区分：满足 `context.DeadlineExceeded`（首事件超时、BiDi 等待回复超时、协议位置读取超时等）时照样加前缀，截断日志的 `停在` 为 `-`；上一次是上游 RPC 错误（5xx、协议错误码等，不满足 `DeadlineExceeded`）时保留原错误与状态码，不加前缀，也不写截断日志 |
| 首事件后 | 空闲 `REQUEST_TIMEOUT` | `forwardEvents` 的看门狗：上游连续这么久既没有事件也没有网络字节（`upstreamActivity`，单调时钟）才到期；向下游交付被阻塞的时间（客户端读得慢）不算上游空闲 | 504（流式在流内），`本服务首事件后空闲期限 <期限> 到期（已输出 推理=N 正文=M）` |
| 整条请求 | `max(1h, REQUEST_TIMEOUT)`（`generateMaxDuration`，不是配置项） | 从建立上下文算起 | 504，`本服务总时长上限 <期限> 到期（已输出 推理=N 正文=M）` |

收到首个非错误上游事件、转发之前 `generateWithRetry` 调用 `beginStreaming`，在同一个上下文上把期限延长到总时长上限，之前派生的尝试、上游连接与响应体随之延长；首事件与首事件前期限同时到达时这个事件丢弃（还没有向客户端输出任何内容），按首事件前到期结束。三种错误都只解包为 `context.DeadlineExceeded`：对外仍是 504 官方原句 `Deadline expired before operation could complete.`，状态记 `failed`，不含“本服务”字样；管理端取消、生成服务停止与客户端断开照旧立即结束两个阶段。开始输出后照旧以各协议的流内 error 结束，不伪造 `finish_reason`。非流式请求同样不再在 `REQUEST_TIMEOUT` 处被切断。计数、视频、转写与 Live 等其他请求仍用 `dataRequestContext`，整段受 `REQUEST_TIMEOUT` 限制。进度日志与排查记录的文案见[运行日志](logging.md#流式等待)。

**截断来源的分辨**：客户端看到的文案都相同，区别只在 `request.error` 与用量账本的 `error`：

| `error` | 来源 | 耗时特征 |
| --- | --- | --- |
| `本服务首事件前期限 …` | 本服务首事件前期限：首字之前就到时 | 约 `REQUEST_TIMEOUT`，`first_event_ms` 为 0 |
| `本服务首事件后空闲期限 …` | 开始输出后上游卡住 | 任意；请求时间线的最后一次 `事件流停顿` 之后没有 `事件流恢复` |
| `本服务总时长上限 …` | 总时长上限 | 约 1 小时（或 `REQUEST_TIMEOUT`） |
| `… 返回 HTTP 504、协议错误码 4: Deadline expired …` | Google 侧的期限（`aistudio.RPCError`） | `duration_ms` 是 Google 期限的估计 |
| `AI Studio stream closed before finish` | 上游在结束帧之前断流（浏览器中继、网络） | 任意 |
| 只有 `context deadline exceeded` 本身（没有前缀） | 旧版本的整段 `REQUEST_TIMEOUT`，或其他数据请求。带 `AI Studio … 协议位置`（浏览器连续 60 秒没有回应）或 `BiDi … 等待回复超时` 的是上游或浏览器链路超时，看同账号同一时刻的告警，不是本服务期限 | 约 `REQUEST_TIMEOUT`；链路超时任意 |
| 状态 499、`context canceled` | 客户端、中转或 CDN 断开，管理端取消，停止服务 | 经 Cloudflare 的非流式约 100 秒 |

`bash tools/diag_regen.sh` 按这张表分类并找出相同输入的重发（用法见 README 的“长回复中途截断或重新生成”）。

**new-api 中转**（依据 new-api `d8e6561` 的源码，其他版本可能不同）：

- Claude 类型渠道：流里出现 error 事件时一律改写成 500（`relay/channel/claude/relay-claude.go`），原来 504 不重试的排除规则因此失效；重试循环（`controller/relay.go`）不检查响应是否已经写出，500 又在默认的可重试范围内（`service/relay_error.go`、`setting/operation_setting/status_code_ranges.go`），所以失败重试次数（`RetryTimes`，全局设置，默认 0）大于 0 时会在同一个下游连接里重新请求本服务：客户端在同一条回复里先看到截断的前半段（没有 `message_stop`），再看到第二个 `message_start` 从头生成，最多重复 `RetryTimes`+1 遍。本服务每个 HTTP 请求都生成新的请求 ID，看到的是相同输入、ID 不同的新请求。处理：把重试次数设为 0；其他渠道需要保留重试时，把接本服务的渠道改成 OpenAI 类型（OpenAI、Gemini 类型渠道开流后的错误不重试）。渠道的状态码复写把 500 改成不重试的状态码也能避开，但会改变客户端看到的状态码。
- new-api 只转发上游的 `data:` 行：本服务 OpenAI Chat、Responses 与 Interactions 的心跳 `: ping` 注释帧不会到达客户端（读到它们仍会重置 new-api 自己的 `STREAMING_TIMEOUT`）；Anthropic 的心跳是 `data: {"type":"ping"}`，照常转发。客户端隐藏思考（思考期间没有 `data` 事件）而 new-api 前面又有 Cloudflare 时，Cloudflare 约 100 秒读不到字节就返回 524：开启 new-api 自带的流式 ping（系统设置的 `general_setting.ping_interval_enabled`，间隔 `ping_interval_seconds` 小于 100 秒）。
- 非流式请求在生成完之前一个字节都不写，也没有心跳：前面有 Cloudflare 时约 100 秒返回 524（本服务记为约 100 秒的 `cancelled`/499），客户端或 SDK 自身的超时也可能先到，重发即整段重新生成。长思考模型请用流式。本服务不做“先写 200 再补空白保活”：之后的错误只能以 200 加错误体返回，会破坏 SDK 按状态码的处理；也不在输出后续接或重做，那样一定产生重复内容。

**隐藏思考与单次调用**：模型名带 `-nothinking` 后缀时上游照样返回思考块，只是本服务在本地丢弃，首事件就是第一个思考事件，思考的网络字节同样算上游活动。Anthropic 传 `thinking.type=disabled`、Gemini 传 `includeThoughts=false` 时 Build 通道上游不返回思考，Playground 通道上游照样返回、本服务在调用上游的一层就丢弃（不产生推理事件，截断日志的 `推理` 不计入，思考字节仍计入 `网络字节`）；管理页模型目录（`GET /api/models`）里 `build_unary=true` 的模型首事件就是整段回复。后两种情况整段思考（或整段生成）都计入首事件前期限，要按最长的生成调大 `REQUEST_TIMEOUT`。

**Worker 层的影响**：开始输出的生成请求最长持续到总时长上限，排空超时与后台关闭失败 Worker 的等待上限随之改为总时长上限 + 30 秒（`drainTimeout`、`workerResetLimit`），不再在长流期间反复放弃、重新登记：

- 同账号协议 Code 5 的 Worker 要等长流结束才能关闭重建（最长约 1 小时 30 秒，不切断长流）；这期间只能用这个账号的请求（指定账户、文件绑定，或换号找不到其他账号的）在自己的首事件前期限内以 504 结束（主用模型为 429），不固定账号的请求换到其他账号。这是不切长流的代价。Live 同样：不固定账号、还有其他候选时只等 2 秒（`defaultWorkerResetWait`）就换号（`dataResetSwitchWait`；有进行中后台关闭的候选不算，换过去会在取租约里排到那次关闭结束，原账号先关闭完成也用不上），转写只在协议 Code 5 时这样做（Worker 浏览器失败的错误类型不一，aistudio 层只对超时、网络错误、断流换号，其余直接失败；照旧等关闭完成后在原账号重放。浏览器退出时同账号的流随之断开，关闭很快完成；浏览器卡死时，同账号的流最多在 60 秒无进展（`protectedStallLimit`）后断开）；只剩这个账号（Live 还有指定账户与会话绑定）时转写照旧等到 `REQUEST_TIMEOUT`，Live 的请求没有期限，最多等 `REQUEST_TIMEOUT` 后以 503 结束，原因按后台关闭的结果写明（等满上限没有关闭重建、关闭失败的 Worker 出错、后台关闭已放弃或服务正在重启，或到期后关闭恰好刚完成，`liveResetExpired`）；之后按同一账号重试时，这个账号的后台关闭还没结束就先等它，同样最多 `REQUEST_TIMEOUT` 后以 503 结束（`awaitLiveReset`，到期后关闭恰好成功结束时照常建立）。Live 会话建立的取租约另有 `REQUEST_TIMEOUT` 的期限（`liveSetupDeadline`）：只在取租约时计时，取到租约后解除，恢复回调返回、准备换号或重放时重新计时，到期以 503 结束（“Live 会话建立在 … 内没有取到账号”）；覆盖候选都在后台关闭、排队后同账号才登记关闭（`awaitLiveReset` 只检查进入时已登记的关闭）与候选并发一直占满。会话本身的上下文由建立用的上下文派生（aistudio 层不另分），期限只在计时中取消、建立结束后不再生效，会话建立成功后不受影响。视频不换号：aistudio 层的视频循环只对 401、403、429、5xx 换号，Code 5 与 Worker 浏览器失败都不换号，所以恢复照旧等关闭完成后在原账号重放，受 `REQUEST_TIMEOUT` 限制（同账号的长流一直在输出时以 504 结束）。
- 排空中的冷却占位可能保持到长流结束，有空槽时替补在排空开始时已经启动。排空超过 `REQUEST_TIMEOUT` + 30 秒（`drainSlotTimeout`）后保留排空标记，但不再占排空名额（`drainSlotCountLocked`），同分区其他忙碌的死槽位照常排空；让出额度仍最多 `drainLimit` 个，这时的替补多半因槽位已满没有启动，旧 Worker 关闭后补上，浏览器总数仍不超过峰值加 `drainLimit`。排空总数（含已超时、不再占名额的）另有上限 2 × `drainLimit`（`drainTotalLimit`）：替补起不来的排空只让账号不再接不固定账户的请求，长流各自占着的死槽位逐个超时、逐个再排空时，分区可接单的 Worker 会降到远低于峰值；有这个上限，可接单的 Worker 不少于峰值减 `drainLimit`。运行日志 `WAA Worker 排空 | … | 排空中=<N>（占名额 <M>/<drainLimit>）` 中 N 为分区全部排空数，M 为占名额的排空数。
- 空闲回收与淘汰选号按最近活动时间判断空闲（取得租约与普通租约全部释放中较晚的一个，`Account.lastReleased`），刚结束长流的 Worker 不会被当成空闲了整条流的时长先被关闭；状态页的 `last_used` 仍是最近一次取得租约的时间。
- 管理端保存账户、重新登录、验证登录最多等 `REQUEST_TIMEOUT` + 30 秒取得账户（`acquireAccountForOperation`）：不超过 `REQUEST_TIMEOUT` 的请求照常等它们结束后生效，同账号有长流仍在输出时返回 409 `account_busy`（`账户有长时间生成在进行，稍后重试`），登记随之撤销，不让一次点击把账户挂到长流结束。
- 认证续签（等同账号其他请求最多 12 秒后照常续签）、需要重建生成服务的配置修改（最多等 30 秒）与退出（最多等 `SHUTDOWN_GRACE`，默认 60 秒）仍会中断进行中的长流。

### 代理写法与规范化

全局 `PROXY` 与账户 `proxy` 只经 `config.NormalizeProxy`（`internal/config/config.go`）一个函数规范化，并且只在值进入系统的入口调用：`config.Load`（`.env` 与进程环境变量，经 `CanonicalProxy`）、`Config.UnmarshalJSON`、`UpdateRuntimeConfig`（`PUT /api/config`）、账户的创建、Chrome 导入与编辑（三个管理入口在 `Validate` 之前规范化，错误加固定前缀 `账户 proxy 无效: `；Chrome 导入在 OAuth 网络导入之前就校验）、`readAccountConfig`（外部手写的写法读入时改写，读时不回写磁盘）、`parseFlags` 与 `parseSetupFlags`（只在显式传了 `--proxy` 时）。`sameDataConfig` 对页面提交值再做一次 `CanonicalProxy`，幂等、无副作用。`.env`、`account.json`、`config.Config`、`AccountConfig` 与 `generation.config` 里一律存规范化后的上游 URL，永远不存本机中转地址，因此保存值与生效值逐字节相等：`requiresRebuild`、`sameDataConfig`、`clientForProxy` 的缓存键两边一致，不会出现“需要重启”的循环，`ServiceRestartRequired` 只在上游真正变化时为真。设置页保存后把服务端返回的规范值同步回表单（`SettingsPanel.vue` 的 `doSave`，只在提交了失焦字段且提交后没有再改动时同步），否则会一直显示未保存、每次失焦重复保存。`ProxyInput.vue` 的“测试代理”按钮在 `mousedown` 上 `preventDefault`，点击时输入框不失焦、不触发失焦保存，测的是当前输入的值；键盘 Tab 到按钮时输入框会先失焦保存，所以组件在测试开始与结束时发 `testing` 事件，`SettingsPanel.vue` 的 `onProxyTesting` 在测试期间暂停或推迟“自动应用”的倒计时，结束后只在两条路径都拿到出口 IP、而且仍需要应用时重新开始，否则留给用户点“立即应用”。

| 输入 | 规范输出 | 说明 |
| --- | --- | --- |
| ` http://127.0.0.1:7890 `、`HTTP://Proxy.example.com:8080/`、`http://h` | 去首尾空白后原样返回 | 不带认证的合法 URL 走直通分支（`legacyProxyValid`，即旧 `ValidateProxy` 去掉“不能包含认证信息”），Firefox prefs、user.js、Go 传输拿到的字符串都与以前逐字节相同；唯一的例外见下面 `http://h:1:u:123` 一行 |
| `http://h:1:u:123`、`http://h:1:u:` | `http://u:123@h:1`、`http://u:@h:1` | 旧规则接受、但会改变的唯一一类值：带协议的冒号写法、密码为纯数字或为空时，`url.Parse` 把它读成主机名带冒号（`h:1:u`）的无认证 URL，原来就连不上。这里先判冒号写法（`colonProxyCredentials`）、再走直通分支，与方案 2.3 的“直通分支优先”顺序不同，是有意的；其中按冒号写法报错的值（`http://myuser:54321:1.2.3.4:8080` 报 `errProxyAmbiguous`，`http://h:0:u:1` 报 `errProxyPort`）旧规则接受、现在拒绝，写在 `.env` 里时 `config.Load` 失败、进程启动即退出，写在 `account.json` 里时 `LoadTolerant` 跳过这个账户。`TestNormalizeProxyLegacyPassthrough` 把这类值作为例外逐条列出 |
| `1.2.3.4:8080:user:pass` | `http://user:pass@1.2.3.4:8080` | 不写协议按 http；冒号写法里第 3 个冒号之后全部算密码，按字面处理 |
| `user:pass@1.2.3.4:8080` | `http://user:pass@1.2.3.4:8080` | 按最后一个 `@` 拆分，userinfo 按第一个 `:` 拆成用户名与密码，各自 `url.PathUnescape`（解码失败按字面） |
| `socks5://1.2.3.4:1080:user:pass` | `socks5://user:pass@1.2.3.4:1080` | 带协议也可以用冒号写法 |
| `socks5h://u:p@h:1080`、`socks5h://h:1080` | `socks5://…` | socks5h 按 socks5 处理 |
| `1.2.3.4:8080` | `http://1.2.3.4:8080` | 旧规则拒绝，现在补上 http |
| `http://u:p@h` | `http://u:p@h:80` | 带认证时补默认端口：http 补 80、https 补 443，SOCKS5 必须写端口 |
| `[::1]:8080:u:p` | `http://u:p@[::1]:8080` | IPv6 必须写成 `[v6]` |
| `http://2001:470::1:3128` | 原样 | 不带认证、不加方括号的 IPv6 旧值照旧走直通分支（`bareIPv6URL`），不当成冒号写法拆账号密码；不写协议或带账号密码时报 `errProxyIPv6` |
| `http://h:1::2` | 原样 | 冒号写法的用户名为空不可能是账号密码（`colonProxyCredentials` 要求用户名非空），照旧走直通分支 |
| `myuser:54321:1.2.3.4:8080`、`1.2.3.4:8080@myuser:54321` | 报 `errProxyAmbiguous` | host 段不是 IP、不含点、也不是 `[v6]`，而 用户名:密码 段本身像 IPv4、带点域名或 `[v6]` 加端口（`ambiguousProxyOrder`）：整串更像把账号密码与 host:port 写反了（`用户名:密码:host:port` 或 `host:port@用户名:密码`），照常拆分会把数字密码当成端口、真实密码进日志与页面。冒号写法按字面判断；`@` 写法与 URL 写法按解码后的用户名与密码判断（`a%2Eb:1234@localhost:8080` 同样报错，否则规范输出 `http://a.b:1234@localhost:8080` 再规范化会报错，函数不幂等）。代理主机名不含点、用户名像域名、密码是纯数字时同样报错，只能把代理主机写成 IP |
| `http://user:12/ab@h:8080` | 报 `errProxyPathCredential` | `url.Parse` 把 `user:12` 读成 host:port、其余读成路径；最后一个 `@` 在第一个 `/?#` 之后时提示把 `/ ? #` 编码或改用冒号写法（不改判定，`http://h:8080/x@y` 照旧拒绝） |

账号密码按严格的百分号编码输出：只保留 `A-Za-z0-9-._~`，其余字节（包括 `$&+,;=:@/?#` 与空格）一律编码，因此 `formatEnvValue` 写回 `.env` 时不需要引号，`source .env` 也不会遇到 shell 元字符；`url.Parse` 解码后与原文完全一致，Go 传输、tls-client 与 x/net 都按 `User.Username()`、`Password()` 取用。规范输出里密码的 `:` 已编码、用户名不含 `:`，不会再命中冒号写法，函数是幂等的。`@` 写法与 URL 写法里合法的 `%XX` 会被解码，要原样保留 `%41` 这类字面量请用冒号写法（界面说明里写了这一条）；不写协议时只要开头是 `host:数字端口:` 就按冒号写法处理（例如 `user:123:x@host:99` 会被当成 host `user`、端口 123、用户名 `x@host`、密码 `99`；只有 用户名:密码 段整体像 `IPv4 或带点域名:端口` 时才按上一段的歧义报错），不写协议时端口为空（`1.2.3.4:`）算没写端口，首尾空白会去掉，所以密码不能以空格开头或结尾。

错误全部是固定文本，不含任何输入内容（`url.Parse` 的错误里有原文与密码，不能用 `%w` 包装）：

| 错误 | 文案 |
| --- | --- |
| `errProxyFormat` | `PROXY 格式无效：支持 http://host:port、http://用户名:密码@host:port、socks5://用户名:密码@host:port、host:port:用户名:密码、用户名:密码@host:port（不写协议按 http）` |
| `errProxyScheme` | `PROXY 协议必须是 http、https 或 socks5（socks5h 按 socks5 处理）` |
| `errProxyPath` | `PROXY 不能包含路径、查询参数或片段` |
| `errProxyPort` | `PROXY 端口必须是 1 到 65535 的数字`（不允许前导零） |
| `errProxySOCKS5Port` | `SOCKS5 代理必须写端口` |
| `errProxyNoPort` | `PROXY 不写协议时必须写端口，例如 1.2.3.4:8080` |
| `errProxyUser` | `PROXY 的用户名不能为空` |
| `errProxyControl` | `PROXY 不能包含换行、制表符等控制字符` |
| `errProxySOCKS5Length` | `SOCKS5 代理的用户名和密码都不能超过 255 字节` |
| `errProxyCredentialLength` | `PROXY 的用户名和密码合计不能超过 1024 字节` |
| `errProxyUserColon` | `HTTP 代理的用户名不能包含冒号（:），Basic 认证会在第一个冒号处拆开用户名和密码`（只可能来自 `%3A` 解码；SOCKS5 按长度传用户名，不受影响） |
| `errProxyAmbiguous` | `PROXY 无法判断账号密码与 host:port 的先后（host:port:用户名:密码、host:port@用户名:密码 还是 用户名:密码:host:port），请改用 http://用户名:密码@host:port 写法；代理主机名不含点时请写成 IP` |
| `errProxyIPv6` | `PROXY 的 IPv6 地址必须加方括号，例如 http://[2001:db8::1]:8080` |
| `errProxyPathCredential` | `PROXY 不能包含路径、查询参数或片段；密码里有 / ? # 时请写成 %2F、%3F、%23，或改用 host:port:用户名:密码 写法` |

旧文案“PROXY 不能包含认证信息”已删除。`config.RedactProxy` 返回去掉账号密码的 `scheme://host:port`（不用 `url.URL.Redacted()`：它保留用户名，动态 IP 的用户名里常带 session 参数），供日志、“测试代理”结果与提示使用；`config.ProxyHasCredentials` 按规范化结果判断是否带认证。管理接口照旧返回完整的规范值（与 `proxy_api_key` 明文返回同级），只在前端脱敏显示（`web/src/proxy.ts` 的 `maskProxy`，账户表格、搜索与 `ProxyInput.vue` 的掩码按钮都用它）：服务端返回掩码的话，onboarding 回写、`setEnabled`、编辑草稿和设置页的 payload 会把掩码写回磁盘。`maskProxy` 与服务端的歧义判定一致：会报 `errProxyAmbiguous` 的值（冒号写法与 `@` 写法，`@` 写法同样按解码后的值判断）显示为 `***`（不把被当成端口的数字密码显示出来），不加方括号的 IPv6 旧值、以及服务端按直通分支原样保留的 `http://h:1:2`、`http://h:1::2` 这类旧值原样显示，不标“带认证”；`tools/report.sh` 写 config.txt 时对前者输出 `<已隐藏>`。

### 本机代理中转

Camoufox 通过 Firefox prefs 配代理，prefs 里没有账号密码字段（`internal/camoufoxnative/launcher.go` 也拒绝带认证的值），所以带账号密码的代理由 `internal/proxyrelay` 在本机起一个中转交给 Camoufox。只有 Camoufox 走中转：WAA Worker（`aistudio.NewNativeWorker`）、隔离登录与登录验证（`NativeLoginDriver.Login`/`Verify`，覆盖管理页新增账户、重新登录、验证、新账户自动处理、`setup --login` 与 go 后端的 `lazyLoginDriver`）。Go 侧消费者（业务 RPC、Drive、WebChannel、公开头提供器、Go WAA、chromeauth 导入与续签、setup 验证）本来就支持认证，直接拿规范化后的带认证上游 URL：CONNECT 的 Basic 认证在 `transport_browser.go`，SOCKS5 的 RFC 1929 认证经 x/net，chromeauth 经 tls-client。上游代理收到的隧道字节、Google 看到的 TLS ClientHello 与出口 IP 两种方式完全一样，Go 侧走中转没有收益，只会多占 fd、多一段自研代码在流式生成的路径上。不带认证的代理不经过中转，所有路径的行为与以前逐字节一致。中转地址只存在于 `NewNativeWorker` 与 `NativeLoginDriver` 内部的局部变量 `options.Proxy` 里，不会流回 `.env`、`account.json`、配置快照或任何比较。

| 文件 | 内容 |
| --- | --- |
| `relay.go` | `Registry`、`Lease`、`Options`、包级默认注册表、`Acquire`、`SetLogger`、监听器生命周期（按活动判定关闭） |
| `upstream.go` | 上游解析、连上游（TCP 或 TLS）、HTTP CONNECT 握手、自写的 SOCKS5 客户端握手 |
| `http.go` | 本地 HTTP 中转：CONNECT 与 absolute-form 明文请求（含长连接上游的后台探测） |
| `socks.go` | 本地 SOCKS5 中转（服务端最小实现） |
| `pipe.go` | 双向对拷、半关闭、兜底计时 |
| `failure.go` | 失败分类（上游层面 / 按目标）、每个监听器的失败记录、`Describe`、`Scrub`、日志节流 |
| `limits_unix.go`、`limits_windows.go` | 进程级连接上限的默认值 |

包只依赖标准库，不依赖 `config` 与 `aistudio`。日志出口由 `proxyrelay.SetLogger` 注入：主程序在 `internal/app/lifecycle.go` 里接到请求日志的 `service` 来源，setup 写到标准错误；日志行见 [日志说明](logging.md#本机代理中转)。

**租约与监听器**：`proxyrelay.Acquire(upstream, owner)` 对不带账号密码的值（含空串与中转地址本身）返回直通租约，`Address()` 为原值，不起监听；带账号密码时每个租约新起一个只给它用的监听器（`127.0.0.1` 字面量、系统随机端口，同一上游的多次申请也各自独立，不共享）。中转协议跟上游一致：`http`/`https` 上游对应本地 HTTP 中转（`http://127.0.0.1:N`），`socks5` 上游对应本地 SOCKS5 中转（`socks5://127.0.0.1:N`），Firefox 继续按“HTTP 代理”或“SOCKS 代理”这一类行为工作，跟直连这个上游时一样。租约释放后，监听器要同时满足“没有进行中的连接”和“连续 5 分钟（`IdleGrace`）没有新连接”才关闭；不主动关闭进行中的隧道。释放后仍有新连接时照常服务（拒绝的话 Firefox 看到的是拒绝连接），并记一次 WARN。每个监听器只服务一个浏览器，连接上限与失败记录都按浏览器计，不会被别的使用者占满或张冠李戴。

| 使用方 | 申请 | 释放 | 浏览器可能还活着时的兜底 |
| --- | --- | --- | --- |
| Camoufox Worker | `NewNativeWorker` 启动浏览器之前 | `NativeWorker.Close()` 中 `runtime.Close()` 返回 nil 之后；关闭失败、停在 `WorkerClosing` 时保留租约，关闭重试成功后再释放 | 关闭失败时不释放 |
| Worker 启动失败 | 同上 | `camoufoxnative.Start` 返回错误后立即释放，错误经 `WrapFailure` 附上中转记录的原因 | `Start` 内部的 abort 结果被丢弃，确认不了浏览器已退出，靠按活动判定关闭：浏览器还在发连接就不关 |
| 隔离登录、登录验证 | `Login`/`Verify` 开头 | `defer` 释放 | `session.Close()` 的错误被 `errors.Join` 进返回值，驱动分不清浏览器是否已退出，同样靠按活动判定关闭 |
| 测试代理 | `runtimeManager.TestProxy` | 探测结束后 `defer` 释放 | 不涉及浏览器 |

Worker 与登录驱动经 `aistudio.acquireProxyRelay` 申请：中转解析不了的值（`ErrInvalidUpstream`，只会来自未经规范化校验的配置，例如带账号密码的 `socks4://`）不起中转，原值照旧交给 Camoufox，由它按原有规则以固定文案拒绝（不含账号密码），与改动前一致；测试代理的输入已规范化，不会遇到这种值。

残余风险：浏览器在 Close 失败后仍然活着、又连续 5 分钟没有任何连接，之后再发连接时监听器已经关闭，会收到拒绝连接（系统请求可能回退直连）；极小概率下这个端口已被另一个租约的新监听器拿到（约 1/28000），这时会经别的账户的上游出网。

**转发规则**：HTTP 中转手工读请求头并保留原始字节，只删掉所有 `Proxy-Authorization` 行、在空行前追加 `Proxy-Authorization: Basic <base64>`，其余行的顺序、大小写、内容一律不动（不用 `http.ReadRequest` + `WriteProxy` 重建：重建会按字母排序头部、提前 `Content-Length`、遇到 `Pragma: no-cache` 自动加 `Cache-Control`，这些会经上游传给 Google 的 OCSP 服务）。CONNECT 的上游响应头手工解析，只有严格等于 200 才算成功，成功后给浏览器写固定的 `HTTP/1.1 200 Connection established`，先转发两侧缓冲区里的字节再对拷，绝不读或关闭响应体。明文请求只接受 `http://` 绝对 URI（origin-form、`OPTIONS *` 与 `https://` 绝对 URI 回 400：前者挡住本机网页的 fetch 与 DNS rebinding，后者转发的话上游会替我们握 TLS、指纹就变了），每条下游连接对应一条上游连接，不复用、不合并，“每条连接换 IP”的代理语义不变；等下一个请求时后台盯住上游，上游先断开或先发字节就关闭客户端、不写任何字节，非首个请求在收到响应字节前失败也只关连接、不回 502，让 Firefox 像直连时那样重试；转发响应体期间后台盯住浏览器（`watchClient`），浏览器断开就让上游读立即超时，上游停在响应体半路、不再发字节时也不会一直占着 goroutine、fd、上游连接，监听器照常按活动关闭。连接数已满时先读完请求头再回 503（避免内核发 RST），第一个字节也只按 `HeaderTimeout` 等，被拒的连接不计入上限，不能各自占着 fd 等 `FirstByteTimeout`。SOCKS5 中转只支持 CONNECT，原样保留客户端的 ATYP 与地址字节（域名不在本地解析），上游握手用自写客户端（问候 `05 02 00 02` 与 x/net 相同，`io.ReadFull` 精确读取，不多读隧道字节）。隧道阶段不设空闲超时，死连接靠 30 秒的 TCP keepalive 发现。

| 期限或上限 | 默认值 |
| --- | --- |
| 接受连接后等第一个字节（`FirstByteTimeout`） | 3 分钟（Firefox 会预先建连、先空闲一段） |
| 读完请求头或 SOCKS 握手（`HeaderTimeout`，读完后清掉，不作用于请求体） | 30 秒 |
| 连上游 + TLS + CONNECT 或 SOCKS 握手（`UpstreamTimeout`） | 25 秒（小于 Go 侧 30 秒的握手期限） |
| 明文请求之间的空闲（`KeepAliveIdle`） | 3 分钟（大于 Firefox 长连接的 115 秒） |
| 一个方向结束后等另一方向（`HalfCloseGrace`） | 60 秒 |
| 单个监听器同时处理的连接（`MaxConnections`） | 1024（大于 Firefox `network.http.max-connections` 的 900，正常碰不到） |
| 整个进程同时处理的连接（`MaxTotalConnections`） | unix 取 `RLIMIT_NOFILE` 软限制的 1/4，夹在 1024 到 65536 之间；Windows 16384 |

都是包内常量，不是配置项。达到上限时不等待：HTTP 回 503（写之前先读完请求头，避免 RST），SOCKS5 回 REP=04；进程级连接数达到 80% 时记 WARN。资源估算：空闲监听器约 1 个 fd、1 个 goroutine、不到 10 KB；活跃隧道 2 个 fd、约 75 KB；监听器总数约等于同时活着的、带认证代理的浏览器数（受 `MAX_ACTIVE_WORKERS` 等约束）加上释放后 5 分钟内的，与账户总数无关；Worker 反复启动失败时每次多留一个监听器 5 分钟，按启动名额与单次启动耗时估算最多几百个。

**判定上游失败**：`Lease.WrapFailure` 有失败说明时，返回的错误另外包进 `proxyrelay.ErrUpstream`（经一个 `Error()` 为空串的私有标记，错误文字与以前逐字相同，管理页与 `tools/diag_*.sh` 照旧按文案匹配），调用方用 `errors.Is(err, proxyrelay.ErrUpstream)` 判断 Worker 启动失败是不是代理造成的：IP 池的端口健康（规则 B）与动态 IP 换会话只计这一类。`proxyrelay.TotalConnectionLimit()` 返回包级默认注册表的进程连接上限，IP 池开启时低于 `容量 × 16` 会 WARN 并提示调高 `ulimit -n`。

**错误映射**：上游失败一律用协议层错误表达，不直接断开（直接断开会落进 Firefox 回退直连的列表）。407 不原样转给浏览器：有界面的登录浏览器会弹认证框卡住，无头 Worker 会进入 BiDi authRequired 一直卡到 `INIT_TIMEOUT`；转成 502 后 Firefox 立刻显示错误页，Worker 快速失败，原因由 `Lease.WrapFailure` 附在错误后面（`…；本机代理中转：上游代理拒绝了账号密码（407），请检查用户名和密码`）。

| 上游情况 | 类别 | HTTP 中转回给浏览器 | SOCKS5 中转回给浏览器 | 失败说明（`FailureSince`） |
| --- | --- | --- | --- | --- |
| CONNECT 返回 200 | — | `HTTP/1.1 200 Connection established` | 成功回复 | — |
| 407 | auth | `502 Upstream Proxy Authentication Failed`，不带 `Proxy-Authenticate` | — | `上游代理拒绝了账号密码（407），请检查用户名和密码` |
| SOCKS5 认证失败 / 不接受账号密码认证 | auth | — | REP=04 | `上游 SOCKS5 代理拒绝了账号密码，请检查用户名和密码` / `上游 SOCKS5 代理不接受账号密码认证` |
| 其他非 200（403、429、502、503、504…） | target | 状态码原样，原因短语只留可打印 ASCII（最多 64 个字符），不转发响应体 | — | `上游代理拒绝了隧道：403 Forbidden（目标 aistudio.google.com）` |
| 上游 SOCKS5 回 REP≠0 | target | — | REP=04（上游的 REP 码不转发） | `上游 SOCKS5 代理连接目标失败：05 拒绝连接（目标 …）` |
| 连不上上游（拒绝连接、无法解析、网络不可达） | unreachable | `502 Upstream Proxy Unreachable` | REP=04 | `连不上上游代理 host:port（连接被拒绝）` 等 |
| 超时 | timeout | `504 Upstream Proxy Timeout` | REP=04 | `连接上游代理 host:port 超时` |
| TLS 握手失败（https 上游） | tls | `502 Upstream Proxy TLS Failed` | — | `与上游代理 host:port 的 TLS 握手失败` |
| 首个请求时上游响应格式错误或提前断开 | malformed | `502 Bad Upstream Response` | REP=04 | `上游代理的响应无法解析` |
| 长连接上的非首个请求失败 | — | 关闭客户端连接，不写任何字节 | — | 不记 |
| 中转连接数已满 | busy | `503 Relay Busy` | REP=04 | `本机代理中转连接数已满` |

上游层面的失败（auth、unreachable、timeout、tls、malformed、busy）总是附加；按目标的失败（target）只在调用方的 `relevant` 认为目标相关时附加：Worker 与登录路径传 `proxyrelay.GoogleTarget`，测试代理只认 `api.ipify.org`，某个第三方域名被代理商拦截不会附加到 Worker 的启动错误上。Go 侧错误由 `proxyrelay.Describe` 转成中文说明：x/net 的回复码错误只按 `unknown error ` 前缀识别，拨号阶段的 `*net.OpError` 判为连不上上游，不再匹配 `connection refused` 子串（拨号失败的错误里也有它）。所有拼进日志或错误的外部错误文本先经过 `proxyrelay.Scrub`（去掉用户名与密码的原文、百分号编码与 Basic base64 三种形式）。

**为什么 SOCKS5 中转的失败统一回 10 字节的 REP=04**（对照 `mozilla-firefox/firefox` main 分支的 `nsSOCKSIOLayer.cpp`、`nsHttpChannel.cpp` 等）：

| 中转回给 Firefox 的 | Firefox 内部的 NSPR 错误 | 映射成的 nsresult | 是否在回退直连的列表里 |
| --- | --- | --- | --- |
| REP=01、02、05、07 或其他 | `PR_CONNECT_REFUSED_ERROR` | `NS_ERROR_PROXY_CONNECTION_REFUSED` | 是 |
| REP=03 | `PR_NETWORK_UNREACHABLE_ERROR` | `NS_ERROR_PROXY_CONNECTION_REFUSED` | 是 |
| REP=06 | `PR_CONNECT_TIMEOUT_ERROR` | `NS_ERROR_NET_TIMEOUT` | 是 |
| REP=04、08 | `PR_BAD_ADDRESS_ERROR` | `NS_ERROR_UNKNOWN_HOST` | 否 |
| 直接断开或拒绝连接 | `PR_CONNECT_REFUSED_ERROR` 或 RESET | `NS_ERROR_PROXY_CONNECTION_REFUSED` / `NS_ERROR_NET_RESET` | 是 |

回退直连的列表是 `NS_ERROR_PROXY_CONNECTION_REFUSED`、`NS_ERROR_UNKNOWN_PROXY_HOST`、`NS_ERROR_NET_TIMEOUT`、`NS_ERROR_NET_RESET`。手动配置的单个代理没有 `mNext`，只有编译时定义了 `MOZ_PROXY_DIRECT_FAILOVER`（默认定义）、`network.proxy.failover_direct` 为 true（默认）而且请求是 beConservative 的系统请求时才改走直连：AI Studio 页面请求不会回退，会回退的是 Firefox 自己的系统请求（远程设置、更新等），它们会用真实 IP 连 Mozilla。所以 SOCKS5 中转用 REP=04 表达所有失败，代价是浏览器错误页显示“找不到服务器”，真实原因由 `WrapFailure` 带到管理页。失败回复写满 10 字节（Firefox 先凑齐 5 字节才解析，两字节的简写会让它一直等或按拒绝连接结束），写完后半关闭、最多读 1 秒或 4 KiB 剩余数据再关，避免内核发 RST。HTTP 中转回的 502、503、504 映射成 `NS_ERROR_PROXY_BAD_GATEWAY`、`NS_ERROR_CONNECTION_REFUSED`（无 PROXY_ 前缀）、`NS_ERROR_PROXY_GATEWAY_TIMEOUT` 等，都不在列表里。Camoufox 152 是否与 main 分支一致需要实测（抓包确认 407/503/504、SOCKS 认证失败/REP=01/REP=06 时没有直连外网的 SYN，监听器关闭作对照组）。

**本机安全**：只绑 `127.0.0.1`，端口由系统随机分配（Windows 上同样可行，只绑回环不会触发防火墙弹窗）；拒绝 origin-form 与 `https://` 绝对 URI，SOCKS5 只支持 CONNECT。中转本身没有认证，本机任何进程都能在浏览器存活期间借它出网，这与 Camoufox 以 `--remote-debugging-port=0` 在回环上无认证暴露浏览器控制权是同一级别。账号密码不再写进 Camoufox 临时 profile 的 user.js，带认证时 user.js 里只有 `127.0.0.1:N`。`https://` 不带认证时 Camoufox 以明文连接代理的 TLS 端口（现状，保留；“测试代理”给出 `https_plain_browser` 提示）。

### IP 池

“没有账户代理时用什么出口”从固定的 `PROXY` 换成可热更新的 IP 池（`PROXY_POOL_*`）：静态 IP 时每个运行中的浏览器（Worker、登录、验证）独占端口段里的一个端口，动态 IP 时每个账户用一个固定会话号。实现分在 `internal/config/proxy_pool.go`（类型、规范化、校验、`StaticURL`/`SessionURL`、`EndpointKey`）、`internal/app/proxy_pool.go`（分配、租约、冷却、状态）、`proxy_pool_store.go`（持久化）、`proxy_pool_check.go`（检查全部端口）、`worker_proxy.go`（Worker 接入、容量、跟随默认代理）、`proxy_pool_admin.go`（登录、验证、导入的租约，`/api/config` 字段，检查全部端口与跟随默认代理的管理接口）与 `proxy_pool_probe.go`（抽测 IP 池与测试账户实际出口）；`internal/aistudio` 只多了 `ProxyBinding` 与账户池的批量推送接口（`proxy_binding.go`）、`NativeWorker.OnClosed`、端口锁（`proxy_port_lock.go`）与 Go 侧 client 缓存上限（`proxy_client_cache.go`），`waa`、`chromeauth`、`camoufoxnative`、`transport_browser.go`、`runtime_go.go` 的网络与指纹逻辑都不变（只是换了上游代理地址）。

**取代理只用 `EffectiveProxy`**：`aistudio.Account.EffectiveProxy(global)` 为 账户固定代理 > IP 池绑定（`ProxyBinding.URL`，为空表示沿用 `global`）> `global`。普通 RPC、WebChannel、上传、续签、公共头发现与登录验证都经它取代理，绑定一变下一次请求就跟着变；公共头提供器在需要发现时按当前 `EffectiveProxy` 取出口，与缓存的 client 不同就换新的。新代码不得绕过它直接读 `Config.Proxy` 或 `PROXY`：绕过的路径会从默认代理、`PROXY` 为空时甚至从服务器真实 IP 出网。

**进程级分配器**：`runtimeManager` 在 `newRuntimeManager` 里创建唯一的 `proxyPool`，经 `newRuntime` 的参数交给每个生成服务实例。端口占用对应进程里活着的浏览器，与生成服务实例、账户目录无关：改 `AISTUDIO_AUTH_STATES` 重建、旧实例关闭失败的浏览器仍占着的端口，新实例都看得到。同机多进程按端口加 flock（`aistudio.TryLockProxyPort`，文件 `proxy-port-<EndpointKey>-<端口>.lock`，与账户 runtime 租约同目录；Linux 上按打开的文件描述互斥），本进程同一端口只持一把锁（持有者从 0 变 1 时加锁、归零时释放），被其他进程锁住的端口 1 分钟后再试。锁出错（打开文件数用满、缓存目录暂时不可写等）时 WARN（一段出错期一次），5 分钟内照常分配、不加锁，到期再试；恢复后第一次加锁成功时给出错期间分出、仍持有的段内端口补锁，补不上的（已被其他进程锁住）记 WARN。默认出口不加锁，但避开有“被其他进程锁住”记录的端口：记录在重试时间之后也不算数，要等本进程真正锁到、或到期后检测一次（`TryLock` 后立即释放，只检测、不持有；本进程当初没锁到、正在共用这个端口时留下这把锁）锁已释放才清掉。

**租约**：每次浏览器类操作申请一个租约（`proxyPool.acquire`），租约记下实际使用的上游 URL 与端口：

| 使用方 | 申请 | 归还 |
| --- | --- | --- |
| Worker | `startReservedWorker` 拿到启动名额之后（`workerProxyLease`；池分配时只改这次启动的局部 `options.Proxy`，`accountWorker.config` 不变） | 启动成功后 `lease.attach(worker)` 登记 `NativeWorker.OnClosed`：`Close` 成功时与本机中转同一时机归还；关闭失败停在 `WorkerClosing` 时保持占用，清理重试成功后归还；`Remove` 后关闭失败的计入 `orphaned`。启动失败时 `launchFailed(err, ctx.Err() != nil)` |
| 新账户浏览器登录 | `CreateAccount` 开头，租约键 `login:<随机串>`（并发的登录各自独占） | `defer`；账户新建成功时 `adopt(账户 ID)`（`addAccount` 的 `onCreated`：`store.Create` 之后、`workers.Add` 与 `AccountPool.Add` 之前）：静态把端口记为它的上次端口与归属，动态把登录用的临时会话号存成它的会话号。邮箱已存在时 `store.Create` 失败、不 adopt，已有账户的端口与会话号不受影响；adopt 之后登录租约在账户名下，归还前启动的 Worker 与它共用端口（登录租约是端口不足时与别人共用的除外：Worker 有空闲端口时另分） |
| 重新登录、验证 | `LoginAccount`、`verifyLogin` 开头；跟随账户绑定所用的出口（`followServingLocked`，同一账户同时只用一个 IP）：静态共用它的端口（不因冷却拒绝），动态沿用它的会话号（换会话之后、Worker 重启之前仍是旧会话），正在服务的 Worker 走直通出口（IP 池开启前启动、或当时没有可用端口）时用同一个上游、不另分端口或会话（`browserProxy` 返回租约的 URL）；没有正在用的出口时按账户自己的端口或会话申请 | `defer` |
| Chrome 导入 | IP 池开启且没有填账户代理时逐个账号调用 `chromeauth.Import`（每次 `AccountIDs` 只放一个），各自一个登录租约 | 导入只在 Go 侧做 OAuth、没有浏览器在用，每个账号导入完就归还（`releaseClaim`，不占着独占端口等整批添加结束），只记下独占过的端口号或临时会话号（`proxyPoolClaim`）；账户新建成功时 `proxyPoolClaim.adopt` 按记下的值登记到账户名下（静态在端口仍属同一代理商、在段内且没被其他键持有时记亲和与归属，动态存成账户的基础会话号） |
| `setup`（独立进程） | 没传 `--proxy` 且 IP 池开启时：静态从随机起点依次 `TryLockProxyPort`，用第一个锁到的端口，全被其他进程锁住时共用随机起点的端口（不加锁，不退回 `PROXY`）；动态用 `n` 开头的临时会话号。一次 setup 只有这一个出口，批量导入 Chrome 时整批共用 | 进程退出；不记上次端口 |

账户有自己的代理时不申请租约。IP 池关闭时申请直通租约（URL 为启动时的 `PROXY`，参数不改，不记日志），off → on 时据此识别仍走 `PROXY` 的 Worker（状态 `unpooled`）。

**绑定经账户池加锁推送**：账户的绑定 = 绑定所用租约的 URL（`Active=true`，`bindingLeaseLocked`）：最新的已启动 Worker 租约（`attach` 之后，或“改为跟随默认代理”补登记的），没有时为最新的活动租约（启动中的 Worker、登录、验证）。先启后停时新浏览器起来之前 Go 侧仍走正在服务的旧浏览器的出口（WAA proof 与 RPC 同一个 IP），启动失败也不来回切换；裸 429 与生成成功同样记在这个租约的端口上。没有活动租约时为按当前配置算出的默认出口（`Active=false`）：静态依次为 归自己的端口（上次端口且最后的使用者是自己、不冷却、没被别人持有）> 账户自己的探测序列上第一个可用端口（可分配且没有“被其他进程锁住”的记录）> 清除（回到 `PROXY`）> 哈希端口（与别人共用）。探测序列（`proxyPoolProbe`）为 `起点 + k × 步长 (mod 端口数)`，起点与步长分别取 `fnv64a(账户 ID)` 的低、高 32 位，步长从与端口数互质的值里取（`configure` 时算好），不重复地走遍全部端口；不同账户的步长不同，一个端口被占时落在它上面的账户各自走到不同的下一个端口（约 账户数 ÷ 空闲端口数 个），不会整批堆到同一个端口上、再在它被占时整批挪走。一批账户（`pushLocked` 至少 16 个、全量重算、重推在等的账户）共用一份端口快照（`proxyPoolDefaultView`，扫描一次端口段），没有可用端口时不逐个探测。后两种情况记进 `defaultWaiting`，端口重新可分配时重推：端口归还后等 30 秒（`proxyPoolWakeDelay`，期间多半有 Worker 来占它，多次归还合并成一次）；冷却到期、其他进程锁的重试时间到时同样重推；由定时器触发，申请、归还与状态接口也顺带检查。重推前先扫描端口段，没有可用端口时它们的结果不变，跳过。动态为账户会话 URL；关闭时清除。写入只有两条路：

1. 装配前直接写对象（`proxyPool.prime`）：`newRuntime` 在 `aistudio.NewAccountPool` 之前、`workers.Add` 在 `AccountPool.Add` 之前，账户池构造时按带绑定的 `EffectiveProxy` 挂出口通道，第一次就挂对。
2. 运行中经 `AccountPool.SetProxyBindings` / `UpdateProxyBindings` 在账户池的 `p.mu` 内按账户 ID 写“当前账户池”里的对象（`attach` 切换推送目标）；写入有变化的账户在同一次加锁内按新的出口键重挂出口通道（`proxyBindingChangedLocked` → `attachEgressLocked`），有账户换了通道时 `notifyLocked`（按出口排队的请求重新选号），绑定变化与重挂是同一个原子步骤，`verifyEgressLocked` 看不到中间态。重建期间旧实例的目录监视只能 `prime` 旧对象、`refresh` 会因 owner 不符被忽略，碰不到新账户池。

锁序为 `account.startupMu` / `accountWorker.mu` / `NativeWorker.operationMu` / `runtimeMu` → `proxyPool.mu` → `AccountPool.mu`：账户池不回调 app 层（`notifyLocked` 只关通道），任何代码都不能在持有 `AccountPool.mu` 时调 IP 池；`proxyPool.mu` 内只做 map 操作、非阻塞的 `TryLock` 与批量推送，日志与 `onPortCooled` 收集后在锁外执行。`ProxyBinding.EgressTag`：会话号写在密码里时为会话号，静态与会话号写在用户名里时为空。`EgressKey` 去掉密码，不加区分的话全部会话会合成一个出口，所以出口键为 `egressKeyOfLocked`：`EgressKey(EffectiveProxy)`，账户没有自己的代理、绑定 URL 非空且带 `EgressTag` 时追加 `"#" + EgressTag`；`attachEgressLocked`、`verifyEgressLocked` 与 `EgressKeyOf` 都按它算，`EgressLabel` 把 `#会话号` 与用户名一起哈希（同一用户名不同会话的标签不同，不显示会话号）。

**静态端口的挑选**（`acquire`）：同键或同一账户名下（含 adopt 过来的登录租约）已有活动租约时共用它的端口（同一账户先启后停、Worker 运行中重新登录或验证、新账户的 Worker 在登录租约归还前启动）；Worker 遇到冷却的端口另选，登录、验证与导入不因冷却拒绝（冷却只拦把端口新分给其他账号）；Worker 也不加入端口不足时与别人共用的租约（加入后整个生命周期都和别的账号共用一个 IP），先按下面的次序另分，确实没有可分配端口时才回到这个共用端口（仍是这个账户的 IP，来源记 `同账号`）；其次是有效的上次端口（亲和，只在端口最后的使用者是自己时有效，账号之间不会来回抢）；再按次序挑可分配端口：从没分给过账号的 > 归属账号已不指向它的 > 别人的归属端口，同档按最久未用（刚归还的端口不马上分给下一个账号）、落在它上面的默认出口最少（占用后要挪走的账户最少；从零预热时各端口都没用过，靠它分散）、端口号。可分配 = 在段内、没有持有者、不冷却、不是外部占用（账户代理或 `PROXY` 经 `HostMatches` 指向它）、没被其他进程锁住。都没有时与持有键最少的段内端口共用（`shared`，WARN 每分钟一行），全部冷却时用恢复最早的。

**健康与冷却**：观察点只有两个，都只计入账户绑定所用的、池分配的段内静态端口租约：生成本身的裸限流 429（`rateLimited`，附件上传的 429 不算）与收到上游首个事件（`succeeded`）。规则 A：端口 10 分钟内来自至少 2 个不同账号的裸 429，且从第一个起没有生成成功（Worker 启动成功不清零它：独占时端口换手一定是新账号先启动成功再发请求）。只计入被拒像是 IP 问题的账号（`strikeLocked`：账户自最近一次生成成功以来的裸 429 记在 `strikes` 里，只在这个端口上被拒、第一次被拒之前坏号记忆还没标它；第一次在另一个出口上被拒时从第一个端口的记录里删掉它），并要求其中至少一个 24 小时内在别的出口上生成成功过（`successes`：`succeeded` 记下成功时绑定所用的段内端口，不经 IP 池端口的出口记为 0；“这次之前已是坏号”由调用方在记坏号记忆之前取 `bad()`，随 `rateLimited` 传入）：裸 429 多半跟着账号走（见 `bad_accounts.go`），额度用完的账号在端口换手后接连被拒不能当成 IP 的问题。`strikes`、`successes` 只在内存（换代理商或子账号时随端口记录清空），`strikes` 在生成成功时清除，端口冷却撤回坏号时保留（撤回后在新端口上又被拒的账号不计入新端口，误判不会沿着换端口传下去）；规则 B：端口连续 2 次 `ErrUpstream` 启动失败、其间没有启动成功（取消与其他原因的失败不计），同时清除指向它的亲和。冷却第 n 档为 `min(15m × 2^(n-1), 2h)`，24 小时没有再冷却则档位清零；冷却不影响已在用它的浏览器，只重推落在它上面的默认出口。整体故障保护：窗口内出错的端口达到 `max(5, 50% × 使用中端口数)` 时不冷却。动态模式没有端口冷却：同一账户 30 分钟内 2 次 `ErrUpstream` 启动失败、距上次换会话至少 10 分钟时代号加一（会话号为 `sha256` 前 12 位或登录时采用的临时会话号，加 `r` 与 base36 代号，最长 15 个字符），裸 429 不换会话。

**持久化**：`runtime/proxy-pool.json`（与 `runtime/requests.db` 同目录，版本 1，文件 0600、目录 0700），记账户上次的端口、端口归属与冷却档位、动态会话号与代号，不含任何地址、主机名与账号密码；变化后 3 秒合并写一次（锁内复制快照、锁外原子写），生成服务关闭与 `runtimeManager.Close` 时 flush。`endpoint_key`（主机与用户名的哈希）与当前配置不同、或运行中改了主机或用户名时丢掉端口的冷却与归属（不动持有记录）；30 天没用过的账户记录在写文件时修剪。文件坏或版本不认识时 WARN 后从空开始。

**热更新**：`applyLiveFields` 每次都调 `proxyPool.configure(cfg.ProxyPool, cfg.Proxy, proxyPoolCapacity(cfg))`（峰值与启动并发变化同样要重新判断端口是否够）；`ProxyPool` 进 `liveFieldsEqual` 与 `sameDataConfig`，不进 `requiresRebuild`，`.env` → `/api/config` → 页面原样提交 → `.env` 逐字节不变、不会出现“需要重启”。`configure` 永不重置持有记录，`release` 按租约自己记的端口归还（不经当前设置），往返切换模式不会把仍在用的端口再分出去。

| 变化 | 运行中的 Worker | 新启动的 Worker | 没有 Worker 的账户（Go 侧） |
| --- | --- | --- | --- |
| off → static/dynamic | 不变（直通租约，仍走 `PROXY`，计入 `unpooled`） | 按 IP 池分配 | 立即切到默认出口 |
| static/dynamic → off | 不变（旧出口，计入 `stale`） | 走 `PROXY` | 立即回到 `PROXY` |
| static ↔ dynamic、改端口段、改地址 | 不变（`stale`；段外端口归还后不再分出） | 按新设置 | 立即切换 |
| 改峰值、Ultra 峰值、`WARM_STARTUP_CONCURRENCY` | 不变 | 不变 | 不变（只重新判断端口是否够，状态切换时记一行日志） |
| 只改账户自己的代理 | 现有流程（先关 Worker，租约随之归还） | 用账户代理，不申请租约 | `refresh` 重算外部占用与默认出口 |

`PROXY` 的修改仍需要重建。`applyLiveFields` 只要发现已保存的配置里有还没应用的、需要重建的改动（`requiresRebuild`：账户目录、`PROXY`、WAA 后端、上游通道），就整体不做热更新：同一次保存既改 `PROXY` 又改 IP 池，或之前保存过需要重建的改动、还没应用时再单独改 IP 池，IP 池都随重建在 `newRuntime` 里生效（设置页状态行为“已保存，待应用”，与此一致）。

**容量**：`proxyPoolCapacity` = Σ各分区（峰值 + `drainLimitFor(峰值, WARM_STARTUP_CONCURRENCY)`），`drainLimitFor` 是从 `drainLimit` 抽出的纯函数（行为不变），不含登录与验证；端口数少于它时 WARN，恢复够用时 INFO。另有两条泄漏报警：已删除账户仍占用（`orphaned` 增加）与池租约数持续 5 分钟大于容量加验证并发上限（不用 `Workers.Occupied` 比较：排空中的 Worker 不计入它，会误报）。

**Go 侧连接复用**：`transport_http.go` 的 `clientForProxy` 按完整 URL 字符串缓存 tls-client，上限 1024、按最近使用淘汰（淘汰时 `CloseIdleConnections`，进行中的请求不受影响）；`configure` 预先算好每个端口的 URL，同一端口、同一会话每次都是同一个字符串。只影响连接复用，不改 TLS 指纹与请求形态。

**管理页**：设置页的 IP 池分组在 `web/src/components/ProxyPoolSettings.vue`。`proxy_pool` 在 `SettingsPanel.vue` 里与 `proxy` 一样属于失焦保存的字段（`BLUR_KEYS`）：自动保存时沿用已保存的值；模式选完、地址或端口失焦时由 `commitProxyPool` 提交，提交前先用 `web/src/proxyPool.ts` 的 `proxyPoolProblem` 校验模式与地址、端口的组合（与服务端 `ProxyPool.Validate` 一致，另粗查“不写协议时必须写端口”），不合法时不提交、不弹提示，错误显示在对应输入框旁；保存后把服务端的规范值同步回表单。前端查不出的地址格式问题（例如冒号写法的主机里写了 `{session}`）由服务端拒绝：`doSave` 收到 400 `invalid_config`、且这次提交的 IP 池设置与已保存的不同时，沿用已保存的 IP 池设置再提交一次，成功说明是 IP 池设置的问题，错误记成 IP 池的服务端错误（显示在地址框旁，提交时沿用已保存的值，IP 池字段一改就清掉），同一次失焦保存的其他字段照常保存；再失败时按普通的保存错误显示。旧版服务端不返回 `proxy_pool` 时不显示分组、不提交。端口需求由 `proxyPoolCapacity` 按表单里的峰值与启动并发现场估算，与服务端 `proxyPoolCapacity` 同一公式。“抽测出口”与 `ProxyInput.vue` 的“测试代理”一样在测试期间发 `testing` 事件、暂停自动应用的倒计时。账户列表不随 Worker 启停推送，代理列的实时端口取状态接口 `proxy_pool.assignments`（按账户 ID 合并，与 `warm_ids` 的做法相同），没有时才用账户 DTO 的 `proxy_pool_slot` 兜底；“固定为默认代理”标记取账户 DTO 的 `proxy_pool_pinned`（`proxyPool.markPinned`，与 `pinned_default`、“改为跟随默认代理”同按当前生效的 `PROXY`），前端不再自己比较字符串。账户编辑对话框的“测试代理”留空时总带 `account_id`：账户原来有自己的代理（对话框里清空了）时服务端测清空后的默认出口（`proxyPool.accountPreview`，只读），文案为“清空账户代理后由 IP 池分配，例如：端口 N”。

**与 egress_guard（429 治理）的关系**（接线见 `internal/app/proxy_pool_egress.go`）：出口统计与限速仍由 egress_guard 负责，出口键为 `egressKeyOfLocked`（账户实际使用的代理的规范化值，会话号在密码里时带 `#会话号`），静态每个端口、动态每个会话是独立出口，`PROXY` 为空时账户不会全部挂在“直连”一个键上。

- 租约跟到实际出口：生成租约的出口通道在取得时定下，而静态 IP 下没有 Worker 的账户挂在默认出口上（探测序列上的空闲端口、端口用满时的 `PROXY`，`PROXY` 为空时与别人共用的哈希端口）。现场提升、补覆盖与转写、Live、Veo、固定账户的冷启动都是先取租约、再启动 Worker，`allocateStaticLocked` 另挑端口、绑定随之重挂。`AccountLease.RebindEgress` 把租约改挂到账户当前的通道（在账户池锁内改写租约的 `egress`，`EgressKey`、`EgressLane` 改为在锁内读）：没发出的预占一起搬过去（旧通道退还预占，消耗过令牌时退还令牌并唤醒等待方；新通道照 `reserveEgressLocked` 预占、限速时消耗一个令牌）。`acquireLease` 在现场提升成功后显式调用，`recordEgressSentLocked` 在每次生成发送前自动调用一次（覆盖转写等经 `Worker()` 冷启动、以及尝试内重开 Worker 的路径），`generateWithRetry` 在 `Generate` 返回后重读出口键；非生成 RPC 的计数（`noteOtherSend`）记在账户当前的通道上。这样尝试结果、`quota.egressKey`、预占与发送计数都记在请求实际发出的端口上，不同账号的冷启动首发不会汇到默认出口（端口快用满时多半是 `PROXY`）上凑成“冷号首发被拒”的判定。

- 冷启动闸门：`egressColdStartGate` 在出口判定成立时先问 `proxyPool.poolsAccount`：IP 池开启（静态有端口段、动态有地址模板）且账户没有自己的代理时直接放行（闸门看的是账户现在的默认出口，真正启动时池会另分端口，每个出口只有这一个账号）。锁序 `rebalanceMu → proxyPool.mu`。同样的理由，剔除受限出口上的备用账户（`acquireLease` 的现场提升、扩容与回收，`withoutLimitedStandby` 的转写、Live、Veo）走 `filterLimitedStandby`：先由账户池 `FilterAccountsByEgress` 剔除，再对剔除的账户问一次 `proxyPool.pooledAccounts`（一次加锁，锁不嵌套），由 IP 池分配出口的留下（端口用满或冷却时它们的默认出口退回 `PROXY`，`PROXY` 判定为限流时不应因此被剔除）。
- 端口冷却撤回误伤：`newRuntime` 装配成功后 `setPortCooledHook(service.proxyPortCooled)`（进程级 IP 池，每次重建换成新生成服务的）。回调在 IP 池锁外调用，可能持有某个账户的 `startupMu`（Worker 启动失败时），所以在后台执行（与进入出口判定时的撤回共用 `relaxWG`）：规则 A 对计入规则 A 的账号（`proxyPoolCooling.RateLimitedAccounts`，见上文“健康与冷却”）`RelaxRateLimitCooldowns(账号, 现在 + egressLimitedCooldown, 1)`（第 1 档起全部撤回、清除档位，原因前缀改为“出口限流撤回”），并 `badAccountMemory.pardon(账号, proxyPoolFailureWindow)` 撤回坏号记忆（清零连续裸限流次数；窗口内标上的坏号清除、照常写盘，更早标上的保留；不记成功）；规则 A 与 B 都对正用这个端口服务的账户（`proxyPoolCooling.Workers`：绑定所用的租约是这个端口上已启动的 Worker）`scheduleWorkerReset(…, workerResetProxyPort)`，等同账号进行中的请求结束后关闭，补齐预热时换到别的端口：先取 Worker 版本、再用 `servesOnPort` 核对账户仍在这个端口上服务（回调在后台，期间可能已换成别的端口上的新 Worker），日志的“重启 Worker”只计真正登记的关闭，ResetAll、Close 期间或独占等待登记失败的计入“未登记”。裸 429 的 `rateLimited` 放在 `applyRateLimitCooldown` 写完逐级冷却之后，端口因这次 429 冷却时撤回的冷却包括刚写的这一次。
- 动态换会话：`applyRateLimitCooldown` 在出口判定成立的分支写完冷却日志后调 `proxyPool.rotateSession(账号, lease.EgressKey(), "出口限流判定成立")`，只在判定的出口就是账户当前会话的出口时换（`sessionEgressKeyLocked`：动态模式、账户没有自己的代理，绑定所用的租约是池按当前设置分配的当前会话，或没有活动租约；出口键按 `aistudio.ProxyBinding.EgressKey` 算，与账户池挂通道的键相同），受 10 分钟最短间隔约束。开启动态 IP 之前启动、仍走 `PROXY` 或旧端口的直通 Worker 所在的共用出口判定成立时不换（换一个没用过的会话只会刷日志、占掉间隔）。每个会话各自是独立出口、只有一个账号，凑不够判定门槛（`egressMinAccounts`），所以这里实际走不到：被上游限制、但代理能连通的会话不会自动换（裸 429 单看一个会话分不清是账号还是 IP 的问题，需要另定触发依据）。
- 出口状态瘦身：账户池侧没有账户、没有预占、窗口内没有发送、不限速、没有登记唤醒的出口记录在取快照时删除（`pruneEgressLocked`），只检查 `egressIdle`（两条通道都没有账户的记录：建立时与账户数减到 0 时记进来，又挂上账户时移出），不扫全部出口；编号对记进 `egressFree` 给新出口复用。动态 IP 下没有 Worker 的账户各自挂在自己的会话上，出口数与账户数相当（上万），全量 `EgressSnapshot` 在账户池锁内要十几毫秒：判定器计时（每 5 秒）只取有判定状态的出口（`EgressSnapshotFor(键)`，O(键数)），状态接口 `adminStatus` 同样只取这些出口的计数、其余出口按 `EgressSnapshotFor` 给的总数计入正常（管理页事件流有请求时最快每 50ms 取一次状态），排队进度只取所排的那一个出口；全量快照只留给 `status()`（测试与诊断）与管理动作按编号找出口。`BenchmarkEgressSnapshot1e4` 对比两者。判定器侧正常、10 分钟历史为空、没有排队与冷启动、没有限速记录的出口状态在计时里删除（`pruneIdleLocked`）。出口多于 16 个时 `egress` 只列出判定成立或已撤销的出口，正常的只计入 `egress_summary`；不多于 16 个时全部列出（全量快照便宜，IP 池关闭时多半只有一个出口）。
- 日志带出口：裸 429 写入的 `账号冷却` 行末尾追加 `proxyPool.slotLabel(账号)`（` | 端口=10023` 或 ` | 会话=…`），`WAA Worker 启动失败` 行末尾追加这次启动租约的 `logField()`（在 `launchFailed` 归还之前取）。

## 4. Go 协议层、公开端点与 Vue 管理端

公开端点统一读取同一份实时模型目录和规范事件：

| 协议 | 端点 |
| --- | --- |
| OpenAI Chat | `GET /v1/models`、`GET /v1/models/{model}`、`POST /v1/chat/completions` |
| OpenAI Responses | `POST /v1/responses` |
| Gemini Interactions | `POST /v1beta/interactions`、`POST /v1/interactions` |
| OpenAI Files | `POST /v1/files`、`GET/DELETE /v1/files/{file}`、`GET /v1/files/{file}/content` |
| OpenAI 媒体 | `POST /v1/images/generations`、`POST /v1/audio/speech`、`POST /v1/videos`、`GET /v1/videos/{id}`、`GET /v1/videos/{id}/content` |
| OpenAI Transcribe | `POST /v1/audio/transcriptions` |
| Anthropic | `POST /v1/messages`、`POST /v1/messages/count_tokens` |
| Gemini | `GET /v1beta/models`、`GET /v1beta/models/{model}`、`POST /v1beta/models/{model}:generateContent`、`:streamGenerateContent`、`:countTokens`、`:predictLongRunning`、`GET /v1beta/operations/{id}` |
| Realtime | `GET /v1/live`、`GET /v1/robotics/stream` |

管理端路由：

| 能力 | 路由 |
| --- | --- |
| 健康与状态 | `GET /health`、`GET /api/status` |
| 模型与账户 | `GET /api/models`、`GET/POST /api/accounts`、`GET/POST /api/accounts/import/chrome`、`POST /api/accounts/bulk-enable`、`PUT/DELETE /api/accounts/{id}` |
| 登录与验证 | `POST /api/accounts/{id}/login`、`POST /api/accounts/{id}/verify` |
| 生成服务 | `POST /api/control/start`、`POST /api/control/stop` |
| 配置 | `GET /api/config`、`PUT /api/config` |
| 冷却与请求 | `GET /api/cooldowns`、`GET /api/requests`、`POST /api/requests/{id}/cancel` |
| 403 无权限账户 | `GET /api/accounts/permission-denied`、`POST /api/accounts/permission-denied/clear` |
| 日志与事件 | `DELETE /api/logs`、`GET /api/events` |

`/api` 接受 loopback 请求，并在请求带 `Origin` 时执行 same-origin 校验。`/v1` 与 `/v1beta` 使用公开 API key 与 CORS。`authMiddleware` 经 `requestAPIKeyMatches` 逐个比较 `?key=`、`X-Goog-API-Key`、`X-API-Key`、`Api-Key` 与 `Authorization: Bearer`（`bearerToken`），每个来源都按常量时间比较，任一相同即放行并标记访问日志已通过校验；不再只取优先级最高的一个来源。

`/ultra/v1/...` 与 `/ultra/v1beta/...` 去掉前缀后交给与主路由完全相同的处理链（API key、CORS、来源检查、请求体上限、请求日志、排查记录与正文记录，见 `internal/api/ultra.go`），并把请求标记为 Ultra 号池；其余 `/ultra/*` 路径返回 404。`/trace/ultra/v1/...` 由 `traceEntry` 同时去掉两个前缀并标记 Ultra 号池，日志与排查记录的路径保留 `/trace/ultra` 前缀；`/ultra/trace/...` 仍返回 404。普通路径由 `poolScopeMiddleware` 按 `ULTRA_EXCLUSIVE` 标记为普通号池或不限号池。别名解析的模型目录快照按号池分别缓存（进程级，30 秒过期；过期后仍先返回旧快照，同时在后台刷新，刷新只由带后缀的请求触发，见 `aliasCatalog`）：`ULTRA_INCLUDE_PRO` 切换后号池内容变了，但快照不随开关作废，带后缀的模型名（如 `gemini-4-argon-low`）要等该号池的快照过期、并由过期后的第一个带后缀请求触发的后台刷新完成，才按新目录解析。在此之前的请求（快照过期前的请求、触发刷新的那个请求，以及刷新完成前同时到达的请求）仍按切换前的目录解析，旧目录里没有的模型解析不到，原名转发后可能因找不到模型而失败。带后缀的请求稀少时，切换时快照往往已经过期，切换后第一个这样的请求无论离切换多久都按旧目录解析，所以 30 秒的过期时间并不是旧目录的使用时限。不带后缀的模型名原样转发、不受影响；这与账户目录变化后的行为相同，不为开关另设缓存代数；请求日志、活动请求与用量账本带 `pool`（`/ultra` 请求为 `ultra`），日志路径保留 `/ultra` 前缀。

OpenAI Responses 的 `previous_response_id` 与 Gemini Interactions 的 `previous_interaction_id` 共用当前进程内最多 256 个响应节点，用于重建下一轮完整 contents（Interactions 不保存音频输出）；进程重启后客户端应重新提交完整上下文。Drive 文件、Veo operation 和产物文件的账户绑定写入 `runtime-state.json`，重启后仍可轮询和下载。

新增上游能力从 `internal/aistudio` 开始：编码真实数组槽位、解码服务器事件，再由 `internal/api` 投影到公开协议。模型方法、上下文、输出上限、工具、声音、图片规格和视频规格均来自实时 `ListModels`。

函数参数 Schema 的 `required` 补齐（`defineRequiredProperties`，`internal/aistudio/schema.go`）有三个调用点：Playground 的 `encodeFunctionDeclarationHint`（`tools.go`）、Build 的 `parametersJsonSchema`（`build.go`）与 `strict` 核对的 `compileToolSchema`（`tool_contract.go`，在其大小检查之后）；改动补齐规则时三处要一起测，保证发给上游的 Schema 与核对用的 Schema 一致。整数字段规范化（`canonicalSchemaIntegers`）与 `TYPE_UNSPECIFIED` 处理只在 Playground 的 `encodeSchemaNode` 中进行，Build 与 `strict` 核对不做这两项（改动会改变发给 Google 的 Build 请求）。上游结束说明解码为 `Event.FinishMessage`（Playground 读 candidate `[3]`，Build 读 `finishMessage`），由 `internal/app` 的 `forwardEvents` 写进请求进度、由 `internal/api` 的 `setGeminiFinish` 写进 Gemini `finishMessage`；拦截说明进 `PromptFeedbackError.Message`，公开错误文案（`official_errors.go`）只按 `Reason` 生成。

流式心跳由各协议的写入器提供给 `consumeStreamEvents`：OpenAI Chat、Responses 与 Interactions 用 `writeSSEHeartbeat`（`: ping`），Anthropic 用 `anthropicStreamWriter.heartbeat`（`event: ping`），Gemini 用 `geminiChunkWriter.heartbeat`（SSE 与 JSON 数组都写一个空行）。新增流式协议时按该协议官方 SDK 能跳过的格式选择心跳。

`runtimeManager` 同时实现基础 `Service` 以及 Video、File、Bidi 和 Transcription 扩展接口。`internal/app` 负责资源账户绑定、跨账户文件复制、运行状态和重试流程；`internal/api` 负责 HTTP、SSE 与 WebSocket DTO，即公开协议使用的请求、响应和事件对象。完整请求字段、响应 DTO、SSE 和 WebSocket 事件见 [protocol.md](protocol.md)。

前端开发命令：

```powershell
cd web
npm run dev
npm run typecheck
npm run lint
npm run format:check
npm run build
```

Vite 将生产产物写入 `internal/webui/dist`。管理端通过本机 `/api` 路由管理生成服务、日志、账户、配置、模型冷却、活动请求和 SSE 状态事件；认证状态与 WAA 对象不进入浏览器存储。

`internal/webui/embed.go` 使用 `//go:embed dist`，因此 Go 构建前必须生成当前前端产物。管理端从 `/api/events` 接收 `status`、`models`、`accounts`、`log`、`cooldowns` 和 `request` 事件。

用量账本（`internal/requestdb`）的原始记录与小时、本地日汇总都有 `pool` 列（`/ultra` 请求为 `ultra`，其余为空），汇总主键包含 `pool`。旧版 `runtime/requests.db` 每次打开时由 `migrate` 检查并升级，可重复执行：原始记录缺少 `pool` 时 `ALTER TABLE ... ADD COLUMN` 并建立 `requests_pool_time` 索引，汇总表缺少 `pool` 时在一个事务内按新主键重建并复制旧汇总，旧数据归入普通号池。用量接口的 `pool` 维度对外取值为 `normal`、`ultra`，可筛选、分组与堆叠；协议归类先去掉 `/trace` 与 `/ultra` 前缀。

用量页（`UsagePanel.vue` 与 `components/usage/`）按需加载：ECharts 只注册用到的折线图、柱状图与组件，单独构建为 `echarts-*.js`，与用量页代码一起在第一次打开用量页时下载，不进入主包。用量页通过 `/api/usage`、`/api/usage/records`、`/api/usage/records.csv` 与 `/api/requests/{id}/body` 读取账本，与其他管理接口一样依靠管理令牌 Cookie；账本未启用时这些接口返回 404，页面显示账本未启用。

API 试用通过 `eventsource-parser` 读取 SSE，以各协议完成事件结束请求；流内错误与提前断流显示为失败。响应头、正文和心跳的刷新错误沿 HTTP 写入路径返回，事件转发在取消时释放账户租约。试用页同一时刻执行一个请求，停止后可继续提交。

## 5. 协议实现

新增上游能力按以下顺序实现：

1. 在 `internal/aistudio` 定义规范请求、响应类型与模型能力
2. 编码 [protocol.md](protocol.md) 中对应的 JSON+protobuf 数组
3. 将网络增量解码为规范事件
4. 由 `internal/api` 分别投影 OpenAI、Responses、Anthropic 和 Gemini 协议
5. 将结构化状态与操作入口绑定到管理端

`internal/api` 消费规范请求与事件，`internal/aistudio` 管理账户文件、WAA runtime、原始上游数组和实时模型能力。资源型操作在创建时记录账户 ID，后续轮询、下载与提示引用使用该账户。

已识别字段校验类型和 oneof 约束。未知非空槽保留为 provider 事件或原始扩展字段；无法解释的已消费字段、缺失完成帧和无效媒体内容返回结构化协议错误。

## 6. 构建与贡献

发布二进制前先构建前端：

```powershell
cd web
npm ci
npm run build
cd ..
go build -trimpath -o aistudio2api.exe ./cmd/aistudio2api
```

提交前执行与修改相关的功能验收、前端检查及 Go 静态检查：

```powershell
go vet ./...
```

Windows 发布包包含 `aistudio2api.exe` 与 `start.bat`；其他平台使用同一 Go 程序。Camoufox 在首次启动时自动准备；`WAA_BACKEND=go` 时在首次浏览器登录时准备。贡献内容聚焦单一功能或协议变更，并使用脱敏后的请求与响应样例。

GitHub Actions 对 `main` 提交和 Pull Request 执行前端 lint、类型检查、构建，以及 `go.mod` 最低版本的 Go 检查。发布包使用当前稳定版 Go 构建，覆盖 Windows amd64、Linux amd64/arm64、macOS amd64/arm64。推送 `v*` 版本标签后自动创建 Release，附上二进制、启动文件、示例配置和文档；含 `-` 的标签发布为预发布版本。也可以在 Actions 页面手动运行 Build And Release 并填写版本号（如 `v1.0.0`），工作流先在 `main` 当前提交上创建该标签再构建发布；只能从 `main` 发布，标签已指向其他提交时拒绝。普通构建产物在 Actions 中保留七天，Release 附件长期保留。

源码提交包含协议实现、前端源码和公开文档。本机账户状态、Cookie、token、proof、提示正文、响应正文和运行产物留在本机。
