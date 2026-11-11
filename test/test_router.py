# -*- coding: utf-8 -*-
# 日期: 2026-11-11 20:11:22.415
# 作者: Zhai Wenjia
"""``flying_gateway.router`` 的单元测试。"""

from __future__ import annotations

import unittest

from flying_gateway.config import Endpoint
from flying_gateway.registry import EndpointRegistry, RegistryError
from flying_gateway.router import Router


def _endpoint(
    name: str, model: str = "m", priority: int = 0, retries: int = 2
) -> Endpoint:
    return Endpoint(
        name=name,
        base_url=f"https://{name}.example.com/v1",
        model=model,
        context_window=1000,
        max_retries=retries,
        priority=priority,
    )


class TestPriorityStrategy(unittest.TestCase):
    """``priority``：取优先级最高（数值最小）的端点。"""

    def setUp(self):
        self.registry = EndpointRegistry([
            _endpoint("backup", priority=1),
            _endpoint("primary", priority=0),
            _endpoint("local", priority=2),
        ])
        self.router = Router(self.registry, strategy="priority")

    def test_select_returns_highest_priority(self):
        self.assertEqual(self.router.select("m").name, "primary")

    def test_select_is_deterministic(self):
        """同一份注册表 + 同一个请求 ⇒ 同一个决策（可复现是硬要求）。"""
        names = [self.router.select("m").name for _ in range(5)]
        self.assertEqual(names, ["primary"] * 5)

    def test_fallback_chain_excludes_primary_and_is_ordered(self):
        primary = self.router.select("m")
        chain = self.router.fallback_chain(primary)
        self.assertEqual([e.name for e in chain], ["backup", "local"])

    def test_fallback_chain_only_same_model(self):
        registry = EndpointRegistry([
            _endpoint("a", model="m1", priority=0),
            _endpoint("b", model="m2", priority=1),
        ])
        router = Router(registry)
        chain = router.fallback_chain(router.select("m1"))
        self.assertEqual(chain, [])

    def test_unknown_model_raises(self):
        """没有端点的模型必须报错，不许静默 fallback 到一个默认端点。"""
        with self.assertRaises(RegistryError):
            self.router.select("nope")


class TestRoundRobinStrategy(unittest.TestCase):
    """``round_robin``：轮转摊平负载，轮转位置由显式状态维护。"""

    def setUp(self):
        self.registry = EndpointRegistry([
            _endpoint("a", priority=0),
            _endpoint("b", priority=1),
            _endpoint("c", priority=2),
        ])
        self.router = Router(self.registry, strategy="round_robin")

    def test_select_cycles(self):
        seen = []
        for _ in range(6):
            seen.append(self.router.select("m").name)
            self.router.advance()
        self.assertEqual(seen, ["a", "b", "c", "a", "b", "c"])

    def test_advance_is_noop_for_priority(self):
        router = Router(self.registry, strategy="priority")
        router.advance()
        self.assertEqual(router.select("m").name, "a")

    def test_cursor_state_is_explicit_not_derived_from_registry_order(self):
        """轮转位置不许依赖 dict 迭代顺序 —— 那样两次启动顺序会不同。"""
        r1 = Router(self.registry, strategy="round_robin")
        r2 = Router(self.registry, strategy="round_robin")
        self.assertEqual(r1.select("m").name, r2.select("m").name)


class TestRetryBudget(unittest.TestCase):
    """重试预算必须是有限值。"""

    def test_retry_budget_comes_from_endpoint(self):
        registry = EndpointRegistry([_endpoint("a", retries=3)])
        router = Router(registry)
        self.assertEqual(router.retry_budget(router.select("m")), 3)

    def test_zero_retries_allowed(self):
        registry = EndpointRegistry([_endpoint("a", retries=0)])
        router = Router(registry)
        self.assertEqual(router.retry_budget(router.select("m")), 0)


class TestRouterConstruction(unittest.TestCase):
    def test_unsupported_strategy_rejected(self):
        with self.assertRaises(RegistryError):
            Router(EndpointRegistry(), strategy="random")

    def test_property_returns_strategy(self):
        router = Router(EndpointRegistry(), strategy="round_robin")
        self.assertEqual(router.strategy, "round_robin")


if __name__ == "__main__":
    unittest.main()
