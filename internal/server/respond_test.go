// 日期: 2026-11-14 16:08:29.843
// 作者: Zhai Wenjia

package server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// TestFrameAccumulatorThinkingSignature 钉住帧累加器把 thinking 签名落进
// core.OutputItem.ThinkingSignature（流式非流式同一出口）：签名帧
// （FrameReasoningDelta 且 ThinkingSignature 非空，承载约定见 stream.FrameReasoningDelta）
// 收口一个 thinking 块 ⇒ 两个签名块产出两个 reasoning item、各带各的签名与正文。
// 错误实现会红：签名被丢（帧抽象无槽位时的旧行为）⇒ ThinkingSignature 非空断言红；
// 两块并成一块（只留末签名 / 丢首签名）⇒ 逐块签名断言红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/server/ -run TestFrameAccumulatorThinkingSignature -v
func TestFrameAccumulatorThinkingSignature(t *testing.T) {
	acc := newFrameAccumulator()
	acc.add(stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: "先想"})
	acc.add(stream.Frame{Kind: stream.FrameReasoningDelta, ThinkingSignature: "sig-1"})
	acc.add(stream.Frame{Kind: stream.FrameReasoningDelta, Reasoning: "再想"})
	acc.add(stream.Frame{Kind: stream.FrameReasoningDelta, ThinkingSignature: "sig-2"})
	acc.add(stream.Frame{Kind: stream.FrameTextDelta, Text: "答"})

	resp := acc.result("m")
	var reasoning []core.OutputItem
	for _, it := range resp.OutputItems {
		if it.Type == "reasoning" {
			reasoning = append(reasoning, it)
		}
	}
	if len(reasoning) != 2 {
		t.Fatalf("reasoning item 应有 2 个（每 thinking 块一个，流式非流式同形），实得 %d：%+v",
			len(reasoning), resp.OutputItems)
	}
	if reasoning[0].ReasoningText != "先想" || reasoning[0].ThinkingSignature != "sig-1" {
		t.Errorf("thinking 块1 错（正文+签名逐块落）: %+v", reasoning[0])
	}
	if reasoning[1].ReasoningText != "再想" || reasoning[1].ThinkingSignature != "sig-2" {
		t.Errorf("thinking 块2 错（正文+签名逐块落）: %+v", reasoning[1])
	}
}

// ---- flush 策略实证（W-optA 流式热路径优化；调研 L3/L4）----

// BenchmarkFlushCall 隔离 flush() 调用本身的固定开销：httptest.ResponseRecorder
// 的 Flush 是 no-op ⇒ 只剩 ResponseController 构造等分配成本，不含系统调用。
func BenchmarkFlushCall(b *testing.B) {
	w := httptest.NewRecorder()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		flush(w)
	}
}

// flushPolicy 是流式写出的 flush 策略参数（写侧策略，均为 [write, flush] 泵的变体）。
type flushPolicy struct {
	name       string
	every      int  // 每 N 次 write 后 flush（1=每帧；0=仅结尾；N>1=聚合）
	flushFirst bool // 首帧写出立即 flush（TTFB 优先）
}

func (p flushPolicy) do(w http.ResponseWriter, i int) {
	switch {
	case p.every == 1:
		flush(w)
	case p.every == 0:
		// 仅结尾：循环外统一 flush。
	case (p.flushFirst && i == 0) || (i+1)%p.every == 0:
		flush(w)
	}
}

var flushPolicies = []flushPolicy{
	{name: "flush每帧_现状", every: 1, flushFirst: true},
	{name: "flush首帧加每8帧", every: 8, flushFirst: true},
	{name: "flush首帧加每32帧", every: 32, flushFirst: true},
	{name: "flush仅结尾", every: 0, flushFirst: false},
}

// BenchmarkFlushPolicy 实证 flush 策略（op = 一条完整流式响应经 loopback TCP 写完
// 并被客户端读完；同一策略先测 fast 再测 slow）：
//   - fast：200 帧 × 300B 连发（系统调用压力最大的形态）⇒ ns/op 总耗时 + ttfb-µs/op；
//   - slow：50 帧、每帧间隔 2ms（贴近真实 LLM 帧率）⇒ lag-µs/op 逐帧交付延迟
//     （帧内嵌写入时刻戳，客户端收到即算差）——聚合 flush 的代价正是逐帧交付延迟，
//     只看 TTFB 会漏掉它。
//
// 结论口径：流式 TTFB 优先（任务书），聚合策略必须用 lag 数据证明无害才可采用。
func BenchmarkFlushPolicy(b *testing.B) {
	for _, p := range flushPolicies {
		p := p
		b.Run(p.name+"/fast", func(b *testing.B) {
			const frames, frameSize = 200, 300
			payload := bytes.Repeat([]byte("d"), frameSize)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				for i := 0; i < frames; i++ {
					_, _ = w.Write(payload)
					p.do(w, i)
				}
				flush(w) // 收口必刷（所有策略一致）
			}))
			defer srv.Close()
			client := srv.Client()
			var ttfbSum int64
			buf := make([]byte, 32<<10)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				t0 := time.Now()
				resp, err := client.Get(srv.URL)
				if err != nil {
					b.Fatal(err)
				}
				n, err := resp.Body.Read(buf)
				if err != nil || n == 0 {
					b.Fatalf("首读失败: n=%d err=%v", n, err)
				}
				ttfbSum += time.Since(t0).Microseconds()
				if _, err := io.Copy(io.Discard, resp.Body); err != nil {
					b.Fatal(err)
				}
				resp.Body.Close()
			}
			b.StopTimer()
			b.ReportMetric(float64(ttfbSum)/float64(b.N), "ttfb-µs/op")
		})
		b.Run(p.name+"/slow", func(b *testing.B) {
			const frames, gap = 50, 2 * time.Millisecond
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				for i := 0; i < frames; i++ {
					// 帧内嵌写入时刻戳 + padding（客户端据此算逐帧交付延迟）。
					_, _ = fmt.Fprintf(w, "%d %s\n", time.Now().UnixNano(), strings.Repeat("d", 180))
					p.do(w, i)
					time.Sleep(gap)
				}
				flush(w)
			}))
			defer srv.Close()
			client := srv.Client()
			var lagSum int64
			var lagN int64
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				resp, err := client.Get(srv.URL)
				if err != nil {
					b.Fatal(err)
				}
				sc := bufio.NewScanner(resp.Body)
				for sc.Scan() {
					var ts int64
					if _, err := fmt.Sscanf(sc.Text(), "%d", &ts); err != nil {
						continue
					}
					lagSum += time.Since(time.Unix(0, ts)).Microseconds()
					lagN++
				}
				resp.Body.Close()
			}
			b.StopTimer()
			if lagN == 0 {
				b.Fatal("未收到任何帧（基准失真）")
			}
			b.ReportMetric(float64(lagSum)/float64(lagN), "lag-µs/帧")
		})
	}
}
