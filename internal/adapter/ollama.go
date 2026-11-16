// 日期: 2026-11-14 14:49:54.932
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// Ollama 适配器（统一 SDD 4.3.6），两形态按 res.Shape 选：
//   - 原生 /api/chat（ShapeOllamaNative）：NDJSON 流（application/x-ndjson，⛔ 非 SSE、⛔ 无 [DONE]），
//     usage 只在 done:true 帧（prompt_eval_count / prompt_eval_cached_count / eval_count 平铺）；
//   - /v1 兼容层（res.Shape=APIShapeOpenAIChat 注册键）：出站 OpenAI Responses 形 POST {base}/v1/responses
//     （v0.13.3+ 支持、仅无状态 flavor），装配 / 响应解析 / 流式解码走响应系共享机件；
//     ⛔ tool_choice / n / user / logit_bias 不发（4.3.6 已知偏差；tool_choice 剔除、非 auto 才记降级）；
//     usage 两种树按实际字段选口径（usageTreeShape，4.4.2 Ollama /v1 行）。
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

// ---- /v1 兼容层（OpenAI Responses 形：POST {base}/v1/responses）----

type ollamaCompatAdapter struct {
	hc *http.Client
}

// NewOllamaCompat 构造 Ollama /v1 兼容层适配器。
func NewOllamaCompat(hc *http.Client) Adapter { return &ollamaCompatAdapter{hc: hc} }

func (a *ollamaCompatAdapter) Provider() core.Provider      { return core.ProviderOllama }
func (a *ollamaCompatAdapter) OutboundShape() OutboundShape { return ShapeResponses }

// SupportsPreviousResponseID=false：/v1/responses 仅无状态 flavor（官方原话「Only the non-stateful
// flavor is supported (i.e., there is no previous_response_id or conversation support)」，v0.13.3+）。
func (a *ollamaCompatAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 空：/v1 兼容层鉴权任意值忽略（4.3.6）。
func (a *ollamaCompatAdapter) AuthScheme() AuthScheme { return AuthScheme{} }

// ParseUsage 委托 usage.Normalize（4.4）：按实际 usage 树选口径（usageTreeShape）——Responses 树
// （input_tokens*）按 input_tokens_details 归一；Chat 树（prompt_tokens*）无缓存明细 ⇒ 读写记 0
// （4.4.2 Ollama /v1 行）。
func (a *ollamaCompatAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderOllama, usageTreeShape(raw), raw)
}

// BuildRequestWithDegradations 构造 POST {base}/v1/responses 请求并返回降级记录。
// 装配走共享机件 responsesBuildParams（3.6）；家差：tool_choice 官方未列支持（OpenAI compatibility
// Supported features 仅 Streaming / Tools (function calling) / Reasoning summaries）⇒ 请求带
// tool_choice 时出站剔除，仅非 auto（none / required / 指定函数名）记降级——auto 与 Ollama 默认
// 行为语义一致、无损失 ⛔ 不记（DegradationAware，口径同 zhipu / moonshot）；⛔ n / user / logit_bias
// 不发（4.3.6 已知偏差，Responses 形装配本就不产出这些字段）。
func (a *ollamaCompatAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	// previous_response_id 不支持（官方：v0.13.3+ 仅无状态 flavor）⇒ 显式报错（3.5）。
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, nil, err
	}
	var degs []core.Degradation
	rc := *r
	if r.ToolChoice != nil {
		// tool_choice 官方未列支持 ⇒ 出站一律剔除；仅当调用方意图真正丢失（none / required /
		// 指定函数名）时记降级——auto 与 Ollama 默认行为（模型自行决定）语义一致、无损失
		// ⛔ 不记（口径同 zhipu / moonshot：auto 不记、非 auto 才记）。
		if r.ToolChoice.Mode != "auto" {
			degs = append(degs, core.Degradation{
				Field:  "tool_choice",
				Reason: core.DegradationCapabilityUnsupported,
				Detail: "Ollama /v1 兼容层不支持 tool_choice，已从出站请求剔除",
			})
		}
		// 从装配入参剔除 ⇒ wire 不出 tool_choice 键（⛔ 不静默丢弃：降级随 DegradationAware 返回）。
		rc.ToolChoice = nil
	}
	p, err := responsesBuildParams(&rc, res)
	if err != nil {
		return nil, nil, err
	}
	raw, err := marshalBody(p)
	if err != nil {
		return nil, nil, err
	}
	// 供应商特异参数经 Extra 透传（mergeExtraBody 黑名单口径，3.1）。
	raw, err = mergeExtraBody(raw, r.Extra)
	if err != nil {
		return nil, nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/v1/responses"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, degs, nil
}

// BuildRequest 委托 BuildRequestWithDegradations（降级记录由上层合并进 x_gateway.degradations[]）。
func (a *ollamaCompatAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// ParseResponse 解析 Responses 响应对象（共享机件 parseResponsesBody；真机样本
// testdata/real/ollama_responses_*_real.json 无家差特殊解析需求）。
func (a *ollamaCompatAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	return parseResponsesBody(a.ParseUsage, body)
}

// streamDecoder 返回 Responses 形帧解码器（共享机件；真机流以 response.completed 收口、
// 无 [DONE]，[DONE] 有无都容忍）。
func (a *ollamaCompatAdapter) streamDecoder() *openAIStreamDecoder {
	return newResponsesStreamDecoder(a.ParseUsage)
}

// StreamStart：强制 stream=true ⇒ BuildRequest ⇒ 真实 HTTP ⇒ SSE 解码（照 openai.go）。
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
