// 日期: 2026-11-14 15:34:25.462
// 作者: Zhai Wenjia

package server

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/policy"
	"github.com/zhaiwenjia/FlyingGateway/internal/security"
	"github.com/zhaiwenjia/FlyingGateway/internal/share"
)

// 本文件承载上游自有令牌池与限流轮换（⛔ 与调用方 API key 解耦 —— 上游永远用网关
// 自有令牌，调用方凭据只做调用方鉴权与计费口径）：
//   - 每个 credential_ref 一池；池值来自环境变量（8.1 凭据只从来源引用解析），
//     多令牌以英文逗号 / 换行分隔，单令牌 = 单槽池（向后兼容）；
//   - 每令牌一个本地令牌桶（policy.TokenBucket，可关）+ 上游 429 冷却（Retry-After
//     优先，无头取默认冷却）；
//   - 选令牌：先取「未冷却且桶可支付」的令牌（稳定槽序，决策可复现）；
//     全部受限 ⇒ 取**等待时间最短**的令牌（max(桶等待, 冷却剩余) 最小者），
//     等待 ≤ MaxTokenWait ⇒ 等满即用；超上限 ⇒ 429 + Retry-After（⛔ 不盲发）。

// tokenSlot 是池内一个自有令牌。
type tokenSlot struct {
	token string
	// bucket 是本地令牌桶（全局兜底，容量 / 速率 ≤ 0 时为 nil = 不启用）。
	bucket *policy.TokenBucket
	// contract 是该 key 的契约桶（credential_rate_limits 的 per_key / key_limits 层；
	// nil = 不限流，锚点分离：配额数字只来自套餐登记）。
	contract *policy.RateLimitBuckets

	// 共享模式（store 非 nil）：冷却截止存 share.Store（跨实例一份冷却）；
	// 内存字段 cooldownUntil 仅在 store == nil 时生效。
	store        share.Store
	coolKey      string
	onShareError func(error)

	mu sync.Mutex
	// cooldownUntil 是上游 429 冷却截止时刻（此前不选该令牌）。
	cooldownUntil time.Time
}

// cooling 返回该令牌是否在冷却期（now 时刻），以及剩余冷却时长。
// 共享模式读 store（存储不可用 ⇒ fail-open 视为不冷却 + 显式告警，⛔ 不静默）。
func (s *tokenSlot) cooling(now time.Time) (bool, time.Duration) {
	if s.store != nil {
		until, err := s.store.CooldownUntil(s.coolKey)
		if err != nil {
			if s.onShareError != nil {
				s.onShareError(err)
			}
			return false, 0 // fail-open（告警已发）
		}
		if now.Before(until) {
			return true, until.Sub(now)
		}
		return false, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.cooldownUntil) {
		return true, s.cooldownUntil.Sub(now)
	}
	return false, 0
}

// markRateLimited 记一次上游限流冷却（取已有截止与新截止的较大者 —— 重复 429 不缩短；
// 共享模式「只增不减」由 share.Store.SetCooldown 的原子语义保证）。
func (s *tokenSlot) markRateLimited(now time.Time, d time.Duration) {
	if d <= 0 {
		return
	}
	if s.store != nil {
		if err := s.store.SetCooldown(s.coolKey, now.Add(d)); err != nil && s.onShareError != nil {
			s.onShareError(err)
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	until := now.Add(d)
	if until.After(s.cooldownUntil) {
		s.cooldownUntil = until
	}
}

// tokenPool 是一个 credential_ref 的自有令牌池。
type tokenPool struct {
	slots []*tokenSlot
	// shared 是全池共享契约桶（credential_rate_limits 的 shared 层：org/项目级配额池，
	// 多 key 共享一个配额；nil = 不限流）。
	shared *policy.RateLimitBuckets
	// chargeMu 串行化「同时可付才扣」的组合支付（共享桶 + 每 key 桶 + 全局兜底桶，
	// ⛔ 不半扣 —— 先 Has 预查全过再 Allow 真扣）。
	chargeMu sync.Mutex
}

// pay 组合支付一次调用（est 归一化 token）：三级桶全部可付才扣减。
// 任一不足 ⇒ 全不扣并返回补足等待（多级并联的判定与计数口径，统一 SDD 7.2）。
func (p *tokenPool) pay(est int64, slot *tokenSlot) (bool, time.Duration) {
	p.chargeMu.Lock()
	defer p.chargeMu.Unlock()
	var wait time.Duration
	step := func(ok bool, w time.Duration) bool {
		if !ok {
			if w > wait {
				wait = w
			}
			return false
		}
		return true
	}
	if p.shared != nil {
		if ok, w := p.shared.Has(est); !step(ok, w) {
			return false, wait
		}
	}
	if slot.contract != nil {
		if ok, w := slot.contract.Has(est); !step(ok, w) {
			return false, wait
		}
	}
	if slot.bucket != nil {
		if ok, w := slot.bucket.Has(est); !step(ok, w) {
			return false, wait
		}
	}
	if p.shared != nil {
		p.shared.Allow(est)
	}
	if slot.contract != nil {
		slot.contract.Allow(est)
	}
	if slot.bucket != nil {
		slot.bucket.Allow(est)
	}
	return true, 0
}

// Size 返回池内令牌数（轮换次数上界）。
func (p *tokenPool) Size() int { return len(p.slots) }

// PickResult 是一次选令牌的结果。
type PickResult struct {
	// Slot 是槽下标（MarkRateLimited 用）。
	Slot int
	// Token 是选中的自有令牌明文（只进请求头，⛔ 不进日志）。
	Token string
	// Wait 是「全部受限」时的最短等待（ok=false 时非零）。
	Wait time.Duration
	// OK = 找到可立即使用的令牌。
	OK bool
}

// Pick 选令牌（统一 SDD 24 轮换口径）：
//   - 可用轮：未冷却且本地桶能支付 est（Allow 成功即扣减 —— 选中即计额）；
//   - 全部受限：取 max(桶等待, 冷却剩余) 最小的令牌（等待时间最短），
//     不扣减、不消费 —— 等满后由调用方重新 Pick。
//
// 槽序稳定（数组序）⇒ 同输入同输出（决策可复现）。
func (p *tokenPool) Pick(est int64, now time.Time) PickResult {
	best := PickResult{Slot: -1, OK: false, Wait: 0}
	bestWait := time.Duration(0)
	for i, slot := range p.slots {
		cooling, remain := slot.cooling(now)
		var bucketWait time.Duration
		if !cooling {
			// 先探后扣（pay 内部 Has 全过才 Allow）：桶空的槽跳过试下一槽 ——
			// 前槽配额扣满自动溢到后槽（fill_first 溢出式均衡，多 key 池化共识模型）。
			if ok, retryAfter := p.pay(est, slot); ok {
				return PickResult{Slot: i, Token: slot.token, OK: true}
			} else {
				bucketWait = retryAfter
			}
		}
		// 该槽不可用：登记它的可用等待（max(桶等待, 冷却剩余)）。
		w := bucketWait
		if remain > w {
			w = remain
		}
		if best.Slot < 0 || w < bestWait {
			best = PickResult{Slot: i, Token: slot.token, OK: false, Wait: w}
			bestWait = w
		}
	}
	return best
}

// HasAvailable 返回是否存在未冷却令牌（轮换决策用；⛔ 不扣减桶）。
// 注：只看冷却状态 —— 桶额度由 Pick 的 Allow 试扣为准。
func (p *tokenPool) HasAvailable(now time.Time) bool {
	for _, slot := range p.slots {
		if cooling, _ := slot.cooling(now); !cooling {
			return true
		}
	}
	return false
}

// MarkRateLimited 记某槽被上游限流（Retry-After 冷却）。
func (p *tokenPool) MarkRateLimited(slot int, d time.Duration) {
	if slot < 0 || slot >= len(p.slots) {
		return
	}
	p.slots[slot].markRateLimited(time.Now(), d)
}

// poolManager 按 credential_ref 管理令牌池（惰性构建 + 缓存）。
type poolManager struct {
	mu    sync.Mutex
	pools map[string]*tokenPool

	// bucketCapacity / bucketRefill 是每令牌本地桶参数（≤ 0 = 不启用本地限流）。
	bucketCapacity int64
	bucketRefill   float64
	// defaultCooldown 是上游 429 无 Retry-After 时的默认冷却。
	defaultCooldown time.Duration
	// limits 是每池契约配额（credential_rate_limits；未登记的池不限流）。
	limits map[string]*poolLimits
	// store 非 nil ⇒ 共享模式（冷却与桶状态跨实例一致；slot 构造时注入）。
	store share.Store
	// onShareError 是共享存储故障回调（warn 告警注入；nil = 不报）。
	onShareError func(error)
}

// poolLimits 是一个上游令牌池的契约配额（credential_rate_limits 三层，统一 SDD 7.2 锚点分离）。
// ⛔ 与调用方令牌无关：调用方 key 由外部项目签发（管理本系统消费额度），永不发给上游；
// 本结构只管**上游自有令牌**（供应商多 key）的套餐配额。
type poolLimits struct {
	// perKey 是每 key 桶的**模板**（⚠️ 非共享实例）：每个 key 各建一份桶 ——
	// N 个 key = N 份套餐容量（业界多 key 池化共识模型）。
	perKey *rlSpec
	// shared 是全池唯一共享桶（org/项目级配额池：多 key 共享一个配额）。
	shared *policy.RateLimitBuckets
	// keyLimits 是逐 key 覆盖模板（按池内 key 顺序；nil 项用 perKey）。
	keyLimits []*rlSpec

	// ref 是 credential_ref 名（共享键拼装用）；store 非 nil ⇒ 共享模式。
	ref          string
	store        share.Store
	onShareError func(error)
}

// rlSpec 是契约桶参数模板（登记值；每 key 装配时各建一份桶）。
type rlSpec struct {
	tpm, rpm, burstS float64
}

// build 构造一份桶实例（每 key 独立实例 —— ⛔ 不跨 key 共享，共享语义只走 shared 层）。
func (s *rlSpec) build() *policy.RateLimitBuckets {
	return policy.NewRateLimitBuckets(s.tpm, s.rpm, s.burstS)
}

// buildShared 构造共享模式桶（状态存 store，跨实例一份配额）。
func (s *rlSpec) buildShared(store share.Store, skey string, onShareError func(error)) *policy.RateLimitBuckets {
	return policy.NewSharedRateLimitBuckets(store, skey, onShareError, s.tpm, s.rpm, s.burstS)
}

// bucketsFor 为槽 i 构造契约桶（key_limits 按槽序覆盖优先，缺省用 perKey 模板）。
func (l *poolLimits) bucketsFor(i int) *policy.RateLimitBuckets {
	if l == nil {
		return nil
	}
	spec := l.perKey
	if i < len(l.keyLimits) && l.keyLimits[i] != nil {
		spec = l.keyLimits[i]
	}
	if spec == nil {
		return nil
	}
	if l.store != nil {
		return spec.buildShared(l.store, fmt.Sprintf("fg:rl:poolkey:%s:%d", l.ref, i), l.onShareError)
	}
	return spec.build()
}

// newPoolManager 构造池管理器。
func newPoolManager(bucketCapacity int64, bucketRefill float64, defaultCooldown time.Duration, limits map[string]*poolLimits, store share.Store, onShareError func(error)) *poolManager {
	if defaultCooldown <= 0 {
		defaultCooldown = 30 * time.Second
	}
	if limits == nil {
		limits = map[string]*poolLimits{}
	}
	return &poolManager{
		pools:           map[string]*tokenPool{},
		bucketCapacity:  bucketCapacity,
		bucketRefill:    bucketRefill,
		defaultCooldown: defaultCooldown,
		limits:          limits,
		store:           store,
		onShareError:    onShareError,
	}
}

// DefaultRateLimitCooldown 是上游 429 无 Retry-After 时的默认冷却（可配）。
const DefaultRateLimitCooldown = 30 * time.Second

// DefaultMaxTokenWait 是「全部令牌受限」时的最长等待上限（可配；超上限改回 429）。
const DefaultMaxTokenWait = 3 * time.Second

// defaultCooldownFor 取默认冷却时长。
func (m *poolManager) defaultCooldownFor() time.Duration {
	if m == nil || m.defaultCooldown <= 0 {
		return DefaultRateLimitCooldown
	}
	return m.defaultCooldown
}

// PoolFor 取（并按需构建）指定来源引用的令牌池。
// 来源值 = 环境变量里的凭据：多令牌以英文逗号 / 换行分隔，单值即单令牌池。
func (m *poolManager) PoolFor(credentialRef string) (*tokenPool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pools[credentialRef]; ok {
		return p, nil
	}
	value, err := security.ResolveCredential(credentialRef)
	if err != nil {
		return nil, err
	}
	tokens := splitTokens(value)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("server: 凭据来源 %s 解析出 0 个令牌（值为空）", credentialRef)
	}
	lim := m.limits[credentialRef]
	if lim != nil && len(lim.keyLimits) > len(tokens) {
		return nil, fmt.Errorf("server: credential_rate_limits 的 key_limits 有 %d 条，多于 %s 池内令牌数 %d（逐 key 覆盖按池内顺序一一对应）",
			len(lim.keyLimits), credentialRef, len(tokens))
	}
	p := &tokenPool{slots: make([]*tokenSlot, 0, len(tokens))}
	if lim != nil {
		p.shared = lim.shared
	}
	for i, tok := range tokens {
		slot := &tokenSlot{token: tok, store: m.store, onShareError: m.onShareError,
			coolKey: fmt.Sprintf("fg:cool:%s:%d", credentialRef, i)}
		if m.bucketCapacity > 0 && m.bucketRefill > 0 {
			if m.store != nil {
				slot.bucket = policy.NewSharedTokenBucket(m.store,
					fmt.Sprintf("fg:rl:pool:%s:%d:g", credentialRef, i), m.onShareError, m.bucketCapacity, m.bucketRefill)
			} else {
				slot.bucket = policy.NewTokenBucket(m.bucketCapacity, m.bucketRefill)
			}
		}
		slot.contract = lim.bucketsFor(i)
		p.slots = append(p.slots, slot)
	}
	m.pools[credentialRef] = p
	return p, nil
}

// ---- 池观测（管理面只读视图：多 key 限流「怎么算」的运行面证据；⛔ 无令牌明文）----

// keySlotState 是池内一个 key 槽的观测状态（⚠️ 只有槽序，⛔ 不含令牌内容）。
type keySlotState struct {
	Slot               int                 `json:"slot"`
	Cooling            bool                `json:"cooling"`
	CoolingRemainingMS int64               `json:"cooling_remaining_ms,omitempty"`
	ContractTokens     *policy.BucketState `json:"contract_tokens,omitempty"`
	ContractRequests   *policy.BucketState `json:"contract_requests,omitempty"`
	GlobalTokens       *policy.BucketState `json:"global_tokens,omitempty"`
}

// poolSnapshot 是一个令牌池的观测状态（共享桶 + 逐 key 槽）。
type poolSnapshot struct {
	CredentialRef  string              `json:"credential_ref"`
	KeyCount       int                 `json:"key_count"`
	SharedTokens   *policy.BucketState `json:"shared_tokens,omitempty"`
	SharedRequests *policy.BucketState `json:"shared_requests,omitempty"`
	Keys           []keySlotState      `json:"keys"`
}

// Snapshot 返回全部**已构建**池的状态（惰性构建：尚未用过的池不在视图内）。
func (m *poolManager) Snapshot() []poolSnapshot {
	m.mu.Lock()
	pools := make([]*tokenPool, 0, len(m.pools))
	refs := make([]string, 0, len(m.pools))
	for ref, p := range m.pools {
		refs = append(refs, ref)
		pools = append(pools, p)
	}
	m.mu.Unlock()
	out := make([]poolSnapshot, 0, len(pools))
	for i, p := range pools {
		ps := poolSnapshot{CredentialRef: refs[i], KeyCount: len(p.slots)}
		if p.shared != nil {
			ps.SharedTokens, ps.SharedRequests = p.shared.Snapshot()
		}
		now := time.Now()
		for j, slot := range p.slots {
			ks := keySlotState{Slot: j}
			if cooling, remain := slot.cooling(now); cooling {
				ks.Cooling = true
				ks.CoolingRemainingMS = remain.Milliseconds()
			}
			if slot.contract != nil {
				ks.ContractTokens, ks.ContractRequests = slot.contract.Snapshot()
			}
			if slot.bucket != nil {
				st := slot.bucket.Snapshot()
				ks.GlobalTokens = &st
			}
			ps.Keys = append(ps.Keys, ks)
		}
		out = append(out, ps)
	}
	return out
}

// splitTokens 切分多令牌凭据（英文逗号 / 换行分隔；去空白、去空段、去重、保持原序）。
func splitTokens(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	seen := map[string]bool{}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
