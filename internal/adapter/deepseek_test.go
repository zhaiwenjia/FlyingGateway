// 日期: 2026-11-14 16:38:51.651
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件覆盖 DeepSeek 适配器（统一 SDD 4.3.4；出站 Chat Completions）：
// BuildRequest（POST {base}/chat/completions、messages[] / max_tokens chat 形、
// previous_response_id 无 wire 载体 ⇒ 显式报错）、ParseResponse（chat.completion，
// finish_reason 独有取值超集原值）、ParseUsage（命中/未命中双字段：同值取其一 ⛔ 不累加）、
// 真实流样本直喂（testdata/real/deepseek_stream_real.sse，帧序列逐字）。

func newDeepSeekResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderDeepSeek,
		Shape:    core.APIShapeOpenAIResponses,
		BaseURL:  base,
		Model:    "deepseek-reasoner",
	}
}

// TestDeepSeekBuildRequestPath 钉住 chat 形请求（2026-11-14 改形）：
// ① POST {base}/chat/completions；② messages[]（Instructions 并为首个 system 消息、
// reasoning_content 回放、工具结果 role=tool）+ max_tokens + prompt_cache_key 透传（3.5）；
// ③ tool_choice 命名形态是 chat 标准 {"type":"function","function":{"name":...}}；
// ④ previous_response_id 无 wire 载体 ⇒ 按不支持处理、显式 400 unsupported_capability（⛔ 不静默丢弃）。
// 错误实现会红：错发 /responses ⇒ URL 断言红；reasoning_content 不回放 ⇒ 字段断言红；
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
	}
	res := newDeepSeekResolution("https://api.deepseek.com")
	out, err := a.BuildRequest(context.Background(), r, res)
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.Method != "POST" || out.URL != "https://api.deepseek.com/chat/completions" {
		t.Fatalf("URL/方法不对: %s %s（chat 形：POST /chat/completions）", out.Method, out.URL)
	}
	m := bodyMap(t, out)
	// messages[]（chat 形）：Instructions 并为首个 system 消息，原 system 消息原样保留。
	msgs := mustSlice(t, m, "messages")
	roles := []string{"system", "system", "user", "assistant", "tool"}
	if len(msgs) != len(roles) {
		t.Fatalf("messages 数 = %d，期望 %d：%v", len(msgs), len(roles), msgs)
	}
	for i, want := range roles {
		if got := mustString(t, msgs[i].(map[string]any), "role"); got != want {
			t.Fatalf("messages[%d].role = %q，期望 %q", i, got, want)
		}
	}
	if mustString(t, msgs[0].(map[string]any), "content") != "主指令" {
		t.Fatalf("messages[0].content = %v，期望主指令（Instructions 并为首个 system 消息）", msgs[0])
	}
	// reasoning_content 回放（DeepSeek 扩展名逐字）。
	as := msgs[3].(map[string]any)
	if mustString(t, as, "reasoning_content") != "想" {
		t.Fatalf("assistant reasoning_content 未回放: %v", as)
	}
	// 工具调用与工具结果（chat 形 tool_calls / role=tool）。
	tc := mustAt(t, as, "tool_calls").([]any)[0].(map[string]any)
	if mustString(t, tc, "id") != "call_1" || mustString(t, tc, "function", "name") != "get_weather" {
		t.Fatalf("tool_calls[0] = %v", tc)
	}
	toolMsg := msgs[4].(map[string]any)
	if mustString(t, toolMsg, "tool_call_id") != "call_1" || mustString(t, toolMsg, "content") != "结果" {
		t.Fatalf("tool 消息 = %v", toolMsg)
	}
	if mustNumber(t, m, "max_tokens") != 1024 {
		t.Fatalf("max_tokens = %v，期望 1024（chat 形普通字段）", m["max_tokens"])
	}
	if mustString(t, m, "prompt_cache_key") != "pk-1" {
		t.Fatalf("prompt_cache_key 未透传: %v", m["prompt_cache_key"])
	}
	// tool_choice 命名形态（chat 标准：function 对象内嵌 name）。
	if mustString(t, m, "tool_choice", "function", "name") != "get_weather" {
		t.Fatalf("tool_choice = %v，期望 {type:function,function:{name:get_weather}}", m["tool_choice"])
	}
	t0 := mustSlice(t, m, "tools")[0].(map[string]any)
	if mustString(t, t0, "type") != "function" || mustString(t, t0, "function", "name") != "get_weather" {
		t.Fatalf("tools[0] = %v，期望 chat 形 function 对象", t0)
	}
	// previous_response_id：chat 形 wire 无此字段 ⇒ 显式报错（⛔ 不静默丢弃，3.5）。
	if a.SupportsPreviousResponseID() {
		t.Fatalf("DeepSeek chat 形不支持 previous_response_id（wire 无此字段）")
	}
	_, err = a.BuildRequest(context.Background(), &core.Request{PreviousResponseID: "resp_prev"}, res)
	requireDetailErr(t, err, "unsupported_capability")
	// AuthScheme（8.4）：Authorization + "Bearer "。
	sch := a.AuthScheme()
	if sch.HeaderName != "Authorization" || sch.Prefix != "Bearer " {
		t.Fatalf("AuthScheme = %+v，期望 Authorization/Bearer ", sch)
	}
}

// TestDeepSeekParseResponseFinishSuperset 钉住 DeepSeek 独有 finish_reason（4.5.1 / 4.5.2）：
// aborted / insufficient_system_resource ⇒ 超集原值（⛔ 不得当作未知值丢弃）。
// 错误实现会红：被归并成 unknown ⇒ 原值断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekParseResponseFinishSuperset -v
func TestDeepSeekParseResponseFinishSuperset(t *testing.T) {
	a := NewDeepSeek(http.DefaultClient)
	out, err := a.ParseResponse(newDeepSeekResolution(""), loadFixture(t, "deepseek_response.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if out.FinishReason != core.FinishAborted {
		t.Fatalf("FinishReason = %q，期望 aborted（DeepSeek 独有，4.5.1）", out.FinishReason)
	}
	if out.Text != "完成" || out.Usage.InputTokens != 7 {
		t.Fatalf("解析不对：Text=%q Usage.In=%d", out.Text, out.Usage.InputTokens)
	}
	// reasoning_content（扩展字段）⇒ 推理正文位。
	if len(out.OutputItems) == 0 || out.OutputItems[0].ReasoningText != "推理正文" {
		t.Fatalf("reasoning_content 未进推理正文位: %+v", out.OutputItems)
	}
	// insufficient_system_resource 同为 DeepSeek 独有值。
	body := `{"id":"r","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"insufficient_system_resource"}]}`
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

// TestDeepSeekStreamRealSample 钉住真实流样本直喂（2026-11-14 改形验收点）：
// testdata/real/deepseek_stream_real.sse（官方文档完整逐帧示例）经 StreamStart 直喂 ⇒
// 帧序列与样本逐字一致：10 个文本增量（首块 content 空串不出帧）+ 末帧 usage（hit/miss 归一）
// + done（finish_reason=stop 超集原值）。
// 错误实现会红：文本增量串不对 ⇒ 逐帧断言红；usage 缺失或归一口径错 ⇒ usage 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekStreamRealSample -v
func TestDeepSeekStreamRealSample(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(loadFixture(t, "real/deepseek_stream_real.sse"))
	}))
	defer srv.Close()

	a := NewDeepSeek(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newDeepSeekResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	wantText := []string{"Hello", "!", " 👋", " How", " can", " I", " help", " you", " today", "?"}
	if len(frames) != len(wantText)+2 {
		t.Fatalf("帧数 = %d，期望 %d（文本 + usage + done）：%+v", len(frames), len(wantText)+2, frames)
	}
	for i, want := range wantText {
		if frames[i].Kind != stream.FrameTextDelta || frames[i].Text != want {
			t.Fatalf("帧[%d] = %+v，期望文本增量 %q（样本逐字）", i, frames[i], want)
		}
	}
	u := frames[len(wantText)]
	if u.Kind != stream.FrameUsage {
		t.Fatalf("帧[%d] = %+v，期望 usage", len(wantText), u)
	}
	// hit/miss 双字段：命中 0、未命中 7（4.3.4 取其一 ⛔ 不累加）。
	if u.Usage.InputTokens != 7 || u.Usage.OutputTokens != 11 || u.Usage.CacheReadTokens != 0 || u.Usage.InputTokensUncached != 7 {
		t.Fatalf("usage 归一 = In=%d(7) Out=%d(11) Read=%d(0) Uncached=%d(7)",
			u.Usage.InputTokens, u.Usage.OutputTokens, u.Usage.CacheReadTokens, u.Usage.InputTokensUncached)
	}
	d := frames[len(wantText)+1]
	if d.Kind != stream.FrameDone || d.Status != "stop" {
		t.Fatalf("帧[%d] = %+v，期望 done stop（finish_reason 超集原值）", len(wantText)+1, d)
	}
}

// TestDeepSeekStreamToolCallName 钉住 chat chunk 的工具帧与推理增量（帧承载约定：
// FrameToolCallArgsDelta.Text 载函数名、ToolCallID 载调用 id）：首块带名进补名帧、
// 后续块按 index 对齐同一调用 id 传参数增量、delta.reasoning_content（扩展名逐字）进推理帧。
// 错误实现会红：不发补名帧 ⇒ named=0 ⇒ 红；index 不对齐 ⇒ 参数帧 ToolCallID 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekStreamToolCallName -v
func TestDeepSeekStreamToolCallName(t *testing.T) {
	const sse = "" +
		"data: {\"id\":\"cmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_abc\",\"type\":\"function\",\"function\":{\"name\":\"search\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"cmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"q\\\":1}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"cmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"想一想\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"cmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
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
		t.Fatalf("参数帧缺失（后续块按 index 对齐 call_abc 且 Arguments={\"q\":1}）：%+v", frames)
	}
	if reasoning == 0 {
		t.Fatalf("delta.reasoning_content 未进推理帧：%+v", frames)
	}
	if doneStatus != "tool_calls" {
		t.Fatalf("done 状态 = %q，期望 tool_calls（finish_reason 超集原值）", doneStatus)
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
