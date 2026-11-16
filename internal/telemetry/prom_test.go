// 日期: 2026-11-16 23:13:43.700
// 作者: Zhai Wenjia

package telemetry

import (
	"strings"
	"testing"
)

// TestPromRequestsCounter 钉住 counter 渲染（HELP/TYPE 元数据 + 标签 + _total 后缀）。
func TestPromRequestsCounter(t *testing.T) {
	m := NewMetrics()
	m.ObserveRequest("openai", "gpt-6", "ok")
	m.ObserveRequest("openai", "gpt-6", "ok")
	m.ObserveRequest("openai", "gpt-6", "timeout")
	var b strings.Builder
	m.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		"# HELP gateway_requests_total ",
		"# TYPE gateway_requests_total counter",
		`gateway_requests_total{provider="openai",model="gpt-6",result="ok"} 2`,
		`gateway_requests_total{provider="openai",model="gpt-6",result="timeout"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("渲染缺 %q：\n%s", want, out)
		}
	}
}

// TestPromHistogramCumulative 钉住直方图「独立计数 ⇒ 累计桶」转换（Prometheus histogram
// 桶语义 cumulative）：样本 50ms / 300ms / 6000ms ⇒
// le=0.1→1、le=0.25→1、le=0.5→2、le=1→2、le=2→2、le=5→2、+Inf→3；sum=6.35s；count=3。
// 错误实现对照：把内存独立计数直接当累计输出 ⇒ le=0.5 行是 1 ⇒ 红。
func TestPromHistogramCumulative(t *testing.T) {
	m := NewMetrics()
	m.ObserveCancelLatency(50)
	m.ObserveCancelLatency(300)
	m.ObserveCancelLatency(6000)
	var b strings.Builder
	m.WritePrometheus(&b)
	out := b.String()
	want := map[string]string{
		`gateway_cancel_detected_latency_seconds_bucket{le="0.1"} 1`:  "le=0.1",
		`gateway_cancel_detected_latency_seconds_bucket{le="0.25"} 1`: "le=0.25",
		`gateway_cancel_detected_latency_seconds_bucket{le="0.5"} 2`:  "le=0.5",
		`gateway_cancel_detected_latency_seconds_bucket{le="1"} 2`:    "le=1",
		`gateway_cancel_detected_latency_seconds_bucket{le="2"} 2`:    "le=2",
		`gateway_cancel_detected_latency_seconds_bucket{le="5"} 2`:    "le=5",
		`gateway_cancel_detected_latency_seconds_bucket{le="+Inf"} 3`: "+Inf",
		"gateway_cancel_detected_latency_seconds_sum 6.35":            "sum",
		"gateway_cancel_detected_latency_seconds_count 3":             "count",
	}
	for line, name := range want {
		if !strings.Contains(out, line) {
			t.Fatalf("缺 %s 行（期望 %q）：\n%s", name, line, out)
		}
	}
}

// TestPromLabelEscaping label 值转义（\ 、" 、换行）——错误实现对照：不转义 ⇒ 断言红。
func TestPromLabelEscaping(t *testing.T) {
	m := NewMetrics()
	m.ObserveCancel(`a"b\c` + "\n" + "d")
	var b strings.Builder
	m.WritePrometheus(&b)
	out := b.String()
	if !strings.Contains(out, `gateway_cancelled_total{reason="a\"b\\c\nd"} 1`) {
		t.Fatalf("label 值未按规范转义：\n%s", out)
	}
}

// TestPromTokensAndUsageRate tokens counter 与 usage_missing_rate gauge。
func TestPromTokensAndUsageRate(t *testing.T) {
	m := NewMetrics()
	m.ObserveTokens("input", 100)
	m.ObserveTokens("output", 30)
	m.ObserveUsage(true)
	m.ObserveUsage(false)
	var b strings.Builder
	m.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`gateway_tokens_total{direction="input"} 100`,
		`gateway_tokens_total{direction="output"} 30`,
		"# TYPE gateway_usage_missing_rate gauge",
		"gateway_usage_missing_rate 0.5",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("渲染缺 %q：\n%s", want, out)
		}
	}
}

// TestPromLineFormat 末行以 \n 收尾、无空指标时 usage gauge 也恒输出（Prometheus 文本格式口径）。
func TestPromLineFormat(t *testing.T) {
	m := NewMetrics()
	var b strings.Builder
	m.WritePrometheus(&b)
	out := b.String()
	if out == "" || !strings.HasSuffix(out, "\n") {
		t.Fatalf("输出应非空且以 \\n 收尾：%q", out)
	}
	if !strings.Contains(out, "gateway_usage_missing_rate 0") {
		t.Fatalf("空指标集也应输出 usage gauge：\n%s", out)
	}
}
