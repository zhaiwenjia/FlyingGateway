// 日期: 2026-11-14 11:54:49.167
// 作者: Zhai Wenjia

package usage

import "github.com/zhaiwenjia/FlyingGateway/internal/core"

// EstimatedFrom 在 usage 缺失 / 被截断（流中断、取消）时产出估算标记的用量（统一 SDD 3.2）：
// Estimated = true，⛔ 不按 0 计（Estimated 标志使下游按估算计费 / 限流，不当免费），
// ⛔ 不产生负数（partial 里的负值一律截为 0）。
// partial 为 nil 表示末帧 usage 完全没拿到：返回全 0 且 Estimated=true 的保守结果。
func EstimatedFrom(partial *core.Usage) *core.Usage {
	out := &core.Usage{Estimated: true}
	if partial == nil {
		return out
	}
	out.InputTokens = nonNeg(partial.InputTokens)
	out.CacheReadTokens = nonNeg(partial.CacheReadTokens)
	out.CacheWriteTokens = nonNeg(partial.CacheWriteTokens)
	out.InputTokensUncached = nonNeg(partial.InputTokensUncached)
	out.OutputTokens = nonNeg(partial.OutputTokens)
	out.ReasoningTokens = nonNeg(partial.ReasoningTokens)
	out.TotalTokens = nonNeg(partial.TotalTokens)
	out.ToolUsePromptTokens = nonNeg(partial.ToolUsePromptTokens)
	out.CacheReadMode = partial.CacheReadMode
	if out.CacheReadTokens == 0 {
		out.CacheReadMode = core.CacheModeNone
	}
	out.Raw = copyRaw(partial.Raw)
	return out
}
