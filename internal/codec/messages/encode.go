// 日期: 2026-11-15 00:04:25.586
// 作者: Zhai Wenjia

package messages

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// ---- 出站编码 wire 结构（Messages 响应体）----

// messageOut 是 Messages 响应体（type="message"）。
type messageOut struct {
	ID           string       `json:"id"`
	Type         string       `json:"type"` // "message"
	Role         string       `json:"role"` // "assistant"
	Model        string       `json:"model"`
	Content      []blockOut   `json:"content"`
	StopReason   string       `json:"stop_reason"`
	StopSequence *string      `json:"stop_sequence"`
	Usage        usageOut     `json:"usage"`
	XGateway     *xGatewayOut `json:"x_gateway,omitempty"`
}

// blockOut 是 content[] 输出块（text / thinking / redacted_thinking / tool_use）。
type blockOut struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// usageOut 是 Messages usage（⚠️ input_tokens 为**未缓存**输入；缓存两相分列
// ——与 core.Usage.InputTokens 的「三项之和」口径互逆，见 usageOf）。
type usageOut struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type xGatewayOut struct {
	Degradations []degradationOut `json:"degradations"`
}

type degradationOut struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// EncodeResponse 把 core.Response 编码为 Messages 响应体（统一 SDD 3.4）。
// 块映射：text ⇒ text；reasoning/thinking ⇒ thinking（签名原样承载，⛔ 不压平）；
// EncryptedContent ⇒ redacted_thinking（不透明密文的原生承载位）；工具调用 ⇒ tool_use。
func EncodeResponse(resp *core.Response) ([]byte, error) {
	if resp == nil {
		return nil, errors.New("messages: EncodeResponse 收到 nil Response")
	}
	out := messageOut{
		ID:       resp.ID,
		Type:     "message",
		Role:     "assistant",
		Model:    resp.Model,
		Usage:    usageOf(resp.Usage),
		XGateway: xgOf(resp.Degradations),
	}
	if out.ID == "" {
		out.ID = nextMessageID()
	}
	stop := stopReasonOf(resp.FinishReason)
	out.StopReason = stop

	var degradations []core.Degradation
	for _, item := range resp.OutputItems {
		switch item.Type {
		case "message":
			for _, p := range item.Content {
				switch p.Type {
				case "text", "output_text":
					// output_text 为历史/异路径部件名的兼容认列（2026-11-17 修复后
					// core 树生产者统一 "text"；兼容认列防旧数据/跨出口转换再丢正文）。
					if p.Text != "" {
						out.Content = append(out.Content, blockOut{Type: "text", Text: p.Text})
					}
				default:
					degradations = append(degradations, core.Degradation{
						Field:  p.Type,
						Reason: core.DegradationResponseToChat,
						Detail: "Messages 响应 content 只承载 text / thinking / tool_use，该内容部件已丢弃",
					})
				}
			}
			if item.Refusal != "" {
				out.Content = append(out.Content, blockOut{Type: "text", Text: item.Refusal})
			}
		case "reasoning", "thinking":
			if item.EncryptedContent != "" {
				out.Content = append(out.Content, blockOut{Type: "redacted_thinking", Data: item.EncryptedContent})
			}
			if item.ReasoningText != "" || item.ThinkingSignature != "" {
				out.Content = append(out.Content, blockOut{
					Type: "thinking", Thinking: item.ReasoningText, Signature: item.ThinkingSignature,
				})
			}
		case "function_call", "tool_use":
			input := json.RawMessage("{}")
			if item.Arguments != "" {
				if json.Valid([]byte(item.Arguments)) {
					input = json.RawMessage(item.Arguments)
				} else {
					degradations = append(degradations, core.Degradation{
						Field:  "tool_arguments",
						Reason: core.DegradationUpstreamShapeMismatch,
						Detail: "工具入参非合法 JSON，tool_use.input 置 {}",
					})
				}
			}
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			out.Content = append(out.Content, blockOut{Type: "tool_use", ID: id, Name: item.Name, Input: input})
		default:
			degradations = append(degradations, core.Degradation{
				Field:  item.Type,
				Reason: core.DegradationResponseToChat,
				Detail: "该输出项类型无 Messages 对应块，已丢弃",
			})
		}
	}
	// 丢标记并入 x_gateway（无丢失则不输出该扩展字段）。
	if out.XGateway == nil && len(degradations) > 0 {
		out.XGateway = xgOf(degradations)
	} else if out.XGateway != nil {
		out.XGateway.Degradations = append(out.XGateway.Degradations, toOut(degradations)...)
	}
	if out.Content == nil {
		out.Content = []blockOut{}
	}
	return json.Marshal(out)
}

// usageOf 把归一 usage 拆回 Messages 三项口径：
// input_tokens = 总输入 − 缓存写入 − 缓存命中（⛔ 不整包塞总输入 —— 那会三倍计输入）。
func usageOf(u *core.Usage) usageOut {
	if u == nil {
		return usageOut{}
	}
	uncached := u.InputTokens - u.CacheWriteTokens - u.CacheReadTokens
	if uncached < 0 {
		uncached = 0
	}
	return usageOut{
		InputTokens:              uncached,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
	}
}

// stopReasonOf 把超集 finish 映射到 Messages stop_reason：
// Anthropic 原值（end_turn / max_tokens / tool_use / stop_sequence / refusal）原样透传，
// Chat 家族值翻译（stop→end_turn、length→max_tokens、tool_calls/function_call→tool_use、
// content_filter→refusal），未知值兜底 end_turn。
func stopReasonOf(finish string) string {
	switch finish {
	case "end_turn", "max_tokens", "tool_use", "stop_sequence", "refusal",
		"model_context_window_exceeded", "pause_turn":
		return finish
	case "stop", "":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func xgOf(ds []core.Degradation) *xGatewayOut {
	if len(ds) == 0 {
		return nil
	}
	return &xGatewayOut{Degradations: toOut(ds)}
}

func toOut(ds []core.Degradation) []degradationOut {
	out := make([]degradationOut, 0, len(ds))
	for _, d := range ds {
		out = append(out, degradationOut{Field: d.Field, Reason: d.Reason, Detail: d.Detail})
	}
	return out
}

// nextMessageID 生成响应 id（msg_ 前缀 + 单调计数与时间戳混合，避免并发撞车）。
func nextMessageID() string {
	return fmt.Sprintf("msg_%d_%d", timeNowUnixNano(), idCounter.Add(1))
}
