// 日期: 2026-11-14 11:33:06.951
// 作者: Zhai Wenjia

package core

// CostSource 是费用来源枚举（统一 SDD 5.1 / 5.6）。
type CostSource string

const (
	// CostProviderReported = 供应商返回了费用（如 xAI 的 cost_in_usd_ticks）。
	CostProviderReported CostSource = "provider_reported"
	// CostEstimated = 按 5.4 费率总表计算。
	CostEstimated CostSource = "estimated"
	// CostUnknown = 既无费率也无供应商数据（CostCNY = 0，⛔ 不猜价）。
	CostUnknown CostSource = "unknown"
)

// Cost 是统一费用结构（统一 SDD 5.1，逐字段定义）。
// 币种：主币种人民币；美元仅作参考（CostUSD = CostCNY / 7，round4）。
// 精度：全精度计算、最后统一舍入一次（统一 SDD 5.2 / 5.7）。
type Cost struct {
	// ---- 人民币（主币种）----
	// CostCNY 是本次调用总费用（人民币元），保留 4 位小数。
	CostCNY float64

	// ---- 美元（参考）----
	// CostUSD 是本次调用总费用（美元），按固定汇率换算，保留 4 位小数。
	CostUSD float64

	// ---- 明细（人民币元，均保留 4 位小数）----

	// InputCostCNY 是未缓存输入（InputTokensUncached）费用。
	InputCostCNY float64
	// CacheReadCostCNY 是缓存命中（CacheReadTokens）读取费用（显式 / 隐式按对应档单价）。
	CacheReadCostCNY float64
	// CacheWriteCostCNY 是缓存写入（CacheWriteTokens）费用（按 TTL 分档计价）。
	CacheWriteCostCNY float64
	// OutputCostCNY 是输出（OutputTokens）费用，含 reasoning / thinking token。
	OutputCostCNY float64

	// ---- 来源与可信度 ----

	// Source 是费用来源（ProviderReported / Estimated / Unknown）。
	Source CostSource
	// RateSnapshot 是费率快照标识，形如 "deepseek-flash@2026-11-13-peak"。
	RateSnapshot string
	// PeakApplied 表示本次是否应用了峰谷加价（DeepSeek）。
	PeakApplied bool
	// Estimated = true 表示费用为估算值（与 Usage.Estimated 联动透传）。
	Estimated bool
}
