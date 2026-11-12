# 001 —— 网关能力分层与流式取消

对应：commit 1107211（直推 main，无 PR）

> **类型**：需求规格（spec）
> **状态**：待实施
> **上游文档**：[调研：智能体 Gateway 整体设计](../../docs/20261112-网关能力-调研-整体设计.md)
> **实现计划**：[plan.md](plan.md) · **任务拆解**：[tasks.md](tasks.md)

## 一、目标

把 FlyingGateway 从「**只决定发给谁**」（当前实现：配置口径 + 注册表 + 路由决策 + 降级链）扩成「**OpenAI 兼容 HTTP 网关**」，五个功能域齐备：

| 域 | 本 spec 的功能需求 |
|---|---|
| 协议 / 注册 / 执行 | F1 |
| 路由 / 上下文 | F2 |
| 错误 / 治理 | F3 |
| 可观测 | F4 |
| 安全 | F5 |

并以**流式请求的「用户点停止」**作为贯穿性验收场景 —— 它同时压到 F1（协议与执行）、F3（取消 ≠ 错误）、F4（Trace 能查到并取消结束）、F5（凭据不泄漏），是检验整条链是否真的闭合的**唯一一个能端到端验收的场景**。

**一句话验收**：用户点停止后，网关在**有界延迟**内关闭上游连接、**不再发任何帧**、**不触发降级或重试**，且**这条调用在 Trace 里已收口为 `cancelled`（span status 非 ERROR）**。

## 二、不做什么（non-goals）

- **不做 MCP / A2A 的实现**。调研文档已完整覆盖两者的协议与取消语义；实现列入后续 spec。
- **不做语义缓存 / prompt cache 命中优化**。只**计量**缓存读写 token，不做缓存层。
- **不做多租户虚拟密钥与计费报表**。只保留 `Endpoint` 的**凭据来源引用**字段（schema 一步到位，密钥体系后做）。
- **不做 Kubernetes / Envoy 数据面 / xDS**。
- **不做前端**。前端「停止后不再追加内容」是客户端职责（客户端已断开时网关**发不出**任何通知帧，见调研 7.2 动作 5）。网关只提供**证据**。
- **不做请求合并（coalescing）**。调研 7.3 指出它是「唯一送达过载后每次调用」的机制，但需要跨请求状态，且其价值与「立即取消 vs 延迟取消」的产品取舍强耦合 ⇒ 留待老宅拍板（见「未决事项」）。
- **不做带状态的路由策略**（least-busy / usage-based / cost-based）。它们与 `AGENTS.md` 规则 24 的「同输入 + 同注册表 + 同配置 ⇒ 同决策」冲突，除非状态可重放。
- **不做托管护栏**（OpenAI moderation / Bedrock Guardrails / Model Armor）—— 需外部服务与凭据。只做正则 + Webhook 接口。

## 三、术语

| 术语 | 定义 |
|---|---|
| **客户端请求标识** | `client_request_id`：一次用户轮次对话的唯一标识，由客户端提供或网关生成，**贯穿该轮的全部尝试**（含重试 / 降级 / 第二跳）。调研 7.4 的 late cancel race 与 7.3 的 coalescing 前提都依赖它。 |
| **取消** | 终止一次**进行中**的上游执行。分四类原因：`client_disconnect` / `upstream_error` / `timeout` / `shutdown`。 |
| **取消注册表** | `client_request_id → 上游执行句柄` 的映射。显式取消（`POST /v1/cancel`）与被动取消（客户端断连）**共用同一条路径**。 |
| **上游执行** | 网关对某一个具体端点的一次 HTTP 调用（含其重试）。降级链上每个端点各有一次独立执行。 |
| **降级链** | 显式有序的端点序列。**按失败类型分链**（见 F2）。 |
| **重试** | 打**同一个**端点的再次尝试。与降级**分别有上限**（`AGENTS.md` 规则 24）。 |

## 四、功能需求

> 每条给出「输入 / 输出 / 失败语义」。凡涉及数据的边界，一律用 **Pydantic** 约束（老宅口径：凡是信息接收 / 产生 / 传输处都用声明式 schema），未知字段**拒绝**（延续 `config.py` 既有口径）。

### F1 —— 协议、注册与执行

**F1.1 入口协议**
- **输入**：`POST /v1/chat/completions`，OpenAI 兼容请求体（含 `stream: bool`）。请求体由 Pydantic 模型校验，未知字段拒绝。
- **输出**：
  - `stream=false` → OpenAI 兼容 JSON 响应；
  - `stream=true` → `text/event-stream`，逐帧 `data: {...}`，末尾 `data: [DONE]`。
- **失败语义**：请求体非法 → `400`，**不发起任何上游请求**；模型未登记 → `404`（对齐 LiteLLM 的「模型不在 `model_list`」语义，调研 4.1）。

**F1.2 SSE 兼容条款**（缺一即被代理缓冲，用户看到「一次性蹦出」）
- 响应头：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`X-Accel-Buffering: no`。
- 心跳：空闲时每 **15 秒**发一行 `:keepalive` 注释行（防中间层 60s 空闲超时 —— nginx 与 AWS ALB 默认 idle timeout 均为 60s）。
- 结束哨兵：`data: [DONE]`。
- 流内错误：HTTP 状态码**已发出无法改** ⇒ 用 `event: error` 帧承载错误载荷，再关闭流。

**F1.3 端点注册表字段**（`Endpoint` 扩展；对齐 `AGENTS.md` 规则 23）

| 字段 | 语义 | 约束 |
|---|---|---|
| `name` | 端点标识 | 全局唯一 |
| `base_url` | OpenAI 兼容端点根地址 | 必须 http/https |
| `model` | **客户端请求的模型名（别名）** | 非空 |
| `upstream_model` | **实际上游模型标识** | 非空；与 `model` 不同时以本字段发给上游（对齐 LiteLLM 的 `model_name` / `litellm_params.model` 两层语义，调研 1.1） |
| `provider_name` | **OTel `gen_ai.provider.name` 取值** | 枚举；缺省由 `base_url` 推导，推导不出则报错（字段名不许撒谎，规则 23） |
| `context_window` | 上下文窗口（token） | 正整数 |
| `max_output_tokens` | 输出上限 | 正整数 |
| `capabilities` | 能力标记 | 结构体：`supports_tools` / `supports_stream` / `supports_vision`，逐项 bool |
| `credential_ref` | **凭据来源引用** | 环境变量名或密钥别名；**不存明文**（规则 21） |
| `timeout_s` / `max_retries` / `priority` | 沿用现有语义 | 同现有 `config.py` |

**F1.4 上游执行**
- **输入**：`Endpoint` + 归一化后的请求体 + `client_request_id`。
- **输出**：非流式为完整响应；流式为**帧迭代器**（异步生成器）。
- **失败语义**：上游连接失败 / 超时 → 抛**带原始上游状态码与端点名**的错误（对外归一，对内保留，见 F3.1）。

### F2 —— 路由与上下文

**F2.1 策略**：`priority` / `round_robin`（现有）保持**可复现**（规则 24）。不引入带状态策略。

**F2.2 按失败类型分链**（新增）
- **输入**：主端点 + 失败类型（`general` / `context_window` / `content_policy` / `budget` / `rate_limit` / `timeout` / `upstream_error`）。
- **输出**：该类型下的降级链（显式有序）。
- **失败语义**：某类型无降级端点 ⇒ **显式报错**，不静默回落到通用链（对齐 LiteLLM「`default_fallbacks` 不适用于 `ContentPolicyViolationError` / `ContextWindowExceededError`」的坑，调研 3.1）。

**F2.3 上下文越界判定**
- **输入**：请求的估算输入 token + 请求 `max_tokens`。
- **输出**：与 `context_window` 比较的结果。
- **失败语义**：越界 ⇒ 走 `context_window` 降级链；**无该链** ⇒ 显式报错并告知「上下文超限且无可用的大窗口端点」（⛔ 不许截断后悄悄发出去）。

### F3 —— 错误、治理与成本

**F3.1 错误分类**（`AGENTS.md` 规则 32）
- 分七类：`invalid_request`（4xx 类，含请求畸形 / 上下文超限 / 内容策略）/ `auth` / `rate_limit` / `upstream_error`（5xx 类）/ `timeout` / `budget_exceeded` / `cancelled`。
- **4xx 与 5xx 分开归类**，⛔ 不许把「端点校验更严导致 400」记成「模型没能力」。
- 错误对象**同时携带**：对外归一分类 + **原始上游状态码** + **上游端点名** + **上游请求 ID**（若有）。

**F3.2 重试与降级分别有上限**（规则 24）
- 重试：打同一端点，上限 = `Endpoint.max_retries`。
- 降级：换端点，上限 = 独立配置。
- **两者不合并成兜底循环**；任一上限用尽即显式失败。

**F3.3 token 级限流**
- 令牌桶按 **token** 计（不是只按请求数）—— Denial-of-Wallet（调研 4.2）。
- 桶的容量以 token 为单位，补充速率以 token/秒为单位。
- 超限 → `429`，**带 `Retry-After`**。

**F3.4 预算上限**
- 每调用方（`client_request_id` 的调用主体）有 token 预算上限。
- **触顶即拒绝**（不是只做报表）—— 调研 4.2 引的「预算能阻断」口径。
- 超限 → `429`（分类 `budget_exceeded`），**在发起上游请求之前**拒绝。

**F3.5 Guardrails**
- **正则**：对 prompt 做已知不安全模式匹配；命中 ⇒ 拒绝并记录。
- **Webhook**：POST 原始 prompt 给用户自定义校验端点；非 2xx ⇒ 拒绝。
- 两者均在**上游请求之前**执行。

### F4 —— 可观测

**F4.1 span 结构**
- 每次**上游执行**一个 CLIENT span；降级链上每个端点**各自独立 span**。
- span 名 = `{gen_ai.operation.name}`（规范口径）。
- `gen_ai.operation.name` = `chat`。
- 自动重试**合并进同一个 span**（规范：span SHOULD 覆盖含全部重试的逻辑操作）。

**F4.2 属性**（字段名集中在**一个**映射模块管理 —— semconv 已搬家且未 stable）

标准字段：`gen_ai.provider.name` / `gen_ai.request.model` / `gen_ai.response.model` / `gen_ai.request.stream` / `gen_ai.request.stream_cursor` / `gen_ai.usage.input_tokens` / `gen_ai.usage.output_tokens` / `gen_ai.response.status` / `gen_ai.response.finish_reasons`。

**F4.3 取消的 Trace 口径**（本 spec 最重要的一条）
- `gen_ai.response.status` = **`cancelled`**（合法枚举值）。
- `gen_ai.response.finish_reasons` = `["error"]`（规范对 cancelled 的映射）。
- **span status 留 `Unset`**；**`error.type` 不设**。
  - 依据：recording-errors 规范「Span Status Code MUST be left unset if the instrumented operation has ended without any errors」；语义约定编写指南点名 cancellations 属「无法清晰归类为错误」的条件。
  - ⛔ **取消若记成 ERROR，错误率被污染。**
- **span 在取消发生的那一刻结束**（不等上游自然结束）⇒ 取消后立即能在 Trace 里查到这条调用已收口。

**F4.4 取消细节（自定义命名空间）**

| 属性 | 含义 |
|---|---|
| `gateway.cancel.reason` | `client_disconnect` / `upstream_error` / `timeout` / `shutdown` |
| `gateway.cancel.detected_latency_ms` | 从检测到断连到关闭上游的延迟 |
| `gateway.cancel.upstream_closed` | 是否真的关了上游连接（bool） |
| `gateway.cancel.tokens_after_cancel` | 取消后仍产生的 token 数（应为 0） |

⚠️ 用 `gateway.*` 前缀，**不塞进 `gen_ai.` 命名空间**（封闭语义命名空间）。

**F4.5 内容采集**
- 默认 `NO_CONTENT`（对齐 `OTEL_INSTRUMENTATION_GENAI_CAPTURE_CONTENT` 的默认值与回落值）。
- **不采集** prompt 全文 / 响应全文 / 工具参数 / 工具结果。

**F4.6 指标**
- 取消**必须**有独立计数指标（trace 可能被 head sampling 采掉，取消不能跟着一起丢）。
- 至少：`gateway.requests`（按 provider / 模型 / 结果标签）、`gateway.cancelled`（按原因）、`gateway.tokens`（按方向）、`gateway.cancel.detected_latency`（直方图）。

### F5 —— 安全

**F5.1 凭据隔离**
- `Endpoint.credential_ref` 只存**来源引用**，⛔ 不存明文。
- 运行时从环境变量 / 密钥服务读取，**在出口层**（日志 / 轨迹 / 异常序列化）统一遮蔽为 `[REDACTED:<类型>]`（规则 21）。
- ⛔ **不透传调用方凭据**：网关用自己的凭据打上游（MCP 授权规范的 token passthrough 禁令，调研 2.3）。

**F5.2 出网管控**
- 上游 URL 在**单一控制点**校验：协议白名单 + 拒绝私网 / 回环 / 链路本地地址（防 SSRF 与 shadow access，调研 6.3）。
- **失败语义**：校验不通过 ⇒ 显式报错，⛔ 不静默改投其他端点。

**F5.3 管理面与数据面分离**
- 数据面（`/v1/chat/completions`）与管理面（注册表查看等）**不同端口 / 不同进程**；管理面默认**不绑定公网**。

## 五、流式取消验收条款（SC-1 … SC-8）

> 每条**可判定**。测试写法与断言要求见 `plan.md` 的「测试策略」与 `AGENTS.md` 规则 22。

| 编号 | 验收条款 | 判定方式 |
|---|---|---|
| **SC-1** | 客户端断连后，网关在**有界延迟**内关闭上游连接 | 断言上游 `closed` 标志为 true；且关闭延迟 ≤ 配置上界（默认 1s） |
| **SC-2** | 上游响应对象已关闭，且关闭后**不再有读取** | 断言上游读取计数在取消后不再增长 |
| **SC-3** | Trace 中该 span **已结束**、`gen_ai.response.status == "cancelled"`、**span status 不是 ERROR**、未设 `error.type` | 断言 span 的 `end_time` 已设置 + 上述属性（用 `InMemorySpanExporter`） |
| **SC-4** | 取消后**不再向客户端转发任何分片** | 断言客户端收到的帧数 == 取消点前的帧数；且 `gateway.cancel.tokens_after_cancel == 0` |
| **SC-5** | 取消**不触发**降级或重试 | 断言：断连后无新的上游请求发起（上游请求计数 == 1）；同 `client_request_id` 的后续尝试被拒 |
| **SC-6** | 断连检测延迟**有上界且可测** | socket 级测试：读 N 帧后突然关闭 → 断言取消在 ≤ 上界内发生；且测出**实际延迟值**（进 `gateway.cancel.detected_latency_ms`） |
| **SC-7** | **非流式**路径客户端断开，同样被取消 | socket 级测试：非流式请求的客户端断开后，上游请求在 ≤ 上界内被取消（框架默认**不会**取消普通 handler —— 这是本 spec 强制要求自挂 watcher 的理由） |
| **SC-8** | 客户端断连与网关主动取消**原因可区分** | 断言 `gateway.cancel.reason` 在两条路径上分别为 `client_disconnect` 与 `upstream_error`（或 `timeout`） |

### 5.1 SC-1/SC-2 的关键约束（来自真实缺陷）

⚠️ **清理必须独立于 body 迭代**：若清理只写在 SSE body 生成器的 `finally` 里，当框架在生成器**挂起或尚未启动**时停止消费它，上游响应不会被关闭 ⇒ **SC-1 / SC-2 会红**。

⇒ 实现必须同时具备**两道闸**：
1. SSE 生成器 `finally`（正常路径）；
2. 请求级 `finally`（生成器从未启动 / 挂起时的兜底）。

（来源：OpenViking issue #6888 的三组 ASGI 模拟用例；见调研 7.2 动作 3）

### 5.2 与 MCP 取消语义的对应（借鉴，非 MCP 规范）

本 spec 的 HTTP 侧取消语义**借鉴** MCP 的 `notifications/cancelled`（调研 2.2），具体对应：

| MCP 条款 | 本 spec 的设计 |
|---|---|
| fire-and-forget、无 ack | `POST /v1/cancel` **不返回确认**；查不到 / 已收口 ⇒ 静默忽略（不报错） |
| 接收方 SHOULD 停止处理 + 释放资源 + 不发响应 | 取消后**不再向下游发任何帧**，直接收口 |
| MAY 忽略（未知 / 已完成 / 无法取消） | 取消注册表查不到 ⇒ 静默忽略 |
| 发送方 SHOULD 忽略迟到的响应 | 取消后到达的迟到分片**丢弃** |
| MUST 优雅处理竞态 | 取消与「上游刚好完成」并发 ⇒ **幂等收口**，只记一次取消 |
| SHOULD 记录取消原因 | `gateway.cancel.reason` |

⚠️ **明确声明**：HTTP / SSE 侧**没有**对应标准，MCP 规范也**没有**定义 HTTP 侧的取消事件。上表是**本仓自定**的映射（调研第九章第 1 条）。

### 5.3 取消的外部可核对要求（L3 证据）

⚠️ **网关自己的 span 不能作为「真的省下了」的充分证据**（调研 7.3 / arXiv 2611.08684）：

- 取消被标记 ≠ 上游停下了；上游可能已消耗 79–85% 的 prompt token；
- provider 自报的 token 数不可靠（论文实测：vLLM 的 prompt-token 计数器漏掉三分之一到一半）。

⇒ 本 spec 追加两条**运维口径**（非单测可判，属部署验收）：

- **OC-1**：网关须暴露「重试额外制造的上游执行数」与「客户端真正收到的调用数」两个口径 —— 取消率单独看会骗人。
- **OC-2**：对自建上游（vLLM 等），须能取到调度器侧的 aborted 计数与**实际计算量**，与网关侧记录对账；provider 自报值不得作为唯一依据。

## 六、非功能要求

| 编号 | 要求 |
|---|---|
| NF-1 | 单文件有效代码 ≤ **600** 行（规则 36 的拆后收口线）；`src/` 单文件 ≤ 1000 行（触发线） |
| NF-2 | 依赖集合：`httpx` + `fastapi` + `pydantic` + `opentelemetry-sdk` + `opentelemetry-exporter-otlp-proto-http`（+ `uvicorn` 运行）；Python 约束不变（`>=3.12,<3.13`） |
| NF-3 | `ruff check src/ test/ main.py` 与 `ruff format --check src/ test/ main.py` 全绿 |
| NF-4 | `uv run python -m unittest discover -t . -s test -v` 全绿，且**新用例数**在 `progress/` 中逐字记录 |
| NF-5 | `__version__` 只存在于 `src/` 与 `main.py`（规则 14）；改动的 `src/` 脚本按双轨制 bump（规则 15） |
| NF-6 | **零 mock / 零占位**：⛔ 不许用假响应糊弄测试（规则 1）。上游 fixture 用**真实录制**的响应体，配可重跑录制脚本 + 来源元信息 |
| NF-7 | 取消路径的每个断言须先验「错误实现会红」（规则 22） |

## 七、验收总表

| 编号 | 验收口径 |
|---|---|
| A1 | F1–F5 全部有对应实现与测试；每条功能需求可指到具体模块与测试 |
| A2 | SC-1 … SC-8 全部通过，且 SC-6/SC-7 用**真实 socket** 测（不用假断连模拟） |
| A3 | 取消路径的 span：已结束、`cancelled`、span status ≠ ERROR、无 `error.type` |
| A4 | 取消不触发降级 / 重试；同 `client_request_id` 的后续尝试被拒 |
| A5 | 凭据：注册表与日志 / 轨迹 / 异常中**无明文凭据**；遮蔽形态统一为 `[REDACTED:<类型>]` |
| A6 | 内容采集默认关闭；trace 中**无** prompt / 响应全文 |
| A7 | 7.3 的三层证据链在本仓落地：L1 自证可断言、L2 计数可对账、L3 口径在运维层可查 |
| A8 | 四道本地门禁全绿（ruff ×2 + semver + unittest），且**文档改动排在全量测试之前**（规则 9 的次序要求） |
| A9 | 调研文档第九章的 6 条未验证 / 推测项，本轮实现后**逐条更新状态**（实测确认 / 仍待验证 / 已被推翻） |

## 八、未决事项（实施前需老宅拍板）

1. **取消的默认时机**：客户端断连**立即**取消上游（对齐 MCP 的「stop processing + free resources」），还是**短暂延迟**再取消以支持请求合并？
   - 调研 7.3 引的论文实测：延迟 10 秒再取消，能让重试重新挂到活着的执行上（服务恢复）；立即取消则让重试无处可挂。
   - ⇒ 这个取舍与「是否引入请求合并」**强耦合**，两个问题一起定。
2. **是否引入请求合并（coalescing）**：需要跨请求状态（Redis 或本地缓存），与本仓「0 依赖」的历史姿态冲突。
3. **provider 断连即停的逐家差异**（调研第九章第 3 条）⇒ 接真实 provider 时按家实测并回写 `docs/`。

## 九、对应的调研出处

| 条款 | 出处（详见调研文档「证据索引」） |
|---|---|
| F1.2（SSE 兼容条款） | `https://www.devopsness.com/blog/streaming-llm-responses-sse-backpressure`（nginx `X-Accel-Buffering` / 60s idle timeout / 心跳）、`https://app-lab.ai/blog/fastapi-streaming-llm-sse` |
| F1.3（注册表字段两层语义） | `https://github.com/agents-inc/skills/blob/main/src/skills/ai-infrastructure-litellm/SKILL.md` |
| F2.2（按失败类型分链） | `https://litellm.vercel.app/docs/proxy/fallback_management` |
| F2.3 / F3.1（4xx 与 5xx 分开） | `https://www.swfte.com/errors/litellm-errors`、本仓 `AGENTS.md` 规则 32 |
| F3.3（token 级限流） | `https://aigateway.envoyproxy.io/release-notes/v1.0/`、`https://nhimg.org/ai-gateway-and-llm-gateway-guide` |
| F4.2 / F4.3（属性与取消口径） | `https://raw.githubusercontent.com/open-telemetry/semantic-conventions-genai/main/docs/gen-ai/gen-ai-spans.md`、`https://raw.githubusercontent.com/open-telemetry/semantic-conventions/v1.45.0/docs/general/recording-errors.md`、`https://andygol-otel.netlify.app/docs/specs/semconv/how-to-write-conventions/` |
| F4.5（内容采集默认关闭） | 同上 spans 规范；反面教材 `https://nhimg.org/ai-gateway-and-llm-gateway-guide` |
| F5.1（不透传凭据） | `https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization`、本仓规则 21 |
| F5.2（出网管控） | `https://agentgateway.dev`、`https://a2a-protocol.org/v0.2.0/specification/` |
| SC-1 / SC-2（两道闸） | `https://github.com/volcengine/OpenViking/issues/6888` |
| SC-6 / SC-7（框架行为） | `https://async-concurrency.com/network-io-protocol-handling/asgi-servers-and-frameworks/detecting-client-disconnects-in-asgi-handlers` |
| 5.2 / 5.3（取消语义与 L3 证据） | `https://modelcontextprotocol.io/specification/2025-06-18/basic/utilities/cancellation.md`、`https://arxiv.org/abs/2611.08684` |
