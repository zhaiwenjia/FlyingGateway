// 日期: 2026-11-14 15:07:19.509
// 作者: Zhai Wenjia

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/telemetry"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/policy"
)

// 本文件是请求管线的功能测试（统一 SDD 12.1 A2 / A11 / A12 / 12.2 D2 / D3 / D5 / 11.4）：
// 双入口非流式与流式、/v1/models、404、会话态、prompt_cache_key、非法请求、
// 治理三件套、egress、上下文、重试与降级、鉴权三形态注入。

const chatHelloBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`

// dataLine 取 SSE 块里的 data: JSON。
func dataLine(t *testing.T, block string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(block, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err == nil {
			return m
		}
	}
	return nil
}

// TestChatCompletionsNonStream 断言 Chat 入口非流式跑通（A2）：内容、usage、finish_reason。
func TestChatCompletionsNonStream(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（原文 %s）", resp.StatusCode, readRaw(t, resp))
	}
	m := readBody(t, resp)
	if m["object"] != "chat.completion" {
		t.Fatalf("object = %v，期望 chat.completion", m["object"])
	}
	choices, _ := m["choices"].([]any)
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	if msg["content"] != "Hello, world!" {
		t.Errorf("message.content = %v，期望 Hello, world!", msg["content"])
	}
	if fr, _ := c0["finish_reason"].(string); fr == "" {
		t.Errorf("finish_reason 为空，期望非空终止原因")
	}
	usage, _ := m["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(12) || usage["completion_tokens"] != float64(7) || usage["total_tokens"] != float64(19) {
		t.Errorf("usage = %v，期望 prompt=12 completion=7 total=19（fixture 逐字）", usage)
	}
}

// TestResponsesNonStream 断言 Responses 入口非流式跑通（A2）：status / output_text / usage。
func TestResponsesNonStream(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/responses", `{"model":"gpt-4o-mini","input":"hi"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", resp.StatusCode)
	}
	m := readBody(t, resp)
	if m["status"] != "completed" {
		t.Errorf("status = %v，期望 completed", m["status"])
	}
	if m["output_text"] != "Hello, world!" {
		t.Errorf("output_text = %v，期望 Hello, world!", m["output_text"])
	}
	usage, _ := m["usage"].(map[string]any)
	if usage["input_tokens"] != float64(12) || usage["output_tokens"] != float64(7) {
		t.Errorf("usage = %v，期望 input=12 output=7（fixture 逐字）", usage)
	}
}

// TestChatStreamFrameSequence 断言流式帧序列、末帧 usage 与终止哨兵（11.4）。
func TestChatStreamFrameSequence(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}
	raw := readRaw(t, resp)
	blocks := splitBlocks(raw)

	// 帧序列：内容分片（fixture 顺序 Hello / ", world!"）→ 终止帧（finish_reason 非 null）
	// → usage 块（choices: []）→ data: [DONE] 哨兵。
	if got := countContent(blocks); got != 2 {
		t.Fatalf("内容分片数 = %d，期望 2（fixture 两段 delta）", got)
	}
	var seenText []string
	var sawFinish, sawUsage, sawUsageChunk bool
	for _, b := range blocks {
		m := dataLine(t, b)
		if m == nil {
			continue
		}
		if m["object"] != "chat.completion.chunk" {
			continue
		}
		choices, _ := m["choices"].([]any)
		if len(choices) > 0 {
			c0, _ := choices[0].(map[string]any)
			if fr, ok := c0["finish_reason"].(string); ok && fr != "" {
				sawFinish = true
			}
			delta, _ := c0["delta"].(map[string]any)
			if content, _ := delta["content"].(string); content != "" {
				seenText = append(seenText, content)
			}
		}
		if usage, ok := m["usage"].(map[string]any); ok && usage != nil {
			sawUsage = true
			if len(choices) == 0 {
				sawUsageChunk = true
			}
			if usage["prompt_tokens"] != float64(12) || usage["completion_tokens"] != float64(7) {
				t.Errorf("末帧 usage = %v，期望 prompt=12 completion=7（fixture 逐字）", usage)
			}
		}
	}
	if strings.Join(seenText, "") != "Hello, world!" {
		t.Errorf("内容序列 = %q，期望 Hello, world!（分片顺序保持）", strings.Join(seenText, ""))
	}
	if !sawFinish {
		t.Error("缺终止帧（finish_reason 非 null）")
	}
	if !sawUsage || !sawUsageChunk {
		t.Errorf("缺 usage 块（usage=%v usageChunk=%v，期望终止后独立 usage 块）", sawUsage, sawUsageChunk)
	}
	if !hasSentinel(blocks) {
		t.Error("缺终止哨兵 data: [DONE]")
	}
}

// TestResponsesStreamTerminal 断言 Responses 入口流式跑通（A2）：事件序列 + 终态 + 哨兵。
func TestResponsesStreamTerminal(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/responses", `{"model":"gpt-4o-mini","input":"hi","stream":true}`, nil)
	raw := readRaw(t, resp)
	blocks := splitBlocks(raw)
	var deltas int
	var completed bool
	for _, b := range blocks {
		if strings.Contains(b, "event: response.output_text.delta") {
			deltas++
		}
		if strings.Contains(b, "event: response.completed") {
			completed = true
			m := dataLine(t, b)
			if m != nil {
				inner, _ := m["response"].(map[string]any)
				if inner != nil && inner["status"] != "completed" {
					t.Errorf("终态 response.status = %v，期望 completed", inner["status"])
				}
			}
		}
	}
	if deltas != 2 {
		t.Errorf("output_text.delta 事件数 = %d，期望 2", deltas)
	}
	if !completed {
		t.Error("缺 response.completed 终态事件")
	}
	if !hasSentinel(blocks) {
		t.Error("缺终止哨兵 data: [DONE]")
	}
}

// TestUnsupportedCapabilityPreviousResponseID 断言会话态口径（A11）：
// previous_response_id 打到不支持上游 ⇒ 400 unsupported_capability，⛔ 不静默丢弃。
// （用 deepseek 端点：anthropic 端点有入口协议绑定在前，进不到本检查。）
func TestUnsupportedCapabilityPreviousResponseID(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec {
		ds := epSpec{
			name: "deepseek", base: base, model: "deepseek-chat", upstreamModel: "deepseek-chat",
			provider: "deepseek", credRef: "TEST_KEY_OPENAI",
			ctxWindow: 64000, maxOut: 4096, maxRetries: 0, priority: 1, timeoutS: 5,
		}
		return []epSpec{primaryEP(base), ds}
	}, nil, nil)
	e.up.setEventsFor("/responses", loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/responses",
		`{"model":"deepseek-chat","input":"hi","previous_response_id":"prev_1"}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400", resp.StatusCode)
	}
	typ, code, msg := errorFields(t, readBody(t, resp))
	if code != "unsupported_capability" {
		t.Errorf("code = %q，期望 unsupported_capability（type=%q）", code, typ)
	}
	if !strings.Contains(msg, "该上游不支持服务端会话，请改为客户端带全量历史") {
		t.Errorf("错误信息 = %q，须写明「该上游不支持服务端会话，请改为客户端带全量历史」", msg)
	}
	if n, _, _, _ := e.up.stats(); n != 0 {
		t.Errorf("上游调用数 = %d，期望 0（⛔ 不静默丢弃也不发上游）", n)
	}
}

// TestPreviousResponseIDPassthrough 断言支持上游的 previous_response_id 原样透传（3.5）。
func TestPreviousResponseIDPassthrough(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/responses",
		`{"model":"gpt-4o-mini","input":"hi","previous_response_id":"prev_9"}`, nil)
	readBody(t, resp)
	if !strings.Contains(string(e.up.lastBody()), `"previous_response_id":"prev_9"`) {
		t.Errorf("上游请求体未透传 previous_response_id：%s", e.up.lastBody())
	}
}

// TestPromptCacheKeyPassthrough 断言 prompt_cache_key 始终透传（A12 / 3.5 优化字段）。
func TestPromptCacheKeyPassthrough(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/responses",
		`{"model":"gpt-4o-mini","input":"hi","prompt_cache_key":"pck-123"}`, nil)
	readBody(t, resp)
	if !strings.Contains(string(e.up.lastBody()), `"prompt_cache_key":"pck-123"`) {
		t.Errorf("上游请求体未透传 prompt_cache_key：%s", e.up.lastBody())
	}
}

// TestInvalidBody400NoUpstream 断言非法请求 400 且 ⛔ 不发起上游请求。
func TestInvalidBody400NoUpstream(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	resp := e.postJSON("/v1/chat/completions", `{not-json`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400", resp.StatusCode)
	}
	typ, code, _ := errorFields(t, readBody(t, resp))
	if code != "invalid_request" || typ != "invalid_request" {
		t.Errorf("error = %q/%q，期望 invalid_request/invalid_request", typ, code)
	}
	if n, _, _, _ := e.up.stats(); n != 0 {
		t.Errorf("上游调用数 = %d，期望 0", n)
	}
}

// TestBudgetExceeded429 断言预算触顶 ⇒ 429 budget_exceeded（上游调用 0，7.2 / 8.3）。
func TestBudgetExceeded429(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		o.Budget = policy.NewBudget(10)
	})
	resp := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("状态 = %d，期望 429", resp.StatusCode)
	}
	_, code, _ := errorFields(t, readBody(t, resp))
	if code != "budget_exceeded" {
		t.Errorf("code = %q，期望 budget_exceeded", code)
	}
	if n, _, _, _ := e.up.stats(); n != 0 {
		t.Errorf("上游调用数 = %d，期望 0（⛔ 触顶即拒、发起上游之前）", n)
	}
}

// TestGuardrailContentPolicy403 断言护栏命中 ⇒ 403 content_policy（上游调用 0）。
func TestGuardrailContentPolicy403(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		o.Guardrail = policy.NewGuardrail([]*regexp.Regexp{regexp.MustCompile("forbidden-word")}, "", nil)
	})
	resp := e.postJSON("/v1/chat/completions",
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"say forbidden-word now"}]}`, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("状态 = %d，期望 403", resp.StatusCode)
	}
	_, code, _ := errorFields(t, readBody(t, resp))
	if code != "content_policy" {
		t.Errorf("code = %q，期望 content_policy", code)
	}
	if n, _, _, _ := e.up.stats(); n != 0 {
		t.Errorf("上游调用数 = %d，期望 0", n)
	}
}

// TestRateLimit429RetryAfter 断言 token 级限流超限 ⇒ 429 + Retry-After（2.3 治理三件套）。
func TestRateLimit429RetryAfter(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		o.TokenBucket = policy.NewTokenBucket(1, 0.0001)
	})
	resp := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("状态 = %d，期望 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("缺 Retry-After 头（429 须带）")
	}
	_, code, _ := errorFields(t, readBody(t, resp))
	if code != "rate_limit" {
		t.Errorf("code = %q，期望 rate_limit", code)
	}
	if n, _, _, _ := e.up.stats(); n != 0 {
		t.Errorf("上游调用数 = %d，期望 0", n)
	}
}

// TestEgressDenied403 断言出网管控（8.2）：私网地址 ⇒ 403 egress_denied、⛔ 不改投。
func TestEgressDenied403(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec {
		ep := primaryEP(base)
		ep.base = "http://10.0.0.9" // 私网（默认拒绝段）
		return []epSpec{ep}
	}, nil, nil)
	resp := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("状态 = %d，期望 403", resp.StatusCode)
	}
	_, code, _ := errorFields(t, readBody(t, resp))
	if code != "egress_denied" {
		t.Errorf("code = %q，期望 egress_denied", code)
	}
	if n, _, _, _ := e.up.stats(); n != 0 {
		t.Errorf("上游调用数 = %d，期望 0（⛔ 不改投其他端点）", n)
	}
}

// TestContextExceeded400 断言上下文越界显式报错（A2/D2）：400 context_exceeded、⛔ 不静默截断。
func TestContextExceeded400(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec {
		ep := primaryEP(base)
		ep.ctxWindow = 100
		return []epSpec{ep}
	}, nil, nil)
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"` + strings.Repeat("x", 400) + `"}],"max_tokens":200}`
	resp := e.postJSON("/v1/chat/completions", body, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400", resp.StatusCode)
	}
	_, code, msg := errorFields(t, readBody(t, resp))
	if code != "context_exceeded" {
		t.Errorf("code = %q，期望 context_exceeded", code)
	}
	if !strings.Contains(msg, "context_exceeded") {
		t.Errorf("错误信息 = %q，须含越界判定明细", msg)
	}
	if n, _, _, _ := e.up.stats(); n != 0 {
		t.Errorf("上游调用数 = %d，期望 0（⛔ 不静默截断后发出）", n)
	}
}

// TestContextWindowFallbackChain 断言上下文越界走 context_window 专用降级链（2.3 / 7.4）：
// 同名模型组里主端点窗口不足 ⇒ 降级到链内大窗口端点（⛔ 不静默截断、⛔ 不报错了事）。
func TestContextWindowFallbackChain(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec {
		primary := primaryEP(base)
		primary.ctxWindow = 100
		large := primaryEP(base)
		large.name = "large" // 同名模型组（model 同为 gpt-4o-mini），窗口更大
		large.ctxWindow = 128000
		large.priority = 1
		return []epSpec{primary, large}
	}, map[string][]string{"context_window": {"large"}}, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"` + strings.Repeat("x", 400) + `"}],"max_tokens":200}`
	resp := e.postJSON("/v1/chat/completions", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（走 context_window 专用链的大窗口端点）", resp.StatusCode)
	}
	if n, _, _, _ := e.up.stats(); n != 1 {
		t.Errorf("上游调用数 = %d，期望 1（链内大窗口端点命中即发）", n)
	}
}

// TestRetrySameEndpointThenSuccess 断言重试打同一端点、上限 = Endpoint.MaxRetries（7.4）。
func TestRetrySameEndpointThenSuccess(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec {
		ep := primaryEP(base)
		ep.maxRetries = 1
		return []epSpec{ep}
	}, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	e.up.setFailFirstN(1) // 首次 500、第二次成功

	resp := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（首次 500、重试成功）", resp.StatusCode)
	}
	if n, _, _, _ := e.up.stats(); n != 2 {
		t.Errorf("上游调用数 = %d，期望 2（1 次失败 + 1 次重试）", n)
	}
}

// TestDegradeToChainEndpoint 断言降级换端点（7.4）：按失败类型分链、⛔ 不合成兜底循环。
func TestDegradeToChainEndpoint(t *testing.T) {
	up2 := newFakeUpstream(t)
	t.Cleanup(up2.close)
	up2.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	e := newEnv(t, func(base string) []epSpec {
		primary := primaryEP(base)
		backup := primaryEP(up2.srv.URL)
		backup.name = "backup"
		backup.priority = 1
		return []epSpec{primary, backup}
	}, map[string][]string{"upstream_error": {"backup"}}, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	e.up.setFailFirstN(99) // 主端点恒 500

	resp := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（降级到 backup）", resp.StatusCode)
	}
	if n, _, _, _ := e.up.stats(); n != 1 {
		t.Errorf("主端点上游调用数 = %d，期望 1（重试上限 0）", n)
	}
	if n, _, _, _ := up2.stats(); n != 1 {
		t.Errorf("backup 上游调用数 = %d，期望 1", n)
	}
}

// TestAuthHeaderInjectionThreeForms 断言鉴权头三种形态按 provider 注入（D5 / 8.4）：
// Authorization: Bearer / x-api-key + anthropic-version / api-key；⛔ 不透传调用方凭据。
func TestAuthHeaderInjectionThreeForms(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec {
		return []epSpec{primaryEP(base), claudeEP(base), mimoEP(base)}
	}, nil, nil)
	e.up.setEventsFor("/responses", loadSSE(t, "openai_responses_stream.sse"))
	e.up.setEventsFor("/messages", loadSSE(t, "anthropic_messages_stream.sse"))

	// OpenAI 形：Authorization: Bearer <网关凭据>（⛔ 不是调用方凭据）。
	resp := e.postJSON("/v1/chat/completions", chatHelloBody,
		map[string]string{"Authorization": "Bearer caller-secret", "X-Api-Key": "caller-x"})
	readBody(t, resp)
	h0 := e.up.headersAt(0)
	if got := h0.Get("Authorization"); got != "Bearer sk-gw-openai-0001" {
		t.Errorf("OpenAI 上游 Authorization = %q，期望 Bearer sk-gw-openai-0001（⛔ 不透传调用方凭据）", got)
	}
	if got := h0.Get("X-Api-Key"); got != "" {
		t.Errorf("OpenAI 上游 X-Api-Key = %q，期望空（⛔ 不透传调用方凭据）", got)
	}

	// Anthropic 形：x-api-key + anthropic-version（走 /v1/messages —— 协议绑定后
	// anthropic 端点的唯一合法入口）。
	resp2 := e.postJSON("/v1/messages",
		`{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer caller-secret"})
	readBody(t, resp2)
	h1 := e.up.headersAt(1)
	if got := h1.Get("X-Api-Key"); got != "sk-gw-anthropic-0002" {
		t.Errorf("Anthropic 上游 x-api-key = %q，期望 sk-gw-anthropic-0002", got)
	}
	if got := h1.Get("Anthropic-Version"); got == "" {
		t.Error("Anthropic 上游缺 anthropic-version 头（ExtraHeaders 须随鉴权头同批注入）")
	}
	if got := h1.Get("Authorization"); got != "" {
		t.Errorf("Anthropic 上游 Authorization = %q，期望空（⛔ 不透传调用方凭据）", got)
	}

	// MiMo 形：api-key（非标准头名）。
	resp3 := e.postJSON("/v1/chat/completions",
		`{"model":"mimo-v2","messages":[{"role":"user","content":"hi"}]}`, nil)
	readBody(t, resp3)
	h2 := e.up.headersAt(2)
	if got := h2.Get("Api-Key"); got != "sk-gw-mimo-0003" {
		t.Errorf("MiMo 上游 api-key = %q，期望 sk-gw-mimo-0003", got)
	}
	if got := h2.Get("Authorization"); got != "" {
		t.Errorf("MiMo 上游 Authorization = %q，期望空（⛔ 不用全局硬编码头名）", got)
	}
}

// TestAnthropicResidualUsage 断言 Anthropic 残值语义（A6）：
// 归一 InputTokens = input_tokens(20) + cache_creation(0) + cache_read(3) = 23；
// Messages 出口按三项拆分承载（input_tokens 为未缓存输入 20、cache_read 单列 3）。
func TestAnthropicResidualUsage(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec {
		return []epSpec{claudeEP(base)}
	}, nil, nil)
	e.up.setEventsFor("/messages", loadSSE(t, "anthropic_messages_stream.sse"))

	resp := e.postJSON("/v1/messages",
		`{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	m := readBody(t, resp)
	usage, _ := m["usage"].(map[string]any)
	if usage["input_tokens"] != float64(20) {
		t.Errorf("input_tokens = %v，期望 20（未缓存输入，A6 拆分口径）", usage["input_tokens"])
	}
	if usage["cache_read_input_tokens"] != float64(3) {
		t.Errorf("cache_read_input_tokens = %v，期望 3（缓存命中单列）", usage["cache_read_input_tokens"])
	}
	if usage["cache_creation_input_tokens"] != float64(0) {
		t.Errorf("cache_creation_input_tokens = %v，期望 0", usage["cache_creation_input_tokens"])
	}
	if usage["output_tokens"] != float64(5) {
		t.Errorf("output_tokens = %v，期望 5", usage["output_tokens"])
	}
}

// claudeEP 是 Anthropic 形端点（x-api-key 注入、⛔ 不支持服务端会话）。
func claudeEP(base string) epSpec {
	return epSpec{
		name: "claude", base: base, model: "claude-3-5-sonnet", upstreamModel: "claude-3-5-sonnet",
		provider: "anthropic", credRef: "TEST_KEY_ANTHROPIC",
		ctxWindow: 200000, maxOut: 8192, maxRetries: 0, priority: 1, timeoutS: 5,
	}
}

// mimoEP 是小米 MiMo 端点（api-key 非标准头名注入）。
func mimoEP(base string) epSpec {
	return epSpec{
		name: "mimo", base: base, model: "mimo-v2", upstreamModel: "mimo-v2",
		provider: "xiaomi", credRef: "TEST_KEY_MIMO",
		ctxWindow: 128000, maxOut: 4096, maxRetries: 0, priority: 2, timeoutS: 5,
	}
}

// readRaw 读响应体原文。
func readRaw(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	return string(raw)
}

// splitBlocks 把 SSE 响应原文切成事件块。
func splitBlocks(raw string) []string {
	var out []string
	for _, b := range strings.Split(raw, "\n\n") {
		if strings.TrimSpace(b) != "" {
			out = append(out, b+"\n\n")
		}
	}
	return out
}

// TestAdapterDetailTimeoutRetryable 钉住超时误分类修复（2026-11-14 真实压测实测）：
// context deadline exceeded 必须归 timeout 类（可重试），⛔ 不得归 invalid_request
// （否则可重试判定把它排除、重试被静默绕过，成功率掉 63/64）。
// 错误实现对照：兜底一律 invalid_request ⇒ 本测试必红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/server/ -run TestAdapterDetailTimeoutRetryable -v
func TestAdapterDetailTimeoutRetryable(t *testing.T) {
	if d := adapterDetail(context.DeadlineExceeded); d.Category != core.ErrTimeout {
		t.Fatalf("context.DeadlineExceeded 归类 = %s，期望 timeout（可重试）", d.Category)
	}
	if d := adapterDetail(fmt.Errorf("do request: %w", context.DeadlineExceeded)); d.Category != core.ErrTimeout {
		t.Fatalf("包装后的超时归类 = %s，期望 timeout", d.Category)
	}
	if d := adapterDetail(context.Canceled); d.Category != core.ErrCancelled {
		t.Fatalf("context.Canceled 归类 = %s，期望 cancelled（⛔ 不重试）", d.Category)
	}
}

// TestTimeoutRetriedThenSucceeds 端到端钉住「成功率恒 100%」口径：上游首请求卡满超时、
// 同端点重试成功 ⇒ 客户端拿到 200、上游恰好被打 2 次、RetryCount 计数分离。
// 错误实现对照：超时不重试（误分类回归）⇒ 首试即失败、上游只被打 1 次 ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/server/ -run TestTimeoutRetriedThenSucceeds -v
func TestTimeoutRetriedThenSucceeds(t *testing.T) {
	t.Setenv("TEST_KEY_MIMO", "sk-gw-mimo-0003")
	const okChunk = "" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			time.Sleep(3 * time.Second) // 首请求卡死 ⇒ 触发网关侧超时（TimeoutS=1）
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, okChunk)
	}))
	defer up.Close()
	reg := writeRegistry(t, []epSpec{{
		name: "flaky", base: up.URL, model: "m", upstreamModel: "mimo-v2.6-flash",
		provider: "xiaomi", credRef: "TEST_KEY_MIMO",
		ctxWindow: 128000, maxOut: 1024, maxRetries: 1, priority: 0, timeoutS: 1,
	}}, nil)
	srv, err := New(Options{
		Registry:            reg,
		Tracer:              telemetry.NewTracer(&telemetry.InMemoryExporter{}),
		Metrics:             telemetry.NewMetrics(),
		AllowLoopbackEgress: true,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		RetryBackoff:        Backoff{Base: time.Millisecond, Jitter: 0, Cap: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("server.New 失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("重试后应 200（成功率恒 100%% 口径），实际 %d：%s", resp.StatusCode, body[:200])
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("上游调用 = %d，期望 2（首试超时 + 重试成功）", got)
	}
}

// TestRetryBackoffAppliedOnUpstreamError 钉住「同端点重试前指数退避」（统一 SDD 7.4）：
// 上游 5xx 两次后成功、max_retries=2 ⇒ 重试前各等 Base·2^(n-1)（50ms + 100ms）、
// 上游恰好被打 3 次、客户端 200。
// 错误实现对照：无退避 ⇒ elapsed < 150ms 红；不重试 ⇒ calls != 3 红。
func TestRetryBackoffAppliedOnUpstreamError(t *testing.T) {
	t.Setenv("TEST_KEY_MIMO", "sk-gw-mimo-0003")
	const okChunk = "" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, okChunk)
	}))
	defer up.Close()
	reg := writeRegistry(t, []epSpec{{
		name: "flaky", base: up.URL, model: "m", upstreamModel: "mimo-v2.6-flash",
		provider: "xiaomi", credRef: "TEST_KEY_MIMO",
		ctxWindow: 128000, maxOut: 1024, maxRetries: 2, priority: 0, timeoutS: 5,
	}}, nil)
	srv, err := New(Options{
		Registry:            reg,
		Tracer:              telemetry.NewTracer(&telemetry.InMemoryExporter{}),
		Metrics:             telemetry.NewMetrics(),
		AllowLoopbackEgress: true,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Jitter=0 保持确定性：n=1 ⇒ 50ms、n=2 ⇒ 100ms ⇒ 总退避 150ms。
		RetryBackoff: Backoff{Base: 50 * time.Millisecond, Jitter: 0, Cap: 200 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("server.New 失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	start := time.Now()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("退避重试后应 200，实际 %d：%s", resp.StatusCode, body[:200])
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("上游调用 = %d，期望 3（两次 500 + 重试成功）", got)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("端到端耗时 %v < 150ms —— 重试前未按 50ms+100ms 退避", elapsed)
	}
}

// TestEndpointConcurrencyLimitRejectsAndReleases 断言端点并发维生效（白组预占 + 收口释放）：
// 并发位满 ⇒ 429（message 标明并发闸）；正常请求收口后位释放（inFlight 归零）。
func TestEndpointConcurrencyLimitRejectsAndReleases(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec {
		p := primaryEP(base)
		p.concurrencyLimit = 1
		return []epSpec{p}
	}, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`

	// 预占唯一并发位 ⇒ 下一发应被并发闸拒绝。
	e.srv.limitGroups["primary"].inFlight.Store(1)
	resp := e.postJSON("/v1/chat/completions", body, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("并发位满时 = %d，期望 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 缺 Retry-After 头")
	}
	_, code, msg := errorFields(t, readBody(t, resp))
	if code != "rate_limit" || !strings.Contains(msg, "并发") {
		t.Fatalf("code=%q msg=%q，期望 rate_limit 且标明并发闸", code, msg)
	}

	// 释放后放行，且收口自动归还并发位。
	e.srv.limitGroups["primary"].inFlight.Store(0)
	resp2 := e.postJSON("/v1/chat/completions", body, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("释放后 = %d，期望 200", resp2.StatusCode)
	}
	readBody(t, resp2)
	if got := e.srv.limitGroups["primary"].inFlight.Load(); got != 0 {
		t.Fatalf("收口后并发位 = %d，期望 0（defer release）", got)
	}
}

// TestLimitsGroupSharesQuota 断言同 limits_group 的端点共享一份契约桶：
// 端点 A 用掉配额后，端点 B（同组）应被拒 —— 若各端点各建桶则 B 会放行（= 共享配额池超发）。
func TestLimitsGroupSharesQuota(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec {
		a := primaryEP(base)
		a.limitsGroup = "pool"
		a.rateLimit = map[string]any{"tpm": 1200, "rpm": 0, "burst_s": 1} // 桶深 20，每请求预估 13
		b := primaryEP(base)
		b.name, b.model, b.priority = "secondary", "gpt-4o", 1
		b.limitsGroup = "pool"
		b.rateLimit = map[string]any{"tpm": 1200, "rpm": 0, "burst_s": 1}
		return []epSpec{a, b}
	}, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/chat/completions",
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("端点 A 首发 = %d，期望 200", resp.StatusCode)
	}
	readBody(t, resp)

	resp2 := e.postJSON("/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`, nil)
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("同组端点 B = %d，期望 429（共享桶剩余 7 < 预估 13）", resp2.StatusCode)
	}
	readBody(t, resp2)
	if n, _, _, _ := e.up.stats(); n != 1 {
		t.Errorf("上游调用数 = %d，期望 1（429 不打上游）", n)
	}
}

// TestMaxInFlightSheds 断言全局过载闸（load shedding）：在途超上限 ⇒ 429 code=overloaded，
// 与限流（rate_limit）分开编码；压力回落后恢复。
func TestMaxInFlightSheds(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		o.MaxInFlight = 1
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`

	e.srv.inFlight.Store(1) // 白组预占：模拟在途已满
	resp := e.postJSON("/v1/chat/completions", body, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("过载时 = %d，期望 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 缺 Retry-After 头")
	}
	_, code, _ := errorFields(t, readBody(t, resp))
	if code != "overloaded" {
		t.Fatalf("code = %q，期望 overloaded（过载闸与限流分账）", code)
	}

	e.srv.inFlight.Store(0)
	resp2 := e.postJSON("/v1/chat/completions", body, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("压力回落后 = %d，期望 200", resp2.StatusCode)
	}
	readBody(t, resp2)
}

// TestMessagesIngressEndToEnd 断言 Anthropic Messages 入口端到端（统一 SDD 3.1 第三入站形）：
// Messages 形请求 ⇒ 管线 ⇒ Messages 形响应（type=message、stop_reason、usage 三项拆分）。
func TestMessagesIngressEndToEnd(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{claudeEP(base)} }, nil, nil)
	e.up.setEventsFor("/messages", loadSSE(t, "anthropic_messages_stream.sse"))
	body := `{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	resp := e.postJSON("/v1/messages", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", resp.StatusCode)
	}
	m := readBody(t, resp)
	if m["type"] != "message" || m["role"] != "assistant" {
		t.Fatalf("外壳失真（须 type=message/role=assistant）: %v", m)
	}
	if sr, _ := m["stop_reason"].(string); sr != "end_turn" {
		t.Fatalf("stop_reason = %v，期望 end_turn（原值透传）", m["stop_reason"])
	}
	// usage 三项拆分：归一 Input=20+3（缓存读）⇒ 出参 input_tokens=20、cache_read=3、output=5。
	u, _ := m["usage"].(map[string]any)
	if u == nil || u["input_tokens"] != 20.0 || u["output_tokens"] != 5.0 ||
		u["cache_read_input_tokens"] != 3.0 {
		t.Fatalf("usage 三项拆分失真: %v", m["usage"])
	}
	if _, ok := m["content"].([]any); !ok {
		t.Fatalf("缺 content 数组: %v", m)
	}

	// Anthropic 语义：max_tokens 必填 ⇒ 缺失 400。
	resp2 := e.postJSON("/v1/messages", `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 max_tokens = %d，期望 400", resp2.StatusCode)
	}
	readBody(t, resp2)
}

// TestMessagesIngressStream 断言 Messages 入口流式：Anthropic 事件序列完整。
func TestMessagesIngressStream(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{claudeEP(base)} }, nil, nil)
	e.up.setEventsFor("/messages", loadSSE(t, "anthropic_messages_stream.sse"))
	body := `{"model":"claude-3-5-sonnet","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp := e.postJSON("/v1/messages", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("读流失败: %v", err)
	}
	got := string(raw)
	for _, want := range []string{"event: message_start", "event: content_block_delta",
		"event: message_delta", "event: message_stop"} {
		if !strings.Contains(got, want) {
			t.Fatalf("流式缺 %q；实际尾部: %.300s", want, got[max(0, len(got)-300):])
		}
	}
}

// TestIngressProtocolMismatch 断言入口协议与模型族硬绑定（Claude 模型只走 /v1/messages，
// 其它供应商只走 OpenAI 形入口）：违配显式 400 ingress_protocol_mismatch，⛔ 不静默翻译。
func TestIngressProtocolMismatch(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec {
		return []epSpec{claudeEP(base), primaryEP(base)}
	}, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	e.up.setEventsFor("/messages", loadSSE(t, "anthropic_messages_stream.sse"))

	// Claude 模型走 OpenAI 形入口 ⇒ 400。
	resp := e.postJSON("/v1/chat/completions",
		`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("chat+claude = %d，期望 400", resp.StatusCode)
	}
	_, code, _ := errorFields(t, readBody(t, resp))
	if code != "ingress_protocol_mismatch" {
		t.Fatalf("code = %q，期望 ingress_protocol_mismatch", code)
	}

	// 非 anthropic 模型走 Messages 入口 ⇒ 400。
	resp2 := e.postJSON("/v1/messages",
		`{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("messages+gpt = %d，期望 400", resp2.StatusCode)
	}
	_, code2, _ := errorFields(t, readBody(t, resp2))
	if code2 != "ingress_protocol_mismatch" {
		t.Fatalf("code = %q，期望 ingress_protocol_mismatch", code2)
	}

	// 同族两向放行。
	resp3 := e.postJSON("/v1/messages",
		`{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("messages+claude = %d，期望 200", resp3.StatusCode)
	}
	readBody(t, resp3)
	resp4 := e.postJSON("/v1/chat/completions",
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("chat+gpt = %d，期望 200", resp4.StatusCode)
	}
	readBody(t, resp4)
}

// TestBreakerTripsAndSkipsToFallback 钉住「熔断期间跳过该端点转降级链」（统一 SDD 7.3）：
// 主端点连续失败达阈值（threshold=1）⇒ 熔断打开；后续请求⛔ 不再打主上游（调用数冻结）、
// 直接走降级链到 backup；两请求客户端均 200。
// 错误实现对照：熔断后仍打主上游 ⇒ 第 2 次断言主计数红；跳过后不降级 ⇒ 第 2 请求非 200。
func TestBreakerTripsAndSkipsToFallback(t *testing.T) {
	t.Setenv("TEST_KEY_MIMO", "sk-gw-mimo-0003")
	const okChunk = "" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	var primaryCalls, backupCalls int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryCalls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupCalls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, okChunk)
	}))
	defer backup.Close()
	reg := writeRegistry(t, []epSpec{
		{
			name: "primary", base: primary.URL, model: "m", upstreamModel: "mimo-v2.6-flash",
			provider: "xiaomi", credRef: "TEST_KEY_MIMO",
			ctxWindow: 128000, maxOut: 1024, maxRetries: 0, priority: 0, timeoutS: 5,
			breaker: map[string]any{"failure_threshold": 1, "cooldown_s": 30},
		},
		{
			name: "backup", base: backup.URL, model: "m", upstreamModel: "mimo-v2.6-flash",
			provider: "xiaomi", credRef: "TEST_KEY_MIMO",
			ctxWindow: 128000, maxOut: 1024, maxRetries: 0, priority: 1, timeoutS: 5,
		},
	}, map[string][]string{
		"general":        {"backup"},
		"upstream_error": {"backup"},
	})
	srv, err := New(Options{
		Registry:            reg,
		Tracer:              telemetry.NewTracer(&telemetry.InMemoryExporter{}),
		Metrics:             telemetry.NewMetrics(),
		AllowLoopbackEgress: true,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		RetryBackoff:        Backoff{Base: time.Millisecond, Jitter: 0, Cap: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("server.New 失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func() *http.Response {
		t.Helper()
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		return resp
	}

	// 请求 1：主 500（阈值 1 ⇒ 熔断开）⇒ 降级 backup 200。
	resp1 := post()
	if resp1.StatusCode != 200 {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("请求 1 = %d，期望 200（降级成功）：%s", resp1.StatusCode, body[:200])
	}
	resp1.Body.Close()
	if got := atomic.LoadInt32(&primaryCalls); got != 1 {
		t.Fatalf("请求 1 后主上游调用 = %d，期望 1", got)
	}
	// 请求 2：主已熔断 ⇒ ⛔ 不打主上游、直接降级 backup。
	resp2 := post()
	if resp2.StatusCode != 200 {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("请求 2 = %d，期望 200（熔断跳过后降级成功）：%s", resp2.StatusCode, body[:200])
	}
	resp2.Body.Close()
	if got := atomic.LoadInt32(&primaryCalls); got != 1 {
		t.Fatalf("请求 2 后主上游调用 = %d，期望 1（熔断后冻结，⛔ 不再打）", got)
	}
	if got := atomic.LoadInt32(&backupCalls); got != 2 {
		t.Fatalf("backup 调用 = %d，期望 2", got)
	}
	if st := srv.breakerFor("primary").State(); st != "open" {
		t.Fatalf("primary 熔断状态 = %s，期望 open", st)
	}
}
