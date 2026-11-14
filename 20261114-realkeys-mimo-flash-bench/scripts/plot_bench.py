#!/usr/bin/env python3
# 日期: 2026-11-14 17:55:00.000
# 作者: Zhai Wenjia
"""压测对比柱状图（matplotlib + ggplot 风格）：FlyingGateway vs LiteLLM。

用法：/tmp/bench/bin/python scripts/plot_bench.py
输出：../../docs/images/20261114-bench-*.png（README 引用）+ raw/ 下副本
"""
import json, glob, os
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib import font_manager
import numpy as np

# macOS 自带中文字体（DejaVu 无 CJK，标题会成方块）；逐个尝试，失败回落英文标注
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

# 跨轮聚合：每 (target, conc) 取各轮中位数（抗上游尾部波动；轮次见文件名 tag）
rounds = {}  # (target, conc) -> [summary, ...]
for f in sorted(glob.glob(os.path.join(RAW, "loadtest_*.json"))):
    j = json.load(open(f))
    sm = j["summary"]
    rounds.setdefault((sm["target"], sm["concurrency"]), []).append(sm)

def median(vals):
    v = sorted(vals)
    n = len(v)
    return v[n // 2] if n % 2 else (v[n // 2 - 1] + v[n // 2]) / 2

data = {}  # target -> {conc: summary(中位聚合)}
for (t, c), lst in rounds.items():
    agg = {"target": t, "concurrency": c,
           "requests": lst[0]["requests"], "rounds": len(lst),
           "ok": int(median([x["ok"] for x in lst])),
           "fail": int(median([x["fail"] for x in lst])),
           "wall_s": median([x["wall_s"] for x in lst]),
           "throughput_rps": median([x["throughput_rps"] for x in lst]),
           "latency_ms": {k: median([x["latency_ms"][k] for x in lst]) for k in ("p50", "p90", "p99", "mean", "max")}}
    data.setdefault(t, {})[c] = agg
nr = max(len(v) for v in rounds.values())
print(f"聚合 {nr} 轮（每格取中位）")

concs = [1, 8, 32]
targets = ["gateway", "litellm"]
labels = {"gateway": "FlyingGateway", "litellm": "LiteLLM"}
colors = {"gateway": "#4C72B0", "litellm": "#DD8452"}

plt.style.use("ggplot")

def grouped_bars(ax, get_vals, title, ylabel, fmt="{:.1f}"):
    x = np.arange(len(concs))
    w = 0.36
    for i, t in enumerate(targets):
        vals = [get_vals(data[t][c]) for c in concs]
        bars = ax.bar(x + (i - 0.5) * w, vals, w, label=labels[t], color=colors[t], edgecolor="white")
        for b, v in zip(bars, vals):
            ax.annotate(fmt.format(v), (b.get_x() + b.get_width() / 2, v), ha="center", va="bottom", fontsize=9)
    ax.set_xticks(x)
    ax.set_xticklabels([f"并发 {c}" for c in concs])
    ax.set_title(title, fontsize=12)
    ax.set_ylabel(ylabel)
    ax.legend()

# 成功率不作图：验收口径恒 100%（任何非 100% 按 bug 修，修后复测归 100%）
fig, axes = plt.subplots(1, 3, figsize=(13, 4.4))
grouped_bars(axes[0], lambda s: s["latency_ms"]["p50"], "P50 延迟（越低越好）", "ms")
grouped_bars(axes[1], lambda s: s["latency_ms"]["p99"], "P99 延迟（越低越好）", "ms")
grouped_bars(axes[2], lambda s: s["throughput_rps"], "吞吐量（越高越好）", "req/s", "{:.2f}")
fig.suptitle(f"mimo-v2.6-flash 高并发压测：FlyingGateway vs LiteLLM（同负载，每档 {nr} 轮取中位；成功率恒 100% 不作图）", fontsize=12)
fig.tight_layout(rect=[0, 0, 1, 0.93])
for name in ["bench-overview", "bench-latency"]:
    pass
p = os.path.join(IMG, "20261114-bench-overview.png")
fig.savefig(p, dpi=130)
fig.savefig(os.path.join(RAW, "20261114-bench-overview.png"), dpi=130)
print("saved:", p)

# 单独延迟图（README 主图：p50/p99 同图分组）
fig2, ax = plt.subplots(figsize=(9, 5))
x = np.arange(len(concs)); w = 0.2
series = [("gateway", "p50"), ("gateway", "p99"), ("litellm", "p50"), ("litellm", "p99")]
cols = ["#4C72B0", "#8DA0CB", "#DD8452", "#E0A374"]
for i, ((t, pctl), c) in enumerate(zip(series, cols)):
    vals = [data[t][cc]["latency_ms"][pctl] for cc in concs]
    bars = ax.bar(x + (i - 1.5) * w, vals, w, label=f"{labels[t]} {pctl.upper()}", color=c, edgecolor="white")
    for b, v in zip(bars, vals):
        ax.annotate(f"{v:.0f}", (b.get_x() + b.get_width() / 2, v), ha="center", va="bottom", fontsize=8)
ax.set_xticks(x); ax.set_xticklabels([f"并发 {c}" for c in concs])
ax.set_ylabel("ms"); ax.set_title("端到端延迟分位对比（mimo-v2.6-flash）", fontsize=12)
ax.legend()
fig2.tight_layout()
p2 = os.path.join(IMG, "20261114-bench-latency.png")
fig2.savefig(p2, dpi=130)
fig2.savefig(os.path.join(RAW, "20261114-bench-latency.png"), dpi=130)
print("saved:", p2)

# 汇总 CSV（原始数据留档）
csv = ["target,concurrency,rounds,requests,ok,fail,wall_s,throughput_rps,p50_ms,p90_ms,p99_ms,mean_ms,max_ms"]
for t in targets:
    for c in concs:
        s = data[t][c]
        l = s["latency_ms"]
        csv.append(f"{t},{c},{nr},{s['requests']},{s['ok']},{s['fail']},{s['wall_s']},{s['throughput_rps']},{l['p50']},{l['p90']},{l['p99']},{l['mean']},{l['max']}")
open(os.path.join(RAW, "bench-summary.csv"), "w").write("\n".join(csv) + "\n")
print("saved: raw/bench-summary.csv")
