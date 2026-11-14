// 日期: 2026-11-14 11:50:04.176
// 作者: Zhai Wenjia

package errors

import (
	"strings"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// 本文件承载错误归一化分类（统一 SDD 2.3 错误分类表 / 7.1）。
//
// 分类枚举九值以 core.ErrorCategory 为准（core/error.go）。
// ⚠️ 统一 SDD 2.3 表头写「七类」但表体列了九行（含 context_exceeded /
// content_policy / budget_exceeded / cancelled）⇒ 按九值实现：
// context_exceeded 与 content_policy 独立成类，7.4「按失败类型分链」需要
// 用它们选专用降级链，不能折进 invalid_request。
//
// 各类的重试 / 降级语义（统一 SDD 2.3 表，逐字口径）：
//
//	invalid_request   可重试 ❌  可降级 ❌
//	context_exceeded  可重试 ❌  可降级 ✅（context_window 专用链）
//	content_policy    可重试 ❌  可降级 ✅（content_policy 专用链）
//	auth              可重试 ❌  可降级 ❌（换端点也一样）
//	rate_limit        可重试 ✅（退避）  可降级 ✅（换端点）
//	upstream_error    可重试 ✅（退避）  可降级 ✅
//	timeout           可重试 ✅  可降级 ✅
//	budget_exceeded   可重试 ❌  可降级 ❌（触顶即拒）
//	cancelled         可重试 ❌  可降级 ❌（⛔ 取消不触发降级或重试，统一 SDD 7.5）
//
// ⛔ 4xx 与 5xx 必须分开归类（7.1）：4xx 归 invalid_request 系（含其特化类），
// 5xx / 连接失败归 upstream_error，不许互相吞并。

// classifyHTTPStatus 按上游 HTTP 状态码 + 错误体语义归类（统一 SDD 7.1）。
// body 传 nil 表示无错误体（如连接失败后拿不到响应）。
func classifyHTTPStatus(upstreamStatus int, body []byte) core.ErrorCategory {
	text := strings.ToLower(string(body))
	switch {
	case upstreamStatus == 400:
		// 400 基线是请求不合法；但错误体带 context/window 语义 ⇒ 上下文超限，
		// 带内容安全语义 ⇒ content_policy（统一 SDD 2.3 错误分类表）。
		if hasContextSemantics(text) {
			return core.ErrContextExceeded
		}
		if hasContentPolicySemantics(text) {
			return core.ErrContentPolicy
		}
		return core.ErrInvalidRequest
	case upstreamStatus == 401:
		return core.ErrAuth
	case upstreamStatus == 403:
		// 403 二义：内容安全拦截 ⇒ content_policy；权限不足 ⇒ auth。
		// 错误体语义判定，歧义时按权限不足归 auth（403 的经典语义是禁止访问）。
		if hasContentPolicySemantics(text) {
			return core.ErrContentPolicy
		}
		return core.ErrAuth
	case upstreamStatus == 404:
		// 404（端点 / 模型不存在）属请求侧问题 ⇒ invalid_request
		//（模型不存在的对外错误码 model_not_found 由上层在 Code 里标注）。
		return core.ErrInvalidRequest
	case upstreamStatus == 429:
		// 429 基线是限流；错误体含 budget 语义 ⇒ 预算触顶（budget_exceeded）。
		if strings.Contains(text, "budget") {
			return core.ErrBudgetExceeded
		}
		return core.ErrRateLimit
	case upstreamStatus >= 500:
		return core.ErrUpstreamError
	case upstreamStatus >= 400:
		// 其余 4xx（405 / 409 / 422 / …）归请求侧。
		return core.ErrInvalidRequest
	default:
		// 非错误状态走到错误路径属上游异常（如 HTTP 200 但响应不可解析）。
		return core.ErrUpstreamError
	}
}

// contextSemantics 是错误体里的「上下文 / 窗口超限」信号（小写匹配）。
// 取值来自各家真实错误串：OpenAI "This model's maximum context length is …"、
// Anthropic "input length and max_tokens exceed context limit"、
// MiniMax "1039 Token 超限" 等。
var contextSemantics = []string{
	"context_length",
	"context length",
	"context_window",
	"context window",
	"maximum context",
	"max context",
	"too many tokens",
	"prompt is too long",
	"input is too long",
	"reduce the length",
	"token limit",
	"token 超限",
	"exceed context",
	"context limit",
}

// contentPolicySemantics 是错误体里的「内容安全拦截」信号（小写匹配）。
var contentPolicySemantics = []string{
	"content_policy",
	"content policy",
	"content_filter",
	"content filter",
	"moderation",
	"prohibited",
	"inappropriate",
	"violates",
	"safety system",
	"sensitive content",
	"敏感",
	"违规",
}

func hasContextSemantics(textLower string) bool {
	for _, kw := range contextSemantics {
		if strings.Contains(textLower, kw) {
			return true
		}
	}
	return false
}

func hasContentPolicySemantics(textLower string) bool {
	for _, kw := range contentPolicySemantics {
		if strings.Contains(textLower, kw) {
			return true
		}
	}
	return false
}

// statusForCategory 给非 HTTP 通道（MiniMax 业务码 / 传输层错误）取对外状态码。
func statusForCategory(cat core.ErrorCategory) int {
	switch cat {
	case core.ErrInvalidRequest, core.ErrContextExceeded:
		return 400
	case core.ErrContentPolicy:
		return 403
	case core.ErrAuth:
		return 401
	case core.ErrRateLimit, core.ErrBudgetExceeded:
		return 429
	case core.ErrTimeout:
		return 504
	case core.ErrCancelled:
		// 499（nginx 惯例）：客户端已断开，响应未必送达。
		return 499
	default:
		// upstream_error 等
		return 502
	}
}
