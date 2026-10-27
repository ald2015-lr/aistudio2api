#!/usr/bin/env bash
# AIStudio2API 应急：把“AI Studio 已被账号所属组织停用”的账户目录移出账户目录，程序随即把它们移出账户池，不再占启动名额。
# 依据：最近的运行日志里，启动或验证时被跳到 access.workspace.google.com/ServiceNotAllowed（页面写着
# “Your account is managed by an organisation that has this service turned off”），或模型目录同步返回 “AI Studio app is disabled”；
# 这段时间里又启动成功过的账户不算。
# 默认只列出，不改任何文件；APPLY=1 时移动目录（不删除），RESTORE=1 时按记录全部移回原处（程序会自动重新导入）。
# 移到 $APP_DIR/auth-quarantine/，记录写在其中的 manifest.jsonl；正在被浏览器使用（camoufox-cache/.lock 被占用）的目录本次跳过。
#
# 用法（放在程序目录的 tools/ 下运行，或用 APP_DIR 指定程序目录）：
#   bash tools/quarantine_disabled.sh                    只列出（默认统计最近 360 分钟的日志）
#   APPLY=1 bash tools/quarantine_disabled.sh            移走这些账户目录
#   RESTORE=1 bash tools/quarantine_disabled.sh          全部移回原处
#   LOG_MINUTES=720 bash tools/quarantine_disabled.sh    改统计窗口
set -u
script_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd)"
if [ -z "${APP_DIR:-}" ]; then
  for candidate in "$script_dir/.." "$PWD" /www/wwwroot/Chat2API/build; do
    if [ -f "$candidate/start.sh" ]; then APP_DIR="$(cd "$candidate" && pwd)"; break; fi
  done
fi
APP_DIR="${APP_DIR:-/www/wwwroot/Chat2API/build}"
: "${LOG_MINUTES:=360}" "${APPLY:=0}" "${RESTORE:=0}"
case "$LOG_MINUTES" in "" | *[!0-9]* | 0) echo "LOG_MINUTES 必须是正整数"; exit 1 ;; esac
command -v python3 >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }
cd "$APP_DIR" || { echo "找不到程序目录 $APP_DIR"; exit 1; }
AUTH=$(grep -E '^AISTUDIO_AUTH_STATES=' .env 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d '"[:space:]'"'")

python3 -I - "$APP_DIR" "${AUTH:-auth}" "$LOG_MINUTES" "$APPLY" "$RESTORE" <<'PY'
import collections, datetime, fcntl, json, os, re, sys, time
app, auth, minutes, apply, restore = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4] == "1", sys.argv[5] == "1"
quarantine = os.path.join(app, "auth-quarantine")
manifest = os.path.join(quarantine, "manifest.jsonl")
EMAIL_RE = re.compile(r"\b([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+)")
TIME_RE = re.compile(r"^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$")
CATALOG_RE = re.compile(r"刷新账户 (\S+) 的模型目录")


def mask(text):
    # 本地部分只留前两位，域名保留（按域名能看出是哪一批账号）
    return EMAIL_RE.sub(lambda m: m.group(1) + "***@" + m.group(2), str(text))


def parse_time(value):
    m = TIME_RE.match((value or "").strip())
    if not m:
        return None
    zone = "+00:00" if m.group(3) == "Z" else m.group(3)
    try:
        return datetime.datetime.fromisoformat(f"{m.group(1)}.{(m.group(2) or '0')[:6].ljust(6, '0')}{zone}")
    except ValueError:
        return None


roots = []
for part in [p for p in auth.split(",") if p]:
    root = os.path.realpath(part if os.path.isabs(part) else os.path.join(app, part))
    if os.path.isdir(root):
        roots.append(root)
real_quarantine = os.path.realpath(quarantine)
for root in roots:
    if real_quarantine == root or real_quarantine.startswith(root + os.sep) or root.startswith(real_quarantine + os.sep):
        print(f"隔离目录 {quarantine} 与账户目录 {root} 重叠，已放弃")
        raise SystemExit(1)


def move(source, target):
    if os.path.lexists(target):
        return f"目标已存在：{target}"
    try:
        os.rename(source, target)
    except OSError as err:
        return f"移动失败：{err}"
    return ""


if restore:
    if not os.path.exists(manifest):
        print("没有隔离记录，无需恢复")
        raise SystemExit
    rows = []
    with open(manifest, encoding="utf-8") as handle:
        for line in handle:
            try:
                rows.append(json.loads(line))
            except ValueError:
                continue
    moved, failed, kept = 0, 0, []
    for row in rows:
        source, target = row.get("to", ""), row.get("from", "")
        # 只移回本脚本记录过的、仍在隔离目录里的目录，原处必须在某个账户目录下
        if not source.startswith(real_quarantine + os.sep) or not os.path.isdir(source) or not any(os.path.dirname(target) == r for r in roots):
            continue
        problem = move(source, target)
        if problem:
            failed += 1
            kept.append(row)
            print("  " + problem)
        else:
            moved += 1
    with open(manifest + ".tmp", "w", encoding="utf-8") as handle:
        for row in kept:
            handle.write(json.dumps(row, ensure_ascii=False) + "\n")
    os.replace(manifest + ".tmp", manifest)
    print(f"已移回 {moved} 个账户目录，失败 {failed} 个；程序会在几十秒内自动重新导入")
    raise SystemExit

# 账户目录：每个账户目录下有 storage-state.json 或 account.json；按 account.json 的 label 与目录名索引
directories = {}
for root in roots:
    if os.path.exists(os.path.join(root, "storage-state.json")) or os.path.exists(os.path.join(root, "account.json")):
        continue  # 单个账户目录直接列在 AISTUDIO_AUTH_STATES 里：不移动整个根
    for name in sorted(os.listdir(root)):
        directory = os.path.join(root, name)
        if name.startswith(".") or not os.path.isdir(directory) or os.path.islink(directory):
            continue
        if not (os.path.exists(os.path.join(directory, "storage-state.json")) or os.path.exists(os.path.join(directory, "account.json"))):
            continue
        label = ""
        try:
            with open(os.path.join(directory, "account.json"), encoding="utf-8") as f:
                label = str(json.load(f).get("label") or "").strip()
        except Exception:
            pass
        for key in (label, name):
            if key:
                directories.setdefault(key.lower(), directory)

since = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(minutes=minutes)
disabled = collections.defaultdict(lambda: {"page": 0, "catalog": 0, "verify": 0, "last": None})
ready_at = {}
log = os.path.join(app, "logs", "aistudio2api.log")
for path in (log + ".1", log):
    if not os.path.exists(path):
        continue
    with open(path, "rb") as handle:
        size = os.path.getsize(path)
        handle.seek(max(0, size - (512 << 20)))
        if handle.tell():
            handle.readline()
        for raw in handle:
            if b"ServiceNotAllowed" not in raw and b"AI Studio app is disabled" not in raw and b"WAA Worker" not in raw:
                continue
            try:
                event = json.loads(raw)
            except ValueError:
                continue
            moment = parse_time(event.get("time", ""))
            if not moment or moment < since:
                continue
            msg = str(event.get("msg", ""))
            source = str(event.get("source", "")).strip()
            head = msg.split("\n")[0].split(" | ")[0].strip()
            if head == "WAA Worker 就绪" and source:
                ready_at[source.lower()] = max(ready_at.get(source.lower(), moment), moment)
                continue
            if "access.workspace.google.com/ServiceNotAllowed" in msg and source:
                kind = "verify" if "验证" in head or "登录" in head else "page"
                key = source.lower()
            elif "AI Studio app is disabled" in msg:
                m = CATALOG_RE.search(msg)
                key = (m.group(1) if m else source).lower()
                kind = "catalog"
            else:
                continue
            if not key:
                continue
            entry = disabled[key]
            entry[kind] += 1
            entry["last"] = moment if entry["last"] is None else max(entry["last"], moment)

found, unmatched, recovered = [], [], 0
for key, entry in disabled.items():
    if key in ready_at and ready_at[key] > entry["last"]:
        recovered += 1
        continue
    directory = directories.get(key)
    if directory:
        found.append((key, directory, entry))
    else:
        unmatched.append(key)
found.sort(key=lambda item: item[1])

print(f"==================== 组织停用 AI Studio 的账户（最近 {minutes} 分钟的日志）====================")
print(f"账户目录 {len(set(directories.values()))} 个；日志里被判定为组织停用的账户 {len(disabled)} 个，其中之后又启动成功的 {recovered} 个（不算）")
print(f"要隔离的 {len(found)} 个；在账户目录里找不到的 {len(unmatched)} 个（已被移走或删除）")
domains = collections.Counter(key.split("@", 1)[1] if "@" in key else "（不是邮箱的标签）" for key, _, _ in found)
if domains:
    print("按域名：" + "，".join(f"{domain} {count}" for domain, count in domains.most_common(15)))
evidence = collections.Counter()
for _, _, entry in found:
    for kind in ("page", "catalog", "verify"):
        if entry[kind]:
            evidence[kind] += 1
print(f"依据：启动时被跳到 ServiceNotAllowed 的 {evidence['page']} 个，模型目录返回 AI Studio app is disabled 的 {evidence['catalog']} 个，验证或登录时的 {evidence['verify']} 个")
for key, directory, entry in found[:10]:
    print(f"  {mask(key)}  目录 {os.path.basename(directory)}  启动 {entry['page']} 次 / 模型目录 {entry['catalog']} 次 / 验证 {entry['verify']} 次")
if len(found) > 10:
    print(f"  …… 另有 {len(found) - 10} 个")

if not apply:
    print()
    print("以上只是列出，没有改任何文件。确认后执行：APPLY=1 bash tools/quarantine_disabled.sh")
    print(f"会把这些账户目录移到 {quarantine}/（不删除）；以后要恢复执行：RESTORE=1 bash tools/quarantine_disabled.sh")
    raise SystemExit

os.makedirs(quarantine, exist_ok=True)
moved, busy, failed = 0, 0, 0
with open(manifest, "a", encoding="utf-8") as record:
    for key, directory, entry in found:
        # 只移动账户目录下的直接子目录，且不是符号链接
        parent = os.path.dirname(directory)
        if parent not in roots or os.path.islink(directory) or not os.path.isdir(directory):
            failed += 1
            continue
        lock_path = os.path.join(directory, "camoufox-cache", ".lock")
        if os.path.exists(lock_path):
            try:
                with open(lock_path, "rb") as lock_file:
                    fcntl.flock(lock_file, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    fcntl.flock(lock_file, fcntl.LOCK_UN)
            except OSError:
                busy += 1
                continue
        target = os.path.join(quarantine, f"{roots.index(parent)}-{os.path.basename(directory)}")
        problem = move(directory, target)
        if problem:
            failed += 1
            print("  " + problem)
            continue
        record.write(json.dumps({"from": directory, "to": target, "time": int(time.time())}, ensure_ascii=False) + "\n")
        record.flush()
        moved += 1
print()
print(f"已移走 {moved} 个账户目录到 {quarantine}/；正在被浏览器使用、本次跳过的 {busy} 个（过几分钟再执行一次）；失败 {failed} 个")
print("程序会在几十秒内把它们移出账户池。要恢复：RESTORE=1 bash tools/quarantine_disabled.sh")
PY
