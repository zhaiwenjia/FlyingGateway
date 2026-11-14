<div align="center">

<img src="docs/images/flying-gateway-icon.jpg" alt="FlyingGateway" width="220">

# FlyingGateway

**智能体 LLM 网关** —— 多模型端点注册、请求路由与分发、失败重试与降级。

</div>

## 一、项目概述

面向智能体（agent）的 LLM 网关。一个 agent 的请求往往要打到**不同模型**上
（快的打热路径、强的打难任务、贵的要省着），网关就是中间那层：

- **注册** —— 登记可用端点（地址、模型、上下文窗口、超时、重试预算、优先级）；
- **路由** —— 按策略决定主端点（`priority` 优先级 / `round_robin` 轮转）；
- **降级** —— 主端点失败时沿一条**显式有序**的降级链回退。

**技术栈：Go**（实现设计见 `docs/`）。当前进度：**约束体系与 CI 已就绪**
（AGENTS.md 规则、Go 门禁流水线、git tag 版本机制），**实现代码待写**。

## 二、目录结构

⚠️ **本段按实测写**（口径：`find . -name '*.go' | wc -l`），
改动目录结构后请重新实测再改这里（`AGENTS.md` 铁律 2：文档描述必须与实况一致）。

> 当前实测（2026-11-14 约束 Go 化）：`.go` 文件 **1** 个（`internal/version`），
> 实现代码待写；测试 **0** 项。

```
FlyingGateway/                ← 仓库根
├── go.mod                    module github.com/zhaiwenjia/FlyingGateway（go 1.27）
├── .env                      运行配置（含真值，不提交）
├── .env.example              配置模板（提交，告诉别人要配哪些项）
├── .gitignore
├── AGENTS.md                 ★ 协作规则的唯一来源
├── README.md
├── LICENSE
├── internal/
│   └── version/              版本载体（ldflags 注入点）
│       └── version.go
├── cmd/                      程序入口（待建）
├── config/
│   └── gateway.example.json  示例网关配置
├── docs/                     调研与结论文档
│   ├── README.md
│   └── images/               项目图等静态资源
├── progress/                 事件流日文件（YYYY-MM-DD.md）
│   ├── README.md
│   └── 2026-11-1x.md
├── specs/                    需求规格（spec 驱动）
│   └── README.md
└── reference/                外部参考代码（只读，不参与构建/测试/CI）
```

## 三、快速开始

需 Go 1.27+（[go.dev](https://go.dev/dl/)）。

```bash
go build ./...                   # 编译
go test -race ./...              # 全量测试（含竞态检测）
go run ./cmd/gateway             # 运行入口（待建）
```

## 四、文档索引

| 文档 | 内容 |
|---|---|
| [`docs/20261114-统一SDD.md`](docs/20261114-统一SDD.md) | ★ **开发唯一标准**：统一软件设计文档（产品需求基线 + 五域设计 + 供应商契约 + 费用计量与 6.4 费率总表 + 测试与验收），自包含，冲突以它为准 |
| [`docs/全文 SDD.md`](docs/全文%20SDD.md) | 产品场景与需求原始出处（跨境数字员工 W0：场景、PRD、用户故事、12 周计划） |
| [`docs/20261112-网关能力-调研-整体设计.md`](docs/20261112-网关能力-调研-整体设计.md) | 网关能力五域调研（协议/注册/执行、路由/上下文、错误/治理、可观测、安全），含流式取消专题（证据留档） |
| [`docs/20261112-多供应商API-Schema-调研-接口与用量口径.md`](docs/20261112-多供应商API-Schema-调研-接口与用量口径.md) | 11 家供应商的 API schema 与 token 用量口径对照（证据留档） |
| [`docs/20261112-多供应商网关-Go实现设计.md`](docs/20261112-多供应商网关-Go实现设计.md) | Go 实现设计（统一格式、出站优先级链、取消语义、费用计量；结论已吸收进统一 SDD） |
| [`docs/20261113-供应商模型费率表.md`](docs/20261113-供应商模型费率表.md) | 供应商模型费率表（2026-11-13 快照留档；**费用数据的唯一真源是统一 SDD 6.4 费率总表**） |

## 五、协作

本仓的协作规则（禁止 mock / 占位、证据与复核、git tag 版本机制、CI 口径、
调研纪律、代码规模两档等）全在 [`AGENTS.md`](AGENTS.md) —— **那是唯一来源**。
事件流在 `progress/`，技术结论在 `docs/`。
