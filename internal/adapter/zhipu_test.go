// 日期: 2026-11-14 16:38:51.679
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件覆盖智谱 GLM 适配器（统一 SDD 4.3.8；出站 OpenAI Responses）：
// BuildRequest（URL base 后缀重写 /api/paas/v4 → /api/v1 + /responses、Responses 形请求体
// input 数组 / instructions / tools 扁平形、previous_response_id 直通、Extra 透传）、
// tool_choice 降级（官方 enum 仅 none/auto ⇒ required / 指定函数名降为 auto 并记 Degradation）、
// ParseResponse（真实样本 testdata/real/zhipu_responses_*_real.json 直喂；sensitive / network_error /
// model_context_window_exceeded 非标准 finish_reason 超集原值）、ParseUsage（命中单列、无写入）、
// 流式（真实样本 zhipu_responses_stream_real.sse 直喂 + Responses 形工具补名帧）。

func newZhipuResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderZhipu,
		Shape:    core.APIShapeOpenAIResponses,
		BaseURL:  base,
		Model:    "glm-5.3-flash",
	}
}

// TestZhipuBuildRequestResponses 钉住智谱 Responses 出站请求（2026-11-16 实测口径）：
// ① URL 后缀重写：chat 形 base …/api/paas/v4 ⇒ …/api/v1/responses（同 base 的 /responses
//
//	实测 4×404，/api/v1/responses 同一把普通 key 4/4 全通）；其它 base 原样拼 /responses。
//
// ② 请求体 Responses 形：input 数组 / instructions 顶层 / tools 扁平形；
//
//	previous_response_id 支持 ⇒ 直通不报错（服务端会话 store=true、7 天，官方）；
//	thinking.type 经 Extra 透传。
//
// 错误实现会红：URL 不重写 ⇒ 断言红；消息塞 messages[] ⇒ input 断言红；Extra 被丢 ⇒ thinking 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuBuildRequestResponses -v
func TestZhipuBuildRequestResponses(t *testing.T) {
	a := NewZhipu(http.DefaultClient)
	r := &core.Request{
		Instructions: "主指令",
		Messages: []core.Message{
			{Role: "system", Content: []core.ContentPart{{Text: "系统提示"}}},
			{Role: "user", Content: []core.ContentPart{{Text: "你好"}}},
		},
		Tools:              []core.Tool{{Type: "function", Name: "get_weather", Description: "查天气", Parameters: []byte(`{"type":"object"}`)}},
		MaxOutputTokens:    1024,
		PromptCacheKey:     "pk-1",
		PreviousResponseID: "resp_prev",
		Reasoning:          &core.ReasoningConfig{Effort: "high"},
		Extra:              map[string]any{"thinking": map[string]any{"type": "enabled"}},
	}
	// ① chat 形 base（/api/paas/v4）⇒ 后缀重写 /api/v1 再拼 /responses。
	out, err := a.BuildRequest(context.Background(), r, newZhipuResolution("https://open.bigmodel.cn/api/paas/v4"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.Method != "POST" || out.URL != "https://open.bigmodel.cn/api/v1/responses" {
		t.Fatalf("URL/方法 = %s %s，期望 POST https://open.bigmodel.cn/api/v1/responses（base 后缀重写）", out.Method, out.URL)
	}
	if out.Headers["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type = %q", out.Headers["Content-Type"])
	}
	// 其它 base 原样拼 /responses（含尾斜杠容忍）。
	for _, tc := range []struct{ base, want string }{
		{"https://open.bigmodel.cn/api/v1", "https://open.bigmodel.cn/api/v1/responses"},
		{"https://example.com/gw/", "https://example.com/gw/responses"},
	} {
		o, err := a.BuildRequest(context.Background(), r, newZhipuResolution(tc.base))
		if err != nil {
			t.Fatalf("BuildRequest(%s) 报错: %v", tc.base, err)
		}
		if o.URL != tc.want {
			t.Fatalf("base %s ⇒ URL = %s，期望 %s（非 chat base 原样拼接）", tc.base, o.URL, tc.want)
		}
	}

	m := bodyMap(t, out)
	if mustString(t, m, "model") != "glm-5.3-flash" {
		t.Fatalf("model = %v", m["model"])
	}
	// instructions 顶层（core.Instructions + system 消息并入）。
	if got := mustString(t, m, "instructions"); got != "主指令\n系统提示" {
		t.Fatalf("instructions = %q，期望 %q", got, "主指令\n系统提示")
	}
	// input 数组（⛔ 不是 messages[]）。
	items := mustSlice(t, m, "input")
	if len(items) != 1 {
		t.Fatalf("input item 数 = %d，期望 1（system 并入 instructions）：%v", len(items), items)
	}
	u := items[0].(map[string]any)
	if u["type"] != "message" || u["role"] != "user" {
		t.Fatalf("input[0] = %v，期望 message/user", u)
	}
	up := u["content"].([]any)[0].(map[string]any)
	if up["type"] != "input_text" || up["text"] != "你好" {
		t.Fatalf("input[0].content[0] = %v，期望 input_text/你好", up)
	}
	// tools 扁平形（type/name 顶层，⛔ 不是 chat 形 function 嵌套）。
	t0 := mustSlice(t, m, "tools")[0].(map[string]any)
	if t0["type"] != "function" || t0["name"] != "get_weather" || t0["description"] != "查天气" {
		t.Fatalf("tools[0] = %v，期望扁平形 {type:function,name:get_weather}", t0)
	}
	if _, ok := t0["parameters"]; !ok {
		t.Fatalf("tools[0] 缺 parameters（原文透传）: %v", t0)
	}
	// prompt_cache_key / previous_response_id 始终透传（3.5；智谱支持 ⇒ 不报错）。
	if mustString(t, m, "prompt_cache_key") != "pk-1" {
		t.Fatalf("prompt_cache_key 未透传: %v", m["prompt_cache_key"])
	}
	if mustString(t, m, "previous_response_id") != "resp_prev" {
		t.Fatalf("previous_response_id 未透传: %v", m["previous_response_id"])
	}
	if !a.SupportsPreviousResponseID() {
		t.Fatalf("智谱 Responses 支持 previous_response_id（store=true、7 天，官方）")
	}
	if mustNumber(t, m, "max_output_tokens") != 1024 {
		t.Fatalf("max_output_tokens = %v，期望 1024", m["max_output_tokens"])
	}
	if mustString(t, m, "reasoning", "effort") != "high" {
		t.Fatalf("reasoning 对象不对: %v", m["reasoning"])
	}
	// thinking.type 经 Extra 透传（4.3.8 特异参数）。
	if mustString(t, m, "thinking", "type") != "enabled" {
		t.Fatalf("thinking = %v，期望 thinking.type=enabled（Extra 透传）", m["thinking"])
	}
	// 非流式 ⇒ ⛔ 不发 stream 键。
	assertNoKey(t, m, "stream")

	// 流式 ⇒ stream:true（usage 由 response.completed / incomplete 自带，⛔ 无 stream_options）。
	r.Stream = true
	out, err = a.BuildRequest(context.Background(), r, newZhipuResolution("https://open.bigmodel.cn/api/paas/v4"))
	if err != nil {
		t.Fatalf("BuildRequest(流式) 报错: %v", err)
	}
	m = bodyMap(t, out)
	if mustAt(t, m, "stream") != true {
		t.Fatalf("stream = %v，期望 true", m["stream"])
	}
	assertNoKey(t, m, "stream_options")
}

// TestZhipuToolChoiceDegradationResponses 钉住 1.4.2 不静默降级：智谱 Responses 的 tool_choice
// 官方 enum 仅 none/auto ⇒ required / 指定函数名降为 auto 并产生 Degradation 记录（⛔ 不静默
// 丢弃无标记）；none / auto 直通不降级。
// 错误实现对照：只降不记 ⇒ 本测试必红；none 被误降 ⇒ none 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuToolChoiceDegradationResponses -v
func TestZhipuToolChoiceDegradationResponses(t *testing.T) {
	a := NewZhipu(&http.Client{})
	res := newZhipuResolution("https://open.bigmodel.cn/api/paas/v4")
	base := &core.Request{
		Messages: []core.Message{{Role: "user", Content: []core.ContentPart{{Type: "text", Text: "hi"}}}},
		Tools:    []core.Tool{{Type: "function", Name: "get_weather"}},
	}
	// required / 指定函数名 ⇒ 降为 auto + degradation。
	for _, tc := range []*core.ToolChoice{{Mode: "required"}, {Mode: "function", Name: "get_weather"}} {
		r := *base
		r.ToolChoice = tc
		out, degs, err := a.(DegradationAware).BuildRequestWithDegradations(context.Background(), &r, res)
		if err != nil {
			t.Fatalf("BuildRequestWithDegradations(%v) 失败: %v", tc, err)
		}
		found := false
		for _, d := range degs {
			if d.Field == "tool_choice" && d.Reason == core.DegradationCapabilityUnsupported {
				found = true
			}
		}
		if !found {
			t.Fatalf("tool_choice=%+v 降级未记 degradation（degs=%v），违反 1.4.2 不静默降级", tc, degs)
		}
		if bodyMap(t, out)["tool_choice"] != "auto" {
			t.Fatalf("tool_choice = %v，期望降为 auto", bodyMap(t, out)["tool_choice"])
		}
	}
	// none / auto ⇒ 直通（官方 enum），无降级记录。
	for _, tc := range []*core.ToolChoice{{Mode: "none"}, {Mode: "auto"}} {
		r := *base
		r.ToolChoice = tc
		out, degs, err := a.(DegradationAware).BuildRequestWithDegradations(context.Background(), &r, res)
		if err != nil {
			t.Fatalf("BuildRequestWithDegradations(%v) 失败: %v", tc, err)
		}
		if len(degs) != 0 {
			t.Fatalf("tool_choice=%v 直通不应记 degradation: %v", tc.Mode, degs)
		}
		if bodyMap(t, out)["tool_choice"] != tc.Mode {
			t.Fatalf("tool_choice = %v，期望原样 %s", bodyMap(t, out)["tool_choice"], tc.Mode)
		}
	}
}

// TestZhipuParseResponseFinishSuperset 钉住智谱独有 / 非标准 finish_reason（4.5.1 / 4.5.2）：
// sensitive / network_error / model_context_window_exceeded 经 Responses 对象的显式
// finish_reason 字段超集原值保留（⛔ 不得当作未知值丢弃）。
// 错误实现会红：被归并成 unknown ⇒ 原值断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuParseResponseFinishSuperset -v
func TestZhipuParseResponseFinishSuperset(t *testing.T) {
	a := NewZhipu(http.DefaultClient)
	for _, want := range []string{core.FinishSensitive, core.FinishNetworkError, core.FinishModelContextWindowExceeded} {
		body := `{"id":"resp_1","object":"response","status":"completed","output":[],"finish_reason":"` + want + `"}`
		out, err := a.ParseResponse(newZhipuResolution(""), []byte(body))
		if err != nil {
			t.Fatalf("ParseResponse(%s) 报错: %v", want, err)
		}
		if out.FinishReason != want {
			t.Fatalf("FinishReason = %q，期望超集原值 %q", out.FinishReason, want)
		}
	}
}

// TestZhipuParseResponseRealSample 钉住真实样本直喂（2026-11-16 真机直采，glm-5.3-flash 普通 key
// /api/v1，来源见 testdata/real/README.md）上 ParseResponse 成立：
// output[0] 是 reasoning（⛔ 不假设 output[0] 是 message）、message 文本逐字、
// status=incomplete + incomplete_details.max_output_tokens ⇒ finish=length、
// usage 归一（In 14 / Out 256 已含 reasoning ⛔ 不二次加 / Reason 231 / Total 270）。
// 错误实现会红：只取 output[0] 当 message ⇒ Text 断言红；reasoning 二次加进输出 ⇒ usage 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuParseResponseRealSample -v
func TestZhipuParseResponseRealSample(t *testing.T) {
	a := NewZhipu(http.DefaultClient)
	out, err := a.ParseResponse(newZhipuResolution(""), loadFixture(t, "real/zhipu_responses_basic_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if len(out.OutputItems) != 2 || out.OutputItems[0].Type != "reasoning" {
		t.Fatalf("OutputItems 顺序/类型不对: %+v（⛔ 不假设 output[0] 是 message）", out.OutputItems)
	}
	if !strings.HasPrefix(out.OutputItems[0].ReasoningText, "嗯，用户只输入了“说好”两个字") {
		t.Fatalf("reasoning_text 未进推理正文位: %.30q", out.OutputItems[0].ReasoningText)
	}
	const wantText = "你说“说好”，我理解为希望达成一个约定或者进行一次良好的沟通。这可以有很多含义，取决于具体的"
	if out.OutputItems[1].Type != "message" || out.Text != wantText {
		t.Fatalf("message 文本 = %q，期望逐字 %q", out.Text, wantText)
	}
	// status=incomplete + incomplete_details.reason=max_output_tokens ⇒ length（4.5.2）。
	if out.FinishReason != core.FinishLength || out.Status != core.StatusIncomplete {
		t.Fatalf("finish/status = %q/%q，期望 length/incomplete", out.FinishReason, out.Status)
	}
	u := out.Usage
	if u == nil || u.InputTokens != 14 || u.CacheReadTokens != 0 || u.OutputTokens != 256 ||
		u.ReasoningTokens != 231 || u.TotalTokens != 270 {
		t.Fatalf("usage 归一 = %+v，期望 In14/Read0/Out256/Reason231/Total270（样本逐字）", u)
	}

	// reasoning 样本：message 文本逐字 + completed ⇒ stop。
	out2, err := a.ParseResponse(newZhipuResolution(""), loadFixture(t, "real/zhipu_responses_reasoning_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse(reasoning 样本) 报错: %v", err)
	}
	const wantText2 = "**9.9 更大。**\n\n因为 9.9 = 9.90，而 9.11 < 9.90（0.9 > 0.11）。"
	if out2.Text != wantText2 {
		t.Fatalf("message 文本 = %q，期望逐字 %q", out2.Text, wantText2)
	}
	if out2.FinishReason != core.FinishStop || out2.Status != core.StatusCompleted {
		t.Fatalf("finish/status = %q/%q，期望 stop/completed", out2.FinishReason, out2.Status)
	}
	if u := out2.Usage; u == nil || u.InputTokens != 24 || u.OutputTokens != 169 || u.ReasoningTokens != 128 || u.TotalTokens != 193 {
		t.Fatalf("usage 归一 = %+v，期望 In24/Out169/Reason128/Total193（样本逐字）", u)
	}
}

// TestZhipuParseResponseToolCallRealSample 钉住真实 tools 样本（扁平工具形出 function_call item，
// 2026-11-16 真机直采）上函数名 / 参数 / 调用 id 逐字进 core.OutputItem，finish=tool_calls。
// 错误实现会红：name 不进 Name、call_id 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuParseResponseToolCallRealSample -v
func TestZhipuParseResponseToolCallRealSample(t *testing.T) {
	a := NewZhipu(http.DefaultClient)
	out, err := a.ParseResponse(newZhipuResolution(""), loadFixture(t, "real/zhipu_responses_tools_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if len(out.OutputItems) != 2 || out.OutputItems[1].Type != "function_call" {
		t.Fatalf("OutputItems = %+v，期望 reasoning + function_call", out.OutputItems)
	}
	tc := out.OutputItems[1]
	if tc.Name != "get_weather" || tc.Arguments != `{"city":"北京"}` ||
		tc.CallID != "call_-7079779938462593202" || tc.ID != "fc_call_-7079779938462593202" {
		t.Fatalf("function_call item = %+v，期望 name/arguments/call_id 逐字（样本）", tc)
	}
	if out.FinishReason != core.FinishToolCalls {
		t.Fatalf("FinishReason = %q，期望 tool_calls（status=completed + 有工具调用）", out.FinishReason)
	}
	if u := out.Usage; u == nil || u.InputTokens != 157 || u.OutputTokens != 54 || u.ReasoningTokens != 42 || u.TotalTokens != 211 {
		t.Fatalf("usage 归一 = %+v，期望 In157/Out54/Reason42/Total211（样本逐字）", u)
	}
}

// TestZhipuStreamRealSample 钉住真实流样本直喂（2026-11-16 真机逐帧录制，无 [DONE]，
// response.incomplete 收口，来源见 testdata/real/README.md）：
// 帧序列 = lifecycle×2 + 推理增量×180 + 文本增量×73 + usage + done = 257 帧；
// 尾部 usage（In14/Out256/Reason180/Total270）先于 done（incomplete）。
// 错误实现会红：漏 lifecycle / 增量计数不齐 / usage 与 done 顺序颠倒 ⇒ 逐项红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuStreamRealSample -v
func TestZhipuStreamRealSample(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(loadFixture(t, "real/zhipu_responses_stream_real.sse"))
	}))
	defer srv.Close()

	a := NewZhipu(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newZhipuResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v（无 [DONE]，response.incomplete 收口）", cerr)
	}
	if len(frames) != 257 {
		t.Fatalf("帧数 = %d，期望 257（lifecycle×2 + 推理×180 + 文本×73 + usage + done）", len(frames))
	}
	for i, want := range []string{"response.created", "response.in_progress"} {
		if frames[i].Kind != stream.FrameLifecycle || frames[i].Status != want {
			t.Fatalf("帧[%d] = %+v，期望 lifecycle %s", i, frames[i], want)
		}
	}
	// 推理增量（样本逐字前三帧）。
	for i, want := range []string{"嗯", "，", "用户"} {
		f := frames[2+i]
		if f.Kind != stream.FrameReasoningDelta || f.Reasoning != want {
			t.Fatalf("帧[%d] = %+v，期望推理增量 %q（样本逐字）", 2+i, f, want)
		}
	}
	if frames[181].Kind != stream.FrameReasoningDelta {
		t.Fatalf("帧[181] = %+v，期望推理增量（末个）", frames[181])
	}
	// 文本增量（样本逐字前三帧）。
	for i, want := range []string{"看起来", "你", "只"} {
		f := frames[182+i]
		if f.Kind != stream.FrameTextDelta || f.Text != want {
			t.Fatalf("帧[%d] = %+v，期望文本增量 %q（样本逐字）", 182+i, f, want)
		}
	}
	if frames[254].Kind != stream.FrameTextDelta {
		t.Fatalf("帧[254] = %+v，期望文本增量（末个）", frames[254])
	}
	// 尾两帧：usage（response.incomplete 自带）→ done incomplete。
	u := frames[255]
	if u.Kind != stream.FrameUsage || u.Usage.InputTokens != 14 || u.Usage.OutputTokens != 256 ||
		u.Usage.ReasoningTokens != 180 || u.Usage.TotalTokens != 270 {
		t.Fatalf("帧[255] = %+v，期望 usage（In14/Out256/Reason180/Total270，样本逐字）", u)
	}
	d := frames[256]
	if d.Kind != stream.FrameDone || d.Status != core.StatusIncomplete {
		t.Fatalf("帧[256] = %+v，期望 done incomplete", d)
	}
}

// TestZhipuStreamToolCallName 钉住 Responses 形流式工具调用名进帧（帧承载约定：
// FrameToolCallArgsDelta.Text 载函数名、ToolCallID 载调用 id）：output_item.added 的
// item.name 必须以补名帧（ToolCallID=call_abc、Text=search）进入帧序列，参数增量帧照常到
// （partial_json ⇒ Arguments）、completed 收口（⛔ 无 [DONE] 也必须收口）。
// 错误实现会红：不发补名帧 ⇒ named=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuStreamToolCallName -v
func TestZhipuStreamToolCallName(t *testing.T) {
	const sse = "" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"item_id\":\"fc_1\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_abc\",\"name\":\"search\"}}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":1,\"item_id\":\"fc_1\",\"partial_json\":\"{\\\"q\\\":1}\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"usage\":{\"input_tokens\":157,\"output_tokens\":54,\"total_tokens\":211}}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewZhipu(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newZhipuResolution(srv.URL))
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

// TestZhipuParseUsage 钉住智谱 usage 归一（4.4.2 智谱行）：
// 命中 = prompt_tokens_details.cached_tokens = 30、无写入（存储限时免费 ⇒ 记 0）、
// OutputTokens = completion_tokens = 60（已含 reasoning ⛔ 不再加一次）。
// 错误实现会红：输出加 reasoning ⇒ 80 ⇒ 红；写入误记非 0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuParseUsage -v
func TestZhipuParseUsage(t *testing.T) {
	a := NewZhipu(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "zhipu_usage.json"))
	if u.InputTokens != 100 || u.CacheReadTokens != 30 || u.CacheWriteTokens != 0 || u.OutputTokens != 60 {
		t.Fatalf("归一不对：In=%d(100) Read=%d(30) Write=%d(0) Out=%d(60 含 reasoning ⛔ 不二次加)",
			u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens)
	}
	if u.InputTokensUncached != 70 {
		t.Fatalf("Uncached = %d，期望 70 = 100−30", u.InputTokensUncached)
	}
	if u.ReasoningTokens != 20 {
		t.Fatalf("ReasoningTokens = %d，期望 20（completion_tokens_details.reasoning_tokens 单列）", u.ReasoningTokens)
	}
}

// TestZhipuParseUsageRealSample 钉住真实样本（智谱官方文档上下文缓存指南响应示例，真实 request_id
// 202609171836159342f8e00bec415f，来源见 testdata/real/README.md）上 ParseUsage 归一成立：
// 命中 1024 单列、无写入（存储限时免费）、Uncached=1075−1024=51；
// completion 241 已含 reasoning ⇒ Out=241 ⛔ 不二次加（样本 reasoning=0）。
// 错误实现会红：命中加进输入或 reasoning 二次加 ⇒ 数值对不上 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuParseUsageRealSample -v
func TestZhipuParseUsageRealSample(t *testing.T) {
	a := NewZhipu(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/zhipu_usage.json"))
	if u.InputTokens != 1075 || u.CacheReadTokens != 1024 || u.InputTokensUncached != 51 ||
		u.OutputTokens != 241 || u.ReasoningTokens != 0 || u.TotalTokens != 1316 {
		t.Fatalf("归一 = In%d/Read%d/Unc%d/Out%d/Reason%d/Total%d，期望 1075/1024/51/241/0/1316（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.InputTokensUncached, u.OutputTokens, u.ReasoningTokens, u.TotalTokens)
	}
}
