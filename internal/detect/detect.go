// 日期: 2026-11-14 12:02:39.157
// 作者: Zhai Wenjia

// Package detect 做供应商识别：给定端点注册表提供的 (base_url, upstream_model)，
// 判定 Provider + APIShape + 置信度（统一 SDD 3.3 / 4.6）。
//
// 识别顺序 host 优先、model 前缀兜底、OpenAI 兼容兜底；⛔ 不提供用户显式覆盖 provider
// （统一 SDD 4.6.4）。本包是纯函数（统一 SDD 10.1：detect 不依赖 core 以外的任何包）：
// 同输入同输出，⛔ 不掺随机 / 时间 / 遍历顺序（统一 SDD 1.4.3）。
package detect

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// shapeRule 是 path → shape 后缀规则（统一 SDD 3.3 step 4）。
// 最具体的形态排前面；命中即停（确定性）。
var shapeRules = []struct {
	suffix string
	shape  core.APIShape
}{
	{":streamGenerateContent", core.APIShapeGeminiNative},
	{":generateContent", core.APIShapeGeminiNative},
	{"/chat/completions", core.APIShapeOpenAIChat},
	{"/responses", core.APIShapeOpenAIResponses},
	{"/messages", core.APIShapeAnthropicMessages},
}

// shapeFromPath 按 path 后缀判定 shape（统一 SDD 3.3 step 4）。
func shapeFromPath(path string) (core.APIShape, bool) {
	for _, rule := range shapeRules {
		if strings.HasSuffix(path, rule.suffix) {
			return rule.shape, true
		}
	}
	return core.APIShapeUnknown, false
}

// defaultShape 是 path 无信号时按 host 规则表（统一 SDD 3.3 Shape 列）取的默认形状：
// 只有 Anthropic（AnthropicMessages）与 Gemini（原生 generateContent）的默认形状唯一，
// 其余各家同 host 多形状（Chat / Responses / 原生）⇒ 无 path 信号时按 OpenAI 兼容处理。
func defaultShape(provider core.Provider) core.APIShape {
	switch provider {
	case core.ProviderAnthropic:
		return core.APIShapeAnthropicMessages
	case core.ProviderGemini:
		return core.APIShapeGeminiNative
	default:
		return core.APIShapeOpenAIChat
	}
}

// Detect 识别端点的供应商与 API 形状（统一 SDD 3.3）。
//
// 输入 (baseURL, upstreamModel) 由端点注册表提供（⛔ 调用方请求不携带 base_url /
// 不指定上游模型，统一 SDD 3.3）；返回的 core.Resolution 中
// CredentialRef / SupportsPreviousResponseID 由 adapter 声明（统一 SDD 3.5 / 9.1），
// 识别层不填、保持零值。
//
// 识别顺序（统一 SDD 3.3 / 4.6.3 / 4.6.4）：
//  1. host 规则表命中 ⇒ provider 确定，confidence=high；
//  2. host 未命中、model 前缀命中 ⇒ provider 确定，confidence=medium；
//  3. 都未命中 ⇒ provider=Unknown、shape=OpenAIChat、Fallback=true、confidence=fallback。
//
// shape 判定：host / 前缀命中时按 path 后缀规则（统一 SDD 3.3 step 4，同 host 多形状
// 由路径决定形状、host 只定供应商，统一 SDD 4.6.3）；path 无信号时按 defaultShape。
// 走 OpenAI 兼容兜底时形状就是 OpenAIChat（统一 SDD 4.6.3「host 不可识别 ⇒ 走 OpenAI
// 兼容兜底」，兜底连形状一起按 OpenAI 兼容处理）。
//
// Qwen 地域绑定（统一 SDD 3.3 实现约束 / 11.1）：host 是 *.maas.aliyuncs.com 但地域段
// 非 cn-beijing ⇒ 显式报错，⛔ 不静默换端点。
func Detect(baseURL, upstreamModel string) (core.Resolution, error) {
	res := core.Resolution{BaseURL: baseURL, Model: upstreamModel}

	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return res, fmt.Errorf("detect: base URL %q 无法解析出 host（须形如 scheme://host[/path]）", baseURL)
	}
	host := strings.ToLower(u.Host)
	hostname := strings.ToLower(u.Hostname())
	path := strings.TrimRight(u.Path, "/")

	// Qwen 地域绑定校验先行：命中 maas 后缀就要求地域段可验证且为 cn-beijing。
	if region, ok := qwenRegion(hostname); ok && region != qwenCNBeijingRegion {
		return res, &RegionBindingError{Host: host, Region: region}
	}

	// 1. host 优先（confidence=high）：base URL 优先级高于 model name。
	if provider, ok := matchHost(host, hostname); ok {
		res.Provider = provider
		res.Confidence = core.ConfidenceHigh
		if shape, ok := shapeFromPath(path); ok {
			res.Shape = shape
		} else {
			res.Shape = defaultShape(provider)
		}
		return res, nil
	}

	// 2. model 前缀兜底（confidence=medium）。
	if provider, ok := matchPrefix(upstreamModel); ok {
		res.Provider = provider
		res.Confidence = core.ConfidenceMedium
		if shape, ok := shapeFromPath(path); ok {
			res.Shape = shape
		} else {
			res.Shape = core.APIShapeOpenAIChat
		}
		return res, nil
	}

	// 3. 双兜底：OpenAI 兼容兜底，标低置信度（统一 SDD 4.6.3 / 4.6.4）。
	res.Provider = core.ProviderUnknown
	res.Shape = core.APIShapeOpenAIChat
	res.Confidence = core.ConfidenceFallback
	res.Fallback = true
	return res, nil
}
