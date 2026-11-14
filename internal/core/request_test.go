// 日期: 2026-11-14 15:22:40.318
// 作者: Zhai Wenjia

package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCacheControlJSONKeys 钉住 4.3.2 的 cache_control 嵌套键名逐字为 type/ttl。
// ⚠️ 现象（2026-11-14 实测）：无 json tag 时序列化输出 Go 字段名（Type/TTL），
// Anthropic 上游认不了断点 ⇒ 缓存静默失效（无报错、只表现为成本上升）；
// 根因：CacheControl 被 anthropic 请求体直接嵌入序列化，字段名默认取 Go 标识符；
// 为什么这么修：在类型上补 json tag（一处真值），⛔ 不另写 anthropic 侧镜像结构；
// 不修会怎样：断点无效且无任何错误信号，缓存命中率悄悄归零。
// 错误实现对照：删掉 json tag ⇒ 本测试必红。
func TestCacheControlJSONKeys(t *testing.T) {
	raw, err := json.Marshal(CacheControl{Type: "ephemeral", TTL: "5m"})
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"type":"ephemeral"`) || !strings.Contains(s, `"ttl":"5m"`) {
		t.Fatalf("cache_control 序列化 = %s，期望小写键名 type/ttl（4.3.2 逐字）", s)
	}
	if strings.Contains(s, "Type") || strings.Contains(s, "TTL") {
		t.Fatalf("cache_control 序列化出现 Go 字段名键（Type/TTL）: %s", s)
	}
}
