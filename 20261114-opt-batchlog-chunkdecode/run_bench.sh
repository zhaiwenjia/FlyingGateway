#!/bin/bash
# 复现本轮基准与验证（Apple M1 Max / go1.27.3；逐条命令与 raw/ 日志一一对应）。
set -euo pipefail
export PATH="/opt/homebrew/bin:$PATH"
cd "$(dirname "$0")/.."

# 1) 全量门禁
gofmt -l .
go vet ./...
go test -race -count=1 ./... | tee 20261114-opt-batchlog-chunkdecode/raw/test_full_after.log

# 2) chunk 解码 bench（真实样本 + 构造 chunk，-benchmem ×3）
go test ./internal/adapter/ -run '^$' -bench 'ChatChunkDecode' -benchmem -count=3 \
  | tee 20261114-opt-batchlog-chunkdecode/raw/bench_decode_after.log

# 3) 日志落盘 bench（sync 基线 + batch 对照，同轮；固定 20000 条、零丢弃口径）
go test ./internal/telemetry/ -run '^$' -bench 'SpanLog' -benchmem -benchtime=20000x -count=3 \
  | tee 20261114-opt-batchlog-chunkdecode/raw/bench_log_after.log

# 4) 基线（bench_decode_before.log / bench_logsync_before.log）的取法：
#    解码真基线 = 把 internal/adapter/{qwen,moonshot,zhipu,deepseek,xiaomi}.go 精确逆向回
#    openai-go typed 全量 Unmarshal 版本后跑第 2) 条，再恢复新实现（本仓历史可取回原文）；
#    日志真基线 = BenchmarkSpanLogSync（同步直写路径本就未改，3) 里同轮复测）。
