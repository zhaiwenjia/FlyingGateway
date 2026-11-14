// 日期: 2026-11-14 16:08:29.843
// 作者: Zhai Wenjia

package server

import (
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// TestFrameAccumulatorThinkingSignature 钉住帧累加器把 thinking 签名落进
// core.OutputItem.ThinkingSignature（流式非流式同一出口）：签名帧
// （FrameReasoningDelta 且 ThinkingSignature 非空，承载约定见 stream.FrameReasoningDelta）
// 收口一个 thinking 块 ⇒ 两个签名块产出两个 reasoning item、各带各的签名与正文。
// 错误实现会红：签名被丢（帧抽象无槽位时的旧行为）⇒ ThinkingSignature 非空断言红；
// 两块并成一块（只留末签名 / 丢首签名）⇒ 逐块签名断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/server/ -run TestFrameAccumulatorThinkingSignature -v
func TestFrameAccumulatorThinkingSignature(t *testing.T) {
	acc := newFrameAccumulator()
	acc.add(stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: "先想"})
	acc.add(stream.Frame{Kind: stream.FrameReasoningDelta, ThinkingSignature: "sig-1"})
	acc.add(stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: "再想"})
	acc.add(stream.Frame{Kind: stream.FrameReasoningDelta, ThinkingSignature: "sig-2"})
	acc.add(stream.Frame{Kind: stream.FrameTextDelta, Text: "答"})

	resp := acc.result("m")
	var reasoning []core.OutputItem
	for _, it := range resp.OutputItems {
		if it.Type == "reasoning" {
			reasoning = append(reasoning, it)
		}
	}
	if len(reasoning) != 2 {
		t.Fatalf("reasoning item 应有 2 个（每 thinking 块一个，流式非流式同形），实得 %d：%+v",
			len(reasoning), resp.OutputItems)
	}
	if reasoning[0].ReasoningText != "先想" || reasoning[0].ThinkingSignature != "sig-1" {
		t.Errorf("thinking 块1 错（正文+签名逐块落）: %+v", reasoning[0])
	}
	if reasoning[1].ReasoningText != "再想" || reasoning[1].ThinkingSignature != "sig-2" {
		t.Errorf("thinking 块2 错（正文+签名逐块落）: %+v", reasoning[1])
	}
}
