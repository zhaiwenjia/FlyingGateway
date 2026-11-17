// 日期: 2026-11-17 07:52:00.000
// 作者: Zhai Wenjia

package messages

import (
	"encoding/json"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// TestEncodeResponseTextPartNames 钉住 core 树文本部件双名兼容（2026-11-17 丢正文修复）：
// "text"（标准口径）与 "output_text"（历史/异路径兼容）都必须出 text 块、⛔ 不落丢弃分支。
// 错误实现对照：只认 "text" ⇒ output_text 分支红（messages 非流式丢正文的回归钉）。
func TestEncodeResponseTextPartNames(t *testing.T) {
	for _, partType := range []string{"text", "output_text"} {
		resp := &core.Response{Model: "ma", FinishReason: core.FinishStop,
			Usage: &core.Usage{InputTokens: 1, OutputTokens: 1},
			OutputItems: []core.OutputItem{{
				Type: "message", Role: "assistant",
				Content: []core.ContentPart{{Type: partType, Text: "正文"}},
			}}}
		raw, err := EncodeResponse(resp)
		if err != nil {
			t.Fatalf("EncodeResponse(%s) 失败: %v", partType, err)
		}
		var out struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			XGateway *struct {
				Degradations []map[string]any `json:"degradations"`
			} `json:"x_gateway"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != "正文" {
			t.Fatalf("part=%s ⇒ content=%+v，期望单个 text 块「正文」（⛔ 丢正文）", partType, out.Content)
		}
		if out.XGateway != nil && len(out.XGateway.Degradations) > 0 {
			t.Fatalf("part=%s 不应产生降级标记：%+v", partType, out.XGateway.Degradations)
		}
	}
}
