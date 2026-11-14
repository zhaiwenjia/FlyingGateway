// 日期: 2026-11-14 21:18:08.779
// 作者: Zhai Wenjia

// benchload 受控基准闭合式吞吐/延迟压测器（单文件 main，纯标准库）。
//
// 职责：固定并发 c 个 worker 闭合循环打非流式 chat.completions，测吞吐量（req/s）
// 与延迟分位（P50/P90/P99）。与 ultraload 分工：ultraload 是开环在途容量风暴（按住不收口），
// benchload 是闭合式吞吐阶梯（收口即发下一个）——两者的观测量不同，故分开两个工具。
//
// 等价性：被测两侧（FlyingGateway / LiteLLM）用同一 benchload、同一请求体、同一假上游。
// 请求体与 loadtest.py 逐字一致（model/messages/max_tokens/temperature，非流式）。
//
// 源端口：单 IP 临时端口池仅 16384，c 打到上限会被客户端自己的端口墙挡住 ⇒ -sportlo/-sporthi
// 显式轮转绑定宽源端口区间（沿用 ultraload 口径 20000–49151）。
//
// 用法：
//
//	go run ./20261114-realkeys-mimo-flash-bench/scripts/benchload \
//	    -url http://127.0.0.1:8081/v1/chat/completions -token local-bench \
//	    -c 512 -warmup 2s -dur 5s -ramp 2000 -sportlo 20000 -sporthi 49151 \
//	    -o raw/ladder/P3_gateway_c512.json -detail raw/ladder/P3_gateway_c512.jsonl.gz
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// measured 是测量窗内的一条完成记录（逐请求明细落盘用）。
type measured struct {
	I         int     `json:"i"`
	OK        bool    `json:"ok"`
	Status    int     `json:"status"`
	LatencyMS float64 `json:"latency_ms"`
	Err       string  `json:"err,omitempty"`
}

func main() {
	var (
		url     = flag.String("url", "http://127.0.0.1:8081/v1/chat/completions", "被测端点（POST）")
		model   = flag.String("model", "mimo-v2.6-flash", "请求模型名")
		token   = flag.String("token", "", "Bearer token（空 = 不带 Authorization）")
		target  = flag.String("target", "gateway", "摘要里的 target 名（gateway|litellm）")
		c       = flag.Int("c", 8, "闭合式并发 worker 数")
		warmup  = flag.Duration("warmup", 2*time.Second, "爬坡完成后的热身窗（不计量）")
		dur     = flag.Duration("dur", 5*time.Second, "测量窗时长")
		ramp    = flag.Int("ramp", 2000, "worker 启动速率（个/秒，0 = 全开）")
		timeout = flag.Duration("timeout", 30*time.Second, "单请求超时")
		sportlo = flag.Int("sportlo", 0, "源端口下界（0 = 临时端口自动分配）")
		sporthi = flag.Int("sporthi", 0, "源端口上界（与 sportlo 同用，含端点）")
		outPath = flag.String("o", "", "摘要 JSON 输出路径（空 = 只打印 stdout）")
		detail  = flag.String("detail", "", "逐请求明细 gzip JSONL 输出路径（空 = 不落）")
	)
	flag.Parse()

	payload := map[string]any{
		"model": *model,
		"messages": []map[string]any{
			{"role": "user", "content": "用一句话回答：1+1=?"},
		},
		"max_tokens":  16,
		"temperature": 0,
	}
	body, _ := json.Marshal(payload)

	// 传输层：连接池开到并发上限（闭合式 worker 各占一条在途连接，池小会连接抖动）。
	// 源端口：-sportlo/-sporthi 显式轮转绑定宽端口区间（绕开单 IP 临时端口池 16384 墙）。
	var dialCtx func(ctx context.Context, network, addr string) (net.Conn, error)
	if *sportlo > 0 && *sporthi >= *sportlo {
		var portSeq int64
		nports := int64(*sporthi - *sportlo + 1)
		dialCtx = func(ctx context.Context, network, addr string) (net.Conn, error) {
			var lastErr error
			for try := int64(0); try < nports && try < 64; try++ {
				p := *sportlo + int(atomic.AddInt64(&portSeq, 1)%nports)
				d := net.Dialer{Timeout: 10 * time.Second, LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p}}
				conn, err := d.DialContext(ctx, network, addr)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		}
	} else {
		d := &net.Dialer{Timeout: 10 * time.Second}
		dialCtx = d.DialContext
	}
	tr := &http.Transport{
		MaxIdleConns:        *c * 2,
		MaxIdleConnsPerHost: *c * 2,
		DisableCompression:  true,
		DialContext:         dialCtx,
	}
	run(*c, *warmup, *dur, *ramp, *timeout, *url, *token, *target, *model, body, tr, *outPath, *detail)
}

// run 驱动整场压测：爬坡启动 → 热身 → 测量窗 → 收口 → 汇总。
func run(c int, warmup, dur time.Duration, ramp int, timeout time.Duration,
	url, token, targetName, model string, body []byte, tr *http.Transport, outPath, detailPath string) {

	client := &http.Client{Transport: tr, Timeout: timeout}
	phase := atomic.Int32{} // 0=热身 1=测量 2=停止
	var (
		mu   sync.Mutex
		recs []measured
		seq  int64
		wg   sync.WaitGroup
	)
	worker := func() {
		defer wg.Done()
		for phase.Load() < 2 {
			req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			t0 := time.Now()
			resp, err := client.Do(req)
			ms := float64(time.Since(t0).Microseconds()) / 1000
			rec := measured{LatencyMS: ms}
			if err != nil {
				rec.Err = trunc(err.Error())
			} else {
				n, _ := io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				rec.Status = resp.StatusCode
				rec.OK = resp.StatusCode == 200 && n > 0
				if !rec.OK {
					rec.Err = fmt.Sprintf("status=%d bytes=%d", resp.StatusCode, n)
				}
			}
			if phase.Load() == 1 {
				rec.I = int(atomic.AddInt64(&seq, 1))
				mu.Lock()
				recs = append(recs, rec)
				mu.Unlock()
			}
		}
	}

	// 爬坡启动（防 somaxconn=128 浅队列瞬间溢出——超并发模拟实测教训）。
	for i := 0; i < c; i++ {
		wg.Add(1)
		go worker()
		if ramp > 0 {
			time.Sleep(time.Second / time.Duration(ramp))
		}
	}
	time.Sleep(warmup)
	mStart := time.Now()
	phase.Store(1)
	time.Sleep(dur)
	phase.Store(2)
	mWall := time.Since(mStart).Seconds()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout + 5*time.Second):
		fmt.Fprintln(os.Stderr, "warn: 有 worker 未在超时内收口（其请求未计入测量窗）")
	}

	sort.Slice(recs, func(i, j int) bool { return recs[i].LatencyMS < recs[j].LatencyMS })
	okLats := make([]float64, 0, len(recs))
	ok, fail := 0, 0
	for _, r := range recs {
		if r.OK {
			ok++
			okLats = append(okLats, r.LatencyMS)
		} else {
			fail++
		}
	}
	pct := func(p float64) any {
		if len(okLats) == 0 {
			return nil
		}
		idx := int(float64(len(okLats)) * p)
		if idx > len(okLats)-1 {
			idx = len(okLats) - 1
		}
		return round1(okLats[idx])
	}
	sum := 0.0
	for _, v := range okLats {
		sum += v
	}
	mean := any(nil)
	if len(okLats) > 0 {
		mean = round1(sum / float64(len(okLats)))
	}
	summary := map[string]any{
		"ts":             time.Now().Format("2006-01-02 15:04:05"),
		"tool":           "benchload",
		"target":         targetName,
		"model":          model,
		"concurrency":    c,
		"warmup_s":       warmup.Seconds(),
		"dur_s":          dur.Seconds(),
		"ramp_per_s":     ramp,
		"requests":       len(recs),
		"ok":             ok,
		"fail":           fail,
		"wall_s":         round1(mWall),
		"throughput_rps": round1(float64(len(recs)) / mWall),
		"latency_ms": map[string]any{
			"p50": pct(0.50), "p90": pct(0.90), "p99": pct(0.99),
			"mean": mean, "max": pct(1.0),
		},
	}
	blob, _ := json.MarshalIndent(map[string]any{"summary": summary}, "", "  ")
	if outPath != "" {
		_ = os.WriteFile(outPath, append(blob, '\n'), 0o644)
	}
	fmt.Println(string(blob))

	if detailPath != "" {
		f, err := os.Create(detailPath)
		if err == nil {
			defer f.Close()
			gz := gzip.NewWriter(f)
			defer gz.Close()
			enc := json.NewEncoder(gz)
			for _, r := range recs {
				_ = enc.Encode(r)
			}
		}
	}
}

func trunc(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}
