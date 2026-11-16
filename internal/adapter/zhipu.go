// 日期: 2026-11-14 16:38:51.679
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// 智谱 GLM / BigModel 适配器（统一 SDD 4.3.8）：
// 出站 Responses：POST {base}/responses（出站方法口径「除 Claude 外优先 OpenAI SDK Response API」，
// 2026-11-16 真实实测 4/4 全通：basic / tools / reasoning / stream，样本见
// testdata/real/zhipu_responses_*_real.*，glm-5.3-flash 普通 key 直采）。
// ⚠️ URL 特殊：Responses 端点是 https://open.bigmodel.cn/api/v1/responses，而注册 base 是
// chat 形 base https://open.bigmodel.cn/api/paas/v4（其下 /responses 实测 4×404）
// ⇒ BuildRequest 对 base 做后缀重写（见 zhipuResponsesURL）。
// AuthScheme: Authorization + "Bearer "。
// previous_response_id：支持 = true（官方「创建 Response」：服务端会话 store=true、保留 7 天）⇒ 直通。
// tool_choice：官方 enum 仅 none/auto ⇒ required / 指定函数名按降级处理（降为 auto 并记
// Degradation，⛔ 不静默丢弃无标记）；thinking.type / thinking.clear_thinking 经 core.Request.Extra 透传。
// 请求构造走共享机件 responsesBuildParams（openai-go/v3 typed 参数，统一 SDD 3.6 / 10.3）；
// 响应解析走共享机件 parseResponsesBody（output[] 异构联合；finish_reason 非标准取值
// sensitive / network_error / model_context_window_exceeded ⇒ 超集原值，4.5.1 ⛔ 不丢弃）；
// 流式走共享机件 newResponsesStreamDecoder（response.* 事件、usage 在 completed/incomplete 帧）。

type zhipuAdapter struct {
	hc *http.Client
}

// NewZhipu 构造智谱 GLM 适配器（hc 为出站 HTTP 客户端）。
func NewZhipu(hc *http.Client) Adapter { return &zhipuAdapter{hc: hc} }

func (a *zhipuAdapter) Provider() core.Provider          { return core.ProviderZhipu }
func (a *zhipuAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *zhipuAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 4.3.8：Authorization: Bearer。
func (a *zhipuAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4.2 智谱行：命中单列、无写入、completion/output 含 reasoning）。
func (a *zhipuAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderZhipu, usageTreeShape(raw), raw)
}

// ---- 请求体（Responses 形状 + 智谱 URL 后缀重写）----

// zhipuResponsesURL 拼智谱 Responses 端点：注册 base 是 chat 形 base
// （https://open.bigmodel.cn/api/paas/v4，其下 /responses 实测 4×404）
// ⇒ 以 /api/paas/v4 结尾的 base 重写为 /api/v1 再拼 /responses；其它 base 原样拼接。
// 出处：2026-11-16 真实实测 —— 同一把普通 key，/api/v1/responses 4/4 全通、
// /api/paas/v4/responses 404；官方「创建 Response」API 参考亦载 /api/v1。
func zhipuResponsesURL(base string) string {
	b := strings.TrimRight(base, "/")
	if strings.HasSuffix(b, "/api/paas/v4") {
		b = strings.TrimSuffix(b, "/api/paas/v4") + "/api/v1"
	}
	return joinURL(b, "/responses")
}

// BuildRequestWithDegradations 构造 /responses 请求并返回降级记录（4.3.8 关键字段逐字）。
// tool_choice 降级：智谱 Responses 官方 enum 仅 none/auto ⇒ required / 指定函数名 / 其它
// 非 none-auto 取值降为 auto（保持 chat 形时代降级语义）并记 degradation（1.4.2 不静默降级，
// ⛔ 不静默丢弃无标记）。
func (a *zhipuAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, nil, err
	}
	rc := *r
	var degs []core.Degradation
	if tc := r.ToolChoice; tc != nil && tc.Mode != "auto" && tc.Mode != "none" {
		// 智谱 Responses 仅支持 tool_choice=none/auto（官方 enum）⇒ required / 指定函数名
		// （及未知取值）降为 auto 并记 degradation（与 chat 形时代降级语义、标记一致）。
		degs = append(degs, core.Degradation{
			Field:  "tool_choice",
			Reason: core.DegradationCapabilityUnsupported,
			Detail: "智谱 GLM Responses 仅支持 tool_choice=none/auto，required/指定函数名已降为 auto",
		})
		rc.ToolChoice = &core.ToolChoice{Mode: "auto"}
	}
	p, err := responsesBuildParams(&rc, res)
	if err != nil {
		return nil, nil, err
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
		URL:     zhipuResponsesURL(res.BaseURL),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, degs, nil
}

// BuildRequest 委托 BuildRequestWithDegradations（降级记录由上层合并进 x_gateway.degradations[]）。
func (a *zhipuAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// ---- 响应体（Responses 对象；解析共享机件见 parseResponsesBody）----

// ParseResponse 解析 Responses 对象 → 规范 Response：finish_reason 非标准取值（sensitive /
// network_error / model_context_window_exceeded）经共享机件的显式 finish_reason 通道
// 超集原值保留（4.5.1）；output[] 异构 item（reasoning / message / function_call）逐项映射。
func (a *zhipuAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	return parseResponsesBody(a.ParseUsage, body)
}

// ---- 流式（Responses SSE；真实样本 testdata/real/zhipu_responses_stream_real.sse 可直喂）----

// zhipuChatStreamDecoder 是 chat chunk 帧解码器（3.7：每家一个帧解码器）：
// delta.content ⇒ 文本增量；delta.reasoning_content（智谱扩展名逐字）⇒ 推理增量；
// delta.tool_calls ⇒ 工具帧（帧承载约定：Text 载函数名、ToolCallID 载调用 id、Arguments 载参数增量）；
// finish_reason 超集原值收口。
// ⚠️ 出站流式已改 Responses 事件形（StreamStart 走 newResponsesStreamDecoder）；
// 本解码器保留作 chatchunk.go 共享机件的测试 / 基准载体（chatchunk_test.go 直接引用
// zhipuAdapter.streamDecoder().decodeOne，⛔ 不可删）。
type zhipuChatStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[int64]string // chunk tool_call.index → 调用 id（id 只在首块出现，后续块按 index 对齐）
	// pendingDone 暂存 finish 状态（延后收口口径见 decodeOne 注释）。
	pendingDone string
}

// streamDecoder 返回 chat chunk 解码器（⚠️ 仅供 chatchunk_test.go 共享机件测试 / 基准载体，
// 出站流式不走它）。
func (a *zhipuAdapter) streamDecoder() *zhipuChatStreamDecoder {
	return &zhipuChatStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[int64]string{}}
}

// decodeOne 解码单个 chat chunk 负载（[DONE] 哨兵 / 非 JSON 帧容忍）。
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

// StreamStart：强制 stream=true ⇒ 真实 HTTP ⇒ Responses 事件 SSE 解码（共享机件
// newResponsesStreamDecoder；usage 由 response.completed / response.incomplete 自带）。
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
	return newSSEIter(ctx, resp.Body, newResponsesStreamDecoder(a.ParseUsage).decode, nil), nil
}
