// 日期: 2026-11-16 23:39:48.270
// 作者: Zhai Wenjia

package share

import (
	"fmt"
	"sync"
	"time"
)

// MemoryStore 是进程内内存实现（单实例形态；语义与 RedisStore 逐条对齐，
// 供单实例部署与 Redis 故障回退共用）。
type MemoryStore struct {
	mu    sync.Mutex
	bucks map[string]*memBucket // 令牌桶状态
	used  map[string]int64      // 预算累计
	cools map[string]time.Time  // 冷却截止
}

type memBucket struct {
	tokens   float64
	lastFill time.Time
	inited   bool
}

// NewMemoryStore 构造内存存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		bucks: map[string]*memBucket{},
		used:  map[string]int64{},
		cools: map[string]time.Time{},
	}
}

// TryTake 全部可付才扣（先全量预检、后统一扣减，⛔ 不半扣）。
func (m *MemoryStore) TryTake(now time.Time, takes []Take) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 预检：逐桶算 refill 后余额，全部 ≥ need 才动。
	for _, t := range takes {
		if m.fillLocked(t.Key, t.Capacity, t.RefillPerSec, now) < float64(t.Need) {
			return false, nil
		}
	}
	for _, t := range takes {
		m.bucks[t.Key].tokens -= float64(t.Need)
	}
	return true, nil
}

// fillLocked 计算并落桶的补充（调用方持锁），返回补充后余额。
func (m *MemoryStore) fillLocked(key string, cap int64, refill float64, now time.Time) float64 {
	b, ok := m.bucks[key]
	if !ok {
		b = &memBucket{tokens: float64(cap), lastFill: now, inited: true}
		m.bucks[key] = b
		return b.tokens
	}
	if !b.inited {
		b.tokens = float64(cap)
		b.lastFill = now
		b.inited = true
		return b.tokens
	}
	elapsed := now.Sub(b.lastFill).Seconds()
	if elapsed > 0 && refill > 0 {
		b.tokens += elapsed * refill
		if max := float64(cap); b.tokens > max {
			b.tokens = max
		}
		b.lastFill = now
	}
	return b.tokens
}

// Get 返回预算累计值。
func (m *MemoryStore) Get(key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used[key], nil
}

// IncrBy 原子累计；limit > 0 且超限 ⇒ 回滚并报超限。
func (m *MemoryStore) IncrBy(key string, delta, limit int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.used[key] + delta
	if limit > 0 && v > limit {
		return m.used[key], fmt.Errorf("share: 预算超限（key=%s 现值 %d + %d > 上限 %d，已回滚）", key, m.used[key], delta, limit)
	}
	m.used[key] = v
	return v, nil
}

// SetCooldown 记冷却截止。
func (m *MemoryStore) SetCooldown(key string, until time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 重复冷却不缩短（与 tokenpool.markRateLimited 同口径）。
	if old, ok := m.cools[key]; ok && old.After(until) {
		return nil
	}
	m.cools[key] = until
	return nil
}

// CooldownUntil 读冷却截止（无 ⇒ 零值）。
func (m *MemoryStore) CooldownUntil(key string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cools[key], nil
}
