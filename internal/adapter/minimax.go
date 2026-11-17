// 日期: 2026-11-14 14:29:38.492
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// MiniMax 适配器（统一 SDD 4.3.11，单形态：国内供应商统一 OpenAI Responses 通道，
// APIShapeOpenAIChat / APIShapeOpenAIResponses 两个登记键归同一适配器）：
// POST {base}/responses（国内站 https://api.minimax.cn/v1/responses；⛔ 不走国际版 api.minimax.io）。
// 装配 / 响应解析 / 流式解码走响应系共享机件（responsesBuildParams / parseResponsesBody /
// newResponsesStreamDecoder，3.6：响应系共用 openai-go/v3）；quirks（GroupId 禁发、
// temperature/top_p 剔除、base_resp 必检、tool_choice 降级记录）继续独立处理。
//
// 形态特性（4.3.11）：
//   - ⛔ 不传 GroupId / group_id（旧版遗留口径，沿用会引入无效参数）。
//   - ⚠️ temperature / top_p 不受支持（传值无效，恒为 1 / 0.95）⇒ 构造时剔除。
//   - ⚠️ base_resp 必检：HTTP 200 + status_code != 0 是业务错误 ⇒ errors.FromMiniMaxBaseResp
//     （⛔ 不检查 base_resp 就会把错误当成功）。
//   - 推理走 reasoning item / reasoning_text 事件（官方 Create Response 示例），⛔ 不拆 think 标签。
//   - usage 按实际 usage 树选口径（usageTreeShape）：Responses 树带
//     input_tokens_details.cached_tokens（2026-11-16 真实样本命中 189/197），按通用口径归一；
//     Chat 树无缓存命中/写入字段 ⇒ 记 0。
//   - finish_reason 取值 stop / length / content_filter / tool_calls（流式 chunk 与非流式同一枚举口径）。
//
// previous_response_id：/responses 按不支持处理（官方 Create Response schema 明言「请求中暂不支持
// 该参数，传入后将被忽略，响应中固定返回 null」——上游静默吞参 ⇒ 统一 SDD 3.5 显式报错）。

// ---- 共享：base_resp 必检 ----

// minimaxBaseResp 是错误对象 base_resp（4.3.11：HTTP 200 + 业务错误码并存）。
type minimaxBaseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// minimaxCheckBaseResp 必检 base_resp：status_code != 0 ⇒ errors.FromMiniMaxBaseResp（4.3.11）。
func minimaxCheckBaseResp(b *minimaxBaseResp) *core.ErrorDetail {
	if b == nil || b.StatusCode == 0 {
		return nil
	}
	return gwerrors.FromMiniMaxBaseResp(b.StatusCode, b.StatusMsg)
}

// thinkOpen / thinkClose 是 MiniMax thinking 标签（reasoning_split=false 时思考并入 content，
// 4.3.11）。字面量用十六进制转义写死：写入链路会静默吞掉尖括号闭合形态的字面量
// （2026-11-14 实测——同文件多处字符串因此未终止，函数尾部整段丢失）。
const (
	thinkOpen  = "\x3cthink\x3e"
	thinkClose = "\x3c/think\x3e"
)

// ---- ① OpenAI 形（chat/completions）----

type minimaxAdapter struct {
	hc *http.Client
}

// NewMiniMax 构造 MiniMax OpenAI 形适配器（hc 为出站 HTTP 客户端）。
func NewMiniMax(hc *http.Client) Adapter { return &minimaxAdapter{hc: hc} }

func (a *minimaxAdapter) Provider() core.Provider      { return core.ProviderMiniMax }
func (a *minimaxAdapter) OutboundShape() OutboundShape { return ShapeResponses }

// SupportsPreviousResponseID=false：官方 Create Response schema 明言 previous_response_id
// 「请求中暂不支持该参数，传入后将被忽略，响应中固定返回 null」（上游静默吞参）⇒ 3.5：
// 按不支持处理、显式报错（⛔ 不静默丢弃；出处 platform.minimax.cn/docs/api-reference/responses-create）。
func (a *minimaxAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 4.3.11：仅 Authorization: Bearer（⛔ 无 GroupId）。
func (a *minimaxAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4）：按实际 usage 树选口径（usageTreeShape）——Chat 树
// 无缓存命中/写入字段 ⇒ 记 0；Responses 树带 input_tokens_details.cached_tokens
// （2026-11-16 真实样本：tools 例命中 189、reasoning 例 197）⇒ 按通用口径归一，⛔ 不记 0。
func (a *minimaxAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderMiniMax, usageTreeShape(raw), raw)
}

// ---- 请求体（Responses 形；装配走共享机件 responsesBuildParams，3.6）----

// minimaxBaseRespProbe 探测 base_resp（4.3.11 形态共性：HTTP 200 + status_code != 0 ⇒ 业务错误，
// ⛔ 不检查 base_resp 就会把错误当成功）。响应对象顶层与流事件的 response 载荷两处都探：
// chat 形 chunk 的 base_resp 在顶层（4.3.11 实证口径），/responses 官方示例未见该字段、无 key 实测
// ⇒ 任务书「base_resp 错误处理保持现状」，保守两处都探（缺失时按正常路径解析）。
type minimaxBaseRespProbe struct {
	ID       string           `json:"id"`
	Model    string           `json:"model"`
	BaseResp *minimaxBaseResp `json:"base_resp"`
	Response *struct {
		BaseResp *minimaxBaseResp `json:"base_resp"`
	} `json:"response"`
}

// baseRespOf 取 base_resp（响应对象顶层优先，流事件退回 response 载荷内）。
func (p *minimaxBaseRespProbe) baseRespOf() *minimaxBaseResp {
	if p.BaseResp != nil {
		return p.BaseResp
	}
	if p.Response != nil {
		return p.Response.BaseResp
	}
	return nil
}

// BuildRequestWithDegradations 构造 POST {base}/responses 请求并返回降级记录。
// 装配走共享机件 responsesBuildParams（3.6：响应系共用 openai-go/v3）；家差：
//   - tool_choice 仅 auto（官方 Create Response schema `enum: [auto]`；4.3.11 chat 形同口径）：
//     非 auto 降为 auto 并记降级（⛔ 不静默）；
//   - thinking.type / reasoning_split / service_tier 等特异参数经 Extra 透传（mergeExtraBody），
//     ⛔ temperature / top_p / GroupId 已被 Extra 黑名单剔除（4.3.11 不支持 / 旧版遗留）。
func (a *minimaxAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	// previous_response_id：官方 Create Response schema 明言「请求中暂不支持该参数，传入后将被
	// 忽略，响应中固定返回 null」（上游静默吞参）⇒ 统一 SDD 3.5：显式报错（⛔ 不静默丢弃）。
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, nil, err
	}
	p, err := responsesBuildParams(r, res)
	if err != nil {
		return nil, nil, err
	}
	var degs []core.Degradation
	if r.ToolChoice != nil {
		// tool_choice 仅 auto（官方 Create Response schema enum 仅 auto；4.3.11 chat 形同口径）
		// ⇒ 非 auto 降为 auto 并记降级（⛔ 不静默）。
		if r.ToolChoice.Mode != "auto" {
			degs = append(degs, core.Degradation{
				Field:  "tool_choice",
				Reason: core.DegradationCapabilityUnsupported,
				Detail: "MiniMax 仅支持 tool_choice=auto，已降为 auto",
			})
		}
		p.ToolChoice = openAIToolChoiceParam(&core.ToolChoice{Mode: "auto"})
	}
	raw, err := marshalBody(p)
	if err != nil {
		return nil, nil, err
	}
	// thinking.type / reasoning_split / service_tier 等特异参数经 Extra 透传（4.3.11）。
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
func (a *minimaxAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

func (a *minimaxAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var probe minimaxBaseRespProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	if d := minimaxCheckBaseResp(probe.baseRespOf()); d != nil {
		return &core.Response{ID: probe.ID, Model: probe.Model, Status: core.StatusFailed, Error: d}, nil
	}
	return parseResponsesBody(a.ParseUsage, body)
}

// ---- Responses 形流式（共享解码 newResponsesStreamDecoder；家差：base_resp 必检包裹）----

// streamDecoder 返回 Responses 形帧解码器（共享机件，3.7 容忍口径见 openAIStreamDecoder）。
func (a *minimaxAdapter) streamDecoder() *openAIStreamDecoder {
	return newResponsesStreamDecoder(a.ParseUsage)
}

// minimaxStreamDecode 包裹共享解码器：每帧先探 base_resp（4.3.11「流式 chunk 同样携带」+ 任务书
// 「base_resp 错误处理保持现状」；⚠️ /responses 官方 SSE 示例未见 base_resp ⇒ 保守沿用 chat 形口径），
// 业务错误 ⇒ 单帧 FrameError + 终态收口（⛔ 不静默吞掉）；其余帧原样交共享解码器。
func minimaxStreamDecode(dec func(stream.SSEEvent) ([]stream.Frame, bool, error)) func(stream.SSEEvent) ([]stream.Frame, bool, error) {
	return func(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
		var probe minimaxBaseRespProbe
		if json.Unmarshal([]byte(ev.Data), &probe) == nil {
			if det := minimaxCheckBaseResp(probe.baseRespOf()); det != nil {
				return []stream.Frame{{Kind: stream.FrameError, Status: det.Code, Text: det.Message}}, true, nil
			}
		}
		return dec(ev)
	}
}

// StreamStart：强制 stream=true ⇒ BuildRequest ⇒ 真实 HTTP ⇒ SSE 解码（照 openai.go；
// base_resp 必检包裹为 MiniMax 家差）。
func (a *minimaxAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
	return newSSEIter(ctx, resp.Body, minimaxStreamDecode(a.streamDecoder().decode), nil), nil
}
