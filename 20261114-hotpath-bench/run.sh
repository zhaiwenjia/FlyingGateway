#!/usr/bin/env bash
# 日期: 2026-11-14 19:39:55.271
# 作者: Zhai Wenjia
#
# 复现 W-optB 编解码/计量热路径基准。基线与优化后用同一命令、同一机器
# （darwin/arm64, go1.27.3, -10 表示 GOMAXPROCS=10），输出分别存
# bench_before.txt / bench_after.txt；对比口径取每项 5 次的中位数，
# 噪声带参考调研 3.1「±1-2% 是噪声不是结论」。
set -euo pipefail
export PATH="/opt/homebrew/bin:$PATH"
cd "$(dirname "$0")/.."
go test -bench=. -benchmem -count=5 -run='^$' \
  ./internal/usage ./internal/cost ./internal/errors ./internal/security \
  ./internal/codec/chat ./internal/codec/responses
