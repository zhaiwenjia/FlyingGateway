// 日期: 2026-11-14 14:29:38.450
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

// Grok / xAI 适配器（统一 SDD 4.3.5）：
// 出站 Responses：POST {base}/responses（任务书「出站 Responses /v1/responses」；Chat 已标 legacy）。
// AuthScheme: Authorization + "Bearer "；previous_response_id 支持 = true（3.5 表）。
// usage 含 cost_in_usd_ticks（实际计费额，$1 USD = 10^10 ticks）与 cached_prompt_text_tokens
// ⇒ 两字段随 Raw 保留，归 usage.Normalize 处理其余口径（4.3.5 / 5.6 ①自报费用优先）。

type xaiAdapter struct {
	hc *http.Client
}

// NewXAI 构造 Grok / xAI 适配器（hc 为出站 HTTP 客户端）。
func NewXAI(hc *http.Client) Adapter { return &xaiAdapter{hc: hc} }

func (a *xaiAdapter) Provider() core.Provider          { return core.ProviderXAI }
func (a *xaiAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *xaiAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 8.4：Authorization: Bearer <key>。
func (a *xaiAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize：xAI 两种 usage 树并存（4.3.5：Chat 树含 cached_prompt_text_tokens /
// cost_in_usd_ticks；Responses 树 input_tokens/output_tokens + cost_in_usd_ticks）⇒ 按实际字段选口径，
// cost_in_usd_ticks 原文留 Raw（5.6 ①）。
func (a *xaiAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderXAI, usageTreeShape(raw), raw)
}

// ---- 请求体（Responses 形状，4.3.1 字段名逐字；max_output_tokens 见 4.3.5）----

type xaiReq struct {
	Model              string         `json:"model"`
	Input              []xaiInputItem `json:"input"`
	Instructions       string         `json:"instructions,omitempty"`
	Tools              []xaiTool      `json:"tools,omitempty"`
	ToolChoice         any            `json:"tool_choice,omitempty"`
	Reasoning          *xaiReasoning  `json:"reasoning,omitempty"`
	MaxOutputTokens    int            `json:"max_output_tokens,omitempty"`
	PromptCacheKey     string         `json:"prompt_cache_key,omitempty"`
	PreviousResponseID string         `json:"previous_response_id,omitempty"`
	Stream             bool           `json:"stream,omitempty"`
}

// xaiReasoning 是 reasoning 对象；effort 取值含 low/medium/high/xhigh（4.3.5）。
type xaiReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type xaiInputItem struct {
	Type             string         `json:"type"`
	Role             string         `json:"role,omitempty"`
	Content          []xaiInputPart `json:"content,omitempty"`
	CallID           string         `json:"call_id,omitempty"`
	Output           string         `json:"output,omitempty"`
	ID               string         `json:"id,omitempty"`
	Name             string         `json:"name,omitempty"`
	Arguments        string         `json:"arguments,omitempty"`
	EncryptedContent string         `json:"encrypted_content,omitempty"`
}

type xaiInputPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type xaiTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// BuildRequest 构造 /v1/responses 请求（4.3.1 Responses 字段名逐字）。
func (a *xaiAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, err
	}
	body := xaiReq{
		Model:              res.Model,
		Instructions:       r.Instructions,
		MaxOutputTokens:    r.MaxOutputTokens,
		PromptCacheKey:     r.PromptCacheKey,
		PreviousResponseID: r.PreviousResponseID,
		Stream:             r.Stream,
	}
	if r.Reasoning != nil {
		body.Reasoning = &xaiReasoning{Effort: r.Reasoning.Effort}
	}
	for _, m := range r.Messages {
		items, sys := xaiInputItems(m)
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
		body.Tools = append(body.Tools, xaiTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	if r.ToolChoice != nil {
		body.ToolChoice = toolChoiceResponses(r.ToolChoice)
	}
	raw, err := marshalBody(body)
	if err != nil {
		return nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/responses"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, nil
}

// xaiInputItems 把规范消息拆成 Responses input item（4.2.3 Chat → Responses 转换口径）。
func xaiInputItems(m core.Message) ([]xaiInputItem, string) {
	if m.Role == "system" {
		return nil, contentText(m.Content)
	}
	if m.Role == "tool" {
		return []xaiInputItem{{Type: "function_call_output", CallID: m.ToolCallID, Output: contentText(m.Content)}}, ""
	}
	var items []xaiInputItem
	if m.EncryptedContent != "" || m.ReasoningContent != "" {
		it := xaiInputItem{Type: "reasoning", EncryptedContent: m.EncryptedContent}
		if m.ReasoningContent != "" {
			it.Content = []xaiInputPart{{Type: "reasoning_text", Text: m.ReasoningContent}}
		}
		items = append(items, it)
	}
	partType := "input_text"
	if m.Role == "assistant" {
		partType = "output_text"
	}
	var parts []xaiInputPart
	for _, p := range m.Content {
		switch {
		case p.ImageURL != "":
			parts = append(parts, xaiInputPart{Type: "input_image", ImageURL: p.ImageURL})
		case p.Text != "":
			parts = append(parts, xaiInputPart{Type: partType, Text: p.Text})
		}
	}
	if len(parts) > 0 {
		items = append(items, xaiInputItem{Type: "message", Role: m.Role, Content: parts})
	}
	for _, tc := range m.ToolCalls {
		id := tc.CallID
		if id == "" {
			id = tc.ID
		}
		items = append(items, xaiInputItem{Type: "function_call", ID: tc.ID, CallID: id, Name: tc.Name, Arguments: tc.Arguments})
	}
	return items, ""
}

// ---- 响应体（Responses 对象；usage 里的 cost_in_usd_ticks 随 Raw 保留）----

type xaiResp struct {
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
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"output"`
	OutputText        string `json:"output_text"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage        json.RawMessage `json:"usage"`
	FinishReason string          `json:"finish_reason"`
}

// ParseResponse 解析 xAI Responses 对象（4.3.1 output[] 异构；4.3.5 usage 特异字段留 Raw）。
func (a *xaiAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r xaiResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{ID: r.ID, Model: r.Model, Status: r.Status, Text: r.OutputText}
	hasToolCall := false
	var text string
	for _, it := range r.Output {
		oi := core.OutputItem{Type: it.Type, ID: it.ID, Role: it.Role, EncryptedContent: it.EncryptedContent,
			CallID: it.CallID, Name: it.Name, Arguments: it.Arguments}
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

// ---- 流式（Responses SSE：event: + data:；usage 在 response.completed）----

type xaiStreamEvent struct {
	Type           string   `json:"type"`
	SequenceNumber int64    `json:"sequence_number"`
	Response       *xaiResp `json:"response"`
	Delta          string   `json:"delta"`
	PartialJSON    string   `json:"partial_json"`
	ItemID         string   `json:"item_id"`
	Item           *struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	} `json:"item"`
}

type xaiStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[string]string
}

func (a *xaiAdapter) streamDecoder() *xaiStreamDecoder {
	return &xaiStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[string]string{}}
}

func (d *xaiStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if data == "[DONE]" {
		return nil, true, nil
	}
	var e xaiStreamEvent
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
		return nil, false, nil // 未知事件容忍（3.7 流式容错）
	}
}

// StreamStart：强制 stream=true ⇒ 真实 HTTP ⇒ SSE 解码。
func (a *xaiAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
