#!/usr/bin/env bash
# AIStudio2API 一键诊断"没调用过的模型出现在 403 无权限账户列表里"（管理页“403 无权限账户”与“出问题”列出
# 403 无权限（gemini-omni-1.1-flash）：The caller does not have permission，自己却没有调用过这个模型）：这次 403 是谁、什么时候、
# 从哪个接口、以什么形态调用的。依次输出：
#   1. 版本与实例：本目录进程的版本（是否早于 93c16fa：带图的 Omni 403 会误登记正常账户）、本机所有 aistudio2api 实例与各自的账户目录
#      （多个实例共用一个账户目录时登记文件是共用的）、登记文件最后写入时间、源码版本与最近的升级、重启记录
#   2. 403 登记：每个登记账户在这个模型上的通道、首次与最近一次的时间、次数、上游方法，以及新版本记下的最近一次请求来源
#      （last_request：请求 ID、接口、请求名、消息条数、内联图片数、文件引用数）；需要验证账号的账户的登记默认只给个数与时间范围。
#      需要验证账号按账户判断，与管理页 403 列表隐藏的账户同一口径：登记里任一模型是账户级 403（verify-account）的账户，以及冷却列表
#      （GET /api/cooldowns）里需要验证账号的暂停仍生效的账户（读不到时按运行日志近似）；读得到冷却列表时另写其中暂停已到期的个数
#      （单个模型的写暂停未生效的个数：已到期或没有写入，与管理页同一说法；第 7 段按运行日志另写其中从没写入的）。
#      分两类汇总，与管理页同一口径：整户（整户暂停生效，或登记里 2 个及以上模型要求验证）与单个模型需要验证（只暂停那个模型，另写模型分布）
#   3. 用量账本最近 DAYS 天（默认 92，最多 92）这个模型的请求：按接口、按天（总数/成功）；每个模型最多统计最近的 10000 条，超过时写明
#   4. 每个登记账户最近一次 403 对应的请求与经过：登记带 last_request 时按请求 ID 找（本实例账本里没有这个 ID 时写明），没有时按时间
#      在账本里单独查一个窄窗口（请求结束时间在最近一次 403 前 2 分钟到后 60 分钟、且这个账户在其中返回过 403），再到运行日志的
#      request.started 里看请求形态；需要验证账号的账户的登记照样查（第 7 段要用），默认只给个数与时间范围。
#      登记与运行日志都看不到请求形态（f06ddf9 之前的版本不记 last_request、运行日志已轮转）、账本里却对上了请求时，用账本补判：
#      记录带正文（has_body）时读 GET /api/requests/{id}/body（客户端发来的原始请求体，截断到 64 KiB，只保留最近 1000 条；按请求 ID
#      只读一次，最多 300 个不同的请求，超出时写明跳过了多少；读正文超时或连接中断一次后不再读，同样写明个数），按接口解析出消息条数、
#      文字字数、图片与其他内联数据、文件引用、max tokens 参数与请求名，与 request.started 同口径判断（只有系统提示时按 1 条用户消息、
#      发给 Omni 视频模型时不计被去掉的系统提示；请求名是目录别名或 MODEL_FALLBACKS 的源模型时照常判为别名、改用；不输出正文文字）；
#      运行日志结束行的 fallback_from、账本记录的上游尝试（额度用完改用时源模型与目标模型的尝试各带模型，中间有一条改用备注）说明登记的
#      模型是改用的目标时，有没有正文都先判为额度用完改用（登记的模型是改用的源模型时照常按形态判断）；
#      没有正文时按账本的输入 tokens 判断：输入含本服务加的零宽字符随机后缀（REPEAT_PROMPT_NONCE，默认开启，十几到近百个 token），
#      渠道测试的上限按同一次运行里已确认是渠道测试的请求校准（没有样本时只认不超过 8 的），下游的门槛为校准值（没有样本时为后缀可能的
#      最大贡献 96）加 100；逐条输出时多一行“账本补判”：正文有/无/被截断/读取失败与原因、上游尝试里改用过的模型、账本的输入/回复 tokens
#   5. 目录里这个模型的别名（别名本身不是目录 ID 时，请求这个别名会落到这个模型上、登记在它名下）
#   6. 运行日志里这个模型的 403 冷却与需要验证账号的暂停行（需要验证账号的账户的整户暂停、单个模型暂停、解除与 403 冷却行默认只给行数、
#      账户数与时间范围）
#   7. 结论：每个登记账户一行（带最近一次 403 的本机时间与请求 ID，对应管理页 403 列表的行），按 README“没有调用过的模型出现在 403 列表里”
#      的判断：多半是旧记录（含升级到含 93c16fa 的版本之前带图 Omni 请求的误登记）/ new-api 渠道测试 / 下游主动调用 / 别名 /
#      额度用完改用（本服务的 MODEL_FALLBACKS）/ 模型重定向或改用（未确认）/ 直连 interactions /
#      账本里没有对应请求（可能是共用账户目录的其他实例或复制来的目录）。需要验证账号的账户（账户级 403：2 个及以上模型或整户暂停生效的
#      整户暂停调度 30 天，只在一个模型上的只暂停那个模型；管理页整个账户不列在 403 列表里、另有汇总块与“403 单个模型需要验证”；这个账户
#      在其他模型上的普通 403 也算在内）同样判断请求来源，整户与单个模型各汇总成一小段：
#      账户数、最近一次 403 的时间范围、按请求来源分类的账户数、涉及的请求 ID 数与账户数最多的前 5 个请求（一个请求依次换号、每个都 403）。
#      账户级 403 的那条登记不套用旧记录、升级前带图误登记与下游带图三条（它们针对的是 The caller does not have permission 被误登记的
#      情形，结论是“账户本身多半正常”），按请求形态归入渠道测试、下游等；同一账户在其他模型上的普通 403 照常套用。
#      汇总另给按接口的账户数，账户数最多的请求带上接口；大部分账户无法判断请求形态时写明主要原因与下一步。
#      账本补判出来的来源照常归类；仍判断不了时写“无法判断请求形态”，附上接口、这个请求依次试了几个账户、最终结果与看不到形态的原因
#      （登记早于记录请求形态的版本、运行日志已轮转或覆盖这段时间却没有开始行、账本里没有正文、tokens 判断不了）。账本里也没有、只有请求 ID 时按 ID 前缀
#      补一句接口类型：resp_ 为 Gemini 原生 generateContent 或 OpenAI Responses，chatcmpl_ 为 /v1/chat/completions，msg_ 为
#      Anthropic /v1/messages，int_ 为 Interactions
#   8. 所有模型的 403 与号池级 403（不看 MODEL，ACCOUNT 照常；判断有没有上游 529043e 修的那种“长时间运行后全部账户 403”）：运行日志里
#      “上游返回 4xx … HTTP=403”行按文案分为需要验证账号、The caller does not have permission 与其他（列出最多的 5 种），按天、按小时计数；
#      号池级 403 判定行与撤销登记的 INFO 行的次数与时间分布；每次 403 时这个账户的 Worker 已运行多久（距最近一次“WAA Worker 就绪”）与
#      距进程启动多久；Worker 存活时长分布（按停止的“起因=”）；同一分钟 3 个及以上账户出现非 verify 403 的聚类。程序目录与本机其他实例目录
#      的运行日志分开统计，另读外部轮转出的 .2、.N.gz、-日期 等文件
# 只读：只对本机管理接口发 GET 请求，只读 .env、运行日志（logs/aistudio2api.log 与 .1，第 8 段另读轮转出的其他文件与本机其他实例的运行日志）、
#   登记文件（管理接口连不上时）与进程信息，
# 不改任何配置、记录、冷却或账户；邮箱已打码，不输出管理令牌、API 密钥与提示词（账本里的请求正文只取消息条数、字数、媒体数与参数值，
# 不输出任何文字内容）。
#
# 用法（放在程序目录的 tools/ 下运行，或用 APP 指定程序目录）：
#   bash tools/diag_403.sh                        查 gemini-omni-1.1-flash（默认）
#   MODEL=gemini-4-carbon bash tools/diag_403.sh   查别的模型
#   MODEL=all bash tools/diag_403.sh               查登记里的所有模型
#   ACCOUNT=abc bash tools/diag_403.sh             只看邮箱（账户 ID）含 abc 的账户（不区分大小写）
#   APP=/path/to/app bash tools/diag_403.sh        指定程序目录（含 start.sh、.env、.admin-token、logs/、auth/；默认为脚本所在的程序目录，
#                                                  找不到时为 /www/wwwroot/Chat2API/build）
#   DAYS=30 bash tools/diag_403.sh                 用量账本回看的天数（默认 92，账本保留约 90 天；查询范围最多 93 天，超过 92 按 92）
#   VERIFY=list bash tools/diag_403.sh             第 2、4、6、7 段逐个列出需要验证账号的账户（默认只给汇总；第 7 段每行附请求来源）
#   PYTHON=python3.11 bash tools/diag_403.sh       指定 Python（3.7 及以上）
set -u
script_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd)"
APP="${APP:-${APP_DIR:-}}"
if [ -z "$APP" ]; then
  # 与其他 diag_*.sh 一样按 start.sh（或程序本身）认程序目录，另要求有 .env 或 .admin-token：别的项目目录（如 new-api 的 docker-compose
  # 目录）里常有 .env，不能只看它
  for candidate in "$script_dir/.." "$script_dir" "$PWD" /www/wwwroot/Chat2API/build; do
    if { [ -f "$candidate/start.sh" ] || [ -f "$candidate/aistudio2api" ]; } &&
      { [ -f "$candidate/.env" ] || [ -f "$candidate/.admin-token" ]; }; then
      APP="$(cd "$candidate" && pwd)"
      break
    fi
  done
fi
APP="${APP:-/www/wwwroot/Chat2API/build}"
MODEL="${MODEL:-gemini-omni-1.1-flash}"
DAYS="${DAYS:-92}"
VERIFY="${VERIFY:-}"
PYTHON="${PYTHON:-python3}"
case "$DAYS" in "" | *[!0-9]* | 0) echo "DAYS 必须是正整数"; exit 1 ;; esac
case "$VERIFY" in "" | list) ;; *) echo "VERIFY 只能是 list（逐个列出需要验证账号的账户）或不设"; exit 1 ;; esac
# 用量账本一次查询最多 93 天（多出的 5 分钟算在里面），超过 92 天按 92
if [ "$DAYS" -gt 92 ]; then echo "DAYS=$DAYS 超过账本一次查询的上限，按 92 天查"; DAYS=92; fi
command -v "$PYTHON" >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }
cd "$APP" 2>/dev/null || { echo "找不到程序目录 $APP（用 APP=/path/to/app 指定）"; exit 1; }
mask() { sed -E 's/([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@/\1***@/g'; }
# env_file_value 文件 键：与服务读 .env 的写法一致（允许 export 前缀、引号，无引号的值去掉 “ #” 之后的注释），取最后一行
env_file_value() {
  sed -n "s/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}$2=//p" "$1" 2>/dev/null | tail -n 1 |
    sed -E "s/^\"([^\"]*)\".*/\1/; t; s/^'([^']*)'.*/\1/; t; s/[[:space:]]+#.*//; s/[[:space:]]+$//"
}

printf '==================== 403 无权限账户诊断 %s（%s）====================\n' "$(date '+%F %T %z')" "$([ "$MODEL" = all ] && echo "全部模型" || echo "模型 $MODEL")"
echo "程序目录：$APP${ACCOUNT:+；只看账户含 \"$ACCOUNT\" 的}${VERIFY:+；逐个列出需要验证账号的账户}"

printf '\n== 1. 版本与实例\n'
FIX_COUNT=""
PROC_START=""
pid="$({ tr -cd '0-9' < run/aistudio2api.pid; } 2>/dev/null)"
if [ -n "$pid" ] && [ -r "/proc/$pid/exe" ]; then
  FIX_COUNT="$(grep -a -c OMNI_IMAGE_BINDING "/proc/$pid/exe" 2>/dev/null)"
  # 进程启动时间（Unix 秒）：含修复的进程启动之前的登记可能是升级前的旧版本写的
  elapsed="$(ps -o etimes= -p "$pid" 2>/dev/null | tr -cd '0-9')"
  [ -n "$elapsed" ] && PROC_START="$(($(date +%s) - elapsed))"
  echo "本目录进程 PID $pid，启动于 $(ps -o lstart= -p "$pid" 2>/dev/null | sed 's/^ *//')，程序含修复字符串的行数 ${FIX_COUNT:-0}（0 = 早于 93c16fa，带图的 Omni 403 会误登记正常账户）"
else
  echo "run/aistudio2api.pid 不存在或进程已退出（服务没有运行，或不是用 start.sh 启动的）"
fi
APP_AUTH="$(env_file_value .env AISTUDIO_AUTH_STATES)"
APP_AUTH="${APP_AUTH:-auth}"; APP_AUTH="${APP_AUTH%%,*}"
case "$APP_AUTH" in /*) ;; *) APP_AUTH="$APP/$APP_AUTH" ;; esac
APP_AUTH="$(readlink -f "$APP_AUTH" 2>/dev/null || echo "$APP_AUTH")"
INSTANCES=""
for p in $(pgrep -x aistudio2api 2>/dev/null); do
  d="$(readlink "/proc/$p/cwd" 2>/dev/null)" || continue
  a="$(env_file_value "$d/.env" AISTUDIO_AUTH_STATES)"
  a="${a:-auth}"; a="${a%%,*}"
  case "$a" in /*) ;; *) a="$d/$a" ;; esac
  a="$(readlink -f "$a" 2>/dev/null || echo "$a")"
  started="$(ps -o etimes= -p "$p" 2>/dev/null | tr -cd '0-9')"
  INSTANCES="$INSTANCES$p|$d|$a|${started:+$(($(date +%s) - started))}
"
  echo "实例 PID $p | 目录 $d | 账户目录 $a | 启动于 $(ps -o lstart= -p "$p" 2>/dev/null | sed 's/^ *//')"
done
[ -z "$INSTANCES" ] && echo "本机没有名为 aistudio2api 的进程（docker 等其他方式运行的实例列不出来）"
SHARED="$(printf '%s' "$INSTANCES" | awk -F'|' -v dir="$APP_AUTH" '$3 == dir {n++} END {print n + 0}')"
if [ "${SHARED:-0}" -ge 2 ]; then
  echo "⚠ $SHARED 个实例共用本目录的账户目录 $APP_AUTH：403 登记文件是共用的，别的实例的请求同样会登记在这里"
fi
stat -c '403 登记文件 %n 最后写入 %y（登记表任何变化都会更新它，只能作参考）' "$APP_AUTH/.permission-denied.json" 2>/dev/null ||
  echo "403 登记文件 $APP_AUTH/.permission-denied.json 不存在"
if git -C "$APP" rev-parse --git-dir >/dev/null 2>&1; then
  echo "源码 $(git -C "$APP" log -1 --format='%h %cd' --date=iso 2>/dev/null)"
  if ! git -C "$APP" cat-file -e '93c16fa^{commit}' 2>/dev/null; then echo "源码里没有 93c16fa 这个提交（更早的版本或不是同一仓库）"
  elif git -C "$APP" merge-base --is-ancestor 93c16fa HEAD 2>/dev/null; then echo "源码含 93c16fa"; else echo "源码不含 93c16fa"; fi
  git -C "$APP" reflog --date=iso -n 8 --format='  %h %gd %<(40,trunc)%gs' 2>/dev/null
else
  echo "程序目录不是 git 源码目录（到源码目录看升级记录）"
fi
starts="$(grep -a -h '^===== .* start.sh ' logs/aistudio2api.log.1 logs/aistudio2api.log 2>/dev/null | tail -n 5)"
[ -n "$starts" ] && { echo "最近的启动记录："; printf '%s\n' "$starts" | sed 's/^/  /' | mask; }

export APP MODEL DAYS VERIFY FIX_COUNT PROC_START SHARED APP_AUTH INSTANCES
"$PYTHON" -I - <<'PY'
# 只读：只对本机管理接口发 GET、只读日志与登记文件；邮箱打码，不输出令牌与提示词。兼容 Python 3.7（不用 3.8 以后的语法）
import collections
import datetime
import http.client
import json
import os
import re
import urllib.error
import urllib.parse
import urllib.request

ENV = os.environ
APP = ENV["APP"].rstrip("/")
MODEL = (ENV.get("MODEL") or "gemini-omni-1.1-flash").strip()
ALL = MODEL.lower() == "all"
ACCOUNT = (ENV.get("ACCOUNT") or "").strip().lower()
# VERIFY=list 时第 2、4、6、7 段逐个列出需要验证账号（账户级 403）的登记与暂停行；默认只给汇总，免得上千个整户暂停的账户淹没其他结论
VERIFY_LIST = (ENV.get("VERIFY") or "").strip().lower() == "list"
# 用量账本一次查询最多 93 天（usageMaxSpan），查询多出 5 分钟，所以最多 92 天
DAYS = min(92, max(1, int(ENV.get("DAYS") or 92)))
FIX_COUNT = ENV.get("FIX_COUNT") or ""
SHARED = int(ENV.get("SHARED") or 0)
AUTH_DIR = ENV.get("APP_AUTH") or os.path.join(APP, "auth")
UTC = datetime.timezone.utc
NOW = datetime.datetime.now(UTC)
try:
    PROC_START = datetime.datetime.fromtimestamp(int(ENV.get("PROC_START") or ""), UTC)
except ValueError:
    PROC_START = None
# 93c16fa（带图的 Omni 请求被拒时不再登记为 403 无权限账户）的提交时间：最近一次 403 早于它的登记多半是旧版本误登记或很早以前的调用
FIX_AT = datetime.datetime(2026, 11, 5, 10, 28, 7, tzinfo=UTC)
EMAIL_RE = re.compile(r"([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@")
TIME_RE = re.compile(r"^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$")
ACCOUNT_LEVEL = re.compile(r"verify-account|must be verified", re.I)
INTERACTIONS = ("/v1/interactions", "/v1beta/interactions")
# new-api 渠道测试的请求形态：一条短消息（默认 "hi"），max_tokens 为 16 或 3000、或不带（按 QuantumNous/new-api 的渠道测试）
TEST_PATHS = ("/v1/chat/completions", "/v1/messages", "/v1/responses", ":generateContent", ":streamGenerateContent")
TEST_TOKENS = re.compile(r"^(16|3000|默认)(\D|$)")
# 账本补判（登记与运行日志都没有请求形态时，见 ledger_info 与 ledger_judge）：
# 请求正文（GET /api/requests/{id}/body）是客户端发来的原始 POST 请求体（API key 校验之后、协议转换之前，不含请求头），截断到前 64 KiB
# （internal/api/ledger.go 的 requestBodyLimit），只保留最近 1000 条（internal/requestdb 的 bodyRetention），REQUEST_BODY_LOG=false 时不存。
# 按请求 ID 只读一次，最多读 BODY_FETCH_LIMIT 个不同的请求（上千个账户多半是少数几个请求依次换号），超出的写明跳过了多少
BODY_LIMIT = 64 << 10
BODY_FETCH_LIMIT = 300
SUCCESS_STATES = ("completed", "tool_calls", "limited")
# 没有正文时按账本的 input_tokens 判断。它是最终那次上游尝试的输入用量（上游 usageMetadata 的 promptTokenCount 加 toolUsePromptTokenCount，
# Interactions 为用量数组的输入数；上游没给用量时为本地估算；见 internal/app/request_logging.go 的 requestLogData 与 requestRow），
# 被拒绝的尝试不计入，请求最终失败时为 0；上游终止生成（blocked：finishReason 不是 stop，HTTP 200）时上游已处理了提示词，用量照样是上游给的。
# 它比客户端发来的多出本服务加的内容：
#   随机后缀（REPEAT_PROMPT_NONCE，默认开启）：客户端没有指定 seed（IGNORE_CLIENT_SEED 默认忽略客户端的 seed）、温度不为 0、模型名不带 image、
#     tts、audio、veo 等的请求，发往上游之前在最后一条用户消息末尾加 32 个零宽字符（U+200B/U+200C，internal/app/duplicates.go 的
#     maybeAddPromptNonce）。运行日志的字数不含它（SetAccessLogGenerationInput 在加后缀之前），上游的输入 tokens 含它：按分词方式约十几个到
#     近百个 token（每个零宽字符按 UTF-8 3 字节回退时 96 个），所以一条 “hi” 的渠道测试在账本里也远不止几个 token；
#   系统提示：本服务不加（发给 Omni 视频模型时反而去掉客户端的系统提示，见 internal/api/omni_system.go；按工具设置追加的要求只在请求带工具时
#     才有，Omni 请求的工具在选号前已去掉）。
# 所以渠道测试的上限从账本自己校准（见 token_calibration）：同一次运行里请求形态已确认是渠道测试（运行日志的开始行或账本正文：1 条不超过 8 字的
# 消息、没有媒体与文件、max_tokens 为 16/3000/不带、渠道测试会用的接口）、最终成功的请求，按模型取输入 tokens 的最大值。超过 8 加随机后缀最大
# 贡献（NONCE_TOKENS_MAX）的样本不算：一条短消息加后缀到不了，多半另有工具声明等内容。阈值：
#   渠道测试（只认最终成功的）：不超过校准值加 8（与渠道测试判定里“1 条不超过 8 字的消息”同一量级，中文 1 字约 1 token，英文更少）；没有校准样本时
#     只认不超过 8 的（没有加随机后缀，与一条 “hi” 相当），随机后缀开启时介于之间的写出数字并注明含后缀，不下结论；
#   下游主动调用：不少于校准值（没有样本时为随机后缀可能的最大贡献，REPEAT_PROMPT_NONCE 关闭时为 0）加 100（约 75 个英文单词或上百个汉字，
#     带一张图就有几百 token，远不止一条短消息）；上游终止生成的请求用量是真的，同样按这条判下游；
#   之间：一句短问题、带默认系统提示的客户端都可能落在这里，不下结论，写出数字。
# .env 只反映现在的设置，那时的 REPEAT_PROMPT_NONCE 可能不同；校准样本是那时的实际输入 tokens，不受影响
LEDGER_TEST_TOKENS = 8
LEDGER_DOWNSTREAM_TOKENS = 100
NONCE_TOKENS_MAX = 96
# 请求 ID 的前缀（internal/api 里 newID 的调用处）：账本里没有这个请求、只知道请求 ID 时据此补一句接口类型，不改分类
ID_PREFIXES = (("resp_", "Gemini 原生 generateContent 或 OpenAI Responses"), ("chatcmpl_", "OpenAI /v1/chat/completions"),
               ("msg_", "Anthropic /v1/messages"), ("int_", "Interactions（直连本服务，new-api 不转发）"))
# 每个模型（以及每个登记条目的窄窗口查询）最多读的账本页数，每页 200 条
LEDGER_PAGES = 50
LEDGER_PAGE_SIZE = 200
# MODEL_FALLBACKS 没有写时的默认值（见 internal/config/model_fallback.go 的 DefaultModelFallbacks）
DEFAULT_FALLBACKS = "gemini-4-argon=gemini-4-carbon,gemini-4-carbon=gemini-4-argon"


def mask(text):
    return EMAIL_RE.sub(lambda m: m.group(1) + "***@", str(text or ""))


def parse_time(value):
    match = TIME_RE.match((value or "").strip())
    if not match:
        return None
    fraction = (match.group(2) or "0")[:6].ljust(6, "0")
    zone = "+00:00" if match.group(3) == "Z" else match.group(3)
    try:
        return datetime.datetime.fromisoformat("%s.%s%s" % (match.group(1), fraction, zone))
    except ValueError:
        return None


def show(moment):
    return moment.astimezone().strftime("%Y-%m-%d %H:%M:%S") if moment else "-"


def short(moment):
    # 与管理页“出问题”的“最近 MM-DD HH:MM”相同（本机时区）
    return moment.astimezone().strftime("%m-%d %H:%M") if moment else "-"


def utc_param(moment):
    return moment.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def env_entry(name):
    # 与服务读 .env 的写法一致：允许 export 前缀、引号，无引号的值去掉 “ #” 之后的注释；取最后一次出现的值。
    # 返回 (是否写了这个键, 值)：MODEL_FALLBACKS 写空值表示关闭，没有写才用默认值
    found, value = False, ""
    try:
        with open(os.path.join(APP, ".env"), encoding="utf-8-sig", errors="replace") as handle:
            for line in handle:
                line = line.strip()
                if re.match(r"export\s", line):
                    line = line[6:].strip()
                key, sep, raw = line.partition("=")
                if sep and key.strip() == name:
                    found = True
                    raw = raw.strip()
                    if raw[:1] in ("'", '"') and raw[0] in raw[1:]:
                        value = raw[1:raw.index(raw[0], 1)]
                    else:
                        value = re.split(r"\s#", raw, maxsplit=1)[0].strip()
    except OSError:
        pass
    return found, value


def env_value(name):
    return env_entry(name)[1]


def base_path(path):
    # 排查路由 /trace/ 与号池路由 /ultra/、/pro/ 的请求去掉前缀后再归类（/trace/ultra/、/trace/pro/ 依次去掉）
    path = path or ""
    for prefix in ("/trace", "/ultra", "/pro"):
        if path.startswith(prefix + "/"):
            path = path[len(prefix):]
    return path


def fallback_name(name):
    name = (name or "").strip()
    return name[len("models/"):] if name.startswith("models/") else name


# MODEL_FALLBACKS：源模型（小写）→ 目标模型（小写）；写法无效时服务按默认值启动，这里同样按默认值
def parse_fallbacks(value):
    rules = {}
    for item in re.split(r"[,，\r\n]", value or ""):
        source, sep, targets = item.strip().partition("=")
        if not item.strip():
            continue
        if not sep or not fallback_name(source):
            return None
        rules[fallback_name(source).lower()] = [fallback_name(t).lower() for t in targets.split("|") if fallback_name(t)]
    return rules


FALLBACKS_SET, FALLBACKS_RAW = env_entry("MODEL_FALLBACKS")
FALLBACKS = parse_fallbacks(FALLBACKS_RAW if FALLBACKS_SET else DEFAULT_FALLBACKS)
if FALLBACKS is None:
    FALLBACKS = parse_fallbacks(DEFAULT_FALLBACKS)
FALLBACK_SCOPE = (env_value("MODEL_FALLBACK_SCOPE") or "ultra").strip().lower()
if FALLBACK_SCOPE not in ("ultra", "all", "off"):
    FALLBACK_SCOPE = "ultra"
def env_off(name):
    # 默认开启的开关（按 Go 的 strconv.ParseBool 解析）：.env 里写成 false 的写法时为关闭；没有写、写空或写法无效时服务按默认值开启
    return env_value(name).strip() in ("0", "f", "F", "FALSE", "false", "False")


# REQUEST_BODY_LOG：关闭时账本不存请求正文；REPEAT_PROMPT_NONCE：开启时上游的输入 tokens 含本服务加的随机后缀（见 LEDGER_TEST_TOKENS 的说明）；
# OMNI_IGNORE_SYSTEM_PROMPT：开启时发给 Omni 视频模型的请求去掉客户端的系统提示，运行日志的字数不含它（见 shape_counts）
REQUEST_BODY_OFF = env_off("REQUEST_BODY_LOG")
NONCE_ON = not env_off("REPEAT_PROMPT_NONCE")
OMNI_SYSTEM_DROP = not env_off("OMNI_IGNORE_SYSTEM_PROMPT")


def fallback_applies(path):
    # MODEL_FALLBACK_SCOPE：ultra（默认）只对 /ultra 路径（含 /trace/ultra）的请求改用，不含 /pro；all 对所有路径（默认地址、/pro 与 /ultra）；
    # off 关闭；路径不知道时按可能
    if FALLBACK_SCOPE == "off":
        return False
    if FALLBACK_SCOPE == "all" or not path:
        return True
    if path.startswith("/trace/"):
        path = path[len("/trace"):]
    return path.startswith("/ultra/")


# ---------- 管理接口（只发 GET） ----------
host, _, port = (env_value("LISTEN_ADDR") or "127.0.0.1:2048").rpartition(":")
host = "127.0.0.1" if host in ("", "0.0.0.0", "[::]", "::") else host
try:
    with open(os.path.join(APP, ".admin-token"), encoding="utf-8", errors="replace") as handle:
        TOKEN = re.sub(r"[^0-9a-fA-F]", "", handle.read())
except OSError:
    TOKEN = ""
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
API_STATE = {"value": "ok", "reason": ""}


def get(path, timeout=30, sticky=True):
    """返回 (数据, HTTP 状态, 错误说明)；连不上或令牌被拒绝时之后不再访问。状态 0 为没有响应。sticky 为假时（读请求正文）超时、连接中断与
    响应不是 JSON 只返回说明，不把管理接口当成连不上：一次读正文超时不影响之后的请求"""
    if API_STATE["value"] in ("down", "denied"):
        return None, 0, API_STATE["reason"]
    request = urllib.request.Request("http://%s:%s%s" % (host, port, path), headers={"X-Admin-Token": TOKEN})
    try:
        with OPENER.open(request, timeout=timeout) as response:
            return json.load(response), response.status, ""
    except urllib.error.HTTPError as error:
        detail = ""
        try:
            body = json.loads(error.read().decode("utf-8", "replace") or "{}")
            problem = body.get("error") if isinstance(body, dict) else None
            detail = str((problem.get("message") if isinstance(problem, dict) else problem) or "")
        except (ValueError, OSError, AttributeError):
            pass
        if error.code in (401, 403):
            API_STATE["value"], API_STATE["reason"] = "denied", "管理接口拒绝访问（HTTP %d）：%s" % (
                error.code, "找不到 .admin-token" if not TOKEN else "管理令牌不对，确认 .admin-token 属于这个服务")
            detail = API_STATE["reason"]
        return None, error.code, mask(detail)[:200]
    except (urllib.error.URLError, OSError, ValueError, http.client.HTTPException) as error:
        if not sticky:
            cause = error.reason if isinstance(error, urllib.error.URLError) and isinstance(error.reason, BaseException) else error
            return None, 0, "%s（%s）" % ("响应不是 JSON" if isinstance(error, ValueError) else "超时或连接中断", type(cause).__name__)
        API_STATE["value"], API_STATE["reason"] = "down", "管理接口 http://%s:%s 连不上（服务没有运行，或 LISTEN_ADDR 不是这个地址）：%s" % (
            host, port, type(error).__name__)
        return None, 0, API_STATE["reason"]


def wanted_account(account_id, label):
    return not ACCOUNT or ACCOUNT in (account_id or "").lower() or ACCOUNT in (label or "").lower()


# ---------- 2. 403 登记 ----------
print("\n== 2. 403 登记（时间为本机时区 %s）" % datetime.datetime.now().astimezone().strftime("%z"))
listed, _, _ = get("/api/accounts/permission-denied")
source = "管理接口"
if listed is None:
    print("⚠ " + (API_STATE["reason"] or "读不到管理接口") + "；改读登记文件")
    source = "登记文件"
    listed = {"accounts": []}
    try:
        with open(os.path.join(AUTH_DIR, ".permission-denied.json"), encoding="utf-8") as handle:
            stored = json.load(handle).get("accounts") or {}
        for account_id, account in stored.items():
            models = []
            for model_id, model in ((account or {}).get("models") or {}).items():
                if model and model.get("channels"):
                    models.append(dict(model, model=model_id))
            listed["accounts"].append({"id": account_id, "label": account_id, "models": models})
    except (OSError, ValueError) as error:
        print("登记文件读不到：%s" % type(error).__name__)


def account_keys(account_id, label):
    return {key for key in ((account_id or "").lower(), (label or "").lower()) if key}


# 需要验证账号按账户判断（与管理页 403 列表隐藏的账户同一口径，不受 MODEL 过滤）：登记里任一模型的原文是账户级 403 的账户，以及
# 需要验证账号的暂停此刻仍生效的账户（冷却列表里 verify 为 account 的整户暂停、为 model 的单个模型需要验证；旧版本没有 verify 字段时
# 按原因：含 verify-account、以“次返回 403 无权限，自动暂停”结尾，以“单个模型需要验证账号”开头的是单个模型。登记已清除、暂停还在的也算）。
# 分两类，与服务端、管理页同一口径：整户（整户暂停此刻生效，或登记里账户级 403 涉及 2 个及以上模型，按小写、去掉 models/ 归一）与单个模型
# （其余：只暂停那个模型，账户其他模型照常调度）。这些账户整个不列在管理页 403 列表里（见需要验证账号汇总块与“403 单个模型需要验证”），
# 这里同样默认只汇总；同一账户其他模型上的普通 403 也算在它名下
MODEL_VERIFY_PREFIX = "单个模型需要验证账号"


def norm_model(model):
    return re.sub(r"^models/", "", (model or "").strip()).lower()


VERIFY_MODELS = {}  # 账户（小写 ID 与邮箱）→ 登记里账户级 403 的模型
for account in listed.get("accounts") or []:
    verify_models = [m.get("model") or "" for m in account.get("models") or [] if ACCOUNT_LEVEL.search(m.get("message") or "")]
    if verify_models:
        for key in account_keys(account.get("id"), account.get("label")):
            VERIFY_MODELS[key] = verify_models
# 冷却列表（只读 GET；整户暂停按账户的每个模型展开成一条）。读不到（旧版本、管理接口连不上）时按运行日志近似（见 log_paused），
# 也不知道哪些账户的暂停已到期
api_before = dict(API_STATE)
cooldown_list, _, _ = get("/api/cooldowns", timeout=60)
if cooldown_list is None and api_before["value"] == "ok" and API_STATE["value"] == "down":
    # 上千个账户整户暂停时冷却列表有几万条，读超时不当成管理接口连不上，后面照常读目录与账本
    API_STATE.update(api_before)
COOLDOWNS_OK = cooldown_list is not None
PAUSED = set()  # 需要验证账号整户暂停此刻仍生效的账户（小写 ID 与邮箱）
MODEL_VERIFY = {}  # 单个模型需要验证的暂停此刻仍生效的账户（小写 ID 与邮箱）→ 归一的模型


def verify_kind(cooldown, reason):
    """冷却的需要验证账号种类：account、model 或空；优先用冷却列表的 verify 字段，旧版本没有时按原因判断"""
    kind = cooldown.get("verify") or ""
    if kind in ("account", "model"):
        return kind
    if reason.endswith("次返回 403 无权限，自动暂停") and ACCOUNT_LEVEL.search(reason):
        return "model" if reason.startswith(MODEL_VERIFY_PREFIX) else "account"
    return ""


for cooldown in (cooldown_list or {}).get("cooldowns") or []:
    reason = (cooldown.get("reason") or "").strip()
    until = parse_time(cooldown.get("until"))
    if not until or until <= NOW:
        continue
    kind = verify_kind(cooldown, reason)
    if kind == "account":
        PAUSED.update(account_keys(cooldown.get("account_id"), cooldown.get("account_label")))
    elif kind == "model":
        for key in account_keys(cooldown.get("account_id"), cooldown.get("account_label")):
            MODEL_VERIFY.setdefault(key, set()).add(norm_model(cooldown.get("model_id")))


def log_paused():
    """读不到冷却列表时按本实例运行日志近似需要验证账号的暂停仍生效的账户，返回（整户暂停的账户，单个模型需要验证的账户 → 模型）：
    整户为这个账户最后一行需要验证账号的“账号暂停调度”之后没有“解除需要验证账号的整户暂停”；单个模型为这个账户这个模型最后一行
    “账号冷却 | 类型=单个模型需要验证”之后没有“解除单个模型需要验证的暂停”；都要不到 30 天（管理页“验证”通过与认证文件更新解除暂停时
    不写解除行，这里看不出来）"""
    last, models = {}, {}
    for name in ("aistudio2api.log.1", "aistudio2api.log"):
        try:
            handle = open(os.path.join(APP, "logs", name), encoding="utf-8", errors="replace")
        except OSError:
            continue
        with handle:
            for line in handle:
                if "账号暂停调度" not in line and "解除需要验证账号的整户暂停" not in line and \
                        "类型=单个模型需要验证" not in line and "解除单个模型需要验证的暂停" not in line:
                    continue
                try:
                    item = json.loads(line)
                except ValueError:
                    continue
                message, account = item.get("msg") or "", (item.get("source") or "").lower()
                moment = parse_time(item.get("time"))
                if "账号暂停调度" in message and ACCOUNT_LEVEL.search(message):
                    last[account] = moment
                elif "解除需要验证账号的整户暂停" in message and "失败" not in message:
                    last[account] = None
                elif message.startswith("账号冷却 | 类型=单个模型需要验证"):
                    scope = re.search(r"范围=([^ |]+)", message)
                    if scope:
                        models.setdefault(account, {})[norm_model(scope.group(1))] = moment
                elif "解除单个模型需要验证的暂停" in message and "失败" not in message:
                    models.pop(account, None)
    recent = lambda moment: moment and NOW - moment < datetime.timedelta(days=30)
    paused = {account for account, moment in last.items() if recent(moment)}
    cooled = {account: {model for model, moment in items.items() if recent(moment)} for account, items in models.items()}
    return paused, {account: items for account, items in cooled.items() if items}


if not COOLDOWNS_OK:
    PAUSED, MODEL_VERIFY = log_paused()

entries = []
for account in listed.get("accounts") or []:
    account_id, label = account.get("id") or "", account.get("label") or account.get("id") or ""
    if not wanted_account(account_id, label):
        continue
    keys = account_keys(account_id, label)
    verify_models = next((VERIFY_MODELS[key] for key in keys if key in VERIFY_MODELS), [])
    paused = bool(keys & PAUSED)
    cooled_models = set().union(*[MODEL_VERIFY.get(key, set()) for key in keys])
    registry_models = {norm_model(model) for model in verify_models}
    # verify_scope：整户（整户暂停此刻生效，或登记里账户级 403 涉及 2 个及以上模型）、单个模型（其余有账户级登记或单个模型暂停的）、空
    verify_scope = "account" if paused or len(registry_models) >= 2 else "model" if registry_models or cooled_models else ""
    models = account.get("models") or []
    for item in models:
        if not ALL and item.get("model") != MODEL:
            continue
        entries.append({
            "id": account_id, "label": label, "model": item.get("model") or "", "channels": item.get("channels") or [],
            "first": parse_time(item.get("first_at")), "last": parse_time(item.get("last_at")), "count": item.get("count") or 0,
            "message": item.get("message") or "", "last_request": item.get("last_request") or None,
            "others": [m.get("model") for m in models if m.get("model") != item.get("model")],
            # verify：这一条是账户级 403（需要验证账号）；verify_account：账户需要验证账号（任一模型有账户级登记，或整户暂停仍生效），
            # 第一次遇到就整户暂停调度 30 天，管理页不列在 403 列表里、另有汇总块；verify_models 为登记里账户级 403 的模型；
            # paused 为整户暂停此刻仍生效（读不到冷却列表时按运行日志近似，见 log_paused）
            "verify": bool(ACCOUNT_LEVEL.search(item.get("message") or "")),
            "verify_account": bool(verify_scope), "verify_models": verify_models, "paused": paused,
            # verify_scope 见上；model_paused 为单个模型需要验证的暂停此刻仍生效；verify_targets 为单个模型时暂停的模型（登记 ∪ 冷却）
            "verify_scope": verify_scope, "model_paused": bool(cooled_models),
            "verify_targets": sorted(registry_models | cooled_models),
        })
entries.sort(key=lambda e: e["last"] or datetime.datetime.min.replace(tzinfo=UTC), reverse=True)
VERIFY_ENTRIES = [e for e in entries if e["verify_account"]]
VERIFY_ACCOUNT_ENTRIES = [e for e in VERIFY_ENTRIES if e["verify_scope"] == "account"]
VERIFY_MODEL_ENTRIES = [e for e in VERIFY_ENTRIES if e["verify_scope"] == "model"]


def detailed(entry):
    # 是否逐条列出：需要验证账号的账户的登记默认只汇总，VERIFY=list 时逐条列出
    return VERIFY_LIST or not entry["verify_account"]


def pause_expired(entry):
    # 暂停不在生效中（读得到冷却列表、账户却不在暂停里）。整户看整户暂停：暂停 30 天到期，或旧版本留下、启动时已到期没有补写暂停的登记，
    # 写“暂停已到期”；单个模型看那个模型的暂停：除了到期，生成路径也可能没有写入（目录代际变化等，运行日志为“账号冷却未写入”），
    # 与管理页同一说法写“暂停未生效（已到期或没有写入）”，第 6 段读到运行日志后能分出没有写入的（见 model_pause_unwritten）。
    # 读不到冷却列表时不知道，不算
    if entry.get("verify_scope") == "model":
        return COOLDOWNS_OK and not entry["model_paused"]
    return COOLDOWNS_OK and not entry["paused"]


# MODEL_UNWRITTEN 为运行日志里有“账号冷却未写入 | 类型=单个模型需要验证”、却没有一行写入成功（账号冷却 | 类型=单个模型需要验证）的账户
# （小写邮箱或 ID），第 6 段读运行日志时填入；之前为 None（第 2、4 段只能写“已到期或没有写入”）
MODEL_UNWRITTEN = None


def model_pause_unwritten(entry):
    # 单个模型需要验证的暂停此刻不生效、运行日志里只有没有写入的行：暂停从没写入，不是 30 天到期
    return entry.get("verify_scope") == "model" and pause_expired(entry) and bool(MODEL_UNWRITTEN) and \
        bool(account_keys(entry["id"], entry["label"]) & MODEL_UNWRITTEN)


def per_account(items):
    # 每个账户（不区分大小写）取一条：优先账户级 403 的那条（整户暂停由它引起），其次最近一条；items 已按最近一次 403 从新到旧排列
    chosen = collections.OrderedDict()
    for item in items:
        key = (item["id"] or item["label"] or "").lower()
        if key not in chosen or item["verify"] and not chosen[key]["verify"]:
            chosen[key] = item
    return list(chosen.values())


def verify_brief(items):
    # 需要验证账号的账户数、登记条数与最近一次 403 的时间范围（每个账户取一条，见 per_account），读得到冷却列表时另写暂停已到期的个数；
    # 单个模型的另写模型分布
    accounts = per_account(items)
    times = [item["last"] for item in accounts if item["last"]]
    span = "最近一次 403 在 %s 至 %s" % (show(min(times)), show(max(times))) if times else "登记里没有时间"
    expired = sum(1 for item in accounts if pause_expired(item))
    single = bool(accounts) and accounts[0].get("verify_scope") == "model"
    if not expired:
        state = ""
    elif single:
        # 与管理页“403 单个模型需要验证”同一说法：暂停未生效（已到期或没有写入）；读到运行日志后另写其中没有写入的个数
        unwritten = sum(1 for item in accounts if model_pause_unwritten(item))
        state = "；其中 %d 个该模型的暂停仍生效、%d 个暂停未生效（已到期或没有写入，该模型照常调度%s）" % (
            len(accounts) - expired, expired, "；其中 %d 个运行日志里只有“账号冷却未写入”，暂停从没写入" % unwritten if unwritten else "")
    else:
        state = "；其中 %d 个整户暂停仍生效、%d 个暂停已到期（重新参与调度）" % (len(accounts) - expired, expired)
    spread = ""
    if single:
        counts = collections.Counter(model for item in accounts for model in item["verify_targets"])
        spread = "；模型分布 " + "、".join("%s %d" % item for item in counts.most_common())
    return "%d 个账户（%d 条登记），%s%s%s" % (len(accounts), len(items), span, state, spread)


for entry in entries:
    if not detailed(entry):
        continue
    method = re.search(r"AI Studio (\w+) 返回", entry["message"])
    line = "%s | %s | 通道 %s | 首次 %s | 最近 %s | %d 次 | 方法 %s | 该账户其他 403 模型 %d 个" % (
        mask(entry["label"]), entry["model"], ",".join(entry["channels"]) or "-", show(entry["first"]), show(entry["last"]),
        entry["count"], method.group(1) if method else "-", len(entry["others"]))
    scope = "整户" if entry["verify_scope"] == "account" else "单个模型，只暂停 " + "、".join(entry["verify_targets"])
    if entry["verify"]:
        line += " | 需要验证账号（账户级，%s）" % scope
    elif entry["verify_account"]:
        line += " | 该账户需要验证账号（%s；%s）" % (scope,
            "账户级 403 登记在 " + "、".join(entry["verify_models"]) if entry["verify_models"] else "登记里没有账户级条目，暂停仍生效")
    print(line)
    last = entry["last_request"]
    if last:
        print("    最近一次请求：%s · 请求名 %s · %d 条消息 · 图片 %d · 文件 %d · ID %s" % (
            last.get("path") or "-", last.get("requested_model") or entry["model"], last.get("messages") or 0,
            last.get("images") or 0, last.get("files") or 0, last.get("id") or "-"))
if VERIFY_ACCOUNT_ENTRIES and not VERIFY_LIST:
    print("需要验证账号（整户：2 个及以上模型要求验证或整户暂停生效，整户暂停调度 30 天，管理页不在 403 列表里）：%s；不逐个列出（VERIFY=list 逐个列出）" % (
        verify_brief(VERIFY_ACCOUNT_ENTRIES)))
if VERIFY_MODEL_ENTRIES and not VERIFY_LIST:
    print("单个模型需要验证（只暂停该模型最长 30 天，其他模型照常，管理页列在“403 单个模型需要验证”）：%s；不逐个列出（VERIFY=list 逐个列出）" % (
        verify_brief(VERIFY_MODEL_ENTRIES)))
print("共 %d 条%s（来源：%s）" % (len(entries), "，其中需要验证账号的账户 %d 条" % len(VERIFY_ENTRIES) if VERIFY_ENTRIES else "", source)
      if entries else "没有%s的登记（来源：%s）" % ("" if ALL else "这个模型", source))

# 模型目录（第 4 段按别名找改用的源模型、第 5 段列出别名、第 7 段判断别名与 Interaction 模型）
catalog, _, _ = get("/api/models")
CATALOG_OK = catalog is not None
CATALOG = (catalog or {}).get("models") or []
CATALOG_IDS = {m.get("id") for m in CATALOG}
ALIAS_OF = {}  # 小写别名 → 目录 ID（别名本身不是目录 ID 时）
for item in CATALOG:
    for alias in (item.get("capability_options") or {}).get("aliases") or []:
        if alias not in CATALOG_IDS:
            ALIAS_OF.setdefault(alias.lower(), item.get("id") or "")


def canonical(name):
    name = fallback_name(name)
    return (ALIAS_OF.get(name.lower()) or name).lower()


def fallback_sources(model_id):
    # 按 MODEL_FALLBACKS 改用到 model_id 的源模型（规则里的写法；目标写的是别名时按目录换成正式 ID 再比较）
    target = model_id.lower()
    return sorted(source for source, targets in FALLBACKS.items() if any(canonical(t) == target or t == target for t in targets))


def fallback_pair(requested, model_id):
    # 客户端请求 requested、MODEL_FALLBACKS 把它改用到 model_id 的规则（源写的是别名时同样认）
    names = {requested.lower(), canonical(requested)}
    return any(source in names or canonical(source) in names for source in fallback_sources(model_id))


def interaction_model(model_id):
    for item in CATALOG:
        if item.get("id") == model_id:
            return bool((item.get("capabilities") or {}).get("interactions_api"))
    return model_id.lower().startswith("gemini-omni")


def omni_video(model_id):
    # 能输出视频的 Omni 模型（与 aistudio.OmniVideoModel 相同：目录里有 interactions_api 能力、带视频选项）：OMNI_IGNORE_SYSTEM_PROMPT 开启时
    # 发给它的请求去掉客户端的系统提示。目录里没有这个模型时按名字
    for item in CATALOG:
        if item.get("id") == model_id:
            options = item.get("capability_options") or {}
            return bool((item.get("capabilities") or {}).get("interactions_api")) and any(
                options.get(key) for key in ("video_output_resolutions", "video_duration_range", "video_tasks"))
    return model_id.lower().startswith("gemini-omni")


# ---------- 3. 用量账本 ----------
models_of_interest = sorted({e["model"] for e in entries}) if ALL else [MODEL]
print("\n== 3. 用量账本最近 %d 天的请求（账本只记模型、接口、账户与结果，不记密钥、IP、User-Agent 与提示词）" % DAYS)
records = {}
# ledger：ok 读到；off 账本没有打开（接口 404）；bad 查询被拒绝（400）；down 连不上；denied 令牌被拒；error 其他错误
ledger, ledger_detail = "ok", ""


def ledger_state(status, detail):
    if status == 0:
        return "denied" if API_STATE["value"] == "denied" else "down"
    if status in (401, 403):
        return "denied"
    return {404: "off", 400: "bad"}.get(status, "error")


truncated = {}
for model_id in models_of_interest:
    params = {"from": utc_param(NOW - datetime.timedelta(days=DAYS)), "to": utc_param(NOW + datetime.timedelta(minutes=5)),
              "model": model_id, "limit": str(LEDGER_PAGE_SIZE)}
    cursor, oldest = "", None
    for _ in range(LEDGER_PAGES):
        query = dict(params)
        if cursor:
            query["cursor"] = cursor
        page, status, detail = get("/api/usage/records?" + urllib.parse.urlencode(query))
        if page is None:
            ledger, ledger_detail = ledger_state(status, detail), "HTTP %d%s" % (status, "：" + detail if detail else "")
            break
        for record in page.get("items") or []:
            records[record.get("id")] = record
            oldest = record.get("time") or oldest
        cursor = page.get("next_cursor") or ""
        if not cursor:
            break
    if ledger != "ok":
        break
    if cursor:
        truncated[model_id] = parse_time(oldest)
if ledger == "off":
    print("用量账本读不到（%s：多半是账本没有打开，启动日志里有“请求账本打开失败，本次运行不记录用量”的 WARN）；第 3 段跳过，第 4 段只能用登记里的"
          " last_request 与运行日志里对应的请求" % ledger_detail)
elif ledger == "bad":
    print("用量账本查询被拒绝（%s）" % ledger_detail)
elif ledger in ("down", "denied"):
    print("读不到用量账本：" + (API_STATE["reason"] or ledger_detail))
elif ledger == "error":
    print("用量账本读取失败（%s）" % ledger_detail)
ordered = sorted(records.values(), key=lambda r: r.get("time") or "")
by_model = collections.defaultdict(lambda: {"paths": collections.Counter(), "days": collections.defaultdict(lambda: [0, 0])})
for record in ordered:
    stats = by_model[record.get("model") or "-"]
    stats["paths"][record.get("path") or "-"] += 1
    day = stats["days"][show(parse_time(record.get("time")))[:10]]
    day[0] += 1
    day[1] += record.get("state") not in ("failed", "cancelled")
if ledger == "ok":
    for model_id in models_of_interest:
        stats = by_model.get(model_id)
        count = sum(stats["paths"].values()) if stats else 0
        print("%s：%d 条" % (model_id, count))
        if model_id in truncated:
            print("  ⚠ 超过 %d 条，只统计了最近的 %d 条（最早到 %s）；第 4 段按每条登记的时间单独查账本，不受这个上限影响" % (
                LEDGER_PAGES * LEDGER_PAGE_SIZE, LEDGER_PAGES * LEDGER_PAGE_SIZE, show(truncated[model_id])))
        if stats:
            print("  按接口：" + "；".join("%s %d" % item for item in stats["paths"].most_common()))
            print("  按天（总数/成功）：" + "；".join("%s %d/%d" % (k, v[0], v[1]) for k, v in sorted(stats["days"].items())))


# ---------- 4. 最近一次 403 对应的请求 ----------
def same_account(value, entry):
    value = (value or "").lower()
    return value != "" and value in ((entry["id"] or "").lower(), (entry["label"] or "").lower())


def denied_here(record, entry):
    tried = [a for a in record.get("attempts") or [] if same_account(a.get("account"), entry) and "403" in (a.get("error") or "")]
    return bool(tried) or same_account(record.get("account"), entry) and "403" in (record.get("error") or "")


def ledger_window(entry):
    """没有 last_request 的旧登记按时间在账本里找：请求结束时间在最近一次 403 前 2 分钟到后 60 分钟、模型为登记的模型（或按 MODEL_FALLBACKS
    改用到它的源模型）、这个账户在其中返回过 403，取结束时间离最近一次 403 最近的一条；返回 (记录, 说明)"""
    models = [entry["model"]] + [m for m in fallback_sources(entry["model"]) if m != entry["model"].lower()]
    params = {"from": utc_param(entry["last"] - datetime.timedelta(minutes=2)), "to": utc_param(entry["last"] + datetime.timedelta(minutes=60)),
              "model": ",".join(models), "limit": str(LEDGER_PAGE_SIZE)}
    best, best_gap, cursor, pages = None, None, "", 0
    for pages in range(1, LEDGER_PAGES + 1):
        query = dict(params)
        if cursor:
            query["cursor"] = cursor
        page, status, detail = get("/api/usage/records?" + urllib.parse.urlencode(query))
        if page is None:
            return best, "账本查询失败（HTTP %d%s）" % (status, "：" + detail if detail else "")
        for record in page.get("items") or []:
            finished = parse_time(record.get("time"))
            if finished is None or not denied_here(record, entry):
                continue
            gap = abs((finished - entry["last"]).total_seconds())
            if best_gap is None or gap < best_gap:
                best, best_gap = record, gap
        cursor = page.get("next_cursor") or ""
        if not cursor:
            break
    note = "（这段时间超过 %d 条，只看了前 %d 页）" % (LEDGER_PAGES * LEDGER_PAGE_SIZE, pages) if cursor else ""
    if best is None:
        return None, note
    finished = parse_time(best.get("time"))
    return best, "按时间在账本里找到（最近一次 403 后 %d 秒结束）%s" % (round((finished - entry["last"]).total_seconds()), note)


for entry in entries:
    entry["record"], entry["how"], entry["id_missing"] = None, "", False
    last = entry["last_request"] or {}
    entry["age"] = (NOW - entry["last"]).days if entry["last"] else -1
    if last.get("id"):
        entry["how"] = "登记的最近一次请求（按请求 ID）"
        entry["record"] = records.get(last["id"])
        if entry["record"] is None and ledger == "ok":
            page, _, _ = get("/api/usage/records?" + urllib.parse.urlencode({
                "from": utc_param(NOW - datetime.timedelta(days=DAYS)), "to": utc_param(NOW + datetime.timedelta(minutes=5)),
                "q": last["id"], "limit": "20"}))
            for record in (page or {}).get("items") or []:
                if record.get("id") == last["id"]:
                    entry["record"] = record
            # 账本开着、请求 ID 却查不到，最近一次 403 又在查询范围内：请求不是经过本实例的
            entry["id_missing"] = entry["record"] is None and page is not None and 0 <= entry["age"] < DAYS
        continue
    if ledger != "ok" or entry["last"] is None:
        continue
    entry["record"], entry["how"] = ledger_window(entry)

# 运行日志：只取这些请求的 request.started 与 request.finished（只读，逐行扫描），另记下运行日志最早一行的时间
wanted = {e["record"]["id"] for e in entries if e["record"]} | {(e["last_request"] or {}).get("id") for e in entries}
wanted.discard(None)
wanted.discard("")
starts, finishes, log_files, log_since = {}, {}, [], None
for name in ("aistudio2api.log.1", "aistudio2api.log"):
    path = os.path.join(APP, "logs", name)
    try:
        handle = open(path, encoding="utf-8", errors="replace")
    except OSError:
        continue
    log_files.append(name)
    with handle:
        for line in handle:
            if log_since is None and line.startswith("{"):
                try:
                    log_since = parse_time(json.loads(line).get("time"))
                except ValueError:
                    pass
            if '"request.started"' not in line and '"request.finished"' not in line:
                continue
            try:
                item = json.loads(line)
            except ValueError:
                continue
            request = item.get("request") or {}
            if request.get("id") not in wanted:
                continue
            (starts if item.get("event") == "request.started" else finishes)[request["id"]] = request


def log_covers(entry):
    # 运行日志最早一行比这次 403 早一小时以上（请求开始在 403 之前，长请求也很少超过一小时）：这个请求如果经过本实例，开始行应该在日志里
    return bool(log_files) and log_since is not None and entry["last"] is not None and log_since <= entry["last"] - datetime.timedelta(hours=1)


# 账本里没有这个请求 ID、本实例运行日志里却有它时的说明：请求经过本实例，账本在请求结束时才写（登记在第一次 403 时就写了），
# 写入队列满或写入失败时会丢记录
LEDGER_PENDING = "账本里还没有这个请求（可能还在进行，或账本写入丢失，见运行日志的“请求账本写入队列已满”与“请求账本写入失败”）"


def log_absence(entry):
    # 运行日志里没有这个请求时按时间说明：日志覆盖这次 403 时不是轮转掉了；账本里也没有它时多半不是经过本实例的
    if not log_files or log_since is None:
        return "没有运行日志（logs/aistudio2api.log 与 .1 都不在）"
    if log_covers(entry):
        if entry["record"]:
            return "本实例运行日志（从 %s 开始）里没有这个请求的开始行" % show(log_since)
        return "本实例运行日志（从 %s 开始）里没有这个请求：多半不是经过本实例的" % show(log_since)
    if entry["last"] is not None and log_since <= entry["last"]:
        # 日志开始得比这次 403 早、却不到一小时：请求可能在日志开始之前就已开始，不说“晚于”，也不下“已轮转”的结论
        return "运行日志从 %s 开始，只比这次 403 早 %d 分钟，不能确定请求开始时日志是否已在（只保留当前和上一份，各约 50MB）" % (
            show(log_since), (entry["last"] - log_since).total_seconds() // 60)
    return "运行日志从 %s 开始，晚于这次 403：已轮转（只保留当前和上一份，各约 50MB）" % show(log_since)


def request_began(entry):
    # 请求开始的时间：账本的时间是请求结束的时间，减去耗时；没有记录时按最近一次 403
    record = entry["record"] or {}
    finished = parse_time(record.get("time"))
    if finished is None:
        return entry["last"]
    try:
        elapsed = max(0, int(record.get("duration_ms") or 0))
    except (TypeError, ValueError):
        elapsed = 0
    return finished - datetime.timedelta(milliseconds=elapsed)


def log_gap(entry):
    # 运行日志看不到形态的原因：(分类, 说明)。按请求开始的时间区分“已轮转”与“覆盖这段时间却没有开始行”；账本补判的条目第 4、7 段都用它
    if entry.get("start"):
        return "loggap", "运行日志的开始行里没有消息条数"
    if entry.get("finish"):
        return "loggap", "运行日志里只有这个请求的结束行，开始行不在"
    if not log_files or log_since is None:
        return "nolog", "没有运行日志（logs/aistudio2api.log 与 .1 都不在）"
    began = request_began(entry)
    if began is not None and log_since <= began:
        return "loggap", "运行日志（从 %s 开始）覆盖这个请求开始的时间（%s），却没有它的开始行" % (show(log_since), show(began))
    return "rotated", "运行日志已轮转（从 %s 开始，晚于这个请求开始的 %s；只保留当前和上一份，各约 50MB）" % (show(log_since), show(began))


# ---------- 账本补判请求形态 ----------
# 登记没有请求形态（f06ddf9 之前的版本不记 last_request）、运行日志里也没有这个请求的开始行，账本里却对上了记录时，用账本补判：
# 先看记录里的请求正文（has_body），按接口解析出与运行日志 request.started 同口径的形态，交给第 7 段同一套判断；没有正文时按最终成功时的
# 输入 tokens；仍判断不了时写明已知的与看不到形态的原因。正文只取计数、字数与参数值，解析后不保留、不输出任何文字
BODY_CACHE = {}  # 请求 ID → 正文解析出的形态与说明（见 body_info），不存正文
BODY_SKIPPED = set()  # 超过 BODY_FETCH_LIMIT 没有读正文的请求 ID
# 读正文超时或连接中断一次后不再读（几百个请求各等 30 秒太久），之后的请求 ID 记入 BODY_ABORTED；BODY_ABORT 为那一次的说明与请求 ID
BODY_ABORTED = set()
BODY_ABORT = {"reason": "", "id": ""}
BODY_PROTOCOLS = ("openai-chat", "openai-responses", "anthropic", "gemini", "interactions")
INLINE_HINT = re.compile(r';base64,|"(?:inlineData|inline_data|input_audio)"\s*:|"type"\s*:\s*"base64"|"file_data"\s*:\s*"')
# Anthropic messages 里的 system 角色消息转成 <system-reminder> 用户消息（internal/api/anthropic.go），包装的字数计入运行日志的 input_text_chars
SYSTEM_REMINDER_CHARS = len("<system-reminder>\n") + len("\n</system-reminder>")


def as_list(value):
    return value if isinstance(value, list) else []


def as_dict(value):
    return value if isinstance(value, dict) else {}


def text_chars(value):
    # 与运行日志的 input_text_chars 同口径：按字符（Unicode 码点）计
    return len(value) if isinstance(value, str) else 0


def token_param(value):
    # max tokens 参数与运行日志的 max_output_tokens 同一写法：没有写（或写成 null）为“默认”
    if value is None:
        return "默认"
    if isinstance(value, float) and value.is_integer():
        value = int(value)
    return str(value)[:20]


OPENAI_TEXT_TYPES = ("text", "input_text", "output_text")


def block_texts(content, kinds):
    # 字符串，或内容块数组里类型为 kinds 的文字段
    if isinstance(content, str):
        return [content]
    return [as_dict(block).get("text") for block in as_list(content)
            if as_dict(block).get("type") in kinds and isinstance(as_dict(block).get("text"), str)]


def openai_content(content, shape):
    """OpenAI Chat 与 Responses 的 content（字符串或内容块数组）：返回文字字数；图片、其他内联数据（音频、视频、内联文件与文件地址）
    与文件引用（file_id）计入 shape"""
    if isinstance(content, str):
        return len(content)
    chars = 0
    for block in as_list(content):
        block = as_dict(block)
        kind = block.get("type")
        if kind in OPENAI_TEXT_TYPES:
            chars += text_chars(block.get("text"))
        elif kind in ("image_url", "input_image"):
            shape["images"] += 1
        elif kind in ("video_url", "input_video", "input_audio"):
            shape["inline"] += 1
        elif kind in ("file", "input_file"):
            item = as_dict(block.get("file"))
            shape["inline" if item.get("file_data") or block.get("file_data") or block.get("file_url") else "files"] += 1
    return chars


def gemini_content(content, shape):
    # Gemini 原生的一轮（parts 里的 text、inlineData/inline_data、fileData/file_data）：返回文字字数
    chars = 0
    for part in as_list(as_dict(content).get("parts")):
        part = as_dict(part)
        chars += text_chars(part.get("text"))
        blob = as_dict(part.get("inlineData") or part.get("inline_data"))
        if blob:
            mime = str(blob.get("mimeType") or blob.get("mime_type") or "").lower()
            shape["images" if mime.startswith("image/") else "inline"] += 1
        if part.get("fileData") or part.get("file_data"):
            shape["files"] += 1
    return chars


def anthropic_content(content, shape):
    # Anthropic 的 content（字符串或内容块数组）：返回文字字数；工具结果里的图片与文档会作为真正的媒体发送，同样计入（其中的文字不计）
    if isinstance(content, str):
        return len(content)
    chars = 0
    for block in as_list(content):
        block = as_dict(block)
        kind = block.get("type")
        if kind == "text":
            chars += text_chars(block.get("text"))
        elif kind == "image":
            shape["images"] += 1
        elif kind == "document":
            shape["files" if as_dict(block.get("source")).get("type") == "file" else "inline"] += 1
        elif kind == "tool_result":
            anthropic_content([b for b in as_list(block.get("content")) if as_dict(b).get("type") != "text"], shape)
    return chars


def interaction_block(block, shape):
    # Interactions 的内容块：text 返回字数；image、audio、video、document 带 data 为内联、带 uri 为文件引用
    block = as_dict(block)
    kind = block.get("type")
    if kind == "text":
        return text_chars(block.get("text"))
    if kind in ("image", "audio", "video", "document"):
        shape["files" if block.get("uri") and not block.get("data") else "images" if kind == "image" else "inline"] += 1
    return 0


def body_shape(protocol, body):
    """按接口解析请求体，只取形态：对话消息条数与字数（不含系统提示）、系统提示的字数（多条按换行拼接，与服务端相同）、Anthropic messages 里
    system 角色消息转成的轮次、最后一条用户消息的字数、图片、其他内联数据、文件引用、max tokens 参数（名与值）、是否续接上一轮与请求体里的
    模型名；与运行日志 request.started 同口径的消息条数与字数见 shape_counts。认不出时返回 None"""
    if not isinstance(body, dict):
        return None
    shape = {"protocol": protocol, "messages": 0, "chars": 0, "last_user": None, "images": 0, "inline": 0, "files": 0,
             "system": 0, "system_text": False, "system_turns": 0, "system_turn_chars": 0,
             "tokens_name": "", "tokens": "默认", "continuation": False, "model": ""}
    if isinstance(body.get("model"), str):
        shape["model"] = body["model"].strip()[:80]
    system = []  # 系统提示的各段文字（服务端去掉空段、按换行拼接成一段），只用来计数，函数返回前丢弃

    def add_system(text):
        if isinstance(text, str) and text:
            system.append(text)

    if protocol == "openai-chat":
        if not isinstance(body.get("messages"), list):
            return None
        for message in body["messages"]:
            message = as_dict(message)
            if message.get("role") in ("system", "developer"):
                add_system("".join(block_texts(message.get("content"), OPENAI_TEXT_TYPES)))
                continue
            chars = openai_content(message.get("content"), shape)
            shape["chars"] += chars
            shape["messages"] += 1
            if message.get("role") == "user":
                shape["last_user"] = chars
        # max_completion_tokens 写了就以它为准（与 chatRequest.generationConfig 相同）
        name = "max_completion_tokens" if body.get("max_completion_tokens") is not None else "max_tokens"
        shape["tokens_name"], shape["tokens"] = name, token_param(body.get(name))
    elif protocol == "openai-responses":
        items = body.get("input")
        shape["continuation"] = bool(body.get("previous_response_id"))
        add_system(body.get("instructions"))
        if isinstance(items, str):
            shape["messages"], shape["last_user"] = 1, len(items)
            shape["chars"] += len(items)
        elif isinstance(items, list):
            for item in items:
                item = as_dict(item)
                kind = item.get("type") or "message"
                if kind == "message":
                    if item.get("role") in ("system", "developer"):
                        add_system("".join(block_texts(item.get("content"), OPENAI_TEXT_TYPES)))
                        continue
                    chars = openai_content(item.get("content"), shape)
                    shape["chars"] += chars
                    shape["messages"] += 1
                    if item.get("role") == "user":
                        shape["last_user"] = chars
                elif kind in ("agent_message", "function_call", "function_call_output"):
                    shape["messages"] += 1
        elif not shape["continuation"]:
            return None
        shape["tokens_name"], shape["tokens"] = "max_output_tokens", token_param(body.get("max_output_tokens"))
    elif protocol == "anthropic":
        if not isinstance(body.get("messages"), list):
            return None
        # 顶层 system 与 system 角色消息的文字块按换行拼接（internal/api/anthropic.go 的 anthropicSystemText）
        add_system("\n".join(t for t in block_texts(body.get("system"), ("text",)) if t))
        for message in body["messages"]:
            message = as_dict(message)
            if message.get("role") == "system":
                # 转成一条 <system-reminder> 用户消息（文字为空时不发），Omni 视频模型去掉系统提示时整轮去掉
                text = "\n".join(t for t in block_texts(message.get("content"), ("text",)) if t)
                if text.strip():
                    shape["system_turns"] += 1
                    shape["system_turn_chars"] += len(text) + SYSTEM_REMINDER_CHARS
                continue
            chars = anthropic_content(message.get("content"), shape)
            shape["chars"] += chars
            shape["messages"] += 1
            if message.get("role") == "user":
                shape["last_user"] = chars
        shape["tokens_name"], shape["tokens"] = "max_tokens", token_param(body.get("max_tokens"))
    elif protocol == "gemini":
        request = body
        if not isinstance(request.get("contents"), list):
            # 批量写法 {"requests": [...]} 只支持一条，countTokens 的 generateContentRequest 包装
            if as_list(request.get("requests")):
                request = as_dict(request["requests"][0])
            elif isinstance(request.get("generateContentRequest"), dict):
                request = request["generateContentRequest"]
        instruction = request.get("systemInstruction") or request.get("system_instruction")
        contents = request.get("contents")
        if not isinstance(contents, list):
            if isinstance(request.get("content"), dict):
                contents = [request["content"]]
            elif isinstance(instruction, dict):
                # 只有 systemInstruction、没有 contents（new-api 把只有 system 的 OpenAI 对话转过来时就是这样）：服务端照常接受
                contents = []
            else:
                return None
        # 系统提示只取文字（各 part 直接拼接；带媒体时服务端返回 400）
        add_system("".join(t for t in (as_dict(part).get("text") for part in as_list(as_dict(instruction).get("parts"))) if isinstance(t, str)))
        for content in contents:
            if not as_list(as_dict(content).get("parts")):
                continue  # 没有 parts 的轮次服务端跳过
            chars = gemini_content(content, shape)
            shape["chars"] += chars
            shape["messages"] += 1
            if as_dict(content).get("role") in (None, "", "user"):
                shape["last_user"] = chars
        value = as_dict(request.get("generationConfig")).get("maxOutputTokens")
        if value is None:
            snake = as_dict(request.get("generation_config"))
            value = snake.get("max_output_tokens", snake.get("maxOutputTokens"))
        shape["tokens_name"], shape["tokens"] = "maxOutputTokens", token_param(value)
    elif protocol == "interactions":
        raw = body.get("input")
        shape["continuation"] = bool(body.get("previous_interaction_id"))
        add_system(body.get("system_instruction"))
        if isinstance(raw, str):
            if raw.strip():
                shape["messages"], shape["last_user"] = 1, len(raw)
                shape["chars"] += len(raw)
        elif isinstance(raw, (list, dict)):
            # 同一角色相邻的步骤并成一轮（与 appendInteractionParts 相同）
            turns = []
            for item in raw if isinstance(raw, list) else [raw]:
                item = as_dict(item)
                kind, chars = item.get("type"), 0
                if kind == "thought":
                    continue
                if kind in ("user_input", "model_output"):
                    role = "model" if kind == "model_output" else "user"
                    for block in as_list(item.get("content")):
                        chars += interaction_block(block, shape)
                elif kind in ("function_call", "function_result"):
                    role = "model" if kind == "function_call" else "tool"
                else:
                    role, chars = "user", interaction_block(item, shape)
                shape["chars"] += chars
                if turns and turns[-1][0] == role:
                    turns[-1][1] += chars
                else:
                    turns.append([role, chars])
            shape["messages"] = len(turns)
            users = [chars for role, chars in turns if role == "user"]
            shape["last_user"] = users[-1] if users else None
        elif not shape["continuation"] and not system:
            # 没有输入时只有系统指令的请求按用户消息生成（internal/api/interactions.go），两者都没有才是 400
            return None
        shape["tokens_name"], shape["tokens"] = "max_output_tokens", token_param(as_dict(body.get("generation_config")).get("max_output_tokens"))
    else:
        return None
    joined = "\n".join(system)
    shape["system"], shape["system_text"] = len(joined), bool(joined.strip())
    return shape


def shape_counts(shape, omni_drop):
    """与运行日志 request.started 同口径的 (消息条数, 文字字数, 最后一条用户消息的字数, 去掉的系统提示字数)。运行日志在协议转换、整理系统提示
    之后才记（internal/api/middleware.go 的 setGenerationInput）：系统提示不算消息、字数算在内；Anthropic 的 system 角色消息按用户消息算。
    omni_drop（发给能输出视频的 Omni 模型且 OMNI_IGNORE_SYSTEM_PROMPT 开启）时与 aistudio.DropClientSystemPrompt 相同：有对话消息时去掉
    系统提示与 system 角色消息转成的轮次；只有 system 角色消息时只去掉系统提示；只有系统提示时不去掉。之后只有系统提示、没有对话消息的请求
    按一条用户消息发送（internal/app/runtime.go 的 startGenerate），消息 1 条、字数为系统提示的字数"""
    messages, chars, last_user = shape["messages"], shape["chars"], shape["last_user"]
    turns, turn_chars, system = shape["system_turns"], shape["system_turn_chars"], shape["system"]
    if omni_drop and messages:
        return messages, chars, last_user, system + turn_chars
    if omni_drop and turns:
        return turns, turn_chars, last_user, system
    if not messages and not turns and shape["system_text"] and not shape["continuation"]:
        return 1, system, system, 0
    return messages + turns, chars + system + turn_chars, last_user, 0


def body_protocol(record):
    # 账本记录的 protocol（internal/app/request_logging.go 的 requestProtocol）；旧记录或认不出时按路径
    protocol = (record.get("protocol") or "").strip()
    if protocol in BODY_PROTOCOLS:
        return protocol
    path = base_path(record.get("path") or "")
    if path == "/v1/chat/completions":
        return "openai-chat"
    if path == "/v1/responses":
        return "openai-responses"
    if path.startswith("/v1/messages"):
        return "anthropic"
    if path in INTERACTIONS:
        return "interactions"
    if path.endswith((":generateContent", ":streamGenerateContent")):
        return "gemini"
    return protocol or "-"


def body_info(record):
    """读这个请求的正文（只读 GET /api/requests/{id}/body）并解析出形态，按请求 ID 只读、只解析一次：BODY_CACHE 只存解析结果与说明，
    不存正文。超过 BODY_FETCH_LIMIT 个不同的请求时不再读（记入 BODY_SKIPPED）；读正文超时或连接中断一次后之后的都不再读（记入
    BODY_ABORTED），这次失败也不把管理接口当成连不上（get 的 sticky 为假）。返回 shape（body_shape 的结果，读不到或解析不了时为 None）、
    truncated（正文被截断、解析不了时的同口径形态）、gap（正文用不上的原因：分类与说明）、short（结论里的简短说明）、note（第 4 段
    “账本补判”一行的正文部分，有 shape 时由 ledger_info 按条目写）与 size（请求体字节数）、cut（是否被截断）"""
    request_id = record.get("id") or ""
    if request_id in BODY_CACHE:
        return BODY_CACHE[request_id]
    info = {"shape": None, "truncated": None, "gap": None, "short": "", "note": "", "size": 0, "cut": False, "protocol": body_protocol(record)}
    if BODY_ABORT["reason"]:
        BODY_ABORTED.add(request_id)
        text = "前面读正文%s，之后的不再读" % BODY_ABORT["reason"]
        info["gap"], info["short"], info["note"] = ("bodyfail", "正文没有读（%s）" % text), "正文没有读", "正文没有读（%s）" % text
        return info
    if len(BODY_CACHE) >= BODY_FETCH_LIMIT:
        BODY_SKIPPED.add(request_id)
        text = "需要读正文的请求超过 %d 个，这一条没有读" % BODY_FETCH_LIMIT
        info["gap"], info["short"], info["note"] = ("bodyfail", text), "正文没有读", "正文没有读（需要读正文的请求超过 %d 个）" % BODY_FETCH_LIMIT
        return info
    data, status, detail = get("/api/requests/%s/body" % urllib.parse.quote(request_id, safe=""), sticky=False)
    if not isinstance(data, dict):
        if status == 404 and "正文不存在" in detail:
            # 正文不存在时服务返回 request_body_not_found（说明为“请求正文不存在”）：只保留最近 1000 条，读取时已被挤出
            info["gap"] = ("nobody", "账本里这条请求的正文读取时已不在（只保留最近 1000 条）")
            info["note"] = "正文无（读取时已不在：只保留最近 1000 条）"
        elif status == 0 and API_STATE["value"] == "ok":
            # 超时、连接中断或响应不是 JSON（服务多半在运行，只是这一个请求慢或出错）：不当成管理接口连不上；超时与连接中断后不再读之后的正文
            text = "读正文%s" % detail
            if not detail.startswith("响应不是 JSON"):
                BODY_ABORT["reason"], BODY_ABORT["id"] = detail, request_id
            info["gap"], info["note"] = ("bodyfail", text), text
        else:
            # 没有这个接口的旧版本服务同样是 404，说明不同
            text = "读不到正文（HTTP 404：旧版本服务没有正文接口）" if status == 404 else "正文读取失败（%s）" % (
                "HTTP %d%s" % (status, "：" + detail if detail else "") if status else detail or "没有响应")
            info["gap"], info["note"] = ("bodyfail", text), text
        info["short"] = "正文读不到"
        BODY_CACHE[request_id] = info
        return info
    raw = data.get("request") if isinstance(data.get("request"), str) else ""
    size = data.get("request_size") if isinstance(data.get("request_size"), int) else len(raw.encode("utf-8"))
    cut = size > BODY_LIMIT or size > len(raw.encode("utf-8"))
    info["size"], info["cut"] = size, cut
    try:
        body = json.loads(raw) if raw.strip() else None
    except ValueError:
        body = None
    shape = body_shape(info["protocol"], body) if body is not None else None
    if shape is None and cut:
        # 截断本身说明请求体超过 64 KiB，不是渠道测试的一条短消息；前 64 KiB 里能看到的内联数据照样算上
        inline = bool(INLINE_HINT.search(raw))
        info["truncated"] = {"_ledger": True, "_truncated": True, "_size": size, "input_media": 1 if inline else 0, "input_files": 0}
        info["note"] = "正文被截断（请求体 %d 字节，账本只存前 64 KiB，解析不了；前 64 KiB 里%s内联数据）" % (size, "有" if inline else "没有")
    elif shape is None:
        text = "正文为空" if not raw.strip() else "正文不是 JSON（请求体 %d 字节）" % size if body is None else "认不出这个接口（%s）的请求体" % info["protocol"]
        info["gap"], info["short"], info["note"] = ("bodyfail", text), "正文解析不了", "正文有但解析不了：" + text
    info["shape"] = shape
    BODY_CACHE[request_id] = info
    return info


# 账本里额度用完改用的备注（app/model_fallback.go 的 modelFallbackSwitchNote：“模型 <源> 额度用完（<原因>），按 MODEL_FALLBACKS 改用 <目标>”，
# 后面可能还有“ | 历史里的 N 个思考签名…”）
SWITCH_NOTE = re.compile(r"模型 (\S+) 额度用完.*?按 MODEL_FALLBACKS 改用 ([^\s|，（]+)")


def text_value(value):
    return value.strip() if isinstance(value, str) else ""


def ledger_switch(record):
    """账本记录里看得到的额度用完改用（MODEL_FALLBACKS，e2e0a20 起；见 app/model_fallback.go 的 switchModelFallback、endModelFallback）：
    第一次改用时先给已记下的源模型尝试补记源模型（TagAccessLogAttempts），再追加一条账户与模型都为空的改用备注，之后目标模型的尝试在
    记下时就写目标模型；目标都失败时记录的模型恢复为源模型（RestoreAccessLogSource）。返回 (源模型, 目标模型集合（小写，按目录换成正式
    ID）, 依次试过的模型)：源模型取备注里写的，备注认不出时取第一条备注之前的尝试所记的模型（源模型的候选全部冷却、一次也没有发出时
    备注之前没有尝试）；目标取备注里写的与第一条备注之后的尝试所记的模型。没有改用过的记录（没有目标）返回 ("", set(), [])"""
    source, tagged, targets, order, seen = "", "", set(), [], False

    def add(name):
        if name and (not order or order[-1].lower() != name.lower()):
            order.append(name)

    for attempt in as_list(record.get("attempts")):
        if not isinstance(attempt, dict):
            continue
        model = fallback_name(text_value(attempt.get("model")))
        if model:
            if seen:
                targets.add(canonical(model))
            else:
                tagged = tagged or model
            add(model)
            continue
        if text_value(attempt.get("account")):
            continue
        # 账户与模型都为空：改用备注（尝试只在改用时才带模型，改用之前没有模型的尝试在第一次改用时都补记了源模型）
        seen = True
        match = SWITCH_NOTE.search(text_value(attempt.get("error")))
        if match:
            source = source or fallback_name(match.group(1))
            targets.add(canonical(match.group(2)))
            if not order:
                add(fallback_name(match.group(1)))
            add(fallback_name(match.group(2)))
    if not targets:
        return "", set(), []
    return source or tagged, targets, order


# ledger_requested 的改用证据：来源 → 第 7 段结论末尾写的依据
SWITCH_BASIS = {"finish": "运行日志的结束行", "attempts": "账本记录的上游尝试", "record": "账本记录的模型（改用的目标都失败时恢复为源模型）"}


def ledger_requested(entry, shape):
    """账本里看得到的客户端请求名：请求体里的 model，Gemini 原生为路径里的模型名（/v1beta/models/<请求名>:generateContent）。只在能确定时
    返回 (请求名, 来源)，交给 judge 判别名与改用（与 request.started 的模型名同口径：目录别名落到这个模型，或按 MODEL_FALLBACKS 改用到这个
    模型）；带思考、视频等后缀的其他写法不返回，免得把后缀判成“模型重定向或改用（未确认）”。
    请求体与路径都确定不了时，再看现成的额度用完改用证据，它们直接说明发生过改用（judge 不再按现在的 MODEL_FALLBACK_SCOPE 判断路径）：
      finish：运行日志结束行的 fallback_from（请求以改用后的目标结束），与登记的模型不同时就是客户端请求的源模型；
      attempts：账本记录的上游尝试（见 ledger_switch）说明登记的模型是改用的目标，源模型与它不同时；
      record：登记的模型是改用的目标、备注与尝试都看不出源模型时，记录的模型（目标都失败时恢复为源模型）与它不同、且按 MODEL_FALLBACKS
              改用到它时。
    默认的 MODEL_FALLBACKS 是双向的（argon=carbon、carbon=argon）：登记的模型是改用的源模型时（这个账户在源模型上 403，之后额度用完改用
    到别的模型），客户端请求的就是登记的模型，这里不返回，按形态照常判断"""
    record = entry["record"] or {}
    names = [(shape["model"], "body")] if shape and shape.get("model") else []
    match = re.search(r"/models/([^/:]+):", record.get("path") or "")
    if match:
        names.append((urllib.parse.unquote(match.group(1)), "path"))
    target = entry["model"].lower()
    for raw, where in names:
        name = fallback_name(raw)
        if not name or name.lower() == target:
            continue
        if canonical(name) == target or fallback_applies(record.get("path") or "") and fallback_pair(name, entry["model"]):
            return name, where
    name = fallback_name(text_value((entry.get("finish") or {}).get("fallback_from")))
    if name and name.lower() != target and canonical(name) != target:
        return name, "finish"
    source, targets, _ = ledger_switch(record)
    if target in targets or canonical(target) in targets:
        if source and source.lower() != target and canonical(source) != target:
            return source, "attempts"
        name = fallback_name(text_value(record.get("model")))
        if not source and name and name.lower() != target and fallback_pair(name, entry["model"]):
            return name, "record"
    return "", ""


def needs_ledger(entry):
    """是否要用账本补判：账本里对上了记录，登记与运行日志都没有消息条数，且第 7 段不会先按其他规则下结论（早于 93c16fa 的普通 403 判为
    旧记录、直连 interactions、请求名与登记的模型不同）。已有请求形态的条目不读正文，结论不变"""
    last, start, record = entry["last_request"] or {}, entry.get("start") or {}, entry["record"]
    if not record or last.get("messages") or start.get("input_messages") or last.get("requested_model"):
        return False
    if base_path(last.get("path") or record.get("path") or "") in INTERACTIONS:
        return False
    return entry["verify"] or not (entry["last"] and entry["last"] < FIX_AT)


def final_text(record):
    # 账本记录的最终结果：成功（completed、tool_calls、limited）、上游终止生成（blocked：finishReason 不是 stop，HTTP 200）、取消、失败
    state = record.get("state")
    return "成功" if state in SUCCESS_STATES else "上游终止生成" if state == "blocked" else "取消" if state == "cancelled" else "失败"


def record_tokens(record):
    # 账本的输入、回复 tokens（不是整数时按 0）
    values = []
    for key in ("input_tokens", "reply_tokens"):
        try:
            values.append(max(0, int(record.get(key) or 0)))
        except (TypeError, ValueError):
            values.append(0)
    return values[0], values[1]


def ledger_info(entry):
    """账本补判用到的信息（见 body_info）：记录没有正文（has_body 为假）时不读正文接口。有正文形态时按这个条目换成与 request.started 同口径的
    start（发给 Omni 视频模型且 OMNI_IGNORE_SYSTEM_PROMPT 开启时不计被去掉的系统提示，只有系统提示时按 1 条用户消息，见 shape_counts）；
    请求名能确定是目录别名或 MODEL_FALLBACKS 的源模型时带上（见 ledger_requested）。note 为第 4 段“账本补判”一行：正文部分、账本的输入、
    回复 tokens 与最终结果"""
    record = entry["record"]
    if record.get("has_body"):
        body = body_info(record)
    else:
        reason = ".env 里 REQUEST_BODY_LOG=false，不记正文" if REQUEST_BODY_OFF else "正文只保留最近 1000 条，或当时 REQUEST_BODY_LOG 关闭"
        body = {"shape": None, "truncated": None, "gap": ("nobody", "账本里没有这条请求的正文（%s）" % reason), "short": "账本里没有请求正文",
                "note": "正文无（%s）" % reason}
    info = {"start": None, "gap": body["gap"], "short": body["short"], "note": body["note"]}
    shape = body.get("shape")
    info["requested"] = ledger_requested(entry, shape)
    if shape:
        dropping = OMNI_SYSTEM_DROP and omni_video(entry["model"])
        messages, chars, last_user, dropped = shape_counts(shape, dropping)
        info["start"] = {"_ledger": True, "_shape": shape, "_continuation": shape["continuation"],
                         "input_messages": messages, "input_text_chars": chars,
                         "input_media": shape["images"] + shape["inline"], "input_files": shape["files"],
                         "parameters": {"max_output_tokens": shape["tokens"]}}
        system = shape["system"] + shape["system_turn_chars"]
        notes = []
        if dropped:
            notes.append("客户端的系统提示 %d 字发给 Omni 视频模型时服务端去掉，不计入，与运行日志同口径" % dropped)
        elif messages and not shape["messages"] and not shape["system_turns"]:
            notes.append("只有系统提示，服务端把它作为 1 条用户消息发送")
        elif system:
            notes.append("含系统提示 %d 字" % system)
        if last_user is not None:
            notes.append("最后一条用户消息 %d 字" % last_user)
        info["note"] = "正文有（%s，请求体 %d 字节%s）：%d 条消息、文字 %d 字%s、图片 %d、其他内联数据 %d、文件 %d、%s %s%s%s" % (
            shape["protocol"], body["size"], "，被截断但前 64 KiB 解析得了" if body["cut"] else "", messages, chars,
            "（%s）" % "；".join(notes) if notes else "",
            shape["images"], shape["inline"], shape["files"], shape["tokens_name"] or "max tokens", shape["tokens"],
            "、续接上一轮" if shape["continuation"] else "", "、请求体里的模型名 %s" % shape["model"] if shape["model"] else "")
    elif body.get("truncated"):
        info["start"] = dict(body["truncated"])
    requested, where = info["requested"]
    if requested and info["start"]:
        info["start"]["model"] = requested
        if where in SWITCH_BASIS:
            info["start"]["_switched"] = True
    if requested and where == "path":
        info["note"] += "；路径里的请求名 %s" % requested
    source, _, order = ledger_switch(record)
    if order:
        # 改用备注认不出、改用前又没有源模型的尝试时看不出源模型，写成“? → 目标”
        info["note"] += "；上游尝试的模型：%s%s（MODEL_FALLBACKS 额度用完改用）" % ("" if source else "? → ", " → ".join(order))
    tokens, reply = record_tokens(record)
    info["note"] += " · 按账本：输入 %d / 回复 %d tokens（最终%s，HTTP %s %s）" % (
        tokens, reply, final_text(record), record.get("status") or "-", record.get("state") or "-")
    return info


def id_kind(request_id):
    # 请求 ID 前缀对应的接口类型（见 ID_PREFIXES）；认不出时为空
    for prefix, label in ID_PREFIXES:
        if (request_id or "").startswith(prefix):
            return prefix, label
    return "", ""


def id_only(entry):
    # 账本里没有记录、登记里也没有接口与请求形态，只知道请求 ID：按 ID 前缀补一句接口类型
    last = entry["last_request"] or {}
    return entry["record"] is None and bool(entry.get("request_id")) and not last.get("path") and not last.get("messages") and bool(
        id_kind(entry["request_id"])[1])


def test_shaped(start, path):
    # 请求形态与渠道测试相符（与 judge 的渠道测试判定同一套：1 条不超过 8 字的消息、没有媒体与文件、max_tokens 为 16/3000/不带、渠道测试会用的接口）
    if not start or start.get("_truncated") or start.get("_continuation"):
        return False
    chars = start.get("input_text_chars")
    tokens = str((start.get("parameters") or {}).get("max_output_tokens") or "")
    return (start.get("input_messages") == 1 and not start.get("input_media") and not start.get("input_files") and isinstance(chars, int)
            and chars <= 8 and bool(TEST_TOKENS.match(tokens or "默认")) and path.endswith(TEST_PATHS))


def token_calibration():
    """渠道测试的输入 tokens 校准样本（见 LEDGER_TEST_TOKENS 的说明）：请求形态已确认是渠道测试（运行日志的开始行，或账本正文得出的同口径形态）、
    最终成功、账本有输入 tokens 的请求，按最终的模型（小写）取输入 tokens，每个请求只算一次；超过 8 加随机后缀最大贡献的不算"""
    samples = collections.defaultdict(dict)
    for entry in entries:
        record = entry["record"]
        if not record or record.get("state") not in SUCCESS_STATES:
            continue
        tokens = record_tokens(record)[0]
        start = (entry.get("ledger") or {}).get("start") or entry.get("start") or {}
        path = base_path((entry["last_request"] or {}).get("path") or record.get("path") or "")
        if 0 < tokens <= LEDGER_TEST_TOKENS + NONCE_TOKENS_MAX and test_shaped(start, path):
            samples[(record.get("model") or entry["model"]).lower()][record.get("id")] = tokens
    return {model: sorted(values.values()) for model, values in samples.items()}


def token_limits(record, entry):
    """(校准样本, 渠道测试上限, 下游门槛)：有同一模型的校准样本时上限为样本最大值加 LEDGER_TEST_TOKENS、门槛为最大值加 LEDGER_DOWNSTREAM_TOKENS；
    没有样本时上限为 LEDGER_TEST_TOKENS，门槛为随机后缀可能的最大贡献（REPEAT_PROMPT_NONCE 关闭时为 0）加 LEDGER_DOWNSTREAM_TOKENS"""
    samples = CALIBRATION.get((record.get("model") or entry["model"]).lower()) or []
    if samples:
        return samples, samples[-1] + LEDGER_TEST_TOKENS, samples[-1] + LEDGER_DOWNSTREAM_TOKENS
    return samples, LEDGER_TEST_TOKENS, (NONCE_TOKENS_MAX if NONCE_ON else 0) + LEDGER_DOWNSTREAM_TOKENS


def samples_text(samples):
    return "%d 个请求，输入 %s tokens" % (len(samples), "%d" % samples[0] if samples[0] == samples[-1] else "%d～%d" % (samples[0], samples[-1]))


print("\n== 4. 每个登记账户最近一次 403 对应的请求与经过（运行日志：%s）" % ("、".join(log_files) or "没有"))
for entry in entries:
    head = "%s · %s：" % (mask(entry["label"]), entry["model"])
    record, last = entry["record"], entry["last_request"] or {}
    request_id = (record or {}).get("id") or last.get("id") or ""
    start = starts.get(request_id) or {}
    entry["start"], entry["finish"], entry["request_id"] = start, finishes.get(request_id) or {}, request_id
    # 登记带请求 ID、运行日志覆盖这次 403 却没有这个请求：账本读不到时据此判断请求不是经过本实例的
    entry["log_missing"] = bool(last.get("id")) and not start and not entry["finish"] and log_covers(entry)
    # 登记与运行日志都没有请求形态、账本里对上了记录时用账本补判（需要验证账号的账户同样要，第 7 段用）
    entry["ledger"] = ledger_info(entry) if needs_ledger(entry) else None
    # 需要验证账号的账户的登记照样对照（第 7 段要判断请求来源），默认不逐条输出，循环后只给个数与时间范围
    if not detailed(entry):
        continue
    if not record and not last:
        if ledger == "ok":
            print(head + "账本里没有对应的请求（最近一次 403 在 %d 天前）%s%s" % (
                entry["age"], "，早于账本保留期" if entry["age"] > 90 else "", entry["how"] or ""))
        else:
            print(head + "登记里没有请求来源（旧版本的记录），账本也读不到，无法对照")
        continue
    print(head + entry["how"])
    if record:
        chain = [("%s %s" % (mask(a.get("account")), "403" if "403" in (a.get("error") or "") else "失败")) for a in record.get("attempts") or []]
        chain.append("%s %s" % (mask(record.get("account")) or "-", "成功" if record.get("state") in ("completed", "tool_calls", "limited") else record.get("state")))
        print("    %s | %s | %s | HTTP %s %s | 正文记录%s" % (
            show(parse_time(record.get("time"))), record.get("id"), record.get("path"), record.get("status"), record.get("state"),
            "有" if record.get("has_body") else "无"))
        print("    经过：" + " → ".join(chain))
    elif ledger == "ok":
        if entry["age"] >= DAYS:
            print("    账本里没有这个请求 ID（最近一次 403 在 %d 天前，超过查询的 %d 天）" % (entry["age"], DAYS))
        elif start or entry["finish"]:
            print("    %s；本实例运行日志里有它，请求经过本实例" % LEDGER_PENDING)
        else:
            print("    账本里没有这个请求 ID（最近一次 403 在 %d 天前、在查询范围内）：不在本实例的账本里，多半是共用账户目录的其他实例"
                  "写的登记，或账本被清理过" % entry["age"])
    shape = []
    if last:
        shape.append("登记：%s · 请求名 %s · %d 条消息 · 图片 %d · 文件 %d" % (
            last.get("path") or "-", last.get("requested_model") or entry["model"], last.get("messages") or 0,
            last.get("images") or 0, last.get("files") or 0))
    if start:
        params = start.get("parameters") or {}
        shape.append("运行日志：模型名 %s | 消息 %s 条 | 文字 %s 字 | 媒体 %s | 文件 %s | max_output_tokens %s" % (
            start.get("model", "-"), start.get("input_messages", 0), start.get("input_text_chars", 0), start.get("input_media", 0),
            start.get("input_files", 0), params.get("max_output_tokens", "-")))
    elif entry["finish"]:
        shape.append("运行日志：只有这个请求的结束行（HTTP %s %s），开始行不在日志里，看不到消息数与 max_output_tokens" % (
            entry["finish"].get("status", "-"), entry["finish"].get("state", "-")))
    elif request_id:
        # 账本补判的条目与第 7 段同一口径（按账本算出的请求开始时间，见 log_gap）；其余照旧按这次 403 的时间
        shape.append(log_gap(entry)[1] if entry["ledger"] else log_absence(entry))
    if entry["finish"].get("fallback_from"):
        shape.append("运行日志：request.finished 带 fallback_from=%s（额度用完改用）" % entry["finish"]["fallback_from"])
    if entry["ledger"]:
        shape.append("账本补判：" + entry["ledger"]["note"])
    if id_only(entry):
        shape.append("请求 ID 前缀 %s：%s（只按 ID 推断接口类型）" % id_kind(request_id))
    for item in shape:
        print("    " + item)
if VERIFY_ENTRIES and not VERIFY_LIST:
    for group, title in ((VERIFY_ACCOUNT_ENTRIES, "需要验证账号（整户）"), (VERIFY_MODEL_ENTRIES, "单个模型需要验证")):
        if group:
            print("%s的账户的登记不逐条列出：%s；请求来源见第 7 段的汇总（VERIFY=list 逐条列出）" % (title, verify_brief(group)))
if BODY_SKIPPED:
    print("⚠ 需要读请求正文的请求有 %d 个，超过上限 %d 个：其中 %d 个没有读正文，按账本的 tokens 与其他信息判断" % (
        len(BODY_CACHE) + len(BODY_SKIPPED) + len(BODY_ABORTED), BODY_FETCH_LIMIT, len(BODY_SKIPPED)))
if BODY_ABORTED:
    print("⚠ 读请求 %s 的正文%s（管理接口没有当成连不上）：之后 %d 个请求的正文不再读，按账本的 tokens 与其他信息判断" % (
        BODY_ABORT["id"], BODY_ABORT["reason"], len(BODY_ABORTED)))
# 账本补判按输入 tokens 判断时的渠道测试校准（见 token_calibration），有用账本补判的条目时写明
CALIBRATION = token_calibration()
if any(entry["ledger"] for entry in entries):
    print("账本补判按输入 tokens 判断时：随机后缀（REPEAT_PROMPT_NONCE）%s；渠道测试的校准样本（形态已确认是渠道测试、最终成功）%s" % (
        "开启，输入 tokens 含后缀" if NONCE_ON else "关闭", "：" + "；".join("%s %s" % (model, samples_text(values)) for model, values in sorted(
            CALIBRATION.items())) if CALIBRATION else "没有：渠道测试只认输入不超过 %d 的，不少于 %d 才判为下游" % (
            LEDGER_TEST_TOKENS, (NONCE_TOKENS_MAX if NONCE_ON else 0) + LEDGER_DOWNSTREAM_TOKENS)))

# ---------- 5. 目录别名 ----------
print("\n== 5. 目录里的别名（别名本身不是目录 ID 时，请求这个别名会落到这个模型、登记在它名下）")
if not CATALOG_OK:
    print("⚠ 读不到模型目录" + ("：" + API_STATE["reason"] if API_STATE["reason"] else ""))
for model in CATALOG:
    aliases = (model.get("capability_options") or {}).get("aliases") or []
    if model.get("id") in models_of_interest:
        kind = "Interaction 模型（Omni）" if (model.get("capabilities") or {}).get("interactions_api") else "模型"
        print("%s（%s）| 别名 %s" % (model.get("id"), kind, "、".join(
            "%s（%s）" % (alias, "也是独立目录 ID" if alias in CATALOG_IDS else "不是目录 ID：请求它会落到这个模型") for alias in aliases) or "无"))
if CATALOG and models_of_interest and not any(m.get("id") in models_of_interest for m in CATALOG):
    print("目录里没有 %s（账户目录已不列出它）" % "、".join(models_of_interest))
elif not models_of_interest:
    print("没有要看的模型")
targets = sorted({t for ts in FALLBACKS.values() for t in ts if any(canonical(t) == m.lower() or t == m.lower() for m in models_of_interest)})
print("MODEL_FALLBACKS（%s，生效范围 %s）：%s" % (
    ".env" if FALLBACKS_SET else "没有写，按默认值", FALLBACK_SCOPE,
    "；".join("%s=%s" % (s, "|".join(t)) for s, t in sorted(FALLBACKS.items())) or "无（关闭）"))
if targets:
    print("  额度用完时会改用到 %s：这个模型的 403 可能来自客户端请求的源模型" % "、".join(targets))

# ---------- 6. 403 冷却与暂停行 ----------
print("\n== 6. 运行日志里的 403 冷却与需要验证账号的暂停（没有请求 ID，按时间与账户对照；时间为本机时区）")
# 需要验证账号的账户的行（暂停、解除，以及它们的 403 冷却行）默认只给行数、账户数与时间范围，VERIFY=list 时与其他行一起逐行列出：
# 运行时遇到需要验证账号的 403，登记里只有这一个模型时只写一行单个模型暂停（账号冷却 | 类型=单个模型需要验证；没有写入时为
# “账号冷却未写入 | 类型=单个模型需要验证”，另计、不算暂停，与 log_paused 同一口径），两个及以上模型时先写一行这个模型的 403 冷却
# （账号冷却 | 类型=无权限）、再写整户暂停（账号暂停调度）；上千个账户各有这些行时，最近 20 行全是它们，看不到其他账户这个模型的
# 403 冷却。需要验证账号的账户与第 2 段同一口径（登记里有账户级 403、或需要验证账号的暂停仍生效，读不到冷却列表时按运行日志近似）
log_rows = []  # (时间, 账户, 原文, 行的种类：cool 403 冷却、model 单个模型暂停、unwritten 单个模型暂停未写入、pause 整户暂停、lift 解除)
for name in log_files:
    with open(os.path.join(APP, "logs", name), encoding="utf-8", errors="replace") as handle:
        for line in handle:
            if "类型=无权限" not in line and "账号暂停调度" not in line and "解除需要验证账号的整户暂停" not in line and \
                    "类型=单个模型需要验证" not in line and "解除单个模型需要验证的暂停" not in line:
                continue
            try:
                item = json.loads(line)
            except ValueError:
                continue
            message = item.get("msg") or ""
            scoped = "类型=无权限" in message or "类型=单个模型需要验证" in message
            if scoped and not ALL and not re.search(r"范围=(build:)?%s( |$)" % re.escape(MODEL), message):
                continue
            if not wanted_account(item.get("source"), item.get("source")):
                continue
            # cool 403 冷却、model 单个模型需要验证的暂停（只认以“账号冷却 | 类型=单个模型需要验证”开头的写入行）、unwritten 没有写入的
            # （账号冷却未写入 | 类型=单个模型需要验证）、pause 整户暂停、lift 解除（两种）
            kind = "cool" if "类型=无权限" in message else \
                "model" if message.startswith("账号冷却 | 类型=单个模型需要验证") else \
                "unwritten" if "类型=单个模型需要验证" in message else \
                "pause" if "账号暂停调度" in message else "lift"
            log_rows.append((item.get("time"), item.get("source") or "", message, kind))
log_verify = set(VERIFY_MODELS) | PAUSED | set(MODEL_VERIFY)
# 只有“账号冷却未写入”、没有一行写入成功的单个模型需要验证的账户（第 7 段据此写“暂停没有写入”，见 model_pause_unwritten）
MODEL_UNWRITTEN = {account.lower() for _, account, _, kind in log_rows if kind == "unwritten"} - \
    {account.lower() for _, account, _, kind in log_rows if kind == "model"}
rows = []
verify_rows, verify_sources, verify_times = collections.Counter(), set(), []
for moment, account, message, kind in log_rows:
    if not VERIFY_LIST and (kind != "cool" or account.lower() in log_verify):
        verify_rows[kind] += 1
        verify_sources.add(account.lower())
        moment = parse_time(moment)
        if moment:
            verify_times.append(moment)
        continue
    rows.append("%s | %s | %s" % (show(parse_time(moment)), mask(account), mask(message)[:240]))
if verify_rows:
    print("需要验证账号的账户：%d 个账户、%d 行（%s）%s；不逐行列出（VERIFY=list 逐行列出）" % (
        len(verify_sources), sum(verify_rows.values()), "、".join("%s %d 行" % (label, verify_rows[kind]) for kind, label in (
            ("pause", "整户暂停"), ("model", "单个模型暂停"), ("unwritten", "单个模型暂停未写入"), ("lift", "解除"),
            ("cool", "403 冷却")) if verify_rows[kind]),
        "，%s 至 %s" % (show(min(verify_times)), show(max(verify_times))) if verify_times else ""))
print("\n".join(rows[-20:]) if rows else "运行日志里没有%s" % ("其他账户的 403 冷却行" if verify_rows else "这些行"))

# ---------- 7. 结论 ----------
ADVICE = {
    "旧记录": "点 403 列表这一行的“清除”（不要“一键删除”）",
    "渠道测试": "把 new-api 渠道的测试模型改成纯文字模型；不打算提供这个模型就从渠道模型列表与模型重定向里去掉、关掉自动同步上游模型",
    "下游": "按时间到 new-api 查是谁在用；不打算提供这个模型就从渠道里去掉",
    "下游带图": "多半不是账户没有权限：点这一行的“清除”；反复出现时看运行日志的出口被拒判定，要查是谁在用就按时间到 new-api 查",
    "别名": "改客户端配置或 new-api 的模型重定向（本服务收到的是重定向之后的名字），确认是否有意用到这个模型",
    "改用": "本服务的 MODEL_FALLBACKS 改用，不是 new-api 重定向：目标模型对这个账户无权限，可以清除这一行，或在管理页设置里调整 MODEL_FALLBACKS",
    "未确认": "对照第 4 段的运行日志与第 5 段的目录别名、MODEL_FALLBACKS；本服务收到的已是 new-api 重定向之后的名字，请求名与登记的模型不同"
              "多半是目录别名（目录可能已更新）或 MODEL_FALLBACKS",
    "直连": "new-api 不转发这个接口：查谁拿到了本服务的 API 密钥",
    # 需要验证账号是账户级的：整户暂停调度，不是只冷却这个模型
    "需要验证": "整户已暂停调度（不占 Worker，最长 30 天）：到官网验证后在管理页点“验证”；不要这些号就删除；请求来源见结论",
    "需要验证（暂停已到期）": "整户暂停已到期、重新参与调度，仍未验证的下一次请求会再返回 403 并重新暂停：到官网验证后在管理页点“验证”；"
                       "不要这些号就删除；请求来源见结论",
    # 只在一个模型上要求验证：只暂停这个模型，账户其他模型照常调度
    "单个模型需要验证": "只暂停这个模型（最长 30 天），其他模型照常调度、Worker 不释放：到官网验证后在管理页点“验证”，或在“403 单个模型需要验证”里点“清除”；"
                  "不要这个号就删除；请求来源见结论",
    "单个模型需要验证（暂停未生效）": "这个模型的暂停未生效（已到期或没有写入）、照常调度，仍未验证的下一次请求会再返回 403 并重新暂停："
                         "到官网验证后在管理页点“验证”；请求来源见结论",
    "账本没有": "看第 1 段有没有多个实例共用账户目录、账户目录是不是从别处复制或恢复来的；确认后点这一行的“清除”",
    "无法对照": "按最近一次 403 的时间（或账本里这个请求的时间）到 new-api 查",
    # 账本里有这个请求，登记、运行日志与账本都看不出形态（见 ledger_judge）
    "无法判断形态": "按这个请求的时间与接口到 new-api 的使用日志里查这个模型（看是哪个令牌、哪个用户）；看第 3 段这个模型按接口、按天的"
              "请求数与成功数，判断现在还有没有这类请求",
}

# 看不到请求形态的原因（ledger_judge 记在 entry["gaps"]），第 7 段需要验证账号的汇总按它说明主要原因
GAP_LABELS = collections.OrderedDict((
    ("reg", "这些登记早于记录请求形态的版本 f06ddf9"), ("rotated", "运行日志已轮转"), ("loggap", "运行日志覆盖这段时间却没有开始行"),
    ("nolog", "没有运行日志"), ("nobody", "账本里也没有正文"), ("bodyfail", "正文读不到或解析不了"), ("noshape", "正文里没有对话消息"),
    ("failed", "请求最终没有成功、账本里没有 tokens"),
    ("tokens", "账本的输入 tokens 判断不了（为 0、介于渠道测试与下游之间，或请求没有成功）")))


def ledger_shape_text(start):
    # 按账本正文得出的形态（见 ledger_info）
    if start.get("_truncated"):
        return "按账本里的请求正文：请求体 %d 字节，超过 64 KiB 被截断%s" % (
            start["_size"], "，前 64 KiB 里有内联数据" if start.get("input_media") else "")
    shape = start["_shape"]
    return "按账本里的请求正文：%d 条消息、图片 %d、其他内联数据 %d、文件 %d%s" % (
        start["input_messages"], shape["images"], shape["inline"], shape["files"], "、续接上一轮" if shape["continuation"] else "")


def shape_text(last, start):
    if start.get("_ledger"):
        return ledger_shape_text(start)
    if last:
        text = "%d 条消息、图片 %d、文件 %d" % (last.get("messages") or 0, last.get("images") or 0, last.get("files") or 0)
        if start:
            # 登记的图片只数 image/* 内联数据，内联的视频、音频只算在运行日志的媒体里
            text += "，运行日志：媒体 %s、文件 %s" % (start.get("input_media", 0), start.get("input_files", 0))
        return text
    if start:
        return "%s 条消息、媒体 %s、文件 %s" % (start.get("input_messages", 0), start.get("input_media", 0), start.get("input_files", 0))
    return ""


def shared_note():
    return "；第 1 段发现 %d 个实例共用账户目录" % SHARED if SHARED >= 2 else ""


def judge(entry, start):
    """请求来源的结论与分类。账户级 403（需要验证账号）的那条登记同样判断来源，但跳过三条“多半与账户权限无关”的判定（早于 93c16fa 的
    旧记录、升级前带图 Omni 的误登记、下游带图调用 Omni）：上游原文已明确是账户需要验证，这些判定针对的是“The caller does not have
    permission”被误登记的情形，套用会得出“账户本身多半正常”的错误结论；它们按请求形态照常归入渠道测试、下游等。需要验证账号的账户在
    其他模型上的普通 403 照常套用。start 为运行日志的 request.started，或账本补判按正文得出的同口径形态（带 _ledger，见 ledger_info）；
    账本里有这个请求、却看不出形态时返回 (None, None)，由 ledger_judge 补判"""
    last, record = entry["last_request"] or {}, entry["record"]
    from_ledger = bool(start.get("_ledger"))
    account_level = entry["verify"]
    if not account_level and entry["last"] and entry["last"] < FIX_AT:
        return "多半是旧记录（最近一次 403 早于 93c16fa 修复带图 Omni 403 误登记的时间 %s）" % show(FIX_AT), "旧记录"
    # 账本里没有这个请求 ID，本实例运行日志里也没有它的开始行与结束行时才判为其他实例；运行日志里有它时照常判断形态，结论后附 LEDGER_PENDING
    if entry["id_missing"] and not start and not entry.get("finish"):
        shape = shape_text(last, start)
        return "账本里没有对应请求（请求 ID 不在本实例的账本里：多半是共用账户目录的其他实例或复制来的目录%s）%s" % (
            shared_note(), "；登记的请求形态：%s %s" % (last.get("path") or "-", shape) if shape else ""), "账本没有"
    if ledger != "ok" and entry["log_missing"]:
        shape = shape_text(last, start)
        return "运行日志里没有对应请求（请求 ID 不在本实例的运行日志里，用量账本也读不到：多半是共用账户目录的其他实例或复制来的目录%s）%s" % (
            shared_note(), "；登记的请求形态：%s %s" % (last.get("path") or "-", shape) if shape else ""), "账本没有"
    path = base_path(last.get("path") or (record or {}).get("path") or "")
    raw_path = last.get("path") or (record or {}).get("path") or ""
    # 按账本正文补判时登记里没有图片数（或登记的 last_request 里没有消息数），看正文得出的媒体数
    media = (last.get("images") or 0) if last and not from_ledger else (start.get("input_media") or 0)
    code7 = "协议错误码 7" in entry["message"]
    if not account_level and interaction_model(entry["model"]) and media > 0 and (not last or FIX_COUNT == "0"):
        text = "多半是升级到含 93c16fa 的版本之前带图请求的误登记（%s）：新版本不再把带图、协议错误码 7 的 Omni 403 登记为账户无权限，账户本身多半正常" % (
            shape_text(last, start) or "带图")
        if FIX_COUNT == "0":
            text += "；正在运行的程序早于 93c16fa，先升级"
        elif PROC_START and entry["last"] and entry["last"] < PROC_START:
            text += "；最近一次 403 早于本目录进程启动（%s，升级不晚于这个时间），对照第 1 段的升级与重启记录" % show(PROC_START)
        elif not code7:
            text += "；原文不带协议错误码 7：若这次在升级之后，则是错误体解析不出协议错误码的 403（出口或 GFE 的 HTML 错误页），同样多半与账户权限无关"
        else:
            text += "；对照第 1 段的升级与重启记录确认这次 403 在升级之前"
        return text, "旧记录"
    if path in INTERACTIONS:
        return "直连 interactions（%s）" % path, "直连"
    requested = last.get("requested_model") or (start.get("model") if start.get("model") not in (None, "", entry["model"]) else "")
    if requested and requested.lower() != entry["model"].lower():
        finish = entry.get("finish") or {}
        # 账本补判看到了改用的证据（运行日志结束行的 fallback_from、账本记录的上游尝试，见 ledger_requested）时不再按现在的生效范围判断
        if start.get("_switched") or (finish.get("fallback_from") or "").lower() == requested.lower() or \
                fallback_applies(raw_path) and fallback_pair(requested, entry["model"]):
            return "额度用完改用（MODEL_FALLBACKS：客户端请求 %s，改用 %s 后 403）" % (requested, entry["model"]), "改用"
        if canonical(requested) == entry["model"].lower():
            return "别名（客户端请求的是目录别名 %s，落到了 %s）" % (requested, entry["model"]), "别名"
        return "模型重定向或改用（未确认：客户端请求 %s，登记在 %s 名下；%s）" % (
            requested, entry["model"], "目录里它现在不是这个模型的别名" if CATALOG_OK else "读不到模型目录，无法核对别名"), "未确认"
    messages = last.get("messages") or start.get("input_messages")
    if messages:
        images = (last.get("images") or 0) if last and not from_ledger else (start.get("input_media") or 0)
        # 登记的图片只数 image/* 内联数据，内联的视频、音频只算在运行日志的媒体里：判断下游取两者较大的；渠道测试从不带媒体与文件，
        # 运行日志里媒体或文件不为 0 的在这里归为下游，不会进入下面的渠道测试判定
        inline = max(images, start.get("input_media") or 0)
        files = max(last.get("files") or 0, start.get("input_files") or 0)
        if not account_level and images and interaction_model(entry["model"]) and not code7:
            return "下游带图调用 Omni（%s）：新版本只在 403 的错误体不带协议错误码 7 时（出口或 GFE 的 HTML 错误页）才登记带图请求，" \
                   "多半与账户权限无关" % shape_text(last, start), "下游带图"
        # 续接上一轮（previous_response_id、previous_interaction_id）的请求带着历史，与多条消息一样是下游（只有账本正文看得出）
        if inline or files or messages > 1 or start.get("_continuation"):
            return "下游主动调用（%s）" % shape_text(last, start), "下游"
        tokens = str((start.get("parameters") or {}).get("max_output_tokens") or "")
        chars = start.get("input_text_chars")
        if path.endswith(TEST_PATHS):
            if start and (chars is None or chars <= 8) and TEST_TOKENS.match(tokens or "默认"):
                return "new-api 渠道测试（1 条 %s 字的消息，max_output_tokens %s）" % (chars, tokens or "默认"), "渠道测试"
            if not start:
                return "多半是 new-api 渠道测试（1 条消息、没有图片与文件；运行日志里没有这个请求的开始行，看不到 max_tokens 与视频、音频等其他内联数据）", \
                    "渠道测试"
        return "下游主动调用（1 条消息，形态不像渠道测试：文字 %s 字，max_output_tokens %s）" % (chars, tokens or "-"), "下游"
    if ledger != "ok":
        return "无法对照（登记里没有请求来源，用量账本也读不到）", "无法对照"
    if record is None and (start or entry.get("finish")):
        return "无法判断请求形态（本实例运行日志里有这个请求，但登记与运行日志都看不到消息条数）", "无法对照"
    if record is None:
        extra = "，早于账本保留期" if entry["age"] > 90 else ""
        return "账本里没有对应请求（可能是共用账户目录的其他实例或复制来的目录%s%s）" % (extra, shared_note()), "账本没有"
    return None, None


def tried_accounts(record):
    # 账本里这个请求试过的不同账户数（不区分大小写）：请求最终失败时 account 是最后一次尝试的账户，它也在 attempts 里；同一账户换通道再试、
    # 额度用完改用的备注（账户为空）都不另算
    return len({(attempt.get("account") or "").lower() for attempt in record.get("attempts") or [] if attempt.get("account")} |
               ({record["account"].lower()} if record.get("account") else set()))


def record_brief(record):
    # 账本里已知的：接口、依次试了几个不同的账户与最终结果
    return "接口 %s %s，这个请求依次试了 %d 个不同的账户，最终%s（HTTP %s %s）" % (
        record.get("protocol") or "-", record.get("path") or "-", tried_accounts(record), final_text(record), record.get("status") or "-",
        record.get("state") or "-")


def ledger_judge(entry):
    """账本里有这个请求、登记与运行日志都看不出形态时补判（判断规则与 judge 是同一套）：
      a. 有正文：按正文得出的同口径形态交给 judge（请求名能确定是目录别名或 MODEL_FALLBACKS 的源模型时一并交给它）；正文被截断时截断本身
         说明请求体超过 64 KiB，不是渠道测试的一条短消息。没有正文、Gemini 原生路径里的请求名是别名或源模型时同样先判别名、改用；
         运行日志结束行的 fallback_from、账本记录的上游尝试说明登记的模型是额度用完改用的目标时，有没有正文都先判为改用（与有开始行时
         judge 先判改用相同）
      b. 仍看不出：按账本的输入 tokens（阈值见 LEDGER_TEST_TOKENS 的说明与 token_limits）。渠道测试只认最终成功的；上游终止生成（blocked）的
         用量是上游给的，够得上下游门槛时同样判下游；取消、失败的请求有 tokens 也不按它判断
      c. 仍判断不了：写“无法判断请求形态”，附上接口、依次试了几个账户、最终结果，与看不到形态的原因（分类记在 entry["gaps"]）"""
    info = entry.get("ledger") or ledger_info(entry)
    record, last = entry["record"], entry["last_request"] or {}
    pseudo, gaps, short = info["start"], [], info["short"] or "没有请求正文"
    requested, where = info.get("requested") or ("", "")
    if pseudo:
        text, kind = judge(entry, pseudo)
        if kind is None and pseudo.get("_truncated"):
            text, kind = "下游主动调用（%s：远不止渠道测试的一条短消息）" % ledger_shape_text(pseudo), "下游"
        elif kind is None and pseudo.get("_continuation"):
            text, kind = "下游主动调用（%s）" % ledger_shape_text(pseudo), "下游"
        if kind is not None:
            # 结论里用到了正文形态（ledger_shape_text）时已写明“按账本里的请求正文”，其余（渠道测试、别名等）在末尾注明
            basis = SWITCH_BASIS[where] if kind == "改用" and where in SWITCH_BASIS else \
                "账本记录的请求路径" if where == "path" and kind in ("别名", "改用") else "账本里的请求正文"
            return text if "按账本里的请求正文" in text else text + "，依据" + basis, kind
        gaps.append(("noshape", "正文里没有对话消息"))
        short = "正文里没有对话消息"
    elif requested:
        # 没有可用的正文，Gemini 原生路径里的请求名是目录别名或 MODEL_FALLBACKS 的源模型，或运行日志的结束行、账本记录的上游尝试说明
        # 发生过额度用完改用（见 ledger_requested）
        text, kind = judge(entry, {"_ledger": True, "model": requested, "_switched": where in SWITCH_BASIS})
        if kind is not None:
            return text + "，依据" + SWITCH_BASIS.get(where, "账本记录的请求路径"), kind
    if not pseudo and info["gap"]:
        gaps.append(info["gap"])
    path = base_path(last.get("path") or record.get("path") or "")
    tokens, reply = record_tokens(record)
    state = record.get("state")
    success = state in SUCCESS_STATES
    samples, test_limit, down_limit = token_limits(record, entry)
    if tokens > 0 and (success or state == "blocked"):
        if success and tokens <= test_limit and path.endswith(TEST_PATHS):
            basis = "与一条“hi”相当" if tokens <= LEDGER_TEST_TOKENS else "不超过本次账本里已确认的渠道测试（%s%s）加 %d" % (
                samples_text(samples), "，含本服务加的随机后缀" if NONCE_ON else "", LEDGER_TEST_TOKENS)
            return "多半是 new-api 渠道测试（按账本：输入 %d tokens、回复 %d tokens，%s；%s，看不到 max_tokens 与消息条数）" % (
                tokens, reply, basis, short), "渠道测试"
        if tokens >= down_limit:
            limit = "按本次已确认的渠道测试校准" if samples else "已算上本服务加的随机后缀" if NONCE_ON else ""
            return "下游主动调用（按账本：输入 %d tokens，%s远不止一条短消息%s）" % (
                tokens, "不少于 %d（%s），" % (down_limit, limit) if limit else "",
                "；请求最终被上游终止生成（HTTP %s blocked），输入 tokens 是上游给的" % (record.get("status") or "-") if not success else ""), "下游"
        if not success:
            gaps.append(("tokens", "请求最终被上游终止生成（HTTP %s blocked），按账本输入 %d tokens、回复 %d tokens，不到下游主动调用的 %d；"
                                   "渠道测试只按最终成功的请求判断" % (record.get("status") or "-", tokens, reply, down_limit)))
        elif tokens <= test_limit:
            gaps.append(("tokens", "按账本输入只有 %d tokens、回复 %d tokens，但接口 %s 不是渠道测试会用的接口" % (tokens, reply, path or "-")))
        elif samples:
            gaps.append(("tokens", "按账本输入 %d tokens、回复 %d tokens，介于渠道测试（按本次账本里已确认的渠道测试校准：%s，不超过 %d）"
                                   "与下游主动调用（不少于 %d）之间" % (tokens, reply, samples_text(samples), test_limit, down_limit)))
        elif NONCE_ON:
            gaps.append(("tokens", "按账本输入 %d tokens、回复 %d tokens：输入含本服务加的 32 个零宽字符随机后缀（REPEAT_PROMPT_NONCE，约十几到"
                                   "近百个 token），一条“hi”的渠道测试也会到这个数；本次账本里没有可校准的渠道测试（形态已确认、最终成功），"
                                   "不按 tokens 判断（不少于 %d 才判为下游）" % (tokens, reply, down_limit)))
        else:
            gaps.append(("tokens", "按账本输入 %d tokens、回复 %d tokens，介于渠道测试（不超过 %d）与下游主动调用（不少于 %d）之间" % (
                tokens, reply, test_limit, down_limit)))
    elif tokens > 0:
        gaps.append(("tokens", "请求最终没有成功（HTTP %s %s），按账本输入 %d tokens、回复 %d tokens，不按 tokens 判断" % (
            record.get("status") or "-", state or "-", tokens, reply)))
    elif success:
        gaps.append(("tokens", "账本里这个请求的输入 tokens 为 0"))
    else:
        gaps.append(("failed", "请求最终没有成功，账本里没有输入 tokens"))
    gaps = [("reg", "登记没有请求形态（f06ddf9 之前的版本不记 last_request）" if not last else "登记的 last_request 里没有消息条数"),
            log_gap(entry)] + gaps
    entry["gaps"] = [code for code, _ in gaps]
    return "无法判断请求形态（按账本：%s；看不到形态：%s）" % (record_brief(record), "；".join(text for _, text in gaps)), "无法判断形态"


def request_source(entry):
    # 先按登记与运行日志判断（judge），账本里有这个请求却看不出形态时用账本补判（ledger_judge）；账本里也没有、只知道请求 ID 时按 ID 前缀
    # 补一句接口类型（只作补充说明，不改分类）
    text, kind = judge(entry, entry.get("start") or {})
    if kind is None:
        text, kind = ledger_judge(entry)
    if id_only(entry):
        text += "；请求 ID 前缀 %s：%s（只按 ID 推断接口类型）" % id_kind(entry["request_id"])
    return text, kind


def conclude(entry):
    """结论与分类；需要验证账号的账户（按账户，见第 2 段）的登记写成“需要验证账号（…）；请求来源：…”，分类为“需要验证”（读得到冷却列表、
    整户暂停已到期时为“需要验证（暂停已到期）”；单个模型的暂停不生效时为“单个模型需要验证（暂停未生效）”），来源的分类另记在 entry["source_kind"]。这个账户在其他模型上的普通 403 同样归在
    需要验证账号名下（管理页按账户把它整个移出 403 列表），结论写明账户级 403 登记在哪个模型、这一条是普通 403"""
    text, kind = request_source(entry)
    entry["source_kind"] = kind
    if not entry["verify_account"]:
        return text, kind
    if entry["verify_scope"] == "model":
        targets = "、".join(entry["verify_targets"])
        # 暂停不生效：运行日志里只有没有写入的行时写“没有写入”，否则与管理页同一说法写“未生效（已到期或没有写入）”，不说成 30 天已到期
        if model_pause_unwritten(entry):
            state = "%s 的暂停没有写入（运行日志：账号冷却未写入），照常调度" % targets
        elif pause_expired(entry):
            state = "%s 的暂停未生效（已到期或没有写入），照常调度" % targets
        else:
            state = "只暂停 %s，其他模型照常" % targets
        head = "单个模型需要验证（%s%s）" % (state, "" if entry["verify"] else "；这一条是普通 403")
        return "%s；请求来源：%s" % (head, text), "单个模型需要验证（暂停未生效）" if pause_expired(entry) else "单个模型需要验证"
    state = "整户暂停已到期，重新参与调度" if pause_expired(entry) else "整户暂停调度中"
    if entry["verify"]:
        head = "需要验证账号（账户级，%s）" % state
    elif entry["verify_models"]:
        head = "需要验证账号（账户级，%s；账户级 403 登记在 %s，这一条是普通 403）" % (state, "、".join(entry["verify_models"]))
    else:
        head = "需要验证账号（账户级，%s；登记里已没有账户级条目，这一条是普通 403）" % state
    return "%s；请求来源：%s" % (head, text), "需要验证（暂停已到期）" if pause_expired(entry) else "需要验证"


def path_label(item):
    # 汇总用的接口：登记或账本记录的路径，按 base_path 去掉 /trace、/ultra、/pro 前缀，Gemini 原生路径里的模型名换成 *（别名与正式名不分开计）；
    # 都没有时按请求 ID 前缀写接口类型
    path = base_path((item["last_request"] or {}).get("path") or (item["record"] or {}).get("path") or "")
    if path:
        return re.sub(r"/models/[^/:]+:", "/models/*:", path)
    prefix = id_kind(item.get("request_id"))[0]
    return "接口不明（请求 ID 前缀 %s）" % prefix if prefix else "接口不明"


def verify_summary(group, single):
    # 需要验证账号的账户汇总成一小段（每个账户取一条登记，见 per_account），整户与单个模型各一段：账户数与时间范围、按请求来源分类的账户数、
    # 按接口的账户数、涉及的请求 ID，以及账户数最多的前 5 个请求（一个请求依次换号、每个账户都 403，管理页里就多出这么多需要验证账号的账户）；
    # 大部分账户无法判断请求形态时写明主要原因与下一步
    accounts = per_account(group)
    if single:
        print("单个模型需要验证（只在一个模型上要求验证，只暂停该模型最长 30 天，其他模型照常，管理页列在“403 单个模型需要验证”）：%s" % verify_brief(group))
    else:
        print("需要验证账号（整户：2 个及以上模型要求验证或整户暂停生效，整户暂停调度 30 天，管理页不在 403 列表里）：%s" % verify_brief(group))
    kinds = collections.Counter(item["source_kind"] for item in accounts)
    print("  按请求来源（账户数）：" + "、".join("%s %d" % item for item in kinds.most_common()))
    print("  按接口（账户数）：" + "、".join("%s %d" % item for item in collections.Counter(path_label(item) for item in accounts).most_common()))
    groups, no_id = collections.OrderedDict(), []
    for item in accounts:
        if item["request_id"]:
            groups.setdefault(item["request_id"], []).append(item)
        else:
            no_id.append(item)
    # 没有请求 ID 的：登记里没有 last_request、账本里也没有对上的请求。账本读不到时没有按时间查过，不能说“没找到”
    missing = []
    if no_id and ledger != "ok":
        missing.append("%d 个账户的登记没有请求 ID，账本读不到，无法按时间找对应请求" % len(no_id))
    elif no_id:
        timed = sum(1 for item in no_id if item["last"])
        if timed:
            missing.append("%d 个账户的登记没有请求 ID、按时间在账本里也没找到对应请求" % timed)
        if len(no_id) > timed:
            missing.append("%d 个账户的登记里没有请求 ID 与时间" % (len(no_id) - timed))
    print("  涉及 %d 个不同的请求 ID%s" % (len(groups), "；另有 " + "，".join(missing) if missing else ""))
    top = [group for group in sorted(groups.items(), key=lambda group: -len(group[1]))[:5] if len(group[1]) > 1]
    if top:
        print("  账户数最多的请求（一个请求依次换号，每个账户都返回需要验证账号的 403）：")
    elif groups:
        print("  每个请求 ID 只涉及 1 个账户")
    for request_id, items in top:
        times = [item["last"] for item in items if item["last"]]
        record = next((item["record"] for item in items if item["record"]), None)
        ledger_note = ""
        if record:
            # 按不同的账户计（见 tried_accounts）
            ledger_note = "；账本：这个请求依次试了 %d 个账户，最终%s" % (
                tried_accounts(record), "成功" if record.get("state") in SUCCESS_STATES else
                "%s（HTTP %s %s）" % (final_text(record), record.get("status") or "-", record.get("state") or "-"))
        print("    请求 %s（%s · %s）：连续换了 %d 个账户，每个都 403%s%s" % (
            request_id, items[0]["source_kind"], path_label(items[0]), len(items),
            "，%s 至 %s" % (show(min(times)), show(max(times))) if times else "", ledger_note))
    unknown = [item for item in accounts if item["source_kind"] == "无法判断形态"]
    if len(unknown) * 2 > len(accounts):
        # 大部分账户看不出请求形态：按 ledger_judge 记下的原因计数，写明主要原因与下一步
        gaps = collections.Counter(code for item in unknown for code in set(item.get("gaps") or []))
        print("  大部分账户（%d/%d 个）无法判断请求形态，主要原因：%s；下一步看第 3 段这个模型按接口、按天的请求数与成功数，判断现在还有没有"
              "这类请求，并按时间与接口到 new-api 的使用日志里查这个模型" % (len(unknown), len(accounts), "、".join(
                  "%s（%d 个）" % (label, gaps[code]) for code, label in GAP_LABELS.items() if gaps[code]) or "见逐条结论"))
    if BODY_SKIPPED:
        print("  ⚠ 需要读请求正文的请求超过 %d 个，%d 个没有读正文（见第 4 段末尾）" % (BODY_FETCH_LIMIT, len(BODY_SKIPPED)))
    if BODY_ABORTED:
        print("  ⚠ 读请求正文%s，之后 %d 个请求的正文没有读（见第 4 段末尾）" % (BODY_ABORT["reason"], len(BODY_ABORTED)))
    expired = sum(1 for item in accounts if pause_expired(item))
    print("  → " + ADVICE["单个模型需要验证" if single else "需要验证"] +
          (("；暂停未生效（已到期或没有写入）的 %d 个该模型照常调度" if single else "；暂停已到期的 %d 个已重新参与调度") % expired +
           "，仍未验证的下一次请求会再返回 403 并重新暂停" if expired else "") +
          ("" if VERIFY_LIST else "（VERIFY=list 逐个列出这些账户与各自的请求来源）"))


print("\n== 7. 结论（每个登记账户一行，带最近一次 403 的本机时间，与管理页 403 列表的“最近一次”相同；规则见 README“没有调用过的模型出现在 403 列表里”；"
      "需要验证账号的账户先汇总，VERIFY=list 逐个列出）")
if not entries:
    print("没有要判断的登记。")
concluded = [(entry,) + conclude(entry) for entry in entries]
if VERIFY_ENTRIES:
    if VERIFY_ACCOUNT_ENTRIES:
        verify_summary(VERIFY_ACCOUNT_ENTRIES, False)
    if VERIFY_MODEL_ENTRIES:
        verify_summary(VERIFY_MODEL_ENTRIES, True)
    if any(detailed(entry) for entry in entries):
        print("逐条结论%s：" % ("" if VERIFY_LIST else "（需要验证账号以外的账户）"))
for entry, text, kind in concluded:
    if not detailed(entry):
        continue
    notes = []
    if entry["id_missing"] and (entry["start"] or entry["finish"]):
        notes.append(LEDGER_PENDING)
    if entry["others"] and not kind.startswith("需要验证") and not kind.startswith("单个模型需要验证"):
        notes.append("该账户另有 %d 个模型 403，账户本身可能受限" % len(entry["others"]))
    stats = by_model.get(entry["model"])
    day = show(entry["last"])[:10] if entry["last"] else ""
    if kind not in ("旧记录", "需要验证", "需要验证（暂停已到期）", "单个模型需要验证", "单个模型需要验证（暂停未生效）", "账本没有") and entry["count"] >= 3 and entry["last"] and (NOW - entry["last"]).days < 3 and stats and stats["days"].get(day, [0, 0])[1] > 0:
        notes.append("近期反复 403、同一天这个模型有成功：这个账户多半确实不能用这个模型")
    where = "最近 %s" % short(entry["last"])
    if entry["request_id"]:
        where += " · 请求 %s" % entry["request_id"]
    print("%s · %s（%s）：%s%s → %s" % (mask(entry["label"]), entry["model"], where, text, ("；" + "；".join(notes)) if notes else "", ADVICE[kind]))
if FIX_COUNT == "0":
    print("⚠ 正在运行的程序早于 93c16fa：带图的 Omni 请求被拒时仍会误登记正常账户，先升级")

# ---------- 8. 所有模型的 403 与号池级 403 ----------
# 回答“有没有出现上游 Mag1cFall/AIStudio2API 529043e 修的那种‘长时间运行后全部账户 403’”：上游那种 403 是 WAA VM 运行 12 小时后在后台重建、
# 拿到匿名挑战，之后所有账户的请求都被拒（文案按经验多为 The caller does not have permission），要等下一个 12 小时；本服务没有那个缺陷，
# 这一段只用运行日志把它变成可以核查的数据（只读，不看 MODEL，ACCOUNT 照常只看邮箱含它的账户；账号打码，上游文案打码后最多 100 字）：
#   a. 所有模型的“上游返回 4xx … HTTP=403 …”行（请求进度，来源为执行账号，见 internal/app/upstream_error_log.go）按文案分三类：
#      需要验证账号（verify-account / must be verified）、The caller does not have permission、其他（列出最多的 5 种文案），按天、按小时计数；
#   b. 号池级 403 判定行（“号池内 N 分钟内至少 M 个账号对模型 X 返回 403 且没有成功，按号池级 403 处理，不冷却账号”，见 runtime.go）与
#      撤销登记的 INFO 行（“号池级 403 判定成立，撤销 403 无权限账户记录 | 模型= | 账户= | 起点=”，见 permission_denied.go）的次数与时间分布；
#      判定行按触发的账户过滤；撤销行不带账户，设了 ACCOUNT 时按全号池单独写在 b 段（标明“全号池”），不进按天、按小时表；
#   c. 每次 403 与号池级判定发生时，这个账户的 Worker 已经活了多久（距同一账户最近一次“WAA Worker 就绪”，同一 PID 的“WAA Worker 停止”、
#      进程重启或同一账户下一次就绪都算它结束）与距进程启动多久（start.sh 写的“===== … start.sh … =====”分隔行、或“管理监听启动”行，
#      2 分钟内的算同一次；日志里没有时按正在运行的进程的启动时间）；
#   d. Worker 存活时长分布（就绪到停止），按停止的“起因=”分开；到日志末尾仍在运行、进程重启时仍在运行的另计；
#   e. 同一分钟 3 个及以上不同账户出现非 verify 403 的聚类：账户数、文案类别、方法、模型（按同一请求的 request.started）、Worker 年龄、
#      前后 10 分钟内有没有号池级判定。
# 读程序目录与本机其他实例目录各自的 logs/aistudio2api.log 与轮转出的 aistudio2api.log.1（以及 .2、.N.gz、-日期 这类外部轮转的文件），
# 按修改时间从旧到新读、事件再按时间排序；每个实例单独统计（Worker 只在本实例的日志里就绪、停止）
import gzip

LOG_NAME = re.compile(r"^aistudio2api\.log(?:\.\d+|-\d{8,10})?(?:\.gz)?$")
START_LINE = re.compile(r"^===== (\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) start\.sh ")
DENY_CLASSES = (("verify", "需要验证账号（verify-account）"), ("caller", "The caller does not have permission"), ("other", "其他文案"))
CALLER = re.compile(r"caller does not have permission", re.I)
# 号池级 403 判定的门槛（runtime.go 的 forbiddenWindow 与 forbiddenSystemicAccounts）写在判定行里，这里只取模型
POOL_LINE = re.compile(r"对模型 (\S+) 返回 403 且没有成功，按号池级 403 处理")
REVOKE_LINE = re.compile(r"号池级 403 判定成立，撤销 403 无权限账户记录 \| 模型=([^ |]+) \| 账户=(\d+)")
FIELD = re.compile(r"(?:^| \| )([^=| ]+)=([^|]*?)(?= \| |$)")
# 年龄分段：11–13 小时单独一段，上游那种 403 集中在 VM 运行约 12 小时之后
AGE_BUCKETS = ((60, "<1 分钟"), (300, "1–5 分钟"), (1800, "5–30 分钟"), (7200, "30 分钟–2 小时"), (21600, "2–6 小时"),
               (39600, "6–11 小时"), (46800, "11–13 小时"), (None, "13 小时以上"))
LONG_RUN = 11 * 3600
CLUSTER_ACCOUNTS = 3
HOUR_ROWS = 48


def duration_text(seconds):
    seconds = max(0, int(seconds))
    if seconds < 120:
        return "%d 秒" % seconds
    if seconds < 7200:
        return "%d 分" % (seconds // 60)
    if seconds < 172800:
        return "%.1f 小时" % (seconds / 3600.0)
    return "%.1f 天" % (seconds / 86400.0)


def bucket_text(values):
    counts = collections.Counter()
    for value in values:
        for limit, label in AGE_BUCKETS:
            if limit is None or value < limit:
                counts[label] += 1
                break
    return "、".join("%s %d" % (label, counts[label]) for _, label in AGE_BUCKETS if counts[label]) or "-"


def median(values):
    ordered = sorted(values)
    return ordered[len(ordered) // 2] if ordered else 0


def spread_text(values):
    # 个数、中位、最长与分段；没有值时写“没有”
    if not values:
        return "没有"
    return "%d 条，中位 %s，最长 %s；%s" % (len(values), duration_text(median(values)), duration_text(max(values)), bucket_text(values))


def log_paths(directory):
    # 运行日志与轮转出的文件，按修改时间从旧到新，当前的 aistudio2api.log 放最后（start.sh 轮转是复制为 .1 再截断原文件）
    folder = os.path.join(directory, "logs")
    try:
        names = [name for name in os.listdir(folder) if LOG_NAME.match(name)]
    except OSError:
        return []
    def order(name):
        try:
            modified = os.path.getmtime(os.path.join(folder, name))
        except OSError:
            modified = 0
        return (name == "aistudio2api.log", modified, name)
    return [os.path.join(folder, name) for name in sorted(names, key=order)]


def open_log(path):
    if path.endswith(".gz"):
        return gzip.open(path, "rt", encoding="utf-8", errors="replace")
    return open(path, encoding="utf-8", errors="replace")


def deny_class(message):
    if ACCOUNT_LEVEL.search(message):
        return "verify"
    if CALLER.search(message):
        return "caller"
    return "other"


def scan_instance(paths):
    """逐行读一个实例的运行日志，返回 (事件列表, 请求 ID → (模型, 号池), 日志最早与最晚的时间)。事件为 (时间, 序号, 种类, 账户, 数据)，
    种类：start 进程启动、ready Worker 就绪、stop Worker 停止、deny 上游 403、pool 号池级判定、revoke 撤销登记"""
    events, requests, first, last = [], {}, None, None
    order = 0
    for path in paths:
        try:
            handle = open_log(path)
        except OSError:
            continue
        with handle:
            for line in handle:
                order += 1
                if line.startswith("====="):
                    match = START_LINE.match(line)
                    if match:
                        try:
                            moment = datetime.datetime.strptime(match.group(1), "%Y-%m-%d %H:%M:%S").astimezone()
                        except ValueError:
                            continue
                        events.append((moment, order, "start", "", {}))
                    continue
                if not line.startswith("{"):
                    continue
                interesting = ("WAA Worker 就绪" in line or "WAA Worker 停止 |" in line or "管理监听启动" in line or
                               ("上游返回 4xx" in line and "HTTP=403" in line) or "按号池级 403 处理" in line or
                               "号池级 403 判定成立" in line or '"request.started"' in line)
                if not interesting and first is not None:
                    # 只为记下日志最晚的时间解析时间字段（不解析整行 JSON）
                    stamp = line.find('"time":"')
                    if stamp >= 0:
                        moment = parse_time(line[stamp + 8:line.find('"', stamp + 8)])
                        if moment:
                            last = moment
                    continue
                try:
                    item = json.loads(line)
                except ValueError:
                    continue
                moment = parse_time(item.get("time"))
                if moment is None:
                    continue
                first = first or moment
                last = moment
                if not interesting:
                    continue
                message = item.get("msg") or ""
                source = item.get("source") or ""
                request = item.get("request") or {}
                if item.get("event") == "request.started":
                    if request.get("id"):
                        requests[request["id"]] = (request.get("model") or "", request.get("pool") or "")
                    continue
                if message.startswith("管理监听启动"):
                    events.append((moment, order, "start", "", {}))
                    continue
                if message.startswith("号池级 403 判定成立"):
                    match = REVOKE_LINE.search(message)
                    if match:
                        events.append((moment, order, "revoke", "", {"model": match.group(1), "count": int(match.group(2))}))
                    continue
                if not wanted_account(source, source):
                    continue
                fields = dict((key, value.strip()) for key, value in FIELD.findall(message))
                if message.startswith("WAA Worker 就绪"):
                    events.append((moment, order, "ready", source.lower(), {
                        "pid": fields.get("PID", ""), "source": fields.get("来源", ""), "cause": fields.get("起因", "")}))
                elif message.startswith("WAA Worker 停止 |"):
                    events.append((moment, order, "stop", source.lower(), {"pid": fields.get("PID", ""), "cause": fields.get("起因", "")}))
                elif message.startswith("上游返回 4xx") and fields.get("HTTP") == "403":
                    wording = re.sub(r"\s+", " ", fields.get("文案", "")).strip()
                    events.append((moment, order, "deny", source.lower(), {
                        "class": deny_class(message), "method": fields.get("方法", "") or "?", "wording": wording,
                        "request": request.get("id") or "", "label": source}))
                elif "按号池级 403 处理" in message:
                    match = POOL_LINE.search(message)
                    events.append((moment, order, "pool", source.lower(), {
                        "model": match.group(1) if match else "?", "request": request.get("id") or "", "label": source}))
    events.sort(key=lambda event: (event[0], event[1]))
    return events, requests, first, last


def pool_label(pool):
    return {"": "默认", "normal": "默认", "free": "默认"}.get(pool, pool)


def analyze_instance(directory, started_at):
    paths = log_paths(directory)
    print("\n-- 实例目录 %s（运行日志：%s）" % (directory, "、".join(os.path.basename(path) for path in paths) or "没有"))
    if not paths:
        print("没有运行日志（logs/aistudio2api.log 与轮转出的 .1 等都不在）")
        return
    events, requests, first, last = scan_instance(paths)
    if first is None:
        print("运行日志里没有可解析的 JSON 行")
        return
    print("日志从 %s 到 %s" % (show(first), show(last)))
    current = {}  # 账户 → 当前 Worker：{"at": 就绪时间, "pid": PID, "cause": 就绪起因}
    lifetimes = []  # (秒, 停止起因)
    cut_by_restart = []  # 进程重启时仍在运行的 Worker 已运行的秒数
    starts = []
    last_start = None
    ages = {kind: [] for kind, _ in DENY_CLASSES}
    unknown_age = {kind: collections.Counter() for kind, _ in DENY_CLASSES}
    since_start = {kind: [] for kind, _ in DENY_CLASSES}
    by_class = {kind: [] for kind, _ in DENY_CLASSES}
    pools, revokes = [], []
    daily, hourly = collections.OrderedDict(), collections.OrderedDict()
    unmatched_stops = 0

    def counted(table, key, column, amount=1):
        table.setdefault(key, collections.Counter())[column] += amount

    def process_age(moment):
        if last_start is not None:
            return (moment - last_start).total_seconds()
        if started_at is not None and started_at <= moment:
            return (moment - started_at).total_seconds()
        return None

    for moment, _, kind, account, data in events:
        local = moment.astimezone()
        day, hour = local.strftime("%Y-%m-%d"), local.strftime("%Y-%m-%d %H 时")
        if kind == "start":
            if last_start is not None and (moment - last_start).total_seconds() < 120:
                continue
            cut_by_restart.extend((moment - worker["at"]).total_seconds() for worker in current.values())
            current.clear()
            last_start = moment
            starts.append(moment)
        elif kind == "ready":
            old = current.get(account)
            if old is not None:
                # 同一账户新 Worker 就绪：旧的在发布时关掉（原槽位重开不写停止行），停止行随后到来时 PID 对不上、不再重复计
                lifetimes.append(((moment - old["at"]).total_seconds(), "同账户新 Worker 接替"))
            current[account] = {"at": moment, "pid": data["pid"], "cause": data["cause"]}
        elif kind == "stop":
            worker = current.get(account)
            if worker is not None and (data["pid"] in ("", "0") or worker["pid"] in ("", "0") or worker["pid"] == data["pid"]):
                lifetimes.append(((moment - worker["at"]).total_seconds(), data["cause"] or "未写起因（旧版本）"))
                del current[account]
            else:
                unmatched_stops += 1
        elif kind in ("deny", "pool"):
            worker = current.get(account)
            age = (moment - worker["at"]).total_seconds() if worker else None
            uptime = process_age(moment)
            data = dict(data, at=moment, age=age, uptime=uptime)
            if kind == "deny":
                group = data["class"]
                by_class[group].append(data)
                counted(daily, day, group)
                counted(hourly, hour, group)
                if age is not None:
                    ages[group].append(age)
                else:
                    unknown_age[group]["就绪早于日志开头" if last_start is None else "这次进程里没有这个账户的就绪行"] += 1
                if uptime is not None:
                    since_start[group].append(uptime)
            else:
                pools.append(data)
                counted(daily, day, "pool")
                counted(hourly, hour, "pool")
        elif kind == "revoke":
            revokes.append(dict(data, at=moment))
            if ACCOUNT:
                # 撤销行不带账户、不能按 ACCOUNT 过滤：只在 b 段按全号池写，不进按天、按小时表（表里其余各列都只算 ACCOUNT 的账户）
                continue
            counted(daily, day, "revoke")
            counted(daily, day, "revoked", data["count"])
            counted(hourly, hour, "revoke")
            counted(hourly, hour, "revoked", data["count"])
    running = [(last - worker["at"]).total_seconds() for worker in current.values()]

    if starts:
        print("进程启动（start.sh 分隔行或“管理监听启动”）：%s" % "、".join(show(moment) for moment in starts[-5:]) +
              ("（共 %d 次，只列最近 5 次）" % len(starts) if len(starts) > 5 else ""))
    elif started_at is not None:
        print("日志里没有进程启动行，距进程启动按正在运行的进程算（启动于 %s）" % show(started_at))
    else:
        print("日志里没有进程启动行，也没有正在运行的进程：距进程启动算不出")

    total = sum(len(items) for items in by_class.values())
    print("a. 上游 403（所有模型，“上游返回 4xx … HTTP=403”行）共 %d 行、%d 个账户%s" % (
        total, len({item["label"].lower() for items in by_class.values() for item in items}),
        "" if total else "：运行日志覆盖的这段时间没有上游 403"))
    for kind, label in DENY_CLASSES:
        items = by_class[kind]
        if not items:
            print("  %s：0 行" % label)
            continue
        methods = collections.Counter(item["method"] for item in items)
        print("  %s：%d 行、%d 个账户，%s 至 %s；方法 %s" % (
            label, len(items), len({item["label"].lower() for item in items}), show(items[0]["at"]), show(items[-1]["at"]),
            "、".join("%s×%d" % pair for pair in methods.most_common(4))))
        if kind == "other":
            wordings = collections.Counter(mask(item["wording"])[:100] or "（没有文案）" for item in items)
            for wording, count in wordings.most_common(5):
                print("    %d × %s" % (count, wording))
    columns = (("verify", "verify"), ("caller", "caller"), ("other", "其他"), ("pool", "号池级判定"), ("revoke", "撤销行"))

    def table_row(key, counts):
        cells = ["%s %d" % (title, counts[column]) for column, title in columns if counts[column]]
        if counts["revoked"]:
            cells.append("撤销账户 %d" % counts["revoked"])
        return "  %s | %s" % (key, " | ".join(cells))

    if daily:
        print("  按天（本机时区；verify = 需要验证账号，caller = The caller does not have permission）：")
        for key, counts in daily.items():
            print(table_row(key, counts))
        rows = list(hourly.items())
        print("  按小时%s：" % ("（只列最近 %d 个有记录的小时，更早的 %d 个略）" % (HOUR_ROWS, len(rows) - HOUR_ROWS) if len(rows) > HOUR_ROWS else ""))
        for key, counts in rows[-HOUR_ROWS:]:
            print(table_row(key, counts))

    if ACCOUNT:
        print("b. 号池级 403 判定（号池内 10 分钟内至少 5 个账号对同一模型 403 且没有成功，不冷却账号）：由邮箱含 ACCOUNT 的账户触发的 %d 次；"
              "撤销登记的 INFO 行不带账户、不按 ACCOUNT 过滤，按全号池计（不进上面的按天、按小时表）：%d 条、共撤销 %d 个账户记录" % (
                  len(pools), len(revokes), sum(item["count"] for item in revokes)))
    else:
        print("b. 号池级 403 判定（号池内 10 分钟内至少 5 个账号对同一模型 403 且没有成功，不冷却账号）：%d 次；撤销登记的 INFO 行 %d 条、共撤销 %d 个账户记录" % (
            len(pools), len(revokes), sum(item["count"] for item in revokes)))
    if pools:
        models = collections.Counter(item["model"] for item in pools)
        print("  模型：%s；%s 至 %s" % ("、".join("%s×%d" % pair for pair in models.most_common(5)), show(pools[0]["at"]), show(pools[-1]["at"])))
        print("  判定时距进程启动：%s" % spread_text([item["uptime"] for item in pools if item["uptime"] is not None]))
        print("  判定时触发账户的 Worker 年龄：%s" % spread_text([item["age"] for item in pools if item["age"] is not None]))
        for item in pools[-10:]:
            model, pool = requests.get(item["request"], ("", ""))
            print("  %s | 模型 %s | 号池 %s | %s | Worker 年龄 %s | 距进程启动 %s" % (
                show(item["at"]), item["model"], pool_label(pool) if item["request"] in requests else "?", mask(item["label"]),
                duration_text(item["age"]) if item["age"] is not None else "?",
                duration_text(item["uptime"]) if item["uptime"] is not None else "?"))
    if revokes:
        models = collections.Counter()
        for item in revokes:
            models[item["model"]] += item["count"]
        print("  撤销%s：%s（账户记录数）；%s 至 %s" % ("（全号池）" if ACCOUNT else "","、".join("%s×%d" % pair for pair in models.most_common(5)), show(revokes[0]["at"]), show(revokes[-1]["at"])))

    print("c. 403 发生时 Worker 已运行多久（距同一账户最近一次“WAA Worker 就绪”）与距进程启动多久：")
    for kind, label in DENY_CLASSES:
        if not by_class[kind]:
            continue
        missing = unknown_age[kind]
        print("  %s：" % label)
        print("    Worker 年龄：%s%s" % (spread_text(ages[kind]), "；算不出 %d 条（%s）" % (
            sum(missing.values()), "、".join("%s %d" % pair for pair in missing.most_common())) if missing else ""))
        print("    距进程启动：%s" % spread_text(since_start[kind]))

    ended = [seconds for seconds, _ in lifetimes]
    print("d. Worker 存活时长（就绪到停止）：已结束 %s" % spread_text(ended))
    causes = collections.defaultdict(list)
    for seconds, cause in lifetimes:
        causes[cause].append(seconds)
    if causes:
        print("  按停止起因：%s" % "；".join("%s %d 个（中位 %s）" % (cause, len(values), duration_text(median(values)))
                                       for cause, values in sorted(causes.items(), key=lambda pair: -len(pair[1]))))
    if running:
        print("  到日志末尾仍在运行 %d 个：已运行中位 %s、最长 %s" % (len(running), duration_text(median(running)), duration_text(max(running))))
    if cut_by_restart:
        print("  进程重启时仍在运行 %d 个：已运行中位 %s、最长 %s（重启前没有停止行）" % (
            len(cut_by_restart), duration_text(median(cut_by_restart)), duration_text(max(cut_by_restart))))
    if unmatched_stops:
        print("  另有 %d 行停止对不上就绪行（就绪早于日志开头，或已被同账户新 Worker 接替）" % unmatched_stops)

    minutes = collections.OrderedDict()
    for kind in ("caller", "other"):
        for item in by_class[kind]:
            minutes.setdefault(item["at"].astimezone().strftime("%Y-%m-%d %H:%M"), []).append(item)
    clusters = [(key, items) for key, items in sorted(minutes.items())
                if len({item["label"].lower() for item in items}) >= CLUSTER_ACCOUNTS]
    print("e. 同一分钟 %d 个及以上不同账户出现非 verify 403：%s" % (CLUSTER_ACCOUNTS, "%d 个分钟" % len(clusters) if clusters else "没有"))
    for key, items in clusters[-10:]:
        accounts = {item["label"].lower() for item in items}
        groups = collections.Counter(dict(DENY_CLASSES)[item["class"]] for item in items)
        methods = collections.Counter(item["method"] for item in items)
        models = collections.Counter(requests.get(item["request"], ("?", ""))[0] or "?" for item in items)
        known = [item["age"] for item in items if item["age"] is not None]
        moment = items[0]["at"]
        nearby = any(abs((pool["at"] - moment).total_seconds()) <= 600 for pool in pools)
        print("  %s | %d 个账户、%d 行 | %s | 方法 %s | 模型 %s | Worker 年龄中位 %s | 前后 10 分钟%s号池级判定" % (
            key, len(accounts), len(items), "、".join("%s×%d" % pair for pair in groups.most_common()),
            "、".join("%s×%d" % pair for pair in methods.most_common(3)), "、".join("%s×%d" % pair for pair in models.most_common(3)),
            duration_text(median(known)) if known else "?", "有" if nearby else "没有"))
    if len(clusters) > 10:
        print("  （只列最近 10 个）")

    systemic = by_class["caller"] + by_class["other"]
    long_age = [item for item in systemic if item["age"] is not None and item["age"] >= LONG_RUN]
    long_uptime = [item for item in systemic if item["uptime"] is not None and item["uptime"] >= LONG_RUN]
    if not by_class["caller"] and not pools:
        print("→ 没有 The caller does not have permission，也没有号池级 403 判定：这段日志里没有上游那种“长时间运行后全部账户 403”")
    else:
        print("→ 非 verify 403 %d 行：Worker 年龄 11 小时以上 %d 行、距进程启动 11 小时以上 %d 行，同一分钟多账户聚类 %d 个，号池级判定 %d 次。"
              "上游那种 403 的特征是成批出现（同一分钟很多账户、号池级判定）、集中在进程或 Worker 运行约 12 小时之后、文案多为 "
              "The caller does not have permission；都对得上时把这一段发给开发者，只是零散几行多半是单个账户或模型没有权限" % (
                  len(systemic), len(long_age), len(long_uptime), len(clusters), len(pools)))


print("\n== 8. 所有模型的 403 与号池级 403（不看 MODEL%s；判断有没有上游那种“长时间运行后全部账户 403”；时间为本机时区）" % (
    "，只看账户含 \"%s\" 的" % ACCOUNT if ACCOUNT else ""))
instance_dirs = collections.OrderedDict()
instance_dirs[os.path.realpath(APP)] = PROC_START
for row in (ENV.get("INSTANCES") or "").splitlines():
    parts = row.split("|")
    if len(parts) < 2 or not parts[1]:
        continue
    try:
        started = datetime.datetime.fromtimestamp(int(parts[3]), UTC) if len(parts) > 3 and parts[3] else None
    except ValueError:
        started = None
    directory = os.path.realpath(parts[1])
    if instance_dirs.get(directory) is None:
        instance_dirs[directory] = started
if len(instance_dirs) > 1:
    print("本机有 %d 个实例目录，各自的运行日志分开统计（Worker 只在本实例的日志里就绪、停止）" % len(instance_dirs))
for directory, started_at in instance_dirs.items():
    analyze_instance(directory, started_at)
PY
