// 日期: 2026-11-14 16:08:29.664
// 作者: Zhai Wenjia

// Package stream 承载供应商无关的统一帧抽象与流解析核心（统一 SDD 3.7）：
// 一套流解析核心（SSE / NDJSON）+ 每家一个「帧解码器」（adapter 层）
// + 两个「帧编码器」（codec/chat、codec/responses，客户端侧）。
package stream

import "github.com/zhaiwenjia/FlyingGateway/internal/core"

// FrameKind 是统一帧类型（统一 SDD 3.7）。
type FrameKind int

const (
	// FrameTextDelta 是文本增量。
	FrameTextDelta FrameKind = iota
	// FrameReasoningDelta 是推理文本增量。
	//
	// 签名帧承载约定：thinking 块签名复用本帧类型 —— ThinkingSignature 非空且
	// Reasoning 为空的帧即「签名帧」（Anthropic content_block_delta 的
	// signature_delta ⇒ delta.signature，⛔ 不可压平），一帧收口一个 thinking 块；
	// 消费端以 ThinkingSignature != "" 区分签名帧与推理文本帧。
	FrameReasoningDelta
	// FrameToolCallArgsDelta 是工具调用参数增量。
	FrameToolCallArgsDelta
	// FrameUsage 是用量帧（流中断时由取消路径补的终止帧含 Estimated=true 的 usage）。
	FrameUsage
	// FrameLifecycle 是生命周期帧（created / in_progress 等）。
	FrameLifecycle
	// FrameDone 是终止帧。
	FrameDone
	// FrameError 是错误帧。
	FrameError
)

// Frame 是供应商无关的内部帧（统一 SDD 3.7，逐字段；ThinkingSignature 为
// 签名打通增补字段，见 FrameReasoningDelta 的签名帧承载约定）。
type Frame struct {
	Kind       FrameKind
	Text       string
	Reasoning  string
	ToolCallID string
	Arguments  string
	Usage      *core.Usage
	Status     string
	Seq        int64
	// ThinkingSignature 承载 thinking 块签名（⛔ 不可压平）：Responses 出口落
	// reasoning item 的 signature（无损）；Chat 出口承载不了 ⇒ x_gateway.degradations[] 标记。
	ThinkingSignature string
}

// FrameIterator 是流式帧迭代器（统一 SDD 3.6 适配器接口的返回形态）。
// Next 返回 false 后，Err 返回终态错误（nil 表示正常结束）。
type FrameIterator interface {
	Next() bool
	Frame() Frame
	Err() error
}
