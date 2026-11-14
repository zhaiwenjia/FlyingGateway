// 日期: 2026-11-14 16:38:51.637
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// OpenAI 适配器（统一 SDD 4.3.1「统一格式的基准」）：
// 出站 Responses：POST {base}/responses（任务书「出站 Responses /v1/responses」）。
// AuthScheme: Authorization + "Bearer "；previous_response_id 支持 = true（3.5 表）。
// 请求体：input[] 异构 item、instructions 顶层、reasoning 对象、prompt_cache_key 透传；
// 响应体：output[] 异构联合（⛔ 不假设 output[0] 是 message）、encrypted_content 保留；
// 流式：SSE `event:` + `data:` 双字段、response.* 事件、sequence_number；usage 在 response.completed。
//
// 请求构造走 openai-go/v3 的 typed 参数（统一 SDD 3.6 / 10.3：响应系共用 openai-go/v3）：
// responses.ResponseNewParams 序列化出 Outbound.Body，BuildRequest / ParseResponse 的
// body []byte 签名不变（SDK 返回值先序列化再传入）。流式 HTTP 执行保留 streamRequest
//（Tier 2 兜底，依据见交付报告「每家 Tier」一节），流事件解析用 SDK 的
// responses.ResponseStreamEventUnion（SDK 值经 RawJSON 序列化后进既有归一）。

type openaiAdapter struct {
	hc *http.Client
}

// NewOpenAI 构造 OpenAI 适配器（hc 为出站 HTTP 客户端；测试传 httptest server 的 client）。
func NewOpenAI(hc *http.Client) Adapter { return &openaiAdapter{hc: hc} }

func (a *openaiAdapter) Provider() core.Provider          { return core.ProviderOpenAI }
func (a *openaiAdapter) OutboundShape() OutboundShape     { return ShapeResponses }
func (a *openaiAdapter) SupportsPreviousResponseID() bool { return true }

// AuthScheme 按 8.4：Authorization: Bearer <key>。
func (a *openaiAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "Authorization", Prefix: "Bearer "}
}

// ParseUsage 委托 usage.Normalize（4.4）：OpenAI 两种 usage 树并存（4.3.1：
// Chat 树 prompt_tokens_details.* / Responses 树 input_tokens_details.*）⇒ 按实际字段选口径。
func (a *openaiAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderOpenAI, usageTreeShape(raw), raw)
}

// ---- 请求体（Responses 形状，SDK typed 参数：responses.ResponseNewParams）----

// openAIInputItems 把规范消息拆成 Responses input item（4.2.3：Chat → Responses = 一条 message
// 拆多个 item；SDK union：responses.ResponseInputItemUnionParam）。
// system 消息不出 item，返回给调用方并入顶层 instructions（4.2.2）。
func openAIInputItems(m core.Message) (items []responses.ResponseInputItemUnionParam, sys string, err error) {
	if m.Role == "system" {
		return nil, contentText(m.Content), nil
	}
	if m.Role == "tool" {
		// 工具结果 ⇒ function_call_output（4.2.2：Responses 的工具结果是独立 item）。
		out := responses.ResponseInputItemFunctionCallOutputParam{}
		if m.ToolCallID != "" {
			out.CallID = param.NewOpt(m.ToolCallID)
		}
		if text := contentText(m.Content); text != "" {
			out.Output.OfString = param.NewOpt(text)
		}
		return []responses.ResponseInputItemUnionParam{{OfFunctionCallOutput: &out}}, "", nil
	}
	// 推理链回放：EncryptedContent 是语义字段（⛔ 必须保留，4.2.5）。
	if m.EncryptedContent != "" || m.ReasoningContent != "" {
		ri := responses.ResponseReasoningItemParam{}
		if m.EncryptedContent != "" {
			ri.EncryptedContent = param.NewOpt(m.EncryptedContent)
		}
		if m.ReasoningContent != "" {
			ri.Content = []responses.ResponseReasoningItemContentParam{{Text: m.ReasoningContent}}
		}
		// 旧 wire 的 reasoning item 不带 id（omitempty）；SDK 的必填 id 零值会序列化成
		// "id":"" ⇒ 用 param.Omit 删键，保持请求体逐字不因 SDK 而变（3.6）。
		ri.SetExtraFields(map[string]any{"id": param.Omit})
		items = append(items, responses.ResponseInputItemUnionParam{OfReasoning: &ri})
	}
	if m.Role == "assistant" {
		// assistant 文本 part 是 output_text、图片 part 是 input_image（4.2.3 逐字）。
		var parts []responses.ResponseOutputMessageContentUnionParam
		for _, p := range m.Content {
			switch {
			case p.ImageURL != "":
				part, err := openAIOutputImagePart(p.ImageURL)
				if err != nil {
					return nil, "", err
				}
				parts = append(parts, part)
			case p.Text != "":
				parts = append(parts, responses.ResponseOutputMessageContentUnionParam{
					OfOutputText: &responses.ResponseOutputTextParam{Text: p.Text},
				})
			}
		}
		if len(parts) > 0 {
			items = append(items, responses.ResponseInputItemUnionParam{
				OfOutputMessage: &responses.ResponseOutputMessageParam{Content: parts},
			})
		}
	} else {
		var parts responses.ResponseInputMessageContentListParam
		for _, p := range m.Content {
			switch {
			case p.ImageURL != "":
				parts = append(parts, responses.ResponseInputContentUnionParam{
					OfInputImage: &responses.ResponseInputImageParam{ImageURL: param.NewOpt(p.ImageURL)},
				})
			case p.Text != "":
				parts = append(parts, responses.ResponseInputContentUnionParam{
					OfInputText: &responses.ResponseInputTextParam{Text: p.Text},
				})
			}
		}
		if len(parts) > 0 {
			items = append(items, responses.ResponseInputItemUnionParam{
				OfInputMessage: &responses.ResponseInputItemMessageParam{Type: "message", Role: m.Role, Content: parts},
			})
		}
	}
	for _, tc := range m.ToolCalls {
		id := tc.CallID
		if id == "" {
			id = tc.ID
		}
		fc := responses.ResponseFunctionToolCallParam{CallID: id, Name: tc.Name, Arguments: tc.Arguments}
		if tc.ID != "" {
			fc.ID = param.NewOpt(tc.ID)
		}
		// 旧 wire 这三个字段是 omitempty（空值不出键）⇒ SDK 恒序列化的必填零值用 param.Omit 删键。
		ex := map[string]any{}
		if tc.Arguments == "" {
			ex["arguments"] = param.Omit
		}
		if tc.Name == "" {
			ex["name"] = param.Omit
		}
		if id == "" {
			ex["call_id"] = param.Omit
		}
		if len(ex) > 0 {
			fc.SetExtraFields(ex)
		}
		items = append(items, responses.ResponseInputItemUnionParam{OfFunctionCall: &fc})
	}
	return items, "", nil
}

// openAIOutputImagePart 把 assistant 消息的图片 part 装载进 output content union：
// 该 union 只有 output_text / refusal 变体，input_image 形状经 param.Override 跨 union 装载
// （SDK 公开通道），序列化仍由 SDK typed 参数（ResponseInputImageParam）完成、形状逐字不变。
func openAIOutputImagePart(imageURL string) (responses.ResponseOutputMessageContentUnionParam, error) {
	b, err := marshalBody(responses.ResponseInputImageParam{ImageURL: param.NewOpt(imageURL)})
	if err != nil {
		return responses.ResponseOutputMessageContentUnionParam{}, err
	}
	return param.Override[responses.ResponseOutputMessageContentUnionParam](json.RawMessage(b)), nil
}

// openAIToolChoiceParam 映射 tool_choice 的 SDK 形态（4.3.1：auto / none / required / 指定函数名）。
func openAIToolChoiceParam(tc *core.ToolChoice) responses.ResponseNewParamsToolChoiceUnion {
	var u responses.ResponseNewParamsToolChoiceUnion
	switch tc.Mode {
	case "function":
		if tc.Name != "" {
			u.OfFunctionTool = &responses.ToolChoiceFunctionParam{Name: tc.Name}
			return u
		}
		u.OfToolChoiceMode = param.NewOpt(responses.ToolChoiceOptionsAuto)
	case "none":
		u.OfToolChoiceMode = param.NewOpt(responses.ToolChoiceOptionsNone)
	case "required":
		u.OfToolChoiceMode = param.NewOpt(responses.ToolChoiceOptionsRequired)
	default:
		u.OfToolChoiceMode = param.NewOpt(responses.ToolChoiceOptionsAuto)
	}
	return u
}

// openAIInputExtras 回填旧 wire 里无 omitempty 的顶层键（model / input 恒在，input 无 item 时为
// null）：SDK 的 omitzero 会省略零值键 ⇒ 用 SetExtraFields 回填，请求体不因 SDK 而变（3.6）。
func openAIInputExtras(ex map[string]any, res *core.Resolution, itemCount int) {
	if res.Model == "" {
		ex["model"] = ""
	}
	if itemCount == 0 {
		ex["input"] = nil
	}
}

// BuildRequest 用 SDK typed 参数构造 /responses 请求（4.3.1 逐字字段；4.2.2 系统提示走顶层
// instructions；3.5：prompt_cache_key / previous_response_id 始终透传）。
func (a *openaiAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(true, r); err != nil {
		return nil, err
	}
	p := responses.ResponseNewParams{Model: shared.ResponsesModel(res.Model)}
	if r.MaxOutputTokens > 0 {
		p.MaxOutputTokens = param.NewOpt(int64(r.MaxOutputTokens))
	}
	if r.PromptCacheKey != "" {
		p.PromptCacheKey = param.NewOpt(r.PromptCacheKey)
	}
	if r.PreviousResponseID != "" {
		p.PreviousResponseID = param.NewOpt(r.PreviousResponseID)
	}
	ex := map[string]any{}
	if r.Stream {
		// ResponseNewParams 无 stream 字段（SDK 在执行期以 WithJSONSet 注入）⇒ 序列化自建时走
		// extras；旧 wire 为 stream:true / 省略（omitempty）⇒ 只在 true 时注入，零值不补。
		ex["stream"] = true
	}
	if r.Reasoning != nil {
		// reasoning 对象（4.3.1：context / effort / mode / summary）。
		p.Reasoning = shared.ReasoningParam{
			Effort:  shared.ReasoningEffort(r.Reasoning.Effort),
			Summary: shared.ReasoningSummary(r.Reasoning.Summary),
			Context: shared.ReasoningContext(r.Reasoning.Context),
			Mode:    shared.ReasoningMode(r.Reasoning.Mode),
		}
		if r.Reasoning.Effort == "" && r.Reasoning.Summary == "" &&
			r.Reasoning.Context == "" && r.Reasoning.Mode == "" {
			// 旧 wire：reasoning 指针非 nil 即发对象（可为空 {}）；SDK omitzero 会整个省略 ⇒ 回填。
			ex["reasoning"] = map[string]any{}
		}
	}
	instructions := r.Instructions
	var input []responses.ResponseInputItemUnionParam
	for _, m := range r.Messages {
		items, sys, err := openAIInputItems(m)
		if err != nil {
			return nil, err
		}
		if sys != "" {
			if instructions == "" {
				instructions = sys
			} else {
				instructions += "\n" + sys
			}
			continue
		}
		input = append(input, items...)
	}
	if instructions != "" {
		p.Instructions = param.NewOpt(instructions)
	}
	p.Input.OfInputItemList = input
	for _, t := range r.Tools {
		ft := responses.FunctionToolParam{Name: t.Name}
		if t.Description != "" {
			ft.Description = param.NewOpt(t.Description)
		}
		if len(t.Parameters) > 0 {
			// parameters 是调用方给定的 JSON Schema ⇒ 原文透传（⛔ 不重整键序）。
			ft.SetExtraFields(map[string]any{"parameters": json.RawMessage(t.Parameters)})
		}
		p.Tools = append(p.Tools, responses.ToolUnionParam{OfFunction: &ft})
	}
	if r.ToolChoice != nil {
		p.ToolChoice = openAIToolChoiceParam(r.ToolChoice)
	}
	openAIInputExtras(ex, res, len(input))
	if len(ex) > 0 {
		p.SetExtraFields(ex)
	}
	raw, err := marshalBody(p)
	if err != nil {
		return nil, err
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/responses"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, nil
}

// toolChoiceResponses 映射 tool_choice（4.3.1：auto / none / required / 指定函数名）。
// ⚠️ 供 qwen / xai 适配器沿用的旧形态映射（⛔ 不改签名）；openai 自身走 openAIToolChoiceParam。
func toolChoiceResponses(tc *core.ToolChoice) any {
	switch tc.Mode {
	case "function":
		if tc.Name != "" {
			return map[string]any{"type": "function", "name": tc.Name}
		}
		return "auto"
	case "none", "required", "auto":
		return tc.Mode
	default:
		return "auto"
	}
}

// ---- 响应体（4.3.1：output[] 异构联合，⛔ 不假设 output[0] 是 message）----

type openAIResp struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"`
	Model             string             `json:"model"`
	Output            []openAIOutputItem `json:"output"`
	OutputText        string             `json:"output_text"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Usage json.RawMessage `json:"usage"`
	// FinishReason 是 DeepSeek / 智谱 / MiMo 等兼容端点回落的 chat 语义字段（4.5.2 非标准取值 ⇒ 超集原值）。
	FinishReason string `json:"finish_reason"`
}

// openAIOutputItem 是 output[] 的异构 item（4.3.1 item 内部结构）。
type openAIOutputItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Role    string `json:"role"`
	Status  string `json:"status"`
	Content []struct {
		Type    string `json:"type"` // output_text / refusal / reasoning_text / summary_text
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
	// EncryptedContent 是 reasoning item 的加密推理链（⛔ 必须保留，4.2.5）。
	EncryptedContent string `json:"encrypted_content"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	// Refusal 是 message item 级的拒绝文本（4.3.1：规范形态是 content 里的 {type:"refusal", refusal} 项；
	// 部分兼容端点把 refusal 直接放 item 级 ⇒ 两级都收，归一进 core.OutputItem.Refusal）。
	Refusal string `json:"refusal"`
	// ReasoningContent 是 Moonshot 等兼容端点把思考放 message.reasoning_content 的扩展（4.3.9）。
	ReasoningContent string `json:"reasoning_content"`
}

// ParseResponse 解析 response 对象 → 规范 Response（4.3.1 响应体关键字段）。
// 先经 SDK typed 解析（统一 SDD 3.6：SDK 返回值先序列化再传入）——response.Response 结构化
// 解析后用 RawJSON 原样送进既有归一逻辑 ⇒ finish_reason / reasoning_content 等兼容端点扩展
// 随原始字节无损进入，⛔ 不因 SDK 类型裁剪而丢。
func (a *openaiAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var sr responses.Response
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	var r openAIResp
	if err := json.Unmarshal([]byte(sr.RawJSON()), &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{
		ID:     r.ID,
		Model:  r.Model,
		Status: r.Status,
	}
	hasToolCall := false
	for _, it := range r.Output {
		out.OutputItems = append(out.OutputItems, mapOutputItem(&it))
		if it.Type == "function_call" {
			hasToolCall = true
		}
	}
	// output_text 是便捷字段（4.3.1）；缺失时按 message 的 output_text 累加。
	out.Text = r.OutputText
	if out.Text == "" {
		var b strings.Builder
		for _, it := range r.Output {
			if it.Type != "message" {
				continue
			}
			for _, p := range it.Content {
				if p.Type == "output_text" {
					b.WriteString(p.Text)
				}
			}
		}
		out.Text = b.String()
	}
	out.FinishReason = deriveResponsesFinish(r.FinishReason, r.Status, incompleteReason(r.IncompleteDetails), hasToolCall)
	if len(r.Usage) > 0 {
		out.Usage = a.ParseUsage(r.Usage)
	}
	if r.Error != nil {
		out.Status = core.StatusFailed
		out.Error = gwerrors.New(core.ErrUpstreamError, 0, r.Error.Code, r.Error.Message)
	}
	return out, nil
}

// mapOutputItem 把异构 output item 映射到规范 OutputItem（type / id 保留原值）。
// 文本 part 归一到 ContentPart.Type="text"；refusal 收进 OutputItem.Refusal。
func mapOutputItem(it *openAIOutputItem) core.OutputItem {
	oi := core.OutputItem{
		Type:             it.Type,
		ID:               it.ID,
		Role:             it.Role,
		EncryptedContent: it.EncryptedContent,
		CallID:           it.CallID,
		Name:             it.Name,
		Arguments:        it.Arguments,
		ReasoningText:    it.ReasoningContent,
	}
	for _, p := range it.Content {
		switch p.Type {
		case "refusal":
			oi.Refusal += p.Refusal
		case "output_text", "text":
			oi.Content = append(oi.Content, core.ContentPart{Type: "text", Text: p.Text})
		case "reasoning_text":
			oi.ReasoningText += p.Text
		}
	}
	for _, s := range it.Summary {
		oi.Summary = append(oi.Summary, core.SummaryPart{Type: s.Type, Text: s.Text})
	}
	if it.Refusal != "" {
		oi.Refusal += it.Refusal
	}
	return oi
}

// incompleteReason 取 incomplete_details.reason（4.3.1：max_output_tokens / max_messages / content_filter / steered）。
func incompleteReason(d *struct {
	Reason string `json:"reason"`
}) string {
	if d == nil {
		return ""
	}
	return d.Reason
}

// deriveResponsesFinish 按 4.5.2 OpenAI Responses 行推导终止原因：
// Responses 用 status 六值体系；显式 finish_reason（兼容端点回落字段）⇒ 超集原值优先；
// incomplete ⇒ incomplete_details.reason 归「超上下文 / length」类（4.5.1 归一化映射规则）。
// ⚠️ 供 qwen / xai 适配器沿用（⛔ 不改签名）。
func deriveResponsesFinish(explicit, status, incomplete string, hasToolCall bool) string {
	if explicit != "" {
		return explicit // 4.5.1：非标准取值 ⛔ 不得丢弃 ⇒ 超集原值
	}
	switch status {
	case core.StatusCompleted:
		if hasToolCall {
			return core.FinishToolCalls
		}
		return core.FinishStop
	case core.StatusIncomplete:
		switch incomplete {
		case "max_output_tokens", "max_messages":
			return core.FinishLength
		case "content_filter":
			return core.FinishContentFilter
		case "":
			return core.FinishUnknown
		default:
			return incomplete // steered 等 ⇒ 超集原值
		}
	case core.StatusFailed, core.StatusCancelled, core.StatusInProgress, core.StatusQueued:
		return core.FinishUnknown
	default:
		return core.FinishUnknown
	}
}

// ---- 流式（4.3.1 Responses 流式事件形态：event: + data: 双字段、sequence_number、59 种事件）----

// responseEventExtra 从 response.* 事件原始 JSON 取 SDK union 未逐字建模的嵌套字段：
// response.usage 原文（含兼容端点扩展字段，⛔ 不经 SDK 类型重整）与 response.error 的存在性
// （旧口径按 error 指针判存在）。事件 union 的 RawJSON 即 SDK 值的序列化形态（3.6 口径）。
type responseEventExtra struct {
	Response struct {
		Usage json.RawMessage `json:"usage"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
}

// rawPartialJSON 取流事件原始 JSON 的 partial_json（函数参数增量，4.3.1 逐字字段名）：
// SDK 的事件 union 未建模该字段（其 delta 命名不同）⇒ 只能从原始 JSON 取原值。
func rawPartialJSON(eventRaw string) (string, bool) {
	var q struct {
		PartialJSON string `json:"partial_json"`
	}
	// eventRaw 是已成功反序列化过的 JSON 对象，此处仅取一个 string 字段；
	// 字段类型非 string（畸形上游）⇒ 返回 false，按旧口径整帧容忍跳过。
	if err := json.Unmarshal([]byte(eventRaw), &q); err != nil {
		return "", false
	}
	return q.PartialJSON, true
}

// openAIStreamDecoder 是 OpenAI 的帧解码器（3.7：每家一个帧解码器）。
// 容忍：缺 output_item.added（gpt-5.3-codex 实测，4.3.1）、缺 [DONE]、未知事件、乱序。
// 流事件解析用 SDK 的 responses.ResponseStreamEventUnion（typed 字段 + RawJSON 取扩展字段）。
type openAIStreamDecoder struct {
	usageFn func(json.RawMessage) *core.Usage
	toolIDs map[string]string // item_id → call_id（缺 output_item.added 时退回 item_id）
}

func (a *openaiAdapter) streamDecoder() *openAIStreamDecoder {
	return &openAIStreamDecoder{usageFn: a.ParseUsage, toolIDs: map[string]string{}}
}

func (d *openAIStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	if data == "[DONE]" {
		// 终止哨兵（部分后端省略 ⇒ 不能只靠它判结束，4.3.1）。
		return nil, true, nil
	}
	var e responses.ResponseStreamEventUnion
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return nil, false, nil // 非 JSON 帧（keep-alive 等）容忍跳过
	}
	etype := e.Type
	if etype == "" {
		etype = ev.Event // data 内 type 与 event 名冗余，任选其一解析（4.3.1）
	}
	seq := e.SequenceNumber
	switch etype {
	case "response.created", "response.in_progress", "response.queued":
		return []stream.Frame{{Kind: stream.FrameLifecycle, Status: etype, Seq: seq}}, false, nil
	case "response.output_item.added":
		if e.Item.Type == "function_call" && e.ItemID != "" {
			id := e.Item.CallID
			if id == "" {
				id = e.ItemID
			}
			d.toolIDs[e.ItemID] = id
			// 补名帧（4.3.1：output_item.added 的 item.name 载工具函数名，后续
			// function_call_arguments.delta 不再带名）⇒ 名字必须在此进帧；
			// 帧承载约定：FrameToolCallArgsDelta 的 Text 载函数名、ToolCallID 载调用 id。
			// name 为空不发帧，保持原行为。
			if e.Item.Name != "" {
				return []stream.Frame{{Kind: stream.FrameToolCallArgsDelta, ToolCallID: id, Text: e.Item.Name, Seq: seq}}, false, nil
			}
		}
		return nil, false, nil
	case "response.output_text.delta":
		return []stream.Frame{{Kind: stream.FrameTextDelta, Text: e.Delta, Seq: seq}}, false, nil
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		return []stream.Frame{{Kind: stream.FrameReasoningDelta, Reasoning: e.Delta, Seq: seq}}, false, nil
	case "response.function_call_arguments.delta":
		id := d.toolIDs[e.ItemID]
		if id == "" {
			id = e.ItemID // 缺 output_item.added 时容忍：直接用 item_id（4.3.1 解析器容忍性）
		}
		args, ok := rawPartialJSON(e.RawJSON())
		if !ok {
			return nil, false, nil // partial_json 畸形（非 string）⇒ 按旧口径整帧跳过
		}
		return []stream.Frame{{Kind: stream.FrameToolCallArgsDelta, ToolCallID: id, Arguments: args, Seq: seq}}, false, nil
	case "response.completed", "response.incomplete":
		status := core.StatusCompleted
		if etype == "response.incomplete" {
			status = core.StatusIncomplete
		}
		frames := []stream.Frame{{Kind: stream.FrameDone, Status: status, Seq: seq}}
		var ex responseEventExtra
		_ = json.Unmarshal([]byte(e.RawJSON()), &ex) // RawJSON 是已解析过的 JSON，仅取嵌套字段
		if len(ex.Response.Usage) > 0 {
			// usage 由 response.completed 自带（⛔ 不需要 stream_options.include_usage，4.3.1）。
			frames = append([]stream.Frame{{Kind: stream.FrameUsage, Usage: d.usageFn(ex.Response.Usage), Seq: seq}}, frames...)
		}
		return frames, true, nil
	case "response.failed":
		msg := "上游流式响应失败"
		code := "response_failed"
		var ex responseEventExtra
		_ = json.Unmarshal([]byte(e.RawJSON()), &ex)
		if ex.Response.Error != nil {
			msg, code = ex.Response.Error.Message, ex.Response.Error.Code
		}
		return []stream.Frame{{Kind: stream.FrameError, Status: code, Text: msg, Seq: seq}}, true, nil
	default:
		// 未知事件类型 ⇒ 忽略（3.7 流式容错；事件总数 59 种，⛔ 不假设全部认识）。
		return nil, false, nil
	}
}

// StreamStart：BuildRequest（强制 stream=true，3.7 上游传输恒为流式）⇒ 真实 HTTP ⇒ SSE 解码。
func (a *openaiAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
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
	return newSSEIter(ctx, resp.Body, a.streamDecoder().decode, nil), nil
}
