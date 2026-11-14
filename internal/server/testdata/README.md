# testdata fixture 来源标注

本目录的 SSE fixture 是**上游假服务回放的响应样本**，字段形状逐字按《`docs/20261114-统一SDD.md`》
4.3 各家「请求/响应 schema 要点」的文档字段构造（⛔ 不手写臆造形状；数值为样本值，语义与字段名
以 SDD 4.3 与 OpenAI / Anthropic 官方 API 参考为准）。集成测试用 httptest 起真实 HTTP 假上游，
按事件逐条回放本目录样本（时序控制只加延迟 / 停顿，不改字段）。

| 文件 | 形状 | 来源（字段口径） | 用途 |
|---|---|---|---|
| `openai_responses_stream.sse` | OpenAI Responses 流式事件（`event:` + `data:` 双字段、`sequence_number`、`response.created` / `response.output_text.delta` / `response.completed` / `data: [DONE]`） | SDD 4.3.1（OpenAI「统一格式的基准」） | 双入口跑通、11.4 帧序列/末帧 usage/终止哨兵、SC-1–SC-8 主样本；MiMo（4.3.10，OpenAI 兼容 Responses 形）复用本样本 |
| `anthropic_messages_stream.sse` | Anthropic Messages 流式事件（`message_start` / `content_block_delta` / `message_delta` / `message_stop`，无 `[DONE]`，`input_tokens` 残值语义） | SDD 4.3.2（Claude / Anthropic 原生形状） | x-api-key 鉴权头注入、残值 usage 归一（A6 口径）断言 |
| `openai_responses_failed.sse` | `response.failed` 终态事件（带 `error.code` / `error.message`） | SDD 4.3.1（`response.failed` 为 Responses 三终态之一） | SC-8 上游失败收口原因（`gateway.cancel.reason=upstream_error`） |

数值口径（逐字可核）：
- `openai_responses_stream.sse` usage：`input_tokens=12`、`input_tokens_details.cached_tokens=2`、
  `output_tokens=7`、`output_tokens_details.reasoning_tokens=0`、`total_tokens=19`
  ⇒ 归一后 `InputTokens=12 / CacheReadTokens=2 / OutputTokens=7 / TotalTokens=19`（4.4.3 Responses 树）。
- `anthropic_messages_stream.sse` usage：`message_start` 侧 `input_tokens=20`（残值）、
  `cache_read_input_tokens=3`；`message_delta` 侧 `output_tokens=5`（累计口径）
  ⇒ 归一后 `InputTokens=23`（= 20 + 0 + 3，4.3.2 残值合成）/ `CacheReadTokens=3` / `OutputTokens=5`。
