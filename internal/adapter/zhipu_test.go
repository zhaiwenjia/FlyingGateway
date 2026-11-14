// 日期: 2026-11-14 14:58:31.824
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

// 本文件覆盖智谱 GLM 适配器（统一 SDD 4.3.8）：
// BuildRequest（流式参数是 stream_options.chunk_include_usage ⛔ 不是 include_usage——
// 照搬会静默拿不到 usage）、ParseResponse（sensitive / network_error /
// model_context_window_exceeded 独有 / 非标准取值超集原值）、ParseUsage（命中单列、无写入）。

func newZhipuResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderZhipu,
		Shape:    core.APIShapeOpenAIResponses,
		BaseURL:  base,
		Model:    "glm-4.7",
	}
}

// TestZhipuStreamOptionsChunkIncludeUsage 钉住智谱流式参数名（4.3.8 逐字）：
// stream_options.chunk_include_usage=true（⛔ 不是 include_usage）；非流式不发 stream_options。
// 错误实现会红：照搬 OpenAI 的 include_usage ⇒ 键名断言红（且线上会静默拿不到 usage）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuStreamOptionsChunkIncludeUsage -v
func TestZhipuStreamOptionsChunkIncludeUsage(t *testing.T) {
	a := NewZhipu(http.DefaultClient)
	r := &core.Request{
		Messages:  []core.Message{{Role: "user", Content: []core.ContentPart{{Text: "你好"}}}},
		Stream:    true,
		Reasoning: &core.ReasoningConfig{Effort: "high"},
	}
	out, err := a.BuildRequest(context.Background(), r, newZhipuResolution("https://open.bigmodel.cn/api/paas/v4"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	m := bodyMap(t, out)
	so := mustAt(t, m, "stream_options").(map[string]any)
	v, ok := so["chunk_include_usage"]
	if !ok || v != true {
		t.Fatalf("stream_options = %v，期望 {chunk_include_usage:true}（⛔ 非 include_usage）", so)
	}
	assertNoKey(t, m, "include_usage")
	if _, ok := so["include_usage"]; ok {
		t.Fatalf("stream_options 含 include_usage（⛔ 智谱参数名是 chunk_include_usage）: %v", so)
	}
	// reasoning_effort 顶层字段（4.3.8）。
	if mustString(t, m, "reasoning_effort") != "high" {
		t.Fatalf("reasoning_effort = %v", m["reasoning_effort"])
	}

	// 非流式 ⇒ ⛔ 不发 stream_options。
	r.Stream = false
	out, err = a.BuildRequest(context.Background(), r, newZhipuResolution("https://open.bigmodel.cn/api/paas/v4"))
	if err != nil {
		t.Fatalf("BuildRequest(非流式) 报错: %v", err)
	}
	assertNoKey(t, bodyMap(t, out), "stream_options")
}

// TestZhipuParseResponseFinishSuperset 钉住智谱独有 / 非标准 finish_reason（4.5.1 / 4.5.2）：
// sensitive / network_error / model_context_window_exceeded ⇒ 超集原值（⛔ 不得当作未知值丢弃）。
// 错误实现会红：被归并成 unknown ⇒ 原值断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuParseResponseFinishSuperset -v
func TestZhipuParseResponseFinishSuperset(t *testing.T) {
	a := NewZhipu(http.DefaultClient)
	out, err := a.ParseResponse(newZhipuResolution(""), loadFixture(t, "zhipu_response.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if out.FinishReason != core.FinishSensitive {
		t.Fatalf("FinishReason = %q，期望 sensitive（智谱独有，4.5.1）", out.FinishReason)
	}
	for _, want := range []string{core.FinishNetworkError, core.FinishModelContextWindowExceeded} {
		body := `{"id":"r","status":"completed","output":[],"finish_reason":"` + want + `"}`
		out, err := a.ParseResponse(newZhipuResolution(""), []byte(body))
		if err != nil {
			t.Fatalf("ParseResponse(%s) 报错: %v", want, err)
		}
		if out.FinishReason != want {
			t.Fatalf("FinishReason = %q，期望超集原值 %q", out.FinishReason, want)
		}
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

// TestZhipuToolChoiceDegradationRecorded 钉住 1.4.2 不静默降级：tool_choice 非 auto
// 降为 auto 必须产生 Degradation 记录（4.3.8）。错误实现对照：只降不记 ⇒ 本测试必红。
func TestZhipuToolChoiceDegradationRecorded(t *testing.T) {
	a := NewZhipu(&http.Client{})
	tval := 0.7
	r := &core.Request{
		Model:       "glm-5.3",
		Messages:    []core.Message{{Role: "user", Content: []core.ContentPart{{Type: "text", Text: "hi"}}}},
		ToolChoice:  &core.ToolChoice{Mode: "required"},
		Temperature: &tval,
	}
	out, degs, err := a.(DegradationAware).BuildRequestWithDegradations(context.Background(), r, &core.Resolution{Model: "glm-5.3"})
	if err != nil {
		t.Fatalf("BuildRequestWithDegradations 失败: %v", err)
	}
	if out == nil {
		t.Fatal("Outbound 为 nil")
	}
	found := false
	for _, d := range degs {
		if d.Field == "tool_choice" && d.Reason == core.DegradationCapabilityUnsupported {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool_choice 降级未记 degradation（degs=%v），违反 1.4.2 不静默降级", degs)
	}
	var body map[string]any
	if err := json.Unmarshal(out.Body, &body); err != nil {
		t.Fatalf("请求体解析失败: %v", err)
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v，期望降为 auto", body["tool_choice"])
	}
}

// TestZhipuStreamToolCallName 钉住流式工具调用名进帧（帧承载约定：FrameToolCallArgsDelta.Text
// 载函数名、ToolCallID 载调用 id）：output_item.added 的 item.name 必须以补名帧
// （ToolCallID=call_abc、Text=search）进入帧序列，参数帧照常到（ToolCallID 一致、Arguments 完整）。
// 错误实现会红：不发补名帧 ⇒ 帧序列里只有 Text 为空的参数帧 ⇒ named=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuStreamToolCallName -v
func TestZhipuStreamToolCallName(t *testing.T) {
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

	a := NewZhipu(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newZhipuResolution(srv.URL))
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
