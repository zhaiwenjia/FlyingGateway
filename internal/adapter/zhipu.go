// 日期: 2026-11-14 14:29:38.471
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// 智谱 GLM / BigModel 适配器（统一 SDD 4.3.8）：
// 出站 Responses：POST {base}/responses（4.1.4：智谱 /v1/responses 原生）。
// AuthScheme: Authorization + "Bearer "；previous_response_id 支持 = true（3.5 表）。
// ⚠️ 流式请求参数是 stream_options.chunk_include_usage（⛔ 不是 include_usage，4.3.8 逐字：
// 照搬 include_usage 会静默拿不到 usage）；chunk_include_usage 开启后逐块返回**累计** usage（⛔ 不可相加）。
// finish_reason 含 sensitive / network_error / model_context_window_exceeded（智谱独有 / 非标准取值）
// ⇒ core.FinishReason 超集原值（4.5.1 ⛔ 不得当作未知值丢弃）。
// thinking.type / thinking.clear_thinking 经 core.Request.Extra 透传；reasoning_effort 映射
// core.ReasoningConfig.Effort（4.3.8 取值 max/xhigh/high/medium/low/minimal/none）。

type zhipuAdapter struct {
	hc *http.Client
}

// NewZhipu 构造智谱 GLM 适配器（hc 为出站 HTTP 客户端）。
func NewZhipu(hc *http.Client) Adapter { return &zhipuAdapter{hc: hc} }

func (a *zhipuAdapter) Provider() core.Provider          { return core.ProviderZhipu }
func (a *zhipuAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *zhipuAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 4.3.8：Authorization: Bearer。
func (a *zhipuAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4.2 智谱行：命中单列、无写入、completion 含 reasoning）。
func (a *zhipuAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderZhipu, usageTreeShape(raw), raw)
}

// ---- 请求体（Responses 形状 + 智谱特异字段）----

type zhipuReq struct {
	Model              string           `json:"model"`
	Input              []zhipuInputItem `json:"input"`
	Instructions       string           `json:"instructions,omitempty"`
	Tools              []zhipuTool      `json:"tools,omitempty"`
	ToolChoice         any              `json:"tool_choice,omitempty"`
	MaxOutputTokens    int              `json:"max_output_tokens,omitempty"`
	PreviousResponseID string           `json:"previous_response_id,omitempty"`
	Stream             bool             `json:"stream,omitempty"`
	// ReasoningEffort 是智谱的推理档位字段（4.3.8：max/xhigh/high/medium/low/minimal/none）。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// StreamOptions 仅流式时置位：⚠️ 参数名是 chunk_include_usage（⛔ 不是 include_usage，4.3.8）。
	StreamOptions any `json:"stream_options,omitempty"`
	// thinking.type / thinking.clear_thinking 经 Extra 透传（applyExtra 并入）。
}

type zhipuInputItem struct {
	Type             string           `json:"type"`
	Role             string           `json:"role,omitempty"`
	Content          []zhipuInputPart `json:"content,omitempty"`
	CallID           string           `json:"call_id,omitempty"`
	Output           string           `json:"output,omitempty"`
	ID               string           `json:"id,omitempty"`
	Name             string           `json:"name,omitempty"`
	Arguments        string           `json:"arguments,omitempty"`
	EncryptedContent string           `json:"encrypted_content,omitempty"`
}

type zhipuInputPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type zhipuTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// BuildRequestWithDegradations 构造 /responses 请求并返回降级记录（4.3.8 关键字段逐字）。
// ⚠️ 2026-11-14 修复：tool_choice 非 auto 被降为 auto 原先未记 degradation（静默降级，
// 违反 1.4.2 不静默降级三则之二）——现象：请求带 tool_choice=required 实际发出 auto，
// 响应无任何标记；根因：只实现了降级动作没接降级记录通道（minimax 同类降级有记，可对照）；
// 为什么不改成拒绝请求：4.3.8 明文「tool_choice 仅 auto」，降为 auto 是能力边界内的
// 既有口径，问题只在「丢意图不可见」⇒ 补 DegradationAware 通道（BuildRequestWithDegradations）；
// 不修会怎样：调用方以为 required 生效，工具未被强制调用，排查方向从一开始就跑偏。
func (a *zhipuAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, nil, err
	}
	body := zhipuReq{
		Model:              res.Model,
		Instructions:       r.Instructions,
		MaxOutputTokens:    r.MaxOutputTokens,
		PreviousResponseID: r.PreviousResponseID,
		Stream:             r.Stream,
	}
	if r.Reasoning != nil {
		body.ReasoningEffort = r.Reasoning.Effort
	}
	if r.Stream {
		// ⚠️ chunk_include_usage（⛔ 不是 include_usage）：逐块返回累计 usage（4.3.8）。
		body.StreamOptions = map[string]any{"chunk_include_usage": true}
	}
	for _, m := range r.Messages {
		items, sys := zhipuInputItems(m)
		if sys != "" {
			if body.Instructions == "" {
				body.Instructions = sys
			} else {
				body.Instructions += "\n" + sys
			}
			continue
		}
		body.Input = append(body.Input, items...)
	}
	for _, t := range r.Tools {
		body.Tools = append(body.Tools, zhipuTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	var degs []core.Degradation
	if r.ToolChoice != nil {
		// 4.3.8：tool_choice 仅 auto ⇒ 非 auto 一律降为 auto 并记 degradation（⛔ 不静默）。
		if r.ToolChoice.Mode != "auto" {
			degs = append(degs, core.Degradation{
				Field:  "tool_choice",
				Reason: core.DegradationCapabilityUnsupported,
				Detail: "智谱 GLM 仅支持 tool_choice=auto，已降为 auto",
			})
		}
		body.ToolChoice = "auto"
	}
	raw, err := marshalBody(body)
	if err != nil {
		return nil, nil, err
	}
	// thinking.type / thinking.clear_thinking 等特异参数经 Extra 透传（4.3.8）。
	raw, err = mergeExtraBody(raw, r.Extra)
	if err != nil {
		return nil, nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/responses"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, degs, nil
}

// BuildRequest 委托 BuildRequestWithDegradations（降级记录由上层合并进 x_gateway.degradations[]）。
func (a *zhipuAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// zhipuInputItems 把规范消息拆成 Responses input item（4.2.3）。
func zhipuInputItems(m core.Message) ([]zhipuInputItem, string) {
	if m.Role == "system" {
		return nil, contentText(m.Content)
	}
	if m.Role == "tool" {
		return []zhipuInputItem{{Type: "function_call_output", CallID: m.ToolCallID, Output: contentText(m.Content)}}, ""
	}
	var items []zhipuInputItem
	if m.EncryptedContent != "" || m.ReasoningContent != "" {
		it := zhipuInputItem{Type: "reasoning", EncryptedContent: m.EncryptedContent}
		if m.ReasoningContent != "" {
			it.Content = []zhipuInputPart{{Type: "reasoning_text", Text: m.ReasoningContent}}
		}
		items = append(items, it)
	}
	partType := "input_text"
	if m.Role == "assistant" {
		partType = "output_text"
	}
	var parts []zhipuInputPart
	for _, p := range m.Content {
		switch {
		case p.ImageURL != "":
			parts = append(parts, zhipuInputPart{Type: "input_image", ImageURL: p.ImageURL})
		case p.Text != "":
			parts = append(parts, zhipuInputPart{Type: partType, Text: p.Text})
		}
	}
	if len(parts) > 0 {
		items = append(items, zhipuInputItem{Type: "message", Role: m.Role, Content: parts})
	}
	for _, tc := range m.ToolCalls {
		id := tc.CallID
		if id == "" {
			id = tc.ID
		}
		items = append(items, zhipuInputItem{Type: "function_call", ID: tc.ID, CallID: id, Name: tc.Name, Arguments: tc.Arguments})
	}
	return items, ""
}

// ---- 响应体（Responses 对象 + 智谱 finish_reason 非标准取值）----

type zhipuResp struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Model  string `json:"model"`
	Output []struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		EncryptedContent string `json:"encrypted_content"`
		Summary          []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
		CallID           string `json:"call_id"`
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
		ReasoningContent string `json:"reasoning_content"`
	} `json:"output"`
	OutputText        string `json:"output_text"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage json.RawMessage `json:"usage"`
	// FinishReason 取值含 sensitive / network_error / model_context_window_exceeded（4.5.2 智谱行）
	// ⇒ core.FinishReason 超集原值。
	FinishReason string `json:"finish_reason"`
}

// ParseResponse 解析智谱响应（finish_reason 非标准取值 ⇒ 超集原值，4.5.1）。
func (a *zhipuAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r zhipuResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{ID: r.ID, Model: r.Model, Status: r.Status, Text: r.OutputText}
	hasToolCall := false
	var text string
	for _, it := range r.Output {
		oi := core.OutputItem{Type: it.Type, ID: it.ID, Role: it.Role, EncryptedContent: it.EncryptedContent,
			CallID: it.CallID, Name: it.Name, Arguments: it.Arguments, ReasoningText: it.ReasoningContent}
		for _, p := range it.Content {
			if p.Text != "" {
				oi.Content = append(oi.Content, core.ContentPart{Type: "text", Text: p.Text})
				if it.Type == "message" {
					text += p.Text
				}
			}
		}
		for _, s := range it.Summary {
			oi.Summary = append(oi.Summary, core.SummaryPart{Type: s.Type, Text: s.Text})
		}
		if it.Type == "function_call" {
			hasToolCall = true
		}
		out.OutputItems = append(out.OutputItems, oi)
	}
	if out.Text == "" {
		out.Text = text
	}
	reason := ""
	if r.IncompleteDetails != nil {
		reason = r.IncompleteDetails.Reason
	}
	out.FinishReason = deriveResponsesFinish(r.FinishReason, r.Status, reason, hasToolCall)
	if len(r.Usage) > 0 {
		out.Usage = a.ParseUsage(r.Usage)
	}
	return out, nil
}

// ---- 流式（SSE；chunk_include_usage ⇒ 逐块累计 usage（⛔ 不可相加，取最后一次快照））----

type zhipuStreamEvent struct {
	Type           string     `json:"type"`
	SequenceNumber int64      `json:"sequence_number"`
	Response       *zhipuResp `json:"response"`
	Delta          string     `json:"delta"`
	PartialJSON    string     `json:"partial_json"`
	ItemID         string     `json:"item_id"`
	Item           *struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	} `json:"item"`
}

type zhipuStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[string]string
}

func (a *zhipuAdapter) streamDecoder() *zhipuStreamDecoder {
	return &zhipuStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[string]string{}}
}

func (d *zhipuStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if data == "[DONE]" {
		// 4.3.8：data: [DONE] 结束。
		return nil, true, nil
	}
	var e zhipuStreamEvent
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return nil, false, nil
	}
	etype := e.Type
	if etype == "" {
		etype = ev.Event
	}
	switch etype {
	case "response.created", "response.in_progress", "response.queued":
		return []stream.Frame{{Kind: stream.FrameLifecycle, Status: etype, Seq: e.SequenceNumber}}, false, nil
	case "response.output_item.added":
		if e.Item != nil && e.Item.Type == "function_call" && e.ItemID != "" {
			id := e.Item.CallID
			if id == "" {
				id = e.ItemID
			}
			d.toolIDs[e.ItemID] = id
			// 补名帧：item.name 载工具函数名（arguments.delta 不再带名）⇒ 名字必须在此进帧；
			// 帧承载约定：FrameToolCallArgsDelta 的 Text 载函数名。name 为空不发帧，保持原行为。
			if e.Item.Name != "" {
				return []stream.Frame{{Kind: stream.FrameToolCallArgsDelta, ToolCallID: id, Text: e.Item.Name, Seq: e.SequenceNumber}}, false, nil
			}
		}
		return nil, false, nil
	case "response.output_text.delta":
		return []stream.Frame{{Kind: stream.FrameTextDelta, Text: e.Delta, Seq: e.SequenceNumber}}, false, nil
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		return []stream.Frame{{Kind: stream.FrameReasoningDelta, Reasoning: e.Delta, Seq: e.SequenceNumber}}, false, nil
	case "response.function_call_arguments.delta":
		id := d.toolIDs[e.ItemID]
		if id == "" {
			id = e.ItemID
		}
		return []stream.Frame{{Kind: stream.FrameToolCallArgsDelta, ToolCallID: id, Arguments: e.PartialJSON, Seq: e.SequenceNumber}}, false, nil
	case "response.completed", "response.incomplete":
		status := core.StatusCompleted
		if etype == "response.incomplete" {
			status = core.StatusIncomplete
		}
		frames := []stream.Frame{{Kind: stream.FrameDone, Status: status, Seq: e.SequenceNumber}}
		if e.Response != nil && len(e.Response.Usage) > 0 {
			frames = append([]stream.Frame{{Kind: stream.FrameUsage, Usage: d.usageFn(e.Response.Usage), Seq: e.SequenceNumber}}, frames...)
		}
		return frames, true, nil
	case "response.failed":
		return []stream.Frame{{Kind: stream.FrameError, Status: "response_failed", Text: "上游流式响应失败", Seq: e.SequenceNumber}}, true, nil
	default:
		return nil, false, nil
	}
}

// StreamStart：强制 stream=true（请求体带 stream_options.chunk_include_usage）⇒ 真实 HTTP ⇒ SSE 解码。
func (a *zhipuAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
	rc := *r
	rc.Stream = true
	out, err := a.BuildRequest(ctx, &rc, res)
	if err != nil {
		return nil, err
	}
	resp, err := streamRequest(ctx, a.hc, a.Provider(), out)
	if err != nil {
		return nil, err
	}
	return newSSEIter(ctx, resp.Body, a.streamDecoder().decode, nil), nil
}
