// 日期: 2026-11-14 15:38:17.246
// 作者: Zhai Wenjia

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/policy"
)

// 本文件测试调用方校验面（R-43 / AC-36，registry callers 单源）：
//   - 四类拒：无 key / 错 key ⇒ 401、过期 key ⇒ 401、超 scope（模型/端点/预算）⇒ 403、
//     伪造身份 header ⇒ 入口剥离不直通（F-36 / F-37）；
//   - key 只以 SHA-256 哈希入配置（存储形态检查）、双 key 轮换窗口并存可用；
//   - 审计四元组（caller / route / decision / correlation-id）可查且无密钥；
//   - 调用方 key 与上游自有令牌解耦（⛔ 调用方凭据永不发给上游）。

// sha256hex 是登记形态的 key 哈希（配置只存它，⛔ 明文不落配置）。
func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// callersWire 构造注册表 callers 节（测试输入是配置文本，非 mock 数据）。
//
// ⚠️ expires_at 用 RFC3339Nano（保留亚秒）——四件事：
//   - 现象：过期用例取「now+250ms」做将来时刻，Format(time.RFC3339) 秒级截断后
//     可能 ≤ now ⇒ registry.Load 按「已过期」拒，测试偶发红在夹具装配而非目标断言；
//   - 根因：RFC3339 丢弃亚秒，「未来 250ms」在秒边界处被截成当前秒甚至过去，
//     正常路径（+24h 等整时刻）没事，只有亚秒级窗口才踩中；
//   - 为什么这个修法：换 RFC3339Nano 保真传时刻（time.Time JSON 解析同口径）；
//     备选是把窗口拉大到秒级再 sleep——测试慢 2s 且仍留边界，不取；
//   - 不修会怎样：过期用例成 flaky（时好时坏看跑的毫秒），掩盖真回归。
func callersWire(name, plainKey string, expires time.Time, models, endpoints []string, tokenBudget int64, rl map[string]any) map[string]any {
	w := map[string]any{
		"name":  name,
		"keys":  []map[string]any{{"key_hash": sha256hex(plainKey), "expires_at": expires.UTC().Format(time.RFC3339Nano)}},
		"scope": map[string]any{"models": models, "endpoints": endpoints, "token_budget": tokenBudget},
	}
	if rl != nil {
		w["rate_limit"] = rl
	}
	return w
}

// memLog 是审计断言用的内存日志收集器（收集真实 slog 出口，⛔ 不伪造审计面）。
type memLog struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *memLog) Enabled(context.Context, slog.Level) bool { return true }

func (h *memLog) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r)
	return nil
}

func (h *memLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *memLog) WithGroup(string) slog.Handler      { return h }

// callerDecisions 取全部审计判定行（消息 = caller_decision）的字段表。
func (h *memLog) callerDecisions() []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]string
	for _, r := range h.recs {
		if r.Message != "caller_decision" {
			continue
		}
		m := map[string]string{}
		r.Attrs(func(a slog.Attr) bool {
			m[a.Key] = a.Value.String()
			return true
		})
		out = append(out, m)
	}
	return out
}

// TestCallerAuth401 断言鉴权基线（AC-36 类一/类二）：缺失 / 错误 key ⇒ 401 通用错误
// （⛔ 响应不区分缺失 / 无效 / 过期，防枚举）；有效 key ⇒ 放行；401 路径不打上游。
func TestCallerAuth401(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil, map[string]any{
		"callers": []map[string]any{callersWire("harness", "ck-alpha", time.Now().Add(24*time.Hour),
			[]string{"gpt-4o-mini"}, []string{"primary"}, 0, nil)},
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

	// 错误 key ⇒ 401（与缺失同形 —— 防枚举）。
	resp2 := e.postJSON("/v1/chat/completions", chatHelloBody,
		map[string]string{"Authorization": "Bearer wrong-key"})
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 key = %d，期望 401", resp2.StatusCode)
	}
	typ2, code2, _ := errorFields(t, readBody(t, resp2))
	if typ2 != "auth" || code2 != "unauthorized" {
		t.Errorf("错误 key error = %q/%q，期望与缺失同形 auth/unauthorized", typ2, code2)
	}

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

// TestCallerKeyExpiredRejected 断言过期 key 必拒（F-36 ⛔ 永不过期）：
// 运行期到达 expires_at 后旧 key 立即失效（轮换窗口关闭），401 与无效同形。
func TestCallerKeyExpiredRejected(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil, map[string]any{
		"callers": []map[string]any{callersWire("harness", "ck-soon", time.Now().Add(250*time.Millisecond),
			[]string{"gpt-4o-mini"}, []string{"primary"}, 0, nil)},
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	time.Sleep(350 * time.Millisecond) // 等过期时刻越过后再打（expiry 由运行时刻判定）

	resp := e.postJSON("/v1/chat/completions", chatHelloBody,
		map[string]string{"Authorization": "Bearer ck-soon"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("过期 key = %d，期望 401", resp.StatusCode)
	}
	typ, code, _ := errorFields(t, readBody(t, resp))
	if typ != "auth" || code != "unauthorized" {
		t.Errorf("error = %q/%q，期望与无效同形 auth/unauthorized", typ, code)
	}
	if n, _, _, _ := e.up.stats(); n != 0 {
		t.Errorf("上游调用数 = %d，期望 0（过期 key 不打上游）", n)
	}
}

// TestCallerScopeModelDenied 断言模型超 scope ⇒ 403（AC-36 类三）。
// 两个模型都登记在注册表，但 scope 只放一个 —— 拒绝只能来自 scope 闸。
func TestCallerScopeModelDenied(t *testing.T) {
	eps := func(base string) []epSpec {
		return []epSpec{primaryEP(base), {
			name: "secondary", base: base, model: "gpt-4o-2", upstreamModel: "gpt-4o-mini",
			provider: "openai", credRef: "TEST_KEY_OPENAI",
			ctxWindow: 128000, maxOut: 4096, priority: 1, timeoutS: 5,
		}}
	}
	e := newEnv(t, eps, nil, nil, map[string]any{
		"callers": []map[string]any{callersWire("harness", "ck-alpha", time.Now().Add(24*time.Hour),
			[]string{"gpt-4o-mini"}, []string{"primary", "secondary"}, 0, nil)},
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/chat/completions",
		`{"model":"gpt-4o-2","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`,
		map[string]string{"Authorization": "Bearer ck-alpha"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("模型超 scope = %d，期望 403", resp.StatusCode)
	}
	typ, code, _ := errorFields(t, readBody(t, resp))
	if typ != "auth" || code != "scope_exceeded" {
		t.Errorf("error = %q/%q，期望 auth/scope_exceeded", typ, code)
	}
	if n, _, _, _ := e.up.stats(); n != 0 {
		t.Errorf("上游调用数 = %d，期望 0（超 scope 不打上游）", n)
	}
}

// TestCallerScopeEndpointDenied 断言端点超 scope ⇒ 403（AC-36 类三）：
// 模型在允许集内、路由选中的端点不在 ⇒ 拒（scope 按路由落点判，⛔ 不按模型名糊过）。
func TestCallerScopeEndpointDenied(t *testing.T) {
	eps := func(base string) []epSpec {
		return []epSpec{primaryEP(base), {
			name: "secondary", base: base, model: "gpt-4o-2", upstreamModel: "gpt-4o-mini",
			provider: "openai", credRef: "TEST_KEY_OPENAI",
			ctxWindow: 128000, maxOut: 4096, priority: 1, timeoutS: 5,
		}}
	}
	e := newEnv(t, eps, nil, nil, map[string]any{
		"callers": []map[string]any{callersWire("harness", "ck-alpha", time.Now().Add(24*time.Hour),
			[]string{"gpt-4o-mini", "gpt-4o-2"}, []string{"secondary"}, 0, nil)},
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/chat/completions", chatHelloBody,
		map[string]string{"Authorization": "Bearer ck-alpha"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("端点超 scope = %d，期望 403", resp.StatusCode)
	}
	_, code, _ := errorFields(t, readBody(t, resp))
	if code != "scope_exceeded" {
		t.Errorf("code = %q，期望 scope_exceeded", code)
	}
	if n, _, _, _ := e.up.stats(); n != 0 {
		t.Errorf("上游调用数 = %d，期望 0", n)
	}
}

// TestCallerScopeTokenBudget 断言 scope token 预算是第三闸（AC-36 类三）：
// 打满预算 ⇒ 403 scope_exceeded；预算按调用方独立（另一调用方不受影响）。
func TestCallerScopeTokenBudget(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil, map[string]any{
		"callers": []map[string]any{
			callersWire("alpha", "ck-alpha", time.Now().Add(24*time.Hour),
				[]string{"gpt-4o-mini"}, []string{"primary"}, 20, nil), // 每请求预估 13 ⇒ 一发后余 7 < 13
			callersWire("beta", "ck-beta", time.Now().Add(24*time.Hour),
				[]string{"gpt-4o-mini"}, []string{"primary"}, 0, nil),
		},
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`

	if resp := e.postJSON("/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer ck-alpha"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("alpha 首发 = %d，期望 200", resp.StatusCode)
	} else {
		readBody(t, resp)
	}
	resp2 := e.postJSON("/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer ck-alpha"})
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("alpha 预算触顶 = %d，期望 403", resp2.StatusCode)
	}
	_, code, _ := errorFields(t, readBody(t, resp2))
	if code != "scope_exceeded" {
		t.Errorf("code = %q，期望 scope_exceeded（预算属 scope 三闸）", code)
	}

	// beta 预算独立 ⇒ 放行。
	resp3 := e.postJSON("/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer ck-beta"})
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("beta = %d，期望 200（预算按调用方独立）", resp3.StatusCode)
	}
	readBody(t, resp3)
}

// TestCallerIdentityHeadersStripped 断言伪造身份 header 不直通（AC-36 类四，F-37）：
// 入口剥离 X-User-Id 等身份类 header（⛔ 公网 header 直通）；身份一律以 key 鉴权结果为准。
func TestCallerIdentityHeadersStripped(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil, map[string]any{
		"callers": []map[string]any{callersWire("harness", "ck-alpha", time.Now().Add(24*time.Hour),
			[]string{"gpt-4o-mini"}, []string{"primary"}, 0, nil)},
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	resp := e.postJSON("/v1/chat/completions", chatHelloBody, map[string]string{
		"Authorization":    "Bearer ck-alpha",
		"X-User-Id":        "admin",
		"X-Org-Id":         "root-org",
		"X-Role":           "superuser",
		"X-Tenant-Id":      "tenant-admin",
		"X-Forwarded-User": "admin",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带伪造身份头 = %d，期望 200（剥离后照常处理）", resp.StatusCode)
	}
	readBody(t, resp)
	got := e.up.lastHeaders()
	for _, h := range []string{"X-User-Id", "X-Org-Id", "X-Role", "X-Tenant-Id", "X-Forwarded-User"} {
		if v := got.Get(h); v != "" {
			t.Errorf("上游收到身份类 header %s = %q，期望空（⛔ 公网 header 直通）", h, v)
		}
	}
}

// TestCallerIdentityHeadersStrippedAtEntry 断言剥离发生在入口（wrap 内）：后续处理
// 拿到的 r.Header 已不含身份类 header（F-37）。⛔ 不靠「上游恰好不转发客户端头」的
// 结构性巧合过活 —— 本断言对「去掉 Del」变异必红（端到端那条在该变异下仍绿）。
func TestCallerIdentityHeadersStrippedAtEntry(t *testing.T) {
	c := newCallerAuth(nil, nil, nil, nil) // 开放模式同样必须剥离
	var seen http.Header
	handler := c.wrap(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for _, h := range identityHeaders {
		req.Header.Set(h, "forged")
	}
	req.Header.Set("X-Custom-Keep", "keep")
	handler(httptest.NewRecorder(), req)

	for _, h := range identityHeaders {
		if seen.Get(h) != "" {
			t.Errorf("入口未剥离 %s（⛔ 公网身份 header 直通）", h)
		}
	}
	if seen.Get("X-Custom-Keep") != "keep" {
		t.Error("非身份头被误伤（剥离只针对身份类 header）")
	}
}

// TestCallerAuditQuartetNoKey 断言审计四元组可查且无密钥（R-43 / R-46）：
// 一拒一允各有审计行（caller / route / decision / correlation-id），⛔ 明文 key 与
// 哈希都不入日志；correlation-id 取客户端 X-Client-Request-Id（可关联）。
func TestCallerAuditQuartetNoKey(t *testing.T) {
	logs := &memLog{}
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		o.Logger = slog.New(logs)
	}, map[string]any{
		"callers": []map[string]any{callersWire("harness", "ck-alpha", time.Now().Add(24*time.Hour),
			[]string{"gpt-4o-mini"}, []string{"primary"}, 0, nil)},
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	const corr = "corr-audit-123"

	// 拒（错 key）。
	resp := e.postJSON("/v1/chat/completions", chatHelloBody, map[string]string{
		"Authorization":       "Bearer wrong-key-42",
		"X-Client-Request-Id": corr,
	})
	readBody(t, resp)
	// 允（有效 key）。
	resp2 := e.postJSON("/v1/chat/completions", chatHelloBody, map[string]string{
		"Authorization":       "Bearer ck-alpha",
		"X-Client-Request-Id": corr,
	})
	readBody(t, resp2)

	decisions := logs.callerDecisions()
	if len(decisions) < 2 {
		t.Fatalf("审计判定行 = %d，期望 ≥2（一拒一允）", len(decisions))
	}
	// 判定行分两层：鉴权层 route = "POST /v1/chat/completions"，scope 层 route =
	// "model@endpoint" —— 断言必须锚定到层，否则 scope 层的 allow 会掩盖鉴权层的
	// 判定翻转（变异 M4 实测：鉴权 allow 翻 deny 全绿 ⇒ 恒真断言，当场收紧到层粒度）。
	sawAuthDeny, sawAuthAllow := false, false
	for _, d := range decisions {
		for _, f := range []string{"caller", "route", "decision", "correlation_id"} {
			if d[f] == "" {
				t.Errorf("审计行缺四元组字段 %q: %+v", f, d)
			}
		}
		if d["correlation_id"] != corr {
			t.Errorf("correlation_id = %q，期望 %q（可关联到请求）", d["correlation_id"], corr)
		}
		if d["route"] != "POST /v1/chat/completions" {
			continue // scope 层判定行（model@endpoint）不在本断言面
		}
		switch d["decision"] {
		case "deny":
			sawAuthDeny = true
			if d["caller"] != AnonymousCaller {
				t.Errorf("拒行 caller = %q，期望 %q", d["caller"], AnonymousCaller)
			}
		case "allow":
			sawAuthAllow = true
			if d["caller"] != "harness" {
				t.Errorf("允行 caller = %q，期望登记名 harness（⛔ 不采信身份 header）", d["caller"])
			}
		}
	}
	if !sawAuthDeny || !sawAuthAllow {
		t.Errorf("鉴权层审计应含一拒一允（deny=%v allow=%v）", sawAuthDeny, sawAuthAllow)
	}
	// 反向锁：明文 key 与哈希都不得出现在审计行里。
	for _, secret := range []string{"ck-alpha", "wrong-key-42", sha256hex("ck-alpha")} {
		for _, d := range decisions {
			for k, v := range d {
				if strings.Contains(v, secret) {
					t.Errorf("审计行泄密钥材料（字段 %s 含 %q）", k, secret)
				}
			}
		}
	}
}

// TestCallerRotationDualKey 断言双 key 平滑轮换窗口（Q-23）：同一调用方新旧两把
// key 并存期间都可用（轮换不断流）。
func TestCallerRotationDualKey(t *testing.T) {
	wire := callersWire("harness", "ck-new", time.Now().Add(24*time.Hour),
		[]string{"gpt-4o-mini"}, []string{"primary"}, 0, nil)
	wire["keys"] = []map[string]any{
		{"key_hash": sha256hex("ck-old"), "expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)},
		{"key_hash": sha256hex("ck-new"), "expires_at": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)},
	}
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil, map[string]any{
		"callers": []map[string]any{wire},
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))

	for _, key := range []string{"ck-old", "ck-new"} {
		resp := e.postJSON("/v1/chat/completions", chatHelloBody,
			map[string]string{"Authorization": "Bearer " + key})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("轮换窗口 key %s = %d，期望 200", key, resp.StatusCode)
		}
		readBody(t, resp)
	}
}

// TestCallerKeyDecoupledFromUpstreamToken 断言解耦（统一 SDD 8.1）：调用方 API key
// 只做调用方鉴权；发给上游的**永远是网关自有令牌**（⛔ 调用方凭据不外发）。
func TestCallerKeyDecoupledFromUpstreamToken(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil, map[string]any{
		"callers": []map[string]any{callersWire("harness", "ck-alpha", time.Now().Add(24*time.Hour),
			[]string{"gpt-4o-mini"}, []string{"primary"}, 0, nil)},
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

// TestCallerContractQuotaFromRegistry 断言契约配额（callers.rate_limit）即调用方限流
// 的唯一来源：登记了配额的调用方打满桶 ⇒ 429 rate_limit + Retry-After；未登记配额
// 的调用方不限流（⛔ 不套默认值）。
func TestCallerContractQuotaFromRegistry(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil, map[string]any{
		"callers": []map[string]any{
			callersWire("contract", "ck-contract", time.Now().Add(24*time.Hour),
				[]string{"gpt-4o-mini"}, []string{"primary"}, 0,
				map[string]any{"tpm": 1200, "rpm": 0, "burst_s": 1}), // 桶深 20，每请求预估 13
			callersWire("plain", "ck-plain", time.Now().Add(24*time.Hour),
				[]string{"gpt-4o-mini"}, []string{"primary"}, 0, nil),
		},
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	auth := map[string]string{"Authorization": "Bearer ck-contract"}

	if resp := e.postJSON("/v1/chat/completions", body, auth); resp.StatusCode != http.StatusOK {
		t.Fatalf("契约 key 首发 = %d，期望 200", resp.StatusCode)
	} else {
		readBody(t, resp)
	}
	resp2 := e.postJSON("/v1/chat/completions", body, auth)
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("契约 key 第二发 = %d，期望 429（契约桶扣穿）", resp2.StatusCode)
	}
	if resp2.Header.Get("Retry-After") == "" {
		t.Error("429 缺 Retry-After 头")
	}
	_, code, _ := errorFields(t, readBody(t, resp2))
	if code != "rate_limit" {
		t.Errorf("code = %q，期望 rate_limit", code)
	}

	plain := map[string]string{"Authorization": "Bearer ck-plain"}
	for i := 0; i < 3; i++ {
		if resp := e.postJSON("/v1/chat/completions", body, plain); resp.StatusCode != http.StatusOK {
			t.Fatalf("未登记配额 key 第 %d 发 = %d，期望 200（⛔ 不套默认值）", i+1, resp.StatusCode)
		} else {
			readBody(t, resp)
		}
	}
}

// TestCallerIdentityFeedsBudget 断言全局预算（denial-of-wallet）按调用方身份累计：
// A 打满预算 ⇒ 429 budget_exceeded；B 的预算独立 ⇒ 放行。
func TestCallerIdentityFeedsBudget(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		// 预算 20：max_tokens=10 时每请求预估 13 ⇒ A 一发后余 7 < 13 ⇒ 触顶；B 独立预算。
		o.Budget = policy.NewBudget(20)
	}, map[string]any{
		"callers": []map[string]any{
			callersWire("alpha", "ck-alpha", time.Now().Add(24*time.Hour),
				[]string{"gpt-4o-mini"}, []string{"primary"}, 0, nil),
			callersWire("beta", "ck-beta", time.Now().Add(24*time.Hour),
				[]string{"gpt-4o-mini"}, []string{"primary"}, 0, nil),
		},
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

// TestCallerConcurrencyGateUnit 断言调用方在途并发位的三态（⭐ 2026-11-26
// 老宅架构定界：并发/配额/流量归 FG —— 调用方维度的并发闸）：
// 0 = 不限恒放行；占满拒绝；release 归还后可再占。
func TestCallerConcurrencyGateUnit(t *testing.T) {
	// 0 = 不限
	free := &callerEntry{}
	for i := 0; i < 10; i++ {
		if !free.tryAcquire() {
			t.Fatalf("maxInFlight=0 应恒放行（第 %d 次）", i)
		}
	}
	free.release() // no-op 不 panic

	// 上限 2：第 3 占拒绝；release 后恢复
	e := &callerEntry{maxInFlight: 2}
	if !e.tryAcquire() || !e.tryAcquire() {
		t.Fatal("前两占应成功")
	}
	if e.tryAcquire() {
		t.Fatal("第 3 占应拒绝（满）")
	}
	if got := e.inFlight.Load(); got != 2 {
		t.Fatalf("inFlight = %d, 期望 2（拒绝不泄漏位）", got)
	}
	e.release()
	if !e.tryAcquire() {
		t.Fatal("release 后应可再占")
	}
}

// TestCallerConcurrencyLimitFromRegistry 接线冒烟：注册表 concurrency_limit
// 进 callerEntry 且首发正常（接线不破坏正常路径；闸逻辑由上面单元钉住）。
func TestCallerConcurrencyLimitFromRegistry(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil, map[string]any{
		"callers": []map[string]any{
			{
				"name":              "conc",
				"keys":              []map[string]any{{"key_hash": sha256hex("ck-conc"), "expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)}},
				"scope":             map[string]any{"models": []string{"gpt-4o-mini"}, "endpoints": []string{"primary"}},
				"concurrency_limit": 1,
			},
		},
	})
	// 构造后 entry 带上限（直接查鉴权面 —— newEnv 内部装配，走一次请求验证不炸）
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	auth := map[string]string{"Authorization": "Bearer ck-conc"}
	if resp := e.postJSON("/v1/chat/completions", body, auth); resp.StatusCode != http.StatusOK {
		t.Fatalf("concurrency_limit=1 首发 = %d，期望 200（接线不破坏正常路径）", resp.StatusCode)
	}
	readBody(t, resp200OK(e, body, auth))
}

// resp200OK 是上面冒烟的第二次请求小工具（保持断言集中）。
func resp200OK(e *env, body string, auth map[string]string) *http.Response {
	return e.postJSON("/v1/chat/completions", body, auth)
}
