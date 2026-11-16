// 日期: 2026-11-16 16:02:19.262
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件覆盖小米 MiMo 适配器（统一 SDD 4.3.10；出站 Responses）：
// AuthScheme 唯一非标准头名 api-key（⛔ 非 Authorization）、developer 角色保留、
// usage 自带于收口帧（⛔ 不发 stream_options——Responses 形无此开关）、
// finish_reason 含 repetition_truncation（MiMo 独有）、previous_response_id 不支持 ⇒ 显式报错、
// tool_choice 仅 auto ⇒ 降级记 degradation（1.4.2 ⛔ 不静默）、真实样本直喂
// （testdata/real/mimo_responses_*_real.*）与真实流样本直喂解码器（mimo_responses_stream_real.sse）。

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
// ② previous_response_id 不支持（官方明列「不兼容参数:暂不支持 background、previous_response_id、
// context_management」）⇒ 显式 400 unsupported_capability（⛔ 不静默丢弃）。
// 错误实现会红：头名归并成 Authorization ⇒ 断言红；静默丢 previous_response_id ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiAuthSchemeAndPreviousResponseID -v
func TestXiaomiAuthSchemeAndPreviousResponseID(t *testing.T) {
	a := NewXiaomi(http.DefaultClient)
	sch := a.AuthScheme()
	if sch.HeaderName != "api-key" || sch.Prefix != "" {
		t.Fatalf("AuthScheme = %+v，期望 HeaderName=api-key（⛔ 非 Authorization）", sch)
	}
	if a.SupportsPreviousResponseID() {
		t.Fatalf("MiMo 不支持 previous_response_id（官方：不兼容参数明列）")
	}
	_, err := a.BuildRequest(context.Background(),
		&core.Request{PreviousResponseID: "resp_prev"}, newXiaomiResolution("https://api.xiaomimimo.com/v1"))
	requireDetailErr(t, err, "unsupported_capability")
}

// TestXiaomiBuildRequestFields 钉住 MiMo 请求字段（4.3.10 逐字；Responses 形）：
// ① developer 是 MiMo 多出的角色 ⇒ ⛔ 不许丢，input item 角色原样保留；
// ② ⛔ 不发 stream_options（usage 自带于收口帧，Responses 形无此开关）；
// ③ POST {base}/responses + max_output_tokens（Responses 形字段）+ stream:true。
// 错误实现会红：developer 被归并成 user ⇒ 角色断言红；错发 /chat/completions ⇒ URL 断言红。
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
	if out.URL != "https://api.xiaomimimo.com/v1/responses" {
		t.Fatalf("URL = %s，期望 {base}/responses（Responses 形）", out.URL)
	}
	m := bodyMap(t, out)
	items := mustSlice(t, m, "input")
	if len(items) != 2 {
		t.Fatalf("input item 数 = %d，期望 2", len(items))
	}
	first := items[0].(map[string]any)
	if first["type"] != "message" || first["role"] != "developer" {
		t.Fatalf("input[0] = %v，期望 developer item（⛔ MiMo 多出的角色不许丢）", first)
	}
	dp := first["content"].([]any)[0].(map[string]any)
	if dp["type"] != "input_text" || dp["text"] != "开发指令" {
		t.Fatalf("input[0].content[0] = %v，期望 input_text/开发指令", dp)
	}
	if mustNumber(t, m, "max_output_tokens") != 256 {
		t.Fatalf("max_output_tokens = %v，期望 256（Responses 形字段）", m["max_output_tokens"])
	}
	if m["stream"] != true {
		t.Fatalf("stream = %v，期望 true（StreamStart/流式请求注入）", m["stream"])
	}
	// ② ⛔ 无 stream_options（usage 自带于收口帧，无需开关，4.3.10）。
	assertNoKey(t, m, "stream_options")
}

// TestXiaomiToolChoiceDegradationRecorded 钉住 1.4.2 不静默降级（4.3.10 同口径）。
// 错误实现对照：只降不记 ⇒ 本测试必红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiToolChoiceDegradationRecorded -v
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
	m := bodyMap(t, out)
	if m["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v，期望降为 auto（Responses 形取值）", m["tool_choice"])
	}
}

// TestXiaomiParseResponseFinishSuperset 钉住 MiMo 独有 finish_reason（4.5.1 / 4.5.2）：
// repetition_truncation ⇒ 显式 finish_reason 回落字段超集原值（共享 deriveResponsesFinish 的
// explicit 优先分支，⛔ 不得当作未知值丢弃）。
// 错误实现会红：被归并成 unknown ⇒ 原值断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiParseResponseFinishSuperset -v
func TestXiaomiParseResponseFinishSuperset(t *testing.T) {
	a := NewXiaomi(http.DefaultClient)
	body := `{"id":"r","object":"response","status":"completed","finish_reason":"repetition_truncation","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"重复检测截断"}]}]}`
	out, err := a.ParseResponse(newXiaomiResolution(""), []byte(body))
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

// TestXiaomiParseResponseRealSample 钉住真实样本直喂（2026-11-16 上游直采，
// 来源见 testdata/real/README.md）：
// basic：output_text 便捷字段 + reasoning_text ⇒ 推理正文位 + usage 归一；
// tools：function_call item（call_id/name/arguments 逐字）+ finish 推导 tool_calls、
// output_text 空且无 message ⇒ Text 空。
// 错误实现会红：output_text 不取 ⇒ Text 断言红；function_call 解析错位 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiParseResponseRealSample -v
func TestXiaomiParseResponseRealSample(t *testing.T) {
	a := NewXiaomi(http.DefaultClient)
	out, err := a.ParseResponse(newXiaomiResolution(""), loadFixture(t, "real/mimo_responses_basic_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	const wantText = "你好！😊 请问你想说什么呢？消息似乎不太完整，欢迎继续补充，我很乐意帮忙！"
	if out.Text != wantText {
		t.Fatalf("Text = %q，期望 %q（output_text 便捷字段）", out.Text, wantText)
	}
	if len(out.OutputItems) != 2 || out.OutputItems[0].Type != "reasoning" || out.OutputItems[1].Type != "message" {
		t.Fatalf("OutputItems 顺序/类型不对: %+v", out.OutputItems)
	}
	if !strings.HasPrefix(out.OutputItems[0].ReasoningText, "The user said") {
		t.Fatalf("reasoning_text 未进推理正文位: %.30q", out.OutputItems[0].ReasoningText)
	}
	if out.FinishReason != core.FinishStop || out.Status != core.StatusCompleted {
		t.Fatalf("FinishReason=%q Status=%q，期望 stop/completed", out.FinishReason, out.Status)
	}
	// usage 样本逐字：in 9 / out 70 / reasoning 46 / total 79（input_tokens_details 为空 ⇒ 命中 0）。
	if out.Usage.InputTokens != 9 || out.Usage.CacheReadTokens != 0 || out.Usage.OutputTokens != 70 ||
		out.Usage.ReasoningTokens != 46 || out.Usage.TotalTokens != 79 {
		t.Fatalf("usage 归一 = In%d/Read%d/Out%d/Reas%d/Total%d，期望 9/0/70/46/79",
			out.Usage.InputTokens, out.Usage.CacheReadTokens, out.Usage.OutputTokens, out.Usage.ReasoningTokens, out.Usage.TotalTokens)
	}
	out, err = a.ParseResponse(newXiaomiResolution(""), loadFixture(t, "real/mimo_responses_tools_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse(tools) 报错: %v", err)
	}
	if out.FinishReason != core.FinishToolCalls {
		t.Fatalf("FinishReason = %q，期望 tool_calls（hasToolCall 推导）", out.FinishReason)
	}
	if out.Text != "" {
		t.Fatalf("Text = %q，期望空（output_text 空且无 message item）", out.Text)
	}
	fc := out.OutputItems[1]
	if fc.Type != "function_call" || fc.CallID != "call_a7d1697c86d5405abfa57513" ||
		fc.Name != "get_weather" || fc.Arguments != `{"city": "北京"}` {
		t.Fatalf("function_call item 不对: %+v（call_id/name/arguments 逐字）", fc)
	}
	if out.Usage.InputTokens != 90 || out.Usage.OutputTokens != 65 || out.Usage.TotalTokens != 155 {
		t.Fatalf("usage 归一 = In%d/Out%d/Total%d，期望 90/65/155",
			out.Usage.InputTokens, out.Usage.OutputTokens, out.Usage.TotalTokens)
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

// TestXiaomiStreamRealSample 钉住真实流样本直喂解码器（2026-11-16 上游直采）：
// testdata/real/mimo_responses_stream_real.sse ⇒ 帧序列与样本逐字一致：
// lifecycle×2 + 推理增量×11（response.reasoning_text.delta）+ 文本增量×15（逐字清单）
// + usage（9/0/67/76）+ done(completed)——usage 自带于 response.completed（⛔ 无需开关）。
// 错误实现会红：文本增量串不对 ⇒ 逐帧断言红；usage 帧缺失 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiStreamRealSample -v
func TestXiaomiStreamRealSample(t *testing.T) {
	a := NewXiaomi(http.DefaultClient)
	frames := feedSSEFile(t, a.(*xiaomiAdapter).streamDecoder().decode, "real/mimo_responses_stream_real.sse")
	const nReason, nText = 11, 15
	if len(frames) != 2+nReason+nText+2 {
		t.Fatalf("帧数 = %d，期望 %d（lifecycle×2 + 推理×%d + 文本×%d + usage + done）：%+v",
			len(frames), 2+nReason+nText+2, nReason, nText, frames)
	}
	if frames[0].Kind != stream.FrameLifecycle || frames[0].Status != "response.created" ||
		frames[1].Kind != stream.FrameLifecycle || frames[1].Status != "response.in_progress" {
		t.Fatalf("帧[0..1] = %+v / %+v，期望 lifecycle created / in_progress", frames[0], frames[1])
	}
	for i := 0; i < nReason; i++ {
		if frames[2+i].Kind != stream.FrameReasoningDelta {
			t.Fatalf("帧[%d] = %+v，期望推理增量", 2+i, frames[2+i])
		}
	}
	if frames[2].Reasoning != "The" || frames[2+nReason-1].Reasoning != " me respond appropriately." {
		t.Fatalf("推理增量首尾 = %.3q / %.3q，期望 The / ' me respond appropriately.'", frames[2].Reasoning, frames[2+nReason-1].Reasoning)
	}
	// 文本增量逐字清单。
	wantText := []string{"你好", "！看起来你的消息", "可能没有", "发", "完整", " 😊", "\n\n", "\"",
		"说好\"——", "你想说什么", "呢？欢迎", "继续补充", "，我很", "乐意帮助", "你！"}
	for i, want := range wantText {
		f := frames[2+nReason+i]
		if f.Kind != stream.FrameTextDelta || f.Text != want {
			t.Fatalf("帧[%d] = %+v，期望文本增量 %q（样本逐字）", 2+nReason+i, f, want)
		}
	}
	// usage（response.completed 自带）：样本逐字 9/0/67/76。
	u := frames[2+nReason+nText]
	if u.Kind != stream.FrameUsage || u.Usage.InputTokens != 9 || u.Usage.CacheReadTokens != 0 ||
		u.Usage.OutputTokens != 67 || u.Usage.ReasoningTokens != 35 || u.Usage.TotalTokens != 76 {
		t.Fatalf("usage 帧 = %+v，期望 In9/Read0/Out67/Reas35/Total76（样本逐字）", u)
	}
	d := frames[len(frames)-1]
	if d.Kind != stream.FrameDone || d.Status != core.StatusCompleted {
		t.Fatalf("末帧 = %+v，期望 done completed（usage 帧先于 done 帧）", d)
	}
}

// TestXiaomiStreamToolCallName 钉住 Responses 流的工具帧与推理增量（帧承载约定：
// FrameToolCallArgsDelta.Text 载函数名、ToolCallID 载调用 id、Arguments 载 partial_json 增量）：
// output_item.added 带名进补名帧、参数增量到、response.reasoning_text.delta（MiMo 事件名逐字）
// 进推理帧；[DONE] 容忍（completed 后的哨兵帧不重复收口、不报错）。
// 错误实现会红：不发补名帧 ⇒ named=0 ⇒ 红；[DONE] 当错误帧 ⇒ 终态错误红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiStreamToolCallName -v
func TestXiaomiStreamToolCallName(t *testing.T) {
	const sse = "" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"item_id\":\"fc_1\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_abc\",\"name\":\"search\"}}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":1,\"item_id\":\"fc_1\",\"partial_json\":\"{\\\"q\\\":1}\"}\n\n" +
		"event: response.reasoning_text.delta\n" +
		"data: {\"type\":\"response.reasoning_text.delta\",\"sequence_number\":2,\"delta\":\"想一想\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":3}\n\n" +
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
		t.Fatalf("流式终态错误: %v（[DONE] 必须容忍）", cerr)
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
		t.Fatalf("参数帧缺失（partial_json={\"q\":1}）：%+v", frames)
	}
	if reasoning == 0 {
		t.Fatalf("response.reasoning_text.delta 未进推理帧：%+v", frames)
	}
	if doneStatus != core.StatusCompleted {
		t.Fatalf("done 状态 = %q，期望 completed（[DONE] 容忍不重复收口）", doneStatus)
	}
}

// TestXiaomiParseUsageRealSample 钉住真实样本（小米 MiMo 官方文档响应示例：语音识别 id 9f51eba4…、
// 音频理解 id 020d7a2c-…，来源见 testdata/real/README.md）上 ParseUsage 归一成立：
// ASR 样本命中 45 ⇒ Uncached = 46 − 45 = 1（真实字段 audio_tokens/seconds ⛔ 不干扰解析）；
// 音频样本无命中 ⇒ Uncached=86，且 reasoning_tokens=66 入 Reasoning。
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
