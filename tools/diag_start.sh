#!/usr/bin/env bash
# AIStudio2API 一键诊断“Worker 起不来”：系统资源 → 状态接口（Worker、预热、IP 池、出口）→ 绕过程序直连测 IP 池端口 →
# 最近的 Worker 启动次数、就绪耗时、失败原因与代理日志 → 失败时的页面状态（启动失败与 Worker 重建日志末尾的“页面=…”快照）
# → AI Studio 首次使用页（自动完成、跳过与未能完成的次数，失败停在哪一步、原因、“已做”与结构摘要样本）
# 组织停用 AI Studio 的账户（Google Workspace 账号所属组织关闭了 AI Studio，载入时跳到 access.workspace.google.com）：状态段显示
# 现在的账户数，启动失败原因里单独归类（新旧两种文案），并统计“账号所属组织已停用 AI Studio，停止调度”的来源
# 官网停用 Build 通道的生成（PROXY_GENERATION_DISABLED）：状态段显示 Build 通道是否全局暂停、开始与下次试探时间、暂停中与等待试探的账号数
# 只读：只对本机管理接口发 GET 请求、读运行日志，直连测试只访问 api.ipify.org 与 aistudio.google.com 首页；
# 不改配置、不重启。输出同时写到 $OUT（默认 logs/diag_start.txt），代理密码与账号邮箱已打码
#
# 用法（放在程序目录的 tools/ 下运行，或用 APP_DIR 指定程序目录）：
#   bash tools/diag_start.sh
#   LOG_MINUTES=60 bash tools/diag_start.sh       统计最近 60 分钟的日志（默认 15）
#   APP_DIR=/path/to/app OUT=/root/diag_start.txt bash tools/diag_start.sh
set -u
script_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd)"
if [ -z "${APP_DIR:-}" ]; then
  for candidate in "$script_dir/.." "$PWD" /www/wwwroot/Chat2API/build; do
    if [ -f "$candidate/start.sh" ]; then APP_DIR="$(cd "$candidate" && pwd)"; break; fi
  done
fi
APP_DIR="${APP_DIR:-/www/wwwroot/Chat2API/build}"
: "${OUT:=$APP_DIR/logs/diag_start.txt}" "${LOG_MINUTES:=15}"
: "${CHECK_URL:=https://api.ipify.org}" "${GOOGLE_URL:=https://aistudio.google.com/}"
case "$LOG_MINUTES" in "" | *[!0-9]* | 0) echo "LOG_MINUTES 必须是正整数"; exit 1 ;; esac
command -v python3 >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }
cd "$APP_DIR" || { echo "找不到程序目录 $APP_DIR"; exit 1; }
mkdir -p "$(dirname "$OUT")" || exit 1
env_value() { grep -E "^$1=" .env | tail -n 1 | cut -d= -f2- | sed 's/^["'\'']//; s/["'\'']$//'; }
PORT=$(env_value LISTEN_ADDR | sed 's/.*://'); PORT=${PORT:-2048}
A="http://127.0.0.1:$PORT"
T=$(tr -cd '0-9a-fA-F' < .admin-token 2>/dev/null)
POOL_MODE=$(env_value PROXY_POOL_MODE); POOL_URL=$(env_value PROXY_POOL_URL)
POOL_START=$(env_value PROXY_POOL_PORT_START); POOL_END=$(env_value PROXY_POOL_PORT_END)
SECRETS=$(echo "$POOL_URL" | sed -n 's#^[a-zA-Z0-9]*://\([^@/]*\)@.*#\1#p')
export SECRETS

main() {
  echo "==================== Worker 启动诊断 $(date '+%F %T %z')（UTC $(date -u '+%T')）===================="
  echo "版本：$(git log --oneline -1 2>/dev/null | cut -c1-80)；CPU $(nproc) 核；负载 $(cut -d' ' -f1-3 /proc/loadavg)"
  free -m | awk 'NR == 2 {print "内存：已用 " $3 " MB / 共 " $2 " MB，可用 " $7 " MB"}'
  echo "浏览器主进程 $(pgrep -c -x camoufox-bin) 个；全系统线程 $(ps -eo nlwp= | awk '{s += $1} END {print s}')，上限 $(cat /proc/sys/kernel/threads-max)，用户进程数上限 $(ulimit -u)"
  pid=$(pgrep -x aistudio2api | head -n 1)
  if [ -n "$pid" ]; then
    echo "程序进程 $pid：文件数上限 $(awk '/open files/ {print $4}' "/proc/$pid/limits")，已打开 $(ls "/proc/$pid/fd" 2>/dev/null | wc -l)；线程 $(awk '/^Threads/ {print $2}' "/proc/$pid/status")"
  fi
  df -h "$APP_DIR" | awk 'NR == 2 {print "磁盘：已用 " $3 " / " $2 "，剩余 " $4}'

  echo
  echo "==================== 状态接口 ===================="
  status_file=$(mktemp) || return
  curl -sS --noproxy '*' -m 15 -H "X-Admin-Token: $T" "$A/api/status" > "$status_file"
  python3 -I - "$status_file" <<'PY'
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as f:
        d = json.load(f)
except Exception as err:
    print("读取失败：", err)
    raise SystemExit
w = d.get("workers", {})
p = w.get("prewarm", {})
print(f"运行 {d.get('running')}；Worker 就绪 {w.get('warm')} / 目标 {w.get('target')} / 峰值 {w.get('max')}，启动中 {w.get('starting')}，占用槽位 {w.get('occupied')}，死槽位 {w.get('dead')}")
a = d.get("accounts") or {}
if a:
    # org_disabled 为所属组织停用了 AI Studio、已停止调度的账户；程序版本早于组织停用识别时没有这个字段，显示为 -
    org = a.get("org_disabled")
    print(f"账户：共 {a.get('total')}，就绪 {a.get('ready')}，忙碌 {a.get('busy')}，冷却 {a.get('cooldown')}，需要登录 {a.get('auth_required')}，"
          f"组织已停用 AI Studio {'-' if org is None else org}")
print(f"预热：进行中 {p.get('active')}，在途 {p.get('inflight')}，本轮已启动 {p.get('launched')}，本轮已 {p.get('round_seconds')} 秒，原因 {p.get('reason') or '-'}")
u = d.get("ultra_workers") or {}
if u:
    print(f"Ultra 分区：就绪 {u.get('warm')}，启动中 {u.get('starting')}，目标 {u.get('target')}，峰值 {u.get('max')}")
for item in d.get("primary_supply") or []:
    print("主用模型供给：" + json.dumps(item, ensure_ascii=False)[:240])
pool = d.get("proxy_pool") or {}
if pool:
    print(f"IP 池：模式 {pool.get('mode')}，端口 {pool.get('port_start')}–{pool.get('port_end')} 共 {pool.get('total')}，使用中 {pool.get('in_use')}，租约 {pool.get('leases')}，冷却 {pool.get('cooling')}，共用 {pool.get('shared')}，外部占用 {pool.get('external')}，固定默认代理的账户 {pool.get('pinned_default')}，中转连接上限 {pool.get('relay_limit')}")
    for port in (pool.get("cooling_ports") or [])[:10]:
        print(f"  冷却端口 {port.get('port')}：第 {port.get('level')} 档，到 {port.get('until')}")
    for port in (pool.get("hot_ports") or [])[:5]:
        print(f"  裸 429 多的端口 {port.get('port')}：{port.get('rate_limited')} 次 / {port.get('accounts')} 个账号")
    check = pool.get("check")
    if check:
        print("  最近一次检查全部端口：" + json.dumps(check, ensure_ascii=False)[:300])
s = d.get("egress_summary")
if s:
    print(f"出口：共 {s.get('total')}，正常 {s.get('normal')}，判定限流 {s.get('limited')}，已撤销 {s.get('rejected')}")
# build_channel 为 Build 通道生成的暂停状态（官网停用经 Build 代理的生成时）；没有启用 Build、服务未运行或程序版本较旧时没有这个字段
b = d.get("build_channel")
if not isinstance(b, dict):
    print("Build 通道：-（没有启用 Build、服务未运行或程序版本较旧）")
elif b.get("paused"):
    # 全局暂停时所有账号都不走 Build，paused_accounts 为遇到过 PROXY_GENERATION_DISABLED 的账号数
    probing = "（试探中）" if b.get("probing") else ""
    print(f"Build 通道：官网停用了生成（PROXY_GENERATION_DISABLED），全局暂停，生成改走 Playground；开始 {b.get('since') or '-'}，"
          f"下次试探 {b.get('next_probe') or '-'}{probing}，遇到过的账号 {b.get('paused_accounts')}")
elif b.get("paused_accounts"):
    awaiting = f"；另有 {b.get('awaiting_probe')} 个账号暂停已到期，等待试探" if b.get("awaiting_probe") else ""
    print(f"Build 通道：{b.get('paused_accounts')} 个账号暂停（官网对它们停用了 Build 生成，改走 Playground），其余照常{awaiting}")
elif b.get("awaiting_probe"):
    print(f"Build 通道：{b.get('awaiting_probe')} 个账号的暂停已到期，等待试探（它们在 Build 上的下一次请求成功即恢复）")
else:
    print("Build 通道：正常")
PY
  rm -f -- "$status_file"

  echo
  echo "==================== 直连测 IP 池端口（不经程序，看 Decodo 本身）===================="
  echo "模式 ${POOL_MODE:-off}，端口 ${POOL_START:-?}–${POOL_END:-?}"
  if [ "${POOL_MODE:-off}" = static ] && [ -n "$POOL_URL" ] && [ -n "$POOL_START" ] && [ -n "$POOL_END" ]; then
    hostpart=$(echo "$POOL_URL" | sed -E 's#^([a-zA-Z0-9]+)://([^@/]*@)?([^:/]+).*#\1://\3#')
    auth=$(echo "$POOL_URL" | python3 -I -c 'import sys, urllib.parse as u
p = u.urlsplit(sys.stdin.read().strip())
print(u.unquote(p.username or "") + ":" + u.unquote(p.password or "") if p.username else "")')
    span=$((POOL_END - POOL_START))
    for port in "$POOL_START" $((POOL_START + span / 4)) $((POOL_START + span / 2)) $((POOL_START + span * 3 / 4)) "$POOL_END"; do
      opt=(); [ -n "$auth" ] && opt=(-U "$auth")
      ip=$(curl -sS -m 15 "${opt[@]}" -x "$hostpart:$port" -w ' %{http_code} %{time_total}s' "$CHECK_URL" 2>&1 | tr '\n' ' ' | cut -c1-120)
      g=$(curl -sS -m 20 "${opt[@]}" -x "$hostpart:$port" -o /dev/null -w '%{http_code} %{time_total}s' "$GOOGLE_URL" 2>&1 | tr '\n' ' ' | cut -c1-120)
      echo "  端口 $port：出口 $ip；AI Studio $g"
    done
    echo "到端口段的已建立连接：$(ss -Htn state established "( dport >= :$POOL_START and dport <= :$POOL_END )" | wc -l) 条（Decodo 线程上限按两台服务器合计算）"
  else
    echo "IP 池没有开启静态模式，跳过"
  fi

  echo
  echo "==================== 最近 $LOG_MINUTES 分钟的 Worker 启动与代理日志 ===================="
  cooldown_file=$(mktemp) || return
  curl -sS --noproxy '*' -m 30 -H "X-Admin-Token: $T" "$A/api/cooldowns" > "$cooldown_file" 2>/dev/null
  python3 -I - "$APP_DIR/logs/aistudio2api.log" "$LOG_MINUTES" "$cooldown_file" <<'PY'
import collections, datetime, json, os, re, statistics, sys
path, minutes, cooldown_path = sys.argv[1], int(sys.argv[2]), sys.argv[3]
denied = set()
try:
    with open(cooldown_path, encoding="utf-8") as f:
        for item in json.load(f).get("cooldowns") or []:
            if "无权限" in (item.get("reason") or "") or "403" in (item.get("reason") or ""):
                denied.add(item.get("account_label") or item.get("account_id"))
except Exception:
    denied = None
TIME_RE = re.compile(r'^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$')
def parse_time(value):
    m = TIME_RE.match((value or "").strip())
    if not m:
        return None
    zone = "+00:00" if m.group(3) == "Z" else m.group(3)
    try:
        return datetime.datetime.fromisoformat(f"{m.group(1)}.{(m.group(2) or '0')[:6].ljust(6, '0')}{zone}")
    except ValueError:
        return None
def duration(text):
    total, found = 0.0, False
    for number, unit in re.findall(r"([\d.]+)(ms|h|m|s)", text or ""):
        found = True
        total += float(number) * {"ms": 0.001, "s": 1, "m": 60, "h": 3600}[unit]
    return total if found else None
def fields(msg):
    out = {}
    for part in msg.split("\n")[0].split(" | ")[1:]:
        key, sep, value = part.partition("=")
        if sep:
            out[key.strip()] = value.strip()
    return out
since = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(minutes=minutes)
paths = [p for p in (path + ".1", path) if os.path.exists(p)]
if not paths:
    print("读不到日志")
    raise SystemExit
DIGITS = re.compile(r"\d+")
heads, kinds, samples = collections.Counter(), collections.Counter(), {}
fail_reasons, fail_ports, proxy_heads, ready_secs = collections.Counter(), collections.Counter(), collections.Counter(), []
fail_lines = []
last_step = {}
fail_steps, fail_full, fail_accounts, ready_accounts = collections.Counter(), {}, set(), set()
STEP_RE = re.compile(r"^WAA Worker 启动 \| (\d)/7 \| ([^|\n]+)")
# 失败时的页面状态：页面类失败的错误末尾带“ | 页面={…}”或“ | 页面=快照失败: <原因>”（见 docs/logging.md）。
# 快照里的“|”已换成“¦”、换行已转义，只占一行、不含“ | ”；快照之后可能还接着本机代理中转的说明（“；本机代理中转：…”，
# 接在整条错误之后）与日志字段（“ | 端口=…”）。超长被硬截断时 JSON 不完整，只按正则取分组与分布用的几个字段
PAGE_MARK = " | 页面="
PAGE_RE = re.compile(r" \| 页面=.*?(?=；本机代理中转：| \| |\n|$)")
# 启动失败、Worker 重建，以及运行中准备 proof 失败后没等到 Worker 关闭就换号的“账号切换”（同一次失败只记其中一种）
PAGE_HEADS = ("WAA Worker 启动失败", "WAA Worker 重建", "账号切换")
# 账号切换里运行中准备 proof 的失败：普通请求为“准备 fresh WAA proof: …”，Live（bidi）通道为“准备 bidi setup fresh WAA proof: …”与“准备 bidi fresh WAA proof: …”
PROOF_REASON = re.compile(r"准备 (?:bidi (?:setup )?)?fresh WAA proof")
# 组织停用 AI Studio（Google Workspace 账号所属组织关闭了 AI Studio）：新版文案以 ORG_PREFIX 开头（页面停在 access.workspace.google.com，
# 立即失败）；旧版程序在这一页干等到超时，文案为“AI Studio 输入框未就绪 url=https://access.workspace.google.com/…: context deadline exceeded”。
# 启动失败原因里两种都归到 ORG_LABEL；“账号所属组织已停用 AI Studio，停止调度 | 来源=…”为新版进入组织停用状态的日志（同一账户只写一行）
ORG_PREFIX = "AI Studio 已被账号所属组织停用"
ORG_HOST = "access.workspace.google.com"
ORG_LABEL = "组织停用 AI Studio（页面停在 access.workspace.google.com）"
ORG_ENTER = "账号所属组织已停用 AI Studio，停止调度"
def org_disabled_error(error):
    """启动失败的原错误是否为组织停用（新版文案，或旧版停在 access.workspace.google.com 的“输入框未就绪”）"""
    return ORG_PREFIX in (error or "") or ORG_HOST in (error or "")
org_fail_new, org_fail_old, org_fail_accounts = 0, 0, set()
org_enter_sources, org_enter_accounts, org_catalog_failed = collections.Counter(), set(), 0
page_total, page_heads, page_groups, page_samples, page_kinds = collections.Counter(), collections.Counter(), collections.Counter(), {}, {}
# 分组键相同的失败可能根因不同（改版、整页刷新、一直没加载出来）：按（分组, 失败原因）另统计这几项的分布（见 PAGE_DIST），各留最近一条样本
page_dist = {}
snapshot_failures = collections.Counter()
# AI Studio 首次使用页（见 docs/logging.md）：Worker 启动时自动完成写“完成 AI Studio 首次使用页 | 勾选=N（标签；[补]标签） | 按钮=… |
# 已做=勾选:…；点击:…；跳过:… | 耗时=…”（括号里为各次勾选的标签，已打码截短，按放宽规则补勾的带“[补]”；已做为按顺序做过的每一步，
# 跳过只在不涉及同意的步骤里点，如用途调查），账户验证、登录与添加时写“账户验证 | 完成 AI Studio 首次使用页 | …”（账户登录、账户添加同理）；
# 没能完成的错误以“AI Studio 首次使用页未能完成: <原因>”开头（启动失败、账户验证失败），原因之后接“ | 已做=…”（这次做过的每一步，
# 什么都没做时为“-”；次数用完的失败没有），再接页面快照与“ | 结构={…}”（勾选框与按钮的结构摘要，已打码；scope 为查找范围，cur 为当前一步
# 所在的 ms- 组件（当前一步的勾选框、继续与跳过按钮、标题所在的组件），即停在多步向导的哪一步；parts 为范围里可见的 ms- 组件，可能含同时
# 渲染的其他各步与常驻的卡片预览，旧版日志没有 cur 时退回按 parts 看）；订阅网络事件之后才到首次使用页时原错误不变，末尾补“；已完成首次使用页”
# （同一次就地完成另写了上面那行 INFO）、“；首次使用页已自行离开”（识别之后页面自己离开、什么都没点，不写 INFO）或
# “；首次使用页未能完成: <原因> | 已做=…”。同一次失败还会出现在“账号切换 | …\n原因: 获取账户 WAA preparer: …”（请求触发的冷启动）
# 与“新账户自动处理出错…”里，只按 ONBOARD_FAIL_HEADS 计
ONBOARD_DONE_RE = re.compile(r"^(?:(账户验证|账户登录|账户添加) \| )?完成 AI Studio 首次使用页(?: \||$)")
ONBOARD_CHECKED_RE = re.compile(r"^(\d+)(?:（(.*)）)?$")
ONBOARD_RELAXED = "[补]"
ONBOARD_FAIL_HEADS = ("WAA Worker 启动失败", "账户验证失败")
ONBOARD_FAIL_RE = re.compile(r"AI Studio 首次使用页未能完成: ([^|\n]*)")
ONBOARD_INPLACE_DONE = "；已完成首次使用页"
ONBOARD_INPLACE_LEFT = "；首次使用页已自行离开"
ONBOARD_INPLACE_FAIL_RE = re.compile(r"；首次使用页未能完成(?:: ([^|\n]*))?")
STRUCT_RE = re.compile(r" \| 结构=(\{.*?)(?=；本机代理中转：| \| |\n|$)")
ONBOARD_PROGRESS_RE = re.compile(r" \| 已做=(.*?)(?=；本机代理中转：| \| |\n|$)")
ONBOARD_CHECK_MARK, ONBOARD_CLICK_MARK, ONBOARD_SKIP_MARK = "勾选:", "点击:", "跳过:"
STRUCT_SCOPE_RE = re.compile(r'"scope":"([^"]*)"')
STRUCT_PARTS_RE = re.compile(r'"parts":\[([^\]]*)\]')
STRUCT_CUR_RE = re.compile(r'"cur":\[([^\]]*)\]')
onboard_done, onboard_checked, onboard_buttons, onboard_secs = collections.Counter(), collections.Counter(), collections.Counter(), []
onboard_labels, onboard_relaxed_runs, onboard_relaxed_boxes = collections.Counter(), 0, 0
onboard_fail_heads, onboard_fail_reasons, onboard_structures = collections.Counter(), collections.Counter(), []
onboard_inplace_done, onboard_inplace_left = 0, 0
onboard_skip_runs, onboard_skip_total, onboard_progress = 0, 0, collections.Counter()
onboard_fail_steps, onboard_fail_parts, onboard_fail_done, onboard_fail_samples = collections.Counter(), collections.Counter(), collections.Counter(), []
def strip_page(text):
    """去掉页面快照、首次使用页的进度记录与结构摘要（见 ONBOARD_PROGRESS_RE、STRUCT_RE，汇总在最后一段），之后的本机代理中转说明与日志字段照常保留"""
    return STRUCT_RE.sub("", ONBOARD_PROGRESS_RE.sub("", PAGE_RE.sub("", text or "")))
def progress_items(progress):
    """“已做”拆成各步（“-”为什么都没做，返回空列表）"""
    return [item for item in (progress or "").split("；") if item and item != "-"]
def onboard_step(structure):
    """失败停在多步向导的哪一步：结构摘要的查找范围与当前一步所在的 ms- 组件（cur），写成“ms-onboarding: ms-intent-selection+…”；
    旧版日志没有 cur 时退回按范围里可见的 ms- 组件（parts，可能含同时渲染的其他各步）写，并标上“（parts）”。返回（步骤, cur 组件列表）"""
    if not structure:
        return "（没有结构摘要）", []
    scope = STRUCT_SCOPE_RE.search(structure)
    name = scope.group(1) if scope else "?"
    cur_match = STRUCT_CUR_RE.search(structure)
    if cur_match:
        cur = re.findall(r'"([^"]*)"', cur_match.group(1))
        return (name + ": " + "+".join(cur) if cur else name), cur
    parts_match = STRUCT_PARTS_RE.search(structure)
    parts = re.findall(r'"([^"]*)"', parts_match.group(1)) if parts_match else []
    return (name + ": " + "+".join(parts) + "（parts）" if parts else name), []
def onboard_other_parts(structure, cur):
    """范围里可见、不在 cur 里的 ms- 组件（parts，按原顺序），写进“已做”样本：cur 只剩共用页脚（ms-horizontal-stepper）时据此看停在哪一步；
    没有 cur（旧版日志，步骤已按 parts 写）时为空"""
    if not cur:
        return []
    parts_match = STRUCT_PARTS_RE.search(structure or "")
    parts = re.findall(r'"([^"]*)"', parts_match.group(1)) if parts_match else []
    return [part for part in parts if part not in cur]
def start_error(msg):
    """启动失败的原错误（去掉页面快照、拼回快照之后的本机代理中转说明）"""
    return fields(strip_page(msg)).get("错误", "")
def page_reason(head, clean):
    """失败原因（已去掉页面快照）：启动失败取“错误=”字段，重建与账号切换取下一行“原因: ”之后"""
    if head == "WAA Worker 启动失败":
        return fields(clean).get("错误", clean.split("\n")[0]).strip()
    return clean.split("\n原因: ", 1)[-1].strip()
EXCEPTION_MARK = "页面表达式异常: "
def collapse_exception(reason):
    """页面表达式异常的 JSON（键按字母序，报错正文 text 排在最后）折叠成报错正文：截短后也看得到“提示词输入框不存在”等原因"""
    pos = 0
    while True:
        at = reason.find(EXCEPTION_MARK, pos)
        if at < 0:
            return reason
        start = at + len(EXCEPTION_MARK)
        rest, text, end = reason[start:], None, 0
        try:
            details, end = json.JSONDecoder().raw_decode(rest)
            if isinstance(details, dict) and isinstance(details.get("exceptionDetails"), dict):
                text = details["exceptionDetails"].get("text")
        except ValueError:
            m = re.search(r'"text":"((?:[^"\\]|\\.)*)"', rest)
            if m:
                try:
                    text = json.loads('"' + m.group(1) + '"')
                except ValueError:
                    text = m.group(1)
                end = len(rest)
        if not text:
            pos = start
            continue
        reason = reason[:start] + str(text) + rest[end:]
        pos = start + len(str(text))
def loose_string(text, key):
    m = re.search('"' + key + r'":("(?:[^"\\]|\\.)*")', text)
    try:
        return json.loads(m.group(1)) if m else None
    except ValueError:
        return None
def page_snapshot(value):
    """返回（快照字段, 快照原文）；“快照失败: …”返回 (None, value)"""
    if not value.startswith("{"):
        return None, value
    try:
        snap, end = json.JSONDecoder().raw_decode(value)
        if isinstance(snap, dict):
            return snap, value[:end]
    except ValueError:
        pass
    snap = {key: loose_string(value, key) for key in ("url", "title", "ready", "nav")}
    for key in ("input", "run"):
        m = re.search('"' + key + r'":\[([\d,]*)\]', value)
        if m:
            snap[key] = [int(x) for x in m.group(1).split(",") if x]
    for key in ("age", "since"):
        m = re.search('"' + key + r'":(\d+)', value)
        if m:
            snap[key] = int(m.group(1))
    m = re.search(r'"hook":(true|false)', value)
    if m:
        snap["hook"] = m.group(1) == "true"
    m = re.search(r'"ta":\["((?:[^"\\]|\\.)*)', value)
    if m:
        snap["ta"] = [m.group(1)]
    # 截断多半落在弹层或正文里：第一条弹层文字可能没有结尾的引号
    m = re.search(r'"overlays":\["((?:[^"\\]|\\.)*)', value)
    if m:
        try:
            snap["overlays"] = [json.loads('"' + m.group(1) + '"')]
        except ValueError:
            snap["overlays"] = [m.group(1)]
    return snap, value
def page_count(snap, key):
    value = snap.get(key)
    return str(value[1]) if isinstance(value, list) and len(value) > 1 else "?"
def page_replaced(snap):
    """导航之后文档是否被替换过（刷新、replace/assign 跳转、meta refresh）：since（程序导航至今）比 age（当前文档载入至今）多 5 秒以上为“是”"""
    age, since = snap.get("age"), snap.get("since")
    if not isinstance(age, int) or not isinstance(since, int):
        return "-"
    return "是" if since - age > 5 else "否"
def page_component(snap):
    """第一个可见文本输入框所在的组件（ta 首项“@”之后、“:”之前）"""
    editors = snap.get("ta")
    if not isinstance(editors, list) or not editors:
        return "-"
    return str(editors[0]).split("@", 1)[-1].split(":", 1)[0].strip() or "-"
def page_flag(snap, key):
    value = snap.get(key)
    if isinstance(value, bool):
        return "true" if value else "false"
    return str(value) if value else "-"
PAGE_DIST = (
    ("nav", lambda snap: page_flag(snap, "nav")),
    ("hook", lambda snap: page_flag(snap, "hook")),
    ("ready", lambda snap: page_flag(snap, "ready")),
    ("导航后换过文档", page_replaced),
    ("输入组件", page_component),
)
def format_dist(dists):
    """把若干份分布合起来，写成“nav: navigate×3 reload×2；hook: …”"""
    parts = []
    for label, _ in PAGE_DIST:
        total = collections.Counter()
        for dist in dists:
            total.update(dist[label])
        parts.append(label + ": " + " ".join(f"{value}×{n}" for value, n in total.most_common(4)))
    return "；".join(parts)
def page_key(snap):
    overlays = snap.get("overlays")
    overlay = str(overlays[0])[:40] if isinstance(overlays, list) and overlays else "-"
    path = str(snap.get("url") or "-").split("?", 1)[0]
    return (path, str(snap.get("title") or "-")[:60], page_count(snap, "input"), page_count(snap, "run"), overlay)
for p in paths:
    with open(p, "rb") as handle:
        size = os.path.getsize(p)
        handle.seek(max(0, size - (96 << 20)))
        if handle.tell():
            handle.readline()
        for raw in handle:
            try:
                event = json.loads(raw)
            except ValueError:
                continue
            moment = parse_time(event.get("time", ""))
            if not moment or moment < since:
                continue
            msg = str(event.get("msg", ""))
            head = msg.split("\n")[0].split(" | ")[0].strip()
            level = str(event.get("level", "")).upper()
            source = str(event.get("source", ""))
            step = STEP_RE.match(msg)
            if step:
                last_step[source] = f"{step.group(1)}/7 {step.group(2).strip()}"
            if head.startswith("WAA Worker") or head.startswith("Ultra Worker"):
                heads[head] += 1
                f = fields(msg)
                if head == "WAA Worker 就绪":
                    ready_accounts.add(source)
                # 原错误另带页面快照时，“错误=”字段之外还要拼回快照之后的本机代理中转说明（start_error）；页面快照见最后一段
                error = start_error(msg) if f.get("错误") else ""
                if head == "WAA Worker 启动失败":
                    fail_accounts.add(source)
                    fail_steps[last_step.get(source, "（没有步骤日志）")] += 1
                    key = DIGITS.sub("#", collapse_exception(error))[:120]
                    fail_full.setdefault(key, (error or msg.split("\n")[0])[:900])
                if "就绪" in head and f.get("耗时"):
                    seconds = duration(f["耗时"])
                    if seconds is not None:
                        ready_secs.append(seconds)
                if "失败" in head:
                    if org_disabled_error(error):
                        # 组织停用单独归类（后台扩容、替补等其他失败日志带同一错误时注明是哪一种）；启动失败另按新旧文案计数、账号去重
                        fail_reasons[ORG_LABEL + ("" if head == "WAA Worker 启动失败" else f"（{head}）")] += 1
                        if head == "WAA Worker 启动失败":
                            org_fail_accounts.add(source)
                            if ORG_PREFIX in error:
                                org_fail_new += 1
                            else:
                                org_fail_old += 1
                    else:
                        fail_reasons[DIGITS.sub("#", collapse_exception(error or msg.split("\n")[0]))[:170]] += 1
                    if f.get("端口"):
                        fail_ports[f["端口"]] += 1
                    fail_lines.append(f"{moment.astimezone():%H:%M:%S} {strip_page(msg).replace(chr(10), ' / ')[:400]}")
            # 账号切换只收运行中准备 proof 的失败：“获取账户 WAA preparer: …”是冷启动失败，同一次失败已记在“WAA Worker 启动失败”
            if head in PAGE_HEADS and (head != "账号切换" or PROOF_REASON.match(page_reason(head, msg))):
                page_total[head] += 1
                m = PAGE_RE.search(msg)
                if m:
                    reason = DIGITS.sub("#", collapse_exception(page_reason(head, strip_page(msg))).replace(chr(10), " / "))[:160]
                    value = m.group(0)[len(PAGE_MARK):].strip()
                    snap, text = page_snapshot(value)
                    if snap is None:
                        snapshot_failures[DIGITS.sub("#", value)[:120]] += 1
                    else:
                        key = page_key(snap)
                        page_heads[head] += 1
                        page_groups[key] += 1
                        page_samples[(key, reason)] = text
                        page_kinds.setdefault(key, collections.Counter())[reason] += 1
                        dist = page_dist.setdefault((key, reason), {label: collections.Counter() for label, _ in PAGE_DIST})
                        for label, value_of in PAGE_DIST:
                            dist[label][value_of(snap)] += 1
            done = ONBOARD_DONE_RE.match(msg)
            if done:
                f = fields(msg)
                onboard_done[done.group(1) or "Worker 启动"] += 1
                checked = ONBOARD_CHECKED_RE.match(f.get("勾选", ""))
                onboard_checked[checked.group(1) if checked else "?"] += 1
                labels = [item for item in ((checked.group(2) or "").split("；") if checked else []) if item]
                for item in labels:
                    onboard_labels[item] += 1
                relaxed = sum(1 for item in labels if item.startswith(ONBOARD_RELAXED))
                if relaxed:
                    onboard_relaxed_runs += 1
                    onboard_relaxed_boxes += relaxed
                onboard_buttons[f.get("按钮", "?")] += 1
                done_items = progress_items(f.get("已做"))
                skips = sum(1 for item in done_items if item.startswith(ONBOARD_SKIP_MARK))
                if skips:
                    onboard_skip_runs += 1
                    onboard_skip_total += skips
                if f.get("已做"):
                    onboard_progress[f["已做"][:200]] += 1
                seconds = duration(f.get("耗时"))
                if seconds is not None:
                    onboard_secs.append(seconds)
            if "首次使用页" in msg and head in ONBOARD_FAIL_HEADS:
                clean = strip_page(msg)
                failed = ONBOARD_FAIL_RE.search(clean)
                inplace_failed = None if failed else ONBOARD_INPLACE_FAIL_RE.search(clean)
                if failed or inplace_failed:
                    label = head
                    if inplace_failed:
                        label += "（订阅之后，就地完成失败）"
                    onboard_fail_heads[label] += 1
                    reason = ((failed or inplace_failed).group(1) or "").split("；本机代理中转：", 1)[0].strip() or "-"
                    onboard_fail_reasons[DIGITS.sub("#", reason)[:120]] += 1
                    m = STRUCT_RE.search(msg)
                    sample = ""
                    if m:
                        try:
                            _, end = json.JSONDecoder().raw_decode(m.group(1))
                            sample = m.group(1)[:end]
                        except ValueError:
                            sample = m.group(1)
                        if sample not in onboard_structures and len(onboard_structures) < 3:
                            onboard_structures.append(sample[:1000])
                    step, parts = onboard_step(sample)
                    onboard_fail_steps[step] += 1
                    for part in parts:
                        onboard_fail_parts[part] += 1
                    other_parts = onboard_other_parts(sample, parts)
                    pm = ONBOARD_PROGRESS_RE.search(msg)
                    done_items = progress_items(pm.group(1)) if pm else []
                    if not pm:
                        onboard_fail_done["没有“已做”（程序版本早于进度记录，或次数用完、没有开始完成）"] += 1
                    elif not done_items:
                        onboard_fail_done["这次什么都没做（-）"] += 1
                    else:
                        onboard_fail_done["这次做过"] += 1
                        for mark, text in ((ONBOARD_CHECK_MARK, "其中勾选过"), (ONBOARD_CLICK_MARK, "点击过继续"), (ONBOARD_SKIP_MARK, "跳过过")):
                            if any(item.startswith(mark) for item in done_items):
                                onboard_fail_done[text] += 1
                    if pm and len(onboard_fail_samples) < 5:
                        line = DIGITS.sub("#", reason)[:80] + " | 已做=" + pm.group(1).strip()[:300] + " | 停在 " + step
                        if other_parts:
                            line += "（另有 " + "、".join(other_parts[:4]) + "）"
                        if line not in onboard_fail_samples:
                            onboard_fail_samples.append(line)
                elif ONBOARD_INPLACE_DONE in clean and head == "WAA Worker 启动失败":
                    onboard_inplace_done += 1
                elif ONBOARD_INPLACE_LEFT in clean and head == "WAA Worker 启动失败":
                    onboard_inplace_left += 1
            if head == ORG_ENTER:
                org_enter_sources[fields(msg).get("来源", "?")] += 1
                org_enter_accounts.add(source)
            elif head == "模型目录同步失败" and "app is disabled" in msg.lower():
                # 旧版程序：组织停用的账户每分钟重试一次目录同步
                org_catalog_failed += 1
            if head.startswith(("上游代理", "本机代理", "IP 池", "代理")):
                status = fields(msg).get("状态", "")
                proxy_heads[head + (f"（状态 {status}）" if status else "")] += 1
            if level in ("WARN", "WARNING", "ERROR"):
                kinds[(level, head)] += 1
                samples.setdefault((level, head), strip_page(msg))
print("Worker 相关日志：")
for head, count in heads.most_common(20):
    print(f"  {head}：{count}")
if ready_secs:
    ready_secs.sort()
    print(f"就绪耗时：{len(ready_secs)} 个，中位 {statistics.median(ready_secs):.0f}s，P90 {ready_secs[int(len(ready_secs) * 0.9) - 1 if len(ready_secs) > 1 else 0]:.0f}s，最长 {ready_secs[-1]:.0f}s")
else:
    print("就绪耗时：这段时间没有 Worker 就绪")
if fail_reasons:
    print("启动失败原因（数字已替换为 #）：")
    for reason, count in fail_reasons.most_common(8):
        print(f"  {count} 次：{reason}")
if fail_steps:
    print("启动失败时停在哪一步（该账号最后一条启动步骤日志）：")
    for step, count in fail_steps.most_common(8):
        print(f"  {count} 次：{step}")
if fail_full:
    print("各类启动失败的完整原文（每类一条）：")
    for key, text in list(fail_full.items())[:8]:
        print("  - " + text.replace("\n", " / "))
if org_fail_new or org_fail_old or org_enter_sources or org_catalog_failed:
    sources = "、".join(f"{label} {n}" for label, n in org_enter_sources.most_common()) or "-"
    print(f"组织停用 AI Studio：启动失败 {org_fail_new + org_fail_old} 次（新版立即失败 {org_fail_new}、旧版干等到超时 {org_fail_old}），"
          f"涉及 {len(org_fail_accounts)} 个账号；新进入组织停用状态的 {len(org_enter_accounts)} 个（来源：{sources}）；"
          f"模型目录同步因 app is disabled 失败 {org_catalog_failed} 次（新版不再按失败重试，只每 24 小时复查）")
    print("  这些账号所属组织关闭了 AI Studio，重新登录也没用：管理页“组织已停用 AI Studio 的账户”可复制邮箱、一键删除或恢复调度")
if denied is not None:
    print(f"冷却列表里有 403 无权限冷却的账号 {len(denied)} 个；启动失败的账号 {len(fail_accounts)} 个，其中有 403 冷却的 {len(fail_accounts & denied)} 个；"
          f"就绪的账号 {len(ready_accounts)} 个，其中有 403 冷却的 {len(ready_accounts & denied)} 个")
if fail_ports:
    print("启动失败最多的端口：" + "，".join(f"{port}×{count}" for port, count in fail_ports.most_common(8)))
if proxy_heads:
    print("代理与 IP 池日志：")
    for head, count in proxy_heads.most_common(12):
        print(f"  {head}：{count}")
if fail_lines:
    print("最近 5 条启动失败原文（页面快照见最后一段）：")
    for line in fail_lines[-5:]:
        print("  " + line)
print()
print(f"最近 {minutes} 分钟的警告与错误类型（前 15）：")
for (level, head), count in kinds.most_common(15):
    print(f"  [{level}] {head}：{count} 次 | 示例：{samples[(level, head)].replace(chr(10), ' / ')[:220]}")
print()
print(f"==================== 失败时的页面状态（最近 {minutes} 分钟）====================")
with_snapshot, failed_snapshot = sum(page_groups.values()), sum(snapshot_failures.values())
print(f"启动失败 {page_total['WAA Worker 启动失败']} 条、Worker 重建 {page_total['WAA Worker 重建']} 条、"
      f"准备 proof 失败后换号（账号切换）{page_total['账号切换']} 条；"
      f"带页面快照的 {with_snapshot} 条（启动失败 {page_heads['WAA Worker 启动失败']}、重建 {page_heads['WAA Worker 重建']}、"
      f"账号切换 {page_heads['账号切换']}），"
      f"快照本身失败 {failed_snapshot} 条，没有“页面=”的 {sum(page_total.values()) - with_snapshot - failed_snapshot} 条")
print("  没有“页面=”的：程序版本早于页面快照，不是页面类失败（代理、浏览器启动、BiDi 连接、登录态失效等），或请求已取消、剩余时间不够采集")
if page_groups:
    print("按（路径、标题、输入框可见数、Run 按钮可见数、弹层文字前 40 字）分组，前 6 组；每组附组内分布，再按失败原因（前 3 种）"
          "各附该原因的分布与最近一条完整快照。分布里“导航后换过文档”为 since 比 age 多 5 秒以上（刷新、跳转），-为快照没有 since；"
          "“输入组件”为第一个可见文本输入框所在的 ms- 组件：")
    for index, (key, count) in enumerate(page_groups.most_common(6), 1):
        path, title, inputs, runs, overlay = key
        print(f"  [{index}] {count} 次：路径={path} | 标题={title} | 输入框可见={inputs} | Run 可见={runs} | 弹层={overlay}")
        print("      分布：" + format_dist([page_dist[(key, reason)] for reason in page_kinds[key]]))
        reasons = page_kinds[key].most_common(3)
        for reason, n in reasons:
            print(f"      {n} 次：{reason}")
            print("        分布：" + format_dist([page_dist[(key, reason)]]))
            print(f"        快照：{page_samples[(key, reason)]}")
        rest = count - sum(n for _, n in reasons)
        if rest:
            print(f"      另有 {rest} 次为其余 {len(page_kinds[key]) - len(reasons)} 种原因")
if snapshot_failures:
    print("快照本身失败的原因（数字已替换为 #）：")
    for reason, count in snapshot_failures.most_common(5):
        print(f"  {count} 次：{reason}")
print()
print(f"==================== AI Studio 首次使用页（最近 {minutes} 分钟）====================")
onboard_failed = sum(onboard_fail_heads.values())
onboard_done_total = sum(onboard_done.values())
if not (onboard_done_total or onboard_failed or onboard_inplace_done or onboard_inplace_left):
    print("这段时间没有首次使用页相关日志（没有遇到首次使用页，或程序版本早于自动完成首次使用页）")
else:
    sources = "、".join(f"{label} {n}" for label, n in onboard_done.most_common()) or "-"
    checked = " ".join(f"{value}×{n}" for value, n in onboard_checked.most_common(4)) or "-"
    buttons = " ".join(f"{value}×{n}" for value, n in onboard_buttons.most_common(4)) or "-"
    median = f"，耗时中位 {statistics.median(onboard_secs):.1f}s" if onboard_secs else ""
    print(f"自动完成 {onboard_done_total} 次（{sources}；勾选数：{checked}；按钮：{buttons}{median}）")
    print(f"其中订阅网络事件之后才到首次使用页、就地完成的 {onboard_inplace_done} 次已计入上面的自动完成次数，不要相加"
          "（这次启动照常失败，完成后的 localStorage 已写回，下次启动应能通过）")
    if onboard_inplace_left:
        print(f"订阅网络事件之后识别出首次使用页、但页面随即自行离开（什么都没点，不计入自动完成）的 {onboard_inplace_left} 次")
    print(f"按放宽规则补勾（[补]：没有必选标记、标签含服务条款或年龄关键词）的 {onboard_relaxed_runs} 次，共 {onboard_relaxed_boxes} 项")
    print(f"点过跳过（只在不涉及同意的步骤里点，如用途调查；不替账号选用途）的 {onboard_skip_runs} 次，共跳过 {onboard_skip_total} 步")
    if onboard_progress:
        print("自动完成时按顺序做过的步骤（已做，前 5 种；勾选:勾选框标签，点击:继续按钮，跳过:跳过按钮）：")
        for item, n in onboard_progress.most_common(5):
            print(f"  {n} 次：{item}")
    if onboard_labels:
        print("勾选过的标签（已打码截短，前 8 种；[补] 为放宽规则补勾的）：")
        for item, n in onboard_labels.most_common(8):
            print(f"  {n} 次：{item}")
    heads_text = "、".join(f"{label} {n}" for label, n in onboard_fail_heads.most_common()) or "-"
    print(f"未能完成 {onboard_failed} 次（{heads_text}）")
    if onboard_fail_reasons:
        print("未能完成的原因（数字已替换为 #）：")
        for reason, count in onboard_fail_reasons.most_common(6):
            print(f"  {count} 次：{reason}")
    if onboard_fail_steps:
        print("未能完成时停在哪一步（结构摘要的查找范围: 当前一步所在的 ms- 组件 cur，即当前一步的勾选框、继续与跳过按钮、标题所在的组件，"
              "如 ms-tos-disclaimer 为服务条款、ms-intent-selection 为用途调查；旧版日志没有 cur 时按 parts 写并标“（parts）”，"
              "parts 可能含同时渲染的其他各步与常驻的卡片预览，以其中是否有 ms-tos-disclaimer、ms-intent-selection 为准）：")
        for step, count in onboard_fail_steps.most_common(6):
            print(f"  {count} 次：{step}")
        if onboard_fail_parts:
            print("  按当前一步的组件（cur）：" + " ".join(f"{part}×{n}" for part, n in onboard_fail_parts.most_common(8)))
    if onboard_fail_done:
        order = ["这次做过", "其中勾选过", "点击过继续", "跳过过", "这次什么都没做（-）", "没有“已做”（程序版本早于进度记录，或次数用完、没有开始完成）"]
        print("未能完成前这次做过什么（已做；勾选过多半是服务条款那一步完成过）：" + "；".join(f"{key} {onboard_fail_done[key]}" for key in order if onboard_fail_done[key]))
    if onboard_fail_samples:
        print("未能完成时的“已做”样本（前 5 条，去重；原因 | 已做 | 停在哪一步，括号里为范围里另外可见、不在 cur 里的 ms- 组件）：")
        for line in onboard_fail_samples:
            print("  - " + line)
    if onboard_structures:
        print("未能完成时的结构摘要（前 3 条，去重；scope 为查找范围，bodyBoxes 为整页可见勾选框数，allBoxes 为整页全部勾选框数（含不可见的）；"
              "cur 为当前一步所在的 ms- 组件（停在多步向导的哪一步）；parts 为范围里可见的 ms- 组件（可能含同时渲染的其他各步）；"
              "boxes 为勾选框：req 必选、on 已勾、dis 不可勾、by 必选依据、t 含服务条款或年龄关键词、o 含邮件通知类关键词、hid 勾选框自己不可见、"
              "intent 用途调查选项（ms-intent-selection 里、标签取自按钮自己的 button[role=checkbox]；其余勾选框标签认不出也按涉及同意处理，不点跳过）、"
              "kind 类别、label 标签开头 40 字与结尾 15 字；buttons 为按钮（先列当前一步的，本身是勾选框的不列）：text 文字前 40 字、on 启用、off 不在当前一步、skip 为不点的原因"
              "（link 链接、area 勾选框标签开关与页头横幅里的、cookie Cookie 横幅、popup 菜单、card 轮播与卡片预览里的、neg 关闭返回拒绝跳过与切换账号类"
              "（跳过只在不涉及同意的步骤里另按跳过规则点，轮播与卡片预览里的跳过不点）、opt 订阅邮件类、icon 只有图标认不出的）；links 为范围内的链接；"
              "controls 为范围内其他表单控件的数量与类别；outside 为范围外的按钮）：")
        for sample in onboard_structures:
            print("  - " + sample)
PY
  rm -f -- "$cooldown_file"
}

main 2>&1 | python3 -I -c '
import os, re, sys
secrets = [s for s in os.environ.get("SECRETS", "").split() if len(s) >= 4]
for line in sys.stdin:
    for s in secrets:
        line = line.replace(s, "***")
        if ":" in s:
            line = line.replace(s.split(":", 1)[1], "***")
    line = re.sub(r"(://)[^/@\s:]+:[^/@\s]+@", r"\1***@", line)
    line = re.sub(r"\b([A-Za-z0-9._%+-]{2})[A-Za-z0-9._%+-]*@([A-Za-z0-9-]+\.)+[A-Za-z]{2,}", r"\1***@***", line)
    sys.stdout.write(line)
    sys.stdout.flush()
' | tee "$OUT"
echo
echo "已保存到 $OUT（代理密码与账号邮箱已打码），把全部内容发给我"
