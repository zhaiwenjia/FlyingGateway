#!/bin/bash
# 日期: 2026-11-14 21:18:08.779
# 作者: Zhai Wenjia
# 吞吐/延迟阶梯压测跑批：1→16384 并发 ×2 侧 ×2 轮（P3/P4），每档 warmup 2s + 测量 5s。
# 用法：bash scripts/run_ladder.sh   （需先起 Go 假上游(9099,-latency 300ms)/网关(8081)/LiteLLM(4001)）
set -u
cd "$(dirname "$0")/../.."   # 到 worktree 根
BIN=20261114-realkeys-mimo-flash-bench/bin/benchload
OUT=20261114-realkeys-mimo-flash-bench/raw/ladder
mkdir -p "$OUT"
LEVELS="1 2 4 8 16 32 64 128 256 512 1024 2048 4096 8192 16384"
for tag in P3 P4; do
  for c in $LEVELS; do
    for t in gateway litellm; do
      if [ "$t" = gateway ]; then
        url="http://127.0.0.1:8081/v1/chat/completions"; token="local-bench"
      else
        url="http://127.0.0.1:4001/v1/chat/completions"; token="sk-litellm-local-master"
      fi
      "$BIN" -url "$url" -token "$token" -target "$t" -c "$c" \
        -warmup 2s -dur 5s -ramp 2000 -sportlo 20000 -sporthi 49151 \
        -o "$OUT/${tag}_${t}_c${c}.json" -detail "$OUT/${tag}_${t}_c${c}.jsonl.gz" \
        | python3 -c "import sys,json;s=json.load(sys.stdin)['summary'];print('$tag','$t','c=$c','ok=%d'%s['ok'],'fail=%d'%s['fail'],'rps=%.1f'%s['throughput_rps'],'p50=%s'%s['latency_ms']['p50'])"
    done
  done
done
echo LADDER-DONE
