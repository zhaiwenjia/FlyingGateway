// 日期: 2026-11-14 14:36:39.153
// 作者: Zhai Wenjia

// Package server 是服务装配层（统一 SDD 2.1 ① 入口层）：
// 数据面双入口（/v1/chat/completions、/v1/responses）+ /v1/models + /health + /v1/cancel，
// 管理面（/admin/endpoints、/admin/health、/admin/credential-pools、/metrics）默认关闭、
// 显式开启 + 强制鉴权 + 只绑内网。
// 执行与取消见 execute.go / cancel.go。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/cost"
	"github.com/zhaiwenjia/FlyingGateway/internal/policy"
	"github.com/zhaiwenjia/FlyingGateway/internal/registry"
	"github.com/zhaiwenjia/FlyingGateway/internal/share"
	"github.com/zhaiwenjia/FlyingGateway/internal/telemetry"
)

// Options 是服务装配输入（cmd/gateway 装配后传入；测试直接构造）。
type Options struct {
	// Registry 是端点注册表（路由唯一真源，统一 SDD 9.1）。
	Registry *registry.Registry
	// Rates 是费率表（统一 SDD 5.4 / 9.3；nil ⇒ 费用按 CostUnknown 计，⛔ 不猜价）。
	Rates *cost.Rates
	// Tracer 是 span 构造器（每次上游执行一个 CLIENT span，统一 SDD 6.2）。
	Tracer *telemetry.Tracer
	// Metrics 是取消事件独立计数指标（统一 SDD 6.4）。
	Metrics *telemetry.Metrics

	// TokenBucket 是 token 级限流（nil ⇒ 不限流；统一 SDD 7.2）。
	TokenBucket *policy.TokenBucket
	// Budget 是每调用方 token 预算（nil ⇒ 不限预算；统一 SDD 7.2）。
	Budget *policy.Budget
	// Guardrail 是内容护栏（nil ⇒ 不设护栏；统一 SDD 2.3 治理三件套）。
	Guardrail *policy.Guardrail

	// AllowLoopbackEgress 控制出网校验是否放行环回地址（本地 Ollama 等，统一 SDD 8.2）。
	AllowLoopbackEgress bool

	// ---- 调用方校验（registry callers 单源，R-43 / AC-36）----
	// 调用方登记（哈希 key + 过期 + scope + 配额）全部来自注册表 callers 节：
	// callers 为空 ⇒ 开放模式（不校验，向后兼容本地形态）；非空 ⇒ /v1/* 强制鉴权，
	// 缺失 / 无效 / 过期 401、超 scope 403（鉴权面见 caller.go）。

	// MaxInFlight 是过载保护的在途请求数上限（load shedding，锚定实测容量，统一 SDD 7.2
	// 锚点分离：限流护套餐配额、过载闸护网关自身容量）；≤ 0 = 不启用。
	// 超限 ⇒ 429 + Retry-After（⛔ 不排队无界、⛔ 不静默丢弃）。
	MaxInFlight int64

	// UpstreamProbeInterval 是上游周期巡检间隔（统一 SDD 2.8：主动巡检是预防）；
	// 0 = 不启用（默认关、显式开）。探测为 HTTP 可达性巡检（⛔ 不发模型调用、不花配额），
	// 结果进 /admin/health 的 probes 视图并与熔断器联动（不可达 ⇒ 主动熔断）。
	UpstreamProbeInterval time.Duration
	// UpstreamProbeTimeout 是单次巡检探测超时（默认 3s）。
	UpstreamProbeTimeout time.Duration

	// Share 是跨实例共享计数存储（统一 SDD 7.2 多实例一致性）；nil = 内存模式（单实例）。
	// 非 nil ⇒ 限流桶 / 预算 / 上游冷却计数存该存储，多实例共享一份配额（⛔ 不再 N 倍配额）。
	Share share.Store

	// RegistryPath / RatesPath 是配置文件路径（热加载数据源，统一 SDD 9.2 / 5.4）。
	// 非空 ⇒ 支持 SIGHUP 与 POST /admin/reload 热加载（重读该路径、原子替换）；
	// 空 ⇒ 热加载显式报错（⛔ 不静默无操作）。
	RegistryPath string
	RatesPath    string

	// ---- 上游自有令牌池与限流轮换 ----

	// UpstreamBucketCapacity / UpstreamBucketRefill 是每自有令牌的本地令牌桶；≤ 0 = 不启用
	// （轮换仍按上游 429 冷却生效）。
	UpstreamBucketCapacity int64
	UpstreamBucketRefill   float64
	// RateLimitCooldown 是上游 429 无 Retry-After 时的默认冷却（默认 30s）。
	RateLimitCooldown time.Duration
	// MaxTokenWait 是「全部自有令牌受限」时的最长等待上限（默认 3s；
	// 超上限 ⇒ 429 + Retry-After=最短等待，⛔ 不盲发）。
	MaxTokenWait time.Duration

	// CancelGracePeriod 是延迟取消窗口（默认 0 = 立即取消，统一 SDD 3.7）。
	CancelGracePeriod time.Duration
	// CancelGraceByProvider 是按 provider 的窗口覆盖（统一 SDD 3.7 作用域）。
	CancelGraceByProvider map[string]time.Duration
	// CancelRetention 是取消终态条目的保留时长（同 id 后续尝试被拒的窗口，SC-5）。
	CancelRetention time.Duration
	// DisconnectPollInterval 是断连轮询兜底周期（watcher + 轮询双路，统一 SDD 6.6 要求 1；
	// 检测延迟上界 = 该周期的 4 倍，默认 1s 上界 ⇒ 默认周期 250ms）。
	DisconnectPollInterval time.Duration

	// Logger 是结构化日志出口（nil ⇒ slog.Default()）。
	Logger *slog.Logger
}

// gatewayCfg 是可热加载配置的原子快照（注册表 + 费率表配套替换，⛔ 不出现
// 新注册表配旧费率表的混合态；对象发布后不可变，统一 SDD 9.2）。
type gatewayCfg struct {
	reg   *registry.Registry
	rates *cost.Rates
}

// Server 是网关服务（数据面 + 管理面）。
type Server struct {
	cfg       atomic.Pointer[gatewayCfg]
	regPath   string // 热加载数据源（空 = 不支持热加载）
	ratesPath string
	tracer    *telemetry.Tracer
	metrics   *telemetry.Metrics

	bucket    *policy.TokenBucket
	budget    *policy.Budget
	guardrail *policy.Guardrail

	callers      *callerAuth
	pools        *poolManager
	maxTokenWait time.Duration

	// limitGroups 是配额共享组（键 = limits_group，缺省 = 端点名）：同组共享一份契约桶 +
	// 一个在途并发计数（调研结论：并发按在途上游尝试计、归属按供应商配额 scope，
	// 见 docs/20261114-多供应商并发-调研-流量上限归属与并发计量.md）。
	limitGroups map[string]*limitGroup
	// maxInFlight / inFlight 是过载闸（load shedding；≤ 0 = 不启用）。
	maxInFlight int64
	inFlight    atomic.Int64

	// breakers 是端点熔断器（键 = 端点名，统一 SDD 7.3 三态；注册表每端点一个）。
	breakers map[string]*EndpointBreaker

	// prober 是上游周期巡检器（nil = 不启用）。
	prober *upstreamProber
	// shuttingDown 由 Shutdown 置位：readiness 转 503（先摘流量再排空，K8s readiness 口径）。
	shuttingDown atomic.Bool

	allowLoopback bool
	pollInterval  time.Duration
	cancels       *cancelRegistry
	logger        *slog.Logger
}

// limitGroup 是一个配额共享组（端点级限流的两道闸：在途并发位 + 契约桶）。
// 并发计量单位 = 在途上游尝试（runEndpoint 发起 acquire / 收口 release，流式长调用
// 占位到流收口）；⛔ 不按 TCP 连接数计（HTTP/2 多路复用下连接数不约束并发）。
type limitGroup struct {
	// buckets 是契约桶（tpm/rpm 双维；nil = 不限流）。
	buckets *policy.RateLimitBuckets
	// maxInFlight 是在途并发上限（0 = 不限）。
	maxInFlight int64
	inFlight    atomic.Int64
}

// acquire 过两道闸（先并发位后契约桶 —— 桶是消耗品不回滚，并发位免费可退，
// 故先占位后扣桶，桶不足时退位）：任一不足 ⇒ 拒绝并给出 Retry-After。
// 返回的 concurrency 标识失败发生在哪道闸（对外错误分类同为 rate_limit，message 分开）。
// 并发位满的等待按 1s 保守估（位释放时机不可预知，⛔ 不猜精确时刻）。
func (g *limitGroup) acquire(est int64) (ok bool, retryAfter time.Duration, concurrency bool) {
	if g.maxInFlight > 0 && g.inFlight.Add(1) > g.maxInFlight {
		g.inFlight.Add(-1)
		return false, time.Second, true
	}
	if g.buckets != nil {
		if okB, wait := g.buckets.Allow(est); !okB {
			if g.maxInFlight > 0 {
				g.inFlight.Add(-1)
			}
			return false, wait, false
		}
	}
	return true, 0, false
}

// release 释放一个并发位（runEndpoint 收口时；幂等由调用方 defer 保证恰好一次）。
func (g *limitGroup) release() {
	if g.maxInFlight > 0 {
		g.inFlight.Add(-1)
	}
}

// DefaultDetectBound 是断连检测延迟上界默认值（统一 SDD 6.6 要求 1「检测延迟上界可配置，默认 1s」）。
const DefaultDetectBound = 1 * time.Second

// New 装配网关服务。必填：Registry / Tracer / Metrics（⛔ 不用 nil 顶替）。
func New(opts Options) (*Server, error) {
	if opts.Registry == nil {
		return nil, fmt.Errorf("server: Registry 必填（注册表是路由的唯一真源）")
	}
	if opts.Tracer == nil {
		return nil, fmt.Errorf("server: Tracer 必填（每次上游执行一个 CLIENT span）")
	}
	if opts.Metrics == nil {
		return nil, fmt.Errorf("server: Metrics 必填（取消事件独立计数指标）")
	}
	poll := opts.DisconnectPollInterval
	if poll <= 0 {
		// 检测延迟上界默认 1s ⇒ 轮询兜底周期取上界的 1/4（调度极值下仍有界）。
		poll = DefaultDetectBound / 4
	}
	lg := opts.Logger
	if lg == nil {
		lg = slog.Default()
	}
	s := &Server{
		regPath:       opts.RegistryPath,
		ratesPath:     opts.RatesPath,
		tracer:        opts.Tracer,
		metrics:       opts.Metrics,
		bucket:        opts.TokenBucket,
		budget:        opts.Budget,
		guardrail:     opts.Guardrail,
		pools:         nil, // 契约配额构建后于下文赋值（limits 依赖注册表登记）
		maxTokenWait:  opts.MaxTokenWait,
		maxInFlight:   opts.MaxInFlight,
		allowLoopback: opts.AllowLoopbackEgress,
		pollInterval:  poll,
		logger:        lg,
	}
	s.cfg.Store(&gatewayCfg{reg: opts.Registry, rates: opts.Rates})
	// 配额共享组装配（统一 SDD 7.2 锚点分离：限流锚定契约，未登记 ⇒ 不限）：
	// 同组端点共享同一个 limitGroup 实例（一份桶 + 一个并发计数，⛔ 不按端点各建一份 ——
	// 共享配额池按端点各计一份会 N 倍超发）。
	// 共享存储故障口径：fail-open 放行 + 显式告警（⛔ 不静默；详见 policy 双模式注释）。
	onShareError := func(err error) {
		lg.Warn("共享计数存储故障（限流/预算 fail-open 放行，恢复后自动回归）", "err", err)
	}
	s.limitGroups = make(map[string]*limitGroup)
	for _, ep := range opts.Registry.List() {
		if ep.RateLimit == nil && ep.ConcurrencyLimit <= 0 {
			continue // 无登记 = 不限流，不建组
		}
		key := ep.LimitsGroup
		if key == "" {
			key = ep.Name
		}
		if _, ok := s.limitGroups[key]; ok {
			continue // 同组声明经 registry 一致性校验，首个即代表
		}
		g := &limitGroup{maxInFlight: ep.ConcurrencyLimit}
		if ep.RateLimit != nil {
			if opts.Share != nil {
				g.buckets = policy.NewSharedRateLimitBuckets(opts.Share, "fg:rl:group:"+key, onShareError,
					ep.RateLimit.TPM, ep.RateLimit.RPM, ep.RateLimit.BurstS)
			} else {
				g.buckets = policy.NewRateLimitBuckets(ep.RateLimit.TPM, ep.RateLimit.RPM, ep.RateLimit.BurstS)
			}
		}
		s.limitGroups[key] = g
	}
	// 上游令牌池契约配额（多 key：per_key 每 key 一份桶 / shared 全池共享 / key_limits 逐 key 覆盖）。
	// key_ref 环境变量未设置 ⇒ 启动报错（⛔ 不静默忽略登记）。
	poolLim := make(map[string]*poolLimits, len(opts.Registry.CredentialRateLimits))
	for _, crl := range opts.Registry.CredentialRateLimits {
		if strings.TrimSpace(os.Getenv(crl.KeyRef)) == "" {
			return nil, fmt.Errorf("server: credential_rate_limits 的 key_ref %q 未在环境变量中取到值（⛔ 不静默忽略登记）", crl.KeyRef)
		}
		pl := &poolLimits{ref: crl.KeyRef, store: opts.Share, onShareError: onShareError}
		if crl.PerKey != nil {
			pl.perKey = &rlSpec{tpm: crl.PerKey.TPM, rpm: crl.PerKey.RPM, burstS: crl.PerKey.BurstS}
		}
		if crl.Shared != nil {
			if opts.Share != nil {
				pl.shared = policy.NewSharedRateLimitBuckets(opts.Share, "fg:rl:poolc:"+crl.KeyRef, onShareError,
					crl.Shared.TPM, crl.Shared.RPM, crl.Shared.BurstS)
			} else {
				pl.shared = policy.NewRateLimitBuckets(crl.Shared.TPM, crl.Shared.RPM, crl.Shared.BurstS)
			}
		}
		for _, kl := range crl.KeyLimits {
			pl.keyLimits = append(pl.keyLimits, &rlSpec{tpm: kl.TPM, rpm: kl.RPM, burstS: kl.BurstS})
		}
		poolLim[crl.KeyRef] = pl
	}

	s.pools = newPoolManager(opts.UpstreamBucketCapacity, opts.UpstreamBucketRefill, opts.RateLimitCooldown, poolLim, opts.Share, onShareError)
	// 调用方校验面（registry callers 单源）：哈希 key + 过期 + scope + 配额随登记构造。
	s.callers = newCallerAuth(opts.Registry.Callers, s.logger, opts.Share, onShareError)
	// 端点熔断器（统一 SDD 7.3）：逐端点按注册表 breaker 配置构造（缺省 = 默认 5/30s/1）。
	s.breakers = make(map[string]*EndpointBreaker, len(opts.Registry.List()))
	for _, ep := range opts.Registry.List() {
		th, cd, pb := 0, time.Duration(0), 0
		if ep.Breaker != nil {
			th, cd, pb = ep.Breaker.FailureThreshold, time.Duration(ep.Breaker.CooldownS*float64(time.Second)), ep.Breaker.Probe
		}
		s.breakers[ep.Name] = NewEndpointBreaker(ep.Name, th, cd, pb, s.logger)
	}
	// 上游周期巡检器（统一 SDD 2.8；interval=0 ⇒ 不启用，newUpstreamProber 返回 nil）。
	s.prober = newUpstreamProber(opts.Registry.List(), s.breakers,
		opts.UpstreamProbeInterval, opts.UpstreamProbeTimeout, s.allowLoopback, s.logger)
	if s.maxTokenWait <= 0 {
		s.maxTokenWait = DefaultMaxTokenWait
	}
	installAuthTransport() // 凭据注入点（ctx 键精确到请求，统一 SDD 8.4）
	s.cancels = newCancelRegistry(opts.CancelGracePeriod, opts.CancelGraceByProvider, opts.CancelRetention)
	return s, nil
}

// Handler 返回数据面路由（统一 SDD 2.1 ①）。
// /v1/* 经调用方鉴权（注册表 callers 非空时强制）；/health 保持开放（存活探针）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.callers.wrap(s.handleChatCompletions))
	mux.HandleFunc("POST /v1/responses", s.callers.wrap(s.handleResponses))
	mux.HandleFunc("POST /v1/messages", s.callers.wrap(s.handleMessages))
	mux.HandleFunc("GET /v1/models", s.callers.wrap(s.handleModels))
	mux.HandleFunc("POST /v1/cancel", s.callers.wrap(s.handleCancel))
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /health/readiness", s.handleReadiness)
	return mux
}

// handleReadiness 是就绪探针（GET /health/readiness，K8s readiness 口径：管「能不能接流量」）：
// 就绪 ⇒ 200 {"status":"ready"}；Shutdown 进行中 ⇒ 503（先摘流量再排空在途执行）。
// ⛔ 不因上游端点不健康而 503 —— 上游是共享外部依赖，全部实例会同时不就绪 ⇒
// 全摘流量比上游故障本身更糟；上游健康由熔断器逐端点管理（统一 SDD 2.8 / 7.3 分工）。
func (s *Server) handleReadiness(w http.ResponseWriter, _ *http.Request) {
	if s.shuttingDown.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "shutting_down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

// AdminHandler 返回管理面路由（统一 SDD 8.5：强制鉴权；只读视图）。
// 调用方必须先过 ValidateAdminAddr 保证只绑内网。
func (s *Server) AdminHandler(adminToken string) (http.Handler, error) {
	if adminToken == "" {
		return nil, fmt.Errorf("server: 管理面强制鉴权 —— admin token 为空时拒绝起管理面（统一 SDD 2.3 域⑤ / 8.5）")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/endpoints", s.adminAuth(adminToken, s.handleAdminEndpoints))
	mux.HandleFunc("GET /admin/health", s.adminAuth(adminToken, s.handleAdminHealth))
	mux.HandleFunc("GET /admin/credential-pools", s.adminAuth(adminToken, s.handleAdminCredentialPools))
	mux.HandleFunc("GET /metrics", s.adminAuth(adminToken, s.handleAdminMetrics))
	mux.HandleFunc("POST /admin/reload", s.adminAuth(adminToken, s.handleAdminReload))
	return mux, nil
}

// handleAdminMetrics 输出 Prometheus 文本暴露格式 0.0.4 指标（GET /metrics，管理面鉴权）：
// telemetry 指标（SDD 6.4）+ 端点熔断状态 gauge（closed=0 / half_open=1 / open=2）。
// Content-Type 逐字按官方口径 `text/plain; version=0.0.4; charset=utf-8`（Prometheus 3.0
// 起抓取端校验 Content-Type，缺失/不受支持 ⇒ 抓取失败）。
func (s *Server) handleAdminMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.WritePrometheus(w)
	// 端点熔断状态（统一 SDD 7.3 可观测：⛔ 静默熔断是谜之故障）。
	if len(s.breakers) > 0 {
		names := make([]string, 0, len(s.breakers))
		for name := range s.breakers {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintln(w, "# HELP gateway_endpoint_breaker_state 端点熔断状态（0=closed 1=half_open 2=open，SDD 7.3）。")
		fmt.Fprintln(w, "# TYPE gateway_endpoint_breaker_state gauge")
		for _, name := range names {
			fmt.Fprintf(w, "gateway_endpoint_breaker_state{endpoint=%q} %d\n", name, s.breakers[name].StateCode())
		}
	}
}

// adminAuth 是管理面强制鉴权（Bearer token，常量时间比较；鉴权失败 401）。
func (s *Server) adminAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	want := "Bearer " + token
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			writeError(w, http.StatusUnauthorized, "auth", "unauthorized", "管理面需要有效 Bearer token")
			return
		}
		next(w, r)
	}
}

// ValidateAdminAddr 校验管理面监听地址只绑内网（统一 SDD 8.5「管理面默认不绑定公网」）：
// 主机必须是环回（127.0.0.0/8、::1）或私网（10/8、172.16/12、192.168/16、fc00::/7）；
// 0.0.0.0 / :: / 空主机（全接口）/ 公网地址一律拒绝 —— 公网暴露必须是显式动作。
func ValidateAdminAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("server: 管理面监听地址 %q 非法（须 host:port）: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("server: 管理面监听地址 %q 绑全接口（⛔ 不绑公网），须显式给内网 IP", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// 主机名不解析判定（解析结果可能随 DNS 变化 ⇒ 不可复现）⇒ 显式拒绝。
		return fmt.Errorf("server: 管理面监听地址 %q 必须是 IP 字面量（⛔ 不用主机名，防解析漂移到公网）", addr)
	}
	if ip.IsLoopback() {
		return nil
	}
	if ip.IsPrivate() {
		return nil
	}
	return fmt.Errorf("server: 管理面监听地址 %q 非环回 / 私网（⛔ 不绑公网）", addr)
}

// Shutdown 优雅关闭：进程关闭 ⇒ 全部在途执行以 shutdown 原因立即收口
// （统一 SDD 6.6 要求 4 / 8.5）。
func (s *Server) Shutdown() {
	s.shuttingDown.Store(true) // readiness 转 503：先摘流量（K8s readiness 口径）
	if s.prober != nil {
		s.prober.Stop() // 停上游巡检（⛔ 关闭后继续敲上游）
	}
	s.cancels.cancelAll(reasonShutdown)
}

// ExecutionSnapshot 返回指定 client_request_id 的执行观测快照（测试与排障用，
// 统一 SDD 6.5 L1 网关自证）。
func (s *Server) ExecutionSnapshot(clientRequestID string) (ExecSnapshot, bool) {
	return s.cancels.snapshot(clientRequestID)
}

// CancelPendingCount 返回当前 CANCEL_PENDING 请求数（统一 SDD 3.7
// gateway.cancel.pending_count）。
func (s *Server) CancelPendingCount() int { return s.cancels.pendingCount() }

// ---- 数据面端点 ----

// handleHealth 是存活探针（GET /health）。
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// modelInfo 是 /v1/models 的一项（值与注册表逐字一致，统一 SDD 2.3）。
type modelInfo struct {
	ID              string `json:"id"`
	Object          string `json:"object"`
	OwnedBy         string `json:"owned_by"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Capabilities    struct {
		Tools  bool `json:"tools"`
		Stream bool `json:"stream"`
		Vision bool `json:"vision"`
	} `json:"capabilities"`
}

// handleModels 由注册表生成模型列表（GET /v1/models，统一 SDD 2.3 / 9.1）：
// ⛔ 不做动态模型发现，注册表是唯一真源。
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	eps := s.curReg().List()
	data := make([]modelInfo, 0, len(eps))
	for _, ep := range eps {
		var m modelInfo
		m.ID = ep.Model // 客户端别名
		m.Object = "model"
		m.OwnedBy = ep.ProviderName
		m.ContextWindow = ep.ContextWindow
		m.MaxOutputTokens = ep.MaxOutputTokens
		if ep.Capabilities != nil {
			m.Capabilities.Tools = ep.Capabilities.SupportsTools
			m.Capabilities.Stream = ep.Capabilities.SupportsStream
			m.Capabilities.Vision = ep.Capabilities.SupportsVision
		}
		data = append(data, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// cancelRequest 是 POST /v1/cancel 的请求体（fire-and-forget，统一 SDD 6.6 协议层形状）。
type cancelRequest struct {
	ClientRequestID string `json:"client_request_id"`
}

// handleCancel 是显式取消端点（POST /v1/cancel）：
// 显式取消优先于延迟窗口等待（统一 SDD 3.7）；查不到 / 已收口 ⇒ 静默忽略（7.5）。
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	var body cancelRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ClientRequestID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid_request",
			"请求体须为 {\"client_request_id\": \"...\"} 且 id 非空")
		return
	}
	hit := s.cancels.cancelByID(body.ClientRequestID)
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": hit})
}

// ---- 管理面端点（只读视图；注册表变更是显式运维动作，见报告偏差说明）----

// handleAdminEndpoints 返回端点注册表全量视图（GET /admin/endpoints）。
// 注册表只存凭据来源引用、⛔ 不存明文 ⇒ 视图可安全输出（统一 SDD 8.1）。
func (s *Server) handleAdminEndpoints(w http.ResponseWriter, _ *http.Request) {
	eps := s.curReg().List()
	out := make([]map[string]any, 0, len(eps))
	for _, ep := range eps {
		out = append(out, map[string]any{
			"name":              ep.Name,
			"base_url":          ep.BaseURL,
			"model":             ep.Model,
			"upstream_model":    ep.UpstreamModel,
			"provider_name":     ep.ProviderName,
			"context_window":    ep.ContextWindow,
			"max_output_tokens": ep.MaxOutputTokens,
			"capabilities": map[string]any{
				"supports_tools":  ep.Capabilities != nil && ep.Capabilities.SupportsTools,
				"supports_stream": ep.Capabilities != nil && ep.Capabilities.SupportsStream,
				"supports_vision": ep.Capabilities != nil && ep.Capabilities.SupportsVision,
			},
			"credential_ref": ep.CredentialRef,
			"timeout_s":      ep.TimeoutS,
			"priority":       ep.Priority,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": out})
}

// handleAdminCredentialPools 返回上游令牌池的配额观测（GET /admin/credential-pools）：
// 多 key 限流「怎么算」的运行面证据 —— 共享桶与逐 key 桶的剩余/桶深/补充速率、冷却状态。
// ⛔ 只含桶状态与槽序，不含任何令牌明文（统一 SDD 8.1）。
func (s *Server) handleAdminCredentialPools(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"pools": s.pools.Snapshot()})
}

// handleAdminHealth 返回管理面健康视图（GET /admin/health）。
func (s *Server) handleAdminHealth(w http.ResponseWriter, _ *http.Request) {
	// 熔断状态（统一 SDD 7.3 可观测要求：⛔ 静默熔断是谜之故障）。
	breakers := make(map[string]string, len(s.breakers))
	for name, b := range s.breakers {
		breakers[name] = b.State()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "ok",
		"endpoints":          len(s.curReg().List()),
		"strategy":           s.curReg().Strategy,
		"cancel_pending":     s.cancels.pendingCount(),
		"usage_missing_rate": s.metrics.UsageMissingRate(),
		"breakers":           breakers,
		"probes":             s.probeSnapshot(),
	})
}

// probeSnapshot 返回巡检结果视图（未启用 ⇒ "disabled"）。
func (s *Server) probeSnapshot() any {
	if s.prober == nil {
		return "disabled"
	}
	return s.prober.Snapshot()
}

// curReg / curRates 返回当前配置快照（热加载原子切换后新请求见新表；
// 单请求内经同一 gatewayCfg 读到配套的注册表与费率表）。
func (s *Server) curReg() *registry.Registry { return s.cfg.Load().reg }
func (s *Server) curRates() *cost.Rates      { return s.cfg.Load().rates }

// ---- 配置热加载（统一 SDD 9.2：先校验后切换、失败保留旧表）----

// ReloadOutcome 是一次热加载的结果（可观测：成功 / 需重启 / 失败三分，
// 对齐业界 reloadable vs restart-required 口径）。
type ReloadOutcome struct {
	OK              bool     `json:"ok"`
	RestartRequired []string `json:"restart_required,omitempty"` // 需重启的变更点（显式列出）
	Error           string   `json:"error,omitempty"`
}

// Reload 热加载注册表与费率表：重读 → 校验 → 原子替换；任何失败 ⇒ 保留旧表。
//
// 可热更：费率表全量；注册表的端点字段（base_url / upstream_model / timeout_s /
// priority / context_window / max_output_tokens / capabilities）与
// fallback_chains —— 这些只被请求路径按快照读取（每请求重做识别与适配），无运行时组件绑定。
//
// ⛔ 需重启（显式拒绝并列出，⛔ 不静默半生效）：端点增删、rate_limit /
// concurrency_limit / limits_group / breaker / credential_rate_limits /
// callers 变更 —— 这些在启动时构建运行时组件（令牌池 / 配额组 / 熔断器 /
// 调用方桶），热换组件会把桶余额、429 冷却、熔断状态一并洗掉或产生半应用态。
func (s *Server) Reload() ReloadOutcome {
	if s.regPath == "" {
		return ReloadOutcome{Error: "server: 未配置注册表路径（RegistryPath），不支持热加载"}
	}
	newReg, err := registry.Load(s.regPath)
	if err != nil {
		return ReloadOutcome{Error: fmt.Sprintf("注册表重载失败（保留旧表）：%v", err)}
	}
	var newRates *cost.Rates
	if s.ratesPath != "" {
		newRates, err = cost.LoadRates(s.ratesPath)
		if err != nil {
			return ReloadOutcome{Error: fmt.Sprintf("费率表重载失败（保留旧表）：%v", err)}
		}
	}
	old := s.cfg.Load()
	if rr := restartRequiredDiffs(old.reg, newReg); len(rr) > 0 {
		return ReloadOutcome{RestartRequired: rr}
	}
	s.cfg.Store(&gatewayCfg{reg: newReg, rates: newRates})
	s.logger.Info("配置热加载完成",
		"endpoints", len(newReg.List()), "rates_loaded", newRates != nil)
	return ReloadOutcome{OK: true}
}

// restartRequiredDiffs 对比新旧注册表，返回需重启的变更点（空 = 可热更）。
// 判定口径见 Reload 注释：运行时组件绑定的配置变更都归需重启。
func restartRequiredDiffs(oldReg, newReg *registry.Registry) []string {
	var rr []string
	oldEPs := map[string]registry.Endpoint{}
	for _, ep := range oldReg.List() {
		oldEPs[ep.Name] = ep
	}
	newEPs := map[string]registry.Endpoint{}
	for _, ep := range newReg.List() {
		newEPs[ep.Name] = ep
	}
	for name := range newEPs {
		if _, ok := oldEPs[name]; !ok {
			rr = append(rr, "新增端点 "+name)
		}
	}
	for name := range oldEPs {
		if _, ok := newEPs[name]; !ok {
			rr = append(rr, "删除端点 "+name)
		}
	}
	for name, nep := range newEPs {
		oep, ok := oldEPs[name]
		if !ok {
			continue
		}
		if !reflect.DeepEqual(oep.RateLimit, nep.RateLimit) {
			rr = append(rr, "端点 "+name+" 的 rate_limit")
		}
		if oep.ConcurrencyLimit != nep.ConcurrencyLimit {
			rr = append(rr, "端点 "+name+" 的 concurrency_limit")
		}
		if oep.LimitsGroup != nep.LimitsGroup {
			rr = append(rr, "端点 "+name+" 的 limits_group")
		}
		if !reflect.DeepEqual(oep.Breaker, nep.Breaker) {
			rr = append(rr, "端点 "+name+" 的 breaker")
		}
	}
	if !reflect.DeepEqual(oldReg.CredentialRateLimits, newReg.CredentialRateLimits) {
		rr = append(rr, "credential_rate_limits")
	}
	if !reflect.DeepEqual(oldReg.Callers, newReg.Callers) {
		rr = append(rr, "callers")
	}
	sort.Strings(rr)
	return rr
}

// handleAdminReload 触发配置热加载（POST /admin/reload，管理面鉴权）。
// 响应体 = ReloadOutcome：ok / restart_required（列出需重启的变更点）/ error。
func (s *Server) handleAdminReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, string(core.ErrInvalidRequest), "method_not_allowed",
			"热加载仅支持 POST")
		return
	}
	out := s.Reload()
	status := http.StatusOK
	if !out.OK {
		status = http.StatusBadRequest
		if len(out.RestartRequired) > 0 {
			status = http.StatusConflict
		}
	}
	writeJSON(w, status, out)
}

// ---- 错误与 JSON 出口 ----

// errorOut 是对外错误响应体（OpenAI 风格 error 对象；归一化分类对外、
// 原始上游信息对内进 span / 日志，统一 SDD 7.1）。
type errorOut struct {
	Error errorBody `json:"error"`
}

// errorBody 是错误对象的对外部分。
type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 写错误响应（状态码 + 归一化分类 + 对外错误码）。
func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, errorOut{Error: errorBody{Message: msg, Type: typ, Code: code}})
}

// writeRetryAfter 写 429 响应并带 Retry-After（统一 SDD 2.3 治理三件套）。
// retryAfter 向上取整到秒（HTTP 头以秒为单位，⛔ 不给 0 误导客户端立即重打）。
func writeRetryAfter(w http.ResponseWriter, retryAfter time.Duration, typ, code, msg string) {
	secs := int64(retryAfter / time.Second)
	if retryAfter%time.Second != 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
	writeError(w, http.StatusTooManyRequests, typ, code, msg)
}

// normalizeHeaderName 归一头名大小写（鉴权头注入对账用）。
func normalizeHeaderName(h string) string { return strings.ToLower(h) }
