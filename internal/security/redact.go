// 日期: 2026-11-14 19:39:55.215
// 作者: Zhai Wenjia

package security

import (
	"regexp"
	"strings"
)

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
//
// 热路径口径（2026-11-14 优化，遮蔽行为零变化）：先用无正则的触发条件粗筛，
// 七种形态都不可能出现时直接原样返回；否则每个模式只在自己的触发条件命中时才跑。
// ⚠️ 触发条件必须是各模式可匹配的**必要条件**（宁可误触发、⛔ 不许漏触发）——
// 漏触发 = 凭据明文泄漏；判据在各模式注释里逐条写明「匹配 ⇒ 原文必含什么」。
func Redact(s string) string {
	if s == "" {
		return s
	}
	// 触发条件一律在**替换前**的原文上判定：替换只产生 [REDACTED:<类型>] 标记
	// （含 ":" 分隔符，不会拼出新触发子串，也不会拼出 ≥20 连续 token 字符），
	// 其余文本是原文片段 ⇒ 原文无触发子串 ⇒ 之后的中间串同样不可能被匹配。
	// lower 用于 (?i) 模式的子串判定：strings.ToLower 对触发子串的折叠覆盖面
	// = 逐字母核对（authorization / api-key / bearer / key / token 全部纯 ASCII，
	// 含 k 的折叠异体 Kelvin 符号（U+212A）ToLower 归一为 k；唯一 ToLower 不归一的
	// 折叠异体长 s（U+017F）只与 s/S 折叠，而触发子串无一含 s；U+0130 经实测不被
	// (?i) 当 i 匹配）⇒ 触发判定无漏触发面。
	lower := strings.ToLower(s)
	trigAuth := strings.Contains(lower, "authorization") // 模式 1 匹配文本必含 authorization（(?i)）
	trigAPIKey := strings.Contains(lower, "api-key")     // 模式 2 匹配文本必含 api-key / x-api-key（(?i)）
	trigQuery := strings.Contains(s, "=") && (strings.Contains(lower, "key") || strings.Contains(lower, "token"))
	// 模式 3 匹配文本必含 "=" 与 key / api_key / api-key / access_token / access-token / token 之一（(?i)）
	trigBearer := strings.Contains(lower, "bearer") // 模式 4 匹配文本必含 bearer（(?i)）
	trigSk := strings.Contains(s, "sk-")            // 模式 5 匹配文本必含 sk-（大小写敏感）
	trigJWT := strings.Contains(s, "eyJ")           // 模式 6 匹配文本必含 eyJ（大小写敏感）
	trigLong := hasLongTokenRun(s)                  // 模式 7 必含 ≥20 连续 [A-Za-z0-9_+/=]（\b 只会更严）
	if !trigAuth && !trigAPIKey && !trigQuery && !trigBearer && !trigSk && !trigJWT && !trigLong {
		return s
	}
	if trigAuth {
		s = replaceMatches(reAuthHeader, s, func(g []string) string {
			return g[1] + g[2] + redactValue(g[3], "authorization")
		})
	}
	if trigAPIKey {
		s = replaceMatches(reAPIKeyHeader, s, func(g []string) string {
			return g[1] + redactValue(g[2], "api_key")
		})
	}
	if trigQuery {
		s = replaceMatches(reQueryKey, s, func(g []string) string {
			return g[1] + redactValue(g[2], "api_key")
		})
	}
	if trigBearer {
		s = replaceMatches(reBearer, s, func(g []string) string {
			return g[1] + redactValue(g[2], "authorization")
		})
	}
	if trigSk {
		s = reSkToken.ReplaceAllString(s, "[REDACTED:api_key]")
	}
	if trigJWT {
		s = reJWT.ReplaceAllString(s, "[REDACTED:api_key]")
	}
	if trigLong {
		s = replaceMatches(reSecretish, s, func(g []string) string {
			if looksSecret(g[0]) {
				return "[REDACTED:api_key]"
			}
			return g[0]
		})
	}
	return s
}

// replaceMatches 用「一次全局扫描 + 逐段重建」替代 ReplaceAllStringFunc + 回调内
// FindStringSubmatch 的二次正则执行。现象：json 形文本遮蔽一次要 59µs，回调内
// FindStringSubmatch(m) 把刚匹配过的文本又整体跑了一遍正则；
// 根因：ReplaceAllStringFunc 只给匹配文本、不给捕获组，旧实现靠二次匹配取组；
// 为什么这么修：FindAllStringSubmatchIndex 一次拿全部匹配与捕获组索引，
// 行为与 ReplaceAllStringFunc 逐字一致（最左、不重叠、非匹配段原样保留；
// 本仓七个模式都不可能匹配空串，无空匹配推进语义差异）；
// 不修会怎样：每个命中多跑一遍正则，遮蔽在错误/日志出口的 CPU 成本翻倍。
// 替换结果与原文相同时不重建（模式 7 有「疑似但非密钥」原样返回的分支）。
func replaceMatches(re *regexp.Regexp, s string, repl func(groups []string) string) string {
	loc := re.FindAllStringSubmatchIndex(s, -1)
	if len(loc) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prev := 0
	changed := false
	for _, m := range loc {
		b.WriteString(s[prev:m[0]])
		groups := make([]string, len(m)/2)
		for i := range groups {
			if m[2*i] >= 0 {
				groups[i] = s[m[2*i]:m[2*i+1]]
			}
		}
		rep := repl(groups)
		if rep != groups[0] {
			changed = true
		}
		b.WriteString(rep)
		prev = m[1]
	}
	b.WriteString(s[prev:])
	if !changed {
		return s
	}
	return b.String()
}

// hasLongTokenRun 判定是否存在长度 ≥20 的 [A-Za-z0-9_+/=] 连续段（模式 7 的必要条件）。
// ⚠️ 只做必要条件、不做充分条件：不复核 \b 边界与「含字母与数字」（那是
// looksSecret 的细筛）——粗筛多触发只多跑一遍模式 7，漏触发才会漏遮蔽。
func hasLongTokenRun(s string) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '+', c == '/', c == '=':
			run++
			if run >= 20 {
				return true
			}
		default:
			run = 0
		}
	}
	return false
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
