// 日期: 2026-11-14 17:55:21.719
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// Qwen / DashScope 适配器（统一 SDD 4.3.7，两形态按 core.Resolution.Shape 选）：
//
// ① 兼容层（APIShapeOpenAIChat / APIShapeOpenAIResponses 同一适配器）：POST {base}/responses；
//    AuthScheme: Authorization + "Bearer "。出站走 OpenAI Responses（出站方法口径「除 Claude 外
//    优先 OpenAI SDK Response API」，2026-11-16 真实实测 4/4 全通：basic / tools / reasoning /
//    stream，样本见 testdata/real/qwen_responses_*_real.*，qwen3.8-flash 直采）；
//    请求构造走共享机件 responsesBuildParams、响应解析走 parseResponsesBody、流式走
//    newResponsesStreamDecoder（openai-go/v3 typed 结构，统一 SDD 3.6 / 10.3）。
//    enable_thinking / enable_search 等 quirks 继续经 core.Request.Extra 透传（4.3.7 非标准参数）；
//    reasoning.effort 五档随 reasoning 对象透传；tool_choice 语义同 Chat（auto / none / required /
//    指定函数名均直通）；usage 的 cache_creation_input_tokens 归 usage.Normalize（4.4.2 Qwen 兼容行）。
//    previous_response_id：支持 = true（官方「创建响应」：服务端会话保留 7 天）⇒ 直通。
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

// ---- 兼容层请求体（Responses 形状 + enable_thinking / enable_search，共享机件构造）----

// BuildRequest 构造兼容层 /responses 请求（4.3.1 Responses 字段名逐字；prompt_cache_key /
// previous_response_id 始终透传，3.5）。
func (a *qwenAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, err
	}
	p, err := responsesBuildParams(r, res)
	if err != nil {
		return nil, err
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
		URL:     joinURL(res.BaseURL, "/responses"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, nil
}

// ---- 兼容层响应体（Responses 对象；解析共享机件见 parseResponsesBody）----

// ParseResponse 解析 Responses 对象 → 规范 Response（output[] 异构 item：reasoning / message /
// function_call；finish 由 status 推导，显式 finish_reason 超集原值优先）。
func (a *qwenAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	return parseResponsesBody(a.ParseUsage, body)
}

// ---- 兼容层流式（Responses SSE；真实样本 testdata/real/qwen_responses_stream_real.sse 可直喂）----

// qwenChatStreamDecoder 是 chat chunk 帧解码器（3.7：每家一个帧解码器）：
// delta.content ⇒ 文本增量；delta.reasoning_content（Qwen 扩展名逐字）⇒ 推理增量；
// delta.tool_calls ⇒ 工具帧（帧承载约定：Text 载函数名、ToolCallID 载调用 id、Arguments 载参数增量）；
// finish_reason 超集原值收口。
// ⚠️ 出站流式已改 Responses 事件形（StreamStart 走 newResponsesStreamDecoder）；
// 本解码器保留作 chatchunk.go 共享机件的测试 / 基准载体（chatchunk_test.go 直接引用
// qwenChatStreamDecoder 与 qwenAdapter.streamDecoder().decodeOne，⛔ 不可删）。
type qwenChatStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[int64]string // chunk tool_call.index → 调用 id（id 只在首块出现，后续块按 index 对齐）
	// pendingDone 暂存 finish 状态（延后收口口径见 decodeOne 注释）。
	pendingDone string
}

// streamDecoder 返回 chat chunk 解码器（⚠️ 仅供 chatchunk_test.go 共享机件测试 / 基准载体，
// 出站流式不走它）。
func (a *qwenAdapter) streamDecoder() *qwenChatStreamDecoder {
	return &qwenChatStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[int64]string{}}
}

// decodeOne 解码单个 chat chunk 负载（[DONE] 哨兵 / 非 JSON 帧容忍）。
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
			// 延后收口：finish 帧不立即收口——usage 可能在其后独立成帧（真实 wire 两拍形态），
			// finish 即收口会把它丢掉 ⇒ usage 缺失被置 Estimated 估算；同帧带 usage 的形态
			// 由 usageOf 的同帧分支收口，两形态通吃。
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
func (d *qwenChatStreamDecoder) toolCallID(tc chatChunkToolCall) string {
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

// usageOf 取 chunk 的 usage 原文（Qwen 二级缓存字段原样，⛔ 不经类型重整）：
// usage 为 null 或缺键 ⇒ 不发 usage 帧。
func (d *qwenChatStreamDecoder) usageOf(c chatChunk) *core.Usage {
	raw := chunkUsageRaw(c.Usage)
	if raw == nil {
		return nil
	}
	return d.usageFn(raw)
}

// StreamStart（兼容层）：强制 stream=true ⇒ 真实 HTTP ⇒ Responses 事件 SSE 解码（共享机件
// newResponsesStreamDecoder；usage 由 response.completed / response.incomplete 自带）。
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
	return newSSEIter(ctx, resp.Body, newResponsesStreamDecoder(a.ParseUsage).decode, nil), nil
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
