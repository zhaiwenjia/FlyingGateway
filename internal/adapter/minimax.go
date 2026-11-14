// 日期: 2026-11-14 14:29:38.492
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

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

// MiniMax 适配器（统一 SDD 4.3.11，三形态按 core.Resolution.Shape 选）：
//
// ① OpenAI 形（APIShapeOpenAIChat）：POST {base}/chat/completions（4.3.11 端点路径逐字）。
//    请求构造 / 流式执行 / 响应解析经官方 SDK openai-go/v3（10.3：openai-go 服务 8 家
//    OpenAI 兼容端点）；quirks（GroupId 禁发、temperature/top_p 剔除、base_resp 必检、
//    thinking 拆包、tool_choice 降级记录）继续独立处理。
// ② Anthropic 形（APIShapeAnthropicMessages）：POST {base}/messages（base = api.minimax.io/anthropic，
//    4.1.2 /anthropic/v1/messages）；复用 Anthropic 解析器（3.6：适配器可复用），input_tokens 残值同 Claude。
// ③ 原生 V2（APIShapeMiniMaxNativeV2）：POST {base}/text/chatcompletion_v2（4.3.11 逐字）。
//
// 三形态共性（4.3.11）：
//   - ⛔ 不传 GroupId / group_id（旧版遗留口径，沿用会引入无效参数）。
//   - ⚠️ temperature / top_p 不受支持（传值无效，恒为 1 / 0.95）⇒ 构造时剔除。
//   - ⚠️ base_resp 必检：HTTP 200 + status_code != 0 是业务错误 ⇒ errors.FromMiniMaxBaseResp
//     （⛔ 不检查 base_resp 就会把错误当成功）。
//   - reasoning_split=false 时思考并入 content 且用 <think> 标签包裹 ⇒ 必须拆包
//    （思考进推理正文位、正文留 content）。
//   - usage：OpenAI 形 / 原生 V2 无缓存命中/写入字段 ⇒ 记 0；Anthropic 形 input_tokens 是残值（同 Claude）。
//   - finish_reason 取值 stop / length / content_filter / tool_calls（流式 chunk 与非流式同一枚举口径）。
//
// previous_response_id：OpenAI 形 / V2 支持 = true（3.5 表 MiniMax ✅）；Anthropic 形的 wire 无此字段
// ⇒ 按不支持处理（⛔ 不静默丢弃）。

// ---- 共享：base_resp 与 thinking 拆包 ----

// minimaxBaseResp 是错误对象 base_resp（4.3.11：HTTP 200 + 业务错误码并存）。
type minimaxBaseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// minimaxCheckBaseResp 必检 base_resp：status_code != 0 ⇒ errors.FromMiniMaxBaseResp（4.3.11）。
func minimaxCheckBaseResp(b *minimaxBaseResp) *core.ErrorDetail {
	if b == nil || b.StatusCode == 0 {
		return nil
	}
	return gwerrors.FromMiniMaxBaseResp(b.StatusCode, b.StatusMsg)
}

// thinkOpen / thinkClose 是 MiniMax thinking 标签（reasoning_split=false 时思考并入 content，
// 4.3.11）。字面量用十六进制转义写死：写入链路会静默吞掉尖括号闭合形态的字面量
// （2026-11-14 实测——同文件多处字符串因此未终止，函数尾部整段丢失）。
const (
	thinkOpen  = "\x3cthink\x3e"
	thinkClose = "\x3c/think\x3e"
)

// splitThinkingTags 拆 content 里 thinkOpen…thinkClose 段包裹的思考（reasoning_split=false 时
// 思考并入 content，4.3.11）：所有完整段拆出（思考进推理正文位），正文保留其余部分；
// 未闭合的开标签段整段按思考收尾（不丢内容）。跨增量的半截标签由 thinkSplitter 处理。
func splitThinkingTags(content string) (thinking, text string) {
	var tb, txt strings.Builder
	rest := content
	for {
		i := strings.Index(rest, thinkOpen)
		if i < 0 {
			txt.WriteString(rest)
			break
		}
		txt.WriteString(rest[:i])
		rest = rest[i+len(thinkOpen):]
		j := strings.Index(rest, thinkClose)
		if j < 0 {
			tb.WriteString(rest)
			rest = ""
			break
		}
		tb.WriteString(rest[:j])
		rest = rest[j+len(thinkClose):]
	}
	return tb.String(), txt.String()
}

// thinkSplitter 是流式 thinking 拆包状态机：处理跨增量的 thinkOpen / thinkClose 标签边界。
type thinkSplitter struct {
	inThink bool
	pending string
}

// Feed 吃进一个增量，吐出（思考增量, 正文增量）；半截标签留在缓冲等下一片。
func (s *thinkSplitter) Feed(delta string) (thinking, text string) {
	s.pending += delta
	var tb, txt strings.Builder
	for {
		if !s.inThink {
			i := strings.Index(s.pending, thinkOpen)
			if i < 0 {
				k := partialTagTail(s.pending, thinkOpen)
				txt.WriteString(s.pending[:len(s.pending)-k])
				s.pending = s.pending[len(s.pending)-k:]
				break
			}
			txt.WriteString(s.pending[:i])
			s.pending = s.pending[i+len(thinkOpen):]
			s.inThink = true
			continue
		}
		j := strings.Index(s.pending, thinkClose)
		if j < 0 {
			k := partialTagTail(s.pending, thinkClose)
			tb.WriteString(s.pending[:len(s.pending)-k])
			s.pending = s.pending[len(s.pending)-k:]
			break
		}
		tb.WriteString(s.pending[:j])
		s.pending = s.pending[j+len(thinkClose):]
		s.inThink = false
	}
	return tb.String(), txt.String()
}

// Flush 收口：未闭合的 thinkOpen 段按思考收尾（不丢内容）。
func (s *thinkSplitter) Flush() (thinking, text string) {
	if s.inThink {
		return s.pending, ""
	}
	return "", s.pending
}

// partialTagTail 返回 s 尾部与 tag 前缀重合的最大长度（半截标签检测）。
func partialTagTail(s, tag string) int {
	max := len(tag) - 1
	if len(s) < max {
		max = len(s)
	}
	for k := max; k > 0; k-- {
		if strings.HasSuffix(s, tag[:k]) {
			return k
		}
	}
	return 0
}

// ---- ① OpenAI 形（chat/completions）----

type minimaxAdapter struct {
	hc *http.Client
}

// NewMiniMax 构造 MiniMax OpenAI 形适配器（hc 为出站 HTTP 客户端）。
func NewMiniMax(hc *http.Client) Adapter { return &minimaxAdapter{hc: hc} }

func (a *minimaxAdapter) Provider() core.Provider          { return core.ProviderMiniMax }
func (a *minimaxAdapter) OutboundShape() OutboundShape     { return ShapeChatCompletions }
func (a *minimaxAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 4.3.11：仅 Authorization: Bearer（⛔ 无 GroupId）。
func (a *minimaxAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize：OpenAI 形 usage 无缓存命中/写入字段 ⇒ 记 0（4.4.2 MiniMax 行）。
func (a *minimaxAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderMiniMax, usageTreeShape(raw), raw)
}

// minimaxMessage 是 chat 形消息（4.3.11：reasoning_content / reasoning_split 相关字段）。
type minimaxMessage struct {
	Role             string            `json:"role"`
	Content          string            `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []minimaxToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
}

type minimaxToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// minimaxChatMessages 映射 chat 形消息序列（原生 V2 共用；OpenAI 形改走 SDK 消息参数）。
func minimaxChatMessages(r *core.Request) []minimaxMessage {
	var out []minimaxMessage
	for _, m := range r.Messages {
		msg := minimaxMessage{Role: m.Role, Content: contentText(m.Content), ToolCallID: m.ToolCallID}
		if m.Role == "tool" {
			out = append(out, msg)
			continue
		}
		for _, tc := range m.ToolCalls {
			var c minimaxToolCall
			c.ID = tc.ID
			c.Type = "function"
			c.Function.Name = tc.Name
			c.Function.Arguments = tc.Arguments
			msg.ToolCalls = append(msg.ToolCalls, c)
		}
		out = append(out, msg)
	}
	return out
}

// ---- SDK 构造（openai-go/v3 typed 参数，3.6 / 10.3）----

// minimaxSDKClient 构造 openai-go 客户端（Tier 1：流式执行经 SDK 客户端，自定义 base URL +
// 注入既有 *http.Client）：
//   - WithBaseURL：用识别结果的 base（SDK 内部拼 chat/completions，与 joinURL 等价）；
//   - WithHTTPClient：注入既有出站 HTTP 客户端（凭据由 authTransport 在 RoundTrip 按 ctx
//     注入，⛔ 不进适配器也不进 SDK，8.1 / 8.4）；
//   - WithAPIKey("")：⛔ 不读 OPENAI_API_KEY 等环境变量（凭据只经安全层注入，21）；
//   - WithMaxRetries(0)：⛔ SDK 自带重试不许生效 —— 重试与降级分别计数、由网关统一管（24）；
//     SDK 默认 MaxRetries=2 会把一次上游失败变成多次上游调用。
func minimaxSDKClient(hc *http.Client, baseURL string) openai.Client {
	return openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(hc),
		option.WithAPIKey(""),
		option.WithMaxRetries(0),
	)
}

// minimaxSDKMapErr 把 SDK 错误映射回 DetailError（与 streamRequest 的错误口径逐项对齐，7.1）：
// HTTP 错误 ⇒ errors.FromUpstream（保留上游状态码 / 请求 ID / 响应体；SDK 把错误响应体回填
// 进 Response.Body，可原样读出）；其余（传输失败 / 取消）⇒ errors.FromTransportError。
func minimaxSDKMapErr(p core.Provider, err error) error {
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
		return &DetailError{Detail: gwerrors.FromUpstream(p, ae.StatusCode, body, rid)}
	}
	return &DetailError{Detail: gwerrors.FromTransportError(p, err, "")}
}

// minimaxSDKMessages 映射消息序列到 SDK 消息参数（wire 与 minimaxChatMessages 逐字段一致）。
// SDK typed 表达不了的两处既有 wire 用 SetExtraFields 补（quirks 不许丢）：
// ① 非 assistant 消息带 tool_calls（既有 wire 任意角色原样带）；② role=tool 且无 tool_call_id
// 时省略该键（既有 omitempty 口径，SDK 的 tool_call_id 是必发字段）。
func minimaxSDKMessages(r *core.Request) []openai.ChatCompletionMessageParamUnion {
	var out []openai.ChatCompletionMessageParamUnion
	for _, m := range r.Messages {
		text := contentText(m.Content)
		if m.Role == "tool" {
			msg := openai.ToolMessage(text, m.ToolCallID)
			if m.ToolCallID == "" && msg.OfTool != nil {
				msg.OfTool.SetExtraFields(map[string]any{"tool_call_id": param.Omit})
			}
			out = append(out, msg)
			continue
		}
		var calls []minimaxToolCall
		for _, tc := range m.ToolCalls {
			var c minimaxToolCall
			c.ID = tc.ID
			c.Type = "function"
			c.Function.Name = tc.Name
			c.Function.Arguments = tc.Arguments
			calls = append(calls, c)
		}
		var msg openai.ChatCompletionMessageParamUnion
		switch m.Role {
		case "system":
			msg = openai.SystemMessage(text)
		case "developer":
			msg = openai.DeveloperMessage(text)
		case "assistant":
			msg = openai.ChatCompletionMessageParamOfAssistant(text)
		default:
			msg = openai.UserMessage(text)
		}
		if len(calls) > 0 {
			if msg.OfAssistant != nil {
				for _, c := range calls {
					msg.OfAssistant.ToolCalls = append(msg.OfAssistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: c.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Arguments: c.Function.Arguments, Name: c.Function.Name,
							},
						},
					})
				}
			} else if msg.OfUser != nil {
				msg.OfUser.SetExtraFields(map[string]any{"tool_calls": calls})
			} else if msg.OfSystem != nil {
				msg.OfSystem.SetExtraFields(map[string]any{"tool_calls": calls})
			} else if msg.OfDeveloper != nil {
				msg.OfDeveloper.SetExtraFields(map[string]any{"tool_calls": calls})
			}
		}
		out = append(out, msg)
	}
	return out
}

// minimaxSDKTools 映射工具声明（wire 与既有 type:function + function{name,...} 形一致）。
// parameters 是 JSON Schema：`{}` ⇒ 空非 nil map ⇒ 仍发 `"parameters":{}`（与既有
// omitempty RawMessage 口径等价）；nil ⇒ 不发该键。
func minimaxSDKTools(r *core.Request) ([]openai.ChatCompletionToolUnionParam, error) {
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

// minimaxChatParams 用 SDK typed 参数构造 OpenAI 形请求（4.3.11 关键字段逐字）：
// ⛔ 不设 temperature / top_p（不支持，传值无效）；⛔ 无 GroupId；tool_choice 仅 auto
// （非 auto ⇒ 降级记录）。thinking.type / reasoning_split / service_tier 等特异参数经
// Extra 走 mergeExtraBody 黑名单口径（quirks 继续走现有通道），⛔ 不进 typed 参数。
func minimaxChatParams(r *core.Request, res *core.Resolution) (openai.ChatCompletionNewParams, []core.Degradation, error) {
	var body openai.ChatCompletionNewParams
	if err := checkPreviousResponseID(true, r); err != nil {
		return body, nil, err
	}
	body.Model = openai.ChatModel(res.Model)
	body.Messages = minimaxSDKMessages(r)
	tools, err := minimaxSDKTools(r)
	if err != nil {
		return body, nil, err
	}
	body.Tools = tools
	if r.MaxOutputTokens > 0 {
		body.MaxCompletionTokens = param.NewOpt(int64(r.MaxOutputTokens))
	}
	var degs []core.Degradation
	if r.ToolChoice != nil {
		// 4.3.11：tool_choice 仅 auto ⇒ 非 auto 降为 auto 并记降级（⛔ 不静默）。
		if r.ToolChoice.Mode != "auto" {
			degs = append(degs, core.Degradation{
				Field:  "tool_choice",
				Reason: core.DegradationCapabilityUnsupported,
				Detail: "MiniMax 仅支持 tool_choice=auto，已降为 auto",
			})
		}
		body.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("auto")}
	}
	// SDK 的请求参数无 Stream 字段（流式由 SDK 客户端注入）、messages 空时整键省略（omitzero）；
	// 既有 wire 是流式发 `"stream":true`、空消息发 `"messages":null`（无 omitempty）
	// ⇒ SetExtraFields 补齐/补回，保持 wire 一致。
	extras := map[string]any{}
	if r.Stream {
		body.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: param.NewOpt(true)}
		extras["stream"] = true
	}
	if len(body.Messages) == 0 {
		extras["messages"] = nil
	}
	if len(extras) > 0 {
		body.SetExtraFields(extras)
	}
	return body, degs, nil
}

// BuildRequestWithDegradations 构造请求并返回降级记录（tool_choice 仅 auto，4.3.11）。
func (a *minimaxAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	body, degs, err := minimaxChatParams(r, res)
	if err != nil {
		return nil, nil, err
	}
	raw, err := body.MarshalJSON()
	if err != nil {
		return nil, nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_marshal_failed", err.Error())}
	}
	// thinking.type / reasoning_split / service_tier 等特异参数经 Extra 透传（4.3.11）。
	// ⛔ temperature / top_p / GroupId 已被 Extra 黑名单剔除（4.3.11 不支持 / 旧版遗留）。
	raw, err = mergeExtraBody(raw, r.Extra)
	if err != nil {
		return nil, nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/chat/completions"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, degs, nil
}

// BuildRequest 委托 BuildRequestWithDegradations。
func (a *minimaxAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// minimaxChatResp 是 OpenAI 形 / V2 共用的响应形状（choices[] + base_resp）。
type minimaxChatResp struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int             `json:"index"`
		Message      minimaxMessage  `json:"message"`
		Delta        *minimaxMessage `json:"delta"`
		FinishReason string          `json:"finish_reason"`
	} `json:"choices"`
	Usage           json.RawMessage  `json:"usage"`
	BaseResp        *minimaxBaseResp `json:"base_resp"`
	InputSensitive  bool             `json:"input_sensitive"`
	OutputSensitive bool             `json:"output_sensitive"`
}

// minimaxParseChat 是 OpenAI 形 / V2 共用响应解析（4.3.11）：base_resp 必检 ⇒ 拆包 ⇒ 超集 finish_reason。
func minimaxParseChat(usageFn func(json.RawMessage) *core.Usage, body []byte) (*core.Response, error) {
	var r minimaxChatResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{ID: r.ID, Model: r.Model}
	// ⚠️ base_resp 必检（4.3.11：HTTP 200 + status_code != 0 ⇒ 业务错误）。
	if d := minimaxCheckBaseResp(r.BaseResp); d != nil {
		out.Status = core.StatusFailed
		out.Error = d
		return out, nil
	}
	out.Status = core.StatusCompleted
	if len(r.Choices) > 0 {
		c := r.Choices[0]
		thinking, text := splitThinkingTags(c.Message.Content)
		if c.Message.ReasoningContent != "" {
			if thinking != "" {
				thinking = c.Message.ReasoningContent + "\n" + thinking
			} else {
				thinking = c.Message.ReasoningContent
			}
		}
		oi := core.OutputItem{
			Type: "message", Role: "assistant",
			Content:       []core.ContentPart{{Type: "text", Text: text}},
			ReasoningText: thinking,
		}
		out.OutputItems = append(out.OutputItems, oi)
		out.Text = text
		for _, tc := range c.Message.ToolCalls {
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "function_call", ID: tc.ID, CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		// finish_reason 流式与非流式同一枚举口径（4.3.11）⇒ 超集原值。
		out.FinishReason = c.FinishReason
		if out.FinishReason == "" {
			out.FinishReason = core.FinishUnknown
		}
		if out.FinishReason == core.FinishLength {
			out.Status = core.StatusIncomplete
		}
	} else {
		out.FinishReason = core.FinishUnknown
	}
	if len(r.Usage) > 0 {
		out.Usage = usageFn(r.Usage)
	}
	return out, nil
}

// minimaxCompatProbe 是 SDK typed 解析之外的 quirk 原始探测（openai.ChatCompletion 不承载）：
// base_resp 必检（4.3.11）、choices[].message.reasoning_content（思考独立承载位）、
// 原始 usage（vendor 字段须逐字进 usage.Normalize，⛔ 不经 SDK 类型往返丢字段）。
type minimaxCompatProbe struct {
	BaseResp *minimaxBaseResp `json:"base_resp"`
	Usage    json.RawMessage  `json:"usage"`
	Choices  []struct {
		Message struct {
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
}

// ParseResponse 解析 OpenAI 形响应（Tier 1：SDK 响应类型解析 + quirk 原始探测，
// usage 走既有归一 usage.Normalize）。解析口径与 minimaxParseChat（V2 共用）逐项一致。
func (a *minimaxAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var probe minimaxCompatProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	var cc openai.ChatCompletion
	if err := json.Unmarshal(body, &cc); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{ID: cc.ID, Model: cc.Model}
	// ⚠️ base_resp 必检（4.3.11：HTTP 200 + status_code != 0 ⇒ 业务错误）。
	if d := minimaxCheckBaseResp(probe.BaseResp); d != nil {
		out.Status = core.StatusFailed
		out.Error = d
		return out, nil
	}
	out.Status = core.StatusCompleted
	if len(cc.Choices) > 0 {
		c := cc.Choices[0]
		thinking, text := splitThinkingTags(c.Message.Content)
		reasoning := ""
		if len(probe.Choices) > 0 {
			reasoning = probe.Choices[0].Message.ReasoningContent
		}
		if reasoning != "" {
			if thinking != "" {
				thinking = reasoning + "\n" + thinking
			} else {
				thinking = reasoning
			}
		}
		oi := core.OutputItem{
			Type: "message", Role: "assistant",
			Content:       []core.ContentPart{{Type: "text", Text: text}},
			ReasoningText: thinking,
		}
		out.OutputItems = append(out.OutputItems, oi)
		out.Text = text
		for _, tc := range c.Message.ToolCalls {
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "function_call", ID: tc.ID, CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		// finish_reason 流式与非流式同一枚举口径（4.3.11）⇒ 超集原值。
		out.FinishReason = c.FinishReason
		if out.FinishReason == "" {
			out.FinishReason = core.FinishUnknown
		}
		if out.FinishReason == core.FinishLength {
			out.Status = core.StatusIncomplete
		}
	} else {
		out.FinishReason = core.FinishUnknown
	}
	if len(probe.Usage) > 0 {
		out.Usage = a.ParseUsage(probe.Usage)
	}
	return out, nil
}

// minimaxChatStreamDecoder 是 OpenAI 形 / V2 共用帧解码器（SSE chat chunk + [DONE]；含 thinking 拆包）。
type minimaxChatStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	split   thinkSplitter
	toolSeq int
	// pendingDone 暂存 finish 状态（⚠️ 2026-11-14 修复：finish 帧不立即收口——
	// include_usage 的独立 usage 帧在 finish 帧与 [DONE] 哨兵之间（4.1.3），
	// 提前 return 会把 usage 丢掉 ⇒ 末帧 usage 缺失被置 Estimated 估算、计量不准。
	// 现口径：finish 状态暂存，usage 帧（同帧或后帧）先发、Done 后发；
	// [DONE] 时仍无 usage ⇒ 补发 Done 收口，⛔ 不丢终止帧）。
	pendingDone string
}

func newMinimaxChatStreamDecoder(usageFn func(json.RawMessage) *core.Usage) *minimaxChatStreamDecoder {
	return &minimaxChatStreamDecoder{usageFn: usageFn}
}

func (d *minimaxChatStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if data == "[DONE]" {
		if d.pendingDone != "" {
			st := d.pendingDone
			d.pendingDone = ""
			return []stream.Frame{{Kind: stream.FrameDone, Status: st}}, true, nil
		}
		return nil, true, nil
	}
	var r minimaxChatResp
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		return nil, false, nil
	}
	// ⚠️ base_resp 必检（流式 chunk 同样携带，4.3.11）。
	if det := minimaxCheckBaseResp(r.BaseResp); det != nil {
		return []stream.Frame{{Kind: stream.FrameError, Status: det.Code, Text: det.Message}}, true, nil
	}
	var frames []stream.Frame
	if len(r.Choices) > 0 {
		c := r.Choices[0]
		if c.Delta != nil {
			if c.Delta.Content != "" {
				thinking, text := d.split.Feed(c.Delta.Content)
				if thinking != "" {
					frames = append(frames, stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: thinking})
				}
				if text != "" {
					frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: text})
				}
			}
			if c.Delta.ReasoningContent != "" {
				frames = append(frames, stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: c.Delta.ReasoningContent})
			}
			for _, tc := range c.Delta.ToolCalls {
				id := tc.ID
				if id == "" {
					d.toolSeq++
					id = "call_" + strings.Repeat("x", 1) + json.Number(string(rune('0'+d.toolSeq%10))).String()
				}
				frames = append(frames, stream.Frame{
					Kind: stream.FrameToolCallArgsDelta, ToolCallID: id,
					// Text 载函数名（帧承载约定）：OpenAI 形 chunk 通常只在首块带
					// function.name ⇒ 每帧带上已有值，无名的块 Text 为空、消费端只在非空时赋值。
					Text: tc.Function.Name, Arguments: tc.Function.Arguments,
				})
			}
		}
		if c.FinishReason != "" {
			// 收口：未闭合 thinking 段先吐出。
			if th, tx := d.split.Flush(); th != "" || tx != "" {
				if th != "" {
					frames = append(frames, stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: th})
				}
				if tx != "" {
					frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: tx})
				}
			}
			// finish 状态暂存（usage 帧先于 Done，见 pendingDone 注释）；
			// 同帧带 usage 时由下方 usage 分支立即收口。
			d.pendingDone = c.FinishReason
		}
	}
	if len(r.Usage) > 0 {
		frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.usageFn(r.Usage)})
		if d.pendingDone != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: d.pendingDone})
			d.pendingDone = ""
			return frames, true, nil
		}
	}
	return frames, false, nil
}

// ---- OpenAI 形流式（Tier 1：SDK 客户端流式执行 + SDK 流事件 ⇒ stream.Frame）----

// minimaxSDKChunkProbe 是流式 chunk 的 quirk 原始探测（openai.ChatCompletionChunk 不承载）：
// base_resp 必检（4.3.11 流式 chunk 同样携带）、delta.reasoning_content、原始 usage。
type minimaxSDKChunkProbe struct {
	BaseResp *minimaxBaseResp `json:"base_resp"`
	Usage    json.RawMessage  `json:"usage"`
	Choices  []struct {
		Delta struct {
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
}

// minimaxSDKChunkDecoder 把 SDK 流事件映射为内部帧（帧序列与 minimaxChatStreamDecoder 逐项
// 一致：thinking 拆包、tool id 合成、finish 帧提前收口）。
type minimaxSDKChunkDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	split   thinkSplitter
	toolSeq int
	// pendingDone 暂存 finish 状态（⚠️ 2026-11-14 修复：finish 帧不立即收口——
	// include_usage 的独立 usage 帧在 finish 与 [DONE] 之间，提前收口会丢 usage、
	// 末帧 usage 缺失被置 Estimated 估算；口径同 minimaxChatStreamDecoder.pendingDone：
	// usage 帧先发、Done 后发，流末由迭代器补收口）。
	pendingDone string
}

// takePendingDone 取走暂存的 finish 状态（流末补收口用；无暂存返回空串）。
func (d *minimaxSDKChunkDecoder) takePendingDone() string {
	st := d.pendingDone
	d.pendingDone = ""
	return st
}

func newMinimaxSDKChunkDecoder(usageFn func(json.RawMessage) *core.Usage) *minimaxSDKChunkDecoder {
	return &minimaxSDKChunkDecoder{usageFn: usageFn}
}

func (d *minimaxSDKChunkDecoder) decode(c openai.ChatCompletionChunk) ([]stream.Frame, bool) {
	var probe minimaxSDKChunkProbe
	_ = json.Unmarshal([]byte(c.RawJSON()), &probe)
	// ⚠️ base_resp 必检（流式 chunk 同样携带，4.3.11）。
	if det := minimaxCheckBaseResp(probe.BaseResp); det != nil {
		return []stream.Frame{{Kind: stream.FrameError, Status: det.Code, Text: det.Message}}, true
	}
	var frames []stream.Frame
	if len(c.Choices) > 0 {
		choice := c.Choices[0]
		if choice.Delta.Content != "" {
			thinking, text := d.split.Feed(choice.Delta.Content)
			if thinking != "" {
				frames = append(frames, stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: thinking})
			}
			if text != "" {
				frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: text})
			}
		}
		reasoning := ""
		if len(probe.Choices) > 0 {
			reasoning = probe.Choices[0].Delta.ReasoningContent
		}
		if reasoning != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: reasoning})
		}
		for _, tc := range choice.Delta.ToolCalls {
			id := tc.ID
			if id == "" {
				d.toolSeq++
				id = "call_" + strings.Repeat("x", 1) + json.Number(string(rune('0'+d.toolSeq%10))).String()
			}
			frames = append(frames, stream.Frame{
				Kind: stream.FrameToolCallArgsDelta, ToolCallID: id,
				// Text 载函数名（帧承载约定）：OpenAI 形 chunk 通常只在首块带
				// function.name ⇒ 每帧带上已有值，无名的块 Text 为空、消费端只在非空时赋值。
				Text: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		if choice.FinishReason != "" {
			// 收口：未闭合 thinking 段先吐出。
			if th, tx := d.split.Flush(); th != "" || tx != "" {
				if th != "" {
					frames = append(frames, stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: th})
				}
				if tx != "" {
					frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: tx})
				}
			}
			// ⚠️ 2026-11-14 修复（原「观察保留」实为 bug）：finish 帧即收口会把
			// [DONE] 前的独立 usage 帧与同帧 usage 都丢掉（4.4.2「include_usage → 末帧」
			// 落空 ⇒ usage 缺失被置 Estimated 估算、计量不准）⇒ finish 状态暂存，
			// usage 帧先发、Done 后发；流末由 minimaxStreamIter 补收口。
			d.pendingDone = choice.FinishReason
		}
	}
	if len(probe.Usage) > 0 {
		frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.usageFn(probe.Usage)})
		if d.pendingDone != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: d.pendingDone})
			d.pendingDone = ""
			return frames, true
		}
	}
	return frames, false
}

// minimaxStreamIter 是 SDK 流式帧迭代器（语义对齐 sseIter：ctx 取消即中断并关上游连接、
// 终态收口后关闭、序号自增补 Seq）。
type minimaxStreamIter struct {
	ctx     context.Context
	st      *ssestream.Stream[openai.ChatCompletionChunk]
	dec     *minimaxSDKChunkDecoder
	queue   []stream.Frame
	cur     stream.Frame
	err     error
	stop    bool
	closed  bool
	seqAuto int64
}

func (it *minimaxStreamIter) Next() bool {
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
			// 流末补收口：SDK 内部吞掉 [DONE] 哨兵、finish 帧又延后收口（decode 注释）
			// ⇒ 暂存的 Done 在流末补发，⛔ 不丢终止帧。
			if st := it.dec.takePendingDone(); st != "" {
				it.queue = append(it.queue, stream.Frame{Kind: stream.FrameDone, Status: st})
				it.stop = true
				continue
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
func (it *minimaxStreamIter) Frame() stream.Frame { return it.cur }

// Err 返回终态错误（nil 表示正常结束）。
func (it *minimaxStreamIter) Err() error { return it.err }

// close 关闭 SDK 流（连带关闭上游 body）；幂等。
func (it *minimaxStreamIter) close() {
	if !it.closed {
		it.closed = true
		_ = it.st.Close()
	}
}

// StreamStart（OpenAI 形，Tier 1）：强制 stream=true ⇒ SDK 客户端流式执行 ⇒ SDK 流事件映射帧。
// 请求体与 BuildRequest 的 Outbound.Body 同源（含 Extra quirks，经 SetJSON 原样发）。
func (a *minimaxAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
	rc := *r
	rc.Stream = true
	params, _, err := minimaxChatParams(&rc, res)
	if err != nil {
		return nil, err
	}
	raw, err := params.MarshalJSON()
	if err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_marshal_failed", err.Error())}
	}
	raw, err = mergeExtraBody(raw, rc.Extra)
	if err != nil {
		return nil, err
	}
	param.SetJSON(raw, &params)
	cl := minimaxSDKClient(a.hc, res.BaseURL)
	st := cl.Chat.Completions.NewStreaming(ctx, params)
	if err := st.Err(); err != nil {
		return nil, minimaxSDKMapErr(core.ProviderMiniMax, err)
	}
	return &minimaxStreamIter{ctx: ctx, st: st, dec: newMinimaxSDKChunkDecoder(a.ParseUsage)}, nil
}

// ---- ② Anthropic 形（复用 Anthropic 构造 / 解析 / 流式解码，3.6 允许复用）----

type minimaxAnthropicAdapter struct {
	hc *http.Client
}

// NewMiniMaxAnthropic 构造 MiniMax Anthropic 形适配器。
func NewMiniMaxAnthropic(hc *http.Client) Adapter { return &minimaxAnthropicAdapter{hc: hc} }

func (a *minimaxAnthropicAdapter) Provider() core.Provider          { return core.ProviderMiniMax }
func (a *minimaxAnthropicAdapter) OutboundShape() OutboundShape     { return ShapeAnthropicMessages }
func (a *minimaxAnthropicAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 2.3 鉴权头三形态表：MiniMax Anthropic 兼容 ⇒ x-api-key + anthropic-version。
func (a *minimaxAnthropicAdapter) AuthScheme() AuthScheme {
	return AuthScheme{
		HeaderName:   "x-api-key",
		Prefix:       "",
		ExtraHeaders: map[string]string{"anthropic-version": "2026-10-21"},
	}
}

// ParseUsage 委托 usage.Normalize（shape=APIShapeAnthropicMessages）：input_tokens 是残值同 Claude
// （4.3.11：MiniMax Anthropic 端点与 Claude 语义一致）。
func (a *minimaxAnthropicAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderMiniMax, core.APIShapeAnthropicMessages, raw)
}

// BuildRequestWithDegradations：复用 Anthropic 形构造（max_tokens 缺省补值降级，4.2.4）。
func (a *minimaxAnthropicAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
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

// BuildRequest 委托 BuildRequestWithDegradations。
func (a *minimaxAnthropicAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// minimaxAnthropicResp 叠加 base_resp（MiniMax 特有，Anthropic 形响应亦可能携带）。
type minimaxAnthropicResp struct {
	anthropicResp
	BaseResp *minimaxBaseResp `json:"base_resp"`
}

// ParseResponse：base_resp 必检 ⇒ 复用 Anthropic 形解析。
func (a *minimaxAnthropicAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var probe minimaxAnthropicResp
	if err := json.Unmarshal(body, &probe); err == nil {
		if d := minimaxCheckBaseResp(probe.BaseResp); d != nil {
			return &core.Response{Status: core.StatusFailed, Error: d}, nil
		}
	}
	return parseAnthropicMessage(a.ParseUsage, body)
}

// StreamStart（Anthropic 形）：复用 Anthropic 事件解码（message_delta 累计 usage 等，4.3.2）。
func (a *minimaxAnthropicAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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

// ---- ③ 原生 V2（/v1/text/chatcompletion_v2）----

type minimaxV2Adapter struct {
	hc *http.Client
}

// NewMiniMaxNativeV2 构造 MiniMax 原生 V2 适配器。
func NewMiniMaxNativeV2(hc *http.Client) Adapter { return &minimaxV2Adapter{hc: hc} }

func (a *minimaxV2Adapter) Provider() core.Provider          { return core.ProviderMiniMax }
func (a *minimaxV2Adapter) OutboundShape() OutboundShape     { return ShapeMiniMaxNativeV2 }
func (a *minimaxV2Adapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 4.3.11：仅 Authorization: Bearer（⛔ 无 GroupId）。
func (a *minimaxV2Adapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（shape=APIShapeMiniMaxNativeV2）：chatcompletion_v2 的 usage
// 无缓存命中/写入字段 ⇒ 记 0（4.4.2 MiniMax 原生 V2 行）。
func (a *minimaxV2Adapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderMiniMax, core.APIShapeMiniMaxNativeV2, raw)
}

// minimaxV2Req 是原生 V2 请求体（4.3.11 关键字段逐字；⛔ 无 temperature / top_p / GroupId）。
type minimaxV2Req struct {
	Model               string           `json:"model"`
	Messages            []minimaxMessage `json:"messages"`
	MaxCompletionTokens int              `json:"max_completion_tokens,omitempty"`
	Stream              bool             `json:"stream,omitempty"`
	StreamOptions       any              `json:"stream_options,omitempty"`
}

// BuildRequestWithDegradations 构造 V2 请求（tool_choice 仅 auto 同 4.3.11 口径）。
func (a *minimaxV2Adapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, nil, err
	}
	body := minimaxV2Req{
		Model:               res.Model,
		Messages:            minimaxChatMessages(r),
		MaxCompletionTokens: r.MaxOutputTokens,
		Stream:              r.Stream,
	}
	var degs []core.Degradation
	if r.Stream {
		body.StreamOptions = map[string]any{"include_usage": true}
	}
	if r.ToolChoice != nil && r.ToolChoice.Mode != "auto" {
		degs = append(degs, core.Degradation{
			Field:  "tool_choice",
			Reason: core.DegradationCapabilityUnsupported,
			Detail: "MiniMax 仅支持 tool_choice=auto，已降为 auto",
		})
	}
	raw, err := marshalBody(body)
	if err != nil {
		return nil, nil, err
	}
	raw, err = mergeExtraBody(raw, r.Extra)
	if err != nil {
		return nil, nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/text/chatcompletion_v2"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, degs, nil
}

// BuildRequest 委托 BuildRequestWithDegradations。
func (a *minimaxV2Adapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// ParseResponse：V2 响应形状同 chat 形 + base_resp（4.3.11 / 4.4.2 原生 V2 行）。
func (a *minimaxV2Adapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	return minimaxParseChat(a.ParseUsage, body)
}

// StreamStart（V2）：chat chunk SSE + [DONE]，末帧 usage（4.4.2：include_usage → 末帧）。
func (a *minimaxV2Adapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
	return newSSEIter(ctx, resp.Body, newMinimaxChatStreamDecoder(a.ParseUsage).decode, nil), nil
}
