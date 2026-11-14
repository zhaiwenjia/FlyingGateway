// 日期: 2026-11-14 16:08:29.868
// 作者: Zhai Wenjia

package responses

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// ---- 入站解码（统一 SDD 3.4 / 11.2）----

// TestDecodeRequest_StringInput 钉住 input 为字符串 ⇒ Messages[0]{Role:"user"}（3.4）。
func TestDecodeRequest_StringInput(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","input":"你好"}`))
	if err != nil {
		t.Fatalf("DecodeRequest 报错: %v", err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("应为 Messages[0]{Role:user}，实得 %+v", req.Messages)
	}
	if got := req.Messages[0].Content[0].Text; got != "你好" {
		t.Errorf("正文应为 你好，实得 %q", got)
	}
}

// TestDecodeRequest_ItemMapping 钉住 input item 数组逐项映射（统一 SDD 3.4）：
// message⇒Message、function_call⇒Message{ToolCalls}、function_call_output⇒
// Message{Role:"tool", ToolCallID}、reasoning⇒Message{ReasoningContent, EncryptedContent}。
func TestDecodeRequest_ItemMapping(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"查天气"}]},
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"先想"}],
		 "content":[{"type":"reasoning_text","text":"推理正文"}],"encrypted_content":"enc-XYZ"},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"晴"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"晴"}]}
	]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest 报错: %v", err)
	}
	if len(req.Messages) != 5 {
		t.Fatalf("Messages 数应为 5，实得 %d", len(req.Messages))
	}
	// message item：类型逐字保留（input_text）。
	if p := req.Messages[0].Content[0]; p.Type != "input_text" || p.Text != "查天气" {
		t.Errorf("message item content 映射错: %+v", p)
	}
	// reasoning item：encrypted_content 必须保留（⛔ 丢弃，4.2.5）。
	rs := req.Messages[1]
	if rs.EncryptedContent != "enc-XYZ" {
		t.Errorf("encrypted_content 应保留为 enc-XYZ，实得 %q", rs.EncryptedContent)
	}
	if rs.ReasoningContent != "推理正文" {
		t.Errorf("ReasoningContent 应取 content[] 正文，实得 %q", rs.ReasoningContent)
	}
	// function_call item ⇒ Message{ToolCalls}（id / call_id 分列）。
	fc := req.Messages[2]
	if len(fc.ToolCalls) != 1 {
		t.Fatalf("function_call 应映射为 1 个 ToolCall，实得 %+v", fc)
	}
	if fc.ToolCalls[0].ID != "fc_1" || fc.ToolCalls[0].CallID != "call_1" ||
		fc.ToolCalls[0].Name != "get_weather" || fc.ToolCalls[0].Arguments != `{"city":"北京"}` {
		t.Errorf("ToolCall 映射错: %+v", fc.ToolCalls[0])
	}
	// function_call_output item ⇒ Message{Role:"tool", ToolCallID}。
	fo := req.Messages[3]
	if fo.Role != "tool" || fo.ToolCallID != "call_1" || fo.Content[0].Text != "晴" {
		t.Errorf("function_call_output 映射错: %+v", fo)
	}
}

// TestDecodeRequest_ReasoningItemSummaryFallback 钉住 reasoning item 无 content[] 时
// 退回 summary[]（文本不丢）。
func TestDecodeRequest_ReasoningItemSummaryFallback(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"摘要甲"},{"type":"summary_text","text":"摘要乙"}]}
	]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest 报错: %v", err)
	}
	if got := req.Messages[0].ReasoningContent; got != "摘要甲\n摘要乙" {
		t.Errorf("ReasoningContent 应为 summary 拼接，实得 %q", got)
	}
}

// TestDecodeRequest_TopLevelFields 钉住 instructions ⇒ core.Instructions（⛔ 不并进
// messages）与 previous_response_id / conversation / prompt_cache_key ⇒ core 对应字段。
func TestDecodeRequest_TopLevelFields(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","instructions":"系统提示",
		"previous_response_id":"resp_1","conversation":{"id":"conv_1"},"prompt_cache_key":"pk-1",
		"max_output_tokens":512,"stream":true}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest 报错: %v", err)
	}
	if req.Instructions != "系统提示" {
		t.Errorf("Instructions 应为 系统提示，实得 %q", req.Instructions)
	}
	if len(req.Messages) != 1 {
		t.Errorf("instructions 不应并进 messages，实得 %+v", req.Messages)
	}
	if req.PreviousResponseID != "resp_1" || req.Conversation != "conv_1" || req.PromptCacheKey != "pk-1" {
		t.Errorf("会话态字段映射错: %+v", req)
	}
	if req.MaxOutputTokens != 512 || !req.Stream {
		t.Errorf("max_output_tokens/stream 映射错: %+v", req)
	}
}

// TestDecodeRequest_ReasoningConfig 钉住 reasoning{effort/summary/context/mode} ⇒
// core.ReasoningConfig 四字段（4.3.1 reasoning 对象）。
func TestDecodeRequest_ReasoningConfig(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","reasoning":
		{"effort":"high","summary":"auto","context":"current_turn","mode":"pro"}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest 报错: %v", err)
	}
	r := req.Reasoning
	if r == nil || r.Effort != "high" || r.Summary != "auto" || r.Context != "current_turn" || r.Mode != "pro" {
		t.Errorf("ReasoningConfig 映射错: %+v", r)
	}
}

// TestDecodeRequest_UnknownItemTypeTolerated 钉住未知 item 类型容忍跳过、不报错
// （4.3.1 输出联合 29 种，解析器容忍性）。
func TestDecodeRequest_UnknownItemTypeTolerated(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"web_search_call","id":"ws_1"},
		{"type":"message","role":"user","content":"hi"}
	]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("未知 item 类型不应报错: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Errorf("未知 item 应跳过，实得 %+v", req.Messages)
	}
}

// TestDecodeRequest_InvalidJSON 钉住非法 JSON 显式报错。
func TestDecodeRequest_InvalidJSON(t *testing.T) {
	if _, err := DecodeRequest([]byte(`{"input":`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// ---- 出站编码（统一 SDD 3.4 / 11.2）----

// decodeJSON 把 JSON 字节解到通用 map（断言 wire 字段名逐字命中）。
func decodeJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("JSON 解析失败: %v\n原文: %s", err, b)
	}
	return m
}

// TestEncodeResponse_ObjectStatusAndOutputItems 钉住 response 对象（object="response"、
// status 六值原值、output[] 异构联合逐项形状，4.3.1 item 内部结构）。
func TestEncodeResponse_ObjectStatusAndOutputItems(t *testing.T) {
	resp := &core.Response{
		ID:     "resp_1",
		Model:  "m",
		Status: core.StatusIncomplete, // 六值原值，⛔ 不改写
		OutputItems: []core.OutputItem{
			{Type: "message", ID: "msg_1", Role: "assistant",
				Content: []core.ContentPart{{Type: "output_text", Text: "你好"}},
				Refusal: ""},
			{Type: "reasoning", ID: "rs_1",
				Summary:          []core.SummaryPart{{Type: "summary_text", Text: "摘要"}},
				ReasoningText:    "推理正文",
				EncryptedContent: "enc-XYZ"},
			{Type: "function_call", ID: "fc_1", CallID: "call_1",
				Name: "get_weather", Arguments: `{"city":"北京"}`},
		},
	}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 报错: %v", err)
	}
	m := decodeJSON(t, b)
	if m["object"] != "response" {
		t.Errorf("object 应为 response，实得 %v", m["object"])
	}
	if m["status"] != "incomplete" {
		t.Errorf("status 应原值透传 incomplete，实得 %v", m["status"])
	}
	out, ok := m["output"].([]any)
	if !ok || len(out) != 3 {
		t.Fatalf("output 应恰有 3 项，实得 %v", m["output"])
	}
	msg := out[0].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" || msg["id"] != "msg_1" {
		t.Errorf("message item 头字段错: %v", msg)
	}
	part := msg["content"].([]any)[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "你好" {
		t.Errorf("message content 错: %v", part)
	}
	rs := out[1].(map[string]any)
	if rs["type"] != "reasoning" {
		t.Errorf("reasoning item type 错: %v", rs)
	}
	if s := rs["summary"].([]any)[0].(map[string]any); s["type"] != "summary_text" || s["text"] != "摘要" {
		t.Errorf("summary[] 错: %v", s)
	}
	if c := rs["content"].([]any)[0].(map[string]any); c["type"] != "reasoning_text" || c["text"] != "推理正文" {
		t.Errorf("content[] 错: %v", c)
	}
	if rs["encrypted_content"] != "enc-XYZ" {
		t.Errorf("encrypted_content 应保留为 enc-XYZ，实得 %v", rs["encrypted_content"])
	}
	fc := out[2].(map[string]any)
	if fc["type"] != "function_call" || fc["id"] != "fc_1" || fc["call_id"] != "call_1" ||
		fc["name"] != "get_weather" || fc["arguments"] != `{"city":"北京"}` {
		t.Errorf("function_call item 错: %v", fc)
	}
}

// TestEncodeResponse_OutputTextRefusalAndUsage 钉住 output_text 便捷字段、refusal part
// 与 usage 的 Responses 字段名（input_tokens / output_tokens / input_tokens_details.*）。
func TestEncodeResponse_OutputTextRefusalAndUsage(t *testing.T) {
	resp := &core.Response{
		Model:  "m",
		Status: core.StatusCompleted,
		OutputItems: []core.OutputItem{{
			Type:    "message",
			Role:    "assistant",
			Content: []core.ContentPart{{Type: "output_text", Text: "答复"}},
			Refusal: "拒绝回答",
		}},
		Usage: &core.Usage{
			InputTokens:      125,
			CacheReadTokens:  98,
			CacheWriteTokens: 10,
			OutputTokens:     48,
			TotalTokens:      173,
			ReasoningTokens:  7,
		},
	}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 报错: %v", err)
	}
	m := decodeJSON(t, b)
	if m["output_text"] != "答复" {
		t.Errorf("output_text 应为 答复，实得 %v", m["output_text"])
	}
	content := m["output"].([]any)[0].(map[string]any)["content"].([]any)
	last := content[len(content)-1].(map[string]any)
	if last["type"] != "refusal" || last["refusal"] != "拒绝回答" {
		t.Errorf("refusal part 错: %v", last)
	}
	u := m["usage"].(map[string]any)
	if u["input_tokens"] != float64(125) || u["output_tokens"] != float64(48) || u["total_tokens"] != float64(173) {
		t.Errorf("usage 顶层字段错: %v", u)
	}
	id := u["input_tokens_details"].(map[string]any)
	if id["cached_tokens"] != float64(98) || id["cache_write_tokens"] != float64(10) {
		t.Errorf("input_tokens_details 错: %v", id)
	}
	od := u["output_tokens_details"].(map[string]any)
	if od["reasoning_tokens"] != float64(7) {
		t.Errorf("output_tokens_details 错: %v", od)
	}
}

// TestResponsesSignatureCarriedLossless 钉住 Responses 出口承载 thinking 签名
// （无损 ⇒ ⛔ 不标记、⛔ 不压平）：EncodeResponse 的 thinking/reasoning item 落
// item.signature；EventEncoder 的签名帧（FrameReasoningDelta 且 ThinkingSignature
// 非空，承载约定见 stream.FrameReasoningDelta）累积进终态 item.signature、不发增量事件；
// 签名与 encrypted_content 均被承载 ⇒ 不出现 x_gateway.degradations[]。
// 错误实现会红：签名被丢 / 只进内部不进 item.signature（旧行为把 thinking_signature
// 当丢失标记）⇒ item.signature 断言红；承载了还误标 degradation ⇒ x_gateway 缺席断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/codec/responses/ -run TestResponsesSignatureCarriedLossless -v
func TestResponsesSignatureCarriedLossless(t *testing.T) {
	// ---- 非流式：EncodeResponse ----
	resp := &core.Response{
		Model: "m",
		OutputItems: []core.OutputItem{
			{Type: "thinking", ReasoningText: "想", ThinkingSignature: "sig-abc"},
			{Type: "reasoning", EncryptedContent: "enc-XYZ"},
			{Type: "message", Content: []core.ContentPart{{Type: "output_text", Text: "答"}}},
		},
	}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 报错: %v", err)
	}
	m := decodeJSON(t, b)
	out0 := m["output"].([]any)[0].(map[string]any)
	if out0["signature"] != "sig-abc" {
		t.Errorf("item.signature 应为 sig-abc（Responses 形状承载签名 ⇒ 无损），实得 %v", out0)
	}
	if _, ok := m["x_gateway"]; ok {
		t.Errorf("签名与 encrypted_content 均被承载 ⇒ 不应有 x_gateway.degradations[]: %s", b)
	}

	// ---- 流式：EventEncoder（签名帧累积进终态 item.signature）----
	e := NewEventEncoder("m")
	if _, err := e.Encode(stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: "想"}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	bSig, err := e.Encode(stream.Frame{Kind: stream.FrameReasoningDelta, ThinkingSignature: "sig-abc"})
	if err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	if bSig != nil {
		t.Errorf("签名帧不应发增量事件（只累积进终态 item.signature），实得 %s", bSig)
	}
	if _, err := e.Encode(stream.Frame{Kind: stream.FrameDone, Status: core.StatusCompleted}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	out, err := e.Finish()
	if err != nil {
		t.Fatalf("Finish 报错: %v", err)
	}
	evs := sseEvents(t, out)
	term := decodeJSON(t, []byte(evs[0].Payload))
	item := term["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
	if item["signature"] != "sig-abc" {
		t.Errorf("终态 item.signature 应为 sig-abc，实得 %v", item)
	}
}

// TestEncodeResponse_RespDegradationsPassthrough 钉住 resp.Degradations 透传进
// x_gateway.degradations[]。
func TestEncodeResponse_RespDegradationsPassthrough(t *testing.T) {
	resp := &core.Response{
		Model: "m",
		Degradations: []core.Degradation{{
			Field:  "previous_response_id",
			Reason: core.DegradationCapabilityUnsupported,
			Detail: "上游不支持服务端会话态",
		}},
	}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 报错: %v", err)
	}
	degs := decodeJSON(t, b)["x_gateway"].(map[string]any)["degradations"].([]any)
	d := degs[0].(map[string]any)
	if d["field"] != "previous_response_id" || d["reason"] != "capability_unsupported" {
		t.Errorf("Degradations 透传错: %v", d)
	}
}

// ---- 客户端侧事件编码器 EventEncoder（统一 SDD 4.3.1 事件形态）----

// sseEvent 事件名 + data 载荷。
type sseEvent struct {
	Name    string
	Payload string
}

// sseEvents 拆 `event:` + `data:` 双字段事件流。
func sseEvents(t *testing.T, out []byte) []sseEvent {
	t.Helper()
	var evs []sseEvent
	for _, block := range strings.Split(strings.TrimRight(string(out), "\n"), "\n\n") {
		if block == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			name, val, ok := strings.Cut(line, ": ")
			if !ok {
				t.Fatalf("事件行应为 field: value 形态，实得 %q", line)
			}
			switch name {
			case "event":
				ev.Name = val
			case "data":
				ev.Payload = val
			default:
				t.Fatalf("未知事件字段 %q", name)
			}
		}
		evs = append(evs, ev)
	}
	return evs
}

// TestEventEncoder_EventNameMatchesDataTypeAndSeqMonotonic 钉住（统一 SDD 11.2）：
// 事件名与 data.type 一致；sequence_number 单调递增。
func TestEventEncoder_EventNameMatchesDataTypeAndSeqMonotonic(t *testing.T) {
	e := NewEventEncoder("m")
	var out []byte
	b1, err := e.Encode(stream.Frame{Kind: stream.FrameTextDelta, Text: "你"})
	if err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	b2, _ := e.Encode(stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: "想"})
	b3, _ := e.Encode(stream.Frame{
		Kind: stream.FrameToolCallArgsDelta, ToolCallID: "call_1", Text: "f", Arguments: `{"a":`,
	})
	out = append(out, b1...)
	out = append(out, b2...)
	out = append(out, b3...)
	evs := sseEvents(t, out)
	if len(evs) != 3 {
		t.Fatalf("应恰有 3 个事件，实得 %d", len(evs))
	}
	var lastSeq float64 = -1
	for i, ev := range evs {
		m := decodeJSON(t, []byte(ev.Payload))
		if m["type"] != ev.Name {
			t.Errorf("事件 %d 名与 data.type 不一致: %q vs %v", i, ev.Name, m["type"])
		}
		seq, ok := m["sequence_number"].(float64)
		if !ok {
			t.Fatalf("事件 %d 缺 sequence_number: %s", i, ev.Payload)
		}
		if seq <= lastSeq {
			t.Errorf("sequence_number 应严格递增: %v → %v", lastSeq, seq)
		}
		lastSeq = seq
	}
	if evs[0].Name != "response.output_text.delta" ||
		evs[1].Name != "response.reasoning_text.delta" ||
		evs[2].Name != "response.function_call_arguments.delta" {
		t.Errorf("事件名错: %v", evs)
	}
}

// TestEventEncoder_FinishCompleted 钉住终止形态：恰好一个终态事件（response.completed，
// 携带完整 response 对象含 usage 与 output[]）后接 data: [DONE]（4.3.1 终止口径）。
func TestEventEncoder_FinishCompleted(t *testing.T) {
	e := NewEventEncoder("m")
	if _, err := e.Encode(stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: "想"}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	if _, err := e.Encode(stream.Frame{Kind: stream.FrameTextDelta, Text: "答"}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	if _, err := e.Encode(stream.Frame{
		Kind: stream.FrameToolCallArgsDelta, ToolCallID: "call_1", Text: "get_weather", Arguments: `{}`,
	}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	if _, err := e.Encode(stream.Frame{
		Kind:   stream.FrameDone,
		Status: core.StatusCompleted,
		Usage:  &core.Usage{InputTokens: 125, OutputTokens: 48, TotalTokens: 173},
	}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	e.AddDegradation(core.Degradation{
		Field:  "previous_response_id",
		Reason: core.DegradationCapabilityUnsupported,
		Detail: "上游不支持服务端会话态",
	})
	out, err := e.Finish()
	if err != nil {
		t.Fatalf("Finish 报错: %v", err)
	}
	evs := sseEvents(t, out)
	if len(evs) != 2 {
		t.Fatalf("Finish 应产出 终态事件 + [DONE] 共 2 条，实得 %d：%+v", len(evs), evs)
	}
	if evs[1].Name != "" || evs[1].Payload != "[DONE]" {
		t.Errorf("末条应为 data: [DONE]，实得 %+v", evs[1])
	}
	if evs[0].Name != "response.completed" {
		t.Errorf("终态事件应为 response.completed，实得 %q", evs[0].Name)
	}
	m := decodeJSON(t, []byte(evs[0].Payload))
	resp := m["response"].(map[string]any)
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("终态 response 头字段错: %v", resp)
	}
	if resp["output_text"] != "答" {
		t.Errorf("output_text 应为 答，实得 %v", resp["output_text"])
	}
	out0 := resp["output"].([]any)
	if len(out0) != 3 {
		t.Fatalf("output 应有 reasoning+message+function_call 共 3 项，实得 %d", len(out0))
	}
	fc := out0[2].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "get_weather" || fc["arguments"] != "{}" {
		t.Errorf("终态 function_call item 错: %v", fc)
	}
	u := resp["usage"].(map[string]any)
	if u["input_tokens"] != float64(125) || u["output_tokens"] != float64(48) {
		t.Errorf("终态 usage 错: %v", u)
	}
	xg := resp["x_gateway"].(map[string]any)
	if xg["degradations"].([]any)[0].(map[string]any)["field"] != "previous_response_id" {
		t.Errorf("终态应带 x_gateway.degradations[]（AddDegradation 透传），实得 %v", xg)
	}
}

// TestEventEncoder_FinishFailedAndIncomplete 钉住 failed / incomplete 两个终态：
// FrameError ⇒ response.failed（带 error 对象）；Done(Status=incomplete) ⇒
// response.incomplete（带 incomplete_details.reason）。
func TestEventEncoder_FinishFailedAndIncomplete(t *testing.T) {
	// failed
	e1 := NewEventEncoder("m")
	if _, err := e1.Encode(stream.Frame{Kind: stream.FrameError, Text: "上游炸了", Status: "upstream_error"}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	out1, err := e1.Finish()
	if err != nil {
		t.Fatalf("Finish 报错: %v", err)
	}
	evs1 := sseEvents(t, out1)
	if evs1[0].Name != "response.failed" {
		t.Errorf("终态应为 response.failed，实得 %q", evs1[0].Name)
	}
	resp1 := decodeJSON(t, []byte(evs1[0].Payload))["response"].(map[string]any)
	er := resp1["error"].(map[string]any)
	if er["code"] != "upstream_error" || er["message"] != "上游炸了" {
		t.Errorf("error 对象错: %v", er)
	}
	// incomplete
	e2 := NewEventEncoder("m")
	if _, err := e2.Encode(stream.Frame{Kind: stream.FrameError, Text: "max_output_tokens", Status: core.StatusIncomplete}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	if _, err := e2.Encode(stream.Frame{Kind: stream.FrameDone, Status: core.StatusIncomplete}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	out2, err := e2.Finish()
	if err != nil {
		t.Fatalf("Finish 报错: %v", err)
	}
	evs2 := sseEvents(t, out2)
	if evs2[0].Name != "response.incomplete" {
		t.Errorf("终态应为 response.incomplete，实得 %q", evs2[0].Name)
	}
	resp2 := decodeJSON(t, []byte(evs2[0].Payload))["response"].(map[string]any)
	if resp2["status"] != "incomplete" {
		t.Errorf("response.status 应为 incomplete，实得 %v", resp2["status"])
	}
	if r := resp2["incomplete_details"].(map[string]any)["reason"]; r != "max_output_tokens" {
		t.Errorf("incomplete_details.reason 应为 max_output_tokens，实得 %v", r)
	}
}

// TestEventEncoder_NameOnlyFrameKeepsName 钉住补名帧（output_item.added 语义）：
// 不发事件，但函数名进终态 function_call item（⛔ 不静默丢工具名）。
func TestEventEncoder_NameOnlyFrameKeepsName(t *testing.T) {
	e := NewEventEncoder("m")
	// 补名帧：Arguments 空、Text=函数名 ⇒ 不发事件。
	bName, err := e.Encode(stream.Frame{Kind: stream.FrameToolCallArgsDelta, ToolCallID: "call_1", Text: "get_weather"})
	if err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	if len(bName) != 0 {
		t.Errorf("补名帧不应发事件，实得 %q", bName)
	}
	bDelta, err := e.Encode(stream.Frame{Kind: stream.FrameToolCallArgsDelta, ToolCallID: "call_1", Arguments: `{}`})
	if err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	bTail, err := e.Finish()
	if err != nil {
		t.Fatalf("Finish 报错: %v", err)
	}
	evs := sseEvents(t, append(bDelta, bTail...))
	// 只有 1 条参数增量事件 + 终态 + [DONE]（补名帧不发事件）。
	if len(evs) != 3 {
		t.Fatalf("应恰有 3 条（1 增量 + 终态 + [DONE]），实得 %d：%+v", len(evs), evs)
	}
	resp := decodeJSON(t, []byte(evs[1].Payload))["response"].(map[string]any)
	fc := resp["output"].([]any)[0].(map[string]any)
	if fc["name"] != "get_weather" || fc["arguments"] != "{}" {
		t.Errorf("终态 function_call item 应带工具名，实得 %v", fc)
	}
}

// TestEventEncoder_MisuseErrors 钉住误用显式报错：Finish 后 Encode、重复 Finish。
func TestEventEncoder_MisuseErrors(t *testing.T) {
	e := NewEventEncoder("m")
	if _, err := e.Finish(); err != nil {
		t.Fatalf("Finish 报错: %v", err)
	}
	if _, err := e.Encode(stream.Frame{Kind: stream.FrameTextDelta, Text: "x"}); err == nil {
		t.Error("Finish 后 Encode 应报错")
	}
	if _, err := e.Finish(); err == nil {
		t.Error("重复 Finish 应报错（终态事件必须恰好一个）")
	}
}

// ---- 上游事件解码 ParseEvent（统一 SDD 3.7 / 4.3.1 容忍性）----

// TestParseEvent_Deltas 钉住三类增量事件的帧映射
// （reasoning_text 与 reasoning_summary_text 都归 ReasoningDelta）。
func TestParseEvent_Deltas(t *testing.T) {
	cases := []struct {
		data string
		kind stream.FrameKind
	}{
		{`{"type":"response.output_text.delta","delta":"你"}`, stream.FrameTextDelta},
		{`{"type":"response.reasoning_text.delta","delta":"想"}`, stream.FrameReasoningDelta},
		{`{"type":"response.reasoning_summary_text.delta","delta":"摘"}`, stream.FrameReasoningDelta},
		{`{"type":"response.function_call_arguments.delta","item_id":"call_1","delta":"{}"}`, stream.FrameToolCallArgsDelta},
	}
	for _, c := range cases {
		frames, err := ParseEvent(core.ProviderOpenAI, stream.SSEEvent{Data: c.data})
		if err != nil {
			t.Fatalf("ParseEvent 报错: %v", err)
		}
		if len(frames) != 1 || frames[0].Kind != c.kind {
			t.Errorf("%s 应出 %v 帧，实得 %+v", c.data, c.kind, frames)
		}
	}
}

// TestParseEvent_CompletedUsageAndDone 钉住 response.completed ⇒ Usage + Done
// （usage 自带、无需开关，4.3.1）；Status 用六值原值。
func TestParseEvent_CompletedUsageAndDone(t *testing.T) {
	data := `{"type":"response.completed","sequence_number":7,"response":{
		"status":"completed",
		"usage":{"input_tokens":125,"output_tokens":48,"total_tokens":173,
			"input_tokens_details":{"cached_tokens":98},
			"output_tokens_details":{"reasoning_tokens":7}}
	}}`
	frames, err := ParseEvent(core.ProviderOpenAI, stream.SSEEvent{Event: "response.completed", Data: data})
	if err != nil {
		t.Fatalf("ParseEvent 报错: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("应为 [Usage, Done] 共 2 帧，实得 %+v", frames)
	}
	if frames[0].Kind != stream.FrameUsage || frames[0].Usage == nil {
		t.Fatalf("帧 0 应为 Usage 帧，实得 %+v", frames[0])
	}
	u := frames[0].Usage
	if u.InputTokens != 125 || u.OutputTokens != 48 || u.CacheReadTokens != 98 || u.ReasoningTokens != 7 {
		t.Errorf("usage 归一错: %+v", u)
	}
	if frames[1].Kind != stream.FrameDone || frames[1].Status != core.StatusCompleted {
		t.Errorf("帧 1 应为 Done(Status=completed)，实得 %+v", frames[1])
	}
	// sequence_number 随帧上抛（乱序只记不中断）。
	if frames[0].Seq != 7 || frames[1].Seq != 7 {
		t.Errorf("Frame.Seq 应记 sequence_number=7，实得 %d/%d", frames[0].Seq, frames[1].Seq)
	}
}

// TestParseEvent_FailedAndIncomplete 钉住 failed / incomplete ⇒ Error + Done。
func TestParseEvent_FailedAndIncomplete(t *testing.T) {
	frames, err := ParseEvent(core.ProviderOpenAI, stream.SSEEvent{
		Event: "response.failed",
		Data:  `{"type":"response.failed","response":{"status":"failed","error":{"code":"upstream_error","message":"炸了"}}}`,
	})
	if err != nil {
		t.Fatalf("ParseEvent 报错: %v", err)
	}
	if len(frames) != 2 || frames[0].Kind != stream.FrameError || frames[1].Kind != stream.FrameDone {
		t.Fatalf("failed 应为 [Error, Done]，实得 %+v", frames)
	}
	if frames[0].Text != "炸了" || frames[0].Status != "upstream_error" {
		t.Errorf("Error 帧应载 message/code，实得 %+v", frames[0])
	}
	if frames[1].Status != core.StatusFailed {
		t.Errorf("Done.Status 应为 failed，实得 %+v", frames[1])
	}

	frames, err = ParseEvent(core.ProviderOpenAI, stream.SSEEvent{
		Event: "response.incomplete",
		Data:  `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
	})
	if err != nil {
		t.Fatalf("ParseEvent 报错: %v", err)
	}
	if len(frames) != 2 || frames[0].Kind != stream.FrameError || frames[1].Kind != stream.FrameDone {
		t.Fatalf("incomplete 应为 [Error, Done]，实得 %+v", frames)
	}
	if frames[0].Text != "max_output_tokens" || frames[1].Status != core.StatusIncomplete {
		t.Errorf("incomplete 帧映射错: %+v", frames)
	}
}

// TestParseEvent_OutputItemAddedCarriesName 钉住 function_call 的 output_item.added
// ⇒ 补名帧（Text=函数名，⛔ 不丢工具名）。
func TestParseEvent_OutputItemAddedCarriesName(t *testing.T) {
	data := `{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":""}}`
	frames, err := ParseEvent(core.ProviderOpenAI, stream.SSEEvent{Data: data})
	if err != nil {
		t.Fatalf("ParseEvent 报错: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind != stream.FrameToolCallArgsDelta {
		t.Fatalf("应为补名帧，实得 %+v", frames)
	}
	if frames[0].ToolCallID != "call_1" || frames[0].Text != "get_weather" {
		t.Errorf("补名帧应载 call_id/函数名，实得 %+v", frames[0])
	}
}

// TestParseEvent_Tolerant 钉住解析器容忍性（4.3.1）：未知事件类型忽略不报错、
// 缺 output_item.added 直接 delta 容忍、缺 [DONE] 不影响、乱序 sequence_number 只记不中断。
func TestParseEvent_Tolerant(t *testing.T) {
	// 未知事件类型 ⇒ 忽略但不报错。
	frames, err := ParseEvent(core.ProviderOpenAI, stream.SSEEvent{
		Data: `{"type":"response.mystery_event","sequence_number":3}`,
	})
	if err != nil || len(frames) != 0 {
		t.Errorf("未知事件应忽略且不报错，实得 frames=%+v err=%v", frames, err)
	}
	// 事件名也没有 ⇒ 同样忽略。
	frames, err = ParseEvent(core.ProviderOpenAI, stream.SSEEvent{Data: `{"sequence_number":4}`})
	if err != nil || len(frames) != 0 {
		t.Errorf("无 type 未知事件应忽略，实得 frames=%+v err=%v", frames, err)
	}
	// 缺 output_item.added 直接 delta ⇒ 容忍出帧。
	frames, err = ParseEvent(core.ProviderOpenAI, stream.SSEEvent{
		Data: `{"type":"response.output_text.delta","sequence_number":2,"delta":"x"}`,
	})
	if err != nil || len(frames) != 1 || frames[0].Text != "x" {
		t.Errorf("缺 added 的 delta 应容忍出帧，实得 frames=%+v err=%v", frames, err)
	}
	// 乱序 sequence_number（变小）⇒ 只记（Frame.Seq）不中断。
	frames, err = ParseEvent(core.ProviderOpenAI, stream.SSEEvent{
		Data: `{"type":"response.output_text.delta","sequence_number":1,"delta":"y"}`,
	})
	if err != nil || len(frames) != 1 || frames[0].Seq != 1 {
		t.Errorf("乱序 sequence_number 应只记不中断，实得 frames=%+v err=%v", frames, err)
	}
}

// TestParseEvent_DoneSentinel 钉住 data:[DONE] 字面量 ⇒ Done 帧。
func TestParseEvent_DoneSentinel(t *testing.T) {
	frames, err := ParseEvent(core.ProviderOpenAI, stream.SSEEvent{Data: "[DONE]"})
	if err != nil {
		t.Fatalf("ParseEvent 报错: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind != stream.FrameDone {
		t.Errorf("[DONE] 应出 Done 帧，实得 %+v", frames)
	}
}

// TestParseEvent_Lifecycle 钉住生命周期事件 ⇒ FrameLifecycle（Status=事件后缀）。
func TestParseEvent_Lifecycle(t *testing.T) {
	frames, err := ParseEvent(core.ProviderOpenAI, stream.SSEEvent{
		Event: "response.created",
		Data:  `{"type":"response.created"}`,
	})
	if err != nil {
		t.Fatalf("ParseEvent 报错: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind != stream.FrameLifecycle || frames[0].Status != "created" {
		t.Errorf("response.created 应出 FrameLifecycle(Status=created)，实得 %+v", frames)
	}
}

// TestParseEvent_InvalidJSON 钉住非法 JSON 显式报错。
func TestParseEvent_InvalidJSON(t *testing.T) {
	if _, err := ParseEvent(core.ProviderOpenAI, stream.SSEEvent{Data: `{"type":`}); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// TestEncodeResponse_StatusSixValues 钉住 status 六值原值逐个透传（统一 SDD 4.2.2）。
func TestEncodeResponse_StatusSixValues(t *testing.T) {
	for _, st := range []string{
		core.StatusCompleted, core.StatusFailed, core.StatusIncomplete,
		core.StatusCancelled, core.StatusInProgress, core.StatusQueued,
	} {
		b, err := EncodeResponse(&core.Response{Model: "m", Status: st})
		if err != nil {
			t.Fatalf("EncodeResponse 报错: %v", err)
		}
		if got := decodeJSON(t, b)["status"]; got != st {
			t.Errorf("status 应原值透传 %q，实得 %v", st, got)
		}
	}
}

// TestParseEvent_EventNameFallback 钉住 data.type 缺失时取 event: 行（二者冗余，任选其一）。
func TestParseEvent_EventNameFallback(t *testing.T) {
	frames, err := ParseEvent(core.ProviderOpenAI, stream.SSEEvent{
		Event: "response.output_text.delta",
		Data:  `{"delta":"z"}`,
	})
	if err != nil {
		t.Fatalf("ParseEvent 报错: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind != stream.FrameTextDelta || frames[0].Text != "z" {
		t.Errorf("event: 行回退映射错，实得 %+v", frames)
	}
}

// TestEventEncoder_IDMonotonic 钉住 id / created_at 单调（跨编码器 id 严格递增）。
func TestEventEncoder_IDMonotonic(t *testing.T) {
	e1 := NewEventEncoder("m")
	e2 := NewEventEncoder("m")
	r1 := terminalResponseID(t, e1)
	r2 := terminalResponseID(t, e2)
	n1, err := strconv.ParseInt(strings.TrimPrefix(r1, "resp_"), 10, 64)
	if err != nil {
		t.Fatalf("id1 形态错: %q", r1)
	}
	n2, err := strconv.ParseInt(strings.TrimPrefix(r2, "resp_"), 10, 64)
	if err != nil {
		t.Fatalf("id2 形态错: %q", r2)
	}
	if n2 <= n1 {
		t.Errorf("id 应严格递增: %d → %d", n1, n2)
	}
}

// terminalResponseID 取编码器终态 response 对象的 id。
func terminalResponseID(t *testing.T, e *EventEncoder) string {
	t.Helper()
	out, err := e.Finish()
	if err != nil {
		t.Fatalf("Finish 报错: %v", err)
	}
	evs := sseEvents(t, out)
	return decodeJSON(t, []byte(evs[0].Payload))["response"].(map[string]any)["id"].(string)
}

// TestDecodeRequestReasoningSignatureRoundTrip 钉住入站回传方向的签名保留：
// reasoning item 的 signature ⇒ Message.ThinkingSignature（回传丢失与出站丢失同罪，
// 1.4.1 语义字段）。错误实现对照：漏解析 signature ⇒ 本测试必红。
func TestDecodeRequestReasoningSignatureRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"enc-1","signature":"sig-back-1","content":[{"type":"reasoning_text","text":"思"}]}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest 失败: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("消息数 = %d，期望 1", len(req.Messages))
	}
	m := req.Messages[0]
	if m.ThinkingSignature != "sig-back-1" {
		t.Fatalf("ThinkingSignature = %q，期望 sig-back-1（入站回传签名被丢）", m.ThinkingSignature)
	}
	if m.EncryptedContent != "enc-1" || m.ReasoningContent != "思" {
		t.Fatalf("EncryptedContent/ReasoningContent = %q/%q", m.EncryptedContent, m.ReasoningContent)
	}
}
