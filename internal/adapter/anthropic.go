// 日期: 2026-11-14 14:29:38.436
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// Anthropic 适配器（统一 SDD 4.3.2「原生形状，不可压平」）：
// 出站原生 Messages：POST {base}/messages；AuthScheme: x-api-key + anthropic-version: 2026-10-21。
// ⚠️ API version 2026-10-21 起 temperature / top_p / top_k 一律 400 ⇒ 构造时必须剔除（4.3.2）。
// max_tokens 必填：core 无值 ⇒ 给默认值并记 Degradation「max_tokens 缺省已补」（4.2.4 / 11.2）。
// thinking 块的 signature ⛔ 不可压平（4.2.5：thinking block 必须原样回传，修改即 400）。
// usage 的 input_tokens 是残值 ⇒ 归一须三项合成（4.4.3），委托 usage.Normalize(APIShapeAnthropicMessages)。
// 流式：message_start 初始 usage + message_delta 累计 usage（⛔ 不逐帧相加）；无 [DONE]。
// previous_response_id 支持 = false（3.5 表）。
//
// MiniMax 的 Anthropic 兼容端点复用本文件的构造 / 解析 / 流式解码（3.6：适配器可复用 Anthropic 解析器）。

// anthropicDefaultMaxTokens 是 max_tokens 缺省补值（SDD 未给定数值；取 Anthropic 常用上限档位之一，
// 仅在 core.Request.MaxOutputTokens 无值时补，且必然伴随 Degradation 记录，⛔ 不静默）。
const anthropicDefaultMaxTokens = 4096

type anthropicAdapter struct {
	hc *http.Client
}

// NewAnthropic 构造 Anthropic 适配器（hc 为出站 HTTP 客户端）。
func NewAnthropic(hc *http.Client) Adapter { return &anthropicAdapter{hc: hc} }

func (a *anthropicAdapter) Provider() core.Provider          { return core.ProviderAnthropic }
func (a *anthropicAdapter) OutboundShape() OutboundShape     { return ShapeAnthropicMessages }
func (a *anthropicAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 8.4 / 2.3 鉴权头三形态表：x-api-key + anthropic-version: 2026-10-21（4.3.2）。
func (a *anthropicAdapter) AuthScheme() AuthScheme {
	return AuthScheme{
		HeaderName:   "x-api-key",
		Prefix:       "",
		ExtraHeaders: map[string]string{"anthropic-version": "2026-10-21"},
	}
}

// ParseUsage 委托 usage.Normalize：shape=APIShapeAnthropicMessages ⇒ 残值语义（4.4.3：
// total_input = cache_read + cache_creation + input_tokens）。
func (a *anthropicAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderAnthropic, core.APIShapeAnthropicMessages, raw)
}

// ---- 请求体（4.3.2 请求体关键字段逐字：model / messages / max_tokens / system / tools /
// tool_choice / stream / thinking / cache_control）----

type anthropicReq struct {
	Model      string             `json:"model"`
	Messages   []anthropicMessage `json:"messages"`
	MaxTokens  int                `json:"max_tokens"`
	System     string             `json:"system,omitempty"`
	Tools      []anthropicTool    `json:"tools,omitempty"`
	ToolChoice any                `json:"tool_choice,omitempty"`
	Stream     bool               `json:"stream,omitempty"`
	Thinking   any                `json:"thinking,omitempty"`
	// ⛔ temperature / top_p / top_k 不设字段（2026-10-21 起传值即 400，4.3.2）。
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock 是 content[] 内容块（4.3.2：text / thinking / redacted_thinking / tool_use / tool_result）。
type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// thinking 块（⛔ signature 不可压平，4.2.5）
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// redacted_thinking 块
	Data string `json:"data,omitempty"`
	// tool_use 块
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`
	// tool_result 块
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
	// cache_control 缓存断点（4.3.2：{type:"ephemeral", ttl:"5m"|"1h"}）
	CacheControl *core.CacheControl `json:"cache_control,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicRequestBody 是 Anthropic 形请求体构造（Claude 与 MiniMax Anthropic 兼容共用，3.6 允许复用）。
// 返回构造体与转换期 degradation 记录（max_tokens 缺省已补等）。
func anthropicRequestBody(r *core.Request) (*anthropicReq, []core.Degradation, error) {
	body := &anthropicReq{
		Model:     r.Model,
		MaxTokens: r.MaxOutputTokens,
		Stream:    r.Stream,
	}
	var degs []core.Degradation
	if body.MaxTokens == 0 {
		// max_tokens 必填（4.2.4）：core 无值 ⇒ 补默认值并显式记录降级（11.2 Chat → Anthropic 行）。
		body.MaxTokens = anthropicDefaultMaxTokens
		degs = append(degs, core.Degradation{
			Field:  "max_tokens",
			Reason: core.DegradationUpstreamShapeMismatch,
			Detail: "max_tokens 缺省已补",
		})
	}
	if r.Reasoning != nil {
		// thinking 映射 core.ReasoningConfig（4.3.2：thinking: {type:"adaptive", display:"summarized"}）。
		body.Thinking = map[string]any{"type": "adaptive", "display": "summarized"}
	}
	for _, m := range r.Messages {
		msgs, sys, err := anthropicMessageOf(m)
		if err != nil {
			return nil, nil, err
		}
		if sys != "" {
			if body.System == "" {
				body.System = sys
			} else {
				body.System += "\n" + sys
			}
			continue
		}
		body.Messages = append(body.Messages, msgs...)
	}
	// cache_control 断点（4.3.2）：core.Request.CacheControl 落到最后一个消息块（缓存整个前缀）。
	if r.CacheControl != nil && len(body.Messages) > 0 {
		last := &body.Messages[len(body.Messages)-1]
		if n := len(last.Content); n > 0 {
			last.Content[n-1].CacheControl = r.CacheControl
		}
	}
	for _, t := range r.Tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		body.Tools = append(body.Tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	if r.ToolChoice != nil {
		body.ToolChoice = anthropicToolChoice(r.ToolChoice)
	}
	return body, degs, nil
}

// anthropicMessageOf 把规范消息映射为 Anthropic messages（system ⇒ 顶层 system，4.2.4）。
func anthropicMessageOf(m core.Message) ([]anthropicMessage, string, error) {
	if m.Role == "system" {
		return nil, contentText(m.Content), nil
	}
	if m.Role == "tool" {
		// 工具结果是 user 角色的 tool_result 块（4.2.4：tool_calls[] → tool_use 结构转换）。
		return []anthropicMessage{{
			Role:    "user",
			Content: []anthropicBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: contentText(m.Content)}},
		}}, "", nil
	}
	role := m.Role
	if role == "developer" {
		role = "user" // Anthropic 无 developer 角色
	}
	var blocks []anthropicBlock
	if m.ThinkingSignature != "" || m.ReasoningContent != "" {
		// thinking 块原样回传（⛔ signature 不可压平，4.2.5：修改即 400 invalid_request_error）。
		blocks = append(blocks, anthropicBlock{
			Type: "thinking", Thinking: m.ReasoningContent, Signature: m.ThinkingSignature,
		})
	}
	for _, p := range m.Content {
		if p.Text != "" {
			blocks = append(blocks, anthropicBlock{Type: "text", Text: p.Text})
		}
	}
	for _, tc := range m.ToolCalls {
		var input any
		if tc.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
				return nil, "", &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "invalid_tool_arguments",
					"tool_call arguments 不是合法 JSON，无法映射为 Anthropic tool_use.input")}
			}
		}
		if input == nil {
			input = map[string]any{}
		}
		blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
	}
	if len(blocks) == 0 {
		return nil, "", nil
	}
	return []anthropicMessage{{Role: role, Content: blocks}}, "", nil
}

// anthropicToolChoice 映射 tool_choice（Anthropic：auto / none / any / tool）。
func anthropicToolChoice(tc *core.ToolChoice) any {
	switch tc.Mode {
	case "none":
		return map[string]any{"type": "none"}
	case "required":
		return map[string]any{"type": "any"}
	case "function":
		if tc.Name != "" {
			return map[string]any{"type": "tool", "name": tc.Name}
		}
		return map[string]any{"type": "auto"}
	default:
		return map[string]any{"type": "auto"}
	}
}

// BuildRequestWithDegradations 构造请求并返回转换期 degradation（接口 DegradationAware）。
func (a *anthropicAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, nil, err
	}
	body, degs, err := anthropicRequestBody(r)
	if err != nil {
		return nil, nil, err
	}
	body.Model = res.Model
	raw, err := marshalBody(body)
	if err != nil {
		return nil, nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/messages"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, degs, nil
}

// BuildRequest 委托 BuildRequestWithDegradations（签名钉死的接口方法）。
func (a *anthropicAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// ---- 响应体（4.3.2：content[] 块数组 + stop_reason 七值 + usage 残值）----

type anthropicResp struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Model   string `json:"model"`
	Content []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		Data      string          `json:"data"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason   string          `json:"stop_reason"`
	StopSequence string          `json:"stop_sequence"`
	Usage        json.RawMessage `json:"usage"`
}

// parseAnthropicMessage 是 Anthropic 形响应解析（Claude 与 MiniMax Anthropic 兼容共用，3.6 允许复用）。
// usageFn 由所属适配器注入（usage 语义各家仍需各自核对，4.3.11：MiniMax 的 input_tokens 也是残值）。
func parseAnthropicMessage(usageFn func(json.RawMessage) *core.Usage, body []byte) (*core.Response, error) {
	var r anthropicResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{ID: r.ID, Model: r.Model}
	var text string
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text += b.Text
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "message", Role: "assistant",
				Content: []core.ContentPart{{Type: "text", Text: b.Text}},
			})
		case "thinking":
			// signature ⇒ core.OutputItem.ThinkingSignature（⛔ 不可压平，4.2.5）。
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "thinking", ReasoningText: b.Thinking, ThinkingSignature: b.Signature,
			})
		case "redacted_thinking":
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "redacted_thinking", EncryptedContent: b.Data,
			})
		case "tool_use":
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "tool_use", ID: b.ID, CallID: b.ID, Name: b.Name, Arguments: string(b.Input),
			})
		default:
			out.OutputItems = append(out.OutputItems, core.OutputItem{Type: b.Type, ID: b.ID})
		}
	}
	out.Text = text
	// stop_reason 七值 ⇒ 超集原值（4.5.2：与 OpenAI finish_reason 不是同一套，映射有损 ⇒ 保留原值）。
	out.FinishReason = r.StopReason
	if out.FinishReason == "" {
		out.FinishReason = core.FinishUnknown
	}
	switch r.StopReason {
	case "max_tokens", "model_context_window_exceeded":
		out.Status = core.StatusIncomplete
	default:
		out.Status = core.StatusCompleted
	}
	if len(r.Usage) > 0 {
		out.Usage = usageFn(r.Usage)
	}
	return out, nil
}

// ---- 流式（4.3.2 流式事件形态：message_start → content_block_* → message_delta → message_stop）----

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Name      string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"` // text_delta / thinking_delta / signature_delta / input_json_delta
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage json.RawMessage `json:"usage"` // message_delta：⚠️ 累计值，⛔ 不逐帧相加（4.3.2）
}

// anthropicStreamDecoder 是 Anthropic 形帧解码器（Claude / MiniMax Anthropic 兼容共用）。
type anthropicStreamDecoder struct {
	usageFn    func(json.RawMessage) *core.Usage
	toolIDs    map[int]string // content block index → tool_use id
	startUsage *core.Usage    // message_start 的初始 usage（message_delta 缺输入侧时合并）
	stopReason string
}

func newAnthropicStreamDecoder(usageFn func(json.RawMessage) *core.Usage) *anthropicStreamDecoder {
	return &anthropicStreamDecoder{usageFn: usageFn, toolIDs: map[int]string{}}
}

func (d *anthropicStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := ev.Data
	if data == "" {
		return nil, false, nil
	}
	var e anthropicStreamEvent
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return nil, false, nil
	}
	etype := e.Type
	if etype == "" {
		etype = ev.Event
	}
	switch etype {
	case "message_start":
		frames := []stream.Frame{{Kind: stream.FrameLifecycle, Status: "message_start"}}
		if e.Message != nil && len(e.Message.Usage) > 0 {
			d.startUsage = d.usageFn(e.Message.Usage)
			frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.startUsage})
		}
		return frames, false, nil
	case "content_block_start":
		if e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
			id := e.ContentBlock.ID
			if id == "" {
				id = e.ContentBlock.Name
			}
			d.toolIDs[e.Index] = id
			frames := []stream.Frame{{Kind: stream.FrameLifecycle, Status: "tool_use", ToolCallID: id}}
			// 补名帧：函数名只在 content_block_start 的 tool_use 块出现（4.3.2），
			// 后续 input_json_delta 不再带名 ⇒ 名字必须在此进帧。⚠️ 不能搭在
			// FrameLifecycle 上——帧承载约定里只有 FrameToolCallArgsDelta 的 Text
			// 会被消费端读作函数名，生命周期帧会被帧累加器忽略 ⇒ 用参数帧补名
			//（Arguments 为空，累加无副作用；name 为空不发帧，保持原行为）。
			if e.ContentBlock.Name != "" {
				frames = append(frames, stream.Frame{Kind: stream.FrameToolCallArgsDelta, ToolCallID: id, Text: e.ContentBlock.Name})
			}
			return frames, false, nil
		}
		return nil, false, nil
	case "content_block_delta":
		if e.Delta == nil {
			return nil, false, nil
		}
		switch e.Delta.Type {
		case "text_delta":
			return []stream.Frame{{Kind: stream.FrameTextDelta, Text: e.Delta.Text}}, false, nil
		case "thinking_delta":
			return []stream.Frame{{Kind: stream.FrameReasoningDelta, Reasoning: e.Delta.Thinking}}, false, nil
		case "input_json_delta":
			return []stream.Frame{{Kind: stream.FrameToolCallArgsDelta, ToolCallID: d.toolIDs[e.Index], Arguments: e.Delta.PartialJSON}}, false, nil
		case "signature_delta":
			// signature 无法承载于 stream.Frame（帧抽象无签名字段，3.7）⇒ 流式路径不回传签名，
			// 多轮 thinking 续聊走非流式路径；见交付报告偏差说明。
			return nil, false, nil
		default:
			return nil, false, nil
		}
	case "content_block_stop", "ping":
		return nil, false, nil
	case "message_delta":
		if e.Delta != nil && e.Delta.StopReason != "" {
			d.stopReason = e.Delta.StopReason
		}
		if len(e.Usage) == 0 {
			return nil, false, nil
		}
		// ⚠️ message_delta 的 usage 是累计值（4.3.2）⇒ 取累计快照，⛔ 不逐帧相加。
		u := d.usageFn(e.Usage)
		if u.InputTokens == 0 && d.startUsage != nil {
			// 累计快照缺输入侧时合并 message_start 的初始值（输入侧不随输出增长）。
			u.InputTokens = d.startUsage.InputTokens
			u.InputTokensUncached = d.startUsage.InputTokensUncached
			u.CacheReadTokens = d.startUsage.CacheReadTokens
			u.CacheWriteTokens = d.startUsage.CacheWriteTokens
		}
		return []stream.Frame{{Kind: stream.FrameUsage, Usage: u}}, false, nil
	case "message_stop":
		status := d.stopReason
		if status == "" {
			status = core.FinishUnknown
		}
		return []stream.Frame{{Kind: stream.FrameDone, Status: status}}, true, nil
	default:
		// 未知事件（含扩展事件）⇒ 忽略（3.7 流式容错）。
		return nil, false, nil
	}
}

// ParseResponse 解析 Anthropic 形响应（4.3.2）。
func (a *anthropicAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	return parseAnthropicMessage(a.ParseUsage, body)
}

// StreamStart：BuildRequest（强制 stream）⇒ 真实 HTTP ⇒ Anthropic 事件解码（⛔ 无 [DONE]）。
func (a *anthropicAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
	return newSSEIter(ctx, resp.Body, newAnthropicStreamDecoder(a.ParseUsage).decode, nil), nil
}
