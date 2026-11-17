// 日期: 2026-11-14 12:02:39.194
// 作者: Zhai Wenjia

package detect

import (
	"errors"
	"reflect"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// TestDetectHostRules 表逐行断言 11 家 host 各命中正确 Provider 与 confidence=high
// （统一 SDD 11.1「host 识别」行）。
// 对照点：host 表错行（如把 api.x.ai 归 Gemini）或匹配串写错 ⇒ 对应子例立即红。
func TestDetectHostRules(t *testing.T) {
	cases := []struct {
		name     string
		baseURL  string
		provider core.Provider
	}{
		// 4.6.1 第 1 行 Qwen：两种形态。
		{"qwen-maas", "https://ws-abc.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", core.ProviderQwen},
		{"qwen-dashscope", "https://dashscope.aliyuncs.com/compatible-mode/v1", core.ProviderQwen},
		// 第 2 行 Kimi。
		{"moonshot", "https://api.moonshot.cn/v1", core.ProviderMoonshot},
		// 第 3 行 智谱。
		{"zhipu", "https://open.bigmodel.cn/api/paas/v4", core.ProviderZhipu},
		// 第 4 行 DeepSeek。
		{"deepseek", "https://api.deepseek.com/v1", core.ProviderDeepSeek},
		// 第 5 行 小米 MiMo：三种形态。
		{"xiaomi-api", "https://api.xiaomimimo.com/v1", core.ProviderXiaomi},
		{"xiaomi-batch", "https://batch-api-cn.xiaomimimo.com/v1", core.ProviderXiaomi},
		{"xiaomi-tokenplan", "https://token-plan-cn.xiaomimimo.com/v1", core.ProviderXiaomi},
		// 第 6 行 MiniMax。
		{"minimax", "https://api.minimax.cn/v1", core.ProviderMiniMax},
		// 第 7 行 OpenAI。
		{"openai", "https://api.openai.com/v1", core.ProviderOpenAI},
		// 第 8 行 Claude。
		{"anthropic", "https://api.anthropic.com", core.ProviderAnthropic},
		// 第 9 行 Grok。
		{"xai", "https://api.x.ai/v1", core.ProviderXAI},
		// 第 10 行 Gemini：两种形态。
		{"gemini-gen", "https://generativelanguage.googleapis.com/v1beta", core.ProviderGemini},
		{"gemini-aiplatform", "https://aiplatform.googleapis.com/v1", core.ProviderGemini},
		// 第 11 行 Ollama：两种形态（端口是规则的一部分）。
		{"ollama-localhost", "http://localhost:11434", core.ProviderOllama},
		{"ollama-loopback", "http://127.0.0.1:11434", core.ProviderOllama},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Detect(tc.baseURL, "some-model")
			if err != nil {
				t.Fatalf("Detect(%q) 报错: %v", tc.baseURL, err)
			}
			if res.Provider != tc.provider {
				t.Errorf("provider = %v, want %v", res.Provider, tc.provider)
			}
			if res.Confidence != core.ConfidenceHigh {
				t.Errorf("confidence = %v, want high（host 命中）", res.Confidence)
			}
			if res.Fallback {
				t.Errorf("Fallback = true, want false（host 已命中，不走兜底）")
			}
			if res.BaseURL != tc.baseURL || res.Model != "some-model" {
				t.Errorf("Resolution 回填输入不符: BaseURL=%q Model=%q", res.BaseURL, res.Model)
			}
		})
	}
}

// TestDetectModelPrefixFallback 断言 host 不可识别但模型名可识别 ⇒ confidence=medium
// （统一 SDD 11.1「前缀兜底」行，4.6.2 前缀表逐行取样）。
// 对照点：若实现只认 host（丢掉前缀兜底）或把置信度标成 high/fallback ⇒ 红。
func TestDetectModelPrefixFallback(t *testing.T) {
	const unknownHost = "https://myproxy.internal.example.com/v1"
	cases := []struct {
		model    string
		provider core.Provider
	}{
		{"qwen-plus", core.ProviderQwen},
		{"qwq-32b", core.ProviderQwen},
		{"kimi-k2", core.ProviderMoonshot},
		{"moonshot-v1-128k", core.ProviderMoonshot},
		{"glm-4-plus", core.ProviderZhipu},
		{"deepseek-chat", core.ProviderDeepSeek},
		{"deepseek-reasoner", core.ProviderDeepSeek},
		{"mimo-7b-rl", core.ProviderXiaomi},
		{"abab6.5-chat", core.ProviderMiniMax},
		{"gpt-4o-mini", core.ProviderOpenAI},
		{"o3-mini", core.ProviderOpenAI},
		{"chatgpt-4o-latest", core.ProviderOpenAI},
		{"claude-3-5-sonnet", core.ProviderAnthropic},
		{"grok-3", core.ProviderXAI},
		{"gemini-2.5-pro", core.ProviderGemini},
		{"llama3.3:70b", core.ProviderOllama},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			res, err := Detect(unknownHost, tc.model)
			if err != nil {
				t.Fatalf("Detect 报错: %v", err)
			}
			if res.Provider != tc.provider {
				t.Errorf("provider = %v, want %v", res.Provider, tc.provider)
			}
			if res.Confidence != core.ConfidenceMedium {
				t.Errorf("confidence = %v, want medium（前缀兜底）", res.Confidence)
			}
			if res.Fallback {
				t.Errorf("Fallback = true, want false（前缀已命中）")
			}
		})
	}
}

// TestDetectDoubleFallback 断言 host 与 model 都不可识别 ⇒
// Fallback=true、shape=OpenAIChat、confidence=fallback（统一 SDD 11.1「双重兜底」行 / 3.3 step 3b）。
// 对照点：兜底不置 Fallback=true 或形状取别的值 ⇒ 红。
func TestDetectDoubleFallback(t *testing.T) {
	res, err := Detect("https://unknown-gateway.example.com/v1", "totally-custom-model")
	if err != nil {
		t.Fatalf("Detect 报错: %v", err)
	}
	if !res.Fallback {
		t.Errorf("Fallback = false, want true（双兜底）")
	}
	if res.Shape != core.APIShapeOpenAIChat {
		t.Errorf("shape = %v, want openai_chat（兜底形状）", res.Shape)
	}
	if res.Confidence != core.ConfidenceFallback {
		t.Errorf("confidence = %v, want fallback", res.Confidence)
	}
	if res.Provider != core.ProviderUnknown {
		t.Errorf("provider = %v, want unknown", res.Provider)
	}
}

// TestDetectAmbiguityHostPriority 断言识别歧义由 host 优先消解（统一 SDD 11.1「歧义消解」行 / 4.6.3）：
// `deepseek-r1` + Ollama host ⇒ Ollama；`deepseek-r1` + DeepSeek host ⇒ DeepSeek。
// 对照点：把优先级改成 model 前缀优先 ⇒ 两个子例都红（前缀表会把 deepseek-r1 判给别家）。
func TestDetectAmbiguityHostPriority(t *testing.T) {
	cases := []struct {
		name     string
		baseURL  string
		provider core.Provider
	}{
		{"deepseek-r1-on-ollama", "http://localhost:11434", core.ProviderOllama},
		{"deepseek-r1-on-deepseek", "https://api.deepseek.com/v1", core.ProviderDeepSeek},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Detect(tc.baseURL, "deepseek-r1")
			if err != nil {
				t.Fatalf("Detect 报错: %v", err)
			}
			if res.Provider != tc.provider {
				t.Errorf("provider = %v, want %v（host 优先于 model 前缀）", res.Provider, tc.provider)
			}
			if res.Confidence != core.ConfidenceHigh {
				t.Errorf("confidence = %v, want high（host 命中）", res.Confidence)
			}
		})
	}
}

// TestDetectPrefixTableOrder 钉住前缀表内部的确定性消歧（统一 SDD 4.6.2 表序 + 1.4.3）：
// `deepseek-r1` 裸名在 4.6.2 表里 Qwen 行先于 DeepSeek / Ollama 行出现 ⇒ 表序首中判 Qwen；
// `qwen2.5` 命中 Qwen 行 `qwen` 前缀；`deepseek-chat` 命中 DeepSeek 行。
// 对照点：实现改为「最长前缀」或乱序遍历 ⇒ deepseek-r1 / qwen2.5 的归属漂移，子例红。
func TestDetectPrefixTableOrder(t *testing.T) {
	cases := []struct {
		model    string
		provider core.Provider
	}{
		{"deepseek-r1", core.ProviderQwen},
		{"qwen2.5-7b-instruct", core.ProviderQwen},
		{"deepseek-chat", core.ProviderDeepSeek},
		{"qwen2", core.ProviderQwen},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			res, err := Detect("https://unrecognized.example.org", tc.model)
			if err != nil {
				t.Fatalf("Detect 报错: %v", err)
			}
			if res.Provider != tc.provider {
				t.Errorf("provider = %v, want %v（4.6.2 表序首中）", res.Provider, tc.provider)
			}
			if res.Confidence != core.ConfidenceMedium {
				t.Errorf("confidence = %v, want medium", res.Confidence)
			}
		})
	}
}

// TestDetectQwenRegionBinding 断言 Qwen 地域绑定：host 是 *.maas.aliyuncs.com 但地域段
// 非 cn-beijing（如 us-east-1）⇒ 显式报错（统一 SDD 11.1「Qwen 地域」行 / 3.3 实现约束）。
// 对照点：不做地域校验的实现返回 nil 错误 ⇒ 报错子例红。
func TestDetectQwenRegionBinding(t *testing.T) {
	t.Run("北京 WorkspaceId + us-east-1 host ⇒ 报错", func(t *testing.T) {
		_, err := Detect("https://ws-bj001.us-east-1.maas.aliyuncs.com/compatible-mode/v1", "qwen-plus")
		if err == nil {
			t.Fatalf("期望报错，实际 nil（⛔ 不静默换端点）")
		}
		if !errors.Is(err, ErrRegionMismatch) {
			t.Errorf("err = %v, want errors.Is ErrRegionMismatch", err)
		}
		var re *RegionBindingError
		if !errors.As(err, &re) {
			t.Errorf("err = %v, want *RegionBindingError", err)
		} else if re.Region != "us-east-1" {
			t.Errorf("Region = %q, want us-east-1", re.Region)
		}
	})
	t.Run("cn-beijing host ⇒ 正常识别", func(t *testing.T) {
		res, err := Detect("https://ws-bj001.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", "qwen-plus")
		if err != nil {
			t.Fatalf("Detect 报错: %v", err)
		}
		if res.Provider != core.ProviderQwen || res.Confidence != core.ConfidenceHigh {
			t.Errorf("provider/confidence = %v/%v, want qwen/high", res.Provider, res.Confidence)
		}
	})
	t.Run("dashscope host 不带地域段 ⇒ 不触发地域校验", func(t *testing.T) {
		if _, err := Detect("https://dashscope.aliyuncs.com/compatible-mode/v1", "qwen-plus"); err != nil {
			t.Fatalf("Detect 报错: %v", err)
		}
	})
}

// TestDetectShapeFromPath 断言 path 后缀决定形状（统一 SDD 3.3 step 4 / 4.6.3「由路径决定形状」）。
// 对照点：形状改成由 host 决定或后缀串写错 ⇒ 对应子例红。
func TestDetectShapeFromPath(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		shape   core.APIShape
	}{
		{"chat-completions", "https://api.deepseek.com/v1/chat/completions", core.APIShapeOpenAIChat},
		{"responses", "https://api.openai.com/v1/responses", core.APIShapeOpenAIResponses},
		{"messages", "https://api.anthropic.com/v1/messages", core.APIShapeAnthropicMessages},
		{"gemini-generate", "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:generateContent", core.APIShapeGeminiNative},
		{"gemini-stream", "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:streamGenerateContent", core.APIShapeGeminiNative},
		// path 无信号 ⇒ 按 host 默认形状（3.3 Shape 列：Anthropic=Messages、Gemini=原生、其余=OpenAIChat）。
		{"anthropic-default", "https://api.anthropic.com/v1", core.APIShapeAnthropicMessages},
		{"gemini-default", "https://generativelanguage.googleapis.com/v1beta", core.APIShapeGeminiNative},
		{"openai-default", "https://api.openai.com/v1", core.APIShapeOpenAIChat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Detect(tc.baseURL, "some-model")
			if err != nil {
				t.Fatalf("Detect 报错: %v", err)
			}
			if res.Shape != tc.shape {
				t.Errorf("shape = %v, want %v", res.Shape, tc.shape)
			}
		})
	}
}

// TestDetectReproducible 断言识别可复现：同一输入识别 100 次结果完全一致
// （统一 SDD 11.3「路由-可复现性」行 / 1.4.3）。对照点：实现里掺 map 遍历 / 随机 /
// 时间 ⇒ 只要顺序敏感就会出现不一致，DeepEqual 断言红。
func TestDetectReproducible(t *testing.T) {
	type input struct{ baseURL, model string }
	inputs := []input{
		{"http://localhost:11434", "deepseek-r1"},
		{"https://api.deepseek.com/v1", "deepseek-r1"},
		{"https://myproxy.example.com/v1", "totally-custom-model"},
		{"https://ws-a.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", "qwen-plus"},
	}
	for _, in := range inputs {
		first, err := Detect(in.baseURL, in.model)
		if err != nil {
			t.Fatalf("Detect(%q, %q) 报错: %v", in.baseURL, in.model, err)
		}
		for i := 0; i < 100; i++ {
			got, err := Detect(in.baseURL, in.model)
			if err != nil {
				t.Fatalf("第 %d 次 Detect 报错: %v", i, err)
			}
			if !reflect.DeepEqual(got, first) {
				t.Fatalf("第 %d 次结果与首次不一致: got %+v, want %+v", i, got, first)
			}
		}
	}
}

// TestDetectInvalidBaseURL 断言无法解析出 host 的输入显式报错（⛔ 不猜端点）。
// 对照点：宽松实现把垃圾串当 path 静默兜底 ⇒ 红。
func TestDetectInvalidBaseURL(t *testing.T) {
	for _, bad := range []string{"not-a-url", "", "://no-scheme"} {
		if _, err := Detect(bad, "gpt-4o-mini"); err == nil {
			t.Errorf("Detect(%q) 期望报错，实际 nil", bad)
		}
	}
}
