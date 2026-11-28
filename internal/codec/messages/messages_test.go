// 日期: 2026-11-15 00:04:25.586
// 作者: Zhai Wenjia

package messages

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// TestDecodeRequestBasics 断言 Messages 入站翻译：system / 消息 / 工具 / tool_choice /
// thinking / top_k / tool_result 拆分，字段口径与 adapter/anthropic.go 镜像。
func TestDecodeRequestBasics(t *testing.T) {
	body := `{
	  "model": "claude-sonnet-5.5",
	  "max_tokens": 1024,
	  "system": "你是助手",
	  "temperature": 0.5, "top_p": 0.9, "top_k": 5, "stop_sequences": ["END"],
	  "thinking": {"type": "enabled", "budget_tokens": 1024},
	  "tools": [{"name": "f", "description": "d", "input_schema": {"type": "object"}}],
	  "tool_choice": {"type": "tool", "name": "f"},
	  "messages": [
	    {"role": "user", "content": "hi"},
	    {"role": "assistant", "content": [
	      {"type": "thinking", "thinking": "想想", "signature": "sig-1"},
	      {"type": "text", "text": "好"},
	      {"type": "tool_use", "id": "tu_1", "name": "f", "input": {"x": 1}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "tu_1", "content": "结果"},
	      {"type": "text", "text": "继续"}
	    ]}
	  ],
	  "metadata": {"user_id": "u"}
	}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest 失败: %v", err)
	}
	if req.Model != "claude-sonnet-5.5" || req.MaxOutputTokens != 1024 {
		t.Fatalf("model/max_tokens 失真: %q %d", req.Model, req.MaxOutputTokens)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content[0].Text != "你是助手" {
		t.Fatalf("system 未落首条消息: %+v", req.Messages[0])
	}
	if req.Stop[0] != "END" || req.Temperature == nil || req.TopP == nil {
		t.Fatalf("采样/停止参数失真: %+v", req)
	}
	if req.Extra["top_k"] != 5 {
		t.Fatalf("top_k 未进 Extra: %v", req.Extra)
	}
	if req.Reasoning == nil {
		t.Fatal("thinking 未映射 Reasoning")
	}
	// assistant：thinking 签名 + text + tool_use
	asst := req.Messages[2]
	if asst.ReasoningContent != "想想" || asst.ThinkingSignature != "sig-1" {
		t.Fatalf("thinking 映射失真: %+v", asst)
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "tu_1" || asst.ToolCalls[0].Name != "f" {
		t.Fatalf("tool_use 映射失真: %+v", asst.ToolCalls)
	}
	// tool_result 拆成独立 tool 消息（在 user 段之前，按块序）
	tm := req.Messages[3]
	if tm.Role != "tool" || tm.ToolCallID != "tu_1" || tm.Content[0].Text != "结果" {
		t.Fatalf("tool_result 拆分失真: %+v", tm)
	}
	if req.Messages[4].Role != "user" || req.Messages[4].Content[0].Text != "继续" {
		t.Fatalf("tool_result 后文本段失真: %+v", req.Messages[4])
	}
	if req.Tools[0].Name != "f" || req.ToolChoice == nil || req.ToolChoice.Mode != "function" {
		t.Fatalf("tools/tool_choice 映射失真: %+v / %+v", req.Tools, req.ToolChoice)
	}
}

// TestDecodeRequestRejects 断言非法输入显式拒绝：缺 max_tokens、role 越界、
// tool_choice 缺 name。
// ⚠️ CAC-12（2026-11-29）：「多个不同 cache_control 断点」不再属于非法输入 ——
// 位置化断点由 CacheMarks 承载，见 TestDecodeRequestKeepsMultipleCacheMarks。
func TestDecodeRequestRejects(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"x"}]}`,                                              // 缺 max_tokens
		`{"model":"m","max_tokens":1,"messages":[{"role":"system","content":"x"}]}`,                             // role 越界
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"tool"}}`, // 缺 name
	} {
		if _, err := DecodeRequest([]byte(body)); err == nil {
			t.Fatalf("应拒绝: %.60s…", body)
		}
	}
}

// TestEncodeResponse 断言响应编码：块映射、stop_reason 超集映射、usage 三项拆分
// （input_tokens = 总输入 − 缓存写入 − 缓存命中，⛔ 不整包塞总输入）。
func TestEncodeResponse(t *testing.T) {
	resp := &core.Response{
		ID: "resp_1", Model: "claude-sonnet-5.5", FinishReason: "length",
		OutputItems: []core.OutputItem{
			{Type: "reasoning", ReasoningText: "思考", ThinkingSignature: "sig"},
			{Type: "message", Role: "assistant", Content: []core.ContentPart{{Type: "text", Text: "答案"}}},
			{Type: "function_call", CallID: "tu_1", Name: "f", Arguments: `{"x":1}`},
		},
		Usage: &core.Usage{InputTokens: 12, CacheReadTokens: 3, CacheWriteTokens: 2, OutputTokens: 7},
	}
	blob, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("输出非 JSON: %v", err)
	}
	if out["type"] != "message" || out["role"] != "assistant" || out["stop_reason"] != "max_tokens" {
		t.Fatalf("外壳/stop_reason 失真: %v", out)
	}
	content := out["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content 块数 = %d，期望 3", len(content))
	}
	thinking := content[0].(map[string]any)
	if thinking["type"] != "thinking" || thinking["signature"] != "sig" {
		t.Fatalf("thinking 块失真: %v", thinking)
	}
	tu := content[2].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "tu_1" {
		t.Fatalf("tool_use 块失真: %v", tu)
	}
	u := out["usage"].(map[string]any)
	if u["input_tokens"] != 7.0 || u["cache_read_input_tokens"] != 3.0 ||
		u["cache_creation_input_tokens"] != 2.0 || u["output_tokens"] != 7.0 {
		t.Fatalf("usage 三项拆分失真（input 须 = 12-3-2 = 7）: %v", u)
	}
}

// TestEventEncoderSequence 断言流式事件序列：message_start → 块 start/delta/stop →
// message_delta（累计 usage + stop_reason）→ message_stop；签名帧一帧收口一块。
func TestEventEncoderSequence(t *testing.T) {
	e := NewEventEncoder("claude-sonnet-5.5")
	var buf []byte
	enc := func(f stream.Frame) {
		b, err := e.Encode(f)
		if err != nil {
			t.Fatalf("Encode 失败: %v", err)
		}
		buf = append(buf, b...)
	}
	enc(stream.Frame{Kind: stream.FrameTextDelta, Text: "你"})
	enc(stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: "想"})
	enc(stream.Frame{Kind: stream.FrameReasoningDelta, ThinkingSignature: "sig-1"})
	enc(stream.Frame{Kind: stream.FrameToolCallArgsDelta, ToolCallID: "tu_1", Text: "f", Arguments: `{"x":`})
	enc(stream.Frame{Kind: stream.FrameToolCallArgsDelta, ToolCallID: "tu_1", Arguments: `1}`})
	enc(stream.Frame{Kind: stream.FrameUsage, Usage: &core.Usage{InputTokens: 5, OutputTokens: 2}})
	enc(stream.Frame{Kind: stream.FrameDone, Status: "tool_use"})
	s := string(buf)
	for _, want := range []string{
		"event: message_start", "event: content_block_start", "text_delta",
		"thinking_delta", "signature_delta", "input_json_delta",
		"event: message_delta", `"stop_reason":"tool_use"`, `"input_tokens":5`,
		"event: message_stop", `"output_tokens":2`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("事件流缺 %q；实际尾部:\n%s", want, s[len(s)-300:])
		}
	}
	// Finish 幂等：不再追加。
	b, err := e.Finish()
	if err != nil || len(b) != 0 {
		t.Fatalf("Finish 应幂等空返回: %d %v", len(b), err)
	}
}

// TestStopReasonMapping 断言 stop_reason 超集映射（Chat 家族值翻译、Anthropic 原值透传）。
func TestStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use",
		"content_filter": "refusal", "end_turn": "end_turn", "stop_sequence": "stop_sequence",
		"": "end_turn", "unknown_x": "end_turn",
	}
	for in, want := range cases {
		if got := stopReasonOf(in); got != want {
			t.Errorf("stopReasonOf(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestDecodeRequestKeepsMultipleCacheMarks 钉 CAC-12：多个不同 cache_control 断点
// 必须按**位置**保留（system 第 0 块 / 消息第 0 条第 0 块 / 工具第 0 个），
// ⛔ 不许静默取一，也不再报错。
func TestDecodeRequestKeepsMultipleCacheMarks(t *testing.T) {
	body := `{"model":"m","max_tokens":1,
	  "system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}],
	  "messages":[{"role":"user","content":[{"type":"text","text":"b","cache_control":{"type":"ephemeral","ttl":"5m"}}]}],
	  "tools":[{"name":"f","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("多断点应被接受（CAC-12）: %v", err)
	}
	if len(req.CacheMarks) != 3 {
		t.Fatalf("CacheMarks = %d，期望 3: %+v", len(req.CacheMarks), req.CacheMarks)
	}
	want := []struct {
		where string
		mi    int
		pi    int
	}{
		{"system", 0, 0},
		{"messages", 1, 0},
		{"tools", 0, 0},
	}
	for i, w := range want {
		mk := req.CacheMarks[i]
		if mk.Where != w.where || mk.MessageIndex != w.mi || mk.PartIndex != w.pi {
			t.Fatalf("mark[%d] = %+v，期望 %+v", i, mk, w)
		}
	}
	// 兼容视图：CacheControl = 最后一个断点（工具上的那个 ephemeral，无 ttl）
	if req.CacheControl == nil || req.CacheControl.TTL != "" {
		t.Fatalf("CacheControl 兼容视图失真: %+v", req.CacheControl)
	}
}
