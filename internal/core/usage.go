// 日期: 2026-11-14 11:33:06.951
// 作者: Zhai Wenjia

package core

import "encoding/json"

// Usage 是统一 token 用量结构（统一 SDD 3.2，逐字段定义）。
// 三项必准字段：InputTokens / CacheReadTokens / OutputTokens；
// 缓存写入单列不累加；流中断时 Estimated=true 且不产生负数。
type Usage struct {
	// ---- 三项必准字段 ----

	// InputTokens 是**总输入** token。Anthropic 系必须合成：
	// input_tokens + cache_creation_input_tokens + cache_read_input_tokens。
	InputTokens int64
	// CacheReadTokens 是输入缓存命中（从缓存读取），与 CacheWriteTokens 分列。
	CacheReadTokens int64
	// OutputTokens 是输出 token。
	OutputTokens int64

	// ---- 补充维度（必需，否则成本算不准）----

	// CacheWriteTokens 是写入缓存（Qwen / Kimi / Claude / OpenAI 都有）。
	CacheWriteTokens int64
	// InputTokensUncached 是未缓存的新增输入。
	// 对 Anthropic 系 = input_tokens；对 OpenAI 系 = prompt_tokens − cached − cache_write。
	InputTokensUncached int64
	// ReasoningTokens 是推理 token（计入输出计费）。
	ReasoningTokens int64
	// TotalTokens 是 provider 原报的总值（保留，⛔ 不重算）。
	TotalTokens int64
	// ToolUsePromptTokens 是 Gemini 的 tool_use_prompt_token_count。
	ToolUsePromptTokens int64

	// ---- 可信度标记 ----

	// Estimated = true 表示 usage 缺失 / 被截断、用量为估算值。
	// 流中断 / 取消时置 true，⛔ 不按 0 计、⛔ 不产生负数 token。
	Estimated bool

	// CacheReadMode 标记缓存命中是显式还是隐式档（统一 SDD 5.7「缓存命中档位」）：
	// 费用计算按档取 CacheReadExplicitCNY / CacheReadImplicitCNY，由适配器按
	// 该家缓存形态标记；单一命中价的供应商两档同值。
	CacheReadMode string

	// ---- 原始数据 ----

	// Raw 保留 provider 原始 usage JSON（账单核对 / 协议排障）。
	Raw json.RawMessage
}

// CacheReadMode 取值（统一 SDD 5.7）。
const (
	CacheModeNone     = ""         // 无缓存命中
	CacheModeExplicit = "explicit" // 显式缓存（Anthropic cache_control / Qwen 显式 / OpenAI）
	CacheModeImplicit = "implicit" // 隐式缓存（Qwen 隐式 / DeepSeek / 智谱 / Kimi 自动命中）
)

// NormalizedTokens 返回限流 / 预算的计数口径（统一 SDD 7.2）：
// normalized = InputTokens + OutputTokens（缓存命中与写入均计入 InputTokens，不打折）。
func (u *Usage) NormalizedTokens() int64 {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.OutputTokens
}
