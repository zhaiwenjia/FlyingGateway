// 日期: 2026-11-14 14:58:31.628
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

// 本文件覆盖 DeepSeek 适配器（统一 SDD 4.3.4）：
// BuildRequest（/responses、Bearer）、ParseResponse（finish_reason 独有取值超集原值）、
// ParseUsage（命中/未命中双字段：同值取其一 ⛔ 不累加）、流式末帧 usage。

func newDeepSeekResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderDeepSeek,
		Shape:    core.APIShapeOpenAIResponses,
		BaseURL:  base,
		Model:    "deepseek-reasoner",
	}
}

// TestDeepSeekBuildRequestPath 钉住出站路径与鉴权（4.3.4 / 3.6 ② Responses 优先）。
// 错误实现会红：错发 /chat/completions ⇒ URL 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekBuildRequestPath -v
func TestDeepSeekBuildRequestPath(t *testing.T) {
	a := NewDeepSeek(http.DefaultClient)
	out, err := a.BuildRequest(context.Background(), &core.Request{},
		newDeepSeekResolution("https://api.deepseek.com"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "https://api.deepseek.com/responses" {
		t.Fatalf("URL = %s，期望 {base}/responses", out.URL)
	}
	sch := a.AuthScheme()
	if sch.HeaderName != "Authorization" || sch.Prefix != "Bearer " {
		t.Fatalf("AuthScheme = %+v，期望 Authorization/Bearer ", sch)
	}
	if !a.SupportsPreviousResponseID() {
		t.Fatalf("DeepSeek 支持 previous_response_id（3.5 表）")
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
	// insufficient_system_resource 同为 DeepSeek 独有值。
	body := `{"id":"r","status":"completed","output":[],"finish_reason":"insufficient_system_resource"}`
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

// TestDeepSeekStreamFinalUsage 钉住流式末帧 usage（4.3.4：SSE OpenAI 形状，末帧携带 usage）。
// 错误实现会红：usage 帧缺失或位置不在 done 前 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekStreamFinalUsage -v
func TestDeepSeekStreamFinalUsage(t *testing.T) {
	const sse = "" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":0,\"delta\":\"你好\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":11,\"total_tokens\":18,\"prompt_cache_hit_tokens\":0,\"prompt_cache_miss_tokens\":7}}}\n\n"
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
	if len(frames) != 3 {
		t.Fatalf("帧数 = %d，期望 3（text + usage + done）：%+v", len(frames), frames)
	}
	if frames[1].Kind != stream.FrameUsage || frames[1].Usage.InputTokens != 7 || frames[1].Usage.OutputTokens != 11 {
		t.Fatalf("帧[1] = %+v，期望末帧 usage（7/11）", frames[1])
	}
	if frames[2].Kind != stream.FrameDone || frames[2].Status != core.StatusCompleted {
		t.Fatalf("帧[2] = %+v，期望 done completed", frames[2])
	}
}

// TestDeepSeekStreamToolCallName 钉住流式工具调用名进帧（帧承载约定：FrameToolCallArgsDelta.Text
// 载函数名、ToolCallID 载调用 id）：output_item.added 的 item.name 必须以补名帧
// （ToolCallID=call_abc、Text=search）进入帧序列，参数帧照常到（ToolCallID 一致、Arguments 完整）。
// 错误实现会红：不发补名帧 ⇒ 帧序列里只有 Text 为空的参数帧 ⇒ named=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestDeepSeekStreamToolCallName -v
func TestDeepSeekStreamToolCallName(t *testing.T) {
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

	a := NewDeepSeek(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newDeepSeekResolution(srv.URL))
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
