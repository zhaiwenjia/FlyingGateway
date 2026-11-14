// 日期: 2026-11-14 11:50:04.226
// 作者: Zhai Wenjia

package errors

import (
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// TestFromUpstream_PreservesUpstreamFields 断言原始上游信息保留且脱敏
// （统一 SDD 11.3「原始信息保留」）。
// 断言有效性：若实现只留归一分类不填 UpstreamStatus/UpstreamRequestID/UpstreamBody，
// 对应字段断言会红；若实现不脱敏，真实 key 命中断言会红。
func TestFromUpstream_PreservesUpstreamFields(t *testing.T) {
	const realKey = "sk-upstream-secret-9f8e7d6c5b4a"
	body := []byte(`{"error":{"message":"bad key ` + realKey + `","type":"invalid_request_error","code":"invalid_api_key"}}`)
	d := FromUpstream(core.ProviderOpenAI, 401, body, "req_abc123")

	if d.UpstreamStatus != 401 {
		t.Fatalf("UpstreamStatus = %d, 期望 401", d.UpstreamStatus)
	}
	if d.UpstreamRequestID != "req_abc123" {
		t.Fatalf("UpstreamRequestID = %q, 期望 req_abc123", d.UpstreamRequestID)
	}
	if d.UpstreamBody == "" {
		t.Fatal("UpstreamBody 为空，原始响应体丢失")
	}
	if strings.Contains(d.UpstreamBody, realKey) {
		t.Fatalf("UpstreamBody 含真实 key：%s", d.UpstreamBody)
	}
	if strings.Contains(d.Message, realKey) {
		t.Fatalf("Message 含真实 key：%s", d.Message)
	}
	if d.Provider != core.ProviderOpenAI {
		t.Fatalf("Provider = %v, 期望 openai", d.Provider)
	}
	if d.Category != core.ErrAuth {
		t.Fatalf("Category = %s, 期望 auth", d.Category)
	}
	if d.Code != "invalid_api_key" {
		t.Fatalf("Code = %q, 期望 invalid_api_key（取自错误体 error.code）", d.Code)
	}
}

// TestFromUpstream_CategorySeparation 断言 4xx 与 5xx 归类不混（统一 SDD 7.1）。
// 断言有效性：实现若把 5xx 也归 invalid_request 或反之，本测试会红。
func TestFromUpstream_CategorySeparation(t *testing.T) {
	cases := []struct {
		status int
		want   core.ErrorCategory
	}{
		{400, core.ErrInvalidRequest},
		{401, core.ErrAuth},
		{403, core.ErrAuth},
		{404, core.ErrInvalidRequest},
		{429, core.ErrRateLimit},
		{500, core.ErrUpstreamError},
		{503, core.ErrUpstreamError},
	}
	for _, c := range cases {
		d := FromUpstream(core.ProviderAnthropic, c.status, []byte(`{"error":{"message":"x"}}`), "req_1")
		if d.Category != c.want {
			t.Fatalf("status=%d ⇒ Category=%s, 期望 %s", c.status, d.Category, c.want)
		}
	}
}

// TestFromMiniMaxBaseResp 映射 MiniMax 业务码（统一 SDD 4.3.11）。
// 断言有效性：映射表错位（如把 1008 余额当限流）任一行会红；
// 未知码若被猜成某具体类而非 upstream_error，末行会红。
func TestFromMiniMaxBaseResp(t *testing.T) {
	cases := []struct {
		code int
		want core.ErrorCategory
	}{
		{1004, core.ErrAuth},
		{1002, core.ErrRateLimit},
		{1008, core.ErrBudgetExceeded},
		{1001, core.ErrTimeout},
		{1013, core.ErrUpstreamError},
		{2013, core.ErrInvalidRequest},
		{1039, core.ErrContextExceeded},
		{1027, core.ErrContentPolicy},
		{1000, core.ErrUpstreamError},
		{9999, core.ErrUpstreamError},
	}
	for _, c := range cases {
		d := FromMiniMaxBaseResp(c.code, "some status msg")
		if d.Category != c.want {
			t.Fatalf("base_resp.status_code=%d ⇒ Category=%s, 期望 %s", c.code, d.Category, c.want)
		}
		if d.Provider != core.ProviderMiniMax {
			t.Fatalf("Provider = %v, 期望 minimax", d.Provider)
		}
		if d.UpstreamStatus != 200 {
			t.Fatalf("UpstreamStatus = %d, 期望 200（业务错误承载于 HTTP 200）", d.UpstreamStatus)
		}
		if d.Code != fmt.Sprintf("minimax_%d", c.code) {
			t.Fatalf("Code = %q, 期望 minimax_%d", d.Code, c.code)
		}
	}
}

// TestFromTransportError 覆盖超时 / 取消 / 连接失败归类（统一 SDD 11.3「超时/取消」行）。
// 断言有效性：把 timeout 归成 upstream_error 或把 cancelled 当可重试错误归类，
// 对应断言会红；连接失败若归 timeout 也会红。
func TestFromTransportError(t *testing.T) {
	// 真实超时错误：os.ErrDeadlineExceeded 实现 net.Error{Timeout()=true}。
	timeoutErr := fmt.Errorf("dial tcp: %w", os.ErrDeadlineExceeded)
	d := FromTransportError(core.ProviderQwen, timeoutErr, "req_t")
	if d.Category != core.ErrTimeout {
		t.Fatalf("超时 ⇒ Category=%s, 期望 timeout", d.Category)
	}

	// 真实取消：context.Canceled。
	cancelErr := fmt.Errorf("request canceled: %w", context.Canceled)
	d = FromTransportError(core.ProviderQwen, cancelErr, "req_c")
	if d.Category != core.ErrCancelled {
		t.Fatalf("取消 ⇒ Category=%s, 期望 cancelled", d.Category)
	}
	// ⛔ 取消不触发降级 / 重试（统一 SDD 7.5）：分类必须是 cancelled 而不是
	// upstream_error（upstream_error 可重试可降级），否则上层会错误重试。
	if d.Category == core.ErrUpstreamError {
		t.Fatal("取消被归成 upstream_error ⇒ 会触发降级/重试，违反 7.5")
	}

	// 连接失败：net.OpError（net.Dial 的真实错误形态）。
	connErr := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 8000}, Err: stderrors.New("connect: connection refused")}
	d = FromTransportError(core.ProviderOllama, connErr, "req_x")
	if d.Category != core.ErrUpstreamError {
		t.Fatalf("连接失败 ⇒ Category=%s, 期望 upstream_error", d.Category)
	}
}

// TestNew_RedactsMessage 断言构造时消息过统一遮蔽（出口层一次遮蔽，8.1）。
// 断言有效性：不过遮蔽的实现会红（真实 key 命中）。
func TestNew_RedactsMessage(t *testing.T) {
	const realKey = "sk-new-secret-112233445566"
	d := New(core.ErrAuth, 401, "invalid_api_key", "bad key "+realKey)
	if strings.Contains(d.Message, realKey) {
		t.Fatalf("Message 含真实 key：%s", d.Message)
	}
	if d.Category != core.ErrAuth || d.HTTPStatus != 401 {
		t.Fatalf("Category=%s HTTPStatus=%d, 期望 auth/401", d.Category, d.HTTPStatus)
	}
}
