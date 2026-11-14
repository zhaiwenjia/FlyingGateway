// 日期: 2026-11-14 11:50:04.844
// 作者: Zhai Wenjia

package policy

import (
	stderrors "errors"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// TestBudget_BeforeUpstreamCall 断言预算触顶在发起上游请求之前拒绝
// （统一 SDD 11.3「预算触顶」：上游调用计数 == 0）。
// 观测量 = Allow 返回错误后 upstreamCalls 仍为 0（Allow 是上游调用前的闸，
// 错误实现若放行后才计费，upstreamCalls 会变非 0 ⇒ 红）。
func TestBudget_BeforeUpstreamCall(t *testing.T) {
	b := NewBudget(100)
	upstreamCalls := 0
	tryRequest := func(client string, est int64) error {
		if err := b.Allow(client, est); err != nil {
			return err // ⛔ 触顶即拒，不得发起上游调用
		}
		upstreamCalls++
		b.Record(client, est)
		return nil
	}

	if err := tryRequest("client-a", 120); err == nil {
		t.Fatal("预估 120 > 上限 100 应拒绝")
	}
	if upstreamCalls != 0 {
		t.Fatalf("预算触顶后仍有上游调用：upstreamCalls=%d，期望 0", upstreamCalls)
	}
	var be *BudgetExceededError
	err := tryRequest("client-a", 120)
	if !stderrors.As(err, &be) {
		t.Fatalf("期望 *BudgetExceededError，实际 %v", err)
	}
	if be.Category != core.ErrBudgetExceeded {
		t.Fatalf("Category = %s, 期望 budget_exceeded", be.Category)
	}
	if be.LimitTokens != 100 || be.RequestedTokens != 120 {
		t.Fatalf("错误对象字段失真：limit=%d requested=%d", be.LimitTokens, be.RequestedTokens)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstreamCalls=%d, 期望保持 0", upstreamCalls)
	}
}

// TestBudget_RecordAndExhaust 断言 Record 累计实际消耗、触顶后 Allow 报错。
// 断言有效性：Record 不累计的错误实现会红（第二次 Allow(50) 会错误放行）。
func TestBudget_RecordAndExhaust(t *testing.T) {
	b := NewBudget(100)
	if err := b.Allow("c", 60); err != nil {
		t.Fatalf("Allow(60) 不应报错：%v", err)
	}
	b.Record("c", 60)
	if err := b.Allow("c", 50); err == nil {
		t.Fatal("已用 60 + 预估 50 > 100 应报错")
	}
	// 恰好用满上限放行。
	if err := b.Allow("c", 40); err != nil {
		t.Fatalf("已用 60 + 预估 40 = 100 恰好用满，应放行：%v", err)
	}
}

// TestBudget_PerClientIsolation 断言每调用方独立预算（统一 SDD 8.3）。
// 断言有效性：全局共享预算的错误实现会红（client-b 会被 client-a 拖垮）。
func TestBudget_PerClientIsolation(t *testing.T) {
	b := NewBudget(100)
	b.Record("client-a", 100)
	if err := b.Allow("client-a", 1); err == nil {
		t.Fatal("client-a 触顶后 Allow 应报错")
	}
	if err := b.Allow("client-b", 100); err != nil {
		t.Fatalf("client-b 独立预算应放行：%v", err)
	}
}

// TestBudget_NegativeRejected 断言负数预估显式报错（usage 不产生负数 token）。
func TestBudget_NegativeRejected(t *testing.T) {
	b := NewBudget(100)
	if err := b.Allow("c", -1); err == nil {
		t.Fatal("负数预估应报错")
	}
}
