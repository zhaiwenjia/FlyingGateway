// 日期: 2026-11-14 11:54:49.174
// 作者: Zhai Wenjia

package cost

import (
	"fmt"
	"math"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// usdToCNY 是美元换算系数（1 USD = 7 CNY，暂定、可配置；统一 SDD 5.1 / 5.2）。
// 费率文件的 usd_to_cny 记录同值；换算在费率入库期完成（Converted 行），
// 运行期只有「算钱」和「舍入」两件事（5.7），CostUSD = CostCNY / 7 走此系数。
const usdToCNY = 7.0

// round4 四舍五入到 4 位小数（费用与单价的统一精度口径）。
// 先乘后取整，避免"先除后乘"在边界上差一个最小单位。
func round4(v float64) float64 {
	return math.Round(v*1e4) / 1e4
}

// CalculateCost 按统一 SDD 5.2 公式与 5.7 伪代码计算本次调用费用：
//
//   - 峰谷：rate.PeakMultiplier > 1 且 isPeakHour(callTime) ⇒ m = PeakMultiplier、PeakApplied = true；
//     判定用 callTime（⛔ 不是响应完成时间），holiday_calendar = cn-fixed（5.3）；
//   - 阶梯：InputTiers 按 usage.InputTokens（**总输入**）选档，⛔ 不按 InputTokensUncached，
//     整请求同价、⛔ 不分段，输出价随档变（5.2 计价口径 6）；
//   - 输入计价用 InputTokensUncached（未缓存新增），⛔ 不用 InputTokens（5.2 计价口径 1）；
//   - 缓存价按 usage.CacheReadMode 取显式 / 隐式档；空值取两者同值口径（单一命中价供应商两档同值，
//     任取其一）；CachePriceUnknown = true ⇒ 命中费 0 + Estimated = true，⛔ 不猜价，写入价照表（5.2 口径 8）；
//   - 四类分项全精度 n·p·m/1e6，⛔ 途中不舍入；total = 四项和后 round4 一次，
//     CostUSD = round4(total/7)，明细各自 round4（5.2 / 5.7）；
//   - 未知费率（rate == nil）⇒ Source = CostUnknown、CostCNY = 0，⛔ 不猜价（5.6 ④）。
func CalculateCost(usage *core.Usage, rate *ModelRate, callTime time.Time) core.Cost {
	if usage == nil {
		usage = &core.Usage{} // 防御：nil usage 按空用量落账（不 panic）
	}
	if rate == nil {
		// 5.6 ④：费率总表没有该模型 ⇒ Source=Unknown、CostCNY=0，⛔ 不猜价、⛔ 不借用相似模型的价。
		return core.Cost{
			Source:    core.CostUnknown,
			Estimated: usage.Estimated,
		}
	}

	// 1. 峰谷判定（仅 PeakMultiplier > 1 的模型，DeepSeek = 2.0）
	multiplier := 1.0
	peakApplied := false
	if rate.PeakMultiplier > 1.0 && isPeakHour(callTime) {
		multiplier = rate.PeakMultiplier
		peakApplied = true
	}

	// 2. 阶梯计费：选档依据 = 单次请求的输入 Token 总量（官方口径），⛔ 不是 InputTokensUncached
	inputPrice := rate.InputPriceCNY
	outputPrice := rate.OutputPriceCNY
	if len(rate.InputTiers) > 0 {
		if in, out, ok := selectTier(rate.InputTiers, usage.InputTokens, usage.OutputTokens); ok {
			inputPrice, outputPrice = in, out
		}
		// 超出所有档位上限 ⇒ 用行级 InputPriceCNY / OutputPriceCNY（表内默认档），⛔ 不猜档外价
	}

	// 3. 缓存价取档与未知处理
	cacheReadPrice := cacheReadPriceFor(rate, usage)
	cacheWritePrice := rate.CacheWriteCNY // TTL 短档 / 默认档（Kimi 默认 TTL = 5m）；长档价由通道层按 5.2 口径 2 取数
	estimated := usage.Estimated
	if rate.CachePriceUnknown {
		cacheReadPrice = 0 // 命中费按 0 计并置 Estimated=true，⛔ 不猜价；写入价照表
		estimated = true
	}

	// 4. 分项计算（单位：元 / 1M tokens → 元），保持全精度，⛔ 不在此处舍入
	inputCost := float64(usage.InputTokensUncached) * inputPrice * multiplier / 1e6
	cacheReadCost := float64(usage.CacheReadTokens) * cacheReadPrice * multiplier / 1e6
	cacheWriteCost := float64(usage.CacheWriteTokens) * cacheWritePrice * multiplier / 1e6
	outputCost := float64(usage.OutputTokens) * outputPrice * multiplier / 1e6

	// 5. 求和后统一舍入一次（⛔ 不要对每个分项先舍入再相加）
	total := inputCost + cacheReadCost + cacheWriteCost + outputCost

	return core.Cost{
		CostCNY:          round4(total),
		CostUSD:          round4(total / usdToCNY),
		InputCostCNY:     round4(inputCost),
		CacheReadCostCNY: round4(cacheReadCost),
		CacheWriteCostCNY: round4(cacheWriteCost),
		OutputCostCNY:    round4(outputCost),
		// 费率表计算一律 Source=Estimated（5.6 ② ③：usage 实测 / 估算都由费率表算出，非供应商自报）；
		// Estimated 标志与 Usage.Estimated 联动透传（5.6）。
		Source:       core.CostEstimated,
		RateSnapshot: rate.SnapshotID(callTime),
		PeakApplied:  peakApplied,
		Estimated:    estimated,
	}
}

// FromUSDTicks 用 xAI 自报 cost_in_usd_ticks 落账（统一 SDD 5.6 ① / 5.4.8）：
// 1 USD = 1e10 ticks，自报值是美元（折扣后、含服务端工具费的实际计费额）⇒
// 转人民币走 ÷ 1e10 × usdToCNY（7）；Source = CostProviderReported。
// 自报值只给总额，⛔ 不猜分项明细（四项明细为 0）。
func FromUSDTicks(ticks int64, rate *ModelRate) core.Cost {
	if ticks < 0 {
		ticks = 0 // 自报负值属协议异常，按 0 落账（⛔ 不产生负数）
	}
	usd := float64(ticks) / 1e10
	c := core.Cost{
		CostCNY: round4(usd * usdToCNY),
		CostUSD: round4(usd),
		Source:  core.CostProviderReported,
	}
	if rate != nil {
		// 自报费用不参与费率表计价 ⇒ 快照标识只记兜底费率行与时态来源，不标峰谷时段。
		c.RateSnapshot = fmt.Sprintf("%s@%s-provider-reported", rate.Model, rate.SnapshotDate)
	}
	return c
}

// cacheReadPriceFor 按缓存命中档位取价（5.7「缓存命中档位」）：
// 显式命中取 CacheReadExplicitCNY、隐式命中取 CacheReadImplicitCNY；
// CacheReadMode 为空 ⇒ 单一命中价口径（两档同值，任取一档——此处取显式档，两值相等）。
func cacheReadPriceFor(rate *ModelRate, usage *core.Usage) float64 {
	if usage.CacheReadMode == core.CacheModeImplicit {
		return rate.CacheReadImplicitCNY
	}
	return rate.CacheReadExplicitCNY
}

// selectTier 按阶梯 / 分档选档（统一 SDD 5.4.13 选档规则）：
// 按**单次请求的输入 Token 总量**（InputTokens）落档，整请求按所在档单价结算、⛔ 不分段；
// 双维度档（MaxOutputTokens > 0）同时按输出长度落档；输出价随档位变。
// 档位按 MaxInputTokens 升序排列，取第一个命中档；全部不命中 ⇒ ok = false（调用方回落行级单价）。
func selectTier(tiers []PriceTier, inputTokens, outputTokens int64) (inputPrice, outputPrice float64, ok bool) {
	for _, tier := range tiers {
		if inputTokens > int64(tier.MaxInputTokens) {
			continue
		}
		if tier.MaxOutputTokens > 0 && outputTokens > int64(tier.MaxOutputTokens) {
			continue
		}
		return tier.InputPriceCNY, tier.OutputPriceCNY, true
	}
	return 0, 0, false
}
