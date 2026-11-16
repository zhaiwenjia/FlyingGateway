// 日期: 2026-11-16 23:13:43.700
// 作者: Zhai Wenjia

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/registry"
)

// TestProberTripsBreakerOnUnreachable 钉住「巡检不可达 ⇒ 主动熔断」（统一 SDD 2.8 预防语义）：
// 一个 200 上游、一个 500 上游 ⇒ 500 记 unhealthy 且熔断 Trip（Allow 拒绝）、200 记 healthy。
// 错误实现对照：巡检失败不 Trip ⇒ 熔断断言红；≥500 当健康 ⇒ healthy 断言红。
func TestProberTripsBreakerOnUnreachable(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	breakers := map[string]*EndpointBreaker{
		"good": NewEndpointBreaker("good", 5, 30*time.Second, 1, logger),
		"bad":  NewEndpointBreaker("bad", 5, 30*time.Second, 1, logger),
	}
	eps := []registry.Endpoint{
		{Name: "good", BaseURL: good.URL},
		{Name: "bad", BaseURL: bad.URL},
	}
	p := newUpstreamProber(eps, breakers, 0, time.Second, true, logger) // interval=0 不起 goroutine
	if p != nil {
		t.Fatal("interval=0 应返回 nil（不启用）")
	}
	// 手动构造跑一轮（interval=0 ⇒ newUpstreamProber 返回 nil，直接 new 结构体）。
	p = &upstreamProber{
		results:       map[string]*probeResult{},
		endpoints:     eps,
		breakers:      breakers,
		timeout:       time.Second,
		allowLoopback: true,
		logger:        logger,
	}
	p.ProbeNow()

	snap := p.Snapshot()
	if !snap["good"].Healthy {
		t.Fatalf("good 应 healthy：%+v", snap["good"])
	}
	if snap["bad"].Healthy {
		t.Fatalf("bad（500）应 unhealthy：%+v", snap["bad"])
	}
	if breakers["good"].State() != "closed" {
		t.Fatalf("good 熔断 = %s，期望 closed（巡检成功不清真实计数也不开闸）", breakers["good"].State())
	}
	if breakers["bad"].State() != "open" {
		t.Fatalf("bad 熔断 = %s，期望 open（巡检不可达 ⇒ 主动熔断）", breakers["bad"].State())
	}
}

// TestProberClosesHalfOpen 半开时巡检成功即探测闭合（巡检即探测）；
// CLOSED 时巡检成功 ⛔ 不清零流量失败计数。
func TestProberClosesHalfOpen(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 场景 1：半开 ⇒ 巡检成功闭合。
	brk := NewEndpointBreaker("ep", 1, 30*time.Second, 1, logger)
	t0 := time.Now()
	brk.RecordFailure(t0) // 熔断打开
	if !brk.Allow(t0.Add(30 * time.Second)) {
		t.Fatal("冷却期满应转半开")
	}
	p := &upstreamProber{
		results:       map[string]*probeResult{},
		endpoints:     []registry.Endpoint{{Name: "ep", BaseURL: good.URL}},
		breakers:      map[string]*EndpointBreaker{"ep": brk},
		timeout:       time.Second,
		allowLoopback: true,
		logger:        logger,
	}
	p.ProbeNow()
	if got := brk.State(); got != "closed" {
		t.Fatalf("半开巡检成功后 = %s，期望 closed", got)
	}

	// 场景 2：closed + 4 次流量失败（threshold=5 的独立熔断器）⇒ 巡检成功不清计数
	// ⇒ 第 5 次失败仍开闸。
	brk2 := NewEndpointBreaker("ep2", 5, 30*time.Second, 1, logger)
	p.endpoints = []registry.Endpoint{{Name: "ep2", BaseURL: good.URL}}
	p.breakers = map[string]*EndpointBreaker{"ep2": brk2}
	for i := 0; i < 4; i++ {
		brk2.RecordFailure(t0)
	}
	p.ProbeNow() // 巡检成功 ⛔ 不清零
	if got := brk2.State(); got != "closed" {
		t.Fatalf("4 次失败后 = %s，期望 closed", got)
	}
	brk2.RecordFailure(t0)
	if got := brk2.State(); got != "open" {
		t.Fatalf("第 5 次失败后 = %s，期望 open（巡检成功未清零流量计数）", got)
	}
}

// TestProberEgressDenied 出网校验拒绝的端点记 unhealthy（⛔ 巡检不开绕过出网校验的通道）。
func TestProberEgressDenied(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := &upstreamProber{
		results:       map[string]*probeResult{},
		endpoints:     []registry.Endpoint{{Name: "intranet", BaseURL: "http://10.0.0.1:8000/v1"}},
		breakers:      map[string]*EndpointBreaker{},
		timeout:       time.Second,
		allowLoopback: false, // 默认禁内网
		logger:        logger,
	}
	p.ProbeNow()
	snap := p.Snapshot()
	if snap["intranet"].Healthy {
		t.Fatalf("内网目标出网校验应拒绝 ⇒ unhealthy：%+v", snap["intranet"])
	}
	if snap["intranet"].Error == "" {
		t.Fatal("unhealthy 应带失败原因")
	}
}

// TestReadinessAndShutdown 钉住就绪探针口径：正常 200 ready；Shutdown 后 503
// （先摘流量再排空，K8s readiness）。
func TestReadinessAndShutdown(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	resp := e.get("/health/readiness")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("就绪 = %d，期望 200", resp.StatusCode)
	}
	readBody(t, resp)
	e.srv.Shutdown()
	resp2 := e.get("/health/readiness")
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Shutdown 后 = %d，期望 503（摘流量）", resp2.StatusCode)
	}
	m := readBody(t, resp2)
	if m["status"] != "shutting_down" {
		t.Fatalf("status = %v，期望 shutting_down", m["status"])
	}
}
