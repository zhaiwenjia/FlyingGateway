// 日期: 2026-11-14 11:33:06.951
// 作者: Zhai Wenjia

package core

// Provider 是供应商枚举（统一 SDD 3.3）。
type Provider int

const (
	ProviderUnknown Provider = iota
	ProviderOpenAI
	ProviderAnthropic
	ProviderDeepSeek
	ProviderMoonshot // Kimi
	ProviderZhipu    // 智谱 GLM
	ProviderQwen     // DashScope
	ProviderMiniMax
	ProviderXiaomi // MiMo
	ProviderXAI    // Grok
	ProviderGemini
	ProviderOllama
)

// String 返回 provider 名（OTel gen_ai.provider.name 口径，统一 SDD 6.2）。
func (p Provider) String() string {
	switch p {
	case ProviderOpenAI:
		return "openai"
	case ProviderAnthropic:
		return "anthropic"
	case ProviderDeepSeek:
		return "deepseek"
	case ProviderMoonshot:
		return "moonshot"
	case ProviderZhipu:
		return "zhipu"
	case ProviderQwen:
		return "qwen"
	case ProviderMiniMax:
		return "minimax"
	case ProviderXiaomi:
		return "xiaomi"
	case ProviderXAI:
		return "xai"
	case ProviderGemini:
		return "gemini"
	case ProviderOllama:
		return "ollama"
	default:
		return "unknown"
	}
}

// APIShape 是上游 API 形状（统一 SDD 3.3 / 4.1）。
type APIShape int

const (
	APIShapeUnknown APIShape = iota
	APIShapeOpenAIChat
	APIShapeOpenAIResponses
	APIShapeAnthropicMessages
	APIShapeGeminiNative    // generateContent / streamGenerateContent
	APIShapeOllamaNative    // /api/chat（NDJSON）
	APIShapeMiniMaxNativeV2 // /v1/text/chatcompletion_v2
	APIShapeQwenNative      // /api/v1/services/aigc/text-generation/generation
)

// String 返回形状名（供 Trace gateway.shape 使用）。
func (s APIShape) String() string {
	switch s {
	case APIShapeOpenAIChat:
		return "openai_chat"
	case APIShapeOpenAIResponses:
		return "openai_responses"
	case APIShapeAnthropicMessages:
		return "anthropic_messages"
	case APIShapeGeminiNative:
		return "gemini_native"
	case APIShapeOllamaNative:
		return "ollama_native"
	case APIShapeMiniMaxNativeV2:
		return "minimax_native_v2"
	case APIShapeQwenNative:
		return "qwen_native"
	default:
		return "unknown"
	}
}

// DetectConfidence 是识别置信度（统一 SDD 3.3）：host 命中 = high，
// model 前缀命中 = medium，都不中 = fallback。
type DetectConfidence int

const (
	ConfidenceFallback DetectConfidence = iota
	ConfidenceMedium
	ConfidenceHigh
)

// String 返回置信度名（供 Trace gateway.detect_confidence 使用）。
func (c DetectConfidence) String() string {
	switch c {
	case ConfidenceHigh:
		return "high"
	case ConfidenceMedium:
		return "medium"
	default:
		return "fallback"
	}
}

// Resolution 是识别 / 路由决策结果（统一 SDD 3.3）。
// 同一输入 + 同一份注册表 + 同一份配置 ⇒ 同一个 Resolution（1.4.3 可复现）。
type Resolution struct {
	// Provider 是判定的供应商。
	Provider Provider
	// Shape 是要调用的上游 API 形状。
	Shape APIShape
	// BaseURL 是上游 base URL（来自端点注册表）。
	BaseURL string
	// Model 是实际发往上游的模型标识（Endpoint.UpstreamModel）。
	Model string
	// CredentialRef 只存来源引用（环境变量名 / 密钥别名），⛔ 不存明文。
	CredentialRef string
	// Confidence：host 命中 = high，前缀命中 = medium，都不中 = fallback。
	Confidence DetectConfidence
	// Fallback = true 表示走 OpenAI 兼容兜底。
	Fallback bool
	// SupportsPreviousResponseID 由 adapter 声明（⛔ 不靠运行时探测），统一 SDD 3.5。
	SupportsPreviousResponseID bool
}
