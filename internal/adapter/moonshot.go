// 日期: 2026-11-16 16:02:19.250
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// Kimi / Moonshot 适配器（统一 SDD 4.3.9）：
// 出站 Responses：POST {base}/responses（2026-11-16 真实实测 4/4：basic/tools/reasoning/stream，
// 样本 testdata/real/moonshot_responses_*_real.*，登记见 testdata/real/README.md）。
// AuthScheme: Authorization + "Bearer "。
// previous_response_id 不支持（官方 OpenAPI：store 固定 false、previous_response_id 固定 null）
// ⇒ checkPreviousResponseID(false, r)（3.5：⛔ 不静默丢弃，显式报错）。
// ⚠️ tool_choice 官方 enum 仅 auto ⇒ none / required / 指定函数名一律降为 auto 并记
// Degradation（1.4.2 ⛔ 不静默丢弃意图；DegradationAware 通道，口径同 zhipu）。
// prompt_cache_options{mode, ttl} 经 core.Request.Extra 透传（4.3.9）；
// ⛔ 显式 prompt_cache_breakpoint 不发（会被拒 400，4.3.9）⇒ Extra 黑名单剔除。
// 输出上限走 Responses 标准字段 max_output_tokens（响应系共享机件装配）。
// usage 含扩展字段 cache_write_tokens（4.4.2 Kimi 行）：chat 树归 parseKimi，Responses 树
// 归 parseResponses（resolveResponsesCacheWrite 收数值 / TTL 细分两形）⇒ 按实际字段选口径。
// reasoning 承载：Responses 形是 reasoning item 的 summary[]（真实样本逐字）；
// message.reasoning_content 扩展（4.3.9）由共享解析收进推理正文位。
// 请求构造 / 响应解析 / 流事件解析走响应系共享机件（openai.go，⛔ 不改签名）。

type moonshotAdapter struct {
	hc *http.Client
}

// NewMoonshot 构造 Kimi / Moonshot 适配器（hc 为出站 HTTP 客户端）。
func NewMoonshot(hc *http.Client) Adapter { return &moonshotAdapter{hc: hc} }

func (a *moonshotAdapter) Provider() core.Provider          { return core.ProviderMoonshot }
func (a *moonshotAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *moonshotAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 4.3.9：Authorization: Bearer。
func (a *moonshotAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4.2 Kimi 行：prompt_tokens 总数 = 未命中 + 命中 + 写入，
// 三者互斥；Responses 树 input_tokens 是总输入、cache_write_tokens 在二级）。
func (a *moonshotAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderMoonshot, usageTreeShape(raw), raw)
}

// BuildRequestWithDegradations 构造 POST {base}/responses 请求并返回降级记录。
// ⚠️ Moonshot tool_choice 官方 enum 仅 auto ⇒ 非 auto（none / required / 指定函数名）降为
// auto 并记 degradation（1.4.2 ⛔ 不静默降级；口径同 zhipu 的 tool_choice 降级）：
// 为什么不改成拒绝请求——降为 auto 是能力边界内的既有口径，问题只在「丢意图不可见」
// ⇒ 走 DegradationAware 通道随 Outbound 返回，由上层合并进 x_gateway.degradations[]。
func (a *moonshotAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, nil, err
	}
	rc := *r
	var degs []core.Degradation
	if r.ToolChoice != nil && r.ToolChoice.Mode != "auto" {
		degs = append(degs, core.Degradation{
			Field:  "tool_choice",
			Reason: core.DegradationCapabilityUnsupported,
			Detail: "Moonshot Kimi 仅支持 tool_choice=auto，已降为 auto",
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
	// prompt_cache_options{mode,ttl} 等特异参数经 Extra 透传（4.3.9）；
	// ⛔ prompt_cache_breakpoint / temperature 等已被 Extra 黑名单剔除（4.3.9 显式断点被拒 400）。
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
func (a *moonshotAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// ParseResponse 解析 response 对象 → 规范 Response（共享机件 parseResponsesBody：
// output[] 异构 item、reasoning summary[]、finish 推导 deriveResponsesFinish；
// message.reasoning_content 扩展（4.3.9）由共享解析收进推理正文位，⛔ 无需家差解析）。
func (a *moonshotAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	return parseResponsesBody(a.ParseUsage, body)
}

// streamDecoder 返回 Responses 形帧解码器（共享 newResponsesStreamDecoder；Moonshot 流式
// 事件 response.reasoning_summary_text.delta / completed 带 usage 均已覆盖）。
func (a *moonshotAdapter) streamDecoder() *openAIStreamDecoder {
	return newResponsesStreamDecoder(a.ParseUsage)
}

// StreamStart：强制 stream=true（3.7 上游传输恒为流式）⇒ 真实 HTTP ⇒ Responses SSE 解码。
func (a *moonshotAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
