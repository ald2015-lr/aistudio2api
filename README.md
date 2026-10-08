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
- **两种 WAA 后端**: 默认由 Camoufox 持有官方 WAA 生命周期；设置 `WAA_BACKEND=go` 后由纯 Go 生成官方 proof，运行时不下载、不启动浏览器
- **四套 API 协议**: OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 与 Gemini GenerateContent
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

从 [Releases](https://github.com/Mag1cFall/AIStudio2API/releases) 下载 `windows-amd64.zip` 发布包，解压后运行 `start.bat`。发布包已包含管理界面，可直接运行。

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

Gemini 原生接口非流式响应：连续的正文与连续的思考各合并为一个 part（与 Gemini API 一致），单独到达的思考签名挂到前一个文本 part 上。此前按上游增量逐段生成 part，SillyTavern 等客户端用空行拼接多个 part，会导致句子中间断行、数字被拆开、出现多余空行。

随机种子：本服务不缓存响应、也不合并相同请求；客户端传入的 seed 会原样转给上游，固定 seed 会让相同提示词得到几乎相同的回复。客户端没有传 seed（或已按配置忽略）时不发送 seed，与网页版一致；生成参数（温度、top_p、top_k、最大输出、思考强度）都按客户端传入的值，没传的项用模型目录里的默认值（与网页版相同）。避免相同请求得到相同回复靠的是随机后缀。Playground 请求的字段与网页版一致，不再附带时区。请求日志参数中显示 seed（未传为“默认”）；服务配置中的“忽略客户端传入的 seed”（IGNORE_CLIENT_SEED，默认关闭，可热更新）开启后丢弃客户端 seed、不发给上游，日志中标注“（已忽略）”。

冷却记录与每日额度通道判定：冷却保存在各账户的 runtime-state.json，重启后自动恢复，到期自动解除。同一账户 Build 与 Playground 是否共用某模型的每日额度，从实际请求中学习——一个通道达到每日限额后，看该账户在另一个通道上的下一次尝试（只计在限额之后才发起的尝试）：成功记为独立，同样达到每日限额记为共用；共用证据至少 8 次且独立占比低于约 5% 时判定为共用，之后一个通道达到每日限额就同时冷却另一个通道，不再让请求去另一个通道失败一次再换号。判定为共用后，每 20 次每日限额仍放行一次另一个通道作为复核，额度后来变为独立时判定随之修正；判定记录保存在账户目录的 .quota-sharing.json，重启后继续有效；`./start.sh diag` 会显示磁盘上保存的有效冷却条数与判定结论。

浏览器轮询：受保护请求等待响应头与读取响应数据时采用自适应间隔，有新数据时 10ms，连续没有数据时逐步放宽到 100ms（此前固定 10ms，请求等待上游思考的几秒里会向浏览器发出数百条空命令，高并发时占用大量 CPU）。诊断脚本会检查主程序打开的文件数与系统线程数是否接近上限。

高并发调度：获取账户 Worker 时先在不持有全局 rebalanceMu 的情况下检查现成 Worker；持有 rebalanceMu 时不再等待单个账户的锁（该锁在生成 WAA proof 时会被占用数秒，原先会让全系统请求一起排队）。账户候选按 ID 排序的结果缓存到账户增删时才重建；账户“是否支持某模型”与 Build 可生成模型改为按账户目录索引查找，目录替换时自动重建。

Build 独有模型计算：Playground 目录按模型 ID 与别名建立索引，账户增删或目录变化时才重建；判断 Build 独有模型与账户 Build 通道是否支持某模型改为查索引。此前每次重算都要对每个启用账户、每个 Build 模型扫描全部账户的全部模型，计算量随账户数平方增长并全程占用账户池锁，账户数千时每次要一两秒，期间所有请求都在等锁。

预热池冷却轮换：每 30 秒检查一次，某模型在预热池中长时间冷却（所有启用通道都冷却且剩余 15 分钟以上，如当天额度已用完）的账户达到预热数的 5%（至少 3 个）时，在后台把这些空闲 Worker 换成没有冷却的账户（每轮最多“启动预热并发”个、需有可接替账户、刚就绪 2 分钟内的不换），预热补位时也优先选没有冷却的账户。此前只有请求到来找不到可用 Worker 时才现场启动或替换浏览器，这几秒到十几秒都算在请求等待里。状态接口只统计账户数量、新账户处理只查询单个账户，不再频繁构建上千个账户的完整状态。

浏览器控制连接（WebDriver BiDi）：改为后台协程统一读取、按编号分发回复，同一浏览器上的多条命令可以同时进行；单条命令超时不再关闭整条连接（原实现超时即断开，浏览器稍慢一次，该 Worker 上所有进行中的请求会一起失败）。受保护请求轮询偶尔超时时继续等待，连续 60 秒无回应才判定失败；控制连接断开、写入失败、发起请求的命令 15 秒无回应或轮询连续 60 秒无回应的 Worker 如实报告失败（状态查询不持有 Worker 内部锁），请求换号或在重建后重放、Worker 自动重建。解析上游响应时保留原始错误链，读取超时等可重试错误会换号重试而不是直接返回 502。

账户池锁优化：全体账户模型目录的合并改为在全局账户池锁外进行，并改为单遍线性合并（结果与原逐步合并完全一致）；Build 独有模型判断改为查表；自动重启监督只读取服务状态。此前账户数百个、后台持续自动验证或同步目录时，每次重算都要在锁内做几十万次集合运算，期间所有请求都拿不到锁。

并发调度优化：分配账号时不再逐个等待正在生成 WAA proof 的账户（每次 1～5 秒），调度耗时不再随并发线性增长；候选全部没有空位且有账户冷却时，从磁盘刷新运行态改为同一模型每 3 秒最多一次；没有额度信息的 429 也会让该账户的这个模型冷却 30 秒，不再被反复选中白白失败。额度周期按 `quota_unit`、`quota_limit`、`quota_metric`、错误文案依次判定，按模型计的分钟限额只冷却该模型，不再冻结整个账号；上游给出 RetryInfo 或 Retry-After 时按它计算恢复时间（每日限额不会因此提前恢复）。

参数容错：`max_tokens` 超过模型上限时按上限发送，不再报错；`reasoning_effort` 兼容 `xhigh`、`max`（视为最高）与 `auto`（使用模型默认）；只支持思考预算的模型收到思考强度时换算为预算（low 1024、medium 8192、high 24576）；不支持调节思考的模型忽略思考参数。Playground 与 Build 两个通道一致。

优雅退出：收到停止信号（`./start.sh stop/restart`）后先停止接收新请求，等进行中的请求完成再结束运行时，最多等待 SHUTDOWN_GRACE（默认 60 秒）；管理页实时事件流会主动断开，不拖住等待。此前运行时直接跟随退出信号，每次重启都会立即截断所有进行中的回复。start.sh 强制结束前的等待相应放宽到 90 秒（STOP_TIMEOUT）。

回复指纹与重复回复分析：请求日志记录回复正文的指纹（reply_hash，只用于比较是否相同，不保存内容）；`./start.sh diag` 会找出内容完全相同的回复并列出当时的 seed 与温度，判断是客户端固定 seed、温度为 0，还是请求根本没有到达本服务（中转站或客户端缓存）。

工具结果中的图片：工具（如读取文件、截图、MCP 工具）返回的 base64 图片会取出来作为真正的图片发送，放在这组工具结果之后的用户消息里并标注来自哪次调用；工具结果原位置替换为文字说明。此前图片的 base64 会被当成文字塞进工具结果：模型看不到图片，输入 token 暴涨，连续读取两张图片基本必然报错。支持 OpenAI、Anthropic、Responses 与 MCP 格式；网络地址形式的图片保持原样。

对话历史分轮修正：发往上游前自动合并连续的模型消息与连续的工具结果；历史以模型工具调用开头时在前面补一条用户消息；历史以模型文字消息结尾（预填充、续写）时在最后补一条请模型从原消息结尾接着写、不重复已有内容的用户消息，避免上游返回 400 “Requests ending with a model turn are not supported”。用于兼容把说明文字和工具调用、或把并行工具调用拆成多条 assistant 消息的客户端，避免上游返回 400 “function call turn comes immediately after a user turn or after a function response turn”。只调整分轮，不改动内容与思考签名。

新账户自动处理（账户页顶部面板）：新目录导入后进入队列，待处理达到“每批数量”（默认 10）或最早的账户已等待 2 分钟时开始处理。开启自动验证时，账户在停用状态下先验证登录（启动浏览器，默认并发 3），通过后才启用，失效的保持停用，不会接到请求；验证过程出错会间隔 2 分钟重试，共 3 次。设置与处理记录保存在账户目录的 `.onboarding.json`，重启后继续有效，服务停止期间上传的账户在启动后同样会处理。首次开启时已有账户记为基线不做改动，面板上的“处理现有停用账户”可以把它们交给同一流程；管理页手动添加的账户不自动处理；删除后重新上传的账户会再次处理。

服务器运维：`./start.sh report` 一键打包排查资料到 reports/（诊断结果、统计窗口内的日志、崩溃堆栈、服务状态与程序快照、系统信息、隐藏密钥的配置；账户邮箱替换为固定代号，密钥、口令、代理账号密码、API Key、令牌与 Cookie 均隐藏；`MINUTES=120 ./start.sh report` 扩大窗口）；`./start.sh diag` 一键诊断回复慢与截断（截断分析把已开始输出但未正常写完的回复按 Google 内容审核、Google 端异常、输出上限、客户端断开、传输出错分类并给出责任方；系统资源、账号冷却与预热池交叉核对、排队/生成 proof/等上游响应头/响应头到首字各阶段耗时、状态接口卡住时的锁等待快照、程序内部等待点，并给出结论；请求日志会记录“WAA proof 完成”“上游已返回响应头”两个阶段时间点；`MINUTES=120 ./start.sh diag` 扩大统计窗口）；`./start.sh disk` 查看账户目录与浏览器缓存占用；`./start.sh clean-cache` 清理没有 Worker 在用的账户浏览器缓存（每个账户上限约 256MB，账户越多累积越多，运行中执行也安全）。

管理面访问控制：管理页面与 `/api/` 控制面必须携带管理令牌，不再按“来源是本机”放行（默认配置的 nginx `proxy_pass` 转发的外网请求来源同样是 127.0.0.1，且不带转发头）。令牌在首次启动时随机生成并保存在程序目录的 `.admin-token`（权限 0600，删除后重启即更换）；启动日志与 `./start.sh status` 会打印 `http://127.0.0.1:2048/?admin_token=<令牌>`，用它打开一次后浏览器以 HttpOnly、SameSite=Strict 的 Cookie 记住登录，`start.bat` 与直接运行程序时自动打开的页面已带令牌。脚本调用控制面时在 `X-Admin-Token` 请求头中携带令牌（`start.sh` 与 `tools/` 下的诊断脚本会自动读取）。设置 `ADMIN_PASSWORD` 后也可以用 HTTP Basic 密码登录；错误次数按反代传来的真实客户端地址分别计数。

账号 403 自动暂停：同一账号 10 分钟内对至少 2 个不同模型连续 3 次返回 HTTP 403 无权限（中间没有成功请求），自动进入 30 分钟全局冷却，不再参与调度，到期自动恢复；只对单个模型 403（常见于免费账号调用付费模型）不会触发。暂停记录在“冷却与请求”页可见，重启后保留。

Worker 预热采用滑动窗口：按“启动预热并发”同时启动多个 Worker，任意一个完成后立刻补上下一个，单个慢账户不会拖住其他槽位。

运行中无需重启的热更新：

- **账户目录**：放进 `auth/` 的新账户目录约 10 秒内自动导入；目录被删除后自动移出账户池（不删除任何文件）；“需要登录”的账户在认证文件被替换后自动重新载入。单个目录损坏或正在复制不影响其他账户启动
- **服务配置**：管理页面修改后自动保存。常驻/峰值 Worker 数、启动预热并发、单账户并发、账户选择策略、初始化与请求超时、临时对话、API 密钥保存后**立即生效，不重启、不中断请求**；账户目录、默认代理、WAA 后端、上游通道决定运行时的装配方式，仍需重启生成服务（倒计时后自动应用，应用前等待进行中的请求结束，最多 30 秒）；监听地址需 `./start.sh restart`。管理页面需要管理令牌（见上文“管理面访问控制”），远程管理可以用带令牌的地址、`ADMIN_PASSWORD`，或 SSH 隧道：`ssh -L 2048:127.0.0.1:2048 <服务器>`。`LISTEN_ADDR` 对外监听时务必把 `PROXY_API_KEY` 改为自定义密钥。

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

文本对话模型支持在模型名后加后缀，可组合、顺序不限（例如 `gemini-3.1-pro-preview-128-nothinking-online`），`/v1/models` 与 `/v1beta/models` 会同时列出这些别名：

| 写法 | 效果 |
| --- | --- |
| 不带后缀 | 思考强度设为最高；客户端显式传了 `reasoning_effort`、`thinking` 等思考参数时以客户端为准 |
| `-128` | 最低思考：支持思考等级的模型用最低等级，只支持思考预算的模型用 128 token |
| `-nothinking` | 模型照常思考，但不返回思维链（保留多轮工具调用需要的思考签名） |
| `-online` | 开启 Google 搜索（模型支持时） |调用 API 必须携带 `PROXY_API_KEY`（默认 `sk-onechat-fun-fun`，可在管理页“服务配置”中修改）；使用默认密钥时，浏览器中只有本机页面可以直接调用接口，网页版客户端需要改为自定义密钥。

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

生成请求中的内联附件会优先上传为临时 Drive 文件，随请求结束清理；账户未授予 Drive 权限时保持内联数据发送。图片、音频、视频、PDF 等输入仍需所选模型支持。重复使用的附件可通过 Files 接口上传一次并复用文件 ID。

Gemini 附件与视频图片输入支持 `inlineData` / `inline_data`、`fileData` / `file_data`、`mimeType` / `mime_type` 和 `fileUri` / `file_uri`。媒体 Base64 数据支持标准与 URL-safe 字母表、带填充与无填充形式，以及 `data:<MIME>;base64,` 前缀。OpenAI 助手历史中的 Markdown 图片同样支持 URL-safe Base64 和 CR/LF 换行。内联 GIF 和视频表单上传的 GIF 按首帧静态图片转换为 PNG，保留逻辑画布、帧位置与透明背景。

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
| `PROXY` | 空 | Chrome 导入、登录和账户默认使用的 HTTP、HTTPS 或 SOCKS5 代理 |
| `INIT_TIMEOUT` | `2m` | 单账户 WAA 初始化超时 |
| `REQUEST_TIMEOUT` | `5m` | 单次请求最大执行时间 |
| `WARM_WORKER_LIMIT` | `5` | 常驻预热账户数 |
| `MAX_ACTIVE_WORKERS` | `10` | 高峰期最多同时运行的 Worker 数 |
| `WARM_STARTUP_CONCURRENCY` | `2` | 同时初始化的预热账户数 |
| `PER_ACCOUNT_CONCURRENCY` | `2` | 单账号同时执行的请求数 |
| `ROUTING_STRATEGY` | `round-robin` | `round-robin` 轮询；`fill-first` 账号粘性优先 |
| `UPSTREAM_CHANNELS` | `playground,build` | 生成请求使用的上游通道，可只保留其一 |
| `WAA_BACKEND` | `camoufox` | `camoufox` 在 Camoufox 页面运行 WAA；`go` 在服务进程内运行 WAA，不下载也不启动 Camoufox |
| `TEMPORARY_CHAT` | `false` | WAA 预热页是否使用临时对话 |
| `AUTO_START` | `true` | 管理进程启动后自动启动生成服务；启动失败或意外停止时自动重新启动（5 秒起、最长 1 分钟退避，手动停止后不再自动启动）；`false` 时保持 `STOPPED`，需在管理页面手动启动 |
| `ADMIN_PASSWORD` | 空 | 设置后除管理令牌外也可以用密码打开管理页面，浏览器弹出登录框（用户名随意，密码为此值）；同一 IP 10 分钟内错 10 次封禁 15 分钟；留空时只能用管理令牌（`.admin-token`）登录。纯 HTTP 下令牌与密码均为明文传输，建议配合 HTTPS |

服务启动时会载入 `AISTUDIO_AUTH_STATES` 中的全部账户；`WARM_WORKER_LIMIT` 控制常驻预热规模，`MAX_ACTIVE_WORKERS` 控制峰值 Worker 上限，`WARM_STARTUP_CONCURRENCY` 控制启动预热并发，`PER_ACCOUNT_CONCURRENCY` 控制单账户请求槽位。

### 端口配置

- **管理页面与 API**: 默认端口 `2048`
- **Camoufox**: 由程序动态分配本机端口

## 高级功能

### 代理配置

支持通过无认证信息的 HTTP、HTTPS 或 SOCKS5 代理访问 AI Studio：

1. 在“服务配置”中设置全局代理
2. 在“账户”页面编辑单个账户时可以设置账户专用代理
3. 账户代理同时用于登录、WAA 与业务请求

### 认证文件管理

认证文件默认存储在 `auth/` 目录：

| 路径 | 内容 |
| --- | --- |
| `auth/<Google 邮箱>/account.json` | 账户邮箱、代理、语言、时区和启用状态 |
| `auth/<Google 邮箱>/storage-state.json` | Google Cookie 与认证续签材料 |
| `auth/<Google 邮箱>/runtime-state.json` | 权益等级、模型资格、冷却状态与资源所属账户 |
| `auth/<Google 邮箱>/camoufox-cache/` | 该账户浏览器的网页缓存，服务停止时可删除 |
| `auth/.leases/<Google 邮箱>.lock` | 同一账户目录的跨进程占用锁 |
| `[用户缓存]/AIStudio2API/runtime-leases/<Google 邮箱>.lock` | 当前电脑上该邮箱的 WAA Worker 占用锁 |

账户邮箱同时作为目录名、管理页面标识和日志来源，统一使用小写形式。`.leases` 协调账户目录读写，用户缓存中的 runtime lease 保证同一邮箱在当前电脑上只有一个 WAA Worker。

账户页支持 Chrome 批量导入和隔离 Camoufox 登录。`ready` 账户可以编辑、停用、验证和删除，`auth_required` 账户可以重新登录。

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
- **Responses 会话**: `previous_response_id` 仅在当前进程内保存，重启后不会保留
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

### 错误响应格式

返回给客户端的错误统一使用各协议的官方结构（OpenAI 的 `error.message/type/param/code`，Anthropic 的 `type: error` 加 `request_id`，Gemini 的 `error.code/message/status`），消息使用 Google Gemini API 的官方英文措辞；内部详细原因（账号、上游协议细节、中文说明）只写管理日志。

| 情况 | 状态码 | 返回的消息 |
|---|---|---|
| 输入超过模型 token 上限等参数错误 | 400 | 谷歌原文（去掉 `[original: …]`、`(qos=…)` 等内部标记）；OpenAI 附 `code: context_length_exceeded`，Anthropic 以 `prompt is too long:` 开头 |
| 输入被安全策略拦截 | 400 | 如 `Prompt was blocked due to prohibited content. (blockReason: PROHIBITED_CONTENT)` |
| 模型不存在或没有账号能提供 | 404 | `models/<模型> is not found for API version v1beta, …` |
| 号池全部冷却、上游限流 | 429 | `Resource has been exhausted (e.g. check quota).` |
| 号池内部的账号认证、权限、目录问题，服务未就绪或重启中 | 503 | `The service is currently unavailable.` |
| 上游过载 | 503 | `The model is overloaded. Please try again later.` |
| 超时 | 504 | `Deadline expired before operation could complete.` |
| 其他内部错误 | 500 | `An internal error has occurred. …` |

号池内部的 401/403 不会透传给客户端：new-api 等中转遇到 401（Gemini 类型渠道还包括 403）会自动禁用整个渠道，而这些问题与客户端的密钥无关。

### 上游降级与输入上限

Build 通道响应的每一块都带 `modelVersion`：前面的内容块沿用请求的模型名，最后一块（含最终用量）报告实际服务的模型。实测有的对话请求 `gemini-3.1-pro-preview`，最后一块标明为 `3.1-flash-lite-preview-03-2026`，同时出字速度约为 Pro 的两倍。发现这种情况时写一条警告，管理日志里记 `served_model`，`./start.sh diag` 统计降级比例；Playground 通道的响应里没有模型名，只能按出字速度推测。Playground 对部分账号的输入长度另有上限（如 131072 token），返回这个错误时自动改走 Build 通道重试一次。

测试某段对话是否被降级：`python3 tools/test_downgrade.py --key 你的APIKey [--trace] [--request 请求体或排查记录.json]`，逐次报告首字、出字速度和上游标明的模型，并用普通提示词做对照。

### 拒绝被降级的回复（降级判定）

默认开启（服务配置页"拒绝被上游降级的回复"，修改后立即生效）。请求列表中的模型时（默认只有 `gemini-3.1-pro-preview`，`-nothinking`、`-128` 等别名同样适用），本服务在把正文交给客户端之前判断这次是否被上游换成了其他模型；判定为降级就取消上游请求，按 Google 输入被内容策略拦截（PROHIBITED_CONTENT）的官方措辞返回 HTTP 400：说明为 "Input blocked: The model could not generate output because the input violates Google's Generative AI Prohibited Use policy. If you think this was an error, send feedback. (blockReason: PROHIBITED_CONTENT)"（含官方链接），Gemini 格式的 `status` 为 `INVALID_ARGUMENT`，OpenAI 格式的 `code` 为 `content_policy_violation`，与上游真的拦截时相同。不把降级的内容发给用户，也不换号重试。其他模型不受影响。

- 返回码：服务配置页“拒绝时的返回码”（`.env` 中 `DOWNGRADE_REJECT_STATUS`，修改后立即生效）可选 400（默认，即上面的内容策略拦截格式，客户端与中转通常不会重试）或 503（如实说明上游换用了其他模型、回复已丢弃，按服务暂时不可用返回，Gemini `status` 为 `UNAVAILABLE`，Anthropic 类型为 `overloaded_error`，客户端与中转可以重试或切换渠道）。

- 判定依据：从第一块正文之后算起（第一块在到达前就已生成，用时不可知），新增正文达到 150 token、且经过 2.5 秒时按出字速度判定（窗口越长测速越稳；回复比窗口短时，上游结束后按整段速度判定，窗口下限 0.8 秒），达到 190 tok/s 即为降级（正常 Pro 约 90～150，Flash-Lite 约 230～270）。Playground 通道每块都带上游的累计正文数，速度是精确的，所以被拦截的模型优先走 Playground，没有可用的 Playground 账号时才退回 Build。Build 通道按"第一块的输入 token ÷ 本次实际发送的输入字数（含随机后缀）"的比例估算正文 token，估算速度落在 160～220 的模糊区间或高于上限时用同一账号调用 CountTokens 精确计算（超时 1.5 秒，失败按估算）；回复含函数调用时，结束复核只按正文计数，上游标明的模型作为最终确认。系统提示与回复语言不同时比例会有偏差，看诊断里的"Build 估算偏差"与"漏判"。
- 缓存方式：严格模式（默认）判定前不发出任何内容（包括思考与心跳），判定为降级时直接返回 HTTP 400。正常请求的正文首字约晚 2.5～3 秒，显示思考的客户端要等思考结束后才开始收到内容；被降级的请求在思考结束后约 2.5 秒收到 400。长思考期间连接上没有任何字节，前面若有反向代理或中转，读超时要大于最长思考时间。想缩短等待可以在服务配置页设"最长延后"（例如 3000 毫秒，默认 0 为不限）：到时限还没判定就先返回 200 并发出思考、正文继续缓存，之后判定为降级只能在流中发送错误事件；也可以打开快速模式（思考一开始就实时转发）。非流式请求没有额外延迟：判定为降级立即返回 400，判定正常后继续收集，上游结束时再复核一次。不在拦截列表里的模型完全不经过判定，速度不受影响。
- 对话记录：被判定为降级的对话（按模型 + 系统提示 + 第一条消息识别，同时记下当时的消息条数与哈希）在 30 分钟内再次请求、且这些消息原样都在时（重新生成、接着往下聊），发送前直接拒绝；用户改过其中任何一条就重新判定。只有一条消息的对话不记录（记录按首条消息识别，单条记录会波及所有同一开场白的对话），客户端中途断开的请求不判定也不记录。只保存哈希，不保存内容。走 `/trace/` 排查路由的请求不查记录，便于反复测试。
- 查看：管理日志里被拦截模型的每个请求都有判定依据（请求详情"降级判定"）；拒绝时另写一条 WARN 运行日志（含"启动以来第 N 次"）；排查记录的自动结论列出判定依据；`./start.sh diag` 第 4 段汇总拒绝次数与依据、平均额外首字延迟、CountTokens 次数与耗时，4b 段显示启动以来的统计（含 Build 估算偏差与"漏判"：流式放行后结束时复核为降级的次数）。
- 测 CountTokens 耗时：`python3 tools/test_downgrade.py --key 你的APIKey --bench-count 10`。

### 排查记录的自动结论

经 `/trace/` 路由的请求，排查记录（`logs/trace/*.json`）最前面有自动结论：`summary` 为一行摘要（状态、模型、通道、耗时、首字、token、客户端），`findings` 逐项列出发现的问题，包括请求失败、客户端提前断开、换号重试原因、等待账号、首字慢、输出中途停顿、收尾慢、回复为空、被截断、上游提前终止（安全拦截等）、生成速度异常快（疑似上游重放）、客户端实际收到的内容与上游返回的不一致。`timeline` 为该请求的完整进度记录。`./start.sh trace` 打包时把全部结论汇总到包内的 `summary.txt`，并在屏幕上列出发现异常的记录；`/api/debug/traces` 也会返回每条记录的摘要。

### 最大输出 token 下限

Gemini 的思考也算在最大输出里，客户端把最大输出设得很小（如 50、10）时，思考就把额度用完，回复被截断甚至为空。「服务配置」里的「最大输出 token 下限」（`.env` 中 `MIN_OUTPUT_TOKENS`，默认 60000）会把更小的值提高到下限，且不超过模型上限；客户端没有设置时沿用模型上限。图片、语音等模型不调整；设为 0 关闭。修改后立即生效，管理日志里被提高的请求会在最大输出一栏标注"已提高到 60000"。

### 账户停用原因与认证文件更新

停用的账户会在管理页账户列表的邮箱下方显示停用原因和时间：管理页或管理接口停用（附调用来源地址）、新账户自动处理验证未通过或出错（附具体原因）、导入时 account.json 为停用。原因保存在账户目录下的 `.disable-reasons.json`（不写入各账户的 account.json，旧版本与外部工具读取不受影响）；升级前就已停用的账户显示"停用原因未记录"。Cookie 过期不会让账户变成"已停用"，而是"需要登录"。

认证文件（各账户目录下的 `storage-state.json`）被外部程序更新后，约 10～20 秒内自动载入，不需要重启：只比较登录签名 Cookie（SAPISID 等），本服务自己回写的轮换 Cookie 不会触发；账户空闲时载入并重启它的浏览器，正在处理请求的账户下一轮再试，不会打断请求。需要登录的账户随之恢复调度；自动停用（非手动停用）的账户重新加入新账户自动处理，验证通过后启用；手动停用的账户只载入新 Cookie、保持停用。

### 排查路由 /trace/

`/trace/` 前缀下的全部接口与主路由完全相同（同一个 API Key、同样的调度与转换），只是额外为每个 POST 请求在 `logs/trace/` 写一份完整记录：客户端原始请求（鉴权头已隐藏）、协议转换后与发往上游的参数（含实际使用的 seed）、真正发给 AI Studio 的请求体、每次换号尝试的账号与失败原因、上游原始响应开头、完整回复与返回给客户端的内容。只保留最近 100 个文件，主路由不受影响。

用法：在中转里新建一个测试渠道，接口地址填 `http://服务器:2048/trace`（其余与正式渠道相同），用它复现问题，然后执行 `./start.sh trace`，把 `reports/` 下生成的压缩包发给开发者。记录里有提示词与回复原文，分享前请确认内容可以公开。

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
