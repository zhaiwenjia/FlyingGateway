// 日期: 2026-11-16 16:02:19.253
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

// 小米 MiMo 适配器（统一 SDD 4.3.10）：
// 出站 Responses：POST {base}/responses（2026-11-16 真实实测 4/4：basic/tools/reasoning/stream，
// 样本 testdata/real/mimo_responses_*_real.*，登记见 testdata/real/README.md）。
// ⚠️ AuthScheme HeaderName = "api-key"（⛔ 非 Authorization——MiMo 唯一非标准头名，4.3.10 / 8.4）。
// ⚠️ messages[].role 比 OpenAI 多一个 developer ⇒ 必须保留（4.3.10）：共享装配按原角色
// 出 input item（role:"developer" 逐字），⛔ 不归并成 system / user。
// usage 自带于 response.completed（无需 stream_options 开关，4.3.10）⇒ 请求体 ⛔ 不发 stream_options。
// finish_reason 含 repetition_truncation（MiMo 独有，4.5.1）⇒ 共享 deriveResponsesFinish 的
// explicit 优先分支承接超集原值，⛔ 不得丢弃。
// previous_response_id 不支持（官方明列「不兼容参数:暂不支持 background、previous_response_id、
// context_management」）⇒ checkPreviousResponseID(false, r)（⛔ 不静默丢弃，显式报错）。
// ⚠️ tool_choice 仅 auto ⇒ 非 auto 降为 auto 并记 Degradation（1.4.2 ⛔ 不静默；
// chat 形时代同等语义与标记，DegradationAware 通道）。
// 请求构造 / 响应解析 / 流事件解析走响应系共享机件（openai.go，⛔ 不改签名）。

type xiaomiAdapter struct {
	hc *http.Client
}

// NewXiaomi 构造小米 MiMo 适配器（hc 为出站 HTTP 客户端）。
func NewXiaomi(hc *http.Client) Adapter { return &xiaomiAdapter{hc: hc} }

func (a *xiaomiAdapter) Provider() core.Provider          { return core.ProviderXiaomi }
func (a *xiaomiAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *xiaomiAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 4.3.10 / 8.4：api-key: <key>（⚠️ 非标准头名，⛔ 非 Authorization）。
func (a *xiaomiAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "api-key", Prefix: ""}
}

// ParseUsage 委托 usage.Normalize（4.4.2 MiMo 行：chat 树命中在二级、无写入；
// Responses 树 input_tokens_details，无命中）⇒ 按实际字段选口径。
func (a *xiaomiAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderXiaomi, usageTreeShape(raw), raw)
}

// BuildRequestWithDegradations 构造 POST {base}/responses 请求并返回降级记录。
// ⚠️ tool_choice 非 auto 被降为 auto 必须记 degradation（1.4.2 ⛔ 不静默降级；chat 形时代
// 修复口径，语义与标记原样保留）：为什么不改成拒绝请求——4.3.10 tool_choice 仅 auto，
// 降为 auto 是能力边界内的既有口径，问题只在「丢意图不可见」⇒ 走 DegradationAware 通道。
func (a *xiaomiAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, nil, err
	}
	rc := *r
	var degs []core.Degradation
	if r.ToolChoice != nil {
		// 4.3.10：tool_choice 仅 auto ⇒ 非 auto 一律降为 auto 并记 degradation（⛔ 不静默）。
		if r.ToolChoice.Mode != "auto" {
			degs = append(degs, core.Degradation{
				Field:  "tool_choice",
				Reason: core.DegradationCapabilityUnsupported,
				Detail: "小米 MiMo 仅支持 tool_choice=auto，已降为 auto",
			})
		}
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
	// 家差特异参数经 Extra 透传（黑名单见 adapter.extraBlocklist）。
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
func (a *xiaomiAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// ParseResponse 解析 response 对象 → 规范 Response（共享机件 parseResponsesBody：
// output[] 异构 item、output_text 便捷字段、finish 推导 deriveResponsesFinish——
// repetition_truncation 等显式 finish_reason 超集原值优先，⛔ 无需家差解析）。
func (a *xiaomiAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	return parseResponsesBody(a.ParseUsage, body)
}

// streamDecoder 返回 Responses 形帧解码器（共享 newResponsesStreamDecoder；MiMo 流式
// 事件 response.reasoning_text.delta / output_text.delta / completed 带 usage 均已覆盖）。
func (a *xiaomiAdapter) streamDecoder() *openAIStreamDecoder {
	return newResponsesStreamDecoder(a.ParseUsage)
}

// StreamStart：强制 stream=true（3.7 上游传输恒为流式）⇒ 真实 HTTP ⇒ Responses SSE 解码。
func (a *xiaomiAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
