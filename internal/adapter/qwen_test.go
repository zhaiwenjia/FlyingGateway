// 日期: 2026-11-14 17:58:58.750
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件覆盖 Qwen / DashScope 适配器两形态（统一 SDD 4.3.7）：
// ① 兼容层（OpenAI Responses 形，2026-11-16 真机直采 4/4 全通）：POST {base}/responses、
//    请求体 Responses 形（input 数组 / instructions / tools 扁平形）、
//    enable_thinking / enable_search 经 Extra 透传、reasoning.effort 透传、
//    tool_choice 同 Chat 语义（auto / none / required / 指定函数名直通）、
//    previous_response_id 支持（服务端会话 7 天）⇒ 直通、usage 归一（4.4.2 Qwen 兼容行）、
//    流式真实样本直喂（testdata/real/qwen_responses_stream_real.sse）；
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

// TestQwenCompatBuildRequestResponses 钉住兼容层 Responses 请求（2026-11-16 实测口径）：
// ① POST {base}/responses + input 数组 + instructions 顶层 + tools 扁平形 +
//
//	max_output_tokens / prompt_cache_key / reasoning.effort 透传 + previous_response_id 直通；
//
// ② enable_thinking / enable_search 经 Extra 透传；流式 stream:true（usage 由
//
//	response.completed 自带，⛔ 无 stream_options）。
//
// 错误实现会红：仍发 /chat/completions ⇒ URL 断言红；消息塞 messages[] ⇒ input 断言红；
// Extra 被丢 ⇒ enable_thinking 断言红；照搬 chat 形 stream_options ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenCompatBuildRequestResponses -v
func TestQwenCompatBuildRequestResponses(t *testing.T) {
	a := NewQwen(http.DefaultClient)
	r := &core.Request{
		Messages: []core.Message{
			{Role: "system", Content: []core.ContentPart{{Text: "系统提示"}}},
			{Role: "user", Content: []core.ContentPart{{Text: "你好"}}},
		},
		MaxOutputTokens:    256,
		PromptCacheKey:     "pk-qwen-1",
		PreviousResponseID: "resp_prev",
		Reasoning:          &core.ReasoningConfig{Effort: "high"},
		Stream:             true,
		Tools:              []core.Tool{{Type: "function", Name: "search", Description: "搜索", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:         &core.ToolChoice{Mode: "auto"},
		Extra:              map[string]any{"enable_thinking": true, "enable_search": false},
	}
	out, err := a.BuildRequest(context.Background(), r, newQwenResolution("https://dashscope.aliyuncs.com/compatible-mode/v1"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.Method != "POST" || out.URL != "https://dashscope.aliyuncs.com/compatible-mode/v1/responses" {
		t.Fatalf("URL/方法 = %s %s，期望 POST {base}/responses", out.Method, out.URL)
	}
	m := bodyMap(t, out)
	if mustString(t, m, "model") != "qwen3.8-max" {
		t.Fatalf("model = %v", m["model"])
	}
	// instructions 顶层 + input 数组（⛔ 不是 messages[]）。
	if mustString(t, m, "instructions") != "系统提示" {
		t.Fatalf("instructions = %v，期望系统消息并入顶层", m["instructions"])
	}
	items := mustSlice(t, m, "input")
	if len(items) != 1 || items[0].(map[string]any)["role"] != "user" {
		t.Fatalf("input = %v，期望单条 user item", items)
	}
	if mustNumber(t, m, "max_output_tokens") != 256 {
		t.Fatalf("max_output_tokens = %v，期望 256（Responses 形字段名）", m["max_output_tokens"])
	}
	if mustAt(t, m, "stream") != true {
		t.Fatalf("stream = %v，期望 true", m["stream"])
	}
	// ⛔ 无 stream_options：Responses 的 usage 由 response.completed 自带。
	assertNoKey(t, m, "stream_options")
	// tools 扁平形（type/name 顶层，⛔ 不是 chat 形 function 嵌套）。
	t0 := mustSlice(t, m, "tools")[0].(map[string]any)
	if t0["type"] != "function" || t0["name"] != "search" {
		t.Fatalf("tools[0] = %v，期望扁平形 {type:function,name:search}", t0)
	}
	if mustString(t, m, "tool_choice") != "auto" {
		t.Fatalf("tool_choice = %v，期望 auto", m["tool_choice"])
	}
	// reasoning.effort 五档透传（4.3.7）。
	if mustString(t, m, "reasoning", "effort") != "high" {
		t.Fatalf("reasoning 对象不对: %v", m["reasoning"])
	}
	// quirks 经 Extra 透传（4.3.7 非标准参数）。
	if mustAt(t, m, "enable_thinking") != true {
		t.Fatalf("enable_thinking = %v，期望 true（4.3.7 非标准参数）", m["enable_thinking"])
	}
	if mustAt(t, m, "enable_search") != false {
		t.Fatalf("enable_search = %v，期望 false", m["enable_search"])
	}
	// prompt_cache_key / previous_response_id 始终透传（3.5；Qwen 支持 ⇒ 不报错）。
	if mustString(t, m, "prompt_cache_key") != "pk-qwen-1" {
		t.Fatalf("prompt_cache_key = %v", m["prompt_cache_key"])
	}
	if mustString(t, m, "previous_response_id") != "resp_prev" {
		t.Fatalf("previous_response_id 未透传: %v", m["previous_response_id"])
	}
	if !a.SupportsPreviousResponseID() {
		t.Fatalf("Qwen Responses 支持 previous_response_id（服务端会话 7 天，官方「创建响应」）")
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
	assertNoKey(t, m, "stream")
}

// TestQwenCompatToolChoicePassthrough 钉住 tool_choice 同 Chat 语义（保持现状语义）：
// auto / none / required 原样直通、指定函数名出 {type:function,name} 形、
// function 无名退 auto（与 chat 形映射逐项一致）。
// 错误实现会红：required 被误降 ⇒ 断言红；函数名不带 name ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenCompatToolChoicePassthrough -v
func TestQwenCompatToolChoicePassthrough(t *testing.T) {
	a := NewQwen(http.DefaultClient)
	res := newQwenResolution("https://dashscope.aliyuncs.com/compatible-mode/v1")
	for _, tc := range []struct {
		in   *core.ToolChoice
		want any
	}{
		{&core.ToolChoice{Mode: "auto"}, "auto"},
		{&core.ToolChoice{Mode: "none"}, "none"},
		{&core.ToolChoice{Mode: "required"}, "required"},
		{&core.ToolChoice{Mode: "function", Name: "search"}, map[string]any{"type": "function", "name": "search"}},
		{&core.ToolChoice{Mode: "function"}, "auto"},
	} {
		r := &core.Request{
			Messages:   []core.Message{{Role: "user", Content: []core.ContentPart{{Text: "hi"}}}},
			ToolChoice: tc.in,
		}
		out, err := a.BuildRequest(context.Background(), r, res)
		if err != nil {
			t.Fatalf("BuildRequest(%+v) 报错: %v", tc.in, err)
		}
		got := bodyMap(t, out)["tool_choice"]
		wantJSON, _ := json.Marshal(tc.want)
		gotJSON, _ := json.Marshal(got)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("tool_choice(%+v) = %s，期望 %s", tc.in, gotJSON, wantJSON)
		}
	}
}

// TestQwenCompatParseResponseRealSample 钉住真实样本直喂（2026-11-16 真机直采 qwen3.8-flash，
// 来源见 testdata/real/README.md）上 ParseResponse 成立：reasoning item 的 summary 保留、
// message 文本逐字、finish 由 status 推导（completed ⇒ stop / 有工具调用 ⇒ tool_calls）、
// usage 归一（Responses 树 input_tokens_details，x_details 扩展不干扰）。
// 错误实现会红：丢 summary ⇒ 断言红；x_details 干扰解析 ⇒ usage 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenCompatParseResponseRealSample -v
func TestQwenCompatParseResponseRealSample(t *testing.T) {
	a := NewQwen(http.DefaultClient)
	out, err := a.ParseResponse(newQwenResolution(""), loadFixture(t, "real/qwen_responses_basic_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if len(out.OutputItems) != 2 || out.OutputItems[0].Type != "reasoning" {
		t.Fatalf("OutputItems 顺序/类型不对: %+v（⛔ 不假设 output[0] 是 message）", out.OutputItems)
	}
	if len(out.OutputItems[0].Summary) != 1 ||
		!strings.HasPrefix(out.OutputItems[0].Summary[0].Text, `用户说了"说好"，这是一个简短的中文回复。`) {
		t.Fatalf("reasoning summary 丢失/不对: %+v", out.OutputItems[0].Summary)
	}
	const wantText = "好的 👌 有什么需要我帮忙的吗？"
	if out.OutputItems[1].Type != "message" || out.Text != wantText {
		t.Fatalf("message 文本 = %q，期望逐字 %q", out.Text, wantText)
	}
	if out.FinishReason != core.FinishStop || out.Status != core.StatusCompleted {
		t.Fatalf("finish/status = %q/%q，期望 stop/completed", out.FinishReason, out.Status)
	}
	u := out.Usage
	if u == nil || u.InputTokens != 94 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 ||
		u.OutputTokens != 137 || u.ReasoningTokens != 124 || u.TotalTokens != 231 {
		t.Fatalf("usage 归一 = %+v，期望 In94/Read0/Write0/Out137/Reason124/Total231（样本逐字）", u)
	}

	// reasoning 样本：message 文本逐字 + usage（In106/Out118/Reason91/Total224）。
	out2, err := a.ParseResponse(newQwenResolution(""), loadFixture(t, "real/qwen_responses_reasoning_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse(reasoning 样本) 报错: %v", err)
	}
	const wantText2 = "**9.9** 更大。9.9 = 9.90 > 9.11。"
	if out2.Text != wantText2 {
		t.Fatalf("message 文本 = %q，期望逐字 %q", out2.Text, wantText2)
	}
	if u := out2.Usage; u == nil || u.InputTokens != 106 || u.OutputTokens != 118 || u.ReasoningTokens != 91 || u.TotalTokens != 224 {
		t.Fatalf("usage 归一 = %+v，期望 In106/Out118/Reason91/Total224（样本逐字）", u)
	}
}

// TestQwenCompatParseResponseToolCallRealSample 钉住真实 tools 样本（扁平工具形出 function_call
// item，2026-11-16 真机直采）上函数名 / 参数 / 调用 id 逐字进 core.OutputItem，finish=tool_calls。
// 错误实现会红：name 不进 Name、call_id 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenCompatParseResponseToolCallRealSample -v
func TestQwenCompatParseResponseToolCallRealSample(t *testing.T) {
	a := NewQwen(http.DefaultClient)
	out, err := a.ParseResponse(newQwenResolution(""), loadFixture(t, "real/qwen_responses_tools_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if len(out.OutputItems) != 2 || out.OutputItems[1].Type != "function_call" {
		t.Fatalf("OutputItems = %+v，期望 reasoning + function_call", out.OutputItems)
	}
	tc := out.OutputItems[1]
	if tc.Name != "get_weather" || tc.Arguments != `{"city": "北京"}` ||
		tc.CallID != "call_0c62a2295f62452e876e6905" || tc.ID != "fc_cb37f532-01ff-4405-9e10-69d3f2e4c11d" {
		t.Fatalf("function_call item = %+v，期望 name/arguments/call_id 逐字（样本）", tc)
	}
	if out.FinishReason != core.FinishToolCalls {
		t.Fatalf("FinishReason = %q，期望 tool_calls", out.FinishReason)
	}
	if u := out.Usage; u == nil || u.InputTokens != 339 || u.OutputTokens != 68 || u.ReasoningTokens != 40 || u.TotalTokens != 407 {
		t.Fatalf("usage 归一 = %+v，期望 In339/Out68/Reason40/Total407（样本逐字）", u)
	}
}

// TestQwenStreamRealSample 钉住真实流样本直喂（2026-11-16 真机逐帧录制；真实 wire 是
// `data:{json}` 冒号无空格 + `:HTTP_STATUS/200` 注释行 + `id:` 行 + 无 [DONE]、
// response.incomplete 收口，来源见 testdata/real/README.md）：
// 帧序列 = lifecycle×2 + 推理增量×78 + usage + done = 82 帧；usage（In94/Out256/Reason256/Total350）
// 先于 done（incomplete）。
// 错误实现会红：只认 `data: {`（带空格）⇒ 帧全丢 ⇒ 计数红；usage/done 顺序颠倒 ⇒ 帧序红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenStreamRealSample -v
func TestQwenStreamRealSample(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(loadFixture(t, "real/qwen_responses_stream_real.sse"))
	}))
	defer srv.Close()

	a := NewQwen(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newQwenResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v（无 [DONE]，response.incomplete 收口）", cerr)
	}
	if len(frames) != 82 {
		t.Fatalf("帧数 = %d，期望 82（lifecycle×2 + 推理×78 + usage + done）", len(frames))
	}
	for i, want := range []string{"response.created", "response.in_progress"} {
		if frames[i].Kind != stream.FrameLifecycle || frames[i].Status != want {
			t.Fatalf("帧[%d] = %+v，期望 lifecycle %s", i, frames[i], want)
		}
	}
	// 推理增量（样本逐字前三帧）。
	for i, want := range []string{"用户", "说“", "说好”，"} {
		f := frames[2+i]
		if f.Kind != stream.FrameReasoningDelta || f.Reasoning != want {
			t.Fatalf("帧[%d] = %+v，期望推理增量 %q（样本逐字）", 2+i, f, want)
		}
	}
	if frames[79].Kind != stream.FrameReasoningDelta {
		t.Fatalf("帧[79] = %+v，期望推理增量（末个）", frames[79])
	}
	// 尾两帧：usage（response.incomplete 自带）→ done incomplete，usage 先于 done。
	u := frames[80]
	if u.Kind != stream.FrameUsage || u.Usage.InputTokens != 94 || u.Usage.OutputTokens != 256 ||
		u.Usage.ReasoningTokens != 256 || u.Usage.TotalTokens != 350 {
		t.Fatalf("帧[80] = %+v，期望 usage（In94/Out256/Reason256/Total350，样本逐字）", u)
	}
	d := frames[81]
	if d.Kind != stream.FrameDone || d.Status != core.StatusIncomplete {
		t.Fatalf("帧[81] = %+v，期望 done incomplete", d)
	}
}

// TestQwenStreamToolCallName 钉住 Responses 形流式工具调用名进帧（帧承载约定：
// FrameToolCallArgsDelta.Text 载函数名、ToolCallID 载调用 id）：output_item.added 的
// item.name 必须以补名帧（ToolCallID=call_abc、Text=search）进入帧序列，参数增量帧照常到
// （partial_json ⇒ Arguments）、completed 收口（⛔ 无 [DONE] 也必须收口）。
// 错误实现会红：不发补名帧 ⇒ named=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestQwenStreamToolCallName -v
func TestQwenStreamToolCallName(t *testing.T) {
	const sse = "" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"item_id\":\"fc_1\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_abc\",\"name\":\"search\"}}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":1,\"item_id\":\"fc_1\",\"partial_json\":\"{\\\"q\\\":1}\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"usage\":{\"input_tokens\":339,\"output_tokens\":68,\"total_tokens\":407}}}\n\n"
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
		t.Fatalf("流式终态错误: %v（⛔ 无 [DONE]，completed 必须收口）", cerr)
	}
	var named, args int
	var doneStatus string
	for _, f := range frames {
		if f.Kind == stream.FrameToolCallArgsDelta && f.ToolCallID == "call_abc" {
			if f.Text == "search" {
				named++
			}
			if f.Arguments == "{\"q\":1}" {
				args++
			}
		}
		if f.Kind == stream.FrameDone {
			doneStatus = f.Status
		}
	}
	if named == 0 {
		t.Fatalf("缺补名帧（ToolCallID=call_abc 且 Text=search）：%+v", frames)
	}
	if args == 0 {
		t.Fatalf("参数帧缺失（partial_json={\"q\":1}）：%+v", frames)
	}
	if doneStatus != core.StatusCompleted {
		t.Fatalf("done 状态 = %q，期望 completed", doneStatus)
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
