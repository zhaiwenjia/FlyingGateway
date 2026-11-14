# 真实样本登记表（adapter 层）

本目录是 **真实录制 / 厂商公开渠道的真实响应样本**（⛔ 非合成、⛔ 非 SDD 字段表构造），供 `internal/adapter`
各 `<供应商>_test.go` 追加的真实样本测试（`Test*ParseUsageRealSample`）与 `TestAnthropicStreamRealCapture` 读取。
获取日期一律 **2026-11-14**。16 个 `*_usage*.json` / `*_usage.json` 与 `internal/usage/testdata/real/`
同名文件**字节一致**（镜像，供两个包各自测试就地读取；`diff` 可验）；usage 对象的来源与期望值详见
`internal/usage/testdata/real/README.md` 登记表（同一批样本、同一批出处），本表逐条列出本目录文件。

| 文件 | 供应商 | 来源 | 为什么算真实样本 |
|---|---|---|---|
| anthropic_usage_cachewrite.json | Claude | `reference/litellm/tests/integration/translation/templates/anthropic.py`（`ANTHROPIC_MESSAGES_RESPONSE["usage"]`） | LiteLLM translation 测试 README 明文「captured once from the real provider and stored verbatim」= 真实 API 录制（msg_011Cg32pxq64L8VQGo1JKTnH） |
| anthropic_usage_cachehit.json | Claude | `reference/litellm/tests/integration/translation/messages/prompt_caching/test_messages_prompt_caching_anthropic.py` | 同上，真实缓存命中轮（msg_011Cg32qgbJRU3vZX2TB6vPx） |
| openai_responses_usage.json | OpenAI | `reference/litellm/tests/integration/translation/templates/openai.py`（`OPENAI_RESPONSES_RESPONSE["usage"]`） | 同上真实录制（resp_0c11adb5…） |
| openai_chat_usage_azure.json | OpenAI（Azure 部署，Chat 形） | `reference/litellm/tests/integration/translation/messages/bases/azure.py` | 同上真实录制（chatcmpl-EVjWSpHSltjjRtiCWAwJpzcB7dyOQ；含真实未知字段 `latency_checkpoint`） |
| gemini_usage_metadata.json | Gemini | `reference/litellm/tests/integration/translation/templates/gemini.py`（`usageMetadata`） | 同上真实录制（responseId 1CbQat_JEK-jqtsPtf-QoQU） |
| deepseek_usage.json | DeepSeek | https://api-docs.deepseek.com/api/create-chat-completion/ 响应示例 | 官方 API 参考页响应示例（真实 id/指纹），亦是该页流式末帧 usage |
| kimi_usage.json | Kimi | https://platform.moonshot.ai/docs/api/chat 非流式示例 | 官方 API 文档响应示例（真实 id cmpl-04ea9261…） |
| zhipu_usage.json | 智谱 | https://docs.bigmodel.cn/cn/guide/capabilities/cache 响应示例 | 官方文档响应示例（真实 request_id） |
| qwen_compat_usage.json | Qwen | https://www.alibabacloud.com/help/doc-detail/2862577.html Cache hit examples · OpenAI-compatible | 官方文档响应示例（真实 id chatcmpl-6ada9ed2-…） |
| qwen_native_usage.json | Qwen | 同上文档 Cache hit examples · DashScope | 官方文档响应示例（真实 request_id） |
| minimax_usage.json | MiniMax | https://platform.minimax.cn/docs/api-reference/text-post 响应示例 | 官方 API 文档响应示例（真实 id 04ecb5d9…） |
| mimo_usage_asr.json | MiMo | https://mimo.mi.com/docs/zh-CN/api/audio/Speech-Recognition-post 响应示例 | 小米官方文档响应示例（真实 id 9f51eba4…） |
| mimo_usage_audio.json | MiMo | https://mimo.mi.com/docs/quick-start/usage-guide/multimodal-understanding/audio-understanding 响应示例 | 同上（真实 id 020d7a2c-…） |
| grok_responses_usage.json | Grok | https://docs.x.ai/docs/cost-tracking usage 示例 | xAI 官方文档示例（含 `cost_in_usd_ticks: 158500`） |
| ollama_usage_real.json | Ollama | https://www.coder000.com/post/ollama-api-response-cut-off-done-reason-length-num-predict | 排障实录贴真机响应（Ollama 0.40.14）⚠️ 原文 `context` 数组含省略号 ⇒ 仅裁掉该字段，其余逐字 |
| ollama_usage_docs.json | Ollama | https://docs.ollama.com/api/chat 响应示例 | Ollama 官方文档响应示例 |
| anthropic_stream_real.sse | Claude | `reference/litellm/tests/integration/translation/chat_completions/streaming/test_chat_completions_streaming_anthropic.py` 的 `mock_provider_frames`（msg_011CfyYUVrHK4BM53hfSH5Ld） | 真实 API 流式帧捕获（同上 verbatim 口径）；帧 data 载荷逐字照抄，SSE 信封（`event:` / `data:` 行）按 Anthropic 线格式装配 |
| deepseek_stream_real.sse | DeepSeek | https://api-docs.deepseek.com/api/create-chat-completion/ 流式响应示例 | 官方文档完整逐帧示例（真实 id 071f229d-… 与 system_fingerprint），逐字照抄 |
| kimi_stream_docs_example.txt | Kimi | https://platform.moonshot.ai/docs/api/chat 「Streaming Response」示例块 | 官方文档示例块逐字照抄（⚠️ id 为文档占位 `cmpl-xxx`；文档以 `...` 省略中间帧，原样保留该行、⛔ 不补造省略帧） |
| zhipu_stream_docs_example.txt | 智谱 | https://docs.bigmodel.cn/cn/guide/capabilities/streaming 「响应示例」块 | 官方文档示例块逐字照抄（⚠️ id 为文档占位 `"1"`；文档以 `...` 省略中间帧，原样保留、⛔ 不补造） |

## 归一化口径与真实样本的出入（⚠️ 只列不改）

与 `internal/usage/testdata/real/README.md`「出入」小节同一批结论，要点：
1. Qwen 原生样本 `prompt_tokens_details.cached_tokens=2048` 未入 `parseQwenNative`（Read=0）。
2. MiMo 样本 `completion_tokens_details.reasoning_tokens=66` 未入 `parseMiMo`（Reasoning=0）。
3. Grok Chat 形缓存字段：实测/官方文档显示 `prompt_tokens_details.cached_tokens`，SDD 取
   `cached_prompt_text_tokens`（Chat 形 raw JSON 未取得，见缺口）。
4. 流式帧形状：DeepSeek / Kimi / 智谱 / MiMo 真实流为 `chat.completion.chunk` 形，本仓适配器流式解码按
   Responses 事件形 ⇒ 真实流样本不能直接喂 `StreamStart`（Anthropic 原生 SSE 形吻合，已加解析测试）。

## 缺口（检索记录：如实留空，⛔ 不拿合成的顶替）

- **Grok Chat 形 raw usage JSON**：docs.x.ai 成本追踪页只有 Responses 形示例；computingforgeeks 实测贴
  （2026-09-25 真实调用）只有 jq 打印值（prompt 1258 / cached 1152 / completion 62 / reasoning 121 /
  cost \$0.001886）非原始 JSON；litellm 全树无 `cached_prompt_text_tokens` 字面量。⇒ 留空。
- **OpenAI 官方直连 Chat 形 raw usage**：litellm 真实录制层只有 Azure OpenAI 部署捕获（同构，已收录）。
- **流式完整逐帧真实记录**：OpenAI Chat chunk / Gemini / Qwen / MiniMax / MiMo / Grok / Ollama 未取得
  （文档流式示例为片段或未给逐帧）。已取得：Anthropic（真实捕获）、DeepSeek（官方完整示例）、
  Kimi/智谱（官方示例块，含省略标记）。
- **Ollama `/v1` 兼容层 usage**：排障贴实测过 `/v1` 但未贴原始 usage JSON ⇒ 留空。
