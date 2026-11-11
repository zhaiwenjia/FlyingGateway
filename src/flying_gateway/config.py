# -*- coding: utf-8 -*-
# 日期: 2026-11-11 20:02:31.509
# 作者: Zhai Wenjia
"""网关配置：端点定义与网关级配置。

⛔ **字段名不许撒谎**（AGENTS.md 的证据条）：本模块里每个字段的语义就是
它字面意思。历史上最贵的一类 bug 不是「值算错了」，而是「**字段名与实际
口径不符**」—— 可配多种策略时把其中一种的分数记成另一个的名字，下游分析
就会**持续**按错的口径解读。
⇒ 本模块只放**单义字段**；真要上多策略，字段名必须自带策略限定词
（如 ``fused_score``），不许用裸 ``score``。
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path


__version__ = "0.1.0"


#: 单次请求默认超时（秒）。
DEFAULT_TIMEOUT_S = 30.0
#: 单个端点默认最大重试次数（**有上限**：不许无限重试，见 AGENTS.md 的网关条）。
DEFAULT_MAX_RETRIES = 2
#: 默认路由策略。
DEFAULT_STRATEGY = "priority"

#: 支持的路由策略。
SUPPORTED_STRATEGIES = ("priority", "round_robin")


class ConfigError(ValueError):
    """配置非法。

    ⛔ **不许降级成「用默认值接着跑」** —— 配置错了就停下来告知，
    否则一次手滑的 base_url 会让所有请求静默打到错误的端点上。
    """


@dataclass(frozen=True)
class Endpoint:
    """一个 LLM 端点。

    字段全是单义的：
    - ``name``      端点标识，**全局唯一**（注册表按它索引）；
    - ``base_url``  OpenAI 兼容端点的根地址；
    - ``model``     该端点上的模型名；
    - ``context_window`` 上下文窗口（token 数）；
    - ``timeout_s`` 单次请求超时（秒）；
    - ``max_retries`` 该端点允许的最大重试次数；
    - ``priority``  优先级，**数值越小越优先**（降级链按它升序排）。
    """

    name: str
    base_url: str
    model: str
    context_window: int
    timeout_s: float = DEFAULT_TIMEOUT_S
    max_retries: int = DEFAULT_MAX_RETRIES
    priority: int = 0

    def __post_init__(self) -> None:
        if not self.name.strip():
            raise ConfigError("endpoint.name must not be empty")
        if not self.base_url.startswith(("http://", "https://")):
            raise ConfigError(
                f"endpoint {self.name!r}: base_url must start with http:// or https://, "
                f"got {self.base_url!r}"
            )
        if not self.model.strip():
            raise ConfigError(f"endpoint {self.name!r}: model must not be empty")
        if self.context_window <= 0:
            raise ConfigError(
                f"endpoint {self.name!r}: context_window must be positive, "
                f"got {self.context_window!r}"
            )
        if self.timeout_s <= 0:
            raise ConfigError(
                f"endpoint {self.name!r}: timeout_s must be positive, "
                f"got {self.timeout_s!r}"
            )
        if self.max_retries < 0:
            raise ConfigError(
                f"endpoint {self.name!r}: max_retries must not be negative, "
                f"got {self.max_retries!r}"
            )


@dataclass(frozen=True)
class GatewayConfig:
    """网关级配置。

    - ``endpoints`` 已登记的端点；
    - ``strategy``  路由策略，取值见 :data:`SUPPORTED_STRATEGIES`。
    """

    endpoints: tuple[Endpoint, ...]
    strategy: str = DEFAULT_STRATEGY

    def __post_init__(self) -> None:
        if self.strategy not in SUPPORTED_STRATEGIES:
            raise ConfigError(
                f"unsupported strategy {self.strategy!r}; "
                f"expected one of {SUPPORTED_STRATEGIES}"
            )
        seen: set[str] = set()
        for ep in self.endpoints:
            if ep.name in seen:
                raise ConfigError(f"duplicate endpoint name: {ep.name!r}")
            seen.add(ep.name)

    @classmethod
    def from_mapping(cls, data: dict) -> "GatewayConfig":
        """从 ``dict``（如 JSON 反序列化结果）构造配置。

        ⚠️ **未知键直接报错**，不静默忽略 —— 拼错的字段名如果被忽略，
        用户会以为它生效了（AGENTS.md 的证据条：文档/字段必须与实况一致）。
        """
        known = {"endpoints", "strategy"}
        unknown = set(data) - known
        if unknown:
            raise ConfigError(f"unknown config keys: {sorted(unknown)}")

        raw_endpoints = data.get("endpoints", [])
        if not isinstance(raw_endpoints, list):
            raise ConfigError("endpoints must be a list")

        endpoints = []
        for raw in raw_endpoints:
            if not isinstance(raw, dict):
                raise ConfigError(
                    f"endpoint entry must be a mapping, got {type(raw)!r}"
                )
            endpoint_known = {
                "name",
                "base_url",
                "model",
                "context_window",
                "timeout_s",
                "max_retries",
                "priority",
            }
            extra = set(raw) - endpoint_known
            if extra:
                raise ConfigError(
                    f"unknown endpoint keys: {sorted(extra)} (endpoint "
                    f"{raw.get('name', '<unnamed>')!r})"
                )
            missing = {"name", "base_url", "model", "context_window"} - set(raw)
            if missing:
                raise ConfigError(
                    f"endpoint {raw.get('name', '<unnamed>')!r} missing keys: "
                    f"{sorted(missing)}"
                )
            endpoints.append(Endpoint(**raw))

        return cls(
            endpoints=tuple(endpoints),
            strategy=data.get("strategy", DEFAULT_STRATEGY),
        )

    @classmethod
    def load(cls, path: str | Path) -> "GatewayConfig":
        """从 JSON 文件读取配置。

        ⛔ 文件不存在 / 不是合法 JSON / 结构不合法 ⇒ 一律抛
        :class:`ConfigError`，**不返回空配置**（空配置会让网关「看起来正常」
        却一个请求都发不出去）。
        """
        p = Path(path)
        try:
            text = p.read_text(encoding="utf-8")
        except FileNotFoundError as exc:
            raise ConfigError(f"config file not found: {p}") from exc
        try:
            data = json.loads(text)
        except json.JSONDecodeError as exc:
            raise ConfigError(f"config file is not valid JSON: {p}: {exc}") from exc
        if not isinstance(data, dict):
            raise ConfigError(f"config file must contain a JSON object: {p}")
        return cls.from_mapping(data)
