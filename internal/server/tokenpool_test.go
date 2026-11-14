// 日期: 2026-11-14 15:38:42.661
// 作者: Zhai Wenjia

package server

import (
	"net/http"
	"testing"
	"time"
)

// 本文件测试上游自有令牌池与限流轮换（⛔ 与调用方 key 解耦）：
// 多令牌拆分、冷却跳过、全部受限取等待时间最短、上游 429 自动换令牌、
// 轮换不吃重试预算、等待超上限显式 429（⛔ 不盲发）。

// TestSplitTokens 断言多令牌凭据拆分（逗号 / 换行、去空白、去重、保序）。
func TestSplitTokens(t *testing.T) {
	got := splitTokens("tok-A, tok-B\ntok-A\n\ntok-C,")
	want := []string{"tok-A", "tok-B", "tok-C"}
	if len(got) != len(want) {
		t.Fatalf("splitTokens = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitTokens[%d] = %q，期望 %q（去重保序）", i, got[i], want[i])
		}
	}
	if len(splitTokens("  ")) != 0 {
		t.Error("空白值应拆出 0 个令牌")
	}
}

// TestPoolPickSkipsCooledTokens 断言选令牌口径：跳过冷却中的令牌；
// 全部冷却 ⇒ 取等待时间最短者（⛔ 不扣减桶、⛔ 不盲发）。
func TestPoolPickSkipsCooledTokens(t *testing.T) {
	t.Setenv("TEST_POOL_UNIT", "tok-A,tok-B")
	m := newPoolManager(0, 0, time.Second) // 本地桶关闭 ⇒ 只看冷却
	p, err := m.PoolFor("TEST_POOL_UNIT")
	if err != nil {
		t.Fatalf("PoolFor 失败: %v", err)
	}
	if p.Size() != 2 {
		t.Fatalf("池大小 = %d，期望 2", p.Size())
	}

	// 初始：稳定槽序取 tok-A。
	if got := p.Pick(10, time.Now()); !got.OK || got.Token != "tok-A" {
		t.Fatalf("首选 = %+v，期望 tok-A（稳定槽序、可复现）", got)
	}
	// 冷却 A ⇒ 选 B。
	p.MarkRateLimited(0, 5*time.Second)
	if got := p.Pick(10, time.Now()); !got.OK || got.Token != "tok-B" {
		t.Fatalf("A 冷却后 = %+v，期望 tok-B（跳过受限令牌）", got)
	}
	// B 也冷却（1s < 5s）⇒ 全部受限，取等待最短的 B（1s）。
	p.MarkRateLimited(1, 1*time.Second)
	got := p.Pick(10, time.Now())
	if got.OK {
		t.Fatalf("全部冷却后仍返回可用：%+v", got)
	}
	if got.Token != "tok-B" {
		t.Errorf("全部受限的选择 = %q，期望 tok-B（等待时间最短）", got.Token)
	}
	if got.Wait < 500*time.Millisecond || got.Wait > 1200*time.Millisecond {
		t.Errorf("最短等待 = %v，期望约 1s（B 的冷却剩余）", got.Wait)
	}
	if p.HasAvailable(time.Now()) {
		t.Error("HasAvailable 应为 false（全部在冷却）")
	}
}

// TestUpstreamTokenRotationOn429 断言限流轮换（本任务核心）：
// 自有令牌 tok-A 被上游 429 ⇒ 自动换未限流的 tok-B 重打并成功；
// max_retries=0 ⇒ 轮换 ⛔ 不吃重试预算（独立机制）。
func TestUpstreamTokenRotationOn429(t *testing.T) {
	t.Setenv("TEST_KEY_POOL", "tok-A,tok-B")
	e := newEnv(t, func(base string) []epSpec {
		ep := primaryEP(base)
		ep.credRef = "TEST_KEY_POOL"
		ep.maxRetries = 0 // 轮换必须不吃重试预算
		return []epSpec{ep}
	}, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	e.up.setRateLimit("Bearer tok-A", 1, 5) // tok-A 限流一次（Retry-After 5s）

	resp := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（轮换到 tok-B 成功）", resp.StatusCode)
	}
	readBody(t, resp)

	if n, _, _, _ := e.up.stats(); n != 2 {
		t.Fatalf("上游调用数 = %d，期望 2（tok-A 429 一次 + tok-B 成功一次）", n)
	}
	if got := e.up.headersAt(0).Get("Authorization"); got != "Bearer tok-A" {
		t.Errorf("第 1 发 Authorization = %q，期望 Bearer tok-A", got)
	}
	if got := e.up.headersAt(1).Get("Authorization"); got != "Bearer tok-B" {
		t.Errorf("第 2 发 Authorization = %q，期望轮换后的 Bearer tok-B", got)
	}
}

// TestAllTokensLimitedPicksShortestWait 断言「所有令牌都限流时切换到等待时间最短的令牌」：
// A/B 双双被 429（A 冷却 5s、B 冷却 1s）⇒ 下一请求等最短的 B 恢复后用 B 发出。
func TestAllTokensLimitedPicksShortestWait(t *testing.T) {
	t.Setenv("TEST_KEY_POOL", "tok-A,tok-B")
	e := newEnv(t, func(base string) []epSpec {
		ep := primaryEP(base)
		ep.credRef = "TEST_KEY_POOL"
		ep.maxRetries = 0
		return []epSpec{ep}
	}, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	e.up.setRateLimit("Bearer tok-A", 1, 5) // A 冷却 5s
	e.up.setRateLimit("Bearer tok-B", 1, 1) // B 冷却 1s（更短）

	// 第一请求：A 429 → 轮换 B 429 → 全部受限且无可轮换 ⇒ 显式 429（⛔ 不盲发）。
	resp := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("第一请求 = %d，期望 429（全部令牌受限）", resp.StatusCode)
	}
	readBody(t, resp)
	if n, _, _, _ := e.up.stats(); n != 2 {
		t.Fatalf("上游调用数 = %d，期望 2（A、B 各一发）", n)
	}

	// 第二请求：全部冷却 ⇒ 等待时间最短的 B（≈1s）恢复后用 B 发出。
	t0 := time.Now()
	resp2 := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	elapsed := time.Since(t0)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("第二请求 = %d，期望 200（等最短冷却后用 tok-B）", resp2.StatusCode)
	}
	readBody(t, resp2)
	if elapsed < 500*time.Millisecond {
		t.Errorf("第二请求耗时 %v，期望 ≥ 500ms（确实在等 B 的最短冷却）", elapsed)
	}
	if n, _, _, _ := e.up.stats(); n != 3 {
		t.Fatalf("上游调用数 = %d，期望 3", n)
	}
	if got := e.up.headersAt(2).Get("Authorization"); got != "Bearer tok-B" {
		t.Errorf("第 3 发 Authorization = %q，期望 Bearer tok-B（等待时间最短的令牌）", got)
	}
}

// TestAllTokensLimitedWaitExceedsCap 断言等待超上限的显式拒绝：
// 最短等待 > MaxTokenWait ⇒ 429 + Retry-After（= 最短等待），⛔ 不盲发上游。
func TestAllTokensLimitedWaitExceedsCap(t *testing.T) {
	t.Setenv("TEST_KEY_POOL", "tok-A,tok-B")
	e := newEnv(t, func(base string) []epSpec {
		ep := primaryEP(base)
		ep.credRef = "TEST_KEY_POOL"
		ep.maxRetries = 0
		return []epSpec{ep}
	}, nil, func(o *Options) {
		o.MaxTokenWait = 100 * time.Millisecond
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	e.up.setRateLimit("Bearer tok-A", 1, 5)
	e.up.setRateLimit("Bearer tok-B", 1, 5)

	// 先把 A/B 都打进冷却（一轮轮换各试一次）。
	resp := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	readBody(t, resp)
	nAfterFirst, _, _, _ := e.up.stats()

	// 全部冷却且最短等待 5s > 上限 100ms ⇒ 429 token_pool_exhausted，上游不再收发。
	resp2 := e.postJSON("/v1/chat/completions", chatHelloBody, nil)
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("第二请求 = %d，期望 429（等待超上限）", resp2.StatusCode)
	}
	if resp2.Header.Get("Retry-After") == "" {
		t.Error("缺 Retry-After（= 最短等待）")
	}
	_, code, _ := errorFields(t, readBody(t, resp2))
	if code != codeTokenPoolExhausted {
		t.Errorf("code = %q，期望 %q（本地池闸、非上游回执）", code, codeTokenPoolExhausted)
	}
	if n, _, _, _ := e.up.stats(); n != nAfterFirst {
		t.Errorf("上游调用数 %d → %d，期望不变（⛔ 不盲发）", nAfterFirst, n)
	}
}

// TestSingleTokenPoolBackwardCompat 断言单令牌值兼容（池退化为 1 槽，无轮换但语义一致）。
func TestSingleTokenPoolBackwardCompat(t *testing.T) {
	t.Setenv("TEST_POOL_SINGLE", "only-tok")
	m := newPoolManager(0, 0, time.Second)
	p, err := m.PoolFor("TEST_POOL_SINGLE")
	if err != nil {
		t.Fatalf("PoolFor 失败: %v", err)
	}
	if p.Size() != 1 {
		t.Fatalf("池大小 = %d，期望 1（单值兼容）", p.Size())
	}
	if got := p.Pick(10, time.Now()); !got.OK || got.Token != "only-tok" {
		t.Fatalf("Pick = %+v，期望 only-tok", got)
	}
	p.MarkRateLimited(0, time.Second)
	if p.HasAvailable(time.Now()) {
		t.Error("单令牌冷却后应无可用令牌")
	}
}
