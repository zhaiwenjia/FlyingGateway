// 日期: 2026-11-14 14:58:32.070
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件覆盖 Ollama 适配器两形态（统一 SDD 4.3.6）：
// ① 原生 /api/chat：NDJSON 流（⛔ 非 SSE、⛔ 无 [DONE]）、usage 只在 done:true 帧、
//    done_reason 三值（stop/load/unload）、usage 平铺字段（prompt_eval_count 含命中、
//    Uncached = prompt_eval_count − prompt_eval_cached_count）；
// ② /v1 兼容层（出站已 Responses 化）：POST {base}/v1/responses、⛔ 无 tool_choice（兼容层不支持
//    ⇒ 剔除 + 降级记录）、previous_response_id 不支持（无状态 flavor）⇒ 显式报错、
//    ParseResponse / 流式用真实样本直喂（testdata/real/ollama_responses_*_real.json / .sse，
//    response.completed 收口、无 [DONE]）。

func newOllamaResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderOllama,
		Shape:    core.APIShapeOllamaNative,
		BaseURL:  base,
		Model:    "qwen3:8b",
	}
}

func newOllamaCompatResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderOllama,
		Shape:    core.APIShapeOpenAIChat,
		BaseURL:  base,
		Model:    "qwen3:8b",
	}
}

// TestOllamaNativeBuildRequest 钉住原生请求（4.3.6 请求体关键字段）：
// /api/chat；think 参数（effort 原值 / 其余非空 ⇒ true / none 空 ⇒ 不发）；
// temperature 等进 options（num_predict）。
// 错误实现会红：think 映射错档 ⇒ 断言红；参数放顶层 ⇒ options 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaNativeBuildRequest -v
func TestOllamaNativeBuildRequest(t *testing.T) {
	a := NewOllama(http.DefaultClient)
	temp, topP := 0.7, 0.9
	r := &core.Request{
		Messages:        []core.Message{{Role: "user", Content: []core.ContentPart{{Text: "你好"}}}},
		Temperature:     &temp,
		TopP:            &topP,
		MaxOutputTokens: 100,
		Reasoning:       &core.ReasoningConfig{Effort: "high"},
	}
	out, err := a.BuildRequest(context.Background(), r, newOllamaResolution("http://127.0.0.1:11434"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "http://127.0.0.1:11434/api/chat" {
		t.Fatalf("URL = %s，期望 {base}/api/chat（4.3.6 原生）", out.URL)
	}
	m := bodyMap(t, out)
	if mustAt(t, m, "think") != "high" {
		t.Fatalf("think = %v，期望 \"high\"（effort 原值，4.3.6：think 是 bool 或 low/medium/high/max）", m["think"])
	}
	opts := mustAt(t, m, "options").(map[string]any)
	if opts["temperature"] != 0.7 || opts["top_p"] != 0.9 || opts["num_predict"] != float64(100) {
		t.Fatalf("options = %v，期望 temperature/top_p/num_predict 进 options", opts)
	}
	// effort 非枚举值 ⇒ true；none/空 ⇒ 不发。
	r.Reasoning = &core.ReasoningConfig{Effort: "xhigh"}
	out, _ = a.BuildRequest(context.Background(), r, newOllamaResolution("http://127.0.0.1:11434"))
	if mustAt(t, bodyMap(t, out), "think") != true {
		t.Fatalf("think = %v，期望 true（非枚举 effort）", bodyMap(t, out)["think"])
	}
	r.Reasoning = &core.ReasoningConfig{Effort: "none"}
	out, _ = a.BuildRequest(context.Background(), r, newOllamaResolution("http://127.0.0.1:11434"))
	assertNoKey(t, bodyMap(t, out), "think")
	// 鉴权：本地进程无鉴权（4.3.6）⇒ AuthScheme 空。
	if sch := a.AuthScheme(); sch.HeaderName != "" {
		t.Fatalf("AuthScheme = %+v，期望空（本地无鉴权）", sch)
	}
}

// TestOllamaCompatBuildRequest 钉住 /v1 兼容层 Responses 形请求（4.3.6 家差 + 3.5/3.6）：
// ① URL {base}/v1/responses；② 请求体 Responses 形（input 数组 / instructions 顶层 / tools 扁平形）；
// ③ ⛔ 不发 tool_choice / n / user / logit_bias（4.3.6 已知偏差；官方 Supported features 仅
//
//	Streaming / Tools (function calling) / Reasoning summaries）⇒ tool_choice 出站剔除 + 降级记录
//	（DegradationAware，⛔ 不静默丢弃无标记）；
//
// ④ stream:true（Responses 形 usage 随 response.completed，⛔ 无 stream_options）；
// ⑤ Extra 透传（mergeExtraBody 黑名单口径）。
// 错误实现会红：tool_choice 漏进 body ⇒ assertNoKey 红；降级不记 ⇒ degs 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaCompatBuildRequest -v
func TestOllamaCompatBuildRequest(t *testing.T) {
	a := NewOllamaCompat(http.DefaultClient)
	r := &core.Request{
		Instructions: "主指令",
		Messages: []core.Message{
			{Role: "system", Content: []core.ContentPart{{Text: "系统提示"}}},
			{Role: "user", Content: []core.ContentPart{{Text: "你好"}}},
		},
		Tools: []core.Tool{{Type: "function", Name: "get_weather", Description: "查天气",
			Parameters: []byte(`{"type":"object"}`)}},
		ToolChoice: &core.ToolChoice{Mode: "required"},
		Stream:     true,
		Extra:      map[string]any{"think": true},
	}
	out, degs, err := a.(*ollamaCompatAdapter).BuildRequestWithDegradations(context.Background(), r, newOllamaCompatResolution("http://127.0.0.1:11434"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "http://127.0.0.1:11434/v1/responses" {
		t.Fatalf("URL = %s，期望 {base}/v1/responses（v0.13.3+ Responses，无状态 flavor）", out.URL)
	}
	m := bodyMap(t, out)
	// ② Responses 形：input 数组 + instructions 顶层 + tools 扁平形。
	items := mustSlice(t, m, "input")
	if len(items) != 1 {
		t.Fatalf("input item 数 = %d，期望 1（system 并入 instructions）：%v", len(items), items)
	}
	if mustString(t, m, "instructions") != "主指令\n系统提示" {
		t.Fatalf("instructions = %q，期望 %q", m["instructions"], "主指令\n系统提示")
	}
	t0 := mustSlice(t, m, "tools")[0].(map[string]any)
	if t0["type"] != "function" || t0["name"] != "get_weather" {
		t.Fatalf("tools[0] = %v，期望扁平形 {type:function,name:get_weather}", t0)
	}
	if _, nested := t0["function"]; nested {
		t.Fatalf("tools[0] 含 function 嵌套层 %v，期望扁平形（Responses 规范）", t0)
	}
	// ③ 已知偏差字段不发 + tool_choice 降级记录（⛔ 不静默丢弃）。
	assertNoKey(t, m, "tool_choice")
	assertNoKey(t, m, "n")
	assertNoKey(t, m, "user")
	assertNoKey(t, m, "logit_bias")
	if len(degs) != 1 || degs[0].Field != "tool_choice" || degs[0].Reason != core.DegradationCapabilityUnsupported {
		t.Fatalf("degradations = %+v，期望 1 条 tool_choice 降级（⛔ 不静默丢弃无标记）", degs)
	}
	// ④ stream:true、⛔ 无 stream_options。
	if mustAt(t, m, "stream") != true {
		t.Fatalf("stream = %v，期望 true", m["stream"])
	}
	assertNoKey(t, m, "stream_options")
	// ⑤ Extra 透传。
	if mustAt(t, m, "think") != true {
		t.Fatalf("think = %v，期望 true（Extra 透传）", m["think"])
	}
}

// TestOllamaPreviousResponseIDRejected 钉住 3.5：Ollama 两形态都不支持服务端会话 ⇒ 显式报错
// （/v1/responses 仅无状态 flavor，4.3.6）。
// 错误实现会红：静默丢弃 ⇒ err==nil ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaPreviousResponseIDRejected -v
func TestOllamaPreviousResponseIDRejected(t *testing.T) {
	for _, a := range []Adapter{NewOllama(http.DefaultClient), NewOllamaCompat(http.DefaultClient)} {
		_, err := a.BuildRequest(context.Background(),
			&core.Request{PreviousResponseID: "resp_prev"}, newOllamaResolution("http://127.0.0.1:11434"))
		requireDetailErr(t, err, "unsupported_capability")
	}
}

// TestOllamaParseResponseNative 钉住原生响应解析与平铺 usage（4.3.6 / 4.4.2 / 11.1「Ollama 平铺」）：
// prompt_eval_count=26 是总输入（含命中）、prompt_eval_cached_count=13 ⇒ 缓存读、
// Uncached = 26−13 = 13（⛔ 不能拿 26 当未缓存量）；done_reason 三值超集原值、空且 done ⇒ stop。
// 错误实现会红：Uncached 取 prompt_eval_count ⇒ 26 ⇒ 红；done_reason 被并入 OpenAI 枚举 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaParseResponseNative -v
func TestOllamaParseResponseNative(t *testing.T) {
	a := NewOllama(http.DefaultClient)
	out, err := a.ParseResponse(newOllamaResolution(""), loadFixture(t, "ollama_response.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if out.Text != "你好" || out.FinishReason != core.FinishStop {
		t.Fatalf("解析不对：Text=%q Finish=%q", out.Text, out.FinishReason)
	}
	u := out.Usage
	if u.InputTokens != 26 || u.CacheReadTokens != 13 || u.OutputTokens != 18 {
		t.Fatalf("平铺归一不对：In=%d(26) Read=%d(13) Out=%d(18)", u.InputTokens, u.CacheReadTokens, u.OutputTokens)
	}
	if u.InputTokensUncached != 13 {
		t.Fatalf("Uncached = %d，期望 13 = 26−13（4.4.3 Ollama 坑）", u.InputTokensUncached)
	}
	// done_reason 三值（4.5.2）+ 空且 done ⇒ stop。
	for _, c := range []struct {
		body, want string
	}{
		{`{"message":{"content":"x"},"done":true,"done_reason":"load"}`, core.FinishLoad},
		{`{"message":{"content":"x"},"done":true,"done_reason":"unload"}`, core.FinishUnload},
		{`{"message":{"content":"x"},"done":true}`, core.FinishStop},
	} {
		out, err := a.ParseResponse(newOllamaResolution(""), []byte(c.body))
		if err != nil {
			t.Fatalf("ParseResponse(%s) 报错: %v", c.body, err)
		}
		if out.FinishReason != c.want {
			t.Fatalf("body=%s ⇒ FinishReason=%q，期望 %q", c.body, out.FinishReason, c.want)
		}
	}
}

// TestOllamaCompatParseUsage 钉住 /v1 兼容层 usage（4.4.2 Ollama /v1 行）：
// 仅 prompt/completion/total 三字段，无缓存明细 ⇒ 读写记 0。
// 错误实现会红：按原生平铺字段解 ⇒ In=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaCompatParseUsage -v
func TestOllamaCompatParseUsage(t *testing.T) {
	a := NewOllamaCompat(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "ollama_v1_usage.json"))
	if u.InputTokens != 30 || u.OutputTokens != 12 {
		t.Fatalf("归一不对：In=%d(30) Out=%d(12)", u.InputTokens, u.OutputTokens)
	}
	if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		t.Fatalf("/v1 无缓存明细 ⇒ 记 0：Read=%d Write=%d", u.CacheReadTokens, u.CacheWriteTokens)
	}
}

// TestOllamaNativeStreamNDJSON 钉住原生流式（3.6 坑 2 / 4.3.6）：
// NDJSON 一行一帧（⛔ 非 SSE、⛔ 无 [DONE]）；usage 只在 done:true 帧；done_reason 帧载终态。
// 错误实现会红：按 SSE 解析 NDJSON ⇒ 帧全丢 ⇒ 断言红；usage 逐帧乱记 ⇒ 帧数断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaNativeStreamNDJSON -v
func TestOllamaNativeStreamNDJSON(t *testing.T) {
	const body = "" +
		"{\"model\":\"qwen3:8b\",\"message\":{\"role\":\"assistant\",\"content\":\"你\"},\"done\":false}\n" +
		"{\"model\":\"qwen3:8b\",\"message\":{\"role\":\"assistant\",\"content\":\"好\"},\"done\":false}\n" +
		"{\"model\":\"qwen3:8b\",\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":true,\"done_reason\":\"stop\",\"prompt_eval_count\":26,\"prompt_eval_cached_count\":13,\"eval_count\":18}\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	a := NewOllama(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newOllamaResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v（⛔ 无 [DONE]，以 done:true 帧判终态）", cerr)
	}
	var texts []string
	usageCount := 0
	var doneStatus string
	for _, f := range frames {
		switch f.Kind {
		case stream.FrameTextDelta:
			texts = append(texts, f.Text)
		case stream.FrameUsage:
			usageCount++
			if f.Usage.InputTokens != 26 || f.Usage.CacheReadTokens != 13 || f.Usage.OutputTokens != 18 {
				t.Fatalf("usage 帧 = %+v，期望 26/13/18（只在 done:true 帧）", f.Usage)
			}
		case stream.FrameDone:
			doneStatus = f.Status
		}
	}
	if len(texts) != 2 || texts[0] != "你" || texts[1] != "好" {
		t.Fatalf("文本增量 = %v，期望 [你 好]", texts)
	}
	if usageCount != 1 {
		t.Fatalf("usage 帧数 = %d，期望 1（usage 只在 done:true 帧，4.3.6）", usageCount)
	}
	if doneStatus != core.FinishStop {
		t.Fatalf("done 状态 = %q，期望 stop（done_reason 原值）", doneStatus)
	}
}

// TestOllamaCompatStreamDoneSentinel 钉住 /v1 兼容层流式收口语义（wire 迁移后主题不变）：
// Responses 形以 response.completed 收口（usage 自带 ⇒ Usage 帧先于 Done 帧）；[DONE] 是 Chat 形
// 哨兵，出现时容忍（真机流无 [DONE]，见 TestOllamaCompatStreamRealResponses）。
// 错误实现会红：不收口 ⇒ collectFrames 挂起/报错 ⇒ 红；usage 丢失 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaCompatStreamDoneSentinel -v
func TestOllamaCompatStreamDoneSentinel(t *testing.T) {
	const body = "" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","sequence_number":0,"delta":"你好"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","sequence_number":1,"response":{"usage":{"input_tokens":30,"output_tokens":12,"total_tokens":42}}}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	a := NewOllamaCompat(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newOllamaCompatResolution(srv.URL))
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
	if frames[0].Kind != stream.FrameTextDelta || frames[0].Text != "你好" {
		t.Fatalf("帧[0] = %+v", frames[0])
	}
	if frames[1].Kind != stream.FrameUsage || frames[1].Usage.InputTokens != 30 {
		t.Fatalf("帧[1] = %+v，期望 usage（30/12，response.completed 自带、先于 Done）", frames[1])
	}
	if frames[2].Kind != stream.FrameDone || frames[2].Status != core.StatusCompleted {
		t.Fatalf("帧[2] = %+v，期望 done/completed（response.completed 收口）", frames[2])
	}
}

// TestOllamaCompatParseResponseReal 钉住真实样本直喂 ParseResponse（testdata/real/
// ollama_responses_*_real.json，2026-11-16 本机 qwen3.5:2b 直采，来源见 testdata/real/README.md）：
// basic：output[reasoning, message]、encrypted_content / summary 保留、真机 message 文本为空、
// usage Responses 树逐字（input 11 / output 256 / total 267）；
// tools：output[reasoning, function_call]（call_id/name/arguments 逐字）、finish ⇒ tool_calls。
// 错误实现会红：按 chat 形解析 ⇒ items 全丢 ⇒ 断言红；usage 树混读 ⇒ 数值 0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaCompatParseResponseReal -v
func TestOllamaCompatParseResponseReal(t *testing.T) {
	a := NewOllamaCompat(http.DefaultClient)
	out, err := a.ParseResponse(newOllamaCompatResolution(""), loadFixture(t, "real/ollama_responses_basic_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse(basic) 报错: %v", err)
	}
	if out.Status != core.StatusCompleted || out.FinishReason != core.FinishStop {
		t.Fatalf("basic 解析 = %q/%q，期望 completed/stop", out.Status, out.FinishReason)
	}
	if out.Text != "" {
		t.Fatalf("Text = %q，期望空（真机样本 message 文本为空，输出预算被 reasoning 占满）", out.Text)
	}
	if len(out.OutputItems) != 2 || out.OutputItems[0].Type != "reasoning" || out.OutputItems[1].Type != "message" {
		t.Fatalf("basic OutputItems = %+v，期望 [reasoning, message]", out.OutputItems)
	}
	rs := out.OutputItems[0]
	if rs.EncryptedContent == "" || len(rs.Summary) != 1 || rs.Summary[0].Text == "" {
		t.Fatalf("reasoning item = %+v，期望 encrypted_content + summary 保留", rs)
	}
	u := out.Usage
	if u.InputTokens != 11 || u.CacheReadTokens != 0 || u.OutputTokens != 256 || u.TotalTokens != 267 {
		t.Fatalf("basic usage = In%d/Read%d/Out%d/Total%d，期望 11/0/256/267（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.OutputTokens, u.TotalTokens)
	}
	out, err = a.ParseResponse(newOllamaCompatResolution(""), loadFixture(t, "real/ollama_responses_tools_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse(tools) 报错: %v", err)
	}
	if out.FinishReason != core.FinishToolCalls {
		t.Fatalf("tools FinishReason = %q，期望 tool_calls（completed + function_call 推导）", out.FinishReason)
	}
	if len(out.OutputItems) != 2 {
		t.Fatalf("tools OutputItems = %+v，期望 2 项", out.OutputItems)
	}
	fc := out.OutputItems[1]
	if fc.Type != "function_call" || fc.CallID != "call_q2sb8jru" || fc.Name != "get_weather" || fc.Arguments != "{\"city\":\"北京\"}" {
		t.Fatalf("function_call item = %+v，期望 call_q2sb8jru/get_weather 逐字", fc)
	}
	if out.Usage.InputTokens != 270 || out.Usage.OutputTokens != 75 || out.Usage.TotalTokens != 345 {
		t.Fatalf("tools usage = %d/%d/%d，期望 270/75/345（样本逐字）",
			out.Usage.InputTokens, out.Usage.OutputTokens, out.Usage.TotalTokens)
	}
}

// TestOllamaCompatStreamRealResponses 钉住真实流样本直喂解码器（testdata/real/
// ollama_responses_stream_real.sse，2026-11-16 本机 qwen3.5:2b 逐帧直采）：
// response.completed 收口（无 [DONE]）；帧序列 Lifecycle×2 → ReasoningDelta×213 → TextDelta×35
// → Usage → Done（事件含 response.reasoning_summary_text.delta）；usage 由 response.completed 自带。
// 错误实现会红：帧序/帧种错配 ⇒ 序列断言红；usage 丢失 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaCompatStreamRealResponses -v
func TestOllamaCompatStreamRealResponses(t *testing.T) {
	f, err := os.Open("testdata/real/ollama_responses_stream_real.sse")
	if err != nil {
		t.Fatalf("打开真实样本失败: %v", err)
	}
	defer f.Close()
	a := NewOllamaCompat(http.DefaultClient)
	d := a.(*ollamaCompatAdapter).streamDecoder()
	sc := stream.NewSSEScanner(f)
	var frames []stream.Frame
	done := false
	for sc.Scan() {
		fs, dn, err := d.decode(sc.Event())
		if err != nil {
			t.Fatalf("decode 出错: %v", err)
		}
		frames = append(frames, fs...)
		if dn {
			done = true
			break
		}
	}
	if !done {
		t.Fatal("response.completed 未收口（真机流无 [DONE]，以 response.completed 判终态）")
	}
	// 帧序列核对：Kind 分组边界 + 逐段计数。
	wantKinds := []struct {
		kind  stream.FrameKind
		count int
	}{
		{stream.FrameLifecycle, 2},
		{stream.FrameReasoningDelta, 213},
		{stream.FrameTextDelta, 35},
		{stream.FrameUsage, 1},
		{stream.FrameDone, 1},
	}
	var wantSeq []stream.FrameKind
	for _, w := range wantKinds {
		for i := 0; i < w.count; i++ {
			wantSeq = append(wantSeq, w.kind)
		}
	}
	if len(frames) != len(wantSeq) {
		t.Fatalf("帧数 = %d，期望 %d", len(frames), len(wantSeq))
	}
	var think, text string
	for i, fr := range frames {
		if fr.Kind != wantSeq[i] {
			t.Fatalf("帧[%d].Kind = %v，期望 %v（Lifecycle×2→ReasoningDelta×213→TextDelta×35→Usage→Done）", i, fr.Kind, wantSeq[i])
		}
		switch fr.Kind {
		case stream.FrameReasoningDelta:
			think += fr.Reasoning
		case stream.FrameTextDelta:
			text += fr.Text
		}
	}
	if len([]rune(think)) != 422 || !strings.HasPrefix(think, "嗯，用户发的是“说好”，看起来非常简短。") {
		t.Fatalf("推理增量拼接 = %d 字，前缀不对（期望 422 字、以「嗯，用户发的是“说好”」开头）", len([]rune(think)))
	}
	const wantText = "抱歉，我无法提供涉及保密或个人隐私的内容。如果您有其他问题需要帮助，比如如何保护个人信息、法律常识，或者日常生活的实用建议，我很乐意为您解答！😊"
	if text != wantText {
		t.Fatalf("正文增量拼接 = %q，期望 %q", text, wantText)
	}
	usageFrame := frames[len(frames)-2]
	if usageFrame.Usage == nil || usageFrame.Usage.InputTokens != 11 || usageFrame.Usage.OutputTokens != 256 || usageFrame.Usage.TotalTokens != 267 {
		t.Fatalf("usage 帧 = %+v，期望 11/256/267（response.completed 自带，样本逐字）", usageFrame.Usage)
	}
	if frames[len(frames)-1].Status != core.StatusCompleted {
		t.Fatalf("Done.Status = %q，期望 completed", frames[len(frames)-1].Status)
	}
}

// TestOllamaParseUsageRealSample 钉住真实样本（排障实录贴真机响应 Ollama 0.40.14 + 官方文档示例，
// 来源见 testdata/real/README.md）上 ParseUsage 归一成立：
// prompt_eval_count=40 是总输入（含命中）⇒ Uncached = 40 − 24 = 16（⛔ 不能拿 prompt_eval_count 当未缓存量）；
// 官方示例无 prompt_eval_cached_count ⇒ Read=0、Uncached=11；两者都无总值原报 ⇒ Total=0。
// 错误实现会红：Uncached=40 ≠ 16 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaParseUsageRealSample -v
func TestOllamaParseUsageRealSample(t *testing.T) {
	a := NewOllama(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/ollama_usage_real.json"))
	if u.InputTokens != 40 || u.CacheReadTokens != 24 || u.InputTokensUncached != 16 || u.OutputTokens != 10 || u.TotalTokens != 0 {
		t.Fatalf("真机样本归一 = In%d/Read%d/Unc%d/Out%d/Total%d，期望 40/24/16/10/0（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.InputTokensUncached, u.OutputTokens, u.TotalTokens)
	}
	u = a.ParseUsage(loadFixture(t, "real/ollama_usage_docs.json"))
	if u.InputTokens != 11 || u.CacheReadTokens != 0 || u.InputTokensUncached != 11 || u.OutputTokens != 18 {
		t.Fatalf("文档样本归一 = In%d/Read%d/Unc%d/Out%d，期望 11/0/11/18（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.InputTokensUncached, u.OutputTokens)
	}
}
