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
	want := float64(tokens)

	b.mu.Lock()
	defer b.mu.Unlock()

	// 按经过时间补充（封顶 capacity）。
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.refillPerSec)
		b.lastRefill = now
	}

	if want <= b.tokens {
		b.tokens -= want
		return true, 0
	}
	// 拒绝：算出补足缺口所需时长（向上取整到毫秒，保证 > 0）。
	need := want - b.tokens
	after := time.Duration(math.Ceil(need/b.refillPerSec*1000)) * time.Millisecond
	if after <= 0 {
		after = time.Millisecond
	}
	return false, after
}
