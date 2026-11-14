// 日期: 2026-11-14 14:22:50.775
// 作者: Zhai Wenjia

package stream

import (
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
