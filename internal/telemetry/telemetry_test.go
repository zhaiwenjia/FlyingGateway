// 日期: 2026-11-14 11:47:34.859
// 作者: Zhai Wenjia

package telemetry

import "testing"

// TestSpanCancelSemantics 钉住统一 SDD 6.3 取消口径：
// 取消 ⇒ gen_ai.response.status=cancelled、span status 留 Unset、⛔ 不设 error.type。
// 恒假对照：把实现改成「取消时 SetStatus(SpanError) + SetAttr(error.type)」时本断言必红。
func TestSpanCancelSemantics(t *testing.T) {
	exp := &InMemoryExporter{}
	tr := NewTracer(exp)
	sp := tr.Start(OperationChat)
	sp.SetAttr(AttrProviderName, "openai")
	sp.SetAttr(AttrResponseStatus, ResponseCancelled)
	sp.SetAttr(AttrGatewayCancelReason, CancelClientDisconnect)
	// 取消口径：status 留 Unset（⛔ 不调用 SetStatus(SpanError)）
	sp.End(tr)

	got := exp.Spans()
	if len(got) != 1 {
		t.Fatalf("导出 span 数 = %d，期望 1", len(got))
	}
	s := got[0]
	if s.Status != SpanUnset {
		t.Errorf("取消场景 span status = %v，期望 Unset（取消不是错误）", s.Status)
	}
	if s.HasErrorType {
		t.Errorf("取消场景设置了 error.type，期望不设")
	}
	if s.Attributes[AttrResponseStatus] != ResponseCancelled {
		t.Errorf("gen_ai.response.status = %v，期望 cancelled", s.Attributes[AttrResponseStatus])
	}
	if !s.Ended() || s.EndTime.IsZero() {
		t.Errorf("span 未收口（EndTime 未设置）⇒ 悬挂 span，违反 6.3 时间线")
	}
}

// TestSpanErrorSemantics 对照：上游 5xx ⇒ span status Error 且设 error.type。
func TestSpanErrorSemantics(t *testing.T) {
	exp := &InMemoryExporter{}
	tr := NewTracer(exp)
	sp := tr.Start(OperationChat)
	sp.SetAttr(AttrErrorType, "upstream_5xx")
	sp.SetAttr(AttrResponseStatus, ResponseFailed)
	sp.SetStatus(SpanError)
	sp.End(tr)

	s := exp.Spans()[0]
	if s.Status != SpanError || !s.HasErrorType {
		t.Errorf("失败场景应为 Status=Error 且设 error.type，实际 status=%v hasErrorType=%v",
			s.Status, s.HasErrorType)
	}
}

// TestSpanEndIdempotent 钉住 End 幂等（重复收口不重复导出）。
func TestSpanEndIdempotent(t *testing.T) {
	exp := &InMemoryExporter{}
	tr := NewTracer(exp)
	sp := tr.Start(OperationChat)
	sp.End(tr)
	sp.End(tr)
	if n := len(exp.Spans()); n != 1 {
		t.Errorf("重复 End 导出 %d 个 span，期望 1", n)
	}
}

// TestAttributeNamesGenAINamespace 钉住 6.1：自定义属性 ⛔ 不进 gen_ai. 命名空间。
func TestAttributeNamesGenAINamespace(t *testing.T) {
	for _, name := range []string{
		AttrGatewayProvider, AttrGatewayShape, AttrGatewayDetectConfidence,
		AttrGatewayFallback, AttrGatewayDegradations, AttrGatewayCacheReadTokens,
		AttrGatewayCacheWriteTokens, AttrGatewayUsageEstimated,
		AttrGatewayCancelReason, AttrGatewayCancelDetectedLatency,
		AttrGatewayCancelUpstreamClosed, AttrGatewayCancelTokensAfterCancel,
		AttrGatewayCancelDeferredMs,
	} {
		if len(name) >= 7 && name[:7] == "gen_ai." {
			t.Errorf("自定义属性 %s 不得使用 gen_ai. 前缀（6.1 封闭命名空间）", name)
		}
	}
}

// TestMetrics 钉住 6.4：取消计数独立于采样、按原因分列、usage 缺失率有值。
func TestMetrics(t *testing.T) {
	m := NewMetrics()
	m.ObserveRequest("openai", "gpt-6-luna", "cancelled")
	m.ObserveCancel(CancelClientDisconnect)
	m.ObserveCancel(CancelClientDisconnect)
	m.ObserveTokens("input", 100)
	m.ObserveTokens("output", 30)
	m.ObserveCancelLatency(120)
	m.ObserveUsage(true)
	m.ObserveUsage(false)

	if n := m.Requests("openai", "gpt-6-luna", "cancelled"); n != 1 {
		t.Errorf("gateway.requests = %d，期望 1", n)
	}
	if n := m.Cancelled(CancelClientDisconnect); n != 2 {
		t.Errorf("gateway.cancelled(client_disconnect) = %d，期望 2", n)
	}
	if n := m.Tokens("input"); n != 100 {
		t.Errorf("gateway.tokens(input) = %d，期望 100", n)
	}
	if r := m.UsageMissingRate(); r != 0.5 {
		t.Errorf("gateway.usage_missing_rate = %v，期望 0.5", r)
	}
}
