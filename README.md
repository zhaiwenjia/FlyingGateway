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

当前实现到「**决定发给谁**」为止：装配、路由决策、配置口径都已落地并有测试覆盖，
**HTTP 客户端那一层还没接**（见 `specs/`）。因此 CLI **不发任何网络请求**，
也不会伪造一份「响应」出来。

## 二、目录结构

⚠️ **本段的数字全部按实测写**（口径：`find src -name '*.py' | wc -l`），
改动目录结构后请重新实测再改这里（`AGENTS.md` 铁律 2：文档描述必须与实况一致）。

> 当前实测（2026-11-11 首版骨架）：`src/` **4** 个 `.py`
> （`config.py` `registry.py` `router.py` `__init__.py`）、
> `test/` **4** 个 `.py`（3 个测试文件 + `__init__.py`）、
> 单测 **38** 项。

```
FlyingGateway/                ← 仓库根
├── main.py                   ★ 程序入口（CLI）：加载配置 → 建注册表 → 路由决策
├── bump_version.py           SemVer bump + 组件版本快照（双轨制）
├── pyproject.toml            uv 项目配置 + 依赖声明 + ruff 配置
├── uv.lock                   uv 锁文件
├── .python-version           3.12
├── .env                      运行配置（含真值，不提交）
├── .env.example              配置模板（提交，告诉别人要配哪些项）
├── .gitignore
├── AGENTS.md                 ★ 协作规则的唯一来源
├── README.md
├── LICENSE
├── config/
│   └── gateway.example.json  示例网关配置
├── docs/                     调研与结论文档
│   ├── README.md
│   └── images/               项目图等静态资源
├── progress/                 事件流日文件（YYYY-MM-DD.md）
│   ├── README.md
│   └── 2026-11-11.md
├── specs/                    需求规格（spec 驱动）
│   └── README.md
├── snapshots/                bump_version.py 生成的组件版本快照
├── src/flying_gateway/       源码包
│   ├── __init__.py
│   ├── config.py             端点与网关配置（数据结构 + 校验）
│   ├── registry.py           端点注册表（登记 / 查询 / 排序）
│   └── router.py             路由决策与降级链
└── test/                     单元测试（unittest，不带 s）
    ├── __init__.py
    ├── test_config.py
    ├── test_registry.py
    └── test_router.py
```

## 三、快速开始

需 Python 3.12 与 [uv](https://docs.astral.sh/uv/)。

```bash
uv sync                            # 按 uv.lock 创建 .venv 并安装依赖
uv run python main.py --list       # 列出已登记的端点与模型
uv run python main.py --model gpt-4o-mini   # 打印一次路由决策
```

改配置：复制 `config/gateway.example.json`，改完用 `--config <路径>` 指定，
或设环境变量 `GATEWAY_CONFIG`。

## 四、协作

本仓的协作规则（禁止 mock / 占位、证据与复核、双轨 SemVer、CI 口径、
调研纪律、代码规模两档等）全在 [`AGENTS.md`](AGENTS.md) —— **那是唯一来源**。
事件流在 `progress/`，技术结论在 `docs/`。
