#!/usr/bin/env python3
# 日期: 2026-11-14 20:58:18.185
# 作者: Zhai Wenjia
"""性能对比总结图（优化后实测）：专业术语、直述结论、版式克制。

图 1 吞吐量与延迟（受控基准：固定 300ms 上游延迟，2 轮取中位）
图 2 并发容量与单连接内存（1k→100k 在途压力实测）
"""
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

def style(ax):
    ax.spines[["top", "right"]].set_visible(False)
    ax.grid(axis="y", alpha=0.25, linewidth=0.8)
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

# 图 1：吞吐量与延迟
fig, axes = plt.subplots(1, 2, figsize=(12.5, 5))
grouped(axes[0], [1, 8, 32], [3.29, 26.2, 102.5], [3.14, 23.1, 69.4],
        "吞吐量（req/s，越高越好）")
axes[0].set_title("吞吐量对比", fontsize=14, fontweight="bold")
axes[0].text(0.5, -0.22, "32 并发时吞吐量高约 48%", transform=axes[0].transAxes,
             ha="center", fontsize=11, color="#333333")
grouped(axes[1], [1, 8, 32], [304, 304, 308], [314, 346, 432],
        "端到端延迟 P50（ms，越低越好）")
axes[1].set_title("延迟对比（P50）", fontsize=14, fontweight="bold")
axes[1].text(0.5, -0.22, "延迟贴近上游基线（300ms），附加开销 ≈4ms；LiteLLM 高并发时 +124ms",
             transform=axes[1].transAxes, ha="center", fontsize=11, color="#333333")
fig.suptitle("性能对比（优化后实测 · 固定 300ms 上游延迟 · 2 轮取中位）", fontsize=15, fontweight="bold")
fig.tight_layout(rect=[0, 0.04, 1, 0.95])
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
fig2.suptitle("容量与资源对比（1k→100k 在途压力实测 · 2026-11-14）", fontsize=15, fontweight="bold")
fig2.tight_layout(rect=[0, 0.04, 1, 0.95])
p2 = os.path.join(IMG, "20261114-perf-capacity-mem.png")
fig2.savefig(p2, dpi=130)
print("saved:", p1)
print("saved:", p2)
