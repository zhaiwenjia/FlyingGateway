// 日期: 2026-11-14 14:22:50.775
// 作者: Zhai Wenjia

package stream

import (
	"io"
	"strings"
	"testing"
)

// collectEvents 扫完整个流，收集事件序列。
func collectEvents(t *testing.T, r *strings.Reader) []SSEEvent {
	t.Helper()
	s := NewSSEScanner(r)
	var evs []SSEEvent
	for s.Scan() {
		evs = append(evs, s.Event())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("扫描不应报错: %v", err)
	}
	return evs
}

// TestSSEScanner_NoSpaceAfterColon 钉住 Qwen 原生坑（统一 SDD 3.6 坑 1）：
// `data:{...}` 冒号后无空格照样解析，值不多吃空格。
func TestSSEScanner_NoSpaceAfterColon(t *testing.T) {
	evs := collectEvents(t, strings.NewReader("data:{\"a\":1}\n\n"))
	if len(evs) != 1 {
		t.Fatalf("事件数应为 1，实得 %d：%+v", len(evs), evs)
	}
	if evs[0].Data != `{"a":1}` {
		t.Errorf("Data 应为 {\"a\":1}，实得 %q", evs[0].Data)
	}
}

// TestSSEScanner_EventAndDataDualField 钉住 event: + data: 双字段（Responses 形状）。
func TestSSEScanner_EventAndDataDualField(t *testing.T) {
	evs := collectEvents(t, strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
	if len(evs) != 1 {
		t.Fatalf("事件数应为 1，实得 %d", len(evs))
	}
	if evs[0].Event != "response.created" {
		t.Errorf("Event 应为 response.created，实得 %q", evs[0].Event)
	}
	if evs[0].Data != `{"type":"response.created"}` {
		t.Errorf("Data 错: %q", evs[0].Data)
	}
}

// TestSSEScanner_MultiLineDataJoined 钉住多行 data 以 \n 拼接（SSE 规范口径）。
func TestSSEScanner_MultiLineDataJoined(t *testing.T) {
	evs := collectEvents(t, strings.NewReader("data: line1\ndata: line2\ndata: line3\n\n"))
	if len(evs) != 1 {
		t.Fatalf("事件数应为 1，实得 %d", len(evs))
	}
	if evs[0].Data != "line1\nline2\nline3" {
		t.Errorf("多行 data 应以 \\n 拼接，实得 %q", evs[0].Data)
	}
}

// TestSSEScanner_CommentLinesSkipped 钉住注释行（`: ping` 等）跳过、不进事件。
func TestSSEScanner_CommentLinesSkipped(t *testing.T) {
	in := ": ping\ndata: {\"a\":1}\n\n: keep-alive\n\ndata: {\"b\":2}\n\n"
	evs := collectEvents(t, strings.NewReader(in))
	if len(evs) != 2 {
		t.Fatalf("事件数应为 2，实得 %d：%+v", len(evs), evs)
	}
	if evs[0].Data != `{"a":1}` || evs[1].Data != `{"b":2}` {
		t.Errorf("注释行不应混入 Data: %+v", evs)
	}
}

// TestSSEScanner_EOFResidualEventFlushed 钉住流末无空行的残留事件补发
// （后端不发终止空行的容忍）。
func TestSSEScanner_EOFResidualEventFlushed(t *testing.T) {
	evs := collectEvents(t, strings.NewReader("data: {\"a\":1}\n\nevent: done\ndata: tail"))
	if len(evs) != 2 {
		t.Fatalf("事件数应为 2（末尾无空行也要补发），实得 %d：%+v", len(evs), evs)
	}
	if evs[1].Event != "done" || evs[1].Data != "tail" {
		t.Errorf("残留事件补发错: %+v", evs[1])
	}
}

// TestSSEScanner_DoneSentinelPassedThrough 钉住 `data: [DONE]` 当普通数据上抛，
// 由调用方判哨兵（⛔ 扫描器不吞）。
func TestSSEScanner_DoneSentinelPassedThrough(t *testing.T) {
	evs := collectEvents(t, strings.NewReader("data: [DONE]\n\n"))
	if len(evs) != 1 {
		t.Fatalf("事件数应为 1，实得 %d", len(evs))
	}
	if evs[0].Data != "[DONE]" {
		t.Errorf("[DONE] 应原样上抛，实得 %q", evs[0].Data)
	}
}

// TestSSEScanner_UnknownFieldLinesTolerated 钉住 id: / retry: 等未知字段行跳过但容忍
// （不因此中断）。
func TestSSEScanner_UnknownFieldLinesTolerated(t *testing.T) {
	evs := collectEvents(t, strings.NewReader("id: 3\nretry: 100\ndata: x\n\n"))
	if len(evs) != 1 {
		t.Fatalf("事件数应为 1，实得 %d：%+v", len(evs), evs)
	}
	if evs[0].Data != "x" {
		t.Errorf("Data 应为 x，实得 %q", evs[0].Data)
	}
}

// TestSSEScanner_EmptyEventsSkipped 钉住连续空行的空事件跳过。
func TestSSEScanner_EmptyEventsSkipped(t *testing.T) {
	evs := collectEvents(t, strings.NewReader("\n\n\ndata: x\n\n"))
	if len(evs) != 1 || evs[0].Data != "x" {
		t.Errorf("空事件应跳过，实得 %+v", evs)
	}
}

// TestSSEScanner_MultipleEvents 钉住多事件顺序推进。
func TestSSEScanner_MultipleEvents(t *testing.T) {
	in := "data: {\"n\":1}\n\ndata: {\"n\":2}\n\ndata: {\"n\":3}\n\n"
	evs := collectEvents(t, strings.NewReader(in))
	if len(evs) != 3 {
		t.Fatalf("事件数应为 3，实得 %d", len(evs))
	}
	for i, ev := range evs {
		want := `{"n":` + string(rune('1'+i)) + `}`
		if ev.Data != want {
			t.Errorf("事件 %d Data 应为 %s，实得 %q", i, want, ev.Data)
		}
	}
}

// TestSSEScanner_CRLFTolerated 钉住 \r\n 行尾容忍（代理 / 网关转发常见）。
func TestSSEScanner_CRLFTolerated(t *testing.T) {
	evs := collectEvents(t, strings.NewReader("event: e\r\ndata: v\r\n\r\n"))
	if len(evs) != 1 {
		t.Fatalf("事件数应为 1，实得 %d：%+v", len(evs), evs)
	}
	if evs[0].Event != "e" || evs[0].Data != "v" {
		t.Errorf("CRLF 剥离错: %+v", evs[0])
	}
}

// ---- 微基准（流式转发热路径：SSE 大流扫描）----

// benchReader 是可复位的字符串读取器（⛔ 不让 reader 自身的分配混进扫描器口径）。
type benchReader struct {
	s   string
	off int
}

func (r *benchReader) Reset(s string) { r.s = s; r.off = 0 }

func (r *benchReader) Read(p []byte) (int, error) {
	if r.off >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.off:])
	r.off += n
	return n, nil
}

// benchSSEChatStream 构造贴近真实上游的 chat SSE 大流：n 个 OpenAI chunk 形状的
// content 增量帧 + 每 20 帧一条 `: ping` 心跳注释 + [DONE] 哨兵。
func benchSSEChatStream(n int) string {
	const chunk = `data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":1763100000,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"你好，世界"},"finish_reason":null}]}` + "\n\n"
	var b strings.Builder
	b.Grow(n * (len(chunk) + 8))
	for i := 0; i < n; i++ {
		if i%20 == 19 {
			b.WriteString(": ping\n\n")
		}
		b.WriteString(chunk)
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// benchSSEResponsesStream 构造 responses 形状大流（event: + data: 双字段）。
func benchSSEResponsesStream(n int) string {
	const ev = "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","sequence_number":0,"output_index":0,"content_index":0,"delta":"你好，世界"}` + "\n\n"
	var b strings.Builder
	b.Grow(n * (len(ev) + 8))
	for i := 0; i < n; i++ {
		b.WriteString(ev)
	}
	b.WriteString("event: response.completed\ndata: {}\n\n")
	return b.String()
}

// benchSSEMultiLineStream 构造多行 data 事件流（SSE 规范多行拼接口径）。
func benchSSEMultiLineStream(n int) string {
	const ev = "data: {\"id\":\"ml\",\"object\":\"chat.completion.chunk\",\n" +
		"data: \"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n"
	var b strings.Builder
	b.Grow(n * (len(ev) + 8))
	for i := 0; i < n; i++ {
		b.WriteString(ev)
	}
	return b.String()
}

// BenchmarkSSEScanner 扫描大流（op = 构造扫描器 + 扫完整条流 + 归还缓冲（生产生命周期等价）。
// chat 形状（单行 data + 心跳注释）是流式转发的主负载；responses（event:+data:
// 双字段）与多行 data 是两种必保形态。
func BenchmarkSSEScanner(b *testing.B) {
	cases := []struct {
		name    string
		payload string
	}{
		{"chat_1000事件", benchSSEChatStream(1000)},
		{"responses_1000事件", benchSSEResponsesStream(1000)},
		{"multiline_500事件", benchSSEMultiLineStream(500)},
	}
	var r benchReader
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.Reset(c.payload)
				s := NewSSEScanner(&r)
				total := 0
				for s.Scan() {
					ev := s.Event()
					total += len(ev.Data) + len(ev.Event)
				}
				if err := s.Err(); err != nil {
					b.Fatalf("扫描报错: %v", err)
				}
				s.Release() // 生产生命周期：流收口即归还（httpstream.go close 路径）
				if total == 0 {
					b.Fatal("空扫描（基准失真）")
				}
			}
		})
	}
}

// ---- 缓冲池化与 Release 失效口径钉子（W-optA 流式热路径优化）----

// TestReleaseScanBuf_Threshold 钉住「超阈值丢弃」（调研 §4.2 内存滞留坑）：
// cap 超 maxPooledScanBuf 的大块 ⛔ 不入池（返回 false），常规缓冲入池（返回 true）。
func TestReleaseScanBuf_Threshold(t *testing.T) {
	big := make([]byte, 0, maxPooledScanBuf+1)
	if releaseScanBuf(&big) {
		t.Error("cap 超阈值的缓冲不应入池（大块不跟池转）")
	}
	norm := make([]byte, 0, scanBufSize)
	if !releaseScanBuf(&norm) {
		t.Error("常规扫描缓冲应入池")
	}
}

// TestReleaseScanBuf_ResetBeforePut 钉住「Put 前 Reset」（调研 §4.2 脏数据坑）：
// 归还的缓冲再借出时长度必须为 0，上一流残留内容 ⛔ 不得越过 len 泄漏给下一流。
func TestReleaseScanBuf_ResetBeforePut(t *testing.T) {
	b := make([]byte, 0, scanBufSize)
	b = append(b, "stale-event"...)
	if !releaseScanBuf(&b) {
		t.Fatal("常规扫描缓冲应入池")
	}
	got := getScanBuf()
	if len(*got) != 0 {
		t.Errorf("借出缓冲应为零长（Put 前已 Reset），实得 len=%d", len(*got))
	}
	releaseScanBuf(got)
}

// TestSSEScanner_ReleaseAliasSafety 钉住「已交付事件串与扫描缓冲零别名」：
// Release 后把底层缓冲整体覆写，已收集事件串必须完好 —— 若 Event/Data 引用
// 缓冲内存（如 no-copy String 优化），覆写即污染。⛔ 该优化明令禁止，本测试守门。
func TestSSEScanner_ReleaseAliasSafety(t *testing.T) {
	s := NewSSEScanner(strings.NewReader("event: e\ndata: {\"a\":1}\n\ndata: {\"b\":2}\n\n"))
	var evs []SSEEvent
	for s.Scan() {
		evs = append(evs, s.Event())
	}
	buf := *s.buf // 底层扫描缓冲（测试输入无超长行 ⇒ 未发生 bufio 自增长）
	s.Release()
	full := buf[:cap(buf)]
	for i := range full {
		full[i] = 'X'
	}
	if evs[0].Event != "e" || evs[0].Data != `{"a":1}` || evs[1].Data != `{"b":2}` {
		t.Errorf("Release 后事件串被缓冲覆写污染（别名泄漏）: %+v", evs)
	}
}

// TestSSEScanner_ReleaseIdempotent 钉住 Release 幂等（⛔ 不双 Put）+
// Release 后再 Scan 返回 false（bufio 在 EOF/出错后惰性，⛔ 不向已归还缓冲读写）。
func TestSSEScanner_ReleaseIdempotent(t *testing.T) {
	s := NewSSEScanner(strings.NewReader("data: x\n\n"))
	for s.Scan() {
	}
	s.Release()
	s.Release() // 幂等：第二次是 no-op
	if s.Scan() {
		t.Error("Release 后 Scan 应返回 false")
	}
	if err := s.Err(); err != nil {
		t.Errorf("EOF 后 Err 应为 nil，实得 %v", err)
	}
}
