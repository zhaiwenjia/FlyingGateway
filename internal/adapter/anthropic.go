// 日期: 2026-11-14 16:58:48.735
// 作者: Zhai Wenjia

package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// Anthropic 适配器（统一 SDD 4.3.2「原生形状，不可压平」）：
// 出站原生 Messages：POST {base}/messages；AuthScheme: x-api-key + anthropic-version: 2026-10-21。
// ⚠️ API version 2026-10-21 起 temperature / top_p / top_k 一律 400 ⇒ 构造时必须剔除（4.3.2）。
// max_tokens 必填：core 无值 ⇒ 给默认值并记 Degradation「max_tokens 缺省已补」（4.2.4 / 11.2）。
// thinking 块的 signature ⛔ 不可压平（4.2.5：thinking block 必须原样回传，修改即 400）。
// usage 的 input_tokens 是残值 ⇒ 归一须三项合成（4.4.3），委托 usage.Normalize(APIShapeAnthropicMessages)。
// 流式：message_start 初始 usage + message_delta 累计 usage（⛔ 不逐帧相加）；无 [DONE]。
// previous_response_id 支持 = false（3.5 表）。
//
// SDK 化（3.6 ① 优先级：官方 anthropic-sdk-go，Tier 1 全 SDK）：
// BuildRequest 用 SDK typed 参数（MessageNewParams）构造序列化 Outbound.Body；
// ParseResponse 用 SDK 响应类型（Message）解析；StreamStart 经 SDK 客户端
//（自定义 base URL + 注入现有 *http.Client）流式执行、SDK 流事件 ⇒ stream.Frame。
// ⚠️ SDK 序列化与既有 wire 的合法差异在 anthropicWireNormalize 归一回契约形状（逐条见其注释）。
//
// MiniMax 的 Anthropic 兼容端点复用本文件的手写构造 / 解析 / 流式解码
//（anthropicRequestBody / parseAnthropicMessage / newAnthropicStreamDecoder，
// 3.6：适配器可复用 Anthropic 解析器；签名与行为不变）。

// anthropicDefaultMaxTokens 是 max_tokens 缺省补值（SDD 未给定数值；取 Anthropic 常用上限档位之一，
// 仅在 core.Request.MaxOutputTokens 无值时补，且必然伴随 Degradation 记录，⛔ 不静默）。
const anthropicDefaultMaxTokens = 4096

type anthropicAdapter struct {
	hc *http.Client
}

// NewAnthropic 构造 Anthropic 适配器（hc 为出站 HTTP 客户端）。
func NewAnthropic(hc *http.Client) Adapter { return &anthropicAdapter{hc: hc} }

func (a *anthropicAdapter) Provider() core.Provider          { return core.ProviderAnthropic }
func (a *anthropicAdapter) OutboundShape() OutboundShape     { return ShapeAnthropicMessages }
func (a *anthropicAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 8.4 / 2.3 鉴权头三形态表：x-api-key + anthropic-version: 2026-10-21（4.3.2）。
func (a *anthropicAdapter) AuthScheme() AuthScheme {
	return AuthScheme{
		HeaderName:   "x-api-key",
		Prefix:       "",
		ExtraHeaders: map[string]string{"anthropic-version": "2026-10-21"},
	}
}

// ParseUsage 委托 usage.Normalize：shape=APIShapeAnthropicMessages ⇒ 残值语义（4.4.3：
// total_input = cache_read + cache_creation + input_tokens）。
func (a *anthropicAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderAnthropic, core.APIShapeAnthropicMessages, raw)
}

// ---- 手写请求体（MiniMax Anthropic 兼容端点使用；Claude 走 SDK typed 构造 anthropicSDKParams。
// 4.3.2 请求体关键字段逐字：model / messages / max_tokens / system / tools /
// tool_choice / stream / thinking / cache_control）----

type anthropicReq struct {
	Model      string             `json:"model"`
	Messages   []anthropicMessage `json:"messages"`
	MaxTokens  int                `json:"max_tokens"`
	System     string             `json:"system,omitempty"`
	Tools      []anthropicTool    `json:"tools,omitempty"`
	ToolChoice any                `json:"tool_choice,omitempty"`
	Stream     bool               `json:"stream,omitempty"`
	Thinking   any                `json:"thinking,omitempty"`
	// ⛔ temperature / top_p / top_k 不设字段（2026-10-21 起传值即 400，4.3.2）。
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock 是 content[] 内容块（4.3.2：text / thinking / redacted_thinking / tool_use / tool_result）。
type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// thinking 块（⛔ signature 不可压平，4.2.5）
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// redacted_thinking 块
	Data string `json:"data,omitempty"`
	// tool_use 块
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`
	// tool_result 块
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
	// cache_control 缓存断点（4.3.2：{type:"ephemeral", ttl:"5m"|"1h"}）
	CacheControl *core.CacheControl `json:"cache_control,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicRequestBody 是 Anthropic 形请求体构造（MiniMax Anthropic 兼容端点使用，3.6 允许复用；
// Claude 的 BuildRequest 走 SDK typed 构造 anthropicSDKParams，两侧语义由 anthropic_test /
// minimax_test 各自钉住）。
// 返回构造体与转换期 degradation 记录（max_tokens 缺省已补等）。
func anthropicRequestBody(r *core.Request) (*anthropicReq, []core.Degradation, error) {
	body := &anthropicReq{
		Model:     r.Model,
		MaxTokens: r.MaxOutputTokens,
		Stream:    r.Stream,
	}
	var degs []core.Degradation
	if body.MaxTokens == 0 {
		// max_tokens 必填（4.2.4）：core 无值 ⇒ 补默认值并显式记录降级（11.2 Chat → Anthropic 行）。
		body.MaxTokens = anthropicDefaultMaxTokens
		degs = append(degs, core.Degradation{
			Field:  "max_tokens",
			Reason: core.DegradationUpstreamShapeMismatch,
			Detail: "max_tokens 缺省已补",
		})
	}
	if r.Reasoning != nil {
		// thinking 映射 core.ReasoningConfig（4.3.2：thinking: {type:"adaptive", display:"summarized"}）。
		body.Thinking = map[string]any{"type": "adaptive", "display": "summarized"}
	}
	for _, m := range r.Messages {
		msgs, sys, err := anthropicMessageOf(m)
		if err != nil {
			return nil, nil, err
		}
		if sys != "" {
			if body.System == "" {
				body.System = sys
			} else {
				body.System += "\n" + sys
			}
			continue
		}
		body.Messages = append(body.Messages, msgs...)
	}
	// cache_control 断点（4.3.2）：core.Request.CacheControl 落到最后一个消息块（缓存整个前缀）。
	if r.CacheControl != nil && len(body.Messages) > 0 {
		last := &body.Messages[len(body.Messages)-1]
		if n := len(last.Content); n > 0 {
			last.Content[n-1].CacheControl = r.CacheControl
		}
	}
	for _, t := range r.Tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		body.Tools = append(body.Tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	if r.ToolChoice != nil {
		body.ToolChoice = anthropicToolChoice(r.ToolChoice)
	}
	return body, degs, nil
}

// anthropicMessageOf 把规范消息映射为 Anthropic messages（system ⇒ 顶层 system，4.2.4）。
func anthropicMessageOf(m core.Message) ([]anthropicMessage, string, error) {
	if m.Role == "system" {
		return nil, contentText(m.Content), nil
	}
	if m.Role == "tool" {
		// 工具结果是 user 角色的 tool_result 块（4.2.4：tool_calls[] → tool_use 结构转换）。
		return []anthropicMessage{{
			Role:    "user",
			Content: []anthropicBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: contentText(m.Content)}},
		}}, "", nil
	}
	role := m.Role
	if role == "developer" {
		role = "user" // Anthropic 无 developer 角色
	}
	var blocks []anthropicBlock
	if m.ThinkingSignature != "" || m.ReasoningContent != "" {
		// thinking 块原样回传（⛔ signature 不可压平，4.2.5：修改即 400 invalid_request_error）。
		blocks = append(blocks, anthropicBlock{
			Type: "thinking", Thinking: m.ReasoningContent, Signature: m.ThinkingSignature,
		})
	}
	for _, p := range m.Content {
		if p.Text != "" {
			blocks = append(blocks, anthropicBlock{Type: "text", Text: p.Text})
		}
	}
	for _, tc := range m.ToolCalls {
		var input any
		if tc.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
				return nil, "", &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "invalid_tool_arguments",
					"tool_call arguments 不是合法 JSON，无法映射为 Anthropic tool_use.input")}
			}
		}
		if input == nil {
			input = map[string]any{}
		}
		blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
	}
	if len(blocks) == 0 {
		return nil, "", nil
	}
	return []anthropicMessage{{Role: role, Content: blocks}}, "", nil
}

// anthropicToolChoice 映射 tool_choice（Anthropic：auto / none / any / tool）。
func anthropicToolChoice(tc *core.ToolChoice) any {
	switch tc.Mode {
	case "none":
		return map[string]any{"type": "none"}
	case "required":
		return map[string]any{"type": "any"}
	case "function":
		if tc.Name != "" {
			return map[string]any{"type": "tool", "name": tc.Name}
		}
		return map[string]any{"type": "auto"}
	default:
		return map[string]any{"type": "auto"}
	}
}

// ---- SDK typed 请求构造（Tier 1：anthropic.MessageNewParams 为构造真值，3.6「SDK 构造」）----

// anthropicSDKParams 把规范 Request 构造为 anthropic-sdk-go 的 MessageNewParams（typed）：
// 语义与 anthropicRequestBody 逐条一致（system 顶层、thinking 块 signature 原样、
// tool_result 形态、max_tokens 缺省补值 + degradation、⛔ temperature/top_p/top_k）；
// 返回值经 SDK MarshalJSON 序列化后由 anthropicWireNormalize 归一回既有 wire 契约。
// schemas 是各 tool 的原始 JSON Schema（typed *Schema 会丢未知键 ⇒ 序列化后原样回填）。
func anthropicSDKParams(r *core.Request, res *core.Resolution) (anthropic.MessageNewParams, []core.Degradation, []json.RawMessage, error) {
	var degs []core.Degradation
	maxTokens := r.MaxOutputTokens
	if maxTokens == 0 {
		// max_tokens 必填（4.2.4）：core 无值 ⇒ 补默认值并显式记录降级（11.2）。
		maxTokens = anthropicDefaultMaxTokens
		degs = append(degs, core.Degradation{
			Field:  "max_tokens",
			Reason: core.DegradationUpstreamShapeMismatch,
			Detail: "max_tokens 缺省已补",
		})
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(res.Model),
		MaxTokens: int64(maxTokens),
	}
	if r.Reasoning != nil {
		// thinking 映射 core.ReasoningConfig（4.3.2：thinking: {type:"adaptive", display:"summarized"}）。
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{
				Display: anthropic.ThinkingConfigAdaptiveDisplaySummarized,
			},
		}
	}
	for _, m := range r.Messages {
		msgParams, sys, err := anthropicMessageParamOf(m)
		if err != nil {
			return params, nil, nil, err
		}
		if sys != "" {
			if len(params.System) == 0 {
				params.System = []anthropic.TextBlockParam{{Text: sys}}
			} else {
				params.System[0].Text += "\n" + sys
			}
			continue
		}
		params.Messages = append(params.Messages, msgParams...)
	}
	var schemas []json.RawMessage
	for _, t := range r.Tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		// typed *Schema 只承载标准 JSON Schema 键，未知键（$schema / additionalProperties 等）会丢 ⇒
		// 这里用 typed 解析做形状校验，wire 上的 input_schema 由 anthropicWireNormalize 原样回填。
		var typed anthropic.ToolInputSchemaParam
		if err := json.Unmarshal(schema, &typed); err != nil {
			return params, nil, nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "invalid_tool_schema",
				"tool parameters 不是合法 JSON Schema，无法映射为 Anthropic tool input_schema")}
		}
		tool := &anthropic.ToolParam{Name: t.Name, InputSchema: typed}
		if t.Description != "" {
			tool.Description = param.NewOpt(t.Description)
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: tool})
		schemas = append(schemas, schema)
	}
	if r.ToolChoice != nil {
		params.ToolChoice = anthropicToolChoiceParam(r.ToolChoice)
	}
	return params, degs, schemas, nil
}

// anthropicMessageParamOf 把规范消息映射为 SDK 的 MessageParam（与 anthropicMessageOf 同语义）。
func anthropicMessageParamOf(m core.Message) ([]anthropic.MessageParam, string, error) {
	if m.Role == "system" {
		return nil, contentText(m.Content), nil
	}
	if m.Role == "tool" {
		// 工具结果是 user 角色的 tool_result 块（4.2.4）。
		return []anthropic.MessageParam{{
			Role: anthropic.MessageParamRoleUser,
			Content: []anthropic.ContentBlockParamUnion{{
				OfToolResult: &anthropic.ToolResultBlockParam{
					ToolUseID: m.ToolCallID,
					Content: []anthropic.ToolResultBlockParamContentUnion{
						{OfText: &anthropic.TextBlockParam{Text: contentText(m.Content)}},
					},
				},
			}},
		}}, "", nil
	}
	role := anthropic.MessageParamRole(m.Role)
	if m.Role == "developer" {
		role = anthropic.MessageParamRoleUser // Anthropic 无 developer 角色
	}
	var blocks []anthropic.ContentBlockParamUnion
	if m.ThinkingSignature != "" || m.ReasoningContent != "" {
		// thinking 块原样回传（⛔ signature 不可压平，4.2.5）。
		blocks = append(blocks, anthropic.ContentBlockParamUnion{
			OfThinking: &anthropic.ThinkingBlockParam{Thinking: m.ReasoningContent, Signature: m.ThinkingSignature},
		})
	}
	for _, p := range m.Content {
		if p.Text != "" {
			blocks = append(blocks, anthropic.ContentBlockParamUnion{
				OfText: &anthropic.TextBlockParam{Text: p.Text},
			})
		}
	}
	for _, tc := range m.ToolCalls {
		var input any
		if tc.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
				return nil, "", &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "invalid_tool_arguments",
					"tool_call arguments 不是合法 JSON，无法映射为 Anthropic tool_use.input")}
			}
		}
		if input == nil {
			input = map[string]any{}
		}
		blocks = append(blocks, anthropic.ContentBlockParamUnion{
			OfToolUse: &anthropic.ToolUseBlockParam{ID: tc.ID, Name: tc.Name, Input: input},
		})
	}
	if len(blocks) == 0 {
		return nil, "", nil
	}
	return []anthropic.MessageParam{{Role: role, Content: blocks}}, "", nil
}

// anthropicToolChoiceParam 映射 tool_choice（与 anthropicToolChoice 同语义：auto / none / any / tool）。
func anthropicToolChoiceParam(tc *core.ToolChoice) anthropic.ToolChoiceUnionParam {
	switch tc.Mode {
	case "none":
		return anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
	case "required":
		return anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
	case "function":
		if tc.Name != "" {
			return anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: tc.Name}}
		}
		return anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	default:
		return anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	}
}

// anthropicWireNormalize 把 SDK 序列化产物归一回适配器既有 wire 契约（SDK 形态的合法差异，
// 逐条列明；不归一会改变线上 wire 的都归一，行为不变红线以既有测试为门）：
//
// ① system：SDK 的 System 是 []TextBlockParam（块数组形态），既有 wire 是顶层 string（4.2.4）
//
//	⇒ 归一为 string（多段以 "\n" 连接，与 anthropicRequestBody 同口径）；空则删键。
//
// ② tool_result.content：SDK 的 Content 是 []ToolResultBlockParamContentUnion（块数组），
//
//	既有 wire 是 string ⇒ 归一为 string（text 拼接，与 contentText 同口径）；空则删键。
//
// ③ cache_control 断点注入最后一个消息块（4.3.2）：SDK 仅 text/tool_use/tool_result 参数带
//
//	cache_control 字段，thinking / redacted_thinking 参数没有 ⇒ 统一在序列化后注入，
//	形状逐字 {type, ttl} 小写（2026-11-14 刚修的 core.CacheControl tag 契约，⛔ 不许回到 Go 字段名）。
//
// ④ stream：SDK 的 MessageNewParams 无 stream 字段（流式由 NewStreaming 注入）⇒
//
//	r.Stream 时回填 "stream":true，保持既有 wire。
//
// ⑤ tool input_schema：typed ToolInputSchemaParam 会丢未知 JSON Schema 键 ⇒ 原始 schema
//
//	字节原样回填（⛔ 不重排键序）。
//
// ⑥ 空串键：SDK 的 thinking/signature/tool_use_id 等响应侧字段无 omitempty ⇒ 空值会序列化出
//
//	`"key":""`，既有 wire 用 omitempty 不出键 ⇒ 删掉内容块上的空串键。
func anthropicWireNormalize(raw []byte, r *core.Request, schemas []json.RawMessage) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_marshal_failed",
			"SDK 请求体反序列化失败（wire 归一）")}
	}
	// ① system 块数组 ⇒ string。
	if arr, ok := m["system"].([]any); ok {
		var texts []string
		for _, b := range arr {
			if bm, ok := b.(map[string]any); ok {
				if s, ok := bm["text"].(string); ok && s != "" {
					texts = append(texts, s)
				}
			}
		}
		if len(texts) > 0 {
			m["system"] = strings.Join(texts, "\n")
		} else {
			delete(m, "system")
		}
	}
	// 消息块遍历（② tool_result content 归一 + ⑥ 空串键剔除 + ③ cache_control 注入）。
	msgs, _ := m["messages"].([]any)
	for _, mv := range msgs {
		msg, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		blocks, _ := msg["content"].([]any)
		for _, bv := range blocks {
			blk, ok := bv.(map[string]any)
			if !ok {
				continue
			}
			// ⑥ 内容块空串键 ⇒ 不出键（对齐手写 struct 的 omitempty）。
			for _, k := range []string{"text", "thinking", "signature", "data", "id", "name", "tool_use_id"} {
				if s, ok := blk[k].(string); ok && s == "" {
					delete(blk, k)
				}
			}
			// ② tool_result.content 块数组 ⇒ string。
			if blk["type"] == "tool_result" {
				if arr, ok := blk["content"].([]any); ok {
					var t string
					for _, c := range arr {
						if cm, ok := c.(map[string]any); ok {
							if s, ok := cm["text"].(string); ok {
								t += s
							}
						}
					}
					if t == "" {
						delete(blk, "content")
					} else {
						blk["content"] = t
					}
				}
			}
		}
	}
	// ③ cache_control 断点落最后一个消息块（缓存整个前缀，4.3.2）。
	if r.CacheControl != nil && len(msgs) > 0 {
		msg, ok := msgs[len(msgs)-1].(map[string]any)
		if ok {
			if blocks, ok := msg["content"].([]any); ok && len(blocks) > 0 {
				if blk, ok := blocks[len(blocks)-1].(map[string]any); ok {
					cc := map[string]any{"type": r.CacheControl.Type}
					if r.CacheControl.TTL != "" {
						cc["ttl"] = r.CacheControl.TTL
					}
					blk["cache_control"] = cc
				}
			}
		}
	}
	// ④ stream 标志。
	if r.Stream {
		m["stream"] = true
	}
	// ⑤ tool input_schema 原样回填（Anthropic 的 tools[] 元素即 {name, description, input_schema}）。
	if len(schemas) > 0 {
		tools, _ := m["tools"].([]any)
		for i, tv := range tools {
			if i >= len(schemas) {
				break
			}
			if tool, ok := tv.(map[string]any); ok {
				tool["input_schema"] = schemas[i]
			}
		}
	}
	return marshalBody(m)
}

// BuildRequestWithDegradations 构造请求并返回转换期 degradation（接口 DegradationAware）。
// Tier 1：SDK typed 参数构造 ⇒ SDK 序列化 ⇒ anthropicWireNormalize 归一回既有 wire。
func (a *anthropicAdapter) BuildRequestWithDegradations(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, []core.Degradation, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, nil, err
	}
	params, degs, schemas, err := anthropicSDKParams(r, res)
	if err != nil {
		return nil, nil, err
	}
	raw, err := marshalBody(params)
	if err != nil {
		return nil, nil, err
	}
	raw, err = anthropicWireNormalize(raw, r, schemas)
	if err != nil {
		return nil, nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/messages"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, degs, nil
}

// BuildRequest 委托 BuildRequestWithDegradations（签名钉死的接口方法）。
func (a *anthropicAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	out, _, err := a.BuildRequestWithDegradations(ctx, r, res)
	return out, err
}

// ---- 手写响应体（MiniMax Anthropic 兼容端点使用；Claude 走 SDK 响应类型解析）----

type anthropicResp struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Model   string `json:"model"`
	Content []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		Data      string          `json:"data"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason   string          `json:"stop_reason"`
	StopSequence string          `json:"stop_sequence"`
	Usage        json.RawMessage `json:"usage"`
}

// parseAnthropicMessage 是 Anthropic 形响应解析（MiniMax Anthropic 兼容端点使用，3.6 允许复用；
// Claude 的 ParseResponse 走 SDK 响应类型 anthropic.Message 解析）。
// usageFn 由所属适配器注入（usage 语义各家仍需各自核对，4.3.11：MiniMax 的 input_tokens 也是残值）。
func parseAnthropicMessage(usageFn func(json.RawMessage) *core.Usage, body []byte) (*core.Response, error) {
	var r anthropicResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{ID: r.ID, Model: r.Model}
	var text string
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text += b.Text
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "message", Role: "assistant",
				Content: []core.ContentPart{{Type: "text", Text: b.Text}},
			})
		case "thinking":
			// signature ⇒ core.OutputItem.ThinkingSignature（⛔ 不可压平，4.2.5）。
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "thinking", ReasoningText: b.Thinking, ThinkingSignature: b.Signature,
			})
		case "redacted_thinking":
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "redacted_thinking", EncryptedContent: b.Data,
			})
		case "tool_use":
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "tool_use", ID: b.ID, CallID: b.ID, Name: b.Name, Arguments: string(b.Input),
			})
		default:
			out.OutputItems = append(out.OutputItems, core.OutputItem{Type: b.Type, ID: b.ID})
		}
	}
	out.Text = text
	// stop_reason 七值 ⇒ 超集原值（4.5.2：与 OpenAI finish_reason 不是同一套，映射有损 ⇒ 保留原值）。
	out.FinishReason = r.StopReason
	if out.FinishReason == "" {
		out.FinishReason = core.FinishUnknown
	}
	switch r.StopReason {
	case "max_tokens", "model_context_window_exceeded":
		out.Status = core.StatusIncomplete
	default:
		out.Status = core.StatusCompleted
	}
	if len(r.Usage) > 0 {
		out.Usage = usageFn(r.Usage)
	}
	return out, nil
}

// ParseResponse 用 SDK 响应类型 anthropic.Message 解析（Tier 1，3.6「SDK 响应类型解析」），
// 语义与 parseAnthropicMessage 逐条一致：content[] 块数组、stop_reason 七值超集原值、
// thinking signature ⛔ 不可压平；usage 取原始 JSON 字节（usage.Normalize.Raw 逐字保留）。
func (a *anthropicAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var msg anthropic.Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{ID: msg.ID, Model: string(msg.Model)}
	var text string
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			text += b.Text
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "message", Role: "assistant",
				Content: []core.ContentPart{{Type: "text", Text: b.Text}},
			})
		case "thinking":
			// signature ⇒ core.OutputItem.ThinkingSignature（⛔ 不可压平，4.2.5）。
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "thinking", ReasoningText: b.Thinking, ThinkingSignature: b.Signature,
			})
		case "redacted_thinking":
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "redacted_thinking", EncryptedContent: b.Data,
			})
		case "tool_use":
			// Input 是 json.RawMessage（SDK 联合体字段）⇒ tool_use.input 字节原样保留。
			out.OutputItems = append(out.OutputItems, core.OutputItem{
				Type: "tool_use", ID: b.ID, CallID: b.ID, Name: b.Name, Arguments: string(b.Input),
			})
		default:
			out.OutputItems = append(out.OutputItems, core.OutputItem{Type: b.Type, ID: b.ID})
		}
	}
	out.Text = text
	// stop_reason 七值 ⇒ 超集原值（4.5.2；SDK 的 StopReason 是字符串类型，未知取值原样保留）。
	out.FinishReason = string(msg.StopReason)
	if out.FinishReason == "" {
		out.FinishReason = core.FinishUnknown
	}
	switch msg.StopReason {
	case "max_tokens", "model_context_window_exceeded":
		out.Status = core.StatusIncomplete
	default:
		out.Status = core.StatusCompleted
	}
	// usage 原文逐字进 usage.Normalize.Raw ⇒ 取原始 JSON 字节（⛔ 不用 typed 重序列化）。
	if raw := json.RawMessage(msg.JSON.Usage.Raw()); len(raw) > 0 {
		out.Usage = a.ParseUsage(raw)
	}
	return out, nil
}

// ---- 手写流式（MiniMax Anthropic 兼容端点使用；Claude 走 SDK 流事件 anthropicSDKIter）----

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Name      string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"` // text_delta / thinking_delta / signature_delta / input_json_delta
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage json.RawMessage `json:"usage"` // message_delta：⚠️ 累计值，⛔ 不逐帧相加（4.3.2）
}

// anthropicStreamDecoder 是 Anthropic 形帧解码器（MiniMax Anthropic 兼容端点使用；
// Claude 走 SDK 流事件转换器 anthropicSDKIter，帧语义两侧一致、由 anthropic_test /
// minimax_test 各自钉住）。
type anthropicStreamDecoder struct {
	usageFn    func(json.RawMessage) *core.Usage
	toolIDs    map[int]string // content block index → tool_use id
	startUsage *core.Usage    // message_start 的初始 usage（message_delta 缺输入侧时合并）
	stopReason string
}

func newAnthropicStreamDecoder(usageFn func(json.RawMessage) *core.Usage) *anthropicStreamDecoder {
	return &anthropicStreamDecoder{usageFn: usageFn, toolIDs: map[int]string{}}
}

func (d *anthropicStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := ev.Data
	if data == "" {
		return nil, false, nil
	}
	var e anthropicStreamEvent
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return nil, false, nil
	}
	etype := e.Type
	if etype == "" {
		etype = ev.Event
	}
	switch etype {
	case "message_start":
		frames := []stream.Frame{{Kind: stream.FrameLifecycle, Status: "message_start"}}
		if e.Message != nil && len(e.Message.Usage) > 0 {
			d.startUsage = d.usageFn(e.Message.Usage)
			frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.startUsage})
		}
		return frames, false, nil
	case "content_block_start":
		if e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
			id := e.ContentBlock.ID
			if id == "" {
				id = e.ContentBlock.Name
			}
			d.toolIDs[e.Index] = id
			frames := []stream.Frame{{Kind: stream.FrameLifecycle, Status: "tool_use", ToolCallID: id}}
			// 补名帧：函数名只在 content_block_start 的 tool_use 块出现（4.3.2），
			// 后续 input_json_delta 不再带名 ⇒ 名字必须在此进帧。⚠️ 不能搭在
			// FrameLifecycle 上——帧承载约定里只有 FrameToolCallArgsDelta 的 Text
			// 会被消费端读作函数名，生命周期帧会被帧累加器忽略 ⇒ 用参数帧补名
			//（Arguments 为空，累加无副作用；name 为空不发帧，保持原行为）。
			if e.ContentBlock.Name != "" {
				frames = append(frames, stream.Frame{Kind: stream.FrameToolCallArgsDelta, ToolCallID: id, Text: e.ContentBlock.Name})
			}
			return frames, false, nil
		}
		return nil, false, nil
	case "content_block_delta":
		if e.Delta == nil {
			return nil, false, nil
		}
		switch e.Delta.Type {
		case "text_delta":
			return []stream.Frame{{Kind: stream.FrameTextDelta, Text: e.Delta.Text}}, false, nil
		case "thinking_delta":
			return []stream.Frame{{Kind: stream.FrameReasoningDelta, Reasoning: e.Delta.Thinking}}, false, nil
		case "input_json_delta":
			return []stream.Frame{{Kind: stream.FrameToolCallArgsDelta, ToolCallID: d.toolIDs[e.Index], Arguments: e.Delta.PartialJSON}}, false, nil
		case "signature_delta":
			// thinking 块签名（delta.signature，⛔ 不可压平，4.2.5）⇒ 签名帧
			//（承载约定见 stream.FrameReasoningDelta：复用 FrameReasoningDelta，
			// ThinkingSignature 非空、Reasoning 为空，一帧收口一个 thinking 块）。
			// 消费端：帧累加器落 core.OutputItem.ThinkingSignature；Responses 出口落
			// item.signature（无损）；Chat 出口承载不了 ⇒ x_gateway.degradations[] 标记。
			return []stream.Frame{{Kind: stream.FrameReasoningDelta, ThinkingSignature: e.Delta.Signature}}, false, nil
		default:
			return nil, false, nil
		}
	case "content_block_stop", "ping":
		return nil, false, nil
	case "message_delta":
		if e.Delta != nil && e.Delta.StopReason != "" {
			d.stopReason = e.Delta.StopReason
		}
		if len(e.Usage) == 0 {
			return nil, false, nil
		}
		// ⚠️ message_delta 的 usage 是累计值（4.3.2）⇒ 取累计快照，⛔ 不逐帧相加。
		u := d.usageFn(e.Usage)
		if u.InputTokens == 0 && d.startUsage != nil {
			// 累计快照缺输入侧时合并 message_start 的初始值（输入侧不随输出增长）。
			u.InputTokens = d.startUsage.InputTokens
			u.InputTokensUncached = d.startUsage.InputTokensUncached
			u.CacheReadTokens = d.startUsage.CacheReadTokens
			u.CacheWriteTokens = d.startUsage.CacheWriteTokens
		}
		return []stream.Frame{{Kind: stream.FrameUsage, Usage: u}}, false, nil
	case "message_stop":
		status := d.stopReason
		if status == "" {
			status = core.FinishUnknown
		}
		return []stream.Frame{{Kind: stream.FrameDone, Status: status}}, true, nil
	default:
		// 未知事件（含扩展事件）⇒ 忽略（3.7 流式容错）。
		return nil, false, nil
	}
}

// ---- SDK 流式（Tier 1：client.Messages.NewStreaming 流式执行、SDK 流事件 ⇒ stream.Frame）----

// anthropicStreamWire 是 SDK 流式执行的线上状态挂钩：SDK 的 URL 相对路径解析
// （base.Parse("v1/messages")）与适配器契约 {base}/messages 不一致（base 不以 /v1 结尾时
// 多出 /v1 段）⇒ 中间件把请求 URL 改写回契约 URL；同时捕获非 2xx 响应（状态/头/体），
// 供错误归一精确还原 streamRequest 的 FromUpstream 口径（7.1：错误对象含
// UpstreamStatus / UpstreamRequestID / UpstreamBody）。
type anthropicStreamWire struct {
	target *url.URL
	resp   *http.Response
	body   []byte
}

// middleware 是 SDK 请求管道中间件（option.Middleware）：改写契约 URL + 捕获非 2xx 响应
// + 给流式响应体加 SSE 尾事件容错垫片（sseTailFlush）。
// 读走响应体后原样放回（io.NopCloser），SDK 的错误解析（apierror）不受影响。
func (w *anthropicStreamWire) middleware() option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		req.URL = w.target
		resp, err := next(req)
		if resp != nil {
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(b))
				w.resp, w.body = resp, b
			} else {
				resp.Body = &sseTailFlush{rc: resp.Body}
			}
		}
		return resp, err
	}
}

// sseTailFlush 是 SSE 尾事件容错垫片（SDK 使用坑，实测钉住）：
// 现象：testdata/real/anthropic_stream_real.sse（真捕获）以 message_stop 帧收尾、
// ⛔ 无终止空行（末字节 `}\n`），SDK 的 ssestream.eventStreamDecoder 只在空行派发事件，
// 流末尾未分隔的最后一个事件在 EOF 被静默丢弃 ⇒ 帧序列缺 message_stop、done 状态为空
// （实测输出：`done 状态 = ""，期望 end_turn`）；
// 根因：SDK 解析器无「残留事件补发」逻辑，而既有 stream.SSEScanner 的口径是
// 「后端不发终止空行 ⇒ 流结束补发一次」（internal/stream/sse.go，3.7 流式容错）；
// 为什么这个修法：在响应体读到 EOF 时注入一次 "\n\n"（SSE 合法空行，对已正常分隔的流
// 是无操作——SDK 对空事件 `event=="" && data 空` 直接 continue），恢复既有容错口径，
// ⛔ 不改上游线上字节（只动 SDK 读到的流），比重写 SDK 解析器或降级整条链路都小；
// 不修会怎样：凡上游以未分隔尾帧结束的流（真捕获即此形态）都会静默丢最后一个事件，
// message_stop 丢失 ⇒ 终止帧丢失 ⇒ 下游拿不到 done。
type sseTailFlush struct {
	rc      io.ReadCloser
	tail    []byte
	flushed bool
}

// Read 实现 io.Reader：底层 EOF 时一次性注入 "\n\n"，随后照常 EOF。
func (f *sseTailFlush) Read(p []byte) (int, error) {
	if len(f.tail) > 0 {
		n := copy(p, f.tail)
		f.tail = f.tail[n:]
		return n, nil
	}
	n, err := f.rc.Read(p)
	if err == io.EOF && !f.flushed {
		f.flushed = true
		f.tail = []byte("\n\n")
		if n > 0 {
			return n, nil
		}
		m := copy(p, f.tail)
		f.tail = f.tail[m:]
		return m, nil
	}
	return n, err
}

// Close 关闭底层响应体（ssestream 收口路径调用）。
func (f *sseTailFlush) Close() error { return f.rc.Close() }

// detail 把 SDK 错误归一为 DetailError（与 streamRequest 的错误出口同口径）：
// 非 2xx ⇒ FromUpstream（保留状态码 / 请求 ID / 响应体）；传输失败 ⇒ FromTransportError。
func (w *anthropicStreamWire) detail(err error) error {
	if w.resp != nil {
		return &DetailError{Detail: gwerrors.FromUpstream(core.ProviderAnthropic,
			w.resp.StatusCode, w.body, w.resp.Header.Get("X-Request-Id"))}
	}
	return &DetailError{Detail: gwerrors.FromTransportError(core.ProviderAnthropic, err, "")}
}

// anthropicSDKIter 是 SDK 流事件 ⇒ stream.Frame 的帧迭代器：帧语义与
// anthropicStreamDecoder.decode 逐条一致（补名帧 / 签名帧 / message_delta 累计 usage 合并 /
// stop_reason 超集 / 未知事件忽略），usage 取原始 JSON 字节（⛔ 不用 typed 重序列化，
// usage.Normalize.Raw 逐字保留）。ping 事件由 SDK 流解析层过滤（与解码器忽略等效）。
type anthropicSDKIter struct {
	ctx        context.Context
	stream     *ssestream.Stream[anthropic.MessageStreamEventUnion]
	usageFn    func(json.RawMessage) *core.Usage
	wire       *anthropicStreamWire
	toolIDs    map[int64]string // content block index → tool_use id
	startUsage *core.Usage      // message_start 的初始 usage（message_delta 缺输入侧时合并）
	stopReason string
	queue      []stream.Frame
	cur        stream.Frame
	err        error
	stop       bool // 终态事件已解出（队列发完即收口）
	closed     bool
	seqAuto    int64 // 无 sequence_number 事件的自增序号（与 sseIter 同口径）
}

// Next 推进到下一帧；流结束或出错返回 false（此时关闭上游流）。语义与 sseIter.Next 一致。
func (it *anthropicSDKIter) Next() bool {
	for len(it.queue) == 0 {
		if it.stop {
			it.close()
			return false
		}
		if err := it.ctx.Err(); err != nil {
			// ctx 取消 ⇒ 立即中断并关闭上游连接（3.7 取消语义；cancel_grace_period 默认 0）。
			it.err = err
			it.close()
			return false
		}
		if !it.stream.Next() {
			if e := it.stream.Err(); e != nil {
				if err := it.ctx.Err(); err != nil {
					it.err = err
				} else {
					it.err = it.wire.detail(e)
				}
			}
			it.close()
			return false
		}
		frames, finished := it.convert(it.stream.Current())
		for i := range frames {
			it.seqAuto++
			if frames[i].Seq == 0 {
				frames[i].Seq = it.seqAuto
			}
		}
		it.queue = frames
		it.stop = finished
	}
	it.cur = it.queue[0]
	it.queue = it.queue[1:]
	return true
}

// Frame 返回当前帧。
func (it *anthropicSDKIter) Frame() stream.Frame { return it.cur }

// Err 返回终态错误（nil 表示正常结束）。
func (it *anthropicSDKIter) Err() error { return it.err }

// close 关闭上游流（停止时关流，3.6）；幂等。
func (it *anthropicSDKIter) close() {
	if !it.closed {
		it.closed = true
		_ = it.stream.Close()
	}
}

// convert 把 SDK 流事件转为内部帧（与 anthropicStreamDecoder.decode 同语义）。
func (it *anthropicSDKIter) convert(ev anthropic.MessageStreamEventUnion) ([]stream.Frame, bool) {
	switch ev.Type {
	case "message_start":
		frames := []stream.Frame{{Kind: stream.FrameLifecycle, Status: "message_start"}}
		// usage 取原始 JSON 字节（message_start 的 usage 在 message 对象内）。
		if raw := rawJSONTopField(ev.JSON.Message.Raw(), "usage"); len(raw) > 0 {
			it.startUsage = it.usageFn(raw)
			frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: it.startUsage})
		}
		return frames, false
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			id := ev.ContentBlock.ID
			if id == "" {
				id = ev.ContentBlock.Name
			}
			it.toolIDs[ev.Index] = id
			frames := []stream.Frame{{Kind: stream.FrameLifecycle, Status: "tool_use", ToolCallID: id}}
			// 补名帧（帧承载约定同 decode：函数名只在 content_block_start 出现，
			// ⛔ 不能搭在 FrameLifecycle 上）。
			if ev.ContentBlock.Name != "" {
				frames = append(frames, stream.Frame{Kind: stream.FrameToolCallArgsDelta, ToolCallID: id, Text: ev.ContentBlock.Name})
			}
			return frames, false
		}
		return nil, false
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			return []stream.Frame{{Kind: stream.FrameTextDelta, Text: ev.Delta.Text}}, false
		case "thinking_delta":
			return []stream.Frame{{Kind: stream.FrameReasoningDelta, Reasoning: ev.Delta.Thinking}}, false
		case "input_json_delta":
			return []stream.Frame{{Kind: stream.FrameToolCallArgsDelta, ToolCallID: it.toolIDs[ev.Index], Arguments: ev.Delta.PartialJSON}}, false
		case "signature_delta":
			// thinking 块签名 ⇒ 签名帧（FrameReasoningDelta 且 ThinkingSignature 非空、
			// Reasoning 为空，⛔ 不可压平，4.2.5）。
			return []stream.Frame{{Kind: stream.FrameReasoningDelta, ThinkingSignature: ev.Delta.Signature}}, false
		default:
			return nil, false
		}
	case "content_block_stop", "ping":
		return nil, false
	case "message_delta":
		if ev.Delta.StopReason != "" {
			it.stopReason = string(ev.Delta.StopReason)
		}
		// usage 取原始 JSON 字节；⚠️ message_delta 的 usage 是累计值（4.3.2）⇒
		// 取累计快照，⛔ 不逐帧相加。
		raw := json.RawMessage(ev.JSON.Usage.Raw())
		if len(raw) == 0 {
			return nil, false
		}
		u := it.usageFn(raw)
		if u.InputTokens == 0 && it.startUsage != nil {
			// 累计快照缺输入侧时合并 message_start 的初始值（输入侧不随输出增长）。
			u.InputTokens = it.startUsage.InputTokens
			u.InputTokensUncached = it.startUsage.InputTokensUncached
			u.CacheReadTokens = it.startUsage.CacheReadTokens
			u.CacheWriteTokens = it.startUsage.CacheWriteTokens
		}
		return []stream.Frame{{Kind: stream.FrameUsage, Usage: u}}, false
	case "message_stop":
		status := it.stopReason
		if status == "" {
			status = core.FinishUnknown
		}
		return []stream.Frame{{Kind: stream.FrameDone, Status: status}}, true
	default:
		// 未知事件（含扩展事件）⇒ 忽略（3.7 流式容错）。
		return nil, false
	}
}

// rawJSONTopField 取 JSON 对象顶层字段的原始字节（usage 原文逐字进 Normalize.Raw 用）。
func rawJSONTopField(obj, key string) json.RawMessage {
	if obj == "" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(obj), &m); err != nil {
		return nil
	}
	return m[key]
}

// StreamStart：SDK 客户端流式执行（Tier 1：自定义 base URL + 注入现有 *http.Client）⇒
// SDK 流事件 ⇒ stream.Frame（⛔ 无 [DONE]；错误出口与 streamRequest 同口径）。
func (a *anthropicAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, err
	}
	rc := *r
	rc.Stream = true
	params, _, _, err := anthropicSDKParams(&rc, res)
	if err != nil {
		return nil, err
	}
	// 契约 URL（3.6：POST {base}/messages）——SDK 的固定相对路径 v1/messages 经
	// base.Parse 解析后与契约不一致（base 不以 /v1 结尾时多出 /v1 段）⇒ 中间件改写。
	target, err := url.Parse(joinURL(res.BaseURL, "/messages"))
	if err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_build_failed", err.Error())}
	}
	wire := &anthropicStreamWire{target: target}
	client := anthropic.NewClient(
		// ⛔ 不读环境凭据（21 凭据隔离）：上游凭据一律由安全层 authTransport 注入。
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(res.BaseURL),
		option.WithHTTPClient(a.hc),
		option.WithHeader("anthropic-version", "2026-10-21"),
		option.WithMaxRetries(0), // ⛔ 适配器层不重试（7.4：重试与降级由网关层分别计数）
		option.WithMiddleware(wire.middleware()),
	)
	st := client.Messages.NewStreaming(ctx, params)
	if err := st.Err(); err != nil {
		// 请求期失败（非 2xx / 传输失败）保持旧口径：StreamStart 直接返回错误。
		st.Close()
		return nil, wire.detail(err)
	}
	return &anthropicSDKIter{
		ctx: ctx, stream: st, usageFn: a.ParseUsage, wire: wire, toolIDs: map[int64]string{},
	}, nil
}
