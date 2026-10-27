#!/usr/bin/env bash
# AIStudio2API 应急：把“AI Studio 已被账号所属组织停用”的账户目录移出账户目录，程序随即把它们移出账户池，不再占启动名额。
# 依据：最近 LOG_MINUTES 分钟的运行日志里，Worker 启动或后台扩容、账户验证、登录、新账户自动处理时被跳到 AI Studio 的停用页
# access.workspace.google.com/ServiceNotAllowed（application=151518919664，或 continue= 指向 aistudio.google.com；页面写着
# “Your account is managed by an organisation that has this service turned off”），或模型目录同步返回 “AI Studio app is disabled”。
# 只关了 Drive 的（Drive 授权停在 ServiceNotAllowed）不算；之后又出现过 “WAA Worker 就绪” 的不算；RESTORE 移回之后（记在
# auth-quarantine/restored.jsonl）、或程序重新导入这个目录之后（日志 “自动导入账户”），还没有新的停用记录的也不算。
# Worker 启动、验证、登录类日志按账户标签（account.json 的 label，不区分大小写）记，模型目录与后台扩容按账户 ID（目录名，与程序
# 一样区分大小写）记，分别换算到账户目录；一个标签对应多个账户目录、分不清是哪一个的单独列出，不移动（INCLUDE_AMBIGUOUS=1 时一并算上）。
# 默认只列出，不改任何文件；APPLY=1 移动目录（不删除），RESTORE=1 按记录移回原处（程序会自动重新导入）。
# 移到 $APP_DIR/auth-quarantine/<根序号>-<目录名>；账户根目录与它不在同一文件系统时改移到 <账户根目录>/.quarantine-disabled/<目录名>
# （点开头，程序与 start.sh 都不扫描）。记录在 auth-quarantine/manifest.jsonl：先写完整一行记录（落盘）再移动，写记录失败（如磁盘写满）
# 时截回、停止，不移动这一个；RESTORE 只删除确认移回的记录。
# 移动时持有该目录 camoufox-cache/.lock 的锁（浏览器使用缓存期间一直持有），被占用的本次跳过；想一次全部移走：
# ./start.sh stop，再 APPLY=1 bash tools/quarantine_disabled.sh，再 ./start.sh start。
#
# 用法（放在程序目录的 tools/ 下运行，或用 APP_DIR 指定程序目录）：
#   bash tools/quarantine_disabled.sh                            只列出（默认统计最近 360 分钟的日志；SHOW_ALL=1 列出全部名单）
#   APPLY=1 bash tools/quarantine_disabled.sh                    移走这些账户目录
#   RESTORE=1 bash tools/quarantine_disabled.sh                  按记录全部移回原处
#   RESTORE=1 RESTORE_MATCH=corp.com bash tools/quarantine_disabled.sh   只移回目录名含 corp.com 的（不区分大小写）
#   RESTORE=1 RESTORE_FORCE=1 bash tools/quarantine_disabled.sh  原账户根目录已不在 AISTUDIO_AUTH_STATES 里、但仍存在时也移回
#   LOG_MINUTES=720 bash tools/quarantine_disabled.sh            改统计窗口
# 只在组织已重新开放 AI Studio、或程序已能识别 ServiceNotAllowed 之后再 RESTORE，否则这些账户会再次占满启动名额。
set -u
script_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd)"
if [ -z "${APP_DIR:-}" ]; then
  for candidate in "$script_dir/.." "$PWD" /www/wwwroot/Chat2API/build; do
    if [ -f "$candidate/start.sh" ]; then APP_DIR="$(cd "$candidate" && pwd -P)"; break; fi
  done
fi
APP_DIR="${APP_DIR:-/www/wwwroot/Chat2API/build}"
: "${LOG_MINUTES:=360}" "${APPLY:=0}" "${RESTORE:=0}" "${RESTORE_MATCH:=}" "${RESTORE_FORCE:=0}" "${SHOW_ALL:=0}" "${INCLUDE_AMBIGUOUS:=0}"
case "$LOG_MINUTES" in "" | *[!0-9]* | 0) echo "LOG_MINUTES 必须是正整数"; exit 1 ;; esac
if [ "$APPLY" = 1 ] && [ "$RESTORE" = 1 ]; then echo "APPLY=1 与 RESTORE=1 不能同时使用"; exit 1; fi
command -v python3 >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }
cd "$APP_DIR" || { echo "找不到程序目录 $APP_DIR"; exit 1; }

# 与 start.sh 的 trim/dotenv_value 相同（与程序解析一致：允许 export 前缀、BOM、首尾空白、引号、未加引号时 “ #” 之后的行内注释）
trim() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "$s"
}
dotenv_value() {
  local key="$1" file="${2:-.env}" value="" line quote rest
  [[ -f "$file" ]] || return 0
  line="$(sed '1s/^\xEF\xBB\xBF//' "$file" 2>/dev/null | grep -E "^[[:space:]]*(export[[:space:]]+)?${key}[[:space:]]*=" | tail -n 1 | tr -d '\r')"
  if [[ -n "$line" ]]; then
    value="$(trim "${line#*=}")"
    quote="${value:0:1}"
    if [[ "$quote" == '"' || "$quote" == "'" ]]; then
      rest="${value:1}"
      value="${rest%%"$quote"*}"
    else
      value="$(trim "${value%% #*}")"
    fi
  fi
  printf '%s' "$value"
}
# 与程序相同的优先级：非空的进程环境变量优先，其次 .env，最后默认 auth
AUTH="$(trim "${AISTUDIO_AUTH_STATES-}")"
AUTH_FROM="进程环境变量 AISTUDIO_AUTH_STATES"
if [ -z "$AUTH" ]; then
  AUTH="$(dotenv_value AISTUDIO_AUTH_STATES)"
  AUTH_FROM=".env"
fi
if [ -z "$AUTH" ]; then
  AUTH=auth
  AUTH_FROM="默认值 auth（进程环境与 .env 都没有设置）"
fi

python3 -I - "$(pwd -P)" "$AUTH" "$AUTH_FROM" "$LOG_MINUTES" "$APPLY" "$RESTORE" "$RESTORE_MATCH" "$RESTORE_FORCE" "$SHOW_ALL" "$INCLUDE_AMBIGUOUS" <<'PY'
import collections, datetime, errno, fcntl, json, os, re, shutil, signal, sys, time

args = sys.argv[1:]
app = os.path.realpath(args[0])
auth, auth_from, minutes = args[1], args[2], int(args[3])
apply, restore = args[4] == "1", args[5] == "1"
restore_match = args[6].strip().lower()
restore_force, show_all, include_ambiguous = args[7] == "1", args[8] == "1", args[9] == "1"

# 记录里一律写解析过符号链接的真实路径：APP_DIR 或账户根目录带符号链接时，APPLY 与 RESTORE 看到的路径相同
quarantine = os.path.realpath(os.path.join(app, "auth-quarantine"))
manifest = os.path.join(quarantine, "manifest.jsonl")
# RESTORE 每移回一个目录记一行 {"path": 原处真实路径, "time": 移回时间}：之前的停用依据属于隔离前，列出与 APPLY 时不再算数
restored_log = os.path.join(quarantine, "restored.jsonl")
FALLBACK = ".quarantine-disabled"
ACCOUNT_FILES = ("storage-state.json", "account.json")
STAMP = time.strftime("%Y%m%d-%H%M%S")
# 本地部分允许撇号（Workspace 用户名可以有 '）；不用 \b 定位：撇号或汉字紧挨着时 \b 会漏掉开头
EMAIL_RE = re.compile(r"(?<![A-Za-z0-9._%+'-])([A-Za-z0-9._%+'-]{1,2})[A-Za-z0-9._%+'-]*@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+)")
RECORD_START_RE = re.compile(rb'\{\s*"from"\s*:')
TIME_RE = re.compile(r"^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$")
CATALOG_RE = re.compile(r"刷新账户 (\S+) 的模型目录")
NOT_ALLOWED_RE = re.compile(r"access\.workspace\.google\.com/ServiceNotAllowed[^\s\"'|]*", re.I)
AISTUDIO_CONTINUE_RE = re.compile(r"continue=https?(?::|%3A|%253A)(?:/|%2F|%252F){2}aistudio\.google\.com", re.I)
AISTUDIO_APPLICATION = "application=151518919664"
# 日志行首 → 依据类别。Worker 启动失败、验证、登录、新账户自动处理的 source 是账户标签；后台扩容失败与模型目录的是账户 ID（目录名）
WORKER_HEADS = {"WAA Worker 启动失败": "start", "WAA Worker 后台扩容失败": "expand"}
VERIFY_HEADS = {"账户验证", "账户验证失败", "账户验证完成", "新账户自动处理", "账户登录", "账户登录失败"}
NON_ACCOUNT_SOURCES = {"", "auth", "service"}
STOP_SIGNALS = {signal.SIGINT, signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT}


def mask(text):
    # 本地部分只留前两位，域名保留（按域名能看出是哪一批账号）；目录名、路径、错误文字打印前都先过这里
    return EMAIL_RE.sub(lambda m: m.group(1) + "***@" + m.group(2), str(text))


def examples(items, limit=3):
    items = list(items)
    return "，".join(mask(item) for item in items[:limit]) + ("……" if len(items) > limit else "")


def reason(err):
    # 只用 strerror：str(err) 里带完整的源路径与目标路径（含邮箱）
    return err.strerror or ("errno " + str(err.errno))


def parse_time(value):
    m = TIME_RE.match((value or "").strip())
    if not m:
        return None
    zone = "+00:00" if m.group(3) == "Z" else m.group(3)
    try:
        return datetime.datetime.fromisoformat(f"{m.group(1)}.{(m.group(2) or '0')[:6].ljust(6, '0')}{zone}")
    except ValueError:
        return None


def clock(moment):
    return moment.astimezone().strftime("%m-%d %H:%M") if moment else "-"


def is_account(directory):
    return any(os.path.exists(os.path.join(directory, name)) for name in ACCOUNT_FILES)


def read_label(directory):
    try:
        with open(os.path.join(directory, "account.json"), encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, ValueError):
        return ""
    return str(value.get("label") or "").strip() if isinstance(value, dict) else ""


def parse_row(raw):
    try:
        row = json.loads(raw)
    except ValueError:
        return None
    if not (isinstance(row, dict) and isinstance(row.get("from"), str) and isinstance(row.get("to"), str)
            and os.path.isabs(row["from"]) and os.path.isabs(row["to"])):
        return None
    return row


def read_manifest():
    """返回 [(原始行, 解析出的记录或 None)]，没有记录文件时返回 None。解析不了的行原样保留，不丢。
    旧版本写记录失败（磁盘写满）留下半行、下一条记录接在它后面时（如 {"{"from": ...}），按记录开头拆开，记录照常认"""
    try:
        handle = open(manifest, "rb")
    except FileNotFoundError:
        return None
    rows = []
    with handle:
        for raw in handle:
            if not raw.strip():
                continue
            if not raw.endswith(b"\n"):
                raw += b"\n"
            row = parse_row(raw)
            if row is None:
                starts = [m.start() for m in RECORD_START_RE.finditer(raw)]
                pieces = [raw[a:b].strip() for a, b in zip([0] + starts, starts + [len(raw)])]
                pieces = [piece + b"\n" for piece in pieces if piece]
                if len(pieces) > 1 and any(parse_row(piece) for piece in pieces):
                    rows.extend((piece, parse_row(piece)) for piece in pieces)
                    continue
            rows.append((raw, row))
    return rows


def truncate_record(fd, offset):
    try:
        os.ftruncate(fd, offset)
        os.fsync(fd)
        return True
    except OSError:
        return False


def append_record(fd, line):
    """把完整一行追加到记录文件并落盘（自己写完，不经过 Python 缓冲，关闭时不会补写残余）。
    返回 (写之前的长度, 失败原因)；失败时尽力截回原长度，不留半行"""
    offset = os.fstat(fd).st_size
    try:
        view = memoryview(line)
        while view:
            written = os.write(fd, view)
            if written <= 0:
                raise OSError(errno.EIO, os.strerror(errno.EIO))
            view = view[written:]
        os.fsync(fd)
    except OSError as err:
        truncate_record(fd, offset)
        return offset, reason(err)
    return offset, ""


def open_record(path):
    """以追加方式打开记录文件，返回 (fd, 说明)。末尾没有换行的半行（上次写记录失败或被强行结束时留下）：能解析的补上换行；
    解析不了的是没写完的记录，记录写完、落盘之前不会移动目录，截掉。修不了时抛出 OSError"""
    fd = os.open(path, os.O_RDWR | os.O_APPEND | os.O_CREAT, 0o600)
    try:
        size = os.fstat(fd).st_size
        tail = os.pread(fd, min(size, 1 << 16), max(0, size - (1 << 16))) if size else b""
        if not size or tail.endswith(b"\n"):
            return fd, ""
        cut = tail.rfind(b"\n")
        fragment = tail[cut + 1:] if cut >= 0 or size <= len(tail) else None
        try:
            whole = fragment is not None and isinstance(json.loads(fragment), dict)
        except ValueError:
            whole = False
        if whole or fragment is None:
            _, error = append_record(fd, b"\n")
            if error:
                raise OSError(errno.EIO, error)
            return fd, f"{os.path.basename(path)} 末尾的一行缺换行，已补上"
        if not truncate_record(fd, size - len(fragment)):
            raise OSError(errno.EIO, "截掉末尾没写完的半行失败")
        return fd, f"{os.path.basename(path)} 末尾有半行没写完的记录（上次写记录失败或被强行结束时留下的，对应的目录没有移动），已去掉"
    except BaseException:
        os.close(fd)
        raise


def read_restored():
    """restored.jsonl → {原处路径: 最近一次移回的时间}"""
    times = {}
    try:
        handle = open(restored_log, "rb")
    except OSError:
        return times
    with handle:
        for raw in handle:
            try:
                row = json.loads(raw)
            except ValueError:
                continue
            if isinstance(row, dict) and isinstance(row.get("path"), str) and isinstance(row.get("time"), (int, float)):
                moment = datetime.datetime.fromtimestamp(row["time"], datetime.timezone.utc)
                times[row["path"]] = max(times.get(row["path"], moment), moment)
    return times


def program_running():
    """程序是否在运行：先看 start.sh 的 PID 文件，再找 /proc 里工作目录是程序目录的 aistudio2api 进程；判断不了时返回 None"""
    def ours(pid, need_cwd):
        try:
            with open(f"/proc/{pid}/comm") as handle:
                if handle.read().strip() != "aistudio2api":
                    return False
        except OSError:
            return False
        try:
            return os.path.realpath(os.readlink(f"/proc/{pid}/cwd")) == app
        except OSError:
            return not need_cwd  # 与 start.sh 的 pid_is_ours 相同：看不到工作目录时按进程名认
    try:
        with open(os.path.join(app, "run", "aistudio2api.pid")) as handle:
            pid = "".join(ch for ch in handle.read() if ch.isdigit())
        if pid and ours(pid, False):
            return True
    except OSError:
        pass
    try:
        return any(ours(name, True) for name in os.listdir("/proc") if name.isdigit())
    except OSError:
        return None


def recorded_in_quarantine(row):
    # 只认本脚本的隔离位置：auth-quarantine/，或原账户根目录下的 .quarantine-disabled/（旧版本记的未解析链接的路径也认）
    parent = os.path.realpath(os.path.dirname(row["to"]))
    return parent == quarantine or parent == os.path.realpath(os.path.join(os.path.dirname(row["from"]), FALLBACK))


def lock_instance():
    os.makedirs(quarantine, exist_ok=True)
    handle = open(os.path.join(quarantine, ".lock"), "a")
    try:
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        print("另一个 quarantine_disabled.sh（APPLY=1 或 RESTORE=1）正在运行，等它结束再执行")
        raise SystemExit(1)
    return handle


def stop(signum, frame):
    raise KeyboardInterrupt


class Shielded:
    """写记录与改名之间不响应 Ctrl+C、kill：信号推迟到这一段结束后处理"""

    def __enter__(self):
        self.saved = signal.pthread_sigmask(signal.SIG_BLOCK, STOP_SIGNALS)

    def __exit__(self, *exc):
        signal.pthread_sigmask(signal.SIG_SETMASK, self.saved)
        return False


def print_failures(failures):
    for problem, names in sorted(failures.items(), key=lambda item: -len(item[1])):
        print(f"  {problem}：{len(names)} 个（例：{examples(names)}）")


# ---------- 账户根目录：与程序相同，逗号分隔、去首尾空白，相对路径按程序目录 ----------
roots, singles, problems = [], [], []
position = {}  # 单个账户目录在 AISTUDIO_AUTH_STATES 里是第几项：打印时邮箱打了码，按项号找
for number, part in enumerate([part.strip() for part in auth.split(",") if part.strip()], 1):
    path = os.path.realpath(part if os.path.isabs(part) else os.path.join(app, part))
    if os.path.isdir(path):
        bucket = singles if is_account(path) else roots
    elif os.path.isfile(path) and os.path.basename(path) == "storage-state.json":
        path, bucket = os.path.dirname(path), singles
    else:
        problems.append(path)
        continue
    if path not in bucket:
        bucket.append(path)
        position.setdefault(path, number)

print("==================== 组织停用 AI Studio 的账户 ====================")
print(f"账户根目录（AISTUDIO_AUTH_STATES，来自 {auth_from}）：")
if not roots and not singles:
    for path in problems:
        print(f"  不存在或不是目录：{mask(path)}")
    print("AISTUDIO_AUTH_STATES 里的路径一个都不存在，已放弃，没有改任何文件（请核对 .env 或进程环境变量）")
    raise SystemExit(1)
for root in roots + singles:
    if quarantine == root or quarantine.startswith(root + os.sep) or root.startswith(quarantine + os.sep):
        print(f"隔离目录 {mask(quarantine)} 与账户目录 {mask(root)} 重叠，已放弃")
        raise SystemExit(1)

if restore:
    # ---------- RESTORE：只删除确认已在原处的记录，其余原样保留 ----------
    for index, root in enumerate(roots):
        print(f"  [{index}] {mask(root)}")
    for path in problems:
        print(f"  不存在或不是目录（已忽略）：{mask(path)}")
    if not os.path.isdir(quarantine):
        print(f"没有隔离目录 {mask(quarantine)}，无需恢复")
        raise SystemExit
    instance = lock_instance()
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGHUP, stop)
    notes = []
    try:
        if os.path.exists(manifest):
            fd, note_text = open_record(manifest)
            os.close(fd)
            notes.append(note_text)
        ledger, note_text = open_record(restored_log)
        notes.append(note_text)
    except OSError as err:
        print(f"打开或修复记录文件失败：{reason(err)}，没有移回任何目录")
        raise SystemExit(1)
    for note_text in filter(None, notes):
        print(note_text)
    rows = read_manifest() or []
    root_set, absent_roots = set(roots), set(problems)
    kept, restored, dropped, stubbed = [], [], 0, 0
    skipped = collections.defaultdict(list)
    other_roots = collections.Counter()
    interrupted, record_error, rewrite_error, unvisited = False, "", "", 0

    def stub_lock(directory):
        """原处只有缓存残骸（没有账户文件，至多一个 camoufox-cache）且拿到了它的缓存锁时返回锁（空目录返回 -1），否则返回 None"""
        if os.path.islink(directory) or not os.path.isdir(directory) or is_account(directory):
            return None
        try:
            entries = os.listdir(directory)
        except OSError:
            return None
        if not entries:
            return -1
        cache = os.path.join(directory, "camoufox-cache")
        if entries != ["camoufox-cache"] or os.path.islink(cache) or not os.path.isdir(cache):
            return None
        try:
            fd = os.open(os.path.join(cache, ".lock"), os.O_RDONLY | os.O_CREAT, 0o600)
        except OSError:
            return None
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            os.close(fd)
            return None
        return fd

    try:
        for number, (raw, row) in enumerate(rows, 1):
            if row is None:
                kept.append((raw, row))
                skipped["无法解析的记录（原样保留）"].append(f"第 {number} 行")
                continue
            source, target = row["to"], row["from"]
            name = os.path.basename(target)
            parent = os.path.dirname(target)
            if not recorded_in_quarantine(row):
                kept.append((raw, row))
                skipped["隔离位置不是本脚本的隔离目录（原样保留）"].append(name)
                continue
            if restore_match and restore_match not in name.lower():
                kept.append((raw, row))
                skipped["不匹配 RESTORE_MATCH（留在记录里）"].append(name)
                continue
            if parent in absent_roots:
                kept.append((raw, row))
                skipped["原账户根目录现在不存在，是否没挂载？（留在记录里）"].append(name)
                continue
            if parent not in root_set and not (restore_force and os.path.isdir(parent)):
                kept.append((raw, row))
                other_roots[parent] += 1
                skipped["原账户根目录不在当前 AISTUDIO_AUTH_STATES 里（留在记录里）"].append(name)
                continue
            if not os.path.isdir(parent):
                kept.append((raw, row))
                skipped["原账户根目录不存在（留在记录里）"].append(name)
                continue
            if os.path.islink(source) or (os.path.lexists(source) and not os.path.isdir(source)):
                kept.append((raw, row))
                skipped["隔离目录里的条目不是目录（留在记录里）"].append(name)
                continue
            if not os.path.isdir(source):
                if os.path.lexists(target):
                    dropped += 1  # 已经在原处：之前移回过（含中断的 RESTORE），或写了记录但没移成
                else:
                    kept.append((raw, row))
                    skipped["隔离目录与原处都没有（留在记录里）"].append(name)
                continue
            if os.path.lexists(target):
                fd = stub_lock(target)
                if fd is None:
                    kept.append((raw, row))
                    skipped["原处已有账户目录或正在使用，没有覆盖（留在记录里）"].append(name)
                    continue
                # 原处只是 APPLY 时正在启动的 Worker 重建的缓存残骸：持锁挪进隔离目录后照常移回
                problem = ""
                for base in (os.path.dirname(source), os.path.join(parent, FALLBACK)):
                    try:
                        os.makedirs(base, 0o700, exist_ok=True)
                        os.rename(target, os.path.join(base, name + ".stub-" + STAMP))
                        problem = ""
                        break
                    except OSError as err:
                        problem = "原处的缓存残骸挪不开：" + reason(err) + "（留在记录里）"
                if fd >= 0:
                    os.close(fd)
                if problem:
                    kept.append((raw, row))
                    skipped[problem].append(name)
                    continue
                stubbed += 1
            # 先记下移回时间（落盘）再移回：之后列出与 APPLY 时，早于这个时间的停用依据不再算数，不会按隔离前的旧依据再次移走
            line = (json.dumps({"path": target, "time": int(time.time())}, ensure_ascii=False) + "\n").encode("utf-8")
            with Shielded():
                offset, error = append_record(ledger, line)
                if error:
                    kept.append((raw, row))
                    kept.extend(rows[number:])
                    record_error = "写移回记录 restored.jsonl 失败：" + error
                    unvisited = len(rows) - number + 1
                    break
                try:
                    os.rename(source, target)
                    restored.append(name)
                except OSError as err:
                    truncate_record(ledger, offset)
                    kept.append((raw, row))
                    skipped["移回失败：" + reason(err) + "（留在记录里）"].append(name)
    except KeyboardInterrupt:
        interrupted = True
    finally:
        os.close(ledger)
    backup = manifest + ".bak-" + STAMP
    serial = 0
    while os.path.lexists(backup):
        serial += 1
        backup = f"{manifest}.bak-{STAMP}-{serial}"
    if not interrupted and len(kept) != len(rows):
        temporary = manifest + ".tmp"
        with Shielded():
            try:
                shutil.copy2(manifest, backup)
                with open(temporary, "wb") as handle:
                    handle.writelines(raw for raw, _ in kept)
                    handle.flush()
                    os.fsync(handle.fileno())
                os.replace(temporary, manifest)
            except OSError as err:
                # 记录保持原样：已移回的那些下次执行时按“已在原处”自动去掉
                rewrite_error = reason(err)
                for leftover in (temporary, backup):
                    try:
                        os.unlink(leftover)
                    except OSError:
                        pass
    print()
    if interrupted:
        print(f"已中断：已移回 {len(restored)} 个，记录没有改写（已移回的下次执行时自动去掉），再执行一次 RESTORE=1 即可继续")
        raise SystemExit(130)
    left = sum(len(names) for names in skipped.values()) + unvisited
    print(f"已移回 {len(restored)} 个账户目录；未处理 {left} 个（仍在记录中）；已在原处、删除记录的 {dropped} 个")
    if record_error:
        print(f"  {record_error}，已停止：还有 {unvisited} 个没有处理（仍在记录中，目录没有移回）；腾出空间后再执行一次 RESTORE=1")
    print_failures(skipped)
    for parent, count in other_roots.most_common(5):
        print(f"  原账户根目录 {mask(parent)}：{count} 个；改回 AISTUDIO_AUTH_STATES，或确认后加 RESTORE_FORCE=1 移回")
    if stubbed:
        print(f"原处的缓存残骸（APPLY 时正在启动的 Worker 重建的 camoufox-cache）挪进隔离目录 {stubbed} 个（*.stub-*，可以删除）")
    if rewrite_error:
        print(f"改写记录失败：{rewrite_error}；记录保持原样，已移回的下次执行 RESTORE=1 时自动去掉")
    elif len(kept) != len(rows):
        print(f"记录已改写，改写前的备份：{mask(backup)}")
    recorded = {os.path.realpath(row["to"]) for _, row in kept if row}
    unrecorded, stubs = [], []
    for area in [quarantine] + [os.path.join(root, FALLBACK) for root in roots]:
        if not os.path.isdir(area):
            continue
        for name in sorted(os.listdir(area)):
            path = os.path.join(area, name)
            if name.startswith(".") or os.path.islink(path) or not os.path.isdir(path):
                continue
            if ".stub-" in name:
                stubs.append(name)
            elif os.path.realpath(path) not in recorded:
                unrecorded.append(path)
    if unrecorded:
        print(f"隔离目录里没有记录的目录 {len(unrecorded)} 个（例：{examples(unrecorded)}）：")
        print("  多半是旧版本脚本中途被打断留下的；auth-quarantine/ 下名为 <根序号>-<目录名>，.quarantine-disabled/ 下就是目录名，")
        print("  确认后手动移回，例如：mv auth-quarantine/0-<目录名> <账户根目录>/<目录名>")
    if stubs:
        print(f"隔离目录里的缓存残骸 {len(stubs)} 个（*.stub-*，不是账户，可以删除）")
    if restored:
        print("程序每 10 秒扫描一次，移回的账户约 20 秒内重新导入：没有手动停用记录的按默认启用，进入新账户自动处理，也会被预热选中。")
        print(f"移回时间记在 {mask(restored_log)}：之后列出与 APPLY 只认移回之后新的停用记录，隔离前的旧记录不再算数。")
    print("只在组织已重新开放 AI Studio、或程序已能识别 ServiceNotAllowed 之后再恢复，否则这些账户会再次每个占住一个启动名额约 2 分钟。")
    raise SystemExit(1 if record_error else 0)

# ---------- 账户目录：分别按账户 ID（目录名）与账户标签（account.json 的 label）索引 ----------
accounts, seen = [], set()
by_id, by_label = collections.defaultdict(list), collections.defaultdict(list)
per_root = collections.Counter()


def add_account(directory, root_index):
    if directory in seen:
        return
    seen.add(directory)
    item = {"path": directory, "id": os.path.basename(directory), "label": read_label(directory), "root": root_index}
    accounts.append(item)
    # 账户 ID 与程序一样区分大小写（程序按目录名原文认账户）；不同根下同名的目录一起算：程序只载入其中一个，另一个还在时移不出账户池
    by_id[item["id"]].append(item)
    if item["label"]:
        by_label[item["label"].lower()].append(item)
    if root_index is not None:
        per_root[root_index] += 1


for index, root in enumerate(roots):
    try:
        names = sorted(os.listdir(root))
    except OSError as err:
        print(f"  读取 {mask(root)} 失败：{reason(err)}")
        continue
    for name in names:
        directory = os.path.join(root, name)
        if name.startswith(".") or os.path.islink(directory) or not os.path.isdir(directory) or not is_account(directory):
            continue
        add_account(directory, index)
for directory in singles:
    add_account(directory, None)
single_items = [item for item in accounts if item["root"] is None]
movable = len(accounts) - len(single_items)
for index, root in enumerate(roots):
    print(f"  [{index}] {mask(root)}  账户目录 {per_root[index]} 个")
if single_items:
    print(f"  直接列出的单个账户目录 {len(single_items)} 个：第 {'、'.join(str(position[item['path']]) for item in single_items)} 项")
for path in problems:
    print(f"  不存在或不是目录（已忽略）：{mask(path)}")
if not accounts:
    print("这些账户根目录下没有账户目录（含 storage-state.json 或 account.json 的子目录），已放弃；请核对 AISTUDIO_AUTH_STATES")
    raise SystemExit(1)

# 已隔离的目录：日志里对应到它们的依据单独计数，不算“找不到”
quarantined_ids, quarantined_labels, quarantined = {}, {}, 0
for _, row in read_manifest() or []:
    if row and recorded_in_quarantine(row) and os.path.isdir(row["to"]):
        quarantined += 1
        quarantined_ids[os.path.basename(row["from"])] = row["to"]
        label = read_label(row["to"])
        if label:
            quarantined_labels[label.lower()] = row["to"]
print(f"隔离目录：{mask(quarantine)}" + (f"（已隔离 {quarantined} 个）" if quarantined else ""))

# ---------- 日志 ----------
now = datetime.datetime.now(datetime.timezone.utc)
since = now - datetime.timedelta(minutes=minutes)
evidence, ready, ambiguous = {}, {}, {}
# fresh：目录最近一次 RESTORE 移回或被程序重新导入的时间，早于它的停用依据属于移走前，不算；reset：因此不算的目录
fresh, reset = read_restored(), set()
unmatched = {"quarantined": set(), "unknown": set(), "nonaccount": 0}
READY_BYTES = "WAA Worker 就绪".encode()
IMPORT_HEAD = "自动导入账户"
IMPORT_BYTES = (IMPORT_HEAD + " | ").encode()


def line_time(raw):
    try:
        event = json.loads(raw)
    except ValueError:
        return None
    return parse_time(str(event.get("time", ""))) if isinstance(event, dict) else None


def tail_time(path, size):
    with open(path, "rb") as handle:
        handle.seek(max(0, size - (1 << 20)))
        for raw in reversed(handle.read().split(b"\n")):
            moment = line_time(raw) if raw.strip() else None
            if moment:
                return moment
    return None


def classify(head, msg, source):
    """返回 (类别, 标识, 标识种类 label 或 id)；不是停用依据时返回 None"""
    if "AI Studio app is disabled" in msg:
        m = CATALOG_RE.search(msg)
        if m:
            return "catalog", m.group(1), "id"
        if head == "模型目录同步失败":
            return "catalog", source, "id"
    if "ServiceNotAllowed" not in msg or "Drive 授权" in msg:
        return None
    urls = NOT_ALLOWED_RE.findall(msg)
    aistudio = any(AISTUDIO_APPLICATION in url or AISTUDIO_CONTINUE_RE.search(url) for url in urls)
    other = not aistudio and any("application=" in url or "continue=" in url for url in urls)
    if head in WORKER_HEADS:
        # Worker 只打开 AI Studio：快照里只有路径、没有参数的也算，参数指向别的服务的不算
        if urls and not other:
            return WORKER_HEADS[head], source, ("label" if head == "WAA Worker 启动失败" else "id")
        return None
    if head in VERIFY_HEADS and aistudio:
        return "verify", source, "label"
    return None


def note(item, kind, moment):
    if item["path"] in fresh and moment <= fresh[item["path"]]:
        reset.add(item["path"])
        return
    entry = evidence.setdefault(item["path"], {"item": item, "start": 0, "expand": 0, "catalog": 0, "verify": 0, "last": moment})
    entry[kind] += 1
    entry["last"] = max(entry["last"], moment)


def handle_event(raw):
    try:
        event = json.loads(raw)
    except ValueError:
        return
    if not isinstance(event, dict):
        return
    moment = parse_time(str(event.get("time", "")))
    if not moment or moment < since:
        return
    msg = str(event.get("msg", ""))
    source = str(event.get("source", "")).strip()
    head = msg.split("\n", 1)[0].split(" | ", 1)[0].strip()
    if head == "WAA Worker 就绪":
        # 就绪按标签记：换算到这个标签下的所有目录（宁可少移）
        for item in by_label.get(source.lower(), ()):
            ready[item["path"]] = max(ready.get(item["path"], moment), moment)
        return
    if head == IMPORT_HEAD:
        # 程序把一个新出现的目录导入账户池（RESTORE 或手动移回、重新上传）：之前的依据属于移走前的目录，按标签全部作废（宁可少移）；
        # 之后再失败于停用页会重新算上。日志按时间顺序，之后读到的依据照常计
        label = ""
        for part in msg.split("\n", 1)[0].split(" | ")[1:]:
            if part.startswith("账户="):
                label = part[len("账户="):].strip().lower()
        for item in by_label.get(label, ()) if label else ():
            fresh[item["path"]] = max(fresh.get(item["path"], moment), moment)
            if evidence.pop(item["path"], None):
                reset.add(item["path"])
        if label and ambiguous.pop(label, None):
            reset.update(item["path"] for item in by_label.get(label, ()))
        return
    found_kind = classify(head, msg, source)
    if not found_kind:
        return
    kind, ident, scheme = found_kind
    # 标签（邮箱）不区分大小写；账户 ID 是目录名原文，与程序一样精确匹配
    key = ident.strip().lower() if scheme == "label" else ident.strip()
    items = (by_label if scheme == "label" else by_id).get(key, [])
    if not items:
        if key.lower() in NON_ACCOUNT_SOURCES or (scheme == "label" and "@" not in key):
            unmatched["nonaccount"] += 1
        elif key in (quarantined_labels if scheme == "label" else quarantined_ids):
            unmatched["quarantined"].add((quarantined_labels if scheme == "label" else quarantined_ids)[key])
        else:
            unmatched["unknown"].add(key)
        return
    if scheme == "label" and len(items) > 1 and not include_ambiguous:
        entry = ambiguous.setdefault(key, {"items": items, "last": moment})
        entry["last"] = max(entry["last"], moment)
        return
    for item in items:
        note(item, kind, moment)


log = os.path.join(app, "logs", "aistudio2api.log")
files = []
for path in (log + ".1", log):
    if not os.path.isfile(path):
        continue
    size = os.path.getsize(path)
    start = max(0, size - (512 << 20))
    lines, first = 0, None
    with open(path, "rb") as handle:
        handle.seek(start)
        if start:
            handle.readline()
        for raw in handle:
            lines += 1
            if first is None:
                first = line_time(raw)
            if b"ServiceNotAllowed" in raw or b"app is disabled" in raw or READY_BYTES in raw or IMPORT_BYTES in raw:
                handle_event(raw)
    files.append((os.path.basename(path), lines, first, tail_time(path, size), start > 0))

found, singles_found, recovered = [], [], 0
for path, entry in evidence.items():
    if path in ready and ready[path] > entry["last"]:
        recovered += 1
        continue
    (singles_found if entry["item"]["root"] is None else found).append(entry)
found.sort(key=lambda entry: entry["item"]["path"])
found_paths = {entry["item"]["path"] for entry in found}
open_ambiguous = []
for key, entry in sorted(ambiguous.items()):
    rest = []
    for item in entry["items"]:
        if item["path"] in fresh and fresh[item["path"]] >= entry["last"]:
            reset.add(item["path"])
        elif item["path"] not in found_paths and not (item["path"] in ready and ready[item["path"]] > entry["last"]):
            rest.append(item)
    if rest:
        open_ambiguous.append((key, rest))
pending_paths = set(evidence) | {item["path"] for _, rest in open_ambiguous for item in rest}
reset_only = [path for path in reset if path not in pending_paths]

print(f"统计窗口：最近 {minutes} 分钟（{clock(since)} 起）")
if not files:
    print("  没有找到运行日志 logs/aistudio2api.log 或 .1")
for name, lines, first, last, partial in files:
    print(f"  读取 logs/{name}：{lines} 行，{clock(first)} ~ {clock(last)}" + ("（只读了最后 512MB）" if partial else ""))
firsts = [first for _, _, first, _, _ in files if first]
if firsts and min(firsts) > since:
    gap = int((min(firsts) - since).total_seconds() // 60)
    print(f"  注意：日志只覆盖到 {clock(min(firsts))}，比统计窗口起点晚 {gap} 分钟（start.sh 把超过上限的日志复制成 .1 再截断，.1 每次被覆盖），"
          "更早的停用记录已经看不到")

print(f"判定为组织停用的账户目录 {len(evidence)} 个，其中之后又出现 “WAA Worker 就绪” 的 {recovered} 个（不算）")
if reset_only:
    print(f"RESTORE 移回或程序重新导入（“自动导入账户”）之后还没有新的停用记录的 {len(reset_only)} 个（不算；之后再失败于停用页会重新算上）")
busy = 0
for entry in found:
    try:
        fd = os.open(os.path.join(entry["item"]["path"], "camoufox-cache", ".lock"), os.O_RDONLY)
    except OSError:
        continue
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        busy += 1
    finally:
        os.close(fd)
print(f"要隔离的 {len(found)} 个" + (f"（其中 {busy} 个正在被浏览器使用，APPLY 时会跳过）" if busy else ""))
if busy and not apply:
    print("  想一次全部移走：./start.sh stop，再 APPLY=1 bash tools/quarantine_disabled.sh，再 ./start.sh start"
          "（stop 后守护不会拉起、浏览器都已退出，没有缓存锁被占用；进行中的请求会中断）")


def domain_of(item):
    for value in (item["label"], item["id"]):
        if "@" in value:
            return value.rsplit("@", 1)[1].lower()
    return "（不是邮箱）"


if found:
    totals = collections.Counter(domain_of(item) for item in accounts if item["root"] is not None)
    domains = collections.Counter(domain_of(entry["item"]) for entry in found)
    ranked = domains.most_common()
    others = ranked[15:]
    print("按域名（判定数/该域名账户目录数）：" + "，".join(f"{domain} {count}/{totals[domain]}" for domain, count in ranked[:15])
          + (f"……另有 {len(others)} 个域名共 {sum(count for _, count in others)} 个" if others else ""))
    personal = [domain for domain in domains if domain in ("gmail.com", "googlemail.com")]
    if personal:
        print(f"  警告：名单里有个人账户（{'、'.join(personal)}），个人账户不会被组织停用，依据可疑，请先核对日志")
    kinds = collections.Counter()
    for entry in found:
        for kind in ("start", "expand", "catalog", "verify"):
            if entry[kind]:
                kinds[kind] += 1
    print(f"依据：Worker 启动失败于停用页 {kinds['start']} 个，后台扩容失败于停用页 {kinds['expand']} 个，"
          f"模型目录返回 AI Studio app is disabled {kinds['catalog']} 个，验证、登录或新账户自动处理时 {kinds['verify']} 个")
    limit = len(found) if show_all else 10
    for entry in found[:limit]:
        item = entry["item"]
        where = "" if item["id"].lower() == item["label"].lower() else f"  目录 {mask(item['id'])}"
        if len(roots) > 1:
            where += f"  根[{item['root']}]"
        print(f"  {mask(item['label'] or item['id'])}{where}  启动失败 {entry['start']} 次 / 扩容失败 {entry['expand']} 次 / "
              f"模型目录 {entry['catalog']} 次 / 验证 {entry['verify']} 次  最近 {clock(entry['last'])}")
    if len(found) > limit:
        print(f"  …… 另有 {len(found) - limit} 个（SHOW_ALL=1 列出全部）")
    if len(found) == movable and not single_items:
        print("  警告：要隔离的是全部账户目录。程序一轮扫描一个账户目录都没有时视为异常、不移出任何账户，APPLY 后需要 ./start.sh restart 才生效")

if unmatched["quarantined"]:
    print(f"日志里另有 {len(unmatched['quarantined'])} 个账户已在隔离目录（之前已处理，不再移动）")
if singles_found or open_ambiguous or unmatched["unknown"] or unmatched["nonaccount"]:
    print("未处理：")
if singles_found:
    print(f"  直接列在 AISTUDIO_AUTH_STATES 里的单个账户目录 {len(singles_found)} 个：脚本不移动，请从 .env 里去掉这些路径后 ./start.sh restart")
    for entry in singles_found:
        print(f"    第 {position[entry['item']['path']]} 项：{mask(entry['item']['path'])}")
if open_ambiguous:
    count = sum(len(rest) for _, rest in open_ambiguous)
    print(f"  标签对应多个账户目录、分不清是哪一个的 {len(open_ambiguous)} 个标签（{count} 个目录），没有算进去：")
    for key, rest in open_ambiguous[:5]:
        print(f"    {mask(key)}：目录 {examples((item['id'] for item in rest), 5)}")
    print("    确认这些目录是同一邮箱的副本（组织策略相同）后，可加 INCLUDE_AMBIGUOUS=1 一并算上")
if unmatched["nonaccount"]:
    print(f"  来源不是账户的 {unmatched['nonaccount']} 行（auth、service 等，例如添加账户时），对应不到账户目录")
if unmatched["unknown"]:
    print(f"  在账户目录里找不到的 {len(unmatched['unknown'])} 个（已被删除或改名；例：{examples(sorted(unmatched['unknown']))}）")

if not apply:
    print()
    if not found:
        print("没有新的要隔离的账户目录；以上只是列出，没有改任何文件")
        if quarantined:
            print("以后要恢复已隔离的：RESTORE=1 bash tools/quarantine_disabled.sh（RESTORE_MATCH=<域名或邮箱片段> 只恢复一部分）")
        raise SystemExit
    print("以上只是列出，没有改任何文件。确认后执行：APPLY=1 bash tools/quarantine_disabled.sh")
    print(f"会把这些账户目录移到 {mask(quarantine)}/（不删除；账户根目录在另一文件系统时移到 <账户根目录>/{FALLBACK}/）；"
          "以后要恢复执行：RESTORE=1 bash tools/quarantine_disabled.sh（RESTORE_MATCH=<域名或邮箱片段> 只恢复一部分）")
    print("只在组织已重新开放 AI Studio、或程序已能识别 ServiceNotAllowed 之后再恢复，否则这些账户会再次占满启动名额。")
    raise SystemExit

# ---------- APPLY：持有缓存锁，先写记录（落盘）再移动 ----------
if not found:
    print()
    print("没有新的要隔离的账户目录")
    raise SystemExit
instance = lock_instance()
signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGHUP, stop)
quarantine_device = os.stat(quarantine).st_dev
destination = {}
for root in roots:
    try:
        same = os.stat(root).st_dev == quarantine_device
    except OSError:
        same = False
    destination[root] = quarantine if same else os.path.join(root, FALLBACK)


def lock_cache(directory):
    """取得账户缓存锁（Worker 用的同一个 camoufox-cache/.lock），没有时先建出来；被占用时返回 None"""
    cache = os.path.join(directory, "camoufox-cache")
    owner = os.stat(directory)
    created = []
    if not os.path.lexists(cache):
        os.mkdir(cache, 0o700)
        created.append(cache)
    lock_path = os.path.join(cache, ".lock")
    if not os.path.lexists(lock_path):
        created.append(lock_path)
    fd = os.open(lock_path, os.O_RDONLY | os.O_CREAT, 0o600)
    for path in created:
        try:
            os.chown(path, owner.st_uid, owner.st_gid)
        except OSError:
            pass
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as err:
        os.close(fd)
        if err.errno in (errno.EWOULDBLOCK, errno.EAGAIN, errno.EACCES):
            return None
        raise
    return fd


def unique_target(base, name):
    target, serial = os.path.join(base, name), 0
    while os.path.lexists(target):
        serial += 1
        target = os.path.join(base, f"{name}.{STAMP}-{serial}")
    return target


moved, failures, busy, fallback_used = [], collections.defaultdict(list), 0, set()


def move_one(item, record):
    """返回 (失败原因, 是否写记录失败)；写记录失败时这一个没有移动，记录已截回"""
    directory, root = item["path"], roots[item["root"]]
    for attempt in (0, 1):
        base = destination[root]
        try:
            os.makedirs(base, 0o700, exist_ok=True)
        except OSError as err:
            return "建隔离目录失败：" + reason(err), False
        target = unique_target(base, item["id"] if base != quarantine else f"{item['root']}-{item['id']}")
        line = (json.dumps({"from": directory, "to": target, "time": int(time.time())}, ensure_ascii=False) + "\n").encode("utf-8")
        with Shielded():
            offset, error = append_record(record, line)
            if error:
                return "写记录失败：" + error, True
            try:
                os.rename(directory, target)
            except OSError as err:
                # 截不回时留下一条没移成的记录也无妨：RESTORE 见隔离目录里没有、原处还在，直接去掉这条
                truncate_record(record, offset)
                if err.errno == errno.EXDEV and base == quarantine and attempt == 0:
                    # st_dev 相同但跨挂载点（如 bind mount）时改名仍会失败：这个根改用根目录下的隔离目录
                    destination[root] = os.path.join(root, FALLBACK)
                    continue
                return "移动失败：" + reason(err), False
            moved.append((directory, target))
            if base != quarantine:
                fallback_used.add(root)
            return "", False
    return "移动失败", False


try:
    record, repaired = open_record(manifest)
except OSError as err:
    print()
    print(f"打开或修复记录文件 {mask(manifest)} 失败：{reason(err)}，没有移动任何目录（磁盘写满时腾出空间后再执行）")
    raise SystemExit(1)
if repaired:
    print(repaired)
running = program_running()
try:
    log_offset = os.path.getsize(log)
except OSError:
    log_offset = 0
interrupted, record_error, unvisited = False, "", 0
try:
    for number, entry in enumerate(found, 1):
        item = entry["item"]
        if os.path.islink(item["path"]) or not os.path.isdir(item["path"]):
            failures["已不在原处"].append(item["id"])
            continue
        try:
            fd = lock_cache(item["path"])
        except OSError as err:
            failures["锁定 camoufox-cache/.lock 失败：" + reason(err)].append(item["id"])
            continue
        if fd is None:
            busy += 1
            continue
        try:
            problem, fatal = move_one(item, record)
        finally:
            os.close(fd)
        if fatal:
            record_error, unvisited = problem, len(found) - number + 1
            break
        if problem:
            failures[problem].append(item["id"])
except KeyboardInterrupt:
    interrupted = True
finally:
    os.close(record)

print()
if interrupted:
    print(f"已中断：已移走 {len(moved)} 个，每个都有记录；再执行一次 APPLY=1 继续，或 RESTORE=1 移回")
    raise SystemExit(130)
failed = sum(len(names) for names in failures.values())
print(f"已移走 {len(moved)} 个账户目录；正在被浏览器使用、本次跳过的 {busy} 个；失败 {failed} 个")
if record_error:
    print(f"  {record_error}，已停止：还有 {unvisited} 个没有处理（目录都没有移动，记录里没有它们）；腾出空间后再执行一次 APPLY=1")
print_failures(failures)
for base, count in collections.Counter(os.path.dirname(target) for _, target in moved).most_common():
    print(f"  移到 {mask(base)}/：{count} 个")
for root in sorted(fallback_used):
    print(f"  账户根目录 {mask(root)} 与 {mask(quarantine)} 不在同一文件系统，改移到它下面的 {FALLBACK}/（点开头，程序与 start.sh 都不扫描）")
if moved and running is not False:
    # 移动前已经读完认证文件、还没拿到缓存锁的 Worker 会在原处重建 camoufox-cache：几秒后看原路径有没有重新出现
    time.sleep(5)
    stubs = [os.path.basename(source) for source, _ in moved if os.path.lexists(source) and not is_account(source)]
    again = [os.path.basename(source) for source, _ in moved if os.path.lexists(source) and is_account(source)]
    if stubs:
        print(f"原处又出现了只有缓存的目录 {len(stubs)} 个（例：{examples(stubs)}）：移动时正在启动的 Worker 重建的 camoufox-cache，"
              "不是账户，程序不会导入；RESTORE 时会自动挪开")
    if again:
        print(f"原处又出现了带账户文件的目录 {len(again)} 个（例：{examples(again)}）：程序回写了认证文件或有人放回，这些账户仍在账户池里，请核对")
threshold = max(20, int(len(accounts) * 0.1))
if running is False:
    if moved:
        print("程序没有在运行：./start.sh start 后不会载入这些账户")
    if busy:
        print(f"跳过的 {busy} 个的缓存锁被别的进程占着（程序没有在运行，多半是残留的 camoufox 进程，./start.sh stop 会清掉），之后再执行一次 APPLY=1")
else:
    if moved:
        print("程序每 10 秒扫描一次账户目录，没有在启动的账户一般 10~20 秒移出账户池"
              + (f"；这次移走 {len(moved)} 个，超过 {threshold} 个，要连续缺失约 1 分钟才移出，期间运行日志的 WARN “账户目录大量缺失” 是正常的"
                 if len(moved) > threshold else "")
              + "。")
        print("  启动名额占满时，目录扫描会停在某个被移走、正在排队等名额的账户上，等它这次启动结束（排队时间加最长约 INIT_TIMEOUT）才继续，"
              "这期间被移走的账户都还在账户池里，也不导入新目录。")
        print("  运行日志的 WARN “移出已删除目录的账户失败，下轮重试 | …正在使用” 是有请求正在使用这个账户，下一轮会再试，不是失败。")
        print(f"确认：tail -c +{log_offset + 1} logs/aistudio2api.log | grep -c '账户目录已删除，已移出账户池'"
              "（只数这次 APPLY 之后写的；期间日志被轮转成 .1 时，改为执行前后各数一次整份日志看增加了多少）")
    if busy:
        print(f"跳过的 {busy} 个还在账户目录里，多半正在启动、占着启动名额。最省事的顺序：./start.sh stop，再执行一次 APPLY=1 bash tools/quarantine_disabled.sh，"
              "再 ./start.sh start（stop 后守护不会拉起、浏览器都已退出，没有缓存锁被占用，可以一次全部移走，start 时不再载入这些账户；进行中的请求会中断）。")
        print("  不要直接 ./start.sh restart：跳过的这些重启后又会被预热，再各占一个启动名额约 2 分钟；不想停服务就过几分钟再执行一次 APPLY=1")
    elif moved and not record_error:
        print("想立即生效可以 ./start.sh restart（这次没有跳过的账户，重启后不再载入它们；现有就绪 Worker 都会重启）")
if moved and len(moved) == movable and not single_items:
    print("警告：全部账户目录都被移走了" + ("，程序一个目录都扫不到时视为异常、不移出任何账户，需要 ./start.sh restart" if running is not False else "，启动后账户池为空"))
print("要恢复：RESTORE=1 bash tools/quarantine_disabled.sh（只在组织已重新开放 AI Studio、或程序已能识别 ServiceNotAllowed 之后）")
raise SystemExit(1 if record_error else 0)
PY
