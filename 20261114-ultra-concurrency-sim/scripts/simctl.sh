#!/usr/bin/env bash
# 日期: 2026-11-14 19:40:02.771
# 作者: Zhai Wenjia
# simctl 超高并发等价模拟的起停/冒烟控制（bash；规避 nohup & 的 && 链陷阱）。
# 用法: simctl.sh upstream|gateway|litellm|down|smoke|stats
set -uo pipefail
export PATH="/opt/homebrew/bin:$PATH"
SIM="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RAW="$SIM/raw"
BIN="$SIM/bin"
RATES="$SIM/../config/rates.json"
LISTEN="127.0.0.1:9102,127.0.0.1:9103,127.0.0.1:9104,127.0.0.1:9105,127.0.0.1:9106,127.0.0.1:9107,127.0.0.1:9108,127.0.0.1:9109"
mkdir -p "$RAW"

pidfile() { echo "$RAW/.$1.pid"; }

start_upstream() {
  local hold="${1:-30}"
  if [ -f "$(pidfile fu)" ] && kill -0 "$(cat "$(pidfile fu)")" 2>/dev/null; then echo "upstream 已在运行 pid=$(cat "$(pidfile fu)")"; return 0; fi
  nohup "$BIN/fakeupstream" -listen "$LISTEN" -hold "$hold" >"$RAW/fakeupstream.log" 2>&1 &
  echo $! >"$(pidfile fu)"
  sleep 1
  echo "upstream 启动 pid=$(cat "$(pidfile fu)") hold=$hold"
}

start_gateway() {
  if [ -f "$(pidfile gw)" ] && kill -0 "$(cat "$(pidfile gw)")" 2>/dev/null; then echo "gateway 已在运行 pid=$(cat "$(pidfile gw)")"; return 0; fi
  ULTRA_FAKE_KEY="sk-ultra-fake" GATEWAY_CALLER_KEYS="" \
    nohup "$BIN/gateway" -config "$SIM/gateway-ultra.json" -rates "$RATES" \
      -listen 127.0.0.1:8090 -L --caller-bucket-capacity 0 --upstream-bucket-capacity 0 \
      >"$RAW/gateway.log" 2>&1 &
  echo $! >"$(pidfile gw)"
  sleep 2
  echo "gateway 启动 pid=$(cat "$(pidfile gw)")"
}

start_litellm() {
  if [ -f "$(pidfile ll)" ] && kill -0 "$(cat "$(pidfile ll)")" 2>/dev/null; then echo "litellm 已在运行 pid=$(cat "$(pidfile ll)")"; return 0; fi
  nohup /tmp/bench/bin/litellm --config "$SIM/litellm-ultra.yaml" --host 127.0.0.1 --port 8081 \
      >"$RAW/litellm.log" 2>&1 &
  echo $! >"$(pidfile ll)"
  sleep 6
  echo "litellm 启动 pid=$(cat "$(pidfile ll)")"
}

down() {
  for n in ll gw fu; do
    if [ -f "$(pidfile $n)" ]; then
      pid="$(cat "$(pidfile $n)")"
      kill "$pid" 2>/dev/null && echo "killed $n pid=$pid"
      rm -f "$(pidfile $n)"
    fi
  done
  pkill -f 'bin/fakeupstream' 2>/dev/null
  pkill -f 'bin/gateway' 2>/dev/null
  pkill -f 'litellm' 2>/dev/null
  sleep 1
  echo "down 完成"
}

smoke() {
  echo "=== smoke: 1 个 quick 请求走 gateway ==="
  curl -sS -m 20 -X POST http://127.0.0.1:8090/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -d '{"model":"ultra-model","messages":[{"role":"user","content":"ultra-quick"}],"stream":false}' \
    -w '\nHTTP %{http_code} time=%{time_total}s\n' | head -40
}

stats() {
  for n in fu gw ll; do
    [ -f "$(pidfile $n)" ] || continue
    pid="$(cat "$(pidfile $n)")"
    rss="$(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ')"
    fds="$(lsof -p "$pid" 2>/dev/null | wc -l | tr -d ' ')"
    echo "$n pid=$pid rss_kb=$rss fds=$fds"
  done
}

case "${1:-}" in
  upstream) shift; start_upstream "${1:-30}" ;;
  gateway) start_gateway ;;
  litellm) start_litellm ;;
  down) down ;;
  smoke) smoke ;;
  stats) stats ;;
  *) echo "用法: simctl.sh upstream|gateway|litellm|down|smoke|stats"; exit 1 ;;
esac
