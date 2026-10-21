# 运行日志

管理页面与控制台使用同一组结构化事件。页面按请求汇总展示，控制台向标准错误输出逐行 JSON。日志来源使用 `service`、`request` 或实际执行账户的 Google 邮箱。

管理 API 通过 `GET /api/events` 推送历史快照和后续增量。账户创建等尚未绑定正式账户的认证操作使用 `auth` 来源。

## 日志结构

| 列 | 含义 | 示例 |
| --- | --- | --- |
| 时间 | 事件发生时间 | `22:26:46` |
| 级别 | 当前事件结果 | `INFO`、`WARN`、`ERROR` |
| 来源 | 服务、待调度请求或执行账户 | `service`、`request`、`account@example.com` |
| 消息 | 阶段、现场指标和错误 | `生成服务就绪`、`事件流停顿` |

管理事件 DTO（管理 API 返回的事件对象）使用以下字段：

| 字段 | 语义 | 示例 |
| --- | --- | --- |
| `time` | RFC 3339 时间；页面显示本地时间 | `2026-08-30T02:10:00+08:00` |
| `level` | 当前事件级别 | `WARN` |
| `source` | `service`、`request`、`auth` 或账户 label | `request` |
| `message` | 阶段、指标和原始错误 | `事件流停顿 | ...` |
| `event` | 稳定事件名 | `request.started`、`request.progress`、`request.finished`、`runtime.message` |
| `request` | 请求标识、状态、用量与诊断字段 | 请求事件携带该对象 |

`INFO` 记录状态推进和完成结果，`WARN` 记录等待、切换与客户端取消，`ERROR` 记录失败结果。管理页面支持按级别、来源和消息文本筛选，日志正文可以选择和横向滚动。

请求错误保存在 `request.error` 并直接展示在请求行下方。运行事件的错误详情保留在 `message` 中。

## 启动过程

管理进程先载入账户并启动控制面。完成后管理页面可以查看账户、配置和日志，此时生成服务保持 `STOPPED`。

```text
INFO  service  运行时装配 | 1/3 | 载入账户
INFO  service  运行时装配 | 2/3 | 校验 Camoufox | 账户=28
INFO  service  运行时装配 | 3/3 | 创建协议客户端
INFO  service  协议运行时就绪 | 账户=28 | 耗时=31ms
INFO  service  管理监听启动 | 地址=127.0.0.1:2048
INFO  service  管理服务就绪 | 地址=http://127.0.0.1:2048
INFO  service  管理页面已打开 | 地址=http://127.0.0.1:2048
```

点击“启动服务”后状态进入 `LAUNCHING`。服务先读取当前 `generation`（一次启动所创建的生成服务实例）的 `CachedModels`，并为全部 `enabled`、`ready` 或 `busy` 账户并发执行 `ListModels`。缓存非空时立即预热 Worker；没有模型缓存的 generation 等待首个非空真实目录，其余账户继续后台同步。首个 Worker 就绪后状态进入 `RUNNING` 并开始接收请求，其余目标 Worker 继续预热。`LAUNCHING` 期间点击“停止服务”会取消当前阶段并回到 `STOPPED`。

```text
INFO  service              生成服务启动 | 1/2 | 准备模型目录 | 缓存=0
INFO  service              生成服务启动 | 2/2 | 预热 WAA Worker | 模型=39 | 目标=5
INFO  service              模型目录后台同步完成 | 同步=28 | 非空=28 | 模型=39 | 待重试账户=0 | 耗时=2.792s
INFO  account@example.com  WAA Worker 就绪 | 页面模型=gemini-flash-latest | PID=18240 | 排队=0s | 启动=8.172s | 耗时=8.172s
INFO  service              生成服务就绪 | 模型=39 | Worker=1/5 | 耗时=10.686s
INFO  service              WAA Worker 预热完成 | Worker=5/5 | 耗时=34.903s
```

`准备模型目录` 的 `缓存` 是启动瞬间当前 generation 合并后的真实模型数。`模型目录后台同步完成` 的 `同步` 是本次无错误返回的账户数，包含空目录；`非空` 是返回非空真实目录的账户数；`模型` 是合并后的公共模型数；`待重试账户` 是 `pending set`（等待重试的账户 ID 集合）的大小。每个非空结果到达时已经写入公共目录、发布管理事件并在运行期预热更多 Worker，因此后台完成日志可以出现在 `生成服务就绪` 之后。

目录为空、单账户同步失败或单账户同步成功但仍为空的账户进入 pending ID 集合。初次 `fan-out`（同时向全部符合条件的账户发出 `ListModels`）结束后，处于 `RUNNING` 的生成服务使用单个 30 秒 ticker（定时器）对全部 pending ID 再次并发执行 `ListModels`；非空成功立即更新账户、模型和 Worker 选择，失败与空结果继续保留：

```text
INFO  service  模型目录重试完成 | 同步=2 | 非空=2 | 待重试账户=0
```

取消与失败日志：

```text
INFO   service  生成服务启动已取消 | 耗时=2.132s
ERROR  service  生成服务启动失败 | 耗时=2.132s | 错误=<ERROR>
```

启动失败、取消或 Stop 会通过 Go `context` 的取消信号停止模型目录 fan-out，并最多等待 2 秒；超时错误为 `模型目录刷新停止超时`。控制请求等待正在进行的启动或停止操作最多 12 秒，对应 `生成服务启动停止超时`、`生成服务停止超时` 或 `生成服务切换停止超时`；这些错误与 Worker 清理错误进入同一错误链。

停止生成服务会保留管理监听器，并记录 Worker 数和总耗时：

```text
INFO   service  生成服务停止 | Worker=5
INFO   service  生成服务已停止 | 耗时=3.204s
INFO   service  生成服务已处于停止状态
ERROR  service  生成服务停止失败 | 耗时=10.006s | 错误=<JOINED_ERROR>
```

`服务配置已保存` 表示 `.env` 已写入。管理进程配置通过进程重启应用；生成服务配置通过停止、启动生成服务应用。

### Worker 生命周期

每个账户从自己的实时目录选择 WAA 启动模型。`gemini-flash-latest` 在目录中可用且明确支持 `generateContent` 与 `chat` 能力字段时优先，随后按实时目录顺序尝试其余合格模型。日志保留实际页面模型。

WAA Bootstrap（页面初始化）在页面生成 proof 能力和动态请求头后，通过 WebDriver BiDi 终止用于初始化的 `GenerateContent`，模型输出量为零。首个候选完成后结束候选循环。

```text
INFO  account@example.com  WAA Worker 启动 | 1/7 | 初始化页面 | 页面模型=gemini-flash-latest
INFO  account@example.com  WAA Worker 启动 | 2/7 | 准备浏览器配置
INFO  account@example.com  WAA Worker 启动 | 3/7 | 启动 Camoufox
INFO  account@example.com  WAA Worker 启动 | 4/7 | 连接 WebDriver BiDi
INFO  account@example.com  WAA Worker 启动 | 5/7 | 载入 AI Studio
INFO  account@example.com  WAA Worker 启动 | 6/7 | 定位 WAA 服务
INFO  account@example.com  WAA Worker 启动 | 7/7 | 执行 WAA Bootstrap
INFO  account@example.com  WAA Worker 就绪 | 页面模型=gemini-flash-latest | PID=18240 | 排队=0s | 启动=10.842s | 耗时=10.842s
```

Camoufox 冷启动名额已满时，`1/7` 之前先记录一行 `WAA Worker 启动 | 等待冷启动名额 | 类型=按需|预热 | 占用=N/M`：`M` 为该类型可用的名额，按需启动比预热多 1 个保留名额（见 `WARM_STARTUP_CONCURRENCY`）。单个预热或替补任务的期限（`INIT_TIMEOUT` 加 1 分钟）包含等名额的时间：等到名额时剩余期限已不够一次完整启动（`INIT_TIMEOUT`）的，放回名额、不启动浏览器，按槽位已满处理，不记为账户预热失败，预热等待的原因写 `冷启动名额被按需启动占满（启动中 N / 上限 M，按需排队 K）`；等名额期间期限就到了的（按需启动排队时预热一律让行，排队长时多半是这种）同样按冷启动名额被占满处理，等待原因相同，不写成 Worker 槽位已满。没有可启动的账户时预热等待的原因为 `暂无可启动的账户（预热失败冷却中 N 个，启动失败暂停中 M 个）`，启动失败暂停中的账户不预热（见下文 `WAA Worker 启动失败计入账户`）。

`WAA Worker 就绪` 与 `WAA Worker 启动失败` 的 `排队` 为从开始启动到取得冷启动名额的时间（含等 runtime 租约与等名额），`启动` 为取得名额之后的浏览器启动时长，`耗时` 为两者之和。只看 `耗时` 会把排队算成启动慢；真实的启动时长看 `启动`。

启动失败的 ERROR 写明失败时的阶段、类别、排队与启动时长和原始错误；取得 runtime 租约失败时写在 `1/7` 之前，没有阶段：

```text
ERROR  account@example.com  WAA Worker 启动失败 | 页面模型=<MODEL> | 阶段=5/7 | 类别=登录失效 | 排队=12.031s | 启动=7.412s | 耗时=19.443s | 错误=登录态失效: 隔离登录态失效 url=https://accounts.google.com/v3/signin/identifier?...
ERROR  account@example.com  WAA Worker 启动失败 | 页面模型=<MODEL> | 阶段=5/7 | 类别=原因不确定 | 排队=41.207s | 启动=2m0.001s | 耗时=2m41.208s | 错误=AI Studio 输入框未就绪 url=: context deadline exceeded
ERROR  account@example.com  WAA Worker 启动失败 | 取得 runtime 租约 | 类别=环境问题 | 耗时=3ms | 错误=<ERROR>
INFO   account@example.com  WAA Worker 启动取消 | 原因=期限已到 | 阶段=3/7 | 排队=1m2.118s | 启动=4.006s
```

`阶段=n/7` 为失败时最后报告的启动阶段（见上文 1/7 到 7/7，还没报告第 2 阶段时为 1）。`类别` 为：

| 类别 | 判定 | 处理 |
| --- | --- | --- |
| `登录失效` | 确认登录失效，包括载入页面时被跳转到主机名为 `accounts.google.com` 的 Google 登录页 | 没有续签材料时标为需要重新登录；带请求租约时续签一次再启动；账户因此处于需要重新登录状态时写入失效账户记录（有续签材料、没有租约时账户状态不变，不写） |
| `指纹文件损坏` | 阶段 1–2 账户 Camoufox 指纹文件（`camoufox-fingerprint.json`）的内容无法使用（解析失败、指纹为空；文件不存在、打开文件过多、磁盘满等系统错误除外） | 指纹不是登录凭据，重新上传认证文件、验证与重新登录都修不好：不改变账户状态，不写失效账户记录，只暂停调度，日志写明指纹文件路径 |
| `认证文件损坏` | 阶段 1–2 storage state 的内容无法使用（解析失败、没有 Cookie；文件不存在、打开文件过多、没有权限等系统错误除外） | 标为需要重新登录（原因以“认证文件无法用于启动浏览器：”开头），标记成功时写入失效账户记录 |
| `环境问题` | 阶段 1–4 的其余失败（启动 Camoufox、连接 BiDi、读写文件的系统错误等），以及取得 runtime 租约失败 | 多半是机器资源或部署问题，只计入状态接口的 `startup.env_failures_10m`，不计入账户；阶段 1–2 文件不存在且账户文件确实不在时标为不可用（见下文） |
| `页面受阻` | 阶段 7 页面已经打开，但 bootstrap 提示词填不进去（`填写 bootstrap 提示词失败`）或官网 Run 按钮一直不可点（`等待官网 Run 按钮启用`）；同一错误链里有 BiDi 连接断开、浏览器退出等迹象，或失败时整个启动已经用满 `INIT_TIMEOUT` 的除外 | 多半是账户在官网有需要处理的提示（如未同意的条款或通知）：暂停同原因不确定，不改变账户状态；同一份认证文件下连续 3 次且每次号池健康时确认为失效账户（见下文） |
| `原因不确定` | 其余：阶段 5–7 的页面未就绪、WAA 失败、超时 | 机器慢与号的问题报同一串错误，只暂停调度，不改变账户状态，不能据此删号 |

`WAA Worker 启动取消` 为启动途中请求取消、任务期限已到或生成服务停止（`原因=已取消|期限已到|生成服务停止`），只写 INFO，不计入账户。请求排队时发起的后台扩容只为非启动错误另写 WARN `WAA Worker 后台扩容失败 | 错误=...`，启动错误已有上面的 ERROR，不再重复。

启动失败计入账户时（登录失效、认证文件损坏、指纹文件损坏、页面受阻、原因不确定；启动途中认证文件被替换或续签了新认证的不计入），账户另写一行 WARN；环境问题里账户文件缺失的另写一行 `账户文件缺失，标为不可用`：

```text
WARN  account@example.com  WAA Worker 启动失败计入账户 | 类别=原因不确定 | 阶段=5/7 | 连续失败=2 | 暂停调度=10m0s
WARN  account@example.com  WAA Worker 启动失败计入账户 | 类别=原因不确定 | 阶段=6/7 | 连续失败=0 | 暂停调度=1m0s | 近 5 分钟没有任何 Worker 启动成功，按系统性问题处理，不计次数
WARN  account@example.com  WAA Worker 启动失败计入账户 | 类别=页面受阻 | 阶段=7/7 | 连续失败=2 | 暂停调度=10m0s | 失效确认=2/3
WARN  account@example.com  WAA Worker 启动失败计入账户 | 类别=页面受阻 | 阶段=7/7 | 连续失败=2 | 暂停调度=10m0s | 近 10 分钟没有其他账户启动成功，不计入失效确认 | 失效确认=1/3
WARN  account@example.com  WAA Worker 启动失败计入账户 | 类别=页面受阻 | 阶段=7/7 | 连续失败=3 | 暂停调度=30m0s | 已确认为失效账户（账户状态不变，每 30 分钟重试一次）
WARN  account@example.com  WAA Worker 页面受阻已确认为失效账户 | 连续失败=3 | 最近错误=填写 bootstrap 提示词失败 value="" err=<ERROR> | 可在账户页删除，或到官网登录检查是否有需要处理的提示
WARN  service  大量账户页面受阻，可能是官网页面改版，暂不确认为失效 | 账户数=37 | 启动尝试账户数=120
WARN  account@example.com  WAA Worker 启动失败计入账户 | 类别=指纹文件损坏 | 阶段=2/7 | 连续失败=1 | 暂停调度=2m0s | 指纹文件=<账户目录>/camoufox-fingerprint.json | 与登录无关，账户状态不变、不计入失效账户（重新上传认证文件、验证与浏览器登录都修不好）：从备份恢复该文件，或删除它（下次启动按当前版本重新生成指纹）后清除暂停
WARN  account@example.com  WAA Worker 启动失败计入账户 | 类别=登录失效 | 阶段=5/7 | 连续失败=1 | 暂停调度=否 | 已标为需要重新登录
WARN  account@example.com  WAA Worker 启动失败计入账户 | 类别=登录失效 | 阶段=5/7 | 暂停调度=否 | 账户状态不变，不计入失效账户（有续签材料时留给带租约的请求恢复；启动期间验证通过或认证已更新时不标记）
WARN  account@example.com  WAA Worker 启动失败 | 账户文件缺失，标为不可用（文件恢复后自动恢复，不计入失效账户） | 阶段=1/7 | 错误=读取 storage state: open ...: no such file or directory
INFO  account@example.com  WAA Worker 保底试探 | 候选都在启动失败暂停中，提前放行最早到期的账户 | 原暂停至=2026-10-21T16:30:00+08:00
INFO  account@example.com  WAA Worker 启动成功，清除失效账户记录
```

- 原因不确定：`连续失败` 为计入退避的连续失败次数，`暂停调度` 依次为 `2m0s`、`10m0s`、`30m0s`（封顶）；近 5 分钟没有任何 Worker 启动成功（包括生成服务刚启动）时只暂停 `1m0s`、次数不加。暂停只在内存里，到期自动唤醒调度。
- 页面受阻：暂停与原因不确定相同（`2m0s`、`10m0s`、`30m0s` 封顶，近 5 分钟没有任何 Worker 启动成功时只暂停 `1m0s`、不计次数；已确认为失效的账户除外，见下文），账户状态不变。同一账户、同一份认证文件下连续 3 次页面受阻，而且每一次失败时号池是健康的（失败前 10 分钟内至少有一个其他账户的 Worker 启动成功，且不处于下面说的大面积爆发），写 WARN `WAA Worker 页面受阻已确认为失效账户 | 连续失败=<N> | 最近错误=<ERROR> | 可在账户页删除，或到官网登录检查是否有需要处理的提示`，写入失效账户记录（`kind=page_blocked`），列入失效账户、可以一键删除；账户仍是就绪，之后每次失败都暂停 `30m0s`（近 5 分钟没有任何 Worker 启动成功时也是，只是不计次数、不更新记录），问题消失、启动成功时清除记录（`WAA Worker 启动成功，清除失效账户记录`）。号池不健康的那次照常暂停，但确认失效的连续次数（`失效确认=n/3`）不增加、也不清零，行尾写明原因（`近 10 分钟没有其他账户启动成功，不计入失效确认`，或 `大量账户页面受阻（可能是官网页面改版），不计入失效确认`）；按系统性问题处理的那次同样不计入。已确认的账户之后每次失败行尾写 `已确认为失效账户（账户状态不变，每 30 分钟重试一次）`。
- `大量账户页面受阻，可能是官网页面改版，暂不确认为失效 | 账户数=<N> | 启动尝试账户数=<M>`（`service` 写 WARN）：最近 30 分钟内出现页面受阻的不同账户数 `账户数` 超过 max(10, 最近 30 分钟内尝试启动的不同账户数 `启动尝试账户数` × 20%)，多半是官网页面改版或灰度。两个数都不含已确认为页面受阻失效的账户：它们每 30 分钟重试一次、每次都页面受阻，留在号池里等待检查的失效账户不会让号池一直处于爆发。进入这种状态时写一次，期间不把任何新的页面受阻确认为失效（已有记录不动），状态接口的 `startup.page_blocked_burst` 为 `true`，管理页账户页显示黄色提示；不要据此批量删号。
- 指纹文件损坏：暂停与原因不确定相同（`2m0s`、`10m0s`、`30m0s` 封顶），但与机器负载无关，每次都计次数；换一类原因时从头计。账户状态不变，不写失效账户记录，失效账户列表把它列在“启动反复失败（未确认）”里。`指纹文件` 为该账户的 `camoufox-fingerprint.json`（多半是首次启动或升级后写这个文件时磁盘满或进程被杀，留下空文件或截断的 JSON）：从备份恢复，或删除它（下次启动按当前版本重新生成该账户的指纹），再在管理页清除暂停。
- 登录失效与认证文件损坏：不暂停调度。账户已标为需要重新登录时写入失效账户记录，`连续失败` 为同一份认证文件下同一类别的连续次数（记录的 `count`）；账户状态没变（有续签材料、留给带租约的请求恢复，或启动开始后验证通过、认证已更新，较早开始的启动的标记被拒绝）时写 `不计入失效账户`，没有 `连续失败`，不写记录，失效账户列表不列出，不会被一键删除。
- `账户文件缺失，标为不可用`：阶段 1–2 读认证文件或指纹时文件不存在，且账户的 `account.json` 或 `storage-state.json` 确实不在（多半是整体替换账户目录的中途）：与请求取账户时发现文件缺失一样把就绪账户标为不可用，文件复制回来、稳定一段时间后由账户目录监视重新同步模型目录恢复；目录一直不在时按原有规则移出账户池。文件已经复制回来的什么都不改，只计入环境类失败。
- `WAA Worker 保底试探`：候选都在启动失败暂停中、又没有已启动的 Worker 时，全局每分钟提前放行一个最早到期的账户，`原暂停至` 为它原来的暂停期限（RFC 3339，服务器本地时区）。
- `WAA Worker 启动成功，清除失效账户记录`：有失效账户记录的账户（如续签恢复之前记下的登录页跳转，或确认为页面受阻的账户问题已经消失）之后启动成功。
- 认证文件无法用于启动浏览器、标记却失败时另写 WARN `WAA Worker 标记需要重新登录失败 | 错误=...`。

按需容量事件说明当前热池动作：

| 事件 | 语义 |
| --- | --- |
| `WAA Worker 按需扩容 | Worker=N/M` | 活动 Worker 未达 `MAX_ACTIVE_WORKERS`，新 Worker 成功发布 |
| `WAA Worker 按需替换 | Worker=N/M` | `pending Worker`（正在启动的替代 Worker）成功启动并替换一个空闲 Worker |
| `WAA Worker 空闲回收 | Worker=N/M` | 超出热池目标的 Worker 空闲 5 分钟后关闭 |
| `WAA Worker 旧实例停止失败` | 旧实例关闭失败，发布中止并开始关闭 pending Worker |
| `WAA Worker 重建 | 模型=... | 重放当前请求` | 当前账户 Worker 已失效，业务请求在新实例重放；下一行 `原因:` 为触发重建的错误 |
| `WAA Worker 后台恢复放弃 | 等待=<DURATION> | ...` | 后台关闭失败的 Worker 时超过 `REQUEST_TIMEOUT` + 30 秒仍有同账户请求在用，放弃这次关闭、账户恢复接受请求；浏览器已失败的 Worker 由失败 Worker 巡检重新登记，协议 Code 5 的 Worker 在再次失败时重新登记 |
| `WAA Worker 后台恢复出错 | 错误=...` | 后台关闭失败的 Worker 出错（浏览器关不掉时另有 `WAA Worker 停止失败`）；不固定账户的请求已经换号 |
| `常驻 Worker 已失败，后台关闭以便重新预热` | 失败 Worker 巡检登记的后台关闭完成，随后由预热补齐；出错时为 `关闭失败的常驻 Worker 出错 | ...` |
| `WAA Worker 已更新 | 模型=... | 重放当前请求` | 并发路径已经替换 Worker，当前请求使用新实例 |

不固定账户的生成请求没等到失败 Worker 的后台关闭完成就换号时，请求进度写 `WAA Worker 恢复等待 <DURATION> 未完成（同账号仍有进行中的请求），后台继续关闭失败的 Worker，换号重试 | 原因=...`（关闭出错时为 `WAA Worker 恢复时关闭失败的 Worker 出错（<ERROR>），换号重试`，后台放弃时为 `WAA Worker 恢复的后台关闭已放弃，换号重试`），随后照常记录 `账号切换`；换号没有其他可用账户时写 `换号没有可用账号，回到原账号等待 Worker 恢复后重放`，换号次数用完或到连续失败上限时还有没等到关闭就换走的原账户，同样回去等待，写 `换号次数已用完，回到原账号等待 Worker 恢复后重放` 或 `已连续 5 次失败，回到原账号等待 Worker 恢复后重放`。这次失败计入连续失败上限后就到上限的请求不再换号，直接等原账户关闭完成。换号后的选号在排队（其他账户都忙）时原账户关闭完成，写 `换号排队中原账号的 WAA Worker 已关闭完成，原账号重新参与选号`：不再排除原账户，按原来的排队位置重新选号，不固定到原账户（关闭后它没有 Worker，要冷启动并占一个 Worker 槽位），原账户与其他账户哪个先能用就用哪个。回到原账户等待时请求超时或被取消，错误为 `回到原账号等待 <DURATION> 后请求超时（最后原因=WAA Worker 恢复，上次失败=...）: context deadline exceeded`（取消时为 `请求已取消`），对外仍是 504 或取消的官方措辞。

按需扩容、按需替换与空闲回收按 Worker 分区计数：上表的 `N/M` 为普通分区的 Worker 数与 `MAX_ACTIVE_WORKERS`；Ultra 分区（Ultra 号池的账户：权益为 Ultra，`ULTRA_INCLUDE_PRO=true` 时也含权益为 Pro 的账户）的同类事件以 `Ultra WAA Worker` 开头，`M` 为 `ULTRA_MAX_ACTIVE_WORKERS`，常驻数为 `ULTRA_WARM_WORKER_LIMIT`。预热暂停原因中的 `Ultra Worker 槽位已满` 表示 Ultra 分区已满；预热不足按分区判断，Ultra 分区不足时记录 `Ultra WAA Worker 预热等待 | Worker=N/M`，`N/M` 为该分区驻留的 Worker 数与预热目标。保存服务配置或启动生成服务后生效的 `ULTRA_EXCLUSIVE` 发生变化时记录 `Ultra 独占设置已更新并立即生效 | 独占=<BOOL>`。服务配置热更新的汇总行 `配置已热更新 | ...` 带 `Pro 并入 Ultra=<BOOL>`，为账户池当前生效的 `ULTRA_INCLUDE_PRO`。

保存服务配置使 `ULTRA_INCLUDE_PRO` 实际切换时（热更新；同一次保存还改了需要重建生成服务的字段时随重建生效，不写这几行），`service` 依次记录下面几行。数值都在切换之后读取当前状态：`并入` 为账户池当前的开关，`Ultra 号池账户` 为当前属于 Ultra 号池的账户数，两个分区的 Worker 数为驻留与正在启动的 Worker 之和，`/` 后为 `ULTRA_MAX_ACTIVE_WORKERS` 与 `MAX_ACTIVE_WORKERS`。已运行的 Worker 不重启，只改按新分区计数：

- INFO `Pro 账户并入 Ultra 号池设置已更新并立即生效 | 并入=<BOOL> | Ultra 号池账户=<N> | Ultra 分区 Worker=<N>/<M> | 普通分区 Worker=<N>/<M>`：开关已在账户池生效，排空中的 Worker 已按新成员改标分区，等待容量的请求已唤醒。
- WARN `<分区> Worker 超过峰值 | 当前=<N> | 峰值=<M> | 空闲 Worker 将逐步回收，可调大对应峰值`：`<分区>` 为 `Ultra 分区` 或 `普通分区`，换分区的 Pro Worker 让该分区驻留与启动中的 Worker 超过峰值。不会主动关闭忙碌的 Worker，超出的空闲 Worker 空闲 5 分钟后回收；开启时通常是 Ultra 分区，可调大 `ULTRA_MAX_ACTIVE_WORKERS`。
- WARN `ULTRA_EXCLUSIVE=true 时 Pro 账户不再服务普通路径；Pro 账户上的文件与视频改从 /ultra 引用`：只在开关开启且 `ULTRA_EXCLUSIVE=true` 时记录（`ULTRA_EXCLUSIVE=false` 时普通路径仍可使用 Pro 账户，不记录）。此后普通路径的公开模型目录不再含只有 Pro、Ultra 账户能服务的模型，绑定在 Pro 账户上的资源从普通路径引用返回 400。

```text
INFO  service  Pro 账户并入 Ultra 号池设置已更新并立即生效 | 并入=true | Ultra 号池账户=42 | Ultra 分区 Worker=7/5 | 普通分区 Worker=6/10
WARN  service  Ultra 分区 Worker 超过峰值 | 当前=7 | 峰值=5 | 空闲 Worker 将逐步回收，可调大对应峰值
WARN  service  ULTRA_EXCLUSIVE=true 时 Pro 账户不再服务普通路径；Pro 账户上的文件与视频改从 /ultra 引用
```

单个 Worker 停止事件：

```text
INFO   account@example.com  WAA Worker 停止 | PID=18240
INFO   account@example.com  WAA Worker 已停止 | PID=18240 | 耗时=3.014s
ERROR  account@example.com  WAA Worker 停止失败 | PID=18240 | 耗时=10.002s | 错误=<JOINED_ERROR>
```

关闭上界由三个连续阶段构成：

| 阶段 | 上界 | 错误语义 |
| --- | ---: | --- |
| BiDi `session.end` | 3 秒 | 超时后关闭 BiDi 连接并继续关闭进程 |
| 进程终止与 `command.Wait` | 5 秒 | Windows 依次执行 `taskkill /T /F` 与 `Process.Kill`，失败原因保留 |
| 浏览器 profile 目录删除重试 | 2 秒 | 每 100ms 重试，最终返回最后一次 `RemoveAll` 错误 |

`Worker.Close` 的正常总体上界约为 10 秒，另加调度开销。关闭失败时 Worker、进程句柄、`runtime lease`（Worker 占用的账户租约）、热池记录和 generation（Worker 版本号）保持可重试；再次 Stop 会重跑关闭链。热替换中旧实例与 pending Worker 都关闭失败时，两份实例都占用容量槽。

## API 请求

请求事件通过 `request.id` 关联。页面将开始、进展和结束合并为一行，展示最新账户、HTTP 状态、模型、耗时和输出统计。展开详情可查看接口、输入用量、原始终止原因、完整事件时间线与 JSON。

```text
22:11:47  200  gemini-3.8-flash  4.76 s  工具调用 1
思考 32 · 回复 38 · 输出合计 70 tokens · 平均速度 14.7 tokens/s
```

| 字段 | 含义 |
| --- | --- |
| HTTP 状态 | 请求结果；流式响应开始后的错误仍按实际失败状态记录 |
| 耗时 | 从请求进入服务到处理结束，单位为秒 |
| 思考 | `request.usage.reasoning_tokens` |
| 回复 | `request.usage.reply_tokens`，包含文本、工具调用等非思考输出 |
| 输出合计 | `request.usage.output_tokens`，等于思考加回复 |
| 平均速度 | 输出合计除以请求总耗时，单位为 tokens/s，包含准备与等待时间 |
| 工具调用 | 本次模型输出的函数调用事件数 |
| 输入用量 | `request.usage.input_tokens`，包含输入消息与工具声明 |
| 总用量 | `request.usage.total_tokens`，包含输入和输出 |

用量在生成服务返回 usage 后展示；用量未知时省略 `request.usage`。平均速度衡量端到端吞吐，网络缓冲或集中到达的响应也使用同一计算区间。

`request.state` 区分 `running`、`completed`、`tool_calls`、`limited`、`blocked`、`failed` 和 `cancelled`。策略终止保留 HTTP `200` 并显示原始 `finish_reason`；达到输出上限时显示 `max_tokens`。未知终止原因保留为 `provider_<code>`。

上游在终止时给出结束说明（Gemini API 的 `finishMessage`，Playground 与 Build 都会解码；例如 `MALFORMED_FUNCTION_CALL` 时写明出错的调用）时，请求时间线另写一条进度 `上游结束说明 | 结束原因=<reason> | 说明=<message>`，排查时不用再抓原始响应。结束原因为 `stop` 或 `stop_sequence` 时级别为 INFO，其余为 WARN；说明超过 2000 字时截断并以 `…` 结尾。这一行同样进入排查记录（`/trace/`）的 `timeline`；`request.finished` 的字段不变。说明另外作为 Gemini 响应的 `finishMessage` 返回，其他协议不携带（见 [protocol.md](protocol.md)）。

```text
WARN  account@example.com  上游结束说明 | 结束原因=malformed_function_call | 说明=<上游说明>
```

上游拒绝当前输入（prompt feedback）并给出拦截说明（`blockReasonMessage`）时，`request.error` 在原因后附上这段说明，例如 `AI Studio 拒绝当前输入: prohibited_content: <上游说明>`；没有说明时只有原因。客户端收到的错误消息仍按原因生成官方措辞，不含这段说明。

控制台每行是一条独立 JSON 事件，请求数据与中文摘要分离：

```json
{"time":"2026-09-06T14:11:47Z","level":"INFO","msg":"工具调用完成","event":"request.finished","source":"account@example.com","request":{"id":"chatcmpl_example","state":"tool_calls","model":"gemini-3.8-flash","status":200,"duration_ms":4758,"tool_calls":1,"finish_reason":"stop","usage":{"input_tokens":100,"reasoning_tokens":32,"reply_tokens":38,"output_tokens":70,"total_tokens":170,"average_tokens_per_second":14.712064}}}
```

采样参数保存在 `request.parameters`。`first_event_ms`、`queue_ms`、`proof_ms` 和 `upstream_bytes` 作为 JSON 诊断字段保留：`queue_ms` 是请求进入服务到取得最终执行账户的时间，`proof_ms` 是等待并生成 WAA proof 的累计时间。页面支持按级别、账户以及模型、请求 ID、状态码搜索。

客户端取消记录为 `499`；认证、额度和上游失败分别使用对应 HTTP 状态与 `request.error`。

管理端取消活动请求或停止生成服务时，仍连接的客户端按公开协议收到 `503 request_canceled` 或对应的流式 error event。

## 排队等待

生成请求在选号时没有立即可用的账号或 Worker 就排队等待。一次尝试排队满 2 秒仍没有取得账号时，请求时间线写一条 INFO `排队等待`；之后仍在等待的，每个请求最多每 15 秒写一条 WARN。很快取得账号的请求不写。重复的 WARN 全进程限流：每秒最多 2 条，最多攒 30 条；被省略的条数在下一次放行时另写一条服务日志 `排队进度日志已省略 N 条`，不写进任何请求的时间线。主用模型（按请求的号池命中 `PRIMARY_MODELS` 或 `ULTRA_PRIMARY_MODELS`）因容量原因排队时不写这一行，超过排队上限时只写 `主用模型排队超过 Xs，立即返回 429`；普通号池的主用模型在候选全部冷却时的等待不受上限限制，照常写；Ultra 主用模型不等待冷却，写下一节的进度。不等待的选号（流式优先 Playground 的第一步）不写。

| 字段 | 含义 |
| --- | --- |
| `原因` | 当前在等什么：`账号全部忙碌`、`没有空闲的 Worker 槽位`、`Worker 正在启动`、`Worker 正在扩容`、`Worker 容量已满`、`账号忙碌`、`账号冷却中`、`候选账户启动失败暂停中`（备用账户都在启动失败暂停中，等暂停到期或保底试探；属于容量原因，主用模型受排队上限限制） |
| `已等待` | 排队的时间：每次选号从第一次等待到取得账号或选号结束按墙钟计，换号重试接着累加；两次选号之间上游处理、退避与 Worker 恢复的时间不算 |
| `尝试` | 第几次选号（换号重试递增） |
| `同条件队列` | 在选择条件相同的请求（模型、通道限定、号池等）中排第几、共几个 |
| `热空闲`、`热忙碌`、`备用`、`备用忙碌` | 本轮复查时该模型的候选数：已有 Worker 的账号与备用账号，冷却中的不计，启动失败暂停中的备用账户也不计 |
| `启动失败暂停` | 本轮因启动失败暂停而不作为候选的备用账户数，为 0 时不写 |
| `启动中`、`最早恢复` | 有候选的 Worker 正在启动；候选中最早恢复的冷却还剩多久 |

请求在排队中超时或被取消时，错误写明最后的等待原因与已等待时间（同样只算排队的时间，上一次尝试在上游处理的时间不算），例如 `排队 5m0.004s 后请求超时（最后原因=账号全部忙碌）: context deadline exceeded`。它随 `request.finished` 的 `request.error` 写进请求日志与用量账本，对外仍是 504 或取消的官方措辞。

```text
INFO  service  排队等待 | 原因=账号全部忙碌 | 已等待=2.004s | 尝试=1 | 模型=gemini-3.7-flash | 同条件队列=3/57 | 热空闲=0 | 热忙碌=40 | 备用=0 | 备用忙碌=0
WARN  service  排队等待 | 原因=账号冷却中 | 已等待=17.012s | 尝试=2 | 模型=gemini-3.7-flash | 同条件队列=1/12 | 热空闲=0 | 热忙碌=0 | 备用=0 | 备用忙碌=0 | 最早恢复=41s
WARN  service  排队进度日志已省略 128 条（全局限流：每秒最多 2 条）
```

## Ultra 主用模型额度不足

按请求的号池命中 `ULTRA_PRIMARY_MODELS`（默认 `gemini-4-argon`）的请求没有额度时立即返回 429，不在服务里一直换号重试，请求时间线写下面三种进度（普通号池的 `PRIMARY_MODELS` 与其他模型不写）：

| 进度 | 级别 | 何时写 |
| --- | --- | --- |
| `Ultra 主用模型候选全部冷却，立即返回 429` | WARN | 选号时候选全部冷却（还没有遇到额度 429）：不再等待 1 分钟内恢复的冷却，立即按全部冷却返回；`最早恢复` 为候选中最早恢复的冷却还剩多久，也是 `Retry-After` |
| `额度 429，换号重试（只用已启动的 Worker）` | INFO | 遇到额度 429（或租到的账号在请求发出前刚进入冷却）后换号：`额度失败` 为本请求的额度失败次数与上限（额度失败后最多再换 2 次账号），`原因` 为这次的上游错误；紧接着的这次换号只用已启动的 Worker，不现场冷启动备用账号（之后再因额度以外的原因换号时按普通规则选号） |
| `Ultra 主用模型额度不足，立即返回 429` | WARN | 请求以额度不足结束：额度失败达到换号上限、其余候选全部冷却、额度 429 后没有已启动的 Worker（这时分区还能扩容就在后台为一个备用账号启动 Worker，不等它），或没有其他可换的账号 |

最后一行的字段：

| 字段 | 含义 |
| --- | --- |
| `原因` | `额度失败已达换号上限`、`其余候选全部冷却`、`额度 429 后没有已启动的 Worker` 或 `没有其他可换的账号`（额度失败后号池里能服务该模型的账号都已试过，或剩下的暂时不可调度，例如需要重新登录；换号次数用完时也是这个原因，账号少的 Ultra 号池常见） |
| `额度失败` | 本请求因额度失败的尝试次数（输入超过账号上限的 429 不算） |
| `总耗时` | 从第一次选号到返回 429 的墙钟时间 |
| `建议重试` | 写进 `Retry-After` 的时间：其余候选全部冷却时为最早恢复，达到换号上限或没有其他可换的账号时为 5 秒与本请求遇到的最早额度恢复中较早者，没有已启动的 Worker 时为 5 秒，都至少 1 秒 |

额度失败后换号的进度，以及尝试结束时就判定额度不足的（达到换号上限、换号次数用完）写在这次尝试的账号名下；选号时判定的（全部冷却、没有已启动的 Worker、没有其他可换的账号）写在 `service` 名下。请求以 HTTP 429 结束，`request.error` 为中文说明（号池、模型、原因、额度失败次数与最早恢复），对外只返回固定的限流文案 `Resource has been exhausted (e.g. check quota).`，不含上游原文。

```text
WARN  service  Ultra 主用模型候选全部冷却，立即返回 429 | 最早恢复=42s
INFO  account@example.com  额度 429，换号重试（只用已启动的 Worker）| 额度失败=1/2 | 原因=AI Studio GenerateContent 返回 HTTP 429、协议错误码 8: …
WARN  account@example.com  Ultra 主用模型额度不足，立即返回 429 | 原因=额度失败已达换号上限 | 额度失败=3 | 总耗时=6.214s | 建议重试=5s
WARN  service  Ultra 主用模型额度不足，立即返回 429 | 原因=额度 429 后没有已启动的 Worker | 额度失败=1 | 总耗时=1.032s | 建议重试=5s
```

## 流式等待

连续 15 秒没有阶段进展时会写入诊断日志。请求在收到上游终态、客户端取消或达到 `REQUEST_TIMEOUT` 时结束。设置了 `FIRST_EVENT_TIMEOUT` 时，一次尝试从向上游发送起超过该时长仍没有上游事件，写入"上游首事件超时"并换号重试。

| 事件 | 当前停留位置 | 重点字段 |
| --- | --- | --- |
| `请求准备等待` | 账户已选定，正在准备 WAA proof 或等待 GenerateContent 响应头 | `当前`、`模型` |
| `请求准备结束` | 准备阶段超过 15 秒后开始接收响应体 | `等待`、`WAA`、`响应头`、`模型` |
| `上游首事件等待` | 上游响应体已经建立，尚未解出语义事件 | `网络字节`、`最近网络` |
| `上游首事件到达` | 首事件等待后解出第一个语义事件 | `等待`、`事件`、`模型` |
| `事件流停顿` | 已有语义事件，连续 15 秒没有下一事件 | `最近事件`、`推理`、`正文`、`网络字节`、`最近网络` |
| `事件流恢复` | 停顿后解出下一语义事件 | `停顿`、`当前事件` |
| `账号切换` | 当前账户在首个上游语义事件前失败 | 当前账户来源、模型和下一行原始原因 |

`网络字节` 是当前账户尝试累计读取的上游响应体字节数，`最近网络` 是最近一次读取距日志时刻的时间。`网络字节=0` 表示响应体尚未产生数据；数值增长且 `最近网络` 较短，表示网络仍有数据进入、解码器尚未形成新的语义事件。

```text
WARN  account@example.com  请求准备等待 | 已等待=15s | 当前=等待上游响应头 | 模型=gemini-3.7-flash
INFO  account@example.com  请求准备结束 | 等待=47.755s | WAA=1.204s | 响应头=46.551s | 模型=gemini-3.7-flash
```

```text
WARN  account@example.com  上游首事件等待 | 已等待=15s | 模型=gemini-3.7-flash | 网络字节=0
INFO  account@example.com  上游首事件到达 | 等待=28.447s | 事件=reasoning | 模型=gemini-3.7-flash
```

```text
WARN  account@example.com  事件流停顿 | 模型=gemini-3.7-flash | 已等待=15s | 最近事件=reasoning | 推理=4 | 正文=0 | 网络字节=18240 | 最近网络=15.001s
INFO  account@example.com  事件流恢复 | 模型=gemini-3.7-flash | 停顿=1m31.208s | 当前事件=reasoning
```

流式公开协议在连续 10 秒没有语义事件时发送心跳，格式按协议不同。OpenAI Chat、OpenAI Responses 与 Gemini Interactions 发送 SSE 注释帧：

```text
: ping

```

Anthropic 按官方格式发送 ping 事件（Anthropic SDK 识别并跳过，new-api 按官方事件转发）：

```text
event: ping
data: {"type":"ping"}

```

Gemini `:streamGenerateContent` 在 SSE（`alt=sse`）与 JSON 数组两种格式下都只写一个空行：SSE 中的空行不构成事件，JSON 数组元素之间的空白不影响解析。google-genai Python SDK 会把 `: ping` 注释行当作 JSON 解析而报 `UnknownApiResponseError`，所以 Gemini 不用注释帧。

客户端把心跳作为连接存活信号，正文、推理和 usage 继续使用各协议的 `data` 或命名事件。Anthropic 在调用生成服务前发送起始事件；OpenAI Chat 与 Responses 在取得生成事件流后发送起始事件。

## 账户事件

HTTP `401`、协议 Code 16、Worker 启动时跳转登录页或签名 Cookie 失效会使用该 Chrome 导入账户保存的 OAuth/DBSC 材料续签 Cookie、重置 WAA Worker并重放一次请求。续签先等待同账户其他请求结束，12 秒内没有结束时不再等待、继续续签；等待期间请求取消时放弃本次续签，记录 `账户认证续签放弃`。

```text
INFO  account@example.com  账户认证续签 | 1/2 | 刷新 Cookie
INFO  account@example.com  账户认证续签 | 2/2 | 重置协议运行时
INFO  account@example.com  账户认证续签完成 | 耗时=1.116s
ERROR account@example.com  账户认证续签失败 | 耗时=1.116s | 错误=<ERROR>
WARN  account@example.com  账户认证续签 | 同账户其他请求 12s 内没有结束，不再等待
WARN  account@example.com  账户认证续签放弃 | 错误=<ERROR>
```

HTTP `403` 与协议 Code 7 保留当前账户和模型的 `verified` 记录，不标记永久权限失败；没有文件绑定的生成请求对该账号该通道的这个模型写入冷却并记 `账号冷却 | 类型=无权限 | 范围=<模型或 build:模型> | 恢复=<时间> | 档位=<1..>`（30 分钟、6 小时、到太平洋时间次日零点，该模型成功一次即从头计）。协议 Code 5、Worker 进程失败或 Worker 被替换时重建当前账户 Worker 并重放一次；其他可重试错误写入临时冷却。

```text
WARN  account@example.com  账号切换 | 模型=gemini-3.7-flash
                           原因: AI Studio GenerateContent 返回 HTTP 403、协议错误码 7: The caller does not have permission
```

同一账户 Build 与 Playground 是否共用某模型每日额度的判定每记录一次证据，`service` 写一行 `每日额度通道判定 | 模型=<MODEL> | 另一通道也达到限额=<N> | 另一通道仍可用=<N> | 结论=<VERDICT>`，结论为 `共用（达到每日限额时同时冷却两个通道）`、`独立（两个通道分别计算额度）` 或 `观察中`，结论变化的那次为 WARN、其余为 INFO。判定按账户真实权益分组：权益为 Ultra 的账户单独一组，这一行在模型后带 `权益=Ultra`（`每日额度通道判定 | 模型=<MODEL> | 权益=Ultra | ...`）；其余权益不带这个字段，`ULTRA_INCLUDE_PRO=true` 时并入 Ultra 号池的 Pro 账户同样不带。

账户页的 `Free`、`Pro`、`Ultra` 与 `Plus` 来自 `GetAiStudioBenefitTier`，始终是账户的真实权益：`ULTRA_INCLUDE_PRO=true` 时并入 Ultra 号池的 Pro 账户仍显示 `Pro`，另带“Ultra 号池”标记。Paid 模型先按权益和模型访问方式筛选，成功调用过目标模型的账户排在同模型未知账户之前。

管理端账户操作事件：

| 事件 | 结果 |
| --- | --- |
| `账户添加 | 1/2`、`2/2`、`账户添加完成` | 隔离登录、保存认证状态、同步模型目录 |
| `账户登录 | 1/2`、`2/2`、`账户登录完成` | 更新当前账户认证材料与 Worker 实例 |
| `账户验证`、`账户验证完成` | 验证 AI Studio、权益与实时目录 |
| `账户配置已更新`、`账户已删除` | 账户配置与运行对象已更新 |
| `批量删除账户` | 批量删除接口（403 无权限账户与失效账户的“一键删除”）一次请求的汇总，每个删除的账户另写一条 `账户已删除` |
| `清除启动失败记录 | 来源=<来源>` | 认证文件更新（`新增账户`、`浏览器登录`、`重新导入`、`认证文件更新`）或 `验证通过` 时清除了该账户的失效账户记录或启动失败暂停 |
| `已清除启动失败记录 | 账户=<N>` | 管理页“清除记录”“清除暂停”（`POST /api/accounts/startup-failures/clear`）清除了 N 个账户的记录与暂停，来源为 `auth` |
| `账户模型目录同步完成` | 新目录已写入当前模型目录和账户调度状态 |
| `账户模型目录等待重试` | 当前同步失败，运行期重试继续处理 |

批量删除接口每次请求结束写一行 `批量删除账户 | 删除=<N> | 不存在=<N> | 失败=<N> | 剩余=<N>`：`删除` 为本次删除的账户数，`不存在` 为号池里已经没有的账户数（只清除它的 403 记录），`失败` 为没有删掉的账户数（多半是一直在处理请求、等不到空闲，即 `account_busy`），`剩余` 为号池里还有、超过单次上限 500 本次没有处理的账户数（单次上限只计号池里还有的账户，已经没有的计入 `不存在`，不占名额，也不计入 `剩余`）。

失效账户记录（`.startup-failures.json`）在启动生成服务时与账户池对照，有变化时（下面四个数任何一个不为 0）`service` 写一行 INFO `失效账户记录对账 | 重新标为需要重新登录=<N> | 恢复页面受阻暂停=<N> | 清除已不在账户池中=<N> | 清除认证文件已更新=<N>`：同一份认证文件下连续 2 次以上确认登录失效、没有续签材料的账户重新标为需要重新登录，目录已删除或认证文件已换过的账户删除记录；页面受阻的记录认证文件没换过就保留（账户状态不变，仍列为失效账户），`恢复页面受阻暂停` 为其中最近一次失败（记录的 `last_at`）后还不到 30 分钟、重启后恢复暂停到 `last_at` + 30 分钟的账户数（只有这一项不为 0 也会写这一行），已经超过 30 分钟的不恢复暂停，让账户马上重试。文件读不出或解析失败时写 WARN `失效账户记录读取失败，重新开始记录 | 错误=...`，写入失败写 WARN `失效账户记录保存失败 | 错误=...`（下一次变化时重试），重新标记失败时该账户写 WARN `失效账户重新标记失败 | 错误=...`。

资源与 `scope`（状态记录范围）事件：

| 事件 | 语义 |
| --- | --- |
| `文件引用复制` | Drive file 已临时复制到本次目标账户 |
| `内联附件处理完成` | 本次请求的附件数量、原始字节数与处理耗时 |
| `临时文件清理失败` | 本次请求的临时 Drive 文件回收失败，原始错误保留 |
| `转录账号切换` | 转录生成阶段在首个结果前切换候选 |
| `Bidi 账号切换` | Live/Robotics setup 阶段切换候选 |

较晚返回的认证结果只有在 `authGeneration` 和 `checkedAt` 均匹配当前账户时才应用；较晚返回的模型成功或冷却结果只有在 `modelAccessGeneration` 和 `checked_at` 均匹配当前模型目录时才应用。日志记录实际应用到当前状态的变更和持久化错误。

## 用量账本

`runtime/requests.db` 是本地 SQLite 账本（纯 Go 驱动，`CGO_ENABLED=0` 构建同样可用）。`/v1`、`/v1beta`、`/trace/` 与 `/ultra/` 下通过 API key 校验的每个 POST 请求完成后写入一行：请求 ID、完成时间、协议、路径、号池（按请求入口记录：`/ultra` 请求为 `ultra`，其余为空，用量接口中显示为 `normal`；并入 Ultra 号池的 Pro 账户服务的 `/ultra` 请求同样为 `ultra`，`ULTRA_INCLUDE_PRO` 切换前写入的记录不改写）、模型、账户、通道、HTTP 状态、`request.state`、耗时、首个上游事件、排队时间、token 用量、工具调用数、错误摘要、最终结果之前未成功的上游尝试，以及本地的实际服务模型、降级判定结论与回复指纹。token 计数请求（`/v1/messages/count_tokens`、`:countTokens`）不写入，避免抬高请求数；协议按路径归为 `openai-chat`、`openai-responses`、`anthropic`、`interactions`（`/v1/interactions` 与 `/v1beta/interactions`，不并入 `gemini`）、`gemini`、`images`、`audio`、`videos`、`files` 或 `other`，排查路由与 Ultra 路由的请求按去掉 `/trace`、`/ultra` 前缀后的协议归类。账户与错误内容与请求日志相同，不保存 API key、管理令牌、Cookie 与请求头；错误摘要与路径按 2000 字截断；模型名来自客户端，没有通过模型目录校验的请求也会记录，模型名与实际服务模型按 200 字截断。

同一事务按 UTC 小时与服务器本地日累加汇总与耗时分布（汇总键包含号池）：完整落在范围与单个分桶内的本地日读取日汇总，其余整小时读取小时汇总，范围边缘与跨分桶的部分读取原始记录。服务器时区变化后，下次启动时从小时汇总与原始记录重建本地日汇总。写入由后台协程批量提交，不阻塞响应：等待写入的记录超过 4096 条时丢弃新记录，写入恢复后输出丢弃数。记录与汇总按服务器本地日整日保留 90 天，启动时与之后每天清理一次。账本打开失败时写一条 WARN，本次运行不记录用量、不注册用量接口，服务照常运行。

降级拦截率为降级判定结论 `rejected` 的请求占经过判定请求的比例；重复回复率为回复正文与 2 小时内某次回复完全相同的请求占回复不少于 50 token 的请求的比例（与请求日志的重复回复检测一致，过短的固定答复不计入）。用量页的请求记录可以按回复指纹搜索同一回复的全部请求。

```text
WARN  service  请求账本打开失败，本次运行不记录用量 | 路径=<PATH> | 错误=<ERROR>
INFO  service  请求账本已按服务器时区重建本地日汇总 | 天数=<COUNT>
INFO  service  请求账本已加入号池列 | 原始记录=<BOOL> | 重建汇总表=<COUNT>
WARN  service  请求账本写入队列已满 | 丢弃=<COUNT>
ERROR service  请求账本写入失败 | 记录=<COUNT> | 错误=<ERROR>
ERROR service  请求账本清理失败 | 错误=<ERROR>
```

管理页的用量看板通过 `GET /api/usage`、`GET /api/usage/records`、`GET /api/usage/records.csv` 与 `GET /api/requests/{id}/body` 查询，与其他控制面接口一样需要管理令牌。

`REQUEST_BODY_LOG=true` 时，账本同时保存通过 API key 校验的 POST 请求与响应的原始正文（token 计数请求除外），各截断到 64 KiB 并记录原始字节数，只保留最近 1000 条；流式响应保存 SSE 原文。正文包含提示词与模型输出，以明文保存在本机。用量页的请求详情中按请求 ID 查看正文。

## 管理事件流

管理页面连接 `GET /api/events` 时先收到当前状态、模型、账户、最近 200 条日志事件、冷却和活动请求，随后接收增量事件。请求事件在页面按 ID 汇总，控制台通过 Go `slog.JSONHandler` 输出逐行 JSON。

初始事件顺序：

1. `status`
2. `models`
3. `accounts`
4. 最多 2000 条 `log`
5. `cooldowns`
6. 按开始时间排序的活动 `request`

后续增量事件类型为 `status`、`models`、`accounts`、`log`、`cooldowns` 和 `request`。每个 SSE `data` 行是 `{"type":"<TYPE>","data":<DTO>}`，其中 `<DTO>` 是对应类型的事件对象；字段定义见 [protocol.md](protocol.md)。
