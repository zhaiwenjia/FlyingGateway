// 日期: 2026-11-14 19:20:11.402
// 作者: Zhai Wenjia

// fakeupstream 超高并发等价模拟的假上游（单文件 main，纯标准库）。
//
// 职责：扮演 OpenAI Chat Completions 上游，被 FlyingGateway（deepseek 适配器）与 LiteLLM
// （openai-compatible）共同打到「同一假上游」。对每个 /v1/chat/completions（或 /chat/completions）
// 请求按 stream 标记回 chat.completion（JSON）或 chat.completion.chunk（SSE）。
//
// 等价性核心 = 长持在途风暴：
//   - 消息正文含 "ultra-hold" ⇒ 按住 -hold 秒不收口（连接保持在途）；
//   - 消息正文含 "ultra-quick" ⇒ 立即收口（风暴后恢复探针）。
//
// 首字节早发（TTFB 有意义）：流式时先写响应头 + 首个 SSE 块并 flush（TTFB = 网关转发开销），
// 再按住 -hold 秒，最后补齐剩余块 + usage + [DONE]。⇒ 在途时长 = hold，TTFB = 转发延迟。
//
// 端口空间乘算：-listen 支持逗号分隔多 ip:port（如 127.0.0.2:9102,127.0.0.3:9103）。
// 每个 (客户端源IP, 上游ip, 上游port) 四元组各有独立临时端口空间（16384）⇒ 多监听乘算。
//
// 用法：
//
//	go run ./20261114-ultra-concurrency-sim/scripts/fakeupstream -listen 127.0.0.2:9102,127.0.0.3:9103 -hold 30
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// inFlight / served / peakUp 是自证计数（/_stats 暴露，供外部对账「假上游确实按住了 N 个」）。
var (
	inFlight int64
	served   int64
	peakUp   int64
)

func main() {
	var (
		listen = flag.String("listen", "127.0.0.2:9102", "逗号分隔监听地址（多 ip:port 乘算端口空间）")
		hold   = flag.Int("hold", 30, "ultra-hold 请求按住秒数")
	)
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		handleChat(w, r, time.Duration(*hold)*time.Second)
	})
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		handleChat(w, r, time.Duration(*hold)*time.Second)
	})
	mux.HandleFunc("/_stats", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"in_flight": atomic.LoadInt64(&inFlight),
			"served":    atomic.LoadInt64(&served),
			"peak":      atomic.LoadInt64(&peakUp),
		})
	})

	addrs := strings.Split(*listen, ",")
	errCh := make(chan error, len(addrs))
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		ln, err := net.Listen("tcp", a)
		if err != nil {
			errCh <- fmt.Errorf("监听 %s 失败: %w", a, err)
			continue
		}
		srv := &http.Server{Handler: mux}
		go func(a string, ln net.Listener) {
			fmt.Printf("fakeupstream 监听 %s\n", a)
			errCh <- srv.Serve(ln)
		}(a, ln)
	}
	// 任一监听失败即整体退出（不静默降级成少监听，端口乘算被悄悄削弱会污染上限数据）。
	for range addrs {
		if err := <-errCh; err != nil {
			fmt.Printf("fakeupstream 退出: %v\n", err)
			return
		}
	}
}

// chunk 是 chat.completion.chunk 一帧（chat 形，供 deepseek 流解码器读）。
type chunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage any `json:"usage,omitempty"`
}

func mkChunk(model, content string, finish *string, usage any) *chunk {
	c := chunk{ID: "f", Object: "chat.completion.chunk", Model: model, Usage: usage}
	var ch struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}
	ch.Index = 0
	ch.Delta.Content = content
	ch.FinishReason = finish
	c.Choices = []struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}{ch}
	return &c
}

// nonStream 是 chat.completion（非流式一拍）。
var nonStream = map[string]any{
	"id": "f", "object": "chat.completion", "model": "deepseek-chat",
	"choices": []map[string]any{{
		"index":         0,
		"message":       map[string]any{"role": "assistant", "content": "好"},
		"finish_reason": "stop",
	}},
	"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4},
}

// handleChat 处理一个 chat 请求：按 stream 分派、按正文标记决定是否按住。
func handleChat(w http.ResponseWriter, r *http.Request, hold time.Duration) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	raw := string(body)
	isHold := strings.Contains(raw, "ultra-hold")
	// stream 标记：请求体 "stream":true（容忍空白差异）。
	isStream := strings.Contains(raw, "\"stream\"") && strings.Contains(raw, "true")
	d := time.Duration(0)
	if isHold {
		d = hold
	}

	// 在途计数：进入即 +1，收口 -1；峰值自证。
	atomic.AddInt64(&inFlight, 1)
	if cur := atomic.LoadInt64(&inFlight); cur > atomic.LoadInt64(&peakUp) {
		atomic.StoreInt64(&peakUp, cur)
	}
	defer func() {
		atomic.AddInt64(&inFlight, -1)
		atomic.AddInt64(&served, 1)
	}()

	if isStream {
		writeStream(w, d)
		return
	}
	// 非流式：无法拆头/体 ⇒ 先按住再整体回（TTFB 由网关缓冲语义决定）。
	if d > 0 {
		time.Sleep(d)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(nonStream)
}

// writeStream 流式回：先头 + 首块（TTFB），按住 hold，再补余块 + usage + [DONE]。
func writeStream(w http.ResponseWriter, hold time.Duration) {
	fl, ok := w.(http.Flusher)
	if !ok {
		// 无 Flusher 则退化为整体写（不应发生于 net/http）。
		writeStreamAll(w, hold)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// 首块立即发：TTFB = 网关把首字节转给客户端的开销。
	writeSSE(w, mkChunk("deepseek-chat", "好", nil, nil))
	fl.Flush()
	if hold > 0 {
		time.Sleep(hold)
	}
	stop := "stop"
	usage := map[string]any{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4}
	writeSSE(w, mkChunk("deepseek-chat", "", &stop, usage))
	writeSSE(w, nil) // [DONE]
	fl.Flush()
}

// writeStreamAll 无 Flusher 兜底：按住后一次性写完整 SSE。
func writeStreamAll(w http.ResponseWriter, hold time.Duration) {
	if hold > 0 {
		time.Sleep(hold)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	stop := "stop"
	usage := map[string]any{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4}
	var b strings.Builder
	writeSSETo(&b, mkChunk("deepseek-chat", "好", nil, nil))
	writeSSETo(&b, mkChunk("deepseek-chat", "", &stop, usage))
	writeSSETo(&b, nil)
	_, _ = io.WriteString(w, b.String())
}

// writeSSE 写一帧 SSE（c 为 nil 表示 [DONE] 哨兵）。
func writeSSE(w http.ResponseWriter, c *chunk) {
	if c == nil {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	blob, _ := json.Marshal(c)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", blob)
}

// writeSSETo 同 writeSSE，写入 strings.Builder。
func writeSSETo(b *strings.Builder, c *chunk) {
	if c == nil {
		b.WriteString("data: [DONE]\n\n")
		return
	}
	blob, _ := json.Marshal(c)
	fmt.Fprintf(b, "data: %s\n\n", blob)
}
