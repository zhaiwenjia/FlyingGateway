// 日期: 2026-11-14 14:29:38.429
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

// OpenAI 适配器（统一 SDD 4.3.1「统一格式的基准」）：
// 出站 Responses：POST {base}/responses（任务书「出站 Responses /v1/responses」）。
// AuthScheme: Authorization + "Bearer "；previous_response_id 支持 = true（3.5 表）。
// 请求体：input[] 异构 item、instructions 顶层、reasoning 对象、prompt_cache_key 透传；
// 响应体：output[] 异构联合（⛔ 不假设 output[0] 是 message）、encrypted_content 保留；
// 流式：SSE `event:` + `data:` 双字段、response.* 事件、sequence_number；usage 在 response.completed。

type openaiAdapter struct {
	hc *http.Client
}

// NewOpenAI 构造 OpenAI 适配器（hc 为出站 HTTP 客户端；测试传 httptest server 的 client）。
func NewOpenAI(hc *http.Client) Adapter { return &openaiAdapter{hc: hc} }

func (a *openaiAdapter) Provider() core.Provider          { return core.ProviderOpenAI }
func (a *openaiAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *openaiAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 8.4：Authorization: Bearer <key>。
func (a *openaiAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4）：OpenAI 两种 usage 树并存（4.3.1：
// Chat 树 prompt_tokens_details.* / Responses 树 input_tokens_details.*）⇒ 按实际字段选口径。
func (a *openaiAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderOpenAI, usageTreeShape(raw), raw)
}

// ---- 请求体（Responses 形状，4.3.1「Responses 第二模式请求体关键字段」逐字段）----

type openAIReq struct {
	Model              string            `json:"model"`
	Input              []openAIInputItem `json:"input"`
	Instructions       string            `json:"instructions,omitempty"`
	Tools              []openAITool      `json:"tools,omitempty"`
	ToolChoice         any               `json:"tool_choice,omitempty"`
	Reasoning          *openAIReasoning  `json:"reasoning,omitempty"`
	MaxOutputTokens    int               `json:"max_output_tokens,omitempty"`
	PromptCacheKey     string            `json:"prompt_cache_key,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Stream             bool              `json:"stream,omitempty"`
}

// openAIReasoning 是 reasoning 对象（4.3.1：context / effort / mode / summary）。
type openAIReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
	Context string `json:"context,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

// openAIInputItem 是 input[] 异构 item（4.3.1：message / reasoning / function_call / function_call_output）。
type openAIInputItem struct {
	Type    string            `json:"type"`
	Role    string            `json:"role,omitempty"`
	Content []openAIInputPart `json:"content,omitempty"`
	// function_call / function_call_output
	CallID    string `json:"call_id,omitempty"`
	Output    string `json:"output,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// reasoning（⛔ encrypted_content 必须保留，4.2.5）
	EncryptedContent string            `json:"encrypted_content,omitempty"`
	ReasoningContent []openAIInputPart `json:"-"`
}

// openAIInputPart 是 input 的 content 元素（4.3.1：input_text / input_image / input_file / input_audio）。
type openAIInputPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// openAITool 是 function 工具声明（Responses 形状：name/description/parameters 平铺）。
type openAITool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// BuildRequest 构造 /responses 请求（4.3.1 逐字字段；4.2.2 系统提示走顶层 instructions）。
func (a *openaiAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, err
	}
	body := openAIReq{
		Model:              res.Model,
		Instructions:       r.Instructions,
		MaxOutputTokens:    r.MaxOutputTokens,
		PromptCacheKey:     r.PromptCacheKey, // 3.5：prompt_cache_key 始终透传
		PreviousResponseID: r.PreviousResponseID,
		Stream:             r.Stream,
	}
	if r.Reasoning != nil {
		body.Reasoning = &openAIReasoning{
			Effort:  r.Reasoning.Effort,
			Summary: r.Reasoning.Summary,
			Context: r.Reasoning.Context,
			Mode:    r.Reasoning.Mode,
		}
	}
	for _, m := range r.Messages {
		items, sys := a.inputItems(m)
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
		body.Tools = append(body.Tools, openAITool{
			Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		})
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

// inputItems 把规范消息拆成 Responses input item（4.2.3：Chat → Responses = 一条 message 拆多个 item）。
// system 消息不出 item，返回给调用方并入顶层 instructions（4.2.2）。
func (a *openaiAdapter) inputItems(m core.Message) ([]openAIInputItem, string) {
	if m.Role == "system" {
		return nil, contentText(m.Content)
	}
	if m.Role == "tool" {
		// 工具结果 ⇒ function_call_output（4.2.2：Responses 的工具结果是独立 item）。
		return []openAIInputItem{{
			Type: "function_call_output", CallID: m.ToolCallID, Output: contentText(m.Content),
		}}, ""
	}
	var items []openAIInputItem
	// 推理链回放：EncryptedContent 是语义字段（⛔ 必须保留，4.2.5）。
	if m.EncryptedContent != "" || m.ReasoningContent != "" {
		it := openAIInputItem{Type: "reasoning", EncryptedContent: m.EncryptedContent}
		if m.ReasoningContent != "" {
			it.Content = []openAIInputPart{{Type: "reasoning_text", Text: m.ReasoningContent}}
		}
		items = append(items, it)
	}
	partType := "input_text"
	if m.Role == "assistant" {
		partType = "output_text"
	}
	var parts []openAIInputPart
	for _, p := range m.Content {
		switch {
		case p.ImageURL != "":
			parts = append(parts, openAIInputPart{Type: "input_image", ImageURL: p.ImageURL})
		case p.Text != "":
			parts = append(parts, openAIInputPart{Type: partType, Text: p.Text})
		}
	}
	if len(parts) > 0 {
		items = append(items, openAIInputItem{Type: "message", Role: m.Role, Content: parts})
	}
	for _, tc := range m.ToolCalls {
		id := tc.CallID
		if id == "" {
			id = tc.ID
		}
		items = append(items, openAIInputItem{
			Type: "function_call", ID: tc.ID, CallID: id, Name: tc.Name, Arguments: tc.Arguments,
		})
	}
	return items, ""
}

// toolChoiceResponses 映射 tool_choice（4.3.1：auto / none / required / 指定函数名）。
func toolChoiceResponses(tc *core.ToolChoice) any {
	switch tc.Mode {
	case "function":
		if tc.Name != "" {
			return map[string]any{"type": "function", "name": tc.Name}
		}
		return "auto"
	case "none", "required", "auto":
		return tc.Mode
	default:
		return "auto"
	}
}

// ---- 响应体（4.3.1：output[] 异构联合，⛔ 不假设 output[0] 是 message）----

type openAIResp struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"`
	Model             string             `json:"model"`
	Output            []openAIOutputItem `json:"output"`
	OutputText        string             `json:"output_text"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Usage json.RawMessage `json:"usage"`
	// FinishReason 是 DeepSeek / 智谱 / MiMo 等兼容端点回落的 chat 语义字段（4.5.2 非标准取值 ⇒ 超集原值）。
	FinishReason string `json:"finish_reason"`
}

// openAIOutputItem 是 output[] 的异构 item（4.3.1 item 内部结构）。
type openAIOutputItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Role    string `json:"role"`
	Status  string `json:"status"`
	Content []struct {
		Type    string `json:"type"` // output_text / refusal / reasoning_text / summary_text
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
	// EncryptedContent 是 reasoning item 的加密推理链（⛔ 必须保留，4.2.5）。
	EncryptedContent string `json:"encrypted_content"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	// Refusal 是 message item 级的拒绝文本（4.3.1：规范形态是 content 里的 {type:"refusal", refusal} 项；
	// 部分兼容端点把 refusal 直接放 item 级 ⇒ 两级都收，归一进 core.OutputItem.Refusal）。
	Refusal string `json:"refusal"`
	// ReasoningContent 是 Moonshot 等兼容端点把思考放 message.reasoning_content 的扩展（4.3.9）。
	ReasoningContent string `json:"reasoning_content"`
}

// ParseResponse 解析 response 对象 → 规范 Response（4.3.1 响应体关键字段）。
func (a *openaiAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r openAIResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{
		ID:     r.ID,
		Model:  r.Model,
		Status: r.Status,
	}
	hasToolCall := false
	for _, it := range r.Output {
		out.OutputItems = append(out.OutputItems, mapOutputItem(&it))
		if it.Type == "function_call" {
			hasToolCall = true
		}
	}
	// output_text 是便捷字段（4.3.1）；缺失时按 message 的 output_text 累加。
	out.Text = r.OutputText
	if out.Text == "" {
		var b strings.Builder
		for _, it := range r.Output {
			if it.Type != "message" {
				continue
			}
			for _, p := range it.Content {
				if p.Type == "output_text" {
					b.WriteString(p.Text)
				}
			}
		}
		out.Text = b.String()
	}
	out.FinishReason = deriveResponsesFinish(r.FinishReason, r.Status, incompleteReason(r.IncompleteDetails), hasToolCall)
	if len(r.Usage) > 0 {
		out.Usage = a.ParseUsage(r.Usage)
	}
	if r.Error != nil {
		out.Status = core.StatusFailed
		out.Error = gwerrors.New(core.ErrUpstreamError, 0, r.Error.Code, r.Error.Message)
	}
	return out, nil
}

// mapOutputItem 把异构 output item 映射到规范 OutputItem（type / id 保留原值）。
// 文本 part 归一到 ContentPart.Type="text"；refusal 收进 OutputItem.Refusal。
func mapOutputItem(it *openAIOutputItem) core.OutputItem {
	oi := core.OutputItem{
		Type:             it.Type,
		ID:               it.ID,
		Role:             it.Role,
		EncryptedContent: it.EncryptedContent,
		CallID:           it.CallID,
		Name:             it.Name,
		Arguments:        it.Arguments,
		ReasoningText:    it.ReasoningContent,
	}
	for _, p := range it.Content {
		switch p.Type {
		case "refusal":
			oi.Refusal += p.Refusal
		case "output_text", "text":
			oi.Content = append(oi.Content, core.ContentPart{Type: "text", Text: p.Text})
		case "reasoning_text":
			oi.ReasoningText += p.Text
		}
	}
	for _, s := range it.Summary {
		oi.Summary = append(oi.Summary, core.SummaryPart{Type: s.Type, Text: s.Text})
	}
	if it.Refusal != "" {
		oi.Refusal += it.Refusal
	}
	return oi
}

// incompleteReason 取 incomplete_details.reason（4.3.1：max_output_tokens / max_messages / content_filter / steered）。
func incompleteReason(d *struct {
	Reason string `json:"reason"`
}) string {
	if d == nil {
		return ""
	}
	return d.Reason
}

// deriveResponsesFinish 按 4.5.2 OpenAI Responses 行推导终止原因：
// Responses 用 status 六值体系；显式 finish_reason（兼容端点回落字段）⇒ 超集原值优先；
// incomplete ⇒ incomplete_details.reason 归「超上下文 / length」类（4.5.1 归一化映射规则）。
func deriveResponsesFinish(explicit, status, incomplete string, hasToolCall bool) string {
	if explicit != "" {
		return explicit // 4.5.1：非标准取值 ⛔ 不得丢弃 ⇒ 超集原值
	}
	switch status {
	case core.StatusCompleted:
		if hasToolCall {
			return core.FinishToolCalls
		}
		return core.FinishStop
	case core.StatusIncomplete:
		switch incomplete {
		case "max_output_tokens", "max_messages":
			return core.FinishLength
		case "content_filter":
			return core.FinishContentFilter
		case "":
			return core.FinishUnknown
		default:
			return incomplete // steered 等 ⇒ 超集原值
		}
	case core.StatusFailed, core.StatusCancelled, core.StatusInProgress, core.StatusQueued:
		return core.FinishUnknown
	default:
		return core.FinishUnknown
	}
}

// ---- 流式（4.3.1 Responses 流式事件形态：event: + data: 双字段、sequence_number、59 种事件）----

// openAIStreamEvent 覆盖本适配器消费的 response.* 事件；未知事件类型忽略（3.7 流式容错）。
type openAIStreamEvent struct {
	Type           string            `json:"type"`
	SequenceNumber int64             `json:"sequence_number"`
	Response       *openAIResp       `json:"response"`
	Delta          string            `json:"delta"`
	PartialJSON    string            `json:"partial_json"`
	ItemID         string            `json:"item_id"`
	Item           *openAIOutputItem `json:"item"`
}

// openAIStreamDecoder 是 OpenAI 的帧解码器（3.7：每家一个帧解码器）。
// 容忍：缺 output_item.added（gpt-5.3-codex 实测，4.3.1）、缺 [DONE]、未知事件、乱序。
type openAIStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[string]string // item_id → call_id（缺 output_item.added 时退回 item_id）
}

func (a *openaiAdapter) streamDecoder() *openAIStreamDecoder {
	return &openAIStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[string]string{}}
}

func (d *openAIStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if data == "[DONE]" {
		// 终止哨兵（部分后端省略 ⇒ 不能只靠它判结束，4.3.1）。
		return nil, true, nil
	}
	var e openAIStreamEvent
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return nil, false, nil // 非 JSON 帧（keep-alive 等）容忍跳过
	}
	etype := e.Type
	if etype == "" {
		etype = ev.Event // data 内 type 与 event 名冗余，任选其一解析（4.3.1）
	}
	seq := e.SequenceNumber
	switch etype {
	case "response.created", "response.in_progress", "response.queued":
		return []stream.Frame{{Kind: stream.FrameLifecycle, Status: etype, Seq: seq}}, false, nil
	case "response.output_item.added":
		if e.Item != nil && e.Item.Type == "function_call" && e.ItemID != "" {
			id := e.Item.CallID
			if id == "" {
				id = e.ItemID
			}
			d.toolIDs[e.ItemID] = id
			// 补名帧（4.3.1：output_item.added 的 item.name 载工具函数名，后续
			// function_call_arguments.delta 不再带名）⇒ 名字必须在此进帧；
			// 帧承载约定：FrameToolCallArgsDelta 的 Text 载函数名、ToolCallID 载调用 id。
			// name 为空不发帧，保持原行为。
			if e.Item.Name != "" {
				return []stream.Frame{{Kind: stream.FrameToolCallArgsDelta, ToolCallID: id, Text: e.Item.Name, Seq: seq}}, false, nil
			}
		}
		return nil, false, nil
	case "response.output_text.delta":
		return []stream.Frame{{Kind: stream.FrameTextDelta, Text: e.Delta, Seq: seq}}, false, nil
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		return []stream.Frame{{Kind: stream.FrameReasoningDelta, Reasoning: e.Delta, Seq: seq}}, false, nil
	case "response.function_call_arguments.delta":
		id := d.toolIDs[e.ItemID]
		if id == "" {
			id = e.ItemID // 缺 output_item.added 时容忍：直接用 item_id（4.3.1 解析器容忍性）
		}
		return []stream.Frame{{Kind: stream.FrameToolCallArgsDelta, ToolCallID: id, Arguments: e.PartialJSON, Seq: seq}}, false, nil
	case "response.completed":
		frames := []stream.Frame{{Kind: stream.FrameDone, Status: core.StatusCompleted, Seq: seq}}
		if e.Response != nil && len(e.Response.Usage) > 0 {
			// usage 由 response.completed 自带（⛔ 不需要 stream_options.include_usage，4.3.1）。
			frames = append([]stream.Frame{{Kind: stream.FrameUsage, Usage: d.usageFn(e.Response.Usage), Seq: seq}}, frames...)
		}
		return frames, true, nil
	case "response.incomplete":
		frames := []stream.Frame{{Kind: stream.FrameDone, Status: core.StatusIncomplete, Seq: seq}}
		if e.Response != nil && len(e.Response.Usage) > 0 {
			frames = append([]stream.Frame{{Kind: stream.FrameUsage, Usage: d.usageFn(e.Response.Usage), Seq: seq}}, frames...)
		}
		return frames, true, nil
	case "response.failed":
		msg := "上游流式响应失败"
		code := "response_failed"
		if e.Response != nil && e.Response.Error != nil {
			msg, code = e.Response.Error.Message, e.Response.Error.Code
		}
		return []stream.Frame{{Kind: stream.FrameError, Status: code, Text: msg, Seq: seq}}, true, nil
	default:
		// 未知事件类型 ⇒ 忽略（3.7 流式容错；事件总数 59 种，⛔ 不假设全部认识）。
		return nil, false, nil
	}
}

// StreamStart：BuildRequest（强制 stream=true，3.7 上游传输恒为流式）⇒ 真实 HTTP ⇒ SSE 解码。
func (a *openaiAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
