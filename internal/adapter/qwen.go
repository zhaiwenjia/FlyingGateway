// 日期: 2026-11-14 17:55:21.719
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

// Qwen / DashScope 适配器（统一 SDD 4.3.7，两形态按 core.Resolution.Shape 选）：
//
// ① 兼容层（APIShapeOpenAIChat）：POST {base}/chat/completions；AuthScheme: Authorization + "Bearer "。
//    出站走 Chat Completions（method_ab.json 五家 A/B 实测裁定全部走 Chat，Responses 法 Qwen 失分
//    tools / stream）；请求构造 / 响应解析走 openai-go/v3 typed 结构（统一 SDD 3.6 / 10.3）。
//    enable_thinking / enable_search 等 quirks 继续经 core.Request.Extra 透传（4.3.7 非标准参数）；
//    流式带 stream_options.include_usage（Qwen chat 的 usage 在末帧）；
//    usage 的 cache_creation_input_tokens 归 usage.Normalize（4.4.2 Qwen 兼容行）。
//    previous_response_id：chat 形 wire 无此字段 ⇒ 按不支持处理（3.5：⛔ 不静默丢弃，显式报错）。
//
// ② 原生 text-generation（APIShapeQwenNative）：POST {base}/api/v1/services/aigc/text-generation/generation；
//    ⚠️ 流式必须加头 X-DashScope-SSE: enable（3.6 坑 1）；帧格式 `data:{json}`（冒号后**无空格**）、
//    **每帧自带 usage**、**无 [DONE] 哨兵**；usage 字段是 input_tokens / output_tokens（⛔ 与兼容模式
//    的 prompt_tokens / completion_tokens 不同，4.3.7）。
//
// ⚠️ API Key 与地域强绑定（北京 Key 调弗吉尼亚 endpoint 401）——base URL 由注册表决定，适配器只拼路径。

type qwenAdapter struct {
	hc *http.Client
}

// NewQwen 构造 Qwen 兼容层适配器（Chat Completions 形状）。
func NewQwen(hc *http.Client) Adapter { return &qwenAdapter{hc: hc} }

func (a *qwenAdapter) Provider() core.Provider          { return core.ProviderQwen }
func (a *qwenAdapter) OutboundShape() OutboundShape     { return ShapeChatCompletions }
func (a *qwenAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 4.1.1：Authorization: Bearer。
func (a *qwenAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4.2 Qwen 兼容行：命中 + cache_creation_input_tokens 都在二级）。
func (a *qwenAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderQwen, usageTreeShape(raw), raw)
}

// ---- 兼容层请求体（Chat 形状 + enable_thinking / enable_search，openai-go typed 构造）----

// qwenChatMessages 映射 chat 形消息序列（角色分派与 reasoning_content 回放口径同 moonshot）。
func qwenChatMessages(r *core.Request) []openai.ChatCompletionMessageParamUnion {
	var out []openai.ChatCompletionMessageParamUnion
	if r.Instructions != "" {
		out = append(out, openai.ChatCompletionMessageParamUnion{
			OfSystem: &openai.ChatCompletionSystemMessageParam{
				Content: openai.ChatCompletionSystemMessageParamContentUnion{OfString: param.NewOpt(r.Instructions)},
			},
		})
	}
	for _, m := range r.Messages {
		out = append(out, qwenChatMessage(m))
	}
	return out
}

// qwenChatMessage 映射单条规范消息（EncryptedContent 是 OpenAI 加密推理链（Responses 专属载体）
// ⇒ chat 形无对应物，不发）。
func qwenChatMessage(m core.Message) openai.ChatCompletionMessageParamUnion {
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
			// reasoning_content 是 Qwen 思考承载扩展名（逐字）⇒ SDK 类型无此字段，走 extras。
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

// qwenChatToolChoice 映射 tool_choice 的 chat 形态（auto / none / required / 指定函数名；
// 语义与既有 Responses 形逐项一致，Qwen chat 支持 tool_choice=auto——与现状对齐）。
func qwenChatToolChoice(tc *core.ToolChoice) openai.ChatCompletionToolChoiceOptionUnionParam {
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

// qwenChatTools 映射工具声明（chat 形 function 对象；parameters 原文透传）。
func qwenChatTools(r *core.Request) []openai.ChatCompletionToolUnionParam {
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

// BuildRequest 构造兼容层 /chat/completions 请求（chat 形普通字段：messages[] / max_tokens /
// stream / stream_options.include_usage / tools / tool_choice；prompt_cache_key 若请求携带则
// 带在 body，兼容层忽略无害（3.5））。
func (a *qwenAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, err
	}
	p := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(res.Model),
		Messages: qwenChatMessages(r),
	}
	if r.MaxOutputTokens > 0 {
		p.MaxTokens = param.NewOpt(int64(r.MaxOutputTokens))
	}
	if r.PromptCacheKey != "" {
		p.PromptCacheKey = param.NewOpt(r.PromptCacheKey)
	}
	if r.ToolChoice != nil {
		p.ToolChoice = qwenChatToolChoice(r.ToolChoice)
	}
	p.Tools = qwenChatTools(r)
	ex := map[string]any{}
	if r.Stream {
		// Qwen chat 的 usage 在末帧，需 stream_options.include_usage 打开（实测）。
		ex["stream"] = true
		ex["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(ex) > 0 {
		p.SetExtraFields(ex)
	}
	raw, err := marshalBody(p)
	if err != nil {
		return nil, err
	}
	// enable_thinking / enable_search 经 Extra 透传（4.3.7）。
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

// ---- 兼容层响应体（chat.completion；reasoning_content ⇒ 推理正文位）----

// ParseResponse 解析 chat.completion 响应：SDK typed 解析（openai.ChatCompletion，3.6）；
// reasoning_content 是 Qwen 思考承载字段（4.3.7）⇒ 从 message 原始 JSON 取进推理正文位；
// tool_calls 的函数名必须进 core.OutputItem.Name；finish_reason 超集原值。
func (a *qwenAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
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

// ---- 兼容层流式（chat.completion.chunk；usage 在末帧）----

// qwenChatStreamDecoder 是 Qwen 的 chat chunk 帧解码器（3.7：每家一个帧解码器）：
// delta.content ⇒ 文本增量；delta.reasoning_content（Qwen 扩展名逐字）⇒ 推理增量；
// delta.tool_calls ⇒ 工具帧（帧承载约定：Text 载函数名、ToolCallID 载调用 id、Arguments 载参数增量）；
// finish_reason 超集原值收口（usage 若与 finish 同块，usage 帧先于 done 帧）。
type qwenChatStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[int64]string // chunk tool_call.index → 调用 id（id 只在首块出现，后续块按 index 对齐）
	// pendingDone 暂存 finish 状态（延后收口口径见 decodeOne 注释）。
	pendingDone string
}

func (a *qwenAdapter) streamDecoder() *qwenChatStreamDecoder {
	return &qwenChatStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[int64]string{}}
}

func (d *qwenChatStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if !json.Valid([]byte(data)) && strings.Contains(data, "\n") {
		// 紧凑样本分流：厂商文档流示例常是「一行一个 chunk、行间无空行」形，SSE 规范会把
		// 多行 data 拼成一个负载 ⇒ 整负载非 JSON 时按行拆分逐行解，真实样本可直喂。
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
func (d *qwenChatStreamDecoder) decodeOne(payload string) ([]stream.Frame, bool, error) {
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
	var c openai.ChatCompletionChunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return nil, false, nil // 非 JSON 帧（keep-alive / 文档省略标记等）容忍跳过（3.7 流式容错）
	}
	var frames []stream.Frame
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		if ch.Delta.Content != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: ch.Delta.Content})
		}
		var dq struct {
			ReasoningContent string `json:"reasoning_content"`
		}
		_ = json.Unmarshal([]byte(ch.Delta.RawJSON()), &dq) // 扩展字段从原始 delta JSON 取
		if dq.ReasoningContent != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: dq.ReasoningContent})
		}
		for _, tc := range ch.Delta.ToolCalls {
			frames = append(frames, stream.Frame{
				Kind: stream.FrameToolCallArgsDelta, ToolCallID: d.toolCallID(tc),
				Text: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		if ch.FinishReason != "" {
			// ⚠️ 延后收口（与 moonshot / xiaomi 同口径）：finish 帧不立即收口——
			// 现象：Qwen chat 真实样本的 usage 在末帧，finish(usage:null) ⇒ 独立 usage 帧 ⇒ [DONE]
			// 两拍形态，finish 即收口会把独立 usage 帧丢掉 ⇒ usage 缺失被置 Estimated 估算；
			// 根因：同帧带 usage 只是一种形态，真实 wire 是 finish(usage:null) ⇒ 独立 usage 帧；
			// 为什么不按同帧假设收口：Kimi 又是同帧形态 ⇒ 延后收口两形态通吃；
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
func (d *qwenChatStreamDecoder) toolCallID(tc openai.ChatCompletionChunkChoiceDeltaToolCall) string {
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

// usageOf 取 chunk 的 usage 原文（Qwen 二级缓存字段原样，⛔ 不经 SDK 类型重整）：
// usage 为 null 或缺键 ⇒ 不发 usage 帧。
func (d *qwenChatStreamDecoder) usageOf(c openai.ChatCompletionChunk) *core.Usage {
	raw := c.JSON.Usage.Raw()
	if raw == "" || raw == "null" {
		return nil
	}
	return d.usageFn(json.RawMessage(raw))
}

// StreamStart（兼容层）：强制 stream=true ⇒ 真实 HTTP ⇒ chat chunk SSE 解码。
// 请求体与 BuildRequest 的 Outbound.Body 同源（含 Extra quirks，经 mergeExtraBody 并入）。
func (a *qwenAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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

// =====================================================================
// 原生 text-generation 形态（APIShapeQwenNative）
// =====================================================================

type qwenNativeAdapter struct {
	hc *http.Client
}

// NewQwenNative 构造 Qwen 原生 text-generation 适配器。
func NewQwenNative(hc *http.Client) Adapter { return &qwenNativeAdapter{hc: hc} }

func (a *qwenNativeAdapter) Provider() core.Provider          { return core.ProviderQwen }
func (a *qwenNativeAdapter) OutboundShape() OutboundShape     { return ShapeQwenNative }
func (a *qwenNativeAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 4.1.1：Authorization: Bearer（原生与兼容层同一把 key）。
func (a *qwenNativeAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（shape=APIShapeQwenNative，4.3.7：usage 字段 input_tokens /
// output_tokens，⛔ 与兼容模式字段名不同；无缓存明细 ⇒ 读写记 0）。
func (a *qwenNativeAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderQwen, core.APIShapeQwenNative, raw)
}

// ---- 原生请求体（⚠️ SDD 未逐字给字段名 ⇒ 按 DashScope text-generation 通用形状构造：
// model / input.messages / parameters；流式须加头 X-DashScope-SSE: enable，3.6 坑 1）----

type qwenNativeReq struct {
	Model      string           `json:"model"`
	Input      qwenNativeInput  `json:"input"`
	Parameters qwenNativeParams `json:"parameters,omitempty"`
}

type qwenNativeInput struct {
	Messages []qwenNativeMessage `json:"messages"`
}

type qwenNativeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type qwenNativeParams struct {
	ResultFormat   string   `json:"result_format,omitempty"`
	Stream         bool     `json:"stream,omitempty"`
	MaxTokens      int      `json:"max_tokens,omitempty"`
	Temperature    *float64 `json:"temperature,omitempty"`
	TopP           *float64 `json:"top_p,omitempty"`
	Stop           []string `json:"stop,omitempty"`
	EnableThinking *bool    `json:"enable_thinking,omitempty"`
	EnableSearch   any      `json:"enable_search,omitempty"`
}

// BuildRequest 构造原生 text-generation 请求。
func (a *qwenNativeAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, err
	}
	body := qwenNativeReq{
		Model: res.Model,
		Input: qwenNativeInput{},
		Parameters: qwenNativeParams{
			ResultFormat: "message",
			Stream:       r.Stream,
			MaxTokens:    r.MaxOutputTokens,
			Temperature:  r.Temperature,
			TopP:         r.TopP,
			Stop:         r.Stop,
		},
	}
	for _, m := range r.Messages {
		text := contentText(m.Content)
		switch m.Role {
		case "system":
			body.Input.Messages = append(body.Input.Messages, qwenNativeMessage{Role: "system", Content: text})
		case "assistant":
			body.Input.Messages = append(body.Input.Messages, qwenNativeMessage{Role: "assistant", Content: text})
		default:
			body.Input.Messages = append(body.Input.Messages, qwenNativeMessage{Role: "user", Content: text})
		}
	}
	raw, err := marshalBody(body)
	if err != nil {
		return nil, err
	}
	raw, err = mergeExtraBody(raw, r.Extra)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if r.Stream {
		// ⚠️ 原生流式必须加头 X-DashScope-SSE: enable（3.6 坑 1 / 4.3.7）。
		headers["X-DashScope-SSE"] = "enable"
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/api/v1/services/aigc/text-generation/generation"),
		Headers: headers,
		Body:    raw,
	}, nil
}

// ---- 原生响应体（output.text / output.finish_reason / usage.input_tokens|output_tokens）----

type qwenNativeResp struct {
	Output struct {
		Text         string `json:"text"`
		FinishReason string `json:"finish_reason"`
		Choices      []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	} `json:"output"`
	Usage     json.RawMessage `json:"usage"`
	RequestID string          `json:"request_id"`
	Code      string          `json:"code"`
	Message   string          `json:"message"`
}

// ParseResponse 解析原生响应（4.3.7：usage 字段 input_tokens / output_tokens）。
func (a *qwenNativeAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r qwenNativeResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	if r.Code != "" {
		// DashScope 业务错误（code/message 形态）。
		return &core.Response{
			Status: core.StatusFailed,
			Error:  gwerrors.New(core.ErrUpstreamError, 200, r.Code, r.Message),
		}, nil
	}
	out := &core.Response{Status: core.StatusCompleted}
	text := r.Output.Text
	finish := r.Output.FinishReason
	if len(r.Output.Choices) > 0 {
		text = r.Output.Choices[0].Message.Content
		if finish == "" {
			finish = r.Output.Choices[0].FinishReason
		}
	}
	out.Text = text
	out.OutputItems = []core.OutputItem{{
		Type: "message", Role: "assistant", Content: []core.ContentPart{{Type: "text", Text: text}},
	}}
	out.FinishReason = finish
	if out.FinishReason == "" {
		out.FinishReason = core.FinishUnknown
	}
	if len(r.Usage) > 0 {
		out.Usage = a.ParseUsage(r.Usage)
	}
	return out, nil
}

// ---- 原生流式（SSE 帧 `data:{json}` 冒号无空格、每帧自带 usage、⛔ 无 [DONE]）----

type qwenNativeStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
}

func (a *qwenNativeAdapter) streamDecoder() *qwenNativeStreamDecoder {
	return &qwenNativeStreamDecoder{usageFn: a.ParseUsage}
}

func (d *qwenNativeStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	// ⛔ 无 [DONE] 哨兵（4.3.7）；以 output.finish_reason 判终态。
	var r qwenNativeResp
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		return nil, false, nil
	}
	var frames []stream.Frame
	text := r.Output.Text
	if text != "" {
		frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: text})
	}
	// 每帧自带 usage（4.3.7）。
	if len(r.Usage) > 0 {
		frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.usageFn(r.Usage)})
	}
	if r.Output.FinishReason != "" {
		frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: r.Output.FinishReason})
		return frames, true, nil
	}
	return frames, false, nil
}

// StreamStart（原生）：强制 stream=true（请求体 stream + 头 X-DashScope-SSE: enable）⇒ SSE 解码。
func (a *qwenNativeAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
