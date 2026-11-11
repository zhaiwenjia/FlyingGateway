# -*- coding: utf-8 -*-
# 日期: 2026-11-11 20:08:44.193
# 作者: Zhai Wenjia
"""``flying_gateway.config`` 的单元测试。"""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from flying_gateway.config import ConfigError, Endpoint, GatewayConfig


def _endpoint(**overrides) -> Endpoint:
    base = {
        "name": "primary",
        "base_url": "https://api.example.com/v1",
        "model": "m1",
        "context_window": 1000,
    }
    base.update(overrides)
    return Endpoint(**base)


class TestEndpointValidation(unittest.TestCase):
    """端点字段的边界校验。"""

    def test_valid_endpoint_uses_defaults(self):
        ep = _endpoint()
        self.assertEqual(ep.timeout_s, 30.0)
        self.assertEqual(ep.max_retries, 2)
        self.assertEqual(ep.priority, 0)

    def test_empty_name_rejected(self):
        with self.assertRaises(ConfigError):
            _endpoint(name="  ")

    def test_non_http_base_url_rejected(self):
        with self.assertRaises(ConfigError):
            _endpoint(base_url="api.example.com/v1")

    def test_non_positive_context_window_rejected(self):
        with self.assertRaises(ConfigError):
            _endpoint(context_window=0)

    def test_non_positive_timeout_rejected(self):
        with self.assertRaises(ConfigError):
            _endpoint(timeout_s=0)

    def test_negative_retries_rejected(self):
        with self.assertRaises(ConfigError):
            _endpoint(max_retries=-1)


class TestGatewayConfig(unittest.TestCase):
    """网关级配置的构造与校验。"""

    def test_duplicate_endpoint_name_rejected(self):
        with self.assertRaises(ConfigError):
            GatewayConfig(endpoints=(_endpoint(), _endpoint()))

    def test_unsupported_strategy_rejected(self):
        with self.assertRaises(ConfigError):
            GatewayConfig(endpoints=(_endpoint(),), strategy="random")

    def test_from_mapping_unknown_top_key_rejected(self):
        with self.assertRaises(ConfigError):
            GatewayConfig.from_mapping({"endpoints": [], "extra": 1})

    def test_from_mapping_unknown_endpoint_key_rejected(self):
        """未知字段必须报错 —— 拼错的键若被静默忽略，用户会以为它生效了。"""
        with self.assertRaises(ConfigError):
            GatewayConfig.from_mapping({
                "endpoints": [
                    {
                        "name": "a",
                        "base_url": "https://x.example.com",
                        "model": "m",
                        "context_window": 1,
                        "time_out_s": 5,
                    }
                ]
            })

    def test_from_mapping_missing_required_key_rejected(self):
        with self.assertRaises(ConfigError):
            GatewayConfig.from_mapping({"endpoints": [{"name": "a"}]})

    def test_from_mapping_happy_path(self):
        cfg = GatewayConfig.from_mapping({
            "strategy": "round_robin",
            "endpoints": [
                {
                    "name": "a",
                    "base_url": "https://x.example.com",
                    "model": "m",
                    "context_window": 10,
                }
            ],
        })
        self.assertEqual(cfg.strategy, "round_robin")
        self.assertEqual(len(cfg.endpoints), 1)
        self.assertEqual(cfg.endpoints[0].name, "a")


class TestGatewayConfigLoad(unittest.TestCase):
    """从 JSON 文件加载。"""

    def _write(self, content: str) -> str:
        tmp = tempfile.NamedTemporaryFile(
            "w", suffix=".json", delete=False, encoding="utf-8"
        )
        with tmp:
            tmp.write(content)
        self.addCleanup(Path(tmp.name).unlink)
        return tmp.name

    def test_missing_file_raises_config_error(self):
        with self.assertRaises(ConfigError):
            GatewayConfig.load("/nonexistent/path/gateway.json")

    def test_invalid_json_raises_config_error(self):
        path = self._write("{ not json ")
        with self.assertRaises(ConfigError):
            GatewayConfig.load(path)

    def test_non_object_json_raises_config_error(self):
        path = self._write("[1, 2, 3]")
        with self.assertRaises(ConfigError):
            GatewayConfig.load(path)

    def test_valid_json_loads(self):
        payload = {
            "strategy": "priority",
            "endpoints": [
                {
                    "name": "a",
                    "base_url": "https://x.example.com",
                    "model": "m",
                    "context_window": 7,
                }
            ],
        }
        path = self._write(json.dumps(payload))
        cfg = GatewayConfig.load(path)
        self.assertEqual(cfg.endpoints[0].context_window, 7)


if __name__ == "__main__":
    unittest.main()
