#!/usr/bin/env bash
# AIStudio2API 一键对比“启动失败的账号”与“启动成功的账号”保存的浏览器状态：老账号起不来、新账号能跑时用来找原因。
# 对比项：storage-state.json 里 aistudio.google.com 的 localStorage（只列键名与值长度，不输出值）、Cookie 数与文件时间，
# 以及账户目录下 camoufox-cache（HTTP 磁盘缓存）的有无、大小与时间。
# 只读：只读运行日志与账户目录，不访问网络、不改任何文件。输出同时写到 $OUT（默认 logs/diag_profile.txt），账号已打码
#
# 用法（放在程序目录的 tools/ 下运行，或用 APP_DIR 指定程序目录）：
#   bash tools/diag_profile.sh
#   LOG_MINUTES=180 bash tools/diag_profile.sh    按最近 180 分钟的启动结果分组（默认 60）
set -u
script_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd)"
if [ -z "${APP_DIR:-}" ]; then
  for candidate in "$script_dir/.." "$PWD" /www/wwwroot/Chat2API/build; do
    if [ -f "$candidate/start.sh" ]; then APP_DIR="$(cd "$candidate" && pwd)"; break; fi
  done
fi
APP_DIR="${APP_DIR:-/www/wwwroot/Chat2API/build}"
: "${OUT:=$APP_DIR/logs/diag_profile.txt}" "${LOG_MINUTES:=60}"
case "$LOG_MINUTES" in "" | *[!0-9]* | 0) echo "LOG_MINUTES 必须是正整数"; exit 1 ;; esac
command -v python3 >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }
cd "$APP_DIR" || { echo "找不到程序目录 $APP_DIR"; exit 1; }
mkdir -p "$(dirname "$OUT")" || exit 1
AUTH=$(grep -E '^AISTUDIO_AUTH_STATES=' .env 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d '"[:space:]')

# 邮箱打码：保留前两位
mask='import re, sys; [sys.stdout.write(re.sub(r"\b([A-Za-z0-9._%+-]{2})[A-Za-z0-9._%+-]*@([A-Za-z0-9-]+\.)+[A-Za-z]{2,}", r"\1***@***", line)) for line in sys.stdin]'
python3 -I - "$APP_DIR" "${AUTH:-auth}" "$LOG_MINUTES" 2>&1 <<'PY' | python3 -I -c "$mask" | tee "$OUT"
import collections, datetime, json, os, re, statistics, sys, time
app, auth, minutes = sys.argv[1], sys.argv[2], int(sys.argv[3])
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


print(f"==================== 账号浏览器状态对比 {datetime.datetime.now().astimezone():%F %T %z} ====================")
since = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(minutes=minutes)
log = os.path.join(app, "logs", "aistudio2api.log")
ready, failed = collections.Counter(), collections.Counter()
for path in (log + ".1", log):
    if not os.path.exists(path):
        continue
    with open(path, "rb") as handle:
        size = os.path.getsize(path)
        handle.seek(max(0, size - (128 << 20)))
        if handle.tell():
            handle.readline()
        for raw in handle:
            if b"WAA Worker" not in raw:
                continue
            try:
                event = json.loads(raw)
            except ValueError:
                continue
            moment = parse_time(event.get("time", ""))
            if not moment or moment < since:
                continue
            head = str(event.get("msg", "")).split("\n")[0].split(" | ")[0].strip()
            source = str(event.get("source", "")).strip()
            if head == "WAA Worker 就绪":
                ready[source] += 1
            elif head == "WAA Worker 启动失败":
                failed[source] += 1
ok = set(ready)
bad = set(failed) - ok
print(f"最近 {minutes} 分钟：启动成功过的账号 {len(ok)} 个；只失败、没成功过的账号 {len(bad)} 个")
if not ok and not bad:
    print("这段时间没有启动结果，换更长的 LOG_MINUTES 再试")
    raise SystemExit

directories = []
for root in [part for part in auth.split(",") if part]:
    root = root if os.path.isabs(root) else os.path.join(app, root)
    if not os.path.isdir(root):
        continue
    if os.path.exists(os.path.join(root, "storage-state.json")) or os.path.exists(os.path.join(root, "account.json")):
        directories.append(root)
        continue
    for name in sorted(os.listdir(root)):
        directory = os.path.join(root, name)
        if os.path.isdir(directory) and (os.path.exists(os.path.join(directory, "storage-state.json")) or os.path.exists(os.path.join(directory, "account.json"))):
            directories.append(directory)
by_label = {}
for directory in directories:
    label = ""
    try:
        with open(os.path.join(directory, "account.json"), encoding="utf-8") as f:
            label = str(json.load(f).get("label") or "").strip()
    except Exception:
        pass
    by_label.setdefault(label or os.path.basename(directory), directory)
    by_label.setdefault(os.path.basename(directory), directory)
print(f"账户目录 {len(directories)} 个（{auth}）")


def tree_size(path, limit=20000):
    total, count, newest = 0, 0, 0.0
    for current, _, files in os.walk(path):
        for name in files:
            try:
                info = os.stat(os.path.join(current, name))
            except OSError:
                continue
            total += info.st_size
            count += 1
            newest = max(newest, info.st_mtime)
            if count >= limit:
                return total, count, newest
    return total, count, newest


now = time.time()


def profile(label):
    directory = by_label.get(label)
    if not directory:
        return None
    result = {"keys": {}, "cookies": 0, "age_days": None, "origins": 0, "cache": None}
    state_path = os.path.join(directory, "storage-state.json")
    try:
        result["age_days"] = (now - os.stat(state_path).st_mtime) / 86400
        with open(state_path, encoding="utf-8") as f:
            state = json.load(f)
        result["cookies"] = len(state.get("cookies") or [])
        origins = state.get("origins") or []
        result["origins"] = len(origins)
        for origin in origins:
            if "aistudio.google.com" in str(origin.get("origin", "")):
                for item in origin.get("localStorage") or []:
                    result["keys"][str(item.get("name", ""))] = len(str(item.get("value", "")))
    except Exception as err:
        result["error"] = str(err)[:80]
    cache = os.path.join(directory, "camoufox-cache")
    if os.path.isdir(cache):
        size, count, newest = tree_size(cache)
        result["cache"] = (size, count, (now - newest) / 86400 if newest else None)
    return result


def summarize(name, labels):
    profiles = [p for p in (profile(label) for label in sorted(labels)) if p is not None]
    print()
    print(f"---- {name}：{len(labels)} 个，找到账户目录 {len(profiles)} 个 ----")
    if not profiles:
        return profiles
    ages = sorted(p["age_days"] for p in profiles if p["age_days"] is not None)
    if ages:
        print(f"storage-state.json 最后修改距今：中位 {statistics.median(ages):.1f} 天，最新 {ages[0]:.1f} 天，最旧 {ages[-1]:.1f} 天")
    with_ls = [p for p in profiles if p["keys"]]
    print(f"有 aistudio.google.com localStorage 的：{len(with_ls)}/{len(profiles)}，键数中位 {statistics.median([len(p['keys']) for p in profiles]):.0f}")
    print(f"Cookie 数中位 {statistics.median([p['cookies'] for p in profiles]):.0f}；origins 数中位 {statistics.median([p['origins'] for p in profiles]):.0f}")
    caches = [p["cache"] for p in profiles if p["cache"]]
    if caches:
        sizes = sorted(c[0] for c in caches)
        olds = sorted(c[2] for c in caches if c[2] is not None)
        print(f"有 camoufox-cache 的：{len(caches)}/{len(profiles)}，大小中位 {statistics.median(sizes) / 1048576:.1f} MB，"
              f"最近写入距今中位 {statistics.median(olds) if olds else 0:.1f} 天")
    else:
        print(f"有 camoufox-cache 的：0/{len(profiles)}")
    errors = [p["error"] for p in profiles if p.get("error")]
    if errors:
        print(f"读取出错 {len(errors)} 个，例如：{errors[0]}")
    return profiles


bad_profiles = summarize("只失败的账号", bad)
ok_profiles = summarize("成功过的账号", ok)
if bad_profiles and ok_profiles:
    print()
    print("---- localStorage 键在两组中的出现比例（按差距排序，前 30）----")
    keys = set()
    for p in bad_profiles + ok_profiles:
        keys.update(p["keys"])
    rows = []
    for key in keys:
        in_bad = sum(1 for p in bad_profiles if key in p["keys"]) / len(bad_profiles)
        in_ok = sum(1 for p in ok_profiles if key in p["keys"]) / len(ok_profiles)
        lengths_bad = [p["keys"][key] for p in bad_profiles if key in p["keys"]]
        lengths_ok = [p["keys"][key] for p in ok_profiles if key in p["keys"]]
        rows.append((abs(in_bad - in_ok), key, in_bad, in_ok,
                     statistics.median(lengths_bad) if lengths_bad else 0, statistics.median(lengths_ok) if lengths_ok else 0))
    for _, key, in_bad, in_ok, len_bad, len_ok in sorted(rows, reverse=True)[:30]:
        print(f"  {key[:70]:70s} 失败组 {in_bad:5.0%}（值长中位 {len_bad:.0f}）  成功组 {in_ok:5.0%}（值长中位 {len_ok:.0f}）")
print()
print("把以上全部输出发给开发者（只有键名与长度，没有 Cookie 与 localStorage 的值）")
PY
echo
echo "已保存到 $OUT"
