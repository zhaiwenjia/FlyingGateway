// 日期: 2026-11-14 16:38:51.679
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// 智谱 GLM / BigModel 适配器（统一 SDD 4.3.8）：
// 出站 Chat Completions：POST {base}/chat/completions（真实流见 testdata/real/zhipu_stream_docs_example.txt）。
// AuthScheme: Authorization + "Bearer "。
// ⚠️ 流式请求参数是 stream_options.chunk_include_usage（⛔ 不是 include_usage，4.3.8 逐字：
// 照搬 include_usage 会静默拿不到 usage）；chunk_include_usage 开启后逐块返回**累计** usage（⛔ 不可相加）。
// reasoning_effort 是智谱的推理档位顶层字段（4.3.8 取值 max/xhigh/high/medium/low/minimal/none）；
// thinking.type / thinking.clear_thinking 经 core.Request.Extra 透传。
// previous_response_id：chat 形 wire 无此字段 ⇒ 按不支持处理（3.5：⛔ 不静默丢弃，显式报错）。
// finish_reason 含 sensitive / network_error / model_context_window_exceeded（智谱独有 / 非标准取值）
// ⇒ core.FinishReason 超集原值（4.5.1 ⛔ 不得当作未知值丢弃）。
// 构造 / 解析 / 流事件解析走 openai-go/v3 typed 结构（统一 SDD 3.6 / 10.3；body []byte 签名不变）；
// 流式 HTTP 执行保留 streamRequest（Tier 2 兜底，见交付报告）。

type zhipuAdapter struct {
	hc *http.Client
}

// NewZhipu 构造智谱 GLM 适配器（hc 为出站 HTTP 客户端）。
func NewZhipu(hc *http.Client) Adapter { return &zhipuAdapter{hc: hc} }

func (a *zhipuAdapter) Provider() core.Provider          { return core.ProviderZhipu }
func (a *zhipuAdapter) OutboundShape() OutboundShape     { return ShapeChatCompletions }
func (a *zhipuAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 4.3.8：Authorization: Bearer。
func (a *zhipuAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4.2 智谱行：命中单列、无写入、completion 含 reasoning）。
func (a *zhipuAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderZhipu, usageTreeShape(raw), raw)
}

// ---- 请求体（Chat 形状 + 智谱特异字段）----

// zhipuChatMessages 映射 chat 形消息序列（角色分派与 reasoning_content 回放口径同 deepseek）。
func zhipuChatMessages(r *core.Request) []openai.ChatCompletionMessageParamUnion {
	var out []openai.ChatCompletionMessageParamUnion
	if r.Instructions != "" {
		out = append(out, openai.ChatCompletionMessageParamUnion{
			OfSystem: &openai.ChatCompletionSystemMessageParam{
				Content: openai.ChatCompletionSystemMessageParamContentUnion{OfString: param.NewOpt(r.Instructions)},
			},
		})
	}
	for _, m := range r.Messages {
		out = append(out, zhipuChatMessage(m))
	}
	return out
}

// zhipuChatMessage 映射单条规范消息（system / developer 角色原样保留；
// EncryptedContent 是 OpenAI 加密推理链（Responses 专属载体）⇒ chat 形无对应物，不发）。
func zhipuChatMessage(m core.Message) openai.ChatCompletionMessageParamUnion {
	switch m.Role {
	case "tool":
		return openai.ChatCompletionMessageParamUnion{
			OfTool: &openai.ChatCompletionToolMessageParam{
				ToolCallID: m.ToolCallID,
				Content:    openai.ChatCompletionToolMessageParamContentUnion{OfString: param.NewOpt(contentText(m.Content))},
			},
		}
	case "system", "developer":
		if m.Role == "developer" {
			return openai.ChatCompletionMessageParamUnion{
				OfDeveloper: &openai.ChatCompletionDeveloperMessageParam{
					Content: openai.ChatCompletionDeveloperMessageParamContentUnion{OfString: param.NewOpt(contentText(m.Content))},
				},
			}
		}
		return openai.ChatCompletionMessageParamUnion{
			OfSystem: &openai.ChatCompletionSystemMessageParam{
				Content: openai.ChatCompletionSystemMessageParamContentUnion{OfString: param.NewOpt(contentText(m.Content))},
			},
		}
	case "assistant":
		am := openai.ChatCompletionAssistantMessageParam{}
		if text := contentText(m.Content); text != "" {
			am.Content.OfString = param.NewOpt(text)
		}
		if m.ReasoningContent != "" {
			// reasoning_content 是智谱思考承载扩展名（逐字）⇒ SDK 类型无此字段，走 extras。
			am.SetExtraFields(map[string]any{"reasoning_content": m.ReasoningContent})
		}
		for _, tc := range m.ToolCalls {
			id := tc.CallID
			if id == "" {
				id = tc.ID
			}
			am.ToolCalls = append(am.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: id,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name: tc.Name, Arguments: tc.Arguments,
					},
				},
			})
		}
		return openai.ChatCompletionMessageParamUnion{OfAssistant: &am}
	default:
		um := openai.ChatCompletionUserMessageParam{}
		var parts []openai.ChatCompletionContentPartUnionParam
		for _, p := range m.Content {
			switch {
			case p.ImageURL != "":
				parts = append(parts, openai.ChatCompletionContentPartUnionParam{
					OfImageURL: &openai.ChatCompletionContentPartImageParam{
						ImageURL: openai.ChatCompletionContentPartImageImageURLParam{URL: p.ImageURL},
					},
				})
			case p.Text != "":
				parts = append(parts, openai.ChatCompletionContentPartUnionParam{
					OfText: &openai.ChatCompletionContentPartTextParam{Text: p.Text},
				})
			}
		}
		if len(parts) == 0 {
			um.Content.OfString = param.NewOpt("")
		} else if len(parts) == 1 && parts[0].OfText != nil {
			um.Content.OfString = param.NewOpt(parts[0].OfText.Text)
		} else {
			um.Content.OfArrayOfContentParts = parts
		}
		return openai.ChatCompletionMessageParamUnion{OfUser: &um}
	}
}

// zhipuChatTools 映射工具声明（chat 形 function 对象；parameters 原文透传）。
func zhipuChatTools(r *core.Request) []openai.ChatCompletionToolUnionParam {
	var out []openai.ChatCompletionToolUnionParam
	for _, t := range r.Tools {
		fd := shared.FunctionDefinitionParam{Name: t.Name}
		if t.Description != "" {
			fd.Description = param.NewOpt(t.Description)
		}
		if len(t.Parameters) > 0 {
			fd.SetExtraFields(map[string]any{"parameters": json.RawMessage(t.Parameters)})
		}
		out = append(out, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{Function: fd},
		})
	}
	return out
}

// BuildRequestWithDegradations 构造 /chat/completions 请求并返回降级记录（4.3.8 关键字段逐字）。
// ⚠️ 2026-11-14 修复：tool_choice 非 auto 被降为 auto 原先未记 degradation（静默降级，
// 违反 1.4.2 不静默降级三则之二）——现象：请求带 tool_choice=required 实际发出 auto，
// 响应无任何标记；根因：只实现了降级动作没接降级记录通道（minimax 同类降级有记，可对照）；
// 为什么不改成拒绝请求：4.3.8 明文「tool_choice 仅 auto」，降为 auto 是能力边界内的
// 既有口径，问题只在「丢意图不可见」⇒ 补 DegradationAware 通道（BuildRequestWithDegradations）；
// 不修会怎样：调用方以为 required 生效，工具未被强制调用，排查方向从一开始就跑偏。
func (a *zhipuAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, nil, err
	}
	p := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(res.Model),
		Messages: zhipuChatMessages(r),
	}
	if r.MaxOutputTokens > 0 {
		p.MaxTokens = param.NewOpt(int64(r.MaxOutputTokens))
	}
	if r.Reasoning != nil && r.Reasoning.Effort != "" {
		// reasoning_effort 是智谱的推理档位顶层字段（4.3.8：max/xhigh/high/medium/low/minimal/none）。
		p.ReasoningEffort = shared.ReasoningEffort(r.Reasoning.Effort)
	}
	p.Tools = zhipuChatTools(r)
	var degs []core.Degradation
	if r.ToolChoice != nil {
		// 4.3.8：tool_choice 仅 auto ⇒ 非 auto 一律降为 auto 并记 degradation（⛔ 不静默）。
		if r.ToolChoice.Mode != "auto" {
			degs = append(degs, core.Degradation{
				Field:  "tool_choice",
				Reason: core.DegradationCapabilityUnsupported,
				Detail: "智谱 GLM 仅支持 tool_choice=auto，已降为 auto",
			})
		}
		p.ToolChoice.OfAuto = param.NewOpt("auto")
	}
	ex := map[string]any{}
	if r.Stream {
		// ⚠️ chunk_include_usage（⛔ 不是 include_usage）：逐块返回累计 usage（4.3.8）。
		ex["stream"] = true
		ex["stream_options"] = map[string]any{"chunk_include_usage": true}
	}
	if len(ex) > 0 {
		p.SetExtraFields(ex)
	}
	raw, err := marshalBody(p)
	if err != nil {
		return nil, nil, err
	}
	// thinking.type / thinking.clear_thinking 等特异参数经 Extra 透传（4.3.8）。
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

// BuildRequest 委托 BuildRequestWithDegradations（降级记录由上层合并进 x_gateway.degradations[]）。
func (a *zhipuAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// ---- 响应体（chat.completion；智谱 finish_reason 非标准取值）----

// ParseResponse 解析 chat.completion 响应：SDK typed 解析（openai.ChatCompletion，3.6）；
// finish_reason 非标准取值 ⇒ 超集原值（sensitive / network_error /
// model_context_window_exceeded，4.5.1）；reasoning_content ⇒ 推理正文位。
func (a *zhipuAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var c openai.ChatCompletion
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{ID: c.ID, Model: c.Model, Status: core.StatusCompleted}
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		out.FinishReason = ch.FinishReason
		if out.FinishReason == "" {
			out.FinishReason = core.FinishUnknown
		}
		if out.FinishReason == core.FinishLength {
			out.Status = core.StatusIncomplete
		}
		var mq struct {
			ReasoningContent string `json:"reasoning_content"`
		}
		_ = json.Unmarshal([]byte(ch.Message.RawJSON()), &mq) // RawJSON 是已解析过的 JSON，仅取扩展字段
		oi := core.OutputItem{Type: "message", Role: "assistant", ReasoningText: mq.ReasoningContent}
		if ch.Message.Content != "" {
			oi.Content = []core.ContentPart{{Type: "text", Text: ch.Message.Content}}
		}
		out.OutputItems = append(out.OutputItems, oi)
		out.Text = ch.Message.Content
		for _, tc := range ch.Message.ToolCalls {
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "function_call", ID: tc.ID, CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
	} else {
		out.FinishReason = core.FinishUnknown
	}
	if raw := c.JSON.Usage.Raw(); raw != "" && raw != "null" {
		out.Usage = a.ParseUsage(json.RawMessage(raw))
	}
	return out, nil
}

// ---- 流式（chat.completion.chunk；真实样本 testdata/real/zhipu_stream_docs_example.txt 可直喂）----

// zhipuChatStreamDecoder 是智谱的 chat chunk 帧解码器（3.7：每家一个帧解码器）：
// delta.content ⇒ 文本增量；delta.reasoning_content（智谱扩展名逐字）⇒ 推理增量；
// delta.tool_calls ⇒ 工具帧（帧承载约定：Text 载函数名、ToolCallID 载调用 id、Arguments 载参数增量）；
// finish_reason 超集原值收口。chunk_include_usage 开启后逐块返回**累计** usage（⛔ 不可相加，
// 消费端取最后一次快照）⇒ 每块带 usage 就发 usage 帧。
type zhipuChatStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[int64]string // chunk tool_call.index → 调用 id（id 只在首块出现，后续块按 index 对齐）
	// pendingDone 暂存 finish 状态（延后收口口径见 decodeOne 注释）。
	pendingDone string
}

func (a *zhipuAdapter) streamDecoder() *zhipuChatStreamDecoder {
	return &zhipuChatStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[int64]string{}}
}

func (d *zhipuChatStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if !json.Valid([]byte(data)) && strings.Contains(data, "\n") {
		// 紧凑样本分流：厂商文档流示例常是「一行一个 chunk、行间无空行」形（如
		// testdata/real/zhipu_stream_docs_example.txt），SSE 规范会把多行 data 拼成一个负载
		// ⇒ 整负载非 JSON 时按行拆分逐行解，真实样本可直喂（2026-11-14 改形验收点）。
		var frames []stream.Frame
		for _, line := range strings.Split(data, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			fs, done, err := d.decodeOne(line)
			frames = append(frames, fs...)
			if err != nil || done {
				return frames, done, err
			}
		}
		return frames, false, nil
	}
	return d.decodeOne(data)
}

// decodeOne 解码单个 chunk 负载（[DONE] 哨兵 / 非 JSON 帧容忍）。
func (d *zhipuChatStreamDecoder) decodeOne(payload string) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(payload)
	if data == "" {
		return nil, false, nil
	}
	if data == "[DONE]" {
		// [DONE] 处补收口：延后 Done 仍未发（无 usage 的流）⇒ 补发，⛔ 不丢终止帧。
		if d.pendingDone != "" {
			st := d.pendingDone
			d.pendingDone = ""
			return []stream.Frame{{Kind: stream.FrameDone, Status: st}}, true, nil
		}
		return nil, true, nil
	}
	// 两段式按需解码（chatchunk.go：轻量严格解码 ⇒ 异型帧 typed 兜底，帧语义零变化）。
	c, ok := decodeChatChunk(data)
	if !ok {
		return nil, false, nil // 非 JSON 帧（keep-alive / 文档省略标记等）容忍跳过（3.7 流式容错）
	}
	var frames []stream.Frame
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		if ch.Delta.Content != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: ch.Delta.Content})
		}
		if ch.Delta.ReasoningContent != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: ch.Delta.ReasoningContent})
		}
		for _, tc := range ch.Delta.ToolCalls {
			frames = append(frames, stream.Frame{
				Kind: stream.FrameToolCallArgsDelta, ToolCallID: d.toolCallID(tc),
				Text: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		if ch.FinishReason != "" {
			// ⚠️ 2026-11-14 真实样本实测修复：finish 帧不立即收口——
			// 现象：MiMo 真实流的独立 usage 帧（choices:[] + usage）在 finish 帧之后、[DONE] 之前
			//（mimo_stream_real.sse），finish 即收口会把它丢掉 ⇒ usage 缺失被置 Estimated 估算；
			// 根因：文档说「末帧同帧带 usage」，真实 wire 是 finish(usage:null) ⇒ 独立 usage 帧两拍；
			// 为什么不改回同帧假设：Kimi 又是同帧形态（kimi_stream_real.sse）⇒ 延后收口两形态通吃；
			// 不修会怎样：usage 计量缺尾、成本审计对不上账。
			d.pendingDone = ch.FinishReason
		}
	}
	if u := d.usageOf(c); u != nil {
		frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: u})
		if d.pendingDone != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: d.pendingDone})
			d.pendingDone = ""
			return frames, true, nil
		}
	}
	return frames, false, nil
}

// toolCallID 取工具调用 id：chunk 的 id 只在首块出现，后续块按 index 对齐（⛔ 不让参数帧断链）；
// 全程无 id（畸形上游）⇒ 合成稳定的 per-index 占位 id。
func (d *zhipuChatStreamDecoder) toolCallID(tc chatChunkToolCall) string {
	if tc.ID != "" {
		d.toolIDs[tc.Index] = tc.ID
		return tc.ID
	}
	if id := d.toolIDs[tc.Index]; id != "" {
		return id
	}
	id := "call_idx_" + strconv.FormatInt(tc.Index, 10)
	d.toolIDs[tc.Index] = id
	return id
}

// usageOf 取 chunk 的 usage 原文（智谱命中单列字段原样，⛔ 不经类型重整）：
// usage 为 null 或缺键 ⇒ 不发 usage 帧。
func (d *zhipuChatStreamDecoder) usageOf(c chatChunk) *core.Usage {
	raw := chunkUsageRaw(c.Usage)
	if raw == nil {
		return nil
	}
	return d.usageFn(raw)
}

// StreamStart：强制 stream=true（请求体带 stream_options.chunk_include_usage）⇒ 真实 HTTP ⇒
// chat chunk SSE 解码。
func (a *zhipuAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
