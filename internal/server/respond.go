// 日期: 2026-11-14 14:54:17.768
// 作者: Zhai Wenjia

package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/codec/chat"
	"github.com/zhaiwenjia/FlyingGateway/internal/codec/responses"
	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/cost"
	"github.com/zhaiwenjia/FlyingGateway/internal/security"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// 本文件承载客户端侧响应装配与收口（统一 SDD 3.4 出站编码 / 3.7 终止帧 / 5.6 计费）：
// 帧累加器（非流式装配）+ 客户端帧编码器（流式转发）+ 请求级 finally 记账。

// clientEncoder 是客户端侧帧编码器（统一 SDD 3.7「两个帧编码器」）：
// chat ⇒ chat.completion.chunk SSE；responses ⇒ response.* 事件流。
type clientEncoder interface {
	Encode(f stream.Frame) ([]byte, error)
	Finish() ([]byte, error)
	AddDegradation(d core.Degradation)
}

// newClientEncoder 按入站模式选客户端帧编码器。
func newClientEncoder(mode inboundMode, model string) clientEncoder {
	if mode == modeResponses {
		return responses.NewEventEncoder(model)
	}
	return chat.NewChunkEncoder(model)
}

// frameAccumulator 把上游帧装配成规范 Response（非流式返回 / 取消路径估算用）。
type frameAccumulator struct {
	text      strings.Builder
	reasoning []string
	calls     map[string]*core.OutputItem
	callOrder []string
	usage     *core.Usage
	finish    string
}

func newFrameAccumulator() *frameAccumulator {
	return &frameAccumulator{calls: make(map[string]*core.OutputItem)}
}

// add 累加一帧（FrameUsage / FrameDone 携带 usage 与终态；生命周期帧无客户端语义忽略）。
func (a *frameAccumulator) add(f stream.Frame) {
	switch f.Kind {
	case stream.FrameTextDelta:
		a.text.WriteString(f.Text)
	case stream.FrameReasoningDelta:
		a.reasoning = append(a.reasoning, f.Reasoning)
	case stream.FrameToolCallArgsDelta:
		key := f.ToolCallID
		if key == "" {
			key = fmt.Sprintf("_auto_%d", len(a.callOrder))
		}
		it, ok := a.calls[key]
		if !ok {
			it = &core.OutputItem{Type: "function_call", ID: f.ToolCallID, CallID: f.ToolCallID}
			a.calls[key] = it
			a.callOrder = append(a.callOrder, key)
		}
		if f.Text != "" {
			it.Name = f.Text
		}
		it.Arguments += f.Arguments
	case stream.FrameUsage:
		if f.Usage != nil {
			a.usage = f.Usage
		}
	case stream.FrameDone:
		if f.Status != "" {
			a.finish = f.Status
		}
		if f.Usage != nil {
			a.usage = f.Usage
		}
	}
}

// result 装配规范 Response（model 用客户端别名 —— 响应体面向调用方）。
func (a *frameAccumulator) result(model string) *core.Response {
	resp := &core.Response{Model: model, FinishReason: a.finish, Usage: a.usage}
	if a.text.Len() > 0 {
		resp.Text = a.text.String()
		resp.OutputItems = append(resp.OutputItems, core.OutputItem{
			Type:    "message",
			Role:    "assistant",
			Content: []core.ContentPart{{Type: "output_text", Text: resp.Text}},
		})
	}
	if len(a.reasoning) > 0 {
		resp.OutputItems = append(resp.OutputItems, core.OutputItem{
			Type:          "reasoning",
			ReasoningText: strings.Join(a.reasoning, ""),
		})
	}
	for _, key := range a.callOrder {
		resp.OutputItems = append(resp.OutputItems, *a.calls[key])
	}
	return resp
}

// ---- 流式写入（SSE；head 惰性写，首帧前失败仍可回退到 JSON 错误响应）----

// writeStreamHead 写 SSE 响应头（统一 SDD 4.3.1 流式形态）。
func writeStreamHead(w http.ResponseWriter, mode inboundMode) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flush(w)
}

// writeStreamBytes 写一段 SSE 字节；head 未写则先写（惰性，保证首帧前可回退 JSON 错误）。
func writeStreamBytes(w http.ResponseWriter, b []byte, headWritten *bool, mode inboundMode) error {
	if !*headWritten {
		writeStreamHead(w, mode)
		*headWritten = true
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	flush(w)
	return nil
}

// flush 刷新响应缓冲（客户端断连时的写失败是断连第三信号）。
func flush(w http.ResponseWriter) {
	_ = http.NewResponseController(w).Flush()
}

// ---- 收口写入 ----

// writeOutcome 写终态响应：非流式 ⇒ EncodeResponse 一次返回；流式 ⇒ 终止帧已在
// runEndpoint 收口（⛔ 不重复写）。
func (s *Server) writeOutcome(w http.ResponseWriter, r *http.Request, h *execHandle, req *core.Request, mode inboundMode, resp *core.Response, degs []core.Degradation, cancelled bool) {
	if req.Stream {
		return // 流式终态由 runEndpoint 的 Finish 收口
	}
	if resp == nil {
		resp = &core.Response{Model: req.Model}
	}
	if resp.Model == "" {
		resp.Model = req.Model
	}
	if resp.ID == "" {
		resp.ID = responseID(mode, h.id)
	}
	resp.Degradations = degs
	if cancelled {
		if resp.Usage == nil {
			resp.Usage = usage.EstimatedFrom(nil)
		}
		resp.Status = core.StatusCancelled
		resp.FinishReason = core.FinishAborted
	}
	var (
		body []byte
		err  error
	)
	if mode == modeResponses {
		body, err = responses.EncodeResponse(resp)
	} else {
		body, err = chat.EncodeResponse(resp)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, string(core.ErrUpstreamError), "encode_failed", security.Redact(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeFinalError 写终态错误：流已提交 ⇒ ⛔ 不再写（传输层已收口）；未提交 ⇒ JSON 错误。
func (s *Server) writeFinalError(w http.ResponseWriter, h *execHandle, detail *core.ErrorDetail) {
	if h.committed() {
		return
	}
	s.writeDetail(w, detail)
}

// writeDetail 写错误响应（归一化分类对外、原始上游信息进 span / 日志，统一 SDD 7.1）。
func (s *Server) writeDetail(w http.ResponseWriter, detail *core.ErrorDetail) {
	if detail == nil {
		writeError(w, http.StatusInternalServerError, string(core.ErrUpstreamError), "internal", "未知错误")
		return
	}
	status := detail.HTTPStatus
	if status == 0 {
		status = http.StatusInternalServerError
	}
	writeError(w, status, string(detail.Category), detail.Code, detail.Message)
}

// responseID 生成响应 id（chat / responses 两模式前缀区分）。
func responseID(mode inboundMode, clientID string) string {
	if mode == modeResponses {
		return "resp-" + clientID
	}
	return "chatcmpl-" + clientID
}

// ---- 请求级 finally 记账（统一 SDD 6.4 指标 / 7.2 计费边界 / 5.6 费用）----

// finalizeExec 是请求级收口（清理第二道闸，与帧循环 finally 独立、幂等）：
// 上游收口兜底、取消计数与延迟指标、预算记账（计费至取消点 ⛔ 不清零，按调用方口径）、
// 费用落日志、注册表收口。
func (s *Server) finalizeExec(h *execHandle, req *core.Request, caller string, startTime time.Time) {
	h.markUpstreamClosed() // 请求级兜底闸（帧循环闸已收口时幂等无事）
	snap := h.snapshot()
	if snap.CancelReason != "" {
		s.metrics.ObserveCancel(snap.CancelReason)
		if snap.DetectedLatencyMs > 0 {
			s.metrics.ObserveCancelLatency(snap.DetectedLatencyMs)
		}
	}
	if u := h.finalUsage(); u != nil {
		if s.budget != nil {
			// 计费边界：计费至取消点，取消的执行照记已生成用量（统一 SDD 7.2）。
			// 预算按**调用方**累计（调用方 key 与上游令牌解耦后的计费口径）。
			s.budget.Record(caller, u.NormalizedTokens())
		}
		if s.rates != nil {
			rate := s.rates.Lookup(h.upstreamModelName())
			c := cost.CalculateCost(u, rate, startTime)
			s.logger.Info("upstream 执行计费",
				"client_request_id", h.id,
				"upstream_model", h.upstreamModelName(),
				"cost_cny", c.CostCNY,
				"cost_source", string(c.Source),
				"usage_estimated", u.Estimated,
			)
		}
	}
	s.cancels.finish(h)
}
