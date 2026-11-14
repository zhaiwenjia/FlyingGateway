// 日期: 2026-11-14 16:38:51.651
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

// DeepSeek 适配器（统一 SDD 4.3.4）：
// 出站 Chat Completions：POST {base}/chat/completions（真实流为 chat.completion.chunk 形，
// 官方文档逐帧示例见 testdata/real/deepseek_stream_real.sse）。
// AuthScheme: Authorization + "Bearer "。
// previous_response_id：chat 形 wire 无此字段 ⇒ 按不支持处理（3.5：⛔ 不静默丢弃，显式报错）。
// usage 是「命中 / 未命中」双字段（prompt_cache_hit_tokens 与 prompt_cache_miss_tokens，
// 4.3.4：⚠️ 唯一给双字段的家；prompt_cache_hit_tokens 与 prompt_tokens_details.cached_tokens 同值
// ⇒ 取其一 ⛔ 不累加）——归 usage.Normalize 的 DeepSeek 逐家口径。
// finish_reason 含 aborted / insufficient_system_resource（DeepSeek 独有，4.5.1）⇒ 超集原值。
// 构造 / 解析 / 流事件解析走 openai-go/v3 typed 结构（统一 SDD 3.6 / 10.3；body []byte 签名不变）；
// 流式 HTTP 执行保留 streamRequest（Tier 2 兜底，见交付报告）。

type deepseekAdapter struct {
	hc *http.Client
}

// NewDeepSeek 构造 DeepSeek 适配器（hc 为出站 HTTP 客户端）。
func NewDeepSeek(hc *http.Client) Adapter { return &deepseekAdapter{hc: hc} }

func (a *deepseekAdapter) Provider() core.Provider          { return core.ProviderDeepSeek }
func (a *deepseekAdapter) OutboundShape() OutboundShape     { return ShapeChatCompletions }
func (a *deepseekAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 4.3.4：Authorization: Bearer。
func (a *deepseekAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize：DeepSeek usage 树是 hit/miss 形（4.3.4 / 4.4.2 DeepSeek 行：
// prompt_tokens = 命中 + 未命中、缓存命中取其一不累加）⇒ 按实际字段选口径。
func (a *deepseekAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderDeepSeek, usageTreeShape(raw), raw)
}

// ---- 请求体（Chat 形状，SDK typed 参数：openai.ChatCompletionNewParams）----

// deepseekChatMessages 映射 chat 形消息序列：
//   - r.Instructions 并为首个 system 消息（Chat 侧由 messages 承载，core.Request 注释口径）；
//   - system / developer 角色原样保留（⛔ developer 是小米多出的角色，不许丢）；
//   - assistant 的 ReasoningContent 回放为 reasoning_content（DeepSeek 扩展名逐字）；
//     EncryptedContent 是 OpenAI 加密推理链（Responses 专属载体）⇒ chat 形无对应物，不发；
//   - 文本内容发纯字符串；用户消息带图时发 parts 数组（type=image_url 的标准 chat 部件）；
//   - 工具结果 ⇒ role=tool（tool_call_id + content）。
func deepseekChatMessages(r *core.Request) []openai.ChatCompletionMessageParamUnion {
	var out []openai.ChatCompletionMessageParamUnion
	if r.Instructions != "" {
		out = append(out, openai.ChatCompletionMessageParamUnion{
			OfSystem: &openai.ChatCompletionSystemMessageParam{
				Content: openai.ChatCompletionSystemMessageParamContentUnion{OfString: param.NewOpt(r.Instructions)},
			},
		})
	}
	for _, m := range r.Messages {
		out = append(out, deepseekChatMessage(m))
	}
	return out
}

// deepseekChatMessage 映射单条规范消息（角色分派见 deepseekChatMessages 注释）。
func deepseekChatMessage(m core.Message) openai.ChatCompletionMessageParamUnion {
	switch m.Role {
	case "tool":
		return openai.ChatCompletionMessageParamUnion{
			OfTool: &openai.ChatCompletionToolMessageParam{
				ToolCallID: m.ToolCallID,
				Content:    openai.ChatCompletionToolMessageParamContentUnion{OfString: param.NewOpt(contentText(m.Content))},
			},
		}
	case "system", "developer":
		// system / developer 角色原样保留（⛔ developer 不许丢，4.3.10）。
		content := openai.ChatCompletionSystemMessageParamContentUnion{OfString: param.NewOpt(contentText(m.Content))}
		if m.Role == "developer" {
			return openai.ChatCompletionMessageParamUnion{
				OfDeveloper: &openai.ChatCompletionDeveloperMessageParam{
					Content: openai.ChatCompletionDeveloperMessageParamContentUnion{OfString: param.NewOpt(contentText(m.Content))},
				},
			}
		}
		return openai.ChatCompletionMessageParamUnion{
			OfSystem: &openai.ChatCompletionSystemMessageParam{Content: content},
		}
	case "assistant":
		am := openai.ChatCompletionAssistantMessageParam{}
		if text := contentText(m.Content); text != "" {
			am.Content.OfString = param.NewOpt(text)
		}
		if m.ReasoningContent != "" {
			// reasoning_content 是 DeepSeek 思考承载扩展名（逐字）⇒ SDK 类型无此字段，走 extras。
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

// deepseekToolChoiceParam 映射 tool_choice 的 chat 形态（auto / none / required / 指定函数名；
// 命名形态是 chat 标准的 {"type":"function","function":{"name":...}}）。
func deepseekToolChoiceParam(tc *core.ToolChoice) openai.ChatCompletionToolChoiceOptionUnionParam {
	var u openai.ChatCompletionToolChoiceOptionUnionParam
	switch tc.Mode {
	case "function":
		if tc.Name != "" {
			u.OfFunctionToolChoice = &openai.ChatCompletionNamedToolChoiceParam{
				Function: openai.ChatCompletionNamedToolChoiceFunctionParam{Name: tc.Name},
			}
			return u
		}
		u.OfAuto = param.NewOpt("auto")
	case "none":
		u.OfAuto = param.NewOpt("none")
	case "required":
		u.OfAuto = param.NewOpt("required")
	default:
		u.OfAuto = param.NewOpt("auto")
	}
	return u
}

// deepseekChatTools 映射工具声明（chat 形：function 对象内嵌 name / description / parameters；
// parameters 是调用方给定的 JSON Schema ⇒ 原文透传，⛔ 不重整键序）。
func deepseekChatTools(r *core.Request) []openai.ChatCompletionToolUnionParam {
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

// BuildRequest 构造 /chat/completions 请求（chat 形普通字段：messages[] / max_tokens / stream /
// tools / tool_choice；prompt_cache_key 始终透传，3.5）。
func (a *deepseekAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, err
	}
	p := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(res.Model),
		Messages: deepseekChatMessages(r),
	}
	if r.MaxOutputTokens > 0 {
		p.MaxTokens = param.NewOpt(int64(r.MaxOutputTokens))
	}
	if r.PromptCacheKey != "" {
		p.PromptCacheKey = param.NewOpt(r.PromptCacheKey)
	}
	if r.ToolChoice != nil {
		p.ToolChoice = deepseekToolChoiceParam(r.ToolChoice)
	}
	p.Tools = deepseekChatTools(r)
	ex := map[string]any{}
	if r.Stream {
		ex["stream"] = true
	}
	if len(ex) > 0 {
		p.SetExtraFields(ex)
	}
	raw, err := marshalBody(p)
	if err != nil {
		return nil, err
	}
	// enable_thinking 等特异参数经 Extra 透传（黑名单见 adapter.extraBlocklist）。
	raw, err = mergeExtraBody(raw, r.Extra)
	if err != nil {
		return nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/chat/completions"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, nil
}

// ---- 响应体（chat.completion；finish_reason 超集原值）----

// ParseResponse 解析 chat.completion 响应：SDK typed 解析（openai.ChatCompletion，3.6）；
// reasoning_content 是 DeepSeek 思考承载扩展字段（逐字）⇒ 从 message 原始 JSON 取进推理正文位；
// finish_reason 超集原值（aborted / insufficient_system_resource，4.5.1 ⛔ 不得丢弃）。
func (a *deepseekAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
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

// ---- 流式（chat.completion.chunk；真实样本 testdata/real/deepseek_stream_real.sse 可直喂）----

// deepseekChatStreamDecoder 是 DeepSeek 的 chat chunk 帧解码器（3.7：每家一个帧解码器）：
// delta.content ⇒ 文本增量；delta.reasoning_content（DeepSeek 扩展名逐字）⇒ 推理增量；
// delta.tool_calls ⇒ 工具帧（帧承载约定：Text 载函数名、ToolCallID 载调用 id、Arguments 载参数增量）；
// finish_reason 超集原值收口（usage 若与 finish 同块，usage 帧先于 done 帧）。
type deepseekChatStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[int64]string // chunk tool_call.index → 调用 id（id 只在首块出现，后续块按 index 对齐）
	// pendingDone 暂存 finish 状态（延后收口口径见 decodeOne 注释）。
	pendingDone string
}

func (a *deepseekAdapter) streamDecoder() *deepseekChatStreamDecoder {
	return &deepseekChatStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[int64]string{}}
}

func (d *deepseekChatStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if !json.Valid([]byte(data)) && strings.Contains(data, "\n") {
		// 紧凑样本分流：厂商文档流示例常是「一行一个 chunk、行间无空行」形（如
		// testdata/real/kimi_stream_docs_example.txt），SSE 规范会把多行 data 拼成一个负载
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
func (d *deepseekChatStreamDecoder) decodeOne(payload string) ([]stream.Frame, bool, error) {
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
func (d *deepseekChatStreamDecoder) toolCallID(tc chatChunkToolCall) string {
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

// usageOf 取 chunk 的 usage 原文（含 hit/miss 扩展字段，⛔ 不经类型重整）：
// usage 为 null 或缺键 ⇒ 不发 usage 帧（真实样本逐块带 "usage":null 的口径）。
func (d *deepseekChatStreamDecoder) usageOf(c chatChunk) *core.Usage {
	raw := chunkUsageRaw(c.Usage)
	if raw == nil {
		return nil
	}
	return d.usageFn(raw)
}

// StreamStart：强制 stream=true ⇒ 真实 HTTP ⇒ chat chunk SSE 解码。
func (a *deepseekAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
