// 日期: 2026-11-15 00:04:25.586
// 作者: Zhai Wenjia

package messages

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// idCounter 是响应 id 单调计数（跨编码器实例全局递增）。
var idCounter atomic.Int64

// timeNowUnixNano 便于测试替换的时钟缝。
var timeNowUnixNano = func() int64 { return time.Now().UnixNano() }

// EventEncoder 把内部帧编码为 Anthropic 事件流（统一 SDD 3.7 客户端帧编码器第三实现）：
//
//	message_start → content_block_start / content_block_delta / content_block_stop ×N
//	→ message_delta（stop_reason + 累计 usage）→ message_stop
//
// 块规则：text / thinking 串行各占一段；签名帧（ThinkingSignature 非空且正文为空）一帧
// 收口一个 thinking 块；tool_use 每个 ToolCallID 一块（增量按 index 归位）。
// x_gateway.degradations 挂在 message_delta 扩展字段（与 chat/responses 终止帧同位）。
type EventEncoder struct {
	mu       sync.Mutex
	model    string
	id       string
	started  bool
	finished bool

	openKind  string // 当前文本类块类型（"" = 无）
	openIndex int
	nextIndex int            // 块序号分发
	toolIndex map[string]int // ToolCallID → 块序号
	toolOpen  map[int]bool   // 工具块是否已 start 且未 stop

	usage  *usageOut
	finish string
	degras []degradationOut
}

// NewEventEncoder 构造编码器；id 在此定下（单调生成）。
func NewEventEncoder(model string) *EventEncoder {
	return &EventEncoder{
		model:     model,
		id:        nextMessageID(),
		toolIndex: make(map[string]int),
		toolOpen:  make(map[int]bool),
	}
}

// AddDegradation 登记不可逆语义丢失标记，随 message_delta 的 x_gateway 输出。
func (e *EventEncoder) AddDegradation(d core.Degradation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.degras = append(e.degras, degradationOut{Field: d.Field, Reason: d.Reason, Detail: d.Detail})
}

// Encode 把单个内部帧编码为 0..N 条 SSE 事件。
// FrameUsage 只记录（随 message_delta 输出）；FrameDone 收口整条消息。
func (e *EventEncoder) Encode(f stream.Frame) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return nil, errors.New("messages: EventEncoder 已 Finish，不能再 Encode")
	}
	var buf []byte
	var err error
	e.ensureStart(&buf)
	switch f.Kind {
	case stream.FrameTextDelta:
		e.openTextLike(&buf, "text")
		e.delta(&buf, map[string]any{"type": "text_delta", "text": f.Text})
	case stream.FrameReasoningDelta:
		if f.ThinkingSignature != "" {
			// 签名帧：签名增量后一帧收口一块（流式签名承载约定，⛔ 不压平）。
			e.openTextLike(&buf, "thinking")
			e.delta(&buf, map[string]any{"type": "signature_delta", "signature": f.ThinkingSignature})
			e.closeTextLike(&buf)
			return buf, nil
		}
		if f.Reasoning == "" {
			return buf, nil
		}
		e.openTextLike(&buf, "thinking")
		e.delta(&buf, map[string]any{"type": "thinking_delta", "thinking": f.Reasoning})
	case stream.FrameToolCallArgsDelta:
		idx := e.toolSlot(f.ToolCallID)
		if !e.toolOpen[idx] {
			e.closeTextLike(&buf)
			e.toolOpen[idx] = true
			name := f.Text // 工具名随首段增量的文本载体（Frame 契约同 chat 编码器）
			e.emit(&buf, "content_block_start", map[string]any{
				"type": "content_block_start", "index": idx,
				"content_block": map[string]any{
					"type": "tool_use", "id": f.ToolCallID, "name": name, "input": map[string]any{},
				},
			})
		}
		if f.Arguments != "" {
			e.emit(&buf, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": idx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": f.Arguments},
			})
		}
	case stream.FrameUsage:
		if f.Usage != nil {
			u := usageOf(f.Usage)
			e.usage = &u
		}
	case stream.FrameDone:
		if f.Status != "" {
			e.finish = f.Status
		}
		e.closeAll(&buf)
		e.finishMessage(&buf)
		return buf, err
	case stream.FrameError:
		e.emit(&buf, "error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": f.Status, "message": f.Text},
		})
	}
	return buf, nil
}

// Finish 收口整条消息（取消 / 提前结束路径的兜底终止；幂等）。
func (e *EventEncoder) Finish() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return nil, nil
	}
	var buf []byte
	e.ensureStart(&buf)
	e.closeAll(&buf)
	e.finishMessage(&buf)
	return buf, nil
}

// ensureStart 发 message_start（首次 Encode / Finish 前，调用方已持 e.mu）。
func (e *EventEncoder) ensureStart(buf *[]byte) {
	if e.started {
		return
	}
	e.started = true
	e.emit(buf, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": e.id, "type": "message", "role": "assistant", "model": e.model,
			"content": []any{}, "usage": usageOut{},
		},
	})
}

// openTextLike 打开 text / thinking 块（同类复用，异类先收口；调用方已持 e.mu）。
func (e *EventEncoder) openTextLike(buf *[]byte, kind string) {
	if e.openKind == kind {
		return
	}
	e.closeTextLike(buf)
	e.openIndex = e.nextIndex
	e.nextIndex++
	e.openKind = kind
	block := map[string]any{"type": "text", "text": ""}
	if kind == "thinking" {
		block = map[string]any{"type": "thinking", "thinking": ""}
	}
	e.emit(buf, "content_block_start", map[string]any{
		"type": "content_block_start", "index": e.openIndex, "content_block": block,
	})
}

// closeTextLike 收口当前文本类块（调用方已持 e.mu）。
func (e *EventEncoder) closeTextLike(buf *[]byte) {
	if e.openKind == "" {
		return
	}
	e.emit(buf, "content_block_stop", map[string]any{"type": "content_block_stop", "index": e.openIndex})
	e.openKind = ""
}

// toolSlot 取工具调用块序号（每个 ToolCallID 首见分配一次）。
func (e *EventEncoder) toolSlot(id string) int {
	if idx, ok := e.toolIndex[id]; ok {
		return idx
	}
	idx := e.nextIndex
	e.nextIndex++
	e.toolIndex[id] = idx
	return idx
}

// closeAll 收口全部未收块（文本类 + 工具类，按序号升序；调用方已持 e.mu）。
func (e *EventEncoder) closeAll(buf *[]byte) {
	e.closeTextLike(buf)
	open := make([]int, 0, len(e.toolOpen))
	for idx, isOpen := range e.toolOpen {
		if isOpen {
			open = append(open, idx)
		}
	}
	for i := 0; i < len(open); i++ {
		for j := i + 1; j < len(open); j++ {
			if open[j] < open[i] {
				open[i], open[j] = open[j], open[i]
			}
		}
	}
	for _, idx := range open {
		e.emit(buf, "content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		e.toolOpen[idx] = false
	}
}

// finishMessage 发 message_delta + message_stop（调用方已持 e.mu）。
func (e *EventEncoder) finishMessage(buf *[]byte) {
	e.finished = true
	payload := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReasonOf(e.finish), "stop_sequence": nil},
		"usage": usageOut{},
	}
	if e.usage != nil {
		payload["usage"] = *e.usage
	}
	if len(e.degras) > 0 {
		payload["x_gateway"] = xGatewayOut{Degradations: e.degras}
	}
	e.emit(buf, "message_delta", payload)
	e.emit(buf, "message_stop", map[string]any{"type": "message_stop"})
}

// delta 发内容增量到当前文本类块。
func (e *EventEncoder) delta(buf *[]byte, d map[string]any) {
	e.emit(buf, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": e.openIndex, "delta": d,
	})
}

// emit 编码一条 SSE 事件（event: 名 + data: JSON + 空行）。
func (e *EventEncoder) emit(buf *[]byte, name string, payload any) {
	blob, err := json.Marshal(payload)
	if err != nil {
		*buf = append(*buf, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"encode failed\"}}\n\n"...)
		return
	}
	*buf = append(*buf, "event: "+name+"\ndata: "+string(blob)+"\n\n"...)
}
