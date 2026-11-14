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
	buf *[]byte // 池借出的初始扫描缓冲（Release 归还；可不调 —— 不调走 GC）
	err error
}

// NewNDJSONScanner 构造扫描器（初始缓冲从 scanBufPool 借出，与 SSE 共池，
// 容量规格与 maxSSELine 兼容口径见 sse.go）。
func NewNDJSONScanner(r io.Reader) *NDJSONScanner {
	bp := getScanBuf()
	sc := bufio.NewScanner(r)
	sc.Buffer(*bp, maxSSELine)
	return &NDJSONScanner{sc: sc, buf: bp}
}

// Release 归还扫描缓冲（幂等）。可不调（不调则缓冲随 GC 回收）；
// ⛔ 调用后不得再 Scan / Line（Line 的返回切片指向该缓冲，口径同
// bufio.Scanner.Bytes：下次 Scan 后即失效，Release 只是把失效点提前到归还时）。
func (n *NDJSONScanner) Release() {
	if n.buf == nil {
		return
	}
	releaseScanBuf(n.buf)
	n.buf = nil
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
// ⚠️ 有效期内用完即弃（下次 Scan / Release 后失效），⛔ 不得跨 Scan 持有。
func (n *NDJSONScanner) Line() []byte { return n.sc.Bytes() }

// Err 返回扫描错误（EOF 为 nil）。
func (n *NDJSONScanner) Err() error { return n.err }
