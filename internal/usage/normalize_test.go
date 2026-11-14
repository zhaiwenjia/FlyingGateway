// 日期: 2026-11-14 11:54:49.169
// 作者: Zhai Wenjia

package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// 本文件覆盖逐家归一化口径（SDD 3.2 / 4.4.2 / 4.4.3 / 11.1 usage 断言行）。
// 样本值逐字取自 SDD 11.1 断言行与任务书给定口径（测试数字是对账期望值，不是运行时价）。

// TestNormalizeAnthropicResidual 钉住 Anthropic 残值语义（11.1「Anthropic 残值」两例）：
// ① input_tokens=25, cache_creation=0, cache_read=0 ⇒ 归一 InputTokens=25；
// ② input_tokens=50, cache_read=200000 ⇒ 归一 InputTokens=200050（⛔ 不是 50）。
// 错误实现（直接取 input_tokens）会在 ② 上得 50 ⇒ 断言会红（恒假样本已核）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeAnthropicResidual -v
func TestNormalizeAnthropicResidual(t *testing.T) {
	// 例①：残值即全部输入（SDD 3.2 / 11.1：input_tokens=25 ⇒ InputTokens=25）。
	u := Normalize(core.ProviderAnthropic, core.APIShapeAnthropicMessages,
		json.RawMessage(`{"input_tokens":25,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":10}`))
	if u.InputTokens != 25 {
		t.Fatalf("例① InputTokens = %d，期望 25（11.1 Anthropic 残值）", u.InputTokens)
	}
	if u.InputTokensUncached != 25 {
		t.Fatalf("例① InputTokensUncached = %d，期望 25（4.4.3：Anthropic 系 Uncached = input_tokens 残值）", u.InputTokensUncached)
	}

	// 例②：总输入 = 50 + 0 + 200000 = 200050（SDD 3.2 例：⛔ 不是 50）。
	u = Normalize(core.ProviderAnthropic, core.APIShapeAnthropicMessages,
		json.RawMessage(`{"input_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":200000,"output_tokens":30}`))
	if u.InputTokens != 200050 {
		t.Fatalf("例② InputTokens = %d，期望 200050（3.2：input_tokens=50, cache_read=200000 ⇒ 200050，⛔ 非 50）", u.InputTokens)
	}
	if u.InputTokensUncached != 50 {
		t.Fatalf("例② InputTokensUncached = %d，期望 50（残值本身）", u.InputTokensUncached)
	}
	if u.CacheReadTokens != 200000 {
		t.Fatalf("例② CacheReadTokens = %d，期望 200000", u.CacheReadTokens)
	}
}

// TestNormalizeMiniMaxAnthropicShape 钉住 MiniMax Anthropic 形同 Claude 残值语义（任务书 MiniMax 条）：
// 50 + 200000 ⇒ InputTokens=200050，⛔ 非 50。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeMiniMaxAnthropicShape -v
func TestNormalizeMiniMaxAnthropicShape(t *testing.T) {
	u := Normalize(core.ProviderMiniMax, core.APIShapeAnthropicMessages,
		json.RawMessage(`{"input_tokens":50,"cache_creation_input_tokens":8,"cache_read_input_tokens":200000,"output_tokens":30}`))
	if u.InputTokens != 200058 {
		t.Fatalf("InputTokens = %d，期望 200058 = 50+8+200000（Anthropic 残值语义）", u.InputTokens)
	}
	if u.InputTokensUncached != 50 || u.CacheWriteTokens != 8 || u.CacheReadTokens != 200000 {
		t.Fatalf("残值三分不对：Uncached=%d(期望50) Write=%d(期望8) Read=%d(期望200000)",
			u.InputTokensUncached, u.CacheWriteTokens, u.CacheReadTokens)
	}
}

// TestNormalizeKimiThreeWay 钉住 Kimi 三分互斥（11.1「Kimi 三分」）：
// prompt_tokens=309, cached=53, cache_write=256 ⇒ InputTokens=309、CacheRead=53、
// CacheWrite=256、Uncached=0（三者互斥，⛔ 不累加）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeKimiThreeWay -v
func TestNormalizeKimiThreeWay(t *testing.T) {
	u := Normalize(core.ProviderMoonshot, core.APIShapeOpenAIChat,
		json.RawMessage(`{"prompt_tokens":309,"cached_tokens":53,"prompt_tokens_details":{"cached_tokens":53,"cache_write_tokens":256},"completion_tokens":12,"total_tokens":321}`))
	if u.InputTokens != 309 {
		t.Fatalf("InputTokens = %d，期望 309（prompt_tokens 是总数）", u.InputTokens)
	}
	if u.CacheReadTokens != 53 {
		t.Fatalf("CacheReadTokens = %d，期望 53（顶层 cached_tokens 与 details 同值取其一，⛔ 不累加为 106）", u.CacheReadTokens)
	}
	if u.CacheWriteTokens != 256 {
		t.Fatalf("CacheWriteTokens = %d，期望 256（cache_write_tokens 单列）", u.CacheWriteTokens)
	}
	if u.InputTokensUncached != 0 {
		t.Fatalf("InputTokensUncached = %d，期望 0（309 − 53 − 256 = 0，三者互斥）", u.InputTokensUncached)
	}
}

// TestNormalizeDeepSeekCacheHitDedup 钉住 DeepSeek 二选一不累加（11.1「DeepSeek 二选一」）：
// 同时有 prompt_cache_hit_tokens=40 与 prompt_tokens_details.cached_tokens=40 ⇒ 取其一，
// CacheRead=40（⛔ 不是 80）；prompt_tokens=100 = 命中 40 + 未命中 60。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeDeepSeekCacheHitDedup -v
func TestNormalizeDeepSeekCacheHitDedup(t *testing.T) {
	u := Normalize(core.ProviderDeepSeek, core.APIShapeOpenAIChat,
		json.RawMessage(`{"prompt_tokens":100,"prompt_cache_hit_tokens":40,"prompt_cache_miss_tokens":60,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens":7,"total_tokens":107}`))
	if u.CacheReadTokens != 40 {
		t.Fatalf("CacheReadTokens = %d，期望 40（同值取其一，⛔ 不累加为 80）", u.CacheReadTokens)
	}
	if u.InputTokens != 100 {
		t.Fatalf("InputTokens = %d，期望 100（prompt_tokens = 命中 + 未命中，总输入直接用）", u.InputTokens)
	}
	if u.InputTokensUncached != 60 {
		t.Fatalf("InputTokensUncached = %d，期望 60（prompt_cache_miss_tokens）", u.InputTokensUncached)
	}
	if u.CacheWriteTokens != 0 {
		t.Fatalf("CacheWriteTokens = %d，期望 0（DeepSeek 无写入字段 ⇒ 记 0）", u.CacheWriteTokens)
	}
}

// TestNormalizeOllamaNative 钉住 Ollama 平铺口径（11.1「Ollama 平铺」）：
// prompt_eval_count=26, prompt_eval_cached_count=13 ⇒ InputTokens=26、CacheRead=13、Uncached=13、
// eval_count=13 ⇒ Output=13。缓存命中不减少 prompt_eval_count（4.4.3 Ollama 的坑）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeOllamaNative -v
func TestNormalizeOllamaNative(t *testing.T) {
	u := Normalize(core.ProviderOllama, core.APIShapeOllamaNative,
		json.RawMessage(`{"prompt_eval_count":26,"prompt_eval_cached_count":13,"eval_count":13}`))
	if u.InputTokens != 26 || u.CacheReadTokens != 13 || u.InputTokensUncached != 13 || u.OutputTokens != 13 {
		t.Fatalf("Ollama 平铺归一不对：Input=%d(26) Read=%d(13) Uncached=%d(13) Output=%d(13)",
			u.InputTokens, u.CacheReadTokens, u.InputTokensUncached, u.OutputTokens)
	}
}

// TestNormalizeGeminiThoughts 钉住 Gemini thoughts 口径（3.2 Gemini 行 / 11.1「Gemini 含 thoughts」）：
// OutputTokens = candidatesTokenCount + thoughtsTokenCount（thoughts 独立加项、⛔ 不含在 candidates 内、
// ⛔ 不重复加）；InputTokens = promptTokenCount + toolUsePromptTokenCount；TotalTokens 保留原报 ⛔ 不重算。
// 样本取 prompt=50、toolUse=10、candidates=100、thoughts=40、total=200：
// 期望 Output=140（⛔ 180 = thoughts 加两次的错误实现、⛔ 100 = 只取 candidates 的错误实现都会红）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeGeminiThoughts -v
func TestNormalizeGeminiThoughts(t *testing.T) {
	u := Normalize(core.ProviderGemini, core.APIShapeGeminiNative,
		json.RawMessage(`{"promptTokenCount":50,"toolUsePromptTokenCount":10,"candidatesTokenCount":100,"thoughtsTokenCount":40,"cachedContentTokenCount":0,"totalTokenCount":200}`))
	if u.OutputTokens != 140 {
		t.Fatalf("OutputTokens = %d，期望 140 = 100 + 40（thoughts 独立加项，3.2：不含在 candidates 内）", u.OutputTokens)
	}
	if u.InputTokens != 60 {
		t.Fatalf("InputTokens = %d，期望 60 = 50 + 10（promptTokenCount + toolUsePromptTokenCount）", u.InputTokens)
	}
	if u.ToolUsePromptTokens != 10 {
		t.Fatalf("ToolUsePromptTokens = %d，期望 10（原报保留）", u.ToolUsePromptTokens)
	}
	if u.TotalTokens != 200 {
		t.Fatalf("TotalTokens = %d，期望 200（provider 原报总值保留，⛔ 不重算）", u.TotalTokens)
	}

	// 带缓存命中的样本：cachedContentTokenCount ⇒ CacheRead，缓存写 0（3.2：显式缓存按存储费计）。
	u = Normalize(core.ProviderGemini, core.APIShapeGeminiNative,
		json.RawMessage(`{"promptTokenCount":1000,"candidatesTokenCount":20,"thoughtsTokenCount":5,"cachedContentTokenCount":700,"totalTokenCount":1025}`))
	if u.CacheReadTokens != 700 || u.CacheWriteTokens != 0 || u.InputTokensUncached != 300 {
		t.Fatalf("Gemini 缓存归一不对：Read=%d(700) Write=%d(0) Uncached=%d(300)",
			u.CacheReadTokens, u.CacheWriteTokens, u.InputTokensUncached)
	}
}

// TestNormalizeMiniMaxNoCache 钉住 MiniMax 无缓存字段 ⇒ 读写记 0（任务书 MiniMax 条 / 3.2 MiniMax 行）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeMiniMaxNoCache -v
func TestNormalizeMiniMaxNoCache(t *testing.T) {
	u := Normalize(core.ProviderMiniMax, core.APIShapeMiniMaxNativeV2,
		json.RawMessage(`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"total_characters":999}`))
	if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		t.Fatalf("MiniMax 无缓存字段 ⇒ 读写记 0：Read=%d Write=%d", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.InputTokens != 100 || u.InputTokensUncached != 100 || u.OutputTokens != 20 {
		t.Fatalf("MiniMax 归一不对：Input=%d(100) Uncached=%d(100) Output=%d(20)",
			u.InputTokens, u.InputTokensUncached, u.OutputTokens)
	}
	if u.CacheReadMode != core.CacheModeNone {
		t.Fatalf("CacheReadMode = %q，期望 none（无缓存命中）", u.CacheReadMode)
	}
	if !strings.Contains(string(u.Raw), `"total_characters":999`) {
		t.Fatalf("Raw 未逐字保留原文（total_characters 应随 Raw 保留）：%s", u.Raw)
	}
}

// TestNormalizeOpenAIChat 钉住 OpenAI Chat 口径（4.4.2 OpenAI Chat 行）：
// prompt_tokens 总输入、prompt_tokens_details.cached_tokens 二级命中、
// completion_tokens_details.reasoning_tokens 单列；Uncached = prompt − cached − write（4.4.3 R1 公式）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeOpenAIChat -v
func TestNormalizeOpenAIChat(t *testing.T) {
	u := Normalize(core.ProviderOpenAI, core.APIShapeOpenAIChat,
		json.RawMessage(`{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"prompt_tokens_details":{"cached_tokens":600,"cache_write_tokens":100},"completion_tokens_details":{"reasoning_tokens":30}}`))
	if u.InputTokens != 1000 || u.CacheReadTokens != 600 || u.CacheWriteTokens != 100 {
		t.Fatalf("OpenAI Chat 输入侧不对：Input=%d(1000) Read=%d(600) Write=%d(100)",
			u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.InputTokensUncached != 300 {
		t.Fatalf("InputTokensUncached = %d，期望 300 = 1000 − 600 − 100（4.4.3 R1 公式）", u.InputTokensUncached)
	}
	if u.OutputTokens != 50 || u.ReasoningTokens != 30 || u.TotalTokens != 1050 {
		t.Fatalf("OpenAI Chat 输出侧不对：Output=%d(50) Reasoning=%d(30) Total=%d(1050)",
			u.OutputTokens, u.ReasoningTokens, u.TotalTokens)
	}

	// 无缓存明细字段 ⇒ 记 0（3.2「原始 usage 无对应字段 ⇒ 归一化记 0」）。
	u = Normalize(core.ProviderOpenAI, core.APIShapeOpenAIChat,
		json.RawMessage(`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`))
	if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 || u.InputTokensUncached != 10 {
		t.Fatalf("无缓存字段应记 0：Read=%d Write=%d Uncached=%d(10)", u.CacheReadTokens, u.CacheWriteTokens, u.InputTokensUncached)
	}
}

// TestNormalizeOpenAIResponses 钉住 Responses 口径（4.4.2 OpenAI Responses 行）：
// input_tokens / output_tokens 总输入输出、input_tokens_details.* 二级缓存明细。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeOpenAIResponses -v
func TestNormalizeOpenAIResponses(t *testing.T) {
	u := Normalize(core.ProviderOpenAI, core.APIShapeOpenAIResponses,
		json.RawMessage(`{"input_tokens":1000,"output_tokens":80,"total_tokens":1080,"input_tokens_details":{"cached_tokens":400,"cache_write_tokens":100},"output_tokens_details":{"reasoning_tokens":20}}`))
	if u.InputTokens != 1000 || u.CacheReadTokens != 400 || u.CacheWriteTokens != 100 {
		t.Fatalf("Responses 输入侧不对：Input=%d(1000) Read=%d(400) Write=%d(100)",
			u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.InputTokensUncached != 500 {
		t.Fatalf("InputTokensUncached = %d，期望 500 = 1000 − 400 − 100", u.InputTokensUncached)
	}
	if u.OutputTokens != 80 || u.ReasoningTokens != 20 {
		t.Fatalf("Responses 输出侧不对：Output=%d(80) Reasoning=%d(20)", u.OutputTokens, u.ReasoningTokens)
	}

	// TTL 细分对象形态（4.4.2：cache_write_tokens TTL 细分 ttl_30m_tokens / ttl_12h_tokens，互斥分段求和）。
	u = Normalize(core.ProviderOpenAI, core.APIShapeOpenAIResponses,
		json.RawMessage(`{"input_tokens":300,"output_tokens":10,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":{"ttl_30m_tokens":120,"ttl_12h_tokens":80}}}`))
	if u.CacheWriteTokens != 200 {
		t.Fatalf("TTL 细分求和 = %d，期望 200 = 120 + 80（30m 与 12h 互斥分段，⛔ 非同值双报）", u.CacheWriteTokens)
	}
}

// TestNormalizeGrokChat 钉住 Grok Chat 口径（3.2 xAI/Grok（Chat）行）：
// cached_prompt_text_tokens 命中、写入 0、cost_in_usd_ticks 只留 Raw（不入 Usage）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeGrokChat -v
func TestNormalizeGrokChat(t *testing.T) {
	u := Normalize(core.ProviderXAI, core.APIShapeOpenAIChat,
		json.RawMessage(`{"prompt_tokens":500,"cached_prompt_text_tokens":300,"completion_tokens":40,"reasoning_tokens":10,"total_tokens":540,"cost_in_usd_ticks":12300000000}`))
	if u.CacheReadTokens != 300 || u.CacheWriteTokens != 0 {
		t.Fatalf("Grok Chat 缓存不对：Read=%d(300) Write=%d(0)", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.InputTokens != 500 || u.InputTokensUncached != 200 || u.OutputTokens != 40 {
		t.Fatalf("Grok Chat 归一不对：Input=%d(500) Uncached=%d(200) Output=%d(40)",
			u.InputTokens, u.InputTokensUncached, u.OutputTokens)
	}
	if u.ReasoningTokens != 10 {
		t.Fatalf("ReasoningTokens = %d，期望 10（reasoning_tokens 单列）", u.ReasoningTokens)
	}
	if !strings.Contains(string(u.Raw), `"cost_in_usd_ticks":12300000000`) {
		t.Fatalf("cost_in_usd_ticks 应随 Raw 保留（3.2 xAI 条），实际 Raw=%s", u.Raw)
	}
}

// TestNormalizeGrokResponses 钉住 Grok Responses 口径（任务书：input_tokens/output_tokens 无缓存）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeGrokResponses -v
func TestNormalizeGrokResponses(t *testing.T) {
	u := Normalize(core.ProviderXAI, core.APIShapeOpenAIResponses,
		json.RawMessage(`{"input_tokens":500,"output_tokens":40,"total_tokens":540}`))
	if u.InputTokens != 500 || u.OutputTokens != 40 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		t.Fatalf("Grok Responses 归一不对：Input=%d(500) Output=%d(40) Read=%d(0) Write=%d(0)",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
}

// TestNormalizeQwenCompatCacheMode 钉住 Qwen 兼容口径（任务书：cached_tokens + cache_creation_input_tokens；
// 显式 / 隐式按 raw 是否含对应字段标 CacheReadMode）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeQwenCompatCacheMode -v
func TestNormalizeQwenCompatCacheMode(t *testing.T) {
	// 含 cache_creation_input_tokens ⇒ 显式缓存通道（explicit）。
	u := Normalize(core.ProviderQwen, core.APIShapeOpenAIChat,
		json.RawMessage(`{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":300,"cache_creation_input_tokens":250}}`))
	if u.CacheReadMode != core.CacheModeExplicit {
		t.Fatalf("CacheReadMode = %q，期望 explicit（raw 含 cache_creation_input_tokens）", u.CacheReadMode)
	}
	if u.CacheReadTokens != 300 || u.CacheWriteTokens != 250 || u.InputTokensUncached != 450 {
		t.Fatalf("Qwen 显式归一不对：Read=%d(300) Write=%d(250) Uncached=%d(450)",
			u.CacheReadTokens, u.CacheWriteTokens, u.InputTokensUncached)
	}

	// 只有 cached_tokens ⇒ 隐式自动命中（implicit）。
	u = Normalize(core.ProviderQwen, core.APIShapeOpenAIChat,
		json.RawMessage(`{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":300}}`))
	if u.CacheReadMode != core.CacheModeImplicit {
		t.Fatalf("CacheReadMode = %q，期望 implicit（仅 cached_tokens）", u.CacheReadMode)
	}
	if u.CacheWriteTokens != 0 || u.InputTokensUncached != 700 {
		t.Fatalf("Qwen 隐式归一不对：Write=%d(0) Uncached=%d(700)", u.CacheWriteTokens, u.InputTokensUncached)
	}

	// 无命中 ⇒ none。
	u = Normalize(core.ProviderQwen, core.APIShapeOpenAIChat,
		json.RawMessage(`{"prompt_tokens":10,"completion_tokens":2}`))
	if u.CacheReadMode != core.CacheModeNone {
		t.Fatalf("CacheReadMode = %q，期望 none（无命中量）", u.CacheReadMode)
	}
}

// TestNormalizeZhipu 钉住智谱口径（3.2 智谱行）：命中单列、思考计入输出计费、无写入。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeZhipu -v
func TestNormalizeZhipu(t *testing.T) {
	u := Normalize(core.ProviderZhipu, core.APIShapeOpenAIChat,
		json.RawMessage(`{"prompt_tokens":800,"completion_tokens":60,"total_tokens":860,"prompt_tokens_details":{"cached_tokens":500},"completion_tokens_details":{"reasoning_tokens":25}}`))
	if u.CacheReadTokens != 500 || u.CacheWriteTokens != 0 {
		t.Fatalf("智谱缓存不对：Read=%d(500) Write=%d(0，隐式缓存无写入计费)", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.OutputTokens != 60 {
		t.Fatalf("OutputTokens = %d，期望 60（思考计入输出计费：completion_tokens 已含 reasoning，⛔ 不再加）", u.OutputTokens)
	}
	if u.ReasoningTokens != 25 {
		t.Fatalf("ReasoningTokens = %d，期望 25（单列）", u.ReasoningTokens)
	}
}

// TestNormalizeMiMo 钉住 MiMo 口径（任务书：cached_tokens 命中、无写入）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeMiMo -v
func TestNormalizeMiMo(t *testing.T) {
	u := Normalize(core.ProviderXiaomi, core.APIShapeOpenAIChat,
		json.RawMessage(`{"prompt_tokens":400,"completion_tokens":30,"total_tokens":430,"prompt_tokens_details":{"cached_tokens":150}}`))
	if u.CacheReadTokens != 150 || u.CacheWriteTokens != 0 {
		t.Fatalf("MiMo 缓存不对：Read=%d(150) Write=%d(0)", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.InputTokens != 400 || u.InputTokensUncached != 250 || u.OutputTokens != 30 {
		t.Fatalf("MiMo 归一不对：Input=%d(400) Uncached=%d(250) Output=%d(30)",
			u.InputTokens, u.InputTokensUncached, u.OutputTokens)
	}
}

// TestNormalizeOllamaV1 钉住 Ollama /v1 口径（4.4.2 Ollama /v1 行：无缓存明细 ⇒ 读写记 0）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeOllamaV1 -v
func TestNormalizeOllamaV1(t *testing.T) {
	u := Normalize(core.ProviderOllama, core.APIShapeOpenAIChat,
		json.RawMessage(`{"prompt_tokens":26,"completion_tokens":13,"total_tokens":39,"prompt_tokens_details":{"cached_tokens":13}}`))
	if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		t.Fatalf("Ollama /v1 无缓存明细 ⇒ 读写记 0：Read=%d Write=%d", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.InputTokens != 26 || u.InputTokensUncached != 26 || u.OutputTokens != 13 {
		t.Fatalf("Ollama /v1 归一不对：Input=%d(26) Uncached=%d(26) Output=%d(13)",
			u.InputTokens, u.InputTokensUncached, u.OutputTokens)
	}
}

// TestNormalizeQwenNative 钉住 Qwen 原生口径（4.4.2 Qwen 原生行：usage.input_tokens / output_tokens，无缓存记 0）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeQwenNative -v
func TestNormalizeQwenNative(t *testing.T) {
	u := Normalize(core.ProviderQwen, core.APIShapeQwenNative,
		json.RawMessage(`{"input_tokens":700,"output_tokens":35,"total_tokens":735}`))
	if u.InputTokens != 700 || u.OutputTokens != 35 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		t.Fatalf("Qwen 原生归一不对：Input=%d(700) Output=%d(35) Read=%d(0) Write=%d(0)",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
}

// TestNormalizeParseFailure 钉住解析失败口径（任务书：解析失败不 panic、返回 Estimated=true 的保守结果、
// Raw 保留原文）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeParseFailure -v
func TestNormalizeParseFailure(t *testing.T) {
	cases := map[string]json.RawMessage{
		"非法 JSON": json.RawMessage(`{"prompt_tokens":`),
		"空原文":     json.RawMessage(``),
		"null":    json.RawMessage(`null`),
		"非对象":     json.RawMessage(`[1,2,3]`),
		"字段类型错":   json.RawMessage(`{"prompt_tokens":"x"}`),
	}
	for name, raw := range cases {
		u := Normalize(core.ProviderOpenAI, core.APIShapeOpenAIChat, raw)
		if u == nil {
			t.Fatalf("%s：Normalize 返回 nil，解析失败应返回保守结果", name)
		}
		if !u.Estimated {
			t.Fatalf("%s：Estimated 应为 true（保守结果）", name)
		}
		if string(u.Raw) != string(raw) {
			t.Fatalf("%s：Raw 应逐字保留原文：%q ≠ %q", name, u.Raw, raw)
		}
		if u.InputTokens != 0 || u.CacheReadTokens != 0 || u.OutputTokens != 0 {
			t.Fatalf("%s：保守结果应全 0：Input=%d Read=%d Output=%d", name, u.InputTokens, u.CacheReadTokens, u.OutputTokens)
		}
	}
}

// TestNormalizeRawPreserved 钉住原始数据保留（3.2「原始数据保留」）：成功解析也逐字保留原文。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeRawPreserved -v
func TestNormalizeRawPreserved(t *testing.T) {
	raw := json.RawMessage(`{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}`)
	u := Normalize(core.ProviderOpenAI, core.APIShapeOpenAIChat, raw)
	if string(u.Raw) != string(raw) {
		t.Fatalf("Raw = %s，期望逐字保留 %s", u.Raw, raw)
	}
}

// ---- 真实样本回归层（testdata/real/：来源 / 获取日期 / 真实性依据逐条见 testdata/real/README.md）----

// loadRealUsage 读取 testdata/real/ 下的真实 usage 样本（原封不动照抄来源，⛔ 非合成）。
func loadRealUsage(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "real", name))
	if err != nil {
		t.Fatalf("读真实样本 %s 失败: %v", name, err)
	}
	return json.RawMessage(b)
}

// TestNormalizeRealSamples 钉住归一化口径在 11 家供应商真实样本上成立（期望值逐字推自样本数值）：
// ① Anthropic 系残值合成 Input = input + cache_creation + cache_read（⛔ 不取 input_tokens 残值）；
// ② Kimi 三分互斥 Uncached = prompt − cached − write；DeepSeek 有 prompt_cache_miss_tokens 取 miss、
//
//	无则 prompt − hit；OpenAI 系 Uncached = prompt − cached − write；
//
// ③ Gemini Output = candidates + thoughts（独立加项），Total 保留原报 ⛔ 不重算。
// 错误实现会红：Anthropic 直接取 input_tokens ⇒ cachewrite 样本 Input=4 ≠ 880 红；
// Kimi 同值双报累加 ⇒ Read=20 ≠ 10 红；Gemini 不加 thoughts ⇒ Out=80 ≠ 655 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeRealSamples -v
func TestNormalizeRealSamples(t *testing.T) {
	cases := []struct {
		name               string
		file               string
		provider           core.Provider
		shape              core.APIShape
		in, read, write    int64
		unc, out, rea, tot int64
	}{
		// anthropic_usage_cachewrite.json（litellm 真实捕获，写缓存轮）：
		// Input = 4 + 876 + 0 = 880（残值合成）；Uncached = 残值 4；Write = 876；Out = 235。
		{"anthropic-cachewrite", "anthropic_usage_cachewrite.json", core.ProviderAnthropic, core.APIShapeAnthropicMessages, 880, 0, 876, 4, 235, 0, 0},
		// anthropic_usage_cachehit.json（litellm 真实捕获，命中轮）：Input = 4 + 0 + 873 = 877；Read = 873。
		{"anthropic-cachehit", "anthropic_usage_cachehit.json", core.ProviderAnthropic, core.APIShapeAnthropicMessages, 877, 873, 0, 4, 254, 0, 0},
		// openai_responses_usage.json（litellm 真实捕获）：命中/写入均 0 ⇒ Uncached=309；Reasoning=53；Total=427。
		{"openai-responses", "openai_responses_usage.json", core.ProviderOpenAI, core.APIShapeOpenAIResponses, 309, 0, 0, 309, 118, 53, 427},
		// openai_chat_usage_azure.json（litellm 真实捕获，Chat 形）：Input=19/Out=5/Total=24；
		// 未知字段 latency_checkpoint 随 Raw 保留、⛔ 不干扰解析。
		{"openai-chat", "openai_chat_usage_azure.json", core.ProviderOpenAI, core.APIShapeOpenAIChat, 19, 0, 0, 19, 5, 0, 24},
		// gemini_usage_metadata.json（litellm 真实捕获）：Out = 80 + 575 = 655（thoughts 独立加项）；
		// Total=1013 保留原报（358+80+575 自洽）。
		{"gemini", "gemini_usage_metadata.json", core.ProviderGemini, core.APIShapeGeminiNative, 358, 0, 0, 358, 655, 0, 1013},
		// deepseek_usage.json（官方文档示例）：miss 字段 7 ⇒ Uncached=7（⛔ 非 prompt−hit 兜底路径）。
		{"deepseek", "deepseek_usage.json", core.ProviderDeepSeek, core.APIShapeOpenAIChat, 7, 0, 0, 7, 11, 0, 18},
		// kimi_usage.json（官方文档示例）：prompt=19 = 10 命中 + 0 写 + 9 未命中（三分互斥）。
		{"kimi", "kimi_usage.json", core.ProviderMoonshot, core.APIShapeOpenAIChat, 19, 10, 0, 9, 21, 0, 40},
		// zhipu_usage.json（官方文档示例）：命中 1024 单列；completion 含 reasoning ⇒ Out=241 ⛔ 不二次加。
		{"zhipu", "zhipu_usage.json", core.ProviderZhipu, core.APIShapeOpenAIChat, 1075, 1024, 0, 51, 241, 0, 1316},
		// qwen_compat_usage.json（官方文档示例）：命中 2048、无写入 ⇒ Uncached=971（隐式缓存档）。
		{"qwen-compat", "qwen_compat_usage.json", core.ProviderQwen, core.APIShapeOpenAIChat, 3019, 2048, 0, 971, 104, 0, 3123},
		// qwen_native_usage.json（官方文档示例）：⚠️ 2026-11-14 实测修正——原生样本带
		// prompt_tokens_details.cached_tokens=2048，有字段就解析 ⇒ Read=2048、Uncached=971。
		{"qwen-native", "qwen_native_usage.json", core.ProviderQwen, core.APIShapeQwenNative, 3019, 2048, 0, 971, 101, 0, 3120},
		// minimax_usage.json（官方文档示例）：无缓存字段 ⇒ 读写 0；reasoning 214 单列；total_characters 随 Raw。
		{"minimax", "minimax_usage.json", core.ProviderMiniMax, core.APIShapeMiniMaxNativeV2, 26, 0, 0, 26, 223, 214, 249},
		// mimo_usage_asr.json（官方文档示例）：命中 45 ⇒ Uncached = 46 − 45 = 1；seconds=4 随 Raw。
		{"mimo-asr", "mimo_usage_asr.json", core.ProviderXiaomi, core.APIShapeOpenAIChat, 46, 45, 0, 1, 20, 0, 66},
		// mimo_usage_audio.json（官方文档示例）：无命中 ⇒ Uncached=86；reasoning_tokens=66 入
		// Reasoning（⚠️ 2026-11-14 实测修正：SDD 4.3.10 字段表列了该字段、实现原漏读）。
		{"mimo-audio", "mimo_usage_audio.json", core.ProviderXiaomi, core.APIShapeOpenAIChat, 86, 0, 0, 86, 98, 66, 184},
		// grok_responses_usage.json（官方文档示例）：Responses 树；cost_in_usd_ticks=158500 随 Raw 保留。
		{"grok-responses", "grok_responses_usage.json", core.ProviderXAI, core.APIShapeOpenAIResponses, 199, 0, 0, 199, 1, 0, 200},
		// ollama_usage_real.json（排障实录贴真机响应）：prompt_eval_count=40 含命中 ⇒ Uncached=40−24=16；
		// 无总值原报 ⇒ Total=0。
		{"ollama-real", "ollama_usage_real.json", core.ProviderOllama, core.APIShapeOllamaNative, 40, 24, 0, 16, 10, 0, 0},
		// ollama_usage_docs.json（官方文档示例）：无 prompt_eval_cached_count ⇒ Read=0、Uncached=11。
		{"ollama-docs", "ollama_usage_docs.json", core.ProviderOllama, core.APIShapeOllamaNative, 11, 0, 0, 11, 18, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := Normalize(c.provider, c.shape, loadRealUsage(t, c.file))
			if u.Estimated {
				t.Fatalf("Estimated=true，期望真实样本解析成功（%s）", c.file)
			}
			if u.InputTokens != c.in || u.CacheReadTokens != c.read || u.CacheWriteTokens != c.write ||
				u.InputTokensUncached != c.unc || u.OutputTokens != c.out || u.ReasoningTokens != c.rea || u.TotalTokens != c.tot {
				t.Fatalf("%s 归一 = Input%d/Read%d/Write%d/Unc%d/Out%d/Reason%d/Total%d，期望 %d/%d/%d/%d/%d/%d/%d",
					c.file, u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.InputTokensUncached,
					u.OutputTokens, u.ReasoningTokens, u.TotalTokens, c.in, c.read, c.write, c.unc, c.out, c.rea, c.tot)
			}
			if len(u.Raw) == 0 {
				t.Fatalf("%s：Raw 应逐字保留原文", c.file)
			}
		})
	}
}

// TestNormalizeGrokChatCachedDetailsFallback 钉住 Grok Chat 命中字段的现行形态：
// prompt_tokens_details.cached_tokens（2026-11-14 实测修正，xAI 现行文档与真实样本口径），
// 与旧形态 cached_prompt_text_tokens 两形态取其一 ⛔ 不累加。
// 错误实现会红：只认旧字段 ⇒ 62 ≠ 0 红；两字段相加 ⇒ 124 ≠ 62 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/usage/ -run TestNormalizeGrokChatCachedDetailsFallback -v
func TestNormalizeGrokChatCachedDetailsFallback(t *testing.T) {
	u := Normalize(core.ProviderXAI, core.APIShapeOpenAIChat, json.RawMessage(
		`{"prompt_tokens":1258,"completion_tokens":121,"total_tokens":1379,"prompt_tokens_details":{"cached_tokens":62}}`))
	if u.CacheReadTokens != 62 || u.InputTokensUncached != 1196 {
		t.Fatalf("现行形态命中 = Read%d/Unc%d，期望 62/1196", u.CacheReadTokens, u.InputTokensUncached)
	}
	// 两形态并存时取其一不累加（旧字段优先为兼容，合计必须仍 62）。
	u2 := Normalize(core.ProviderXAI, core.APIShapeOpenAIChat, json.RawMessage(
		`{"prompt_tokens":1258,"completion_tokens":121,"cached_prompt_text_tokens":62,"prompt_tokens_details":{"cached_tokens":62}}`))
	if u2.CacheReadTokens != 62 {
		t.Fatalf("两形态并存应取其一 ⇒ Read%d，期望 62（⛔ 不累加成 124）", u2.CacheReadTokens)
	}
}
