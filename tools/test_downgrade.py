#!/usr/bin/env python3
"""测试 Pro 请求有没有被上游降级（例如请求 gemini-3.1-pro-preview，实际由 3.1-flash-lite 生成）。

用同一段会被降级的对话（downgrade_request.json）连续请求几次，再用一段普通提示词做对照，逐次报告：
首字时间、总耗时、思考/正文 token、出字速度，以及上游在响应里依次标明的模型。

判断依据：
  降级       上游标明的模型系列与请求不同（Build 通道在最后一块报告实际服务的模型）
  疑似降级   Pro 请求出字 ≥180 tok/s（Playground 通道响应里没有模型名，只能看速度；Pro 正常约 90～150）
  正常       以上都不是

只用 Python 标准库，不需要安装依赖。用法：
  python3 tools/test_downgrade.py --key 你的APIKey
  python3 tools/test_downgrade.py --key 你的APIKey --runs 5 --trace      # 走 /trace/ 路由，服务端写排查记录
  python3 tools/test_downgrade.py --key 你的APIKey --request logs/trace/某条记录.json   # 直接重放一条排查记录
  python3 tools/test_downgrade.py --key 你的APIKey --bisect --runs 2      # 拆分对话，找出触发降级的部分
  python3 tools/test_downgrade.py --key 你的APIKey --formats all --runs 2 --trace   # 同一段对话换各种请求格式对比
  python3 tools/test_downgrade.py --key 你的APIKey --bench-count 10      # 测本服务 CountTokens 的耗时（降级判定会用到）
本服务开启"拒绝被降级的回复"时，被判定为降级的请求返回 HTTP 400（结论显示为"已拒绝"）；同一段对话 30 分钟内
再次请求会在发送前直接被拒绝，反复测试同一段对话请加 --trace（/trace 路由不查历史记录，照常判定）。
格式（--formats，逗号分隔，或 all）：
  gemini-stream     Gemini 原生流式（默认）           gemini-unary   Gemini 原生非流式
  gemini-merged     全部历史合并成一条用户消息         gemini-nosig   去掉历史里的思考签名
  gemini-sysuser    系统提示并入第一条用户消息         openai-chat    OpenAI Chat Completions（流式）
  openai-responses  OpenAI Responses（流式）
API Key 也可以放在环境变量 API_KEY 里。请求内容只发给本服务，脚本不打印对话内容。
"""
import argparse
import json
import os
import sys
import time
import uuid
import urllib.error
import urllib.request

CONTROL_PROMPT = (
    "请写一篇约 3000 字的中文短篇小说：一位老钟表匠在小镇上修了一辈子钟，某天收到一只会倒着走的怀表。"
    "要求有完整的起承转合、细致的人物与环境描写，直接输出正文，不要标题和说明。"
)


def model_family(model):
    name = (model or "").lower().replace("models/", "").strip()
    if "flash-lite" in name:
        return "flash-lite"
    if "flash" in name:
        return "flash"
    if "pro" in name:
        return "pro"
    return name


def load_request(path, model):
    """读取请求体；传入的是排查记录（logs/trace/*.json）时取其中客户端的原始请求与模型"""
    with open(path, encoding="utf-8") as source:
        data = json.load(source)
    if isinstance(data, dict) and "client" in data:
        client = data["client"]
        body = (client.get("body") or {}).get("json")
        if not isinstance(body, dict):
            sys.exit(f"排查记录 {path} 里没有可重放的 JSON 请求体")
        route = client.get("path") or ""
        if "/models/" in route and model is None:
            model = route.split("/models/", 1)[1].split(":", 1)[0]
        data = body
    return data, model or "gemini-3.1-pro-preview"


FORMATS = ["gemini-stream", "gemini-unary", "gemini-merged", "gemini-nosig", "gemini-sysuser",
           "openai-chat", "openai-responses"]


def gemini_text(content):
    return "".join(part.get("text") or "" for part in (content or {}).get("parts") or [])


def build_request(fmt, args, model, body):
    """按格式构造请求：返回 (url, 鉴权头, 请求体, 解析方式, 是否流式)。内容与参数保持一致，只换写法"""
    root = args.base.rstrip("/") + ("/trace" if args.trace else "")
    contents = body.get("contents") or []
    system = gemini_text(body.get("systemInstruction"))
    config = body.get("generationConfig") or {}
    if fmt.startswith("gemini-"):
        payload = json.loads(json.dumps(body))
        if fmt == "gemini-merged":
            merged = "\n\n".join(f"【{'用户' if c.get('role') == 'user' else '模型'}】\n{gemini_text(c)}" for c in contents)
            payload["contents"] = [{"role": "user", "parts": [{"text": merged}]}]
        elif fmt == "gemini-nosig":
            for content in payload.get("contents") or []:
                for part in content.get("parts") or []:
                    part.pop("thoughtSignature", None)
                    part.pop("thought_signature", None)
        elif fmt == "gemini-sysuser":
            payload.pop("systemInstruction", None)
            if system and payload.get("contents"):
                first = payload["contents"][0]
                first["parts"] = [{"text": system}] + list(first.get("parts") or [])
        method = "generateContent" if fmt == "gemini-unary" else "streamGenerateContent?alt=sse"
        return f"{root}/v1beta/models/{model}:{method}", {"x-goog-api-key": args.key}, payload, "gemini", fmt != "gemini-unary"
    history = [{"role": "user" if c.get("role") == "user" else "assistant", "content": gemini_text(c)} for c in contents]
    headers = {"Authorization": f"Bearer {args.key}"}
    if fmt == "openai-chat":
        payload = {"model": model, "messages": ([{"role": "system", "content": system}] if system else []) + history,
                   "stream": True, "stream_options": {"include_usage": True}}
        if "temperature" in config:
            payload["temperature"] = config["temperature"]
        if "maxOutputTokens" in config:
            payload["max_tokens"] = config["maxOutputTokens"]
        return f"{root}/v1/chat/completions", headers, payload, "openai", True
    if fmt == "openai-responses":
        payload = {"model": model, "input": history, "stream": True}
        if system:
            payload["instructions"] = system
        if "temperature" in config:
            payload["temperature"] = config["temperature"]
        if "maxOutputTokens" in config:
            payload["max_output_tokens"] = config["maxOutputTokens"]
        return f"{root}/v1/responses", headers, payload, "responses", True
    sys.exit(f"未知格式 {fmt}，可选：{', '.join(FORMATS)}")


def note_model(result, name):
    if name and (not result["versions"] or result["versions"][-1] != name):
        result["versions"].append(name)


def handle_event(kind, event, result, now):
    """解析一个事件，统一记录：上游标明的模型、首个/最后一个内容的时间、正文与思考字节、用量（统一成 Gemini 的字段名）"""
    def content(text, thought):
        if not text:
            return
        if result["first"] is None:
            result["first"] = now
        result["last"] = now
        result["thought" if thought else "text"] += len(text.encode("utf-8"))

    if isinstance(event, list):
        for item in event:
            handle_event(kind, item, result, now)
        return
    if not isinstance(event, dict):
        return
    if event.get("error"):
        result["error"] = json.dumps(event["error"], ensure_ascii=False)[:500]
    if kind == "gemini":
        note_model(result, event.get("modelVersion"))
        for candidate in event.get("candidates") or []:
            if candidate.get("finishReason"):
                result["finish"] = candidate["finishReason"]
            for part in (candidate.get("content") or {}).get("parts") or []:
                content(part.get("text") or "", part.get("thought"))
        if event.get("usageMetadata"):
            result["usage"] = event["usageMetadata"]
            result["last"] = now
    elif kind == "openai":
        note_model(result, event.get("model"))
        note_model(result, event.get("provider_model"))
        for choice in event.get("choices") or []:
            delta = choice.get("delta") or choice.get("message") or {}
            content(delta.get("reasoning_content") or delta.get("reasoning") or "", True)
            content(delta.get("content") if isinstance(delta.get("content"), str) else "", False)
            if choice.get("finish_reason"):
                result["finish"] = choice["finish_reason"]
        usage = event.get("usage")
        if usage:
            reasoning = int((usage.get("completion_tokens_details") or {}).get("reasoning_tokens") or 0)
            result["usage"] = {"promptTokenCount": usage.get("prompt_tokens"), "thoughtsTokenCount": reasoning,
                               "candidatesTokenCount": int(usage.get("completion_tokens") or 0) - reasoning}
            result["last"] = now
    elif kind == "responses":
        kind_name = event.get("type") or ""
        if kind_name.endswith("output_text.delta"):
            content(event.get("delta") or "", False)
        elif "reasoning" in kind_name and kind_name.endswith(".delta"):
            content(event.get("delta") or "", True)
        response = event.get("response") if isinstance(event.get("response"), dict) else None
        if response:
            note_model(result, response.get("model"))
            note_model(result, response.get("provider_model"))
            usage = response.get("usage")
            if usage:
                reasoning = int((usage.get("output_tokens_details") or {}).get("reasoning_tokens") or 0)
                result["usage"] = {"promptTokenCount": usage.get("input_tokens"), "thoughtsTokenCount": reasoning,
                                   "candidatesTokenCount": int(usage.get("output_tokens") or 0) - reasoning}
                result["last"] = now
                result["finish"] = response.get("status") or result["finish"]


def request_once(fmt, args, model, body, agent):
    url, headers, payload, kind, streaming = build_request(fmt, args, model, body)
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    request = urllib.request.Request(url, data=data, method="POST", headers=dict(headers, **{
        "Content-Type": "application/json", "Accept": "text/event-stream" if streaming else "application/json",
        "User-Agent": agent,
    }))
    result = {"status": 0, "first": None, "last": None, "versions": [], "usage": {}, "finish": "",
              "text": 0, "thought": 0, "error": "", "streaming": streaming}
    started = time.monotonic()
    try:
        response = urllib.request.urlopen(request, timeout=args.timeout)
    except urllib.error.HTTPError as error:
        result["status"] = error.code
        result["error"] = error.read().decode("utf-8", "replace")[:500]
        result["total"] = time.monotonic() - started
        return result
    except Exception as error:  # 连接失败、超时等
        result["error"] = str(error)
        result["total"] = time.monotonic() - started
        return result
    result["status"] = response.status
    with response:
        if not streaming:
            raw = response.read().decode("utf-8", "replace")
            try:
                handle_event(kind, json.loads(raw), result, time.monotonic() - started)
            except ValueError:
                result["error"] = raw[:500]
        else:
            for raw in response:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                chunk = line[5:].strip()
                if not chunk or chunk == "[DONE]":
                    continue
                try:
                    event = json.loads(chunk)
                except ValueError:
                    continue
                handle_event(kind, event, result, time.monotonic() - started)
    result["total"] = time.monotonic() - started
    if not streaming:
        # 非流式拿不到首字时间：按总耗时算速度（含等待时间，是偏低的下限）
        result["first"], result["last"] = 0.0, result["total"]
    return result


def admin_headers():
    """控制面 /api/ 必须携带管理令牌：优先读环境变量 ADMIN_TOKEN，其次读程序目录（脚本上一级）或当前目录的 .admin-token"""
    token = os.environ.get("ADMIN_TOKEN", "").strip()
    if not token:
        here = os.path.dirname(os.path.abspath(__file__))
        for candidate in (os.path.join(here, "..", ".admin-token"), ".admin-token"):
            try:
                with open(candidate, encoding="utf-8") as file:
                    token = file.read().strip()
                    break
            except OSError:
                continue
    return {"X-Admin-Token": token} if token else {}


def trace_summary(base, agent):
    """按本次请求独有的 User-Agent 找到对应的排查记录（别的请求同时走 /trace 也不会认错），返回自动结论；
    记录在响应结束后才写出，最多等 6 秒"""
    for _ in range(6):
        try:
            request = urllib.request.Request(f"{base.rstrip('/')}/api/debug/traces", headers=admin_headers())
            with urllib.request.urlopen(request, timeout=10) as response:
                files = json.load(response).get("files") or []
        except Exception as error:
            return f"（读取排查记录失败：{error}；可在服务器上运行 ./start.sh trace 查看）"
        for item in files[:40]:
            if agent in (item.get("summary") or ""):
                return item["summary"]
        time.sleep(1)
    return "（没有找到这次请求的排查记录）"


def judge(model, result):
    usage = result["usage"]
    thoughts = int(usage.get("thoughtsTokenCount") or 0)
    output = int(usage.get("candidatesTokenCount") or 0)
    speed = 0.0
    if result["first"] is not None and result["last"] and result["last"] - result["first"] >= 1:
        speed = (thoughts + output) / (result["last"] - result["first"])
    asked = model_family(model)
    served = [v for v in result["versions"] if model_family(v) != asked]
    error_text = result["error"] or ""
    if "Prohibited Use policy" in error_text:
        verdict = "已拒绝（本服务判定为降级，返回 400）"
    elif "blockReason" in error_text:
        verdict = "上游拦截（" + error_text.split("blockReason:")[-1].split(")")[0].strip() + "）"
    elif result["status"] != 200 or result["error"]:
        verdict = "失败"
    elif served:
        verdict = f"降级（上游标明 {served[-1]}）"
    elif asked == "pro" and speed >= 180 and thoughts + output >= 500:
        verdict = "疑似降级（速度远高于 Pro）"
    elif asked == "pro" and speed >= 150 and thoughts + output >= 500:
        verdict = "偏快（需要和对照组比较）"
    else:
        verdict = "正常"
    return thoughts, output, speed, verdict


def run_group(name, args, model, body, runs, fmt="gemini-stream"):
    rows = []
    for index in range(1, runs + 1):
        agent = "downgrade-test/" + uuid.uuid4().hex[:8]
        result = request_once(fmt, args, model, body, agent)
        thoughts, output, speed, verdict = judge(model, result)
        first = f"{result['first']:.1f}s" if result["first"] is not None and result["streaming"] else "-（非流式）"
        line = (f"[{name} #{index}] HTTP {result['status']} · 首字 {first} · 总 {result['total']:.1f}s · "
                f"思考 {thoughts} · 正文 {output} token · 速度 {speed:.0f} tok/s · "
                f"模型 {' → '.join(result['versions']) or '-'} · 结论：{verdict}")
        print(line, flush=True)
        if result["error"]:
            print(f"    错误：{result['error']}", flush=True)
        if result["status"] == 200 and not result["text"] and result["thought"]:
            print("    注意：只有思考内容、没有正文", flush=True)
        if args.trace:
            print(f"    排查记录：{trace_summary(args.base, agent)}", flush=True)
        rows.append((speed, verdict, int(result["usage"].get("promptTokenCount") or 0)))
        if index < runs:
            time.sleep(args.interval)
    return rows


def bisect_variants(body):
    """把对话拆成几种组合分别请求，看哪一部分触发降级；截断后保证第一条和最后一条都是用户消息"""
    contents = list(body.get("contents") or [])
    system = body.get("systemInstruction")
    base = {key: value for key, value in body.items() if key not in ("contents", "systemInstruction")}

    def from_user(items):
        start = 0
        while start < len(items) and items[start].get("role") != "user":
            start += 1
        return items[start:]

    def end_user(items):
        end = len(items)
        while end > 0 and items[end - 1].get("role") != "user":
            end -= 1
        return items[:end]

    def build(items, with_system):
        variant = dict(base, contents=items)
        if with_system and system:
            variant["systemInstruction"] = system
        return variant

    half = len(contents) // 2
    quarter = len(contents) * 3 // 4
    last_user = end_user(contents)[-1:]
    variants = [
        ("完整对话", build(contents, True)),
        ("去掉系统提示", build(contents, False)),
        ("系统提示 + 后一半消息", build(from_user(contents[half:]), True)),
        ("系统提示 + 最后四分之一", build(from_user(contents[quarter:]), True)),
        ("系统提示 + 前一半消息", build(end_user(contents[:half]), True)),
        ("系统提示 + 最后一条用户消息", build(last_user, True)),
        ("只有最后一条用户消息", build(last_user, False)),
    ]
    return [(name, variant) for name, variant in variants if variant.get("contents")]


def summarize(name, rows):
    if not rows:
        return
    ok = [speed for speed, verdict, _ in rows if verdict != "失败" and speed > 0]
    counts = {}
    for _, verdict, _ in rows:
        key = verdict.split("（")[0]
        counts[key] = counts.get(key, 0) + 1
    average = sum(ok) / len(ok) if ok else 0
    detail = "、".join(f"{key} {value} 次" for key, value in counts.items())
    tokens = max(prompt for _, _, prompt in rows)
    print(f"  {name}：{len(rows)} 次，输入约 {tokens} token，平均出字 {average:.0f} tok/s，{detail}")


def bench_count(args, model, runs):
    """测本服务 Gemini countTokens 接口的耗时。这里每次都要选号；降级判定在生成请求已分到的账号上直接计数，
    省掉选号，实际会略快"""
    bench_args = argparse.Namespace(**vars(args))
    bench_args.trace = False
    body = {"contents": [{"role": "user", "parts": [{"text": CONTROL_PROMPT * 8}]}]}
    url, headers, payload, _, _ = build_request("gemini-unary", bench_args, model, body)
    url = url.split("?")[0].replace(":generateContent", ":countTokens")
    data = json.dumps({"contents": payload["contents"]}, ensure_ascii=False).encode("utf-8")
    print(f"测 CountTokens 耗时：{url}（约 {len(CONTROL_PROMPT) * 8} 字，{runs} 次）")
    costs = []
    for index in range(runs):
        request = urllib.request.Request(url, data=data, method="POST", headers=dict(headers, **{
            "Content-Type": "application/json", "User-Agent": f"test_downgrade-count/{uuid.uuid4().hex[:8]}"}))
        started = time.monotonic()
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                tokens = json.load(response).get("totalTokens")
        except urllib.error.HTTPError as error:
            print(f"  第 {index + 1} 次失败：HTTP {error.code} {error.read().decode('utf-8', 'replace')[:200]}")
            continue
        except Exception as error:  # 连接失败、超时等
            print(f"  第 {index + 1} 次失败：{error}")
            continue
        cost = (time.monotonic() - started) * 1000
        costs.append(cost)
        print(f"  第 {index + 1} 次：{cost:.0f} ms（{tokens} token）")
        time.sleep(0.3)
    if not costs:
        return
    costs.sort()
    print(f"\nCountTokens 耗时：最快 {costs[0]:.0f} ms · 中位 {costs[len(costs) // 2]:.0f} ms · 平均 {sum(costs) / len(costs):.0f} ms"
          f" · 最慢 {costs[-1]:.0f} ms（成功 {len(costs)}/{runs} 次）")
    print("降级判定只在 Build 通道估算速度落在模糊区间时调用，并且不需要选号，实际会比这里略快；"
          "若中位数接近或超过服务配置里的 CountTokens 超时（默认 1500 ms），把超时调大或把模糊区间收窄。")


def main():
    parser = argparse.ArgumentParser(description="测试 Pro 请求是否被上游降级")
    parser.add_argument("--base", default=os.environ.get("BASE_URL", "http://127.0.0.1:2048"), help="本服务地址")
    parser.add_argument("--key", default=os.environ.get("API_KEY", ""), help="本服务的 API Key")
    parser.add_argument("--model", default=None, help="请求的模型，默认 gemini-3.1-pro-preview")
    parser.add_argument("--request", default=None, help="降级对话的请求体（默认 downgrade_request.json），也可以是一条排查记录")
    parser.add_argument("--runs", type=int, default=3, help="降级对话请求次数")
    parser.add_argument("--control-runs", type=int, default=2, help="对照组请求次数，0 为不跑")
    parser.add_argument("--interval", type=float, default=3, help="两次请求之间等待的秒数")
    parser.add_argument("--timeout", type=float, default=600, help="单次请求超时秒数")
    parser.add_argument("--trace", action="store_true", help="走 /trace/ 路由，并读取服务端排查记录的自动结论")
    parser.add_argument("--bisect", action="store_true",
                        help="拆分对比：完整对话、去掉系统提示、只用后一半/前一半消息等组合各请求 --runs 次，找出触发降级的部分")
    parser.add_argument("--formats", default="",
                        help="同一段对话换请求格式对比，逗号分隔或 all：" + ", ".join(FORMATS))
    parser.add_argument("--bench-count", type=int, default=0,
                        help="只测本服务 CountTokens 接口的耗时 N 次（降级判定在 Build 通道会用到），不发生成请求")
    args = parser.parse_args()
    if not args.key:
        sys.exit("缺少 API Key：用 --key 指定，或设置环境变量 API_KEY")
    if args.bench_count > 0:
        bench_count(args, args.model or "gemini-3.1-pro-preview", args.bench_count)
        return

    path = args.request
    if path is None:
        here = os.path.dirname(os.path.abspath(__file__))
        for candidate in ("downgrade_request.json", os.path.join(here, "downgrade_request.json")):
            if os.path.exists(candidate):
                path = candidate
                break
    if path is None or not os.path.exists(path):
        sys.exit("找不到降级对话的请求体：把 downgrade_request.json 放在当前目录或脚本同目录，或用 --request 指定")
    body, model = load_request(path, args.model)
    print(f"服务地址 {args.base}{' （/trace 路由）' if args.trace else ''} · 模型 {model} · 请求体 {path}"
          f"（{len(body.get('contents') or [])} 条消息）")
    print("通道（Playground / Build）由号池选择，多跑几次才能两种都碰到；只有 Build 通道的响应会标明实际模型。")
    print("本服务开启降级判定时，被降级的请求返回 400（结论\"已拒绝\"）；反复测试同一段对话请加 --trace，否则 30 分钟内"
          "会被历史记录直接拒绝。\n")

    if args.formats:
        formats = FORMATS if args.formats.strip() == "all" else [f.strip() for f in args.formats.split(",") if f.strip()]
        for fmt in formats:
            if fmt not in FORMATS:
                sys.exit(f"未知格式 {fmt}，可选：{', '.join(FORMATS)}")
        results = []
        for fmt in formats:
            print(f"\n== 格式 {fmt}")
            results.append((fmt, run_group(fmt, args, model, body, args.runs, fmt)))
        if args.control_runs > 0:
            print("\n== 对照组（普通提示词，Gemini 原生流式）")
            control_body = {
                "contents": [{"role": "user", "parts": [{"text": CONTROL_PROMPT}]}],
                "generationConfig": {"temperature": 1, "maxOutputTokens": 64000},
            }
            results.append(("对照组", run_group("对照组", args, model, control_body, args.control_runs)))
        print("\n格式对比汇总（上游标明其他模型或出字 ≥180 tok/s 即为降级；Pro 正常约 90～150 tok/s）")
        for name, rows in results:
            summarize(name, rows)
        print("  注意：各种格式进入本服务后都会转换成同一种上游请求；只有 merged、nosig、sysuser 这几种改变了发给谷歌的内容写法。")
        return

    if args.bisect:
        results = []
        for name, variant in bisect_variants(body):
            note = "，带系统提示" if "systemInstruction" in variant else ""
            print(f"\n== {name}（{len(variant['contents'])} 条消息{note}）")
            results.append((name, run_group(name, args, model, variant, args.runs)))
        print("\n拆分对比汇总（出字 ≥180 tok/s 或上游标明其他模型即为降级；Pro 正常约 90～150 tok/s）")
        for name, rows in results:
            summarize(name, rows)
        return

    downgraded = run_group("降级对话", args, model, body, args.runs)
    control = []
    if args.control_runs > 0:
        print()
        control_body = {
            "contents": [{"role": "user", "parts": [{"text": CONTROL_PROMPT}]}],
            "generationConfig": {"temperature": 1, "maxOutputTokens": 64000},
        }
        control = run_group("对照组", args, model, control_body, args.control_runs)
    print("\n汇总")
    summarize("降级对话", downgraded)
    summarize("对照组", control)
    print("  对照组是普通提示词，反映这批账号上 Pro 的正常速度；降级对话明显更快、或上游标明了其他模型，说明这段对话仍被降级。")


if __name__ == "__main__":
    main()
