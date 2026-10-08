#!/usr/bin/env bash
# AIStudio2API 一键诊断"回复慢"：系统资源 → 服务状态 → 账号冷却 → 各阶段耗时与结论 → 程序内部等待点
# 用法：bash /root/diag_slow.sh        扩大统计窗口：MINUTES=120 bash /root/diag_slow.sh
APP_DIR="${APP_DIR:-$(cd "$(dirname "$0")/.." 2>/dev/null && [ -f start.sh ] && pwd || echo /www/wwwroot/Chat2API/build)}"
MINUTES="${MINUTES:-30}"
LOG="$APP_DIR/logs/aistudio2api.log"
PORT="$(grep -E '^LISTEN_ADDR=' "$APP_DIR/.env" 2>/dev/null | tail -n 1 | sed 's/.*://; s/["[:space:]]//g')"
API="http://127.0.0.1:${PORT:-2048}"
# 控制面 /api/ 必须携带管理令牌（程序目录下的 .admin-token）
ADMIN_TOKEN="$(tr -cd '0-9a-fA-F' < "$APP_DIR/.admin-token" 2>/dev/null)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
section() { printf '\n==================== %s ====================\n' "$1"; }
command -v python3 >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }

section "1. 系统资源"
cores="$(nproc)"
read -r load1 load5 load15 _ < /proc/loadavg
echo "  CPU：$cores 核，负载 $load1 / $load5 / $load15（1/5/15 分钟平均）"
free -m | awk 'NR==2{printf "  内存：已用 %.1f GB / 共 %.1f GB，可用 %.1f GB（%.0f%%）\n",$3/1024,$2/1024,$7/1024,$7*100/$2} NR==3{printf "  交换区：已用 %d MB / 共 %d MB\n",$3,$2}'
ps -eo rss=,args= | awk -v dir="$APP_DIR/runtime/camoufox/camoufox-bin" '
  index($2, dir) == 1 { total += $1; if ($0 !~ /-contentproc/) main++ }
  END { printf "  Camoufox 浏览器：%d 个，连同子进程共占内存 %.1f GB\n", main, total / 1048576 }'
oom="$(dmesg -T 2>/dev/null | grep -iE 'out of memory|oom-kill|killed process' | tail -n 3)"
[ -n "$oom" ] && { echo "  ⚠ 系统最近因内存不足杀过进程："; echo "$oom" | sed 's/^/    /'; }
echo "  磁盘：$(df -h "$APP_DIR" 2>/dev/null | awk 'NR==2{print "已用 "$5"，剩余 "$4}')"
# 高并发下会突然失败的硬上限：进程可打开的文件数、系统线程数
app_pid="$(cat "$APP_DIR/run/aistudio2api.pid" 2>/dev/null)"
if [ -n "$app_pid" ] && [ -d "/proc/$app_pid" ]; then
  fds="$(ls "/proc/$app_pid/fd" 2>/dev/null | wc -l)"
  fd_limit="$(awk '/Max open files/ {print $4}' "/proc/$app_pid/limits" 2>/dev/null)"
  echo "  主程序打开的文件/连接：$fds / 上限 ${fd_limit:-未知}"
  if [ -n "$fd_limit" ] && [ "$fd_limit" != "unlimited" ] && [ "$fds" -gt $((fd_limit * 7 / 10)) ]; then
    echo "  ⚠ 打开的文件数已超过上限的 70%，高并发时可能出现 too many open files"
  fi
fi
threads="$(ps -eLf --no-headers 2>/dev/null | wc -l)"
thread_limit="$(cat /proc/sys/kernel/threads-max 2>/dev/null)"
pid_limit="$(cat /proc/sys/kernel/pid_max 2>/dev/null)"
echo "  系统线程：$threads（线程上限 ${thread_limit:-未知}，进程号上限 ${pid_limit:-未知}）"
limit_min="$pid_limit"; [ -n "$thread_limit" ] && [ "${thread_limit:-0}" -lt "${limit_min:-0}" ] && limit_min="$thread_limit"
if [ -n "$limit_min" ] && [ "$threads" -gt $((limit_min * 7 / 10)) ]; then
  echo "  ⚠ 系统线程数已超过上限的 70%，继续增加浏览器可能启动失败"
fi
echo "  CPU 占用最高的进程："
ps -eo pcpu=,rss=,comm= --sort=-pcpu | head -n 5 | awk '{printf "    %5.1f%% CPU  %6.1f GB  %s\n", $1, $2/1048576, $3}'
ps -eo pcpu=,rss=,comm= | awk '$3 !~ /^(aistudio2api|camoufox-bin|Web|WebExtensions|Isolated|Privileged|RDD|Socket|Utility|forkserver)/ && ($1 > 80 || $2 > 4194304) {
  printf "  ⚠ 其他程序占用较多资源：%s（%.0f%% CPU，%.1f GB 内存），会和本服务争抢 CPU 与内存\n", $3, $1, $2 / 1048576 }'

section "2. 服务状态"
# 状态接口 1.5 秒还没返回时，趁它卡着抓一次程序快照，看它在等什么、谁占着锁
( curl -s -m 20 -o "$WORK/status.json" -w '%{time_total}' --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/status" > "$WORK/status_time" 2>/dev/null ) &
status_pid=$!
sleep 1.5
if kill -0 "$status_pid" 2>/dev/null; then
  curl -s -m 15 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/debug/goroutines" > "$WORK/g_status.txt" 2>/dev/null
fi
wait "$status_pid" 2>/dev/null
status_time="$(cat "$WORK/status_time" 2>/dev/null)"
curl -s -m 20 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/cooldowns" > "$WORK/cooldowns.json"
if [ -s "$WORK/g_status.txt" ]; then
python3 - "$WORK/g_status.txt" > "$WORK/locks.txt" <<'PY'
import re, sys, collections
blocks = [b for b in open(sys.argv[1], errors="replace").read().split("\n\n") if b.startswith("goroutine ")]
def frames(block):
    lines, result = block.splitlines(), []
    for index in range(1, len(lines)):
        line = lines[index]
        if line.startswith("\t") or line.startswith("created by"):
            continue
        name = re.sub(r"\([^()]*\)$", "", line.strip()).replace("github.com/Mag1cFall/AIStudio2API/internal/", "")
        location = lines[index + 1].strip() if index + 1 < len(lines) else ""
        location = re.sub(r"^.*/internal/", "", re.sub(r" \+0x[0-9a-f]+$", "", location))
        result.append((name, location))
    return result
def state(block):
    match = re.match(r"goroutine \d+ \[([^\]]*)\]", block)
    return match.group(1) if match else "?"
def ours(block, limit):
    picked = [f"{n} ({l})" for n, l in frames(block) if n.startswith(("app.", "aistudio.", "api.", "camoufoxnative."))]
    return " <- ".join(picked[:limit])
print()
print("  ▶ 状态接口卡住时的程序快照")
stuck = [b for b in blocks if "runtimeAdmin).Status" in b]
if not stuck:
    print("    快照时状态接口已经返回，没有抓到卡住的位置")
for block in stuck[:2]:
    print(f"    状态接口正在：[{state(block)}]")
    for name, location in frames(block)[:6]:
        print(f"      {name}  {location}")
waiters = collections.Counter()
for block in blocks:
    if re.search(r"\[(sync\.(RW)?Mutex|semacquire)", block.splitlines()[0]):
        waiters[ours(block, 2) or "?"] += 1
if waiters:
    print(f"    等锁的任务共 {sum(waiters.values())} 个，按位置：")
    for key, count in waiters.most_common(10):
        print(f"      {count:>4}  {key}")
holders = collections.Counter()
for block in blocks:
    if not re.search(r"\[(running|runnable|syscall|IO wait)", block.splitlines()[0]):
        continue
    if not re.search(r"AccountPool\)|accountWorkerManager\)|requestRegistry\)|runtimeAdmin\)", block):
        continue
    holders[f"[{state(block).split(',')[0]}] " + ours(block, 3)] += 1
if holders:
    print("    此刻正在账户池 / Worker 管理器 / 日志里运行的任务（可能正占着锁）：")
    for key, count in holders.most_common(8):
        print(f"      {count:>4}  {key}")
PY
fi
python3 - "$WORK/status.json" "$WORK/cooldowns.json" "${status_time:-0}" "$WORK/pool.json" "$WORK/locks.txt" <<'PY'
import json, sys, collections, datetime
elapsed = float(sys.argv[3] or 0)
try:
    status = json.load(open(sys.argv[1]))
except Exception:
    status = None
    if elapsed >= 19:
        print("  ⚠ 状态接口 20 秒内没有返回：服务在运行但非常繁忙（账户池锁竞争严重）")
    else:
        print("  无法读取服务状态：服务没有运行，或端口不对")
if status and elapsed >= 2:
    print(f"  ⚠ 状态接口耗时 {elapsed:.1f} 秒（正常应在 0.5 秒内），说明服务内部繁忙")
if status:
    accounts, workers = status.get("accounts") or {}, status.get("workers") or {}
    prewarm = workers.get("prewarm") or {}
    print(f"  服务：{status.get('state')}    正在处理的请求：{status.get('active_requests')}")
    print(f"  账户：共 {accounts.get('total')}，就绪 {accounts.get('ready')}，忙碌 {accounts.get('busy')}，"
          f"冷却 {accounts.get('cooldown')}，需要登录 {accounts.get('auth_required')}")
    print(f"  Worker：运行 {workers.get('warm')} / 目标 {workers.get('target')}，启动中 {workers.get('starting')}，"
          f"槽位 {workers.get('occupied')} / 峰值 {workers.get('max')}，预热{'进行中' if prewarm.get('active') else '空闲'}")
try:
    locks = open(sys.argv[5]).read().rstrip()
except Exception:
    locks = ""
if locks:
    print(locks)
print()
print("==================== 3. 账号冷却 ====================")
try:
    cooldowns = json.load(open(sys.argv[2])).get("cooldowns") or []
except Exception:
    print("  无法读取冷却信息"); sys.exit()
if not cooldowns:
    print("  当前没有账号在冷却"); sys.exit()
kinds, models, accounts_cooling = collections.Counter(), collections.Counter(), set()
for item in cooldowns:
    reason = item.get("reason") or ""
    kind = reason.split(":", 1)[0] if ":" in reason[:12] else ("403 自动暂停" if "403" in reason else "其他")
    kinds[kind] += 1
    models[item.get("model_id") or "全部模型"] += 1
    accounts_cooling.add(item.get("account_id"))
print(f"  冷却记录 {len(cooldowns)} 条，涉及账号 {len(accounts_cooling)} 个")
print("  按原因：" + "，".join(f"{k} {v}" for k, v in kinds.most_common()))
print("  按模型：" + "，".join(f"{k} {v}" for k, v in models.most_common(6)))
# 交叉核对：长时间冷却（剩余 15 分钟以上）且两个通道都冷却的账号，有多少正占着预热浏览器
import re
def parse(value):
    # Go 的 time.Time JSON：可能带 0～9 位小数秒，时区为 Z 或 ±hh:mm（每日重置时间按太平洋时间计算，常见 -07:00/-08:00）
    match = re.match(r'^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$', value or "")
    if not match:
        return None
    fraction = (match.group(2) or "0")[:6].ljust(6, "0")
    zone = "+00:00" if match.group(3) == "Z" else match.group(3)
    return datetime.datetime.fromisoformat(f"{match.group(1)}.{fraction}{zone}")
now = datetime.datetime.now(datetime.timezone.utc)
channels = collections.defaultdict(set)
for item in cooldowns:
    until = parse(item.get("until") or "")
    if until is None or (until - now).total_seconds() < 900:
        continue
    channels[(item.get("account_id"), item.get("model_id") or "*")].add(item.get("channel") or "playground")
fully = collections.defaultdict(set)
for (account, model), used in channels.items():
    if model == "*" or {"playground", "build"} <= used:
        fully[model].add(account)
warm_ids = set(((status or {}).get("workers") or {}).get("warm_ids") or [])
summary = {"warm": len(warm_ids), "models": {}, "status_seconds": elapsed}
if fully:
    print("  当天额度用完（两个通道都冷却、剩余 15 分钟以上）的账号：")
    for model, accounts_set in sorted(fully.items(), key=lambda x: -len(x[1])):
        in_warm = len(accounts_set & warm_ids)
        summary["models"][model] = {"cooled": len(accounts_set), "cooled_warm": in_warm}
        warm_text = f"，其中 {in_warm} 个正占着预热浏览器（预热池共 {len(warm_ids)} 个）" if warm_ids else "（状态接口未返回，无法核对预热池）"
        print(f"    {model}：{len(accounts_set)} 个{warm_text}")
json.dump(summary, open(sys.argv[4], "w"))
PY
# 冷却记录保存在每个账户的 runtime-state.json 里，重启后自动恢复；这里直接统计磁盘上的有效冷却，便于重启前后对比
auth_dir="$(grep -E '^AISTUDIO_AUTH_STATES=' "$APP_DIR/.env" 2>/dev/null | tail -n 1 | cut -d= -f2- | cut -d, -f1 | tr -d '"[:space:]')"
auth_dir="${auth_dir:-auth}"
case "$auth_dir" in /*) ;; *) auth_dir="$APP_DIR/$auth_dir" ;; esac
python3 - "$auth_dir" <<'PY'
import datetime, glob, json, os, re, sys
root = sys.argv[1]
now = datetime.datetime.now(datetime.timezone.utc)
def parse(value):
    match = re.match(r'^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$', value or "")
    if not match:
        return None
    fraction = (match.group(2) or "0")[:6].ljust(6, "0")
    zone = "+00:00" if match.group(3) == "Z" else match.group(3)
    return datetime.datetime.fromisoformat(f"{match.group(1)}.{fraction}{zone}")
active = accounts = 0
for path in glob.glob(os.path.join(root, "*", "runtime-state.json")):
    try:
        cooldowns = json.load(open(path)).get("cooldowns") or {}
    except Exception:
        continue
    count = sum(1 for value in cooldowns.values() if (parse((value or {}).get("until", "")) or now) > now)
    if count:
        active += count
        accounts += 1
print(f"  磁盘上保存的有效冷却：{active} 条，涉及账号 {accounts} 个（保存在各账号的 runtime-state.json，重启后自动恢复）")
try:
    models = json.load(open(os.path.join(root, ".quota-sharing.json"))).get("models") or {}
except Exception:
    models = {}
if models:
    print("  每日额度通道判定（Build 与 Playground 是否共用额度，从实际请求中学习，重启后保留）：")
    for model, stats in sorted(models.items()):
        shared, independent = stats.get("shared", 0), stats.get("independent", 0)
        verdict = ("共用：达到每日限额时同时冷却两个通道" if shared >= 8 and independent * 20 <= shared else
                   "独立：两个通道分别计算额度" if independent and independent * 20 > shared else "观察中")
        # Ultra 号池单独学习，键为 模型|ultra
        name = model[:-len("|ultra")] + "（Ultra 号池）" if model.endswith("|ultra") else model
        print(f"    {name}：另一通道也达到限额 {shared} 次，另一通道仍可用 {independent} 次 → {verdict}")
PY


section "4. 最近请求的各阶段耗时"
if [ -f "$LOG" ]; then
  cat "$LOG.1" "$LOG" 2>/dev/null | tail -n 150000 > "$WORK/recent.log"
  cat > "$WORK/analyze.py" <<'PY'
import json, re, sys, os, datetime, collections

MINUTES = int(os.environ.get("MINUTES", "30"))
TIME_RE = re.compile(r'^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$')

def parse_time(value):
    m = TIME_RE.match(value or "")
    if not m:
        return None
    frac = (m.group(2) or "0")[:6].ljust(6, "0")
    zone = "+00:00" if m.group(3) == "Z" else m.group(3)
    return datetime.datetime.fromisoformat(f"{m.group(1)}.{frac}{zone}")

def pct(values, p):
    if not values:
        return None
    values = sorted(values)
    return values[min(len(values) - 1, int(round((len(values) - 1) * p)))]

def fmt(v):
    return "  -  " if v is None else f"{v:5.1f}s"

entries = []
for line in sys.stdin:
    line = line.strip()
    if not line.startswith("{"):
        continue
    try:
        e = json.loads(line)
    except ValueError:
        continue
    t = parse_time(e.get("time"))
    if t:
        e["_t"] = t
        entries.append(e)
if not entries:
    print("  日志里没有可解析的记录（服务刚启动，或日志路径不对）")
    sys.exit(0)
latest = max(e["_t"] for e in entries)
since = latest - datetime.timedelta(minutes=MINUTES)
entries = [e for e in entries if e["_t"] >= since]

started, first_wait, attempts, finished = {}, {}, collections.Counter(), {}
last_wait, proof_done, header_seen, first_seen = {}, {}, {}, {}
switch_reasons, rebuilds, pauses = collections.Counter(), 0, 0
restarts = []
stop_signals = []
lifecycle = collections.Counter()
for e in entries:
    event, msg, req = e.get("event", ""), e.get("msg", ""), e.get("request") or {}
    rid = req.get("id")
    if event == "request.started" and rid:
        started.setdefault(rid, e["_t"])
    elif event == "request.progress" and rid and msg.startswith("等待上游响应"):
        first_wait.setdefault(rid, e["_t"])
        last_wait[rid] = e["_t"]
        attempts[rid] += 1
    elif event == "request.progress" and rid and msg.startswith("WAA proof 完成"):
        proof_done[rid] = e["_t"]
    elif event == "request.progress" and rid and msg.startswith("上游已返回响应头"):
        header_seen[rid] = e["_t"]
    elif event == "request.progress" and rid and msg.startswith("收到上游首个事件"):
        first_seen[rid] = e["_t"]
    elif event == "request.finished" and rid:
        finished[rid] = e
    elif msg.startswith("账号切换"):
        reason = msg.split("原因:", 1)[-1].strip() if "原因:" in msg else msg
        key = ("429 限流/额度" if "429" in reason else "403 无权限" if "403" in reason else
               "超时" if ("deadline" in reason or "超时" in reason) else "浏览器连接断开" if ("BiDi" in reason or "closed network" in reason) else
               "Worker 初始化失败" if "初始化" in reason else "其他")
        switch_reasons[key] += 1
    elif msg.startswith("WAA Worker 重建"):
        rebuilds += 1
    elif msg.startswith("管理服务就绪"):
        restarts.append(e["_t"])
    elif msg.startswith("收到停止信号"):
        stop_signals.append(e["_t"])
    elif msg.startswith("WAA Worker 就绪"):
        lifecycle["新启动"] += 1
    elif msg.startswith("WAA Worker 按需扩容"):
        lifecycle["按需扩容"] += 1
    elif msg.startswith("WAA Worker 空闲回收"):
        lifecycle["空闲回收"] += 1
    elif msg.startswith("WAA Worker 冷却轮换"):
        match = re.search(r"替换=(\d+)", msg)
        lifecycle["冷却轮换"] += int(match.group(1)) if match else 1
    elif msg.startswith("账号自动暂停"):
        pauses += 1

rows = []
for rid, e in finished.items():
    req = e["request"]
    duration = (req.get("duration_ms") or 0) / 1000
    first = (req.get("first_event_ms") or 0) / 1000 or None
    queue = (first_wait[rid] - started[rid]).total_seconds() if rid in first_wait and rid in started else None
    upstream = (first - queue) if (first is not None and queue is not None) else None
    usage = req.get("usage") or {}
    proof = header = after = None
    if rid in last_wait and rid in proof_done and proof_done[rid] >= last_wait[rid]:
        proof = (proof_done[rid] - last_wait[rid]).total_seconds()
        if rid in header_seen and header_seen[rid] >= proof_done[rid]:
            header = (header_seen[rid] - proof_done[rid]).total_seconds()
            # 直接用"收到上游首个事件"时间点计算；顺序不合理的不计入（不再用推算值）
            if rid in first_seen and first_seen[rid] >= header_seen[rid]:
                after = (first_seen[rid] - header_seen[rid]).total_seconds()
    rows.append({
        "proof": proof, "header": header, "after": after,
        "time": e["_t"], "id": rid, "model": req.get("model", "?"), "state": req.get("state", "?"),
        "status": req.get("status"), "channel": req.get("channel", "-"),
        "thinking": (req.get("parameters") or {}).get("thinking", "默认"),
        "duration": duration, "first": first, "queue": queue, "upstream": upstream,
        "reasoning": usage.get("reasoning_tokens"), "reply": usage.get("reply_tokens"),
        "tps": usage.get("average_tokens_per_second"), "chars": req.get("input_text_chars", 0),
        "source": e.get("source", ""), "attempts": attempts.get(rid, 0),
        "finish": req.get("finish_reason", ""), "error": req.get("error", ""),
        "max_out": (req.get("parameters") or {}).get("max_output_tokens", "默认"),
        "seed": (req.get("parameters") or {}).get("seed", "默认") or "默认",
        "temp": (req.get("parameters") or {}).get("temperature", "默认") or "默认",
        "hash": req.get("reply_hash", ""), "served": req.get("served_model", ""),
        "downgrade": req.get("downgrade") or {},
    })

print(f"  统计窗口：最近 {MINUTES} 分钟（{since:%H:%M} ~ {latest:%H:%M}），已结束请求 {len(rows)} 个")
if not rows:
    print("  这段时间没有已结束的请求。可以设置 MINUTES=120 扩大窗口后重跑")
    sys.exit(0)
SUCCESS = ("completed", "tool_calls", "limited")
ok = [r for r in rows if r["state"] in SUCCESS]
states = collections.Counter(r["state"] for r in rows)
names = {"completed": "正常完成", "tool_calls": "工具调用", "limited": "达到输出上限", "blocked": "被上游中止",
         "failed": "失败", "cancelled": "客户端取消"}
print("  " + "，".join(f"{names.get(k, k)} {v}" for k, v in states.most_common()))
blocked = [r for r in rows if r["state"] == "blocked"]
if blocked:
    reasons = collections.Counter((r["finish"] or "未知").upper() for r in blocked)
    print("    被上游中止的原因：" + "，".join(f"{k} {v}" for k, v in reasons.most_common(5))
          + "（SAFETY/PROHIBITED_CONTENT 为安全审核拦截）")
def error_summary(text):
    text = re.sub(r"\(qos=[^)]*\)|\[original: [^\]]*\]", "", text or "")
    text = text.split("\n")[0]
    text = re.split(r"协议错误码 \d+:|错误码 \d+:", text)[-1]
    return re.sub(r"\d+", "N", text).strip(" :;，。")[:70] or "(无详情)"

failed = [r for r in rows if r["state"] == "failed"]
if failed:
    def reason(r):
        text, status = r["error"] or "", r["status"] or 0
        if "Requests ending with a model turn" in text: return "以模型消息结尾（已在新版修复）"
        if "function call turn" in text or "function response turn" in text: return "工具调用历史格式"
        if "超过模型上限" in text or "max output" in text: return "输出长度超上限（已在新版修复）"
        if "没有可用" in text or "冷却" in text or "cooling" in text.lower() or status == 429: return "429 额度/限流，所有账号都不可用"
        if "deadline" in text or "超时" in text: return "超时"
        if "BiDi" in text or "closed network" in text: return "浏览器连接断开"
        if status == 400: return "400 请求参数被拒"
        if status == 403: return "403 无权限"
        if status >= 500: return f"{status} 上游/代理错误"
        return (text.split("\n")[0][:40] or f"HTTP {status}")
    reasons = collections.Counter(reason(r) for r in failed)
    print("    失败原因：" + "，".join(f"{k} {v}" for k, v in reasons.most_common(6)))
    bad_requests = collections.Counter(error_summary(r["error"]) for r in failed if (r["status"] or 0) == 400)
    if bad_requests:
        print("    400 的具体原因：")
        for text, count in bad_requests.most_common(4):
            print(f"      {count:>4}  {text}")

def column(key, data):
    return [r[key] for r in data if r[key] is not None]
print()
print("  各阶段耗时（成功请求）        中位数    P90    最长")
has_phases = bool(column("proof", ok))
stages = [("① 排队分配账号", "queue"), ("② proof + 上游首字", "upstream")]
if has_phases:
    stages += [("   ②a 生成 WAA proof", "proof"), ("   ②b 等上游响应头", "header"), ("   ②c 响应头到首字", "after")]
stages += [("③ 首字总耗时(①+②)", "first"), ("④ 整个请求", "duration")]
for label, key in stages:
    values = column(key, ok)
    print(f"    {label:<20} {fmt(pct(values, .5))}  {fmt(pct(values, .9))}  {fmt(max(values) if values else None)}")
if not has_phases:
    print("    （升级到新版后，② 会拆成生成 proof、等上游响应头、响应头到首字三段）")
retried = sum(1 for r in rows if r["attempts"] > 1)
print(f"    需要换号/重放的请求：{retried} 个（{retried * 100 // max(len(rows), 1)}%）；WAA Worker 重建 {rebuilds} 次；403 自动暂停 {pauses} 次")
if switch_reasons:
    print("    换号原因：" + "，".join(f"{k} {v} 次" for k, v in switch_reasons.most_common()))
if lifecycle:
    print("    Worker 变动：" + "，".join(f"{k} {v} 个" for k, v in lifecycle.items()))

slow_by_model = collections.Counter(r["model"] for r in ok if (r["queue"] or 0) > 3)
all_by_model = collections.Counter(r["model"] for r in ok if r["queue"] is not None)
if slow_by_model:
    print("    排队超过 3 秒的请求按模型：" + "，".join(
        f"{m} {n} 个（占该模型请求 {n * 100 // max(all_by_model[m], 1)}%）" for m, n in slow_by_model.most_common(4)))
print()
print("  按思考强度（成功请求）      请求数  首字中位数  思考token中位数  输出速度中位数")
groups = collections.defaultdict(list)
for r in ok:
    groups[r["thinking"]].append(r)
for level, data in sorted(groups.items(), key=lambda x: -len(x[1])):
    tps = pct(column("tps", data), .5)
    print(f"    {level:<18} {len(data):>6}  {fmt(pct(column('first', data), .5))}      {str(pct(column('reasoning', data), .5) or '-'):>8}        {('%.0f tok/s' % tps) if tps else '-'}")

print()
print("  按输入长度（成功请求）      请求数  首字中位数  上游首字中位数")
buckets = (("1 万字以内", 0, 10000), ("1～5 万字", 10000, 50000), ("5～15 万字", 50000, 150000), ("15 万字以上", 150000, 10**12))
bucket_first = {}
for label, low, high in buckets:
    data = [r for r in ok if low <= (r["chars"] or 0) < high]
    if data:
        bucket_first[label] = pct(column("first", data), .5)
        print(f"    {label:<18} {len(data):>6}  {fmt(pct(column('first', data), .5))}      {fmt(pct(column('upstream', data), .5))}")
print()
print("  按通道（成功请求）          请求数  首字中位数  整体中位数")
channels = collections.defaultdict(list)
for r in ok:
    channels[r["channel"]].append(r)
for channel, data in sorted(channels.items(), key=lambda x: -len(x[1])):
    print(f"    {channel:<18} {len(data):>6}  {fmt(pct(column('first', data), .5))}    {fmt(pct(column('duration', data), .5))}")

print()
print("  按模型（思考+正文 ≥1000 token）  请求数  生成速度中位/P90（从首个事件起）  整体速度中位（含首字前）  思考token中位")
by_model = collections.defaultdict(list)
for r in ok:
    tokens = (r["reasoning"] or 0) + (r["reply"] or 0)
    if tokens >= 1000 and r["duration"] and r["first"] is not None and r["duration"] > r["first"] + 1:
        by_model[r["model"]].append((tokens / (r["duration"] - r["first"]), tokens / r["duration"], r["reasoning"] or 0))
for model, data in sorted(by_model.items(), key=lambda x: -len(x[1]))[:6]:
    speeds = sorted(d[0] for d in data)
    overall = sorted(d[1] for d in data)
    think = sorted(d[2] for d in data)
    print(f"    {model[:30]:<30} {len(data):>6}  {speeds[len(speeds) // 2]:6.0f} / {speeds[int(len(speeds) * 0.9)]:4.0f} tok/s"
          f"              {overall[len(overall) // 2]:6.0f} tok/s          {think[len(think) // 2]:7.0f}")

# 降级判定（拒绝被降级的回复）：被拦截模型的每个请求都在请求日志的 downgrade 字段写明判定依据
guarded = [r for r in rows if r.get("downgrade")]
if guarded:
    def guard_time(r):
        t = r["time"]
        return t.strftime("%m-%d %H:%M:%S") if hasattr(t, "strftime") else str(t)[:19]
    verdicts = collections.Counter(r["downgrade"].get("verdict", "?") for r in guarded)
    rejected = [r for r in guarded if r["downgrade"].get("verdict") == "rejected"]
    print(f"  降级判定：被拦截模型的请求 {len(guarded)} 个，因降级拒绝 {verdicts.get('rejected', 0)} 个"
          f"（{verdicts.get('rejected', 0) * 100 // len(guarded)}%），判定正常 {verdicts.get('passed', 0)} 个，"
          f"数据不足放行 {verdicts.get('unjudged', 0)} 个")
    names = {"speed": "出字速度", "model": "上游标明的模型", "memory": "历史记录（发送前拒绝）",
             "final_speed": "结束时复核速度", "final_model": "结束时复核模型"}
    reasons = collections.Counter(r["downgrade"].get("reason", "?") for r in rejected)
    if reasons:
        print("    拒绝依据：" + "、".join(f"{names.get(k, k)} {v} 个" for k, v in reasons.most_common()))
    released = [r["downgrade"] for r in guarded
                if r["downgrade"].get("verdict") != "rejected" and r["downgrade"].get("mode") in ("strict", "fast")]
    if released:
        held = sum(d["held_ms"] for d in released) / len(released) / 1000
        text_held = sum(d.get("text_held_ms") or 0 for d in released) / len(released) / 1000
        print(f"    放行的流式请求 {len(released)} 个：判定期间平均缓存 {held:.1f} 秒（首个内容被延后的时间，严格模式含思考），"
              f"正文首字平均延后 {text_held:.1f} 秒")
    capped = sum(1 for r in guarded if r["downgrade"].get("capped"))
    if capped:
        print(f"    到最长延后时限先发出思考 {capped} 个（这些请求若被判定为降级，只能在流中报错，不能返回 400）")
    counted = [r["downgrade"] for r in guarded if r["downgrade"].get("count_tokens")]
    if counted:
        cost = sum(d.get("count_tokens_ms") or 0 for d in counted) / len(counted)
        failed = sum(1 for d in counted if d.get("count_tokens_error"))
        print(f"    调用 CountTokens {len(counted)} 次（Build 通道估算落在模糊区间），平均 {cost:.0f} ms，失败 {failed} 次")
    speeds = sorted(r["downgrade"]["speed"] for r in guarded
                    if r["downgrade"].get("verdict") == "passed" and r["downgrade"].get("speed"))
    if speeds:
        print(f"    判定正常的请求正文速度：中位 {speeds[len(speeds) // 2]:.0f}、最高 {speeds[-1]:.0f} tok/s"
              f"（最高值接近阈值说明阈值可能偏低）")
    for r in rejected[-5:]:
        print(f"    {guard_time(r)}  {r['downgrade'].get('basis', '')[:150]}")

# 上游降级：Build 通道响应的最后一块报告实际服务的模型（前面的内容块沿用请求的模型名）
served_rows = [r for r in ok if r.get("served")]
build_ok = [r for r in ok if r["channel"] == "build"]
print()
print(f"  上游降级（Build 通道上游标明的实际模型与请求不同）：{len(served_rows)} 个，占 Build 成功请求 "
      f"{len(served_rows) * 100 // max(len(build_ok), 1)}%（应为 0）")
for (asked, served), count in collections.Counter((r["model"], r["served"]) for r in served_rows).most_common(5):
    print(f"    {asked} → {served}：{count} 个")
pro_play = [r for r in ok if "pro" in str(r["model"]) and r["channel"] == "playground" and r["duration"] and r["first"] is not None
            and (r["reasoning"] or 0) + (r["reply"] or 0) >= 1000 and r["duration"] > r["first"] + 1]
if pro_play:
    fast_play = [r for r in pro_play if ((r["reasoning"] or 0) + (r["reply"] or 0)) / (r["duration"] - r["first"]) >= 200]
    print(f"  Playground 通道 Pro 请求出字 ≥200 tok/s（该通道无法核对实际模型，疑似降级或重放）：{len(fast_play)} 个，"
          f"占 {len(fast_play) * 100 // len(pro_play)}%")

print()
print("  首字最慢的 5 个请求")
for r in sorted(ok, key=lambda r: -(r["first"] or 0))[:5]:
    print(f"    {r['time']:%H:%M:%S}  {r['model'][:28]:<28} 思考={r['thinking']:<8} 排队={fmt(r['queue'])} 首字={fmt(r['first'])} "
          f"总={fmt(r['duration'])} 思考token={r['reasoning'] or '-'} 输入={r['chars']}字 尝试={r['attempts']}次")

# 截断分析：已经开始输出、却没有正常写完的回复，逐类给出原因与责任方
POLICY = {"PROHIBITED_CONTENT", "SAFETY", "RECITATION", "BLOCKLIST", "SPII", "IMAGE_SAFETY"}
had_output = [r for r in rows if r["first"]]
policy_cut = [r for r in had_output if r["state"] == "blocked" and (r["finish"] or "").upper() in POLICY]
upstream_cut = [r for r in had_output if r["state"] == "blocked" and (r["finish"] or "").upper() not in POLICY]
limit_cut = [r for r in rows if r["state"] == "limited"]
def near_restart(moment):
    # 新版以"收到停止信号"为准；旧版以重启后的"管理服务就绪"往前推 2 分钟估计
    if any(stop <= moment <= stop + datetime.timedelta(seconds=120) for stop in stop_signals):
        return True
    return any(ready - datetime.timedelta(seconds=120) <= moment <= ready for ready in restarts)
restart_cut = [r for r in had_output if r["state"] in ("cancelled", "failed") and near_restart(r["time"])]
restart_ids = {r["id"] for r in restart_cut}
client_cut = [r for r in had_output if r["state"] == "cancelled" and r["id"] not in restart_ids]
proxy_cut = [r for r in had_output if r["state"] == "failed" and r["id"] not in restart_ids]
truncated = len(policy_cut) + len(upstream_cut) + len(limit_cut) + len(client_cut) + len(proxy_cut) + len(restart_cut)
print()
print(f"  ▶ 截断分析：已开始输出、但没有正常写完的回复 {truncated} 个（占全部请求 {truncated * 100 // max(len(rows), 1)}%）")
if truncated:
    def share(items):
        return f"{len(items):>4} 个（{len(items) * 100 // truncated:>3}%）"
    print(f"    Google 内容审核中止   {share(policy_cut)}  官方：硬性内容政策，无法关闭；可调的四类安全等级本服务已全部设为最低")
    if upstream_cut:
        abort_reasons = collections.Counter((r["finish"] or "未知").upper() for r in upstream_cut)
        print(f"    Google 端异常结束     {share(upstream_cut)}  官方：" + "，".join(f"{k} {v}" for k, v in abort_reasons.most_common(3)))
    if limit_cut:
        settings = collections.Counter(str(r["max_out"]) for r in limit_cut).most_common(3)
        thinking = pct([r["reasoning"] for r in limit_cut if r["reasoning"]], .5)
        replies = pct([r["reply"] for r in limit_cut if r["reply"]], .5)
        limit_detail = "客户端设置的最大输出长度不够：常见设置 " + "、".join(f"{k}（{v} 次）" for k, v in settings)
        if thinking:
            limit_detail += f"；思考用掉中位 {thinking} tokens、正文只写了 {replies or '-'} tokens（Gemini 的思考也算在输出上限内）"
        print(f"    达到输出上限         {share(limit_cut)}  客户端：{limit_detail}")
    if client_cut:
        durations = sorted(r["duration"] for r in client_cut)
        print(f"    客户端中途断开       {share(client_cut)}  客户端：用户点了停止，或客户端/中转站超时"
              f"（断开时已进行 中位 {pct(durations, .5):.0f}s / 最长 {durations[-1]:.0f}s；若集中在同一时长附近，多半是中转站超时）")
    if proxy_cut:
        errors = collections.Counter(error_summary(r["error"]) for r in proxy_cut)
        print(f"    传输中途出错         {share(proxy_cut)}  本服务或网络：" + "；".join(f"{k}（{v}）" for k, v in errors.most_common(3)))
    if restart_cut or restarts:
        print(f"    服务重启中断         {share(restart_cut)}  本服务：窗口内重启 {len(restarts)} 次（" +
              "、".join(f"{moment:%H:%M:%S}" for moment in restarts[:5]) +
              "）；旧版本重启会立即中断所有进行中的回复，新版会先等进行中的请求完成（最多 60 秒）")
    official = len(policy_cut) + len(upstream_cut)
    client = len(limit_cut) + len(client_cut)
    ours = len(proxy_cut) + len(restart_cut)
    print(f"    合计：官方 {official * 100 // truncated}%，客户端 {client * 100 // truncated}%，本服务/网络 {ours * 100 // truncated}%")

# 重复回复分析：按回复正文指纹找出内容完全相同的回复，并列出当时的 seed 与温度
print()
def seed_kind(value):
    value = str(value)
    if value in ("", "默认"):
        return "未传"
    if value.startswith("随机"):
        return "随机"
    if "（已忽略）" in value:
        return "已忽略"
    return "固定"
seeds = collections.Counter(str(r["seed"]) for r in rows)
kinds = collections.Counter(seed_kind(r["seed"]) for r in rows)
fixed = {k: v for k, v in seeds.items() if seed_kind(k) == "固定"}
ignored = kinds.get("已忽略", 0)
temps = collections.Counter(str(r["temp"]) for r in rows)
print("  ▶ 客户端参数与重复回复")
print(f"    seed：客户端未传 {kinds.get('未传', 0) + kinds.get('随机', 0)} 次（其中本服务改用随机 seed {kinds.get('随机', 0)} 次），固定值 {sum(fixed.values())} 次"
      + (f"（最常见 " + "、".join(f"{k}×{v}" for k, v in collections.Counter(fixed).most_common(3)) + "）" if fixed else "")
      + (f"，已按配置忽略 {ignored} 次" if ignored else ""))
print("    温度：" + "，".join(f"{k}×{v}" for k, v in temps.most_common(5)))
hashed = [r for r in rows if r["hash"] and r["state"] in SUCCESS]
groups = collections.defaultdict(list)
for r in hashed:
    groups[r["hash"]].append(r)
duplicates = sorted((g for g in groups.values() if len(g) >= 2), key=len, reverse=True)
duplicate_verdict = None
if not hashed:
    print("    （升级到新版后，日志会记录回复指纹，这里会列出内容完全相同的回复）")
elif not duplicates:
    print(f"    最近 {MINUTES} 分钟 {len(hashed)} 个回复中，没有内容完全相同的回复")
    duplicate_verdict = ("本服务没有出现内容完全相同的回复。如果用户仍然看到相同回复，说明那些请求没有真正到达本服务："
                         "多半是中转站或客户端返回了缓存/重放的结果，请检查中转站的缓存设置")
else:
    repeated = sum(len(g) for g in duplicates)
    print(f"    内容完全相同的回复：{len(duplicates)} 组，共 {repeated} 个请求（占有指纹的回复 {repeated * 100 // len(hashed)}%）")
    # 正文长的排在前面：几个字的问候语每次一样是正常的，长回复一字不差才是问题
    for group in sorted(duplicates, key=lambda g: (-(g[0]["reply"] or 0), -len(g)))[:6]:
        group.sort(key=lambda r: r["time"])
        group_seeds = collections.Counter(str(r["seed"]) for r in group)
        group_temps = collections.Counter(str(r["temp"]) for r in group)
        accounts = len({r["source"] for r in group})
        group_channels = collections.Counter(str(r["channel"]) for r in group)
        lengths = sorted({r["chars"] for r in group if r["chars"] is not None})
        length_text = (f"{lengths[0]} 字" if len(lengths) == 1 else
                       f"{lengths[0]}～{lengths[-1]} 字（提示词不同）" if lengths else "- 字")
        seed_text = "/".join(list(group_seeds)[:3]) + (f" 等 {len(group_seeds)} 种" if len(group_seeds) > 3 else "")
        print(f"      {len(group)} 次 {group[0]['model'][:26]}  {group[0]['time']:%H:%M:%S}~{group[-1]['time']:%H:%M:%S}  "
              f"正文 {group[0]['reply'] or '-'} token  输入 {length_text}  seed={seed_text}  温度={'/'.join(group_temps)}  "
              f"通道={'/'.join(f'{k}×{v}' for k, v in group_channels.items())}  账号 {accounts} 个")
    dup_rows = [r for g in duplicates for r in g]
    # 疑似上游重放：Pro 模型正常出字约 110 tok/s；上游直接重放以前生成过的结果时明显更快（排查中重复回复中位 240 tok/s）。
    # 在本窗口找不到相同回复的，多半是重放了窗口之前、甚至别的用户用同一提示词生成过的结果
    def decode_speed(r):
        return ((r["reasoning"] or 0) + (r["reply"] or 0)) / (r["duration"] - r["first"])
    pro_rows = [r for r in hashed if "pro" in str(r["model"]) and r["duration"] and r["first"] is not None
                and (r["reasoning"] or 0) + (r["reply"] or 0) >= 1000 and r["duration"] > r["first"] + 1]
    if pro_rows:
        speeds = sorted(decode_speed(r) for r in pro_rows)
        fast = [r for r in pro_rows if decode_speed(r) >= 200]
        counts = collections.Counter(r["hash"] for r in hashed)
        fast_dup = sum(1 for r in fast if counts[r["hash"]] > 1)
        print(f"    疑似降级或重放（Pro 模型、思考+正文 ≥1000 token、出字速度 ≥200 tok/s；正常生成中位 {speeds[len(speeds) // 2]:.0f} tok/s）："
              f"{len(fast)} 个（占 {len(fast) * 100 // len(pro_rows)}%），其中本窗口内能找到相同回复的 {fast_dup} 个")
    fixed_share = sum(1 for r in dup_rows if seed_kind(r["seed"]) == "固定") * 100 // len(dup_rows)
    random_share = sum(1 for r in dup_rows if seed_kind(r["seed"]) == "随机") * 100 // len(dup_rows)
    unset_share = sum(1 for r in dup_rows if seed_kind(r["seed"]) in ("未传", "已忽略")) * 100 // len(dup_rows)
    dup_channels = collections.Counter(str(r["channel"]) for r in dup_rows)
    all_channels = collections.Counter(str(r["channel"]) for r in hashed)
    zero_share = sum(1 for r in dup_rows if str(r["temp"]) in ("0", "0.0")) * 100 // len(dup_rows)
    if fixed_share >= 50:
        duplicate_verdict = (f"回复重复是客户端固定 seed 导致：重复的回复中 {fixed_share}% 带了固定 seed。"
                             "在「服务配置」里开启「忽略客户端传入的 seed」，或让客户端/中转站不要传 seed")
    elif zero_share >= 50:
        duplicate_verdict = f"回复重复是温度为 0 导致：重复的回复中 {zero_share}% 的温度为 0，请让客户端把温度调高"
    elif unset_share >= 50:
        top_channel, top_count = dup_channels.most_common(1)[0]
        channel_note = ""
        if top_count * 100 // len(dup_rows) >= 80 and all_channels[top_channel] * 100 // len(hashed) < 70:
            channel_note = f"；重复几乎都出现在 {top_channel} 通道，说明该通道在未带 seed 时会返回相同结果"
        duplicate_verdict = (f"回复重复发生在客户端未传 seed、且当时还没有使用随机 seed 的请求上{channel_note}。"
                             "新版在未传 seed 时会自动使用随机 seed，更新后应消失")
    elif random_share >= 50:
        duplicate_verdict = ("每次都用了不同的随机 seed 仍然回复相同：上游对这些请求返回了相同内容，"
                             "请把 ./start.sh report 生成的排查包发给开发者")
    else:
        duplicate_verdict = ("回复重复：这些请求都真实发给了 Google（本服务日志中是多次独立请求），但没有固定 seed、温度也不为 0。"
                             "请把 ./start.sh report 生成的排查包发给开发者")

# 结论
print()
print("  ▶ 结论")
verdicts = []
q50, q90 = pct(column("queue", ok), .5), pct(column("queue", ok), .9)
u50 = pct(column("upstream", ok), .5)
f50 = pct(column("first", ok), .5)
if q50 is not None and (q50 > 3 or (q90 or 0) > 10):
    verdicts.append(f"排队分配账号慢（中位 {q50:.1f}s，P90 {q90:.1f}s）：请求在分到账号前等了很久，看下方服务状态里冷却/忙碌的账号数")
think_first = {level: pct(column("first", data), .5) for level, data in groups.items()}
thinking_matters = (think_first.get("high") and think_first.get("minimal")
                    and think_first["high"] >= think_first["minimal"] * 1.3)
if u50 is not None and u50 > 15 and not has_phases:
    advice = "思考强度越高首字越慢，想要快可用 -128 后缀" if thinking_matters else "下次诊断（新版）会拆开看具体慢在哪一段"
    verdicts.append(f"分到账号后等首字慢（中位 {u50:.1f}s）：这段是生成 proof + Google 返回首字。{advice}")
if len(rows) and retried * 100 // len(rows) >= 20:
    top = switch_reasons.most_common(1)[0][0] if switch_reasons else "未知"
    verdicts.append(f"频繁换号（{retried * 100 // len(rows)}% 的请求需要重试，主要原因：{top}），每次重试都会增加等待")
slow_output = [r for r in ok if r["first"] and r["duration"] - r["first"] > 60 and (r["tps"] or 99) < 15]
if len(slow_output) * 5 >= max(len(ok), 1):
    verdicts.append("输出阶段慢：首字之后输出速度低于 15 tok/s，可能是 Google 端输出慢或代理出口带宽不足")
small, large = bucket_first.get("1 万字以内"), max((v for k, v in bucket_first.items() if k != "1 万字以内" and v), default=None)
if small and large and large > small * 2 and large > 15:
    verdicts.append(f"长上下文是首字慢的主因：1 万字以内首字中位 {small:.1f}s，长输入达到 {large:.1f}s。"
                    "Google 要先读完整段上下文才开始输出，这部分时间代理无法缩短；精简聊天记录、世界书或开启总结可以明显变快")
think = {level: pct(column("first", data), .5) for level, data in groups.items()}
high_first, low_first = think.get("high"), think.get("minimal")
if high_first and low_first and abs(high_first - low_first) < max(high_first, low_first) * 0.25:
    verdicts.append(f"思考强度不是主因：high 与 minimal 的首字中位数接近（{high_first:.1f}s / {low_first:.1f}s）")
if len(rows) and len(failed) * 100 // len(rows) >= 5:
    top = reasons.most_common(1)[0][0] if failed else ""
    verdicts.append(f"失败率偏高（{len(failed) * 100 // len(rows)}%），最多的原因：{top}")
if len(rows) and len(blocked) * 100 // len(rows) >= 5:
    verdicts.append(f"{len(blocked) * 100 // len(rows)}% 的回复被上游中止（多为安全审核），客户端上看是回复突然截断或为空")
slow_queue = [r for r in ok if (r["queue"] or 0) > 3]
pool = {}
try:
    pool = json.load(open(os.environ.get("POOL_SUMMARY", "")))
except Exception:
    pass
confirmed = False
for model, info in sorted((pool.get("models") or {}).items(), key=lambda x: -x[1].get("cooled_warm", 0)):
    if not pool.get("warm") or info.get("cooled_warm", 0) * 5 < pool["warm"] or all_by_model[model] < 10:
        continue
    share_model = slow_by_model[model] * 100 // max(all_by_model[model], 1)
    others_slow = sum(n for m, n in slow_by_model.items() if m != model)
    others_all = sum(n for m, n in all_by_model.items() if m != model)
    share_others = others_slow * 100 // max(others_all, 1)
    if others_all >= 10 and share_model >= 2 * max(share_others, 5):
        confirmed = True
        verdicts.append(
            f"已确认排队慢是额度导致：{model} 有 {share_model}% 的请求排队超过 3 秒，其他模型只有 {share_others}%；"
            f"预热池 {pool['warm']} 个浏览器中有 {info['cooled_warm']} 个属于该模型当天额度已用完的账号，请求只能现场启动新浏览器")
    else:
        verdicts.append(
            f"额度不是排队慢的主因：预热池 {pool['warm']} 个浏览器中虽有 {info['cooled_warm']} 个属于 {model} 当天额度已用完的账号，"
            f"但它排队超过 3 秒的比例（{share_model}%）和其他模型（{share_others}%）差不多")
    break
status_seconds = pool.get("status_seconds") or 0
if not confirmed and status_seconds >= 2 and len(slow_queue) * 100 // max(len(ok), 1) >= 5:
    verdicts.append(f"所有模型都在排队，且状态接口耗时 {status_seconds:.1f} 秒：程序内部锁竞争是共同原因，看第 2 段的锁等待分析")
if ok and len(slow_queue) * 100 // len(ok) >= 5 and not confirmed and not any(v.startswith("排队") for v in verdicts):
    quota_ruled_out = any(v.startswith("额度不是") for v in verdicts)
    cause = ("常见原因：刚重启预热未满，或同时进行的请求超过了预热浏览器的数量" if quota_ruled_out else
             "常见原因：刚重启预热未满；或预热的账号当天额度已用完（看第 3 段），新版会在后台做冷却轮换，提前换成可用账号")
    verdicts.append(f"{len(slow_queue) * 100 // len(ok)}% 的请求排队超过 3 秒：分到的账号没有现成的浏览器，要现场启动。{cause}")
proof50, header50, after50 = (pct(column(k, ok), .5) for k in ("proof", "header", "after"))
if proof50 is not None and proof50 > 3:
    verdicts.append(f"生成 WAA proof 慢（中位 {proof50:.1f}s）：浏览器处理慢，常见于 CPU 被其他程序抢占或同一浏览器排队")
if header50 is not None and header50 > 5:
    verdicts.append(f"等上游响应头慢（中位 {header50:.1f}s）：请求已发出、Google 还没开始回应，通常是 Google 端排队或代理出口慢")
if after50 is not None and after50 > 10:
    verdicts.append(f"响应头到首字慢（中位 {after50:.1f}s）：Google 已经收到请求，时间花在读上下文和思考上")
if duplicate_verdict:
    verdicts.append(duplicate_verdict)
if not verdicts:
    verdicts.append(f"各阶段未发现明显异常（首字中位 {fmt(f50).strip()}）。如果仍觉得慢，把整段输出发给开发者")
for v in verdicts:
    print("    - " + v)
PY
  MINUTES="$MINUTES" POOL_SUMMARY="$WORK/pool.json" python3 "$WORK/analyze.py" < "$WORK/recent.log"
else
  echo "  找不到日志文件：$LOG"
fi

section "4b. 账户池锁与浏览器命令（实时采样 5 秒）"
curl -s -m 10 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/debug/perf" > "$WORK/perf1.json" 2>/dev/null
sleep 5
curl -s -m 10 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/debug/perf" > "$WORK/perf2.json" 2>/dev/null
python3 - "$WORK/perf1.json" "$WORK/perf2.json" <<'PY'
import json, sys
try:
    first, second = (json.load(open(path)) for path in sys.argv[1:3])
    la, lb, ba, bb = first["pool_lock"], second["pool_lock"], first["browser"], second["browser"]
except Exception:
    print("  （当前运行的版本没有 /api/debug/perf，升级后可用）")
    sys.exit(0)
seconds = max(lb["uptime_s"] - la["uptime_s"], 0.001)
locks = lb["acquisitions"] - la["acquisitions"]
wait = lb["wait_ms_total"] - la["wait_ms_total"]
hold = lb["hold_ms_total"] - la["hold_ms_total"]
busy = hold / (seconds * 1000) * 100
print(f"  账户池锁：每秒加锁 {locks / seconds:.0f} 次，平均等锁 {wait / max(locks, 1):.2f} ms，锁占用率 {busy:.0f}%")
print(f"    启动以来：最长等锁 {lb['wait_ms_max']:.0f} ms，最长持锁 {lb['hold_ms_max']:.0f} ms，"
      f"持锁超过 {lb['slow_hold_threshold_ms']:.0f} ms 共 {lb['slow_holds']} 次")
for site in (lb.get("slow_sites") or [])[:5]:
    print(f"      {site['count']:6} 次  合计 {site['total_ms']:8.0f} ms  最长 {site['max_ms']:6.0f} ms  {site['site']}")
commands = bb["bidi_commands"] - ba["bidi_commands"]
chunk_polls = bb["chunk_polls"] - ba["chunk_polls"]
header_polls = bb["header_polls"] - ba["header_polls"]
with_data = bb["chunk_polls_with_data"] - ba["chunk_polls_with_data"]
print(f"  浏览器命令：每秒 {commands / seconds:.0f} 条，此刻进行中 {bb['bidi_inflight']} 条；"
      f"读响应头轮询每秒 {header_polls / seconds:.0f} 次，读响应块轮询每秒 {chunk_polls / seconds:.0f} 次"
      f"（{with_data * 100 // max(chunk_polls, 1)}% 带回数据，页面内长轮询 {bb['long_poll_ms']} ms）")
guard = second.get("downgrade_guard") or {}
if guard.get("judged"):
    by = "、".join(f"{k} {v}" for k, v in (guard.get("rejected_by") or {}).items()) or "-"
    print(f"  降级判定（启动以来）：判定 {guard.get('judged', 0)} 次，因降级拒绝 {guard.get('rejected', 0)} 次（{by}），"
          f"判定正常 {guard.get('passed', 0)}，数据不足放行 {guard.get('unjudged', 0)}；"
          f"放行后结束时复核为降级（漏判）{guard.get('missed', 0)} 次")
    if guard.get("held_samples"):
        print(f"    放行的流式请求平均额外首字延迟 {guard.get('avg_held_ms', 0) / 1000:.1f} 秒"
              f"（正文首字 {guard.get('avg_text_held_ms', 0) / 1000:.1f} 秒，{guard['held_samples']} 个样本）")
    if guard.get("capped"):
        print(f"    到最长延后时限先发出思考 {guard['capped']} 次（这些请求若被判定为降级，只能在流中报错）")
    if guard.get("count_tokens_calls"):
        deviation = ""
        if guard.get("count_estimate_samples"):
            deviation = (f"；估算与精确值平均偏差 {guard.get('count_estimate_error_pct', 0):+.0f}%"
                         f"（绝对 {guard.get('count_estimate_abs_error_pct', 0):.0f}%）")
        print(f"    CountTokens {guard['count_tokens_calls']} 次，平均 {guard.get('count_tokens_avg_ms', 0):.0f} ms，"
              f"失败 {guard.get('count_tokens_failures', 0)} 次{deviation}")
    if guard.get("build_estimate_samples"):
        print(f"    Build 按比例估算的正文 token 与最终真实数平均偏差 {guard.get('build_estimate_error_pct', 0):+.0f}%"
              f"（绝对 {guard.get('build_estimate_abs_error_pct', 0):.0f}%，{guard['build_estimate_samples']} 个样本；明显偏低会漏判）")
    if guard.get("last_missed"):
        print(f"    最近一次漏判：{guard['last_missed'][:150]}")
    print(f"    记住的被降级对话：{guard.get('memory_entries', 0)} 条")
served = second.get("models") or {}
if served:
    print(f"  上游实际服务的模型与请求不同（只能核对 Build 通道）：启动以来 {served.get('mismatches', 0)} 次，应为 0"
          + (f"；最近一次 {served.get('last')}" if served.get("last") else ""))
if busy >= 50:
    print("  ⚠ 账户池锁占用率超过 50%：调度会在锁上排队，这是\"排队分配账号\"随并发变慢的直接原因，看上面耗时最多的持锁位置")
PY

section "5. 程序此刻在等什么"
if [ -n "$GOROUTINE_FILE" ]; then cp "$GOROUTINE_FILE" "$WORK/g.txt"; else curl -s -m 10 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/debug/goroutines" > "$WORK/g.txt"; fi
if [ -s "$WORK/g.txt" ]; then
  awk 'BEGIN { RS = "" }
  {
    queue += /acquireWarmLease/; browser += /waitProtectedHeaders|protectedResponseBody/;
    proof += /\)\.Proof\(|accountWorkerPreparer\)\.Prepare/; locked += /\[sync\.Mutex\.Lock/;
    starting += /startReservedWorker/; verifying += /VerifyAccount/
  }
  END {
    printf "  正在排队分配账号的请求：%d\n  正在生成 WAA proof 的请求：%d\n  正在通过浏览器等上游返回的请求：%d\n", queue, proof, browser
    printf "  正在启动的浏览器：%d    正在验证的新账户：%d    在等锁的任务：%d\n", starting, verifying, locked
  }' "$WORK/g.txt"
  echo "  等待点明细（数量 / 最久分钟 / 状态 / 代码位置）："
  awk 'BEGIN { RS = "" }
  {
    n = split($0, L, "\n"); state = L[1]; sub(/^goroutine [0-9]+ \[/, "", state); sub(/\]:$/, "", state)
    minutes = 0
    if (match(state, /, [0-9]+ minutes/)) { minutes = substr(state, RSTART + 2, RLENGTH - 10) + 0; sub(/, [0-9]+ minutes/, "", state) }
    sig = ""; c = 0
    for (i = 2; i <= n && c < 2; i++) if (L[i] ~ /^github\.com\/Mag1cFall\/AIStudio2API\/internal\//) {
      f = L[i]; sub(/\([^()]*\)$/, "", f); sub(/^github\.com\/Mag1cFall\/AIStudio2API\/internal\//, "", f)
      l = L[i + 1]; sub(/^[ \t]+/, "", l); sub(/ \+0x[0-9a-f]+$/, "", l); sub(/^.*\/internal\//, "", l)
      sig = sig " <- " f " (" l ")"; c++
    }
    if (sig == "") next
    key = "[" state "]" sig; count[key]++; if (minutes > longest[key]) longest[key] = minutes
  }
  END { for (k in count) printf "%5d  %3d分  %s\n", count[k], longest[k], k }' "$WORK/g.txt" | sort -rn | head -n 12 | sed 's/^/  /'
else
  echo "  无法读取程序内部状态（服务未运行或版本过旧）"
fi
echo
echo "把从「1. 系统资源」开始的全部输出复制发给开发者即可"
