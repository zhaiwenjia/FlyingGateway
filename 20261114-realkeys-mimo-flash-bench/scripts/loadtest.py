#!/usr/bin/env python3
# 日期: 2026-11-14 17:35:00.000
# 作者: Zhai Wenjia
"""mimo-v2.6-flash 高并发压测：FlyingGateway vs LiteLLM 同负载对比。

用法：/tmp/bench/bin/python scripts/loadtest.py --target gateway|litellm --concurrency 8 --requests 32
输出：raw/loadtest_<target>_c<concurrency>.json（逐请求延迟/状态 + 汇总统计）
⚠️ 只打本地被测服务；上游 key 经被测服务的环境注入，本脚本不接触明文 key。
"""
import argparse, asyncio, json, os, statistics, time

async def one(client, url, headers, payload, results, idx):
    t0 = time.perf_counter()
    ok, status, err = False, 0, ""
    try:
        async with client.post(url, headers=headers, json=payload) as r:
            status = r.status
            body = await r.text()
            ok = (status == 200 and '"choices"' in body)
            if not ok:
                err = body[:200]
    except Exception as e:
        err = repr(e)[:200]
    dt = time.perf_counter() - t0
    results.append({"i": idx, "ok": ok, "status": status, "latency_s": round(dt, 4), "err": err})

async def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--target", choices=["gateway", "litellm"], required=True)
    ap.add_argument("--concurrency", type=int, default=8)
    ap.add_argument("--requests", type=int, default=32)
    ap.add_argument("--model", default="mimo-v2.6-flash")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--tag", default="")
    ap.add_argument("--port", type=int, default=0)
    args = ap.parse_args()
    import aiohttp
    port = args.port or (8080 if args.target == "gateway" else 4000)
    url = f"http://{args.host}:{port}/v1/chat/completions"
    headers = {"Content-Type": "application/json"}
    if args.target == "gateway":
        headers["Authorization"] = "Bearer local-bench"   # 网关本地开放模式可不校验
    else:
        headers["Authorization"] = "Bearer sk-litellm-local-master"
    payload = {"model": args.model, "messages": [{"role": "user", "content": "用一句话回答：1+1=?"}],
               "max_tokens": 16, "temperature": 0}
    results, t0 = [], time.perf_counter()
    sem = asyncio.Semaphore(args.concurrency)
    async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=120)) as client:
        async def guarded(i):
            async with sem:
                await one(client, url, headers, payload, results, i)
        await asyncio.gather(*[guarded(i) for i in range(args.requests)])
    wall = time.perf_counter() - t0
    lat = sorted(r["latency_s"] for r in results if r["ok"])
    def pct(p):
        return round(lat[min(len(lat) - 1, int(len(lat) * p))] * 1000, 1) if lat else None
    summary = {
        "target": args.target, "model": args.model, "concurrency": args.concurrency,
        "requests": args.requests, "ok": sum(r["ok"] for r in results),
        "fail": sum(not r["ok"] for r in results),
        "wall_s": round(wall, 3),
        "throughput_rps": round(sum(r["ok"] for r in results) / wall, 2) if wall else 0,
        "latency_ms": {"p50": pct(0.50), "p90": pct(0.90), "p99": pct(0.99),
                       "mean": round(statistics.mean(lat) * 1000, 1) if lat else None,
                       "max": round(max(lat) * 1000, 1) if lat else None},
    }
    tag = (args.tag + "_") if args.tag else ""
    out = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "raw",
                       f"loadtest_{tag}{args.target}_c{args.concurrency}.json")
    json.dump({"ts": time.strftime("%Y-%m-%d %H:%M:%S"), "summary": summary, "results": results},
              open(out, "w"), ensure_ascii=False, indent=1)
    print(json.dumps(summary, ensure_ascii=False))
    print("saved:", out)

asyncio.run(main())
