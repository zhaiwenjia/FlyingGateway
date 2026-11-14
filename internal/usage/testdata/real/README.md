# 真实样本登记表（usage 层）

本目录是 **真实录制 / 厂商公开渠道的真实响应样本**（⛔ 非合成、⛔ 非 SDD 字段表构造），供 `internal/usage` 的
`TestNormalizeRealSamples` 钉住「归一化口径在真实样本上成立」。同名镜像文件在
`internal/adapter/testdata/real/`（字节一致，供各 `adapter.ParseUsage` 测试读取）。
获取日期一律 **2026-11-14**。每个样本是真实响应里 `usage` / `usageMetadata` 对象本体（Ollama 除外，见备注），
字段名与数值逐字照抄来源，⛔ 不改写。

| 文件 | 供应商 | 来源 | 为什么算真实样本 | 归一化期望（Input / Read / Write / Uncached / Out / Reasoning / Total） |
|---|---|---|---|---|
| anthropic_usage_cachewrite.json | Claude | `reference/litellm/tests/integration/translation/templates/anthropic.py` 的 `ANTHROPIC_MESSAGES_RESPONSE["usage"]`（msg_011Cg32pxq64L8VQGo1JKTnH） | LiteLLM translation 测试 README 明文「Provider responses in translation cases are captured once from the real provider and stored verbatim」= 真实 API 一次录制、逐字保存（真实 msg id 与 thinking signature 可证） | 880 / 0 / 876 / 4 / 235 / – / –（Input = 4+876+0 残值合成） |
| anthropic_usage_cachehit.json | Claude | `reference/litellm/tests/integration/translation/messages/prompt_caching/test_messages_prompt_caching_anthropic.py` 的 `CLAUDE_SONNET_5_5_CACHE_HIT_RESPONSE_FIELDS["usage"]`（msg_011Cg32qgbJRU3vZX2TB6vPx） | 同上：真实录制的**缓存命中轮**响应（与 cachewrite 同请求的后续真实调用） | 877 / 873 / 0 / 4 / 254 / – / –（Input = 4+0+873） |
| openai_responses_usage.json | OpenAI | `reference/litellm/tests/integration/translation/templates/openai.py` 的 `OPENAI_RESPONSES_RESPONSE["usage"]`（resp_0c11adb5…，gpt-5.6-sol） | 同上真实录制（Responses 形，id/usage 逐字保留） | 309 / 0 / 0 / 309 / 118 / 53 / 427 |
| openai_chat_usage_azure.json | OpenAI（Azure OpenAI 部署，Chat 形） | `reference/litellm/tests/integration/translation/messages/bases/azure.py` 的 `mock_provider_response["usage"]`（chatcmpl-EVjWSpHSltjjRtiCWAwJpzcB7dyOQ，gpt-5.4） | 同上真实录制；Azure OpenAI 与 OpenAI Chat 形 usage 同构，且带真实未知字段 `latency_checkpoint`（解析须不受干扰） | 19 / 0 / 0 / 19 / 5 / 0 / 24 |
| gemini_usage_metadata.json | Gemini | `reference/litellm/tests/integration/translation/templates/gemini.py` 的 `usageMetadata`（responseId 1CbQat_JEK-jqtsPtf-QoQU，gemini-3.8-flash） | 同上真实录制 | 358 / 0 / 0 / 358 / 655 / – / 1013（Out = 80+575 thoughts 独立加项） |
| deepseek_usage.json | DeepSeek | https://api-docs.deepseek.com/api/create-chat-completion/ （Chat Completions API 参考页响应示例，id 88beaa90-17fd-45fc-939c-2ef75e2d913c + system_fingerprint） | 厂商官方 API 参考页的响应示例：含真实 id / 指纹，字段数值逐字照抄；同一 usage 也是该页流式示例**末帧**的 usage | 7 / 0 / 0 / 7 / 11 / – / 18（Uncached 取 miss 字段 7） |
| kimi_usage.json | Kimi | https://platform.moonshot.ai/docs/api/chat （Chat Completions API 非流式示例响应，id cmpl-04ea926191a14749b7f2c7a48a68abc6，kimi-k2.6） | 厂商官方 API 文档的响应示例（真实 id，字段数值逐字照抄） | 19 / 10 / 0 / 9 / 21 / – / 40 |
| zhipu_usage.json | 智谱 | https://docs.bigmodel.cn/cn/guide/capabilities/cache （上下文缓存指南「响应包含上下文缓存的 Token 使用信息」示例，glm-5.3，request_id 202609171836159342f8e00bec415f） | 厂商官方文档的响应示例（真实 request_id 与命中 1024 的实际用量，逐字照抄） | 1075 / 1024 / 0 / 51 / 241 / 0 / 1316 |
| qwen_compat_usage.json | Qwen | https://www.alibabacloud.com/help/doc-detail/2862577.html （Context Cache 文档 Cache hit examples · OpenAI-compatible，qwen-plus，id chatcmpl-6ada9ed2-7f33-9de2-8bb0-78bd4035025a） | 厂商官方文档的响应示例（真实 id 与命中 2048 的实际用量，逐字照抄） | 3019 / 2048 / 0 / 971 / 104 / – / 3123 |
| qwen_native_usage.json | Qwen | 同上文档 Cache hit examples · DashScope（request_id f3acaa33-e248-97bb-96d5-cbeed34699e1） | 同上（DashScope 原生形，逐字照抄） | 3019 / 0 / – / 3019 / 101 / – / 3120 ⚠️ 样本 `prompt_tokens_details.cached_tokens=2048` 未被原生口径捕捉（见出入 1） |
| minimax_usage.json | MiniMax | https://platform.minimax.cn/docs/api-reference/text-post （文本合成接口响应示例，id 04ecb5d9b1921ae0fb0e8da9017a5474，MiniMax-M3.1-Flash） | 厂商官方 API 文档的响应示例（真实 id，逐字照抄；含 `total_characters` 真实字段） | 26 / 0 / 0 / 26 / 223 / 214 / 249 |
| mimo_usage_asr.json | MiMo | https://mimo.mi.com/docs/zh-CN/api/audio/Speech-Recognition-post （语音识别响应示例，id 9f51eba459dd4dfdabb31cabba0cb7dc，mimo-v2.5-asr） | 小米 MiMo 官方文档的响应示例（真实 id，逐字照抄） | 46 / 45 / 0 / 1 / 20 / 0 / 66 |
| mimo_usage_audio.json | MiMo | https://mimo.mi.com/docs/quick-start/usage-guide/multimodal-understanding/audio-understanding （音频理解响应示例，id 020d7a2c-f8e7-4e5a-aaea-790ba908b51a…，mimo-v2.6-pro） | 同上 | 86 / 0 / 0 / 86 / 98 / 0 / 184 ⚠️ 样本 `completion_tokens_details.reasoning_tokens=66` 未被 MiMo 口径捕捉（见出入 2） |
| grok_responses_usage.json | Grok | https://docs.x.ai/docs/cost-tracking （Cost Tracking 文档「Chat Completions and Responses API」usage 示例） | xAI 官方文档的响应 usage 示例（含自报费用 `cost_in_usd_ticks: 158500`，逐字照抄） | 199 / 0 / 0 / 199 / 1 / – / 200 |
| ollama_usage_real.json | Ollama | https://www.coder000.com/post/ollama-api-response-cut-off-done-reason-length-num-predict （排障实录贴，Ollama 0.40.14 + qwen2.5:0.5b 真机复现的 /api/generate 响应） | 排障原文贴的真实响应（真机版本、时间戳、纳秒耗时俱在）⚠️ 原文 `"context":[151644,...]` 含省略号无法逐字照抄 ⇒ 仅裁掉 `context` 字段，其余字段逐字 | 40 / 24 / 0 / 16 / 10 / – / –（无总值原报 ⇒ Total 0） |
| ollama_usage_docs.json | Ollama | https://docs.ollama.com/api/chat （/api/chat 响应示例，gemma4） | Ollama 官方 API 文档的响应示例（逐字照抄） | 11 / 0 / 0 / 11 / 18 / – / –（样本无 `prompt_eval_cached_count` ⇒ Read 0） |

「–」= 该样本无对应字段、归一结果为 0。镜像一致性：两个目录下同名 `.json` 字节一致（`diff` 可验）。

## 归一化口径与真实样本的出入（⚠️ 只列不改，实现口径以 SDD 为准）

1. **Qwen 原生缓存命中未入归一**：`qwen_native_usage.json` 带 `prompt_tokens_details.cached_tokens=2048`，
   而 `parseQwenNative`（SDD 4.4.2 Qwen 原生行「无缓存字段」）⇒ `CacheReadTokens=0`、Uncached=3019。
   真实样本显示 DashScope 原生形**有**缓存命中上报。
2. **MiMo reasoning 未入归一**：`mimo_usage_audio.json` 带 `completion_tokens_details.reasoning_tokens=66`，
   `parseMiMo`（SDD 4.3.10）不取 reasoning ⇒ `ReasoningTokens=0`（输出计费侧已含在 OutputTokens=98 内）。
3. **Grok Chat 形缓存字段名存疑**：SDD 4.3.5 / `parseGrokChat` 取 `cached_prompt_text_tokens`；
   但公开实测（computingforgeeks《Grok 4.7 Tested》，2026-09-25 对 xAI 真实 API 调用）打印的是
   `usage.prompt_tokens_details.cached_tokens`（实测 1258/1152/62/121），且 xAI 官方 prompt-caching 文档
   （https://docs.x.ai/developers/advanced-api-usage/prompt-caching/maximizing-cache-hits）示例代码也读
   `prompt_tokens_details.cached_tokens`。⚠️ 未取得 Grok Chat 形原始 usage JSON（见缺口），按现状归一
   Chat 形真实样本会 `CacheReadTokens=0`。
4. **流式帧形状与适配器解码器不一致**：DeepSeek / Kimi / 智谱 / MiMo 的真实流式是 `chat.completion.chunk` 形
   （见 `internal/adapter/testdata/real/` 的流式样本），而本仓对应适配器的流式解码按 Responses 事件形
   （`response.output_text.delta` / `response.completed`）⇒ 真实流样本 ⛔ 不能直接喂 `StreamStart`；
   仅 Anthropic（原生 SSE 事件）与 Ollama `/v1` 兼容层（`chat.completion.chunk` 解码）形状吻合。

## 缺口（检索记录：如实留空，⛔ 不拿合成的顶替）

- **Grok Chat 形 raw usage JSON**：搜「xAI Grok usage cached_prompt_text_tokens / cost_in_usd_ticks」——
  docs.x.ai 成本追踪页仅有 Responses 形 usage 示例（已收录）；computingforgeeks 实测贴只有 jq 打印的字段值
  非原始 JSON；litellm 全树无 `cached_prompt_text_tokens` 字面量。⇒ Chat 形样本留空。
- **OpenAI Chat（官方直连）raw usage**：litellm 真实录制层只有 Azure OpenAI 部署的 Chat 形捕获（已收录，
  同构）；OpenAI 官网文档为 JS 渲染页未取得可逐字照抄的示例块。⇒ 以 Azure 捕获代表 Chat 形。
- **流式帧缺口**：OpenAI Chat chunk / Gemini streamGenerateContent / Qwen / MiniMax / MiMo / Grok / Ollama
  的完整逐帧真实记录未取得（各文档流式示例为片段或未给逐帧）；已取得的流式样本见
  `internal/adapter/testdata/real/`（Anthropic 真实捕获、DeepSeek 官方文档完整示例、Kimi/智谱官方文档示例块）。
- **Ollama `/v1` 兼容层 usage**：排障贴实测过 `/v1` 但未贴原始 usage JSON ⇒ 留空（原生形样本已收录）。
