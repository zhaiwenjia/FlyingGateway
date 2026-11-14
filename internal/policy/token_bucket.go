// 日期: 2026-11-14 11:50:04.639
// 作者: Zhai Wenjia

package policy

import (
	"math"
	"sync"
	"time"
)

// TokenBucket 是 token 级令牌桶限流（统一 SDD 7.2 / 8.3）。
// 桶容量以归一化 token 为单位、补充速率以 token/秒为单位 —— ⛔ 不只按请求数限流，
// 否则挡不住按 token 放大的成本与滥用（Denial-of-Wallet）。
// 计数口径：normalized = InputTokens + OutputTokens（缓存命中与写入计入
// InputTokens 不打折，见 core.Usage.NormalizedTokens）。
//
// 并发安全：Allow 可并发调用。
type TokenBucket struct {
	mu           sync.Mutex
	capacity     float64
	refillPerSec float64
	tokens       float64
	lastRefill   time.Time
}

// NewTokenBucket 构造令牌桶。
// capacity <= 0 或 refillPerSec <= 0 属配置错误，直接 panic —— ⛔ 不静默按 0 处理
// （静默 0 会把「限流全拒 / 限流全放」伪装成正常运行）。
func NewTokenBucket(capacity int64, refillPerSec float64) *TokenBucket {
	if capacity <= 0 {
		panic("policy: TokenBucket 容量必须 > 0")
	}
	if refillPerSec <= 0 {
		panic("policy: TokenBucket 补充速率必须 > 0")
	}
	return &TokenBucket{
		capacity:     float64(capacity),
		refillPerSec: refillPerSec,
		tokens:       float64(capacity),
		lastRefill:   time.Now(),
	}
}

// Allow 尝试扣减 tokens 个归一化 token。
// 放行 ⇒ ok=true（已扣减）；拒绝 ⇒ ok=false 且 retryAfter 是「还需等多久才够」，
// 供上层出 `429` + `Retry-After`（统一 SDD 2.3 治理三件套）。
//
// 边界：
//   - tokens <= 0 属误用，不放行（retryAfter=0）；
//   - tokens > capacity 的请求永远放不进桶 ⇒ 恒拒绝（调用方应拆分请求），
//     retryAfter 仍给出按补充速率补足所需时长。
func (b *TokenBucket) Allow(tokens int64) (ok bool, retryAfter time.Duration) {
	if tokens <= 0 {
		return false, 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if ok, after := b.hasLocked(tokens); !ok {
		return false, after
	}
	b.takeLocked(tokens)
	return true, 0
}

// Has 非扣减探查：tokens 个 token 现在能否放行（含补足等待时长）。
// 供复合多级限流（RateLimitBuckets 与「共享桶 + 每 key 桶」组合）做「同时可付才扣」的预查。
func (b *TokenBucket) Has(tokens int64) (ok bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hasLocked(tokens)
}

// hasLocked 非扣减探查（须持 b.mu）：先按经过时间补充（封顶 capacity），
// 再判 want 是否可付；不足时给出补足等待（向上取整到毫秒，保证拒绝时 > 0）。
// 供 Allow 与复合桶（RateLimitBuckets「同时可付才扣」）共用同一套补充/判定口径。
func (b *TokenBucket) hasLocked(tokens int64) (bool, time.Duration) {
	if tokens <= 0 {
		return false, 0
	}
	want := float64(tokens)

	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.refillPerSec)
		b.lastRefill = now
	}

	if want <= b.tokens {
		return true, 0
	}
	need := want - b.tokens
	after := time.Duration(math.Ceil(need/b.refillPerSec*1000)) * time.Millisecond
	if after <= 0 {
		after = time.Millisecond
	}
	return false, after
}

// takeLocked 扣减 tokens 个 token（须持 b.mu；调用前须经 hasLocked 确认可付）。
func (b *TokenBucket) takeLocked(tokens int64) {
	b.tokens -= float64(tokens)
}
