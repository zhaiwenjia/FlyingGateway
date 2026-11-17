// 日期: 2026-11-14 14:49:54.932
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

// Ollama 适配器（统一 SDD 4.3.6，单形态：/v1 兼容层，国内供应商统一 OpenAI Responses 通道）：
//   - /v1 兼容层（APIShapeOpenAIChat / APIShapeOpenAIResponses 两个登记键归同一适配器）：
//     出站 OpenAI Responses 形 POST {base}/v1/responses（v0.13.3+ 支持、仅无状态 flavor），
//     装配 / 响应解析 / 流式解码走响应系共享机件；
//     ⛔ tool_choice / n / user / logit_bias 不发（4.3.6 已知偏差；tool_choice 剔除、非 auto 才记降级）；
//     usage 两种树按实际字段选口径（usageTreeShape，4.4.2 Ollama /v1 行）。
//   ⛔ 无原生 /api/chat 形态（ShapeOllamaNative 已移除）——Ollama 同样统一 Responses 通道。
// 鉴权：本地进程无鉴权（/v1 兼容层任意值忽略）⇒ AuthScheme 空。
// SupportsPreviousResponseID=false（3.5 表：Ollama ❌；/v1/responses 仅无状态 flavor）。

// ---- /v1 兼容层（OpenAI Responses 形：POST {base}/v1/responses）----

type ollamaCompatAdapter struct {
	hc *http.Client
}

// NewOllamaCompat 构造 Ollama /v1 兼容层适配器。
func NewOllamaCompat(hc *http.Client) Adapter { return &ollamaCompatAdapter{hc: hc} }

func (a *ollamaCompatAdapter) Provider() core.Provider      { return core.ProviderOllama }
func (a *ollamaCompatAdapter) OutboundShape() OutboundShape { return ShapeResponses }

// SupportsPreviousResponseID=false：/v1/responses 仅无状态 flavor（官方原话「Only the non-stateful
// flavor is supported (i.e., there is no previous_response_id or conversation support)」，v0.13.3+）。
func (a *ollamaCompatAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 空：/v1 兼容层鉴权任意值忽略（4.3.6）。
func (a *ollamaCompatAdapter) AuthScheme() AuthScheme { return AuthScheme{} }

// ParseUsage 委托 usage.Normalize（4.4）：按实际 usage 树选口径（usageTreeShape）——Responses 树
// （input_tokens*）按 input_tokens_details 归一；Chat 树（prompt_tokens*）无缓存明细 ⇒ 读写记 0
// （4.4.2 Ollama /v1 行）。
func (a *ollamaCompatAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderOllama, usageTreeShape(raw), raw)
}

// BuildRequestWithDegradations 构造 POST {base}/v1/responses 请求并返回降级记录。
// 装配走共享机件 responsesBuildParams（3.6）；家差：tool_choice 官方未列支持（OpenAI compatibility
// Supported features 仅 Streaming / Tools (function calling) / Reasoning summaries）⇒ 请求带
// tool_choice 时出站剔除，仅非 auto（none / required / 指定函数名）记降级——auto 与 Ollama 默认
// 行为语义一致、无损失 ⛔ 不记（DegradationAware，口径同 zhipu / moonshot）；⛔ n / user / logit_bias
// 不发（4.3.6 已知偏差，Responses 形装配本就不产出这些字段）。
func (a *ollamaCompatAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	// previous_response_id 不支持（官方：v0.13.3+ 仅无状态 flavor）⇒ 显式报错（3.5）。
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, nil, err
	}
	var degs []core.Degradation
	rc := *r
	if r.ToolChoice != nil {
		// tool_choice 官方未列支持 ⇒ 出站一律剔除；仅当调用方意图真正丢失（none / required /
		// 指定函数名）时记降级——auto 与 Ollama 默认行为（模型自行决定）语义一致、无损失
		// ⛔ 不记（口径同 zhipu / moonshot：auto 不记、非 auto 才记）。
		if r.ToolChoice.Mode != "auto" {
			degs = append(degs, core.Degradation{
				Field:  "tool_choice",
				Reason: core.DegradationCapabilityUnsupported,
				Detail: "Ollama /v1 兼容层不支持 tool_choice，已从出站请求剔除",
			})
		}
		// 从装配入参剔除 ⇒ wire 不出 tool_choice 键（⛔ 不静默丢弃：降级随 DegradationAware 返回）。
		rc.ToolChoice = nil
	}
	p, err := responsesBuildParams(&rc, res)
	if err != nil {
		return nil, nil, err
	}
	raw, err := marshalBody(p)
	if err != nil {
		return nil, nil, err
	}
	// 供应商特异参数经 Extra 透传（mergeExtraBody 黑名单口径，3.1）。
	raw, err = mergeExtraBody(raw, r.Extra)
	if err != nil {
		return nil, nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/v1/responses"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, degs, nil
}

// BuildRequest 委托 BuildRequestWithDegradations（降级记录由上层合并进 x_gateway.degradations[]）。
func (a *ollamaCompatAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// ParseResponse 解析 Responses 响应对象（共享机件 parseResponsesBody；真机样本
// testdata/real/ollama_responses_*_real.json 无家差特殊解析需求）。
func (a *ollamaCompatAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	return parseResponsesBody(a.ParseUsage, body)
}

// streamDecoder 返回 Responses 形帧解码器（共享机件；真机流以 response.completed 收口、
// 无 [DONE]，[DONE] 有无都容忍）。
func (a *ollamaCompatAdapter) streamDecoder() *openAIStreamDecoder {
	return newResponsesStreamDecoder(a.ParseUsage)
}

// StreamStart：强制 stream=true ⇒ BuildRequest ⇒ 真实 HTTP ⇒ SSE 解码（照 openai.go）。
func (a *ollamaCompatAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
