// 日期: 2026-11-14 19:37:05.316
// 作者: Zhai Wenjia

package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 遥测日志落盘热路径基准（PERF-PROFILE 建议 2 前后对照）----
//
// 被测对象：每请求一条 span 日志的落盘路径（画像口径：log/slog Logger.Info cum 16.22%/17.90%、
// os.File.Write cum 15.18%/16.93%）。基线 = TextHandler 直写文件（每条一次 write 系统调用）；
// 批量变体 = BatchHandler + BatchWriter（后台批量 flush，收口 flush 计入计时，零丢弃口径）。
// 复现：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/telemetry/ -run 'x' \
//   -bench 'SpanLog' -benchmem -benchtime=20000x -count=3

// logSpanRecord 与 cmd/gateway 的 logExporter 同形的一条 span 日志（遥测热路径每请求一条）。
func logSpanRecord(l *slog.Logger) {
	l.Info("upstream span",
		"name", OperationChat,
		"status", "UNSET",
		"ended", true,
		"attrs", map[string]any{
			AttrOperationName:    OperationChat,
			AttrProviderName:     "xiaomi",
			AttrRequestModel:     "mimo-v2.6-flash",
			AttrResponseModel:    "mimo-v2.6-flash",
			AttrResponseStatus:   ResponseCompleted,
			AttrFinishReasons:    "stop",
			AttrRequestStream:    true,
			AttrUsageInputTokens: int64(17),
		},
	)
}

// BenchmarkSpanLogSync 基线：slog TextHandler 直写文件，每条一次 write（当前线上路径）。
func BenchmarkSpanLogSync(b *testing.B) {
	f, err := os.Create(filepath.Join(b.TempDir(), "bench.log"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	logger := slog.New(slog.NewTextHandler(f, nil))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logSpanRecord(logger)
	}
	b.StopTimer()
}

// BenchmarkSpanLogBatch 批量异步路径：BatchHandler 有界队列 + BatchWriter 合并写；
// 收口 Shutdown（排空 + flush）计入计时 ⇒ 端到端每条总成本；背压等待保证零丢弃口径。
func BenchmarkSpanLogBatch(b *testing.B) {
	f, err := os.Create(filepath.Join(b.TempDir(), "bench.log"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	bw := NewBatchWriter(f, 0)
	h := NewBatchHandler(slog.NewTextHandler(bw, nil), bw, BatchOptions{QueueSize: 4096})
	logger := slog.New(h)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for h.QueueLen() >= 4095 {
			runtime.Gosched()
		}
		logSpanRecord(logger)
	}
	if err := h.Shutdown(); err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	if d := h.Dropped(); d != 0 {
		b.Fatalf("零丢弃口径破坏: dropped=%d", d)
	}
}

// ---- BatchHandler / BatchWriter 钉子测试 ----

// collectHandler 收集记录（可选首条阻塞，用于确定性构造队列溢出）。
type collectHandler struct {
	mu      sync.Mutex
	records []slog.Record
	block   chan struct{} // 非 nil ⇒ 第 1 条阻塞直至关闭
	entered chan struct{} // 非 nil ⇒ 首条进入 Handle 时关闭（测试同步点）
}

func (h *collectHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *collectHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r)
	n := len(h.records)
	h.mu.Unlock()
	if h.block != nil && n == 1 {
		if h.entered != nil {
			close(h.entered)
		}
		<-h.block
	}
	return nil
}

func (h *collectHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *collectHandler) WithGroup(string) slog.Handler      { return h }

func (h *collectHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

func testRecord(msg string) slog.Record {
	return slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0)
}

// TestBatchHandlerDropExplicit 钩住溢出显式丢弃（⛔ 不静默丢）：队列满 ⇒ Handle 返回
// ErrLogDropped + Dropped() 计数 + Shutdown 收口补汇总记录（dropped 进日志本体）。
// 错误实现会红：静默丢（返回 nil / 无计数 / 无汇总）任一即红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/telemetry/ -run TestBatchHandlerDrop -v
func TestBatchHandlerDropExplicit(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{})
	inner := &collectHandler{block: block, entered: entered}
	h := NewBatchHandler(inner, nil, BatchOptions{QueueSize: 2, BatchSize: 1})

	if err := h.Handle(context.Background(), testRecord("r1")); err != nil {
		t.Fatalf("r1 入队失败: %v", err)
	}
	<-entered // worker 已取走 r1 并阻塞在内层
	if err := h.Handle(context.Background(), testRecord("r2")); err != nil {
		t.Fatalf("r2 入队失败: %v", err)
	}
	if err := h.Handle(context.Background(), testRecord("r3")); err != nil {
		t.Fatalf("r3 入队失败: %v", err)
	}
	for _, msg := range []string{"r4", "r5"} {
		if err := h.Handle(context.Background(), testRecord(msg)); !errors.Is(err, ErrLogDropped) {
			t.Fatalf("%s 溢出应返回 ErrLogDropped，实际 %v", msg, err)
		}
	}
	if d := h.Dropped(); d != 2 {
		t.Fatalf("Dropped() = %d，期望 2（显式计数）", d)
	}
	if n := h.QueueLen(); n != 2 {
		t.Fatalf("QueueLen() = %d，期望 2（r2/r3 在队）", n)
	}
	close(block)
	if err := h.Shutdown(); err != nil {
		t.Fatalf("Shutdown 返回错误: %v", err)
	}

	recs := inner.snapshot()
	var delivered, summary int
	for _, r := range recs {
		switch r.Message {
		case "telemetry 日志队列溢出丢弃汇总":
			summary++
			var dropped int64 = -1
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "dropped" {
					dropped = a.Value.Int64()
				}
				return true
			})
			if dropped != 2 {
				t.Fatalf("汇总记录 dropped = %d，期望 2", dropped)
			}
		case "r1", "r2", "r3":
			delivered++
		default:
			t.Fatalf("意外记录: %q", r.Message)
		}
	}
	if delivered != 3 || summary != 1 {
		t.Fatalf("收口交付 = r1..r3 %d 条 + 汇总 %d 条，期望 3 + 1（收口排空，⛔ 不丢在途）", delivered, summary)
	}
}

// TestBatchHandlerShutdownDrainsAll 钩住收口排空：500 条全量交付、零丢弃、无汇总记录。
// 错误实现会红：Shutdown 提前返回（在途丢失）⇒ 数目红；误发汇总 ⇒ summary 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/telemetry/ -run TestBatchHandlerShutdown -v
func TestBatchHandlerShutdownDrainsAll(t *testing.T) {
	inner := &collectHandler{}
	h := NewBatchHandler(inner, nil, BatchOptions{QueueSize: 1024})
	for i := 0; i < 500; i++ {
		if err := h.Handle(context.Background(), testRecord("m")); err != nil {
			t.Fatalf("第 %d 条入队失败: %v", i, err)
		}
	}
	if err := h.Shutdown(); err != nil {
		t.Fatalf("Shutdown 返回错误: %v", err)
	}
	var summary int
	for _, r := range inner.snapshot() {
		if r.Message == "telemetry 日志队列溢出丢弃汇总" {
			summary++
		}
	}
	if n := len(inner.snapshot()); n != 500 || summary != 0 {
		t.Fatalf("收口交付 %d 条（汇总 %d），期望 500 条（汇总 0）", n, summary)
	}
	if d := h.Dropped(); d != 0 {
		t.Fatalf("Dropped() = %d，期望 0", d)
	}
	// 幂等：二次 Shutdown 不再动作、不报错。
	if err := h.Shutdown(); err != nil {
		t.Fatalf("二次 Shutdown 返回错误: %v", err)
	}
}

// countWriter 记录底层 write 次数与内容。
type countWriter struct {
	mu     sync.Mutex
	writes int
	buf    []byte
}

func (w *countWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *countWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

// TestBatchWriterFlushBatches 钩住合并写：N 次行写 ⇒ Flush 后恰一次底层 write；
// 达上限提前收口（两个口径都要在，否则小流量滞留 / 大流量爆缓冲）。
// 错误实现会红：每条一写（writes=N）或达限不 flush（len(p) 超限仍无写）即红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/telemetry/ -run TestBatchWriter -v
func TestBatchWriterFlushBatches(t *testing.T) {
	cw := &countWriter{}
	bw := NewBatchWriter(cw, 64)
	bw.Write([]byte("a\n"))
	bw.Write([]byte("b\n"))
	if n := cw.count(); n != 0 {
		t.Fatalf("flush 前底层 write = %d，期望 0（先攒后写）", n)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("Flush 错误: %v", err)
	}
	if n := cw.count(); n != 1 || string(cw.buf) != "a\nb\n" {
		t.Fatalf("Flush 后 write 次数 = %d 内容 = %q，期望 1 次 / a\\nb\\n", n, cw.buf)
	}
	bw.Write([]byte(strings.Repeat("x", 64))) // 达上限 ⇒ 提前收口
	if n := cw.count(); n != 2 {
		t.Fatalf("达上限后底层 write = %d，期望 2（提前收口）", n)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("Flush 错误: %v", err)
	}
	if n := cw.count(); n != 2 {
		t.Fatalf("空缓冲 Flush 底层 write = %d，期望 2（空 flush 不产生 0 字节写）", n)
	}
}

// TestBatchHandlerOverIODiscard 钩住 io.Discard 组装口径不变：TextHandler(io.Discard) 全链路
// 无错、零丢弃、收口干净（测试 io.Discard 行为不变红线）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/telemetry/ -run TestBatchHandlerOverIODiscard -v
func TestBatchHandlerOverIODiscard(t *testing.T) {
	bw := NewBatchWriter(io.Discard, 0)
	h := NewBatchHandler(slog.NewTextHandler(bw, nil), bw, BatchOptions{})
	logger := slog.New(h)
	for i := 0; i < 100; i++ {
		logger.Info("upstream span", "i", i)
	}
	if err := h.Shutdown(); err != nil {
		t.Fatalf("Shutdown 返回错误: %v", err)
	}
	if d := h.Dropped(); d != 0 {
		t.Fatalf("Dropped() = %d，期望 0", d)
	}
}

// TestBatchHandlerWithAttrsDerived 钩住派生 handler 共享队列且字段语义不变：
// With 派生后记录经内层 TextHandler 渲染仍带原属性（k=v）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/telemetry/ -run TestBatchHandlerWithAttrs -v
func TestBatchHandlerWithAttrsDerived(t *testing.T) {
	var buf bytes.Buffer
	bw := NewBatchWriter(&buf, 0)
	h := NewBatchHandler(slog.NewTextHandler(bw, nil), bw, BatchOptions{})
	logger := slog.New(h).With("k", "v")
	logger.Info("hello")
	if err := h.Shutdown(); err != nil {
		t.Fatalf("Shutdown 返回错误: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "msg=hello") || !strings.Contains(out, "k=v") {
		t.Fatalf("派生记录渲染 = %q，期望含 msg=hello 与 k=v（字段语义不变）", out)
	}
}
