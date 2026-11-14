// 日期: 2026-11-14 14:58:32.070
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

// 本文件覆盖 Ollama 适配器两形态（统一 SDD 4.3.6）：
// ① 原生 /api/chat：NDJSON 流（⛔ 非 SSE、⛔ 无 [DONE]）、usage 只在 done:true 帧、
//    done_reason 三值（stop/load/unload）、usage 平铺字段（prompt_eval_count 含命中、
//    Uncached = prompt_eval_count − prompt_eval_cached_count）；
// ② /v1 兼容层：/v1/chat/completions、⛔ 无 tool_choice（兼容层不支持）、
//    usage 无缓存明细 ⇒ 读写记 0、[DONE] 哨兵结束。

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

// TestOllamaCompatBuildRequest 钉住 /v1 兼容层请求（4.3.6 已知偏差）：
// /v1/chat/completions；⛔ 不发 tool_choice / n / user / logit_bias（兼容层不支持）。
// 错误实现会红：tool_choice 漏进 body ⇒ assertNoKey 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaCompatBuildRequest -v
func TestOllamaCompatBuildRequest(t *testing.T) {
	a := NewOllamaCompat(http.DefaultClient)
	r := &core.Request{
		Messages:   []core.Message{{Role: "user", Content: []core.ContentPart{{Text: "你好"}}}},
		ToolChoice: &core.ToolChoice{Mode: "required"},
		Stream:     true,
	}
	out, err := a.BuildRequest(context.Background(), r, newOllamaCompatResolution("http://127.0.0.1:11434"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "http://127.0.0.1:11434/v1/chat/completions" {
		t.Fatalf("URL = %s，期望 {base}/v1/chat/completions（3.6 ③ Chat 兜底）", out.URL)
	}
	m := bodyMap(t, out)
	assertNoKey(t, m, "tool_choice")
	assertNoKey(t, m, "n")
	assertNoKey(t, m, "user")
	assertNoKey(t, m, "logit_bias")
	// 流式 stream_options.include_usage（OpenAI 形口径）。
	so := mustAt(t, m, "stream_options").(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("stream_options = %v，期望 include_usage:true", so)
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

// TestOllamaCompatStreamDoneSentinel 钉住 /v1 兼容层流式（OpenAI 形 + [DONE] 哨兵结束）。
// 错误实现会红：[DONE] 不收口 ⇒ collectFrames 挂起/报错 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaCompatStreamDoneSentinel -v
func TestOllamaCompatStreamDoneSentinel(t *testing.T) {
	const body = "" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":12,\"total_tokens\":42}}\n\n" +
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
		t.Fatalf("帧数 = %d，期望 3（text + done + usage）：%+v", len(frames), frames)
	}
	if frames[0].Kind != stream.FrameTextDelta || frames[0].Text != "你好" {
		t.Fatalf("帧[0] = %+v", frames[0])
	}
	if frames[1].Kind != stream.FrameDone || frames[1].Status != core.FinishStop {
		t.Fatalf("帧[1] = %+v，期望 done/stop", frames[1])
	}
	if frames[2].Kind != stream.FrameUsage || frames[2].Usage.InputTokens != 30 {
		t.Fatalf("帧[2] = %+v，期望 usage（30/12）", frames[2])
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
