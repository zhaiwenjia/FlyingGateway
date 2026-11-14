// 日期: 2026-11-14 11:54:49.170
// 作者: Zhai Wenjia

package usage

import (
	"encoding/json"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// TestEstimatedFromNil 钉住中断无 usage 口径（11.1「中断无 usage」）：
// 流在末帧前断开 ⇒ Estimated=true，且 CacheRead / Output 不为负（全 0、非负）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestEstimatedFromNil -v
func TestEstimatedFromNil(t *testing.T) {
	u := EstimatedFrom(nil)
	if u == nil {
		t.Fatal("EstimatedFrom(nil) 返回 nil，应返回保守结果")
	}
	if !u.Estimated {
		t.Fatal("Estimated 应为 true（usage 缺失 ⇒ 估算值，⛔ 不按 0 计）")
	}
	if u.CacheReadTokens < 0 || u.OutputTokens < 0 || u.InputTokens < 0 || u.CacheWriteTokens < 0 {
		t.Fatalf("⛔ 不产生负数：Read=%d Output=%d Input=%d Write=%d",
			u.CacheReadTokens, u.OutputTokens, u.InputTokens, u.CacheWriteTokens)
	}
}

// TestEstimatedFromPartialNonNegative 钉住截断 partial 口径：保留已知计数、负值截 0、Estimated=true、
// Raw 原文随迁（11.1「中断无 usage」：Estimated=true 且所有字段非负）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestEstimatedFromPartialNonNegative -v
func TestEstimatedFromPartialNonNegative(t *testing.T) {
	partial := &core.Usage{
		InputTokens:         100,
		CacheReadTokens:     -5, // 协议异常负值：截 0
		OutputTokens:        -1,
		InputTokensUncached: 90,
		Raw:                 json.RawMessage(`{"prompt_tokens":100}`),
		CacheReadMode:       core.CacheModeExplicit,
	}
	u := EstimatedFrom(partial)
	if !u.Estimated {
		t.Fatal("Estimated 应为 true（usage 被截断 ⇒ 估算值）")
	}
	if u.InputTokens != 100 || u.InputTokensUncached != 90 {
		t.Fatalf("已知计数应保留：Input=%d(100) Uncached=%d(90)", u.InputTokens, u.InputTokensUncached)
	}
	if u.CacheReadTokens != 0 || u.OutputTokens != 0 {
		t.Fatalf("负值应截 0：Read=%d(0) Output=%d(0)", u.CacheReadTokens, u.OutputTokens)
	}
	if string(u.Raw) != `{"prompt_tokens":100}` {
		t.Fatalf("Raw 应随迁原文：%s", u.Raw)
	}
}
