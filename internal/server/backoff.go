// 日期: 2026-11-16 22:44:50.162
// 作者: Zhai Wenjia

package server

import (
	"context"
	"math"
	"time"
)

// 本文件承载同端点重试的截断指数退避 + 抖动（统一 SDD 7.4 退避口径）。
//
// 算法口径（业界推荐的 truncated exponential backoff with jitter，Google Cloud
// 「Retry strategy / Exponential backoff」文档逐字口径）：
// 第 n 次重试前等待 min(Base·2^(n-1) + U[0,Jitter), Cap) —— 抖动为 [0, Jitter) 均匀
// 随机小数、每次重算（防多客户端同步重试的 thundering herd），整体截断在 Cap。
// 默认参数取同源推荐值：Base=1s、Jitter≤1s、Cap=32s（原文「Typical values are
// 32 or 64 seconds」取下限）。
//
// ⛔ 429 不走本退避：上游 Retry-After / 默认冷却已在令牌池承载等待语义
//（respect Retry-After 优先于自算退避，Microsoft Learn / Atlassian 同口径），
// 叠加退避会把同一限流事件拖成「冷却等待 + 退避等待」双重等待。
// 5xx / 超时没有恢复信号，才用指数退避给上游喘息空间。

// Backoff 是同端点重试的退避参数。
// **整体零值** ⇒ 全默认（Base=1s、Jitter=1s、Cap=32s）；只要显式设了任一字段，
// 未设字段按 0 处理（Jitter=0 = 不抖动、Base/Cap ≤ 0 取默认）——
// ⛔ 不把「显式 Jitter=0」误补成默认抖动（测试注入确定性参数的通道）。
type Backoff struct {
	Base   time.Duration // 初始等待（默认 1s）
	Jitter time.Duration // 抖动上限，取 [0, Jitter)（默认 1s）
	Cap    time.Duration // 单次等待上限（默认 32s）
}

// 默认参数（来源见文件头：Google Cloud truncated exponential backoff 推荐值）。
const (
	defaultBackoffBase   = time.Second
	defaultBackoffJitter = time.Second
	defaultBackoffCap    = 32 * time.Second
)

// withDefaults 返回补齐默认后的副本（口径见 Backoff 注释）。
func (b Backoff) withDefaults() Backoff {
	if b == (Backoff{}) {
		return Backoff{Base: defaultBackoffBase, Jitter: defaultBackoffJitter, Cap: defaultBackoffCap}
	}
	if b.Base <= 0 {
		b.Base = defaultBackoffBase
	}
	if b.Jitter < 0 {
		b.Jitter = 0
	}
	if b.Cap <= 0 {
		b.Cap = defaultBackoffCap
	}
	if b.Cap < b.Base {
		b.Cap = b.Base
	}
	return b
}

// Wait 返回第 n 次重试前的等待时长（n ≥ 1；n=1 ⇒ 第一次重试）。
// rnd 返回 [0,1) 的均匀随机小数，由调用方注入（生产用 math/rand 全局源；
// 测试注入定值做确定性断言 —— 参数化随机源，⛔ 不 mock 时钟）。
// rnd 为 nil ⇒ 不加抖动。
//
// 公式（逐字照抄 Google Cloud 口径）：wait = min(Base·2^(n-1) + rnd()·Jitter, Cap)。
func (b Backoff) Wait(n int, rnd func() float64) time.Duration {
	b = b.withDefaults()
	if n < 1 {
		n = 1
	}
	// 2^(n-1) 先于乘法溢出保护：指数级超界直接截断到 Cap（等价 min 语义）。
	exp := math.Pow(2, float64(n-1))
	base := float64(b.Base) * exp
	if math.IsInf(exp, 0) || base >= float64(b.Cap) {
		return b.Cap
	}
	var jitter float64
	if b.Jitter > 0 && rnd != nil {
		f := rnd()
		if f < 0 {
			f = 0
		}
		if f >= 1 {
			f = 0.999999999
		}
		jitter = f * float64(b.Jitter)
	}
	w := base + jitter
	if w >= float64(b.Cap) {
		return b.Cap
	}
	return time.Duration(w)
}

// waitSleep 可取消等待：ctx 取消（客户端断连 / 显式取消 / 进程关闭）⇒ 立即返回
// ctx.Err()，由调用方走取消收口（统一 SDD 7.5：取消 ⛔ 不再触发重试）。
func waitSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
