# -*- coding: utf-8 -*-
# 日期: 2026-11-11 20:07:03.541
# 作者: Zhai Wenjia
"""FlyingGateway 的程序入口（CLI）。

用法::

    # 打印一次路由决策（不发请求：本仓还没接 HTTP 客户端）
    uv run python main.py --config config/gateway.example.json --model gpt-4o-mini

    # 列出注册表里的全部端点与模型
    uv run python main.py --config config/gateway.example.json --list

⚠️ **本入口只做「决定发给谁」**：加载配置 → 建注册表 → 选主端点 → 给降级链。
真正发请求的那一层还没进来（见 AGENTS.md 的分层口径）。
⇒ 所以这里**不发任何网络请求**，也不会伪造一份「响应」出来。
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

from flying_gateway.config import ConfigError, GatewayConfig
from flying_gateway.registry import EndpointRegistry, RegistryError
from flying_gateway.router import Router


__version__ = "0.1.0"

DEFAULT_CONFIG_PATH = "config/gateway.example.json"


def _parse_argv(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="main.py",
        description="FlyingGateway —— 智能体 LLM 网关（当前实现：路由决策）",
    )
    parser.add_argument(
        "--config",
        default=os.environ.get("GATEWAY_CONFIG", DEFAULT_CONFIG_PATH),
        help=(
            f"网关配置文件路径（默认 {DEFAULT_CONFIG_PATH}；"
            "环境变量 GATEWAY_CONFIG 优先于默认值）"
        ),
    )
    parser.add_argument(
        "--model",
        default=None,
        help="要路由的模型名；省略时取配置里第一个模型",
    )
    parser.add_argument(
        "--list",
        action="store_true",
        help="只列出已登记的端点与模型，不做路由决策",
    )
    return parser.parse_args(argv)


def _print_registry(registry: EndpointRegistry) -> None:
    print(f"端点（{len(registry.names())} 个）：")
    for ep in registry.all():
        print(
            f"  {ep.name:<16} model={ep.model:<20} "
            f"priority={ep.priority} timeout={ep.timeout_s}s "
            f"max_retries={ep.max_retries} ctx={ep.context_window}"
        )
    print(f"模型：{', '.join(registry.models()) or '(无)'}")


def _print_decision(router: Router, model: str) -> None:
    primary = router.select(model)
    chain = router.fallback_chain(primary)
    decision = {
        "model": model,
        "strategy": router.strategy,
        "primary": {
            "name": primary.name,
            "base_url": primary.base_url,
            "timeout_s": primary.timeout_s,
            "retry_budget": router.retry_budget(primary),
        },
        "fallback_chain": [e.name for e in chain],
    }
    print("路由决策：")
    print(json.dumps(decision, ensure_ascii=False, indent=2))


def main(argv: list[str] | None = None) -> int:
    args = _parse_argv(list(sys.argv[1:] if argv is None else argv))

    try:
        config = GatewayConfig.load(args.config)
    except ConfigError as exc:
        print(f"启动失败：配置不可用 —— {exc}", file=sys.stderr)
        return 1

    registry = EndpointRegistry(config.endpoints)
    router = Router(registry, strategy=config.strategy)

    if args.list:
        _print_registry(registry)
        return 0

    model = args.model
    if model is None:
        models = registry.models()
        if not models:
            print("启动失败：配置里一个端点都没有。", file=sys.stderr)
            return 1
        model = models[0]

    try:
        _print_decision(router, model)
    except RegistryError as exc:
        print(f"路由失败：{exc}", file=sys.stderr)
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
