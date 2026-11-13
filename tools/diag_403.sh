#!/usr/bin/env bash
# AIStudio2API 一键诊断"没调用过的模型出现在 403 无权限账户列表里"（管理页“403 无权限账户”与“出问题”列出
# 403 无权限（gemini-omni-1.1-flash）：The caller does not have permission，自己却没有调用过这个模型）：这次 403 是谁、什么时候、
# 从哪个接口、以什么形态调用的。依次输出：
#   1. 版本与实例：本目录进程的版本（是否早于 93c16fa：带图的 Omni 403 会误登记正常账户）、本机所有 aistudio2api 实例与各自的账户目录
#      （多个实例共用一个账户目录时登记文件是共用的）、登记文件最后写入时间、源码版本与最近的升级、重启记录
#   2. 403 登记：每个登记账户在这个模型上的通道、首次与最近一次的时间、次数、上游方法，以及新版本记下的最近一次请求来源
#      （last_request：请求 ID、接口、请求名、消息条数、内联图片数、文件引用数）
#   3. 用量账本最近 DAYS 天（默认 92，最多 92）这个模型的请求：按接口、按天（总数/成功）；每个模型最多统计最近的 10000 条，超过时写明
#   4. 每个登记账户最近一次 403 对应的请求与经过：登记带 last_request 时按请求 ID 找（本实例账本里没有这个 ID 时写明），没有时按时间
#      在账本里单独查一个窄窗口（请求结束时间在最近一次 403 前 2 分钟到后 60 分钟、且这个账户在其中返回过 403），再到运行日志的
#      request.started 里看请求形态
#   5. 目录里这个模型的别名（别名本身不是目录 ID 时，请求这个别名会落到这个模型上、登记在它名下）
#   6. 运行日志里这个模型的 403 冷却与需要验证账号的暂停行
#   7. 结论：每个登记账户一行（带最近一次 403 的本机时间与请求 ID，对应管理页 403 列表的行），按 README“没有调用过的模型出现在 403 列表里”
#      的判断：多半是旧记录（含升级到含 93c16fa 的版本之前带图 Omni 请求的误登记）/ new-api 渠道测试 / 下游主动调用 / 别名 /
#      额度用完改用（本服务的 MODEL_FALLBACKS）/ 模型重定向或改用（未确认）/ 直连 interactions / 账户确实无权限 /
#      账本里没有对应请求（可能是共用账户目录的其他实例或复制来的目录）
# 只读：只对本机管理接口发 GET 请求，只读 .env、运行日志（logs/aistudio2api.log 与 .1）、登记文件（管理接口连不上时）与进程信息，
# 不改任何配置、记录、冷却或账户；邮箱已打码，不输出管理令牌、API 密钥与提示词（运行日志与账本本来也不记提示词）。
#
# 用法（放在程序目录的 tools/ 下运行，或用 APP 指定程序目录）：
#   bash tools/diag_403.sh                        查 gemini-omni-1.1-flash（默认）
#   MODEL=gemini-4-carbon bash tools/diag_403.sh   查别的模型
#   MODEL=all bash tools/diag_403.sh               查登记里的所有模型
#   ACCOUNT=abc bash tools/diag_403.sh             只看邮箱（账户 ID）含 abc 的账户（不区分大小写）
#   APP=/path/to/app bash tools/diag_403.sh        指定程序目录（含 start.sh、.env、.admin-token、logs/、auth/；默认为脚本所在的程序目录，
#                                                  找不到时为 /www/wwwroot/Chat2API/build）
#   DAYS=30 bash tools/diag_403.sh                 用量账本回看的天数（默认 92，账本保留约 90 天；查询范围最多 93 天，超过 92 按 92）
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
PYTHON="${PYTHON:-python3}"
case "$DAYS" in "" | *[!0-9]* | 0) echo "DAYS 必须是正整数"; exit 1 ;; esac
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
echo "程序目录：$APP${ACCOUNT:+；只看账户含 \"$ACCOUNT\" 的}"

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
  INSTANCES="$INSTANCES$p|$d|$a
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

export APP MODEL DAYS FIX_COUNT PROC_START SHARED APP_AUTH
"$PYTHON" -I - <<'PY'
# 只读：只对本机管理接口发 GET、只读日志与登记文件；邮箱打码，不输出令牌与提示词。兼容 Python 3.7（不用 3.8 以后的语法）
import collections
import datetime
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
    # 排查路由 /trace/ 与 Ultra 路由 /ultra/ 的请求去掉前缀后再归类
    path = path or ""
    for prefix in ("/trace", "/ultra"):
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


def fallback_applies(path):
    # MODEL_FALLBACK_SCOPE：ultra（默认）只对 /ultra 路径的请求改用，all 对所有路径，off 关闭；路径不知道时按可能
    if FALLBACK_SCOPE == "off":
        return False
    return FALLBACK_SCOPE == "all" or not path or path.startswith("/ultra/")


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


def get(path, timeout=30):
    """返回 (数据, HTTP 状态, 错误说明)；连不上或令牌被拒绝时之后不再访问。状态 0 为没有响应"""
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
    except (urllib.error.URLError, OSError, ValueError) as error:
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
entries = []
for account in listed.get("accounts") or []:
    account_id, label = account.get("id") or "", account.get("label") or account.get("id") or ""
    if not wanted_account(account_id, label):
        continue
    models = account.get("models") or []
    for item in models:
        if not ALL and item.get("model") != MODEL:
            continue
        entries.append({
            "id": account_id, "label": label, "model": item.get("model") or "", "channels": item.get("channels") or [],
            "first": parse_time(item.get("first_at")), "last": parse_time(item.get("last_at")), "count": item.get("count") or 0,
            "message": item.get("message") or "", "last_request": item.get("last_request") or None,
            "others": [m.get("model") for m in models if m.get("model") != item.get("model")],
        })
entries.sort(key=lambda e: e["last"] or datetime.datetime.min.replace(tzinfo=UTC), reverse=True)
for entry in entries:
    method = re.search(r"AI Studio (\w+) 返回", entry["message"])
    line = "%s | %s | 通道 %s | 首次 %s | 最近 %s | %d 次 | 方法 %s | 该账户其他 403 模型 %d 个" % (
        mask(entry["label"]), entry["model"], ",".join(entry["channels"]) or "-", show(entry["first"]), show(entry["last"]),
        entry["count"], method.group(1) if method else "-", len(entry["others"]))
    if ACCOUNT_LEVEL.search(entry["message"]):
        line += " | 需要验证账号（账户级）"
    print(line)
    last = entry["last_request"]
    if last:
        print("    最近一次请求：%s · 请求名 %s · %d 条消息 · 图片 %d · 文件 %d · ID %s" % (
            last.get("path") or "-", last.get("requested_model") or entry["model"], last.get("messages") or 0,
            last.get("images") or 0, last.get("files") or 0, last.get("id") or "-"))
print("共 %d 条（来源：%s）" % (len(entries), source) if entries else "没有%s的登记（来源：%s）" % ("" if ALL else "这个模型", source))

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
    return "运行日志从 %s 开始，晚于这次 403：已轮转（只保留当前和上一份，各约 50MB）" % show(log_since)


print("\n== 4. 每个登记账户最近一次 403 对应的请求与经过（运行日志：%s）" % ("、".join(log_files) or "没有"))
for entry in entries:
    head = "%s · %s：" % (mask(entry["label"]), entry["model"])
    record, last = entry["record"], entry["last_request"] or {}
    request_id = (record or {}).get("id") or last.get("id") or ""
    start = starts.get(request_id) or {}
    entry["start"], entry["finish"], entry["request_id"] = start, finishes.get(request_id) or {}, request_id
    # 登记带请求 ID、运行日志覆盖这次 403 却没有这个请求：账本读不到时据此判断请求不是经过本实例的
    entry["log_missing"] = bool(last.get("id")) and not start and not entry["finish"] and log_covers(entry)
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
        shape.append(log_absence(entry))
    if entry["finish"].get("fallback_from"):
        shape.append("运行日志：request.finished 带 fallback_from=%s（额度用完改用）" % entry["finish"]["fallback_from"])
    for item in shape:
        print("    " + item)

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
rows = []
for name in log_files:
    with open(os.path.join(APP, "logs", name), encoding="utf-8", errors="replace") as handle:
        for line in handle:
            if "类型=无权限" not in line and "账号暂停调度" not in line and "解除需要验证账号的整户暂停" not in line:
                continue
            try:
                item = json.loads(line)
            except ValueError:
                continue
            message = item.get("msg") or ""
            if "类型=无权限" in message and not ALL and not re.search(r"范围=(build:)?%s( |$)" % re.escape(MODEL), message):
                continue
            if not wanted_account(item.get("source"), item.get("source")):
                continue
            rows.append("%s | %s | %s" % (show(parse_time(item.get("time"))), mask(item.get("source")), mask(message)[:240]))
print("\n".join(rows[-20:]) if rows else "运行日志里没有这些行")

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
    "无权限": "需要验证账号的到官网验证后在管理页点“验证”；其余可以不管（冷却只影响这个模型），多个模型都 403 时考虑重新获取认证文件",
    "账本没有": "看第 1 段有没有多个实例共用账户目录、账户目录是不是从别处复制或恢复来的；确认后点这一行的“清除”",
    "无法对照": "按最近一次 403 的时间（或账本里这个请求的时间）到 new-api 查",
}


def shape_text(last, start):
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


def conclude(entry):
    last, start, record = entry["last_request"] or {}, entry.get("start") or {}, entry["record"]
    if ACCOUNT_LEVEL.search(entry["message"]):
        return "账户确实无权限（需要验证账号，账户级）", "无权限"
    if entry["last"] and entry["last"] < FIX_AT:
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
    media = (last.get("images") or 0) if last else (start.get("input_media") or 0)
    code7 = "协议错误码 7" in entry["message"]
    if interaction_model(entry["model"]) and media > 0 and (not last or FIX_COUNT == "0"):
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
        if (finish.get("fallback_from") or "").lower() == requested.lower() or fallback_applies(raw_path) and fallback_pair(requested, entry["model"]):
            return "额度用完改用（MODEL_FALLBACKS：客户端请求 %s，改用 %s 后 403）" % (requested, entry["model"]), "改用"
        if canonical(requested) == entry["model"].lower():
            return "别名（客户端请求的是目录别名 %s，落到了 %s）" % (requested, entry["model"]), "别名"
        return "模型重定向或改用（未确认：客户端请求 %s，登记在 %s 名下；%s）" % (
            requested, entry["model"], "目录里它现在不是这个模型的别名" if CATALOG_OK else "读不到模型目录，无法核对别名"), "未确认"
    messages = last.get("messages") or start.get("input_messages")
    if messages:
        images = (last.get("images") or 0) if last else (start.get("input_media") or 0)
        # 登记的图片只数 image/* 内联数据，内联的视频、音频只算在运行日志的媒体里：判断下游取两者较大的；渠道测试从不带媒体与文件，
        # 运行日志里媒体或文件不为 0 的在这里归为下游，不会进入下面的渠道测试判定
        inline = max(images, start.get("input_media") or 0)
        files = max(last.get("files") or 0, start.get("input_files") or 0)
        if images and interaction_model(entry["model"]) and not code7:
            return "下游带图调用 Omni（%s）：新版本只在 403 的错误体不带协议错误码 7 时（出口或 GFE 的 HTML 错误页）才登记带图请求，" \
                   "多半与账户权限无关" % shape_text(last, start), "下游带图"
        if inline or files or messages > 1:
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
    return "无法判断请求形态（账本里有这个请求，运行日志里没有它的开始行）", "无法对照"


print("\n== 7. 结论（每个登记账户一行，带最近一次 403 的本机时间，与管理页 403 列表的“最近一次”相同；规则见 README“没有调用过的模型出现在 403 列表里”）")
if not entries:
    print("没有要判断的登记。")
for entry in entries:
    text, kind = conclude(entry)
    notes = []
    if entry["id_missing"] and (entry["start"] or entry["finish"]):
        notes.append(LEDGER_PENDING)
    if entry["others"] and kind != "无权限":
        notes.append("该账户另有 %d 个模型 403，账户本身可能受限" % len(entry["others"]))
    stats = by_model.get(entry["model"])
    day = show(entry["last"])[:10] if entry["last"] else ""
    if kind not in ("旧记录", "无权限", "账本没有") and entry["count"] >= 3 and entry["last"] and (NOW - entry["last"]).days < 3 and stats and stats["days"].get(day, [0, 0])[1] > 0:
        notes.append("近期反复 403、同一天这个模型有成功：这个账户多半确实不能用这个模型")
    where = "最近 %s" % short(entry["last"])
    if entry["request_id"]:
        where += " · 请求 %s" % entry["request_id"]
    print("%s · %s（%s）：%s%s → %s" % (mask(entry["label"]), entry["model"], where, text, ("；" + "；".join(notes)) if notes else "", ADVICE[kind]))
if FIX_COUNT == "0":
    print("⚠ 正在运行的程序早于 93c16fa：带图的 Omni 请求被拒时仍会误登记正常账户，先升级")
PY
