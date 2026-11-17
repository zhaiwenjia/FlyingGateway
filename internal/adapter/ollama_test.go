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

func newOllamaCompatResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderOllama,
		Shape:    core.APIShapeOpenAIChat,
		BaseURL:  base,
		Model:    "qwen3:8b",
	}
}
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
	// auto 不记降级（语义无损失：Ollama 默认即模型自决；口径同 zhipu/moonshot「auto 不记」）。
	rAuto := *r
	rAuto.ToolChoice = &core.ToolChoice{Mode: "auto"}
	outAuto, degsAuto, err := a.(*ollamaCompatAdapter).BuildRequestWithDegradations(context.Background(), &rAuto, newOllamaCompatResolution("http://127.0.0.1:11434"))
	if err != nil {
		t.Fatalf("BuildRequest(tool_choice=auto) 报错: %v", err)
	}
	assertNoKey(t, bodyMap(t, outAuto), "tool_choice")
	if len(degsAuto) != 0 {
		t.Fatalf("tool_choice=auto 的 degradations = %+v，期望 0 条（auto 无语义损失 ⛔ 不记）", degsAuto)
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

// TestOllamaPreviousResponseIDRejected 钉住 3.5：Ollama 不支持服务端会话 ⇒ 显式报错
// （/v1/responses 仅无状态 flavor，4.3.6）。
// 错误实现会红：静默丢弃 ⇒ err==nil ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOllamaPreviousResponseIDRejected -v
func TestOllamaPreviousResponseIDRejected(t *testing.T) {
	a := NewOllamaCompat(http.DefaultClient)
	_, err := a.BuildRequest(context.Background(),
		&core.Request{PreviousResponseID: "resp_prev"}, newOllamaCompatResolution("http://127.0.0.1:11434"))
	requireDetailErr(t, err, "unsupported_capability")
}
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
