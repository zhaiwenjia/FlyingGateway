// 日期: 2026-11-14 15:05:41.583
// 作者: Zhai Wenjia

package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/policy"
	"github.com/zhaiwenjia/FlyingGateway/internal/registry"
	"github.com/zhaiwenjia/FlyingGateway/internal/telemetry"
)

// 本文件承载测试基建（假上游 / 注册表构造 / 原始 TCP 客户端）与 server.go 的端点测试。
// 假上游是真实 HTTP（httptest），回放 testdata/ 下按 SDD 4.3 文档字段构造的样本
// （⛔ 不手写合成 mock，见 testdata/README.md）。

// ---- fixture 加载 ----

// loadSSE 读 testdata 下的 SSE 样本并切成事件块（以空行分隔，块内保留原样）。
func loadSSE(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读 fixture %s 失败: %v", name, err)
	}
	var blocks []string
	for _, b := range strings.Split(string(raw), "\n\n") {
		if strings.TrimSpace(b) != "" {
			blocks = append(blocks, b+"\n\n")
		}
	}
	if len(blocks) == 0 {
		t.Fatalf("fixture %s 无事件块", name)
	}
	return blocks
}

// ---- 假上游（真实 HTTP，事件回放 + 时序控制）----

// fakeUpstream 是 httptest 假上游：按 fixture 事件块回放，支持「播完挂住 / 持续吐帧 /
// 手动推帧」三种时序控制（SC 时序测试用）；记录请求次数 / 请求头 / 请求体 /
// 成功写帧数 / 关闭时刻（SC-1 关闭延迟实测）。
type fakeUpstream struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	requests      int
	headers       []http.Header
	bodies        [][]byte
	writes        int
	closed        bool
	closedAt      time.Time
	defaultEvents []string
	eventsByPath  map[string][]string
	failFirstN    int                      // 前 N 个请求回 500（重试 / 降级测试用）
	rateLimit     map[string]fakeRateLimit // 凭据值 → 限流行为（令牌轮换测试用）
	pushMore      chan struct{}
	holdLimit     time.Duration
}

// fakeRateLimit 是假上游按凭据值的限流行为：回 count 次 429（带 Retry-After）。
type fakeRateLimit struct {
	count         int
	retryAfterSec int
}

// newFakeUpstream 起假上游（POST /responses 与 POST /messages 都接，按路径取事件）。
func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{t: t, pushMore: make(chan struct{}, 64), holdLimit: 10 * time.Second}
	f.srv = httptest.NewServer(f)
	return f
}

// setEvents 设置默认事件块（所有路径，除非 setEventsFor 覆盖）。
func (f *fakeUpstream) setEvents(blocks []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultEvents = blocks
}

// setEventsFor 设置指定路径的事件块。
func (f *fakeUpstream) setEventsFor(path string, blocks []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.eventsByPath == nil {
		f.eventsByPath = map[string][]string{}
	}
	f.eventsByPath[path] = blocks
}

// setFailFirstN 设「前 N 个请求回 500」（重试 / 降级测试用；持锁防 data race）。
func (f *fakeUpstream) setFailFirstN(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failFirstN = n
}

// setRateLimit 设「凭据值 authValue 的请求回 count 次 429 + Retry-After: N 秒」
// （令牌限流轮换测试用）。
func (f *fakeUpstream) setRateLimit(authValue string, count, retryAfterSec int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rateLimit == nil {
		f.rateLimit = map[string]fakeRateLimit{}
	}
	f.rateLimit[authValue] = fakeRateLimit{count: count, retryAfterSec: retryAfterSec}
}

// consumeRateLimit 消费一次限流（返回是否应发 429 及 Retry-After 秒数）。
func (f *fakeUpstream) consumeRateLimit(authValue string) (bool, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rl, ok := f.rateLimit[authValue]
	if !ok || rl.count <= 0 {
		return false, 0
	}
	rl.count--
	f.rateLimit[authValue] = rl
	return true, rl.retryAfterSec
}

// push 推一帧文本增量（手动时序控制）。
func (f *fakeUpstream) push() {
	select {
	case f.pushMore <- struct{}{}:
	default:
	}
}

// markClosed 记录上游连接被网关关闭的时刻（SC-1 关闭延迟实测的终点）。
func (f *fakeUpstream) markClosed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		f.closedAt = time.Now()
	}
}

// stats 返回（请求次数、成功写帧数、是否已关闭、关闭时刻）。
func (f *fakeUpstream) stats() (requests, writes int, closed bool, closedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.writes, f.closed, f.closedAt
}

// lastHeaders 返回最近一次请求头快照。
func (f *fakeUpstream) lastHeaders() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.headers) == 0 {
		return http.Header{}
	}
	return f.headers[len(f.headers)-1]
}

// lastBody 返回最近一次请求体快照。
func (f *fakeUpstream) lastBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil
	}
	return f.bodies[len(f.bodies)-1]
}

// headersAt 返回第 i 次请求的头快照（鉴权注入对账用）。
func (f *fakeUpstream) headersAt(i int) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.headers) {
		return http.Header{}
	}
	return f.headers[i]
}

// ServeHTTP 实现假上游：记账 → 按事件块回放 → 手动推帧 / 挂住（观察关闭）。
func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	f.requests++
	f.headers = append(f.headers, r.Header.Clone())
	f.bodies = append(f.bodies, body)
	events := f.defaultEvents
	if evs, ok := f.eventsByPath[r.URL.Path]; ok {
		events = evs
	}
	fail := f.requests <= f.failFirstN
	f.mu.Unlock()

	if fail {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"上游服务内部错误","type":"server_error","code":"upstream_error"}}`))
		return
	}
	// 令牌轮换测试：按凭据值回 429 + Retry-After（模拟上游对该令牌限流）。
	if limited, ra := f.consumeRateLimit(authValueOf(r)); limited {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(ra))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"令牌限流","type":"rate_limit","code":"rate_limit"}}`))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := http.NewResponseController(w)
	send := func(block string) bool {
		if _, err := w.Write([]byte(block)); err != nil {
			f.markClosed()
			return false
		}
		if err := fl.Flush(); err != nil {
			f.markClosed()
			return false
		}
		f.mu.Lock()
		f.writes++
		f.mu.Unlock()
		return true
	}
	for _, ev := range events {
		if !send(ev) {
			return
		}
	}
	// 手动推帧 + 挂住：让「取消后是否还会读 / 转发」成为可判定的观测量（SC-2 / SC-4）。
	deadline := time.NewTimer(f.holdLimit)
	defer deadline.Stop()
	tick := 0
	for {
		select {
		case <-r.Context().Done():
			f.markClosed()
			return
		case <-f.pushMore:
			tick++
			if !send(fmt.Sprintf("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":%d,\"delta\":\"tick\"}\n\n", 100+tick)) {
				return
			}
		case <-deadline.C:
			f.t.Errorf("假上游等待网关关闭超时（%s）—— 上游连接未被关闭", f.holdLimit)
			f.markClosed()
			return
		}
	}
}

// authValueOf 取请求携带的凭据值（三种鉴权形态任一；令牌轮换测试按值分派行为）。
func authValueOf(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return v
	}
	return r.Header.Get("Api-Key")
}

// close 关停假上游。
func (f *fakeUpstream) close() { f.srv.Close() }

// ---- 注册表配置构造 ----

// epSpec 是注册表端点项的构造参数（字段名与 registry.Endpoint 的 JSON tag 逐字一致）。
type epSpec struct {
	name, base, model, upstreamModel, provider, credRef string
	ctxWindow, maxOut, maxRetries, priority             int
	timeoutS                                            float64
	// 以下为可选配额声明（rate_limit / concurrency_limit / limits_group）。
	rateLimit        map[string]any
	concurrencyLimit int64
	limitsGroup      string
	breaker          map[string]any
}

// toWire 转注册表配置行（⛔ 缺任何必备字段 —— 与 9.2 启动校验一致）。
func (e epSpec) toWire() map[string]any {
	m := map[string]any{
		"name":              e.name,
		"base_url":          e.base,
		"model":             e.model,
		"upstream_model":    e.upstreamModel,
		"provider_name":     e.provider,
		"context_window":    e.ctxWindow,
		"max_output_tokens": e.maxOut,
		"capabilities": map[string]any{
			"supports_tools":  true,
			"supports_stream": true,
			"supports_vision": false,
		},
		"credential_ref": e.credRef,
		"timeout_s":      e.timeoutS,
		"max_retries":    e.maxRetries,
		"priority":       e.priority,
	}
	if e.rateLimit != nil {
		m["rate_limit"] = e.rateLimit
	}
	if e.concurrencyLimit != 0 {
		m["concurrency_limit"] = e.concurrencyLimit
	}
	if e.limitsGroup != "" {
		m["limits_group"] = e.limitsGroup
	}
	if e.breaker != nil {
		m["breaker"] = e.breaker
	}
	return m
}

// primaryEP 是默认主端点（OpenAI 形，凭据来源 TEST_KEY_OPENAI）。
func primaryEP(base string) epSpec {
	return epSpec{
		name: "primary", base: base, model: "gpt-4o-mini", upstreamModel: "gpt-4o-mini",
		provider: "openai", credRef: "TEST_KEY_OPENAI",
		ctxWindow: 128000, maxOut: 4096, maxRetries: 0, priority: 0, timeoutS: 5,
	}
}

// writeRegistry 写注册表配置并加载（strategy=priority；chains 可空；extraTop 可注入
// 顶层附加节，如 caller_rate_limits）。
func writeRegistry(t *testing.T, eps []epSpec, chains map[string][]string, extraTop ...map[string]any) *registry.Registry {
	t.Helper()
	epsWire := make([]map[string]any, 0, len(eps))
	for _, e := range eps {
		epsWire = append(epsWire, e.toWire())
	}
	cfg := map[string]any{"strategy": "priority", "endpoints": epsWire}
	if chains != nil {
		cfg["fallback_chains"] = chains
	}
	for _, x := range extraTop {
		for k, v := range x {
			cfg[k] = v
		}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("注册表配置序列化失败: %v", err)
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写注册表配置失败: %v", err)
	}
	reg, err := registry.Load(path)
	if err != nil {
		t.Fatalf("注册表加载失败: %v", err)
	}
	return reg
}

// ---- 测试环境 ----

// env 是一套「假上游 + 网关 + 观测出口」的测试环境（网关挂在真实 TCP 监听上）。
type env struct {
	t       *testing.T
	up      *fakeUpstream
	srv     *Server
	http    *httptest.Server
	export  *telemetry.InMemoryExporter
	metrics *telemetry.Metrics
}

// newEnv 装配测试环境：假上游 → 注册表 → server.New → httptest 真实监听。
// tweak 在 New 之前改 Options（限流 / 预算 / 护栏 / 取消窗口等）；
// extraTop 注入注册表顶层附加节（如 caller_rate_limits）。
func newEnv(t *testing.T, eps func(base string) []epSpec, chains map[string][]string, tweak func(o *Options), extraTop ...map[string]any) *env {
	t.Helper()
	t.Setenv("TEST_KEY_OPENAI", "sk-gw-openai-0001")
	t.Setenv("TEST_KEY_ANTHROPIC", "sk-gw-anthropic-0002")
	t.Setenv("TEST_KEY_MIMO", "sk-gw-mimo-0003")

	up := newFakeUpstream(t)
	t.Cleanup(up.close) // 后注册 ⇒ 先关网关监听再关假上游
	reg := writeRegistry(t, eps(up.srv.URL), chains, extraTop...)
	exp := &telemetry.InMemoryExporter{}
	met := telemetry.NewMetrics()
	opts := Options{
		Registry:            reg,
		Tracer:              telemetry.NewTracer(exp),
		Metrics:             met,
		AllowLoopbackEgress: true, // 假上游在 127.0.0.1（统一 SDD 8.2 环回可配放行）
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tweak != nil {
		tweak(&opts)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("server.New 失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &env{t: t, up: up, srv: srv, http: ts, export: exp, metrics: met}
}

// postJSON 发 JSON 请求（走真实 HTTP）。
func (e *env) postJSON(path, body string, hdr map[string]string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.http.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("请求 %s 失败: %v", path, err)
	}
	return resp
}

// get 发 GET 请求。
func (e *env) get(path string) *http.Response {
	e.t.Helper()
	resp, err := http.Get(e.http.URL + path)
	if err != nil {
		e.t.Fatalf("GET %s 失败: %v", path, err)
	}
	return resp
}

// readBody 读响应体并解 JSON 到 map。
func readBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("响应体不是 JSON: %v（原文 %s）", err, raw)
	}
	return m
}

// errorFields 取错误响应的 error 对象字段。
func errorFields(t *testing.T, m map[string]any) (typ, code, msg string) {
	t.Helper()
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应体无 error 对象: %v", m)
	}
	typ, _ = e["type"].(string)
	code, _ = e["code"].(string)
	msg, _ = e["message"].(string)
	return typ, code, msg
}

// waitUntil 轮询等待条件成立（10ms 步进；超时返回 false）。
func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// ---- 原始 TCP 客户端（SC 断连测试用：可随时硬断）----

// rawStream 是原始 TCP 上的 HTTP/1.1 流式响应读取器。
type rawStream struct {
	conn   net.Conn
	br     *bufio.Reader
	header bool
}

// dialRaw 连网关数据面（真实 socket）。
func dialRaw(t *testing.T, gatewayURL string) *rawStream {
	t.Helper()
	addr := strings.TrimPrefix(gatewayURL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("拨号 %s 失败: %v", addr, err)
	}
	return &rawStream{conn: conn, br: bufio.NewReader(conn)}
}

// send 写原始 HTTP/1.1 POST 请求。
func (s *rawStream) send(t *testing.T, path, host, body string, hdr map[string]string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n", path, host, len(body))
	for k, v := range hdr {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	if _, err := s.conn.Write([]byte(b.String())); err != nil {
		t.Fatalf("写请求失败: %v", err)
	}
}

// readHeaders 读完响应头（跳过）。
func (s *rawStream) readHeaders(t *testing.T) {
	t.Helper()
	if s.header {
		return
	}
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			t.Fatalf("读响应头失败: %v", err)
		}
		if line == "\r\n" {
			s.header = true
			return
		}
	}
}

// nextEvent 读一个 SSE 事件块（到空行为止）；流结束返回 ("", io.EOF)。
func (s *rawStream) nextEvent(t *testing.T) (string, error) {
	t.Helper()
	s.readHeaders(t)
	var b strings.Builder
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			return b.String(), err
		}
		b.WriteString(line)
		if line == "\n" || line == "\r\n" {
			return b.String(), nil
		}
	}
}

// countContent 统计 SSE 事件块里的 chat.completion.chunk 内容分片（delta.content 非空）。
func countContent(blocks []string) int {
	n := 0
	for _, b := range blocks {
		if !strings.Contains(b, "chat.completion.chunk") {
			continue
		}
		var chunk map[string]any
		for _, line := range strings.Split(b, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
				continue
			}
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			cm, _ := c.(map[string]any)
			delta, _ := cm["delta"].(map[string]any)
			if content, _ := delta["content"].(string); content != "" {
				n++
			}
		}
	}
	return n
}

// hasSentinel 判断事件块里是否出现终止哨兵 data: [DONE]。
func hasSentinel(blocks []string) bool {
	for _, b := range blocks {
		if strings.Contains(b, "data: [DONE]") {
			return true
		}
	}
	return false
}

// ---- server.go 端点测试 ----

// TestHealthEndpoint 断言 GET /health 存活。
func TestHealthEndpoint(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	resp := e.get("/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health = %d，期望 200", resp.StatusCode)
	}
	m := readBody(t, resp)
	if m["status"] != "ok" {
		t.Fatalf("health.status = %v，期望 ok", m["status"])
	}
}

// TestModelsEndpointMatchesRegistry 断言 /v1/models 的值与注册表逐字一致
// （统一 SDD 2.3：id=客户端别名、owned_by=provider_name、context_window /
// max_output_tokens / capabilities 全部来自注册表，⛔ 不动态发现）。
func TestModelsEndpointMatchesRegistry(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec {
		return []epSpec{primaryEP(base)}
	}, nil, nil)
	resp := e.get("/v1/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models = %d，期望 200", resp.StatusCode)
	}
	m := readBody(t, resp)
	if m["object"] != "list" {
		t.Fatalf("object = %v，期望 list", m["object"])
	}
	data, _ := m["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data 长度 = %d，期望 1", len(data))
	}
	item, _ := data[0].(map[string]any)
	want := map[string]any{
		"id":                "gpt-4o-mini",
		"object":            "model",
		"owned_by":          "openai",
		"context_window":    float64(128000),
		"max_output_tokens": float64(4096),
	}
	for k, v := range want {
		if item[k] != v {
			t.Errorf("models.%s = %v，期望与注册表逐字一致 %v", k, item[k], v)
		}
	}
	caps, _ := item["capabilities"].(map[string]any)
	if caps["tools"] != true || caps["stream"] != true || caps["vision"] != false {
		t.Errorf("models.capabilities = %v，期望 {tools:true stream:true vision:false}（与注册表逐字一致）", caps)
	}
}

// TestModelNotFoundReturns404 断言列表外模型 ⇒ 404 model_not_found 且上游调用 0
// （统一 SDD 9.1：⛔ 不透传任意模型名）。
func TestModelNotFoundReturns404(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	resp := e.postJSON("/v1/chat/completions", `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态 = %d，期望 404", resp.StatusCode)
	}
	typ, code, _ := errorFields(t, readBody(t, resp))
	if code != "model_not_found" {
		t.Errorf("code = %q，期望 model_not_found（type=%q）", code, typ)
	}
	if n, _, _, _ := e.up.stats(); n != 0 {
		t.Errorf("上游调用数 = %d，期望 0（⛔ 不透传任意模型名）", n)
	}
}

// TestAdminDefaultOff 断言管理面默认关闭（数据面 mux 不含 /admin 路由）。
func TestAdminDefaultOff(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	resp := e.get("/admin/endpoints")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /admin/endpoints = %d，期望 404（管理面默认关闭）", resp.StatusCode)
	}
}

// TestAdminRequiresToken 断言管理面强制鉴权：无 token 不给起、无凭据 401、正确 Bearer 200。
func TestAdminRequiresToken(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	if _, err := e.srv.AdminHandler(""); err == nil {
		t.Fatal("AdminHandler(\"\") 应报错（管理面强制鉴权）")
	}
	admin, err := e.srv.AdminHandler("adm-token")
	if err != nil {
		t.Fatalf("AdminHandler 报错: %v", err)
	}
	ts := httptest.NewServer(admin)
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/admin/endpoints")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无凭据 = %d，期望 401", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/endpoints", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("凭据错误 = %d，期望 401", resp2.StatusCode)
	}
	req3, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/endpoints", nil)
	req3.Header.Set("Authorization", "Bearer adm-token")
	resp3, _ := http.DefaultClient.Do(req3)
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("凭据正确 = %d，期望 200", resp3.StatusCode)
	}
	m := readBody(t, resp3)
	if _, ok := m["endpoints"]; !ok {
		t.Errorf("管理面视图缺 endpoints 字段: %v", m)
	}
}

// TestValidateAdminAddr 断言管理面只绑内网（统一 SDD 8.5：⛔ 不绑公网、⛔ 不绑全接口）。
func TestValidateAdminAddr(t *testing.T) {
	ok := []string{"127.0.0.1:9090", "[::1]:9090", "10.0.0.5:9090", "192.168.1.2:9090", "172.16.3.4:9090"}
	for _, addr := range ok {
		if err := ValidateAdminAddr(addr); err != nil {
			t.Errorf("ValidateAdminAddr(%q) = %v，期望放行", addr, err)
		}
	}
	bad := []string{"0.0.0.0:9090", ":9090", "[::]:9090", "8.8.8.8:9090", "myhost.internal:9090"}
	for _, addr := range bad {
		if err := ValidateAdminAddr(addr); err == nil {
			t.Errorf("ValidateAdminAddr(%q) 放行，期望拒绝（⛔ 不绑公网 / 全接口 / 主机名）", addr)
		}
	}
}

// TestCancelEndpointShape 断言 POST /v1/cancel 的请求体口径与幂等语义
// （fire-and-forget：查不到 / 已收口 ⇒ 静默忽略，统一 SDD 7.5）。
func TestCancelEndpointShape(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil)
	resp := e.postJSON("/v1/cancel", `{"foo":"bar"}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 client_request_id = %d，期望 400", resp.StatusCode)
	}
	readBody(t, resp)

	resp2 := e.postJSON("/v1/cancel", `{"client_request_id":"unknown-id"}`, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("未知 id = %d，期望 200（静默忽略、不报错）", resp2.StatusCode)
	}
	m := readBody(t, resp2)
	if m["cancelled"] != false {
		t.Errorf("未知 id 的 cancelled = %v，期望 false", m["cancelled"])
	}
}

// TestLimitGroupAcquireRelease 断言并发位的占/放语义：满则拒（标记并发闸）、放后可复用。
func TestLimitGroupAcquireRelease(t *testing.T) {
	g := &limitGroup{maxInFlight: 1}
	if ok, _, _ := g.acquire(1); !ok {
		t.Fatal("首占应成功")
	}
	ok, wait, concurrency := g.acquire(1)
	if ok {
		t.Fatal("并发位已满时应拒绝")
	}
	if !concurrency || wait <= 0 {
		t.Fatalf("拒绝应标记并发闸且给 Retry-After（concurrency=%v wait=%v）", concurrency, wait)
	}
	g.release()
	if ok, _, _ := g.acquire(1); !ok {
		t.Fatal("释放后应可复用")
	}
}

// TestLimitGroupNoHalfCharge 断言「先占位后扣桶、桶不足退位」：契约桶拒绝后
// 并发位必须退回（否则拒绝路径会漏占并发位，把后续流量挤死）。
func TestLimitGroupNoHalfCharge(t *testing.T) {
	g := &limitGroup{maxInFlight: 5, buckets: policy.NewRateLimitBuckets(600, 0, 1)} // token 桶深 10
	if ok, _, _ := g.acquire(10); !ok {
		t.Fatal("首占应成功")
	}
	ok, _, concurrency := g.acquire(10) // 桶不足
	if ok {
		t.Fatal("契约桶不足时应拒绝")
	}
	if concurrency {
		t.Fatal("失败应在契约桶闸（非并发闸）")
	}
	// 首发占位未放（仍持有 1 位），失败发的占位必须退回 ⇒ 总在途 = 1。
	if got := g.inFlight.Load(); got != 1 {
		t.Fatalf("桶拒绝后失败发的并发位未退回：inFlight = %d，期望 1（首发仍持有）", got)
	}
	g.release()
	if got := g.inFlight.Load(); got != 0 {
		t.Fatalf("release 后 inFlight = %d，期望 0", got)
	}
}
