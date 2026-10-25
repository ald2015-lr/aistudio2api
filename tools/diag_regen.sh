#!/usr/bin/env bash
# AIStudio2API 一键诊断"长回复中途截断 / 截断后又重新生成 / 生成久了会重复生成"：配置与链路 → 结束原因与耗时 →
# 本服务期限截断 → 相同输入的重发 → 服务端换号与客户端/中转重试 → 日志关键字 → 样例 → 结论
# 只读：只对本机管理接口发 GET 请求（/api/config、/api/requests），只读运行日志、用量账本（SQLite 以只读方式打开）与 nginx 配置文件，
# 不改任何配置、冷却、账号或 Worker，也不发生成请求；临时文件放在 mktemp -d 建的目录里，退出时删除（建不了时直接退出）。
# python3 没有 sqlite3 模块时不读用量账本，只分析运行日志。
#
# 截断来源按用量账本与请求日志的 error 分辨（客户端看到的都是 504 "Deadline expired before operation could complete."）：
#   本服务首事件前期限 … 到期          首字之前就到时（排队、冷启动、隐藏思考时的长思考：Build 通道上游不返回思考，
#                                        Playground 通道上游返回但本服务丢弃）
#   本服务首事件后空闲期限 … 到期      开始输出后上游连续 REQUEST_TIMEOUT 没有事件也没有网络字节
#   本服务总时长上限 … 到期            整条请求超过 max(1 小时, REQUEST_TIMEOUT)
#   … 协议错误码 4: Deadline expired … Google 侧自己的期限
#   AI Studio stream closed before finish  上游在结束帧之前断流
#   只有 context deadline exceeded 本身   旧版本的整段 REQUEST_TIMEOUT（或计数、视频、转写请求）；带 "AI Studio … 协议位置"
#                                        或 "BiDi … 等待回复超时" 的是上游或浏览器链路超时，归到上游断流，不是本服务期限
#
# 用法（放在程序目录的 tools/ 下运行，或用 APP_DIR 指定程序目录）：
#   bash tools/diag_regen.sh
#   MINUTES=4320 bash tools/diag_regen.sh         统计最近 72 小时（默认 1440 分钟 = 24 小时；日志只保留当前与 .1 两个文件）
#   MODEL=<模型> bash tools/diag_regen.sh          重点分析的模型（默认取窗口内截断最多的模型，没有截断时取 ULTRA_PRIMARY_MODELS 的
#                                                 第一个，都没有时取请求最多的模型；MODEL=all 看全部模型）
#   RETRY_GAP=600 bash tools/diag_regen.sh        前一个请求结束后多少秒内来的相同输入算"重发"（默认 300）
#   SAMPLES=20 bash tools/diag_regen.sh           每类示例最多列几条（默认 8）
#   DIAG_REQUEST_TIMEOUT=30m bash tools/diag_regen.sh
#                                                 读不到 /api/config（OFFLINE、服务没运行、没有 CONFIG_FILE）时按这个 REQUEST_TIMEOUT 判断
#                                                 （顺序：/api/config 或 CONFIG_FILE，再 DIAG_REQUEST_TIMEOUT，再 .env，最后按 5m）；
#                                                 不读 shell 里导出的 REQUEST_TIMEOUT，免得 source 过 .env 后盖过管理页热更新的值
#   NGINX_DIRS="/etc/nginx /www/server/panel/vhost/nginx" bash tools/diag_regen.sh   指定要只读扫描的 nginx 配置目录
#   OFFLINE=1 bash tools/diag_regen.sh            不访问管理接口，只分析运行日志与用量账本
#   APP_DIR=/path/to/app bash tools/diag_regen.sh 指定程序目录（含 start.sh、.env、logs/、runtime/ 的目录）
#   LOG_FILE=… LEDGER_DB=… DIAG_NOW=2026-10-22T08:00:00Z OFFLINE=1 bash tools/diag_regen.sh
#                                                 分析排查包里的运行日志与账本副本，"现在"按 DIAG_NOW（RFC 3339）计；
#                                                 CONFIG_FILE、REQUESTS_FILE 可指定保存下来的 /api/config、/api/requests 输出
set -u
script_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd)"
if [ -z "${APP_DIR:-}" ]; then
  for candidate in "$script_dir/.." "$script_dir" "$PWD" /www/wwwroot/Chat2API/build; do
    if [ -f "$candidate/start.sh" ]; then APP_DIR="$(cd "$candidate" && pwd)"; break; fi
  done
fi
APP_DIR="${APP_DIR:-/www/wwwroot/Chat2API/build}"
MINUTES="${MINUTES:-1440}"
RETRY_GAP="${RETRY_GAP:-300}"
SAMPLES="${SAMPLES:-8}"
LOG="${LOG_FILE:-$APP_DIR/logs/aistudio2api.log}"
DB="${LEDGER_DB:-$APP_DIR/runtime/requests.db}"
NGINX_DIRS="${NGINX_DIRS:-/etc/nginx /usr/local/nginx/conf /usr/local/openresty/nginx/conf /www/server/nginx/conf /www/server/panel/vhost/nginx}"
# 管理接口与公开 API 共用 LISTEN_ADDR；绑定在 0.0.0.0、:: 或没有写主机时从本机 127.0.0.1 访问
addr="$(grep -E '^LISTEN_ADDR=' "$APP_DIR/.env" 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d '"[:space:]')"
port="${addr##*:}"
host="${addr%:*}"
[ "$host" = "$addr" ] && host=""
case "$host" in "" | 0.0.0.0 | "[::]" | "::") host="127.0.0.1" ;; esac
case "$port" in "" | *[!0-9]*) port="2048" ;; esac
API="http://$host:$port"
# 控制面 /api/ 必须携带管理令牌（程序目录下的 .admin-token）
ADMIN_TOKEN="$(tr -cd '0-9a-fA-F' 2>/dev/null < "$APP_DIR/.admin-token")"
command -v python3 >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }
case "$MINUTES" in "" | *[!0-9]* | 0) echo "MINUTES 必须是正整数"; exit 1 ;; esac
case "$RETRY_GAP" in "" | *[!0-9]* | 0) echo "RETRY_GAP 必须是正整数（秒）"; exit 1 ;; esac
case "$SAMPLES" in "" | *[!0-9]*) echo "SAMPLES 必须是非负整数"; exit 1 ;; esac
# mktemp 失败（TMPDIR 不存在、磁盘已满）时 WORK 为空，临时文件会写到根目录且不会清理：直接退出
WORK="$(mktemp -d)" && [ -n "$WORK" ] && [ -d "$WORK" ] || { echo "无法创建临时目录（检查 TMPDIR 与磁盘空间）"; exit 1; }
trap 'rm -rf "$WORK"' EXIT
section() { printf '\n==================== %s ====================\n' "$1"; }

# fetch 路径 超时秒数 输出文件：只发 GET；失败时输出文件为空并说明原因。连不上或令牌被拒绝时不再访问其他接口
API_STATE="unknown"
[ "${OFFLINE:-0}" = "1" ] && API_STATE="offline"
if [ "$API_STATE" != "offline" ] && ! command -v curl >/dev/null; then
  API_STATE="offline"
  echo "  没有 curl（apt install -y curl）：不访问管理接口，REQUEST_TIMEOUT 按 CONFIG_FILE、DIAG_REQUEST_TIMEOUT、.env 的顺序判断，都没有时按 5m"
fi
fetch() {
  local code rc
  : > "$WORK/$3"
  case "$API_STATE" in down | denied | offline) return ;; esac
  code="$(curl -s -m "$2" --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" -o "$WORK/$3" -w '%{http_code}' "$API$1" 2>/dev/null)"
  rc=$?
  if [ "$code" = "200" ]; then
    API_STATE="ok"
    return
  fi
  : > "$WORK/$3"
  case "$rc:$code" in
    7:* | 6:*)
      API_STATE="down"
      echo "  ⚠ 管理接口 $API 连不上（服务没有运行，或 LISTEN_ADDR 不是这个地址）：以下只分析运行日志与用量账本" ;;
    28:*) echo "  ⚠ 读取 $1 超过 $2 秒没有返回：服务在运行但很忙，这部分数据缺失" ;;
    *:401 | *:403)
      API_STATE="denied"
      if [ -z "$ADMIN_TOKEN" ]; then
        echo "  ⚠ 管理接口拒绝访问（HTTP $code）：找不到 $APP_DIR/.admin-token；以下只分析运行日志与用量账本"
      else
        echo "  ⚠ 管理接口拒绝访问（HTTP $code）：管理令牌不对，确认 $APP_DIR/.admin-token 属于这个服务；以下只分析运行日志与用量账本"
      fi ;;
    *) echo "  ⚠ 读取 $1 失败（HTTP ${code:-无响应}，curl 退出码 $rc）" ;;
  esac
}

section "数据来源"
echo "  程序目录：$APP_DIR    管理接口：$API$([ "$API_STATE" = "offline" ] && echo "（不访问）")"
echo "  现在：$(date '+%F %T %Z')（UTC $(date -u '+%T')）$([ -n "${DIAG_NOW:-}" ] && echo "；分析按 DIAG_NOW=$DIAG_NOW 计")"
fetch /api/config 10 config.json
# CONFIG_SOURCE 为 config.json 的来源：api 为管理接口（当前生效值），file 为 CONFIG_FILE（保存时的值，可能已过时），空为都没有
CONFIG_SOURCE=""
[ -s "$WORK/config.json" ] && CONFIG_SOURCE="api"
fetch /api/requests 15 requests.json
# 离线分析时可以指定保存下来的接口输出
[ -n "${CONFIG_FILE:-}" ] && [ ! -s "$WORK/config.json" ] && cp "$CONFIG_FILE" "$WORK/config.json" 2>/dev/null && CONFIG_SOURCE="file"
[ -n "${REQUESTS_FILE:-}" ] && [ ! -s "$WORK/requests.json" ] && cp "$REQUESTS_FILE" "$WORK/requests.json" 2>/dev/null
# .env 里的 REQUEST_TIMEOUT / FIRST_EVENT_TIMEOUT（管理接口读不到时的后备）
ENV_REQUEST_TIMEOUT="$(grep -E '^REQUEST_TIMEOUT=' "$APP_DIR/.env" 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d '"[:space:]')"
ENV_FIRST_EVENT_TIMEOUT="$(grep -E '^FIRST_EVENT_TIMEOUT=' "$APP_DIR/.env" 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d '"[:space:]')"

# nginx 配置只读扫描：与长请求有关的超时、重试、缓冲与 Cloudflare 真实 IP 设置（只 grep，不加载、不 reload）
: > "$WORK/nginx.txt"
for dir in $NGINX_DIRS; do
  [ -e "$dir" ] || continue
  grep -rnIE --exclude='*.default' --exclude='*.bak' \
    '^[[:space:]]*(proxy_read_timeout|proxy_send_timeout|send_timeout|proxy_connect_timeout|keepalive_timeout|proxy_next_upstream[a-z_]*|proxy_buffering|proxy_ignore_client_abort|proxy_request_buffering|set_real_ip_from|real_ip_header|proxy_pass|server_name|location)[[:space:]]' \
    "$dir" 2>/dev/null | head -n 400 >> "$WORK/nginx.txt"
done

cat > "$WORK/diag_regen.py" <<'PY'
import collections
import datetime
import hashlib
import json
import os
import re
import shutil
import urllib.parse

try:
    import sqlite3
except ImportError:
    # 自行编译、缺 sqlite 开发包的 python3 没有 _sqlite3 模块：不读用量账本，退回到只分析运行日志（见 open_db）
    sqlite3 = None

ENV = os.environ
UTC = datetime.timezone.utc
TD = datetime.timedelta
MINUTES = max(1, int(ENV.get("MINUTES") or 1440))
RETRY_GAP = max(1, int(ENV.get("RETRY_GAP") or 300))
SAMPLES = max(0, int(ENV.get("SAMPLES") or 8))
SUCCESS = ("completed", "tool_calls", "limited")
API_STATE = ENV.get("API_STATE") or "unknown"
VERDICTS = []
TIME_RE = re.compile(r'^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$')
# 生成请求总时长上限的默认值（app/generate_context.go generateMaxDuration）：实际为 max(1 小时, REQUEST_TIMEOUT)
GENERATE_MAX_DURATION = 3600.0
# 前一个失败后多少秒内以 Anthropic 协议重发相同输入，按 new-api Claude 类型渠道的失败重试看（它的重试不等待）
RELAY_RETRY_GAP = 3.0


def parse_time(value):
    # Go 的时间：RFC 3339，0～9 位小数秒，时区为 Z 或 ±hh:mm
    match = TIME_RE.match((value or "").strip())
    if not match:
        return None
    fraction = (match.group(2) or "0")[:6].ljust(6, "0")
    zone = "+00:00" if match.group(3) == "Z" else match.group(3)
    try:
        return datetime.datetime.fromisoformat(f"{match.group(1)}.{fraction}{zone}")
    except ValueError:
        return None


def model_key(model):
    model = (model or "").strip().lower()
    return model[len("models/"):] if model.startswith("models/") else model


NOW = parse_time(ENV.get("DIAG_NOW") or "") or datetime.datetime.now(UTC)
UNTIL = NOW + TD(minutes=1)
SINCE = NOW - TD(minutes=MINUTES)
MODEL = ""


def section(title):
    print()
    print(f"==================== {title} ====================")


def share(count, total):
    return f"{count}（{count * 100 // total}%）" if total else str(count)


def pct(values, p):
    values = sorted(v for v in values if v is not None)
    if not values:
        return None
    return values[min(len(values) - 1, int(round((len(values) - 1) * p)))]


def secs(value):
    if value is None:
        return "-"
    if value < 1:
        return f"{value * 1000:.0f}ms"
    if value < 120:
        return f"{value:.1f}s"
    if value < 7200:
        return f"{value / 60:.1f}min"
    return f"{value / 3600:.1f}h"


def dist(values):
    values = [v for v in values if v is not None]
    if not values:
        return "-"
    return (f"中位 {secs(pct(values, .5))}，P90 {secs(pct(values, .9))}，P99 {secs(pct(values, .99))}，"
            f"最长 {secs(max(values))}（{len(values)} 个）")


def num_dist(values):
    values = [v for v in values if v is not None]
    if not values:
        return "-"
    return f"中位 {pct(values, .5)}，P90 {pct(values, .9)}，最多 {max(values)}（{len(values)} 个）"


def top(counter, limit=6, total=None):
    items = counter.most_common(limit)
    text = "；".join(f"{key} {share(value, total) if total else value}" for key, value in items)
    rest = sum(counter.values()) - sum(value for _, value in items)
    return text + (f"；其他 {rest}" if rest > 0 else "") if text else "-"


def local(moment):
    return moment.astimezone().strftime("%m-%d %H:%M:%S") if moment else "-"


def mask(label):
    # 账号只显示前两位与域名，输出可以直接发给开发者
    label = label or ""
    name, at, domain = label.partition("@")
    return (name[:2] + "***" + at + domain) if at else (label[:4] + "***" if len(label) > 4 else label)


def short_id(rid):
    # 请求 ID 形如 <前缀>_<纳秒>_<序号>（api/router.go newID：chatcmpl_、msg_、resp_、int_，其余为 req_）：保留前缀与末尾，便于在日志里搜
    rid = rid or "-"
    return rid if len(rid) <= 28 else rid[:12] + "…" + rid[-12:]


def ms_to_time(value):
    return datetime.datetime.fromtimestamp((value or 0) / 1000, UTC)


def go_duration(text):
    # Go 的 time.Duration 文本（850ms、10.4s、5m0s、1h0m0s）转秒；纯数字按秒
    text = (text or "").strip()
    if re.fullmatch(r"\d+(\.\d+)?", text):
        return float(text)
    parts = re.findall(r"([0-9]*\.?[0-9]+)(ns|us|µs|ms|h|m|s)", text)
    if not parts:
        return None
    scale = {"ns": 1e-9, "us": 1e-6, "µs": 1e-6, "ms": 1e-3, "s": 1, "m": 60, "h": 3600}
    return sum(float(number) * scale[unit] for number, unit in parts)


def matches(model, target):
    # 成功的请求记录上游标明的模型名，可能带版本后缀；一并算作同一个模型
    key = model_key(model)
    if not key or not target:
        return False
    if target in ("all", "*"):
        return True
    return key == target or key.startswith(target + "-")


def is_model(model):
    return matches(model, MODEL)


def protocol_of(path):
    # 与 app/request_logging.go requestProtocol 一致：去掉 /trace、/ultra 前缀后按路径归类
    path = path or ""
    if path.startswith("/trace"):
        path = path[len("/trace"):]
    if path.startswith("/ultra"):
        path = path[len("/ultra"):]
    if path.endswith("/count_tokens") or path.endswith(":countTokens"):
        return "count_tokens"
    if path == "/v1/chat/completions":
        return "openai-chat"
    if path == "/v1/responses":
        return "openai-responses"
    if path.startswith("/v1/messages"):
        return "anthropic"
    if path in ("/v1/interactions", "/v1beta/interactions"):
        return "interactions"
    if path.startswith("/v1beta/"):
        return "gemini"
    return "other"


def load_json(path):
    try:
        with open(path, encoding="utf-8") as handle:
            return json.load(handle)
    except Exception:
        return None


def short_text(text, limit=60):
    text = (text or "").split("\n")[0]
    match = re.search(r"返回 HTTP \d{3}(?:、协议错误码 -?\d+)?: (.*)", text)
    if match:
        text = match.group(1)
    text = re.sub(r"\d+", "N", text).strip(" :;，。")
    return text[:limit] or "(无详情)"


def brief(row, limit=48):
    # 示例里的错误摘要：本服务期限截断原样保留期限与已输出数（不把数字换成 N），其余按 short_text
    if row.get("cut"):
        return row["error"].split("\n")[0][:limit + 12]
    return short_text(row["error"], limit) if row["error"] else ""


def fields(message):
    # "名称 | 键=值 | 键=值" → {键: 值}（只取第一行）
    result = {}
    for part in message.split("\n")[0].split(" | ")[1:]:
        key, sep, value = part.partition("=")
        if sep:
            result[key.strip()] = value.strip()
    return result


# ---------- 本服务自己的期限截断（app/generate_context.go serviceDeadlineError.Error） ----------
# 首事件前：本服务首事件前期限 <期限> 到期[（停在 <位置>）]: <原错误或 context deadline exceeded>
# 首事件后：本服务首事件后空闲期限 <期限> 到期（已输出 推理=N 正文=M）: context deadline exceeded
# 总上限：  本服务总时长上限 <期限> 到期（已输出 推理=N 正文=M）: context deadline exceeded
CUT_RE = re.compile(r"本服务(首事件前期限|首事件后空闲期限|总时长上限) (\S+) 到期")
CUT_KIND = {"首事件前期限": "timeout_start", "首事件后空闲期限": "timeout_idle", "总时长上限": "timeout_total"}
SERVICE_CUT_KINDS = ("timeout_start", "timeout_idle", "timeout_total")


def service_cut(error):
    match = CUT_RE.search(error or "")
    if not match:
        return None
    rest = error[match.end():]
    where = re.match(r"（停在 (.*?)）: ", rest)
    output = re.match(r"（已输出 推理=(\d+) 正文=(\d+)）", rest)
    inner = rest.split(": ", 1)[1] if ": " in rest else ""
    return {
        "kind": CUT_KIND[match.group(1)], "limit": go_duration(match.group(2)), "limit_text": match.group(2),
        "where": where.group(1) if where else "", "inner": inner,
        "reasoning": int(output.group(1)) if output else None, "content": int(output.group(2)) if output else None,
    }


# Worker 页面类失败的错误末尾带页面快照“ | 页面={…}”或“ | 页面=快照失败: …”（camoufoxnative.PageSnapshotMarker，见 docs/logging.md）：
# 快照只占一行、不含“ | ”，之后可能还接着本机代理中转的说明（“；本机代理中转：…”）。快照里是页面上的文字与数字（如 "age":1429），
# 按子串给错误分类前先去掉，免得被当成 429、403、超时等
PAGE_SNAPSHOT_RE = re.compile(r" \| 页面=.*?(?=；本机代理中转：| \| |\n|$)")


def strip_page(text):
    return PAGE_SNAPSHOT_RE.sub("", text or "")


# ---------- 结束原因分类（文案均已在代码里核实，见每条注释） ----------
def end_class(row):
    state, status, error = row["state"], row["status"], strip_page(row["error"])
    low = error.lower()
    if state in SUCCESS:
        return ("达到输出上限（limited）" if state == "limited" else "完成"), "ok"
    if state == "blocked":
        return "上游终止生成（blocked，安全等原因）", "blocked"
    # 本服务自己的期限，必须先于"排队"判断：首事件前期限在排队或回到原账号等待时到期，错误为
    # "本服务首事件前期限 5m0s 到期: 排队 4m59.9s 后请求超时（最后原因=…）: context deadline exceeded"
    cut = row.get("cut")
    if cut:
        if cut["kind"] == "timeout_start":
            if cut["where"]:
                detail = "停在 " + cut["where"]
            elif cut["inner"].startswith("排队"):
                detail = "排队中"
            elif cut["inner"].startswith("回到原账号等待"):
                detail = "回到原账号等 Worker 恢复"
            else:
                detail = "尝试之间（退避等）"
            return "本服务首事件前期限到期·" + detail, "timeout_start"
        if cut["kind"] == "timeout_idle":
            return "本服务首事件后空闲期限到期（开始输出后上游连续无事件、无字节）", "timeout_idle"
        return "本服务总时长上限到期", "timeout_total"
    # 旧版本：app/queue_progress.go queueEnded "排队 X 后请求超时（最后原因=…）: context deadline exceeded"
    if "排队" in error and "后请求超时" in error:
        return "排队中请求超时（旧版本，没有本服务前缀）", "queue_timeout"
    # 旧版本：app/worker_reset.go workerResetWaitEnded "回到原账号等待 X 后请求超时（…）"
    if "回到原账号等待" in error and "后请求超时" in error:
        return "等 Worker 恢复时请求超时（旧版本，没有本服务前缀）", "queue_timeout"
    # api/middleware.go：context.Canceled 或客户端断开 → 499、state=cancelled（app/request_logging.go requestOutcome）
    if state == "cancelled" or status == 499:
        return "已取消 499（客户端/中转/代理断开；或服务重启、管理端取消）", "cancel"
    # 上游 RPC 的 DEADLINE_EXCEEDED（aistudio/client.go RPCError："AI Studio <方法> 返回 HTTP 504、协议错误码 4: Deadline expired…"）
    if "协议错误码 4:" in error or "deadline expired" in low or "deadline_exceeded" in low:
        return "Google 侧期限（协议错误码 4：Deadline expired before operation could complete.）", "upstream_deadline"
    # 浏览器链路超时（camoufoxnative/bidi.go："BiDi <方法> 等待回复超时: context deadline exceeded"）：满足 DeadlineExceeded（504），
    # 但不是本服务期限，必须先于下面的 context deadline exceeded 判断
    if "bidi " in low and "等待回复超时" in error:
        return "浏览器链路超时（BiDi 等待回复超时）", "proto_break"
    # 没有本服务前缀的 ctx 到期：旧版本 dataRequestContext 的整段 REQUEST_TIMEOUT（含流式输出），或计数、视频、转写等其他数据请求。
    # 只认 error 只有 context deadline exceeded 本身（errors.Join 可能重复成多行）：带 "AI Studio … 协议位置 …: context deadline exceeded"
    # 的是浏览器 60 秒没有回应等上游链路超时（camoufoxnative 的响应体读取），留给下面的协议位置判断
    lines = [line.strip() for line in low.strip().split("\n")]
    if lines and all(line == "context deadline exceeded" for line in lines):
        return "本服务期限到时（没有前缀：旧版本整段 REQUEST_TIMEOUT，或其他数据请求）", "timeout_legacy"
    # app/runtime.go errStreamClosedBeforeFinish（forwardEvents 读到事件流关闭而没有终态）/ api/generate.go errIncompleteStream
    if "stream closed before finish" in low:
        return "上游流没有结束帧就关闭（stream closed before finish）", "stream_closed"
    if "stream closed before first event" in low:
        return "首事件前上游流关闭", "stream_closed"
    # aistudio/decoder.go ProtocolEvidenceError："AI Studio <方法> 协议位置 $[…]: <读取错误>"：上游途中断开也可能报成协议错误；
    # 读取错误为 context deadline exceeded 时是浏览器连续 60 秒没有回应（上游链路超时，不是本服务期限）。
    # 断流落在两个元素之间（逗号之前）时读取错误被吞掉，只报“…没有正常结束”（Build 的响应列表、Playground 的 field 1 与根数组、
    # Interactions 的事件列表），同样是中途断开
    if "协议位置" in error:
        if re.search(r"eof|reset|closed|broken pipe|timeout|deadline|i/o|connection|没有正常结束", low):
            return "上游流中途断开（报为协议错误）", "proto_break"
        return "上游协议解析错误·" + short_text(error, 40), "proto"
    if status == 429 or "HTTP 429" in error:
        return "429 限流/额度", "429"
    if status == 504:
        return "504 其他超时·" + short_text(error, 40), "timeout_other"
    if status >= 500 or re.search(r"返回 HTTP 5\d\d", error):
        return f"{status} 上游/内部错误·" + short_text(error, 40), "5xx"
    return f"HTTP {status}·" + short_text(error, 40), "other"


def attempt_class(error):
    error = strip_page(error)
    lower = error.lower()
    if "账户已进入冷却" in error:
        return "发出前已冷却（没有发到上游）"
    if "HTTP 429" in error:
        return "429"
    if "HTTP 401" in error:
        return "401 登录失效"
    if "HTTP 403" in error or "permission_denied" in lower:
        return "403 无权限"
    if "stream closed before first event" in lower:
        return "首事件前流关闭"
    if "协议错误码 4:" in error:
        return "Google 侧期限（协议错误码 4）"
    if re.search(r"HTTP 5\d\d", error):
        return "5xx 上游错误"
    # BiDi 等待回复超时（"BiDi <方法> 等待回复超时: context deadline exceeded"）与浏览器连接断开带 deadline 也不是超时，先于"超时"判断；
    # 协议位置不能整体提前：首事件超时或期限取消的尝试也可能报成协议位置
    if "bidi" in lower or "closed network" in lower:
        return "浏览器/Worker 失败"
    if "首事件超时" in error or "deadline exceeded" in lower:
        return "超时"
    if "协议位置" in error:
        return "协议错误/断流"
    if "worker" in lower or "waa" in lower:
        return "浏览器/Worker 失败"
    return "其他·" + short_text(error, 30)


# ---------- 用量账本 ----------
def open_db(path):
    if not path or not os.path.isfile(path):
        return None, f"找不到用量账本 {path}"
    if sqlite3 is None:
        return None, "python3 没有 sqlite3 模块，只分析运行日志"
    problems = []
    try:
        # 账本是 WAL 模式：服务运行时 -wal、-shm 都在，按普通只读读者打开；没有 -wal 时（服务已停止、已全部合并进主文件）
        # 加 immutable=1，不在程序目录里新建 -wal、-shm
        uri = "file:" + urllib.parse.quote(os.path.abspath(path)) + "?mode=ro"
        if not os.path.exists(path + "-wal"):
            uri += "&immutable=1"
        connection = sqlite3.connect(uri, uri=True, timeout=15)
        connection.execute("PRAGMA query_only = ON")
        connection.execute("SELECT 1 FROM requests LIMIT 1").fetchall()
        return connection, ""
    except Exception as exc:
        problems.append(str(exc))
    # 只读打开失败（例如 WAL 的共享内存文件没有读权限）时复制到临时目录再读；临时目录空间不够或账本太大时放弃
    work = ENV.get("WORK_DIR") or ""
    size = os.path.getsize(path) + (os.path.getsize(path + "-wal") if os.path.isfile(path + "-wal") else 0)
    if work and size < 4 << 30 and shutil.disk_usage(work).free > size * 2:
        try:
            copy = os.path.join(work, "requests-copy.db")
            shutil.copyfile(path, copy)
            if os.path.isfile(path + "-wal"):
                shutil.copyfile(path + "-wal", copy + "-wal")
            connection = sqlite3.connect(copy, timeout=15)
            connection.execute("SELECT 1 FROM requests LIMIT 1").fetchall()
            return connection, f"只读打开失败（{problems[0]}），改读临时副本"
        except Exception as exc:
            problems.append(str(exc))
    return None, "无法读取用量账本：" + "；".join(problems)


def columns_of(connection, table):
    try:
        return {row[1] for row in connection.execute(f"PRAGMA table_info({table})")}
    except Exception:
        return set()


def load_rows(connection, earliest):
    # requests 表（requestdb/requestdb.go schema）：time 为结束时间（Unix 毫秒），*_ms 为毫秒；
    # first_event_ms 为进入服务到收到上游首个事件（0 表示一直没收到），queue_ms 为取得最终执行账号前的等待；
    # attempts 为最终结果之前每次失败的上游尝试（JSON 数组，元素 account、channel、error、duration_ms，api/ledger.go RequestAttempt），
    # 只在首事件之前记录；model 为请求的模型名，收到首事件后改为上游标明的模型名（可能带版本后缀）；served_model 只在与请求的
    # 模型系列不同时有值；旧账本没有的列按空读。账本没有输入指纹列（只有回复指纹 reply_hash），输入指纹改从运行日志取
    have = columns_of(connection, "requests")
    wanted = ["id", "time", "protocol", "path", "model", "account", "channel", "status", "state", "duration_ms",
              "first_event_ms", "queue_ms", "input_tokens", "reasoning_tokens", "reply_tokens", "tool_calls", "error",
              "attempts", "served_model", "reply_hash", "duplicate", "pool"]
    select = ", ".join(column if column in have else f"NULL AS {column}" for column in wanted)
    bounds = (int(earliest.timestamp() * 1000), int(UNTIL.timestamp() * 1000))
    cursor = connection.execute(f"SELECT {select} FROM requests WHERE time >= ? AND time <= ? ORDER BY time", bounds)
    rows = []
    for values in cursor:
        record = dict(zip(wanted, values))
        try:
            attempts = json.loads(record["attempts"] or "[]") or []
        except ValueError:
            attempts = []
        end = ms_to_time(record["time"])
        duration = (record["duration_ms"] or 0) / 1000
        rows.append({
            "id": record["id"] or "", "end": end, "start": end - TD(seconds=duration), "protocol": record["protocol"] or "",
            "path": record["path"] or "", "model": model_key(record["model"]), "served": model_key(record["served_model"]),
            "account": record["account"] or "", "channel": record["channel"] or "", "status": record["status"] or 0,
            "state": record["state"] or "", "duration": duration, "first": (record["first_event_ms"] or 0) / 1000,
            "queue": (record["queue_ms"] or 0) / 1000, "input_tokens": record["input_tokens"] or 0,
            "reasoning": record["reasoning_tokens"] or 0, "reply": record["reply_tokens"] or 0, "tools": record["tool_calls"] or 0,
            "error": record["error"] or "", "attempts": [a for a in attempts if isinstance(a, dict)],
            "reply_hash": record["reply_hash"] or "", "duplicate": bool(record["duplicate"]), "pool": record["pool"] or "",
            "source": "账本",
        })
    latest = connection.execute("SELECT max(time) FROM requests WHERE time <= ?", (bounds[1],)).fetchone()[0]
    return rows, (ms_to_time(latest) if latest else None)


def load_bodies(connection, ids):
    # bodies 表只有开启 REQUEST_BODY_LOG 时才有内容，只保留最近 1000 条，请求与响应各截断到 64KB（api/ledger.go requestBodyLimit）
    if "request" not in columns_of(connection, "bodies"):
        return {}, 0
    total = connection.execute("SELECT count(*) FROM bodies").fetchone()[0]
    result = {}
    wanted = list(ids)
    for index in range(0, len(wanted), 500):
        chunk = wanted[index:index + 500]
        marks = ",".join("?" * len(chunk))
        for rid, request, request_size, response, response_size in connection.execute(
                f"SELECT id, request, request_size, response, response_size FROM bodies WHERE id IN ({marks})", chunk):
            result[rid] = {"request": request or "", "request_size": request_size or 0,
                           "response": response or "", "response_size": response_size or 0}
    return result, total


# ---------- 运行日志（logs/aistudio2api.log，slog JSON：time、level、msg、event、source、request） ----------
LOG_TIME_RE = re.compile(rb'"time":\s*"([^"]+)"')
# slog JSONHandler 不带空格（"level":"WARN"）；也接受手工整理过、带空格的 JSON
ALWAYS_KEYS = [b'"request.finished"', b'"request.started"', b'"level":"WARN"', b'"level":"ERROR"',
               b'"level": "WARN"', b'"level": "ERROR"']
# INFO 级别里需要的几类（request.progress 与生命周期），其余 INFO 跳过，避免读几百 MB 的排队进度
INFO_KEYS = [key.encode() for key in (
    "事件流恢复", "上游首事件到达", "收到上游首个事件", "等待上游响应", "请求准备结束", "上游结束说明",
    "收到停止信号", "进行中的请求已全部完成", "应用新配置", "生成服务已停止", "管理服务就绪", "配置已热更新")]
SKIP_KEYS = ["排队等待 |".encode()]  # 排队进度行很多，这里用不到


def first_time_from(handle, position, size):
    handle.seek(position)
    if position:
        handle.readline()
    for _ in range(200):
        offset = handle.tell()
        raw = handle.readline()
        if not raw:
            return None, size
        match = LOG_TIME_RE.search(raw[:200])
        moment = parse_time(match.group(1).decode("ascii", "replace")) if match else None
        if moment:
            return moment, offset
    return None, handle.tell()


def last_time(handle, size):
    handle.seek(max(0, size - (1 << 16)))
    for raw in reversed(handle.read().splitlines()):
        match = LOG_TIME_RE.search(raw[:200])
        moment = parse_time(match.group(1).decode("ascii", "replace")) if match else None
        if moment:
            return moment
    return None


def seek_since(handle, size, since):
    # 日志按时间追加：二分查找窗口起点，避免从头读几个 GB 的日志；多往前读 8MB 容忍时间乱序
    # （控制台队列最多积压 4096 条，独立队列与汇总行会越过积压先写出，文件顺序与 time 最多差一整个队列）
    low, high = 0, size
    while high - low > 1 << 16:
        middle = (low + high) // 2
        moment, _ = first_time_from(handle, middle, size)
        if moment is None or moment >= since:
            high = middle
        else:
            low = middle
    return max(0, low - (8 << 20))


def read_logs(paths, since):
    entries, coverage = [], []
    for path in paths:
        if not path or not os.path.isfile(path) or os.path.getsize(path) == 0:
            continue
        size = os.path.getsize(path)
        with open(path, "rb") as handle:
            start = seek_since(handle, size, since)
            first, _ = first_time_from(handle, start, size)
            last = last_time(handle, size)
            coverage.append((path, first, last))
            if last and last < since:
                continue
            handle.seek(start)
            if start:
                handle.readline()
            for raw in handle:
                if any(key in raw for key in SKIP_KEYS):
                    continue
                if not any(key in raw for key in ALWAYS_KEYS) and not any(key in raw for key in INFO_KEYS):
                    continue
                try:
                    event = json.loads(raw)
                except ValueError:
                    continue
                if not isinstance(event, dict):
                    continue
                moment = parse_time(event.get("time"))
                if not moment or moment < since or moment > UNTIL:
                    continue
                entries.append({
                    "t": moment, "level": event.get("level", ""), "msg": event.get("msg") or "", "event": event.get("event") or "",
                    "source": event.get("source") or "", "request": event.get("request") or {},
                })
    entries.sort(key=lambda entry: entry["t"])
    return entries, coverage


# ================= 读取数据 =================
config = load_json(ENV.get("CONFIG_JSON", "")) or {}
active = (load_json(ENV.get("REQUESTS_JSON", "")) or {}).get("requests") or []
# 按用法说明的顺序取值：/api/config（当前生效值，含管理页热更新）或 CONFIG_FILE（保存时的值）优先，读不到时才用命令行
# DIAG_REQUEST_TIMEOUT，再 .env；CONFIG_SOURCE 区分 config.json 来自管理接口还是 CONFIG_FILE
TIMEOUT = go_duration(config.get("request_timeout") or "")
if TIMEOUT:
    timeout_note = "（CONFIG_FILE，保存时的值）" if ENV.get("CONFIG_SOURCE") == "file" else "（/api/config，当前生效值）"
elif go_duration(ENV.get("REQUEST_TIMEOUT_OVERRIDE") or ""):
    TIMEOUT, timeout_note = go_duration(ENV.get("REQUEST_TIMEOUT_OVERRIDE")), "（命令行 DIAG_REQUEST_TIMEOUT）"
elif go_duration(ENV.get("ENV_REQUEST_TIMEOUT") or ""):
    TIMEOUT, timeout_note = go_duration(ENV.get("ENV_REQUEST_TIMEOUT")), "（.env）"
else:
    TIMEOUT, timeout_note = 300.0, "（读不到配置，按默认 5m；config/config.go defaultRequestTimeout）"
TOTAL_CAP = max(GENERATE_MAX_DURATION, TIMEOUT)
FIRST_EVENT_TIMEOUT = config.get("first_event_timeout") or ENV.get("ENV_FIRST_EVENT_TIMEOUT") or "0s(默认)"
# 往前多读：窗口开头结束的长请求最长可能从总时长上限之前开始，判断重发还要看它前一个请求
EARLIEST = SINCE - TD(seconds=RETRY_GAP + TOTAL_CAP + 600)

connection, db_note = open_db(ENV.get("DB_PATH", ""))
rows, db_latest, bodies, bodies_total = [], None, {}, 0
if connection is not None:
    try:
        rows, db_latest = load_rows(connection, EARLIEST)
    except Exception as exc:
        db_note = f"读取用量账本出错：{exc}"
        connection = None
log_paths = [path for path in (ENV.get("LOG_PATHS") or "").split("\n") if path]
logs, coverage = read_logs(log_paths, EARLIEST)

log_req = {}                                   # 请求 ID → started/finished 的时间与载荷
progress = collections.defaultdict(list)        # 请求 ID → 请求时间线（request.progress）
lifecycle = []                                  # 服务生命周期事件
account_warn = collections.defaultdict(list)    # 账号 → 不属于某个请求的 WARN/ERROR（runtime.message）
quota_after_output = []                         # "账号冷却 | … | 已输出内容后"
duplicate_reply_logs = []                       # "重复回复 | …"
hot_timeouts = []                               # 配置热更新里的请求超时
LIFECYCLE_KEYS = ("收到停止信号", "等待超时，仍有", "进行中的请求已全部完成", "应用新配置 | 重启生成服务", "生成服务已停止", "管理服务就绪")
for entry in logs:
    request = entry["request"] if isinstance(entry["request"], dict) else {}
    rid = request.get("id") or ""
    if entry["event"] in ("request.started", "request.finished") and rid:
        item = log_req.setdefault(rid, {})
        item["started" if entry["event"] == "request.started" else "finished"] = entry["t"]
        item["data_" + entry["event"].split(".")[1]] = request
        if entry["source"] not in ("", "service", "request"):
            item["source"] = entry["source"]
        continue
    if entry["event"] == "request.progress" and rid:
        progress[rid].append(entry)
        continue
    message = entry["msg"]
    if any(key in message for key in LIFECYCLE_KEYS):
        lifecycle.append(entry)
    if message.startswith("配置已热更新"):
        match = re.search(r"请求超时=([^ |]+)", message)
        if match:
            hot_timeouts.append((entry["t"], match.group(1)))
    if message.startswith("账号冷却") and "已输出内容后" in message:
        quota_after_output.append(entry)
    if message.startswith("重复回复"):
        duplicate_reply_logs.append(entry)
    if entry["level"] in ("WARN", "ERROR") and entry["source"] not in ("", "service", "request"):
        account_warn[entry["source"]].append(entry)


def payload(item):
    return item.get("data_finished") or item.get("data_started") or {}


if connection is None:
    # 没有账本时用日志里的 request.finished 还原（没有逐次尝试 attempts）
    for rid, item in log_req.items():
        data = item.get("data_finished")
        if not data or not item.get("finished"):
            continue
        duration = (data.get("duration_ms") or 0) / 1000
        end = item["finished"]
        usage = data.get("usage") or {}
        rows.append({
            "id": rid, "end": end, "start": end - TD(seconds=duration), "protocol": protocol_of(data.get("path")),
            "path": data.get("path") or "", "model": model_key(data.get("model")), "served": model_key(data.get("served_model")),
            "account": item.get("source") or "", "channel": data.get("channel") or "", "status": data.get("status") or 0,
            "state": data.get("state") or "",
            "duration": duration, "first": (data.get("first_event_ms") or 0) / 1000, "queue": (data.get("queue_ms") or 0) / 1000,
            "input_tokens": usage.get("input_tokens") or 0, "reasoning": usage.get("reasoning_tokens") or 0,
            "reply": usage.get("reply_tokens") or 0, "tools": data.get("tool_calls") or 0, "error": data.get("error") or "",
            "attempts": [], "reply_hash": data.get("reply_hash") or "", "duplicate": False, "pool": data.get("pool") or "",
            "source": "日志",
        })
    rows.sort(key=lambda row: row["end"])

for row in rows:
    row["cut"] = service_cut(row["error"])
    row["class"], row["kind"] = end_class(row)
    row["success"] = row["kind"] == "ok"
# 运行的是拆分期限后的版本：账本或日志里出现过本服务期限的新文案（只说明出现过，没有出现不代表是旧版本）
cut_logs = [entry for entry in logs if entry["msg"].startswith("本服务期限截断 | ")]
NEW_VERSION = any(row["cut"] for row in rows) or bool(cut_logs)
window_all = [row for row in rows if row["end"] >= SINCE]


def pick_model():
    # 重点模型：MODEL 指定的；否则取窗口内截断（本服务期限、Google 期限、断流、旧版本整段期限，或已输出后失败）最多的模型；
    # 没有截断时取 ULTRA_PRIMARY_MODELS 的第一个；都没有时取请求最多的模型
    explicit = model_key(ENV.get("MODEL"))
    if explicit:
        return explicit, "MODEL 指定"
    cut_kinds = SERVICE_CUT_KINDS + ("upstream_deadline", "stream_closed", "proto_break", "timeout_legacy")
    cuts = collections.Counter(row["served"] or row["model"] for row in window_all
                               if row["model"] and (row["kind"] in cut_kinds or (not row["success"] and row["first"])))
    if cuts:
        model, count = cuts.most_common(1)[0]
        return model, f"窗口内截断最多的模型（{count} 个）"
    for model in config.get("ultra_primary_models") or []:
        if model_key(model):
            return model_key(model), "ULTRA_PRIMARY_MODELS 的第一个（窗口内没有截断）"
    requested = collections.Counter(row["model"] for row in window_all if row["model"]).most_common(1)
    if requested:
        return requested[0][0], f"请求最多的模型（{requested[0][1]} 个）"
    return "all", "窗口内没有请求，看全部模型"


MODEL, model_reason = pick_model()

# 只看目标模型：账本的 model 为请求的模型名（取得首事件后改成上游标明的模型名），实际服务模型另记在 served_model
model_rows_all = [row for row in rows if is_model(row["model"]) or is_model(row["served"])]
model_ids = {row["id"] for row in model_rows_all}
if connection is not None and model_ids:
    try:
        bodies, bodies_total = load_bodies(connection, model_ids)
    except Exception:
        bodies, bodies_total = {}, 0


def fingerprints(row):
    # 输入指纹（可能有两种，任一种相同就算相同输入）：
    #   - 日志：协议、号池、模型、消息数、文字字数、内联附件数与字节、文件数、生成参数（不含 seed，客户端每次可能换 seed），来自
    #     request.started / request.finished 日志（api/middleware.go setGenerationInput 统计 API 层收到的输入；生成服务追加的随机后缀不计入）；
    #   - 正文：开启 REQUEST_BODY_LOG 时请求正文前 64KB（去掉 seed）的摘要 + 正文总字节，只保留最近 1000 条，所以与日志指纹并用
    result = []
    item = log_req.get(row["id"]) or {}
    data = payload(item)
    messages, chars = data.get("input_messages") or 0, data.get("input_text_chars") or 0
    if messages or chars:
        params = dict(data.get("parameters") or {})
        params.pop("seed", None)
        # 账本的 model 收到首事件后改成上游标明的模型名：MODEL=all 时用请求开始时的模型名，失败与重发的同一输入才对得上
        family = MODEL if MODEL not in ("all", "*") else model_key((item.get("data_started") or {}).get("model") or row["model"])
        key = (row["protocol"] or protocol_of(row["path"]), row["pool"], family, messages, chars, data.get("input_media") or 0,
               data.get("input_media_bytes") or 0, data.get("input_files") or 0, json.dumps(params, sort_keys=True, ensure_ascii=False))
        result.append(("日志:" + hashlib.sha1(repr(key).encode()).hexdigest()[:12], "日志"))
    body = bodies.get(row["id"])
    if body and body["request"]:
        text = re.sub(r'"seed"\s*:\s*-?\d+\s*,?', "", body["request"])
        result.append(("正文:" + hashlib.sha1(text.encode("utf-8", "replace")).hexdigest()[:12] + f":{body['request_size']}", "正文"))
    return result


def stream_flag(row):
    path = row["path"]
    if ":streamGenerateContent" in path:
        return "流式"
    if ":generateContent" in path:
        return "非流式"
    body = bodies.get(row["id"])
    if body:
        match = re.search(r'"stream"\s*:\s*(true|false)', body["request"][:65536])
        if match:
            return "流式" if match.group(1) == "true" else "非流式"
    return "未知"


def near_timeout(row):
    # 旧版本的整段 REQUEST_TIMEOUT 从 startGenerate 建立请求上下文开始计，账本耗时从进入 HTTP 中间件开始计，略长几百毫秒到几秒
    return TIMEOUT - 2 <= row["duration"] <= TIMEOUT + 20


def response_ending(body):
    # 只看响应正文结尾有没有错误帧或正常结束帧，不输出正文内容；正文超过 64KB 时只保存了开头，看不到结尾
    response = body.get("response") or ""
    if not response:
        return ""
    if len(response.encode("utf-8", "replace")) + 16 < (body.get("response_size") or 0):
        return f"响应正文共 {body['response_size']} 字节，只保存了前 64KB，看不到结尾"
    tail = response[-4096:]
    marks = []
    for key, label in (("event: error", "Anthropic error 事件"), ("response.failed", "Responses response.failed"),
                       ('"error"', "error 对象"), ("message_stop", "message_stop"), ("[DONE]", "[DONE]"),
                       ("response.completed", "response.completed"), ('"finishReason"', "finishReason")):
        if key in tail:
            marks.append(label)
    return f"响应正文共 {body['response_size']} 字节，结尾含：{'、'.join(marks) or '没有结束帧也没有错误帧'}"


for row in model_rows_all:
    row["fps"] = fingerprints(row)
    row["fp"] = bool(row["fps"])
    row["fp_kinds"] = {kind for _, kind in row["fps"]}
    row["stream"] = stream_flag(row)
    row["near_timeout"] = near_timeout(row)
    data = payload(log_req.get(row["id"]) or {})
    row["upstream_bytes"] = data.get("upstream_bytes") or 0
    timeline = progress.get(row["id"], [])
    stalls = [e for e in timeline if e["msg"].startswith("事件流停顿")]
    resumes = [e for e in timeline if e["msg"].startswith("事件流恢复")]
    row["stalls"] = len(stalls)
    row["stall_max"] = max([go_duration((re.search(r"停顿=([^ |]+)", e["msg"]) or [None, ""])[1]) or 0 for e in resumes] or [0])
    # 停顿后没有恢复就结束：最后一次"事件流停顿"之后没有"事件流恢复"
    last_stall = stalls[-1]["t"] if stalls else None
    last_resume = resumes[-1]["t"] if resumes else None
    row["ended_stalled"] = bool(last_stall and (not last_resume or last_resume < last_stall))
    row["stall_at_end"] = ""
    if row["ended_stalled"]:
        # 停顿日志在最后一个事件 15 秒后写出（app/runtime.go streamStallThreshold = 15s）
        silence = (row["end"] - last_stall).total_seconds() + 15
        detail = re.search(r"最近事件=([^ |]+) \| 推理=(\d+) \| 正文=(\d+)", stalls[-1]["msg"])
        row["stall_at_end"] = f"结束前上游已静默约 {secs(silence)}" + (
            f"（最近事件={detail.group(1)}，推理事件 {detail.group(2)}、正文事件 {detail.group(3)}）" if detail else "")
        row["silence_at_end"] = silence
    row["cut_log"] = next((e for e in timeline if e["msg"].startswith("本服务期限截断 | ")), None)
    row["upstream_waits"] = sum(1 for e in timeline if e["msg"].startswith("等待上游响应"))
    row["first_events"] = sum(1 for e in timeline if e["msg"].startswith("收到上游首个事件"))
    row["first_wait_logged"] = any(e["msg"].startswith("上游首事件等待") for e in timeline)

window_rows = [row for row in model_rows_all if row["end"] >= SINCE]
matched_models = collections.Counter(row["served"] or row["model"] for row in window_rows)

print(f"  统计窗口：最近 {MINUTES} 分钟    重点模型：{MODEL}（{model_reason}；可用 MODEL=<模型> 指定，MODEL=all 看全部）"
      f"    重发判定间隔：{RETRY_GAP}s")
print(f"  用量账本：{ENV.get('DB_PATH')}" + (f"（{db_note}）" if db_note else "")
      + (f"，最新一条 {local(db_latest)}" if connection is not None else ""))
if connection is not None and db_latest and (NOW - db_latest).total_seconds() > 1800:
    print(f"  ⚠ 账本最新一条已是 {(NOW - db_latest).total_seconds() / 60:.0f} 分钟前：服务可能没在处理请求，或账本写入失败")
for path, first, last in coverage:
    note = "，全部早于统计窗口" if last and last < EARLIEST else ""
    print(f"  运行日志：{path}（从 {local(first)} 读起，最后一行 {local(last)}{note}）")
if not coverage:
    print("  运行日志：找不到 " + "、".join(log_paths) + "：输入指纹、停顿/恢复、本服务期限截断进度与生命周期部分会缺失")
elif min((c[1] for c in coverage if c[1]), default=NOW) > SINCE + TD(minutes=10):
    print(f"  ⚠ 日志只覆盖到 {local(min(c[1] for c in coverage if c[1]))} 之后（start.sh rotate 在 50MB 时轮转，只留当前与 .1）："
          "更早的请求没有输入指纹，只能用账本的弱指纹")
print(f"  {MODEL} 在窗口内：{len(window_rows)} 个已结束的请求" + (f"（匹配到的模型名：{top(matched_models, 4)}）" if window_rows else ""))
if API_STATE in ("down", "denied", "offline"):
    print("  管理接口不可用：REQUEST_TIMEOUT 按 CONFIG_FILE、DIAG_REQUEST_TIMEOUT、.env 的顺序判断，都没有时按 5m；"
          "第 4b 段（正在进行的请求）会缺失")
print("  说明：控制台日志在缓冲满时会丢条（app/admin.go 控制台队列 4096 条，满了直接丢，每 10 秒检查一次、有丢弃时汇总一行“控制台队列已满”；“额度 429 详情未识别”、生命周期分界行等少数几种不丢），计数以账本为准，日志看比例与时间线")
console_dropped = sum(int((re.search(r"丢弃=(\d+)", e["msg"]) or [0, 0])[1]) for e in logs if e["t"] >= SINCE and "控制台队列已满" in e["msg"])
if console_dropped:
    print(f"  ⚠ 控制台队列满，运行日志丢弃 {console_dropped} 条：时间线可能缺行")
if bodies_total:
    print(f"  请求正文（REQUEST_BODY_LOG）：bodies 表共 {bodies_total} 条，其中 {len(bodies)} 条属于 {MODEL}：这些请求用正文摘要做指纹、判断是否流式")

# ================= 0. 配置与链路 =================
section("0. 相关配置与反向代理")
print(f"  REQUEST_TIMEOUT={secs(TIMEOUT)}{timeout_note}    总时长上限={secs(TOTAL_CAP)}    FIRST_EVENT_TIMEOUT={FIRST_EVENT_TIMEOUT}")
print("    · 拆分期限后的版本（app/generate_context.go）：REQUEST_TIMEOUT 是首个上游事件之前的总期限（排队、冷启动、WAA proof、附件上传、"
      "等首字与首事件前换号都计入）；开始输出后改为空闲上限（上游连续这么久既没有事件也没有网络字节才结束）；整条生成请求最长 "
      f"max(1 小时, REQUEST_TIMEOUT)={secs(TOTAL_CAP)}。三种到期对外都是 504 \"Deadline expired before operation could complete.\"，"
      "error 以“本服务…”开头")
print("    · 旧版本：REQUEST_TIMEOUT 覆盖整条请求（含全部流式输出），到时 error 为没有前缀的 context deadline exceeded")
if NEW_VERSION:
    print("    · 账本或日志里出现过“本服务…期限”文本：运行的是拆分期限后的版本，下面按 error 文本判断截断来源，"
          "\"REQUEST_TIMEOUT 附近结束\"只作参考")
else:
    print("    · 账本与日志里没有“本服务…期限”文本：要么运行的是旧版本，要么这段时间本服务期限没有到期过；"
          "error 只有 context deadline exceeded 本身的按旧版本的整段期限看")
if config.get("ultra_primary_models") is not None:
    print(f"  Ultra 主用模型={','.join(config.get('ultra_primary_models') or []) or '(空)'}  排队上限={config.get('ultra_primary_queue_wait')}")
guard = config.get("downgrade_guard") or {}
if guard.get("enabled") and guard.get("models"):
    hold = guard.get("max_hold_ms") or 0
    print(f"  降级判定：开启，模型={','.join(guard.get('models') or [])}，流式严格模式最长扣住 {'不限' if not hold else secs(hold / 1000)}"
          "（判定出结果前不写响应头；扣住超过 100 秒时前面的 Cloudflare 会 524）")
for moment, value in hot_timeouts[-5:]:
    print(f"  {local(moment)} 配置热更新：请求超时={value}（只影响之后的新请求）")
nginx_lines = [line.rstrip("\n") for line in open(ENV.get("NGINX_TXT") or os.devnull, encoding="utf-8", errors="replace")]
if nginx_lines:
    directives = collections.defaultdict(list)
    for line in nginx_lines:
        match = re.match(r"^(.*?):(\d+):\s*([a-z_]+)\s+(.*?);?\s*$", line)
        if match:
            directives[match.group(3)].append((match.group(1), match.group(2), match.group(4).rstrip(";").strip()))
    print(f"  nginx 配置（只读 grep {ENV.get('NGINX_DIRS')}）：")
    for name in ("proxy_read_timeout", "proxy_send_timeout", "send_timeout", "keepalive_timeout", "proxy_buffering",
                 "proxy_request_buffering", "proxy_ignore_client_abort", "proxy_next_upstream", "proxy_next_upstream_tries",
                 "proxy_next_upstream_timeout"):
        values = collections.Counter(value for _, _, value in directives.get(name, []))
        if values:
            print(f"    {name}：{top(values, 4)}")
    retry_post = [item for item in directives.get("proxy_next_upstream", []) if "non_idempotent" in item[2]]
    if retry_post:
        print(f"    ⚠ proxy_next_upstream 带 non_idempotent（{retry_post[0][0]}:{retry_post[0][1]}）：nginx 会把超时/出错的 POST 转发到下一个上游重发，"
              "在本服务看来就是一个新请求 ID")
        VERDICTS.append("nginx 配置了 proxy_next_upstream non_idempotent：nginx 自己会重发 POST，见第 0 段")
    if any(value == "on" for _, _, value in directives.get("proxy_ignore_client_abort", [])):
        print("    ⚠ proxy_ignore_client_abort on：客户端断开后 nginx 不断开本服务，本服务会继续生成并记成功，账本看不到客户端已经放弃")
    if directives.get("set_real_ip_from") or any("CF-Connecting-IP" in v for _, _, v in directives.get("real_ip_header", [])):
        print("    检测到 set_real_ip_from / real_ip_header：前面很可能有 Cloudflare 等 CDN（Cloudflare 对源站 100 秒读不到字节回 524）")
    if not directives.get("proxy_read_timeout"):
        print("    没有找到 proxy_read_timeout（nginx 默认 60s：非流式请求 60 秒没有字节会被 nginx 断开，客户端看到 504）")
else:
    print("  没有读到 nginx 配置（目录不存在或无权限；可用 NGINX_DIRS 指定）")

# ================= 1. 请求概况 =================
section(f"1. {MODEL} 最近 {MINUTES} 分钟的请求：耗时、首事件与结束原因")
start_rows = [row for row in window_rows if row["kind"] == "timeout_start"]
idle_rows = [row for row in window_rows if row["kind"] == "timeout_idle"]
total_rows = [row for row in window_rows if row["kind"] == "timeout_total"]
legacy_rows = [row for row in window_rows if row["kind"] == "timeout_legacy"]
code4_rows = [row for row in window_rows if row["kind"] == "upstream_deadline"]
closed_rows = [row for row in window_rows if row["kind"] in ("stream_closed", "proto_break")]
if not window_rows:
    print("  这段时间没有该模型已结束的请求（可以用 MINUTES 放大窗口，或 MODEL 换名字）")
else:
    total = len(window_rows)
    by_kind = collections.Counter(row["class"] for row in window_rows)
    print(f"  共 {total} 个请求，结束情况：")
    for label, count in by_kind.most_common(14):
        print(f"    {share(count, total):>10}  {label}")
    statuses = collections.Counter(f"{row['state']}/{row['status']}" for row in window_rows)
    print(f"  状态/状态码：{top(statuses, 8)}")
    streams = collections.Counter(row["stream"] for row in window_rows)
    print(f"  流式与否：{top(streams, 3)}（账本不记 stream 参数：只有 Gemini 路径或开启 REQUEST_BODY_LOG 时能判断）")
    protocols = collections.Counter(f"{row['protocol'] or protocol_of(row['path'])}{'(ultra)' if row['pool'] == 'ultra' else ''}" for row in window_rows)
    print(f"  协议：{top(protocols, 6)}")
    ok_rows = [row for row in window_rows if row["success"]]
    bad_rows = [row for row in window_rows if not row["success"]]
    print(f"  成功请求总耗时：{dist([row['duration'] for row in ok_rows])}")
    print(f"  成功请求首事件：{dist([row['first'] for row in ok_rows if row['first']])}")
    print(f"  成功请求首事件后的输出时长：{dist([row['duration'] - row['first'] for row in ok_rows if row['first']])}")
    longest_ok = [row for row in ok_rows if row["duration"] > TIMEOUT + 20]
    if longest_ok:
        print(f"  成功且总耗时超过 REQUEST_TIMEOUT 的：{len(longest_ok)}（拆分期限后的版本开始输出后不再按 REQUEST_TIMEOUT 截断；"
              "旧版本出现这种情况说明 REQUEST_TIMEOUT 当时更大）")
    print(f"  失败/取消总耗时：{dist([row['duration'] for row in bad_rows])}")
    print(f"  排队（queue_ms）：{dist([row['queue'] for row in window_rows if row['queue']])}")
    with_first = [row for row in window_rows if row["first"]]
    if with_first:
        hold = sum(1 for row in with_first if row["first"] > 50)
        print(f"  首事件超过 50 秒的 {share(hold, len(with_first))}：流式请求写出响应头最多等 50 秒（api/generate.go streamStartHoldMax），"
              "之后先回 200 再推心跳，后面的错误只能放进流里；非流式请求在生成完之前一直没有字节")

    # 本服务自己的期限截断（新版本）
    print()
    print(f"  ▶ 本服务自己的期限截断（error 以“本服务…”开头）：首事件前 {len(start_rows)}、首事件后空闲 {len(idle_rows)}、"
          f"总时长上限 {len(total_rows)}")
    if start_rows:
        print(f"    首事件前期限：{top(collections.Counter(row['class'].split('·', 1)[1] for row in start_rows), 6)}；"
              f"总耗时 {dist([row['duration'] for row in start_rows])}；期限 {top(collections.Counter(row['cut']['limit_text'] for row in start_rows), 3)}")
        waited_first = [row for row in start_rows if "等待上游首事件" in row["cut"]["where"]]
        if waited_first:
            print(f"      其中停在等待上游首事件：{len(waited_first)}（上游已建立响应、一直没有转发给客户端的事件：隐藏思考时的长思考"
                  "（Build 通道上游不返回思考，Playground 通道上游返回但本服务丢弃）、Build 单次调用整段生成，或上游卡住）")
    if idle_rows:
        reasoning = [row["cut"]["reasoning"] for row in idle_rows if row["cut"]["reasoning"] is not None]
        content = [row["cut"]["content"] for row in idle_rows if row["cut"]["content"] is not None]
        print(f"    首事件后空闲期限：总耗时 {dist([row['duration'] for row in idle_rows])}；首事件 {dist([row['first'] for row in idle_rows if row['first']])}；"
              f"已输出推理事件 {num_dist(reasoning)}，正文事件 {num_dist(content)}")
        stalled = [row for row in idle_rows if row["ended_stalled"]]
        if stalled:
            print(f"      结束前有\"事件流停顿\"而没有恢复的 {share(len(stalled), len(idle_rows))}：上游开始输出后卡住，不是还在正常输出")
    if total_rows:
        print(f"    总时长上限：总耗时 {dist([row['duration'] for row in total_rows])}（上限 {top(collections.Counter(row['cut']['limit_text'] for row in total_rows), 2)}）")
    if code4_rows:
        print(f"  ▶ Google 侧期限（协议错误码 4）：{len(code4_rows)}，总耗时 {dist([row['duration'] for row in code4_rows])}，"
              f"其中已收到首事件 {sum(1 for row in code4_rows if row['first'])}（duration_ms 是 Google 期限的估计）")
    if closed_rows:
        print(f"  ▶ 上游断流（stream closed before finish / 协议错误断流）：{len(closed_rows)}，总耗时 {dist([row['duration'] for row in closed_rows])}，"
              f"其中已收到首事件 {sum(1 for row in closed_rows if row['first'])}")

    # 耗时分档
    edges = [0, 30, 50, 60, 90, 110, 130, 170, 230, TIMEOUT - 30, TIMEOUT - 2, TIMEOUT + 20, TIMEOUT * 2, TOTAL_CAP - 30, TOTAL_CAP + 60, 10 ** 9]
    edges = sorted(set(edge for edge in edges if edge >= 0))
    print("  总耗时分档（成功 / 失败 / 取消）：")
    for low, high in zip(edges, edges[1:]):
        bucket = [row for row in window_rows if low <= row["duration"] < high]
        if not bucket:
            continue
        ok = sum(1 for row in bucket if row["success"])
        cancel = sum(1 for row in bucket if row["kind"] == "cancel")
        label = f"{secs(low)}–{secs(high) if high < 10 ** 8 else '∞'}"
        mark = "  ← REQUEST_TIMEOUT 附近" if low == TIMEOUT - 2 else ("  ← 总时长上限附近" if low == TOTAL_CAP - 30 else "")
        print(f"    {label:>14}  {len(bucket):>5} 个：成功 {ok}、失败 {len(bucket) - ok - cancel}、取消 {cancel}{mark}")

    near = [row for row in window_rows if row["near_timeout"]]
    near_bad = [row for row in near if not row["success"]]
    print()
    print(f"  ▶ 在 REQUEST_TIMEOUT（{secs(TIMEOUT)}）附近 [{secs(TIMEOUT - 2)}, {secs(TIMEOUT + 20)}] 结束的：{share(len(near), total)}，"
          f"其中失败/取消 {len(near_bad)}：{top(collections.Counter(row['class'] for row in near_bad), 4)}"
          + ("（新版本只作参考，以上面的 error 分类为准）" if NEW_VERSION else ""))
    if legacy_rows:
        streamed = [row for row in legacy_rows if row["first"]]
        print(f"  ▶ 只有 context deadline exceeded 本身（没有前缀）：{len(legacy_rows)}，其中已收到首事件（正在输出时被整段期限切断）{len(streamed)}，"
              f"总耗时 {dist([row['duration'] for row in legacy_rows])}")
        active_until_end = [row for row in streamed if not row["ended_stalled"] and progress.get(row["id"])]
        if active_until_end:
            print(f"    有时间线、到时前仍在持续收到事件（正常长生成被截断）：{len(active_until_end)}")

    after_output = [row for row in bad_rows if row["first"]]
    print()
    print(f"  ▶ \"已输出内容后\"失败（失败/取消且 first_event_ms>0，服务端已收到上游首事件）：{share(len(after_output), total)}")
    for label, count in collections.Counter(row["class"] for row in after_output).most_common(8):
        rows_of = [row for row in after_output if row["class"] == label]
        print(f"    {count:>5}  {label}：总耗时 {dist([row['duration'] for row in rows_of])}")
    before_output = [row for row in bad_rows if not row["first"]]
    if before_output:
        print(f"  首事件前就失败/取消：{len(before_output)}：{top(collections.Counter(row['class'] for row in before_output), 5)}")

    # 失败与取消的耗时扎堆点：某个外部计时器（代理/中转/客户端/本服务）在同一时刻切断
    print()
    print("  ▶ 失败/取消的耗时扎堆点（同一时刻被切断，说明是某个固定的计时器）：")
    bins = collections.Counter()
    for row in bad_rows:
        if row["duration"] >= 20:
            bins[int(round(row["duration"] / 5.0)) * 5] += 1
    known = {50: "流式响应头等待上限 50s（api/generate.go streamStartHoldMax，只是先回 200，不会断开）",
             60: "nginx 默认 proxy_read_timeout 60s / 常见客户端 60s 超时",
             100: "Cloudflare 回源 100 秒没有字节 → 524",
             120: "常见客户端/中转 120s 超时",
             180: "常见客户端 180s 超时",
             300: "new-api STREAMING_TIMEOUT 默认 300s（空闲计时，心跳会重置）",
             600: "常见客户端 600s 超时（如 Anthropic SDK 默认 10 分钟）"}
    shown = 0
    threshold = max(2, len(bad_rows) // 10)
    for value, count in bins.most_common(8):
        if count < threshold:
            continue
        cluster = [row for row in bad_rows if abs(row["duration"] - value) <= 2.5]
        hint = ""
        for mark, text in known.items():
            if abs(value - mark) <= 5:
                hint = text
        if abs(value - TIMEOUT) <= 10:
            hint = f"本服务 REQUEST_TIMEOUT={secs(TIMEOUT)}（新版本只限首字之前；旧版本覆盖整条请求）"
        if abs(value - TOTAL_CAP) <= 30:
            hint = f"本服务总时长上限 {secs(TOTAL_CAP)}"
        print(f"    ≈{value}s：{count} 个（{top(collections.Counter(row['class'] for row in cluster), 3)}）" + (f"  ← {hint}" if hint else ""))
        shown += 1
        if hint.startswith("Cloudflare") and any(row["kind"] == "cancel" for row in cluster):
            VERDICTS.append(f"有 {count} 个 {MODEL} 请求在 ≈{value}s 被断开（499 取消）：与 Cloudflare 回源 100 秒读超时（524）吻合，"
                            "多半是非流式请求（非流式在生成完之前没有任何字节；流式每 10 秒有心跳，api/generate.go streamHeartbeatInterval），"
                            "长思考模型请用流式；经 new-api 的 OpenAI 类型渠道时 : ping 不会转发，见第 1 段扎堆点")
        elif not hint.startswith("本服务") and any(row["kind"] == "cancel" for row in cluster) and count >= 3:
            VERDICTS.append(f"有 {count} 个 {MODEL} 请求在 ≈{value}s 被客户端/中转/代理断开（499）：前面某一跳有 ≈{value}s 的超时，见第 1 段扎堆点")
    if not shown:
        print("    没有明显的扎堆点")

    if idle_rows:
        VERDICTS.append(f"{len(idle_rows)} 个被本服务首事件后空闲期限截断（开始输出后上游连续 REQUEST_TIMEOUT 没有事件也没有网络字节）："
                        "上游卡住，不是正常长生成被切断；看第 4 段\"本服务期限截断\"的最近事件与最近网络、同账号告警")
    if start_rows:
        where = top(collections.Counter(row["class"].split("·", 1)[1] for row in start_rows), 3)
        VERDICTS.append(f"{len(start_rows)} 个在首字之前被本服务首事件前期限截断（{where}）：排队或冷启动长时看 diag_busy.sh；"
                        "停在等待上游首事件的，多为隐藏思考（Anthropic thinking.type=disabled、Gemini includeThoughts=false：Build 通道上游"
                        "不返回，Playground 通道上游返回但本服务丢弃）时的长思考或单次调用整段生成，要按最长的生成调大 REQUEST_TIMEOUT")
    if total_rows:
        VERDICTS.append(f"{len(total_rows)} 个达到本服务总时长上限 {secs(TOTAL_CAP)}：上游以极低速率一直输出；确实需要更长时调大 REQUEST_TIMEOUT")
    if code4_rows:
        VERDICTS.append(f"{len(code4_rows)} 个是 Google 侧自己的期限（协议错误码 4），耗时 {dist([row['duration'] for row in code4_rows])}："
                        "不是本服务的期限，调大 REQUEST_TIMEOUT 无效，只能缩短思考或输出")
    legacy_streamed = [row for row in legacy_rows if row["first"] and row["near_timeout"]]
    if legacy_streamed:
        VERDICTS.append(f"{len(legacy_streamed)} 个在 REQUEST_TIMEOUT={secs(TIMEOUT)} 处以只有 context deadline exceeded 本身的 error 结束且已经在输出："
                        "运行的是旧版本（REQUEST_TIMEOUT 覆盖整条请求），升级到拆分期限的版本后正在出字的流不再在这里被切断，"
                        "或先调大 REQUEST_TIMEOUT；再看第 2 段是否紧接着有相同输入的新请求")

# ================= 2. 重复请求证据 =================
section(f"2. 重复生成的证据：相同输入、不同请求 ID（{RETRY_GAP}s 内）")
fp_rows = [row for row in model_rows_all if row["fp"]]
print(f"  有输入指纹的请求：{share(len([r for r in window_rows if r['fp']]), len(window_rows))}"
      f"（日志 {sum(1 for r in window_rows if '日志' in r['fp_kinds'])}、正文 {sum(1 for r in window_rows if '正文' in r['fp_kinds'])}）")
print("  指纹 = 协议 + 号池 + 模型 + 消息数 + 文字字数 + 附件数/字节 + 文件数 + 生成参数（不含 seed），来自 request.started/finished 日志"
      "（api/middleware.go）；开启 REQUEST_BODY_LOG 时另用请求正文摘要，任一种相同即算相同输入；账本本身没有输入指纹列")
CUT_FOR_RETRY = SERVICE_CUT_KINDS + ("upstream_deadline", "stream_closed", "proto_break", "timeout_legacy", "5xx", "timeout_other")
groups = collections.defaultdict(list)
for row in fp_rows:
    for fp, _ in row["fps"]:
        groups[fp].append(row)
pairs = []
paired = set()
for fp, items in groups.items():
    items.sort(key=lambda row: row["start"])
    for prev, nxt in zip(items, items[1:]):
        # 日志指纹与正文指纹各分一次组，同一对只算一次
        if nxt["end"] < SINCE or (prev["id"], nxt["id"]) in paired:
            continue
        paired.add((prev["id"], nxt["id"]))
        gap = (nxt["start"] - prev["end"]).total_seconds()
        lead = (nxt["start"] - prev["start"]).total_seconds()
        if gap > RETRY_GAP:
            continue
        relay = False
        if lead < 2:
            kind = "几乎同时发出（2 秒内，双发/并发扇出）"
        elif gap < -1:
            kind = "前一个还没结束就又发了同样的请求"
        else:
            kind = "前一个结束后重发：" + prev["class"]
            # new-api Claude 类型渠道把流内错误改写成 500 后立即在同一个下游连接里重试（不等待），本服务收到的是 Anthropic 协议
            relay = (not prev["success"] and gap <= RELAY_RETRY_GAP
                     and (prev["protocol"] or protocol_of(prev["path"])) == "anthropic")
        pairs.append({"prev": prev, "next": nxt, "gap": gap, "lead": lead, "kind": kind, "relay": relay})
weak_pairs = []
if window_rows:
    # 没有输入指纹的请求（日志缺失或已轮转）退回到弱指纹：同协议、同路径、同模型，前一个失败/取消后 30 秒内的下一个请求
    by_path = collections.defaultdict(list)
    for row in model_rows_all:
        if not row["fp"]:
            by_path[(row["protocol"], row["path"])].append(row)
    for items in by_path.values():
        items.sort(key=lambda row: row["start"])
        for prev, nxt in zip(items, items[1:]):
            gap = (nxt["start"] - prev["end"]).total_seconds()
            if nxt["end"] >= SINCE and not prev["success"] and -1 <= gap <= min(RETRY_GAP, 30):
                weak_pairs.append({"prev": prev, "next": nxt, "gap": gap})
if pairs:
    kinds = collections.Counter(pair["kind"] for pair in pairs)
    print(f"  相同输入的相邻请求对：{len(pairs)}")
    for label, count in kinds.most_common(10):
        gaps = [pair["gap"] if pair["gap"] >= 0 else pair["lead"] for pair in pairs if pair["kind"] == label]
        print(f"    {count:>5}  {label}；间隔 中位 {secs(pct(gaps, .5))}、P90 {secs(pct(gaps, .9))}")
    print(f"    （\"间隔\"对重发是前一个结束到下一个开始，对重叠的是两次开始之间；new-api 失败重试不等待，通常在 {RELAY_RETRY_GAP:.0f} 秒内，"
          "客户端自动重试与人工点重新生成一般更久）")
    chains = list({tuple(r["id"] for r in items): items for items in groups.values()
                   if len([r for r in items if r["end"] >= SINCE]) >= 3}.values())
    if chains:
        print(f"  同一输入在窗口内出现 ≥3 次的：{len(chains)} 组；示例：")
        for items in sorted(chains, key=len, reverse=True)[:max(1, SAMPLES // 2)]:
            steps = " → ".join(f"{'OK' if r['success'] else r['state']}/{r['status']}@{secs(r['duration'])}" for r in items[:8])
            print(f"    {local(items[0]['start'])} 起 {len(items)} 次：{steps}")
    print("  示例（前一个 → 下一个）：")
    order = sorted(pairs, key=lambda pair: (pair["prev"]["kind"] not in CUT_FOR_RETRY, pair["next"]["start"]))
    for pair in order[:SAMPLES]:
        prev, nxt = pair["prev"], pair["next"]
        print(f"    {local(prev['start'])} {short_id(prev['id'])} {prev['protocol'] or protocol_of(prev['path'])} {prev['stream']} "
              f"首事件 {secs(prev['first'])} 总 {secs(prev['duration'])} {prev['state']}/{prev['status']} {brief(prev)}")
        gap_text = f"{secs(pair['gap'])} 后" if pair["gap"] >= 0 else f"前一个开始 {secs(pair['lead'])} 后（重叠）"
        print(f"      └ {gap_text} {short_id(nxt['id'])} 总 {secs(nxt['duration'])} {nxt['state']}/{nxt['status']}"
              + ("  ← 像 new-api Claude 渠道在同一连接里的重试" if pair["relay"] else ""))
    retry_after_cut = [p for p in pairs if p["kind"].startswith("前一个结束后重发") and p["prev"]["kind"] in CUT_FOR_RETRY]
    relay_pairs = [p for p in pairs if p["relay"]]
    retry_after_cancel = [p for p in pairs if p["prev"]["kind"] == "cancel" and p["gap"] >= -1]
    overlapped = [p for p in pairs if p["kind"].startswith("前一个还没结束")]
    if retry_after_cut:
        after_output = sum(1 for p in retry_after_cut if p["prev"]["first"])
        VERDICTS.append(f"{len(retry_after_cut)} 次是\"前一个被截断后，客户端/中转用相同输入新发请求\"（新请求 ID，其中前一个已在输出的 {after_output}）："
                        f"截断→重新生成链条成立；前一个的截断来源：{top(collections.Counter(p['prev']['class'] for p in retry_after_cut), 3)}")
    if relay_pairs:
        VERDICTS.append(f"{len(relay_pairs)} 次在前一个失败后 {RELAY_RETRY_GAP:.0f} 秒内以 Anthropic 协议重发相同输入：符合 new-api Claude 类型渠道的失败重试"
                        "（流内错误被改写成 500，在同一个下游连接里重新请求，客户端在一条回复里看到截断后从头再生成）："
                        "把 new-api 的重试次数设为 0，或把接本服务的渠道改成 OpenAI 类型")
    if retry_after_cancel:
        VERDICTS.append(f"{len(retry_after_cancel)} 次是\"前一个被断开（499）后，用相同输入新发请求\"：断开来自客户端/中转/代理的超时，见第 1 段扎堆点")
    if overlapped:
        still = collections.Counter(p["prev"]["class"] for p in overlapped)
        VERDICTS.append(f"{len(overlapped)} 次在前一个还在生成时又来了相同请求（前一个最后：{top(still, 3)}）：上游一跳已放弃等待并重发，"
                        "而本服务没收到断开（连接没断或代理忽略客户端断开），同一输入同时在生成两次")
if weak_pairs:
    print(f"  没有输入指纹的请求（日志缺失或已轮转）改用弱指纹（同协议、同路径、前一个失败/取消后 30 秒内的下一个请求）：{len(weak_pairs)} 对")
    for label, count in collections.Counter(p["prev"]["class"] for p in weak_pairs).most_common(5):
        print(f"    {count:>5}  前一个：{label}")
    print("    弱指纹不能确认是同一段对话，只能作为旁证")
if not pairs and not weak_pairs:
    print("  没有找到相同输入的相邻请求" + ("（窗口内的请求没有输入指纹：日志缺失或已轮转）" if window_rows and not fp_rows else ""))
dup_rows = [row for row in window_rows if row["duplicate"]]
if dup_rows or duplicate_reply_logs:
    same_prompt = sum(1 for e in duplicate_reply_logs if "提示词=相同" in e["msg"])
    print(f"  回复完全相同：账本 duplicate=1 的 {len(dup_rows)} 个；日志\"重复回复\"{len(duplicate_reply_logs)} 条（其中提示词相同 {same_prompt}）"
          "——这是\"两次都生成完了\"的重复，不是截断")

# ================= 3. 服务端重试 vs 客户端重试 =================
section("3. 服务端重试（同一请求 ID 多次上游尝试）与客户端/中转重试（新请求 ID）")
print("  代码事实：服务端只在\"还没收到上游首个事件\"时换号重试（app/runtime.go generateWithRetry：失败的尝试记进 attempts，"
      "收到首个非错误事件即跳出重试循环），之后 forwardEvents 只转发、不重试。所以服务端重试不会出现\"输出一半又从头再来\"，"
      "首事件前的换号客户端只会看到首字慢。")
print("  请求 ID 每个 HTTP 请求新生成一次（api/router.go newID：chatcmpl_/msg_/resp_/int_<纳秒>_<序号>），客户端或中转重发一定是新 ID。")
retried = [row for row in window_rows if row["attempts"]]
if connection is None:
    print("  账本读不到，没有 attempts，只能看日志里每个请求\"等待上游响应\"的次数")
print(f"  带服务端重试的请求：{share(len(retried), len(window_rows))}")
if retried:
    counts = collections.Counter(len(row["attempts"]) for row in retried)
    print(f"    失败尝试次数：{top(collections.Counter({f'{k} 次': v for k, v in counts.items()}), 5)}")
    classes = collections.Counter(attempt_class(a.get("error")) for row in retried for a in row["attempts"])
    print(f"    失败尝试的原因：{top(classes, 6)}")
    spent = [sum((a.get("duration_ms") or 0) for a in row["attempts"]) / 1000 for row in retried]
    print(f"    失败尝试累计耗时：{dist(spent)}（这段时间计入首事件前期限 REQUEST_TIMEOUT）")
    print(f"    最终结果：{top(collections.Counter(row['class'] for row in retried), 4)}")
    start_retried = [row for row in retried if row["kind"] in ("timeout_start", "timeout_legacy", "queue_timeout")]
    if start_retried:
        print(f"    ⚠ 其中 {len(start_retried)} 个最后因首事件前期限结束：换号耗掉的时间让首字之前就到时")
multi_first = [row for row in window_rows if row["first_events"] > 1]
if progress:
    waits = collections.Counter(min(row["upstream_waits"], 4) for row in window_rows if progress.get(row["id"]))
    print(f"  日志核对：每个请求\"等待上游响应\"次数（=发到上游的尝试数）：{top(collections.Counter({f'{k} 次' if k < 4 else '≥4 次': v for k, v in waits.items()}), 5)}")
    print(f"  同一请求 ID 出现多次\"收到上游首个事件\"：{len(multi_first)}（按代码应为 0）")
    if multi_first:
        VERDICTS.append(f"⚠ 有 {len(multi_first)} 个请求 ID 收到了不止一次上游首事件：与代码预期不符，请把第 3 段与对应请求的日志发给开发者")
if retried and not pairs:
    print("  只有服务端重试、没有相同输入的新请求：重试都发生在首事件之前，客户端看到的只是首字慢")

# ================= 4. 日志关键字 =================
section("4. 日志关键字（文案按代码核实）")
timeline_rows = [row for row in window_rows if progress.get(row["id"])]
if not logs:
    print("  没有可用的运行日志")
else:
    print(f"  {MODEL} 有请求时间线的：{share(len(timeline_rows), len(window_rows))}")
    # 本服务期限截断（app/runtime.go：首事件前在 generateWithRetry 的最终错误路径写在 service 名下，首事件后在 forwardEvents 写在账户名下）
    in_window = [entry for entry in cut_logs if entry["t"] >= SINCE]
    mine = [entry for entry in in_window if (entry["request"] or {}).get("id") in model_ids or MODEL in ("all", "*")]
    stages = collections.Counter(fields(entry["msg"]).get("阶段", "?") for entry in mine)
    print(f"  \"本服务期限截断 | 阶段=…\"：全部 {len(in_window)} 条，其中 {MODEL} {len(mine)} 条：{top(stages, 3) if mine else '-'}")
    before = [fields(entry["msg"]) for entry in mine if fields(entry["msg"]).get("阶段") == "首事件前"]
    if before:
        print(f"    首事件前：停在 {top(collections.Counter(item.get('停在', '?') for item in before), 5)}")
    after = [fields(entry["msg"]) for entry in mine if fields(entry["msg"]).get("阶段") in ("首事件后空闲", "总时长上限")]
    if after:
        quiet = [go_duration(item.get("最近网络")) for item in after if item.get("最近网络")]
        no_bytes = sum(1 for item in after if item.get("网络字节") == "0")
        print(f"    首事件后：最近事件 {top(collections.Counter(item.get('最近事件', '?') for item in after), 4)}；"
              f"最近网络 {dist(quiet)}；从开始就没有网络字节的 {no_bytes}")
    for key, note in (("上游首事件到达时请求已结束，不转发", "首事件与首事件前期限（或取消）同时到达，事件丢弃、未输出"),
                      ("首事件到达时请求已结束，上游事件流未及时结束", "丢弃首事件后 5 秒内上游事件流没有结束"),
                      ("终态后上游事件流", "终态已交给客户端、上游不关流，按完成记录"),
                      ("本服务期限截断后上游事件流未及时结束", "截断后 5 秒内上游事件流没有结束")):
        hits = [e for e in logs if e["t"] >= SINCE and e["msg"].startswith(key)
                and ((e["request"] or {}).get("id") in model_ids or MODEL in ("all", "*") or f"模型={MODEL}" in e["msg"])]
        if hits:
            print(f"  \"{key}\"：{len(hits)}（{note}）")
    with_stall = [row for row in timeline_rows if row["stalls"]]
    print(f"  \"事件流停顿 | 模型=… | 已等待=15s | 最近事件=… | 推理=… | 正文=…\"（forwardEvents，首事件后 15 秒没有新事件，只写日志）："
          f"{share(len(with_stall), len(timeline_rows))} 个请求出现，共 {sum(r['stalls'] for r in with_stall)} 次")
    resumed = [row for row in timeline_rows if row["stall_max"]]
    if resumed:
        print(f"  \"事件流恢复 | 模型=… | 停顿=…\"：单个请求最长停顿 {dist([r['stall_max'] for r in resumed])}")
    ended_stalled = [row for row in timeline_rows if row["ended_stalled"]]
    if ended_stalled:
        print(f"  停顿后没有恢复就结束：{len(ended_stalled)} 个：{top(collections.Counter(r['class'] for r in ended_stalled), 4)}；"
              f"结束前静默 {dist([r.get('silence_at_end') for r in ended_stalled])}")
        bad_stalled = [row for row in ended_stalled if not row["success"] and row["kind"] != "timeout_idle"]
        if len(bad_stalled) >= 2:
            VERDICTS.append(f"{len(bad_stalled)} 个失败请求在结束前上游已静默（停顿后没恢复），结束原因 "
                            f"{top(collections.Counter(r['class'] for r in bad_stalled), 3)}：这部分是上游卡住，不是还在正常输出")
    first_wait = [row for row in timeline_rows if row["first_wait_logged"]]
    print(f"  \"上游首事件等待 | 已等待=15s\"（发出后 15 秒还没首事件）：{share(len(first_wait), len(timeline_rows))}")
    prep_wait = sum(1 for row in timeline_rows if any(e["msg"].startswith("请求准备等待") for e in progress[row["id"]]))
    if prep_wait:
        print(f"  \"请求准备等待\"（WAA proof / 响应头 15 秒还没好）：{prep_wait} 个请求")
    first_timeout = sum(1 for row in timeline_rows if any(e["msg"].startswith("上游首事件超时") for e in progress[row["id"]]))
    if first_timeout:
        print(f"  \"上游首事件超时\"（FIRST_EVENT_TIMEOUT 生效，换号重试）：{first_timeout} 个请求")
    texts = collections.Counter()
    for row in window_rows:
        error = row["error"]
        for key in ("本服务首事件前期限", "本服务首事件后空闲期限", "本服务总时长上限", "协议错误码 4", "stream closed before finish",
                    "stream closed before first event", "context deadline exceeded", "Deadline expired", "后请求超时", "后请求已取消",
                    "协议位置", "context canceled"):
            if key.lower() in error.lower():
                texts[key] += 1
    print(f"  账本 error 文案命中：{top(texts, 12) if texts else '无'}")
    print("    · 本服务首事件前期限 / 首事件后空闲期限 / 总时长上限 = 本服务自己的期限（新版本，三者都带 context deadline exceeded 结尾）；"
          "error 只有 context deadline exceeded 本身 = 旧版本整段 REQUEST_TIMEOUT 或其他数据请求，带 AI Studio … 协议位置 或 "
          "BiDi … 等待回复超时 的 = 上游或浏览器链路超时；协议错误码 4 / Deadline expired = "
          "Google 侧期限；stream closed before finish = 上游断流；客户端看到的 504 文案都相同（api/official_errors.go）")
    finished_cancel = [row for row in window_rows if row["kind"] == "cancel"]
    print(f"  \"请求已取消\"（request.finished WARN，app/request_logging.go requestOutcome）：{len(finished_cancel)}")
    if quota_after_output:
        mine = [e for e in quota_after_output if any(r["account"] == e["source"] and abs((r["end"] - e["t"]).total_seconds()) < 30
                                                     for r in window_rows)]
        print(f"  \"账号冷却 | … | 已输出内容后\"（流内 429，无法换号）：全部 {len(quota_after_output)} 条，其中对得上 {MODEL} 请求的 {len(mine)}")
    else:
        print("  \"账号冷却 | … | 已输出内容后\"：无")
    # 生命周期：重启、改配置、停止都会打断进行中的长请求（state=cancelled，看起来和客户端断开一样）
    if lifecycle:
        print("  服务生命周期事件：")
        for entry in lifecycle[-10:]:
            print(f"    {local(entry['t'])}  {entry['msg'][:90]}")
        hit = []
        for row in window_rows:
            if row["success"]:
                continue
            for entry in lifecycle:
                # 请求在事件前 5 秒到事件后 95 秒内结束（应用新配置等 30s、收到停止信号等 SHUTDOWN_GRACE 默认 60s，再加余量）
                if -5 <= (row["end"] - entry["t"]).total_seconds() <= 95:
                    if any(key in entry["msg"] for key in ("收到停止信号", "等待超时", "应用新配置", "生成服务已停止")):
                        hit.append(row)
                        break
        if hit:
            print(f"    ⚠ {len(hit)} 个失败/取消在停止、改配置重启前后结束（应用新配置最多等 30s，收到停止信号最多等 SHUTDOWN_GRACE=60s）")
            VERDICTS.append(f"{len(hit)} 个 {MODEL} 请求在服务停止/改配置重启时被打断（第 4 段生命周期），这类在账本里也记为 499 取消")
    vanished = []
    running_ids = {item.get("id") for item in active}
    for rid, item in log_req.items():
        data = item.get("data_started") or {}
        if item.get("started") and not item.get("finished") and rid not in running_ids and rid not in model_ids \
                and is_model(data.get("model")) and item["started"] >= SINCE and (NOW - item["started"]).total_seconds() > TOTAL_CAP + 120:
            vanished.append(item["started"])
    if vanished:
        print(f"  ⚠ 日志里开始了、却没有结束记录也不在账本的 {MODEL} 请求：{len(vanished)}（进程被杀/重启时进行中的请求不会写账本）")
    # 失败前后同账号的告警（Worker、浏览器、代理等），帮助判断"上游中途断开"的来源
    near_warn = collections.Counter()
    for row in window_rows:
        if row["success"] or not row["account"] or not row["first"]:
            continue
        seen = set()
        for entry in account_warn.get(row["account"], []):
            delta = (entry["t"] - row["end"]).total_seconds()
            if -15 <= delta <= 5:
                key = entry["msg"].split(" | ")[0][:40]
                if key not in seen:
                    seen.add(key)
                    near_warn[key] += 1
    if near_warn:
        print(f"  已输出内容后失败的请求，结束前 15 秒到后 5 秒同账号的告警：{top(near_warn, 8)}")

# ================= 4b. 正在进行的请求 =================
running = [item for item in active if is_model(item.get("model"))]
if running:
    section(f"4b. 正在进行的 {MODEL} 请求（/api/requests）")
    for item in running[:SAMPLES or 8]:
        started = parse_time(item.get("started_at"))
        elapsed = (NOW - started).total_seconds() if started else None
        flag = ""
        if elapsed and elapsed >= TOTAL_CAP - 300:
            flag = "  ⚠ 快到总时长上限"
        elif elapsed and elapsed >= TIMEOUT:
            flag = "  已超过 REQUEST_TIMEOUT（新版本开始输出后不再按它截断）"
        print(f"  {short_id(item.get('id'))}  {item.get('state')}  已运行 {secs(elapsed)}  账号 {mask(item.get('account_label'))}{flag}")

# ================= 5. 样例 =================
samples = [row for row in window_rows if not row["success"]
           and (row["first"] or row["near_timeout"] or row["kind"] in CUT_FOR_RETRY)]
if samples and SAMPLES:
    section("5. 截断与已输出内容后失败的样例（可在日志里按请求 ID 搜完整时间线）")
    for row in sorted(samples, key=lambda r: r["end"], reverse=True)[:SAMPLES]:
        attempts = f" 服务端失败尝试 {len(row['attempts'])}" if row["attempts"] else ""
        print(f"  {local(row['start'])} {row['id']}  {row['stream']}  账号 {mask(row['account'])} 通道 {row['channel'] or '-'}{attempts}")
        print(f"    排队 {secs(row['queue'])} → 首事件 {secs(row['first'])} → 结束 {secs(row['duration'])}  {row['class']}"
              + (f"  上游字节 {row['upstream_bytes']}" if row["upstream_bytes"] else "")
              + (f"  停顿 {row['stalls']} 次" + (f"（恢复过的最长 {secs(row['stall_max'])}）" if row["stall_max"] else "") if row["stalls"] else ""))
        if row["stall_at_end"]:
            print(f"    {row['stall_at_end']}")
        if row["cut_log"]:
            print(f"    进度：{row['cut_log']['msg'][:200]}")
        if row["error"]:
            print(f"    错误：{row['error'][:200]}")
        body = bodies.get(row["id"])
        if body:
            ending = response_ending(body)
            if ending:
                print(f"    {ending}")

# ================= 6. 结论 =================
section("6. 结论（自动推断，请结合上面的数字核对）")
if window_rows:
    bad = [row for row in window_rows if not row["success"]]
    service_cuts = sum(1 for row in bad if row["kind"] in SERVICE_CUT_KINDS)
    VERDICTS.insert(0, f"{MODEL}：最近 {MINUTES} 分钟 {len(window_rows)} 个请求，失败/取消 {share(len(bad), len(window_rows))}，"
                       f"其中已输出内容后 {sum(1 for row in bad if row['first'])}，本服务期限截断 {service_cuts}"
                       f"（首事件前 {len(start_rows)}、首事件后空闲 {len(idle_rows)}、总时长上限 {len(total_rows)}）")
    upstream_cut = [row for row in window_rows if row["kind"] in ("proto_break", "stream_closed")]
    if upstream_cut:
        VERDICTS.append(f"{len(upstream_cut)} 个被上游/浏览器链路中途断开（{top(collections.Counter(r['class'] for r in upstream_cut), 3)}），"
                        f"耗时 {dist([r['duration'] for r in upstream_cut])}：看第 4 段同账号告警与第 5 段样例")
if not VERDICTS:
    VERDICTS.append("没有发现明显异常；如果问题仍在，把整段输出发给开发者")
for verdict in VERDICTS:
    print("  - " + verdict)
print()
print("把从「数据来源」开始的全部输出复制发给开发者即可（账号已打码；不含密钥、提示词与回复原文）")
PY

MINUTES="$MINUTES" MODEL="${MODEL:-}" RETRY_GAP="$RETRY_GAP" SAMPLES="$SAMPLES" DIAG_NOW="${DIAG_NOW:-}" DB_PATH="$DB" \
  LOG_PATHS="$LOG.1
$LOG" CONFIG_JSON="$WORK/config.json" CONFIG_SOURCE="$CONFIG_SOURCE" REQUESTS_JSON="$WORK/requests.json" WORK_DIR="$WORK" API_STATE="$API_STATE" \
  NGINX_TXT="$WORK/nginx.txt" NGINX_DIRS="$NGINX_DIRS" REQUEST_TIMEOUT_OVERRIDE="${DIAG_REQUEST_TIMEOUT:-}" \
  ENV_REQUEST_TIMEOUT="$ENV_REQUEST_TIMEOUT" ENV_FIRST_EVENT_TIMEOUT="$ENV_FIRST_EVENT_TIMEOUT" \
  python3 -I "$WORK/diag_regen.py"
