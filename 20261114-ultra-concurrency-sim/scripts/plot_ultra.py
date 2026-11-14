#!/usr/bin/env python3
# 日期: 2026-11-14 20:02:10.880
# 作者: Zhai Wenjia
"""ultra-concurrency-sim 汇总 + 绘图（matplotlib + ggplot 风格，PingFang 字体）。

读 raw/ 下每档 storm/res/recover JSON，产出：
  - raw/summary.json（每 (target,tier) 一行汇总，README 表格数据源）
  - raw/20261114-ultra-*.png（图）+ docs/images/ 下副本
  - stdout 打印 markdown 数据表

用法：/private/tmp/bench/bin/python scripts/plot_ultra.py
"""
import glob
import json
import os

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib import font_manager
import numpy as np

for _fp in ["/System/Library/Fonts/PingFang.ttc", "/System/Library/Fonts/Hiragino Sans GB.ttc",
            "/System/Library/Fonts/STHeiti Light.ttc"]:
    if os.path.exists(_fp):
        try:
            font_manager.fontManager.addfont(_fp)
            plt.rcParams["font.family"] = font_manager.FontProperties(fname=_fp).get_name()
            plt.rcParams["axes.unicode_minus"] = False
            break
        except Exception:
            continue

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RAW = os.path.join(ROOT, "raw")
IMG = os.path.join(os.path.dirname(ROOT), "docs", "images")
os.makedirs(IMG, exist_ok=True)

TARGETS = ["gateway", "litellm"]
LABELS = {"gateway": "FlyingGateway", "litellm": "LiteLLM"}
COLORS = {"gateway": "#4C72B0", "litellm": "#DD8452"}
TIERS = [1000, 5000, 10000, 25000, 50000, 100000]
plt.style.use("ggplot")


def load(path):
    try:
        with open(path) as f:
            return json.load(f)
    except Exception:
        return None


def res_peak(path):
    """res.json JSONL：取 tgt_rss_kb / tgt_fds 的最大值（≈峰值在途成本）。"""
    rss = []
    fds = []
    try:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                r = json.loads(line)
                if r.get("tgt_rss_kb") is not None:
                    rss.append(r["tgt_rss_kb"])
                if r.get("tgt_fds") is not None:
                    fds.append(r["tgt_fds"])
    except Exception:
        pass
    return (max(rss) if rss else None, max(fds) if fds else None)


def build():
    rows = {}
    for t in TARGETS:
        for tier in TIERS:
            storm = load(os.path.join(RAW, f"{t}-{tier}.json"))
            if not storm:
                continue
            r = storm["result"]
            rss, fds = res_peak(os.path.join(RAW, f"{t}-{tier}-res.json"))
            rec = load(os.path.join(RAW, f"{t}-{tier}-recover.json"))
            rr = rec["result"] if rec else {}
            rows[(t, tier)] = {
                "target": t, "tier": tier,
                "achieved": r.get("status", {}).get("200", 0),  # 成功在途（真正被代理的）
                "peak": r.get("peak_inflight", 0),              # 客户端峰值在途（含拒绝中）
                "launched": r.get("launched", 0),
                "completed": r.get("completed", 0),
                "failed": r.get("failed", 0),
                "rej_502": r.get("status", {}).get("502", 0),
                "status": r.get("status", {}),
                "errors": r.get("errors", {}),
                "ttfb_p50": r.get("ttfb_ms", {}).get("p50"),
                "ttfb_p99": r.get("ttfb_ms", {}).get("p99"),
                "total_p50": r.get("total_ms", {}).get("p50"),
                "rss_kb": rss, "fds": fds,
                "rec_ttfb_p50": rr.get("ttfb_ms", {}).get("p50"),
                "rec_ttfb_p99": rr.get("ttfb_ms", {}).get("p99"),
            }
    return rows


def md_table(rows):
    hdr = "| 目标 | 档 | 成功在途(200) | 峰值在途 | 502拒绝 | 客户端失败 | 成功率 | TTFB P50 | TTFB P99 | RSS(MB) | fd | 恢复P50 | 恢复P99 |"
    sep = "|---|---|---|---|---|---|---|---|---|---|---|---|---|"
    out = [hdr, sep]
    for t in TARGETS:
        for tier in TIERS:
            r = rows.get((t, tier))
            if not r:
                continue
            succ = 100.0 * r["achieved"] / r["launched"] if r["launched"] else 0
            rss_mb = r["rss_kb"] / 1024 if r["rss_kb"] else 0
            out.append("| {} | {} | {} | {} | {} | {} | {:.1f}% | {} | {} | {:.0f} | {} | {} | {} |".format(
                LABELS[t], tier, r["achieved"], r["peak"], r["rej_502"], r["failed"], succ,
                _f(r["ttfb_p50"]), _f(r["ttfb_p99"]), rss_mb,
                r["fds"] if r["fds"] is not None else "-",
                _f(r["rec_ttfb_p50"]), _f(r["rec_ttfb_p99"])))
    return "\n".join(out)


def _f(x):
    return "{:.1f}".format(x) if isinstance(x, (int, float)) else "-"


def series(rows, t, key):
    xs, ys = [], []
    for tier in TIERS:
        r = rows.get((t, tier))
        if r and r.get(key) is not None:
            xs.append(tier)
            ys.append(r[key])
    return xs, ys


def plot(rows):
    # 图 1：达成在途 vs 目标档（谁能压到多少）。
    fig, ax = plt.subplots(figsize=(8, 5))
    x = np.arange(len(TIERS))
    w = 0.36
    for i, t in enumerate(TARGETS):
        vals = [rows.get((t, tier), {}).get("achieved", 0) for tier in TIERS]
        ax.bar(x + (i - 0.5) * w, vals, w, label=LABELS[t], color=COLORS[t], edgecolor="white")
    ax.set_xticks(x)
    ax.set_xticklabels([f"{tier//1000}k" if tier >= 1000 else str(tier) for tier in TIERS])
    ax.set_xlabel("目标在途档")
    ax.set_ylabel("实际达成峰值在途")
    ax.set_title("各档达成在途（假上游按住 30s，等价模拟）")
    ax.legend()
    fig.tight_layout()
    _save(fig, "20261114-ultra-achieved")

    # 图 2：RSS 成本 vs 达成在途（每 1k 成本）。
    fig, ax = plt.subplots(figsize=(8, 5))
    for t in TARGETS:
        xs, ys = series(rows, t, "achieved")
        _, rss = series(rows, t, "rss_kb")
        ys_mb = [r / 1024 for r in rss]
        ax.plot(xs, ys_mb, "o-", label=LABELS[t], color=COLORS[t])
    ax.set_xlabel("达成峰值在途")
    ax.set_ylabel("目标进程 RSS (MB)")
    ax.set_title("RSS 随在途并发增长（每 1k 在途成本）")
    ax.legend()
    fig.tight_layout()
    _save(fig, "20261114-ultra-rss")

    # 图 3：fd 成本 vs 达成在途。
    fig, ax = plt.subplots(figsize=(8, 5))
    for t in TARGETS:
        xs, ys = series(rows, t, "achieved")
        _, fds = series(rows, t, "fds")
        ax.plot(xs, fds, "o-", label=LABELS[t], color=COLORS[t])
    ax.set_xlabel("达成峰值在途")
    ax.set_ylabel("目标进程 fd 数")
    ax.set_title("fd 随在途并发增长（网关≈2 fd/连接：1 入站 + 1 出站）")
    ax.legend()
    fig.tight_layout()
    _save(fig, "20261114-ultra-fd")

    # 图 4：风暴中 TTFB（首字节转发开销）。
    fig, ax = plt.subplots(figsize=(8, 5))
    for t in TARGETS:
        xs, ys = series(rows, t, "ttfb_p50")
        _, ys99 = series(rows, t, "ttfb_p99")
        ax.plot(xs, ys, "o-", label=LABELS[t] + " P50", color=COLORS[t])
        ax.plot(xs, ys99, "s--", label=LABELS[t] + " P99", color=COLORS[t], alpha=0.6)
    ax.set_xlabel("目标在途档")
    ax.set_ylabel("TTFB (ms)")
    ax.set_title("风暴中 TTFB（首字节，反映转发开销）")
    ax.legend()
    fig.tight_layout()
    _save(fig, "20261114-ultra-ttfb")

    # 图 5：风暴后恢复力（100 短请求 TTFB）。
    fig, ax = plt.subplots(figsize=(8, 5))
    for t in TARGETS:
        xs, ys = series(rows, t, "rec_ttfb_p50")
        _, ys99 = series(rows, t, "rec_ttfb_p99")
        ax.plot(xs, ys, "o-", label=LABELS[t] + " P50", color=COLORS[t])
        ax.plot(xs, ys99, "s--", label=LABELS[t] + " P99", color=COLORS[t], alpha=0.6)
    ax.set_xlabel("刚经历的风暴档")
    ax.set_ylabel("恢复探针 TTFB (ms)")
    ax.set_title("风暴后恢复力（100 短请求 P50/P99）")
    ax.legend()
    fig.tight_layout()
    _save(fig, "20261114-ultra-recovery")


def _save(fig, name):
    for d in (RAW, IMG):
        fig.savefig(os.path.join(d, name + ".png"), dpi=110)
    plt.close(fig)
    print("图 →", name + ".png")


def main():
    rows = build()
    with open(os.path.join(RAW, "summary.json"), "w") as f:
        json.dump({f"{k[0]}-{k[1]}": v for k, v in rows.items()}, f, ensure_ascii=False, indent=2)
    print("汇总 → raw/summary.json\n")
    print(md_table(rows))
    print()
    plot(rows)


if __name__ == "__main__":
    main()
