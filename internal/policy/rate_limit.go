// 日期: 2026-11-14 21:50:33.031
// 作者: Zhai Wenjia

package policy

import (
	"sync"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/share"
)

// RateLimitBuckets 是「套餐契约配额」的双维令牌桶（token 维 + 请求维，任一不足即拒 ——
// LiteLLM rpm+tpm 同款口径）。
//
// 锚点分离（统一 SDD 7.2）：限流锚定**上游套餐契约**（tpm / rpm / burst_s 由部署方按契约登记），
// ⛔ 不设拍脑袋默认值 —— 未登记配额的维度不限流。原全局默认桶（容量 60000 / 补充
// 10000 tokens/s）在小请求形态下实测造出 ≈454 rps 的隐藏吞吐天花板（P3 阶梯 c=256 档
// ok=2269 / fail=193858 全部 429，与 10000÷22≈454 吻合），属「nobody can discover 的数字」
// （调研见 docs/20261114-限流过载-调研-业界限流锚点与容量保护.md）。
//
// 两维分工：token 维防 Denial-of-Wallet（大 prompt 放大成本），请求维防连接洪水。
// 并发安全：Allow 可并发调用（内部互斥保证双桶「同时可付才扣」，⛔ 不做半扣）。
// 双模式：内存模式（store == nil）/ 共享模式（子桶为共享 TokenBucket，Allow 走
// share.Store 的多桶原子 TryTake；故障 fail-open + 显式告警，见 TokenBucket 注释）。
type RateLimitBuckets struct {
	mu       sync.Mutex
	tokens   *TokenBucket // nil = token 维不限
	requests *TokenBucket // nil = 请求维不限（每请求扣 1）

	onShareError func(error) // 共享存储故障回调（共享模式用）
}

// NewRateLimitBuckets 由契约配额构造双维桶：
//   - tpm = tokens/min（归一化 token 计数口径），补充速率 = tpm/60 token/s；
//   - rpm = requests/min，补充速率 = rpm/60 请求/s；
//   - burstS = 桶深折算秒数（桶深 = 速率 × burst_s，即突发上限来自真实客户端扇出形态）。
//
// tpm <= 0 ⇒ token 维不限；rpm <= 0 ⇒ 请求维不限；两者均 ≤ 0 ⇒ 返回 nil（不限流）。
func NewRateLimitBuckets(tpm, rpm, burstS float64) *RateLimitBuckets {
	if burstS <= 0 {
		panic("policy: RateLimitBuckets burst_s 必须 > 0")
	}
	b := &RateLimitBuckets{}
	if tpm > 0 {
		refill := tpm / 60
		b.tokens = NewTokenBucket(int64(refill*burstS), refill)
	}
	if rpm > 0 {
		refill := rpm / 60
		b.requests = NewTokenBucket(int64(refill*burstS), refill)
	}
	if b.tokens == nil && b.requests == nil {
		return nil
	}
	return b
}

// NewSharedRateLimitBuckets 构造共享模式双维桶（子桶状态存 store，跨实例一份配额）。
// skey 是共享键前缀（token 维 = skey+":t"、请求维 = skey+":r"）。
func NewSharedRateLimitBuckets(store share.Store, skey string, onShareError func(error), tpm, rpm, burstS float64) *RateLimitBuckets {
	if store == nil {
		panic("policy: NewSharedRateLimitBuckets 的 store 不得为 nil")
	}
	if burstS <= 0 {
		panic("policy: RateLimitBuckets burst_s 必须 > 0")
	}
	b := &RateLimitBuckets{onShareError: onShareError}
	if tpm > 0 {
		refill := tpm / 60
		b.tokens = NewSharedTokenBucket(store, skey+":t", onShareError, int64(refill*burstS), refill)
	}
	if rpm > 0 {
		refill := rpm / 60
		b.requests = NewSharedTokenBucket(store, skey+":r", onShareError, int64(refill*burstS), refill)
	}
	if b.tokens == nil && b.requests == nil {
		return nil
	}
	return b
}

// sharedStore 返回共享存储（内存模式 ⇒ nil）。
func (b *RateLimitBuckets) sharedStore() share.Store {
	if b.tokens != nil && b.tokens.store != nil {
		return b.tokens.store
	}
	if b.requests != nil && b.requests.store != nil {
		return b.requests.store
	}
	return nil
}

// Snapshot 返回 token 维与请求维的桶状态（nil 维 = 该维不限流）。
func (b *RateLimitBuckets) Snapshot() (tokens, requests *BucketState) {
	if b.sharedStore() != nil {
		if b.tokens != nil {
			st := b.tokens.Snapshot()
			tokens = &st
		}
		if b.requests != nil {
			st := b.requests.Snapshot()
			requests = &st
		}
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens != nil {
		st := b.tokens.snapshotLocked()
		tokens = &st
	}
	if b.requests != nil {
		st := b.requests.snapshotLocked()
		requests = &st
	}
	return
}

// Has 非扣减探查：一次调用（est 个 token + 1 个请求）现在能否支付（含补足等待）。
// 供「共享桶 + 每 key 桶」两级组合做预查 —— 两级同时可付才各自 Allow（⛔ 不半扣）。
func (b *RateLimitBuckets) Has(est int64) (ok bool, retryAfter time.Duration) {
	if store := b.sharedStore(); store != nil {
		// 共享模式预检退化（同 TokenBucket.Has 口径）：超桶深恒拒照判，其余由 Allow 兜底。
		if b.tokens != nil {
			if okT, w := b.tokens.Has(est); !okT {
				return false, w
			}
		}
		if b.requests != nil {
			if okR, w := b.requests.Has(1); !okR {
				return false, w
			}
		}
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var waitTok, waitReq time.Duration
	if b.tokens != nil {
		if okT, w := b.tokens.hasLocked(est); !okT {
			waitTok = w
		}
	}
	if b.requests != nil {
		if okR, w := b.requests.hasLocked(1); !okR {
			waitReq = w
		}
	}
	if waitTok > 0 || waitReq > 0 {
		wait := waitTok
		if waitReq > wait {
			wait = waitReq
		}
		return false, wait
	}
	return true, 0
}

// Allow 尝试支付一次调用（est 个归一化 token + 1 个请求）。
// 双维**同时可付才扣减**（⛔ 不半扣 —— 半扣会在拒绝路径上白白吃掉配额）。
// 放行 ⇒ ok=true；拒绝 ⇒ ok=false 且 retryAfter 是两维补足等待的较大者（供 429 + Retry-After）。
func (b *RateLimitBuckets) Allow(est int64) (ok bool, retryAfter time.Duration) {
	if store := b.sharedStore(); store != nil {
		// 共享模式：双维一次 TryTake 原子扣（⛔ 不半扣）；故障 fail-open + 显式告警。
		var takes []share.Take
		if b.tokens != nil {
			takes = append(takes, share.Take{
				Key: b.tokens.skey, Capacity: int64(b.tokens.capacity),
				RefillPerSec: b.tokens.refillPerSec, Need: est,
			})
		}
		if b.requests != nil {
			takes = append(takes, share.Take{
				Key: b.requests.skey, Capacity: int64(b.requests.capacity),
				RefillPerSec: b.requests.refillPerSec, Need: 1,
			})
		}
		ok, err := store.TryTake(time.Now(), takes)
		if err != nil {
			if b.onShareError != nil {
				b.onShareError(err)
			}
			return true, 0 // fail-open（显式告警已发，⛔ 不静默）
		}
		if !ok {
			var wait time.Duration
			if b.tokens != nil {
				wait = b.tokens.refillEstimate(est)
			}
			if b.requests != nil {
				if w := b.requests.refillEstimate(1); w > wait {
					wait = w
				}
			}
			return false, wait
		}
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	var waitTok, waitReq time.Duration
	if b.tokens != nil {
		if okT, w := b.tokens.hasLocked(est); !okT {
			waitTok = w
		}
	}
	if b.requests != nil {
		if okR, w := b.requests.hasLocked(1); !okR {
			waitReq = w
		}
	}
	if waitTok > 0 || waitReq > 0 {
		wait := waitTok
		if waitReq > wait {
			wait = waitReq
		}
		return false, wait
	}
	if b.tokens != nil {
		b.tokens.takeLocked(est)
	}
	if b.requests != nil {
		b.requests.takeLocked(1)
	}
	return true, 0
}
