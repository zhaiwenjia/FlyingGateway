#!/usr/bin/env python3
# 日期: 2026-11-14 19:55:30.220
# 作者: Zhai Wenjia
"""ultra-concurrency-sim 编排器：真打 10 万级在途并发，FlyingGateway 与 LiteLLM 各档对比。

用法：
    python3 scripts/run_sim.py gateway            # 跑 FlyingGateway 全档
    python3 scripts/run_sim.py litellm            # 跑 LiteLLM 全档
    python3 scripts/run_sim.py gateway --tiers 1000,5000   # 只跑指定档

每档：起资源采样 → ultraload 打 -c 个按住 30s 的请求（源端口加宽 + 平缓爬坡）→
停采样落 raw/<target>-<tier>-res.json → 风暴后 100 个短请求恢复探针 → 冷却。
"""
import argparse
import json
import os
import signal
import subprocess
import sys
import time

SIM = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(SIM, "bin")
RAW = os.path.join(SIM, "raw")
RATES = os.path.join(SIM, "..", "config", "rates.json")
LISTEN = "127.0.0.1:9102,127.0.0.1:9103,127.0.0.1:9104,127.0.0.1:9105,127.0.0.1:9106,127.0.0.1:9107,127.0.0.1:9108,127.0.0.1:9109"
GATEWAY_URL = "http://127.0.0.1:8090"
LITELLM_URL = "http://127.0.0.1:8081"
ALL_TIERS = [1000, 5000, 10000, 25000, 50000, 100000]

os.makedirs(RAW, exist_ok=True)


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def rss_kb(pid):
    try:
        out = subprocess.check_output(["ps", "-o", "rss=", "-p", str(pid)], text=True)
        return int(out.strip())
    except Exception:
        return None


def fd_count(pid):
    try:
        out = subprocess.check_output(["lsof", "-p", str(pid)], stderr=subprocess.DEVNULL, text=True)
        return out.count("\n")
    except Exception:
        return None


class Sampler:
    """后台线程：每 rss_iv 采目标/上游 RSS，每 fd_iv 采 fd，落 JSONL。"""

    def __init__(self, targets, out_path, rss_iv=1.0, fd_iv=6.0):
        self.targets = targets  # dict name->pid
        self.out_path = out_path
        self.rss_iv = rss_iv
        self.fd_iv = fd_iv
        self._stop = False
        self._thread = None
        self.samples = []

    def start(self):
        import threading
        self._stop = False
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()

    def stop(self):
        self._stop = True
        if self._thread:
            self._thread.join(timeout=5)
        with open(self.out_path, "w") as f:
            for s in self.samples:
                f.write(json.dumps(s) + "\n")

    def _run(self):
        t0 = time.time()
        last_fd = -999
        while not self._stop:
            t = round(time.time() - t0, 2)
            rec = {"t": t}
            for name, pid in self.targets.items():
                if pid is None:
                    continue
                rec[name + "_rss_kb"] = rss_kb(pid)
                if t - last_fd >= self.fd_iv:
                    rec[name + "_fds"] = fd_count(pid)
            if any(k.endswith("_fds") for k in rec):
                last_fd = t
            self.samples.append(rec)
            time.sleep(self.rss_iv)


def start_upstream(hold):
    logf = open(os.path.join(RAW, "fakeupstream.log"), "a")
    p = subprocess.Popen([os.path.join(BIN, "fakeupstream"), "-listen", LISTEN, "-hold", str(hold)],
                         stdout=logf, stderr=subprocess.STDOUT)
    return p


def start_gateway():
    env = os.environ.copy()
    env["ULTRA_FAKE_KEY"] = "sk-ultra-fake"
    env.pop("GATEWAY_CALLER_KEYS", None)
    logf = open(os.path.join(RAW, "gateway.log"), "a")
    p = subprocess.Popen(
        [os.path.join(BIN, "gateway"), "-config", os.path.join(SIM, "gateway-ultra.json"),
         "-rates", RATES, "-listen", "127.0.0.1:8090", "-L",
         "--caller-bucket-capacity", "0", "--upstream-bucket-capacity", "0"],
        env=env, stdout=logf, stderr=subprocess.STDOUT)
    return p


def start_litellm():
    logf = open(os.path.join(RAW, "litellm.log"), "a")
    p = subprocess.Popen(
        ["/tmp/bench/bin/litellm", "--config", os.path.join(SIM, "litellm-ultra.yaml"),
         "--host", "127.0.0.1", "--port", "8081"],
        stdout=logf, stderr=subprocess.STDOUT)
    return p


def wait_http(url, timeout=30):
    import urllib.request
    import urllib.error
    end = time.time() + timeout
    probe = url.rstrip("/") + "/v1/chat/completions"
    while time.time() < end:
        try:
            req = urllib.request.Request(probe, data=b'{"model":"ultra-model","messages":[{"role":"user","content":"ultra-quick"}],"stream":false}',
                                         headers={"Content-Type": "application/json"}, method="POST")
            urllib.request.urlopen(req, timeout=5)
            return True
        except Exception:
            time.sleep(0.5)
    return False


def ramp_for(tier):
    # 爬坡速率：让 target/ramp ≈ 15s（< 30s hold 内达成峰值），钳在 [2000, 8000]。
    return max(2000, min(8000, tier // 15))


def sport_for(tier):
    # 客户端显式源端口：恒低于临时区间(49152+)，与网关出站临时端口池物理隔离。
    # 实测：客户端走临时端口会与网关出站抢全局临时池（16384）⇒ tier10000 假性端口耗尽；
    # 用显式非临时端口(20000-49151≈29k)后 tier10000 干净、tier25000 能推过 16384（网关真上限）。
    # 单源 IP 客户端干净上限 ≈ 29k；网关出站（排他 bind）上限 = 16384（临时区间）。
    return (20000, 49151)


def run_ultraload(target_url, tier, out, quick=False, dur=60, srcips=1):
    cmd = [os.path.join(BIN, "ultraload"), "-c", str(tier), "-d", f"{dur}s",
           "-url", target_url, "-ramp", str(ramp_for(tier)), "-srcips", str(srcips), "-o", out]
    sp = sport_for(tier)
    if sp and not quick:
        cmd += ["-sportlo", str(sp[0]), "-sporthi", str(sp[1])]
    if quick:
        cmd.append("-quick")
    log(f"  ultraload c={tier} quick={quick} ramp={ramp_for(tier)} sport={sp}")
    subprocess.run(cmd, stdout=open(os.path.join(RAW, "ultraload.last.log"), "w"), stderr=subprocess.STDOUT)
    try:
        with open(out) as f:
            return json.load(f)
    except Exception:
        return {}


def run_recovery(target_url, out):
    # 风暴后恢复力：100 个短请求，测 P50/P99（quick 非流式、不按住）。
    return run_ultraload(target_url, 100, out, quick=True, dur=30)


def kill_quiet(p):
    if p is None:
        return
    try:
        p.send_signal(signal.SIGTERM)
        p.wait(timeout=5)
    except Exception:
        try:
            p.kill()
        except Exception:
            pass


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("target", choices=["gateway", "litellm"])
    ap.add_argument("--tiers", default="", help="逗号分隔档位（默认全档）")
    ap.add_argument("--hold", type=int, default=30)
    ap.add_argument("--cooldown", type=int, default=60, help="档间冷却秒（清 TIME_WAIT）")
    args = ap.parse_args()

    tiers = [int(x) for x in args.tiers.split(",")] if args.tiers else ALL_TIERS
    target_url = GATEWAY_URL if args.target == "gateway" else LITELLM_URL

    log(f"起假上游 hold={args.hold}")
    up = start_upstream(args.hold)
    time.sleep(1)
    if args.target == "gateway":
        tgt = start_gateway()
    else:
        tgt = start_litellm()
    log(f"起 {args.target} pid={tgt.pid}，等就绪…")
    if not wait_http(target_url):
        log(f"⚠ {args.target} 未在超时内就绪，仍继续（记录失败）")

    for tier in tiers:
        log(f"=== {args.target} 档位 {tier} ===")
        res_out = os.path.join(RAW, f"{args.target}-{tier}-res.json")
        samp = Sampler({"tgt": tgt.pid, "up": up.pid}, res_out)
        samp.start()
        storm = run_ultraload(target_url, tier, os.path.join(RAW, f"{args.target}-{tier}.json"), dur=60)
        samp.stop()
        r = storm.get("result", {})
        log(f"  达成在途 peak={r.get('peak_inflight')} 完成={r.get('completed')} 失败={r.get('failed')} status={r.get('status')}")
        rec = run_recovery(target_url, os.path.join(RAW, f"{args.target}-{tier}-recover.json"))
        rr = rec.get("result", {})
        log(f"  恢复探针 100 短请求 ttfb_p50={rr.get('ttfb_ms', {}).get('p50')} p99={rr.get('ttfb_ms', {}).get('p99')}")
        if tier != tiers[-1]:
            log(f"  冷却 {args.cooldown}s（清 TIME_WAIT）")
            time.sleep(args.cooldown)

    kill_quiet(tgt)
    kill_quiet(up)
    log("完成 + 清理")


if __name__ == "__main__":
    main()
