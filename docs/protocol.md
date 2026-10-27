# Google AI Studio 私有协议

本文定义 AIStudio2API 使用的 Google AI Studio 私有协议、认证状态、WAA 运行时、JSON+protobuf 数组、增量事件、工具与媒体链。模型方法、限制和能力由账户的实时 `ListModels` 返回，公开 API 将原始结构投影为规范事件和兼容响应。

## 1. 协议范围、入口与公共头

| 用途 | 入口 | 格式 |
| --- | --- | --- |
| 页面 origin | `https://aistudio.google.com` | HTTPS |
| MakerSuite RPC | `https://alkalimakersuite-pa.clients6.google.com/$rpc/google.internal.alkali.applications.makersuite.v1.MakerSuiteService/<METHOD>` | `application/json+protobuf` |
| WAA RPC | `https://waa-pa.clients6.google.com/$rpc/google.internal.waa.v1.Waa/<METHOD>` | `application/json+protobuf` |
| BotGuard interpreter | `https://www.google.com/js/bg/<INTERPRETER_HASH>.js` | JavaScript |
| Drive 上传 | `https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart&fields=id` | `multipart/related` |
| Drive resumable 上传 | `https://www.googleapis.com/upload/drive/v3/files?uploadType=resumable&fields=id` | 分块 HTTPS body |
| Drive 下载 | `https://www.googleapis.com/drive/v3/files/<FILE_ID>?alt=media` | HTTPS body |

MakerSuite 请求使用以下公共头：

| Header | 来源 |
| --- | --- |
| `content-type` | 固定为 `application/json+protobuf` |
| `user-agent` | 账户固定指纹的 Firefox UA |
| `x-user-agent` | 官网 gRPC-Web 标识 |
| `x-goog-api-key` | AI Studio 首页或当前官网请求动态值 |
| `x-goog-authuser` | 当前账户官网请求 |
| `x-aistudio-visit-id` | 首页初始化或当前官网请求 |
| `x-aistudio-g1-tier` | `GetAiStudioBenefitTier` 返回值映射为 `TIER0`、`TIER1` 或 `TIER2` |
| `x-goog-ext-519733851-bin` | 当前官网请求动态值；纯 Go WAA 后端由 `GetLoggingContext` 编码 |
| `authorization` | 三段 SAPISID 签名 |
| `cookie` | 当前账户对目标 RPC 可见的 Cookie |
| `origin`、`referer` | `https://aistudio.google.com` |
| `accept-language` | 账户 locale |

请求头 `x-goog-api-key` 是 AI Studio 页面使用的动态公共值，与用户创建的 Google Cloud API key 不同；免费网页链仍依赖 Cookie、SAPISID 签名和 WAA proof。

受 WAA 保护的 `GenerateContent` 通过账户固定指纹 Camoufox 页面发送，保留原生 Firefox TLS、HTTP/2、请求头、Cookie 与页面指纹；其他 MakerSuite 与 Drive 请求使用同账户固定出口的 Go HTTP transport。`WAA_BACKEND=go` 时受保护请求由服务进程以 Firefox 152 网络形状经同一固定出口发送，见 [WAA 实现](waa.md)。

JSON+protobuf 使用数组表示 protobuf message。数组索引从 `0` 开始，protobuf field 从 `1` 开始，因此 field `N` 对应索引 `N-1`。Google 响应允许省略空槽并形成 `[,value]`；解码器先把省略槽规范化为 `null`，再从完整 JSON 根值中提取 repeated message。HTTPS chunk 仅提供字节序列，业务事件起止由数组结构确定。

协议核心使用以下 MakerSuite RPC：

| RPC | 用途 |
| --- | --- |
| `ListModels` | 读取模型、方法、限制、默认参数与能力选项 |
| `CountTokens` | 权威输入 token 计数 |
| `GenerateContent` | 文本、思考、函数、Google 工具、图片、语音与音乐 |
| `GenerateAccessToken` | 获取 Drive bearer token |
| `GenerateVideo` | 创建 Veo 长任务 |
| `GetGenerateVideoOperation` | 轮询 Veo 长任务 |

AI Studio 页面初始化还包含以下控制面 RPC：

| RPC | 用途 |
| --- | --- |
| `GetLoggingContext` | 页面日志上下文 |
| `GetUserPreferences` | 用户偏好与欢迎状态 |
| `UpdateUserPreferences` | 更新欢迎状态等用户偏好 |
| `ListPromos` | 页面活动信息 |
| `GetAiStudioBenefitTier` | 账户权益枚举与 tier 请求头 |
| `ListRecentApplets` | 最近 Applet |
| `ListPrompts` | 提示词目录 |
| `GetUserRestrictions` | 账户限制 |

管理进程启动时加载账户并准备公共头。`POST /api/control/start` 刷新实时模型目录、预热 WAA Worker并启用数据面，业务能力随后按需调用对应 RPC。

本文中的数据面指处理公开 API 请求、可通过 Stop/Start 启停的生成服务。

## 2. SAPISID、Chrome DBSC、Cookie 与账户状态

### SAPISID 授权

`authorization` 由三个 Cookie 分别签名：

| 令牌标签 | Cookie |
| --- | --- |
| `SAPISIDHASH` | `SAPISID` |
| `SAPISID1PHASH` | `__Secure-1PAPISID` |
| `SAPISID3PHASH` | `__Secure-3PAPISID` |

三段使用相同的 Unix 秒级时间戳：

```text
source = "<TIMESTAMP> <COOKIE_VALUE> https://aistudio.google.com"
digest = lowercase_hex(SHA1(source))
token = "<LABEL> <TIMESTAMP>_<DIGEST>"
authorization = token_1 + " " + token_2 + " " + token_3
```

MakerSuite 响应的 `Set-Cookie` 在响应头到达时与账户最新 `storage-state.json` 串行合并并原子写回。签名、Cookie 选择和过期判断均以请求时重新读取的账户状态为准。

### Windows Chrome DBSC 导入

Windows Chrome 导入从 Profile 恢复 OAuth 与 Device Bound Session Credentials：

```text
Chrome Local State + Profile Preferences + Web Data/token_service
  -> Gaia ID、v20 refresh token 密文、wrapped binding key
  -> 解开 App-Bound v20 主密钥
  -> AES-256-GCM 解密 refresh token
  -> OAuthMultilogin sentinel 请求取得 DBSC challenge
  -> NCrypt 设备密钥签发 ES256 assertion
  -> X25519/HPKE 解密服务端 Cookie
  -> 保存 Playwright storage state 结构与续签材料
```

`Local State.os_crypt.app_bound_encrypted_key` 使用 Base64 编码并带 `APPB` 前缀。程序把内嵌 ABE helper 加载到独立、隐藏的 Chrome 进程中，取得 32 字节主密钥；临时进程树由 Windows Job Object 管理。`token_service.encrypted_token` 使用 `v20 || nonce[12] || ciphertext+tag`，以该主密钥执行 AES-GCM 解密。

OAuthMultilogin 使用 `MultiOAuth` 头。第一次 assertion 为 `DBSC_CHALLENGE_IF_REQUIRED`，响应提供 challenge；第二次 assertion 的 JWT header 使用 `ES256` 与 `DEVICE_BOUND_SESSION_CREDENTIALS_ASSERTION`。payload 绑定 Google OAuth client、challenge、设备公钥 issuer 和临时 HPKE 公钥。Cookie 密文使用 X25519、HKDF-SHA256 与 AES-128-GCM 解密。

Chrome 导入状态在 `storage-state.json` 的 `aistudio2api` 扩展中保存来源、Gaia ID、refresh token 与 wrapped binding key。用 Cookie 认证的 MakerSuite RPC 返回 HTTP `401` 或协议 Code 16、纯 Go 后端载入首页跳转到 `accounts.google.com`、签名 Cookie 缺失或过期都按登录失效处理，覆盖普通 RPC、受保护 RPC、Live 建连和按需启动 Worker；`Drive*` 的 Bearer 401 与 `GenerateAccessToken` 的 401 不算（后者表示账户没有授权 Google Drive：内联附件改为内联发送，其他需要 Drive 的路径只让这次尝试失败，都不写冷却、不改账户状态，见“Drive 上传与文件 Part”）。有续签材料且平台支持（只有 Windows，其他平台视为没有续签材料）时服务在同一账户出口续签 Cookie、使动态头失效、关闭该账户 WAA runtime，并重放一次。没有续签材料、续签失败或重放仍失效时：生成服务运行中、失效证据只是上游的 401 / Code 16 时，账户先记“疑似登录失效”、写 5 分钟全局冷却，后台用磁盘 Cookie 经 Go HTTP 再发一次 `GetAiStudioBenefitTier`（不续签、不重放）复核，仍是 401 才标为 `auth_required`、释放驻留 Worker，复核通过则解除冷却；登录页跳转、签名 Cookie 缺失这类本地证据，以及生成服务没有运行时，直接标为 `auth_required`。`auth_required` 的账户每 15 分钟一轮低速复核（每轮最多 50 个、并发 2、每秒最多 1 个，按账户退避 15 分钟起到 6 小时，出口限流期间暂停），复核通过即恢复为就绪并重新排进目录同步。单个生成请求因登录失效最多换号 2 次，之后按原错误结束（对外 503）。Worker 启动完成后浏览器的 Cookie 按 (name, domain, path) 合并写回 `storage-state.json`（新值覆盖旧值，浏览器没有导出的保留），生成后的写回同样改为合并。续签先等待同账户其他正常请求结束（最多 12 秒，超过后不再等待、继续续签），并发失效复用一次提交结果；降级判定借用生成租约的计数不做认证恢复。预热没有请求租约，没有续签材料的账户直接标为 `auth_required`，有续签材料的留给按需启动恢复。生成只在正常结束时确认登录有效，正文之后返回的 401 仍写回 `auth_required`；认证结果按认证代际和请求开始时间写回，管理端重新登录或验证之前开始的请求不会覆盖其结果。HTTP `403`、协议 Code 7 与 Drive `unauthorized_client` 保留上游错误，不清除账户或模型成功状态；首个上游语义事件前可以切换到下一个同能力账户。隔离 Camoufox 登录和外部 storage state 不携带 Chrome OAuth 扩展。

`storage-state.json` 保留 Playwright 根字段和未知扩展字段，已定义形状如下。`wrapped_binding_key` 是 Go `[]byte` 的 Base64 JSON 字符串。

```json
{
  "cookies": [
    {
      "name": "<NAME>",
      "value": "<VALUE>",
      "domain": ".example.com",
      "path": "/",
      "expires": -1,
      "httpOnly": true,
      "secure": true,
      "sameSite": "Lax",
      "partitionKey": "<OPTIONAL>"
    }
  ],
  "origins": [
    {
      "origin": "https://example.com",
      "localStorage": [{"name": "<NAME>", "value": "<VALUE>"}]
    }
  ],
  "aistudio2api": {
    "source": {"browser": "chrome", "profile": "<PROFILE>", "email": "<EMAIL>"},
    "oauth": {
      "gaia_id": "<GAIA_ID>",
      "refresh_token": "<REFRESH_TOKEN>",
      "wrapped_binding_key": "<BASE64>"
    }
  }
}
```

Cookie 的 `name`、`value`、`domain`、`path`、`expires`、`httpOnly`、`secure`、`sameSite` 与可选 `partitionKey` 原样持久化；`sameSite` 接受空值、`Lax`、`Strict` 或 `None`。origin 必须包含 scheme 与 host。请求 Cookie 过滤过期项与不匹配的 Secure/domain/path 条目，同名项按 path 长度降序发送；普通 HTTP 响应的 `Set-Cookie` 按 name、domain、path 替换或删除未分区项，同名分区项独立保留。浏览器恢复时，`partitionKey` 映射到 BiDi `storageKey.sourceOrigin`，空值使用默认分区。

localStorage 由站点脚本之前运行的 preload 按 origin 恢复，只写入页面里还没有的键（`getItem` 为 `null` 时才 `setItem`）：每次启动的 Camoufox profile 都是新建的，第一次载入时保存的键全部写入；preload 在每次导航（含页面自己刷新）时都会运行，页面自己写入或原地升级过的值保留，不再被保存的旧值覆盖。这与 Playwright storageState（建上下文时每个 origin 只写一次）不同：之后每次导航仍会补上为 `null` 的键，页面删掉的键（`removeItem` 或 `clear` 之后刷新）会被重新写入；没有在 `sessionStorage` 等处记标记让它只生效一次，因为页面脚本能看到多出来的存储项。WAA Worker 启动完成、发布之前，服务用只读的 `script.evaluate` 读出当前 `https://aistudio.google.com` 页面的全部 localStorage，替换 `storage-state.json` 里这个 origin 的 `localStorage`（浏览器里的值以保存的值为起点、经页面升级，是权威版本），其他 origin 原样保留。写回与 Cookie 写回一样按认证代际判断，启动期间保存过新登录或续签时不写；认证文件被外部换成另一个登录（登录签名 Cookie 与内存不同）、扫描还没载入时 Cookie 与 localStorage 都不写，记下账户交给账户目录扫描载入；浏览器没有导出任何项、与磁盘相同时不写文件；键名与值合计超过 512 KB 时不写。多个进程共用账户目录时，403 无权限账户按认证文件内容判断是否重新上传，这个 origin 的 localStorage 与轮换 Cookie 一样不计入比较。写回只在启动成功后发生：如果保存的旧格式本身就让页面起不来，这些账号拿不到新格式，需要重新登录（登录用全新的浏览器，导出页面新建的 localStorage）。读取或写回失败只记一条 `WAA Worker 启动后写回 localStorage 失败` 警告，不影响启动；日志只写原因与大小，不写 localStorage 的值。纯 Go 后端没有浏览器页面，不写回。

### 账户持久状态

| 文件 | 内容 |
| --- | --- |
| `auth/<Google 邮箱>/account.json` | 邮箱、enabled（缺少该字段按启用读取）、proxy（规范化后的上游 URL，可含账号密码，明文保存，文件 0600）、locale、timezone |
| `auth/<Google 邮箱>/storage-state.json` | Cookie、localStorage 和可选 Chrome 续签材料 |
| `auth/<Google 邮箱>/camoufox-fingerprint.json` | 账户固定的 navigator、屏幕、字体、语言、地区和时区配置 |
| `auth/<Google 邮箱>/runtime-state.json` | 账户权益、实测模型资格、冷却与 Drive/Veo 资源绑定 |
| `auth/<Google 邮箱>/camoufox-cache/` | 该账户 Camoufox 的 HTTP 磁盘缓存（上限 256 MB），使用它的 WAA Worker 持有目录内 `.lock` 独占；`.created` 记录这份缓存的创建时间，超过 3 天的在下次启动拿到锁后先清空再用；页面状态类启动失败后清空（12 小时内新建或清空过的不清，同账户 10 分钟最多一次），见 [开发文档](development.md#账户浏览器缓存) |
| `auth/.leases/<Google 邮箱>.lock` | 同一账户目录的跨进程占用锁 |
| `auth/.leases/<Google 邮箱>.runtime.lock` | 每次 `runtime-state.json` 读取、合并与写回的短事务锁 |
| `[用户缓存]/AIStudio2API/runtime-leases/<Google 邮箱>.lock` | 当前电脑上该邮箱的 WAA Worker 占用锁 |

Google 邮箱的小写形式同时作为账户目录、管理页面标识和日志来源。新账户的 locale 与 timezone 读取当前电脑设置，管理页面使用浏览器语言和 IANA 时区；CLI 使用操作系统语言和时区。初始化、WAA、MakerSuite、OAuth 续签和 Drive 使用账户固定代理。代理带账号密码时，Camoufox（WAA Worker、隔离登录与登录验证）经 `127.0.0.1` 随机端口上的本机中转连接上游代理（Firefox prefs 没有账号密码字段），隧道内字节不变；Go 侧请求（MakerSuite、Drive、WebChannel、Go WAA、Chrome OAuth 导入与续签）直接带认证连接上游，两条路径的出口相同；不带账号密码的代理不经过中转，见 [开发文档](development.md#本机代理中转)。locale 同时设置 navigator language、Accept-Language 与地区，timezone 设置浏览器时区；重新登录和 WAA runtime 复用同一账户指纹。同一电脑上的多个进程按邮箱共享 WAA runtime lease，调度器只会为未被占用的邮箱创建 Worker。

`runtime-state.json` 根字段为 `cooldowns`、`resources`、`model_access`、`benefit_tier`、`catalog_fingerprint`。cooldown value 为 `{until,reason?}`；model access value 为 `{state,checked_at,reason?}`；resource value 为 `{kind?,name?,mime?,size?,purpose?,created_at,video?:{model,seconds,size}}`。

每次运行状态事务先取得 `auth/.leases/<账户>.runtime.lock`，每 25ms 尝试一次，等待上限为 2 秒；携带调用方 context（Go 取消上下文）的事务在调用方取消或 deadline 更早到达时立即结束。取得锁后重新读取当前磁盘值，只合并目标字段，以临时文件原子替换 `runtime-state.json`，再同步内存状态与资源归属。锁等待失败、读取失败、写回失败和 unlock 失败均返回原始错误链。

调度器按实时模型方法、capability、AccessModes 和账户权益选择候选，优先使用已经成功调用目标 scope（模型资格与冷却的状态记录范围）的预热账户，再按目标模型最近一次真实首事件耗时排序。Code 7 保留已有账户和模型成功记录。常驻 Worker 优先覆盖可调用模型更多的账户。预热数量低于上限时提升待机账户，预热账户均忙时等待并发槽位。同账号 WAA proof 串行生成，已准备的 MakerSuite HTTP 请求并发执行；首个活动请求获取 `.leases/<邮箱>.lock`，最后一个释放。Drive file、Veo operation 与产物 file 始终使用创建账户。

### 账户权益

`GetAiStudioBenefitTier` 请求为 `[]`，响应 field 1 的枚举映射如下。`[]`、`[null]` 和 `[0]` 均表示 Free；响应存在后续字段时，权益仍由 field 1 决定。

| 值 | 权益 | RPC Header |
| ---: | --- | --- |
| 0 | Free | 无 |
| 1 | Pro | `X-AIStudio-G1-Tier: TIER1` |
| 2 | Ultra | `X-AIStudio-G1-Tier: TIER2` |
| 3 | Plus | `X-AIStudio-G1-Tier: TIER0` |

官网为 `GenerateContent`、`CountTokens`、Interaction、Code Assistant 与 Veo RPC 注入该 header。模型 field 83 描述访问方式：`1` 为付费 API key，`3` 为 Pro/Ultra 订阅，`4` 为 Ultra 订阅。公开模型目录合并全部账户的实时 `ListModels` 记录；账户调度使用 AccessModes、BenefitTier 与成功调用历史选择具体账户。号池划分（见下段）不改变权益：`ULTRA_INCLUDE_PRO=true` 时并入 Ultra 号池的 Pro 账户仍发 `X-AIStudio-G1-Tier: TIER1`，access mode `3` 的模型 Pro 与 Ultra 账户都可服务，只有 access mode `4`（Ultra 订阅）可用的模型仍只由 Ultra 账户服务。

账户按当前权益与 `ULTRA_INCLUDE_PRO` 分为两个号池：权益为 Ultra 的账户，以及 `ULTRA_INCLUDE_PRO=true` 时权益为 Pro 的账户，组成 Ultra 号池（付费号池），其余账户（含尚未读取权益的账户，`benefit_tier_known=false`）组成普通号池。`/ultra` 前缀的请求只在 Ultra 号池内选号；`ULTRA_EXCLUSIVE=true`（默认）时普通路径只在普通号池内选号，为 `false` 时普通路径不限号池。号池限定作用于全部选号、模型存在性校验、公开模型目录与别名解析、换号、通道回退、文件复制与上传候选；不和请求关联的预热、轮换、登录检查不限号池。权益在目录同步或运行状态刷新后变化、或 `ULTRA_INCLUDE_PRO` 保存后切换时，账户立即按新的划分归入号池（开关不需要重启生成服务）。`ULTRA_EXCLUSIVE=true` 且 `ULTRA_INCLUDE_PRO=true` 时普通路径不再使用 Pro 账户：只有 Pro、Ultra 账户能服务的模型（access mode `3`、`4`；只在 Build 通道提供的模型如 `gemini-4-argon` 不按权益判断，只看哪些账户的 Build 目录列出它，普通号池里没有这样的账户时同样如此）不再出现在普通路径的公开模型目录里，普通路径上输入超限后的更高权益重试也不会再选到 Pro 账户。Worker 按同样的划分计入普通分区或 Ultra 分区，两个分区分别受 `WARM_WORKER_LIMIT`/`MAX_ACTIVE_WORKERS` 与 `ULTRA_WARM_WORKER_LIMIT`/`ULTRA_MAX_ACTIVE_WORKERS` 约束，淘汰与轮换不跨分区。

## 3. WAA challenge、官方 VM 与 fresh proof

受保护请求使用以下链路：

```text
Waa/Create
  -> decode challenge
  -> load interpreter by current hash
  -> initialize official VM lifecycle
  -> SHA-256(binding prompt) as lowercase hex
  -> snapshot({TYb:{content:<DIGEST>}})
  -> write fresh proof into request
  -> send protected MakerSuite RPC
```

`WAA_BACKEND=camoufox` 时官方 VM 运行在账户固定指纹的 Camoufox 页面，受保护请求由页面原生 `fetch` 发送；`WAA_BACKEND=go` 时 VM 运行在服务进程内的 goja 与 Firefox 形状宿主中，受保护请求由 Go HTTP 以 Firefox 152 网络形状发送。bootstrap、challenge 解码、解释器、初始化参数、宿主、生命周期、失败处理与上游变化的定位方法见 [WAA 实现](waa.md)。

`Waa/Create` 请求是 JSON+protobuf 数组。Worker 启动时只带 request key，VM 刷新时追加当前 interpreter hash 与上一 VM 的无绑定 snapshot：

```json
["lmnUSbltwc5ULv48iKLX"]
["lmnUSbltwc5ULv48iKLX", "<INTERPRETER_HASH>", "<PREVIOUS_SNAPSHOT>"]
```

响应外层索引 `1` 是 Base64 challenge，解码后每个字节加 `97`，得到 message ID、interpreter、program、全局函数名与 client experiments 状态。interpreter 按 hash 缓存，摘要为 SHA-256 的 Base64URL 无 padding 编码；program 属于当前 Create 生命周期，proof 绑定当前 prompt 摘要与 VM 内部状态，每个请求生成新的 proof。

snapshot 的底层输入是四槽数组，首槽承载 binding：

```javascript
[{content: sha256(bindingPrompt)}, undefined, undefined, undefined]
```

返回值是 `!` 开头的 proof。`GenerateContent` 与 `CreateInteractionStream` 写入 field 5，Build 代理写入 field 3，`GenerateVideo` 写入 field 8，Bidi 的每个客户端 wire 写入 field 6；原请求的其他槽位保持不变。各 RPC 的 binding prompt 见 [WAA 实现](waa.md)，GenerateContent 按 contents 和 parts 原顺序以单个空格连接。

`Waa/Ping` 请求和成功响应：

```json
["lmnUSbltwc5ULv48iKLX", "<BOTGUARD_RESPONSE>"]
[]
```

field 1 是 `request_key`，field 2 是 `botguard_response`。正确 proof、损坏 proof、省略 field 2、错误 request key 与无账户认证均可返回 HTTP 200 和 `[]`。Ping 成功表示 WAA RPC、API consumer identity 与字段类型可达；Worker VM、snapshot proof、binding、账户会话、模型资格和 GenerateContent 接受状态由实际受保护业务 RPC 判定。

生成服务启动时按配置的常驻数与启动并发数预热账户 WAA Worker。一个账户 Worker 为该账户的所有普通生成模型提供 proof，业务模型切换直接复用当前 Worker；同一账户的 snapshot 串行执行。

## 4. ListModels、CountTokens 与 GenerateContent 请求

### ListModels

请求正文：

```json
[]
```

响应根形状为 `[[<MODEL_ROW>, ...]]`。模型列表位于 field 1，后续根字段不改变模型目录。模型行字段：

| JSON 索引 | protobuf field | 内容 |
| ---: | ---: | --- |
| 0 | 1 | `models/<MODEL_ID>` |
| 2 | 3 | 版本 |
| 3 | 4 | 显示名称 |
| 4 | 5 | 描述 |
| 5 | 6 | 输入 token 上限 |
| 6 | 7 | 输出 token 上限 |
| 7 | 8 | 支持的方法 |
| 8 | 9 | 默认 temperature |
| 9 | 10 | 默认 topP |
| 10 | 11 | 默认 topK |
| 56 | 57 | 模型别名 |
| 64 | 65 | 主能力码 |
| 66 | 67 | TTS voice 列表 |
| 70 | 71 | Veo 配置 |
| 71 | 72 | thinking 默认配置 |
| 74 | 75 | 次能力码 |
| 75 | 76 | 图片宽高比码 |
| 76 | 77 | 图片输出分辨率码 |
| 77 | 78 | Paid 标记，值 `2` 时显示 Paid |
| 78 | 79 | Interaction 配置：索引 2 为后台任务，索引 3 类型 `1` 为 agent、`2` 为模型 |
| 82 | 83 | 模型访问方式 |

能力码映射：

| 码 | 能力 | 码 | 能力 |
| ---: | --- | ---: | --- |
| 1 | chat model | 9 | code execution |
| 10 | function declarations | 12 | Google Search |
| 13 | URL Context | 20 | Veo route |
| 21 | image route | 25 | thinking |
| 26 | live route | 35 | thinking budget |
| 37 | speech route | 43 | media resolution |
| 47 | aspect ratio | 49 | output resolution |
| 52 | thinking level | 53 | music route |
| 54 | image search | 58 | Google Maps |
| 59 | private Interaction route | 85 | TTS 分段说话人 |
| 46 | Live 实时翻译 | | |

未知能力码按原值保留为 `capability_code_<N>` 或 `secondary_capability_code_<N>`。

公开模型对象为已知主能力码增加以下语义键：

| 码 | capability 键 | 码 | capability 键 |
| ---: | --- | ---: | --- |
| 1 | `chat_model` | 9 | `code_execution` |
| 10 | `function_declarations` | 12 | `google_search` |
| 13 | `browse` | 20 | `video_route` |
| 21 | `image_route` | 25 | `thinking` |
| 26 | `live_route` | 35 | `thinking_budget` |
| 37 | `speech_route` | 43 | `media_resolution` |
| 47 | `aspect_ratio` | 49 | `output_resolution` |
| 52 | `thinking_level` | 53 | `music_route` |
| 54 | `image_search` | 58 | `google_maps` |
| 59 | `interaction_route` | 74 | `transcription_word_timestamps` |
| 76 | `transcription_language_codes` | 77 | `transcription_output` |
| 80 | `transcription_speaker_labels` | 81 | `transcription_custom_vocabulary` |
| 84 | `transcription_smart` | 85 | `speech_metadata` |
| 46 | `speech_translation` | | |

每个主能力码都保留为 `capability_code_<N>`，同时为上表中的已知码增加语义键；次能力码保留为 `secondary_capability_code_<N>`。

图片与视频选项使用枚举码：

| 类型 | 码值映射 |
| --- | --- |
| 图片/视频宽高比 | `1=1:1`、`2=9:16`、`3=16:9`、`4=3:4`、`5=4:3`、`6=3:2`、`7=2:3`、`8=5:4`、`9=4:5`、`10=21:9`、`11=9:21`、`12=1:4`、`13=4:1`、`14=1:8`、`15=8:1` |
| 图片分辨率 | `1=1K`、`2=2K`、`3=4K`、`4=512` |
| 视频时长 | `1=5s`、`2=6s`、`3=7s`、`4=8s`、`5=4s` |
| 视频分辨率 | `1=720p`、`2=1080p`、`3=4k`、`4=368p`、`5=360p` |

Veo field 71 的宽高比、时长和分辨率分别位于子索引 `4`、`5`、`9`；子索引 `7` 含 `3` 而子索引 `9` 没有列出分辨率时，`video_output_resolutions` 取官网提供的 `720p`、`1080p`、`4k`（与上游 Mag1cFall/AIStudio2API 相同）；子索引 `7` 含 `4` 时模型接受起始帧与尾帧，含 `5` 时接受参考图，公开为 `video_inputs` 的 `frames` 与 `reference_images`。Omni 的 field 71 子索引 `10` 为时长滑块的下限、上限、步长与默认值（秒），公开为 `video_duration_range` 的 `min=`、`max=`、`step=`、`default=` 四项；子索引 `11` 为帧率，`1` 公开为 `24fps`；field 85 为视频任务，`1`–`5` 公开为 `text_to_video`、`image_to_video`、`reference_to_video`、`edit`、`extend`。这四项（`video_inputs`、`video_duration_range`、`video_frame_rates`、`video_tasks`）取值不是整数数组时只省略该项，不让整个目录解析失败；每个账户原始目录里 `video_output_resolutions` 的第一项为该账户 Omni 的默认分辨率。TTS field 67 是 repeated voice row，每行索引 `0` 为 voice name。thinking field 72 的默认 level 位于子索引 `5`。

field 57 alias 可以是 `["models/<ALIAS>"]`，也可以是 repeated row；row 形状时取每行索引 `0` 并移除 `models/` 前缀。公开 `capability_options` 的键全集为 `aliases`、`voices`、`image_aspect_ratios`、`image_output_resolutions`、`video_aspect_ratios`、`video_durations_seconds`、`video_output_resolutions`、`video_inputs`、`video_duration_range`、`video_frame_rates` 和 `video_tasks`；没有值的键省略。公开的目录（`/v1/models`、`/v1beta/models` 与管理页模型详情）是各账户目录的合并结果，只有一个账户时也一样：每个键的取值按字符串排序去重，不代表上游顺序。Omni 的默认分辨率取所选账户原始目录 `video_output_resolutions` 的第一项（官网一般为 `360p`），在公开列表里看不出。`video_duration_range` 合并后同名项可能出现多次，使用时按数值取各账户都能接受的范围：下限取最大值、上限取最小值、步长取最大值，默认值取落在范围内的最大一个（都不在范围内时换成范围内最接近的）；范围为空时协议层不调整时长，交给发送前按所选账户的目录调整。Omni 的公开目录示例：

```json
"capability_options": {
  "video_aspect_ratios": ["16:9", "9:16"],
  "video_output_resolutions": ["1080p", "360p", "4k", "720p"],
  "video_duration_range": ["default=10", "max=15", "min=4", "step=1"],
  "video_frame_rates": ["24fps"],
  "video_tasks": ["edit", "extend", "image_to_video", "reference_to_video", "text_to_video"]
}
```

示例中的时长范围为示意，以实时目录为准。

### CountTokens

纯文本且无 system：

```json
["models/<MODEL_ID>", [<CONTENT>, ...]]
```

含 system、inline data、外部媒体或 Drive file：

```json
["models/<MODEL_ID>", null, ["models/<MODEL_ID>", [<CONTENT>, ...], null, null, null, <SYSTEM>]]
```

请求形状选择：

| 条件 | 根结构 | GenerateContent 子消息位置 |
| --- | --- | --- |
| 纯文本 contents | `[model, contents]` | — |
| system instruction | `[model, null, generate]` | `$[2][5]` |
| function / Google tools | `[model, null, generate]` | `$[2][6]` |
| inline data、external media、Drive、function call/result、code result | `[model, null, generate]` | `$[2][1]` |

包含 system 与函数声明的完整计数请求：

```json
[
  "models/gemini-3.6-flash",
  null,
  [
    "models/gemini-3.6-flash",
    [
      [
        [[null, "调用 ping 检查服务"]],
        "user"
      ]
    ],
    null,
    null,
    null,
    [
      [[null, "你是诊断助手"]],
      "user"
    ],
    [
      [null, [["ping", "检查服务"]]]
    ]
  ]
]
```

响应为单元素数组：

```text
[<INPUT_TOKEN_COUNT>]
```

索引 `0` 是权威输入 token 数。其他槽按不透明协议字段保留。

### Content、Part 与 system

Content 形状：

```json
[[<PART>, ...], "user|model"]
```

带 finish reason 的模型完成帧可以使用 `[null,"model"]`，该帧只提供终止状态与 usage。

客户端 tool result 使用 `user` role。Part 字段：

| JSON 索引 | protobuf field | 内容 |
| ---: | ---: | --- |
| 1 | 2 | 文本 |
| 2 | 3 | inline data `[mime, base64]` |
| 5 | 6 | Drive file `[fileId]` |
| 6 | 7 | 外部媒体 `[mime, url]` |
| 7 | 8 | executable code `[languageCode, code]` |
| 8 | 9 | code execution result `[outcomeCode, output]` |
| 10 | 11 | function call `[name, Struct, callId?]` |
| 11 | 12 | function result `[name, Struct, callId?]` |
| 12 | 13 | thought boolean |
| 14 | 15 | thought signature |
| 22 | 23 | transcription metadata `[text, speaker?, timestamp spans?]` |

system instruction：

```json
[[[null, "<SYSTEM_TEXT>"]], "user"]
```

### GenerateContent

根消息字段：

| JSON 索引 | protobuf field | 内容 |
| ---: | ---: | --- |
| 0 | 1 | `models/<MODEL_ID>` |
| 1 | 2 | contents |
| 2 | 3 | safety settings |
| 3 | 4 | generation config |
| 4 | 5 | fresh WAA proof |
| 5 | 6 | system instruction |
| 6 | 7 | tools |
| 10 | 11 | 固定值 `1` |
| 13 | 14 | `[[null,null,<TIMEZONE>]]` |
| 14 | 15 | 用户 Cloud API key，免费网页链保持 `null` |

safety settings：

```json
[
  [null, null, 7, 5],
  [null, null, 8, 5],
  [null, null, 9, 5],
  [null, null, 10, 5]
]
```

每项为 `[null, null, 类别, 阈值]`，编号与 Gemini API 枚举相同：类别 `HARM_CATEGORY_HARASSMENT`=7、`HATE_SPEECH`=8、`SEXUALLY_EXPLICIT`=9、`DANGEROUS_CONTENT`=10、`CIVIC_INTEGRITY`=11；阈值 `BLOCK_LOW_AND_ABOVE`=1、`BLOCK_MEDIUM_AND_ABOVE`=2、`BLOCK_ONLY_HIGH`=3、`BLOCK_NONE`=4、`OFF`=5。非图片模型默认发送上面 7–10 四类 `OFF`，Gemini 协议请求中的类别按名称（不区分大小写）覆盖或追加，未列出的类别保持 `OFF`；图片模型只发送请求中的类别，没有时为 `null`。上游对 `HARM_CATEGORY_UNSPECIFIED`（0）与 `DEROGATORY` 至 `DANGEROUS`（1–6）返回 400，这些类别与其他未知类别或阈值在选号前跳过并记一条 WARN（最多逐条列出 8 条、其余计数，过长的名称截断；排查记录同时写入时间线），其余设置照常发送；阈值 `HARM_BLOCK_THRESHOLD_UNSPECIFIED` 沿用默认值。

generation config 字段：

| JSON 索引 | protobuf field | 内容 |
| ---: | ---: | --- |
| 1 | 2 | stop sequences |
| 3 | 4 | max output tokens |
| 4 | 5 | temperature |
| 5 | 6 | topP |
| 6 | 7 | topK |
| 7 | 8 | response MIME type |
| 8 | 9 | response schema |
| 13 | 14 | 固定值 `1` |
| 14 | 15 | response modalities：TEXT=`1`、IMAGE=`2`、AUDIO=`3` |
| 15 | 16 | speech config |
| 16 | 17 | thinking config `[1, budget?, null, level]` |
| 17 | 18 | media resolution：LOW=`1`、MEDIUM=`2`、HIGH=`3` |
| 18 | 19 | seed |
| 26 | 27 | image config `[aspectRatio?, imageSize?]` |
| 31 | 32 | transcription config |

生成参数校验：

| 参数 | 默认来源 | 有效值 |
| --- | --- | --- |
| max output | ListModels field 7 | `1..model.outputTokenLimit` |
| temperature | ListModels field 9 | `0..2` |
| topP | ListModels field 10 | `0..1` |
| topK | ListModels field 11 | 非负整数 |
| thinking level | ListModels field 72 | Low=`1`、Medium=`2`、High=`3`、Minimal=`4` |
| thinking budget | 请求值 | 模型能力码包含 thinking budget |

`reasoning_effort` / `thinkingLevel` / Anthropic `output_config.effort` 接受 `none`、`minimal`、`low`、`medium`、`high`。`none` 在只支持 thinking budget 的模型使用预算 0，支持 thinking level 的模型使用最低可用 level，不支持思考的模型忽略该值。模型只有 thinking budget 能力时，显式 effort 需要同时提供 budget；模型只有 thinking level 能力时，显式 budget 退化为 level。两类能力都缺少时返回参数错误。

response modalities：

`wire` 指发送给上游的原始数组字段。

| 输出 | wire | 默认路由 |
| --- | --- | --- |
| text | `[1]` | chat |
| image | `[2]` | image route |
| image + text | `[2,1]` | 显式组合请求 |
| audio | `[3]` | speech / music route |

AUDIO 采用独立输出模态。JSON Schema type code 为 string=`1`、number=`2`、integer=`3`、boolean=`4`、array=`5`、object=`6`；schema 支持 format、description、nullable、enum、items、properties、required 和 field 23 `propertyOrdering`。

以下最小组合请求包含 system、文本、函数声明、generation config、WAA proof 与账户时区。连续空槽保持在同行，字段含义查上表：

```json
[
  "models/gemini-3.6-flash",
  [
    [
      [[null, "调用 ping 检查服务"]],
      "user"
    ]
  ],
  [
    [null, null, 7, 5],
    [null, null, 8, 5],
    [null, null, 9, 5],
    [null, null, 10, 5]
  ],
  [null, null, null, 512, 0.2, 0.95, 40, null, null, null, null, null, null, 1],
  "!WAA_PROOF",
  [
    [[null, "你是诊断助手"]],
    "user"
  ],
  [
    [null, [["ping", "检查服务"]]]
  ],
  null,
  null,
  null,
  1,
  null,
  null,
  [[null, null, "Asia/Taipei"]]
]
```

## 5. 增量流、思考、usage、来源与错误

`GenerateContent` 返回持续增长的 JSON+protobuf 根数组，根索引 `0` 是 repeated frames。帧结构：

| 路径 | 内容 |
| --- | --- |
| `$[0][frame][0]` | candidates |
| `$[0][frame][0][0][0]` | candidate content |
| `$[0][frame][0][0][1]` | finish reason code |
| `$[0][frame][0][0][3]` | finish message（上游对结束原因的说明，如 `MALFORMED_FUNCTION_CALL` 的具体问题；不是字符串时忽略） |
| `$[0][frame][0][0][6]` | citations |
| `$[0][frame][0][0][7]` | grounding metadata |
| `$[0][frame][2]` | usage |
| `$[0][frame][7]` | response ID |
| `$[0][frame][3]` 且 frame 0 为空 | interaction metadata |
| `$[0][frame][1]` 且 frame 0 为 null | prompt feedback：`[0]` 为 block reason code，`[2]` 为拦截说明（block reason message，不是字符串时忽略） |

传输正文是一个 JSON 根值，网络 chunk 提供字节；解码器在 `$[0]` 中每出现一个完整 repeated frame 时立即消费该 frame。每个内容帧包含一个 candidate，candidate content 为 `[[parts...], "model"]`。完成帧可以同时携带最后一组 Part、usage、response ID 和 finish reason，根数组解析完成后结束读取。

从 `$[0]` 提取出的文本帧：

```json
[
  [
    [
      [
        [[null, "42"]],
        "model"
      ]
    ]
  ]
]
```

随后到达的完成帧包含 `finish=1`、usage 和 response ID：

```json
[
  [[null, 1]],
  null,
  [27, 1, 28, null, null, null, null, 0, null, 0],
  null,
  null,
  null,
  null,
  "response_01"
]
```

高频路径速查：

| 结构 | JSONPath | 内容 |
| --- | --- | --- |
| GenerateContent | `$[0]` | model |
| GenerateContent | `$[1]` | contents |
| GenerateContent | `$[3]` | generation config |
| GenerateContent | `$[4]` | WAA proof |
| GenerateContent | `$[5]` | system instruction |
| GenerateContent | `$[6]` | tools |
| GenerateContent | `$[13][0][2]` | timezone |
| response root | `$[0][frame]` | repeated frame |
| candidate content | `$[0][frame][0][0][0]` | `[[parts], "model"]` |
| candidate finish | `$[0][frame][0][0][1]` | finish reason code |
| Part text | `...parts[part][1]` | text |
| Part inline data | `...parts[part][2]` | `[mime, base64]` |
| Part function call | `...parts[part][10]` | `[name, Struct, callId?]` |
| Part thought | `...parts[part][12]` | boolean |
| Part signature | `...parts[part][14]` | signature |
| frame usage | `$[0][frame][2]` | usage array |
| frame response ID | `$[0][frame][7]` | response ID |

Part 文本带 `part[12]=true` 时属于 reasoning summary，普通文本属于可见正文；带 `part[12]=true` 的内联图片是图片模型思考过程中的草图，不作为输出媒体返回，最终图片以普通 Part 另行返回；`part[14]` 是 thought signature。签名可以附在文本、函数调用或独立空 Part 上，下一轮必须原样回传：

| 公开协议 | 签名输入 | 签名输出 |
| --- | --- | --- |
| OpenAI Chat | assistant tool call 的 `extra_content.google.thought_signature` | tool call 的同名扩展字段 |
| OpenAI Responses | `reasoning.encrypted_content` 紧邻后续 `function_call` | reasoning item 的 `encrypted_content` |
| Anthropic | `thinking` 或 `redacted_thinking` block 的 `signature` | thinking block 的 `signature` |
| Gemini | 数据 Part 或独立 Part 的 `thoughtSignature` | Part 的 `thoughtSignature` |

OpenAI Chat 的 assistant tool call 未带 `extra_content` 时，服务按调用 ID、函数名和参数补回本进程最近签发的签名；查不到时写入 `skip_thought_signature_validator`。

Anthropic redacted thinking block 以 `data` 承载同一份不透明状态；适配器在输入与输出两侧保留该值。正文块打开时单独到达的签名（流式与非流式）先暂存，正文块结束后按到达顺序输出为 `redacted_thinking`，与上一个相同的签名只输出一次，正文不会被切成多个文本块；随后是不带签名的工具调用时排在该调用之后输出（紧挨在 `tool_use` 前的 `redacted_thinking` 回传时会成为该调用的签名）。正文分片自带的签名不输出。

reasoning summary 是服务端返回的摘要文本。thought signature 作为下一轮请求的协议状态字段原样回传。

协议核心按网络顺序输出 `text`、`reasoning`、`tool_call`、`executable_code`、`code_execution_result`、`grounding`、`citation`、`media`、`thought_signature`、`usage`、`finish` 和 `error`。

### Grounding 与引用

grounding metadata 字段：

| JSON 索引 | 内容 |
| ---: | --- |
| 0 | search entry point `[renderedContent?, sdkBlob?]` |
| 1 | grounding chunks |
| 2 | grounding supports |
| 3 | retrieval metadata，动态分数位于子索引 1 |
| 4 | web search queries |
| 5 | 第二个 repeated web search query 槽 |
| 6 | Maps widget context token |

索引 `4`、`5` 按槽位与元素顺序合并并去重。

`oneof` 表示同组字段中最多选择一种值。

grounding chunk 的 oneof 索引 `0/1/2` 分别为 web、retrieved context、maps；内部字段依次为 URI、title、text、place ID。support 为 `[segment, chunkIndices, confidenceScores]`，segment 为 `[partIndex,startIndex,endIndex,text]`。candidate citations 的 entries 位于 metadata 索引 0，每项 URL 在索引 2、title 在索引 3。

包含 web chunk、maps chunk、正文 support、检索分数和查询词的 raw metadata：

```json
[
  ["<div>Search results</div>", "SDK_BLOB"],
  [
    [["https://example.com/gemini", "Gemini Guide", "Protocol overview"]],
    [null, null, ["https://maps.google.com/?cid=1", "Google Taipei", "", "ChIJ_demo"]]
  ],
  [
    [[0, 0, 12, "Gemini Guide"], [0], [0.98]]
  ],
  [null, 0.91],
  ["Gemini AI Studio protocol"],
  null,
  "MAPS_WIDGET_CONTEXT_TOKEN"
]
```

Code Execution 的 language code 为 `0=LANGUAGE_UNSPECIFIED`、`1=PYTHON`。执行结果 outcome code 为 `0=OUTCOME_UNSPECIFIED`、`1=OUTCOME_OK`、`2=OUTCOME_FAILED`、`3=OUTCOME_DEADLINE_EXCEEDED`。

### Usage

完成帧 usage：

| 数组索引 | 语义 | 规范字段 |
| ---: | --- | --- |
| 0 | input tokens | `input_tokens` |
| 1 | visible output tokens | `output_tokens` |
| 2 | total tokens | `total_tokens` |
| 7 | tool tokens | `tool_tokens` |
| 9 | thought tokens | `reasoning_tokens` |

完整 usage 直接按上游原值返回。完成帧省略 visible output tokens 时，服务按上游 total 与其余分类字段恢复该值。完整 usage 缺失时，内置 Gemini SentencePiece tokenizer 在本地统计可观测输入、工具声明、reasoning summary 和实际输出。

OpenAI 与 Anthropic 的输入统计为 input + tool，输出统计为 visible output + reasoning。Gemini 分别投影 `promptTokenCount`、`candidatesTokenCount`、`thoughtsTokenCount` 与 `totalTokenCount`。隐藏思考用量来自上游 usage field 9；本地 fallback 统计服务端返回的 reasoning summary。

Anthropic 流式 `message_start` 写入即时输入估算，最终 `message_delta` 使用完成 usage 覆盖为权威输入与输出统计。

携带 stop sequence 的请求并行执行同账户 `CountTokens`。正文匹配实际序列时，协议核心关闭生成流，使用 `CountTokens` 的输入总数以及已输出的正文、reasoning 和工具统计构造最终 usage；计数失败时仍返回 `stop_sequence` 终态并省略 usage。

### Finish 与错误

| code | reason | code | reason |
| ---: | --- | ---: | --- |
| 0 | unspecified | 1 | stop |
| 2 | max_tokens | 3 | safety |
| 4 | recitation | 5 | other |
| 6 | language | 7 | blocklist |
| 8 | prohibited_content | 9 | spii |
| 10 | malformed_function_call | 11 | image_safety |
| 12 | unexpected_tool_call | 13 | too_many_tool_calls |
| 14 | image_prohibited_content | 15 | image_other |
| 16 | no_image | 17 | image_recitation |
| 18 | missing_thought_signature | 19 | `provider_19` |
| 其他整数 | `provider_<code>` | | |

错误响应根形状为 `[null,[code,message,...]]`。协议核心保留 HTTP 状态、协议 code 与 message；公开适配器映射为 OpenAI、Anthropic 或 Gemini 错误对象。Chat、Responses、Anthropic Messages 与 Gemini GenerateContent 将媒体模型的普通文本作为文本结果输出；专用图片端点要求图片结果。HTTP/协议错误或缺失完成帧形成失败；上游 finish reason 作为正常终态保留并映射到各公开协议。

frame 0 为 null 且带 prompt feedback 时表示上游拒绝当前输入（Build 为没有 candidate 而带 `promptFeedback.blockReason`），形成输入拦截错误。上游给出的拦截说明（Playground 的 prompt feedback `[2]`、Build 的 `promptFeedback.blockReasonMessage`）只附在内部错误与管理日志的 `request.error` 中（`AI Studio 拒绝当前输入: <reason>: <说明>`）；公开协议的错误消息仍按 block reason 统一生成官方措辞，不含这段原文。

各协议的具体终态转换如下：

| 上游终态 | OpenAI Chat | OpenAI Responses | Anthropic Messages | Gemini GenerateContent |
| --- | --- | --- | --- | --- |
| `stop` 且包含函数调用 | `tool_calls` | `completed`，保留 function call item | `tool_use` | `STOP`，保留 functionCall Part |
| `stop_sequence` | `stop` | `completed` | `stop_sequence` 与实际序列 | `STOP` |
| `max_tokens` | `length` | `incomplete/max_output_tokens` | `max_tokens` | `MAX_TOKENS` |
| 内容拦截：`safety`、`recitation`、`blocklist`、`prohibited_content`、`spii`、`image_safety`、`image_prohibited_content`、`image_recitation` | `content_filter` | `incomplete/content_filter` | `refusal` | 对应 Gemini 枚举 |
| 其他非标准终态：`malformed_function_call`、`unexpected_tool_call`、`too_many_tool_calls`、`missing_thought_signature`、`other`、`unspecified`、`provider_*` 等 | `stop`（含函数调用时 `tool_calls`） | `completed` | `end_turn`（含函数调用时 `tool_use`） | 对应 Gemini 枚举或 `OTHER` |

内容拦截终态优先于同一结果中的工具调用终态，已产生的正文、reasoning、工具事件和 usage 保持在响应中。非标准终态（包括内容拦截与 `provider_*`）在 Chat choice、Responses response、Anthropic message 或 `message_delta` 的 `provider_finish_reason` 中保留原值。Gemini 的 `finishMessage` 优先原样使用上游给出的结束说明（Playground 的 candidate `[3]`、Build 的 `finishMessage`）；上游没有给出时，`missing_thought_signature` 写 `Missing thought signature`，`provider_*` 写 `AI Studio finish reason <code>` 保留编号。OpenAI、Responses、Anthropic 与 Interactions 不携带这段说明。上游给出结束说明时，管理日志另记一条 `上游结束说明`（见 [logging.md](logging.md)）。`provider_19` 对应 AI Studio 页面的 `Content blocked`。

## 6. 函数、Google 工具、Drive 与媒体

### 函数与 Google 工具

根 field 7 是 repeated Tool：

| 工具 | Tool 数组形状 |
| --- | --- |
| Function declarations | `[null, [[name, description?, schema?], ...]]` |
| Code Execution | `[[]]` |
| Google Search | `[null,null,null,[null,[searchTypes]]]`，searchTypes 索引 0 为 `[]` |
| Image Search | 同一 Search tool，searchTypes 索引 1 为 `[]` |
| URL Context | 8 槽数组，索引 7 为 `[]` |
| Google Maps | 11 槽数组，索引 10 为 `[]` |

Search tool 的 index `3` 是 `[timeRange?,searchTypes]`。timeRange 为 `[start?,end?]`，每个时间值编码为 `["<UNIX_SECONDS>"]`；searchTypes 的索引 `0/1` 分别启用 web 与 image search。

公开工具名称归一化后再生成上述 Tool 数组：

| AI Studio 工具 | OpenAI Chat / Responses | Anthropic | Gemini |
| --- | --- | --- | --- |
| function declarations | `function` | 空 type 或 `custom` | `functionDeclarations` |
| Google Search | `web_search`、`web_search_preview` | `web_search*` | `googleSearch`、`googleSearchRetrieval` |
| Image Search | `image_search` | `image_search` | `imageSearch` |
| URL Context | `url_context` | `web_fetch*`、`url_context*` | `urlContext` |
| Code Execution | `code_interpreter` | `code_execution*` | `codeExecution` |
| Google Maps | `google_maps` | `google_maps*` | `googleMaps` |

Anthropic 接受的具体 server tool type 为：

| AI Studio 工具 | Anthropic type |
| --- | --- |
| Google Search | `web_search_*`（任何日期版本，如 `web_search_20250305`） |
| Image Search | `image_search` |
| URL Context | `web_fetch_*`（任何日期版本，如 `web_fetch_20250910`）、`url_context` |
| Code Execution | `code_execution_*`（任何日期版本，如 `code_execution_20250522`、`code_execution_20250825`） |
| Google Maps | `google_maps` |
| 无（忽略） | `tool_search_tool_*`、type 含 `toolset` 的定义 |

根 field 7 按请求声明逐项编码，函数声明和各类 Google 工具按上表对应的 Tool entry 编码。模型的工具范围取自实时能力码。根 field 14 为 ToolConfig；请求同时携带函数声明与 Google 工具时，其 field 3 `include_server_side_tool_invocations` 为 `true`，由上游在同一轮内执行 Google 工具并返回函数调用。

编码器将全部函数声明合并为一个 Tool entry；Google Search 与 Image Search 合并为一个 search entry 并分别占用 `searchTypes` 索引 `0/1`；Code Execution、URL Context 与 Maps 各占一个 entry。Google Maps 与 Code Execution/URL Context 构成互斥工具组，每个请求选择其中一组。

Anthropic server tool 的 `name` 必须分别为 `web_search`、`image_search`、`web_fetch`、`code_execution`、`url_context` 或 `google_maps`。这些定义接受 `type` 与对应 `name`，各 type 的文档化选项见 Anthropic Messages 一节，其余选项与 custom tool 一样忽略；`description` 或 `input_schema` 返回 `400 invalid_request_error`。被忽略的 `tool_search_tool_*` 与 toolset 定义不做这些检查。

函数 JSON Struct 使用 protobuf `Struct/Value` 数组：map 为 `[[[key,value],...]]`；Value oneof 索引 `0..5` 分别表示 null、number、string、bool、Struct、ListValue。对象键排序后编码。

例如以下函数参数：

```json
{
  "city": "Taipei",
  "days": 2,
  "metric": true,
  "note": null,
  "units": ["C", "F"]
}
```

编码后的 Struct 为：

```json
[
  [
    ["city", [null, null, "Taipei"]],
    ["days", [null, 2]],
    ["metric", [null, null, null, true]],
    ["note", [0]],
    [
      "units",
      [
        null,
        null,
        null,
        null,
        null,
        [[[null, null, "C"], [null, null, "F"]]]
      ]
    ]
  ]
]
```

完整 function call Part 的关键槽位为：

```json
[
  null,
  null,
  null,
  null,
  null,
  null,
  null,
  null,
  null,
  null,
  ["multiply", [[["a", [null, 21]], ["b", [null, 2]]]], "call_01"],
  null,
  null,
  null,
  "!THOUGHT_SIGNATURE"
]
```

其中 Part 索引 `10` 保存 function call，索引 `14` 保存 thought signature。

函数参数和结构化输出 Schema 使用以下 protobuf fields：

| JSON Schema | Field | JSON Schema | Field |
| --- | ---: | --- | ---: |
| `type` | 1 | `format` | 2 |
| `description` | 3 | `nullable` | 4 |
| `enum` | 5 | `items` | 6 |
| `properties` | 7 | `required` | 8 |
| `minProperties` | 9 | `maxProperties` | 10 |
| `minimum` | 11 | `maximum` | 12 |
| `minLength` | 13 | `maxLength` | 14 |
| `pattern` | 15 | `example` | 16 |
| `oneOf` | 17 | `anyOf` | 18 |
| `allOf` | 19 | `not` | 20 |
| `maxItems` | 21 | `minItems` | 22 |
| `propertyOrdering` | 23 | | |

Schema 归一化规则：

| 输入结构 | 编码结果 |
| --- | --- |
| `$schema`、`default`、`additionalProperties`、`exclusiveMinimum`、`propertyNames`、`prefixItems` | 从 wire schema 中省略 |
| 顶层 schema 为空或 `null` | 按没有参数的 object 编码；`response_format` 等结构化输出的 `schema: null` 按未设置处理，只保留 JSON 模式 |
| 值为显式 `null` 的字段（`const` 除外） | 按缺省处理 |
| `minItems`、`maxItems`、`minProperties`、`maxProperties`、`minLength`、`maxLength` 的字符串写法（`"5"`，`@google/genai` 等 SDK 把 int64 序列化为字符串）或零小数写法（`5.0`） | 按整数 `5` 编码；带小数、负数、非数字或超过 2^53 的写法照旧按其他 Schema 字段降级 |
| `type: "TYPE_UNSPECIFIED"`（Gemini 枚举零值，不区分大小写），包括 `anyOf` / `oneOf` / `allOf` 的直接分支 | 按没有声明 `type` 处理，由下面的缺少 `type` 规则推断，推断时不借用这个值 |
| `required` 列出而 `properties` 没有定义的属性（MCP 与 agent 客户端常见） | 函数参数补上不限类型的定义 `{}`，上游不再以 `property is not defined` 拒绝；递归处理 `properties`、`$defs`、`definitions`、`items`、`additionalProperties`、`not`、`anyOf`、`oneOf`、`allOf`、`prefixItems`；`properties` 不是 object 时不补 |
| `items: true` / `items: false` | `true` 不限制元素；`false` 编码为 `maxItems=0`，元素类型按缺省补齐 |
| `properties` 中的 `true` / `null` / `false` | `true` 与 `null` 按开放节点处理；`false` 删除该属性并同步移出 `required` |
| `not` 为 `null`、`false`、`true`、`""`、空对象 | 删除；`true` 本应禁止所有值，按宽松处理 |
| `not: {"type":"null"}` | `nullable=false` |
| `not` 为字符串或数组 | 转为 `not: {"enum": [...]}` |
| `type: [T, "null"]` | 根类型 `T` 与 `nullable=true` |
| `anyOf` / `oneOf` 的 null 分支 | 移除 null 分支并设置 `nullable=true` |
| 多个非 null `type` | 首项作为根类型，完整类型集合写入 `anyOf` |
| 组合 Schema 缺少根 `type` | 首个带类型的分支作为根类型，该分支的 `items` 同时写入根节点 |
| 其他节点缺少 `type` | 含 `properties` 为 object，含 `items` 或 `prefixItems` 为 array，其余为 string |
| array 缺少 `items` | `prefixItems` 中带类型的项组成 `anyOf`；没有时为 string |
| 其他 Schema 字段 | 降级编码：只编码层级与类型，完整 Schema 附在说明里（函数参数附在函数说明，结构化输出附在根节点说明；见下文） |

函数参数 Schema 含 Playground 无法编码的写法（`$ref`/`$defs`、`uniqueItems`、`examples`、数值或 null 常量、非字符串 `enum` 等）时不再让请求失败：只编码层级与类型（常量与非字符串枚举写进字段说明），并在函数说明末尾附上 `Arguments must follow this JSON Schema: <完整 Schema>`（压缩后超过 16 KB 时不附）。`strict` 函数同样附上完整 Schema。超过 1 MiB 或 64 层嵌套的 Schema 仍返回 400。Build 通道发送 `parametersJsonSchema`，不经过降级，字符串整数与 `TYPE_UNSPECIFIED` 也原样发送，只补齐 `required` 中未定义的属性。

`required` 补齐在 Playground 编码、Build `parametersJsonSchema` 与 `strict` 参数核对三处一致（核对用补齐后的 Schema，`additionalProperties:false` 时这些必填属性才可能满足，缺少必填属性的调用仍判为不符合）。大小与嵌套检查在前：超过 1 MiB 的 Schema 原样交给原有处理（Playground 返回 400，Build 原样发送）；已处于嵌套上限的节点不补，补齐不会让原本合规的 Schema 超过嵌套上限。不需要补齐时原样发送原始字节；补齐后重新编码，数值保留原写法，键顺序与空白可能变化。

结构化输出（`response_format` / `text.format` / `responseSchema` / Anthropic `output_config.format`）在 Playground 上按同样规则编码与降级（整数与 `TYPE_UNSPECIFIED` 规则同样适用，不做 `required` 补齐），完整 Schema 以 `The response must follow this JSON Schema: ...` 附在根节点说明；Build 通道原样发送 `responseJsonSchema`。

AI Studio 网页协议使用自动函数调用：auto 请求只携带根 field 7 的函数声明，由模型决定是否调用；none 省略 tools。Playground 没有工具选择字段，必须调用、指定函数与单次调用以系统指令写明要求，再由生成链路核对上游实际返回的调用；Build 通道写入原生 `toolConfig.functionCallingConfig`（必须调用为 `ANY`，指定函数写入 `allowedFunctionNames`，`validated` 或任一函数 `strict` 时为 `VALIDATED`）。客户端工具选择映射如下：

| 公开协议 | 自动 / 不调用 | 必须调用 | 指定函数 | 单次调用 |
| --- | --- | --- | --- | --- |
| OpenAI Chat / Responses | 默认、`auto`、`none` | `required` | `{"type":"function","function":{"name"}}`；Responses 为 `{"type":"function","name","namespace"?}` | `parallel_tool_calls:false` |
| Anthropic | 默认、`auto`、`none` | `any` | `{"type":"tool","name"}` | `disable_parallel_tool_use:true` |
| Gemini | 默认、`AUTO`、`NONE` | `ANY` | `allowedFunctionNames` | — |

Gemini `VALIDATED` 对所有函数按 Schema 核对参数。核对规则：指定函数时只发送被选中的函数；上游调用了未选择的函数、`strict` 函数的参数不符合 Schema（JSON Schema 2020-12 校验，`pattern` 按 ECMAScript 正则、单次匹配最多 1 秒）、要求调用却没有任何工具调用（声明了 Google 搜索等内置工具且上游使用了它们时视为满足）时，事件流以错误结束，HTTP 状态 502；首个事件之前发生时按非流式返回。要求单次调用而上游返回多个调用时只保留第一个。客户端 Schema 本身无法编译（如引用了未提供的外部定义）时不阻止请求，只是不核对该函数的参数。

函数调用响应 Part 为 `[name, Struct, callId?]`；上游没有给出 callId 时，代理补一个 `call_local_` 开头的本地 ID 返回给客户端，客户端带回结果时按它关联，发回上游时去掉；下一轮 function result 使用同一形状并原样带回 thought signature。tool result 显式提供函数名时保留该值；缺少名称时，先按 call ID 关联当前轮尚未返回结果的调用，未匹配且仅剩一个调用时使用其名称。每个结果对应一个调用，关联上的调用从待返回列表移除，调用与结果之间的助手文本不影响关联，新一轮普通对话开始后重新建立关联；存在歧义或缺少调用记录时返回参数错误。Build 通道按同样规则关联（同一轮两个调用时，第一个结果按 ID 关联后，第二个不带 ID 的结果关联剩下的调用；两个不带名称的结果对应同一个调用时同样返回参数错误）。函数参数和结果使用 JSON object，标量或数组结果封装为 `{"result":<VALUE>}`。

### Drive 上传与文件 Part

```text
GenerateAccessToken ["users/me"]
  -> response ["<BEARER_TOKEN>"]
  -> POST Drive multipart/related
       part 1: {"mimeType":"<MIME>","name":"<NAME>"}
       part 2: raw bytes
  -> {"id":"<FILE_ID>"}
  -> GenerateContent Part field 6 ["<FILE_ID>"]
```

Drive token、上传和下载使用文件所属账户的固定出口。文件 ID 与账户绑定写入 `runtime-state.json`；生成请求可以组合不同账户的文件，其他账户的文件会临时复制到本次生成账户，并在请求结束后回收副本。

生成请求的内联附件优先上传到本次生成账户，正文使用 Drive file Part。`GenerateAccessToken` 返回 `401` 或协议 Code 16 表示账户没有授权 Google Drive，包括 `OAuth error: unauthorized_client` 与 `Request is missing required authentication credential`（导入的账户多半没有走过官网的 Drive 同意，这些账户用 Cookie 认证的其他 RPC 与生成都正常）：正文保留原始 inline data Part，请求在这个账户上继续生成，不写冷却、不换号、不改账户状态，也不计入登录失效与换号的次数；管理日志写 `Drive 未授权，附件改为内联`（同账户 10 分钟最多一行，见 [logging.md](logging.md)）。账户没有 Drive 授权时附件内联发送，服务不为此另设大小上限（仍只有公开接口的请求体上限），过大的文件可能被上游以 400 拒绝；较大或要重复使用的附件可先经 `/v1/files` 上传、再按文件 ID 引用。服务在内存里按账户记住“Drive 不可用”（不落盘，重启或重建运行时后重新探测），有效期 6 小时，记录带着发出那次 `GenerateAccessToken` 之前的认证代际：重新登录、续签或更新认证文件推进认证代际后失效，管理验证通过时清除，任何路径换 token 成功也清除；记录有效期间这个账户的内联附件不再调用 `GenerateAccessToken`。签名 Cookie 缺失、登录页跳转等登录失效照旧按登录失效处理；内联发送后账户若其实已登出，随后的 `GenerateContent` 返回 401，由生成的认证流程处理。其他需要 Drive 的路径（`/v1/files` 上传、文件下载与删除、跨账户复制文件引用、转写的临时上传）遇到 `GenerateAccessToken` 的 401 只让这次尝试失败、按原有逻辑换号（换号只在本请求内排除失败的账户），不写全局冷却，同样记住“Drive 不可用”；不固定账户的文件上传先试没有这条记录的账户，有记录的账户排在最后仍会尝试。原先除 `unauthorized_client` 外的 401 让附件尝试失败、对账户写 2 分钟模型无关的全局冷却后换号，冷却原因以 `附件授权失败` 开头；没有 Drive 授权的账户一多，带图请求把大部分驻留 Worker 挡住，纯文字请求也用不上。其他令牌错误保持失败语义。Drive 上传、下载、删除的 Bearer 401 同样不改账户状态。上传使用公开适配器输出的 MIME 和字节内容；内联 GIF 在适配层提取首帧并编码为 `image/png`。图片、音频、视频、PDF 等附件的支持范围由所选模型决定。普通文本和 YouTube 外部媒体保持各自的 Part 编码。

OpenAI 文件入口接收 `multipart/form-data` 的 `file` 与 `purpose`，单文件上限为 512 MiB。未知长度的请求使用 Drive resumable upload 和 8 MiB 分块；上传完成后 `POST /v1/files` 返回持久文件对象，`GET /v1/files/{id}` 从资源绑定读取文件名、大小、purpose 与创建时间。客户端取消会终止上传并释放账户租约。

### Gemini 3.5 Transcribe

`gemini-3.5-transcribe` 使用 Drive file Part 与 GenerateContent generation config field 32。转录配置子字段如下：

| protobuf field | 内容 |
| ---: | --- |
| 5 | word timestamps，启用值为 `1` |
| 6 | speaker labels，启用值为 `1` |
| 7 | repeated custom vocabulary |
| 8 | repeated language codes |
| 9 | smart transcription，启用值为 `2` |

转录元数据位于响应 Part field 23，子字段 1 为文本、2 为 speaker label、3 为 repeated timestamp span。每个 span 的 field 2 与 field 3 分别是开始和结束时间，时间消息使用 seconds 与 nanos。

`POST /v1/audio/transcriptions` 接受最大 512 MiB 的音频、MP4 或 WebM 文件，公开格式为 `json`、`text`、`verbose_json` 与 `diarized_json`。每次账户尝试创建一个临时 Drive file；生成结束后在同一账户删除该文件。`smart_transcription` 与显式 word timestamps 或 speaker labels 的组合返回 `400 invalid_request`。

### Nano、TTS 与 Lyria

三类媒体复用 `GenerateContent`：

| 路由 | generation config | 响应 |
| --- | --- | --- |
| Nano image | modalities `[2]`，image config `[aspectRatio?, imageSize?]` | Part field 3 `[mime, base64]` |
| TTS | modalities `[3]`，speech config | Part field 3 音频 chunk |
| Lyria | modalities `[3]` | Part field 3 音频 chunk |

单声音 speech config 为 `[[[voiceName]]]`。多说话人 speech config 为 `[null,null,[null,[[speaker,[[voiceName]]],...],mode?]]`，mode `1` 为 `VERBATIM`、`2` 为 `CONVERSATIONAL`。文本 Part field 41 为 SpeechMetadata `[speaker?, style?]`；能力码 85 的 TTS 模型要求多说话人请求的每个文本 Part 带 speaker，且台词不写 `## Transcript:`。非流式结果只把相邻、MIME 相同且前后都不带签名的 `audio/l16` 音频 Part 按到达顺序拼接，其他音频格式与带签名的分片保持独立；`/v1/audio/speech` 按到达顺序拼接同一 MIME 的全部音频片段。图片宽高比、图片分辨率与 TTS voice 必须来自当前模型能力选项。

### Veo

`GenerateVideo` 使用 8 槽数组，WAA proof 位于 field 8：

```json
[
  "models/<MODEL_ID>",
  "<PROMPT>",
  [1, "<ASPECT_RATIO>", ["<SECONDS>"], "<RESOLUTION>"],
  ["<IMAGE_MIME>", "<BASE64>"] | null,
  ["<DRIVE_FILE_ID>"] | null,
  null,
  null,
  "<WAA_PROOF>"
]
```

起始帧的图像来源 oneof 为 inline image 或 Drive file。创建响应 field 1 是 operation ID。轮询请求为 `["<OPERATION_ID>"]`；轮询响应 field 1 是 done，产物 Drive file ID 位于 `$[1][0][0][0]`。operation 与结果 file 均绑定创建账户，再通过 Drive bearer 下载媒体。count、宽高比、秒数和分辨率按实时模型 field 71 校验。

### Omni Interaction

模型目录 field 79 类型为 `2` 且不是后台任务的模型（`gemini-omni-1.1-flash`、`gemini-omni-flash-preview`）不接受 `GenerateContent`，四套公开生成接口对这类模型改用 `CreateInteractionStream`，账户选择、Worker 与 WAA 与 GenerateContent 相同，WAA proof 位于 field 5，binding 为发送的全部文本以空格连接：

```json
[1, 1, null, <INTERACTION>, "<WAA_PROOF>", 1]
```

| interaction 索引 | 内容 |
| ---: | --- |
| 6 | system instruction |
| 17 | `["models/<MODEL_ID>", [null,null,null,null,null,<THINKING_LEVEL>,1,<MAX_OUTPUT_TOKENS>]]`，历史中没有模型输出时配置索引 24（GenerationConfig field 25）为 `[]`，设置视频任务时为 `[<TASK>]`（`1`–`5` 依次为 text_to_video、image_to_video、reference_to_video、edit、extend）；有模型输出时配置只到索引 7，不能设置视频任务 |
| 26 | `[[<STEP>...]]`，用户 step 为 `[[<CONTENT>...]]`，模型 step 为 `[null,[<CONTENT>...]]`；文本 content 为 `[[<TEXT>]]`，Drive 附件 content 为 `[null×8,["<DRIVE_FILE_ID>"]]` |
| 53 | 输出格式（field 54）`[[[null,null,null,[null,null,null,<ASPECT>,[<SECONDS>],<RESOLUTION>]]]]`：宽高比 `1` 为 16:9、`2` 为 9:16，未设置（官网 Auto）时为 null；时长未设置时为 null；分辨率 `1`–`4` 为 360p、720p、1080p、4k，未设置时取所选账户目录 `video_output_resolutions` 的第一项（`368p` 按 `360p`；目录没有时为 `1`，即官网默认 360p）；任务为 `edit` 时没有输出格式，interaction 只到索引 26 |

thinking level 取值 MINIMAL=1、LOW=2、MEDIUM=3、HIGH=4，由 `reasoning_effort` 或思考预算按模型支持的等级换算，默认使用目录默认等级。Interaction 请求接受 user 与 assistant 的文本内容，以及 user 的图片与视频附件；内联附件先上传到所选账户的 Drive，再以 file ID 引用，文件 ID 不参与 binding。模型 step 只带文本：assistant 消息里的附件（客户端回传的生成图片、本服务恢复的图片链接、`previous_interaction_id` 续接历史里模型生成的视频，与 user 附件一样先上传 Drive）连同一句说明“以下是上文模型回复里的图片或视频：”移到紧随其后的 user step 最前面，以 assistant 消息结尾时单独成一条 user step；只有附件的 assistant 消息整条省略，说明文字参与 binding。例如先用图片模型出图，再在同一对话里换成 Omni 让它“动起来”时，Omni 能看到这张图。账户没有 Drive 授权（附件仍是 inline data）时与 user 附件一样返回 400，assistant 消息里的外部地址附件同样返回 400，不静默丢弃；原先 assistant 消息里带附件的请求一律返回 400。音频附件由上游以 code 3 `Audio input modality is not enabled for this model` 拒绝，映射为 HTTP 400；函数与工具返回参数错误，temperature、topP、topK 与 seed 不发送。stop sequences 不发送也不在本地匹配：没有 CreateInteractionStream 停止序列字段的现场证据，不猜字段号（官网 Run settings 有 Add stop sequence，上游 Mag1cFall/AIStudio2API 同样不发送，改为与 GenerateContent 一样在本地匹配正文；本服务 GenerateContent 的 stop sequences 也只在本地匹配，但 Omni 的回复主要是视频，在本地按正文截断会连带切掉视频，所以不匹配）。协议层对能输出视频的 Omni 模型清空并在该请求的管理日志记一行 `未确认 CreateInteractionStream 的停止序列字段，已忽略 <N> 个停止序列`；协议层没有处理到的请求（启动期间别名目录取不到、目录没有列出视频选项的 Interaction 模型）在发送前补记同一行（原先整次请求返回参数错误）。

视频设置（官网 Run settings 的 Aspect ratio、Video duration、Resolution、Frame rate 与 Advanced 的 Video task）的公开写法见 [Omni 视频设置与模型名后缀](#omni-视频设置与模型名后缀)。协议层按合并目录整理并留下请求日志说明，发送前再按所选账户的目录整理一次（合并目录的时长范围已取各账户的交集，一般不会再改；别名目录缓存还没刷新或协议层没有整理到时可能不同），这一次留下的新说明（协议层没有记过的）以该账户为来源写进同一请求的管理日志；视频任务是否可用以合并目录为准，所选账户目录没有列出时照发，由上游判断（不按单个账户返回 400，免得另一个账户能完成的请求直接失败）；编码时宽高比与分辨率取最接近的上游枚举，在上游能编码的取值上与上游 Mag1cFall/AIStudio2API 的 `interactionVideoFormat` 一致；有两处不同：所选账户目录的默认分辨率为空或为 `368p` 时本服务发 `1`（官网默认 360p），上游不带；另外本服务多接受 `auto`、`ASPECT_RATIO_UNSPECIFIED`、`16x9`、`VIDEO_RESOLUTION_` 与 `VIDEO_TASK_` 前缀、`unspecified` 等写法，上游会报错。帧率只校验不发送（官网只有 24fps 且不可选）。

响应为 `[[<EVENT>...], <STATUS>?]`。事件索引 10 的 content delta 中 field 1 为正文、field 5 为视频 `[1,"<BASE64 MP4>"]`、field 6 为思考摘要，分别映射为 text、`video/mp4` 媒体与 reasoning 事件；索引 19 的最终 interaction 状态 `3` 产生 usage（输入、输出、思考、总 token 位于 usage 索引 0、4、8、9）与 `stop` 终态，状态 `4`、`5` 返回错误。事件列表后的 google.rpc 状态按 code 映射 HTTP 状态，额度 code 8 为 429；首个正文事件之前的错误进入换号与冷却，之后的错误以流内 error 结束。

### Build 代理

官网 Build 应用经宿主页调用 MakerSuite 代理 RPC 访问 Gemini API，额度与 Playground 分开计算。`UPSTREAM_CHANNELS` 启用 `build` 时，生成请求在 Build 通道上编码为 Gemini API JSON：

```json
["/v1beta/models/<MODEL_ID>:streamGenerateContent", "<GEMINI_API_JSON>", "<WAA_PROOF>"]
["/v1beta/models/<MODEL_ID>:generateContent", "<GEMINI_API_JSON>", "<WAA_PROOF>", "POST"]
```

前者发往 `ProxyStreamedCall`，后者发往 `ProxyUnaryCall`。WAA proof 位于 field 3，binding 为路径与请求体以空格连接后的 SHA-256。权益头 `X-AIStudio-G1-Tier` 只随 `ProxyUnaryCall` 发送，需订阅权益的模型经 `ProxyUnaryCall` 生成，其余经 `ProxyStreamedCall`。模型目录、资格、调度与冷却、请求字段映射、响应解码、错误映射与未接入范围见 [Build 通道](build.md)。

### Live 与 Robotics WebSocket

`GET /v1/live` 与 `GET /v1/robotics/stream` 升级为 WebSocket。连接建立后的首个客户端 JSON 必须是 setup：

```json
{"type":"setup","model":"gemini-3.1-flash-live-preview","input_modalities":["text"],"output_modalities":["audio"],"tools":[{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"session_token":""}
```

Live 的 input modalities 可以由 text、audio、image 组成，output modalities 为 `["audio"]` 或 `["text"]`，按模型能力确定：能力码 46 的实时翻译模型输出 audio 并要求 `translation`，能力码 77 的实时转录模型输出 text 并可带 `transcription`，其他 Live 模型输出 audio。Robotics 输入输出均为 text。两个入口都使用客户端提供的模型，并由实时模型目录的 `bidiGenerateContent` 方法选择账户。客户端在 WebSocket 建立后 10 秒内发送 setup 首帧；上游握手、backchannel 就绪和 setup complete 共用 `INIT_TIMEOUT`，随后依次发送 `session_opened` 与 `setup_complete`。

上游 WebChannel 由握手、前向 POST、长轮询 backchannel 与 terminate 组成。握手 query 使用 `VER=8`、随机 `RID`、`CVER=22`、`X-HTTP-Session-Id=gsessionid` 与 `count=0`；响应 header 给出 gsessionid，首个控制 envelope 为：

```json
[[0,["c","<SID>","",8]]]
```

后续前向请求的 query 字段为 `VER`、`gsessionid`、`SID`、`RID`、`AID`、`zx`、`t`；form 字段依次为 `count=<N>`、`ofs=<OFFSET>`、`req0___data__` 至 `req<N-1>___data__`。客户端消息按到达顺序排队，同一时间只有一个前向 POST，在途期间到达的消息合并进下一个 POST，每个 POST 最多 25 条，与官网页面一致。audio 与 image 帧入队即返回，text、tool_response、media_end 等待自身所在 POST 的 ACK，因此 media_end 返回时此前的媒体帧均已送达；前向 POST 失败后，队列内与后续发送均返回该错误。成功响应是三个整数的 ACK 数组，三个槽只校验整数形状，第二槽不绑定本地 RID、AID 或 ofs。HTTP 200 且 ACK 合法后提交 `RID+1` 与 `ofs+N`；失败、取消或 ACK 无效时保持原值。

backchannel 使用 `RID=rpc`、当前 SID、gsessionid 与 AID。每个网络帧为十进制长度、LF 和对应 JSON 文本，长度按 UTF-16 码元计数：

```text
<DECIMAL_UTF16_LENGTH><LF>
<JSON_TEXT>
```

JSON 根值包含 repeated `[serverAID,payload]` envelope。payload 解码成功并按顺序发布后，将 AID 提交为 `max(currentAID,serverAID)`。首次 backchannel 建立后的临时读取失败保留 SID、gsessionid 与已提交 AID 并重新连接；首次建立失败、协议终态、显式 close 或不可恢复错误进入关闭链。会话状态为 `new -> handshaking -> ready -> reconnecting -> ready`，关闭路径依次进入 `closing -> closed`。

客户端帧：

| type | 字段 | 作用 |
| --- | --- | --- |
| `text` | `text` | 发送文本输入 |
| `audio` | `mime_type: audio/pcm`、`data` | 发送 PCM Base64 |
| `image` | `mime_type: image/jpeg`、`data` | 发送 JPEG Base64 |
| `media_end` | 无 | 结束当前媒体输入 |
| `tool_response` | `tool_responses` | 批量发送函数结果，保留调用的 `id` 与 `name` |
| `close` | 无 | 关闭逻辑会话 |

`tool_response` 示例：

```json
{"type":"tool_response","tool_responses":[{"id":"call-1","name":"get_weather","content":{"temperature":26}}]}
```

服务端事件使用 `session_opened`、`setup_complete`、`text`、`media`、`input_transcription`、`output_transcription`、`interim_input_transcription`、`tool_call`、`tool_call_cancellation`、`interrupted`、`generation_complete`、`turn_complete`、`session_resumption`、`usage`、`go_away`、`provider`、`closed` 和 `error`。`tool_call` 携带单个函数调用；一条上游消息包含多个调用时按顺序发送多条事件。`tool_call_cancellation.tool_call_ids` 携带被取消的调用 ID。新的 `session_resumption.session_token` 原子替换上一枚 token；后续 setup 携带该 token 时绑定原账户恢复。

Live 纯文本使用模型 scope，Live 音频/图像和 Robotics 使用 `bidi-media:<modelID>` scope。上游 Code 7 通过 `error` 事件返回并结束当前连接。客户端单帧上限为 8 MiB，超限使用 WebSocket close code `1009`。

上游 Bidi 客户端 wire 使用六槽或七槽稀疏数组。setup 位于外层 field 7：

| setup JSON 索引 | protobuf field | 内容 |
| ---: | ---: | --- |
| 0 | 1 | `models/<MODEL_ID>` |
| 1 | 2 | generation configuration |
| 2 | 3 | tools |
| 6 | 7 | `[sessionToken]`；无 token 时 Live 对话与 Robotics 为 `[]`，实时翻译与转录省略 |
| 7 | 8 | `[104857,[52428]]` buffering 参数；实时翻译与转录省略 |
| 9 | 10 | 输入音频转录参数；空数组，实时转录的语言写在子索引 `7` |
| 10 | 11 | 固定空数组 |
| 15 | 16 | timezone `[null,null,null,null,[zone]]` |

Live 对话与 Robotics 的 configuration 使用 18 槽数组：

| JSON 索引 | 内容 |
| ---: | --- |
| 14 | 输出 modalities；TEXT=`[1]`、AUDIO=`[3]` |
| 15 | Live voice `[[["Zephyr"]]]` |
| 16 | thinking `[1,null,null,level]`；Live Minimal=`4`、Robotics High=`3` |
| 17 | MediaResolution 固定值 `2` |

实时翻译模型不接受 MediaResolution，configuration 使用 31 槽：索引 13 为 `0`、14 为 `[3]`、30 为 TranslationConfig `[echoTargetLanguage 0/1, targetLanguageCode]`。实时转录模型只接受 TEXT 输出，configuration 为 `[null×14,[1]]`。两者均不带 voice 与 thinking。上游在 serverContent 索引 5、6 返回输入与翻译文本，实时转录在索引 10 返回当前累计的临时输入转写，映射为 `interim_input_transcription`，音频结束后在索引 5 返回最终输入转写。

最小 setup 外层形状：

```json
[
  null, null, null, null, null, null,
  [
    "models/<MODEL_ID>",
    [null, null, null, null, null, null, null, null, null, null, null, null, null, null, [3], [[["Zephyr"]]], [1, null, null, 4], 2],
    null,
    null, null, null,
    [],
    [104857, [52428]],
    null,
    [],
    [],
    null, null, null, null,
    [null, null, null, null, ["Asia/Taipei"]]
  ]
]
```

后续客户端 wire：

| 公开帧 | 外层位置 | 子消息 |
| --- | --- | --- |
| `text` | index 2 / field 3 | realtime input index 4 保存文本 |
| `audio` | index 2 / field 3 | realtime input index 1 保存 `[mime,base64]` |
| `image` | index 2 / field 3 | realtime input index 3 保存 `[mime,base64]` |
| `media_end` | index 2 / field 3 | realtime input index 2 为 `1` |
| `tool_response` | index 3 / field 4 | 子消息 index 1 保存 repeated `[name,Struct,id]` |

setup 与每个后续客户端 wire 都在发送前把 fresh WAA proof 写入外层 index `5` / field `6`。snapshot binding 输入：

| wire | binding prompt |
| --- | --- |
| setup | `models/<MODEL_ID>`，随后按声明顺序追加每个 `name + " " + description`，各段以单个空格连接 |
| text、audio、image、media end | 空字符串 |
| tool response | 第一条 function response 的 call ID |

服务端业务 message 索引：

| JSON 索引 | 内容 |
| ---: | --- |
| 1 | setup complete |
| 2 | server content |
| 3 | tool calls |
| 4 | tool cancellation |
| 5 | usage raw |
| 6 | go away raw |
| 7 | session resumption |

server content 的 index `0/1/2/4/5/6` 分别为 model content、turn complete、interrupted、generation complete、input transcription、output transcription；model content 的 index `0` 是 repeated Part。transcription 子索引 `0/1/2/3` 分别为 text、finished、duration milliseconds、language code。tool calls 位于 message index `3` 的子索引 `1`，每项为 `[name,Struct,id]`；tool cancellation 位于 message index `4` 的子索引 `0`，值为 repeated call ID。session resumption 子索引 `0/1` 分别为 token 与 resumable。对象状态错误形状为 `{"__sm__":{"status":[[[code,message]]]}}`；字符串 payload `"noop"`、`"close"`、`"stop"` 分别表示空操作、正常关闭与错误停止。

## 7. 公开端点、状态映射与实现

| 协议 | 端点 |
| --- | --- |
| OpenAI Chat | `GET /v1/models`、`GET /v1/models/{model}`、`POST /v1/chat/completions` |
| OpenAI Responses | `POST /v1/responses` |
| Gemini Interactions | `POST /v1beta/interactions`、`POST /v1/interactions` |
| OpenAI 媒体 | `POST /v1/images/generations`、`POST /v1/audio/speech`、`POST /v1/videos`、`GET /v1/videos/{id}`、`GET /v1/videos/{id}/content` |
| Anthropic | `POST /v1/messages`、`POST /v1/messages/count_tokens` |
| Gemini | `GET /v1beta/models`、`GET /v1beta/models/{model}`、`POST /v1beta/models/{model}:generateContent`、`:streamGenerateContent`、`:countTokens`、`:predictLongRunning`、`GET /v1beta/operations/{id}` |

扩展端点：

| 协议 | 端点 |
| --- | --- |
| OpenAI 文件 | `POST /v1/files`、`GET /v1/files/{id}`、`GET /v1/files/{id}/content`、`DELETE /v1/files/{id}` |
| OpenAI 转录 | `POST /v1/audio/transcriptions` |
| 实时 WebSocket | `GET /v1/live`、`GET /v1/robotics/stream` |

上表全部端点另有 `/ultra` 前缀的版本（`/ultra/v1/...`、`/ultra/v1beta/...`），去掉前缀后交给与主路由完全相同的处理链（API key、CORS、来源检查、请求体上限、请求日志、排查记录与正文记录），只是请求只使用 Ultra 号池的账户；去掉前缀时保留客户端的转义（如 ID 中的 `%2F`），与直接请求 `/v1` 时匹配到同一个路由；客户端的接口地址为 OpenAI `http://<host>:<port>/ultra/v1`、Anthropic `http://<host>:<port>/ultra`、Gemini `http://<host>:<port>/ultra`（请求 `/ultra/v1beta/...`）。其余 `/ultra/*` 路径返回 404；排查路由 `/trace/` 按普通路径的规则选号，`/trace/ultra/v1/...` 去掉两个前缀并只用 Ultra 号池，`/ultra/trace/...` 返回 404。`/ultra` 请求在 Ultra 号池为空或账户都不可调度时返回 503（消息说明 Ultra 号池没有可用账户），全部冷却返回 429 与 `Retry-After`；`ULTRA_PRIMARY_MODELS` 中的模型没有额度时立即返回 429 与 `Retry-After`（见下文 `PrimaryQuotaError`）；引用的文件、视频或 operation 绑定在另一号池的账户上时（`ULTRA_EXCLUSIVE=true` 时普通路径引用 Ultra 号池账户的资源同样如此，含 `ULTRA_INCLUDE_PRO=true` 时 Pro 账户上的资源）按各协议格式返回 400，消息说明资源属于哪个号池。资源不随 `ULTRA_INCLUDE_PRO` 切换迁移，是否放行按资源所属账户当前的号池判断：开启后，`ULTRA_EXCLUSIVE=true` 时经普通路径引用 Pro 账户上已有的资源返回 400（消息说明资源属于 Ultra 号池，要改用 `/ultra`），`ULTRA_EXCLUSIVE=false` 时普通路径不检查号池，开启不会产生 400；关闭后经 `/ultra` 引用 Pro 账户上的资源返回 400（消息说明资源属于普通号池，要改用普通路径）。`/ultra/v1/models`、`/ultra/v1beta/models` 与 Anthropic 模型列表只列 Ultra 号池账户可用的模型。

动态路由的注册形状为 `GET /v1/files/{file}`、`GET /v1/files/{file}/content`、`DELETE /v1/files/{file}`、`GET /v1/videos/{video}`、`GET /v1/videos/{video}/content`、`POST /v1beta/models/{action}` 与 `GET /v1beta/operations/{operation}`；端点表中的 `{id}` 表示对应资源标识。

公开 `/v1` 与 `/v1beta` 从 `?key=`、`X-Goog-API-Key`、`X-API-Key`、`api-key` 请求头与 `Authorization: Bearer` 读取 API key，任一来源与配置的密钥相同即通过，每个来源分别按常量时间比较；其他来源为空、是旧密钥或下游令牌都不影响结果（例如经 new-api 转发时 `?key=` 里是下游令牌、请求头里才是本服务密钥）。只支持配置一个密钥，不按逗号拆分。配置为空时关闭本地 API key 校验，此时 `Origin` 为 `null` 或非 localhost、非回环地址的 http/https 页面请求返回 401，不带 `Origin` 的客户端与其他 scheme 不受限制。`/v1*` 响应允许任意 origin，允许 `GET/POST/PUT/DELETE/OPTIONS` 与 `Authorization`、`Content-Type`、`X-API-Key`、`X-Goog-API-Key`、`Api-Key`、`Anthropic-Version`、`Anthropic-Beta` headers。`/v1*` 请求体上限约为 684 MiB，可容纳 Base64 编码的 512 MiB 文件。`/api` 控制面要求来源地址为 loopback 且 `Host` 为 localhost 或回环地址，携带 Origin 时执行 same-origin 校验。全部响应携带 `X-Frame-Options: DENY`、`Content-Security-Policy: frame-ancestors 'none'`、`X-Content-Type-Options: nosniff` 与 `Referrer-Policy: no-referrer`。`GET /health` 返回 `{"status":"ok"}`。

| 控制能力 | 端点 |
| --- | --- |
| 状态与模型 | `GET /api/status`、`GET /api/models` |
| 生成服务 | `POST /api/control/start`、`POST /api/control/stop` |
| 账户 | `GET /api/accounts`、`POST /api/accounts`、`GET/POST /api/accounts/import/chrome`、`POST /api/accounts/bulk-enable`、`PUT /api/accounts/{id}`、`DELETE /api/accounts/{id}`、`POST /api/accounts/bulk-delete`、`POST /api/accounts/proxy/follow-default` |
| 账户认证 | `POST /api/accounts/{id}/login`、`POST /api/accounts/{id}/verify` |
| 配置 | `GET /api/config`、`PUT /api/config` |
| 代理测试 | `POST /api/proxy/test`、`POST /api/proxy/pool/check` |
| 冷却与请求 | `GET /api/cooldowns`、`GET /api/requests`、`POST /api/requests/{id}/cancel` |
| 403 无权限账户与组织停用账户 | `GET /api/accounts/permission-denied`、`POST /api/accounts/permission-denied/clear`、`POST /api/accounts/org-disabled/clear` |
| 出口限流判定 | `POST /api/egress/{id}/withdraw`、`POST /api/egress/{id}/resume` |
| 日志与事件 | `DELETE /api/logs`、`GET /api/events` |
| 用量账本 | `GET /api/usage`、`GET /api/usage/records`、`GET /api/usage/records.csv`、`GET /api/requests/{id}/body`（账本打开失败时不注册） |

管理 API 使用 DTO（data transfer object）表示请求、响应和事件对象。端点结果：

| 端点 | 成功状态 | body |
| --- | ---: | --- |
| `GET /api/status` | 200 | `AdminStatus` |
| `GET /api/models` | 200 | `{"models":[Model,...]}` |
| `GET /api/accounts` | 200 | `{"accounts":[AdminAccount,...]}` |
| `POST /api/accounts` | 201 | `{"account":AdminAccount}` |
| `GET /api/accounts/import/chrome` | 200 | `{"profiles":[ChromeImportProfile,...]}` |
| `POST /api/accounts/import/chrome` | 201 | `{"accounts":[AdminAccount,...]}` |
| `POST /api/accounts/bulk-enable` | 200 | `BulkEnableResult` |
| `PUT /api/accounts/{id}` | 200 | `{"account":AdminAccount}` |
| `POST /api/accounts/{id}/login`、`verify` | 200 | `{"account":AdminAccount}` |
| `DELETE /api/accounts/{id}` | 204 | 空 body |
| `POST /api/accounts/bulk-delete` | 200 | `BulkDeleteResult` |
| `POST /api/control/start`、`stop` | 200 | `AdminStatus` |
| `GET /api/config`、`PUT /api/config` | 200 | `RuntimeConfig` |
| `POST /api/proxy/test` | 200 | `ProxyTestResult`，见下文“代理测试” |
| `POST /api/proxy/pool/check` | 202 | 空 body；请求体 `{"pool"?:ProxyPoolConfig}`（省略 `pool` 或请求体为空时用当前生效的设置，只支持静态 IP），在后台检查端口段里每个端口的出口 IP，进度与结果在 `AdminStatus.proxy_pool.check`；已在进行时 409，见下文“IP 池” |
| `POST /api/accounts/proxy/follow-default` | 200 | `{"changed":N,"running":N,"skipped":N}`，见下文“IP 池” |
| `GET /api/cooldowns` | 200 | `{"cooldowns":[AdminCooldown,...]}` |
| `GET /api/accounts/permission-denied` | 200 | `{"accounts":[AdminPermissionDenied,...],"org_disabled":[AdminOrgDisabled,...]}`，`accounts` 按最近一次 403 从新到旧；`org_disabled` 为组织停用 AI Studio 的账户（见下文“组织停用 AI Studio 的账户”） |
| `POST /api/egress/{id}/withdraw`、`resume` | 200 | `AdminStatus`（动作后的最新状态）；`{id}` 为 `AdminEgress.id`。`withdraw` 撤销该出口的判定，30 分钟内不再判定（交回逐级冷却），`resume` 取消撤销、回到 `normal`；只改内存，不落盘、不是配置项。没有这个出口或管理端不支持时 404 |
| `POST /api/accounts/org-disabled/clear` | 200 | 请求体同下一行；清除组织停用状态、立即恢复调度，返回 `{"cleared":<清除的账户数>}` |
| `POST /api/accounts/permission-denied/clear` | 200 | 请求体 `{"account_ids":[...]}` 或 `{"all":true}`（两者都没有时 400；账户 ID 只去掉首尾空白，匹配不区分大小写）；返回 `{"cleared":<清除的账户数>}`，只清除记录、不解除冷却 |
| `GET /api/requests` | 200 | `{"requests":[AdminRequest,...]}` |
| `POST /api/requests/{id}/cancel` | 204 | 空 body |
| `DELETE /api/logs` | 204 | 空 body |
| `GET /api/events` | 200 SSE | `{"type":"<TYPE>","data":<DTO>}` |
| `GET /api/usage` | 200 | `UsageReport`；必填 `from`、`to`（RFC 3339，不超过 93 天），可选 `tz`、`bucket`、`stack` 与按维度逗号分隔的 `model`、`account`、`channel`、`protocol`、`state`、`pool`（`normal` 或 `ultra`）筛选；`stack` 与分项 `groups` 同样包含 `pool` |
| `GET /api/usage/records`、`records.csv` | 200 | `{"items":[UsageRecord,...],"next_cursor"?}` 或带 BOM 的 UTF-8 CSV；另接受 `status`、`q`（请求 ID、错误或回复指纹）、`cursor`、`limit`（1 到 200）；记录与 CSV 带 `pool`（`normal` 或 `ultra`） |
| `GET /api/requests/{id}/body` | 200 / 404 | `RequestBody`；只在 `REQUEST_BODY_LOG=true` 时保存 |

管理 DTO 字段：

| DTO | 字段 |
| --- | --- |
| `AdminStatus` | `state`、`running`、`ready`、`version`、`active_requests`、`accounts`、`workers`、`ultra_accounts`、`ultra_workers`、可选 `primary_supply`（`AdminPrimarySupply` 数组，`PRIMARY_MODELS` 与 `ULTRA_PRIMARY_MODELS` 的每个模型各一项，同一模型在两份列表里时有两项；两份列表都为空、服务未运行或还没统计过时省略）；服务运行时另有 `egress`（`AdminEgress` 数组，出口的限流判定与限速：出口不超过 16 个时全部列出、含正常的，否则只列出判定成立或已撤销的、正常的不列出，这时全部正常则省略）、`egress_summary`（`AdminEgressSummary`，全部出口的汇总计数）、`catalog_sync`（`AdminCatalogSync`，模型目录缓存与后台同步）、`auth_recheck`（`AdminAuthRecheck`，登录复核）；`bad_accounts` 为现在标为坏号（连续裸限流、期间没有成功）、移出预热、扩容与替补候选的账号数；可选 `proxy_pool`（`AdminProxyPool`：`PROXY_POOL_MODE` 不为 `off`，或关闭后仍有浏览器在用旧出口时返回） |
| `AdminAccountCounts` | `total`、`ready`、`busy`、`cooldown`、`auth_required`、`org_disabled`（所属组织停用了 AI Studio 的启用账户），`accounts` 另有 `auth_required_10m`（最近 10 分钟新被标为需要登录的账户数）；`accounts` 为全部账户，`ultra_accounts` 只含 Ultra 号池成员（`ULTRA_INCLUDE_PRO=true` 时含权益为 Pro 的账户） |
| `AdminAuthRecheck` | `suspects`（疑似登录失效、等待复核的账户）、`queued`（本轮还没复核的需要登录账户）、`running`、`cleared_1h`（疑似复核通过）、`confirmed_1h`（疑似复核仍是 401、已标需要登录）、`recovered_1h`（需要登录账户复核通过、已恢复）、`still_invalid_1h`、`skipped_1h`（账户忙、出口限流或复核出错）、`next_round`、可选 `pause_reason`（`出口限流`） |
| `AdminWorkerCounts` | `warm`、`starting`、`target`、`max`、`occupied`、`dead`、`prewarm`、`warm_ids`、`starting_ids`；数量与上限只含普通分区，`warm_ids`、`starting_ids` 包含两个分区；`occupied` 为占用的槽位（含正在关闭的；排空中的 Worker 按已让出计，与调度的容量判断一致，排空期间浏览器数可以比它多）；`dead` 为 `warm` 中账号全局长时间冷却，或在热门冷却模型、所在分区的主用模型（普通分区按 `PRIMARY_MODELS`，Ultra 分区按 `ULTRA_PRIMARY_MODELS`）上长时间冷却（如每日限额）、占着名额却服务不了请求、等待淘汰的死槽位，与淘汰登记的口径一致；`draining` 为其中排空中的 Worker（不再接不固定账户的新请求，进行中的请求结束后关闭，替补已先启动）；`retiring` 为待淘汰登记数；`unschedulable` 为 `warm` 中账号已不能服务任何模型（需要登录、已停用、不可用、目录加载过却没有可初始化的模型）、等待释放的驻留 Worker |
| `AdminWorkerPartition` | `warm`、`starting`、`occupied`、`dead`、`draining`、`retiring`、`unschedulable`、`target`、`warm_limit`、`max`；`ultra_workers` 为 Ultra 分区，`target` 为常驻数与分区内可预热账户数取小，其余字段口径同 `AdminWorkerCounts` |
| `AdminEgress` | `id`（出口编号，代理键 sha256 前 12 位，管理动作用，不含代理原文）、`label`（脱敏显示：`直连`、`scheme://host:port`，带用户名时追加 `#` 与用户名 sha256 前 4 位；IP 池的会话号写在密码里时出口键带 `#会话号`，这时是“用户名#会话号”一起取 sha256 前 4 位，同一用户名的不同会话标签不同；无法解析时为 `<无法识别的代理>#` 加原文哈希）、`mode`（`normal`、`limited`、`rejected`）、`limiter`（`off`、`active`、`suspended`：限速判为无效已停止）、可选 `since`、`accounts`、`lanes`（`AdminEgressLane` 数组）、`success_1m`、`bare_429_1m`、`other_1m`、`ratio_2m`（2 分钟内裸 429 占尝试结果的比例，0–1，不含已降级账号）、可选 `ratio_entry`（判定成立时的比例）、`fresh_rejects_2m`（冷号首发被拒）、`queued`（正在按“出口限速”排队的请求）、`local_429_1m`（本地返回的 429：出口限速排队超限与出口限流换号上限）、可选 `retried`、`recovered`、`demoted`、`relaxed`（本次判定期间复试、复试后成功、按账号证据降级、判定成立时撤回逐级冷却的账号数）、可选 `retry_at`、`rejected_until`、`rejected_by`（`安全阀` 或 `管理操作`） |
| `AdminEgressSummary` | `total`（有账户、有活动或有判定记录的出口数）、`normal`、`limited`、`rejected`；IP 池开启后静态每个端口、动态每个会话各是一个出口，出口多于 16 个时 `egress` 只列出 `limited` 与 `rejected` 的 |
| `AdminEgressLane` | `lane`（`normal` 或 `ultra`：Ultra 分区的账户单独一条令牌桶）、`accounts`、`limit_rpm`（0 为不限）、可选 `burst`、`sent_1m`、`primary_sent_1m`、`other_sent_1m`（经租约发出的非生成 RPC，只计数）、`pending`（已取得生成租约、还没发出的预占）、`cold_starting`（闸门放行、正在启动的冷启动）、`cold_started_1m`、`refill_deferred_1m`（最近一分钟因补位并发已满被推迟的补位） |
| `AdminCatalogSync` | `cached`（有目录的账户，含来自磁盘的）、`from_disk`（本代运行时启动时从磁盘恢复的账户）、`missing`（启用、就绪却没有目录）、`expired`（超过有效期、等待刷新）、`auth_suspects`（疑似登录失效、等待探测）、`queued`、`running`、可选 `paused_until`、`pause_reason`（`上游 429` 或 `出口限流`）、`synced_1h`、`failed_1h`、`rate_limited_1h`、`skipped_busy_1h`（账户有进行中的请求、推后刷新的次数） |
| `AdminPrimarySupply` | `model`、`warm`、`standby`、`cooling`、可选 `standby_capped`、可选 `pool`；一个主用模型在所属分区的可用供给：`PRIMARY_MODELS` 的条目统计普通分区，`pool` 省略；`ULTRA_PRIMARY_MODELS` 的条目 `pool` 为 `ultra`，只统计 Ultra 分区的驻留 Worker 与 Ultra 号池的账号（`ULTRA_INCLUDE_PRO=true` 时含 Pro 账户）。供给由 `keepWarm` 约每 30 秒统计一次并缓存（任一份主用模型列表变化或 `ULTRA_INCLUDE_PRO` 切换后的下一个周期立即重新统计，约 5 秒内），状态请求只读缓存。`warm` 为能服务它的驻留 Worker（是该模型的候选、对它没有冷却、不在排空中），`standby` 为能服务它的空闲备用账号（没有驻留 Worker、不在启动中、空闲、对它没有冷却），最多统计 1000 个，达到上限时 `standby_capped` 为 `true`；`cooling` 总是输出，为对它冷却中的驻留 Worker（不在排空中、是该模型的候选，但所有支持的启用通道都在冷却，含剩余不到 2 分钟的短冷却，与调度分类口径一致），这些 Worker 既不计入 `warm` 也不计入 `standby`，驻留 Worker 大多在这里时，主用模型的请求只能等冷却到期或换上新号；模型在号池里找不到时各项都为 0。`standby` 为 0 表示没有还能服务该模型的空闲账号，多半是当天额度已用完（太平洋时间零点重置） |
| `AdminAccount` | `id`、`label`、`enabled`、`state`、`proxy`、`locale`、`timezone`、`models`、`benefit_tier`、`benefit_tier_known`、`pool`（`ultra` 或 `normal`，账户当前所属的号池，即成员关系：`ULTRA_INCLUDE_PRO=true` 时 Pro 账户为 `ultra`）、`message`、可选 `proxy_pool_slot`（账户没有自己的代理时 IP 池分给它的端口号或会话号）、可选 `proxy_pool_active`（有浏览器正在用这个出口）、可选 `proxy_pool_pinned`（IP 池开启、账户代理与当前生效的 `PROXY` 规范值相同，因此不用 IP 池；与 `AdminProxyPool.pinned_default`、`POST /api/accounts/proxy/follow-default` 同一份判断，保存了新的 `PROXY` 但还没重启生成服务时仍按旧值）、可选 `org_disabled`（账户所属组织停用了 AI Studio，不区分是否启用：启用账户的 `state` 为 `org_disabled`，停用账户的 `state` 仍为 `disabled`、`message` 在停用原因后接上 `AI Studio 已被账号所属组织停用`）；`benefit_tier` 始终是账户的真实权益，并入 Ultra 号池的 Pro 账户仍为 `Pro`。`POST /api/accounts/{id}/verify` 的返回另带 `authenticated`（浏览器已登录且接口复核通过才为 `true`）与 `verify_reason`（未通过的原因：浏览器判定未登录时带跳转到的登录页 host+path，接口复核 401 时为 `浏览器已登录，但接口认证失败：Cookie 可能已被 Google 作废，需要重新登录`，遇到组织停用时为 `AI Studio 已被账号所属组织停用`，同时 `org_disabled` 为 `true`）；停用账户的 `state` 始终是 `disabled`，验证结果看 `authenticated`、是否组织停用看 `org_disabled`；账户列表不随 Worker 启停推送，两个 IP 池字段可能晚于实际，实时端口看 `AdminProxyPool.assignments` |
| `AdminProxyPool` | `mode`（`off`、`static`、`dynamic`）、可选 `endpoint`（脱敏的 `scheme://host`，不含端口、账号密码与会话号）、可选 `port_start`、`port_end`、`total`（静态的端口数，动态为 0）、`in_use`（静态为有浏览器占用的端口，动态为有浏览器在用的会话）、`leases`（池分配的活动租约：Worker 与登录、验证）、`capacity`（可能同时运行的浏览器数 Σ(峰值 + 排空上限)，不含登录与验证）、`shared`（被两个及以上账号同时占用的端口）、`cooling`、`external`（账户固定代理或 `PROXY` 指向、或被其他进程锁住、不再分配的端口）、`stale`（改配置前分出、浏览器还没关闭的出口）、`unpooled`（IP 池开启时仍走 `PROXY` 的 Worker）、`orphaned`（账户已删除、浏览器关闭失败仍占着的租约）、`pinned_default`（账户代理与 `PROXY` 规范值相同、因此不用 IP 池的账户数）、可选 `rotations`（动态换会话次数）、`affinity_hits`、`affinity_misses`（本次运行回到上次端口的次数与没能回到的次数）、可选 `cooling_ports`（`{port,until,level}`，最多 20 个，按恢复时间）、可选 `hot_ports`（`{port,rate_limited,accounts}`，最近 10 分钟裸 429 最多的 5 个端口）、可选 `assignments`（`{account_id,slot,kind}`：活动的池租约，`kind` 为 `worker` 或 `login`（登录、验证、导入），新账户登录中还没有账户 ID 时 `account_id` 为空）、可选 `check`（最近一次“检查全部端口”：`running`、`started_at`、可选 `finished_at`、`total`、`done`、`ok`、`distinct_ips`、可选 `duplicates`（`{ip,ports}`，出口 IP 相同的端口组，最多 20 组）、可选 `failed`（`{port,error}`，最多 50 个，错误已脱敏））、可选 `relay_limit`（本机代理中转的进程连接上限）。不含账号密码与用户名 |
| `AccountCreateInput` | `proxy`、`locale`、`timezone` |
| `AccountInput` | `label`、`enabled`、`proxy`、`locale`、`timezone` |
| `ChromeImportProfile` | `id`、`profile`、`display_name`、`email`、`locale` |
| `ChromeImportInput` | `account_ids`、`proxy`、`locale`、`timezone` |
| `BulkEnableInput` | `account_ids`、`all_disabled`（为 `true` 时启用全部停用账户，忽略 `account_ids`） |
| `BulkEnableResult` | `enabled`（本次启用数）、`skipped`（原本已启用数）、`failed`（`{id,code,message}` 数组，`code` 为 `account_busy`、`account_not_found` 等）、`remaining`（超过单次上限 1000、本次没有处理的停用账户数） |
| `BulkDeleteInput` | `account_ids`（必填；去掉首尾空白、去重，没有账户时返回 400） |
| `BulkDeleteResult` | `deleted`（本次删除数）、`missing`（号池里已经没有的账户数，已删除或从未载入，同时清除它的 403 记录，不算失败）、`failed`（`{id,code,message}` 数组，`code` 为 `account_busy`（等不到账户空闲）或其他错误码）、`remaining`（号池里还有、超过单次上限 500 本次没有处理的账户数；已经没有的账户计入 `missing`，不计入这里） |
| `AdminCooldown` | `account_id`、`account_label`、`model_id`、`until`、可选 `reason` |
| `AdminPermissionDenied` | `id`、`label`、`models`、`count`（各模型次数之和）、`first_at`、`last_at`；`models` 每项为 `model`、`channels`（返回过 403、之后还没有成功过的通道）、`count`、`first_at`、`last_at`、可选 `message`（上游原文，最多 300 字符） |
| `AdminRequest` | `id`、`model`、`account_id`、`account_label`、`state`、`started_at`、可选 `channel`、`pool`（`/ultra` 请求为 `ultra`） |
| `AdminLog` | `time`、`level`、`source`、`message`、`event`；请求事件携带 `request`，包含 `id`、`state`、HTTP `status`、`model`、`path`（`/ultra` 请求带前缀）、`pool`（`/ultra` 请求为 `ultra`）、`duration_ms`、`tool_calls`、`usage` 与诊断字段，字段口径见 [logging.md](logging.md) |
| `AdminEvent` | `type`、`data` |

`AdminAccount`、`AccountCreateInput`、`AccountInput`、`ChromeImportInput` 与 `RuntimeConfig` 的 `proxy` 字段不变：提交时接受任何支持的写法，服务端规范化后保存；读取时返回完整的规范值（含账号密码），只在管理页前端掩码显示。

`AdminStatus.state` 为 `STOPPED`、`LAUNCHING` 或 `RUNNING`；`running` 只在 `RUNNING` 为 true；`ready` 要求 `RUNNING` 且至少一个账户处于 ready 或 busy；`version` 来自构建信息；`active_requests` 是当前进程请求注册表数量。`AdminAccount.message` 保存当前状态原因，`models` 是该账户实时目录 ID。`until` 与 `started_at` 使用 RFC 3339 JSON time。

Chrome 导入列表按 `Preferences.account_info` 中的 Gaia ID 与邮箱逐个列出账号，同一 Profile 可以包含多个账号，同一邮箱只列出一次。`ChromeImportProfile.id` 为 `<Profile>/<Gaia ID>`；导入读取 `token_service` 中 service 为 `AccountId-<Gaia ID>` 的凭据。管理页列表默认不勾选，并提供全选。CLI 的 `--profile` 导入该 Profile 下的全部账号，交互编号对应单个账号。

`AccountCreateInput` 启动隔离 Camoufox 登录，邮箱由 AI Studio 页面读取。`ChromeImportInput.account_ids` 可一次选择多个账号。`AccountInput.label` 必须与不可变的 Google 邮箱 ID 一致，`locale` 与 `timezone` 必须非空，`proxy` 接受 `config.NormalizeProxy` 支持的全部写法：不带认证的 `http`/`https`/`socks5` URL、`http://用户名:密码@host:port`、`socks5://用户名:密码@host:port`、`host:port:用户名:密码`、`用户名:密码@host:port`（不写协议按 http，`socks5h` 按 `socks5`），不能带 path、query 或 fragment；三个入口在校验与 Chrome 导入之前规范化，格式错误返回 400 `invalid_account`，文案以固定前缀 `账户 proxy 无效: ` 开头、不含输入内容。保存与返回的 `AdminAccount.proxy` 是规范化后的上游 URL（例如 `1.2.3.4:8080:u:p` 保存为 `http://u:p@1.2.3.4:8080`），不是本机中转地址。管理接口与 `.env`、`account.json` 的信任级别相同（需要管理令牌或管理密码），因此照旧返回含账号密码的完整规范值，与 `proxy_api_key` 明文返回同级：服务端返回掩码的话，新账户自动处理的回写、启停与编辑草稿会把掩码写回磁盘。新增、导入、登录和验证成功后立即刷新该账户模型目录，并发布最新账户与模型事件。

`POST /api/accounts/bulk-enable` 一次请求启用一批账户：去重后只处理停用的账户，单次最多 1000 个（其余计入 `remaining`），16 个并行取得账户独占租约（最多等 3 秒，等不到记为 `account_busy`）并只把 `account.json` 的 `enabled` 改为 `true`，代理、语言、时区不变，不重建固定出口与 Worker 配置，清除停用原因；全部写完后登记一次模型变化、合并并推送一次快照，再在后台为没有模型目录的账户同步目录（与单个启用共用 16 个并发名额，整批同步完合并一次）。启用不等待目录，登录已失效的账户由目录同步标为 `auth_required`。请求体缺少 `account_ids` 且 `all_disabled` 不为 `true` 时返回 400 `invalid_account`。管理页“一键启用”与选中账户的启用按 500 个一批依次提交，两批之间可以停止。

`POST /api/accounts/bulk-delete` 一次请求删除一批账户，每个账户的效果与 `DELETE /api/accounts/{id}` 相同：从账户池移除并删除账户目录，清除它的 403 无权限记录、停用原因与新账户自动处理记录，关闭它的 Worker；每个账户仍写一条“账户已删除”，整批结束后只合并、推送一次账户与模型快照，并写一条汇总日志（见 [logging.md](logging.md)）。`account_ids` 去掉首尾空白、去重后按顺序处理：号池里已经没有的账户（已删除或从未载入）计入 `missing` 并清除它的 403 记录，不占单次名额；号池里还有的账户单次最多删除 500 个，其余计入 `remaining`。例如 600 个 ID 中前 100 个已经删除时返回 `deleted` 500、`missing` 100、`remaining` 0。原样再次提交同一列表即可继续：已经删掉的按 `missing` 处理，不占名额。同一账户被并发删除时同样按 `missing` 处理。账户 ID 按原样精确匹配（区分大小写，与 `DELETE /api/accounts/{id}` 相同），只差大小写的 ID 按 `missing` 处理，也只清除这个 ID 原样对应的 403 记录。正在处理请求的账户不在请求进行中删除：先不再接新请求，在整批共用的等待上限（约 10 秒）内等它空闲后删除；等不到的记为 `account_busy` 并撤销为删除做的准备，账户恢复正常调度。请求体没有账户时返回 400。管理页 403 无权限账户列表的“一键删除”按 500 个一批依次提交整个列表（包括折叠未显示的），两批之间可以停止；确认后先重新读取一次列表，每批提交前再对照，只提交确认时列出、提交时仍在列表里的账户（确认期间重新上传认证文件、已移出列表的账户不提交）。

`GET /api/accounts/permission-denied` 列出的账户在认证文件更新时移出，同时解除它的 403 冷却与暂停：账户目录扫描发现登录签名 Cookie（`SAPISID`、`__Secure-1PAPISID`、`__Secure-3PAPISID`）变了，或者列出的账户磁盘上的 `storage-state.json` 与本进程内存中的登录状态不同（同一登录会话里重新导出时登录签名不变）；内容比较不计 Google 定期轮换的 `__Secure-1PSIDTS`、`__Secure-3PSIDTS`、`SIDCC`、`__Secure-1PSIDCC`、`__Secure-3PSIDCC`，也不计 `https://aistudio.google.com` 的 localStorage（Worker 启动成功后写回），只有这些不同的重新导出不算更新。本服务自己写回认证文件（回写 Cookie 与 localStorage、续签）时同时更新内存，续签提交时还更新登记时记下的登录签名，都不算更新；服务停止期间换上的认证文件在启动时按登录签名对照。这套判断假定只有本进程写这个账户目录：多个进程共用一个 auth 目录时，另一个进程回写轮换 Cookie 与 AI Studio 的 localStorage 不算更新，但它写进认证文件的其他变化（轮换 Cookie 之外的 Cookie、其他 origin 的本地存储）在本进程看来与内存不同，仍按重新上传清除本进程的记录与 403 冷却；`.permission-denied.json` 也由各进程整份写出，不做合并。

`AdminOrgDisabled` 为所属组织停用了 AI Studio 的账户（Google Workspace 账号所属组织关闭了 AI Studio：载入时跳到 `access.workspace.google.com`，RPC 返回 HTTP 403、协议错误码 7、`AI Studio app is disabled`）：`id`、`label`、`source`（第一次进入的来源：启动、模型目录、验证、登录、请求、登录复核；登录复核为疑似登录失效或需要登录账户的复核请求返回 app is disabled）、`last_source`、`count`、`first_at`、`last_at`、`check_at`（下一次长间隔复查）、`message`（最近一次的原文，已打码截短），按最近一次从新到旧。这些账户的对外状态为 `org_disabled`、不参与任何调度，`POST /api/accounts/org-disabled/clear` 清除后立即恢复；识别、进入、持久化、复查与清除规则见[开发文档](development.md#组织停用-ai-studio-的账户)。

`PUT /api/accounts/{id}` 的提交顺序固定为：校验不可变邮箱 ID，取得账户独占租约，创建未发布的新固定出口，关闭当前 Worker 并把新 Worker 配置标记为 `pending`（尚未发布），在模型目录写锁内原子写入 `account.json` 并更新账户池，随后发布 Worker 配置、替换固定出口、释放租约并重建模型缓存。`account.json` 写入是唯一持久提交点。提交前的出口创建、Worker 关闭或写入错误会丢弃这份待发布配置并保持旧配置；已经关闭的 Worker 由后续请求按旧配置重建。持久写入后，新配置、Worker 配置与固定出口共同成为已提交状态。租约释放错误保留该提交状态并返回原始 unlock 错误；释放成功后记录完成日志并同步模型缓存；账户由停用改为启用且没有模型目录时，在后台补一次目录同步。

`RuntimeConfig` 字段。`response-only` 表示字段由服务器填充，客户端提交的值不参与保存：

| 字段 | 读写与生效时机 |
| --- | --- |
| `auth_states`、`proxy`、`init_timeout`、`request_timeout` | 保存值；下一次启动生成服务时使用。`proxy` 提交时接受任何支持的写法，服务端先规范化再写入 `.env`（格式错误返回 400 `invalid_config`，文案为固定文本），响应里是规范值；管理页保存后把它同步回表单 |
| `first_event_timeout` | 保存值，保存后立即生效；`0s` 为关闭，提交时省略则沿用现值 |
| `warm_worker_limit`、`max_active_workers`、`warm_startup_concurrency`、`per_account_concurrency` | 保存值，保存后立即热更新；`warm_worker_limit` 与 `max_active_workers` 只约束普通分区 |
| `account_rpm_limit` | 保存值，保存后立即热更新：单账号最近 60 秒最多发出的生成请求数（整数 `0..1000`，默认 `10`，`0` 为关闭），满窗的账号按忙碌处理、请求换到其他账号；读取时总是返回，提交时省略（旧版页面）则沿用现值；超出范围时返回 400 `invalid_config` |
| `proxy_pool` | 保存值，保存后立即热更新（不重启生成服务）：`ProxyPoolConfig` `{mode, url, port_start, port_end}`，对应 `PROXY_POOL_MODE`、`PROXY_POOL_URL`、`PROXY_POOL_PORT_START`、`PROXY_POOL_PORT_END`。读取时总是返回，`url` 与 `proxy` 一样是含账号密码的规范全文（只在这个接口返回，管理页掩码显示）；提交时省略（旧版页面）则沿用现值。`mode` 不区分大小写、保存为小写；`url` 按 `NormalizeProxy` 的全部写法规范化，`{session}` 与 `%7Bsession%7D` 不区分大小写、保存为 `{session}`；端口任何模式下都要 `1..65535` 且起始不大于结束、最多 5000 个；`static` 时 `url` 必填且不能含 `{session}`，`dynamic` 时必填且必须在用户名或密码里含 `{session}`，`off` 时可以保留地址。不合法时返回 400 `invalid_config`，文案为固定文本、不含输入内容 |
| `ultra_exclusive` | 保存值，保存后立即生效：`true` 时普通路径不使用 Ultra 号池的账户；读取时总是返回，提交时省略则沿用现值 |
| `ultra_include_pro` | 保存值，保存后立即热更新（不重启生成服务与 Worker）：`true` 时权益为 Pro 的账户并入 Ultra 号池与 Ultra 分区，按全部 Ultra 号池设置调度，账户权益不变；读取时总是返回，提交时省略（旧版页面）则沿用现值；默认 `false`。进程环境变量设置了 `ULTRA_INCLUDE_PRO` 时以环境变量为准 |
| `ultra_warm_worker_limit`、`ultra_max_active_workers` | 保存值，保存后立即热更新：Ultra 分区的常驻数（可以为 0）与峰值数（至少 1 且不小于常驻数）；读取时总是返回，提交时省略则沿用现值 |
| `primary_models`、`primary_queue_wait` | 保存值，保存后立即生效：普通号池的主用模型（字符串数组）与排队上限（时长文本，`0s` 为不排队，不超过 `request_timeout`）；读取时总是返回，提交时省略则沿用现值，空数组为清空 |
| `ultra_primary_models`、`ultra_primary_queue_wait` | 保存值，保存后立即生效：Ultra 号池的主用模型（字符串数组，默认 `["gemini-4-argon"]`，关闭时为空数组）与排队上限（规则同上）；读取时总是返回，提交时省略（`null` 或空串）则沿用现值，空数组为关闭 |
| `temporary_chat` | 保存值；下一次启动生成服务时使用 |
| `listen_addr`、`proxy_api_key` | 保存值；下一管理进程使用 |
| `active_listen_addr`、`active_proxy_api_key` | response-only；当前管理进程固定值 |
| `management_restart_required` | response-only；保存的 listen/key 与当前管理进程不同 |
| `service_restart_required` | response-only；保存的生成服务配置与当前生成服务实例不同 |

`PUT /api/config` 原子保存配置。监听地址和 API key 在管理进程重启后生效；账户路径、代理、timeout、容量与临时对话在 Stop/Start 创建的新生成服务实例中生效。`proxy` 的保存值与生效值都是规范化后的上游 URL（`config.Load`、命令行 `--proxy` 与页面提交都经同一个规范化函数），`service_restart_required` 比较时对提交值再规范化一次，写法不同但上游相同不算变化，只有上游真正变化时才为真。启动时读取最新配置；配置加载、校验、实例创建失败或启用前取消时保留原实例，切换到新实例后由它完成启动或进入 `STOPPED`。

**代理测试**：`POST /api/proxy/test` 经代理访问第三方出口 IP 服务 `https://api.ipify.org?format=json`，分别测试 Camoufox 的路径（带账号密码时经本机中转）与 Go 后台请求的路径（直连上游代理），检查账号密码是否有效、IP 是否按连接轮换。只访问 api.ipify.org，不访问 Google 的任何域名；api.ipify.org 会看到出口 IP。实现挂在生成服务管理器上，生成服务 `STOPPED` 时也能测试。请求体为 `ProxyTestInput`：

```json
{"proxy":"1.2.3.4:8080:user:pass"}
```

`proxy` 为输入框原文（任何支持的写法）；为空时测试新账户登录实际会用的出口：IP 池开启时为此刻会分给登录的池出口（静态为按挑选次序的第一个可分配端口，动态为一个新的临时会话号；不占用，响应另有 `slot`，日志与抽测相同），否则为当前生成服务实例的默认代理，即登录与 Worker 实际使用的那份配置快照（保存后还没重启生成服务时仍是旧值）。服务端先按 `config.NormalizeProxy` 规范化，再并发跑两次探测，每次用一条新连接，共用 40 秒期限（不小于中转连上游的 25 秒加 TLS 与一次请求的余量）；探测与 Go 后台请求使用同一个 `aistudio.NewProxyHTTPClient`，不跟随跳转（3xx 报 `出口 IP 服务返回 HTTP 30x`），状态码必须是 200，响应最多读 4 KiB，`ip` 用 `net.ParseIP` 校验。浏览器路径失败时，用本次中转租约自己的失败记录（只认上游层面的失败与目标为 `api.ipify.org` 的失败）替换错误说明，例如 407 显示为 `上游代理拒绝了账号密码（407），请检查用户名和密码`。成功时返回 200 与 `ProxyTestResult`：

```json
{
  "proxy": "http://1.2.3.4:8080",
  "authenticated": true,
  "relayed": true,
  "browser": {"ip": "203.0.113.7", "elapsed_ms": 812},
  "direct": {"ip": "203.0.113.9", "elapsed_ms": 640},
  "same_ip": false,
  "warnings": ["rotating_ip"]
}
```

| 字段 | 语义 |
| --- | --- |
| `proxy` | 去掉账号密码的 `scheme://host:port`（`config.RedactProxy`），不含用户名 |
| `authenticated` | 规范化后的代理是否带账号密码 |
| `relayed` | 浏览器路径是否经本机中转（带账号密码时为 `true`） |
| `browser`、`direct` | `ProxyTestProbe`：`ip`（成功时的出口 IP，失败时省略）、`elapsed_ms`（耗时毫秒）、`error`（失败时的中文说明，已去掉账号密码，最长 200 个字符，成功时省略） |
| `same_ip` | 两边都拿到 IP 且相同时为 `true` |
| `warnings` | 可选：`rotating_ip`（两边都成功但 IP 不同，代理按连接轮换）、`https_plain_browser`（`https://` 且不带账号密码，Camoufox 会以明文 HTTP 代理的方式连接） |

两条路径都失败也返回 200，失败写在各自的 `error` 里。接口本身的错误按管理端错误外壳返回：

| 状态 | `error.code` | 情况 |
| --- | --- | --- |
| 400 | `invalid_request` | 请求体不是合法 JSON |
| 400 | `invalid_config` | 代理格式无效，文案为规范化的固定错误文本，不含输入内容 |
| 400 | `proxy_required` | `没有填写代理，服务配置的默认代理也为空` |
| 409 | `proxy_test_busy` | `上一次代理测试还没有结束`：同一时间只跑一次测试 |
| 500 | `proxy_relay_failed` | 启动本机中转失败（如监听 `127.0.0.1:0` 失败）；错误文本不含代理地址与账号密码 |
| 501 | `proxy_test_unsupported` | 管理服务不支持代理测试 |

每次测试在 `service` 来源记一条日志，两边都成功为 INFO，否则为 WARN，代理只写 `RedactProxy` 的结果（格式见 [logging.md](logging.md#本机代理中转)）。

**IP 池**：`ProxyTestInput` 另有两种用法，旧请求（只有 `proxy`）行为不变：

```json
{"pool":{"mode":"static","url":"http://user:pass@isp.decodo.com:10000","port_start":10001,"port_end":10200},"samples":3}
{"account_id":"a@example.com"}
```

- `pool`（`ProxyPoolConfig`，输入框里当前的设置，不必先保存）与可选 `samples`（默认 3，最多 10）：抽测 IP 池。静态在端口段里均匀抽首、中、尾等 `min(samples, 端口数)` 个端口，动态用 `t` 开头的临时会话号（15 个字符，等于程序可能生成的会话号最大长度，能提前发现代理商的长度限制）；每个出口各测浏览器路径与后台请求路径，并发 5，共用同一个期限。响应另有 `samples`（`ProxyTestSample` 数组：`slot`（端口号或临时会话号）、`browser`、`direct`、`same_ip`）与 `distinct_ips`（不同出口 IP 数）；`warnings` 可能另有 `duplicate_ip`（有出口的 IP 相同）与 `pool_partial`（部分出口失败）。错误按该出口的上游脱敏。
- `account_id`（`proxy` 为空）：管理页账户编辑对话框里代理留空点“测试代理”时用它。账户没有自己的代理时测它实际使用的出口（`EffectiveProxy`：IP 池绑定或默认代理），响应另有 `slot`（当前的端口号或会话号，不在 IP 池时省略）；账户有自己的代理（对话框里清空了，还没保存）时测清空后 IP 池会给它的出口：静态为它的默认出口（归自己的端口或探测序列上第一个可用的端口，之后启动的 Worker 可能另分端口），动态为它的会话号，响应另有 `slot`，日志与抽测相同；IP 池关闭、或静态没有可用端口且 `PROXY` 不为空时测当前生成服务的 `PROXY`，`PROXY` 也为空时返回 400 `proxy_required`。

`POST /api/proxy/pool/check` 在后台逐个检查静态 IP 端口段里每个端口的出口 IP：只走后台请求路径，并发 10，最长 5 分钟，不占“测试代理”的单飞（检查进行中也能测试代理）；请求体 `{"pool":ProxyPoolConfig}` 省略 `pool` 或请求体为空时用当前生效的设置（IP 池热更新，保存后即生效；管理页总会带上输入框里当前的设置）。返回 202，进度与结果写进 `AdminStatus.proxy_pool.check`；已在进行时返回 409，动态 IP 或设置不合法时返回 400。结束时记一行 `IP 池检查完成`（见 [logging.md](logging.md#ip-池)）。

`POST /api/accounts/proxy/follow-default` 把 `pinned_default` 里的账户（账户代理与 `PROXY` 规范值相同）改为跟随默认代理：逐个取账户租约后把 `account.json` 的 `proxy` 写为空，与单个账户保存走同一条路径；运行中的 Worker 不重启（补登记直通租约，Go 侧在浏览器关闭前仍走 `PROXY`），关闭后再启动时改用 IP 池；正在被其他操作使用、取不到租约的账户跳过。返回 `changed`（改了的账户数）、`running`（其中仍有运行中 Worker 的）与 `skipped`（跳过的）。

`GET /api/events` 的初始顺序为 `status`、`models`、`accounts`、最近 200 条 `log`、`cooldowns`、按开始时间排序的活动 `request`。后续事件的 `data` 形状：

| `type` | `data` |
| --- | --- |
| `status` | `AdminStatus` |
| `models` | `{"models":[Model,...]}` |
| `accounts` | `{"accounts":[AdminAccount,...]}` |
| `log` | `AdminLog` |
| `cooldowns` | `[AdminCooldown,...]` |
| `request` | `AdminRequest`，状态变化时重复发送同一 ID |

管理错误统一为：

```json
{"error":{"code":"invalid_request","message":"..."}}
```

控制面错误码包括 `control_plane_forbidden`、`control_plane_origin_forbidden`、`invalid_request`、`invalid_account`、`account_not_found`、`account_busy`、`account_required`、`request_not_found` 和 `upstream_error`。

| HTTP | 管理错误码 |
| ---: | --- |
| 400 | `invalid_request`、`invalid_account`、`account_required` |
| 403 | `control_plane_forbidden`、`control_plane_origin_forbidden` |
| 404 | `account_not_found`、`request_not_found` |
| 409 | `account_busy` |
| 上游状态或 502 | `upstream_error` |

运行状态机如下。目录缓存为 `runtime/model-catalogs/` 下的写盘缓存，后台同步只低速刷新缺失、不一致、疑似登录失效、失败待重试与过期的账户：

```text
process start
  -> control plane ready
  -> STOPPED

runtime assembly
  -> restore catalogs, benefit tiers and tier headers from runtime/model-catalogs (one lock)

POST /api/control/start
  -> LAUNCHING
  -> load CachedModels
  -> queue missing / stale / suspected / expired accounts for background sync
     (2 concurrent, >=1s apart; 4 concurrent while no catalog is cached)
  -> start login recheck (suspected 401s and auth_required accounts)
  -> if cache is empty, wait for the first non-empty live catalog
  -> prewarm up to WARM_WORKER_LIMIT workers
     with WARM_STARTUP_CONCURRENCY bootstraps
  -> first worker ready
  -> RUNNING
  -> continue background catalog sync (idle accounts only; upstream 429 pauses globally,
     egress-limited accounts are deferred) and remaining worker prewarm

request
  -> resolve model and endpoint capability
  -> acquire one PER_ACCOUNT_CONCURRENCY slot
  -> reserve one ACCOUNT_RPM_LIMIT slot (generateContent only)
  -> prepare WAA proof
  -> record generation send (60s window)
  -> send MakerSuite RPC
  -> stream frames
  -> release slot

POST /api/control/stop
  -> cancel launch or active requests
  -> bound catalog sync shutdown to 2s (flush the catalog cache)
  -> close WAA workers
  -> bound unfinished lifecycle-transition wait to 12s
  -> STOPPED
```

管理状态字段使用大写服务状态 `STOPPED`、`LAUNCHING`、`RUNNING`。请求状态使用 `queued`、`running`、`completed`、`cancelled`、`failed`。视频状态使用 `queued`、`completed`、`failed`，对应 progress `0` 或 `100`。

模型目录保存在写盘缓存 `runtime/model-catalogs/`：`index.json` 记录每个账户的目录 blob 键、权益、最近一次成功同步、连续失败与下次重试、最近一次被标需要登录的时间与当时认证文件的修改时间，`blobs/` 按内容去重保存 `ListModels` 原文（恢复时用同一个解析器重新解析）与 Build 目录。写盘最多每 30 秒一次，跨进程加锁、先写 blob 再写 index、同目录临时文件加改名；index 损坏时改名为 `index.json.corrupt-<unix>` 保留、按空缓存继续。新 generation（`runtimeGeneration` 表示的一次生成服务实例）装配时一次恢复账户池与协议客户端的目录与权益（含 Pro/Ultra 的 `X-AIStudio-G1-Tier`），权益不符、blob 缺失或版本不符的账户不恢复、排进队列。启动只把缺失、不一致、疑似登录失效与过期（上次成功 + 24 小时 ×U(0.9, 1.1)）的账户排进后台队列，不再全量同步；后台同步只取空闲账户（有进行中的请求推后 2 分钟），常规 2 并发、两次开始至少间隔 1 秒，完全没有缓存时 4 并发、间隔 500 毫秒；上游 429 全局暂停 30 秒起翻倍到 30 分钟（连续 10 次成功后清零），出口限流判定成立时该出口的账户推后，其他失败按账户退避 1 分钟起到 6 小时，疑似登录失效、等待复核的账户推后到复核之后；ListModels 返回空列表时账户已有目录就保留原目录，连续 3 次且跨 1 小时才接受。当前 generation 有目录时直接进入 Worker 预热；完全没有目录时等第一个非空结果。后台结果最多每 30 秒合并应用一次。第一个 Worker 就绪后重新读取当前缓存，并在持有账户与配置变化锁时把服务状态设为 `RUNNING`。重启不再清掉 `auth_required` 与账号全局冷却（原先由启动全量同步顺带清掉），需要登录的账户由登录复核恢复；后台刷新目录成功同样不清账号的全局冷却（全局每日限额、403 自动暂停等到期自然恢复），管理页刷新目录与验证照旧清。第一轮后台同步只算启动时就到期的缺失账户，重启前同步失败、还在退避中的到期后照常重试；一个目录都没有恢复出来时重启当作一次重试，缺失账户立即同步。删除的账户超过 7 天的条目在每小时一次的清理里从 index 与内存一并删掉。

账户状态：

| 状态 | 调度语义 |
| --- | --- |
| `ready` | 认证有效且存在可用槽位 |
| `busy` | 就绪账户存在独占操作、认证刷新或活动请求；调度仍按 `PER_ACCOUNT_CONCURRENCY` 判断剩余槽位。需要登录或不可用的账户有活动请求时仍显示原状态。每分钟请求已满（`ACCOUNT_RPM_LIMIT`）只让调度把账户当作忙碌，不改变账户状态 |
| `cooldown` | 账户的全局 `*` 冷却仍有效，或至少有一个模型在账户可用的每个启用通道上都在冷却（Playground 键为模型 ID，Build 键为 `build:<模型>`；只有一个通道冷却、另一个通道还能用时仍为 `ready`）；忙碌优先于冷却。状态摘要 `accounts.cooldown` 按同一口径计数；单个模型 scope 的冷却只影响对应请求的候选分类 |
| `auth_required` | 账户级认证失败 |
| `unavailable` | 当前运行时不可用 |
| `disabled` | 配置已停用 |

认证状态写回携带 `authGeneration` 与 `checkedAt`，仅应用账户对象、`authGeneration` 值与时间均匹配当前状态的结果；相同时间以 `ready` 为最终状态。模型访问和冷却写回携带 `modelAccessGeneration` 与 `checked_at`，仅应用 `modelAccessGeneration` 值匹配且时间不早于当前状态的结果；相同时间以 `verified` 为最终状态。账号全局 `*` 上剩余不到 2 分钟的分钟限额或限流冷却，成功写回（包括非生成操作的成功清除）只推进 `*` 的 `checked_at`，不清除这条冷却，等它自行到期；其他冷却照旧被成功清除，包括剩余不少于 2 分钟的冷却与转录上传失败等暂时故障写的 30 秒全局冷却。`verified` 表示该账户已经成功调用对应模型或能力。

`ModelAccessKey(scope, model)` 先移除 `models/` 前缀；空 scope 返回 canonical model ID（规范化模型 ID），非空 scope 返回 `<scope>:<canonicalModelID>`。各能力使用以下值：

| 能力 | scope | 成功记录 |
| --- | --- | --- |
| 普通生成 | `<modelID>` | 规范事件 `EventFinish` 到达时写 `verified` |
| CountTokens | `count-tokens:<modelID>` | 清 scope 冷却，保留普通生成资格 |
| Transcribe | `<modelID>` | 非空 text 或 segments 写 `verified` |
| Live 文本 | `<modelID>` | setup complete 写 `verified`；每次 text 开始一次模型资格检查 |
| Live 音频/图像 | `bidi-media:<modelID>` | media 开始媒体资格检查；Live text 使用普通模型 scope |
| Robotics | `bidi-media:<modelID>` | text 开始模型资格检查 |

Code 7 保留当前 operation scope 的 verified 状态；认证失败更新账户级 `auth_required`。上传、临时文件清理等非生成失败使用全局 `*` 冷却。

普通流式生成在消费到规范事件 `EventFinish` 时写入 verified。正文、reasoning、工具、usage 与首事件用于输出和性能统计；终态前断流、客户端取消或错误保持原有验证状态。

Bidi setup 成功使用 lease（本次会话持有的账户租约）的 `checkedAt`。此后每个会更新模型资格的轮次分配会话内严格递增的 attempt 时间，`turn_complete` 消费对应 attempt 并写入资格。Code 7 不更新模型资格。Bidi 认证成功与 401 使用相同 attempt 顺序，setup 后较晚返回的 401 可以更新账户为 `auth_required`。

确认浏览器进程退出后，将 process 和 Worker 状态设置为 `closed`。关闭失败时保留 Worker、runtime lease、warm 标记与 generation（Worker 实例版本号）；后续 Stop 即使服务状态已经是 `STOPPED`，也会再次执行 Worker reset。Camoufox 关闭阶段上界为 BiDi 3 秒、进程终止与等待 5 秒、profile 删除 2 秒；各阶段错误使用 `errors.Join` 保留。

模型目录 fan-out 绑定当前生成服务的 context。启动失败、启动取消或 Stop 会取消全部目录任务，并最多等待 2 秒确认后台目录协程退出。Stop 遇到尚未完成的 `LAUNCHING` 或其他启动/停止切换时，等待 transition channel（切换完成通知 channel）的上界为 12 秒；该上界覆盖 2 秒目录退出与有界 Worker 清理，超时错误与清理错误使用 `errors.Join` 返回。

按需热替换先启动 pending Worker（正在启动、尚未发布的替代 Worker），再关闭旧 Worker；旧实例成功退出后，替代 Worker 才成为当前 Worker。旧实例关闭和替代 Worker 回收同时失败时，两者都保留等待再次清理，并各占一个活动容量槽；达到容量上限后停止新建 Worker。完整生成服务 Stop/Start 的顺序为：Start 创建新生成服务实例前先重试停止旧实例，旧 PID 未退出时返回停止错误并保留原实例。

管理状态使用 `STOPPED`。该状态下生成与计数端点返回 `503 service_stopped`。Code 7 不清除账户或 operation scope 的成功状态。Worker 进程故障、Worker 被替换与协议 Code 5 会重建当前账户 Worker：失败的 Worker 在后台关闭（同一 Worker 版本只关闭一次，关闭前等同账户其他进行中的请求结束，期间该账户不接新请求）。不指定账户、也没有文件绑定的生成请求最多等 2 秒（`PRIMARY_MODELS` 中的模型最多 1 秒且不超过剩余排队预算的四分之一），关闭完成就在原账户重放一次，否则换到其他账户重试；换号时没有其他可用账户则回到原账户，等关闭完成后重放。这次失败计入连续失败上限（5 次）后就不能再换号的请求不再只等 2 秒，一直等关闭完成后在原账户重放；换号次数用完或到连续失败上限时还有没等到关闭就换走的原账户，同样回到原账户等关闭完成后重放；换号后排队等其他账户时原账户关闭完成，即不再排除原账户，按原来的排队位置重新选号，原账户与其他账户哪个先能用就用哪个。回到原账户等待期间请求超时或被取消时按超时（504）或取消结束。指定账户、文件绑定的请求以及 Veo 等关闭完成后在原账户重放一次；Live 不指定账户、还有其他候选时（转录只在协议 Code 5 时；正在后台关闭失败 Worker 的候选不算）同样最多等 2 秒，等不到就换号，否则等关闭完成后重放（转录受 `REQUEST_TIMEOUT` 限制；Live 最多等 `REQUEST_TIMEOUT`，到期返回 503，关闭没有结束时按同一账户重试同样最多等 `REQUEST_TIMEOUT`）。Live 会话建立时取账户租约最多等 `REQUEST_TIMEOUT`（候选都在后台关闭或并发都已占满时），到期返回 503；会话建立后不受这个期限限制。号池中没有任何账户符合请求的模型、方法、能力、通道与权益时按请求本身的原因返回：请求指明模型时为 HTTP 404 模型不存在，否则为 HTTP 400（Gemini status 为 `INVALID_ARGUMENT`）。号池一侧的暂时性原因返回 HTTP 503（OpenAI type 为 `server_error`，Anthropic type 为 `api_error`，Gemini status 为 `UNAVAILABLE`）：支持请求的账户都处于需要重新登录、不可用或已停用状态，候选账户的 runtime 被其他进程占用，号池为空或模型目录尚未加载；管理日志里的错误逐个列出账户、状态与原因，客户端只收到官方措辞。候选全部冷却时返回 HTTP 429，并在 `Retry-After` 中给出距最早恢复时间的秒数。`PRIMARY_MODELS` 中的模型排队超过 `PRIMARY_QUEUE_WAIT` 仍没有空闲的账号或 Worker 时同样返回 HTTP 429 与 `Retry-After`（排队上限限定在 1 到 30 秒，上限为 0 时 5 秒），不换号；排队上限整个请求共用一份、按实际经过的时间计，换号重试的尝试与退避都算在内，请求自己现场冷启动的浏览器启动时间不计；管理日志、请求列表与用量账本记录“号池繁忙：模型 X 暂时没有空闲的账号或 Worker，请稍后重试”，客户端只收到官方的限流措辞。主用模型按请求的号池查找：`/ultra` 只查 `ULTRA_PRIMARY_MODELS`（默认 `gemini-4-argon`），`ULTRA_EXCLUSIVE=true` 的普通路径只查 `PRIMARY_MODELS`，`ULTRA_EXCLUSIVE=false` 的普通路径先查 `PRIMARY_MODELS`、没有再查 `ULTRA_PRIMARY_MODELS`。按 Ultra 列表命中的请求没有额度时立即返回 HTTP 429 与 `Retry-After`，不在号池里挨个换号：候选全部冷却时立即按全部冷却返回（不再等待 1 分钟内恢复的冷却，降级判定的 Playground 优先选号仍先改为不限通道）；请求中遇到额度 429（输入超过账号上限的 429 不算）后，额度失败分两类计数：每日类（每日限额，以及剩余不少于 15 分钟的冷却）最多再换 2 次账号，限流类（分钟限额、没有额度信息的 30 秒限流）最多再换 5 次；租约账号在请求发出前进入冷却时，只有拦下它的冷却剩余不少于 15 分钟才记一次每日类，短冷却照常换号、不计数。每次额度失败后紧接着的那次选号只用已启动的 Worker，不现场冷启动备用账号，某一类额度失败达到它的换号上限（原因分别为“额度失败已达换号上限”“限流失败已达换号上限”）、其余候选全部冷却、没有已启动的 Worker，或没有其他可换的账号（能服务该模型的账号都已试过、剩下的暂时不可调度，或换号次数用完）时返回 `PrimaryQuotaError`。`Retry-After`：其余候选全部冷却时为最早恢复（不设上限），每日类达到换号上限或没有其他可换的账号时为 5 秒与本请求遇到的最早额度恢复中较早者，限流类达到换号上限时为本请求遇到的限流冷却中最早的恢复（不超过 1 分钟，不知道时 5 秒），没有已启动的 Worker 时为 5 秒，都至少 1 秒。管理日志写明原因、额度失败与限流失败次数、总耗时，客户端只收到与全部冷却相同的官方限流措辞，不含上游原文。租约账号在请求发出前进入冷却（`AccountCoolingDownError`，请求没有发到上游，多半是同账号别的请求刚遇到额度 429）时，任何模型都按已冷却换号，不计入连续失败上限、不退避；普通模型（以及 Ultra 主用模型被剩余不到 15 分钟的冷却拦下、此前也没有额度失败时）不能再换号时返回 HTTP 429，`Retry-After` 为拦下它的冷却的恢复时间；Ultra 主用模型被剩余不少于 15 分钟的冷却拦下时按上文记一次每日类额度失败，以额度不足结束时 `Retry-After` 按上文每日类的规则。所有带 `Retry-After` 的 429 在建议间隔上随机往后加 0 到 min(建议间隔 ÷ 5 秒, 3) 秒（建议间隔不到 5 秒不加），免得冷却一到期所有客户端同时重试。

管理操作（新增、登录、验证、批量启用）的单账户同步不受暂停与速率限制，结果照常写缓存、排下次刷新，并立即发布当前快照。需要登录、停用、不可用的账户移出后台队列，恢复就绪后由对照账户池（每分钟一次）或登录复核的放回重新入队；删除账户同时移除它的任务、嫌疑与登录复核记录。目录同步遇到 401 时同样先记疑似登录失效、由登录复核确认，确认前不移出队列。

模型目录投影：

| 规则 | 结果 |
| --- | --- |
| OpenAI | `GET /v1/models` 返回 OpenAI model list |
| Anthropic | `GET /v1/models` 携带 `Anthropic-Version` 时返回 Anthropic model list |
| 单模型查询 | `GET /v1/models/{model}` 按正式 ID、`models/` 前缀、目录别名或后缀别名（含 Omni 视频后缀组合）查找，携带 `Anthropic-Version` 时返回 Anthropic model 对象；不存在时 OpenAI 返回 404 `model_not_found`，Anthropic 返回 404 `not_found_error`。`GET /v1beta/models/{model}` 规则相同，返回 Gemini 格式 |
| Gemini | 模型名称使用 `models/<ID>` |
| 多账户同模型 | generation methods 与能力选项取并集 |
| 多账户 token limit | 输入和输出上限分别取正数最小值 |
| 模型别名 | 来自 ListModels field 57 |
| 请求匹配 | model ID/alias、method、capability、AccessModes、账户权益与运行状态决定候选集合 |
| 候选排序 | `verified` 与目标模型首事件耗时用于排序，未验证账户仍可进入候选 |
| capability 约束 | 端点要求的 capability 与账户实时能力同时命中 |

管理模型对象完整字段：

```json
{
  "id": "gemini-example",
  "name": "Gemini Example",
  "description": "...",
  "methods": ["countTokens", "generateContent"],
  "input_token_limit": 1048576,
  "output_token_limit": 65536,
  "capabilities": {"thinking": true, "capability_code_25": true},
  "capability_options": {"aliases": ["gemini-example-latest"]},
  "access_modes": [3, 4],
  "paid": true,
  "build_unary": true
}
```

`build_unary` 只出现在管理目录（`GET /api/models` 与管理事件流的 `models` 事件）：模型需要订阅权益、在 Build 通道只能一次性返回整段回复时为 `true`。服务配置页据此列出可以加入 `STREAM_PLAYGROUND_MODELS` 的模型（只列 `channels` 同时含 `playground` 与 `build` 的模型），见 [Build 通道](build.md)。

OpenAI `GET /v1/models`：

```json
{
  "object": "list",
  "data": [{
    "id": "gemini-example",
    "object": "model",
    "created": 0,
    "owned_by": "google",
    "name": "Gemini Example",
    "description": "...",
    "supported_generation_methods": ["countTokens", "generateContent"],
    "input_token_limit": 1048576,
    "output_token_limit": 65536,
    "capabilities": {},
    "capability_options": {},
    "access_modes": [],
    "paid": true,
    "channels": ["playground", "build"]
  }]
}
```

请求携带 `Anthropic-Version` 时，同一路由返回：

```json
{
  "data": [{"id":"gemini-example","type":"model","display_name":"Gemini Example","created_at":"1970-01-01T00:00:00Z"}],
  "has_more": false,
  "first_id": "gemini-example",
  "last_id": "gemini-example"
}
```

Gemini `GET /v1beta/models` 返回 `{"models":[...]}`，单模型路由直接返回一个对象。字段为 `name`、`displayName`、`description`、`supportedGenerationMethods`、`inputTokenLimit`、`outputTokenLimit`、可选 `capabilities`、`capabilityOptions`、`accessModes`、`paid`、`channels`。

`GET /v1beta/models/{model}` 只按 canonical model ID 查找；生成、计数、视频、转录与 Bidi 同时接受 canonical ID 和 `capability_options.aliases` 中的 alias；alias 在调度和发送上游前换成对应的 canonical ID。

管理模型中的 `description`、token limits、capabilities、capability options、access modes、false `paid` 与 false `build_unary` 使用 `omitempty`；OpenAI 和 Gemini 响应始终包含身份、methods 与 token limits，并在 map/slice 非空或 `paid=true` 时增加对应扩展字段。

管理目录合并全部账户的上游实时模型集合，并按 ID 排序。公开目录保留至少一个启用账户具有访问资格、且当前公开协议承载其调用方法的模型；启用 Build 通道时同时包含 Build 独有的可生成模型，`channels` 列出可调用该模型的通道，见 [Build 通道](build.md)。短期冷却和忙碌状态由请求调度处理。多账户同 ID 的 methods、capabilities、capability options 和 access modes 取并集，`paid` 取逻辑 OR，正数 token limit 取最小值。调度使用上游 methods、capabilities、access modes、账户权益和当前运行状态。

主要请求格式：

| 端点 | 必需字段 | 主要结果 |
| --- | --- | --- |
| `/v1/chat/completions` | `model`、非空 `messages` | Chat completion 或增量 chunk |
| `/v1/responses` | `model`、`input` | Response object 或 `response.*` 事件 |
| `/v1/files` | multipart `file`、`purpose` | OpenAI file object |
| `/v1/messages` | `model`、非空 `messages`、`max_tokens` | Anthropic message 或 message 事件 |
| `:generateContent` / `:streamGenerateContent` | 非空 `contents` | Gemini candidates、usage 与 grounding metadata |
| `/v1/images/generations` | `model`、`prompt`、固定 `n=1` | `b64_json` 或 data URL |
| `/v1/audio/speech` | `model`、`input` | WAV、PCM 或 MP3 body |
| `/v1/audio/transcriptions` | multipart `file`；`model` 默认 `gemini-3.5-transcribe` | 文本或转录 JSON |
| `/v1/videos` | `model`、`prompt` | 长任务对象，随后轮询并下载内容 |

Anthropic assistant prefill 以最后一条 `assistant` message 表示。AI Studio 当前没有对应生成前缀字段，`/v1/messages` 与其他三个入口一样按续写处理：以模型文字消息结尾的历史末尾补一条请模型从上一条消息结尾处接着写、不重复已有内容的用户消息（见上文分轮修正）。以工具调用结尾的历史仍按缺少工具结果处理。

流式请求在写出响应头前最多等待首个事件 10 秒（主用模型为对应列表的排队上限加 5 秒与 10 秒中的较大者，按请求的号池取：`/ultra` 上的 `ULTRA_PRIMARY_MODELS` 为 `ULTRA_PRIMARY_QUEUE_WAIT`，普通路径上的 `PRIMARY_MODELS` 为 `PRIMARY_QUEUE_WAIT`，`ULTRA_EXCLUSIVE=false` 时普通路径上只在 Ultra 列表里的模型为 `ULTRA_PRIMARY_QUEUE_WAIT`）：首个事件之前出现的参数错误、无可用账号、全部冷却、号池繁忙、Ultra 主用模型额度不足、降级拒绝等错误按非流式返回 HTTP 状态与官方格式的错误对象；等待期内没有事件时照常开始推流，之后的错误仍在流内返回（号池繁忙的 429 以 HTTP 状态返回同样是尽力而为：排队预算整个请求共用一份，换号重试与退避都计入，通常早于这个等待；Ultra 主用模型的请求自己现场冷启动时，等待按启动时长延后（启动期间不开始推流；合计不超过 `INIT_TIMEOUT`，且从开始等待算起最多 50 秒，等待本来更长时按它），冷启动后才遇到的额度不足通常同样以 HTTP 状态返回；普通号池的主用模型在请求自己现场冷启动或候选冷却延后了截止时间而晚于这个等待时，在流内返回）。既没有系统提示也没有对话内容的请求在选择账户前返回 400。

四套生成入口共享同一规范请求，输入映射如下：

| 能力 | OpenAI Chat | OpenAI Responses | Anthropic | Gemini |
| --- | --- | --- | --- | --- |
| system | `system` / `developer` messages | `instructions` 和 system/developer message items | `system` 字符串或 text blocks | `systemInstruction` text parts |
| text | 字符串或 text content part | 字符串、message item | 字符串或 text block | Part `text` |
| image/document | Base64 data URL、`file_id` | `input_image`、`input_file` | base64、URL 或 file source；document 另有 text 与 content source | `inlineData`、`fileData` |
| audio input | `input_audio` Base64 | message content 中的 `input_audio` | base64 document source | `inlineData` |
| YouTube | `video_url` / `input_video` | `input_video` | URL source | `fileData.fileUri` |
| function call | assistant `tool_calls` | `function_call` item | `tool_use` block | `functionCall` Part |
| function result | tool message | `function_call_output` item | `tool_result` block | `functionResponse` Part |
| structured output | `response_format` | `text.format` | `output_config.format`、`output_format` | `responseMimeType` 与 response schema |
| thinking | `reasoning_effort` 或 `reasoning.effort` | `reasoning.effort` | `thinking.budget_tokens`、`output_config.effort` | `thinkingConfig` |

Gemini 附件与 `predictLongRunning` 的图片输入接受 `inlineData` / `inline_data`、`fileData` / `file_data`、`mimeType` / `mime_type` 和 `fileUri` / `file_uri`。同一别名对同时出现时，外层优先选择驼峰对象，内层优先选择非空驼峰值。

媒体 Base64 输入接受标准和 URL-safe 字母表、可选的 `=` 填充，以及 `data:<MIME>;base64,` 前缀。GIF 内联图片和 OpenAI 视频 `input_reference` 表单附件提取首帧，按逻辑画布尺寸与帧偏移编码为 PNG 后发送。透明首帧保留透明背景；不透明首帧的未覆盖区域使用全局色表中的背景色。

OpenAI Chat 与 Anthropic 省略转换后没有 parts 的空历史消息；纯空白文本、工具调用、工具结果及媒体保留原有内容。

生成参数映射：

| 参数 | 规则 |
| --- | --- |
| OpenAI max tokens | `max_completion_tokens` 优先于 `max_tokens` |
| Anthropic max tokens | `max_tokens` 映射 generation config field 4 |
| Gemini max tokens | `maxOutputTokens` 映射 generation config field 4 |
| temperature / topP / topK / seed | 映射 generation config fields 5 / 6 / 7 / 19 |
| stop sequence | 映射 generation config field 2 |
| stop sequence 命中 | 协议核心在正文事件流中匹配并返回实际命中的序列；最多 32 个、单个最长 1024 字节，超过时返回 400 |
| structured output | MIME type 映射 field 8，Schema 映射 field 9 |
| Gemini `mediaResolution` | 映射 generation config field 18；Build 发送枚举名 |
| Gemini `safetySettings` | 映射 GenerateContent field 3；Build 发送 `safetySettings`；OpenAI Chat、Responses 与 Anthropic 没有对应字段，按默认值发送（非图片模型四类 `OFF`，图片模型不发送） |
| OpenAI Chat `n` | 仅接受省略或 `1` |
| OpenAI Chat `parallel_tool_calls` | `false` 时一次回复最多一个函数调用（见上文工具选择） |
| OpenAI Chat `logprobs` / `top_logprobs` / `logit_bias` | 上游没有对应参数，任何取值都忽略；不返回对数概率，非流式 choice 的 `logprobs` 恒为 `null` |
| OpenAI Chat frequency / presence penalty | 上游没有对应参数，任何取值都忽略（SillyTavern、LobeChat 等客户端常带非 0 值） |
| OpenAI Chat function `strict` | `true` 时核对上游返回的参数（见上文工具选择） |
| Responses `parallel_tool_calls` | 写入响应元数据；`false` 时一次回复最多一个函数调用 |
| Responses `truncation` | 省略、`disabled` 或 `auto`；`auto` 在本地估算超过模型输入上限一半时按 CountTokens 权威计数，超出上限则从最早的完整轮次开始删除（不拆开工具调用与结果，最多计数 6 次），最新一轮本身超限时返回 400 |
| Responses function `strict` | `true` 时核对上游返回的参数 |
| Responses `store` | 省略或 `true` 时保存当前进程会话节点；`false` 只返回本次结果 |
| Gemini frequency / presence penalty | 上游没有对应参数，任何取值都忽略 |
| Gemini `candidateCount` | 仅接受省略或 `1` |
| Gemini `responseLogprobs` / `logprobs` | 上游没有对应参数，任何取值都忽略，响应不含 `logprobsResult` |
| Gemini `googleSearchRetrieval` | 接受任意对象，映射为 Google Search；`dynamicRetrievalConfig`（旧版动态检索阈值）没有对应参数，忽略 |
| Anthropic `thinking` | `enabled` 携带 `budget_tokens`，支持 thinking budget 的模型直接写入预算，只支持 thinking level 的模型按 0、1024、8192 以内与更大预算分别使用 minimal、low、medium、high；`adaptive` 使用模型默认思考 |
| Anthropic thinking capability | 模型既不支持 thinking budget 也不支持 thinking level 时形成 `invalid_request_error`；非流式返回 HTTP 400，流式返回 Anthropic error event |
| Anthropic thinking type | `disabled` 按最低思考强度（minimal）生成，不返回思考正文（保留思考签名），优先于 `output_config.effort`；未知 type 返回 `400 invalid_request_error` |
| Anthropic 结构化输出 | `output_config.format` 与 beta 写法 `output_format` 为 `{type:"json_schema",schema}`，两者同时出现时以 `output_config.format` 为准；映射为与 OpenAI `response_format` 相同的 JSON 模式与 response schema（Playground 无法编码的 Schema 同样降级，仍按 JSON 输出）；`schema` 为 `null` 或省略时只保留 JSON 模式；其他 `type` 返回 `400 invalid_request_error` |

### Omni 视频设置与模型名后缀

能输出视频的 Omni 模型（目录带 Interaction 配置 field 79，且列出 `video_output_resolutions`、`video_duration_range` 或 `video_tasks`，如 `gemini-omni-1.1-flash`）接受下列视频设置；其他模型忽略这些字段，也不解析视频后缀。

| 协议 | 写法 |
| --- | --- |
| Gemini GenerateContent | `generationConfig.responseFormat`（单个对象或数组）的 `video:{aspectRatio,duration,resolution,frameRate,fps}`；扩展字段 `generationConfig.videoConfig:{task,aspectRatio,durationSeconds,duration,resolution,frameRate,fps}`，同一项以 `videoConfig` 为准；整个 `generation_config` 用 snake_case 时同样接受 |
| OpenAI Chat Completions、OpenAI Responses、Anthropic Messages | 顶层 `video_config:{aspect_ratio,duration_seconds,duration,resolution,frame_rate,fps,task}`（OpenAI SDK 的 `extra_body` 会合并到顶层）；`duration_seconds` 与 `duration` 同时出现时以前者为准 |
| Gemini Interactions | `response_format` 的 `{type:"video",aspect_ratio,duration,resolution,frame_rate,fps,mime_type?:"video/mp4"}` 与 `generation_config.video_config:{task,aspect_ratio,duration_seconds,duration,resolution,frame_rate,fps}`，同一项以 `video_config` 为准 |
| 模型名后缀 | `-16x9`、`-9x16`（宽高比），`-360p`、`-720p`、`-1080p`、`-4k`（分辨率），`-<N>s`（时长，1–3 位数字，如 `-8s`），`-t2v`、`-i2v`、`-r2v`、`-edit`、`-extend`（任务）；优先于以上参数；目录 ID 与 Omni 的目录别名都可以带 |

取值与整理规则：

| 项 | 规则 |
| --- | --- |
| 宽高比 | `auto`、`ASPECT_RATIO_UNSPECIFIED`、空或省略时不指定（官网 Auto）；`16:9`、`9:16`、`16x9` 与 Gemini 枚举名 `ASPECT_RATIO_SIXTEEN_BY_NINE` 等；其他比例按比值对数换成模型选项（`video_aspect_ratios` 与上游枚举的交集）中最接近的，距离相同取较大值（`1:1` → `16:9`），管理日志记 `视频宽高比 4:3 不是模型选项，按最接近的 16:9 发送`；无法解析时 400 |
| 分辨率 | `360p`、`720p`、`1080p`、`4k`，不区分大小写，接受 `VIDEO_RESOLUTION_` / `RESOLUTION_` 前缀；模型选项先换成像素高度最接近的上游取值（目录的 `368p` 即 `360p`，请求 `360p` 或 `368p` 都按 `360p` 发送、不记说明）；其他高度（`2160p`、`1440p`）换成模型选项中最接近的并记一行说明；省略、`auto` 或 `VIDEO_RESOLUTION_UNSPECIFIED` 时由编码取所选账户的默认分辨率；无法解析时 400 |
| 时长 | JSON 数字或 `"8s"`、`"8"` 字符串，四舍五入为整秒；负数或无法解析时 400；省略或 0 时不带，由上游按模型默认（与上游 `interactionVideoFormat` 一致；上游试用页的时长滑块停在目录默认值时同样不发送）；超出 `video_duration_range` 的范围或不在步长上（从下限起算）时换成范围内最接近的合法秒数（距离相同取较大值，超过上限时退一个步长），不返回 400，管理日志记 `视频时长 30 秒不是模型可选的时长（4–15 秒、步长 1），按最接近的 15 秒发送`；目录只有 `video_durations_seconds` 时取其中最接近的 |
| 帧率 | 数字或 `"24fps"`，各协议都接受 `frame_rate`（`frameRate`）与 `fps` 两种字段名，同时出现时以前者为准；只校验，不发送：不在 `video_frame_rates`（目录没有时为 `24fps`）中时 400 `video frame_rate 30 is not supported; this model only supports 24fps` |
| 任务 | `text_to_video`、`image_to_video`、`reference_to_video`、`edit`、`extend`，不区分大小写，接受 `VIDEO_TASK_` 前缀；`unspecified`、`auto`、`VIDEO_TASK_UNSPECIFIED` 与省略相同（官网 Unspecified）；其他值 400；合并目录的 `video_tasks` 没有列出时 400（所选账户的目录没有列出时照发，见 [Omni Interaction](#omni-interaction)）；对话里已有会编码为模型 step 的模型消息时 400 `Video task is only supported on the first turn of a conversation`（与官网提示一致；只有附件或只有思考的模型消息不算）；`image_to_video`、`reference_to_video` 需要至少一张图片、`edit`、`extend` 需要至少一段视频（模型消息里会移到用户 step 的附件同样计入，类型不明的文件引用两种都算），缺少时 400 `video task edit requires an input video`；`edit` 的请求不带输出格式，同时设置了宽高比、时长或分辨率时记一行 `视频任务 edit 不带输出格式，宽高比、时长与分辨率不发送` |

错误在选号前返回（OpenAI、Responses 为 OpenAI 格式，Anthropic 为 Anthropic 格式，Gemini 与 Interactions 为 Gemini 格式），说明为英文原文；整理说明写进该请求的管理日志（WARN，`来源=service`），不影响响应。

模型名后缀可与 `-low`、`-high`、`-minimal`、`-medium`、`-128`、`-nothinking` 组合，顺序不限、不分大小写，同一项出现多次时以最靠后的为准，例如 `gemini-omni-1.1-flash-9x16-720p-8s-low`。解析先按原有后缀进行（与原先完全相同）；解析不到时再连同视频后缀解析，且结果必须是能输出视频的 Omni 模型，其他模型名带 `-720p`、`-8s` 等字样时保持原名，目录 ID 本身带这类字样时按目录 ID。视频后缀也可以加在 Omni 的目录别名（`capability_options.aliases`）后面，别名与某个目录 ID 同名时按该 ID；按别名调用时请求与响应里的模型名为去掉后缀的别名，调度前再换成目录 ID。Omni 的思考等级后缀优先于请求参数，不带等级后缀时沿用请求参数与模型默认等级（不像文本模型那样设为最高）；`-online` 对 Omni 不生效。响应里的模型名为去掉后缀的目录模型名。`/v1/models` 不展开这些组合，单模型查询能查到。

### OpenAI Chat Completions

`POST /v1/chat/completions` 请求字段：

| 字段 | 类型与语义 |
| --- | --- |
| `model` | 必需模型 ID |
| `messages` | 必需非空 message 数组 |
| `stream` | boolean |
| `stream_options.include_usage` | 在 finish chunk 后发送 usage-only chunk |
| `tools` | function 或 Google server tool 数组 |
| `tool_choice` | 省略/`auto`/`none`/`required`/指定函数 |
| `web_search_options` | 对象，开启 Google Search；`search_context_size`（low/medium/high）与 `user_location` 以系统指令提示传给上游（软约束） |
| `temperature`、`top_p` | 可选采样值 |
| `max_tokens`、`max_completion_tokens` | 后者优先 |
| `frequency_penalty`、`presence_penalty` | 接受任意值，忽略 |
| `n` | 省略或 `1` |
| `parallel_tool_calls` | `false` 时一次回复最多一个函数调用 |
| `logprobs`、`top_logprobs` | 接受任意值，忽略；非流式 choice 的 `logprobs` 为 `null` |
| `logit_bias` | 接受任意值，忽略 |
| `stop` | string 或 string array；空字符串从条件中移除 |
| `response_format` | `{type:"text"}`、`{type:"json_object"}` 或 `{type:"json_schema",json_schema:{schema}}` |
| `reasoning_effort` | thinking effort |
| `reasoning.effort` | nested thinking effort；与顶层字段映射到同一配置 |
| `seed` | 64 位整数 |
| `video_config` | Omni 视频设置扩展字段，见 [Omni 视频设置与模型名后缀](#omni-视频设置与模型名后缀) |

message 字段为 `role`、`content`、可选 `name`、`tool_call_id`、`tool_calls`。assistant tool call：

```json
{
  "id": "call_01",
  "type": "function",
  "function": {"name":"get_weather","arguments":"{\"city\":\"Taipei\"}"},
  "extra_content": {"google":{"thought_signature":"<SIGNATURE>"}}
}
```

历史 tool call 的 `arguments` 为空时按 `{}` 发送；不是 JSON object 时（被截断的 JSON、字符串、数组、`null` 等）包成 `{"_raw":"<原文>"}` 发送，不返回 400。Cline、Roo 等客户端回放一次被截断的调用后，会话照常继续。

`content` 可以是 string 或 Part 数组。Part 字段：

| `type` | 其他字段 |
| --- | --- |
| `text`、`input_text`、`output_text` | `text` |
| `image_url`、`input_image` | `image_url` string 或 `{"url":"..."}` |
| `video_url`、`input_video` | `video_url` string 或 `{"url":"..."}` |
| `input_file`、`file` | `file_id`，或 `filename` + `file_data` |
| `input_audio` | `input_audio.data`、`input_audio.format` |

OpenAI `image_url` / `input_image` 值为 Base64 data URL 时形成 inline data，值为 YouTube URL 时形成 external media，其他 http/https 地址同样作为 external media 交给上游读取（媒体类型按扩展名推断，未知时为 `image/*`，与 Anthropic URL source 一致；适配器自己不下载），其余字符串按已上传 file ID 解析。`video_url` / `input_video` 只接受 YouTube URL。Chat 的 file part 为 `{"type":"file","file":{"file_id"|"file_data","filename"}}`，Responses 的 `input_file` 把这些字段放在顶层并可用 `file_url`；`file_data` 接受 Base64 data URL 或不带前缀的 Base64（类型按文件名推断，默认 PDF）。

function tool 使用 `{"type":"function","function":{"name","description","parameters","strict"}}`。`strict:true` 时核对上游返回的参数。Google tool type 为 `web_search`、`web_search_preview`、`image_search`、`url_context`、`code_interpreter`、`google_maps`。

非流式响应：

```json
{
  "id": "chatcmpl_...",
  "object": "chat.completion",
  "created": 0,
  "model": "gemini-example",
  "provider_model": "gemini-provider-id",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "...",
      "reasoning_content": "...",
      "tool_calls": [],
      "annotations": []
    },
    "logprobs": null,
    "finish_reason": "stop",
    "provider_finish_reason": "provider_19"
  }],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 20,
    "total_tokens": 30,
    "completion_tokens_details": {"reasoning_tokens": 5}
  }
}
```

`provider_model`、`provider_finish_reason`、`reasoning_content`、`tool_calls`、`annotations` 和 `usage` 仅在有对应数据时出现；`logprobs` 与官方一致始终出现且为 `null`（请求里的 `logprobs` 被忽略）。annotation 形状为 `{"type":"url_citation","url_citation":{"url","title","start_index","end_index"}}`。

Chat SSE 顺序：

1. role chunk：`delta={"role":"assistant","content":""}`
2. 正文 `delta.content`、思考 `delta.reasoning_content`、工具 `delta.tool_calls`、媒体或代码渲染 `delta.content`
3. 引用 `delta.annotations`
4. finish chunk：`finish_reason`，可选 `provider_finish_reason`
5. `include_usage=true` 且上游有 usage 时发送 `choices:[]` 的 usage-only chunk
6. `data: [DONE]`

每个普通 chunk 为 `{id,object:"chat.completion.chunk",created,model,choices:[{index,delta,finish_reason}],usage?}`。响应头已发送后的失败为 `data: {"error":{"message","type","code"}}`。

### OpenAI Responses

`POST /v1/responses` 请求字段：

| 字段 | 类型与语义 |
| --- | --- |
| `model` | 必需模型 ID |
| `input` | string 或 input item 数组 |
| `instructions` | 顶层 system instruction |
| `stream` | boolean |
| `tools`、`tool_choice` | function、namespace 与 Google tools；namespace 内的 function 以 `命名空间.函数名` 的全名发给上游（不同命名空间可以有同名函数），调用结果还原为原函数名并以 `namespace` 字段标明所属命名空间，全名重复时返回 400；choice 为 auto/none/required/指定函数 |
| `temperature`、`top_p`、`max_output_tokens` | 生成参数 |
| `reasoning` | `{"effort":"..."}` |
| `text` | `{"format":{"type":"text|json_object|json_schema","schema":...}}` |
| `previous_response_id` | 当前进程内已保存的前一响应 ID |
| `parallel_tool_calls` | `false` 时一次回复最多一个函数调用 |
| `truncation` | 省略、`disabled` 或 `auto`（见上文） |
| `metadata` | string-to-string object |
| `store` | 省略/`true` 保存节点，`false` 只返回本次结果 |
| `video_config` | Omni 视频设置扩展字段，与 Chat 相同 |

input item 字段为 `type`、`role`、`content`、`call_id`、`name`、`namespace`、`arguments`、`output`、`encrypted_content`。支持 message、`function_call`、`function_call_output` 与 reasoning item；`function_call.arguments` 为空时按 `{}`，不是 JSON object 时与 Chat 相同包成 `{"_raw":"<原文>"}`，不返回 400；无状态客户端把上一轮输出放回 input 时，`web_search_call`、`code_interpreter_call`、`image_generation_call` 与 `item_reference` 按已在上游执行的调用跳过。message content Part：

| type | 字段 |
| --- | --- |
| `input_text`、`output_text` | `text` |
| `input_image` | `image_url` string 或 `{url}` |
| `input_file` | `file_id`，或 `filename` + `file_data` |
| `input_audio` | `input_audio:{data,format}` |
| `input_video` | `video_url` string 或 `{url}` |

Responses 的图片、视频与文件 Part 复用上述 data URL、file ID 与 YouTube 规则。

Responses tool 字段：

| tool type | 字段 |
| --- | --- |
| `function` | `name`、`description`、`parameters`、`strict` |
| `web_search`、`web_search_2025_08_26`、`web_search_preview`、`web_search_preview_2025_03_11` | `search_context_size`、`user_location`、`filters.allowed_domains` 以系统指令提示传给上游（软约束） |
| `image_search`、`url_context`、`google_maps` | `type` |
| `code_interpreter` | `container` 可省略、为 `"auto"`，或为 `{"type":"auto","file_ids":[]}`；非空 `file_ids` 返回 400 |

响应 shell 的字段始终存在：

```json
{
  "id": "resp_...",
  "object": "response",
  "created_at": 0,
  "completed_at": null,
  "status": "in_progress",
  "error": null,
  "incomplete_details": null,
  "instructions": null,
  "metadata": {},
  "model": "gemini-example",
  "output": [],
  "output_text": "",
  "parallel_tool_calls": true,
  "previous_response_id": null,
  "reasoning": null,
  "temperature": null,
  "text": {"format":{"type":"text"}},
  "tool_choice": "auto",
  "tools": [],
  "top_p": null,
  "truncation": "disabled",
  "max_output_tokens": null,
  "usage": null
}
```

完成对象可以增加 `provider_model` 与 `provider_finish_reason`。status 为 `completed`、`incomplete` 或 `failed`；长度终态使用 `incomplete_details.reason=max_output_tokens`，策略终态使用 `content_filter`。

output item 联合类型：

| type | 字段 |
| --- | --- |
| `reasoning` | `id`、`status`、`summary:[{type:"summary_text",text}]`、可选 `encrypted_content` |
| `message` | `id`、`status`、`role:"assistant"`、`content:[{type:"output_text",text,annotations}]` |
| `function_call` | `id`、`status`、`call_id`、`name`、`arguments` |
| `code_interpreter_call` | `id`、`status`、`code`、`container_id:"aistudio"`、`outputs:[{type:"logs",logs}]` |
| `image_generation_call` | `id`、`status`、Base64 `result` |
| `web_search_call` | `id`、`status`、`action` |

`web_search_call.action` 为 `{"type":"search","query":"...","sources":[{"type":"url","url":"..."}]}`，query 按首次出现去重，sources 按 URI 去重。`code_interpreter_call.status` 在没有结果时为 `incomplete`，成功结果为 `completed`，非 `OUTCOME_OK` 结果为 `failed`；stdout 写入 `outputs[].logs`，失败文本加 `stderr:` 前缀。流式 `output_item.added` 使用 `in_progress`，对应 `output_item.done` 使用最终状态。

Responses usage：

```json
{
  "input_tokens": 10,
  "output_tokens": 20,
  "total_tokens": 30,
  "input_tokens_details": {"cached_tokens":0},
  "output_tokens_details": {"reasoning_tokens":5}
}
```

每个 Responses SSE payload 都包含 `type` 和从 0 单调递增的 `sequence_number`：

| 事件 | 事件字段 |
| --- | --- |
| `response.created`、`response.in_progress` | `response` shell |
| `response.output_item.added`、`response.output_item.done` | `output_index`、`item` |
| `response.reasoning_summary_part.added`、`response.reasoning_summary_part.done` | `item_id`、`output_index`、`summary_index`、`part` |
| `response.reasoning_summary_text.delta` | `item_id`、`output_index`、`summary_index`、`delta` |
| `response.reasoning_summary_text.done` | 上述索引与 `text` |
| `response.content_part.added`、`response.content_part.done` | `item_id`、`output_index`、`content_index`、`part` |
| `response.output_text.delta` | `item_id`、`output_index`、`content_index`、`delta`、`logprobs:[]` |
| `response.output_text.done` | 上述索引与 `text`、`logprobs:[]` |
| `response.output_text.annotation.added` | `item_id`、`output_index`、`content_index`、`annotation_index`、`annotation` |
| `response.function_call_arguments.delta` | `item_id`、`output_index`、`delta` |
| `response.function_call_arguments.done` | `item_id`、`output_index`、`arguments`、`name` |
| `response.image_generation_call.in_progress`、`response.image_generation_call.completed` | `item_id`、`output_index` |
| `response.code_interpreter_call.in_progress`、`response.code_interpreter_call.interpreting`、`response.code_interpreter_call.completed` | `item_id`、`output_index` |
| `response.code_interpreter_call_code.delta` | `item_id`、`output_index`、`delta` |
| `response.code_interpreter_call_code.done` | `item_id`、`output_index`、`code` |
| `response.web_search_call.in_progress`、`response.web_search_call.searching`、`response.web_search_call.completed` | `item_id`、`output_index` |
| `response.completed`、`response.incomplete`、`response.failed` | 完整 `response` |

web search 发生时，search call item 排在 message 前；无 grounding query 时只输出 message。已产生的 delta 在 `response.failed` 前保持原顺序。

### Anthropic Messages

`POST /v1/messages` 请求字段：

| 字段 | 类型与语义 |
| --- | --- |
| `model` | 必需模型 ID |
| `messages` | 必需非空 `{role,content}` 数组；role 为 `user`、`assistant` 或 `system`，`system` 消息在原位置以 `<system-reminder>` 包裹的用户内容发送 |
| `system` | string 或 text block 数组 |
| `max_tokens` | 必需正整数 |
| `stop_sequences` | string array |
| `stream` | boolean |
| `temperature`、`top_p`、`top_k` | 生成参数 |
| `tools`、`tool_choice` | custom/server tools 与 auto/none/any/指定 tool |
| `thinking` | `{type:"enabled",budget_tokens:<INT>}` 或 `{type:"adaptive"}` |
| `output_config` | `{effort:"...",format:{type:"json_schema",schema}}`；`format` 为结构化输出（见上文生成参数映射） |
| `output_format` | 结构化输出的 beta 写法 `{type:"json_schema",schema}`；与 `output_config.format` 同时出现时以后者为准 |
| `video_config` | Omni 视频设置扩展字段，与 Chat 相同，见 [Omni 视频设置与模型名后缀](#omni-视频设置与模型名后缀) |

message content 可以是 string 或 block 数组：

| block type | 字段 |
| --- | --- |
| `text` | `text` |
| `thinking` | `thinking`、`signature` |
| `redacted_thinking` | `data` |
| `image`、`document` | `source:{type,media_type,data,url,file_id,content}` |
| `tool_use` | `id`、`name`、object `input` |
| `tool_result` | `tool_use_id`、`content`、`is_error` |
| `server_tool_use` | `id`、`name`、`input`；web_search 为 `input:{query}` |
| `web_search_tool_result` | `tool_use_id`、`content:[{type:"web_search_result",url,title,encrypted_content,page_age}]` 或失败对象 `{type:"web_search_tool_result_error",error_code}` |
| `search_result` | `source`、`title`、`content`（字符串或 text、image block 数组） |
| `web_fetch_tool_result`、`code_execution_tool_result`、`bash_code_execution_tool_result`、`text_editor_code_execution_tool_result`、`tool_search_tool_result` | `tool_use_id`、`content` |

`image` / `document` 的 Base64 source 使用 `type:"base64"`、`media_type`、`data`；URL source 使用 `type:"url"` 与非空 `url`，省略 media type 时 image 默认 `image/*`、document 默认 `application/pdf`；file source 使用 `type:"file"` 与非空 `file_id`，按本服务 Files 接口上传的文件 ID 引用（与 OpenAI 的 `file_id` 相同），缺少 `file_id` 时返回 `invalid_request_error`。document 另外接受 text source（`type:"text"`，`data` 为纯文本文档正文，作为文本发送，空文本省略）与 content source（`type:"content"`，`content` 为字符串或 text、image 块数组）。客户端提供的 `search_result` 块以 `Search result: <title>` 加来源的文本发送，`content` 同样只接受字符串或 text、image 块。这两处嵌套内容出现其他块（如再嵌套的 `search_result`、`document` 或 `tool_use`）时返回 `invalid_request_error`，不再按普通内容块逐层递归转换：逐层递归时每层都要重新解析剩余的 JSON，几千层嵌套的请求体就会耗尽内存。

其他服务端工具的历史调用与结果按文本保留上下文，不返回 400（会话从别的 Claude 渠道切过来时常带这些块）：非 web_search 的 `server_tool_use` 写成 `Server tool call <name>: <input>`；`web_fetch_tool_result` 写成 `Web fetch: <url>`，结果的 `content` 须为单个 `document` 块，按上文 document source 转换（文本、PDF 等），其他块返回 `invalid_request_error`，失败结果写成 `Web fetch failed: <error_code>`；code_execution 与 tool_search 的结果写成 `Server tool result <type>: <content>`。`container_upload` 等其他未知块仍返回 `invalid_request_error`。

`tool_result` 中的 Base64 图片、`document`（如 PDF）、MCP `resource` blob 与 `audio` 作为附件放在随后的用户消息中，原位置换成说明文字；沿 `{"content": ...}` 对象向内查找附件最多 8 层，更深的部分原样按 JSON 发送（OpenAI Chat、Responses 与 Interactions 的工具结果同样如此）；`tool_result.is_error=true` 把合法 JSON content 包装为 `{"error":<CONTENT>}`；普通标量或数组结果包装为 `{"result":<CONTENT>}`。

custom tool 为 `{name,description,input_schema}`，可选 `type:"custom"`；`strict:true` 时核对上游返回的参数，`cache_control`、`defer_loading` 等其他字段只影响 Anthropic 自身的缓存与加载，忽略。server tool 字段：

| type | 必需 name |
| --- | --- |
| `web_search_*`（任何日期版本，如 `web_search_20250305`） | `web_search` |
| `image_search` | `image_search` |
| `web_fetch_*`（任何日期版本，如 `web_fetch_20250910`） | `web_fetch` |
| `code_execution_*`（任何日期版本，如 `code_execution_20250522`、`code_execution_20250825`） | `code_execution` |
| `url_context` | `url_context` |
| `google_maps` | `google_maps` |
| `tool_search_tool_*`（如 regex、bm25 两种工具搜索），以及 type 含 `toolset` 的定义（如 MCP toolset） | 忽略整个定义，不检查 name 与其他字段 |

server tool type 不区分大小写，`web_search_`、`web_fetch_`、`code_execution_` 按前缀识别：Anthropic 定期发布带新日期的版本，新版本无需等本服务更新。工具搜索只决定 Anthropic 侧按需加载哪些工具，MCP toolset 由 Anthropic 连接远端 MCP 服务器，上游都没有对应能力：这两类定义直接忽略，`defer_loading` 的函数照常全部声明。`bash_*`、`text_editor_*`、`memory_*` 等其他 type 仍返回 `invalid_request_error`（`unsupported tool type`）。

`web_search_*` 接受 `max_uses`，调用次数由上游决定，忽略；`allowed_domains`（`site:` 查询）、`blocked_domains`（`-site:` 查询）与 `user_location` 以系统指令提示传给上游（软约束）；`allowed_domains` 与 `blocked_domains` 同时非空时返回 `invalid_request_error`。`web_fetch_*` 的 `max_uses`、`citations`、`max_content_tokens` 上游没有对应参数，忽略；`allowed_domains`、`blocked_domains` 是显式的抓取范围限制，URL Context 无法遵守，非空时返回 `invalid_request_error`，不悄悄放开限制。

server tool 的 `cache_control`、`defer_loading` 等只影响 Anthropic 自身缓存与加载的字段以及未知字段忽略，与 custom tool 一致；`description` 或 `input_schema` 返回 `invalid_request_error`。tool choice 接受省略、`{"type":"auto"}`、`{"type":"none"}`、`{"type":"any"}` 与 `{"type":"tool","name"}`，以及 `disable_parallel_tool_use`。

非流式响应：

```json
{
  "id": "msg_...",
  "type": "message",
  "role": "assistant",
  "model": "gemini-example",
  "content": [
    {"type":"thinking","thinking":"...","signature":"..."},
    {"type":"text","text":"..."},
    {"type":"tool_use","id":"call_01","name":"get_weather","input":{}}
  ],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "provider_model": "gemini-provider-id",
  "provider_finish_reason": "provider_19",
  "usage": {"input_tokens":10,"output_tokens":20}
}
```

content 输出 block 为 `text`、`thinking`、`redacted_thinking`、`tool_use`、`server_tool_use` 或 `web_search_tool_result`。stop reason 为 `end_turn`、`tool_use`、`stop_sequence`、`max_tokens`、`pause_turn` 或 `refusal`。`POST /v1/messages/count_tokens` 接受同一 message/system/tools 输入并返回 `{"input_tokens":<INT>}`；它提供独立计数估算，PDF 等媒体请求的最终用量以生成响应 `usage.input_tokens` 为准。

Anthropic 响应中的媒体编码为 text block 中的 Markdown：`MEDIA_OUTPUT=inline` 时为 data URL，`url` 时为本服务链接（见下文“生成媒体链接”），代码编码为 fenced text。Google Search 的每个去重查询生成一组 `server_tool_use` 和 `web_search_tool_result`，结果按 URL 去重，来源集合覆盖本次搜索的全部查询。`usage.server_tool_use.web_search_requests` 记录上游报告的去重查询数，`web_fetch_requests` 为 `0`。URL Context 等其余引用在末尾追加 `Sources:` Markdown 列表。

搜索来源的 `encrypted_content` 由本服务生成，保存上游返回的 URL、标题与可用摘要。客户端在下一轮消息中回传搜索块时，`server_tool_use` 与对应的 `web_search_tool_result` 合并为一个文本块 `Web search: <query>`，后面逐条列出结果的标题与 URL；摘要只从本服务用当前 `PROXY_API_KEY` 签发、且 URL 与结果一致的来源中恢复。以下情况都按文本保留，不返回 400：更换 API key 后或会话从别的 Claude 渠道切过来、`encrypted_content` 解不开（只保留标题与 URL）；调用与结果缺少对应的一半（只有结果时以 `Web search results:` 开头）；结果为 `error_code` 失败对象（写成 `Web search failed: <error_code>`）。`content` 既不是结果数组也不是带 `error_code` 的对象时仍返回 `invalid_request_error`。该字段用于本服务的多轮会话，和 Anthropic 服务的来源令牌分别使用。

Anthropic SSE：

| 事件 | payload |
| --- | --- |
| `message_start` | `{type,message:{id,type,role,model,content:[],stop_reason:null,stop_sequence:null,usage}}` |
| `content_block_start` | `{type,index,content_block}` |
| `content_block_delta` | `{type,index,delta}` |
| `content_block_stop` | `{type,index}` |
| `message_delta` | `{type,delta:{stop_reason,stop_sequence,provider_finish_reason?},usage}` |
| `message_stop` | `{type:"message_stop"}` |
| `ping` | `{type:"ping"}`；10 秒没有语义事件时作为心跳发送 |
| `error` | `{type:"error",error:{type,message}}` |

delta 联合类型为 `text_delta{text}`、`thinking_delta{thinking}`、`signature_delta{signature}`、`input_json_delta{partial_json}`。thinking signature 在对应 thinking block 关闭前发送；redacted thinking 使用一个 start/stop block；tool_use 先发送空 input，再通过 `input_json_delta` 发送完整参数 JSON。搜索块在来源汇总后以完整的 start/stop block 输出，查询计数随最终 `message_delta.usage` 返回。

### Gemini Interactions

`POST /v1beta/interactions` 与 `POST /v1/interactions` 接受同一创建请求，与其他公开端点一样必须携带 API key（`x-goog-api-key`、Bearer、`X-API-Key`、`api-key` 或 `key` 查询参数，任一来源相同即通过）；缺少或错误时两个入口都返回 Gemini 格式的 401 `UNAUTHENTICATED`。

```json
{
  "model": "<TTS 模型>",
  "input": [{"type":"user_input","content":[{
    "type":"text","text":"Have a wonderful day!",
    "annotations":[{"type":"speech_metadata","style":"cheerful and friendly"}]
  }]}],
  "response_format": {"type":"audio","mime_type":"audio/l16","sample_rate":24000},
  "generation_config": {"speech_config":[{"voice":"Kore"}]},
  "stream": true
}
```

| 字段 | 映射 |
| --- | --- |
| `model` | 必需；可带 `models/` 前缀，文本对话模型的 `-low`、`-medium`、`-high`、`-minimal`、`-128`、`-nothinking`、`-online` 后缀与 Omni 的视频后缀与其他入口一样解析，交互资源的 `model` 报告去掉后缀的模型名 |
| `input` | 字符串、单个内容块、内容块数组或步骤数组；内容类型为 `text`、`image`、`audio`、`video`、`document`。没有输入、续接历史和系统指令时选号前返回 400，只有系统指令时按用户消息生成 |
| 媒体内容 | `mime_type` 与 `data`（Base64）或 `uri` 二选一，复用 Gemini 文件与内联媒体解析 |
| 输入步骤 | `user_input`、`model_output`、`thought`、`function_call`、`function_result`；`function_call` 必须带 `id`，函数结果通过 `call_id` 匹配历史调用并补齐名称，对不上任何调用时返回 400；`thought.signature` 挂到随后的模型内容或函数调用上，连续多个签名各自保留 |
| 函数结果媒体 | `function_result.result` 中以 Base64 内嵌的 `image`、`audio`、`document`（`data` 与 `mime_type`）以及其他协议的图片、文件写法先取出作为真正的媒体发送，原位置换成文字说明 |
| `system_instruction` | 当前请求的系统指令 |
| `generation_config` | `temperature`、`top_p`、`top_k`、`max_output_tokens`、`seed`、`stop_sequences`、`thinking_level`、`thinking_summaries`（`auto` 或 `none`）、`speech_config`、`tool_choice`、`video_config`（Omni 视频任务与设置） |
| `response_format` | 单对象或数组；文本使用 `{type:"text",mime_type:"application/json",schema:{...}}` 请求结构化输出；图片使用 `{type:"image",aspect_ratio?,image_size?}`；音频使用 `{type:"audio",mime_type?:"audio/wav|audio/l16"}`；Omni 视频使用 `{type:"video",aspect_ratio?,duration?,resolution?,frame_rate?,fps?}`，见 [Omni 视频设置与模型名后缀](#omni-视频设置与模型名后缀) |
| 语音配置 | `speech_config:[{voice}]`；多说话人使用 `{speakers:[{speaker,voice}],mode?}`，`mode` 为 `verbatim` 或 `conversational` |
| 语音文本 | 文本块 `annotations` 中的 `{type:"speech_metadata",speaker?,style?}` 保留说话人与风格 |
| 函数与工具 | `tools:[{type:"function",name,description?,parameters?}]`；另接受 `google_search`、`url_context`、`code_execution`、`google_maps`；`tool_choice` 为 `auto` 或 `none` |
| 续接 | 默认保存；`previous_interaction_id` 重建前序内容，`store:false` 仅返回本次响应。与 Responses 共用当前服务实例内最多 256 个响应节点，只接受 `int_` 开头的交互 ID，`resp_` 开头的 Responses ID 返回 400；音频输出不保存进节点（只保留音频分片带的签名） |

音频输出为 24 kHz、16-bit 小端、单声道。非流式默认 `audio/wav`，流式默认 `audio/l16`；显式 WAV 流在音频汇总完成后发送一个有效 WAV 块。`sample_rate` 可省略或设为 `24000`，`delivery` 可省略或设为 `inline`。创建请求在当前连接内执行，`background` 可省略或设为 `false`。

非流式响应包含 `id`、`object:"interaction"`、`model`、`created`、`updated`、`status`、`steps` 与 `usage`。`steps` 的 `model_output.content` 保存文本或媒体，音频位于 `{type:"audio",data,mime_type,sample_rate,channels}`。函数调用作为 `function_call` 步骤返回，`id` 一定非空：上游没有给调用 ID 时为 `call_local_` 开头的本地 ID，客户端原样回传即可对应结果，发回上游时去掉；调用携带的思考签名在调用之前作为 `thought` 步骤返回。有函数调用时状态为 `requires_action`；输出上限与内容拦截为 `incomplete`；停止序列、`malformed_function_call` 等其他结束原因与其他协议一样为 `completed`。

SSE 使用相同的事件名与 JSON `event_type`：`interaction.created` → `step.start` → `step.delta` → `step.stop` → `interaction.completed`。步骤以 `index` 对应，思考的增量为 `thought_summary` 与 `thought_signature`，已带签名的思考再遇到签名时另起一步；`model_output` 进行中到达的只带签名的思考（流式与非流式）暂存到这段正文结束后再作为 thought 步骤输出，与上一个相同的签名只输出一次，正文保持一个步骤；随后是不带签名的函数调用或不带签名的思考摘要时排在它们之后，回传时不会成为不带签名的调用的签名；音频增量为 `{type:"audio",data,mime_type,sample_rate,channels}`。与其他流式入口一样，写出响应头前最多等待首个事件 10 秒，首个事件就是错误时返回 Gemini HTTP 错误对象；开始推流后，10 秒无语义事件时发送 `: ping`，上游错误或缺失终态发送 `error` 事件 `{"event_type":"error","error":{"code":"<RPC 状态名>","message":"..."}}` 并结束，消息与 HTTP 错误一样按官方措辞脱敏；客户端断开会取消上游生成。

### Gemini GenerateContent

`POST /v1beta/models/{model}:generateContent`、`:streamGenerateContent` 与 `:countTokens` 接受：

```json
{
  "contents": [{"role":"user","parts":[{"text":"Hello"}]}],
  "systemInstruction": {"role":"user","parts":[{"text":"Be concise"}]},
  "generationConfig": {},
  "tools": [],
  "toolConfig": {},
  "safetySettings": [{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_ONLY_HIGH"}]
}
```

`safetySettings` 也可写成 `safety_settings`，不是数组或条目字段类型不对时返回 400；类别与阈值的取值见上文 GenerateContent 的 safety settings。

Content 字段为 `role` 与 `parts`。Part oneof：

| Part | 字段 |
| --- | --- |
| text/thought | `text`、可选 `thought`、`thoughtSignature` |
| inline data | `inlineData:{mimeType,data}` |
| file data | `fileData:{mimeType?,fileUri,displayName?}`；`fileUri` 必需，`mimeType` 可省略 |
| function call | `functionCall:{id,name,args}` |
| function response | `functionResponse:{id,name,response}` |
| executable code | `executableCode:{language,code}` |
| code result | `codeExecutionResult:{outcome,output,error}` |

`fileData.fileUri` 指向本服务 `/media/<文件名>` 且文件仍保存着时，用户消息里的恢复为内联媒体，模型消息里的只恢复图片（见下文“生成媒体链接”），不按已上传文件查找。`fileData` 省略 `mimeType` 时（官方 Files API 的 `fileUri` 自带类型）按 `displayName` 的扩展名推断，推断不出再按 `fileUri` 路径（URL 取 path，不含查询参数）的扩展名推断，仍推断不出时留空；本服务上传的文件按文件 ID 发送，不依赖这里的类型，YouTube 等外部媒体 URL 按外部媒体处理。缺少 `fileUri` 时返回 400。

`generationConfig` 全字段：

| 类别 | 字段 |
| --- | --- |
| sampling | `temperature`、`topP`、`topK`、`seed`；`frequencyPenalty`、`presencePenalty` 接受任意值并忽略 |
| media | `mediaResolution`（或 `media_resolution`）：`MEDIA_RESOLUTION_LOW`、`MEDIA_RESOLUTION_MEDIUM`、`MEDIA_RESOLUTION_HIGH`，不区分大小写；其他值在选号前返回 400 |
| output limits | `candidateCount`、`maxOutputTokens`、`stopSequences` |
| log probabilities | `responseLogprobs`、`logprobs` 接受任意值并忽略 |
| structured output | `responseMimeType`、`responseSchema`、`responseJsonSchema`；`responseFormat` 的 `text:{mimeType,schema}` 与之等价（`APPLICATION_JSON`、`TEXT_PLAIN` 换成 MIME） |
| modalities | `responseModalities` |
| image | `imageConfig:{aspectRatio,imageSize}`；`responseFormat` 的 `image:{aspectRatio,imageSize}` 与之等价（`ASPECT_RATIO_*`、`IMAGE_SIZE_*` 枚举名换成取值） |
| video | Omni：`responseFormat` 的 `video:{aspectRatio,duration,resolution,frameRate,fps}` 与扩展字段 `videoConfig:{task,aspectRatio,durationSeconds,resolution,frameRate,fps}`，见 [Omni 视频设置与模型名后缀](#omni-视频设置与模型名后缀)；`responseFormat` 可以是单个对象或数组，`null` 为未设置 |
| thinking | `thinkingConfig:{thinkingBudget,thinkingLevel,includeThoughts}`；`thinkingBudget:-1`（动态思考）按未设置处理，使用模型默认强度；`includeThoughts:false` 照常思考但不返回思考正文（保留签名），未设置时返回思考 |
| transcription | `transcriptionConfig:{languageCodes,customVocabulary,wordTimestamps,speakerLabels,smartTranscription}` |
| speech | `speechConfig` |

`responseModalities` 接受 `TEXT`、`IMAGE` 与 `AUDIO`，`AUDIO` 与其他模态互斥；枚举零值 `MODALITY_UNSPECIFIED`（SDK 序列化未赋值的枚举时会带上）忽略，只有它时与省略 `responseModalities` 相同，其他值返回 400。图像模型省略模态或仅请求 `IMAGE` 时发送 `[IMAGE,TEXT]`；`imageConfig` 保留显式宽高比与尺寸，支持输出分辨率的模型省略图片配置时使用 `1K`。

`speechConfig.voiceConfig` 与 `multiSpeakerVoiceConfig` 互斥；单声音必须提供 `prebuiltVoiceConfig.voiceName`，每个多说话人条目必须提供非空 `speaker` 与 `voiceConfig.prebuiltVoiceConfig.voiceName`；`multiSpeakerVoiceConfig.mode` 可选 `VERBATIM` 或 `CONVERSATIONAL`。文本 part 的 `speechMetadata`（或 `speech_metadata`）`{speaker,style}` 写入 Part field 41。能力码 85 的模型把未带 speaker 的文本按行拆分，以配置中说话人名加冒号开头的行开始新分段，续行并入上一分段，首个说话人行之前的文本不发送；没有匹配行的文本原样发送。旧 TTS 模型把 `speechMetadata` 写回 `speaker: 台词` 前缀与 `style\n\n` 说明段落。`transcriptionConfig.smartTranscription=true` 与显式 true 的 `wordTimestamps` 或 `speakerLabels` 互斥；language code `detect` 归一为空自动检测。

单声音 speech config：

```json
{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"Kore"}}}
```

多说话人：

```json
{
  "multiSpeakerVoiceConfig": {
    "speakerVoiceConfigs": [{
      "speaker": "Speaker A",
      "voiceConfig": {"prebuiltVoiceConfig":{"voiceName":"Kore"}}
    }]
  }
}
```

tool group 字段：

| 工具 | 字段 |
| --- | --- |
| functions | `functionDeclarations:[{name,description,parameters,parametersJsonSchema}]` |
| search | `googleSearch` 或 `googleSearchRetrieval` |
| URL | `urlContext` |
| code | `codeExecution` |
| maps | `googleMaps` |
| image search | `imageSearch` |

`googleSearch.searchTypes` 可以包含 `webSearch` 与 `imageSearch` 空对象；未提供或两项均未启用时默认 web search。`timeRangeFilter.startTime/endTime` 使用 RFC 3339 Nano。`googleSearchRetrieval` 接受任意对象并映射为 Google Search，`dynamicRetrievalConfig`（旧版动态检索阈值）没有对应参数，忽略。tool choice 位于 `toolConfig.functionCallingConfig:{mode,allowedFunctionNames}`，接受 `AUTO`、`NONE`、`ANY` 与 `VALIDATED`，枚举零值 `MODE_UNSPECIFIED` 与省略相同（按 `AUTO`）；`allowedFunctionNames` 限定可调用的函数（见上文工具选择）。

`:countTokens` 返回：

```json
{"totalTokens": 123}
```

非流式生成响应：

```json
{
  "candidates": [{
    "content": {"role":"model","parts":[]},
    "index": 0,
    "finishReason": "STOP",
    "finishMessage": "...",
    "groundingMetadata": {},
    "citationMetadata": {}
  }],
  "modelVersion": "gemini-requested-model",
  "responseId": "request-id",
  "usageMetadata": {
    "promptTokenCount": 10,
    "candidatesTokenCount": 20,
    "thoughtsTokenCount": 5,
    "toolUsePromptTokenCount": 0,
    "totalTokenCount": 35
  }
}
```

输出 Part 使用与输入相同的 `text`、`thought`、`thoughtSignature`、`inlineData`、`fileData`、`functionCall`、`executableCode`、`codeExecutionResult`。转录文本可以携带：

```json
{
  "text": "...",
  "transcriptionMetadata": {
    "speaker": "Speaker 1",
    "timestamps": [{
      "start":{"seconds":0,"nanos":0},
      "end":{"seconds":1,"nanos":250000000}
    }]
  }
}
```

`groundingMetadata` 字段为 `searchEntryPoint`、`groundingChunks`、`groundingSupports`、`retrievalMetadata`、`webSearchQueries`、`googleMapsWidgetContextToken`。`searchEntryPoint` 包含 `renderedContent`、`sdkBlob`；`groundingChunks` 元素的 oneof 为 `web:{uri,title}`、`retrievedContext:{uri,title,text}` 或 `maps:{uri,title,text,placeId}`；`groundingSupports` 元素包含 `segment:{partIndex,startIndex,endIndex,text}`、`groundingChunkIndices` 和可选 `confidenceScores`；`retrievalMetadata` 包含 `googleSearchDynamicRetrievalScore`。`citationMetadata.citationSources` 的元素包含 `uri`、`title`、`startIndex`、`endIndex`。

非流式响应把相邻的同类正文或思考片段合并为一个 Part，不论片段是否带签名；同一段里出现多个签名时保留最后一个（gemini-4 等模型给许多正文分片都带签名，按签名拆开时 new-api、SillyTavern 拼接多个 Part 会在句子中间断行）。工具调用、媒体与带转录元数据的 Part 保持独立；引用、搜索来源与用量不产生 Part，也不打断合并。正文或思考中途单独到达的签名挂到正在合并的 Part 上；输出开头的单个签名在合并完成后挂到随后的第一个 Part 上，该 Part 合并后已带签名时前置签名单独成 Part，不因前置签名拆开连续正文。其余无处可挂的独立签名（工具调用、媒体之后，以及流式响应中的独立签名）以 `{"text":"","thought":true,"thoughtSignature":"..."}` 承载，作为输入回传时还原为纯签名 Part。

`modelVersion` 为请求的模型名去掉 `-low`、`-128`、`-nothinking` 等后缀（与 OpenAI、Anthropic、Responses、Interactions 的 `model` 一致），流式的每一帧相同，不换成上游报告的实际服务模型版本；实际服务的模型与请求的不是同一系列时，管理日志另记 `served_model`（同一系列的版本号差异不记录）。

`:streamGenerateContent` 与官方一致：带 `alt=sse` 时使用 SSE，不带时返回逐块写出的 JSON 数组（`application/json`）。两种格式的心跳都是一个空行：JSON 数组元素之间的空白不影响解析，SSE 中的空行不构成事件，官方 SDK 与 new-api 都直接跳过。SSE 不使用 `: ping` 注释帧：google-genai Python SDK 把不以 `data: ` 开头的行当作分行的 JSON 错误体拼接解析，注释行会让整个流报 `UnknownApiResponseError`。每个语义事件发送一个部分 `GenerateContentResponse`，包含 `responseId`、`modelVersion` 与一个 candidate Part、grounding 或 citation；最后一帧包含 candidate `finishReason`、可选 `finishMessage`（与非流式相同，上游给出的结束说明优先，见第 5 节“Finish 与错误”）和 `usageMetadata`。响应头后的错误以 `{"error":{"code","message","status"}}` 作为一帧（SSE 的 `data:` 或数组元素）发出。

### Files、Transcribe 与媒体

`POST /v1/files` 接受 multipart `file` 与 `purpose`，两者可以按任意顺序到达。文件上限 512 MiB，请求额外允许 1 MiB multipart overhead；普通 scalar part 上限 64 KiB。filename、非空 file 和非空 purpose 必需。Content-Type 为空或 `application/octet-stream` 时从文件前缀检测 MIME。

File object：

```json
{
  "id": "file_...",
  "object": "file",
  "bytes": 1234,
  "created_at": 0,
  "filename": "document.pdf",
  "purpose": "assistants",
  "status": "processed"
}
```

`POST /v1/files` 与 `GET /v1/files/{id}` 返回该对象。`GET /v1/files/{id}/content` 返回原始 body，并设置 `Content-Type`、attachment `Content-Disposition` 和已知时的 `Content-Length`。`DELETE /v1/files/{id}` 返回：

```json
{"id":"file_...","object":"file","deleted":true}
```

未知文件返回 404 `file_not_found`，超过大小限制返回 413 `file_too_large`。Drive file 绑定创建账户；跨账户生成会临时复制文件并在本次尝试结束后清理副本。

`POST /v1/audio/transcriptions` multipart 字段：

| 字段 | 取值与处理 |
| --- | --- |
| `file` | 必需非空；`audio/*`、`video/mp4`、`video/webm`；最大 512 MiB |
| `model` | 默认 `gemini-3.5-transcribe`；接受可选 `models/` 前缀 |
| `response_format` | `json`、`text`、`verbose_json`、`diarized_json`；默认 `json` |
| `language` | 语言码；`detect` 与 `auto` 映射为空自动检测 |
| `temperature` | `0..2` |
| `custom_vocabulary` | 可重复文本字段或 JSON string array |
| `word_timestamps`、`speaker_labels`、`smart_transcription` | `true` 或 `false` |
| `prompt` | 当前 wire 无对应字段，非空值返回 400 |

`smart_transcription=true` 与显式 true 的 word timestamps 或 speaker labels 互斥；custom vocabulary 与 word timestamps 互斥。每次账户尝试创建临时 Drive file，生成结束、失败或取消后在同账户清理。

`text` 格式返回 `text/plain; charset=utf-8`。`json` 返回 `{"text":"...","usage":...}`。详细格式：

```json
{
  "task": "transcribe",
  "language": "en",
  "duration": 1.25,
  "text": "Hello",
  "segments": [{"id":0,"start":0,"end":1.25,"text":"Hello","speaker":"Speaker 1"}],
  "words": [{"word":"Hello","start":0,"end":1.25,"speaker":"Speaker 1"}],
  "usage": {"input_tokens":10,"output_tokens":2,"total_tokens":12}
}
```

`verbose_json` 与 `diarized_json` 使用同一详细对象形状。`language` 是规范化后的请求语言；`detect` / `auto` 时为空并省略。`segments` 来自 response Part field 23；单词数与 timestamp span 数一致时生成 `words`。

`POST /v1/images/generations`：

| 字段 | 取值与处理 |
| --- | --- |
| `model`、`prompt` | 必需 |
| `n` | 默认与唯一值 `1` |
| `size` | `auto`、`1024x1024`、`1536x1024`、`1024x1536` |
| `quality` | `auto`；`low/standard=1K`、`medium/hd=2K`、`high=4K` |
| `response_format` | `b64_json` 返回 Base64；其他值返回 data URL |

响应为 `{"created":<UNIX>,"data":[{"b64_json":"...","revised_prompt":"..."}]}` 或 `{"created":<UNIX>,"data":[{"url":"data:<MIME>;base64,...","revised_prompt":"..."}]}`。`revised_prompt` 只在上游同时返回文本时出现。上游未返回最终图片时返回 HTTP 502 `upstream_error`，结束原因不是正常结束时写入错误消息，例如 `image_recitation`。

`POST /v1/audio/speech`：

| 字段 | 取值与处理 |
| --- | --- |
| `model`、`input` | 必需 |
| `voice` | 默认 `Zephyr` |
| `response_format` | 默认 `wav`；支持 `wav`、`pcm`，上游已经返回 `audio/mpeg` 时支持 `mp3` |
| `speed` | 省略/`0` 或 `1` |
| `instructions` | 作为文本 part 的 `speechMetadata.style`；旧 TTS 模型以 `instructions + "\n\n" + input` 形成提示 |

`pcm` 返回上游 PCM body 与 MIME；`wav` 要求上游 `audio/l16` 和有效 rate，再封装 16-bit WAV；响应设置 `Content-Type` 与 `Content-Length`。

旧 TTS 模型的语音请求按官网 wire 在首个文本前写入 `## Transcript:\n`，AUDIO-only generation config 不写默认 `maxOutputTokens`；`responseModalities` 与 `speechConfig` 分别写入官网确认的槽位。

### Video

OpenAI `POST /v1/videos` 接受 JSON 或 multipart：

| 字段 | 取值与处理 |
| --- | --- |
| `model`、`prompt` | 必需 |
| `seconds` | 整数字符串；默认 4 |
| `size` | `1280x720`、`720x1280`、`1792x1024`、`1920x1080`、`1024x1792`、`1080x1920` |
| `input_reference` | JSON 中为 file ID/data URL；multipart 中为文件 |

OpenAI video object：

```json
{
  "id": "operation-id",
  "object": "video",
  "model": "veo-example",
  "status": "queued",
  "progress": 0,
  "created_at": 0,
  "size": "1280x720",
  "seconds": "4"
}
```

`GET /v1/videos/{id}` 返回当前对象。完成时 status 为 `completed`、progress 为 `100`；上游 done 且无 file 时 status 为 `failed`。`GET /v1/videos/{id}/content` 接受省略或 `variant=video`，未完成时返回 409 `video_not_ready`；成功下载设置媒体 `Content-Type`、`attachment; filename="video.mp4"` 和已知的 `Content-Length`。

视频创建成功后按 operation ID 写入以下资源绑定：

```json
{
  "kind": "video-operation",
  "created_at": "2026-01-01T00:00:00Z",
  "video": {
    "model": "veo-example",
    "seconds": "4",
    "size": "1280x720"
  }
}
```

`model`、`seconds`、`size` 与 UTC `created_at` 随创建账户持久保存，后续轮询从资源绑定恢复；生成服务或进程重新启动后，OpenAI POST 与 GET 仍返回相同字段。OpenAI 请求显式提供 `size` 时原样保存该公开值，省略时按默认 16:9、720p 保存 `1280x720`。Gemini 请求保存归一化输出尺寸：720p 为 `1280x720` 或 `720x1280`，1080p 为 `1920x1080` 或 `1080x1920`，4k 为 `3840x2160` 或 `2160x3840`。公开对象的 `created_at` 是绑定创建时间的 Unix 秒。

Gemini `:predictLongRunning` 请求：

```json
{
  "instances": [{
    "prompt": "...",
    "image": {
      "inlineData": {"mimeType":"image/jpeg","data":"..."}
    }
  }],
  "parameters": {
    "numberOfVideos": 1,
    "sampleCount": 1,
    "aspectRatio": "16:9",
    "durationSeconds": 4,
    "resolution": "720p"
  }
}
```

`instances` 必须只有一个非空 prompt；image 必须在 `inlineData` 与 `fileData:{mimeType,fileUri}` 中选择一个。`numberOfVideos` 为 0 时读取 `sampleCount`，两者均为 0 时默认 1，最终只接受一个结果。`durationSeconds` 接受 JSON integer 或十进制字符串。duration、aspect ratio 与 resolution 省略时分别为 `4`、`16:9`、`720p`，并与实时模型的 `video_durations_seconds`、`video_aspect_ratios`、`video_output_resolutions` 校验。创建返回 `{"name":"operations/<ID>"}`。Gemini operation 使用同一份持久元数据恢复创建账户与轮询上下文；Gemini 响应只包含 `name`、`done` 与完成后的 `response.generateVideoResponse.generatedSamples`。

`GET /v1beta/operations/{id}`：

```json
{
  "name": "operations/<ID>",
  "done": true,
  "response": {
    "generateVideoResponse": {
      "generatedSamples": [{
        "video": {"uri":"http://<HOST>/v1/videos/<ID>/content","mimeType":"video/mp4"}
      }]
    }
  }
}
```

`response` 只在 done 时出现；done 且无产物时 `generatedSamples` 为空。operation 与结果 file 绑定创建账户。经 `/ultra/v1beta/operations/<ID>` 查询时下载地址带 `/ultra` 前缀（`http://<HOST>/ultra/v1/videos/<ID>/content`），按该地址下载仍使用 Ultra 号池。

### Live 与 Robotics 公开帧

连接升级后 10 秒内发送 setup：

```json
{
  "type": "setup",
  "model": "gemini-live-model",
  "input_modalities": ["text", "audio", "image"],
  "output_modalities": ["audio"],
  "tools": [{"name":"get_weather","description":"...","parameters":{"type":"object"}}],
  "session_token": ""
}
```

Live input modalities 是 text/audio/image 的非空子集，output 为 `["audio"]` 或 `["text"]`。实时翻译模型要求 output `["audio"]` 与 `"translation":{"target_language_code":"es","echo_target_language":false}`，输入音频的原文经 `input_transcription`、译文经 `output_transcription` 与 `media` 返回；实时转录模型要求 output `["text"]`，可带 `"transcription":{"language_codes":["en"]}`，转写经 `interim_input_transcription`（当前累计文本）与 `input_transcription`（最终文本）返回。两类模型不接受 tools。Robotics input/output 必须分别为 `["text"]` 与 `["text"]`，不接受 translation 与 transcription。数组不接受空字符串或重复项。

setup 之后的客户端对象统一字段为 `type`、可选 `text`、`mime_type`、Base64 `data`、`tool_responses`。各帧字段：

| type | 字段 |
| --- | --- |
| `text` | `text`，且 setup 声明 text |
| `audio` | `mime_type:"audio/pcm"`、`data`；MIME 省略时使用该默认值 |
| `image` | `mime_type:"image/jpeg"`、`data`；MIME 省略时使用该默认值 |
| `media_end` | 无附加字段 |
| `tool_response` | `tool_responses:[{id,name,content}]`，setup 必须声明 tools |
| `close` | 无附加字段 |

服务端对象字段全集：

```json
{
  "type": "text",
  "model": "gemini-live-model",
  "text": "...",
  "mime_type": "audio/l16;rate=24000",
  "data": "<BASE64>",
  "transcription": {"text":"...","finished":true,"duration_ms":1000,"language_code":"en"},
  "tool_call": {"id":"call-1","name":"get_weather","arguments":{}},
  "tool_call_ids": ["call-1"],
  "session_token": "...",
  "resumable": true,
  "raw": {},
  "error": "...",
  "code": "...",
  "retryable": true
}
```

按 type 使用对应字段：`session_opened{model}`、`setup_complete`、`text{text}`、`media{mime_type,data}`、`input_transcription/output_transcription/interim_input_transcription{transcription}`、`tool_call{tool_call}`、`tool_call_cancellation{tool_call_ids}`、`interrupted`、`generation_complete`、`turn_complete`、`session_resumption{session_token,resumable}`、`usage{raw}`、`go_away{raw}`、`provider{raw}`、`closed`、`error{error,code,retryable,raw?}`。

首帧或后续客户端字段无效时发送 `{type:"error",code:"invalid_request",error:"..."}`。上游错误保留原始 `error` 内容。客户端单帧上限 8 MiB，超限发送 WebSocket close code 1009。每次写操作上限 10 秒；会话结束等待读取和发送 goroutine（Go 协程）的上限各 5 秒。

流式端点统一使用 `text/event-stream`，每个 SSE frame 以空行结束：

| 协议 | 首事件 | 内容序列 | usage | 终止事件 |
| --- | --- | --- | --- | --- |
| OpenAI Chat | assistant role chunk | chat completion delta | `include_usage=true` 时位于 finish chunk 之后 | `data: [DONE]` |
| OpenAI Responses | `response.created`、`response.in_progress` | output item / content part / delta / done | 完成 response 的 `usage` | `response.completed` 或 `response.incomplete` |
| Anthropic | `message_start` | `content_block_start`、delta、`content_block_stop` | `message_delta.usage` | `message_stop` |
| Gemini | candidate Part | `GenerateContentResponse` 增量 | 最后一帧 `usageMetadata` | 最后一帧 finish reason |

上游语义事件间隔达到 10 秒时，各流式协议发送心跳并立即 flush：

| 协议 | 心跳 |
| --- | --- |
| OpenAI Chat、OpenAI Responses、Gemini Interactions | SSE 注释帧 `: ping` |
| Anthropic | 官方 ping 事件 `event: ping` 加 `data: {"type":"ping"}`；Anthropic SDK 识别并跳过，new-api 按官方事件转发 |
| Gemini `:streamGenerateContent`（SSE 与 JSON 数组） | 一个空行 |

OpenAI Chat 先发送 assistant role chunk，Responses 先发送 `response.created` 与 `response.in_progress`，Anthropic 先发送 `message_start`；这些起始事件在账户调度期间即可到达客户端。Gemini 的首个帧来自上游语义事件，之前只有心跳空行。

Responses 的 `web_search_call` 由实际 grounding query 触发。搜索发生时，SSE 先输出 index 0 的 search call，再输出 index 1 的 message；模型未调用搜索时仅输出 message，并按正文事件逐块发送。上游在正文后失败时，已收到的正文 delta 位于 `response.failed` 之前。

公开适配规则：

| 规范事件 | OpenAI Chat | Responses | Anthropic | Gemini |
| --- | --- | --- | --- | --- |
| text | message/content delta | output_text | text block | candidate text Part |
| reasoning | `reasoning_content` | reasoning summary | thinking block | thought Part |
| function | `tool_calls` | function_call item | tool_use block | functionCall Part |
| function result | tool message | function_call_output | tool_result | functionResponse Part |
| code execution | 可读 Markdown | code_interpreter item | text block | executableCode/result Part |
| grounding/citation | annotations | output annotations | text sources | groundingMetadata |
| media | data URL、`/media/` 链接或媒体端点 | output content | content block | inlineData Part；url 模式为链接文本 Part 加 fileData Part |
| usage | prompt/completion/total | input/output/total | input/output | prompt/candidates/thoughts/total |

Responses 媒体使用 image generation item；Anthropic 媒体使用 text block 中的 Markdown（data URL 或 `/media/` 链接）。Anthropic 来源在 text block 末尾使用 `Sources:` Markdown 列表。

OpenAI Chat 使用 Markdown data URL 承载生成图片；客户端把 assistant `message.content` 回传下一轮时，适配器将其中的图片恢复为 inline data Part，保留图片多轮上下文。图片 Base64 支持标准与 URL-safe 字母表、可选填充及 CR/LF 换行；图片前后的文本保持原顺序。

**生成媒体链接（MEDIA_OUTPUT=url）**

`MEDIA_OUTPUT` 只作用于 Chat Completions、Messages 与 Gemini 原生接口（`generateContent`、`streamGenerateContent`，含 `/ultra` 前缀）；Responses、Interactions、Live、`/v1/images/generations`、`/v1/audio/speech` 与视频接口的输出不变。默认 `inline` 时输出与原先逐字节相同。`url` 时带内容的媒体按内容 SHA-256 命名保存（PCM `audio/L16` 包装成 `audio/wav`；流式时连续、不带签名的 PCM 分片合并到下一个其他事件之前输出一次），正文改为 `<base>/media/<64 位十六进制>.<扩展名>`：`base` 为 `MEDIA_BASE_URL`，为空时取请求的 `X-Forwarded-Proto`（只认 http、https）与 `X-Forwarded-Host` 或 `Host`，主机含不能写进链接的字符时为空（链接为相对地址）。链接文字图片为 `image`、视频 `video`、音频 `audio`，其他 `file`。保存失败时该媒体按 inline 输出，请求照常成功。只有地址没有内容的媒体（上游返回的 URL）与原先一样输出。

OpenAI Chat（非流式 `message.content`，流式 `delta.content`，与 inline 一样在前面正文没有以换行结尾时先补一个换行）：

```json
{"choices":[{"message":{"role":"assistant","reasoning_content":"…","content":"视频已生成：\n[video](https://ai.example.com/media/3d29…a370d0.mp4)"}}]}
```

```text
data: {"choices":[{"index":0,"delta":{"content":"\n[video](https://ai.example.com/media/3d29…a370d0.mp4)"},"finish_reason":null}],…}
```

Anthropic Messages：每个媒体另起一个 text block（与 inline 相同），流式为该 block 的一个 `text_delta`：

```json
{"content":[{"type":"text","text":"视频已生成："},{"type":"text","text":"[video](https://ai.example.com/media/3d29…a370d0.mp4)"}]}
```

Gemini：每个媒体输出两个 Part，先是链接文本（前面的正文没有以换行结尾时先补一个换行，末尾总带一个换行），再是 `fileData`；流式时两者在同一块里，文本在前。媒体事件的签名放在 `fileData` 上（inline 时在 `inlineData` 上）。new-api 等把 Gemini 回复转成 OpenAI 格式的中转只保留文本、思考与图片 `inlineData`，文本链接能带给用户，`fileData` 被当作空文本；原生 SDK 从 `fileData.fileUri` 取地址：

```json
{"candidates":[{"content":{"role":"model","parts":[
  {"text":"视频已生成："},
  {"text":"\n[video](https://ai.example.com/media/3d29…a370d0.mp4)\n"},
  {"fileData":{"mimeType":"video/mp4","fileUri":"https://ai.example.com/media/3d29…a370d0.mp4"}}
]},"index":0}]}
```

`GET`/`HEAD /media/<文件名>` 不校验 API key：文件名必须为 64 位小写十六进制加 `png`、`jpg`、`webp`、`gif`、`mp4`、`webm`、`mov`、`mp3`、`wav`、`ogg`、`m4a`、`aac`、`flac` 或 `bin`，只提供媒体目录第一层的普通文件；`Content-Type` 按扩展名（`bin` 为 `application/octet-stream` 并带 `Content-Disposition: attachment`），支持 Range 与 `If-None-Match`，`ETag` 为内容哈希，`Cache-Control: public, max-age=31536000, immutable`，`X-Content-Type-Options: nosniff`。名字不合法、文件不存在或已清理、不是 url 模式时返回 404，其他方法返回 405。

回传恢复（不区分是否为 url 模式）：Chat、Messages 与 Gemini 的 assistant 文本里指向本服务的 Markdown 图片（`![…](…/media/<哈希>.png|jpg|webp|gif)`，绝对或相对地址）恢复为 inline data Part，前后文本保持原顺序，视频与音频链接保持文本；用户消息的 `image_url`、`fileData` 等外部地址指向已保存的媒体时恢复为 inline data Part（含视频）；模型消息的 `fileData` 只恢复图片，视频与音频在同一条消息里已有文本链接时去掉（签名单独保留），没有时改为文本链接；文件已清理时图片链接保持原样，`fileData` 改为文本链接。同一文件一次请求只恢复一处：从最新的消息往前选，同一条消息里 `fileData` 与外部地址先于文本链接，其余引用不恢复（文本链接保持文本，`fileData` 与外部地址按上面的规则去掉或改为文本链接）；一次请求恢复的内容合计不超过 64MB，超出时较早的引用不恢复（文本链接保持文本，`fileData` 改为文本链接，外部地址保持原样）。

用户文本中的 `youtu.be/<ID>`、`youtube.com/watch?v=<ID>`、`/shorts/<ID>`、`/live/<ID>` 和 `/embed/<ID>` 会转换为 `video/*` 外部媒体 part，并从用户 text part 中移除；重复 URL 合并为一个附件。OpenAI `video_url`/`input_video`、Anthropic URL source 与 Gemini `fileData.fileUri` 使用相同的外部媒体编码。

OpenAI Responses 的 `previous_response_id` 与 Gemini Interactions 的 `previous_interaction_id` 在进程内共用最多 256 个响应节点并重建完整 contents；重启后客户端重新提交完整上下文。Drive 与 Veo 资源绑定持久化到磁盘。

`store` 省略或为 `true` 时建立响应节点；`store=false` 返回当前响应并保持既有续接链。

模型、参数、账户与上游错误按下方状态表投影。客户端取消会关闭上游 reader并释放账户租约。

错误对象与状态语义：

| 情况 | HTTP | OpenAI | Anthropic | Gemini |
| --- | ---: | --- | --- | --- |
| 参数、Schema、tool choice 无效 | 400 | `invalid_request` | `invalid_request_error` | `INVALID_ARGUMENT` |
| 输入超过模型本身的 token 上限（带计数的谷歌原文） | 400 | `context_length_exceeded` | `invalid_request_error`（消息以 `prompt is too long:` 开头） | `INVALID_ARGUMENT` |
| 输入超过账号的输入上限（`… allowed 131072`；上游以 400 或 429 返回都算，不冷却账号）：文案与安全拦截相同 | 400 | `content_policy_violation` | `invalid_request_error` | `INVALID_ARGUMENT` |
| 没有符合条件的账户 | 400 | `account_required` | `invalid_request_error` | `INVALID_ARGUMENT` |
| 支持请求的账户均不可调度 | 503 | `account_unavailable` | `api_error` | `UNAVAILABLE` |
| 本地 API key 无效 | 401 | `invalid_api_key` | `authentication_error` | `UNAUTHENTICATED` |
| 模型或方法不存在 | 404 | `model_not_found` | `not_found_error` | `NOT_FOUND` |
| 本地文件不存在 | 404 | `file_not_found` | `not_found_error` | `NOT_FOUND` |
| 上游拒绝权限 | 403 | `upstream_error` | `permission_error` | `PERMISSION_DENIED` |
| 视频仍在生成 | 409 | `video_not_ready` | `api_error` | `INTERNAL` |
| 文件超过 512 MiB | 413 | `file_too_large` | `request_too_large` | `INTERNAL` |
| 上游配额或限流（输入超限的 429 除外，见上） | 429 | `upstream_error` | `rate_limit_error` | `RESOURCE_EXHAUSTED` |
| 候选账户均在冷却且 1 分钟内不恢复 | 429 | `rate_limit_exceeded` | `rate_limit_error` | `RESOURCE_EXHAUSTED` |
| 主用模型（`PRIMARY_MODELS`、`ULTRA_PRIMARY_MODELS`）排队超过对应的排队上限没有空闲的账号或 Worker（号池繁忙） | 429 | `rate_limit_exceeded` | `rate_limit_error` | `RESOURCE_EXHAUSTED` |
| Ultra 主用模型（`ULTRA_PRIMARY_MODELS`）额度不足（`PrimaryQuotaError`：额度 429 后达到每日类或限流类的换号上限、其余候选全部冷却、没有已启动的 Worker 或没有其他可换的账号），带 `Retry-After`，消息固定为 `Resource has been exhausted (e.g. check quota).` | 429 | `rate_limit_exceeded` | `rate_limit_error` | `RESOURCE_EXHAUSTED` |
| 普通号池额度不足（`PrimaryQuotaError` 的 `Kind` 为 `QuotaFailSwitchLimit`：每日类额度失败超过 4 个账号、限流类超过 2 次、额度失败后没有已启动的 Worker、其余候选全部冷却、没有其他可换的账号，或达到每请求 8 个账号的硬上限）与上游出口限流（`QuotaFailEgress`：所在出口正在限流、限流类换号已达上限），带 `Retry-After`，消息固定为 `Resource has been exhausted (e.g. check quota).` | 429 | `rate_limit_exceeded` | `rate_limit_error` | `RESOURCE_EXHAUSTED` |
| 出口限速排队超限（`CapacityBusyError` 的 `Reason` 为 `出口限速`：所在出口每分钟请求已满，主用模型按 `PRIMARY_QUEUE_WAIT`、其他模型整个请求累计 30 秒），带 `Retry-After` | 429 | `rate_limit_exceeded` | `rate_limit_error` | `RESOURCE_EXHAUSTED` |
| 出口限流期间要冷启动的 Worker 被冷启动闸门暂停（`CapacityBusyError` 的 `Reason` 为 `出口限流中，冷启动按配额进行`：转写、Live、Veo 选中没有 Worker 的账号，或指定账户的生成请求），带 `Retry-After`（5 秒） | 429 | `rate_limit_exceeded` | `rate_limit_error` | `RESOURCE_EXHAUSTED` |
| 租约账号在请求发出前进入冷却、又不能再换号（`AccountCoolingDownError`，`errors.Is(err, ErrAccountCoolingDown)` 成立；请求没有发到上游），带类型时附 `Retry-After`（拦下它的冷却的恢复时间），消息固定为 `Resource has been exhausted (e.g. check quota).`；原先落到兜底的 502/500 | 429 | `rate_limit_exceeded` | `rate_limit_error` | `RESOURCE_EXHAUSTED` |
| 上游过载 | 529 | `upstream_error` | `overloaded_error` | `INTERNAL` |
| 当前客户端请求被管理端取消 | 503 | `request_canceled` | `api_error` | `UNAVAILABLE` |
| 生成服务已停止 | 503 | `service_stopped` | `api_error` | `UNAVAILABLE` |
| 请求期限到期（本服务的首事件前期限、首事件后空闲期限或总时长上限，见 [长生成与中转重试](development.md#长生成与中转重试)；上游的协议错误码 4 同样映射到这里） | 504 | `upstream_error` | `api_error` | `DEADLINE_EXCEEDED` |
| 传输、Content-Type、解码或缺失终态 | 502 | `upstream_error` | `api_error` | `INTERNAL` |

429 响应另带 `X-AIStudio2API-Reason` 区分本地与上游原因（文案与状态码不变）：`local_queue`（号池繁忙，本地排队超限）、`local_cooling`（候选全部冷却或租约账号在发出前进入冷却）、`upstream_quota`（上游额度或限流 429，含换号上限）、`egress_limited`（出口限速排队超限，出口限流时换号已达上限，或出口限流期间要冷启动的 Worker 被闸门暂停）。

带 `Retry-After` 的 429（`AllCoolingError`、`CapacityBusyError`、`PrimaryQuotaError`、`AccountCoolingDownError`）在 HTTP 边界写入整秒数：建议间隔向上取整、至少 1 秒，再随机往后加 0 到 min(建议间隔 ÷ 5 秒, 3) 秒（建议间隔不到 5 秒或已过时不加），只往后加、不早于真实恢复时间，免得冷却一到期所有客户端同时重试；管理日志与请求进度里的恢复时间不含这部分。

上游 RPC 的 HTTP 404 表示传输或上游失败，默认映射为 502；公开 404 对应本地模型目录或本地资源查找失败。客户端自身断开时，访问日志记录 499 并结束响应写入。

错误对象 raw body：

**OpenAI Chat / Responses**

```json
{
  "error": {
    "message": "upstream response ended before finish frame",
    "type": "api_error",
    "code": "upstream_error"
  }
}
```

**Anthropic**

```json
{
  "type": "error",
  "error": {
    "type": "api_error",
    "message": "upstream response ended before finish frame"
  }
}
```

**Gemini**

```json
{
  "error": {
    "code": 502,
    "message": "upstream response ended before finish frame",
    "status": "INTERNAL"
  }
}
```

已开始流式响应后的终止原文：

```text
# OpenAI Chat
data: {"error":{"message":"...","type":"api_error","code":"upstream_error"}}

# OpenAI Responses
event: response.failed
data: {"response":{"id":"resp_...","object":"response","status":"failed","error":{"code":"upstream_error","message":"..."}}}

# Anthropic
event: error
data: {"type":"error","error":{"type":"api_error","message":"..."}}

# Gemini
data: {"error":{"code":502,"message":"...","status":"INTERNAL"}}

# Gemini Interactions
event: error
data: {"event_type":"error","error":{"code":"INTERNAL","message":"..."}}
```

MakerSuite 错误解析：

| 来源 | 路径 | 公开结果 |
| --- | --- | --- |
| HTTP status | response status | 保留原状态码 |
| protocol code | `$[1][0]` | 映射到协议 error code/type/status |
| protocol message | `$[1][1]` | 写入公开错误的 `message` |
| 原始形状 | `[null,[code,message,...]]` | 解析后进入规范错误事件 |

请求生命周期：

| 阶段 | HTTP / SSE 行为 | 资源状态 |
| --- | --- | --- |
| response headers 前失败 | 返回对应 HTTP status 与协议 JSON error | 释放账户槽位 |
| SSE 已开始后失败 | 发送 OpenAI error、`response.failed`、Anthropic `error` 或 Gemini error frame | 关闭上游 reader并释放账户槽位 |
| 完成帧 | 输出 finish reason、usage 与协议终止事件 | 合并 Set-Cookie并释放账户槽位 |
| 客户端取消 | 结束上游读取 | 取消请求上下文并释放账户槽位 |

欢迎二次开发，如果对你有帮助，考虑给仓库点一个Star~
