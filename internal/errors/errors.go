// 日期: 2026-11-14 19:39:55.243
// 作者: Zhai Wenjia

// Package errors 承载错误归一化（统一 SDD 7.1）：对外给归一化分类，
// 对内保留原始上游信息（状态码 / 请求 ID / 响应体，脱敏后）。
//
// ⚠️ 本包与标准库 errors 同名，引用标准库时统一别名 stderrors。
// ⚠️ 遮蔽实现只在 internal/security 一处（统一 SDD 8.1 出口层一次遮蔽），
// 本包只调用 security.Redact，⛔ 不重复实现遮蔽规则。
package errors

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"net"
	"strconv"
	"strings"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	"github.com/zhaiwenjia/FlyingGateway/internal/security"
)

// New 构造一个归一化错误对象。msg 与 code 均按出口层口径过遮蔽。
func New(cat core.ErrorCategory, status int, code, msg string) *core.ErrorDetail {
	return &core.ErrorDetail{
		Category:   cat,
		HTTPStatus: status,
		Code:       security.Redact(code),
		Message:    security.Redact(msg),
	}
}

// FromUpstream 把上游错误响应归一化为 ErrorDetail（统一 SDD 7.1）：
// 归一分类对外，⛔ 原始上游信息（状态码 / 请求 ID / 响应体）对内保留不丢，
// 响应体按出口层口径遮敏后存入 UpstreamBody。
func FromUpstream(provider core.Provider, upstreamStatus int, body []byte, requestID string) *core.ErrorDetail {
	cat := classifyHTTPStatus(upstreamStatus, body)
	code, msg := extractUpstreamError(body)
	if code == "" {
		code = string(cat)
	}
	if msg == "" {
		msg = "upstream returned HTTP " + strconv.Itoa(upstreamStatus)
	}
	return &core.ErrorDetail{
		Category:          cat,
		HTTPStatus:        upstreamStatus,
		Code:              security.Redact(code),
		Message:           security.Redact(msg),
		Provider:          provider,
		UpstreamStatus:    upstreamStatus,
		UpstreamBody:      security.Redact(string(body)),
		UpstreamRequestID: requestID,
	}
}

// minimaxCategory 映射 MiniMax 业务错误码（统一 SDD 4.3.11）：
// 1000 未知 / 1013 内部错误 ⇒ upstream_error；1001 超时 ⇒ timeout；
// 1002 限流 ⇒ rate_limit；1004 鉴权失败 ⇒ auth；1008 余额不足 ⇒ budget_exceeded；
// 1027 输出内容错误 ⇒ content_policy；1039 Token 超限 ⇒ context_exceeded；
// 2013 参数错误 ⇒ invalid_request；未知码 ⇒ upstream_error。
func minimaxCategory(statusCode int) core.ErrorCategory {
	switch statusCode {
	case 1001:
		return core.ErrTimeout
	case 1002:
		return core.ErrRateLimit
	case 1004:
		return core.ErrAuth
	case 1008:
		return core.ErrBudgetExceeded
	case 1027:
		return core.ErrContentPolicy
	case 1039:
		return core.ErrContextExceeded
	case 2013:
		return core.ErrInvalidRequest
	default:
		// 1000 未知 / 1013 内部错误 / 其余未知码
		return core.ErrUpstreamError
	}
}

// FromMiniMaxBaseResp 处理 MiniMax 特例（统一 SDD 4.3.11）：
// HTTP 200 但 base_resp.status_code != 0 ⇒ 业务错误，按 status_code 映射分类。
// ⚠️ MiniMax 的业务错误藏在 HTTP 200 里，⛔ 不检查 base_resp 就会把错误当成功。
func FromMiniMaxBaseResp(statusCode int, statusMsg string) *core.ErrorDetail {
	cat := minimaxCategory(statusCode)
	return &core.ErrorDetail{
		Category:   cat,
		HTTPStatus: statusForCategory(cat),
		Code:       "minimax_" + strconv.Itoa(statusCode),
		Message:    security.Redact(statusMsg),
		Provider:   core.ProviderMiniMax,
		// 业务错误承载于 HTTP 200 响应内（4.3.11），故 UpstreamStatus 记 200。
		UpstreamStatus: 200,
		// base_resp 仅有 status_code / status_msg 两个字段，status_msg 已入 Message，
		// 这里不伪造完整响应体，UpstreamBody 留空由上层按需补真实体。
		UpstreamBody: "",
	}
}

// FromTransportError 归一化传输层失败（统一 SDD 7.1）：
// 超时 ⇒ timeout；取消 ⇒ cancelled；连接失败等其余传输错误 ⇒ upstream_error。
//
// ⛔ cancelled 分类语义：取消不触发降级、不触发重试（统一 SDD 7.5），
// 断连后不得发起新的上游请求；上层拿到 ErrCancelled 必须直接收口。
func FromTransportError(provider core.Provider, err error, requestID string) *core.ErrorDetail {
	cat := core.ErrUpstreamError
	switch {
	case stderrors.Is(err, context.Canceled):
		cat = core.ErrCancelled
	default:
		var netErr net.Error
		if stderrors.As(err, &netErr) && netErr.Timeout() {
			cat = core.ErrTimeout
		} else if strings.Contains(strings.ToLower(err.Error()), "timeout") {
			// 裸字符串兜底（如 url.Error 包装链里 net.Error 不可见时）。
			cat = core.ErrTimeout
		}
	}
	return &core.ErrorDetail{
		Category:          cat,
		HTTPStatus:        statusForCategory(cat),
		Code:              string(cat),
		Message:           security.Redact(err.Error()),
		Provider:          provider,
		UpstreamStatus:    0, // 传输失败，无 HTTP 状态码
		UpstreamRequestID: requestID,
	}
}

// upstreamErrEnvelope 覆盖各家错误体的常见形状：
// OpenAI / DeepSeek / Kimi / 智谱 / Qwen / Grok: {"error":{"message","type","code"}}
// Anthropic: {"type":"error","error":{"type","message"}}
// Gemini: {"error":{"code","message","status"}}
// MiniMax: {"base_resp":{"status_code","status_msg"}}
// 也兼容顶层 {"message"} / {"code"}。
type upstreamErrEnvelope struct {
	Error *struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
	Message  string `json:"message"`
	BaseResp *struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// extractUpstreamError 从错误体提取对外错误码与消息；解析不了就返回空串，
// 由调用方兜底（⛔ 不猜、不编造）。
func extractUpstreamError(body []byte) (code, msg string) {
	if len(body) == 0 {
		return "", ""
	}
	var env upstreamErrEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", ""
	}
	if env.BaseResp != nil {
		// MiniMax 形状：业务码即对外码。
		return "minimax_" + strconv.Itoa(env.BaseResp.StatusCode), env.BaseResp.StatusMsg
	}
	if env.Error != nil {
		code = rawCodeToString(env.Error.Code)
		if code == "" {
			code = env.Error.Type
		}
		return code, env.Error.Message
	}
	return "", env.Message
}

// rawCodeToString 兼容 error.code 为字符串或数字两种真实形态。
// 按字面形状分派、每种形态只 Unmarshal 一次（旧实现「先试 string、再试 Number」
// 对数字形态必白跑一次失败解析）；实测口径：null 落哪支都出空串、
// true/数组/对象两种做法都拿不到值 ⇒ 分派后的行为与旧实现逐字一致。
func rawCodeToString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var n json.Number
		if err := json.Unmarshal(raw, &n); err == nil {
			return n.String()
		}
	}
	return ""
}
