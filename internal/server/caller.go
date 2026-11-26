// 日期: 2026-11-14 15:34:46.250
// 作者: Zhai Wenjia

package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/policy"
	"github.com/zhaiwenjia/FlyingGateway/internal/registry"
	"github.com/zhaiwenjia/FlyingGateway/internal/share"
)

// 本文件承载调用方校验面（R-43 / AC-36，registry callers 单源）：
//   - 鉴权：Authorization: Bearer <key>；key 以 SHA-256 哈希登记（⛔ 明文不落配置），
//     比对走常量时间；密钥可轮换（双 key 窗口）、必有过期（⛔ 永不过期，F-36）；
//   - scope 三闸：允许的模型集 / 端点集 / token 预算，超 scope ⇒ 403 scope_exceeded；
//   - 入口剥离外部身份类 header（⛔ 公网 header 直通，F-37），身份一律以 key 鉴权
//     结果为准（网关自设）；
//   - 审计四元组：caller / route / decision / correlation-id，⛔ 无密钥（R-46）；
//   - 调用方 key 与上游自有令牌**解耦** —— ⛔ 调用方凭据永不发给上游。

// callerIdentityKey 是 ctx 上的调用方登记键（*callerEntry）。
type callerIdentityKey struct{}

// AnonymousCaller 是未启用调用方鉴权时的统一身份（预算 / 限流按此归并）。
const AnonymousCaller = "anonymous"

// identityHeaders 是入口剥离的外部身份类 header。与 FH 侧 gateway/policy.ts 的
// IDENTITY_HEADERS 是同一份清单（两仓口径必须一致）：伪造身份 header 直通即绕过
// 全部下游授权（F-37）⇒ 一律剥离，身份只认网关自设（key 鉴权结果）。
var identityHeaders = []string{"X-User-Id", "X-Org-Id", "X-Role", "X-Tenant-Id", "X-Forwarded-User"}

// callerKey 是运行面的密钥登记（key_hash 在构造时解码一次，⛔ 每请求不重复解码）。
type callerKey struct {
	hash      []byte
	expiresAt time.Time
}

// callerEntry 是 registry.Caller 的运行面（限流桶 / 预算器随登记构造）。
type callerEntry struct {
	name   string
	keys   []callerKey
	scope  registry.CallerScope
	rl     *policy.RateLimitBuckets // nil ⇒ 不限流（rate_limit 省略，⛔ 不套默认值）
	budget *policy.Budget           // nil ⇒ token 预算不限（token_budget = 0）
	// 调用方在途并发位（⭐ 2026-11-26 老宅架构定界，见 registry.Caller.ConcurrencyLimit）。
	// maxInFlight = 0 ⇒ 不限（与端点级 concurrency_limit 同语义）。
	maxInFlight int64
	inFlight    atomic.Int64
}

// tryAcquire 占一个调用方在途位（满 ⇒ false，调用方给 429+Retry-After）。
// 语义与 limitGroup.acquire 的并发位一致：先占位、收口 defer release 恰好一次。
func (e *callerEntry) tryAcquire() bool {
	if e.maxInFlight <= 0 {
		return true
	}
	// ⚠️ 超限必须回退位（与 limitGroup.acquire 同款）：Add(1) 超上限时
	// 位已自增 ⇒ 不回退就泄漏 —— 单元钉 TestCallerConcurrencyGateUnit
	// 「拒绝不泄漏位」实测抓到过这个 bug（inFlight 卡在 3/2）。
	if e.inFlight.Add(1) > e.maxInFlight {
		e.inFlight.Add(-1)
		return false
	}
	return true
}

// release 归还一个调用方在途位（幂等由调用方 defer 保证；不限时 no-op）。
func (e *callerEntry) release() {
	if e.maxInFlight > 0 {
		e.inFlight.Add(-1)
	}
}

// callerAuth 是调用方鉴权 + scope 三闸 + 审计（registry callers 单源）。
type callerAuth struct {
	entries []*callerEntry
	logger  *slog.Logger
}

// newCallerAuth 从注册表 callers 构造鉴权面（callers 为空 ⇒ 开放模式，向后兼容
// 本地单机形态）。限流桶 / 预算器按登记构造（store 非 nil ⇒ 跨实例共享一份配额）。
func newCallerAuth(callers []registry.Caller, logger *slog.Logger, store share.Store, onShareError func(error)) *callerAuth {
	if logger == nil {
		logger = slog.Default()
	}
	c := &callerAuth{logger: logger}
	for _, cl := range callers {
		e := &callerEntry{name: cl.Name, scope: cl.Scope, maxInFlight: cl.ConcurrencyLimit}
		for _, k := range cl.Keys {
			// 注册表已验 key_hash 形状（64 位小写 hex），此处解码不会失败。
			raw, _ := hex.DecodeString(k.KeyHash)
			e.keys = append(e.keys, callerKey{hash: raw, expiresAt: k.ExpiresAt})
		}
		if cl.RateLimit != nil {
			if store != nil {
				e.rl = policy.NewSharedRateLimitBuckets(store, "fg:rl:caller:"+cl.Name, onShareError,
					cl.RateLimit.TPM, cl.RateLimit.RPM, cl.RateLimit.BurstS)
			} else {
				e.rl = policy.NewRateLimitBuckets(cl.RateLimit.TPM, cl.RateLimit.RPM, cl.RateLimit.BurstS)
			}
		}
		if cl.Scope.TokenBudget > 0 {
			if store != nil {
				e.budget = policy.NewSharedBudget(store, "fg:budget:caller:", onShareError, cl.Scope.TokenBudget)
			} else {
				e.budget = policy.NewBudget(cl.Scope.TokenBudget)
			}
		}
		c.entries = append(c.entries, e)
	}
	return c
}

// enabled 返回是否启用调用方鉴权。
func (c *callerAuth) enabled() bool { return len(c.entries) > 0 }

// authenticate 校验 Bearer key：SHA-256 哈希 + 常量时间比对 + 过期判定。
// 返回 (entry, denyReason)；denyReason 非空 = 拒，仅供审计与内部归因 ——
// ⛔ 响应恒为通用 401（缺失 / 无效 / 过期不区分，防枚举）。
// 比对恒扫全部登记（不提前返回，观测到的差异只剩登记数量）；除常量时间比较外
// ⛔ 不做任何按命中分支的花活。
func (c *callerAuth) authenticate(r *http.Request) (*callerEntry, string) {
	auth := r.Header.Get("Authorization")
	key, ok := strings.CutPrefix(auth, "Bearer ")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return nil, "missing_key"
	}
	sum := sha256.Sum256([]byte(key))
	now := time.Now()
	var matched *callerEntry
	matchedExpired := false
	for _, e := range c.entries {
		for _, k := range e.keys {
			if subtle.ConstantTimeCompare(sum[:], k.hash) != 1 {
				continue
			}
			if k.expiresAt.After(now) {
				matched = e
			} else {
				matchedExpired = true
			}
		}
	}
	if matched == nil {
		if matchedExpired {
			return nil, "expired_key"
		}
		return nil, "invalid_key"
	}
	return matched, ""
}

// audit 记校验判定四元组（R-43 / R-46）：caller / route / decision / correlation-id。
// ⛔ 明文 key 与哈希都不入日志（审计只见身份名，不见凭据材料）。
func (c *callerAuth) audit(caller, route, decision, reason, corr string) {
	c.logger.Info("caller_decision",
		"caller", caller,
		"route", route,
		"decision", decision,
		"reason", reason,
		"correlation_id", corr)
}

// wrap 是 /v1/* 的强制鉴权中间件：
//   - 入口先剥离外部身份类 header（F-37，开放模式同样剥离）；
//   - callers 为空 ⇒ 开放模式直通（向后兼容）；
//   - 鉴权失败 ⇒ 401 通用错误（8.1 语义）+ 审计 deny（⛔ 拒绝静默）；
//   - 鉴权成功 ⇒ 审计 allow，登记与关联号入 ctx（身份 = 网关自设）。
func (c *callerAuth) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, h := range identityHeaders {
			r.Header.Del(h)
		}
		if !c.enabled() {
			next(w, r)
			return
		}
		entry, deny := c.authenticate(r)
		corr := clientRequestID(r)
		route := r.Method + " " + r.URL.Path
		if deny != "" {
			c.audit(AnonymousCaller, route, "deny", deny, corr)
			writeError(w, http.StatusUnauthorized, string(core.ErrAuth), "unauthorized",
				"缺少或无效的调用方 API key（Authorization: Bearer <key>）")
			return
		}
		c.audit(entry.name, route, "allow", "authenticated", corr)
		ctx := context.WithValue(r.Context(), callerIdentityKey{}, entry)
		ctx = context.WithValue(ctx, clientRequestIDKey{}, corr)
		next(w, r.WithContext(ctx))
	}
}

// scopeCheck 是 scope 三闸（R-43）：允许的模型集 / 端点集 / token 预算。
// 返回拒绝原因（空 = 全过）；token 预算按调用方累计（Allow 预占、finalizeExec 记实际）。
func (e *callerEntry) scopeCheck(model, endpoint string, est int64) string {
	if !slices.Contains(e.scope.Models, model) {
		return fmt.Sprintf("model out of scope: %s", model)
	}
	if !slices.Contains(e.scope.Endpoints, endpoint) {
		return fmt.Sprintf("endpoint out of scope: %s", endpoint)
	}
	if e.budget != nil {
		if err := e.budget.Allow(e.name, est); err != nil {
			return fmt.Sprintf("token budget exceeded: %s", err.Error())
		}
	}
	return ""
}

// allowsEndpoint 判端点是否在允许集（降级链过滤用；⛔ 降级不得绕过 scope）。
func (e *callerEntry) allowsEndpoint(name string) bool {
	return slices.Contains(e.scope.Endpoints, name)
}

// scopeFilterChain 按调用方 scope 过滤降级链候选（⛔ 降级不得绕过 scope：端点不在
// 允许集的不作为降级目标 —— 否则「主端点限死、降级链全开」等于 scope 形同虚设；
// 开放模式不过滤）。
func scopeFilterChain(r *http.Request, chain []string) []string {
	ce := callerEntryOf(r)
	if ce == nil {
		return chain
	}
	out := make([]string, 0, len(chain))
	for _, name := range chain {
		if ce.allowsEndpoint(name) {
			out = append(out, name)
		}
	}
	return out
}

// recordUsage 记实际消耗（scope token 预算累计；无预算 / 未登记 ⇒ 无操作）。
func (c *callerAuth) recordUsage(caller string, tokens int64) {
	for _, e := range c.entries {
		if e.name == caller {
			if e.budget != nil {
				e.budget.Record(caller, tokens)
			}
			return
		}
	}
}

// callerIdentityOf 取请求的调用方身份名（中间件放入 ctx；未放 ⇒ anonymous）。
func callerIdentityOf(r *http.Request) string {
	if e := callerEntryOf(r); e != nil {
		return e.name
	}
	return AnonymousCaller
}

// callerEntryOf 取请求的调用方登记（scope / 限流 / 预算用；开放模式 ⇒ nil）。
func callerEntryOf(r *http.Request) *callerEntry {
	e, _ := r.Context().Value(callerIdentityKey{}).(*callerEntry)
	return e
}
