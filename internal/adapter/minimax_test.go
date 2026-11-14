// 日期: 2026-11-14 14:58:31.945
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

// 本文件覆盖 MiniMax 适配器（统一 SDD 4.3.11，三形态）：
// BuildRequest（⛔ 无 GroupId / temperature / top_p、max_completion_tokens、
// tool_choice 仅 auto ⇒ 降级记录）、base_resp 必检（HTTP 200 也判错）、
// thinking 拆包（splitThinkingTags / thinkSplitter —— 2026-11-14 修复后必须钉死）、
// ParseUsage（无缓存字段 ⇒ 记 0）。
//
// ⚠️ think 标签字面量在本文件用 \x3c 转义自建（⛔ 不复用实现里的常量——自建标签与断言
// 期望都独立于实现，实现常量若被写入链路吞掉，本测试立刻红；写入链路会静默吞掉
// 尖括号闭合形态的字面量，2026-11-14 实测）。

const (
	testThinkOpen  = "\x3cthink\x3e"
	testThinkClose = "\x3c/think\x3e"
)

func newMiniMaxResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderMiniMax,
		Shape:    core.APIShapeOpenAIChat,
		BaseURL:  base,
		Model:    "MiniMax-Text-01",
	}
}

// TestMiniMaxBuildRequestContract 钉住 MiniMax OpenAI 形请求契约（4.3.11 逐字）：
// ① ⛔ 无 GroupId / group_id（旧版遗留，沿用会引入无效参数）；
// ② ⛔ 无 temperature / top_p（不支持，传值无效 ⇒ 统一模板必须剔除）；
// ③ 输出上限 max_completion_tokens；流式 stream_options.include_usage；
// ④ tool_choice 仅 auto ⇒ 非 auto 降为 auto 并记降级（⛔ 不静默）。
// 错误实现会红：任一禁发键进 body ⇒ assertNoKey 红；tool_choice 降级不记 ⇒ degs 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxBuildRequestContract -v
func TestMiniMaxBuildRequestContract(t *testing.T) {
	a := NewMiniMax(http.DefaultClient)
	temp := 0.3
	r := &core.Request{
		Messages:        []core.Message{{Role: "user", Content: []core.ContentPart{{Text: "你好"}}}},
		MaxOutputTokens: 512,
		Stream:          true,
		Temperature:     &temp,
		ToolChoice:      &core.ToolChoice{Mode: "required"},
		Extra: map[string]any{
			"temperature":     0.3,
			"top_p":           0.9,
			"GroupId":         "g1",
			"group_id":        "g1",
			"reasoning_split": true,
		},
	}
	out, degs, err := a.(*minimaxAdapter).BuildRequestWithDegradations(context.Background(), r, newMiniMaxResolution("https://api.minimax.io/v1"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "https://api.minimax.io/v1/chat/completions" {
		t.Fatalf("URL = %s，期望 {base}/chat/completions（4.3.11）", out.URL)
	}
	m := bodyMap(t, out)
	// ①② 禁发键（4.3.11）。
	assertNoKey(t, m, "GroupId")
	assertNoKey(t, m, "group_id")
	assertNoKey(t, m, "temperature")
	assertNoKey(t, m, "top_p")
	// ③ max_completion_tokens + stream_options.include_usage。
	if mustNumber(t, m, "max_completion_tokens") != 512 {
		t.Fatalf("max_completion_tokens = %v，期望 512", m["max_completion_tokens"])
	}
	so := mustAt(t, m, "stream_options").(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("stream_options = %v，期望 include_usage:true", so)
	}
	// reasoning_split 等特异参数经 Extra 透传（4.3.11）。
	if mustAt(t, m, "reasoning_split") != true {
		t.Fatalf("reasoning_split = %v", m["reasoning_split"])
	}
	// ④ tool_choice 仅 auto + 降级记录。
	if mustAt(t, m, "tool_choice") != "auto" {
		t.Fatalf("tool_choice = %v，期望 auto（4.3.11 仅支持 auto）", m["tool_choice"])
	}
	if len(degs) != 1 || degs[0].Field != "tool_choice" || degs[0].Reason != core.DegradationCapabilityUnsupported {
		t.Fatalf("degradations = %+v，期望 1 条 tool_choice 降级（⛔ 不静默）", degs)
	}
	// AuthScheme：仅 Authorization: Bearer（⛔ 无 GroupId 鉴权遗留）。
	sch := a.AuthScheme()
	if sch.HeaderName != "Authorization" || sch.Prefix != "Bearer " {
		t.Fatalf("AuthScheme = %+v", sch)
	}
}

// TestMiniMaxV2BuildRequest 钉住原生 V2 端点（4.3.11：POST {base}/text/chatcompletion_v2）。
// 错误实现会红：端点错拼 ⇒ URL 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxV2BuildRequest -v
func TestMiniMaxV2BuildRequest(t *testing.T) {
	a := NewMiniMaxNativeV2(http.DefaultClient)
	out, err := a.BuildRequest(context.Background(), &core.Request{},
		&core.Resolution{Provider: core.ProviderMiniMax, Shape: core.APIShapeMiniMaxNativeV2,
			BaseURL: "https://api.minimax.io/v1", Model: "MiniMax-Text-01"})
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "https://api.minimax.io/v1/text/chatcompletion_v2" {
		t.Fatalf("URL = %s，期望 {base}/text/chatcompletion_v2", out.URL)
	}
	if a.OutboundShape() != ShapeMiniMaxNativeV2 {
		t.Fatalf("OutboundShape = %s", a.OutboundShape())
	}
}

// TestMiniMaxAnthropicShape 钉住 Anthropic 形（4.1.2 /anthropic/v1/messages + 4.3.11 残值语义）：
// x-api-key + anthropic-version 头；previous_response_id 不支持 ⇒ 显式报错（3.5）。
// 错误实现会红：静默丢 previous_response_id ⇒ err==nil ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxAnthropicShape -v
func TestMiniMaxAnthropicShape(t *testing.T) {
	a := NewMiniMaxAnthropic(http.DefaultClient)
	res := &core.Resolution{Provider: core.ProviderMiniMax, Shape: core.APIShapeAnthropicMessages,
		BaseURL: "https://api.minimax.io/anthropic/v1", Model: "MiniMax-Text-01"}
	out, err := a.BuildRequest(context.Background(), &core.Request{}, res)
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.URL != "https://api.minimax.io/anthropic/v1/messages" {
		t.Fatalf("URL = %s，期望 {base}/messages", out.URL)
	}
	sch := a.AuthScheme()
	if sch.HeaderName != "x-api-key" || sch.ExtraHeaders["anthropic-version"] != "2026-10-21" {
		t.Fatalf("AuthScheme = %+v，期望 x-api-key + anthropic-version", sch)
	}
	_, err = a.BuildRequest(context.Background(), &core.Request{PreviousResponseID: "resp_prev"}, res)
	requireDetailErr(t, err, "unsupported_capability")
	// 残值语义同 Claude（4.3.11 / 4.4.2 MiniMax Anthropic 行）。
	u := a.ParseUsage(json.RawMessage(`{"input_tokens":50,"output_tokens":30,"cache_creation_input_tokens":0,"cache_read_input_tokens":200000}`))
	if u.InputTokens != 200050 {
		t.Fatalf("Anthropic 形 InputTokens = %d，期望 200050（残值三项合成）", u.InputTokens)
	}
}

// TestMiniMaxBaseRespError 钉住 base_resp 必检（4.3.11：HTTP 200 + status_code != 0 ⇒ 业务错误，
// ⛔ 不检查 base_resp 就会把错误当成功）：1008 余额不足 ⇒ budget_exceeded 分类。
// 错误实现会红：跳过 base_resp 检查 ⇒ Status=completed ⇒ 红（恒假样本已核，见变异自检）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxBaseRespError -v
func TestMiniMaxBaseRespError(t *testing.T) {
	a := NewMiniMax(http.DefaultClient)
	out, err := a.ParseResponse(newMiniMaxResolution(""), loadFixture(t, "minimax_response_error.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if out.Status != core.StatusFailed || out.Error == nil {
		t.Fatalf("base_resp.status_code=1008 ⇒ 必须判错：%+v", out)
	}
	if out.Error.Code != "minimax_1008" || out.Error.Category != core.ErrBudgetExceeded {
		t.Fatalf("错误明细 = %+v，期望 minimax_1008 / budget_exceeded（4.3.11 错误码表）", out.Error)
	}
	if out.Error.UpstreamStatus != 200 {
		t.Fatalf("UpstreamStatus = %d，期望 200（业务错误承载于 HTTP 200 内）", out.Error.UpstreamStatus)
	}
	// V2 同口径（4.3.11 三形态共性）。
	v2 := NewMiniMaxNativeV2(http.DefaultClient)
	out, err = v2.ParseResponse(newMiniMaxResolution(""), loadFixture(t, "minimax_response_error.json"))
	if err != nil {
		t.Fatalf("ParseResponse(V2) 报错: %v", err)
	}
	if out.Status != core.StatusFailed || out.Error == nil {
		t.Fatalf("V2 base_resp 未判错: %+v", out)
	}
	// base_resp.status_code=0 ⇒ 正常；finish_reason=length ⇒ incomplete。
	out, err = a.ParseResponse(newMiniMaxResolution(""), loadFixture(t, "minimax_response_ok.json"))
	if err != nil {
		t.Fatalf("ParseResponse(ok) 报错: %v", err)
	}
	if out.Status != core.StatusIncomplete || out.FinishReason != core.FinishLength {
		t.Fatalf("ok 形解析不对: %q / %q", out.Status, out.FinishReason)
	}
	if out.Text != "正文内容" || out.OutputItems[0].ReasoningText != "显式思考" {
		t.Fatalf("thinking/正文分离不对: %+v（reasoning_content 进推理正文位）", out.OutputItems)
	}
}

// TestMiniMaxParseUsageNoCache 钉住无缓存字段 ⇒ 读写记 0（4.4.2 MiniMax 行 / 3.2）。
// 错误实现会红：凭 total−prompt 之类推缓存 ⇒ 非 0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxParseUsageNoCache -v
func TestMiniMaxParseUsageNoCache(t *testing.T) {
	a := NewMiniMax(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "minimax_usage.json"))
	if u.InputTokens != 100 || u.OutputTokens != 30 {
		t.Fatalf("归一不对：In=%d(100) Out=%d(30)", u.InputTokens, u.OutputTokens)
	}
	if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		t.Fatalf("MiniMax 无缓存字段 ⇒ 记 0：Read=%d Write=%d", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.InputTokensUncached != 100 {
		t.Fatalf("Uncached = %d，期望 100", u.InputTokensUncached)
	}
}

// TestSplitThinkingTags 钉住 thinking 拆包（4.3.11：reasoning_split=false 时思考并入 content
// 并用 think 标签包裹 ⇒ 必须拆包；2026-11-14 修复后本函数是重点钉死对象）：
// ① 完整段拆出（思考进 thinking、正文留 text）；② 多段拼接；③ 未闭合段按思考收尾 ⛔ 不丢内容；
// ④ 无标签纯正文；⑤ 空段。
// 错误实现会红：标签残留进正文 ⇒ 断言红；未闭合段被丢 ⇒ thinking 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestSplitThinkingTags -v
func TestSplitThinkingTags(t *testing.T) {
	cases := []struct {
		name, content, wantThink, wantText string
	}{
		{"完整段", "前文" + testThinkOpen + "思考" + testThinkClose + "后文", "思考", "前文后文"},
		{"多段拼接", testThinkOpen + "A" + testThinkClose + "中" + testThinkOpen + "B" + testThinkClose, "AB", "中"},
		{"未闭合段按思考收尾", "前" + testThinkOpen + "未闭合思考", "未闭合思考", "前"},
		{"无标签纯正文", "纯文本", "", "纯文本"},
		{"空段", testThinkOpen + testThinkClose, "", ""},
	}
	for _, c := range cases {
		think, text := splitThinkingTags(c.content)
		if think != c.wantThink || text != c.wantText {
			t.Fatalf("[%s] think=%q text=%q，期望 think=%q text=%q",
				c.name, think, text, c.wantThink, c.wantText)
		}
	}
}

// TestThinkSplitter 钉住流式 thinking 拆包状态机（跨增量半截标签 + Flush 收口）：
// ① 标签被增量切在中间（<thi / nk> 与 </th / ink>）⇒ 照常拆出，⛔ 不丢字；
// ② Flush 收口：未闭合段的残留按思考收尾、非思考态的半截开标签按正文收尾（不丢内容）。
// 错误实现会红：半截标签被当正文吐出 ⇒ 序列断言红；拼接结果含标签残片 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestThinkSplitter -v
func TestThinkSplitter(t *testing.T) {
	// ① 跨增量半截标签。
	var s thinkSplitter
	var think, text string
	for _, d := range []string{"前文" + testThinkOpen[:4], testThinkOpen[4:] + "思", "考" + testThinkClose[:4], testThinkClose[4:] + "后文"} {
		th, tx := s.Feed(d)
		think += th
		text += tx
	}
	th, tx := s.Flush()
	think += th
	text += tx
	if think != "思考" || text != "前文后文" {
		t.Fatalf("跨增量拆包不对：think=%q text=%q，期望 思考 / 前文后文", think, text)
	}

	// ② Flush 收口：未闭合 think 段的残留半截闭标签 ⇒ 按思考收尾。
	var s2 thinkSplitter
	th2, tx2 := s2.Feed("A" + testThinkOpen + "B" + testThinkClose[:3])
	if th2 != "B" || tx2 != "A" {
		t.Fatalf("Feed = (%q,%q)，期望 (B,A)", th2, tx2)
	}
	fh, ft := s2.Flush()
	if fh != testThinkClose[:3] || ft != "" {
		t.Fatalf("Flush = (%q,%q)，期望残留闭标签按思考收尾 (%q,\"\")", fh, ft, testThinkClose[:3])
	}

	// ③ 非思考态的半截开标签 ⇒ 按正文收尾（⛔ 不丢内容）。
	var s3 thinkSplitter
	_, tx3 := s3.Feed("X" + testThinkOpen[:4])
	if tx3 != "X" {
		t.Fatalf("Feed 文本 = %q，期望 X", tx3)
	}
	fh3, ft3 := s3.Flush()
	if fh3 != "" || ft3 != testThinkOpen[:4] {
		t.Fatalf("Flush = (%q,%q)，期望半截开标签按正文收尾 (\"\",%q)", fh3, ft3, testThinkOpen[:4])
	}
}

// TestMiniMaxStreamThinkingSplit 钉住流式 thinking 跨增量拆包 + 终态收口（4.3.11 + 3.7）：
// content 增量把 think 标签切在中间 ⇒ 思考/正文仍完整分离；finish_reason 帧前 Flush 收口。
// 错误实现会红：不拆包 ⇒ 文本帧含标签/思考混入正文 ⇒ 拼接断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxStreamThinkingSplit -v
func TestMiniMaxStreamThinkingSplit(t *testing.T) {
	chunk := func(content string) string {
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": content}}},
		})
		return "data: " + string(b) + "\n\n"
	}
	finish := `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	body := chunk("前文"+testThinkOpen[:4]) +
		chunk(testThinkOpen[4:]+"思") +
		chunk("考"+testThinkClose[:4]) +
		chunk(testThinkClose[4:]+"后文") +
		finish +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	a := NewMiniMax(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newMiniMaxResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	var think, text, doneStatus string
	for _, f := range frames {
		switch f.Kind {
		case stream.FrameReasoningDelta:
			think += f.Reasoning
		case stream.FrameTextDelta:
			text += f.Text
		case stream.FrameDone:
			doneStatus = f.Status
		}
	}
	if think != "思考" || text != "前文后文" {
		t.Fatalf("流式拆包不对：think=%q text=%q，期望 思考 / 前文后文（⛔ 标签残片混入正文）", think, text)
	}
	if doneStatus != "stop" {
		t.Fatalf("done 状态 = %q，期望 stop（4.3.11 流式 chunk 与非流式同一枚举）", doneStatus)
	}
}

// TestMiniMaxStreamBaseRespError 钉住流式 chunk 的 base_resp 必检（4.3.11：流式 chunk 同样携带）：
// status_code=1002 限流 ⇒ FrameError + 终态收口。
// 错误实现会红：流式忽略 base_resp ⇒ 无错误帧 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxStreamBaseRespError -v
func TestMiniMaxStreamBaseRespError(t *testing.T) {
	const body = `data: {"choices":[],"base_resp":{"status_code":1002,"status_msg":"限流"}}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	a := NewMiniMax(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newMiniMaxResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	if len(frames) != 1 || frames[0].Kind != stream.FrameError {
		t.Fatalf("frames = %+v，期望单个错误帧", frames)
	}
	if frames[0].Status != "minimax_1002" {
		t.Fatalf("错误帧 Status = %q，期望 minimax_1002（4.3.11 错误码 1002 限流）", frames[0].Status)
	}
}

// TestMiniMaxStreamToolCallName 钉住流式工具调用名进帧（帧承载约定：FrameToolCallArgsDelta.Text
// 载函数名）：OpenAI 形 chunk 的 tool_calls[].function.name（通常只在首块）必须进工具帧 Text，
// 参数增量照常拼接（ToolCallID 一致）。
// 错误实现会红：帧不带 Text ⇒ name 为空 ≠ search ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxStreamToolCallName -v
func TestMiniMaxStreamToolCallName(t *testing.T) {
	body := "" +
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","function":{"name":"search","arguments":"{\"q\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","function":{"arguments":"\"1\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	a := NewMiniMax(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newMiniMaxResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	var name, args string
	for _, f := range frames {
		if f.Kind == stream.FrameToolCallArgsDelta && f.ToolCallID == "call_1" {
			if f.Text != "" {
				name = f.Text
			}
			args += f.Arguments
		}
	}
	if name != "search" {
		t.Fatalf("工具函数名 = %q，期望 search（帧承载约定：FrameToolCallArgsDelta.Text 载函数名）；frames=%+v", name, frames)
	}
	if args != "{\"q\":\"1\"}" {
		t.Fatalf("参数拼接 = %q，期望 {\"q\":\"1\"}", args)
	}
}

// TestMiniMaxParseUsageRealSample 钉住真实样本（MiniMax 官方 API 文档文本合成响应示例，
// id 04ecb5d9b1921ae0fb0e8da9017a5474，来源见 testdata/real/README.md）上 ParseUsage 归一成立：
// 无缓存命中/写入字段 ⇒ 读写记 0；reasoning 214 单列（已含在 completion 223 内 ⛔ 不二次加）；
// 真实字段 total_characters 随 Raw 保留。
// 错误实现会红：reasoning 二次加进输出 ⇒ 437 ≠ 223 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxParseUsageRealSample -v
func TestMiniMaxParseUsageRealSample(t *testing.T) {
	a := NewMiniMaxNativeV2(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/minimax_usage.json"))
	if u.InputTokens != 26 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 || u.InputTokensUncached != 26 ||
		u.OutputTokens != 223 || u.ReasoningTokens != 214 || u.TotalTokens != 249 {
		t.Fatalf("归一 = In%d/Read%d/Write%d/Unc%d/Out%d/Reason%d/Total%d，期望 26/0/0/26/223/214/249（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.InputTokensUncached, u.OutputTokens, u.ReasoningTokens, u.TotalTokens)
	}
}

// TestMiniMaxDeferredDoneUsageBeforeDone 钉住 4.4.2「include_usage → 末帧」收口语义：
// finish 帧不立即收口，独立 usage 帧（finish 与 [DONE] 之间）先发、Done 后发。
// ⚠️ 2026-11-14 修复的 bug：原实现 finish 帧即收口 ⇒ usage 帧（同帧与后帧）全丢、
// 末帧 usage 缺失被置 Estimated 估算。错误实现对照：提前收口 ⇒ usage 帧缺失即红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxDeferredDone -v
func TestMiniMaxDeferredDoneUsageBeforeDone(t *testing.T) {
	d := newMinimaxChatStreamDecoder(func(json.RawMessage) *core.Usage {
		return &core.Usage{InputTokens: 10, OutputTokens: 5}
	})
	ev := func(data string) stream.SSEEvent { return stream.SSEEvent{Data: data} }
	var got []stream.Frame
	f, done, _ := d.decode(ev(`{"choices":[{"delta":{"content":"你好"},"finish_reason":"stop"}]}`))
	if done {
		t.Fatal("finish 帧处不应终态（独立 usage 帧在其后）")
	}
	got = append(got, f...)
	// usage 帧到达即收口：同批发 [Usage, Done]（usage 先于 Done 的承载约定）。
	f, done, _ = d.decode(ev(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	got = append(got, f...)
	if !done {
		t.Fatal("usage 帧处应随批收口（[Usage, Done] 同批，usage 先于 Done）")
	}
	f, done2, _ := d.decode(ev(`[DONE]`))
	got = append(got, f...)
	if !done2 || len(f) != 0 {
		t.Fatalf("[DONE] 已收口后应无帧，实际 %+v", f)
	}
	if len(got) != 3 || got[0].Kind != stream.FrameTextDelta || got[1].Kind != stream.FrameUsage || got[2].Kind != stream.FrameDone {
		t.Fatalf("帧序列 = %v/%v/%v，期望 TextDelta→Usage→Done（usage 先于 Done）", got[0].Kind, got[1].Kind, got[2].Kind)
	}
	if got[2].Status != "stop" {
		t.Fatalf("Done.Status = %q，期望 stop（暂存的 finish 原值）", got[2].Status)
	}
}

// TestMiniMaxSameChunkUsageFinish 钉住同帧 usage + finish 的收口：Usage→Done 同批返回。
// 错误实现对照：finish 分支提前 return 丢同帧 usage ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxSameChunkUsageFinish -v
func TestMiniMaxSameChunkUsageFinish(t *testing.T) {
	d := newMinimaxChatStreamDecoder(func(json.RawMessage) *core.Usage {
		return &core.Usage{InputTokens: 7, OutputTokens: 3}
	})
	f, done, _ := d.decode(stream.SSEEvent{Data: `{"choices":[{"delta":{"content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`})
	if !done || len(f) != 3 || f[1].Kind != stream.FrameUsage || f[2].Kind != stream.FrameDone || f[2].Status != "length" {
		t.Fatalf("同帧收口 = %+v done=%v，期望 TextDelta→Usage→Done(length) 且终态", f, done)
	}
}

// TestMiniMaxFinishWithoutUsageDoneAtSentinel 钉住无 usage 时 [DONE] 处补发 Done：
// 终止帧不许丢（否则客户端悬挂）。错误实现对照：延后收口后忘记在哨兵补 Done ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxFinishWithoutUsage -v
func TestMiniMaxFinishWithoutUsageDoneAtSentinel(t *testing.T) {
	d := newMinimaxChatStreamDecoder(func(json.RawMessage) *core.Usage { return nil })
	ev := func(data string) stream.SSEEvent { return stream.SSEEvent{Data: data} }
	d.decode(ev(`{"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`))
	f, done, _ := d.decode(ev(`[DONE]`))
	if !done || len(f) != 1 || f[0].Kind != stream.FrameDone || f[0].Status != "stop" {
		t.Fatalf("[DONE] 补收口 = %+v done=%v，期望恰一帧 Done(stop)", f, done)
	}
}
