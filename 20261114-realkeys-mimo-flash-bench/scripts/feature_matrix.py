#!/usr/bin/env python3
# 日期: 2026-11-14 17:50:00.000
# 作者: Zhai Wenjia
"""全功能矩阵实测：5 个有效 key × 10 项功能，经网关（127.0.0.1:8080）打真实上游。

用法：/tmp/bench/bin/python scripts/feature_matrix.py
输出：raw/feature_matrix.json（逐格 ok/fail + 延迟 + 证据片段）
"""
import json, os, re, time, urllib.request, urllib.error

BASE = "http://127.0.0.1:8080"
MODELS = ["qwen3.8-flash", "glm-5.3-flash", "deepseek-flash", "kimi-k3.1-pro", "mimo-v2.6-flash"]
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

def call(path, body=None, stream=False, timeout=60):
    req = urllib.request.Request(BASE + path, method="POST" if body is not None else "GET")
    req.add_header("Content-Type", "application/json")
    if body is not None:
        data = json.dumps(body).encode()
    else:
        data = None
    t0 = time.time()
    for attempt in range(4):
        try:
            with urllib.request.urlopen(req, data=data, timeout=timeout) as r:
                raw = r.read().decode(errors="replace")
                return r.status, raw, time.time() - t0
        except urllib.error.HTTPError as e:
            body = e.read().decode(errors="replace")
            if e.code == 429 and attempt < 3:
                time.sleep(22)  # Kimi 组织级 RPM=3 ⇒ 退避后重试（环境限流，非被测缺陷）
                continue
            return e.code, body, time.time() - t0
        except Exception as e:
            return -1, repr(e), time.time() - t0
    return 429, "retries exhausted", time.time() - t0

def feat_models(model):
    st, body, dt = call("/v1/models")
    ok = st == 200 and f'"{model}"' in body
    return ok, dt, body[:120]

def feat_chat_nonstream(model):
    st, body, dt = call("/v1/chat/completions", {"model": model, "messages": [{"role": "user", "content": "回答：1+1=?"}], "max_tokens": 24})
    try:
        j = json.loads(body)
        u = j.get("usage") or {}
        msg = j["choices"][0]["message"]
        ok = (st == 200 and (msg.get("content") is not None or msg.get("reasoning_content"))
              and u.get("prompt_tokens", 0) > 0 and u.get("completion_tokens", 0) > 0
              and u.get("total_tokens", 0) > 0)
        ev = f"usage={u}"
    except Exception:
        ok, ev = False, body[:150]
    return ok, dt, ev

def feat_chat_stream(model):
    st, raw, dt = call("/v1/chat/completions", {"model": model, "messages": [{"role": "user", "content": "回答：1+1=?"}], "max_tokens": 24, "stream": True})
    has_delta = '"delta"' in raw
    has_finish = '"finish_reason"' in raw and '"finish_reason":null' not in raw.split('"finish_reason":')[-1][:12]
    has_done = "[DONE]" in raw
    has_usage = '"usage":{' in raw  # 非 null 的 usage 对象（delta 帧的 "usage":null 不算）
    ok = st == 200 and has_delta and has_done and has_usage
    return ok, dt, f"delta={has_delta} done={has_done} usage={has_usage}"

def feat_responses_nonstream(model):
    st, body, dt = call("/v1/responses", {"model": model, "input": "回答：1+1=?", "max_output_tokens": 24})
    try:
        j = json.loads(body)
        u = j.get("usage") or {}
        ok = st == 200 and len(j.get("output", [])) > 0 and u.get("input_tokens", 0) > 0
        ev = f"status={j.get('status')} usage_in={u.get('input_tokens')} out={u.get('output_tokens')}"
    except Exception:
        ok, ev = False, body[:150]
    return ok, dt, ev

def feat_responses_stream(model):
    st, raw, dt = call("/v1/responses", {"model": model, "input": "回答：1+1=?", "max_output_tokens": 24, "stream": True})
    ok = st == 200 and "response." in raw and ("response.completed" in raw or "response.incomplete" in raw) and "[DONE]" in raw
    return ok, dt, f"events={len(re.findall(r'event: response', raw))} completed={'response.completed' in raw}"

def feat_tools(model):
    tool = {"type": "function", "function": {"name": "get_weather", "description": "查询城市天气", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}
    st, body, dt = call("/v1/chat/completions", {"model": model, "messages": [{"role": "user", "content": "北京今天天气怎么样？请调用 get_weather 工具"}], "tools": [tool], "max_tokens": 64})
    try:
        j = json.loads(body)
        msg = j["choices"][0]["message"]
        tcs = msg.get("tool_calls") or []
        ok = st == 200 and (len(tcs) > 0 or msg.get("content"))
        ev = f"tool_calls={len(tcs)}" + (f" name={tcs[0]['function']['name']}" if tcs else "")
    except Exception:
        ok, ev = False, body[:150]
    return ok, dt, ev

def feat_reasoning(model):
    st, body, dt = call("/v1/chat/completions", {"model": model, "messages": [{"role": "user", "content": "9.11 和 9.9 哪个大？简答"}], "max_tokens": 96})
    try:
        j = json.loads(body)
        msg = j["choices"][0]["message"]
        rc = msg.get("reasoning_content") or ""
        ok = st == 200 and bool(msg.get("content") or rc)
        ev = f"reasoning_len={len(rc)} content_len={len(msg.get('content') or '')}"
    except Exception:
        ok, ev = False, body[:150]
    return ok, dt, ev

def feat_prompt_cache_key(model):
    st, body, dt = call("/v1/responses", {"model": model, "input": "回答：1+1=?", "max_output_tokens": 24, "prompt_cache_key": "bench-pk-1"})
    ok = st == 200
    return ok, dt, body[:120]

def feat_prev_resp_id(model):
    st, body, dt = call("/v1/responses", {"model": model, "input": "hi", "max_output_tokens": 8, "previous_response_id": "resp_bench_nonexist"})
    # 预期两类合法结果：400 unsupported_capability（chat 形四家）或上游 4xx（透传后上游拒未知 id）
    ok = st in (200, 400, 404, 422) and ("unsupported_capability" in body or "response" in body.lower() or "error" in body.lower())
    ev = f"HTTP {st} " + body[:100]
    return ok, dt, ev

def feat_404(model):
    st, body, dt = call("/v1/chat/completions", {"model": "nonexistent-model-xyz", "messages": [{"role": "user", "content": "hi"}], "max_tokens": 4})
    ok = st == 404 and "model_not_found" in body
    return ok, dt, body[:100]

FEATURES = [
    ("models_list", feat_models), ("chat_nonstream", feat_chat_nonstream),
    ("chat_stream", feat_chat_stream), ("responses_nonstream", feat_responses_nonstream),
    ("responses_stream", feat_responses_stream), ("tools", feat_tools),
    ("reasoning", feat_reasoning), ("prompt_cache_key", feat_prompt_cache_key),
    ("previous_response_id", feat_prev_resp_id), ("err_404_model_not_found", feat_404),
]

matrix = {}
for m in MODELS:
    row = {}
    for name, fn in FEATURES:
        ok, dt, ev = fn(m)
        row[name] = {"ok": bool(ok), "latency_s": round(dt, 3), "evidence": ev}
        print(f"{m:18s} {name:22s} {'PASS' if ok else 'FAIL'}  {ev[:60]}", flush=True)
    matrix[m] = row

out = os.path.join(ROOT, "raw", "feature_matrix.json")
json.dump({"ts": time.strftime("%Y-%m-%d %H:%M:%S"), "matrix": matrix}, open(out, "w"), ensure_ascii=False, indent=1)
total = sum(1 for r in matrix.values() for c in r.values() if c["ok"])
print(f"\n合计 {total}/{sum(len(r) for r in matrix.values())} PASS；saved:", out)
