// 日期: 2026-11-14 11:47:34.859
// 作者: Zhai Wenjia

package telemetry

import (
	"sync"
)

// Metrics 是取消事件的独立计数指标（统一 SDD 6.4：head sampling 无法保证保留
// 未来才出错的 trace（取消即「未来的失败」）⇒ 取消计数必须独立于采样）。
type Metrics struct {
	mu sync.Mutex
	// requests: label key = "provider|model|result" → 计数。
	requests map[string]int64
	// cancelled: label key = reason → 计数。
	cancelled map[string]int64
	// tokens: label key = direction（input / output）→ 累计。
	tokens map[string]int64
	// cancelDetectedLatency: 直方图（毫秒分桶）。
	latencyBuckets map[float64]int64
	latencySumMs   float64
	latencyCount   int64
	// usageMissing / usageTotal 供 gateway.usage_missing_rate。
	usageMissing int64
	usageTotal   int64
	// cacheTokens: label key = phase（read / write）→ 累计（CAC-08，2026-11-26：
	// cached_tokens 原先只进 trace span ⇒ 不采 trace 算不出缓存命中率）。
	cacheTokens map[string]int64
}

// NewMetrics 构造指标集合。
func NewMetrics() *Metrics {
	return &Metrics{
		requests:       map[string]int64{},
		cancelled:      map[string]int64{},
		tokens:         map[string]int64{},
		latencyBuckets: map[float64]int64{},
		cacheTokens:    map[string]int64{},
	}
}

// ObserveRequest 记一次上游执行结果（gateway.requests）。
func (m *Metrics) ObserveRequest(provider, model, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[provider+"|"+model+"|"+result]++
}

// ObserveCancel 记一次取消（gateway.cancelled，按原因）。
func (m *Metrics) ObserveCancel(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancelled[reason]++
}

// ObserveTokens 记 token（gateway.tokens，按方向 input / output）。
func (m *Metrics) ObserveTokens(direction string, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[direction] += n
}

// ObserveCache 记缓存 token（gateway.cache_tokens_total，按相 read / write）。
// CAC-08（2026-11-26）：cached_tokens 原先只进 trace span ⇒ 不采 trace 就算不出
// 缓存命中率；进聚合指标后 `/metrics` 直接可算 Σread/Σinput。
func (m *Metrics) ObserveCache(read, write int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheTokens["read"] += read
	m.cacheTokens["write"] += write
}

// CacheTokens 读缓存累计（相 = read / write）。
func (m *Metrics) CacheTokens(phase string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cacheTokens[phase]
}

// ObserveCancelLatency 记断连检测延迟（gateway.cancel.detected_latency 直方图，毫秒）。
func (m *Metrics) ObserveCancelLatency(ms float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bucket := 100.0
	for _, upper := range []float64{100, 250, 500, 1000, 2000, 5000} {
		if ms <= upper {
			bucket = upper
			break
		}
		bucket = 0 // 超出所有上界 ⇒ +Inf 桶
	}
	m.latencyBuckets[bucket]++
	m.latencySumMs += ms
	m.latencyCount++
}

// ObserveUsage 记 usage 是否缺失（gateway.usage_missing_rate）。
func (m *Metrics) ObserveUsage(missing bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usageTotal++
	if missing {
		m.usageMissing++
	}
}

// Requests 返回指定标签的请求数（无该标签 ⇒ 0）。
func (m *Metrics) Requests(provider, model, result string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests[provider+"|"+model+"|"+result]
}

// Cancelled 返回指定原因的取消计数。
func (m *Metrics) Cancelled(reason string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelled[reason]
}

// Tokens 返回指定方向的 token 累计。
func (m *Metrics) Tokens(direction string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens[direction]
}

// UsageMissingRate 返回 usage 缺失率（无样本 ⇒ 0）。
func (m *Metrics) UsageMissingRate() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.usageTotal == 0 {
		return 0
	}
	return float64(m.usageMissing) / float64(m.usageTotal)
}
