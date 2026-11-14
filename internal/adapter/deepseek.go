// 日期: 2026-11-14 14:29:38.457
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

// DeepSeek 适配器（统一 SDD 4.3.4）：
// 出站 Responses：POST {base}/responses（任务书「deepseek（Responses；Bearer）：/responses」）。
// AuthScheme: Authorization + "Bearer "；previous_response_id 支持 = true（3.5 表）。
// usage 是「命中 / 未命中」双字段（prompt_cache_hit_tokens 与 prompt_cache_miss_tokens，
// 4.3.4：⚠️ 唯一给双字段的家；prompt_cache_hit_tokens 与 prompt_tokens_details.cached_tokens 同值
// ⇒ 取其一 ⛔ 不累加）——归 usage.Normalize 的 DeepSeek 逐家口径。
// finish_reason 含 aborted / insufficient_system_resource（DeepSeek 独有，4.5.1）⇒ 超集原值。
// 流式：SSE（OpenAI 形状），末帧携带 usage。

type deepseekAdapter struct {
	hc *http.Client
}

// NewDeepSeek 构造 DeepSeek 适配器（hc 为出站 HTTP 客户端）。
func NewDeepSeek(hc *http.Client) Adapter { return &deepseekAdapter{hc: hc} }

func (a *deepseekAdapter) Provider() core.Provider          { return core.ProviderDeepSeek }
func (a *deepseekAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *deepseekAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 4.3.4：Authorization: Bearer。
func (a *deepseekAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize：DeepSeek usage 树是 hit/miss 形（4.3.4 / 4.4.2 DeepSeek 行：
// prompt_tokens = 命中 + 未命中、缓存命中取其一不累加）⇒ 按实际字段选口径。
func (a *deepseekAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderDeepSeek, usageTreeShape(raw), raw)
}

// ---- 请求体（Responses 形状，4.3.1 字段名逐字）----

type deepseekReq struct {
	Model              string              `json:"model"`
	Input              []deepseekInputItem `json:"input"`
	Instructions       string              `json:"instructions,omitempty"`
	Tools              []deepseekTool      `json:"tools,omitempty"`
	ToolChoice         any                 `json:"tool_choice,omitempty"`
	Reasoning          *deepseekReasoning  `json:"reasoning,omitempty"`
	MaxOutputTokens    int                 `json:"max_output_tokens,omitempty"`
	PromptCacheKey     string              `json:"prompt_cache_key,omitempty"`
	PreviousResponseID string              `json:"previous_response_id,omitempty"`
	Stream             bool                `json:"stream,omitempty"`
}

type deepseekReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type deepseekInputItem struct {
	Type             string              `json:"type"`
	Role             string              `json:"role,omitempty"`
	Content          []deepseekInputPart `json:"content,omitempty"`
	CallID           string              `json:"call_id,omitempty"`
	Output           string              `json:"output,omitempty"`
	ID               string              `json:"id,omitempty"`
	Name             string              `json:"name,omitempty"`
	Arguments        string              `json:"arguments,omitempty"`
	EncryptedContent string              `json:"encrypted_content,omitempty"`
}

type deepseekInputPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type deepseekTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// BuildRequest 构造 /responses 请求（任务书：/responses；4.3.1 Responses 字段名逐字）。
func (a *deepseekAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, err
	}
	body := deepseekReq{
		Model:              res.Model,
		Instructions:       r.Instructions,
		MaxOutputTokens:    r.MaxOutputTokens,
		PromptCacheKey:     r.PromptCacheKey,
		PreviousResponseID: r.PreviousResponseID,
		Stream:             r.Stream,
	}
	if r.Reasoning != nil {
		body.Reasoning = &deepseekReasoning{Effort: r.Reasoning.Effort}
	}
	for _, m := range r.Messages {
		items, sys := deepseekInputItems(m)
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
		body.Tools = append(body.Tools, deepseekTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters})
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

// deepseekInputItems 把规范消息拆成 Responses input item（4.2.3）。
func deepseekInputItems(m core.Message) ([]deepseekInputItem, string) {
	if m.Role == "system" {
		return nil, contentText(m.Content)
	}
	if m.Role == "tool" {
		return []deepseekInputItem{{Type: "function_call_output", CallID: m.ToolCallID, Output: contentText(m.Content)}}, ""
	}
	var items []deepseekInputItem
	if m.EncryptedContent != "" || m.ReasoningContent != "" {
		it := deepseekInputItem{Type: "reasoning", EncryptedContent: m.EncryptedContent}
		if m.ReasoningContent != "" {
			it.Content = []deepseekInputPart{{Type: "reasoning_text", Text: m.ReasoningContent}}
		}
		items = append(items, it)
	}
	partType := "input_text"
	if m.Role == "assistant" {
		partType = "output_text"
	}
	var parts []deepseekInputPart
	for _, p := range m.Content {
		switch {
		case p.ImageURL != "":
			parts = append(parts, deepseekInputPart{Type: "input_image", ImageURL: p.ImageURL})
		case p.Text != "":
			parts = append(parts, deepseekInputPart{Type: partType, Text: p.Text})
		}
	}
	if len(parts) > 0 {
		items = append(items, deepseekInputItem{Type: "message", Role: m.Role, Content: parts})
	}
	for _, tc := range m.ToolCalls {
		id := tc.CallID
		if id == "" {
			id = tc.ID
		}
		items = append(items, deepseekInputItem{Type: "function_call", ID: tc.ID, CallID: id, Name: tc.Name, Arguments: tc.Arguments})
	}
	return items, ""
}

// ---- 响应体（Responses 对象 + DeepSeek 回落字段 finish_reason）----

type deepseekResp struct {
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
		// ReasoningContent 是 DeepSeek 思考模式的专有字段（4.3.4：choices[].delta.reasoning_content），
		// 兼容端点可能回落到 item 上。
		ReasoningContent string `json:"reasoning_content"`
	} `json:"output"`
	OutputText        string `json:"output_text"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage json.RawMessage `json:"usage"`
	// FinishReason：DeepSeek 兼容端点回落的 chat 语义字段（4.5.2 取值含 aborted /
	// insufficient_system_resource）⇒ 超集原值。
	FinishReason string `json:"finish_reason"`
}

// ParseResponse 解析 DeepSeek 响应（4.3.4 / 4.5.2：finish_reason 原值入超集）。
func (a *deepseekAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r deepseekResp
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

// ---- 流式（SSE OpenAI 形状；末帧携带 usage）----

type deepseekStreamEvent struct {
	Type           string        `json:"type"`
	SequenceNumber int64         `json:"sequence_number"`
	Response       *deepseekResp `json:"response"`
	Delta          string        `json:"delta"`
	PartialJSON    string        `json:"partial_json"`
	ItemID         string        `json:"item_id"`
	Item           *struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	} `json:"item"`
}

type deepseekStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[string]string
}

func (a *deepseekAdapter) streamDecoder() *deepseekStreamDecoder {
	return &deepseekStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[string]string{}}
}

func (d *deepseekStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if data == "[DONE]" {
		return nil, true, nil
	}
	var e deepseekStreamEvent
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
		// 末帧携带 usage（4.3.4 流式事件形态）。
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
func (a *deepseekAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
