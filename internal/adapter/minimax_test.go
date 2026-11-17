// 日期: 2026-11-14 14:58:31.945
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

// 本文件覆盖 MiniMax 适配器（统一 SDD 4.3.11，单形态：国内供应商统一 OpenAI Responses 通道）：
// BuildRequest（POST {base}/responses、input/instructions/tools 扁平形、⛔ 无 GroupId /
// temperature / top_p、max_output_tokens、tool_choice 仅 auto ⇒ 降级记录、previous_response_id
// 不支持 ⇒ 显式报错）、ParseResponse / 流式（真实样本直喂 + 构造 wire 断言）、
// base_resp 必检（HTTP 200 也判错）、ParseUsage（无缓存字段 ⇒ 记 0）。
// ⛔ 原生 V2 / Anthropic 形态已移除——出站一律 Responses 通道；推理走 reasoning item / 事件，⛔ 不拆标签。
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
	out, degs, err := a.(*minimaxAdapter).BuildRequestWithDegradations(context.Background(), r, newMiniMaxResolution("https://api.minimax.cn/v1"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.Method != "POST" || out.URL != "https://api.minimax.cn/v1/responses" {
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
	_, err = a.BuildRequest(context.Background(), &core.Request{PreviousResponseID: "resp_prev"}, newMiniMaxResolution("https://api.minimax.cn/v1"))
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
	// ② base_resp.status_code=0 ⇒ 正常（Responses 形推理走 reasoning item，见
	// TestMiniMaxResponsesParseResponse；⛔ 无 chat 形 thinking 拆包——原生 V2 已移除）。
	out, err = a.ParseResponse(newMiniMaxResolution(""),
		[]byte(`{"id":"resp_y","model":"MiniMax-Text-01","status":"completed","base_resp":{"status_code":0},"output":[]}`))
	if err != nil {
		t.Fatalf("ParseResponse(ok) 报错: %v", err)
	}
	if out.Status != core.StatusCompleted {
		t.Fatalf("status_code=0 ⇒ 正常：Status=%q", out.Status)
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
	a := NewMiniMax(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/minimax_usage.json"))
	if u.InputTokens != 26 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 || u.InputTokensUncached != 26 ||
		u.OutputTokens != 223 || u.ReasoningTokens != 214 || u.TotalTokens != 249 {
		t.Fatalf("归一 = In%d/Read%d/Write%d/Unc%d/Out%d/Reason%d/Total%d，期望 26/0/0/26/223/214/249（样本逐字）",
			u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.InputTokensUncached, u.OutputTokens, u.ReasoningTokens, u.TotalTokens)
	}
}
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
