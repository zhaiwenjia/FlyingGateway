// 日期: 2026-11-14 16:38:51.694
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件覆盖小米 MiMo 适配器（统一 SDD 4.3.10；出站 Chat Completions）：
// AuthScheme 唯一非标准头名 api-key（⛔ 非 Authorization）、developer 角色保留、
// ⛔ 不发 stream_options（末帧自动带 usage，无需开关）、
// finish_reason 含 repetition_truncation（MiMo 独有）、previous_response_id 不支持 ⇒ 显式报错。
// MiMo 无真实流样本 ⇒ 流式用构造 fixture（直喂口径同真实样本）。

func newXiaomiResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderXiaomi,
		Shape:    core.APIShapeOpenAIResponses,
		BaseURL:  base,
		Model:    "mimo-v2.6-pro",
	}
}

// TestXiaomiAuthSchemeAndPreviousResponseID 钉住鉴权头与会话语义（4.3.10 / 8.4 / 3.5）：
// ① HeaderName = "api-key"（⛔ 非 Authorization——MiMo 唯一非标准头名）；
// ② previous_response_id 不支持 ⇒ 显式 400 unsupported_capability（⛔ 不静默丢弃）。
// 错误实现会红：头名归并成 Authorization ⇒ 断言红；静默丢 previous_response_id ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiAuthSchemeAndPreviousResponseID -v
func TestXiaomiAuthSchemeAndPreviousResponseID(t *testing.T) {
	a := NewXiaomi(http.DefaultClient)
	sch := a.AuthScheme()
	if sch.HeaderName != "api-key" || sch.Prefix != "" {
		t.Fatalf("AuthScheme = %+v，期望 HeaderName=api-key（⛔ 非 Authorization）", sch)
	}
	if a.SupportsPreviousResponseID() {
		t.Fatalf("MiMo 不支持 previous_response_id（3.5 表未列入）")
	}
	_, err := a.BuildRequest(context.Background(),
		&core.Request{PreviousResponseID: "resp_prev"}, newXiaomiResolution("https://api.xiaomimimo.com/v1"))
	requireDetailErr(t, err, "unsupported_capability")
}

// TestXiaomiBuildRequestFields 钉住 MiMo 请求字段（4.3.10 逐字；chat 形 messages[]）：
// ① messages[].role 比 OpenAI 多一个 developer ⇒ ⛔ 不许丢，消息角色原样保留；
// ② ⛔ 不发 stream_options（末帧自动带 usage，无需开关）；
// ③ POST {base}/chat/completions + max_tokens（chat 形普通字段）。
// 错误实现会红：developer 被归并成 user ⇒ 角色断言红；照搬 stream_options ⇒ 无键断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiBuildRequestFields -v
func TestXiaomiBuildRequestFields(t *testing.T) {
	a := NewXiaomi(http.DefaultClient)
	r := &core.Request{
		Messages: []core.Message{
			{Role: "developer", Content: []core.ContentPart{{Text: "开发指令"}}},
			{Role: "user", Content: []core.ContentPart{{Text: "你好"}}},
		},
		MaxOutputTokens: 256,
		Stream:          true,
	}
	out, err := a.BuildRequest(context.Background(), r, newXiaomiResolution("https://api.xiaomimimo.com/v1"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "https://api.xiaomimimo.com/v1/chat/completions" {
		t.Fatalf("URL = %s，期望 {base}/chat/completions", out.URL)
	}
	m := bodyMap(t, out)
	msgs := mustSlice(t, m, "messages")
	if len(msgs) != 2 {
		t.Fatalf("messages 数 = %d，期望 2", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "developer" {
		t.Fatalf("messages[0].role = %v，期望 developer（⛔ MiMo 多出的角色不许丢）", first["role"])
	}
	if mustString(t, first, "content") != "开发指令" {
		t.Fatalf("messages[0].content = %v", first)
	}
	if mustNumber(t, m, "max_tokens") != 256 {
		t.Fatalf("max_tokens = %v，期望 256（chat 形普通字段）", m["max_tokens"])
	}
	// ② ⛔ 无 stream_options（4.3.10「无需开关」）。
	assertNoKey(t, m, "stream_options")
}

// TestXiaomiParseResponseFinishSuperset 钉住 MiMo 独有 finish_reason（4.5.1 / 4.5.2）：
// repetition_truncation ⇒ 超集原值（⛔ 不得当作未知值丢弃）。
// 错误实现会红：被归并成 unknown ⇒ 原值断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiParseResponseFinishSuperset -v
func TestXiaomiParseResponseFinishSuperset(t *testing.T) {
	a := NewXiaomi(http.DefaultClient)
	out, err := a.ParseResponse(newXiaomiResolution(""), loadFixture(t, "mimo_response.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if out.FinishReason != core.FinishRepetitionTruncation {
		t.Fatalf("FinishReason = %q，期望 repetition_truncation（MiMo 独有，4.5.1）", out.FinishReason)
	}
	if out.Text != "重复检测截断" {
		t.Fatalf("Text = %q", out.Text)
	}
}

// TestXiaomiParseUsage 钉住 MiMo usage 归一（4.4.2 小米 MiMo 行）：
// 命中 = prompt_tokens_details.cached_tokens = 15、无写入 ⇒ 0。
// 错误实现会红：命中取顶层 cached_tokens（不存在）⇒ Read=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiParseUsage -v
func TestXiaomiParseUsage(t *testing.T) {
	a := NewXiaomi(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "mimo_usage.json"))
	if u.InputTokens != 80 || u.CacheReadTokens != 15 || u.CacheWriteTokens != 0 || u.OutputTokens != 20 {
		t.Fatalf("归一不对：In=%d(80) Read=%d(15) Write=%d(0) Out=%d(20)",
			u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens)
	}
	if u.InputTokensUncached != 65 {
		t.Fatalf("Uncached = %d，期望 65 = 80−15", u.InputTokensUncached)
	}
}

// TestXiaomiStreamFinalUsage 钉住末帧自动带 usage（4.3.10：无需 stream_options 开关）：
// 构造 chat chunk fixture（MiMo 无真实流样本）经 StreamStart 直喂 ⇒ 文本增量 + 末帧 usage（80/15）
// + done（finish_reason 超集原值）。
// 错误实现会红：等 include_usage 开关才收 usage ⇒ 帧缺失 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiStreamFinalUsage -v
func TestXiaomiStreamFinalUsage(t *testing.T) {
	const sse = "" +
		"data: {\"id\":\"cmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}],\"usage\":null}\n\n" +
		"data: {\"id\":\"cmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"},\"finish_reason\":null}],\"usage\":null}\n\n" +
		"data: {\"id\":\"cmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":80,\"completion_tokens\":20,\"total_tokens\":100,\"prompt_tokens_details\":{\"cached_tokens\":15}}}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewXiaomi(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newXiaomiResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	if len(frames) != 3 {
		t.Fatalf("帧数 = %d，期望 3（文本 + usage + done）：%+v", len(frames), frames)
	}
	if frames[0].Kind != stream.FrameTextDelta || frames[0].Text != "你好" {
		t.Fatalf("帧[0] = %+v，期望文本增量 你好", frames[0])
	}
	if frames[1].Kind != stream.FrameUsage || frames[1].Usage.InputTokens != 80 || frames[1].Usage.CacheReadTokens != 15 {
		t.Fatalf("帧[1] = %+v，期望末帧 usage（80/15）", frames[1])
	}
	if frames[2].Kind != stream.FrameDone || frames[2].Status != "stop" {
		t.Fatalf("帧[2] = %+v，期望 done stop（finish_reason 超集原值）", frames[2])
	}
}

// TestXiaomiToolChoiceDegradationRecorded 钉住 1.4.2 不静默降级（4.3.10 同口径）。
// 错误实现对照：只降不记 ⇒ 本测试必红。
func TestXiaomiToolChoiceDegradationRecorded(t *testing.T) {
	a := NewXiaomi(&http.Client{})
	r := &core.Request{
		Model:      "mimo-v2.6-pro",
		Messages:   []core.Message{{Role: "user", Content: []core.ContentPart{{Type: "text", Text: "hi"}}}},
		ToolChoice: &core.ToolChoice{Mode: "function", Name: "search"},
	}
	out, degs, err := a.(DegradationAware).BuildRequestWithDegradations(context.Background(), r, &core.Resolution{Model: "mimo-v2.6-pro"})
	if err != nil {
		t.Fatalf("BuildRequestWithDegradations 失败: %v", err)
	}
	found := false
	for _, d := range degs {
		if d.Field == "tool_choice" && d.Reason == core.DegradationCapabilityUnsupported {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool_choice 降级未记 degradation（degs=%v），违反 1.4.2", degs)
	}
	var body map[string]any
	if err := json.Unmarshal(out.Body, &body); err != nil {
		t.Fatalf("请求体解析失败: %v", err)
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v，期望降为 auto", body["tool_choice"])
	}
}

// TestXiaomiStreamToolCallName 钉住 chat chunk 的工具帧与推理增量（帧承载约定：
// FrameToolCallArgsDelta.Text 载函数名、ToolCallID 载调用 id）：首块带名进补名帧、
// 后续块按 index 对齐同一调用 id 传参数增量、delta.reasoning_content（扩展名逐字）进推理帧。
// 错误实现会红：不发补名帧 ⇒ named=0 ⇒ 红；index 不对齐 ⇒ 参数帧 ToolCallID 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiStreamToolCallName -v
func TestXiaomiStreamToolCallName(t *testing.T) {
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

	a := NewXiaomi(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newXiaomiResolution(srv.URL))
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

// TestXiaomiParseUsageRealSample 钉住真实样本（小米 MiMo 官方文档响应示例：语音识别 id 9f51eba4…、
// 音频理解 id 020d7a2c-…，来源见 testdata/real/README.md）上 ParseUsage 归一成立：
// ASR 样本命中 45 ⇒ Uncached = 46 − 45 = 1（真实字段 audio_tokens/seconds ⛔ 不干扰解析）；
// 音频样本无命中 ⇒ Uncached=86，且 reasoning_tokens=66 入 Reasoning
// （⚠️ 2026-11-14 实测修正：SDD 4.3.10 字段表本就列了该字段、实现原先漏读）。
// 错误实现会红：Uncached 取 prompt_tokens ⇒ 46 ≠ 1 红；漏读 reasoning ⇒ 66 ≠ 0 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiParseUsageRealSample -v
func TestXiaomiParseUsageRealSample(t *testing.T) {
	a := NewXiaomi(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/mimo_usage_asr.json"))
	if u.InputTokens != 46 || u.CacheReadTokens != 45 || u.InputTokensUncached != 1 || u.OutputTokens != 20 || u.TotalTokens != 66 {
		t.Fatalf("ASR 归一 = In%d/Read%d/Unc%d/Out%d/Total%d，期望 46/45/1/20/66（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.InputTokensUncached, u.OutputTokens, u.TotalTokens)
	}
	u = a.ParseUsage(loadFixture(t, "real/mimo_usage_audio.json"))
	if u.InputTokens != 86 || u.CacheReadTokens != 0 || u.InputTokensUncached != 86 || u.OutputTokens != 98 || u.TotalTokens != 184 || u.ReasoningTokens != 66 {
		t.Fatalf("音频归一 = In%d/Read%d/Unc%d/Out%d/Total%d/Reas%d，期望 86/0/86/98/184/66（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.InputTokensUncached, u.OutputTokens, u.TotalTokens, u.ReasoningTokens)
	}
}

// TestXiaomiStreamRealCaptureUsageSplit 钉住真实流的 usage 两拍形态（真捕获
// testdata/real/mimo_stream_real.sse）：finish 帧 usage:null ⇒ 独立 usage 帧 ⇒ [DONE]。
// ⚠️ 2026-11-14 修复的 bug：finish 即收口会丢独立 usage 帧（真实样本实测根因）。
// 错误实现对照：提前收口 ⇒ 无 Usage 帧或 Done 在 Usage 前 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiStreamRealCapture -v
func TestXiaomiStreamRealCaptureUsageSplit(t *testing.T) {
	f, err := os.Open("testdata/real/mimo_stream_real.sse")
	if err != nil {
		t.Fatalf("打开真实样本失败: %v", err)
	}
	defer f.Close()
	a := NewXiaomi(http.DefaultClient)
	d := a.(*xiaomiAdapter).streamDecoder()
	sc := stream.NewSSEScanner(f)
	var frames []stream.Frame
	for sc.Scan() {
		fs, done, err := d.decode(sc.Event())
		if err != nil {
			t.Fatalf("decode 出错: %v", err)
		}
		frames = append(frames, fs...)
		if done {
			break
		}
	}
	ui, di := -1, -1
	for i, fr := range frames {
		if fr.Kind == stream.FrameUsage {
			ui = i
			if fr.Usage == nil || fr.Usage.InputTokens <= 0 || fr.Usage.OutputTokens <= 0 {
				t.Fatalf("usage 帧数值非法: %+v", fr.Usage)
			}
		}
		if fr.Kind == stream.FrameDone {
			di = i
		}
	}
	if ui < 0 {
		t.Fatal("真实样本未产出 usage 帧（独立 usage 帧被丢）")
	}
	if di < 0 || ui > di {
		t.Fatalf("usage 帧(%d) 必须先于 Done 帧(%d)", ui, di)
	}
}

// TestXiaomiDeferredDoneAtSentinel 钉住无 usage 流的 [DONE] 补收口（终止帧不丢）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiDeferredDoneAtSentinel -v
func TestXiaomiDeferredDoneAtSentinel(t *testing.T) {
	a := NewXiaomi(http.DefaultClient)
	d := a.(*xiaomiAdapter).streamDecoder()
	fs, done, _ := d.decode(stream.SSEEvent{Data: `{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`})
	if done || len(fs) != 0 {
		t.Fatalf("finish 帧不应终态（usage 可能在其后），实际 done=%v frames=%d", done, len(fs))
	}
	fs, done, _ = d.decode(stream.SSEEvent{Data: `[DONE]`})
	if !done || len(fs) != 1 || fs[0].Kind != stream.FrameDone || fs[0].Status != "stop" {
		t.Fatalf("[DONE] 补收口 = %+v done=%v，期望恰一帧 Done(stop)", fs, done)
	}
}
