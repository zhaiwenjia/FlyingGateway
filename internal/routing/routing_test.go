// 日期: 2026-11-14 14:13:45.676
// 作者: Zhai Wenjia

package routing

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// TestCheckContextExceededSide 断言上下文越界侧（统一 SDD 11.3「路由-上下文越界」行）：
// 估算输入 + max_output_tokens > context_window ⇒ 报 context_exceeded 类错误
// （*ContextExceededError、Category()==core.ErrContextExceeded、errors.Is 命中哨兵）。
// 对照点：实现返回普通 error（无 Category）或不判越界 ⇒ 本测试红。
func TestCheckContextExceededSide(t *testing.T) {
	err := CheckContext(100, 50, 128)
	if err == nil {
		t.Fatalf("期望越界报错，实际 nil（⛔ 不静默截断）")
	}
	if !errors.Is(err, ErrContextExceeded) {
		t.Errorf("err = %v, want errors.Is ErrContextExceeded", err)
	}
	var ce *ContextExceededError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ContextExceededError", err)
	}
	if ce.Category() != core.ErrContextExceeded {
		t.Errorf("Category() = %v, want %v（context_exceeded 类）", ce.Category(), core.ErrContextExceeded)
	}
	if ce.EstimatedInputTokens != 100 || ce.MaxOutputTokens != 50 || ce.ContextWindow != 128 {
		t.Errorf("错误字段回填不符: %+v", ce)
	}
}

// TestCheckContextWithinLimit 断言未越界侧放行（统一 SDD 11.3「路由-上下文越界」行）。
// 边界口径：恰好等于 context_window 不算越界（判定是严格大于）。
// 对照点：实现用 >= 判越界 ⇒ 恰好相等的子例红。
func TestCheckContextWithinLimit(t *testing.T) {
	cases := []struct {
		name              string
		input, output, cw int
	}{
		{"严格小于", 78, 50, 128},
		{"恰好等于不越界", 100, 28, 128},
		{"零输出", 128, 0, 128},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckContext(tc.input, tc.output, tc.cw); err != nil {
				t.Errorf("CheckContext(%d,%d,%d) = %v, want nil", tc.input, tc.output, tc.cw, err)
			}
		})
	}
}

// TestCheckContextInvalidArgs 断言非法入参显式报错（⛔ 不拿非法值绕过判定）。
func TestCheckContextInvalidArgs(t *testing.T) {
	cases := []struct {
		name              string
		input, output, cw int
	}{
		{"负输入", -1, 50, 128},
		{"负输出", 100, -1, 128},
		{"非正窗口", 100, 50, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckContext(tc.input, tc.output, tc.cw)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("err = %v, want errors.Is ErrInvalidArgument", err)
			}
		})
	}
}

// testChains 是全局降级链样例（键=失败类型，值=有序端点名数组，统一 SDD 7.4）。
var testChains = map[FailureKind][]string{
	FailGeneral:       {"backup", "local"},
	FailContextWindow: {"large-context"},
	FailContentPolicy: {"backup"},
	FailBudget:        {"budget-holder"},
	FailRateLimit:     {"backup", "local"},
	FailTimeout:       {"backup"},
	FailUpstreamError: {"backup", "local"},
}

// TestSelectChainAllSevenFailureKinds 断言七类失败类型各自命中自己的链（统一 SDD 7.4
// 失败类型清单逐字）：输出为该类型下的显式有序降级链（不含主端点）。
// 对照点：实现把某一类映射到别的链或丢掉某类 ⇒ 对应子例红。
func TestSelectChainAllSevenFailureKinds(t *testing.T) {
	cases := []struct {
		kind FailureKind
		want []string
	}{
		{FailGeneral, []string{"backup", "local"}},
		{FailContextWindow, []string{"large-context"}},
		{FailContentPolicy, []string{"backup"}},
		{FailBudget, []string{"budget-holder"}},
		{FailRateLimit, []string{"backup", "local"}},
		{FailTimeout, []string{"backup"}},
		{FailUpstreamError, []string{"backup", "local"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			got, err := SelectChain(testChains, "primary", tc.kind)
			if err != nil {
				t.Fatalf("SelectChain(kind=%q) 报错: %v", tc.kind, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("chain = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSelectChainNoChainExplicitError 断言无链显式报错（统一 SDD 7.4）：
// 某类型无降级端点 ⇒ *NoChainError，⛔ 不静默回落到通用链；
// context_window 无链时消息含「上下文超限且无可用的大窗口端点」（7.4 逐字）。
// 对照点：实现拿 general 兜底或返回空链 nil 错误 ⇒ 本测试红。
func TestSelectChainNoChainExplicitError(t *testing.T) {
	// 只有 general 链：其余六类都必须显式报错，⛔ 不许吃到 general 的链。
	generalOnly := map[FailureKind][]string{FailGeneral: {"backup", "local"}}
	for _, kind := range []FailureKind{
		FailContextWindow, FailContentPolicy, FailBudget,
		FailRateLimit, FailTimeout, FailUpstreamError,
	} {
		t.Run(string(kind), func(t *testing.T) {
			got, err := SelectChain(generalOnly, "primary", kind)
			if err == nil {
				t.Fatalf("kind=%q 无链却返回 nil 错误，got chain=%v（⛔ 不静默回落通用链）", kind, got)
			}
			if !errors.Is(err, ErrNoChain) {
				t.Errorf("err = %v, want errors.Is ErrNoChain", err)
			}
			var nc *NoChainError
			if !errors.As(err, &nc) {
				t.Errorf("err = %v, want *NoChainError", err)
			} else if nc.Kind != kind || nc.Primary != "primary" {
				t.Errorf("NoChainError = %+v, want Kind=%q Primary=primary", nc, kind)
			}
			if got != nil {
				t.Errorf("报错时须返回 nil 链，got %v", got)
			}
		})
	}
	t.Run("context_window 无链告知无大窗口端点", func(t *testing.T) {
		_, err := SelectChain(map[FailureKind][]string{}, "primary", FailContextWindow)
		if err == nil {
			t.Fatalf("期望报错，实际 nil")
		}
		if !strings.Contains(err.Error(), "上下文超限且无可用的大窗口端点") {
			t.Errorf("消息 %q 须含「上下文超限且无可用的大窗口端点」（统一 SDD 7.4 逐字）", err.Error())
		}
	})
	t.Run("空数组同无链", func(t *testing.T) {
		_, err := SelectChain(map[FailureKind][]string{FailTimeout: {}}, "primary", FailTimeout)
		if !errors.Is(err, ErrNoChain) {
			t.Errorf("err = %v, want errors.Is ErrNoChain（空数组 = 无降级端点）", err)
		}
	})
	t.Run("链只含主端点同无链", func(t *testing.T) {
		_, err := SelectChain(map[FailureKind][]string{FailTimeout: {"primary"}}, "primary", FailTimeout)
		if !errors.Is(err, ErrNoChain) {
			t.Errorf("err = %v, want errors.Is ErrNoChain（剔除主端点后为空 = 无降级端点）", err)
		}
	})
}

// TestSelectChainFiltersPrimary 断言主端点从降级链中剔除且顺序保持
// （全局链表可能包含主端点；主端点已失败，⛔ 不回到自己的降级链里形成兜底循环）。
// 对照点：实现照抄链表（含主端点）⇒ 子例 1 红；实现打乱顺序 ⇒ 子例 2 红。
func TestSelectChainFiltersPrimary(t *testing.T) {
	t.Run("链含主端点被剔除", func(t *testing.T) {
		chains := map[FailureKind][]string{FailGeneral: {"backup", "primary", "local"}}
		got, err := SelectChain(chains, "primary", FailGeneral)
		if err != nil {
			t.Fatalf("SelectChain 报错: %v", err)
		}
		if want := []string{"backup", "local"}; !reflect.DeepEqual(got, want) {
			t.Errorf("chain = %v, want %v", got, want)
		}
	})
	t.Run("链不含主端点原序返回副本", func(t *testing.T) {
		chains := map[FailureKind][]string{FailGeneral: {"backup", "local"}}
		got, err := SelectChain(chains, "primary", FailGeneral)
		if err != nil {
			t.Fatalf("SelectChain 报错: %v", err)
		}
		if want := []string{"backup", "local"}; !reflect.DeepEqual(got, want) {
			t.Errorf("chain = %v, want %v", got, want)
		}
		got[0] = "mutated"
		if chains[FailGeneral][0] != "backup" {
			t.Errorf("返回值须是副本，修改返回值影响了原链表: %v", chains[FailGeneral])
		}
	})
}

// TestSelectChainInvalidArgs 断言非法入参显式报错。
func TestSelectChainInvalidArgs(t *testing.T) {
	t.Run("空 primary", func(t *testing.T) {
		_, err := SelectChain(testChains, "", FailGeneral)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("err = %v, want errors.Is ErrInvalidArgument", err)
		}
	})
	t.Run("未知失败类型", func(t *testing.T) {
		_, err := SelectChain(testChains, "primary", FailureKind("bogus"))
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("err = %v, want errors.Is ErrInvalidArgument", err)
		}
	})
	t.Run("链含空端点名", func(t *testing.T) {
		_, err := SelectChain(map[FailureKind][]string{FailGeneral: {"backup", ""}}, "primary", FailGeneral)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("err = %v, want errors.Is ErrInvalidArgument", err)
		}
	})
}

// TestParseFailureKind 断言七类逐字可解析、其余值显式报错。
func TestParseFailureKind(t *testing.T) {
	for _, s := range []string{
		"general", "context_window", "content_policy", "budget",
		"rate_limit", "timeout", "upstream_error",
	} {
		if got, err := ParseFailureKind(s); err != nil || string(got) != s {
			t.Errorf("ParseFailureKind(%q) = %q, %v; want 原值 nil 错误", s, got, err)
		}
	}
	for _, bad := range []string{"", "General", "fallback", "invalid_request"} {
		if _, err := ParseFailureKind(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ParseFailureKind(%q) err = %v, want errors.Is ErrInvalidArgument", bad, err)
		}
	}
}

// TestAttemptCountersFallback 断言降级计数：降级换端点记一次（统一 SDD 7.4）。
// 同端点重试已移除（gateway 不重试，模型调用重试由 harness 系统控制），计数器只有级内项。
func TestAttemptCountersFallback(t *testing.T) {
	var c AttemptCounters
	c.RecordFallback()
	if c.FallbackCount != 1 {
		t.Errorf("FallbackCount = %d, want 1", c.FallbackCount)
	}
}
