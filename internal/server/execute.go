// 日期: 2026-11-14 14:44:40.449
// 作者: Zhai Wenjia

package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/adapter"
	"github.com/zhaiwenjia/FlyingGateway/internal/codec/chat"
	"github.com/zhaiwenjia/FlyingGateway/internal/codec/messages"
	"github.com/zhaiwenjia/FlyingGateway/internal/codec/responses"
	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/detect"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/registry"
	"github.com/zhaiwenjia/FlyingGateway/internal/routing"
	"github.com/zhaiwenjia/FlyingGateway/internal/security"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/telemetry"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// 本文件承载请求管线（统一 SDD 2.1 数据主线 / 3.4 双入口 / 3.7 恒流式执行 /
// 7.2 治理 / 8.2 出网单一控制点）：解析 → 注册表 → 识别 → 适配器 → 会话态 →
// 上下文判定 → 治理 → 恒流式执行（重试 / 降级分列）→ 流式转发或收包返回。
// 响应装配与收口写入见 respond.go；取消注册表见 cancel.go。

// inboundMode 是入站双模式（统一 SDD 1.2 / 3.4）。
type inboundMode int

const (
	modeChat inboundMode = iota
	modeResponses
	modeMessages
)

const (
	// maxRequestBodyBytes 是入站请求体上限（超出 ⇒ 413）。
	maxRequestBodyBytes = 32 << 20
	// clientRequestIDHeader 是客户端提供的本轮唯一标识（未提供则网关生成，
	// 统一 SDD 6.5 请求合并：client_request_id 贯穿该轮全部尝试）。
	clientRequestIDHeader = "X-Client-Request-Id"
)

// clientRequestIDKey 是 ctx 上的关联号键（鉴权中间件统一生成一次，审计与执行共用
// 同一个 id —— ⛔ 不各生成一个、对不上账）。
type clientRequestIDKey struct{}

// handleChatCompletions 处理 POST /v1/chat/completions。
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handle(w, r, modeChat)
}

// handleResponses 处理 POST /v1/responses。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.handle(w, r, modeResponses)
}

// handleMessages 处理 POST /v1/messages（Anthropic Messages 入口，统一 SDD 3.1 第三入站形）。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handle(w, r, modeMessages)
}

// handle 是双入口共用管线。
func (s *Server) handle(w http.ResponseWriter, r *http.Request, mode inboundMode) {
	startTime := time.Now()

	// 过载闸（load shedding，统一 SDD 7.2 锚点分离）：护网关自身容量，与限流（护套餐配额）
	// 分账 —— 超限 ⇒ 429 + Retry-After（显式背压，⛔ 不排队无界、⛔ 不静默丢弃）。
	if s.maxInFlight > 0 {
		if s.inFlight.Add(1) > s.maxInFlight {
			s.inFlight.Add(-1)
			writeRetryAfterWait(w, time.Second, string(core.ErrRateLimit), "overloaded",
				fmt.Sprintf("网关过载：在途请求已达上限 %d（max-in-flight），请稍后重试", s.maxInFlight),
				"overloaded")
			return
		}
		defer s.inFlight.Add(-1)
	}

	// 1. 请求体（上限保护）。
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, string(core.ErrInvalidRequest), "invalid_request", "读取请求体失败: "+security.Redact(err.Error()))
		return
	}
	if len(body) > maxRequestBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, string(core.ErrInvalidRequest), "request_too_large",
			fmt.Sprintf("请求体超过 %d 字节上限", maxRequestBodyBytes))
		return
	}

	// 2. 入站解析（解析失败 ⇒ 400，⛔ 不发起上游请求）。
	var req *core.Request
	switch mode {
	case modeChat:
		req, err = chat.DecodeRequest(body)
	case modeMessages:
		req, err = messages.DecodeRequest(body)
	default:
		req, err = responses.DecodeRequest(body)
	}
	if err != nil {
		var de *adapter.DetailError
		if errors.As(err, &de) && de.Detail != nil {
			s.writeDetail(w, de.Detail)
			return
		}
		writeError(w, http.StatusBadRequest, string(core.ErrInvalidRequest), "invalid_request", "请求体解析失败: "+security.Redact(err.Error()))
		return
	}

	// 3. 取消注册表登记（同 id 在途 / 取消终态 ⇒ 409 拒绝后续尝试，统一 SDD 7.5 / SC-5）。
	clientID := clientRequestID(r)
	h, err := s.cancels.register(clientID)
	if err != nil {
		code := "request_in_flight"
		if errors.Is(err, errRequestCancelled) {
			code = "request_cancelled"
		}
		writeError(w, http.StatusConflict, string(core.ErrInvalidRequest), code, security.Redact(err.Error()))
		return
	}
	// 请求级 finally（清理第二道闸）：注册表收口、定时器清理、记账（与帧循环 finally 独立）。
	caller := callerIdentityOf(r)
	defer s.finalizeExec(h, req, caller, startTime)

	// 4. 模型必须在注册表列表内（⛔ 不透传任意模型名，统一 SDD 9.1）。
	ep, err := s.curReg().Lookup(req.Model)
	if err != nil {
		s.writeDetail(w, gwerrors.New(core.ErrInvalidRequest, http.StatusNotFound, "model_not_found", security.Redact(err.Error())))
		return
	}

	// 入口协议与模型族绑定（Claude 模型全程走 Anthropic Messages，其它供应商走 OpenAI 形）：
	// 调用面（自研 agent runtime）按模型族选择 SDK，协议错配是调用方 bug ⇒ 显式 400，
	// ⛔ 不静默跨形状翻译（跨形状的语义损失本可避免 —— 让调用方先纠正用法）。
	// 判定取主端点（Lookup 结果）的 provider_name；降级链内部换供应商不受此限。
	if isMsg := mode == modeMessages; isMsg != (ep.ProviderName == "anthropic") {
		msg := fmt.Sprintf("Claude 模型 %q 只接受 /v1/messages 入口（Anthropic 形）", req.Model)
		if isMsg {
			msg = fmt.Sprintf("模型 %q（provider %s）须走 OpenAI 形入口（/v1/chat/completions 或 /v1/responses）；/v1/messages 仅接受 anthropic 模型",
				req.Model, ep.ProviderName)
		}
		s.writeDetail(w, gwerrors.New(core.ErrInvalidRequest, http.StatusBadRequest, "ingress_protocol_mismatch", msg))
		return
	}

	// 5. 识别（注册表的 base_url + upstream_model；⛔ 调用方不带 base_url / 不指定 provider）。
	res, err := detect.Detect(ep.BaseURL, ep.UpstreamModel)
	if err != nil {
		s.writeDetail(w, gwerrors.New(core.ErrInvalidRequest, http.StatusBadRequest, "detect_failed", security.Redact(err.Error())))
		return
	}
	res.CredentialRef = ep.CredentialRef

	// 6. 适配器（形态按 core.Resolution.Shape；未登记组合 ⇒ provider 默认形态，
	// detect 的无 path 信号 OpenAIChat 是 OpenAI 兼容占位、非出站指令，统一 SDD 3.6）。
	ad, err := adapterFor(res)
	if err != nil {
		s.writeDetail(w, adapterDetail(err))
		return
	}
	res.SupportsPreviousResponseID = ad.SupportsPreviousResponseID()

	// 7. 会话态（统一 SDD 3.5）：previous_response_id 是语义字段 ⇒ 上游不支持显式报错。
	if req.PreviousResponseID != "" && !res.SupportsPreviousResponseID {
		s.writeDetail(w, gwerrors.New(core.ErrInvalidRequest, http.StatusBadRequest, "unsupported_capability",
			"该上游不支持服务端会话，请改为客户端带全量历史"))
		return
	}
	// prompt_cache_key 是优化字段 ⇒ 始终透传（不支持的上游由适配器静默忽略，3.5）。

	// 8. 上下文越界判定（统一 SDD 2.3：⛔ 不静默截断）。
	estInput := estimateInputTokens(req)
	outCap := req.MaxOutputTokens
	if outCap <= 0 {
		outCap = ep.MaxOutputTokens
	}
	if err := routing.CheckContext(estInput, outCap, ep.ContextWindow); err != nil {
		s.handleContextExceeded(w, r, h, req, mode, ep, &res, ad, err)
		return
	}

	// 9. 治理三件套（上游请求之前，统一 SDD 2.3 / 7.2 / 8.3）。
	if !s.checkGovernance(w, r, h, req, ep, estInput, outCap) {
		return // checkGovernance 已写响应
	}

	// 10. 断连检测：watcher + 轮询双路（统一 SDD 6.6 要求 1；非流式路径同样自挂，SC-7）。
	stopWatch := s.watchClient(r, h)
	defer stopWatch()

	// 11. 执行（恒流式：stream 参数只决定客户端侧返回形态，统一 SDD 3.7）。
	s.runAttempts(w, r, h, req, mode, ep, &res, ad, int64(estInput+outCap))
}

// clientRequestID 取本轮唯一标识：ctx（鉴权中间件统一生成）优先，其次客户端头，
// 都没有则生成（统一 SDD 6.5）。
func clientRequestID(r *http.Request) string {
	if id, ok := r.Context().Value(clientRequestIDKey{}).(string); ok && id != "" {
		return id
	}
	if id := r.Header.Get(clientRequestIDHeader); id != "" {
		return id
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("gw-%x", b[:])
	}
	return fmt.Sprintf("gw-%d", time.Now().UnixNano())
}

// estimateInputTokens 是请求输入的保守 token 上界估算（统一 SDD 2.3「估算不确定 ⇒
// 判定用保守上界，宁可早报错，不可悄悄截断」）：把指令 / 消息文本 / 工具 JSON Schema 的
// UTF-8 字节数折半向上取整 —— 英文约 4 字节/token ⇒ 估为约 2 倍实际；中日韩约 2–3 字节/token
// ⇒ 估为 ≥1.5 倍实际。两条语言路径都高估，⛔ 不用 bytes/4 这类低估口径。
func estimateInputTokens(req *core.Request) int {
	var n int
	add := func(s string) { n += len(s) }
	add(req.Instructions)
	for _, m := range req.Messages {
		add(m.Role)
		add(m.ReasoningContent)
		add(m.ThinkingSignature)
		add(m.EncryptedContent)
		add(m.Name)
		add(m.ToolCallID)
		for _, p := range m.Content {
			add(p.Type)
			add(p.Text)
			add(p.ImageURL)
			add(p.MIMEType)
			add(p.Data)
		}
		for _, tc := range m.ToolCalls {
			add(tc.ID)
			add(tc.CallID)
			add(tc.Name)
			add(tc.Arguments)
		}
	}
	for _, t := range req.Tools {
		add(t.Name)
		add(t.Description)
		n += len(t.Parameters)
	}
	for _, s := range req.Stop {
		add(s)
	}
	if n == 0 {
		return 0
	}
	return (n + 1) / 2
}

// adapterFor 按识别结果选适配器：先按 Resolution.Shape（base_url 带端点路径信号时权威），
// 未登记组合（detect 的 OpenAI 兼容占位等）⇒ provider 默认形态（统一 SDD 3.6 优先级链）。
func adapterFor(res core.Resolution) (adapter.Adapter, error) {
	ad, err := adapter.ForShape(res.Provider, res.Shape)
	if err == nil {
		return ad, nil
	}
	ad2, err2 := adapter.For(res.Provider)
	if err2 != nil {
		return nil, err // 两个都失败时回原始错误（含 unsupported_provider / unsupported_shape 明细）
	}
	return ad2, nil
}

// adapterDetail 把适配器错误转成 ErrorDetail（含非 DetailError 的兜底）。
// ⚠️ 2026-11-14 真实压测实测修复：兜底原先一律归 `invalid_request`（400 adapter_error）——
// 现象：上游单请求卡满 60s（context deadline exceeded）对外吐 400，且因
// `invalid_request` 不在可重试集（rate_limit/timeout/upstream_error）里，**重试被静默绕过**，
// 该档成功率掉到 63/64；根因：传输类错误走了「请求不合法」的分类，可重试判定跟着错；
// 为什么不这么修：可重试集/降级链都按分类分流（7.4），在重试处打补丁会掩盖分类错误；
// 不修会怎样：任何上游尾部卡顿都直接穿透成客户端失败，成功率无法保证 100%。
// 现按传输语义分类（7.1）：超时 ⇒ timeout（可重试）、取消 ⇒ cancelled（⛔ 不重试，7.5）、
// 其余非 DetailError（连接失败 / 上游坏 JSON 等）⇒ upstream_error（可重试）。
func adapterDetail(err error) *core.ErrorDetail {
	var de *adapter.DetailError
	if errors.As(err, &de) && de.Detail != nil {
		return de.Detail
	}
	return gwerrors.FromTransportError(core.ProviderUnknown, err, "")
}

// ---- 凭据注入（统一 SDD 8.1 / 8.4）----

// authHeadersKey 是 ctx 上的上游鉴权载荷键（凭据随请求上下文走、请求结束即释放，
// ⛔ 不在函数间层层传递明文，统一 SDD 8.1）。
type authHeadersKey struct{}

// upstreamAuth 是一次出站请求的鉴权载荷：注入头 + 上游 429 冷却回调（令牌轮换信号）。
type upstreamAuth struct {
	headers map[string]string
	// onRateLimited 在上游回 429 时被调（Retry-After 已解析；0 = 上游未给）。
	onRateLimited func(retryAfter time.Duration)
}

// authTransport 是出站请求的鉴权头注入点：凭据值（网关自有令牌，⛔ 与调用方 key 解耦）
// 按 adapter.AuthScheme 构造、经 ctx 键携带，⛔ 不进 adapter.Outbound（Outbound 只含
// 非凭据头，可安全入日志）。无 ctx 键的请求（如 guardrail webhook）原样放行。
// 同一传输链顺带观测上游 429 的 Retry-After（令牌池冷却信号）。
type authTransport struct{ base http.RoundTripper }

// RoundTrip 实现 http.RoundTripper。
func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	payload, _ := r.Context().Value(authHeadersKey{}).(*upstreamAuth)
	if payload != nil {
		for k, v := range payload.headers {
			r.Header.Set(k, v)
		}
	}
	resp, err := t.base.RoundTrip(r)
	if err == nil && resp != nil && payload != nil && payload.onRateLimited != nil && resp.StatusCode == http.StatusTooManyRequests {
		payload.onRateLimited(parseRetryAfter(resp.Header.Get("Retry-After")))
	}
	return resp, err
}

// parseRetryAfter 解析 Retry-After 头（delta-seconds 或 HTTP-date；缺失 / 非法 ⇒ 0
// —— 由调用方取默认冷却，⛔ 不猜）。
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

var installOnce sync.Once

// installAuthTransport 把鉴权注入包装进 http.DefaultClient 的传输链（幂等）。
// adapter.For/ForShape 的出站客户端即 http.DefaultClient（adapter 构造函数只留
// 测试注入缝）⇒ 这是不经 adapter 改动、凭据又不进 Outbound 的唯一注入通道；
// 注入按 ctx 键精确到请求，不影响其它使用 http.DefaultClient 的调用。
func installAuthTransport() {
	installOnce.Do(func() {
		base := http.DefaultClient.Transport
		if base == nil {
			// ⚠️ 2026-11-14 压测 c=8 档瓶颈修复：http.DefaultTransport 的
			// MaxIdleConnsPerHost=2（Go 默认）——现象：c=8 档吞吐/延迟差于对照组
			//（3 轮中位 2.80 vs 3.16 rps、P50 2017 vs 1683ms），c=1/c=32 反而正常；
			// 根因：中等并发下连接用完即关（空闲池每 host 只留 2 条），下一波请求
			// 全量 TLS 重连，握手开销专打中等并发（c=1 无争用、c=32 全忙不回池）；
			// 为什么不改别处：池参数是传输层单点，改一处全体出站受益（SDK 客户端
			// 注入的是同一个 *http.Client）；不修会怎样：并发吞吐随空闲池上限折损，
			// 表现为「网关中并发不如直连」，且随上游 RTT 放大。
			// 保活池按最大并发档（32）留足：每 host 空闲 100、总 200、超时 90s。
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.MaxIdleConns = 200
			tr.MaxIdleConnsPerHost = 100
			tr.IdleConnTimeout = 90 * time.Second
			base = tr
		}
		http.DefaultClient.Transport = &authTransport{base: base}
	})
}

// buildUpstreamAuth 从自有令牌池选令牌并按 adapter.AuthScheme 构造鉴权头
// （三种形态：Authorization+Prefix / api-key / x-api-key + ExtraHeaders，统一 SDD 8.4）。
// 全部令牌受限时取等待时间最短的令牌：等待 ≤ maxWait ⇒ 等满重选；超上限 ⇒
// 显式 429（code=token_pool_exhausted，⛔ 不盲发、⛔ 不改投其他端点），
// 此时返回的 hint 即最短等待（供 Retry-After 头）。
func buildUpstreamAuth(pools *poolManager, ad adapter.Adapter, credentialRef string, est int64, maxWait time.Duration) (*upstreamAuth, time.Duration, *core.ErrorDetail) {
	pool, err := pools.PoolFor(credentialRef)
	if err != nil {
		return nil, 0, gwerrors.New(core.ErrAuth, http.StatusInternalServerError, "credential_unresolved", security.Redact(err.Error()))
	}
	now := time.Now()
	pick := pool.Pick(est, now)
	if !pick.OK {
		if maxWait <= 0 {
			maxWait = DefaultMaxTokenWait
		}
		if pick.Wait > maxWait {
			detail := gwerrors.New(core.ErrRateLimit, http.StatusTooManyRequests, codeTokenPoolExhausted,
				fmt.Sprintf("全部 %d 个自有令牌受限，最短等待 %v 超过上限 %v", pool.Size(), pick.Wait, maxWait))
			return nil, pick.Wait, detail
		}
		time.Sleep(pick.Wait)
		pick = pool.Pick(est, time.Now())
		if !pick.OK {
			detail := gwerrors.New(core.ErrRateLimit, http.StatusTooManyRequests, codeTokenPoolExhausted,
				fmt.Sprintf("全部 %d 个自有令牌受限（最短等待 %v 后仍未恢复）", pool.Size(), pick.Wait))
			return nil, pick.Wait, detail
		}
	}
	scheme := ad.AuthScheme()
	if scheme.HeaderName == "" {
		return nil, 0, gwerrors.New(core.ErrAuth, http.StatusInternalServerError, "auth_scheme_missing",
			fmt.Sprintf("适配器 %s 未声明鉴权头名", ad.Provider()))
	}
	hs := make(map[string]string, 1+len(scheme.ExtraHeaders))
	hs[scheme.HeaderName] = scheme.Prefix + pick.Token
	for k, v := range scheme.ExtraHeaders {
		hs[k] = v
	}
	slot := pick.Slot
	payload := &upstreamAuth{
		headers: hs,
		onRateLimited: func(retryAfter time.Duration) {
			if retryAfter <= 0 {
				retryAfter = pools.defaultCooldownFor()
			}
			pool.MarkRateLimited(slot, retryAfter)
		},
	}
	return payload, 0, nil
}

// withUpstreamAuth 把鉴权载荷挂到执行上下文（authTransport 在出网时注入 + 观测 429）。
func withUpstreamAuth(ctx context.Context, payload *upstreamAuth) context.Context {
	return context.WithValue(ctx, authHeadersKey{}, payload)
}

// ---- 治理（统一 SDD 2.3 治理三件套；均在上游请求之前）----

// checkGovernance 依次过 scope 三闸（模型集 / 端点集 / token 预算，R-43 / AC-36）/
// 限流（全局 + 调用方契约配额）/ 预算 / 护栏；拒绝时写响应并返回 false。
// 计数口径：归一化 token 的保守预估（估算输入 + 输出上限），⛔ 不只按请求数（8.3）。
// scope 先于限流（⛔ 越权请求先烧配额）；契约配额与预算按**调用方身份**累计
// （调用方 key 与上游令牌解耦后的计费口径）。
func (s *Server) checkGovernance(w http.ResponseWriter, r *http.Request, h *execHandle, req *core.Request, ep *registry.Endpoint, estInput, outCap int) bool {
	est := int64(estInput + outCap)
	caller := callerIdentityOf(r)

	// scope 三闸（AC-36 类三）：超 scope ⇒ 403 scope_exceeded + 审计（⛔ 拒绝静默）；
	// 模型 / 端点 / 预算三闸合并一条判定留痕。
	if ce := callerEntryOf(r); ce != nil {
		if reason := ce.scopeCheck(req.Model, ep.Name, est); reason != "" {
			s.callers.audit(caller, req.Model+"@"+ep.Name, "deny", reason, clientRequestID(r))
			s.writeDetail(w, gwerrors.New(core.ErrAuth, http.StatusForbidden, "scope_exceeded", reason))
			return false
		}
		s.callers.audit(caller, req.Model+"@"+ep.Name, "allow", "in scope", clientRequestID(r))
	}

	if s.bucket != nil {
		if ok, retryAfter := s.bucket.Allow(est); !ok {
			writeRetryAfterWait(w, retryAfter, string(core.ErrRateLimit), "rate_limit",
				fmt.Sprintf("token 级限流超限（预估 %d 归一化 token），请稍后重试", est),
				"rate_limited")
			return false
		}
	}
	// 调用方契约配额（registry callers.rate_limit）：省略 ⇒ 不限流（⛔ 不套默认值）。
	if ce := callerEntryOf(r); ce != nil && ce.rl != nil {
		if ok, retryAfter := ce.rl.Allow(est); !ok {
			writeRetryAfterWait(w, retryAfter, string(core.ErrRateLimit), "rate_limit",
				fmt.Sprintf("调用方契约配额超限（预估 %d 归一化 token），请稍后重试", est),
				"rate_limited")
			return false
		}
	}
	if s.budget != nil {
		if err := s.budget.Allow(caller, est); err != nil {
			// 触顶即拒（⛔ 上游调用数 0，统一 SDD 7.2 / 8.3）。
			s.writeDetail(w, gwerrors.New(core.ErrBudgetExceeded, http.StatusTooManyRequests, "budget_exceeded", security.Redact(err.Error())))
			return false
		}
	}
	if s.guardrail != nil {
		if err := s.guardrail.Check(r.Context(), guardrailText(req)); err != nil {
			s.writeDetail(w, gwerrors.New(core.ErrContentPolicy, http.StatusForbidden, "content_policy", security.Redact(err.Error())))
			return false
		}
	}
	return true
}

// guardrailText 抽取请求文本供护栏校验（统一 SDD 2.3：正则 + Webhook 在上游请求之前）。
func guardrailText(req *core.Request) string {
	var b strings.Builder
	b.WriteString(req.Instructions)
	for _, m := range req.Messages {
		b.WriteString(m.ReasoningContent)
		for _, p := range m.Content {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ---- 断连检测：watcher + 轮询双路（统一 SDD 6.6 要求 1）----

// watchClient 自挂断连检测（⛔ 不依赖框架默认行为，SC-7）：
//   - watcher：监听请求上下文取消事件（客户端断连 ⇒ net/http 取消 r.Context）；
//   - 轮询：按 DisconnectPollInterval 兜底复查（watcher 调度被饿死时仍有界触发）；
//   - 写失败路径在转发循环里单独触发（第三信号）。
//
// 检测延迟上界 = 轮询周期 × 4（默认周期 250ms ⇒ 默认上界 1s，统一 SDD 6.6）。
func (s *Server) watchClient(r *http.Request, h *execHandle) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	stop = func() { once.Do(func() { close(done) }) }

	// 路 1：watcher（事件驱动）。
	go func() {
		select {
		case <-r.Context().Done():
			h.trigger(reasonClientDisconnect, false)
		case <-done:
		}
	}()
	// 路 2：轮询兜底（有界延迟）。
	go func() {
		t := time.NewTicker(s.pollInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if r.Context().Err() != nil {
					h.trigger(reasonClientDisconnect, false)
					return
				}
			case <-done:
				return
			}
		}
	}()
	return stop
}

// ---- 上下文越界（统一 SDD 2.3 / 7.4）----

// handleContextExceeded 处理上下文越界：走 context_window 专用降级链；
// 无链 / 链内无可用大窗口端点 ⇒ 显式报 400 context_exceeded（⛔ 不静默截断）。
func (s *Server) handleContextExceeded(w http.ResponseWriter, r *http.Request, h *execHandle, req *core.Request, mode inboundMode, ep *registry.Endpoint, res *core.Resolution, ad adapter.Adapter, ctxErr error) {
	chain, err := routing.SelectChain(s.curReg().FallbackChains, ep.Name, routing.FailContextWindow)
	if err != nil {
		s.writeDetail(w, gwerrors.New(core.ErrContextExceeded, http.StatusBadRequest, "context_exceeded", security.Redact(ctxErr.Error())))
		return
	}
	chain = scopeFilterChain(r, chain) // 降级不绕过 scope（R-43）
	estInput := estimateInputTokens(req)
	for _, name := range chain {
		next := s.endpointByName(name)
		if next == nil {
			continue
		}
		outCap := req.MaxOutputTokens
		if outCap <= 0 {
			outCap = next.MaxOutputTokens
		}
		if err := routing.CheckContext(estInput, outCap, next.ContextWindow); err != nil {
			continue
		}
		nres, derr := detect.Detect(next.BaseURL, next.UpstreamModel)
		if derr != nil {
			continue
		}
		nres.CredentialRef = next.CredentialRef
		nad, aerr := adapterFor(nres)
		if aerr != nil {
			continue
		}
		nres.SupportsPreviousResponseID = nad.SupportsPreviousResponseID()
		*res = nres
		// 降级目标作为本轮主执行（上下文专用链，独立于通用降级链）。
		s.runAttempts(w, r, h, req, mode, next, res, nad, int64(estInput+outCap))
		return
	}
	s.writeDetail(w, gwerrors.New(core.ErrContextExceeded, http.StatusBadRequest, "context_exceeded", security.Redact(ctxErr.Error())))
}

// ---- 执行（恒流式 + 重试 / 降级分列，统一 SDD 3.7 / 7.4 / 7.5）----

// runAttempts 执行上游调用。尝试族两类分开、⛔ 不合成兜底循环（统一 SDD 7.4 + 令牌轮换）：
//   - 令牌轮换（同端点换自有令牌）：上游 429 且池内还有未冷却令牌 ⇒ 换令牌重打；
//     轮换上界 = 池大小（每令牌每轮至多一次）；
//   - 降级（换端点，上限 = 降级链长度，routing.SelectChain 显式分链）。
//
// ⛔ 同端点重试已移除：gateway 不做模型调用重试，重试由 harness 系统控制 ——
// 失败原样返回（HTTP 状态码 + Retry-After 头），由 harness 决定是否重发。
//
// 取消 ⇒ 立即收口，⛔ 不触发任何一族（7.5，SC-5）；轮换 / 降级只在
// 「尚未向客户端提交」时进行（流式转发后重放会向客户端发重复内容）。
// span 按端点持有：令牌轮换合并进同一 span，降级换端点才另起（统一 SDD 6.2）。
func (s *Server) runAttempts(w http.ResponseWriter, r *http.Request, h *execHandle, req *core.Request, mode inboundMode, ep *registry.Endpoint, res *core.Resolution, ad adapter.Adapter, est int64) {
	var counters routing.AttemptCounters
	var rotations int
	brk := s.breakerFor(ep.Name)
	span := s.startEndpointSpan(ep, res, req, ad)
	for {
		if h.isCancelled() {
			s.endEndpointSpan(span, h, nil, gwerrors.New(core.ErrCancelled, 499, "cancelled", "执行已取消"))
			s.writeOutcome(w, r, h, req, mode, nil, nil, true)
			return
		}
		// 熔断闸（统一 SDD 7.3）：OPEN 冷却中 / HALF-OPEN 探测满 ⇒ 跳过该端点（⛔ 不打网络），
		// 按 upstream_error 走降级链顺延；放行的尝试按「成功 / 可归因失败 / 非可归因」
		// 三分类收口记入熔断器（429 / 取消 / 内容策略等不算端点故障）。
		now := time.Now()
		var resp *core.Response
		var degs []core.Degradation
		var detail *core.ErrorDetail
		if brk.Allow(now) {
			resp, degs, detail = s.runEndpoint(w, r, h, req, mode, ep, res, ad, est)
			switch {
			case detail == nil:
				brk.RecordSuccess(time.Now())
			case detail.Category == core.ErrUpstreamError || detail.Category == core.ErrTimeout:
				brk.RecordFailure(time.Now())
			default:
				brk.RecordNeutral(time.Now())
			}
		} else {
			detail = gwerrors.New(core.ErrUpstreamError, http.StatusBadGateway, codeEndpointCooling,
				fmt.Sprintf("端点 %q 熔断冷却中，跳过本次尝试转降级链（状态见 /admin/health）", ep.Name))
		}
		if detail == nil {
			s.endEndpointSpan(span, h, resp, nil)
			s.writeOutcome(w, r, h, req, mode, resp, degs, false)
			return
		}
		if h.isCancelled() || detail.Category == core.ErrCancelled {
			// 取消收口（⛔ 不重试、⛔ 不降级、⛔ 不轮换，SC-5）。
			s.endEndpointSpan(span, h, resp, detail)
			s.writeOutcome(w, r, h, req, mode, resp, degs, true)
			return
		}
		// 已向客户端提交流（写了响应头 / 分片）⇒ 不能重放；错误已在响应侧收口。
		if h.committed() {
			s.endEndpointSpan(span, h, resp, detail)
			return
		}
		// 熔断跳过的尝试 ⛔ 不轮换（端点在冷却中，同端点重打没有意义），直接走降级。
		// 令牌轮换（同端点换自有令牌）：上游 429 且池内仍有未冷却令牌 ⇒ 直接换令牌重打；
		// token_pool_exhausted 是本地池闸（无令牌可换）⇒ 不轮换。
		// ⛔ 同端点重试已移除（gateway 不重试，模型调用重试由 harness 系统控制）：
		// 失败原样进降级 / 最终错误，HTTP 状态码与 Retry-After 头供 harness 决策重试。
		if detail.Code != codeEndpointCooling &&
			detail.Category == core.ErrRateLimit && detail.Code != codeTokenPoolExhausted &&
			rotations < s.poolSize(ep.CredentialRef) && s.poolHasUncool(ep.CredentialRef) {
			rotations++
			continue
		}
		kind := failureKind(detail.Category)
		// 降级换端点（级间）：按失败类型分链；无链 ⇒ 显式失败（⛔ 不静默回落通用链）。
		chain, err := routing.SelectChain(s.curReg().FallbackChains, ep.Name, kind)
		if err != nil || counters.FallbackCount >= len(chain) {
			s.endEndpointSpan(span, h, resp, detail)
			s.writeFinalError(w, h, detail)
			return
		}
		chain = scopeFilterChain(r, chain) // 降级不绕过 scope（R-43）
		if counters.FallbackCount >= len(chain) {
			s.endEndpointSpan(span, h, resp, detail)
			s.writeFinalError(w, h, detail)
			return
		}
		next := s.endpointByName(chain[counters.FallbackCount])
		if next == nil {
			s.endEndpointSpan(span, h, resp, detail)
			s.writeFinalError(w, h, detail)
			return
		}
		// 降级目标重新识别 + 适配（端点不同 ⇒ base_url / upstream_model 可能不同）。
		nres, derr := detect.Detect(next.BaseURL, next.UpstreamModel)
		if derr != nil {
			s.endEndpointSpan(span, h, resp, detail)
			s.writeFinalError(w, h, detail)
			return
		}
		nres.CredentialRef = next.CredentialRef
		nad, aerr := adapterFor(nres)
		if aerr != nil {
			s.endEndpointSpan(span, h, resp, detail)
			s.writeFinalError(w, h, detail)
			return
		}
		nres.SupportsPreviousResponseID = nad.SupportsPreviousResponseID()
		counters.RecordFallback()
		s.endEndpointSpan(span, h, resp, detail) // 旧端点 span 收口
		ep = next
		*res = nres
		ad = nad
		rotations = 0 // 新端点的令牌池独立轮换
		brk = s.breakerFor(ep.Name)
		span = s.startEndpointSpan(ep, res, req, ad)
	}
}

// codeTokenPoolExhausted 是「全部自有令牌受限」的对外错误码（本地池闸、非上游回执）。
const codeTokenPoolExhausted = "token_pool_exhausted"

// codeEndpointCooling 是「端点熔断冷却中被跳过」的对外错误码（本地熔断闸、非上游回执）。
// 分类为 upstream_error（502）⇒ 按该类走降级链顺延；⛔ 不重试不轮换（端点在冷却中）。
const codeEndpointCooling = "endpoint_cooling"

// breakerFor 返回端点的熔断器（注册表每端点一个，New 时构造）。
// 防御：未知端点名 ⇒ 现造一个默认熔断器（⛔ 不返回 nil 让调用方炸）。
func (s *Server) breakerFor(name string) *EndpointBreaker {
	if b, ok := s.breakers[name]; ok {
		return b
	}
	return NewEndpointBreaker(name, 0, 0, 0, s.logger)
}

// poolSize 返回端点凭据池的令牌数（轮换上界；解析失败 ⇒ 0）。
func (s *Server) poolSize(credentialRef string) int {
	if s.pools == nil {
		return 0
	}
	p, err := s.pools.PoolFor(credentialRef)
	if err != nil {
		return 0
	}
	return p.Size()
}

// poolHasUncool 返回池内是否仍有未冷却令牌（轮换前提；⛔ 不扣减桶）。
func (s *Server) poolHasUncool(credentialRef string) bool {
	if s.pools == nil {
		return false
	}
	p, err := s.pools.PoolFor(credentialRef)
	if err != nil {
		return false
	}
	return p.HasAvailable(time.Now())
}

// endpointByName 按端点名查注册表（降级链引用的是端点名，非模型别名）。
func (s *Server) endpointByName(name string) *registry.Endpoint {
	for _, ep := range s.curReg().List() {
		if ep.Name == name {
			e := ep
			return &e
		}
	}
	return nil
}

// limitGroupFor 取端点所属的配额共享组（limits_group 缺省 = 端点名自成组）；
// 无任何配额登记的组不建组 ⇒ nil（不限流）。
func (s *Server) limitGroupFor(ep *registry.Endpoint) *limitGroup {
	key := ep.LimitsGroup
	if key == "" {
		key = ep.Name
	}
	return s.limitGroups[key]
}

// failureKind 把错误分类映射到降级链失败类型（统一 SDD 7.4 七类）。
func failureKind(cat core.ErrorCategory) routing.FailureKind {
	switch cat {
	case core.ErrContextExceeded:
		return routing.FailContextWindow
	case core.ErrContentPolicy:
		return routing.FailContentPolicy
	case core.ErrBudgetExceeded:
		return routing.FailBudget
	case core.ErrRateLimit:
		return routing.FailRateLimit
	case core.ErrTimeout:
		return routing.FailTimeout
	case core.ErrUpstreamError:
		return routing.FailUpstreamError
	default:
		return routing.FailGeneral
	}
}

// runEndpoint 执行一次上游调用（⛔ 不自建 span —— span 由 runAttempts 按端点持有，
// 自动重试 / 令牌轮换合并进同一 span，统一 SDD 6.2）：
// 网关→上游恒流式（StreamStart）；stream=true 转发帧给客户端、stream=false 收完帧装配完整响应。
// 返回装配响应（失败 / 取消时为已收部分）+ 降级标记 + 终态错误。
func (s *Server) runEndpoint(w http.ResponseWriter, r *http.Request, h *execHandle, req *core.Request, mode inboundMode, ep *registry.Endpoint, res *core.Resolution, ad adapter.Adapter, est int64) (*core.Response, []core.Degradation, *core.ErrorDetail) {
	// 出站构造（degradation 收集 + 出网 URL 单一控制点，统一 SDD 8.2）。
	var degs []core.Degradation
	out, err := ad.BuildRequest(r.Context(), req, res)
	if err == nil {
		if da, ok := ad.(adapter.DegradationAware); ok {
			_, degs, err = da.BuildRequestWithDegradations(r.Context(), req, res)
		}
	}
	if err != nil {
		detail := adapterDetail(err)
		s.observeExec(ep, req, nil, "failed")
		return nil, degs, detail
	}
	if err := security.ValidateEgressURL(out.URL, s.allowLoopback); err != nil {
		detail := gwerrors.New(core.ErrInvalidRequest, http.StatusForbidden, "egress_denied", security.Redact(err.Error()))
		s.observeExec(ep, req, nil, "failed")
		return nil, degs, detail
	}

	// 端点配额共享组两道闸（统一 SDD 7.2 锚点分离）：在途并发位 + 契约桶（token/rpm 双维），
	// 任一不足 ⇒ 429 + Retry-After 进重试/降级决策（⛔ 不静默换端点）。并发位占到本尝试
	// 收口才释放（流式长调用占位到流收口 —— DeepSeek 类「只限并发」的供应商按此计量），
	// release 由 defer 保证恰好一次。同组端点共享同一份计数（供应商配额池语义）。
	if g := s.limitGroupFor(ep); g != nil {
		ok, wait, concurrency := g.acquire(est)
		if !ok {
			h.setRetryAfterHint(wait)
			h.setWaitPhase("waiting_for_concurrency") // 等待信号 F1（并发位满）
			gate := "契约桶不足"
			if concurrency {
				gate = fmt.Sprintf("在途并发位已满（上限 %d）", g.maxInFlight)
			}
			detail := gwerrors.New(core.ErrRateLimit, http.StatusTooManyRequests, "rate_limit",
				fmt.Sprintf("端点 %q 配额超限：%s（预估 %d 归一化 token），请稍后重试", ep.Name, gate, est))
			s.observeExec(ep, req, nil, resultForDetail(detail))
			return nil, degs, detail
		}
		defer g.release()
	}

	// 调用方在途并发位（⭐ 2026-11-26 老宅架构定界：并发/配额/流量归 FG，
	// 上游框架认证/装配后直发 —— 本闸按**调用方身份**限并发，单个调用方
	// 打满不挤占其他调用方；0 = 不限（callerEntry.tryAcquire 直接放行）。
	// 拒绝给 429 + Retry-After 进重试/降级决策（⛔ 不静默排队）；release
	// 与端点并发位同款 defer 恰好一次（本尝试收口才释放 —— 流式长调用
	// 占位到流收口）。
	if ce := callerEntryOf(r); ce != nil && !ce.tryAcquire() {
		h.setRetryAfterHint(time.Second)
		h.setWaitPhase("waiting_for_concurrency") // 等待信号 F1
		detail := gwerrors.New(core.ErrRateLimit, http.StatusTooManyRequests, "rate_limit",
			fmt.Sprintf("调用方 %q 在途并发位已满（上限 %d），请稍后重试", ce.name, ce.maxInFlight))
		s.observeExec(ep, req, nil, resultForDetail(detail))
		return nil, degs, detail
	} else if ce != nil {
		defer ce.release()
	}

	// 自有令牌选择 + 凭据注入（8.4 三种形态按 provider 分派；⛔ 不透传调用方凭据 ——
	// 调用方 API key 与自有令牌解耦，上游头永远是池内令牌）。
	payload, waitHint, authDetail := buildUpstreamAuth(s.pools, ad, res.CredentialRef, est, s.maxTokenWait)
	if authDetail != nil {
		h.setRetryAfterHint(waitHint) // token_pool_exhausted 的 Retry-After = 最短等待
		s.observeExec(ep, req, nil, resultForDetail(authDetail))
		return nil, degs, authDetail
	}

	// 每次上游调用一个超时预算（Endpoint.TimeoutS）；执行上下文挂在取消句柄上
	// （客户端断连 / 显式取消 / 超时 / 进程关闭四路都汇到 h.ctx 的取消上）。
	attemptCtx, cancelAttempt := context.WithTimeout(withUpstreamAuth(h.ctx, payload), secondsToDuration(ep.TimeoutS))
	defer cancelAttempt()
	h.setExecTarget(ep.ProviderName, ep.UpstreamModel)

	it, err := ad.StreamStart(attemptCtx, req, res)
	if err != nil {
		detail := adapterDetail(err)
		if h.isCancelled() || detail.Category == core.ErrCancelled {
			detail = gwerrors.New(core.ErrCancelled, http.StatusBadGateway, "cancelled", "执行已取消")
		} else {
			// 网关主动收口：上游失败 / 超时 ⇒ 收口原因分类记账（SC-8，⛔ 不进取消终态）。
			h.markTeardownReason(teardownReasonFor(detail))
		}
		s.observeExec(ep, req, nil, resultForDetail(detail))
		return nil, degs, detail
	}
	h.markUpstreamStarted()
	// finishUpstream 是帧循环 finally（清理第一道闸）：取消上游执行上下文 ⇒ 传输层收口
	// 上游连接，收口时刻定格观测值（幂等；请求级第二道闸在 finalizeExec，两闸独立）。
	// ⛔ span 的 gateway.cancel.* 属性必须在收口之后落（upstream_closed 才是真的）。
	finishUpstream := sync.OnceFunc(func() {
		cancelAttempt()
		h.markUpstreamClosed()
	})
	defer finishUpstream()

	// ---- 帧循环（恒流式解析，统一 SDD 3.7）----
	var (
		acc         = newFrameAccumulator()
		detail      *core.ErrorDetail
		encoder     = newClientEncoder(mode, req.Model)
		headWritten bool
	)
	for _, d := range degs {
		encoder.AddDegradation(d)
	}
	for it.Next() {
		h.noteFrameRead()
		f := it.Frame()
		if h.isCancelled() {
			// CANCELLED：⛔ 不再读取（SC-2）；迟到分片丢弃（⛔ 不转发）。
			break
		}
		if h.isCancelPending() {
			// 延迟窗口内：上游连接保持打开、继续解码与计费（3.7），⛔ 不向客户端转发。
			acc.add(f)
			continue
		}
		if f.Kind == stream.FrameError {
			// 上游显式失败事件（如 response.failed）⇒ 立即按上游失败收口（⛔ 不冒充正常完成）。
			detail = gwerrors.New(core.ErrUpstreamError, http.StatusBadGateway, f.Status, security.Redact(f.Text))
			break
		}
		acc.add(f)
		if req.Stream {
			b, err := encoder.Encode(f)
			if err != nil {
				// chat 形状无法承载错误帧 ⇒ 传输层终止流并上报（codec 契约）。
				detail = gwerrors.New(core.ErrUpstreamError, http.StatusBadGateway, "stream_encode_failed", security.Redact(err.Error()))
				break
			}
			if len(b) > 0 {
				if err := writeStreamBytes(w, b, &headWritten, mode); err != nil {
					// 客户端写失败 = 断连第三信号（统一 SDD 6.6 要求 1）。
					h.trigger(reasonClientDisconnect, false)
					break
				}
				h.markWritten()
				h.noteFrameForwarded(frameTokens(f))
			}
		}
	}
	if detail == nil && it.Err() != nil {
		if h.isCancelled() {
			// 取消引起的传输中断不算错误（3.7：取消不是错误）。
			detail = gwerrors.New(core.ErrCancelled, http.StatusBadGateway, "cancelled", "执行已取消")
		} else {
			detail = adapterDetail(it.Err())
			if detail.Category == core.ErrCancelled {
				detail = gwerrors.New(core.ErrCancelled, detail.HTTPStatus, "cancelled", "执行已取消")
			}
		}
	}
	if detail != nil && detail.Category != core.ErrCancelled && !h.isCancelled() {
		// 网关主动收口（上游失败 / 超时 / 编码失败）⇒ 记收口原因（SC-8，⛔ 不进取消终态）。
		h.markTeardownReason(teardownReasonFor(detail))
	}
	finishUpstream() // 帧循环收口：先关上游，再落 span 的取消 / 失败属性（SC-1/SC-3 时序）

	// ---- 收口（响应装配 + 取消路径补终止帧）----
	resp := acc.result(req.Model)
	if h.isCancelled() {
		// 取消路径补终止帧（usage Estimated=true ⛔ 不按 0 计、⛔ 不产生负数，3.7 / 3.2）。
		resp.Usage = usage.EstimatedFrom(resp.Usage)
		resp.Status = core.StatusCancelled
		resp.FinishReason = core.FinishAborted
		h.setFinalUsage(resp.Usage)
		if req.Stream {
			_, _ = encoder.Encode(stream.Frame{Kind: stream.FrameDone, Status: core.StatusCancelled, Usage: resp.Usage})
			if b, err := encoder.Finish(); err == nil {
				if werr := writeStreamBytes(w, b, &headWritten, mode); werr == nil {
					h.markWritten() // 客户端已断开时写失败无害（fire-and-forget）
				}
			}
		}
		detail = gwerrors.New(core.ErrCancelled, 499, "cancelled", "执行已取消")
		s.observeExec(ep, req, resp.Usage, "cancelled")
		return resp, degs, detail
	}
	if detail != nil {
		resp.Usage = usage.EstimatedFrom(resp.Usage)
		resp.Status = core.StatusFailed
		resp.FinishReason = core.FinishUnknown
		h.setFinalUsage(resp.Usage)
		if req.Stream && headWritten {
			// 已提交流才收口 response.failed；未提交（首帧前失败）留给重试 / JSON 错误。
			// chat 形状按 codec 契约终止流不造终止帧（错误帧无法承载）。
			if mode == modeResponses {
				_, _ = encoder.Encode(stream.Frame{Kind: stream.FrameError, Text: detail.Message, Status: detail.Code})
				if b, err := encoder.Finish(); err == nil {
					if werr := writeStreamBytes(w, b, &headWritten, mode); werr == nil {
						h.markWritten()
					}
				}
			}
		}
		s.observeExec(ep, req, resp.Usage, "failed")
		return resp, degs, detail
	}
	// 正常完成。
	resp.Status = core.StatusCompleted
	h.setFinalUsage(resp.Usage)
	if req.Stream {
		if b, err := encoder.Finish(); err != nil {
			detail = gwerrors.New(core.ErrUpstreamError, http.StatusBadGateway, "stream_finish_failed", security.Redact(err.Error()))
			s.observeExec(ep, req, resp.Usage, "failed")
			return resp, degs, detail
		} else if err := writeStreamBytes(w, b, &headWritten, mode); err != nil {
			h.trigger(reasonClientDisconnect, false)
		} else {
			h.markWritten()
		}
	}
	s.observeExec(ep, req, resp.Usage, "ok")
	return resp, degs, nil
}

// startEndpointSpan 建端点级 span（统一 SDD 6.2：每次上游执行一个 CLIENT span；
// 自动重试 / 令牌轮换合并进同一 span，降级换端点才另起）。
func (s *Server) startEndpointSpan(ep *registry.Endpoint, res *core.Resolution, req *core.Request, ad adapter.Adapter) *telemetry.Span {
	span := s.tracer.Start(telemetry.OperationChat)
	span.SetAttr(telemetry.AttrProviderName, ep.ProviderName)
	span.SetAttr(telemetry.AttrRequestModel, req.Model)
	span.SetAttr(telemetry.AttrResponseModel, res.Model)
	span.SetAttr(telemetry.AttrGatewayProvider, res.Provider.String())
	span.SetAttr(telemetry.AttrGatewayShape, string(ad.OutboundShape()))
	span.SetAttr(telemetry.AttrGatewayDetectConfidence, res.Confidence.String())
	span.SetAttr(telemetry.AttrGatewayFallback, res.Fallback)
	if req.Stream {
		span.SetAttr(telemetry.AttrRequestStream, true)
	}
	return span
}

// endEndpointSpan 落终态属性并结束 span（每端点恰好一次；属性按最终结果一次写全，
// ⛔ 不让失败尝试的 error.type 污染后续成功 / 取消的收口）。
func (s *Server) endEndpointSpan(span *telemetry.Span, h *execHandle, resp *core.Response, detail *core.ErrorDetail) {
	switch {
	case detail != nil && detail.Category == core.ErrCancelled:
		s.applyCancelToSpan(span, h)
		span.SetAttr(telemetry.AttrResponseStatus, telemetry.ResponseCancelled)
		span.SetAttr(telemetry.AttrFinishReasons, []string{"error"})
	case detail != nil:
		s.applyDetailToSpan(span, h, detail)
	default:
		span.SetAttr(telemetry.AttrResponseStatus, telemetry.ResponseCompleted)
		finish := ""
		if resp != nil {
			finish = resp.FinishReason
		}
		span.SetAttr(telemetry.AttrFinishReasons, []string{finishOrStop(finish)})
		span.SetStatus(telemetry.SpanUnset)
	}
	if resp != nil {
		applyUsageToSpan(span, resp.Usage)
	} else {
		applyUsageToSpan(span, nil)
	}
	s.finishSpan(span, h)
}

// finishOrStop 空 finish 按 stop 记（与 codec.Finish 的收口口径一致）。
func finishOrStop(f string) string {
	if f == "" {
		return core.FinishStop
	}
	return f
}

// resultForDetail 错误明细对应的指标结果标签。
func resultForDetail(detail *core.ErrorDetail) string {
	if detail != nil && detail.Category == core.ErrCancelled {
		return "cancelled"
	}
	return "failed"
}

// teardownReasonFor 把错误明细映射到收口原因四分类（统一 SDD 6.6 要求 4 / SC-8）。
func teardownReasonFor(detail *core.ErrorDetail) string {
	if detail != nil && detail.Category == core.ErrTimeout {
		return reasonTimeout
	}
	return reasonUpstreamError
}

// secondsToDuration 秒数（浮点）转 Duration；非正值给保守默认 60s。
func secondsToDuration(s float64) time.Duration {
	if s <= 0 {
		return 60 * time.Second
	}
	return time.Duration(s * float64(time.Second))
}

// frameTokens 是单帧的 token 保守估算（内容按字节折半；usage 帧是总量非增量、不计）。
func frameTokens(f stream.Frame) int64 {
	n := len(f.Text) + len(f.Reasoning) + len(f.Arguments)
	if n == 0 {
		return 0
	}
	return int64((n + 1) / 2)
}

// applyDetailToSpan 把错误明细落 span（失败口径：response.status=failed、
// error.type 设、span status Error，统一 SDD 6.3 三情形表）；网关主动收口时
// 补 gateway.cancel.reason / upstream_closed（SC-8 原因可区分）。
func (s *Server) applyDetailToSpan(span *telemetry.Span, h *execHandle, detail *core.ErrorDetail) {
	span.SetAttr(telemetry.AttrResponseStatus, telemetry.ResponseFailed)
	span.SetAttr(telemetry.AttrFinishReasons, []string{"error"})
	span.SetAttr(telemetry.AttrErrorType, string(detail.Category))
	span.SetStatus(telemetry.SpanError)
	if detail.UpstreamStatus != 0 {
		span.SetAttr("gateway.upstream_status", detail.UpstreamStatus)
	}
	if detail.UpstreamRequestID != "" {
		span.SetAttr("gateway.upstream_request_id", detail.UpstreamRequestID)
	}
	if reason := h.cancelReason(); reason != "" {
		span.SetAttr(telemetry.AttrGatewayCancelReason, reason)
		span.SetAttr(telemetry.AttrGatewayCancelUpstreamClosed, h.snapshot().UpstreamClosed)
	}
}

// applyCancelToSpan 落取消四件套（统一 SDD 6.3）：status=cancelled + span Unset +
// ⛔ 不设 error.type + gateway.cancel.*。
func (s *Server) applyCancelToSpan(span *telemetry.Span, h *execHandle) {
	snap := h.snapshot()
	span.SetAttr(telemetry.AttrGatewayCancelReason, snap.CancelReason)
	span.SetAttr(telemetry.AttrGatewayCancelUpstreamClosed, snap.UpstreamClosed)
	span.SetAttr(telemetry.AttrGatewayCancelTokensAfterCancel, snap.TokensAfterCancel)
	if snap.DetectedLatencyMs > 0 {
		span.SetAttr(telemetry.AttrGatewayCancelDetectedLatency, snap.DetectedLatencyMs)
	}
	if snap.DeferredMs > 0 {
		span.SetAttr(telemetry.AttrGatewayCancelDeferredMs, snap.DeferredMs)
	}
	span.SetStatus(telemetry.SpanUnset)
}

// applyUsageToSpan 落 usage 属性（缓存读写单独计量 ⛔ 不并入 input 重复计，6.2）。
func applyUsageToSpan(span *telemetry.Span, u *core.Usage) {
	if u == nil {
		span.SetAttr(telemetry.AttrGatewayUsageEstimated, true)
		return
	}
	span.SetAttr(telemetry.AttrUsageInputTokens, u.InputTokens)
	span.SetAttr(telemetry.AttrUsageOutputTokens, u.OutputTokens)
	span.SetAttr(telemetry.AttrUsageCacheRead, u.CacheReadTokens)
	span.SetAttr(telemetry.AttrUsageCacheCreation, u.CacheWriteTokens)
	span.SetAttr(telemetry.AttrGatewayCacheReadTokens, u.CacheReadTokens)
	span.SetAttr(telemetry.AttrGatewayCacheWriteTokens, u.CacheWriteTokens)
	span.SetAttr(telemetry.AttrGatewayUsageEstimated, u.Estimated)
}

// finishSpan 补流游标后结束 span（统一 SDD 6.2 gen_ai.request.stream_cursor）。
func (s *Server) finishSpan(span *telemetry.Span, h *execHandle) {
	if snap := h.snapshot(); snap.FramesRead > 0 {
		span.SetAttr(telemetry.AttrStreamCursor, snap.FramesRead)
	}
	span.End(s.tracer)
}

// observeExec 记上游执行指标（统一 SDD 6.4：gateway.requests / tokens / usage_missing_rate）。
func (s *Server) observeExec(ep *registry.Endpoint, req *core.Request, u *core.Usage, result string) {
	s.metrics.ObserveRequest(ep.ProviderName, req.Model, result)
	if u != nil {
		s.metrics.ObserveTokens("input", u.InputTokens)
		s.metrics.ObserveTokens("output", u.OutputTokens)
		s.metrics.ObserveUsage(u.Estimated)
	} else {
		s.metrics.ObserveUsage(true)
	}
}
