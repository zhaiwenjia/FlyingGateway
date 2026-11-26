// 日期: 2026-11-16 23:13:43.700
// 作者: Zhai Wenjia

package telemetry

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// 本文件把内存指标（统一 SDD 6.4 独立计数指标）渲染为 Prometheus 文本暴露格式
//（exposition format 0.0.4，官方口径：HTTP Content-Type
// `text/plain; version=0.0.4; charset=utf-8`、UTF-8、\n 行尾、末行以 \n 收尾）：
//   - `# HELP <name> <doc>` / `# TYPE <name> <counter|gauge|histogram>` 元数据行；
//   - counter 命名带 `_total` 后缀（Prometheus 惯例）；
//   - histogram 输出累计桶 `<name>_bucket{le="..."}`（含 `+Inf`）+ `<name>_sum` +
//     `<name>_count` —— 内存里按「上界独立计数」存，渲染时转**累计**（Prometheus
//     histogram 桶语义是 cumulative，⛔ 不把独立计数当累计值直接输出）；
//   - label 值转义 `\` `"` `\n`，HELP 文本转义 `\` `\n`（同源格式规范）。
//
// 指标名与 SDD 6.4 的对应：gateway.requests → gateway_requests_total（counter 加
// _total 是 Prometheus 命名惯例，HELP 里注明 SDD 原名）；cancel.detected_latency
// 存储单位毫秒、导出单位秒（Prometheus 时间量惯例 _seconds）。

// 延迟直方图上界（毫秒，与 ObserveCancelLatency 的分桶一致；0 = +Inf 桶）。
var latencyBoundsMs = []float64{100, 250, 500, 1000, 2000, 5000}

// WritePrometheus 把指标全量渲染为 Prometheus 文本格式写入 w。
func (m *Metrics) WritePrometheus(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// ---- gateway_requests_total（counter，标签 provider/model/result）----
	var reqKeys []string
	for k := range m.requests {
		reqKeys = append(reqKeys, k)
	}
	sort.Strings(reqKeys)
	if len(reqKeys) > 0 {
		fmt.Fprintln(w, "# HELP gateway_requests_total 上游执行结果计数（SDD gateway.requests）。")
		fmt.Fprintln(w, "# TYPE gateway_requests_total counter")
		for _, k := range reqKeys {
			parts := strings.SplitN(k, "|", 3)
			if len(parts) != 3 {
				continue
			}
			fmt.Fprintf(w, "gateway_requests_total{provider=%s,model=%s,result=%s} %d\n",
				quoteLabel(parts[0]), quoteLabel(parts[1]), quoteLabel(parts[2]), m.requests[k])
		}
	}

	// ---- gateway_cancelled_total（counter，标签 reason）----
	var cancelKeys []string
	for k := range m.cancelled {
		cancelKeys = append(cancelKeys, k)
	}
	sort.Strings(cancelKeys)
	if len(cancelKeys) > 0 {
		fmt.Fprintln(w, "# HELP gateway_cancelled_total 取消事件计数（SDD gateway.cancelled，不受 trace 采样影响）。")
		fmt.Fprintln(w, "# TYPE gateway_cancelled_total counter")
		for _, k := range cancelKeys {
			fmt.Fprintf(w, "gateway_cancelled_total{reason=%s} %d\n", quoteLabel(k), m.cancelled[k])
		}
	}

	// ---- gateway_tokens_total（counter，标签 direction）----
	var tokenKeys []string
	for k := range m.tokens {
		tokenKeys = append(tokenKeys, k)
	}
	sort.Strings(tokenKeys)
	if len(tokenKeys) > 0 {
		fmt.Fprintln(w, "# HELP gateway_tokens_total token 累计（SDD gateway.tokens，按方向）。")
		fmt.Fprintln(w, "# TYPE gateway_tokens_total counter")
		for _, k := range tokenKeys {
			fmt.Fprintf(w, "gateway_tokens_total{direction=%s} %d\n", quoteLabel(k), m.tokens[k])
		}
	}

	// ---- gateway_cache_tokens_total（counter，标签 phase；CAC-08，2026-11-26）----
	var cachePhases []string
	for k := range m.cacheTokens {
		cachePhases = append(cachePhases, k)
	}
	sort.Strings(cachePhases)
	if len(cachePhases) > 0 {
		fmt.Fprintln(w, "# HELP gateway_cache_tokens_total 缓存 token 累计（read=命中 / write=写入）。")
		fmt.Fprintln(w, "# TYPE gateway_cache_tokens_total counter")
		for _, k := range cachePhases {
			fmt.Fprintf(w, "gateway_cache_tokens_total{phase=%s} %d\n", quoteLabel(k), m.cacheTokens[k])
		}
	}

	// ---- gateway_cancel_detected_latency_seconds（histogram）----
	if m.latencyCount > 0 {
		fmt.Fprintln(w, "# HELP gateway_cancel_detected_latency_seconds 断连检测延迟（SDD gateway.cancel.detected_latency）。")
		fmt.Fprintln(w, "# TYPE gateway_cancel_detected_latency_seconds histogram")
		// 内存按上界独立计数 ⇒ 渲染转累计（le 桶含等于）。
		cum := int64(0)
		for _, upper := range latencyBoundsMs {
			cum += m.latencyBuckets[upper]
			fmt.Fprintf(w, "gateway_cancel_detected_latency_seconds_bucket{le=%s} %d\n",
				quoteLabel(formatFloat(upper/1000)), cum)
		}
		cum += m.latencyBuckets[0] // +Inf 桶 = 全部样本
		fmt.Fprintf(w, "gateway_cancel_detected_latency_seconds_bucket{le=\"+Inf\"} %d\n", cum)
		fmt.Fprintf(w, "gateway_cancel_detected_latency_seconds_sum %s\n", formatFloat(m.latencySumMs/1000))
		fmt.Fprintf(w, "gateway_cancel_detected_latency_seconds_count %d\n", m.latencyCount)
	}

	// ---- gateway_usage_missing_rate（gauge，0-1）----
	fmt.Fprintln(w, "# HELP gateway_usage_missing_rate usage 缺失率（SDD gateway.usage_missing_rate，0-1）。")
	fmt.Fprintln(w, "# TYPE gateway_usage_missing_rate gauge")
	fmt.Fprintf(w, "gateway_usage_missing_rate %s\n", formatFloat(m.UsageMissingRateLocked()))
}

// UsageMissingRateLocked 返回 usage 缺失率（调用方必须已持 m.mu）。
func (m *Metrics) UsageMissingRateLocked() float64 {
	if m.usageTotal == 0 {
		return 0
	}
	return float64(m.usageMissing) / float64(m.usageTotal)
}

// quoteLabel 按 Prometheus 文本格式转义 label 值并加引号（\ 与 " 与换行转义）。
func quoteLabel(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// formatFloat 用最短可读表示渲染浮点（Prometheus 文本格式接受常规十进制）。
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
