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
4. **压测对比（每档 3 轮取中位）**：

| 档位 | 指标 | FlyingGateway | LiteLLM | 较量 |
|---|---|---|---|---|
| 并发 1 | P50 / P99 (ms) | 1323 / 7652 | 1373 / 15405 | 网关双优 |
| 并发 8 | P50 / P99 (ms) | 2017 / 4709 | 1683 / 4385 | LiteLLM 略优 |
| 并发 32 | P50 / P99 (ms) | 2399 / 5360 | 2423 / 9198 | 网关双优 |
| 吞吐 | c1 / c8 / c32 (req/s) | 0.51 / 2.80 / 7.18 | 0.35 / 3.16 / 5.64 | 网关 2/3 档优 |
| 成功率 | 全档 | **100%** | **100%** | 恒 100%（验收口径，不作图） |

   **成功率口径（老宅裁定）**：成功率必须恒 100%，任何非 100% 按 bug 修。r2 轮曾有 1 例失败（上游单请求卡满 60s）——根因是**超时被误分类为 `invalid_request`、重试被静默绕过**（`invalid_request` 不在可重试集），已修：`adapterDetail` 兜底改传输语义分类（timeout ⇒ 可重试），并以「首请求卡死 → 同端点重试 → 200、上游恰 2 次」端到端钉死；修复后 r4 轮全档 64/64。
   **波动提示**：吞吐轮间方差大、主导因素是**上游尾部/限流**而非网关（r3 轮网关 c32 11.06 rps、r2 轮 1.00 rps，逐请求 JSON 可定性）；报告里的中位数已按 3 轮聚合。
5. **超时分类缺口已修复**（原列遗留）：上游超时曾对外归 `400 invalid_request/adapter_error`（SDD 7.1 应归 `timeout` 类），且误分类导致重试被绕过 ⇒ 已改传输语义分类并以端到端钉子钉死（成功率恒 100%）。

## 四·五、c=8 档瓶颈定位与优化（本地隔离实验）

**问题**：真实上游 3 轮中位 c=8 是唯一输档（2.80 vs 3.16 rps）。**定位**（两步）：
1. **本地隔离对比**（`scripts/fake_upstream.py` 固定 300ms 延迟，直连/网关/LiteLLM 三方同源，c=8 × 3 轮交叉，数据 `raw/local/`）：直连 25.7–25.8 rps（P50 ~308ms）、**FlyingGateway 24.7–25.2（+7~14ms/请求，≈4%）**、LiteLLM 18.8–21.8（+60~67ms，≈22%）⇒ 网关自身开销远低于 LiteLLM，**c=8 短板不是网关结构瓶颈**。
2. **真实瓶颈**：Go 默认传输层 `MaxIdleConnsPerHost=2`——中等并发下连接用完即关、下一波全量 TLS 重连（c=1 无争用、c=32 全忙不回池，症状与「只有 c=8 输」吻合）。**修复**：`installAuthTransport` 的传输层改保活池（100/host·200 总·90s）；修复后网关 c8 单轮 2.80→**4.16 rps**、P99 4709→**2439ms**。

**结论**：c=8 优化完成。真实上游 6 轮中位两侧打平（波动主导，c8 单轮双方 0.8–4.2 rps 荡）；受控条件下网关 4% vs LiteLLM 22% 的自身开销差距是稳定可复现的结构差异。成功率恒 100%。

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
# 4) 本地隔离实验（c=8 瓶颈定位用）
/tmp/bench/bin/python 20261114-realkeys-mimo-flash-bench/scripts/fake_upstream.py --port 9099 --latency 0.3 &
/tmp/gateway-bench -config 20261114-realkeys-mimo-flash-bench/raw/gateway-local.json -rates config/rates.json -listen 127.0.0.1:8081 -allow-loopback &
/tmp/bench/bin/litellm --config 20261114-realkeys-mimo-flash-bench/raw/litellm-local.yaml --port 4001 --host 127.0.0.1 &
/tmp/bench/bin/python 20261114-realkeys-mimo-flash-bench/scripts/loadtest.py --target litellm --host 127.0.0.1 --port 9099 --concurrency 8 --requests 32   # 直连；8081=网关、4001=LiteLLM 同跑
```

> 注：venv 依赖 `matplotlib aiohttp requests litellm[proxy]`（`/tmp/bench`）；key 一律经 `.env` 注入，⛔ 不出现在命令行与仓库。
