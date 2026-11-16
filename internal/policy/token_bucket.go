// 日期: 2026-11-14 11:50:04.639
// 作者: Zhai Wenjia

package policy

import (
	"math"
	"sync"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/share"
)

// TokenBucket 是 token 级令牌桶限流（统一 SDD 7.2 / 8.3）。
// 桶容量以归一化 token 为单位、补充速率以 token/秒为单位 —— ⛔ 不只按请求数限流，
// 否则挡不住按 token 放大的成本与滥用（Denial-of-Wallet）。
// 计数口径：normalized = InputTokens + OutputTokens（缓存命中与写入计入
// InputTokens 不打折，见 core.Usage.NormalizedTokens）。
//
// 双模式（⛔ 不同混用）：内存模式（store == nil，单实例）与**共享模式**（store 非 nil，
// 桶状态存 share.Store、跨实例一份配额）。共享模式故障口径：存储不可用 ⇒
// **fail-open 放行**（限流不可用时业务可用性优先）+ onShareError 显式告警，⛔ 不静默。
//
// 并发安全：Allow 可并发调用。
type TokenBucket struct {
	mu           sync.Mutex
	capacity     float64
	refillPerSec float64
	tokens       float64
	lastRefill   time.Time

	// 共享模式字段（store 非 nil 时生效；内存字段不再使用）。
	store        share.Store
	skey         string
	onShareError func(error)
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

// NewSharedTokenBucket 构造共享模式令牌桶（桶状态存 store，跨实例一份配额）。
// onShareError 是存储故障回调（warn 告警注入；nil = 不报）。
func NewSharedTokenBucket(store share.Store, skey string, onShareError func(error), capacity int64, refillPerSec float64) *TokenBucket {
	if store == nil {
		panic("policy: NewSharedTokenBucket 的 store 不得为 nil")
	}
	if capacity <= 0 {
		panic("policy: TokenBucket 容量必须 > 0")
	}
	if refillPerSec <= 0 {
		panic("policy: TokenBucket 补充速率必须 > 0")
	}
	return &TokenBucket{
		capacity:     float64(capacity),
		refillPerSec: refillPerSec,
		store:        store,
		skey:         skey,
		onShareError: onShareError,
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
	if b.store != nil {
		return b.allowShared(tokens)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if ok, after := b.hasLocked(tokens); !ok {
		return false, after
	}
	b.takeLocked(tokens)
	return true, 0
}

// allowShared 共享模式扣减（桶状态在 share.Store，原子「可付才扣」）。
// 故障口径：存储不可用 ⇒ fail-open 放行 + onShareError 显式告警（⛔ 不静默）；
// 拒绝时 retryAfter 给「补足本次需求」的时长上界（共享模式不读余额，估上界安全）。
func (b *TokenBucket) allowShared(tokens int64) (bool, time.Duration) {
	if float64(tokens) > b.capacity {
		return false, b.refillEstimate(tokens) // 超桶深恒拒
	}
	ok, err := b.store.TryTake(time.Now(), []share.Take{{
		Key: b.skey, Capacity: int64(b.capacity), RefillPerSec: b.refillPerSec, Need: tokens,
	}})
	if err != nil {
		if b.onShareError != nil {
			b.onShareError(err)
		}
		return true, 0 // fail-open：限流不可用时业务可用性优先（显式告警已发）
	}
	if !ok {
		return false, b.refillEstimate(tokens)
	}
	return true, 0
}

// refillEstimate 返回补足 need 个 token 的时长（按补充速率的上界估计）。
func (b *TokenBucket) refillEstimate(need int64) time.Duration {
	if b.refillPerSec <= 0 {
		return 0
	}
	return time.Duration(float64(need) / b.refillPerSec * float64(time.Second))
}

// Snapshot 返回桶状态（管理面只读视图）。
// 共享模式从 store 读余额（Remaining = -1 表示存储不可用，观测显式标未知）。
func (b *TokenBucket) Snapshot() BucketState {
	if b.store != nil {
		tokens, err := b.store.BucketTokens(b.skey, int64(b.capacity), b.refillPerSec, time.Now())
		if err != nil {
			if b.onShareError != nil {
				b.onShareError(err)
			}
			return BucketState{Remaining: -1, Capacity: b.capacity, RefillPerSec: b.refillPerSec}
		}
		return BucketState{Remaining: tokens, Capacity: b.capacity, RefillPerSec: b.refillPerSec}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotLocked()
}

// Has 非扣减探查：tokens 个 token 现在能否放行（含补足等待时长）。
// 供复合多级限流（RateLimitBuckets 与「共享桶 + 每 key 桶」组合）做「同时可付才扣」的预查。
//
// 共享模式：预检**退化**（跨实例无损预检必有竞态窗口）——超桶深恒拒照判，
// 其余情形由 Allow 的原子扣减兜底判定（探后扣窗口内的过量会被 TryTake 原子拒绝）。
func (b *TokenBucket) Has(tokens int64) (ok bool, retryAfter time.Duration) {
	if b.store != nil {
		if tokens <= 0 {
			return false, 0
		}
		if float64(tokens) > b.capacity {
			return false, b.refillEstimate(tokens)
		}
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hasLocked(tokens)
}

// BucketState 是一层桶的观测快照（管理面只读视图；⛔ 不含任何凭据内容）。
type BucketState struct {
	// Remaining 是当前剩余（token 数或请求数）。
	Remaining float64 `json:"remaining"`
	// Capacity 是桶深（突发上限）。
	Capacity float64 `json:"capacity"`
	// RefillPerSec 是补充速率（稳态上限）。
	RefillPerSec float64 `json:"refill_per_sec"`
}

// snapshotLocked 返回桶状态（须持 b.mu；先补后读，与 Allow/Has 同一口径）。
func (b *TokenBucket) snapshotLocked() BucketState {
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.refillPerSec)
		b.lastRefill = now
	}
	return BucketState{Remaining: b.tokens, Capacity: b.capacity, RefillPerSec: b.refillPerSec}
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
