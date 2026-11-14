#!/usr/bin/env python3
# 日期: 2026-11-14 18:55:10.442
# 作者: Zhai Wenjia
"""本地阶梯压测曲线图（matplotlib + ggplot 风格）：网关 vs 直连，延迟 P50/P99 + 吞吐 vs 并发。

用法：/tmp/bench/bin/python scripts/plot_sweep.py
输入：raw/local/sweep_summary.json（local_sweep.py -m sweep 的跨轮中位聚合）
输出：raw/local/20261114-sweep-curve.png
"""
import json, os
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib import font_manager

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
OUT = os.path.join(ROOT, "raw", "local")

data = json.load(open(os.path.join(OUT, "sweep_summary.json")))["aggregate"]
by = {(d["target"], d["concurrency"]): d for d in data}
concs = sorted({d["concurrency"] for d in data})
labels = {"gateway": "FlyingGateway（经网关）", "direct": "直连假上游"}
colors = {"gateway": "#4C72B0", "direct": "#DD8452"}
plt.style.use("ggplot")

fig, axes = plt.subplots(1, 2, figsize=(12.5, 4.8))

# 左图：延迟 P50/P99 vs 并发（对数横轴）
for t in ("direct", "gateway"):
    for pctl, ls in (("p50", "-"), ("p99", "--")):
        vals = [by[(t, c)]["latency_ms"][pctl] for c in concs]
        axes[0].plot(concs, vals, ls, marker="o", color=colors[t],
                     label=f"{labels[t]} {pctl.upper()}")
axes[0].set_xscale("log", base=2)
axes[0].set_xticks(concs)
axes[0].set_xticklabels([str(c) for c in concs])
axes[0].set_xlabel("并发数")
axes[0].set_ylabel("延迟（ms）")
axes[0].set_title("端到端延迟 P50 / P99（假上游固定 300ms）")
axes[0].legend(fontsize=9)

# 右图：吞吐 vs 并发
for t in ("direct", "gateway"):
    vals = [by[(t, c)]["throughput_rps"] for c in concs]
    axes[1].plot(concs, vals, "-o", color=colors[t], label=labels[t])
    for c, v in zip(concs, vals):
        axes[1].annotate(f"{v:.0f}", (c, v), textcoords="offset points", xytext=(0, 6),
                         ha="center", fontsize=8)
axes[1].set_xscale("log", base=2)
axes[1].set_xticks(concs)
axes[1].set_xticklabels([str(c) for c in concs])
axes[1].set_xlabel("并发数")
axes[1].set_ylabel("吞吐（req/s）")
axes[1].set_title("吞吐量 vs 并发（越高越好）")
axes[1].legend(fontsize=9)

rounds = max(d["rounds"] for d in data)
fig.suptitle(f"本地受控阶梯压测：FlyingGateway vs 直连假上游（每档 {rounds} 轮取中位，成功率见 sweep_summary.json）",
             fontsize=11)
fig.tight_layout(rect=[0, 0, 1, 0.93])
p = os.path.join(OUT, "20261114-sweep-curve.png")
fig.savefig(p, dpi=130)
print("saved:", p)
