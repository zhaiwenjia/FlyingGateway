// 日期: 2026-11-14 12:02:39.083
// 作者: Zhai Wenjia

package detect

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// hostPattern 是一条 host 匹配形态（统一 SDD 4.6.1）。
type hostPattern int

const (
	// hostExact：hostname 精确相等。
	hostExact hostPattern = iota
	// hostSuffix：hostname 等于 value 或以 ".value" 结尾（对应 `*.maas.aliyuncs.com` 形态）。
	hostSuffix
	// hostPrefix：hostname 以 value 开头（对应 `batch-api-cn.*` / `token-plan-cn.*` 形态）。
	hostPrefix
	// hostPort：host（含端口）精确相等（对应 `localhost:11434` / `127.0.0.1:11434`，端口是规则的一部分）。
	hostPort
)

// hostRule 是 host → 供应商规则表的一行（统一 SDD 4.6.1，逐行照抄 11 家）。
// 同一家的多个 host 形态放在同一行的 patterns 里，任一命中即该行命中。
//
// ⛔ 用有序切片而非 map：识别决策必须可复现（统一 SDD 1.4.3），
// 不许让 map 遍历顺序成为契约（统一 SDD 24 路由可复现）。
type hostRule struct {
	patterns []hostPattern
	values   []string
	provider core.Provider
}

// hostRules 是 host → 供应商规则表（统一 SDD 4.6.1，行序即表序）。
var hostRules = []hostRule{
	// 4.6.1 第 1 行：`*.maas.aliyuncs.com` / `dashscope.aliyuncs.com` ⇒ Qwen。
	// ⚠️ Qwen 例外：API Key 与地域强绑定 ⇒ 命中 maas 后缀后还要做地域段校验（见 qwenRegion）。
	{
		patterns: []hostPattern{hostSuffix, hostExact},
		values:   []string{"maas.aliyuncs.com", "dashscope.aliyuncs.com"},
		provider: core.ProviderQwen,
	},
	// 第 2 行：`api.moonshot.cn` ⇒ Kimi。
	{patterns: []hostPattern{hostExact}, values: []string{"api.moonshot.cn"}, provider: core.ProviderMoonshot},
	// 第 3 行：`open.bigmodel.cn` ⇒ 智谱。
	{patterns: []hostPattern{hostExact}, values: []string{"open.bigmodel.cn"}, provider: core.ProviderZhipu},
	// 第 4 行：`api.deepseek.com` ⇒ DeepSeek。
	{patterns: []hostPattern{hostExact}, values: []string{"api.deepseek.com"}, provider: core.ProviderDeepSeek},
	// 第 5 行：`api.xiaomimimo.com` / `batch-api-cn.*` / `token-plan-cn.*` ⇒ 小米 MiMo。
	{
		patterns: []hostPattern{hostExact, hostPrefix, hostPrefix},
		values:   []string{"api.xiaomimimo.com", "batch-api-cn.", "token-plan-cn."},
		provider: core.ProviderXiaomi,
	},
	// 第 6 行：`api.minimax.io` ⇒ MiniMax。
	{patterns: []hostPattern{hostExact}, values: []string{"api.minimax.io"}, provider: core.ProviderMiniMax},
	// 第 7 行：`api.openai.com` ⇒ OpenAI。
	{patterns: []hostPattern{hostExact}, values: []string{"api.openai.com"}, provider: core.ProviderOpenAI},
	// 第 8 行：`api.anthropic.com` ⇒ Claude。
	{patterns: []hostPattern{hostExact}, values: []string{"api.anthropic.com"}, provider: core.ProviderAnthropic},
	// 第 9 行：`api.x.ai` ⇒ Grok。
	{patterns: []hostPattern{hostExact}, values: []string{"api.x.ai"}, provider: core.ProviderXAI},
	// 第 10 行：`generativelanguage.googleapis.com` / `aiplatform.googleapis.com` ⇒ Gemini。
	{
		patterns: []hostPattern{hostExact, hostExact},
		values:   []string{"generativelanguage.googleapis.com", "aiplatform.googleapis.com"},
		provider: core.ProviderGemini,
	},
	// 第 11 行：`localhost:11434` / `127.0.0.1:11434` ⇒ Ollama（端口是规则的一部分）。
	{
		patterns: []hostPattern{hostPort, hostPort},
		values:   []string{"localhost:11434", "127.0.0.1:11434"},
		provider: core.ProviderOllama,
	},
}

// matchHost 按规则表顺序匹配 host，返回供应商与是否命中。
// host 含端口（hostPort 规则需要），hostname 不含端口（域名规则用）。
func matchHost(host, hostname string) (core.Provider, bool) {
	for _, rule := range hostRules {
		for i, kind := range rule.patterns {
			value := rule.values[i]
			switch kind {
			case hostExact:
				if hostname == value {
					return rule.provider, true
				}
			case hostSuffix:
				if hostname == value || strings.HasSuffix(hostname, "."+value) {
					return rule.provider, true
				}
			case hostPrefix:
				if strings.HasPrefix(hostname, value) {
					return rule.provider, true
				}
			case hostPort:
				if host == value {
					return rule.provider, true
				}
			}
		}
	}
	return core.ProviderUnknown, false
}

// qwenCNBeijingRegion 是 Qwen 唯一允许的地域段（统一 SDD 4.6.1 Qwen 例外 / 11.1 验收行）。
const qwenCNBeijingRegion = "cn-beijing"

// qwenRegion 返回 *.maas.aliyuncs.com host 的地域段。
//
// 地域段取 `maas` 前一段（端点形如 `{workspace-id}.{region}.maas.aliyuncs.com`，
// 例如 `ws-xxx.us-east-1.maas.aliyuncs.com` 的地域段是 `us-east-1`）。
// ok=false 表示 host 不是 maas 后缀，不做地域校验。
func qwenRegion(hostname string) (region string, ok bool) {
	labels := strings.Split(hostname, ".")
	for i := 0; i+2 < len(labels); i++ {
		if labels[i] == "maas" && labels[i+1] == "aliyuncs" && labels[i+2] == "com" {
			if i == 0 {
				// `maas.aliyuncs.com` 裸域：无地域段信息，按无法验证处理（返回空段）。
				return "", true
			}
			return labels[i-1], true
		}
	}
	return "", false
}

// RegionBindingError 是 Qwen 地域绑定不匹配错误（统一 SDD 3.3 两条实现约束 / 11.1）。
// ⛔ 显式报错、不静默换端点：用北京 Key 调弗吉尼亚 endpoint 上游只会回 401 invalid_api_key。
type RegionBindingError struct {
	Host   string
	Region string
}

// ErrRegionMismatch 是 Qwen 地域绑定不匹配的哨兵错误（可 errors.Is 判定）。
var ErrRegionMismatch = errors.New("detect: Qwen 地域绑定不匹配")

func (e *RegionBindingError) Error() string {
	return fmt.Sprintf(
		"Qwen API Key 与地域强绑定：host %q 的地域段 %q 非 %s ⇒ 拒绝识别（⛔ 不静默换端点）",
		e.Host, e.Region, qwenCNBeijingRegion)
}

// Unwrap 让 errors.Is 命中 ErrRegionMismatch。
func (e *RegionBindingError) Unwrap() error { return ErrRegionMismatch }
