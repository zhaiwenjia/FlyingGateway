// 日期: 2026-11-15 00:04:25.586
// 作者: Zhai Wenjia

// Package messages 是 Anthropic Messages 入口的编解码（统一 SDD 3.1/3.4 的第三入站形）：
//   - DecodeRequest：Messages 请求体 ⇒ core.Request（system / content blocks / tools 反向翻译）；
//   - EncodeResponse：core.Response ⇒ Messages 响应体（content blocks / stop_reason / usage）；
//   - EventEncoder：内部帧 ⇒ Anthropic 事件流（message_start / content_block_* / message_delta /
//     message_stop），实现 server.clientEncoder 契约。
//
// 与 adapter/anthropic.go 是**镜像方向**：适配器把 core 翻成 Messages 打上游，本包把
// Messages 翻成 core 进管线 —— 线上字段名两边逐字一致（⛔ 不各写一份口径）。
package messages

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// ---- 入站 wire 结构（Messages 请求体）----

type requestWire struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system"` // string 或 text 块数组
	Messages      []messageWire   `json:"messages"`
	Tools         []toolWire      `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"` // {type, name?}
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Thinking      *thinkingWire   `json:"thinking,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"` // 用户侧计费标识，网关不消费
}

type thinkingWire struct {
	Type         string `json:"type"` // enabled / disabled / adaptive
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type messageWire struct {
	Role    string          `json:"role"`    // user / assistant
	Content json.RawMessage `json:"content"` // string 或块数组
}

type blockWire struct {
	Type         string             `json:"type"`
	Text         string             `json:"text,omitempty"`
	Thinking     string             `json:"thinking,omitempty"`
	Signature    string             `json:"signature,omitempty"`
	Data         string             `json:"data,omitempty"` // redacted_thinking
	ID           string             `json:"id,omitempty"`   // tool_use
	Name         string             `json:"name,omitempty"`
	Input        json.RawMessage    `json:"input,omitempty"`
	ToolUseID    string             `json:"tool_use_id,omitempty"` // tool_result
	Content      json.RawMessage    `json:"content,omitempty"`     // tool_result 内容
	Source       *imageSourceWire   `json:"source,omitempty"`      // image
	CacheControl *core.CacheControl `json:"cache_control,omitempty"`
}

type imageSourceWire struct {
	Type      string `json:"type"` // base64 / url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type toolWire struct {
	Name         string             `json:"name"`
	Description  string             `json:"description,omitempty"`
	InputSchema  json.RawMessage    `json:"input_schema"`
	CacheControl *core.CacheControl `json:"cache_control,omitempty"`
}

type toolChoiceWire struct {
	Type string `json:"type"` // auto / any / tool / none
	Name string `json:"name,omitempty"`
}

// DecodeRequest 把 Messages 请求体翻译为 core.Request（统一 SDD 3.1）。
// 语义丢失处的口径：多个 cache_control 断点 ⇒ 显式 400（当前 core 只承载单断点，
// ⛔ 不静默取一）；metadata 不消费；top_k 进 Extra 透传。
func DecodeRequest(body []byte) (*core.Request, error) {
	var w requestWire
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("messages: 请求体解析失败: %w", err)
	}
	if w.Model == "" {
		return nil, errors.New("messages: model 必填")
	}
	if w.MaxTokens <= 0 {
		return nil, errors.New("messages: max_tokens 必填且须为正")
	}
	if len(w.Messages) == 0 {
		return nil, errors.New("messages: messages 不能为空")
	}

	req := &core.Request{
		Model:           w.Model,
		MaxOutputTokens: w.MaxTokens,
		Temperature:     w.Temperature,
		TopP:            w.TopP,
		Stop:            w.StopSequences,
		Stream:          w.Stream,
	}
	if w.TopK != nil {
		req.Extra = map[string]any{"top_k": *w.TopK}
	}
	// thinking ⇒ ReasoningConfig（出站 anthropic 据此开 adaptive thinking）。
	if w.Thinking != nil && w.Thinking.Type != "" && w.Thinking.Type != "disabled" {
		req.Reasoning = &core.ReasoningConfig{Effort: "high"}
	}

	// system（string | text 块数组）⇒ 首条 system 消息。
	if len(w.System) > 0 {
		parts, cc, err := decodeSystem(w.System)
		if err != nil {
			return nil, err
		}
		if len(parts) > 0 {
			cc2, err := takeCacheControl(req.CacheControl, cc)
			if err != nil {
				return nil, err
			}
			req.CacheControl = cc2
			req.Messages = append(req.Messages, core.Message{Role: "system", Content: parts})
		}
	}

	for i, m := range w.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, fmt.Errorf("messages: messages[%d].role = %q 只允许 user / assistant", i, m.Role)
		}
		blocks, err := decodeContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages: messages[%d]: %w", i, err)
		}
		// 按块序切段：连续非 tool_result 归原消息；每个 tool_result 独立成 tool 消息
		//（Chat 家族的工具回填形态），顺序保持。
		var seg []blockWire
		flush := func() error {
			if len(seg) == 0 {
				return nil
			}
			msg, cc, err := messageOf(m.Role, seg)
			if err != nil {
				return err
			}
			cc2, err := takeCacheControl(req.CacheControl, cc)
			if err != nil {
				return err
			}
			req.CacheControl = cc2
			req.Messages = append(req.Messages, msg)
			seg = seg[:0]
			return nil
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				if err := flush(); err != nil {
					return nil, fmt.Errorf("messages: messages[%d]: %w", i, err)
				}
				tm, cc, err := toolResultOf(b)
				if err != nil {
					return nil, fmt.Errorf("messages: messages[%d]: %w", i, err)
				}
				cc2, err := takeCacheControl(req.CacheControl, cc)
				if err != nil {
					return nil, err
				}
				req.CacheControl = cc2
				req.Messages = append(req.Messages, tm)
				continue
			}
			seg = append(seg, b)
		}
		if err := flush(); err != nil {
			return nil, fmt.Errorf("messages: messages[%d]: %w", i, err)
		}
	}

	// tools：input_schema ⇒ Parameters（JSON Schema 原样）。
	for i, t := range w.Tools {
		if t.Name == "" || len(t.InputSchema) == 0 {
			return nil, fmt.Errorf("messages: tools[%d] 须含 name 与 input_schema", i)
		}
		req.Tools = append(req.Tools, core.Tool{
			Type: "function", Name: t.Name, Description: t.Description,
			Parameters: json.RawMessage(t.InputSchema),
		})
		cc2, err := takeCacheControl(req.CacheControl, t.CacheControl)
		if err != nil {
			return nil, err
		}
		req.CacheControl = cc2
	}

	// tool_choice：auto / any / tool / none ⇒ 规范四态。
	if len(w.ToolChoice) > 0 {
		var tc toolChoiceWire
		if err := json.Unmarshal(w.ToolChoice, &tc); err != nil {
			return nil, fmt.Errorf("messages: tool_choice 解析失败: %w", err)
		}
		switch tc.Type {
		case "auto":
			req.ToolChoice = &core.ToolChoice{Mode: "auto"}
		case "any":
			req.ToolChoice = &core.ToolChoice{Mode: "required"}
		case "tool":
			if tc.Name == "" {
				return nil, errors.New("messages: tool_choice.type=tool 须带 name")
			}
			req.ToolChoice = &core.ToolChoice{Mode: "function", Name: tc.Name}
		case "none":
			req.ToolChoice = &core.ToolChoice{Mode: "none"}
		default:
			return nil, fmt.Errorf("messages: tool_choice.type = %q 不支持（auto / any / tool / none）", tc.Type)
		}
	}
	return req, nil
}

// decodeSystem 解析 system（string 或 text 块数组）。
func decodeSystem(raw json.RawMessage) ([]core.ContentPart, *core.CacheControl, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil, nil
		}
		return []core.ContentPart{{Type: "text", Text: s}}, nil, nil
	}
	blocks, err := decodeContent(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("system: %w", err)
	}
	var parts []core.ContentPart
	var cc *core.CacheControl
	for _, b := range blocks {
		if b.Type != "text" {
			return nil, nil, fmt.Errorf("system 只允许 text 块，收到 %q", b.Type)
		}
		parts = append(parts, core.ContentPart{Type: "text", Text: b.Text})
		cc2, err := takeCacheControl(cc, b.CacheControl)
		if err != nil {
			return nil, nil, err
		}
		cc = cc2
	}
	return parts, cc, nil
}

// decodeContent 解析消息内容（string 或块数组）。
func decodeContent(raw json.RawMessage) ([]blockWire, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []blockWire{{Type: "text", Text: s}}, nil
	}
	var blocks []blockWire
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content 解析失败: %w", err)
	}
	return blocks, nil
}

// messageOf 把非 tool_result 块段翻译成一条规范消息。
func messageOf(role string, blocks []blockWire) (core.Message, *core.CacheControl, error) {
	msg := core.Message{Role: role}
	var cc *core.CacheControl
	for _, b := range blocks {
		cc2, err := takeCacheControl(cc, b.CacheControl)
		if err != nil {
			return msg, nil, err
		}
		cc = cc2
		switch b.Type {
		case "text":
			msg.Content = append(msg.Content, core.ContentPart{Type: "text", Text: b.Text})
		case "image":
			part := core.ContentPart{Type: "image"}
			if b.Source != nil {
				part.MIMEType = b.Source.MediaType
				switch b.Source.Type {
				case "base64":
					part.Data = b.Source.Data
				case "url":
					part.ImageURL = b.Source.URL
				default:
					return msg, nil, fmt.Errorf("image.source.type = %q 不支持（base64 / url）", b.Source.Type)
				}
			}
			msg.Content = append(msg.Content, part)
		case "thinking":
			msg.ReasoningContent = joinNonEmpty(msg.ReasoningContent, b.Thinking)
			if b.Signature != "" {
				msg.ThinkingSignature = b.Signature
			}
		case "redacted_thinking":
			msg.EncryptedContent = b.Data
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			msg.ToolCalls = append(msg.ToolCalls, core.ToolCall{
				ID: b.ID, CallID: b.ID, Name: b.Name, Arguments: args, Type: "function",
			})
		default:
			return msg, nil, fmt.Errorf("内容块类型 %q 不支持", b.Type)
		}
	}
	return msg, cc, nil
}

// toolResultOf 把 tool_result 块翻译成 tool 消息（Chat 家族回填形态）。
func toolResultOf(b blockWire) (core.Message, *core.CacheControl, error) {
	parts, _, err := decodeResultContent(b.Content)
	if err != nil {
		return core.Message{}, nil, err
	}
	return core.Message{Role: "tool", ToolCallID: b.ToolUseID, Content: parts}, b.CacheControl, nil
}

// decodeResultContent 解析 tool_result 内容（string | 文本/图片块数组）。
func decodeResultContent(raw json.RawMessage) ([]core.ContentPart, *core.CacheControl, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []core.ContentPart{{Type: "text", Text: s}}, nil, nil
	}
	blocks, err := decodeContent(raw)
	if err != nil {
		return nil, nil, err
	}
	var parts []core.ContentPart
	var cc *core.CacheControl
	for _, b := range blocks {
		cc2, err := takeCacheControl(cc, b.CacheControl)
		if err != nil {
			return nil, nil, err
		}
		cc = cc2
		switch b.Type {
		case "text":
			parts = append(parts, core.ContentPart{Type: "text", Text: b.Text})
		case "image":
			part := core.ContentPart{Type: "image"}
			if b.Source != nil {
				part.MIMEType = b.Source.MediaType
				part.Data = b.Source.Data
				part.ImageURL = b.Source.URL
			}
			parts = append(parts, part)
		default:
			return nil, nil, fmt.Errorf("tool_result 内容块类型 %q 不支持", b.Type)
		}
	}
	return parts, cc, nil
}

// takeCacheControl 归并 cache_control 断点：当前 core 只承载单断点 ⇒ 多个**不同**断点
// 显式报错（⛔ 不静默取一 —— 静默取一会让缓存前缀边界悄悄变样）；重复相同断点容忍。
func takeCacheControl(cur, next *core.CacheControl) (*core.CacheControl, error) {
	if next == nil {
		return cur, nil
	}
	if cur == nil {
		return next, nil
	}
	if cur.Type == next.Type && cur.TTL == next.TTL {
		return cur, nil
	}
	return cur, errors.New("messages: 当前仅支持单个 cache_control 断点（多断点的缓存前缀边界无法经 core 表达，⛔ 不静默取一）")
}

func joinNonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n" + b
}
