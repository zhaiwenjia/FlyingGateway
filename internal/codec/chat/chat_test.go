// 日期: 2026-11-14 16:08:29.894
// 作者: Zhai Wenjia

package chat

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// ---- 入站解码（统一 SDD 3.4 / 11.2）----

// TestDecodeRequest_SystemRoleNotPromoted 钉住「messages[0].role=system 保留 system 角色，
// ⛔ 不提升为 Instructions」（统一 SDD 3.4）。错误实现（把 system 提升进 Instructions）会红：
// Instructions 必须为空、Messages[0].Role 必须仍是 "system"。
func TestDecodeRequest_SystemRoleNotPromoted(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"system","content":"你是助手"},
		{"role":"user","content":"你好"}
	]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest 报错: %v", err)
	}
	if req.Instructions != "" {
		t.Errorf("Instructions 应为空（system 不提升），实得 %q", req.Instructions)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("Messages 数应为 2，实得 %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("Messages[0].Role 应为 system，实得 %q", req.Messages[0].Role)
	}
	if got := req.Messages[0].Content[0].Text; got != "你是助手" {
		t.Errorf("Messages[0] 正文应为 你是助手，实得 %q", got)
	}
}

// TestDecodeRequest_ToolMessageAndToolCalls 钉住 tool 消息的 ToolCallID 与
// assistant 消息 tool_calls 的逐字段映射。
func TestDecodeRequest_ToolMessageAndToolCalls(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}
		]},
		{"role":"tool","tool_call_id":"call_1","name":"get_weather","content":"晴"}
	]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest 报错: %v", err)
	}
	a := req.Messages[0]
	if len(a.ToolCalls) != 1 {
		t.Fatalf("tool_calls 数应为 1，实得 %d", len(a.ToolCalls))
	}
	tc := a.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "get_weather" || tc.Arguments != `{"city":"北京"}` || tc.Type != "function" {
		t.Errorf("tool_call 映射错: %+v", tc)
	}
	tm := req.Messages[1]
	if tm.Role != "tool" || tm.ToolCallID != "call_1" || tm.Name != "get_weather" {
		t.Errorf("tool 消息映射错: Role=%q ToolCallID=%q Name=%q", tm.Role, tm.ToolCallID, tm.Name)
	}
	if got := tm.Content[0].Text; got != "晴" {
		t.Errorf("tool 消息正文应为 晴，实得 %q", got)
	}
}

// TestDecodeRequest_ReasoningContentPreserved 钉住 reasoning_content 扩展名逐字保留
// （Qwen/Kimi/MiMo/MiniMax/智谱/DeepSeek 扩展）。
func TestDecodeRequest_ReasoningContentPreserved(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":"答案","reasoning_content":"先想一步"}
	]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest 报错: %v", err)
	}
	if got := req.Messages[0].ReasoningContent; got != "先想一步" {
		t.Errorf("ReasoningContent 应为 先想一步，实得 %q", got)
	}
}

// TestDecodeRequest_ContentPartArray 钉住 content part 数组（text / image_url）逐字段映射。
func TestDecodeRequest_ContentPartArray(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":[
			{"type":"text","text":"看图"},
			{"type":"image_url","image_url":{"url":"https://x/y.png","detail":"high"}}
		]}
	]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest 报错: %v", err)
	}
	parts := req.Messages[0].Content
	if len(parts) != 2 {
		t.Fatalf("part 数应为 2，实得 %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "看图" {
		t.Errorf("text part 映射错: %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL != "https://x/y.png" {
		t.Errorf("image part 映射错: %+v", parts[1])
	}
}

// TestDecodeRequest_Params 钉住 stream/temperature/top_p/stop/max_tokens|max_completion_tokens/
// tools/tool_choice ⇒ core 字段（统一 SDD 4.3.1 Chat 请求体关键字段）。
func TestDecodeRequest_Params(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"stream":true,
		"temperature":0.7,
		"top_p":0.9,
		"stop":"\n\n",
		"max_tokens":100,
		"reasoning_effort":"high",
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"f"}}
	}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest 报错: %v", err)
	}
	if !req.Stream {
		t.Error("Stream 应为 true")
	}
	if req.Temperature == nil || *req.Temperature != 0.7 {
		t.Errorf("Temperature 应为 0.7，实得 %v", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.9 {
		t.Errorf("TopP 应为 0.9，实得 %v", req.TopP)
	}
	if len(req.Stop) != 1 || req.Stop[0] != "\n\n" {
		t.Errorf("Stop（字符串形态）应为 [\"\\n\\n\"]，实得 %v", req.Stop)
	}
	if req.MaxOutputTokens != 100 {
		t.Errorf("MaxOutputTokens 应为 100，实得 %d", req.MaxOutputTokens)
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "high" {
		t.Errorf("Reasoning.Effort 应为 high，实得 %+v", req.Reasoning)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "f" || req.Tools[0].Description != "d" {
		t.Errorf("Tools 映射错: %+v", req.Tools)
	}
	if string(req.Tools[0].Parameters) != `{"type":"object"}` {
		t.Errorf("Parameters 应逐字保留，实得 %s", req.Tools[0].Parameters)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != "function" || req.ToolChoice.Name != "f" {
		t.Errorf("ToolChoice 映射错: %+v", req.ToolChoice)
	}
}

// TestDecodeRequest_StopArrayAndMaxCompletionTokens 钉住 stop 数组形态与
// max_completion_tokens（OpenAI 现名）⇒ MaxOutputTokens。
func TestDecodeRequest_StopArrayAndMaxCompletionTokens(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
		"stop":["a","b"],"max_tokens":1,"max_completion_tokens":512,
		"tool_choice":"auto"}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest 报错: %v", err)
	}
	if len(req.Stop) != 2 || req.Stop[0] != "a" || req.Stop[1] != "b" {
		t.Errorf("Stop（数组形态）映射错: %v", req.Stop)
	}
	if req.MaxOutputTokens != 512 {
		t.Errorf("max_completion_tokens 优先 ⇒ MaxOutputTokens=512，实得 %d", req.MaxOutputTokens)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != "auto" {
		t.Errorf("ToolChoice 应为 auto，实得 %+v", req.ToolChoice)
	}
}

// TestDecodeRequest_UnknownFieldsTolerated 钉住读侧宽容：未知字段忽略、不报错。
func TestDecodeRequest_UnknownFieldsTolerated(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
		"n":2,"foo":{"bar":1},"response_format":{"type":"json_object"}}`)
	if _, err := DecodeRequest(body); err != nil {
		t.Fatalf("未知字段不应报错，实得 %v", err)
	}
}

// TestDecodeRequest_InvalidJSON 钉住非法 JSON 显式报错（⛔ 不静默吞）。
func TestDecodeRequest_InvalidJSON(t *testing.T) {
	if _, err := DecodeRequest([]byte(`{"model":`)); err == nil {
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

// chatMessageOf 取 chat 响应体 choices[0].message。
func chatMessageOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	choices, ok := m["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices 应恰有 1 项，实得 %v", m["choices"])
	}
	msg, ok := choices[0].(map[string]any)["message"].(map[string]any)
	if !ok {
		t.Fatalf("choices[0].message 缺失: %v", choices[0])
	}
	return msg
}

// TestEncodeResponse_MessageAndToolCalls 钉住 choices[0].message 的 content/role/tool_calls。
func TestEncodeResponse_MessageAndToolCalls(t *testing.T) {
	resp := &core.Response{
		ID:    "chatcmpl-1",
		Model: "m",
		OutputItems: []core.OutputItem{
			{Type: "message", Role: "assistant", Content: []core.ContentPart{{Type: "text", Text: "你好"}}},
			{Type: "function_call", CallID: "call_1", Name: "get_weather", Arguments: `{"city":"北京"}`},
		},
		FinishReason: core.FinishToolCalls,
	}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 报错: %v", err)
	}
	m := decodeJSON(t, b)
	if m["object"] != "chat.completion" {
		t.Errorf("object 应为 chat.completion，实得 %v", m["object"])
	}
	if m["id"] != "chatcmpl-1" || m["model"] != "m" {
		t.Errorf("id/model 透传错: %v %v", m["id"], m["model"])
	}
	choices0 := m["choices"].([]any)[0].(map[string]any)
	if choices0["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason 应为 tool_calls，实得 %v", choices0["finish_reason"])
	}
	msg := chatMessageOf(t, m)
	if msg["role"] != "assistant" || msg["content"] != "你好" {
		t.Errorf("message 映射错: %v", msg)
	}
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls 应恰有 1 项，实得 %v", msg["tool_calls"])
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" || fn["name"] != "get_weather" || fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("tool_call 输出错: id=%v type=%v name=%v arguments=%v", call["id"], call["type"], fn["name"], fn["arguments"])
	}
}

// TestEncodeResponse_UsageFieldNames 钉住 usage 用 OpenAI Chat 字段名与二级层级
// （统一 SDD 4.3.1 usage 树样例数字逐字：125 / 48 / 173 / 98）。
func TestEncodeResponse_UsageFieldNames(t *testing.T) {
	resp := &core.Response{
		Model:       "m",
		OutputItems: []core.OutputItem{{Type: "message", Content: []core.ContentPart{{Type: "text", Text: "x"}}}},
		Usage: &core.Usage{
			InputTokens:     125,
			CacheReadTokens: 98,
			OutputTokens:    48,
			TotalTokens:     173,
			ReasoningTokens: 7,
		},
	}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 报错: %v", err)
	}
	m := decodeJSON(t, b)
	u, ok := m["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage 缺失: %s", b)
	}
	if u["prompt_tokens"] != float64(125) || u["completion_tokens"] != float64(48) || u["total_tokens"] != float64(173) {
		t.Errorf("usage 顶层字段错: %v", u)
	}
	pd := u["prompt_tokens_details"].(map[string]any)
	if pd["cached_tokens"] != float64(98) {
		t.Errorf("prompt_tokens_details.cached_tokens 应为 98，实得 %v", pd["cached_tokens"])
	}
	cd := u["completion_tokens_details"].(map[string]any)
	if cd["reasoning_tokens"] != float64(7) {
		t.Errorf("completion_tokens_details.reasoning_tokens 应为 7，实得 %v", cd["reasoning_tokens"])
	}
}

// TestEncodeResponse_FinishReasonSupersetPassthrough 钉住 finish_reason 超集原值透传
// （统一 SDD 4.5.1：⛔ 不归一化、⛔ 不丢弃非标准取值）。
func TestEncodeResponse_FinishReasonSupersetPassthrough(t *testing.T) {
	for _, reason := range []string{
		core.FinishRepetitionTruncation,       // MiMo 独有
		core.FinishSensitive,                  // 智谱独有
		core.FinishModelContextWindowExceeded, // 超上下文
		core.FinishInsufficientSystemResource, // DeepSeek 独有
	} {
		resp := &core.Response{Model: "m", FinishReason: reason}
		b, err := EncodeResponse(resp)
		if err != nil {
			t.Fatalf("EncodeResponse 报错: %v", err)
		}
		choices0 := decodeJSON(t, b)["choices"].([]any)[0].(map[string]any)
		if choices0["finish_reason"] != reason {
			t.Errorf("finish_reason 应原值透传 %q，实得 %v", reason, choices0["finish_reason"])
		}
	}
}

// TestEncodeResponse_Degradations 钉住不可逆语义丢失的显式标记（统一 SDD 3.4 字段设计
// 逐字：field=thinking_signature/encrypted_content、reason=upstream_shape_mismatch/
// response_to_chat_conversion）；⛔ 不静默丢（验收 A8）。
func TestEncodeResponse_Degradations(t *testing.T) {
	resp := &core.Response{
		Model: "m",
		OutputItems: []core.OutputItem{
			{Type: "thinking", ReasoningText: "想", ThinkingSignature: "sig-abc"},
			{Type: "reasoning", EncryptedContent: "enc-xyz"},
			{Type: "message", Content: []core.ContentPart{{Type: "text", Text: "答"}}},
		},
	}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 报错: %v", err)
	}
	xg, ok := decodeJSON(t, b)["x_gateway"].(map[string]any)
	if !ok {
		t.Fatalf("x_gateway 缺失（语义丢失被静默丢）: %s", b)
	}
	degs, ok := xg["degradations"].([]any)
	if !ok || len(degs) != 2 {
		t.Fatalf("degradations 应恰有 2 条，实得 %v", xg["degradations"])
	}
	got := map[string]string{}
	for _, d := range degs {
		dd := d.(map[string]any)
		got[dd["field"].(string)] = dd["reason"].(string)
		if dd["detail"] == "" {
			t.Errorf("detail 不应为空: %v", dd)
		}
	}
	if got["thinking_signature"] != "upstream_shape_mismatch" {
		t.Errorf("thinking_signature 的 reason 应为 upstream_shape_mismatch，实得 %q", got["thinking_signature"])
	}
	if got["encrypted_content"] != "response_to_chat_conversion" {
		t.Errorf("encrypted_content 的 reason 应为 response_to_chat_conversion，实得 %q", got["encrypted_content"])
	}
}

// TestEncodeResponse_NoDegradationWhenClean 钉住反向：无语义丢失 ⇒ 无标记
// （恒真防线：错误实现无脑塞标记时会红）。
func TestEncodeResponse_NoDegradationWhenClean(t *testing.T) {
	resp := &core.Response{
		Model:       "m",
		OutputItems: []core.OutputItem{{Type: "message", Content: []core.ContentPart{{Type: "text", Text: "答"}}}},
	}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 报错: %v", err)
	}
	if _, ok := decodeJSON(t, b)["x_gateway"]; ok {
		t.Errorf("无语义丢失时不应有 x_gateway: %s", b)
	}
}

// TestEncodeResponse_ReasoningContent 钉住推理文本走 reasoning_content 扩展（4.2.2）。
func TestEncodeResponse_ReasoningContent(t *testing.T) {
	resp := &core.Response{
		Model: "m",
		OutputItems: []core.OutputItem{
			{Type: "reasoning", ReasoningText: "推理过程"},
			{Type: "message", Content: []core.ContentPart{{Type: "text", Text: "答"}}},
		},
	}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 报错: %v", err)
	}
	if got := chatMessageOf(t, decodeJSON(t, b))["reasoning_content"]; got != "推理过程" {
		t.Errorf("reasoning_content 应为 推理过程，实得 %v", got)
	}
}

// ---- 客户端侧帧编码器 ChunkEncoder（统一 SDD 3.4 流式位置）----

// decodeSSEData 解析一条 `data: {...}` SSE 行的 JSON 载荷。
func decodeSSEData(t *testing.T, b []byte) map[string]any {
	t.Helper()
	s := strings.TrimSpace(string(b))
	s, ok := strings.CutPrefix(s, "data: ")
	if !ok {
		t.Fatalf("Encode 输出应为 data: 行，实得 %q", s)
	}
	return decodeJSON(t, []byte(s))
}

// sseDataPayloads 拆出 SSE data 行的载荷序列（每事件一行 data: ...）。
func sseDataPayloads(t *testing.T, out []byte) []string {
	t.Helper()
	var payloads []string
	for _, ev := range strings.Split(strings.TrimRight(string(out), "\n"), "\n\n") {
		if ev == "" {
			continue
		}
		if !strings.HasPrefix(ev, "data: ") {
			t.Fatalf("chunk 流事件应为 data: 行，实得 %q", ev)
		}
		payloads = append(payloads, strings.TrimPrefix(ev, "data: "))
	}
	return payloads
}

// TestChunkEncoder_DeltaChunks 钉住三类增量帧的 chat.completion.chunk 形状。
func TestChunkEncoder_DeltaChunks(t *testing.T) {
	e := NewChunkEncoder("m")
	b1, err := e.Encode(stream.Frame{Kind: stream.FrameTextDelta, Text: "你"})
	if err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	m := decodeSSEData(t, b1)
	if m["object"] != "chat.completion.chunk" || m["model"] != "m" {
		t.Errorf("chunk 头字段错: %v", m)
	}
	c0 := m["choices"].([]any)[0].(map[string]any)
	if c0["finish_reason"] != nil {
		t.Errorf("增量帧 finish_reason 应为 null，实得 %v", c0["finish_reason"])
	}
	if d := c0["delta"].(map[string]any); d["content"] != "你" {
		t.Errorf("delta.content 应为 你，实得 %v", d["content"])
	}

	b2, _ := e.Encode(stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: "想"})
	d2 := decodeSSEData(t, b2)["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if d2["reasoning_content"] != "想" {
		t.Errorf("delta.reasoning_content 应为 想，实得 %v", d2["reasoning_content"])
	}

	b3, _ := e.Encode(stream.Frame{
		Kind:       stream.FrameToolCallArgsDelta,
		ToolCallID: "call_1",
		Text:       "get_weather",
		Arguments:  `{"city":`,
	})
	d3 := decodeSSEData(t, b3)["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	calls := d3["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" || call["index"] != float64(0) {
		t.Errorf("tool_call 帧头错: %v", call)
	}
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":` {
		t.Errorf("tool_call function 错: %v", fn)
	}
}

// TestChunkEncoder_FinishTerminalUsageAndDone 钉住 Finish 输出序：
// 终止帧（finish_reason 非 null，payload 带 x_gateway.degradations[]）
// + usage 块（choices: []）+ 末尾 data: [DONE]（统一 SDD 3.4 流式位置）。
func TestChunkEncoder_FinishTerminalUsageAndDone(t *testing.T) {
	e := NewChunkEncoder("m")
	if _, err := e.Encode(stream.Frame{Kind: stream.FrameTextDelta, Text: "答"}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	if _, err := e.Encode(stream.Frame{
		Kind:   stream.FrameDone,
		Status: core.FinishLength,
		Usage:  &core.Usage{InputTokens: 125, CacheReadTokens: 98, OutputTokens: 48, TotalTokens: 173},
	}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	e.AddDegradation(core.Degradation{
		Field:  "thinking_signature",
		Reason: core.DegradationUpstreamShapeMismatch,
		Detail: degradationDetailSignature,
	})
	out, err := e.Finish()
	if err != nil {
		t.Fatalf("Finish 报错: %v", err)
	}
	payloads := sseDataPayloads(t, out)
	if len(payloads) != 3 {
		t.Fatalf("Finish 应产出 终止帧+usage块+[DONE] 共 3 条，实得 %d：%v", len(payloads), payloads)
	}
	if payloads[2] != "[DONE]" {
		t.Errorf("末条应为 [DONE]，实得 %q", payloads[2])
	}
	term := decodeJSON(t, []byte(payloads[0]))
	c0 := term["choices"].([]any)[0].(map[string]any)
	if c0["finish_reason"] != core.FinishLength {
		t.Errorf("终止帧 finish_reason 应为 length，实得 %v", c0["finish_reason"])
	}
	xg := term["x_gateway"].(map[string]any)
	degs := xg["degradations"].([]any)
	if len(degs) != 1 || degs[0].(map[string]any)["field"] != "thinking_signature" {
		t.Errorf("终止帧 payload 应带 x_gateway.degradations[]，实得 %v", xg)
	}
	usageChunk := decodeJSON(t, []byte(payloads[1]))
	if choices, ok := usageChunk["choices"].([]any); !ok || len(choices) != 0 {
		t.Errorf("usage 块 choices 应为 []，实得 %v", usageChunk["choices"])
	}
	u := usageChunk["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(125) || u["completion_tokens"] != float64(48) {
		t.Errorf("usage 块字段错: %v", u)
	}
}

// TestChunkEncoder_SignatureFrameDegradation 钉住流式 thinking 签名丢失的显式标记
// （统一 SDD 4.2.5 / 3.4：Chat 出口承载不了 thinking 块签名 ⇒ x_gateway.degradations[]
// 标记，⛔ 不静默丢）：签名帧（FrameReasoningDelta 且 ThinkingSignature 非空，承载
// 约定见 stream.FrameReasoningDelta）不产出空 delta chunk，终止帧 payload 的
// degradations[] 含 field=thinking_signature，且多块签名只记一次（去重口径与
// 非流式 collectDegradations 一致）。
// 错误实现会红：静默丢签名（无标记）⇒ degradations 缺失断言红；每块重复标记 ⇒
// 恰 1 条断言红；签名帧当普通推理帧发空 reasoning_content chunk ⇒ 不产出 chunk 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/codec/chat/ -run TestChunkEncoder_SignatureFrameDegradation -v
func TestChunkEncoder_SignatureFrameDegradation(t *testing.T) {
	e := NewChunkEncoder("m")
	bSig1, err := e.Encode(stream.Frame{Kind: stream.FrameReasoningDelta, ThinkingSignature: "sig-1"})
	if err != nil || bSig1 != nil {
		t.Fatalf("签名帧不应产出 chunk（err=%v b=%v）", err, bSig1)
	}
	if _, err := e.Encode(stream.Frame{Kind: stream.FrameReasoningDelta, ThinkingSignature: "sig-2"}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	if _, err := e.Encode(stream.Frame{Kind: stream.FrameDone, Status: core.FinishStop}); err != nil {
		t.Fatalf("Encode 报错: %v", err)
	}
	out, err := e.Finish()
	if err != nil {
		t.Fatalf("Finish 报错: %v", err)
	}
	payloads := sseDataPayloads(t, out)
	term := decodeJSON(t, []byte(payloads[0]))
	xg, ok := term["x_gateway"].(map[string]any)
	if !ok {
		t.Fatalf("thinking 签名丢失必须产生 x_gateway.degradations[] 标记: %s", payloads[0])
	}
	degs := xg["degradations"].([]any)
	if len(degs) != 1 {
		t.Fatalf("thinking_signature 应恰记 1 条（多块签名去重），实得 %v", degs)
	}
	d := degs[0].(map[string]any)
	if d["field"] != "thinking_signature" || d["reason"] != "upstream_shape_mismatch" || d["detail"] == "" {
		t.Errorf("degradation 字段错: %v", d)
	}
}

// TestChunkEncoder_IDCreatedMonotonic 钉住 id/created 单调：
// 同一编码器内恒定；跨编码器 id 严格递增、created 非递减。
func TestChunkEncoder_IDCreatedMonotonic(t *testing.T) {
	e1 := NewChunkEncoder("m")
	e2 := NewChunkEncoder("m")
	b1, _ := e1.Encode(stream.Frame{Kind: stream.FrameTextDelta, Text: "a"})
	b1b, _ := e1.Encode(stream.Frame{Kind: stream.FrameTextDelta, Text: "b"})
	b2, _ := e2.Encode(stream.Frame{Kind: stream.FrameTextDelta, Text: "c"})
	m1 := decodeSSEData(t, b1)
	m1b := decodeSSEData(t, b1b)
	m2 := decodeSSEData(t, b2)
	if m1["id"] != m1b["id"] || m1["created"] != m1b["created"] {
		t.Errorf("同一编码器 id/created 应恒定: %v vs %v", m1, m1b)
	}
	id1 := m1["id"].(string)
	id2 := m2["id"].(string)
	if id1 == id2 {
		t.Errorf("跨编码器 id 不应重复: %q", id1)
	}
	n1, err := strconv.ParseInt(strings.TrimPrefix(id1, "chatcmpl-"), 10, 64)
	if err != nil {
		t.Fatalf("id1 形态错: %q", id1)
	}
	n2, err := strconv.ParseInt(strings.TrimPrefix(id2, "chatcmpl-"), 10, 64)
	if err != nil {
		t.Fatalf("id2 形态错: %q", id2)
	}
	if n2 <= n1 {
		t.Errorf("id 应严格递增: %d → %d", n1, n2)
	}
	c1 := m1["created"].(float64)
	c2 := m2["created"].(float64)
	if c2 < c1 {
		t.Errorf("created 应非递减: %v → %v", c1, c2)
	}
}

// TestChunkEncoder_MisuseErrors 钉住误用显式报错：Finish 后再 Encode、重复 Finish、
// 错误帧无法承载（⛔ 不静默吞）。
func TestChunkEncoder_MisuseErrors(t *testing.T) {
	e := NewChunkEncoder("m")
	if _, err := e.Encode(stream.Frame{Kind: stream.FrameError, Text: "boom"}); err == nil {
		t.Error("错误帧在 chat 形状应显式报错")
	}
	if _, err := e.Finish(); err != nil {
		t.Fatalf("Finish 报错: %v", err)
	}
	if _, err := e.Encode(stream.Frame{Kind: stream.FrameTextDelta, Text: "x"}); err == nil {
		t.Error("Finish 后 Encode 应报错")
	}
	if _, err := e.Finish(); err == nil {
		t.Error("重复 Finish 应报错（终止帧必须恰好一次）")
	}
}

// ---- 上游 chunk 解码 ParseChunk（统一 SDD 3.7 帧映射）----

// TestParseChunk_Deltas 钉住 delta.content / delta.reasoning_content / delta.tool_calls
// 三类增量的帧映射（reasoning_content 为 Qwen/Kimi/MiMo/MiniMax/智谱/DeepSeek 扩展）。
func TestParseChunk_Deltas(t *testing.T) {
	data := []byte(`{"id":"c1","object":"chat.completion.chunk","choices":[
		{"index":0,"delta":{"content":"你","reasoning_content":"想","tool_calls":[
			{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}
		]},"finish_reason":null}
	],"usage":null}`)
	frames, err := ParseChunk(core.ProviderQwen, data)
	if err != nil {
		t.Fatalf("ParseChunk 报错: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("帧数应为 3，实得 %d：%+v", len(frames), frames)
	}
	if frames[0].Kind != stream.FrameTextDelta || frames[0].Text != "你" {
		t.Errorf("帧 0 应为 TextDelta(你)，实得 %+v", frames[0])
	}
	if frames[1].Kind != stream.FrameReasoningDelta || frames[1].Reasoning != "想" {
		t.Errorf("帧 1 应为 ReasoningDelta(想)，实得 %+v", frames[1])
	}
	if frames[2].Kind != stream.FrameToolCallArgsDelta ||
		frames[2].ToolCallID != "call_1" || frames[2].Arguments != "{}" || frames[2].Text != "f" {
		t.Errorf("帧 2 应为 ToolCallArgsDelta(call_1/f/{})，实得 %+v", frames[2])
	}
}

// TestParseChunk_FinishReasonSuperset 钉住 finish_reason 非 null ⇒ Done 帧，
// Status 用超集原值（⛔ 不归一化，统一 SDD 4.5.1）。
func TestParseChunk_FinishReasonSuperset(t *testing.T) {
	data := []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"sensitive"}]}`)
	frames, err := ParseChunk(core.ProviderZhipu, data)
	if err != nil {
		t.Fatalf("ParseChunk 报错: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind != stream.FrameDone || frames[0].Status != "sensitive" {
		t.Errorf("应为 Done(Status=sensitive)，实得 %+v", frames)
	}
}

// TestParseChunk_DoneSentinel 钉住 data:[DONE] 字面量 ⇒ Done 帧。
func TestParseChunk_DoneSentinel(t *testing.T) {
	frames, err := ParseChunk(core.ProviderOpenAI, []byte("[DONE]"))
	if err != nil {
		t.Fatalf("ParseChunk 报错: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind != stream.FrameDone {
		t.Errorf("[DONE] 应出 Done 帧，实得 %+v", frames)
	}
}

// TestParseChunk_UsageFrame 钉住 choices:[]+usage ⇒ Usage 帧，字段经 usage.Normalize
// 归一（prompt_tokens⇒InputTokens、prompt_tokens_details.cached_tokens⇒CacheReadTokens）。
func TestParseChunk_UsageFrame(t *testing.T) {
	data := []byte(`{"choices":[],"usage":{
		"prompt_tokens":125,"completion_tokens":48,"total_tokens":173,
		"prompt_tokens_details":{"cached_tokens":98,"cache_write_tokens":10},
		"completion_tokens_details":{"reasoning_tokens":7}
	}}`)
	frames, err := ParseChunk(core.ProviderOpenAI, data)
	if err != nil {
		t.Fatalf("ParseChunk 报错: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind != stream.FrameUsage || frames[0].Usage == nil {
		t.Fatalf("应为 Usage 帧，实得 %+v", frames)
	}
	u := frames[0].Usage
	if u.InputTokens != 125 || u.OutputTokens != 48 || u.TotalTokens != 173 ||
		u.CacheReadTokens != 98 || u.CacheWriteTokens != 10 || u.ReasoningTokens != 7 {
		t.Errorf("usage 归一错: %+v", u)
	}
}

// TestParseChunk_UsageNullTolerated 钉住 usage 为 null / 缺失均容忍、不出帧不报错
// （统一 SDD 4.3.1：其余 chunk 的 usage 为 null）。
func TestParseChunk_UsageNullTolerated(t *testing.T) {
	for _, data := range []string{
		`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}],"usage":null}`,
		`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`,
	} {
		frames, err := ParseChunk(core.ProviderOpenAI, []byte(data))
		if err != nil {
			t.Fatalf("ParseChunk 报错: %v", err)
		}
		if len(frames) != 1 || frames[0].Kind != stream.FrameTextDelta {
			t.Errorf("应只出 TextDelta 帧，实得 %+v", frames)
		}
	}
}

// TestParseChunk_DeepSeekCacheOneOfTwo 钉住 DeepSeek 命中字段「二选一不累加」
// （统一 SDD 11.1：prompt_cache_hit_tokens 与 prompt_tokens_details.cached_tokens 同值 ⇒
// CacheReadTokens=53，⛔ 不是 106）。
func TestParseChunk_DeepSeekCacheOneOfTwo(t *testing.T) {
	data := []byte(`{"choices":[],"usage":{
		"prompt_tokens":309,"completion_tokens":10,"total_tokens":319,
		"prompt_cache_hit_tokens":53,
		"prompt_tokens_details":{"cached_tokens":53}
	}}`)
	frames, err := ParseChunk(core.ProviderDeepSeek, data)
	if err != nil {
		t.Fatalf("ParseChunk 报错: %v", err)
	}
	if got := frames[0].Usage.CacheReadTokens; got != 53 {
		t.Errorf("CacheReadTokens 应为 53（二选一不累加），实得 %d", got)
	}
}

// TestParseChunk_ContentWithFinishReason 钉住同帧携带增量与 finish_reason 时的帧序：
// 增量在前、Done 在后。
func TestParseChunk_ContentWithFinishReason(t *testing.T) {
	data := []byte(`{"choices":[{"index":0,"delta":{"content":"尾"},"finish_reason":"stop"}]}`)
	frames, err := ParseChunk(core.ProviderOpenAI, data)
	if err != nil {
		t.Fatalf("ParseChunk 报错: %v", err)
	}
	if len(frames) != 2 || frames[0].Kind != stream.FrameTextDelta || frames[1].Kind != stream.FrameDone {
		t.Errorf("帧序应为 [TextDelta, Done]，实得 %+v", frames)
	}
}

// TestParseChunk_InvalidJSON 钉住非法 JSON 显式报错。
func TestParseChunk_InvalidJSON(t *testing.T) {
	if _, err := ParseChunk(core.ProviderOpenAI, []byte(`{"choices":`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}
