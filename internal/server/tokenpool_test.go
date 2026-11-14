// 日期: 2026-11-14 15:38:42.661
// 作者: Zhai Wenjia

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/policy"
	"github.com/zhaiwenjia/FlyingGateway/internal/telemetry"
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
	m := newPoolManager(0, 0, time.Second, nil) // 本地桶关闭 ⇒ 只看冷却
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
	m := newPoolManager(0, 0, time.Second, nil)
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

// ---- 多 key 契约配额（credential_rate_limits：per_key / shared / key_limits）----

// rl 是测试用契约桶**模板**（per_key / key_limits 层：每 key 各建一份实例）。
func rl(tpm, burstS float64) *rlSpec {
	return &rlSpec{tpm: tpm, burstS: burstS}
}

// rlb 是测试用共享桶**实例**（shared 层：全池唯一）。
func rlb(tpm, burstS float64) *policy.RateLimitBuckets {
	return policy.NewRateLimitBuckets(tpm, 0, burstS)
}

// TestPoolPerKeyContractIndependent 断言每 key 一份桶：槽 0 配额扣满后溢到槽 1
// （fill_first 溢出式均衡），两 key 配额互相独立，全满才整体受限。
func TestPoolPerKeyContractIndependent(t *testing.T) {
	t.Setenv("TEST_POOL_PERKEY", "tok-A,tok-B")
	m := newPoolManager(0, 0, time.Second, map[string]*poolLimits{
		"TEST_POOL_PERKEY": {perKey: rl(600, 1)}, // 每 key 桶深 10
	})
	p, err := m.PoolFor("TEST_POOL_PERKEY")
	if err != nil {
		t.Fatalf("PoolFor 失败: %v", err)
	}
	if got := p.Pick(10, time.Now()); !got.OK || got.Slot != 0 {
		t.Fatalf("首发 = %+v，期望槽 0", got)
	}
	// 槽 0 配额已扣满（10/10）⇒ 溢出到槽 1。
	if got := p.Pick(10, time.Now()); !got.OK || got.Slot != 1 {
		t.Fatalf("二发 = %+v，期望槽 1（槽 0 桶空溢出）", got)
	}
	// 两 key 都满 ⇒ 整体受限（不扣减、给等待）。
	got := p.Pick(10, time.Now())
	if got.OK || got.Wait <= 0 {
		t.Fatalf("双 key 配额耗尽应受限: %+v", got)
	}
}

// TestPoolSharedContract 断言全池共享桶：多 key 共享一个配额池（org/项目级），
// 共享桶扣满后即使别的 key 还空着也不放行。
func TestPoolSharedContract(t *testing.T) {
	t.Setenv("TEST_POOL_SHARED", "tok-A,tok-B")
	m := newPoolManager(0, 0, time.Second, map[string]*poolLimits{
		"TEST_POOL_SHARED": {shared: rlb(600, 1)}, // 全池共享桶深 10
	})
	p, err := m.PoolFor("TEST_POOL_SHARED")
	if err != nil {
		t.Fatalf("PoolFor 失败: %v", err)
	}
	if got := p.Pick(10, time.Now()); !got.OK || got.Slot != 0 {
		t.Fatalf("首发 = %+v，期望槽 0", got)
	}
	got := p.Pick(10, time.Now())
	if got.OK {
		t.Fatalf("共享桶空后仍放行（%+v）——多 key 各计一份就超发了", got)
	}
}

// TestPoolKeyLimitsOverride 断言逐 key 覆盖：key_limits 按槽序覆盖 per_key
// （不同订阅档混池），缺省槽用 per_key。
func TestPoolKeyLimitsOverride(t *testing.T) {
	t.Setenv("TEST_POOL_KEYLIM", "tok-A,tok-B")
	m := newPoolManager(0, 0, time.Second, map[string]*poolLimits{
		"TEST_POOL_KEYLIM": {
			perKey:    rl(300, 1),                 // 缺省每 key 桶深 5
			keyLimits: []*rlSpec{rl(600, 1), nil}, // 槽 0 覆盖为 10；槽 1 用缺省 5
		},
	})
	p, err := m.PoolFor("TEST_POOL_KEYLIM")
	if err != nil {
		t.Fatalf("PoolFor 失败: %v", err)
	}
	// 槽 0 覆盖桶深 10 ⇒ 一发 6、二发 4 都走槽 0。
	if got := p.Pick(6, time.Now()); !got.OK || got.Slot != 0 {
		t.Fatalf("首发 = %+v，期望槽 0", got)
	}
	if got := p.Pick(4, time.Now()); !got.OK || got.Slot != 0 {
		t.Fatalf("二发 = %+v，期望仍槽 0（覆盖桶深 10 未满）", got)
	}
	// 槽 0 满 ⇒ 溢到槽 1（缺省桶深 5）。
	if got := p.Pick(5, time.Now()); !got.OK || got.Slot != 1 {
		t.Fatalf("三发 = %+v，期望槽 1（缺省桶深 5）", got)
	}
	if got := p.Pick(1, time.Now()); got.OK {
		t.Fatalf("槽 1 缺省桶（5）也满后应受限: %+v", got)
	}
}

// TestPoolNoHalfCharge 断言组合支付不半扣：共享桶拒绝后，每 key 桶不得被偷扣
// （拒绝风暴会把桶抽干），且共享桶自身也不因拒绝多扣。
func TestPoolNoHalfCharge(t *testing.T) {
	t.Setenv("TEST_POOL_NOHALF", "tok-A")
	m := newPoolManager(0, 0, time.Second, map[string]*poolLimits{
		"TEST_POOL_NOHALF": {shared: rlb(600, 1), perKey: rl(6000, 1)}, // 共享桶深 10 / 每 key 桶深 100
	})
	p, err := m.PoolFor("TEST_POOL_NOHALF")
	if err != nil {
		t.Fatalf("PoolFor 失败: %v", err)
	}
	if got := p.Pick(5, time.Now()); !got.OK {
		t.Fatalf("首发应放行: %+v", got)
	}
	// 共享桶剩 5 < 9 ⇒ 拒；连拒三次。
	for i := 0; i < 3; i++ {
		if got := p.Pick(9, time.Now()); got.OK {
			t.Fatalf("第 %d 次拒绝失败: %+v", i+2, got)
		}
	}
	// 共享桶剩 5 未被拒绝路径偷扣 ⇒ 5 仍可付。
	if got := p.Pick(5, time.Now()); !got.OK {
		t.Fatalf("拒绝路径半扣了共享桶: %+v（剩 5 应可付）", got)
	}
}

// TestPoolKeyLimitsOverflowRejected 断言 key_limits 多于池内令牌数时启动报错
// （逐 key 覆盖按池内顺序一一对应，多出来的是配置错误）。
func TestPoolKeyLimitsOverflowRejected(t *testing.T) {
	t.Setenv("TEST_POOL_OVERFLOW", "tok-A")
	m := newPoolManager(0, 0, time.Second, map[string]*poolLimits{
		"TEST_POOL_OVERFLOW": {keyLimits: []*rlSpec{rl(600, 1), rl(600, 1)}},
	})
	if _, err := m.PoolFor("TEST_POOL_OVERFLOW"); err == nil {
		t.Fatal("key_limits 多于池内令牌数应报错")
	}
}

// TestCredentialRateLimitsMissingEnvFails 断言 key_ref 环境变量取不到值时启动报错
// （⛔ 不静默忽略登记 —— 静默忽略会把「契约配额」变成「不限流」）。
func TestCredentialRateLimitsMissingEnvFails(t *testing.T) {
	reg := writeRegistry(t, []epSpec{primaryEP("http://127.0.0.1:1")}, nil, map[string]any{
		"credential_rate_limits": []map[string]any{
			{"key_ref": "TEST_POOL_KEY_MISSING", "per_key": map[string]any{"tpm": 600, "rpm": 0, "burst_s": 5}},
		},
	})
	_, err := New(Options{
		Registry: reg,
		Tracer:   telemetry.NewTracer(&telemetry.InMemoryExporter{}),
		Metrics:  telemetry.NewMetrics(),
	})
	if err == nil {
		t.Fatal("key_ref 环境变量缺失时 server.New 应报错")
	}
}

// ---- 多 key 核算模型（10 个 key 的限流怎么算：聚合 = min(Σ per-key, shared)）----

// TestPoolTenKeysAccounting 断言核算公式（SDD 7.2 多 key 核算模型）：
//
//	① 只有 per_key：聚合稳态 = N × 单 key（10 key × 每 key 桶深 1 ⇒ 连发 10 次全过、第 11 次受限）；
//	② 加 shared 封顶：聚合 = min(Σ per-key, shared)（共享桶深 5 ⇒ 只过 5 次）。
func TestPoolTenKeysAccounting(t *testing.T) {
	keys := "k0,k1,k2,k3,k4,k5,k6,k7,k8,k9"
	// ① per_key only：桶深 1（refill 0.1/s，测试内补充可忽略），est=1。
	t.Setenv("TEST_POOL_TEN_A", keys)
	m := newPoolManager(0, 0, time.Second, map[string]*poolLimits{
		"TEST_POOL_TEN_A": {perKey: rl(6, 10)}, // cap = 0.1×10 = 1
	})
	p, _ := m.PoolFor("TEST_POOL_TEN_A")
	for i := 0; i < 10; i++ {
		if got := p.Pick(1, time.Now()); !got.OK || got.Slot != i {
			t.Fatalf("第 %d 发 = %+v，期望槽 %d（每 key 一份桶）", i+1, got, i)
		}
	}
	if got := p.Pick(1, time.Now()); got.OK {
		t.Fatalf("第 11 发应受限（聚合 = 10 key × 1 = 10）: %+v", got)
	}

	// ② shared 封顶：同 10 key，加共享桶深 5 ⇒ 聚合 = min(10, 5) = 5。
	t.Setenv("TEST_POOL_TEN_B", keys)
	m2 := newPoolManager(0, 0, time.Second, map[string]*poolLimits{
		"TEST_POOL_TEN_B": {perKey: rl(6, 10), shared: rlb(6, 50)}, // shared cap = 0.1×50 = 5
	})
	p2, _ := m2.PoolFor("TEST_POOL_TEN_B")
	for i := 0; i < 5; i++ {
		if got := p2.Pick(1, time.Now()); !got.OK {
			t.Fatalf("第 %d 发应过（共享桶深 5）: %+v", i+1, got)
		}
	}
	if got := p2.Pick(1, time.Now()); got.OK {
		t.Fatalf("第 6 发应受限（min(Σ per-key=10, shared=5) = 5）: %+v", got)
	}
}

// TestAdminCredentialPools 断言管理面配额观测：只含桶状态与槽序、⛔ 无令牌明文。
func TestAdminCredentialPools(t *testing.T) {
	t.Setenv("TEST_POOL_VIEW", "tok-secret-A,tok-secret-B")
	m := newPoolManager(0, 0, time.Second, map[string]*poolLimits{
		"TEST_POOL_VIEW": {perKey: rl(600, 1), shared: rlb(600, 1)},
	})
	if _, err := m.PoolFor("TEST_POOL_VIEW"); err != nil {
		t.Fatalf("PoolFor 失败: %v", err)
	}
	s := &Server{pools: m}
	rec := httptest.NewRecorder()
	s.handleAdminCredentialPools(rec, httptest.NewRequest("GET", "/admin/credential-pools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"credential_ref":"TEST_POOL_VIEW"`) ||
		!strings.Contains(body, `"contract_tokens"`) ||
		!strings.Contains(body, `"shared_tokens"`) {
		t.Fatalf("观测缺桶状态字段: %s", body[:200])
	}
	if strings.Contains(body, "tok-secret") {
		t.Fatalf("观测泄露令牌明文: %s", body)
	}
}
