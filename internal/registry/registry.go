// 日期: 2026-11-14 14:13:45.765
// 作者: Zhai Wenjia

// Package registry 实现端点注册表（统一 SDD 9.1 / 9.2）：注册表是路由的唯一真源 ——
// 列表名 model（客户端别名）映射到 upstream_model + 端点；GET /v1/models 由注册表生成；
// 请求的 model 必须是列表内名称，列表外 ⇒ 显式报错 404 model_not_found，⛔ 不透传任意模型名。
//
// 加载口径（统一 SDD 9.2）：未知字段拒绝（DisallowUnknownFields）；upstream_model /
// capabilities / credential_ref 缺失 ⇒ 启动报错，⛔ 不用默认值静默补齐；凭据只存来源
// 引用（环境变量名 / 密钥别名），⛔ 不存明文。
package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/routing"
)

// 路由策略取值（统一 SDD 9.2：strategy ∈ {priority, round_robin}）。
const (
	StrategyPriority   = "priority"
	StrategyRoundRobin = "round_robin"
)

// Capabilities 是端点能力标记（统一 SDD 9.1：tools / stream / vision 三项）。
type Capabilities struct {
	SupportsTools  bool `json:"supports_tools"`
	SupportsStream bool `json:"supports_stream"`
	SupportsVision bool `json:"supports_vision"`
}

// Endpoint 是端点注册表的一行（统一 SDD 9.1 Endpoint schema，字段语义逐条见该节）。
type Endpoint struct {
	// Name 是端点名称，全局唯一。
	Name string `json:"name"`
	// BaseURL 是上游 base URL（识别层按 host 规则表判定供应商）。
	BaseURL string `json:"base_url"`
	// Model 是客户端请求的模型名（别名）——调用方看到的模型标识。
	Model string `json:"model"`
	// UpstreamModel 是实际上游模型标识（别名与真实上游模型分离，统一 SDD 9.1 两层语义）。
	UpstreamModel string `json:"upstream_model"`
	// ProviderName 是 OTel gen_ai.provider.name 取值（可观测字段与实际配置逐字一致）。
	ProviderName string `json:"provider_name"`
	// ContextWindow 是上下文长度上限（token）；越界判定与 /v1/models 输出都用它。
	ContextWindow int `json:"context_window"`
	// MaxOutputTokens 是输出上限（token）；越界判定用「估算输入 + max_output_tokens > context_window」。
	MaxOutputTokens int `json:"max_output_tokens"`
	// Capabilities 是能力标记（tools / stream / vision 三项）。
	// 指针承载「capabilities 键是否出现」的判定（统一 SDD 9.2：缺失 ⇒ 启动报错，
	// ⛔ 不用默认值静默补齐；显式全 false 是合法的「无能力」声明）；Load 校验后恒非空。
	Capabilities *Capabilities `json:"capabilities"`
	// CredentialRef 是凭据来源引用（环境变量名 / 密钥别名），⛔ 不存明文。
	CredentialRef string `json:"credential_ref"`
	// TimeoutS 是默认超时（秒）。
	TimeoutS float64 `json:"timeout_s"`
	// Priority 是优先级，数值越小越优先。
	Priority int `json:"priority"`
	// RateLimit 是该端点的上游套餐配额（限流锚点，统一 SDD 7.2 锚点分离）。
	// 未配置（缺键）⇒ 该端点不限流 —— 配额数字只来自套餐契约，⛔ 不造默认值
	//（无锚点的全局默认桶实测会造出隐藏吞吐天花板，见 policy.RateLimitBuckets 注释）。
	RateLimit *RateLimit `json:"rate_limit,omitempty"`
	// ConcurrencyLimit 是该端点（或其共享组）的在途上游请求数上限（0 = 不限）。
	// 并发是一等限流维（DeepSeek 类供应商只限并发；流式长调用占位到流收口），
	// 计量单位 = 在途上游尝试（发起 +1 / 收口 −1），⛔ 不按 TCP 连接数计
	//（HTTP/2 多路复用下连接数不约束并发，调研见 docs/20261114-多供应商并发-调研-*.md）。
	ConcurrencyLimit int64 `json:"concurrency_limit,omitempty"`
	// LimitsGroup 是配额共享组名（可选）：同组端点共享同一份 rate_limit / concurrency_limit
	// 计数（同供应商多模型 / 多 key / 多部署共享一个配额池的情形，如项目级配额）。
	// 缺省（空）⇒ 该端点自成一组。同组各端点的配额声明必须逐字一致，否则启动报错。
	LimitsGroup string `json:"limits_group,omitempty"`
	// Breaker 是端点熔断配置（统一 SDD 7.3；缺键 ⇒ 默认 5 / 30s / 1）。
	Breaker *BreakerConfig `json:"breaker,omitempty"`
}

// BreakerConfig 是端点熔断三态配置（统一 SDD 7.3）：连续可归因失败达阈值 ⇒ 熔断打开、
// 冷却期满转半开放行探测、探测成功闭合 / 失败重新计冷却。零值字段取默认
// （FailureThreshold=5、CooldownS=30、Probe=1）；负数 ⇒ 加载报错（⛔ 不静默按默认处理）。
type BreakerConfig struct {
	FailureThreshold int     `json:"failure_threshold"` // 连续可归因失败阈值（默认 5）
	CooldownS        float64 `json:"cooldown_s"`        // 熔断打开时长秒（默认 30）
	Probe            int     `json:"probe"`             // 半开同时放行的探测请求数（默认 1）
}

// RateLimit 是端点的上游套餐配额（tokens/min、requests/min 与突发桶深）。
type RateLimit struct {
	// TPM 是每分钟归一化 token 配额（<= 0 ⇒ token 维不限）。
	TPM float64 `json:"tpm"`
	// RPM 是每分钟请求数配额（<= 0 ⇒ 请求维不限）。
	RPM float64 `json:"rpm"`
	// BurstS 是桶深折算秒数（桶深 = 速率/60 × burst_s；突发上限来自真实客户端扇出形态，⛔ 不猜）。
	BurstS float64 `json:"burst_s"`
}

// CallerKey 是调用方 key 的登记：SHA-256 哈希 + 过期时刻（R-43 / F-36：哈希存储、
// 可轮换、有过期；⛔ 明文不入配置 —— key_hash 只收 64 位小写 hex，非此形状 = 明文
// 混入，启动报错）。ExpiresAt 必填且必须是未来时刻（⛔ 永不过期；过期登记 = 配置
// 错误，轮换后请同步清理）。
type CallerKey struct {
	KeyHash   string    `json:"key_hash"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CallerScope 是 per-caller scope（R-43：允许的模型集 / 端点集 / token 预算）。
// 模型集与端点集显式非空且引用已登记对象（⛔ 不设默认全集）；TokenBudget ≥ 0
// （0 = 不限，超预算按 AC-36 归「超 scope」⇒ 403）。
type CallerScope struct {
	Models      []string `json:"models"`
	Endpoints   []string `json:"endpoints"`
	TokenBudget int64    `json:"token_budget,omitempty"`
}

// Caller 是调用方登记（registry callers 单源）：身份 + 密钥（哈希 + 过期）+ scope +
// 配额合一登记 —— 替代明文 GATEWAY_CALLER_KEYS 白名单 + caller_rate_limits 双机制。
// 调用方 key 只做调用方鉴权与限流 / 计费口径，⛔ 永不发给上游。Keys 1–2 把
// （双 key 平滑轮换窗口，新旧并存、⛔ 堆历史 key）；RateLimit 省略 ⇒ 不限流
// （⛔ 不套默认值，统一 SDD 7.2 锚点分离）。
type Caller struct {
	Name      string      `json:"name"`
	Keys      []CallerKey `json:"keys"`
	Scope     CallerScope `json:"scope"`
	RateLimit *RateLimit  `json:"rate_limit,omitempty"`
}

// CredentialRateLimit 是上游自有令牌池的契约配额登记（同供应商多 key 场景，统一 SDD 7.2
// 锚点分离）。⚠️ 与调用方令牌两域解耦：调用方 key 由外部项目签发（只管理本系统的消费额度，
// 见 callers.rate_limit / scope.token_budget），⛔ 永不发给供应商；本登记只管**上游自有令牌**的套餐配额。
// KeyRef 与端点 credential_ref 同名的环境变量（池内多 key 逗号/换行分隔）；
// 未登记 ⇒ 该池不限流（⛔ 不造默认值）。三层语义（可组合，任一不足即拒）：
//   - PerKey：每个 key 一份桶（N 个 key = N 份套餐容量，业界多 key 池化的共识模型）；
//   - Shared：全池共享一份桶（org/项目级配额池：多 key 共享一个配额的场景）；
//   - KeyLimits：逐 key 覆盖（按池内 key 顺序对应，缺省用 PerKey —— 不同订阅档混池）。
type CredentialRateLimit struct {
	KeyRef    string       `json:"key_ref"`
	PerKey    *RateLimit   `json:"per_key,omitempty"`
	Shared    *RateLimit   `json:"shared,omitempty"`
	KeyLimits []*RateLimit `json:"key_limits,omitempty"`
}

// Registry 是端点注册表（统一 SDD 9.1 / 9.2）。
type Registry struct {
	// Strategy 是路由策略：priority（按 Priority 选）/ round_robin（轮转）。
	Strategy string
	// FallbackChains 是按失败类型的降级链（统一 SDD 7.4；实现计划偏差 4）：
	// 键 = 失败类型，值 = 有序端点名数组（不含主端点语义由 routing.SelectChain 处理）。
	FallbackChains map[routing.FailureKind][]string
	// Callers 是调用方登记（registry callers 单源：哈希 key + 过期 + scope + 配额合一）。
	Callers []Caller
	// CredentialRateLimits 是上游令牌池的契约配额登记（多 key：per_key / shared / key_limits）。
	CredentialRateLimits []CredentialRateLimit

	endpoints []Endpoint             // 确定性排序后的全量端点
	byModel   map[string][]*Endpoint // model 别名 → 候选端点（同 endpoints 排序）
	rrCounter atomic.Uint64          // round_robin 轮转计数（显式计数器驱动，⛔ 不掺随机 / 时间）
}

// fileWire 是配置文件顶层结构（统一 SDD 9.2）。
type fileWire struct {
	Strategy             string                `json:"strategy"`
	Endpoints            []Endpoint            `json:"endpoints"`
	FallbackChains       map[string][]string   `json:"fallback_chains"`
	Callers              []Caller              `json:"callers"`
	CredentialRateLimits []CredentialRateLimit `json:"credential_rate_limits"`
}

// ErrModelNotFound 是「模型不在注册表内」的哨兵错误（可 errors.Is 判定）。
var ErrModelNotFound = errors.New("registry: 模型不在注册表内")

// ModelNotFoundError 是列表外模型的类型化错误（统一 SDD 9.1：上层映射 404 model_not_found）。
type ModelNotFoundError struct {
	Model string
}

func (e *ModelNotFoundError) Error() string {
	return fmt.Sprintf("model_not_found: 模型 %q 不在注册表（⛔ 不透传任意模型名）", e.Model)
}

// Unwrap 让 errors.Is 命中 ErrModelNotFound。
func (e *ModelNotFoundError) Unwrap() error { return ErrModelNotFound }

// Load 加载端点注册表并做启动期自检（统一 SDD 9.2 结构与校验规则）：
//   - 未知字段拒绝（顶层 / 端点 / capabilities 三层，DisallowUnknownFields）；
//   - strategy ∈ {priority, round_robin}，缺失或未知 ⇒ 报错；
//   - 必备字段完整：name / base_url / model / upstream_model / provider_name /
//     credential_ref 非空、capabilities 键出现，缺失 ⇒ 启动报错（⛔ 不用默认值静默补齐）；
//   - credential_ref 是来源引用，明文密钥形态（sk- 开头）⇒ 报错（⛔ 不存明文）；
//   - name 全局唯一（9.1）；context_window > 0（与 routing.CheckContext 契约一致）；
//     timeout_s > 0、max_output_tokens ≥ 0（负数 / 零超时无语义）；
//   - fallback_chains（可选）：键 = 七类失败类型之一，值 = 有序端点名数组，
//     引用不存在端点或空数组 ⇒ 报错。
func Load(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("registry: 读配置 %s 失败: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // 未知字段拒绝（统一 SDD 2.3 域①约束 / 9.2）
	var doc fileWire
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("registry: 解析配置 %s 失败（未知字段一并拒绝）: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("registry: 配置 %s 尾部有多余内容", path)
	}
	if doc.Strategy != StrategyPriority && doc.Strategy != StrategyRoundRobin {
		return nil, fmt.Errorf("registry: 配置 %s 自检失败：strategy = %q，只允许 %s / %s",
			path, doc.Strategy, StrategyPriority, StrategyRoundRobin)
	}
	if len(doc.Endpoints) == 0 {
		return nil, fmt.Errorf("registry: 配置 %s 自检失败：endpoints 为空（注册表至少一条端点）", path)
	}

	r := &Registry{
		Strategy:       doc.Strategy,
		FallbackChains: make(map[routing.FailureKind][]string, len(doc.FallbackChains)),
		endpoints:      make([]Endpoint, 0, len(doc.Endpoints)),
		byModel:        make(map[string][]*Endpoint, len(doc.Endpoints)),
	}

	seen := make(map[string]bool, len(doc.Endpoints))
	for i := range doc.Endpoints {
		ep := doc.Endpoints[i]
		if err := validateEndpoint(path, i+1, ep); err != nil {
			return nil, err
		}
		if seen[ep.Name] {
			return nil, fmt.Errorf("registry: 配置 %s 自检失败：端点名 %q 重复（Name 全局唯一，统一 SDD 9.1）", path, ep.Name)
		}
		seen[ep.Name] = true
		r.endpoints = append(r.endpoints, ep)
	}

	// 配额共享组一致性：同组（limits_group 相同）各端点的配额声明必须逐字一致 ——
	// 共享一个计数器的组里出现两份不同声明，要么超发要么欠发，都是隐性错误 ⇒ 启动报错。
	type limitDecl struct {
		rl  *RateLimit
		con int64
	}
	groupDecl := make(map[string]limitDecl, len(doc.Endpoints))
	for _, ep := range doc.Endpoints {
		key := ep.LimitsGroup
		if key == "" {
			continue // 自成一组，无一致性问题
		}
		d := limitDecl{rl: ep.RateLimit, con: ep.ConcurrencyLimit}
		if prev, ok := groupDecl[key]; ok {
			sameRL := (prev.rl == nil && d.rl == nil) ||
				(prev.rl != nil && d.rl != nil && *prev.rl == *d.rl)
			if !sameRL || prev.con != d.con {
				return nil, fmt.Errorf("registry: 配置 %s 自检失败：limits_group %q 内配额声明不一致（端点 %q 与先前声明不同；同组共享一份计数，声明必须逐字一致）", path, key, ep.Name)
			}
			continue
		}
		groupDecl[key] = d
	}

	// 调用方登记（可选；registry callers 单源 —— 哈希 key + 过期 + scope + 配额合一，
	// R-43 / F-36 / Q-23）：⛔ 明文 key（key_hash 只收 64 位小写 hex）、⛔ 永不过期
	//（expires_at 必填且必须未来）、scope 显式（模型集 / 端点集非空且引用已登记）、
	// 轮换窗口双 key（1–2 把，⛔ 堆历史 key）、key_hash 全局唯一。
	if len(doc.Callers) > 0 {
		modelSet := make(map[string]bool, len(doc.Endpoints))
		for _, ep := range doc.Endpoints {
			modelSet[ep.Model] = true
		}
		seenName := make(map[string]bool, len(doc.Callers))
		seenHash := make(map[string]bool)
		for i, cl := range doc.Callers {
			who := fmt.Sprintf("第 %d 条 callers", i+1)
			if cl.Name == "" {
				return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s：name 缺失或为空（审计 / 预算 / 限流按 name 归并）", path, who)
			}
			if seenName[cl.Name] {
				return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s：name %q 重复（全局唯一）", path, who, cl.Name)
			}
			seenName[cl.Name] = true
			if len(cl.Keys) < 1 || len(cl.Keys) > 2 {
				return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s（%s）：keys 数量 = %d，须 1–2 把（双 key 平滑轮换窗口，⛔ 堆历史 key）", path, who, cl.Name, len(cl.Keys))
			}
			for j, k := range cl.Keys {
				// 存储形态检查（AC-36）：key_hash 只收 64 位小写 hex —— 明文 / 非 SHA-256 形态
				// 一律拒（⛔ 配置里出现可直接使用的凭据）。
				decoded, err := hex.DecodeString(k.KeyHash)
				if err != nil || len(decoded) != sha256.Size || k.KeyHash != strings.ToLower(k.KeyHash) {
					return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s（%s）的 keys[%d].key_hash 非 64 位小写 hex（疑似明文 key？⛔ 只存 SHA-256 哈希）", path, who, cl.Name, j)
				}
				if seenHash[k.KeyHash] {
					return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s（%s）的 keys[%d].key_hash 与已有登记重复（全局唯一）", path, who, cl.Name, j)
				}
				seenHash[k.KeyHash] = true
				if k.ExpiresAt.IsZero() {
					return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s（%s）的 keys[%d].expires_at 缺失（⛔ 永不过期，F-36）", path, who, cl.Name, j)
				}
				if !k.ExpiresAt.After(time.Now()) {
					return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s（%s）的 keys[%d].expires_at = %s 已过期（轮换后请同步清理过期 key）", path, who, cl.Name, j, k.ExpiresAt.Format(time.RFC3339))
				}
			}
			if len(cl.Scope.Models) == 0 || len(cl.Scope.Endpoints) == 0 {
				return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s（%s）：scope.models / scope.endpoints 必须显式非空（⛔ 不设默认全集）", path, who, cl.Name)
			}
			if cl.Scope.TokenBudget < 0 {
				return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s（%s）：scope.token_budget = %d 不得为负（0 = 不限）", path, who, cl.Name, cl.Scope.TokenBudget)
			}
			for _, m := range cl.Scope.Models {
				if !modelSet[m] {
					return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s（%s）：scope.models 引用不存在的模型 %q", path, who, cl.Name, m)
				}
			}
			for _, n := range cl.Scope.Endpoints {
				if !seen[n] {
					return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s（%s）：scope.endpoints 引用不存在的端点 %q", path, who, cl.Name, n)
				}
			}
			if cl.RateLimit != nil {
				if err := validateRateLimitShape(path, fmt.Sprintf("%s（%s）的 rate_limit", who, cl.Name),
					cl.RateLimit.TPM, cl.RateLimit.RPM, cl.RateLimit.BurstS); err != nil {
					return nil, err
				}
			}
		}
	}
	r.Callers = doc.Callers

	// 上游令牌池契约配额（可选）：三层至少登记一层，各层形状与 rate_limit 同口径。
	for i, crl := range doc.CredentialRateLimits {
		who := fmt.Sprintf("第 %d 条 credential_rate_limits", i+1)
		if crl.KeyRef == "" {
			return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s：key_ref 缺失或为空（只存来源引用，⛔ 不存明文 key）", path, who)
		}
		if crl.PerKey == nil && crl.Shared == nil && len(crl.KeyLimits) == 0 {
			return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s：per_key / shared / key_limits 至少登记一层（全空 = 不限流，请省略该登记）", path, who)
		}
		for _, lvl := range []struct {
			name string
			rl   *RateLimit
		}{{"per_key", crl.PerKey}, {"shared", crl.Shared}} {
			if lvl.rl == nil {
				continue
			}
			if err := validateRateLimitShape(path, fmt.Sprintf("%s 的 %s", who, lvl.name),
				lvl.rl.TPM, lvl.rl.RPM, lvl.rl.BurstS); err != nil {
				return nil, err
			}
		}
		for j, kl := range crl.KeyLimits {
			if kl == nil {
				// null = 该 key 用 per_key 缺省（混池只覆盖部分 key 的简写）；无 per_key 兜底则是空壳。
				if crl.PerKey == nil {
					return nil, fmt.Errorf("registry: 配置 %s 自检失败：%s：key_limits[%d] 为 null 且无 per_key 兜底（null 语义 = 该 key 用 per_key 缺省）", path, who, j)
				}
				continue
			}
			if err := validateRateLimitShape(path, fmt.Sprintf("%s 的 key_limits[%d]", who, j),
				kl.TPM, kl.RPM, kl.BurstS); err != nil {
				return nil, err
			}
		}
	}
	r.CredentialRateLimits = doc.CredentialRateLimits

	// 确定性排序：Priority 升序（数值越小越优先），同值按 Name 升序
	//（⛔ 不依赖配置文件顺序 / map 遍历顺序，统一 SDD 1.4.3 路由决策可复现）。
	sort.Slice(r.endpoints, func(a, b int) bool {
		if r.endpoints[a].Priority != r.endpoints[b].Priority {
			return r.endpoints[a].Priority < r.endpoints[b].Priority
		}
		return r.endpoints[a].Name < r.endpoints[b].Name
	})
	for i := range r.endpoints {
		ep := &r.endpoints[i]
		r.byModel[ep.Model] = append(r.byModel[ep.Model], ep)
	}

	for key, chain := range doc.FallbackChains {
		kind, err := routing.ParseFailureKind(key)
		if err != nil {
			return nil, fmt.Errorf("registry: 配置 %s 自检失败：fallback_chains 键 %q 非法: %w", path, key, err)
		}
		if len(chain) == 0 {
			return nil, fmt.Errorf("registry: 配置 %s 自检失败：fallback_chains[%s] 为空数组（无降级端点请直接省略该键）", path, key)
		}
		for _, name := range chain {
			if name == "" {
				return nil, fmt.Errorf("registry: 配置 %s 自检失败：fallback_chains[%s] 含空端点名", path, key)
			}
			if !seen[name] {
				return nil, fmt.Errorf("registry: 配置 %s 自检失败：fallback_chains[%s] 引用不存在的端点 %q", path, key, name)
			}
		}
		r.FallbackChains[kind] = append([]string(nil), chain...)
	}
	return r, nil
}

// validateEndpoint 做单端点字段自检（统一 SDD 9.1 字段语义 / 9.2 必备字段完整性）。
func validateEndpoint(path string, ordinal int, ep Endpoint) error {
	prefix := fmt.Sprintf("registry: 配置 %s 自检失败：第 %d 个端点", path, ordinal)
	for _, f := range []struct {
		name, value string
	}{
		{"name", ep.Name}, {"base_url", ep.BaseURL}, {"model", ep.Model},
		{"upstream_model", ep.UpstreamModel}, {"provider_name", ep.ProviderName},
		{"credential_ref", ep.CredentialRef},
	} {
		if f.value == "" {
			return fmt.Errorf("%s：%s 缺失或为空（⛔ 不用默认值静默补齐，统一 SDD 9.2）", prefix, f.name)
		}
	}
	if ep.Capabilities == nil {
		return fmt.Errorf("%s：capabilities 缺失（⛔ 不用默认值静默补齐，统一 SDD 9.2）", prefix)
	}
	// 明文密钥形态拒绝：credential_ref 只能是来源引用（环境变量名 / 密钥别名），
	// sk- 开头是典型明文密钥形态（统一 SDD 8.1 凭据隔离三步：⛔ 不存明文）。
	if strings.HasPrefix(ep.CredentialRef, "sk-") {
		return fmt.Errorf("%s：credential_ref %q 疑似明文密钥（sk- 开头）⇒ 启动报错（⛔ 注册表只存来源引用）",
			prefix, ep.CredentialRef)
	}
	if ep.ContextWindow <= 0 {
		return fmt.Errorf("%s：%q 的 context_window = %d 必须为正（越界判定依赖它）", prefix, ep.Name, ep.ContextWindow)
	}
	if ep.MaxOutputTokens < 0 {
		return fmt.Errorf("%s：%q 的 max_output_tokens = %d 不得为负", prefix, ep.Name, ep.MaxOutputTokens)
	}
	if ep.TimeoutS <= 0 {
		return fmt.Errorf("%s：%q 的 timeout_s = %v 必须为正（默认超时无零值语义）", prefix, ep.Name, ep.TimeoutS)
	}
	if ep.ConcurrencyLimit < 0 {
		return fmt.Errorf("%s：%q 的 concurrency_limit = %d 不得为负（0 = 不限）", prefix, ep.Name, ep.ConcurrencyLimit)
	}
	if ep.BaseURL != "" {
		if !strings.Contains(ep.BaseURL, "://") {
			return fmt.Errorf("%s：%q 的 base_url %q 须形如 scheme://host[/path]", prefix, ep.Name, ep.BaseURL)
		}
	}
	if ep.RateLimit != nil {
		if err := validateRateLimitShape(path, fmt.Sprintf("端点 %q 的 rate_limit", ep.Name),
			ep.RateLimit.TPM, ep.RateLimit.RPM, ep.RateLimit.BurstS); err != nil {
			return err
		}
	}
	if ep.Breaker != nil {
		if ep.Breaker.FailureThreshold < 0 {
			return fmt.Errorf("%s：%q 的 breaker.failure_threshold = %d 不得为负（0 = 默认 5）", prefix, ep.Name, ep.Breaker.FailureThreshold)
		}
		if ep.Breaker.CooldownS < 0 {
			return fmt.Errorf("%s：%q 的 breaker.cooldown_s = %v 不得为负（0 = 默认 30）", prefix, ep.Name, ep.Breaker.CooldownS)
		}
		if ep.Breaker.Probe < 0 {
			return fmt.Errorf("%s：%q 的 breaker.probe = %d 不得为负（0 = 默认 1）", prefix, ep.Name, ep.Breaker.Probe)
		}
	}
	return nil
}

// validateRateLimitShape 校验配额登记形状（端点 rate_limit / callers.rate_limit / credential_rate_limits 共用）：
// burst_s 必须为正（桶深 = 速率/60 × burst_s，突发上限由部署方按真实扇出登记，⛔ 不猜）；
// tpm / rpm 不得为负且至少一维为正（两维全 0 = 不限流 ⇒ 请直接省略该登记，别写空壳）。
func validateRateLimitShape(path, who string, tpm, rpm, burstS float64) error {
	prefix := fmt.Sprintf("registry: 配置 %s 自检失败：%s", path, who)
	if tpm < 0 || rpm < 0 {
		return fmt.Errorf("%s：tpm=%v / rpm=%v 不得为负", prefix, tpm, rpm)
	}
	if tpm == 0 && rpm == 0 {
		return fmt.Errorf("%s：tpm 与 rpm 同时为 0 等于不限流（请省略该登记，⛔ 不写空壳）", prefix)
	}
	if burstS <= 0 {
		return fmt.Errorf("%s：burst_s = %v 必须为正（桶深折算秒数，突发上限按真实客户端扇出登记）", prefix, burstS)
	}
	return nil
}

// Lookup 按模型别名选端点（统一 SDD 9.1 模型映射）：
// 列表外 ⇒ *ModelNotFoundError（上层映射 404 model_not_found）。
// 列表内多端点同别名（模型组）时按 strategy 调度：
//   - priority：取 Priority 最小者（同值按 Name 升序，显式排序、可复现）；
//   - round_robin：在确定性排序的候选里显式计数轮转（⛔ 不掺随机 / 时间）。
//
// 返回副本指针，调用方修改不影响注册表。
func (r *Registry) Lookup(model string) (*Endpoint, error) {
	cands := r.byModel[model]
	if len(cands) == 0 {
		return nil, &ModelNotFoundError{Model: model}
	}
	var chosen *Endpoint
	if r.Strategy == StrategyRoundRobin {
		n := r.rrCounter.Add(1)
		chosen = cands[(n-1)%uint64(len(cands))]
	} else {
		chosen = cands[0]
	}
	ep := *chosen
	return &ep, nil
}

// List 返回全量端点，确定性排序：Priority 升序（数值越小越优先），同值按 Name 升序。
// 返回副本，调用方修改不影响注册表。
func (r *Registry) List() []Endpoint {
	out := make([]Endpoint, len(r.endpoints))
	copy(out, r.endpoints)
	return out
}
