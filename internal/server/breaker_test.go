// 日期: 2026-11-16 23:01:50.884
// 作者: Zhai Wenjia

package server

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// testLogger 是测试用丢弃日志（状态迁移日志不断言内容，只断言状态机行为）。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBreakerTripAndRecover 钉住三态主路径（统一 SDD 7.3）：
// CLOSED 连续 5 次可归因失败 ⇒ OPEN（冷却中拒绝）⇒ 冷却期满转 HALF-OPEN 放探测
// ⇒ 探测成功回 CLOSED（计数清零）。
// 错误实现对照：阈值判断错（>= 改 >）⇒ 第 2 断言红；探测成功不闭合 ⇒ 第 5 断言红。
func TestBreakerTripAndRecover(t *testing.T) {
	b := NewEndpointBreaker("ep", 5, 30*time.Second, 1, testLogger())
	t0 := time.Now()

	if !b.Allow(t0) {
		t.Fatal("CLOSED 应放行")
	}
	for i := 1; i <= 4; i++ {
		b.RecordFailure(t0)
		if b.Allow(t0) {
			continue
		}
		t.Fatalf("第 %d 次失败后不应熔断（阈值 5）", i)
	}
	b.RecordFailure(t0) // 第 5 次 ⇒ 熔断打开
	if b.Allow(t0) {
		t.Fatal("达阈值后应熔断拒绝")
	}
	if got := b.State(); got != "open" {
		t.Fatalf("状态 = %s，期望 open", got)
	}
	// 冷却未满仍拒绝。
	if b.Allow(t0.Add(29 * time.Second)) {
		t.Fatal("冷却未满应拒绝")
	}
	// 冷却期满 ⇒ 半开放探测。
	if !b.Allow(t0.Add(30 * time.Second)) {
		t.Fatal("冷却期满应放行探测")
	}
	if got := b.State(); got != "half_open" {
		t.Fatalf("状态 = %s，期望 half_open", got)
	}
	b.RecordSuccess(t0.Add(31 * time.Second))
	if got := b.State(); got != "closed" {
		t.Fatalf("探测成功后状态 = %s，期望 closed", got)
	}
	// 闭合后计数清零：再来 4 次失败仍不熔断。
	for i := 0; i < 4; i++ {
		b.RecordFailure(t0.Add(time.Minute))
	}
	if !b.Allow(t0.Add(time.Minute)) {
		t.Fatal("闭合后失败计数应清零（4 次失败不应再次熔断）")
	}
}

// TestBreakerHalfOpenFailureReopens 探测失败 ⇒ 重新打开、冷却重新计时
// （⛔ 不因半开就半途闭合）。
func TestBreakerHalfOpenFailureReopens(t *testing.T) {
	b := NewEndpointBreaker("ep", 1, 30*time.Second, 1, testLogger())
	t0 := time.Now()
	b.RecordFailure(t0) // 熔断打开（阈值 1）
	if !b.Allow(t0.Add(30 * time.Second)) {
		t.Fatal("半开应放行探测")
	}
	b.RecordFailure(t0.Add(31 * time.Second)) // 探测失败 ⇒ 重新打开
	if got := b.State(); got != "open" {
		t.Fatalf("探测失败后状态 = %s，期望 open", got)
	}
	// 冷却重新计时：探测失败时刻 + 30s 内拒绝，期满再半开。
	if b.Allow(t0.Add(40 * time.Second)) {
		t.Fatal("探测失败后冷却应重新计时（40s < 31s+30s）")
	}
	if !b.Allow(t0.Add(61 * time.Second)) {
		t.Fatal("重新计时的冷却期满应再半开")
	}
}

// TestBreakerHalfOpenProbeLimit 半开同时放行探测数上限（默认 1）：
// 第一个探测放行、并发第二个拒绝；探测收口释放位后可再放。
func TestBreakerHalfOpenProbeLimit(t *testing.T) {
	b := NewEndpointBreaker("ep", 1, 30*time.Second, 1, testLogger())
	t0 := time.Now()
	b.RecordFailure(t0)
	if !b.Allow(t0.Add(30 * time.Second)) {
		t.Fatal("第一个探测应放行")
	}
	if b.Allow(t0.Add(30 * time.Second)) {
		t.Fatal("探测在途时第二个应拒绝（probe=1）")
	}
	b.RecordNeutral(t0.Add(30 * time.Second)) // 探测收口（非可归因）⇒ 释放位
	if !b.Allow(t0.Add(30 * time.Second)) {
		t.Fatal("探测收口释放位后应再放行")
	}
}

// TestBreakerNeutralNotCounted 非可归因收口（429 / 取消 / 内容策略等）⛔ 不计失败：
// 4 次失败 + 4 次 neutral 后未熔断，第 5 次失败才熔断。
func TestBreakerNeutralNotCounted(t *testing.T) {
	b := NewEndpointBreaker("ep", 5, 30*time.Second, 1, testLogger())
	t0 := time.Now()
	for i := 0; i < 4; i++ {
		b.RecordFailure(t0)
		b.RecordNeutral(t0)
	}
	if !b.Allow(t0) {
		t.Fatal("neutral 不应累计失败计数（4 次失败 + 4 次 neutral 不熔断）")
	}
	b.RecordFailure(t0)
	if b.Allow(t0) {
		t.Fatal("第 5 次可归因失败应熔断")
	}
}

// TestBreakerConcurrentAllow 半开探测并发不超上限（race 检测下跑）。
func TestBreakerConcurrentAllow(t *testing.T) {
	b := NewEndpointBreaker("ep", 1, 30*time.Second, 2, testLogger())
	t0 := time.Now()
	b.RecordFailure(t0)
	// 半开瞬间并发 Allow 10 次 ⇒ 放行数恰为 probe 数。
	gate := make(chan struct{})
	oks := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			<-gate
			oks <- b.Allow(t0.Add(30 * time.Second))
		}()
	}
	close(gate)
	passed := 0
	for i := 0; i < 10; i++ {
		if <-oks {
			passed++
		}
	}
	if passed != 2 {
		t.Fatalf("半开放行 = %d，期望 2（probe=2，⛔ 并发超放）", passed)
	}
}
