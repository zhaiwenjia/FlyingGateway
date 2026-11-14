// 日期: 2026-11-14 16:38:51.679
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

// 本文件覆盖智谱 GLM 适配器（统一 SDD 4.3.8；出站 Chat Completions）：
// BuildRequest（流式参数是 stream_options.chunk_include_usage ⛔ 不是 include_usage——
// 照搬会静默拿不到 usage；reasoning_effort 顶层字段）、ParseResponse（sensitive / network_error /
// model_context_window_exceeded 独有 / 非标准取值超集原值）、ParseUsage（命中单列、无写入）、
// 真实流样本直喂（testdata/real/zhipu_stream_docs_example.txt）。

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
	if out.Text != "被拦截" {
		t.Fatalf("Text = %q，期望 被拦截", out.Text)
	}
	for _, want := range []string{core.FinishNetworkError, core.FinishModelContextWindowExceeded} {
		body := `{"id":"r","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"` + want + `"}]}`
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

// TestZhipuStreamDocsExample 钉住真实流样本直喂（2026-11-14 改形验收点）：
// testdata/real/zhipu_stream_docs_example.txt（官方文档「响应示例」块逐字，含 `...` 省略行、
// ⛔ 不补造）经 StreamStart 直喂 ⇒ 帧序列与样本逐字一致：文本增量 春 / 天 / 来 +
// 末帧 usage（8/262）+ done（finish_reason=stop 超集原值）。
// 错误实现会红：`...` 省略行中断解析 ⇒ 帧缺失红；usage 与 done 顺序颠倒 ⇒ 帧序红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuStreamDocsExample -v
func TestZhipuStreamDocsExample(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(loadFixture(t, "real/zhipu_stream_docs_example.txt"))
	}))
	defer srv.Close()

	a := NewZhipu(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newZhipuResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v（`...` 省略行必须容忍）", cerr)
	}
	if len(frames) != 5 {
		t.Fatalf("帧数 = %d，期望 5（春/天/来 + usage + done）：%+v", len(frames), frames)
	}
	for i, want := range []string{"春", "天", "来"} {
		if frames[i].Kind != stream.FrameTextDelta || frames[i].Text != want {
			t.Fatalf("帧[%d] = %+v，期望文本增量 %q（样本逐字）", i, frames[i], want)
		}
	}
	u := frames[3]
	if u.Kind != stream.FrameUsage || u.Usage.InputTokens != 8 || u.Usage.OutputTokens != 262 {
		t.Fatalf("帧[3] = %+v，期望 usage（8/262，样本逐字）", u)
	}
	if frames[4].Kind != stream.FrameDone || frames[4].Status != "stop" {
		t.Fatalf("帧[4] = %+v，期望 done stop（finish_reason 超集原值）", frames[4])
	}
}

// TestZhipuStreamToolCallName 钉住 chat chunk 的工具帧与推理增量（帧承载约定：
// FrameToolCallArgsDelta.Text 载函数名、ToolCallID 载调用 id）：首块带名进补名帧、
// 后续块按 index 对齐同一调用 id 传参数增量、delta.reasoning_content（扩展名逐字）进推理帧。
// 错误实现会红：不发补名帧 ⇒ named=0 ⇒ 红；index 不对齐 ⇒ 参数帧 ToolCallID 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestZhipuStreamToolCallName -v
func TestZhipuStreamToolCallName(t *testing.T) {
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

	a := NewZhipu(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newZhipuResolution(srv.URL))
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
