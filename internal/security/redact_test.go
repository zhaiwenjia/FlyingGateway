// 日期: 2026-11-14 19:39:55.222
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

// ---- 微基准（出口遮蔽热路径；先测后改，基线 vs 优化对照）----
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/security/ -bench=. -benchmem

// benchRedactClean 是长普通文本（无凭据；中文 + 英文 token 混排，约 4.4KB）。
var benchRedactClean = strings.Repeat("请求失败：上下文超限（context_window 128000 tokens），请裁剪历史后重试；transmission completed normally. ", 40)

// benchRedactJSON 是 JSON 形态的异常序列化文本（Authorization / x-api-key / 查询串 / JWT / 裸密钥）。
var benchRedactJSON = `{"error":{"message":"调用上游失败 sk-live-9f8e7d6c5b4a3311","type":"invalid_request_error","code":"invalid_api_key"},"headers":{"Authorization":"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U","x-api-key":"json-x-key-445566778899"},"url":"https://api.example.com/v1/models?key=AIzaSyRealQueryKey123456"}`

// benchRedactBigToken 是含超长 base64 形 token 的文本（reSecretish 的最坏扫描形态）。
var benchRedactBigToken = "附件 data: " + strings.Repeat("YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU2Nzg5", 200) + " 完"

// BenchmarkRedact 测出口层遮蔽（错误 / 日志 / 轨迹序列化都过这里）。
func BenchmarkRedact(b *testing.B) {
	for _, c := range []struct{ name, s string }{
		{"clean-long-text", benchRedactClean},
		{"json-form", benchRedactJSON},
		{"big-base64-token", benchRedactBigToken},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Redact(c.s)
			}
		})
	}
}

// TestRedact_FastPathTriggerCoverage 钉住「触发条件不许漏」：Redact 的无正则预筛
// 只在触发条件命中时才跑对应模式（2026-11-14 性能优化），触发条件必须是各模式
// 可匹配的必要条件。这里给每个模式造「大小写 / Unicode 折叠变体」的可匹配输入，
// 值都短到模式 7（长 token）不可能兜住 ⇒ 谁漏触发谁泄漏、断言必红。
// 断言有效性：把任一触发条件改成大小写敏感 Contains 后，kelvin-query-key /
// long-s-access-token / upper-bearer 会红；删掉任一模式的执行分支也会红。
func TestRedact_FastPathTriggerCoverage(t *testing.T) {
	cases := []struct{ name, raw, secret string }{
		{"upper-auth", "AUTHORIZATION: Bearer sk-live-a1b2c3d4e5", "sk-live-a1b2c3d4e5"},
		{"upper-bearer", "got token BeArEr abc12", "abc12"},
		{"kelvin-query-key", "?to\u212Aen=abc12", "abc12"},
		{"long-s-access-token", "?acce\u017Fs-to\u212Aen=abc12", "abc12"},
		{"upper-api-key", "X-API-KEY: z9y8", "z9y8"},
		{"upper-jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N", "dozjgNryP4J3jVmNHl0w5N"},
	}
	for _, c := range cases {
		out := Redact(c.raw)
		if strings.Contains(out, c.secret) {
			t.Fatalf("%s: 触发条件漏判，真实值原样泄漏：%s", c.name, out)
		}
	}
}
