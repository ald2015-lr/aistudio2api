#!/usr/bin/env bash
# AIStudio2API 只读诊断"号不够用/排队慢/浏览器频繁重启"：浏览器启动与淘汰来源、排队原因、429 与取消耗时、
# 整户暂停的原因与权益分布、付费/Ultra 模型在目录里的权益标记、常驻浏览器的权益构成
# 用法：bash diag_capacity.sh        扩大统计窗口：MINUTES=240 bash diag_capacity.sh
# 只读：不改任何文件与设置；输出不含邮箱、密码、Key、令牌与提示词
APP_DIR="${APP_DIR:-$(cd "$(dirname "$0")/.." 2>/dev/null && [ -f start.sh ] && pwd || echo /www/wwwroot/Chat2API/build)}"
MINUTES="${MINUTES:-120}"
cd "$APP_DIR" 2>/dev/null || { echo "找不到程序目录 $APP_DIR（可用 APP_DIR=/程序目录 指定）"; exit 1; }
command -v python3 >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }
PORT="$(grep -E '^LISTEN_ADDR=' .env 2>/dev/null | tail -n 1 | sed 's/.*://; s/["[:space:]]//g')"
API="http://127.0.0.1:${PORT:-2048}"
ADMIN_TOKEN="$(tr -cd '0-9a-fA-F' < .admin-token 2>/dev/null)"
WORK="$(mktemp -d)" && [ -n "$WORK" ] && [ -d "$WORK" ] || { echo "无法创建临时目录（检查 TMPDIR 与磁盘空间）"; exit 1; }
trap 'rm -rf "$WORK"' EXIT
AUTH="$(grep -E '^AISTUDIO_AUTH_STATES=' .env 2>/dev/null | tail -n 1 | cut -d= -f2- | cut -d, -f1 | tr -d '"[:space:]')"
AUTH="${AUTH:-auth}"
case "$AUTH" in /*) ;; *) AUTH="$APP_DIR/$AUTH" ;; esac

echo "==================== 0. 相关设置（.env 里没写的按默认值） ===================="
grep -E '^(ROUTING_STRATEGY|REQUEST_TIMEOUT|FIRST_EVENT_TIMEOUT|INIT_TIMEOUT|WARM_STARTUP_CONCURRENCY|WARM_WORKER_LIMIT|MAX_ACTIVE_WORKERS|ULTRA_WARM_WORKER_LIMIT|ULTRA_MAX_ACTIVE_WORKERS|PER_ACCOUNT_CONCURRENCY|ACCOUNT_RPM_LIMIT|PRIMARY_MODELS|PRIMARY_QUEUE_WAIT|ULTRA_PRIMARY_MODELS|ULTRA_PRIMARY_QUEUE_WAIT|ULTRA_INCLUDE_PRO|ULTRA_EXCLUSIVE|PRO_POOL|PRO_WARM_WORKER_LIMIT|PRO_MAX_ACTIVE_WORKERS|EXTRA_POOL|EXTRA_POOL_MODE|EXTRA_POOL_DEFAULT_PAID|EXTRA_POOL_INCLUDE_ULTRA|EXTRA_WARM_WORKER_LIMIT|EXTRA_MAX_ACTIVE_WORKERS|PROXY_POOL_MODE|PROXY_POOL_FIXED|PROXY_POOL_PORT_START|PROXY_POOL_PORT_END)=' .env 2>/dev/null | sed 's/^/  /'
git log --oneline -1 2>/dev/null | cut -c1-7 | sed 's/^/  版本：/'

for path in status accounts cooldowns accounts/permission-denied; do
  curl -s -m 120 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/$path" > "$WORK/$(echo "$path" | tr '/' '_').json" 2>/dev/null
done
cat logs/aistudio2api.log.1 logs/aistudio2api.log 2>/dev/null > "$WORK/log.jsonl"

MINUTES="$MINUTES" python3 -I - "$WORK" "$AUTH" "runtime/model-catalogs" <<'PY'
import collections, datetime, glob, json, os, re, sys
work, auth, catalogs = sys.argv[1], sys.argv[2], sys.argv[3]
MINUTES = int(os.environ.get("MINUTES") or 120)
TIME_RE = re.compile(r'^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$')
def parse_time(v):
    m = TIME_RE.match(v or "")
    if not m:
        return None
    zone = "+00:00" if m.group(3) == "Z" else m.group(3)
    return datetime.datetime.fromisoformat(f"{m.group(1)}.{(m.group(2) or '0')[:6].ljust(6, '0')}{zone}")
DUR_RE = re.compile(r'(\d+(?:\.\d+)?)(h|ms|µs|us|ns|m|s)')
def dur(text):
    # Go 的 time.Duration 文本（1m2.5s、850ms、35.1s）换成秒
    total, unit = 0.0, {"h": 3600, "m": 60, "s": 1, "ms": 1e-3, "µs": 1e-6, "us": 1e-6, "ns": 1e-9}
    for n, u in DUR_RE.findall(text or ""):
        total += float(n) * unit[u]
    return total
def pct(values, p):
    if not values:
        return None
    values = sorted(values)
    return values[min(len(values) - 1, int(round((len(values) - 1) * p)))]
def stats(values):
    if not values:
        return "  -"
    return f"中位 {pct(values, .5):6.1f}s  P90 {pct(values, .9):6.1f}s  最长 {max(values):6.1f}s"
def load(name):
    try:
        return json.load(open(os.path.join(work, name)))
    except Exception:
        return None
def section(title):
    print(f"\n==================== {title} ====================")

entries = []
for line in open(os.path.join(work, "log.jsonl"), errors="ignore"):
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
if entries:
    latest = max(e["_t"] for e in entries)
    entries = [e for e in entries if e["_t"] >= latest - datetime.timedelta(minutes=MINUTES)]

section(f"1. 浏览器启动（最近 {MINUTES} 分钟）")
starts, start_secs = collections.Counter(), collections.defaultdict(list)
worker_events, slot_waits = collections.Counter(), collections.Counter()
for e in entries:
    msg = e.get("msg") or ""
    if msg.startswith("WAA Worker 就绪"):
        m = re.search(r"来源=([^ |]+)", msg)
        source = m.group(1) if m else "未标来源"
        starts[source] += 1
        m = re.search(r"耗时=([0-9.hmµuns]+)", msg)
        if m:
            start_secs[source].append(dur(m.group(1)))
    elif "等待冷启动名额 | 类型=" in msg:
        m = re.search(r"类型=([^ |]+)", msg)
        slot_waits[m.group(1) if m else "?"] += 1
    elif msg.startswith("WAA Worker "):
        worker_events[msg.split(" | ")[0]] += 1
if not entries:
    print("  日志里没有可解析的记录（日志路径不对，或服务刚启动）")
print(f"  就绪的新浏览器共 {sum(starts.values())} 个，按来源（启动耗时）：")
for source, n in starts.most_common():
    print(f"    {n:6}  {source:<10} {stats(start_secs[source])}")
print("  浏览器的其他变动（按日志开头）：")
for kind, n in worker_events.most_common(20):
    print(f"    {n:6}  {kind}")
print("  等待冷启动名额（启动并发已满时排队）：" + ("，".join(f"{k} {v} 次" for k, v in slot_waits.most_common()) or "0 次"))

section("2. 排队在等什么（按模型，每个请求取最后一次排队原因与等待时长）")
model_of, reason_of, waited_of = {}, {}, collections.defaultdict(float)
finished = {}
for e in entries:
    req = e.get("request") or {}
    rid = req.get("id")
    if not rid:
        continue
    if req.get("model"):
        model_of.setdefault(rid, req.get("model"))
    msg = e.get("msg") or ""
    if e.get("event") == "request.progress" and msg.startswith("排队等待 | 原因="):
        m = re.match(r"排队等待 \| 原因=(.*?) \| 已等待=([^ |]+)", msg)
        if m:
            reason_of[rid] = re.sub(r"\d+", "N", m.group(1))[:40]
            waited_of[rid] = max(waited_of[rid], dur(m.group(2)))
    elif e.get("event") == "request.finished":
        finished[rid] = req
        if req.get("model"):
            model_of[rid] = req.get("model")
groups = collections.defaultdict(list)
for rid, reason in reason_of.items():
    groups[(model_of.get(rid, "?"), reason)].append(waited_of[rid])
print("    请求数  模型                         最后原因                                  等待时长")
for (model, reason), values in sorted(groups.items(), key=lambda x: -len(x[1]))[:25]:
    print(f"    {len(values):6}  {model[:28]:<28} {reason:<40} {stats(values)}")
if not groups:
    print("    没有排队记录")

section("3. 失败与客户端取消的耗时（按模型）")
def group_by_model(rows):
    g = collections.defaultdict(list)
    for r in rows:
        g[r.get("model") or "?"].append((r.get("duration_ms") or 0) / 1000)
    return g
rows = list(finished.values())
for title, picked in (("429 失败", [r for r in rows if r.get("state") == "failed" and r.get("status") == 429]),
                      ("其他失败", [r for r in rows if r.get("state") == "failed" and r.get("status") != 429]),
                      ("客户端取消", [r for r in rows if r.get("state") == "cancelled"])):
    print(f"  {title}：共 {len(picked)} 个，其中耗时 ≥ 290 秒的 {sum(1 for r in picked if (r.get('duration_ms') or 0) >= 290000)} 个")
    for model, values in sorted(group_by_model(picked).items(), key=lambda x: -len(x[1]))[:8]:
        print(f"    {len(values):6}  {model[:28]:<28} {stats(values)}")

section("4. 403 暂停与按模型的 403")
pause_hours, model_403, model_verify = collections.Counter(), collections.Counter(), collections.Counter()
for e in entries:
    msg = e.get("msg") or ""
    if msg.startswith("账号暂停调度"):
        pause_hours[(e.get("time") or "")[:13]] += 1
    elif msg.startswith("账号冷却 | 类型=无权限"):
        m = re.search(r"范围=([^ |]+)", msg)
        model_403[m.group(1) if m else "?"] += 1
    elif msg.startswith("账号冷却 | 类型=单个模型需要验证"):
        # 只在一个模型上要求验证账号：只暂停这个模型，账户其他模型照常调度、Worker 不释放
        m = re.search(r"范围=([^ |]+)", msg)
        model_verify[m.group(1) if m else "?"] += 1
print("  新的整户暂停（需要验证账号，2 个及以上模型）按小时：" + ("，".join(f"{h[5:].replace('T', ' ')}时 {n}" for h, n in sorted(pause_hours.items())) or "无"))
print("  单个模型需要验证（只暂停该模型）按模型：" + ("，".join(f"{k} {v}" for k, v in model_verify.most_common(10)) or "无"))
print("  按模型的 403 冷却：" + ("，".join(f"{k} {v}" for k, v in model_403.most_common(10)) or "无"))

accounts = (load("accounts.json") or {}).get("accounts") or []
acc = {a.get("id"): a for a in accounts}
def tier(a):
    item = acc.get(a) or {}
    return (item.get("benefit_tier") or "?") + ("" if item.get("benefit_tier_known", True) else "(未读取)")
def state(a):
    return (acc.get(a) or {}).get("state") or "?"
def kind(r):
    if r.startswith("需要验证账号（verify-account）"): return "A 需要验证账号，整户暂停 30 天"
    if r.startswith("单个模型需要验证账号"): return "C 单个模型需要验证，只暂停该模型 30 天"
    if r.endswith("次返回 403 无权限，自动暂停"): return "B 多个模型 403，整户暂停 30 分钟"
    if r.startswith("无权限（模型）"): return "D 单个模型 403"
    if r.startswith("每日限额"): return "E 单个模型每日限额"
    if r.startswith(("限流", "分钟限额")): return "F 限流/分钟限额"
    if r.startswith("疑似登录失效"): return "G 疑似登录失效"
    return "H 其他：" + re.sub(r"\d+", "N", r[:24])

section("5. 冷却账号的真实原因 × 权益 × 状态（按账号计）")
cooldowns = (load("cooldowns.json") or {}).get("cooldowns")
if not accounts or cooldowns is None:
    print("  读不到账户或冷却信息（服务没运行、端口不对，或 .admin-token 不对）")
else:
    now = datetime.datetime.now(datetime.timezone.utc)
    global_kind = {}
    for path in glob.glob(os.path.join(auth, "*", "runtime-state.json")):
        try:
            item = (json.load(open(path)).get("cooldowns") or {}).get("*")
        except Exception:
            continue
        if item and (parse_time(item.get("until")) or now) > now:
            global_kind[os.path.basename(os.path.dirname(path)).lower()] = kind(item.get("reason") or "")
    per = collections.defaultdict(set)
    for c in cooldowns:
        per[c.get("account_id")].add(kind(c.get("reason") or ""))
    def cause(a):
        g = global_kind.get((a or "").lower())
        if g:
            return "整户 " + g
        return ("只有单个模型：" + min(per[a])) if per[a] else "没有冷却"
    table = collections.Counter((cause(a), tier(a), state(a)) for a in acc)
    print(f"  账号 {len(acc)} 个，磁盘上有整户冷却的 {len(global_kind)} 个")
    print("    账号数  原因                                         权益            状态")
    for (c, t, s), n in sorted(table.items(), key=lambda x: (x[0][0], -x[1])):
        print(f"    {n:6}  {c:<44} {t:<15} {s}")
    cooling = [a for a in acc if state(a) == "cooldown"]
    print(f"  状态为“冷却”的 {len(cooling)} 个里：整户暂停 {sum(1 for a in cooling if cause(a).startswith('整户'))} 个，"
          f"只有部分模型冷却（其他模型仍可用）{sum(1 for a in cooling if not cause(a).startswith('整户'))} 个")
    verify = re.compile(r"verify-account|must be verified", re.I)
    by_model, by_tier, by_day = collections.Counter(), collections.Counter(), collections.Counter()
    for item in (load("accounts_permission-denied.json") or {}).get("accounts") or []:
        hit = [m for m in item.get("models") or [] if verify.search(m.get("message") or "")]
        if not hit:
            continue
        by_tier[tier(item.get("id"))] += 1
        for m in hit:
            by_model[m.get("model")] += 1
            by_day[(m.get("first_at") or "")[:10]] += 1
    print("  需要验证账号：触发的模型 " + (str(dict(by_model.most_common(8))) if by_model else "无"))
    print("  需要验证账号：按权益 " + (str(dict(by_tier)) if by_tier else "无"))
    print("  需要验证账号：首次 403 按日期 " + (str(dict(sorted(by_day.items())[-10:])) if by_day else "无"))

section("6. 付费/Ultra 模型在各权益账号目录里的标记（access_modes：3=Pro 与 Ultra，4=仅 Ultra，空=所有权益）")
want = ("gemini-3.1-pro-preview", "gemini-4-carbon", "gemini-4-argon")
names = {0: "Free", 1: "Pro", 2: "Ultra", 3: "Plus"}
try:
    index = json.load(open(os.path.join(catalogs, "index.json"))).get("accounts") or {}
except Exception:
    index = None
if index is None:
    print("  读不到模型目录缓存")
else:
    cache = {}
    def blob(key):
        if key not in cache:
            try:
                cache[key] = json.load(open(os.path.join(catalogs, "blobs", key + ".json")))
            except Exception:
                cache[key] = {}
        return cache[key]
    def rows_of(x):
        if isinstance(x, str):
            try:
                x = json.loads(x)
            except ValueError:
                return
        stack = [x]
        while stack:
            item = stack.pop()
            if isinstance(item, list):
                if item and isinstance(item[0], str) and item[0].startswith("models/") and item[0][7:] in want:
                    yield item
                else:
                    stack.extend(item)
    marks = collections.Counter()
    for entry in index.values():
        playground = {}
        if entry.get("p"):
            for row in rows_of(blob(entry["p"]).get("raw")):
                playground[row[0][7:]] = row[82] if len(row) > 82 else None
        build = {m.get("id") for m in (blob(entry["b"]).get("models") or [])} if entry.get("b") else set()
        for model in want:
            mark = json.dumps(playground[model]) if model in playground else "目录里没有"
            marks[(names.get(entry.get("t"), str(entry.get("t"))), model, mark, "Build 有" if model in build else "Build 无")] += 1
    print("    账号数  权益   模型                      Playground 标记      Build")
    for (t, model, mark, build), n in sorted(marks.items()):
        print(f"    {n:6}  {t:<6} {model:<25} {mark:<20} {build}")

section("7. 常驻浏览器与能用付费模型的账号（按权益）")
status = load("status.json") or {}
warm = (status.get("workers") or {}).get("warm_ids") or []
print("  常驻浏览器按权益：" + (str(dict(collections.Counter(tier(a) for a in warm))) if warm else "读不到"))
# 号池（成员关系，不是权益）：PRO_POOL=true（默认）时 Pro 账户在 Pro 号池（只服务 /pro），关闭时留在普通号池或经 ULTRA_INCLUDE_PRO 并入 Ultra 号池
pool_of = {a.get("id"): a.get("pool") or "normal" for a in accounts}
print("  常驻浏览器按号池：" + (str(dict(collections.Counter(pool_of.get(a, "?") for a in warm))) if warm else "读不到"))
for key, name in (("workers", "普通"), ("ultra_workers", "Ultra"), ("pro_workers", "Pro")):
    part = status.get(key) or {}
    if part.get("max"):
        print(f"  {name} 分区 Worker：就绪 {part.get('warm')} / 预热目标 {part.get('target')} / 峰值 {part.get('max')}")
# 补充池（EXTRA_POOL）：开启，或关闭后仍有补充池 Worker 在归还时状态接口才有 extra_pool；上面各分区的计数不含补充池 Worker
extra = status.get("extra_pool")
if extra:
    part = extra.get("workers") or {}
    mode = "一起分担" if extra.get("mode") != "overflow" else "不够时才补"
    print(f"  补充池（{'开启' if extra.get('enabled') else '已关闭，等待归还'}，{mode}）Worker：就绪 {part.get('warm')} / 启动中 {part.get('starting')}"
          f" / 预热目标 {part.get('target')} / 常驻 {part.get('warm_limit')} / 峰值 {part.get('max')}")
    print(f"    成员 {extra.get('members')}（Free {extra.get('free')} · Pro {extra.get('pro')} · Ultra {extra.get('ultra')}，冷成员 {extra.get('cold_members')}）"
          f"，可借出 {extra.get('lendable')}，归还中 {extra.get('returning')}，本进程借出 {extra.get('borrows')} 次")
    extra_ids = set((status.get("workers") or {}).get("extra_ids") or [])
    if extra_ids:
        print("    补充池 Worker 按权益：" + str(dict(collections.Counter(tier(a) for a in extra_ids))))
for model in ("gemini-3.1-pro-preview", "gemini-4-carbon"):
    usable = collections.Counter((tier(a.get("id")), a.get("state")) for a in accounts if model in (a.get("models") or []))
    print(f"  目录里有 {model} 的账号（权益, 状态）：" + (", ".join(f"{t}/{s} {n}" for (t, s), n in usable.most_common(12)) or "无"))
PY
echo
echo "把从「0. 相关设置」开始的全部输出复制发给开发者即可（不含邮箱、密码、Key）"
