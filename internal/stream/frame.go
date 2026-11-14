// 日期: 2026-11-14 11:33:06.951
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

// Frame 是供应商无关的内部帧（统一 SDD 3.7，逐字段）。
type Frame struct {
	Kind       FrameKind
	Text       string
	Reasoning  string
	ToolCallID string
	Arguments  string
	Usage      *core.Usage
	Status     string
	Seq        int64
}

// FrameIterator 是流式帧迭代器（统一 SDD 3.6 适配器接口的返回形态）。
// Next 返回 false 后，Err 返回终态错误（nil 表示正常结束）。
type FrameIterator interface {
	Next() bool
	Frame() Frame
	Err() error
}
