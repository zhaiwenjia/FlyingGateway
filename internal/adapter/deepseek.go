// 日期: 2026-11-16 16:02:19.247
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

// DeepSeek 适配器（统一 SDD 4.3.4）：
// 出站 Responses：POST {base}/responses（2026-11-16 真实实测 4/4：basic/tools/reasoning/stream，
// 样本 testdata/real/deepseek_responses_*_real.*，登记见 testdata/real/README.md）。
// AuthScheme: Authorization + "Bearer "。
// previous_response_id 不支持（官方原话「该 API 是无状态的:服务端不存储响应与会话」）
// ⇒ checkPreviousResponseID(false, r)（3.5：⛔ 不静默丢弃，显式报错）。
// finish_reason 超集值（aborted / insufficient_system_resource 等显式 finish_reason 回落字段，
// 4.5.1）由共享 deriveResponsesFinish 的 explicit 优先分支承接，⛔ 不得丢弃。
// 流式事件含 response.reasoning_text.delta（DeepSeek 扩展事件名逐字）⇒ 共享解码器已覆盖。
// usage 两种树并存：chat 树是「命中 / 未命中」双字段（prompt_cache_hit_tokens 与
// prompt_cache_miss_tokens，4.3.4：⚠️ 唯一给双字段的家，同值取其一 ⛔ 不累加），
// Responses 树 input_tokens_details.cached_tokens ⇒ ParseUsage 按实际字段选口径（usage.Normalize
// DeepSeek 逐家口径）。
// 请求构造 / 响应解析 / 流事件解析走响应系共享机件（openai.go：responsesBuildParams /
// parseResponsesBody / newResponsesStreamDecoder，⛔ 不改签名）；DeepSeek 无家差字段。

type deepseekAdapter struct {
	hc *http.Client
}

// NewDeepSeek 构造 DeepSeek 适配器（hc 为出站 HTTP 客户端）。
func NewDeepSeek(hc *http.Client) Adapter { return &deepseekAdapter{hc: hc} }

func (a *deepseekAdapter) Provider() core.Provider          { return core.ProviderDeepSeek }
func (a *deepseekAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *deepseekAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 4.3.4：Authorization: Bearer。
func (a *deepseekAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4.2 DeepSeek 行：chat 树 prompt_tokens = 命中 + 未命中、
// 缓存命中双字段同值取其一不累加；Responses 树 input_tokens 是总输入）⇒ 按实际字段选口径。
func (a *deepseekAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderDeepSeek, usageTreeShape(raw), raw)
}

// BuildRequest 构造 POST {base}/responses 出站请求（Responses 形装配走共享机件
// responsesBuildParams：model / input items / instructions 顶层 / tools 扁平形 / tool_choice /
// reasoning / max_output_tokens / prompt_cache_key / stream）。
// enable_thinking 等特异参数经 Extra 透传（黑名单见 adapter.extraBlocklist）。
func (a *deepseekAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
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

// ParseResponse 解析 response 对象 → 规范 Response（共享机件 parseResponsesBody：
// output[] 异构 item、finish 推导 deriveResponsesFinish——显式 finish_reason 超集原值优先，
// DeepSeek 独有取值 aborted / insufficient_system_resource 随之保留，⛔ 无需家差解析）。
func (a *deepseekAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	return parseResponsesBody(a.ParseUsage, body)
}

// streamDecoder 返回 Responses 形帧解码器（共享 newResponsesStreamDecoder；DeepSeek 流式
// 事件 response.reasoning_text.delta / partial_json / completed 带 usage 均已覆盖）。
func (a *deepseekAdapter) streamDecoder() *openAIStreamDecoder {
	return newResponsesStreamDecoder(a.ParseUsage)
}

// StreamStart：强制 stream=true（3.7 上游传输恒为流式）⇒ 真实 HTTP ⇒ Responses SSE 解码。
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
