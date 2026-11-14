// 日期: 2026-11-14 15:34:46.250
// 作者: Zhai Wenjia

package server

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/policy"
)

// 本文件承载调用方 API key 侧（统一 SDD 8.1 两段式凭据的「调用方段」）：
//   - 调用方凭据与上游自有令牌**解耦** —— 调用方 key 只做调用方鉴权与限流 / 计费口径，
//     ⛔ 永不发给上游；上游请求头永远是网关自有令牌（注入点在 authTransport）；
//   - 每调用方一个令牌桶（policy.TokenBucket，token 级，⛔ 不只按请求数，8.3）；
//   - 未配置调用方 key 时不校验（本地开放模式）；配置了则缺失 / 无效一律 401
//     （鉴权错误按语义返回，8.1）。

// callerIdentityKey 是 ctx 上的调用方身份键。
type callerIdentityKey struct{}

// AnonymousCaller 是未启用调用方鉴权时的统一身份（预算 / 限流按此归并）。
const AnonymousCaller = "anonymous"

// callerAuth 是调用方鉴权与每调用方限流。
type callerAuth struct {
	// keys 是调用方 API key 白名单（空 = 开放模式，不校验）。
	keys map[string]bool

	mu sync.Mutex
	// buckets 是每调用方令牌桶（惰性创建；全局兜底参数，token 维）。
	buckets map[string]*policy.TokenBucket
	// bucketCapacity / bucketRefill 是全局兜底桶参数（≤ 0 = 不启用）。
	bucketCapacity int64
	bucketRefill   float64

	// rlOverrides 是按 key 的契约配额（注册表 caller_rate_limits，key 明文值 → 双维桶）。
	// 命中 ⇒ 以契约桶为准（覆盖全局兜底）；未登记 ⇒ 不限流（⛔ 不套默认值）。
	rlOverrides map[string]*policy.RateLimitBuckets
}

// newCallerAuth 构造调用方鉴权。keys 为空 ⇒ 开放模式。
func newCallerAuth(keys []string, bucketCapacity int64, bucketRefill float64, rlOverrides map[string]*policy.RateLimitBuckets) *callerAuth {
	c := &callerAuth{
		keys:           map[string]bool{},
		buckets:        map[string]*policy.TokenBucket{},
		bucketCapacity: bucketCapacity,
		bucketRefill:   bucketRefill,
		rlOverrides:    rlOverrides,
	}
	if c.rlOverrides == nil {
		c.rlOverrides = map[string]*policy.RateLimitBuckets{}
	}
	for _, k := range keys {
		if k != "" {
			c.keys[k] = true
		}
	}
	return c
}

// enabled 返回是否启用调用方鉴权。
func (c *callerAuth) enabled() bool { return len(c.keys) > 0 }

// identity 取请求的调用方身份：开放模式 ⇒ anonymous；开启鉴权 ⇒ 校验 Bearer key
// （⛔ 不做常量时间比较之外的花活 —— key 命中即身份，未命中即 401）。
func (c *callerAuth) identity(r *http.Request) (string, bool) {
	if !c.enabled() {
		return AnonymousCaller, true
	}
	auth := r.Header.Get("Authorization")
	key, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return "", false
	}
	key = strings.TrimSpace(key)
	if key == "" || !c.keys[key] {
		return "", false
	}
	return key, true
}

// wrap 是 /v1/* 的强制鉴权中间件（缺失 / 无效 ⇒ 401 auth，8.1 语义）。
func (c *callerAuth) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := c.identity(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, string(core.ErrAuth), "unauthorized",
				"缺少或无效的调用方 API key（Authorization: Bearer <key>）")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), callerIdentityKey{}, id)))
	}
}

// rateLimitsFor 取指定调用方的契约配额桶（注册表 caller_rate_limits）；未登记 ⇒ nil（不限流）。
func (c *callerAuth) rateLimitsFor(id string) *policy.RateLimitBuckets {
	if c == nil {
		return nil
	}
	return c.rlOverrides[id]
}

// bucketFor 取（并按需创建）指定调用方的令牌桶；未启用每调用方限流 ⇒ nil。
func (c *callerAuth) bucketFor(id string) *policy.TokenBucket {
	if c == nil || c.bucketCapacity <= 0 || c.bucketRefill <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.buckets[id]
	if !ok {
		b = policy.NewTokenBucket(c.bucketCapacity, c.bucketRefill)
		c.buckets[id] = b
	}
	return b
}

// callerIdentityOf 取请求的调用方身份（中间件放入 ctx；未放 ⇒ anonymous）。
func callerIdentityOf(r *http.Request) string {
	if id, ok := r.Context().Value(callerIdentityKey{}).(string); ok && id != "" {
		return id
	}
	return AnonymousCaller
}
