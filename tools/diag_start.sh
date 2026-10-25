#!/usr/bin/env bash
# AIStudio2API 一键诊断“Worker 起不来”：系统资源 → 状态接口（Worker、预热、IP 池、出口）→ 绕过程序直连测 IP 池端口 →
# 最近的 Worker 启动次数、就绪耗时、失败原因与代理日志
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
  python3 -I - "$APP_DIR/logs/aistudio2api.log" "$LOG_MINUTES" <<'PY'
import collections, datetime, json, os, re, statistics, sys
path, minutes = sys.argv[1], int(sys.argv[2])
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
            if head.startswith("WAA Worker") or head.startswith("Ultra Worker"):
                heads[head] += 1
                f = fields(msg)
                if "就绪" in head and f.get("耗时"):
                    seconds = duration(f["耗时"])
                    if seconds is not None:
                        ready_secs.append(seconds)
                if "失败" in head:
                    fail_reasons[DIGITS.sub("#", f.get("错误", msg.split("\n")[0]))[:170]] += 1
                    if f.get("端口"):
                        fail_ports[f["端口"]] += 1
                    fail_lines.append(f"{moment.astimezone():%H:%M:%S} {msg.replace(chr(10), ' / ')[:400]}")
            if head.startswith(("上游代理", "本机代理", "IP 池", "代理")):
                status = fields(msg).get("状态", "")
                proxy_heads[head + (f"（状态 {status}）" if status else "")] += 1
            if level in ("WARN", "WARNING", "ERROR"):
                kinds[(level, head)] += 1
                samples.setdefault((level, head), msg)
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
if fail_ports:
    print("启动失败最多的端口：" + "，".join(f"{port}×{count}" for port, count in fail_ports.most_common(8)))
if proxy_heads:
    print("代理与 IP 池日志：")
    for head, count in proxy_heads.most_common(12):
        print(f"  {head}：{count}")
if fail_lines:
    print("最近 5 条启动失败原文：")
    for line in fail_lines[-5:]:
        print("  " + line)
print()
print(f"最近 {minutes} 分钟的警告与错误类型（前 15）：")
for (level, head), count in kinds.most_common(15):
    print(f"  [{level}] {head}：{count} 次 | 示例：{samples[(level, head)].replace(chr(10), ' / ')[:220]}")
PY
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
