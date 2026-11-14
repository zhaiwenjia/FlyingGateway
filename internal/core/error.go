// 日期: 2026-11-14 11:33:06.951
// 作者: Zhai Wenjia

package core

// ErrorCategory 是错误归一化分类（统一 SDD 2.3 错误分类表 / 7.1）。
// 4xx 与 5xx 分开归类；context_exceeded / content_policy 独立成类
// （它们有独立的降级链语义，见统一 SDD 7.4）。
type ErrorCategory string

const (
	ErrInvalidRequest  ErrorCategory = "invalid_request"
	ErrContextExceeded ErrorCategory = "context_exceeded"
	ErrContentPolicy   ErrorCategory = "content_policy"
	ErrAuth            ErrorCategory = "auth"
	ErrRateLimit       ErrorCategory = "rate_limit"
	ErrUpstreamError   ErrorCategory = "upstream_error"
	ErrTimeout         ErrorCategory = "timeout"
	ErrBudgetExceeded  ErrorCategory = "budget_exceeded"
	// ErrCancelled：取消。⛔ 取消不触发降级或重试（统一 SDD 7.5）。
	ErrCancelled ErrorCategory = "cancelled"
)

// ErrorDetail 是错误对象（统一 SDD 7.1）：
// 归一化分类对外、原始上游信息对内保留，⛔ 不许只留归一分类。
type ErrorDetail struct {
	// ---- 归一化分类（对外）----
	Category   ErrorCategory
	HTTPStatus int    // 对外状态码
	Code       string // 对外错误码（unsupported_capability / model_not_found /
	//                    context_exceeded / egress_denied / budget_exceeded / …）
	Message string // 对外消息（脱敏后）

	// ---- 原始上游信息（对内保留，⛔ 不丢）----
	Provider          Provider // 上游供应商
	UpstreamStatus    int      // 上游原始 HTTP 状态码
	UpstreamBody      string   // 上游原始响应体（脱敏后）
	UpstreamRequestID string   // 上游请求 ID（若有）
}
