// 日期: 2026-11-14 12:02:39.231
// 作者: Zhai Wenjia

package routing

import (
	"errors"
	"fmt"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// ErrContextExceeded 是上下文越界的哨兵错误（可 errors.Is 判定）。
var ErrContextExceeded = errors.New("routing: 上下文越界")

// ErrInvalidArgument 是入参非法的哨兵错误（负数 token / 非正 contextWindow 等）。
var ErrInvalidArgument = errors.New("routing: 入参非法")

// ContextExceededError 是上下文越界的类型化错误（统一 SDD 2.3 上下文越界判定表 / 7.4）。
// 上层可按 Category 归为 core.ErrContextExceeded，或 errors.Is(err, ErrContextExceeded)。
type ContextExceededError struct {
	EstimatedInputTokens int
	MaxOutputTokens      int
	ContextWindow        int
}

func (e *ContextExceededError) Error() string {
	return fmt.Sprintf(
		"context_exceeded: 估算输入 %d + max_output_tokens %d > context_window %d（⛔ 不静默截断）",
		e.EstimatedInputTokens, e.MaxOutputTokens, e.ContextWindow)
}

// Unwrap 让 errors.Is 命中 ErrContextExceeded。
func (e *ContextExceededError) Unwrap() error { return ErrContextExceeded }

// Category 返回错误归一化分类（统一 SDD 7.1），供上层映射 core.ErrContextExceeded。
func (e *ContextExceededError) Category() core.ErrorCategory { return core.ErrContextExceeded }

// CheckContext 做上下文越界判定（统一 SDD 2.3 域② / 上下文越界判定表）：
// 估算输入 token + max_output_tokens > context_window ⇒ 越界，返回 *ContextExceededError
// （上层走 context_window 专用降级链，无链 ⇒ 显式报错 400 context_exceeded）。
//
// ⚠️ 估算不确定（各家 tokenizer 不同）⇒ estimatedInputTokens 必须按保守上界给
// （宁可早报错，不可悄悄截断，统一 SDD 2.3）；网关只判定越界、不主动改写历史
// （上下文压缩是客户端职责）。
//
// 边界口径：恰好等于 context_window 不算越界（判定是严格大于）。
// ⛔ 负数 token / 非正 contextWindow 显式报错，不许拿非法值绕过判定。
func CheckContext(estimatedInputTokens, maxOutputTokens, contextWindow int) error {
	if estimatedInputTokens < 0 || maxOutputTokens < 0 {
		return fmt.Errorf("%w: estimatedInputTokens=%d maxOutputTokens=%d 不得为负",
			ErrInvalidArgument, estimatedInputTokens, maxOutputTokens)
	}
	if contextWindow <= 0 {
		return fmt.Errorf("%w: contextWindow=%d 必须为正", ErrInvalidArgument, contextWindow)
	}
	if estimatedInputTokens+maxOutputTokens > contextWindow {
		return &ContextExceededError{
			EstimatedInputTokens: estimatedInputTokens,
			MaxOutputTokens:      maxOutputTokens,
			ContextWindow:        contextWindow,
		}
	}
	return nil
}
