// 日期: 2026-11-14 22:18:40.774
// 作者: Zhai Wenjia

package policy

import (
	"testing"
	"time"
)

// TestNewRateLimitBuckets_Unlimited 断言两维都未登记 ⇒ nil（不限流，⛔ 不建空壳桶）。
func TestNewRateLimitBuckets_Unlimited(t *testing.T) {
	if b := NewRateLimitBuckets(0, 0, 5); b != nil {
		t.Fatal("tpm/rpm 均为 0 时应返回 nil（不限流）")
	}
}

// TestNewRateLimitBuckets_PanicWithoutBurst 断言 burst_s <= 0 属配置错误直接 panic
// （⛔ 不静默按默认值建桶 —— 桶深来自真实客户端扇出登记，不猜）。
func TestNewRateLimitBuckets_PanicWithoutBurst(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("burst_s <= 0 应 panic")
		}
	}()
	_ = NewRateLimitBuckets(600, 0, 0)
}

// TestRateLimitBuckets_TokenDimension 断言 token 维的桶深/补充速率推导
// （桶深 = tpm/60 × burst_s，补充 = tpm/60 token/s）与拒绝语义。
func TestRateLimitBuckets_TokenDimension(t *testing.T) {
	b := NewRateLimitBuckets(600, 0, 1) // refill 10/s，桶深 10
	if got := b.tokens.capacity; got != 10 {
		t.Fatalf("token 桶深 = %v，期望 10", got)
	}
	if ok, _ := b.Allow(10); !ok {
		t.Fatal("桶满时 Allow(10) 应放行")
	}
	ok, wait := b.Allow(1)
	if ok {
		t.Fatal("桶空时 Allow(1) 应拒绝")
	}
	if wait <= 0 {
		t.Fatal("拒绝应给出 >0 的 Retry-After")
	}
	// 等补充 ≥1 token 后应恢复（refill 10/s ⇒ ~120ms）。
	time.Sleep(120 * time.Millisecond)
	if ok, _ := b.Allow(1); !ok {
		t.Fatal("补充后 Allow(1) 应放行")
	}
}

// TestRateLimitBuckets_RequestDimension 断言请求维独立生效（rpm 桶每请求扣 1）。
func TestRateLimitBuckets_RequestDimension(t *testing.T) {
	b := NewRateLimitBuckets(0, 60, 1) // 请求维桶深 1
	if ok, _ := b.Allow(500); !ok {
		t.Fatal("首发放行（token 维不限、请求维有位）")
	}
	if ok, _ := b.Allow(500); ok {
		t.Fatal("请求维桶空时第二发应拒绝（哪怕 token 维不限）")
	}
}

// TestRateLimitBuckets_NoHalfCharge 断言复合拒绝不半扣（桶是消耗品：并发下
// 「同时可付才扣」，拒绝路径吃掉配额会在拒绝风暴里把桶抽干）。
func TestRateLimitBuckets_NoHalfCharge(t *testing.T) {
	b := NewRateLimitBuckets(6000, 60, 1) // token 桶深 100（refill 100/s），请求桶深 1
	if ok, _ := b.Allow(50); !ok {
		t.Fatal("首发应放行")
	}
	// 连拒三次（请求维满）；token 桶状态不得被扣（允许少量补充）。
	for i := 0; i < 3; i++ {
		if ok, _ := b.Allow(50); ok {
			t.Fatalf("第 %d 发应被请求维拒绝", i+2)
		}
	}
	if got := b.tokens.tokens; got < 50 || got > 53 {
		t.Fatalf("拒绝路径被半扣：token 桶余量 = %v，期望 ≈50（±补充）", got)
	}
	// 请求维补充后（refill 1/s ⇒ 1.1s）应能放行大额（token 桶 ≥50）。
	time.Sleep(1100 * time.Millisecond)
	if ok, _ := b.Allow(50); !ok {
		t.Fatal("请求维补充后 Allow(50) 应放行（token 桶未被拒绝路径偷扣）")
	}
}
