# 001 —— 实施计划

> **上游**：[spec.md](spec.md) · [tasks.md](tasks.md)
> **调研依据**：[调研：智能体 Gateway 整体设计](../../docs/20261112-网关能力-调研-整体设计.md)
> **状态**：待实施（本轮不执行）

## 一、工作区与分支

- **必须开独立 worktree**（`AGENTS.md` 规则 16：本轮实施涉及 `src/` / `test/` / `pyproject.toml` / `uv.lock`）。
  ```bash
  git worktree add .worktrees/20261112-网关能力分层 .worktrees/20261112-网关能力分层 2>/dev/null || \
  git worktree add .worktrees/20261112-网关能力分层 -b 20261112-网关能力分层
  ```
- 分支名：`20261112-网关能力分层`（规则 8：`年月日-分支目的`）。
- **本会话只开一个 worktree**，后续同领域任务沿用。
- ⚠️ **删 worktree 前先在主仓跑 `uv sync`**（规则 17：`.venv` 是软链，`uv run` 会把 editable install 指向 worktree，删了就悬空）。
- 提交：一次改动合成一个提交、一次推送（规则 10）；推分支允许，**开 PR 须老宅明确授权**。
- `pyproject.toml` + `uv.lock` 属排除清单 ⇒ **必须走 PR**，⛔ 不可直推 main。

## 二、模块布局（按职责切，不按行数硬切）

```
src/flying_gateway/
├── __init__.py          # 包导出（版本号不在此，见规则 14）
├── config.py            # 扩展 Endpoint 字段（别名/上游模型/provider/能力标记/凭据引用/输出上限）
├── registry.py          # 注册表（单一真源，规则 31）— 现有，扩字段后微调
├── router.py            # 路由 + 按失败类型分链 — 现有，扩 fallback_chain
├── errors.py            # 错误分类（七类）+ 原始上游信息保留 + 出口层遮蔽
├── protocol/
│   ├── __init__.py
│   ├── openai.py        # Pydantic 请求/响应/流式帧模型（边界约束，未知字段拒绝）
│   └── sse.py           # SSE 帧编解码 + [DONE] + 心跳 + 错误帧
├── upstream.py          # httpx 客户端封装（超时/连接池/凭据注入/aclose/abort）
├── streaming.py         # 流式生命周期状态机 + 取消注册表 + client_request_id
├── policy/
│   ├── __init__.py
│   ├── ratelimit.py     # 令牌桶（按 token）
│   ├── budget.py        # 预算上限（Denial-of-Wallet，触顶即拒）
│   └── guardrail.py     # 正则 + Webhook 前置校验
├── telemetry.py         # OTel GenAI span 构造 + 字段名映射层（集中管理）
└── server.py            # FastAPI/Starlette 应用 + /v1/chat/completions + /v1/cancel + 管理面
```

**每件预估有效行**（规则 36 的 600 收口线，留增长余量）：

| 模块 | 预估 | 备注 |
|---|---|---|
| `config.py` | ~260 | 现有 184 + 字段与校验 |
| `router.py` | ~140 | 现有 85 + 分链 |
| `errors.py` | ~180 | 新增 |
| `protocol/openai.py` | ~260 | 新增（Pydantic 模型） |
| `protocol/sse.py` | ~200 | 新增 |
| `upstream.py` | ~300 | 新增（httpx 封装 + 取消） |
| `streaming.py` | ~320 | 新增（状态机 + 取消注册表） |
| `policy/*` | 各 ~150–220 | 新增 |
| `telemetry.py` | ~280 | 新增（字段映射层是重点） |
| `server.py` | ~380 | 新增（路由 + watcher + 两道闸） |

⚠️ 若任一件超 600 ⇒ 按**职责**再切（例：`streaming.py` 的「取消注册表」可独立成 `cancellation.py`），⛔ 不按行数硬切前半段/后半段（规则 36）。

## 三、关键设计说明

### 3.1 取消传播的两道闸（对应 SC-1 / SC-2）

**问题来源**：OpenViking #6888 —— 清理只挂在 SSE body 生成器的 `finally` 里，当框架在生成器**挂起或尚未启动**时停止消费它，上游响应不会被关闭。

**两道闸**：

```
请求级 try/finally            ← 闸 2：兜底（生成器从未启动 / 挂起时也执行）
  └─ StreamingResponse(body=gen())
       └─ gen() try/finally    ← 闸 1：正常路径（正常结束 / 异常 / 被框架取消）
            ├─ 读上游帧 → 写下游
            └─ finally: 关上游 + 结束 span + 记取消原因
```

- **闸 2 必须独立于 body 迭代** —— 它挂在 handler 的 `finally` 上，不依赖生成器被启动。
- 两道闸的清理动作**幂等**（重复 close 无害），且**只记一次取消**（用 `cancellation.py` 的状态机保证）。

### 3.2 watcher 模式（对应 SC-6 / SC-7）

- 流式与**非流式统一**挂一个 watcher task，监听 ASGI `http.disconnect`；收到即 cancel 主任务 + 关上游。
- ⛔ **不依赖框架的隐式行为**：实测普通 request-response handler 不会因客户端离开被取消（跑满 5s）。
- `request.is_disconnected()` 轮询作为**第二路**（watcher 是快路径，轮询是兜底）。
- 断连检测延迟 ⇒ 写入 `gateway.cancel.detected_latency_ms`，并作为**可测上界**（默认 1s，进配置）。

### 3.3 取消注册表（`cancellation.py` 或 `streaming.py` 内）

- 键：`client_request_id`；值：上游执行句柄 + 状态机。
- 状态：`RUNNING` → `CANCELLED` / `COMPLETED` / `FAILED`（**终态不可回退**）。
- 两条取消入口**共用一条路径**：
  - 显式：`POST /v1/cancel`（body: `client_request_id` + 可选 `reason`）—— fire-and-forget，**不返回确认**；
  - 被动：客户端断连（watcher 触发），原因 `client_disconnect`。
- **迟到响应丢弃**：终态后到达的帧直接丢弃（对齐 MCP「发送方 SHOULD 忽略迟到的响应」）。
- **终态 id 拒绝后续尝试**：同 `client_request_id` 的重试 / 降级 / 第二跳**必须被拒**（SC-5；late cancel race）。
- 显式取消查不到 / 已收口 ⇒ **静默忽略**（对齐 MCP「MAY 忽略」）。

### 3.4 Telemetry 字段映射层（`telemetry.py`）

- **所有** `gen_ai.*` / `gateway.*` 字段名集中在**这一个模块**定义，其他模块只引用常量。
- 理由：semconv 已搬家到 `open-telemetry/semantic-conventions-genai` 且全部属性仍是 Development ⇒ 字段名会漂移（调研 5.1）。
- span 构造分两段：**开始时**设低基数属性（供采样判定：`gen_ai.operation.name` / `gen_ai.provider.name` / `server.address` / `server.port`）；**结束时**设结果属性（usage / finish_reasons / response.status）。
- 取消路径：`gen_ai.response.status="cancelled"` + `finish_reasons=["error"]` + **span status 留 `Unset`** + **不设 `error.type`**（spec F4.3）。

### 3.5 Pydantic 边界

- 请求体、响应体、SSE 帧模型在**接收与产生**处用 Pydantic 约束（老宅口径）。
- 未知字段**拒绝**（延续 `config.py` 既有 `from_mapping` 口径）。
- 写侧严格（`extra="forbid"`）；读侧从上游响应解析时**宽容**（忽略未知字段，避免上游新增字段即崩）。

### 3.6 重试与降级的执行序（与 F2.2 / F3.2 对齐）

```
for endpoint in primary + chain(failure_kind):
    for attempt in range(endpoint.max_retries + 1):
        if client_request_id 已终态: 停止（SC-5）
        try: 执行 → 成功则结束
        except 上游错误:
            classify(错误) → 得到 failure_kind
            if 不可重试: break 到降级
```

- 关键：链的选取**依赖失败类型** ⇒ 首次失败后要**重算**链（不能一条链走到底）。
- `fallback_chain(primary, failure_kind)` 返回该类型专属链；无链 ⇒ 显式报错。

## 四、执行顺序

| 阶段 | 内容 | 产出 | 门禁 |
|---|---|---|---|
| **P0 分水岭** | 依赖引入 + `uv.lock` 重生成 + 版本 bump | `pyproject.toml` / `uv.lock` / `snapshots/` | ruff + semver |
| **P1** | `config.py` 扩展 + `registry.py` 微调 + `router.py` 分链 | F1.3 / F2.1 / F2.2 | 单测 |
| **P2** | `errors.py`（七类分类 + 原始信息 + 出口层遮蔽） | F3.1 / F5.1 | 单测 |
| **P3** | `protocol/openai.py` + `protocol/sse.py` | F1.1 / F1.2 | 单测 |
| **P4** | `upstream.py`（httpx 封装 + aclose + abort） | F1.4 | 单测（真实录制 fixture） |
| **P5** | `telemetry.py`（字段映射层 + span 构造 + 取消口径） | F4.1–F4.6 | 单测（`InMemorySpanExporter`） |
| **P6** | `streaming.py` + `cancellation.py`（状态机 + 注册表 + 两道闸 + watcher） | SC-1–SC-5 / SC-8 | 单测 + socket 测试 |
| **P7** | `policy/*`（限流 / 预算 / 护栏） | F3.3–F3.5 | 单测 |
| **P8** | `server.py` 组装（路由 + SSE 端点 + 管理面分离） | 端到端 | 端到端测试 |
| **P9** | 端到端 + 四道门禁 + 文档回填 + 事件流 | A1–A9 | 全量 |

⚠️ **次序要求**（规则 9）：**文档改动必须排在全量测试之前** —— 否则「本地全量」覆盖不到那行改动，是假绿（规则 19）。

## 五、测试策略

### 5.1 框架与命令

- **unittest**（仓库口径，规则 19）：`uv run python -m unittest discover -t . -s test -v`。
- ⛔ 不用 pytest（本仓未安装，规则 20）。
- 测试文件命名 `test_<被测模块>.py`（规则 8：以 `test` 开头才能被 discover 捡到）。

### 5.2 三类测试

| 类别 | 手段 | 覆盖 |
|---|---|---|
| **纯逻辑单测** | 直接构造对象 | 配置校验、路由分链、错误分类、限流桶、预算、护栏正则、SSE 帧编解码 |
| **span 断言** | `opentelemetry-sdk` 的 `InMemorySpanExporter` + `SimpleSpanProcessor` | F4 全部属性与 SC-3 的取消口径（**SDK 官方测试设施，非伪造数据**） |
| **端到端 socket 测试** | 起 uvicorn 子进程 + `asyncio.open_connection` 读 N 帧后**突然关闭** | SC-1 / SC-2 / SC-4 / SC-6 / SC-7 |

### 5.3 上游 fixture：**真实录制**（规则 1 + 老宅 2026-11-04 口径）

⛔ **不手写合成 JSON 冒充上游响应**。

- **录制脚本**：`test/fixtures/record_upstream.py` —— 可重跑，从真实端点录一段 SSE 响应体。
- **fixture 文件**：原封不动的录制产物（如 `test/fixtures/chat_completions_sse.txt`）。
- **来源元信息**：`test/fixtures/recordings.json` —— 记录来源 URL、录制时间、模型名、录制命令。
- 断连测试用的**假上游**必须是**回放真实录制流**的本地服务器（⛔ 不许返回手写的假 token 序列）。

⚠️ 真实录制需要真实凭据与网络 ⇒ 若本轮环境不可得，则该测试**显式 `skip` 并写明原因**（规则 19），⛔ 不许用假数据顶替后报绿。

### 5.4 断言有效性自检（规则 22）

每条取消相关断言落笔前先造两个样本：

1. **恒真样本**（错误实现也通过）—— 确认断言不是恒真；
2. **恒假样本**（错误实现也红）—— 确认断言真的在测它声称测的东西。

典型验法：
- 断言「上游已关闭」 ⇒ 写一版**不调 `aclose()`** 的错误实现，确认该实现下断言**红**；
- 断言「span status 非 ERROR」 ⇒ 写一版**取消时设 ERROR** 的错误实现，确认该实现下断言**红**；
- 断言「取消不触发重试」 ⇒ 写一版**取消后仍重试**的错误实现，确认该实现下断言**红**。

### 5.5 端到端断连测试的骨架

```
1. 起 uvicorn 子进程（真实服务器，不是 TestClient 假件）
2. asyncio.open_connection 连上，发起 stream=true 请求
3. 读满 N 帧 SSE
4. 关闭 socket（模拟浏览器 abort）
5. 断言：
   - 上游服务器观测到连接被关闭（本地 fake 上游的 closed 标志）
   - 客户端收到的帧数 == N（无追加）
   - Trace 中该 span 已结束、status=cancelled、span status 非 ERROR
   - 上游请求数 == 1（无重试）
6. 收尾：关子进程、断言无残留线程 / 句柄
```

⚠️ 用**真实 socket**而非 TestClient 的原因：TestClient 走的是进程内 ASGI 调用，**不会**产生真实的 TCP 断连 ⇒ 测不到 watcher 的真实行为（调研 7.2 的实测数据正是真实 socket 测出来的）。

## 六、风险与预案

| 编号 | 风险 | 预案 |
|---|---|---|
| **R1** | **TUNA 镜像源同步滞后** ⇒ `uv lock` 拉不到新包 | 临时用 `uv lock --index <官方源>` 覆盖，⛔ **不写进 `pyproject.toml`**（仓库级钉 TUNA 是刻意的，规则 6 的延伸） |
| **R2** | FastAPI/Starlette 在 **Python 3.12** 上的版本兼容 | `requires-python` 约束不变；装完先跑一次 import 自检；版本写进 `pyproject.toml` 下界 |
| **R3** | **OTel semconv 未 stable** ⇒ 字段名漂移 | 字段名集中在 `telemetry.py`；pin `opentelemetry-sdk` 版本；升级时跑一次「字段 diff」验证（对比升级前后的属性名集合） |
| **R4** | **真实录制 fixture 不可得**（无凭据 / 无网络） | 该测试**显式 skip + 写明原因**（规则 19）；⛔ 绝不用手写假数据顶替后报绿 |
| **R5** | **`uv run` 在 worktree 里把 `.venv` 指向 worktree** | 删 worktree 前**先在主仓 `uv sync`**（规则 17） |
| **R6** | **两道闸实现错位**（只在生成器里清理） | SC-1 / SC-2 各配一个「不调 `aclose()`」的错误实现做恒假验证（5.4） |
| **R7** | **取消被记成 ERROR** ⇒ 错误率污染 | SC-3 断言 span status 非 ERROR；并加一条「取消不计入 `error.type`」的显式断言 |
| **R8** | **依赖变更属排除清单** ⇒ 误直推 main | 本实施走分支 + PR；`gh pr create` 须老宅明确授权（规则 10） |
| **R9** | **文档与实况脱节**（调研第九章 6 条未验证项） | 实施后逐条更新状态（实测确认 / 仍待验证 / 已被推翻），写进 `docs/` 与事件流 |

## 七、完成定义（DoD）

- [ ] F1–F5 全部实现，各有对应测试
- [ ] SC-1 … SC-8 全绿（SC-6 / SC-7 用真实 socket）
- [ ] 四道本地门禁全绿：`ruff check` / `ruff format --check` / SemVer bump 校验 / `uv run python -m unittest discover -t . -s test -v`
- [ ] 改动的 `src/` 脚本按双轨制 bump（规则 15），`snapshots/` 已生成
- [ ] 调研文档第九章 6 条未验证 / 推测项**逐条更新状态**
- [ ] `docs/` 技术结论文档（规则 12：定位 → 归因 → 方案 → 结果 四段式 + 证据索引 + 复现命令）
- [ ] `progress/` 事件流（含 commit 短哈希 + PR 号，按规则 9 的次序在拿到编号后的**后续提交**补写）
- [ ] `README.md` 目录结构段按实测更新（规则 2）
