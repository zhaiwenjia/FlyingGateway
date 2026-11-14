# 20261114-realkeys-mimo-flash-bench —— 真实 key 全功能实测 · 方法 A/B · mimo 压测对比

> **日期**：2026-11-14 · **执行**：MCode（主会话 + 专项子任务）
> **配套**：仓库根 README「四、真实 API 实测与性能对比」；测试报告 `docs/20261114-统一SDD实现-测试报告.md`

## 一、问题

1. 老宅提供 8 个真实 API key（部分不可用/无余额/失效）：逐个用**各家费率最低模型**实测定性，只保留有效 key。
2. 用**全部有效 key 过全部功能面**（10 项功能 × 5 家，经网关打真实上游）。
3. 方法 A/B：每家上游 `/chat/completions`（Chat 法）vs `/responses`（Responses 法）同电池对比，**选出兼容性更好、功能更全的方法**并落地。
4. **mimo-v2.6-flash 高并发压测**：FlyingGateway vs LiteLLM 同负载全方位对比，数据留档 + 柱状图进 README。

## 二、方法

- **被测链路**：FlyingGateway（本仓 `cmd/gateway`，配置 `raw/gateway-bench.json`，5 端点）vs LiteLLM 代理 v1.83.9（`raw/litellm-config.yaml`），同机 127.0.0.1，上游同为 MiMo 开放平台。
- **压测负载**：`scripts/loadtest.py`（asyncio 定并发），每档 16/32/64 请求、max_tokens=16、temperature=0；并发档 1/8/32；**每档 2 轮取中位**（r2 静默批 + r3 交叉批，抗上游尾部波动；r1 被 r2 覆盖，量级一致）。
- **功能矩阵**：`scripts/feature_matrix.py`（10 功能 × 5 家经网关）；**方法 A/B**：`scripts/method_ab.py`（直连上游两端点同电池 4 项）。
- **绘图**：`scripts/plot_bench.py`（matplotlib + `ggplot` 风格，macOS PingFang 字体）。
- **密钥**：一律走 `.env`（⛔ 不入库，`git check-ignore` 验证）；脚本只读环境变量，⛔ 不落明文。

## 三、原始数据（raw/）

| 文件 | 内容 |
|---|---|
| `keycheck.json` | 8 key 探测逐条报文（分类判定依据） |
| `feature_matrix.json` | 5×10 功能矩阵逐格证据（第一轮 38/50 + 修复后 50/50） |
| `method_ab.json` | Chat vs Responses 逐格证据与逐家评分/结论 |
| `loadtest_*.json`、`rounds/r2_*.json` | 压测逐请求延迟/状态 + 汇总（r2/r3 两轮） |
| `bench-summary.csv` | 跨轮中位汇总表 |
| `20261114-bench-overview.png` / `-latency.png` | 柱状图副本（主用图在 docs/images/） |
| `gateway-bench.json` / `litellm-config.yaml` | 两份被测服务配置 |

## 四、结论

1. **key 定性**（8 个）：**5 个有效**（Qwen/智谱/DeepSeek/Kimi/MiMo，费率最低模型实调 200）；OpenAI **区域封锁**（403 unsupported_country_region）；MiniMax **鉴权拒绝**；火山 **鉴权有效但账号未开通任何模型**（ModelNotOpen）。`.env` 只留 5 个有效 key。
2. **全功能矩阵**：修复两个真实 wire bug 后 **50/50 全 PASS**——① MiMo 流式 usage 两拍（finish 帧 `usage:null` ⇒ 独立 usage 帧）被提前收口丢弃；② Kimi usage 在 `choices[0].usage`（非顶层）探测不到。修复后各自新增真捕获钉子（`internal/adapter/testdata/real/` 的 `mimo_stream_real.sse` / `kimi_stream_real.sse`）。
3. **方法 A/B（裁定：全部 Chat 法）**：

| 供应商 | Chat 法（兼容/完整） | Responses 法（兼容/完整） | 结论 |
|---|---|---|---|
| Qwen | 4 / 8 | 2 / 5（tools、stream 丢） | **Chat** |
| 智谱 | 4 / 8 | 0 / 0（**无 /responses 端点**） | **Chat** |
| DeepSeek | 4 / 8 | 2 / 5 | **Chat** |
| Kimi | 4 / 8 | 2 / 5 | **Chat** |
| MiMo | 4 / 8 | 3 / 7（stream 丢） | **Chat** |

   ⇒ 出站方法**全量统一 Chat Completions**（Qwen 从 Responses 翻 Chat 已落地；OpenAI key 区域封锁无法实测，保留 Responses——其官方原生即 Responses，SDD 3.6 优先级②）。
4. **压测对比（每档 2 轮取中位）**：

| 档位 | 指标 | FlyingGateway | LiteLLM | 较量 |
|---|---|---|---|---|
| 并发 1 | P50 / P99 (ms) | 1449 / 6296 | 1356 / 8805 | P99 网关优 |
| 并发 8 | P50 / P99 (ms) | 2147 / 4700 | 1971 / 3224 | LiteLLM 略优 |
| 并发 32 | P50 / P99 (ms) | 2179 / 4403 | 2263 / 6696 | P50/P99 网关优 |
| 吞吐 | c1 / c8 / c32 (req/s) | 0.52 / 3.03 / 6.03 | 0.53 / 3.68 / 8.02 | 中位 LiteLLM 高 |
| 成功率 | 三档 | 100/100/98% | 100/100/100% | 持平 |

   **波动提示**：吞吐轮间方差大且主导因素是**上游尾部/限流**而非网关——r3 交叉轮 c=32 网关 11.06 rps 反超 LiteLLM 5.64；r2 轮网关 c=32 的 1 例失败是上游单请求卡满 60s（`context deadline exceeded`）。P50 双方 ±7% 内，网关的差异化在**尾部稳定性**（P99 2/3 档更优）与治理语义（取消/降级/计数口径），不在峰值吞吐。
5. **遗留观察**：上游超时的对外错误目前是 `400 invalid_request/adapter_error`，按 SDD 7.1 应归 `timeout` 类（错误分类小缺口，列入测试报告第六节）。

## 五、复现

```bash
export PATH="/opt/homebrew/bin:$PATH"
cd .worktrees/20261114-统一SDD实现
# 1) key 探测（读 .env）
/tmp/bench/bin/python 20261114-realkeys-mimo-flash-bench/scripts/probe_keys.py
# 2) 起被测（网关 + LiteLLM）
set -a && . ./.env && set +a
go build -o /tmp/gateway-bench ./cmd/gateway
/tmp/gateway-bench -config 20261114-realkeys-mimo-flash-bench/raw/gateway-bench.json -rates config/rates.json -listen 127.0.0.1:8080 -allow-loopback &
/tmp/bench/bin/litellm --config 20261114-realkeys-mimo-flash-bench/raw/litellm-config.yaml --port 4000 --host 127.0.0.1 &
# 3) 功能矩阵 / 方法 A/B / 压测 / 绘图
/tmp/bench/bin/python 20261114-realkeys-mimo-flash-bench/scripts/feature_matrix.py
/tmp/bench/bin/python 20261114-realkeys-mimo-flash-bench/scripts/method_ab.py
/tmp/bench/bin/python 20261114-realkeys-mimo-flash-bench/scripts/loadtest.py --target gateway --tag rN --concurrency 32 --requests 64   # 同理 litellm、各并发档
/tmp/bench/bin/python 20261114-realkeys-mimo-flash-bench/scripts/plot_bench.py
```

> 注：venv 依赖 `matplotlib aiohttp requests litellm[proxy]`（`/tmp/bench`）；key 一律经 `.env` 注入，⛔ 不出现在命令行与仓库。
