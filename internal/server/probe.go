// 日期: 2026-11-16 23:13:43.700
// 作者: Zhai Wenjia

package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/registry"
	"github.com/zhaiwenjia/FlyingGateway/internal/security"
)

// 本文件承载上游周期巡检（统一 SDD 2.8：主动巡检是预防，被动冷却是兜底）：
// 后台 goroutine 按间隔对每个注册端点做**低成本可达性探测**，结果进 /admin/health
// 的 probes 视图，并与熔断器联动（巡检不可达 ⇒ 主动熔断 Trip；半开时巡检成功即探测闭合）。
//
// 探测语义（⛔ 不夸大）：GET {base_url}，状态码 < 500 视为「可达且在应答」、
// ≥ 500 / 连接失败 / 超时视为不可达 —— 这是 **HTTP 可达性巡检，不是模型能力探活**
//（⛔ 不发模型调用、不花 token 配额）。能力级失败由真实流量的失败计数与熔断兜底。
//
// 出网口径与执行链一致：每次探测前过 security.ValidateEgressURL（统一 SDD 8.2
// 出网单一控制点），⛔ 不因巡检另开绕过出网校验的通道。

// probeResult 是单端点的巡检结果快照。
type probeResult struct {
	LastCheck time.Time `json:"last_check"`
	Healthy   bool      `json:"healthy"`
	Error     string    `json:"error,omitempty"` // 失败原因（已脱敏）
}

// upstreamProber 是上游周期巡检器（并发安全）。
type upstreamProber struct {
	mu            sync.Mutex
	results       map[string]*probeResult
	endpoints     []registry.Endpoint
	breakers      map[string]*EndpointBreaker
	interval      time.Duration
	timeout       time.Duration
	allowLoopback bool
	logger        *slog.Logger

	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

// newUpstreamProber 构造巡检器。interval <= 0 ⇒ 不启用（返回 nil）。
// timeout <= 0 ⇒ 默认 3s（DefaultProbeTimeout）。
func newUpstreamProber(eps []registry.Endpoint, breakers map[string]*EndpointBreaker,
	interval, timeout time.Duration, allowLoopback bool, logger *slog.Logger) *upstreamProber {
	if interval <= 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	p := &upstreamProber{
		results:       make(map[string]*probeResult, len(eps)),
		endpoints:     eps,
		breakers:      breakers,
		interval:      interval,
		timeout:       timeout,
		allowLoopback: allowLoopback,
		logger:        logger,
		stop:          make(chan struct{}),
		stopped:       make(chan struct{}),
	}
	go p.loop()
	return p
}

// DefaultProbeTimeout 是单次巡检探测超时（统一 SDD 2.8 探针轻量口径）。
const DefaultProbeTimeout = 3 * time.Second

// loop 周期执行巡检轮（启动即先跑一轮，不等第一个 tick）。
func (p *upstreamProber) loop() {
	defer close(p.stopped)
	p.ProbeNow()
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.ProbeNow()
		}
	}
}

// Stop 停止巡检（幂等；等待在途轮收口）。
func (p *upstreamProber) Stop() {
	p.once.Do(func() {
		close(p.stop)
		<-p.stopped
	})
}

// ProbeNow 执行一轮巡检（逐端点串行探测；可手动触发，测试与管理面可用）。
func (p *upstreamProber) ProbeNow() {
	client := &http.Client{Timeout: p.timeout}
	for _, ep := range p.endpoints {
		healthy, err := p.probeOne(client, &ep)
		now := time.Now()
		p.mu.Lock()
		r := &probeResult{LastCheck: now, Healthy: healthy}
		if err != nil {
			r.Error = security.Redact(err.Error())
		}
		p.results[ep.Name] = r
		p.mu.Unlock()
		// 熔断联动：不可达 ⇒ 主动开闸（预防语义）；可达仅在半开时当探测闭合。
		if brk, ok := p.breakers[ep.Name]; ok {
			if !healthy {
				brk.Trip(now)
			} else {
				brk.RecordProbeSuccess(now)
			}
		}
	}
}

// probeOne 探测单端点：过出网校验后 GET base_url，状态码 < 500 视为可达。
func (p *upstreamProber) probeOne(client *http.Client, ep *registry.Endpoint) (bool, error) {
	if err := security.ValidateEgressURL(ep.BaseURL, p.allowLoopback); err != nil {
		return false, fmt.Errorf("probe: 出网校验拒绝 %s：%w", ep.Name, err)
	}
	resp, err := client.Get(ep.BaseURL)
	if err != nil {
		return false, fmt.Errorf("probe: %s 不可达：%w", ep.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return false, fmt.Errorf("probe: %s 返回 %d（≥500 视为不可达）", ep.Name, resp.StatusCode)
	}
	return true, nil
}

// Snapshot 返回全端点巡检结果快照（管理面视图用）。
func (p *upstreamProber) Snapshot() map[string]probeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]probeResult, len(p.results))
	for k, v := range p.results {
		out[k] = *v
	}
	return out
}
