# -*- coding: utf-8 -*-
# 日期: 2026-11-11 20:05:12.884
# 作者: Zhai Wenjia
"""路由决策：选主端点、给降级链。

两个策略：
- ``priority``    取该模型下**优先级最高**（数值最小）的端点；
- ``round_robin`` 在该模型的所有端点之间轮转，摊平负载。

⚠️ **决策必须可复现**：同一份注册表 + 同一个请求序列 ⇒ 同一串决策。
``round_robin`` 的轮转位置由**显式状态**维护（不依赖进程启动时刻、
不依赖 dict 迭代顺序），否则两次启动的路由顺序会不一样 ⇒ 故障复现不了。
"""

from __future__ import annotations

from flying_gateway.config import Endpoint
from flying_gateway.registry import EndpointRegistry, RegistryError


__version__ = "0.1.0"


class Router:
    """按策略在注册表里选端点。

    典型用法::

        router = Router(registry, strategy="priority")
        endpoint = router.select("gpt-4o-mini")
        chain = router.fallback_chain(endpoint)
    """

    def __init__(self, registry: EndpointRegistry, strategy: str = "priority") -> None:
        if strategy not in ("priority", "round_robin"):
            raise RegistryError(f"unsupported strategy: {strategy!r}")
        self._registry = registry
        self._strategy = strategy
        self._rr_cursor = 0

    @property
    def strategy(self) -> str:
        """当前路由策略名。"""
        return self._strategy

    def candidates(self, model: str) -> list[Endpoint]:
        """某个模型的候选端点（按优先级升序、同级按名字排）。"""
        return self._registry.by_model(model)

    def select(self, model: str) -> Endpoint:
        """为主请求选一个端点。

        ⛔ **该模型一个端点都没有时抛错**，不许返回一个「默认端点」——
        静默 fallback 到一个不存在的端点，症状是「所有请求 404」，
        而真因（模型没配）在日志里一个字都看不到。
        """
        cands = self.candidates(model)
        if not cands:
            raise RegistryError(f"no endpoint registered for model {model!r}")
        if self._strategy == "priority":
            return cands[0]
        # round_robin：按显式游标轮转
        return cands[self._rr_cursor % len(cands)]

    def advance(self) -> None:
        """轮转游标前进一格（仅 ``round_robin`` 有意义）。"""
        if self._strategy == "round_robin":
            self._rr_cursor += 1

    def fallback_chain(self, primary: Endpoint) -> list[Endpoint]:
        """给定主端点，返回**降级链**（不含主端点本身）。

        降级链 = 同模型的其余端点，按优先级升序、同级按名字排。
        ⚠️ 链必须是**显式有序**的：失败后往哪儿退，是要写进事故报告的事实，
        不是一个「差不多就行」的集合。
        """
        return [e for e in self.candidates(primary.model) if e.name != primary.name]

    def retry_budget(self, endpoint: Endpoint) -> int:
        """某端点的重试次数上限（含首次请求在内的额外尝试数）。

        ⛔ 必须是**有限**的：无限重试会把「端点持续 5xx」变成一次永远跑不完的
        挂起，而挂起比失败更难排查。
        """
        return endpoint.max_retries
