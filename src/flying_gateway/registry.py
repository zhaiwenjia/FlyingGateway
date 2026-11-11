# -*- coding: utf-8 -*-
# 日期: 2026-11-11 20:03:47.226
# 作者: Zhai Wenjia
"""端点注册表：登记、查询、给出某个模型的候选端点。

职责边界：**只管「有哪些端点」**，不管「这次该发给谁」（那是
:mod:`flying_gateway.router` 的事）。分开的理由：注册表是可被多处复用的
事实来源，路由是每次请求都要重算的决策；混在一起会让「改一次路由策略」
顺手改到事实数据上。
"""

from __future__ import annotations

from collections.abc import Iterable

from flying_gateway.config import Endpoint


__version__ = "0.1.0"


class RegistryError(KeyError):
    """注册表操作非法（重名、找不到等）。"""


class EndpointRegistry:
    """按名字索引的端点注册表。

    ⚠️ **单一数据源**：同一个端点只允许登记一次，重复登记直接报错。
    「后写的覆盖先写的」会让「当前实际生效的端点」变成一个要靠调用顺序猜
    的东西 —— 而网关出问题时，第一句要问的永远是「现在到底配的是哪个」。
    """

    def __init__(self, endpoints: Iterable[Endpoint] = ()) -> None:
        self._by_name: dict[str, Endpoint] = {}
        for ep in endpoints:
            self.register(ep)

    def register(self, endpoint: Endpoint) -> Endpoint:
        """登记一个端点；重名则抛 :class:`RegistryError`。"""
        if endpoint.name in self._by_name:
            raise RegistryError(f"endpoint already registered: {endpoint.name!r}")
        self._by_name[endpoint.name] = endpoint
        return endpoint

    def remove(self, name: str) -> Endpoint:
        """移除一个端点；不存在则抛 :class:`RegistryError`。"""
        if name not in self._by_name:
            raise RegistryError(f"unknown endpoint: {name!r}")
        return self._by_name.pop(name)

    def get(self, name: str) -> Endpoint:
        """按名字取端点；不存在则抛 :class:`RegistryError`。"""
        if name not in self._by_name:
            raise RegistryError(f"unknown endpoint: {name!r}")
        return self._by_name[name]

    def names(self) -> list[str]:
        """已登记的端点名（按名字排序，便于稳定输出）。"""
        return sorted(self._by_name)

    def all(self) -> list[Endpoint]:
        """已登记的全部端点（按优先级升序，**同级按名字排**）。

        ⚠️ 排序必须是**稳定且完全确定**的：同优先级的端点若顺序不定，
        同一份配置两次启动会给出不同的降级链 ⇒ 故障复现不了。
        """
        return sorted(self._by_name.values(), key=lambda e: (e.priority, e.name))

    def by_model(self, model: str) -> list[Endpoint]:
        """某个模型名对应的全部端点（同样按优先级升序、同级按名字排）。"""
        return [e for e in self.all() if e.model == model]

    def models(self) -> list[str]:
        """注册表里出现过的模型名（去重后排序）。"""
        return sorted({e.model for e in self._by_name.values()})
