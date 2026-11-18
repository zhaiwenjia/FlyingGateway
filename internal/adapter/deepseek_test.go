// 日期: 2026-11-16 16:02:19.256
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件覆盖 DeepSeek 适配器（统一 SDD 4.3.4；出站 Responses）：
// BuildRequest（POST {base}/responses、input[] / instructions 顶层 / tools 扁平形、
// previous_response_id 不支持 ⇒ 显式报错、Extra 透传）、ParseResponse（真实样本直喂
// testdata/real/deepseek_responses_{basic,tools}_real.json；finish_reason 超集原值）、
// ParseUsage（chat 树命中/未命中双字段：同值取其一 ⛔ 不累加）、真实流样本直喂解码器
// （testdata/real/deepseek_responses_stream_real.sse）。
// feedSSEFile 是 W1 三家共用的「直喂解码器」小工具（⛔ 不经 HTTP 层）。

func newDeepSeekResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderDeepSeek,
		Shape:    core.APIShapeOpenAIResponses,
		BaseURL:  base,
		Model:    "deepseek-reasoner",
	}
}

// feedSSEFile 把真实 SSE 样本逐帧喂给解码器（「直喂解码器」口径）并收集全部帧；
// 解码器报错即红（容忍口径下不应报错）。W1 三家真实流样本直喂共用。
func feedSSEFile(t *testing.T, decode func(stream.SSEEvent) ([]stream.Frame, bool, error), rel string) []stream.Frame {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatalf("打开真实样本失败: %v", err)
	}
	defer f.Close()
	sc := stream.NewSSEScanner(f)
	var frames []stream.Frame
	for sc.Scan() {
		fs, done, err := decode(sc.Event())
		if err != nil {
			t.Fatalf("decode 出错: %v", err)
		}
		frames = append(frames, fs...)
		if done {
			break
		}
	}
	return frames
}

// TestDeepSeekBuildRequestPath 钉住 Responses 形请求（2026-11-16 出站 Responses 化）：
// ① POST {base}/responses；② input[] 异构 item（system 并入顶层 instructions、
// assistant 拆 reasoning + message + function_call、工具结果 function_call_output）
// + max_output_tokens + prompt_cache_key 透传（3.5）；
// ③ tools 扁平形 {type:"function",name,...}、tool_choice 命名形态 {type:"function",name}；
// ④ previous_response_id 不支持（官方「该 API 是无状态的:服务端不存储响应与会话」）
// ⇒ 显式 400 unsupported_capability（⛔ 不静默丢弃）；⑤ Extra 透传（enable_thinking）。
// 错误实现会红：错发 /chat/completions ⇒ URL 断言红；system 不并 instructions ⇒ 断言红；
// previous_response_id 被静默吞掉 ⇒ 显式报错断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekBuildRequestPath -v
func TestDeepSeekBuildRequestPath(t *testing.T) {
	a := NewDeepSeek(http.DefaultClient)
	r := &core.Request{
		Instructions: "主指令",
		Messages: []core.Message{
			{Role: "system", Content: []core.ContentPart{{Text: "系统提示"}}},
			{Role: "user", Content: []core.ContentPart{{Text: "你好"}}},
			{
				Role:             "assistant",
				ReasoningContent: "想",
				Content:          []core.ContentPart{{Text: "答案"}},
				ToolCalls:        []core.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"北京"}`}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: []core.ContentPart{{Text: "结果"}}},
		},
		Tools:           []core.Tool{{Type: "function", Name: "get_weather", Description: "查天气", Parameters: []byte(`{"type":"object"}`)}},
		ToolChoice:      &core.ToolChoice{Mode: "function", Name: "get_weather"},
		MaxOutputTokens: 1024,
		PromptCacheKey:  "pk-1",
		Extra:           map[string]any{"enable_thinking": true},
	}
	res := newDeepSeekResolution("https://api.deepseek.com")
	out, err := a.BuildRequest(context.Background(), r, res)
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.Method != "POST" || out.URL != "https://api.deepseek.com/responses" {
		t.Fatalf("URL/方法不对: %s %s（Responses 形：POST /responses）", out.Method, out.URL)
	}
	m := bodyMap(t, out)
	if mustString(t, m, "model") != "deepseek-reasoner" {
		t.Fatalf("model = %v，期望 res.Model", m["model"])
	}
	// instructions 顶层（4.2.2）：core.Instructions + system 消息并入。
	if got := mustString(t, m, "instructions"); got != "主指令\n系统提示" {
		t.Fatalf("instructions = %q，期望 %q", got, "主指令\n系统提示")
	}
	// input[] 异构 item（4.2.3：一条 assistant message ⇒ reasoning + message + function_call 多 item）。
	items := mustSlice(t, m, "input")
	wantTypes := []string{"message", "reasoning", "message", "function_call", "function_call_output"}
	if len(items) != len(wantTypes) {
		t.Fatalf("input item 数 = %d，期望 %d：%v", len(items), len(wantTypes), items)
	}
	for i, wt := range wantTypes {
		it := items[i].(map[string]any)
		if it["type"] != wt {
			t.Fatalf("input[%d].type = %v，期望 %s", i, it["type"], wt)
		}
	}
	// user 消息 part 用 input_text；assistant 文本 part 用 output_text（4.3.1 item 内部结构）。
	up := items[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if up["type"] != "input_text" || up["text"] != "你好" {
		t.Fatalf("input[0].content[0] = %v，期望 input_text/你好", up)
	}
	// reasoning item：ReasoningContent 回放为 reasoning_text part（4.2.5 语义字段）。
	rs := items[1].(map[string]any)
	rc := rs["content"].([]any)[0].(map[string]any)
	if rc["type"] != "reasoning_text" || rc["text"] != "想" {
		t.Fatalf("reasoning item content = %v，期望 reasoning_text/想", rc)
	}
	// function_call（工具调用）与 function_call_output（工具结果是独立 item，4.2.2）。
	fc := items[3].(map[string]any)
	if fc["call_id"] != "call_1" || fc["name"] != "get_weather" {
		t.Fatalf("function_call = %v", fc)
	}
	fo := items[4].(map[string]any)
	if fo["call_id"] != "call_1" || fo["output"] != "结果" {
		t.Fatalf("function_call_output = %v", fo)
	}
	if mustNumber(t, m, "max_output_tokens") != 1024 {
		t.Fatalf("max_output_tokens = %v，期望 1024（Responses 形字段）", m["max_output_tokens"])
	}
	assertNoKey(t, m, "max_tokens") // ⛔ chat 形字段名
	if mustString(t, m, "prompt_cache_key") != "pk-1" {
		t.Fatalf("prompt_cache_key 未透传: %v", m["prompt_cache_key"])
	}
	// tools 扁平形（4.3.1：FunctionToolParam{name,...}，⛔ 非 chat 形 function 内嵌对象）。
	t0 := mustSlice(t, m, "tools")[0].(map[string]any)
	if t0["type"] != "function" || t0["name"] != "get_weather" {
		t.Fatalf("tools[0] = %v，期望扁平形 {type:function,name:get_weather}", t0)
	}
	// tool_choice：请求的指定函数名 ⇒ 降为 auto（2026-11-18 修复：thinking 模式只吃
	// none/auto，上游 400 实测；降级矩阵见 TestDeepSeekToolChoiceDegradationResponses；
	// 命名形态 wire 由 openai 线测试钉住——那里上游接受命名形态）。
	if mustString(t, m, "tool_choice") != "auto" {
		t.Fatalf("tool_choice = %v，期望降为 auto（DeepSeek thinking 模式仅 none/auto）", m["tool_choice"])
	}
	// Extra 透传（enable_thinking 等特异参数，3.1）。
	if m["enable_thinking"] != true {
		t.Fatalf("enable_thinking 未透传: %v", m["enable_thinking"])
	}
	// previous_response_id：不支持 ⇒ 显式报错（⛔ 不静默丢弃，3.5）。
	if a.SupportsPreviousResponseID() {
		t.Fatalf("DeepSeek 不支持 previous_response_id（官方：API 无状态、不存储响应与会话）")
	}
	_, err = a.BuildRequest(context.Background(), &core.Request{PreviousResponseID: "resp_prev"}, res)
	requireDetailErr(t, err, "unsupported_capability")
	// AuthScheme（8.4）：Authorization + "Bearer "。
	sch := a.AuthScheme()
	if sch.HeaderName != "Authorization" || sch.Prefix != "Bearer " {
		t.Fatalf("AuthScheme = %+v，期望 Authorization/Bearer ", sch)
	}
}

// TestDeepSeekToolChoiceDegradationResponses 钉住 DeepSeek tool_choice 降级
// （2026-11-18 修复回归锁）：thinking 模式只吃 none/auto（上游实测逐字：none 200、
// required / 指定函数名 400 "Thinking mode does not support this tool_choice"）
// ⇒ required / 指定函数名降为 auto 并记 degradation（1.4.2 不静默降级）；
// none / auto 原样直通、零降级记录。
// 错误实现会红：不降级 ⇒ body tool_choice 非 auto 断言红；静默降级 ⇒ degradation 断言红；
// none/auto 被误降 ⇒ 直通断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekToolChoiceDegradationResponses -v
func TestDeepSeekToolChoiceDegradationResponses(t *testing.T) {
	a := NewDeepSeek(&http.Client{})
	res := newDeepSeekResolution("https://api.deepseek.com")
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
	// none / auto ⇒ 直通（thinking 模式官方可吃），无降级记录。
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

// TestDeepSeekParseResponseRealSample 钉住真实样本直喂（2026-11-16 上游直采，
// 来源见 testdata/real/README.md）：
// basic：output[] 异构（reasoning→message，⛔ 不假设 output[0] 是 message）、
// reasoning_text ⇒ 推理正文位、encrypted_content 保留（4.2.5）、usage Responses 树归一；
// tools：function_call item（call_id/name/arguments 逐字）+ finish 推导 tool_calls。
// 错误实现会红：只取 output[0] 当 message ⇒ Text 断言红；丢 encrypted_content ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekParseResponseRealSample -v
func TestDeepSeekParseResponseRealSample(t *testing.T) {
	a := NewDeepSeek(http.DefaultClient)
	out, err := a.ParseResponse(newDeepSeekResolution(""), loadFixture(t, "real/deepseek_responses_basic_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if out.Text != "好，我在。请说。" {
		t.Fatalf("Text = %q，期望 %q", out.Text, "好，我在。请说。")
	}
	if len(out.OutputItems) != 2 || out.OutputItems[0].Type != "reasoning" {
		t.Fatalf("OutputItems 顺序/类型不对: %+v（⛔ 不假设 output[0] 是 message）", out.OutputItems)
	}
	if out.OutputItems[0].EncryptedContent != "8fe8302f-08b8-4554-beba-cc40898622c4-0" {
		t.Fatalf("encrypted_content 丢失: %+v（4.2.5 ⛔ 必须保留）", out.OutputItems[0])
	}
	if got := out.OutputItems[0].ReasoningText; !strings.HasPrefix(got, "We need answer to") {
		t.Fatalf("reasoning_text 未进推理正文位（前缀逐字 We need answer to）: %.30q", got)
	}
	if out.OutputItems[1].Type != "message" || out.OutputItems[1].Content[0].Text != "好，我在。请说。" {
		t.Fatalf("message item 解析不对: %+v", out.OutputItems[1])
	}
	if out.FinishReason != core.FinishStop || out.Status != core.StatusCompleted {
		t.Fatalf("FinishReason=%q Status=%q，期望 stop/completed（status 推导）", out.FinishReason, out.Status)
	}
	// usage Responses 树归一（样本逐字：in 32 / cached 0 / out 243 / reasoning 235 / total 275）。
	if out.Usage.InputTokens != 32 || out.Usage.CacheReadTokens != 0 || out.Usage.OutputTokens != 243 ||
		out.Usage.ReasoningTokens != 235 || out.Usage.TotalTokens != 275 {
		t.Fatalf("usage 归一 = In%d/Read%d/Out%d/Reas%d/Total%d，期望 32/0/243/235/275",
			out.Usage.InputTokens, out.Usage.CacheReadTokens, out.Usage.OutputTokens, out.Usage.ReasoningTokens, out.Usage.TotalTokens)
	}
	out, err = a.ParseResponse(newDeepSeekResolution(""), loadFixture(t, "real/deepseek_responses_tools_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse(tools) 报错: %v", err)
	}
	if out.FinishReason != core.FinishToolCalls {
		t.Fatalf("FinishReason = %q，期望 tool_calls（hasToolCall 推导）", out.FinishReason)
	}
	fc := out.OutputItems[1]
	if fc.Type != "function_call" || fc.CallID != "call_00_jA9ZGY83r7ioyS5ZSRun3520" ||
		fc.Name != "get_weather" || fc.Arguments != `{"city": "北京"}` {
		t.Fatalf("function_call item 不对: %+v（call_id/name/arguments 逐字）", fc)
	}
	if out.Usage.InputTokens != 293 || out.Usage.CacheReadTokens != 128 || out.Usage.OutputTokens != 53 || out.Usage.TotalTokens != 346 {
		t.Fatalf("usage 归一 = In%d/Read%d/Out%d/Total%d，期望 293/128/53/346（cached_tokens=128 逐字）",
			out.Usage.InputTokens, out.Usage.CacheReadTokens, out.Usage.OutputTokens, out.Usage.TotalTokens)
	}
}

// TestDeepSeekParseResponseFinishSuperset 钉住 DeepSeek 独有 finish_reason（4.5.1 / 4.5.2）：
// aborted / insufficient_system_resource ⇒ 显式 finish_reason 回落字段超集原值
// （共享 deriveResponsesFinish 的 explicit 优先分支，⛔ 不得当作未知值丢弃）。
// 错误实现会红：被归并成 unknown ⇒ 原值断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekParseResponseFinishSuperset -v
func TestDeepSeekParseResponseFinishSuperset(t *testing.T) {
	a := NewDeepSeek(http.DefaultClient)
	body := `{"id":"r","object":"response","status":"completed","finish_reason":"aborted","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"完成"}]}],"usage":{"input_tokens":7,"output_tokens":11,"total_tokens":18}}`
	out, err := a.ParseResponse(newDeepSeekResolution(""), []byte(body))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if out.FinishReason != core.FinishAborted {
		t.Fatalf("FinishReason = %q，期望 aborted（DeepSeek 独有，4.5.1）", out.FinishReason)
	}
	if out.Text != "完成" || out.Usage.InputTokens != 7 {
		t.Fatalf("解析不对：Text=%q Usage.In=%d", out.Text, out.Usage.InputTokens)
	}
	body = `{"id":"r","object":"response","status":"completed","finish_reason":"insufficient_system_resource","output":[]}`
	out, err = a.ParseResponse(newDeepSeekResolution(""), []byte(body))
	if err != nil {
		t.Fatalf("ParseResponse(insufficient) 报错: %v", err)
	}
	if out.FinishReason != core.FinishInsufficientSystemResource {
		t.Fatalf("FinishReason = %q，期望 insufficient_system_resource", out.FinishReason)
	}
}

// TestDeepSeekParseUsageHitMiss 钉住命中/未命中双字段口径（4.3.4 / 4.4.2 DeepSeek 行）：
// ① 逐字样本：prompt 7 = 命中 0 + 未命中 7 ⇒ In=7, Read=0, Out=11, Uncached=7；
// ② prompt_cache_hit_tokens 与 prompt_tokens_details.cached_tokens 同值 ⇒ 取其一 ⛔ 不累加
//
//	⇒ Read=5（⛔ 非 10）、Uncached=miss=2。
//
// 错误实现会红：两处命中相加 ⇒ Read=10 ⇒ 红（11.1「DeepSeek 二选一」行）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekParseUsageHitMiss -v
func TestDeepSeekParseUsageHitMiss(t *testing.T) {
	a := NewDeepSeek(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "deepseek_usage_sample.json"))
	if u.InputTokens != 7 || u.CacheReadTokens != 0 || u.OutputTokens != 11 || u.InputTokensUncached != 7 {
		t.Fatalf("逐字样本归一不对：In=%d(7) Read=%d(0) Out=%d(11) Uncached=%d(7)",
			u.InputTokens, u.CacheReadTokens, u.OutputTokens, u.InputTokensUncached)
	}
	u = a.ParseUsage(loadFixture(t, "deepseek_usage_hitmiss.json"))
	if u.CacheReadTokens != 5 {
		t.Fatalf("CacheRead = %d，期望 5（hit 与 cached 同值取其一 ⛔ 不累加成 10）", u.CacheReadTokens)
	}
	if u.InputTokens != 7 || u.InputTokensUncached != 2 || u.OutputTokens != 11 {
		t.Fatalf("归一不对：In=%d(7) Uncached=%d(2=miss) Out=%d(11)",
			u.InputTokens, u.InputTokensUncached, u.OutputTokens)
	}
}

// TestDeepSeekStreamRealSample 钉住真实流样本直喂解码器（2026-11-16 上游直采）：
// testdata/real/deepseek_responses_stream_real.sse ⇒ 帧序列与样本逐字一致：
// lifecycle×2（created / in_progress）+ 推理增量×111（response.reasoning_text.delta，
// DeepSeek 扩展事件名）+ 文本增量×10（逐字清单）+ usage（32/0/122/154）+ done(completed)。
// 错误实现会红：文本增量串不对 ⇒ 逐帧断言红；usage 缺失或归一错 ⇒ usage 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekStreamRealSample -v
func TestDeepSeekStreamRealSample(t *testing.T) {
	a := NewDeepSeek(http.DefaultClient)
	frames := feedSSEFile(t, a.(*deepseekAdapter).streamDecoder().decode, "real/deepseek_responses_stream_real.sse")
	const nReason, nText = 111, 10
	if len(frames) != 2+nReason+nText+2 {
		t.Fatalf("帧数 = %d，期望 %d（lifecycle×2 + 推理×%d + 文本×%d + usage + done）：%+v",
			len(frames), 2+nReason+nText+2, nReason, nText, frames)
	}
	if frames[0].Kind != stream.FrameLifecycle || frames[0].Status != "response.created" ||
		frames[1].Kind != stream.FrameLifecycle || frames[1].Status != "response.in_progress" {
		t.Fatalf("帧[0..1] = %+v / %+v，期望 lifecycle created / in_progress", frames[0], frames[1])
	}
	// 推理增量（response.reasoning_text.delta）：数量、首尾逐字、拼接全文逐字（样本原文）。
	var rb []string
	for _, f := range frames[2 : 2+nReason] {
		if f.Kind != stream.FrameReasoningDelta {
			t.Fatalf("帧类型 = %+v，期望推理增量", f)
		}
		rb = append(rb, f.Reasoning)
	}
	const wantReason = `We need answer to user "说好" which in Chinese means "Okay" / "Deal" / "Agreed". They just said "说好" maybe typo? Could be "说好" as in "agreed". Need respond appropriately in Chinese, likely acknowledge. Maybe ask what they want to agree on? The context absent. Need be helpful. We can say "好的，说定了。需要我帮你做什么？" Or if they meant "说好" maybe "好，说定了！". We need final concise.`
	if rb[0] != "We" || rb[nReason-1] != "." || joinAll(rb) != wantReason {
		t.Fatalf("推理增量首尾/拼接 = %.3q / %.3q / %.60q，期望 We / . / 样本拼接全文（逐字）", rb[0], rb[nReason-1], joinAll(rb))
	}
	// 文本增量逐字清单。
	wantText := []string{"好", "，", "说", "定了", "！", "需要", "我", "帮你", "做什么", "？"}
	for i, want := range wantText {
		f := frames[2+nReason+i]
		if f.Kind != stream.FrameTextDelta || f.Text != want {
			t.Fatalf("帧[%d] = %+v，期望文本增量 %q（样本逐字）", 2+nReason+i, f, want)
		}
	}
	// usage（response.completed 自带，⛔ 无需 stream_options）：样本逐字 32/0/122/154。
	u := frames[2+nReason+nText]
	if u.Kind != stream.FrameUsage || u.Usage.InputTokens != 32 || u.Usage.CacheReadTokens != 0 ||
		u.Usage.OutputTokens != 122 || u.Usage.ReasoningTokens != 111 || u.Usage.TotalTokens != 154 {
		t.Fatalf("usage 帧 = %+v，期望 In32/Read0/Out122/Reas111/Total154（样本逐字）", u)
	}
	d := frames[len(frames)-1]
	if d.Kind != stream.FrameDone || d.Status != core.StatusCompleted {
		t.Fatalf("末帧 = %+v，期望 done completed（usage 帧先于 done 帧）", d)
	}
}

// joinAll 拼接增量文本（帧序列断言的辅助统计）。
func joinAll(parts []string) string {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return string(b)
}

// TestDeepSeekStreamToolCallName 钉住 Responses 流的工具帧与推理增量（帧承载约定：
// FrameToolCallArgsDelta.Text 载函数名、ToolCallID 载调用 id、Arguments 载 partial_json 增量）：
// output_item.added 带名进补名帧、function_call_arguments.delta 传参数增量、
// response.reasoning_text.delta（DeepSeek 扩展事件名逐字）进推理帧；缺 [DONE] 不丢终止帧。
// 错误实现会红：不发补名帧 ⇒ named=0 ⇒ 红；partial_json 不解 ⇒ 参数帧断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekStreamToolCallName -v
func TestDeepSeekStreamToolCallName(t *testing.T) {
	const sse = "" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"item_id\":\"fc_1\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_abc\",\"name\":\"search\"}}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":1,\"item_id\":\"fc_1\",\"partial_json\":\"{\\\"q\\\":1}\"}\n\n" +
		"event: response.reasoning_text.delta\n" +
		"data: {\"type\":\"response.reasoning_text.delta\",\"sequence_number\":2,\"delta\":\"想一想\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":3}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewDeepSeek(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newDeepSeekResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v（缺 [DONE] 必须容忍）", cerr)
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
		t.Fatalf("response.reasoning_text.delta 未进推理帧：%+v", frames)
	}
	if doneStatus != core.StatusCompleted {
		t.Fatalf("done 状态 = %q，期望 completed", doneStatus)
	}
}

// TestDeepSeekParseUsageRealSample 钉住真实样本（DeepSeek 官方 API 文档响应示例，含真实 id/指纹，
// 来源见 testdata/real/README.md）上 ParseUsage 归一成立：
// prompt_tokens=7 是总输入；命中 0（顶层与二级同值取其一）；Uncached 取 prompt_cache_miss_tokens=7。
// 错误实现会红：命中/未命中双字段累加或 Uncached 公式错位 ⇒ 7 ≠ 7 之外的值 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekParseUsageRealSample -v
func TestDeepSeekParseUsageRealSample(t *testing.T) {
	a := NewDeepSeek(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/deepseek_usage.json"))
	if u.InputTokens != 7 || u.CacheReadTokens != 0 || u.InputTokensUncached != 7 || u.OutputTokens != 11 || u.TotalTokens != 18 {
		t.Fatalf("归一 = In%d/Read%d/Unc%d/Out%d/Total%d，期望 7/0/7/11/18（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.InputTokensUncached, u.OutputTokens, u.TotalTokens)
	}
}
