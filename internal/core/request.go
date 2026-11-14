// 日期: 2026-11-14 11:33:06.951
// 作者: Zhai Wenjia

// Package core 承载内部规范模型：OpenAI 骨架的超集（统一 SDD 3.1）。
// 本包是纯数据结构，⛔ 不依赖 adapter / codec（统一 SDD 10.1）。
package core

import "encoding/json"

// Request 是内部规范请求模型（统一 SDD 3.1）。
// 以 OpenAI Chat Completions 骨架为基线，承载 reasoning / signature /
// cache_control / finish_reason 超集，同时容纳 Chat 与 Responses 两种入站形状。
type Request struct {
	Model    string
	Messages []Message // Chat 形状的消息序列
	// Instructions 是 Responses 形状的系统提示；Chat 侧由 messages[0] 承载。
	Instructions    string
	Tools           []Tool
	ToolChoice      *ToolChoice
	MaxOutputTokens int
	Temperature     *float64
	TopP            *float64
	Stop            []string
	Reasoning       *ReasoningConfig
	Stream          bool
	// CacheControl 是 Anthropic 的缓存断点（Chat 形状无对应物）。
	CacheControl *CacheControl
	// PreviousResponseID / Conversation 是 Responses 的服务端会话态。
	PreviousResponseID string
	Conversation       string
	// PromptCacheKey 是优化字段，始终透传给支持的上游（统一 SDD 3.5）。
	PromptCacheKey string
	// Extra 承载供应商特有的非标准参数（enable_thinking / prompt_cache_options / …）。
	Extra map[string]any
}

// Message 是规范消息（统一 SDD 3.1）。
type Message struct {
	Role    string        // system / user / assistant / tool / developer
	Content []ContentPart // 统一的多模态 part
	// ReasoningContent 承载各家推理文本。
	ReasoningContent string
	// ThinkingSignature 承载 Anthropic thinking.signature（⛔ 不可压平）。
	ThinkingSignature string
	// EncryptedContent 承载 OpenAI reasoning item 的 encrypted_content（⛔ 必须保留）。
	EncryptedContent string
	ToolCalls        []ToolCall
	ToolCallID       string // role=tool 时
	Name             string
}

// ContentPart 是统一的多模态内容部件（统一 SDD 3.1）。
type ContentPart struct {
	Type     string // text / image_url / input_text / input_image / file / …
	Text     string
	ImageURL string
	MIMEType string
	Data     string // base64
}

// ToolCall 是统一的工具调用（统一 SDD 3.1）。
type ToolCall struct {
	ID     string
	CallID string // Responses 的 call_id（与 ID 不同）
	Name   string
	// Arguments 是 JSON 字符串。
	Arguments string
	Type      string // "function"
}

// Tool 是工具声明（function 形状）。
type Tool struct {
	Type        string // "function"
	Name        string
	Description string
	// Parameters 是 JSON Schema 字符串。
	Parameters json.RawMessage
}

// ToolChoice 是工具选择指令：auto / none / required / 指定函数名。
type ToolChoice struct {
	Mode string // auto / none / required / function
	Name string // Mode=function 时的函数名
}

// ReasoningConfig 是推理（思考）控制参数的规范形态。
// 各家差异（K3 顶层 reasoning_effort、K2.x thinking.type、智谱 thinking.type 等）
// 由出站适配器映射，本结构只承载语义。
type ReasoningConfig struct {
	// Effort: none / minimal / low / medium / high / xhigh / max。
	Effort string
	// Summary: auto / concise / detailed（Responses reasoning.summary）。
	Summary string
	// Context: auto / current_turn / all_turns（Responses reasoning.context）。
	Context string
	// Mode: standard / pro（Responses reasoning.mode）。
	Mode string
}

// CacheControl 是 Anthropic 缓存断点（统一 SDD 4.3.2）。
type CacheControl struct {
	Type string // "ephemeral"
	TTL  string // "5m" / "1h"
}
