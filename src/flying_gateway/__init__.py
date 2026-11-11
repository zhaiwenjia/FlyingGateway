# -*- coding: utf-8 -*-
# 日期: 2026-11-11 20:01:44.812
# 作者: Zhai Wenjia
"""
flying_gateway —— 智能体 LLM 网关。

分层（每层只管一件事）：
- ``config``   端点与网关级配置的数据结构与校验；
- ``registry`` 端点注册表：登记、查询、按模型给出有序候选；
- ``router``   路由决策：按策略挑主端点、按优先级给出降级链。

⚠️ 本包**不含 HTTP 客户端**：发请求那一层还没进来（见 AGENTS.md 的
「先接线再扩展」口径）。这里做的是**决定发给谁**，那部分不依赖任何网络库。
"""

from flying_gateway.config import (
    ConfigError,
    Endpoint,
    GatewayConfig,
)
from flying_gateway.registry import EndpointRegistry, RegistryError
from flying_gateway.router import Router

__all__ = [
    "ConfigError",
    "Endpoint",
    "EndpointRegistry",
    "GatewayConfig",
    "RegistryError",
    "Router",
]
