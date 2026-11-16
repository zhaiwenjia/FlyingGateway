// 日期: 2026-11-16 16:02:19.259
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

// 本文件覆盖 Kimi / Moonshot 适配器（统一 SDD 4.3.9；出站 Responses）：
// BuildRequest（POST {base}/responses、prompt_cache_options 进 body、
// ⛔ 无 prompt_cache_breakpoint——显式断点被拒 400）、tool_choice 官方 enum 仅 auto ⇒
// 非 auto 降级记 degradation（1.4.2 ⛔ 不静默）、ParseUsage（Kimi 三分：未命中 + 命中 + 写入
// 互斥，⛔ 不累加）、真实样本直喂（testdata/real/moonshot_responses_*_real.*）与
// 真实流样本直喂解码器（moonshot_responses_stream_real.sse）。

func newMoonshotResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderMoonshot,
		Shape:    core.APIShapeOpenAIResponses,
		BaseURL:  base,
		Model:    "kimi-k2.7",
	}
}

// TestMoonshotBuildRequestFields 钉住 Moonshot 特异字段（4.3.9 逐字；Responses 形）：
// ① 输出上限走 Responses 标准 max_output_tokens（⛔ 不发 chat 形 max_tokens /
// max_completion_tokens）；② prompt_cache_options{mode, ttl} 经 Extra 透传进 body（ttl 仅 5m / 1h）；
// ③ ⛔ prompt_cache_breakpoint 显式断点被拒（400）⇒ 黑名单剔除不发；
// ④ Extra 黑名单连带剔除 temperature 等由 core 承载 / 禁发键；
// ⑤ previous_response_id 不支持（官方 OpenAPI：store 固定 false、previous_response_id 固定 null）
// ⇒ 显式 400 unsupported_capability（⛔ 不静默丢弃）。
// 错误实现会红：发 chat 形字段 ⇒ 键断言红；breakpoint 漏进 body ⇒ 无键断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMoonshotBuildRequestFields -v
func TestMoonshotBuildRequestFields(t *testing.T) {
	a := NewMoonshot(http.DefaultClient)
	r := &core.Request{
		Messages:        []core.Message{{Role: "user", Content: []core.ContentPart{{Text: "你好"}}}},
		MaxOutputTokens: 512,
		Extra: map[string]any{
			"prompt_cache_options":    map[string]any{"mode": "auto", "ttl": "5m"},
			"prompt_cache_breakpoint": "explicit-1",
			"temperature":             0.3,
		},
	}
	res := newMoonshotResolution("https://api.moonshot.cn/v1")
	out, err := a.BuildRequest(context.Background(), r, res)
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "https://api.moonshot.cn/v1/responses" {
		t.Fatalf("URL = %s，期望 {base}/responses（Responses 形）", out.URL)
	}
	m := bodyMap(t, out)
	// ① 输出上限是 max_output_tokens（Responses 形）。
	if mustNumber(t, m, "max_output_tokens") != 512 {
		t.Fatalf("max_output_tokens = %v，期望 512", m["max_output_tokens"])
	}
	assertNoKey(t, m, "max_tokens")
	assertNoKey(t, m, "max_completion_tokens")
	// ② prompt_cache_options 进 body。
	pco := mustAt(t, m, "prompt_cache_options").(map[string]any)
	if pco["ttl"] != "5m" {
		t.Fatalf("prompt_cache_options = %v，期望 ttl=5m", pco)
	}
	// ③④ 黑名单剔除。
	assertNoKey(t, m, "prompt_cache_breakpoint")
	assertNoKey(t, m, "temperature")
	// input[]（Responses 形）原样映射：单条 user。
	items := mustSlice(t, m, "input")
	if len(items) != 1 || items[0].(map[string]any)["role"] != "user" {
		t.Fatalf("input = %v，期望单条 user item", items)
	}
	// ⑤ previous_response_id：不支持 ⇒ 显式报错（⛔ 不静默丢弃，3.5）。
	if a.SupportsPreviousResponseID() {
		t.Fatalf("Moonshot 不支持 previous_response_id（官方 OpenAPI：store=false、previous_response_id=null）")
	}
	_, err = a.BuildRequest(context.Background(), &core.Request{PreviousResponseID: "resp_prev"}, res)
	requireDetailErr(t, err, "unsupported_capability")
}

// TestMoonshotToolChoiceDegradationRecorded 钉住 tool_choice 官方 enum 仅 auto（4.3.9）：
// none / required / 指定函数名 ⇒ 降为 auto 且记 degradation（1.4.2 ⛔ 不静默丢弃意图）；
// auto / 未设 ⇒ 无降级记录。错误实现对照：只降不记 ⇒ 本测试必红；把 auto 也记降级 ⇒ 后半必红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMoonshotToolChoiceDegradationRecorded -v
func TestMoonshotToolChoiceDegradationRecorded(t *testing.T) {
	a := NewMoonshot(http.DefaultClient)
	res := newMoonshotResolution("https://api.moonshot.cn/v1")
	for _, tc := range []*core.ToolChoice{
		{Mode: "none"},
		{Mode: "required"},
		{Mode: "function", Name: "search"},
	} {
		r := &core.Request{
			Messages:   []core.Message{{Role: "user", Content: []core.ContentPart{{Text: "hi"}}}},
			ToolChoice: tc,
		}
		out, degs, err := a.(DegradationAware).BuildRequestWithDegradations(context.Background(), r, res)
		if err != nil {
			t.Fatalf("BuildRequestWithDegradations(%s) 失败: %v", tc.Mode, err)
		}
		found := false
		for _, d := range degs {
			if d.Field == "tool_choice" && d.Reason == core.DegradationCapabilityUnsupported {
				found = true
			}
		}
		if !found {
			t.Fatalf("tool_choice=%s 降级未记 degradation（degs=%v），违反 1.4.2", tc.Mode, degs)
		}
		m := bodyMap(t, out)
		if m["tool_choice"] != "auto" {
			t.Fatalf("tool_choice = %v，期望降为 auto（Responses 形取值）", m["tool_choice"])
		}
	}
	// auto ⇒ 官方支持原样发送、⛔ 不记降级。
	out, degs, err := a.(DegradationAware).BuildRequestWithDegradations(context.Background(),
		&core.Request{Messages: []core.Message{{Role: "user", Content: []core.ContentPart{{Text: "hi"}}}},
			ToolChoice: &core.ToolChoice{Mode: "auto"}}, res)
	if err != nil {
		t.Fatalf("BuildRequestWithDegradations(auto) 失败: %v", err)
	}
	if len(degs) != 0 {
		t.Fatalf("tool_choice=auto 不应记降级（degs=%v）", degs)
	}
	if bodyMap(t, out)["tool_choice"] != "auto" {
		t.Fatalf("tool_choice=auto 应原样发送")
	}
	// 未设 ⇒ 无 tool_choice 键（omitzero）。
	out, degs, err = a.(DegradationAware).BuildRequestWithDegradations(context.Background(),
		&core.Request{Messages: []core.Message{{Role: "user", Content: []core.ContentPart{{Text: "hi"}}}}}, res)
	if err != nil {
		t.Fatalf("BuildRequestWithDegradations(未设) 失败: %v", err)
	}
	if len(degs) != 0 {
		t.Fatalf("未设 tool_choice 不应记降级（degs=%v）", degs)
	}
	assertNoKey(t, bodyMap(t, out), "tool_choice")
}

// TestMoonshotParseResponseRealSample 钉住真实样本直喂（2026-11-16 上游直采，
// 来源见 testdata/real/README.md）：
// basic：status=incomplete + incomplete_details.reason=max_output_tokens ⇒ finish 推导 length、
// reasoning item 的 summary[] 保留；tools：function_call item + finish 推导 tool_calls；
// reasoning：文本 + summary 保留；usage Responses 树归一（cache_write_tokens 扩展字段在二级）。
// 错误实现会红：finish 推导丢 incomplete ⇒ length 断言红；summary 丢失 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMoonshotParseResponseRealSample -v
func TestMoonshotParseResponseRealSample(t *testing.T) {
	a := NewMoonshot(http.DefaultClient)
	out, err := a.ParseResponse(newMoonshotResolution(""), loadFixture(t, "real/moonshot_responses_basic_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if out.FinishReason != core.FinishLength || out.Status != core.StatusIncomplete {
		t.Fatalf("FinishReason=%q Status=%q，期望 length/incomplete（incomplete_details.reason=max_output_tokens 推导）",
			out.FinishReason, out.Status)
	}
	if len(out.OutputItems) != 1 || out.OutputItems[0].Type != "reasoning" {
		t.Fatalf("OutputItems = %+v，期望单 reasoning item", out.OutputItems)
	}
	if sm := out.OutputItems[0].Summary; len(sm) != 1 || !strings.HasPrefix(sm[0].Text, "I need respond to user Chinese") {
		t.Fatalf("reasoning summary 丢失/不对: %+v", out.OutputItems[0])
	}
	// usage 样本逐字：in 86 / cached 0 / write 0 / out 256 / reasoning 253 / total 342。
	if out.Usage.InputTokens != 86 || out.Usage.CacheReadTokens != 0 || out.Usage.CacheWriteTokens != 0 ||
		out.Usage.OutputTokens != 256 || out.Usage.ReasoningTokens != 253 || out.Usage.TotalTokens != 342 {
		t.Fatalf("usage 归一 = In%d/Read%d/Write%d/Out%d/Reas%d/Total%d，期望 86/0/0/256/253/342",
			out.Usage.InputTokens, out.Usage.CacheReadTokens, out.Usage.CacheWriteTokens,
			out.Usage.OutputTokens, out.Usage.ReasoningTokens, out.Usage.TotalTokens)
	}
	out, err = a.ParseResponse(newMoonshotResolution(""), loadFixture(t, "real/moonshot_responses_tools_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse(tools) 报错: %v", err)
	}
	if out.FinishReason != core.FinishToolCalls {
		t.Fatalf("FinishReason = %q，期望 tool_calls", out.FinishReason)
	}
	fc := out.OutputItems[1]
	if fc.Type != "function_call" || fc.CallID != "get_weather_0_8784a7ba" ||
		fc.Name != "get_weather" || fc.Arguments != `{"city":"北京"}` {
		t.Fatalf("function_call item 不对: %+v（call_id/name/arguments 逐字）", fc)
	}
	out, err = a.ParseResponse(newMoonshotResolution(""), loadFixture(t, "real/moonshot_responses_reasoning_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse(reasoning) 报错: %v", err)
	}
	if out.Text != "9.9 大（9.9＝9.90，＞9.11）。" {
		t.Fatalf("Text = %q，期望 %q", out.Text, "9.9 大（9.9＝9.90，＞9.11）。")
	}
	if out.FinishReason != core.FinishStop {
		t.Fatalf("FinishReason = %q，期望 stop", out.FinishReason)
	}
	if out.Usage.InputTokens != 97 || out.Usage.OutputTokens != 151 || out.Usage.ReasoningTokens != 115 || out.Usage.TotalTokens != 248 {
		t.Fatalf("usage 归一 = In%d/Out%d/Reas%d/Total%d，期望 97/151/115/248",
			out.Usage.InputTokens, out.Usage.OutputTokens, out.Usage.ReasoningTokens, out.Usage.TotalTokens)
	}
}

// TestMoonshotParseUsageKimiThreeWay 钉住 Kimi 三分口径（11.1「Kimi 三分」逐字行 309/53/256）：
// InputTokens=309（总 = 未命中 + 命中 + 写入，三者互斥已含在内）、CacheRead=53
// （顶层 cached_tokens 与二级同值 ⇒ 取其一 ⛔ 不累加成 106）、CacheWrite=256、Uncached=0。
// 错误实现会红：三分相加当总数 ⇒ 568 ⇒ 红；命中两处相加 ⇒ 106 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMoonshotParseUsageKimiThreeWay -v
func TestMoonshotParseUsageKimiThreeWay(t *testing.T) {
	a := NewMoonshot(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "kimi_usage.json"))
	if u.InputTokens != 309 {
		t.Fatalf("InputTokens = %d，期望 309（总输入，⛔ 非三分相加 568）", u.InputTokens)
	}
	if u.CacheReadTokens != 53 {
		t.Fatalf("CacheRead = %d，期望 53（顶层与二级同值取其一 ⛔ 非 106）", u.CacheReadTokens)
	}
	if u.CacheWriteTokens != 256 || u.InputTokensUncached != 0 {
		t.Fatalf("Write=%d(256) Uncached=%d(0)（4.4.2 Kimi 行：三者互斥）",
			u.CacheWriteTokens, u.InputTokensUncached)
	}
	if u.OutputTokens != 21 {
		t.Fatalf("Output = %d，期望 21", u.OutputTokens)
	}
}

// TestMoonshotStreamRealSample 钉住真实流样本直喂解码器（2026-11-16 上游直采）：
// testdata/real/moonshot_responses_stream_real.sse ⇒ 帧序列与样本逐字一致：
// lifecycle×2 + 推理增量×252（response.reasoning_summary_text.delta，Moonshot 事件名）
// + usage（86/0/0/256/342）+ done(incomplete)——收口帧是 response.incomplete（max_output_tokens）。
// 错误实现会红：summary 增量断链 ⇒ 拼接全文断言红；usage 帧缺失 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMoonshotStreamRealSample -v
func TestMoonshotStreamRealSample(t *testing.T) {
	a := NewMoonshot(http.DefaultClient)
	frames := feedSSEFile(t, a.(*moonshotAdapter).streamDecoder().decode, "real/moonshot_responses_stream_real.sse")
	const nReason = 252
	if len(frames) != 2+nReason+2 {
		t.Fatalf("帧数 = %d，期望 %d（lifecycle×2 + 推理×%d + usage + done）：%+v",
			len(frames), 2+nReason+2, nReason, frames)
	}
	if frames[0].Kind != stream.FrameLifecycle || frames[0].Status != "response.created" ||
		frames[1].Kind != stream.FrameLifecycle || frames[1].Status != "response.in_progress" {
		t.Fatalf("帧[0..1] = %+v / %+v，期望 lifecycle created / in_progress", frames[0], frames[1])
	}
	var rb []string
	for _, f := range frames[2 : 2+nReason] {
		if f.Kind != stream.FrameReasoningDelta {
			t.Fatalf("帧类型 = %+v，期望推理增量", f)
		}
		rb = append(rb, f.Reasoning)
	}
	// 推理增量拼接全文逐字（样本原文，252 段重组）。
	const wantReason = `用户发送了"说好"，这两个字在中文里。让我想想这是什么意思。

"说好"可能有几种理解：
1. 用户可能想说"说好了"——表示约定、达成一致，比如"就这么说定了"
2. 可能是打字不完整，或者想表达别的什么
3. 可能是"说好"作为某个对话的延续，但这是对话的开始，没有上下文
4. 可能用户想让我"说好"——比如答应某件事，但没有前文
5. 也可能是"说'好'"的意思，让我说"好"这个字？有点像测试或者玩笑
6. 可能是想打"说好话"、"说好的..."等，没打完

这是对话的第一条消息，没有任何上下文。用户只发了两个字"说好"。

我应该怎么回应？最自然的方式是友好地询问用户想表达什么，或者想让我做什么。因为信息太少，我无法确定意图。

可能的回应：
- "好呀！不过我们好像还没聊到具体要约定什么～你想说什么？"
- 轻松一点："好～😄 不过这是咱们的第一句对话，你想让我答应什么，或者想聊点什么？"

我需要避免过度解读，也不要太长。应该简短、友好、自然，澄清意图，同时可以给出一点`
	if rb[0] != "用户" || rb[nReason-1] != "一点" || joinAll(rb) != wantReason {
		t.Fatalf("推理增量首尾/拼接 = %.3q / %.3q / %.60q，期望 用户 / 一点 / 样本拼接全文（逐字）", rb[0], rb[nReason-1], joinAll(rb))
	}
	// usage（response.incomplete 自带，cache_write_tokens 扩展字段在二级）：样本逐字 86/0/0/256/342。
	u := frames[2+nReason]
	if u.Kind != stream.FrameUsage || u.Usage.InputTokens != 86 || u.Usage.CacheReadTokens != 0 ||
		u.Usage.CacheWriteTokens != 0 || u.Usage.OutputTokens != 256 || u.Usage.TotalTokens != 342 {
		t.Fatalf("usage 帧 = %+v，期望 In86/Read0/Write0/Out256/Total342（样本逐字）", u)
	}
	d := frames[len(frames)-1]
	if d.Kind != stream.FrameDone || d.Status != core.StatusIncomplete {
		t.Fatalf("末帧 = %+v，期望 done incomplete（usage 帧先于 done 帧）", d)
	}
}

// TestMoonshotStreamToolCallName 钉住 Responses 流的工具帧与推理增量（帧承载约定：
// FrameToolCallArgsDelta.Text 载函数名、ToolCallID 载调用 id、Arguments 载 partial_json 增量）：
// output_item.added 带名进补名帧、参数增量到、response.reasoning_summary_text.delta
// （Moonshot 事件名逐字）进推理帧。
// 错误实现会红：不发补名帧 ⇒ named=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMoonshotStreamToolCallName -v
func TestMoonshotStreamToolCallName(t *testing.T) {
	const sse = "" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"item_id\":\"fc_1\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_abc\",\"name\":\"search\"}}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":1,\"item_id\":\"fc_1\",\"partial_json\":\"{\\\"q\\\":1}\"}\n\n" +
		"event: response.reasoning_summary_text.delta\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"sequence_number\":2,\"delta\":\"想一想\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":3}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewMoonshot(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newMoonshotResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	var named, args, reasoning int
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
		if f.Kind == stream.FrameReasoningDelta && f.Reasoning == "想一想" {
			reasoning++
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
	if reasoning == 0 {
		t.Fatalf("response.reasoning_summary_text.delta 未进推理帧：%+v", frames)
	}
	if doneStatus != core.StatusCompleted {
		t.Fatalf("done 状态 = %q，期望 completed（[DONE] 容忍不重复收口）", doneStatus)
	}
}

// TestMoonshotParseUsageRealSample 钉住真实样本（Kimi 官方 API 文档响应示例，id cmpl-04ea9261…，
// 来源见 testdata/real/README.md）上 ParseUsage 归一成立：三分互斥 prompt 19 = 命中 10 + 写 0 + 未命中 9；
// 顶层 cached_tokens 与二级同值取其一 ⛔ 不累加为 20。
// 错误实现会红：同值双报累加 ⇒ Read=20 ≠ 10 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMoonshotParseUsageRealSample -v
func TestMoonshotParseUsageRealSample(t *testing.T) {
	a := NewMoonshot(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/kimi_usage.json"))
	if u.InputTokens != 19 || u.CacheReadTokens != 10 || u.CacheWriteTokens != 0 ||
		u.InputTokensUncached != 9 || u.OutputTokens != 21 || u.TotalTokens != 40 {
		t.Fatalf("归一 = In%d/Read%d/Write%d/Unc%d/Out%d/Total%d，期望 19/10/0/9/21/40（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.InputTokensUncached, u.OutputTokens, u.TotalTokens)
	}
}
