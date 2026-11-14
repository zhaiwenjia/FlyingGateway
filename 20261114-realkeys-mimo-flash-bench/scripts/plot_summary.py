#!/usr/bin/env python3
# 日期: 2026-11-14 20:58:18.185
# 作者: Zhai Wenjia
"""性能对比总结图（优化后实测）：专业术语、直述结论、版式克制。

图 1 吞吐量/延迟 vs 并发（受控基准：固定 300ms 上游延迟，闭合式 1→16384 指数阶梯，2 轮取中位）
图 2 并发容量与单连接内存（1k→100k 在途压力实测）
"""
import glob
import json
import os
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib import font_manager
import numpy as np

# CJK 字体候选（本机 macOS 27 无 PingFang.ttc，实测 STHeiti/Arial Unicode 存在）
for _fp in ["/System/Library/Fonts/STHeiti Light.ttc",
            "/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
            "/Library/Fonts/Arial Unicode.ttf",
            "/System/Library/Fonts/Hiragino Sans GB.ttc",
            "/System/Library/Fonts/PingFang.ttc"]:
    if os.path.exists(_fp):
        font_manager.fontManager.addfont(_fp)
        plt.rcParams["font.family"] = font_manager.FontProperties(fname=_fp).get_name()
        break
plt.rcParams["axes.unicode_minus"] = False

GW, LL = "#4C72B0", "#DD8452"
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
IMG = os.path.join(os.path.dirname(ROOT), "docs", "images")
LADDER = os.path.join(ROOT, "raw", "ladder")

def style(ax):
    ax.spines[["top", "right"]].set_visible(False)
    ax.grid(alpha=0.25, linewidth=0.8)
    ax.set_axisbelow(True)

def grouped(ax, xs, gw, ll, ylabel, fmt="{:g}"):
    x = np.arange(len(xs)); w = 0.34
    for i, (vals, c, name) in enumerate([(gw, GW, "FlyingGateway"), (ll, LL, "LiteLLM")]):
        bs = ax.bar(x + (i - 0.5) * w, vals, w, color=c, label=name, edgecolor="white")
        for b, v in zip(bs, vals):
            ax.annotate(fmt.format(v), (b.get_x() + b.get_width() / 2, v),
                        ha="center", va="bottom", fontsize=11)
    ax.set_xticks(x)
    ax.set_xticklabels([str(v) for v in xs])
    ax.set_xlabel("并发请求数")
    ax.set_ylabel(ylabel)
    ax.legend(fontsize=11)
    ax.set_ylim(0, max(max(gw), max(ll)) * 1.28)
    style(ax)

def load_ladder():
    """raw/ladder/{tag}_{target}_c{c}.json → {target: {c: [每轮摘要]}}。
    各指标取跨轮中位（图注口径「2 轮取中位」）。"""
    data = {}
    for f in sorted(glob.glob(os.path.join(LADDER, "*_c*.json"))):
        name = os.path.basename(f)[:-len(".json")]
        tag, target, c = name.split("_")
        c = int(c[1:])
        s = json.load(open(f))["summary"]
        data.setdefault(target, {}).setdefault(c, []).append(s)
    return data

def median_of(recs, getter):
    vals = [getter(r) for r in recs]
    vals = [v for v in vals if v is not None]
    return float(np.median(vals)) if vals else float("nan")

lv = [1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384]
lad = load_ladder()
series = {}
for target in ("gateway", "litellm"):
    rps = [median_of(lad.get(target, {}).get(c, []), lambda r: r["throughput_rps"]) for c in lv]
    p50 = [median_of(lad.get(target, {}).get(c, []), lambda r: r["latency_ms"]["p50"]) for c in lv]
    series[target] = {"rps": rps, "p50": p50}

gw, ll = series["gateway"], series["litellm"]

# 图 1：吞吐量/延迟 vs 并发（折线，log2 横轴）
fig, axes = plt.subplots(1, 2, figsize=(12.5, 5))
for ax, key, ylabel in ((axes[0], "rps", "吞吐量（req/s，越高越好）"),
                        (axes[1], "p50", "端到端延迟 P50（ms，越低越好）")):
    ax.plot(lv, gw[key], marker="o", ms=5, color=GW, label="FlyingGateway")
    ax.plot(lv, ll[key], marker="s", ms=5, color=LL, label="LiteLLM")
    ax.set_xscale("log", base=2)
    ax.set_xticks(lv)
    ax.set_xticklabels([str(v) for v in lv], fontsize=9)
    ax.set_xlabel("并发请求数（log2）")
    ax.set_ylabel(ylabel)
    ax.legend(fontsize=11)
    style(ax)

axes[0].set_title("吞吐量对比", fontsize=14, fontweight="bold")
gw_peak, ll_peak = max(gw["rps"]), max(ll["rps"])
axes[0].annotate(f"峰值 {gw_peak:.0f}", (lv[gw["rps"].index(gw_peak)], gw_peak),
                 textcoords="offset points", xytext=(0, 8), ha="center", fontsize=10, color=GW)
axes[0].annotate(f"峰值 {ll_peak:.0f}", (lv[ll["rps"].index(ll_peak)], ll_peak),
                 textcoords="offset points", xytext=(0, 8), ha="center", fontsize=10, color=LL)
axes[0].text(0.5, -0.22, f"峰值吞吐量约为 LiteLLM 的 {gw_peak / ll_peak:.1f}×；LiteLLM 超出容量后断连丢请求、吞吐塌落",
             transform=axes[0].transAxes, ha="center", fontsize=11, color="#333333")

axes[1].set_title("延迟对比（P50）", fontsize=14, fontweight="bold")
axes[1].axhline(300, color="#999999", linewidth=1, linestyle="--")
axes[1].text(lv[0], 300, " 上游基线 300ms", fontsize=9, color="#777777", va="bottom")
axes[1].text(0.5, -0.22, f"FlyingGateway 全程贴近基线（最高档附加 ≈{max(gw['p50']) - 300:.0f}ms）；"
                         f"LiteLLM 高并发攀升至 {max(v for v in ll['p50'] if v == v):.0f}ms",
             transform=axes[1].transAxes, ha="center", fontsize=11, color="#333333")
fig.suptitle("吞吐量 / 延迟 vs 并发（固定 300ms 上游延迟 · 闭合式 1→16384 指数阶梯 · 2 轮取中位）",
             fontsize=14, fontweight="bold")
fig.tight_layout(rect=[0, 0.04, 1, 0.94])
p1 = os.path.join(IMG, "20261114-perf-throughput-latency.png")
fig.savefig(p1, dpi=130)

# 图 2：并发容量与内存
fig2, axes2 = plt.subplots(1, 2, figsize=(12.5, 5))
grouped(axes2[0], ["最大并发容量"], [16500], [2000], "成功并发连接数", fmt="{:.0f}")
axes2[0].set_title("最大并发容量（在途请求压力测试）", fontsize=14, fontweight="bold")
axes2[0].set_xlabel("")
axes2[0].text(0.5, -0.16, "超限时 FlyingGateway 返回 502 优雅拒绝；LiteLLM 断连丢请求",
              transform=axes2[0].transAxes, ha="center", fontsize=11, color="#333333")
grouped(axes2[1], ["单连接内存开销"], [146], [450], "KB / 连接", fmt="{:.0f}")
axes2[1].set_title("单连接内存开销", fontsize=14, fontweight="bold")
axes2[1].set_xlabel("")
axes2[1].text(0.5, -0.16, "同等硬件资源下容量相差约 3 倍；过载恢复 1.5ms vs 4.9s",
              transform=axes2[1].transAxes, ha="center", fontsize=11, color="#333333")
fig2.suptitle("容量与资源对比（1k→100k 在途压力实测）", fontsize=15, fontweight="bold")
fig2.tight_layout(rect=[0, 0.04, 1, 0.95])
p2 = os.path.join(IMG, "20261114-perf-capacity-mem.png")
fig2.savefig(p2, dpi=130)
print("saved:", p1)
print("saved:", p2)
