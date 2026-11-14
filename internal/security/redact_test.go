// 日期: 2026-11-14 11:50:04.486
// 作者: Zhai Wenjia

package security

import (
	"strings"
	"testing"
)

// TestRedact_NoRealKeySurvives 把五种真实凭据形态拼进一段「日志 / 异常序列化」
// 文本，遮蔽后逐个搜真实 key ⇒ 必须 0 命中（统一 SDD 11.3「凭据不落明文」）。
//
// 断言有效性（11.5「错误实现会红」对照点）：
//   - 若遮蔽只处理 Authorization 不处理 x-api-key / api-key / ?key= / sk-，
//     对应 key 的 Contains 断言会红；
//   - 若遮蔽形态各写各的（*** / xxx），[REDACTED: 前缀断言会红。
func TestRedact_NoRealKeySurvives(t *testing.T) {
	const (
		authKey   = "sk-auth-9f8e7d6c5b4a3311"
		xAPIKey   = "xk-x-header-77aa88bb99cc"
		apiKeyVal = "mk-mimo-554433221100"
		queryKey  = "AIzaSyRealQueryKey123456"
		skBare    = "sk-bare-token-0a1b2c3d4e5f"
	)
	raw := "异常序列化: 调用上游失败 " +
		"Authorization: Bearer " + authKey + " " +
		"x-api-key: " + xAPIKey + " " +
		"api-key: " + apiKeyVal + " " +
		"GET https://generativelanguage.googleapis.com/v1/models?key=" + queryKey + " " +
		"裸密钥 " + skBare + " 结束"

	out := Redact(raw)

	for _, k := range []string{authKey, xAPIKey, apiKeyVal, queryKey, skBare} {
		if strings.Contains(out, k) {
			t.Fatalf("遮蔽后仍含真实 key %q：%s", k, out)
		}
	}
	if !strings.Contains(out, "[REDACTED:authorization]") {
		t.Fatalf("Authorization 未按 [REDACTED:authorization] 形态遮蔽：%s", out)
	}
	if !strings.Contains(out, "[REDACTED:api_key]") {
		t.Fatalf("未按 [REDACTED:api_key] 形态遮蔽：%s", out)
	}
}

// TestRedact_JsonSerializationForm 覆盖 JSON 序列化形态的头字段
// （异常序列化后就是这样），并断言引号结构保留。
// 断言有效性：不处理 `"x-api-key": "..."` 形态的实现会红。
func TestRedact_JsonSerializationForm(t *testing.T) {
	const (
		jwtish = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
		xk     = "json-x-key-445566778899"
	)
	raw := `{"Authorization":"Bearer ` + jwtish + `","x-api-key":"` + xk + `"}`
	out := Redact(raw)
	if strings.Contains(out, jwtish) {
		t.Fatalf("JWT 未被遮蔽：%s", out)
	}
	if strings.Contains(out, xk) {
		t.Fatalf("x-api-key 值未被遮蔽：%s", out)
	}
	if !strings.Contains(out, `"Authorization":"[REDACTED:authorization]"`) {
		t.Fatalf("Authorization JSON 形态遮蔽后引号结构被破坏：%s", out)
	}
	if !strings.Contains(out, `"x-api-key":"[REDACTED:api_key]"`) {
		t.Fatalf("x-api-key JSON 形态遮蔽后引号结构被破坏：%s", out)
	}
}

// TestRedact_BenignTextUnchanged 断言普通文本不被误伤（观测量=原样返回）。
// 断言有效性：无差别全替换的错误实现会红。
func TestRedact_BenignTextUnchanged(t *testing.T) {
	benign := "请求失败：上下文超限（context_window 128000 tokens），请裁剪历史后重试"
	if got := Redact(benign); got != benign {
		t.Fatalf("普通文本被误伤：%s", got)
	}
}
