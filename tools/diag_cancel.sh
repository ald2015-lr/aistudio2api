#!/usr/bin/env bash
# AIStudio2API 一键诊断"请求被取消 / 客户端连接中止"（客户端报 Software caused connection abort、Connection closed while receiving data，
# 本服务记 499 / cancelled）：最近被取消的请求是谁断开的（客户端 / 服务端）、断开在首事件前还是流中、断开前是否已提交响应头、
# 写响应头后 1 秒内就断开的有多少、已写出多少，以及断开前最后一次数据写出（中转会转发的 data，不含换行与 ": ping" 心跳）、
# 最后一次写出（含心跳）与上游最近一次事件距断开多久的分布。解读规则与 docs/logging.md“请求取消的断开现场”一致：
#   - 距最近数据几十秒、已用时扎堆在 60s、100s 附近：连接长时间没有中转会转发的数据后被中间层读超时（nginx 默认 60s，Cloudflare 100s）
#     或手机系统、VPN 掐断，打开 STREAM_DATA_HEARTBEAT 与 new-api 的保持连接心跳（Ping 间隔 10–15 秒）；距最近写出很短而距最近数据
#     很长，说明换行或 ": ping" 心跳在写、但被中转丢了
#   - 响应头已提交而距最近数据为"无"（关闭数据型心跳或官方 SDK 直连时只写换行，一块 data 都没写出）：经中转时从进入服务到断开，
#     中转后面的客户端一直零字节，按已用时代替距最近数据参与上面的判断
#   - 距最近数据在 10s 以内仍断开：心跳已写到下一跳，多半是客户端自己走了（转入后台、切网），或中转没把数据转给它的客户端
#   - 经 new-api 时"客户端断开"指 new-api 关闭了到本服务的连接；写响应头后 1 秒内断开（与阶段无关：首个事件在等待内就到时响应头随它
#     提交，阶段显示为流中），多为手机在本服务写响应头之前就断开了（new-api 发上游请求不绑定手机连接，收到响应头开始读流时才发现手机
#     已走），真实时刻看 new-api 日志的 stream ended … reason=client_gone
# 断开现场来自 request.finished 的 request.disconnect（程序版本早于断开诊断时没有，单独计数；早于数据写出诊断时没有
# since_last_data_write_ms，按距最近写出计并注明）。
# 只读：只读运行日志（logs/aistudio2api.log 与 .1）与 .env，不访问网络、不改任何配置；账号邮箱已打码
#
# 用法（放在程序目录的 tools/ 下运行，或用 APP_DIR 指定程序目录）：
#   bash tools/diag_cancel.sh
#   MINUTES=180 bash tools/diag_cancel.sh       统计最近 180 分钟（默认 1440 分钟 = 24 小时）
#   SAMPLES=20 bash tools/diag_cancel.sh        列出最近多少个被取消的请求（默认 10）
#   MODEL=gemini-3.8-flash bash tools/diag_cancel.sh   只看这个模型
#   LOG_FILE=… DIAG_NOW=2026-10-22T08:00:00Z bash tools/diag_cancel.sh   分析排查包里的运行日志副本，“现在”按 DIAG_NOW（RFC 3339）计
set -u
script_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd)"
if [ -z "${APP_DIR:-}" ]; then
  for candidate in "$script_dir/.." "$script_dir" "$PWD" /www/wwwroot/Chat2API/build; do
    if [ -f "$candidate/start.sh" ]; then APP_DIR="$(cd "$candidate" && pwd)"; break; fi
  done
fi
APP_DIR="${APP_DIR:-/www/wwwroot/Chat2API/build}"
MINUTES="${MINUTES:-1440}"
SAMPLES="${SAMPLES:-10}"
LOG="${LOG_FILE:-$APP_DIR/logs/aistudio2api.log}"
case "$MINUTES" in "" | *[!0-9]* | 0) echo "MINUTES 必须是正整数"; exit 1 ;; esac
case "$SAMPLES" in "" | *[!0-9]*) echo "SAMPLES 必须是 0 或正整数"; exit 1 ;; esac
command -v python3 >/dev/null || { echo "需要 python3：apt install -y python3"; exit 1; }
# .env 里的数据型心跳设置（没有写时为默认开启）
HEARTBEAT="$(grep -E '^STREAM_DATA_HEARTBEAT=' "$APP_DIR/.env" 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d '"[:space:]')"
export APP_DIR MINUTES SAMPLES LOG HEARTBEAT

python3 -I - <<'PY'
import collections
import datetime
import json
import os
import re

ENV = os.environ
UTC = datetime.timezone.utc
MINUTES = max(1, int(ENV.get("MINUTES") or 1440))
SAMPLES = max(0, int(ENV.get("SAMPLES") or 10))
MODEL = (ENV.get("MODEL") or "").strip().lower()
TIME_RE = re.compile(r'^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$')
EMAIL_RE = re.compile(r'([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@([A-Za-z0-9.-]+)')
# 静默时长的分档（秒）：60、100 附近为常见的中间层读超时
BUCKETS = [(0, 5), (5, 10), (10, 15), (15, 30), (30, 55), (55, 65), (65, 95), (95, 105), (105, 300), (300, None)]


def parse_time(value):
    match = TIME_RE.match((value or "").strip())
    if not match:
        return None
    fraction = (match.group(2) or "0")[:6].ljust(6, "0")
    zone = "+00:00" if match.group(3) == "Z" else match.group(3)
    try:
        return datetime.datetime.fromisoformat(f"{match.group(1)}.{fraction}{zone}")
    except ValueError:
        return None


def mask(text):
    return EMAIL_RE.sub(lambda m: m.group(1) + "***@" + m.group(2), str(text or ""))


def bucket_label(low, high):
    return f"≥{low}s" if high is None else f"{low}–{high}s"


def distribution(values):
    counts = collections.Counter()
    for value in values:
        for low, high in BUCKETS:
            if value >= low and (high is None or value < high):
                counts[bucket_label(low, high)] += 1
                break
    return "，".join(f"{bucket_label(low, high)} {counts[bucket_label(low, high)]}" for low, high in BUCKETS
                    if counts[bucket_label(low, high)]) or "无"


def local(moment):
    return moment.astimezone().strftime("%m-%d %H:%M:%S")


def number(disconnect, key):
    value = disconnect.get(key)
    return value if isinstance(value, (int, float)) and not isinstance(value, bool) else None


def right_after_headers(disconnect):
    # 写响应头后 1 秒内断开（与阶段无关）；新版记录直接带 right_after_headers，旧版按 since_headers_sent_ms 推算
    if "right_after_headers" in disconnect:
        return bool(disconnect.get("right_after_headers"))
    since = number(disconnect, "since_headers_sent_ms")
    return disconnect.get("initiator") == "client" and since is not None and since < 1000


def quiet_seconds(disconnect):
    # 断开前中转后面的客户端多久没收到数据（秒）：有距最近数据时按它；响应头已提交、却一块 data 都没写出的新版记录
    # （有 since_headers_sent_ms，没有 since_last_data_write_ms）从进入服务到断开一直零字节，按已用时；其余返回 None
    since = number(disconnect, "since_last_data_write_ms")
    if since is not None:
        return since / 1000
    if disconnect.get("headers_sent") and number(disconnect, "since_headers_sent_ms") is not None:
        return (number(disconnect, "elapsed_ms") or 0) / 1000
    return None


NOW = parse_time(ENV.get("DIAG_NOW") or "") or datetime.datetime.now(UTC)
SINCE = NOW - datetime.timedelta(minutes=MINUTES)
paths = [ENV["LOG"] + ".1", ENV["LOG"]] if not ENV.get("LOG_FILE") else [ENV["LOG"]]
cancelled, legacy, downstream, finished_total = [], 0, collections.Counter(), 0
read_any = False
for path in paths:
    if not os.path.isfile(path):
        continue
    read_any = True
    with open(path, "rb") as handle:
        for raw in handle:
            if b'"request.finished"' not in raw and "下游已断开".encode() not in raw:
                continue
            try:
                entry = json.loads(raw)
            except ValueError:
                continue
            if not isinstance(entry, dict):
                continue
            moment = parse_time(entry.get("time"))
            if not moment or moment < SINCE or moment > NOW:
                continue
            request = entry.get("request") or {}
            if not isinstance(request, dict):
                continue
            message = entry.get("msg") or ""
            if entry.get("event") == "request.progress" and message.startswith("下游已断开"):
                downstream[request.get("id") or ""] += 1
                continue
            if entry.get("event") != "request.finished":
                continue
            if MODEL and (request.get("model") or "").strip().lower() != MODEL:
                continue
            finished_total += 1
            if request.get("state") != "cancelled":
                continue
            disconnect = request.get("disconnect")
            if not isinstance(disconnect, dict):
                legacy += 1
                continue
            cancelled.append({"t": moment, "request": request, "disconnect": disconnect, "source": entry.get("source") or ""})

print(f"==================== 最近被取消的请求（{MINUTES} 分钟内{'，模型 ' + MODEL if MODEL else ''}） ====================")
print(f"现在：{local(NOW)}；运行日志：{', '.join(p for p in paths if os.path.isfile(p)) or '找不到 ' + ENV['LOG']}")
heartbeat = ENV.get("HEARTBEAT") or ""
print(f"数据型心跳（STREAM_DATA_HEARTBEAT）：{heartbeat or '未写，默认开启'}"
      + ("；已关闭：经 new-api 等中转时心跳传不到客户端，建议打开" if heartbeat.lower() == "false" else ""))
if not read_any:
    raise SystemExit
print(f"完成的请求 {finished_total} 个，其中取消 {len(cancelled) + legacy} 个"
      + (f"（{legacy} 个没有断开现场：程序版本早于断开诊断）" if legacy else ""))
if not cancelled:
    raise SystemExit

initiators = collections.Counter(item["disconnect"].get("initiator") for item in cancelled)
print(f"谁断开：客户端 {initiators.get('client', 0)}，服务端取消（管理端取消或服务停止、重启） {initiators.get('service', 0)}")
clients = [item for item in cancelled if item["disconnect"].get("initiator") == "client"]
right_after_all = [item for item in clients if right_after_headers(item["disconnect"])]
if right_after_all:
    print(f"写响应头后 1 秒内断开：{len(right_after_all)} 个（不论阶段；经 new-api 时多为手机在本服务写响应头之前就断开了，"
          "首个事件在等待内就到时阶段显示为流中）")
for phase, title in (("before_first_event", "首事件前断开"), ("mid_stream", "流中断开")):
    group = [item for item in clients if item["disconnect"].get("phase") == phase]
    if not group:
        continue
    headers = sum(1 for item in group if item["disconnect"].get("headers_sent"))
    no_bytes = sum(1 for item in group if not item["disconnect"].get("written_bytes"))
    right_after = [item for item in group if right_after_headers(item["disconnect"])]
    print(f"\n[{title}] {len(group)} 个：已提交响应头 {headers}（其中写响应头后 1 秒内断开 {len(right_after)}），一个字节都没写出 {no_bytes}")
    data, legacy_data, zero_data = [], 0, 0
    for item in group:
        disconnect = item["disconnect"]
        since = number(disconnect, "since_last_data_write_ms")
        if since is not None:
            data.append(since / 1000)
        elif disconnect.get("headers_sent") and number(disconnect, "since_headers_sent_ms") is None:
            # 早于数据写出诊断的记录：已提交响应头却没有 since_headers_sent_ms（两项同时加入）
            legacy_data += 1
        elif disconnect.get("headers_sent"):
            zero_data += 1
    never = len(group) - len(data) - legacy_data - zero_data
    print("  断开前距最近数据写出（中转会转发的 data）：" + (distribution(data) if data else "都没有")
          + (f"；{zero_data} 个响应头已提交、却一块数据都没写出（只有换行或 \": ping\" 心跳，按已用时计入下面的判断）" if zero_data else "")
          + (f"；{never} 个响应头没提交、什么都没写出" if never else "")
          + (f"；{legacy_data} 个记录早于数据写出诊断（没有这一项）" if legacy_data else ""))
    print("  断开前距最近写出（含心跳与响应头）：" + distribution((number(item["disconnect"], "since_last_write_ms") or 0) / 1000 for item in group))
    upstream = [number(item["disconnect"], "since_upstream_event_ms") / 1000 for item in group
                if number(item["disconnect"], "since_upstream_event_ms") is not None]
    print("  断开前距上游最近事件：" + (distribution(upstream) if upstream else "都还没有上游事件"))
    print("  从进入服务到断开：" + distribution((number(item["disconnect"], "elapsed_ms") or 0) / 1000 for item in group))
    quiet = [(item, quiet_seconds(item["disconnect"])) for item in group]
    quiet = [(item, value) for item, value in quiet if value is not None]
    silent = [value for _, value in quiet if value >= 30]
    if silent:
        print(f"  其中 {len(silent)} 个断开前 30 秒以上没有写出数据（含一块数据都没写出、按已用时计的）：中转后面的客户端那一段长时间零字节，"
              "检查中间层读超时与心跳设置")
    hidden = [item for item, value in quiet
              if value * 1000 - (number(item["disconnect"], "since_last_write_ms") or 0) >= 20000]
    if hidden:
        print(f"  其中 {len(hidden)} 个只有换行或 \": ping\" 心跳在写（距最近写出比距最近数据短 20 秒以上）：经中转时这些心跳传不到客户端")
    if right_after:
        print(f"  其中 {len(right_after)} 个写响应头后 1 秒内断开：经 new-api 时多为手机在本服务写响应头之前就断开了，"
              "真实时刻看 new-api 日志的 stream ended … reason=client_gone")
    fixed = [item for item in group if 55000 <= (number(item["disconnect"], "elapsed_ms") or 0) < 65000
             or 95000 <= (number(item["disconnect"], "elapsed_ms") or 0) < 105000]
    if len(fixed) >= 2:
        print(f"  其中 {len(fixed)} 个在 60s 或 100s 附近断开：多半是前置 nginx（proxy_read_timeout 默认 60s）或 Cloudflare（100s）")
if downstream:
    print(f"\n转发层记录的“下游已断开，结束转发”：{sum(downstream.values())} 条（{len(downstream)} 个请求）")

if SAMPLES:
    print(f"\n最近 {min(SAMPLES, len(cancelled))} 个被取消的请求：")
    for item in cancelled[-SAMPLES:]:
        request, disconnect = item["request"], item["disconnect"]
        upstream = disconnect.get("since_upstream_event_ms")
        upstream_text = f"{upstream / 1000:.1f}s" if isinstance(upstream, (int, float)) else "无"
        data_since = disconnect.get("since_last_data_write_ms")
        data_text = f"{data_since / 1000:.1f}s" if isinstance(data_since, (int, float)) else "无"
        header_since = disconnect.get("since_headers_sent_ms")
        header_text = f"（断开前 {header_since / 1000:.1f}s）" if isinstance(header_since, (int, float)) else ""
        print(f"  {local(item['t'])} {request.get('id', '-')} {request.get('model', '-')} 账号={mask(item['source'])} "
              f"通道={request.get('channel') or '-'} | {'客户端' if disconnect.get('initiator') == 'client' else '服务端'} "
              f"{'流中' if disconnect.get('phase') == 'mid_stream' else '首事件前'} | 响应头{'已' if disconnect.get('headers_sent') else '未'}提交{header_text} | "
              f"已写出 {disconnect.get('written_chunks', 0)} 块 {disconnect.get('written_bytes', 0)} B | "
              f"距最近写出 {disconnect.get('since_last_write_ms', 0) / 1000:.1f}s | 距最近数据 {data_text} | 距上游最近事件 {upstream_text} | "
              f"已用时 {disconnect.get('elapsed_ms', 0) / 1000:.1f}s"
              + (" | 写响应头后立即断开" if right_after_headers(disconnect) else ""))
PY
