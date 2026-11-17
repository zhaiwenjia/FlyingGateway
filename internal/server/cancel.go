// 日期: 2026-11-14 14:35:15.347
// 作者: Zhai Wenjia

package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/telemetry"
)

// 本文件承载取消注册表与执行句柄（统一 SDD 3.7 / 6.6 / 7.5）：
//   - client_request_id → 上游执行句柄 映射（fire-and-forget 语义）；
//   - 状态机 RUNNING →（cancel_grace_period > 0 时）CANCEL_PENDING → CANCELLED，
//     终态不可回退（统一 SDD 3.7 实施要点）；
//   - 取消原因四分类（client_disconnect / upstream_error / timeout / shutdown）；
//   - 清理三路径：帧循环 finally / 请求级 finally / 进程关闭，定时器三处都清
//     （⛔ 定时器泄漏 = 上游连接泄漏，统一 SDD 3.7）；
//   - 幂等收口：取消与「上游刚好完成」并发 ⇒ 只记一次取消；查不到 / 已收口 ⇒ 静默忽略
//     （统一 SDD 7.5 竞态容忍）。

// execState 是取消注册表状态机取值（统一 SDD 3.7）。
type execState int

const (
	// stateRunning：上游执行进行中。
	stateRunning execState = iota
	// stateCancelPending：延迟取消窗口内（非终态），上游连接保持打开（统一 SDD 3.7）。
	stateCancelPending
	// stateCancelled：已取消（终态，不可回退）。
	stateCancelled
	// stateDone：正常收口（终态）。
	stateDone
)

// String 返回状态名（观测快照用）。
func (s execState) String() string {
	switch s {
	case stateRunning:
		return "running"
	case stateCancelPending:
		return "cancel_pending"
	case stateCancelled:
		return "cancelled"
	case stateDone:
		return "done"
	default:
		return "unknown"
	}
}

// 取消原因四分类（统一 SDD 6.6 要求 4 / 6.3）：直接复用 telemetry 常量，
// ⛔ 不另立第二套取值（双真值会漂移）。
const (
	reasonClientDisconnect = telemetry.CancelClientDisconnect // client_disconnect
	reasonUpstreamError    = telemetry.CancelUpstreamError    // upstream_error
	reasonTimeout          = telemetry.CancelTimeout          // timeout
	reasonShutdown         = telemetry.CancelShutdown         // shutdown
)

var (
	// errRequestInFlight 是「同 client_request_id 已有执行在途」的哨兵错误。
	errRequestInFlight = errors.New("server: 同 client_request_id 已有执行在途")
	// errRequestCancelled 是「client_request_id 已进取消终态」的哨兵错误
	// （统一 SDD 7.5：进入取消终态的 id 拒绝后续尝试，SC-5）。
	errRequestCancelled = errors.New("server: client_request_id 已进取消终态")
)

// ExecSnapshot 是一次上游执行的观测快照（统一 SDD 6.5 L1 网关自证 / 6.3 取消口径）。
// 供测试断言与排障读取：取消后读取计数冻结（SC-2）、取消后转发 token 为 0（SC-4）、
// 检测→关闭延迟（SC-6）都在这里逐字可查。
type ExecSnapshot struct {
	// ClientRequestID 是本轮对话的唯一标识（客户端提供或网关生成）。
	ClientRequestID string
	// State 是状态机取值（running / cancel_pending / cancelled / done）。
	State string
	// CancelReason 是取消原因四分类之一（未取消时为空）。
	CancelReason string
	// UpstreamStarted 表示上游执行句柄已建立（已发起上游调用）。
	UpstreamStarted bool
	// UpstreamClosed 表示上游资源已收口（统一 SDD 6.3 gateway.cancel.upstream_closed）。
	UpstreamClosed bool
	// FramesRead 是从上游帧迭代器读出的帧数（SC-2 取消后不再增长）。
	FramesRead int64
	// FramesForwarded 是已转发给客户端的帧数。
	FramesForwarded int64
	// TokensAfterCancel 是取消触发后仍转发给客户端的 token 数（应为 0，SC-4）。
	TokensAfterCancel int64
	// DetectedLatencyMs 是「检测到取消 → 上游收口」的实测延迟（统一 SDD 6.3：
	// gateway.cancel.detected_latency_ms「从检测到断连到关闭上游的延迟」，SC-6）。
	DetectedLatencyMs float64
	// DeferredMs 是延迟取消窗口实际延迟多久才执行取消（统一 SDD 3.7
	// gateway.cancel.deferred_ms；立即取消路径为 0）。
	DeferredMs float64
}

// execHandle 是一次上游执行的取消句柄（统一 SDD 6.6 实现清单「取消注册表」）。
type execHandle struct {
	id string
	// startedAt 是执行开始时刻（费率快照与费用口径的计费时点，统一 SDD 5.6）。
	startedAt time.Time
	// ctx 是本次执行的上游执行上下文：客户端断连（r.Context 取消）、显式取消、
	// 超时、进程关闭四条路都汇到它的取消上。
	ctx    context.Context
	cancel context.CancelFunc

	mu            sync.Mutex
	state         execState
	reason        string
	provider      string        // 当前尝试落到的 provider（按 provider 覆盖 cancel_grace_period 用）
	upstreamModel string        // 当前执行的上游模型标识（费率查询用）
	finalUsageVal *core.Usage   // 终态用量（请求级记账用）
	retryHint     time.Duration // 本地池闸拒绝的最短等待（Retry-After 头取值）
	grace         func(provider string) time.Duration
	timer         *time.Timer // 延迟窗口定时器（三路径清理，⛔ 不泄漏）
	detectedAt    time.Time   // 取消触发（检测）时刻
	deferredAt    time.Time   // 实际执行取消的时刻（延迟取消观测用）
	upstreamDone  bool        // 上游资源是否已收口（幂等置位）
	closedAt      time.Time   // 上游收口时刻（detected_latency 用）

	// 计数（原子；帧循环与观测快照跨 goroutine 读）。
	started         atomic.Bool
	written         atomic.Bool
	framesRead      atomic.Int64
	framesForwarded atomic.Int64
	tokensAfter     atomic.Int64
}

// setExecTarget 登记当前尝试的 provider 与上游模型（延迟窗口按 provider 覆盖取值；
// 计费查费率表按上游模型）。
func (h *execHandle) setExecTarget(provider, upstreamModel string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.provider = provider
	h.upstreamModel = upstreamModel
}

// upstreamModelName 返回当前执行的上游模型标识（费率查询用）。
func (h *execHandle) upstreamModelName() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.upstreamModel
}

// setFinalUsage 登记终态用量（请求级记账用；计费至取消点）。
func (h *execHandle) setFinalUsage(u *core.Usage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.finalUsageVal = u
}

// finalUsage 返回终态用量（未执行过 ⇒ nil）。
func (h *execHandle) finalUsage() *core.Usage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.finalUsageVal
}

// markWritten 记一次客户端写入成功（流已提交 —— 此后 ⛔ 不再重试 / 降级）。
func (h *execHandle) markWritten() { h.written.Store(true) }

// committed 返回是否已向客户端提交（写了响应头 / 分片 / 终止帧）。
func (h *execHandle) committed() bool { return h.written.Load() }

// setRetryAfterHint 记一次「本地池闸拒绝」的最短等待（token_pool_exhausted 的
// Retry-After 头取值；core.ErrorDetail 无该槽位，经句柄带到写响应处）。
func (h *execHandle) setRetryAfterHint(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retryHint = d
}

// retryAfterHint 返回本地池闸拒绝的最短等待。
func (h *execHandle) retryAfterHint() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.retryHint
}

// markUpstreamStarted 登记上游执行已发起。
func (h *execHandle) markUpstreamStarted() { h.started.Store(true) }

// noteFrameRead 记一次上游帧读取（SC-2：取消后不再读取的观测量）。
func (h *execHandle) noteFrameRead() { h.framesRead.Add(1) }

// noteFrameForwarded 记一次客户端转发；取消触发后转发的 token 计入
// tokens_after_cancel（SC-4 应为 0）。
func (h *execHandle) noteFrameForwarded(tokens int64) {
	h.framesForwarded.Add(1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.detectedAt.IsZero() {
		h.tokensAfter.Add(tokens)
	}
}

// markUpstreamClosed 登记上游资源收口（帧循环 finally 与请求级 finally 都会调，
// 幂等）；detected_latency / deferred 观测值在这一刻定格。
func (h *execHandle) markUpstreamClosed() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.upstreamDone {
		return
	}
	h.upstreamDone = true
	h.closedAt = time.Now()
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
}

// trigger 触发一次取消（统一 SDD 3.7 状态机 + 显式取消优先 + 幂等收口）。
//   - RUNNING：cancel_grace_period > 0 且非显式、非进程关闭 ⇒ 进 CANCEL_PENDING
//     （上游连接保持打开），窗口到期才真正取消；否则立即 CANCELLED；
//   - CANCEL_PENDING：显式取消（POST /v1/cancel）或进程关闭 ⇒ 立即收口，不傻等窗口；
//     其余触发只记一次（幂等）；
//   - CANCELLED / DONE：静默忽略（统一 SDD 7.5 竞态容忍，⛔ 不报错）。
//
// 返回本次触发是否真的推进了状态（观测计数用）。
func (h *execHandle) trigger(reason string, explicit bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch h.state {
	case stateCancelled, stateDone:
		return false
	case stateCancelPending:
		if explicit || reason == reasonShutdown {
			h.stopTimerLocked()
			h.doCancelLocked(reason)
			return true
		}
		// 已在延迟窗口内：重复触发不重置窗口（⛔ 重挂机制不做，统一 SDD 3.7）。
		return false
	case stateRunning:
		grace := time.Duration(0)
		if h.grace != nil {
			grace = h.grace(h.provider)
		}
		// 延迟窗口只挂「隐式客户端断连」：显式取消（POST /v1/cancel）立即收口
		// （统一 SDD 3.7「显式取消可提前结束」）；上游失败 / 超时 / 进程关闭的收口
		// 没有等待语义（上游流已死 / 进程要退），一律立即取消。
		if grace > 0 && !explicit && reason == reasonClientDisconnect {
			h.state = stateCancelPending
			if h.reason == "" {
				h.reason = reason
			}
			h.detectedAt = time.Now()
			r := h.reason
			h.timer = time.AfterFunc(grace, func() {
				h.mu.Lock()
				defer h.mu.Unlock()
				// 窗口到期才真正取消；期间被显式取消 / 收口则这里静默无事。
				if h.state == stateCancelPending {
					h.stopTimerLocked()
					h.doCancelLocked(r)
				}
			})
			return true
		}
		h.doCancelLocked(reason)
		return true
	}
	return false
}

// doCancelLocked 执行真正的取消（调用方持锁）：进终态 CANCELLED + 取消上游上下文。
func (h *execHandle) doCancelLocked(reason string) {
	h.state = stateCancelled
	if h.reason == "" {
		h.reason = reason
	}
	if h.detectedAt.IsZero() {
		h.detectedAt = time.Now()
	}
	h.deferredAt = time.Now()
	h.cancel()
}

// stopTimerLocked 清延迟窗口定时器（调用方持锁；三路径清理共用，幂等）。
func (h *execHandle) stopTimerLocked() {
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
}

// isCancelled 返回执行是否已进取消终态（帧循环据此停止读取，SC-2）。
func (h *execHandle) isCancelled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state == stateCancelled
}

// isCancelPending 返回是否处于延迟取消窗口（3.7：窗口内连接保持打开、继续解码与计费、
// ⛔ 不向客户端转发）。
func (h *execHandle) isCancelPending() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state == stateCancelPending
}

// cancelReason 返回取消 / 收口原因（四分类；未触发为空）。
// 注意：上游失败 / 超时的收口原因经 markTeardownReason 记录（不进取消终态 ——
// 失败的 id 仍可复用，⛔ 不误拒为「已取消」）。
func (h *execHandle) cancelReason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason
}

// markTeardownReason 记录收口原因（网关主动收口：upstream_error / timeout）：
// 只记原因供 gateway.cancel.reason 观测（统一 SDD 6.3 / SC-8），⛔ 不改状态机 ——
// 取消终态只属客户端取消 / 显式取消 / 进程关闭（同 id 拒绝语义只对取消成立，7.5）。
func (h *execHandle) markTeardownReason(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reason == "" {
		h.reason = reason
	}
}

// snapshot 返回观测快照（detected_latency / deferred 在收口后定格；全部字段持锁读，⛔ 无锁外裸读）。
func (h *execHandle) snapshot() ExecSnapshot {
	h.mu.Lock()
	state := h.state
	reason := h.reason
	if state != stateCancelled {
		reason = ""
	}
	closed := h.upstreamDone
	var latencyMs, deferredMs float64
	if !h.detectedAt.IsZero() && !h.closedAt.IsZero() {
		latencyMs = float64(h.closedAt.Sub(h.detectedAt)) / float64(time.Millisecond)
	}
	if !h.detectedAt.IsZero() && !h.deferredAt.IsZero() && state == stateCancelled {
		deferredMs = float64(h.deferredAt.Sub(h.detectedAt)) / float64(time.Millisecond)
	}
	h.mu.Unlock()
	return ExecSnapshot{
		ClientRequestID:   h.id,
		State:             state.String(),
		CancelReason:      reason,
		UpstreamStarted:   h.started.Load(),
		UpstreamClosed:    closed,
		FramesRead:        h.framesRead.Load(),
		FramesForwarded:   h.framesForwarded.Load(),
		TokensAfterCancel: h.tokensAfter.Load(),
		DetectedLatencyMs: latencyMs,
		DeferredMs:        deferredMs,
	}
}

// cancelRegistry 是取消注册表（统一 SDD 6.6 实现清单）。
type cancelRegistry struct {
	mu        sync.Mutex
	entries   map[string]*execHandle
	grace     time.Duration            // 全局 cancel_grace_period（秒，默认 0 = 立即取消）
	byProv    map[string]time.Duration // 按 provider 覆盖（统一 SDD 3.7 作用域）
	retention time.Duration            // 取消终态条目的保留时长（过后清除，防无界增长）
	now       func() time.Time         // 时间注入点（测试用；默认 time.Now）
}

// newCancelRegistry 构造取消注册表。grace 默认 0（立即取消）。
func newCancelRegistry(grace time.Duration, byProvider map[string]time.Duration, retention time.Duration) *cancelRegistry {
	if retention <= 0 {
		retention = 10 * time.Minute
	}
	by := make(map[string]time.Duration, len(byProvider))
	for k, v := range byProvider {
		by[k] = v
	}
	return &cancelRegistry{
		entries:   make(map[string]*execHandle),
		grace:     grace,
		byProv:    by,
		retention: retention,
		now:       time.Now,
	}
}

// graceFor 取该 provider 的延迟窗口（未配置 ⇒ 全局值）。
func (r *cancelRegistry) graceFor(provider string) time.Duration {
	if g, ok := r.byProv[provider]; ok {
		return g
	}
	return r.grace
}

// register 注册一次执行（统一 SDD 7.5：进取消终态的 id 拒绝后续尝试，SC-5）。
// 同 id 在途 ⇒ errRequestInFlight；已取消终态（保留期内）⇒ errRequestCancelled。
//
// ⭐ 执行上下文 ⛔ 不挂在请求上下文下：延迟取消窗口要求「客户端断连后上游连接保持打开、
// 继续解码与计费」（统一 SDD 3.7 / A13）——若 h.ctx 是 r.Context() 的子上下文，断连会
// 立刻连带取消上游、窗口被整体绕过（实测：cancel_grace_period>0 时断连后快照直接消失、
// 窗口从未生效）。取消统一走三条显式路径：trigger（断连 / 显式 / 关闭）、
// 每次上游调用的超时、finish（请求级收口）。
func (r *cancelRegistry) register(id string) (*execHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgeExpiredLocked()
	if old, ok := r.entries[id]; ok {
		old.mu.Lock()
		st := old.state
		old.mu.Unlock()
		switch st {
		case stateCancelled:
			return nil, fmt.Errorf("%w: id=%s", errRequestCancelled, id)
		case stateCancelPending:
			return nil, fmt.Errorf("%w: id=%s（取消进行中）", errRequestCancelled, id)
		case stateRunning:
			return nil, fmt.Errorf("%w: id=%s", errRequestInFlight, id)
		case stateDone:
			// 已正常收口的旧条目（防御性：finish 本应已移除）⇒ 允许新轮使用。
			delete(r.entries, id)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &execHandle{
		id:        id,
		startedAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
		state:     stateRunning,
		grace:     r.graceFor,
	}
	r.entries[id] = h
	return h, nil
}

// purgeExpiredLocked 清除超过保留期的取消终态条目（惰性清理，⛔ 不靠后台定时器）。
func (r *cancelRegistry) purgeExpiredLocked() {
	now := r.now()
	for id, h := range r.entries {
		h.mu.Lock()
		st := h.state
		at := h.deferredAt
		h.mu.Unlock()
		if st == stateCancelled && !at.IsZero() && now.Sub(at) > r.retention {
			h.cancel()
			delete(r.entries, id)
		}
	}
}

// finish 请求级收口（请求级 finally，与帧循环 finally 独立、幂等）：
// 正常完成的条目立即移除；取消终态条目保留 retention（供同 id 后续尝试被拒，SC-5）。
// 无论哪条路径都清理延迟窗口定时器。
func (r *cancelRegistry) finish(h *execHandle) {
	h.mu.Lock()
	h.stopTimerLocked()
	if h.state == stateRunning || h.state == stateCancelPending {
		// 请求结束即收口（正常完成或随请求结束而终止）。
		h.state = stateDone
		h.cancel()
	}
	keep := h.state == stateCancelled
	h.mu.Unlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if !keep {
		if cur, ok := r.entries[h.id]; ok && cur == h {
			delete(r.entries, h.id)
		}
	}
}

// cancelByID 显式取消（POST /v1/cancel，统一 SDD 3.7「显式取消优先」）。
// 查不到 / 已收口 ⇒ 静默忽略（fire-and-forget，⛔ 不报错，统一 SDD 7.5）。
// 返回是否命中在途执行。
func (r *cancelRegistry) cancelByID(id string) bool {
	r.mu.Lock()
	h, ok := r.entries[id]
	r.mu.Unlock()
	if !ok {
		return false
	}
	return h.trigger(reasonClientDisconnect, true)
}

// cancelAll 进程关闭时取消全部在途执行（原因 shutdown，立即收口不等窗口）。
func (r *cancelRegistry) cancelAll(reason string) {
	r.mu.Lock()
	handles := make([]*execHandle, 0, len(r.entries))
	for _, h := range r.entries {
		handles = append(handles, h)
	}
	r.mu.Unlock()
	for _, h := range handles {
		h.trigger(reason, false)
	}
}

// pendingCount 返回当前处于 CANCEL_PENDING 的请求数（统一 SDD 3.7
// gateway.cancel.pending_count）。
func (r *cancelRegistry) pendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, h := range r.entries {
		h.mu.Lock()
		if h.state == stateCancelPending {
			n++
		}
		h.mu.Unlock()
	}
	return n
}

// timerCount 返回仍在挂的延迟窗口定时器数（A13「定时器无泄漏：请求结束后计数归零」的观测量）。
func (r *cancelRegistry) timerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, h := range r.entries {
		h.mu.Lock()
		if h.timer != nil {
			n++
		}
		h.mu.Unlock()
	}
	return n
}

// snapshot 返回指定 id 的执行观测快照（条目不在 ⇒ false）。
func (r *cancelRegistry) snapshot(id string) (ExecSnapshot, bool) {
	r.mu.Lock()
	h, ok := r.entries[id]
	r.mu.Unlock()
	if !ok {
		return ExecSnapshot{}, false
	}
	return h.snapshot(), true
}
