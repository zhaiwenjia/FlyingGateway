// 日期: 2026-11-26 09:10:00.000
// 作者: Zhai Wenjia
package server

// 等待中信号 F1 钉子（NGAS SDD spec-waiting-signal AC-04，先红后绿）：
// 429 响应体加结构化 wait 字段（phase + retry_after_s）；Retry-After 头与
// error.code/message/type 老字段逐字节不变（向后兼容：不读 wait 的老客户端
// 行为不变）。
//
// 运行：go test ./internal/server/ -run 'Test429WaitField' -count=1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Test429WaitField 单元钉：writeRetryAfterWait 的响应形状。
func Test429WaitField(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRetryAfterWait(rec, 3*time.Second, "rate_limit", "rate_limit",
		"配额超限", "rate_limited")

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, 期望 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q, 期望 3（向上取整语义不变）", got)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
			Wait    *struct {
				Phase       string `json:"phase"`
				RetryAfterS int64  `json:"retry_after_s"`
			} `json:"wait"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	// 老字段不变（AC-04 向后兼容）
	if body.Error.Code != "rate_limit" || body.Error.Type != "rate_limit" ||
		body.Error.Message != "配额超限" {
		t.Fatalf("老字段被改动: %+v", body.Error)
	}
	// 新增 wait 字段
	if body.Error.Wait == nil {
		t.Fatal("429 响应体必须含 error.wait（AC-04）")
	}
	if body.Error.Wait.Phase != "rate_limited" {
		t.Fatalf("wait.phase = %q, 期望 rate_limited", body.Error.Wait.Phase)
	}
	if body.Error.Wait.RetryAfterS != 3 {
		t.Fatalf("wait.retry_after_s = %d, 期望 3", body.Error.Wait.RetryAfterS)
	}
}

// Test429WaitFieldOnRateLimit 集成钉（AC-04 端到端）：契约配额 429 的真实
// 响应路径携带 error.wait（复用 caller_test 的契约桶先例环境）。
func Test429WaitFieldOnRateLimit(t *testing.T) {
	e := newEnv(t, func(base string) []epSpec { return []epSpec{primaryEP(base)} }, nil, nil, map[string]any{
		"callers": []map[string]any{
			callersWire("concwait", "ck-concwait", time.Now().Add(24*time.Hour),
				[]string{"gpt-4o-mini"}, []string{"primary"}, 0,
				map[string]any{"tpm": 1200, "rpm": 0, "burst_s": 1}),
		},
	})
	e.up.setEvents(loadSSE(t, "openai_responses_stream.sse"))
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	auth := map[string]string{"Authorization": "Bearer ck-concwait"}
	resp := e.postJSON("/v1/chat/completions", body, auth)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("首发 = %d, 期望 200", resp.StatusCode)
	}
	readBody(t, resp)
	resp2 := e.postJSON("/v1/chat/completions", body, auth)
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("第二发 = %d, 期望 429（契约桶扣穿）", resp2.StatusCode)
	}
	if resp2.Header.Get("Retry-After") == "" {
		t.Fatal("429 缺 Retry-After 头（老契约不变）")
	}
	// readBody 返回已解析的 map（caller_test 同款口径）
	parsed := readBody(t, resp2)
	errObj, _ := parsed["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("响应缺 error 对象: %#v", parsed)
	}
	if code, _ := errObj["code"].(string); code != "rate_limit" {
		t.Fatalf("code = %q, 期望 rate_limit（老字段不变）", code)
	}
	wait, _ := errObj["wait"].(map[string]any)
	if wait == nil {
		t.Fatal("限流 429 必须携带 error.wait（AC-04）")
	}
	if ph, _ := wait["phase"].(string); ph != "rate_limited" {
		t.Fatalf("wait.phase = %q, 期望 rate_limited", ph)
	}
	if ra, _ := wait["retry_after_s"].(float64); ra < 1 {
		t.Fatalf("wait.retry_after_s = %v, 期望 ≥1", ra)
	}
}

// Test429WaitFieldBackwardCompat 单元钉：老 writeRetryAfter 不产生 wait 字段
// （只增不改：老调用点行为逐字节不变）。
func Test429WaitFieldBackwardCompat(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRetryAfter(rec, 2*time.Second, "rate_limit", "rate_limit", "老路径")

	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, 期望 2", got)
	}
	var body struct {
		Error struct {
			Code string          `json:"code"`
			Wait json.RawMessage `json:"wait"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Error.Code != "rate_limit" {
		t.Fatalf("code = %q", body.Error.Code)
	}
	if len(body.Error.Wait) != 0 {
		t.Fatalf("老路径不得出现 wait 字段（只增不改），实得 %s", body.Error.Wait)
	}
}
