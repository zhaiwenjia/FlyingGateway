# 多供应商 LLM API Schema 调研：接口形态与用量口径

> 本文档是 FlyingGateway 的**调研文档**，按 `AGENTS.md` 规则 11 构造：每条结论带**可点击 URL + 来源名 + 原话要点**；**显式区分「来自搜索」与「我的推测」**；文首维护「检索轮次」；搜不到的写进「未验证与推测清单」，不伪装成结论。

- **调研任务**：11 家 LLM 供应商的 API endpoint schema，用于设计一个 Go 语言的「多供应商统一网关」框架
- **覆盖对象**：Qwen / Kimi / 智谱 / DeepSeek / 小米 MiMo / MiniMax / OpenAI / Claude / Grok / Gemini / Ollama
- **下游文档**：[Go 实现设计](20261112-多供应商网关-Go实现设计.md)
- **本轮不做什么**：不写实现代码；不定代码落点

## 检索轮次

| 轮次 | 时间 | 因为什么现象 / 要解决的问题 | 搜了什么 | 得到什么结论 |
|---|---|---|---|---|
| R1 | 2026-11-12 20:08 | 老宅要求按 base URL + model name 自动选供应商，需先知道各家「怎么认出自己」 | 各家 endpoint 文档 | 11 家端点与路径清点 |
| R2 | 2026-11-12 20:10 | 需要**缓存命中 token**的准确字段名（这是老宅点名的三项之一） | Anthropic Messages / prompt-caching、Gemini generate-content、DeepSeek chat completion、Ollama api.md、Grok usage-pricing | 各家 usage 字段树（见第四章总表） |
| R3 | 2026-11-12 20:11 | 供应商识别需要 base URL 的**精确 host**（含地域差异） | Qwen 百炼、智谱、Kimi、MiMo、MiniMax 官方文档 | 精确 host 清单；发现 MiniMax GroupId 已在现行文档中消失 |
| R4 | 2026-11-12 20:14 | 流式响应里 usage 到底怎么带（每帧？末帧？要不要开关？） | Anthropic streaming、Kimi OpenAPI、DashScope 文本生成 | **三种 usage 携带模式**（末帧 / 每帧累计 / 原生自带） |
| R5 | 2026-11-12 20:16 | 老宅指出「claude 肯定要使用自己的格式」⇒ 需核准 Anthropic 原生形状与 OpenAI 形状的**不可逆差异** | Anthropic messages.md / streaming.md / prompt-caching.md | `thinking.signature`、`cache_control` 断点、`stop_reason` 七值等不可压平项 |

**来源分级**：

- **【一手】** —— 官方规范 / OpenAPI / 官方 README 原文（本轮 11 家中 8 家取到 `.md` 或 OpenAPI 原文）
- **【二手】** —— 第三方分析与博客（仅用于交叉印证，不单独支撑关键数字）
- **【推测】** —— 我的推断，集中在第八章

## 一、调研范围与方法

### 1.1 方法

- 官方文档优先取 **`.md` 原文 / OpenAPI 原文**（Docusaurus 与 Mintlify 站点普遍提供），不拿搜索摘要当原文。
- 字段名**逐字抄录**，⛔ 不用记忆里的字段名冒充查到的。
- 遇到**同名不同义**的字段（如 `input_tokens` 在 Anthropic 是「新增」还是「总数」），单独标注并给出来源原文。

### 1.2 三条贯穿全局的结论

**① 「OpenAI 兼容」是事实标准，但不是全部。** 11 家里 9 家提供 OpenAI 形状的 `/v1/chat/completions`；但 **Claude 是原生形状**（`/v1/messages`，content block 数组、`system` 顶层、`max_tokens` 必填），**Gemini 原生形状不同**（`contents` / `candidates` / `parts`），**Ollama 原生形状也不同**（`/api/chat`，`message.content` 而非 `choices[]`）。⇒ 网关必须是**适配器架构**，不是「一套请求体打天下」。

**② token 用量口径是各家差异最大的地方**，且差异集中在三处：缓存字段的**层级**、输入 token 的**语义**（总数 vs 新增）、流式 usage 的**携带方式**。⇒ 必须**按供应商做 usage 适配器**，⛔ 不能写一个通用 struct 硬解。

**③ 一半以上供应商额外提供 Anthropic 兼容端点**，这进一步佐证「出站用各家原生形状」是业界共识：

| 供应商 | Anthropic 兼容端点 | 出处 |
|---|---|---|
| DeepSeek | `https://api.deepseek.com/anthropic` → `/anthropic/v1/messages` | [DeepSeek Models & Pricing](https://api-docs.deepseek.com/quick_start/pricing) |
| 智谱 GLM | `https://open.bigmodel.cn/api/anthropic` → `/api/anthropic/v1/messages` | [Claude API 兼容](https://docs.bigmodel.cn/cn/guide/develop/claude/introduction) |
| Kimi | `https://api.moonshot.cn/anthropic` → `/anthropic/v1/messages` | [Kimi API 概述](https://platform.kimi.com/docs/api/overview) |
| 小米 MiMo | `https://api.xiaomimimo.com/anthropic` → `/anthropic/v1/messages` | [MiMo 首次调用](https://mimo.mi.com/static/docs/quick-start/summary/first-api-call.md) |
| MiniMax | `https://api.minimax.cn/anthropic` → `/anthropic/v1/messages` | [MiniMax 前置准备](https://platform.minimaxi.com/docs/guides/quickstart-preparation) |

## 二、总览：11 家端点矩阵

| 供应商 | 主 base URL（OpenAI 形状） | 路径 | 原生形状 | 鉴权头 |
|---|---|---|---|---|
| **Qwen / DashScope** | `https://{WorkspaceId}.{region}.maas.aliyuncs.com/compatible-mode/v1` | `/chat/completions` | `/api/v1/services/aigc/text-generation/generation`（原生） | `Authorization: Bearer` |
| **Kimi / Moonshot** | `https://api.moonshot.cn/v1` | `/chat/completions` | 有 Responses / Anthropic / 专属工具端点 | `Authorization: Bearer` |
| **智谱 GLM** | `https://open.bigmodel.cn/api/paas/v4` | `/chat/completions` | v4 本身即 OpenAI 形状 | `Authorization: Bearer` |
| **DeepSeek** | `https://api.deepseek.com` | `/chat/completions` | 有 Responses / Messages / FIM / Search&Fetch | `Authorization: Bearer` |
| **小米 MiMo** | `https://api.xiaomimimo.com/v1` | `/chat/completions` | 有 Anthropic / Responses / Batch | `api-key:` **或** `Authorization: Bearer` |
| **MiniMax** | `https://api.minimax.cn/v1` | `/chat/completions` | `/v1/text/chatcompletion_v2`（原生 V2）/ Anthropic | `Authorization: Bearer`（Anthropic 兼容另有 `x-api-key`） |
| **OpenAI** | `https://api.openai.com/v1` | `/chat/completions`、**`/responses`** | Responses API 是新基准 | `Authorization: Bearer` |
| **Claude** | `https://api.anthropic.com/v1` | **`/messages`（原生，非 OpenAI 形状）** | 原生即唯一 | `x-api-key` + `anthropic-version` |
| **Grok / xAI** | `https://api.x.ai/v1` | `/chat/completions`（**已标 legacy**）/ `/responses` | Responses 为主推 | `Authorization: Bearer` |
| **Gemini** | `https://generativelanguage.googleapis.com/v1beta` | **`/models/{model}:generateContent`**（原生） | 原生；另有 OpenAI 兼容层 | `x-goog-api-key` 或 `?key=` |
| **Ollama** | `http://localhost:11434` | **`/api/chat`（原生）** + `/v1/chat/completions` | `/api/chat` | 无（本地） |

**【一手】逐条出处**：Qwen [兼容 OpenAI](https://www.alibabacloud.com/help/zh/doc-detail/2833609.html) · [原生文本生成](https://help.aliyun.com/zh/model-studio/text-generation)｜Kimi [API 概述](https://platform.kimi.com/docs/api/overview) · [Chat Completions](https://platform.kimi.com/docs/api/chat)｜智谱 [对话补全 OpenAPI](https://docs.bigmodel.cn/api-reference/模型-api/对话补全) · [HTTP 调用](https://docs.bigmodel.cn/cn/guide/develop/http/introduction)｜DeepSeek [Chat Completions](https://api-docs.deepseek.com/api/create-chat-completion) · [Models & Pricing](https://api-docs.deepseek.com/quick_start/pricing)｜MiMo [OpenAI 兼容](https://mimo.mi.com/static/docs/api/chat/openai-api-post.md) · [首次调用](https://mimo.mi.com/static/docs/quick-start/summary/first-api-call.md)｜MiniMax [Create Chat Completion](https://platform.minimax.io/docs/api-reference/text-chat-openai) · [前置准备](https://platform.minimaxi.com/docs/guides/quickstart-preparation)｜OpenAI [openapi.yaml](https://github.com/openai/openai-openapi)｜Claude [Messages](https://platform.claude.com/docs/en/api/messages)｜Grok [Chat Completions](https://docs.x.ai/docs/guides/chat-completions)｜Gemini [Generate Content](https://ai.google.dev/api/generate-content)｜Ollama [api.md](https://github.com/ollama/ollama/blob/main/docs/api.md)

## 三、逐家 schema

### 3.1 OpenAI（统一格式的基准）

**端点**：`https://api.openai.com/v1`，`POST /chat/completions`（**【一手】** [openapi.yaml](https://github.com/openai/openai-openapi)，`servers: - url: https://api.openai.com/v1`）；另有 `POST /responses`。

**鉴权**：`Authorization: Bearer $OPENAI_API_KEY`（同 openapi.yaml `security: ApiKeyAuth`）。

**请求体**：`model` / `messages` / `stream` / `max_completion_tokens` / `temperature` / `top_p` / `tools` / `tool_choice` / `reasoning_effort` / `response_format`；流式可用 `stream_options.include_usage`。

**响应体**：`choices[].message.content` / `choices[].delta.content`；`finish_reason` 取值含 `stop` / `length` / `tool_calls` / `content_filter` / `function_call`。

**⭐ token 计量**（**【一手】** [xAI 官方 usage 文档](https://docs.x.ai/developers/advanced-api-usage/prompt-caching/usage-and-pricing) 给出的 OpenAI 形状完整 usage 树，与 openapi.yaml 一致）：

```json
{
  "usage": {
    "prompt_tokens": 125,
    "completion_tokens": 48,
    "total_tokens": 173,
    "prompt_tokens_details": {
      "text_tokens": 125, "audio_tokens": 0, "image_tokens": 0,
      "cached_tokens": 98
    },
    "completion_tokens_details": {
      "reasoning_tokens": 0, "audio_tokens": 0,
      "accepted_prediction_tokens": 0, "rejected_prediction_tokens": 0
    }
  }
}
```

⚠️ `cached_tokens` 位于 **`usage.prompt_tokens_details.cached_tokens`**（**二级**），⛔ 不在 `usage` 顶层。

**流式 usage**：需 `stream_options: {"include_usage": true}` ⇒ 在 `data: [DONE]` **之前**追加一个 chunk（`choices: []`、`usage` 在顶层）；其余 chunk 的 `usage` 为 `null`。

⚠️ 官方注释：「If the stream is interrupted or cancelled, you may not receive the final usage chunk」⇒ **流中断会丢 usage**。

#### OpenAI —— Responses API（**第二模式**）

**端点**：`POST https://api.openai.com/v1/responses`（**【一手】** [openai-python SDK](https://github.com/openai/openai-python) `types/responses/response.py`）。

**请求体**（**【一手】** 逐字）：`model` / **`input`** / **`instructions`**（系统提示，独立于 input）/ `tools` / `tool_choice` / `reasoning` / `include` / `store` / **`previous_response_id`** / **`conversation`** / `truncation` / `max_output_tokens` / `max_tool_calls` / `prompt_cache_key` / `prompt_cache_retention` / `prompt_cache_options{mode, ttl, comparison_response_id}` / `text{format}` / `background`。

**`input` 的形状**（**【一手】**）：不是 `messages[]`，而是**异构 item 数组**：

```json
"input": [
  { "type": "message", "role": "user", "content": [{ "type": "input_text", "text": "..." }] }
]
```

（也接受纯字符串。）

**`reasoning` 对象**（**【一手】**）：`context`（`auto`/`current_turn`/`all_turns`）/ `effort` / `generate_summary`（deprecated）/ `mode`（`standard`/`pro`）/ `summary`（`auto`/`concise`/`detailed`）。`effort` 取值：`none` / `minimal` / `low` / `medium` / `high` / `xhigh` / `max`。

**响应体**（**【一手】**）：`object="response"` / `id` / `created_at` / **`status`** / `model` / **`output[]`** / `usage` / `incomplete_details{reason}` / `error` / `output_text`（便捷字段）/ `parallel_tool_calls` / `metadata` / `previous_response_id` / `conversation` / `service_tier` / `completed_at` / `background` / `moderation` / `prompt_cache_diagnostics`。

**`status` 取值**（**【一手】**）：`completed` / `failed` / `in_progress` / **`cancelled`** / `queued` / **`incomplete`**。

**`incomplete_details.reason` 取值**（**【一手】**）：`max_output_tokens` / `max_messages` / `content_filter` / `steered`。

**`output[]` 是异构联合**（**【一手】** 官方联合类型逐字，29 种 item 类型）：

```
message · reasoning · function_call · function_call_output · file_search_call
computer_call · computer_tool_call_output · code_interpreter_call · local_shell_call
local_shell_call_output · custom_tool_call · custom_tool_call_output
image_generation_call · mcp_call · mcp_list_tools · mcp_approval_request
mcp_approval_response · web_search_call · tool_search_call · tool_search_output
shell_call · shell_call_output · apply_patch_call · apply_patch_call_output
compaction · program · program_output · additional_tools
```

⚠️ 官方注释明确「**不要假设 `output[0]` 是 message**」——应遍历或用 `output_text`。

**item 内部结构**（**【一手】**）：

- `message`：`{id, type:"message", role:"assistant", status, content:[{type:"output_text", text, annotations} | {type:"refusal", refusal}]}`
- `reasoning`：`{id, type:"reasoning", summary:[{type:"summary_text", text}], content:[{type:"reasoning_text", text}], encrypted_content, status}`
- `function_call`：`{id, type:"function_call", call_id, name, arguments, status}`

**⭐ Responses 的 token 计量**（**【一手】** `types/responses/response_usage.py`）：

```
usage.input_tokens
usage.output_tokens
usage.total_tokens
usage.input_tokens_details.cached_tokens
usage.input_tokens_details.cache_write_tokens
usage.input_tokens_details.cache_write_tokens_details.ttl_30m_tokens
usage.input_tokens_details.cache_write_tokens_details.ttl_12h_tokens
usage.output_tokens_details.reasoning_tokens
```

⚠️ **Chat 与 Responses 的 usage 字段名不同**（`prompt_tokens`/`completion_tokens` vs `input_tokens`/`output_tokens`），**缓存字段层级也不同**（`prompt_tokens_details` vs `input_tokens_details`）⇒ 归一化器需覆盖两套。

⚠️ **`prompt_tokens_details.cache_write_tokens` 也是 OpenAI Chat 侧的新字段** ⇒ 「缓存写入」在 OpenAI 两种模式下都有计数字段。

**⭐ Responses 流式事件体系**（**【一手】** `lib/streaming/responses/_events.py`，官方 SDK 的 `ResponseStreamEvent` 联合类型）：

- **SSE 帧同时带 `event:` 与 `data:` 两个字段**（Chat Completions **只有 `data:`**）；`data` 内的 `type` 与 `event` 名相同（冗余，可任选其一解析）。
- 每个事件带 **`sequence_number`**（单调递增，可用于检测丢事件）。
- **事件总数 59 种具名类型**（官方 SDK 联合类型的成员数）。
- 事件分类：
  - **生命周期**：`response.created` / `response.in_progress` / `response.completed` / `response.failed` / `response.incomplete` / `response.queued`
  - **输出项**：`response.output_item.added` / `.done`
  - **内容部分**：`response.content_part.added` / `.done`
  - **文本**：`response.output_text.delta` / `.done`
  - **拒绝**：`response.refusal.delta` / `.done`
  - **函数参数**：`response.function_call_arguments.delta` / `.done`
  - **推理**：`response.reasoning_summary_part.added/.done`、`response.reasoning_summary_text.delta/.done`、`response.reasoning_text.delta/.done`
  - **工具族**：`response.code_interpreter_call.*` / `response.file_search_call.*` / `response.web_search_call.*` / `response.mcp_call.*` / `response.image_gen_call.*` / `response.shell_call.*` / `response.custom_tool_call_input.delta/.done` / `response.compaction.compacting` 等
- **usage 携带**：Responses 由 **`response.completed` 事件自带完整 `response.usage`** ⇒ **不需要** `stream_options.include_usage`（与 Chat 模式相反）。
- **终止**：**恰好一个**终态事件（`completed` / `failed` / `incomplete`），随后 `data: [DONE]`。
- **顺序约束**（**【二手】**）：`response.created` MUST 最先；每个 output item 必须先 `output_item.added` 再有 delta；每个 content part 必须先 `content_part.added` 再有 text delta。

⚠️ **已知实现偏差**（**【二手】**，来源：`https://github.com/air-gapped/skills/blob/main/.claude/skills/responses-api/references/streaming-events.md`）：
- 「some backends (vLLM, llama.cpp) omit `[DONE]`; mistral.rs sends it」
- 「OpenAI's gpt-5.3-codex sometimes skips `response.output_item.added` and starts with deltas directly」
- Ollama：**不支持** `previous_response_id`（PR #15404 未合并）
- LiteLLM：Azure passthrough 会剥掉 setup events
- SGLang：自定义工具在 `/v1/responses` 上返回空 tool call；`tool_choice="required"` 返 400

⇒ 网关的 Responses 流式解析器**必须容忍缺事件、乱序、缺 `[DONE]`**。

### 3.2 Claude / Anthropic（**原生形状，不可压平**）

⚠️ **版本相关的硬约束**（**【一手】** [Streaming](https://platform.claude.com/docs/en/build-with-claude/streaming) 与 Messages 文档）：API version `2026-10-21` 起，请求带 `temperature` / `top_p` / `top_k` **一律返回 400** ⇒ 网关若用统一参数模板，**必须按供应商剔除这些字段**，不能原样透传。

**端点**：`POST https://api.anthropic.com/v1/messages`（**【一手】** [Messages](https://platform.claude.com/docs/en/api/messages)）。

**鉴权**：`x-api-key: $ANTHROPIC_API_KEY` + `anthropic-version: 2026-10-21` + `content-type: application/json`。

**请求体**：`model` / `messages` / **`max_tokens`（必填）** / `system`（**顶层参数**，非 `messages[0]`）/ `tools` / `tool_choice` / `stream` / `thinking: {type: "adaptive", display: "summarized"}` / **`cache_control: {type: "ephemeral", ttl: "5m"|"1h"}`**。

**响应体**：`content[]` 是**内容块数组**，块的 `type` 有 `text` / `thinking` / `redacted_thinking` / `tool_use` / `server_tool_use` / `web_search_tool_result` 等。`stop_reason` 取值（**【一手】逐字**）：`end_turn` / `max_tokens` / `stop_sequence` / `tool_use` / `pause_turn` / `refusal` / `model_context_window_exceeded`。

**⭐ token 计量**（**【一手】** [Messages](https://platform.claude.com/docs/en/api/messages) 的 `usage` 对象）：

```json
{
  "usage": {
    "input_tokens": 25,
    "output_tokens": 15,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0,
    "output_tokens_details": { "thinking_tokens": 0 },
    "cache_creation": {
      "ephemeral_5m_input_tokens": 0,
      "ephemeral_1h_input_tokens": 0
    },
    "server_tool_use": { "web_search_requests": 0, "web_fetch_requests": 0 },
    "service_tier": "standard",
    "speed": "standard"
  }
}
```

⚠️⚠️ **关键语义（最易算错的一处）**：Anthropic 官方明确（**【一手】** [Prompt caching FAQ](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)，原文）：

> ```
> total_input_tokens = cache_read_input_tokens + cache_creation_input_tokens + input_tokens
> ```
> "**`input_tokens` does NOT represent all input tokens, only the portion after your last cache breakpoint.**"

⇒ **`input_tokens` 是「新增部分」不是「总数」**。统一口径必须按此公式合成 `total_input`，⛔ 不能把 `input_tokens` 直接当输入总数（否则带缓存的请求会被严重低估）。

**缓存要点**：最多 **4 个** `cache_control` 断点；前缀顺序固定为 **tools → system → messages**；查找窗口 **20 个 block**；5 分钟档写入价为基础输入价 ×1.25，1 小时档 ×2，缓存命中为基础价 ×0.1（部分模型有例外倍率）。

**⭐ 流式事件序列**（**【一手】** [Streaming messages](https://platform.claude.com/docs/en/build-with-claude/streaming)，原文顺序）：

```
message_start            ← 带 usage（input_tokens / cache_* / output_tokens 初始值）
  ├─ content_block_start
  ├─ content_block_delta  ← delta.type ∈ text_delta / thinking_delta / signature_delta / input_json_delta
  ├─ content_block_stop
  └─ （可穿插 ping 事件）
message_delta            ← 带累计 usage（output_tokens 等）
message_stop
```

⚠️ 原文警告：「The token counts shown in the `usage` field of the `message_delta` event are **cumulative**.」⇒ **流式时 usage 要取 `message_delta` 的累计值，不能逐帧相加**。

**不可压平项（老宅「claude 肯定要使用自己的格式」的硬依据）**：

| Anthropic 原生项 | 压成 OpenAI 形状的后果 | 出处 |
|---|---|---|
| `thinking` block 的 `signature` | 规范原文：thinking block **必须原样回传**且保持原顺序，修改即 `400 invalid_request_error` ⇒ 多轮推理直接失效 | [Messages](https://platform.claude.com/docs/en/api/messages) |
| `cache_control` 断点（顶层 + block 级） | OpenAI 形状无对应物 ⇒ 缓存断点位置丢失，成本上升 | [Prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching) |
| `system` 顶层参数 | 位置语义不同 | 同 Messages |
| `max_tokens` 必填 | OpenAI 侧可选 | 同 Messages |
| `stop_reason` 七值 | OpenAI `finish_reason` 五值，映射有损 | 同 Messages |
| `tool_use` 是 content block | OpenAI 是独立 `tool_calls` 数组，结构不同 | 同 Messages |
| `redacted_thinking` 块 | 无对应物 | 同 Messages |

### 3.3 Gemini

**端点**：`POST https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent` 与 `:streamGenerateContent`（**【一手】** [Generate Content](https://ai.google.dev/api/generate-content)，原文 "POST /v1beta/models/{model}:generateContent"）。

**鉴权**：`x-goog-api-key` 头或 `?key=` query（**【二手】** Google GenAI 官方 Java SDK javadoc 的调用示例）。

**请求体**：`contents` / `generationConfig` / `systemInstruction` / `tools` / `toolConfig` / `safetySettings` / `cachedContent`。

**响应体**：`candidates[].content.parts[]`（`text` / `functionCall` / `inlineData` …）/ `candidates[].finishReason` / `promptFeedback` / `usageMetadata`。

**⚠️ `finishReason` 有 21 个取值**（**【一手】** Google 官方 API discovery 文档，`https://generativelanguage.googleapis.com/$discovery/rest?version=v1beta` 的 `Candidate.finishReason` 枚举，逐字）：

```
FINISH_REASON_UNSPECIFIED · STOP · MAX_TOKENS · SAFETY · RECITATION · OTHER
BLOCKLIST · PROHIBITED_CONTENT · SPII · MALFORMED_FUNCTION_CALL · MODEL_ARMOR
IMAGE_SAFETY · IMAGE_PROHIBITED_CONTENT · IMAGE_RECITATION · IMAGE_OTHER
UNEXPECTED_TOOL_CALL · NO_IMAGE · MALFORMED_RESPONSE · CONTINUATION
TOO_MANY_TOOL_CALLS
```

⇒ Gemini 的终止原因是**所有家里最宽的**（含图像类与内容策略类），归一化枚举必须容纳。

⚠️ **模型名必须带 `models/` 前缀**：路径参数正则 `^models/[^/]+$`（**【一手】** discovery 文档）⇒ 网关拼 URL 时要自己补前缀。

⚠️ **原生鉴权是 query 参数 `key`**（discovery 文档原文 "Required unless you provide an OAuth 2.0 token"），官方示例亦用头 `x-goog-api-key`；**OpenAI 兼容层**用 `Authorization: Bearer` ⇒ 三种形态并存。

**⭐ token 计量**（**【一手】** [Google GenAI Java SDK `UsageMetadata` javadoc](https://googleapis.github.io/java-genai/javadoc/com/google/genai/types/UsageMetadata.html)，字段逐字）：

```
promptTokenCount              提示中的 token 总数
candidatesTokenCount          候选回复的 token 总数
totalTokenCount               = prompt + candidates + toolUsePrompt + thoughts
thoughtsTokenCount            模型「思考」输出占用的 token
toolUsePromptTokenCount       工具执行结果回灌给模型的 token
cachedContentTokenCount       命中缓存的 token
cacheTokensDetails[]          缓存 token 按模态拆分
promptTokensDetails[] / responseTokensDetails[] / toolUsePromptTokensDetails[]
serviceTier / trafficType
```

⚠️ **命名风格完全不同**：驼峰（`promptTokenCount`），⛔ 与 OpenAI 形状的蛇形（`prompt_tokens`）不通用。

⚠️ **输出 token 不是 `candidatesTokenCount` 单独值**：`totalTokenCount` 的原文定义是「This is the sum of `prompt_token_count`, `candidates_token_count`, `tool_use_prompt_token_count`, and `thoughts_token_count`」⇒ 若要统计「用户可见的输出 token」，用 `candidatesTokenCount`；若统计「总消耗」，用 `totalTokenCount`；**思考 token 已含在 total 里，不要重复累加**。

### 3.4 DeepSeek

**端点**：`POST https://api.deepseek.com/chat/completions`（**【一手】** [Chat Completions](https://api-docs.deepseek.com/api/create-chat-completion)，原文 "POST /chat/completions"）；另有 `/responses`、`/messages`、`/completions`。

**鉴权**：`Authorization: Bearer $DEEPSEEK_API_KEY`。

**请求体**：`model`（`deepseek-chat` / `deepseek-reasoner`）/ `messages` / `stream` / `max_tokens` / `temperature` / `top_p` / `tools` / `response_format`。

**响应体**：`choices[].delta.content` / `choices[].delta.reasoning_content`（**思考模式专有字段**）/ `finish_reason` 取值：**【一手】逐字** `stop` / `length` / `tool_calls` / `content_filter` / `insufficient_system_resource` / `aborted`。

**⭐ token 计量**（**【一手】** Chat Completions 文档的流式响应示例末帧逐字）：

```json
"usage": {
  "prompt_tokens": 7,
  "completion_tokens": 11,
  "total_tokens": 18,
  "prompt_tokens_details": { "cached_tokens": 0 },
  "prompt_cache_hit_tokens": 0,
  "prompt_cache_miss_tokens": 7
}
```

⚠️ **DeepSeek 是唯一提供「命中 / 未命中」两个独立字段的**：`prompt_cache_hit_tokens` 与 `prompt_cache_miss_tokens`（顶层），同时又有标准的 `prompt_tokens_details.cached_tokens`。⇒ 解析时**取其一，⛔ 不要累加**。

### 3.5 Grok / xAI

**端点**：`POST https://api.x.ai/v1/chat/completions`（**【一手】** [Chat Completions](https://docs.x.ai/docs/guides/chat-completions)，示例 curl 逐字 `curl https://api.x.ai/v1/chat/completions`）。

**鉴权**：`Authorization: Bearer $XAI_API_KEY`。

⚠️ **状态提示**：官方文档原文「Chat Completions is offered as a **legacy** endpoint. New features will come to the Responses API first.」⇒ 若做长期方案，Grok 走 `/v1/responses` 更稳；但 OpenAI 形状的 `/chat/completions` 仍可用。

**请求体**：`model`（`grok-4.7` / `grok-4`…）/ `messages` / `stream` / `reasoning_effort`（`low`/`medium`/`high`/`xhigh`）/ `tools` / `tool_choice` / `max_output_tokens`。

**响应体**：`choices[].message.content` / `choices[].delta.content` / `finish_reason`。

**⭐ token 计量**（**【一手】** [Usage & Pricing](https://docs.x.ai/developers/advanced-api-usage/prompt-caching/usage-and-pricing)，原文 JSON）：

```json
{
  "usage": {
    "prompt_tokens": 125,
    "completion_tokens": 48,
    "total_tokens": 173,
    "prompt_tokens_details": { "text_tokens": 125, "audio_tokens": 0, "image_tokens": 0, "cached_tokens": 98 },
    "completion_tokens_details": { "reasoning_tokens": 0, "audio_tokens": 0, "accepted_prediction_tokens": 0, "rejected_prediction_tokens": 0 }
  }
}
```

与 OpenAI 形状**完全一致** ⇒ Grok 适配器可复用 OpenAI 兼容解析器。缓存命中语义原文：`cached_tokens` 为 0 = 未命中；等于 `prompt_tokens` = 全命中。

⚠️ Grok 有会话级缓存头 `x-grok-conv-id` 与 `prompt_cache_key`（**【一手】** 同页：「verify that you're setting `x-grok-conv-id` (or `prompt_cache_key`)」）。

### 3.6 Ollama

**端点**：`POST http://localhost:11434/api/chat`（原生）与 `POST http://localhost:11434/api/generate`（**【一手】** [api.md](https://github.com/ollama/ollama/blob/main/docs/api.md)，原文 "POST /api/chat"）；另有 OpenAI 兼容层 `/v1/chat/completions`。

**鉴权**：无（本地进程）。

**请求体**（原生）：`model`（`model:tag` 格式，tag 缺省 `latest`）/ `messages` / `stream` / `tools` / `think`（bool 或 `"low"`/`"medium"`/`"high"`/`"max"`）/ `format` / `options`（temperature 等）/ `keep_alive`。

**响应体**（原生，**不是** `choices[]`）：`message.role` / `message.content` / `message.thinking` / `message.tool_calls[]` / `done` / `done_reason`（`stop` / `load` / `unload`）/ `created_at` / `context[]`。

**⭐ token 计量**（**【一手】** api.md 原文，字段名与 OpenAI 形状**完全不同**）：

```
prompt_eval_count          提示中的 token 数
prompt_eval_cached_count   从缓存读取的提示 token 数   ← Ollama 独有
prompt_eval_duration       评估未缓存提示 token 的耗时（纳秒）
eval_count                 回复中的 token 数
eval_duration              生成耗时（纳秒）
total_duration / load_duration
```

⚠️ **Ollama 是唯一把「缓存命中」放在与输入 token 平级、且不叫 `cached_tokens` 的** ⇒ 适配器必须单独映射。

⚠️ Ollama 的 usage **只在最后一帧**出现（`done: true` 那帧），且 `response` 字段在流式时被清空。

⚠️ **Ollama 原生流式不是 SSE，是 NDJSON**（**【一手】** `https://docs.ollama.com/api/chat.md` 的 OpenAPI：响应 media type 为 `application/x-ndjson`，一行一个 JSON 对象），**无 `[DONE]` 哨兵**；且 `/api/chat` **默认就是流式**（`stream` 默认 `true`）。

⚠️ **Ollama 的 `/v1` OpenAI 兼容层**（**【一手】** `https://docs.ollama.com/api/openai-compatibility.md`）自 v0.13.3 起也提供 `/v1/responses`，但**仅无状态 flavor**（不支持 `previous_response_id`）；兼容层**不支持** `tool_choice` / `n` / `user` / `logit_bias`。

### 3.7 Qwen / DashScope

**端点**：兼容模式 `POST {base_url}/chat/completions`；原生 `POST https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api/v1/services/aigc/text-generation/generation`（**【一手】** [文本生成](https://help.aliyun.com/zh/model-studio/text-generation)）。

**base URL 地域化**（**【一手】** [兼容 OpenAI](https://www.alibabacloud.com/help/zh/doc-detail/2833609.html)）：`https://{WorkspaceId}.{cn-beijing|cn-hongkong|ap-southeast-1|ap-northeast-1|us-east-1}.maas.aliyuncs.com/compatible-mode/v1`。

⚠️ **API Key 与地域强绑定**：用北京 Key 调弗吉尼亚 endpoint 返回 401 / `invalid_api_key`（**【一手】** [模型服务概览](https://help.aliyun.com/zh/document_detail/2586399.html)）⇒ 网关识别供应商时**不能只看 host 后缀，还要看 WorkspaceId 与地域的组合**。

**请求体**：OpenAI 形状 + 两个**非标准**参数：`enable_thinking`（bool，**【一手】** [深度思考](https://www.alibabacloud.com/help/zh/model-studio/deep-thinking)）、`enable_search`。

**响应体**：`choices[].delta.reasoning_content`；`finish_reason` 兼容页原文只列 `null` / `stop` / `length` 三种。

**⭐ token 计量**：
- 兼容模式：`usage.prompt_tokens` / `usage.completion_tokens` / `usage.total_tokens` / **`usage.prompt_tokens_details.cached_tokens`** / `usage.prompt_tokens_details.cache_creation_input_tokens` / `usage.completion_tokens_details.reasoning_tokens`
- 原生 text-generation：`usage.total_tokens` / **`usage.input_tokens`** / `usage.output_tokens` / `usage.output_tokens_details.reasoning_tokens`

⚠️ **原生模式的字段名与兼容模式完全不同**（`input_tokens` vs `prompt_tokens`）⇒ 同一个供应商的两种模式也需要两套解析。

**流式**：兼容模式末帧 `data: [DONE]`；**原生模式须加请求头 `X-DashScope-SSE: enable`**，且帧格式为 `data:{json}`（**冒号后无空格**）、**未见 `[DONE]` 哨兵**、**每帧自带 usage**。

### 3.8 智谱 GLM / BigModel

**端点**：`POST https://open.bigmodel.cn/api/paas/v4/chat/completions`（**【一手】** [HTTP API 调用](https://docs.bigmodel.cn/cn/guide/develop/http/introduction)，原文 base URL `https://open.bigmodel.cn/api/paas/v4/`）。

**鉴权**：`Authorization: Bearer YOUR_API_KEY`（也支持 JWT 方式）。

**请求体**：`thinking.type`（`enabled`/`disabled`）/ **`thinking.clear_thinking`**（bool，默认 `true`；Preserved Thinking 须置 `false`）/ `reasoning_effort`（`max`/`xhigh`/`high`/`medium`/`low`/`minimal`/`none`）/ **`stream_options.chunk_include_usage`**（**不是** `include_usage`！）/ `tools` / `tool_choice`（**仅 `auto`**）/ `response_format`（仅 `text`/`json_object`，**无 `json_schema`**）。

**响应体**：`choices[].message.reasoning_content` / `choices[].delta.reasoning_content`；`finish_reason` 取值（**【一手】逐字**）：`stop` / `length` / `tool_calls` / **`sensitive`** / **`network_error`**。

**⭐ token 计量**：`usage.prompt_tokens` / `usage.completion_tokens` / `usage.total_tokens` / `usage.prompt_tokens_details.cached_tokens` / `usage.completion_tokens_details.reasoning_tokens`。

⚠️ **流式 usage 有两种模式**（**【一手】** [流式消息](https://docs.bigmodel.cn/cn/guide/capabilities/streaming)）：默认**仅最后一个数据块**返回；开 `stream_options.chunk_include_usage=true` 后**逐块返回累计 usage**，官方明确「**累计值不能相加**」。

⚠️ **参数名 `chunk_include_usage` 与 OpenAI 的 `include_usage` 不同** ⇒ 照搬会**静默拿不到 usage**。

### 3.9 Kimi / Moonshot

**端点**：`POST https://api.moonshot.cn/v1/chat/completions`（**【一手】** [Chat Completions API](https://platform.kimi.com/docs/api/chat)，OpenAPI `servers: https://api.moonshot.cn`）。

**鉴权**：`Authorization: Bearer $MOONSHOT_API_KEY`。额外可选 `X-Msh-Request-Nonce` 头 —— 携带后服务端回 `Msh-Request-Timestamp` / `Msh-Request-Signature`（前缀 `reqsigv1_`）用于溯源验签（**【一手】** 同页 OpenAPI）。

**请求体**：`model` / `messages` / `stream` / `stream_options.include_usage` / **`max_completion_tokens`**（`max_tokens` 已 deprecated）/ `response_format`（支持 `json_schema`）/ `tools` / `tool_choice` / **`prompt_cache_options: {mode:"implicit", ttl:"5m"|"1h"}`** / `prompt_cache_key` / `partial`（**写在 message 上**，非顶层）/ `safety_identifier`。

**思考控制跨代不同**（**【一手】**）：K3 系列用**顶层 `reasoning_effort`**（`low`/`high`/`max`，默认 `max`），**不用** `thinking`；K2.6/K2.7 用 `thinking.type` + `thinking.keep`。

**响应体**：`choices[].message.reasoning_content` / `choices[].delta.reasoning_content`；`finish_reason` 取值（**【一手】逐字**）：`stop` / `length` / `tool_calls`。

**⭐ token 计量**（**【一手】** OpenAPI 原文）：

```
usage.prompt_tokens                              总输入 token（始终为总数）
usage.completion_tokens
usage.total_tokens
usage.cached_tokens                              ⚠️ 顶层另有一份命中量
usage.prompt_tokens_details.cached_tokens        命中缓存（读取）
usage.prompt_tokens_details.cache_write_tokens   本次写入缓存
usage.completion_tokens_details.reasoning_tokens
```

⚠️ **Kimi 是唯一在 `prompt_tokens_details` 里同时给「读取」与「写入」两个字段的**。文档原文关系：**`cached_tokens` + `cache_write_tokens` + 未缓存部分 = `prompt_tokens`**（三者互斥，缓存读写**已包含在** `prompt_tokens` 中，⛔ 不要重复累加）。

⚠️ Kimi 的 `usage.cached_tokens`（顶层）与 `usage.prompt_tokens_details.cached_tokens` 语义相同、**重复存在**，解析时任选其一。

**流式**：`stream_options: {"include_usage": true}` ⇒ `[DONE]` 前一帧携带（`choices: []`）；其余帧 `usage: null`。文档原文提示「**如果流中断，可能无法收到包含总 Token 用量的最终 chunk**」。

### 3.10 小米 MiMo

**端点**：`POST https://api.xiaomimimo.com/v1/chat/completions`（**【一手】** [OpenAI Chat Completions API 兼容](https://mimo.mi.com/static/docs/api/chat/openai-api-post.md)）；另有 `/anthropic/v1/messages`、`/v1/responses`、Batch 端点 `https://batch-api-cn.xiaomimimo.com/v1`。

**鉴权**：**两种都支持** —— `api-key: $MIMO_API_KEY` **或** `Authorization: Bearer $MIMO_API_KEY`（**【一手】** 同页原文）⇒ ⚠️ MiMo 是唯一**非标准头名**的（`api-key:`）。

**请求体**：`model`（`mimo-v2.6-flash` / `mimo-v2.6-pro` / `mimo-v2.6-pro-ultraspeed`）/ `messages` / `stream` / `max_completion_tokens` / `temperature` / `top_p` / `tools` / `tool_choice`（**仅 `auto`**）/ `response_format`（**无 `json_schema`**）/ `thinking.type`。

⚠️ **MiMo 的 `messages[].role` 比 OpenAI 多一个 `developer`**（**【一手】** 同页）。

**响应体**：`choices[].message.reasoning_content` / `choices[].delta.reasoning_content` / `choices[].message.annotations[]`；`finish_reason` 取值（**【一手】逐字**）：`stop` / `length` / `tool_calls` / `content_filter` / **`repetition_truncation`**。

**⭐ token 计量**：

```
usage.prompt_tokens
usage.completion_tokens
usage.total_tokens
usage.prompt_tokens_details.cached_tokens
usage.prompt_tokens_details.audio_tokens
usage.prompt_tokens_details.image_tokens
usage.prompt_tokens_details.video_tokens
usage.completion_tokens_details.reasoning_tokens
usage.web_search_usage.tool_usage      联网搜索调用次数（非 token）
usage.web_search_usage.page_usage      联网搜索返回网页数（非 token）
```

⚠️ MiMo **无需** `stream_options` ⇒ 末帧 `choices: []` 自动携带完整 usage。

### 3.11 MiniMax

**端点**：OpenAI 形状 `POST https://api.minimax.cn/v1/chat/completions`；Anthropic 形状 `POST https://api.minimax.cn/anthropic/v1/messages`；原生 V2 `POST https://api.minimax.cn/v1/text/chatcompletion_v2`（**【一手】** [Create Chat Completion](https://platform.minimax.io/docs/api-reference/text-chat-openai) · [Create Message](https://platform.minimax.io/docs/api-reference/text-chat-anthropic) · [Text Generation](https://platform.minimax.io/docs/api-reference/text-post)）。国际站 host 为 `api.minimax.io`。

**鉴权**：OpenAI 形状 `Authorization: Bearer`；Anthropic 形状**同时接受** `Authorization` 与 `x-api-key`，且 **`Authorization` 优先**。

⚠️ **GroupId 已在现行文档中消失**（**【一手】**）：在中国站 5 个相关文档页与国际站 OpenAPI 中检索 `groupid` / `group_id`，**命中数为 0**；旧域名 `api.minimax.chat` / `api.minimaxi.com` 亦未出现在现行文档（现行中国站为 `api.minimax.cn`）⇒ **GroupId 属旧版遗留口径**，沿用会引入无效参数。

**请求体**：`thinking.type`（`disabled`/`adaptive`）/ `reasoning_effort` / **`reasoning_split`**（bool）/ `service_tier`（`standard`/`priority`）/ `stream_options.include_usage` / `max_completion_tokens` / `tools` / `tool_choice`（**仅 `auto`**）。

⚠️ **`reasoning_split` 是「输出格式开关」不是「思考开关」**（**【一手】**）：`true` 时思考单独放 `reasoning_content`；`false` 时**并入 `content` 并用 `<think>` 包裹** ⇒ 网关必须先拆包再取 content。

⚠️ **`temperature` / `top_p` 传了静默无效**（**【一手】** 文档原文 "Not supported yet; any value passed has no effect"，恒为 1 / 0.95）。

**响应体**：`choices[].message.reasoning_content` / `choices[].delta.reasoning_content`；`finish_reason`（**【一手】逐字**）`stop` / `length` / `content_filter` / `tool_calls`（⚠️ **流式 chunk 的枚举里没有 `content_filter`**，两处不一致）。另有顶层 `input_sensitive` / `output_sensitive` / `base_resp{status_code, status_msg}`（错误码 `1000` 未知 / `1001` 超时 / `1002` 限流 / `1004` 鉴权失败 / `1008` 余额不足 / `1013` 内部错误 / `1027` 输出内容错误 / `1039` Token 超限 / `2013` 参数错误）。

**⭐ token 计量**：

*OpenAI 形状*：`usage.prompt_tokens`（总输入）/ `usage.completion_tokens` / `usage.total_tokens` / `usage.total_characters` / **`usage.prompt_tokens_details.cached_tokens`** / `usage.completion_tokens_details.reasoning_tokens`

*Anthropic 形状*：`usage.input_tokens`（⚠️ **新增部分**，与 Claude 同语义）/ `usage.output_tokens` / **`usage.cache_creation_input_tokens`** / **`usage.cache_read_input_tokens`**

⚠️ **MiniMax 的 Anthropic 端点与 Claude 的 `input_tokens` 语义一致**（都是「新增部分」）⇒ 这条口径可跨供应商复用，值得在设计里明确标注。

**缓存门槛**：输入 token 数 **≥ 512** 才触发；前缀匹配顺序为 system prompt → tool 列表 → user 消息（**【一手】** [Prompt 缓存](https://platform.minimax.io/docs/api-reference/text-prompt-caching)）。

### 3.12 ⭐ 双模式对照：Chat Completions vs Responses

> 老宅 2026-11-12 补充要求：「需要同时支持 openai chat completion 和 openai response 两种模式」。
> ⇒ 网关的**对外（入站）**必须同时提供这两套 API 形状；**对内**需要一套能承载两者的规范模型；
> **对上游（出站）**按供应商能力选择形状（多数供应商原生只有 Chat 形状 ⇒ 需 Chat ↔ Responses 双向转换）。

**【一手】两模式的形状差异**（来源：OpenAI 官方 SDK `types/responses/response.py` 与 `types/chat/chat_completion.py`，逐字对照）：

| 维度 | Chat Completions | Responses |
|---|---|---|
| 端点 | `POST /v1/chat/completions` | `POST /v1/responses` |
| 输入载体 | `messages[]`（role + content） | **`input[]` 异构 item 数组**（或纯字符串） |
| 输入 content 元素类型 | `text` / `image_url` / `file` / `input_audio` | `input_text` / `input_image` / `input_file` / `input_audio` |
| 系统提示位置 | `messages[0].role="system"` | **顶层 `instructions`** |
| 输出载体 | `choices[0].message`（**单条**） | **`output[]` 异构联合**（29 种 item 类型） |
| 文本便捷字段 | `choices[0].message.content` | **`output_text`** |
| 推理内容位置 | `message.reasoning_content`（各家扩展） | `output[]` 里的 **`reasoning` item**（含 `encrypted_content`） |
| 工具调用 | `message.tool_calls[]`（`{id,type,function{name,arguments}}`） | `output[]` 里的 **`function_call` item**（`{id,call_id,name,arguments}`，**多 `call_id` 字段**） |
| 终止状态 | `choices[0].finish_reason` | **`status`**（六值）+ `incomplete_details.reason` |
| 终止原因取值 | `stop`/`length`/`tool_calls`/`content_filter`/`function_call` | `status`：`completed`/`failed`/`in_progress`/`cancelled`/`queued`/`incomplete` |
| 输入 token 字段 | `usage.prompt_tokens` | **`usage.input_tokens`** |
| 输出 token 字段 | `usage.completion_tokens` | **`usage.output_tokens`** |
| 缓存命中字段 | `usage.prompt_tokens_details.cached_tokens` | **`usage.input_tokens_details.cached_tokens`** |
| 缓存写入字段 | `usage.prompt_tokens_details.cache_write_tokens` | **`usage.input_tokens_details.cache_write_tokens`**（另有 TTL 细分） |
| 推理 token 字段 | `usage.completion_tokens_details.reasoning_tokens` | **`usage.output_tokens_details.reasoning_tokens`** |
| 会话状态 | ❌ 无（客户端带全部历史） | ✅ **`previous_response_id`** / **`conversation`**（服务端态） |
| 流式帧格式 | **只有 `data:`**（`chat.completion.chunk`） | **`event:` + `data:` 双字段**，带 `sequence_number` |
| 流式事件数 | 1 种有效事件类型 | **59 种具名事件**（官方 SDK `ResponseStreamEvent` 联合类型） |
| 流式 usage | 需 `stream_options.include_usage`，仅末帧非 null | **`response.completed` 事件自带完整 usage**，无需开关 |
| 终止哨兵 | `data: [DONE]` | `data: [DONE]`（但部分后端省略） |
| 中断丢 usage | ⚠️ 会（末帧可能不到） | ⚠️ 会（`response.completed` 可能不到） |

**两条关键差异（决定转换器的工作量）**：

1. **Responses 的 `output[]` 是异构联合，不是单一 message。** Chat 的 `choices[0].message` 是「一个 assistant 消息」，Responses 的 `output[]` 是「一串 item」（可能是 reasoning + message + function_call + …）。⇒ **Chat → Responses 转换 = 拆一条 message 成多个 item；Responses → Chat 转换 = 合并多个 item 取第一个 message（其余丢弃或降级）**。这个方向不对称，**有损的一侧是 Responses → Chat**（`function_call` item 要降级成 `tool_calls[]`；`reasoning` item 要降级成 `reasoning_content` 字符串，且 **`encrypted_content` 无处安放** ⇒ Anthropic 的 `signature` 与 OpenAI 的 `encrypted_content` 都只能丢）。
2. **两者都有「服务端会话态」与「客户端会话态」的混用风险。** Responses 支持 `previous_response_id`（服务端存历史），但**只有 OpenAI 真正实现了**；Ollama 明确不支持（PR 未合并）。⇒ 网关**不能**把 `previous_response_id` 直接透传给非 OpenAI 供应商 ⇒ 必须要么在网关侧维护会话（自建 `conversation`），要么降级为「客户端全量历史」模式。

### 3.13 各家对 Responses API 的支持度

| 供应商 | `/v1/responses` | 出处 |
|---|---|---|
| **OpenAI** | ✅ 原生 | [openai-python SDK](https://github.com/openai/openai-python) |
| **DeepSeek** | ✅ 原生（官方标注 ✓） | [Models & Pricing](https://api-docs.deepseek.com/quick_start/pricing) 特性表 |
| **Grok / xAI** | ✅ 原生且**官方推荐**（Chat 已标 legacy） | [Best Practices Guide](https://docs.x.ai/developers/model-capabilities/text/best-practices) |
| **Kimi** | ✅ 原生（base_url 同为 `https://api.moonshot.cn/v1`） | [API 概述](https://platform.kimi.com/docs/api/overview) |
| **Qwen / DashScope** | ✅ 原生（`usage.input_tokens` / `input_tokens_details.cached_tokens`） | [深度思考](https://www.alibabacloud.com/help/zh/model-studio/deep-thinking) |
| **智谱 GLM** | ✅ 原生 | [文档索引](https://docs.bigmodel.cn/llms.txt) |
| **MiniMax** | ✅ 兼容端点 | [API 概述](https://platform.minimax.io/docs/api-reference/api-overview) |
| **Ollama** | ✅ `/v1/responses`（**v0.13.3+，仅无状态 flavor**；不支持 `previous_response_id`） | [OpenAI Compatibility](https://docs.ollama.com/api/openai-compatibility) |
| **Gemini** | ✅ OpenAI 兼容层 `https://generativelanguage.googleapis.com/v1beta/openai/` | [OpenAI Compatibility](https://ai.google.dev/gemini-api/docs/openai) |
| **Claude / Anthropic** | ❌ **无** `/v1/responses`（只有 `/v1/messages`） | [Messages](https://platform.claude.com/docs/en/api/messages) |

⇒ **10/11 家有 Responses 形状，只有 Anthropic 没有** ⇒ 「出站统一走 Responses」不可行，但「**入站支持双模式** + **出站按能力降级**」完全可行。

### 3.14 双模式的转换代价（设计要点）

| 转换方向 | 代价 | 丢失项 |
|---|---|---|
| Chat → Responses | **低**：一条 message 拆成 `[{type:"message",...}]`；`tool_calls[]` → `function_call` item（补 `call_id`） | 无 |
| Responses → Chat | **高**：多个 item 只能取第一个 message；`function_call` item → `tool_calls[]`；`reasoning` item → `reasoning_content` 字符串 | **`encrypted_content`**、`reasoning.summary[]`、多 item 顺序、`sequence_number`、服务端会话引用 |
| Chat → Anthropic | **中**：`messages` → `messages`（role/content 兼容），`system` 消息提到顶层，`max_tokens` **必填**（OpenAI 侧可选 ⇒ 需给默认值） | `tool_calls[]` → `tool_use` content block（结构转换）；`reasoning_content` 无原生位（Anthropic 用 `thinking` 参数 + 响应侧 thinking block） |
| Anthropic → Chat | **中**：`content[]` 块 → `message.content` / `tool_calls[]` / `reasoning_content` | **`thinking.signature`**（⇒ 多轮推理失效，规范要求原样回传） |

⚠️ **两处不可逆**（必须显式告警，不能静默丢）：
1. **Anthropic → Chat 丢 `thinking.signature`** ⇒ 多轮带思考的对话在 Chat 模式下**功能降级**（模型拿不到签名，回传的 thinking block 会被拒）。
2. **Responses → Chat 丢 `encrypted_content`** ⇒ 同理，OpenAI 的加密推理链在 Chat 模式下不可续。

## 四、⭐ token 用量字段总对照表（本轮核心产出）
| 供应商 / 接口 | 输入 token | 缓存**读取**（命中） | 缓存**写入** | 输出 token | 缓存字段层级 | 流式 usage 携带方式 |
|---|---|---|---|---|---|---|
| **OpenAI** Chat | `prompt_tokens` | `prompt_tokens_details.cached_tokens` | — | `completion_tokens` | `usage` 下二级 | `include_usage=true` → `[DONE]` 前一帧 |
| **OpenAI** Responses | `input_tokens` | `input_tokens_details.cached_tokens` | `input_tokens_details.cache_write_tokens`（TTL 细分 `ttl_30m_tokens`/`ttl_12h_tokens`） | `output_tokens` | `usage` 下二级 | **`response.completed` 事件自带**完整 usage（无需开关） |
| **Claude** Messages | `input_tokens`（⚠️**仅新增部分**） | `cache_read_input_tokens` | `cache_creation_input_tokens` | `output_tokens` | **`usage` 顶层** | `message_start` 初始 + `message_delta` **累计** |
| **Gemini** | `promptTokenCount` | `cachedContentTokenCount` | — | `candidatesTokenCount`（含 thoughts） | `usageMetadata` 下 | chunk 型 |
| **DeepSeek** | `prompt_tokens` | `prompt_cache_hit_tokens`（顶层）/ `prompt_tokens_details.cached_tokens` | — | `completion_tokens` | 顶层 + 二级（重复） | 末帧携带 |
| **Grok** | `prompt_tokens` | `prompt_tokens_details.cached_tokens` | — | `completion_tokens` | `usage` 下二级 | 末帧携带 |
| **Ollama** `/api/chat` | `prompt_eval_count` | **`prompt_eval_cached_count`**（平级） | — | `eval_count` | **平级，非 details** | 末帧（`done:true`） |
| **Qwen** 兼容 | `prompt_tokens` | `prompt_tokens_details.cached_tokens` | `prompt_tokens_details.cache_creation_input_tokens` | `completion_tokens` | `usage` 下二级 | `include_usage=true` → 末帧 |
| **Qwen** 原生 | `input_tokens` | ⚠️ 未在 text-generation 示例中验证 | — | `output_tokens` | `usage` 下二级 | **每帧自带**（须加 `X-DashScope-SSE: enable`） |
| **智谱 GLM** | `prompt_tokens` | `prompt_tokens_details.cached_tokens` | — | `completion_tokens` | `usage` 下二级 | 默认末帧；`chunk_include_usage=true` → 逐块**累计** |
| **Kimi** | `prompt_tokens`（总） | `prompt_tokens_details.cached_tokens`（顶层另有 `cached_tokens`） | **`prompt_tokens_details.cache_write_tokens`** | `completion_tokens` | `usage` 下二级 | `include_usage=true` → 末帧 |
| **小米 MiMo** | `prompt_tokens` | `prompt_tokens_details.cached_tokens` | — | `completion_tokens` | `usage` 下二级 | **无需开关**，末帧自动带 |
| **MiniMax** OpenAI 形状 | `prompt_tokens`（总） | `prompt_tokens_details.cached_tokens` | — | `completion_tokens` | `usage` 下二级 | `include_usage=true` → 末帧 |
| **MiniMax** Anthropic 形状 | `input_tokens`（⚠️**新增部分**） | `cache_read_input_tokens` | `cache_creation_input_tokens` | `output_tokens` | **`usage` 顶层** | `message_delta` 携带 |
| **MiniMax** 原生 V2 | `prompt_tokens` | ⚠️ 未验证 | — | `completion_tokens` | `usage` 下二级 | `include_usage` → 末帧 |

### 4.1 三条必须固化的归一化规则

**R1 —— 输入 token 分两类，字段名必须区分。**
一类是「**总输入**」（OpenAI 形状全部、Qwen 兼容、智谱、Kimi、MiMo、MiniMax OpenAI、Ollama、Gemini 的 `promptTokenCount`）；另一类是「**新增部分**」（Claude / MiniMax-Anthropic 的 `input_tokens`）。
⇒ 归一化必须产出**两个字段**而不是一个：`input_tokens`（总输入）与 `input_tokens_uncached`（未缓存新增）。后者对 Anthropic 系 = `input_tokens`，对 OpenAI 系 = `prompt_tokens − cached_tokens − cache_write_tokens`。

**R2 —— 缓存读与缓存写是两个字段，不是同一个。**
Qwen（`cache_creation_input_tokens`）、Kimi（`cache_write_tokens`）、Claude / MiniMax-Anthropic（`cache_creation_input_tokens`）都把「写入」单列。⇒ 只统计命中量会**低估写入成本**（写入通常是溢价计费：Claude 5m 档 ×1.25、1h 档 ×2）。

**R3 —— 流式 usage 有三种模式，必须枚举化。**

| 模式 | 触发方式 | 语义 |
|---|---|---|
| `IncludeUsage` | 请求带 `stream_options.include_usage=true` | 末帧（`choices: []`）带完整 usage；其余帧 `usage: null`（OpenAI / Qwen / Kimi / MiniMax / Grok） |
| `ChunkIncludeUsage` | 请求带 `stream_options.chunk_include_usage=true` | **逐帧**带**累计** usage（⛔ 不可相加）（智谱） |
| `Implicit` | 无需开关 | 末帧自动带 usage（MiMo / Ollama）；或**每帧自带**（Qwen 原生） |
| `EventCumulative` | 协议事件自带 | `message_start` 初始 + `message_delta` **累计**（Claude / MiniMax-Anthropic） |

### 4.2 `finish_reason` / `stop_reason` 超集

| 值 | 出现于 |
|---|---|
| `stop` | 全部 |
| `length` | 全部 |
| `tool_calls` | OpenAI / DeepSeek / 智谱 / Kimi / MiMo / MiniMax |
| `content_filter` | OpenAI / DeepSeek / MiMo / MiniMax（⚠️ MiniMax 流式枚举里**没有**） |
| `aborted` | DeepSeek（**独有**） |
| `insufficient_system_resource` | DeepSeek（**独有**） |
| `sensitive` | 智谱（**独有**，内容安全拦截） |
| `network_error` | 智谱（**独有**） |
| `repetition_truncation` | MiMo（**独有**，模型自检到重复） |
| `end_turn` / `max_tokens` / `stop_sequence` / `tool_use` / `pause_turn` / `refusal` / `model_context_window_exceeded` | Claude（**七值**，与 OpenAI 的 `finish_reason` 不是同一套） |

⚠️ 归一化枚举**必须用超集**，⛔ 不能只认 OpenAI 的三到五个。

### 4.3 输入 token 语义对照（归一化时最容易错的一处）

| 供应商 | 字段 | 语义 | 总输入怎么算 |
|---|---|---|---|
| OpenAI Chat | `prompt_tokens` | **总输入** | 直接用 |
| OpenAI Responses | `input_tokens` | **总输入** | 直接用 |
| Qwen 兼容 / 智谱 / Kimi / MiMo / MiniMax(OpenAI) / Grok | `prompt_tokens` | **总输入** | 直接用 |
| Gemini | `promptTokenCount` | **总输入**（设了 `cachedContent` 时**含**缓存 token） | 直接用 |
| Ollama | `prompt_eval_count` | **总输入**（含命中缓存部分） | 直接用 |
| **Claude / MiniMax(Anthropic)** | `input_tokens` | ⚠️ **仅「最后一个缓存断点之后的新增部分」** | **`input_tokens` + `cache_creation_input_tokens` + `cache_read_input_tokens`** |

⇒ 归一化模型必须输出**两个**输入字段：
- `input_tokens` —— **总输入**（对 Anthropic 系需合成）
- `input_tokens_uncached` —— **未缓存的新增部分**（对 OpenAI 系 = `prompt_tokens − cached_tokens − cache_write_tokens`；对 Anthropic 系 = `input_tokens`）

⚠️ **Ollama 的坑**：缓存命中**不减少** `prompt_eval_count`，只减少 `prompt_eval_duration` 的评估量 ⇒ Ollama 的「未缓存新增」需自己算 `prompt_eval_count − prompt_eval_cached_count`。

## 五、供应商识别信号（base URL + model name）

### 5.1 base URL host → 供应商（**【一手】**逐家 host 见第二章）

| host 片段 | 供应商 | 依据 |
|---|---|---|
| `*.maas.aliyuncs.com` / `dashscope.aliyuncs.com` / `dashscope-intl.aliyuncs.com` / `cn-hongkong.dashscope.aliyuncs.com` | Qwen | [兼容 OpenAI](https://www.alibabacloud.com/help/zh/doc-detail/2833609.html) |
| `api.moonshot.cn` / `api.moonshot.ai` | Kimi | [API 概述](https://platform.kimi.com/docs/api/overview) |
| `open.bigmodel.cn` | 智谱 | [HTTP 调用](https://docs.bigmodel.cn/cn/guide/develop/http/introduction) |
| `api.deepseek.com` | DeepSeek | [Models & Pricing](https://api-docs.deepseek.com/quick_start/pricing) |
| `api.xiaomimimo.com` / `batch-api-cn.xiaomimimo.com` / `token-plan-cn.xiaomimimo.com` | 小米 MiMo | [首次调用](https://mimo.mi.com/static/docs/quick-start/summary/first-api-call.md) |
| `api.minimax.cn` / `api.minimax.io` | MiniMax | [前置准备](https://platform.minimaxi.com/docs/guides/quickstart-preparation) |
| `api.openai.com` | OpenAI | [openapi.yaml](https://github.com/openai/openai-openapi) |
| `api.anthropic.com` | Claude | [Messages](https://platform.claude.com/docs/en/api/messages) |
| `api.x.ai` | Grok | [Chat Completions](https://docs.x.ai/docs/guides/chat-completions) |
| `generativelanguage.googleapis.com` / `aiplatform.googleapis.com` | Gemini | [Generate Content](https://ai.google.dev/api/generate-content) |
| `localhost:11434` / `127.0.0.1:11434` | Ollama | [api.md](https://github.com/ollama/ollama/blob/main/docs/api.md) |

### 5.2 model name → 供应商（前缀信号）

| 前缀 | 供应商 | 出处 |
|---|---|---|
| `qwen` / `qwen3` / `qwq` / `deepseek-r1`（通义） | Qwen | [深度思考](https://www.alibabacloud.com/help/zh/model-studio/deep-thinking) |
| `kimi` / `moonshot` | Kimi | [Chat Completions](https://platform.kimi.com/docs/api/chat) |
| `glm` | 智谱 | [对话补全](https://docs.bigmodel.cn/api-reference/模型-api/对话补全) |
| `deepseek` / `deepseek-chat` / `deepseek-reasoner` | DeepSeek | [Models & Pricing](https://api-docs.deepseek.com/quick_start/pricing) |
| `mimo` | 小米 MiMo | [OpenAI 兼容](https://mimo.mi.com/static/docs/api/chat/openai-api-post.md) |
| `MiniMax` / `minimax` / `abab` | MiniMax | [Create Chat Completion](https://platform.minimax.io/docs/api-reference/text-chat-openai) |
| `gpt-` / `o1` / `o3` / `o4` / `chatgpt-` | OpenAI | [openapi.yaml](https://github.com/openai/openai-openapi) |
| `claude-` | Claude | [Messages](https://platform.claude.com/docs/en/api/messages) |
| `grok-` | Grok | [Chat Completions](https://docs.x.ai/docs/guides/chat-completions) |
| `gemini-` | Gemini | [Generate Content](https://ai.google.dev/api/generate-content) |
| `llama` / `qwen2` / `mistral` / `deepseek-r1` / `gemma` / `phi` 等（Ollama 库名） | Ollama | [api.md](https://github.com/ollama/ollama/blob/main/docs/api.md) |

⚠️ **前缀信号有歧义**：`deepseek-r1` 既是 DeepSeek 官方模型，也常作为 Ollama 本地模型名；`qwen2` 既是 Qwen 模型也是 Ollama 模型家族。⇒ **base URL 优先级高于 model name**。

### 5.3 识别歧义与消解

| 冲突场景 | 消解规则 |
|---|---|
| 同一 host 提供多个 API 形状（如 DeepSeek 的 `/chat/completions` 与 `/anthropic/v1/messages`；Qwen 的兼容与原生） | **由路径决定形状**，host 只定供应商 |
| 模型名像别家（Ollama 本地跑 `deepseek-r1`） | **host 优先**；host 识别失败才回退 model 前缀 |
| 用户只给 base URL 不给模型 | 按 host 定向，请求体里模型名由用户给 |
| base URL 是**代理/网关**（如自建 LiteLLM） | host 不可识别 ⇒ 走 **OpenAI 兼容兜底**，并允许显式覆盖 provider |
| 自建 vLLM / SGLang（OpenAI 兼容但非官方 host） | 同上 ⇒ OpenAI 兼容兜底 |

## 六、归一化设计要点（结论汇总，落到设计文档）

1. **统一格式以 OpenAI Chat Completions 为骨架**（老宅 2026-11-12 拍板），但必须是**超集**，额外承载：`reasoning_content`、`thinking_signature`、`cache_control` 断点信息、`cache_write` 与 `cache_read` 分列、`finish_reason` 超集。
2. **出站用各家原生形状**（老宅拍板「claude 肯定要使用自己的格式」）：每个供应商一个适配器，负责「统一格式 → 供应商请求体」与「供应商响应 → 统一格式」双向映射。
3. **usage 必须按供应商做适配器**，⛔ 不能一个 struct 硬解（缓存层级、输入语义、流式携带三处差异）。
4. **流式 usage 模式枚举化**（4 种，见 4.1 的 R3）。
5. **原始 usage 原文保留**：归一化值用于统计，原始 JSON 用于账单核对与协议排障（**【二手】** [MaaS Usage](https://wener.me/notes/ai/maas/spec/usage) 同口径：「原始 Provider usage：用于供应商账单核对和协议排障；归一化 usage：用于平台统计、限额、报表和跨 Provider 对比」）。
6. **推理内容有三个落点**，归一化时都要能接：OpenAI 形状的 `reasoning_content`（Qwen / Kimi / MiMo / MiniMax / 智谱）、MiniMax `reasoning_split=false` 时退回 `content` 的 `<think>` 包裹、Anthropic 形状的 `content[].type=="thinking"` 块 + `thinking_delta`。
7. **鉴权头三种形态**：`Authorization: Bearer`（绝大多数）、`api-key:`（MiMo 独有）、`x-api-key:` + `anthropic-version`（Claude / 智谱 Anthropic 兼容 / MiniMax Anthropic 兼容）。
8. **中断场景 usage 可能缺失**：Kimi 原文「如果流中断，可能无法收到包含总 Token 用量的最终 chunk」；智谱原文「中断时可能拿不到最终 usage，但**未收到 usage 不代表不计费**」⇒ 归一化 usage 必须带 `estimated` 标志位。

## 七、未验证与推测清单

> 以下条目**不是**查到的结论，读者勿当公论引用。

1. **【推测】供应商识别的优先级是我设计的**（host > 路径 > model 前缀 > OpenAI 兼容兜底）。各家官方文档**均未规定**「如何被一个网关识别」⇒ 这条是本项目自定规则。
2. **【推测】「统一格式 = OpenAI 骨架 + 三个扩展字段」是本项目自定**。业界有 MaaS 规格（Wener 的 [MaaS Usage](https://wener.me/notes/ai/maas/spec/usage) 给了归一化字段建议，含 `input_tokens` / `output_tokens` / `cached_tokens` / `cache_write_tokens` / `reasoning_tokens` / `provider_usage` / `estimated`），但**没有**规定「统一响应消息体」的完整形状。
3. **【未验证】MiniMax 原生 V2 的缓存命中字段名**：该接口文档未见缓存字段。
4. **【未验证】Qwen 原生 text-generation 协议的缓存字段名**：响应示例只有 `total_tokens`/`input_tokens`/`output_tokens`；`input_tokens_details.cached_tokens` 仅出现在 **Responses API** 示例中。
5. **【未验证】智谱 `finish_reason` 的 `model_context_window_exceeded`**：出现在**描述文字**但**不在枚举**内 ⇒ 以枚举为准。
6. **【未验证】Kimi 的 `temperature` / `top_p`**：现行 OpenAPI `ChatRequestCommon` 未声明该两字段，是否仍被后端接受未验证。
7. **【未验证】各家的 `/v1/models` 列表端点字段**：本轮未取（MiMo 有 `api/model/list-models-get.md`、Kimi 有 `/v1/models`、MiniMax 有 openai/anthropic 两版列表接口）。若网关要做动态模型发现需再取。
8. **【未验证】Gemini 流式 usage 的确切携带形态**：本轮确认了 `UsageMetadata` 的字段树（来自官方 Java SDK javadoc），但**未取** `streamGenerateContent` 的响应示例逐帧结构。
9. **【单一来源】MiniMax GroupId 已消失**这一结论：依据是「在中国站 5 个相关文档页 + 国际站 OpenAPI 中检索 `groupid`/`group_id` 命中数为 0」⇒ 属**检索未命中**而非官方明文宣布废弃，标注为单一来源。
10. **【未验证】xAI 的 REST 兼容层字段**：`docs.x.ai` 与 `api.x.ai` 从调研机器**不可达**（web_fetch 两次 network error、curl `SSL_ERROR_SYSCALL`）⇒ xAI 的 `usage` 字段树、`finish_reason` 取值、REST SSE 帧格式**均未在官方 REST 文档逐字验证**；本轮 xAI 一节的 usage 结构依据其**官方 SDK 仓库**（`xai-org/xai-sdk-python`，gRPC 侧 `SamplingUsage`：`completion_tokens` / `reasoning_tokens` / `prompt_tokens` / `total_tokens` / `cached_prompt_text_tokens` / `cost_in_usd_ticks` 等），**与 REST 侧 OpenAI 形状的对应关系未验证**。
11. **【未验证】Gemini `generateContent` 的 SSE 逐帧 framing**：可访问的 `ai.google.dev` 流式页已整体改写为 Interactions API 形态（`event:` + `data: [DONE]`），未再列出 `generateContent` 的帧格式 ⇒ 实施时须实测。
12. **【未验证】Gemini OpenAI 兼容层的 usage 字段名** 与 **Ollama `/v1` 兼容层的缓存 token 字段名**：官方兼容层页只确认了请求字段白名单与 `stream_options.include_usage`，未逐字列出 usage 内字段。
13. **【未验证】各家 `/v1/models` 列表端点字段**：本轮未取；若网关要做动态模型发现需再取。

## 八、证据索引（本轮实际访问过的来源）

**一手规范 / OpenAPI 原文**

- DeepSeek Chat Completions —— https://api-docs.deepseek.com/api/create-chat-completion
- DeepSeek Models & Pricing —— https://api-docs.deepseek.com/quick_start/pricing
- Anthropic Messages —— https://platform.claude.com/docs/en/api/messages.md
- Anthropic Streaming —— https://platform.claude.com/docs/en/build-with-claude/streaming.md
- Anthropic Prompt Caching —— https://platform.claude.com/docs/en/build-with-claude/prompt-caching.md
- OpenAI OpenAPI —— https://raw.githubusercontent.com/openai/openai-openapi/master/openapi.yaml
- Ollama API —— https://raw.githubusercontent.com/ollama/ollama/main/docs/api.md
- Gemini Generate Content —— https://ai.google.dev/api/generate-content
- Google GenAI UsageMetadata javadoc —— https://googleapis.github.io/java-genai/javadoc/com/google/genai/types/UsageMetadata.html
- Kimi Chat Completions（含完整 OpenAPI）—— https://platform.kimi.com/docs/api/chat.md
- Kimi API 概述 —— https://platform.kimi.com/docs/api/overview
- 智谱 HTTP API 调用 —— https://docs.bigmodel.cn/cn/guide/develop/http/introduction.md
- 智谱 对话补全 OpenAPI —— https://docs.bigmodel.cn/api-reference/模型-api/对话补全
- 智谱 流式消息 —— https://docs.bigmodel.cn/cn/guide/capabilities/streaming
- 智谱 Claude API 兼容 —— https://docs.bigmodel.cn/cn/guide/develop/claude/introduction
- Qwen 兼容 OpenAI —— https://www.alibabacloud.com/help/zh/doc-detail/2833609.html
- Qwen 深度思考 —— https://www.alibabacloud.com/help/zh/model-studio/deep-thinking
- Qwen 文本生成（原生）—— https://help.aliyun.com/zh/model-studio/text-generation
- Qwen 模型服务概览 —— https://help.aliyun.com/zh/document_detail/2586399.html
- MiMo OpenAI 兼容 —— https://mimo.mi.com/static/docs/api/chat/openai-api-post.md
- MiMo 首次 API 调用 —— https://mimo.mi.com/static/docs/quick-start/summary/first-api-call.md
- MiMo 深度思考 —— https://mimo.mi.com/static/docs/quick-start/usage-guide/text-generation/deep-thinking.md
- MiniMax Create Chat Completion —— https://platform.minimax.io/docs/api-reference/text-chat-openai
- MiniMax Create Message（Anthropic）—— https://platform.minimax.io/docs/api-reference/text-chat-anthropic
- MiniMax Text Generation（原生 V2）—— https://platform.minimax.io/docs/api-reference/text-post
- MiniMax Prompt 缓存 —— https://platform.minimax.io/docs/api-reference/text-prompt-caching
- MiniMax API 概述 —— https://platform.minimax.io/docs/api-reference/api-overview
- MiniMax 中国站前置准备 —— https://platform.minimaxi.com/docs/guides/quickstart-preparation

**二手 / 交叉印证**

- xAI Usage & Pricing（cached_tokens 结构）—— https://docs.x.ai/developers/advanced-api-usage/prompt-caching/usage-and-pricing
- xAI Chat Completions —— https://docs.x.ai/docs/guides/chat-completions
- MaaS Usage 归一化规格 —— https://wener.me/notes/ai/maas/spec/usage

## 九、复现命令

```bash
# 一手规范原文（多为 .md，比 HTML 干净）
curl -sSL https://platform.claude.com/docs/en/api/messages.md | grep -A20 '"usage"'
curl -sSL https://platform.claude.com/docs/en/build-with-claude/streaming.md | grep -E 'message_start|message_delta'
curl -sSL https://platform.kimi.com/docs/api/chat.md | grep -B2 -A12 'cache_write_tokens'
curl -sSL https://raw.githubusercontent.com/ollama/ollama/main/docs/api.md | grep -E 'prompt_eval_cached_count|eval_count'
curl -sSL https://api-docs.deepseek.com/api/create-chat-completion.md | grep -E 'prompt_cache_hit_tokens|prompt_cache_miss_tokens'
curl -sSL https://docs.bigmodel.cn/cn/guide/capabilities/streaming.md | grep -E 'chunk_include_usage'

# 关键数字核对点（arXiv 论文摘要页，逐字检索）
#   12-38%   79-85%   a third to half
curl -sSL https://arxiv.org/abs/2611.08684 | grep -oE '12-38%|79-85%|a third to half'
```

**本仓文档级自检**：见 [Go 实现设计](20261112-多供应商网关-Go实现设计.md) 的验收表。
