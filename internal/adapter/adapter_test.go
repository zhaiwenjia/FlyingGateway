// 日期: 2026-11-14 14:58:31.370
// 作者: Zhai Wenjia

package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件覆盖 For / ForShape 分派（统一 SDD 3.6 / 任务书 ⑤）与全包共享的测试小工具。
// 断言口径逐字对齐 SDD；每条标注「错误实现会红」的对照点。

// ---- 共享测试小工具 ----

// loadFixture 读取 testdata/ 下的 fixture（来源标注见 testdata/README.md）。
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读 fixture %s 失败: %v", name, err)
	}
	return b
}

// bodyMap 把出站请求体反序列化成 map（顶层键存在性断言用）。
func bodyMap(t *testing.T, out *Outbound) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out.Body, &m); err != nil {
		t.Fatalf("请求体反序列化失败: %v（body=%s）", err, out.Body)
	}
	return m
}

// mustString 取嵌套 map 的字符串字段（keys 逐层下钻）。
func mustString(t *testing.T, m map[string]any, keys ...string) string {
	t.Helper()
	v := mustAt(t, m, keys...)
	s, ok := v.(string)
	if !ok {
		t.Fatalf("字段 %v 不是字符串: %T", keys, v)
	}
	return s
}

// mustNumber 取嵌套 map 的数值字段（JSON 数值统一 float64）。
func mustNumber(t *testing.T, m map[string]any, keys ...string) float64 {
	t.Helper()
	v := mustAt(t, m, keys...)
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("字段 %v 不是数值: %T", keys, v)
	}
	return n
}

// mustAt 沿 keys 下钻取值。
func mustAt(t *testing.T, m map[string]any, keys ...string) any {
	t.Helper()
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("字段路径 %v 在 %q 处不是对象: %T", keys, k, cur)
		}
		cur, ok = mm[k]
		if !ok {
			t.Fatalf("字段路径 %v 缺少 %q", keys, k)
		}
	}
	return cur
}

// mustSlice 取嵌套 map 的数组字段。
func mustSlice(t *testing.T, m map[string]any, keys ...string) []any {
	t.Helper()
	v := mustAt(t, m, keys...)
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("字段 %v 不是数组: %T", keys, v)
	}
	return s
}

// assertNoKey 断言顶层键不存在（「⛔ 不发某字段」类断言用）。
// 错误实现会红：字段一旦被发出 ⇒ 本断言立即红（恒假样本已核，见交付报告变异自检）。
func assertNoKey(t *testing.T, m map[string]any, key string) {
	t.Helper()
	if _, ok := m[key]; ok {
		t.Fatalf("请求体不应含字段 %q（⛔ 不发），body=%v", key, m)
	}
}

// requireDetailErr 断言 err 是 DetailError 且明细码匹配（显式报错类断言用）。
func requireDetailErr(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望显式错误（code=%s），实际 nil（⛔ 不静默丢弃）", code)
	}
	var de *DetailError
	if !errors.As(err, &de) {
		t.Fatalf("期望 *DetailError，实际 %T: %v", err, err)
	}
	if de.Detail == nil || de.Detail.Code != code {
		t.Fatalf("错误码 = %+v，期望 code=%s", de.Detail, code)
	}
}

// collectFrames 排空帧迭代器，返回帧序列与终态错误。
func collectFrames(t *testing.T, it stream.FrameIterator) ([]stream.Frame, error) {
	t.Helper()
	var frames []stream.Frame
	for it.Next() {
		frames = append(frames, it.Frame())
	}
	return frames, it.Err()
}

// ---- ⑤ For / ForShape 分派 ----

// TestForShapeDispatch 钉住 11 家 × 形状组合命中正确适配器（任务书 ⑤ / SDD 3.6 适配器清单）。
// 错误实现会红：分派错家 ⇒ 类型名断言不匹配即红；未登记组合必须显式报错（⛔ 不静默回落）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestForShapeDispatch -v
func TestForShapeDispatch(t *testing.T) {
	cases := []struct {
		p    core.Provider
		s    core.APIShape
		want string // 具体适配器类型（同包内可见）
	}{
		{core.ProviderOpenAI, core.APIShapeOpenAIResponses, "*adapter.openaiAdapter"},
		{core.ProviderAnthropic, core.APIShapeAnthropicMessages, "*adapter.anthropicAdapter"},
		{core.ProviderGemini, core.APIShapeGeminiNative, "*adapter.geminiAdapter"},
		{core.ProviderXAI, core.APIShapeOpenAIResponses, "*adapter.xaiAdapter"},
		{core.ProviderDeepSeek, core.APIShapeOpenAIResponses, "*adapter.deepseekAdapter"},
		{core.ProviderMoonshot, core.APIShapeOpenAIResponses, "*adapter.moonshotAdapter"},
		{core.ProviderZhipu, core.APIShapeOpenAIResponses, "*adapter.zhipuAdapter"},
		{core.ProviderQwen, core.APIShapeOpenAIChat, "*adapter.qwenAdapter"},
		{core.ProviderQwen, core.APIShapeOpenAIResponses, "*adapter.qwenAdapter"},
		{core.ProviderQwen, core.APIShapeQwenNative, "*adapter.qwenNativeAdapter"},
		{core.ProviderMiniMax, core.APIShapeOpenAIChat, "*adapter.minimaxAdapter"},
		{core.ProviderMiniMax, core.APIShapeOpenAIResponses, "*adapter.minimaxAdapter"},
		{core.ProviderMiniMax, core.APIShapeAnthropicMessages, "*adapter.minimaxAnthropicAdapter"},
		{core.ProviderMiniMax, core.APIShapeMiniMaxNativeV2, "*adapter.minimaxV2Adapter"},
		{core.ProviderXiaomi, core.APIShapeOpenAIResponses, "*adapter.xiaomiAdapter"},
		{core.ProviderOllama, core.APIShapeOllamaNative, "*adapter.ollamaAdapter"},
		{core.ProviderOllama, core.APIShapeOpenAIChat, "*adapter.ollamaCompatAdapter"},
		{core.ProviderOllama, core.APIShapeOpenAIResponses, "*adapter.ollamaCompatAdapter"},
	}
	for _, c := range cases {
		a, err := ForShape(c.p, c.s)
		if err != nil {
			t.Fatalf("ForShape(%s, %s) 报错: %v", c.p, c.s, err)
		}
		if got := typeOf(a); got != c.want {
			t.Fatalf("ForShape(%s, %s) = %s，期望 %s", c.p, c.s, got, c.want)
		}
		if a.Provider() != c.p {
			t.Fatalf("ForShape(%s, %s).Provider() = %s", c.p, c.s, a.Provider())
		}
	}
}

// TestForShapeUnsupported 钉住未登记组合显式报错（SDD 3.6：⛔ 不静默回落）。
// 错误实现会红：静默回落任一兜底适配器 ⇒ err==nil ⇒ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestForShapeUnsupported -v
func TestForShapeUnsupported(t *testing.T) {
	// OpenAI × Chat（OpenAI 只登记 Responses 形）。
	_, err := ForShape(core.ProviderOpenAI, core.APIShapeOpenAIChat)
	requireDetailErr(t, err, "unsupported_shape")
	// DeepSeek × Chat（出站方法口径：DeepSeek 支持 /responses ⇒ 只登记 Responses 形）。
	_, err = ForShape(core.ProviderDeepSeek, core.APIShapeOpenAIChat)
	requireDetailErr(t, err, "unsupported_shape")
	// 未登记供应商。
	_, err = ForShape(core.ProviderUnknown, core.APIShapeOpenAIChat)
	requireDetailErr(t, err, "unsupported_provider")
}

// TestForDefaultShape 钉住 For() 的默认出站形状（SDD 3.6 / defaultShapeFor 注释口径）：
// 出站方法口径——除 Claude 外优先 OpenAI SDK Response API，不支持才回退：
// OpenAI / Grok / DeepSeek / Kimi / 智谱 / 小米 / Qwen 兼容 / MiniMax 兼容 / Ollama 兼容 ⇒ Responses；
// Claude ⇒ Messages；Gemini ⇒ 原生（其 OpenAI 兼容层无 /responses）。
// 错误实现会红：默认形状错配 ⇒ OutboundShape 断言不匹配即红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/adapter/ -run TestForDefaultShape -v
func TestForDefaultShape(t *testing.T) {
	cases := []struct {
		p    core.Provider
		want OutboundShape
	}{
		{core.ProviderOpenAI, ShapeResponses},
		{core.ProviderAnthropic, ShapeAnthropicMessages},
		{core.ProviderGemini, ShapeGeminiNative},
		{core.ProviderXAI, ShapeResponses},
		{core.ProviderDeepSeek, ShapeResponses},
		{core.ProviderMoonshot, ShapeResponses},
		{core.ProviderZhipu, ShapeResponses},
		{core.ProviderQwen, ShapeResponses},
		{core.ProviderMiniMax, ShapeResponses},
		{core.ProviderXiaomi, ShapeResponses},
		{core.ProviderOllama, ShapeResponses},
	}
	for _, c := range cases {
		a, err := For(c.p)
		if err != nil {
			t.Fatalf("For(%s) 报错: %v", c.p, err)
		}
		if a.OutboundShape() != c.want {
			t.Fatalf("For(%s).OutboundShape() = %s，期望 %s", c.p, a.OutboundShape(), c.want)
		}
	}
}

// typeOf 返回适配器具体类型名（分派断言用）。
func typeOf(a Adapter) string {
	return fmt.Sprintf("%T", a)
}
