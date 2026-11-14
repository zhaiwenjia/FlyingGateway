// 日期: 2026-11-14 16:08:29.819
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

// 本文件覆盖 Anthropic 适配器（统一 SDD 4.3.2「原生形状，不可压平」）：
// BuildRequest（⛔ temperature/top_p/top_k、max_tokens 必填缺省补值 + 降级记录、
// system 顶层、thinking 块 signature 原样回传、cache_control 断点）、
// ParseResponse（content[] 块数组、stop_reason 七值超集原值）、
// ParseUsage（残值语义：total = cache_read + cache_creation + input_tokens）、
// 流式（message_delta 累计 usage，⛔ 不逐帧相加）。

func newAnthropicResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderAnthropic,
		Shape:    core.APIShapeAnthropicMessages,
		BaseURL:  base,
		Model:    "claude-opus-4.7",
	}
}

// TestAnthropicBuildRequestContract 钉住 Anthropic 请求契约（4.3.2）：
// ① API version 2026-10-21 起 temperature / top_p / top_k 一律 400 ⇒ 构造必须剔除；
// ② max_tokens 必填：无值 ⇒ 补默认值 + BuildRequestWithDegradations 记「max_tokens 缺省已补」；
// ③ system 走顶层参数（⛔ 不是 messages[0]）；
// ④ thinking 块 signature 原样回传（4.2.5：修改即 400）。
// 错误实现会红：透传 temperature ⇒ 无字段断言红；max_tokens 不补 ⇒ ==0 断言红；不记降级 ⇒ degs 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestAnthropicBuildRequestContract -v
func TestAnthropicBuildRequestContract(t *testing.T) {
	a := NewAnthropic(http.DefaultClient)
	temp, topP := 0.7, 0.9
	r := &core.Request{
		Messages: []core.Message{
			{Role: "system", Content: []core.ContentPart{{Text: "系统提示"}}},
			{Role: "assistant", ReasoningContent: "推理过程", ThinkingSignature: "sig-1"},
			{Role: "user", Content: []core.ContentPart{{Text: "你好"}}},
		},
		Temperature:  &temp,
		TopP:         &topP,
		CacheControl: &core.CacheControl{Type: "ephemeral", TTL: "5m"},
		Reasoning:    &core.ReasoningConfig{Effort: "high"},
	}
	res := newAnthropicResolution("https://api.anthropic.com/v1")
	out, degs, err := a.(*anthropicAdapter).BuildRequestWithDegradations(context.Background(), r, res)
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("URL = %s，期望 POST {base}/messages（4.3.2）", out.URL)
	}
	m := bodyMap(t, out)
	// ① ⛔ 无 temperature / top_p / top_k（4.3.2 版本硬约束）。
	assertNoKey(t, m, "temperature")
	assertNoKey(t, m, "top_p")
	assertNoKey(t, m, "top_k")
	// ② max_tokens 必填 + 缺省补值降级。
	if mustNumber(t, m, "max_tokens") != float64(anthropicDefaultMaxTokens) {
		t.Fatalf("max_tokens = %v，期望缺省补值 %d", m["max_tokens"], anthropicDefaultMaxTokens)
	}
	if len(degs) != 1 || degs[0].Field != "max_tokens" || degs[0].Detail != "max_tokens 缺省已补" {
		t.Fatalf("degradations = %+v，期望 [{max_tokens … max_tokens 缺省已补}]", degs)
	}
	// ③ system 顶层参数（⛔ 不是 messages[0]）。
	if mustString(t, m, "system") != "系统提示" {
		t.Fatalf("system = %v，期望顶层 system=系统提示", m["system"])
	}
	msgs := mustSlice(t, m, "messages")
	if len(msgs) != 2 {
		t.Fatalf("messages 数 = %d，期望 2（system 不入 messages）：%v", len(msgs), msgs)
	}
	// ④ thinking 块 signature 原样回传（4.2.5）。
	blk0 := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if blk0["type"] != "thinking" || blk0["signature"] != "sig-1" || blk0["thinking"] != "推理过程" {
		t.Fatalf("thinking 块 = %v，期望 signature 原样保留", blk0)
	}
	// thinking 参数映射（4.3.2：{type:"adaptive", display:"summarized"}）。
	th := mustAt(t, m, "thinking").(map[string]any)
	if th["type"] != "adaptive" || th["display"] != "summarized" {
		t.Fatalf("thinking 参数 = %v", th)
	}
	// cache_control 断点落在最后一个消息块，且嵌套键名逐字是 type/ttl（4.3.2）。
	// ⚠️ 此处钉死键名：2026-11-14 实测 core.CacheControl 缺 json tag 时嵌套键输出 Go 字段名
	// （Type/TTL），上游认不了断点 ⇒ 缓存静默失效；已修（core/request.go 补 tag），
	// 错误实现（去掉 tag）在本断言必红。
	lastMsg := msgs[len(msgs)-1].(map[string]any)
	lastBlk := lastMsg["content"].([]any)[len(lastMsg["content"].([]any))-1].(map[string]any)
	cc, ok := lastBlk["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("最后一个消息块缺 cache_control 断点: %v", lastBlk)
	}
	if cc["type"] != "ephemeral" || cc["ttl"] != "5m" {
		t.Fatalf("cache_control 嵌套键名/取值 = %v，期望 {type:ephemeral, ttl:5m}（键名逐字小写）", cc)
	}
	// AuthScheme（4.3.2：x-api-key + anthropic-version: 2026-10-21）。
	sch := a.AuthScheme()
	if sch.HeaderName != "x-api-key" || sch.Prefix != "" {
		t.Fatalf("AuthScheme = %+v，期望 x-api-key（无前缀）", sch)
	}
	if sch.ExtraHeaders["anthropic-version"] != "2026-10-21" {
		t.Fatalf("anthropic-version = %q，期望 2026-10-21", sch.ExtraHeaders["anthropic-version"])
	}

	// 有值时不补：MaxOutputTokens=777 ⇒ max_tokens=777 且无降级。
	r.MaxOutputTokens = 777
	out, degs, err = a.(*anthropicAdapter).BuildRequestWithDegradations(context.Background(), r, res)
	if err != nil {
		t.Fatalf("BuildRequest(带 max_tokens) 报错: %v", err)
	}
	m = bodyMap(t, out)
	if mustNumber(t, m, "max_tokens") != 777 || len(degs) != 0 {
		t.Fatalf("max_tokens=%v degs=%+v，期望 777 / 无降级", m["max_tokens"], degs)
	}
}

// TestAnthropicPreviousResponseIDRejected 钉住 3.5：上游不支持服务端会话 ⇒ 显式 400
// unsupported_capability（⛔ 不静默丢弃）。
// 错误实现会红：静默丢弃 ⇒ err==nil ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestAnthropicPreviousResponseIDRejected -v
func TestAnthropicPreviousResponseIDRejected(t *testing.T) {
	a := NewAnthropic(http.DefaultClient)
	_, err := a.BuildRequest(context.Background(),
		&core.Request{PreviousResponseID: "resp_prev"}, newAnthropicResolution("https://api.anthropic.com/v1"))
	requireDetailErr(t, err, "unsupported_capability")
}

// TestAnthropicParseResponseBlocks 钉住 content[] 块数组解析（4.3.2）：
// text / thinking（signature ⇒ ThinkingSignature ⛔ 不可压平）/ tool_use；stop_reason 七值超集原值。
// 错误实现会红：signature 被丢 ⇒ 该项断言红；stop_reason 被映射成 OpenAI 五值 ⇒ 原值断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestAnthropicParseResponseBlocks -v
func TestAnthropicParseResponseBlocks(t *testing.T) {
	a := NewAnthropic(http.DefaultClient)
	out, err := a.ParseResponse(newAnthropicResolution(""), loadFixture(t, "anthropic_response.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if len(out.OutputItems) != 3 {
		t.Fatalf("OutputItems = %+v，期望 3 块（text/thinking/tool_use）", out.OutputItems)
	}
	if out.OutputItems[0].Type != "message" || out.OutputItems[0].Content[0].Text != "先想一下：" {
		t.Fatalf("text 块解析不对: %+v", out.OutputItems[0])
	}
	if out.OutputItems[1].Type != "thinking" || out.OutputItems[1].ThinkingSignature != "sig-abc-123" {
		t.Fatalf("thinking 块 signature 丢失: %+v（4.2.5 ⛔ 不可压平）", out.OutputItems[1])
	}
	if out.OutputItems[2].Type != "tool_use" || out.OutputItems[2].Name != "get_weather" {
		t.Fatalf("tool_use 块解析不对: %+v", out.OutputItems[2])
	}
	if out.FinishReason != "tool_use" {
		t.Fatalf("FinishReason = %q，期望超集原值 tool_use（4.5.2 Claude 七值体系）", out.FinishReason)
	}
	if out.Text != "先想一下：" {
		t.Fatalf("Text = %q", out.Text)
	}
}

// TestAnthropicParseUsageResidual 钉住残值语义（4.4.3 合成公式 + 11.1 逐字行）：
// input_tokens 是「新增部分」⛔ 不是总数 ⇒ total = 50 + 0 + 200000 = 200050（⛔ 不是 50）。
// 错误实现会红：直接取 input_tokens ⇒ InputTokens=50 ⇒ 红（SDD 11.5 示例同款）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestAnthropicParseUsageResidual -v
func TestAnthropicParseUsageResidual(t *testing.T) {
	a := NewAnthropic(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "anthropic_usage_residual.json"))
	if u.InputTokens != 200050 {
		t.Fatalf("InputTokens = %d，期望 200050 = 50+0+200000（⛔ 非 50）", u.InputTokens)
	}
	if u.CacheReadTokens != 200000 || u.CacheWriteTokens != 0 || u.OutputTokens != 30 {
		t.Fatalf("归一不对：Read=%d(200000) Write=%d(0) Out=%d(30)",
			u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens)
	}
	if u.InputTokensUncached != 50 {
		t.Fatalf("Uncached = %d，期望 50（Anthropic 系 = input_tokens 残值本身）", u.InputTokensUncached)
	}

	// 4.3.2 逐字样本（input_tokens=25 全量无缓存 ⇒ 总输入即 25）。
	u = a.ParseUsage(loadFixture(t, "anthropic_usage_sample.json"))
	if u.InputTokens != 25 || u.OutputTokens != 15 {
		t.Fatalf("逐字样本归一不对：In=%d(25) Out=%d(15)", u.InputTokens, u.OutputTokens)
	}
}

// TestAnthropicStreamCumulativeUsage 钉住流式 usage 口径（4.3.2：message_delta 的 usage 是
// 累计值 ⇒ 取累计快照，⛔ 不逐帧相加）：message_start 初始 1 + 两次 message_delta 累计 10 / 15
// ⇒ 末次 usage Output=15（逐帧相加的错误实现会得 1+10+15=26 ⇒ 红）。
// 输入侧不随输出增长 ⇒ message_delta 缺输入时合并 message_start 的 25。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestAnthropicStreamCumulativeUsage -v
func TestAnthropicStreamCumulativeUsage(t *testing.T) {
	const sse = "" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"output_tokens\":1,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":10}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":15}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newAnthropicResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	var usages []*core.Usage
	var texts []string
	var doneStatus string
	for _, f := range frames {
		switch f.Kind {
		case stream.FrameUsage:
			usages = append(usages, f.Usage)
		case stream.FrameTextDelta:
			texts = append(texts, f.Text)
		case stream.FrameDone:
			doneStatus = f.Status
		}
	}
	if len(usages) != 3 {
		t.Fatalf("usage 帧数 = %d，期望 3（message_start + 2×message_delta）：%+v", len(usages), frames)
	}
	// 累计快照：1 / 10 / 15（⛔ 不是逐帧相加的 1 / 11 / 26）。
	wantOut := []int64{1, 10, 15}
	for i, w := range wantOut {
		if usages[i].OutputTokens != w {
			t.Fatalf("usage[%d].OutputTokens = %d，期望 %d（累计快照 ⛔ 不逐帧相加）", i, usages[i].OutputTokens, w)
		}
	}
	// 输入侧合并 message_start 的 25（message_delta 缺输入字段）。
	if usages[1].InputTokens != 25 || usages[2].InputTokens != 25 {
		t.Fatalf("message_delta 输入侧 = %d / %d，期望 25（合并 message_start 初始值）",
			usages[1].InputTokens, usages[2].InputTokens)
	}
	if len(texts) != 1 || texts[0] != "你好" {
		t.Fatalf("文本增量 = %v，期望 [你好]", texts)
	}
	if doneStatus != "end_turn" {
		t.Fatalf("done 状态 = %q，期望 end_turn（message_stop 携带 stop_reason 超集原值）", doneStatus)
	}
}

// TestAnthropicStreamToolCallName 钉住流式工具调用名进帧（帧承载约定：FrameToolCallArgsDelta.Text
// 载函数名、ToolCallID 载调用 id）：函数名只在 content_block_start 的 tool_use 块出现
// （4.3.2），必须以补名帧（ToolCallID=toolu_1、Text=search）进入帧序列——⛔ 不能搭在
// FrameLifecycle 上（帧累加器忽略生命周期帧 ⇒ 名字丢失）；参数帧照常到（ToolCallID 一致）。
// 错误实现会红：不发补名帧（或名字只放生命周期帧）⇒ 找不到 Text=search 的参数帧 ⇒ named=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestAnthropicStreamToolCallName -v
func TestAnthropicStreamToolCallName(t *testing.T) {
	const sse = "" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"search\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":1}\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newAnthropicResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	var named, args int
	for _, f := range frames {
		if f.Kind == stream.FrameToolCallArgsDelta && f.ToolCallID == "toolu_1" {
			if f.Text == "search" {
				named++
			}
			if f.Arguments == "{\"q\":1}" {
				args++
			}
		}
	}
	if named == 0 {
		t.Fatalf("缺补名帧（ToolCallID=toolu_1 且 Text=search，函数名只在 content_block_start）：%+v", frames)
	}
	if args == 0 {
		t.Fatalf("参数帧缺失（ToolCallID=toolu_1 且 Arguments={\"q\":1}）：%+v", frames)
	}
}

// TestAnthropicParseUsageRealSample 钉住真实样本（litellm 真实录制，来源见 testdata/real/README.md）上
// ParseUsage 归一成立：残值合成 Input = input + cache_creation + cache_read（⛔ 不取 input_tokens 残值）——
// 写缓存轮 4+876+0=880、命中轮 4+0+873=877。
// 错误实现会红：直接取 input_tokens ⇒ 4 ≠ 880 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestAnthropicParseUsageRealSample -v
func TestAnthropicParseUsageRealSample(t *testing.T) {
	a := NewAnthropic(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/anthropic_usage_cachewrite.json"))
	if u.InputTokens != 880 || u.CacheWriteTokens != 876 || u.InputTokensUncached != 4 || u.OutputTokens != 235 {
		t.Fatalf("写缓存轮归一 = In%d/Write%d/Unc%d/Out%d，期望 880/876/4/235（4+876+0 残值合成）",
			u.InputTokens, u.CacheWriteTokens, u.InputTokensUncached, u.OutputTokens)
	}
	u = a.ParseUsage(loadFixture(t, "real/anthropic_usage_cachehit.json"))
	if u.InputTokens != 877 || u.CacheReadTokens != 873 || u.InputTokensUncached != 4 || u.OutputTokens != 254 {
		t.Fatalf("命中轮归一 = In%d/Read%d/Unc%d/Out%d，期望 877/873/4/254（4+0+873 残值合成）",
			u.InputTokens, u.CacheReadTokens, u.InputTokensUncached, u.OutputTokens)
	}
}

// TestAnthropicStreamRealCapture 钉住真实流式帧捕获（litellm 真实录制，msg_011CfyYUVrHK4BM53hfSH5Ld）
// 可被流式解码正确消费：message_start usage（23/6）+ message_delta 累计 usage（23/6）、文本增量 Hello.、
// done=end_turn；真实帧里的 ping 事件不得干扰。
// 错误实现会红：逐帧相加输出 ⇒ 12 ≠ 6 红；ping 当错误 ⇒ 流中断红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestAnthropicStreamRealCapture -v
func TestAnthropicStreamRealCapture(t *testing.T) {
	body := loadFixture(t, "real/anthropic_stream_real.sse")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	a := NewAnthropic(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newAnthropicResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	var usages []*core.Usage
	var texts []string
	var doneStatus string
	for _, f := range frames {
		switch f.Kind {
		case stream.FrameUsage:
			usages = append(usages, f.Usage)
		case stream.FrameTextDelta:
			texts = append(texts, f.Text)
		case stream.FrameDone:
			doneStatus = f.Status
		}
	}
	if len(usages) != 2 {
		t.Fatalf("usage 帧数 = %d，期望 2（message_start + message_delta）：%+v", len(usages), frames)
	}
	if usages[0].InputTokens != 23 || usages[0].OutputTokens != 6 {
		t.Fatalf("message_start usage = %d/%d，期望 23/6（样本逐字）", usages[0].InputTokens, usages[0].OutputTokens)
	}
	if usages[1].InputTokens != 23 || usages[1].OutputTokens != 6 {
		t.Fatalf("message_delta usage = %d/%d，期望累计 23/6（⛔ 非逐帧相加的 23/12）", usages[1].InputTokens, usages[1].OutputTokens)
	}
	if len(texts) != 1 || texts[0] != "Hello." {
		t.Fatalf("文本增量 = %v，期望 [Hello.]（样本逐字）", texts)
	}
	if doneStatus != "end_turn" {
		t.Fatalf("done 状态 = %q，期望 end_turn（样本逐字）", doneStatus)
	}
}

// TestAnthropicStreamSignatureDelta 钉住流式 thinking 签名进帧（signature_delta ⇒
// delta.signature 落 Frame.ThinkingSignature，⛔ 不可压平）：帧序列必须含签名帧，
// 承载约定为 FrameReasoningDelta 且 ThinkingSignature 非空、Reasoning 为空
// （见 stream.FrameReasoningDelta 注释），推理文本帧与其分离。
// 错误实现会红：signature_delta 被丢（帧抽象无签名槽位时的旧行为）⇒ 签名断言红；
// 签名压进 Reasoning 文本 ⇒ 承载约定断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestAnthropicStreamSignatureDelta -v
func TestAnthropicStreamSignatureDelta(t *testing.T) {
	const sse = "" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"推理过程\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-流式-1\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newAnthropicResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	var sigs []stream.Frame
	var reasoning string
	for _, f := range frames {
		if f.ThinkingSignature != "" {
			sigs = append(sigs, f)
		}
		if f.Kind == stream.FrameReasoningDelta && f.Reasoning != "" {
			reasoning += f.Reasoning
		}
	}
	if len(sigs) != 1 || sigs[0].ThinkingSignature != "sig-流式-1" {
		t.Fatalf("签名帧 = %+v，期望恰一帧带 sig-流式-1（signature_delta ⛔ 不可压平）", sigs)
	}
	if sigs[0].Kind != stream.FrameReasoningDelta || sigs[0].Reasoning != "" {
		t.Errorf("承载约定错（签名帧 = FrameReasoningDelta 且 Reasoning 为空）: %+v", sigs[0])
	}
	if reasoning != "推理过程" {
		t.Errorf("推理文本帧应与签名帧分离，正文 = %q", reasoning)
	}
}
