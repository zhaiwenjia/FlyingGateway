// 日期: 2026-11-14 11:33:06.951
// 作者: Zhai Wenjia

package stream

import (
	"bufio"
	"io"
)

// NDJSONScanner 是 NDJSON 逐行扫描器（统一 SDD 3.6 坑 2：Ollama 原生流是
// `application/x-ndjson`，一行一个 JSON 对象，⛔ 不是 SSE、⛔ 无 [DONE] 哨兵）。
// ⚠️ 读行一律按 \n 切分（⛔ 不用会切 VT/FF/U+2028 的 splitlines 语义）：
// JSON 字符串值里可能含二进制观察（0x0b/0x0c/U+2028…），按更多字符切分会把
// 一行 JSON 碎成多截（行式 JSONL 的碎行事故，2026-11-13 实测教训）。
type NDJSONScanner struct {
	sc  *bufio.Scanner
	err error
}

// NewNDJSONScanner 构造扫描器。
func NewNDJSONScanner(r io.Reader) *NDJSONScanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	return &NDJSONScanner{sc: sc}
}

// Scan 推进到下一行；流结束或出错返回 false。空行跳过。
func (n *NDJSONScanner) Scan() bool {
	for n.sc.Scan() {
		if len(n.sc.Bytes()) > 0 {
			return true
		}
	}
	n.err = n.sc.Err()
	return false
}

// Line 返回当前行的原始 JSON 字节（调用方自行解析；⛔ 不在此处做 JSON 归一化）。
func (n *NDJSONScanner) Line() []byte { return n.sc.Bytes() }

// Err 返回扫描错误（EOF 为 nil）。
func (n *NDJSONScanner) Err() error { return n.err }
