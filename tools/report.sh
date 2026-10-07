#!/usr/bin/env bash
# AIStudio2API 一键打包排查资料：诊断结果、最近日志、服务状态与程序快照、系统信息、配置（已隐藏密钥）。
# 账户邮箱替换为固定代号（同一邮箱在所有文件中代号相同），密钥、口令、代理账号密码与令牌一律隐藏。
# 用法：./start.sh report                 统计最近 30 分钟
#       MINUTES=120 ./start.sh report     统计最近 120 分钟
APP_DIR="${APP_DIR:-$(cd "$(dirname "$0")/.." 2>/dev/null && [ -f start.sh ] && pwd || echo /www/wwwroot/Chat2API/build)}"
MINUTES="${MINUTES:-30}"
LOG="$APP_DIR/logs/aistudio2api.log"
PORT="$(grep -E '^LISTEN_ADDR=' "$APP_DIR/.env" 2>/dev/null | tail -n 1 | sed 's/.*://; s/["[:space:]]//g')"
API="http://127.0.0.1:${PORT:-2048}"
# 控制面 /api/ 必须携带管理令牌（程序目录下的 .admin-token）
ADMIN_TOKEN="$(tr -cd '0-9a-fA-F' < "$APP_DIR/.admin-token" 2>/dev/null)"
STAMP="$(date +%Y%m%d-%H%M%S)"
NAME="aistudio-report-$STAMP"
OUT_DIR="$APP_DIR/reports"
WORK="$(mktemp -d)"
BUNDLE="$WORK/$NAME"
trap 'rm -rf "$WORK"' EXIT
command -v python3 >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }
mkdir -p "$BUNDLE" "$OUT_DIR"

echo "正在收集排查资料（最近 $MINUTES 分钟）..."

echo "  1/6 运行诊断"
MINUTES="$MINUTES" APP_DIR="$APP_DIR" bash "$APP_DIR/tools/diag_slow.sh" > "$BUNDLE/diag.txt" 2>&1

echo "  2/6 读取服务状态与程序快照"
curl -s -m 30 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/status" > "$BUNDLE/status.json" 2>/dev/null
curl -s -m 30 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/cooldowns" > "$BUNDLE/cooldowns.json" 2>/dev/null
curl -s -m 30 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/onboarding" > "$BUNDLE/onboarding.json" 2>/dev/null
curl -s -m 30 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/debug/goroutines" > "$BUNDLE/goroutines.txt" 2>/dev/null
curl -s -m 30 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/debug/duplicates" > "$BUNDLE/duplicates.json" 2>/dev/null
curl -s -m 30 --noproxy '*' -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/debug/perf" > "$BUNDLE/perf.json" 2>/dev/null

echo "  3/6 收集系统信息"
{
  echo "== 时间"; date -R
  echo "== 系统"; uname -a; cat /etc/os-release 2>/dev/null | head -n 3
  echo "== CPU"; nproc; cat /proc/loadavg
  echo "== 内存"; free -m
  echo "== 磁盘"; df -h "$APP_DIR"
  echo "== 上限"; sysctl kernel.pid_max kernel.threads-max 2>/dev/null
  echo "系统线程数：$(ps -eLf --no-headers 2>/dev/null | wc -l)"
  app_pid="$(cat "$APP_DIR/run/aistudio2api.pid" 2>/dev/null)"
  if [ -n "$app_pid" ] && [ -d "/proc/$app_pid" ]; then
    echo "主程序 PID：$app_pid，打开的文件：$(ls "/proc/$app_pid/fd" 2>/dev/null | wc -l)"
    grep -E "Max open files|Max processes" "/proc/$app_pid/limits" 2>/dev/null
  fi
  echo "== 占用最高的进程"; ps -eo pcpu,rss,etimes,comm --sort=-pcpu | head -n 20
  echo "== 浏览器进程数"; ps -eo args | grep -c "[c]amoufox-bin"
  echo "== 内存不足记录"; dmesg -T 2>/dev/null | grep -iE "out of memory|oom-kill|killed process" | tail -n 20
} > "$BUNDLE/system.txt" 2>&1

echo "  4/6 读取配置与版本（隐藏密钥）"
if [ -f "$APP_DIR/.env" ]; then
  # 与程序的 .env 解析一致：键名前后允许空白与 export 前缀
  sed -E 's/^([[:space:]]*(export[[:space:]]+)?(PROXY_API_KEY|ADMIN_PASSWORD)[[:space:]]*=).*/\1<已隐藏>/' "$APP_DIR/.env" > "$BUNDLE/config.txt"
fi
{
  ls -l --time-style=full-iso "$APP_DIR/aistudio2api" 2>/dev/null
  sha256sum "$APP_DIR/aistudio2api" 2>/dev/null
  go version 2>/dev/null
  ls -t "$APP_DIR"/AIStudio2API-main-server*.zip /root/AIStudio2API-main-server*.zip 2>/dev/null | head -n 1
} > "$BUNDLE/version.txt" 2>&1

echo "  5/6 截取最近日志"
if [ -f "$LOG" ]; then
  cat "$LOG.1" "$LOG" 2>/dev/null | tail -n 400000 | MINUTES="$MINUTES" python3 -c '
import datetime, json, os, re, sys
minutes = int(os.environ.get("MINUTES", "30"))
pattern = re.compile(r"^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$")
def parse(value):
    match = pattern.match(value or "")
    if not match:
        return None
    fraction = (match.group(2) or "0")[:6].ljust(6, "0")
    zone = "+00:00" if match.group(3) == "Z" else match.group(3)
    return datetime.datetime.fromisoformat(f"{match.group(1)}.{fraction}{zone}")
entries, other = [], []
for line in sys.stdin:
    line = line.rstrip("\n")
    if line.startswith("{"):
        try:
            moment = parse(json.loads(line).get("time"))
        except ValueError:
            moment = None
        if moment is not None:
            entries.append((moment, line))
            continue
    other.append(line)
latest = max((moment for moment, _ in entries), default=None)
with open(sys.argv[1], "w") as output:
    for moment, line in entries:
        if latest is None or moment >= latest - datetime.timedelta(minutes=minutes):
            output.write(line + "\n")
# 非 JSON 行多为程序崩溃（panic）堆栈或启动脚本输出，保留最后 500 行
with open(sys.argv[2], "w") as output:
    output.write("\n".join(other[-500:]) + "\n")
' "$BUNDLE/logs.jsonl" "$BUNDLE/crash.txt"
else
  echo "找不到日志文件：$LOG" > "$BUNDLE/crash.txt"
fi

echo "  6/6 脱敏并打包"
python3 - "$BUNDLE" <<'PY'
import hashlib, os, re, sys
root = sys.argv[1]
# 每个包随机盐：同一包内代号一致，拿到包的人无法用猜测的邮箱比对代号
ALIAS_SALT = os.urandom(16)
# 邮箱（含 URL 编码的 %40 形式）
email = re.compile(r"[A-Za-z0-9._%+-]+(?:@|%40)[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+")
rules = [
    # 代理地址中的账号密码
    (re.compile(r"(\b[a-z][a-z0-9+.-]*://)[^\s/@:\"']+:[^\s/@\"']+@"), r"\1<账号>:<密码>@"),
    # 查询参数中的 key/token 先整体隐藏，再处理其他位置出现的谷歌 API Key 与 Bearer 令牌
    (re.compile(r"(?i)([?&](?:key|token|access_token|api_key|admin_token)=)[^&\s\"'<]+"), r"\1<已隐藏>"),
    # 管理令牌（启动日志里的登录地址、X-Admin-Token 请求头）
    (re.compile(r"(?i)(admin_token=|x-admin-token:\s*)[0-9a-f]{16,}"), r"\1<已隐藏>"),
    (re.compile(r"AIza[0-9A-Za-z_\-]{30,}"), "<API Key 已隐藏>"),
    (re.compile(r"(?i)(bearer\s+)[A-Za-z0-9._\-+/=]{8,}"), r"\1<令牌已隐藏>"),
    (re.compile(r"(?i)(basic\s+)[A-Za-z0-9+/=]{8,}"), r"\1<凭据已隐藏>"),
    # 各类 API Key 请求头与 JSON 字段（x-goog-api-key、x-api-key、api_key、proxy_api_key）
    (re.compile(r"(?i)((?:x-goog-api-key|x-api-key|proxy_api_key|api_key|apikey)[\"']?\s*[:=]\s*[\"']?)[^\"'\s,&<>]+"), r"\1<已隐藏>"),
    # Cookie 值（如 SAPISID、__Secure-*）
    (re.compile(r"(?i)((?:SAPISID|APISID|SSID|HSID|SIDCC|SID|__Secure-[A-Za-z0-9-]+|NID)=)[^;\s\"']+"), r"\1<已隐藏>"),
    # JSON 形式保存的 Cookie（storage-state 结构：{"name":"SAPISID","value":"..."}）
    (re.compile(r"(?i)(\"name\"\s*:\s*\"(?:SAPISID|APISID|SSID|HSID|SIDCC|SID|__Secure-[^\"]+|NID)\"\s*,\s*\"value\"\s*:\s*\")[^\"]*"), r"\1<已隐藏>"),
]
def alias(match):
    value = match.group(0).lower()
    return "acct-" + hashlib.sha256(ALIAS_SALT + value.encode()).hexdigest()[:8]
for name in os.listdir(root):
    path = os.path.join(root, name)
    try:
        with open(path, encoding="utf-8", errors="replace") as source:
            text = source.read()
    except OSError:
        continue
    # 先隐藏代理账号密码、查询参数、令牌与 Cookie，最后替换邮箱：
    # 否则代理地址里的"密码@主机"会先被当成邮箱替换，账号名反而漏出来
    for pattern, replacement in rules:
        text = pattern.sub(replacement, text)
    text = email.sub(alias, text)
    with open(path, "w", encoding="utf-8") as target:
        target.write(text)
PY
tar -czf "$OUT_DIR/$NAME.tar.gz" -C "$WORK" "$NAME"
size="$(du -h "$OUT_DIR/$NAME.tar.gz" | cut -f1)"
echo
echo "✅ 已生成：$OUT_DIR/$NAME.tar.gz（$size）"
echo "   用宝塔面板「文件」进入 $OUT_DIR 下载这个文件，上传到对话里即可。"
echo "   账户邮箱已替换为固定代号，密钥、口令、代理账号密码与令牌已隐藏。"
