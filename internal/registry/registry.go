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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"

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
	// MaxRetries 是重试次数上限（⛔ 与降级次数分开计，统一 SDD 7.4）。
	MaxRetries int `json:"max_retries"`
	// Priority 是优先级，数值越小越优先。
	Priority int `json:"priority"`
}

// Registry 是端点注册表（统一 SDD 9.1 / 9.2）。
type Registry struct {
	// Strategy 是路由策略：priority（按 Priority 选）/ round_robin（轮转）。
	Strategy string
	// FallbackChains 是按失败类型的降级链（统一 SDD 7.4；实现计划偏差 4）：
	// 键 = 失败类型，值 = 有序端点名数组（不含主端点语义由 routing.SelectChain 处理）。
	FallbackChains map[routing.FailureKind][]string

	endpoints []Endpoint             // 确定性排序后的全量端点
	byModel   map[string][]*Endpoint // model 别名 → 候选端点（同 endpoints 排序）
	rrCounter atomic.Uint64          // round_robin 轮转计数（显式计数器驱动，⛔ 不掺随机 / 时间）
}

// fileWire 是配置文件顶层结构（统一 SDD 9.2）。
type fileWire struct {
	Strategy       string              `json:"strategy"`
	Endpoints      []Endpoint          `json:"endpoints"`
	FallbackChains map[string][]string `json:"fallback_chains"`
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
//     timeout_s > 0、max_output_tokens ≥ 0、max_retries ≥ 0（负数 / 零超时无语义）；
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
	if ep.MaxRetries < 0 {
		return fmt.Errorf("%s：%q 的 max_retries = %d 不得为负", prefix, ep.Name, ep.MaxRetries)
	}
	if ep.BaseURL != "" {
		if !strings.Contains(ep.BaseURL, "://") {
			return fmt.Errorf("%s：%q 的 base_url %q 须形如 scheme://host[/path]", prefix, ep.Name, ep.BaseURL)
		}
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
