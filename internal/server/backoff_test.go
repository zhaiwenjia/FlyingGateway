// 日期: 2026-11-16 22:44:50.162
// 作者: Zhai Wenjia

package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/registry"
)

// TestBackoffWaitSequence 钉住退避序列（Google Cloud 口径：min(Base·2^(n-1) + rnd()·Jitter, Cap)）。
// rnd 恒 0 ⇒ 纯指数序列 1s/2s/4s/8s/16s/32s、之后截断在 32s。
// 错误实现对照：序列压平（无指数）⇒ 第 2 项起红；无截断 ⇒ 第 7 项红。
func TestBackoffWaitSequence(t *testing.T) {
	b := Backoff{Base: time.Second, Jitter: time.Second, Cap: 32 * time.Second}
	zero := func() float64 { return 0 }
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 32 * time.Second, 32 * time.Second,
	}
	for n, w := range want {
		if got := b.Wait(n+1, zero); got != w {
			t.Fatalf("第 %d 次重试等待 = %v，期望 %v", n+1, got, w)
		}
	}
}

// TestBackoffWaitJitterBounds 抖动区间断言：wait ∈ [Base·2^(n-1), Base·2^(n-1)+Jitter)。
// rnd 定值 0.5 / 0.999… 精确对照（⛔ 不用近似断言）。
func TestBackoffWaitJitterBounds(t *testing.T) {
	b := Backoff{Base: time.Second, Jitter: time.Second, Cap: 32 * time.Second}
	if got := b.Wait(1, func() float64 { return 0.5 }); got != 1500*time.Millisecond {
		t.Fatalf("n=1 rnd=0.5 ⇒ %v，期望 1.5s", got)
	}
	if got := b.Wait(2, func() float64 { return 0.5 }); got != 2500*time.Millisecond {
		t.Fatalf("n=2 rnd=0.5 ⇒ %v，期望 2.5s", got)
	}
	if got := b.Wait(1, func() float64 { return 0.999 }); got != 1999*time.Millisecond {
		t.Fatalf("n=1 rnd=0.999 ⇒ %v，期望 1.999s", got)
	}
	if got := b.Wait(1, func() float64 { return 0 }); got != time.Second {
		t.Fatalf("n=1 rnd=0 ⇒ %v，期望 1s", got)
	}
}

// TestBackoffWaitCapEatsJitter 整体截断语义：n=6 时 Base·2^5=32s 已到 Cap ⇒ 抖动被截（32s）。
func TestBackoffWaitCapEatsJitter(t *testing.T) {
	b := Backoff{Base: time.Second, Jitter: time.Second, Cap: 32 * time.Second}
	if got := b.Wait(6, func() float64 { return 0.999 }); got != 32*time.Second {
		t.Fatalf("n=6 触顶 ⇒ %v，期望 32s（Cap 整体截断）", got)
	}
}

// TestBackoffDefaults 整体零值 ⇒ 默认 1s/1s/32s；显式 Jitter=0 ⛔ 不被补默认抖动
// （测试确定性通道 —— 错误实现对照：误补抖动 ⇒ 第 3 断言红）。
func TestBackoffDefaults(t *testing.T) {
	var b Backoff
	if got := b.Wait(1, func() float64 { return 0.5 }); got != 1500*time.Millisecond {
		t.Fatalf("零值 n=1 rnd=0.5 ⇒ %v，期望 1.5s（默认 Base=1s Jitter=1s）", got)
	}
	if got := b.Wait(7, func() float64 { return 0 }); got != 32*time.Second {
		t.Fatalf("零值 n=7 ⇒ %v，期望 32s（默认 Cap）", got)
	}
	noJitter := Backoff{Base: 100 * time.Millisecond, Jitter: 0, Cap: time.Second}
	if got := noJitter.Wait(1, func() float64 { return 0.999 }); got != 100*time.Millisecond {
		t.Fatalf("显式 Jitter=0 ⇒ %v，期望 100ms（⛔ 不误补默认抖动）", got)
	}
}

// TestBackoffWaitInvalidAttempt n<1 按 1 处理（⛔ 不 panic 不负等待）。
func TestBackoffWaitInvalidAttempt(t *testing.T) {
	b := Backoff{Base: time.Second, Jitter: 0, Cap: 32 * time.Second}
	if got := b.Wait(0, nil); got != time.Second {
		t.Fatalf("n=0 ⇒ %v，期望 1s", got)
	}
	if got := b.Wait(-3, nil); got != time.Second {
		t.Fatalf("n=-3 ⇒ %v，期望 1s", got)
	}
}

// TestWaitSleepCancel 等待可取消：ctx 已取消 ⇒ 立即返回 ctx.Err()（⛔ 不等满）。
func TestWaitSleepCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := waitSleep(ctx, 5*time.Second)
	if err == nil {
		t.Fatal("ctx 已取消，waitSleep 应返回错误")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("取消后仍等待 %v（应立即返回）", elapsed)
	}
}

// TestWaitSleepWaitsFullDuration 正常路径等满时长（错误实现对照：sleep 为空 ⇒ 红）。
func TestWaitSleepWaitsFullDuration(t *testing.T) {
	start := time.Now()
	if err := waitSleep(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatalf("waitSleep 出错: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("只等了 %v，期望 ≥ 50ms", elapsed)
	}
}

// TestWaitBeforeRetryBranches 钉住 waitBeforeRetry 的分支语义：
//   - 429（rate_limit）⇒ ⛔ 不退避（Retry-After 语义由令牌冷却承载，立即返回）；
//   - upstream_error / timeout ⇒ 指数退避等足（n=2、Base=Cap=100ms ⇒ 100ms 截断）。
//
// 错误实现对照：429 也套退避 ⇒ 第 1 断言红；退避没等 ⇒ 第 2 断言红。
func TestWaitBeforeRetryBranches(t *testing.T) {
	s := &Server{
		retryBackoff: Backoff{Base: 100 * time.Millisecond, Jitter: 0, Cap: 100 * time.Millisecond},
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	h := &execHandle{ctx: context.Background()}
	ep := &registry.Endpoint{Name: "ep"}

	start := time.Now()
	if err := s.waitBeforeRetry(h, ep, core.ErrRateLimit, 3); err != nil {
		t.Fatalf("429 waitBeforeRetry 出错: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("429 不应退避，实际等了 %v", elapsed)
	}

	start = time.Now()
	if err := s.waitBeforeRetry(h, ep, core.ErrUpstreamError, 2); err != nil {
		t.Fatalf("upstream_error waitBeforeRetry 出错: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("upstream_error 退避只等了 %v，期望 ≥ 100ms（n=2 截断在 Cap=100ms）", elapsed)
	}
}
