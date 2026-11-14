// 日期: 2026-11-14 16:38:51.664
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

// 本文件覆盖 Kimi / Moonshot 适配器（统一 SDD 4.3.9；出站 Chat Completions）：
// BuildRequest（max_completion_tokens ⛔ 非 max_tokens、prompt_cache_options 进 body、
// ⛔ 无 prompt_cache_breakpoint——显式断点被拒 400）、ParseUsage（Kimi 三分：
// 未命中 + 命中 + 写入互斥，⛔ 不累加）、真实流样本直喂（testdata/real/kimi_stream_docs_example.txt）。

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
// ④ Extra 黑名单连带剔除 temperature 等由 core 承载 / 禁发键；
// ⑤ previous_response_id 无 wire 载体（chat 形）⇒ 显式 400 unsupported_capability（⛔ 不静默丢弃）。
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
	res := newMoonshotResolution("https://api.moonshot.cn/v1")
	out, err := a.BuildRequest(context.Background(), r, res)
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
	// messages[]（chat 形）原样映射。
	msgs := mustSlice(t, m, "messages")
	if len(msgs) != 1 || mustString(t, msgs[0].(map[string]any), "role") != "user" {
		t.Fatalf("messages = %v，期望单条 user", msgs)
	}
	// ⑤ previous_response_id 无 wire 载体 ⇒ 显式报错（⛔ 不静默丢弃，3.5）。
	if a.SupportsPreviousResponseID() {
		t.Fatalf("Moonshot chat 形不支持 previous_response_id（wire 无此字段）")
	}
	_, err = a.BuildRequest(context.Background(), &core.Request{PreviousResponseID: "resp_prev"}, res)
	requireDetailErr(t, err, "unsupported_capability")
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

// TestMoonshotStreamDocsExample 钉住真实流样本直喂（2026-11-14 改形验收点）：
// testdata/real/kimi_stream_docs_example.txt（官方文档「Streaming Response」示例块逐字，
// 含 `...` 省略行、⛔ 不补造）经 StreamStart 直喂 ⇒ 帧序列与样本逐字一致：
// 文本增量 Hello + 末帧 usage（19/12/0/13）+ done（finish_reason=stop 超集原值）。
// 错误实现会红：`...` 省略行中断解析 ⇒ 帧缺失红；usage 归一错位 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMoonshotStreamDocsExample -v
func TestMoonshotStreamDocsExample(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(loadFixture(t, "real/kimi_stream_docs_example.txt"))
	}))
	defer srv.Close()

	a := NewMoonshot(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newMoonshotResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v（`...` 省略行必须容忍）", cerr)
	}
	if len(frames) != 3 {
		t.Fatalf("帧数 = %d，期望 3（文本 Hello + usage + done）：%+v", len(frames), frames)
	}
	if frames[0].Kind != stream.FrameTextDelta || frames[0].Text != "Hello" {
		t.Fatalf("帧[0] = %+v，期望文本增量 Hello（样本逐字）", frames[0])
	}
	u := frames[1]
	if u.Kind != stream.FrameUsage {
		t.Fatalf("帧[1] = %+v，期望 usage", u)
	}
	// 三分互斥：In=19 = 未命中 7 + 命中 12 + 写 0；顶层与二级 cached 同值取其一（⛔ 非 24）。
	if u.Usage.InputTokens != 19 || u.Usage.CacheReadTokens != 12 || u.Usage.CacheWriteTokens != 0 || u.Usage.OutputTokens != 13 {
		t.Fatalf("usage 归一 = In%d(19) Read%d(12) Write%d(0) Out%d(13)",
			u.Usage.InputTokens, u.Usage.CacheReadTokens, u.Usage.CacheWriteTokens, u.Usage.OutputTokens)
	}
	if frames[2].Kind != stream.FrameDone || frames[2].Status != "stop" {
		t.Fatalf("帧[2] = %+v，期望 done stop（finish_reason 超集原值）", frames[2])
	}
}

// TestMoonshotStreamToolCallName 钉住 chat chunk 的工具帧与推理增量（帧承载约定：
// FrameToolCallArgsDelta.Text 载函数名、ToolCallID 载调用 id）：首块带名进补名帧、
// 后续块按 index 对齐同一调用 id 传参数增量、delta.reasoning_content（扩展名逐字）进推理帧。
// 错误实现会红：不发补名帧 ⇒ named=0 ⇒ 红；index 不对齐 ⇒ 参数帧 ToolCallID 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMoonshotStreamToolCallName -v
func TestMoonshotStreamToolCallName(t *testing.T) {
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
		t.Fatalf("参数帧缺失（后续块按 index 对齐 call_abc 且 Arguments={\"q\":1}）：%+v", frames)
	}
	if reasoning == 0 {
		t.Fatalf("delta.reasoning_content 未进推理帧：%+v", frames)
	}
	if doneStatus != "tool_calls" {
		t.Fatalf("done 状态 = %q，期望 tool_calls（finish_reason 超集原值）", doneStatus)
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
