// 日期: 2026-11-14 19:39:55.257
// 作者: Zhai Wenjia

// Package chat 实现 OpenAI Chat Completions 形状的入站双模式编解码（统一 SDD 3.4）。
//
//   - DecodeRequest：chat.completion 请求体 ⇒ core.Request。messages[0].role=system
//     保留 system 角色（⛔ 不提升为 Instructions，转换在出站时做）；读侧宽容，未知字段忽略。
//   - EncodeResponse：core.Response ⇒ chat.completion 响应体。Chat 出口承载不了的语义
//     （thinking_signature / encrypted_content）⇒ 响应体顶层 x_gateway.degradations[]
//     显式标记（⛔ 不静默丢，统一 SDD 3.4 字段设计：field / reason / detail）。
//   - ChunkEncoder：内部帧 ⇒ chat.completion.chunk SSE data 行（客户端侧帧编码器）；
//     终止帧带 finish_reason 非 null 与 x_gateway.degradations[]，随后 usage 块与 data: [DONE]。
//   - ParseChunk：上游 chat.completion.chunk ⇒ 内部帧；usage 统一走 usage.Normalize
//     （单一口径真源，provider 参数即为其入参）。
//
// 字段名 / 枚举与统一 SDD 逐字一致；读侧宽容（未知字段忽略），写侧不省略语义字段。
package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// ---- 入站解码 wire 结构（读侧宽容：未声明的字段一律忽略）----

// chatRequest 是 chat.completion 请求体（统一 SDD 4.3.1 Chat 请求体关键字段）。
type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessageIn `json:"messages"`
	Stream              bool            `json:"stream"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"` // string 或 []string
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Tools               []chatToolIn    `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"` // string 或对象
	ReasoningEffort     string          `json:"reasoning_effort"`
}

// chatMessageIn 是 messages[] 元素；reasoning_content 是各家扩展（Qwen/Kimi/MiMo/
// MiniMax/智谱/DeepSeek），字段名逐字保留。
type chatMessageIn struct {
	Role             string           `json:"role"`
	Content          json.RawMessage  `json:"content"` // string 或 part 数组
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []chatToolCallIn `json:"tool_calls"`
	ToolCallID       string           `json:"tool_call_id"`
	Name             string           `json:"name"`
}

// chatToolCallIn 是 message.tool_calls[] 元素（{id,type,function{name,arguments}}）。
type chatToolCallIn struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function *chatFunctionIn `json:"function"`
}

// chatFunctionIn 是工具调用 / 工具声明的 function 对象。
type chatFunctionIn struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Arguments   string          `json:"arguments"`
	Parameters  json.RawMessage `json:"parameters"`
}

// chatToolIn 是 tools[] 元素（function 形状）。
type chatToolIn struct {
	Type     string          `json:"type"`
	Function *chatFunctionIn `json:"function"`
}

// chatContentPartIn 是 content part 数组元素（text / image_url / file / input_audio）。
type chatContentPartIn struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
	InputAudio *struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	} `json:"input_audio"`
	File *struct {
		Filename string `json:"filename"`
		FileData string `json:"file_data"`
		FileID   string `json:"file_id"`
	} `json:"file"`
}

// DecodeRequest 把 chat.completion 请求体解码为 core.Request（统一 SDD 3.4）。
// messages[0].role=system 保留 system 角色（⛔ 不提升为 Instructions）；
// 未知字段忽略（读侧宽容），非法 JSON 显式报错。
func DecodeRequest(body []byte) (*core.Request, error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("chat: 请求体不是合法 JSON: %w", err)
	}
	out := &core.Request{
		Model:       req.Model,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	// 结构预分配：messages / tool_calls / tools 按入参条数预留，消掉 append
	// 逐次扩容的重分配；仅条数 > 0 时预建（空入参保持 nil，序列化形态不变）。
	if len(req.Messages) > 0 {
		out.Messages = make([]core.Message, 0, len(req.Messages))
	}
	for _, m := range req.Messages {
		content, err := decodeContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("chat: message(role=%q) content 解析失败: %w", m.Role, err)
		}
		msg := core.Message{
			Role:             m.Role,
			Content:          content,
			ReasoningContent: m.ReasoningContent,
			ToolCallID:       m.ToolCallID,
			Name:             m.Name,
		}
		if len(m.ToolCalls) > 0 {
			msg.ToolCalls = make([]core.ToolCall, 0, len(m.ToolCalls))
		}
		for _, tc := range m.ToolCalls {
			ct := core.ToolCall{ID: tc.ID, Type: tc.Type}
			if tc.Function != nil {
				ct.Name = tc.Function.Name
				ct.Arguments = tc.Function.Arguments
			}
			msg.ToolCalls = append(msg.ToolCalls, ct)
		}
		out.Messages = append(out.Messages, msg)
	}
	stop, err := decodeStop(req.Stop)
	if err != nil {
		return nil, fmt.Errorf("chat: stop 解析失败: %w", err)
	}
	out.Stop = stop
	// max_completion_tokens 是 OpenAI 现名，max_tokens 是旧名；二者同映射 MaxOutputTokens。
	if req.MaxCompletionTokens != nil {
		out.MaxOutputTokens = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil {
		out.MaxOutputTokens = *req.MaxTokens
	}
	if len(req.Tools) > 0 {
		out.Tools = make([]core.Tool, 0, len(req.Tools))
	}
	for _, t := range req.Tools {
		tool := core.Tool{Type: t.Type}
		if t.Function != nil {
			tool.Name = t.Function.Name
			tool.Description = t.Function.Description
			tool.Parameters = t.Function.Parameters
		}
		out.Tools = append(out.Tools, tool)
	}
	tc, err := decodeToolChoice(req.ToolChoice)
	if err != nil {
		return nil, fmt.Errorf("chat: tool_choice 解析失败: %w", err)
	}
	out.ToolChoice = tc
	if req.ReasoningEffort != "" {
		out.Reasoning = &core.ReasoningConfig{Effort: req.ReasoningEffort}
	}
	return out, nil
}

// decodeContent 解析 message.content：字符串 ⇒ 单个 text part；数组 ⇒ 逐 part 映射。
// 裸字符串 part 类型记 "text"（无 wire 类型可保留时的统一口径）；带 wire 类型的 part 逐字保留。
func decodeContent(raw json.RawMessage) ([]core.ContentPart, error) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || bytes.Equal(trim, []byte("null")) {
		return nil, nil
	}
	if trim[0] == '"' {
		var s string
		if err := json.Unmarshal(trim, &s); err != nil {
			return nil, err
		}
		return []core.ContentPart{{Type: "text", Text: s}}, nil
	}
	var parts []chatContentPartIn
	if err := json.Unmarshal(trim, &parts); err != nil {
		return nil, err
	}
	out := make([]core.ContentPart, 0, len(parts))
	for _, p := range parts {
		cp := core.ContentPart{Type: p.Type, Text: p.Text}
		if p.ImageURL != nil {
			cp.ImageURL = p.ImageURL.URL
		}
		if p.InputAudio != nil {
			cp.Data = p.InputAudio.Data
			if p.InputAudio.Format != "" {
				cp.MIMEType = "audio/" + p.InputAudio.Format
			}
		}
		if p.File != nil {
			cp.Data = p.File.FileData
		}
		out = append(out, cp)
	}
	return out, nil
}

// decodeStop 解析 stop：字符串 ⇒ 单元素切片；数组 ⇒ 逐字保留。
func decodeStop(raw json.RawMessage) ([]string, error) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || bytes.Equal(trim, []byte("null")) {
		return nil, nil
	}
	if trim[0] == '"' {
		var s string
		if err := json.Unmarshal(trim, &s); err != nil {
			return nil, err
		}
		return []string{s}, nil
	}
	var arr []string
	if err := json.Unmarshal(trim, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

// decodeToolChoice 解析 tool_choice："auto"/"none"/"required" 或
// {"type":"function","function":{"name":"..."}}。
func decodeToolChoice(raw json.RawMessage) (*core.ToolChoice, error) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || bytes.Equal(trim, []byte("null")) {
		return nil, nil
	}
	if trim[0] == '"' {
		var s string
		if err := json.Unmarshal(trim, &s); err != nil {
			return nil, err
		}
		return &core.ToolChoice{Mode: s}, nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(trim, &obj); err != nil {
		return nil, err
	}
	tc := &core.ToolChoice{Mode: obj.Type}
	if obj.Function != nil {
		tc.Mode = "function"
		tc.Name = obj.Function.Name
	}
	return tc, nil
}

// ---- 出站编码 wire 结构 ----

// chatResponseOut 是 chat.completion 响应体。
type chatResponseOut struct {
	ID       string          `json:"id"`
	Object   string          `json:"object"`
	Created  int64           `json:"created"`
	Model    string          `json:"model"`
	Choices  []chatChoiceOut `json:"choices"`
	Usage    *chatUsageOut   `json:"usage,omitempty"`
	XGateway *xGatewayOut    `json:"x_gateway,omitempty"`
}

// chatChoiceOut 是 choices[] 元素。
type chatChoiceOut struct {
	Index        int            `json:"index"`
	Message      chatMessageOut `json:"message"`
	FinishReason *string        `json:"finish_reason"`
}

// chatMessageOut 是 choices[].message（content/role/tool_calls + reasoning_content 扩展）。
type chatMessageOut struct {
	Role             string            `json:"role"`
	Content          *string           `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCallOut `json:"tool_calls,omitempty"`
	Refusal          string            `json:"refusal,omitempty"`
}

// chatToolCallOut 是 message.tool_calls[] 元素。
type chatToolCallOut struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function chatFunctionOut `json:"function"`
}

// chatFunctionOut 是工具调用的 function 对象。
type chatFunctionOut struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatUsageOut 是 OpenAI Chat 形 usage（统一 SDD 4.3.1 usage 树：缓存字段在二级）。
type chatUsageOut struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	PromptTokensDetails     chatPromptDetailsOut     `json:"prompt_tokens_details"`
	CompletionTokensDetails chatCompletionDetailsOut `json:"completion_tokens_details"`
}

// chatPromptDetailsOut 是 usage.prompt_tokens_details。
type chatPromptDetailsOut struct {
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// chatCompletionDetailsOut 是 usage.completion_tokens_details。
type chatCompletionDetailsOut struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// degradationOut 是 x_gateway.degradations[] 元素（统一 SDD 3.4：field / reason / detail）。
type degradationOut struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// xGatewayOut 是响应体顶层 x_gateway 对象。
type xGatewayOut struct {
	Degradations []degradationOut `json:"degradations"`
}

// degradationDetailSignature / degradationDetailEncrypted 是统一 SDD 3.4 示例里的
// 两段中文说明（逐字照抄）。
const (
	degradationDetailSignature = "Claude thinking.signature 无 OpenAI 形状对应物，已丢弃；多轮带思考对话将降级"
	degradationDetailEncrypted = "Responses reasoning item 的 encrypted_content 无法映射到 Chat 消息"
)

// collectDegradations 汇总不可逆语义丢失（统一 SDD 3.4 / 验收 A8、A14）：
// resp.Degradations 透传，另对每个 OutputItem 扫描——
//   - ThinkingSignature ⇒ field=thinking_signature, reason=upstream_shape_mismatch；
//   - EncryptedContent  ⇒ field=encrypted_content, reason=response_to_chat_conversion。
//
// Chat 出口承载不了这两者 ⇒ 必须产生标记，⛔ 不静默丢。
// 同 (field, reason) 只记一次。
func collectDegradations(resp *core.Response) []degradationOut {
	// seen 懒建：无降级（常见路径）零分配——读 nil map 合法，首条降级才建表去重。
	var seen map[string]bool
	var out []degradationOut
	add := func(d degradationOut) {
		key := d.Field + "|" + d.Reason
		if seen[key] {
			return
		}
		if seen == nil {
			seen = make(map[string]bool)
		}
		seen[key] = true
		out = append(out, d)
	}
	for _, d := range resp.Degradations {
		add(degradationOut{Field: d.Field, Reason: d.Reason, Detail: d.Detail})
	}
	for _, item := range resp.OutputItems {
		if item.ThinkingSignature != "" {
			add(degradationOut{
				Field:  "thinking_signature",
				Reason: core.DegradationUpstreamShapeMismatch,
				Detail: degradationDetailSignature,
			})
		}
		if item.EncryptedContent != "" {
			add(degradationOut{
				Field:  "encrypted_content",
				Reason: core.DegradationResponseToChat,
				Detail: degradationDetailEncrypted,
			})
		}
	}
	return out
}

// usageOutFrom 把 core.Usage 映射为 OpenAI Chat 形 usage 字段名（统一 SDD 4.3.1）：
// prompt_tokens / completion_tokens / total_tokens + prompt_tokens_details.cached_tokens 等。
// total_tokens 用 provider 原报值（⛔ 不重算，core.Usage.TotalTokens 口径）。
func usageOutFrom(u *core.Usage) *chatUsageOut {
	if u == nil {
		return nil
	}
	return &chatUsageOut{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		PromptTokensDetails: chatPromptDetailsOut{
			CachedTokens:     u.CacheReadTokens,
			CacheWriteTokens: u.CacheWriteTokens,
		},
		CompletionTokensDetails: chatCompletionDetailsOut{
			ReasoningTokens: u.ReasoningTokens,
		},
	}
}

// assembleMessage 把 core.OutputItems 合并成单条 assistant message（4.2.3：
// Responses→Chat 合并多 item 取 message）。text parts 是片段语义 ⇒ 以 "" 拼接；
// 推理文本取 ReasoningText，缺失才退回 summary[]（⛔ 不重复拼接）。
func assembleMessage(resp *core.Response) chatMessageOut {
	msg := chatMessageOut{Role: "assistant"}
	var text strings.Builder
	var reasoning []string
	for _, item := range resp.OutputItems {
		switch item.Type {
		case "message":
			if item.Role != "" {
				msg.Role = item.Role
			}
			for _, p := range item.Content {
				text.WriteString(p.Text)
			}
			if item.Refusal != "" {
				msg.Refusal = item.Refusal
			}
		case "reasoning", "thinking":
			if item.ReasoningText != "" {
				reasoning = append(reasoning, item.ReasoningText)
			} else {
				for _, s := range item.Summary {
					reasoning = append(reasoning, s.Text)
				}
			}
		case "function_call", "tool_use":
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			msg.ToolCalls = append(msg.ToolCalls, chatToolCallOut{
				ID:   id,
				Type: "function",
				Function: chatFunctionOut{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			})
		}
	}
	if text.Len() == 0 && resp.Text != "" {
		// 无 message item 时退回便捷字段（对应 Responses 的 output_text）。
		text.WriteString(resp.Text)
	}
	if text.Len() > 0 {
		s := text.String()
		msg.Content = &s
	}
	if len(reasoning) > 0 {
		msg.ReasoningContent = strings.Join(reasoning, "\n")
	}
	return msg
}

// EncodeResponse 把 core.Response 编码为 chat.completion 响应体（统一 SDD 3.4）。
// finish_reason 用超集原值（⛔ 不归一化、⛔ 不丢弃）；承载不了的语义经
// collectDegradations 落进顶层 x_gateway.degradations[]。
func EncodeResponse(resp *core.Response) ([]byte, error) {
	if resp == nil {
		return nil, errors.New("chat: EncodeResponse 收到 nil Response")
	}
	finish := resp.FinishReason
	var finishPtr *string
	if finish != "" {
		finishPtr = &finish
	}
	out := chatResponseOut{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []chatChoiceOut{{
			Index:        0,
			Message:      assembleMessage(resp),
			FinishReason: finishPtr,
		}},
		Usage: usageOutFrom(resp.Usage),
	}
	if degs := collectDegradations(resp); len(degs) > 0 {
		out.XGateway = &xGatewayOut{Degradations: degs}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("chat: 响应体序列化失败: %w", err)
	}
	return b, nil
}

// ---- 客户端侧帧编码器（内部帧 ⇒ chat.completion.chunk SSE data 行）----

// chunkIDGen 保证 id / created 单调：id 严格递增（进程内序号，⛔ 不复用），
// created 取墙上时钟（秒粒度）并保证非递减。
var chunkIDGen struct {
	sync.Mutex
	seq     int64
	created int64
}

// nextChunkID 生成下一个流的 id 与 created。
func nextChunkID() (string, int64) {
	chunkIDGen.Lock()
	defer chunkIDGen.Unlock()
	chunkIDGen.seq++
	now := time.Now().Unix()
	if now < chunkIDGen.created {
		now = chunkIDGen.created
	}
	chunkIDGen.created = now
	return "chatcmpl-" + strconv.FormatInt(chunkIDGen.seq, 10), now
}

// chatChunkOut 是 chat.completion.chunk（流式单帧）。
type chatChunkOut struct {
	ID       string            `json:"id"`
	Object   string            `json:"object"`
	Created  int64             `json:"created"`
	Model    string            `json:"model"`
	Choices  []chatChunkChoice `json:"choices"`
	Usage    *chatUsageOut     `json:"usage,omitempty"`
	XGateway *xGatewayOut      `json:"x_gateway,omitempty"`
}

// chatChunkChoice 是 chunk 的 choices[] 元素。
type chatChunkChoice struct {
	Index        int            `json:"index"`
	Delta        chatChunkDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

// chatChunkDelta 是增量体。
type chatChunkDelta struct {
	Content          string              `json:"content,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ToolCalls        []chatChunkToolCall `json:"tool_calls,omitempty"`
}

// chatChunkToolCall 是 delta.tool_calls[] 元素（index/id/type/function）。
type chatChunkToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function *chatChunkFunction `json:"function,omitempty"`
}

// chatChunkFunction 是增量里的 function 对象。
type chatChunkFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ChunkEncoder 把内部帧编码为 chat.completion.chunk 的 SSE data 行（统一 SDD 3.4）。
// 增量帧即时输出；FrameUsage / FrameDone 缓存到 Finish 统一收口——
// Finish 输出终止帧（finish_reason 非 null，payload 带 x_gateway.degradations[]）
// + usage 块（choices: []）+ data: [DONE]。同一编码器内 id / created 恒定。
//
// 帧约定（internal/stream Frame 承载）：
//   - FrameToolCallArgsDelta：ToolCallID=工具调用 id、Text=函数名（首帧携带）、
//     Arguments=参数增量；
//   - FrameReasoningDelta：推理文本增量 ⇒ delta.reasoning_content；签名帧
//     （ThinkingSignature 非空，见 stream.FrameReasoningDelta 承载约定）承载不了
//     ⇒ 自动记 thinking_signature 降级（随终止帧 x_gateway.degradations[] 输出）；
//   - FrameDone：Status=finish_reason 超集原值（⛔ 不归一化），可带 Usage；
//   - FrameUsage：整块 usage（include_usage 口径）。
type ChunkEncoder struct {
	mu           sync.Mutex
	model        string
	id           string
	created      int64
	finished     bool
	finishReason string
	usage        *core.Usage
	degradations []degradationOut
	// signatureLossNoted：thinking 签名丢失标记只记一次（多块签名去重口径与
	// collectDegradations 的 (field, reason) 一致）。
	signatureLossNoted bool
	callIndex          map[string]int
	callSeq            int
	// ---- 逐帧序列化复用暂存（Encode/Finish 持 e.mu 串行使用，⛔ 不并发）----
	// 帧装配结构与序列化缓冲帧间复用（减分配）：输出字节是暂存的独立拷贝，
	// 「返回值可跨帧持有」的既有契约不变。
	chunk   chatChunkOut         // 帧装配结构（每帧整值替换，⛔ 无脏字段残留）
	choices [1]chatChunkChoice   // choices[0] 槽位
	fn      chatChunkFunction    // tool_calls 的 function 槽位
	toolOne [1]chatChunkToolCall // tool_calls[0] 槽位
	sink    byteSink             // JSON 直接追加的复用暂存
	jenc    *json.Encoder        // 落点恒为 &sink（构造时定下）
}

// byteSink 把 json.Encoder 的写入直接追加进复用缓冲（⛔ 不经中间 []byte）。
type byteSink struct{ b []byte }

func (s *byteSink) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}

// reset 暂存复位并写入前缀。
func (s *byteSink) reset(prefix string) { s.b = append(s.b[:0], prefix...) }

// NewChunkEncoder 构造编码器；id / created 在此定下（单调生成）。
func NewChunkEncoder(model string) *ChunkEncoder {
	id, created := nextChunkID()
	e := &ChunkEncoder{
		model:     model,
		id:        id,
		created:   created,
		callIndex: make(map[string]int),
	}
	e.jenc = json.NewEncoder(&e.sink)
	return e
}

// AddDegradation 登记一条不可逆语义丢失标记，随终止帧 payload 输出
// （统一 SDD 3.4 流式位置）。Frame 契约无 Degradations 槽位，丢失事实由上游适配层
// 在识别到时注入；thinking 签名的流式丢失由 Encode 见签名帧时自登记（noteSignatureLoss）。
func (e *ChunkEncoder) AddDegradation(d core.Degradation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.degradations = append(e.degradations, degradationOut{
		Field:  d.Field,
		Reason: d.Reason,
		Detail: d.Detail,
	})
}

// noteSignatureLoss 登记 thinking 块签名在 Chat 出口的丢失（调用方已持 e.mu）。
// 多块签名重复帧只记一次 —— 与非流式 collectDegradations 的 (field, reason)
// 去重口径一致，⛔ 不刷屏重复标记。
func (e *ChunkEncoder) noteSignatureLoss() {
	if e.signatureLossNoted {
		return
	}
	e.signatureLossNoted = true
	e.degradations = append(e.degradations, degradationOut{
		Field:  "thinking_signature",
		Reason: core.DegradationUpstreamShapeMismatch,
		Detail: degradationDetailSignature,
	})
}

// Encode 把单个内部帧编码为一条 SSE data 行。
// FrameUsage / FrameDone 只记录（Finish 收口）⇒ 返回空；FrameError 无法在
// chat.completion.chunk 形状里承载 ⇒ 显式报错（⛔ 不静默吞）。
func (e *ChunkEncoder) Encode(f stream.Frame) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return nil, errors.New("chat: ChunkEncoder 已 Finish，不能再 Encode")
	}
	switch f.Kind {
	case stream.FrameTextDelta:
		return e.marshalChunk(chatChunkDelta{Content: f.Text}, nil)
	case stream.FrameReasoningDelta:
		// 签名帧（承载约定见 stream.FrameReasoningDelta）：Chat 出口承载不了
		// thinking 块签名 ⇒ 显式降级标记（统一 SDD 4.2.5 / 3.4，⛔ 不静默丢），
		// 随终止帧 x_gateway.degradations[] 输出；推理文本增量照常发。
		if f.ThinkingSignature != "" {
			e.noteSignatureLoss()
		}
		if f.Reasoning == "" {
			return nil, nil
		}
		return e.marshalChunk(chatChunkDelta{ReasoningContent: f.Reasoning}, nil)
	case stream.FrameToolCallArgsDelta:
		idx := e.callIndexFor(f.ToolCallID)
		e.fn = chatChunkFunction{Arguments: f.Arguments}
		if f.Text != "" {
			e.fn.Name = f.Text
		}
		e.toolOne[0] = chatChunkToolCall{Index: idx, Function: &e.fn}
		tc := &e.toolOne[0]
		if f.ToolCallID != "" {
			tc.ID = f.ToolCallID
			tc.Type = "function"
		}
		return e.marshalChunk(chatChunkDelta{ToolCalls: e.toolOne[:1]}, nil)
	case stream.FrameUsage:
		if f.Usage != nil {
			e.usage = f.Usage
		}
		return nil, nil
	case stream.FrameDone:
		// 空 Status（如 data: [DONE] 哨兵帧）不覆盖已记录的 finish_reason。
		if f.Status != "" {
			e.finishReason = f.Status
		}
		if f.Usage != nil {
			e.usage = f.Usage
		}
		return nil, nil
	case stream.FrameLifecycle:
		// chat.completion.chunk 形状无生命周期事件承载，终态由 Finish 收口。
		return nil, nil
	case stream.FrameError:
		return nil, fmt.Errorf("chat: 错误帧无法在 chat.completion.chunk 形状承载（Text=%q），须由传输层终止流并上报", f.Text)
	}
	return nil, fmt.Errorf("chat: 未知帧类型 %d", f.Kind)
}

// callIndexFor 计算 tool_calls 增量的 index：新 id 顺序取号；id 为空的续帧
// 归到最近一个工具调用（上游按调用顺序串行发参，4.3.1 流式 chunk 口径）。
func (e *ChunkEncoder) callIndexFor(toolCallID string) int {
	if toolCallID == "" {
		if e.callSeq > 0 {
			return e.callSeq - 1
		}
		return 0
	}
	if idx, ok := e.callIndex[toolCallID]; ok {
		return idx
	}
	idx := e.callSeq
	e.callSeq++
	e.callIndex[toolCallID] = idx
	return idx
}

// marshalChunk 组一帧 chunk 并序列化成 SSE data 行。
// 装配走编码器复用暂存（chunk/choices），序列化直写 sink（⛔ 不经中间 []byte）。
func (e *ChunkEncoder) marshalChunk(delta chatChunkDelta, finish *string) ([]byte, error) {
	e.chunk = chatChunkOut{
		ID:      e.id,
		Object:  "chat.completion.chunk",
		Created: e.created,
		Model:   e.model,
		Choices: e.choices[:1],
	}
	e.chunk.Choices[0] = chatChunkChoice{
		Index:        0,
		Delta:        delta,
		FinishReason: finish,
	}
	return e.marshalSSE(&e.chunk)
}

// marshalSSE 把 chunk 对象序列化成一条 SSE data 行（`data: ` + JSON + 结尾空行）。
// Encode 的输出自带一个 \n，补一个即 SSE 事件空行；返回值是暂存的独立拷贝
// （保留「调用方可持有返回值」的既有契约，⛔ 不返回 sink 别名）。
func (e *ChunkEncoder) marshalSSE(v any) ([]byte, error) {
	e.sink.reset("data: ")
	if err := e.jenc.Encode(v); err != nil {
		return nil, fmt.Errorf("chat: chunk 序列化失败: %w", err)
	}
	e.sink.b = append(e.sink.b, '\n')
	out := make([]byte, len(e.sink.b))
	copy(out, e.sink.b)
	return out, nil
}

// Finish 收口：终止帧（finish_reason 非 null + x_gateway.degradations[]）
// + usage 块（choices: []、usage 在顶层，4.3.1 include_usage 口径）+ data: [DONE]。
// 上游未给终止帧时按 stop 收口（中断场景由取消路径先补 FrameDone 再 Finish，统一 SDD 3.7）。
func (e *ChunkEncoder) Finish() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return nil, errors.New("chat: ChunkEncoder.Finish 重复调用（终止帧必须恰好一次）")
	}
	e.finished = true
	reason := e.finishReason
	if reason == "" {
		reason = core.FinishStop
	}
	term := chatChunkOut{
		ID:      e.id,
		Object:  "chat.completion.chunk",
		Created: e.created,
		Model:   e.model,
		Choices: []chatChunkChoice{{
			Index:        0,
			Delta:        chatChunkDelta{},
			FinishReason: &reason,
		}},
	}
	if len(e.degradations) > 0 {
		term.XGateway = &xGatewayOut{Degradations: e.degradations}
	}
	out, err := e.marshalSSE(&term)
	if err != nil {
		return nil, err
	}
	if e.usage != nil {
		usageChunk := chatChunkOut{
			ID:      e.id,
			Object:  "chat.completion.chunk",
			Created: e.created,
			Model:   e.model,
			Choices: []chatChunkChoice{},
			Usage:   usageOutFrom(e.usage),
		}
		b, err := e.marshalSSE(&usageChunk)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	out = append(out, []byte("data: [DONE]\n\n")...)
	return out, nil
}

// ---- 上游 chunk 解码（chat.completion.chunk ⇒ 内部帧）----

// chunkIn 是上游 chat.completion.chunk。
type chunkIn struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// ParseChunk 把上游 chat.completion.chunk 的 data 载荷解码为内部帧
// （统一 SDD 3.7 帧映射）：
//
//   - delta.content            ⇒ FrameTextDelta
//   - delta.reasoning_content  ⇒ FrameReasoningDelta（Qwen/Kimi/MiMo/MiniMax/智谱/DeepSeek 扩展）
//   - delta.tool_calls         ⇒ FrameToolCallArgsDelta（Text 载函数名）
//   - choices:[] + usage       ⇒ FrameUsage（usage 为 null 时容忍、不出帧）
//   - finish_reason 非 null    ⇒ FrameDone（Status=超集原值，⛔ 不归一化）
//   - data:[DONE] 字面量       ⇒ FrameDone
//
// usage 归一化走 usage.Normalize（4.4 单一口径真源）。
func ParseChunk(provider core.Provider, data []byte) ([]stream.Frame, error) {
	trim := bytes.TrimSpace(data)
	if len(trim) == 0 {
		return nil, nil
	}
	if bytes.Equal(trim, []byte("[DONE]")) {
		return []stream.Frame{{Kind: stream.FrameDone}}, nil
	}
	var chunk chunkIn
	if err := json.Unmarshal(trim, &chunk); err != nil {
		return nil, fmt.Errorf("chat: chunk 不是合法 JSON: %w", err)
	}
	var frames []stream.Frame
	var doneStatuses []string
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: c.Delta.Content})
		}
		if c.Delta.ReasoningContent != "" {
			frames = append(frames, stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: c.Delta.ReasoningContent})
		}
		for _, tc := range c.Delta.ToolCalls {
			fr := stream.Frame{
				Kind:       stream.FrameToolCallArgsDelta,
				ToolCallID: tc.ID,
				Arguments:  tc.Function.Arguments,
			}
			if tc.Function.Name != "" {
				fr.Text = tc.Function.Name
			}
			frames = append(frames, fr)
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			doneStatuses = append(doneStatuses, *c.FinishReason)
		}
	}
	// usage 为 null / 缺失均容忍（4.3.1：其余 chunk 的 usage 为 null）。
	usageRaw := bytes.TrimSpace(chunk.Usage)
	if len(usageRaw) > 0 && !bytes.Equal(usageRaw, []byte("null")) {
		frames = append(frames, stream.Frame{
			Kind:  stream.FrameUsage,
			Usage: usage.Normalize(provider, core.APIShapeOpenAIChat, usageRaw),
		})
	}
	for _, st := range doneStatuses {
		frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: st})
	}
	return frames, nil
}
