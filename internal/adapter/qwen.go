// 日期: 2026-11-14 14:29:38.485
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
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// Qwen / DashScope 适配器（统一 SDD 4.3.7，两形态按 core.Resolution.Shape 选）：
//
// ① 兼容层（APIShapeOpenAIResponses）：POST {base}/responses；AuthScheme: Authorization + "Bearer "。
//    请求构造 / 流式执行 / 响应解析经官方 SDK openai-go/v3（10.3：openai-go 服务 8 家
//    OpenAI 兼容端点）；enable_thinking / enable_search 等 quirks 继续经 Extra 透传（4.3.7）。
//    enable_thinking / enable_search 经 core.Request.Extra 透传（4.3.7 非标准参数）；
//    usage 的 cache_creation_input_tokens 归 usage.Normalize（4.4.2 Qwen 兼容行）。
//
// ② 原生 text-generation（APIShapeQwenNative）：POST {base}/api/v1/services/aigc/text-generation/generation；
//    ⚠️ 流式必须加头 X-DashScope-SSE: enable（3.6 坑 1）；帧格式 `data:{json}`（冒号后**无空格**）、
//    **每帧自带 usage**、**无 [DONE] 哨兵**；usage 字段是 input_tokens / output_tokens（⛔ 与兼容模式
//    的 prompt_tokens / completion_tokens 不同，4.3.7）。
//
// ⚠️ API Key 与地域强绑定（北京 Key 调弗吉尼亚 endpoint 401）——base URL 由注册表决定，适配器只拼路径。
// previous_response_id 支持 = true（3.5 表）。

type qwenAdapter struct {
	hc *http.Client
}

// NewQwen 构造 Qwen 兼容层适配器（Responses 形状）。
func NewQwen(hc *http.Client) Adapter { return &qwenAdapter{hc: hc} }

func (a *qwenAdapter) Provider() core.Provider          { return core.ProviderQwen }
func (a *qwenAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *qwenAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 4.1.1：Authorization: Bearer。
func (a *qwenAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4.2 Qwen 兼容行：命中 + cache_creation_input_tokens 都在二级）。
func (a *qwenAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderQwen, usageTreeShape(raw), raw)
}

// ---- 兼容层请求体（Responses 形状 + enable_thinking / enable_search，官方 SDK 构造）----

// qwenCompatSDKClient 构造 openai-go 客户端（Tier 1：流式执行经 SDK 客户端；10.3：openai-go
// 服务 Qwen 兼容端点，改 base_url 即可）。SDK 内部路径是 responses（出站 {base}/responses）；
// 注入既有 *http.Client（凭据由 authTransport 在 RoundTrip 注入）、⛔ 不读环境变量密钥、
// ⛔ SDK 自带重试不许生效（重试/降级由网关统一管，24）。
func qwenCompatSDKClient(hc *http.Client, baseURL string) openai.Client {
	return openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(hc),
		option.WithAPIKey(""),
		option.WithMaxRetries(0),
	)
}

// qwenCompatSDKMapErr 把 SDK 错误映射回 DetailError（与 streamRequest 的错误口径逐项对齐，7.1）。
func qwenCompatSDKMapErr(err error) error {
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
		return &DetailError{Detail: gwerrors.FromUpstream(core.ProviderQwen, ae.StatusCode, body, rid)}
	}
	return &DetailError{Detail: gwerrors.FromTransportError(core.ProviderQwen, err, "")}
}

// qwenCompatSDKInput 把规范消息拆成 Responses input items（4.2.3，wire 与既有 qwenInputItems
// 逐字段一致）；system 消息并入 instructions（既有口径）。
// SDK typed 表达不了的两处既有 wire 用 SetExtraFields 补：① reasoning item 的 id 空时省略
// （SDK 必发）；② assistant 消息带 input_image 部件时 content 整体补回（SDK 的 output message
// content union 无图片部件）。
func qwenCompatSDKInput(r *core.Request) (items responses.ResponseInputParam, instructions string) {
	for _, m := range r.Messages {
		if m.Role == "system" {
			sys := contentText(m.Content)
			if instructions == "" {
				instructions = sys
			} else {
				instructions += "\n" + sys
			}
			continue
		}
		if m.Role == "tool" {
			out := &responses.ResponseInputItemFunctionCallOutputParam{}
			text := contentText(m.Content)
			out.Output.OfString = param.NewOpt(text)
			extras := map[string]any{}
			if text == "" {
				// 既有 wire 的 output 是 omitempty（空不发）；SDK 必发 ⇒ param.Omit 剔除。
				extras["output"] = param.Omit
			}
			if m.ToolCallID != "" {
				out.CallID = param.NewOpt(m.ToolCallID)
			}
			if len(extras) > 0 {
				out.SetExtraFields(extras)
			}
			items = append(items, responses.ResponseInputItemUnionParam{OfFunctionCallOutput: out})
			continue
		}
		if m.EncryptedContent != "" || m.ReasoningContent != "" {
			it := &responses.ResponseReasoningItemParam{}
			if m.EncryptedContent != "" {
				it.EncryptedContent = param.NewOpt(m.EncryptedContent)
			}
			if m.ReasoningContent != "" {
				it.Content = []responses.ResponseReasoningItemContentParam{{Text: m.ReasoningContent}}
			}
			// 既有 wire 的 id 是 omitempty（空不发）；SDK 的 id 必发 ⇒ param.Omit 剔除。
			it.SetExtraFields(map[string]any{"id": param.Omit})
			items = append(items, responses.ResponseInputItemUnionParam{OfReasoning: it})
		}
		partType := "input_text"
		if m.Role == "assistant" {
			partType = "output_text"
		}
		var parts []qwenInputPart
		for _, p := range m.Content {
			switch {
			case p.ImageURL != "":
				parts = append(parts, qwenInputPart{Type: "input_image", ImageURL: p.ImageURL})
			case p.Text != "":
				parts = append(parts, qwenInputPart{Type: partType, Text: p.Text})
			}
		}
		if len(parts) > 0 {
			if m.Role == "assistant" {
				om := &responses.ResponseOutputMessageParam{}
				if hasImagePart(parts) {
					// SDK 的 output message content union（output_text / refusal）无图片部件
					// ⇒ content 整体按既有 wire 形补回（quirks 不许丢）。
					om.SetExtraFields(map[string]any{"content": parts})
				} else {
					for _, p := range parts {
						om.Content = append(om.Content, responses.ResponseOutputMessageContentUnionParam{
							OfOutputText: &responses.ResponseOutputTextParam{Text: p.Text},
						})
					}
				}
				items = append(items, responses.ResponseInputItemUnionParam{OfOutputMessage: om})
			} else {
				im := &responses.ResponseInputItemMessageParam{Type: "message", Role: m.Role}
				for _, p := range parts {
					switch p.Type {
					case "input_image":
						im.Content = append(im.Content, responses.ResponseInputContentUnionParam{
							OfInputImage: &responses.ResponseInputImageParam{ImageURL: param.NewOpt(p.ImageURL)},
						})
					default:
						im.Content = append(im.Content, responses.ResponseInputContentUnionParam{
							OfInputText: &responses.ResponseInputTextParam{Text: p.Text},
						})
					}
				}
				items = append(items, responses.ResponseInputItemUnionParam{OfInputMessage: im})
			}
		}
		for _, tc := range m.ToolCalls {
			fc := &responses.ResponseFunctionToolCallParam{Arguments: tc.Arguments, Name: tc.Name}
			id := tc.CallID
			if id == "" {
				id = tc.ID
			}
			fc.CallID = id
			// 既有 wire 的 id / name / arguments 是 omitempty（空不发）；SDK 三者必发
			// ⇒ 空值用 param.Omit 剔除。
			extras := map[string]any{}
			if tc.ID != "" {
				fc.ID = param.NewOpt(tc.ID)
			} else {
				extras["id"] = param.Omit
			}
			if tc.Name == "" {
				extras["name"] = param.Omit
			}
			if tc.Arguments == "" {
				extras["arguments"] = param.Omit
			}
			if id == "" {
				extras["call_id"] = param.Omit
			}
			if len(extras) > 0 {
				fc.SetExtraFields(extras)
			}
			items = append(items, responses.ResponseInputItemUnionParam{OfFunctionCall: fc})
		}
	}
	return items, instructions
}

// hasImagePart 判断部件序列是否含图片部件。
func hasImagePart(parts []qwenInputPart) bool {
	for _, p := range parts {
		if p.Type == "input_image" {
			return true
		}
	}
	return false
}

// qwenInputPart 是 Responses input content part（消息拆解中间形，wire 逐字）。
type qwenInputPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// qwenCompatSDKTools 映射工具声明（Responses 形是平铺 function tool：
// {"type":"function","name","description","parameters"}，⛔ 非 Chat 形的 function 嵌套）。
func qwenCompatSDKTools(r *core.Request) ([]responses.ToolUnionParam, error) {
	var out []responses.ToolUnionParam
	for _, t := range r.Tools {
		fn := responses.FunctionToolParam{Name: t.Name}
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
		out = append(out, responses.ToolUnionParam{OfFunction: &fn})
	}
	return out, nil
}

// qwenCompatSDKToolChoice 映射 tool_choice（语义同 toolChoiceResponses：auto / none / required /
// 指定函数名；未知模式按 auto）。
func qwenCompatSDKToolChoice(tc *core.ToolChoice) responses.ResponseNewParamsToolChoiceUnion {
	switch tc.Mode {
	case "function":
		if tc.Name != "" {
			return responses.ResponseNewParamsToolChoiceUnion{
				OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: tc.Name},
			}
		}
	case "none", "required", "auto":
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptions(tc.Mode)),
		}
	}
	return responses.ResponseNewParamsToolChoiceUnion{
		OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptions("auto")),
	}
}

// qwenCompatParams 用 SDK typed 参数构造兼容层 /responses 请求（4.3.7）。
// enable_thinking / enable_search 等非标准参数经 Extra 走 mergeExtraBody（quirks 不许丢），
// ⛔ 不进 typed 参数。
func qwenCompatParams(r *core.Request, res *core.Resolution) (responses.ResponseNewParams, error) {
	var body responses.ResponseNewParams
	if err := checkPreviousResponseID(true, r); err != nil {
		return body, err
	}
	body.Model = shared.ResponsesModel(res.Model)
	items, instructions := qwenCompatSDKInput(r)
	body.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: items}
	merged := r.Instructions
	if instructions != "" {
		if merged == "" {
			merged = instructions
		} else {
			merged += "\n" + instructions
		}
	}
	if merged != "" {
		body.Instructions = param.NewOpt(merged)
	}
	if r.MaxOutputTokens > 0 {
		body.MaxOutputTokens = param.NewOpt(int64(r.MaxOutputTokens))
	}
	if r.PromptCacheKey != "" {
		body.PromptCacheKey = param.NewOpt(r.PromptCacheKey)
	}
	if r.PreviousResponseID != "" {
		body.PreviousResponseID = param.NewOpt(r.PreviousResponseID)
	}
	tools, err := qwenCompatSDKTools(r)
	if err != nil {
		return body, err
	}
	body.Tools = tools
	if r.ToolChoice != nil {
		body.ToolChoice = qwenCompatSDKToolChoice(r.ToolChoice)
	}
	extras := map[string]any{}
	if r.Stream {
		extras["stream"] = true
	}
	if len(items) == 0 {
		// 既有 wire：input 无 omitempty ⇒ 空输入发 `"input":null`（SDK omitzero 会整键省略）。
		extras["input"] = nil
	}
	if len(extras) > 0 {
		body.SetExtraFields(extras)
	}
	return body, nil
}

// BuildRequest 构造兼容层 /responses 请求（SDK typed 参数构造序列化 Outbound.Body）。
func (a *qwenAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	body, err := qwenCompatParams(r, res)
	if err != nil {
		return nil, err
	}
	raw, err := body.MarshalJSON()
	if err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_marshal_failed", err.Error())}
	}
	// enable_thinking / enable_search 经 Extra 透传（4.3.7）。
	raw, err = mergeExtraBody(raw, r.Extra)
	if err != nil {
		return nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/responses"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, nil
}

// ---- 兼容层响应体（Responses 对象，SDK typed 解析）----

// qwenCompatProbe 是 SDK typed 解析之外的 quirk 原始探测（responses.Response 不承载）：
// status / output_text / finish_reason（兼容端点回落字段）/ output[].reasoning_content /
// 原始 usage（vendor 字段须逐字进 usage.Normalize，⛔ 不经 SDK 类型往返丢字段）。
type qwenCompatProbe struct {
	Status       string          `json:"status"`
	OutputText   string          `json:"output_text"`
	FinishReason string          `json:"finish_reason"`
	Usage        json.RawMessage `json:"usage"`
	Output       []struct {
		ReasoningContent string `json:"reasoning_content"`
	} `json:"output"`
}

// ParseResponse 解析兼容层响应（Tier 1：SDK 响应类型解析 + quirk 原始探测，
// usage 走既有归一）。解析口径与既有实现逐项一致。
func (a *qwenAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var probe qwenCompatProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	var resp responses.Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{ID: resp.ID, Model: string(resp.Model), Status: probe.Status, Text: probe.OutputText}
	hasToolCall := false
	var text string
	for i, it := range resp.Output {
		oi := core.OutputItem{Type: it.Type, ID: it.ID, Role: it.Role, EncryptedContent: it.EncryptedContent,
			CallID: it.CallID, Name: it.Name, Arguments: it.Arguments.OfString}
		if i < len(probe.Output) {
			oi.ReasoningText = probe.Output[i].ReasoningContent
		}
		for _, p := range it.Content {
			if p.Text != "" {
				oi.Content = append(oi.Content, core.ContentPart{Type: "text", Text: p.Text})
				if it.Type == "message" {
					text += p.Text
				}
			}
		}
		for _, s := range it.Summary {
			oi.Summary = append(oi.Summary, core.SummaryPart{Type: string(s.Type), Text: s.Text})
		}
		if it.Type == "function_call" {
			hasToolCall = true
		}
		out.OutputItems = append(out.OutputItems, oi)
	}
	if out.Text == "" {
		out.Text = text
	}
	out.FinishReason = deriveResponsesFinish(probe.FinishReason, probe.Status, resp.IncompleteDetails.Reason, hasToolCall)
	if len(probe.Usage) > 0 {
		out.Usage = a.ParseUsage(probe.Usage)
	}
	return out, nil
}

// ---- 兼容层流式（Tier 1：SDK 客户端流式执行 + SDK 流事件 ⇒ stream.Frame）----

// qwenCompatEventProbe 是流式事件的 quirk 原始探测（SDK 流事件类型不承载）：
// function_call_arguments.delta 的参数增量在 wire 上是 `partial_json`（SDK 类型按新口径
// 只有 `delta` 字段，⛔ 取不到该值）；response.completed / incomplete 事件里 response.usage
// 的原始 JSON（vendor 字段逐字进 usage.Normalize）。
type qwenCompatEventProbe struct {
	PartialJSON string `json:"partial_json"`
	Response    struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"response"`
}

// qwenCompatStreamDecoder 把 SDK 流事件（responses.ResponseStreamEventUnion）映射为内部帧。
// 帧序列口径（⛔ 不变）：生命周期帧 / 工具补名帧 / 文本·推理·参数增量 / usage 前置 + done 收口。
// ⚠️ 既有解码器在 data 无 type 时回落 SSE `event:` 行事件名；SDK 流事件只解 data
// ⇒ 该回落不保留（见交付报告差异清单）。
type qwenCompatStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[string]string
}

func (a *qwenAdapter) streamDecoder() *qwenCompatStreamDecoder {
	return &qwenCompatStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[string]string{}}
}

func (d *qwenCompatStreamDecoder) decode(e responses.ResponseStreamEventUnion) ([]stream.Frame, bool) {
	var probe qwenCompatEventProbe
	_ = json.Unmarshal([]byte(e.RawJSON()), &probe)
	switch e.Type {
	case "response.created", "response.in_progress", "response.queued":
		return []stream.Frame{{Kind: stream.FrameLifecycle, Status: e.Type, Seq: e.SequenceNumber}}, false
	case "response.output_item.added":
		if e.Item.Type == "function_call" && e.ItemID != "" {
			id := e.Item.CallID
			if id == "" {
				id = e.ItemID
			}
			d.toolIDs[e.ItemID] = id
			// 补名帧：item.name 载工具函数名（arguments.delta 不再带名）⇒ 名字必须在此进帧；
			// 帧承载约定：FrameToolCallArgsDelta 的 Text 载函数名。name 为空不发帧，保持原行为。
			if e.Item.Name != "" {
				return []stream.Frame{{Kind: stream.FrameToolCallArgsDelta, ToolCallID: id, Text: e.Item.Name, Seq: e.SequenceNumber}}, false
			}
		}
		return nil, false
	case "response.output_text.delta":
		return []stream.Frame{{Kind: stream.FrameTextDelta, Text: e.Delta, Seq: e.SequenceNumber}}, false
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		return []stream.Frame{{Kind: stream.FrameReasoningDelta, Reasoning: e.Delta, Seq: e.SequenceNumber}}, false
	case "response.function_call_arguments.delta":
		id := d.toolIDs[e.ItemID]
		if id == "" {
			id = e.ItemID
		}
		return []stream.Frame{{Kind: stream.FrameToolCallArgsDelta, ToolCallID: id, Arguments: probe.PartialJSON, Seq: e.SequenceNumber}}, false
	case "response.completed", "response.incomplete":
		status := core.StatusCompleted
		if e.Type == "response.incomplete" {
			status = core.StatusIncomplete
		}
		frames := []stream.Frame{{Kind: stream.FrameDone, Status: status, Seq: e.SequenceNumber}}
		if len(probe.Response.Usage) > 0 {
			frames = append([]stream.Frame{{Kind: stream.FrameUsage, Usage: d.usageFn(probe.Response.Usage), Seq: e.SequenceNumber}}, frames...)
		}
		return frames, true
	case "response.failed":
		return []stream.Frame{{Kind: stream.FrameError, Status: "response_failed", Text: "上游流式响应失败", Seq: e.SequenceNumber}}, true
	default:
		return nil, false
	}
}

// qwenCompatStreamIter 是 SDK 流式帧迭代器（语义对齐 sseIter）。
type qwenCompatStreamIter struct {
	ctx     context.Context
	st      *ssestream.Stream[responses.ResponseStreamEventUnion]
	dec     *qwenCompatStreamDecoder
	queue   []stream.Frame
	cur     stream.Frame
	err     error
	stop    bool
	closed  bool
	seqAuto int64
}

func (it *qwenCompatStreamIter) Next() bool {
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
func (it *qwenCompatStreamIter) Frame() stream.Frame { return it.cur }

// Err 返回终态错误（nil 表示正常结束）。
func (it *qwenCompatStreamIter) Err() error { return it.err }

// close 关闭 SDK 流（连带关闭上游 body）；幂等。
func (it *qwenCompatStreamIter) close() {
	if !it.closed {
		it.closed = true
		_ = it.st.Close()
	}
}

// StreamStart（兼容层，Tier 1）：强制 stream=true ⇒ SDK 客户端流式执行 ⇒ SDK 流事件映射帧。
// 请求体与 BuildRequest 的 Outbound.Body 同源（含 Extra quirks，经 SetJSON 原样发）。
func (a *qwenAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
	rc := *r
	rc.Stream = true
	params, err := qwenCompatParams(&rc, res)
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
	cl := qwenCompatSDKClient(a.hc, res.BaseURL)
	st := cl.Responses.NewStreaming(ctx, params)
	if err := st.Err(); err != nil {
		return nil, qwenCompatSDKMapErr(err)
	}
	return &qwenCompatStreamIter{ctx: ctx, st: st, dec: a.streamDecoder()}, nil
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
