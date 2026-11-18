// 日期: 2026-11-14 14:58:31.435
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

// 本文件覆盖 OpenAI 适配器（统一 SDD 4.3.1「统一格式的基准」）：
// BuildRequest 的 Responses 形状（input[] 异构 item / instructions / prompt_cache_key /
// previous_response_id）、ParseResponse（output[] 异构联合，⛔ 不假设 output[0] 是 message）、
// ParseUsage 两套 usage 树（Chat 125/48/173/98 逐字样本 / Responses 树）、
// 流式容忍性（缺 output_item.added、未知事件跳过、缺 [DONE]）。

// newOpenAIResolution 构造测试用识别结果。
func newOpenAIResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderOpenAI,
		Shape:    core.APIShapeOpenAIResponses,
		BaseURL:  base,
		Model:    "gpt-5.3",
	}
}

// TestOpenAIBuildRequestResponses 钉住 Responses 请求形状（4.3.1 请求体关键字段逐字）：
// input[] item、instructions 顶层、prompt_cache_key 透传（3.5 始终透传）、previous_response_id。
// 错误实现会红：把消息直接塞 messages[]、prompt_cache_key 不透传、system 不并入 instructions ⇒ 逐项红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOpenAIBuildRequestResponses -v
func TestOpenAIBuildRequestResponses(t *testing.T) {
	a := NewOpenAI(http.DefaultClient)
	r := &core.Request{
		Instructions: "主指令",
		Messages: []core.Message{
			{Role: "system", Content: []core.ContentPart{{Text: "系统提示"}}},
			{Role: "user", Content: []core.ContentPart{{Text: "你好"}}},
			{
				Role:             "assistant",
				EncryptedContent: "enc-1",
				ReasoningContent: "想",
				Content:          []core.ContentPart{{Text: "答案"}},
				ToolCalls:        []core.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"北京"}`}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: []core.ContentPart{{Text: "结果"}}},
		},
		Tools: []core.Tool{{Type: "function", Name: "get_weather", Description: "查天气",
			Parameters: []byte(`{"type":"object"}`)}},
		ToolChoice:         &core.ToolChoice{Mode: "function", Name: "get_weather"},
		MaxOutputTokens:    1024,
		PromptCacheKey:     "pk-1",
		PreviousResponseID: "resp_prev",
		Reasoning:          &core.ReasoningConfig{Effort: "medium", Summary: "auto"},
	}
	res := newOpenAIResolution("https://api.openai.com/v1")
	out, err := a.BuildRequest(context.Background(), r, res)
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	if out.Method != "POST" || out.URL != "https://api.openai.com/v1/responses" {
		t.Fatalf("URL/方法不对: %s %s（4.3.1：POST /responses）", out.Method, out.URL)
	}
	m := bodyMap(t, out)
	if mustString(t, m, "model") != "gpt-5.3" {
		t.Fatalf("model = %v，期望 res.Model=gpt-5.3", m["model"])
	}
	// instructions 顶层（4.2.2）：core.Instructions + system 消息并入。
	if got := mustString(t, m, "instructions"); got != "主指令\n系统提示" {
		t.Fatalf("instructions = %q，期望 %q", got, "主指令\n系统提示")
	}
	// prompt_cache_key / previous_response_id 始终透传（3.5）。
	if mustString(t, m, "prompt_cache_key") != "pk-1" {
		t.Fatalf("prompt_cache_key 未透传: %v", m["prompt_cache_key"])
	}
	if mustString(t, m, "previous_response_id") != "resp_prev" {
		t.Fatalf("previous_response_id 未透传: %v", m["previous_response_id"])
	}
	if mustNumber(t, m, "max_output_tokens") != 1024 {
		t.Fatalf("max_output_tokens = %v，期望 1024", m["max_output_tokens"])
	}
	// reasoning 对象（4.3.1：context / effort / mode / summary）。
	if mustString(t, m, "reasoning", "effort") != "medium" || mustString(t, m, "reasoning", "summary") != "auto" {
		t.Fatalf("reasoning 对象不对: %v", m["reasoning"])
	}
	// input[] 异构 item（4.2.3：一条 assistant message ⇒ reasoning + message + function_call 多 item）。
	items := mustSlice(t, m, "input")
	wantTypes := []string{"message", "reasoning", "message", "function_call", "function_call_output"}
	if len(items) != len(wantTypes) {
		t.Fatalf("input item 数 = %d，期望 %d：%v", len(items), len(wantTypes), items)
	}
	for i, wt := range wantTypes {
		it := items[i].(map[string]any)
		if it["type"] != wt {
			t.Fatalf("input[%d].type = %v，期望 %s（⛔ 不假设 output[0]/input[0] 同形）", i, it["type"], wt)
		}
	}
	// user 消息 part 用 input_text（4.3.1 item 内部结构）。
	u := items[0].(map[string]any)
	if u["role"] != "user" {
		t.Fatalf("input[0].role = %v", u["role"])
	}
	up := u["content"].([]any)[0].(map[string]any)
	if up["type"] != "input_text" || up["text"] != "你好" {
		t.Fatalf("input[0].content[0] = %v，期望 input_text/你好", up)
	}
	// reasoning item：encrypted_content ⛔ 必须保留（4.2.5）。
	rs := items[1].(map[string]any)
	if rs["encrypted_content"] != "enc-1" {
		t.Fatalf("reasoning item 丢 encrypted_content: %v", rs)
	}
	// function_call_output（工具结果是独立 item，4.2.2）。
	fo := items[4].(map[string]any)
	if fo["call_id"] != "call_1" || fo["output"] != "结果" {
		t.Fatalf("function_call_output = %v", fo)
	}
	// tools / tool_choice。
	t0 := mustSlice(t, m, "tools")[0].(map[string]any)
	if t0["name"] != "get_weather" || t0["type"] != "function" {
		t.Fatalf("tools[0] = %v", t0)
	}
	tc := mustAt(t, m, "tool_choice").(map[string]any)
	if tc["type"] != "function" || tc["name"] != "get_weather" {
		t.Fatalf("tool_choice = %v，期望 {type:function,name:get_weather}", tc)
	}
	// AuthScheme（8.4）：Authorization + "Bearer "。
	sch := a.AuthScheme()
	if sch.HeaderName != "Authorization" || sch.Prefix != "Bearer " {
		t.Fatalf("AuthScheme = %+v，期望 Authorization/Bearer ", sch)
	}
	if !a.SupportsPreviousResponseID() {
		t.Fatalf("OpenAI 支持 previous_response_id（3.5 表）")
	}
}

// TestOpenAIBuildRequestToolCallItemID 钉住 function_call item 的 id 形
// （2026-11-18 修复回归锁）：chat 侧 tool_call id 是 call_ 形 tool_call_id ⇒ item 省略
// id 键（OpenAI 严格校验逐字 400："Invalid 'input[1].id': … Expected an ID that begins
// with 'fc'"；无 id 的 item 实测 200 收）；tc.ID 为 fc_ 形（Responses 入口回放的真条目
// id）⇒ 原样回填；call_id 恒为调用 id。
// 错误实现会红：call_ 形照填 item.id ⇒ id 键存在断言红；fc_ 形被吞 ⇒ 回填断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOpenAIBuildRequestToolCallItemID -v
func TestOpenAIBuildRequestToolCallItemID(t *testing.T) {
	a := NewOpenAI(http.DefaultClient)
	res := newOpenAIResolution("https://api.openai.com/v1")
	build := func(id string) map[string]any {
		r := &core.Request{
			Messages: []core.Message{{
				Role:      "assistant",
				ToolCalls: []core.ToolCall{{ID: id, CallID: "call_real", Name: "lookup_weather", Arguments: `{"city":"北京"}`}},
			}},
			Tools: []core.Tool{{Type: "function", Name: "lookup_weather"}},
		}
		out, err := a.BuildRequest(context.Background(), r, res)
		if err != nil {
			t.Fatalf("BuildRequest 报错: %v", err)
		}
		items := mustSlice(t, bodyMap(t, out), "input")
		if len(items) != 1 {
			t.Fatalf("input item 数 = %d，期望 1（纯 tool_calls assistant 只出 function_call item）：%v", len(items), items)
		}
		return items[0].(map[string]any)
	}
	// call_ 形（chat 回放）⇒ 无 id 键、call_id 保留调用 id。
	it := build("call_abc")
	if _, has := it["id"]; has {
		t.Fatalf("call_ 形 id 不得进 item.id（OpenAI 400：Expected an ID that begins with 'fc'）：%v", it)
	}
	if it["type"] != "function_call" || it["call_id"] != "call_real" {
		t.Fatalf("function_call item = %v，期望 type=function_call / call_id=call_real", it)
	}
	// fc_ 形（真条目 id 回放）⇒ 原样回填。
	it2 := build("fc_1234")
	if it2["id"] != "fc_1234" {
		t.Fatalf("fc_ 形条目 id 应原样回填：%v", it2)
	}
}

// TestOpenAIParseResponseOutputUnion 钉住 output[] 异构联合解析（4.3.1）：
// output[0] 是 reasoning（⛔ 不假设 output[0] 是 message）、encrypted_content 保留、
// output_text 便捷字段、finish_reason/status 推导。
// 错误实现会红：只取 output[0] 当 message ⇒ Text 断言红；丢 encrypted_content ⇒ 该项断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOpenAIParseResponseOutputUnion -v
func TestOpenAIParseResponseOutputUnion(t *testing.T) {
	a := NewOpenAI(http.DefaultClient)
	out, err := a.ParseResponse(newOpenAIResolution(""), loadFixture(t, "openai_responses_response.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if out.Text != "答案是 42" {
		t.Fatalf("Text = %q，期望 %q", out.Text, "答案是 42")
	}
	if len(out.OutputItems) != 2 || out.OutputItems[0].Type != "reasoning" {
		t.Fatalf("OutputItems 顺序/类型不对: %+v（⛔ 不假设 output[0] 是 message）", out.OutputItems)
	}
	if out.OutputItems[0].EncryptedContent != "gAAAA-enc-blob" {
		t.Fatalf("encrypted_content 丢失: %+v（4.2.5 ⛔ 必须保留）", out.OutputItems[0])
	}
	if len(out.OutputItems[0].Summary) != 1 || out.OutputItems[0].Summary[0].Text != "先判断题型" {
		t.Fatalf("reasoning summary 丢失: %+v", out.OutputItems[0])
	}
	if out.OutputItems[1].Type != "message" || out.OutputItems[1].Content[0].Text != "答案是 42" {
		t.Fatalf("message item 解析不对: %+v", out.OutputItems[1])
	}
	if out.FinishReason != core.FinishStop {
		t.Fatalf("FinishReason = %q，期望 stop（status=completed 推导）", out.FinishReason)
	}
	if out.Status != core.StatusCompleted {
		t.Fatalf("Status = %q", out.Status)
	}
}

// TestOpenAIParseUsageChatTree 钉住 OpenAI Chat usage 树（4.3.1 逐字样本 125/48/173/98）：
// InputTokens=125、CacheRead=98（⛔ 在 prompt_tokens_details 二级，不在顶层）、
// CacheWrite=0、Output=48、Uncached=125−98−0=27。
// 错误实现会红：把 cached_tokens 当顶层字段取 ⇒ CacheRead=0 ⇒ 红；Uncached 直接取 prompt ⇒ 125 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOpenAIParseUsageChatTree -v
func TestOpenAIParseUsageChatTree(t *testing.T) {
	a := NewOpenAI(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "openai_chat_usage.json"))
	if u.InputTokens != 125 || u.CacheReadTokens != 98 || u.CacheWriteTokens != 0 || u.OutputTokens != 48 {
		t.Fatalf("归一不对：In=%d(125) Read=%d(98) Write=%d(0) Out=%d(48)",
			u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens)
	}
	if u.InputTokensUncached != 27 {
		t.Fatalf("Uncached = %d，期望 27 = 125−98−0（4.4.3 R1 公式）", u.InputTokensUncached)
	}
	if u.TotalTokens != 173 {
		t.Fatalf("TotalTokens = %d，期望 173（原报保留 ⛔ 不重算）", u.TotalTokens)
	}
}

// TestOpenAIParseUsageResponsesTree 钉住 OpenAI Responses usage 树（4.3.1 字段表构造，
// 见 testdata/README.md）：input_tokens 总输入、cache_write_tokens TTL 细分 5+3=8、
// Uncached=200−20−8=172。
// 错误实现会红：TTL 细分只取一档 ⇒ Write=5 ⇒ 红；两套 usage 树混用字段名 ⇒ In=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOpenAIParseUsageResponsesTree -v
func TestOpenAIParseUsageResponsesTree(t *testing.T) {
	a := NewOpenAI(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "openai_responses_usage.json"))
	if u.InputTokens != 200 || u.CacheReadTokens != 20 || u.CacheWriteTokens != 8 || u.OutputTokens != 50 {
		t.Fatalf("归一不对：In=%d(200) Read=%d(20) Write=%d(8=5+3) Out=%d(50)",
			u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens)
	}
	if u.InputTokensUncached != 172 {
		t.Fatalf("Uncached = %d，期望 172 = 200−20−8", u.InputTokensUncached)
	}
	if u.ReasoningTokens != 10 {
		t.Fatalf("ReasoningTokens = %d，期望 10（output_tokens_details.reasoning_tokens）", u.ReasoningTokens)
	}
}

// TestOpenAIStreamTolerance 钉住 Responses 流式解析器容忍性（4.3.1 解析器容忍性要求）：
// ① 缺 response.output_item.added 直接收 delta ⇒ 容忍（工具 id 退回 item_id）；
// ② 未知事件类型 ⇒ 跳过不中断（事件总数 59 种，⛔ 不假设全认识）；
// ③ usage 由 response.completed 自带（⛔ 不需要 stream_options.include_usage）；
// ④ [DONE] 可有可无。
// 错误实现会红：缺 output_item.added 时报错 ⇒ collectFrames err 非 nil ⇒ 红；未知事件中断 ⇒ 帧数不齐 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOpenAIStreamTolerance -v
func TestOpenAIStreamTolerance(t *testing.T) {
	const sse = "" +
		"event: response.created\n" +
		"data: {\"type\":\"response.created\",\"sequence_number\":0}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"你好\"}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":2,\"item_id\":\"fc_9\",\"partial_json\":\"{\\\"a\\\":1}\"}\n\n" +
		"event: response.web_search_call.in_progress\n" +
		"data: {\"type\":\"response.web_search_call.in_progress\",\"sequence_number\":3}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"usage\":{\"input_tokens\":200,\"output_tokens\":50,\"total_tokens\":250}}}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewOpenAI(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newOpenAIResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v（缺 output_item.added / 未知事件必须容忍）", cerr)
	}
	if len(frames) != 5 {
		t.Fatalf("帧数 = %d，期望 5：%+v", len(frames), frames)
	}
	if frames[0].Kind != stream.FrameLifecycle || frames[0].Status != "response.created" {
		t.Fatalf("帧[0] = %+v，期望 lifecycle response.created", frames[0])
	}
	if frames[1].Kind != stream.FrameTextDelta || frames[1].Text != "你好" {
		t.Fatalf("帧[1] = %+v，期望 text 你好", frames[1])
	}
	if frames[2].Kind != stream.FrameToolCallArgsDelta || frames[2].ToolCallID != "fc_9" {
		t.Fatalf("帧[2] = %+v，期望工具帧 ToolCallID=fc_9（缺 output_item.added ⇒ 退回 item_id）", frames[2])
	}
	if frames[3].Kind != stream.FrameUsage || frames[3].Usage.InputTokens != 200 {
		t.Fatalf("帧[3] = %+v，期望 usage（response.completed 自带）", frames[3])
	}
	if frames[4].Kind != stream.FrameDone || frames[4].Status != core.StatusCompleted {
		t.Fatalf("帧[4] = %+v，期望 done completed", frames[4])
	}
}

// TestOpenAIStreamToolCallName 钉住流式工具调用名进帧（帧承载约定：FrameToolCallArgsDelta.Text
// 载函数名、ToolCallID 载调用 id）：output_item.added 的 item.name 必须以补名帧
// （ToolCallID=call_abc、Text=search）进入帧序列，参数帧照常到（ToolCallID 一致、Arguments 完整）。
// 错误实现会红：不发补名帧 ⇒ 帧序列里只有 Text 为空的参数帧 ⇒ named=0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOpenAIStreamToolCallName -v
func TestOpenAIStreamToolCallName(t *testing.T) {
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

	a := NewOpenAI(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newOpenAIResolution(srv.URL))
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

// TestOpenAIStreamToolCallRealShape 钉住真实上游形态的流式工具调用解析（2026-11-18 修复回归锁）：
// fixture 逐字取自 MiMo 真实 Responses 流抓取（api.xiaomimimo.com/v1/responses，mimo-v2.6-pro，
// 2026-11-18 实测，含 data: 无空格 SSE 形态）——两个曾被想象 fixture 掩盖的形态：
// ① output_item.added 顶层无 item_id（id 在 item.id）；
// ② function_call_arguments.delta 的参数增量字段是 delta（非 partial_json）。
// 断言：补名帧 Text=lookup_weather、ToolCallID=call_…（call_id 映射，⛔ 不回退 fc_ item_id）、
// 参数帧 Arguments 逐字拼回 {"city": "北京"}、未知事件（arguments.done/output_item.done）容忍。
// 错误实现会红：旧实现不发补名帧（named 断言红）、args 恒空（拼回断言红）、
// id 回退 item_id（ToolCallID 前缀断言红）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOpenAIStreamToolCallRealShape -v
func TestOpenAIStreamToolCallRealShape(t *testing.T) {
	const sse = `event:response.created
data:{"type":"response.created","sequence_number":0,"response":{"id":"resp_ab08","object":"response","status":"in_progress","output":[]}}

event:response.output_item.added
data:{"type":"response.output_item.added","sequence_number":7,"output_index":1,"item":{"id":"fc_710d5c1581ed43aaaffa4d743be6192b","type":"function_call","call_id":"call_3e3e508571474be2a86405bd","name":"lookup_weather","arguments":"","status":"in_progress"}}

event:response.function_call_arguments.delta
data:{"type":"response.function_call_arguments.delta","sequence_number":8,"item_id":"fc_710d5c1581ed43aaaffa4d743be6192b","output_index":1,"delta":"{\"city\": "}

event:response.function_call_arguments.delta
data:{"type":"response.function_call_arguments.delta","sequence_number":9,"item_id":"fc_710d5c1581ed43aaaffa4d743be6192b","output_index":1,"delta":"\"北京\"}"}

event:response.function_call_arguments.done
data:{"type":"response.function_call_arguments.done","sequence_number":15,"item_id":"fc_710d5c1581ed43aaaffa4d743be6192b","arguments":"{\"city\": \"北京\"}"}

event:response.output_item.done
data:{"type":"response.output_item.done","sequence_number":16,"output_index":1,"item":{"id":"fc_710d5c1581ed43aaaffa4d743be6192b","type":"function_call","call_id":"call_3e3e508571474be2a86405bd","name":"lookup_weather","arguments":"{\"city\": \"北京\"}","status":"completed"}}

event:response.completed
data:{"type":"response.completed","sequence_number":17,"response":{"usage":{"input_tokens":84,"output_tokens":32,"total_tokens":116}}}

data: [DONE]
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	a := NewOpenAI(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newOpenAIResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	const callID = "call_3e3e508571474be2a86405bd"
	named := 0
	argsTotal := ""
	for _, f := range frames {
		if f.Kind == stream.FrameToolCallArgsDelta {
			if strings.HasPrefix(f.ToolCallID, "fc_") {
				t.Fatalf("ToolCallID 回退 item_id（fc_ 前缀），期望 call_id 映射 %s：%+v", callID, f)
			}
			if f.ToolCallID != callID {
				t.Fatalf("ToolCallID = %s，期望 %s：%+v", f.ToolCallID, callID, f)
			}
			if f.Text == "lookup_weather" {
				named++
			}
			argsTotal += f.Arguments
		}
	}
	if named != 1 {
		t.Fatalf("补名帧（Text=lookup_weather）= %d，期望 1（output_item.added 的 item.name 必须进帧）：%+v", named, frames)
	}
	if argsTotal != "{\"city\": \"北京\"}" {
		t.Fatalf("参数拼回 = %q，期望 %q（delta 字段增量必须逐字进帧）", argsTotal, "{\"city\": \"北京\"}")
	}
	done := 0
	for _, f := range frames {
		if f.Kind == stream.FrameDone && f.Status == core.StatusCompleted {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("done completed 帧 = %d，期望 1（未知事件容忍、completed 正常收口）：%+v", done, frames)
	}
}

// TestOpenAIParseUsageRealSample 钉住真实样本（litellm 真实录制，来源见 testdata/real/README.md）上
// ParseUsage 按实际 usage 树选口径成立：Responses 树（input_tokens 309/118/53/427）与
// Chat 树（Azure 部署捕获 19/5/24）各归各位；Chat 树的未知字段 latency_checkpoint ⛔ 不干扰解析。
// 错误实现会红：两树混读（拿 cached_tokens 去 Responses 树）⇒ 数值全 0 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestOpenAIParseUsageRealSample -v
func TestOpenAIParseUsageRealSample(t *testing.T) {
	a := NewOpenAI(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/openai_responses_usage.json"))
	if u.InputTokens != 309 || u.InputTokensUncached != 309 || u.OutputTokens != 118 || u.ReasoningTokens != 53 || u.TotalTokens != 427 {
		t.Fatalf("Responses 归一 = In%d/Unc%d/Out%d/Reason%d/Total%d，期望 309/309/118/53/427（样本逐字）",
			u.InputTokens, u.InputTokensUncached, u.OutputTokens, u.ReasoningTokens, u.TotalTokens)
	}
	u = a.ParseUsage(loadFixture(t, "real/openai_chat_usage_azure.json"))
	if u.InputTokens != 19 || u.InputTokensUncached != 19 || u.OutputTokens != 5 || u.TotalTokens != 24 {
		t.Fatalf("Chat 归一 = In%d/Unc%d/Out%d/Total%d，期望 19/19/5/24（样本逐字）",
			u.InputTokens, u.InputTokensUncached, u.OutputTokens, u.TotalTokens)
	}
}
