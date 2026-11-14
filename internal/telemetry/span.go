// 日期: 2026-11-14 11:47:34.859
// 作者: Zhai Wenjia

package telemetry

import (
	"sync"
	"time"
)

// SpanKind 是 span 种类（统一 SDD 6.2：每次上游执行一个 CLIENT span）。
const SpanKindClient = "CLIENT"

// SpanStatus 口径（统一 SDD 6.3）：操作无错结束保持 Unset；
// ⛔ 取消不记为错误（span status 留 Unset、不设 error.type）。
type SpanStatus int

const (
	SpanUnset SpanStatus = iota
	SpanError
)

// Span 是一次上游执行的 span。降级链上每个端点各自独立 span。
// 自动重试合并进同一个 span（统一 SDD 6.2）。
type Span struct {
	mu sync.Mutex

	Name         string // span 名 = {gen_ai.operation.name}
	Kind         string
	Attributes   map[string]any
	Status       SpanStatus
	HasErrorType bool // error.type 是否被设置（⛔ 取消场景必须 false）

	StartTime time.Time
	EndTime   time.Time
	ended     bool
}

// Exporter 是 span 导出口（统一 SDD 6.1：导出侧做内容遮蔽决策）。
type Exporter interface {
	Export(spans []*Span)
}

// Tracer 构造 span 并在 End 时导出。
type Tracer struct {
	exporter Exporter
}

// NewTracer 构造 Tracer。
func NewTracer(e Exporter) *Tracer { return &Tracer{exporter: e} }

// Start 创建并开始一个 span；span 名 = gen_ai.operation.name 取值（统一 SDD 6.2）。
func (t *Tracer) Start(name string) *Span {
	return &Span{
		Name:       name,
		Kind:       SpanKindClient,
		Attributes: map[string]any{},
		StartTime:  time.Now(),
	}
}

// SetAttr 设置属性；error.type 的设置被单独记录（供取消口径断言）。
func (s *Span) SetAttr(key string, val any) *Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == AttrErrorType {
		s.HasErrorType = true
	}
	s.Attributes[key] = val
	return s
}

// SetStatus 设置 span status。
func (s *Span) SetStatus(st SpanStatus) *Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = st
	return s
}

// End 结束 span 并导出（幂等：重复 End 不重复导出）。
// 取消发生的那一刻即结束 span（⛔ 不悬挂，统一 SDD 6.3 时间线）。
func (s *Span) End(t *Tracer) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.EndTime = time.Now()
	s.mu.Unlock()
	if t != nil && t.exporter != nil {
		t.exporter.Export([]*Span{s})
	}
}

// Ended 返回 span 是否已结束（SC-3 断言用）。
func (s *Span) Ended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// InMemoryExporter 把 span 收在内存里供测试断言（SC-3：用 InMemorySpanExporter）。
type InMemoryExporter struct {
	mu    sync.Mutex
	spans []*Span
}

// Export 实现 Exporter。
func (e *InMemoryExporter) Export(spans []*Span) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
}

// Spans 返回已导出的 span 快照。
func (e *InMemoryExporter) Spans() []*Span {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Span, len(e.spans))
	copy(out, e.spans)
	return out
}
