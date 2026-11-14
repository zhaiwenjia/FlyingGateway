#!/usr/bin/env python3
# 日期: 2026-11-14 18:49:52.071
# 作者: Zhai Wenjia
"""本地假上游受控压测：并发阶梯（网关 vs 直连）+ pprof 热点画像采集。

用法（/tmp/bench/bin/python，须有 go 在 PATH 里用于构建与 go tool pprof）：
  scripts/local_sweep.py -m sweep      # 阶梯压测：c=1/8/32/64/128/256，每档 --rounds 轮取中位
  scripts/local_sweep.py -m profile    # c=64/c=256 压测期间抓 CPU/heap/allocs profile + pprof -top
  scripts/local_sweep.py -m all        # 先 sweep 后 profile

被测链路：loadgen（本机）→ FlyingGateway（127.0.0.1:18080）→ 假上游（127.0.0.1:9099，固定 300ms）。
对照组：loadgen → 假上游直连（同负载同端点）。
输出：raw/local/sweep_<target>_c<并发>_r<轮>.json（逐请求延迟/状态 + 汇总）、
      raw/local/sweep_summary.json（跨轮中位聚合）、
      raw/local/pprof/c<并发>/{cpu,heap,allocs}.pprof + *.top.txt、
      raw/local/gateway_stderr.log（网关日志留档）。

⚠️ 网关限流桶显式调大（-C/-R/-K/-E/-U/-u = 1e8）：本压测测的是数据面吞吐与延迟，
   ⛔ 不让令牌桶把吞吐钳在补充速率上（300ms 固定延迟 × 高并发下 token 消耗 ≫ 默认 10k/s 补充）。
   限流检查本身仍在热路径上执行（policy 环节归因不受影响）。
⚠️ 单机同跑 loadgen / 网关 / 假上游三个进程：绝对值供画像与对照用，不代表生产容量。
"""
import argparse, asyncio, json, os, statistics, subprocess, sys, threading, time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPO = os.path.dirname(ROOT)
OUT = os.path.join(ROOT, "raw", "local")
BIN = os.path.join(ROOT, "bin", "gateway-local")
CFG = os.path.join(OUT, "gateway-local.json")
RATES = os.path.join(REPO, "config", "rates.json")
UPSTREAM = "http://127.0.0.1:9099/v1/chat/completions"
GATEWAY = "http://127.0.0.1:18080/v1/chat/completions"
PAYLOAD = {"model": "mimo-v2.6-flash",
           "messages": [{"role": "user", "content": "用一句话回答：1+1=?"}],
           "max_tokens": 16, "temperature": 0}


def build_gateway():
    os.makedirs(os.path.dirname(BIN), exist_ok=True)
    subprocess.run(["go", "build", "-o", BIN, "./cmd/gateway"], cwd=REPO, check=True)
    print("built:", BIN)


def start_upstream(latency):
    return subprocess.Popen(
        [sys.executable, os.path.join(ROOT, "scripts", "fake_upstream.py"),
         "--port", "9099", "--latency", str(latency)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def start_gateway():
    env = dict(os.environ)
    env["LOCAL_FAKE_UPSTREAM_KEY"] = "local-bench-fake-key"
    log = open(os.path.join(OUT, "gateway_stderr.log"), "a")
    log.write(f"\n===== gateway start {time.strftime('%Y-%m-%d %H:%M:%S')} =====\n")
    log.flush()
    return subprocess.Popen(
        [BIN, "-c", CFG, "-r", RATES, "-l", "127.0.0.1:18080", "-L",
         "-p", "127.0.0.1:6060",
         "-C", "100000000", "-R", "100000000", "-K", "100000000",
         "-E", "100000000", "-U", "100000000", "-u", "100000000"],
        stdout=log, stderr=log, env=env)


def wait_port(port, timeout=10.0):
    import socket
    t0 = time.perf_counter()
    while time.perf_counter() - t0 < timeout:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                return True
        except OSError:
            time.sleep(0.1)
    raise RuntimeError(f"端口 {port} {timeout}s 内未就绪")


async def load_run(url, headers, concurrency, requests=None, duration_s=None):
    """定并发跑一批（requests 条）或跑一段（duration_s 秒），返回 (summary, results)。"""
    import aiohttp
    results, done = [], {"n": 0}
    sem = asyncio.Semaphore(concurrency)
    t0 = time.perf_counter()
    stop = threading.Event()

    async def one(client, idx):
        async with sem:
            t1 = time.perf_counter()
            ok, status, err = False, 0, ""
            try:
                async with client.post(url, headers=headers, json=PAYLOAD) as r:
                    status = r.status
                    body = await r.text()
                    ok = (status == 200 and '"choices"' in body)
                    if not ok:
                        err = body[:200]
            except Exception as e:
                err = repr(e)[:200]
            results.append({"i": idx, "ok": ok, "status": status,
                            "latency_s": round(time.perf_counter() - t1, 4), "err": err})

    async def worker(client, widx):
        i = widx
        while not stop.is_set() and (requests is None or done["n"] < requests):
            done["n"] += 1
            await one(client, i)
            i += 1

    # limit=0：⛔ 不用 aiohttp 默认连接池（100）—— 高并发档会被连接池钳住而失真。
    conn = aiohttp.TCPConnector(limit=0, limit_per_host=0)
    async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=120), connector=conn) as client:
        if requests is not None:
            await asyncio.gather(*[worker(client, w) for w in range(concurrency)])
        else:
            tasks = [asyncio.ensure_future(worker(client, w)) for w in range(concurrency)]
            await asyncio.sleep(duration_s)
            stop.set()
            await asyncio.gather(*tasks)
    wall = time.perf_counter() - t0
    lat = sorted(r["latency_s"] for r in results if r["ok"])

    def pct(p):
        return round(lat[min(len(lat) - 1, int(len(lat) * p))] * 1000, 1) if lat else None

    summary = {
        "target": "gateway" if url == GATEWAY else "direct",
        "model": PAYLOAD["model"], "concurrency": concurrency,
        "requests": len(results), "mode": "count" if requests is not None else "duration",
        "ok": sum(r["ok"] for r in results), "fail": sum(not r["ok"] for r in results),
        "wall_s": round(wall, 3),
        "throughput_rps": round(sum(r["ok"] for r in results) / wall, 2) if wall else 0,
        "latency_ms": {"p50": pct(0.50), "p90": pct(0.90), "p99": pct(0.99),
                       "mean": round(statistics.mean(lat) * 1000, 1) if lat else None,
                       "max": round(max(lat) * 1000, 1) if lat else None},
    }
    return summary, results


def save(name, obj):
    p = os.path.join(OUT, name)
    json.dump(obj, open(p, "w"), ensure_ascii=False, indent=1)
    print("saved:", p)


def median(vals):
    v = sorted(vals)
    n = len(v)
    return v[n // 2] if n % 2 else (v[n // 2 - 1] + v[n // 2]) / 2


def run_sweep(concs, rounds):
    for url in (GATEWAY, UPSTREAM):  # 预热（结果不入档）
        asyncio.run(load_run(url, {"Authorization": "Bearer local-bench"}, 8, requests=32))
    for r in range(1, rounds + 1):
        for c in concs:
            for url in (GATEWAY, UPSTREAM):
                sm, res = asyncio.run(load_run(url, {"Authorization": "Bearer local-bench"}, c, requests=c * 8))
                save(f"sweep_{sm['target']}_c{c}_r{r}.json",
                     {"ts": time.strftime("%Y-%m-%d %H:%M:%S"), "summary": sm, "results": res})
                print(json.dumps(sm, ensure_ascii=False))
    # 跨轮中位聚合
    agg = []
    for c in concs:
        for t in ("gateway", "direct"):
            lst = []
            for r in range(1, rounds + 1):
                lst.append(json.load(open(os.path.join(OUT, f"sweep_{t}_c{c}_r{r}.json")))["summary"])
            agg.append({
                "target": t, "concurrency": c, "rounds": len(lst),
                "ok": int(median([x["ok"] for x in lst])),
                "fail": int(median([x["fail"] for x in lst])),
                "wall_s": median([x["wall_s"] for x in lst]),
                "throughput_rps": median([x["throughput_rps"] for x in lst]),
                "latency_ms": {k: median([x["latency_ms"][k] for x in lst])
                               for k in ("p50", "p90", "p99", "mean", "max")},
            })
    save("sweep_summary.json", {"ts": time.strftime("%Y-%m-%d %H:%M:%S"), "aggregate": agg})


def pprof_top(name, prof, sample_index=None):
    cmd = ["go", "tool", "pprof", "-top", "-nodecount=60"]
    if sample_index:
        cmd += ["-sample_index=" + sample_index]
    cmd.append(prof)
    txt = subprocess.run(cmd, capture_output=True, text=True, check=True).stdout
    out = prof.replace(".pprof", f".{name}.top.txt")
    open(out, "w").write(txt)
    print("saved:", out)


def run_profile(concs, seconds):
    import requests
    for c in concs:
        d = os.path.join(OUT, "pprof", f"c{c}")
        os.makedirs(d, exist_ok=True)
        stop_flag = threading.Event()
        load_box = {}

        def load():
            summary, results = asyncio.run(load_run(GATEWAY, {"Authorization": "Bearer local-bench"},
                                                    c, duration_s=seconds + 5))
            load_box["summary"], load_box["results"] = summary, results

        th = threading.Thread(target=load)
        th.start()
        time.sleep(2)  # 等稳态
        t_start = time.strftime("%Y-%m-%d %H:%M:%S")
        base = "http://127.0.0.1:6060/debug/pprof"
        cpu = requests.get(f"{base}/profile?seconds={seconds}", timeout=seconds + 60)
        open(os.path.join(d, "cpu.pprof"), "wb").write(cpu.content)
        heap = requests.get(f"{base}/heap", timeout=30)
        open(os.path.join(d, "heap.pprof"), "wb").write(heap.content)
        allocs = requests.get(f"{base}/allocs", timeout=30)
        open(os.path.join(d, "allocs.pprof"), "wb").write(allocs.content)
        th.join()
        stop_flag.set()
        save(f"sweep_profile_c{c}.json", {"ts": t_start, "profile_seconds": seconds,
                                          "summary": load_box["summary"], "results": load_box["results"]})
        pprof_top("cpu", os.path.join(d, "cpu.pprof"))
        pprof_top("alloc_space", os.path.join(d, "allocs.pprof"), "alloc_space")
        pprof_top("alloc_objects", os.path.join(d, "allocs.pprof"), "alloc_objects")
        pprof_top("inuse_space", os.path.join(d, "heap.pprof"), "inuse_space")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", "-m", choices=["sweep", "profile", "all"], default="all")
    ap.add_argument("--concs", "-C", default="1,8,32,64,128,256")
    ap.add_argument("--rounds", "-R", type=int, default=2)
    ap.add_argument("--latency", "-t", type=float, default=0.3)
    ap.add_argument("--profile-seconds", "-s", type=int, default=30)
    ap.add_argument("--profile-concs", "-P", default="64,256")
    args = ap.parse_args()
    os.makedirs(OUT, exist_ok=True)
    build_gateway()
    up = start_upstream(args.latency)
    gw = start_gateway()
    try:
        wait_port(9099)
        wait_port(18080)
        if args.mode in ("sweep", "all"):
            run_sweep([int(x) for x in args.concs.split(",")], args.rounds)
        if args.mode in ("profile", "all"):
            run_profile([int(x) for x in args.profile_concs.split(",")], args.profile_seconds)
    finally:
        gw.terminate()
        up.terminate()
        gw.wait(timeout=5)
        up.wait(timeout=5)


if __name__ == "__main__":
    main()
