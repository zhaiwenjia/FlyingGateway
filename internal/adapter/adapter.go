// 日期: 2026-11-14 14:29:38.415
// 作者: Zhai Wenjia

// Package adapter 实现 11 家供应商的出站适配器（统一 SDD 2.2 / 3.6）：
// 每家一个适配器，独立承载请求构造、响应解析、流式帧解码（⛔ 不用一套 struct 硬扛）。
// usage 归一统一委托 internal/usage（4.4）；MiniMax base_resp 错误统一委托 internal/errors（4.3.11）。
// 请求体 / 响应体的 JSON 结构逐家自持（本包 ⛔ 不依赖 internal/codec，SDD 2.2 每家独立承载）。
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// Outbound 是出站请求（统一 SDD 3.6 适配器接口的返回形态）。
// Headers 只含非凭据头（content-type 等）；凭据值由安全层按 AuthScheme 注入（8.4），⛔ 不落此处。
type Outbound struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// OutboundShape 是实际出站形状（供 degradation 标记判定，统一 SDD 3.6）。
type OutboundShape string

const (
	// ShapeResponses 是 OpenAI Responses 形状（POST /responses）。
	ShapeResponses OutboundShape = "responses"
	// ShapeChatCompletions 是 OpenAI Chat Completions 形状（POST /chat/completions）。
	ShapeChatCompletions OutboundShape = "chat_completions"
	// ShapeAnthropicMessages 是 Anthropic Messages 形状（POST /messages）。
	ShapeAnthropicMessages OutboundShape = "anthropic_messages"
	// ShapeGeminiNative 是 Gemini 原生 generateContent 形状。
	ShapeGeminiNative OutboundShape = "gemini_native"
)

// AuthScheme 是上游鉴权头形态（统一 SDD 8.4 三种形态：api-key / x-api-key / Authorization）。
// 只声明头名与前缀，凭据值由安全层在出站请求构造处解析注入，⛔ 不在适配器间传递明文。
type AuthScheme struct {
	HeaderName string
	Prefix     string
	// ExtraHeaders 是与鉴权头同批注入的非凭据头（如 anthropic-version）。
	ExtraHeaders map[string]string
}

// Adapter 是供应商适配器接口（签名钉死，⛔ 不许改）。
type Adapter interface {
	// Provider 返回该适配器服务的供应商。
	Provider() core.Provider
	// BuildRequest 规范 Request → 该供应商的请求体 + 路径 + 鉴权头声明。
	BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error)
	// ParseResponse 供应商响应（非流式）→ 规范 Response。
	ParseResponse(res *core.Resolution, body []byte) (*core.Response, error)
	// StreamStart 流式：BuildRequest ⇒ 真实 HTTP ⇒ 按家分派流式解码 ⇒ 帧迭代器。
	StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error)
	// ParseUsage 从该供应商的 usage 结构归一化（统一委托 internal/usage.Normalize）。
	ParseUsage(raw json.RawMessage) *core.Usage
	// OutboundShape 返回实际出站形状（供 degradation 标记判定）。
	OutboundShape() OutboundShape
	// AuthScheme 返回上游鉴权头形态。
	AuthScheme() AuthScheme
	// SupportsPreviousResponseID 声明上游是否支持服务端会话（统一 SDD 3.5，⛔ 不靠运行时探测）。
	SupportsPreviousResponseID() bool
}

// DegradationAware 是可选接口：出站转换产生语义降级时（Chat → Anthropic 的 max_tokens 缺省已补、
// 仅支持 tool_choice=auto 的上游被迫降为 auto 等），把降级记录随出站产物一起返回，
// 供上层合并进 core.Response.Degradations（统一 SDD 3.4 的 x_gateway.degradations[]）。
//
// ⚠️ 为什么降级不在 ParseResponse 里记：降级发生在**请求构造期**，而 ParseResponse(res, body)
// 的入参只有识别结果与响应体（签名钉死），看不到请求是否补过默认值 ⇒ 只能在构造期随 Outbound 返回。
type DegradationAware interface {
	// BuildRequestWithDegradations 是 BuildRequest 的带降级记录变体（BuildRequest 委托它）。
	BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error)
}

// DetailError 把 core.ErrorDetail 携带为 Go error（适配器错误出口）。
// core.ErrorDetail 本身不是 error，跨层传递需要一个包装载体；上层用 errors.As 取回明细。
type DetailError struct {
	Detail *core.ErrorDetail
}

// Error 实现 error 接口。
func (e *DetailError) Error() string {
	if e == nil || e.Detail == nil {
		return "adapter: 未知错误"
	}
	return e.Detail.Message
}

// defaultClient 是 For/ForShape 构造适配器时的默认 HTTP 客户端；
// 测试用构造函数显式注入 httptest server 的 client。
var defaultClient = http.DefaultClient

// For 按 provider 返回该供应商的默认形态适配器（形态选择见 ForShape）。
func For(p core.Provider) (Adapter, error) {
	return ForShape(p, defaultShapeFor(p))
}

// ForShape 按 (provider, 上游 API 形态) 返回适配器（统一 SDD 3.6 / 任务书：形态按 core.Resolution.Shape 选）。
// 未登记的组合显式报错（⛔ 不静默回落）。
func ForShape(p core.Provider, s core.APIShape) (Adapter, error) {
	switch p {
	case core.ProviderOpenAI:
		if s == core.APIShapeOpenAIResponses {
			return NewOpenAI(defaultClient), nil
		}
	case core.ProviderAnthropic:
		if s == core.APIShapeAnthropicMessages {
			return NewAnthropic(defaultClient), nil
		}
	case core.ProviderGemini:
		if s == core.APIShapeGeminiNative {
			return NewGemini(defaultClient), nil
		}
	case core.ProviderXAI:
		if s == core.APIShapeOpenAIResponses {
			return NewXAI(defaultClient), nil
		}
	case core.ProviderDeepSeek:
		if s == core.APIShapeOpenAIResponses {
			return NewDeepSeek(defaultClient), nil
		}
	case core.ProviderMoonshot:
		if s == core.APIShapeOpenAIResponses {
			return NewMoonshot(defaultClient), nil
		}
	case core.ProviderZhipu:
		if s == core.APIShapeOpenAIResponses {
			return NewZhipu(defaultClient), nil
		}
	case core.ProviderQwen:
		if s == core.APIShapeOpenAIChat || s == core.APIShapeOpenAIResponses {
			// 兼容层出站 Responses（出站方法口径：国内供应商统一 OpenAI Responses 通道）；
			// 两个登记键归同一 responses 形适配器（⛔ 不静默换供应商）。
			return NewQwen(defaultClient), nil
		}
	case core.ProviderMiniMax:
		if s == core.APIShapeOpenAIChat || s == core.APIShapeOpenAIResponses {
			// OpenAI 兼容层出站 Responses（POST {base}/responses，国内站 api.minimax.cn/v1）；
			// 两个登记键归同一适配器。⛔ 无 Anthropic 形 / 原生 V2 分支——国内供应商统一 Responses 通道。
			return NewMiniMax(defaultClient), nil
		}
	case core.ProviderXiaomi:
		if s == core.APIShapeOpenAIResponses {
			return NewXiaomi(defaultClient), nil
		}
	case core.ProviderOllama:
		if s == core.APIShapeOpenAIChat || s == core.APIShapeOpenAIResponses {
			// 兼容层出站 Responses（POST {base}/v1/responses，v0.13.3+ 无状态 flavor）；
			// 两个登记键归同一适配器。⛔ 无原生 /api/chat 分支——Ollama 同样统一 Responses 通道。
			return NewOllamaCompat(defaultClient), nil
		}
	default:
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "unsupported_provider",
			fmt.Sprintf("没有该供应商的出站适配器: %s", p))}
	}
	return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "unsupported_shape",
		fmt.Sprintf("供应商 %s 不支持出站形状 %s（形态清单见 adapter.ForShape）", p, s))}
}

// defaultShapeFor 返回 For() 的默认登记形状（统一 SDD 3.6 / 4.1；出站方法口径：国内供应商
// 统一 OpenAI Responses 通道——OpenAI / Grok / DeepSeek / Kimi / 智谱 / Qwen / 小米 /
// MiniMax / Ollama ⇒ Responses（登记键 OpenAIResponses）；
// Claude ⇒ Messages；Gemini ⇒ 原生 generateContent（其 OpenAI 兼容层无 /responses）。
// ⛔ 国内供应商无原生形状分支（Ollama /api/chat、Qwen 原生、MiniMax V2 / Anthropic 形已移除）。
func defaultShapeFor(p core.Provider) core.APIShape {
	switch p {
	case core.ProviderAnthropic:
		return core.APIShapeAnthropicMessages
	case core.ProviderGemini:
		return core.APIShapeGeminiNative
	default:
		return core.APIShapeOpenAIResponses
	}
}

// ---- 共享小工具（协议无关的机械件；各家的请求构造 / 响应解析仍逐家自持）----

// joinURL 拼 base URL 与路径（base 可能带尾斜杠）。
func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

// checkPreviousResponseID 按统一 SDD 3.5：请求带 previous_response_id 而上游不支持 ⇒
// 立即显式报 400 unsupported_capability（⛔ 不静默丢弃；previous_response_id 是语义字段）。
func checkPreviousResponseID(supports bool, r *core.Request) error {
	if r.PreviousResponseID == "" || supports {
		return nil
	}
	return &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "unsupported_capability",
		"该上游不支持服务端会话，请改为客户端带全量历史")}
}

// extraBlocklist 是 Extra 透传的黑名单：这些键要么由 core 字段承载、要么是 ⛔ 明令禁发的
// 遗留参数（prompt_cache_breakpoint 会 400，GroupId 是 MiniMax 旧版遗留，4.3.9 / 4.3.11）。
var extraBlocklist = map[string]bool{
	"prompt_cache_breakpoint": true, // Moonshot：显式断点被拒（400），4.3.9
	"group_id":                true, // MiniMax 旧版遗留，⛔ 不传，4.3.11
	"GroupId":                 true,
	"model":                   true, // 由 core.Request.Model 承载
	"messages":                true,
	"input":                   true,
	"stream":                  true,
	"temperature":             true, // 由 core.Request.Temperature 承载（且 MiniMax/Anthropic 需剔除）
	"top_p":                   true,
	"top_k":                   true,
	"max_tokens":              true,
	"stream_options":          true, // 由各家适配器按 4.1.3 口径自置
}

// mergeExtraBody 把 core.Request.Extra 的供应商特有参数并入已序列化的请求体（顶层键，3.1：
// Extra 承载 enable_thinking / prompt_cache_options 等非标准参数），黑名单键 ⛔ 不发。
func mergeExtraBody(raw []byte, extra map[string]any) ([]byte, error) {
	if len(extra) == 0 {
		return raw, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_marshal_failed",
			fmt.Sprintf("请求体反序列化失败（Extra 并入）: %v", err))}
	}
	for k, v := range extra {
		if extraBlocklist[k] {
			continue
		}
		m[k] = v
	}
	return marshalBody(m)
}

// usageTreeShape 按 usage 树的判别字段选 usage.Normalize 的口径形状（4.4.2 逐家字段对照总表）：
// 有 prompt_tokens ⇒ OpenAI Chat 树（逐家 vendor 解析器）；否则按 Responses 树（input_tokens）。
// 同一家两种 usage 树并存时（4.3.x「兼容模式 vs 原生模式字段名不同」），按实际字段选口径。
func usageTreeShape(raw json.RawMessage) core.APIShape {
	if jsonHasKey(raw, "prompt_tokens") {
		return core.APIShapeOpenAIChat
	}
	return core.APIShapeOpenAIResponses
}

// jsonHasKey 判断原始 JSON 对象顶层是否含指定字段（键存在即算，值可为 0）。
func jsonHasKey(raw json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// contentText 把规范 ContentPart 序列拼成纯文本（各家文本承载字段的统一取法）。
func contentText(parts []core.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// marshalBody 序列化请求体（无缩进，与上游文档的字段名逐字对应由 struct tag 保证）。
func marshalBody(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_marshal_failed",
			fmt.Sprintf("请求体序列化失败: %v", err))}
	}
	return b, nil
}
