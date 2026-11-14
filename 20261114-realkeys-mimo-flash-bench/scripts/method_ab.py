#!/usr/bin/env python3
# 日期: 2026-11-14 18:20:00.000
# 作者: Zhai Wenjia
"""方法 A/B 实测：每家上游 /chat/completions（Chat 法）vs /responses（Responses 法），
同电池对比兼容性（通过率）与功能完整度（usage/工具名/reasoning/流式），逐家选出更优方法。

用法：/tmp/bench/bin/python scripts/method_ab.py
输出：raw/method_ab.json（逐格证据 + 逐家评分与结论）
"""
import json, os, time, urllib.request, urllib.error

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ENV = {}
for line in open(os.path.join(os.path.dirname(ROOT), ".env")):
    line = line.strip()
    if line and not line.startswith("#") and "=" in line:
        k, v = line.split("=", 1)
        ENV[k.strip()] = v.strip()

PROVIDERS = [
    ("Qwen",      "https://dashscope.aliyuncs.com/compatible-mode/v1", "QWEN_API_KEY",     "qwen3.8-flash"),
    ("Zhipu",     "https://open.bigmodel.cn/api/paas/v4",             "ZHIPU_API_KEY",    "glm-5.3-flash"),
    ("DeepSeek",  "https://api.deepseek.com/v1",                      "DEEPSEEK_API_KEY", "deepseek-flash"),
    ("Moonshot",  "https://api.moonshot.cn/v1",                       "MOONSHOT_API_KEY", "kimi-k3.1-pro"),
    ("MiMo",      "https://api.xiaomimimo.com/v1",                    "MIMO_API_KEY",     "mimo-v2.6-flash"),
]

def call(url, key, body, stream=False, timeout=60):
    req = urllib.request.Request(url, method="POST")
    req.add_header("Authorization", "Bearer " + key)
    req.add_header("Content-Type", "application/json")
    for attempt in range(4):
        t0 = time.time()
        try:
            with urllib.request.urlopen(req, data=json.dumps(body).encode(), timeout=timeout) as r:
                return r.status, r.read().decode(errors="replace"), time.time() - t0
        except urllib.error.HTTPError as e:
            b = e.read().decode(errors="replace")
            if e.code == 429 and attempt < 3:
                time.sleep(22); continue
            return e.code, b, time.time() - t0
        except Exception as e:
            return -1, repr(e)[:150], time.time() - t0

TOOL = {"type": "function", "function": {"name": "get_weather", "description": "查天气",
        "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}

def battery_chat(base, key, model):
    """Chat 法（/chat/completions）功能电池。返回逐项结果。"""
    out = {}
    st, b, dt = call(base + "/chat/completions", key, {"model": model, "messages": [{"role": "user", "content": "说好"}], "max_tokens": 200})
    try:
        j = json.loads(b); msg = j["choices"][0]["message"]; u = j.get("usage") or {}
        out["basic"] = {"ok": st == 200 and (msg.get("content") or msg.get("reasoning_content")), "usage_ok": u.get("prompt_tokens", 0) > 0, "lat": round(dt, 2)}
    except Exception:
        out["basic"] = {"ok": False, "err": b[:120], "lat": round(dt, 2)}
    st, b, dt = call(base + "/chat/completions", key, {"model": model, "messages": [{"role": "user", "content": "北京天气？请调用 get_weather"}], "tools": [TOOL], "max_tokens": 300})
    try:
        j = json.loads(b); tcs = j["choices"][0]["message"].get("tool_calls") or []
        out["tools"] = {"ok": st == 200 and len(tcs) > 0, "name_ok": bool(tcs and tcs[0].get("function", {}).get("name")), "lat": round(dt, 2)}
    except Exception:
        out["tools"] = {"ok": False, "err": b[:120], "lat": round(dt, 2)}
    st, b, dt = call(base + "/chat/completions", key, {"model": model, "messages": [{"role": "user", "content": "9.11和9.9哪个大？简答"}], "max_tokens": 300})
    try:
        j = json.loads(b); msg = j["choices"][0]["message"]
        out["reasoning"] = {"ok": st == 200 and bool(msg.get("reasoning_content") or msg.get("content")), "reasoning_len": len(msg.get("reasoning_content") or ""), "lat": round(dt, 2)}
    except Exception:
        out["reasoning"] = {"ok": False, "err": b[:120], "lat": round(dt, 2)}
    st, b, dt = call(base + "/chat/completions", key, {"model": model, "messages": [{"role": "user", "content": "说好"}], "max_tokens": 200, "stream": True})
    has_usage = '"usage":{' in b
    out["stream"] = {"ok": st == 200 and '"delta"' in b and "[DONE]" in b, "usage_ok": has_usage, "lat": round(dt, 2)}
    return out

def battery_responses(base, key, model):
    """Responses 法（/responses）功能电池。"""
    out = {}
    st, b, dt = call(base + "/responses", key, {"model": model, "input": "说好", "max_output_tokens": 200})
    try:
        j = json.loads(b); u = j.get("usage") or {}
        out["basic"] = {"ok": st == 200 and len(j.get("output", [])) > 0, "usage_ok": (u.get("input_tokens") or 0) > 0, "lat": round(dt, 2)}
    except Exception:
        out["basic"] = {"ok": False, "err": b[:120], "lat": round(dt, 2)}
    st, b, dt = call(base + "/responses", key, {"model": model, "input": "北京天气？请调用 get_weather", "tools": [TOOL], "max_output_tokens": 300})
    try:
        j = json.loads(b)
        fns = [o for o in j.get("output", []) if o.get("type") == "function_call"]
        out["tools"] = {"ok": st == 200 and len(fns) > 0, "name_ok": bool(fns and fns[0].get("name")), "lat": round(dt, 2)}
    except Exception:
        out["tools"] = {"ok": False, "err": b[:120], "lat": round(dt, 2)}
    st, b, dt = call(base + "/responses", key, {"model": model, "input": "9.11和9.9哪个大？简答", "max_output_tokens": 300})
    try:
        j = json.loads(b)
        rs = [o for o in j.get("output", []) if o.get("type") == "reasoning"]
        txt = j.get("output_text") or ""
        out["reasoning"] = {"ok": st == 200 and bool(rs or txt), "reasoning_items": len(rs), "lat": round(dt, 2)}
    except Exception:
        out["reasoning"] = {"ok": False, "err": b[:120], "lat": round(dt, 2)}
    st, b, dt = call(base + "/responses", key, {"model": model, "input": "说好", "max_output_tokens": 200, "stream": True})
    out["stream"] = {"ok": st == 200 and "response." in b and "[DONE]" in b, "usage_ok": '"usage":{' in b or '"usage": {' in b, "lat": round(dt, 2)}
    return out

def score(bat):
    feats = ["basic", "tools", "reasoning", "stream"]
    compat = sum(1 for f in feats if bat.get(f, {}).get("ok"))
    complete = compat + sum(1 for f in feats if bat.get(f, {}).get("usage_ok") or bat.get(f, {}).get("name_ok") or bat.get(f, {}).get("reasoning_len", 0) > 0 or bat.get(f, {}).get("reasoning_items", 0) > 0)
    return compat, complete

results = {}
for name, base, envk, model in PROVIDERS:
    key = ENV[envk]
    chat, resp = battery_chat(base, key, model), battery_responses(base, key, model)
    cs, rs = score(chat), score(resp)
    verdict = "chat" if cs > rs else ("responses" if rs > cs else "平手（保留 chat：转换面更小）")
    results[name] = {"model": model, "chat": chat, "responses": resp,
                     "score_chat": {"compat": cs[0], "complete": cs[1]},
                     "score_responses": {"compat": rs[0], "complete": rs[1]},
                     "verdict": verdict}
    print(f"{name:10s} chat={cs} responses={rs} ⇒ 选 {verdict}", flush=True)

out = os.path.join(ROOT, "raw", "method_ab.json")
json.dump({"ts": time.strftime("%Y-%m-%d %H:%M:%S"), "results": results}, open(out, "w"), ensure_ascii=False, indent=1)
print("saved:", out)
