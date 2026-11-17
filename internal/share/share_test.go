// 日期: 2026-11-16 23:39:48.270
// 作者: Zhai Wenjia

package share

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMemoryStoreTryTake 双桶「同时可付才扣」（统一 SDD 7.2）：
// 两桶足 ⇒ 扣；一桶不足 ⇒ 整拒且**不扣任何桶**（⛔ 半扣）；refill 随时间补充、容量封顶。
// 错误实现对照：半扣（扣了足的那个桶）⇒ 第 3 断言红。
func TestMemoryStoreTryTake(t *testing.T) {
	m := NewMemoryStore()
	t0 := time.Now()
	takes := []Take{
		{Key: "tk", Capacity: 100, RefillPerSec: 10, Need: 30},
		{Key: "rq", Capacity: 5, RefillPerSec: 1, Need: 1},
	}
	// 三次成功扣减（token 100→70→40→10）。
	for i := 0; i < 3; i++ {
		ok, err := m.TryTake(t0, takes)
		if err != nil || !ok {
			t.Fatalf("第 %d 次扣减应成功：ok=%v err=%v", i+1, ok, err)
		}
	}
	// 第四次：token 余额 10 < 30 ⇒ 整拒。
	ok, err := m.TryTake(t0, takes)
	if err != nil || ok {
		t.Fatalf("token 不足应整拒：ok=%v err=%v", ok, err)
	}
	// 半扣检查：请求桶余额应仍可扣（= 拒绝时未动）——单独只扣请求桶应成功。
	ok, err = m.TryTake(t0, []Take{{Key: "rq", Capacity: 5, RefillPerSec: 1, Need: 1}})
	if err != nil || !ok {
		t.Fatalf("整拒后请求桶应未被扣（单独扣应成功）：ok=%v err=%v", ok, err)
	}
	// 半扣场景 2（观察面）：第二桶不足 ⇒ 第一桶也不许扣（⛔ 半扣）。
	tk2 := []Take{
		{Key: "tk2", Capacity: 100, RefillPerSec: 0, Need: 30},
		{Key: "rq2", Capacity: 1, RefillPerSec: 0, Need: 1},
	}
	if ok, _ := m.TryTake(t0, tk2); !ok {
		t.Fatal("tk2/rq2 首扣应成功")
	}
	if ok, _ := m.TryTake(t0, tk2); ok {
		t.Fatal("rq2 耗尽 ⇒ 整拒")
	}
	// 若半扣发生（tk2 被扣 30 剩 40）⇒ 50 拒；正确实现 tk2=70 ⇒ 过。
	if ok, _ := m.TryTake(t0, []Take{{Key: "tk2", Capacity: 100, RefillPerSec: 0, Need: 50}}); !ok {
		t.Fatal("整拒后 tk2 应仍有 70（半扣则只剩 40 ⇒ 拒）")
	}
	// refill：+2s ⇒ token 10 + 2*10 = 30 ⇒ 恰好可付 30。
	ok, err = m.TryTake(t0.Add(2*time.Second), takes)
	if err != nil || !ok {
		t.Fatalf("refill 后应可付：ok=%v err=%v", ok, err)
	}
	// 容量封顶：+100s ⇒ token 封顶 100（不是 100+）。
	ok, err = m.TryTake(t0.Add(200*time.Second), takes[:1])
	if err != nil || !ok {
		t.Fatalf("封顶后应可付：ok=%v err=%v", ok, err)
	}
	ok, err = m.TryTake(t0.Add(200*time.Second), takes[:1])
	if err != nil || !ok {
		t.Fatalf("封顶后第二次（100-30=70 ≥ 30）应可付：ok=%v err=%v", ok, err)
	}
	ok, _ = m.TryTake(t0.Add(200*time.Second), takes[:1])
	ok2, _ := m.TryTake(t0.Add(200*time.Second), takes[:1])
	if ok && ok2 {
		// 100-30-30-30=10 < 30 ⇒ 第三次必拒。
		t.Fatal("容量封顶 100 下三次 30 扣减后不应还有 30")
	}
}

// TestMemoryStoreIncrBy 预算累计：超限 ⇒ 回滚（现值不动）且报错。
func TestMemoryStoreIncrBy(t *testing.T) {
	m := NewMemoryStore()
	v, err := m.IncrBy("budget:a", 30, 100)
	if err != nil || v != 30 {
		t.Fatalf("首次累计 = %d err=%v，期望 30", v, err)
	}
	v, err = m.IncrBy("budget:a", 60, 100)
	if err != nil || v != 90 {
		t.Fatalf("二次累计 = %d err=%v，期望 90", v, err)
	}
	// 超限：90 + 20 > 100 ⇒ 回滚，现值仍 90。
	if _, err := m.IncrBy("budget:a", 20, 100); err == nil {
		t.Fatal("超限应报错")
	}
	if v, _ := m.Get("budget:a"); v != 90 {
		t.Fatalf("超限后现值 = %d，期望 90（回滚）", v)
	}
}

// TestMemoryStoreCooldown 冷却只增不减（⛔ 重复 429 缩短冷却）。
func TestMemoryStoreCooldown(t *testing.T) {
	m := NewMemoryStore()
	t0 := time.Now()
	if err := m.SetCooldown("cool:a", t0.Add(30*time.Second)); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	if err := m.SetCooldown("cool:a", t0.Add(10*time.Second)); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	until, _ := m.CooldownUntil("cool:a")
	if !until.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("冷却截止 = %v，期望 +30s（⛔ 被 10s 缩短）", until)
	}
}

// TestRedisStoreAgainstRealServer 真实 Redis 电池（127.0.0.1:6379，REDIS_ADDR 可覆盖）：
// 与 MemoryStore 同一断言电池逐条对齐（语义一致性实证）；Redis 不可达 ⇒ 显式 Skip。
func TestRedisStoreAgainstRealServer(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rs := NewRedisStore(addr, os.Getenv("REDIS_PASSWORD"), 2*time.Second, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := rs.c.Do(ctx, "PING"); err != nil {
		t.Skipf("redis 不可达（%s）：%v —— 集成测试显式跳过", addr, err)
	}
	prefix := fmt.Sprintf("fgtest:%d:", time.Now().UnixNano())
	t.Cleanup(func() {
		// 清理测试键（尽力而为）。
		for _, k := range []string{prefix + "tk", prefix + "rq", prefix + "tk2", prefix + "rq2", prefix + "budget", prefix + "cool"} {
			_, _ = rs.c.Do(context.Background(), "DEL", k)
		}
	})

	t0 := time.Now()
	takes := []Take{
		{Key: prefix + "tk", Capacity: 100, RefillPerSec: 10, Need: 30},
		{Key: prefix + "rq", Capacity: 5, RefillPerSec: 1, Need: 1},
	}
	for i := 0; i < 3; i++ {
		ok, err := rs.TryTake(t0, takes)
		if err != nil || !ok {
			t.Fatalf("第 %d 次扣减应成功：ok=%v err=%v", i+1, ok, err)
		}
	}
	ok, err := rs.TryTake(t0, takes)
	if err != nil || ok {
		t.Fatalf("token 不足应整拒：ok=%v err=%v", ok, err)
	}
	// 半扣检查：请求桶未被动。
	ok, err = rs.TryTake(t0, []Take{{Key: prefix + "rq", Capacity: 5, RefillPerSec: 1, Need: 1}})
	if err != nil || !ok {
		t.Fatalf("整拒后请求桶应未被扣：ok=%v err=%v", ok, err)
	}
	// 半扣场景 2（观察面）：第二桶不足 ⇒ 第一桶也不许扣（⛔ 半扣）。
	tk2 := []Take{
		{Key: prefix + "tk2", Capacity: 100, RefillPerSec: 0, Need: 30},
		{Key: prefix + "rq2", Capacity: 1, RefillPerSec: 0, Need: 1},
	}
	if ok, _ := rs.TryTake(t0, tk2); !ok {
		t.Fatal("tk2/rq2 首扣应成功")
	}
	if ok, _ := rs.TryTake(t0, tk2); ok {
		t.Fatal("rq2 耗尽 ⇒ 整拒")
	}
	if ok, _ := rs.TryTake(t0, []Take{{Key: prefix + "tk2", Capacity: 100, RefillPerSec: 0, Need: 50}}); !ok {
		t.Fatal("整拒后 tk2 应仍有 70（半扣则只剩 40 ⇒ 拒）")
	}
	// refill（TIME 统一时钟在脚本内，客户端 now 只影响参数语义）。
	time.Sleep(2100 * time.Millisecond)
	ok, err = rs.TryTake(time.Now(), takes)
	if err != nil || !ok {
		t.Fatalf("refill 后应可付：ok=%v err=%v", ok, err)
	}
	// 预算：累计 + 超限回滚。
	if v, err := rs.IncrBy(prefix+"budget", 30, 100); err != nil || v != 30 {
		t.Fatalf("首次累计 = %d err=%v", v, err)
	}
	if v, err := rs.IncrBy(prefix+"budget", 60, 100); err != nil || v != 90 {
		t.Fatalf("二次累计 = %d err=%v", v, err)
	}
	if _, err := rs.IncrBy(prefix+"budget", 20, 100); err == nil {
		t.Fatal("超限应报错")
	}
	if v, _ := rs.Get(prefix + "budget"); v != 90 {
		t.Fatalf("超限后现值 = %d，期望 90（回滚）", v)
	}
	// 冷却只增不减。
	if err := rs.SetCooldown(prefix+"cool", t0.Add(30*time.Second)); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	if err := rs.SetCooldown(prefix+"cool", t0.Add(10*time.Second)); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	until, _ := rs.CooldownUntil(prefix + "cool")
	if !until.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("冷却截止 = %v，期望 +30s（⛔ 被缩短）", until)
	}
}

// TestRespReplyParsing RESP2 五型解析（+ - : $ *，含 null bulk 与嵌套数组）。
func TestRespReplyParsing(t *testing.T) {
	cases := []struct {
		raw  string
		kind byte
		num  int64
		str  string
	}{
		{"+OK\r\n", '+', 0, "OK"},
		{":42\r\n", ':', 42, ""},
		{"$5\r\nhello\r\n", '$', 0, "hello"},
		{"$-1\r\n", '$', 0, ""}, // null bulk
	}
	for _, c := range cases {
		rp, err := readReply(bufio.NewReader(strings.NewReader(c.raw)))
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", c.raw, err)
		}
		if rp.kind != c.kind {
			t.Fatalf("解析 %q kind = %c，期望 %c", c.raw, rp.kind, c.kind)
		}
		if c.kind == ':' && rp.num != c.num {
			t.Fatalf("解析 %q num = %d，期望 %d", c.raw, rp.num, c.num)
		}
		if c.kind == '$' && c.str != "" && string(rp.bulk) != c.str {
			t.Fatalf("解析 %q bulk = %q，期望 %q", c.raw, rp.bulk, c.str)
		}
	}
	// 嵌套数组：*2\r\n:1\r\n:2\r\n
	rp, err := readReply(bufio.NewReader(strings.NewReader("*2\r\n:1\r\n:2\r\n")))
	if err != nil || rp.kind != '*' || len(rp.arr) != 2 || rp.arr[1].num != 2 {
		t.Fatalf("嵌套数组解析失败: %+v err=%v", rp, err)
	}
	// 错误型。
	rp, _ = readReply(bufio.NewReader(strings.NewReader("-ERR boom\r\n")))
	if rp.kind != '-' || rp.str != "ERR boom" {
		t.Fatalf("错误型解析失败: %+v", rp)
	}
}
