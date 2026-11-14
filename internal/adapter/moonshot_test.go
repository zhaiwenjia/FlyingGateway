// 日期: 2026-11-14 14:58:31.762
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

// 本文件覆盖 Kimi / Moonshot 适配器（统一 SDD 4.3.9）：
// BuildRequest（max_completion_tokens ⛔ 非 max_tokens、prompt_cache_options 进 body、
// ⛔ 无 prompt_cache_breakpoint——显式断点被拒 400）、
// ParseUsage（Kimi 三分：未命中 + 命中 + 写入互斥，⛔ 不累加）、流式末帧 usage。

func newMoonshotResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderMoonshot,
		Shape:    core.APIShapeOpenAIResponses,
		BaseURL:  base,
		Model:    "kimi-k2.7",
	}
}

// TestMoonshotBuildRequestFields 钉住 Moonshot 特异字段（4.3.9 逐字）：
// ① 输出上限是 max_completion_tokens（max_tokens 已 deprecated ⇒ ⛔ 不发）；
// ② prompt_cache_options{mode, ttl} 经 Extra 透传进 body（ttl 仅 5m / 1h）；
// ③ ⛔ prompt_cache_breakpoint 显式断点被拒（400）⇒ 黑名单剔除不发；
// ④ Extra 黑名单连带剔除 temperature 等由 core 承载 / 禁发键。
// 错误实现会红：发 max_tokens ⇒ 键断言红；breakpoint 漏进 body ⇒ 无键断言红。
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
	out, err := a.BuildRequest(context.Background(), r, newMoonshotResolution("https://api.moonshot.cn/v1"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	m := bodyMap(t, out)
	// ① max_completion_tokens ⛔ 非 max_tokens（4.3.9）。
	if mustNumber(t, m, "max_completion_tokens") != 512 {
		t.Fatalf("max_completion_tokens = %v，期望 512", m["max_completion_tokens"])
	}
	assertNoKey(t, m, "max_tokens")
	// ② prompt_cache_options 进 body。
	pco := mustAt(t, m, "prompt_cache_options").(map[string]any)
	if pco["ttl"] != "5m" {
		t.Fatalf("prompt_cache_options = %v，期望 ttl=5m", pco)
	}
	// ③④ 黑名单剔除。
	assertNoKey(t, m, "prompt_cache_breakpoint")
	assertNoKey(t, m, "temperature")
	// previous_response_id 支持 = true（3.5 表）。
	if !a.SupportsPreviousResponseID() {
		t.Fatalf("Moonshot 支持 previous_response_id（3.5 表）")
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

// TestMoonshotStreamFinalUsage 钉住流式 usage（4.3.9：include_usage ⇒ 末帧携带）。
// 错误实现会红：usage 帧缺失 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMoonshotStreamFinalUsage -v
func TestMoonshotStreamFinalUsage(t *testing.T) {
	const sse = "" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":0,\"delta\":\"你好\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"usage\":{\"prompt_tokens\":309,\"completion_tokens\":21,\"total_tokens\":330,\"cached_tokens\":53,\"prompt_tokens_details\":{\"cached_tokens\":53,\"cache_write_tokens\":256}}}}\n\n"
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
	if len(frames) != 3 {
		t.Fatalf("帧数 = %d，期望 3：%+v", len(frames), frames)
	}
	if frames[1].Kind != stream.FrameUsage || frames[1].Usage.InputTokens != 309 || frames[1].Usage.CacheWriteTokens != 256 {
		t.Fatalf("帧[1] = %+v，期望末帧 usage（309/256）", frames[1])
	}
	if frames[2].Kind != stream.FrameDone {
		t.Fatalf("帧[2] = %+v，期望 done", frames[2])
	}
}

// TestMoonshotStreamToolCallName 钉住流式工具调用名进帧（帧承载约定：FrameToolCallArgsDelta.Text
// 载函数名、ToolCallID 载调用 id）：output_item.added 的 item.name 必须以补名帧
// （ToolCallID=call_abc、Text=search）进入帧序列，参数帧照常到（ToolCallID 一致、Arguments 完整）。
// 错误实现会红：不发补名帧 ⇒ 帧序列里只有 Text 为空的参数帧 ⇒ named=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMoonshotStreamToolCallName -v
func TestMoonshotStreamToolCallName(t *testing.T) {
	const sse = "" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"item_id\":\"fc_1\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_abc\",\"name\":\"search\"}}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":1,\"item_id\":\"fc_1\",\"partial_json\":\"{\\\"q\\\":1}\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":2}\n\n" +
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
