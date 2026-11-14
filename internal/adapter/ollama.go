// 日期: 2026-11-14 14:49:54.932
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// Ollama 适配器（统一 SDD 4.3.6），两形态按 res.Shape 选：
//   - 原生 /api/chat（ShapeOllamaNative）：NDJSON 流（application/x-ndjson，⛔ 非 SSE、⛔ 无 [DONE]），
//     usage 只在 done:true 帧（prompt_eval_count / prompt_eval_cached_count / eval_count 平铺）；
//   - /v1 兼容层（ShapeChatCompletions）：只走 Chat Completions（Responses 不完整，3.6 ③ 兜底），
//     usage 仅三字段无缓存明细 ⇒ 归一化缓存读写记 0（4.4.2 Ollama /v1 行）。
// 鉴权：本地进程无鉴权（/v1 兼容层任意值忽略）⇒ AuthScheme 空。
// SupportsPreviousResponseID=false（3.5 表：Ollama ❌；/v1/responses 仅无状态 flavor）。

// ---- 原生 /api/chat ----

type ollamaAdapter struct {
	hc *http.Client
}

// NewOllama 构造 Ollama 原生适配器（hc 为出站 HTTP 客户端）。
func NewOllama(hc *http.Client) Adapter { return &ollamaAdapter{hc: hc} }

func (a *ollamaAdapter) Provider() core.Provider          { return core.ProviderOllama }
func (a *ollamaAdapter) OutboundShape() OutboundShape     { return ShapeOllamaNative }
func (a *ollamaAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 空：本地进程无鉴权（4.3.6）。
func (a *ollamaAdapter) AuthScheme() AuthScheme { return AuthScheme{} }

// ParseUsage 委托 usage.Normalize：Ollama 原生是平铺字段（prompt_eval_count 总输入含命中、
// prompt_eval_cached_count ⇒ 缓存读、eval_count ⇒ 输出；Uncached = 两者之差，4.4.3 Ollama 行）。
func (a *ollamaAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderOllama, core.APIShapeOllamaNative, raw)
}

type ollamaReq struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   *bool           `json:"stream,omitempty"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Think    any             `json:"think,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaToolCall struct {
	Function ollamaToolCallFn `json:"function"`
}

type ollamaToolCallFn struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaTool struct {
	Type     string       `json:"type"`
	Function ollamaToolFn `json:"function"`
}

type ollamaToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// BuildRequest：POST {base}/api/chat（4.3.6 请求体关键字段）。
// think 参数：core.Reasoning.Effort ∈ {low, medium, high, max} ⇒ 原值；其余非空 ⇒ true
// （4.3.6：think 是 bool 或 "low"/"medium"/"high"/"max"）。temperature 等进 options。
func (a *ollamaAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, err
	}
	body := ollamaReq{Model: res.Model}
	if r.Stream {
		t := true
		body.Stream = &t
	}
	if r.Reasoning != nil {
		switch r.Reasoning.Effort {
		case "low", "medium", "high", "max":
			body.Think = r.Reasoning.Effort
		case "none", "":
			// 不启用思考 ⇒ 不发 think 字段
		default:
			body.Think = true
		}
	}
	if r.Temperature != nil || r.TopP != nil || r.MaxOutputTokens > 0 {
		body.Options = map[string]any{}
		if r.Temperature != nil {
			body.Options["temperature"] = *r.Temperature
		}
		if r.TopP != nil {
			body.Options["top_p"] = *r.TopP
		}
		if r.MaxOutputTokens > 0 {
			body.Options["num_predict"] = r.MaxOutputTokens
		}
	}
	for _, m := range r.Messages {
		om := ollamaMessage{Role: m.Role, Content: contentText(m.Content), Thinking: m.ReasoningContent, ToolName: m.Name}
		for _, tc := range m.ToolCalls {
			args := json.RawMessage(tc.Arguments)
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			om.ToolCalls = append(om.ToolCalls, ollamaToolCall{
				Function: ollamaToolCallFn{Name: tc.Name, Arguments: args},
			})
		}
		body.Messages = append(body.Messages, om)
	}
	for _, t := range r.Tools {
		body.Tools = append(body.Tools, ollamaTool{
			Type:     "function",
			Function: ollamaToolFn{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	raw, err := marshalBody(body)
	if err != nil {
		return nil, err
	}
	return &Outbound{
		Method:  http.MethodPost,
		URL:     joinURL(res.BaseURL, "/api/chat"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, nil
}

// ollamaResp 是 /api/chat 的响应 / NDJSON 帧（4.3.6：message.role/content/thinking/tool_calls、
// done / done_reason（stop/load/unload）、usage 平铺在顶层、流式时 usage 只在 done:true 帧）。
type ollamaResp struct {
	Model      string        `json:"model"`
	CreatedAt  string        `json:"created_at"`
	Message    ollamaMessage `json:"message"`
	Done       bool          `json:"done"`
	DoneReason string        `json:"done_reason"`
	Context    []int64       `json:"context"`

	PromptEvalCount       int64 `json:"prompt_eval_count"`
	PromptEvalCachedCount int64 `json:"prompt_eval_cached_count"`
	EvalCount             int64 `json:"eval_count"`
}

// ParseResponse 解析非流式单对象响应（done:true 一帧到底）。
func (a *ollamaAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r ollamaResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{Model: r.Model, Status: core.StatusCompleted}
	oi := core.OutputItem{Type: "message", Role: "assistant"}
	oi.Content = append(oi.Content, core.ContentPart{Type: "text", Text: r.Message.Content})
	oi.ReasoningText = r.Message.Thinking
	for _, tc := range r.Message.ToolCalls {
		oi.Type = "message"
		out.OutputItems = append(out.OutputItems, core.OutputItem{
			Type: "function_call", Name: tc.Function.Name, Arguments: string(tc.Function.Arguments),
		})
	}
	out.OutputItems = append([]core.OutputItem{oi}, out.OutputItems...)
	out.Text = r.Message.Content
	out.FinishReason = ollamaDoneReason(r.DoneReason, r.Done)
	if r.PromptEvalCount > 0 || r.EvalCount > 0 {
		out.Usage = a.ParseUsage(body) // 原生 usage 平铺在响应顶层（4.4.2 Ollama /api/chat 行）
	}
	return out, nil
}

// ollamaDoneReason：done_reason 三值（4.5.2 stop/load/unload）⇒ 超集原值；空且 done ⇒ stop。
func ollamaDoneReason(reason string, done bool) string {
	switch reason {
	case core.FinishStop, core.FinishLoad, core.FinishUnload:
		return reason
	case "":
		if done {
			return core.FinishStop
		}
	}
	return reason
}

// ---- 流式（NDJSON：一行一个 JSON 对象；⛔ 无 [DONE] 哨兵）----

type ollamaStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolSeq int
}

func (a *ollamaAdapter) streamDecoder() *ollamaStreamDecoder {
	return &ollamaStreamDecoder{usageFn: a.ParseUsage}
}

func (d *ollamaStreamDecoder) decode(line []byte) ([]stream.Frame, bool, error) {
	var r ollamaResp
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, false, nil // 容忍坏帧（与 SSE 解码口径一致：跳过不中断）
	}
	var frames []stream.Frame
	if r.Message.Thinking != "" {
		frames = append(frames, stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: r.Message.Thinking})
	}
	if r.Message.Content != "" {
		frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: r.Message.Content})
	}
	for _, tc := range r.Message.ToolCalls {
		d.toolSeq++
		frames = append(frames, stream.Frame{
			Kind: stream.FrameToolCallArgsDelta,
			// Frame 承载约定：Text 载函数名、ToolCallID 载调用 id（Ollama 无 call_id ⇒ 顺序生成）。
			ToolCallID: fmt.Sprintf("call_%d", d.toolSeq),
			Text:       tc.Function.Name,
			Arguments:  string(tc.Function.Arguments),
		})
	}
	if r.Done {
		if r.PromptEvalCount > 0 || r.EvalCount > 0 {
			// usage 只在 done:true 帧（4.3.6）。
			frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.usageFn(line)})
		}
		frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: ollamaDoneReason(r.DoneReason, true)})
		return frames, true, nil
	}
	return frames, false, nil
}

// StreamStart：/api/chat 默认即流式 ⇒ 真实 HTTP ⇒ NDJSON 逐行解码（3.6 坑 2）。
func (a *ollamaAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
	return newNDJSONIter(ctx, resp.Body, a.streamDecoder().decode, nil), nil
}

// ---- /v1 兼容层（Chat Completions；⛔ 不用 Responses——4.3.6「仅无状态 flavor」）----

type ollamaCompatAdapter struct {
	hc *http.Client
}

// NewOllamaCompat 构造 Ollama /v1 兼容层适配器。
func NewOllamaCompat(hc *http.Client) Adapter { return &ollamaCompatAdapter{hc: hc} }

func (a *ollamaCompatAdapter) Provider() core.Provider          { return core.ProviderOllama }
func (a *ollamaCompatAdapter) OutboundShape() OutboundShape     { return ShapeChatCompletions }
func (a *ollamaCompatAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 空：/v1 兼容层鉴权任意值忽略（4.3.6）。
func (a *ollamaCompatAdapter) AuthScheme() AuthScheme { return AuthScheme{} }

// ParseUsage 委托 usage.Normalize：/v1 仅 prompt_tokens/completion_tokens/total_tokens，
// 无缓存明细 ⇒ 读写记 0（4.4.2 Ollama /v1 行）。
func (a *ollamaCompatAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderOllama, core.APIShapeOpenAIChat, raw)
}

type ollamaCompatReq struct {
	Model         string          `json:"model"`
	Messages      []ollamaMessage `json:"messages"`
	Stream        bool            `json:"stream,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Stop          []string        `json:"stop,omitempty"`
	Tools         []ollamaTool    `json:"tools,omitempty"`
	StreamOptions map[string]any  `json:"stream_options,omitempty"`
}

// BuildRequest：POST {base}/v1/chat/completions（OpenAI 形状）。
// ⚠️ 兼容层不支持 tool_choice / n / user / logit_bias（4.3.6 已知偏差）⇒ ⛔ 不发这些字段
// （tool_choice 被剔除属能力缺失，按 1.4.2 在 doc 注释登记；上层的 degradation 标记由
// DegradationAware 变体承载，Ollama 兼容层不实现该可选接口）。
func (a *ollamaCompatAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, err
	}
	body := ollamaCompatReq{
		Model:       res.Model,
		Stream:      r.Stream,
		MaxTokens:   r.MaxOutputTokens,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stop:        r.Stop,
	}
	if r.Stream {
		body.StreamOptions = map[string]any{"include_usage": true}
	}
	for _, m := range r.Messages {
		om := ollamaMessage{Role: m.Role, Content: contentText(m.Content), Thinking: m.ReasoningContent, ToolName: m.Name}
		for _, tc := range m.ToolCalls {
			args := json.RawMessage(tc.Arguments)
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			om.ToolCalls = append(om.ToolCalls, ollamaToolCall{Function: ollamaToolCallFn{Name: tc.Name, Arguments: args}})
		}
		body.Messages = append(body.Messages, om)
	}
	for _, t := range r.Tools {
		body.Tools = append(body.Tools, ollamaTool{Type: "function", Function: ollamaToolFn{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	raw, err := marshalBody(body)
	if err != nil {
		return nil, err
	}
	return &Outbound{
		Method:  http.MethodPost,
		URL:     joinURL(res.BaseURL, "/v1/chat/completions"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, nil
}

// ollamaCompatResp 是 /v1 兼容层的 chat.completion 响应 / chunk。
type ollamaCompatResp struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []ollamaToolCall `json:"tool_calls"`
		} `json:"message"`
		Delta struct {
			Content   string           `json:"content"`
			ToolCalls []ollamaToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// ParseResponse 解析 /v1 兼容层非流式响应。
func (a *ollamaCompatAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r ollamaCompatResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{ID: r.ID, Model: r.Model, Status: core.StatusCompleted}
	if len(r.Choices) > 0 {
		c := r.Choices[0]
		oi := core.OutputItem{Type: "message", Role: "assistant"}
		oi.Content = append(oi.Content, core.ContentPart{Type: "text", Text: c.Message.Content})
		out.OutputItems = append(out.OutputItems, oi)
		out.Text = c.Message.Content
		for _, tc := range c.Message.ToolCalls {
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "function_call", Name: tc.Function.Name, Arguments: string(tc.Function.Arguments),
			})
		}
		out.FinishReason = c.FinishReason
		if out.FinishReason == "" {
			out.FinishReason = core.FinishStop
		}
	}
	if len(r.Usage) > 0 && string(r.Usage) != "null" {
		out.Usage = a.ParseUsage(r.Usage)
	}
	return out, nil
}

type ollamaCompatStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolSeq int
}

func (a *ollamaCompatAdapter) streamDecoder() *ollamaCompatStreamDecoder {
	return &ollamaCompatStreamDecoder{usageFn: a.ParseUsage}
}

// decode 解析 chat.completion.chunk（SSE data 行 + [DONE] 哨兵）。
func (d *ollamaCompatStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if data == "[DONE]" {
		return nil, true, nil
	}
	var r ollamaCompatResp
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		return nil, false, nil
	}
	var frames []stream.Frame
	if len(r.Choices) > 0 {
		c := r.Choices[0]
		if c.Delta.Content != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: c.Delta.Content})
		}
		for _, tc := range c.Delta.ToolCalls {
			d.toolSeq++
			frames = append(frames, stream.Frame{
				Kind:       stream.FrameToolCallArgsDelta,
				ToolCallID: fmt.Sprintf("call_%d", d.toolSeq),
				Text:       tc.Function.Name,
				Arguments:  string(tc.Function.Arguments),
			})
		}
		if c.FinishReason != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: c.FinishReason})
		}
	}
	if len(r.Usage) > 0 && string(r.Usage) != "null" {
		frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.usageFn(r.Usage)})
	}
	return frames, false, nil
}

// StreamStart：/v1 兼容层流式（SSE，[DONE] 哨兵）⇒ 真实 HTTP ⇒ SSE 解码。
func (a *ollamaCompatAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
