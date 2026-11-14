// 日期: 2026-11-14 21:50:33.031
// 作者: Zhai Wenjia

package policy

import (
	"sync"
	"time"
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
type RateLimitBuckets struct {
	mu       sync.Mutex
	tokens   *TokenBucket // nil = token 维不限
	requests *TokenBucket // nil = 请求维不限（每请求扣 1）
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

// Allow 尝试支付一次调用（est 个归一化 token + 1 个请求）。
// 双维**同时可付才扣减**（⛔ 不半扣 —— 半扣会在拒绝路径上白白吃掉配额）。
// 放行 ⇒ ok=true；拒绝 ⇒ ok=false 且 retryAfter 是两维补足等待的较大者（供 429 + Retry-After）。
func (b *RateLimitBuckets) Allow(est int64) (ok bool, retryAfter time.Duration) {
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
