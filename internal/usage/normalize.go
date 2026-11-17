// 日期: 2026-11-14 19:39:55.250
// 作者: Zhai Wenjia

// Package usage 实现 token 用量归一化（统一 SDD 3.2 / 4.4）：
// 把 11 家供应商的原始 usage JSON 归一化为 core.Usage。
// 归一化目标字段：InputTokens（总输入）/ CacheReadTokens / CacheWriteTokens / OutputTokens；
// 缓存读写分列不累加；Anthropic 系 input_tokens 是残值、总输入须三项合成；
// 解析失败不 panic，返回 Estimated=true 的保守结果并在 Raw 保留原文。
package usage

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// Normalize 把 provider 原始 usage JSON 归一化为 core.Usage（统一 SDD 3.2 / 4.4）。
//
// 入参 raw 是 provider 原始 usage JSON（usage / usageMetadata 对象本体），
// 原文逐字存入 core.Usage.Raw（账单核对 / 协议排障）。
// 解析失败不 panic：返回 Estimated=true 的保守结果（字段全 0、均非负），Raw 保留原文。
func Normalize(provider core.Provider, shape core.APIShape, raw json.RawMessage) *core.Usage {
	u, err := normalize(provider, shape, raw)
	if err != nil {
		// 保守结果：usage 缺失 / 解析失败 ⇒ Estimated=true（3.2「流中断」同口径），
		// ⛔ 不按 0 计（Estimated 标志使下游按估算处理）、⛔ 不产生负数。
		return &core.Usage{
			Estimated: true,
			Raw:       copyRaw(raw),
		}
	}
	u.Raw = copyRaw(raw)
	finalize(u)
	return u
}

// normalize 按 (provider, shape) 分发到逐家口径解析器。
// shape 优先（Anthropic / Gemini / Responses 由线上形状决定），
// 其余（OpenAIChat 与未知形状）按 provider 分派；ProviderUnknown 走 OpenAI Chat 兼容形（3.3 兜底口径）。
// ⛔ 国内供应商无原生形状分派（Qwen 原生 / MiniMax 原生 V2 / Ollama 原生已移除）——出站统一 Responses 通道。
func normalize(provider core.Provider, shape core.APIShape, raw json.RawMessage) (*core.Usage, error) {
	switch shape {
	case core.APIShapeAnthropicMessages:
		// Anthropic 形（Claude）：input_tokens 是残值语义。
		return parseAnthropic(raw)
	case core.APIShapeGeminiNative:
		return parseGeminiNative(raw)
	case core.APIShapeOpenAIResponses:
		// Responses 形（OpenAI / Grok Responses）。Grok Responses 无缓存字段 ⇒ 解析后记 0。
		return parseResponses(raw)
	}

	switch provider {
	case core.ProviderAnthropic:
		return parseAnthropic(raw)
	case core.ProviderDeepSeek:
		return parseDeepSeek(raw)
	case core.ProviderMoonshot:
		return parseKimi(raw)
	case core.ProviderZhipu:
		return parseZhipu(raw)
	case core.ProviderQwen:
		return parseQwenCompat(raw)
	case core.ProviderMiniMax:
		return parseMiniMax(raw)
	case core.ProviderXiaomi:
		return parseMiMo(raw)
	case core.ProviderXAI:
		return parseGrokChat(raw)
	case core.ProviderOllama:
		return parseOllamaV1(raw)
	default:
		// OpenAI Chat 形（含 ProviderUnknown 兜底、Gemini OpenAI 兼容层）。
		return parseOpenAIChat(raw)
	}
}

// ---- 逐家解析器（口径出处：SDD 3.2 逐家表 / 4.4.2 逐家字段对照总表 / 4.4.3 输入 token 语义对照）----

// openAIChatUsage 是 OpenAI Chat 形 usage（4.4.2：缓存字段在 usage 下二级）。
type openAIChatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
		// CacheWriteTokens 是缓存写入量（4.4.2 OpenAI Chat 行：prompt_tokens_details.cache_write_tokens，
		// TTL 30m / 12h）。3.2 OpenAI 行「无独立字段 ⇒ 0」= 原始 usage 无此字段时记 0。
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// parseOpenAIChat 按 OpenAI Chat 口径归一（4.4.3：prompt_tokens 是总输入，直接用）。
func parseOpenAIChat(raw json.RawMessage) (*core.Usage, error) {
	var v openAIChatUsage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	cached := v.PromptTokensDetails.CachedTokens
	write := v.PromptTokensDetails.CacheWriteTokens
	return &core.Usage{
		InputTokens:         v.PromptTokens,
		CacheReadTokens:     cached,
		CacheWriteTokens:    write,
		InputTokensUncached: uncachedOpenAI(v.PromptTokens, cached, write),
		OutputTokens:        v.CompletionTokens,
		ReasoningTokens:     v.CompletionTokensDetails.ReasoningTokens,
		TotalTokens:         v.TotalTokens,
		CacheReadMode:       core.CacheModeExplicit, // OpenAI prompt caching 显式档（core 常量口径）
	}, nil
}

// responsesDetails 是 Responses 形 input_tokens_details（4.4.2 OpenAI Responses 行：
// cache_write_tokens 带 TTL 细分 ttl_30m_tokens / ttl_12h_tokens，两种形态都可能出现）。
type responsesDetails struct {
	CachedTokens     int64           `json:"cached_tokens"`
	CacheWriteTokens json.RawMessage `json:"cache_write_tokens"`
	TTL30mTokens     int64           `json:"ttl_30m_tokens"`
	TTL12hTokens     int64           `json:"ttl_12h_tokens"`
}

// responsesUsage 是 OpenAI Responses 形 usage（4.4.2：input_tokens / output_tokens 为总输入 / 输出）。
type responsesUsage struct {
	InputTokens         int64            `json:"input_tokens"`
	OutputTokens        int64            `json:"output_tokens"`
	TotalTokens         int64            `json:"total_tokens"`
	InputTokensDetails  responsesDetails `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// parseResponses 按 Responses 口径归一（4.4.3：input_tokens 是总输入，直接用）。
// Grok Responses 无缓存字段 ⇒ CacheRead / CacheWrite 解析后为 0（4.4.2 Grok Responses 行）。
func parseResponses(raw json.RawMessage) (*core.Usage, error) {
	var v responsesUsage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	cached := v.InputTokensDetails.CachedTokens
	write := resolveResponsesCacheWrite(v.InputTokensDetails)
	return &core.Usage{
		InputTokens:         v.InputTokens,
		CacheReadTokens:     cached,
		CacheWriteTokens:    write,
		InputTokensUncached: uncachedOpenAI(v.InputTokens, cached, write),
		OutputTokens:        v.OutputTokens,
		ReasoningTokens:     v.OutputTokensDetails.ReasoningTokens,
		TotalTokens:         v.TotalTokens,
		CacheReadMode:       core.CacheModeExplicit,
	}, nil
}

// resolveResponsesCacheWrite 解析 Responses 形缓存写入量：
// cache_write_tokens 可能是数值，也可能是 {ttl_30m_tokens, ttl_12h_tokens} 细分对象（4.4.2），
// 两级都没有时退回 details 顶层的 TTL 细分字段。两个 TTL 档是**互斥分段**（30m 与 12h 各写各的），
// 相加是分段求和、⛔ 不是同值双报累加。
// 按字面形状分派、每种形态只 Unmarshal 一次（旧实现「先试数值、再试对象」对对象形态
// 必白跑一次失败解析）；实测口径：null/字符串/布尔/数组两种做法都退回 TTL 顶字段
// ⇒ 分派后的行为与旧实现逐字一致。
func resolveResponsesCacheWrite(d responsesDetails) int64 {
	raw := bytes.TrimSpace(d.CacheWriteTokens)
	if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		switch raw[0] {
		case '{':
			var sub struct {
				TTL30mTokens int64 `json:"ttl_30m_tokens"`
				TTL12hTokens int64 `json:"ttl_12h_tokens"`
			}
			if err := json.Unmarshal(raw, &sub); err == nil {
				return sub.TTL30mTokens + sub.TTL12hTokens
			}
		case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			var n int64
			if err := json.Unmarshal(raw, &n); err == nil {
				return n
			}
		}
	}
	return d.TTL30mTokens + d.TTL12hTokens
}

// anthropicUsage 是 Anthropic Messages 形 usage（4.4.2：缓存字段在 usage 顶层）。
type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

// parseAnthropic 按 Anthropic 口径归一（3.2「输入必准」/ 4.4.3）：
// input_tokens 是残值（仅最后一个缓存断点之后的新增部分）⇒
// InputTokens = input_tokens + cache_creation_input_tokens + cache_read_input_tokens（三项之和），
// InputTokensUncached = input_tokens（残值本身）。
// 例（SDD 3.2）：input_tokens=50, cache_read=200000 ⇒ InputTokens=200050（⛔ 不是 50）。
func parseAnthropic(raw json.RawMessage) (*core.Usage, error) {
	var v anthropicUsage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	return &core.Usage{
		InputTokens:         v.InputTokens + v.CacheCreationInputTokens + v.CacheReadInputTokens,
		CacheReadTokens:     v.CacheReadInputTokens,
		CacheWriteTokens:    v.CacheCreationInputTokens,
		InputTokensUncached: v.InputTokens,
		OutputTokens:        v.OutputTokens,
		CacheReadMode:       core.CacheModeExplicit, // cache_control 显式缓存
	}, nil
}

// deepSeekUsage 是 DeepSeek usage（4.4.2：命中字段顶层 + 二级同值）。
type deepSeekUsage struct {
	PromptTokens         int64 `json:"prompt_tokens"`
	CompletionTokens     int64 `json:"completion_tokens"`
	TotalTokens          int64 `json:"total_tokens"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
	// PromptCacheMissTokens 是未命中量；缺失时按 prompt_tokens − 命中 推（4.4.2：prompt_tokens = 命中 + 未命中）。
	PromptCacheMissTokens *int64 `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails   struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// parseDeepSeek 按 DeepSeek 口径归一（4.4.2 / 4.4.3）：
// prompt_tokens = 命中 + 未命中（总输入，直接用）；
// prompt_cache_hit_tokens 与 prompt_tokens_details.cached_tokens 同值 ⇒ 取其一 ⛔ 不累加。
func parseDeepSeek(raw json.RawMessage) (*core.Usage, error) {
	var v deepSeekUsage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	hit := v.PromptCacheHitTokens
	if hit == 0 {
		// 顶层缺 / 为 0 时取二级同值字段（二者同值，取其一，⛔ 不累加）。
		hit = v.PromptTokensDetails.CachedTokens
	}
	uncached := v.PromptTokens - hit
	if v.PromptCacheMissTokens != nil {
		uncached = *v.PromptCacheMissTokens
	}
	return &core.Usage{
		InputTokens:         v.PromptTokens,
		CacheReadTokens:     hit,
		InputTokensUncached: nonNeg(uncached),
		OutputTokens:        v.CompletionTokens,
		TotalTokens:         v.TotalTokens,
		CacheReadMode:       core.CacheModeImplicit, // 前缀自动命中
	}, nil
}

// kimiUsage 是 Kimi / Moonshot usage（4.4.2：cached_tokens 顶层与二级同值，cache_write_tokens 在二级）。
type kimiUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	PromptTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
}

// parseKimi 按 Kimi 口径归一（3.2 / 11.1「Kimi 三分」）：
// prompt_tokens 是总数 = 未命中 + 命中 + 写入（三段相加才等于 prompt_tokens）；
// cached_tokens 顶层与 prompt_tokens_details.cached_tokens 同值 ⇒ 取其一 ⛔ 不累加；
// cache_write_tokens 单列（⛔ 不与命中合并）。
// 例（11.1）：prompt_tokens=309, cached=53, cache_write=256 ⇒ InputTokens=309、CacheRead=53、
// CacheWrite=256、Uncached=0（三者互斥，不累加）。
func parseKimi(raw json.RawMessage) (*core.Usage, error) {
	var v kimiUsage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	cached := v.CachedTokens
	if cached == 0 {
		cached = v.PromptTokensDetails.CachedTokens
	}
	write := v.PromptTokensDetails.CacheWriteTokens
	return &core.Usage{
		InputTokens:         v.PromptTokens,
		CacheReadTokens:     cached,
		CacheWriteTokens:    write,
		InputTokensUncached: nonNeg(v.PromptTokens - cached - write),
		OutputTokens:        v.CompletionTokens,
		TotalTokens:         v.TotalTokens,
		CacheReadMode:       core.CacheModeImplicit, // 自动命中（core 常量口径）
	}, nil
}

// zhipuUsage 是智谱 GLM usage（4.4.2：命中在二级，无写入；completion 含 reasoning）。
type zhipuUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// parseZhipu 按智谱口径归一（3.2）：命中单列、无写入（存储限时免费）、思考计入输出计费
// ⇒ OutputTokens = completion_tokens（已含 reasoning，⛔ 不再把 reasoning 加一次）。
func parseZhipu(raw json.RawMessage) (*core.Usage, error) {
	var v zhipuUsage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	cached := v.PromptTokensDetails.CachedTokens
	return &core.Usage{
		InputTokens:         v.PromptTokens,
		CacheReadTokens:     cached,
		InputTokensUncached: nonNeg(v.PromptTokens - cached),
		OutputTokens:        v.CompletionTokens,
		ReasoningTokens:     v.CompletionTokensDetails.ReasoningTokens,
		TotalTokens:         v.TotalTokens,
		CacheReadMode:       core.CacheModeImplicit, // 隐式缓存自动命中
	}, nil
}

// qwenCompatUsage 是 Qwen / DashScope 兼容层 usage（4.4.2：命中 + 创建写入都在二级）。
type qwenCompatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
		// CacheCreationInputTokens 是显式缓存创建写入量；用指针探测字段是否出现
		// （显式 / 隐式按 raw 是否含对应字段标 CacheReadMode）。
		CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	} `json:"prompt_tokens_details"`
}

// parseQwenCompat 按 Qwen 兼容层口径归一（4.4.2 / 4.4.3：prompt_tokens 是总输入，直接用）：
// 命中 = prompt_tokens_details.cached_tokens，写入 = prompt_tokens_details.cache_creation_input_tokens。
// CacheReadMode 按 raw 是否含对应字段标注：含 cache_creation_input_tokens ⇒ 显式缓存通道（explicit）；
// 否则有命中量 ⇒ 隐式自动命中（implicit）；无命中 ⇒ none。
func parseQwenCompat(raw json.RawMessage) (*core.Usage, error) {
	var v qwenCompatUsage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	cached := v.PromptTokensDetails.CachedTokens
	var write int64
	mode := core.CacheModeNone
	if v.PromptTokensDetails.CacheCreationInputTokens != nil {
		write = *v.PromptTokensDetails.CacheCreationInputTokens
		mode = core.CacheModeExplicit
	} else if cached > 0 {
		mode = core.CacheModeImplicit
	}
	return &core.Usage{
		InputTokens:         v.PromptTokens,
		CacheReadTokens:     cached,
		CacheWriteTokens:    write,
		InputTokensUncached: nonNeg(v.PromptTokens - cached - write),
		OutputTokens:        v.CompletionTokens,
		TotalTokens:         v.TotalTokens,
		CacheReadMode:       mode,
	}, nil
}

// minimaxUsage 是 MiniMax OpenAI 形 / 原生 V2 usage（3.2：无缓存命中/写入字段 ⇒ 记 0）。
type minimaxUsage struct {
	PromptTokens            int64 `json:"prompt_tokens"`
	CompletionTokens        int64 `json:"completion_tokens"`
	TotalTokens             int64 `json:"total_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// parseMiniMax 按 MiniMax OpenAI 形口径归一（3.2 MiniMax 行）：
// 无缓存命中/写入字段 ⇒ CacheReadTokens / CacheWriteTokens 记 0；
// completion_tokens_details.reasoning_tokens 单列（计入输出计费由 cost 层按 OutputTokens 处理）；
// total_characters 等随 Raw 保留。
func parseMiniMax(raw json.RawMessage) (*core.Usage, error) {
	var v minimaxUsage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	return &core.Usage{
		InputTokens:         v.PromptTokens,
		InputTokensUncached: v.PromptTokens,
		OutputTokens:        v.CompletionTokens,
		ReasoningTokens:     v.CompletionTokensDetails.ReasoningTokens,
		TotalTokens:         v.TotalTokens,
		CacheReadMode:       core.CacheModeNone,
	}, nil
}

// mimoUsage 是小米 MiMo usage（4.4.2：命中在二级，无写入）。
// ⚠️ 2026-11-14 实测修正：真实样本带 `completion_tokens_details.reasoning_tokens`
// （SDD 4.3.10 字段表本就列了该字段）——现象：实现漏读 ⇒ Reasoning 恒 0；
// 根因：结构体没建该字段；不修会怎样：推理 token 计量缺失，输出侧成本审计对不上账。
type mimoUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// parseMiMo 按 MiMo 口径归一（4.4.2 小米 MiMo 行）：cached_tokens 命中、无写入 ⇒ 写入记 0。
func parseMiMo(raw json.RawMessage) (*core.Usage, error) {
	var v mimoUsage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	cached := v.PromptTokensDetails.CachedTokens
	return &core.Usage{
		InputTokens:         v.PromptTokens,
		CacheReadTokens:     cached,
		InputTokensUncached: nonNeg(v.PromptTokens - cached),
		OutputTokens:        v.CompletionTokens,
		ReasoningTokens:     v.CompletionTokensDetails.ReasoningTokens,
		TotalTokens:         v.TotalTokens,
		CacheReadMode:       core.CacheModeImplicit, // 自动命中
	}, nil
}

// grokChatUsage 是 Grok / xAI Chat 形 usage（3.2 / 4.4.2：缓存命中字段两形态取其一）。
// ⚠️ 2026-11-14 实测修正：xAI 现行文档与真实样本的命中字段是
// `prompt_tokens_details.cached_tokens`，`cached_prompt_text_tokens` 是旧形态——
// 现象：只认旧字段 ⇒ 真实响应命中归 0、缓存成本被算成全价；根因：SDD 4.3.5 只列了旧字段名；
// 为什么不删旧字段：两形态并存于不同 API 版本，取其一即可（⛔ 不累加）。
type grokChatUsage struct {
	PromptTokens           int64 `json:"prompt_tokens"`
	CompletionTokens       int64 `json:"completion_tokens"`
	TotalTokens            int64 `json:"total_tokens"`
	CachedPromptTextTokens int64 `json:"cached_prompt_text_tokens"` // 旧形态
	PromptTokensDetails    struct {
		CachedTokens int64 `json:"cached_tokens"` // 现行形态（真实样本/xAI 文档）
	} `json:"prompt_tokens_details"`
	// ReasoningTokens 按 3.2「reasoning_tokens 单列」取顶层字段。
	ReasoningTokens         int64 `json:"reasoning_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// cost_in_usd_ticks 不入 Usage：自报费用由 cost.FromUSDTicks 处理（5.6 ①），
	// 原始值随 Raw 保留。
}

// parseGrokChat 按 Grok Chat 口径归一（3.2 xAI/Grok（Chat）行）：
// 命中 = cached_prompt_text_tokens，写入无独立字段 ⇒ 0；
// reasoning_tokens 顶层与二级同值取其一 ⛔ 不累加（兼容两种上报形态）。
func parseGrokChat(raw json.RawMessage) (*core.Usage, error) {
	var v grokChatUsage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	reasoning := v.ReasoningTokens
	if reasoning == 0 {
		reasoning = v.CompletionTokensDetails.ReasoningTokens
	}
	cached := v.CachedPromptTextTokens
	if cached == 0 {
		// 现行形态（prompt_tokens_details.cached_tokens）兜底，两形态取其一 ⛔ 不累加。
		cached = v.PromptTokensDetails.CachedTokens
	}
	return &core.Usage{
		InputTokens:         v.PromptTokens,
		CacheReadTokens:     cached,
		InputTokensUncached: nonNeg(v.PromptTokens - cached),
		OutputTokens:        v.CompletionTokens,
		ReasoningTokens:     reasoning,
		TotalTokens:         v.TotalTokens,
		CacheReadMode:       core.CacheModeImplicit, // 自动命中
	}, nil
}

// geminiUsage 是 Gemini 原生 usageMetadata（3.2 / 4.4.2：camelCase，缓存字段平级）。
type geminiUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
}

// parseGeminiNative 按 Gemini 原生口径归一（3.2 Gemini 行 / 逐家要点 1）：
// InputTokens = promptTokenCount + toolUsePromptTokenCount；
// OutputTokens = candidatesTokenCount + thoughtsTokenCount（thoughts 是独立加项，
// ⛔ 不含在 candidatesTokenCount 内、⛔ 不重复加）；
// cachedContentTokenCount ⇒ CacheRead，缓存写 0（显式缓存按存储费计，不按写入 token 计）；
// ToolUsePromptTokens / TotalTokens 保留原报（TotalTokens ⛔ 不重算）。
func parseGeminiNative(raw json.RawMessage) (*core.Usage, error) {
	var v geminiUsage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	input := v.PromptTokenCount + v.ToolUsePromptTokenCount
	cached := v.CachedContentTokenCount
	return &core.Usage{
		InputTokens:         input,
		CacheReadTokens:     cached,
		InputTokensUncached: nonNeg(input - cached),
		OutputTokens:        v.CandidatesTokenCount + v.ThoughtsTokenCount,
		TotalTokens:         v.TotalTokenCount,
		ToolUsePromptTokens: v.ToolUsePromptTokenCount,
		CacheReadMode:       core.CacheModeExplicit, // cachedContent 显式缓存
	}, nil
}

// ollamaV1Usage 是 Ollama /v1 兼容层 usage（4.4.2：OpenAI 形、无缓存明细）。
type ollamaV1Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// parseOllamaV1 按 Ollama /v1 口径归一（4.4.2 Ollama /v1 行）：无缓存明细 ⇒ 缓存读写记 0。
func parseOllamaV1(raw json.RawMessage) (*core.Usage, error) {
	var v ollamaV1Usage
	if err := decode(raw, &v); err != nil {
		return nil, err
	}
	return &core.Usage{
		InputTokens:         v.PromptTokens,
		InputTokensUncached: v.PromptTokens,
		OutputTokens:        v.CompletionTokens,
		TotalTokens:         v.TotalTokens,
		CacheReadMode:       core.CacheModeNone,
	}, nil
}

// ---- 公共小工具 ----

// decode 校验并解析原始 usage JSON：空 / null / 非法 JSON / 非对象都算解析失败。
func decode(raw json.RawMessage, v any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("usage: 原始 usage 为空")
	}
	if trimmed[0] != '{' {
		return fmt.Errorf("usage: 原始 usage 不是 JSON 对象")
	}
	if err := json.Unmarshal(trimmed, v); err != nil {
		return fmt.Errorf("usage: 解析原始 usage 失败: %w", err)
	}
	return nil
}

// uncachedOpenAI 按 R1 公式（4.4.3）：
// input_tokens_uncached = prompt_tokens − cached_tokens − cache_write_tokens。
// 结果 ⛔ 不产生负数（3.2 流中断口径）。
func uncachedOpenAI(prompt, cached, write int64) int64 {
	return nonNeg(prompt - cached - write)
}

// nonNeg 把负数截为 0（3.2：⛔ 不产生负数 token）。
func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// copyRaw 逐字复制原始 usage JSON（归一化结果与入参解耦，原文保留供账单核对）。
func copyRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out
}

// finalize 做归一化收尾：无缓存命中量时缓存模式置 none；全字段截负（⛔ 不产生负数）。
func finalize(u *core.Usage) {
	u.InputTokens = nonNeg(u.InputTokens)
	u.CacheReadTokens = nonNeg(u.CacheReadTokens)
	u.CacheWriteTokens = nonNeg(u.CacheWriteTokens)
	u.InputTokensUncached = nonNeg(u.InputTokensUncached)
	u.OutputTokens = nonNeg(u.OutputTokens)
	u.ReasoningTokens = nonNeg(u.ReasoningTokens)
	u.TotalTokens = nonNeg(u.TotalTokens)
	u.ToolUsePromptTokens = nonNeg(u.ToolUsePromptTokens)
	if u.CacheReadTokens == 0 {
		u.CacheReadMode = core.CacheModeNone
	}
}
