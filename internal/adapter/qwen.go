// 日期: 2026-11-14 14:29:38.485
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

// Qwen / DashScope 适配器（统一 SDD 4.3.7，两形态按 core.Resolution.Shape 选）：
//
// ① 兼容层（APIShapeOpenAIResponses）：POST {base}/responses；AuthScheme: Authorization + "Bearer "。
//    enable_thinking / enable_search 经 core.Request.Extra 透传（4.3.7 非标准参数）；
//    usage 的 cache_creation_input_tokens 归 usage.Normalize（4.4.2 Qwen 兼容行）。
//
// ② 原生 text-generation（APIShapeQwenNative）：POST {base}/api/v1/services/aigc/text-generation/generation；
//    ⚠️ 流式必须加头 X-DashScope-SSE: enable（3.6 坑 1）；帧格式 `data:{json}`（冒号后**无空格**）、
//    **每帧自带 usage**、**无 [DONE] 哨兵**；usage 字段是 input_tokens / output_tokens（⛔ 与兼容模式
//    的 prompt_tokens / completion_tokens 不同，4.3.7）。
//
// ⚠️ API Key 与地域强绑定（北京 Key 调弗吉尼亚 endpoint 401）——base URL 由注册表决定，适配器只拼路径。
// previous_response_id 支持 = true（3.5 表）。

type qwenAdapter struct {
	hc *http.Client
}

// NewQwen 构造 Qwen 兼容层适配器（Responses 形状）。
func NewQwen(hc *http.Client) Adapter { return &qwenAdapter{hc: hc} }

func (a *qwenAdapter) Provider() core.Provider          { return core.ProviderQwen }
func (a *qwenAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *qwenAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 4.1.1：Authorization: Bearer。
func (a *qwenAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4.2 Qwen 兼容行：命中 + cache_creation_input_tokens 都在二级）。
func (a *qwenAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderQwen, usageTreeShape(raw), raw)
}

// ---- 兼容层请求体（Responses 形状 + enable_thinking / enable_search）----

type qwenReq struct {
	Model              string          `json:"model"`
	Input              []qwenInputItem `json:"input"`
	Instructions       string          `json:"instructions,omitempty"`
	Tools              []qwenTool      `json:"tools,omitempty"`
	ToolChoice         any             `json:"tool_choice,omitempty"`
	MaxOutputTokens    int             `json:"max_output_tokens,omitempty"`
	PromptCacheKey     string          `json:"prompt_cache_key,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	// enable_thinking / enable_search 经 Extra 透传（4.3.7 非标准参数）。
}

type qwenInputItem struct {
	Type             string          `json:"type"`
	Role             string          `json:"role,omitempty"`
	Content          []qwenInputPart `json:"content,omitempty"`
	CallID           string          `json:"call_id,omitempty"`
	Output           string          `json:"output,omitempty"`
	ID               string          `json:"id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Arguments        string          `json:"arguments,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
}

type qwenInputPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type qwenTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// BuildRequest 构造兼容层 /responses 请求。
func (a *qwenAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, err
	}
	body := qwenReq{
		Model:              res.Model,
		Instructions:       r.Instructions,
		MaxOutputTokens:    r.MaxOutputTokens,
		PromptCacheKey:     r.PromptCacheKey,
		PreviousResponseID: r.PreviousResponseID,
		Stream:             r.Stream,
	}
	for _, m := range r.Messages {
		items, sys := qwenInputItems(m)
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
		body.Tools = append(body.Tools, qwenTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	if r.ToolChoice != nil {
		body.ToolChoice = toolChoiceResponses(r.ToolChoice)
	}
	raw, err := marshalBody(body)
	if err != nil {
		return nil, err
	}
	// enable_thinking / enable_search 经 Extra 透传（4.3.7）。
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

// qwenInputItems 把规范消息拆成 Responses input item（4.2.3）。
func qwenInputItems(m core.Message) ([]qwenInputItem, string) {
	if m.Role == "system" {
		return nil, contentText(m.Content)
	}
	if m.Role == "tool" {
		return []qwenInputItem{{Type: "function_call_output", CallID: m.ToolCallID, Output: contentText(m.Content)}}, ""
	}
	var items []qwenInputItem
	if m.EncryptedContent != "" || m.ReasoningContent != "" {
		it := qwenInputItem{Type: "reasoning", EncryptedContent: m.EncryptedContent}
		if m.ReasoningContent != "" {
			it.Content = []qwenInputPart{{Type: "reasoning_text", Text: m.ReasoningContent}}
		}
		items = append(items, it)
	}
	partType := "input_text"
	if m.Role == "assistant" {
		partType = "output_text"
	}
	var parts []qwenInputPart
	for _, p := range m.Content {
		switch {
		case p.ImageURL != "":
			parts = append(parts, qwenInputPart{Type: "input_image", ImageURL: p.ImageURL})
		case p.Text != "":
			parts = append(parts, qwenInputPart{Type: partType, Text: p.Text})
		}
	}
	if len(parts) > 0 {
		items = append(items, qwenInputItem{Type: "message", Role: m.Role, Content: parts})
	}
	for _, tc := range m.ToolCalls {
		id := tc.CallID
		if id == "" {
			id = tc.ID
		}
		items = append(items, qwenInputItem{Type: "function_call", ID: tc.ID, CallID: id, Name: tc.Name, Arguments: tc.Arguments})
	}
	return items, ""
}

// ---- 兼容层响应体（Responses 对象）----

type qwenResp struct {
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
	Usage        json.RawMessage `json:"usage"`
	FinishReason string          `json:"finish_reason"`
}

// ParseResponse 解析兼容层响应。
func (a *qwenAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r qwenResp
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

// ---- 兼容层流式（SSE OpenAI 形状，末帧 data: [DONE]）----

type qwenStreamEvent struct {
	Type           string    `json:"type"`
	SequenceNumber int64     `json:"sequence_number"`
	Response       *qwenResp `json:"response"`
	Delta          string    `json:"delta"`
	PartialJSON    string    `json:"partial_json"`
	ItemID         string    `json:"item_id"`
	Item           *struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	} `json:"item"`
}

type qwenStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[string]string
}

func (a *qwenAdapter) streamDecoder() *qwenStreamDecoder {
	return &qwenStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[string]string{}}
}

func (d *qwenStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if data == "[DONE]" {
		return nil, true, nil
	}
	var e qwenStreamEvent
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

// StreamStart（兼容层）：强制 stream=true ⇒ 真实 HTTP ⇒ SSE 解码。
func (a *qwenAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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

// =====================================================================
// 原生 text-generation 形态（APIShapeQwenNative）
// =====================================================================

type qwenNativeAdapter struct {
	hc *http.Client
}

// NewQwenNative 构造 Qwen 原生 text-generation 适配器。
func NewQwenNative(hc *http.Client) Adapter { return &qwenNativeAdapter{hc: hc} }

func (a *qwenNativeAdapter) Provider() core.Provider          { return core.ProviderQwen }
func (a *qwenNativeAdapter) OutboundShape() OutboundShape     { return ShapeQwenNative }
func (a *qwenNativeAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 4.1.1：Authorization: Bearer（原生与兼容层同一把 key）。
func (a *qwenNativeAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（shape=APIShapeQwenNative，4.3.7：usage 字段 input_tokens /
// output_tokens，⛔ 与兼容模式字段名不同；无缓存明细 ⇒ 读写记 0）。
func (a *qwenNativeAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderQwen, core.APIShapeQwenNative, raw)
}

// ---- 原生请求体（⚠️ SDD 未逐字给字段名 ⇒ 按 DashScope text-generation 通用形状构造：
// model / input.messages / parameters；流式须加头 X-DashScope-SSE: enable，3.6 坑 1）----

type qwenNativeReq struct {
	Model      string           `json:"model"`
	Input      qwenNativeInput  `json:"input"`
	Parameters qwenNativeParams `json:"parameters,omitempty"`
}

type qwenNativeInput struct {
	Messages []qwenNativeMessage `json:"messages"`
}

type qwenNativeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type qwenNativeParams struct {
	ResultFormat   string   `json:"result_format,omitempty"`
	Stream         bool     `json:"stream,omitempty"`
	MaxTokens      int      `json:"max_tokens,omitempty"`
	Temperature    *float64 `json:"temperature,omitempty"`
	TopP           *float64 `json:"top_p,omitempty"`
	Stop           []string `json:"stop,omitempty"`
	EnableThinking *bool    `json:"enable_thinking,omitempty"`
	EnableSearch   any      `json:"enable_search,omitempty"`
}

// BuildRequest 构造原生 text-generation 请求。
func (a *qwenNativeAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, err
	}
	body := qwenNativeReq{
		Model: res.Model,
		Input: qwenNativeInput{},
		Parameters: qwenNativeParams{
			ResultFormat: "message",
			Stream:       r.Stream,
			MaxTokens:    r.MaxOutputTokens,
			Temperature:  r.Temperature,
			TopP:         r.TopP,
			Stop:         r.Stop,
		},
	}
	for _, m := range r.Messages {
		text := contentText(m.Content)
		switch m.Role {
		case "system":
			body.Input.Messages = append(body.Input.Messages, qwenNativeMessage{Role: "system", Content: text})
		case "assistant":
			body.Input.Messages = append(body.Input.Messages, qwenNativeMessage{Role: "assistant", Content: text})
		default:
			body.Input.Messages = append(body.Input.Messages, qwenNativeMessage{Role: "user", Content: text})
		}
	}
	raw, err := marshalBody(body)
	if err != nil {
		return nil, err
	}
	raw, err = mergeExtraBody(raw, r.Extra)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if r.Stream {
		// ⚠️ 原生流式必须加头 X-DashScope-SSE: enable（3.6 坑 1 / 4.3.7）。
		headers["X-DashScope-SSE"] = "enable"
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/api/v1/services/aigc/text-generation/generation"),
		Headers: headers,
		Body:    raw,
	}, nil
}

// ---- 原生响应体（output.text / output.finish_reason / usage.input_tokens|output_tokens）----

type qwenNativeResp struct {
	Output struct {
		Text         string `json:"text"`
		FinishReason string `json:"finish_reason"`
		Choices      []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	} `json:"output"`
	Usage     json.RawMessage `json:"usage"`
	RequestID string          `json:"request_id"`
	Code      string          `json:"code"`
	Message   string          `json:"message"`
}

// ParseResponse 解析原生响应（4.3.7：usage 字段 input_tokens / output_tokens）。
func (a *qwenNativeAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r qwenNativeResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	if r.Code != "" {
		// DashScope 业务错误（code/message 形态）。
		return &core.Response{
			Status: core.StatusFailed,
			Error:  gwerrors.New(core.ErrUpstreamError, 200, r.Code, r.Message),
		}, nil
	}
	out := &core.Response{Status: core.StatusCompleted}
	text := r.Output.Text
	finish := r.Output.FinishReason
	if len(r.Output.Choices) > 0 {
		text = r.Output.Choices[0].Message.Content
		if finish == "" {
			finish = r.Output.Choices[0].FinishReason
		}
	}
	out.Text = text
	out.OutputItems = []core.OutputItem{{
		Type: "message", Role: "assistant", Content: []core.ContentPart{{Type: "text", Text: text}},
	}}
	out.FinishReason = finish
	if out.FinishReason == "" {
		out.FinishReason = core.FinishUnknown
	}
	if len(r.Usage) > 0 {
		out.Usage = a.ParseUsage(r.Usage)
	}
	return out, nil
}

// ---- 原生流式（SSE 帧 `data:{json}` 冒号无空格、每帧自带 usage、⛔ 无 [DONE]）----

type qwenNativeStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
}

func (a *qwenNativeAdapter) streamDecoder() *qwenNativeStreamDecoder {
	return &qwenNativeStreamDecoder{usageFn: a.ParseUsage}
}

func (d *qwenNativeStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	// ⛔ 无 [DONE] 哨兵（4.3.7）；以 output.finish_reason 判终态。
	var r qwenNativeResp
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		return nil, false, nil
	}
	var frames []stream.Frame
	text := r.Output.Text
	if text != "" {
		frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: text})
	}
	// 每帧自带 usage（4.3.7）。
	if len(r.Usage) > 0 {
		frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.usageFn(r.Usage)})
	}
	if r.Output.FinishReason != "" {
		frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: r.Output.FinishReason})
		return frames, true, nil
	}
	return frames, false, nil
}

// StreamStart（原生）：强制 stream=true（请求体 stream + 头 X-DashScope-SSE: enable）⇒ SSE 解码。
func (a *qwenNativeAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
