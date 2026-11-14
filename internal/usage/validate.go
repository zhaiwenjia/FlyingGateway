// 日期: 2026-11-14 11:54:49.168
// 作者: Zhai Wenjia

package usage

import (
	"errors"
	"fmt"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// Validate 校验归一化用量（统一 SDD 3.2 硬规则）：
//  1. 无负数 token —— 三类必准字段与补充维度都不得为负；
//  2. Estimated 语义检查 —— Estimated=false（声称是 provider 原报实测值）时
//     Raw 必须非空（有原始 usage 佐证），否则视为口径失真；Estimated=true（估算值）时
//     允许 Raw 为空（流中断末帧 usage 没拿到）。
func Validate(u *core.Usage) error {
	if u == nil {
		return errors.New("usage: Usage 为 nil")
	}
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"InputTokens", u.InputTokens},
		{"CacheReadTokens", u.CacheReadTokens},
		{"CacheWriteTokens", u.CacheWriteTokens},
		{"InputTokensUncached", u.InputTokensUncached},
		{"OutputTokens", u.OutputTokens},
		{"ReasoningTokens", u.ReasoningTokens},
		{"TotalTokens", u.TotalTokens},
		{"ToolUsePromptTokens", u.ToolUsePromptTokens},
	} {
		if f.v < 0 {
			return fmt.Errorf("usage: %s 为负数（%d），⛔ 不产生负数 token", f.name, f.v)
		}
	}
	if !u.Estimated && len(u.Raw) == 0 {
		return errors.New("usage: Estimated=false 但 Raw 为空——声称实测值必须保留原始 usage 原文")
	}
	return nil
}
