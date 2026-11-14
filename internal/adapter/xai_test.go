// 日期: 2026-11-14 14:58:31.694
// 作者: Zhai Wenjia

package adapter

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件覆盖 Grok / xAI 适配器（统一 SDD 4.3.5）：
// BuildRequest（/responses、Bearer）、ParseUsage（Chat 树 cached_prompt_text_tokens；
// Responses 树 cost_in_usd_ticks 随 Raw 保留，5.6 ① 自报费用优先）、流式末帧 usage。

func newXAIResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderXAI,
		Shape:    core.APIShapeOpenAIResponses,
		BaseURL:  base,
		Model:    "grok-4.7",
	}
}

// TestXAIBuildRequestPath 钉住出站路径与鉴权（4.3.5：/v1/responses 为官方推荐）。
// 错误实现会红：错发 /v1/chat/completions ⇒ URL 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXAIBuildRequestPath -v
func TestXAIBuildRequestPath(t *testing.T) {
	a := NewXAI(http.DefaultClient)
	out, err := a.BuildRequest(context.Background(), &core.Request{},
		newXAIResolution("https://api.x.ai/v1"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "https://api.x.ai/v1/responses" {
		t.Fatalf("URL = %s，期望 {base}/responses", out.URL)
	}
	sch := a.AuthScheme()
	if sch.HeaderName != "Authorization" || sch.Prefix != "Bearer " {
		t.Fatalf("AuthScheme = %+v", sch)
	}
	if !a.SupportsPreviousResponseID() {
		t.Fatalf("xAI 支持 previous_response_id（3.5 表）")
	}
}

// TestXAIParseUsageChatTree 钉住 Grok Chat usage 归一（4.3.5 / 4.4.2 Grok Chat 行）：
// 命中 = cached_prompt_text_tokens = 40；In=100、Out=20、Uncached=60、reasoning=5。
// 错误实现会红：命中取 cached_tokens（不存在）⇒ Read=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXAIParseUsageChatTree -v
func TestXAIParseUsageChatTree(t *testing.T) {
	a := NewXAI(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "xai_chat_usage.json"))
	if u.InputTokens != 100 || u.CacheReadTokens != 40 || u.OutputTokens != 20 {
		t.Fatalf("归一不对：In=%d(100) Read=%d(40) Out=%d(20)",
			u.InputTokens, u.CacheReadTokens, u.OutputTokens)
	}
	if u.InputTokensUncached != 60 {
		t.Fatalf("Uncached = %d，期望 60 = 100−40", u.InputTokensUncached)
	}
	if u.ReasoningTokens != 5 {
		t.Fatalf("ReasoningTokens = %d，期望 5（reasoning_tokens 单列）", u.ReasoningTokens)
	}
}

// TestXAIParseUsageResponsesTreeRawCost 钉住 Responses 树归一 + cost_in_usd_ticks 随 Raw 保留
// （4.3.5：$1 USD = 10^10 ticks，实际计费额由 cost 层换算；usage 层只留原文，5.6 ①）。
// 错误实现会红：Raw 未逐字保留 ⇒ ticks 断言红；缓存字段误记非 0 ⇒ Read 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXAIParseUsageResponsesTreeRawCost -v
func TestXAIParseUsageResponsesTreeRawCost(t *testing.T) {
	a := NewXAI(http.DefaultClient)
	raw := loadFixture(t, "xai_responses_usage.json")
	u := a.ParseUsage(raw)
	if u.InputTokens != 200 || u.OutputTokens != 50 {
		t.Fatalf("归一不对：In=%d(200) Out=%d(50)", u.InputTokens, u.OutputTokens)
	}
	if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		t.Fatalf("Grok Responses 无缓存字段 ⇒ 记 0：Read=%d Write=%d", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if !bytes.Contains(u.Raw, []byte("987654321")) {
		t.Fatalf("cost_in_usd_ticks 未随 Raw 保留: %s（5.6 ① 自报费用优先）", u.Raw)
	}
}

// TestXAIStreamFinalUsage 钉住 Responses 流式末帧 usage（4.3.5）。
// 错误实现会红：usage 帧缺失 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXAIStreamFinalUsage -v
func TestXAIStreamFinalUsage(t *testing.T) {
	const sse = "" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":0,\"delta\":\"你好\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"usage\":{\"input_tokens\":200,\"output_tokens\":50,\"total_tokens\":250,\"cost_in_usd_ticks\":987654321}}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewXAI(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newXAIResolution(srv.URL))
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
	if frames[1].Kind != stream.FrameUsage || frames[1].Usage.InputTokens != 200 {
		t.Fatalf("帧[1] = %+v，期望末帧 usage", frames[1])
	}
	if frames[2].Kind != stream.FrameDone || frames[2].Status != core.StatusCompleted {
		t.Fatalf("帧[2] = %+v，期望 done", frames[2])
	}
}

// TestXAIStreamToolCallName 钉住流式工具调用名进帧（帧承载约定：FrameToolCallArgsDelta.Text
// 载函数名、ToolCallID 载调用 id）：output_item.added 的 item.name 必须以补名帧
// （ToolCallID=call_abc、Text=search）进入帧序列，参数帧照常到（ToolCallID 一致、Arguments 完整）。
// 错误实现会红：不发补名帧 ⇒ 帧序列里只有 Text 为空的参数帧 ⇒ named=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXAIStreamToolCallName -v
func TestXAIStreamToolCallName(t *testing.T) {
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

	a := NewXAI(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newXAIResolution(srv.URL))
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

// TestXAIParseUsageRealSample 钉住真实样本（xAI 官方文档 Cost Tracking usage 示例，含自报费用
// cost_in_usd_ticks=158500，来源见 testdata/real/README.md）上 ParseUsage 归一成立：
// Responses 树 input_tokens=199 是总输入、无缓存字段 ⇒ 读写 0、Uncached=199、Out=1、Total=200；
// cost_in_usd_ticks 不入 Usage（5.6 ①）⇒ 随 Raw 保留。
// 错误实现会红：把 cost_in_usd_ticks 当 token 计入 ⇒ 数值对不上 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXAIParseUsageRealSample -v
func TestXAIParseUsageRealSample(t *testing.T) {
	a := NewXAI(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/grok_responses_usage.json"))
	if u.InputTokens != 199 || u.CacheReadTokens != 0 || u.InputTokensUncached != 199 || u.OutputTokens != 1 || u.TotalTokens != 200 {
		t.Fatalf("归一 = In%d/Read%d/Unc%d/Out%d/Total%d，期望 199/0/199/1/200（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.InputTokensUncached, u.OutputTokens, u.TotalTokens)
	}
}
