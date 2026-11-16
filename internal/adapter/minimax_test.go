// 日期: 2026-11-14 14:58:31.945
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件覆盖 MiniMax 适配器（统一 SDD 4.3.11，三形态；① 出站已 Responses 化）：
// ① BuildRequest（POST {base}/responses、input/instructions/tools 扁平形、⛔ 无 GroupId /
// temperature / top_p、max_output_tokens、tool_choice 仅 auto ⇒ 降级记录、previous_response_id
// 不支持 ⇒ 显式报错）、ParseResponse / 流式（真实样本直喂 + 构造 wire 断言）、
// base_resp 必检（HTTP 200 也判错）；②③ 的既有覆盖（V2 / Anthropic）不动。
// thinking 拆包（splitThinkingTags / thinkSplitter —— 2026-11-14 修复后必须钉死；机件由 V2 承载，
// Responses 形推理走 reasoning item / 事件，⛔ 不拆标签）、ParseUsage（无缓存字段 ⇒ 记 0）。
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

// TestMiniMaxBuildRequestContract 钉住 MiniMax Responses 形请求契约（4.3.11 家差 + 3.5/3.6）：
// ① URL {base}/responses（官方 Create Response：POST /v1/responses）；
// ② 请求体 Responses 形：input 数组 / instructions 顶层 / tools 扁平形（⛔ 无 function 嵌套层）；
// ③ ⛔ 无 GroupId / temperature / top_p（Extra 黑名单剔除）、reasoning_split 等特异参数经 Extra 透传；
// ④ max_output_tokens + stream:true（Responses 形 usage 随 response.completed，⛔ 无 stream_options）；
// ⑤ tool_choice 仅 auto ⇒ 非 auto 降为 auto 并记降级（⛔ 不静默）；
// ⑥ previous_response_id 不支持 ⇒ 传入即 400 unsupported_capability（3.5）。
// 构造 wire 断言（真实样本直喂另见 TestMiniMaxParseResponseReal，2026-11-16 直采）；期望值取自官方 Create Response
// OpenAPI 示例（https://platform.minimax.io/docs/api-reference/responses-create）。
// 错误实现会红：任一禁发键进 body ⇒ assertNoKey 红；tool_choice 降级不记 ⇒ degs 断言红；
// 静默丢 previous_response_id ⇒ err==nil ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxBuildRequestContract -v
func TestMiniMaxBuildRequestContract(t *testing.T) {
	a := NewMiniMax(http.DefaultClient)
	r := &core.Request{
		Instructions: "主指令",
		Messages: []core.Message{
			{Role: "system", Content: []core.ContentPart{{Text: "系统提示"}}},
			{Role: "user", Content: []core.ContentPart{{Text: "你好"}}},
		},
		Tools: []core.Tool{{Type: "function", Name: "get_weather", Description: "查天气",
			Parameters: []byte(`{"type":"object"}`)}},
		MaxOutputTokens: 512,
		Stream:          true,
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
	if out.Method != "POST" || out.URL != "https://api.minimax.io/v1/responses" {
		t.Fatalf("URL/方法 = %s %s，期望 POST {base}/responses（官方 Create Response）", out.Method, out.URL)
	}
	m := bodyMap(t, out)
	// ② Responses 形：input 数组 + instructions 顶层（4.2.2/4.2.3：system 并入 instructions）。
	items := mustSlice(t, m, "input")
	if len(items) != 1 {
		t.Fatalf("input item 数 = %d，期望 1（system 并入 instructions）：%v", len(items), items)
	}
	it := items[0].(map[string]any)
	if it["type"] != "message" || it["role"] != "user" {
		t.Fatalf("input[0] = %v，期望 message/user", it)
	}
	if mustString(t, m, "instructions") != "主指令\n系统提示" {
		t.Fatalf("instructions = %q，期望 %q", m["instructions"], "主指令\n系统提示")
	}
	// tools 扁平形（官方请求示例逐字：{"type":"function","name":...}）。
	t0 := mustSlice(t, m, "tools")[0].(map[string]any)
	if t0["type"] != "function" || t0["name"] != "get_weather" {
		t.Fatalf("tools[0] = %v，期望扁平形 {type:function,name:get_weather}", t0)
	}
	if _, nested := t0["function"]; nested {
		t.Fatalf("tools[0] 含 function 嵌套层 %v，期望扁平形（Responses 规范）", t0)
	}
	// ③ 禁发键（4.3.11）+ 特异参数 Extra 透传。
	assertNoKey(t, m, "GroupId")
	assertNoKey(t, m, "group_id")
	assertNoKey(t, m, "temperature")
	assertNoKey(t, m, "top_p")
	if mustAt(t, m, "reasoning_split") != true {
		t.Fatalf("reasoning_split = %v，期望 true（Extra 透传）", m["reasoning_split"])
	}
	// ④ max_output_tokens + stream:true（⛔ 无 stream_options）。
	if mustNumber(t, m, "max_output_tokens") != 512 {
		t.Fatalf("max_output_tokens = %v，期望 512", m["max_output_tokens"])
	}
	if mustAt(t, m, "stream") != true {
		t.Fatalf("stream = %v，期望 true", m["stream"])
	}
	assertNoKey(t, m, "stream_options")
	// ⑤ tool_choice 仅 auto + 降级记录。
	if mustAt(t, m, "tool_choice") != "auto" {
		t.Fatalf("tool_choice = %v，期望 auto（4.3.11 仅支持 auto）", m["tool_choice"])
	}
	if len(degs) != 1 || degs[0].Field != "tool_choice" || degs[0].Reason != core.DegradationCapabilityUnsupported {
		t.Fatalf("degradations = %+v，期望 1 条 tool_choice 降级（⛔ 不静默）", degs)
	}
	// ⑥ previous_response_id 传入即 400（3.5：⛔ 不静默丢弃）。
	_, err = a.BuildRequest(context.Background(), &core.Request{PreviousResponseID: "resp_prev"}, newMiniMaxResolution("https://api.minimax.io/v1"))
	requireDetailErr(t, err, "unsupported_capability")
	if a.SupportsPreviousResponseID() {
		t.Fatalf("SupportsPreviousResponseID = true，期望 false（/responses 无会话态：官方响应示例 store:false / previous_response_id:null）")
	}
	// AuthScheme：仅 Authorization: Bearer（⛔ 无 GroupId 鉴权遗留）。
	sch := a.AuthScheme()
	if sch.HeaderName != "Authorization" || sch.Prefix != "Bearer " {
		t.Fatalf("AuthScheme = %+v", sch)
	}
}

// TestMiniMaxResponsesParseResponse 钉住 Responses 形响应解析（官方 Create Response 响应示例
// 逐字，出处 platform.minimax.io/docs/api-reference/responses-create 与国内站 .md 同源）：
// output[] 异构 item（reasoning / message / function_call）、finish 推导（completed + function_call
// ⇒ tool_calls；incomplete + max_output_tokens ⇒ length）、usage 注入（Responses 树
// input_tokens_details.cached_tokens / output_tokens_details.reasoning_tokens）。
// ①「工具调用」响应例逐字（id 07229fa4f6440a49153da1b91f2204b1，usage 465/42/507、cached 175）；
// ②「深度思考」响应例逐字（id 07229fa4ff19f5b74ad4581cad3af46c，usage 216/107/323、cached 189、
//
//	reasoning 75）；③ incomplete / error 为构造（incomplete_details.reason 取官方 schema enum 逐字值）；
//
// ④ encrypted_content 保留（构造；真实 wire 会出现——第三方实打样本 reasoning item 带该字段而官方
//
//	schema 未列，见 docs/20261116-MiniMax-调研-Responses回复形态.md）。
//
// ⚠️ 无 key 直采样本（2026-11-14 key 鉴权拒绝）⇒ 官方文档示例断言，真机直采待 key 补采。
// 错误实现会红：丢 encrypted_content / reasoning_text 映射错 ⇒ item 断言红；finish 推导错 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxResponsesParseResponse -v
func TestMiniMaxResponsesParseResponse(t *testing.T) {
	a := NewMiniMax(http.DefaultClient)
	// ① 官方「工具调用」响应例逐字。
	const toolsBody = `{"id":"07229fa4f6440a49153da1b91f2204b1","object":"response","created_at":1792240804,"model":"MiniMax-M3.1-Flash","status":"completed","output":[{"id":"07229fa4f6440a49153da1b91f2204b1_msg","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"I'll check the current weather in San Francisco for you.","annotations":[]}]},{"id":"07229fa4f6440a49153da1b91f2204b1_fc_0","type":"function_call","status":"completed","call_id":"call_function_2uih32x750pd_1","name":"get_weather","arguments":"{\"location\": \"San Francisco, US\"}"}],"output_text":"I'll check the current weather in San Francisco for you.","usage":{"input_tokens":465,"output_tokens":42,"total_tokens":507,"input_tokens_details":{"cached_tokens":175}}}`
	out, err := a.ParseResponse(newMiniMaxResolution(""), []byte(toolsBody))
	if err != nil {
		t.Fatalf("ParseResponse(工具调用例) 报错: %v", err)
	}
	if out.Text != "I'll check the current weather in San Francisco for you." || out.Status != core.StatusCompleted {
		t.Fatalf("Text/Status = %q/%q", out.Text, out.Status)
	}
	if len(out.OutputItems) != 2 || out.OutputItems[0].Type != "message" {
		t.Fatalf("OutputItems = %+v，期望 [message, function_call]", out.OutputItems)
	}
	fc := out.OutputItems[1]
	if fc.Type != "function_call" || fc.CallID != "call_function_2uih32x750pd_1" || fc.Name != "get_weather" ||
		fc.Arguments != `{"location": "San Francisco, US"}` {
		t.Fatalf("function_call item = %+v，期望官方例逐字", fc)
	}
	if out.FinishReason != core.FinishToolCalls {
		t.Fatalf("FinishReason = %q，期望 tool_calls（completed + function_call 推导）", out.FinishReason)
	}
	u := out.Usage
	if u.InputTokens != 465 || u.CacheReadTokens != 175 || u.CacheWriteTokens != 0 || u.OutputTokens != 42 || u.TotalTokens != 507 {
		t.Fatalf("usage 归一 = In%d/Read%d/Write%d/Out%d/Total%d，期望 465/175/0/42/507（官方例逐字）",
			u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens, u.TotalTokens)
	}
	if u.InputTokensUncached != 290 {
		t.Fatalf("Uncached = %d，期望 290 = 465−175−0", u.InputTokensUncached)
	}
	// ② 官方「深度思考」响应例逐字（reasoning item 的 content.reasoning_text 进推理正文位）。
	const thinkBody = `{"id":"07229fa4ff19f5b74ad4581cad3af46c","object":"response","created_at":1792240804,"model":"MiniMax-M3.1-Flash","status":"completed","output":[{"id":"07229fa4ff19f5b74ad4581cad3af46c_rs","type":"reasoning","status":"completed","summary":[],"content":[{"type":"reasoning_text","text":"We need answer Chinese. Need determine 9.11 vs 9.9. Decimal comparison: 9.9 = 9.90 > 9.11. User asks which larger. Need concise maybe explain. Ensure not date ambiguity? They ask 9.11 and 9.9 likely decimals. Answer 9.9 larger. Chinese."}]},{"id":"07229fa4ff19f5b74ad4581cad3af46c_msg","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"9.9 更大。\n\n因为把小数位补齐：9.9 = 9.90，而 9.90 > 9.11。","annotations":[]}]}],"output_text":"9.9 更大。\n\n因为把小数位补齐：9.9 = 9.90，而 9.90 > 9.11。","usage":{"input_tokens":216,"output_tokens":107,"total_tokens":323,"input_tokens_details":{"cached_tokens":189},"output_tokens_details":{"reasoning_tokens":75}}}`
	out, err = a.ParseResponse(newMiniMaxResolution(""), []byte(thinkBody))
	if err != nil {
		t.Fatalf("ParseResponse(深度思考例) 报错: %v", err)
	}
	if out.Text != "9.9 更大。\n\n因为把小数位补齐：9.9 = 9.90，而 9.90 > 9.11。" {
		t.Fatalf("Text = %q，期望官方例逐字", out.Text)
	}
	if len(out.OutputItems) != 2 || out.OutputItems[0].Type != "reasoning" || out.OutputItems[1].Type != "message" {
		t.Fatalf("OutputItems = %+v，期望 [reasoning, message]", out.OutputItems)
	}
	rs := out.OutputItems[0]
	if rs.ReasoningText != "We need answer Chinese. Need determine 9.11 vs 9.9. Decimal comparison: 9.9 = 9.90 > 9.11. User asks which larger. Need concise maybe explain. Ensure not date ambiguity? They ask 9.11 and 9.9 likely decimals. Answer 9.9 larger. Chinese." {
		t.Fatalf("reasoning_text 未进推理正文位: %q", rs.ReasoningText)
	}
	if out.FinishReason != core.FinishStop {
		t.Fatalf("FinishReason = %q，期望 stop", out.FinishReason)
	}
	u = out.Usage
	if u.InputTokens != 216 || u.CacheReadTokens != 189 || u.OutputTokens != 107 || u.TotalTokens != 323 || u.ReasoningTokens != 75 {
		t.Fatalf("usage 归一 = In%d/Read%d/Out%d/Total%d/Reason%d，期望 216/189/107/323/75（官方例逐字）",
			u.InputTokens, u.CacheReadTokens, u.OutputTokens, u.TotalTokens, u.ReasoningTokens)
	}
	if u.InputTokensUncached != 27 {
		t.Fatalf("Uncached = %d，期望 27 = 216−189−0", u.InputTokensUncached)
	}
	// ③ incomplete + max_output_tokens ⇒ length（4.5.2；reason 取官方 schema enum 逐字值）。
	const incBody = `{"id":"resp_mm_2","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},
		"output":[{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":"截断"}]}]}`
	out, err = a.ParseResponse(newMiniMaxResolution(""), []byte(incBody))
	if err != nil {
		t.Fatalf("ParseResponse(incomplete) 报错: %v", err)
	}
	if out.FinishReason != core.FinishLength || out.Status != core.StatusIncomplete {
		t.Fatalf("incomplete 解析 = %q/%q，期望 length/incomplete", out.FinishReason, out.Status)
	}
	// error 对象 ⇒ failed（官方 schema：error 仅在 status=failed 时返回）。
	const errBody = `{"id":"resp_mm_3","status":"failed","error":{"code":"server_error","message":"内部错误"}}`
	out, err = a.ParseResponse(newMiniMaxResolution(""), []byte(errBody))
	if err != nil {
		t.Fatalf("ParseResponse(error) 报错: %v", err)
	}
	if out.Status != core.StatusFailed || out.Error == nil {
		t.Fatalf("error 对象未判错: %+v", out)
	}
	// ④ encrypted_content 保留（4.2.5 ⛔ 必须保留；构造——真实 wire 会出现，出处见注释头）。
	const encBody = `{"id":"resp_mm_4","status":"completed","output":[{"id":"rs_4","type":"reasoning","status":"completed","summary":[],"content":[{"type":"reasoning_text","text":"想"}],"encrypted_content":"enc-mm-1"}]}`
	out, err = a.ParseResponse(newMiniMaxResolution(""), []byte(encBody))
	if err != nil {
		t.Fatalf("ParseResponse(encrypted) 报错: %v", err)
	}
	if len(out.OutputItems) != 1 || out.OutputItems[0].EncryptedContent != "enc-mm-1" {
		t.Fatalf("encrypted_content 丢失: %+v（4.2.5 ⛔ 必须保留）", out.OutputItems)
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
// ① Responses 形（minimaxAdapter）构造 wire 断言（真实样本直喂另见 TestMiniMaxParseResponseReal；base_resp 为 chat 形
//
//	实证形态，/responses 官方示例未见该字段 ⇒ 按「base_resp 错误处理保持现状」保守探测）；
//
// ② V2 同口径（4.3.11 形态共性，既有 fixture 断言不变）；
// ③ base_resp.status_code=0 ⇒ 正常（chat 形 thinking/正文分离由 V2/minimaxParseChat 承载；
//
//	Responses 形推理走 reasoning item，见 TestMiniMaxResponsesParseResponse）。
//
// 错误实现会红：跳过 base_resp 检查 ⇒ Status=completed ⇒ 红（恒假样本已核，见变异自检）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxBaseRespError -v
func TestMiniMaxBaseRespError(t *testing.T) {
	a := NewMiniMax(http.DefaultClient)
	out, err := a.ParseResponse(newMiniMaxResolution(""),
		[]byte(`{"id":"resp_x","model":"MiniMax-Text-01","base_resp":{"status_code":1008,"status_msg":"余额不足"}}`))
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
	// ② V2 同口径（4.3.11 形态共性）。
	v2 := NewMiniMaxNativeV2(http.DefaultClient)
	out, err = v2.ParseResponse(newMiniMaxResolution(""), loadFixture(t, "minimax_response_error.json"))
	if err != nil {
		t.Fatalf("ParseResponse(V2) 报错: %v", err)
	}
	if out.Status != core.StatusFailed || out.Error == nil {
		t.Fatalf("V2 base_resp 未判错: %+v", out)
	}
	// ③ base_resp.status_code=0 ⇒ 正常；finish_reason=length ⇒ incomplete。
	out, err = v2.ParseResponse(newMiniMaxResolution(""), loadFixture(t, "minimax_response_ok.json"))
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
// ⚠️ 钉死目标随拆包机件移位：think 标签拆包是 chat 形语义（reasoning_split=false 时思考并入
// content），Responses 形推理走 reasoning item / 事件（官方 Create Response 示例，⛔ 不拆标签）
// ⇒ 拆包机件唯一存活使用者是 V2 流式路径，本用例改钉 V2（拆包行为本身不变，2026-11-14 修复保留）。
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

	a := NewMiniMaxNativeV2(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{},
		&core.Resolution{Provider: core.ProviderMiniMax, Shape: core.APIShapeMiniMaxNativeV2,
			BaseURL: srv.URL, Model: "MiniMax-Text-01"})
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

// TestMiniMaxStreamBaseRespError 钉住流式事件的 base_resp 必检（4.3.11「流式 chunk 同样携带」+
// 任务书「base_resp 错误处理保持现状」；⚠️ /responses 官方 SSE 示例未见 base_resp ⇒ 构造事件断言）：
// status_code=1002 限流 ⇒ FrameError + 终态收口。
// 错误实现会红：流式忽略 base_resp ⇒ 无错误帧 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxStreamBaseRespError -v
func TestMiniMaxStreamBaseRespError(t *testing.T) {
	const body = "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":0,\"delta\":\"x\",\"base_resp\":{\"status_code\":1002,\"status_msg\":\"限流\"}}\n\n"
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
// 载函数名）：Responses 形 output_item.added 的 item.name 以补名帧进帧序列（arguments.delta
// 不再带名），参数增量照常拼接（ToolCallID 一致）。
// 构造事件断言（真实流直喂另见 TestMiniMaxStreamRealResponses，2026-11-16 直采）。
// 错误实现会红：不发补名帧 ⇒ name 为空 ≠ search ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxStreamToolCallName -v
func TestMiniMaxStreamToolCallName(t *testing.T) {
	body := "" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":0,"item_id":"fc_1","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search"}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","sequence_number":1,"item_id":"fc_1","partial_json":"{\"q\":"}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","sequence_number":2,"item_id":"fc_1","partial_json":"\"1\"}"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","sequence_number":3}` + "\n\n"
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

// TestMiniMaxResponsesStream 钉住 Responses 形流式帧序列（构造事件流）：
// Lifecycle(response.created) → ReasoningDelta → TextDelta → Usage → Done(completed)；
// usage 由 response.completed 自带（⛔ 不需要 stream_options）。
// 构造事件断言（真实流直喂另见 TestMiniMaxStreamRealResponses，2026-11-16 直采）；事件序取自官方 Create Response
// OpenAPI 示例（https://platform.minimax.io/docs/api-reference/responses-create）。
// 错误实现会红：帧序/帧种错配 ⇒ 序列断言红；usage 丢失 ⇒ Usage 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxResponsesStream -v
func TestMiniMaxResponsesStream(t *testing.T) {
	const body = "" +
		"event: response.created\n" +
		`data: {"type":"response.created","sequence_number":0}` + "\n\n" +
		"event: response.reasoning_text.delta\n" +
		`data: {"type":"response.reasoning_text.delta","sequence_number":1,"delta":"想"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","sequence_number":2,"delta":"你好"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","sequence_number":3,"response":{"usage":{"input_tokens":200,"output_tokens":50,"total_tokens":250}}}` + "\n\n"
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
	wantKinds := []stream.FrameKind{stream.FrameLifecycle, stream.FrameReasoningDelta, stream.FrameTextDelta, stream.FrameUsage, stream.FrameDone}
	if len(frames) != len(wantKinds) {
		t.Fatalf("帧数 = %d，期望 %d：%+v", len(frames), len(wantKinds), frames)
	}
	for i, wk := range wantKinds {
		if frames[i].Kind != wk {
			t.Fatalf("帧[%d].Kind = %v，期望 %v（帧序列 ReasoningDelta→TextDelta→Usage→Done）", i, frames[i].Kind, wk)
		}
	}
	if frames[0].Status != "response.created" || frames[1].Reasoning != "想" || frames[2].Text != "你好" {
		t.Fatalf("帧载荷不对: %+v", frames[:3])
	}
	if frames[3].Usage == nil || frames[3].Usage.InputTokens != 200 || frames[3].Usage.OutputTokens != 50 || frames[3].Usage.TotalTokens != 250 {
		t.Fatalf("usage 帧 = %+v，期望 200/50/250（response.completed 自带）", frames[3].Usage)
	}
	if frames[4].Status != core.StatusCompleted {
		t.Fatalf("Done.Status = %q，期望 completed", frames[4].Status)
	}
}

// TestMiniMaxParseResponseReal 钉住真实样本直喂 ParseResponse（testdata/real/
// minimax_responses_*_real.json，2026-11-16 MiniMax 国内站 api.minimax.cn 实调直采
// （MiniMax-M3.1-Flash；key 绑站——io 站同 key 401），来源见 testdata/real/README.md）：
// basic：output[reasoning, message]（message「好，说定了！😄」）、reasoning 带 encrypted_content
// （summary 空）、usage 逐字 204/0/75/279（reasoning 66）；
// tools：output[message, function_call]（call_id/name/arguments 逐字、扁平工具形直通）、
// finish ⇒ tool_calls、**cached_tokens 真实命中 189**（⛔ 「无缓存字段记 0」是 chat 形旧口径）；
// reasoning：缓存命中 197、reasoning_tokens 106。
// 错误实现会红：按 chat 形解析 ⇒ items 全丢；缓存误记 0 ⇒ CacheReadTokens 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxParseResponseReal -v
func TestMiniMaxParseResponseReal(t *testing.T) {
	a := NewMiniMax(http.DefaultClient)
	out, err := a.ParseResponse(newMiniMaxResolution(""), loadFixture(t, "real/minimax_responses_basic_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse(basic) 报错: %v", err)
	}
	if out.Status != core.StatusCompleted || out.FinishReason != core.FinishStop {
		t.Fatalf("basic 解析 = %q/%q，期望 completed/stop", out.Status, out.FinishReason)
	}
	if out.Text != "好，说定了！😄" {
		t.Fatalf("basic Text = %q，期望 %q", out.Text, "好，说定了！😄")
	}
	if len(out.OutputItems) != 2 || out.OutputItems[0].Type != "reasoning" || out.OutputItems[1].Type != "message" {
		t.Fatalf("basic OutputItems = %+v，期望 [reasoning, message]", out.OutputItems)
	}
	if out.OutputItems[0].EncryptedContent == "" {
		t.Fatalf("basic reasoning item = %+v，期望 encrypted_content 保留", out.OutputItems[0])
	}
	u := out.Usage
	if u.InputTokens != 204 || u.CacheReadTokens != 0 || u.OutputTokens != 75 || u.TotalTokens != 279 || u.ReasoningTokens != 66 {
		t.Fatalf("basic usage = In%d/Read%d/Out%d/Total%d/Rsn%d，期望 204/0/75/279/66（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.OutputTokens, u.TotalTokens, u.ReasoningTokens)
	}
	out, err = a.ParseResponse(newMiniMaxResolution(""), loadFixture(t, "real/minimax_responses_tools_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse(tools) 报错: %v", err)
	}
	if out.FinishReason != core.FinishToolCalls {
		t.Fatalf("tools FinishReason = %q，期望 tool_calls", out.FinishReason)
	}
	if len(out.OutputItems) != 2 || out.OutputItems[0].Type != "message" || out.OutputItems[1].Type != "function_call" {
		t.Fatalf("tools OutputItems = %+v，期望 [message, function_call]", out.OutputItems)
	}
	fc := out.OutputItems[1]
	if fc.CallID != "call_cw9YJCKpV3OyLPrwtT4jxuUu" || fc.Name != "get_weather" || fc.Arguments != `{"city": "北京"}` {
		t.Fatalf("function_call item = %+v，期望 call_cw9YJCKpV3OyLPrwtT4jxuUu/get_weather 逐字", fc)
	}
	if out.Usage.InputTokens != 499 || out.Usage.CacheReadTokens != 189 || out.Usage.OutputTokens != 38 || out.Usage.TotalTokens != 537 {
		t.Fatalf("tools usage = In%d/Read%d/Out%d/Total%d，期望 499/189/38/537（样本逐字；缓存命中 189 ⛔ 不记 0）",
			out.Usage.InputTokens, out.Usage.CacheReadTokens, out.Usage.OutputTokens, out.Usage.TotalTokens)
	}
	out, err = a.ParseResponse(newMiniMaxResolution(""), loadFixture(t, "real/minimax_responses_reasoning_real.json"))
	if err != nil {
		t.Fatalf("ParseResponse(reasoning) 报错: %v", err)
	}
	if out.Usage.CacheReadTokens != 197 || out.Usage.ReasoningTokens != 106 {
		t.Fatalf("reasoning usage = Read%d/Rsn%d，期望 197/106（样本逐字）", out.Usage.CacheReadTokens, out.Usage.ReasoningTokens)
	}
}

// TestMiniMaxStreamRealResponses 钉住真实流样本直喂解码器（testdata/real/
// minimax_responses_stream_real.sse，2026-11-16 api.minimax.cn 逐帧直采）：
// response.completed 收口（无 [DONE]）；帧序列 Lifecycle×2 → ReasoningDelta×2 → TextDelta×1
// → Usage → Done；usage 由 response.completed 自带（204/42/246，reasoning 35）。
// 错误实现会红：帧序/帧种错配、收口依赖 [DONE]（真机流无 [DONE]）⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestMiniMaxStreamRealResponses -v
func TestMiniMaxStreamRealResponses(t *testing.T) {
	f, err := os.Open("testdata/real/minimax_responses_stream_real.sse")
	if err != nil {
		t.Fatalf("打开真实样本失败: %v", err)
	}
	defer f.Close()
	a := NewMiniMax(http.DefaultClient)
	d := a.(*minimaxAdapter).streamDecoder()
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
		t.Fatal("response.completed 未收口（真机流无 [DONE]，以终态事件判结束）")
	}
	wantKinds := []stream.FrameKind{
		stream.FrameLifecycle, stream.FrameLifecycle,
		stream.FrameReasoningDelta, stream.FrameReasoningDelta,
		stream.FrameTextDelta, stream.FrameUsage, stream.FrameDone,
	}
	if len(frames) != len(wantKinds) {
		t.Fatalf("帧数 = %d，期望 %d：%+v", len(frames), len(wantKinds), frames)
	}
	var think, text string
	for i, wk := range wantKinds {
		if frames[i].Kind != wk {
			t.Fatalf("帧[%d].Kind = %v，期望 %v（Lifecycle×2→ReasoningDelta×2→TextDelta→Usage→Done）", i, frames[i].Kind, wk)
		}
		switch frames[i].Kind {
		case stream.FrameReasoningDelta:
			think += frames[i].Reasoning
		case stream.FrameTextDelta:
			text += frames[i].Text
		}
	}
	if !strings.HasPrefix(think, `We need respond Chinese. User says "说好"`) || len([]rune(think)) != 144 {
		t.Fatalf("推理增量拼接 = %d 字，前缀不对（期望 144 字、以 We need respond 开头）", len([]rune(think)))
	}
	if text != "好，就这么定了。" {
		t.Fatalf("正文增量拼接 = %q，期望 %q", text, "好，就这么定了。")
	}
	usageFrame := frames[len(frames)-2]
	if usageFrame.Usage == nil || usageFrame.Usage.InputTokens != 204 || usageFrame.Usage.OutputTokens != 42 ||
		usageFrame.Usage.TotalTokens != 246 || usageFrame.Usage.ReasoningTokens != 35 {
		t.Fatalf("usage 帧 = %+v，期望 204/42/246、reasoning 35（response.completed 自带，样本逐字）", usageFrame.Usage)
	}
	if frames[len(frames)-1].Status != core.StatusCompleted {
		t.Fatalf("Done.Status = %q，期望 completed", frames[len(frames)-1].Status)
	}
}
