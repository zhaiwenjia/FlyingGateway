// 日期: 2026-11-14 11:54:49.173
// 作者: Zhai Wenjia

// Package cost 实现费用计量（统一 SDD 第五编）：费率数据结构与加载自检（5.4 / 5.5）、
// 费用计算（5.2 公式 / 5.7 伪代码）、xAI 自报费用换算（5.6 ①）。
// 单价单位：元 / 1M tokens（4 位小数）；费用单位：人民币元（4 位小数）；
// 全精度计算、最后统一舍入一次（5.2 / 5.7）。
package cost

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// PriceSource 取值（统一 SDD 5.4.13）：
// OfficialCNY = 官方人民币原生价；Converted = USD × 7 换算价（入库期已换算，⛔ 运行期不再乘汇率）。
const (
	PriceSourceOfficialCNY = "OfficialCNY"
	PriceSourceConverted   = "Converted"
)

// PriceTier 是阶梯 / 分档维度（统一 SDD 5.4.13）：
// MaxInputTokens × MaxOutputTokens 双维度并集，单维度模型只用输入维度（MaxOutputTokens = 0 表示不限）。
type PriceTier struct {
	// MaxInputTokens 是输入维度档位上限（含）。
	MaxInputTokens int `json:"max_input_tokens"`
	// MaxOutputTokens 是输出维度档位上限（含）；仅输入维度分档时为 0（不限）。
	MaxOutputTokens int `json:"max_output_tokens"`
	// InputPriceCNY 是该档输入单价（元 / 1M tokens，4 位小数）。
	InputPriceCNY float64 `json:"input_price_cny"`
	// OutputPriceCNY 是该档输出单价（元 / 1M tokens，4 位小数）。
	OutputPriceCNY float64 `json:"output_price_cny"`
}

// ModelRate 是单个模型的费率行（统一 SDD 5.4.13 字段清单，逐字段语义见该节）。
type ModelRate struct {
	// Model 是模型名（如 deepseek-flash）。
	Model string `json:"model"`
	// Provider 是供应商（取 core.Provider 名口径：deepseek / openai / anthropic / …）。
	Provider string `json:"provider"`

	// InputPriceCNY 是未缓存输入单价，元 / 1M tokens，4 位小数。
	InputPriceCNY float64 `json:"input_price_cny"`
	// CacheReadExplicitCNY 是缓存命中（显式缓存）读取单价，元 / 1M tokens。
	CacheReadExplicitCNY float64 `json:"cache_read_explicit_cny"`
	// CacheReadImplicitCNY 是缓存命中（隐式缓存）读取单价；单一命中价的供应商两字段同值。
	CacheReadImplicitCNY float64 `json:"cache_read_implicit_cny"`
	// CacheWriteCNY 是缓存写入单价（TTL 短档：Anthropic / Kimi 5m、OpenAI 30m；Qwen = 显式创建价
	// 的入库换算值）；官方未单列时为 0。CalculateCost 用此档计写入费（Kimi 默认 TTL = 5m）。
	CacheWriteCNY float64 `json:"cache_write_cny"`
	// CacheWriteLongCNY 是缓存写入长 TTL 档单价（Anthropic / Kimi 1h、OpenAI 12h）。
	// ⚠️ 扩展字段：SDD 5.4.13 schema 只带单一 CacheWriteCNY，而 5.4 各表写入价分两档
	// （5m / 1h、30m / 12h），为「11 张表全部数据行逐字转录」随行保留长档价；
	// 计价只用短档（core.Usage 无 TTL 维度），长档价供通道层按 5.2 计价口径 2 取数。
	CacheWriteLongCNY float64 `json:"cache_write_long_cny"`
	// OutputPriceCNY 是输出单价（含 reasoning），元 / 1M tokens。
	OutputPriceCNY float64 `json:"output_price_cny"`

	// PeakMultiplier 是峰时倍率；1.0 = 无峰谷；DeepSeek = 2.0（5.3：峰时 = 谷时 × 2，
	// 表内 InputPriceCNY 等为谷时价）。
	PeakMultiplier float64 `json:"peak_multiplier"`
	// InputTiers 是阶梯 / 分档；空 = 单档。档位按 MaxInputTokens 升序排列。
	InputTiers []PriceTier `json:"input_tiers"`
	// LongContextMultiplier 是长上下文档倍率（OpenAI：>272K 输入为短上下文 2 倍；
	// xAI：prompt >200K 时输入 / 输出 / 缓存全部 ×2）。
	LongContextMultiplier float64 `json:"long_context_multiplier"`
	// LongContextThreshold 是长上下文档阈值（tokens）：OpenAI 272K = 272,000、xAI 200,000。
	LongContextThreshold int `json:"long_context_threshold"`
	// PriceValidUntil 是限价有效期（YYYY-MM-DD）；Gemini 多个模型有时间限价，过期须重取官方价。
	PriceValidUntil string `json:"price_valid_until"`
	// Region 是定价地域（百炼按地域分别定价，本表取华北2（北京））；空 = 表内未按地域分价。
	Region string `json:"region"`
	// CachePriceUnknown = true 表示缓存**命中**绝对单价官方未给（「见控制台」）⇒
	// 命中费按 0 计并置 Estimated=true，⛔ 不猜价；写入价照表计（5.2 计价口径 8）。
	CachePriceUnknown bool `json:"cache_price_unknown"`
	// BatchDiscount 是 Batch 折扣倍率；本期不做 Batch 计价，0 = 不支持（5.4.12）。
	BatchDiscount float64 `json:"batch_discount"`
	// LocalDeployment 是本地部署标记（Ollama：CostCNY = 0，无 API 费用但有硬件成本）。
	LocalDeployment bool `json:"local_deployment"`
	// ContextWindow 是上下文长度（tokens）；0 = 费率表未列。
	ContextWindow int `json:"context_window"`
	// Currency 是官方原始币种（CNY / USD）。
	Currency string `json:"currency"`
	// PriceSource 是单价来源（OfficialCNY / Converted），语义与自检见 5.4.13 / 5.5。
	PriceSource string `json:"price_source"`
	// SnapshotDate 是快照日期（YYYY-MM-DD），费率时效的唯一依据（5.4.12）。
	SnapshotDate string `json:"snapshot_date"`
}

// SnapshotID 返回费率快照标识（5.1：形如 deepseek-flash@2026-11-13-peak）。
// 标识含时段（peak / offpeak）与 holiday_calendar 取值（5.3：RateSnapshot 记录所用日历）。
// 峰谷仅对 PeakMultiplier > 1 的模型有意义；其余模型恒 offpeak。
func (r *ModelRate) SnapshotID(callTime time.Time) string {
	slot := "offpeak"
	if r.PeakMultiplier > 1.0 && isPeakHour(callTime) {
		slot = "peak"
	}
	return fmt.Sprintf("%s@%s-%s-%s", r.Model, r.SnapshotDate, slot, HolidayCalendarCNFixed)
}

// Rates 是整张费率表（配置字段 + 费率行索引）。
type Rates struct {
	// USDToCNY 是汇率记录（配置字段 usd_to_cny，1 USD = 7 CNY，暂定、可配置；5.1）。
	// ⚠️ 换算在入库期已完成（Converted 行单价已是 USD × 7 结果），运行期 ⛔ 不再乘汇率（5.5）。
	USDToCNY float64
	// SnapshotDate 是整表快照日期。
	SnapshotDate string

	rows map[string]*ModelRate
}

// ratesFile 是 config/rates.json 的顶层结构。
type ratesFile struct {
	USDToCNY     float64      `json:"usd_to_cny"`
	SnapshotDate string       `json:"snapshot_date"`
	Rates        []ModelRate  `json:"rates"`
}

// LoadRates 加载费率表并做加载期自检（统一 SDD 5.5）：
//   - 国内供应商（DeepSeek / Kimi / 智谱 / Qwen / MiniMax / MiMo）行的 PriceSource
//     必须为 OfficialCNY，否则视为数据错误、直接报错（⛔ 不照用）；
//   - PriceSource 只允许 OfficialCNY / Converted；
//   - model / provider 必填；model 重复报错（Lookup 按精确模型名索引，重复会静默覆盖）。
func LoadRates(path string) (*Rates, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cost: 读费率表 %s 失败: %w", path, err)
	}
	var doc ratesFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("cost: 解析费率表 %s 失败: %w", path, err)
	}
	if doc.USDToCNY <= 0 {
		return nil, fmt.Errorf("cost: 费率表 %s 自检失败：usd_to_cny = %v，必须 > 0", path, doc.USDToCNY)
	}
	rs := &Rates{
		USDToCNY:     doc.USDToCNY,
		SnapshotDate: doc.SnapshotDate,
		rows:         make(map[string]*ModelRate, len(doc.Rates)),
	}
	for i := range doc.Rates {
		r := &doc.Rates[i]
		if r.Model == "" || r.Provider == "" {
			return nil, fmt.Errorf("cost: 费率表 %s 自检失败：第 %d 行 model / provider 为空", path, i+1)
		}
		if r.PriceSource != PriceSourceOfficialCNY && r.PriceSource != PriceSourceConverted {
			return nil, fmt.Errorf("cost: 费率表 %s 自检失败：行 %q 的 PriceSource = %q，只允许 %s / %s",
				path, r.Model, r.PriceSource, PriceSourceOfficialCNY, PriceSourceConverted)
		}
		if isDomesticProvider(r.Provider) && r.PriceSource != PriceSourceOfficialCNY {
			// SDD 5.5：国内供应商一律官方人民币原生价；标 Converted ⇒ 数据错误，自检报错而非照用。
			return nil, fmt.Errorf("cost: 费率表 %s 自检失败：国内供应商 %s 的行 %q 的 PriceSource = %q，必须为 %s（SDD 5.5）",
				path, r.Provider, r.Model, r.PriceSource, PriceSourceOfficialCNY)
		}
		if _, dup := rs.rows[r.Model]; dup {
			return nil, fmt.Errorf("cost: 费率表 %s 自检失败：模型 %q 重复定义", path, r.Model)
		}
		rs.rows[r.Model] = r
	}
	return rs, nil
}

// Lookup 按精确模型名查费率；无该模型 ⇒ nil（⛔ 不借用相似模型的价，5.7「未知费率的处理」）。
func (rs *Rates) Lookup(model string) *ModelRate {
	if rs == nil {
		return nil
	}
	return rs.rows[model]
}

// isDomesticProvider 判定国内供应商（SDD 5.5 自检清单：DeepSeek / Kimi / 智谱 / Qwen / MiniMax / MiMo）。
func isDomesticProvider(provider string) bool {
	switch provider {
	case "deepseek", "moonshot", "zhipu", "qwen", "minimax", "xiaomi":
		return true
	}
	return false
}
