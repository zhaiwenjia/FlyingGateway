// 日期: 2026-11-14 14:49:54.932
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// Ollama 适配器（统一 SDD 4.3.6），两形态按 res.Shape 选：
//   - 原生 /api/chat（ShapeOllamaNative）：NDJSON 流（application/x-ndjson，⛔ 非 SSE、⛔ 无 [DONE]），
//     usage 只在 done:true 帧（prompt_eval_count / prompt_eval_cached_count / eval_count 平铺）；
//   - /v1 兼容层（ShapeChatCompletions）：只走 Chat Completions（Responses 不完整，3.6 ③ 兜底），
//     请求构造 / 流式执行 / 响应解析经官方 SDK openai-go/v3（10.3）；⛔ tool_choice / n / user /
//     logit_bias 不发（4.3.6 已知偏差）；usage 仅三字段无缓存明细 ⇒ 归一化缓存读写记 0（4.4.2 Ollama /v1 行）。
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

// ---- SDK 构造（openai-go/v3 typed 参数，3.6 / 10.3）----

// ollamaCompatSDKClient 构造 openai-go 客户端（Tier 1：流式执行经 SDK 客户端）。
// base 须带 /v1 前缀（SDK 内部路径是 chat/completions，出站端点为 {base}/v1/chat/completions）；
// 其余口径：注入既有 *http.Client（凭据由 authTransport 在 RoundTrip 注入，⛔ 不进 SDK）、
// ⛔ 不读环境变量密钥、⛔ SDK 自带重试不许生效（重试/降级由网关统一管，24）。
func ollamaCompatSDKClient(hc *http.Client, baseURL string) openai.Client {
	return openai.NewClient(
		option.WithBaseURL(joinURL(baseURL, "/v1")),
		option.WithHTTPClient(hc),
		option.WithAPIKey(""),
		option.WithMaxRetries(0),
	)
}

// ollamaCompatSDKMapErr 把 SDK 错误映射回 DetailError（与 streamRequest 的错误口径逐项对齐，7.1）。
func ollamaCompatSDKMapErr(err error) error {
	var ae *openai.Error
	if errors.As(err, &ae) {
		var body []byte
		var rid string
		if ae.Response != nil {
			if ae.Response.Body != nil {
				body, _ = io.ReadAll(ae.Response.Body)
			}
			rid = ae.Response.Header.Get("X-Request-Id")
		}
		return &DetailError{Detail: gwerrors.FromUpstream(core.ProviderOllama, ae.StatusCode, body, rid)}
	}
	return &DetailError{Detail: gwerrors.FromTransportError(core.ProviderOllama, err, "")}
}

// ollamaCompatSDKMessages 映射消息序列（wire 与既有 ollamaMessage 形逐字段一致）。
// SDK typed 只承载 role / content；Ollama 扩展字段（thinking / tool_name）与原生形 tool_calls
// （function.arguments 是 JSON 对象、⛔ 无 id/type）经 SetExtraFields 补（quirks 不许丢）；
// role=tool ⛔ 不发 tool_call_id（既有 wire 无此键、靠 tool_name 关联 ⇒ 用 param.Omit 剔除
// SDK 必发的 tool_call_id）。
func ollamaCompatSDKMessages(r *core.Request) []openai.ChatCompletionMessageParamUnion {
	var out []openai.ChatCompletionMessageParamUnion
	for _, m := range r.Messages {
		text := contentText(m.Content)
		var msg openai.ChatCompletionMessageParamUnion
		switch m.Role {
		case "system":
			msg = openai.SystemMessage(text)
		case "developer":
			msg = openai.DeveloperMessage(text)
		case "assistant":
			msg = openai.ChatCompletionMessageParamOfAssistant(text)
		case "tool":
			msg = openai.ToolMessage(text, m.ToolCallID)
		default:
			msg = openai.UserMessage(text)
		}
		extras := map[string]any{}
		if m.Role == "tool" {
			extras["tool_call_id"] = param.Omit
		}
		if m.ReasoningContent != "" {
			extras["thinking"] = m.ReasoningContent
		}
		if m.Name != "" {
			extras["tool_name"] = m.Name
		}
		if len(m.ToolCalls) > 0 {
			var calls []ollamaToolCall
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Arguments)
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				calls = append(calls, ollamaToolCall{Function: ollamaToolCallFn{Name: tc.Name, Arguments: args}})
			}
			extras["tool_calls"] = calls
		}
		if len(extras) > 0 {
			switch {
			case msg.OfUser != nil:
				msg.OfUser.SetExtraFields(extras)
			case msg.OfSystem != nil:
				msg.OfSystem.SetExtraFields(extras)
			case msg.OfDeveloper != nil:
				msg.OfDeveloper.SetExtraFields(extras)
			case msg.OfAssistant != nil:
				msg.OfAssistant.SetExtraFields(extras)
			case msg.OfTool != nil:
				msg.OfTool.SetExtraFields(extras)
			}
		}
		out = append(out, msg)
	}
	return out
}

// ollamaCompatSDKTools 映射工具声明（wire 与既有 type:function + function{name,...} 形一致）。
// parameters `{}` ⇒ 空非 nil map ⇒ 仍发 `"parameters":{}`；nil ⇒ 不发该键（omitempty 口径）。
func ollamaCompatSDKTools(r *core.Request) ([]openai.ChatCompletionToolUnionParam, error) {
	var out []openai.ChatCompletionToolUnionParam
	for _, t := range r.Tools {
		fn := shared.FunctionDefinitionParam{Name: t.Name}
		if t.Description != "" {
			fn.Description = param.NewOpt(t.Description)
		}
		if len(t.Parameters) > 0 {
			var schema map[string]any
			if err := json.Unmarshal(t.Parameters, &schema); err != nil {
				return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_marshal_failed",
					"工具 "+t.Name+" 的 parameters 不是合法 JSON: "+err.Error())}
			}
			fn.Parameters = schema
		}
		out = append(out, openai.ChatCompletionToolUnionParam{OfFunction: &openai.ChatCompletionFunctionToolParam{Function: fn}})
	}
	return out, nil
}

// ollamaCompatParams 用 SDK typed 参数构造 /v1 请求（wire：model / messages / max_tokens /
// temperature / top_p / stop / tools / stream / stream_options，逐字段不变）：
// max_tokens / temperature / top_p / stop / tools / stream_options.include_usage；
// ⛔ 不设 tool_choice / n / user / logit_bias（4.3.6 已知偏差）。
func ollamaCompatParams(r *core.Request, res *core.Resolution) (openai.ChatCompletionNewParams, error) {
	var body openai.ChatCompletionNewParams
	body.Model = openai.ChatModel(res.Model)
	body.Messages = ollamaCompatSDKMessages(r)
	tools, err := ollamaCompatSDKTools(r)
	if err != nil {
		return body, err
	}
	body.Tools = tools
	if r.MaxOutputTokens > 0 {
		body.MaxTokens = param.NewOpt(int64(r.MaxOutputTokens))
	}
	if r.Temperature != nil {
		body.Temperature = param.NewOpt(*r.Temperature)
	}
	if r.TopP != nil {
		body.TopP = param.NewOpt(*r.TopP)
	}
	if len(r.Stop) > 0 {
		body.Stop = openai.ChatCompletionNewParamsStopUnion{OfStringArray: r.Stop}
	}
	extras := map[string]any{}
	if r.Stream {
		body.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: param.NewOpt(true)}
		extras["stream"] = true
	}
	if len(body.Messages) == 0 {
		// 既有 wire：messages 无 omitempty ⇒ 空消息发 `"messages":null`（SDK omitzero 会整键省略）。
		extras["messages"] = nil
	}
	if len(extras) > 0 {
		body.SetExtraFields(extras)
	}
	return body, nil
}

// BuildRequest：POST {base}/v1/chat/completions（OpenAI 形状，3.6 ③ Chat 兜底）。
// ⚠️ 兼容层不支持 tool_choice / n / user / logit_bias（4.3.6 已知偏差）⇒ ⛔ 不发这些字段
// （tool_choice 被剔除属能力缺失，按 1.4.2 在 doc 注释登记；上层的 degradation 标记由
// DegradationAware 变体承载，Ollama 兼容层不实现该可选接口）。
func (a *ollamaCompatAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, err
	}
	body, err := ollamaCompatParams(r, res)
	if err != nil {
		return nil, err
	}
	raw, err := body.MarshalJSON()
	if err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_marshal_failed", err.Error())}
	}
	return &Outbound{
		Method:  http.MethodPost,
		URL:     joinURL(res.BaseURL, "/v1/chat/completions"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, nil
}

// ollamaCompatProbe 是 SDK typed 解析之外的 quirk 原始探测（openai.ChatCompletion 不承载）：
// message.tool_calls 的 arguments 原样 JSON（既有 wire 把 arguments 当 JSON 值透传，
// ⛔ 不是 SDK 的字符串字段）与原始 usage（vendor 字段逐字进 usage.Normalize）。
type ollamaCompatProbe struct {
	Usage   json.RawMessage `json:"usage"`
	Choices []struct {
		Message struct {
			ToolCalls []ollamaToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

// ParseResponse 解析 /v1 兼容层非流式响应（Tier 1：SDK 响应类型解析 + quirk 原始探测，
// usage 走既有归一）。解析口径与既有实现逐项一致。
func (a *ollamaCompatAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var probe ollamaCompatProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	var cc openai.ChatCompletion
	if err := json.Unmarshal(body, &cc); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{ID: cc.ID, Model: cc.Model, Status: core.StatusCompleted}
	if len(cc.Choices) > 0 {
		c := cc.Choices[0]
		var toolCalls []ollamaToolCall
		if len(probe.Choices) > 0 {
			toolCalls = probe.Choices[0].Message.ToolCalls
		}
		oi := core.OutputItem{Type: "message", Role: "assistant"}
		oi.Content = append(oi.Content, core.ContentPart{Type: "text", Text: c.Message.Content})
		out.OutputItems = append(out.OutputItems, oi)
		out.Text = c.Message.Content
		for _, tc := range toolCalls {
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "function_call", Name: tc.Function.Name, Arguments: string(tc.Function.Arguments),
			})
		}
		out.FinishReason = c.FinishReason
		if out.FinishReason == "" {
			out.FinishReason = core.FinishStop
		}
	}
	if len(probe.Usage) > 0 && string(probe.Usage) != "null" {
		out.Usage = a.ParseUsage(probe.Usage)
	}
	return out, nil
}

// ---- 流式（Tier 1：SDK 客户端流式执行 + SDK 流事件 ⇒ stream.Frame）----

// ollamaCompatChunkProbe 是流式 chunk 的 quirk 原始探测：delta.tool_calls 的 arguments
// 原样 JSON（既有帧把 raw JSON 文本当 Arguments 透传）与原始 usage。
type ollamaCompatChunkProbe struct {
	Usage   json.RawMessage `json:"usage"`
	Choices []struct {
		Delta struct {
			ToolCalls []ollamaToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

// ollamaCompatChunkDecoder 把 SDK 流事件映射为内部帧（帧序列口径 ⛔ 不变：
// 文本增量 → 工具帧（call_N 合成 id）→ done 帧 → usage 帧，⛔ 非终态）。
type ollamaCompatChunkDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolSeq int
}

func newOllamaCompatChunkDecoder(usageFn func(json.RawMessage) *core.Usage) *ollamaCompatChunkDecoder {
	return &ollamaCompatChunkDecoder{usageFn: usageFn}
}

func (d *ollamaCompatChunkDecoder) decode(c openai.ChatCompletionChunk) ([]stream.Frame, bool) {
	var probe ollamaCompatChunkProbe
	_ = json.Unmarshal([]byte(c.RawJSON()), &probe)
	var frames []stream.Frame
	if len(c.Choices) > 0 {
		choice := c.Choices[0]
		if choice.Delta.Content != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: choice.Delta.Content})
		}
		var toolCalls []ollamaToolCall
		if len(probe.Choices) > 0 {
			toolCalls = probe.Choices[0].Delta.ToolCalls
		}
		for _, tc := range toolCalls {
			d.toolSeq++
			frames = append(frames, stream.Frame{
				Kind: stream.FrameToolCallArgsDelta,
				// Frame 承载约定：Text 载函数名、ToolCallID 载调用 id（Ollama 无 call_id ⇒ 顺序生成）。
				ToolCallID: fmt.Sprintf("call_%d", d.toolSeq),
				Text:       tc.Function.Name,
				Arguments:  string(tc.Function.Arguments),
			})
		}
		if choice.FinishReason != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: choice.FinishReason})
		}
	}
	if len(probe.Usage) > 0 && string(probe.Usage) != "null" {
		frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.usageFn(probe.Usage)})
	}
	// ⛔ 非终态（与既有解码器同口径）：终态由 [DONE] 哨兵 / 连接收口判定。
	return frames, false
}

// ollamaCompatStreamIter 是 SDK 流式帧迭代器（语义对齐 sseIter）。
type ollamaCompatStreamIter struct {
	ctx     context.Context
	st      *ssestream.Stream[openai.ChatCompletionChunk]
	dec     *ollamaCompatChunkDecoder
	queue   []stream.Frame
	cur     stream.Frame
	err     error
	stop    bool
	closed  bool
	seqAuto int64
}

func (it *ollamaCompatStreamIter) Next() bool {
	for len(it.queue) == 0 {
		if it.stop {
			it.close()
			return false
		}
		if err := it.ctx.Err(); err != nil {
			// ctx 取消 ⇒ 立即中断并关闭上游连接（3.7 取消语义；cancel_grace_period 默认 0）。
			it.err = err
			it.close()
			return false
		}
		if !it.st.Next() {
			if e := it.st.Err(); e != nil {
				it.err = e
			}
			it.close()
			return false
		}
		frames, done := it.dec.decode(it.st.Current())
		for i := range frames {
			it.seqAuto++
			if frames[i].Seq == 0 {
				frames[i].Seq = it.seqAuto
			}
		}
		it.queue = frames
		it.stop = done
	}
	it.cur = it.queue[0]
	it.queue = it.queue[1:]
	return true
}

// Frame 返回当前帧。
func (it *ollamaCompatStreamIter) Frame() stream.Frame { return it.cur }

// Err 返回终态错误（nil 表示正常结束）。
func (it *ollamaCompatStreamIter) Err() error { return it.err }

// close 关闭 SDK 流（连带关闭上游 body）；幂等。
func (it *ollamaCompatStreamIter) close() {
	if !it.closed {
		it.closed = true
		_ = it.st.Close()
	}
}

// StreamStart（Tier 1）：强制 stream=true ⇒ SDK 客户端流式执行 ⇒ SDK 流事件映射帧。
// 请求体与 BuildRequest 的 Outbound.Body 同源（经 SetJSON 原样发）。
func (a *ollamaCompatAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
	rc := *r
	rc.Stream = true
	params, err := ollamaCompatParams(&rc, res)
	if err != nil {
		return nil, err
	}
	raw, err := params.MarshalJSON()
	if err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_marshal_failed", err.Error())}
	}
	param.SetJSON(raw, &params)
	cl := ollamaCompatSDKClient(a.hc, res.BaseURL)
	st := cl.Chat.Completions.NewStreaming(ctx, params)
	if err := st.Err(); err != nil {
		return nil, ollamaCompatSDKMapErr(err)
	}
	return &ollamaCompatStreamIter{ctx: ctx, st: st, dec: newOllamaCompatChunkDecoder(a.ParseUsage)}, nil
}
