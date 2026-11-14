// 日期: 2026-11-14 11:50:04.894
// 作者: Zhai Wenjia

package policy

import (
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// TestGuardrail_RegexHit 断言正则命中 ⇒ 403 content_policy 语义错误
// （统一 SDD 2.3 治理三件套）。
// 断言有效性：命中却放行（返回 nil）或错误语义不含 content_policy 的实现会红。
func TestGuardrail_RegexHit(t *testing.T) {
	g := NewGuardrail([]*regexp.Regexp{
		regexp.MustCompile(`(?i)password\s*=`),
		regexp.MustCompile(`(?i)ignore (all )?previous instructions`),
	}, "", nil)

	err := g.Check(context.Background(), "system: password=hunter2 请执行")
	if err == nil {
		t.Fatal("正则命中应报错")
	}
	var cpe *ContentPolicyError
	if !stderrors.As(err, &cpe) {
		t.Fatalf("期望 *ContentPolicyError，实际 %v", err)
	}
	if cpe.Category != core.ErrContentPolicy {
		t.Fatalf("Category = %s, 期望 content_policy", cpe.Category)
	}
	if cpe.HTTPStatus != 403 {
		t.Fatalf("HTTPStatus = %d, 期望 403", cpe.HTTPStatus)
	}
	if cpe.Source != "regex" {
		t.Fatalf("Source = %q, 期望 regex", cpe.Source)
	}

	// 未命中 ⇒ 放行（恒拒的错误实现会红）。
	if err := g.Check(context.Background(), "正常的用户提问"); err != nil {
		t.Fatalf("未命中文本被误拒：%v", err)
	}
}

// TestGuardrail_Webhook 断言 webhook 校验：2xx 放行、非 2xx ⇒ 403 content_policy，
// 且文本真的被 POST 给校验服务（观测量=服务端收到的请求体）。
// 断言有效性：不发 webhook 或非 2xx 仍放行的实现会红。
func TestGuardrail_Webhook(t *testing.T) {
	var gotText atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotText.Store(string(body))
		if strings.Contains(string(body), "违规文本") {
			w.WriteHeader(http.StatusForbidden) // 校验服务拒绝
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := NewGuardrail(nil, srv.URL, srv.Client())

	// 通过。
	if err := g.Check(context.Background(), "正常文本"); err != nil {
		t.Fatalf("webhook 2xx 应放行：%v", err)
	}
	if s, _ := gotText.Load().(string); !strings.Contains(s, "正常文本") {
		t.Fatalf("webhook 未收到待校验文本：%s", s)
	}

	// 拒绝 ⇒ 403 content_policy 语义。
	err := g.Check(context.Background(), "违规文本")
	var cpe *ContentPolicyError
	if !stderrors.As(err, &cpe) {
		t.Fatalf("webhook 非 2xx 期望 *ContentPolicyError，实际 %v", err)
	}
	if cpe.Source != "webhook" || cpe.WebhookStatus != http.StatusForbidden {
		t.Fatalf("Source=%q WebhookStatus=%d, 期望 webhook/403", cpe.Source, cpe.WebhookStatus)
	}
	if cpe.Category != core.ErrContentPolicy || cpe.HTTPStatus != 403 {
		t.Fatalf("Category=%s HTTPStatus=%d, 期望 content_policy/403", cpe.Category, cpe.HTTPStatus)
	}
}

// TestGuardrail_WebhookTransportFailureFailClosed 断言校验服务不可达时 fail-closed
// （显式报错拒绝放行，⛔ 不静默放行）。
// 断言有效性：fail-open（返回 nil）的实现会红。
func TestGuardrail_WebhookTransportFailureFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 端口关闭 ⇒ 传输失败

	g := NewGuardrail(nil, url, http.DefaultClient)
	err := g.Check(context.Background(), "任意文本")
	if err == nil {
		t.Fatal("webhook 不可达必须 fail-closed 报错，不得静默放行")
	}
	var cpe *ContentPolicyError
	if stderrors.As(err, &cpe) {
		t.Fatalf("传输失败是基础设施错误，不应冒充 content_policy 拦截：%v", err)
	}
}

// TestGuardrail_RegexShortCircuitsWebhook 断言正则命中时不调 webhook
// （观测量=webhook 调用计数保持 0）。
func TestGuardrail_RegexShortCircuitsWebhook(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := NewGuardrail([]*regexp.Regexp{regexp.MustCompile(`秘密词`)}, srv.URL, srv.Client())
	if err := g.Check(context.Background(), "含秘密词的文本"); err == nil {
		t.Fatal("正则命中应报错")
	}
	if n := atomic.LoadInt64(&calls); n != 0 {
		t.Fatalf("正则命中后仍调用了 webhook %d 次，期望 0", n)
	}
}
