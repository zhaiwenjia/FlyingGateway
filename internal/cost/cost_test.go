// 日期: 2026-11-14 14:05:31.546
// 作者: Zhai Wenjia

package cost

import (
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// 本文件覆盖费用计算口径（SDD 5.2 公式 / 5.3 峰谷 / 5.6 来源优先级 / 5.7 伪代码与舍入口径）。
// 真实行（deepseek-flash / qwen3.8-* / grok-4.7）从 config/rates.json 加载对账；
// 夹具行只用于钉计算逻辑（SDD 5.4 无对应行，价格不是费率断言），每处已标注。

// TestRound4 钉住舍入口径（5.7）：math.Round(v*1e4)/1e4 先乘后取整。
// 边界值 0.12345001 / 0.12344999 分居最小单位两侧 ⇒ 各自进位 / 舍去；
// 1.00005 半值按 round-half-away-from-zero 进位到 1.0001。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestRound4 -v
func TestRound4(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{0.12345001, 0.1235},
		{0.12344999, 0.1234},
		{1.00005, 1.0001},
		{0, 0},
		{8.61, 8.61},
	} {
		if got := round4(c.in); got != c.want {
			t.Fatalf("round4(%v) = %v，期望 %v（5.7：先乘 1e4 再取整，⛔ 不先除后乘）", c.in, got, c.want)
		}
	}
}

// TestCalculateCostRoundsTotalOnce 钉住「总费用求和后舍入一次」（5.2 / 5.7）：
// 四类分项全精度各 2×80/1e6 = 1.6e-4 元（展示值各自 round4 = 0.0002），
// 全精度和 6.4e-4 ⇒ CostCNY = round4 = 0.0006、CostUSD = round4(6.4e-4/7) = 0.0001。
// ⛔ 分项先舍入再相加会得 4×0.0002 = 0.0008 ⇒ 错误实现会红（恒假样本已核）。
// 夹具行四价同值 80（SDD 5.4 无此行）：只钉舍入口径，不是费率对账。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestCalculateCostRoundsTotalOnce -v
func TestCalculateCostRoundsTotalOnce(t *testing.T) {
	rate := &ModelRate{
		Model: "fixture-round", Provider: "qwen",
		InputPriceCNY: 80, CacheReadExplicitCNY: 80, CacheReadImplicitCNY: 80,
		CacheWriteCNY: 80, OutputPriceCNY: 80,
		PeakMultiplier: 1.0, SnapshotDate: "2026-11-13",
	}
	u := &core.Usage{
		InputTokens: 8, InputTokensUncached: 2, CacheReadTokens: 2,
		CacheWriteTokens: 2, OutputTokens: 2,
	}
	c := CalculateCost(u, rate, time.Date(2026, 11, 16, 10, 0, 0, 0, beijing))
	if c.CostCNY != 0.0006 {
		t.Fatalf("CostCNY = %v，期望 0.0006（全精度和 6.4e-4 舍入一次；⛔ 分项先舍入再相加会得 0.0008）", c.CostCNY)
	}
	for name, got := range map[string]float64{
		"InputCostCNY": c.InputCostCNY, "CacheReadCostCNY": c.CacheReadCostCNY,
		"CacheWriteCostCNY": c.CacheWriteCostCNY, "OutputCostCNY": c.OutputCostCNY,
	} {
		if got != 0.0002 {
			t.Fatalf("%s = %v，期望 0.0002（明细展示值各自 round4（1.6e-4））", name, got)
		}
	}
	if c.CostUSD != 0.0001 {
		t.Fatalf("CostUSD = %v，期望 0.0001 = round4(6.4e-4/7)（5.2：CostUSD = round4(S/7)）", c.CostUSD)
	}
}

// TestCalculateCostPeakOffpeak 钉住峰谷判定与倍率（5.3，真实 deepseek-flash 行）：
// 北京时间周一 10:00（峰时窗口内）⇒ m=2、PeakApplied=true、RateSnapshot 时段段为 -peak-；
// 周一 13:30（午休，窗口外）⇒ m=1、-offpeak-；国庆 2026-10-01 10:00（法定节假日）⇒ 谷时。
// 数字逐字：deepseek-flash 谷 输入 ¥1.0000 / 命中 ¥0.0200 / 输出 ¥4.0000（5.4.1），峰 = 谷 × 2。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestCalculateCostPeakOffpeak -v
func TestCalculateCostPeakOffpeak(t *testing.T) {
	rate := loadTestRates(t).Lookup("deepseek-flash")
	if rate == nil {
		t.Fatal("缺 deepseek-flash 行（5.4.1）")
	}
	u := &core.Usage{
		InputTokens: 20000, InputTokensUncached: 10000, CacheReadTokens: 10000, OutputTokens: 1000,
	}

	// 谷时（周一 13:30，9:00–12:00 / 14:00–18:00 之外）：
	// in = 10000×1.0000/1e6 = 0.01；read = 10000×0.0200/1e6 = 0.0002；out = 1000×4.0000/1e6 = 0.004。
	off := CalculateCost(u, rate, time.Date(2026, 11, 16, 13, 30, 0, 0, beijing))
	if off.PeakApplied {
		t.Fatal("周一 13:30 是谷时（5.3：其余时段谷），PeakApplied 应为 false")
	}
	if off.CostCNY != 0.0142 {
		t.Fatalf("谷时 CostCNY = %v，期望 0.0142（0.01 + 0.0002 + 0.004）", off.CostCNY)
	}
	if off.CostUSD != 0.002 {
		t.Fatalf("谷时 CostUSD = %v，期望 0.002 = round4(0.0142/7)（5.2）", off.CostUSD)
	}
	if off.InputCostCNY != 0.01 || off.CacheReadCostCNY != 0.0002 || off.OutputCostCNY != 0.004 {
		t.Fatalf("谷时明细不对：in=%v(0.01) read=%v(0.0002) out=%v(0.004)",
			off.InputCostCNY, off.CacheReadCostCNY, off.OutputCostCNY)
	}
	if off.Source != core.CostEstimated {
		t.Fatalf("Source = %q，期望 estimated（5.6 ②：费率表计算）", off.Source)
	}
	if off.RateSnapshot != "deepseek-flash@2026-11-13-offpeak-cn-fixed" {
		t.Fatalf("谷时 RateSnapshot = %q，期望 deepseek-flash@2026-11-13-offpeak-cn-fixed（5.1 形如 deepseek-flash@2026-11-13-peak + 5.3 记录 holiday_calendar=cn-fixed）", off.RateSnapshot)
	}

	// 峰时（周一 10:00）：全部分项 ×2（5.3：峰谷倍率乘在全部四类分项上）。
	peak := CalculateCost(u, rate, time.Date(2026, 11, 16, 10, 0, 0, 0, beijing))
	if !peak.PeakApplied {
		t.Fatal("周一 10:00 是峰时（5.3：周一至周五 9:00–12:00），PeakApplied 应为 true")
	}
	if peak.CostCNY != 0.0284 {
		t.Fatalf("峰时 CostCNY = %v，期望 0.0284（谷 × 2 = 0.0142 × 2，5.3：峰时 = 谷时 × 2）", peak.CostCNY)
	}
	if peak.CostUSD != 0.0041 {
		t.Fatalf("峰时 CostUSD = %v，期望 0.0041 = round4(0.0284/7)", peak.CostUSD)
	}
	if peak.RateSnapshot != "deepseek-flash@2026-11-13-peak-cn-fixed" {
		t.Fatalf("峰时 RateSnapshot = %q，期望 deepseek-flash@2026-11-13-peak-cn-fixed", peak.RateSnapshot)
	}

	// 法定节假日（国庆 2026-10-01 周四 10:00）全天谷时（5.3：中国法定节假日全天均为空闲时段）。
	hol := CalculateCost(u, rate, time.Date(2026, 10, 1, 10, 0, 0, 0, beijing))
	if hol.PeakApplied {
		t.Fatal("国庆 10:00 虽在时段窗口内，但法定节假日全天谷时（5.3）")
	}
	if hol.CostCNY != 0.0142 {
		t.Fatalf("节假日 CostCNY = %v，期望 0.0142（谷价）", hol.CostCNY)
	}
}

// TestCalculateCostPeakAppliesToAllComponents 钉住 5.3「峰谷倍率乘在全部四类分项上」：
// 夹具行四价同值 1.0、PeakMultiplier=2.0，四类各 1e6 token、峰时 ⇒ 每分项 1×1×2 = 2.0 元、总 8.0。
// 错误实现只对输入/输出乘倍率 ⇒ 缓存两分项会得 1.0 ⇒ 本断言会红（恒假样本已核）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestCalculateCostPeakAppliesToAllComponents -v
func TestCalculateCostPeakAppliesToAllComponents(t *testing.T) {
	rate := &ModelRate{
		Model: "fixture-peak", Provider: "deepseek",
		InputPriceCNY: 1, CacheReadExplicitCNY: 1, CacheReadImplicitCNY: 1,
		CacheWriteCNY: 1, OutputPriceCNY: 1,
		PeakMultiplier: 2.0, SnapshotDate: "2026-11-13",
	}
	u := &core.Usage{
		InputTokens: 4e6, InputTokensUncached: 1e6, CacheReadTokens: 1e6,
		CacheWriteTokens: 1e6, OutputTokens: 1e6,
	}
	c := CalculateCost(u, rate, time.Date(2026, 11, 16, 10, 0, 0, 0, beijing))
	for name, got := range map[string]float64{
		"InputCostCNY": c.InputCostCNY, "CacheReadCostCNY": c.CacheReadCostCNY,
		"CacheWriteCostCNY": c.CacheWriteCostCNY, "OutputCostCNY": c.OutputCostCNY,
	} {
		if got != 2.0 {
			t.Fatalf("%s = %v，期望 2.0（5.3：m 乘在全部四类分项上，1×1×2）", name, got)
		}
	}
	if c.CostCNY != 8.0 {
		t.Fatalf("CostCNY = %v，期望 8.0（四类 2.0 × 4）", c.CostCNY)
	}
}

// TestCalculateCostTierSelection 钉住阶梯计费（5.2 计价口径 6 / 5.4.13 选档规则）：
//   - 选档依据 = usage.InputTokens（总输入），⛔ 不按 InputTokensUncached；
//   - 整请求同价（⛔ 不分段）且输出价随档变（5.4.9：qwen3.7-flash 0–32K 输出 ¥0.8000、
//     256K–1M 输出 ¥4.8000，6 倍 —— 两档输出价逐字取自 SDD）；
//   - 档位上限含（5.4.13：MaxInputTokens 含）；超出所有档位 ⇒ 回落行级单价，⛔ 不猜档外价；
//   - 双维度档（MaxOutputTokens > 0）按输入 × 输出双维度落档（5.4.13）。
//
// ⚠️ 档位输入价 1.0 / 2.0 是选档逻辑夹具（SDD 5.4 未列 qwen3.7 阶梯全表），不是费率断言。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestCalculateCostTierSelection -v
func TestCalculateCostTierSelection(t *testing.T) {
	rate := &ModelRate{
		Model: "fixture-tier", Provider: "qwen",
		InputPriceCNY: 3.0, OutputPriceCNY: 12.0, // 行级回落价（夹具）
		PeakMultiplier: 1.0, SnapshotDate: "2026-11-13",
		InputTiers: []PriceTier{
			// 0–32K 档（5.4.9 逐字输出价 ¥0.8000）；输入价为夹具。
			{MaxInputTokens: 32000, InputPriceCNY: 1.0, OutputPriceCNY: 0.8000},
			// 256K–1M 档（5.4.9 逐字输出价 ¥4.8000）；输入价为夹具。
			{MaxInputTokens: 1000000, InputPriceCNY: 2.0, OutputPriceCNY: 4.8000},
		},
	}

	// 总输入 30000（0–32K）⇒ 档 1：out = 1000×0.8000/1e6 = 0.0008。
	c := CalculateCost(&core.Usage{InputTokens: 30000, InputTokensUncached: 30000, OutputTokens: 1000},
		rate, time.Date(2026, 11, 16, 10, 0, 0, 0, beijing))
	if c.OutputCostCNY != 0.0008 {
		t.Fatalf("30000 总输入 OutputCostCNY = %v，期望 0.0008（档 1 输出价 0.8000，5.4.9）", c.OutputCostCNY)
	}

	// 总输入 32000（档上限含，5.4.13）⇒ 仍是档 1。
	c = CalculateCost(&core.Usage{InputTokens: 32000, InputTokensUncached: 32000, OutputTokens: 1000},
		rate, time.Date(2026, 11, 16, 10, 0, 0, 0, beijing))
	if c.OutputCostCNY != 0.0008 {
		t.Fatalf("32000（上限含）OutputCostCNY = %v，期望 0.0008（5.4.13：档位上限含）", c.OutputCostCNY)
	}

	// 总输入 300000 但 Uncached 仅 1000 ⇒ 按总输入落档 2（⛔ 不按 Uncached 选档），
	// 整请求同价：out = 1000×4.8000/1e6 = 0.0048、in = 1000×2.0/1e6 = 0.002。
	// 错误实现按 Uncached=1000 落档 1 ⇒ out = 0.0008 ⇒ 本断言会红（恒假样本已核）。
	c = CalculateCost(&core.Usage{InputTokens: 300000, InputTokensUncached: 1000, OutputTokens: 1000},
		rate, time.Date(2026, 11, 16, 10, 0, 0, 0, beijing))
	if c.OutputCostCNY != 0.0048 {
		t.Fatalf("OutputCostCNY = %v，期望 0.0048（档 2 输出价 4.8000；⛔ 不按 Uncached 选档）", c.OutputCostCNY)
	}
	if c.InputCostCNY != 0.002 {
		t.Fatalf("InputCostCNY = %v，期望 0.002（1000 × 2.0/1e6，整请求同价）", c.InputCostCNY)
	}

	// 总输入 1000001 超出所有档位 ⇒ 回落行级单价 3.0 / 12.0（⛔ 不猜档外价）。
	c = CalculateCost(&core.Usage{InputTokens: 1000001, InputTokensUncached: 1000, OutputTokens: 1000},
		rate, time.Date(2026, 11, 16, 10, 0, 0, 0, beijing))
	if c.OutputCostCNY != 0.012 {
		t.Fatalf("超档 OutputCostCNY = %v，期望 0.012 = 1000×12.0/1e6（回落行级单价）", c.OutputCostCNY)
	}

	// 双维度档（5.4.13：MaxInputTokens × MaxOutputTokens 双维度落档）：
	// 输出 2000 超过档 1 的 MaxOutputTokens=1000 ⇒ 落档 2。
	dual := &ModelRate{
		Model: "fixture-tier-dual", Provider: "qwen",
		InputPriceCNY: 3.0, OutputPriceCNY: 12.0, PeakMultiplier: 1.0, SnapshotDate: "2026-11-13",
		InputTiers: []PriceTier{
			{MaxInputTokens: 32000, MaxOutputTokens: 1000, InputPriceCNY: 1.0, OutputPriceCNY: 0.8000},
			{MaxInputTokens: 1000000, InputPriceCNY: 2.0, OutputPriceCNY: 4.8000},
		},
	}
	c = CalculateCost(&core.Usage{InputTokens: 30000, InputTokensUncached: 30000, OutputTokens: 2000},
		dual, time.Date(2026, 11, 16, 10, 0, 0, 0, beijing))
	if c.OutputCostCNY != 0.0096 {
		t.Fatalf("双维度 OutputCostCNY = %v，期望 0.0096 = 2000×4.8000/1e6（输出超档 1 上限 ⇒ 档 2，5.4.13 双维度分档）", c.OutputCostCNY)
	}
}

// TestCalculateCostCacheReadMode 钉住缓存命中档位（5.7「缓存命中档位」，真实 qwen3.8-27b 行）：
// 显式命中取 CacheReadExplicitCNY = ¥0.3000、隐式命中取 CacheReadImplicitCNY = ¥0.6000（5.4.9 逐字）。
// 错误实现恒取单档 ⇒ 另一档会红（恒假样本已核）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestCalculateCostCacheReadMode -v
func TestCalculateCostCacheReadMode(t *testing.T) {
	rate := loadTestRates(t).Lookup("qwen3.8-27b")
	if rate == nil {
		t.Fatal("缺 qwen3.8-27b 行（5.4.9）")
	}
	call := time.Date(2026, 11, 16, 10, 0, 0, 0, beijing)

	exp := CalculateCost(&core.Usage{InputTokens: 1000, CacheReadTokens: 1000,
		CacheReadMode: core.CacheModeExplicit}, rate, call)
	if exp.CacheReadCostCNY != 0.0003 {
		t.Fatalf("显式命中 CacheReadCostCNY = %v，期望 0.0003 = 1000×0.3000/1e6（5.4.9 显式档）", exp.CacheReadCostCNY)
	}
	imp := CalculateCost(&core.Usage{InputTokens: 1000, CacheReadTokens: 1000,
		CacheReadMode: core.CacheModeImplicit}, rate, call)
	if imp.CacheReadCostCNY != 0.0006 {
		t.Fatalf("隐式命中 CacheReadCostCNY = %v，期望 0.0006 = 1000×0.6000/1e6（5.4.9 隐式档）", imp.CacheReadCostCNY)
	}
}

// TestCalculateCostCachePriceUnknown 钉住缓存命中价未知口径（5.2 计价口径 8，真实 qwen3.8-max 行）：
// cache_price_unknown=true ⇒ 命中费按 0 计并置 Estimated=true（⛔ 不猜价），写入价照表（¥15.0000）。
// 数字逐字：qwen3.8-max 输入 ¥12.0000 / 输出 ¥36.0000 / 写入（显式创建）¥15.0000（5.4.9）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestCalculateCostCachePriceUnknown -v
func TestCalculateCostCachePriceUnknown(t *testing.T) {
	rate := loadTestRates(t).Lookup("qwen3.8-max")
	if rate == nil {
		t.Fatal("缺 qwen3.8-max 行（5.4.9）")
	}
	u := &core.Usage{
		InputTokens: 2000, InputTokensUncached: 1000, CacheReadTokens: 1000,
		CacheWriteTokens: 1000, OutputTokens: 100,
	}
	c := CalculateCost(u, rate, time.Date(2026, 11, 16, 10, 0, 0, 0, beijing))
	if c.CacheReadCostCNY != 0 {
		t.Fatalf("CacheReadCostCNY = %v，期望 0（5.2 口径 8：命中价官方未给 ⇒ 按 0 计，⛔ 不猜价）", c.CacheReadCostCNY)
	}
	if c.CacheWriteCostCNY != 0.015 {
		t.Fatalf("CacheWriteCostCNY = %v，期望 0.015 = 1000×15.0000/1e6（写入价照表计）", c.CacheWriteCostCNY)
	}
	if c.CostCNY != 0.0306 {
		t.Fatalf("CostCNY = %v，期望 0.0306（in 0.012 + read 0 + write 0.015 + out 0.0036）", c.CostCNY)
	}
	if !c.Estimated {
		t.Fatal("CachePriceUnknown=true ⇒ Estimated 应为 true（5.2 口径 8：命中费按 0 计并置 Estimated=true）")
	}
}

// TestCalculateCostUnknownRate 钉住未知费率口径（5.6 ④）：rate == nil ⇒
// Source = CostUnknown、CostCNY = 0，⛔ 不猜价、⛔ 不借用相似模型的价。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestCalculateCostUnknownRate -v
func TestCalculateCostUnknownRate(t *testing.T) {
	u := &core.Usage{InputTokens: 1000, InputTokensUncached: 1000, OutputTokens: 100, Estimated: true}
	c := CalculateCost(u, nil, time.Date(2026, 11, 16, 10, 0, 0, 0, beijing))
	if c.Source != core.CostUnknown {
		t.Fatalf("Source = %q，期望 unknown（5.6 ④）", c.Source)
	}
	if c.CostCNY != 0 || c.CostUSD != 0 {
		t.Fatalf("未知费率 CostCNY/CostUSD 应为 0（⛔ 不猜价）：%v / %v", c.CostCNY, c.CostUSD)
	}
	if c.RateSnapshot != "" {
		t.Fatalf("未知费率 RateSnapshot 应为空，得到 %q", c.RateSnapshot)
	}
	if !c.Estimated {
		t.Fatal("Estimated 应与 Usage.Estimated 联动透传（5.6）")
	}
}

// TestCalculateCostEstimatedPropagation 钉住 Estimated 联动（5.6）：
// usage 是估算值（流中断）⇒ 费用也是估算值，Source 仍为 estimated（费率表计算）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestCalculateCostEstimatedPropagation -v
func TestCalculateCostEstimatedPropagation(t *testing.T) {
	rate := loadTestRates(t).Lookup("deepseek-flash")
	u := &core.Usage{InputTokens: 1000, InputTokensUncached: 1000, Estimated: true}
	c := CalculateCost(u, rate, time.Date(2026, 11, 16, 10, 0, 0, 0, beijing))
	if !c.Estimated {
		t.Fatal("Usage.Estimated=true ⇒ Cost.Estimated 应为 true（5.6：联动透传）")
	}
	if c.Source != core.CostEstimated {
		t.Fatalf("Source = %q，期望 estimated（5.6 ③）", c.Source)
	}
}

// TestFromUSDTicks 钉住 xAI 自报费用换算（5.6 ① / 5.4.8）：
// 1 USD = 1e10 ticks ⇒ 12300000000 ticks = 1.23 USD ⇒ CostCNY = round4(1.23×7) = 8.61、
// CostUSD = 1.23；Source = ProviderReported；⛔ 不猜分项明细（四项为 0）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestFromUSDTicks -v
func TestFromUSDTicks(t *testing.T) {
	rate := loadTestRates(t).Lookup("grok-4.7")
	if rate == nil {
		t.Fatal("缺 grok-4.7 行（5.4.8）")
	}
	c := FromUSDTicks(12300000000, rate)
	if c.CostUSD != 1.23 {
		t.Fatalf("CostUSD = %v，期望 1.23 = 12300000000 / 1e10（5.6 ①：1 USD = 10^10 ticks）", c.CostUSD)
	}
	if c.CostCNY != 8.61 {
		t.Fatalf("CostCNY = %v，期望 8.61 = round4(1.23×7)（5.6 ①：÷1e10 × 7）", c.CostCNY)
	}
	if c.Source != core.CostProviderReported {
		t.Fatalf("Source = %q，期望 provider_reported（5.6 ①：供应商自报费用优先）", c.Source)
	}
	if c.RateSnapshot != "grok-4.7@2026-11-13-provider-reported" {
		t.Fatalf("RateSnapshot = %q，期望 grok-4.7@2026-11-13-provider-reported", c.RateSnapshot)
	}
	if c.InputCostCNY != 0 || c.CacheReadCostCNY != 0 || c.CacheWriteCostCNY != 0 || c.OutputCostCNY != 0 {
		t.Fatal("自报值只给总额，⛔ 不猜分项明细（四项应为 0）")
	}

	// 1e10 ticks = 1 USD ⇒ 7 CNY（干净值换算）。
	one := FromUSDTicks(10000000000, nil)
	if one.CostCNY != 7 || one.CostUSD != 1 {
		t.Fatalf("1e10 ticks ⇒ CNY=%v(7) USD=%v(1)", one.CostCNY, one.CostUSD)
	}
	if one.RateSnapshot != "" {
		t.Fatalf("rate == nil 时 RateSnapshot 应为空，得到 %q", one.RateSnapshot)
	}

	// 自报负值属协议异常，按 0 落账（⛔ 不产生负数）。
	neg := FromUSDTicks(-5, rate)
	if neg.CostCNY != 0 || neg.CostUSD != 0 {
		t.Fatalf("负 ticks 应按 0 落账：%v / %v", neg.CostCNY, neg.CostUSD)
	}
}
