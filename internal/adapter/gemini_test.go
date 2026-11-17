// 日期: 2026-11-14 14:58:31.569
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

// 本文件覆盖 Gemini 适配器（统一 SDD 4.3.3）：
// BuildRequest（模型名补 models/ 前缀、x-goog-api-key、驼峰字段）、
// ParseResponse（finishReason 21 值映射）、ParseUsage（usageMetadata 驼峰 + thoughts 独立加项）、
// 流式（流结束无 finishReason ⇒ 按错误处理，⛔ 不冒充正常完成）。

func newGeminiResolution(base string) *core.Resolution {
	return &core.Resolution{
		Provider: core.ProviderGemini,
		Shape:    core.APIShapeGeminiNative,
		BaseURL:  base,
		Model:    "gemini-2.5-pro",
	}
}

// TestGeminiBuildRequestShape 钉住 Gemini 原生请求形状（4.3.3）：
// ① 路径参数正则 ^models/[^/]+$ ⇒ 模型名必须带 models/ 前缀（网关自己补）；
// ② AuthScheme x-goog-api-key；
// ③ 请求体驼峰（contents / generationConfig / systemInstruction，⛔ 与 OpenAI 蛇形不通用）。
// 错误实现会红：不补前缀 ⇒ URL 断言红；已带前缀被重复补 ⇒ 双前缀断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestGeminiBuildRequestShape -v
func TestGeminiBuildRequestShape(t *testing.T) {
	a := NewGemini(http.DefaultClient)
	temp := 0.5
	r := &core.Request{
		Messages: []core.Message{
			{Role: "system", Content: []core.ContentPart{{Text: "系统提示"}}},
			{Role: "user", Content: []core.ContentPart{{Text: "你好"}}},
			{Role: "assistant", Content: []core.ContentPart{{Text: "在的"}}},
		},
		Temperature:     &temp,
		MaxOutputTokens: 512,
	}
	out, err := a.BuildRequest(context.Background(), r, newGeminiResolution("https://generativelanguage.googleapis.com/v1beta"))
	if err != nil {
		t.Fatalf("BuildRequest 报错: %v", err)
	}
	// ① models/ 前缀（4.3.3）。
	wantURL := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:generateContent"
	if out.URL != wantURL {
		t.Fatalf("URL = %s，期望 %s", out.URL, wantURL)
	}
	// 已带 models/ 前缀 ⇒ ⛔ 不重复补。
	res := newGeminiResolution("https://generativelanguage.googleapis.com/v1beta")
	res.Model = "models/gemini-2.5-pro"
	out2, err := a.BuildRequest(context.Background(), r, res)
	if err != nil {
		t.Fatalf("BuildRequest(带前缀) 报错: %v", err)
	}
	if out2.URL != wantURL {
		t.Fatalf("URL = %s，期望不重复补前缀 %s", out2.URL, wantURL)
	}

	m := bodyMap(t, out)
	// ③ 驼峰字段（4.3.3）。
	sys := mustSlice(t, m, "systemInstruction", "parts")[0].(map[string]any)
	if sys["text"] != "系统提示" {
		t.Fatalf("systemInstruction = %v，期望系统提示", m["systemInstruction"])
	}
	contents := mustSlice(t, m, "contents")
	if len(contents) != 2 {
		t.Fatalf("contents 数 = %d，期望 2（system 不入 contents）", len(contents))
	}
	if contents[0].(map[string]any)["role"] != "user" || contents[1].(map[string]any)["role"] != "model" {
		t.Fatalf("contents 角色不对: %v（user ⇒ user / assistant ⇒ model）", contents)
	}
	if mustNumber(t, m, "generationConfig", "maxOutputTokens") != 512 {
		t.Fatalf("generationConfig.maxOutputTokens = %v，期望 512（驼峰 ⛔ 非 max_output_tokens）", m["generationConfig"])
	}
	if mustNumber(t, m, "generationConfig", "temperature") != 0.5 {
		t.Fatalf("generationConfig.temperature = %v", m["generationConfig"])
	}
	// ② AuthScheme（4.3.3）。
	sch := a.AuthScheme()
	if sch.HeaderName != "x-goog-api-key" || sch.Prefix != "" {
		t.Fatalf("AuthScheme = %+v，期望 x-goog-api-key", sch)
	}
}

// TestGeminiPreviousResponseIDRejected 钉住 3.5：Gemini 不支持服务端会话 ⇒ 显式报错。
// 错误实现会红：静默丢弃 ⇒ err==nil ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestGeminiPreviousResponseIDRejected -v
func TestGeminiPreviousResponseIDRejected(t *testing.T) {
	a := NewGemini(http.DefaultClient)
	_, err := a.BuildRequest(context.Background(),
		&core.Request{PreviousResponseID: "resp_prev"}, newGeminiResolution("https://generativelanguage.googleapis.com/v1beta"))
	requireDetailErr(t, err, "unsupported_capability")
}

// TestGeminiParseResponseFinishMapping 钉住 finishReason 21 值映射（4.5.1 归一化映射规则）：
// STOP ⇒ stop；MAX_TOKENS ⇒ length（超上下文/length 类）+ incomplete；SAFETY ⇒ content_filter；
// 其余取值（CONTINUATION 等）⛔ 不得当作未知值丢弃 ⇒ 超集原值。
// 错误实现会红：未知值被丢成 "" ⇒ 原值断言红；MAX_TOKENS 当正常完成 ⇒ 状态断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestGeminiParseResponseFinishMapping -v
func TestGeminiParseResponseFinishMapping(t *testing.T) {
	a := NewGemini(http.DefaultClient)
	// fixture：finishReason=STOP。
	out, err := a.ParseResponse(newGeminiResolution(""), loadFixture(t, "gemini_response.json"))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	if out.FinishReason != core.FinishStop || out.Status != core.StatusCompleted {
		t.Fatalf("STOP 映射不对: %q / %q", out.FinishReason, out.Status)
	}
	if out.Text != "你好，我是 Gemini" {
		t.Fatalf("Text = %q（多个 parts 拼接）", out.Text)
	}
	cases := []struct {
		body, wantFinish, wantStatus string
	}{
		{`{"candidates":[{"content":{"parts":[{"text":"截断"}]},"finishReason":"MAX_TOKENS"}]}`,
			core.FinishLength, core.StatusIncomplete},
		{`{"candidates":[{"content":{"parts":[{"text":"拒绝"}]},"finishReason":"SAFETY"}]}`,
			core.FinishContentFilter, core.StatusCompleted},
		{`{"candidates":[{"content":{"parts":[{"text":"续"}]},"finishReason":"CONTINUATION"}]}`,
			"CONTINUATION", core.StatusCompleted}, // 非标准取值 ⇒ 超集原值保留（4.5.1）
	}
	for _, c := range cases {
		out, err := a.ParseResponse(newGeminiResolution(""), []byte(c.body))
		if err != nil {
			t.Fatalf("ParseResponse(%s) 报错: %v", c.body, err)
		}
		if out.FinishReason != c.wantFinish || out.Status != c.wantStatus {
			t.Fatalf("body=%s ⇒ %q/%q，期望 %q/%q", c.body, out.FinishReason, out.Status, c.wantFinish, c.wantStatus)
		}
	}
}

// TestGeminiParseUsageThoughts 钉住 usageMetadata 归一（4.3.3 / 4.4.2 Gemini 行）：
// InputTokens = promptTokenCount + toolUsePromptTokenCount = 120；
// OutputTokens = candidatesTokenCount + thoughtsTokenCount = 40（thoughts 独立加项）；
// 缓存读 = cachedContentTokenCount = 40、缓存写 = 0（显式缓存按存储费计，⛔ 不按写入 token 计）；
// totalTokenCount 原报保留 ⛔ 不重算。
// 错误实现会红：输出只取 candidates ⇒ 30 ⇒ 红；thoughts 加两次 ⇒ 50 ⇒ 红；缓存写记 cache 创建 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestGeminiParseUsageThoughts -v
func TestGeminiParseUsageThoughts(t *testing.T) {
	a := NewGemini(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "gemini_usage.json"))
	if u.InputTokens != 120 || u.OutputTokens != 40 {
		t.Fatalf("归一不对：In=%d(120=100+20) Out=%d(40=30+10)", u.InputTokens, u.OutputTokens)
	}
	if u.CacheReadTokens != 40 || u.CacheWriteTokens != 0 {
		t.Fatalf("缓存不对：Read=%d(40) Write=%d(0，⛔ 显式缓存按存储费计)", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.InputTokensUncached != 80 {
		t.Fatalf("Uncached = %d，期望 80 = 120−40", u.InputTokensUncached)
	}
	if u.TotalTokens != 160 || u.ToolUsePromptTokens != 20 {
		t.Fatalf("原报保留不对：Total=%d(160 ⛔ 不重算) ToolUse=%d(20)", u.TotalTokens, u.ToolUsePromptTokens)
	}
}

// TestGeminiStreamNoFinishReasonIsError 钉住流中断判据（3.7 / 4.3.3）：
// 流结束而无 finishReason ⇒ Err() 非 nil（⛔ 不冒充正常完成）；有 finishReason ⇒ Err()==nil。
// 错误实现会红：无 finishReason 冒充正常完成 ⇒ Err()==nil ⇒ 红（恒假样本已核，见变异自检）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestGeminiStreamNoFinishReasonIsError -v
func TestGeminiStreamNoFinishReasonIsError(t *testing.T) {
	// ① 中断流：两个增量帧，无 finishReason、无 [DONE]。
	const cut = "" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"你\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"好\"}]}}]}\n\n"
	// ② 正常流：终结帧带 finishReason。
	const ok = "" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"你\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"好\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":2,\"totalTokenCount\":7}}\n\n"
	for _, c := range []struct {
		name, body string
		wantErr    bool
	}{
		{"中断无finishReason", cut, true},
		{"正常终结", ok, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(c.body))
		}))
		a := NewGemini(srv.Client())
		it, err := a.StreamStart(context.Background(), &core.Request{}, newGeminiResolution(srv.URL))
		if err != nil {
			srv.Close()
			t.Fatalf("[%s] StreamStart 报错: %v", c.name, err)
		}
		frames, cerr := collectFrames(t, it)
		srv.Close()
		if c.wantErr {
			if cerr == nil {
				t.Fatalf("[%s] 期望 Err() 非 nil（流结束无 finishReason ⇒ 流中断），实际 nil，frames=%+v", c.name, frames)
			}
			requireDetailErr(t, cerr, "gemini_stream_no_finish_reason")
		} else {
			if cerr != nil {
				t.Fatalf("[%s] 期望正常结束，实际 Err()=%v", c.name, cerr)
			}
			last := frames[len(frames)-1]
			if last.Kind != stream.FrameDone || last.Status != core.FinishStop {
				t.Fatalf("[%s] 末帧 = %+v，期望 done/stop", c.name, last)
			}
		}
	}
}

// TestGeminiStreamToolCallSyntheticID 钉住流式工具调用两件事（2026-11-17 契约变更）：
// ① 帧承载约定不变：FrameToolCallArgsDelta.Text 载函数名（functionCall 名字与参数同包）；
// ② ToolCallID 用合成 call id —— 同轮两个同名调用必须拿到不同 id（⛔ 不再沿用函数名）。
// 错误实现会红：ToolCallID=函数名（旧口径）⇒ 同名双调用 id 相同 ⇒ distinct 断言红；
// 帧不带 Text ⇒ Text=search 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestGeminiStreamToolCall -v
func TestGeminiStreamToolCallSyntheticID(t *testing.T) {
	const body = "" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"search\",\"args\":{\"q\":1}}},{\"functionCall\":{\"name\":\"search\",\"args\":{\"q\":2}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	a := NewGemini(srv.Client())
	it, err := a.StreamStart(context.Background(), &core.Request{}, newGeminiResolution(srv.URL))
	if err != nil {
		t.Fatalf("StreamStart 报错: %v", err)
	}
	frames, cerr := collectFrames(t, it)
	if cerr != nil {
		t.Fatalf("流式终态错误: %v", cerr)
	}
	var ids []string
	for _, f := range frames {
		if f.Kind != stream.FrameToolCallArgsDelta {
			continue
		}
		if f.Text != "search" {
			t.Fatalf("工具帧 Text 应载函数名 search：%+v", f)
		}
		if f.ToolCallID == "" || f.ToolCallID == "search" {
			t.Fatalf("工具帧 ToolCallID 应为合成 id（非空且非函数名）：%+v", f)
		}
		ids = append(ids, f.ToolCallID)
	}
	if len(ids) != 2 {
		t.Fatalf("工具帧数 = %d，期望 2：%+v", len(ids), frames)
	}
	if ids[0] == ids[1] {
		t.Fatalf("同轮同名两调用 ToolCallID 相同 = %s（撞 id，下游配对会串）", ids[0])
	}
}

// TestGeminiParseResponseSyntheticCallIDs 钉住非流式 function_call 的合成 call id：
// 同轮两个同名调用 ⇒ 各自非空且互不相同，ID==CallID 同值（Gemini 无 id/call_id 双字段语义）。
// 错误实现会红：旧实现不置 ID/CallID（全空）⇒ 非空断言红；沿用函数名 ⇒ 撞 id 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestGeminiParseResponseSyntheticCallIDs -v
func TestGeminiParseResponseSyntheticCallIDs(t *testing.T) {
	const body = `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"search","args":{"q":1}}},{"functionCall":{"name":"search","args":{"q":2}}}]},"finishReason":"STOP"}]}`
	a := NewGemini(http.DefaultClient)
	resp, err := a.ParseResponse(newGeminiResolution("https://generativelanguage.googleapis.com/v1beta"), []byte(body))
	if err != nil {
		t.Fatalf("ParseResponse 报错: %v", err)
	}
	var ids []string
	for _, item := range resp.OutputItems {
		if item.Type != "function_call" {
			continue
		}
		if item.ID == "" || item.CallID == "" {
			t.Fatalf("function_call item 应带合成 id（ID/CallID 非空）：%+v", item)
		}
		if item.ID != item.CallID {
			t.Fatalf("Gemini 无双字段语义 ⇒ ID 应 == CallID：%+v", item)
		}
		if item.ID == item.Name {
			t.Fatalf("id 不得沿用函数名（同名调用会撞）：%+v", item)
		}
		ids = append(ids, item.ID)
	}
	if len(ids) != 2 {
		t.Fatalf("function_call item 数 = %d，期望 2", len(ids))
	}
	if ids[0] == ids[1] {
		t.Fatalf("同轮同名两调用 id 相同 = %s", ids[0])
	}
}

// TestGeminiToolResultNameFromHistory 钉住回程 FunctionResponse.Name 的历史映射：
// role=tool 消息只带合成 call id 时，函数名必须从同请求历史 assistant.tool_calls 还原。
// 错误实现会红：旧实现 name=ToolCallID（call_xxx）⇒ Name 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestGeminiToolResult -v
func TestGeminiToolResultNameFromHistory(t *testing.T) {
	r := &core.Request{Messages: []core.Message{
		{Role: "user", Content: []core.ContentPart{{Text: "查一下"}}},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_a1", CallID: "call_a1", Name: "search", Arguments: `{"q":1}`, Type: "function"}}},
		{Role: "tool", ToolCallID: "call_a1", Content: []core.ContentPart{{Text: "结果"}}},
	}}
	_, contents, _, err := geminiSDKParams(r)
	if err != nil {
		t.Fatalf("geminiSDKParams 报错: %v", err)
	}
	found := false
	for _, c := range contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				found = true
				if p.FunctionResponse.Name != "search" {
					t.Fatalf("FunctionResponse.Name = %q，期望 search（合成 id ⇒ 历史映射还原函数名）", p.FunctionResponse.Name)
				}
			}
		}
	}
	if !found {
		t.Fatalf("未生成 FunctionResponse：%+v", contents)
	}
}

// TestGeminiToolResultLegacyNameFallback 钉住旧口径兜底：ToolCallID 直接承载函数名
// （合成 id 之前的历史形态）时仍按其作函数名，⛔ 不因映射表缺失而专名。
// 错误实现会红：兜底缺失 ⇒ Name 变空 ⇒ 断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestGeminiToolResultLegacy -v
func TestGeminiToolResultLegacyNameFallback(t *testing.T) {
	r := &core.Request{Messages: []core.Message{
		{Role: "tool", ToolCallID: "search", Content: []core.ContentPart{{Text: "结果"}}},
	}}
	_, contents, _, err := geminiSDKParams(r)
	if err != nil {
		t.Fatalf("geminiSDKParams 报错: %v", err)
	}
	p := contents[0].Parts[0]
	if p.FunctionResponse == nil || p.FunctionResponse.Name != "search" {
		t.Fatalf("旧口径兜底失效：FunctionResponse = %+v，期望 Name=search", p.FunctionResponse)
	}
}

// TestGeminiParseUsageRealSample 钉住真实样本（litellm 真实录制 usageMetadata，responseId
// 1CbQat_JEK-jqtsPtf-QoQU，来源见 testdata/real/README.md）上 ParseUsage 归一成立：
// Out = candidates 80 + thoughts 575 = 655（thoughts 独立加项）、Total=1013 保留原报 ⛔ 不重算；
// 真实样本的 promptTokensDetails（modality 数组）⛔ 不干扰解析。
// 错误实现会红：不加 thoughts ⇒ 80 ≠ 655 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestGeminiParseUsageRealSample -v
func TestGeminiParseUsageRealSample(t *testing.T) {
	a := NewGemini(http.DefaultClient)
	u := a.ParseUsage(loadFixture(t, "real/gemini_usage_metadata.json"))
	if u.InputTokens != 358 || u.OutputTokens != 655 || u.TotalTokens != 1013 || u.CacheReadTokens != 0 {
		t.Fatalf("归一 = In%d/Out%d/Total%d/Read%d，期望 358/655/1013/0（Out=80+575，样本逐字）",
			u.InputTokens, u.OutputTokens, u.TotalTokens, u.CacheReadTokens)
	}
}
