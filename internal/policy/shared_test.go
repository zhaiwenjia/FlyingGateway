// 日期: 2026-11-16 23:39:48.270
// 作者: Zhai Wenjia

package policy

import (
	"os"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/share"
)

// redisStoreForTest 连本地 Redis（REDIS_ADDR 可覆盖）；不可达 ⇒ 显式 Skip。
func redisStoreForTest(t *testing.T) *share.RedisStore {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rs := share.NewRedisStore(addr, os.Getenv("REDIS_PASSWORD"), 2*time.Second, 4)
	if _, err := rs.Get("fgtest:ping"); err != nil {
		t.Skipf("redis 不可达（%s）：%v —— 共享集成测试显式跳过", addr, err)
	}
	return rs
}

// TestSharedRateLimitAcrossInstances 钉住「多实例共享一份配额」（统一 SDD 7.2 多实例一致）：
// 两个 RateLimitBuckets 实例同键同参数连同一 Redis ⇒ 一实例扣掉的配额另一实例看得见
// （总量封顶 = 契约配额，⛔ 不再 N 实例 = N 倍配额）。
// 错误实现对照：共享没接上（各算各的）⇒ 断言红。
func TestSharedRateLimitAcrossInstances(t *testing.T) {
	rs := redisStoreForTest(t)
	prefix := "fgtest:rl:" + time.Now().Format("150405.000000000")
	a := NewSharedRateLimitBuckets(rs, prefix, nil, 600, 60, 1) // token 桶深 10（600/60*1）、rpm 桶深 1
	b := NewSharedRateLimitBuckets(rs, prefix, nil, 600, 60, 1)
	if a == nil || b == nil {
		t.Fatal("共享桶构造失败")
	}
	// 请求维桶深 1（rpm=60 ⇒ 1 req/s × burst 1s）：A 扣 1 次后 B 第一次必拒（共享同一请求桶）。
	if ok, _ := a.Allow(1); !ok {
		t.Fatal("A 首次应放行")
	}
	if ok, _ := b.Allow(1); ok {
		t.Fatal("B 应被拒（请求维共享桶已被 A 扣空，⛔ 不再各算各的）")
	}
}

// TestSharedRateLimitTotalCapped 共享总量封顶：两实例交替扣，总放行数 = 桶深（⛔ 超发）。
func TestSharedRateLimitTotalCapped(t *testing.T) {
	rs := redisStoreForTest(t)
	prefix := "fgtest:rlcap:" + time.Now().Format("150405.000000000")
	// rpm=120、burst=1 ⇒ 请求桶深 2；token 维不设（0）。
	a := NewSharedRateLimitBuckets(rs, prefix, nil, 0, 120, 1)
	b := NewSharedRateLimitBuckets(rs, prefix, nil, 0, 120, 1)
	passed := 0
	for i := 0; i < 6; i++ {
		ins := a
		if i%2 == 1 {
			ins = b
		}
		if ok, _ := ins.Allow(1); ok {
			passed++
		}
	}
	if passed != 2 {
		t.Fatalf("两实例合计放行 = %d，期望 2（共享桶深 2，⛔ N 倍配额）", passed)
	}
}

// TestSharedBudgetAcrossInstances 共享预算跨实例累计：A 记账 90 后 B 的 Allow(20) 被拒。
func TestSharedBudgetAcrossInstances(t *testing.T) {
	rs := redisStoreForTest(t)
	prefix := "fgtest:budget:" + time.Now().Format("150405.000000000") + ":"
	a := NewSharedBudget(rs, prefix, nil, 100)
	b := NewSharedBudget(rs, prefix, nil, 100)
	a.Record("client-x", 90)
	if err := b.Allow("client-x", 20); err == nil {
		t.Fatal("B 应拒（A 记的 90 跨实例可见，90+20 > 100）")
	}
	if err := b.Allow("client-x", 10); err != nil {
		t.Fatalf("B Allow(10) 应放行（恰好用满），实际 %v", err)
	}
}

// TestSharedModeParityMemoryVsRedis 同一断言电池跑内存模式与共享模式（语义对齐实证）。
func TestSharedModeParityMemoryVsRedis(t *testing.T) {
	rs := redisStoreForTest(t)
	prefix := "fgtest:parity:" + time.Now().Format("150405.000000000")
	mem := NewRateLimitBuckets(600, 60, 1) // 内存：token 桶深 10、请求桶深 1
	sh := NewSharedRateLimitBuckets(rs, prefix, nil, 600, 60, 1)
	// 同操作序列：1 放行、2 放行（token 10-2=8）、…按请求数 1 的桶深 ⇒ 第 2 次拒。
	memOK := []bool{}
	shOK := []bool{}
	for i := 0; i < 3; i++ {
		okM, _ := mem.Allow(1)
		okS, _ := sh.Allow(1)
		memOK = append(memOK, okM)
		shOK = append(shOK, okS)
	}
	for i := range memOK {
		if memOK[i] != shOK[i] {
			t.Fatalf("第 %d 步行为不一致：内存=%v 共享=%v（memOK=%v shOK=%v）",
				i+1, memOK[i], shOK[i], memOK, shOK)
		}
	}
}

// TestSharedTokenBucketParity 单桶双模式对齐（TokenBucket.allowShared 的观察面）：
// 内存桶与共享桶同操作序列行为一致。
func TestSharedTokenBucketParity(t *testing.T) {
	rs := redisStoreForTest(t)
	prefix := "fgtest:tb:" + time.Now().Format("150405.000000000")
	mem := NewTokenBucket(10, 1)
	sh := NewSharedTokenBucket(rs, prefix, nil, 10, 1)
	for i := 0; i < 12; i++ {
		okM, _ := mem.Allow(3)
		okS, _ := sh.Allow(3)
		if okM != okS {
			t.Fatalf("第 %d 步不一致：内存=%v 共享=%v", i+1, okM, okS)
		}
	}
}
