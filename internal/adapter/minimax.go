// 日期: 2026-11-14 14:29:38.492
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

// MiniMax 适配器（统一 SDD 4.3.11，三形态按 core.Resolution.Shape 选）：
//
// ① OpenAI 形（APIShapeOpenAIChat）：POST {base}/chat/completions（4.3.11 端点路径逐字）。
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

type minimaxTool struct {
	Type     string `json:"type"`
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// minimaxChatReq 是 OpenAI 形请求体（4.3.11 关键字段逐字）。
// ⛔ 无 temperature / top_p 字段（不支持，传值无效）；⛔ 无 GroupId。
type minimaxChatReq struct {
	Model               string           `json:"model"`
	Messages            []minimaxMessage `json:"messages"`
	MaxCompletionTokens int              `json:"max_completion_tokens,omitempty"`
	Stream              bool             `json:"stream,omitempty"`
	StreamOptions       any              `json:"stream_options,omitempty"`
	Tools               []minimaxTool    `json:"tools,omitempty"`
	ToolChoice          any              `json:"tool_choice,omitempty"`
}

// minimaxChatMessages 映射 chat 形消息序列。
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

// BuildRequestWithDegradations 构造请求并返回降级记录（tool_choice 仅 auto，4.3.11）。
func (a *minimaxAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, nil, err
	}
	body := minimaxChatReq{
		Model:               res.Model,
		Messages:            minimaxChatMessages(r),
		MaxCompletionTokens: r.MaxOutputTokens,
		Stream:              r.Stream,
	}
	var degs []core.Degradation
	if r.Stream {
		body.StreamOptions = map[string]any{"include_usage": true}
	}
	for _, t := range r.Tools {
		tool := minimaxTool{Type: "function"}
		tool.Function = &struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}{Name: t.Name, Description: t.Description, Parameters: t.Parameters}
		body.Tools = append(body.Tools, tool)
	}
	if r.ToolChoice != nil {
		// 4.3.11：tool_choice 仅 auto ⇒ 非 auto 降为 auto 并记降级（⛔ 不静默）。
		if r.ToolChoice.Mode != "auto" {
			degs = append(degs, core.Degradation{
				Field:  "tool_choice",
				Reason: core.DegradationCapabilityUnsupported,
				Detail: "MiniMax 仅支持 tool_choice=auto，已降为 auto",
			})
		}
		body.ToolChoice = "auto"
	}
	raw, err := marshalBody(body)
	if err != nil {
		return nil, nil, err
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

// ParseResponse 解析 OpenAI 形响应。
func (a *minimaxAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	return minimaxParseChat(a.ParseUsage, body)
}

// minimaxChatStreamDecoder 是 OpenAI 形 / V2 共用帧解码器（SSE chat chunk + [DONE]；含 thinking 拆包）。
type minimaxChatStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	split   thinkSplitter
	toolSeq int
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
			frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: c.FinishReason})
			return frames, true, nil
		}
	}
	if len(r.Usage) > 0 {
		frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.usageFn(r.Usage)})
	}
	return frames, false, nil
}

// StreamStart（OpenAI 形）：强制 stream=true ⇒ 真实 HTTP ⇒ SSE 解码。
func (a *minimaxAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
