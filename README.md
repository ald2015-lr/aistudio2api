<div align="center">

# AI Studio to OpenAI, Anthropic & Gemini Compatible API

<p align="center">
  <a href="README.md"><b>中文</b></a>
  &nbsp;|&nbsp;
  <a href="README_en.md">English</a>
</p>

<p>
  <b>一个基于 Go 的高性能代理服务</b><br>
  将 Google AI Studio 网页协议转换为 OpenAI、Responses、Anthropic 和 Gemini 兼容 API
</p>

<p>
  Playground + Build 双额度通道 &nbsp;•&nbsp;
  多账户高并发 &nbsp;•&nbsp;
  Camoufox 与纯 Go 双 WAA 后端<br>
  Claude Code、Codex 等 agent 客户端 &nbsp;•&nbsp;
  Nano Banana、Veo、TTS 与 Omni
</p>

</div>

---

## 核心能力

- **双额度通道**: 每个账户同时拥有 Playground 与 Build 应用代理两份独立额度，`UPSTREAM_CHANNELS` 可单独或同时启用；一个通道触发限额后，同一账户由另一个通道继续
- **多账户高并发**: 识别 Free、Pro、Ultra 与 Plus 权益，按实时模型目录在账户间轮询或优先复用
- **Ultra 独立号池**: Ultra 账户经 `/ultra` 前缀单独调用（如 `http://127.0.0.1:2048/ultra/v1`），默认独占，Worker 常驻与峰值数单独设置；可选让 Pro 账户一起并入（`ULTRA_INCLUDE_PRO`），组成付费号池
- **两种 WAA 后端**: 默认由 Camoufox 持有官方 WAA 生命周期；设置 `WAA_BACKEND=go` 后由纯 Go 生成官方 proof，运行时不下载、不启动浏览器
- **四套 API 协议**: OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 与 Gemini GenerateContent，另支持 Gemini Interactions
- **主流 agent 客户端**: 支持 Claude Code、Codex、OpenCode、pi、omp、OpenClaw、Hermes 的文件读写工具调用，Claude Code、Codex、omp 的原生联网搜索可直接使用
- **四协议工具选择**: 必须调用、指定函数、单次调用、`strict` 参数校验、Gemini `VALIDATED` 与 Anthropic `thinking.disabled`；Playground 无法编码的工具 Schema（`$ref`、`uniqueItems` 等）降级编码并把完整 Schema 交给模型

## 特性

- **原生流式响应**: 实时输出正文、思考摘要、函数调用、Google 工具、媒体和 usage
- **TTS 语音生成**: 支持 Gemini TTS 模型的单/多说话人音频生成
- **图片生成**: 支持 Nano Banana 图片生成
- **视频生成**: 支持 Veo 视频生成和图片转视频；Gemini Omni 通过四套生成接口接收文本、图片与视频输入，输出文本与 MP4 视频
- **YouTube 输入**: 粘贴视频 URL 即可作为外部视频附件读取
- **智能模型切换**: 从 AI Studio 实时发现模型并按 `model` 字段路由
- **Google 工具**: 支持 Search、Image Search、URL Context、Code Execution 和 Maps
- **Files 与 Transcribe**: 支持文件上传、查询、内容读取、删除和音频转录
- **Live 与 Robotics**: 通过 WebSocket 支持文本、音频、JPEG、媒体结束、工具调用、恢复和中断
- **反指纹检测**: 使用 Camoufox 持有官方 WAA 生命周期，并为每个账户固定浏览器指纹与出口
- **图形界面启动器**: 通过网页管理账户、服务启停、实时日志、模型、请求和配置
- **用量看板**: 本地 SQLite 账本统计请求数、成功率、Token、耗时分位、降级拦截率与重复回复率，支持筛选、排行、请求记录与 CSV 导出
- **模块化架构**: Go 负责协议、调度、API 与管理端，Camoufox 负责 WAA 运行时和隔离登录

## 系统要求

- **Windows Release 运行**: Windows 10 或更高版本、`aistudio2api.exe` 和 `start.bat`
- **Linux Release 运行**: 解压 `linux-amd64.tar.gz` 后运行 `./aistudio2api`，Camoufox 需要 Firefox 系运行库，Debian/Ubuntu 执行 `sudo apt install libgtk-3-0 libasound2 libnss3 libdbus-glib-1-2 libxtst6 libxrandr2 libgbm1 libxkbcommon0 libpango-1.0-0 libcairo2 libxcomposite1 libxdamage1 libxfixes3 fonts-liberation`
- **源码运行**: Go 1.25.0+、Node.js 22.13+ 或 24+，以及配套 npm
- **操作系统**: Windows、macOS、Linux
- **内存**: 单账户建议 2GB+ 可用内存，每个常驻预热账户约增加 0.6GB
- **网络**: 稳定的互联网连接访问 Google AI Studio

## 安装步骤

### 方式一：Windows 一键启动（推荐）

从 [Releases](https://github.com/ald2015-lr/aistudio2api/releases) 下载 `windows-amd64.zip` 发布包，解压后运行 `start.bat`。发布包已包含管理界面，可直接运行。

从源码启动时：

```powershell
git clone https://github.com/Mag1cFall/AIStudio2API.git
cd AIStudio2API
copy .env.example .env
```

然后双击运行 `start.bat`。Windows PowerShell 也可以直接执行：

```powershell
.\start.bat
```

已有 `aistudio2api.exe` 时脚本立即运行；源码目录缺少可执行文件时，脚本自动安装前端依赖并构建前端与 Go 程序。

首次启动会自动下载当前平台的 Camoufox 到 `runtime/camoufox/`。也可以通过环境变量 `CAMOUFOX_PATH` 指定已有可执行文件。

### 方式二：Linux 与 macOS 源码构建

#### 1. 安装依赖

- Go 1.25.0 或更高版本
- Node.js 22.13+ 或 24+，以及配套 npm

#### 2. 克隆项目

```bash
git clone https://github.com/Mag1cFall/AIStudio2API.git
cd AIStudio2API
cp .env.example .env
```

#### 3. 构建并运行

```bash
cd web
npm ci
npm run build
cd ..
go build -o aistudio2api ./cmd/aistudio2api
chmod +x ./aistudio2api
./aistudio2api
```

Linux 与 macOS 首次运行同样会自动准备对应平台的 Camoufox。

### 方式三：Linux 服务器后台运行（start.sh）

`start.sh` 以后台进程管理服务，缺少可执行文件时自动构建（需要 Go 1.25+、Node.js 22.13+ 或 24+），构建网络不通时可加 `USE_CN_MIRROR=1`：

```bash
chmod +x start.sh
./start.sh start      # 启动并等待生成服务进入 RUNNING
./start.sh restart    # 重启，修改 .env 后执行
./start.sh stop       # 停止
./start.sh status     # 进程、生成服务与账户状态
./start.sh log        # 跟踪日志 logs/aistudio2api.log
./start.sh build      # 重新构建，restart 后生效
./start.sh enable     # 开机自启（crontab @reboot），disable 取消
```

`AUTO_START` 默认为 `true`，进程启动后自动启动生成服务，无需在管理页面点击。

Gemini 原生接口非流式响应：连续的正文与连续的思考各合并为一个 part（与 Gemini API 一致），不论各分片是否带思考签名；同一段里的多个签名（gemini-4 等模型给许多正文分片都带签名）只保留最后一个，正文或思考中途单独到达的签名同样挂到这个 part 上。输出开头的签名挂到随后第一个 part 上（该 part 合并后已带签名时前置签名单独成 part，正文不会因此拆开）；工具调用、媒体之后等无处可挂的签名（流式同样）以空思考 part `{"text":"","thought":true,"thoughtSignature":"..."}` 返回，回传时还原为纯签名。引用与搜索来源不打断正文合并。此前按上游增量或按签名逐段生成 part，new-api 用换行、SillyTavern 用空行拼接多个 part，会导致句子中间断行、数字被拆开、出现多余空行。Anthropic 与 Interactions 接口在正文进行中收到只带签名的分片时，先暂存到这段正文结束再输出 `redacted_thinking` 块或 thought 步骤，正文保持一个文本块或一个 `model_output` 步骤；随后是不带签名的工具调用时排在调用之后，回传时不会挂到该调用上。

随机种子：本服务不缓存响应、也不合并相同请求；客户端传入的 seed 会原样转给上游，固定 seed 会让相同提示词得到几乎相同的回复。客户端没有传 seed（或已按配置忽略）时不发送 seed，与网页版一致；生成参数（温度、top_p、top_k、最大输出、思考强度）都按客户端传入的值，没传的项用模型目录里的默认值（与网页版相同）。避免相同请求得到相同回复靠的是随机后缀。Playground 请求的字段与网页版一致，不再附带时区。请求日志参数中显示 seed（未传为“默认”）；服务配置中的“忽略客户端传入的 seed”（IGNORE_CLIENT_SEED，默认关闭，可热更新）开启后丢弃客户端 seed、不发给上游，日志中标注“（已忽略）”。

冷却记录与每日额度通道判定：冷却保存在各账户的 runtime-state.json，重启后自动恢复，到期自动解除。同一账户 Build 与 Playground 是否共用某模型的每日额度，从实际请求中学习——一个通道达到每日限额后，看该账户在另一个通道上的下一次尝试（只计在限额之后才发起的尝试）：成功记为独立，同样达到每日限额记为共用；共用证据至少 8 次且独立占比低于约 5% 时判定为共用，之后一个通道达到每日限额就同时冷却另一个通道，不再让请求去另一个通道失败一次再换号。判定为共用后，每 20 次每日限额仍放行一次另一个通道作为复核，额度后来变为独立时判定随之修正；判定按账户真实权益分开学习，Ultra 账户只按 Ultra 账户的证据判定，`ULTRA_INCLUDE_PRO=true` 时并入 Ultra 号池的 Pro 账户仍按 Pro 计入其余权益的判定（Pro 的额度与 Ultra 不同，切换开关也不改变已学到的结论）；判定记录保存在账户目录的 .quota-sharing.json，重启后继续有效；`./start.sh diag` 会显示磁盘上保存的有效冷却条数与判定结论。

浏览器轮询：受保护请求等待响应头与读取响应数据时采用自适应间隔，有新数据时 10ms，连续没有数据时逐步放宽到 100ms（此前固定 10ms，请求等待上游思考的几秒里会向浏览器发出数百条空命令，高并发时占用大量 CPU）。诊断脚本会检查主程序打开的文件数与系统线程数是否接近上限。

高并发调度：获取账户 Worker 时先在不持有全局 rebalanceMu 的情况下检查现成 Worker；持有 rebalanceMu 时不再等待单个账户的锁（该锁在生成 WAA proof 时会被占用数秒，原先会让全系统请求一起排队）。账户候选按 ID 排序的结果缓存到账户增删时才重建；账户“是否支持某模型”与 Build 可生成模型改为按账户目录索引查找，目录替换时自动重建。

Build 独有模型计算：Playground 目录按模型 ID 与别名建立索引，账户增删或目录变化时才重建；判断 Build 独有模型与账户 Build 通道是否支持某模型改为查索引。此前每次重算都要对每个启用账户、每个 Build 模型扫描全部账户的全部模型，计算量随账户数平方增长并全程占用账户池锁，账户数千时每次要一两秒，期间所有请求都在等锁。

预热池冷却轮换：每 30 秒检查一次，某模型在预热池中长时间冷却（所有启用通道都冷却且剩余 15 分钟以上，如当天额度已用完）的账户达到预热数的 5%（至少 3 个）时，在后台把这些空闲 Worker 换成没有冷却的账户（每轮最多“启动预热并发”个、需有可接替账户、刚就绪 2 分钟内的不换），预热补位时也优先选没有冷却的账户。此前只有请求到来找不到可用 Worker 时才现场启动或替换浏览器，这几秒到十几秒都算在请求等待里。状态接口只统计账户数量、新账户处理只查询单个账户，不再频繁构建上千个账户的完整状态。

浏览器控制连接（WebDriver BiDi）：改为后台协程统一读取、按编号分发回复，同一浏览器上的多条命令可以同时进行；单条命令超时不再关闭整条连接（原实现超时即断开，浏览器稍慢一次，该 Worker 上所有进行中的请求会一起失败）。受保护请求轮询偶尔超时时继续等待，连续 60 秒无回应才判定失败；控制连接断开、写入失败、发起请求的命令 15 秒无回应或轮询连续 60 秒无回应的 Worker 如实报告失败（状态查询不持有 Worker 内部锁），请求换号或在重建后重放、Worker 自动重建。解析上游响应时保留原始错误链，读取超时等可重试错误会换号重试而不是直接返回 502。

账户池锁优化：全体账户模型目录的合并改为在全局账户池锁外进行，并改为单遍线性合并（结果与原逐步合并完全一致）；Build 独有模型判断改为查表；自动重启监督只读取服务状态。此前账户数百个、后台持续自动验证或同步目录时，每次重算都要在锁内做几十万次集合运算，期间所有请求都拿不到锁。

并发调度优化：分配账号时不再逐个等待正在生成 WAA proof 的账户（每次 1～5 秒），调度耗时不再随并发线性增长；候选全部没有空位且有账户冷却时，从磁盘刷新运行态改为同一模型每 3 秒最多一次；没有额度信息的 429 也会让该账户的这个模型冷却 30 秒，不再被反复选中白白失败；只有 “You exceeded your current quota” 通用文案、没有其他周期信息的额度 429 按每日限额冷却到太平洋时间次日零点（北京时间夏令时 15:00、冬令时 16:00）。额度周期按 `quota_unit`、`quota_limit`、`quota_metric`、错误文案依次判定，按模型计的分钟限额只冷却该模型，不再冻结整个账号；上游给出 RetryInfo 或 Retry-After 时按它计算恢复时间（每日限额不会因此提前恢复）。

参数容错：`max_tokens` 超过模型上限时按上限发送，不再报错；`reasoning_effort` 兼容 `xhigh`、`max`（视为最高）与 `auto`（使用模型默认）；只支持思考预算的模型收到思考强度时换算为预算（low 1024、medium 8192、high 24576）；不支持调节思考的模型忽略思考参数。Playground 与 Build 两个通道一致。OpenAI 与 Gemini 的 penalty、logprobs、logit_bias 上游没有对应参数，直接忽略，不再返回 400（`n` 与 `candidateCount` 仍只接受 1），其余协议兼容见下文 [API 使用](#api-使用)。

优雅退出：收到停止信号（`./start.sh stop/restart`）后先停止接收新请求，等进行中的请求完成再结束运行时，最多等待 SHUTDOWN_GRACE（默认 60 秒）；管理页实时事件流会主动断开，不拖住等待。此前运行时直接跟随退出信号，每次重启都会立即截断所有进行中的回复。start.sh 强制结束前的等待相应放宽到 90 秒（STOP_TIMEOUT）。

回复指纹与重复回复分析：请求日志记录回复正文的指纹（reply_hash，只用于比较是否相同，不保存内容）；`./start.sh diag` 会找出内容完全相同的回复并列出当时的 seed 与温度，判断是客户端固定 seed、温度为 0，还是请求根本没有到达本服务（中转站或客户端缓存）。

工具结果中的图片：工具（如读取文件、截图、MCP 工具）返回的 base64 图片会取出来作为真正的图片发送，放在这组工具结果之后的用户消息里并标注来自哪次调用；工具结果原位置替换为文字说明。此前图片的 base64 会被当成文字塞进工具结果：模型看不到图片，输入 token 暴涨，连续读取两张图片基本必然报错。支持 OpenAI、Anthropic、Responses 与 MCP 格式；网络地址形式的图片保持原样。

对话历史分轮修正：发往上游前自动合并连续的模型消息与连续的工具结果；历史以模型工具调用开头时在前面补一条用户消息；历史以模型文字消息结尾（预填充、续写）时在最后补一条请模型从原消息结尾接着写、不重复已有内容的用户消息，避免上游返回 400 “Requests ending with a model turn are not supported”。用于兼容把说明文字和工具调用、或把并行工具调用拆成多条 assistant 消息的客户端，避免上游返回 400 “function call turn comes immediately after a user turn or after a function response turn”。只调整分轮，不改动内容与思考签名。

新账户自动处理（账户页顶部面板）：新目录导入后进入队列，待处理达到“每批数量”（默认 10）或最早的账户已等待 2 分钟时开始处理。账户默认启用（见下文“账户停用原因与认证文件更新”），导入的账户已经是启用状态，开启自动验证时启动浏览器验证登录（默认并发 3），未通过的显示为“需要登录”；仍为停用的账户（例如手动停用后交给“处理现有停用账户”）在停用状态下验证，通过后才启用，失效的保持停用、不会接到请求；手动停用的账户导入时不进入队列，排队或验证期间被手动停用的也保持停用（“处理现有停用账户”加入的除外），停用账户的验证结果按浏览器实际验证判断。验证过程出错会间隔 2 分钟重试，共 3 次；处理结果合并约 2 秒写一次 `.onboarding.json`，停止服务时立即写出。设置与处理记录保存在账户目录的 `.onboarding.json`，重启后继续有效，服务停止期间上传的账户在启动后同样会处理。首次开启时已有账户记为基线不做改动，面板上的“处理现有停用账户”可以把它们交给同一流程；管理页手动添加的账户不自动处理；删除后重新上传的账户会再次处理。

服务器运维：`./start.sh report` 一键打包排查资料到 reports/（诊断结果、统计窗口内的日志、崩溃堆栈、服务状态与程序快照、系统信息、隐藏密钥的配置；账户邮箱替换为固定代号，密钥、口令、代理账号密码、API Key、令牌与 Cookie 均隐藏；`MINUTES=120 ./start.sh report` 扩大窗口）；`./start.sh diag` 一键诊断回复慢与截断（截断分析把已开始输出但未正常写完的回复按 Google 内容审核、Google 端异常、输出上限、客户端断开、传输出错分类并给出责任方；系统资源、账号冷却与预热池交叉核对、排队/生成 proof/等上游响应头/响应头到首字各阶段耗时、状态接口卡住时的锁等待快照、程序内部等待点，并给出结论；请求日志会记录“WAA proof 完成”“上游已返回响应头”两个阶段时间点；`MINUTES=120 ./start.sh diag` 扩大统计窗口）；`./start.sh disk` 查看账户目录与浏览器缓存占用；`./start.sh clean-cache` 清理没有 Worker 在用的账户浏览器缓存（每个账户上限约 256MB，账户越多累积越多，运行中执行也安全）。

管理面访问控制：管理页面与 `/api/` 控制面必须携带管理令牌，不再按“来源是本机”放行（默认配置的 nginx `proxy_pass` 转发的外网请求来源同样是 127.0.0.1，且不带转发头）。令牌在首次启动时随机生成并保存在程序目录的 `.admin-token`（权限 0600，删除后重启即更换）；启动日志与 `./start.sh status` 会打印 `http://127.0.0.1:2048/?admin_token=<令牌>`，用它打开一次后浏览器以 HttpOnly、SameSite=Strict 的 Cookie 记住登录，`start.bat` 与直接运行程序时自动打开的页面已带令牌。脚本调用控制面时在 `X-Admin-Token` 请求头中携带令牌（`start.sh` 与 `tools/` 下的诊断脚本会自动读取）。设置 `ADMIN_PASSWORD` 后也可以用 HTTP Basic 密码登录；错误次数按反代传来的真实客户端地址分别计数。

账号 403 冷却与自动暂停：生成请求对某个模型返回 HTTP 403 无权限（常见于免费账号调用付费模型）时，该账号这个通道的这个模型进入冷却，冷却期间不再为该模型选它，换号不退避；冷却时长按 24 小时内的次数递增：第一次 30 分钟，第二次 6 小时，第三次起到太平洋时间次日零点（至少 6 小时）；两个通道分别计档，同一通道冷却期间再收到 403（冷却写入前已发出的请求）不升档，只从当时重新计时，该通道成功一次即从头计。这些 403 换号不退避，但仍计入每个请求的连续失败上限（5 次），模型对所有账号都返回 403 时一个请求不会把号池逐个试完。同一号池 10 分钟内至少 5 个不同账号对同一模型返回 403、这段时间该模型又没有任何成功时，按号池级问题（如 WAA proof 或请求形态失效）处理：不冷却账号、不升档，修复后立即恢复。同一账号另一通道仍可用时先在那里再试一次；所有通道都冷却后，该账号的常驻 Worker 登记淘汰：空闲时关闭；忙碌时只有该模型是主用模型或大量 Worker 都冷却的模型才排空（见“配置说明”中的容量说明），否则等它空闲后关闭。同一账号 10 分钟内对至少 2 个不同模型连续 3 次返回 403（中间没有成功请求）时，另外自动进入 30 分钟全局冷却，不再参与调度，到期自动恢复。冷却与暂停记录在“冷却与请求”页可见，重启后保留。

403 无权限账户列表：生成请求返回 HTTP 403 PERMISSION_DENIED（`The caller does not have permission`，协议错误码 7）的账户另外登记，在管理页“账户”页顶部单独列出：“403 无权限账户（多半未同意 Google 政策，重新获取认证文件后上传即可）N 个”，每个账户显示邮箱、返回 403 的模型与通道、最近一次的时间与次数，超过 20 个时折叠。这类账户多半是还没有同意 Google 的政策，冷却到期后仍然 403，只能重新获取认证文件。处理方法：点“复制全部邮箱”（每行一个）或“下载列表”（txt，同样每行一个邮箱）拿到这些账户，逐个登录 Google 同意政策后重新获取认证文件，覆盖上传到 `auth/<Google 邮箱>/storage-state.json`（或替换整个账户目录）。服务约 10～20 秒内检测到认证文件更新（在已登录的浏览器里同意政策后重新导出、登录 Cookie 没变的认证文件也算，本服务自己写回 Cookie 不算），自动把该账户移出列表，同时解除它的 403 冷却与自动暂停、冷却档位从头计（额度等其他冷却不变），账户立即重新参与调度，不用等冷却到期；服务停止期间换上了新登录的认证文件时，启动时同样清除，目录已删除的账户的记录也一并清除；在管理页对该账户“重新登录”同样会清除。新的认证文件仍然 403 时会重新列出。账户之后在某个模型的某个通道上请求成功时，也会清除该模型这个通道的记录。号池级 403（见上一段）不计入：判定成立后，该号池里这个模型自最近一次成功以来登记的账户全部撤销（故障紧接着成功开始时，判定成立前的约 10 分钟里每个 403 都先按账户登记了）。“清除记录”只清空列表，不解除冷却。不打算再用这些账户时点“一键删除”：确认后删除列表中的全部账户（包括折叠未显示的），从号池移除并删除它们的账户目录（认证文件），与账户列表里单个“删除”的效果相同，403 记录随之清除；删除后不能恢复，要重新上传认证文件才能再用。确认后先重新读取一次列表、每批提交前再对照一次，只删除确认时列出、提交时仍在列表里的账户：确认框打开期间重新上传了认证文件、已经移出列表的账户不会删除，确认后才列出的账户也不删除（服务约 10～20 秒才检测到新的认证文件，刚上传的账户在这之前仍在列表里，会被删除）。管理页每 500 个一批调用 `POST /api/accounts/bulk-delete`，进度与失败明细显示在批量进度面板，两批之间可以停止，完成后提示删除、不存在（号池里已经没有，只清除记录）与失败的个数。正在处理请求的账户先不再接新请求，等它空闲后删除，整批最多等约 10 秒；等不到的列为失败（“账户一直在处理请求”），并恢复正常调度，稍后再点一次即可。记录保存在账户目录下的 `.permission-denied.json`，重启后保留；管理接口为 `GET /api/accounts/permission-denied` 与 `POST /api/accounts/permission-denied/clear`（请求体 `{"account_ids":[...]}` 或 `{"all":true}`，账户 ID 不区分大小写）。

Worker 预热采用滑动窗口：按“启动预热并发”同时启动多个 Worker，任意一个完成后立刻补上下一个，单个慢账户不会拖住其他槽位。请求现场冷启动 Camoufox（包括请求排队时的后台扩容）与预热共用这组名额，另保留 1 个只给请求，有请求排队冷启动时预热让行，预热占满时请求不用等一整轮浏览器启动；调整“启动预热并发”后排队中的启动立即按新值放行。纯 Go 后端不启动浏览器，只受峰值 Worker 数约束。

运行中无需重启的热更新：

- **账户目录**：放进 `auth/` 的新账户目录约 10 秒内自动导入；目录被删除后自动移出账户池（不删除任何文件）；“需要登录”的账户在认证文件被替换后自动重新载入。单个目录损坏或正在复制不影响其他账户启动
- **服务配置**：管理页面修改后自动保存。常驻/峰值 Worker 数、启动预热并发、单账户并发、单账号每分钟上限、账户选择策略、初始化与请求超时、临时对话、IP 池、API 密钥保存后**立即生效，不重启、不中断请求**；账户目录、默认代理、WAA 后端、上游通道决定运行时的装配方式，仍需重启生成服务（倒计时后自动应用，应用前等待进行中的请求结束，最多 30 秒）；默认代理可在输入框旁点“测试代理”检查出口 IP（见“代理配置”），服务停止时也能测试；监听地址需 `./start.sh restart`。管理页面需要管理令牌（见上文“管理面访问控制”），远程管理可以用带令牌的地址、`ADMIN_PASSWORD`，或 SSH 隧道：`ssh -L 2048:127.0.0.1:2048 <服务器>`。`LISTEN_ADDR` 对外监听时务必把 `PROXY_API_KEY` 改为自定义密钥。

## 快速开始

### 首次使用（需要认证）

1. **准备首个账户**:

   Windows 可以导入本机 Chrome 账户：

   ```powershell
   start.bat setup
   ```

   Linux 与 macOS 使用隔离 Camoufox 登录：

   ```bash
   ./aistudio2api setup --login
   ```

   登录完成后会从 AI Studio 页面读取 Google 邮箱，并为账户授权 Google Drive。账户保存到 `.env` 中 `AISTUDIO_AUTH_STATES` 指向的目录；语言和时区默认读取当前电脑设置，也可以通过 `--locale`、`--timezone` 指定。

2. **启动图形界面**:
   - Windows 双击 `start.bat`
   - Linux 与 macOS 运行 `./aistudio2api`
   - 浏览器自动打开带管理令牌的 `http://127.0.0.1:2048/?admin_token=…`（之后浏览器记住登录；手动打开时使用启动日志里的地址）
   - 页面初始状态为 `STOPPED`，默认显示“日志”页面

3. **添加其他账户**:
   - 打开“账户”页面
   - “Chrome 批量导入”可多选本机 Chrome 账户
   - “浏览器登录”会打开独立 Camoufox 窗口，登录完成后自动识别邮箱、授权 Google Drive 并保存；Google 要求验证身份时，在该窗口或手机上确认

4. **启动 API**:
   - 点击“启动服务”启动数据面
   - 状态依次显示 `LAUNCHING` 和 `RUNNING`；`LAUNCHING` 期间可以点击“停止服务”取消启动
   - 在“日志”页面确认账户、模型和请求状态
   - API 默认监听 `http://127.0.0.1:2048`

账户操作随状态显示：

| 账户状态 | 可用操作 |
| --- | --- |
| `ready` | 编辑、停用、验证、删除 |
| `disabled` | 编辑、启用、删除 |
| `auth_required` | 编辑、停用、重新登录、验证、删除 |

“重新登录”只在账户状态为 `auth_required` 时显示。

### 日常使用（已有认证）

1. Windows 双击 `start.bat`；Linux 与 macOS 运行 `./aistudio2api`
2. 点击“启动服务”启用 API
3. 点击“停止服务”会取消正在进行的启动或活动请求并关闭 WAA Worker，管理页面与日志保持可用
4. 再次点击“启动服务”即可恢复 API

停止后再次启动会读取最新 `.env` 生成服务配置；管理页面地址和 `PROXY_API_KEY` 在管理进程重启后生效。

在启动窗口按 `Ctrl+C` 或关闭窗口会退出整个管理进程。关闭浏览器标签页不会停止管理进程。

### 快速启动

`start.bat`：启动管理进程并自动打开网页。

`start.bat -open-ui=false`：启动管理进程但不自动打开网页。

`start.bat setup`：扫描本机 Chrome 账户；也可使用 `--email` 或 `--profile` 选择明确的 Chrome 账户。隔离登录使用 `start.bat setup --login`；文件导入使用 `start.bat setup --storage-state <file>`。

## API 使用

### OpenAI 兼容接口

服务启动后，可以直接使用 OpenAI Chat Completions：

```bash
curl http://127.0.0.1:2048/v1/chat/completions \
  -H "Authorization: Bearer 123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.7-flash",
    "messages": [{"role": "user", "content": "Hello, world!"}],
    "stream": true
  }'
```

### 客户端配置示例

| 协议 | Base URL | API key |
| --- | --- | --- |
| OpenAI Chat / Responses | `http://127.0.0.1:2048/v1` | `.env` 中的 `PROXY_API_KEY` |
| Anthropic Messages | `http://127.0.0.1:2048` | `.env` 中的 `PROXY_API_KEY` |
| Gemini | `http://127.0.0.1:2048` | `.env` 中的 `PROXY_API_KEY` |

模型名称从 `GET /v1/models` 或 `GET /v1beta/models` 读取。

文本对话模型支持在模型名后加后缀，可组合、顺序不限（例如 `gemini-3.1-pro-preview-128-nothinking-online`、`gemini-4-argon-low-nothinking`），`/v1/models` 与 `/v1beta/models` 会同时列出这些别名：

| 写法 | 效果 |
| --- | --- |
| 不带后缀 | 思考强度设为最高；客户端显式传了 `reasoning_effort`、`thinking` 等思考参数时以客户端为准 |
| `-low`、`-medium`、`-high`、`-minimal` | 按名称指定思考等级，优先于客户端传入的思考参数，例如 `gemini-4-argon-low`、`gemini-4-argon-medium`；模型不支持该等级时取最接近的等级，只支持思考预算的模型换算为 1024、8192 token、最大预算与 128 token |
| `-128` | 最低思考，与 `-minimal` 相同：支持思考等级的模型用最低等级，只支持思考预算的模型用 128 token |
| `-nothinking` | 模型照常思考，但不返回思维链（保留多轮工具调用需要的思考签名） |
| `-online` | 开启 Google 搜索（模型支持时） |

模型列表为每个可调节思考的文本模型列出 `-low`、`-medium`、`-128` 等别名；目录里的模型 ID 本身以等级结尾时（例如 `xxx-low`）按 ID 里的等级设置，不再强制最高思考。

响应里的模型名（OpenAI、Anthropic、Responses、Interactions 的 `model`，Gemini 的 `modelVersion`）是请求的模型名去掉上述后缀：请求 `gemini-3.1-pro-preview`、`gemini-3.1-pro-preview-128`、`gemini-3.1-pro-preview-low` 都返回 `gemini-3.1-pro-preview`；目录里没有的名称按请求原样返回。带 `models/` 前缀的名称（如 `models/gemini-3.1-pro-preview-low`）先去掉前缀再解析，各协议一致。不返回上游报告的实际服务模型版本：它只出现在 OpenAI、Responses 与 Anthropic 非流式响应的 `provider_model` 字段里；实际服务的模型与请求的不是同一系列时，管理日志另记 `served_model`。

调用 API 必须携带 `PROXY_API_KEY`（默认 `sk-onechat-fun-fun`，可在管理页“服务配置”中修改）；使用默认密钥时，浏览器中只有本机页面可以直接调用接口，网页版客户端需要改为自定义密钥。密钥可以放在 `Authorization: Bearer`、`x-api-key`、`x-goog-api-key`、`api-key` 请求头或 `?key=` 查询参数中，任一位置与配置的密钥相同即通过：经 new-api 转发时 `?key=` 里常是下游令牌、请求头里才是本服务密钥，这种请求同样放行，不会因 401 让整个渠道被自动停用。只支持一个密钥。

协议兼容（各项细节见 [Google AI Studio 协议规范](docs/protocol.md)）：

- **忽略上游没有的参数**：OpenAI Chat 的 `frequency_penalty`、`presence_penalty`、`logprobs`、`logit_bias` 与 Gemini 的 `frequencyPenalty`、`presencePenalty`、`responseLogprobs`、`logprobs` 直接忽略，不再返回 400（SillyTavern、LobeChat、Open WebUI、Dify 常带非 0 的 penalty）；Chat 非流式响应的 `logprobs` 为 `null`。`n` 与 `candidateCount` 仍只接受 1。
- **历史工具调用参数容错**：Chat `tool_calls` 与 Responses `function_call` 的 `arguments` 不是 JSON object（例如一次被截断的调用）时包成 `{"_raw":"<原文>"}` 发送，Cline、Roo、OpenCode 等客户端回放后会话不再每轮 400。
- **Anthropic 服务端工具**：`web_search_*`、`web_fetch_*`、`code_execution_*` 按前缀识别任何日期版本；`tool_search_tool_*` 与 MCP toolset 定义直接忽略。`web_fetch` 的 `allowed_domains`、`blocked_domains` 仍返回 400。
- **Anthropic 历史**：更换 API key、或会话从别的 Claude 渠道切过来时，web_search 历史按文本保留标题与 URL（摘要只从本服务签发的来源恢复），调用与结果缺一半、搜索失败结果，以及 web_fetch、code_execution、tool_search 等其他服务端工具的历史也都转成文本，不再返回 400；`document` 支持 `text`、`content` 与 `file`（`file_id`）来源，`image` 支持 `file` 来源。
- **Anthropic 结构化输出**：`output_config.format` 与 beta 写法 `output_format` 的 `json_schema` 按 JSON 模式与 Schema 输出（原先被静默忽略，客户端拿到自由文本）。
- **工具参数 Schema**：`required` 列出未定义的属性时补上 `{}`，上游不再以 `property is not defined` 拒绝整个请求（Playground、Build 与 `strict` 核对一致）；Playground 上 `"5"`、`5.0` 这类整数写法与 `TYPE_UNSPECIFIED` 不再让 Schema 降级。
- **Gemini 小兼容**：`MODALITY_UNSPECIFIED`、`MODE_UNSPECIFIED` 按未设置处理；带 `dynamicRetrievalConfig` 的 `googleSearchRetrieval` 照常开启 Google Search；`fileData` 可以省略 `mimeType`（按 `displayName` 或 `fileUri` 的扩展名推断）。
- **上游结束说明**：Google 给出 `finishMessage`（如 `MALFORMED_FUNCTION_CALL` 的具体问题）时，Gemini 响应的 `finishMessage` 原样带上，管理日志另记一行 `上游结束说明`；输入被拦截时的上游说明只写进管理日志，客户端收到的错误不变。
- **流式心跳**：10 秒没有新内容时，Gemini 流写一个空行（google-genai Python SDK 会把 `: ping` 当作 JSON 解析而报错），Anthropic 发送官方 `event: ping`，OpenAI Chat、Responses 与 Interactions 仍发送 `: ping`。

以 Cherry Studio 为例：

1. 打开 Cherry Studio 设置
2. 新增 OpenAI 兼容提供商
3. API 主机地址填写 `http://127.0.0.1:2048/v1`
4. API 密钥填写 `.env` 中的 `PROXY_API_KEY`
5. 从 `/v1/models` 获取模型，或手动添加 `gemini-3.6-flash`、`gemini-3.7-flash`

[Claude Code](https://github.com/anthropics/claude-code) 使用 Anthropic 接口，子 agent 按 opus、sonnet、haiku 档位选择模型，以下变量把它们映射到 AI Studio 模型；WebSearch 由 Google Search 执行：

```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:2048"
$env:ANTHROPIC_API_KEY = "<PROXY_API_KEY>"
$env:ANTHROPIC_MODEL = "gemini-3.8-flash"
$env:ANTHROPIC_DEFAULT_OPUS_MODEL = "gemini-3.1-pro-preview"
$env:ANTHROPIC_DEFAULT_SONNET_MODEL = "gemini-3.8-flash"
$env:ANTHROPIC_DEFAULT_HAIKU_MODEL = "gemini-3.5-flash-lite"
```

[Codex](https://github.com/openai/codex) 使用 Responses 接口，在 `~/.codex/config.toml` 中添加 provider，并把 `PROXY_API_KEY` 写入 `AISTUDIO2API_KEY` 环境变量；Codex 的 `web_search` 工具由 Google Search 执行：

```toml
model = "gemini-3.8-flash"
model_provider = "aistudio"

[model_providers.aistudio]
name = "AIStudio2API"
base_url = "http://127.0.0.1:2048/v1"
env_key = "AISTUDIO2API_KEY"
wire_api = "responses"
```

[omp](https://github.com/can1357/oh-my-pi) 的 `web_search` 工具按自身的搜索来源顺序执行。设置 `GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:2048` 与 `GEMINI_API_KEY=<PROXY_API_KEY>`，并在 omp 配置中优先使用 Gemini 来源：

```yaml
providers:
  webSearchOrder:
    - gemini
  webSearchGeminiModel: gemini-3.8-flash
```

主要端点：

| 能力 | 端点 |
| --- | --- |
| 模型 | `GET /v1/models`、`GET /v1/models/{model}`、`GET /v1beta/models`、`GET /v1beta/models/{model}` |
| OpenAI Chat | `POST /v1/chat/completions` |
| OpenAI Responses | `POST /v1/responses` |
| Gemini Interactions | `POST /v1beta/interactions`、`POST /v1/interactions` |
| Files | `POST /v1/files`、`GET /v1/files/{id}`、`GET /v1/files/{id}/content`、`DELETE /v1/files/{id}` |
| Anthropic | `POST /v1/messages`、`POST /v1/messages/count_tokens` |
| Gemini | `POST /v1beta/models/{model}:generateContent`、`:streamGenerateContent`、`:countTokens` |
| 图片 | `POST /v1/images/generations` |
| 语音 | `POST /v1/audio/speech` |
| 转录 | `POST /v1/audio/transcriptions` |
| 音乐 | Gemini `generateContent` + `responseModalities: ["AUDIO"]` |
| 视频 | `POST /v1/videos`、`GET /v1/videos/{id}`、`GET /v1/videos/{id}/content` |
| Gemini 视频 | `POST /v1beta/models/{model}:predictLongRunning`、`GET /v1beta/operations/{id}` |
| Live（含实时翻译与实时转录）/ Robotics | `GET /v1/live`、`GET /v1/robotics/stream` |

四套生成接口均可按各自协议字段启用 Search、Image Search、URL Context、Code Execution 和 Maps。Files、Transcribe、Live、Robotics 的请求与事件格式见 [Google AI Studio 协议规范](docs/protocol.md)。

生成请求中的内联附件会优先上传为临时 Drive 文件，随请求结束清理；账户未授予 Drive 权限时保持内联数据发送（不冷却、不换号，服务记住这个账户 6 小时，期间不再先换 Drive token），过大的文件可能被上游以 400 拒绝。图片、音频、视频、PDF 等输入仍需所选模型支持。重复使用的附件可通过 Files 接口上传一次并复用文件 ID。

Gemini 附件与视频图片输入支持 `inlineData` / `inline_data`、`fileData` / `file_data`、`mimeType` / `mime_type` 和 `fileUri` / `file_uri`。媒体 Base64 数据支持标准与 URL-safe 字母表、带填充与无填充形式，以及 `data:<MIME>;base64,` 前缀。OpenAI 助手历史中的 Markdown 图片同样支持 URL-safe Base64 和 CR/LF 换行。内联 GIF 和视频表单上传的 GIF 按首帧静态图片转换为 PNG，保留逻辑画布、帧位置与透明背景。

### Ultra 号池（/ultra 前缀）

权益为 Ultra 的账户组成单独的 Ultra 号池（`ULTRA_INCLUDE_PRO=true` 时权益为 Pro 的账户也并入，组成付费号池，见下文“Pro 账户并入”），其余账户（含权益尚未读取的账户）组成普通号池。把客户端的 Base URL 换成 `/ultra` 前缀，请求就只由 Ultra 号池的账户处理；端点、鉴权（同一个 `PROXY_API_KEY`）、请求体与响应格式都和普通接口相同：

| 协议 | 普通号池 Base URL | Ultra 号池 Base URL |
| --- | --- | --- |
| OpenAI Chat / Responses | `http://127.0.0.1:2048/v1` | `http://127.0.0.1:2048/ultra/v1` |
| Anthropic Messages | `http://127.0.0.1:2048` | `http://127.0.0.1:2048/ultra` |
| Gemini | `http://127.0.0.1:2048` | `http://127.0.0.1:2048/ultra`（请求 `/ultra/v1beta/...`） |

```bash
curl http://127.0.0.1:2048/ultra/v1/chat/completions \
  -H "Authorization: Bearer <PROXY_API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model": "<MODEL>", "messages": [{"role": "user", "content": "Hello"}]}'
```

- **独占开关**：`ULTRA_EXCLUSIVE=true`（默认）时 Ultra 号池的账户只服务 `/ultra` 请求，普通路径只用其余账户；设为 `false` 时普通路径也可以用 Ultra 号池的账户，`/ultra` 仍只用 Ultra 号池的账户。
- **Pro 账户并入（付费号池）**：`ULTRA_INCLUDE_PRO=true` 时权益为 Pro 的账户与 Ultra 账户同属 Ultra 号池，按 Ultra 号池的全部设置调度：经 `/ultra` 服务并列入 `/ultra/v1/models`，受 `ULTRA_EXCLUSIVE` 约束，Worker 计入 Ultra 分区与 `ULTRA_WARM_WORKER_LIMIT`、`ULTRA_MAX_ACTIVE_WORKERS` 两个上限，适用 `ULTRA_PRIMARY_MODELS` 的快速失败，也计入侧栏的 Ultra 行、Ultra 主用模型供给与 `/api/status` 的 `ultra_accounts`。默认 `false`，升级后行为不变；在管理页保存后立即生效，不需要重启服务，已运行的 Worker 改按新分区计数，不会重启；旧版管理页面保存时不带这个字段，沿用现值。
  - **普通路径不再用 Pro（`ULTRA_EXCLUSIVE=true` 时）**：这样 Pro 账户只服务 `/ultra`，不再参与普通号池（Free、Plus 等账户）的模型调用。代价有三点：`gemini-4-argon`（只在 Build 通道提供，此前普通路径上的它本来就由 Pro 账户服务）与所有需要订阅权益的模型从普通路径的 `/v1/models` 消失，客户端要改用 `/ultra`；普通号池的容量随之变小；普通路径上 Free、Plus 账户输入超过上限后，不能再换到 Pro 账户重试。`ULTRA_EXCLUSIVE=false` 时普通路径照常可以用 Pro 账户（与 Ultra 账户相同），管理页会提示这个组合没有让 Pro 只服务 `/ultra`。
  - **真实权益不变**：账户页的权益列仍显示 Pro（另带“Ultra 号池”标记，权益筛选仍按权益），权益请求头仍为 `TIER1`，输入上限、模型资格与更高权益重试都按 Pro：只有 Ultra 订阅能用的模型仍只由 Ultra 账户服务，`/ultra` 上 Pro 账户输入超限时可以换 Ultra 账户再试一次；每日额度通道判定也仍按真实权益分组。Plus、Free 与还没读出权益的账户留在普通号池。
  - **Pro 能否服务 `gemini-4-argon` 取决于上游目录**：条件是该 Pro 账户的 Build 目录列出它并且可以生成；上游以后若在某个账户的 Playground 目录里把它标成只有 Ultra 订阅可用，Pro 账户会被排除。开启后看侧栏“Ultra 主用 gemini-4-argon”的备用数是否计入了 Pro 账户即可确认，也可以在 API 试用勾选“使用 Ultra 号池”后直接选它试一次。
  - **容量**：Pro 与 Ultra 共用 Ultra 分区的两个上限，默认 2 与 5 偏小，开启时应按 Pro 账户数调大，并视情况调小普通分区的 `WARM_WORKER_LIMIT`、`MAX_ACTIVE_WORKERS`。预热按权益轮流选号，Pro 排在 Ultra 之前，`ULTRA_WARM_WORKER_LIMIT=1` 时唯一常驻的可能是 Pro 账户，只有 Ultra 订阅能用的模型每次都要等冷启动，建议不小于 2。切换后浏览器总数可能短暂超过两个峰值之和（多出的是换了分区的 Pro Worker），空闲 5 分钟后回收。
  - **切换时**：资源不迁移，Pro 账户上已有的文件、视频与 Veo 操作切换后可能要换入口引用：开启后，`ULTRA_EXCLUSIVE=true` 时经普通路径引用返回 400，要改用 `/ultra`（`ULTRA_EXCLUSIVE=false` 时不受影响）；关闭后经 `/ultra` 引用返回 400，要改用普通路径。建议在没有进行中的视频任务时切换。带后缀的模型名（如 `gemini-4-argon-low`）切换后不会马上按新目录解析：各号池的别名快照 30 秒过期，但过期后仍先返回旧快照，只由带后缀的请求在后台触发刷新，所以刷新完成之前的带后缀请求（快照过期前的请求、过期后触发刷新的那个请求，以及刷新完成前同时到达的请求）仍按切换前的目录解析，可能因找不到模型而失败。带后缀的请求稀少时，这可能发生在切换很久之后，稍后重试即可；不带后缀的模型名不受影响。进程环境变量里设了 `ULTRA_INCLUDE_PRO` 时以环境变量为准，管理页保存的值不会生效（与其他配置项相同）。
- **模型与错误**：`/ultra/v1/models`、`/ultra/v1beta/models` 只列 Ultra 号池账户可用的模型。没有可用的 Ultra 号池账户时返回 503，Ultra 号池账户全部冷却时返回 429 与 `Retry-After`；Ultra 主用模型（`ULTRA_PRIMARY_MODELS`，默认 `gemini-4-argon`）没有额度时立即返回 429，不在服务里一直换号重试，见[主用模型](#主用模型)；文件、视频等资源按所属账户当前所在的号池检查：`/ultra` 请求不能使用普通号池账户上的资源，`ULTRA_EXCLUSIVE=true` 时普通路径也不能使用 Ultra 号池账户上的资源，引用号池之外的资源返回 400，消息说明资源属于哪个号池（`ULTRA_EXCLUSIVE=false` 时普通路径不检查号池）；切换 `ULTRA_INCLUDE_PRO` 时资源不迁移，见上面的“切换时”。
- **单独的 Worker 数**：Ultra 号池账户的 Worker 与普通号池分开计数，`ULTRA_WARM_WORKER_LIMIT`（常驻，默认 2，可以为 0 表示只按需启动）与 `ULTRA_MAX_ACTIVE_WORKERS`（峰值，默认 5）可按需设为 10、20 等；`WARM_WORKER_LIMIT`、`MAX_ACTIVE_WORKERS` 只约束普通号池，浏览器总数最多为两个峰值之和。Ultra 请求不会挤占普通号池的 Worker，反之亦然。开启 `ULTRA_INCLUDE_PRO` 后 Pro 与 Ultra 账户共用这两个上限，默认值偏小，应按需调大。
- **管理页面**：“服务配置”的“Ultra 号池”可修改以上四项，“主用模型”一组可修改 Ultra 主用模型列表与 Ultra 排队上限，保存后立即生效；“账户”页显示 Ultra 号池的就绪数与总数（开启 `ULTRA_INCLUDE_PRO` 时含 Pro 账户），可按权益筛选，权益列显示真实权益，并入 Ultra 号池的 Pro 账户另带“Ultra 号池”标记；侧栏显示 Ultra Worker 占用；请求日志、请求摘要与用量记录中经 `/ultra` 的请求带 Ultra 标记，用量页可按号池筛选与分组；API 试用勾选“使用 Ultra 号池”即经 `/ultra` 发送，模型列表只显示 Ultra 号池账户的模型，只在 Build 通道提供的模型（如 `gemini-4-argon`）照常列出。
- `/ultra` 下其余未知路径返回 404；排查路由 `/trace/` 按普通路径的规则选号，`/trace/ultra/v1/...` 为 Ultra 号池的排查路由。

### Gemini Interactions

新版 Google Gen AI SDK 的 `client.interactions` 与 TalkifyTTS 等客户端连接 `http://127.0.0.1:2048/v1beta/interactions`，稳定版入口为 `/v1/interactions`。与其他接口一样必须携带 `PROXY_API_KEY`，错误按 Gemini 格式返回：

```bash
curl http://127.0.0.1:2048/v1beta/interactions \
  -H "x-goog-api-key: sk-onechat-fun-fun" \
  -H "Content-Type: application/json" \
  -d '{"model": "<模型名>", "input": "Hello, world!", "stream": true}'
```

语音生成（单人或多说话人）：

```python
from google import genai

client = genai.Client(api_key="sk-onechat-fun-fun", http_options={"base_url": "http://127.0.0.1:2048"})
stream = client.interactions.create(
    model="<TTS 模型>",
    input="Hello, this is a test.",
    response_format={"type": "audio"},
    generation_config={"speech_config": [{"voice": "Kore"}]},
    stream=True,
)
for event in stream:
    if event.event_type == "step.delta" and event.delta.type == "audio":
        print(event.delta.data)
```

流式音频默认为 Base64 编码的 24 kHz、16-bit 小端、单声道 PCM；非流式默认为完整 WAV，`response_format.mime_type` 可显式选择 `audio/l16` 或 `audio/wav`。模型名后缀、函数调用（`id` 一定非空，原样回传即可对应结果）与 `previous_interaction_id` 续接均可用；续接与 Responses 共用当前进程内的响应节点，不接受 `resp_` 开头的 Responses ID，语音输出不保存。字段与事件见 [Interactions 协议](docs/protocol.md#gemini-interactions)。

### TTS 语音生成

```bash
curl http://127.0.0.1:2048/v1/audio/speech \
  -H "Authorization: Bearer 123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.1-flash-tts-preview",
    "input": "Hello, this is a test.",
    "voice": "Kore",
    "response_format": "wav"
  }' \
  --output speech.wav
```

多说话人语音可以通过 Gemini `generateContent` 的 `multiSpeakerVoiceConfig` 配置。

```bash
curl http://127.0.0.1:2048/v1beta/models/gemini-2.5-flash-preview-tts:generateContent \
  -H "x-goog-api-key: 123" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [{"parts": [{"text": "Joe: How are you?\nJane: I am fine, thanks!"}]}],
    "generationConfig": {
      "responseModalities": ["AUDIO"],
      "speechConfig": {
        "multiSpeakerVoiceConfig": {
          "speakerVoiceConfigs": [
            {"speaker": "Joe", "voiceConfig": {"prebuiltVoiceConfig": {"voiceName": "Kore"}}},
            {"speaker": "Jane", "voiceConfig": {"prebuiltVoiceConfig": {"voiceName": "Puck"}}}
          ]
        }
      }
    }
  }' --output speech.json
```

可用语音由实时模型目录中的 `capability_options.voices` 返回。`gemini-3.8-flash-tts` 等带 `speech_metadata` 能力的模型同样接受上面的 `说话人: 台词` 写法，也可以为每个文本 part 设置 `speechMetadata.speaker` 与 `speechMetadata.style`，并用 `multiSpeakerVoiceConfig.mode` 选择 `VERBATIM` 或 `CONVERSATIONAL`；OpenAI `instructions` 在这些模型上作为语音风格。

### 图片生成 (Nano Banana)

```bash
curl http://127.0.0.1:2048/v1/images/generations \
  -H "Authorization: Bearer 123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.1-flash-image",
    "prompt": "A cute cat wearing a tiny hat",
    "n": 1,
    "size": "1024x1024"
  }'
```

### 视频生成 (Veo)

```bash
curl http://127.0.0.1:2048/v1/videos \
  -H "Authorization: Bearer 123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "veo-3.1-fast-generate-preview",
    "prompt": "A drone flying over a forest"
  }'
```

创建操作后通过 `GET /v1/videos/{id}` 查询状态，通过 `GET /v1/videos/{id}/content` 下载结果。

## 模型

模型目录会随 AI Studio 更新，客户端从 `/v1/models` 或 `/v1beta/models` 读取当前值。下表保留目录结构示例，模型 ID、限制和方法以运行时结果为准：

| Model ID | Display name | Input | Output | Methods |
| --- | --- | ---: | ---: | --- |
| `antigravity-preview-05-2026` | Antigravity Agent Preview | 131072 | 65536 | `countTokens, generateContent` |
| `gemini-2.5-flash` | Gemini 2.5 Flash | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-2.5-flash-image` | Nano Banana | 32768 | 32768 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-2.5-flash-lite` | Gemini 2.5 Flash-Lite | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-2.5-flash-preview-tts` | Gemini 2.5 Flash Preview TTS | 8192 | 16384 | `countTokens, generateContent` |
| `gemini-2.5-pro` | Gemini 2.5 Pro | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-2.5-pro-preview-tts` | Gemini 2.5 Pro Preview TTS | 8192 | 16384 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-3-flash-preview` | Gemini 3 Flash Preview | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3-pro-image` | Nano Banana Pro | 131072 | 32768 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-3.1-flash-image` | Nano Banana 2 | 65536 | 65536 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-3.1-flash-lite` | Gemini 3.1 Flash Lite | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3.1-flash-lite-image` | Nano Banana 2 Lite | 65536 | 65536 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-3.1-flash-tts-preview` | Gemini 3.1 Flash TTS Preview | 8192 | 16384 | `batchGenerateContent, countTokens, generateContent` |
| `gemini-3.1-pro-preview` | Gemini 3.1 Pro Preview | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3.5-flash` | Gemini 3.5 Flash | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3.5-flash-lite` | Gemini 3.5 Flash Lite | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3.6-flash` | Gemini 3.6 Flash | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-3.7-flash` | Gemini 3.7 Flash | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-flash-latest` | Gemini Flash Latest | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-flash-lite-latest` | Gemini Flash-Lite Latest | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-omni-flash-preview` | Gemini Omni Flash Preview | 131072 | 65536 | `countTokens, generateContent` |
| `gemini-pro-latest` | Gemini Pro Latest | 1048576 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-robotics-er-1.6-preview` | Gemini Robotics-ER 1.6 Preview | 131072 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemini-robotics-er-2-preview` | Gemini Robotics-ER 2 Preview | 131072 | 65536 | `batchGenerateContent, countTokens, createCachedContent, generateContent` |
| `gemma-4-26b-a4b-it` | Gemma 4 26B A4B IT | 262144 | 32768 | `countTokens, generateContent` |
| `gemma-4-31b-it` | Gemma 4 31B IT | 262144 | 32768 | `countTokens, generateContent` |
| `lyria-3-clip-preview` | Lyria 3 Clip Preview | 1048576 | 65536 | `countTokens, generateContent` |
| `lyria-3-pro-preview` | Lyria 3 Pro Preview | 1048576 | 65536 | `countTokens, generateContent` |
| `veo-3.1-fast-generate-preview` | Veo 3.1 fast | 480 | 8192 | `predictLongRunning` |
| `veo-3.1-generate-preview` | Veo 3.1 | 480 | 8192 | `predictLongRunning` |
| `veo-3.1-lite-generate-preview` | Veo 3.1 lite | 480 | 8192 | `predictLongRunning` |

公开端点实现标准 `generateContent`、`countTokens` 和 `predictLongRunning`。`/v1/models` 与 `/v1beta/models` 原样汇总各账户实时上游目录；调度按目录明确提供的模型 ID、方法、能力字段和账户当前运行状态选择账户。

## 项目架构

```text
AIStudio2API/
├── cmd/aistudio2api/        # 薄入口
├── internal/app/            # 命令、管理监听、生成服务生命周期与调度
├── internal/setup/          # 账户导入和独立登录 CLI
├── internal/aistudio/       # AI Studio 协议、认证、模型与媒体
├── internal/api/            # OpenAI、Responses、Anthropic 与 Gemini 适配
├── internal/chromeauth/     # Windows Chrome 与 DBSC 导入
├── internal/camoufoxnative/ # Camoufox BiDi、登录与 WAA Worker
├── internal/webui/          # 内嵌前端产物
├── web/                     # Vue 3 + TypeScript 管理页面
├── docs/                    # 开发文档与协议规范
└── start.bat                # Windows 一键启动入口
```

## 配置说明

### 环境变量配置

复制并编辑环境配置文件：

```bash
cp .env.example .env
```

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `AISTUDIO_AUTH_STATES` | `auth` | 账户文件、目录或多个逗号分隔路径 |
| `LISTEN_ADDR` | `127.0.0.1:2048` | 管理页面与 API 监听地址 |
| `PROXY_API_KEY` | `sk-onechat-fun-fun` | 公开 API key；调用 API 必须携带，留空即使用默认值。默认密钥随源码公开，对外监听时务必改为自定义密钥 |
| `PROXY` | 空 | Chrome 导入、登录和账户默认使用的代理：http、https 或 socks5，可带账号密码（`http://用户名:密码@IP:端口`、`IP:端口:用户名:密码` 等），保存时统一为 URL 形式，见[代理配置](#代理配置) |
| `PROXY_POOL_MODE` | `off` | IP 池模式（不区分大小写）：`off` 关闭；`static` 静态 IP，按端口段给每个运行中的浏览器独占一个端口（一个端口一个固定 IP）；`dynamic` 动态 IP，按账户把地址里的 `{session}` 换成固定会话号。开启后没有自己代理的账户改用 IP 池出网，保存后立即生效，见 [IP 池](#ip-池每个浏览器一个-ip) |
| `PROXY_POOL_URL` | 空 | IP 池的代理地址，写法同 `PROXY`，保存时统一为 URL 形式：静态填任意一个端口的地址（端口会被替换，不能含 `{session}`），动态在用户名或密码里会话号的位置写 `{session}`；关闭时保留地址 |
| `PROXY_POOL_PORT_START` | `10001` | 静态 IP 的起始端口（含） |
| `PROXY_POOL_PORT_END` | `10200` | 静态 IP 的结束端口（含）；端口段最多 5000 个端口，任何模式下都要是 1–65535 且起始不大于结束 |
| `INIT_TIMEOUT` | `2m` | 单账户 WAA 初始化超时 |
| `REQUEST_TIMEOUT` | `5m` | 生成请求在首个上游事件之前的总期限：排队、冷启动、WAA proof、附件上传、等首字与首事件前的换号都计入，到时返回 504。开始输出后改为空闲上限：上游连续这么久既没有事件也没有网络字节才以 504 结束；整条生成请求最长 1 小时（本项更长时按本项）。计数、视频与转写等其他请求整段受它限制。修改只影响新请求，见[长回复中途截断或重新生成](#长回复中途截断或重新生成) |
| `FIRST_EVENT_TIMEOUT` | `0`（关闭） | 每次尝试从向上游发送起等待首个上游事件的上限；超时只放弃这一次尝试并换号重试，不能再换号时返回 504。必须小于 `REQUEST_TIMEOUT`；思考很长的模型可能很久才有第一个事件，开启时要留足余量 |
| `WARM_WORKER_LIMIT` | `5` | 普通号池的常驻预热账户数（不含 Ultra 号池的账户）。应不小于希望常驻的 Worker 数：超出它的 Worker 空闲 5 分钟后被回收，预热也不会补齐超出部分空出的槽位，只能靠请求现场冷启动 |
| `MAX_ACTIVE_WORKERS` | `10` | 普通号池高峰期最多同时运行的 Worker 数；浏览器总数最多为它与 `ULTRA_MAX_ACTIVE_WORKERS` 之和。淘汰仍在服务请求的冷却占位时替补先启动，排空期间每个分区最多再多 `min(WARM_STARTUP_CONCURRENCY×2, 峰值/4)` 个（至少 1 个） |
| `ULTRA_EXCLUSIVE` | `true` | `true` 时 Ultra 号池的账户只服务 `/ultra` 前缀的请求，普通路径只用其余账户；`false` 时普通路径也可以用 Ultra 号池的账户，`/ultra` 仍只用 Ultra 号池的账户 |
| `ULTRA_INCLUDE_PRO` | `false` | `true` 时权益为 Pro 的账户并入 Ultra 号池（付费号池），按 Ultra 号池的全部设置调度（`/ultra`、`ULTRA_EXCLUSIVE`、Ultra 分区与两个 `ULTRA_*` Worker 上限、`ULTRA_PRIMARY_MODELS`）；`ULTRA_EXCLUSIVE=true` 时 Pro 账户不再服务普通路径。账户的真实权益不变（权益请求头 `TIER1`、输入上限、模型资格、账户页显示的权益与每日额度通道判定仍按 Pro）。保存后立即生效，影响与注意事项见 [Ultra 号池](#ultra-号池ultra-前缀) |
| `ULTRA_WARM_WORKER_LIMIT` | `2` | Ultra 号池的常驻预热 Worker 数，可以为 `0`（只按需启动）；开启 `ULTRA_INCLUDE_PRO` 时由 Ultra 与 Pro 账户共用，为 `1` 时唯一常驻的可能是 Pro 账户 |
| `ULTRA_MAX_ACTIVE_WORKERS` | `5` | Ultra 号池高峰期最多同时运行的 Worker 数，至少为 1 且不小于 `ULTRA_WARM_WORKER_LIMIT`；开启 `ULTRA_INCLUDE_PRO` 时由 Ultra 与 Pro 账户共用 |
| `WARM_STARTUP_CONCURRENCY` | `2` | 同时冷启动的 Camoufox Worker 数：预热最多占用该数，请求现场的冷启动另保留 1 个名额并优先；`WAA_BACKEND=go` 不受此限 |
| `PER_ACCOUNT_CONCURRENCY` | `2` | 单账号同时执行的请求数；受每分钟次数上限（`ACCOUNT_RPM_LIMIT`）约束，不宜调高 |
| `ACCOUNT_RPM_LIMIT` | `10` | 单账号最近 60 秒最多发出的生成请求数（上游每账号每分钟 12 次，所有模型与两个通道共用），满了就换号、热 Worker 都满时扩容；只统计本服务，429 冷却仍兜底；`0` 关闭；保存后立即生效 |
| `ROUTING_STRATEGY` | `round-robin` | `round-robin` 轮询；`fill-first` 账号粘性优先 |
| `UPSTREAM_CHANNELS` | `playground,build` | 生成请求使用的上游通道，可只保留其一 |
| `STREAM_PLAYGROUND_MODELS` | 空 | 流式请求优先走 Playground 的模型，逗号分隔。需要订阅权益的模型在 Build 通道只能一次性返回整段回复；列表中的模型的流式请求先用 Playground 逐块输出，Playground 账号都在忙时立即改用 Build，不排队等待 Playground。留空时 Playground 与 Build 照常轮询；非流式请求不受影响。管理页“服务配置”会列出目录中需要订阅权益、可经 Playground 调用的模型，保存后立即生效 |
| `PRIMARY_MODELS` | 空 | 普通号池的主用模型，逗号分隔，只作用于普通路径（不带 `/ultra` 前缀）。列表中的模型的请求最多等 `PRIMARY_QUEUE_WAIT` 空闲的账号或 Worker，超过即返回 429 并带 `Retry-After`，由客户端稍后重试，不再排队到 `REQUEST_TIMEOUT`；留空时所有模型照常排队。候选全部冷却仍按原规则处理。管理页“服务配置”可修改，保存后立即生效（见[主用模型](#主用模型)） |
| `PRIMARY_QUEUE_WAIT` | `10s` | 主用模型的排队上限，整个请求共用一份：从第一次选号起按实际经过的时间计，失败后换号重试的尝试（如 403、额度 429）与换号退避都算在内，不再每次换号重新计时；请求自己现场冷启动的浏览器启动时间不计（最多 `INIT_TIMEOUT`）；上限到了之后只接受立即可用的账号，否则返回 429；`0` 表示不排队，没有立即可用的账号就返回 429；不能超过 `REQUEST_TIMEOUT`（未配置时取 10s 与 `REQUEST_TIMEOUT` 中较小者） |
| `ULTRA_PRIMARY_MODELS` | `gemini-4-argon` | Ultra 号池的主用模型，逗号分隔，作用于 `/ultra` 请求（`ULTRA_EXCLUSIVE=false` 时也作用于普通路径上不在 `PRIMARY_MODELS` 里的这些模型）。排队规则与 `PRIMARY_MODELS` 相同，另外没有额度时立即返回 429：候选全部冷却立即返回；额度 429 后，每日额度用完最多再换 2 次账号，分钟限额与限流最多再换 5 次，每次额度失败后的那次换号只用已启动的 Worker（见[主用模型](#主用模型)）。在 `.env` 写空值或在管理页清空即关闭；进程环境变量的空值不生效。保存后立即生效 |
| `ULTRA_PRIMARY_QUEUE_WAIT` | `10s` | Ultra 主用模型的排队上限，规则与 `PRIMARY_QUEUE_WAIT` 相同：整个请求共用一份，`0` 表示不排队，不能超过 `REQUEST_TIMEOUT`（未配置时取 10s 与 `REQUEST_TIMEOUT` 中较小者） |
| `WAA_BACKEND` | `camoufox` | `camoufox` 在 Camoufox 页面运行 WAA；`go` 在服务进程内运行 WAA，不下载也不启动 Camoufox |
| `TEMPORARY_CHAT` | `false` | WAA 预热页是否使用临时对话 |
| `AUTO_START` | `true` | 管理进程启动后自动启动生成服务；启动失败或意外停止时自动重新启动（5 秒起、最长 1 分钟退避，手动停止后不再自动启动）；`false` 时保持 `STOPPED`，需在管理页面手动启动 |
| `REQUEST_BODY_LOG` | `false` | 用量账本同时保存公开 API POST 请求与响应的正文（各截断到 64 KiB，只保留最近 1000 条），在用量页的请求详情中查看；正文以明文保存在本机，修改后重启程序生效 |
| `ADMIN_PASSWORD` | 空 | 设置后除管理令牌外也可以用密码打开管理页面，浏览器弹出登录框（用户名随意，密码为此值）；同一 IP 10 分钟内错 10 次封禁 15 分钟；留空时只能用管理令牌（`.admin-token`）登录。纯 HTTP 下令牌与密码均为明文传输，建议配合 HTTPS |

管理页面的“用量”页读取本地账本 `runtime/requests.db`：通过 API key 校验的每个 POST 请求完成后记录一行（token 计数请求不计入），提供请求数、成功率、token、耗时分位、降级拦截率、重复回复率、分项排行、请求记录与 CSV 导出；记录与汇总保留 90 天。账本打开失败时只写 WARN，服务照常运行。

服务启动时会载入 `AISTUDIO_AUTH_STATES` 中的全部账户；`WARM_WORKER_LIMIT` 控制普通号池的常驻预热规模，`MAX_ACTIVE_WORKERS` 控制普通号池的峰值 Worker 上限，Ultra 号池由 `ULTRA_WARM_WORKER_LIMIT` 与 `ULTRA_MAX_ACTIVE_WORKERS` 单独控制（见 [Ultra 号池](#ultra-号池ultra-前缀)），这些 Worker 设置与 `ULTRA_EXCLUSIVE`、`ULTRA_INCLUDE_PRO` 在管理页保存后立即生效；`WARM_STARTUP_CONCURRENCY` 控制 Camoufox 冷启动并发（预热最多用满，请求现场冷启动另保留 1 个名额），`PER_ACCOUNT_CONCURRENCY` 控制单账户请求槽位，`ACCOUNT_RPM_LIMIT` 控制单账号每分钟最多发出的生成请求数。

号池的同时生成数约为 `MAX_ACTIVE_WORKERS × PER_ACCOUNT_CONCURRENCY`（Ultra 号池为 `ULTRA_MAX_ACTIVE_WORKERS × PER_ACCOUNT_CONCURRENCY`，开启 `ULTRA_INCLUDE_PRO` 时由 Ultra 与 Pro 账户共用），每分钟生成次数不超过 Worker 数 × `ACCOUNT_RPM_LIMIT`：上游每账号每分钟最多 12 次生成请求，单账号并发调得再高，一分钟内也只能发出这么多。并发请求超过它时，多出来的请求在选号时排队等空闲的账号或 Worker，首字延迟随之拉长，直到 `REQUEST_TIMEOUT` 到期返回 504；账号因每日限额冷却却仍占着 Worker 时可用容量更少。这样的冷却占位（账号全局冷却，或在主用模型、大量 Worker 都冷却的模型上冷却）即使一直有其他模型的请求在用，也会被排空：不再接新的请求，替补账号先启动，进行中的请求结束后关闭；其他模型的请求优先分给冷却占位，把还能服务主用模型的 Worker 留给主用模型。`PRIMARY_MODELS` 的快速失败只是对症状的处理：列表中的模型排队超过 `PRIMARY_QUEUE_WAIT` 就返回 429 让客户端稍后重试，不会增加容量；长期高于容量时，扩容靠增加 Worker 数（`MAX_ACTIVE_WORKERS`、`ULTRA_MAX_ACTIVE_WORKERS`）；单账号并发受每分钟次数上限（`ACCOUNT_RPM_LIMIT`）约束，不宜调高；或者减少并发。

管理页侧栏的容量显示：“冷却占位 N · 待淘汰”为常驻 Worker 中账号长时间冷却（全局冷却，或在主用模型、大量 Worker 都冷却的模型上冷却）的死槽位，其中正在排空的另显示“排空中 N”（不再接新请求，正在服务的请求结束后关闭；有空槽时替补已先启动，否则旧 Worker 关闭后补上）；设置了 `PRIMARY_MODELS` 时每个主用模型一行“主用 <模型>：热 N · 冷却 C · 备用 M”，热为能服务它的常驻 Worker 数，冷却为对它冷却中的常驻 Worker 数（含 2 分钟内到期的短冷却，冷却到期前服务不了它），备用为还能服务它的空闲账号数（最多统计 1000 个，显示为 1000+），约每 30 秒更新，备用为 0 时标黄（见[故障排除](#主用模型返回-429-或首字很慢)）；`ULTRA_PRIMARY_MODELS` 的每个模型在 Ultra Worker 一行下面另有一行“Ultra 主用 <模型>：热 N · 冷却 C · 备用 M”，只统计 Ultra 分区的常驻 Worker 与 Ultra 号池的空闲账号（开启 `ULTRA_INCLUDE_PRO` 时含 Pro 账户，切换后约 5 秒内按新的号池重新统计）。`/api/status` 中对应 `workers` 与 `ultra_workers` 的 `dead`、`draining`、`retiring`（待淘汰登记数）与 `primary_supply`（Ultra 主用模型的条目带 `"pool": "ultra"`）；`primary_supply` 的每个条目另有 `cooling`：对该模型冷却中的常驻 Worker 数（不在排空中、是该模型的候选，但所有支持的启用通道都在冷却，含 2 分钟内就恢复的短冷却），这些 Worker 既不计入热，也不计入备用。

### 主用模型

主用模型有两份列表，按请求所在的号池分别生效，管理页“服务配置”的“主用模型”一组可以修改，保存后立即生效，不用重启：

| 列表 | 默认 | 作用于 | 没有额度时 |
| --- | --- | --- | --- |
| `PRIMARY_MODELS`、`PRIMARY_QUEUE_WAIT` | 空、`10s` | 普通路径（不带 `/ultra` 前缀）的请求 | 与其他模型相同：候选全部冷却且 1 分钟内恢复时等待恢复，额度 429 后换号重试 |
| `ULTRA_PRIMARY_MODELS`、`ULTRA_PRIMARY_QUEUE_WAIT` | `gemini-4-argon`、`10s` | `/ultra` 请求；`ULTRA_EXCLUSIVE=false` 时还有普通路径上不在 `PRIMARY_MODELS`、但在这份列表里的模型（通常是只有 Ultra 号池的账号能服务的模型） | 立即返回 429，不在服务里一直换号重试（见下） |

两份列表都限制排队：列表中的模型最多等排队上限空闲的账号或 Worker，超过即返回 429 并带 `Retry-After`（建议间隔为排队上限，限定在 1 到 30 秒，上限为 0 时 5 秒；响应头另加随机抖动，见下文的 `Retry-After`）；上限整个请求共用一份，规则见上表的 `PRIMARY_QUEUE_WAIT`。同一个模型可以同时在两份列表里：普通路径按普通列表，`/ultra` 只看 Ultra 列表，只在普通列表里的模型在 `/ultra` 上照常排队。死槽位与替补也按分区看各自的列表：Ultra 分区的 Worker 只按 Ultra 主用模型判断，普通分区只按普通列表（`ULTRA_EXCLUSIVE=false` 时也不合并，免得普通号池的主用模型去淘汰数量少的 Ultra Worker）。

Ultra 主用模型没有额度时立即返回 429：

- **候选全部冷却**：立即返回 429，`Retry-After` 为距最早恢复的秒数（每日限额就是真实的恢复时间），不再原地等待 1 分钟内恢复的冷却。还有没冷却的备用账号、只是 Ultra 分区没有空闲的 Worker 槽位时属于容量不足，按排队上限处理。降级判定的 Playground 优先选号全部冷却时照旧改为不限通道选号。
- **请求中遇到额度 429**：额度失败分两类计数，各有换号上限，换号都不退避。每日类（每日限额，以及剩余不少于 15 分钟的冷却，例如上游给出的重试时间很长的分钟限额）最多再换 2 次账号（一个请求最多碰到 3 个因每日额度失败的账号；同一账号另一通道的探测再遇到每日类不另计）；限流类（分钟限额、没有额度信息的限流：第一次冷却 30 秒，连续出现逐级加长，见[号池繁忙 429 与连续限流](#号池繁忙-429-与连续限流)；升到 30 分钟以上后按剩余时间算作每日类）最多再换 5 次：分钟窗口按账号计（每账号每分钟 12 次，所有模型、两个通道共用），一个账号用满不代表其他账号也满。租到的账号在请求发出前刚进入冷却（多半是同账号别的请求刚遇到 429，这次请求没有发到上游）时，只有拦下它的冷却剩余不少于 15 分钟才算一次每日类额度失败，更短的照常换号、不计数。每次额度失败后紧接着的那一次选号只用已经启动的 Worker：有空闲的就用，都在忙时在排队上限内等待，不为这个请求现场冷启动备用账号，也不等后台扩容；其中有每分钟请求已满的 Worker（最长一分钟才释放）时，另在后台为一个备用账号启动 Worker（所在分区还能扩容时），留给之后的请求。没有已启动的 Worker 能服务时立即返回 429，同时（所在分区还能扩容时）在后台为一个备用账号启动 Worker，留给之后的请求。换到的 Worker 再因额度以外的原因失败（例如 5xx、过载）时，下一次选号按普通规则进行，可能现场冷启动备用账号。同一账号另一通道可用时先在那里再试一次（不算换号，请求进度写“同账号换通道重试”；限流类照常计一次，每日类同一账号只计一次）；带文件的请求仍先把文件复制到其他账号再试。
- **不算额度失败**：输入超过账号上限的 429（见[错误响应格式](#错误响应格式)）照旧按 400 处理。第一次尝试照常可以现场冷启动一个备用账号（这时还不知道它有没有额度）。
- **`Retry-After`**：候选全部冷却时为距最早恢复的时间；每日类换号次数用完或没有其他可换的账号时为 5 秒与本请求遇到的最早额度恢复时间中较早者；限流类换号次数用完（“限流失败已达换号上限”）时为本请求遇到的限流冷却中最早的恢复，不超过 1 分钟，不知道时 5 秒；额度 429 后没有已启动的 Worker 时为 5 秒；都至少 1 秒。写进响应头时再随机往后加 0 到 min(建议间隔 ÷ 5 秒, 3) 秒（建议间隔不到 5 秒不加），免得冷却一到期所有客户端同时重试；管理日志里的“建议重试”不含这部分。
- **对外消息固定**：与号池全部冷却相同，为 `Resource has been exhausted (e.g. check quota).`（OpenAI `rate_limit_exceeded`、Anthropic `rate_limit_error`、Gemini `RESOURCE_EXHAUSTED`），不带上游原文；原因、额度失败与限流失败次数、总耗时只写管理日志（“Ultra 主用模型额度不足，立即返回 429”，见[运行日志](docs/logging.md)）。

流式请求写出响应头前等待首个事件的时间按请求的号池取对应列表的排队上限加 5 秒（至少 10 秒）。Ultra 主用模型的请求自己现场冷启动备用账号时（例如第一次尝试时没有已启动的 Worker），这段等待再按启动时长延后（启动期间不写出响应头）：冷启动的账号随后返回额度 429、请求以额度不足结束时，同样以 HTTP 429 和 `Retry-After` 返回。延后有两个上限：合计不超过 `INIT_TIMEOUT`（抵扣用完后再现场冷启动另一个账号时不再延后），且从开始等首个事件算起最多 50 秒（排队上限加 5 秒更长时按它）；到了上限照常先回 200 再推流（之后有心跳），之后的错误在流内返回。延后期间客户端收不到任何字节：前面若有反向代理或中转，读超时要大于这段等待（默认最长 50 秒；nginx 默认的 `proxy_read_timeout` 60 秒与 Cloudflare 的 100 秒都够），更短时会在冷启动期间断开，断开还会取消进行中的启动。这些 429 因此通常以 HTTP 状态返回，而不是 200 之后的流内错误。普通号池的 `PRIMARY_MODELS` 与不在列表里的模型行为不变（普通号池的主用模型在请求自己现场冷启动后排队超限时仍可能是流内错误）。

> **升级提醒**：`ULTRA_PRIMARY_MODELS` 默认不为空，升级后 `/ultra` 上的 `gemini-4-argon` 自动成为 Ultra 主用模型。不需要时在 `.env` 写 `ULTRA_PRIMARY_MODELS=`（空值），或在管理页清空 Ultra 主用模型列表；进程环境变量设为空值不生效（空值按未设置处理，仍用 `.env` 里的值或默认值）。另外 `PRIMARY_MODELS` 从此只作用于普通路径，原先靠它让 `/ultra` 请求快速失败的，把这些模型加进 `ULTRA_PRIMARY_MODELS`。

### 端口配置

- **管理页面与 API**: 默认端口 `2048`
- **Camoufox**: 由程序动态分配本机端口

## 高级功能

### 代理配置

支持通过 HTTP、HTTPS 或 SOCKS5 代理访问 AI Studio，可以带账号密码（代理商给的静态 IP、动态 IP 账号都能直接填）：

1. 在“服务配置”中设置默认代理（即 `.env` 的 `PROXY`）
2. 在“账户”页面编辑单个账户、浏览器登录或 Chrome 导入时可以设置账户专用代理，留空时开启了 IP 池就用 IP 池（见下文 [IP 池](#ip-池每个浏览器一个-ip)），否则用默认代理
3. 账户代理同时用于登录、WAA 与业务请求

支持五种写法，不写协议时按 http，`socks5h://` 按 `socks5://` 处理：

- `http://host:port`（不带认证，`https://`、`socks5://` 同理）
- `http://用户名:密码@host:port`（`https://` 同理）
- `socks5://用户名:密码@host:port`
- `host:port:用户名:密码`（代理商面板最常见的写法；第 3 个冒号之后全部算密码，也可以加协议，例如 `socks5://1.2.3.4:1080:用户名:密码`）
- `用户名:密码@host:port`

不支持 `用户名:密码:host:port` 与 `host:port@用户名:密码` 这两种把账号密码和 host:port 写反的顺序：密码是纯数字时，它们与 `host:port:用户名:密码`、`用户名:密码@host:port` 无法区分（例如 `myuser:54321:1.2.3.4:8080`、`1.2.3.4:8080@myuser:54321`），这时保存会报错并提示改用 `http://用户名:密码@host:port`。判定条件是 host 段不是 IP、也不含点，而 `用户名:密码` 像 IP 或带点域名加端口；代理主机名不含点（例如 `localhost`、内网主机名）、用户名又像域名、密码是纯数字时也会报这个错，请把代理主机写成 IP。IPv6 地址要加方括号，例如 `http://用户名:密码@[2001:db8::1]:8080`。

说明：

- **保存时统一改写成 URL 形式**：例如 `1.2.3.4:8080:user:pass` 保存为 `http://user:pass@1.2.3.4:8080`。账号密码里字母、数字与 `- . _ ~` 以外的字符一律按百分号编码，`.env` 里不需要加引号；不带认证的合法 URL 原样保存。唯一的例外是带协议的冒号写法、密码为纯数字或为空（例如 `http://1.2.3.4:8080:user:123`）：旧版本把它当成主机名带冒号的无认证 URL（本来就连不上），现在按账号密码处理，保存为 `http://user:123@1.2.3.4:8080`；其中按上面的规则会报错的值（例如 `http://myuser:54321:1.2.3.4:8080`）写在 `.env` 里时服务无法启动、写在 `account.json` 里时该账户不会载入，请按错误提示改写。URL 写法与 `用户名:密码@` 写法里的 `%XX` 会按百分号编码解码，密码里要原样保留 `%41` 这类内容时请用冒号写法。写了协议、带认证但没写端口时 http 补 80、https 补 443；不写协议时必须写端口；SOCKS5 必须写端口；HTTP/HTTPS 代理的用户名不能含冒号（Basic 认证在第一个冒号处拆开用户名和密码），SOCKS5 的用户名和密码各不超过 255 字节。
- **两条出网路径**：带账号密码时，浏览器（Camoufox 的 WAA Worker、浏览器登录与登录验证）经本机 `127.0.0.1` 随机端口上的中转连接上游代理（Firefox 的代理设置里没有账号密码，中转负责加上认证，隧道里的字节原样转发）；后台请求（业务请求、Go 后端的 WAA、Chrome 导入与认证续签）直接带账号密码连接上游代理。两条路径连的是同一个上游，固定 IP 或粘性会话时出口 IP 相同。不带账号密码的代理不经过中转，行为与以前完全一样。
- **动态 IP**：建议在代理商处开启固定会话（粘性 IP）并设置较长的会话时长。按连接轮换 IP 的代理会让浏览器页面与后台请求从不同 IP 出网，会话到期时也会换 IP。
- **测试代理**：默认代理和账户代理的输入框旁有“测试代理”按钮，经代理访问第三方出口 IP 服务 `api.ipify.org`，分别给出浏览器路径与后台请求路径的出口 IP 和耗时，两次 IP 不同时提示代理按连接轮换；只访问 api.ipify.org、不访问 Google，api.ipify.org 会看到你的出口 IP。账户对话框里代理留空时：编辑已有账户测它当前实际的出口，账户原来有自己的代理、在对话框里清空了时测清空后 IP 池会给它的出口（显示端口号或会话号，之后启动的 Worker 可能分到别的端口；IP 池关闭时测默认代理）；新账户（浏览器登录、Chrome 导入）在 IP 池开启时测此刻会分给登录的 IP 池出口（显示端口号或会话号，实际以登录时分到的为准），IP 池关闭时测默认代理；服务停止时也能测试。点“测试代理”测的是输入框里当前的值，不会先保存；设置页在测试进行中不开始“自动应用”的倒计时，测试没通过时也不自动应用，需要时手动点“立即应用”。
- **`https://` 不带账号密码**：浏览器（Camoufox）会以明文 HTTP 代理的方式连接这个代理的 TLS 端口，登录与 WAA 多半不可用（现状）；请改用 `http://` 写法，或使用带账号密码的写法（由本机中转负责与上游的 TLS）。
- **账号密码的保存与显示**：`.env` 与 `account.json` 明文保存代理连同账号密码，文件权限 0600；管理接口返回完整代理（与 `PROXY_API_KEY` 同级，管理页本身需要管理令牌），页面上只显示去掉账号密码的 `协议://host:port` 和“带认证”标记，点开输入框才显示完整内容。命令行 `--proxy` 写的账号密码会被同机其他用户在 `ps` 里看到，建议写进 `.env`。`setup` 只在显式传了 `--proxy` 时把它写进账户的 `account.json`（以前的版本没传 `--proxy` 时也会写入当时的默认代理，这些账户可在“服务配置 → IP 池”一键“改为跟随默认代理”）；写进账户的代理密码更换后，这些账户要逐个修改（或清空账户代理改用默认代理）。用服务器 IP 加端口（例如 `http://服务器IP:2048`）直接打开管理页时是纯 HTTP，读取和保存配置、账户列表与实时事件里的代理账号密码，和管理令牌一样明文传输；建议放在 HTTPS 反向代理后面，或用 SSH 隧道 `ssh -L 2048:127.0.0.1:2048 <服务器>` 后访问 `http://127.0.0.1:2048`。
- **本机中转的安全性**：中转只监听 `127.0.0.1`、端口由系统随机分配，本身没有认证：浏览器运行期间，本机其他进程可以借它经你的代理出网（与 Camoufox 在回环地址上开放的调试端口同一级别）。账号密码不会写进 Camoufox 的临时 profile。

### IP 池：每个浏览器一个 IP

经常返回 429 多半是多个账号从同一个出口 IP 出网。开启 IP 池后，没有自己代理的账户不再统一走默认代理 `PROXY`，而是：

- **静态 IP（`PROXY_POOL_MODE=static`）**：代理商按端口区分 IP（例如 Decodo ISP 代理每个端口一个固定 IP）。只填一个地址，程序把端口换成 `PROXY_POOL_PORT_START` 到 `PROXY_POOL_PORT_END` 里的端口（默认 10001–10200，可自己设置），每个运行中的浏览器（WAA Worker、浏览器登录、登录验证）独占一个端口，浏览器确认关闭后归还。账号尽量回到上次用过的端口（记录在 `runtime/proxy-pool.json`，重启后保留）；端口之后被别的账号用过时不再抢回，账号之间不会来回抢。
- **动态 IP（`PROXY_POOL_MODE=dynamic`）**：代理商按会话号决定 IP（例如 ipfoxy 用户名里的 `sessid-会话号`，会话号不同 IP 不同）。在地址里会话号的位置写 `{session}`（不区分大小写，写在用户名或密码里都可以），程序按账户换成固定会话号：只含小写字母和数字，最长 15 个字符，不会出现下划线这类会被代理商以 407 拒绝的字符。新账户浏览器登录用的会话号在登录成功后成为这个账户的会话号，登录与之后的 Worker 用同一个 IP。

```env
# 静态 IP：这台服务器分到 200 个端口（另一台用 10201–10400，端口段不能重叠）
PROXY_POOL_MODE=static
PROXY_POOL_URL=http://用户名:密码@isp.decodo.com:10000
PROXY_POOL_PORT_START=10001
PROXY_POOL_PORT_END=10200

# 动态 IP
PROXY_POOL_MODE=dynamic
PROXY_POOL_URL=http://customer-账号-cc-US-sessid-{session}:密码@gate.ipfoxy.io:58688
```

说明：

- **优先级**：账户自己的代理（`account.json` 的 `proxy`）> IP 池 > 默认代理 `PROXY`。开启后没有自己代理的账户不再使用 `PROXY`；账户自己的代理或 `PROXY` 指向池里的某个端口时，这个端口记为外部占用，不再分配。
- **“一个账号一个 IP”指同一时刻**：同一时刻每个浏览器独占一个 IP；账户数多于端口数时，每个 IP 随时间先后服务多个账号。浏览器运行期间，这个账户的后台请求（业务请求、续签等）与浏览器走同一个出口；同一账户的重新登录、验证也跟着正在运行的 Worker 走同一个出口（Worker 是开启 IP 池之前启动的、仍走 `PROXY` 时同样走 `PROXY`）。Chrome 导入只在后台完成授权、不开浏览器，每个账号导入完就归还端口，账户建好后记下这个端口，之后的 Worker 回到它。没有浏览器的账户只有少量后台请求（目录同步、复核、续签），按这个顺序选出口：自己上次的端口（之后没被别人用过）、当时空闲的端口（各账户按自己的顺序挑，分散在不同端口上）、`PROXY`，最后才与别人共用；端口被别人占用或开始冷却时立即改到别处，端口用满时退回 `PROXY` 的账户在有端口空出来 30 秒后改回 IP 池（这段时间里多半有 Worker 来占它）。
- **端口要够**：可能同时运行的浏览器数为每个分区（普通、Ultra）的“峰值 + 排空上限”之和，排空上限为 `min(2 × WARM_STARTUP_CONCURRENCY, 峰值 / 4)`（至少 1，排空冷却占位时替补先启动、旧 Worker 后关闭）；登录与验证另占端口（新账户自动处理默认验证并发 3）。建议 `端口数 ≥ Σ(峰值 + 排空上限) + 验证并发`。例如 `MAX_ACTIVE_WORKERS=150`、`ULTRA_MAX_ACTIVE_WORKERS=5`、`WARM_STARTUP_CONCURRENCY=16` 时最多 `150+32 + 5+1 = 188` 个浏览器，加验证并发 3 为 191，200 个端口只余 9 个；调高峰值或启动并发前先看设置页给出的端口需求。端口不够时超出的浏览器与其他账号共用端口（同一个 IP），日志记 WARN，设置页与侧栏标黄。
- **端口段按服务器划分**：每台服务器（每个进程）用不重叠的端口段。同一台机器上的多个进程按端口加文件锁（与账户 runtime 租约同目录：`[用户缓存]/AIStudio2API/runtime-leases/`），不会分到同一端口；`runtime/proxy-pool.json` 按工作目录保存，同一工作目录只运行一个进程。
- **端口冷却**：端口出问题时冷却（第 1 次 15 分钟，之后翻倍，最长 2 小时；24 小时内没有再冷却则档位清零）。冷却中的端口不分给新的浏览器，已经在用的浏览器不受影响。冷却条件：10 分钟内 2 个不同账号在这个端口上返回裸限流 429（不是额度 429），其间没有生成成功（同一个账号反复 429 多半是它自己的额度问题，由账号冷却处理）；或连续 2 次因代理连不通（连不上、超时、407 等）启动失败，其间没有启动成功（与代理无关的启动失败不计）。大量端口同时出错时按代理商整体问题处理，暂不冷却。
- **动态 IP 换会话**：同一账户 30 分钟内 2 次因代理连不通启动失败、距上次换会话至少 10 分钟时换一个会话号（没有浏览器的账户立即换，有浏览器的下次启动时换）。裸 429 不换会话：每个会话各自是一个出口、只有一个账号，出口限流判定（要求同一出口上多个账号被拒，见下一条）在会话上不会成立；单看一个会话也分不清是账号额度用完还是这个 IP 被限制，换会话只会让账号频繁换 IP。因此代理能连通、但被上游限制的会话不会自动换。建议在代理商处设置较长的会话时长。
- **与出口限流判定的配合**：出口限流判定（多个账号同一出口被裸 429 时按出口限速，见下文“出口限流、目录缓存与“需要登录””一节）按账户实际使用的出口分组：静态 IP 每个端口、动态 IP 每个会话各是一个出口（会话号写在密码里也分得开），账户换端口或会话时出口统计随之切换；`PROXY` 为空时账户不会全部算在“直连”一个出口上。由 IP 池分配出口的账户不受“出口限流期间冷启动按配额进行”的限制，扩容与按需提升也不因它们暂用的默认出口（端口用满时退回的 `PROXY`）受限而跳过它们（真正启动时池会另分端口）。端口因 2 个不同账号返回裸 429 而冷却时，这些账号因此被逐级加长的冷却、以及这段时间里因此被标上的坏号（见下文“坏号”）会撤回（被拒的是 IP 不是账号）。只计入被拒像是 IP 问题的账号：自最近一次成功以来只在这个端口上被拒、被拒之前还不是坏号，并且其中至少一个 24 小时内在别的端口或出口上成功过；在别的端口上也被拒、早已是坏号的账号（裸 429 多半跟着账号走）不计入、也不撤回，撤回后换到新端口又被拒的账号不再计入新端口。正用这个端口的 Worker 在同账号的请求结束后关闭、换端口重启；因代理连不通冷却时只做后者。`/api/status` 的 `egress` 在出口多于 16 个时只列出判定成立或已撤销的出口，正常的只计入 `egress_summary`（出口少时照常全部列出）；裸 429 的 `账号冷却` 日志与 `WAA Worker 启动失败` 日志末尾带端口号或会话号。
- **热更新**：管理页“服务配置 → IP 池”修改后立即生效，不重启生成服务、不中断请求（已保存的改动里还有没应用的、需要重启生成服务的设置时，例如默认代理、账户目录，IP 池随那次重启生效，设置页显示“已保存，待应用”）：运行中的 Worker 继续用原来的出口（开启 IP 池或“改为跟随默认代理”之前就在运行的仍走 `PROXY`，侧栏显示“仍走默认代理 N”），关闭后才切换；新启动的 Worker 立即按新设置；没有浏览器的账户立即切换。要立即全部切换，在左下角停止服务后再启动。模式与地址的组合不合法（静态地址含 `{session}`、动态地址缺 `{session}`、地址为空、不写协议又不写端口、端口不合法）或地址格式被服务端拒绝时页面不保存 IP 池（沿用已保存的值，其他设置照常保存），错误显示在地址输入框旁，改了 IP 池设置后消失。
- **抽测与检查**：“抽测出口”用输入框里当前的设置（不必先保存）测 3 个端口（首、中、尾）或 3 个临时会话号的出口 IP（临时会话号的长度取程序可能生成的最大值 15，能提前发现代理商的长度限制）；“检查全部端口”在后台逐个查询端口段里每个端口的出口 IP（后台请求路径，并发 10，最长 5 分钟），列出出口 IP 相同的端口组与失败的端口。账户编辑对话框里代理留空时点“测试代理”测这个账户当前实际的出口（显示端口号或会话号），账户原来有自己的代理、在这里清空了时测清空后 IP 池会给它的出口。都只访问 `api.ipify.org`，不访问 Google。
- **账户页与侧栏**：账户列表的代理列显示 `IP 池 · 端口 10023 · 使用中`（浏览器正在用）或 `IP 池 · 端口 10023 · 后台`（没有浏览器，后台请求暂用），开启前启动的 Worker 显示“默认代理（Worker 重启后切换到 IP 池）”，账户代理与 `PROXY` 相同的标“固定为默认代理（不使用 IP 池）”。侧栏 Worker 区下方显示 `IP 池 37/200 · 冷却 2 · 共用 0`（动态为 `IP 池（动态）· 会话 37 · 换会话 3`），悬停列出冷却端口与最近 10 分钟裸 429 最多的端口；`/api/status` 的 `proxy_pool` 有完整计数与每个账户当前的端口（`assignments`）。
- **`ulimit -n`**：带账号密码的代理经本机中转交给浏览器，浏览器多时连接数很多。Linux 下进程的打开文件数上限建议不低于 65536（启动前 `ulimit -n 65536`，或在 systemd 里设 `LimitNOFILE=65536`）；上限偏低时日志与设置页会提示。
- **`setup` 与“改为跟随默认代理”**：`aistudio2api setup` 没传 `--proxy` 时不再把 `PROXY` 写进账户的 `account.json`（账户跟随默认）；IP 池开启时 setup 登录也从池里取端口（与主进程按端口锁互斥；端口全被占用时与其他浏览器共用一个端口，不退回默认代理），一次 setup 只用一个出口（批量导入 Chrome 的多个账号共用它，要一号一 IP 请在管理页导入）。以前用 setup 建的账户固定了 `PROXY`，不会使用 IP 池：设置页 IP 池分组会提示数量，点“改为跟随默认代理”一次清空这些账户的账户代理（运行中的 Worker 不重启，关闭后才改用 IP 池；正在被其他操作使用的账户会跳过，稍后再点一次）。程序不会自动改：有人是故意固定的。
- **账号密码**：`PROXY_POOL_URL` 与 `PROXY` 一样明文保存在 `.env`（权限 0600），只在 `/api/config` 返回给管理页编辑，页面按掩码显示；日志、状态接口、账户列表、`runtime/proxy-pool.json` 与端口锁的文件名里只有 `协议://主机`、端口号与会话号，没有用户名和密码。

### 认证文件管理

认证文件默认存储在 `auth/` 目录：

| 路径 | 内容 |
| --- | --- |
| `auth/<Google 邮箱>/account.json` | 账户邮箱、代理、语言、时区和启用状态（没有 `enabled` 字段按启用；为 `false` 但没有手动停用记录的，启动或导入时改为启用并写回）；代理连同账号密码明文保存，文件权限 0600 |
| `auth/<Google 邮箱>/storage-state.json` | Google Cookie 与认证续签材料 |
| `auth/<Google 邮箱>/runtime-state.json` | 权益等级、模型资格、冷却状态与资源所属账户 |
| `auth/.permission-denied.json` | 403 无权限账户列表（账户、模型、通道、次数与时间），认证文件更新后自动清除对应账户 |
| `auth/<Google 邮箱>/camoufox-cache/` | 该账户浏览器的网页缓存，服务停止时可删除 |
| `auth/.leases/<Google 邮箱>.lock` | 同一账户目录的跨进程占用锁 |
| `[用户缓存]/AIStudio2API/runtime-leases/<Google 邮箱>.lock` | 当前电脑上该邮箱的 WAA Worker 占用锁 |
| `[用户缓存]/AIStudio2API/runtime-leases/proxy-port-<代号>-<端口>.lock` | IP 池静态端口的跨进程占用锁（代号由代理主机与用户名算出，不含账号密码） |
| `runtime/proxy-pool.json` | IP 池记录：账户上次用过的端口、端口归属与冷却、动态 IP 的会话号；不含地址与账号密码 |

账户邮箱同时作为目录名、管理页面标识和日志来源，统一使用小写形式。`.leases` 协调账户目录读写，用户缓存中的 runtime lease 保证同一邮箱在当前电脑上只有一个 WAA Worker。多个进程共用一个账户目录时，停用记录（`.disable-reasons.json`）与 403 无权限账户列表（`.permission-denied.json`）由各进程按自己内存中的内容整份写出、不做合并（后写的覆盖先写的），识别同一登录会话重新导出的认证文件也假定只有一个实例在写回认证文件。

账户页支持 Chrome 批量导入和隔离 Camoufox 登录。`ready` 账户可以编辑、停用、验证和删除，`auth_required` 账户可以重新登录。验证会说明结论（有效，或登录已失效的原因），批量验证分别计“有效 / 登录失效 / 出错”（见[出口限流、目录缓存与“需要登录”](#出口限流目录缓存与需要登录)）。“一键启用”和选中账户的“启用”调用批量启用接口 `POST /api/accounts/bulk-enable`，每 500 个账户一批依次提交（服务端每次最多 1000 个），进度面板按批更新，可以在两批之间停止：每个账户只改写 `account.json` 的启用状态，整批只合并、推送一次账户与模型快照（原先每启用一个账户就重算并推送一次全部账户，上万个账户要几个小时）；正在验证、登录或处理请求的账户等 3 秒拿不到就列为失败（“账户正忙”），稍后重试即可。启用后没有模型目录的账户在后台按 16 个并发同步目录，同步完才能预热、接请求，登录已失效的随之显示为“需要登录”。单个账户启用后同样会补一次目录同步。

## 详细文档

- [开发与贡献](docs/development.md)
- [Google AI Studio 协议规范](docs/protocol.md)
- [WAA 实现](docs/waa.md)
- [Build 通道](docs/build.md)
- [运行日志说明](docs/logging.md)
- [可复用逆向开发指南](docs/reverse-engineering.md)

## 重要提示

### 关于 Camoufox

本项目使用 [Camoufox](https://camoufox.com/) 浏览器来降低被检测为自动化脚本的风险。Camoufox 基于 Firefox，通过修改底层实现来保持真实的设备指纹。

Go 负责编码、调度、流式解码与公开协议；受 WAA 保护的 `GenerateContent` 通过账户固定指纹 Camoufox 页面发送，保留原生 Firefox TLS/HTTP2、请求头、Cookie 与页面指纹。

`WAA_BACKEND=go` 时，WAA 在服务进程内运行，按账户指纹模拟 Firefox 页面环境并以 Firefox 请求头直接发送，运行时不下载也不启动 Camoufox。账户页的浏览器登录仍使用 Camoufox，首次登录时按需准备。

### 使用限制

- **客户端管理历史**: Chat、Anthropic 和 Gemini 请求由客户端提交完整对话上下文
- **AI Studio 历史**: API 请求不保存到官网历史；`TEMPORARY_CHAT=true` 还会关闭 WAA 预热页的自动保存
- **Responses 与 Interactions 会话**: `previous_response_id` 与 `previous_interaction_id` 仅在当前进程内保存，重启后不会保留
- **认证有效期**: Chrome 导入账户保留 DBSC 续签材料；隔离登录账户失效后在账户页重新登录

## 故障排除

### Windows 端口被系统保留

如果启动时提示 `LISTEN_ADDR` 配置的端口被占用，任务管理器中又找不到占用进程，可能是 Hyper-V、WSL2 或 Docker 的 NAT 服务保留了端口段。

以下命令需要在管理员权限的 PowerShell 或 CMD 中运行。

#### 1. 查看被 Windows 保留的端口范围

```powershell
netsh interface ipv4 show excludedportrange protocol=tcp
```

如果 `2048` 落在输出的 `Start Port` 和 `End Port` 范围内，可以修改 `LISTEN_ADDR`，或重启 WinNAT 服务后再次检查：

```powershell
net stop winnat
net start winnat
```

端口空闲后，也可以将 `2048` 加入持久保留：

```powershell
netsh int ipv4 add excludedportrange protocol=tcp startport=2048 numberofports=1 store=persistent
```

常见运行状态：

| 状态 | 处理方法 |
| --- | --- |
| 页面未自动打开 | 手动打开 `.env` 中 `LISTEN_ADDR` 对应的地址 |
| `service_stopped` | 在管理页面点击“启动服务” |
| 没有可用账户 | 在账户页新增、启用或重新登录账户 |
| Camoufox 准备失败 | 检查 GitHub Release 访问，或设置 `CAMOUFOX_PATH` |
| Linux 预热账户 `exit status 255` | 安装 Camoufox 运行库，见“系统要求”中的 apt 命令 |

### 主用模型返回 429 或首字很慢

先看管理页侧栏（或 `/api/status` 的 `primary_supply`）里该模型的一行“主用 <模型>：热 N · 冷却 C · 备用 M”：

- **备用 0（黄色）**：普通号池里已经没有还能服务该模型的空闲账号，多半是可用账号当天的该模型额度已经用完。冷却占位淘汰后换不上能服务它的替补，请求只能等仍能服务它的常驻 Worker 空出，排队超过 `PRIMARY_QUEUE_WAIT` 就返回 429。额度在太平洋时间零点重置（北京时间夏令时 15:00、冬令时 16:00），之后供给自动恢复；在此之前只能加入新的账号，或减少该模型的请求。
- **备用不为 0 但热很少**：检查 `WARM_WORKER_LIMIT`，它应不小于希望常驻的 Worker 数。低于驻留数时，淘汰或回收空出的槽位预热不补，超出的空闲 Worker 5 分钟后被回收，主用模型的请求要等现场冷启动。
- **“冷却占位”与“排空中”长时间不下降**：在运行日志中搜索“替补跳过”（分区内没有能服务这些模型、没有冷却的备用账号）与“排空超时”（固定账户或文件绑定的请求一直在用该 Worker）。另有两种情况不写日志，属于正常：排空中的 Worker 上有开始输出后的长流时，排空保持到流结束，最长到总时长上限（默认 1 小时），“排空超时”要到总时长上限 + 30 秒后才出现；“排空中”已达 2 × 排空名额（`drainLimit`，为 `max(WARM_STARTUP_CONCURRENCY, 1) × 2`，且不超过分区峰值的 1/4、至少 1）时，其余冷却占位要等其中一个排空结束才排空，期间照常服务其他模型。排空让出的名额已满时，新排空的替补因槽位已满没有启动（日志“替补未启动 | 原因=排空”，错误为槽位已满），旧 Worker 关闭后补上。
- **Ultra 主用模型的 429**：看 Ultra Worker 下面的“Ultra 主用 <模型>”一行，口径同上，只统计 Ultra 号池。这些模型没有额度时立即返回 429（见[主用模型](#主用模型)），运行日志里对应“Ultra 主用模型候选全部冷却，立即返回 429”或“Ultra 主用模型额度不足，立即返回 429”，后者写明原因与额度失败次数；客户端按 `Retry-After` 稍后重试即可。
- **冷却很多、热很少**：常驻 Worker 大多对该模型在冷却，多为连续限流或当天额度用完；号池繁忙的 429 看下一节的候选快照与连续限流，或直接运行 `bash tools/diag_busy.sh`。
- **侧栏出现“出口限速”或很多账号同时冷却**：多半是同一出口 IP 被上游整体限流，换号与加账号都无益，见[出口限流、目录缓存与“需要登录”](#出口限流目录缓存与需要登录)；需要登录的账户突然增多时看同一节的登录复核。

### 号池繁忙 429 与连续限流

**号池繁忙 429 是什么**：主用模型（普通路径为 `PRIMARY_MODELS`，`/ultra` 为 `ULTRA_PRIMARY_MODELS`）的请求在排队上限（`PRIMARY_QUEUE_WAIT`、`ULTRA_PRIMARY_QUEUE_WAIT`）内没有等到空闲的账号或 Worker 槽位，立即返回 429 与 `Retry-After`，让客户端稍后重试，而不是一直排队到 `REQUEST_TIMEOUT`。它表示这一刻能服务该模型的容量不够，不是上游的额度错误；客户端收到的消息与额度不足相同（`Resource has been exhausted (e.g. check quota).`），区别只在管理日志：`request.error`（用量账本里同样）以“号池繁忙：模型 … 暂时没有空闲的账号或 Worker”开头，写明已排队、上限、总耗时、第几次尝试与最后等待原因；请求时间线里的“主用模型排队超过 10s，立即返回 429”一行另带这一刻的候选快照（字段见[运行日志](docs/logging.md#排队等待)）：

- `热空闲`、`热忙碌`、`备用`、`备用忙碌`：已有 Worker 的账号与备用账号，冷却中的不计；`每分钟已满` 为其中因 `ACCOUNT_RPM_LIMIT` 按忙碌计的；`启动中=是` 为有候选的 Worker 正在启动；`最早恢复` 为候选中最早恢复的冷却还剩多久。
- `驻留冷却`：对该模型冷却中的常驻 Worker 数，即 `/api/status` 里 `primary_supply` 的 `cooling`（约 30 秒统计一次）。

怎么读：`驻留冷却` 不小于 `热空闲` 加 `热忙碌` 时，常驻 Worker 大多对该模型在冷却（多为下面的连续限流或当天额度用完）；`热忙碌` 大多是 `每分钟已满` 时，每分钟上限是瓶颈；`备用` 与 `备用忙碌` 都为 0 时，号池里已经没有还能服务它的空闲账号；其余多是并发超过了 Worker 数 × 单账户并发（见上一节）。

**连续限流递增冷却**：上游对额度已用完的账号常只返回裸的 `Quota exceeded`（没有额度周期，也没有重试时间）。这类 429 原先只冷却 30 秒，账号每 30 秒恢复、被选中、再失败一次，它的常驻 Worker 也因为冷却太短既不算冷却中、也成不了冷却占位，换不上新的账号。现在同一账号同一模型在冷却到期后再次遇到这种 429 就升一档：

| 档位 | 冷却 | 常驻 Worker |
| --- | --- | --- |
| 第 1 档 | 30 秒 | 剩余不到 2 分钟，不算冷却中，不淘汰 |
| 第 2 档 | 5 分钟 | 剩余 2 分钟以上、所有支持的启用通道都在冷却时算冷却中，分区满时可以被替换成备用账号 |
| 第 3 档 | 30 分钟 | 剩余不少于 15 分钟、所有启用通道都在冷却时是冷却占位，排空淘汰后由替补顶上 |
| 第 4 档 | 2 小时 | 同上 |
| 第 5 档起 | 到太平洋时间次日零点 | 同上 |

- 每档都不超过太平洋时间次日零点；该账号在这个通道上成功一次即回到第 1 档（此前到第 2 档以上时运行日志写“限流档位清零”），跨过额度日也从第 1 档起。
- Build 与 Playground 两个通道没有判定为独立额度时（运行日志的“每日额度通道判定”为共用或观察中，或还没有记录）共用一份档位，并同时冷却另一个启用通道；判定为独立时各通道分别计档。
- 不升档的情况：已经向客户端输出内容后才遇到的 429（照常冷却）；冷却写入前就已发出的并发请求，或同一请求换通道再试时遇到的 429。
- 号池级保护：同一号池 10 分钟内至少 5 个账号对同一模型返回这种 429、期间又没有任何成功时，视为上游整体限流或故障，只按第 1 档冷却、不升档，运行日志写“号池级限流”（每 10 分钟最多一条），免得把整个号池逐级冷却到几个小时。这时换号无益，减少并发或等上游恢复。
- 档位只保存在内存里，重启后从第 1 档起；冷却本身照常写入各账户的 `runtime-state.json`，重启后恢复。
- 上游给出重试时间（RetryInfo、错误文案里的 `retry in <时长>`、`Retry-After`）、带分钟或每日周期，或按账号全局计的 429 不逐级，按原样冷却；复制文件、上传附件时返回的 429 与模型额度无关，也不逐级，只冷却这个通道 30 秒。

运行日志里这类冷却的一行带 `连续限流=第 N 档`，两个通道同步冷却时另带 `同步=两个通道`（见[运行日志](docs/logging.md#额度冷却日志)）。

**三种额度 429 的识别**：上游私有详情 GeminiApiErrorDetails 里的指标、限额值与模型，以及文案里的 `Please retry in <时长>` 都会被解析：

- free 账号的每日限额（文案 `You exceeded your current quota …`，每天 10 次）：按每日限额冷却该账号的这个模型，到太平洋时间次日零点与文案里 `retry in` 的时间中较晚的一个，日志带 `上限=10 | 指标=generate_requests_per_model_per_user | 模型=gemini-3.1-pro`。
- pro/ultra 账号的每日限额（文案 `You've reached your usage limit …`，每天 50 次）：按每日限额冷却到太平洋时间次日零点，日志带 `上限=50`；不再当作裸限流 30 秒、逐级加长（原先同一账号要白试几次、过 5 分钟以上才淘汰）。
- pro/ultra 账号每分钟 12 次的限额（所有模型、两个通道共用）：按分钟限额冷却整个账号到这一分钟结束，日志带 `上限=12 | 指标=generate_requests_global`；`ACCOUNT_RPM_LIMIT` 提前限速。

运行日志里出现 `额度 429 详情未识别` 时，请把整行发给开发者：这种 429 带着 GeminiApiErrorDetails，但布局与上面几种不同、没有解析出指标（冷却日志带 `详情类型=`、没有 `指标=`），冷却只能按文案判断（裸 `Quota exceeded` 按裸限流逐级冷却），这一行带着详情原文，用来扩展解析（同一种 10 分钟最多一条，普通日志占满日志队列时它也不丢，见[运行日志](docs/logging.md#额度冷却日志)；`bash tools/diag_busy.sh` 的第 3 段也会列出这些行）。

每日限额在两个通道已判定共用额度时（运行日志的“每日额度通道判定”为共用）同时冷却另一个通道，常驻 Worker 立即登记淘汰（冷却剩余不少于 15 分钟时），请求立即换号、不退避；观察中时先在同账号的另一个通道再试一次。`模型=` 是上游计额度用的模型，可能与请求的模型名不同（如请求 `gemini-3.1-pro-preview`、日志为 `gemini-3.1-pro`）。没有指标的 429 另带 `详情类型=`（上游附带的详情类型），用来核实裸 `Quota exceeded` 是否带了没有解析的额度详情。

**一键诊断**：`bash tools/diag_busy.sh`（在程序目录下运行，或用 `APP_DIR=/path/to/app` 指定程序目录）汇总号池繁忙的原因，最后给出自动结论：

```bash
bash tools/diag_busy.sh                         # 最近 60 分钟，重点分析主用模型中号池繁忙最多的一个
MINUTES=120 MODEL=<模型> bash tools/diag_busy.sh  # 扩大统计窗口、指定模型
QE_MINUTES=720 bash tools/diag_busy.sh          # 裸 Quota exceeded 逐账号分析回看 12 小时（默认 6 小时）
OFFLINE=1 LOG_FILE=<日志> LEDGER_DB=<账本> DIAG_NOW=2026-10-22T08:00:00Z bash tools/diag_busy.sh  # 分析日志与账本的副本
```

- 内容：配置与当前容量（含主用模型供给的 `cooling`）、各模型的请求结果、号池繁忙的最后等待原因与此前的上游失败、排队超限时的候选快照归因、上游 429 的文案分类、连续限流各档的次数与清零、号池级限流、当前冷却的档位分布、裸 Quota exceeded 逐账号判断（每日额度用完还是短期限流）、常驻 Worker 的冷却与变动、每分钟上限。`ACCOUNTS=0` 不读取 `/api/accounts`（账号上万时这个接口较大，不读时少了按权益分组）。
- 只读：只对本机管理接口发 GET 请求（自动读取 `.admin-token`），用量账本以只读方式打开，不改配置、冷却、账号与 Worker，也不在程序目录里新建文件，临时文件放在系统临时目录、退出时删除；管理接口连不上时只分析运行日志与用量账本，python3 没有 sqlite3 模块时只分析运行日志。账号已打码，输出可以直接发给开发者。

**Worker 起不来**（侧栏“启动中”一直不变成就绪、Worker 长时间为 0）：运行 `bash tools/diag_start.sh`，依次输出系统资源、Worker 与预热、IP 池与出口的实时状态，绕过程序直连测 IP 池首中尾 5 个端口（出口 IP、打开 AI Studio 的状态与耗时），以及最近 15 分钟（`LOG_MINUTES` 可改）Worker 启动、就绪耗时、失败原因（按端口汇总）与代理日志；最后一段“失败时的页面状态”取出启动失败、`WAA Worker 重建` 与运行中准备 proof 失败（含 Live 通道）的 `账号切换` 日志末尾的页面快照 `页面=…`（失败那一刻页面的地址、标题、输入框与 Run 按钮的个数、弹层与提示条文字、当前文档与导航至今的秒数等，字段见[运行日志](docs/logging.md#失败时的页面快照)），按路径、标题、输入框可见数、Run 按钮可见数与弹层文字分组计数，列出前 6 组，每组附 `nav`、`hook`、导航后是否换过文档与输入框所在组件的分布，并按失败原因各附一条完整快照（失败原因里的页面表达式异常折叠成报错正文），用来判断页面是改版、一直没加载出来还是中途刷新或跳转。只读，结果同时写到 `logs/diag_start.txt`，代理密码与账号邮箱已打码。

老账号起不来、新账号能跑时，再运行 `bash tools/diag_profile.sh`：按最近 60 分钟（`LOG_MINUTES` 可改）的启动结果把账号分成只失败的与成功过的两组，对比各组账户目录里 `storage-state.json` 的 aistudio.google.com localStorage（只列键名与值长度）、Cookie 数与文件时间，以及 `camoufox-cache` 缓存的有无、大小与时间。只读，不访问网络，结果同时写到 `logs/diag_profile.txt`。

### 出口限流、目录缓存与“需要登录”

号池账号很多、却仍然频繁 429 时，多半不是账号额度用完，而是同一出口 IP 的请求太多被上游整体限流，或者重启、目录同步与“需要登录”误判把可用账号挤掉了。以下行为不需要配置，阈值是固定值。

- **429 的原因**：本服务判定的 429（排队超限、候选全部冷却、换号次数用完、出口限流）都带 `Retry-After`（客户端按它重试即可），429 另带响应头 `X-AIStudio2API-Reason` 说明来源：`local_queue`（本地排队超限，请求多半没有发往上游）、`local_cooling`（候选账号全部冷却）、`upstream_quota`（上游额度或限流 429，含换号次数用完）、`egress_limited`（所在出口正在限流）。对外的消息与状态码不变，四种协议的流式与非流式 429 都带这个头。
- **普通号池的换号上限**：不固定账号的请求遇到额度 429 时，每日类最多再换 4 个账号，限流类（裸 `Quota exceeded`、分钟限额）最多再换 2 次，每个请求最多试 8 个账号；因登录失效换号最多 2 次。达到上限时立即返回 429 与 `Retry-After`，不再把号池逐个试一遍（原先一个请求可能连续换十几个账号，把更多账号拖进冷却）。Ultra 主用模型的规则不变（见[主用模型](#主用模型)）。
- **出口限流**：一个出口（账号代理规范化后的值，没有代理为直连）2 分钟内有足够多的账号同时被裸 429、新换上的账号第一次请求就被拒时，判定该出口被上游限流：这时换号无益，逐级冷却与按需冷启动暂停，请求按出口限速排队发送，主用模型优先（非主用模型保底约 1/4），限流解除后自动恢复。管理页侧栏显示“出口限速 <出口> · 上游拒绝 R% · 本地限速拒绝 N/分钟 · 排队 Q · 限速 L/分钟”（只有 Ultra 号池的通道在限速时写“Ultra L/分钟”，两条通道都在限速时分别写“普通 L1/分钟 · Ultra L2/分钟”）：“上游拒绝”是发往上游的请求被拒的比例，“本地限速拒绝”是在本地排队超过上限直接返回的 429（响应头 `egress_limited`）。判定不对（例如其实是这些账号当天额度用完）时点行尾的“撤销判定”，交回逐级冷却，30 分钟内不再判定，“恢复判定”取消撤销；被拒的账号再试都不成功时服务也会自己撤销。
- **出口的吞吐上限**：同一出口每分钟能成功的请求数由上游决定，账号再多也提高不了；要提高上限只能让请求分散到多个出口：给账号设置不同的代理（在账户页编辑账户时设置账户专用代理，见[代理配置](#代理配置)），或开启 IP 池。一个代理地址（去掉密码，保留用户名）算一个出口、按一个出口 IP 计：轮换 IP 的代理只要地址相同仍算同一个出口，按用户名选出口的代理用不同用户名即为不同出口。开启 IP 池（见[代理配置](#代理配置)）后，没有自己代理的账户按 IP 池分到的出口计：静态 IP 每个端口、动态 IP 每个会话各算一个出口（会话号写在密码里也分开算）。
- **重启不再全量同步目录**：模型目录保存在 `runtime/model-catalogs/`，重启直接载入，后台只低速刷新缺失、超过 24 小时与疑似失效的账户（只刷空闲账户，遇到上游 429 或出口限流时暂停），重启后预热立即开始，出口不再被启动时成千上万次目录请求打满。因此重启也不再顺带清掉账号的全局冷却（到期自动恢复），后台刷新目录也不清；“需要登录”不写盘，重启后归零，随使用逐渐回升（由下面的复核把误判的恢复）。侧栏“目录同步暂停到…”“目录待同步 N · 疑似失效待探测 M”只在暂停、有缺失或有疑似失效待探测时显示。
- **不可调度的驻留 Worker 立即释放**：驻留 Worker 的账号需要登录、已停用或没有模型目录时，它服务不了任何模型却占着槽位；现在空闲即关、忙碌等请求结束，槽位由预热补上，侧栏 Worker 一行显示“不可调度 N”。
- **“需要登录”的误判收紧与自动恢复**：一次上游 401 不再直接把账号标为需要登录，先写 5 分钟冷却、记“疑似登录失效”，再用认证文件里的 Cookie 复核一次，仍是 401 才标记，通过就解除冷却；带图请求换取附件授权（`GenerateAccessToken`）返回 401 说明账号没有授权 Google Drive，附件改为内联发送，不冷却、不换号，也不标记需要登录；文件上传、下载、删除等其他需要 Drive 的请求只让这次尝试失败（不固定账户的上传照常换号），同样不冷却。已标为需要登录的账户每 15 分钟低速复核一轮，接口认证恢复正常就自动回到就绪，仍失效的逐渐拉长复核间隔（最长 6 小时）；管理验证判定未登录、Worker 启动时跳到登录页的账户是浏览器判定失效，不靠接口复核恢复，需要重新登录或更新认证文件。出口限流期间不复核。侧栏“登录复核 · 疑似失效 N · 10分钟新增需要登录 M · 坏号 K”在有疑似、最近有新增或有坏号时显示；10 分钟内新增很多时多半是出口或上游问题，而不是账号真的失效。
- **坏号**：连续 2 次裸限流、期间没有成功的账号 2 小时内不再用于预热、扩容与替补（还有其他候选时），成功一次即恢复；IP 池端口因不同账号返回裸限流而冷却时（被拒的是 IP），计入这次冷却的账号（自最近一次成功以来只在这个端口上被拒）在这段时间里标上的坏号撤回；在别处也被拒、冷却前已是坏号的不撤回。预热与替补先按能服务的主用模型个数、其次热门模型个数排序，个数相同时优先选最近成功过的账号：近期成功过、但对主用模型冷却（如单模型 403 无权限，仍能服务其他模型；剩余不到 2 分钟的短冷却不算）的账号排在能服务主用模型的账号之后；请求为其他模型按需扩容、现场启动备用账号时同样先比主用模型、再比热门模型（只去掉坏号，不看最近成功）。记录保存在 `runtime/model-catalogs/bad-accounts.json`，重启后保留。
- **按需替换的节奏**：刚就绪不到 2 分钟的 Worker 不会被按需替换掉，每个分区每分钟最多 10 次按需替换，避免请求高峰时 Worker 不停地开了又关。

账户页的“验证”现在说明结论：浏览器已登录时再用接口复核一次，单个验证按结果提示“验证通过”或“登录已失效：<原因>”（浏览器判定未登录时原因带跳转到的登录页，如 `accounts.google.com/v3/signin/identifier`；浏览器已登录但接口认证失败时为“Cookie 可能已被 Google 作废，需要重新登录”）；批量验证分别计“有效 / 登录失效 / 出错”，并列出登录失效的账户与原因，不再“没出错就算成功”。

`bash tools/diag_busy.sh` 的第 0 段显示出口判定、目录同步与登录复核的现状，第 9 段汇总这些日志并在结论里指出出口限流、目录同步暂停或登录复核确认偏多等情况，另有“附件授权”一小段：Drive 上传成功（“内联附件处理完成”，含 Build 通道直接携带附件的请求）、改为内联发送与 `GenerateAccessToken` 401 换号的行数（旧版本的附件尝试，以及新版本里仍需要 Drive 的转写上传与跨账户复制文件引用；窗口内有“Drive 未授权，附件改为内联”即按新版本解读），以及 401 的账号里窗口内没有“内联附件处理完成”的有多少；日志字段见[运行日志](docs/logging.md#429-治理)。

### Ultra 上 gemini-4-argon 首字很慢

`gemini-4-argon` 需要订阅权益、只在 Build 通道提供，首字慢通常是下面几个原因叠加：

- **Build 单次调用**：需要订阅权益的模型在 Build 通道经单次调用生成，整段回复生成完才一次返回，流式请求也要等到这时才收到第一个字，首字时间约等于整段的生成时间。管理页模型目录（`GET /api/models`）里 `build_unary` 为 `true` 的模型都是这样；它没有 Playground 通道，`STREAM_PLAYGROUND_MODELS` 帮不上忙。
- **默认最高思考**：模型名不带后缀时思考强度设为最高（HIGH），只有客户端显式传了 `reasoning_effort` 等思考参数时才以客户端为准；思考越长，单次调用返回得越晚。
- **分钟限额冷却**：按模型计的分钟限额只冷却该账号的这个模型，很快恢复，但 Ultra 号池的账号少时容易全部冷却。作为 Ultra 主用模型（默认）时立即返回 429 与 `Retry-After`；从 `ULTRA_PRIMARY_MODELS` 去掉后，请求会原地等待 1 分钟内恢复的冷却。另有按账号计的分钟限额（`quota_limit=GeneratePerMinutePerProjectPerUser`、`quota_limit_value=12`：每个账号每分钟最多 12 次生成请求，所有模型、两个通道共用），撞上后整个账号冷却到这一分钟结束；`ACCOUNT_RPM_LIMIT`（默认 10）让单账号最近 60 秒发满这么多次就先换号、热 Worker 都满时扩容，提前避开它，429 冷却仍然兜底。运行日志里搜 `类型=分钟限额` 可以看到这类冷却，带 `上限=` 与 `指标=`（见[运行日志](docs/logging.md)）。
- **额度 429 换号与冷启动**：不是 Ultra 主用模型时，额度 429 后会把 Ultra 号池逐个试一遍，换到没有 Worker 的备用账号还要现场冷启动浏览器（每次 10～30 秒以上）；作为 Ultra 主用模型时，每日额度用完最多再换 2 次，分钟限额与限流最多再换 5 次，额度失败后的换号只用已启动的 Worker。

排查：

```bash
./tools/slow_requests.sh 24 30 gemini-4-argon
```

从用量账本列出最近 24 小时首字最慢的 30 个该模型请求，把首字拆成排队（取得最终执行账号之前的时间：排队、换号、等 Worker 与现场冷启动）与上游（首事件减排队）两段，并列出每次尝试的账号、通道、失败原因与耗时：排队长、尝试多是额度换号与冷启动；排队短、上游长是单次调用本身的生成时间（思考加正文）。

处理：

- 保持 `FIRST_EVENT_TIMEOUT=0`（默认）：单次调用的第一个事件就是整段回复，开启首事件超时会把正常但较长的生成中途放弃再换号，白白消耗额度。
- 缩短思考：用 `-low`、`-medium` 后缀（如 `gemini-4-argon-low`，后缀优先于客户端参数），或在客户端传 `reasoning_effort`（`low`、`medium`）。
- 确认 `ULTRA_PRIMARY_MODELS` 里有它（默认就有），没有额度时让客户端按 `Retry-After` 重试，而不是在服务里换号、冷启动。
- Ultra 账号少、常常全部冷却时，可以开启 `ULTRA_INCLUDE_PRO` 让 Pro 账户一起服务 `/ultra`，前提是 Pro 账户的 Build 目录里有它（开启后看侧栏“Ultra 主用 gemini-4-argon”的备用数是否计入了 Pro 账户），同时调大 `ULTRA_WARM_WORKER_LIMIT` 与 `ULTRA_MAX_ACTIVE_WORKERS`，见 [Ultra 号池](#ultra-号池ultra-前缀)。

### 长回复中途截断或重新生成

`REQUEST_TIMEOUT` 只限制首个上游事件之前的阶段（排队、冷启动、WAA proof、等首字）。开始输出后，只要上游还在出事件或有网络字节就不会切断：上游连续 `REQUEST_TIMEOUT` 既没有事件也没有字节才以 504 结束，整条请求最长 1 小时（`REQUEST_TIMEOUT` 更长时按它）。输出开始后的截断照旧以错误结束，不会伪造正常结束。客户端看到的文案都是 504 `Deadline expired before operation could complete.`（流式时在流内），来源要看管理日志的 `request.error` 或用量账本的 `error`：

| `error` | 来源 | 处理 |
| --- | --- | --- |
| `本服务首事件前期限 <期限> 到期[（停在 <位置>）]: …` | 首字之前就到时：排队、现场冷启动，或隐藏思考（Anthropic `thinking.type=disabled`、Gemini `includeThoughts=false`：Build 通道上游不返回思考，Playground 通道上游返回但本服务丢弃）时的长思考。在某次尝试中到期时带 `（停在 …）`，文件资源刷新时到期带 `（停在 文件资源刷新）`；排队或回到原账号等待时到期为 `…到期: 排队 … 后请求超时（…）`、`…到期: 回到原账号等待 … 后请求超时（…）`；其他不在尝试中到期的情形（如换号退避中、上一次为超时类错误）为 `…到期: <上一次错误>` | 调大 `REQUEST_TIMEOUT`，或排查排队（`bash tools/diag_busy.sh`） |
| `本服务首事件后空闲期限 <期限> 到期（已输出 推理=N 正文=M）: …` | 开始输出后上游卡住：连续 `REQUEST_TIMEOUT` 没有事件也没有字节 | 看请求时间线里最后一次 `事件流停顿` |
| `本服务总时长上限 1h0m0s 到期（…）: …` | 整条请求超过总时长上限 | 需要更长时调大 `REQUEST_TIMEOUT` |
| `… 返回 HTTP 504、协议错误码 4: …` | Google 侧自己的期限 | 缩短思考或输出 |
| `AI Studio stream closed before finish` | 上游在结束帧之前断流 | 看同账号同一时刻的告警 |
| 只有 `context deadline exceeded` 本身（没有“本服务”前缀） | 旧版本的整段期限，或计数、视频、转写请求。带 `AI Studio … 协议位置` 或 `BiDi … 等待回复超时` 的是上游或浏览器链路超时，不是本服务期限 | 升级；链路超时看同账号同一时刻的告警 |

“截断后又重新生成”在本服务这边是相同输入、请求 ID 不同的新请求：收到首个上游事件后本服务不会再换号或重做，重新生成来自客户端或中转。最常见的是 new-api 的 Claude 类型渠道开了失败重试：它把流内错误改写成 500，在同一个连接里重新请求，客户端在一条回复里先看到截断的前半段，再看到从头生成的第二段。把 new-api 的重试次数设为 0，或把接本服务的渠道改成 OpenAI 类型。非流式请求在生成完之前没有任何字节，前面有 Cloudflare 时约 100 秒返回 524（本服务记为约 100 秒的取消 499），客户端重发就是整段重新生成，长思考模型请用流式。完整说明（含 new-api 不转发 `: ping` 时的处理、Worker 层的影响）见[长生成与中转重试](docs/development.md#长生成与中转重试)。

**一键诊断**：`bash tools/diag_regen.sh`（在程序目录下运行，或用 `APP_DIR=/path/to/app` 指定程序目录）按上表给截断分类，找出相同输入的重发，最后给出自动结论：

```bash
bash tools/diag_regen.sh                          # 最近 24 小时，重点模型取截断最多的模型
MINUTES=4320 MODEL=<模型> bash tools/diag_regen.sh  # 扩大统计窗口、指定模型（MODEL=all 看全部模型）
RETRY_GAP=600 bash tools/diag_regen.sh            # 前一个请求结束后 600 秒内的相同输入算重发（默认 300）
OFFLINE=1 LOG_FILE=<日志> LEDGER_DB=<账本> DIAG_NOW=2026-10-22T08:00:00Z bash tools/diag_regen.sh  # 分析日志与账本的副本
```

- 内容：相关配置与 nginx 超时设置、各结束原因与耗时分布（含扎堆点）、本服务期限截断的阶段与已输出事件数、相同输入的重发（客户端或中转重试）与服务端首事件前的换号、日志关键字（`本服务期限截断`、`事件流停顿`、`协议错误码 4`、`stream closed before finish` 等）、服务停止或改配置前后被打断的请求。
- 只读：只对本机管理接口发 GET 请求（自动读取 `.admin-token`），用量账本以只读方式打开，只读 nginx 配置文件，不改配置、冷却、账号与 Worker，也不发生成请求；临时文件放在系统临时目录、退出时删除；管理接口连不上时只分析运行日志与用量账本，python3 没有 sqlite3 模块时只分析运行日志。输出不含提示词原文，账号已打码。

### 错误响应格式

返回给客户端的错误统一使用各协议的官方结构（OpenAI 的 `error.message/type/param/code`，Anthropic 的 `type: error` 加 `request_id`，Gemini 的 `error.code/message/status`），消息使用 Google Gemini API 的官方英文措辞；内部详细原因（账号、上游协议细节、中文说明）只写管理日志。

| 情况 | 状态码 | 返回的消息 |
|---|---|---|
| 输入超过模型 token 上限（提示词过长）等参数错误 | 400 | 谷歌原文（去掉 `[original: …]`、`(qos=…)` 等内部标记）；OpenAI 附 `code: context_length_exceeded`，Anthropic 以 `prompt is too long:` 开头 |
| 输入超过账号的输入上限（部分账号的 Playground 与 Build 另有上限，如 131072 token，上游文案为 `The input token count exceeds the maximum number of tokens allowed 131072`） | 400 | 与安全拦截相同：`Prompt was blocked due to prohibited content. (blockReason: PROHIBITED_CONTENT)`，OpenAI 附 `code: content_policy_violation`，不向客户端暴露账号上限；上游按 429 返回时同样按 400 返回，不冷却账号，只改走 Build、改用更高权益账号各再试一次；管理日志仍记录“输入超过模型上限（提示词过长）” |
| 输入被安全策略拦截 | 400 | 如 `Prompt was blocked due to prohibited content. (blockReason: PROHIBITED_CONTENT)` |
| 模型不存在或没有账号能提供 | 404 | `models/<模型> is not found for API version v1beta, …` |
| 号池全部冷却、上游限流、主用模型排队超限、Ultra 主用模型额度不足、租到的账号在请求发出前进入冷却又不能再换号 | 429 | `Resource has been exhausted (e.g. check quota).`；号池全部冷却时附 `Retry-After`（距最早恢复的秒数），主用模型排队超限、Ultra 主用模型额度不足（见[主用模型](#主用模型)）与租到的账号在请求发出前进入冷却（距拦下它的冷却恢复的秒数）时同样附 `Retry-After`；`Retry-After` 在建议间隔上随机往后加 0 到 3 秒（建议间隔不到 5 秒不加） |
| 号池内部的账号认证（需要重新登录）、停用、占用、权限、目录问题，号池为空，服务未就绪或重启中 | 503 | `The service is currently unavailable.` |
| 上游过载 | 503 | `The model is overloaded. Please try again later.` |
| 超时 | 504 | `Deadline expired before operation could complete.` |
| 其他内部错误 | 500 | `An internal error has occurred. …` |

429 另带响应头 `X-AIStudio2API-Reason`，区分本地排队超限（`local_queue`）、候选全部冷却（`local_cooling`）、上游额度或限流（`upstream_quota`）与出口限流（`egress_limited`），见[出口限流、目录缓存与“需要登录”](#出口限流目录缓存与需要登录)。

号池内部的 401/403 不会透传给客户端：new-api 等中转遇到 401（Gemini 类型渠道还包括 403）会自动禁用整个渠道，而这些问题与客户端的密钥无关。

### 上游降级与输入上限

Build 通道响应的每一块都带 `modelVersion`：前面的内容块沿用请求的模型名，最后一块（含最终用量）报告实际服务的模型。实测有的对话请求 `gemini-3.1-pro-preview`，最后一块标明为 `3.1-flash-lite-preview-03-2026`，同时出字速度约为 Pro 的两倍。发现这种情况时写一条警告，管理日志里记 `served_model`，`./start.sh diag` 统计降级比例；Playground 通道的响应里没有模型名，只能按出字速度推测。Playground 对部分账号的输入长度另有上限（如 131072 token），返回这个错误时自动改走 Build 通道再试一次，仍超限且号池里有更高权益的账号时再用它们试一次；两次再试都只用现在就能取得的账号，不排队等待。仍然超限或没有立即可用的账号时按 400 返回给客户端，文案与安全拦截相同（`Prompt was blocked due to prohibited content. (blockReason: PROHIBITED_CONTENT)`），不向客户端暴露账号上限；上游有时把它按 429 返回，但它与账号无关：不按额度冷却账号，也不再换号。带具体计数的模型本身上限（如 `(2000000) exceeds … (1048576)`）任何账号都放不下，直接按 400 返回谷歌原文，不重试。

测试某段对话是否被降级：`python3 tools/test_downgrade.py --key 你的APIKey [--trace] [--request 请求体或排查记录.json]`，逐次报告首字、出字速度和上游标明的模型，并用普通提示词做对照。

### 拒绝被降级的回复（降级判定）

默认开启（服务配置页"拒绝被上游降级的回复"，修改后立即生效）。请求列表中的模型时（默认只有 `gemini-3.1-pro-preview`，`-nothinking`、`-128` 等别名同样适用），本服务在把正文交给客户端之前判断这次是否被上游换成了其他模型；判定为降级就取消上游请求，按 Google 输入被内容策略拦截（PROHIBITED_CONTENT）的官方措辞返回 HTTP 400：说明为 "Input blocked: The model could not generate output because the input violates Google's Generative AI Prohibited Use policy. If you think this was an error, send feedback. (blockReason: PROHIBITED_CONTENT)"（含官方链接），Gemini 格式的 `status` 为 `INVALID_ARGUMENT`，OpenAI 格式的 `code` 为 `content_policy_violation`，与上游真的拦截时相同。不把降级的内容发给用户，也不换号重试。其他模型不受影响。

- 返回码：服务配置页“拒绝时的返回码”（`.env` 中 `DOWNGRADE_REJECT_STATUS`，修改后立即生效）可选 400（默认，即上面的内容策略拦截格式，客户端与中转通常不会重试）或 503（如实说明上游换用了其他模型、回复已丢弃，按服务暂时不可用返回，Gemini `status` 为 `UNAVAILABLE`，Anthropic 类型为 `overloaded_error`，客户端与中转可以重试或切换渠道）。

- 判定依据：从第一块正文之后算起（第一块在到达前就已生成，用时不可知），新增正文达到 150 token、且经过 2.5 秒时按出字速度判定（窗口越长测速越稳；回复比窗口短时，上游结束后按整段速度判定，窗口下限 0.8 秒），达到 190 tok/s 即为降级（正常 Pro 约 90～150，Flash-Lite 约 230～270）。Playground 通道每块都带上游的累计正文数，速度是精确的，所以被拦截的模型优先走 Playground，没有可用的 Playground 账号时才退回 Build。Build 通道按"第一块的输入 token ÷ 本次实际发送的输入字数（含随机后缀、写进系统指令的工具约束与附在说明里的 Schema，按 truncation=auto 截断之后的对话计）"的比例估算正文 token，估算速度落在 160～220 的模糊区间或高于上限时用同一账号调用 CountTokens 精确计算（超时 1.5 秒，失败按估算）；回复含函数调用时，结束复核只按正文计数，上游标明的模型作为最终确认。系统提示与回复语言不同时比例会有偏差，看诊断里的"Build 估算偏差"与"漏判"。
- 缓存方式：严格模式（默认）判定前不发出任何内容（包括思考与心跳），判定为降级时直接返回 HTTP 400。正常请求的正文首字约晚 2.5～3 秒，显示思考的客户端要等思考结束后才开始收到内容；被降级的请求在思考结束后约 2.5 秒收到 400。长思考期间连接上没有任何字节，前面若有反向代理或中转，读超时要大于最长思考时间。想缩短等待可以在服务配置页设"最长延后"（例如 3000 毫秒，默认 0 为不限）：到时限还没判定就先返回 200 并发出思考、正文继续缓存，之后判定为降级只能在流中发送错误事件；也可以打开快速模式（思考一开始就实时转发）。非流式请求没有额外延迟：判定为降级立即返回 400，判定正常后继续收集，上游结束时再复核一次。不在拦截列表里的模型完全不经过判定，速度不受影响。
- 对话记录：被判定为降级的对话（按模型 + 系统提示 + 第一条消息识别，同时记下当时的消息条数与哈希）在 30 分钟内再次请求、且这些消息原样都在时（重新生成、接着往下聊），发送前直接拒绝；用户改过其中任何一条就重新判定。只有一条消息的对话不记录（记录按首条消息识别，单条记录会波及所有同一开场白的对话），客户端中途断开的请求不判定也不记录。只保存哈希，不保存内容。记录按号池分开：经 `/ultra` 的请求只看 Ultra 号池的记录，普通号池记下的对话不会让 `/ultra` 的同一段对话在选号前被拒绝，反之亦然。走 `/trace/` 排查路由的请求不查记录，便于反复测试。
- 查看：管理日志里被拦截模型的每个请求都有判定依据（请求详情"降级判定"）；拒绝时另写一条 WARN 运行日志（含"启动以来第 N 次"）；排查记录的自动结论列出判定依据；`./start.sh diag` 第 4 段汇总拒绝次数与依据、平均额外首字延迟、CountTokens 次数与耗时，4b 段显示启动以来的统计（含 Build 估算偏差与"漏判"：流式放行后结束时复核为降级的次数）。
- 测 CountTokens 耗时：`python3 tools/test_downgrade.py --key 你的APIKey --bench-count 10`。

### 排查记录的自动结论

经 `/trace/` 路由的请求，排查记录（`logs/trace/*.json`）最前面有自动结论：`summary` 为一行摘要（状态、模型、通道、耗时、首字、token、客户端），`findings` 逐项列出发现的问题，包括请求失败、客户端提前断开、换号重试原因、等待账号、首字慢、输出中途停顿、收尾慢、回复为空、被截断、上游提前终止（安全拦截等）、生成速度异常快（疑似上游重放）、客户端实际收到的内容与上游返回的不一致。`timeline` 为该请求的完整进度记录。`./start.sh trace` 打包时把全部结论汇总到包内的 `summary.txt`，并在屏幕上列出发现异常的记录；`/api/debug/traces` 也会返回每条记录的摘要。

### 最大输出 token 下限

Gemini 的思考也算在最大输出里，客户端把最大输出设得很小（如 50、10）时，思考就把额度用完，回复被截断甚至为空。「服务配置」里的「最大输出 token 下限」（`.env` 中 `MIN_OUTPUT_TOKENS`，默认 60000）会把更小的值提高到下限，且不超过模型上限；客户端没有设置时沿用模型上限。图片、语音等模型不调整；设为 0 关闭。修改后立即生效，管理日志里被提高的请求会在最大输出一栏标注"已提高到 60000"。

### 账户停用原因与认证文件更新

账户默认启用：重启服务或运行中自动导入时，account.json 为停用（或没有 `enabled` 字段）、但没有手动停用记录的账户会改为启用并写回 account.json（并行写回，上万个账户几秒内完成，日志只记一条“启动时启用 N 个停用账户（没有手动停用记录）”），随后与其他启用账户一样同步模型目录，登录已失效的显示为“需要登录”。只有在管理页或管理接口停用的账户（手动停用记录）保持停用；已经停用的账户再次停用（例如批量停用时选中了它）同样记为手动停用；新账户自动处理验证未通过而停用的账户在重启或认证文件更新后也按默认启用。目录被移走后又放回的账户保留手动停用记录、继续停用；手动停用记录只在管理员启用（包括批量启用）或删除账户时清除。新账户自动处理不会启用手动停用的账户，面板上的“处理现有停用账户”是例外（确认框中有说明），加入之后再次手动停用的仍保持停用；队列没处理完就停止或重启服务时，重新排队的这些账户仍按这次的要求处理。

停用的账户会在管理页账户列表的邮箱下方显示停用原因和时间：管理页或管理接口停用（附调用来源地址）、新账户自动处理验证未通过或出错（附具体原因）。原因保存在账户目录下的 `.disable-reasons.json`（不写入各账户的 account.json，旧版本与外部工具读取不受影响），新的手动停用记录在管理接口返回前写入，已手动停用的账户再次停用（只更新时间）与其余记录合并约 3 秒写一次，启动时清除已启用账户的自动停用记录，停止服务时立即写出，写入失败记 WARN 日志并在下一次保存时重试；文件存在却无法读取或解析时记 WARN，本次运行没有记录的停用账户不按默认启用并补记为手动停用，改写前原文件另存为 `.disable-reasons.json.unreadable-时间`；多个进程共用一个账户目录时各自整份写出，不做合并；没有记录的停用账户显示"停用原因未记录"。Cookie 过期不会让账户变成"已停用"，而是"需要登录"。

认证文件（各账户目录下的 `storage-state.json`）被外部程序更新后，约 10～20 秒内自动载入，不需要重启：只比较登录签名 Cookie（SAPISID 等），本服务自己回写的轮换 Cookie 不会触发；账户空闲时载入并重启它的浏览器，正在处理请求的账户下一轮再试，不会打断请求。需要登录的账户随之恢复调度；停用但不是手动停用的账户按默认启用并重新同步模型目录；手动停用的账户只载入新 Cookie、保持停用。

### 排查路由 /trace/

`/trace/` 前缀下的全部接口与主路由完全相同（同一个 API Key、同样的调度与转换），只是额外为每个 POST 请求在 `logs/trace/` 写一份完整记录：客户端原始请求（`Authorization`、`x-api-key`、`x-goog-api-key`、`api-key`、Cookie 等鉴权头与 `?key=` 参数已隐藏）、协议转换后与发往上游的参数（含实际使用的 seed）、真正发给 AI Studio 的请求体、每次换号尝试的账号与失败原因、上游原始响应开头、完整回复与返回给客户端的内容。只保留最近 100 个文件，主路由不受影响。

用法：在中转里新建一个测试渠道，接口地址填 `http://服务器:2048/trace`（Ultra 号池填 `http://服务器:2048/trace/ultra`，其余与正式渠道相同），用它复现问题，然后执行 `./start.sh trace`，把 `reports/` 下生成的压缩包发给开发者。记录里有提示词与回复原文，分享前请确认内容可以公开。

主路由也会检测重复回复（只保存指纹，不保存内容）：同一段回复在 2 小时内再次出现时，日志里会有一条 `重复回复` WARN，列出两次请求实际发给上游的 seed、温度、top_p、top_k、思考强度、账号与通道，以及两次提示词是否相同。

排查记录证实，上游对相同、甚至只差几百个字的请求会返回完全相同的回复：换 seed、换账号、温度调到 1.5 都没有用，回复正文、输出 token 数和思考 token 数都完全一致（上游不理会 seed，采样结果由输入决定）。本服务没有任何回复缓存，每次都是上游重新生成。因此默认开启「给请求加不可见随机后缀」（服务配置页，或 .env 中 `REPEAT_PROMPT_NONCE=true`）：在最后一条用户消息末尾加入不可见的零宽字符，让每次发给上游的输入都不同。客户端指定了 seed 或温度设为 0（需要可复现）时不加，带工具结果的轮次不改动。效果可在 `/api/debug/duplicates` 中查看。

管理接口（仅本机或管理密码）：`GET /api/debug/traces`、`GET /api/debug/traces/{name}`、`DELETE /api/debug/traces`、`GET /api/debug/duplicates`、`GET /api/debug/perf`（账户池锁与浏览器命令统计，`./start.sh diag` 第 4b 段会按 5 秒采样换算成每秒数据）。

## 贡献

欢迎提交 Issue 和 Pull Request！

## 开发计划

- ✅ **TTS 支持**: 已适配 `gemini-2.5-flash/pro-preview-tts` 语音生成模型
- ✅ **媒体生成**: 已支持 Imagen 3、Veo 2、Nano Banana 图片/视频生成
- ✅ **文档完善**: 更新并优化 `docs/` 目录下的详细使用文档与 API 规范
- **一键部署**: 提供 Windows/Linux/macOS 的全自动化安装与启动脚本
- ✅ **Go 语言重构**: 将核心代理服务迁移至 Go 以提升并发性能与降低资源占用
- ✅ **多Worker负载均衡**: 支持多 Google 账号轮询池，提高并发限额与稳定性

### 纯 Go WAA 运行时

- ✅ **纯 Go 后端**: `WAA_BACKEND=go` 在服务进程内执行官方 interpreter 与 program，按账户指纹模拟 Firefox 页面环境，运行时不下载、不启动 Camoufox；账户登录仍使用 Camoufox
- **Firefox 引擎细节**: 补齐 `Intl` 格式化、正则字面量的全局解析时机与 `RegExp.prototype` 的 Symbol 键顺序
