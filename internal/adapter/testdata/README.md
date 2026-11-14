# testdata fixture 来源标注

本目录是 `internal/adapter` 测试的响应 / usage fixture。每条 fixture 的来源分两类：

- **SDD 逐字样本**：数字与字段名逐字取自《docs/20261114-统一SDD.md》对应小节的样本块，⛔ 不改写。
- **SDD 字段表构造**：SDD 未给样本块的，按对应小节的字段表构造，字段名逐字照抄，数值取便于对账的自造整数（README 逐条注明构造依据）。

| fixture 文件 | 来源 | 逐字 / 构造 | 归一化期望（Input / CacheRead / CacheWrite / Output） |
|---|---|---|---|
| openai_chat_usage.json | SDD 4.3.1「Chat token 计量」样本块 | 逐字（125/48/173/98） | 125 / 98 / 0 / 48（Uncached=27） |
| openai_responses_usage.json | SDD 4.3.1「Responses token 计量」字段表 | 构造（input 200, cached 20, write 5+3, output 50） | 200 / 20 / 8 / 50（Uncached=172） |
| openai_responses_response.json | SDD 4.3.1 响应体关键字段 + item 内部结构 | 构造（output[0]=reasoning, output[1]=message） | — |
| anthropic_usage_sample.json | SDD 4.3.2「token 计量（usage 对象）」样本块 | 逐字（25/15） | 25 / 0 / 0 / 15（Uncached=25） |
| anthropic_usage_residual.json | SDD 11.1「Anthropic 残值」行 + 4.4.3 合成公式 | 逐字（input_tokens=50, cache_read=200000） | 200050 / 200000 / 0 / 30（Uncached=50） |
| anthropic_response.json | SDD 4.3.2 响应体关键字段（content[] 块数组） | 构造（text + thinking(signature) + tool_use，stop_reason=tool_use） | — |
| deepseek_usage_sample.json | SDD 4.3.4「token 计量」样本块 | 逐字（7/11/18, hit 0, miss 7） | 7 / 0 / 0 / 11（Uncached=7） |
| deepseek_usage_hitmiss.json | SDD 4.3.4「命中/未命中双字段，取其一⛔不累加」 | 构造（prompt 7, hit 5, cached 5 同值, miss 2） | 7 / 5（⛔ 非 10）/ 0 / 11（Uncached=2） |
| deepseek_response.json | SDD 4.3.4 + 4.5.2 DeepSeek 行 | 构造（finish_reason=aborted 独有值） | — |
| xai_chat_usage.json | SDD 4.3.5 Chat Completions usage 字段 | 构造（cached_prompt_text_tokens=40） | 100 / 40 / 0 / 20（Uncached=60） |
| xai_responses_usage.json | SDD 4.3.5 Responses usage 字段（含 cost_in_usd_ticks） | 构造 | 200 / 0 / 0 / 50（cost_in_usd_ticks 随 Raw 保留） |
| kimi_usage.json | SDD 11.1「Kimi 三分」行 | 逐字（309/53/256） | 309 / 53 / 256 / 21（Uncached=0） |
| zhipu_usage.json | SDD 4.3.8 token 计量字段表 | 构造（prompt 100, cached 30, completion 60 含 reasoning 20） | 100 / 30 / 0 / 60（⛔ reasoning 不二次加） |
| zhipu_response.json | SDD 4.5.2 智谱行 finish_reason 取值 | 构造（sensitive 独有值） | — |
| qwen_compat_usage.json | SDD 4.4.2 Qwen 兼容行 | 构造（cached 25, cache_creation_input_tokens 10） | 100 / 25 / 10 / 40（Uncached=65） |
| qwen_native_usage.json | SDD 4.3.7 原生 text-generation usage | 构造（input_tokens 17, output_tokens 64） | 17 / 0 / 0 / 64 |
| qwen_native_response.json | SDD 4.3.7 原生响应字段表 | 构造 | — |
| gemini_usage.json | SDD 4.3.3 usageMetadata 字段表 | 构造（prompt 100 + toolUse 20, candidates 30 + thoughts 10, cached 40, total 160） | 120 / 40 / 0 / 40（⛔ total 保留原报不重算） |
| gemini_response.json | SDD 4.3.3 响应体关键字段 | 构造（finishReason=STOP） | — |
| minimax_usage.json | SDD 4.3.11 OpenAI 形 usage 字段 | 构造（无缓存字段） | 100 / 0 / 0 / 30 |
| minimax_response_error.json | SDD 4.3.11 错误对象 base_resp | 构造（status_code=1008 余额不足） | — |
| minimax_response_ok.json | SDD 4.3.11 响应体关键字段 | 构造（finish_reason=length） | — |
| mimo_usage.json | SDD 4.3.10 token 计量字段表 | 构造（cached 15） | 80 / 15 / 0 / 20（Uncached=65） |
| mimo_response.json | SDD 4.5.2 MiMo 行 finish_reason 取值 | 构造（repetition_truncation 独有值） | — |
| ollama_usage.json | SDD 11.1「Ollama 平铺」行 + 4.3.6 字段表 | prompt_eval_count=26 / prompt_eval_cached_count=13 逐字；eval_count=18 构造（11.1 行未给输出值） | 26 / 13 / 0 / 18（Uncached=13） |
| ollama_response.json | SDD 4.3.6 原生响应字段表 | 构造（done_reason=stop） | — |
| ollama_v1_usage.json | SDD 4.4.2 Ollama /v1 行 | 构造（无缓存明细） | 30 / 0 / 0 / 12 |
| ollama_v1_response.json | SDD 4.3.6 /v1 兼容层字段表 | 构造 | — |

⚠️ **Gemini thoughts 口径说明**：SDD 11.1「Gemini 含 thoughts」行压缩措辞为「OutputTokens 取 candidatesTokenCount，不把 thoughts 重复加进输出」；
本测试与既有 `internal/usage/normalize_test.go` 统一按 4.3.3 / 4.4.2 的展开公式执行：
$\mathrm{OutputTokens} = \mathrm{candidatesTokenCount} + \mathrm{thoughtsTokenCount}$（thoughts 是独立加项、不含在 candidates 内），
「不重复加」指 ⛔ 不在已含 thoughts 的总数上再加一次；`totalTokenCount` 保留原报 ⛔ 不重算。

⚠️ **thinking 标签字面量**：MiniMax 的 think 标签 fixture 一律在 Go 测试代码里用 `\x3c` 转义拼装
（写入链路会静默吞掉尖括号闭合形态的字面量，2026-11-14 实测），故本目录 ⛔ 不放含 think 标签的 JSON。
