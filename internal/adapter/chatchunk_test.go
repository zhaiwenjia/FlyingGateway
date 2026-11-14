// 日期: 2026-11-14 19:34:20.742
// 作者: Zhai Wenjia

package adapter

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// ---- chunk 解码热路径基准（PERF-PROFILE 建议 3 前后对照）----
//
// 被测对象：五家 chat chunk 帧解码器的 decodeOne 热路径（qwen 兼容层 / moonshot / zhipu /
// deepseek / xiaomi）。负载 = 各家真实样本 + 构造 chunk（文本 / 推理 / 工具帧 / finish /
// 顶层 usage / choices[0].usage 六类帧型）。
// 复现：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run 'x' \
//   -bench 'ChatChunkDecode' -benchmem -count=3

// benchConstructedChunks 是构造 chunk 集（覆盖热路径六类帧型，含 null 填充的真实 wire 形态）。
var benchConstructedChunks = []string{
	`{"id":"b","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`,
	`{"id":"b","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"你好，"},"finish_reason":null}],"usage":null}`,
	`{"id":"b","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":null,"reasoning_content":"想一想"},"finish_reason":null}],"usage":null}`,
	`{"id":"b","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]},"finish_reason":null}],"usage":null}`,
	`{"id":"b","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":null}`,
	`{"id":"b","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":17,"completion_tokens":21,"total_tokens":38,"prompt_tokens_details":{"cached_tokens":10}}}`,
	// Moonshot quirk：usage 放 choices[0].usage（kimi_stream_real.sse 实测形态）。
	`{"id":"b","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop","usage":{"prompt_tokens":19,"completion_tokens":21,"total_tokens":40}}],"usage":null}`,
}

// loadRealChunks 读真实样本（.sse / docs .txt 均为「data: {json}」行形，docs 示例带标题行）
// ⇒ chunk 负载列表（⛔ 不改写样本原文）。
func loadRealChunks(tb testing.TB, rel string) []string {
	tb.Helper()
	f, err := os.Open("testdata/" + rel)
	if err != nil {
		tb.Fatalf("打开样本 %s 失败: %v", rel, err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if strings.HasPrefix(line, "{") {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		tb.Fatalf("读样本 %s 失败: %v", rel, err)
	}
	if len(out) == 0 {
		tb.Fatalf("样本 %s 未解析出 chunk 负载", rel)
	}
	return out
}

// benchChunkDecode 逐 payload 循环走 decodeOne（真实样本混构造 chunk，轮转喂入）。
func benchChunkDecode(b *testing.B, decode func(string) ([]stream.Frame, bool, error), payloads []string) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = decode(payloads[i%len(payloads)])
	}
}

// BenchmarkQwenChatChunkDecode：qwen 兼容层（无 chat 流真实捕获 ⇒ docs 示例 + 构造 chunk）。
func BenchmarkQwenChatChunkDecode(b *testing.B) {
	d := NewQwen(http.DefaultClient).(*qwenAdapter).streamDecoder()
	payloads := append(loadRealChunks(b, "real/zhipu_stream_docs_example.txt"), benchConstructedChunks...)
	benchChunkDecode(b, d.decodeOne, payloads)
}

// BenchmarkMoonshotChatChunkDecode：Kimi 真实捕获（含 choices[0].usage quirk）+ docs 示例。
func BenchmarkMoonshotChatChunkDecode(b *testing.B) {
	d := NewMoonshot(http.DefaultClient).(*moonshotAdapter).streamDecoder()
	payloads := append(loadRealChunks(b, "real/kimi_stream_real.sse"), benchConstructedChunks...)
	benchChunkDecode(b, d.decodeOne, payloads)
}

// BenchmarkZhipuChatChunkDecode：智谱 docs 示例（逐块带 usage 形态）。
func BenchmarkZhipuChatChunkDecode(b *testing.B) {
	d := NewZhipu(http.DefaultClient).(*zhipuAdapter).streamDecoder()
	payloads := append(loadRealChunks(b, "real/zhipu_stream_docs_example.txt"), benchConstructedChunks...)
	benchChunkDecode(b, d.decodeOne, payloads)
}

// BenchmarkDeepSeekChatChunkDecode：DeepSeek 真实捕获（逐块 "usage":null 形态）。
func BenchmarkDeepSeekChatChunkDecode(b *testing.B) {
	d := NewDeepSeek(http.DefaultClient).(*deepseekAdapter).streamDecoder()
	payloads := append(loadRealChunks(b, "real/deepseek_stream_real.sse"), benchConstructedChunks...)
	benchChunkDecode(b, d.decodeOne, payloads)
}

// BenchmarkXiaomiChatChunkDecode：MiMo 真实捕获（reasoning_content 承载形态）。
func BenchmarkXiaomiChatChunkDecode(b *testing.B) {
	d := NewXiaomi(http.DefaultClient).(*xiaomiAdapter).streamDecoder()
	payloads := append(loadRealChunks(b, "real/mimo_stream_real.sse"), benchConstructedChunks...)
	benchChunkDecode(b, d.decodeOne, payloads)
}

// ---- 按需解码（chatchunk.go）钉子测试：帧语义零变化红线 ----

// TestChatChunkOddTypeCoercionParity 钉住异型字段的宽容转换面与旧 apijson 口径逐字一致
// （对照 20261114-opt-batchlog-chunkdecode/raw/probe_old_tolerance.log，改前后各跑一次 diff）：
// content:123 ⇒ 文本帧 "123"（数字→字符串）、index:"0" ⇒ 工具帧不断链（字符串→整数）、
// finish_reason:7 ⇒ 延后收口暂存超集原值 "7"、reasoning_content:123 ⇒ 无推理帧（stdlib
// 容错置空，与旧 dq 解析口径一致）。
// 错误实现会红：热路径严格解码失败直接丢帧、不走 typed 兜底 ⇒ 四例全红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestChatChunkOddType -v
func TestChatChunkOddTypeCoercionParity(t *testing.T) {
	a := NewQwen(http.DefaultClient).(*qwenAdapter)
	d := a.streamDecoder()

	fs, done, err := d.decodeOne(`{"choices":[{"delta":{"content":123}}]}`)
	if err != nil || done || len(fs) != 1 || fs[0].Kind != stream.FrameTextDelta || fs[0].Text != "123" {
		t.Fatalf("数字 content 宽松转换 = frames=%+v done=%v err=%v，期望单文本帧 Text=123", fs, done, err)
	}

	fs, done, err = d.decodeOne(`{"choices":[{"delta":{"tool_calls":[{"index":"0","id":"c","function":{"name":"n","arguments":"{}"}}]}}]}`)
	if err != nil || done || len(fs) != 1 || fs[0].Kind != stream.FrameToolCallArgsDelta ||
		fs[0].ToolCallID != "c" || fs[0].Text != "n" || fs[0].Arguments != "{}" {
		t.Fatalf("字符串 index 宽松转换 = frames=%+v done=%v err=%v，期望工具帧不断链（id=c 名=n）", fs, done, err)
	}

	fs, done, err = d.decodeOne(`{"choices":[{"delta":{"content":"a"},"finish_reason":7}],"usage":null}`)
	if err != nil || done || len(fs) != 1 || fs[0].Text != "a" {
		t.Fatalf("数字 finish_reason = frames=%+v done=%v err=%v，期望仅文本帧（延后收口暂存 7）", fs, done, err)
	}
	fs, done, err = d.decodeOne(`[DONE]`)
	if !done || len(fs) != 1 || fs[0].Kind != stream.FrameDone || fs[0].Status != "7" {
		t.Fatalf("[DONE] 补收口 = frames=%+v done=%v，期望 Done(7)（finish_reason 超集原值）", fs, done)
	}

	d2 := a.streamDecoder()
	fs, done, err = d2.decodeOne(`{"choices":[{"delta":{"reasoning_content":123}}]}`)
	if err != nil || done || len(fs) != 0 {
		t.Fatalf("异型 reasoning_content = frames=%+v done=%v err=%v，期望无帧（stdlib 容错置空，旧 dq 口径一致）", fs, done, err)
	}
}

// TestChatChunkMalformedFramesTolerated 钉住畸形帧容忍（⛔ 不中断流）：两段解码皆失败或
// 空结果 ⇒ 无帧无错（3.7 流式容错，旧 apijson 逐字一致——探针 1/2/3/4/5/6）。
// 错误实现会红：任一畸形帧返回 error 或产出帧即红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestChatChunkMalformed -v
func TestChatChunkMalformedFramesTolerated(t *testing.T) {
	a := NewQwen(http.DefaultClient).(*qwenAdapter)
	for _, payload := range []string{
		`{"choices":[{"delta":{"content":"x"}}]`, // 截断 JSON（探针 1：旧口径同样无帧）
		`{"choices":"nope"}`,                     // choices 异型（探针 2）
		`42`, `"str"`, `null`, `{}`,              // 非对象 / 空对象（探针 3–6）
	} {
		d := a.streamDecoder()
		fs, done, err := d.decodeOne(payload)
		if err != nil || done || len(fs) != 0 {
			t.Fatalf("畸形帧 %s = frames=%+v done=%v err=%v，期望无帧无错容忍跳过", payload, fs, done, err)
		}
	}
}

// TestChatChunkUsageRawPassthrough 钉住 usage 原文逐字透传（⛔ 不经类型重整）与判空口径：
// 顶层对象原文（含扩展字段）原样进 usageFn；异型 usage:"x" 同样原样（探针 7 旧口径一致）；
// usage:null 与缺键 ⇒ 不发 usage 帧（与旧 c.JSON.Usage.Raw() 双判一致）。
// 错误实现会红：经 typed 重整后再传 ⇒ 原文比对红；null/缺键误发帧 ⇒ 条数红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestChatChunkUsageRaw -v
func TestChatChunkUsageRawPassthrough(t *testing.T) {
	var got []string
	d := &qwenChatStreamDecoder{
		usageFn: func(raw json.RawMessage) *core.Usage { got = append(got, string(raw)); return &core.Usage{} },
		toolIDs: map[int64]string{},
	}
	d.decodeOne(`{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":2},"x_ext":"v"}}`)
	d.decodeOne(`{"choices":[],"usage":"x"}`)
	d.decodeOne(`{"choices":[],"usage":null}`)
	d.decodeOne(`{"choices":[]}`)
	want := []string{
		`{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":2},"x_ext":"v"}`,
		`"x"`,
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("usage 原文透传 = %q，期望 %q（null/缺键不发帧）", got, want)
	}
}

// TestChatChunkDupKeyLastWins 是残差钉子（⛔ 不是推荐行为）：同一对象重复键（RFC 8259
// 行为未定义）旧 apijson 口径 first-wins、轻量段 last-wins（探针 10：旧 Text=dup / 新
// Text=second）。真实 wire 无重复键，判定可接受并留痕；此处钉住现口径，防未来改动不知情。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestChatChunkDupKey -v
func TestChatChunkDupKeyLastWins(t *testing.T) {
	a := NewQwen(http.DefaultClient).(*qwenAdapter)
	d := a.streamDecoder()
	fs, _, err := d.decodeOne(`{"choices":[{"delta":{"content":"dup"},"finish_reason":null}],"choices":[{"delta":{"content":"second"}}]}`)
	if err != nil || len(fs) != 1 || fs[0].Text != "second" {
		t.Fatalf("重复键口径 = frames=%+v err=%v，期望 last-wins Text=second（残差见 chatchunk.go 文件头注释）", fs, err)
	}
}
