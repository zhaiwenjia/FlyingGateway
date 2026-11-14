// 日期: 2026-11-14 14:58:32.003
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

// 本文件覆盖小米 MiMo 适配器（统一 SDD 4.3.10）：
// AuthScheme 唯一非标准头名 api-key（⛔ 非 Authorization）、developer 角色保留、
// ⛔ 不发 stream_options（末帧自动带 usage，无需开关）、
// finish_reason 含 repetition_truncation（MiMo 独有）、previous_response_id 不支持 ⇒ 显式报错。

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

// TestXiaomiBuildRequestFields 钉住 MiMo 请求字段（4.3.10 逐字）：
// ① messages[].role 比 OpenAI 多一个 developer ⇒ ⛔ 不许丢，input[] item 角色原样保留；
// ② ⛔ 不发 stream_options（末帧自动带 usage，无需开关）。
// 错误实现会红：developer 被归并成 user ⇒ 角色断言红；照搬 stream_options ⇒ 无键断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiBuildRequestFields -v
func TestXiaomiBuildRequestFields(t *testing.T) {
	a := NewXiaomi(http.DefaultClient)
	r := &core.Request{
		Messages: []core.Message{
			{Role: "developer", Content: []core.ContentPart{{Text: "开发指令"}}},
			{Role: "user", Content: []core.ContentPart{{Text: "你好"}}},
		},
		Stream: true,
	}
	out, err := a.BuildRequest(context.Background(), r, newXiaomiResolution("https://api.xiaomimimo.com/v1"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "https://api.xiaomimimo.com/v1/responses" {
		t.Fatalf("URL = %s，期望 {base}/responses", out.URL)
	}
	m := bodyMap(t, out)
	items := mustSlice(t, m, "input")
	if len(items) != 2 {
		t.Fatalf("input item 数 = %d，期望 2", len(items))
	}
	first := items[0].(map[string]any)
	if first["role"] != "developer" {
		t.Fatalf("input[0].role = %v，期望 developer（⛔ MiMo 多出的角色不许丢）", first["role"])
	}
	// ② ⛔ 无 stream_options（4.3.10「无需开关」）。
	assertNoKey(t, m, "stream_options")
}

// TestXiaomiParseResponseFinishSuperset 钉住 MiMo 独有 finish_reason（4.5.1 / 4.5.2）：
// repetition_truncation ⇒ 超集原值（⛔ 不得当作未知值丢弃）。
// 错误实现会红：被归并成 unknown ⇒ 断言红。
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

// TestXiaomiStreamFinalUsage 钉住末帧自动带 usage（4.3.10：无需 stream_options 开关）。
// 错误实现会红：等 include_usage 开关才收 usage ⇒ 帧缺失 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiStreamFinalUsage -v
func TestXiaomiStreamFinalUsage(t *testing.T) {
	const sse = "" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":0,\"delta\":\"你好\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"usage\":{\"prompt_tokens\":80,\"completion_tokens\":20,\"total_tokens\":100,\"prompt_tokens_details\":{\"cached_tokens\":15}}}}\n\n"
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
		t.Fatalf("帧数 = %d，期望 3：%+v", len(frames), frames)
	}
	if frames[1].Kind != stream.FrameUsage || frames[1].Usage.InputTokens != 80 || frames[1].Usage.CacheReadTokens != 15 {
		t.Fatalf("帧[1] = %+v，期望末帧 usage（80/15）", frames[1])
	}
	if frames[2].Kind != stream.FrameDone {
		t.Fatalf("帧[2] = %+v，期望 done", frames[2])
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

// TestXiaomiStreamToolCallName 钉住流式工具调用名进帧（帧承载约定：FrameToolCallArgsDelta.Text
// 载函数名、ToolCallID 载调用 id）：output_item.added 的 item.name 必须以补名帧
// （ToolCallID=call_abc、Text=search）进入帧序列，参数帧照常到（ToolCallID 一致、Arguments 完整）。
// 错误实现会红：不发补名帧 ⇒ 帧序列里只有 Text 为空的参数帧 ⇒ named=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestXiaomiStreamToolCallName -v
func TestXiaomiStreamToolCallName(t *testing.T) {
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

	a := NewXiaomi(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newXiaomiResolution(srv.URL))
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
