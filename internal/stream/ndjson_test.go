// 日期: 2026-11-14 14:22:50.789
// 作者: Zhai Wenjia

package stream

import (
	"bytes"
	"strings"
	"testing"
)

// collectLines 扫完整个 NDJSON 流，收集逐行原始 JSON 字节。
func collectLines(t *testing.T, r *strings.Reader) [][]byte {
	t.Helper()
	n := NewNDJSONScanner(r)
	var lines [][]byte
	for n.Scan() {
		// Line 返回底层缓冲 ⇒ 复制后再存（下一轮 Scan 会复用缓冲）。
		lines = append(lines, append([]byte(nil), n.Line()...))
	}
	if err := n.Err(); err != nil {
		t.Fatalf("扫描不应报错: %v", err)
	}
	return lines
}

// TestNDJSONScanner_SkipsBlankLines 钉住空行跳过（统一 SDD 3.6 坑 2 口径）。
func TestNDJSONScanner_SkipsBlankLines(t *testing.T) {
	in := "\n{\"a\":1}\n\n\n{\"b\":2}\n\n"
	lines := collectLines(t, strings.NewReader(in))
	if len(lines) != 2 {
		t.Fatalf("行数应为 2，实得 %d：%q", len(lines), lines)
	}
	if string(lines[0]) != `{"a":1}` || string(lines[1]) != `{"b":2}` {
		t.Errorf("行内容错: %q", lines)
	}
}

// TestNDJSONScanner_RawLinePreserved 钉住含嵌套 { } 的 JSON 行原样上抛、⛔ 不做归一化
// （调用方自行解析）。
func TestNDJSONScanner_RawLinePreserved(t *testing.T) {
	raw := `{"msg":"{ not json }","nested":{"x":[1,2,{}]},"s":"a b"}`
	lines := collectLines(t, strings.NewReader(raw+"\n"))
	if len(lines) != 1 {
		t.Fatalf("行数应为 1，实得 %d", len(lines))
	}
	if !bytes.Equal(lines[0], []byte(raw)) {
		t.Errorf("行应逐字保留，实得 %q", lines[0])
	}
}

// TestNDJSONScanner_LastLineWithoutNewline 钉住流末无换行的末行照常上抛。
func TestNDJSONScanner_LastLineWithoutNewline(t *testing.T) {
	lines := collectLines(t, strings.NewReader(`{"a":1}`))
	if len(lines) != 1 || string(lines[0]) != `{"a":1}` {
		t.Errorf("末行无换行应照常上抛，实得 %q", lines)
	}
}

// TestNDJSONScanner_ControlCharsDoNotSplitLines 钉住「碎行事故」防线
// （2026-11-13 实测教训）：JSON 字符串值里的 0x0b / 0x0c / U+2028 等字符
// ⛔ 不参与切行，一行 JSON 必须整行上抛（读行一律按 \n 切分）。
func TestNDJSONScanner_ControlCharsDoNotSplitLines(t *testing.T) {
	raw := "{\"s\":\"a\x0bb\x0cc\u2028d\"}\n{\"t\":2}"
	lines := collectLines(t, strings.NewReader(raw))
	if len(lines) != 2 {
		t.Fatalf("行数应为 2（控制字符不切行），实得 %d：%q", len(lines), lines)
	}
	if string(lines[0]) != "{\"s\":\"a\x0bb\x0cc\u2028d\"}" {
		t.Errorf("含控制字符的行应整行保留，实得 %q", lines[0])
	}
	if string(lines[1]) != `{"t":2}` {
		t.Errorf("第二行错: %q", lines[1])
	}
}

// TestNDJSONScanner_EmptyStream 钉住空流不报错、零行。
func TestNDJSONScanner_EmptyStream(t *testing.T) {
	lines := collectLines(t, strings.NewReader(""))
	if len(lines) != 0 {
		t.Errorf("空流应零行，实得 %q", lines)
	}
}

// ---- 微基准（流式转发热路径：NDJSON 扫描）----

// benchNDJSONStream 构造 Ollama 形状 NDJSON 流：n 行 message 增量 + 末行 done:true。
func benchNDJSONStream(n int) string {
	const line = `{"model":"llama3.1","created_at":"2026-11-14T10:00:00.000Z","message":{"role":"assistant","content":"你好，世界"},"done":false}` + "\n"
	var b strings.Builder
	b.Grow(n * (len(line) + 8))
	for i := 0; i < n; i++ {
		b.WriteString(line)
	}
	b.WriteString(`{"model":"llama3.1","created_at":"2026-11-14T10:00:01.000Z","done":true,"done_reason":"stop"}` + "\n")
	return b.String()
}

// benchNDJSONBigLineStream 构造大行流（工具参数 / 长文本单行 JSON，考验扫描器
// 缓冲增长路径）：20 行、每行约 60KB。
func benchNDJSONBigLineStream(n int) string {
	var b strings.Builder
	b.Grow(n * 61000)
	for i := 0; i < n; i++ {
		b.WriteString(`{"model":"llama3.1","message":{"role":"assistant","content":"`)
		for j := 0; j < 1000; j++ {
			b.WriteString("0123456789abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz")
		}
		b.WriteString(`"},"done":false}` + "\n")
	}
	return b.String()
}

// BenchmarkNDJSONScanner 扫描 NDJSON 大流（op = 构造扫描器 + 扫完整条流 + 归还缓冲（生产生命周期等价）。
func BenchmarkNDJSONScanner(b *testing.B) {
	cases := []struct {
		name    string
		payload string
	}{
		{"lines_1000", benchNDJSONStream(1000)},
		{"big_lines_20", benchNDJSONBigLineStream(20)},
	}
	var r benchReader
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.Reset(c.payload)
				s := NewNDJSONScanner(&r)
				total := 0
				for s.Scan() {
					total += len(s.Line())
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

// TestNDJSONScanner_ReleaseIdempotent 钉住 Release 幂等（⛔ 不双 Put）+
// Release 后再 Scan 返回 false（EOF 惰性口径，Line 的失效约定见其 docstring）。
func TestNDJSONScanner_ReleaseIdempotent(t *testing.T) {
	n := NewNDJSONScanner(strings.NewReader("{\"a\":1}\n"))
	for n.Scan() {
	}
	n.Release()
	n.Release() // 幂等：第二次是 no-op
	if n.Scan() {
		t.Error("Release 后 Scan 应返回 false")
	}
	if err := n.Err(); err != nil {
		t.Errorf("EOF 后 Err 应为 nil，实得 %v", err)
	}
}
