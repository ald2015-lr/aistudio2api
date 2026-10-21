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
| `--proxy <URL>` | 固定到账户初始化、WAA 与业务请求 |
| `--locale <LOCALE>` | 设置账户语言 |
| `--timezone <IANA_ZONE>` | 设置账户时区 |

`--storage-state`、`--login` 与 Chrome 导入参数分别构成文件导入、隔离登录和浏览器导入模式。隔离登录使用 `--login`。`setup` 要求 `AISTUDIO_AUTH_STATES` 指向一个账户目录。

`--proxy` 会同时固定到新账户的初始化、WAA 与业务请求，接受无认证信息的 HTTP、HTTPS 或 SOCKS5 URL。`--locale` 和 `--timezone` 设置账户环境；Chrome 导入未显式指定语言时读取 Profile 的首选语言。Camoufox 按以下顺序定位：进程环境变量 `CAMOUFOX_PATH`、`runtime/camoufox/`、可执行文件旁的同名目录、Windows 本机 Camoufox 缓存。全部不存在时自动下载当前平台的固定版本。

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
| `--proxy <URL>` | 覆盖每次新建生成服务实例使用的全局代理 | `PROXY` |
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
internal/config/         全局配置的读取、校验和原子写回
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
| `PROXY` | setup 与未设置账户代理时使用的固定出口 | 空 |
| `INIT_TIMEOUT` | 单账户初始化超时 | `2m` |
| `REQUEST_TIMEOUT` | 单次请求最大执行时间 | `5m` |
| `FIRST_EVENT_TIMEOUT` | 每次尝试从向上游发送起（WAA proof 之后）等待首个上游事件的上限：超时只取消这一次尝试的上下文、读完其事件流后释放租约，按可重试的上游超时换号（不额外冷却账号），不能再换号时返回 504；`0` 关闭，开启时必须小于 `REQUEST_TIMEOUT`，可热更新 | `0` |
| `WARM_WORKER_LIMIT` | 普通分区（不属于 Ultra 号池的账户）的常驻预热账户数，可热更新 | `5` |
| `MAX_ACTIVE_WORKERS` | 普通分区的活动 Worker 容量上限，必须不小于热池目标，可热更新；浏览器总数最多为两个分区上限之和 | `10` |
| `ULTRA_EXCLUSIVE` | `true` 时 Ultra 号池的账户只服务 `/ultra` 前缀的请求，普通路径只用其余账户；`false` 时普通路径也可以用 Ultra 号池的账户，`/ultra` 仍只用 Ultra 号池的账户；保存后立即生效，手动改 `.env` 后停止再启动生成服务时按重新读取的值生效（进程环境变量优先） | `true` |
| `ULTRA_INCLUDE_PRO` | `true` 时权益为 Pro 的账户也属于 Ultra 号池与 Ultra 分区（`AccountPool.SetUltraIncludePro`），按全部 `ULTRA_*` 设置调度；账户权益本身不变（权益请求头、模型资格、输入上限与更高权益重试仍按 Pro），见下文“号池成员规则”一段。保存后立即热更新（`applyLiveConfig`）；同一次保存里还有需要重建生成服务的字段时随重建生效（`newRuntime` 经 `configurePool` 设置）；进程环境变量优先 | `false` |
| `ULTRA_WARM_WORKER_LIMIT` | Ultra 分区（Ultra 号池的账户）的常驻预热账户数，`0` 表示只按需启动，可热更新 | `2` |
| `ULTRA_MAX_ACTIVE_WORKERS` | Ultra 分区的活动 Worker 容量上限，至少为 1 且不小于 `ULTRA_WARM_WORKER_LIMIT`，可热更新 | `5` |
| `WARM_STARTUP_CONCURRENCY` | 同时冷启动的 Camoufox Worker 数：预热最多占用该数，按需冷启动另保留 1 个名额并优先，可热更新；`WAA_BACKEND=go` 不受此限 | `2` |
| `PER_ACCOUNT_CONCURRENCY` | 单账号同时执行的请求数 | `2` |
| `ROUTING_STRATEGY` | 账户轮询 `round-robin` 或粘性优先 `fill-first` | `round-robin` |
| `UPSTREAM_CHANNELS` | 生成请求的上游通道 `playground`、`build`，逗号分隔 | `playground,build` |
| `STREAM_PLAYGROUND_MODELS` | 流式请求优先走 Playground 的模型 ID，逗号分隔：列表中的模型的流式请求每次尝试先不等待地取 Playground 账号，没有立即可用的就按默认条件在全部启用通道中选号；为空时两个通道照常轮询，可热更新，规则见 [Build 通道](build.md) | 空 |
| `PRIMARY_MODELS` | 普通号池的主用模型 ID，逗号分隔，只作用于普通路径（`PoolScopeNormal`；`ULTRA_EXCLUSIVE=false` 时为 `PoolScopeAll`，先于 Ultra 列表查找）：列表中的模型的生成请求最多排队 `PRIMARY_QUEUE_WAIT`，超过即返回 `CapacityBusyError`（429 + `Retry-After`）；为空时所有模型照常排队，可热更新 | 空 |
| `PRIMARY_QUEUE_WAIT` | 主用模型的排队上限，整个请求共用一份、按墙钟计：截止时间为第一次选号前创建预算的时刻加上限，失败的尝试与换号退避都计入，请求自己现场冷启动的浏览器启动时长（最多 `INIT_TIMEOUT`）与候选全部冷却的等待延后截止时间；`0` 表示不排队，不能超过 `REQUEST_TIMEOUT`（未配置时取 `10s` 与 `REQUEST_TIMEOUT` 中较小者），可热更新 | `10s` |
| `ULTRA_PRIMARY_MODELS` | Ultra 号池的主用模型 ID，逗号分隔：作用于 `/ultra` 请求（`PoolScopeUltra`，只查这份列表），以及 `ULTRA_EXCLUSIVE=false` 时普通路径上不在 `PRIMARY_MODELS` 里的这些模型；排队规则与 `PRIMARY_MODELS` 相同，另外没有额度时立即返回 429（候选全部冷却立即返回 `AllCoolingError`，额度 429 后最多再换 2 次账号，额度失败后紧接着的那次选号只用已启动的 Worker，结束时返回 `PrimaryQuotaError`，见下文“主用模型按号池”一段）。键存在时按写的值（空值表示关闭），键不存在时取默认；进程环境变量的空值与其他配置项一样被忽略，关闭只能在 `.env` 写空值或在管理页清空，可热更新 | `gemini-4-argon` |
| `ULTRA_PRIMARY_QUEUE_WAIT` | Ultra 主用模型的排队上限，规则与 `PRIMARY_QUEUE_WAIT` 相同（整个请求共用一份、按墙钟计；`0` 表示不排队，不能超过 `REQUEST_TIMEOUT`，未配置时取 `10s` 与 `REQUEST_TIMEOUT` 中较小者，保存时写具体值），可热更新 | `10s` |
| `WAA_BACKEND` | WAA 后端 `camoufox` 或 `go` | `camoufox` |
| `TEMPORARY_CHAT` | WAA 预热页是否使用临时对话 | `false` |
| `AUTO_START` | 管理进程启动后自动启动生成服务。生成服务期望运行却停在 STOPPED（启动失败或意外停止）时由监督协程自动重新启动：一般失败按 5 秒起、上限 1 分钟退避，没有可用账户时上限 5 分钟；手动停止后不再自动启动 | `true` |
| `REQUEST_BODY_LOG` | 用量账本同时保存通过 API key 校验的 POST 请求与响应正文，各截断到 64 KiB，只保留最近 1000 条；管理页面保存配置时沿用现值，修改后重启程序生效 | `false` |
| `ADMIN_PASSWORD` | 远程管理密码；非空时非回环请求经 HTTP Basic 认证后可访问管理页面与 `/api/`，同一 IP 10 分钟内错 10 次封禁 15 分钟；管理进程重启后生效 | 空 |

`LISTEN_ADDR` 使用 `host:port`，端口范围为 `1..65535`。时长和容量字段必须为正值（`FIRST_EVENT_TIMEOUT`、`PRIMARY_QUEUE_WAIT`、`ULTRA_PRIMARY_QUEUE_WAIT` 与 `ULTRA_WARM_WORKER_LIMIT` 可以为 `0`，两个主用模型排队上限都不超过 `REQUEST_TIMEOUT`），`WARM_STARTUP_CONCURRENCY` 的有效范围为 `1..WARM_WORKER_LIMIT`，`ULTRA_MAX_ACTIVE_WORKERS` 不小于 `ULTRA_WARM_WORKER_LIMIT`。管理页面保存时没有带四个 Ultra 字段、`stream_playground_models`、`primary_models`、`primary_queue_wait`、`ultra_primary_models` 或 `ultra_primary_queue_wait`（旧版页面）则沿用 `.env` 中的现值（省略排队上限而把 `request_timeout` 调到现值以下时按 `request_timeout` 封顶）；`stream_playground_models`、`primary_models` 与 `ultra_primary_models` 为空数组表示清空，排队上限为 `0s` 表示不排队。保存的配置 JSON 缺少 `ultra_primary_models`（旧版数据）时取默认列表，显式的空数组为关闭；缺少 `ultra_include_pro` 时按关闭处理。全局代理 URL 使用 `http`、`https` 或 `socks5` 纯 origin 形状。命令行 `--auth` 与 `--proxy` 会覆盖每次启动生成服务时读取的保存值。

`GET /api/config` 与 `PUT /api/config` 同时暴露保存值和当前生效值：

| 字段 | 语义 |
| --- | --- |
| `auth_states`、`proxy` | 下一次启动生成服务时使用的保存值 |
| `init_timeout`、`request_timeout` | 保存后立即热更新（`applyLiveSettings`、`setRequestTimeout`）：之后开始的 Worker 启动与之后进入的请求按新值 |
| `first_event_timeout` | 每次尝试的首事件超时，保存后立即生效；`0s` 为关闭，提交时省略则沿用现值 |
| `stream_playground_models` | 流式请求优先走 Playground 的模型（字符串数组，读取时总是返回），保存后立即生效；提交时省略则沿用现值 |
| `primary_models`、`primary_queue_wait` | 普通号池的主用模型（字符串数组，读取时总是返回）与它们的排队上限（时长文本，`0s` 为不排队，不超过 `request_timeout`），保存后立即生效；提交时省略则沿用现值（排队上限超过新的 `request_timeout` 时按它封顶） |
| `ultra_primary_models`、`ultra_primary_queue_wait` | Ultra 主用模型（字符串数组，读取时总是返回，关闭时为空数组）与它们的排队上限，规则同上，保存后立即生效（不需要重启生成服务）；提交时省略（`null` 或空串，旧版页面）则沿用现值 |
| `warm_worker_limit`、`max_active_workers`、`warm_startup_concurrency`、`per_account_concurrency` | 容量参数，保存后立即热更新（`applyLiveSettings`、`SetPerAccountConcurrency`）：调小的常驻数由空闲回收逐步收敛，冷启动名额按新的启动预热并发放行排队的启动 |
| `ultra_exclusive`、`ultra_include_pro`、`ultra_warm_worker_limit`、`ultra_max_active_workers` | Ultra 号池设置（读取时总是返回），保存后立即热更新到当前生成服务；提交时省略（旧版页面）则沿用现值。只改这些字段不需要重启生成服务：`sameDataConfig` 与 `liveFieldsEqual` 都比较这四个字段，`requiresRebuild` 不含它们 |
| `temporary_chat`、`waa_backend`、`upstream_channels` | 下一次启动生成服务时使用的 WAA 与上游通道配置 |
| `listen_addr`、`proxy_api_key` | 保存的管理监听配置 |
| `active_listen_addr`、`active_proxy_api_key` | 当前管理进程固定使用的值 |
| `management_restart_required` | 保存的监听地址或 API key 与当前管理进程不同 |
| `service_restart_required` | 保存的生成服务配置与当前生成服务实例不同 |

配置保存使用临时文件、`Sync` 和原子替换。监听地址与本地 API key 由管理进程持有，进程重启后应用；可热更新的字段（`liveFieldsEqual` 比较的字段）保存后由 `applyLiveConfig` 立即应用到当前生成服务；账户目录、默认代理、WAA 后端与上游通道（`requiresRebuild`）在停止并再次启动生成服务后应用。

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
  -> 重新读取 .env 并应用命令行 --auth/--proxy 覆盖
  -> 创建并启用新的生成服务实例
  -> 从当前 generation 的 CachedModels 建立内存目录
  -> 并发刷新全部 enabled ready/busy 账户
  -> 冷 generation 等待首个非空真实目录
  -> 启动首个 WAA Worker并进入 RUNNING；首轮目录同步完成前没有可预热账户时，同步完成后再预热一次
  -> 剩余目录同步与热池预热继续在后台运行
```

配置读取、校验、生成服务实例创建失败或启用前取消时，当前已停止实例保持不变。切换到新实例后启动失败时，该实例进入 `STOPPED`，管理端返回结构化错误。模型目录保存在当前生成服务实例的内存中；正常 Stop/Start 创建的新实例从空 `CachedModels` 冷启动。当前实例已有真实缓存时，`trackedService.Start` 可直接使用该缓存预热 Worker，同时继续刷新全部账户。

Stop/Start 是整套重建：停止走 `finishStop` → `ResetAll` 关闭全部 Worker，`startService` 在 STOPPED 时经 `factory` 新建运行时（`newRuntime` 重新载入全部账户），启动时为全部 enabled ready/busy 账户重新同步模型目录（`modelCatalogRefreshConcurrency` = 32）。只在内存里的状态全部从头开始：`auth_required`（`runtime-state.json` 没有认证字段，载入时按 Cookie 推导）、`warmFailures`、启动失败暂停、runtime 占用退避与 `lastStartupSuccess`；`newRuntime` 里的 `reconcileStartupFailures` 按 `.startup-failures.json` 把反复确认登录失效的账户重新标为 `auth_required`（见下文“启动失败记忆”）。Worker 数、启动并发、单账户并发与超时都由 `applyLiveFields` 热更新，不需要重建。

模型目录刷新与当前生成服务共用 context（Go 取消信号）。启动失败、取消或 Stop 后等待刷新退出的上界为 2 秒；未完成的 lifecycle transition（启动或停止操作）最多等待 12 秒。超时与 Worker 清理错误通过 `errors.Join` 保留在同一错误链中。

每个账户目录包含：

| 文件 | 内容 | 生命周期 |
| --- | --- | --- |
| `account.json` | label、enabled、proxy、locale、timezone | 创建或编辑账户时写入；缺少 `enabled` 字段按启用读取。账户默认启用：启动与目录扫描导入时，`enabled` 为 `false` 但 `.disable-reasons.json` 中没有手动停用记录的账户在加入账户池之前改为启用并原子写回（16 个并行，只写一条汇总日志）；只有管理页或管理接口停用的保持停用。手动停用记录立即写入，目录移出账户池时保留；`.disable-reasons.json` 无法读取时没有记录的停用账户保持停用并补记为手动停用 |
| `storage-state.json` | Cookie、localStorage 与可选 Chrome OAuth/DBSC 续签材料 | 合并 `Set-Cookie` 或认证续签后原子写回 |
| `camoufox-fingerprint.json` | 账户固定的浏览器指纹、语言与时区 | 首次运行生成，空值、窗口几何、字体、语音与媒体设备按 Camoufox 官方启动器规则规范化；重新登录和 WAA runtime 继续复用 |
| `runtime-state.json` | 权益等级、模型资格、冷却与资源账户绑定 | 权益同步、首次模型结果或资源变化后原子写回 |
| `camoufox-cache/` | 该账户 Camoufox 的 HTTP 磁盘缓存，上限 256 MB | WAA runtime 运行期间独占写入，随账户目录删除 |

`runtime-state.json` 的 `model_access` value 为 `{state,checked_at,reason?}`，成功状态为 `verified`；`cooldowns` value 为 `{until,reason?}`；`resources` value 保存 kind、name、mime、size、purpose、created_at 与可选 video 元数据。Drive file、Veo operation、视频产物和 Bidi 恢复 token 均保持创建账户粘性。Veo operation 额外保存公开 video object 的 model、seconds、size 与 UTC 创建时间，生成服务或进程重启后的轮询继续投影相同字段。

活动请求锁保护跨进程账户租约，短事务锁保护 `runtime-state.json` 合并写回。短事务锁位于 `auth/.leases/<账户>.runtime.lock`，以 25ms 间隔等待，最多 2 秒；取得锁后重读磁盘状态，只修改目标字段，原子替换文件，再同步内存与资源索引。接收 context 的资源事务会在取消或 deadline 到达时提前返回。

账户更新以 `account.json` 原子写入为持久提交点。`internal/app` 先准备 `pending`（尚未提交）的固定出口，关闭旧 Worker并锁定该账户的 Worker 配置，再调用 `AccountLease.SaveConfig`；保存成功后依次提交 Worker 配置与固定出口。准备、关闭或保存失败时丢弃 pending 更新；保存后的租约释放错误原样返回，已发布配置继续生效。

账户调度先按每个账户实时 `ListModels` 返回的模型和方法筛选，再选择已经就绪且有并发槽位的 Worker。`ROUTING_STRATEGY=round-robin` 在每个模型的候选账户间轮询，按上次选中的账户 ID 继续；`fill-first` 持续使用 ID 排序后的首个可用账户，并在并发槽位用满、冷却或不可用时切换。生成请求的候选为账户与 `UPSTREAM_CHANNELS` 所启用通道的组合：Playground 按 `ListModels` 与权益判断，Build 按 Build 代理返回的 Gemini API 模型目录与相同权益判断；轮询与粘性在组合间按账户 ID 与通道顺序推进，冷却按通道记录（Build 为 `build:<模型>`），一个通道冷却后同一账户可由另一通道继续，全部启用通道都冷却时按下文的冷却规则排队或返回 429，通道规格见 [Build 通道](build.md)。`STREAM_PLAYGROUND_MODELS` 中的模型的流式请求每次尝试先用 `acquireWarmLeaseNow` 不等待地取 Playground 账号（不加入调度等待队列，只在 Worker 分区有空槽、冷启动名额空闲时现场启动），没有立即可用的账号时返回 `noImmediateLeaseError`，随即按默认条件选号：选择条件带 `PreferPlayground`，只调整候选顺序（每个账户先 Playground，轮询只在账户之间推进），不限定通道；不等待阶段现场启动失败的账户经 `workerStartupFailures` 交给这一步，不再启动。主用模型（按请求的号池查 `PRIMARY_MODELS` 或 `ULTRA_PRIMARY_MODELS`，见下文“主用模型按号池”一段）的生成请求带一份排队预算（`leaseWaitBudget`，`generateWithRetry` 在第一次选号前创建，换号重试共用，经 context 交给 `acquireLease`，签名不变）：截止时间按墙钟计，为创建时刻加 `PRIMARY_QUEUE_WAIT`，选号的分类与复查、等待冷启动名额、失败的上游尝试（403、额度 429）与换号退避都计入；请求自己现场冷启动的浏览器启动本身（`startReservedWorker` 实测，合计不超过 `INIT_TIMEOUT`）与候选全部冷却的等待延后截止时间。截止时间已过后账号忙碌、没有空闲槽位、Worker 启动或扩容中这些容量原因的等待立即返回 `CapacityBusyError`（对外 429 与 `Retry-After`，建议间隔为排队上限限定在 1 到 30 秒，上限为 0 时 5 秒），失败后的下一次尝试只接受立即可用的账号（截止时间已过时也不再退避）；`generateWithRetry` 立即结束请求并在进度里写“主用模型排队超过 Xs，立即返回 429”（带已排队、总耗时与第几次尝试），不换号、不冷却、不退避；等待冷启动名额最多等到截止时间（名额被占满而截止时间已过时按容量已满返回，不在名额队列里等前面的浏览器启动完成）；预算不用 context 超时，不会取消进行中的启动；候选全部冷却的等待不受限，仍按下文的冷却规则（Ultra 主用模型除外：立即返回 429）。流式请求写出响应头前对主用模型多等排队上限加 5 秒的首事件（`StreamStartWait(ctx, model)`，按 context 中的号池取对应列表的上限），换号重试不再得到新预算，号池繁忙的 429 因此以 HTTP 状态返回；请求自己现场冷启动或候选冷却延后了截止时间时仍可能晚于这个等待，此时响应头已按 200 写出，错误在流内返回（Ultra 主用模型的请求自己现场冷启动时通常不会，见下文 `StreamStartHold`）。号池的同时生成数约为 `MAX_ACTIVE_WORKERS × PER_ACCOUNT_CONCURRENCY`，快速失败只是对症状的处理。并发槽位、Worker 与 WAA 按账户共享；计数、Live、Veo、转录与 Drive 文件引用使用 Playground RPC。每个账号最多同时租用 `PER_ACCOUNT_CONCURRENCY` 个请求槽位；首个请求获取跨进程文件锁，最后一个请求释放。WAA proof 由账号 worker 串行生成，`GenerateContent` 由同一 Camoufox 页面并发发送并流式读取（`WAA_BACKEND=go` 时由账户 runtime 的 Go HTTP 发送）；请求前使用 Worker 当前 Cookie 生成 Authorization，响应头到达后把 Worker Cookie 原子同步到账户持久状态。其他 MakerSuite HTTP 响应的 Cookie 在响应头到达时与最新账户状态合并。未固定账户的请求遇到可重试的 401、403、404、429、5xx 或单账户初始化超时时，可以在首个上游语义事件前继续切换尚未尝试的同能力账户（`generateWithRetry` 以排除列表 `ExcludedAccountIDs` 排除已尝试的账户，选号留在候选索引的快路径上，换号重试的请求与同模型的请求同队排队；队列按请求的到达序号排列，`generateWithRetry` 在第一次选号前取一个序号，换号重试排回请求到达时的位置，见 `dispatchQueue.joinAt`；带排除列表的队首被唤醒却仍要等待时、以及现场冷启动备用账号之前，把唤醒交给下一个请求，冷启动前并离开队列，见 `dispatchQueue.passOn`；转录与 Live 的候选列表整个请求不变，同样用排除列表排除已尝试的账户）；没有文件绑定的生成请求遇到单个模型的 403 无权限时，对该账号该通道的这个模型写入冷却（`forbidden_guard.go` 按账号与冷却作用域记档，两个通道分别计：第一次 30 分钟，24 小时内再次 6 小时，第三次起到太平洋时间次日零点、至少 6 小时；同一通道本档冷却未到期时在途请求的 403 不升档；该通道上该模型成功一次即清零），换号不退避，但仍计入连续失败上限（`generateFailureLimit`），同一账号另一通道可用时再试一次，所有启用通道都冷却时经 `retireCooledWorker` 登记淘汰驻留 Worker（只在其他模型上冷却的忙碌 Worker 不排空，空闲后淘汰）；同一号池 10 分钟内至少 5 个不同账号对同一模型返回 403、这段时间该模型又没有任何成功时，按号池级 403（proof、请求形态或 WAA binding 失效等）处理：不写冷却、不升档、不登记淘汰，按一般失败计入连续失败并退避（`observeModelDenied`）；同一账号 10 分钟内对 2 个以上模型连续 3 次 403 时另外暂停整个账号 30 分钟；Drive 引用随需要临时复制到生成账户，显式账户和 Veo operation 保持账户绑定。Chrome 导入状态保留续签材料，HTTP `401`、Code 16 或启动时跳转登录页时在同一固定出口续签一次、重建该账户 WAA runtime 并重放请求；无法续签的账户标为 `auth_required`。Worker 失败与协议 Code 5 要关闭当前 Worker 版本，关闭交给后台（`internal/app/worker_reset.go`）：`scheduleWorkerReset` 按（账户，Worker 版本）登记，同一版本只关闭一次，并发失败的请求与失败 Worker 巡检（`sweepFailedWorkers`，只登记不等待）共用；请求的租约经 `AccountLease.ReleaseToExclusiveReservation` 在账户池的同一次加锁内释放并登记独占等待（`AccountReservation`），排队的请求不会在两者之间取得这个账户，登记保持到关闭结束，期间账户不发出新租约；后台最多等 `REQUEST_TIMEOUT` + 30 秒，请求取消后照常继续，`ResetAll` 与 `Close` 取消并等后台 goroutine 退出。`generateWithRetry` 中不固定账户、还能换号的请求只等 `defaultWorkerResetWait`（2 秒；主用模型最多 1 秒且不超过剩余排队预算的四分之一），等不到就把原账户重新记为已尝试并换号（计入连续失败上限、不退避），换号返回 `ErrNoEligibleAccount`、全部冷却或没有立即可用账号时回到原账户一直等关闭结束后重放；指定账户、文件绑定的请求以及 Veo、Live、转录的恢复照旧等关闭结束后在原账户重放；失败时请求已取消或不再重试只登记、不等待。解释器更新后整个号池的驻留 Worker 都可能返回 Code 5，换号会落到另一个同样过时的 Worker，这是缩短等待的代价。

主用模型按号池（`internal/app/primary_models.go`）：`primaryModelSettings` 保存普通列表（`PRIMARY_MODELS`、`PRIMARY_QUEUE_WAIT`）与 Ultra 列表（`ULTRA_PRIMARY_MODELS`、`ULTRA_PRIMARY_QUEUE_WAIT`，默认 `gemini-4-argon`）两份，`setPrimaryModelSettings` 整体原子替换，启动时与服务配置保存后立即生效（热更新日志的主用模型概况带上 Ultra 列表）。`primaryPolicyFor(pool, model)` 按请求的号池查找：`PoolScopeUltra`（`/ultra`）只查 Ultra 列表，`PoolScopeNormal`（`ULTRA_EXCLUSIVE=true` 的普通路径）只查普通列表，`PoolScopeAll`（`ULTRA_EXCLUSIVE=false` 的普通路径）先查普通列表、命中按普通规则，没有再查 Ultra 列表；命中 Ultra 列表的预算带 `quotaFailFast`（`leaseWaitBudget.failFastOnQuota()`，没有预算时为假），普通列表的预算没有，行为与原来相同。只在普通列表里的模型在 `/ultra` 上不再有排队上限。`StreamStartWait(ctx, model)` 按 `aistudio.PoolScopeFromContext(ctx)` 用同一规则取排队上限加 `primaryStreamStartGrace`（5 秒），不在对应列表里时返回 0；API 层传来的已是规范模型 ID，这里不再解析别名（那会在热路径上拿全局账户池锁）。API 层（`api.StreamStartWaitProvider`）与 `runtimeManager` 的转发都靠接口断言取这个方法，类型断言本身不检查签名，签名不一致只会静默返回 0；`internal/app/lifecycle.go` 因此对 `trackedService` 与 `runtimeManager` 各加一条 `var _ api.StreamStartWaitProvider` 编译期断言，`runtimeManager` 的转发也断言这个导出接口，改签名时两边一起报错。流式请求另经 `aistudio.StreamStartHold` 推迟写出响应头：API 层（`server.generate`，五种协议的流式入口共用）为每个流式请求创建一份，放进交给 `Generate` 的 context，`awaitStreamStart` 读同一份；Ultra 主用模型（`quotaFailFast`）的预算在 `generateWithRetry` 里取它（`holdStreamStart`），请求自己现场冷启动时 `startReservedWorker` 在浏览器启动前调用 `beginColdStart`，把这次启动还能抵扣的时长（`INIT_TIMEOUT` 减去已抵扣的）交给 `BeginColdStart`，抵扣已用完时不推迟；启动后 `creditColdStart` 结束标记并把实际抵扣的时长交给它。启动进行中 `awaitStreamStart` 的截止时间延后这次启动还能抵扣的时长，结束后改为延后实际抵扣的时长，与排队截止时间同步，推迟合计不超过 `INIT_TIMEOUT`（原先启动进行中一直不写出响应头，第一次启动挂到 `INIT_TIMEOUT` 失败后再现场冷启动一个，响应头要压到 `StreamStartWait` 加约两倍 `INIT_TIMEOUT`，多等的时间抵扣不了，额度不足照样在流内返回）。`awaitStreamStart` 另把截止时间限制在开始等首个事件后 `streamStartHoldMax`（50 秒，`StreamStartWait` 更长时按它）以内：推迟期间连接上没有任何字节，前面的反向代理读不到响应头时按读超时断开（nginx 默认 60 秒），断开还会取消请求与进行中的启动。第一次尝试现场冷启动备用账号超过 `StreamStartWait`、随后额度 429 的 `PrimaryQuotaError` 因此仍以 HTTP 429 返回，不是 200 之后的流内错误；启动更久、超过这两个上限时照常先回 200，其后的错误在流内返回。普通号池的主用模型不取它，行为不变；预热与后台扩容没有预算，也不改动它。Worker 管理器同样按分区保存两份列表（`primaryModelListFor(partition)`）：Ultra 分区只用 Ultra 列表，其余只用普通列表，`ULTRA_EXCLUSIVE=false` 时也不合并，免得普通号池的主用模型去淘汰数量少的 Ultra Worker；死槽位模型（`deadSlotModelsFor`，热门模型并上该分区的主用列表）、替补挑选、预热排序（`preferUncooled`）、空闲回收与淘汰登记都按 Worker 所在分区取列表，按选号号池判断时用 `deadSlotModelsForScope`（Ultra、普通各取本分区，`PoolScopeAll` 取两者并集）。

Ultra 主用模型的额度快速失败只对带 `quotaFailFast` 的预算生效。选号时候选全部冷却：`acquireLease` 不再等待 1 分钟内恢复的冷却，也不延后截止时间，立即返回 `AllCoolingError`（`Retry-After` 为最早恢复），进度写“Ultra 主用模型候选全部冷却，立即返回 429”；降级判定的 `PlaygroundFirst` 选号得到全部冷却时调用方仍按原逻辑改为不限通道选号（`playgroundUnavailable`），不直接对外 429；还有没冷却的备用账号、只是没有槽位（`waitReasonNoSlot`）时仍是容量原因，计入排队上限。请求中遇到额度 429：`generateWithRetry` 另记额度失败次数，与 `generateFailureLimit`、`maxAttempts`、`hardFailures` 分开——`applyQuotaCooldown` 成立算一次（输入超过账号上限的 429 不算），租约账号在请求发出前进入冷却（`ErrAccountCoolingDown`）也算一次，不计入 `hardFailures`、不退避。额度失败超过 `primaryQuotaSwitchLimit`（2，即一个请求最多 3 次因额度失败的尝试）时不再重试（租约照常由 `recoverWorkerOnce` 释放），按 `PrimaryQuotaError` 结束，不回到等待重置的原账户。额度失败后的下一次选号带“不现场冷启动”标记（只设在这一次选号的 context 上，不影响之后的选号）：`acquireLease` 不回收死槽位、不启动备用账号，也不排队等后台扩容；有空闲的热 Worker 照常取，都在忙时在排队预算内等待（截止后 `CapacityBusyError`）；没有任何热候选、只有没冷却的备用账号时，为其中一个触发一次后台扩容（所在分区能扩容时，不等待）并立即返回“额度 429 后没有已启动的 Worker”；候选全部冷却同上。额度失败之后选号失败为没有已启动的 Worker、全部冷却或 `ErrNoEligibleAccount`（不含 `AccountsNotReadyError`）时同样按 `PrimaryQuotaError` 结束（原因分别为“额度 429 后没有已启动的 Worker”“其余候选全部冷却”“没有其他可换的账号”）；文件绑定且不复制文件的请求遇到 `ErrNoEligibleAccount` 或全部冷却时先走原来的复制文件到其他账号再试；不固定账号的请求在 Worker 重建后固定到原账号的重放选号失败时（例如原账号在重建期间被其他请求的额度 429 冷却）照原来的规则只排除这个账号、继续换号，不按额度不足结束，那次选号只看了这一个账号；`CapacityBusyError` 原样返回。`AccountsNotReadyError`（剩下能服务该模型的账号需要重新登录、已停用或 runtime 被其他进程占用）照原来的规则先回到没等到关闭的原账号；回不去而上一次失败是额度失败时按“没有其他可换的账号”结束，不以上一次不带 `Retry-After` 的上游额度 429 结束（`maxAttempts` 按可调度账号数计、不看模型，可能比真正能服务该模型的账号多）。同一账号另一通道可用时照旧再试一次（计一次额度失败）；循环因 `maxAttempts` 用完而结束、最后是额度错误时也换成 `PrimaryQuotaError`（原因为“没有其他可换的账号”，账号少的 Ultra 号池常见）。第一次尝试照常可以现场冷启动一个备用账号（这时还不知道它有没有额度）。

`PrimaryQuotaError`（`internal/aistudio/accounts.go`，紧挨 `CapacityBusyError`）记模型、号池、原因（“额度失败已达换号上限”“其余候选全部冷却”“额度 429 后没有已启动的 Worker”“没有其他可换的账号”）、额度失败次数、总耗时、建议重试时间（`Until`）与最后一次上游额度错误（`Cause`，不实现 `Unwrap`，免得被当成上游 `RPCError`、输入超限等处理）；`HTTPStatus` 为 429，`ErrorCode` 为 `rate_limit_exceeded`，`RetryAt` 写进 `Retry-After`。建议重试时间：其余候选全部冷却时取全部冷却的最早恢复与本请求遇到的最早额度恢复中较早者，不设上限（每日限额就写真实恢复时间）；达到换号上限或没有其他可换的账号时取 5 秒后（`primaryRetryFallback`）与最早额度恢复中较早者（号池里可能还有没冷却的账号，判断要在账户池锁内分类全部候选）；没有已启动的 Worker 时为 5 秒后；都不早于 1 秒后。对外映射与 `AllCoolingError`、`CapacityBusyError` 相同：固定文案 `Resource has been exhausted (e.g. check quota).`，不把 `Cause` 或上游原文放进对外消息（见 [协议规范](protocol.md)）；请求记录、用量账本等对这两种错误做类型判断的地方同样把它记为限流而不是上游失败。进度日志的文案见 [运行日志](logging.md)。普通号池的 `PRIMARY_MODELS` 与不在列表里的模型行为不变。

账户池按选择条件（模型、方法、能力、通道限定、号池、资格范围）维护候选索引（`internal/aistudio/scheduling_index.go`）：每个候选集记录各账户的成员状态与冷却期限，未冷却的候选按账户 ID 排成可用列表，冷却中的候选放在按恢复时间排列的最小堆里，到期时移回可用列表、堆顶即最早恢复时间。目录、权益、状态、启用与运行态变化时由 `accountChangedLocked` 增量更新候选集，租约、独占与认证刷新的忙闲由 `busyChangedLocked` 写进按账户下标的紧凑数组，`ClassifyCandidates` 与不限账户的 `AcquireFor` 只扫描可用列表与这些数组，不再在账户池锁内遍历全部账户；只带排除列表（`ExcludedAccountIDs`，供换号重试排除已尝试的账户）的选择同样走候选索引，只跳过这几个账户的下标，不建立整个号池的允许集合，排除列表也不进候选集与调度等待队列的键；排空标记（`SetDraining`，与独占、认证刷新分开记录）让不固定账户的选择把该账户当作忙碌（热账户归入 `WarmBusy`、备用账户归入 `StandbyBusy`）且不再给它新租约，进行中的请求照常结束，指定账户与文件绑定的请求照常使用，账户对外状态、`Activity` 与预热概况不变，账户移出号池时清除；状态摘要（`PoolStateCounts`）、换号上限与预热概况也按这些数组推导账户状态，其中冷却按与账户状态表相同的口径（全局 `*` 冷却，或某个模型在账户可用的每个启用通道上都冷却）记录为到期时刻（`coolUntil`，见 `accountCoolingUntilLocked`），Build 通道对其他账户目录的依赖在目录版本变化后按 1 秒节流重算；冷却轮换与淘汰统计可接替账户（`SpareAccounts`）时同样先按这些数组排除停用、未就绪与全局长时间冷却的账户，指定模型时只计入是这些模型生成请求的候选、且支持的通道中至少一个没有长时间冷却的账户（目录里没有该模型的账户换上去同样是死槽位）；账户增删、上游通道变化或 `ULTRA_INCLUDE_PRO` 切换时整个索引重建，候选集另每分钟全量重建一次兜底。模型存在性检查、别名解析、模型目录的可用通道按目录版本（`eligibilityVersion`，账户增删、目录或权益变化、启用状态、Build 目录、上游通道与号池划分规则变化时递增）缓存并按目录内容去重重建（`pool_catalog_cache.go`）；预热模型、可生成模型数与状态页的模型列表按账户目录缓存；账户超过 200 个时 `Status` 的结果缓存 1 秒。候选都没有空位且有账户冷却时从磁盘重读运行态的刷新只刷新冷却中的候选、最多 32 个并发，间隔按账户数放大（每 1000 个账户加 3 秒，最长 30 秒）。`internal/aistudio` 的 `TestSchedulingIndexMatchesBruteForce` 在随机变更（含排空）后用逐个账户判断的参考实现核对索引（选择含随机排除列表）与可接替账户统计，`TestExclusionLeaseMatchesAllowList` 核对排除列表的选号与等价的限定账户集合一致，`TestClassifyCandidatesScale` 在 15k 账户的合成号池上压测分类耗时与锁持有时间（`POOL_SCALE_SECONDS`、`POOL_SCALE_ASSERT`）。

Worker 容量由热池目标、活动上限和单账户并发共同约束。活动数低于 `MAX_ACTIVE_WORKERS` 时直接启动并发布新 Worker。容量已满且存在空闲旧实例时，先启动 pending Worker（正在启动、尚未发布的替代 Worker），再关闭旧实例并发布替代 Worker；对请求模型处于冷却（全局或该模型限额）的空闲实例优先，同类中最久未用者优先，其次为最久未用实例。多个冷却实例可并行替换，每次替换预留独立的旧实例，启动期间新旧进程会短暂共存。启动失败或取消时现有 Worker 继续服务。请求只在取得容量槽位时占用冷账户；没有可立即使用的槽位时释放该账户并重新分类，由任一空闲的热 Worker 或新释放的槽位接收。存在可调度账户但暂时没有空闲槽位的请求按相同选择条件先到先服务排队，在租约释放或 Worker 状态变化时唤醒，直到请求超时。生成请求的选号带排队进度（`leaseWaitProgress`，与排队预算一样经 context 交给 `acquireLease`，见 `queue_progress.go`）：一次尝试排队满 2 秒仍没有取得账号时在请求时间线写明等待原因（`排队等待`，之后每个请求最多每 15 秒一条、全进程限流，主用模型容量原因的等待不写，见 [运行日志](logging.md)），请求在排队中超时或取消时选号错误带上最后的等待原因与已等待时间（`%w` 包装，504/499 映射不变）。候选账户全部处于冷却时，最早恢复时间在 1 分钟内的请求排队等待恢复，更晚的请求直接返回 429，`Retry-After` 给出距最早恢复时间的秒数；Ultra 主用模型不等待，立即返回 429。热 Worker 都在冷却或忙于其他模型、而还有没冷却的备用账户，只是所在分区没有空槽也没有可回收或替换的空闲 Worker 时，原因是容量而不是冷却：请求按“没有空闲的 Worker 槽位”排队（主用模型受排队上限限制，其余模型等到请求超时），不返回全部冷却的 429（原先的 `Retry-After` 取候选集里最早恢复的账户，可能长达几小时），也不按冷却等待绕过排队上限；热候选取租约时被其他请求抢先同样按“账号全部忙碌”等待。降级判定的 Playground 优先选号除外：它全部冷却时调用方立即改为不限通道选号。超出 `WARM_WORKER_LIMIT` 的 Worker 空闲 5 分钟后关闭，热池保持目标数量。旧 Worker 与 pending 回收同时失败时，两份进程与租约均保留为 cleanup pending（仍待关闭）并占用容量槽，后续 Stop 会重试关闭。账户的 WAA runtime 租约由其他进程持有时，该账户退出预热与调度候选，首次 5 秒后重新探测，每次仍被占用时间隔翻倍、上限 1 分钟，每段占用只记录一条日志；指定该账户或只剩该类账户的请求返回账户正在使用的错误（对外按 503 服务暂时不可用）。

启动失败记忆（`internal/app/startup_failures.go`）：启动失败原先只写一行日志，同一个排队的请求会反复挑同一个失败的备用账户启动（测试里一个失败号 2 秒被启动 97 次，健康账户一次都轮不到），登录页跳转也不会把账户标为需要登录。`startAuthenticatedWorker` 在返回启动失败前调用一次 `noteStartupFailure`，覆盖预热、后台扩容、替补、请求现场启动与 `Worker()`；它只做记录，返回的错误不变。`startReservedWorker` 在 `StartupProgress` 回调里把当前阶段写进局部 `atomic.Int32`（初值 1；Camoufox 读完 storage state 才报告第 2 阶段，纯 Go 后端报告 2、5、6、7），失败时随 `accountWorkerInitError.stage` 返回，取得 runtime 租约失败时 `leaseFailed` 为真；带租约恢复后重新启动的错误同样带上阶段。`classifyStartupFailure(err, stage)` 按顺序取第一条：不是 `*accountWorkerInitError`，或属于容量已满（含 `errStartupSlotsLate`）、正在启动、被其他进程占用、清理未完成的不计入（ctx 已结束的由 `noteStartupFailure` 跳过，启动途中取消只写 `WAA Worker 启动取消`）；`aistudio.DefinitiveAuthenticationFailure` 为登录失效（`auth`）；阶段 ≤2、又不是读写文件的系统错误（`fileSystemError`：`*fs.PathError`、`*os.LinkError` 或 `fs.ErrNotExist`，文件不存在、打开文件过多、权限、磁盘满等不能说明文件的内容坏了）时，错误文本含 `Camoufox 指纹` 的为指纹文件损坏（`fingerprint`，先于 storage state 判断），含 `storage state` 的为认证文件损坏（`files`）；阶段 ≤4 或 `leaseFailed` 为环境问题（`environment`）；阶段为 7、错误文本含 `startupPageBlockedTexts`（`填写 bootstrap 提示词失败`、`等待官网 Run 按钮启用`：Camoufox 的 `填写 bootstrap 提示词失败 value=%q err=%v`、`等待官网 Run 按钮启用: %w`，以及纯 Go 后端 `runtime_go.go` 的 `填写 bootstrap 提示词失败: %w`）、不含 `startupBrowserBrokenTexts`（`BiDi 连接已断开`、`websocket: close` 等浏览器或运行时已断开、退出的迹象），且 `initExpired` 为假（`startReservedWorker` 在 `cancel` 之前读取：调用方 ctx 没结束、`initCtx` 已结束，说明整个启动用满了 `INIT_TIMEOUT`，这时提示词与 Run 按钮只是页面加载太慢没来得及）的为页面受阻（`page_blocked`）；其余（阶段 ≥5 的页面未就绪、WAA 失败、超时）为原因不确定（`ambiguous`）。分类依赖 camoufoxnative 与纯 Go 后端的错误文本与阶段号，那边改文案或阶段时这里要一起改，测试用的是原样错误文本（含 `value=%q` 的转义）。

登录页跳转由 `aistudio.NewNativeWorker` 的 `classifyNativeStartError`（`internal/aistudio/runtime_native.go`）识别：错误已是 `ErrAuthenticationRequired` 时原样返回；否则取错误文本里 `隔离登录态失效 url=` 之后到第一个空白为止的地址（与清理错误 `errors.Join` 时以换行分隔），`url.Parse` 后主机名为 `accounts.google.com` 才用 `fmt.Errorf("%w: %w", ErrAuthenticationRequired, err)` 包装、保留原错误，不按路径判断；`NativeLoginRedirectURL` 取出的地址只用于日志与列表展示。之后沿用启动阶段原有的登录失效处理：无租约且没有续签材料时 `markStartupAuthRequired` 按启动前的认证代际标记，带租约时 `refresher.Recover` 恢复一次（恢复成功后把记录用的代际改为恢复后的代际），仍失败才标记。不修改 `internal/camoufoxnative`。

记录前比较认证代际：`generation` 为 `startAuthenticatedWorker` 开头读取的 `pool.AuthGeneration`，记录时代际已变化的（启动排队或启动途中 `SaveStorageState` 替换了认证文件、续签提交了新认证）不记，旧凭据的失败不记到新凭据头上。按类别处理：`environment` 只记入 manager 的环境类失败时间点（`envFailures`，由 `envFailMu` 保护，状态接口统计近 10 分钟），不写账户记录；其中阶段 ≤2 的 `fs.ErrNotExist` 交给 `noteFilesMissing`：`AccountPool.MarkFilesMissing` 用 `validatePersistentAccountFiles` 确认 `account.json` 或 `storage-state.json` 确实不在、账户为就绪时调 `markStaleAccountUnavailable`（与取得租约时发现文件缺失相同，`AccountLoginSummary.FilesMissing` 为真），`accountWatcher.recoverRestored` 在文件恢复并稳定后重新同步模型目录恢复它，目录一直不在时由 `detachMissing` 移出。原先文件不存在算 `files`，整体替换账户目录期间被预热、扩容或替补启动到的健康账户会被标为需要登录、列为确定失效，文件复制回来后登录签名不变，`recoverRestored` 与 `authFilesUpdated` 都不会解除，能被一键删除。`auth` 与 `files` 不设暂停（已标为需要登录的账户由账户状态退出调度；有续签材料、没有租约的保持原来的行为，留给带租约的按需启动恢复，已知限制是这期间它仍可能被后台扩容反复选中）；`files` 先用 `MarkAuthenticationRequiredIfGeneration` 标为需要登录（原因“认证文件无法用于启动浏览器：…”）。两类都只在账户随后确实处于 `auth_required`（`pool.States`）时写入持久表；有续签材料、留给带租约恢复的，以及启动开始后验证通过（`MarkReady` 推后 `authCheckedAt`）、标记被检查时间拒绝的，写 `不计入失效账户` 后返回，不写记录：这些账户仍可调度、可能恢复，写进表里会被列为确定失效、能被一键删除；`fingerprint` 原先也算 `files`：账户的 `camoufox-fingerprint.json` 由 camoufoxnative 用非原子的 `os.WriteFile` 写入，写到一半时磁盘满或进程被杀会留下空文件或截断的 JSON，之后每次启动都在阶段 2 失败；指纹不是登录凭据，重新上传认证文件不碰它，浏览器登录（`PersistAccountFingerprint`）与验证（`loadAccountCamoufoxConfig`）也都要先读它，标为需要登录会把登录正常的账户列为确定失效、只剩一键删除。现在它与 `ambiguous` 一样由 `notePausedFailure` 只在内存里暂停，不改变账户状态、不写持久表，`StartupFailures` 列为 `confirmed=false`、`kind=fingerprint`，`reason` 与日志写明指纹文件路径（由 accountWorker 配置里的 `StorageStatePath` 所在目录拼出），由管理员从备份恢复或删除该文件后清除暂停；暂停时长同 `ambiguous`，但不按系统性问题处理。`ambiguous` 与 `fingerprint` 由 `notePausedFailure` 记在 accountWorker 上：`startFailUntil`（UnixNano）、`startFailErr`、`startFailGen` 是原子值，类别 `startFailKind`、计数与展示用的阶段、首次与最近时间由 `account.mu` 保护（认证代际或类别变化时计数从头开始），写入顺序同 `markRuntimeBusy`（先写原因与代际再写期限）。暂停时长按计入次数 `startFailCount` 依次为 2m、10m、30m 封顶；`lastStartupSuccess` 距今不少于 5 分钟（含 0，即本次生成服务还没有成功启动过 Worker）时按系统性问题只暂停 1m，`startFailCount` 不加，只加列表展示的 `startFailTotal`。暂停到期由 `time.AfterFunc(delay, notifyScheduling)` 唤醒调度；账户状态永不改变，也不持久化。

页面受阻（`page_blocked`，号池健康的判断见 `internal/app/startup_page_blocked.go`）：后两类错误（填写 bootstrap 提示词失败、Run 按钮一直不可点）发生在页面已经打开之后，用户生产日志里各有 300 多次与 100 多次，失败最多的账户按字母顺序集中在前面，多半是账户本身的问题（官网弹出需要同意的条款或通知，挡住了输入框）；原先与页面未就绪一起归为 `ambiguous`，只暂停、不能删。现在它与 `ambiguous` 一样由 `notePausedFailure` 在内存里暂停（时长、系统性问题的 1m 与不计次数都相同；已确认的账户按系统性问题处理时同样不计次数、不更新记录，但仍暂停 `startupMaxPause`，服务刚启动时不每分钟重启一次已确认的账户），另用 `startFailEvidence`（`account.mu` 保护，认证代际或类别变化时与计数一起从头开始，启动成功清零）记计入失效确认的连续次数：只有失败时号池健康才加一。号池健康由 manager 的 `startupHealth`（`startupHealthTracker`）判断：`startReservedWorker` 在调用 `launch` 前记下尝试启动的账户，`activateAccountWorker` 记下启动成功的账户；健康要求失败前 10 分钟内至少有一个**其他账户**启动成功，且不处于页面受阻的大面积爆发（最近 30 分钟内出现 `page_blocked` 的不同账户数超过 max(10, 最近 30 分钟内尝试启动的不同账户数 × 20%)，可能是官网改版或灰度）。两个账户数都不含已确认的账户：`notePausedFailure` 先按登录签名查持久表再调 `notePageBlocked`（已确认的 `counted` 为假，只返回号池情况），`startReservedWorker` 按账户 ID 查持久表决定是否 `noteAttempt`；否则留在号池里等用户检查的十来个已确认账户每 30 分钟重试一次，会让爆发一直成立、不再确认新的失效，还提示用户不要删除恰恰是它们。爆发期间不确认任何新的页面受阻失效、已有记录不动，进入爆发时写一次 WARN `大量账户页面受阻，可能是官网页面改版，暂不确认为失效`，`AdminStatus.Startup.PageBlockedBurst` 为真。号池不健康的那次（以及按系统性问题处理的那次）照常暂停，但 `startFailEvidence` 不增加、也不清零。同一认证代际下连续 3 次健康的页面受阻时确认为失效：账户状态不变（仍是就绪，不标为需要登录，问题消失时启动成功会自己恢复），之后每次失败都按 `startupMaxPause`（30m）暂停，写入持久表（`kind=page_blocked`，与 `auth`、`files` 同一张表，带 `identity`、`count`、`message`、`stage`、`first_at`、`last_at`），写 WARN `WAA Worker 页面受阻已确认为失效账户`。不修改 `internal/camoufoxnative`，靠错误文本识别。

读取：`startupBackoffAvailable(ids, allowProbe)` 写法同 `runtimeAvailable`，按原顺序过滤，只读原子值、不取账户锁，只对暂停中的账户（在 `manager.mu` 之外）用 `AccountPool.AuthGenerations` 一次取号池锁核对认证代际，代际已变化的视为没有记录；`pausedStartupCount` 与 `startupPauses` 同样一次取得。选号每次重新检查调用两次，部分代理或 WAA 故障时暂停的账户可能上千，逐个 `AuthGeneration` 会在排队请求多时每秒取号池锁几十万次，拖慢同样要取它的分类与租约操作。读取点只有三处：`acquireLease` 中不固定账户的请求过滤 `StandbyBusy` 与 `StandbyReady`（过滤掉的数量写进排队进度的 `启动失败暂停`；备用账户都在暂停时按 `waitReasonStartupPaused` 排队，属于容量原因，主用模型受排队预算限制，不落到全部冷却的 429 或没有账户的 503）、`fillWarm`（`warmFailures` 不动）与 `pickReplacement`。不在 `ensureWorker` 设门：固定账户、文件绑定的请求以及受保护传输经 `Worker()` 的现场重建（持有请求租约，Worker 被重置后要现场重建并可能走认证恢复）不受暂停影响。保底试探：`acquireLease` 没有热候选且 `StandbyBusy` 过滤后为空，或 `fillWarm` 该分区没有热 Worker、没有在启动的、也没有忙碌候选时 `allowProbe` 为真，此时过滤后为空、原列表非空的，用 CAS 更新 `lastProbe`，全局每分钟最多放行一个最早到期的账户（`WAA Worker 保底试探`）；替补不试探。清零：`activateAccountWorker` 在 `warm.Store(true)` 之后调 `clearStartupFailureLocked` 并写 `lastStartupSuccess`，同时删除该账户的持久记录（`WAA Worker 启动成功，清除失效账户记录`）；验证通过、认证文件更新与清除接口经 `clearStartupFailure`/`clearStartupFailures`；账户移除时 accountWorker 随之删除。

持久表（`internal/app/startup_failures_store.go`）只存使账户处于 `auth_required` 的 `auth` 与 `files`，以及确认为失效的 `page_blocked`（账户状态仍是就绪）：`.startup-failures.json` 放在 `store.PrimaryDirectory()`，同一路径共用一个 `startupFailureRegistry`（`startupFailureRegistries`，同 `permissionDeniedRegistries`：重启生成服务时新实例先装配、旧实例后关闭，各自读写会丢掉旧实例最后几秒的变化）。每条记录为 `kind`、`url`、`message`（截断到 300 字符）、`stage`、`count`、`first_at`、`last_at`、`identity`（`LoginIdentity`）；同一 `kind` 与 `identity` 下连续计数，任一变化时从头计。变化约 3 秒合并后原子写入，`closeRuntime` 里 `flush`；读取失败写 WARN 后从空表开始。重启对账 `reconcileStartupFailures` 在 `newRuntime` 里 `reconcilePermissionDenied` 之后运行：账户已不在池里的删除；`identity` 与当前不同的删除（服务停止期间重新上传了认证文件）；`auth` 记录 `count` ≥ 2、账户没有续签材料（`authRefreshMaterial`）、当前不是 `auth_required` 或 `disabled` 的调 `MarkAuthRequired`；其余（`count` 为 1 的 `auth`、`files`）只保留展示（账户为就绪，列表里列为未确认、不能一键删除），让账户重试一次（一次登录页跳转可能是偶发），再次失败、标为需要登录时 `count` 加一并列为确定失效；`page_blocked` 记录不依赖账户状态，`identity` 不变就保留，仍列为确定失效，`restorePageBlockedPause` 把暂停恢复到最近一次失败后 `startupMaxPause`（已经过了的不恢复、让账户重试），连续次数按已确认处理。

清除钩子一律按精确 ID（`remove`），`page_blocked` 记录与 `auth`、`files` 共用同一套，不另起：启动成功（`activateAccountWorker`）、`removeAccount`（单个删除与批量删除共用）、`DeleteAccounts` 里号池中已经没有的 `missing` 循环、`detachAccount`（账户目录被外部移走；它同 `permissionDenied.clearAccount` 一样不经过 `forgetPermissionDenied`）。认证文件更新类改为调用 `trackedService.authFilesUpdated(id, source)`，它包住 `forgetPermissionDenied` 与 `forgetStartupFailure`（持久记录与内存暂停一起清除），替换新增账户（`addAccount`）、浏览器登录（`LoginAccount`）、目录重新导入（`accountWatcher.scan`）与两处外部认证文件更新（`refreshAccountLogin`、`reloadAccountCredentials`）原来的 `forgetPermissionDenied`；`verifyLogin` 验证通过时调 `forgetStartupFailure`；`refresher.renewed` 同时更新两张表记下的 `identity`（本服务续签不当作重新上传）。管理页的清除接口（`startupFailureRegistry.clear` 与 `clearStartupFailures`）先精确匹配，匹配不到的再按 `strings.EqualFold`，同 `permissionDeniedRegistry.clear`；删除钩子不能用它，否则只差大小写的 ID 会清掉号池里另一个账户的记录。

接口与状态：`internal/api/startup_failures.go` 沿用 `accounts_bulk.go` 的可选接口加类型断言（管理端不实现时 501），`runtimeManager` 转交当前生成服务的 `runtimeAdmin`。列表的 `confirmed` 列当前处于 `auth_required` 的账户（有持久记录的带上记录，按 ID 去重），以及账户还在池里的 `kind=page_blocked` 持久记录（账户为就绪，暂停中时带 `until`；账户已不在池里的列为未确认）；未确认的来自 `startupPauses`（代际已变化或已到期的不列），以及账户当前不是 `auth_required` 的持久记录（重启对账只保留展示的、续签或验证通过之前留下的，`reason` 注明下次启动再失败才确认），同一账户只列一次、暂停优先。一键删除只删 `confirmed`，所以仍可调度、可能恢复的账户不会被删。`AdminStatus.Startup` 由 `startupStatus` 填写：`startupSlots.usage()` 的占用与按需、后台排队数，上限 `max(WARM_STARTUP_CONCURRENCY,1)+1`，`pausedStartupCount`（只读原子值、不取账户锁：取账户锁会让 `occupiedWorkersIn` 把正被读取的账户误算成占用的槽位），`dead` 为 `counts.AuthRequired` 加 `pageBlockedDead`（有页面受阻记录、还在池里且不是 `auth_required` 的账户数，不重复计数；与列表的 `confirmed` 一致），`lastStartupSuccess` 与近 10 分钟环境类失败数，`page_blocked_burst` 为当前是否处于页面受阻的大面积爆发。前端只读这些字段：`page_blocked` 的行显示“页面受阻”与错误摘要，`page_blocked_burst` 为真时失效账户卡片顶部显示黄色提示、一键删除的确认框另加警告。

后台启动的冷启动名额：后台启动（`withBackgroundStartup`：预热、补齐、替补）的期限包含等名额的时间。`startReservedWorker` 在等名额之前记下剩余期限是否还够 `INIT_TIMEOUT`，取得名额后如果本来够、现在不够（名额被按需启动占满），放回名额、释放自己取得的 runtime 租约并返回 `errStartupSlotsLate`（`fmt.Errorf("%w: …", errAccountWorkerCapacity)`，`errors.Is` 即可识别），不启动注定被掐断的浏览器；等名额一直等到期限（`acquireStartupSlot` 里后台启动的 `context.DeadlineExceeded`，按需启动排队时后台启动一律让行，长队列时多半是这种）同样包装成 `errStartupSlotsLate`，等名额时被取消的仍只是容量已满。`fillWarm` 按容量已满处理（不记预热失败，本轮该分区不再启动），只有这种原因时等待原因写“冷启动名额被按需启动占满”，不误报为 Worker 槽位已满。期限本来就短于 `INIT_TIMEOUT` 的调用方照常启动。单个预热与替补任务的期限为 `warmTaskTimeout()`（`INIT_TIMEOUT` + `warmTaskGrace` 1 分钟，测试用 `warmGrace` 调低）。`expandInBackground` 的 `WAA Worker 后台扩容失败` 只写非 `accountWorkerInitError` 的错误，启动错误已有带阶段与类别的 ERROR。

驻留 Worker 的账号进入长时间冷却（剩余不少于 15 分钟：每日限额冻结整个账号的全局 `*` 冷却，或请求模型的所有启用通道都冷却）后，这个 Worker 成为“死槽位”：占着常驻名额却服务不了请求（`internal/app/worker_retire.go`）。处理分三层，都与按需替换共用 `tryReserveVictim`，都经账号独占租约淘汰，正在服务请求的 Worker 永远不会被关闭，并且只在分区内有可接替的账户时淘汰（全部账号都冷却的小规模部署保留 Worker，冷却到期后立即可用）：`applyQuotaCooldown` 写入冷却后立即登记淘汰，Worker 空闲就关闭、忙碌则等它的请求释放租约后关闭，冷却被清除或缩短时放弃，淘汰成功后立即补齐预热；`keepWarm` 每个周期清扫全局长时间冷却的空闲 Worker，不受按模型轮换的 5% / 最少 3 个阈值限制，每轮两个分区合计最多关闭 `WARM_STARTUP_CONCURRENCY` 个并并发关闭，接替的 Worker 由预热经冷启动名额补齐；请求选号时备用账户所在的分区已满而分区里有对该模型是死槽位的空闲 Worker，先现场回收一个（每轮一个，只在该分区内）再启动备用账户，不等轮换也不先冷启动替代 Worker，回收写入请求时间线。按模型的冷却轮换（每 30 秒、阈值与一轮替换数不变）继续处理只在个别模型上冷却的账号。淘汰选号（`idleWarmVictimFor`）改为账户池一次加锁取得全部候选的活动与冷却状态，死槽位优先于短暂冷却的账号，再优先于最久未用的账号。状态接口的 `workers.dead`、`ultra_workers.dead` 给出各分区的死槽位数（全局长时间冷却，或在热门冷却模型、所在分区的主用模型上长时间冷却：普通分区按 `PRIMARY_MODELS`，Ultra 分区按 `ULTRA_PRIMARY_MODELS`），`draining` 与 `retiring` 给出其中排空中的数量与待淘汰登记数（`drainCounts`、`retirementCounts`，各一次加锁）。

饱和时其他模型的请求一直在用死槽位，它永远等不到空闲，于是先排空再淘汰（`internal/app/worker_drain.go`）。淘汰循环遇到忙碌的登记（全局冷却，或在主用、热门模型上冷却；只在其他模型上冷却的 Worker 还能服务主用模型，忙碌时不排空，空闲后照常淘汰）时，在分区排空数低于 `drainLimit`（`max(WARM_STARTUP_CONCURRENCY, 1) × 2`，且不超过分区峰值的 1/4、至少 1；峰值为 0 的分区不排空）、分区内又有按登记模型统计的可接替账户时，把账号标记排空（`AccountPool.SetDraining`，最老的登记先排空；`startDrain` 在 `drainMu` 内核对账号仍有驻留 Worker、没有被其他路径选中淘汰）：不固定账户的请求不再分给它，进行中的请求照常结束，最后一个租约释放后由原有的唤醒路径关闭；指定账户与文件绑定的请求照常使用。排空开始时立即在后台启动替补（先启后停）：`ensureWorker`、请求调度的分区容量（`standbyCapacity`，与 `ensureWorker` 一样把启动中的 Worker 计入占用）与状态接口的 `occupied` 都把排空中的 Worker 按已让出槽位计，最多 `drainLimit` 个，分区浏览器总数最多为峰值加 `drainLimit`。排空在 Worker 关闭（任何关闭路径，含 `ResetAll` 与 `Close`）、登记被放弃（冷却被清除或缩短）、淘汰出错时清除；超过 `REQUEST_TIMEOUT` + 30 秒仍有请求时清除标记、1 分钟后再试。替补（`startReplacement`）在排空开始、空闲淘汰、冷却清扫与冷却轮换之后启动（现场回收不启动替补：请求随后自己启动的备用账号补上空出的槽位）：在同一分区按调度索引挑选能服务登记模型（全局冷却的登记按主用模型，没有主用模型时按预热候选）、对热门与主用模型都没有长时间冷却的空闲备用账号（每批 64 个、最多核对 1024 个），经后台冷启动名额启动，只用空槽（含排空让出的），不替换其他账号的空闲 Worker；分区低于预热目标时由预热补齐（排空开始时除外），`WARM_WORKER_LIMIT` 低于驻留数时驻留数靠替补保持；排空开始时的替补启动成功或仍在启动时，旧 Worker 关闭后不再多补。淘汰统计可接替账户时按登记的模型统计（`SpareAccounts`），唯一可接替的账号对同一模型也已冷却时不淘汰、不排空。`keepWarm` 每个周期把驻留 Worker 中的死槽位补登记（`registerDeadWorkers`：一次 `LongCoolingSet` 找出死槽位，只对它们按全局与各模型分别核对范围），重启后、其他进程写入的冷却、冷却时还没驻留的账号同样会被排空淘汰，登记与状态接口的 `dead` 口径一致。放置与挑选也按模型：模型不在主用与热门模型里的不固定账户请求先在对这些模型是死槽位的热 Worker 里取租约（多一次 `LongCoolingSet` 与一次限定账户的 `TryAcquireFor`），取不到再用其余热 Worker，把还能服务主用模型的租约位留给主用模型；请求路径为后台扩容与现场启动挑备用账号时（只核对前 64 个候选）、预热排序时优先没有在热门或主用模型上长时间冷却的账号；超出常驻数的空闲回收只在空闲超时的 Worker 中挑选，其中先关闭这类死槽位（刚被其他模型的请求用过的死槽位不会让回收整轮放弃）。

主用模型供给（`internal/app/primary_supply.go`）：`keepWarm` 的每个周期（`keepWarmPass`）调用 `refreshPrimarySupply`，缓存超过 30 秒、两份主用模型列表任一变化或号池规则版本（`AccountPool.PoolRuleVersion`，见下文“号池成员规则”一段）变化时重新统计，两份都为空时清空，`ResetAll` 时也清空；状态接口的 `primary_supply` 只读这份缓存（服务未运行时不返回），不在状态请求里对上万账户分类。统计时普通列表的每个模型在普通分区、Ultra 列表的每个模型在 Ultra 分区（`PoolScopeUltra`，只算 Ultra 分区的驻留 Worker，条目 `pool` 为 `ultra`，普通条目 `pool` 为空）各按调度索引分类一次（`ClassifyCandidatesCached`，只读内存状态）：排空中的驻留 Worker 不作为热账户传入，随排空标记落入忙碌分组，不计入 `warm`；`warm` 为热账户的三个分组之和，`standby` 为 `StandbyReady` 中不在启动中的账号，最多数到 1000 个（`standby_capped`）。侧栏据此为每个主用模型显示一行“主用 <模型>：热 N · 备用 M”，Ultra 条目在 Ultra Worker 一行下面显示为“Ultra 主用 <模型>：热 N · 备用 M”，备用为 0 时标黄；死槽位一行在有排空时追加“排空中 N”。`TestPrimarySupplyCounts`、`TestPrimarySupplyCachedForStatus`、`TestKeepWarmRefreshesPrimarySupply` 与 `TestStatusShowsDrainingAndRetiring` 覆盖供给口径、缓存与状态字段。

账户按当前权益与 `ULTRA_INCLUDE_PRO` 分为两个号池：权益为 Ultra 的账户，以及 `ULTRA_INCLUDE_PRO=true` 时权益为 Pro 的账户，组成 Ultra 号池（付费号池），其余账户（含权益尚未读取的账户）组成普通号池。请求的号池写在 context 中（`aistudio.ContextWithPoolScope`），`AccountSelection.Pool` 为空时 `AcquireFor`、`TryAcquireFor`、`ClassifyCandidates` 取 context 中的号池；`/ultra` 请求只用 Ultra 号池，`ULTRA_EXCLUSIVE=true` 时普通路径只用普通号池，为 `false` 时普通路径不限号池，预热、轮换与登录检查等后台任务不限号池。选号、模型存在性校验、模型目录与别名解析、换号、跨账户文件复制、上传候选与没有账户时的错误都只看号池内的账户；Ultra 号池为空或账户都不可调度时返回写明 Ultra 号池的 `AccountsNotReadyError`（503），全部冷却仍按 429。文件或视频绑定在号池之外的账户上时返回 `ResourcePoolMismatchError`（400，英文说明资源属于哪个号池），不会改用池外账户。账户权益变化后立即按新权益归入号池；`ULTRA_INCLUDE_PRO` 开着时 Pro 降为 Free 回到普通号池、升为 Ultra 留在 Ultra 号池，都经 `accountChangedLocked` 的增量路径完成。

号池成员规则（`internal/aistudio/pool_scope.go`）：成员只由 `ultraPoolTier(tier, includePro)` 决定（Ultra 总是属于 Ultra 号池，`includePro` 为真时 Pro 也属于；不叫 paid，`Model.Paid` 是模型属性），账户池内的号池判断都经 `inPoolLocked`、`tierInPoolLocked` 与 `poolOfTierLocked`，读取受 `p.mu` 保护的 `ultraIncludePro`，开关与它派生的成员集合、候选集在同一把锁下变化。候选索引按位置只存真实权益（`index.tier[]`），号池在读取时由权益加开关推导，`verifySchedulingIndexLocked` 对权益的核对因此不变。号池划分不改变权益：权益请求头（`HeaderValue`、`applyBenefitTier`，Pro 仍发 `TIER1`）、AccessModes 资格（`modelAllowedByTier`）、`SupportsModel`/`SupportsMethod`/`accountSupportsSelection`、冷却期限、`PreferWarmPool` 的分层（`benefitTierPriority`）、`buildSupportsModelLocked`、`buildUsesUnary`、输入上限与更高权益重试的等级比较（`benefitTierRank`）、预热模型与覆盖率缓存、`SetCatalog` 与 `syncAccountRuntimeLocked` 的权益比较都不读开关；`EnabledAccountsAboveTierOf` 只把号池过滤换成成员判断，权益高低仍按真实权益比较。`PoolOf` 返回成员关系（Worker 分区），`TierPoolOf` 返回按真实权益的号池（Ultra 权益为 `PoolScopeUltra`，其余为 `PoolScopeNormal`），只供每日额度通道判定分组：Pro 的额度与 Ultra 不同，证据记进 `模型|ultra` 后关掉开关也不会消失，`.quota-sharing.json` 因此不需要迁移。`Status` 的 `Pool` 为成员关系，`BenefitTier` 仍是真实权益。号池为空时的原因在开关关闭时保持原文，开启时 Ultra 号池为空写“没有权益为 Ultra 或 Pro 的账户”，独占模式下普通号池为空写明 Pro 也属于 Ultra 号池。

`SetUltraIncludePro` 的做法与 `Remove`、`SetUpstreamChannels` 相同，把号池划分规则变化当作与权益变化同类的事件：在 `p.mu` 内按新规则一次扫描重算 `ultraAccounts`，作废 `UltraAccountIDs` 的快照，`eligibilityVersion++` 让按版本缓存的目录汇总、别名解析与模型通道重建，整个候选索引作废（`invalidateSchedulingLocked`，下次调度时重建；候选集指针都在账户池锁内用完，没有留到锁外），作废状态缓存，`poolRuleVersion` 加一，最后关闭 changed 唤醒等待账户的请求与后台循环。不在这里手工清除各个按号池的缓存，以后新增按 `eligibilityVersion` 缓存的结构自动随之作废。开关值没有变化时什么都不做、返回 `false`。进行中的租约不受影响；按旧成员选中、尚未确认的租约由 `refreshAndValidateLease` 放弃并重新选号；固定账户的请求在账户已离开请求的号池时返回 `ErrNoEligibleAccount`。切换本身在 3.1 万账户上持锁 2.4～6.7 毫秒，之后首次使用多出约 0.3 秒的重建，分摊到随后的多次加锁里。

`UltraAccountIDs` 返回写时复制的共享快照，调用方只能读、不能修改：快照为 nil 时在 `p.mu` 内克隆一次 `ultraAccounts` 并保存，之后的调用直接返回同一份；`accountChangedLocked` 只在成员实际变化时修改集合并作废快照（它每秒被冷却、状态与运行态变化调用很多次，成员不变时作废等于没做写时复制），`Remove` 与 `SetUltraIncludePro` 也作废快照。Pro 并入后成员可达数千，每次调用都克隆会在账户池锁内的热路径上花去近 1 毫秒（4,077 项实测 0.84 毫秒）。

`PoolRuleVersion()` 无锁返回号池规则版本，开关每次实际切换时加一。app 层在锁外缓存、又按号池成员计算的派生结果用它作废：预热概况（`bootstrapSummary`，可预热账户超过 200 个时缓存 60 秒，`UltraAvailable` 按成员计数，沿用旧值会让 Ultra 分区的预热目标在缓存期内仍按旧成员计算）与主用模型供给（`primarySupplySnapshot.ruleVersion`）都在计算前先读版本、与结果一同保存，命中条件加上版本相等。先读版本再计算，切换前开始、切换后才写回的旧结果带着旧版本，下一次调用照样重算，没有竞态；主用模型供给因此在切换后的下一个 `keepWarm` 周期（5 秒内）重算，不等满 30 秒。

Worker 同样按账户当前所在的号池分为普通分区与 Ultra 分区（`internal/app/worker_partition.go`，成员每次调用都从 `UltraAccountIDs` 重新读取）。`WARM_WORKER_LIMIT`、`MAX_ACTIVE_WORKERS` 只约束普通分区，`ULTRA_WARM_WORKER_LIMIT`、`ULTRA_MAX_ACTIVE_WORKERS` 约束 Ultra 分区，浏览器总数最多为两个分区上限之和。预热把每个分区从自己的账户补齐到自己的常驻数（没有可预热的 Ultra 账户时不为 Ultra 分区分类候选；可预热的账户都是 Ultra 时同样不为普通分区分类候选，只有 Ultra 账户的部署按 Ultra 分区完成首轮预热，`ULTRA_WARM_WORKER_LIMIT=0` 时直接完成、Ultra Worker 按需启动），预热不足按分区判断并在日志中写明分区，超出分区常驻数的空闲 Worker 按分区回收；按需启动的容量判断、`idleWarmVictimFor` 的淘汰选号与冷却轮换（`tryReserveVictim`）都只在账户所在分区内进行，只有同分区正在启动的 Worker 才会让淘汰只选冷却中的 Worker、暂停空闲回收，Ultra 请求不会淘汰普通 Worker，反之亦然；请求调度按备用账户各自的分区判断能否扩容，等待队列按含号池的选择条件分组，同分区的 Worker 空闲或容量释放后唤醒排队请求。冷启动名额（`WARM_STARTUP_CONCURRENCY`）、冷却轮换一轮的替换数与单账户并发两个分区共用。账户权益在持有 Worker 期间变化、或 `ULTRA_INCLUDE_PRO` 切换时，该 Worker 从此按新分区计数，不强制重启。四个 Ultra 设置与普通分区设置一样随服务配置保存立即热更新。

`ULTRA_INCLUDE_PRO` 的热切换（`internal/app/hot_config.go` 的 `applyLiveConfig`）：`liveConfigMu` 让配置热更新逐次进行，最后一次应用读到的一定是最后写入的 `.env`，两次并发保存（一开一关）后账户池的开关、`current.config` 与 `.env` 一致；在 `manager.mu` 内先 `applyLiveSettings` 更新两个分区的容量，再 `SetUltraIncludePro` 改成员（锁顺序 `manager.mu` 再到账户池锁，与 `SetPerAccountConcurrency` 相同），同一次保存调大 `ULTRA_*` 峰值时并入的 Pro Worker 按新上限计数；解锁后开关实际变化时调用 `poolRuleChanged`，随后照常 `prewarmIfRunning` 补齐失去或得到 Pro Worker 的分区，`syncModelCache` 推送状态、模型与账户快照。解锁之后的步骤都从当前状态重新推导（成员、开关与生效的 `ULTRA_EXCLUSIVE`），不依赖解锁前捕获的配置。`poolRuleChanged`（`worker_partition.go`）可以重复执行：在 `drainMu` 内取成员快照，把排空中的每个 Worker 按当前成员改标分区（否则排空中的 Pro Worker 会在最长 `REQUEST_TIMEOUT` + 30 秒内继续给旧分区记让出额度），唤醒等待容量的请求，再记录生效日志，分区超过峰值或独占模式下开启时另记 WARN（文案见 [运行日志](logging.md)）。`retirePass` 一轮开始时按成员快照算出分区，并按它核对能否排空、统计可接替账户、启动替补；`startDrain` 在 `drainMu` 内按当前成员重算，切换恰好落在取快照与写入之间、账号已换了分区时不写入排空（这些判断都是对旧分区做的），登记不推迟，下一轮按新的成员快照重新判断。排空开始之后才切换的，排空开始时的替补在挑号前按排空当前记在的分区（`drainPartition`，`poolRuleChanged` 已改标）挑选；冷却淘汰关闭 Worker 后（`finishRetirement`）按当前成员重算分区再决定替补，替补总补在空出槽位、记着让出额度的那个分区。分区计数、预热目标、空闲回收与容量判断不需要迁移；超出峰值的空闲 Worker 由空闲回收收敛（空闲 5 分钟，每 30 秒检查一次），失去 Pro Worker 的分区由预热补齐，期间浏览器总数可能短暂超过两个分区峰值之和，多出的数量等于换分区的 Pro Worker 数；切换瞬间正在启动的那个 Worker 与 `fillWarm` 的一轮分区快照可能按旧计数放行。等待队列（`dispatchKey`）与主用模型规则（`primaryPolicyFor`）都按请求入口的号池，切换后队列身份不变、只需唤醒；降级记忆、号池级 403 跟踪、账本与访问日志的 `pool` 列同样按请求入口记录，不迁移，号池级 403 的旧记录在 `forbiddenWindow` 内自愈。资源不迁移：`resourcePoolErrorLocked` 按资源所属账户当前的号池判断，绑定在 Pro 账户上的文件、视频与 Veo 操作切换后可能要换入口引用。开启后，独占模式下普通路径（`PoolScopeNormal`）引用返回 `ResourcePoolMismatchError`（400，`Owner` 为 Ultra），要改用 `/ultra`；`ULTRA_EXCLUSIVE=false` 时普通路径为 `PoolScopeAll`、不检查号池，开启不会产生 400。关闭后 `/ultra` 引用返回 400（`Owner` 为普通号池），要改用普通路径。`PreferWarmPool` 仍按真实权益分层轮选、Pro 排在 Ultra 之前，`ULTRA_WARM_WORKER_LIMIT=1` 时唯一常驻的 Ultra 分区 Worker 可能是 Pro，只有 Ultra 订阅可用的模型（access mode 4）每次都要冷启动。同一次保存里还有需要重建生成服务的字段（`requiresRebuild`，如 `AISTUDIO_AUTH_STATES`）时 `applyLiveConfig` 照旧整体提前返回，开关在重建后由 `newRuntime` 生效：`configurePool` 在 `NewAccountPool` 之后、创建 Worker 管理器之前依次设置路由策略、上游通道与 `ULTRA_INCLUDE_PRO`，第一次 Worker 分区与预热目标就按正确的成员计算（`NewAccountPool` 构造时按开关关闭计算成员）。生成服务正在启动时保存，与其他热字段一样由新生成的服务使用启动时读到的配置。

故障重置先等待同账户的活动请求释放租约；等待期间该账户暂停接收新请求。客户端取消只结束自身请求。启动预热在官网 Run 按钮启用后提交，请求在发送前再次检查账户冷却状态。

账户状态：

| 状态 | 含义 |
| --- | --- |
| `ready` | 认证有效且存在可调度容量 |
| `busy` | 就绪账户存在独占操作、认证刷新或活动请求；调度仍按 `PER_ACCOUNT_CONCURRENCY` 判断剩余槽位。需要登录或不可用的账户有活动请求时仍显示原状态 |
| `cooldown` | 账户的全局 `*` 冷却仍有效；模型 `scope`（状态记录范围）冷却只影响对应请求的候选分类 |
| `auth_required` | 账户级认证失败，需要重新登录或续签 |
| `unavailable` | 当前运行时无法使用账户 |
| `disabled` | 账户配置已停用 |

认证结果携带 `authGeneration`（认证状态版本）与 `checkedAt`（检查时间），仅在账户对象、authGeneration 和时间顺序均匹配时应用；同一时间点的成功结果优先于失败。模型成功与冷却写回使用独立 `modelAccessGeneration`（模型状态版本）和 `checked_at`，目录变化时 modelAccessGeneration 递增，同一时间点已有 `verified` 时保留成功状态。

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
| 失效账户 | `GET /api/accounts/startup-failures`、`POST /api/accounts/startup-failures/clear` |
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
