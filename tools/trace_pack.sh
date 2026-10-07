#!/usr/bin/env bash
# 打包 /trace/ 排查路由记录的请求详情与重复回复统计，账户邮箱替换为固定代号，密钥、令牌与 Cookie 隐藏。
# 注意：排查记录里有完整的提示词与回复原文，发给别人前请确认内容可以分享。
# 用法：./start.sh trace
APP_DIR="${APP_DIR:-$(cd "$(dirname "$0")/.." 2>/dev/null && [ -f start.sh ] && pwd || echo /www/wwwroot/Chat2API/build)}"
PORT="$(grep -E '^LISTEN_ADDR=' "$APP_DIR/.env" 2>/dev/null | tail -n 1 | sed 's/.*://; s/["[:space:]]//g')"
API="http://127.0.0.1:${PORT:-2048}"
# 控制面 /api/ 必须携带管理令牌（程序目录下的 .admin-token）
ADMIN_TOKEN="$(tr -cd '0-9a-fA-F' < "$APP_DIR/.admin-token" 2>/dev/null)"
TRACE_DIR="$APP_DIR/logs/trace"
LOG="$APP_DIR/logs/aistudio2api.log"
STAMP="$(date +%Y%m%d-%H%M%S)"
NAME="aistudio-trace-$STAMP"
OUT_DIR="$APP_DIR/reports"
WORK="$(mktemp -d)"
BUNDLE="$WORK/$NAME"
trap 'rm -rf "$WORK"' EXIT
command -v python3 >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }
mkdir -p "$BUNDLE" "$OUT_DIR"

count=0
if [ -d "$TRACE_DIR" ]; then
  count="$(find "$TRACE_DIR" -maxdepth 1 -name '*.json' | wc -l)"
  [ "$count" -gt 0 ] && cp "$TRACE_DIR"/*.json "$BUNDLE"/
fi
echo "排查记录：$count 个（$TRACE_DIR，程序只保留最近 100 个）"
curl -s -m 30 -H "X-Admin-Token: $ADMIN_TOKEN" "$API/api/debug/duplicates" > "$BUNDLE/duplicates.json" 2>/dev/null
if [ -f "$LOG" ]; then
  cat "$LOG.1" "$LOG" 2>/dev/null | grep -F "重复回复" | tail -n 2000 > "$BUNDLE/duplicate-log.jsonl"
fi
dups="$(grep -c . "$BUNDLE/duplicate-log.jsonl" 2>/dev/null || echo 0)"
echo "日志中的重复回复记录：$dups 条"

# 每条记录的自动结论汇总到 summary.txt，并在这里列出发现异常的记录
python3 - "$BUNDLE" <<'PY'
import json, os, sys
root = sys.argv[1]
rows = []
for name in sorted(os.listdir(root), reverse=True):
    if not name.endswith(".json") or name == "duplicates.json":
        continue
    try:
        with open(os.path.join(root, name), encoding="utf-8") as source:
            data = json.load(source)
    except Exception:
        continue
    if not isinstance(data, dict) or "started_at" not in data:
        continue
    rows.append((name, data.get("summary") or "（旧版记录，没有自动结论）", data.get("flags") or [], data.get("findings") or []))
with open(os.path.join(root, "summary.txt"), "w", encoding="utf-8") as out:
    for name, summary, flags, findings in rows:
        out.write(f"{name}\n  {summary}\n")
        for finding in findings:
            out.write(f"  [{finding.get('level')}] {finding.get('title')}：{finding.get('detail')}\n")
        out.write("\n")
flagged = [row for row in rows if row[2]]
print(f"自动结论：{len(rows)} 条记录中 {len(flagged)} 条发现异常（详见包内 summary.txt）")
for name, summary, flags, _ in flagged[:15]:
    print(f"  {name[:19]}  {'、'.join(flags)}")
PY

python3 - "$BUNDLE" <<'PY'
import hashlib, os, re, sys
root = sys.argv[1]
email = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+")
rules = [
    (re.compile(r"(\b[a-z][a-z0-9+.-]*://)[^\s/@:\"']+:[^\s/@\"']+@"), r"\1<账号>:<密码>@"),
    (re.compile(r"(?i)([?&](?:key|token|access_token|api_key|admin_token)=)[^&\s\"'<]+"), r"\1<已隐藏>"),
    # 管理令牌（启动日志里的登录地址、X-Admin-Token 请求头）
    (re.compile(r"(?i)(admin_token=|x-admin-token:\s*)[0-9a-f]{16,}"), r"\1<已隐藏>"),
    (re.compile(r"AIza[0-9A-Za-z_\-]{30,}"), "<API Key 已隐藏>"),
    (re.compile(r"(?i)(bearer\s+)[A-Za-z0-9._\-]{16,}"), r"\1<令牌已隐藏>"),
    (re.compile(r"(?i)((?:SAPISID|APISID|SSID|HSID|SID|__Secure-[A-Za-z0-9-]+|NID)=)[^;\s\"']+"), r"\1<已隐藏>"),
]
def alias(match):
    return "acct-" + hashlib.sha256(match.group(0).lower().encode()).hexdigest()[:8]
for name in os.listdir(root):
    path = os.path.join(root, name)
    try:
        with open(path, encoding="utf-8", errors="replace") as source:
            text = source.read()
    except OSError:
        continue
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
echo "   账户邮箱已替换为固定代号，密钥、令牌与 Cookie 已隐藏；提示词与回复原文保留。"
