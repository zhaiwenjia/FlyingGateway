// 日期: 2026-11-14 11:50:04.793
// 作者: Zhai Wenjia

package policy

import (
	"testing"
	"time"
)

// TestTokenBucket_TokenGranularity 断言按 token 计而非按请求数
// （统一 SDD 11.3「token 级限流」：两次小请求 ≠ 一次大请求的判定差异）。
//
// 断言有效性（11.5「错误实现会红」对照点）：
//   - 「只按请求数」的错误实现（两次请求都放行）会在 b1 的第二次 Allow 上红；
//   - 「不扣减只计数」的错误实现会在 b2（两次 40 应都放行）上红。
func TestTokenBucket_TokenGranularity(t *testing.T) {
	// b1：容量 100，两次 60 —— 第二次超出 ⇒ 拒（按请求数的实现会错误放行）。
	b1 := NewTokenBucket(100, 0.001)
	if ok, _ := b1.Allow(60); !ok {
		t.Fatal("首次 Allow(60) 应放行")
	}
	ok, retryAfter := b1.Allow(60)
	if ok {
		t.Fatal("第二次 Allow(60)（累计 120 > 容量 100）应拒绝")
	}
	if retryAfter <= 0 {
		t.Fatalf("拒绝时 retryAfter 应 > 0（供上层出 Retry-After），实际 %v", retryAfter)
	}

	// b2：同样容量，两次 40 —— 两次都该放行（恒拒的错误实现会红）。
	b2 := NewTokenBucket(100, 0.001)
	if ok, _ := b2.Allow(40); !ok {
		t.Fatal("b2 Allow(40) 应放行")
	}
	if ok, _ := b2.Allow(40); !ok {
		t.Fatal("b2 第二次 Allow(40)（累计 80 ≤ 100）应放行")
	}

	// b3：一次 120 超容量 ⇒ 恒拒（含等待后也不放行的语义由调用方拆请求）。
	b3 := NewTokenBucket(100, 1000)
	if ok, _ := b3.Allow(120); ok {
		t.Fatal("Allow(120) 超容量 100 应拒绝")
	}
}

// TestTokenBucket_Refill 断言补充速率生效（等待后重新放行）。
// 断言有效性：从不补充的错误实现会红。观测量=等待后 Allow 的结果。
func TestTokenBucket_Refill(t *testing.T) {
	b := NewTokenBucket(100, 1000) // 1000 token/s
	if ok, _ := b.Allow(100); !ok {
		t.Fatal("Allow(100) 应放行")
	}
	if ok, _ := b.Allow(50); ok {
		t.Fatal("桶空时 Allow(50) 应拒绝")
	}
	time.Sleep(50 * time.Millisecond) // 约补充 50 token
	if ok, _ := b.Allow(20); !ok {
		t.Fatal("等待补充后 Allow(20) 应放行")
	}
}

// TestNewTokenBucket_Panic 断言非法配置显式 panic（⛔ 不静默按 0 处理）。
func TestNewTokenBucket_Panic(t *testing.T) {
	for _, c := range []struct {
		name     string
		capacity int64
		refill   float64
	}{
		{"容量 0", 0, 1},
		{"容量负", -1, 1},
		{"速率 0", 10, 0},
		{"速率负", 10, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("非法配置未 panic")
				}
			}()
			NewTokenBucket(c.capacity, c.refill)
		})
	}
}
