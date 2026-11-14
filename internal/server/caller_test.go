// 日期: 2026-11-14 15:38:17.246
// 作者: Zhai Wenjia

package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/policy"
)

// 本文件测试调用方 API key 侧（统一 SDD 8.1 两段式凭据）：强制鉴权、
// 调用方 key 与上游自有令牌**解耦**（⛔ 调用方凭据永不发给上游）、
// 每调用方令牌桶（token 级限流，⛔ 不只按请求数）。

// TestCallerAuth401 断言调用方鉴权：缺失 / 无效 key ⇒ 401 unauthorized；有效 ⇒ 放行。
func TestCallerAuth401(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		o.CallerKeys = []string{"ck-alpha"}
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	// 无凭据 ⇒ 401。
	resp := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据 = %d，期望 401", resp.StatusCode)
	}
	typ, code, _ := errorFields(t, readBody(t, resp))
	if typ != "auth" || code != "unauthorized" {
		t.Errorf("error = %q/%q，期望 auth/unauthorized", typ, code)
	}

	// 错误 key ⇒ 401。
	resp2 := e.postJSON("/v1/chat/completions", chatHelloBody,
		map[string]string{"Authorization": "Bearer wrong-key"})
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 key = %d，期望 401", resp2.StatusCode)
	}
	readBody(t, resp2)

	// 有效 key ⇒ 放行。
	resp3 := e.postJSON("/v1/chat/completions", chatHelloBody,
		map[string]string{"Authorization": "Bearer ck-alpha"})
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("有效 key = %d，期望 200", resp3.StatusCode)
	}
	readBody(t, resp3)

	// /v1/* 全部要求鉴权（含 /v1/models 与 /v1/cancel）。
	req, _ := http.NewRequest(http.MethodGet, e.http.URL+"/v1/models", nil)
	resp4, _ := http.DefaultClient.Do(req)
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusUnauthorized {
		t.Errorf("/v1/models 无凭据 = %d，期望 401", resp4.StatusCode)
	}
	if n, _, _, _ := e.up.stats(); n != 1 {
		t.Errorf("上游调用数 = %d，期望 1（401 路径不打上游）", n)
	}
}

// TestCallerKeyDecoupledFromUpstreamToken 断言解耦（本任务核心）：
// 调用方 API key 只做调用方鉴权；发给上游的**永远是网关自有令牌**（⛔ 调用方凭据不外发）。
func TestCallerKeyDecoupledFromUpstreamToken(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		o.CallerKeys = []string{"ck-alpha"}
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/chat/completions", chatHelloBody,
		map[string]string{"Authorization": "Bearer ck-alpha", "X-Api-Key": "caller-extra-cred"})
	readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", resp.StatusCode)
	}
	got := e.up.lastHeaders().Get("Authorization")
	if got != "Bearer sk-gw-openai-0001" {
		t.Errorf("上游 Authorization = %q，期望自有令牌 Bearer sk-gw-openai-0001", got)
	}
	if strings.Contains(got, "ck-alpha") {
		t.Errorf("上游凭据混入调用方 key：%q（⛔ 解耦被破坏）", got)
	}
	if v := e.up.lastHeaders().Get("X-Api-Key"); v != "" {
		t.Errorf("上游 X-Api-Key = %q，期望空（⛔ 调用方凭据不外发）", v)
	}
}

// TestCallerBucketPerCallerIsolation 断言每调用方令牌桶（8.3 token 级限流）：
// 调用方 A 打满自己的桶 ⇒ 429 + Retry-After；调用方 B 的桶独立 ⇒ 放行。
func TestCallerBucketPerCallerIsolation(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		o.CallerKeys = []string{"ck-alpha", "ck-beta"}
		// 桶容量 15：max_tokens=10 时每请求预估 13 归一化 token ⇒ 放一发、拒二发。
		o.CallerBucketCapacity = 15
		o.CallerBucketRefill = 0.0001
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`

	// A 第一发放行。
	resp := e.postJSON("/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer ck-alpha"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A 第一发 = %d，期望 200", resp.StatusCode)
	}
	readBody(t, resp)

	// A 第二发触桶 ⇒ 429 + Retry-After。
	resp2 := e.postJSON("/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer ck-alpha"})
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("A 第二发 = %d，期望 429（令牌桶超限）", resp2.StatusCode)
	}
	if resp2.Header.Get("Retry-After") == "" {
		t.Error("A 429 缺 Retry-After 头")
	}
	_, code, _ := errorFields(t, readBody(t, resp2))
	if code != "rate_limit" {
		t.Errorf("code = %q，期望 rate_limit", code)
	}

	// B 的桶独立 ⇒ 放行。
	resp3 := e.postJSON("/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer ck-beta"})
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("B 第一发 = %d，期望 200（每调用方桶隔离）", resp3.StatusCode)
	}
	readBody(t, resp3)

	if n, _, _, _ := e.up.stats(); n != 2 {
		t.Errorf("上游调用数 = %d，期望 2（两次放行各打一发；429 不打上游）", n)
	}
}

// TestCallerBucketDisabledByDefault 断言未配桶参数时不启用每调用方限流（向后兼容）。
func TestCallerBucketDisabledByDefault(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		o.CallerKeys = []string{"ck-alpha"}
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	for i := 0; i < 3; i++ {
		resp := e.postJSON("/v1/chat/completions", body,
			map[string]string{"Authorization": "Bearer ck-alpha"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 发 = %d，期望 200（未启用桶 ⇒ 不限流）", i+1, resp.StatusCode)
		}
		readBody(t, resp)
	}
}

// TestCallerIdentityFeedsBudget 断言预算按调用方累计（调用方解耦后的计费口径）：
// A 打满预算 ⇒ 429 budget_exceeded；B 的预算独立 ⇒ 放行。
func TestCallerIdentityFeedsBudget(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		o.CallerKeys = []string{"ck-alpha", "ck-beta"}
		// 预算 20：max_tokens=10 时每请求预估 13 ⇒ A 一发后余 7 < 13 ⇒ 触顶；B 独立预算。
		o.Budget = policy.NewBudget(20)
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`

	resp := e.postJSON("/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer ck-alpha"})
	readBody(t, resp)

	resp2 := e.postJSON("/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer ck-alpha"})
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("A 预算触顶 = %d，期望 429", resp2.StatusCode)
	}
	_, code, _ := errorFields(t, readBody(t, resp2))
	if code != "budget_exceeded" {
		t.Errorf("code = %q，期望 budget_exceeded", code)
	}

	resp3 := e.postJSON("/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer ck-beta"})
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("B = %d，期望 200（预算按调用方独立累计）", resp3.StatusCode)
	}
	readBody(t, resp3)
}
