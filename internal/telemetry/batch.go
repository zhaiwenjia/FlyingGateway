// 日期: 2026-11-14 19:54:26.023
// 作者: Zhai Wenjia

package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ---- 批量/异步 slog.Handler（PERF-PROFILE 建议 2：span 日志同步落盘占 CPU 16–18%）----
//
// 结构：有界队列（chan）+ 后台 worker 批量 dispatch + BatchWriter 合并写（一次 write 系统调用
// 一批日志）+ Shutdown 收口 flush（排空队列 + 最终 flush）+ 溢出显式丢弃计数。
//
// 溢出策略（⛔ 不静默丢，三通道显式）：队列满 ⇒ Handle 返回 ErrLogDropped + Dropped() 原子计数
// + Shutdown 收口补一条汇总记录（dropped 计数进日志本体）。
// 为什么不阻塞等待：Handle 在请求热路径上，背压阻塞会把日志成本转嫁回请求延迟（正是本轮
// 要消除的形态）；为什么不无限队列：内存不可控，慢盘/管道卡死时 OOM 比丢日志更糟。
//
// 收口策略：Shutdown 先关入口（closed 位 + RWMutex 保证与入队互斥，⛔ 收口后入队变静默丢失）
// ⇒ worker 排空队列 ⇒ flusher.Flush 一次收口 ⇒（有丢弃时）补汇总记录。
// 幂等：sync.Once，可多处调用。
//
// 日志内容/字段语义不变：本 Handler 不改写记录，格式仍由内层 slog.Handler（如 TextHandler）
// 决定；仅把「每条一次 write」合并成「一批一次 write」，并把写盘移出调用方 goroutine。
// ctx 不入队（⛔ 不持有调用方请求上下文）：worker 统一用 context.Background() 回放，
// 与 slog.TextHandler 的 ctx 用法一致（其不读 ctx）。

// ErrLogDropped 是溢出显式丢弃的哨兵错误（Handle 返回 + Dropped() 计数 + 收口汇总，三通道）。
var ErrLogDropped = errors.New("telemetry: 日志队列满，本条已丢弃（计入 Dropped()，收口汇总）")

// Flusher 是缓冲写手的收口口（BatchWriter 实现；nil 表示内层无缓冲、无需收口）。
type Flusher interface {
	Flush() error
}

// BatchOptions 是批量/异步 Handler 配置（零值字段取默认）。
type BatchOptions struct {
	// QueueSize 是有界队列容量（默认 4096）。
	QueueSize int
	// BatchSize 是 worker 单轮批量 dispatch 上限（默认 128）。
	BatchSize int
	// FlushInterval 是后台批量 flush 的空闲兜底间隔（默认 200ms）：
	// 正常路径 worker 每轮 dispatch 后即 flush，此间隔只兜「写了没几条就闲下来」的滞留。
	FlushInterval time.Duration
}

const (
	defaultQueueSize     = 4096
	defaultBatchSize     = 128
	defaultFlushInterval = 200 * time.Millisecond
)

// batchItem 是队列元素：目标内层 handler（WithAttrs/WithGroup 派生后各自带自己的内层）
// + 记录副本（Clone：调用方返回后记录内存可复用，⛔ 不留引用）。
type batchItem struct {
	h   slog.Handler
	rec slog.Record
}

// BatchHandler 是批量/异步 slog.Handler：Handle 只入队（非阻塞），worker goroutine 批量
// 回放到内层 handler 并驱动批量 flush。
type BatchHandler struct {
	inner   slog.Handler
	flusher Flusher
	ch      chan batchItem

	batchSize     int
	flushInterval time.Duration

	// mu 保护 closed 与入队的组合原子性：Handle 持 RLock 完成「判 closed + 入队」，
	// Shutdown 持 Lock 置位 closed ⇒ 收口时刻之后 ⛔ 不存在「已入队但没人取」的静默丢失。
	mu     sync.RWMutex
	closed bool

	quit      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	dropped atomic.Uint64

	// flushErr 记首个 flush 错误（worker 内写、Shutdown 在 worker 退出后读；Once 保 HB）。
	flushErr error
}

// NewBatchHandler 构造批量/异步 Handler 并启动后台 worker。
// inner：内层 handler（格式持有者）；flusher：内层写出口的收口器（可 nil）。
func NewBatchHandler(inner slog.Handler, flusher Flusher, opts BatchOptions) *BatchHandler {
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = defaultFlushInterval
	}
	h := &BatchHandler{
		inner:         inner,
		flusher:       flusher,
		ch:            make(chan batchItem, opts.QueueSize),
		batchSize:     opts.BatchSize,
		flushInterval: opts.FlushInterval,
		quit:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	go h.run()
	return h
}

// Enabled 委托内层。
func (h *BatchHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle 入队（非阻塞）：队列满或已收口 ⇒ 显式丢弃（ErrLogDropped + 计数）。
func (h *BatchHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.enqueue(h.inner, r)
}

// enqueue 是入队共用路径（派生 handler 带各自的内层入队）。
func (h *BatchHandler) enqueue(target slog.Handler, r slog.Record) error {
	it := batchItem{h: target, rec: r.Clone()}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		h.dropped.Add(1)
		return ErrLogDropped
	}
	select {
	case h.ch <- it:
		return nil
	default:
		h.dropped.Add(1)
		return ErrLogDropped
	}
}

// WithAttrs 返回共享同一队列/worker 的派生 handler（⛔ 不另起 goroutine）。
func (h *BatchHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &batchDerived{root: h, inner: h.inner.WithAttrs(attrs)}
}

// WithGroup 返回共享同一队列/worker 的派生 handler。
func (h *BatchHandler) WithGroup(name string) slog.Handler {
	return &batchDerived{root: h, inner: h.inner.WithGroup(name)}
}

// batchDerived 是 WithAttrs/WithGroup 派生面：格式在派生内层，入队回主队列。
type batchDerived struct {
	root  *BatchHandler
	inner slog.Handler
}

func (d *batchDerived) Enabled(ctx context.Context, level slog.Level) bool {
	return d.inner.Enabled(ctx, level)
}

func (d *batchDerived) Handle(ctx context.Context, r slog.Record) error {
	return d.root.enqueue(d.inner, r)
}

func (d *batchDerived) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &batchDerived{root: d.root, inner: d.inner.WithAttrs(attrs)}
}

func (d *batchDerived) WithGroup(name string) slog.Handler {
	return &batchDerived{root: d.root, inner: d.inner.WithGroup(name)}
}

// Dropped 返回溢出显式丢弃计数（⛔ 不静默丢的可观测面）。
func (h *BatchHandler) Dropped() uint64 { return h.dropped.Load() }

// QueueLen 返回当前队列深度（背压/观测用）。
func (h *BatchHandler) QueueLen() int { return len(h.ch) }

// Shutdown 收口：关入口 ⇒ 排空队列 ⇒ flush ⇒（有丢弃时）补汇总记录。幂等；返回首个 flush 错误。
func (h *BatchHandler) Shutdown() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		h.mu.Unlock()
		close(h.quit)
		<-h.done
		// 溢出汇总（⛔ 不静默丢）：丢弃计数进日志本体；worker 已退出，直写内层安全。
		if n := h.dropped.Load(); n > 0 {
			rec := slog.NewRecord(time.Now(), slog.LevelWarn, "telemetry 日志队列溢出丢弃汇总", 0)
			rec.AddAttrs(slog.Int64("dropped", int64(n)))
			_ = h.inner.Handle(context.Background(), rec)
		}
		h.flush()
	})
	return h.flushErr
}

// run 是后台 worker：单轮批量 dispatch（每批收口 flush）+ 空闲 tick 兜底 flush + 收口排空。
func (h *BatchHandler) run() {
	defer close(h.done)
	t := time.NewTicker(h.flushInterval)
	defer t.Stop()
	for {
		select {
		case it := <-h.ch:
			h.dispatch(it)
		drain:
			for n := 1; n < h.batchSize; n++ {
				select {
				case it := <-h.ch:
					h.dispatch(it)
				default:
					break drain
				}
			}
			h.flush()
		case <-t.C:
			h.flush()
		case <-h.quit:
			for {
				select {
				case it := <-h.ch:
					h.dispatch(it)
				default:
					h.flush()
					return
				}
			}
		}
	}
}

// dispatch 回放一条到内层（ctx 用 Background，见文件头注释；Handle 错误与 slog.Logger
// 同口径忽略，写盘错误经 flusher.Flush 透出到 Shutdown 返回值）。
func (h *BatchHandler) dispatch(it batchItem) {
	_ = it.h.Handle(context.Background(), it.rec)
}

// flush 收口缓冲（worker 每批后 / 空闲 tick / Shutdown 各一次）；首个错误留存到 flushErr。
func (h *BatchHandler) flush() {
	if h.flusher == nil {
		return
	}
	if err := h.flusher.Flush(); err != nil && h.flushErr == nil {
		h.flushErr = err
	}
}

// ---- BatchWriter：批量缓冲写手（把 N 次行写合并成一次 write 系统调用）----

// DefaultBatchBufSize 是默认缓冲上限（达到即提前收口 flush；span 日志行长约 300B ⇒ 一批约百行）。
const DefaultBatchBufSize = 32 << 10

// BatchWriter 是 io.Writer 缓冲层：Write 只进缓冲，Flush（或缓冲达上限）一次写下游。
// 线程安全；缓冲区写后复用（⛔ 不每批重新分配）。
type BatchWriter struct {
	mu     sync.Mutex
	w      io.Writer
	buf    []byte
	maxBuf int
	err    error // 首个下游写错误（Flush 返回；⛔ 不吞）
}

// NewBatchWriter 构造缓冲写手；maxBuf 取 0 或负数 ⇒ DefaultBatchBufSize。
func NewBatchWriter(w io.Writer, maxBuf int) *BatchWriter {
	if maxBuf <= 0 {
		maxBuf = DefaultBatchBufSize
	}
	return &BatchWriter{w: w, maxBuf: maxBuf}
}

// Write 进缓冲（io.Writer 语义：记账 len(p) 即返回）；达上限提前 flush。
// 下游写错误不在 Write 里返回而在 Flush 透出——批量语境下「本条写没写成」只有收口才知道，
// 写回路径也不再有逐条错误面（与日志批量化取舍一致）。
func (b *BatchWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) >= b.maxBuf {
		b.flushLocked()
	}
	return len(p), nil
}

// Flush 一次 write 收口全部缓冲；返回首个下游错误。
func (b *BatchWriter) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushLocked()
	return b.err
}

// flushLocked 是无锁收口（调用方持 b.mu）。
func (b *BatchWriter) flushLocked() {
	if len(b.buf) == 0 {
		return
	}
	if _, err := b.w.Write(b.buf); err != nil && b.err == nil {
		b.err = err
	}
	b.buf = b.buf[:0]
}
