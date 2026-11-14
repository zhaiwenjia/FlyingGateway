// 日期: 2026-11-14 12:02:39.120
// 作者: Zhai Wenjia

package detect

import (
	"strings"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// prefixRule 是 model 名前缀信号的一行（统一 SDD 4.6.2，逐行照抄 11 家）。
// 同一行多个前缀任一命中即该行命中。
//
// ⚠️ 前缀信号有歧义（`deepseek-r1` 既是 DeepSeek 官方模型也常作 Ollama 本地模型名，
// `qwen2` 既是 Qwen 模型也是 Ollama 模型家族）⇒ 只作兜底，host 冲突时以 host 为准
// （统一 SDD 4.6.2 / 4.6.3）。
//
// ⛔ 用有序切片而非 map：同名前缀在多行出现时（`deepseek-r1` 同时在 Qwen 行与
// Ollama 行），必须有一个确定的消歧序 ⇒ 按 4.6.2 表格行序、行内条目序首中即停
// （统一 SDD 1.4.3 可复现；不许让 map 遍历顺序决定归属）。
type prefixRule struct {
	prefixes []string
	provider core.Provider
}

// prefixRules 是 model 名前缀信号表（统一 SDD 4.6.2，行序即表序）。
var prefixRules = []prefixRule{
	// 4.6.2 第 1 行：`qwen` / `qwen3` / `qwq` / `deepseek-r1`（通义）⇒ Qwen。
	{prefixes: []string{"qwen", "qwen3", "qwq", "deepseek-r1"}, provider: core.ProviderQwen},
	// 第 2 行：`kimi` / `moonshot` ⇒ Kimi。
	{prefixes: []string{"kimi", "moonshot"}, provider: core.ProviderMoonshot},
	// 第 3 行：`glm` ⇒ 智谱。
	{prefixes: []string{"glm"}, provider: core.ProviderZhipu},
	// 第 4 行：`deepseek` / `deepseek-chat` / `deepseek-reasoner` ⇒ DeepSeek。
	{prefixes: []string{"deepseek", "deepseek-chat", "deepseek-reasoner"}, provider: core.ProviderDeepSeek},
	// 第 5 行：`mimo` ⇒ 小米 MiMo。
	{prefixes: []string{"mimo"}, provider: core.ProviderXiaomi},
	// 第 6 行：`MiniMax` / `minimax` / `abab` ⇒ MiniMax。
	{prefixes: []string{"MiniMax", "minimax", "abab"}, provider: core.ProviderMiniMax},
	// 第 7 行：`gpt-` / `o1` / `o3` / `o4` / `chatgpt-` ⇒ OpenAI。
	{prefixes: []string{"gpt-", "o1", "o3", "o4", "chatgpt-"}, provider: core.ProviderOpenAI},
	// 第 8 行：`claude-` ⇒ Claude。
	{prefixes: []string{"claude-"}, provider: core.ProviderAnthropic},
	// 第 9 行：`grok-` ⇒ Grok。
	{prefixes: []string{"grok-"}, provider: core.ProviderXAI},
	// 第 10 行：`gemini-` ⇒ Gemini。
	{prefixes: []string{"gemini-"}, provider: core.ProviderGemini},
	// 第 11 行：`llama` / `qwen2` / `mistral` / `deepseek-r1` / `gemma` / `phi` 等（Ollama 库名）⇒ Ollama。
	{prefixes: []string{"llama", "qwen2", "mistral", "deepseek-r1", "gemma", "phi"}, provider: core.ProviderOllama},
}

// matchPrefix 按前缀表行序匹配 model 名（大小写不敏感），返回供应商与是否命中。
// 消歧规则：表序首中即停（`deepseek-r1` 裸名落在 Qwen 行 —— 4.6.2 表序），
// 真正的歧义消解靠 host 优先（统一 SDD 4.6.3）。
func matchPrefix(model string) (core.Provider, bool) {
	name := strings.ToLower(model)
	for _, rule := range prefixRules {
		for _, prefix := range rule.prefixes {
			if strings.HasPrefix(name, strings.ToLower(prefix)) {
				return rule.provider, true
			}
		}
	}
	return core.ProviderUnknown, false
}
