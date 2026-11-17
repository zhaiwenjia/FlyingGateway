// 日期: 2026-11-14 11:50:04.691
// 作者: Zhai Wenjia

package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/share"
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
// 双模式：内存模式（store == nil）/ 共享模式（累计存 share.Store 跨实例一份预算；
// 共享键 = 前缀 + client 的 SHA-256 短哈希 —— ⛔ 不把调用方 key 明文写进存储键）。
// 共享模式故障口径：存储不可用 ⇒ Allow fail-open 放行 + 显式告警（⛔ 不静默）。
//
// 并发安全：Allow / Record 可并发调用。
type Budget struct {
	mu    sync.Mutex
	limit int64
	used  map[string]int64

	store        share.Store
	keyPrefix    string
	onShareError func(error)
}

// NewBudget 构造预算器。limitTokens <= 0 属配置错误，直接 panic
// （⛔ 不静默按 0 处理 —— 那会把「全部拒绝」伪装成正常运行）。
func NewBudget(limitTokens int64) *Budget {
	if limitTokens <= 0 {
		panic("policy: Budget 上限必须 > 0")
	}
	return &Budget{limit: limitTokens, used: make(map[string]int64)}
}

// NewSharedBudget 构造共享模式预算器（累计存 store，跨实例一份预算）。
func NewSharedBudget(store share.Store, keyPrefix string, onShareError func(error), limitTokens int64) *Budget {
	if store == nil {
		panic("policy: NewSharedBudget 的 store 不得为 nil")
	}
	if limitTokens <= 0 {
		panic("policy: Budget 上限必须 > 0")
	}
	if keyPrefix == "" {
		keyPrefix = "fg:budget:"
	}
	return &Budget{limit: limitTokens, store: store, keyPrefix: keyPrefix, onShareError: onShareError}
}

// shareKey 是共享存储键（client 短哈希脱敏，⛔ 明文入键）。
func (b *Budget) shareKey(client string) string {
	sum := sha256.Sum256([]byte(client))
	return b.keyPrefix + hex.EncodeToString(sum[:8])
}

// Allow 是上游请求之前的预算闸：预估消耗会使该调用方超出上限 ⇒ 返回
// *BudgetExceededError（⛔ 此后不得发起任何上游调用）。
// 恰好用满上限放行，超出才拒。
//
// 共享模式：读判（Get + 预估）——读与记账非原子，跨实例存在小幅超发窗口
// （业界「限流是近似值」同款取舍；硬闸由 Record 的超限回滚记账兜底）。
func (b *Budget) Allow(client string, estimatedTokens int64) error {
	if estimatedTokens < 0 {
		return fmt.Errorf("policy: estimatedTokens 不得为负：%d", estimatedTokens)
	}
	if b.store != nil {
		used, err := b.store.Get(b.shareKey(client))
		if err != nil {
			if b.onShareError != nil {
				b.onShareError(err)
			}
			return nil // fail-open：预算存储不可用时放行 + 显式告警（⛔ 不静默）
		}
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
// 共享模式：IncrBy 无上限记账（费用已产生，⛔ 不回滚事实；超限由 Allow 前置闸拦）。
func (b *Budget) Record(client string, actualTokens int64) {
	if actualTokens <= 0 {
		return
	}
	if b.store != nil {
		if _, err := b.store.IncrBy(b.shareKey(client), actualTokens, 0); err != nil && b.onShareError != nil {
			b.onShareError(err)
		}
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used[client] += actualTokens
}
