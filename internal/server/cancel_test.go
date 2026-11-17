// 日期: 2026-11-14 15:09:33.138
// 作者: Zhai Wenjia

package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/telemetry"
)

// 本文件是流式取消专项验收 SC-1–SC-8（统一 SDD 12.3）与延迟取消窗口（A13 口径）。
// 断连用真实 TCP 硬断（⛔ 不模拟）；假上游是真实 HTTP（httptest）回放 testdata 样本。

const scStreamBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`

// startStream 发起带 client_request_id 的流式请求并读 n 个事件块，返回连接读取器。
func startStream(t *testing.T, e *env, id string, n int) *rawStream {
	t.Helper()
	s := dialRaw(t, e.http.URL)
	host := strings.TrimPrefix(e.http.URL, "http://")
	s.send(t, "/v1/chat/completions", host, scStreamBody, map[string]string{"X-Client-Request-Id": id})
	for i := 0; i < n; i++ {
		if _, err := s.nextEvent(t); err != nil {
			t.Fatalf("读第 %d 个事件块失败: %v", i+1, err)
		}
	}
	return s
}

// snapOf 取执行观测快照（超时失败）。
func snapOf(t *testing.T, e *env, id string, timeout time.Duration) ExecSnapshot {
	t.Helper()
	var snap ExecSnapshot
	ok := waitUntil(timeout, func() bool {
		s, found := e.srv.ExecutionSnapshot(id)
		if found {
			snap = s
		}
		return found
	})
	if !ok {
		t.Fatalf("client_request_id=%s 的执行快照未就绪", id)
	}
	return snap
}

// waitSpan 等待含指定属性值的 span 导出（InMemoryExporter）。
func waitSpan(t *testing.T, e *env, key string, val any) *telemetry.Span {
	t.Helper()
	var found *telemetry.Span
	ok := waitUntil(3*time.Second, func() bool {
		for _, sp := range e.export.Spans() {
			if sp.Attributes[key] == val {
				found = sp
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatalf("未导出 %s=%v 的 span（共 %d 个）", key, val, len(e.export.Spans()))
	}
	return found
}

// TestSC1_ClientDisconnectClosesUpstreamWithinBound 断言 SC-1：客户端断连后网关在
// 有界延迟内关闭上游连接（关闭延迟 ≤ 默认上界 1s；假上游 closed 标志 + 实测延迟）。
func TestSC1_ClientDisconnectClosesUpstreamWithinBound(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse")[:2]) // created + 1 分片后挂住

	s := startStream(t, e, "sc-1", 2)
	tClose := time.Now()
	s.conn.Close() // 硬断（socket 级）

	if !waitUntil(2*time.Second, func() bool { _, _, closed, _ := e.up.stats(); return closed }) {
		t.Fatal("上游连接未在 2s 内关闭（SC-1 失败）")
	}
	_, _, _, closedAt := e.up.stats()
	latency := closedAt.Sub(tClose)
	t.Logf("SC-1 关闭延迟实测 = %v（上界 1s）", latency)
	if latency > time.Second {
		t.Errorf("关闭延迟 %v 超过上界 1s", latency)
	}
	snap := snapOf(t, e, "sc-1", 2*time.Second)
	if snap.State != "cancelled" {
		t.Errorf("执行状态 = %q，期望 cancelled", snap.State)
	}
	if !snap.UpstreamClosed {
		t.Error("gateway.cancel.upstream_closed = false，期望 true（真的关了上游）")
	}
}

// TestSC2_NoReadsAfterClose 断言 SC-2：上游关闭后不再读取（读取计数不增长）。
// 判别力：关闭未发生时假上游的持续推帧会让读取计数继续增长 ⇒ 错误实现会红。
func TestSC2_NoReadsAfterClose(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse")[:2])

	s := startStream(t, e, "sc-2", 2)
	s.conn.Close()

	if !waitUntil(2*time.Second, func() bool { return snapOf(t, e, "sc-2", time.Second).UpstreamClosed }) {
		t.Fatal("上游未关闭")
	}
	r1 := snapOf(t, e, "sc-2", time.Second).FramesRead
	for i := 0; i < 5; i++ {
		e.up.push() // 假上游继续吐帧（连接已关时写失败；未关时会被读到）
	}
	time.Sleep(400 * time.Millisecond)
	r2 := snapOf(t, e, "sc-2", time.Second).FramesRead
	t.Logf("SC-2 读取计数：取消后 %d → 推帧后 %d", r1, r2)
	if r2 != r1 {
		t.Errorf("取消后读取计数从 %d 增长到 %d（SC-2：关闭后不再读取）", r1, r2)
	}
}

// TestSC3_SpanCancelledUnsetErrorNoErrorType 断言 SC-3：span 已结束、
// gen_ai.response.status=cancelled、span status Unset（非 ERROR）、未设 error.type。
func TestSC3_SpanCancelledUnsetErrorNoErrorType(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse")[:2])

	s := startStream(t, e, "sc-3", 2)
	s.conn.Close()

	sp := waitSpan(t, e, telemetry.AttrResponseStatus, telemetry.ResponseCancelled)
	if !sp.Ended() {
		t.Error("span 未结束（end_time 未设置）——悬挂 span（SC-3 失败）")
	}
	if sp.Status != telemetry.SpanUnset {
		t.Errorf("span status = %v，期望 Unset（⛔ 取消不记为错误）", sp.Status)
	}
	if sp.HasErrorType {
		t.Error("error.type 被设置，取消场景 ⛔ 不设（SC-3 失败）")
	}
	if got := sp.Attributes[telemetry.AttrGatewayCancelReason]; got != reasonClientDisconnect {
		t.Errorf("gateway.cancel.reason = %v，期望 %q", got, reasonClientDisconnect)
	}
	if got := sp.Attributes[telemetry.AttrGatewayCancelUpstreamClosed]; got != true {
		t.Errorf("gateway.cancel.upstream_closed = %v，期望 true", got)
	}
}

// TestSC4_NoFramesAfterCancel 断言 SC-4：取消后不再向客户端转发任何分片
// （取消点后内容帧为 0）+ gateway.cancel.tokens_after_cancel == 0。
// 显式取消（POST /v1/cancel）路径：客户端保持连接读到收口。
func TestSC4_NoFramesAfterCancel(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse")[:2]) // created + 1 分片

	s := startStream(t, e, "sc-4", 2)

	// 显式取消（fire-and-forget）。
	resp := e.postJSON("/v1/cancel", `{"client_request_id":"sc-4"}`, nil)
	m := readBody(t, resp)
	if m["cancelled"] != true {
		t.Fatalf("显式取消返回 cancelled=%v，期望 true（命中在途执行）", m["cancelled"])
	}

	// 假上游再推迟到分片（⛔ 必须被丢弃）。
	for i := 0; i < 3; i++ {
		e.up.push()
	}

	// 读到终止哨兵（data: [DONE]）为止；⛔ 不依赖 EOF —— HTTP/1.1 keep-alive 下
	// 响应收口后连接保持打开、读不会返回 EOF（读超时兜底防挂）。
	_ = s.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var rest []string
	for i := 0; i < 32; i++ {
		block, err := s.nextEvent(t)
		if block != "" {
			rest = append(rest, block)
		}
		if hasSentinel(rest) || err != nil {
			break
		}
	}
	if got := countContent(rest); got != 0 {
		t.Errorf("取消点后客户端仍收到 %d 个内容分片（SC-4 失败）", got)
	}
	if !hasSentinel(rest) {
		t.Error("取消收口缺终止哨兵 data: [DONE]")
	}
	snap := snapOf(t, e, "sc-4", 2*time.Second)
	if snap.TokensAfterCancel != 0 {
		t.Errorf("gateway.cancel.tokens_after_cancel = %d，期望 0", snap.TokensAfterCancel)
	}
	if snap.FramesForwarded != 1 {
		t.Errorf("转发帧数 = %d，期望 1（取消点前的帧数，SC-4）", snap.FramesForwarded)
	}
}

// TestSC5_CancelSuppressesFallbackAndRejectsID 断言 SC-5：取消不触发令牌轮换 / 降级
// （primary 请求计数保持 1、backup 为 0）；同 client_request_id 的后续尝试被拒（409）。
// 判别力：取消引发的传输中断属可重试类（upstream_error）+ 降级链 upstream_error → backup ⇒
// 无取消抑制的实现会降级打 backup ⇒ backup 计数非 0 红。
func TestSC5_CancelSuppressesFallbackAndRejectsID(t *testing.T) {
	up2 := newFakeUpstream(t)
	t.Cleanup(up2.close)
	up2.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	e := newEnv(t, func(base string) []epSpec {
		primary := primaryEP(base)
		backup := primaryEP(up2.srv.URL)
		backup.name = "backup"
		backup.priority = 1
		return []epSpec{primary, backup}
	}, map[string][]string{"upstream_error": {"backup"}}, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse")[:2])

	// 非流式路径：帧已收未转发 ⇒ 降级可行域（无取消抑制的实现在取消后降级打 backup）。
	s := dialRaw(t, e.http.URL)
	host := strings.TrimPrefix(e.http.URL, "http://")
	s.send(t, "/v1/chat/completions", host,
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":false}`,
		map[string]string{"X-Client-Request-Id": "sc-5"})
	time.Sleep(150 * time.Millisecond)
	s.conn.Close()

	if !waitUntil(2*time.Second, func() bool { return snapOf(t, e, "sc-5", time.Second).UpstreamClosed }) {
		t.Fatal("上游未关闭")
	}
	n1, _, _, _ := e.up.stats()
	if n1 != 1 {
		t.Errorf("primary 上游请求计数 = %d，期望 1（⛔ 取消不触发轮换 / 降级，SC-5）", n1)
	}
	if nb, _, _, _ := up2.stats(); nb != 0 {
		t.Errorf("backup 上游请求计数 = %d，期望 0（⛔ 取消不触发降级，SC-5）", nb)
	}

	// 同 id 后续尝试必须被拒（取消终态不可回退）。
	resp := e.postJSON("/v1/chat/completions", chatHelloBody,
		map[string]string{"X-Client-Request-Id": "sc-5"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("同 id 后续尝试 = %d，期望 409", resp.StatusCode)
	}
	_, code, _ := errorFields(t, readBody(t, resp))
	if code != "request_cancelled" {
		t.Errorf("code = %q，期望 request_cancelled", code)
	}
	n2, _, _, _ := e.up.stats()
	if n2 != 1 {
		t.Errorf("被拒后 primary 上游请求计数 = %d，期望仍为 1", n2)
	}
}

// TestSC6_DetectionLatencyBoundedAndMeasured 断言 SC-6：断连检测延迟有上界且可测
// （socket 级实测延迟进 gateway.cancel.detected_latency_ms）。
func TestSC6_DetectionLatencyBoundedAndMeasured(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
		o.DisconnectPollInterval = 25 * time.Millisecond
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse")[:2])

	s := startStream(t, e, "sc-6", 2)
	tClose := time.Now()
	s.conn.Close()

	if !waitUntil(2*time.Second, func() bool { _, _, closed, _ := e.up.stats(); return closed }) {
		t.Fatal("上游未关闭")
	}
	_, _, _, closedAt := e.up.stats()
	measured := closedAt.Sub(tClose)
	t.Logf("SC-6 socket 级实测（断连→上游关闭）= %v", measured)
	if measured > time.Second {
		t.Errorf("实测延迟 %v 超过默认上界 1s", measured)
	}
	snap := snapOf(t, e, "sc-6", 2*time.Second)
	t.Logf("SC-6 网关实测（检测→关闭，进 gateway.cancel.detected_latency_ms）= %vms", snap.DetectedLatencyMs)
	if snap.DetectedLatencyMs <= 0 {
		t.Errorf("gateway.cancel.detected_latency_ms = %v，期望 > 0（实测值可测）", snap.DetectedLatencyMs)
	}
	if snap.DetectedLatencyMs > float64(measured.Milliseconds())+200 {
		t.Errorf("detected_latency_ms = %vms 超出 socket 级实测 %v + 200ms 裕量", snap.DetectedLatencyMs, measured)
	}
	// span 属性同样带实测值（SC-6：进 gateway.cancel.detected_latency_ms）。
	sp := waitSpan(t, e, telemetry.AttrResponseStatus, telemetry.ResponseCancelled)
	if v, ok := sp.Attributes[telemetry.AttrGatewayCancelDetectedLatency]; !ok || v.(float64) <= 0 {
		t.Errorf("span 缺 gateway.cancel.detected_latency_ms 实测值：%v", sp.Attributes)
	}
}

// TestSC7_NonStreamDisconnectCancels 断言 SC-7：非流式路径客户端断开同样被取消
// （⛔ 不依赖框架默认行为，自挂 watcher）。
func TestSC7_NonStreamDisconnectCancels(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse")[:2])

	s := dialRaw(t, e.http.URL)
	host := strings.TrimPrefix(e.http.URL, "http://")
	s.send(t, "/v1/chat/completions", host,
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":false}`,
		map[string]string{"X-Client-Request-Id": "sc-7"})
	time.Sleep(100 * time.Millisecond)
	tClose := time.Now()
	s.conn.Close()

	if !waitUntil(2*time.Second, func() bool { _, _, closed, _ := e.up.stats(); return closed }) {
		t.Fatal("上游未在 2s 内关闭（SC-7 非流式路径未被取消）")
	}
	_, _, _, closedAt := e.up.stats()
	t.Logf("SC-7 非流式断连→上游关闭 = %v", closedAt.Sub(tClose))
	if closedAt.Sub(tClose) > time.Second {
		t.Errorf("非流式关闭延迟 %v 超过上界 1s", closedAt.Sub(tClose))
	}
	snap := snapOf(t, e, "sc-7", 2*time.Second)
	if snap.State != "cancelled" {
		t.Errorf("执行状态 = %q，期望 cancelled（SC-7）", snap.State)
	}
	if snap.CancelReason != reasonClientDisconnect {
		t.Errorf("gateway.cancel.reason = %q，期望 client_disconnect", snap.CancelReason)
	}
}

// TestSC8_CancelReasonsDistinguishable 断言 SC-8：客户端断连与网关主动收口的原因可区分
// （gateway.cancel.reason 分别为 client_disconnect / upstream_error / timeout）。
func TestSC8_CancelReasonsDistinguishable(t *testing.T) {
	t.Run("client_disconnect", func(t *testing.T) {
		e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
		e.up.setEvents(loadSSE(t, "openai_responses_stream.sse")[:2])
		s := startStream(t, e, "sc-8a", 2)
		s.conn.Close()
		sp := waitSpan(t, e, telemetry.AttrGatewayCancelReason, reasonClientDisconnect)
		if sp.Attributes[telemetry.AttrResponseStatus] != telemetry.ResponseCancelled {
			t.Errorf("断连路径 response.status = %v，期望 cancelled", sp.Attributes[telemetry.AttrResponseStatus])
		}
	})

	t.Run("upstream_error", func(t *testing.T) {
		e := newEnv(t, func(base string) []epSpec {
			ep := primaryEP(base)
			return []epSpec{ep}
		}, nil, nil)
		e.up.setEvents(loadSSE(t, "openai_responses_failed.sse")) // 1 分片后 response.failed

		resp := e.postJSON("/v1/chat/completions", scStreamBody,
			map[string]string{"X-Client-Request-Id": "sc-8b"})
		readRaw(t, resp)

		sp := waitSpan(t, e, telemetry.AttrGatewayCancelReason, reasonUpstreamError)
		if sp.Attributes[telemetry.AttrResponseStatus] != telemetry.ResponseFailed {
			t.Errorf("上游失败路径 response.status = %v，期望 failed（6.3 三情形表）", sp.Attributes[telemetry.AttrResponseStatus])
		}
		if sp.Attributes[telemetry.AttrErrorType] != string("upstream_error") {
			t.Errorf("error.type = %v，期望 upstream_error（失败要记错误）", sp.Attributes[telemetry.AttrErrorType])
		}
	})

	t.Run("timeout", func(t *testing.T) {
		e := newEnv(t, func(base string) []epSpec {
			ep := primaryEP(base)
			ep.timeoutS = 0.3
			return []epSpec{ep}
		}, nil, nil)
		e.up.setEvents(nil) // 上游挂住不回（触发超时）

		resp := e.postJSON("/v1/chat/completions", chatHelloBody,
			map[string]string{"X-Client-Request-Id": "sc-8c"})
		readRaw(t, resp)

		sp := waitSpan(t, e, telemetry.AttrGatewayCancelReason, reasonTimeout)
		if sp.Attributes[telemetry.AttrErrorType] != string("timeout") {
			t.Errorf("error.type = %v，期望 timeout", sp.Attributes[telemetry.AttrErrorType])
		}
	})
}

// TestCancelGracePeriodDeferred 断言延迟取消窗口（A13 口径，统一 SDD 3.7）：
// cancel_grace_period > 0 时断连 ⇒ CANCEL_PENDING（上游连接保持打开、继续解码与计费、
// ⛔ 不转发）；窗口到期关闭；显式取消可提前结束；定时器无泄漏。
func TestCancelGracePeriodDeferred(t *testing.T) {
	t.Run("窗口到期收口", func(t *testing.T) {
		e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
			o.CancelGracePeriod = 400 * time.Millisecond
		})
		e.up.setEvents(loadSSE(t, "openai_responses_stream.sse")[:2])

		s := startStream(t, e, "grace-1", 2)
		s.conn.Close()

		// 窗口内：连接保持打开、迟到分片只读不转发。
		if !waitUntil(2*time.Second, func() bool {
			return snapOf(t, e, "grace-1", time.Second).State == "cancel_pending"
		}) {
			t.Fatal("未进入 CANCEL_PENDING（延迟取消窗口未生效）")
		}
		for i := 0; i < 3; i++ {
			e.up.push()
		}
		time.Sleep(150 * time.Millisecond)
		mid := snapOf(t, e, "grace-1", time.Second)
		if mid.FramesForwarded != 1 {
			t.Errorf("窗口内转发帧数 = %d，期望仍为 1（⛔ 不向客户端转发）", mid.FramesForwarded)
		}
		if mid.TokensAfterCancel != 0 {
			t.Errorf("窗口内 tokens_after_cancel = %d，期望 0", mid.TokensAfterCancel)
		}
		if mid.FramesRead <= 2 {
			t.Errorf("窗口内 FramesRead = %d，期望 > 2（连接保持打开、继续解码）", mid.FramesRead)
		}

		// 窗口到期 ⇒ CANCELLED + 上游关闭。
		if !waitUntil(2*time.Second, func() bool {
			return snapOf(t, e, "grace-1", time.Second).State == "cancelled"
		}) {
			t.Fatal("窗口到期后未收口 CANCELLED")
		}
		final := snapOf(t, e, "grace-1", time.Second)
		t.Logf("grace-1 deferred_ms = %vms", final.DeferredMs)
		if final.DeferredMs < 300 {
			t.Errorf("gateway.cancel.deferred_ms = %vms，期望 ≥ 300（窗口 400ms）", final.DeferredMs)
		}
		if !waitUntil(2*time.Second, func() bool { _, _, closed, _ := e.up.stats(); return closed }) {
			t.Error("窗口到期后上游连接未关闭")
		}
		if n := e.srv.cancels.timerCount(); n != 0 {
			t.Errorf("延迟窗口定时器残留 %d 个，期望 0（⛔ 定时器泄漏 = 连接泄漏）", n)
		}
		if n := e.srv.CancelPendingCount(); n != 0 {
			t.Errorf("cancel_pending 计数 = %d，期望 0", n)
		}
	})

	t.Run("显式取消提前收口", func(t *testing.T) {
		e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, func(o *Options) {
			o.CancelGracePeriod = 10 * time.Second // 长窗口：只能被显式取消提前结束
		})
		e.up.setEvents(loadSSE(t, "openai_responses_stream.sse")[:2])

		s := startStream(t, e, "grace-2", 2)
		s.conn.Close()
		if !waitUntil(2*time.Second, func() bool {
			return snapOf(t, e, "grace-2", time.Second).State == "cancel_pending"
		}) {
			t.Fatal("未进入 CANCEL_PENDING")
		}
		t0 := time.Now()
		resp := e.postJSON("/v1/cancel", `{"client_request_id":"grace-2"}`, nil)
		readBody(t, resp)
		if !waitUntil(2*time.Second, func() bool {
			return snapOf(t, e, "grace-2", time.Second).State == "cancelled"
		}) {
			t.Fatal("显式取消未提前收口")
		}
		if d := time.Since(t0); d > time.Second {
			t.Errorf("显式取消收口耗时 %v，期望远小于 10s 窗口（⛔ 不傻等）", d)
		}
		if n := e.srv.cancels.timerCount(); n != 0 {
			t.Errorf("延迟窗口定时器残留 %d 个，期望 0（提前收口要清定时器）", n)
		}
	})
}
