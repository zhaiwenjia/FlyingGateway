// 日期: 2026-11-14 14:29:38.464
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

// Kimi / Moonshot 适配器（统一 SDD 4.3.9）：
// 出站 Responses：POST {base}/responses（4.1.4：base_url 同为 api.moonshot.cn/v1）。
// AuthScheme: Authorization + "Bearer "；previous_response_id 支持 = true（3.5 表）。
// ⚠️ 输出上限字段是 max_completion_tokens（⛔ 非 max_tokens——已 deprecated，任务书逐字）。
// prompt_cache_options{mode, ttl} 经 core.Request.Extra 透传（4.3.9）；
// ⛔ 显式 prompt_cache_breakpoint 不发（会被拒 400，4.3.9）⇒ Extra 黑名单剔除。
// reasoning_content 解析进推理正文位（core.OutputItem.ReasoningText；4.3.9 思考承载字段）。
// usage：cached_tokens 顶层与二级同值取其一、cache_write_tokens 单列（4.4.2 Kimi 行）⇒ parseKimi。

type moonshotAdapter struct {
	hc *http.Client
}

// NewMoonshot 构造 Kimi / Moonshot 适配器（hc 为出站 HTTP 客户端）。
func NewMoonshot(hc *http.Client) Adapter { return &moonshotAdapter{hc: hc} }

func (a *moonshotAdapter) Provider() core.Provider          { return core.ProviderMoonshot }
func (a *moonshotAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *moonshotAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 4.3.9：Authorization: Bearer。
func (a *moonshotAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4.2 Kimi 行：prompt_tokens 总数 = 未命中 + 命中 + 写入，三者互斥）。
func (a *moonshotAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderMoonshot, usageTreeShape(raw), raw)
}

// ---- 请求体（Responses 形状 + Moonshot 特异字段）----

type moonshotReq struct {
	Model        string              `json:"model"`
	Input        []moonshotInputItem `json:"input"`
	Instructions string              `json:"instructions,omitempty"`
	Tools        []moonshotTool      `json:"tools,omitempty"`
	ToolChoice   any                 `json:"tool_choice,omitempty"`
	Reasoning    *moonshotReasoning  `json:"reasoning,omitempty"`
	// MaxCompletionTokens 是 Moonshot 的输出上限字段（4.3.9：max_tokens 已 deprecated，⛔ 不发）。
	MaxCompletionTokens int    `json:"max_completion_tokens,omitempty"`
	PromptCacheKey      string `json:"prompt_cache_key,omitempty"`
	PreviousResponseID  string `json:"previous_response_id,omitempty"`
	Stream              bool   `json:"stream,omitempty"`
	// Extra 透传的供应商特有参数（prompt_cache_options{mode,ttl} 等，4.3.9）由 applyExtra 并入。
	Extra map[string]any `json:"-"`
}

type moonshotReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type moonshotInputItem struct {
	Type             string              `json:"type"`
	Role             string              `json:"role,omitempty"`
	Content          []moonshotInputPart `json:"content,omitempty"`
	CallID           string              `json:"call_id,omitempty"`
	Output           string              `json:"output,omitempty"`
	ID               string              `json:"id,omitempty"`
	Name             string              `json:"name,omitempty"`
	Arguments        string              `json:"arguments,omitempty"`
	EncryptedContent string              `json:"encrypted_content,omitempty"`
}

type moonshotInputPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type moonshotTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// BuildRequest 构造 /responses 请求（4.3.9 关键字段逐字）。
func (a *moonshotAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, err
	}
	body := moonshotReq{
		Model:               res.Model,
		Instructions:        r.Instructions,
		MaxCompletionTokens: r.MaxOutputTokens,
		PromptCacheKey:      r.PromptCacheKey,
		PreviousResponseID:  r.PreviousResponseID,
		Stream:              r.Stream,
	}
	if r.Reasoning != nil {
		body.Reasoning = &moonshotReasoning{Effort: r.Reasoning.Effort}
	}
	for _, m := range r.Messages {
		items, sys := moonshotInputItems(m)
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
		body.Tools = append(body.Tools, moonshotTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	if r.ToolChoice != nil {
		body.ToolChoice = toolChoiceResponses(r.ToolChoice)
	}
	// 序列化后并入 Extra（prompt_cache_options{mode,ttl} 等；⛔ prompt_cache_breakpoint 已被黑名单剔除）。
	raw, err := marshalBody(body)
	if err != nil {
		return nil, err
	}
	raw, err = mergeExtraBody(raw, r.Extra)
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

// moonshotInputItems 把规范消息拆成 Responses input item（4.2.3）。
func moonshotInputItems(m core.Message) ([]moonshotInputItem, string) {
	if m.Role == "system" {
		return nil, contentText(m.Content)
	}
	if m.Role == "tool" {
		return []moonshotInputItem{{Type: "function_call_output", CallID: m.ToolCallID, Output: contentText(m.Content)}}, ""
	}
	var items []moonshotInputItem
	if m.EncryptedContent != "" || m.ReasoningContent != "" {
		it := moonshotInputItem{Type: "reasoning", EncryptedContent: m.EncryptedContent}
		if m.ReasoningContent != "" {
			it.Content = []moonshotInputPart{{Type: "reasoning_text", Text: m.ReasoningContent}}
		}
		items = append(items, it)
	}
	partType := "input_text"
	if m.Role == "assistant" {
		partType = "output_text"
	}
	var parts []moonshotInputPart
	for _, p := range m.Content {
		switch {
		case p.ImageURL != "":
			parts = append(parts, moonshotInputPart{Type: "input_image", ImageURL: p.ImageURL})
		case p.Text != "":
			parts = append(parts, moonshotInputPart{Type: partType, Text: p.Text})
		}
	}
	if len(parts) > 0 {
		items = append(items, moonshotInputItem{Type: "message", Role: m.Role, Content: parts})
	}
	for _, tc := range m.ToolCalls {
		id := tc.CallID
		if id == "" {
			id = tc.ID
		}
		items = append(items, moonshotInputItem{Type: "function_call", ID: tc.ID, CallID: id, Name: tc.Name, Arguments: tc.Arguments})
	}
	return items, ""
}

// ---- 响应体（Responses 对象 + reasoning_content 扩展）----

type moonshotResp struct {
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
		// ReasoningContent 是 Moonshot 思考承载字段（4.3.9）⇒ 推理正文位。
		ReasoningContent string `json:"reasoning_content"`
	} `json:"output"`
	OutputText        string `json:"output_text"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage        json.RawMessage `json:"usage"`
	FinishReason string          `json:"finish_reason"`
}

// ParseResponse 解析 Moonshot 响应（reasoning_content ⇒ core.OutputItem.ReasoningText，4.3.9）。
func (a *moonshotAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r moonshotResp
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

// ---- 流式（Responses SSE；usage 在 response.completed）----

type moonshotStreamEvent struct {
	Type           string        `json:"type"`
	SequenceNumber int64         `json:"sequence_number"`
	Response       *moonshotResp `json:"response"`
	Delta          string        `json:"delta"`
	PartialJSON    string        `json:"partial_json"`
	ItemID         string        `json:"item_id"`
	Item           *struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	} `json:"item"`
}

type moonshotStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[string]string
}

func (a *moonshotAdapter) streamDecoder() *moonshotStreamDecoder {
	return &moonshotStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[string]string{}}
}

func (d *moonshotStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if data == "[DONE]" {
		return nil, true, nil
	}
	var e moonshotStreamEvent
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

// StreamStart：强制 stream=true ⇒ 真实 HTTP ⇒ SSE 解码。
func (a *moonshotAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
