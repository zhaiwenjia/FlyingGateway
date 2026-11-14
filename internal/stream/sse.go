// 日期: 2026-11-14 11:33:06.951
// 作者: Zhai Wenjia

package stream

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"
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
	sc    *bufio.Scanner
	buf   *[]byte // 池借出的初始扫描缓冲（Release 归还；可不调 —— 不调走 GC）
	event SSEEvent
	parts []string // data 行值累积（切片跨事件复用；值是行内容的独立 string 拷贝）
	// pending 已收集到字段但尚未遇到空行。
	pending bool
	err     error
}

const (
	// maxSSELine 是单行上限（防御性：上游一行超大 JSON 不至于 OOM）。
	// ⚠️ 与缓冲复用策略的兼容口径：maxSSELine 只约束 bufio.Scanner 的 token 上限，
	// 池只回收「本方借出的初始缓冲」；超大行由 bufio 自倍增（≤16MB，随流结束被
	// GC 回收），⛔ 不进池 —— 两套上限互不干扰。
	maxSSELine = 16 << 20
	// scanBufSize 是池化初始扫描缓冲容量（与旧实现的每请求 make 64KB 同规格，
	// 只改复用、不改读取行为）。
	scanBufSize = 64 << 10
	// maxPooledScanBuf 是归还入池的容量上限（超阈值丢弃，见 releaseScanBuf）。
	maxPooledScanBuf = 1 << 20
)

// scanBufPool 是扫描缓冲池（sync.Pool 用法与坑位按调研 §4.2 口径逐条设防）：
//   - 池是包级变量（基准失真坑：包级 + 每轮 Get/Put，⛔ 不跨轮次预热复用）；
//   - 谁 Get 谁 Put：NewSSEScanner / NewNDJSONScanner 借出，Release 归还，⛔ 不双 Put；
//   - Put 前 Reset（脏数据坑）：归还前长度归零；残留字节不需清零 ——
//     Scanner API 只暴露 [0,len) 区域，越界残留永不可见，而清零 O(64KB) 会吃掉复用收益；
//   - 超阈值丢弃（内存滞留坑）：大块 ⛔ 不跟池转（见 releaseScanBuf）。
var scanBufPool = sync.Pool{New: func() any {
	b := make([]byte, 0, scanBufSize)
	return &b
}}

// getScanBuf 从池借出初始扫描缓冲（长度归零、容量 ≥ scanBufSize）。
func getScanBuf() *[]byte {
	bp := scanBufPool.Get().(*[]byte)
	*bp = (*bp)[:0]
	return bp
}

// releaseScanBuf 归还缓冲：Put 前 Reset（长度归零），cap 超 maxPooledScanBuf 的
// 大块丢弃不入池（返回 false）。当前池缓冲固定 scanBufSize，阈值闸为防御性：
// 将来若把「bufio 倍增后的自留大缓冲」也纳入归还（需持有其所有权），超过 1MB 的
// 块必须就地丢弃，⛔ 不许跟着池一直转（调研 §4.2 坑 2 原话口径）。
// ⚠️ 形参用 *[]byte 而非 []byte：后者 Put(&b) 会让切片头逃逸，每次归还多 1 个堆分配。
func releaseScanBuf(b *[]byte) bool {
	if cap(*b) > maxPooledScanBuf {
		return false
	}
	*b = (*b)[:0]
	scanBufPool.Put(b)
	return true
}

// NewSSEScanner 构造扫描器（初始缓冲从池借出）。
func NewSSEScanner(r io.Reader) *SSEScanner {
	bp := getScanBuf()
	sc := bufio.NewScanner(r)
	sc.Buffer(*bp, maxSSELine)
	return &SSEScanner{sc: sc, buf: bp}
}

// Release 归还扫描缓冲（幂等）。可不调（不调则缓冲随 GC 回收）；
// ⛔ 调用后不得再 Scan / Event（缓冲可能已被下一个流复用 ——
// 与 bufio.Scanner.Bytes 的「下次 Scan 后失效」同一失效口径）。
func (s *SSEScanner) Release() {
	if s.buf == nil {
		return
	}
	releaseScanBuf(s.buf)
	s.buf = nil
}

// Scan 推进到下一个完整事件；流结束或出错返回 false。
func (s *SSEScanner) Scan() bool {
	s.event = SSEEvent{}
	s.parts = s.parts[:0]
	s.pending = false
	for s.sc.Scan() {
		line := s.sc.Bytes()
		if len(line) == 0 {
			// 空行 = 事件分隔；无任何字段的空事件跳过。
			if s.pending || len(s.parts) > 0 {
				s.emit()
				return true
			}
			continue
		}
		if line[0] == ':' {
			continue // 注释行（含 `: [DONE]` 之类哨兵注释）
		}
		field, value := splitSSEField(line)
		// string(field) 与字面量比较不产生分配（编译器按 memequal 优化）。
		switch {
		case string(field) == "event":
			s.event.Event = string(value) // 行缓冲会被下次 Scan 覆盖 ⇒ 此处必须拷贝
			s.pending = true
		case string(field) == "data":
			s.parts = append(s.parts, string(value))
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
	if s.pending || len(s.parts) > 0 {
		s.pending = false
		s.emit()
		return true
	}
	return false
}

// emit 落定当前事件的 Data（单行直接取值、多行按规范以 \n 拼接）。
func (s *SSEScanner) emit() {
	switch len(s.parts) {
	case 0:
		// 只有 event: 字段（或仅未知字段行）的事件，Data 为空串。
	case 1:
		s.event.Data = s.parts[0]
	default:
		s.event.Data = strings.Join(s.parts, "\n")
	}
}

// Event 返回当前事件。
func (s *SSEScanner) Event() SSEEvent { return s.event }

// Err 返回扫描错误（EOF 为 nil）。
func (s *SSEScanner) Err() error { return s.err }

// splitSSEField 拆分 `field: value`，容忍冒号后有无一个空格（统一 SDD 3.6 坑 1）。
// ⛔ 不做 string 拷贝（调用方只在需要落值时按字节切片拷贝成 string）。
func splitSSEField(line []byte) (field, value []byte) {
	idx := bytes.IndexByte(line, ':')
	if idx < 0 {
		return line, nil // 无冒号的整行按字段名处理（SSE 规范：值为空）
	}
	field = line[:idx]
	value = line[idx+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:] // 只剥一个空格（SSE 规范口径）
	}
	return field, value
}
