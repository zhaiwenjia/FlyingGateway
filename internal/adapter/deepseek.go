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

// BuildRequest 委托 BuildRequestWithDegradations（降级记录由上层合并进 x_gateway.degradations[]）。
func (a *deepseekAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// BuildRequestWithDegradations 构造 POST {base}/responses 出站请求并返回降级记录
// （Responses 形装配走共享机件 responsesBuildParams：model / input items / instructions 顶层 /
// tools 扁平形 / tool_choice / reasoning / max_output_tokens / prompt_cache_key / stream）。
// enable_thinking 等特异参数经 Extra 透传（黑名单见 adapter.extraBlocklist）。
//
// 【2026-11-18 修复·发现即修】tool_choice 降级（none/auto 之外降为 auto，与智谱同形）：
//   - 现象：deepseek-flash 经 GW 带强制 tool_choice 的工具请求流式/非流式双 400
//     （上游逐字："Thinking mode does not support this tool_choice"；20261118-allkeys-toolcall-verify
//     电池 deepseek S1/S2 格 FAIL-FRAMEWORK）。
//   - 根因：DeepSeek thinking 模式只吃 tool_choice=none/auto（2026-11-18 上游实测：
//     none 200、required 与指定函数名均 400），而本 adapter 原无降级块、tool_choice 直通上游。
//   - 为什么这个修法：与 zhipu/xiaomi/moonshot/minimax 同款 DegradationAware 降级块——
//     required/指定函数名/未知取值降为 auto 并记 degradation（1.4.2 不静默降级）；
//     ⛔ 不改拒绝请求（none/auto 之外降 auto 是既有能力边界口径，意图丢失可见即可）。
//   - 不修会怎样：强制 tool_choice 的工具请求（评测电池/Harness 冒烟）在 deepseek 线全 400，
//     且错误伪装成调用方参数问题（invalid_request_error）。
func (a *deepseekAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, nil, err
	}
	rc := *r
	var degs []core.Degradation
	if tc := r.ToolChoice; tc != nil && tc.Mode != "auto" && tc.Mode != "none" {
		degs = append(degs, core.Degradation{
			Field:  "tool_choice",
			Reason: core.DegradationCapabilityUnsupported,
			Detail: "DeepSeek thinking 模式仅支持 tool_choice=none/auto（上游逐字：Thinking mode does not support this tool_choice），required/指定函数名已降为 auto",
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
