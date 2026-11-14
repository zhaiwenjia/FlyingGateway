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
	// bucket 是本地令牌桶（容量 / 速率 ≤ 0 时为 nil = 不做本地限流）。
	bucket *policy.TokenBucket

	mu sync.Mutex
	// cooldownUntil 是上游 429 冷却截止时刻（此前不选该令牌）。
	cooldownUntil time.Time
}

// cooling 返回该令牌是否在冷却期（now 时刻），以及剩余冷却时长。
func (s *tokenSlot) cooling(now time.Time) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.cooldownUntil) {
		return true, s.cooldownUntil.Sub(now)
	}
	return false, 0
}

// markRateLimited 记一次上游限流冷却（取已有截止与新截止的较大者 —— 重复 429 不缩短）。
func (s *tokenSlot) markRateLimited(now time.Time, d time.Duration) {
	if d <= 0 {
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
			if slot.bucket == nil {
				return PickResult{Slot: i, Token: slot.token, OK: true}
			}
			if ok, retryAfter := slot.bucket.Allow(est); ok {
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
}

// newPoolManager 构造池管理器。
func newPoolManager(bucketCapacity int64, bucketRefill float64, defaultCooldown time.Duration) *poolManager {
	if defaultCooldown <= 0 {
		defaultCooldown = 30 * time.Second
	}
	return &poolManager{
		pools:           map[string]*tokenPool{},
		bucketCapacity:  bucketCapacity,
		bucketRefill:    bucketRefill,
		defaultCooldown: defaultCooldown,
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
	p := &tokenPool{slots: make([]*tokenSlot, 0, len(tokens))}
	for _, tok := range tokens {
		slot := &tokenSlot{token: tok}
		if m.bucketCapacity > 0 && m.bucketRefill > 0 {
			slot.bucket = policy.NewTokenBucket(m.bucketCapacity, m.bucketRefill)
		}
		p.slots = append(p.slots, slot)
	}
	m.pools[credentialRef] = p
	return p, nil
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
