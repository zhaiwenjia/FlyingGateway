// 日期: 2026-11-14 11:33:06.951
// 作者: Zhai Wenjia

package stream

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// SSEEvent 是一个 SSE 事件（`event:` + `data:` 双字段）。
type SSEEvent struct {
	// Event 是 `event:` 行的值；只有 `data:` 的帧（OpenAI Chat 形状）为空串。
	Event string
	// Data 是 `data:` 行的拼接值（多行 data 以 \n 连接，SSE 规范口径）。
	Data string
}

// SSEScanner 是容错的 SSE 逐事件扫描器（统一 SDD 3.6 坑 1 / 4.3.1 解析器容忍性）：
//   - 分隔符容忍 `data: {...}`（冒号后一个空格）与 `data:{...}`（冒号后无空格，Qwen 原生）；
//   - 注释行（`: ping` 等）跳过；
//   - 未知字段行（`id:` / `retry:`）跳过；
//   - 多行 `data:` 按 SSE 规范以 \n 拼接；
//   - 不假设事件顺序、不假设 `[DONE]` 哨兵存在（vLLM / llama.cpp 可省略）。
type SSEScanner struct {
	sc      *bufio.Scanner
	event   SSEEvent
	pending bool // 已收集到字段但尚未遇到空行
	err     error
}

// maxSSELine 是单行上限（防御性：上游一行超大 JSON 不至于 OOM）。
const maxSSELine = 16 << 20

// NewSSEScanner 构造扫描器。
func NewSSEScanner(r io.Reader) *SSEScanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	return &SSEScanner{sc: sc}
}

// Scan 推进到下一个完整事件；流结束或出错返回 false。
func (s *SSEScanner) Scan() bool {
	s.event = SSEEvent{}
	s.pending = false
	dataLines := 0
	for s.sc.Scan() {
		line := s.sc.Text()
		if line == "" {
			// 空行 = 事件分隔；无任何字段的空事件跳过。
			if s.pending || dataLines > 0 {
				return true
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // 注释行（含 `: [DONE]` 之类哨兵注释）
		}
		field, value := splitSSEField(line)
		switch field {
		case "event":
			s.event.Event = value
			s.pending = true
		case "data":
			if dataLines > 0 {
				s.event.Data += "\n"
			}
			s.event.Data += value
			dataLines++
			s.pending = true
		default:
			// id: / retry: 及未知字段行 —— 跳过但容忍（不因此中断）。
			s.pending = true
		}
	}
	if err := s.sc.Err(); err != nil {
		s.err = err
		return false
	}
	// 流结束时若还有未分隔的残留事件，补发一次（后端不发终止空行的容忍）。
	if s.pending || dataLines > 0 {
		s.pending = false
		return true
	}
	return false
}

// Event 返回当前事件。
func (s *SSEScanner) Event() SSEEvent { return s.event }

// Err 返回扫描错误（EOF 为 nil）。
func (s *SSEScanner) Err() error { return s.err }

// splitSSEField 拆分 `field: value`，容忍冒号后有无一个空格（统一 SDD 3.6 坑 1）。
func splitSSEField(line string) (field, value string) {
	idx := bytes.IndexByte([]byte(line), ':')
	if idx < 0 {
		return line, "" // 无冒号的整行按字段名处理（SSE 规范：值为空）
	}
	field = line[:idx]
	value = line[idx+1:]
	value = strings.TrimPrefix(value, " ") // 只剥一个空格（SSE 规范口径）
	return field, value
}
