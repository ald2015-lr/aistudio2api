#!/usr/bin/env bash
# AIStudio2API 一键诊断"号池繁忙 429 / 测试调用不返回数据"：配置与容量 → 请求结果 → 号池繁忙与排队快照 → 上游 429 →
# 连续限流递增冷却 → 裸 Quota exceeded 逐账号 → 驻留 Worker 的冷却与变动 → 每分钟上限 → 结论
# 只读：只对本机管理接口发 GET 请求，只读运行日志、用量账本（SQLite 以只读方式打开）与每日额度通道判定，
# 不改任何配置、冷却、账号或 Worker；临时文件放在 mktemp -d 建的目录里，退出时删除（建不了时直接退出）。
# python3 没有 sqlite3 模块时不读用量账本，只分析运行日志。
#
# 用法（放在程序目录的 tools/ 下运行，或用 APP_DIR 指定程序目录）：
#   bash tools/diag_busy.sh
#   MINUTES=120 bash tools/diag_busy.sh          统计最近 120 分钟（默认 60；第 5 段另按 QE_MINUTES 回看）
#   MODEL=<模型> bash tools/diag_busy.sh          重点分析的模型（默认取 PRIMARY_MODELS、ULTRA_PRIMARY_MODELS 中号池繁忙最多的一个，
#                                                 都没有设置时取号池繁忙或请求最多的模型）
#   QE_MINUTES=720 bash tools/diag_busy.sh        第 5 段（裸 Quota exceeded 逐账号）回看的分钟数（默认取 MINUTES 与 360 中较大者）
#   WITHIN=5,15,60 bash tools/diag_busy.sh        第 5 段"出现后 X 分钟内再成功"的 X（分钟，逗号分隔）；最大值为判定窗口：只对第一次出现在
#                                                 这么久以前的账号下结论（太平洋时间零点前任何时候再成功都算恢复，不限于窗口内），
#                                                 短于 40 分钟（第 3 档 30 分钟冷却加余量）时不下结论
#   ACCOUNTS=0 bash tools/diag_busy.sh            不读取 /api/accounts（账号上万时这个接口较大；读取后第 5 段可按权益分组）
#   OFFLINE=1 bash tools/diag_busy.sh             不访问管理接口，只分析运行日志与用量账本
#   APP_DIR=/path/to/app bash tools/diag_busy.sh  指定程序目录（含 start.sh、.env、logs/、runtime/ 的目录）
#   LOG_FILE=… LEDGER_DB=… DIAG_NOW=2026-10-22T08:00:00Z OFFLINE=1 bash tools/diag_busy.sh
#                                                 分析排查包里的运行日志与账本副本，"现在"按 DIAG_NOW（RFC 3339）计
set -u
script_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd)"
if [ -z "${APP_DIR:-}" ]; then
  for candidate in "$script_dir/.." "$script_dir" "$PWD" /www/wwwroot/Chat2API/build; do
    if [ -f "$candidate/start.sh" ]; then APP_DIR="$(cd "$candidate" && pwd)"; break; fi
  done
fi
APP_DIR="${APP_DIR:-/www/wwwroot/Chat2API/build}"
MINUTES="${MINUTES:-60}"
LOG="${LOG_FILE:-$APP_DIR/logs/aistudio2api.log}"
DB="${LEDGER_DB:-$APP_DIR/runtime/requests.db}"
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
# 每日额度通道判定（.quota-sharing.json）保存在第一个账户目录（AISTUDIO_AUTH_STATES，默认 auth）
auth_dir="$(grep -E '^AISTUDIO_AUTH_STATES=' "$APP_DIR/.env" 2>/dev/null | tail -n 1 | cut -d= -f2- | cut -d, -f1 | tr -d '"[:space:]')"
auth_dir="${auth_dir:-auth}"
case "$auth_dir" in /*) ;; *) auth_dir="$APP_DIR/$auth_dir" ;; esac
command -v python3 >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }
command -v curl >/dev/null || { echo "需要 curl：apt install -y curl"; exit 1; }
case "$MINUTES" in "" | *[!0-9]* | 0) echo "MINUTES 必须是正整数"; exit 1 ;; esac
case "${QE_MINUTES:-}" in "" | *[!0-9]*) QE_MINUTES="" ;; esac
# mktemp 失败（TMPDIR 不存在、磁盘已满）时 WORK 为空，临时文件会写到根目录且不会清理：直接退出
WORK="$(mktemp -d)" && [ -n "$WORK" ] && [ -d "$WORK" ] || { echo "无法创建临时目录（检查 TMPDIR 与磁盘空间）"; exit 1; }
trap 'rm -rf "$WORK"' EXIT
section() { printf '\n==================== %s ====================\n' "$1"; }

# fetch 路径 超时秒数 输出文件：只发 GET；失败时输出文件为空并说明原因。连不上或令牌被拒绝时不再访问其他接口
API_STATE="unknown"
[ "${OFFLINE:-0}" = "1" ] && API_STATE="offline"
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
echo "  程序目录：$APP_DIR    管理接口：$API$([ "$API_STATE" = "offline" ] && echo "（OFFLINE=1，不访问）")"
echo "  现在：$(date '+%F %T %Z')（UTC $(date -u '+%T')）$([ -n "${DIAG_NOW:-}" ] && echo "；分析按 DIAG_NOW=$DIAG_NOW 计")"
fetch /api/status 20 status.json
fetch /api/config 10 config.json
fetch /api/cooldowns 60 cooldowns.json
fetch /api/requests 15 requests.json
if [ "${ACCOUNTS:-1}" != "0" ]; then fetch /api/accounts 60 accounts.json; fi

cat > "$WORK/diag_busy.py" <<'PY'
import collections
import datetime
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
MINUTES = max(1, int(ENV.get("MINUTES") or 60))
QE_MINUTES = int(ENV.get("QE_MINUTES") or 0) or max(MINUTES, 360)
WITHIN = sorted({int(x) for x in re.split(r"[,\s]+", ENV.get("WITHIN") or "") if x.isdigit() and int(x) > 0}) or [5, 15, 60]
SUCCESS = ("completed", "tool_calls", "limited")
API_STATE = ENV.get("API_STATE") or "unknown"
VERDICTS = []
# 连续裸限流的冷却档位（app/rate_limit_guard.go）：第 5 档起冷却到太平洋时间次日零点，每档都不超过次日零点
LADDER = {1: "30 秒", 2: "5 分钟", 3: "30 分钟", 4: "2 小时", 5: "到次日零点"}
# 第 5 段的判定：窗口（WITHIN 的最大值）不能短于第 3 档冷却加余量；第一次出现这么久以后仍失败视为歇过第 3 档仍失败
JUDGE_MIN_MINUTES = 40
STILL_FAILING = TD(minutes=30)

TIME_RE = re.compile(r'^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$')


def parse_time(value):
    # Go 的时间：RFC 3339，0～9 位小数秒，时区为 Z 或 ±hh:mm（每日额度恢复时间按太平洋时间，常见 -07:00/-08:00）
    match = TIME_RE.match((value or "").strip())
    if not match:
        return None
    fraction = (match.group(2) or "0")[:6].ljust(6, "0")
    zone = "+00:00" if match.group(3) == "Z" else match.group(3)
    try:
        return datetime.datetime.fromisoformat(f"{match.group(1)}.{fraction}{zone}")
    except ValueError:
        return None


NOW = parse_time(ENV.get("DIAG_NOW") or "") or datetime.datetime.now(UTC)
UNTIL = NOW + TD(minutes=1)
SINCE = NOW - TD(minutes=MINUTES)
QE_SINCE = NOW - TD(minutes=QE_MINUTES)
EARLIEST = min(SINCE, QE_SINCE)


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


def top(counter, limit=6, total=None):
    items = counter.most_common(limit)
    text = "，".join(f"{key} {share(value, total) if total else value}" for key, value in items)
    rest = sum(counter.values()) - sum(value for _, value in items)
    return text + (f"，其他 {rest}" if rest > 0 else "") if text else "-"


def local(moment):
    return moment.astimezone().strftime("%m-%d %H:%M:%S") if moment else "-"


def mask(label):
    # 账号只显示前两位与域名，输出可以直接发给开发者
    label = label or ""
    name, at, domain = label.partition("@")
    return (name[:2] + "***" + at + domain) if at else (label[:4] + "***" if len(label) > 4 else label)


def ms_to_time(value):
    return datetime.datetime.fromtimestamp((value or 0) / 1000, UTC)


def go_duration(text):
    # Go 的 time.Duration 文本（850ms、10.4s、1m2.5s）转秒
    parts = re.findall(r"([0-9]*\.?[0-9]+)(ns|us|µs|ms|h|m|s)", text or "")
    if not parts:
        return None
    scale = {"ns": 1e-9, "us": 1e-6, "µs": 1e-6, "ms": 1e-3, "s": 1, "m": 60, "h": 3600}
    return sum(float(number) * scale[unit] for number, unit in parts)


def model_key(model):
    model = (model or "").strip().lower()
    return model[len("models/"):] if model.startswith("models/") else model


def bare_scope(scope):
    # 冷却范围：Playground 为模型 ID，Build 为 build:<模型>，全局为“全局”
    scope = scope or ""
    return scope[len("build:"):] if scope.startswith("build:") else scope


def matches(model, target):
    # 成功的请求记录上游标明的模型名，可能带版本后缀（如 -0219），一并算作同一个模型
    key = model_key(model)
    return bool(target) and (key == target or re.fullmatch(re.escape(target) + r"-\d[\w.]*", key) is not None)


MODEL = ""


def is_model(model):
    return matches(model, MODEL)


def level_of(text):
    match = re.search(r"第 (\d+) 档", text or "")
    return int(match.group(1)) if match else None


def ladder_name(level):
    return f"第 {level} 档（{LADDER.get(min(level, 5), '?')}）"


def number(info, key):
    value = (info.get(key) or "0").strip()
    return int(value) if value.isdigit() else 0


try:
    from zoneinfo import ZoneInfo
    PACIFIC = ZoneInfo("America/Los_Angeles")
except Exception:
    PACIFIC = None


def next_pacific_midnight(moment):
    # 每日额度在太平洋时间零点恢复；没有时区库时按美国夏令时规则推算
    if PACIFIC is not None:
        day = moment.astimezone(PACIFIC)
        return (datetime.datetime(day.year, day.month, day.day, tzinfo=PACIFIC) + TD(days=1)).astimezone(UTC)
    year = moment.year
    march = datetime.datetime(year, 3, 8, 10, tzinfo=UTC)
    march += TD(days=(6 - march.weekday()) % 7)
    november = datetime.datetime(year, 11, 1, 9, tzinfo=UTC)
    november += TD(days=(6 - november.weekday()) % 7)
    offset = TD(hours=-7 if march <= moment < november else -8)
    day = moment + offset
    return datetime.datetime(day.year, day.month, day.day, tzinfo=UTC) + TD(days=1) - offset


def load_json(path):
    try:
        with open(path, encoding="utf-8") as handle:
            return json.load(handle)
    except Exception:
        return None


# ---------- 错误分类 ----------
Q_MIN, Q_DAY, Q_BARE, Q_RES, Q_INPUT, Q_QUOTA, Q_OTHER = (
    "分钟限额", "每日限额", "裸 Quota exceeded", "Resource exhausted", "输入超长（不是额度）", "其他额度文案", "其他 429")
BUILD_METHODS = ("ProxyStreamedCall", "ProxyUnaryCall")


def upstream_message(text):
    # 上游错误形如"AI Studio <方法> 返回 HTTP 429、协议错误码 8: <原文>"（aistudio/client.go RPCError.Error），只取原文第一行
    match = re.search(r"返回 HTTP \d{3}(?:、协议错误码 -?\d+)?: (.*)", text or "")
    message = match.group(1) if match else (text or "")
    message = re.sub(r"\(qos=[^)]*\)|\[original: [^\]]*\]", "", message.split("\n")[0])
    return message.strip()


def upstream_channel(text):
    # Build 通道经 ProxyStreamedCall（需要订阅权益的模型经 ProxyUnaryCall）发送，其余为 Playground
    match = re.search(r"AI Studio (\w+) 返回 HTTP", text or "")
    if not match:
        return "?"
    return "build" if match.group(1) in BUILD_METHODS else "playground"


def quota_class(text):
    # 与 aistudio/quota.go 的判断对应（账本只有错误文案，没有 ErrorInfo 元数据与 RetryInfo，按文案近似）：
    # 带每分钟/每日周期的分别归入分钟/每日限额；只有"Quota exceeded"的是裸文案，按限流冷却并逐级加长（第 4 段）
    lower = (text or "").lower()
    if ("input token count" in lower and "exceed" in lower) or "exceeds the maximum number of tokens allowed" in lower:
        return Q_INPUT
    message = upstream_message(text).lower()
    if any(key in message for key in ("/min/", "perminute", "per_min", "per minute")):
        return Q_MIN
    # "reached your usage limit" 为 pro/ultra 号的每日限额文案（You've reached your usage limit. Retry with an alternative Gemini model…）
    if any(key in message for key in ("/day/", "perday", "per_day", "per day", "daily limit", "try again tomorrow",
                                      "you exceeded your current quota", "reached your usage limit")):
        return Q_DAY
    if message.rstrip(" .。!") == "quota exceeded":
        return Q_BARE
    if "resource has been exhausted" in message or "resource_exhausted" in message or "resource exhausted" in message:
        return Q_RES
    if "quota" in message:
        return Q_QUOTA
    return Q_OTHER


def short_text(text, limit=50):
    text = upstream_message(text) if "返回 HTTP" in (text or "") else (text or "").split("\n")[0]
    text = re.sub(r"\d+", "N", text).strip(" :;，。")
    return text[:limit] or "(无详情)"


def attempt_class(error):
    error = error or ""
    lower = error.lower()
    if "账户已进入冷却" in error:
        return "发出前已冷却（没有发到上游）"
    if "HTTP 429" in error:
        return "429·" + quota_class(error)
    if "HTTP 401" in error:
        return "401 登录失效"
    if "HTTP 403" in error or "permission_denied" in lower:
        return "403 无权限"
    if "HTTP 400" in error:
        return "400·" + short_text(error, 40)
    if re.search(r"HTTP 5\d\d", error):
        return "5xx 上游错误"
    if "首事件超时" in error or "deadline exceeded" in lower or "timeout" in lower or "超时" in error:
        return "超时"
    if "bidi" in lower or "closed network" in lower or "worker" in lower or "waa" in lower:
        return "浏览器/Worker 失败"
    return "其他·" + short_text(error, 40)


def row_class(row):
    # 请求最终结果的分类（成功返回 None）
    state, status, error = row["state"], row["status"] or 0, row["error"] or ""
    if state in SUCCESS:
        return None
    if state == "cancelled" or status == 499:
        return "客户端取消"
    if state == "blocked":
        return "被上游中止（安全审核等）"
    if "号池繁忙" in error:
        return "号池繁忙 429（排队超上限）"
    if "额度不足：" in error and "主用模型" in error:
        return "Ultra 主用模型额度不足 429"
    if "可用账户均在冷却" in error:
        return "全部冷却 429"
    if "账户已进入冷却" in error:
        return "发出前已冷却 429"
    if status == 429 or "HTTP 429" in error:
        return "上游 429·" + quota_class(error)
    if status == 504 or "deadline exceeded" in error.lower() or "超时" in error:
        return "超时 504" + ("（排队中）" if "排队" in error else "")
    if status == 400:
        return "400·" + short_text(error, 40)
    if status == 403:
        return "403 无权限"
    if status == 503:
        return "503 号池不可用"
    if status >= 500:
        return f"{status} 上游/代理错误"
    return f"HTTP {status}·" + short_text(error, 30)


def busy_fields(error):
    # 号池繁忙文案（aistudio/accounts.go CapacityBusyError.Error）：已排队、上限、总耗时、现场冷启动、第几次尝试、最后等待原因
    if "号池繁忙" not in (error or ""):
        return None

    def grab(pattern):
        match = re.search(pattern, error)
        return match.group(1) if match else None
    attempt = grab(r"第 (\d+) 次尝试")
    return {
        "waited": go_duration(grab(r"已排队 ([^，）]+)")), "limit": go_duration(grab(r"上限 ([^，）]+)")),
        "elapsed": go_duration(grab(r"总耗时 ([^，）（]+)")), "cold": go_duration(grab(r"含现场冷启动 ([^）]+)")),
        "attempt": int(attempt) if attempt else None, "reason": (grab(r"最后等待原因：([^）\n]+)") or "未知").strip(),
    }


def cooldown_kind(reason):
    # 冷却原因为"类型: 原文"（aistudio/quota.go），连续裸限流写成"限流: 连续限流第 N 档，原文"（aistudio/rate_strike.go）
    reason = reason or ""
    level = re.search(r"连续限流第 (\d+) 档", reason[:40])
    if level:
        return "限流·" + ladder_name(int(level.group(1)))
    head = reason.split(":", 1)[0] if ":" in reason[:40] else ""
    if head:
        return head
    if "自动暂停" in reason:
        return "403 自动暂停"
    return "其他"


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


def load_rows(connection):
    # requests 表（internal/requestdb/requestdb.go）：time 为完成时间（Unix 毫秒），*_ms 为毫秒；attempts 为最终结果之前每次失败的上游尝试
    # （JSON 数组，元素含 account、channel、error、duration_ms，见 api/ledger.go RequestAttempt）；pool 为号池（ultra，普通为空），
    # 旧账本没有 pool 列时按空读
    columns = ("id, time, protocol, model, account, channel, status, state, duration_ms, first_event_ms, queue_ms, "
               "reasoning_tokens, reply_tokens, tool_calls, error, attempts")
    query = f"SELECT {columns}, pool FROM requests WHERE time >= ? AND time <= ? ORDER BY time"
    bounds = (int(EARLIEST.timestamp() * 1000), int(UNTIL.timestamp() * 1000))
    try:
        cursor = connection.execute(query, bounds)
    except sqlite3.OperationalError:
        cursor = connection.execute(query.replace(", pool FROM", ", '' FROM"), bounds)
    rows = []
    for (rid, at, protocol, model, account, channel, status, state, duration, first, queue,
         reasoning, reply, tools, error, attempts, pool) in cursor:
        try:
            attempts = json.loads(attempts or "[]") or []
        except ValueError:
            attempts = []
        end = ms_to_time(at)
        rows.append({
            "id": rid, "end": end, "start": end - TD(milliseconds=duration or 0), "protocol": protocol or "",
            "model": model_key(model), "account": account or "", "channel": channel or "", "status": status or 0,
            "state": state or "", "duration": (duration or 0) / 1000, "first": (first or 0) / 1000,
            "queue": (queue or 0) / 1000, "reasoning": reasoning or 0, "reply": reply or 0, "tools": tools or 0,
            "error": error or "", "attempts": [a for a in attempts if isinstance(a, dict)], "pool": pool or "",
        })
    latest = connection.execute("SELECT max(time) FROM requests WHERE time <= ?", (bounds[1],)).fetchone()[0]
    return rows, (ms_to_time(latest) if latest else None)


# ---------- 运行日志（logs/aistudio2api.log，每行一个 JSON 事件：time、level、msg、event、source、request） ----------
LOG_TIME_RE = re.compile(rb'"time":\s*"([^"]+)"')
LOG_KEYS = [key.encode() for key in (
    "账号冷却", "账号切换", "账号每分钟请求已满", "排队等待 |", "主用模型", "限流档位清零", "号池级限流",
    "Worker 就绪", "Worker 启动失败", "按需扩容", "按需替换", "空闲回收", "冷却轮换", "冷却淘汰", "补登记淘汰", "排空",
    "Worker 重建", "Worker 停止 |", "替补", "回收冷却占位", "常驻 Worker 已失败",
    "管理服务就绪", "收到停止信号", "排队进度日志已省略", "请求账本写入队列已满", "控制台队列已满", "额度 429 详情未识别")]


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


def read_logs(paths, since, want_finished):
    keys = LOG_KEYS + ([b'"request.finished"'] if want_finished else [])
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
                if not any(key in raw for key in keys):
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


def fields(message):
    # "名称 | 键=值 | 键=值" → {键: 值}（只取第一行；没有等号的部分如"号池级限流不升档"不在这里，按子串判断）
    result = {}
    for part in message.split("\n")[0].split(" | ")[1:]:
        key, sep, value = part.partition("=")
        if sep:
            result[key.strip()] = value.strip()
    return result


# ================= 读取数据 =================
status = load_json(ENV.get("STATUS_JSON", "")) or {}
config = load_json(ENV.get("CONFIG_JSON", "")) or {}
cooldown_doc = load_json(ENV.get("COOLDOWNS_JSON", ""))
cooldowns = (cooldown_doc or {}).get("cooldowns") or []
active_doc = load_json(ENV.get("REQUESTS_JSON", ""))
active_requests = (active_doc or {}).get("requests") or []
accounts = (load_json(ENV.get("ACCOUNTS_JSON", "")) or {}).get("accounts") or []
tier_of = {item.get("label"): (item.get("benefit_tier") or "?") + ("" if item.get("benefit_tier_known", True) else "(未知)")
           for item in accounts if item.get("label")}
sharing_doc = load_json(os.path.join(ENV.get("AUTH_DIR") or "", ".quota-sharing.json")) if ENV.get("AUTH_DIR") else None

connection, db_note = open_db(ENV.get("DB_PATH", ""))
rows, db_latest = [], None
if connection is not None:
    try:
        rows, db_latest = load_rows(connection)
    except Exception as exc:
        db_note = f"读取用量账本出错：{exc}"
        connection = None
log_paths = [path for path in (ENV.get("LOG_PATHS") or "").split("\n") if path]
logs, coverage = read_logs(log_paths, EARLIEST, connection is None)
if connection is None:
    # 没有账本时退回到日志里的 request.finished（没有逐次尝试，第 3、5 段改用"账号切换"日志行）
    for entry in logs:
        if entry["event"] != "request.finished":
            continue
        request = entry["request"]
        duration = (request.get("duration_ms") or 0) / 1000
        rows.append({
            "id": request.get("id"), "end": entry["t"], "start": entry["t"] - TD(seconds=duration), "protocol": "",
            "model": model_key(request.get("model")),
            "account": entry["source"] if entry["source"] not in ("service", "request") else "",
            "channel": request.get("channel") or "", "status": request.get("status") or 0, "state": request.get("state") or "",
            "duration": duration, "first": (request.get("first_event_ms") or 0) / 1000,
            "queue": (request.get("queue_ms") or 0) / 1000, "reasoning": (request.get("usage") or {}).get("reasoning_tokens") or 0,
            "reply": (request.get("usage") or {}).get("reply_tokens") or 0, "tools": request.get("tool_calls") or 0,
            "error": request.get("error") or "", "attempts": [], "pool": request.get("pool") or "",
        })
for row in rows:
    row["success"] = row["state"] in SUCCESS
    row["class"] = row_class(row)
model_of_request = {row["id"]: row["model"] for row in rows if row.get("id")}
window_rows = [row for row in rows if row["end"] >= SINCE]


def pick_model():
    # 重点模型：MODEL 指定的；否则在两份主用模型列表里取窗口内号池繁忙最多的（都没有时取第一个）；没有主用模型时取号池繁忙或请求最多的
    # （窗口内没有请求时按第 5 段的回看范围找）
    explicit = model_key(ENV.get("MODEL"))
    if explicit:
        return explicit, "MODEL 指定"
    busy = [row["model"] for row in window_rows if (row["class"] or "").startswith("号池繁忙")]
    listed = []
    for key, name in (("primary_models", "PRIMARY_MODELS"), ("ultra_primary_models", "ULTRA_PRIMARY_MODELS")):
        for model in config.get(key) or []:
            if model_key(model) and model_key(model) not in [m for m, _ in listed]:
                listed.append((model_key(model), name))
    if listed:
        counts = [(sum(1 for model in busy if matches(model, candidate)), candidate, name) for candidate, name in listed]
        best = max(counts, key=lambda item: item[0])
        if best[0] > 0:
            return best[1], f"{best[2]} 中号池繁忙最多（{best[0]} 个）"
        return listed[0][0], f"{listed[0][1]} 的第一个"
    if busy:
        model, count = collections.Counter(busy).most_common(1)[0]
        return model, f"号池繁忙最多的模型（{count} 个）"
    for data, scope in ((window_rows, ""), (rows, f"，回看 {QE_MINUTES} 分钟")):
        requested = collections.Counter(row["model"] for row in data if row["model"]).most_common(1)
        if requested:
            return requested[0][0], f"请求最多的模型（{requested[0][1]} 个{scope}）"
    return "", "没有可分析的请求"


MODEL, model_reason = pick_model()
model_rows = [row for row in window_rows if is_model(row["model"])]

print(f"  统计窗口：最近 {MINUTES} 分钟（第 5 段回看 {QE_MINUTES} 分钟）    重点模型：{MODEL or '-'}（{model_reason}；可用 MODEL=<模型> 指定）")
print(f"  用量账本：{ENV.get('DB_PATH')}" + (f"（{db_note}）" if db_note else "")
      + (f"，窗口内记录 {len(window_rows)} 条，最新一条 {local(db_latest)}" if connection is not None else ""))
if connection is not None and db_latest and (NOW - db_latest).total_seconds() > 600:
    print(f"  ⚠ 账本最新一条已是 {(NOW - db_latest).total_seconds() / 60:.0f} 分钟前：服务可能没在处理请求，或账本写入失败")
for path, first, last in coverage:
    if last and last < EARLIEST:
        print(f"  运行日志：{path}（最后一行 {local(last)}，全部早于统计窗口，跳过）")
    else:
        print(f"  运行日志：{path}（从 {local(first)} 读起，最后一行 {local(last)}）")
if not coverage:
    print("  运行日志：找不到 " + "、".join(log_paths) + "，来自日志的统计（排队快照、冷却档位、Worker 变动等）会缺失")
print(f"  日志中与本诊断相关的事件：{len(logs)} 条（时间范围内）")
if API_STATE in ("down", "denied", "offline"):
    print("  管理接口不可用：第 0 段的配置与容量、第 4 段的当前冷却、第 6 段的驻留 Worker 核对会缺失")
omitted = sum(int((re.search(r"已省略 (\d+) 条", e["msg"]) or [0, 0])[1]) for e in logs if e["t"] >= SINCE and "排队进度日志已省略" in e["msg"])
dropped = sum(int((re.search(r"丢弃=(\d+)", e["msg"]) or [0, 0])[1]) for e in logs if e["t"] >= SINCE and "请求账本写入队列已满" in e["msg"])
console_dropped = sum(int((re.search(r"丢弃=(\d+)", e["msg"]) or [0, 0])[1]) for e in logs if e["t"] >= SINCE and "控制台队列已满" in e["msg"])
print("  说明：控制台日志在缓冲满时会丢条（app/admin.go 控制台队列 4096 条，满了直接丢，每 10 秒检查一次、有丢弃时汇总一行“控制台队列已满”；“额度 429 详情未识别”等少数几种不丢），计数以账本为准，日志只看比例与文案")
if console_dropped:
    print(f"  ⚠ 控制台队列满，运行日志丢弃 {console_dropped} 条：来自日志的统计偏少")
if omitted:
    print(f"  排队进度日志被全局限流省略 {omitted} 条（每秒最多 2 条）")
if dropped:
    print(f"  ⚠ 用量账本写入队列满，丢弃 {dropped} 条记录：账本计数偏少")

# ================= 0. 配置与当前容量 =================
section("0. 版本、配置与当前容量")
workers = status.get("workers") or {}
ultra_workers = status.get("ultra_workers") or {}
account_counts = status.get("accounts") or {}
rpm_limit = config.get("account_rpm_limit")
concurrency = config.get("per_account_concurrency")
supplies = [s for s in status.get("primary_supply") or [] if isinstance(s, dict)]
model_supplies = [s for s in supplies if model_key(s.get("model")) == MODEL]
if status:
    print(f"  服务：{status.get('state')}  版本 {status.get('version')}  活动请求 {status.get('active_requests')}")
    print(f"  账户：共 {account_counts.get('total')}，就绪 {account_counts.get('ready')}，忙碌 {account_counts.get('busy')}，"
          f"冷却 {account_counts.get('cooldown')}，需要登录 {account_counts.get('auth_required')}")
    print(f"  普通分区 Worker：就绪 {workers.get('warm')} / 预热目标 {workers.get('target')} / 峰值 {workers.get('max')}，"
          f"启动中 {workers.get('starting')}，占用槽位 {workers.get('occupied')}，死槽位 {workers.get('dead')}，"
          f"排空中 {workers.get('draining')}，待淘汰 {workers.get('retiring')}")
    if ultra_workers.get("max"):
        print(f"  Ultra 分区 Worker：就绪 {ultra_workers.get('warm')} / 预热目标 {ultra_workers.get('target')} / 峰值 {ultra_workers.get('max')}，"
              f"启动中 {ultra_workers.get('starting')}，死槽位 {ultra_workers.get('dead') or 0}，排空中 {ultra_workers.get('draining') or 0}")
    for supply in supplies:
        pool = "Ultra 分区" if supply.get("pool") == "ultra" else "普通分区"
        cooling = (f"，对它冷却中的驻留 Worker {supply.get('cooling')} 个" if "cooling" in supply
                   else "（这个版本的状态接口没有 cooling 字段）")
        print(f"  主用模型供给 {supply.get('model')}（{pool}）：能服务它的驻留 Worker {supply.get('warm')} 个，"
              f"空闲备用账号 {supply.get('standby')}{'+' if supply.get('standby_capped') else ''} 个{cooling}")
    if supplies:
        print("    （约 30 秒统计一次；warm 不含冷却中与排空中的，cooling 为所有支持的启用通道都在冷却的驻留 Worker，含 2 分钟内的短冷却）")
elif API_STATE in ("down", "denied", "offline"):
    print("  管理接口不可用，跳过")
else:
    print("  无法读取 /api/status：端口不对，或管理令牌不对")
if config:
    # GET /api/config 返回已保存的 .env（app/admin.go RuntimeConfig）；单账户并发与每分钟上限保存后热更新，即当前生效值
    rpm_text = "关闭" if rpm_limit == 0 else (f"{rpm_limit} 次/60 秒" if rpm_limit is not None else "未知（旧版没有这个字段）")
    print(f"  单账户并发 PER_ACCOUNT_CONCURRENCY={concurrency}    单账户每分钟上限 ACCOUNT_RPM_LIMIT={rpm_text}")
    print(f"  常驻 WARM_WORKER_LIMIT={config.get('warm_worker_limit')}  峰值 MAX_ACTIVE_WORKERS={config.get('max_active_workers')}  "
          f"冷启动并发 WARM_STARTUP_CONCURRENCY={config.get('warm_startup_concurrency')}  通道 {','.join(config.get('upstream_channels') or [])}  "
          f"选号策略 {config.get('routing_strategy')}")
    print(f"  主用模型 PRIMARY_MODELS={','.join(config.get('primary_models') or []) or '(空)'}  排队上限 PRIMARY_QUEUE_WAIT={config.get('primary_queue_wait')}  "
          f"REQUEST_TIMEOUT={config.get('request_timeout')}  FIRST_EVENT_TIMEOUT={config.get('first_event_timeout')}")
    if config.get("ultra_primary_models"):
        print(f"  Ultra 主用模型 ULTRA_PRIMARY_MODELS={','.join(config.get('ultra_primary_models') or [])}  "
              f"排队上限 ULTRA_PRIMARY_QUEUE_WAIT={config.get('ultra_primary_queue_wait')}")
elif API_STATE not in ("down", "denied", "offline"):
    print("  无法读取 /api/config")
primary_listed = MODEL and MODEL in [model_key(m) for m in (config.get("primary_models") or []) + (config.get("ultra_primary_models") or [])]
normal_supply = next((s for s in model_supplies if s.get("pool") != "ultra"), None) or (model_supplies[0] if model_supplies else None)
if normal_supply and isinstance(rpm_limit, int) and rpm_limit > 0:
    demand = len(model_rows) / MINUTES
    warm = normal_supply.get("warm") or 0
    print(f"  ▶ {MODEL} 的理论上限：能服务它的驻留 Worker {warm} 个 × 每分钟 {rpm_limit} 次 = 每分钟最多 "
          f"{warm * rpm_limit} 次（同时最多 {warm * (concurrency or 1)} 个）；最近 {MINUTES} 分钟平均每分钟到达 {demand:.1f} 个请求")

# ================= 1. 请求结果 =================
section(f"1. 最近 {MINUTES} 分钟的请求结果（" + ("用量账本 runtime/requests.db" if connection is not None else "账本读不到，改用日志里的 request.finished，没有逐次尝试") + "）")
if not window_rows:
    print("  这段时间没有已结束的请求" + ("" if connection is not None else "（账本与日志都读不到）"))
else:
    ok = sum(1 for row in window_rows if row["success"])
    print(f"  全部模型：请求 {len(window_rows)}，成功 {share(ok, len(window_rows))}")
    by_model = collections.defaultdict(list)
    for row in window_rows:
        by_model[(row["model"] or "(无模型)", row["pool"])].append(row)
    ranked = sorted(by_model.items(), key=lambda item: (not is_model(item[0][0]), -len(item[1])))
    for (model, pool), data in ranked[:8]:
        success = sum(1 for row in data if row["success"])
        failures = collections.Counter(row["class"] for row in data if row["class"])
        empty = sum(1 for row in data if row["success"] and not row["reply"] and not row["tools"])
        broken = sum(1 for row in data if row["state"] == "failed" and row["first"])
        label = f"{model}{'（Ultra 号池）' if pool == 'ultra' else ''}"
        print(f"  {label}：请求 {len(data)}，成功 {share(success, len(data))}，失败/取消 {len(data) - success}")
        if failures:
            print(f"    失败原因：{top(failures, 7, len(data))}")
        if empty or broken:
            print(f"    成功但没有正文也没有工具调用（客户端看到空回复）{empty} 个；已开始输出后失败 {broken} 个")
    if len(ranked) > 8:
        print(f"  其余 {len(ranked) - 8} 组模型共 {sum(len(d) for _, d in ranked[8:])} 个请求")

    # 时间分布：看是持续还是阵发
    if model_rows:
        bucket = max(5, MINUTES // 12)
        print()
        print(f"  ▶ {MODEL} 每 {bucket} 分钟：到达请求 / 成功 / 号池繁忙 / 上游 429 / 其他失败")
        counts = collections.OrderedDict()
        start = SINCE
        while start < NOW:
            counts[start] = [0, 0, 0, 0, 0]
            start += TD(minutes=bucket)
        for row in model_rows:
            key = SINCE + TD(minutes=bucket) * int((row["end"] - SINCE).total_seconds() // (bucket * 60))
            if key not in counts:
                continue
            cell = counts[key]
            cell[0] += 1
            if row["success"]:
                cell[1] += 1
            elif row["class"].startswith("号池繁忙"):
                cell[2] += 1
            elif "429" in row["class"]:
                cell[3] += 1
            else:
                cell[4] += 1
        for key, (total, success, busy, quota, other) in counts.items():
            print(f"    {local(key)[6:11]}  {total:>5} / {success:>5} / {busy:>5} / {quota:>5} / {other:>5}")

# ================= 2. 号池繁忙与排队快照 =================
section("2. 号池繁忙 429：最后等待原因、此前的失败与排队超限时的候选快照")
busy_rows = [row for row in window_rows if row["class"] and row["class"].startswith("号池繁忙")]
if not busy_rows:
    print("  账本中窗口内没有号池繁忙 429" + ("" if connection is not None else "（账本读不到）"))
else:
    print(f"  账本中号池繁忙 429 共 {len(busy_rows)} 个（文案见 aistudio/accounts.go CapacityBusyError）")
    model_busy = [row for row in busy_rows if is_model(row["model"])]
    for title, data in (("全部模型", busy_rows), (MODEL, model_busy)):
        if not data or not title:
            continue
        if title == MODEL and len(model_busy) == len(busy_rows):
            print(f"    {MODEL}：全部 {len(data)} 个都是这个模型，明细同上")
        parsed = [(row, busy_fields(row["error"])) for row in data]
        reasons = collections.Counter(info["reason"] for _, info in parsed)
        attempts = collections.Counter(f"第 {info['attempt']} 次" if info["attempt"] else "未写" for _, info in parsed)
        prior = collections.Counter(len(row["attempts"]) for row, _ in parsed)
        prior_classes = collections.Counter(attempt_class(a.get("error")) for row, _ in parsed for a in row["attempts"])
        if title == "全部模型" or len(model_busy) != len(busy_rows):
            pools = collections.Counter("Ultra 号池" if row["pool"] == "ultra" else "普通号池" for row, _ in parsed)
            print(f"    {title}：{len(data)} 个（{top(pools)}）；最后等待原因：{top(reasons, 6, len(data))}")
            print(f"      结束时是第几次尝试：{top(attempts, 5, len(data))}；账本里此前失败的上游尝试数：" +
                  "，".join(f"{k} 次 {share(v, len(data))}" for k, v in sorted(prior.items())[:6]) +
                  ("" if connection is not None else "（日志回退时没有逐次尝试，一律记为 0）"))
            waited = [info["waited"] for _, info in parsed]
            elapsed = [info["elapsed"] for _, info in parsed]
            cold = sum(1 for _, info in parsed if info["cold"])
            burned = [sum((a.get("duration_ms") or 0) for a in row["attempts"]) / 1000 for row, _ in parsed if row["attempts"]]
            print(f"      已排队 中位 {secs(pct(waited, .5))} / P90 {secs(pct(waited, .9))}；总耗时 中位 {secs(pct(elapsed, .5))} / "
                  f"P90 {secs(pct(elapsed, .9))}；含现场冷启动 {cold} 个")
            if burned:
                print(f"      有此前失败尝试的 {len(burned)} 个：失败尝试合计耗时 中位 {secs(pct(burned, .5))} / P90 {secs(pct(burned, .9))}"
                      f"（这段时间计入排队上限）；这些失败尝试的原因：{top(prior_classes, 5)}")
        if title == MODEL:
            pure = prior.get(0, 0)
            VERDICTS.append(
                f"{MODEL} 的号池繁忙 {len(data)} 个中，{share(pure, len(data))} 没有发出过任何上游请求（纯排队，最多的原因"
                f"“{reasons.most_common(1)[0][0]}”）" + (f"，其余在排队上限内先遇到了上游失败（最多的是 {prior_classes.most_common(1)[0][0]}）"
                                                     if prior_classes else ""))


def busy_cause(info):
    # 按排队超限这一刻的候选快照归因（app/queue_progress.go queueSnapshot，冷却中的候选不计入热/备用，驻留冷却来自主用模型供给缓存）
    idle, busy = number(info, "热空闲"), number(info, "热忙碌")
    standby, standby_busy, full = number(info, "备用"), number(info, "备用忙碌"), number(info, "每分钟已满")
    cooling = number(info, "驻留冷却") if "驻留冷却" in info else None
    if cooling and cooling >= idle + busy:
        return "驻留 Worker 大多对它冷却（驻留冷却 ≥ 热空闲+热忙碌）"
    if busy and full * 2 >= busy:
        return "热 Worker 多半每分钟已满（ACCOUNT_RPM_LIMIT）"
    if info.get("启动中") == "是":
        return "等 Worker 启动"
    # 快照里的热空闲已排除本请求失败过的账号（acquireLease 分类时按 ExcludedAccountIDs 与失败的 Worker 排除），
    # 有热空闲时会立即尝试取租约：仍超限说明分类到取租约之间被抢先，或取租约时并发、每分钟名额已满
    if idle:
        return "有热空闲但没租到（分类后被其他请求抢先占用，或取租约时并发/每分钟名额已满）"
    if standby + standby_busy == 0:
        return "热 Worker 全忙且没有备用账号"
    if "槽位" in info.get("原因", "") or "容量" in info.get("原因", ""):
        return "热 Worker 全忙、分区已满，备用账号起不来"
    return "热 Worker 全忙（并发超过容量）"


# 排队超限的进度（日志）：主用模型容量原因的等待不写“排队等待”，超限时写“主用模型排队超过…”，新版本带候选快照与驻留冷却
exceeded = [e for e in logs if e["t"] >= SINCE and (e["msg"].startswith("主用模型排队超过") or e["msg"].startswith("主用模型不排队"))]
print()
print("  ▶ 排队超限进度（运行日志“主用模型排队超过 …，立即返回 429”）")
if not exceeded:
    print("    窗口内没有这类日志")
else:
    infos = []
    for e in exceeded:
        info = fields(e["msg"])
        info["_model"] = model_key(info.get("模型")) or model_of_request.get(e["request"].get("id"), "")
        infos.append(info)
    reasons = collections.Counter(info.get("原因", "?") for info in infos)
    tries = collections.Counter(info.get("尝试", "?") for info in infos)
    by_model = collections.Counter(info["_model"] or "未关联到账本" for info in infos)
    print(f"    共 {len(infos)} 行；原因：{top(reasons, 5)}；尝试：{top(tries, 5)}；模型：{top(by_model, 4)}")
    snapshots = [info for info in infos if "热空闲" in info]
    if not snapshots:
        print("    这些行没有候选快照（热空闲、热忙碌、备用、驻留冷却等）：运行的是旧版本")
    focus = [info for info in snapshots if is_model(info["_model"])]
    title = MODEL
    if not focus:
        focus, title = snapshots, "全部模型"
    if snapshots:
        def median(key):
            values = [number(info, key) for info in focus if key in info or key in ("每分钟已满",)]
            return pct(values, .5)
        queues = [int(info["同条件队列"].split("/")[-1]) for info in focus if re.fullmatch(r"\d+/\d+", info.get("同条件队列", ""))]
        recover = [go_duration(info.get("最早恢复")) for info in focus if info.get("最早恢复")]
        has_cooling = [info for info in focus if "驻留冷却" in info]
        print(f"    {title} 的 {len(focus)} 行快照（中位数）：热空闲 {median('热空闲')}、热忙碌 {median('热忙碌')}、备用 {median('备用')}、"
              f"备用忙碌 {median('备用忙碌')}、每分钟已满 {median('每分钟已满')}"
              + (f"、驻留冷却 {pct([number(info, '驻留冷却') for info in has_cooling], .5)}" if has_cooling else "")
              + (f"；同条件队列总数 中位 {pct(queues, .5)}" if queues else "")
              + (f"；{share(len(recover), len(focus))} 行带最早恢复（中位 {secs(pct(recover, .5))}）" if recover else ""))
        starting = sum(1 for info in focus if info.get("启动中") == "是")
        no_standby = sum(1 for info in focus if number(info, "备用") + number(info, "备用忙碌") == 0)
        print(f"      启动中 {share(starting, len(focus))}；备用为 0 {share(no_standby, len(focus))}"
              + ("" if has_cooling else "；没有驻留冷却字段（只有主用模型供给缓存里有该模型的条目时才写）"))
        causes = collections.Counter(busy_cause(info) for info in focus)
        print(f"      按快照归因：{top(causes, 6, len(focus))}")
        cause, count = causes.most_common(1)[0]
        if count * 2 >= len(focus):
            advice = {
                "驻留 Worker 大多对它冷却（驻留冷却 ≥ 热空闲+热忙碌）":
                    "看第 4 段的连续限流档位与第 6 段的驻留冷却：裸限流连续出现会逐级加长冷却，第 3 档（30 分钟）起驻留 Worker 成为死槽位，排空淘汰后换上备用账号",
                "热 Worker 多半每分钟已满（ACCOUNT_RPM_LIMIT）": "每分钟上限是瓶颈：增加驻留 Worker（MAX_ACTIVE_WORKERS），或减少该模型的并发",
                "热 Worker 全忙且没有备用账号": "号池里已经没有还能服务它的空闲账号，多半是当天额度已用完：加入账号，或等太平洋时间零点",
                "热 Worker 全忙、分区已满，备用账号起不来": "分区峰值不够：调大 MAX_ACTIVE_WORKERS（Ultra 为 ULTRA_MAX_ACTIVE_WORKERS），或减少并发",
                "热 Worker 全忙（并发超过容量）": "并发超过驻留 Worker × 单账户并发：增加 Worker，或减少并发",
            }.get(cause, "")
            VERDICTS.append(f"排队超限时的候选快照：{share(count, len(focus))} 属于“{cause}”" + (f"。{advice}" if advice else ""))

# 排队进度（日志）：其他模型与冷却等待的“排队等待”行
queue_lines = [e for e in logs if e["t"] >= SINCE and e["msg"].startswith("排队等待 |")]
queue_by_model = collections.defaultdict(list)
for e in queue_lines:
    info = fields(e["msg"])
    queue_by_model[info.get("模型", "?")].append(info)
if queue_by_model:
    print()
    print("  ▶ 排队等待日志（非主用模型的容量等待与所有模型的冷却等待）")
for model, items in sorted(queue_by_model.items(), key=lambda kv: -len(kv[1]))[:5]:
    reasons = collections.Counter(info.get("原因", "?") for info in items)
    def avg(key):
        values = [number(info, key) for info in items]
        return sum(values) / max(len(values), 1)
    full = sum(1 for info in items if number(info, "每分钟已满") > 0)
    print(f"    {model}：{len(items)} 行；原因：{top(reasons, 4)}；平均 热空闲 {avg('热空闲'):.1f}、热忙碌 {avg('热忙碌'):.1f}、"
          f"备用 {avg('备用'):.1f}、备用忙碌 {avg('备用忙碌'):.1f}、每分钟已满 {avg('每分钟已满'):.1f}（{share(full, len(items))} 行带每分钟已满）")
if primary_listed:
    print(f"    注意：{MODEL} 在主用模型列表里，因容量原因排队时不写“排队等待”行，只在超限时写上面的“主用模型排队超过…”；冷却等待照常写")

# ================= 3. 上游 429 分类 =================
section("3. 上游 429 按文案分类（账本中每次失败的尝试）")
attempt_records = []
for row in window_rows:
    for attempt in row["attempts"]:
        attempt_records.append((row["model"], attempt.get("channel") or "?", attempt.get("account") or "",
                                attempt_class(attempt.get("error")), attempt.get("error") or ""))
if connection is None:
    print("  没有账本，改用日志里的“账号切换”行（会丢条）：")
    for e in logs:
        if e["t"] >= SINCE and e["msg"].startswith("账号切换"):
            model = model_key(fields(e["msg"]).get("模型", ""))
            reason = e["msg"].split("原因:", 1)[-1].strip()
            attempt_records.append((model, upstream_channel(reason), e["source"], attempt_class(reason), reason))
quota_records = [a for a in attempt_records if a[3].startswith("429·")]
print(f"  窗口内失败的上游尝试 {len(attempt_records)} 次，其中 429 {len(quota_records)} 次、"
      f"发出前已冷却 {sum(1 for a in attempt_records if a[3].startswith('发出前'))} 次")
if quota_records:
    overall = collections.Counter(a[3][4:] for a in quota_records)
    print(f"  429 文案分类：{top(overall, 7, len(quota_records))}")
    print("  按模型 × 通道：")
    per_model = collections.defaultdict(list)
    for record in quota_records:
        per_model[record[0]].append(record)
    for model, items in sorted(per_model.items(), key=lambda kv: (not is_model(kv[0]), -len(kv[1])))[:6]:
        parts = []
        for cls, count in collections.Counter(a[3][4:] for a in items).most_common(5):
            channels = collections.Counter(a[1] for a in items if a[3][4:] == cls)
            accounts_hit = len({a[2] for a in items if a[3][4:] == cls})
            parts.append(f"{cls} {count}（" + "/".join(f"{k} {v}" for k, v in channels.most_common()) + f"，账号 {accounts_hit} 个）")
        print(f"    {model}：{len(items)} 次；" + "；".join(parts))
    samples = collections.Counter(short_text(a[4], 80) for a in quota_records if a[3][4:] in (Q_QUOTA, Q_OTHER, Q_RES))
    if samples:
        print("  其他 429 原文（数字已替换为 N）：")
        for text, count in samples.most_common(4):
            print(f"    {count:>5}  {text}")
others = collections.Counter(a[3] for a in attempt_records if not a[3].startswith("429·"))
if others:
    print(f"  非 429 的失败尝试：{top(others, 6)}")

# 冷却日志：类型（每日限额/分钟限额/限流）、指标（如 generate_requests_global）、额度计数的模型（如 gemini-3.1-pro）与
# 范围（全局、模型=Playground、build:模型）；没有指标的冷却另带详情类型（上游附带、没有解析的 details 类型）。
# 每日限额已判定两个通道共用额度时另一通道的同步冷却行（“与另一通道共用额度，同步冷却”）本来就不带上限、指标与模型：
# 只计入范围、另计行数，不进类型·指标与上限、模型、详情类型的统计，免得被当成没带额度详情的每日限额
cool_lines = [e for e in logs if e["t"] >= SINCE and (e["msg"].startswith("账号冷却 |") or e["msg"].startswith("账号冷却未写入 |"))]
if cool_lines:
    written = [e for e in cool_lines if e["msg"].startswith("账号冷却 |")]
    print(f"  冷却日志：写入 {len(written)} 行，未写入 {len(cool_lines) - len(written)} 行（app/runtime.go applyQuotaCooldown）")
    kinds = collections.Counter()
    scopes = collections.Counter()
    limits = collections.Counter()
    quota_models = collections.Counter()
    detail_types = collections.Counter()
    synced_daily = 0
    for e in written:
        info = fields(e["msg"])
        scope = info.get("范围", "?")
        channel = "全局（整个账号）" if scope == "全局" else "build" if scope.startswith("build:") else "playground"
        scopes[(bare_scope(scope), channel)] += 1
        if "与另一通道共用额度，同步冷却" in e["msg"]:
            synced_daily += 1
            continue
        tag = "逐级" if "连续限流" in info else (info.get("指标") or "无元数据")
        kinds[f"{info.get('类型', '?')}·{tag}"] += 1
        if info.get("上限"):
            limits[f"{info.get('类型')}={info.get('上限')}"] += 1
        if info.get("模型"):
            quota_models[f"{info.get('类型')}·{info.get('模型')}" + (f"={info.get('上限')}" if info.get("上限") else "")] += 1
        if info.get("详情类型") and not info.get("指标"):
            detail_types[info.get("详情类型")] += 1
    print(f"    类型·指标：{top(kinds, 6, len(written) - synced_daily)}（“限流·逐级”为裸限流 429 的连续限流冷却，见第 4 段）")
    if synced_daily:
        print(f"    同步冷却另一通道（每日限额，与另一通道共用额度）：{synced_daily} 行")
    print("    范围：" + "，".join(f"{m}@{c} {n}" for (m, c), n in scopes.most_common(6)))
    if limits:
        print(f"    上游给出的上限值：{top(limits, 4)}")
    if quota_models:
        print(f"    额度计数的模型（模型=，可能与请求的模型名不同）：{top(quota_models, 6)}")
    if detail_types:
        print(f"    没有指标的冷却带的详情类型：{top(detail_types, 4)}（裸 429 带着没有解析的详情，请把这一行发给开发者）")

# 额度 429 详情未识别（app/quota_detail_log.go）：429 带着 GeminiApiErrorDetails 却没有解析出指标（冷却日志带 详情类型=），行里带这份详情的原文；
# 服务端同一种（类型 + 文案，文案里的数字不计）10 分钟最多写一条、任意 10 分钟最多 64 条。这里同样把文案里的连续数字换成 N 再分种，
# 每种最多列 3 条原文不同的行（照原样列），长行截断到 1500 字
UNPARSED_RE = re.compile(r"^额度 429 详情未识别 \| 类型=(.*?) \| 文案=(.*?) \| 原文=(.*) \| 请把这一行发给开发者$")
unparsed_lines = [e for e in logs if e["t"] >= SINCE and e["msg"].startswith("额度 429 详情未识别 |")]
if unparsed_lines:
    unparsed_kinds = {}
    for e in unparsed_lines:
        line = e["msg"].split("\n")[0]
        match = UNPARSED_RE.match(line)
        if match:
            kind, raw = (match.group(1), re.sub(r"[0-9]+", "N", match.group(2))), match.group(3)
        else:
            kind, raw = ("?", re.sub(r"[0-9]+", "N", line[:200])), line
        samples = unparsed_kinds.setdefault(kind, {})
        if raw not in samples and len(samples) < 3:
            samples[raw] = (e["t"], line if len(line) <= 1500 else line[:1500] + f"…（截断，共 {len(line)} 字）")
    print(f"  ⚠ 额度 429 详情未识别：{len(unparsed_lines)} 行、{len(unparsed_kinds)} 种文案（429 带着 GeminiApiErrorDetails，"
          "却没有解析出指标，冷却只能按文案判断，裸 Quota exceeded 按裸限流逐级冷却）")
    print("    请把下面这几行原样发给开发者，用来扩展解析（每种文案最多 3 条，文案里的数字不计）：")
    for samples in unparsed_kinds.values():
        for moment, line in samples.values():
            print(f"    {local(moment)}  {line}")
    VERDICTS.append(f"日志里有 {len(unparsed_lines)} 行“额度 429 详情未识别”（{len(unparsed_kinds)} 种文案）："
                    "这种 429 没有解析出指标，请把第 3 段列出的这几行发给开发者")


def sharing_separate(stats):
    # 每日额度通道判定为“独立”（app/quota_sharing.go sharedFor：Independent > 0 且 Independent × 20 > Shared）
    shared, independent = stats.get("shared", 0) or 0, stats.get("independent", 0) or 0
    return independent > 0 and independent * 20 > shared


def channels_separate(account):
    # 该账号在重点模型上两个通道是否分别计档：只有判定为独立时分别计，共用、观察中、没有记录都共用一份；Ultra 权益单独一组
    # （按 /api/accounts 的权益区分，读不到账号列表时都按普通权益的判定）
    stats = ((sharing_doc or {}).get("models") or {}).get(MODEL + "|ultra" if tier_of.get(account, "").startswith("Ultra") else MODEL)
    return isinstance(stats, dict) and sharing_separate(stats)


# ================= 4. 连续限流递增冷却 =================
section("4. 连续限流递增冷却（裸限流 429：30 秒 → 5 分钟 → 30 分钟 → 2 小时 → 次日零点，成功一次清零）")
rate_written = []
for e in logs:
    if e["t"] < SINCE or not e["msg"].startswith("账号冷却 |"):
        continue
    info = fields(e["msg"])
    if info.get("类型") != "限流":
        continue
    level = level_of(info.get("连续限流"))
    rate_written.append({
        "t": e["t"], "account": e["source"], "scope": info.get("范围", ""), "model": bare_scope(info.get("范围", "")),
        "level": level, "synced": info.get("同步") == "两个通道", "systemic": "| 号池级限流不升档" in e["msg"],
        "after_output": "| 已输出内容后" in e["msg"], "until": parse_time(info.get("恢复")),
    })
laddered = [item for item in rate_written if item["level"]]
rate_skipped = [e for e in logs if e["t"] >= SINCE and e["msg"].startswith("账号冷却未写入 |") and fields(e["msg"]).get("类型") == "限流"]
cleared = []
for e in logs:
    if e["t"] >= SINCE and e["msg"].startswith("限流档位清零 |"):
        info = fields(e["msg"])
        cleared.append({"t": e["t"], "account": e["source"], "scope": info.get("范围", ""),
                        "model": bare_scope(info.get("范围", "")), "level": level_of(info.get("此前连续")) or 0})
systemic_lines = [e for e in logs if e["t"] >= SINCE and e["msg"].startswith("号池级限流 |")]
bare_attempts = sum(1 for a in attempt_records if a[3] == "429·" + Q_BARE)
if not rate_written and not cleared and not systemic_lines:
    print("  窗口内没有“账号冷却 | 类型=限流”日志" + (f"；账本里却有 {bare_attempts} 次裸 Quota exceeded：运行的版本可能还没有逐级冷却，"
                                                 "或这些 429 发生在转写、Live 等不写这行日志的路径" if bare_attempts else ""))
else:
    flat = len(rate_written) - len(laddered)
    print(f"  写入的限流冷却 {len(rate_written)} 行：逐级 {len(laddered)} 行"
          + (f"，不逐级 {flat} 行（上游给了恢复时间（RetryInfo、错误文案里的 retry in、Retry-After）、账号全局限流，或文件复制、附件上传返回的 429，按原样冷却）" if flat else "")
          + f"；未写入 {len(rate_skipped)} 行；档位清零 {len(cleared)} 行；号池级限流告警 {len(systemic_lines)} 行")
    if laddered:
        levels = collections.Counter(ladder_name(min(item["level"], 5)) for item in laddered)
        print(f"  各档写入次数：{top(levels, 6, len(laddered))}")
        synced = sum(1 for item in laddered if item["synced"])
        systemic = sum(1 for item in laddered if item["systemic"])
        after = sum(1 for item in laddered if item["after_output"])
        print(f"    同步冷却另一个通道 {share(synced, len(laddered))}（两个通道共用或尚未判定为独立额度时共用一份档位）；"
              f"号池级限流不升档 {share(systemic, len(laddered))}；已输出内容后（不升档）{share(after, len(laddered))}")
        by_model = collections.Counter(item["model"] for item in laddered)
        print(f"    按模型：{top(by_model, 5)}")
    if rate_skipped:
        reasons = collections.Counter(fields(e["msg"]).get("原因", "?") for e in rate_skipped)
        print(f"  未写入的原因：{top(reasons, 3)}")
    if cleared:
        before = collections.Counter(ladder_name(min(item["level"], 5)) for item in cleared)
        print(f"  档位清零（此前到第 2 档以上、随后在该通道成功一次）：{top(before, 5, len(cleared))}；按模型：{top(collections.Counter(item['model'] for item in cleared), 4)}")
    for e in systemic_lines[-5:]:
        info = fields(e["msg"])
        print(f"  号池级限流 {local(e['t'])}  模型={info.get('模型', '?')}" + (f"  {info.get('号池')}" if info.get("号池") else ""))
    if systemic_lines:
        VERDICTS.append(
            f"窗口内号池级限流告警 {len(systemic_lines)} 次（{top(collections.Counter(fields(e['msg']).get('模型', '?') for e in systemic_lines), 3)}）："
            "号池内 10 分钟内至少 5 个账号裸限流且没有任何成功，判定为上游整体限流或故障，逐级冷却暂停（只按第 1 档 30 秒）；换号无益，减少并发或等上游恢复")

    # 重点模型逐账号：最高档位、之后是否清零（清零 = 冷却到期后在该通道成功了，说明是短期限流）。
    # 结论按“当前档位”下：与生产端一致（aistudio/rate_strike.go），档位在清零与跨过太平洋时间零点时作废，之前的升档不再算数——
    # 歇完第 4 档已清零、之后又短暂升到第 2 档的账号，或零点前升到第 4 档、零点后额度已恢复的账号，不能再按“当天额度已用完”计。
    # 每条写入的“连续限流”是写入后的档位（跨零点作废后从第 1 档起已体现在里面），所以当前档位就是最后一次写入的档位，
    # 最后一个事件是清零、或最后一次写入之后已过太平洋时间零点时为 0。两个通道判定为独立额度时按（账号，通道）分组，否则按账号
    focus = [item for item in laddered if is_model(item["model"])]
    focus_cleared = [item for item in cleared if is_model(item["model"])]
    if MODEL and (focus or focus_cleared):
        def strike_group(item):
            if not channels_separate(item["account"]):
                return (item["account"], "")
            return (item["account"], "build" if item["scope"].startswith("build:") else "playground")
        timeline = collections.defaultdict(list)
        for item in focus:
            timeline[strike_group(item)].append((item["t"], 1, item))
        for item in focus_cleared:
            timeline[strike_group(item)].append((item["t"], 0, item))
        # peak 为窗口内的历史最高档（清零行的“此前连续”也算：第 1～3 档写在窗口外、清零在窗口内的账号同样计入）；
        # current 为当前档位；rise_until 为当前档位那次写入的恢复时间；cleared_after 为窗口内清零过、清零之后没有再升到第 2 档以上的
        peak, current, rise_until, cleared_after, cleared_any, expired = {}, {}, {}, set(), 0, 0
        for group, entries in timeline.items():
            entries.sort(key=lambda entry: (entry[0], entry[1]))
            level, last_write, has_clear, after_clear = 0, None, False, 0
            for _, kind, item in entries:
                peak[group] = max(peak.get(group, 0), item["level"])
                if kind == 0:
                    level, last_write, has_clear, after_clear = 0, None, True, 0
                    continue
                level, last_write = item["level"], item
                after_clear = max(after_clear, item["level"])
            if last_write is not None and level and next_pacific_midnight(last_write["t"]) <= NOW:
                level = 0
                expired += 1
            current[group] = level
            if level >= 2:
                rise_until[group] = last_write["until"]
            cleared_any += has_clear
            if has_clear and after_clear < 2:
                cleared_after.add(group)
        unit = "账号×通道" if any(group[1] for group in peak) else "账号"
        reached = {n: sum(1 for value in peak.values() if value >= n) for n in (2, 3, 4, 5)}
        print(f"  {MODEL}：逐级冷却涉及{unit}{'（两个通道判定为独立额度，分别计档）' if unit != '账号' else ''} {len(peak)} 个；"
              f"窗口内最高到第 2 档以上 {reached[2]} 个、第 3 档以上 {reached[3]} 个"
              f"（死槽位）、第 4 档以上 {reached[4]} 个、到次日零点 {reached[5]} 个；窗口内清零过 {cleared_any} 个"
              + (f"（其中 {cleared_any - len(cleared_after)} 个清零后又升到第 2 档以上，按之后的档位判断）" if cleared_any > len(cleared_after) else ""))
        # 只用有结果的下结论：之后清零的（冷却到期后成功，清零后没有再升到第 2 档以上），或当前在第 4 档以上的（歇过第 3 档的 30 分钟
        # 冷却再试仍失败，且之后没有清零、没有跨过零点）。停在第 2、3 档的冷却期间不会被选中、没有机会清零，连续失败几分钟说明不了
        # 当天额度已用完，单独列出、不计入判断
        failing = {group for group, level in current.items() if level >= 4}
        waiting = [group for group, level in current.items() if 2 <= level < 4]
        pending = sum(1 for group in waiting if rise_until.get(group) is None or rise_until[group] > NOW)
        judged = len(cleared_after) + len(failing)
        print(f"    按当前档位（清零或跨过太平洋时间零点后作废）：有结果的 {judged} 个：之后清零 {len(cleared_after)} 个，"
              f"歇过第 3 档（30 分钟）冷却后仍失败（当前在第 4 档以上）{len(failing)} 个；停在第 2、3 档的：冷却未到期、暂不能判断 {pending} 个，"
              f"冷却已到期、之后没有清零也没有再升档 {len(waiting) - pending} 个（多半还没被再次选中）"
              + (f"；最后一次升档后已过太平洋时间零点、档位作废 {expired} 个（不计入）" if expired else ""))
        if len(failing) >= 3 and len(cleared_after) * 4 <= judged:
            VERDICTS.append(
                f"{MODEL} 有 {len(failing)} 个{unit}歇过第 3 档（30 分钟）冷却后仍裸限流（当前在第 4 档以上），有结果的 {judged} 个中只有 "
                f"{len(cleared_after)} 个之后成功清零：这些账号的裸 Quota exceeded 多半是当天额度已用完。第 3 档（30 分钟）起它们的驻留 Worker "
                "成为死槽位、排空淘汰后换上备用账号；备用账号也用完时只能加入账号或等太平洋时间零点")
        elif judged >= 3 and len(cleared_after) * 2 >= judged:
            VERDICTS.append(
                f"{MODEL} 第 2 档以上有结果的 {judged} 个{unit}中 {len(cleared_after)} 个之后成功清零：裸 Quota exceeded 多为短期限流，"
                "逐级冷却让这些账号歇几分钟后恢复")

# 当前冷却（/api/cooldowns）：原因里带“连续限流第 N 档”的就是逐级冷却，看重点模型此刻各档有多少账号、还剩多久
if cooldown_doc is not None and MODEL:
    current = collections.defaultdict(dict)
    for item in cooldowns:
        level = level_of((item.get("reason") or "")[:40]) if "连续限流" in (item.get("reason") or "")[:40] else None
        until = parse_time(item.get("until"))
        if not level or not until or until <= NOW or not is_model(item.get("model_id")):
            continue
        channel = (item.get("channel") or "playground").lower()
        previous = current[item.get("account_id")].get(channel)
        if not previous or level > previous[0]:
            current[item.get("account_id")][channel] = (level, until)
    if current:
        levels = collections.Counter(ladder_name(min(max(value[0] for value in channels.values()), 5)) for channels in current.values())
        both = sum(1 for channels in current.values() if len(channels) >= 2)
        long_left = sum(1 for channels in current.values() if max(value[1] for value in channels.values()) - NOW >= TD(minutes=15))
        print(f"  此刻 {MODEL} 的逐级冷却：账号 {len(current)} 个（按最高档）{top(levels, 5)}；两个通道都在冷却 {both} 个；"
              f"剩余不少于 15 分钟（死槽位口径）{long_left} 个")
    else:
        print(f"  此刻 {MODEL} 没有逐级冷却中的账号")
elif MODEL and API_STATE in ("down", "denied", "offline"):
    print("  此刻的冷却：管理接口不可用，跳过")

# 两个通道是否共用一份档位：只有每日额度通道判定为“独立”时各通道分别计档，共用、观察中、没有记录都共用（app/quota_sharing.go sharedFor）
if MODEL:
    models = (sharing_doc or {}).get("models") or {}
    found = False
    for key, label in ((MODEL, ""), (MODEL + "|ultra", "（Ultra 权益）")):
        stats = models.get(key)
        if not isinstance(stats, dict):
            continue
        found = True
        shared, independent = stats.get("shared", 0) or 0, stats.get("independent", 0) or 0
        separate = sharing_separate(stats)
        verdict = ("独立：两个通道分别计档，只冷却失败的通道" if separate else
                   "共用：两个通道共用一份档位，同步冷却另一个启用通道" if shared >= 8 else "观察中：按共用处理")
        print(f"  每日额度通道判定 {MODEL}{label}：另一通道也达到限额 {shared} 次，另一通道仍可用 {independent} 次 → {verdict}")
    if not found:
        print(f"  每日额度通道判定：{MODEL} 没有记录（{ENV.get('AUTH_DIR')}/.quota-sharing.json），连续限流按两个通道共用一份档位处理")

# ================= 5. 裸 Quota exceeded 逐账号 =================
section(f"5. {MODEL or '-'} 的裸“Quota exceeded”：每日额度还是短期限流（回看 {QE_MINUTES} 分钟）")


def model_events():
    # 每个账号该模型的事件：bare（裸 Quota exceeded）、quota（其他 429）、fail（其他失败）、blocked（发出前已冷却）、ok（成功）。
    # 账本只记录请求完成时间与每次尝试的耗时：第 i 次尝试的时间按“请求开始 + 前 i 次尝试耗时之和”估算（不含两次之间的排队，偏早几秒）
    events = collections.defaultdict(list)
    for row in rows:
        if row["end"] < QE_SINCE or not is_model(row["model"]):
            continue
        cursor = row["start"]
        for attempt in row["attempts"]:
            cursor = min(cursor + TD(milliseconds=attempt.get("duration_ms") or 0), row["end"])
            account = attempt.get("account") or ""
            if not account:
                continue
            cls = attempt_class(attempt.get("error"))
            kind = ("bare" if cls == "429·" + Q_BARE else "blocked" if cls.startswith("发出前") else
                    "quota" if cls.startswith("429·") else "fail")
            events[account].append((cursor, kind, attempt.get("channel") or ""))
        if row["success"] and row["account"]:
            moment = min(row["start"] + TD(seconds=row["first"]), row["end"]) if row["first"] else row["end"]
            events[row["account"]].append((moment, "ok", row["channel"]))
    if connection is None:
        for e in logs:
            if e["t"] < QE_SINCE or not e["msg"].startswith("账号切换"):
                continue
            if not is_model(fields(e["msg"]).get("模型", "")):
                continue
            reason = e["msg"].split("原因:", 1)[-1]
            cls = attempt_class(reason)
            kind = "bare" if cls == "429·" + Q_BARE else "quota" if cls.startswith("429·") else "fail"
            events[e["source"]].append((e["t"], kind, upstream_channel(reason)))
    for items in events.values():
        items.sort(key=lambda item: item[0])
    return events


def same_channel(a, b):
    # 通道相同；任一边不知道通道（空或“?”）时按相同处理
    return a == b or a in ("", "?") or b in ("", "?")


events = model_events() if MODEL else {}
stats = []
for account, items in events.items():
    bares = [item for item in items if item[1] == "bare"]
    if not bares:
        continue
    first, last = bares[0][0], bares[-1][0]
    reset = next_pacific_midnight(first)
    after = [item for item in items if first < item[0] < reset and item[1] != "blocked"]
    ok_after = [item for item in after if item[1] == "ok"]
    intervals = []
    per_channel = collections.defaultdict(list)
    for item in bares:
        per_channel[item[2]].append(item[0])
    for times in per_channel.values():
        intervals += [(b - a).total_seconds() for a, b in zip(times, times[1:])]
    following = []
    for index, item in enumerate(items):
        if item[1] != "bare":
            continue
        nxt = next((other for other in items[index + 1:] if other[1] != "blocked" and other[2] == item[2]), None)
        following.append((nxt[1], (nxt[0] - item[0]).total_seconds()) if nxt else ("没有再试", None))
    sent = [item for item in items if item[1] != "blocked"]
    # 第一次出现 STILL_FAILING 以后、太平洋时间零点额度重置前仍是裸 Quota exceeded：歇过第 3 档的 30 分钟冷却再试仍失败。
    # 与成功一样按零点截断：零点后的失败属于下一个额度日，不能当作这一天额度用完的证据
    late_item = next((item for item in bares if item[0] - first >= STILL_FAILING and item[0] < reset), None)
    late = late_item[0] if late_item else None
    first_ok = (ok_after[0][0] - first).total_seconds() if ok_after else None
    # 仍失败：late 之前没有成功，late 之后到零点前也没有再成功（之后还能成功说明当天额度没用完，是短期限流）；两个通道判定为
    # 独立额度时只看 late 所在的通道（另一通道的额度另算）
    separate = channels_separate(account)
    still_failing = late is not None and (first_ok is None or first_ok > (late - first).total_seconds()) and not any(
        item[1] == "ok" and item[0] > late and (not separate or same_channel(item[2], late_item[2])) for item in after)
    stats.append({
        "account": account, "bares": len(bares), "first": first, "last": last, "reset": reset,
        "channels": collections.Counter(item[2] for item in bares), "intervals": intervals, "following": following,
        "ok_before": sum(1 for item in items if item[1] == "ok" and item[0] < first),
        "ok_after": len(ok_after), "after": len(after),
        "first_ok": first_ok,
        "still_failing": still_failing,
        "last_kind": sent[-1][1] if sent else "?", "tier": tier_of.get(account, "?"),
    })
ok_accounts = {account for account, items in events.items() if any(item[1] == "ok" for item in items)}
if not MODEL:
    print("  没有可分析的模型")
elif not stats:
    print(f"  回看窗口内 {MODEL} 没有裸 Quota exceeded（或账本里没有 attempts 记录）；成功过的账号 {len(ok_accounts)} 个")
else:
    total_bare = sum(s["bares"] for s in stats)
    print(f"  出现过裸 Quota exceeded 的账号 {len(stats)} 个，共 {total_bare} 次；同期该模型成功过的账号 {len(ok_accounts)} 个"
          f"（其中从没遇到裸 Quota exceeded 的 {len(ok_accounts - {s['account'] for s in stats})} 个）")
    per_account = [s["bares"] for s in stats]
    channels = collections.Counter()
    for s in stats:
        channels.update(s["channels"])
    print(f"  每个账号出现次数：中位 {pct(per_account, .5)}，P90 {pct(per_account, .9)}，最多 {max(per_account)}；通道：{top(channels)}")
    intervals = [value for s in stats for value in s["intervals"]]
    if intervals:
        buckets = collections.Counter()
        for value in intervals:
            buckets["≤35秒" if value <= 35 else "35秒–2分" if value <= 120 else "2–6分" if value <= 360 else "6–35分" if value <= 2100
                    else "35分–2.5小时" if value <= 9000 else ">2.5小时"] += 1
        order = ["≤35秒", "35秒–2分", "2–6分", "6–35分", "35分–2.5小时", ">2.5小时"]
        print(f"  同账号同通道相邻两次的间隔（{len(intervals)} 个，中位 {secs(pct(intervals, .5))}）：" +
              "，".join(f"{key} {share(buckets[key], len(intervals))}" for key in order if buckets[key]))
        print("    （各档冷却 30 秒、5 分钟、30 分钟、2 小时：间隔落在“≤35秒、35秒–2分”说明停在第 1 档（中间成功清零、号池级限流不升档，"
              "或旧版本只冷却 30 秒），间隔随次数拉长说明在逐级升档）")
    following = collections.Counter(kind for s in stats for kind, _ in s["following"])
    names = {"bare": "又是裸 Quota exceeded", "ok": "成功", "quota": "其他 429", "fail": "其他失败", "没有再试": "之后没有再试"}
    total_following = sum(following.values())
    print("  每次出现后，同账号同通道的下一次尝试：" + "，".join(f"{names.get(k, k)} {share(v, total_following)}" for k, v in following.most_common()))
    print(f"  第一次出现之后（到太平洋时间零点额度重置前）还成功过的账号：{share(sum(1 for s in stats if s['ok_after']), len(stats))}；"
          f"第一次出现之前成功次数 中位 {pct([s['ok_before'] for s in stats], .5)}、P90 {pct([s['ok_before'] for s in stats], .9)}")
    for minutes in WITHIN:
        observable = [s for s in stats if s["first"] <= NOW - TD(minutes=minutes)]
        retried = [s for s in observable if s["after"]]
        recovered = [s for s in observable if s["first_ok"] is not None and s["first_ok"] <= minutes * 60]
        print(f"  出现后 {minutes:>3} 分钟内再成功的账号：{share(len(recovered), len(observable))}"
              f"（可观察 {len(observable)} 个，其中第一次出现之后又被尝试过的 {len(retried)} 个）")
    last_kinds = collections.Counter(names.get(s["last_kind"], s["last_kind"]) for s in stats)
    print(f"  这些账号最近一次尝试的结果：{top(last_kinds)}")
    if tier_of:
        tiers = collections.Counter(s["tier"] for s in stats)
        ok_tiers = collections.Counter(tier_of.get(account, "?") for account in ok_accounts)
        print(f"  按权益：出现裸 Quota exceeded 的账号 {top(tiers)}；成功过的账号 {top(ok_tiers)}")
        for tier, _ in tiers.most_common(4):
            group = [s for s in stats if s["tier"] == tier]
            print(f"    {tier}：{len(group)} 个，第一次出现后还成功过 {share(sum(1 for s in group if s['ok_after']), len(group))}")
    print("  出现次数最多的账号（账号已打码；时间为服务器本地时间）：")
    for s in sorted(stats, key=lambda s: -s["bares"])[:8]:
        print(f"    {mask(s['account']):<24} {s['bares']:>4} 次  {local(s['first'])} ~ {local(s['last'])}  之前成功 {s['ok_before']}  之后成功 {s['ok_after']}"
              f"  通道 {'/'.join(f'{k}×{v}' for k, v in s['channels'].items())}" + (f"  {s['tier']}" if tier_of else ""))
    # 判定窗口取 WITHIN 的最大值：逐级冷却下同一账号约在 0、30 秒、5.5 分钟被尝试，第 3 次失败后冷却 30 分钟，
    # 窗口短于这段冷却时窗口内再成功结构上不可能，会把短期限流判成每日额度。窗口只决定哪些账号可以判断（第一次出现在窗口以前），
    # 恢复的证据不受窗口限制：零点前任何时候再成功都说明当天额度没用完（升到第 4 档的账号要歇 2 小时，最早约 2 小时 35 分钟才会成功，
    # 结构上落在窗口外）。只用有结果的账号：零点前再成功过、且不属于下一种的；或第一次出现 30 分钟以后、零点前仍是裸 Quota exceeded、
    # 之后再没成功的（两个通道判定为独立额度时只看那次失败的通道）。既没有成功、30 分钟以后也没有再失败的不算
    judge = max(WITHIN)
    repeat = following.get("bare", 0) / total_following if total_following else 0
    if judge < JUDGE_MIN_MINUTES:
        print(f"  判定窗口 {judge} 分钟（WITHIN 的最大值）短于第 3 档冷却（30 分钟）加余量，不据此下结论；"
              f"WITHIN 里至少放一个不小于 {JUDGE_MIN_MINUTES} 的值")
    else:
        due = [s for s in stats if s["first"] <= NOW - TD(minutes=judge)]
        failing = [s for s in due if s["still_failing"]]
        recovered = [s for s in due if s["first_ok"] is not None and not s["still_failing"]]
        inside = sum(1 for s in recovered if s["first_ok"] <= judge * 60)
        decided = len(recovered) + len(failing)
        print(f"  判定（第一次出现在 {judge} 分钟以前的 {len(due)} 个账号）：零点前再成功 {len(recovered)} 个（{judge} 分钟内 {inside} 个、"
              f"之后 {len(recovered) - inside} 个），30 分钟以后仍是裸 Quota exceeded、之后再没成功 {len(failing)} 个；"
              f"既没有成功、30 分钟以后也没有再失败，暂不能判断 {len(due) - decided} 个")
        rate = len(recovered) / decided if decided else None
        if rate is not None and decided >= 5:
            if rate <= 0.2 and repeat >= 0.5:
                VERDICTS.append(
                    f"{MODEL} 的裸 Quota exceeded 更像每日（长期）额度用完：可判断的 {decided} 个账号中出现后（太平洋时间零点前）再成功的只有 "
                    f"{len(recovered)} 个，{len(failing)} 个歇过 30 分钟冷却后仍是裸 Quota exceeded、之后再没成功，下一次尝试 {repeat * 100:.0f}% 仍是裸 Quota exceeded。"
                    "逐级冷却下这些账号升到第 3 档（30 分钟）以上，驻留 Worker 成为死槽位后排空淘汰；第 4 段若多停在第 1、2 档，看是否有号池级限流告警或已输出内容后的 429")
            elif rate >= 0.6:
                VERDICTS.append(f"{MODEL} 的裸 Quota exceeded 更像短期限流：可判断的 {decided} 个账号中出现后（太平洋时间零点前）再成功的 {len(recovered)} 个"
                                f"（{judge} 分钟内 {inside} 个），逐级冷却让它们歇过几档后恢复、成功一次即清零")
            else:
                VERDICTS.append(f"{MODEL} 的裸 Quota exceeded 介于两者之间：可判断的 {decided} 个账号中零点前再成功 {len(recovered)} 个，"
                                f"下一次仍失败 {repeat * 100:.0f}%；可能是少数账号用完每日额度、多数是短期限流，看上面的按权益分组与第 4 段的档位分布")
        else:
            pending = len(due) - decided
            VERDICTS.append(f"{MODEL} 的裸 Quota exceeded 样本太少（可判断的账号 {decided} 个"
                            + (f"，另有 {pending} 个既没有成功、30 分钟以后也没有再失败" if pending else "")
                            + "）：过半小时再跑，或设 QE_MINUTES=720 扩大回看后再跑")
# 冷却日志里的精确时间（不依赖账本时间估算）：同一账号同一范围相邻两次限流冷却的间隔
precise = collections.defaultdict(list)
for e in logs:
    if e["t"] >= QE_SINCE and e["msg"].startswith("账号冷却 |"):
        info = fields(e["msg"])
        scope = info.get("范围", "")
        if info.get("类型") == "限流" and not info.get("指标") and is_model(bare_scope(scope)):
            precise[(e["source"], scope)].append(e["t"])
if precise:
    gaps = [(b - a).total_seconds() for times in precise.values() for a, b in zip(times, times[1:])]
    print(f"  日志核对：{MODEL} 的“账号冷却 | 类型=限流”（无指标）{sum(len(v) for v in precise.values())} 行，账号×范围 {len(precise)} 组，"
          f"相邻间隔 中位 {secs(pct(gaps, .5))}，≤2 分钟的 {share(sum(1 for g in gaps if g <= 120), len(gaps))}")

# ================= 6. 驻留 Worker 对该模型的冷却 =================
section(f"6. 驻留 Worker 对 {MODEL or '-'} 的冷却、正在处理的请求与 Worker 变动")
warm_ids = set(workers.get("warm_ids") or [])
enabled = [channel.lower() for channel in (config.get("upstream_channels") or ["playground", "build"])]
running = collections.Counter()
by_state = collections.Counter()
running_models = collections.Counter()
for request in active_requests:
    by_state[(model_key(request.get("model")), request.get("state"))] += 1
    if request.get("state") == "running" and request.get("account_id"):
        running[request["account_id"]] += 1
        if request["account_id"] in warm_ids:
            running_models[model_key(request.get("model"))] += 1
cooled = collections.defaultdict(dict)
for item in cooldowns:
    if not is_model(item.get("model_id")):
        continue
    until = parse_time(item.get("until"))
    if not until or until <= NOW:
        continue
    channel = (item.get("channel") or "playground").lower()
    current = cooled[item.get("account_id")].get(channel)
    if not current or until > current[0]:
        cooled[item.get("account_id")][channel] = (until, item.get("reason") or "")
if cooldown_doc is None:
    print("  无法读取 /api/cooldowns" + ("（管理接口不可用）" if API_STATE in ("down", "denied", "offline") else ""))
elif MODEL:
    def remaining_bucket(seconds):
        return ("<30秒" if seconds < 30 else "30秒–2分" if seconds < 120 else "2–5分" if seconds < 300 else "5–15分" if seconds < 900
                else "15–60分" if seconds < 3600 else ">1小时")
    order = ["<30秒", "30秒–2分", "2–5分", "5–15分", "15–60分", ">1小时"]
    for title, pool_ids in (("驻留 Worker", warm_ids), ("全部账号", None)):
        ids = [account for account in cooled if pool_ids is None or account in pool_ids]
        if pool_ids is not None and not pool_ids:
            print("  /api/status 没有返回驻留 Worker 列表（warm_ids），无法核对驻留 Worker")
            continue
        any_cooled = len(ids)
        all_cooled = sum(1 for account in ids if all(channel in cooled[account] for channel in enabled))
        kinds = collections.Counter()
        remaining = collections.Counter()
        for account in ids:
            until, reason = max(cooled[account].values(), key=lambda value: value[0])
            kinds[cooldown_kind(reason)] += 1
            remaining[remaining_bucket((until - NOW).total_seconds())] += 1
        base = f"（驻留 {len(pool_ids)} 个）" if pool_ids is not None else ""
        print(f"  {title}{base}中对 {MODEL} 有冷却的：{any_cooled}，启用的通道（{'/'.join(enabled)}）都冷却的：{all_cooled}")
        if any_cooled:
            print(f"    冷却类型（取恢复最晚的一条）：{top(kinds, 6, any_cooled)}")
            print("    剩余时间：" + "，".join(f"{key} {share(remaining[key], any_cooled)}" for key in order if remaining[key]))
        if title == "驻留 Worker" and pool_ids:
            supply_text = "；".join(
                f"{'Ultra 分区' if s.get('pool') == 'ultra' else '普通分区'} 热 {s.get('warm')} · 驻留冷却 {s.get('cooling', '-')}"
                for s in model_supplies)
            print(f"    驻留 Worker 中两个通道都没冷却 {len(pool_ids) - any_cooled} 个，至少一个通道没冷却 {len(pool_ids) - all_cooled} 个" +
                  (f"；状态接口的主用模型供给（调度口径，约 30 秒统计一次）：{supply_text}" if supply_text else ""))
            idle = sum(1 for account in ids if not running.get(account)) if active_doc is not None else None
            if idle is not None and any_cooled:
                print(f"    对 {MODEL} 冷却中的驻留 Worker 里，此刻没有任何请求在跑的 {share(idle, any_cooled)}（占着槽位却服务不了 {MODEL}）")
            if all_cooled * 2 >= len(pool_ids) and all_cooled:
                short = remaining["<30秒"] + remaining["30秒–2分"]
                long_left = remaining["15–60分"] + remaining[">1小时"]
                VERDICTS.append(
                    f"驻留 Worker {len(pool_ids)} 个中 {all_cooled} 个此刻对 {MODEL} 所有启用通道都在冷却（类型最多的是 {kinds.most_common(1)[0][0]}）："
                    f"剩余不到 2 分钟的 {share(short, any_cooled)} 不算冷却中、不淘汰（第 1 档限流、分钟限额，很快恢复），"
                    f"剩余不少于 15 分钟的 {share(long_left, any_cooled)} 是死槽位（应在排空淘汰，看第 6 段的 Worker 变动）；"
                    f"{MODEL} 此刻实际只有很少的驻留 Worker 可用")
    print("  注：账号全局冷却（*）在 /api/cooldowns 里只列在 playground 通道下（app/admin.go Cooldowns），所以“都冷却”可能偏少；"
          "供给的 cooling 另按账号支持的通道判断，更准")

if active_doc is not None:
    queued = sum(n for (m, s), n in by_state.items() if s == "queued" and is_model(m))
    running_model = sum(n for (m, s), n in by_state.items() if s == "running" and is_model(m))
    print(f"  正在处理的请求 {len(active_requests)} 个；{MODEL or '-'}：排队中 {queued}，已分到账号 {running_model}")
    if warm_ids:
        busy = [running.get(account, 0) for account in warm_ids]
        limit = concurrency or 0
        full = sum(1 for value in busy if limit and value >= limit)
        print(f"    驻留 Worker 上正在运行的请求：空闲 {sum(1 for v in busy if v == 0)} 个，有请求 {sum(1 for v in busy if v)} 个，"
              f"达到单账户并发 {limit} 的 {full} 个；这些请求的模型：{top(running_models, 5)}")
else:
    print("  无法读取 /api/requests" + ("（管理接口不可用）" if API_STATE in ("down", "denied", "offline") else ""))

LIFECYCLE = [
    ("WAA Worker 就绪", "新启动就绪"), ("WAA Worker 启动失败", "启动失败"), ("WAA Worker 按需扩容", "按需扩容"),
    ("WAA Worker 按需替换", "按需替换（替换空闲 Worker）"), ("WAA Worker 空闲回收 |", "空闲回收"), ("WAA Worker 冷却轮换 |", "冷却轮换"),
    ("WAA Worker 冷却淘汰 |", "冷却淘汰"), ("WAA Worker 补登记淘汰", "补登记淘汰"), ("WAA Worker 排空超时", "排空超时"),
    ("WAA Worker 排空 |", "排空"), ("WAA Worker 回收冷却占位 |", "请求现场回收死槽位"), ("WAA Worker 重建 |", "失效重建"),
    ("WAA Worker 停止 |", "停止"), ("WAA Worker 替补启动 |", "替补启动"), ("WAA Worker 替补跳过", "替补跳过"),
    ("WAA Worker 替补未启动", "替补未启动"), ("WAA Worker 替补启动失败", "替补启动失败"), ("常驻 Worker 已失败", "失败常驻关闭"),
]
lifecycle = collections.Counter()
retire_scopes = collections.Counter()
for e in logs:
    if e["t"] < SINCE:
        continue
    message = e["msg"]
    partition = "Ultra·" if message.startswith("Ultra WAA Worker") else ""
    message = message[len("Ultra "):] if partition else message
    for prefix, name in LIFECYCLE:
        if message.startswith(prefix):
            info = fields(message)
            amount = int(info.get("替换") or info.get("新增") or 1) if name in ("冷却轮换", "补登记淘汰") else 1
            lifecycle[partition + name] += amount
            if name in ("冷却淘汰", "排空"):
                retire_scopes[info.get("范围", "?")] += 1
            break
restarts = [e["t"] for e in logs if e["t"] >= SINCE and e["msg"].startswith("管理服务就绪")]
if lifecycle:
    print(f"  最近 {MINUTES} 分钟 Worker 变动（日志关键字，app/runtime.go、worker_retire.go、worker_drain.go、prewarm_keeper.go）：")
    print("    " + "，".join(f"{name} {count}" for name, count in lifecycle.most_common()))
    if retire_scopes:
        print(f"    冷却淘汰/排空的范围：{top(retire_scopes, 5)}")
    starts = lifecycle.get("新启动就绪", 0) + lifecycle.get("Ultra·新启动就绪", 0)
    warm_now = workers.get("warm") or 0
    if warm_now and starts >= warm_now:
        VERDICTS.append(f"{MINUTES} 分钟内新启动 Worker {starts} 个，超过驻留数 {warm_now}：Worker 在频繁更换，每次冷启动都占名额、拖慢排队")
    skipped = lifecycle.get("替补跳过", 0) + lifecycle.get("Ultra·替补跳过", 0)
    if skipped:
        VERDICTS.append(f"替补跳过 {skipped} 次：分区内没有能服务这些模型、没有长时间冷却的备用账号，淘汰死槽位后换不上新号")
else:
    print("  窗口内日志没有 Worker 变动记录")
if restarts:
    print(f"  ⚠ 窗口内服务重启 {len(restarts)} 次：" + "、".join(local(t) for t in restarts[:5]))

# ================= 7. 每分钟上限 =================
section("7. 每账号每分钟上限（ACCOUNT_RPM_LIMIT）与单账户并发")
print(f"  当前 ACCOUNT_RPM_LIMIT={rpm_limit if rpm_limit is not None else '未知'}，PER_ACCOUNT_CONCURRENCY={concurrency if concurrency is not None else '未知'}"
      "（GET /api/config 的 account_rpm_limit、per_account_concurrency；两者保存后立即生效）")
full_lines = [e for e in logs if e["t"] >= SINCE and e["msg"].startswith("账号每分钟请求已满")]
if full_lines:
    accounts_full = {e["source"] for e in full_lines}
    tens = collections.defaultdict(set)
    for e in full_lines:
        tens[e["t"].replace(minute=e["t"].minute // 10 * 10, second=0, microsecond=0)].add(e["source"])
    peak = max(len(v) for v in tens.values())
    print(f"  “账号每分钟请求已满”日志 {len(full_lines)} 行，涉及账号 {len(accounts_full)} 个；"
          f"同一账号 10 分钟最多一行，每 10 分钟最多 {peak} 个账号（驻留 Worker {len(warm_ids) or workers.get('warm') or '?'} 个）")
else:
    print("  窗口内没有“账号每分钟请求已满”日志" + ("（ACCOUNT_RPM_LIMIT=0 时不限速）" if rpm_limit == 0 else ""))
# 账本：每个账号实际发出的生成请求（成功的那次 + 失败的尝试，不含发出前被拦下的），滑动 60 秒窗口的最大次数
sent = collections.defaultdict(list)
for row in window_rows:
    cursor = row["start"]
    for attempt in row["attempts"]:
        cursor = min(cursor + TD(milliseconds=attempt.get("duration_ms") or 0), row["end"])
        if attempt.get("account") and not attempt_class(attempt.get("error")).startswith("发出前"):
            sent[attempt["account"]].append(cursor)
    if row["success"] and row["account"]:
        sent[row["account"]].append(row["start"] + TD(seconds=row["first"]) if row["first"] else row["end"])
if sent:
    peaks = {}
    for account, times in sent.items():
        times.sort()
        best, left = 0, 0
        for right in range(len(times)):
            while (times[right] - times[left]).total_seconds() > 60:
                left += 1
            best = max(best, right - left + 1)
        peaks[account] = best
    values = list(peaks.values())
    total_sent = sum(len(v) for v in sent.values())
    print(f"  账本估算：{len(sent)} 个账号共发出 {total_sent} 次生成请求（平均每分钟 {total_sent / MINUTES:.1f} 次）；"
          f"每个账号任意 60 秒内最多发出 中位 {pct(values, .5)}、P90 {pct(values, .9)}、最多 {max(values)} 次")
    if isinstance(rpm_limit, int) and rpm_limit > 0:
        hit = sum(1 for v in values if v >= rpm_limit)
        print(f"    达到上限 {rpm_limit} 的账号 {share(hit, len(values))}（时间按尝试耗时估算，可能有 ±几秒误差）")
        if hit * 3 >= len(values) and len(values) >= 5:
            VERDICTS.append(f"有 {hit}/{len(values)} 个账号在某个 60 秒内用满了 ACCOUNT_RPM_LIMIT={rpm_limit}：每分钟上限是吞吐的硬约束之一")

# ================= 8. 结论 =================
section("8. 结论（自动推断，请结合上面的数字核对）")
if model_rows:
    busy = sum(1 for row in model_rows if row["class"] and row["class"].startswith("号池繁忙"))
    success = sum(1 for row in model_rows if row["success"])
    VERDICTS.insert(0, f"{MODEL}：最近 {MINUTES} 分钟 {len(model_rows)} 个请求，成功 {share(success, len(model_rows))}，号池繁忙 {share(busy, len(model_rows))}")
empty_rows = [row for row in window_rows if row["success"] and not row["reply"] and not row["tools"]]
if window_rows and len(empty_rows) * 20 >= len(window_rows):
    VERDICTS.append(f"{len(empty_rows)} 个请求成功结束但没有正文也没有工具调用（客户端看到空回复），看第 1 段各模型的“空回复”数")
if not VERDICTS:
    VERDICTS.append("没有发现明显异常；如果问题仍在，把整段输出发给开发者")
for verdict in VERDICTS:
    print("  - " + verdict)
print()
print("把从「数据来源」开始的全部输出复制发给开发者即可（账号已打码，不含密钥）")
PY

MINUTES="$MINUTES" MODEL="${MODEL:-}" QE_MINUTES="$QE_MINUTES" WITHIN="${WITHIN:-}" DIAG_NOW="${DIAG_NOW:-}" DB_PATH="$DB" \
  LOG_PATHS="$LOG.1
$LOG" STATUS_JSON="$WORK/status.json" CONFIG_JSON="$WORK/config.json" COOLDOWNS_JSON="$WORK/cooldowns.json" \
  REQUESTS_JSON="$WORK/requests.json" ACCOUNTS_JSON="$WORK/accounts.json" AUTH_DIR="$auth_dir" API_STATE="$API_STATE" \
  WORK_DIR="$WORK" python3 -I "$WORK/diag_busy.py"
