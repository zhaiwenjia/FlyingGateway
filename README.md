<div align="center">

<img src="images/flying-gateway-icon.jpg" alt="FlyingGateway" width="220">

# FlyingGateway

**智能体 LLM 网关** —— 多供应商端点注册、路由与降级、统一计量与治理。

</div>

## 一、项目概述

面向智能体（agent）的多供应商 LLM 网关：客户端用**一个 OpenAI 兼容入口**打任意注册模型，
网关负责把请求映射到对应供应商上游，并在转发链路上补齐网关该有的治理语义。

**设计要点**：

- **统一入口、注册表映射** —— `/v1/chat/completions`、`/v1/responses` 双入口同管线；
  用户面只认注册表里的模型别名，别名 → 上游模型 / 端点 / 协议形状的映射全在注册表，
  新增供应商只加注册表条目与适配器，不改调用方。
- **11 家供应商适配** —— OpenAI、Anthropic、Gemini、DeepSeek、Qwen、智谱、Kimi（moonshot）、
  MiniMax、MiMo（xiaomi）、Grok（xai）、Ollama；出站统一 Chat Completions 形，
  usage 逐家归一到统一口径，费用按费率表计量。
- **路由与降级** —— `priority`（显式有序降级链）/ `round_robin` 两种策略；
  路由决策可复现（同输入 + 同注册表 + 同配置 ⇒ 同一决策），降级目标不可用时显式报错、
  不静默换端点。
- **治理语义** —— 错误九分类与可重试性、重试与降级分账、token 级限流与预算、
  超时 / 取消 / 上游错误三分、流式逐帧转发与断连检测、显式取消（`/v1/cancel`）。
- **安全** —— 凭据只从环境变量读（注册表只存来源引用）、日志与轨迹统一脱敏
  （`[REDACTED:<类型>]`）、出网目标校验（默认禁内网，环回需显式放行）。

技术栈 **Go 1.27**，核心链路纯标准库（供应商 SDK 仅 3 个官方库）。

## 二、目录结构

```
FlyingGateway/                  ← 仓库根
├── go.mod / go.sum             module github.com/zhaiwenjia/FlyingGateway（go 1.27）
├── .env / .env.example         运行配置（真值只存 .env，⛔ 不入库）
├── AGENTS.md                   ★ 协作规则的唯一来源
├── README.md
├── LICENSE
├── images/                     logo 等静态资源
├── cmd/
│   └── gateway/                程序入口（flag 解析、装配、启动）
├── internal/
│   ├── server/                 HTTP 服务：双入口管线、调用方鉴权、取消注册表、管理面
│   ├── core/                   核心契约（请求/响应/流帧/Provider 枚举）
│   ├── codec/                  入站编解码（Chat / Responses 两形）
│   ├── stream/                 流式转发、SSE 扫描与帧编码
│   ├── adapter/                11 家供应商适配器（出站协议转换、usage 归一）
│   ├── registry/               端点注册表（路由的唯一真源）
│   ├── routing/                路由与降级链
│   ├── detect/                 断连检测
│   ├── policy/                 限流与预算（令牌桶、token 预算）
│   ├── security/               出网校验、凭据脱敏
│   ├── usage/                  用量归一
│   ├── cost/                   费用计量（费率表驱动）
│   ├── errors/                 错误九分类
│   ├── telemetry/              日志与指标（异步批量）
│   └── version/                版本载体（-ldflags 注入点）
├── config/
│   ├── gateway.example.json    端点注册表样例
│   └── rates.json              费率表（费用计量数据源）
├── progress/                   事件流日文件（YYYY-MM-DD.md）
├── specs/                      需求规格
├── reference/                  外部参考代码（只读，不参与构建/测试/CI）
└── 20261114-*/                 实验与基准目录（一实验一目录：README + 原始产物 + 可复现脚本）
```

## 三、快速开始

需 Go 1.27+（[go.dev](https://go.dev/dl/)）。

**1. 构建**

```bash
go build -o bin/gateway ./cmd/gateway
```

**2. 配环境变量**（凭据只从环境变量读，⛔ 不入库、不进命令行明文）

| 变量 | 用途 |
|---|---|
| `<credential_ref>` | 上游凭据。注册表每条 endpoint 的 `credential_ref` 写**环境变量名**（如 `OPENAI_API_KEY`、`DEEPSEEK_API_KEY`），真值由环境注入 |
| `GATEWAY_CALLER_KEYS` | 调用方 API key 白名单（逗号 / 换行分隔）。非空 ⇒ `/v1/*` 强制鉴权；留空 = 开放模式 |
| `GATEWAY_ADMIN_TOKEN` | 管理面鉴权 token（仅开管理面时需要） |

**3. 配置文件**

- `config/gateway.example.json` —— 端点注册表（字段说明见「四、出入口使用 / 出口」）；
- `config/rates.json` —— 费率表（每模型各类 token 的绝对单价，费用计量数据源）。

**4. 运行**

```bash
./bin/gateway -config config/gateway.example.json -rates config/rates.json -listen 127.0.0.1:8080
```

**5. 验证**

```bash
curl http://127.0.0.1:8080/health
# {"status":"ok"}
```

## 四、出入口使用

### 4.1 入口（客户端 → 网关）

OpenAI 兼容 HTTP API。`GATEWAY_CALLER_KEYS` 非空时，`/v1/*` 须带
`Authorization: Bearer <key>`（缺失 / 无效 ⇒ 401）。

| 方法与路径 | 说明 |
|---|---|
| `POST /v1/chat/completions` | Chat Completions 兼容（流式 / 非流式）。`model` 填注册表的模型别名 |
| `POST /v1/responses` | OpenAI Responses 兼容入口（同一转发管线） |
| `GET /v1/models` | 列出注册表模型别名与能力（tools / stream / vision、上下文窗口、输出上限） |
| `POST /v1/cancel` | 显式取消在途执行，请求体 `{"client_request_id": "..."}` |
| `GET /health` | 存活探针（不鉴权） |

**非流式调用**：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_CALLER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-luna", "messages": [{"role": "user", "content": "你好"}], "stream": false}'
```

**流式调用**（`"stream": true`，收 SSE 帧；末帧带 usage 归一后计量）：

```bash
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_CALLER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-luna", "messages": [{"role": "user", "content": "你好"}], "stream": true}'
```

**请求标识与取消**：可选请求头 `X-Client-Request-Id` 携带本轮唯一标识（不传则网关生成），
后续用它取消：`POST /v1/cancel` 传 `{"client_request_id": "..."}`，返回 `{"cancelled": true|false}`
（fire-and-forget：查不到 / 已收口静默忽略）。同 id 已有执行在途 ⇒ 409 `request_in_flight`；
该 id 已进取消终态 ⇒ 409 `request_cancelled`。

**错误形态**：统一 OpenAI 风格 error 对象（`code` / `type` / `message`），错误九分类
（auth、invalid_request、request_too_large、request_in_flight、request_cancelled、
rate_limited、timeout、cancelled、upstream_error 等）；凭据类信息一律脱敏。

**请求上限**：请求体 32 MB（超出 413 `request_too_large`）。

### 4.2 出口（网关 → 上游）

出口由**端点注册表**（`-config` 指定的 JSON）描述，`strategy` 定路由策略，
`endpoints[]` 每条登记一个上游端点：

| 字段 | 含义 |
|---|---|
| `name` | 端点名（日志 / 指标用） |
| `base_url` | 上游地址（含 `/v1`） |
| `model` | **用户面模型别名**（客户端 `model` 字段填它） |
| `upstream_model` | 上游真实模型名（网关出站时替换） |
| `provider_name` | 适配器：`openai` / `anthropic` / `gemini` / `deepseek` / `qwen` / `zhipu` / `moonshot` / `minimax` / `xai` / `xiaomi` / `ollama` |
| `context_window` | 上下文窗口（token） |
| `max_output_tokens` | 输出上限（token） |
| `capabilities` | 能力标记：`supports_tools` / `supports_stream` / `supports_vision` |
| `credential_ref` | **凭据来源引用**（环境变量名，⛔ 不是凭据本身） |
| `timeout_s` | 单次上游调用超时（秒） |
| `max_retries` | 重试上限（重试打同一端点，与降级分开计数） |
| `priority` | 降级优先级（`strategy=priority` 时数值小者先） |
| `rate_limit` | 可选，套餐契约配额 `{tpm, rpm, burst_s}`（tokens/min、requests/min、桶深折算秒数）；**未登记 ⇒ 不限流**，⛔ 无默认值 |
| `concurrency_limit` | 可选，在途上游请求数上限（0 = 不限）；并发按**在途上游尝试**计（发起 +1 / 收口 −1，流式占位到流收口） |
| `limits_group` | 可选，配额共享组名：同组端点共享一份 `rate_limit` / `concurrency_limit` 计数（同供应商多模型 / 多 key / 多部署共享配额池的语义）；空 = 自成一组，同组声明必须逐字一致 |

- **路由语义**：`priority` = 显式有序降级链（主端点失败沿链回退，降级目标须可用，
  不可用显式报错）；`round_robin` = 轮转。路由决策可复现，顺序不依赖遍历偶然性。
- **配额与过载（锚点分离）**：限流锚定套餐契约（`rate_limit` / 顶层 `caller_rate_limits`
  按 key 登记，`key_ref` = 环境变量名），过载保护锚定实测容量（`--max-in-flight`）；
  两道闸并联判定，超限一律 `429` + `Retry-After`。多供应商混合流量下，每次上游尝试
  占它命中端点（或其共享组）的并发位与配额 —— 降级换供应商即换计数主体。
- **出站协议**：统一 Chat Completions 形（实测裁定），流式经 SSE 逐帧转发；
  usage 逐家归一后计量费用（费率表）。
- **出网校验**：默认拒绝内网目标地址；本地 Ollama 等环回上游用 `-allow-loopback` 放行。
- **管理面**（默认关闭）：`-admin-listen <内网地址>` + `GATEWAY_ADMIN_TOKEN` 开启后提供
  `GET /admin/endpoints`（注册表全量视图，只含凭据来源引用，可安全输出）与
  `GET /admin/health`。⛔ 管理面不绑公网。

## 五、运行参数

长选项与单字母缩写双形式等价（如 `--config` / `-c`）。

| 参数 | 短 | 说明 | 默认 |
|---|---|---|---|
| `--config` | `-c` | 端点注册表配置路径（JSON） | `config/gateway.example.json` |
| `--rates` | `-r` | 费率表路径（JSON） | `config/rates.json` |
| `--listen` | `-l` | 数据面监听地址（host:port） | `127.0.0.1:8080` |
| `--admin-listen` | `-a` | 管理面监听地址（须内网 IP，⛔ 不绑公网） | 关闭 |
| `--allow-loopback` | `-L` | 出网校验放行环回地址（本地 Ollama 等） | 关 |
| `--pprof-listen` | `-p` | pprof 排障端点（只绑环回，⛔ 不绑公网） | 关闭 |
| `--cancel-grace` | `-g` | 延迟取消窗口（0 = 立即取消） | `0` |
| `--cancel-grace-provider` | `-G` | 按 provider 覆盖延迟窗口（`provider=时长`，可重复） | — |
| `--detect-bound` | `-d` | 断连检测延迟上界 | `1s` |
| `--rate-capacity` | `-C` | token 限流桶容量（0 = 不限流） | `0` |
| `--rate-refill` | `-R` | token 限流补充速率（token/秒） | `0` |
| `--budget-tokens` | `-B` | 每调用方 token 预算上限（0 = 不限） | `0` |
| `--caller-bucket-capacity` | `-K` | 每调用方令牌桶的全局兜底容量（契约配额走 `caller_rate_limits`；0 = 关闭） | `0` |
| `--caller-bucket-refill` | `-E` | 每调用方令牌桶的全局兜底补充速率（token/秒） | `0` |
| `--upstream-bucket-capacity` | `-U` | 每自有令牌的本地令牌桶容量（契约配额走注册表 `rate_limit`；0 = 关闭） | `0` |
| `--upstream-bucket-refill` | `-u` | 每自有令牌的本地令牌桶补充速率（token/秒） | `0` |
| `--rate-limit-cooldown` | `-k` | 上游 429 无 Retry-After 时的令牌默认冷却 | `30s` |
| `--max-token-wait` | `-m` | 全部令牌受限时的最长等待（超上限 ⇒ 429 + Retry-After） | `3s` |
| `--max-in-flight` | `-i` | 过载保护：在途请求数上限（0 = 不限；按实测容量标定，超限 ⇒ 429 + Retry-After） | `0` |
| `--guardrail-pattern` | `-P` | 护栏正则（可重复） | — |
| `--guardrail-webhook` | `-W` | 护栏 Webhook URL（空 = 只做正则） | — |
| `--version` | — | 打印版本号后退出 | — |

## 六、性能对比（与 LiteLLM，简要）

受控基准（固定 300ms 上游延迟，闭合式 1→16384 并发指数阶梯，2 轮取中位；两侧同假上游同压测器；
生产形配置 = 契约桶开 + 并发闸 16000 + 过载闸 16500）：

![吞吐量与延迟](images/20261114-perf-throughput-latency.png)

| 指标 | FlyingGateway | LiteLLM |
|---|---|---|
| 峰值吞吐量（成功 req/s，2 轮中位） | **5797**（c=4096） | 219（c=256） |
| 端到端延迟 P50，c≤1024 | **301–316ms**（贴 300ms 上游基线） | 325ms 起爬升至 1.5s |
| 最大并发容量（成功在途） | **~16.5k**（超限 502 优雅拒绝） | ~2k（超限断连丢请求） |
| 单连接内存（RSS/连接） | **~146KB** | ~450KB |

![并发容量与单连接内存](images/20261114-perf-capacity-mem.png)

- 过载语义：FlyingGateway 超限返回 502 优雅拒绝、恢复 P50 1.5ms；LiteLLM 断连丢弃在途请求、恢复 267ms→4.9s。
- 口径注记：吞吐为**成功口径**；顶档（c≥8192）轮间方差大（一轮出 502 浪潮、一轮排队无失败），图为 2 轮中位。
- 单源 IP 出站本地端口 bind 排他（16384 端口池）是超高并发的硬上限；部署建议：多源 IP / 多实例横向扩出站。

## 七、测试与版本

```bash
gofmt -l .            # 格式门禁（有输出即失败）
go vet ./...          # 静态检查
go test -race ./...   # 全量测试（含竞态检测）
```

版本 = **git tag**（`v主.大.小`），⛔ 不在源码硬编码版本常量；需要运行时版本的构建用
`-ldflags` 注入 `internal/version` 的 `Version` 变量。协作规则见 [`AGENTS.md`](AGENTS.md)。
