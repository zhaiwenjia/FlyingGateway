// 日期: 2026-11-14 19:39:55.264
// 作者: Zhai Wenjia

// Package responses 实现 OpenAI Responses 形状的入站双模式编解码（统一 SDD 3.4）。
//
//   - DecodeRequest：response 创建请求体 ⇒ core.Request。input 字符串 ⇒
//     Messages[0]{Role:"user"}；input item 数组逐项映射（message / function_call /
//     function_call_output / reasoning），reasoning item 的 encrypted_content 必须保留。
//   - EncodeResponse：core.Response ⇒ response 对象（object="response"、status 六值、
//     output[] 异构联合、output_text 便捷字段）。thinking 块签名落 reasoning item 的
//     signature 字段（Responses 形状承载签名 ⇒ 无损，⛔ 不标记）；其余承载不了的语义
//     ⇒ 顶层 x_gateway.degradations[] 显式标记（⛔ 不静默丢）。
//   - EventEncoder：内部帧 ⇒ response.* 事件流（event: 与 data: 双字段，事件名与
//     data.type 一致，sequence_number 单调递增，终止为恰好一个终态事件后接 data: [DONE]）。
//   - ParseEvent：上游 response.* 事件 ⇒ 内部帧；容忍缺事件、乱序、缺 [DONE]、
//     未知事件类型（统一 SDD 4.3.1 解析器容忍性）。
//
// 字段名 / 枚举与统一 SDD 逐字一致；读侧宽容（未知字段 / 未知 item 类型忽略）。
package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// ---- 入站解码 wire 结构（读侧宽容：未声明的字段一律忽略）----

// responsesRequest 是 /v1/responses 请求体（统一 SDD 4.3.1 Responses 第二模式）。
type responsesRequest struct {
	Model              string           `json:"model"`
	Input              json.RawMessage  `json:"input"` // string 或 item 数组
	Instructions       string           `json:"instructions"`
	Tools              []respToolIn     `json:"tools"`
	ToolChoice         json.RawMessage  `json:"tool_choice"`
	MaxOutputTokens    int              `json:"max_output_tokens"`
	Temperature        *float64         `json:"temperature"`
	TopP               *float64         `json:"top_p"`
	Stream             bool             `json:"stream"`
	PreviousResponseID string           `json:"previous_response_id"`
	Conversation       json.RawMessage  `json:"conversation"` // string 或 {id}
	PromptCacheKey     string           `json:"prompt_cache_key"`
	Reasoning          *respReasoningIn `json:"reasoning"`
}

// respReasoningIn 是 reasoning 控制对象（effort / summary / context / mode）。
type respReasoningIn struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
	Context string `json:"context"`
	Mode    string `json:"mode"`
}

// respToolIn 是 tools[] 元素（function 形状）。
type respToolIn struct {
	Type     string `json:"type"`
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// respInputItem 是 input item 数组元素（异构联合，4.3.1 item 内部结构）。
type respInputItem struct {
	Type             string          `json:"type"`
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"` // message：string 或 part 数组；reasoning：part 数组
	ID               string          `json:"id"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Arguments        string          `json:"arguments"`
	Output           json.RawMessage `json:"output"` // function_call_output：string 或 part 数组
	EncryptedContent string          `json:"encrypted_content"`
	// Signature 是 reasoning item 的 thinking 签名（Anthropic thinking.signature 的
	// Responses 对应物）。⚠️ 2026-11-14 补：出站侧已承载签名，入站回传方向原先漏解析
	// ⇒ Message.ThinkingSignature 无生产者、多轮续聊签名被丢——回传丢失与出站丢失同罪
	//（1.4.1 语义字段），必须保留。
	Signature string         `json:"signature"`
	Summary   []respTextPart `json:"summary"`
}

// respTextPart 是 reasoning 的 summary[] / content[] 元素（summary_text / reasoning_text）。
type respTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// respContentPart 是 input message content part 元素（input_text / input_image /
// input_file / input_audio 等，类型逐字保留）。
type respContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Refusal  string `json:"refusal"`
	ImageURL string `json:"image_url"`
	FileData string `json:"file_data"`
	FileID   string `json:"file_id"`
	Filename string `json:"filename"`
	Data     string `json:"data"`
	Format   string `json:"format"`
}

// DecodeRequest 把 /v1/responses 请求体解码为 core.Request（统一 SDD 3.4）：
// input 字符串 ⇒ Messages[0]{Role:"user"}；item 数组逐项映射；
// instructions ⇒ core.Instructions（⛔ 不并进 messages）；reasoning ⇒ core.ReasoningConfig。
// 未知 item 类型容忍跳过（4.3.1 输出联合有 29 种 item，规范模型只承载语义四类）。
// knownResponsesTopLevelKeys 返回 responsesRequest 的 JSON 顶层键集
// （反射自结构体 tag，单一真源）。CAC-11（2026-11-26）：白名单外顶层键进
// core.Request.Extra 透传（供应商特有参数通道）。
func knownResponsesTopLevelKeys() map[string]bool {
	t := reflect.TypeOf(responsesRequest{})
	keys := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}

func DecodeRequest(body []byte) (*core.Request, error) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("responses: 请求体不是合法 JSON: %w", err)
	}
	out := &core.Request{
		Model:              req.Model,
		Instructions:       req.Instructions,
		Temperature:        req.Temperature,
		TopP:               req.TopP,
		Stream:             req.Stream,
		MaxOutputTokens:    req.MaxOutputTokens,
		PreviousResponseID: req.PreviousResponseID,
		PromptCacheKey:     req.PromptCacheKey,
	}
	// CAC-11：白名单外顶层键进 Extra 透传（与 chat 入口同款语义）。
	var rawTop map[string]json.RawMessage
	if err := json.Unmarshal(body, &rawTop); err == nil {
		known := knownResponsesTopLevelKeys()
		for k, v := range rawTop {
			if known[k] {
				continue
			}
			var val any
			if err := json.Unmarshal(v, &val); err != nil {
				continue
			}
			if out.Extra == nil {
				out.Extra = map[string]any{}
			}
			out.Extra[k] = val
		}
	}
	conv, err := decodeConversation(req.Conversation)
	if err != nil {
		return nil, fmt.Errorf("responses: conversation 解析失败: %w", err)
	}
	out.Conversation = conv
	msgs, err := decodeInput(req.Input)
	if err != nil {
		return nil, err
	}
	out.Messages = msgs
	// 结构预分配：tools 按入参条数预留（空入参保持 nil，序列化形态不变）。
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
		return nil, fmt.Errorf("responses: tool_choice 解析失败: %w", err)
	}
	out.ToolChoice = tc
	if req.Reasoning != nil {
		out.Reasoning = &core.ReasoningConfig{
			Effort:  req.Reasoning.Effort,
			Summary: req.Reasoning.Summary,
			Context: req.Reasoning.Context,
			Mode:    req.Reasoning.Mode,
		}
	}
	return out, nil
}

// decodeConversation 解析 conversation：字符串或 {"id": "..."} 都收。
func decodeConversation(raw json.RawMessage) (string, error) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || bytes.Equal(trim, []byte("null")) {
		return "", nil
	}
	if trim[0] == '"' {
		var s string
		if err := json.Unmarshal(trim, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(trim, &obj); err != nil {
		return "", err
	}
	return obj.ID, nil
}

// decodeInput 解析 input：字符串 ⇒ Messages[0]{Role:"user"}；item 数组逐项映射。
func decodeInput(raw json.RawMessage) ([]core.Message, error) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || bytes.Equal(trim, []byte("null")) {
		return nil, nil
	}
	if trim[0] == '"' {
		var s string
		if err := json.Unmarshal(trim, &s); err != nil {
			return nil, err
		}
		return []core.Message{{
			Role:    "user",
			Content: []core.ContentPart{{Type: "text", Text: s}},
		}}, nil
	}
	var items []respInputItem
	if err := json.Unmarshal(trim, &items); err != nil {
		return nil, fmt.Errorf("responses: input 解析失败: %w", err)
	}
	// 结构预分配：messages 按 item 条数预留（空数组保持 nil，序列化形态不变）。
	var out []core.Message
	if len(items) > 0 {
		out = make([]core.Message, 0, len(items))
	}
	for _, it := range items {
		switch it.Type {
		case "message":
			content, err := decodeContent(it.Content)
			if err != nil {
				return nil, fmt.Errorf("responses: message item content 解析失败: %w", err)
			}
			out = append(out, core.Message{Role: it.Role, Content: content})
		case "function_call":
			// function_call item ⇒ Message{ToolCalls}（4.2.2：多 call_id 字段，
			// ID=item id、CallID=call_id，⛔ 不合并）。
			out = append(out, core.Message{
				Role: "assistant",
				ToolCalls: []core.ToolCall{{
					ID:        it.ID,
					CallID:    it.CallID,
					Name:      it.Name,
					Arguments: it.Arguments,
					Type:      "function",
				}},
			})
		case "function_call_output":
			content, err := decodeContent(it.Output)
			if err != nil {
				return nil, fmt.Errorf("responses: function_call_output 解析失败: %w", err)
			}
			out = append(out, core.Message{
				Role:       "tool",
				ToolCallID: it.CallID,
				Content:    content,
			})
		case "reasoning":
			// reasoning item ⇒ Message{ReasoningContent, EncryptedContent}：
			// encrypted_content 必须保留（⛔ 丢弃，统一 SDD 3.4 / 4.2.5）。
			var contentParts []respTextPart
			if len(bytes.TrimSpace(it.Content)) > 0 {
				if err := json.Unmarshal(it.Content, &contentParts); err != nil {
					return nil, fmt.Errorf("responses: reasoning item content 解析失败: %w", err)
				}
			}
			text := joinTextParts(contentParts)
			if text == "" {
				// content[] 无正文时退回 summary[]（文本不丢，结构压平到 ReasoningContent）。
				text = joinSummaryParts(it.Summary)
			}
			out = append(out, core.Message{
				Role:              "assistant",
				ReasoningContent:  text,
				EncryptedContent:  it.EncryptedContent,
				ThinkingSignature: it.Signature,
			})
		default:
			// 未知 item 类型（web_search_call / mcp_call 等 29 种联合的其余项）
			// ⇒ 容忍跳过（4.3.1 解析器容忍性）。
		}
	}
	return out, nil
}

// decodeContent 解析 content / output 载荷：字符串 ⇒ 单个 text part；数组 ⇒ 逐 part 映射。
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
	var parts []respContentPart
	if err := json.Unmarshal(trim, &parts); err != nil {
		return nil, err
	}
	out := make([]core.ContentPart, 0, len(parts))
	for _, p := range parts {
		cp := core.ContentPart{Type: p.Type, Text: p.Text}
		if p.Refusal != "" {
			// refusal part 无独立槽位 ⇒ 文本载入 Text，类型逐字保留。
			cp.Text = p.Refusal
		}
		cp.ImageURL = p.ImageURL
		switch {
		case p.FileData != "":
			cp.Data = p.FileData
		case p.Data != "":
			cp.Data = p.Data
			if p.Format != "" {
				cp.MIMEType = "audio/" + p.Format
			}
		}
		out = append(out, cp)
	}
	return out, nil
}

// joinTextParts 拼 reasoning item 的 content[]（reasoning_text）文本，块间 "\n"。
func joinTextParts(parts []respTextPart) string {
	var blocks []string
	for _, p := range parts {
		if p.Text != "" {
			blocks = append(blocks, p.Text)
		}
	}
	return strings.Join(blocks, "\n")
}

// joinSummaryParts 拼 reasoning item 的 summary[]（summary_text）文本，块间 "\n"。
func joinSummaryParts(parts []respTextPart) string {
	var blocks []string
	for _, p := range parts {
		if p.Text != "" {
			blocks = append(blocks, p.Text)
		}
	}
	return strings.Join(blocks, "\n")
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

// respResponseOut 是 response 对象（4.3.1 响应体关键字段）。
type respResponseOut struct {
	ID         string             `json:"id"`
	Object     string             `json:"object"` // "response"
	CreatedAt  int64              `json:"created_at"`
	Status     string             `json:"status"` // 六值
	Model      string             `json:"model"`
	Output     []any              `json:"output"`
	OutputText string             `json:"output_text"`
	Usage      *respUsageOut      `json:"usage,omitempty"`
	Incomplete *respIncompleteOut `json:"incomplete_details,omitempty"`
	Error      *respErrorOut      `json:"error,omitempty"`
	XGateway   *xGatewayOut       `json:"x_gateway,omitempty"`
}

// respMessageItemOut 是 output[] 的 message item（content: output_text / refusal）。
type respMessageItemOut struct {
	ID      string        `json:"id,omitempty"`
	Type    string        `json:"type"` // "message"
	Role    string        `json:"role"`
	Content []respPartOut `json:"content"`
}

// respPartOut 是 message item 的 content 元素（output_text / refusal）。
type respPartOut struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Annotations []any  `json:"annotations,omitempty"`
	Refusal     string `json:"refusal,omitempty"`
}

// respReasoningItemOut 是 output[] 的 reasoning item（summary[] / content[] /
// encrypted_content / signature，⛔ encrypted_content 与 signature 不压平）。
type respReasoningItemOut struct {
	ID               string         `json:"id,omitempty"`
	Type             string         `json:"type"` // "reasoning"
	Summary          []respTextPart `json:"summary"`
	Content          []respTextPart `json:"content,omitempty"`
	EncryptedContent string         `json:"encrypted_content,omitempty"`
	Signature        string         `json:"signature,omitempty"`
}

// respFunctionCallItemOut 是 output[] 的 function_call item（id / call_id / name / arguments）。
type respFunctionCallItemOut struct {
	ID        string `json:"id,omitempty"`
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// respUsageOut 是 Responses 形 usage（4.3.1：input_tokens / output_tokens / 二级 details）。
type respUsageOut struct {
	InputTokens         int64                `json:"input_tokens"`
	OutputTokens        int64                `json:"output_tokens"`
	TotalTokens         int64                `json:"total_tokens"`
	InputTokensDetails  respInputDetailsOut  `json:"input_tokens_details"`
	OutputTokensDetails respOutputDetailsOut `json:"output_tokens_details"`
}

// respInputDetailsOut 是 usage.input_tokens_details。
type respInputDetailsOut struct {
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// respOutputDetailsOut 是 usage.output_tokens_details。
type respOutputDetailsOut struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// respIncompleteOut 是 incomplete_details（reason 取值 4.3.1：max_output_tokens /
// max_messages / content_filter / steered）。
type respIncompleteOut struct {
	Reason string `json:"reason"`
}

// respErrorOut 是 response.error。
type respErrorOut struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
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

// collectDegradations 汇总不可逆语义丢失：Responses 出口承载 encrypted_content 与
// thinking 块签名（reasoning item 的 encrypted_content / signature）⇒ 不标记；
// 此处透传上游适配层识别的丢失记录（resp.Degradations），同 (field, reason) 只记一次
// （统一 SDD 3.4 / 验收 A8、A14，⛔ 不静默丢）。
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
	return out
}

// usageOutFrom 把 core.Usage 映射为 Responses 形 usage 字段名（统一 SDD 4.3.1）。
// total_tokens 用 provider 原报值（⛔ 不重算）。
func usageOutFrom(u *core.Usage) *respUsageOut {
	if u == nil {
		return nil
	}
	return &respUsageOut{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens,
		InputTokensDetails: respInputDetailsOut{
			CachedTokens:     u.CacheReadTokens,
			CacheWriteTokens: u.CacheWriteTokens,
		},
		OutputTokensDetails: respOutputDetailsOut{
			ReasoningTokens: u.ReasoningTokens,
		},
	}
}

// buildOutputItems 把 core.OutputItems 映射为 output[] 异构联合（4.3.1 item 内部结构）：
// message item（content: output_text / refusal）、reasoning item（summary[] / content[] /
// encrypted_content）、function_call item（id / call_id / name / arguments）。
// text parts 是片段语义 ⇒ 以 "" 拼接便捷字段 output_text。
func buildOutputItems(resp *core.Response) (items []any, outputText string) {
	var text strings.Builder
	for _, item := range resp.OutputItems {
		switch item.Type {
		case "message":
			role := item.Role
			if role == "" {
				role = "assistant"
			}
			m := respMessageItemOut{ID: item.ID, Type: "message", Role: role}
			for _, p := range item.Content {
				if p.Text == "" {
					continue
				}
				m.Content = append(m.Content, respPartOut{
					Type:        "output_text",
					Text:        p.Text,
					Annotations: []any{},
				})
				text.WriteString(p.Text)
			}
			if item.Refusal != "" {
				m.Content = append(m.Content, respPartOut{Type: "refusal", Refusal: item.Refusal})
			}
			items = append(items, m)
		case "reasoning", "thinking":
			r := respReasoningItemOut{ID: item.ID, Type: "reasoning"}
			for _, s := range item.Summary {
				r.Summary = append(r.Summary, respTextPart{Type: "summary_text", Text: s.Text})
			}
			if r.Summary == nil {
				r.Summary = []respTextPart{}
			}
			if item.ReasoningText != "" {
				r.Content = []respTextPart{{Type: "reasoning_text", Text: item.ReasoningText}}
			}
			r.EncryptedContent = item.EncryptedContent
			// thinking 块签名 ⇒ item.signature（Responses 形状承载签名 ⇒ 无损，⛔ 不压平）。
			r.Signature = item.ThinkingSignature
			items = append(items, r)
		case "function_call", "tool_use":
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			items = append(items, respFunctionCallItemOut{
				ID:        item.ID,
				Type:      "function_call",
				CallID:    callID,
				Name:      item.Name,
				Arguments: item.Arguments,
			})
		}
	}
	return items, text.String()
}

// EncodeResponse 把 core.Response 编码为 response 对象（统一 SDD 3.4）。
// status 用六值原值（⛔ 不改写）；承载不了的语义经 collectDegradations 落进
// 顶层 x_gateway.degradations[]。
func EncodeResponse(resp *core.Response) ([]byte, error) {
	if resp == nil {
		return nil, errors.New("responses: EncodeResponse 收到 nil Response")
	}
	status := resp.Status
	if status == "" {
		status = core.StatusCompleted
	}
	items, outputText := buildOutputItems(resp)
	out := respResponseOut{
		ID:         resp.ID,
		Object:     "response",
		CreatedAt:  time.Now().Unix(),
		Status:     status,
		Model:      resp.Model,
		Output:     items,
		OutputText: outputText,
		Usage:      usageOutFrom(resp.Usage),
	}
	if out.Output == nil {
		out.Output = []any{}
	}
	if resp.Error != nil {
		out.Error = &respErrorOut{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	if degs := collectDegradations(resp); len(degs) > 0 {
		out.XGateway = &xGatewayOut{Degradations: degs}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("responses: 响应体序列化失败: %w", err)
	}
	return b, nil
}

// ---- 客户端侧事件编码器（内部帧 ⇒ response.* 事件流）----

// eventIDGen 保证 id / created_at 单调：id 严格递增（进程内序号，⛔ 不复用），
// created_at 取墙上时钟（秒粒度）并保证非递减。
var eventIDGen struct {
	sync.Mutex
	seq     int64
	created int64
}

// nextEventID 生成下一个响应流的 id 与 created_at。
func nextEventID() (string, int64) {
	eventIDGen.Lock()
	defer eventIDGen.Unlock()
	eventIDGen.seq++
	now := time.Now().Unix()
	if now < eventIDGen.created {
		now = eventIDGen.created
	}
	eventIDGen.created = now
	return "resp_" + fmt.Sprintf("%d", eventIDGen.seq), now
}

// respEventOut 是 response.* 事件的 data 载荷（event 名与 type 一致）。
type respEventOut struct {
	Type           string           `json:"type"`
	SequenceNumber int64            `json:"sequence_number"`
	OutputIndex    int              `json:"output_index,omitempty"`
	ContentIndex   int              `json:"content_index,omitempty"`
	ItemID         string           `json:"item_id,omitempty"`
	Delta          string           `json:"delta,omitempty"`
	Response       *respResponseOut `json:"response,omitempty"`
}

// EventEncoder 把内部帧编码为 response.* SSE 事件（统一 SDD 3.4 / 4.3.1）：
// 每个事件 `event:` + `data:` 双字段、事件名与 data.type 一致、sequence_number 单调递增；
// 终止为恰好一个终态事件（response.completed / failed / incomplete）后接 data: [DONE]。
//
// 帧约定（internal/stream Frame 承载）：
//   - FrameToolCallArgsDelta：ToolCallID=call_id、Text=函数名（仅补名帧，不发事件）、
//     Arguments=参数增量；
//   - FrameReasoningDelta：推理文本增量发 reasoning_text.delta；签名帧
//     （ThinkingSignature 非空，见 stream.FrameReasoningDelta 承载约定）不发事件，
//     只累积进 reasoning item 的 signature 字段（Responses 形状承载签名 ⇒ 无损）；
//   - FrameDone：Status=六值状态原值；FrameError：Text=错误消息、Status=错误码。
//
// reasoningBlock 是一个 thinking 块的流式累积（正文增量 + 块签名）。
// 签名帧收口当前块并落签名（⛔ 不可压平）⇒ 多 thinking 块逐块保真，
// 与非流式 EncodeResponse 的逐块 item 同形。
type reasoningBlock struct {
	parts []string
	sig   string
}

type EventEncoder struct {
	mu           sync.Mutex
	model        string
	id           string
	created      int64
	seq          int64
	finished     bool
	doneStatus   string
	errMessage   string
	errCode      string
	text         strings.Builder
	reasoning    []reasoningBlock
	callNames    map[string]string
	callArgs     map[string]string
	callOrder    []string
	usage        *core.Usage
	degradations []degradationOut
	callIndex    map[string]int
	callSeq      int
	// ---- 逐帧序列化复用暂存（Encode/Finish 持 e.mu 串行使用，⛔ 不并发）----
	// 事件装配结构与序列化缓冲帧间复用（减分配）：输出字节是暂存的独立拷贝，
	// 「返回值可跨帧持有」的既有契约不变。
	ev   respEventOut // 事件装配结构（每帧整值替换，⛔ 无脏字段残留）
	sink byteSink     // JSON 直接追加的复用暂存
	jenc *json.Encoder
}

// byteSink 把 json.Encoder 的写入直接追加进复用缓冲（⛔ 不经中间 []byte）。
type byteSink struct{ b []byte }

func (s *byteSink) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}

// reset 暂存复位并写入前缀。
func (s *byteSink) reset(prefix string) { s.b = append(s.b[:0], prefix...) }

// NewEventEncoder 构造编码器；id / created_at 在此定下（单调生成）。
func NewEventEncoder(model string) *EventEncoder {
	id, created := nextEventID()
	e := &EventEncoder{
		model:     model,
		id:        id,
		created:   created,
		callNames: make(map[string]string),
		callArgs:  make(map[string]string),
		callIndex: make(map[string]int),
	}
	e.jenc = json.NewEncoder(&e.sink)
	return e
}

// AddDegradation 登记一条不可逆语义丢失标记，随终态事件的 response 对象输出
// （统一 SDD 3.4 流式位置）。Frame 契约无 Degradations 槽位（⛔ 不改 internal/stream），
// 丢失事实由上游适配层在识别到时注入。
func (e *EventEncoder) AddDegradation(d core.Degradation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.degradations = append(e.degradations, degradationOut{
		Field:  d.Field,
		Reason: d.Reason,
		Detail: d.Detail,
	})
}

// Encode 把单个内部帧编码为一条 response.* 事件（event: + data: 双字段）。
// FrameUsage / FrameDone / FrameError 记录到终态收口；FrameLifecycle 无客户端语义，忽略。
func (e *EventEncoder) Encode(f stream.Frame) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return nil, errors.New("responses: EventEncoder 已 Finish，不能再 Encode")
	}
	switch f.Kind {
	case stream.FrameTextDelta:
		e.text.WriteString(f.Text)
		return e.marshalEvent("response.output_text.delta", respEventOut{
			Type:         "response.output_text.delta",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        f.Text,
		})
	case stream.FrameReasoningDelta:
		// 签名帧（承载约定见 stream.FrameReasoningDelta）：只累积进 reasoning item 的
		// signature（终态 output[] 承载，无损），不发增量事件；推理文本增量照常发。
		e.noteReasoning(f.Reasoning, f.ThinkingSignature)
		if f.Reasoning == "" {
			return nil, nil
		}
		return e.marshalEvent("response.reasoning_text.delta", respEventOut{
			Type:  "response.reasoning_text.delta",
			Delta: f.Reasoning,
		})
	case stream.FrameToolCallArgsDelta:
		if f.Arguments == "" && f.Text != "" {
			// 补名帧（output_item.added 语义）：只登记函数名，不发事件。
			e.noteCall(f.ToolCallID, f.Text, "")
			return nil, nil
		}
		e.noteCall(f.ToolCallID, f.Text, f.Arguments)
		idx := e.callIndexFor(f.ToolCallID)
		return e.marshalEvent("response.function_call_arguments.delta", respEventOut{
			Type:        "response.function_call_arguments.delta",
			OutputIndex: idx,
			ItemID:      f.ToolCallID,
			Delta:       f.Arguments,
		})
	case stream.FrameUsage:
		if f.Usage != nil {
			e.usage = f.Usage
		}
		return nil, nil
	case stream.FrameDone:
		// 空 Status（如 data: [DONE] 哨兵帧）不覆盖已记录的终态。
		if f.Status != "" {
			e.doneStatus = f.Status
		}
		if f.Usage != nil {
			e.usage = f.Usage
		}
		return nil, nil
	case stream.FrameError:
		e.errMessage = f.Text
		e.errCode = f.Status
		return nil, nil
	case stream.FrameLifecycle:
		return nil, nil
	}
	return nil, fmt.Errorf("responses: 未知帧类型 %d", f.Kind)
}

// noteReasoning 累积推理增量与 thinking 块签名：签名帧（sig 非空）收口当前块并落
// 签名（已收口的块不再追加正文，后到正文属下一个块）⇒ 多 thinking 块逐块保真。
func (e *EventEncoder) noteReasoning(text, sig string) {
	if sig != "" {
		if n := len(e.reasoning); n == 0 || e.reasoning[n-1].sig != "" {
			e.reasoning = append(e.reasoning, reasoningBlock{sig: sig})
		} else {
			e.reasoning[n-1].sig = sig
		}
	}
	if text == "" {
		return
	}
	if n := len(e.reasoning); n == 0 || e.reasoning[n-1].sig != "" {
		e.reasoning = append(e.reasoning, reasoningBlock{})
	}
	last := &e.reasoning[len(e.reasoning)-1]
	last.parts = append(last.parts, text)
}

// noteCall 登记工具调用的名称 / 参数增量（按到达顺序归属，⚠️ Frame 无 index 槽位，
// 多工具并发交错的续帧只能归属最近一个调用）。
func (e *EventEncoder) noteCall(toolCallID, name, args string) {
	key := toolCallID
	if key == "" {
		if len(e.callOrder) > 0 {
			key = e.callOrder[len(e.callOrder)-1]
		} else {
			// 首帧就无 id：用哨兵键占位，保持顺序。
			key = "_"
		}
	}
	if _, seen := e.callNames[key]; !seen {
		// 首见即入序并建槽（⛔ 不看 name 是否为空，否则无名调用会重复入序）。
		e.callNames[key] = name
		e.callArgs[key] = ""
		e.callOrder = append(e.callOrder, key)
	} else if name != "" {
		e.callNames[key] = name
	}
	e.callArgs[key] += args
}

// callIndexFor 计算 function_call 增量的 output_index（新 id 顺序取号；空 id 归最近调用）。
func (e *EventEncoder) callIndexFor(toolCallID string) int {
	key := toolCallID
	if key == "" {
		if e.callSeq > 0 {
			return e.callSeq - 1
		}
		return 0
	}
	if idx, ok := e.callIndex[key]; ok {
		return idx
	}
	idx := e.callSeq
	e.callSeq++
	e.callIndex[key] = idx
	return idx
}

// marshalEvent 组一条事件：`event:` + `data:` 双字段（事件名与 data.type 一致），
// sequence_number 单调递增。装配走编码器复用暂存（ev 整值替换）、序列化直写
// sink（⛔ 不经中间 []byte）；返回值是暂存的独立拷贝（⛔ 不返回 sink 别名）。
func (e *EventEncoder) marshalEvent(name string, ev respEventOut) ([]byte, error) {
	e.ev = ev // 整值替换：上一帧的未设置字段随值拷贝归零，⛔ 无脏字段残留
	e.ev.SequenceNumber = e.seq
	e.seq++
	e.sink.reset("event: ")
	e.sink.b = append(e.sink.b, name...)
	e.sink.b = append(e.sink.b, '\n')
	e.sink.b = append(e.sink.b, "data: "...)
	if err := e.jenc.Encode(&e.ev); err != nil {
		return nil, fmt.Errorf("responses: 事件序列化失败: %w", err)
	}
	e.sink.b = append(e.sink.b, '\n') // Encode 自带一个 \n，补足 SSE 事件空行
	out := make([]byte, len(e.sink.b))
	copy(out, e.sink.b)
	return out, nil
}

// terminalResponse 组终态事件携带的 response 对象（从累计帧回装 output[]）。
func (e *EventEncoder) terminalResponse(status string) *respResponseOut {
	resp := &core.Response{
		ID:     e.id,
		Model:  e.model,
		Status: status,
		Text:   e.text.String(),
		Usage:  e.usage,
	}
	// 逐块装配 reasoning item：签名落 item 的 signature 字段（与非流式同形，⛔ 不压平）。
	for _, b := range e.reasoning {
		if len(b.parts) == 0 && b.sig == "" {
			continue
		}
		resp.OutputItems = append(resp.OutputItems, core.OutputItem{
			Type:              "reasoning",
			ReasoningText:     strings.Join(b.parts, "\n"),
			ThinkingSignature: b.sig,
		})
	}
	if e.text.Len() > 0 {
		resp.OutputItems = append(resp.OutputItems, core.OutputItem{
			Type:    "message",
			Role:    "assistant",
			Content: []core.ContentPart{{Type: "text", Text: e.text.String()}}, // core 树口径 "text"（wire 的 output_text 只在出口编码层出现，见 2026-11-17 messages 丢正文修复）
		})
	}
	for _, key := range e.callOrder {
		id := key
		if key == "_" {
			id = ""
		}
		resp.OutputItems = append(resp.OutputItems, core.OutputItem{
			Type:      "function_call",
			ID:        id,
			CallID:    id,
			Name:      e.callNames[key],
			Arguments: e.callArgs[key],
		})
	}
	items, outputText := buildOutputItems(resp)
	out := &respResponseOut{
		ID:         e.id,
		Object:     "response",
		CreatedAt:  e.created,
		Status:     status,
		Model:      e.model,
		Output:     items,
		OutputText: outputText,
		Usage:      usageOutFrom(e.usage),
	}
	if out.Output == nil {
		out.Output = []any{}
	}
	if len(e.degradations) > 0 {
		out.XGateway = &xGatewayOut{Degradations: e.degradations}
	}
	return out
}

// Finish 收口：恰好一个终态事件（response.completed / failed / incomplete）
// 后接 data: [DONE]（统一 SDD 4.3.1 终止口径）。
//   - FrameError 已见 ⇒ response.failed（带 error 对象）；
//   - FrameDone.Status=incomplete ⇒ response.incomplete（带 incomplete_details.reason）；
//   - 其余 ⇒ response.completed（带完整 usage 与 output[]）。
func (e *EventEncoder) Finish() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return nil, errors.New("responses: EventEncoder.Finish 重复调用（终态事件必须恰好一个）")
	}
	e.finished = true
	// 终态事件只有三值（completed / failed / incomplete，4.3.1 终止口径）；
	// response.status 保留原值（六值），⛔ 不覆写。
	status := e.doneStatus
	var name string
	switch {
	case status == core.StatusFailed || (status == "" && e.errMessage != ""):
		name = "response.failed"
	case status == core.StatusIncomplete, status == core.StatusCancelled:
		// 取消不算错误（3.7）⇒ 不进 failed；按未完成收口（cancelled 保留原值）。
		name = "response.incomplete"
	case status == "":
		name = "response.completed"
		status = core.StatusCompleted
	default:
		name = "response.completed"
	}
	resp := e.terminalResponse(status)
	switch name {
	case "response.failed":
		resp.Error = &respErrorOut{Code: e.errCode, Message: e.errMessage}
	case "response.incomplete":
		resp.Incomplete = &respIncompleteOut{Reason: e.errMessage}
	}
	out, err := e.marshalEvent(name, respEventOut{
		Type:     name,
		Response: resp,
	})
	if err != nil {
		return nil, err
	}
	out = append(out, []byte("data: [DONE]\n\n")...)
	return out, nil
}

// ---- 上游事件解码（response.* 事件 ⇒ 内部帧）----

// respEventIn 是上游 response.* 事件的 data 载荷。
type respEventIn struct {
	Type           string          `json:"type"`
	SequenceNumber int64           `json:"sequence_number"`
	Delta          string          `json:"delta"`
	ItemID         string          `json:"item_id"`
	OutputIndex    int             `json:"output_index"`
	Response       *respResponseIn `json:"response"`
	Item           *respItemIn     `json:"item"`
}

// respResponseIn 是事件携带的 response 对象（completed / failed / incomplete）。
type respResponseIn struct {
	Status            string             `json:"status"`
	Usage             json.RawMessage    `json:"usage"`
	Error             *respErrorOut      `json:"error"`
	IncompleteDetails *respIncompleteOut `json:"incomplete_details"`
}

// respItemIn 是 output_item.added 携带的 item（取 function_call 的 call_id / name）。
type respItemIn struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ParseEvent 把上游 response.* 事件解码为内部帧（统一 SDD 3.7 / 4.3.1）：
//
//   - response.output_text.delta                ⇒ FrameTextDelta
//   - response.reasoning_text / summary delta   ⇒ FrameReasoningDelta
//   - response.function_call_arguments.delta    ⇒ FrameToolCallArgsDelta
//   - response.output_item.added（function_call）⇒ FrameToolCallArgsDelta（Text=函数名补名帧；
//     缺 added 直接 delta ⇒ 容忍，4.3.1 实测偏差）
//   - response.completed                       ⇒ FrameUsage + FrameDone（Status=六值原值）
//   - response.failed / incomplete              ⇒ FrameError + FrameDone
//   - data:[DONE] 字面量                         ⇒ FrameDone
//   - 未知事件类型                                ⇒ 忽略但不报错
//
// sequence_number 随帧上抛到 Frame.Seq（乱序只记不中断，⛔ 不因乱序报错）。
// data.type 优先于 event: 行（二者冗余，4.3.1：可任选其一解析）。
// usage 归一化走 usage.Normalize（4.4 单一口径真源）。
func ParseEvent(provider core.Provider, event stream.SSEEvent) ([]stream.Frame, error) {
	trim := bytes.TrimSpace([]byte(event.Data))
	if len(trim) == 0 {
		return nil, nil
	}
	if bytes.Equal(trim, []byte("[DONE]")) {
		return []stream.Frame{{Kind: stream.FrameDone}}, nil
	}
	var ev respEventIn
	if err := json.Unmarshal(trim, &ev); err != nil {
		return nil, fmt.Errorf("responses: 事件 data 不是合法 JSON: %w", err)
	}
	typ := ev.Type
	if typ == "" {
		typ = event.Event
	}
	seq := ev.SequenceNumber
	var frames []stream.Frame
	add := func(kind stream.FrameKind, fill func(*stream.Frame)) {
		fr := stream.Frame{Kind: kind, Seq: seq}
		fill(&fr)
		frames = append(frames, fr)
	}
	switch typ {
	case "response.output_text.delta":
		add(stream.FrameTextDelta, func(fr *stream.Frame) { fr.Text = ev.Delta })
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		add(stream.FrameReasoningDelta, func(fr *stream.Frame) { fr.Reasoning = ev.Delta })
	case "response.function_call_arguments.delta":
		add(stream.FrameToolCallArgsDelta, func(fr *stream.Frame) {
			fr.ToolCallID = ev.ItemID
			fr.Arguments = ev.Delta
		})
	case "response.output_item.added":
		// 只补 function_call 的工具名（Frame 无独立 name 槽 ⇒ 载于 Text）；
		// 其余 item 类型的 added 无帧语义，忽略。
		if ev.Item != nil && ev.Item.Type == "function_call" {
			add(stream.FrameToolCallArgsDelta, func(fr *stream.Frame) {
				fr.ToolCallID = ev.Item.CallID
				if fr.ToolCallID == "" {
					fr.ToolCallID = ev.Item.ID
				}
				fr.Text = ev.Item.Name
			})
		}
	case "response.completed":
		if ev.Response != nil && len(ev.Response.Usage) > 0 &&
			!bytes.Equal(bytes.TrimSpace(ev.Response.Usage), []byte("null")) {
			add(stream.FrameUsage, func(fr *stream.Frame) {
				fr.Usage = usage.Normalize(provider, core.APIShapeOpenAIResponses, ev.Response.Usage)
			})
		}
		status := core.StatusCompleted
		if ev.Response != nil && ev.Response.Status != "" {
			status = ev.Response.Status
		}
		add(stream.FrameDone, func(fr *stream.Frame) { fr.Status = status })
	case "response.failed":
		msg, code := "", ""
		if ev.Response != nil && ev.Response.Error != nil {
			msg = ev.Response.Error.Message
			code = ev.Response.Error.Code
		}
		add(stream.FrameError, func(fr *stream.Frame) { fr.Text = msg; fr.Status = code })
		add(stream.FrameDone, func(fr *stream.Frame) { fr.Status = core.StatusFailed })
	case "response.incomplete":
		reason := ""
		if ev.Response != nil && ev.Response.IncompleteDetails != nil {
			reason = ev.Response.IncompleteDetails.Reason
		}
		add(stream.FrameError, func(fr *stream.Frame) { fr.Text = reason; fr.Status = core.StatusIncomplete })
		add(stream.FrameDone, func(fr *stream.Frame) { fr.Status = core.StatusIncomplete })
	case "response.created", "response.in_progress", "response.queued":
		add(stream.FrameLifecycle, func(fr *stream.Frame) {
			fr.Status = strings.TrimPrefix(typ, "response.")
		})
	default:
		// 未知事件类型 ⇒ 忽略但不报错（4.3.1 容忍性）。
	}
	return frames, nil
}
