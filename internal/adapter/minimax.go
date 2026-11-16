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
// ① OpenAI Responses 形（APIShapeOpenAIChat）：POST {base}/responses（官方 Create Response：
//    POST https://api.minimax.io/v1/responses，国内站 https://api.minimax.cn/v1/responses）。
//    装配 / 响应解析 / 流式解码走响应系共享机件（responsesBuildParams / parseResponsesBody /
//    newResponsesStreamDecoder，3.6：响应系共用 openai-go/v3）；quirks（GroupId 禁发、
//    temperature/top_p 剔除、base_resp 必检、tool_choice 降级记录）继续独立处理。
// ② Anthropic 形（APIShapeAnthropicMessages）：POST {base}/messages（base = api.minimax.io/anthropic，
//    4.1.2 /anthropic/v1/messages）；复用 Anthropic 解析器（3.6：适配器可复用），input_tokens 残值同 Claude。
// ③ 原生 V2（APIShapeMiniMaxNativeV2）：POST {base}/text/chatcompletion_v2（4.3.11 逐字）。
//
// 形态共性（4.3.11）：
//   - ⛔ 不传 GroupId / group_id（旧版遗留口径，沿用会引入无效参数）。
//   - ⚠️ temperature / top_p 不受支持（传值无效，恒为 1 / 0.95）⇒ 构造时剔除。
//   - ⚠️ base_resp 必检：HTTP 200 + status_code != 0 是业务错误 ⇒ errors.FromMiniMaxBaseResp
//     （⛔ 不检查 base_resp 就会把错误当成功）。
//   - reasoning_split=false 时思考并入 content 且用 <think> 标签包裹 ⇒ 必须拆包
//    （思考进推理正文位、正文留 content）。⚠️ 仅 chat 形（③）承载——Responses 形（①）的推理
//    走 reasoning item / reasoning_text 事件（官方 Create Response 示例），⛔ 不拆标签。
//   - usage：① 按实际 usage 树选口径（usageTreeShape）；③ 无缓存命中/写入字段 ⇒ 记 0；
//    ② input_tokens 是残值（同 Claude）。
//   - finish_reason 取值 stop / length / content_filter / tool_calls（流式 chunk 与非流式同一枚举口径）。
//
// previous_response_id：① /responses 按不支持处理（官方 Create Response schema 明言「请求中暂不支持
// 该参数，传入后将被忽略，响应中固定返回 null」——上游静默吞参 ⇒ 统一 SDD 3.5 显式报错）；
// ③ 支持 = true（3.5 表）；② 的 wire 无此字段 ⇒ 按不支持处理（⛔ 不静默丢弃）。

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

func (a *minimaxAdapter) Provider() core.Provider      { return core.ProviderMiniMax }
func (a *minimaxAdapter) OutboundShape() OutboundShape { return ShapeResponses }

// SupportsPreviousResponseID=false：官方 Create Response schema 明言 previous_response_id
// 「请求中暂不支持该参数，传入后将被忽略，响应中固定返回 null」（上游静默吞参）⇒ 3.5：
// 按不支持处理、显式报错（⛔ 不静默丢弃；出处 platform.minimax.cn/docs/api-reference/responses-create）。
func (a *minimaxAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 4.3.11：仅 Authorization: Bearer（⛔ 无 GroupId）。
func (a *minimaxAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4）：按实际 usage 树选口径（usageTreeShape）——Chat 树
// 无缓存命中/写入字段 ⇒ 记 0（4.4.2 MiniMax 行）；Responses 树按 input_tokens_details 归一。
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

// minimaxChatMessages 映射 chat 形消息序列（原生 V2 使用；① Responses 形走共享装配 responsesBuildParams）。
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

// ---- 请求体（Responses 形；装配走共享机件 responsesBuildParams，3.6）----

// minimaxBaseRespProbe 探测 base_resp（4.3.11 形态共性：HTTP 200 + status_code != 0 ⇒ 业务错误，
// ⛔ 不检查 base_resp 就会把错误当成功）。响应对象顶层与流事件的 response 载荷两处都探：
// chat 形 chunk 的 base_resp 在顶层（4.3.11 实证口径），/responses 官方示例未见该字段、无 key 实测
// ⇒ 任务书「base_resp 错误处理保持现状」，保守两处都探（缺失时按正常路径解析）。
type minimaxBaseRespProbe struct {
	ID       string           `json:"id"`
	Model    string           `json:"model"`
	BaseResp *minimaxBaseResp `json:"base_resp"`
	Response *struct {
		BaseResp *minimaxBaseResp `json:"base_resp"`
	} `json:"response"`
}

// baseRespOf 取 base_resp（响应对象顶层优先，流事件退回 response 载荷内）。
func (p *minimaxBaseRespProbe) baseRespOf() *minimaxBaseResp {
	if p.BaseResp != nil {
		return p.BaseResp
	}
	if p.Response != nil {
		return p.Response.BaseResp
	}
	return nil
}

// BuildRequestWithDegradations 构造 POST {base}/responses 请求并返回降级记录。
// 装配走共享机件 responsesBuildParams（3.6：响应系共用 openai-go/v3）；家差：
//   - tool_choice 仅 auto（官方 Create Response schema `enum: [auto]`；4.3.11 chat 形同口径）：
//     非 auto 降为 auto 并记降级（⛔ 不静默）；
//   - thinking.type / reasoning_split / service_tier 等特异参数经 Extra 透传（mergeExtraBody），
//     ⛔ temperature / top_p / GroupId 已被 Extra 黑名单剔除（4.3.11 不支持 / 旧版遗留）。
func (a *minimaxAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	// previous_response_id：官方 Create Response schema 明言「请求中暂不支持该参数，传入后将被
	// 忽略，响应中固定返回 null」（上游静默吞参）⇒ 统一 SDD 3.5：显式报错（⛔ 不静默丢弃）。
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, nil, err
	}
	p, err := responsesBuildParams(r, res)
	if err != nil {
		return nil, nil, err
	}
	var degs []core.Degradation
	if r.ToolChoice != nil {
		// tool_choice 仅 auto（官方 Create Response schema enum 仅 auto；4.3.11 chat 形同口径）
		// ⇒ 非 auto 降为 auto 并记降级（⛔ 不静默）。
		if r.ToolChoice.Mode != "auto" {
			degs = append(degs, core.Degradation{
				Field:  "tool_choice",
				Reason: core.DegradationCapabilityUnsupported,
				Detail: "MiniMax 仅支持 tool_choice=auto，已降为 auto",
			})
		}
		p.ToolChoice = openAIToolChoiceParam(&core.ToolChoice{Mode: "auto"})
	}
	raw, err := marshalBody(p)
	if err != nil {
		return nil, nil, err
	}
	// thinking.type / reasoning_split / service_tier 等特异参数经 Extra 透传（4.3.11）。
	raw, err = mergeExtraBody(raw, r.Extra)
	if err != nil {
		return nil, nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/responses"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, degs, nil
}

// BuildRequest 委托 BuildRequestWithDegradations（降级记录由上层合并进 x_gateway.degradations[]）。
func (a *minimaxAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// minimaxChatResp 是 chat 形响应形状（choices[] + base_resp；③ V2 解析使用）。
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

// minimaxParseChat 是 chat 形响应解析（4.3.11；③ V2 使用）：base_resp 必检 ⇒ 拆包 ⇒ 超集 finish_reason。
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

// ParseResponse 解析 Responses 响应对象 → 规范 Response（解析走共享机件 parseResponsesBody）。
// 家差特殊解析：base_resp 必检（4.3.11 形态共性 + 任务书「base_resp 错误处理保持现状」；⚠️ /responses
// 官方示例未见 base_resp、无 key 实测 ⇒ 保守保留探测，缺失时按正常路径解析）。
func (a *minimaxAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var probe minimaxBaseRespProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	if d := minimaxCheckBaseResp(probe.baseRespOf()); d != nil {
		return &core.Response{ID: probe.ID, Model: probe.Model, Status: core.StatusFailed, Error: d}, nil
	}
	return parseResponsesBody(a.ParseUsage, body)
}

// minimaxChatStreamDecoder 是 chat 形帧解码器（SSE chat chunk + [DONE]；含 thinking 拆包；③ V2 流式使用）。
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

// ---- Responses 形流式（共享解码 newResponsesStreamDecoder；家差：base_resp 必检包裹）----

// streamDecoder 返回 Responses 形帧解码器（共享机件，3.7 容忍口径见 openAIStreamDecoder）。
func (a *minimaxAdapter) streamDecoder() *openAIStreamDecoder {
	return newResponsesStreamDecoder(a.ParseUsage)
}

// minimaxStreamDecode 包裹共享解码器：每帧先探 base_resp（4.3.11「流式 chunk 同样携带」+ 任务书
// 「base_resp 错误处理保持现状」；⚠️ /responses 官方 SSE 示例未见 base_resp ⇒ 保守沿用 chat 形口径），
// 业务错误 ⇒ 单帧 FrameError + 终态收口（⛔ 不静默吞掉）；其余帧原样交共享解码器。
func minimaxStreamDecode(dec func(stream.SSEEvent) ([]stream.Frame, bool, error)) func(stream.SSEEvent) ([]stream.Frame, bool, error) {
	return func(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
		var probe minimaxBaseRespProbe
		if json.Unmarshal([]byte(ev.Data), &probe) == nil {
			if det := minimaxCheckBaseResp(probe.baseRespOf()); det != nil {
				return []stream.Frame{{Kind: stream.FrameError, Status: det.Code, Text: det.Message}}, true, nil
			}
		}
		return dec(ev)
	}
}

// StreamStart：强制 stream=true ⇒ BuildRequest ⇒ 真实 HTTP ⇒ SSE 解码（照 openai.go；
// base_resp 必检包裹为 MiniMax 家差）。
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
	return newSSEIter(ctx, resp.Body, minimaxStreamDecode(a.streamDecoder().decode), nil), nil
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
