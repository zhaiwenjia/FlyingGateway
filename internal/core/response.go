// 日期: 2026-11-14 11:33:06.951
// 作者: Zhai Wenjia

package core

// Response 是内部规范响应模型（统一 SDD 3.1）。
type Response struct {
	ID     string
	Model  string
	Status string // 见 Status* 常量（六值）
	// OutputItems 是异构联合，承载 Chat 的单条 message 与 Responses 的多 item。
	OutputItems []OutputItem
	// Text 是便捷字段（对应 Responses 的 output_text）。
	Text         string
	FinishReason string // 超集枚举，见 FinishReason* 常量
	Usage        *Usage
	Error        *ErrorDetail
	// Degradations 承载不可逆转换的显式标记（统一 SDD 3.4 的 x_gateway.degradations[]）。
	Degradations []Degradation
}

// Status 六值（统一 SDD 4.2.2 / 6.2）。
const (
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
	StatusIncomplete = "incomplete"
	StatusCancelled  = "cancelled"
	StatusInProgress = "in_progress"
	StatusQueued     = "queued"
)

// FinishReason 超集枚举（统一 SDD 4.5.1）：
// 归一化枚举必须容纳全部非标准取值（⛔ 不能只认 OpenAI 的三到五个）。
const (
	FinishStop                       = "stop"
	FinishLength                     = "length"
	FinishToolCalls                  = "tool_calls"
	FinishContentFilter              = "content_filter"
	FinishFunctionCall               = "function_call"
	FinishAborted                    = "aborted"                       // DeepSeek 独有
	FinishInsufficientSystemResource = "insufficient_system_resource"  // DeepSeek 独有
	FinishSensitive                  = "sensitive"                     // 智谱独有（内容安全拦截）
	FinishNetworkError               = "network_error"                 // 智谱独有
	FinishRepetitionTruncation       = "repetition_truncation"         // MiMo 独有（模型自检到重复）
	FinishModelContextWindowExceeded = "model_context_window_exceeded" // Claude / 智谱（超上下文）
	FinishEndTurn                    = "end_turn"                      // Claude 七值体系
	FinishMaxTokens                  = "max_tokens"
	FinishStopSequence               = "stop_sequence"
	FinishToolUse                    = "tool_use"
	FinishPauseTurn                  = "pause_turn"
	FinishRefusal                    = "refusal"
	FinishLoad                       = "load"   // Ollama done_reason
	FinishUnload                     = "unload" // Ollama done_reason
	FinishUnknown                    = "unknown"
)

// OutputItem 是输出异构联合体的一项（统一 SDD 3.1 + 4.3.1 的 output[] 语义）。
type OutputItem struct {
	Type    string // message / reasoning / function_call / thinking / tool_use / …
	ID      string
	Role    string
	Content []ContentPart
	// Summary 承载 reasoning item 的 summary[]（Responses）。
	Summary []SummaryPart
	// ReasoningText 承载推理正文（reasoning item 的 content[] / Anthropic thinking 块）。
	ReasoningText string
	// EncryptedContent 承载 reasoning item 的 encrypted_content（⛔ 必须保留）。
	EncryptedContent string
	// ThinkingSignature 承载 Anthropic thinking 块的 signature（⛔ 不可压平）。
	ThinkingSignature string
	// FunctionCall 相关
	CallID    string
	Name      string
	Arguments string
	// Refusal
	Refusal string
}

// SummaryPart 是 reasoning summary 的一段（统一 SDD 4.3.1：{type:"summary_text", text}）。
type SummaryPart struct {
	Type string // "summary_text"
	Text string
}

// Degradation 是一次不可逆语义丢失的显式标记（统一 SDD 3.4 的 x_gateway.degradations[]）。
type Degradation struct {
	// Field 是被丢失的语义字段名（thinking_signature / encrypted_content /
	// previous_response_id / reasoning_summary 等）。
	Field string
	// Reason 是丢失原因分类（upstream_shape_mismatch /
	// response_to_chat_conversion / capability_unsupported）。
	Reason string
	// Detail 是人可读说明（给排障看）。
	Detail string
}

// Degradation 原因分类（统一 SDD 3.4）。
const (
	DegradationUpstreamShapeMismatch = "upstream_shape_mismatch"
	DegradationResponseToChat        = "response_to_chat_conversion"
	DegradationCapabilityUnsupported = "capability_unsupported"
)
