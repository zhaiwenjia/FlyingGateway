# -*- coding: utf-8 -*-
# 日期: 2026-11-11 20:09:58.706
# 作者: Zhai Wenjia
"""``flying_gateway.registry`` 的单元测试。"""

from __future__ import annotations

import unittest

from flying_gateway.config import Endpoint
from flying_gateway.registry import EndpointRegistry, RegistryError


def _endpoint(name: str, model: str = "m", priority: int = 0) -> Endpoint:
    return Endpoint(
        name=name,
        base_url=f"https://{name}.example.com/v1",
        model=model,
        context_window=1000,
        priority=priority,
    )


class TestRegistryBasics(unittest.TestCase):
    """登记、查询、移除。"""

    def test_register_and_get(self):
        reg = EndpointRegistry()
        ep = reg.register(_endpoint("a"))
        self.assertIs(reg.get("a"), ep)

    def test_duplicate_register_rejected(self):
        """重复登记必须报错 —— 「后写的覆盖先写的」会让实际生效端点变成要靠
        调用顺序猜的东西。"""
        reg = EndpointRegistry()
        reg.register(_endpoint("a"))
        with self.assertRaises(RegistryError):
            reg.register(_endpoint("a"))

    def test_get_unknown_raises(self):
        reg = EndpointRegistry()
        with self.assertRaises(RegistryError):
            reg.get("nope")

    def test_remove_unknown_raises(self):
        reg = EndpointRegistry()
        with self.assertRaises(RegistryError):
            reg.remove("nope")

    def test_remove_returns_removed_endpoint(self):
        reg = EndpointRegistry([_endpoint("a")])
        ep = reg.remove("a")
        self.assertEqual(ep.name, "a")
        self.assertEqual(reg.names(), [])

    def test_names_sorted(self):
        reg = EndpointRegistry([_endpoint("b"), _endpoint("a")])
        self.assertEqual(reg.names(), ["a", "b"])

    def test_models_deduped_and_sorted(self):
        reg = EndpointRegistry([
            _endpoint("a", model="z"),
            _endpoint("b", model="a"),
            _endpoint("c", model="z"),
        ])
        self.assertEqual(reg.models(), ["a", "z"])


class TestRegistryOrdering(unittest.TestCase):
    """排序必须完全确定，否则故障复现不了。"""

    def test_all_sorted_by_priority_then_name(self):
        reg = EndpointRegistry([
            _endpoint("c", priority=1),
            _endpoint("b", priority=0),
            _endpoint("a", priority=0),
        ])
        self.assertEqual([e.name for e in reg.all()], ["a", "b", "c"])

    def test_same_priority_order_is_stable_across_insertion_orders(self):
        """同优先级端点的顺序不许随登记顺序变 —— 否则同一份配置两次启动
        会给出不同的降级链。"""
        eps = [_endpoint("a", priority=0), _endpoint("b", priority=0)]
        first = [e.name for e in EndpointRegistry(eps).all()]
        second = [e.name for e in EndpointRegistry(list(reversed(eps))).all()]
        self.assertEqual(first, second)

    def test_by_model_filters(self):
        reg = EndpointRegistry([
            _endpoint("a", model="m1"),
            _endpoint("b", model="m2"),
            _endpoint("c", model="m1"),
        ])
        self.assertEqual([e.name for e in reg.by_model("m1")], ["a", "c"])
        self.assertEqual(reg.by_model("nope"), [])


if __name__ == "__main__":
    unittest.main()
