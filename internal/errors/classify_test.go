// 日期: 2026-11-14 11:50:04.279
// 作者: Zhai Wenjia

package errors

import (
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// TestClassifyHTTPStatus 覆盖 400/401/403/404/429/500/503 各自归类
// （统一 SDD 11.3「七类归类」，4xx 与 5xx ⛔ 不混）。
//
// 断言有效性（11.5「错误实现会红」对照点）：
//   - 若实现把 429 归成 upstream_error（4xx/5xx 混类），429 行会红；
//   - 若实现把 500 归成 invalid_request，500 行会红；
//   - 若实现不看错误体语义（400 恒 invalid_request），context/body 行会红。
func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   core.ErrorCategory
	}{
		{"400 基线", 400, `{"error":{"message":"messages[0].role 必须是 system/user"}}`, core.ErrInvalidRequest},
		{"400 context 语义", 400, `{"error":{"message":"This model's maximum context length is 128000 tokens"}}`, core.ErrContextExceeded},
		{"400 window 语义", 400, `{"error":{"message":"input length and max_tokens exceed context limit"}}`, core.ErrContextExceeded},
		{"400 content_policy 语义", 400, `{"error":{"message":"Your request was rejected as a result of our safety system"}}`, core.ErrContentPolicy},
		{"401", 401, `{"error":{"message":"Incorrect API key provided"}}`, core.ErrAuth},
		{"403 内容安全拦截", 403, `{"error":{"message":"output content_filter triggered，内容违规"}}`, core.ErrContentPolicy},
		{"403 权限不足", 403, `{"error":{"message":"Permission denied: insufficient scope"}}`, core.ErrAuth},
		{"404", 404, `{"error":{"message":"The model ` + "`gpt-9`" + ` does not exist"}}`, core.ErrInvalidRequest},
		{"429 限流", 429, `{"error":{"message":"Rate limit reached for requests"}}`, core.ErrRateLimit},
		{"429 预算", 429, `{"error":{"message":"budget exceeded for this workspace"}}`, core.ErrBudgetExceeded},
		{"500", 500, `{"error":{"message":"The server had an error"}}`, core.ErrUpstreamError},
		{"503", 503, `{"error":{"message":"Service unavailable"}}`, core.ErrUpstreamError},
		{"无错误体 502", 502, ``, core.ErrUpstreamError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyHTTPStatus(c.status, []byte(c.body))
			if got != c.want {
				t.Fatalf("classifyHTTPStatus(%d, %q) = %s, 期望 %s", c.status, c.body, got, c.want)
			}
		})
	}
}
