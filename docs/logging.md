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
INFO  account@example.com  WAA Worker 就绪 | 页面模型=gemini-flash-latest | PID=18240 | 耗时=8.172s
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
INFO  account@example.com  WAA Worker 就绪 | 页面模型=gemini-flash-latest | PID=18240 | 耗时=10.842s
```

Camoufox 冷启动名额已满时，`1/7` 之前先记录一行 `WAA Worker 启动 | 等待冷启动名额 | 类型=按需|预热 | 占用=N/M`：`M` 为该类型可用的名额，按需启动比预热多 1 个保留名额（见 `WARM_STARTUP_CONCURRENCY`）。预热等待名额超过单个预热任务的时限时按槽位已满处理，不记为账户预热失败。

启动失败记录页面模型、耗时和原始错误。按需容量事件说明当前热池动作：

| 事件 | 语义 |
| --- | --- |
| `WAA Worker 按需扩容 | Worker=N/M` | 活动 Worker 未达 `MAX_ACTIVE_WORKERS`，新 Worker 成功发布 |
| `WAA Worker 按需替换 | Worker=N/M` | `pending Worker`（正在启动的替代 Worker）成功启动并替换一个空闲 Worker |
| `WAA Worker 空闲回收 | Worker=N/M` | 超出热池目标的 Worker 空闲 5 分钟后关闭 |
| `WAA Worker 旧实例停止失败` | 旧实例关闭失败，发布中止并开始关闭 pending Worker |
| `WAA Worker 重建 | 模型=... | 重放当前请求` | 当前账户 Worker 已失效，业务请求在新实例重放；下一行 `原因:` 为触发重建的错误 |
| `WAA Worker 后台恢复放弃 | 等待=<DURATION> | ...` | 后台关闭失败的 Worker 时超过排空超时（总时长上限 + 30 秒，总时长上限为 1 小时与 `REQUEST_TIMEOUT` 中较长者）仍有同账户请求在用，放弃这次关闭、账户恢复接受请求；浏览器已失败的 Worker 由失败 Worker 巡检重新登记，协议 Code 5 的 Worker 在再次失败时重新登记 |
| `WAA Worker 后台恢复出错 | 错误=...` | 后台关闭失败的 Worker 出错（浏览器关不掉时另有 `WAA Worker 停止失败`）；不固定账户的请求已经换号 |
| `常驻 Worker 已失败，后台关闭以便重新预热` | 失败 Worker 巡检登记的后台关闭完成，随后由预热补齐；出错时为 `关闭失败的常驻 Worker 出错 | ...` |
| `WAA Worker 已更新 | 模型=... | 重放当前请求` | 并发路径已经替换 Worker，当前请求使用新实例 |

不固定账户的生成请求没等到失败 Worker 的后台关闭完成就换号时，请求进度写 `WAA Worker 恢复等待 <DURATION> 未完成（同账号仍有进行中的请求），后台继续关闭失败的 Worker，换号重试 | 原因=...`（关闭出错时为 `WAA Worker 恢复时关闭失败的 Worker 出错（<ERROR>），换号重试`，后台放弃时为 `WAA Worker 恢复的后台关闭已放弃，换号重试`），随后照常记录 `账号切换`；换号没有其他可用账户时写 `换号没有可用账号，回到原账号等待 Worker 恢复后重放`，换号次数用完或到连续失败上限时还有没等到关闭就换走的原账户，同样回去等待，写 `换号次数已用完，回到原账号等待 Worker 恢复后重放` 或 `已连续 5 次失败，回到原账号等待 Worker 恢复后重放`。这次失败计入连续失败上限后就到上限的请求不再换号，直接等原账户关闭完成。换号后的选号在排队（其他账户都忙）时原账户关闭完成，写 `换号排队中原账号的 WAA Worker 已关闭完成，原账号重新参与选号`：不再排除原账户，按原来的排队位置重新选号，不固定到原账户（关闭后它没有 Worker，要冷启动并占一个 Worker 槽位），原账户与其他账户哪个先能用就用哪个。回到原账户等待时请求超时或被取消，错误为 `回到原账号等待 <DURATION> 后请求超时（最后原因=WAA Worker 恢复，上次失败=...）: context deadline exceeded`（取消时为 `请求已取消`），对外仍是 504 或取消的官方措辞；经生成服务以首事件前期限结束时，`request.error` 前面另加 `本服务首事件前期限 <DURATION> 到期: `（见[流式等待](#流式等待)）。

按需扩容、按需替换与空闲回收按 Worker 分区计数：上表的 `N/M` 为普通分区的 Worker 数与 `MAX_ACTIVE_WORKERS`；Ultra 分区（Ultra 号池的账户：权益为 Ultra，`ULTRA_INCLUDE_PRO=true` 时也含权益为 Pro 的账户）的同类事件以 `Ultra WAA Worker` 开头，`M` 为 `ULTRA_MAX_ACTIVE_WORKERS`，常驻数为 `ULTRA_WARM_WORKER_LIMIT`。预热暂停原因中的 `Ultra Worker 槽位已满` 表示 Ultra 分区已满；预热不足按分区判断，Ultra 分区不足时记录 `Ultra WAA Worker 预热等待 | Worker=N/M`，`N/M` 为该分区驻留的 Worker 数与预热目标。保存服务配置或启动生成服务后生效的 `ULTRA_EXCLUSIVE` 发生变化时记录 `Ultra 独占设置已更新并立即生效 | 独占=<BOOL>`。服务配置热更新的汇总行 `配置已热更新 | ...` 带 `Pro 并入 Ultra=<BOOL>`，为账户池当前生效的 `ULTRA_INCLUDE_PRO`；`单账户并发=<N>` 之后带 `单账户每分钟上限=<N> 次`，为保存的 `ACCOUNT_RPM_LIMIT`，`0` 时写 `单账户每分钟上限=关闭`。

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

## 本机代理中转

带账号密码的代理由本机中转交给 Camoufox（见 [开发文档](development.md#本机代理中转)），中转日志使用 `service` 来源，setup 进程写到标准错误。`上游` 只写 `scheme://host:port`，不写用户名和密码；`本机` 是 `127.0.0.1:N`；`使用者` 是租约的使用方（Worker 为账户 ID，登录与验证为 `登录:<账户>`、`验证:<账户>`，新账户为 `登录:新账户`，测试代理为 `代理测试`），每个监听器只有一个使用者。`原因` 与 `错误` 字段先经过 `proxyrelay.Scrub` 去掉账号密码。不带账号密码的代理不经过中转，没有这些日志。

```text
INFO  service  本机代理中转启动 | 上游=http://1.2.3.4:8080 | 本机=127.0.0.1:41234 | 使用者=a@example.com
INFO  service  本机代理中转关闭 | 上游=http://1.2.3.4:8080 | 本机=127.0.0.1:41234 | 使用者=a@example.com | 释放后=5m12s
WARN  service  本机代理中转在租约释放后仍有新连接 | 本机=127.0.0.1:41234 | 使用者=a@example.com | 释放后=2m3s | 浏览器可能没有退出，中转继续保留
WARN  service  上游代理认证失败 | 上游=http://1.2.3.4:8080 | 本机=127.0.0.1:41234 | 使用者=a@example.com | 已向浏览器返回 502，请检查代理用户名和密码
WARN  service  上游代理拒绝隧道 | 上游=http://1.2.3.4:8080 | 本机=127.0.0.1:41234 | 状态=403 Forbidden | 目标=aistudio.google.com:443 | 使用者=a@example.com
WARN  service  上游代理连接失败 | 上游=http://1.2.3.4:8080 | 本机=127.0.0.1:41234 | 原因=连接被拒绝 | 使用者=a@example.com
WARN  service  本机代理中转拒绝请求 | 本机=127.0.0.1:41234 | 原因=不是 http:// 绝对地址的请求
WARN  service  本机代理中转连接数已满 | 本机=127.0.0.1:41234 | 范围=单个浏览器 | 上限=1024
WARN  service  本机代理中转连接数接近上限 | 进行中=13108 | 上限=16384
WARN  service  本机代理中转接受连接失败 | 本机=127.0.0.1:41234 | 错误=accept tcp 127.0.0.1:41234: accept4: too many open files
```

| 日志 | 含义 |
| --- | --- |
| `本机代理中转启动` / `本机代理中转关闭` | 一个租约的监听器起停。租约释放后，监听器在没有进行中的连接、且连续 5 分钟没有新连接时关闭，`释放后` 为从释放到关闭的时长 |
| `本机代理中转在租约释放后仍有新连接` | 租约释放后浏览器仍在发连接，说明浏览器可能没有退出（Close 失败或启动中途放弃），中转继续服务；每个监听器只记一次 |
| `上游代理认证失败` | HTTP 上游回 407（中转向浏览器返回 502），或 SOCKS5 上游拒绝账号密码、不接受账号密码认证（末尾写 `已向浏览器返回 SOCKS 失败`）。同时记入失败记录，Worker 启动、登录与验证失败时附在错误后面：`…；本机代理中转：上游代理拒绝了账号密码（407），请检查用户名和密码` |
| `上游代理拒绝隧道` | 上游对某个 CONNECT 回了非 200（`状态` 为状态码与原因短语），或上游 SOCKS5 连目标失败（`状态` 为 `SOCKS 05 拒绝连接` 这类回复码说明） |
| `上游代理连接失败` | 连不上上游代理本身：`原因` 为 `连接被拒绝`、`主机名无法解析`、`超时`、`TLS 握手失败` 或 `响应无法解析` |
| `本机代理中转拒绝请求` | 收到 origin-form（`/path`）、`OPTIONS *` 或 `https://` 绝对地址的明文请求，回 400；多半是本机其他程序误连了这个端口 |
| `本机代理中转连接数已满` | 单个浏览器（1024 条）或整个进程（按 `RLIMIT_NOFILE` 推算，Windows 16384）的连接数达到上限，HTTP 中转回 503、SOCKS5 中转回失败，不等待 |
| `本机代理中转连接数接近上限` | 整个进程的连接数达到上限的 80% |
| `本机代理中转接受连接失败` | `Accept` 出错（例如 fd 耗尽），从 5 毫秒起翻倍退避、最长 1 秒，不退出 |

**节流**：凭据错误时浏览器的每个资源请求都会失败，因此 WARN 按（监听器，日志名）节流，进程级的两条按（进程，日志名）节流：同一个键 60 秒内只记第一条，其余计数；窗口结束后再出现时，这一条末尾加 `| 60 秒内另有 N 次`。`本机代理中转启动`、`本机代理中转关闭` 与 `本机代理中转在租约释放后仍有新连接` 不节流（后者每个监听器本来只记一次）。

**代理测试**：管理页“测试代理”（`POST /api/proxy/test`）每次记一条，两条路径都成功为 INFO，否则为 WARN；`代理` 只写去掉账号密码的 `scheme://host:port`，失败说明同样不含账号密码：

```text
INFO  service  代理测试 | 代理=http://1.2.3.4:8080 | 认证=是 | 浏览器路径=203.0.113.7（812ms） | 后台请求路径=203.0.113.7（640ms）
WARN  service  代理测试 | 代理=http://1.2.3.4:8080 | 认证=是 | 浏览器路径=203.0.113.7（812ms） | 后台请求路径=失败：上游代理拒绝了账号密码（407），请检查用户名和密码
```

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
| `原因` | 当前在等什么：`账号全部忙碌`、`没有空闲的 Worker 槽位`、`Worker 正在启动`、`Worker 正在扩容`、`Worker 容量已满`、`账号忙碌`、`账号冷却中` |
| `已等待` | 排队的时间：每次选号从第一次等待到取得账号或选号结束按墙钟计，换号重试接着累加；两次选号之间上游处理、退避与 Worker 恢复的时间不算 |
| `尝试` | 第几次选号（换号重试递增） |
| `同条件队列` | 在选择条件相同的请求（模型、通道限定、号池等）中排第几、共几个 |
| `热空闲`、`热忙碌`、`备用`、`备用忙碌` | 本轮复查时该模型的候选数：已有 Worker 的账号与备用账号，冷却中的不计 |
| `每分钟已满` | 其中因最近 60 秒的生成请求已达 `ACCOUNT_RPM_LIMIT` 而按忙碌分组的候选数（已计入 `热忙碌` 与 `备用忙碌`）；为 0 时不写。在排除本请求已失败的 Worker 之前取，只作提示。满窗不算冷却，不新增等待原因：热账号都满时原因照常为 `账号全部忙碌` 等 |
| `启动中`、`最早恢复` | 有候选的 Worker 正在启动；候选中最早恢复的冷却还剩多久 |

请求在排队中超时或被取消时，错误写明最后的等待原因与已等待时间（同样只算排队的时间，上一次尝试在上游处理的时间不算），例如 `排队 5m0.004s 后请求超时（最后原因=账号全部忙碌）: context deadline exceeded`。它随 `request.finished` 的 `request.error` 写进请求日志与用量账本，对外仍是 504 或取消的官方措辞。生成请求以首事件前期限结束时前面另加 `本服务首事件前期限 <DURATION> 到期: `，例如 `本服务首事件前期限 5m0s 到期: 排队 4m59.9s 后请求超时（最后原因=账号全部忙碌）: context deadline exceeded`（见[流式等待](#流式等待)）。

```text
INFO  service  排队等待 | 原因=账号全部忙碌 | 已等待=2.004s | 尝试=1 | 模型=gemini-3.7-flash | 同条件队列=3/57 | 热空闲=0 | 热忙碌=40 | 备用=0 | 备用忙碌=0
WARN  service  排队等待 | 原因=账号冷却中 | 已等待=17.012s | 尝试=2 | 模型=gemini-3.7-flash | 同条件队列=1/12 | 热空闲=0 | 热忙碌=0 | 备用=0 | 备用忙碌=0 | 最早恢复=41s
INFO  service  排队等待 | 原因=账号全部忙碌 | 已等待=2.001s | 尝试=1 | 模型=gemini-3.7-flash | 同条件队列=1/4 | 热空闲=0 | 热忙碌=10 | 备用=0 | 备用忙碌=0 | 每分钟已满=6
WARN  service  排队进度日志已省略 128 条（全局限流：每秒最多 2 条）
```

主用模型排队超过排队上限时，请求时间线在 `service` 名下写一条 WARN，随后请求以 429 结束：

- `主用模型排队超过 <上限>，立即返回 429 | 原因=<原因> | 已排队=<DURATION> | 总耗时=<DURATION> | 尝试=<N> | 模型=<MODEL>[ | 同条件队列=<i>/<n>] | 热空闲=<N> | 热忙碌=<N> | 备用=<N> | 备用忙碌=<N>[ | 每分钟已满=<N>][ | 启动中=是][ | 最早恢复=<DURATION>][ | 驻留冷却=<N>]`
- 排队上限为 0 时为 `主用模型不排队，立即返回 429 | 原因=<原因> | 总耗时=<DURATION> | 尝试=<N>`，后面接同样的快照（从 `模型=` 起）。

`原因` 为最后一次等待的原因；`已排队` 为计入排队上限的时间（整个请求共用一份预算，不含请求自己现场冷启动与候选冷却的等待），`总耗时` 为从第一次选号起的墙钟时间（含失败的尝试与换号退避），`尝试` 为第几次选号。从 `模型=` 起是超限这一刻的候选快照，`同条件队列` 到 `最早恢复` 与上表相同；`驻留冷却` 取自主用模型供给（`/api/status` 的 `primary_supply[].cooling`，约 30 秒统计一次的缓存）里该模型在请求号池对应分区的条目：不在排空中、是该模型的候选、但所有支持的启用通道都在冷却的驻留 Worker 数（含剩余不到 2 分钟的短冷却），缓存里没有对应条目时不写。快照只写进这一行，不进 `request.error` 与用量账本，那里仍是 `号池繁忙：模型 <MODEL> 暂时没有空闲的账号或 Worker，请稍后重试（[Ultra 号池，]已排队 <DURATION>，上限 <DURATION>[，总耗时 <DURATION>[（含现场冷启动 <DURATION>）]][，第 <N> 次尝试]，最后等待原因：<原因>）`。`驻留冷却` 不小于 `热空闲` 加 `热忙碌` 时，驻留 Worker 大多对该模型在冷却（见[额度冷却日志](#额度冷却日志)的连续限流）；`bash tools/diag_busy.sh` 按这些快照归因并汇总。

```text
WARN  service  主用模型排队超过 10s，立即返回 429 | 原因=账号全部忙碌 | 已排队=10.204s | 总耗时=25.311s | 尝试=3 | 模型=<MODEL> | 同条件队列=2/9 | 热空闲=0 | 热忙碌=2 | 备用=0 | 备用忙碌=0 | 最早恢复=4m10s | 驻留冷却=6
WARN  service  主用模型不排队，立即返回 429 | 原因=没有空闲的 Worker 槽位 | 总耗时=1.2s | 尝试=1 | 模型=<MODEL> | 热空闲=0 | 热忙碌=4 | 备用=3 | 备用忙碌=0 | 每分钟已满=4 | 驻留冷却=1
```

## Ultra 主用模型额度不足

按请求的号池命中 `ULTRA_PRIMARY_MODELS`（默认 `gemini-4-argon`）的请求没有额度时立即返回 429，不在服务里一直换号重试，请求时间线写下面几种进度（普通号池的 `PRIMARY_MODELS` 与其他模型不写）：

| 进度 | 级别 | 何时写 |
| --- | --- | --- |
| `Ultra 主用模型候选全部冷却，立即返回 429` | WARN | 选号时候选全部冷却（还没有遇到额度 429）：不再等待 1 分钟内恢复的冷却，立即按全部冷却返回；`最早恢复` 为候选中最早恢复的冷却还剩多久，也是 `Retry-After` |
| `额度 429，换号重试（只用已启动的 Worker）` | INFO | 遇到每日类额度失败（每日限额，剩余不少于 15 分钟的冷却，或租到的账号在请求发出前被这样的长冷却拦下）后换号：`额度失败` 为本请求遇到每日类额度失败的账号数与上限（最多再换 2 次账号；同一账号另一通道的探测再遇到每日类不另计），`类型` 为冷却类型（拦在发出前时为 `发出前已冷却`），`原因` 为这次的上游错误；紧接着的这次换号只用已启动的 Worker，不现场冷启动备用账号（之后再因额度以外的原因换号时按普通规则选号） |
| `限流 429，换号重试（只用已启动的 Worker）` | INFO | 遇到限流类额度失败（分钟限额、没有额度信息的限流）后换号：`限流失败` 为本请求的限流类失败次数与上限（最多再换 5 次账号），`类型` 为 `分钟限额` 或 `限流`，`额度失败` 为每日类的账号数与上限；其余同上。租到的账号在请求发出前被短冷却拦下时两种进度都不写，照常换号 |
| `额度 429，同账号换通道重试`／`限流 429，同账号换通道重试` | INFO | 额度冷却只写在本通道、同一账号的另一通道仍可用时（例如每日额度通道判定还在观察中、判定共用后定期复核放行另一通道，或两个通道判定为独立额度）不换号，在同账号另一通道再试；字段与对应的“换号重试”相同。这次不换号，所以不带“只用已启动的 Worker”。每日类同一账号只计一次：另一通道的探测再遇到每日限额时 `额度失败` 不变，紧接着的换号进度与它是同一个 N/2；限流类照常计一次 |
| `Ultra 主用模型额度不足，立即返回 429` | WARN | 请求以额度不足结束：某一类额度失败达到它的换号上限、其余候选全部冷却、额度 429 后没有已启动的 Worker（这时分区还能扩容就在后台为一个备用账号启动 Worker，不等它），或没有其他可换的账号 |

最后一行的字段：

| 字段 | 含义 |
| --- | --- |
| `原因` | `额度失败已达换号上限`（每日类）、`限流失败已达换号上限`（限流类）、`其余候选全部冷却`、`额度 429 后没有已启动的 Worker` 或 `没有其他可换的账号`（额度失败后号池里能服务该模型的账号都已试过，或剩下的暂时不可调度，例如需要重新登录；换号次数用完时也是这个原因，账号少的 Ultra 号池常见） |
| `额度失败` | 本请求遇到每日类额度失败的账号数（同一账号两个通道都失败、或发出前又被长冷却拦下只计一次；输入超过账号上限的 429 不算） |
| `限流失败` | 本请求因限流类额度失败（分钟限额、限流）的尝试次数；为 0 时不写 |
| `总耗时` | 从第一次选号到返回 429 的墙钟时间 |
| `建议重试` | 写进 `Retry-After` 的时间：其余候选全部冷却时为最早恢复；每日类达到换号上限或没有其他可换的账号时为 5 秒与本请求遇到的最早额度恢复中较早者；限流类达到换号上限时为本请求遇到的限流冷却中最早的恢复，不超过 1 分钟，不知道时 5 秒；没有已启动的 Worker 时为 5 秒；都至少 1 秒。响应头里的 `Retry-After` 另随机往后加 0 到 3 秒（建议间隔不到 5 秒不加），这里不含 |

额度失败后换号的进度，以及尝试结束时就判定额度不足的（达到换号上限、换号次数用完）写在这次尝试的账号名下；选号时判定的（全部冷却、没有已启动的 Worker、没有其他可换的账号）写在 `service` 名下。请求以 HTTP 429 结束，`request.error` 为中文说明（号池、模型、原因、额度失败次数，有限流失败时另写 `限流失败 N 次`，以及最早恢复），对外只返回固定的限流文案 `Resource has been exhausted (e.g. check quota).`，不含上游原文。

```text
WARN  service  Ultra 主用模型候选全部冷却，立即返回 429 | 最早恢复=42s
INFO  account@example.com  额度 429，换号重试（只用已启动的 Worker）| 额度失败=1/2 | 类型=每日限额 | 原因=AI Studio GenerateContent 返回 HTTP 429、协议错误码 8: …
INFO  account2@example.com  限流 429，换号重试（只用已启动的 Worker）| 限流失败=1/5 | 类型=分钟限额 | 额度失败=1/2 | 原因=AI Studio GenerateContent 返回 HTTP 429、协议错误码 8: …
INFO  account3@example.com  额度 429，同账号换通道重试 | 额度失败=1/2 | 类型=每日限额 | 原因=AI Studio GenerateContent 返回 HTTP 429、协议错误码 8: You've reached your usage limit. …
WARN  account@example.com  Ultra 主用模型额度不足，立即返回 429 | 原因=额度失败已达换号上限 | 额度失败=3 | 总耗时=6.214s | 建议重试=5s
WARN  account6@example.com  Ultra 主用模型额度不足，立即返回 429 | 原因=限流失败已达换号上限 | 额度失败=0 | 限流失败=6 | 总耗时=3.871s | 建议重试=49s
WARN  service  Ultra 主用模型额度不足，立即返回 429 | 原因=额度 429 后没有已启动的 Worker | 额度失败=1 | 总耗时=1.032s | 建议重试=5s
```

## 流式等待

连续 15 秒没有阶段进展时会写入诊断日志。请求在收到上游终态、客户端取消，或本服务的期限到期时结束：首个上游事件之前为首事件前期限（`REQUEST_TIMEOUT`，含排队、冷启动与首事件前的换号），开始输出后为首事件后空闲期限（上游连续 `REQUEST_TIMEOUT` 既没有事件也没有网络字节）与总时长上限（1 小时与 `REQUEST_TIMEOUT` 中较长者，从请求开始算），见[长生成与中转重试](development.md#长生成与中转重试)。设置了 `FIRST_EVENT_TIMEOUT` 时，一次尝试从向上游发送起超过该时长仍没有上游事件，写入"上游首事件超时"并换号重试。

| 事件 | 当前停留位置 | 重点字段 |
| --- | --- | --- |
| `请求准备等待` | 账户已选定，正在准备 WAA proof 或等待 GenerateContent 响应头 | `当前`、`模型` |
| `请求准备结束` | 准备阶段超过 15 秒后开始接收响应体 | `等待`、`WAA`、`响应头`、`模型` |
| `上游首事件等待` | 上游响应体已经建立，尚未解出语义事件 | `网络字节`、`最近网络` |
| `上游首事件到达` | 首事件等待后解出第一个语义事件 | `等待`、`事件`、`模型` |
| `事件流停顿` | 已有语义事件，连续 15 秒没有下一事件 | `最近事件`、`推理`、`正文`、`网络字节`、`最近网络` |
| `事件流恢复` | 停顿后解出下一语义事件 | `停顿`、`当前事件` |
| `账号切换` | 当前账户在首个上游语义事件前失败 | 当前账户来源、模型和下一行原始原因 |
| `本服务期限截断` | 本服务自己的期限到期（WARN）：首事件前写在 `service` 名下，首事件后写在账户名下 | `阶段`、`期限`、`已用时`，首事件前另有 `停在`、`原因`，首事件后另有 `最近事件`、`推理`、`正文`、`网络字节`、`最近网络` |
| `上游首事件到达时请求已结束，不转发` | 首个上游事件与首事件前期限到期（或请求取消）同时到达：这个事件丢弃，没有向客户端输出任何内容（WARN） | `原因` |
| `终态后上游事件流 <DURATION> 内没有结束，结束转发` | 终态已经交给客户端，上游事件流在空闲期限内仍不关闭：只结束上游与租约，请求仍按终态记录（WARN） | `模型`、`网络字节`、`最近网络` |

`账号切换` 的原因以 `账户已进入冷却` 开头时（如 `账户已进入冷却（分钟限额，恢复时间 <RFC3339>，请求没有发到上游）`），这次请求在发往上游前被租约账号的冷却拦下（多半是同账号别的请求刚遇到额度 429）：任何模型都按已冷却换号，不计入连续失败上限（`已连续 5 次失败，不再换号`）、不退避，不再是 500。普通模型（以及 Ultra 主用模型被剩余不到 15 分钟的冷却拦下、本请求此前也没有额度失败时）不能再换号就以 429 结束，`Retry-After` 为拦下它的冷却的恢复时间。Ultra 主用模型被剩余不少于 15 分钟的冷却拦下时记一次每日类额度失败（进度 `额度 429，换号重试` 的类型为 `发出前已冷却`），超过换号上限或没有其他可换的账号时以 `Ultra 主用模型额度不足` 结束（即使还有其他账号，这样拦下 3 次后也不再换号），`Retry-After` 见上一节的 `建议重试`。

本服务期限截断（`本服务期限截断 | …`，WARN）的两种格式：

- 首事件前：`本服务期限截断 | 阶段=首事件前 | 期限=<DURATION> | 已用时=<DURATION> | 停在=<位置> | 原因=<原因>`，写在 `service` 名下。`停在` 为期限在某次尝试中到期时那次尝试停在哪一步：`WAA proof`、`等待上游响应头`、`等待上游首事件`、`文件复制或附件上传`、`首事件与期限同时到达（已丢弃，未向客户端输出）`；不在尝试中（排队、回到原账号等待时，或换号退避中）到期为 `-`。`原因` 为加前缀之前的错误。期限在换号退避中到期时按上一次尝试的错误区分：上一次的错误满足 `context.DeadlineExceeded`（首事件超时、`BiDi … 等待回复超时`、`协议位置 …: context deadline exceeded` 这类读取超时等）时照样加前缀、写这一行，`停在` 为 `-`，`原因` 为那次的错误；上一次是上游返回的 RPC 错误（如上游 5xx、协议错误码，不满足 `context.DeadlineExceeded`）时保留那次的上游错误与状态码，不加前缀，也不写这一行。文件资源刷新（引用的文件在请求开始时刷新）时到期的不写这一行，`request.error` 为 `本服务首事件前期限 <DURATION> 到期（停在 文件资源刷新）: …`。
- 首事件后：`本服务期限截断 | 阶段=首事件后空闲 | 期限=<DURATION> | 已用时=<DURATION> | 最近事件=<KIND> | 推理=<N> | 正文=<N> | 网络字节=<N> | 最近网络=<DURATION>`，写在账户名下；总时长上限到期时 `阶段=总时长上限`、`期限` 为总时长上限；还没有收到网络字节时末尾只有 `网络字节=0`。`已用时` 从请求开始算，`推理`、`正文` 为首事件后收到的推理与正文事件数（模型名带 `-nothinking` 时推理事件在协议层丢弃，同样计数；Anthropic `thinking.type=disabled`、Gemini `includeThoughts=false` 时推理事件在本服务调用上游的一层就丢弃（Build 通道上游不返回），不计入 `推理`，Playground 通道的思考字节仍计入 `网络字节` 与 `最近网络`）。

请求随后以 `request.finished` 结束，`request.error` 分别为：

```text
本服务首事件前期限 5m0s 到期（停在 等待上游首事件）: context deadline exceeded
本服务首事件前期限 5m0s 到期: 排队 4m59.9s 后请求超时（最后原因=账号全部忙碌）: context deadline exceeded
本服务首事件后空闲期限 5m0s 到期（已输出 推理=812 正文=40）: context deadline exceeded
本服务总时长上限 1h0m0s 到期（已输出 推理=812 正文=4033）: context deadline exceeded
```

三种错误都只解包为 `context.DeadlineExceeded`：HTTP 状态记 504、状态为 `failed`，客户端收到的仍是 504 官方原句 `Deadline expired before operation could complete.`，不含“本服务”字样。上游自己的期限是 `AI Studio <方法> 返回 HTTP 504、协议错误码 4: …`，上游断流是 `AI Studio stream closed before finish`，两者都不带这个前缀。排查记录（`/trace/`）的 `notes` 另记 `本服务首事件前期限到期: <错误>` 或 `本服务期限截断: <错误>`。

截断或丢弃首事件之后，服务读完这次尝试的事件流、确认上游 goroutine 退出后再释放租约；5 秒内没有结束时另写一条 WARN：首事件前为 `首事件到达时请求已结束，上游事件流未及时结束`，首事件后为 `本服务期限截断后上游事件流未及时结束`。

```text
WARN  service  本服务期限截断 | 阶段=首事件前 | 期限=5m0s | 已用时=5m0.001s | 停在=等待上游首事件 | 原因=context deadline exceeded
WARN  service  本服务期限截断 | 阶段=首事件前 | 期限=5m0s | 已用时=5m0s | 停在=- | 原因=排队 4m59.9s 后请求超时（最后原因=账号全部忙碌）: context deadline exceeded
WARN  account@example.com  本服务期限截断 | 阶段=首事件后空闲 | 期限=5m0s | 已用时=9m12.408s | 最近事件=reasoning | 推理=812 | 正文=40 | 网络字节=1832210 | 最近网络=5m0.002s
WARN  account@example.com  本服务期限截断 | 阶段=总时长上限 | 期限=1h0m0s | 已用时=1h0m0s | 最近事件=text | 推理=812 | 正文=4033 | 网络字节=9921034 | 最近网络=180ms
WARN  account@example.com  上游首事件到达时请求已结束，不转发 | 原因=context deadline exceeded
WARN  account@example.com  终态后上游事件流 5m0s 内没有结束，结束转发 | 模型=gemini-3.7-flash | 网络字节=20480 | 最近网络=5m0.004s
```

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
| `批量删除账户` | 批量删除接口（403 无权限账户的“一键删除”）一次请求的汇总，每个删除的账户另写一条 `账户已删除` |
| `账户模型目录同步完成` | 新目录已写入当前模型目录和账户调度状态 |
| `账户模型目录等待重试` | 当前同步失败，运行期重试继续处理 |

批量删除接口每次请求结束写一行 `批量删除账户 | 删除=<N> | 不存在=<N> | 失败=<N> | 剩余=<N>`：`删除` 为本次删除的账户数，`不存在` 为号池里已经没有的账户数（只清除它的 403 记录），`失败` 为没有删掉的账户数（多半是一直在处理请求、等不到空闲，即 `account_busy`），`剩余` 为号池里还有、超过单次上限 500 本次没有处理的账户数（单次上限只计号池里还有的账户，已经没有的计入 `不存在`，不占名额，也不计入 `剩余`）。

资源与 `scope`（状态记录范围）事件：

| 事件 | 语义 |
| --- | --- |
| `文件引用复制` | Drive file 已临时复制到本次目标账户 |
| `内联附件处理完成` | 本次请求的附件数量、原始字节数与处理耗时 |
| `临时文件清理失败` | 本次请求的临时 Drive 文件回收失败，原始错误保留 |
| `转录账号切换` | 转录生成阶段在首个结果前切换候选 |
| `Bidi 账号切换` | Live/Robotics setup 阶段切换候选 |

较晚返回的认证结果只有在 `authGeneration` 和 `checkedAt` 均匹配当前账户时才应用；较晚返回的模型成功或冷却结果只有在 `modelAccessGeneration` 和 `checked_at` 均匹配当前模型目录时才应用。日志记录实际应用到当前状态的变更和持久化错误。

### 额度冷却日志

生成请求遇到额度 429 时按冷却类型写入冷却，并在执行账号名下记一行：

- 写入时 WARN `账号冷却 | 类型=<类型> | 范围=<范围> | 恢复=<RFC3339>[ | 上限=<N>][ | 指标=<METRIC>][ | 模型=<QUOTA_MODEL>][ | 详情类型=<TYPES>][ | 已输出内容后]`。
  - `类型` 为 `每日限额`、`分钟限额` 或 `限流`（没有额度周期信息的 429：上游给出恢复时间时冷却到那时，账号全局的冷却 30 秒，文件复制、附件上传返回的只冷却本通道 30 秒，其余按连续次数逐级冷却，见下）。只按这次尝试本身的错误判断：生成失败后删除临时 Drive 副本返回的 429、403 照旧并入请求的错误，但不写冷却。
  - `范围` 为 `全局`（账号全局 `*` 冷却，例如按账号计的分钟限额 `GeneratePerMinutePerProjectPerUser`，冻结这个账号的所有模型与两个通道到窗口结束）、模型 ID（Playground）或 `build:<模型>`。
  - 恢复时间依次取上游 RetryInfo、错误文案里的 `retry in <时长>`（如 `Please retry in 16h6m4.716518794s.`，超过 24 小时按 24 小时）与 HTTP `Retry-After` 中第一个有的；每日限额取它与太平洋时间次日零点中较晚的一个，分钟限额与 `限流` 以它为准。冷却到太平洋时间次日零点时（每日限额没有更晚的 `retry in`），`恢复` 按太平洋时区输出，例如 `2026-10-23T00:00:00-07:00`（冬令时为 `-08:00`）。
  - 三种常见额度 429 的识别结果：free 账号的每日限额（文案 `You exceeded your current quota …`，详情只有 GeminiApiErrorDetails，文案带 `Please retry in <时长>`）记 `类型=每日限额`、范围为请求的模型，`上限=10 | 指标=generate_requests_per_model_per_user | 模型=gemini-3.1-pro`，`恢复` 为太平洋时间次日零点与 `retry in` 中较晚的一个；pro/ultra 账号的每日限额（文案 `You've reached your usage limit …`，详情同上、没有恢复时间）记 `类型=每日限额`、`上限=50`，冷却到太平洋时间次日零点；pro/ultra 账号每分钟 12 次的限额（ErrorInfo `GeneratePerMinutePerProjectPerUser`）记 `类型=分钟限额 | 范围=全局 | … | 上限=12 | 指标=generate_requests_global`。每日限额按模型冷却，再按两个通道的共用判定决定是否同步冷却另一通道（见上文账户事件里的 `每日额度通道判定`：已判定共用时同步冷却，观察中时同账号另一通道先再试一次）。
  - `上限` 为上游给出的限额值：ErrorInfo 的 `quota_limit_value`（如 `12`），或 QuotaFailure、GeminiApiErrorDetails 违规项的 `quota_value`（如 `10`、`50`）；`指标` 为 `quota_metric` 去掉服务名前缀（`alkalimakersuite-pa.googleapis.com/generate_requests_global` 记为 `generate_requests_global`）；`模型` 为违规项维度里的 `model`，即上游计额度用的模型（如 `gemini-3.1-pro`，可能与请求的模型名如 `gemini-3.1-pro-preview` 不同）。上游没给时不写，三者只用来核实各档位、各模型的真实上限。
  - `详情类型` 只在没有 `指标` 时写：状态 details 里出现过的全部类型（去掉 `type.googleapis.com/` 前缀，按出现顺序去重，逗号分隔，含没有解析的类型），用来核实裸 429 带了哪些详情；状态没有 details 时不写。
  - `已输出内容后` 表示流已经输出内容后才遇到这次 429。
  - 每日限额且该模型已判定两个通道共用额度时，另一通道的同步冷却另记 `账号冷却 | 类型=每日限额 | 范围=<另一通道> | 恢复=<RFC3339> | 与另一通道共用额度，同步冷却`，不带上限、指标与模型。
- 没有写入时 INFO `账号冷却未写入 | 类型=<类型> | 范围=<范围> | 恢复=<RFC3339>[ | 上限=<N>][ | 指标=<METRIC>][ | 模型=<QUOTA_MODEL>][ | 详情类型=<TYPES>] | 原因=<原因>[ | 已输出内容后]`。`原因` 为 `同账号取得租约更晚的请求已写回结果`（该作用域已有不早于本次租约的记录，例如同账号更晚取得租约的请求已经成功或已写入冷却）、`模型目录已更新，旧租约的结果不再写回`，或 `已有更晚的冷却，保留原冷却`（该作用域已有仍生效且更晚的冷却，例如同账号并发请求刚写入的逐级冷却：不缩短，只推进检查时间，这时 `恢复` 为保留的冷却的恢复时间）。这时不登记淘汰、不做通道共用额度的学习，请求照常按已冷却换号。
- 生成请求本身返回的、没有额度周期信息、上游也没给恢复时间的非全局限流 429（裸 `Quota exceeded` 等）按连续次数逐级冷却，这一行在上限、指标、模型、详情类型之后另带档位：WARN `账号冷却 | 类型=限流 | 范围=<模型或 build:模型> | 恢复=<RFC3339>[ | 上限=<N>][ | 指标=<METRIC>][ | 模型=<QUOTA_MODEL>][ | 详情类型=<TYPES>] | 连续限流=第 <N> 档[ | 同步=两个通道][ | 号池级限流不升档][ | 已输出内容后]`。
  - `连续限流` 为写入后的档位，第 1 档也写：第 1～4 档冷却 30 秒、5 分钟、30 分钟、2 小时，第 5 档冷却到太平洋时间次日零点，每档都不超过次日零点。同一账号同一模型（两个通道共用时共用一份）冷却到期后再次失败才升一档；冷却写入前就已发出的请求、同一请求换通道再试，以及带 `号池级限流不升档` 或 `已输出内容后` 的这一次不升档，冷却续到本档到期与第 1 档时长中较晚的一个。
  - `恢复` 为本通道实际生效的恢复时间：该键已有更晚的冷却时保留它，不缩短。
  - `同步=两个通道` 表示另一个启用通道同步冷却（该通道已有更晚的冷却时保留它），不另写一行：两个通道没有判定为独立额度（每日额度通道判定为共用、观察中或没有记录）时共用一份档位；另一通道已有不早于本次租约的记录，或另一通道没有启用时不带这个字段。
  - `号池级限流不升档` 表示这次判定为号池级限流（见下），档位不变；`已输出内容后` 表示流已经输出内容后才遇到这次 429，同样不升档。
  - 没有写入时照常记 INFO `账号冷却未写入 | 类型=限流 | 范围=<范围> | 恢复=<RFC3339>[ | 上限=<N>][ | 指标=<METRIC>][ | 模型=<QUOTA_MODEL>][ | 详情类型=<TYPES>] | 原因=<原因>[ | 已输出内容后]`，`恢复` 为第 1 档的恢复时间，档位不变。
- 档位清零：该账号在某个通道上生成成功（收到结束事件）、这次成功能清的档位此前到第 2 档以上时，在账号名下记 INFO `限流档位清零 | 范围=<模型或 build:模型> | 此前连续=第 <N> 档`，`范围` 为成功的通道（Build 为 `build:<模型>`），`此前连续` 为被清档位中的最高档。清两个通道共用的一份与这个通道自己的一份，只在这次成功的租约晚于档位最近一次失败的租约时清；从第 1 档清零不写，跨过太平洋时间零点作废的档位不写，重启后档位从第 1 档起（档位只在内存中）也不写。
- 号池级限流：同一号池 10 分钟内至少 5 个不同账号对同一模型逐级限流、这段时间该模型在号池里又没有任何成功时，`service` 记 WARN `号池级限流 | 模型=<MODEL>[ | 号池=<号池>] | 号池内 10 分钟内至少 5 个账号返回限流且没有成功，不升级冷却`，同一号池同一模型每 10 分钟最多一条；判定成立期间的冷却都带 `号池级限流不升档`。`号池` 为请求的号池：`/ultra` 请求为 `Ultra 号池`，普通路径在 `ULTRA_EXCLUSIVE=true` 时为 `普通号池`，不限号池（`ULTRA_EXCLUSIVE=false`）时不写。号池内该模型有一次请求成功即清空判定。
- 额度 429 详情未识别：额度 429 带着 GeminiApiErrorDetails（AI Studio 网页后端的私有详情）、冷却日志却没有 `指标`（带 `详情类型=`）时（布局与已知的每日限额样本不同：没有从中解析出违规项，或违规项只有限额名、维度而没有指标；冷却只能按文案判断，裸 `Quota exceeded` 按裸限流逐级兜底），在上面的冷却日志（写入或未写入）之后于执行账号名下记 INFO `额度 429 详情未识别 | 类型=<TYPE> | 文案=<MESSAGE> | 原文=<RAW> | 请把这一行发给开发者`（进入独立的小队列、优先写出，普通日志占满控制台队列时也不丢：这时它在文件里可能先于还在队列里的冷却日志出现，按 `time` 字段仍在其后；前面那条冷却日志本身也可能已被丢弃，只剩这一行）。日志里出现这一行时请把整行发给开发者，据此扩展解析；`bash tools/diag_busy.sh` 在第 3 段列出窗口内的这些行。
  - `类型` 为这个详情的类型（去掉 `type.googleapis.com/` 前缀）；`文案` 为上游错误文案，压成一行（连续空白合成一个空格），超过 200 字截断并加 `…`；`原文` 为该详情值的原样 JSON（压缩掉空白，不改内容，来自上游），超过 1200 字节按 UTF-8 边界截断并注明 `…（已截断，共 <N> 字节）`。同一个 429 带多个这样的详情时写第一个没有解析出违规项的，都解析出违规项（没有指标）时写第一个；任何详情（含 ErrorInfo、QuotaFailure）解析出指标时不写。
  - 同一种详情（`类型` 加 `文案`，文案里的连续数字不计，只差 project_number、retry 时长等数字的算同一种，日志照原样写文案）10 分钟内最多一条，不论来自哪个账号；最多同时记 64 种，满时先清掉过期的，仍满时这一种先不写、不挤掉其他种；此后 10 分钟内已记下的种类过期后再来也不写、不续期，把名额留给这一种，所以它隔不到 10 分钟又来时，等最早的一种过期（最晚 10 分钟）后再来就写（同时被拒的不止一种时按到来先后），任意 10 分钟最多 64 行。文件复制、附件上传阶段与已输出内容后的额度 429 同样写。这一行不影响冷却的分类与档位。

```text
WARN  account@example.com  账号冷却 | 类型=分钟限额 | 范围=全局 | 恢复=2026-10-21T21:03:19+08:00 | 上限=12 | 指标=generate_requests_global
INFO  account@example.com  账号冷却未写入 | 类型=分钟限额 | 范围=全局 | 恢复=2026-10-21T21:03:19+08:00 | 上限=12 | 指标=generate_requests_global | 原因=同账号取得租约更晚的请求已写回结果
WARN  free@example.com  账号冷却 | 类型=每日限额 | 范围=gemini-3.1-pro-preview | 恢复=2026-10-23T16:05:04+08:00 | 上限=10 | 指标=generate_requests_per_model_per_user | 模型=gemini-3.1-pro
WARN  pro@example.com  账号冷却 | 类型=每日限额 | 范围=gemini-3.1-pro-preview | 恢复=2026-10-23T00:00:00-07:00 | 上限=50 | 指标=generate_requests_per_model_per_user | 模型=gemini-3.1-pro
WARN  pro@example.com  账号冷却 | 类型=每日限额 | 范围=build:gemini-3.1-pro-preview | 恢复=2026-10-23T00:00:00-07:00 | 与另一通道共用额度，同步冷却
WARN  account4@example.com  账号冷却 | 类型=限流 | 范围=<MODEL> | 恢复=2026-10-21T21:03:49+08:00 | 详情类型=google.rpc.Help | 连续限流=第 1 档 | 同步=两个通道
WARN  account@example.com  账号冷却 | 类型=限流 | 范围=<MODEL> | 恢复=2026-10-21T21:08:19+08:00 | 连续限流=第 2 档 | 同步=两个通道
WARN  account@example.com  账号冷却 | 类型=限流 | 范围=build:<MODEL> | 恢复=2026-10-21T21:38:21+08:00 | 连续限流=第 3 档 | 同步=两个通道
INFO  account2@example.com  限流档位清零 | 范围=<MODEL> | 此前连续=第 2 档
WARN  account3@example.com  账号冷却 | 类型=限流 | 范围=<MODEL> | 恢复=2026-10-21T21:03:49+08:00 | 连续限流=第 1 档 | 号池级限流不升档
WARN  service  号池级限流 | 模型=<MODEL> | 号池=普通号池 | 号池内 10 分钟内至少 5 个账号返回限流且没有成功，不升级冷却
INFO  account@example.com  额度 429 详情未识别 | 类型=google.internal.alkali.applications.makersuite.v1.GeminiApiErrorDetails | 文案=Quota exceeded | 原文=[null,null,null,3] | 请把这一行发给开发者
```

`ACCOUNT_RPM_LIMIT` 大于 0 时，生成请求（经 `generateWithRetry`，含流式）取得的租约刚好占满账号最近 60 秒的名额（已发出数加预占数达到上限）时，在该账号名下记 INFO `账号每分钟请求已满 | 上限=<N> 次/60 秒 | 窗口释放前新请求换到其他账号，热 Worker 都满且分区有空槽时扩容`，`上限` 为当前生效的 `ACCOUNT_RPM_LIMIT`；同一账号 10 分钟最多一条，这 10 分钟从写出这条日志时算起。转写自取的租约占满窗口时不写这条日志，也不占用这 10 分钟的名额。满窗不写冷却、不记 `账号冷却`。

```text
INFO  account@example.com  账号每分钟请求已满 | 上限=10 次/60 秒 | 窗口释放前新请求换到其他账号，热 Worker 都满且分区有空槽时扩容
```

统计分钟限额（日志文件为 `logs/aistudio2api.log`，每行一个 JSON 事件）：

- `grep -c '账号冷却 | 类型=分钟限额'` 只数写入的冷却。写入按取得租约的时刻判断：处理较晚、租约也较晚的 429 会再写一次，所以同一窗口内同一账号可能有多行；只有租约较晚的请求先写回结果（成功或冷却）时，租约较早的才记为 `账号冷却未写入`。
- `grep -cE '账号冷却(未写入)? \| 类型=分钟限额'` 数经 `generateWithRetry`（含流式）的生成请求的全部分钟限额 429。只搜 `类型=分钟限额` 还会数进 Ultra 主用模型的 `限流 429，换号重试` 进度行。
- 这两条 grep 只覆盖经 `generateWithRetry` 的生成请求：转写（与聊天共用每账号每分钟的窗口）、Live 与 Veo 的 429 直接写冷却，不写 `账号冷却` 这一行，也不记进下面的 `requests.attempts`。转写换号时只写 `账号切换`（下一行原因里带上游原文；生成请求换号时同样写这一行），转写的分钟限额 429 只能从转写请求的这类行里数，下面的 SQL 也数不到。
- `grep -oE '连续限流=第 [0-9]+ 档' logs/aistudio2api.log | sort | uniq -c` 数各档写入的次数，`grep -c '限流档位清零'` 与 `grep -c '号池级限流 |'` 数清零与号池级限流；`bash tools/diag_busy.sh` 把档位、清零、号池级限流、当前冷却的档位与号池繁忙快照一起汇总（见 [README](../README.md#号池繁忙-429-与连续限流)）。
- 控制台日志缓冲（4096 条）满时会丢弃新日志（每 10 秒检查一次，有丢弃时在 `service` 名下汇总记一行 WARN `控制台队列已满，丢弃 <COUNT> 条日志 | 丢弃=<COUNT> | 管理页面只保留最近 1500 条，计数以用量账本为准`，这一行只写运行日志、不进管理页面；`额度 429 详情未识别`、`号池级限流`、结论变化时的 `每日额度通道判定`、`排队进度日志已省略`、`请求账本写入队列已满`、`收到停止信号`、`等待超时，仍有 N 个请求未完成` 与 `应用新配置 | 重启生成服务` 走独立的小队列（256 条）、优先写出，普通日志占满控制台队列时也不丢；停止服务时写入协程收尾，写完两个队列里剩下的日志与丢弃汇总（进程退出前最多等 5 秒），收尾开始后的日志直接写），管理页只保留最近的日志，生成请求较准确的统计仍是 `runtime/requests.db`，SQL 不变：每次失败尝试都在 `requests.attempts`（JSON 数组，元素含 `account`、`channel`、`error`、`duration_ms`）里，例如 `SELECT json_extract(j.value,'$.account') a, count(*) FROM requests r, json_each(r.attempts) j WHERE json_extract(j.value,'$.error') LIKE '%per minute per user%' AND r.time > (strftime('%s','now')-3600)*1000 GROUP BY a ORDER BY 2 DESC LIMIT 20;` 列出最近一小时每个账号撞上分钟限额的次数。

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
