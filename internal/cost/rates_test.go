// 日期: 2026-11-14 11:54:49.175
// 作者: Zhai Wenjia

package cost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 本文件是 config/rates.json 与 SDD 5.4 费率总表（快照 2026-11-13）的逐行对账测试。
// ⚠️ 测试里的数字是「对账期望值」——逐字取自 SDD 5.4.x 表格，不是运行时价
// （运行时只查 rates.json）；每条断言注释写明出处小节号。

// loadTestRates 加载仓库内 config/rates.json。
func loadTestRates(t *testing.T) *Rates {
	t.Helper()
	rs, err := LoadRates(filepath.Join("..", "..", "config", "rates.json"))
	if err != nil {
		t.Fatalf("加载 config/rates.json 失败：%v", err)
	}
	return rs
}

// assertPrice 是单字段单价对账（元 / 1M tokens，4 位小数原样）。
func assertPrice(t *testing.T, model, field string, got, want float64) {
	t.Helper()
	if got != want {
		t.Fatalf("%s.%s = %v，期望 %v（SDD 5.4 对账）", model, field, got, want)
	}
}

// TestLoadRatesReconcileDeepSeek 对账 5.4.1 DeepSeek（峰谷定价，官方人民币价）：
// deepseek-flash 谷 输入 1.0000 / 命中 0.0200 / 输出 4.0000；峰 输入 2.0000 / 命中 0.0400 / 输出 8.0000；
// deepseek-v4-pro 谷 输入 4.5000 / 命中 0.1500 / 输出 13.5000；峰 输入 9.0000 / 命中 0.3000 / 输出 27.0000。
// 表内为谷时价 + peak_multiplier=2.0（5.3：峰时 = 谷时 × 2）；峰行数字 = 谷 × 倍率，逐字对账。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesReconcileDeepSeek -v
func TestLoadRatesReconcileDeepSeek(t *testing.T) {
	rs := loadTestRates(t)

	flash := rs.Lookup("deepseek-flash")
	if flash == nil {
		t.Fatal("缺 deepseek-flash 行")
	}
	assertPrice(t, "deepseek-flash", "谷 输入（5.4.1）", flash.InputPriceCNY, 1.0000)
	assertPrice(t, "deepseek-flash", "谷 命中（5.4.1）", flash.CacheReadExplicitCNY, 0.0200)
	assertPrice(t, "deepseek-flash", "谷 输出（5.4.1）", flash.OutputPriceCNY, 4.0000)
	if flash.PeakMultiplier != 2.0 {
		t.Fatalf("deepseek-flash peak_multiplier = %v，期望 2.0（5.3：峰时 = 谷时 × 2）", flash.PeakMultiplier)
	}
	// 峰行逐字（5.4.1：峰 输入 2.0000 / 命中 0.0400 / 输出 8.0000）
	assertPrice(t, "deepseek-flash", "峰 输入（5.4.1）", flash.InputPriceCNY*flash.PeakMultiplier, 2.0000)
	assertPrice(t, "deepseek-flash", "峰 命中（5.4.1）", flash.CacheReadExplicitCNY*flash.PeakMultiplier, 0.0400)
	assertPrice(t, "deepseek-flash", "峰 输出（5.4.1）", flash.OutputPriceCNY*flash.PeakMultiplier, 8.0000)
	// 5.4.1：本表无缓存写入价 ⇒ CacheWriteCNY = 0
	assertPrice(t, "deepseek-flash", "缓存写入（5.4.1）", flash.CacheWriteCNY, 0.0000)

	pro := rs.Lookup("deepseek-v4-pro")
	if pro == nil {
		t.Fatal("缺 deepseek-v4-pro 行")
	}
	assertPrice(t, "deepseek-v4-pro", "谷 输入（5.4.1）", pro.InputPriceCNY, 4.5000)
	assertPrice(t, "deepseek-v4-pro", "谷 命中（5.4.1）", pro.CacheReadExplicitCNY, 0.1500)
	assertPrice(t, "deepseek-v4-pro", "谷 输出（5.4.1）", pro.OutputPriceCNY, 13.5000)
	assertPrice(t, "deepseek-v4-pro", "峰 输入（5.4.1）", pro.InputPriceCNY*pro.PeakMultiplier, 9.0000)
	assertPrice(t, "deepseek-v4-pro", "峰 命中（5.4.1）", pro.CacheReadExplicitCNY*pro.PeakMultiplier, 0.3000)
	assertPrice(t, "deepseek-v4-pro", "峰 输出（5.4.1）", pro.OutputPriceCNY*pro.PeakMultiplier, 27.0000)
}

// TestLoadRatesReconcileOpenAI 对账 5.4.2 OpenAI（USD 官方价 × 7 换算）四模型逐字，
// 缓存写入 30m（cache_write_cny）/ 12h（cache_write_long_cny）两档分列；
// 长上下文档：long_context_multiplier=2.0、阈值 272K = 272,000。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesReconcileOpenAI -v
func TestLoadRatesReconcileOpenAI(t *testing.T) {
	rs := loadTestRates(t)
	want := map[string][5]float64{
		// {输入, 命中, 写入30m, 写入12h, 输出}（5.4.2 逐字）
		"gpt-6-astra":   {70.0000, 7.0000, 87.5000, 175.0000, 350.0000},
		"gpt-6.1-sol":   {14.0000, 0.7000, 17.5000, 35.0000, 70.0000},
		"gpt-6.1-terra": {7.0000, 0.3500, 8.7500, 17.5000, 35.0000},
		"gpt-6-luna":    {0.7000, 0.0700, 0.8750, 1.7500, 3.5000},
	}
	for model, w := range want {
		r := rs.Lookup(model)
		if r == nil {
			t.Fatalf("缺 %s 行", model)
		}
		assertPrice(t, model, "输入（5.4.2）", r.InputPriceCNY, w[0])
		assertPrice(t, model, "命中（5.4.2）", r.CacheReadExplicitCNY, w[1])
		assertPrice(t, model, "写入30m（5.4.2）", r.CacheWriteCNY, w[2])
		assertPrice(t, model, "写入12h（5.4.2）", r.CacheWriteLongCNY, w[3])
		assertPrice(t, model, "输出（5.4.2）", r.OutputPriceCNY, w[4])
		if r.LongContextMultiplier != 2.0 {
			t.Fatalf("%s long_context_multiplier = %v，期望 2.0（5.4.2：>272K 为短上下文 2 倍）", model, r.LongContextMultiplier)
		}
		if r.LongContextThreshold != 272000 {
			t.Fatalf("%s long_context_threshold = %d，期望 272000（5.4.2：272K）", model, r.LongContextThreshold)
		}
	}
}

// TestLoadRatesReconcileAnthropic 对账 5.4.3 Anthropic 四行逐字，
// 缓存写入 5m（cache_write_cny）/ 1h（cache_write_long_cny）分列。
// 2026-11-21 移除「claude-haiku-5.5（prompt >100K）」行（key = claude-haiku-5.5@prompt-gt-100k）：
// Lookup 按精确模型名索引（rates.go Lookup）且全仓无代码构造该 key ⇒ 运行期不可达的死数据；
// SDD 5.4.3 与 docs/20261113-供应商模型费率表.md 仍记录官方两档价（调研事实，未改）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesReconcileAnthropic -v
func TestLoadRatesReconcileAnthropic(t *testing.T) {
	rs := loadTestRates(t)
	want := map[string][5]float64{
		// {输入, 写入5m, 写入1h, 命中, 输出}（5.4.3 逐字）
		"claude-fable-5.2":  {56.0000, 70.0000, 112.0000, 1.4000, 280.0000},
		"claude-opus-5.5":   {28.0000, 35.0000, 56.0000, 1.4000, 140.0000},
		"claude-sonnet-5.5": {14.0000, 17.5000, 28.0000, 0.7000, 70.0000},
		"claude-haiku-5.5":  {0.7000, 0.8750, 1.4000, 0.0700, 3.5000},
	}
	for model, w := range want {
		r := rs.Lookup(model)
		if r == nil {
			t.Fatalf("缺 %s 行", model)
		}
		assertPrice(t, model, "输入（5.4.3）", r.InputPriceCNY, w[0])
		assertPrice(t, model, "写入5m（5.4.3）", r.CacheWriteCNY, w[1])
		assertPrice(t, model, "写入1h（5.4.3）", r.CacheWriteLongCNY, w[2])
		assertPrice(t, model, "命中（5.4.3）", r.CacheReadExplicitCNY, w[3])
		assertPrice(t, model, "输出（5.4.3）", r.OutputPriceCNY, w[4])
	}
}

// TestLoadRatesReconcileGemini 对账 5.4.4 Gemini 四模型逐字 + 三行限价有效期（price_valid_until）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesReconcileGemini -v
func TestLoadRatesReconcileGemini(t *testing.T) {
	rs := loadTestRates(t)
	want := map[string][3]float64{
		// {输入, 输出（含 thinking）, 缓存读取}（5.4.4 逐字）
		"gemini-4-carbon":   {14.0000, 70.0000, 0.7000},
		"gemini-4-argon":    {14.0000, 70.0000, 0.7000},
		"gemini-4-hydrogen": {10.5000, 52.5000, 0.5250},
		"gemini-3.8-flash":  {5.2500, 26.2500, 0.5250},
	}
	for model, w := range want {
		r := rs.Lookup(model)
		if r == nil {
			t.Fatalf("缺 %s 行", model)
		}
		assertPrice(t, model, "输入（5.4.4）", r.InputPriceCNY, w[0])
		assertPrice(t, model, "输出（5.4.4）", r.OutputPriceCNY, w[1])
		assertPrice(t, model, "缓存读取（5.4.4）", r.CacheReadExplicitCNY, w[2])
	}
	// 5.4.4 限价到期列逐字
	if got := rs.Lookup("gemini-4-carbon").PriceValidUntil; got != "2026-11-30" {
		t.Fatalf("gemini-4-carbon price_valid_until = %q，期望 2026-11-30（5.4.4）", got)
	}
	if got := rs.Lookup("gemini-4-argon").PriceValidUntil; got != "2026-11-30" {
		t.Fatalf("gemini-4-argon price_valid_until = %q，期望 2026-11-30（5.4.4）", got)
	}
	if got := rs.Lookup("gemini-3.8-flash").PriceValidUntil; got != "2026-12-31" {
		t.Fatalf("gemini-3.8-flash price_valid_until = %q，期望 2026-12-31（5.4.4）", got)
	}
	if got := rs.Lookup("gemini-4-hydrogen").PriceValidUntil; got != "" {
		t.Fatalf("gemini-4-hydrogen price_valid_until = %q，期望空（5.4.4：限价到期列「—」）", got)
	}
}

// TestLoadRatesReconcileKimi 对账 5.4.5 Kimi / Moonshot 逐字 + 行级快照 2026-11-12（5.4.5 表头注记）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesReconcileKimi -v
func TestLoadRatesReconcileKimi(t *testing.T) {
	rs := loadTestRates(t)
	r := rs.Lookup("kimi-k3.1-pro")
	if r == nil {
		t.Fatal("缺 kimi-k3.1-pro 行")
	}
	assertPrice(t, "kimi-k3.1-pro", "输入未缓存（5.4.5）", r.InputPriceCNY, 13.0000)
	assertPrice(t, "kimi-k3.1-pro", "缓存命中（5.4.5）", r.CacheReadExplicitCNY, 1.3000)
	assertPrice(t, "kimi-k3.1-pro", "写入5m（5.4.5）", r.CacheWriteCNY, 13.0000)
	assertPrice(t, "kimi-k3.1-pro", "写入1h（5.4.5）", r.CacheWriteLongCNY, 26.0000)
	assertPrice(t, "kimi-k3.1-pro", "输出（5.4.5）", r.OutputPriceCNY, 68.0000)
	if r.ContextWindow != 1048576 {
		t.Fatalf("context_window = %d，期望 1048576（5.4.5：1,048,576 tokens）", r.ContextWindow)
	}
	if r.SnapshotDate != "2026-11-12" {
		t.Fatalf("snapshot_date = %q，期望 2026-11-12（5.4.5 表头注记：快照 2026-11-12）", r.SnapshotDate)
	}
}

// TestLoadRatesReconcileZhipu 对账 5.4.6 智谱 GLM 逐字（缓存存储限时免费 ⇒ 写入 0）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesReconcileZhipu -v
func TestLoadRatesReconcileZhipu(t *testing.T) {
	rs := loadTestRates(t)
	for model, w := range map[string][3]float64{
		// {输入, 输出, 缓存命中}（5.4.6 逐字）
		"glm-5.3":       {8.0000, 28.0000, 2.0000},
		"glm-5.3-flash": {0.8000, 2.8000, 0.2300},
	} {
		r := rs.Lookup(model)
		if r == nil {
			t.Fatalf("缺 %s 行", model)
		}
		assertPrice(t, model, "输入（5.4.6）", r.InputPriceCNY, w[0])
		assertPrice(t, model, "输出（5.4.6）", r.OutputPriceCNY, w[1])
		assertPrice(t, model, "缓存命中（5.4.6）", r.CacheReadExplicitCNY, w[2])
		assertPrice(t, model, "缓存写入（5.4.6 存储限时免费）", r.CacheWriteCNY, 0.0000)
	}
}

// TestLoadRatesReconcileMiniMax 对账 5.4.7 MiniMax 逐字；缓存写入官方未单列 ⇒ 0（5.4.7 注记）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesReconcileMiniMax -v
func TestLoadRatesReconcileMiniMax(t *testing.T) {
	rs := loadTestRates(t)
	r := rs.Lookup("MiniMax-M3.1-Flash")
	if r == nil {
		t.Fatal("缺 MiniMax-M3.1-Flash 行")
	}
	assertPrice(t, "MiniMax-M3.1-Flash", "输入（5.4.7）", r.InputPriceCNY, 1.0000)
	assertPrice(t, "MiniMax-M3.1-Flash", "缓存读取（5.4.7）", r.CacheReadExplicitCNY, 0.1000)
	assertPrice(t, "MiniMax-M3.1-Flash", "输出（5.4.7）", r.OutputPriceCNY, 4.0000)
	assertPrice(t, "MiniMax-M3.1-Flash", "缓存写入（5.4.7 官方未单列）", r.CacheWriteCNY, 0.0000)
}

// TestLoadRatesReconcileGrok 对账 5.4.8 xAI Grok 逐字 + 长上下文（阈值 200,000、全部 ×2）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesReconcileGrok -v
func TestLoadRatesReconcileGrok(t *testing.T) {
	rs := loadTestRates(t)
	r := rs.Lookup("grok-4.7")
	if r == nil {
		t.Fatal("缺 grok-4.7 行")
	}
	assertPrice(t, "grok-4.7", "输入（5.4.8）", r.InputPriceCNY, 14.0000)
	assertPrice(t, "grok-4.7", "缓存命中（5.4.8）", r.CacheReadExplicitCNY, 3.5000)
	assertPrice(t, "grok-4.7", "输出（5.4.8）", r.OutputPriceCNY, 42.0000)
	if r.LongContextMultiplier != 2.0 || r.LongContextThreshold != 200000 {
		t.Fatalf("长上下文字段不对：multiplier=%v(2.0) threshold=%d(200000)（5.4.8）",
			r.LongContextMultiplier, r.LongContextThreshold)
	}
	if r.Region != "us-east-1" {
		t.Fatalf("region = %q，期望 us-east-1（5.4.8：us-central-1 = us-east-1 × 1.1，表内为 us-east-1 基准价）", r.Region)
	}
}

// TestLoadRatesReconcileQwen 对账 5.4.9 Qwen / DashScope 三行逐字 + 缓存命中 / 写入分列 +
// cache_price_unknown（命中价官方未给 ⇒ 0、⛔ 不猜价）+ region（华北2（北京））。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesReconcileQwen -v
func TestLoadRatesReconcileQwen(t *testing.T) {
	rs := loadTestRates(t)

	// qwen3.8-max（5.4.9）：输入 12.0000 / 输出 36.0000 / 命中官方未给 / 写入（显式创建）15.0000
	max := rs.Lookup("qwen3.8-max")
	if max == nil {
		t.Fatal("缺 qwen3.8-max 行")
	}
	assertPrice(t, "qwen3.8-max", "输入（5.4.9）", max.InputPriceCNY, 12.0000)
	assertPrice(t, "qwen3.8-max", "输出（5.4.9）", max.OutputPriceCNY, 36.0000)
	assertPrice(t, "qwen3.8-max", "写入显式创建（5.4.9）", max.CacheWriteCNY, 15.0000)
	assertPrice(t, "qwen3.8-max", "命中（5.4.9 官方未给 ⇒ 0 不猜价）", max.CacheReadExplicitCNY, 0.0000)
	if !max.CachePriceUnknown {
		t.Fatal("qwen3.8-max cache_price_unknown 应为 true（5.4.9：命中价「见控制台」）")
	}

	// qwen3.8-flash（5.4.9）：输入 0.8000 / 输出 2.7000 / 命中官方未给 / 写入 1.0000
	flash := rs.Lookup("qwen3.8-flash")
	if flash == nil {
		t.Fatal("缺 qwen3.8-flash 行")
	}
	assertPrice(t, "qwen3.8-flash", "输入（5.4.9）", flash.InputPriceCNY, 0.8000)
	assertPrice(t, "qwen3.8-flash", "输出（5.4.9）", flash.OutputPriceCNY, 2.7000)
	assertPrice(t, "qwen3.8-flash", "写入显式创建（5.4.9）", flash.CacheWriteCNY, 1.0000)
	if !flash.CachePriceUnknown {
		t.Fatal("qwen3.8-flash cache_price_unknown 应为 true（5.4.9）")
	}

	// qwen3.8-27b（5.4.9）：输入 3.0000 / 输出 12.0000 / 命中显式 0.3000 / 命中隐式 0.6000 / 写入 3.7500
	b27 := rs.Lookup("qwen3.8-27b")
	if b27 == nil {
		t.Fatal("缺 qwen3.8-27b 行")
	}
	assertPrice(t, "qwen3.8-27b", "输入（5.4.9）", b27.InputPriceCNY, 3.0000)
	assertPrice(t, "qwen3.8-27b", "输出（5.4.9）", b27.OutputPriceCNY, 12.0000)
	// 缓存命中 / 写入分列：显式命中 0.3000、隐式命中 0.6000、显式创建 3.7500 是三笔独立价（5.4.9）
	assertPrice(t, "qwen3.8-27b", "命中显式（5.4.9）", b27.CacheReadExplicitCNY, 0.3000)
	assertPrice(t, "qwen3.8-27b", "命中隐式（5.4.9）", b27.CacheReadImplicitCNY, 0.6000)
	assertPrice(t, "qwen3.8-27b", "写入显式创建（5.4.9）", b27.CacheWriteCNY, 3.7500)
	if b27.CachePriceUnknown {
		t.Fatal("qwen3.8-27b cache_price_unknown 应为 false（5.4.9 表内给了绝对单价）")
	}
	for _, r := range []*ModelRate{max, flash, b27} {
		if r.Region != "华北2（北京）" {
			t.Fatalf("%s region = %q，期望 华北2（北京）（5.4.9：本表取华北2（北京））", r.Model, r.Region)
		}
	}
}

// TestLoadRatesReconcileMiMo 对账 5.4.10 小米 MiMo 逐字（缓存写入限时免费 ⇒ 0）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesReconcileMiMo -v
func TestLoadRatesReconcileMiMo(t *testing.T) {
	rs := loadTestRates(t)
	for model, w := range map[string][3]float64{
		// {输入, 缓存命中, 输出}（5.4.10 逐字）
		"mimo-v2.6-pro":   {3.0000, 0.0250, 6.0000},
		"mimo-v2.6-flash": {1.0000, 0.0200, 2.0000},
	} {
		r := rs.Lookup(model)
		if r == nil {
			t.Fatalf("缺 %s 行", model)
		}
		assertPrice(t, model, "输入（5.4.10）", r.InputPriceCNY, w[0])
		assertPrice(t, model, "缓存命中（5.4.10）", r.CacheReadExplicitCNY, w[1])
		assertPrice(t, model, "输出（5.4.10）", r.OutputPriceCNY, w[2])
		assertPrice(t, model, "缓存写入（5.4.10 限时免费）", r.CacheWriteCNY, 0.0000)
		if r.ContextWindow != 1000000 {
			t.Fatalf("%s context_window = %d，期望 1000000（5.4.10：1M）", model, r.ContextWindow)
		}
	}
}

// TestLoadRatesReconcileOllama 对账 5.4.11 Ollama（本地部署，费用 = 0）：
// 全部本地模型 ¥0.0000，local_deployment = true。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesReconcileOllama -v
func TestLoadRatesReconcileOllama(t *testing.T) {
	rs := loadTestRates(t)
	r := rs.Lookup("全部本地模型")
	if r == nil {
		t.Fatal("缺 Ollama「全部本地模型」行（5.4.11）")
	}
	assertPrice(t, "全部本地模型", "输入（5.4.11）", r.InputPriceCNY, 0.0000)
	assertPrice(t, "全部本地模型", "输出（5.4.11）", r.OutputPriceCNY, 0.0000)
	if !r.LocalDeployment {
		t.Fatal("local_deployment 应为 true（5.4.11）")
	}
}

// TestLoadRatesRowCount 钉住费率表行数与字段结构：11 张表 25 个数据行
// （DeepSeek 2 + OpenAI 4 + Anthropic 4（haiku >100K 档行 2026-11-21 移除）+ Gemini 4 + Kimi 1 + 智谱 2 +
// MiniMax 1 + Grok 1 + Qwen 3 + MiMo 2 + Ollama 1），命中 / 写入字段分列互不共用。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesRowCount -v
func TestLoadRatesRowCount(t *testing.T) {
	rs := loadTestRates(t)
	if len(rs.rows) != 25 {
		t.Fatalf("费率行数 = %d，期望 25（SDD 5.4 十一张表数据行合计）", len(rs.rows))
	}
	// 缓存命中 / 写入分列：qwen3.8-27b 三个字段值互不相同 ⇒ 不是同一个字段记三个名字
	r := rs.Lookup("qwen3.8-27b")
	if r.CacheReadExplicitCNY == r.CacheWriteCNY || r.CacheReadImplicitCNY == r.CacheWriteCNY {
		t.Fatal("缓存命中与写入未分列（5.2 计价口径 2：⛔ 不与缓存命中合并）")
	}
	if r.CacheReadExplicitCNY == r.CacheReadImplicitCNY {
		t.Fatal("显式 / 隐式命中价应分档（5.4.13：单一命中价的供应商两字段同值，qwen3.8-27b 不是）")
	}
	if rs.USDToCNY != 7 {
		t.Fatalf("usd_to_cny = %v，期望 7（5.1：1 USD = 7 CNY，暂定、可配置）", rs.USDToCNY)
	}
}

// TestLoadRatesPriceSourceSelfCheck 钉住 5.5 加载期自检：
// 国内供应商（DeepSeek / Kimi / 智谱 / Qwen / MiniMax / MiMo）行 PriceSource 标 Converted ⇒ 报错；
// 海外供应商 Converted ⇒ 通过；非法 PriceSource ⇒ 报错。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesPriceSourceSelfCheck -v
func TestLoadRatesPriceSourceSelfCheck(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "rates.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("写临时费率表失败：%v", err)
		}
		return path
	}

	// 国内行标 Converted ⇒ 数据错误，自检报错而非照用（5.5）。
	bad := write(t, `{"usd_to_cny":7,"rates":[{"model":"deepseek-flash","provider":"deepseek","price_source":"Converted"}]}`)
	if _, err := LoadRates(bad); err == nil {
		t.Fatal("国内供应商行 PriceSource=Converted 应报错（SDD 5.5）")
	}

	// 海外行 Converted ⇒ 合法（USD × 7 换算价）。
	ok := write(t, `{"usd_to_cny":7,"rates":[{"model":"gpt-6-astra","provider":"openai","price_source":"Converted"}]}`)
	if _, err := LoadRates(ok); err != nil {
		t.Fatalf("海外供应商行 PriceSource=Converted 应通过：%v", err)
	}

	// 非法 PriceSource ⇒ 报错。
	weird := write(t, `{"usd_to_cny":7,"rates":[{"model":"x","provider":"openai","price_source":"Guess"}]}`)
	if _, err := LoadRates(weird); err == nil {
		t.Fatal("PriceSource=Guess 应报错（只允许 OfficialCNY / Converted）")
	}

	// 重复模型名 ⇒ 报错（Lookup 精确索引，重复会静默覆盖）。
	dup := write(t, `{"usd_to_cny":7,"rates":[{"model":"m","provider":"openai","price_source":"Converted"},{"model":"m","provider":"openai","price_source":"Converted"}]}`)
	if _, err := LoadRates(dup); err == nil {
		t.Fatal("模型名重复应报错")
	}
}

// TestLookupMissingModel 钉住 Lookup 未知模型 ⇒ nil（⛔ 不借用相似模型的价，5.7）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLookupMissingModel -v
func TestLookupMissingModel(t *testing.T) {
	rs := loadTestRates(t)
	if r := rs.Lookup("not-a-model"); r != nil {
		t.Fatalf("未知模型应返回 nil，得到 %+v", r)
	}
	if r := (*Rates)(nil).Lookup("deepseek-flash"); r != nil {
		t.Fatal("nil Rates 的 Lookup 应返回 nil")
	}
}

// TestLoadRatesFieldPresence 钉住「字段齐」（SDD 5.4.13 字段清单 + 扩展 cache_write_long_cny）：
// config/rates.json 每一行都必须带齐 schema 全部键 —— 缺键在 Go 零值语义下会静默变 0
// （如 peak_multiplier 缺失会当 1.0 口径失效、cache_price_unknown 缺失会当 false 照价猜算），
// ⛔ 不能靠零值兜底。键名逐字取自 ModelRate / ratesFile 的 json tag。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/cost/ -run TestLoadRatesFieldPresence -v
func TestLoadRatesFieldPresence(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "rates.json"))
	if err != nil {
		t.Fatalf("读 config/rates.json 失败：%v", err)
	}
	var doc struct {
		Rates []map[string]json.RawMessage `json:"rates"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("解析 config/rates.json 失败：%v", err)
	}
	want := []string{
		"model", "provider", // 5.4.13 必填标识
		"input_price_cny", "cache_read_explicit_cny", "cache_read_implicit_cny",
		"cache_write_cny", "output_price_cny", // 5.4.13 单价五列
		"cache_write_long_cny",     // 扩展列（5.4 各表写入价 5m/1h、30m/12h 两档，5.4.13 注记）
		"peak_multiplier",          // 5.4.13（DeepSeek = 2.0，其余 1.0）
		"input_tiers",              // 5.4.13（空 = 单档）
		"long_context_multiplier",  // 5.4.13（OpenAI / xAI 长上下文档）
		"long_context_threshold",   // 5.4.13（272K / 200K）
		"price_valid_until",        // 5.4.13（Gemini 限价行）
		"region",                   // 5.4.13（Qwen 华北2（北京）/ Grok us-east-1）
		"cache_price_unknown",      // 5.4.13（qwen3.8-max / qwen3.8-flash）
		"batch_discount",           // 5.4.13（0 = 不支持）
		"local_deployment",         // 5.4.13（Ollama）
		"context_window",           // 5.4.13（Kimi / Qwen / MiMo）
		"currency", "price_source", // 5.4.13（币种与来源，5.5 自检依据）
		"snapshot_date", // 5.4.12（费率时效唯一依据）
	}
	for i, row := range doc.Rates {
		name := string(row["model"])
		for _, k := range want {
			if _, ok := row[k]; !ok {
				t.Fatalf("第 %d 行 %q 缺字段 %q（SDD 5.4.13 字段清单必须齐）", i+1, name, k)
			}
		}
	}
}
