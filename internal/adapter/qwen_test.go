// 日期: 2026-11-14 17:58:58.750
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件覆盖 Qwen / DashScope 适配器两形态（统一 SDD 4.3.7）：
// ① 兼容层（Chat 形，A/B 实测裁定全部走 Chat Completions）：/chat/completions、
//    enable_thinking / enable_search 经 Extra 透传、stream_options.include_usage、
//    usage 的 cached_tokens + cache_creation_input_tokens（4.4.2 Qwen 兼容行）、
//    流式延后收口（usage 在末帧的两拍形态）、previous_response_id 显式 400；
// ② 原生 text-generation：/api/v1/services/aigc/text-generation/generation、
//    流式须加头 X-DashScope-SSE: enable（3.6 坑 1）、帧 `data:{json}` 冒号无空格、
//    每帧自带 usage、无 [DONE]、usage 字段 input_tokens/output_tokens（⛔ 与兼容模式不同）。

func newQwenResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderQwen,
		Shape:    core.APIShapeOpenAIChat,
		BaseURL:  base,
		Model:    "qwen3.8-max",
	}
}

func newQwenNativeResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderQwen,
		Shape:    core.APIShapeQwenNative,
		BaseURL:  base,
		Model:    "qwen3.8-max",
	}
}

// TestQwenCompatBuildRequest 钉住兼容层请求（Chat 形，4.3.7）：
// ① POST {base}/chat/completions + messages[] + max_tokens + stream + stream_options.include_usage
//   - tools（chat 形 function 对象）+ tool_choice；
//
// ② enable_thinking / enable_search 经 Extra 透传；prompt_cache_key 若请求携带则带在 body。
// 错误实现会红：仍发 /responses ⇒ URL 断言红；Extra 被丢 ⇒ enable_thinking 断言红；
// 漏发 stream_options ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenCompatBuildRequest -v
func TestQwenCompatBuildRequest(t *testing.T) {
	a := NewQwen(http.DefaultClient)
	r := &core.Request{
		Messages:        []core.Message{{Role: "user", Content: []core.ContentPart{{Text: "你好"}}}},
		MaxOutputTokens: 256,
		PromptCacheKey:  "pk-qwen-1",
		Stream:          true,
		Tools:           []core.Tool{{Type: "function", Name: "search", Description: "搜索", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:      &core.ToolChoice{Mode: "auto"},
		Extra:           map[string]any{"enable_thinking": true, "enable_search": false},
	}
	out, err := a.BuildRequest(context.Background(), r, newQwenResolution("https://dashscope.aliyuncs.com/compatible-mode/v1"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions" {
		t.Fatalf("URL = %s，期望 {base}/chat/completions", out.URL)
	}
	m := bodyMap(t, out)
	if mustString(t, m, "model") != "qwen3.8-max" {
		t.Fatalf("model = %v", m["model"])
	}
	msgs := mustSlice(t, m, "messages")
	if len(msgs) != 1 || mustString(t, msgs[0].(map[string]any), "role") != "user" {
		t.Fatalf("messages = %v，期望单条 user（chat 形 messages[]）", msgs)
	}
	if mustNumber(t, m, "max_tokens") != 256 {
		t.Fatalf("max_tokens = %v，期望 256（chat 形普通字段）", m["max_tokens"])
	}
	if mustAt(t, m, "stream") != true {
		t.Fatalf("stream = %v，期望 true", m["stream"])
	}
	so := mustAt(t, m, "stream_options").(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("stream_options = %v，期望 include_usage:true（Qwen chat usage 在末帧）", so)
	}
	tools := mustSlice(t, m, "tools")
	t0 := tools[0].(map[string]any)
	if fn := t0["function"].(map[string]any); fn["name"] != "search" {
		t.Fatalf("tools[0] = %v，期望 chat 形 function 对象 name=search", t0)
	}
	if mustString(t, m, "tool_choice") != "auto" {
		t.Fatalf("tool_choice = %v，期望 auto", m["tool_choice"])
	}
	// quirks 经 Extra 透传（4.3.7 非标准参数）。
	if mustAt(t, m, "enable_thinking") != true {
		t.Fatalf("enable_thinking = %v，期望 true（4.3.7 非标准参数）", m["enable_thinking"])
	}
	if mustAt(t, m, "enable_search") != false {
		t.Fatalf("enable_search = %v，期望 false", m["enable_search"])
	}
	// prompt_cache_key 若请求携带则带在 body（兼容层忽略无害）。
	if mustString(t, m, "prompt_cache_key") != "pk-qwen-1" {
		t.Fatalf("prompt_cache_key = %v", m["prompt_cache_key"])
	}
	sch := a.AuthScheme()
	if sch.HeaderName != "Authorization" || sch.Prefix != "Bearer " {
		t.Fatalf("AuthScheme = %+v", sch)
	}

	// 非流式 ⇒ ⛔ 不发 stream / stream_options。
	r.Stream = false
	out, err = a.BuildRequest(context.Background(), r, newQwenResolution("https://dashscope.aliyuncs.com/compatible-mode/v1"))
	if err != nil {
		t.Fatalf("BuildRequest(非流式) 报错: %v", err)
	}
	m = bodyMap(t, out)
	assertNoKey(t, m, "stream_options")
	if v, ok := m["stream"]; ok && v == true {
		t.Fatalf("非流式不应发 stream:true: %v", m["stream"])
	}
}

// TestQwenCompatPreviousResponseIDRejected 钉住 3.5：chat 形 wire 无 previous_response_id
// ⇒ SupportsPreviousResponseID=false，携带即显式 400 unsupported_capability（⛔ 不静默丢弃）。
// 错误实现会红：声明 true 或静默丢弃 ⇒ 断言 / requireDetailErr 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenCompatPreviousResponseIDRejected -v
func TestQwenCompatPreviousResponseIDRejected(t *testing.T) {
	a := NewQwen(http.DefaultClient)
	if a.SupportsPreviousResponseID() {
		t.Fatalf("Qwen chat 形不支持 previous_response_id（wire 无此字段）")
	}
	_, err := a.BuildRequest(context.Background(), &core.Request{PreviousResponseID: "resp_prev"},
		newQwenResolution("https://dashscope.aliyuncs.com/compatible-mode/v1"))
	requireDetailErr(t, err, "unsupported_capability")
}

// TestQwenCompatParseResponseChatShape 钉住 chat.completion 解析：message.content /
// reasoning_content 进推理正文位 / tool_calls 函数名进 core.OutputItem.Name /
// finish_reason 超集原值 / usage 经 usage.Normalize(Qwen)。
// 错误实现会红：name 不进 Name ⇒ 断言红；finish_reason 被归并改写 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenCompatParseResponseChatShape -v
func TestQwenCompatParseResponseChatShape(t *testing.T) {
	const body = `{"id":"chatcmpl-6ada","model":"qwen3.8-max","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"你好","reasoning_content":"思考中",` +
		`"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"search","arguments":"{\"q\":1}"}}]},` +
		`"finish_reason":"content_filter"}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":40,"total_tokens":140,` +
		`"prompt_tokens_details":{"cached_tokens":25,"cache_creation_input_tokens":10}}}`
	a := NewQwen(http.DefaultClient)
	out, err := a.ParseResponse(newQwenResolution("https://dashscope.aliyuncs.com/compatible-mode/v1"), []byte(body))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if out.Text != "你好" {
		t.Fatalf("Text = %q，期望 你好", out.Text)
	}
	if len(out.OutputItems) != 2 {
		t.Fatalf("OutputItems = %+v，期望 message + function_call", out.OutputItems)
	}
	if out.OutputItems[0].ReasoningText != "思考中" {
		t.Fatalf("ReasoningText = %q，期望 思考中（reasoning_content 进推理正文位）", out.OutputItems[0].ReasoningText)
	}
	tc := out.OutputItems[1]
	if tc.Type != "function_call" || tc.Name != "search" || tc.Arguments != "{\"q\":1}" || tc.CallID != "call_abc" {
		t.Fatalf("tool item = %+v，期望 Name=search（name 必须进 Name）", tc)
	}
	if out.FinishReason != "content_filter" {
		t.Fatalf("FinishReason = %q，期望超集原值 content_filter", out.FinishReason)
	}
	if out.Usage == nil || out.Usage.InputTokens != 100 || out.Usage.CacheReadTokens != 25 ||
		out.Usage.CacheWriteTokens != 10 || out.Usage.OutputTokens != 40 {
		t.Fatalf("usage 归一 = %+v，期望 In100/Read25/Write10/Out40（4.4.2 Qwen 兼容行）", out.Usage)
	}
}

// TestQwenNativeBuildRequest 钉住原生请求（4.3.7 / 3.6 坑 1）：
// 端点 /api/v1/services/aigc/text-generation/generation；流式必须加头 X-DashScope-SSE: enable；
// 请求体 model / input.messages / parameters（result_format=message）。
// 错误实现会红：缺头 ⇒ 头断言红；端点错拼 ⇒ URL 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenNativeBuildRequest -v
func TestQwenNativeBuildRequest(t *testing.T) {
	a := NewQwenNative(http.DefaultClient)
	r := &core.Request{
		Messages: []core.Message{
			{Role: "system", Content: []core.ContentPart{{Text: "系统提示"}}},
			{Role: "user", Content: []core.ContentPart{{Text: "你好"}}},
		},
		Stream:          true,
		MaxOutputTokens: 100,
	}
	out, err := a.BuildRequest(context.Background(), r, newQwenNativeResolution("https://qwen-cn-beijing.maas.aliyuncs.com"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "https://qwen-cn-beijing.maas.aliyuncs.com/api/v1/services/aigc/text-generation/generation" {
		t.Fatalf("URL = %s，期望原生 text-generation 端点", out.URL)
	}
	// 流式必须加头（3.6 坑 1）。
	if out.Headers["X-DashScope-SSE"] != "enable" {
		t.Fatalf("头 X-DashScope-SSE = %q，期望 enable", out.Headers["X-DashScope-SSE"])
	}
	m := bodyMap(t, out)
	if mustString(t, m, "model") != "qwen3.8-max" {
		t.Fatalf("model = %v", m["model"])
	}
	msgs := mustSlice(t, m, "input", "messages")
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("input.messages = %v，期望 system + user", msgs)
	}
	if mustAt(t, m, "parameters", "stream") != true {
		t.Fatalf("parameters.stream = %v，期望 true", m["parameters"])
	}
	if mustString(t, m, "parameters", "result_format") != "message" {
		t.Fatalf("parameters.result_format = %v", m["parameters"])
	}

	// 非流式 ⇒ ⛔ 无 X-DashScope-SSE 头。
	r.Stream = false
	out, err = a.BuildRequest(context.Background(), r, newQwenNativeResolution("https://qwen-cn-beijing.maas.aliyuncs.com"))
	if err != nil {
		t.Fatalf("BuildRequest(非流式) 报错: %v", err)
	}
	if _, ok := out.Headers["X-DashScope-SSE"]; ok {
		t.Fatalf("非流式不应带 X-DashScope-SSE 头: %v", out.Headers)
	}
}

// TestQwenNativeParseUsage 钉住原生 usage 字段名（4.3.7：input_tokens / output_tokens，
// ⛔ 与兼容模式的 prompt_tokens / completion_tokens 不同；无缓存明细 ⇒ 读写记 0）。
// 错误实现会红：按兼容字段名解 ⇒ In=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenNativeParseUsage -v
func TestQwenNativeParseUsage(t *testing.T) {
	a := NewQwenNative(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "qwen_native_usage.json"))
	if u.InputTokens != 17 || u.OutputTokens != 64 {
		t.Fatalf("归一不对：In=%d(17=input_tokens) Out=%d(64=output_tokens)", u.InputTokens, u.OutputTokens)
	}
	if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		t.Fatalf("原生无缓存明细 ⇒ 记 0：Read=%d Write=%d", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.TotalTokens != 81 {
		t.Fatalf("TotalTokens = %d，期望 81（原报保留）", u.TotalTokens)
	}
}

// TestQwenCompatParseUsage 钉住兼容层 usage（4.4.2 Qwen 兼容行）：
// 命中 = cached_tokens=25、写入 = cache_creation_input_tokens=10（两级都在 prompt_tokens_details）。
// 错误实现会红：写入丢 ⇒ Write=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenCompatParseUsage -v
func TestQwenCompatParseUsage(t *testing.T) {
	a := NewQwen(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "qwen_compat_usage.json"))
	if u.InputTokens != 100 || u.CacheReadTokens != 25 || u.CacheWriteTokens != 10 || u.OutputTokens != 40 {
		t.Fatalf("归一不对：In=%d(100) Read=%d(25) Write=%d(10) Out=%d(40)",
			u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens)
	}
	if u.InputTokensUncached != 65 {
		t.Fatalf("Uncached = %d，期望 65 = 100−25−10", u.InputTokensUncached)
	}
}

// TestQwenNativeStreamFrames 钉住原生流式（3.6 坑 1 / 4.3.7）：
// 帧格式 `data:{json}`（冒号后无空格）⇒ SSE 解析要容忍；每帧自带 usage；⛔ 无 [DONE] 哨兵；
// 终态以 output.finish_reason 判定。请求头 X-DashScope-SSE: enable 随真实 HTTP 核验。
// 错误实现会红：解析器只认 `data: {`（带空格）⇒ 帧全丢 ⇒ 帧数断言红；无 [DONE] 判不了终态 ⇒ Err 非 nil ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenNativeStreamFrames -v
func TestQwenNativeStreamFrames(t *testing.T) {
	const body = "" +
		"data:{\"output\":{\"text\":\"你\"},\"usage\":{\"input_tokens\":5,\"output_tokens\":1,\"total_tokens\":6}}\n\n" +
		"data:{\"output\":{\"text\":\"好\",\"finish_reason\":\"stop\"},\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7}}\n\n"
	gotHeader := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotHeader = req.Header.Get("X-DashScope-SSE")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	a := NewQwenNative(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newQwenNativeResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v（⛔ 无 [DONE] 哨兵，以 finish_reason 判终态）", cerr)
	}
	if gotHeader != "enable" {
		t.Fatalf("上游收到 X-DashScope-SSE = %q，期望 enable（3.6 坑 1）", gotHeader)
	}
	// [text 你, usage, text 好, usage, done stop]
	if len(frames) != 5 {
		t.Fatalf("帧数 = %d，期望 5（每帧自带 usage）：%+v", len(frames), frames)
	}
	if frames[0].Kind != stream.FrameTextDelta || frames[0].Text != "你" {
		t.Fatalf("帧[0] = %+v，期望 text 你（data:{json} 无空格帧）", frames[0])
	}
	if frames[1].Kind != stream.FrameUsage || frames[1].Usage.OutputTokens != 1 {
		t.Fatalf("帧[1] = %+v，期望第 1 帧 usage", frames[1])
	}
	if frames[2].Kind != stream.FrameTextDelta || frames[2].Text != "好" {
		t.Fatalf("帧[2] = %+v", frames[2])
	}
	if frames[3].Kind != stream.FrameUsage || frames[3].Usage.OutputTokens != 2 {
		t.Fatalf("帧[3] = %+v，期望第 2 帧 usage（每帧自带）", frames[3])
	}
	if frames[4].Kind != stream.FrameDone || frames[4].Status != "stop" {
		t.Fatalf("帧[4] = %+v，期望 done/stop", frames[4])
	}
}

// TestQwenStreamToolCallName 钉住兼容层（Chat 形）流式工具调用名进帧（帧承载约定：
// FrameToolCallArgsDelta.Text 载函数名、ToolCallID 载调用 id）：delta.tool_calls 首块的
// function.name 必须以补名帧（ToolCallID=call_abc、Text=search）进入帧序列，参数帧照常到
// （ToolCallID 一致）。
// 错误实现会红：不发补名帧 ⇒ 帧序列里只有 Text 为空的参数帧 ⇒ named=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenStreamToolCallName -v
func TestQwenStreamToolCallName(t *testing.T) {
	const sse = "" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_abc\",\"type\":\"function\",\"function\":{\"name\":\"search\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"q\\\":1}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":null}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewQwen(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newQwenResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	var named, args int
	for _, f := range frames {
		if f.Kind == stream.FrameToolCallArgsDelta && f.ToolCallID == "call_abc" {
			if f.Text == "search" {
				named++
			}
			if f.Arguments == "{\"q\":1}" {
				args++
			}
		}
	}
	if named == 0 {
		t.Fatalf("缺补名帧（ToolCallID=call_abc 且 Text=search）：%+v", frames)
	}
	if args == 0 {
		t.Fatalf("参数帧缺失（ToolCallID=call_abc 且 Arguments={\"q\":1}）：%+v", frames)
	}
}

// TestQwenStreamUsageBeforeDone 钉住延后收口的两拍形态（Qwen chat 的 usage 在末帧）：
// finish(usage:null) ⇒ 独立 usage 帧 ⇒ [DONE]。finish 即收口会把独立 usage 帧丢掉
// ⇒ usage 缺失被置 Estimated 估算（与 moonshot / xiaomi 同一口径的实测 bug）。
// 错误实现对照：提前收口 ⇒ 无 usage 帧或 Done 在 usage 前 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenStreamUsageBeforeDone -v
func TestQwenStreamUsageBeforeDone(t *testing.T) {
	const sse = "" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewQwen(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newQwenResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	ui, di := -1, -1
	for i, fr := range frames {
		if fr.Kind == stream.FrameUsage {
			ui = i
			if fr.Usage == nil || fr.Usage.InputTokens != 10 || fr.Usage.OutputTokens != 5 {
				t.Fatalf("usage 帧 = %+v，期望 10/5（独立 usage 帧逐字）", fr.Usage)
			}
		}
		if fr.Kind == stream.FrameDone {
			di = i
			if fr.Status != "stop" {
				t.Fatalf("done 状态 = %q，期望 stop 超集原值", fr.Status)
			}
		}
	}
	if ui < 0 {
		t.Fatal("两拍形态未产出 usage 帧（独立 usage 帧被丢）")
	}
	if di < 0 || ui > di {
		t.Fatalf("usage 帧(%d) 必须先于 Done 帧(%d)", ui, di)
	}
}

// TestQwenDeferredDoneAtSentinel 钉住延后收口的 [DONE] 补收口（无 usage 流终止帧不丢）：
// finish(usage:null) 不立即收口；[DONE] 处补发 Done(stop)。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenDeferredDoneAtSentinel -v
func TestQwenDeferredDoneAtSentinel(t *testing.T) {
	a := NewQwen(http.DefaultClient)
	d := a.(*qwenAdapter).streamDecoder()
	fs, done, _ := d.decode(stream.SSEEvent{Data: `{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`})
	if done || len(fs) != 0 {
		t.Fatalf("finish 帧不应终态（usage 可能在其后），实际 done=%v frames=%d", done, len(fs))
	}
	fs, done, _ = d.decode(stream.SSEEvent{Data: `[DONE]`})
	if !done || len(fs) != 1 || fs[0].Kind != stream.FrameDone || fs[0].Status != "stop" {
		t.Fatalf("[DONE] 补收口 = %+v done=%v，期望恰一帧 Done(stop)", fs, done)
	}
}

// TestQwenParseUsageRealSample 钉住真实样本（阿里云 Context Cache 文档 Cache hit examples：
// OpenAI 兼容形 chatcmpl-6ada9ed2-…、DashScope 原生形 f3acaa33-…，来源见 testdata/real/README.md）上
// ParseUsage 归一成立：兼容形命中 2048、Uncached=3019−2048=971；
// 原生形同样命中 2048（⚠️ 2026-11-14 实测修正：原生样本带 prompt_tokens_details.cached_tokens=2048，
// SDD 4.4.2「无缓存字段」是旧口径 ⇒ 已改为有字段就解析）。
// 错误实现会红：兼容形把命中计进 Uncached ⇒ 971 ≠ 3019 红；原生形丢 cached 字段 ⇒ 2048 ≠ 0 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenParseUsageRealSample -v
func TestQwenParseUsageRealSample(t *testing.T) {
	a := NewQwen(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/qwen_compat_usage.json"))
	if u.InputTokens != 3019 || u.CacheReadTokens != 2048 || u.InputTokensUncached != 971 ||
		u.OutputTokens != 104 || u.TotalTokens != 3123 {
		t.Fatalf("兼容形归一 = In%d/Read%d/Unc%d/Out%d/Total%d，期望 3019/2048/971/104/3123（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.InputTokensUncached, u.OutputTokens, u.TotalTokens)
	}
	n := NewQwenNative(http.DefaultClient)
	un := n.ParseUsage(loadFixture(t, "real/qwen_native_usage.json"))
	if un.InputTokens != 3019 || un.CacheReadTokens != 2048 || un.InputTokensUncached != 971 ||
		un.OutputTokens != 101 || un.TotalTokens != 3120 {
		t.Fatalf("原生形归一 = In%d/Read%d/Unc%d/Out%d/Total%d，期望 3019/2048/971/101/3120（样本逐字）",
			un.InputTokens, un.CacheReadTokens, un.InputTokensUncached, un.OutputTokens, un.TotalTokens)
	}
}
