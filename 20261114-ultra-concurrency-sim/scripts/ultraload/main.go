// 日期: 2026-11-14 19:14:23.887
// 作者: Zhai Wenjia

// ultraload 超高并发等价模拟压测器（单文件 main，纯标准库，go run 路径 直跑）。
//
// 等价性核心 = 长持在途风暴：客户端按爬坡速率发起 -c 个请求，假上游把每个请求按住
// hold 秒不返回 ⇒ 同时在途的请求状态 / TCP 连接数逼近 -c，逐档拉高 -c 观察资源曲线。
//
// 端口空间乘算：本机临时端口 49152-65535 每个目标 ip:port 仅 16384 个 ⇒ 源 IP 轮转
// （LocalAddr 绑 127.0.0.1..127.0.0.N），每源 IP 各有独立 16384 端口空间（4 元组不同）。
//
// 爬坡（-ramp）是硬要求：kern.ipc.somaxconn=128，accept 队列浅，瞬间全开会被 SYN 丢弃
// 污染数据；爬坡速率本身是部署参数，写进摘要。
//
// 用法示例：
//
//	go run ./20261114-ultra-concurrency-sim/scripts/ultraload -c 10000 -d 90s \
//	    -url http://127.0.0.1:8080 -token ck-ultra -ramp 4000 -srcips 2 -o raw/gw-10k.json
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// pct 是延迟分位汇总（毫秒）。
type pct struct {
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	P999 float64 `json:"p999"`
	Max  float64 `json:"max"`
}

// sample 是每秒在途曲线采样点。
type sample struct {
	Sec      int `json:"sec"`
	Inflight int `json:"inflight"`
}

// summary 是退出时打印 / 落盘的摘要 JSON。
type summary struct {
	Meta   map[string]any `json:"meta"`
	Result map[string]any `json:"result"`
}

// atomicMax 把 *p 抬到 v（CAS 循环，race-free；峰值追踪用）。
func atomicMax(p *int64, v int64) {
	for {
		cur := atomic.LoadInt64(p)
		if v <= cur {
			return
		}
		if atomic.CompareAndSwapInt64(p, cur, v) {
			return
		}
	}
}

func main() {
	var (
		target  = flag.Int("c", 1000, "目标在途并发")
		dur     = flag.Duration("d", 90*time.Second, "风暴时长上限（到点未完成记 deadline）")
		url     = flag.String("url", "http://127.0.0.1:8080", "被测入口（如 http://127.0.0.1:8080）")
		token   = flag.String("token", "", "Bearer token（空 = 不带 Authorization）")
		ramp    = flag.Int("ramp", 4000, "爬坡速率（新建连接/秒，⛔ 不许瞬间全开）")
		srcips  = flag.Int("srcips", 8, "源 IP 个数（127.0.0.1 起轮转，乘算端口空间）")
		model   = flag.String("model", "ultra-model", "请求模型名")
		hold    = flag.Int("hold", 30, "预期上游按住秒数（仅记录进摘要）")
		quick   = flag.Bool("quick", false, "快速模式（不按住标记 + 非流式，用于风暴后恢复探针）")
		sportlo = flag.Int("sportlo", 0, "源端口下界（0 = 临时端口自动分配；>0 轮转绑定显式源端口乘算并发）")
		sporthi = flag.Int("sporthi", 0, "源端口上界（与 sportlo 同用，含端点）")
		outPath = flag.String("o", "", "摘要 JSON 输出路径（空 = 只打印 stdout）")
	)
	flag.Parse()

	if *srcips < 1 {
		*srcips = 1
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()

	// 源 IP 轮转拨号器：每连接绑一个 127.0.0.x 源地址，乘算临时端口空间。
	var dialSeq uint64
	transport := &http.Transport{
		DisableKeepAlives: true, // 每请求一条新连接：测的就是连接级资源
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			n := atomic.AddUint64(&dialSeq, 1)
			ip := fmt.Sprintf("127.0.0.%d", 1+(int(n)%(*srcips)))
			laddr := &net.TCPAddr{IP: net.ParseIP(ip)}
			// 显式源端口轮转：单源 IP 到单 dst 时临时端口自动分配仅 16384（49152-65535），
			// 绑定更宽的显式源端口区间可突破该上限（每源端口一个四元组）。
			if *sportlo > 0 && *sporthi >= *sportlo {
				span := uint64(*sporthi - *sportlo + 1)
				laddr.Port = *sportlo + int(n%span)
			}
			d := net.Dialer{LocalAddr: laddr, Timeout: 30 * time.Second}
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			// SO_LINGER(0)：关闭即发 RST，客户端不进 TIME_WAIT ⇒ 源端口立即可复用。
			// 现象：tier 10000 实测 1589 个「bind: can't assign requested address」端口耗尽；
			// 根因：客户端关连接进 TIME_WAIT（前档残留）吃光 16384 临时端口池，与网关能力无关；
			// 为什么这么修：负载发生器不该让 TIME_WAIT 成为瓶颈 —— RST 关闭让端口即时回收，
			//   跨档不再污染（网关/上游对已完成响应后的 RST 无感）。不修会怎样：高档位全被
			//   测试端假性端口耗尽掩盖，测不到网关真实上限。
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			return c, nil
		},
	}
	client := &http.Client{Transport: transport}

	var (
		mu       sync.Mutex
		ttfb     []time.Duration
		total    []time.Duration
		statuses = map[string]int{}
		errs     = map[string]int{}
	)
	var inflight int64
	var peakInflight int64
	var launched, completed, failed int64

	// 每秒在途曲线采样。
	curve := make([]sample, 0, int(dur.Seconds())+2)
	var curveMu sync.Mutex
	go func() {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		sec := 0
		for range tk.C {
			curveMu.Lock()
			curve = append(curve, sample{Sec: sec, Inflight: int(atomic.LoadInt64(&inflight))})
			curveMu.Unlock()
			sec++
		}
	}()

	body := buildBody(*model, *quick)
	endpoint := strings.TrimRight(*url, "/") + "/v1/chat/completions"

	// 爬坡启动：每 5ms 放行 ramp/200 个（小批次平滑放行，避免瞬间突发冲垮 somaxconn=128
	// 的 accept 队列 —— 实测 batch>128 的瞬时突发会以 reset 形式丢连接）。
	batch := *ramp / 200
	if batch < 1 {
		batch = 1
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var wg sync.WaitGroup

launch:
	for launched < int64(*target) {
		select {
		case <-ctx.Done():
			break launch
		case <-tick.C:
			for i := 0; i < batch && launched < int64(*target); i++ {
				launched++
				wg.Add(1)
				go func() {
					defer wg.Done()
					atomic.AddInt64(&inflight, 1)
					// 峰值在自增处采（完成路径采会漏掉「还在途」的高点）。
					atomicMax(&peakInflight, atomic.LoadInt64(&inflight))
					t0 := time.Now()
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
					if err != nil {
						atomic.AddInt64(&failed, 1)
						mu.Lock()
						errs["build_request"]++
						mu.Unlock()
						atomic.AddInt64(&inflight, -1)
						return
					}
					req.Header.Set("Content-Type", "application/json")
					if *token != "" {
						req.Header.Set("Authorization", "Bearer "+*token)
					}
					resp, err := client.Do(req)
					ttfbAt := time.Now()
					if err != nil {
						atomic.AddInt64(&failed, 1)
						mu.Lock()
						errs[classifyErr(err)]++
						mu.Unlock()
						atomic.AddInt64(&inflight, -1)
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					endAt := time.Now()
					atomic.AddInt64(&completed, 1)
					mu.Lock()
					statuses[strconv.Itoa(resp.StatusCode)]++
					ttfb = append(ttfb, ttfbAt.Sub(t0))
					total = append(total, endAt.Sub(t0))
					mu.Unlock()
					atomic.AddInt64(&inflight, -1)
				}()
			}
		}
	}
	// 等待在途收尾（或 deadline 到点后请求被 ctx 取消，各自落错误分类）。
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		<-done
	}
	wall := time.Since(started)

	// 峰值修正：曲线采样里的最大值可能高于自增处峰值。
	curveMu.Lock()
	for _, s := range curve {
		atomicMax(&peakInflight, int64(s.Inflight))
	}
	curveMu.Unlock()

	sum := summary{
		Meta: map[string]any{
			"tool":       "ultraload",
			"ts":         time.Now().Format("2006-01-02 15:04:05.000"),
			"mode":       map[bool]string{true: "quick", false: "storm"}[*quick],
			"target":     *target,
			"ramp_per_s": *ramp,
			"src_ips":    *srcips,
			"model":      *model,
			"hold_s":     *hold,
			"url":        *url,
			"wall_ms":    wall.Milliseconds(),
		},
		Result: map[string]any{
			"launched":       launched,
			"completed":      atomic.LoadInt64(&completed),
			"failed":         atomic.LoadInt64(&failed),
			"peak_inflight":  atomic.LoadInt64(&peakInflight),
			"ttfb_ms":        pctOf(ttfb),
			"total_ms":       pctOf(total),
			"status":         statuses,
			"errors":         errs,
			"inflight_curve": curve,
		},
	}
	blob, _ := json.MarshalIndent(sum, "", "  ")
	fmt.Println(string(blob))
	if *outPath != "" {
		if err := os.WriteFile(*outPath, blob, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "写摘要失败: %v\n", err)
			os.Exit(1)
		}
	}
}

// buildBody 构造 chat 请求体：storm 带 ultra-hold 标记（假上游按住），quick 不带。
func buildBody(model string, quick bool) []byte {
	msg := "ultra-hold"
	if quick {
		msg = "ultra-quick"
	}
	type m struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body := map[string]any{
		"model":    model,
		"messages": []m{{Role: "user", Content: msg}},
		"stream":   !quick,
	}
	blob, _ := json.Marshal(body)
	return blob
}

// classifyErr 把拨号 / 传输错误归类（每类是独立失败模式，报告要分开写）。
func classifyErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "Can't assign requested address") || strings.Contains(s, "address already in use"):
		return "dial_no_port" // 临时端口耗尽
	case strings.Contains(s, "connection refused"):
		return "dial_refused"
	case strings.Contains(s, "i/o timeout") && strings.Contains(s, "dial"):
		return "dial_timeout"
	case strings.Contains(s, "connection reset"):
		return "reset"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case strings.Contains(s, "context deadline exceeded") || strings.Contains(s, "Client.Timeout"):
		return "timeout"
	case strings.Contains(s, "EOF"):
		return "eof"
	default:
		return "other: " + s
	}
}

// pctOf 算最近秩分位（毫秒，浮点保留 1 位）。
func pctOf(ds []time.Duration) pct {
	if len(ds) == 0 {
		return pct{}
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := func(p float64) float64 {
		idx := int(math.Ceil(p*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return float64(sorted[idx].Microseconds()) / 1000.0
	}
	return pct{
		P50:  rank(0.50),
		P90:  rank(0.90),
		P99:  rank(0.99),
		P999: rank(0.999),
		Max:  float64(sorted[len(sorted)-1].Microseconds()) / 1000.0,
	}
}
