// 日期: 2026-11-14 11:50:04.743
// 作者: Zhai Wenjia

package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// ContentPolicyError 是 guardrail 拦截的类型化错误（统一 SDD 2.3 治理三件套）。
// 语义固定 403 content_policy：正则命中或 Webhook 前置校验拒绝均返回本错误，
// 上层映射 `403 content_policy`。
type ContentPolicyError struct {
	// Source 标明拦截来源：regex / webhook。
	Source string
	// Pattern 是命中的正则原文（Source=regex 时有值）。
	Pattern string
	// WebhookStatus 是 Webhook 返回的 HTTP 状态码（Source=webhook 时有值）。
	WebhookStatus int
	Message       string
	// Category 固定 core.ErrContentPolicy；HTTPStatus 固定 403，
	// 便于上层直接映射 `403 content_policy`。
	Category   core.ErrorCategory
	HTTPStatus int
}

func (e *ContentPolicyError) Error() string {
	return fmt.Sprintf("policy: content_policy: %s", e.Message)
}

// Guardrail 是内容护栏（统一 SDD 2.3 治理三件套）：正则匹配 + Webhook 前置校验，
// 均在上游请求**之前**执行；命中 / 拒绝 ⇒ 403 content_policy。
type Guardrail struct {
	patterns   []*regexp.Regexp
	webhookURL string
	hc         *http.Client
}

// NewGuardrail 构造护栏。hc 为 nil 时用 http.DefaultClient；
// webhookURL 为空表示只做正则校验。
func NewGuardrail(patterns []*regexp.Regexp, webhookURL string, hc *http.Client) *Guardrail {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Guardrail{patterns: patterns, webhookURL: webhookURL, hc: hc}
}

// guardrailWebhookRequest 是 Webhook 前置校验的请求体。
type guardrailWebhookRequest struct {
	Text string `json:"text"`
}

// Check 在上游请求之前校验文本：先正则、后 Webhook（配置了才调）。
//
//   - 正则命中 ⇒ *ContentPolicyError（403 content_policy）；
//   - Webhook 返回非 2xx ⇒ 同上；
//   - Webhook 网络 / 传输失败 ⇒ 显式报错并拒绝放行（fail-closed）——
//     校验服务不可达时静默放行等于护栏失效，⛔ 不允许。
func (g *Guardrail) Check(ctx context.Context, text string) error {
	for _, p := range g.patterns {
		if p == nil {
			continue
		}
		if p.MatchString(text) {
			return &ContentPolicyError{
				Source:     "regex",
				Pattern:    p.String(),
				Message:    fmt.Sprintf("guardrail 正则命中：%s", p.String()),
				Category:   core.ErrContentPolicy,
				HTTPStatus: 403,
			}
		}
	}
	if g.webhookURL == "" {
		return nil
	}
	payload, err := json.Marshal(guardrailWebhookRequest{Text: text})
	if err != nil {
		return fmt.Errorf("policy: guardrail webhook 请求体序列化失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.webhookURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("policy: guardrail webhook 请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		// fail-closed：校验服务不可达 ⇒ 显式报错拒绝放行。
		return fmt.Errorf("policy: guardrail webhook 调用失败（fail-closed 拒绝放行）: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &ContentPolicyError{
			Source:        "webhook",
			WebhookStatus: resp.StatusCode,
			Message:       fmt.Sprintf("guardrail webhook 拒绝（HTTP %d）", resp.StatusCode),
			Category:      core.ErrContentPolicy,
			HTTPStatus:    403,
		}
	}
	return nil
}
