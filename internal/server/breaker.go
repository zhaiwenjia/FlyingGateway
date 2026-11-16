// 日期: 2026-11-16 23:01:50.884
// 作者: Zhai Wenjia

package server

import (
	"log/slog"
	"sync"
	"time"
)

// 本文件承载端点熔断三态状态机（统一 SDD 7.3 熔断与冷却）：
// CLOSED →（连续可归因失败达阈值）→ OPEN →（冷却期满）→ HALF-OPEN（放行探测请求）
// → 探测成功回 CLOSED / 探测失败回 OPEN（冷却重新计时）。
//
// 口径与成熟方案（circuit breaker pattern，Nygard《Release It!》三态机）对齐：
//   - 可归因失败 = 5xx / 超时 / 连接失败（upstream_error / timeout）；429 / 取消 /
//     内容策略 / 上下文越界等**不算端点故障**（RecordNeutral 释放探测位、不动计数）；
//   - 熔断期间跳过该端点、转降级链顺延（⛔ 不打网络、快速失败）；
//   - 半开同时放行 Probe 个探测（默认 1）——⛔ 冷却一到期就全量灌回坏端点；
//   - 状态迁移逐次落日志（「静默的熔断是谜之故障」——观测是模式的一部分）。
//
// 时间经 now 参数注入（测试可控时序，⛔ 不 mock 时钟——调用方传 time.Now()）。

// breakerState 是熔断三态。
type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// String 返回状态名（日志/管理面输出）。
func (s breakerState) String() string {
	switch s {
	case breakerClosed:
		return "closed"
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half_open"
	}
	return "unknown"
}

// 默认参数（统一 SDD 7.3 默认值表：全部可配置）。
const (
	defaultBreakerThreshold = 5
	defaultBreakerCooldown  = 30 * time.Second
	defaultBreakerProbe     = 1
)

// EndpointBreaker 是单个端点的熔断器（并发安全）。
type EndpointBreaker struct {
	mu sync.Mutex

	name             string
	failureThreshold int
	cooldown         time.Duration
	probes           int

	state            breakerState
	consecutiveFails int
	cooldownUntil    time.Time
	probesInFlight   int
	logger           *slog.Logger
}

// NewEndpointBreaker 构造熔断器。threshold / cooldown / probes ≤ 0 ⇒ 取默认
// （5 / 30s / 1，统一 SDD 7.3 默认值表）。logger 为 nil ⇒ slog.Default()。
func NewEndpointBreaker(name string, threshold int, cooldown time.Duration, probes int, logger *slog.Logger) *EndpointBreaker {
	if threshold <= 0 {
		threshold = defaultBreakerThreshold
	}
	if cooldown <= 0 {
		cooldown = defaultBreakerCooldown
	}
	if probes <= 0 {
		probes = defaultBreakerProbe
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EndpointBreaker{
		name:             name,
		failureThreshold: threshold,
		cooldown:         cooldown,
		probes:           probes,
		state:            breakerClosed,
		logger:           logger,
	}
}

// Allow 报告当前是否放行一次上游调用（⛔ 不打网络语义由调用方执行）：
//   - CLOSED ⇒ 放行；
//   - OPEN 且冷却未满 ⇒ 拒绝（跳过该端点）；
//   - OPEN 冷却期满 ⇒ 转 HALF-OPEN 放探测（probes 内放行、超出拒绝）；
//   - HALF-OPEN ⇒ probesInFlight < probes 才放行（配对收口见 Record*）。
func (b *EndpointBreaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerClosed:
		return true
	case breakerOpen:
		if now.Before(b.cooldownUntil) {
			return false
		}
		// 冷却期满 ⇒ 半开，放第一个探测。
		b.state = breakerHalfOpen
		b.probesInFlight = 0
		b.logger.Info("端点熔断转半开（放行探测）",
			"endpoint", b.name, "probes", b.probes, "cooldown_s", b.cooldown.Seconds())
		fallthrough
	case breakerHalfOpen:
		if b.probesInFlight < b.probes {
			b.probesInFlight++
			return true
		}
		return false
	}
	return false
}

// RecordSuccess 记一次成功收口：CLOSED 清零连续失败计数；HALF-OPEN 探测成功 ⇒ 闭合。
func (b *EndpointBreaker) RecordSuccess(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerClosed:
		b.consecutiveFails = 0
	case breakerHalfOpen:
		b.releaseProbeLocked()
		b.state = breakerClosed
		b.consecutiveFails = 0
		b.logger.Info("端点熔断闭合（探测成功）", "endpoint", b.name)
	case breakerOpen:
		// 防御：OPEN 不放行、不应有收口。
	}
}

// RecordFailure 记一次可归因失败收口（5xx / 超时 / 连接失败）：
//   - CLOSED ⇒ 连续失败计数 +1，达阈值 ⇒ 熔断打开（冷却 cooldown）；
//   - HALF-OPEN ⇒ 探测失败 ⇒ 重新打开、冷却重新计时（⛔ 不因单次成功半途闭合）。
func (b *EndpointBreaker) RecordFailure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerClosed:
		b.consecutiveFails++
		if b.consecutiveFails >= b.failureThreshold {
			b.state = breakerOpen
			b.cooldownUntil = now.Add(b.cooldown)
			b.logger.Warn("端点熔断打开（连续失败达阈值）",
				"endpoint", b.name, "consecutive_fails", b.consecutiveFails,
				"threshold", b.failureThreshold, "cooldown_s", b.cooldown.Seconds())
			b.consecutiveFails = 0
		}
	case breakerHalfOpen:
		b.releaseProbeLocked()
		b.state = breakerOpen
		b.cooldownUntil = now.Add(b.cooldown)
		b.logger.Warn("端点熔断重新打开（探测失败）",
			"endpoint", b.name, "cooldown_s", b.cooldown.Seconds())
	case breakerOpen:
		// 防御：OPEN 不放行、不应有收口。
	}
}

// RecordNeutral 记一次**非可归因**收口（429 / 取消 / 内容策略 / 上下文越界等）：
// 不动失败计数与状态，仅释放 HALF-OPEN 的探测位（这些失败不说明端点故障）。
func (b *EndpointBreaker) RecordNeutral(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == breakerHalfOpen {
		b.releaseProbeLocked()
	}
}

// releaseProbeLocked 释放一个探测位（调用方持锁）。
func (b *EndpointBreaker) releaseProbeLocked() {
	if b.probesInFlight > 0 {
		b.probesInFlight--
	}
}

// State 返回当前状态名（管理面观测；状态迁移本身已逐次落日志）。
func (b *EndpointBreaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.String()
}

// StateCode 返回状态数值（Prometheus gauge 取值：closed=0 / half_open=1 / open=2）。
func (b *EndpointBreaker) StateCode() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int(b.state)
}
