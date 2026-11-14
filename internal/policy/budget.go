// 日期: 2026-11-14 11:50:04.691
// 作者: Zhai Wenjia

package policy

import (
	"fmt"
	"sync"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// BudgetExceededError 是预算触顶的类型化错误（统一 SDD 7.2 / 8.3）。
// 上层映射 `429 budget_exceeded`；⛔ 触顶即拒（不做只报表的软限）。
type BudgetExceededError struct {
	Client          string
	LimitTokens     int64
	UsedTokens      int64
	RequestedTokens int64
	// Category 固定 core.ErrBudgetExceeded，便于上层统一映射。
	Category core.ErrorCategory
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("policy: budget_exceeded: 调用方 %s 预算触顶（上限 %d，已用 %d，本次预估 %d）",
		e.Client, e.LimitTokens, e.UsedTokens, e.RequestedTokens)
}

// Budget 是每调用方的 token 预算（统一 SDD 7.2）：
// 触顶即拒，⛔ 且必须在发起上游请求之前拒绝（上游调用计数保持 0）。
// 计数口径：归一化 token（core.Usage.NormalizedTokens）。
//
// 并发安全：Allow / Record 可并发调用。
type Budget struct {
	mu    sync.Mutex
	limit int64
	used  map[string]int64
}

// NewBudget 构造预算器。limitTokens <= 0 属配置错误，直接 panic
// （⛔ 不静默按 0 处理 —— 那会把「全部拒绝」伪装成正常运行）。
func NewBudget(limitTokens int64) *Budget {
	if limitTokens <= 0 {
		panic("policy: Budget 上限必须 > 0")
	}
	return &Budget{limit: limitTokens, used: make(map[string]int64)}
}

// Allow 是上游请求之前的预算闸：预估消耗会使该调用方超出上限 ⇒ 返回
// *BudgetExceededError（⛔ 此后不得发起任何上游调用）。
// 恰好用满上限放行，超出才拒。
func (b *Budget) Allow(client string, estimatedTokens int64) error {
	if estimatedTokens < 0 {
		return fmt.Errorf("policy: estimatedTokens 不得为负：%d", estimatedTokens)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	used := b.used[client]
	if used+estimatedTokens > b.limit {
		return &BudgetExceededError{
			Client:          client,
			LimitTokens:     b.limit,
			UsedTokens:      used,
			RequestedTokens: estimatedTokens,
			Category:        core.ErrBudgetExceeded,
		}
	}
	return nil
}

// Record 在请求结束后记实际消耗（归一化 token）。
// 非正数不计（usage 不产生负数 token，统一 SDD 3.2）；
// 实际消耗可能超过预估（预估偏小时），照实累计 —— 上限判定下次 Allow 生效。
func (b *Budget) Record(client string, actualTokens int64) {
	if actualTokens <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used[client] += actualTokens
}
