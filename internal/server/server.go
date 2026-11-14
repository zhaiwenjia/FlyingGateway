// 日期: 2026-11-14 14:36:39.153
// 作者: Zhai Wenjia

// Package server 是服务装配层（统一 SDD 2.1 ① 入口层）：
// 数据面双入口（/v1/chat/completions、/v1/responses）+ /v1/models + /health + /v1/cancel，
// 管理面（/admin/endpoints、/admin/health）默认关闭、显式开启 + 强制鉴权 + 只绑内网。
// 执行与取消见 execute.go / cancel.go。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/cost"
	"github.com/zhaiwenjia/FlyingGateway/internal/policy"
	"github.com/zhaiwenjia/FlyingGateway/internal/registry"
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

	// ---- 调用方 API key 与令牌桶（与上游自有令牌解耦）----

	// CallerKeys 是调用方 API key 白名单（Authorization: Bearer <key>）。
	// 空 = 开放模式（不校验）；非空 ⇒ /v1/* 强制鉴权，缺失 / 无效 401（统一 SDD 8.1）。
	CallerKeys []string
	// CallerBucketCapacity / CallerBucketRefill 是每调用方令牌桶（token 级限流，8.3）；
	// ≤ 0 = 不启用每调用方限流。
	CallerBucketCapacity int64
	CallerBucketRefill   float64

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

// Server 是网关服务（数据面 + 管理面）。
type Server struct {
	reg     *registry.Registry
	rates   *cost.Rates
	tracer  *telemetry.Tracer
	metrics *telemetry.Metrics

	bucket    *policy.TokenBucket
	budget    *policy.Budget
	guardrail *policy.Guardrail

	callers       *callerAuth
	pools         *poolManager
	maxTokenWait  time.Duration

	allowLoopback bool
	pollInterval  time.Duration
	cancels       *cancelRegistry
	logger        *slog.Logger
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
		reg:           opts.Registry,
		rates:         opts.Rates,
		tracer:        opts.Tracer,
		metrics:       opts.Metrics,
		bucket:        opts.TokenBucket,
		budget:        opts.Budget,
		guardrail:     opts.Guardrail,
		callers:       newCallerAuth(opts.CallerKeys, opts.CallerBucketCapacity, opts.CallerBucketRefill),
		pools:         newPoolManager(opts.UpstreamBucketCapacity, opts.UpstreamBucketRefill, opts.RateLimitCooldown),
		maxTokenWait:  opts.MaxTokenWait,
		allowLoopback: opts.AllowLoopbackEgress,
		pollInterval:  poll,
		logger:        lg,
	}
	if s.maxTokenWait <= 0 {
		s.maxTokenWait = DefaultMaxTokenWait
	}
	installAuthTransport() // 凭据注入点（ctx 键精确到请求，统一 SDD 8.4）
	s.cancels = newCancelRegistry(opts.CancelGracePeriod, opts.CancelGraceByProvider, opts.CancelRetention)
	return s, nil
}

// Handler 返回数据面路由（统一 SDD 2.1 ①）。
// /v1/* 经调用方鉴权（CallerKeys 非空时强制）；/health 保持开放（存活探针）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.callers.wrap(s.handleChatCompletions))
	mux.HandleFunc("POST /v1/responses", s.callers.wrap(s.handleResponses))
	mux.HandleFunc("GET /v1/models", s.callers.wrap(s.handleModels))
	mux.HandleFunc("POST /v1/cancel", s.callers.wrap(s.handleCancel))
	mux.HandleFunc("GET /health", s.handleHealth)
	return mux
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
	return mux, nil
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
	eps := s.reg.List()
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
	eps := s.reg.List()
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
			"max_retries":    ep.MaxRetries,
			"priority":       ep.Priority,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": out})
}

// handleAdminHealth 返回管理面健康视图（GET /admin/health）。
func (s *Server) handleAdminHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "ok",
		"endpoints":          len(s.reg.List()),
		"strategy":           s.reg.Strategy,
		"cancel_pending":     s.cancels.pendingCount(),
		"usage_missing_rate": s.metrics.UsageMissingRate(),
	})
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
