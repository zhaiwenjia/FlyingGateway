// 日期: 2026-11-14 14:13:45.588
// 作者: Zhai Wenjia

package routing

import (
	"errors"
	"fmt"
)

// FailureKind 是失败类型（统一 SDD 7.4「按失败类型分链」，七类逐字照抄）：
// 输入为主端点 + 失败类型，输出为该类型下的显式有序降级链。
type FailureKind string

const (
	// FailGeneral 是通用失败（无更具体归因时）。
	FailGeneral FailureKind = "general"
	// FailContextWindow 是上下文越界（走 context_window 专用链）。
	FailContextWindow FailureKind = "context_window"
	// FailContentPolicy 是内容策略拒绝（走 content_policy 专用链）。
	FailContentPolicy FailureKind = "content_policy"
	// FailBudget 是预算触顶（统一 SDD 2.3 错误分类表：触顶即拒、不降级，
	// 但仍占一个失败类型，链通常为空 ⇒ 无链显式报错）。
	FailBudget FailureKind = "budget"
	// FailRateLimit 是限流（可退避重试、可换端点降级）。
	FailRateLimit FailureKind = "rate_limit"
	// FailTimeout 是超时。
	FailTimeout FailureKind = "timeout"
	// FailUpstreamError 是上游 5xx / 连接失败。
	FailUpstreamError FailureKind = "upstream_error"
)

// ParseFailureKind 解析失败类型字符串（供注册表校验 fallback_chains 的键）。
// 未知值 ⇒ 显式报错（⛔ 不静默当 general 处理）。
func ParseFailureKind(s string) (FailureKind, error) {
	switch k := FailureKind(s); k {
	case FailGeneral, FailContextWindow, FailContentPolicy,
		FailBudget, FailRateLimit, FailTimeout, FailUpstreamError:
		return k, nil
	default:
		return "", fmt.Errorf("%w: 失败类型 %q 不在七类之列（general/context_window/content_policy/budget/rate_limit/timeout/upstream_error）",
			ErrInvalidArgument, s)
	}
}

// ErrNoChain 是「该失败类型无降级端点」的哨兵错误（可 errors.Is 判定）。
var ErrNoChain = errors.New("routing: 该失败类型无降级端点")

// NoChainError 是某失败类型无降级端点的类型化错误（统一 SDD 7.4）：
// 某类型无降级端点 ⇒ 显式报错，⛔ 不静默回落到通用链。
type NoChainError struct {
	Kind    FailureKind
	Primary string
}

func (e *NoChainError) Error() string {
	if e.Kind == FailContextWindow {
		// 统一 SDD 7.4：context_window 无链时告知「上下文超限且无可用的大窗口端点」。
		return fmt.Sprintf(
			"上下文超限且无可用的大窗口端点（主端点 %q 无 context_window 降级链，⛔ 不静默回落通用链）",
			e.Primary)
	}
	return fmt.Sprintf(
		"失败类型 %q 无降级端点（主端点 %q，⛔ 不静默回落通用链）",
		e.Kind, e.Primary)
}

// Unwrap 让 errors.Is 命中 ErrNoChain。
func (e *NoChainError) Unwrap() error { return ErrNoChain }

// SelectChain 按失败类型取降级链（统一 SDD 7.4「按失败类型分链」）。
//
// 输入主端点 + 失败类型，输出该类型下的显式有序降级链（待降级端点，不含主端点）：
// chains[kind] 是全局链表（注册表 fallback_chains，键=失败类型），可能包含主端点本身
// ⇒ 主端点已失败，从链中剔除（⛔ 不让主端点回到自己的降级链里形成兜底循环）。
//
// 边界口径（统一 SDD 7.4）：
//   - 某类型无降级端点（键缺失 / 空数组 / 剔除主端点后为空）⇒ 返回 *NoChainError，
//     ⛔ 不静默回落到通用链；
//   - kind 不在七类之列 ⇒ 显式报错（ErrInvalidArgument）；
//   - primary 为空或链内含空端点名 ⇒ 显式报错（ErrInvalidArgument）。
//
// 返回值是副本，调用方修改不影响 chains（路由决策可复现，统一 SDD 1.4.3）。
func SelectChain(chains map[FailureKind][]string, primary string, kind FailureKind) ([]string, error) {
	if primary == "" {
		return nil, fmt.Errorf("%w: primary 端点名为空", ErrInvalidArgument)
	}
	if _, err := ParseFailureKind(string(kind)); err != nil {
		return nil, err
	}
	chain, ok := chains[kind]
	if !ok || len(chain) == 0 {
		return nil, &NoChainError{Kind: kind, Primary: primary}
	}
	out := make([]string, 0, len(chain))
	for _, name := range chain {
		if name == "" {
			return nil, fmt.Errorf("%w: 失败类型 %q 的降级链含空端点名", ErrInvalidArgument, kind)
		}
		if name == primary {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		// 链只含主端点自己 ⇒ 同样是「无降级端点」（统一 SDD 7.4 显式报错）。
		return nil, &NoChainError{Kind: kind, Primary: primary}
	}
	return out, nil
}

// AttemptCounters 是降级次数计数（统一 SDD 7.4）：降级换端点（级间），上限 = 降级链长度。
// 同端点重试已移除（gateway ⛔ 不重试，模型重试由 harness 系统控制），
// 计数器不再有级内计数项。
type AttemptCounters struct {
	// FallbackCount 是降级次数（换端点）。
	FallbackCount int
}

// RecordFallback 记一次降级（换端点）。
func (c *AttemptCounters) RecordFallback() { c.FallbackCount++ }
