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

点击“启动服务”后状态进入 `LAUNCHING`。模型目录在装配运行时（`协议运行时就绪` 之前）已经从写盘缓存 `runtime/model-catalogs/` 一次载入（账户池与协议客户端的目录、权益与 Pro/Ultra 的权益头），启动时不再对全部账户全量同步：只把缺失、恢复时不一致、疑似登录失效与过期（24 小时 ±10%）的账户排进后台队列低速刷新（见[模型目录缓存与后台同步](#模型目录缓存与后台同步)）。缓存非空时立即预热 Worker；完全没有缓存（首次升级、缓存损坏）时等第一个非空目录，其余账户继续后台同步。首个 Worker 就绪后状态进入 `RUNNING` 并开始接收请求，其余目标 Worker 继续预热。`LAUNCHING` 期间点击“停止服务”会取消当前阶段并回到 `STOPPED`。

```text
INFO  service              模型目录缓存载入 | 账户=11872 | 目录=6 | 耗时=84ms | 跳过=权益不符 3、文件缺失 0、版本不符 0、已删除账户 12
INFO  service              生成服务启动 | 1/2 | 准备模型目录 | 缓存=39 | 来自磁盘=11872 | 待同步=41 | 过期=310 | 不一致=3 | 疑似失效=12
INFO  service              生成服务启动 | 2/2 | 预热 WAA Worker | 模型=39 | 目标=150
INFO  account@example.com  WAA Worker 就绪 | 页面模型=gemini-flash-latest | PID=18240 | 耗时=8.172s | 来源=预热
INFO  service              生成服务就绪 | 模型=39 | Worker=1/150 | 耗时=10.686s
INFO  service              模型目录后台同步完成 | 同步=41 | 非空=40 | 模型=39 | 待重试账户=1 | 耗时=52.112s | 来自磁盘=11872
```

`准备模型目录` 的 `缓存` 是启动瞬间合并后的真实模型数（第一个键，旧版只有这一个键）；`来自磁盘` 为本代运行时从缓存恢复的账户数，`待同步` 为启用、就绪却没有目录的账户，`过期` 为已到刷新时间的，`不一致` 为恢复了但指纹与运行态不符（或 Build 目录没有恢复）的，`疑似失效` 为缓存记过需要登录、认证文件之后没变过的账户（预热排最后，优先低速探测）。`模型目录后台同步完成` 在第一轮缺失任务全部有了结果时写：`同步` 为无错误返回的账户数（含空目录），`非空` 为返回非空目录的，`待重试账户` 为仍在退避重试的。

取消与失败日志：

```text
INFO   service  生成服务启动已取消 | 耗时=2.132s
ERROR  service  生成服务启动失败 | 耗时=2.132s | 错误=<ERROR>
```

启动失败、取消或 Stop 会通过 Go `context` 的取消信号停止模型目录后台同步（调度器退出前把缓存写盘），并最多等待 2 秒；超时错误为 `模型目录刷新停止超时`。控制请求等待正在进行的启动或停止操作最多 12 秒，对应 `生成服务启动停止超时`、`生成服务停止超时` 或 `生成服务切换停止超时`；这些错误与 Worker 清理错误进入同一错误链。

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

启动失败记录页面模型、耗时和原始错误；页面类失败的错误末尾另带失败那一刻的页面快照 `| 页面=…`（见[失败时的页面快照](#失败时的页面快照)）。

页面停在 AI Studio 首次使用页时启动流程自动完成它，日志与失败文案见[AI Studio 首次使用页](#ai-studio-首次使用页)。

页面被跳到组织停用页（`access.workspace.google.com` 的 ServiceNotAllowed 等，账号所属组织关闭了 AI Studio）时启动立即失败、不再等页面就绪，日志与状态见[组织停用 AI Studio 的账户](#组织停用-ai-studio-的账户)。

### 组织停用 AI Studio 的账户

Google Workspace 账号所属组织（或 Google）关闭了 AI Studio 时，载入 AI Studio 被跳到 `access.workspace.google.com/ServiceNotAllowed`，RPC 返回 `HTTP 403、协议错误码 7: AI Studio app is disabled.`。启动、验证、登录与请求遇到时都进入同一个账户级状态（`org_disabled`，不参与任何调度，见[开发文档](development.md#组织停用-ai-studio-的账户)），不再干等、不再每分钟重试：

| 日志 | 级别 | 含义 |
| --- | --- | --- |
| `WAA Worker 启动失败 \| 页面模型=… \| 耗时=… \| 错误=AI Studio 已被账号所属组织停用 url=access.workspace.google.com/ServiceNotAllowed \| 页面={…}` | ERROR | 导航后页面停在组织停用页（就绪条件立刻成立，不等 120 秒）；地址只留 host+path，快照字段见[失败时的页面快照](#失败时的页面快照)。填写提示词、首次使用页处理途中或订阅网络事件之后被跳过去时同样以这个前缀失败。不算页面状态类，不清浏览器缓存。旧版本在这一页干等到超时，文案为 `AI Studio 输入框未就绪 url=https://access.workspace.google.com/…: context deadline exceeded` |
| `账号所属组织已停用 AI Studio，停止调度 \| 来源=启动\|模型目录\|验证\|登录\|请求\|登录复核 \| 下次复查=<RFC3339> \| 原文=<前 200 字，邮箱已打码>` | WARN | 账户进入组织停用状态（持久行，同一账户只写一行，复查仍是组织停用、其他路径再遇到时不再写）；`下次复查` 为模型目录长间隔复查的时间（24 小时 ±10%）；`登录复核` 为疑似登录失效或需要登录账户的复核请求返回 app is disabled |
| `组织停用账户复查未完成，顺延 \| 错误=<前 200 字，邮箱已打码> \| 下次复查=<RFC3339> \| 连续失败=<n>` | WARN | 到期的长间隔复查遇到其他失败（5xx、断网、代理不通、空目录、401 等），账户仍是组织停用：复查从 1 分钟起翻倍顺延（±20%，最长 24 小时），401 直接顺延一个完整的复查间隔；顺延后的时间与次数写回 `.org-disabled.json`。同一账户只在第 1、2、4、8… 次写，不写 `模型目录同步失败` |
| `已清除组织停用标记，恢复调度 \| 来源=认证文件更新\|浏览器登录\|重新导入\|新增账户\|启动时发现认证文件已更新\|管理员清除\|验证通过\|复查通过` | INFO | 状态清除，账户立即重新参与调度（持久行）；`验证通过` 为管理验证时浏览器停在 AI Studio 输入框且接口复核通过 |
| `恢复组织停用账户的标记，不参与调度 \| 账户=<n>` | INFO | 启动时按 `.org-disabled.json` 恢复的账户数 |
| `清除已不在账户池中的账户的组织停用记录 \| 账户=<n>` | INFO | 启动时目录已删除的账户的记录 |
| `已清除组织停用账户的状态 \| 账户=<n>` | INFO | 管理页“恢复调度”（`POST /api/accounts/org-disabled/clear`） |
| `WAA Worker 释放 \| 原因=组织已停用 \| …` | INFO | 驻留 Worker 空闲后关闭（见[不可调度驻留 Worker 的释放](#不可调度驻留-worker-的释放)） |
| `新账户自动处理 \| 跳过 \| 原因=AI Studio 已被账号所属组织停用`、`新账户自动处理 \| 验证未通过，账号所属组织已停用 AI Studio，停止调度 \| 原因=…` | INFO/WARN | 新账户自动处理不再验证、启用组织停用的账户；验证时遇到的写后一种 |
| `账户验证完成 \| 已认证=false \| 耗时=… \| 接口复核=跳过\|组织停用 \| 页面=access.workspace.google.com/ServiceNotAllowed \| 原因=AI Studio 已被账号所属组织停用` | INFO | 管理验证遇到组织停用页（或浏览器已登录、接口返回 app is disabled）：未通过，不标需要登录 |

模型目录同步返回 `AI Studio app is disabled` 时不写 `模型目录同步失败`、不按失败退避，只排长间隔复查；状态清除（认证文件更新、管理员“恢复调度”）后目录任务改回常规、立即同步，不等原定的复查时间；生成请求遇到时按已冷却换号（不计连续失败上限、不退避），不写 `账号冷却 | 类型=无权限`，不登记为 403 无权限账户。交互式登录遇到组织停用页时写 `账户登录失败 | 错误=AI Studio 已被账号所属组织停用 url=…`，再用账户已保存的认证文件复核一次接口：同样返回 app is disabled 才进入这个状态（来源=登录）；复核通过、401 或出错时（可见浏览器里登录的可能是别的账号）写 `账户登录 | 登录后停在组织停用页，但已保存的认证文件接口复核没有返回 app is disabled（可能在浏览器里登录了别的账号），账户状态不变 | 接口复核=通过|401|出错|跳过`（WARN），账户状态不变。

Worker 启动完成、发布之前，服务把浏览器 Cookie 合并写回认证文件，并用浏览器里 `https://aistudio.google.com` 的 localStorage 替换认证文件里这个 origin 的 localStorage（其他 origin 保留；启动期间保存过新登录或续签时不写，认证文件被外部换成另一个登录、扫描还没载入时 Cookie 与 localStorage 都不写；键名与值合计超过 512 KB 时不写，见[协议文档](protocol.md)第 2 节）。写回 localStorage 失败只记一行警告，不影响启动；`错误` 只写原因与大小，不写 localStorage 的值：

```text
WARN  account@example.com  WAA Worker 启动后写回 localStorage 失败 | 错误=浏览器 localStorage 超过写回上限（origin=https://aistudio.google.com，大小=530123 字节，上限=524288 字节）
WARN  account@example.com  WAA Worker 启动后写回 localStorage 失败 | 错误=导出浏览器 localStorage: 页面 origin 不是 AI Studio（https://accounts.google.com）
```

账户浏览器缓存（`camoufox-cache/`）的自愈清理（见[开发文档](development.md#账户浏览器缓存)）写下面几行，`大小` 为清理前的 MB 数。启动失败属于页面状态类（`AI Studio 输入框未就绪`（只算页面停在 `https://aistudio.google.com/` 的）、`填写 bootstrap 提示词失败`、`等待官网 Run 按钮启用`、`提交官网提示词`、`官网 WAA service 未暴露`、`官网 GenerateContent 拦截事件超时`、`官网高层 snapshot 函数定位超时`；首次使用页上的失败不算，见[AI Studio 首次使用页](#ai-studio-首次使用页)）时，紧跟 `WAA Worker 启动失败` 写一行，`原因` 为匹配的错误前缀，同一账户 10 分钟内最多一行；失败的这次启动没有用上账户缓存（缓存锁被同账户另一个 runtime 占着，用的是临时 profile 内的缓存）、清理时缓存锁被另一个 runtime 或没能确认退出的浏览器持有、缓存是 12 小时内新建或清空过的时不清理、不写（部署前留下、没有 `.created` 的旧缓存第一次失败就清）。启动时缓存超过 3 天先清空，`原因=缓存超过 3 天`，写在 `3/7 | 启动 Camoufox` 之前。清理失败写 WARN；过期清理失败后 3 天内不再重试、不再写：校验没通过（例如 `camoufox-cache` 是符号链接，错误里有 `拒绝清理`）时什么都不删、照常使用这份缓存，只删了一部分时这次启动改用临时 profile 内的缓存。连续 20 个清理过缓存的账号下一次启动仍以页面状态类失败时（多半是官网整体改版，或失败原因不在缓存），写一行 `暂停启动失败后的浏览器缓存清理`，之后 1 小时内不再清理；清理过缓存的账号启动成功时连续计数归零：

```text
ERROR  account@example.com  WAA Worker 启动失败 | 页面模型=gemini-flash-latest | 耗时=2m4.512s | 错误=AI Studio 输入框未就绪 url=https://aistudio.google.com/prompts/new_chat?model=gemini-flash-latest: 等待页面条件超时: ...
INFO   account@example.com  清理账号浏览器缓存 | 原因=AI Studio 输入框未就绪 | 大小=58.1MB
INFO   account@example.com  清理账号浏览器缓存 | 原因=缓存超过 3 天 | 大小=61.4MB
WARN   account@example.com  清理账号浏览器缓存失败 | 原因=缓存超过 3 天 | 错误=清空账户浏览器缓存: ...
WARN   service              暂停启动失败后的浏览器缓存清理 | 原因=连续 20 个清理过缓存的账号下一次启动仍失败 | 时长=1h0m0s
```

按需容量事件说明当前热池动作：

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

按需扩容、按需替换与空闲回收按 Worker 分区计数：上表的 `N/M` 为普通分区的 Worker 数与 `MAX_ACTIVE_WORKERS`；Ultra 分区（Ultra 号池的账户：权益为 Ultra，`ULTRA_INCLUDE_PRO=true` 时也含权益为 Pro 的账户）的同类事件以 `Ultra WAA Worker` 开头，`M` 为 `ULTRA_MAX_ACTIVE_WORKERS`，常驻数为 `ULTRA_WARM_WORKER_LIMIT`。预热暂停原因中的 `Ultra Worker 槽位已满` 表示 Ultra 分区已满；预热不足按分区判断，Ultra 分区不足时记录 `Ultra WAA Worker 预热等待 | Worker=N/M`，`N/M` 为该分区驻留的 Worker 数与预热目标。保存服务配置或启动生成服务后生效的 `ULTRA_EXCLUSIVE` 发生变化时记录 `Ultra 独占设置已更新并立即生效 | 独占=<BOOL>`。服务配置热更新的汇总行 `配置已热更新 | ...` 带 `Pro 并入 Ultra=<BOOL>`，为账户池当前生效的 `ULTRA_INCLUDE_PRO`；`策略=<ROUTING_STRATEGY>` 之后带 `上游通道=<UPSTREAM_CHANNELS>`（如 `playground,build`），上游通道保存后随这次热更新立即生效，运行中新启用 Build 时另写一行 `已启用 Build 通道，后台补齐 Build 模型目录 | 账户=<N>`（没有 Build 目录的账户由后台目录同步按“不一致”优先级补齐）；`单账户并发=<N>` 之后带 `单账户每分钟上限=<N> 次`，为保存的 `ACCOUNT_RPM_LIMIT`，`0` 时写 `单账户每分钟上限=关闭`；末尾带 `IP 池=<关闭 / 静态 scheme://host 端口 a-b（n 个）/ 动态 scheme://host>`，为保存并随这次热更新生效的 IP 池设置（只写 `scheme://host` 与端口段，不写地址全文、用户名和密码，见 [IP 池](#ip-池)）。

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

### 官网停用 Build 通道的生成

官网关闭了经 Build 应用代理的生成时，Build 代理调用（`ProxyStreamedCall`、需要订阅权益的模型走的 `ProxyUnaryCall`）返回 `HTTP 400、协议错误码 3: PROXY_GENERATION_DISABLED: streamGenerateContent is not supported`（或 `… generateContent is not supported`），可能是错误响应，也可能是流尾状态。只认 Build 代理调用、文案以 `PROXY_GENERATION_DISABLED` 开头（区分大小写，其后为冒号、空白或结束；先去掉 `[original: …]`、`generic::…:`、`beyond::…:` 与 `(qos=…)` 等上游内部标记，与对外返回上游文案时的清洗相同）的错误（`errors.Is(err, aistudio.ErrBuildGenerationDisabled)`），与普通 400（包括回显了客户端同名字段的参数错误）、额度 429、403 和组织停用分开。生成请求遇到时立即改走 Playground，账号随即暂停 Build，10 分钟内 3 个不同账号遇到即全局暂停（规则见[开发文档](development.md)与 README“官网停用 Build 通道的生成”）。运行日志只在状态变化时写，同一状态不重复写；这些行走独立队列，控制台队列满时也不丢：

| 日志 | 级别 | 含义 |
| --- | --- | --- |
| `官网停用了 Build 通道的生成（PROXY_GENERATION_DISABLED），暂停 Build，生成改走 Playground \| 下次试探=<RFC3339> \| 范围=该账号` | WARN | 写在账号名下：该账号第一次遇到，之后不经 Build 接生成请求（Playground 照常）。`下次试探` 为到期时间（30 分钟加最多 3 分钟随机），到期后它在 Build 上的下一次真实请求就是试探；试探仍遇到时顺延，不再写这一行。到期后又过 30 分钟仍没有它的 Build 结果（账号被停用、需要登录，或之后都没再经 Build）、或账号被删除、目录被移走时不再记着它（不写日志），之后再遇到按第一次写 |
| `官网停用了 Build 通道的生成（PROXY_GENERATION_DISABLED），暂停 Build，生成改走 Playground \| 下次试探=<RFC3339> \| 范围=全部账号 \| 触发=10 分钟内 <N> 个账号` | WARN | 写在 `service` 名下：进入全局暂停，所有账号都不再走 Build（选号里 Build 视为未启用）。触发全局暂停的那个账号不再另写按账号的一行；之后每到 `下次试探` 放行一个真实请求走 Build，试探失败只顺延（不写日志），请求照常换到 Playground |
| `Build 通道的生成已恢复，解除暂停 \| 范围=全部账号\|该账号 \| 试探账号=<账号> \| 暂停时长=<DURATION> \| 解除账号=<N>` | INFO | 试探成功（全局暂停中的试探，或暂停过的账号在 Build 上成功）：解除全局与全部账号的暂停 |

请求时间线（请求进度）另有这些行，不进运行日志、也不再逐个请求写 `账号切换`：

| 进度 | 级别 | 含义 |
| --- | --- | --- |
| `Build 通道的生成被官网停用（PROXY_GENERATION_DISABLED），改走 Playground 重试 \| 同账号=是\|否` | INFO | 当次请求改走 Playground：`是` 为在同账号的 Playground 重试（不占换号次数），`否` 为换号；之后的尝试每个账号先 Playground。不计连续失败上限、不退避、不冷却 |
| `Build 通道的生成被官网停用（PROXY_GENERATION_DISABLED），输入超过 Playground 上限，换账号的 Build 再试` | WARN | 输入超过 Playground 上限、只能走 Build 的请求还没有全局暂停：换另一个账号的 Build 再试（只取现在就能取得的账号） |
| `输入超过 Playground 上限，官网已停用 Build 通道的生成（PROXY_GENERATION_DISABLED），无法改走 Build，按 400 返回` | WARN | 没有能处理更长输入的通道（全局暂停中且没到试探时间，或改走 Build 时刚遇到），按 400 返回并写明上限；号池里有更高权益的账号时先照常改用它们再试一次 |
| `Build 生成暂停中，这次请求作为试探改走 Build` | INFO | 这次请求被放行作为全局暂停中的试探（只取已启动 Worker、现在就能用的 Build 账号，不在请求路径上冷启动；取不到就按原条件选号、一分钟后再放行。连续 3 次取不到时号池多半持续饱和，之后的试探排队等 Build 账号，最多 5 秒，主用模型不超过排队上限剩余的一半，等不到同样按原条件选号；所以负载一直很高时官网恢复后也只多几分钟就解除，而不是等负载降下来）。只放行 Build 能调用其模型的生成请求，优先不固定账号、不限定通道的（含输入超过 Playground 上限、改走 Build 的再试）；降级判定与流式优先 Playground 的请求在试探到期超过 5 分钟后才放行 |
| `官网停用了 Build 通道的生成，没有其他通道能接这个请求 \| 原因=…` | WARN | 换号结束时最后的错误仍是 PROXY_GENERATION_DISABLED（只剩 Build 能接、换号次数用完等）：按 Build 不可用的说明返回（只能经 Build 调用的模型与 Playground 接不了的请求 503，输入超过 Playground 上限 400），不把原文交给客户端 |

只能经 Build 调用的模型（号池里没有账户能经 Playground 调用，如 `gemini-4-argon`）在全局暂停中不发上游请求，选号直接返回 503（`request.error` 为 `模型 <模型> 只能经 Build 通道调用，官网停用了 Build 通道的生成（PROXY_GENERATION_DISABLED），暂时无法处理`）；Playground 能调用的模型只是 Playground 账号被换号排除或额度用完时照常按额度不足（429）等原来的错误结束。暂停期间 Worker 的死槽位判定、冷却淘汰登记、预热轮换与账户的冷却状态同样把暂停的 Build 当作不可用：Playground 额度用完的账号按 `WAA Worker 补登记淘汰` 等原有日志淘汰、轮换，账户显示冷却（只启用 Build 时不这样算）。暂停状态只在进程内，服务重启（含重建生成服务）后从头判定。当前状态见 `GET /api/status` 的 `build_channel`（`paused`、只在全局暂停时输出的 `since` 与 `next_probe`、`probing`、`paused_accounts`、按账号暂停已到期而等待试探的 `awaiting_probe`；全局暂停时所有账号都不走 Build、各账号也不单独试探，`paused_accounts` 为遇到过的账号数，`awaiting_probe` 为 0），`bash tools/diag_start.sh` 状态段写一行 `Build 通道：…`（正常、按账号暂停、等待试探，或全局暂停及遇到过的账号数）。

### AI Studio 首次使用页

页面停在 AI Studio 首次使用页（`aistudio.google.com/onboarding`，新的服务条款与年满 18 岁确认，老账号常见；服务端直接落到，或输入框出现后 1～2 秒被页面路由过去）时，启动流程自动完成它：只勾必选项（标签带 `*`、标为必选的服务条款与年满 18 岁等），邮件订阅、调研邀请、通知等可选项不碰，再点继续按钮。首次使用页是多步向导：服务条款那一步之后是用途调查（“What brings you to Google AI Studio?”，继续按钮在选一个用途前禁用），之后可能还有介绍卡片与结尾页；不涉及同意的步骤（用途调查：当前一步里有 `ms-intent-selection` 或其中的用途选项，没有必选项、服务条款或年龄勾选框、其他类型的勾选框，也没有服务条款组件；用途选项只算 `ms-intent-selection` 里、标签取自按钮自己的 `button[role=checkbox]`，同样写法的勾选框不在 `ms-intent-selection` 里或标签认不出时按涉及同意处理）在继续按钮不存在或持续 2 秒仍禁用时点当前一步的“跳过”（Skip、跳过、スキップ 等；轮播与卡片预览里的不点），不替账号选用途；服务条款与年龄那一步绝不跳过（条款内容还没渲染出来、只有页脚时也不跳过），介绍卡片与结尾页点继续类按钮。走完各步、离开首次使用页后重新导航对话页、从 `5/7` 重做到填好提示词（流程与上限见[开发文档](development.md#ai-studio-首次使用页)）。完成后写一行 INFO，之后照常写启动步骤；`勾选` 为勾选次数，括号里为各次勾选的标签（已打码，保留开头 20 字与结尾 9 字，以 `；` 分隔；按放宽规则补勾的、没有必选标记但标签含服务条款或年龄关键词的项带 `[补]`，见[开发文档](development.md#ai-studio-首次使用页)的“放宽”），`按钮` 为最后点击的继续按钮文字（已打码，前 40 字，没有点击时写 `-`），`已做` 为按顺序做过的每一步（`勾选:<标签>`、`点击:<继续按钮>`、`跳过:<跳过按钮>`，以 `；` 分隔，按钮文字去掉图标、打码、前 20 字；超过 12 步时保留开头 4 步与结尾 7 步，中间写 `…（省略 N 步）`），`耗时` 为从开始处理到离开首次使用页（不含离开之后为让官网发完保存请求再等的 2 秒）。同时把完成之后页面的 localStorage 写回认证文件（规则同启动完成后的写回；这次启动之后仍失败也写），写回失败只记一行警告 `完成 AI Studio 首次使用页后写回 localStorage 失败 | 错误=…`。账户验证（新账户自动验证、管理页验证）与登录、添加账户时遇到首次使用页同样自动完成，写同样的一行，前面带操作名；这两种情形不另写回 localStorage：登录导出的认证状态本身带着完成之后的值，验证不写回，之后 Worker 启动成功时照常写回：

```text
INFO   account@example.com  WAA Worker 启动 | 5/7 | 载入 AI Studio
INFO   account@example.com  完成 AI Studio 首次使用页 | 勾选=1（* 我已年满 18 周岁） | 按钮=开始使用 | 已做=勾选:* 我已年满 18 周岁；点击:继续；跳过:跳过；点击:开始使用 | 耗时=12.214s
INFO   account@example.com  WAA Worker 启动 | 5/7 | 载入 AI Studio
INFO   account@example.com  WAA Worker 启动 | 6/7 | 定位 WAA 服务
INFO   account@example.com  账户验证 | 完成 AI Studio 首次使用页 | 勾选=2（* 私は 18 歳以上です；[补]Google 利用規約に同意します） | 按钮=続行 | 已做=勾选:* 私は 18 歳以上です；点击:続行；勾选:[补]Google 利用規約に同意します；点击:続行；跳过:スキップ | 耗时=9.871s
ERROR  account@example.com  WAA Worker 启动失败 | 页面模型=gemini-flash-latest | 耗时=58.1s | 错误=AI Studio 首次使用页未能完成: 找不到可点的继续按钮 | 已做=- | 页面={"url":"aistudio.google.com/onboarding","title":"Welcome to AI Studio ¦ Google AI Studio",…} | 结构={"scope":"ms-onboarding","cur":["ms-tos-disclaimer"],"parts":["ms-tos-disclaimer"],"bodyBoxes":1,"allBoxes":1,"boxes":[{"req":true,"on":true,"by":"star","t":true,"kind":"input/mat-checkbox","label":"* 私は 18 歳以上です"}],"buttons":[{"text":"close_small","on":true,"skip":"neg"}],"links":["続行"],"outside":[{"text":"Get API key","on":true}]} | 端口=10001
ERROR  account@example.com  WAA Worker 启动失败 | 页面模型=gemini-flash-latest | 耗时=96.3s | 错误=AI Studio 首次使用页未能完成: 等待超时（继续按钮未启用） | 已做=勾选:* I am 18 years or older；点击:Continue；跳过:Skip | 页面={"url":"aistudio.google.com/onboarding",…} | 结构={"scope":"ms-onboarding","cur":["ms-model-intro"],"parts":["ms-model-intro","ms-stacked-cards-preview"],…} | 端口=10003
ERROR  account@example.com  WAA Worker 启动失败 | 页面模型=gemini-flash-latest | 耗时=31.7s | 错误=等待官网 Run 按钮启用: 等待页面条件超时: …；已完成首次使用页 | 页面={"url":"aistudio.google.com/onboarding",…} | 端口=10002
ERROR  account@example.com  账户验证失败 | 耗时=1m2.4s | 错误=AI Studio 首次使用页未能完成: 已自动完成 2 次仍回到首次使用页 | 页面={…} | 结构={…}
```

没能完成时启动失败（以及新账户验证、管理页验证的 `账户验证失败`）的错误以 `AI Studio 首次使用页未能完成: <原因>` 开头，原因之后接 ` | 已做=…`（这次按顺序做过的每一步，格式同完成日志；什么都没做时为 `-`，可据此看出服务条款那一步这次是否完成过：有 `勾选:` 与 `点击:` 就是做过；`已自动完成 2 次仍回到首次使用页` 没有这一段），再依次接页面快照与结构摘要 ` | 结构={…}`；这类失败与浏览器缓存无关，不触发缓存自愈清理。原因：

| 原因 | 含义 |
| --- | --- |
| `找不到可点的继续按钮` | 首次使用页已渲染、整页载入完成后，持续 10 秒找不到可点的继续按钮（作用域里当前一步只有关闭、返回、链接、订阅类按钮、卡片与轮播，或按钮都被排除；不到整页上找，Cookie 横幅不点；不涉及同意的用途调查一步里有跳过按钮时 2 秒后先点跳过），结构摘要列出看到的按钮与链接 |
| `等待超时（<最后的状态>）` | 75 秒内没有走完各步、离开首次使用页；括号里为最后一步的状态：`继续按钮未启用`（必选项都勾上了按钮仍禁用，已按服务条款与年龄关键词补勾过；结构摘要里看各勾选框的 `req`、`t`、`o` 与 `controls`，判断是没认出必选项、被邮件通知类关键词排除，还是被其他表单控件挡住；不涉及同意的一步里是没有找到跳过按钮，看 `buttons` 里的文字、`skip` 与 `off`；`cur` 不含 `ms-intent-selection`、`boxes` 里也没有 `intent` 的用途选项时不点跳过，`boxes` 里有不带 `intent` 的勾选框时这一步按涉及同意处理）、`必选项不可勾选`（必选项是禁用的）、`首次使用页尚未渲染`（只有页面外壳，或整页一直没有载入完成）、`页面正在载入`、`等待上一次点击生效`、`点击继续后仍停在首次使用页`、`跳过后仍停在首次使用页`、`勾选后页面没有变化`、`页面正在跳转` |
| `点击继续 8 次后仍停在首次使用页` | 每次完成最多点 8 次继续（多步向导每一步各一次），两次点击（继续或跳过）之间至少隔 3 秒；第 8 次点击 3 秒后再读一次页面，仍在首次使用页才失败 |
| `跳过 4 次后仍停在首次使用页` | 不涉及同意的步骤里每次完成最多点 4 次跳过（单独计数，不占点击继续的名额），跳过之后仍要跳过时失败 |
| `勾选 12 次后仍有必选项未勾上` | 勾了仍显示未勾选（页面没有收到 change），不无限重试 |
| `已自动完成 2 次仍回到首次使用页` | 每次启动（每次验证、登录）最多自动完成 2 次，完成后重新导航仍落到首次使用页时失败，防止来回跳转 |
| `页面脚本失败: …`、`页面脚本返回值无效` | 一步式页面脚本抛错或返回值无法解析 |
| `context deadline exceeded（<最后的状态>）` | 初始化超时（`INIT_TIMEOUT`）或验证超时先于 75 秒总时限到了（进入首次使用页时启动已用掉较多时间）；括号里为最后一步的状态，同 `等待超时（…）` |

结构摘要为单行 JSON，不超过 900 字符：`scope` 为查找范围（可见的 `ms-onboarding` 与 `ms-tos-disclaimer` 里含勾选框的那个，都不含时 `ms-onboarding` 优先，都没有时 `body`）；`cur` 为当前一步所在的 `ms-` 组件（当前一步的勾选框、挑出的继续按钮与要报告的跳过按钮各自往上最近的 `ms-` 组件，都没有时取当前一步第一个按钮或链接的；再接当前一步第一个在 `ms-` 组件里的可见标题（`h1`、`h2`、`h3`、`[role=heading]`）所在的组件；最多 4 个；停在多步向导的哪一步以它为准：含 `ms-tos-disclaimer` 为服务条款、含 `ms-intent-selection` 为用途调查；线上用途调查一步的跳过与继续在 `ms-horizontal-stepper` 里，介绍卡片、结尾页的继续也在共用页脚里时靠标题带出 `ms-stacked-cards-preview`、`ms-outro` 等）；`parts` 为范围里可见、不在 `aria-hidden`/`inert` 容器里也不整个在视口外的 `ms-` 组件（最多 6 个；可能包括同时渲染的其他各步与常驻的卡片预览，线上停在用途调查时 `ms-stacked-cards-preview`、`ms-outro` 也在里面，不代表停在了介绍卡片或结尾页）；`bodyBoxes` 为整页可见勾选框数（比 `boxes` 多时有勾选框在范围之外）；`allBoxes` 为整页全部勾选框数（含不可见的，比 `bodyBoxes` 多时有勾选框没被当作可见）；`boxes` 为勾选框（最多 6 个）：`req` 是否判定为必选、`on` 是否已勾、`dis` 不可勾、`by` 必选依据（`required` 属性、`aria` 为 aria-required、`star` 为标签带 `*` 或“必須”字样、`marker` 为含必填标记元素、`ctx` 为容器文字带 `*`）、`t` 标签含服务条款或年龄关键词（放宽时会补勾）、`o` 标签含邮件、订阅、调研、通知类关键词（不按文字判定为必选、不补勾）、`hid` 勾选框自己不可见（原生 input 被隐藏，靠所在组件或 label 算可见）、`intent` 用途调查选项（`ms-intent-selection` 里、标签取自按钮自己的 `button[role=checkbox]`；其余勾选框标签认不出也按涉及同意处理，这一步不点跳过）、`kind` 类别（`input` 或 `role` 加所在组件的标签名，如 `input/mat-checkbox`、`role/div`）、`label` 标签开头 40 字与结尾 15 字（中间以 `…` 省略，结尾的 `*` 不丢）；`buttons` 为看到的按钮（最多 8 个，先列当前一步的、再列其余各步的；本身就是勾选框的 `button[role=checkbox]`（用途选项）已在 `boxes` 里，不再列）：`text` 文字前 40 字（图标按钮取 aria-label）、`on` 是否启用、`off` 不在当前一步（在 `aria-hidden`、`inert` 容器里，或整个在视口左右之外）、`skip` 为不点的原因（`link` 链接、`area` 勾选框、开关与标签里或页头导航与全局横幅里的、`cookie` Cookie 同意横幅里的、`popup` 菜单与展开类、`card` 轮播与卡片预览里的卡片与“前往第 N 张幻灯片”类按钮（只有文字整句是继续类短语的 `button`，如“继续”“Next”“开始使用”，照常参与；卡片文字带 Try、开始等词也不算）、`neg` 关闭返回取消拒绝跳过与切换账号类（含没有 aria-label 的图标按钮 `close_small`、`arrow_back`、`chevron_left` 等，以及“跳到主要内容”这类无障碍跳转；跳过按钮只在不涉及同意、有用途调查选项的一步里另按跳过规则点）、`opt` 订阅邮件调研通知与注册类、`icon` 认不出是什么的按钮（没有 aria-label、title，文字为空或只有图标））；`links` 为范围内可见链接的文字（最多 3 个，继续是链接样式时在这里）；`controls` 为范围内勾选框以外的可见表单控件（`n` 数量，`kinds` 类别，如 `input:date`、`mat-select`、`role:radio`）；`outside` 为范围之外可见的按钮（最多 3 个，格式同 `buttons`）。文字里的邮箱打码为前两位加 `***@***`，`|` 换成 `¦`；超长时先缩短标签（仍保留开头与结尾）与范围外的按钮、再减少链接、`parts`（保留前 3 个）与条目（按钮保留 4 个时当前一步的优先，其中可以点的与 `neg` 优先，继续与跳过不被用途选项、卡片与轮播挤掉），`scope`、`cur`、勾选框数、`controls` 的计数与各条目的标记一直保留。

订阅网络事件、安装拦截之后（等 Run 按钮、提交、等 WAA service、等被拦截的请求、读取与移除请求捕获）的页面类失败发生时页面停在首次使用页（Run 按钮出现后才被路由过去），这次启动不重做（会重复订阅与拦截）：先采页面快照，就地完成首次使用页并写回完成之后的 localStorage（首次使用状态记在服务端时账号下次启动就能过，记在 localStorage 里时靠这次写回；写回没成功时会写上面那条警告），照常以原错误失败，原错误前缀不变，末尾补 `；已完成首次使用页`（勾选或点击过）；识别之后页面随即自己离开了首次使用页、什么都没点时补 `；首次使用页已自行离开`（不写 INFO）；没能完成时补 `；首次使用页未能完成: <原因> | 已做=…` 并在快照之后接结构摘要。就地完成同样写上面那行 `完成 AI Studio 首次使用页` INFO。这类失败同样不触发缓存自愈清理。运行中 Worker 准备 proof 时不处理首次使用页（`同步官网 prompt` 失败、Worker 重建，重建时由启动流程处理）。失败处判断是否停在首次使用页的那次页面读取与页面快照共用 4 秒上限，浏览器卡住时与原来只采快照一样很快失败。`bash tools/diag_start.sh` 最后一段按来源（Worker 启动、账户验证、账户登录、账户添加）统计自动完成次数（含就地完成；`；首次使用页已自行离开` 另计，不算完成）、按放宽规则补勾（`[补]`）的次数与勾选过的标签、点过跳过的次数与步数、自动完成时 `已做` 的常见序列，按 `WAA Worker 启动失败` 与 `账户验证失败` 两类统计未能完成的次数（同一次失败另写的 `账号切换`、`新账户自动处理出错` 不重复计），按结构摘要的 `scope` 与 `cur` 统计失败停在哪一步（如 `ms-onboarding: ms-intent-selection+ms-horizontal-stepper`，另按 `cur` 里的组件计数；旧版日志没有 `cur` 时按 `parts` 写并标 `（parts）`，不计入按组件的统计），统计失败前这次做过什么（勾选过、点击过继续、跳过过、什么都没做），并列出原因、`已做` 样本（前 5 条，停在哪一步之后括号里写范围里另外可见、不在 `cur` 里的 `ms-` 组件）与结构摘要样本（前 3 条）。

### 失败时的页面快照

Worker 启动与运行中准备 proof 时的页面类失败，在原错误末尾追加一段失败那一刻的页面快照 ` | 页面={…}`。原错误的前缀与文字逐字不变（按前缀匹配的应用层判断与 `tools/diag_*.sh` 照常工作），错误链也不变；按文案判断错误类型的地方（应用层的输入超限判断、`tools/diag_*.sh` 的失败分类）先去掉快照，页面上的文字与数字不参与判断。快照是错误文字的一部分，原错误写到哪里快照就跟到哪里：`WAA Worker 启动失败` 行里在 `| 端口=…` 之前，本机代理中转的说明（`；本机代理中转：…`，接在整条错误之后）接在快照之后；`WAA Worker 重建` 与 `账号切换` 在下一行 `原因:` 的末尾。请求时间线的原因按 300 字节截断，页面类失败的原错误本身多已超过这个长度，时间线里通常看不到快照，以运行日志的 `WAA Worker 启动失败`、`WAA Worker 重建`、`账号切换` 行为准。追加快照的失败：

- 启动 `5/7`：`AI Studio 输入框未就绪 url=…`（`url=` 为完整的 `location.href`，初始化超时时也能取到，不再为空）、读取页面地址与处理启动弹层失败
- 启动 `6/7`：定位官网 snapshot 函数超时、安装 bootstrap 请求捕获失败
- 启动 `7/7`：`填写 bootstrap 提示词失败`、`等待官网 Run 按钮启用`、`提交官网提示词`、`官网 WAA service 未暴露`、`官网 GenerateContent 拦截事件超时`（等待被拦截的 GenerateContent）、`官网 bootstrap 请求正文未捕获` 等读取或移除请求捕获的失败
- 运行中：`同步官网 prompt`、`官网 prompt 状态未同步`、`生成 fresh WAA proof`、`fresh WAA proof 前缀无效`（外层为 `准备 fresh WAA proof: …`，Live（bidi）通道为 `准备 bidi setup fresh WAA proof: …` 或 `准备 bidi fresh WAA proof: …`）

快照只读：在页面里执行一次只读取 DOM 的脚本，不点击、不写值、不导航，只在失败时执行。启动时失败处的 ctx 可能已经过期（初始化超时），快照另用一个 4 秒的独立上下文；调用方已取消（停止服务、客户端断开）时不采集。运行中准备 proof 时快照受请求约束：上下文从请求派生，期限截在请求期限之前 200 毫秒（最多 4 秒），请求剩余不足 0.5 秒时不采集，快照期间请求被取消时立即结束、原样返回原错误。快照不会把请求拖过期限：调用方按请求是否已到期区分“请求自己结束”与“Worker 失败”，拖过期限会让坏页面的 Worker 被当成请求到期、不重建。网络拦截、订阅等 BiDi 命令的失败与代理、浏览器启动的失败不带快照。快照本身失败（浏览器已断开、4 秒内没有回应等）时写 ` | 页面=快照失败: <简短原因>`，原错误照常保留。

下面几条为示意：

```text
ERROR  account@example.com  WAA Worker 启动失败 | 页面模型=gemini-flash-latest | 耗时=2m0.012s | 错误=AI Studio 输入框未就绪 url=https://aistudio.google.com/prompts/new_chat?model=gemini-flash-latest: context deadline exceeded | 页面={"url":"aistudio.google.com/prompts/new_chat?model=gemini-flash-latest","title":"Google AI Studio","ready":"complete","vis":"visible","age":116,"since":118,"nav":"navigate","box":[1,0],"input":[1,0],"textareas":0,"run":[1,0,0],"waa":false,"hook":false,"cap":false,"overlays":["dialog: Welcome to the new AI Studio Signed in as ac***@*** Get started"],"ms":["ms-g1-welcome-dialog","ms-app","ms-navbar","ms-nav-item","ms-prompt-input-wrapper","ms-prompt-box"],"body":"Welcome to the new AI Studio"} | 端口=10001
ERROR  account@example.com  WAA Worker 启动失败 | 页面模型=gemini-flash-latest | 耗时=2m31.4s | 错误=AI Studio 输入框未就绪 url=https://aistudio.google.com/prompts/new_chat?model=gemini-flash-latest: context deadline exceeded | 页面={"url":"aistudio.google.com/prompts/new_chat?model=gemini-flash-latest","title":"Google AI Studio","ready":"complete","vis":"visible","age":14,"since":119,"nav":"navigate","box":[0,0],"input":[0,0],"textareas":0,"run":[0,0,0],"waa":false,"hook":false,"cap":false,"ms":["ms-app"],"body":"Loading"} | 端口=10003
WARN   account@example.com  WAA Worker 重建 | 模型=gemini-flash-latest | 重放当前请求
原因: 准备 fresh WAA proof: 同步官网 prompt: 页面表达式异常: {…Error: 提示词输入框不存在…} | 页面={"url":"aistudio.google.com/prompts/new_chat?model=gemini-flash-latest","title":"Google AI Studio","ready":"complete","vis":"visible","age":7,"since":3605,"nav":"reload","box":[0,0],"input":[0,0],"textareas":0,"run":[0,0,0],"waa":false,"hook":false,"cap":false,"ms":["ms-app"],"body":"Loading"}
ERROR  account@example.com  WAA Worker 启动失败 | 页面模型=gemini-flash-latest | 耗时=48.2s | 错误=填写 bootstrap 提示词失败 value="" err=页面表达式异常: {…Error: 提示词输入框不存在…} | 页面={"url":"aistudio.google.com/prompts/new_chat?model=gemini-flash-latest","title":"Google AI Studio","ready":"complete","vis":"visible","age":21,"since":23,"nav":"navigate","box":[0,0],"input":[0,0],"textareas":0,"ta":["div@ms-prompt-editor: Type something"],"run":[0,0,0],"waa":false,"hook":true,"cap":true,"ms":["ms-prompt-editor","ms-prompt-input-wrapper","ms-app","ms-send-button","ms-navbar"]} | 端口=10002
ERROR  account@example.com  WAA Worker 启动失败 | 页面模型=gemini-flash-latest | 耗时=12.4s | 错误=官网 WAA service 未暴露: BiDi 连接已断开: EOF | 页面=快照失败: BiDi 连接已断开: EOF
```

- 第一条停在 `5/7`：输入框在 DOM 里（`box`、`input` 总数为 1）但被页面隐藏了（可见数为 0），同时有一个欢迎弹层，`ms` 最前面的 `ms-g1-welcome-dialog` 是弹层里的组件。可见数不看遮挡（见下表 `input`），这里不能读成“弹层挡住了输入框”：被弹层盖住的输入框仍算可见，就绪检查也会通过；输入框被隐藏与弹层同时出现是否有因果，要结合 `ms`、`overlays` 判断（例如页面切到了新的引导状态）。`since` 只比 `age` 多 2 秒，导航之后文档没有被替换过：页面载入一次后输入框一直没出现。还没到 `6/7`，`hook`、`cap` 为 `false` 是正常的。
- 第二条也停在 `5/7`，但 `since` 比 `age` 多 105 秒：程序导航约 105 秒后文档被替换过（刷新或跳转），当前文档才载入 14 秒，输入框还没出来。`nav` 仍是 `navigate`：`location.replace`、`location.assign`、给 `location.href` 赋值与 meta refresh 都记为 `navigate`，只有 `location.reload()` 记为 `reload`，判断中途刷新看 `since` 与 `age` 之差，不看 `nav`。“耗时”从开始启动算起，含等冷启动名额、启动浏览器、连接 BiDi 的时间，不能拿来和 `age` 比。
- 第三条：Worker 早已就绪（`since` 3605 秒），页面却是整页刷新（`nav` 为 `reload`）后 7 秒，`hook` 为 `false`：`6/7` 写在 window 上的标记与 Bootstrap 取得的 WAA service（`waa`）都随刷新丢了。
- 第四条停在 `7/7`：`hook`、`cap` 仍为 `true`，`since` 与 `age` 只差 2 秒，页面没有重载；`ms-prompt-box` 不见了，可见的输入框是 `ms-prompt-editor` 里的 `div`（`ta`），Run 按钮也不在 `ms-run-button` 里（按文字找到的按钮在 `ms-send-button`）：多为官网改版。

| 字段 | 含义 |
| --- | --- |
| `url` | 页面地址，只保留 host、path 与查询参数里的 `model`（其余参数与 `#` 之后都去掉）；没有 host 的地址写 `about:blank` 这样的 scheme 加内容 |
| `title` | `document.title` 去掉多余空白后的前 120 字 |
| `ready` | `document.readyState`：`loading`、`interactive`、`complete` |
| `vis` | `document.visibilityState`：`visible`、`hidden` |
| `age` | 当前文档载入至今的秒数（`performance.now()`，从这份文档的导航开始算）：页面刷新或跳转后从 0 重新算，与 `since` 对照判断导航之后文档是否被替换过 |
| `since` | 程序发出导航 AI Studio 命令（`5/7` 开头）至今的秒数，由程序记录、不取自页面；运行中准备 proof 时同样从启动时的这次导航算起，登录等其他页面没有这一项。导航之后文档没被替换时 `since` 只比 `age` 多 0–2 秒（命令往返与取整）；多出几秒以上说明导航之后文档被替换过（刷新、`location.replace`/`assign` 跳转、meta refresh 等），替换发生在导航后约 `since − age` 秒，与 `nav` 无关 |
| `nav` | 当前文档的载入方式（`performance.getEntriesByType('navigation')[0].type`）：`navigate`（打开或跳转，含 `location.replace`/`assign`、给 `location.href` 赋值与 meta refresh）、`reload`（`location.reload()` 或浏览器刷新）、`back_forward` |
| `box` | `ms-prompt-box` 的 `[总数, 可见数]`，可见的判断同 `input` |
| `input` | `ms-prompt-box textarea`（启动与 proof 写入的提示词输入框）的 `[总数, 可见数]`：可见数为 0 即“提示词输入框不存在”，大于 1 即“匹配多个可见目标”。可见按 `checkVisibility` 判断（与就绪检查相同）：元素或祖先为 `display:none`、`visibility:hidden` 或处在 `content-visibility:hidden` 里时算不可见；不看透明度，也不看是否被别的元素盖住，被弹层盖住的输入框仍算可见（就绪检查也会通过，不会停在 `5/7`）。可见数为 0 是页面自己把输入框隐藏了，同时出现的弹层只说明它也在，不说明是它挡住了输入框 |
| `textareas` | 页面上所有可见 `textarea` 的个数：`input` 为 0 而这里不为 0，多为界面改版、输入框换了位置 |
| `ta` | 可见的文本输入框（`textarea`、`contenteditable`、`role=textbox`），最多 3 个，每个写成 `标签名@最近的 ms- 祖先: placeholder 或 aria-label 的前 40 字`（没有 `ms-` 祖先写 `-`）：`input` 为 0 时看输入框换到了哪个组件 |
| `run` | `ms-run-button button`（官网 Run 按钮）的 `[总数, 可见数, 可见且启用数]` |
| `waa` | `window.__aistudioWaaService` 是否存在：`7/7` 提交 bootstrap 提示词之后才出现，此前为 `false` 是正常的；运行中变成 `false` 说明页面已经刷新，Bootstrap 取得的 WAA service 随之丢失 |
| `hook` | `window.__aistudioWaaSnapshotKey` 是否存在：`6/7` 定位官网 snapshot 函数时写在 window 上，整页重载后消失。`7/7` 失败与运行中为 `false` 即 `6/7` 之后页面整页重载过；`6/7` 之前为 `false` 是正常的 |
| `cap` | `6/7` 安装的 bootstrap 请求捕获（`window.__aistudioRestoreBootstrapCapture`）是否还在：`7/7` 失败时为 `false` 即页面重载过；Bootstrap 完成时移除，运行中为 `false` 是正常的 |
| `overlays` | 可见的 `[role=dialog]`、`[role=alertdialog]`、`mat-dialog-container`、`.cdk-overlay-container` 里的文字，每条以 `dialog:`、`alertdialog:`、`mat-dialog:`、`cdk-overlay:` 开头，去掉多余空白、各取前 160 字，同一元素与相同文字只记一次，最多 6 条。没有文字的弹层也记一条：写 `(无文字)`，只含 iframe 时写 `(无文字, iframe)`；`.cdk-overlay-container` 常驻且平时为空，只在有遮罩（`.cdk-overlay-backdrop-showing`）时写 `(遮罩)` |
| `ms` | 页面上可见的、以 `ms-` 开头的自定义元素标签名，去重后最多 20 个，按优先级收集：先是可见输入框（同 `ta`）与 Run 按钮（`ms-run-button button`，没有时找文字或 aria-label 以 Run 开头的可见按钮）往上最近的 3 个 `ms-` 祖先，再是弹层里的组件（最多 6 个），最后按文档顺序补足。出现没见过的标签多为官网改版 |
| `alerts` | 弹层之外可见的提示条里的文字：`[role=alert]`、`[role=status]`、`aria-live` 为 `assertive` 或 `polite` 的区域与 `mat-error`，去掉多余空白、各取前 100 字，去重，最多 2 条；弹层里的提示已记在 `overlays`，不重复记。页面主区域里的错误条、加载失败提示多在这里 |
| `body` | `document.body.innerText` 去掉多余空白后的前 200 字（多是侧栏导航文字，超长时最先缩短） |
| `err` | 采集失败的单项与原因（如 `textareas: …`），最多 4 条；每项单独采集，一项失败不影响其他项 |

快照为单行 JSON，总长度不超过 800 字符：超出时依次缩短、清空 `body`，再缩短 `alerts`（只留 1 条、60 字）、`ms`（只保留前面的优先段）、`overlays`、`ta` 与 `title`，计数、`url`、`age`、`since`、`nav`、`waa`、`hook`、`cap` 一直保留，仍超出时按字符截断并以 `…` 结尾。文本里的邮箱一律打码为前两位加 `***@***`（如 `ac***@***`）：页面里先打码再截短（截断点落在邮箱中间也不会留下本地部分），截短处以 `…` 结尾，写日志前再打码一次（截断在域名里、以 `…` 结尾的残缺地址同样打码）；`|` 换成 `¦`（日志字段以 ` | ` 分隔），值为空的字段省略。`bash tools/diag_start.sh` 的“失败时的页面状态”一段按快照分组汇总这些失败（启动失败、`WAA Worker 重建`，以及原因为 `准备 fresh WAA proof: …`、`准备 bidi setup fresh WAA proof: …` 或 `准备 bidi fresh WAA proof: …` 的 `账号切换`：运行中准备 proof 失败后没等到 Worker 关闭就换号时只记这一行）。分组键（路径、标题、输入框可见数、Run 按钮可见数、弹层文字）相同的失败根因可能不同（改版、中途刷新、一直没加载出来），每组另列组内 `nav`、`hook`、`ready`、“导航后换过文档”（`since` 比 `age` 多 5 秒以上）与“输入组件”（`ta` 第一项所在的 `ms-` 组件）的分布，再按失败原因（前 3 种）各列该原因的分布与最近一条完整快照；失败原因里页面表达式异常折叠成报错正文（如 `Error: 提示词输入框不存在`），见 [README](../README.md#号池繁忙-429-与连续限流) 的“Worker 起不来”。

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

## IP 池

IP 池（`PROXY_POOL_*`，见 [开发文档](development.md#ip-池)）的日志只写 `scheme://host`、端口号、会话号与账户标签，不写地址全文、用户名和密码；IP 池关闭时的直通租约不记日志。全局状态用 `service` 来源，单个账户的分配、归还与换会话用账户标签（新账户登录还没有账户 ID 时为 `auth`）。诊断脚本不解析这些行。已有行的首段与字段不变，只在两类行的末尾追加 IP 池出口：生成本身裸限流 429 写入的 `账号冷却 | 类型=限流 | …` 行末尾追加账户当前出口的 ` | 端口=10023` 或 ` | 会话=3fa9c01b7d2e`（看得出裸 429 是否集中在某个端口上），`WAA Worker 启动失败 | 页面模型=… | 耗时=… | 错误=…` 行末尾追加这次启动用的端口或会话（启动失败归还之前取，不是之后的默认出口）；账户有自己的代理、IP 池关闭或走默认代理时不追加。诊断脚本按行首文案与 ` | ` 分隔的键值解析，不受影响。

```text
INFO  service  IP 池已更新 | 模式=静态 | 出口=http://isp.decodo.com | 端口=10001-10200（200 个） | 需要=188
INFO  service  IP 池已更新 | 模式=动态 | 出口=http://gate.ipfoxy.io
INFO  service  IP 池已关闭 | 仍在使用旧出口的浏览器=12（关闭后归还）
INFO  service  IP 池已开启，运行中的 Worker 仍走默认代理，关闭后改用 IP 池 | Worker=140 | 立即切换：停止服务后再启动
INFO  service  IP 池开启后，没有自己代理的账户不再使用默认代理 PROXY
WARN  service  IP 池端口少于可能同时运行的浏览器数，超出的会与其他账号共用端口 | 端口=100 | 需要=188
INFO  service  IP 池端口已足够 | 端口=200 | 需要=188
WARN  service  有账户固定了与默认代理相同的地址，不会使用 IP 池 | 账户=37 | 可在“服务配置 → IP 池”改为跟随默认代理
WARN  service  IP 池端口段与账户固定代理或 PROXY 重叠，这些端口不再分配 | 端口数=3 | 账户数=5
WARN  service  本机代理中转连接上限偏低，浏览器多时可能出现“连接数已满：整个进程” | 上限=1024 | 建议 ulimit -n ≥ 65536
WARN  service  IP 池跨进程端口锁不可用，多个进程使用同一端口段时会重复分配 | 错误=读取用户缓存目录: neither $XDG_CACHE_HOME nor $HOME are defined
WARN  service  IP 池跨进程端口锁不可用，多个进程使用同一端口段时可能重复分配，5 分钟后再试 | 错误=锁定 IP 池端口: too many open files
INFO  service  IP 池跨进程端口锁已恢复 | 补锁端口=12 | 已被其他进程占用=0（浏览器关闭前与其他进程共用）
INFO  a@example.com  IP 池分配 | 端口=10023 | 来源=亲和 | 用途=Worker | 使用中=37/200
INFO  a@example.com  IP 池分配 | 会话=3fa9c01b7d2e | 用途=Worker | 使用中=37
INFO  a@example.com  IP 池分配 | 会话=3fa9c01b7d2e | 来源=同账号 | 用途=验证 | 使用中=37
INFO  a@example.com  IP 池不分配，与仍走默认代理的 Worker 用同一出口 | 用途=验证
INFO  a@example.com  IP 池归还 | 端口=10023 | 使用中=36/200
INFO  a@example.com  IP 池归还 | 会话=3fa9c01b7d2e
WARN  a@example.com  IP 池端口不足，与其他账号共用 | 端口=10023 | 共用账号=2 | 使用中=200/200 | 冷却=3 | 这一分钟内另有 4 次
WARN  a@example.com  IP 池端口全部冷却，使用最早恢复的端口 | 端口=10023 | 恢复=2026-10-23T08:15:00Z
WARN  a@example.com  IP 池出口连不通 | 端口=10023 | 连续=1/2
WARN  a@example.com  IP 池没有可用端口，改用默认代理
WARN  service  IP 池端口冷却 | 端口=10023 | 第 1 档 | 时长=15m0s | 恢复=2026-10-23T08:15:00Z | 原因=10 分钟内 2 个账号返回限流且没有成功
WARN  service  IP 池端口冷却 | 端口=10087 | 第 2 档 | 时长=30m0s | 恢复=2026-10-23T08:30:00Z | 原因=连续 2 次出口连不通
WARN  service  IP 池多数端口同时出错，按上游或代理商整体问题处理，暂不冷却端口 | 出错端口=37/60
WARN  a@example.com  IP 池换会话 | 会话=3fa9c01b7d2e → 3fa9c01b7d2er1 | 原因=连续 2 次出口连不通 | 生效=立即（没有 Worker）
WARN  service  IP 池租约泄漏 | 已删除账户仍占用=2
WARN  service  IP 池占用超过可能的浏览器数 | 占用=205 | 需要=196
WARN  service  IP 池记录读取失败，重新开始记录 | 错误=invalid character '}' looking for beginning of value
WARN  service  IP 池记录写入失败 | 错误=no space left on device
INFO  service  账户改为跟随默认代理 | 账户=37 | 运行中的 Worker=12（关闭后改用 IP 池）| 跳过=0 | 耗时=1.2s
INFO  service  代理测试 | IP 池=静态 | 端口=10001 | 浏览器路径=203.0.113.7（812ms） | 后台请求路径=203.0.113.7（640ms）
INFO  service  代理测试 | 账户=a@example.com | 代理=http://isp.decodo.com:10023 | 端口=10023 | 浏览器路径=203.0.113.7（812ms） | 后台请求路径=203.0.113.7（640ms）
WARN  service  IP 池检查完成 | 出口=http://isp.decodo.com | 端口=200 | 成功=198 | 不同 IP=196 | 重复=2 组 | 失败=2 | 耗时=41s
```

| 日志 | 含义 |
| --- | --- |
| `IP 池已更新` / `IP 池已关闭` | 启动或热更新后生效的设置（配置与生效值相同时不记）；`需要` 为可能同时运行的浏览器数 Σ(峰值 + 排空上限)，不含登录与验证 |
| `IP 池已开启，运行中的 Worker 仍走默认代理…` | 关闭 → 开启时仍有 Worker 持有直通租约（继续走 `PROXY`），它们关闭后再启动时改用 IP 池；要立即全部切换，停止服务后再启动 |
| `IP 池开启后，没有自己代理的账户不再使用默认代理 PROXY` | 开启 IP 池且 `PROXY` 非空时，配置变化时记一次 |
| `IP 池端口少于可能同时运行的浏览器数…` / `IP 池端口已足够` | 端口数少于 `需要` 时超出的浏览器与其他账号共用端口；只在状态切换时记一行（改峰值、Ultra 峰值或启动并发都会重新判断） |
| `有账户固定了与默认代理相同的地址，不会使用 IP 池` | 账户代理的规范值与 `PROXY` 相同（多半是旧版 setup 写进去的），这些账户不用 IP 池；设置页可一键改为跟随默认代理 |
| `IP 池端口段与账户固定代理或 PROXY 重叠…` | 账户代理或 `PROXY` 指向池内端口，这些端口记为外部占用、不再分配 |
| `本机代理中转连接上限偏低…` | 开启 IP 池时进程连接上限低于 `容量 × 16`，浏览器多时中转可能回 503；调高 `ulimit -n` 后重启 |
| `IP 池跨进程端口锁不可用…` | 启动时定不下锁目录（读不到用户缓存目录）时记一次，之后不跨进程加锁；运行中加锁出错（打开文件数用满、缓存目录暂时不可写等）时一段出错期记一次，5 分钟内照常分配、不加锁，到期再试。期间同一台机器上多个进程用同一端口段时可能分到同一端口 |
| `IP 池跨进程端口锁已恢复` | 出错后第一次正常加锁：给出错期间分出、仍在用的段内端口补锁；`已被其他进程占用` 不为 0 时为 WARN，这些端口在浏览器关闭前与其他进程共用 |
| `IP 池分配` / `IP 池归还` | 一个浏览器（Worker、登录、验证）或一次导入申请或归还出口。`来源`：`亲和`（回到上次的端口）、`空闲`、`同账号`（同一账户已有浏览器在用，共用它的端口；Worker 没有可分配端口时回到同一账户正在与别人共用的端口也记这个，另有一行端口不足的 WARN）、`共用`；动态 IP 的登录、验证沿用账户正在用的会话时带 `来源=同账号`（换会话之后、Worker 重启之前仍是旧会话）；`用途`：`Worker`、`登录`、`验证`、`导入`；`使用中` 为有浏览器占用的端口数 / 端口数（动态为在用的会话数）。Chrome 导入每个账号导入完就归还 |
| `IP 池不分配，与仍走默认代理的 Worker 用同一出口` | 账户正在服务的 Worker 是开启 IP 池之前（或当时没有可用端口时）启动的、仍走 `PROXY`：这次登录或验证与它用同一个上游，不另分端口或会话（同一账户同时只用一个 IP）；Worker 关闭后再启动时改用 IP 池 |
| `IP 池端口不足，与其他账号共用` | 没有可分配的端口，与持有最少的端口共用（两个账号同一个 IP）；每分钟最多一行，带这一分钟内另有的次数 |
| `IP 池端口全部冷却，使用最早恢复的端口` | 段内端口都在冷却，用恢复最早的那个 |
| `IP 池出口连不通` | Worker 启动因代理上游失败（连不上、超时、407 等，`errors.Is(err, proxyrelay.ErrUpstream)`）而失败，计入端口的规则 B 或动态 IP 的换会话；取消与其他原因的失败不记 |
| `IP 池端口冷却` | 端口开始冷却：规则 A 为 10 分钟内 2 个不同账号返回裸限流 429 且其间没有生成成功（只计自最近一次成功以来只在这个端口上被拒、被拒前还不是坏号的账号，且其中至少一个 24 小时内在别的端口或出口上成功过），规则 B 为连续 2 次出口连不通；第 n 档 `min(15m × 2^(n-1), 2h)`，冷却不影响已在用它的浏览器 |
| `IP 池多数端口同时出错…` | 出错端口达到 `max(5, 50% × 使用中端口数)`，按上游或代理商整体问题处理，暂不冷却；每个 10 分钟窗口最多一行 |
| `IP 池换会话` | 动态 IP：同一账户 30 分钟内 2 次出口连不通，换到下一个会话号，两次之间至少 10 分钟；`生效` 为 `立即（没有 Worker）` 或 `下次启动 Worker`。裸 429 不换会话：每个会话各自是一个出口、只有一个账号，凑不够出口限流判定的门槛（`原因=出口限流判定成立` 只在判定的出口就是账户当前会话时出现，实际不会出现；仍走 `PROXY` 的 Worker 所在出口判定成立时不换） |
| `IP 池端口冷却：撤回逐级冷却并换端口重启 Worker` | 端口冷却后（`service` 来源，持久行）：`规则=不同账号返回限流`（规则 A）时撤回计入规则 A 的账号（见上一行）的逐级冷却（缩到约 10 秒的短冷却并清除档位，原因改为 `限流: 出口限流撤回（原第 N 档），…`；被拒的是 IP 不是账号），`撤回账号` 为有冷却被撤回的账号数、`撤回坏号` 为坏号标记被撤回的账号数（这个窗口内因连续裸 429 标上的，见 `账号连续裸限流，移出预热与替补候选`；连续次数一并清零）、`失败` 为写运行态出错的；两种规则都让正用这个端口服务的 Worker 在后台关闭（等同账号进行中的请求结束），补齐预热时换到别的端口，`重启 Worker` 为登记关闭的个数（登记前核对账户仍用这个端口服务，已换到别的端口的不计）；`未登记` 只在有时出现，为 ResetAll、Close 期间或独占等待登记失败、没有登记关闭的个数。都为 0 时不记 |
| `IP 池端口冷却，已关闭用这个端口的 Worker，下次启动换端口` / `IP 池端口冷却后关闭 Worker 出错` | 上一行登记的后台关闭的结果（账户标签）；等不到同账号的请求结束时照常记 `WAA Worker 后台恢复放弃` |
| `IP 池端口冷却后撤回逐级冷却失败` | 撤回某个账号的冷却时写运行态出错（账户标签） |
| `IP 池租约泄漏` / `IP 池占用超过可能的浏览器数` | 已删除账户的浏览器关闭失败仍占着出口；或池租约数持续 5 分钟大于 `需要`（容量加登录、验证可能同时占用的 8 个）（多半有浏览器没经 `Close` 结束） |
| `IP 池记录读取失败…` / `IP 池记录写入失败` | `runtime/proxy-pool.json` 坏了、版本不认识或写不进去；读取失败时从空开始记录 |
| `IP 池没有可用端口，改用默认代理` | 端口段内的端口全是外部占用（账户固定代理、`PROXY` 指向或其他进程锁住），这次浏览器改走 `PROXY` |
| `账户改为跟随默认代理` | 设置页“改为跟随默认代理”的结果：`运行中的 Worker` 为不重启、关闭后才改用 IP 池的数量；`跳过` 为正忙或 Worker 正在启动、这次没改的账户（有跳过或失败时为 WARN） |
| `代理测试 \| IP 池=…` / `代理测试 \| 账户=…` | 设置页“抽测出口”每个出口一行，格式同上文“本机代理中转”里的代理测试，多一个 `端口` 或 `会话` 字段；账户编辑对话框里留空测试时测这个账户实际的出口，带它当前的端口或会话（账户原来有自己的代理、在对话框里清空了时测清空后 IP 池给它的默认出口，记一行 `代理测试 \| IP 池=…`）；新账户对话框（浏览器登录、Chrome 导入）里留空测试且 IP 池开启时测此刻会分给登录的出口，记一行 `代理测试 \| IP 池=…` |
| `IP 池检查完成` | “检查全部端口”结束：只走后台请求路径，`不同 IP` 为成功端口中不同出口 IP 的个数，`重复` 为出口 IP 相同的端口组数；有重复或失败时为 WARN |

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

Omni 视频模型的请求在协议层整理时，每条说明在选号前写一条进度（WARN，来源 `service`）；发送前按所选账户的目录再整理时留下的新说明（协议层所用的合并目录与所选账户的目录不一致，例如别名目录缓存还没刷新；或协议层没有整理到的请求，例如启动期间别名目录取不到、目录没有列出视频选项的 Interaction 模型）同样写一条进度（WARN，来源为该账户，换号重试时每个账户各记各的）。两种都进入排查记录的 `timeline`，不影响响应（规则见 [protocol.md](protocol.md#omni-视频设置与模型名后缀)）：

| 消息 | 含义 |
| --- | --- |
| `未确认 CreateInteractionStream 的停止序列字段，已忽略 <N> 个停止序列` | 请求带了 stop sequences（OpenAI `stop`、Anthropic `stop_sequences`、Gemini `stopSequences` 等）：官网 Run settings 有 Add stop sequence，但没有 CreateInteractionStream 停止序列字段的现场证据，不猜字段号，也不在本地匹配，已清空，不再整次返回参数错误 |
| `视频时长 <N> 秒不是模型可选的时长（<MIN>–<MAX> 秒、步长 <STEP>），按最接近的 <M> 秒发送` | 时长超出模型目录 `video_duration_range` 的范围或不在步长上 |
| `视频宽高比 <值> 不是模型选项，按最接近的 <16:9 或 9:16> 发送` | 宽高比不是 16:9、9:16 |
| `视频分辨率 <值> 不是模型选项，按最接近的 <分辨率> 发送` | 分辨率不在模型目录 `video_output_resolutions` 中（如 `2160p`、`1440p`）；目录的 `368p` 与请求的 `360p` 都按 `360p` 发送，不记这一行 |
| `视频任务 edit 不带输出格式，宽高比、时长与分辨率不发送` | 编辑任务同时设置了宽高比、时长或分辨率（编辑请求与官网一样不带输出格式） |

```text
WARN  service  未确认 CreateInteractionStream 的停止序列字段，已忽略 1 个停止序列
WARN  service  视频时长 30 秒不是模型可选的时长（4–15 秒、步长 1），按最接近的 15 秒发送
WARN  account@example.com  视频时长 15 秒不是模型可选的时长（4–8 秒、步长 1），按最接近的 8 秒发送
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
| `原因` | 当前在等什么：`账号全部忙碌`、`没有空闲的 Worker 槽位`、`Worker 正在启动`、`Worker 正在扩容`、`Worker 容量已满`、`账号忙碌`、`账号冷却中`；出口限流判定成立期间另有 `出口限速`（候选所在出口的令牌桶已满）与 `驻留账号该模型额度已用完`（受限出口上的备用账户已剔除、驻留里没有能服务该模型的热 Worker，补覆盖配额已满或正在启动），非主用模型按这两个原因累计排队最多 30 秒（见[出口限流](#出口限流)） |
| `已等待` | 排队的时间：每次选号从第一次等待到取得账号或选号结束按墙钟计，换号重试接着累加；两次选号之间上游处理、退避与 Worker 恢复的时间不算 |
| `尝试` | 第几次选号（换号重试递增） |
| `同条件队列` | 在选择条件相同的请求（模型、通道限定、号池等）中排第几、共几个 |
| `热空闲`、`热忙碌`、`备用`、`备用忙碌` | 本轮复查时该模型的候选数：已有 Worker 的账号与备用账号，冷却中的不计 |
| `每分钟已满` | 其中因最近 60 秒的生成请求已达 `ACCOUNT_RPM_LIMIT` 而按忙碌分组的候选数（已计入 `热忙碌` 与 `备用忙碌`）；为 0 时不写。在排除本请求已失败的 Worker 之前取，只作提示。满窗不算冷却，不新增等待原因：热账号都满时原因照常为 `账号全部忙碌` 等 |
| `启动中`、`最早恢复` | 有候选的 Worker 正在启动；候选中最早恢复的冷却还剩多久 |
| `出口限速`、`出口已满`、`下一个令牌` | 只在有候选因所在出口的令牌桶已满而按忙碌分组时写（写在 `启动中` 之后、`最早恢复` 之前）：`出口限速=<L>/分钟` 为候选所在出口通道当前的限速，`出口已满=<n>` 为这样的候选数，`下一个令牌=<DURATION>` 为主用模型下一个令牌还要等多久 |

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

`原因` 为最后一次等待的原因；`已排队` 为计入排队上限的时间（整个请求共用一份预算，不含请求自己现场冷启动、等这次选号发起的后台扩容与候选冷却的等待；后两者同样延后截止时间，流式请求写出响应头前的等待一并推迟），`总耗时` 为从第一次选号起的墙钟时间（含失败的尝试与换号退避），`尝试` 为第几次选号。从 `模型=` 起是超限这一刻的候选快照，`同条件队列` 到 `最早恢复` 与上表相同；`驻留冷却` 取自主用模型供给（`/api/status` 的 `primary_supply[].cooling`，约 30 秒统计一次的缓存）里该模型在请求号池对应分区的条目：不在排空中、是该模型的候选、但所有支持的启用通道都在冷却的驻留 Worker 数（含剩余不到 2 分钟的短冷却），缓存里没有对应条目时不写。快照只写进这一行，不进 `request.error` 与用量账本，那里仍是 `号池繁忙：模型 <MODEL> 暂时没有空闲的账号或 Worker，请稍后重试（[Ultra 号池，]已排队 <DURATION>，上限 <DURATION>[，总耗时 <DURATION>[（含<冷启动与扩容>）]][，第 <N> 次尝试]，最后等待原因：<原因>）`，其中 `<冷启动与扩容>` 为 `现场冷启动 <DURATION>`、`等后台扩容 <DURATION>` 之一，或两项都有时 `现场冷启动 <DURATION>、等后台扩容 <DURATION>`。出口限速排队超限另用首段 `出口限速：模型 <MODEL> 所在出口每分钟请求已满，请稍后重试（…）`（不写最后等待原因）。出口限流期间要冷启动的 Worker 被冷启动闸门暂停（转写、Live、Veo 选中没有 Worker 的账号，指定账户的生成请求）时同样按号池繁忙 429 返回：`已排队 0s，上限 0s`（没有排队），最后等待原因为 `出口限流中，冷启动按配额进行`，`Retry-After` 5 秒，响应头 `X-AIStudio2API-Reason: egress_limited`。`驻留冷却` 不小于 `热空闲` 加 `热忙碌` 时，驻留 Worker 大多对该模型在冷却（见[额度冷却日志](#额度冷却日志)的连续限流）；`bash tools/diag_busy.sh` 按这些快照归因并汇总。

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

## 429 治理

这一组日志对应“无缘无故 429”的五项治理：普通号池的换号上限、出口限流判定与每出口限速、模型目录写盘缓存、不可调度驻留 Worker 的释放，以及“需要登录”的误判收紧与自动恢复。标“持久”的行走独立的控制台队列（`logDurable`），普通日志把控制台队列挤满时也不丢；`bash tools/diag_busy.sh` 按这些行的首段与 `键=值` 统计，首段文字与键名不要改。

### 普通号池的换号上限

不固定账号、文件绑定的生成请求（普通号池的主用与非主用模型，以及 `/ultra` 上的非主用模型）遇到额度类 429 时按类计数：每日类（每日限额、usage limit、剩余不少于 15 分钟的冷却）最多再换 4 个账号，限流类（裸 `Quota exceeded`、分钟限额）最多再换 2 次，限流类失败后紧接着的选号只用已启动的 Worker；出口正常时第 2 次限流类换号前等 0.3–0.8 秒错开并发重试。每个请求最多在 8 个不同账号上尝试；因登录失效（上游 401）换号最多 2 次，与额度换号分开计。Ultra 主用模型的规则与文案不变（见上一节）。请求进度：

| 进度 | 级别 | 何时写 |
| --- | --- | --- |
| `额度 429，换号重试 \| 额度失败=<n>/4 \| 类型=<冷却类型> \| 原因=…` | INFO | 每日类额度失败后换号 |
| `限流 429，换号重试（只用已启动的 Worker）\| 限流失败=<n>/2 \| 类型=限流 \| 额度失败=<n>/4 \| 原因=…` | INFO | 限流类额度失败后换号（出口正常） |
| `限流 429，经出口限速换号重试 \| 限流失败=<n>/2 \| 出口=<标签> \| 限速=<L>/分钟 \| 原因=…` | INFO | 所在出口正在限流且限速生效：这次换号经出口令牌桶排队，不再抖动 |
| `限流 429，同账号换通道重试 \| 限流失败=<n>/2 \| …` | INFO | 冷却只写在本通道、同账号另一通道可用（出口限流时不再这样探测） |
| `换号上限，立即返回 429 \| 原因=<原因> \| 额度失败=<n> \| 限流失败=<m> \| 尝试账号=<k> \| 总耗时=<d> \| 建议重试=<d>` | WARN | 普通号池以额度不足结束：某类失败达到上限、额度失败后没有已启动的 Worker、其余候选全部冷却或没有其他账号 |
| `上游出口限流，换号已达上限，立即返回 429 \| 出口=<标签> \| 限速=<L>/分钟 \| 限流失败=<m> \| 尝试账号=<k> \| 总耗时=<d> \| 建议重试=<d>` | WARN | 限流类达到上限时所在出口正在限流（限速已停止时写 `限速=已停止`） |
| `已尝试 <n> 个账号，不再换号 \| 原因=…` | WARN | 达到每请求 8 个账号的硬上限 |
| `登录失效换号已达上限，不再换号 \| 登录失效=<n>/3 \| 原因=…` | WARN | 一个请求第 3 次遇到登录失效（一批账号同时 401 时不把号池逐个试一遍），按原错误结束（对外 503） |

普通号池的额度不足 `request.error` 以 `额度不足：<号池的>模型 <ID> 换号重试已达上限，立即返回（…）` 或 `上游限流：<号池的>模型 <ID> 所在出口 <标签> 正在限流，立即返回（…）` 开头，不含“主用模型”字样；对外仍是 429 与固定的限流文案，`Retry-After` 为 `建议重试`（限流类上限、没有已启动的 Worker、硬上限为 5 秒，出口限流时按出口的排队长度估计，限定在 5 到 60 秒）。CountTokens、Live、转写、Veo 这些不固定账户的数据请求最多试 4 个账户。

### 出口限流

按出口（账号代理规范化后的值，空为直连）判定：2 分钟内裸 429 不少于 10 次、来自不少于 5 个账号，并且“比例 ≥ 0.5 且冷号首发被拒 ≥ 3”或“比例 ≥ 0.8 且 ≥ 20 个账号”时判定成立。冷号首发：本进程本额度日内这个账号此前没有任何结果、第一次就被裸 429；已标为坏号（见[坏号与冷启动节奏](#坏号与冷启动节奏)）的账号不计。判定成立期间裸 429 只冷却 10 秒、不升档，冷启动只做低于常驻数的补位与补覆盖，按需替换与扩容暂停，目录同步与登录复核暂停，账户池按出口限速发送（主用模型优先，非主用保底 25%）。以下全部是持久行：

| 日志 | 级别 | 含义 |
| --- | --- | --- |
| `出口限流判定成立 \| 出口=<标签> \| 窗口=2m \| 账号=<D> \| 裸限流=<B> \| 尝试=<T> \| 比例=<R%> \| 冷号首发被拒=<F> \| 条件=冷号首发\|高比例 \| 初始限速=<L>/分钟 \| 下限=<f>/分钟` | WARN | 判定成立，普通通道按最近 60 秒成功 ×1.25 启用令牌桶 |
| `出口限速调整 \| 出口=<标签> \| 通道=普通\|Ultra \| 限速=<A>→<B>/分钟 \| 原因=降速\|降速回退\|快速提速\|提速\|启用 \| 30秒裸限流=<n> \| 30秒成功=<s> \| 30秒发送=<k> \| 排队30秒=<q>` | INFO/WARN | AIMD：30 秒内裸 429 ≥3 且比例 ≥0.2 时 ×0.7；降速满 60 秒成功数少了 20% 以上就回到原值并锁定 10 分钟；没有裸 429 时每 15 秒 ×1.5 |
| `出口限速无效，停止限速 \| 出口=<标签> \| 通道=… \| 进入时比例=<a%> \| 限速后比例=<b%> \| 成功1分钟=<s> \| 限流类换号放宽到 4 次` | WARN | 限速 3 分钟、一直在起作用，比例却没改善 0.2：拒绝与发送速率无关，停止限速，其余保护保留 |
| `出口几乎完全被拒 \| 出口=<标签> \| 限速=<L>/分钟（下限） \| 10分钟比例=<r%> \| 10分钟成功=<s> \| 判定保持，不交回逐级冷却` | WARN | 停在下限 10 分钟仍 ≥90% 被拒，只告警（同出口 10 分钟一条） |
| `出口限流：撤回逐级冷却 \| 出口=<标签> \| 账号=<n> \| 失败=<m> \| 耗时=<d>` | INFO | 判定成立时撤回第 2 档以上的逐级冷却（一次性） |
| `出口限流：账号按账号级失败冷却 \| 出口=<标签> \| 新增=<n> \| 合计=<m> \| 门槛=<k>次 \| 兜底=<b>` | WARN | 判定期间连续被拒、与出口按比例拒绝区分得开的账号写 30 分钟账号冷却（同出口每分钟汇总一条） |
| `出口限流暂停冷启动 \| 出口=<标签> \| 通道=… \| 路径=替换\|超出常驻\|补覆盖配额已满\|补位并发已满 \| 10分钟内跳过=<n>` | INFO | 冷启动闸门拦下的路径（同出口同路径 10 分钟一条） |
| `出口每分钟请求已满 \| 出口=<标签> \| 通道=… \| 限速=<L>/分钟 \| 令牌释放前新请求按“出口限速”排队` | INFO | 令牌桶满（同出口 10 分钟一条） |
| `出口限流解除 \| 出口=<标签> \| 持续=<d> \| 最终限速=<L>/分钟 \| 期间成功=<s> \| 期间裸限流=<b> \| 复试账号=<t> \| 恢复账号=<r> \| 降级账号=<x>` | INFO | 持续 ≥3 分钟、2 分钟比例 <5%、限速连续 2 分钟不起作用时解除 |
| `出口限流判定撤销 \| 出口=<标签> \| 原因=复试账号零成功\|管理操作 \| 持续=<d> \| 30分钟内不再判定` | WARN | 安全阀 A 或管理页“撤销判定”：交回逐级冷却，30 分钟内不再判定 |
| `出口限流判定恢复 \| 出口=<标签> \| 原因=管理操作\|撤销到期` | INFO | 管理页“恢复判定”或撤销到期 |

判定期间的额度冷却日志在 `账号冷却 | 类型=限流 | …` 后追加 ` | 出口限流不升档 | 出口=<标签>`，按账号降级的追加 ` | 出口限流按账号冷却 | 连续=<N> | 出口=<标签>`。只因出口令牌桶已满而排队超限的 `request.error` 以 `出口限速：模型 <ID> 所在出口每分钟请求已满，请稍后重试（…）` 开头，对外 429。`WAA Worker 就绪` 末尾带 ` | 来源=预热|替补|扩容|按需|补覆盖|重开`，`按需扩容`、`按需替换` 无条件写并带 ` | 超出常驻=是|否`。转写、Live、Veo 不经生成的选号：判定成立期间还有热 Worker 时，它们的候选去掉受限出口上的备用账户，只在热 Worker 与未受限出口的备用账户里选；选中的账号要冷启动、被冷启动闸门暂停时按号池繁忙 429 返回（最后等待原因 `出口限流中，冷启动按配额进行`，`Retry-After` 5 秒），指定账户的生成请求同样。

### 模型目录缓存与后台同步

目录保存在 `runtime/model-catalogs/`：`index.json`（账户 → 目录 blob 与同步状态）与按内容去重的 `blobs/`，最多每 30 秒写一次，原子写、跨进程加锁。后台只刷新缺失、恢复时不一致、疑似登录失效、失败待重试与过期的账户，并且只取空闲账户：常规 2 并发、两次开始至少间隔 1 秒，完全没有缓存时 4 并发、间隔 500 毫秒；上游 429 全局暂停 30 秒起翻倍到 30 分钟，出口限流时该出口的账户推后，其他失败按账户退避 1 分钟起到 6 小时；空目录在账户已有目录时不覆盖，连续 3 次且跨 1 小时才接受。

| 日志 | 级别 | 含义 |
| --- | --- | --- |
| `模型目录缓存载入 \| 账户=<n> \| 目录=<blob 数> \| 耗时=<d> \| 跳过=权益不符 <a>、文件缺失 <b>、版本不符 <c>、已删除账户 <d>` | INFO | 启动时一次恢复 |
| `模型目录缓存损坏 \| 文件=<path> \| 已改名=<path>.corrupt-<unix> \| 错误=<err>` | WARN | index 解析失败，改名保留后按空缓存继续（持久） |
| `模型目录缓存读取失败 \| 目录=<dir> \| 错误=<err>` | WARN | 打开缓存时读不了 index（权限、跨进程锁等），按空缓存继续 |
| `模型目录缓存写入失败 \| 错误=<err>` | WARN | 写盘失败（blob 或 index），变化留在内存，下个间隔重试；停止服务、关闭运行时前的写盘失败同样写这一行 |
| `模型目录缓存版本不符，不写盘 \| 文件=<path>/index.json` | WARN | 磁盘上是另一个版本的 index（例如回退过旧版本）：不覆盖也不删，本进程的变化只留在内存；只提示一次 |
| `模型目录后台同步 \| 成功=<s> \| 失败=<f> \| 限流429=<q> \| 空目录=<e> \| 账户忙推后=<b> \| 缺失=<a> \| 过期=<x> \| 疑似失效=<p> \| 队列=<n> \| 暂停到=<RFC3339 或 -> \| 原因=<上游 429\|出口限流\|->` | INFO | 每 5 分钟汇总 |
| `模型目录同步暂停 \| 原因=上游 429 \| 暂停=<d> \| 连续=<k> \| 错误=<摘要>` | WARN | 全局暂停（持久） |
| `模型目录同步恢复 \| 暂停=<d>` | INFO | 暂停结束（持久） |
| `模型目录同步失败 \| 错误=<err> \| 下次=<d> \| 连续失败=<f>` | WARN | 同账户只在第 1、2、4、8… 次失败时写 |
| `模型目录为空，保留原目录 \| 模型=<n> \| 连续空结果=<k> \| 首次为空=<d> 前` | WARN | 已有目录时空结果不覆盖 |
| `模型目录为空，已接受 \| 连续空结果=<k> \| 首次为空=<d> 前` | WARN | 连续 3 次且跨 1 小时后才接受 |

有缓存以后重启不再清掉“需要登录”与账号全局冷却：原先启动全量同步顺带把它们清了（所以“重启就好”），现在由下面的登录复核恢复。后台刷新目录成功也不清账号的全局冷却（全局每日限额、403 自动暂停、出口限流期间的账号冷却都到期自然恢复）；管理页刷新目录与验证照旧清。删除的账户超过 7 天的条目在每小时一次的清理里从 index 与内存一并删掉。

### 不可调度驻留 Worker 的释放

驻留 Worker 的账号需要登录、已停用、不可用，或目录加载过却没有可初始化的模型时，它不是任何模型的候选，却占着驻留槽位。确认需要登录后立即登记释放，`keepWarm` 每 5 秒补登记其他路径造成的；空闲即关，忙碌等请求结束（不排空、不要求可接替账户），槽位由预热补上。

| 日志 | 级别 | 含义 |
| --- | --- | --- |
| `WAA Worker 释放 \| 原因=需要登录\|组织已停用\|已停用\|不可用\|没有模型目录\|不可调度 \| 登记后=<d> \| Worker=<n>/<max>`（Ultra 分区为 `Ultra Worker=`） | INFO | 关闭了一个不可调度的驻留 Worker |
| `WAA Worker 补登记释放 \| 新增=<n> \| 驻留中不可调度=<k>/<warm> \| 原因=需要登录 <a>、已停用 <b>、没有模型目录 <c>` | INFO | keepWarm 补登记（持久） |
| `WAA Worker 释放失败 \| 原因=… \| 错误=<err>` | WARN | 关闭失败，后台重试清理 |
| `账号需要登录 \| 来源=生成\|流内\|续签重放\|启动\|目录同步\|管理验证\|其他请求 \| 出口=<标签> \| 出口限流中=是\|否 \| 错误=<摘要>[ \| 复核=仍失效]` | WARN | 账号被标为需要登录（持久，同账号 1 分钟最多一条）；经登录复核确认的末尾带 `复核=仍失效`，`来源` 为最初那次 401 的路径 |

### 登录失效的复核与恢复

一次上游 401 不再直接判死：只有用 Cookie 认证的 MakerSuite RPC（生成、目录、CountTokens 等）返回 HTTP 401 / 协议错误码 16 才算登录失效的证据；`Drive*` 的 Bearer 401 不算；`GenerateAccessToken`（带图请求换 Drive token）的 401 / 协议错误码 16 表示账号没有 Drive 授权：附件改为内联发送（写 `Drive 未授权，附件改为内联`），不写冷却、不改账户状态、不登记淘汰（原先写 2 分钟模型无关的全局冷却，原因以 `附件授权失败` 开头）。登录页跳转、签名 Cookie 缺失这类本地证据照常直接标记。运行中租约路径上的上游 401 先记“疑似登录失效”：写 5 分钟全局冷却，用磁盘 Cookie 经 Go HTTP 发一次 `GetAiStudioBenefitTier`（不续签、不重放）复核，仍是 401 才标需要登录，通过则解除冷却。需要登录的账户每 15 分钟一轮、每轮最多 50 个低速复核（与疑似复核共用并发 2、每秒最多 1 个），复核仍失效的按账户退避 15 分钟起翻倍到 6 小时，复核通过即恢复就绪并放回目录同步队列。定期复核只复核按接口证据（上游 401，含管理验证里“浏览器已登录、接口 401”）标记的账户：管理验证判定未登录、Worker 启动跳到登录页是浏览器证据，接口用同一份 Cookie 通过不说明浏览器能登录，这些账户等重新登录、认证文件更新或管理验证通过。浏览器登录、认证文件更新、管理验证通过三条人工恢复路径同样清掉疑似失效、登录复核记录与疑似登录失效冷却。出口限流判定成立期间不复核（疑似账号保持短冷却）。非 Windows 平台不再尝试原地续签（chromeauth 只支持 Windows），有续签材料也按没有处理。

| 日志 | 级别 | 含义 |
| --- | --- | --- |
| `账号疑似登录失效 \| 来源=生成\|目录同步\|其他请求 \| 出口=<标签> \| 冷却=5m0s \| 复核=排队 \| 错误=<摘要>` | WARN | 上游 401，排队复核 |
| `疑似登录失效复核通过 \| 来源=… \| 疑似=<d> \| 接口认证正常，已解除冷却` | INFO | 复核通过，账号照常可调度 |
| `疑似登录失效复核未完成，不标记 \| 来源=… \| 复核=<n> 次 \| 错误=<摘要>` | WARN | 复核一直出错（上游 429、连接失败），不标记，冷却自行到期 |
| `登录复核恢复 \| 复核次数=<n> \| 耗时=<d> \| 接口认证正常，账号恢复就绪` | INFO | 需要登录的账户复核通过 |
| `登录复核 \| 恢复=<N> \| 仍失效=<M> \| 跳过=<K> \| 下一轮=<d> \| 疑似排除=<a> \| 疑似确认=<b> \| 待复核疑似=<c> \| 浏览器判定失效不复核=<e>` | INFO | 每轮汇总（持久，没有任何结果时不写）：本轮定期队列做完时一条，期间疑似账户的结果累计到下一次轮次边界（每 15 分钟最多两条）；`跳过` 为账户忙、出口限流或复核出错；`浏览器判定失效不复核` 为本轮开始时因浏览器证据不复核的需要登录账户 |
| `账户验证完成 \| 已认证=true\|false \| 耗时=<d> \| 接口复核=通过\|401\|出错\|跳过 \| 页面=<host+path 或 ->` | INFO | 管理验证的结论：浏览器已登录时用接口复核一次，401 时结论改为未认证；浏览器判定未登录时 `页面` 为跳转到的登录页（不含查询参数） |

管理验证的接口返回带 `authenticated` 与 `verify_reason`：浏览器判定未登录时原因带跳转页，如 `AI Studio 登录已失效（跳转到 accounts.google.com/v3/signin/identifier）`（`speedbump`、`challenge`、`rejected` 等路径多为安全验证或风控）；浏览器已登录、接口复核 401 时为 `浏览器已登录，但接口认证失败：Cookie 可能已被 Google 作废，需要重新登录`，目录保留。验证不导出浏览器 Cookie（要改隔离浏览器的验证流程），这样的账号需要重新登录。Worker 启动完成后会立即把浏览器 Cookie 按 (name, domain, path) 合并写回认证文件，Go 通道随后用的是浏览器轮换过的 Cookie；AI Studio 页面的 localStorage 同时写回（见 [Worker 生命周期](#worker-生命周期)）。状态接口 `accounts.auth_required_10m` 为最近 10 分钟新被标为需要登录的账户数，`auth_recheck` 为复核的计数。

### 坏号与冷启动节奏

账号（不分模型）连续 2 次裸限流、期间没有成功就标为坏号：2 小时内移出预热、扩容（含按需提升）与替补的候选（还有其他候选时），成功一次即清除；预热与替补先按能服务的主用模型个数、其次热门模型个数排，个数相同时最近 24 小时成功过的账号在前：近期成功过、对主用模型冷却（如单模型 403 无权限；剩余不到 2 分钟的短冷却不算）的账号排在能服务主用模型的账号之后；请求为其他模型按需扩容、现场启动备用账号时同样先比主用模型、再比热门模型（只去坏号，不看近期成功）。记录写在 `runtime/model-catalogs/bad-accounts.json`，重启后保留；出口判定的“冷号首发被拒”不计坏号。按需替换与扩容不换就绪不到 2 分钟的 Worker（不可调度与死槽位除外），每个分区每分钟最多 10 次按需替换，超过时排队等待；同分区有 Worker 在启动时，按需替换也可以换掉对请求模型不可服务、或空闲超过 2 分钟的健康 Worker（与现场提升的判断同一口径，不再出现“Worker 容量已满”）。主用模型等这次选号发起的后台扩容时，等待的时间与自己的现场冷启动一样延后排队截止时间（合计不超过 `INIT_TIMEOUT`）。

| 日志 | 级别 | 含义 |
| --- | --- | --- |
| `账号连续裸限流，移出预热与替补候选 \| 连续=<n> \| 时长=2h0m0s \| 恢复=<RFC3339>` | WARN | 标为坏号；状态接口 `bad_accounts` 为现在的坏号数 |

### 429 的原因响应头

对外的 429 文案与状态码不变（兼容中转与客户端），另加响应头 `X-AIStudio2API-Reason`：`local_queue`（本地排队超限，请求多半没有发往上游）、`local_cooling`（候选全部冷却，或租约账号在发出前进入冷却）、`upstream_quota`（上游额度或限流 429，含换号上限）、`egress_limited`（出口限速排队超限，所在出口正在限流、换号已达上限，或出口限流期间要冷启动的 Worker 被冷启动闸门暂停）。四种协议（OpenAI Chat/Responses、Anthropic、Gemini、Interactions）的流式与非流式 429 都带。

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
| `内联附件处理完成` | 本次请求的附件数量、原始字节数与处理耗时：附件已上传为临时 Drive 文件（Build 通道直接携带附件，同样写这一行） |
| `Drive 未授权，附件改为内联` | `Drive 未授权，附件改为内联 \| 附件=<N> \| 原始=<B>B \| 耗时=<d>`（INFO）：Playground 通道上账号没有 Drive 授权（`GenerateAccessToken` 返回 401 / 协议错误码 16，或 6 小时内记着它返回过，这时不换 token、耗时接近 0），附件随生成请求内联发送，不冷却、不换号；同账号 10 分钟最多一行，走独立的控制台队列（`logDurable`），普通日志挤满控制台队列时也不丢。写这一行时不写 `内联附件处理完成`，诊断按开头分开计数 |
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

## 守护日志

`./start.sh enable` 后 cron 每分钟执行一次 `./start.sh watchdog`（判定规则见 [README](../README.md#方式三linux-服务器后台运行startsh)），记录写在 `logs/watchdog.log`，与程序的 JSON 日志分开；超过 5MB 时改名为 `watchdog.log.1`（只保留一个）。进程正常、处于人为停止状态或锁被手动命令占着时不写任何内容。每条记录一行 `时间 [级别] 内容`，附加内容以 `    | ` 开头写在其后几行；`./start.sh status` 显示最近 3 条记录。

| 记录 | 含义 |
| --- | --- |
| `[WARN] 进程不在运行：<原因>` | 原因为 `PID 文件里的进程 N 已不存在`（附 `被系统 OOM 结束`、`系统重启过（PID 文件早于本次开机）`，或 `（读不到内核日志，无法判断是否被 OOM 结束）`）、`PID 文件里的进程 N 已不是 aistudio2api（PID 已被其他程序复用）`、`PID 文件里的进程 N 是其他目录的 aistudio2api（PID 已被复用）`（工作目录不是本目录）、`没有 PID 文件`。其后附主日志里最近一次 start.sh 分隔行之后整段（即退出的这个进程的输出）的线索：第一行 `panic: `、`fatal error: `、`runtime: `、`SIGxxx: ` 或 `unexpected fault` 及其后 7 行（fatal error 打印全部 goroutine 栈时也不会丢；没有这类行时取最后一条 JSON 之后的开头 8 行），最后 20 行中的非 JSON 行，最后一条 `level=ERROR` 的时间与 `msg`、`error` 字段（各前 200 字，以 `：` 连接；启动失败时具体原因在 `error` 里），`这个进程在主日志里最后一条记录的时间`，以及推断的退出方式：`进程收到停止信号（SIGTERM 或 Ctrl+C）后退出…`（主日志里有“收到停止信号”：被打断的 restart、手动 kill 或其他程序），或 `主日志里没有崩溃输出：多半被 SIGKILL 直接结束…`（没有非 JSON 行，也不是“AIStudio2API 启动失败”） |
| `[INFO] 已自动拉起（PID N，打开文件数上限 M，30 分钟内第 K 次）` | 拉起成功；主日志同时写分隔行 `===== 时间 start.sh watchdog 自动拉起：<原因> =====`。降频期间的拉起另注明 `降频中` |
| `[WARN] 自动拉起失败：进程启动后立即退出（PID N，…）` | 下一轮的“进程不在运行”记录附有这次退出前的输出 |
| `[INFO] 健康检查失败 第 N/5 次（PID，http://地址/health 原因）` | 原因为 `10 秒内无响应`、`连接被拒绝`、`返回错误状态码` 等；成功一次写 `健康检查恢复（…此前连续失败 N 次），计数清零`。上次检查已超过 150 秒（守护停用过、cron 中断）时旧计数作废，从第 1 次重新计 |
| `[WARN] 判定卡死：健康检查连续 5 次失败（…），重启（PID N）` | 其后附 stop 的输出与这个进程的主日志线索，再是拉起结果。SIGTERM 后 `STOP_TIMEOUT` 秒内没有退出时发 SIGQUIT（`发送 SIGQUIT（goroutine 栈写进主日志…）`），Go 运行时把全部 goroutine 的栈写进主日志后退出；附加内容里是 `SIGQUIT 时共 N 个 goroutine，按等待原因与第一个非标准库函数计数（前 8 项）`，例如 `120  sync.Mutex.Lock  github.com/…/internal/app.(*runtimeManager).status`，完整的栈在主日志这个进程最后一段的 `SIGQUIT: quit` 之后；5 秒后仍未退出才强制结束 |
| `[WARN] 旧进程 N 强制结束后仍未退出，本轮不拉起，下一轮再检查` | 卡死重启时 stop 之后旧进程还在，不拉起新进程，免得两个实例同时运行 |
| `[INFO] 判定卡死（…），但守护处于降频中，暂不重启，最早 HH:MM:SS 重启（PID N）` | 降频期间每次推迟只记一条；失败计数停在阈值，不再继续累加 |
| `[WARN] 找不到本目录的进程（<原因>），但监听地址 <地址> 上已有程序应答 /health，找不到它的 PID，不拉起…` | PID 文件丢失或程序经另一种路径启动；不拉起第二个实例，每小时最多记一次 |
| `[INFO] 健康检查：PID N 还没有在 <地址> 应答过 /health，不做卡死判定` | 每个进程最多一次；可能是启动卡在开始监听之前（例如首次下载 Camoufox），或 `.env` 的 `LISTEN_ADDR` 与运行中的程序不一致。探测地址优先取运行中进程启动时环境里的 `LISTEN_ADDR` |
| `[WARN] 连续崩溃，降低拉起频率：30 分钟内已自动拉起 5 次，…` | 之后每 10 分钟最多拉起一次，30 分钟内没有再自动拉起时恢复（`./start.sh status` 也不再显示降频）；`./start.sh start`、`restart` 清空计数 |
| `[WARN] 运行中进程的环境里有这些配置不在 .env 里或与 .env 不同：…` | 卡死重启前检查，只列配置名，不写取值 |
| `[WARN] 运行中进程的环境里这些变量与守护拉起时不同：…` | 同上，针对程序只从进程环境读取的 `CAMOUFOX_PATH`、`SHUTDOWN_GRACE`、语言、`TZ` 与代理变量：应写进 `run/watchdog.conf` |
| 附加行 `[INFO] 进程环境里没有、按 …/run/watchdog.conf 补上：CAMOUFOX_PATH …` | 拉起记录前的附加内容：从 `run/watchdog.conf` 补上的变量（只列名字）；`按系统默认补上 LANG=…` 为语言变量都没有时按系统默认语言补上 |
| `[WARN] 进程不在运行：…；没有可执行文件 …，守护不构建也不拉起` | 同样的情况每小时最多记一次 |
| `[WARN] 未找到 flock 命令（util-linux），改用目录锁 …/run/start.lock.d…` | 只记一次；之后守护与 start/stop/restart 用目录锁互斥 |
| `[WARN] 缺少 ps/pgrep（procps），无法判断进程是否在运行，守护不做任何动作…` | 每小时最多记一次；装上 procps 后恢复 |

附加内容先打码再截短（每行最多 300 字）：邮箱只留前两位（`ac***@***`），`scheme://user:pass@`、`user:pass@host` 与 `host:port:user:pass` 中的账号密码（密码一直打码到这一段里最后一个 `@`，密码里有 `/`、`@`、`,` 也不会漏出）、`sk-` 与 `AIza` 开头的密钥、`Bearer`/`Basic` 凭据（含 `Proxy-Authorization`）、Google 登录 Cookie（`SAPISID`、`__Secure-*` 等）、查询参数里的 `key=`/`token=`，`password=`、`token=`、`x-goog-api-key:`、`x-api-key:` 一类键值，以及 `.admin-token`、`ADMIN_PASSWORD`、`PROXY_API_KEY` 的原值都替换为 `***`，规则与 `tools/report.sh` 一致。

守护用到的文件都在 `run/`：`watchdog.conf`（可选，用户自己建，格式同 `.env`，程序不会改写：`WATCHDOG_HEALTH_FAILS`、`WATCHDOG_GRACE` 优先于 `.env`；`CAMOUFOX_PATH`、`SHUTDOWN_GRACE`、`LANG`/`LC_ALL`/`LC_MESSAGES`、`TZ`、`http_proxy` 等代理变量在拉起程序时进程环境里没有才补上）、`stopped`（人为停止标记，内容为时间与 `用户执行 stop`；`restart` 不写）、`start.lock`（start/stop/restart 与守护共用的锁；没有 flock 时改为目录 `start.lock.d`，里面的 `pid` 是持锁的 start.sh）、`watchdog.health`（PID、探测地址、是否应答过、连续失败次数、这次检查的时间）、`watchdog.launches`（30 分钟内每次自动拉起的时间）、`watchdog.throttle`（降频中）、`watchdog.deferred`（降频中推迟卡死重启的记录）、`watchdog.nobin`、`watchdog.orphan` 与 `watchdog.noprocps`（“没有可执行文件”“监听地址上已有程序”“缺少 ps/pgrep”每小时最多记一次）、`watchdog.noflock`（“未找到 flock”只记一次）。
