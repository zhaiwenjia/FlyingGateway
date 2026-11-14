// 日期: 2026-11-14 11:50:04.329
// 作者: Zhai Wenjia

package security

import "regexp"

// Redact 是出口层统一遮蔽（统一 SDD 8.1 / 规则 21）。
// 遮蔽形态统一 [REDACTED:<类型>]（如 [REDACTED:api_key] / [REDACTED:authorization]），
// ⛔ 不许 *** / xxx 之类各写各的 —— 形态不统一，下游就无法只靠一个正则全筛掉。
//
// ⚠️ 全仓遮蔽实现只此一处；errors / 日志 / 轨迹 / 异常序列化都调用本函数，
// ⛔ 不重复实现遮蔽规则（统一 SDD 8.1 出口层一次遮蔽）。
//
// 覆盖形态：
//  1. Authorization: Bearer xxx / Authorization: xxx（含 JSON 序列化形态）
//  2. x-api-key: xxx / api-key: xxx（含 JSON 序列化形态）
//  3. ?key=xxx 查询串（key / api_key / api-key / token / access_token）
//  4. 独立的 Bearer xxx
//  5. sk-... 形态密钥（OpenAI / DeepSeek / Kimi / 智谱 等）
//  6. JWT（eyJ 开头的三段式）
//  7. 任意疑似密钥 token（≥20 字符且含字母与数字的连续 token）
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = reAuthHeader.ReplaceAllStringFunc(s, func(m string) string {
		g := reAuthHeader.FindStringSubmatch(m)
		return g[1] + g[2] + redactValue(g[3], "authorization")
	})
	s = reAPIKeyHeader.ReplaceAllStringFunc(s, func(m string) string {
		g := reAPIKeyHeader.FindStringSubmatch(m)
		return g[1] + redactValue(g[2], "api_key")
	})
	s = reQueryKey.ReplaceAllStringFunc(s, func(m string) string {
		g := reQueryKey.FindStringSubmatch(m)
		return g[1] + redactValue(g[2], "api_key")
	})
	s = reBearer.ReplaceAllStringFunc(s, func(m string) string {
		g := reBearer.FindStringSubmatch(m)
		return g[1] + redactValue(g[2], "authorization")
	})
	s = reSkToken.ReplaceAllString(s, "[REDACTED:api_key]")
	s = reJWT.ReplaceAllString(s, "[REDACTED:api_key]")
	s = reSecretish.ReplaceAllStringFunc(s, func(m string) string {
		if looksSecret(m) {
			return "[REDACTED:api_key]"
		}
		return m
	})
	return s
}

// redactValue 把捕获到的凭据值替换为 [REDACTED:<类型>]，
// 保留值两侧的引号（JSON 序列化形态不被破坏）。
func redactValue(v, typ string) string {
	quote := ""
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		quote = string(v[0])
	}
	return quote + "[REDACTED:" + typ + "]" + quote
}

// valuePattern 匹配一个凭据值：JSON 双引号串 / 单引号串 / 裸 token。
const valuePattern = `("(?:[^"\\]|\\.)*"|'[^']*'|[^\s,;&}\]]+)`

var (
	// Authorization 头：保留头名与 Bearer 前缀，只遮值。
	reAuthHeader = regexp.MustCompile(`(?i)(authorization["']?\s*:\s*)(bearer\s+)?` + valuePattern)
	// x-api-key / api-key 头（含 JSON 序列化的 "x-api-key": "..." 形态）。
	reAPIKeyHeader = regexp.MustCompile(`(?i)(["']?(?:x-api-key|api-key)["']?\s*:\s*)` + valuePattern)
	// 查询串凭据：?key= / &api_key= / &token= 等（⛔ 凭据本不该进查询串，8.1）。
	reQueryKey = regexp.MustCompile(`(?i)([?&]["']?(?:key|api[_-]?key|access[_-]?token|token)["']?\s*=\s*)` + valuePattern)
	// 独立 Bearer token。
	reBearer = regexp.MustCompile(`(?i)\b(bearer\s+)` + valuePattern)
	// sk-... 形态密钥。
	reSkToken = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{6,}`)
	// JWT（三段 base64url）。
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}`)
	// 任意疑似密钥 token（长度与字符集先粗筛，再由 looksSecret 细筛）。
	reSecretish = regexp.MustCompile(`\b[A-Za-z0-9_+/=]{20,}\b`)
)

// looksSecret 判定一个长 token 是否疑似密钥：必须同时含字母与数字。
func looksSecret(tok string) bool {
	hasLetter, hasDigit := false, false
	for _, r := range tok {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
		if hasLetter && hasDigit {
			return true
		}
	}
	return false
}
