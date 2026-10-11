#!/usr/bin/env bash
# slow_requests.sh 从用量账本（runtime/requests.db）里找出首字延迟最长的请求，按时间拆成三段：
#   queue_ms       进入服务到取得最终执行账号的等待（换号、排队、等 Worker 都算在内）
#   first_event_ms 进入服务到上游第一个事件（含 queue_ms）
#   duration_ms    总时长
# 并列出每次尝试（账号、通道、失败原因、耗时），用于判断几百秒的首字延迟是排队、换号还是上游慢。
#
# 用法：./tools/slow_requests.sh [小时数=24] [条数=30] [模型过滤，可空]
# 需要 sqlite3；没有安装时用 python3 读取。
set -u
APP_DIR="${APP_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DB="${LEDGER_DB:-$APP_DIR/runtime/requests.db}"
HOURS="${1:-24}"
LIMIT="${2:-30}"
MODEL="${3:-}"
if [[ ! -f "$DB" ]]; then
    echo "找不到账本：$DB（REQUEST_BODY_LOG 无关，账本默认开启；服务至少运行过一次才会生成）" >&2
    exit 1
fi
SINCE=$(( $(date +%s) - HOURS * 3600 ))
python3 - "$DB" "$SINCE" "$LIMIT" "$MODEL" <<'EOF'
import json, sqlite3, sys
db, since, limit, model = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
conn.row_factory = sqlite3.Row
where = "time >= ?"
latest = conn.execute("select max(time) from requests").fetchone()[0] or 0
args = [since * 1000 if latest > 10**12 else since]
if model:
    where += " and (model = ? or served_model = ?)"
    args += [model, model]
rows = conn.execute(f"""
    select time, protocol, model, served_model, channel, account, status, state,
           queue_ms, first_event_ms, duration_ms, reply_tokens, error, attempts, pool
    from requests where {where} order by first_event_ms desc limit ?""", args + [limit]).fetchall()
if not rows:
    print("该时间段内没有记录"); sys.exit(0)
import datetime
def ts(v):
    v = v / 1000 if v > 10**12 else v
    return datetime.datetime.fromtimestamp(v).strftime("%m-%d %H:%M:%S")
print(f"{'时间':<15} {'模型':<28} {'通道':<10} {'状态':<6} {'排队s':>7} {'首字s':>7} {'总时s':>7} {'尝试':>4}  最后账号 / 错误")
buckets = {"queue": 0, "upstream": 0, "n": 0}
for r in rows:
    attempts = []
    try:
        attempts = json.loads(r["attempts"] or "[]")
    except Exception:
        pass
    q, f, d = (r["queue_ms"] or 0) / 1000, (r["first_event_ms"] or 0) / 1000, (r["duration_ms"] or 0) / 1000
    buckets["n"] += 1
    buckets["queue"] += q
    buckets["upstream"] += max(0, f - q)
    tail = (r["account"] or "-")
    if r["error"]:
        tail += " | " + r["error"][:90]
    print(f"{ts(r['time']):<15} {(r['served_model'] or r['model'] or '')[:28]:<28} {(r['channel'] or '-')[:10]:<10} "
          f"{str(r['status']):<6} {q:7.1f} {f:7.1f} {d:7.1f} {len(attempts):>4}  {tail}")
    for a in attempts:
        err = (a.get("error") or "")[:110]
        print(f"      ↳ {a.get('account','?')[:34]:<34} {a.get('channel','?'):<10} {a.get('duration_ms',0)/1000:7.1f}s  {err}")
if buckets["n"]:
    print()
    print(f"这 {buckets['n']} 条里：平均在我们这边等账号/Worker {buckets['queue']/buckets['n']:.1f}s，"
          f"平均等上游首字 {buckets['upstream']/buckets['n']:.1f}s")
    print("排队占大头 → 调度/换号问题（账号或 Worker 不够、冷却、反复换号）；上游占大头 → 上游慢或首事件超时未开启")
EOF
