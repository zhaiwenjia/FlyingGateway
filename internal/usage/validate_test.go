// 日期: 2026-11-14 11:54:49.171
// 作者: Zhai Wenjia

package usage

import (
	"encoding/json"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// TestValidateRejectsNegative 钉住「无负数 token」校验（任务书 Validate 口径）：
// 任一 token 字段为负 ⇒ 报错；错误实现（不检查）会恒过 ⇒ 本断言在负值样本上必红（恒假样本已核）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestValidateRejectsNegative -v
func TestValidateRejectsNegative(t *testing.T) {
	if err := Validate(nil); err == nil {
		t.Fatal("Validate(nil) 应报错")
	}
	base := core.Usage{Estimated: true}
	for _, mut := range []struct {
		name string
		f    func(*core.Usage)
	}{
		{"InputTokens", func(u *core.Usage) { u.InputTokens = -1 }},
		{"CacheReadTokens", func(u *core.Usage) { u.CacheReadTokens = -1 }},
		{"CacheWriteTokens", func(u *core.Usage) { u.CacheWriteTokens = -1 }},
		{"InputTokensUncached", func(u *core.Usage) { u.InputTokensUncached = -1 }},
		{"OutputTokens", func(u *core.Usage) { u.OutputTokens = -1 }},
		{"ReasoningTokens", func(u *core.Usage) { u.ReasoningTokens = -1 }},
		{"TotalTokens", func(u *core.Usage) { u.TotalTokens = -1 }},
		{"ToolUsePromptTokens", func(u *core.Usage) { u.ToolUsePromptTokens = -1 }},
	} {
		u := base
		mut.f(&u)
		if err := Validate(&u); err == nil {
			t.Fatalf("%s 为负数时 Validate 应报错", mut.name)
		}
	}
	if err := Validate(&base); err != nil {
		t.Fatalf("全 0 非负 usage 应通过：%v", err)
	}
}

// TestValidateEstimatedSemantics 钉住 Estimated 语义检查（任务书 Validate 口径）：
// Estimated=false（声称实测值）必须带 Raw 原文佐证，否则报错；Estimated=true（估算值）允许 Raw 为空。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestValidateEstimatedSemantics -v
func TestValidateEstimatedSemantics(t *testing.T) {
	// 声称实测但无原文 ⇒ 报错（错误实现只查负数会恒过 ⇒ 本断言必红）。
	bad := core.Usage{InputTokens: 10, Estimated: false}
	if err := Validate(&bad); err == nil {
		t.Fatal("Estimated=false 且 Raw 为空应报错（声称实测值必须保留原文）")
	}

	// 声称实测且带原文 ⇒ 通过。
	good := core.Usage{InputTokens: 10, Estimated: false, Raw: json.RawMessage(`{"prompt_tokens":10}`)}
	if err := Validate(&good); err != nil {
		t.Fatalf("Estimated=false 且 Raw 非空应通过：%v", err)
	}

	// 估算值允许无原文（流中断末帧 usage 没拿到）。
	est := core.Usage{Estimated: true}
	if err := Validate(&est); err != nil {
		t.Fatalf("Estimated=true 且 Raw 为空应通过：%v", err)
	}
}
