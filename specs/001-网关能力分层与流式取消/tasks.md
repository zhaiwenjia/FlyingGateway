# 001 —— 任务拆解

> **上游**：[spec.md](spec.md) · [plan.md](plan.md)
> **状态**：待实施
> 勾选约定：`- [ ]` 未开始 · `- [x]` 完成（**完成须附实测证据**：命令 + 输出摘要）

---

## P0 —— 分水岭：依赖与版本

- [ ] **T-001** 引入运行时依赖
  - **改动**：`pyproject.toml`（`dependencies`）、`uv.lock`
  - **内容**：`httpx`、`fastapi`、`pydantic`、`opentelemetry-sdk`、`opentelemetry-exporter-otlp-proto-http`；运行期另加 `uvicorn`
  - **约束**：`requires-python = ">=3.12,<3.13"` 不变；仓库级 TUNA 镜像源**不动**
  - **验收**：`uv sync` 成功；`uv run python -c "import httpx, fastapi, pydantic, opentelemetry.sdk"` 无 ImportError
  - **依赖**：无
  - **风险**：R1（镜像源滞后）→ 临时 `--index` 覆盖，不写进配置

- [ ] **T-002** 版本双轨 bump
  - **改动**：`src/` 下被改脚本的 `__version__`、`snapshots/`
  - **命令**：`python bump_version.py bump <脚本...> --type minor`
  - **验收**：CI 的 SemVer 校验通过；`snapshots/` 生成新文件
  - **依赖**：T-001（须先知道改哪些文件 → 实际在 P1 之后再执行，见 T-013）

---

## P1 —— 配置、注册表、路由分链

- [ ] **T-010** `config.py` 扩展 `Endpoint` 字段
  - **改动**：`src/flying_gateway/config.py`
  - **新增字段**：`upstream_model`、`provider_name`、`max_output_tokens`、`capabilities`（`supports_tools` / `supports_stream` / `supports_vision`）、`credential_ref`
  - **约束**：`credential_ref` **只接受来源引用**（环境变量名 / 密钥别名），⛔ 拒绝看起来像明文密钥的值；`from_mapping` 继续对未知键报错
  - **验收**：`test_config.py` 新增用例 —— 缺必填 / 类型错 / 未知键 / 明文凭据形态均报错；合法配置通过
  - **依赖**：T-001

- [ ] **T-011** `registry.py` 适配新字段
  - **改动**：`src/flying_gateway/registry.py`
  - **内容**：保持单一数据源与稳定排序（规则 31）；新增按 `provider_name` / `capabilities` 的查询
  - **验收**：`test_registry.py` 新增用例；既有 11 项全绿
  - **依赖**：T-010

- [ ] **T-012** `router.py` 按失败类型分链
  - **改动**：`src/flying_gateway/router.py`
  - **内容**：`fallback_chain(primary, failure_kind)` —— 七类失败各一条链；`priority` / `round_robin` 保持可复现（规则 24）
  - **验收**：`test_router.py` 新增用例 —— 各类型链正确、**无链时显式报错**（⛔ 不回落通用链）、既有 11 项全绿
  - **依赖**：T-010

---

## P2 —— 错误分类与遮蔽

- [ ] **T-020** `errors.py` 错误分类
  - **改动**：`src/flying_gateway/errors.py`（新增）
  - **内容**：七类分类（`invalid_request` / `auth` / `rate_limit` / `upstream_error` / `timeout` / `budget_exceeded` / `cancelled`）；错误对象同时携带**原始上游状态码 + 上游端点名 + 上游请求 ID**
  - **验收**：`test_errors.py` —— 4xx 与 5xx 分开归类（⛔ 400 不得归入「模型无能力」）；原始信息不丢
  - **依赖**：无

- [ ] **T-021** 出口层统一遮蔽
  - **改动**：`src/flying_gateway/errors.py`
  - **内容**：`[REDACTED:<类型>]` 统一形态；在**出口层**（日志 / 轨迹 / 异常序列化）做一次，不在每个分支各写各的（规则 21）
  - **验收**：`test_errors.py` —— 含 `Authorization: Bearer sk-xxx` / api_key 的异常消息序列化后**无明文**，且遮蔽形态统一
  - **依赖**：T-020

---

## P3 —— 协议与 SSE

- [ ] **T-030** `protocol/openai.py` Pydantic 模型
  - **改动**：`src/flying_gateway/protocol/openai.py`（新增，含 `__init__.py`）
  - **内容**：请求 / 响应 / 流式帧模型；写侧严格（未知字段拒绝）、读侧宽容（忽略上游未知字段）
  - **验收**：`test_protocol_openai.py` —— 未知字段拒绝；上游新增字段不导致崩溃
  - **依赖**：T-001

- [ ] **T-031** `protocol/sse.py` SSE 帧编解码
  - **改动**：`src/flying_gateway/protocol/sse.py`（新增）
  - **内容**：`data: {...}` 帧、`data: [DONE]` 哨兵、`:keepalive` 心跳注释行、`event: error` 错误帧
  - **验收**：`test_protocol_sse.py` —— 帧往返、哨兵、心跳、错误帧；多行 `data:` 的拼接规则
  - **依赖**：T-001

---

## P4 —— 上游执行

- [ ] **T-040** `upstream.py` httpx 封装
  - **改动**：`src/flying_gateway/upstream.py`（新增）
  - **内容**：连接池、超时按 `Endpoint.timeout_s`、`credential_ref` 解析并注入、**`aclose()` 取消路径**、`upstream_model` 覆盖 `model` 发往上游
  - **验收**：`test_upstream.py` —— 凭据从引用解析（⛔ 配置文件里无明文）；`aclose()` 后不再有读取
  - **依赖**：T-010、T-030

---

## P5 —— 可观测

- [ ] **T-050** `telemetry.py` 字段映射层
  - **改动**：`src/flying_gateway/telemetry.py`（新增）
  - **内容**：**所有** `gen_ai.*` / `gateway.*` 字段名集中定义；span 构造（开始时低基数属性 / 结束时结果属性）；OTLP exporter 装配
  - **验收**：`test_telemetry.py` —— 字段名**只有这一个模块**定义（可用源码扫描断言）；span 名 = `{gen_ai.operation.name}`
  - **依赖**：T-001

- [ ] **T-051** 取消的 Trace 口径
  - **改动**：`src/flying_gateway/telemetry.py`
  - **内容**：`gen_ai.response.status="cancelled"` + `finish_reasons=["error"]` + **span status 留 `Unset`** + **不设 `error.type`** + `gateway.cancel.*` 四字段
  - **验收**：`test_telemetry.py` —— **恒假验证**：写一版「取消时设 ERROR / 设 error.type」的错误实现，确认断言**红**（规则 22）
  - **依赖**：T-050

- [ ] **T-052** 内容采集默认关闭
  - **改动**：`src/flying_gateway/telemetry.py`
  - **内容**：默认 `NO_CONTENT`；⛔ 不采集 prompt 全文 / 响应全文
  - **验收**：`test_telemetry.py` —— 默认配置下 span 属性中**无** prompt / 响应全文
  - **依赖**：T-050

---

## P6 —— 流式生命周期与取消（核心）

- [ ] **T-060** 取消注册表与状态机
  - **改动**：`src/flying_gateway/cancellation.py`（新增；若 `streaming.py` 放不下则独立，否则并入）
  - **内容**：`client_request_id → 句柄`；状态 `RUNNING → CANCELLED / COMPLETED / FAILED`（终态不可回退）；显式取消与被动取消**共用一条路径**；查不到 / 已收口 ⇒ **静默忽略**；**终态 id 拒绝后续尝试**
  - **验收**：`test_cancellation.py` —— 迟到分片丢弃；终态 id 的后续尝试被拒；重复取消只记一次
  - **依赖**：无

- [ ] **T-061** 流式状态机
  - **改动**：`src/flying_gateway/streaming.py`（新增）
  - **内容**：上游帧迭代 → 下游写；四类取消原因；`tokens_after_cancel` 计数
  - **验收**：`test_streaming.py` —— 取消后不再转发任何分片；`tokens_after_cancel == 0`
  - **依赖**：T-031、T-040、T-060

- [ ] **T-062** 两道闸（取消传播）
  - **改动**：`src/flying_gateway/streaming.py`
  - **内容**：闸 1（生成器 `finally`）+ 闸 2（请求级 `finally`，独立于 body 迭代）；清理幂等
  - **验收**：`test_streaming.py` —— **恒假验证**：写一版「只在生成器里清理」的错误实现，确认「生成器从未启动」的用例下断言**红**（SC-1 / SC-2）
  - **依赖**：T-061

- [ ] **T-063** watcher（断连检测）
  - **改动**：`src/flying_gateway/streaming.py`（或 `server.py`）
  - **内容**：`http.disconnect` watcher task + `is_disconnected()` 轮询兜底；**流式与非流式统一**；检测延迟写入 `gateway.cancel.detected_latency_ms`
  - **验收**：`test_streaming.py` —— 假 request 的 `is_disconnected` 在 N 次后返回 true 时能触发取消
  - **依赖**：T-061

- [ ] **T-064** 端到端断连测试（真实 socket）
  - **改动**：`test/test_streaming_e2e.py`（新增）
  - **内容**：起 uvicorn 子进程 + `asyncio.open_connection` 读 N 帧后突然关闭；覆盖 SC-1 / SC-2 / SC-4 / SC-6 / SC-7
  - **验收**：见 spec 的 SC 表；⚠️ **真实 socket**，⛔ 不用 TestClient 假件
  - **依赖**：T-062、T-063、P8

---

## P7 —— 治理

- [ ] **T-070** `policy/ratelimit.py` token 级限流
  - **改动**：新增
  - **内容**：令牌桶以 **token** 为单位；超限 ⇒ `429` + `Retry-After`
  - **验收**：`test_policy_ratelimit.py`
  - **依赖**：T-001

- [ ] **T-071** `policy/budget.py` 预算上限
  - **改动**：新增
  - **内容**：每调用方 token 预算；**触顶即拒**（在发起上游请求**之前**）
  - **验收**：`test_policy_budget.py`
  - **依赖**：T-001

- [ ] **T-072** `policy/guardrail.py` 护栏
  - **改动**：新增
  - **内容**：正则匹配 + Webhook 前置校验；均在**上游请求之前**
  - **验收**：`test_policy_guardrail.py`
  - **依赖**：T-001

---

## P8 —— 服务组装

- [ ] **T-080** `server.py` 应用与路由
  - **改动**：`src/flying_gateway/server.py`（新增）
  - **内容**：`POST /v1/chat/completions`（流式 / 非流式）、`POST /v1/cancel`；SSE 响应头与心跳；**管理面与数据面分离**；出网 URL 校验
  - **验收**：`test_server.py` —— 端点返回正确状态码；非法请求**不发起上游请求**（断言上游计数 == 0）
  - **依赖**：T-030、T-031、T-040、T-061、T-070、T-071、T-072

- [ ] **T-081** 出网 URL 校验
  - **改动**：`src/flying_gateway/upstream.py` 或 `server.py`
  - **内容**：单一控制点校验协议白名单 + 拒绝私网 / 回环 / 链路本地（防 SSRF 与 shadow access）
  - **验收**：`test_upstream.py` —— 私网地址被拒且**显式报错**（⛔ 不静默改投）
  - **依赖**：T-040

---

## P9 —— 真实 fixture、端到端、收口

- [ ] **T-090** 上游真实录制 fixture
  - **改动**：`test/fixtures/record_upstream.py`（新增）、`test/fixtures/chat_completions_sse.txt`（录制产物）、`test/fixtures/recordings.json`（来源元信息）
  - **内容**：可重跑录制脚本；fixture 为**原封不动的真实响应体**
  - **验收**：⛔ 不得手写合成 JSON；若环境无凭据 / 无网络 ⇒ 相关测试**显式 skip 并写明原因**（规则 19），⛔ 不许用假数据顶替后报绿
  - **依赖**：T-040

- [ ] **T-091** 全量门禁
  - **命令**：
    ```bash
    uv run ruff check src/ test/ main.py
    uv run ruff format --check src/ test/ main.py
    uv run python -m unittest discover -t . -s test -v
    ```
  - **验收**：全绿；**文档改动排在全量测试之前**（规则 9 的次序要求）
  - **依赖**：全部

- [ ] **T-092** 调研文档未验证项回填
  - **改动**：`docs/20261112-网关能力-调研-整体设计.md` 第九章
  - **内容**：6 条逐条更新为「实测确认 / 仍待验证 / 已被推翻」，并附实测出处
  - **依赖**：T-064、T-090

- [ ] **T-093** 技术结论文档
  - **改动**：`docs/`（新增，命名 `年月日-核心内容.md`）
  - **内容**：按规则 12 的「一、实验过程 / 二、实验发现 / 三、优化方案 / 四、优化结果」四段式 + 证据索引 + 复现命令；头部写 `对应：commit <短哈希> · PR #<号>`
  - **依赖**：T-091

- [ ] **T-094** 事件流与 README
  - **改动**：`progress/YYYY-MM-DD.md`（当天文件）、`README.md` 目录结构段
  - **内容**：事件流按规则 9 的三段式 + 毫秒时间戳 + commit 短哈希 + PR 号；README 按实测更新（规则 2）
  - **依赖**：T-091

---

## 任务依赖图

```
T-001 ──┬─ T-010 ── T-011 ── T-012
        ├─ T-030 ── T-031
        ├─ T-040 ── T-081
        ├─ T-050 ── T-051 / T-052
        └─ T-070 / T-071 / T-072

T-020 ── T-021

T-031 + T-040 + T-060 ── T-061 ── T-062 ── T-063
                                              │
T-030 + T-031 + T-040 + T-061 + T-070/71/72 ── T-080
                                              │
T-062 + T-063 + T-080 ───────────────────── T-064

全部 ── T-090 ── T-091 ── T-092 / T-093 / T-094
                └─ T-002（版本 bump，最后）
```
