#!/usr/bin/env python3
# 日期: 2026-11-14 17:30:00.000
# 作者: Zhai Wenjia
"""真实 key 探测：每家用【费率最低模型】发 1-token 最小请求，判定 可用/无余额/失效。

用法：/tmp/bench/bin/python scripts/probe_keys.py
数据源：worktree 根 .env（⚠️ 含真值不入库）；结果落 raw/keycheck.json。
"""
import json, os, re, sys, time, urllib.request, urllib.error

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
ENV = {}
for line in open(os.path.join(ROOT, ".env")):
    line = line.strip()
    if line and not line.startswith("#") and "=" in line:
        k, v = line.split("=", 1)
        ENV[k.strip()] = v.strip()

# (名, base_url, env 变量, 费率最低模型, 备选模型列表)
PROVIDERS = [
    ("OpenAI",    "https://api.openai.com/v1",                  "OPENAI_API_KEY",    "gpt-6-luna",      []),
    ("Qwen",      "https://dashscope.aliyuncs.com/compatible-mode/v1", "QWEN_API_KEY", "qwen3.8-flash", ["qwen-turbo"]),
    ("Zhipu",     "https://open.bigmodel.cn/api/paas/v4",       "ZHIPU_API_KEY",     "glm-5.3-flash",   ["glm-4-flash"]),
    ("DeepSeek",  "https://api.deepseek.com/v1",                "DEEPSEEK_API_KEY",  "deepseek-flash",  ["deepseek-chat"]),
    ("Moonshot",  "https://api.moonshot.cn/v1",                 "MOONSHOT_API_KEY",  "kimi-k3.1-pro",   ["moonshot-v1-8k"]),
    ("MiniMax",   "https://api.minimax.io/v1",                  "MINIMAX_API_KEY",   "MiniMax-M3.1-Flash", ["MiniMax-Text-01"]),
    ("MiMo",      "https://api.xiaomimimo.com/v1",              "MIMO_API_KEY",      "mimo-v2.6-flash", ["mimo-v2.6-pro"]),
    ("Volc",      "https://ark.cn-beijing.volces.com/api/v3",   "VOLC_API_KEY",      "",                []),
]

def http(method, url, key, body=None):
    req = urllib.request.Request(url, method=method)
    req.add_header("Authorization", "Bearer " + key)
    if body is not None:
        req.add_header("Content-Type", "application/json")
        data = json.dumps(body).encode()
    else:
        data = None
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, data=data, timeout=30) as r:
            return r.status, r.read().decode(errors="replace"), time.time() - t0
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace"), time.time() - t0
    except Exception as e:
        return -1, repr(e), time.time() - t0

def classify(status, body):
    b = body.lower()
    if status == 200:
        return "可用"
    if status in (401, 403) and any(w in b for w in ("invalid", "incorrect", "unauthorized", "authentication", "禁用", "失效")):
        return "失效/鉴权拒绝"
    if any(w in b for w in ("insufficient", "quota", "balance", "欠费", "余额", "exceeded")):
        return "无余额/配额"
    if status == 404:
        return "模型不存在（key 可能有效）"
    return f"其他（HTTP {status}）"

results = []
for name, base, envk, model, alts in PROVIDERS:
    key = ENV.get(envk, "")
    entry = {"provider": name, "env": envk, "base": base, "tried": []}
    if not key:
        entry["verdict"] = "缺 key"
        results.append(entry); continue
    # 先试 /models 发现可用模型（有的家不支持，失败不碍事）
    st, body, dt = http("GET", base + "/models", key)
    entry["models_list"] = {"status": st, "sample": body[:200], "latency_s": round(dt, 3)}
    models_avail = []
    if st == 200:
        try:
            models_avail = [m.get("id") for m in json.loads(body).get("data", [])]
            entry["models_found"] = models_avail[:30]
        except Exception:
            pass
    candidates = ([model] if model else []) + alts + [m for m in models_avail if m][:6]
    for m in dict.fromkeys(candidates):
        st, body, dt = http("POST", base + "/chat/completions", key, {
            "model": m, "messages": [{"role": "user", "content": "hi"}], "max_tokens": 1,
        })
        v = classify(st, body)
        entry["tried"].append({"model": m, "status": st, "verdict": v, "latency_s": round(dt, 3), "body": body[:300]})
        if st == 200 or "无余额" in v or "失效" in v:
            entry["verdict"] = v
            entry["model_used"] = m
            break
    else:
        entry["verdict"] = entry["tried"][-1]["verdict"] if entry["tried"] else "未测"
    results.append(entry)
    print(f"{name:10s} {entry.get('verdict','?')}  model={entry.get('model_used','-')}", flush=True)

out = os.path.join(ROOT, "20261114-realkeys-mimo-flash-bench", "raw", "keycheck.json")
json.dump({"ts": time.strftime("%Y-%m-%d %H:%M:%S"), "results": results}, open(out, "w"), ensure_ascii=False, indent=1)
print("saved:", out)
