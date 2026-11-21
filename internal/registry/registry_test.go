// 日期: 2026-11-14 14:13:45.857
// 作者: Zhai Wenjia

package registry

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/routing"
)

// writeConfig 把配置 JSON 写进临时目录并返回路径（测试输入是配置文本，非 mock 数据）。
func writeConfig(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	return path
}

// validOne 是一份最小合法配置（字段口径统一 SDD 9.1 / 9.2）。
const validOne = `{
  "strategy": "priority",
  "endpoints": [
    {
      "name": "primary",
      "base_url": "https://api.openai.com/v1",
      "model": "gpt-4o-mini",
      "upstream_model": "gpt-4o-mini",
      "provider_name": "openai",
      "context_window": 128000,
      "max_output_tokens": 16384,
      "capabilities": {"supports_tools": true, "supports_stream": true, "supports_vision": true},
      "credential_ref": "OPENAI_API_KEY",
      "timeout_s": 30,
      "priority": 0
    }
  ]
}`

// TestLoadRejectsMissingRequiredFields 断言必备字段缺失 ⇒ 启动报错
// （统一 SDD 9.2 必备字段完整性 / 11.3「注册表字段完整性」行，⛔ 不用默认值静默补齐）。
// 对照点：实现对缺失字段落零值继续跑 ⇒ 对应子例红。
func TestLoadRejectsMissingRequiredFields(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"缺 upstream_model", `{"strategy":"priority","endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}]}`},
		{"缺 capabilities", `{"strategy":"priority","endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}]}`},
		{"缺 credential_ref", `{"strategy":"priority","endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"timeout_s":30,"priority":0}]}`},
		{"缺 name", `{"strategy":"priority","endpoints":[{"base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}]}`},
		{"缺 base_url", `{"strategy":"priority","endpoints":[{"name":"a","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}]}`},
		{"缺 model", `{"strategy":"priority","endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}]}`},
		{"缺 provider_name", `{"strategy":"priority","endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, tc.raw)); err == nil {
				t.Fatalf("期望启动报错，实际 nil（⛔ 不静默默认值）")
			}
		})
	}
}

// TestLoadRejectsUnknownFields 断言未知字段拒绝（统一 SDD 9.2 / 11.3 注册表完整性）：
// 顶层 / 端点 / capabilities 三层都拒绝，不静默忽略。
// 对照点：去掉 DisallowUnknownFields ⇒ 三个子例全红。
func TestLoadRejectsUnknownFields(t *testing.T) {
	cases := []struct{ name, raw string }{
		// ⚠️ 顶层未知键子例必须带合法端点，让唯一报错源就是未知键本身——
		// 写成 endpoints:[] 会被「endpoints 为空」自检兜住，变异下仍绿（恒真断言，
		// 2026-11-14 变异自检实测发现并修正，统一 SDD 11.5）。
		{"顶层未知键", `{"strategy":"priority","endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}],"bogus":1}`},
		{"端点未知键", `{"strategy":"priority","endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0,"api_key":"sk-x"}]}`},
		{"capabilities 未知键", `{"strategy":"priority","endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{"tools":true},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, tc.raw)); err == nil {
				t.Fatalf("期望未知字段报错，实际 nil")
			}
		})
	}
}

// TestLoadRejectsPlaintextCredential 断言明文密钥形态的 credential_ref ⇒ 报错
// （统一 SDD 8.1：注册表只存来源引用，⛔ 不存明文）。
// 对照点：不做形态检查 ⇒ sk- 子例红；正常环境变量名被误杀 ⇒ 正例子例红。
func TestLoadRejectsPlaintextCredential(t *testing.T) {
	withRef := func(ref string) string {
		return `{"strategy":"priority","endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"` + ref + `","timeout_s":30,"priority":0}]}`
	}
	if _, err := Load(writeConfig(t, withRef("sk-abcdef1234567890"))); err == nil {
		t.Errorf("credential_ref=sk-… 期望报错，实际 nil（⛔ 不存明文）")
	}
	if _, err := Load(writeConfig(t, withRef("OPENAI_API_KEY"))); err != nil {
		t.Errorf("环境变量名引用应通过，got err: %v", err)
	}
}

// TestLoadRejectsInvalidStrategy 断言 strategy 缺失 / 未知 ⇒ 报错（统一 SDD 9.2）。
func TestLoadRejectsInvalidStrategy(t *testing.T) {
	for _, raw := range []string{
		`{"endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}]}`,
		`{"strategy":"random","endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}]}`,
	} {
		if _, err := Load(writeConfig(t, raw)); err == nil {
			t.Errorf("strategy 缺失/未知 期望报错，实际 nil")
		}
	}
}

// TestLoadRejectsBadFallbackChains 断言 fallback_chains 坏引用 / 未知键 / 空数组 ⇒ 报错
// （统一 SDD 7.4 链须显式有序且引用真实端点；实现计划偏差 4）。
// 对照点：不做引用校验 ⇒ 坏引用子例红；不做键校验 ⇒ 未知键子例红。
func TestLoadRejectsBadFallbackChains(t *testing.T) {
	endpoint := `{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}`
	withChains := func(chains string) string {
		return `{"strategy":"priority","endpoints":[` + endpoint + `],"fallback_chains":` + chains + `}`
	}
	cases := []struct{ name, raw string }{
		{"引用不存在端点", withChains(`{"general":["ghost"]}`)},
		{"未知失败类型键", withChains(`{"fallback":["a"]}`)},
		{"空数组", withChains(`{"general":[]}`)},
		{"空端点名", withChains(`{"general":["a",""]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, tc.raw)); err == nil {
				t.Fatalf("期望 fallback_chains 报错，实际 nil")
			}
		})
	}
	t.Run("合法链通过并可取用", func(t *testing.T) {
		r, err := Load(writeConfig(t, withChains(`{"context_window":["a"],"general":["a"]}`)))
		if err != nil {
			t.Fatalf("合法链报错: %v", err)
		}
		if got := r.FallbackChains[routing.FailContextWindow]; !reflect.DeepEqual(got, []string{"a"}) {
			t.Errorf("FallbackChains[context_window] = %v, want [a]", got)
		}
	})
}

// TestLoadRejectsDuplicateEndpointName 断言端点名全局唯一（统一 SDD 9.1）。
func TestLoadRejectsDuplicateEndpointName(t *testing.T) {
	ep := `{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}`
	raw := `{"strategy":"priority","endpoints":[` + ep + `,` + ep + `]}`
	if _, err := Load(writeConfig(t, raw)); err == nil {
		t.Fatalf("重复端点名期望报错，实际 nil")
	}
}

// TestLoadRejectsBadFieldValues 断言数值 / URL 边界非法 ⇒ 报错（不许拿非法值绕过判定）。
func TestLoadRejectsBadFieldValues(t *testing.T) {
	mutate := func(field, value string) string {
		return `{"strategy":"priority","endpoints":[{"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"m","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0,` +
			`"` + field + `":` + value + `}]}`
	}
	cases := []struct{ name, raw string }{
		{"context_window 非正", mutate("context_window", "0")},
		{"max_output_tokens 为负", mutate("max_output_tokens", "-1")},
		{"timeout_s 非正", mutate("timeout_s", "0")},
		{"base_url 无 scheme", mutate("base_url", `"api.openai.com"`)},
		{"endpoints 为空", `{"strategy":"priority","endpoints":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, tc.raw)); err == nil {
				t.Fatalf("期望自检报错，实际 nil")
			}
		})
	}
}

// TestLoadValidConfig 断言合法配置加载成功且逐字段落位（两层模型语义、能力标记、优先级）。
func TestLoadValidConfig(t *testing.T) {
	const raw = `{
  "strategy": "round_robin",
  "endpoints": [
    {"name":"backup","base_url":"https://api.deepseek.com/v1","model":"gpt-4o-mini","upstream_model":"deepseek-chat","provider_name":"deepseek","context_window":64000,"max_output_tokens":8192,"capabilities":{"supports_tools":true,"supports_stream":true,"supports_vision":false},"credential_ref":"DEEPSEEK_API_KEY","timeout_s":45,"priority":1},
    {"name":"primary","base_url":"https://api.openai.com/v1","model":"gpt-4o-mini","upstream_model":"gpt-4o-mini","provider_name":"openai","context_window":128000,"max_output_tokens":16384,"capabilities":{"supports_tools":true,"supports_stream":true,"supports_vision":true},"credential_ref":"OPENAI_API_KEY","timeout_s":30,"priority":0}
  ],
  "fallback_chains": {"general":["backup"],"rate_limit":["backup"]}
}`
	r, err := Load(writeConfig(t, raw))
	if err != nil {
		t.Fatalf("合法配置报错: %v", err)
	}
	if r.Strategy != StrategyRoundRobin {
		t.Errorf("Strategy = %q, want round_robin", r.Strategy)
	}
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("List 长度 = %d, want 2", len(list))
	}
	// priority 数值越小越优先 ⇒ primary（priority=0）排前。
	if list[0].Name != "primary" || list[1].Name != "backup" {
		t.Fatalf("List 顺序 = [%s %s], want [primary backup]", list[0].Name, list[1].Name)
	}
	p := list[0]
	if p.UpstreamModel != "gpt-4o-mini" || p.ProviderName != "openai" || p.Model != "gpt-4o-mini" {
		t.Errorf("两层模型语义 / provider 落位不符: %+v", p)
	}
	if p.MaxOutputTokens != 16384 || p.ContextWindow != 128000 || p.TimeoutS != 30 {
		t.Errorf("数值字段落位不符: %+v", p)
	}
	if p.Capabilities == nil || !p.Capabilities.SupportsTools || !p.Capabilities.SupportsStream || !p.Capabilities.SupportsVision {
		t.Errorf("capabilities 落位不符: %+v", p.Capabilities)
	}
	b := list[1]
	if b.Capabilities == nil || b.Capabilities.SupportsVision {
		t.Errorf("backup capabilities 落位不符: %+v", b.Capabilities)
	}
	if got := r.FallbackChains[routing.FailGeneral]; !reflect.DeepEqual(got, []string{"backup"}) {
		t.Errorf("FallbackChains[general] = %v, want [backup]", got)
	}
}

// TestLookupPriority 断言模型组内按 strategy 调度：priority 策略取 Priority 最小者，
// 同值按 Name 升序（显式排序、可复现，统一 SDD 1.4.3）。
func TestLookupPriority(t *testing.T) {
	const raw = `{
  "strategy": "priority",
  "endpoints": [
    {"name":"beta","base_url":"https://api.openai.com/v1","model":"gpt-4o-mini","upstream_model":"m2","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"K1","timeout_s":30,"priority":2},
    {"name":"alpha","base_url":"https://api.openai.com/v1","model":"gpt-4o-mini","upstream_model":"m3","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"K2","timeout_s":30,"priority":2},
    {"name":"primary","base_url":"https://api.openai.com/v1","model":"gpt-4o-mini","upstream_model":"m1","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"K3","timeout_s":30,"priority":0}
  ]
}`
	r, err := Load(writeConfig(t, raw))
	if err != nil {
		t.Fatalf("合法配置报错: %v", err)
	}
	ep, err := r.Lookup("gpt-4o-mini")
	if err != nil {
		t.Fatalf("Lookup 报错: %v", err)
	}
	if ep.Name != "primary" {
		t.Errorf("Lookup = %q, want primary（priority 最小）", ep.Name)
	}
	if ep.UpstreamModel != "m1" {
		t.Errorf("UpstreamModel = %q, want m1（按别名命中、按 upstream_model 发出）", ep.UpstreamModel)
	}
	// 同 priority 时 Name 升序：alpha 先于 beta。
	t.Run("同 priority 按 Name 升序", func(t *testing.T) {
		raw2 := strings.Replace(raw, `"priority":0`, `"priority":2`, 1)
		r2, err := Load(writeConfig(t, raw2))
		if err != nil {
			t.Fatalf("合法配置报错: %v", err)
		}
		ep2, err := r2.Lookup("gpt-4o-mini")
		if err != nil {
			t.Fatalf("Lookup 报错: %v", err)
		}
		if ep2.Name != "alpha" {
			t.Errorf("Lookup = %q, want alpha（同 priority 按 Name 升序）", ep2.Name)
		}
	})
}

// TestLookupModelNotFound 断言列表外模型 ⇒ 类型化错误（上层映射 404 model_not_found，
// 统一 SDD 9.1：⛔ 不透传任意模型名）。
// 对照点：实现透传任意模型名返回 nil 错误 ⇒ 红。
func TestLookupModelNotFound(t *testing.T) {
	r, err := Load(writeConfig(t, validOne))
	if err != nil {
		t.Fatalf("合法配置报错: %v", err)
	}
	_, err = r.Lookup("gpt-9-turbo")
	if err == nil {
		t.Fatalf("列表外模型期望报错，实际 nil")
	}
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v, want errors.Is ErrModelNotFound", err)
	}
	var mnf *ModelNotFoundError
	if !errors.As(err, &mnf) {
		t.Errorf("err = %v, want *ModelNotFoundError", err)
	} else if mnf.Model != "gpt-9-turbo" {
		t.Errorf("Model = %q, want gpt-9-turbo", mnf.Model)
	}
	if !strings.Contains(err.Error(), "model_not_found") {
		t.Errorf("消息 %q 须含 model_not_found（上层映射 404 的错误码）", err.Error())
	}
}

// TestLookupRoundRobin 断言 round_robin 策略在模型组内显式计数轮转（确定性序列，
// ⛔ 不掺随机 / 时间）。
func TestLookupRoundRobin(t *testing.T) {
	const raw = `{
  "strategy": "round_robin",
  "endpoints": [
    {"name":"a","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"ma","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"K1","timeout_s":30,"priority":0},
    {"name":"b","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"mb","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"K2","timeout_s":30,"priority":1},
    {"name":"c","base_url":"https://api.openai.com/v1","model":"m","upstream_model":"mc","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"K3","timeout_s":30,"priority":2}
  ]
}`
	r, err := Load(writeConfig(t, raw))
	if err != nil {
		t.Fatalf("合法配置报错: %v", err)
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i, w := range want {
		ep, err := r.Lookup("m")
		if err != nil {
			t.Fatalf("第 %d 次 Lookup 报错: %v", i, err)
		}
		if ep.Name != w {
			t.Fatalf("第 %d 次 Lookup = %q, want %q", i, ep.Name, w)
		}
	}
}

// TestListDeterministic 断言 List 确定性排序：跑 100 次全等（统一 SDD 11.3 可复现口径）。
// 对照点：实现按 map 遍历序返回 ⇒ 100 次里出现不一致，DeepEqual 红。
func TestListDeterministic(t *testing.T) {
	const raw = `{
  "strategy": "priority",
  "endpoints": [
    {"name":"c","base_url":"https://api.openai.com/v1","model":"mc","upstream_model":"mc","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"K1","timeout_s":30,"priority":1},
    {"name":"a","base_url":"https://api.openai.com/v1","model":"ma","upstream_model":"ma","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"K2","timeout_s":30,"priority":0},
    {"name":"b","base_url":"https://api.openai.com/v1","model":"mb","upstream_model":"mb","provider_name":"openai","context_window":1000,"max_output_tokens":100,"capabilities":{},"credential_ref":"K3","timeout_s":30,"priority":1}
  ]
}`
	r, err := Load(writeConfig(t, raw))
	if err != nil {
		t.Fatalf("合法配置报错: %v", err)
	}
	first := r.List()
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(names(first), want) {
		t.Fatalf("List 顺序 = %v, want %v（Priority 升序、同值 Name 升序）", names(first), want)
	}
	for i := 0; i < 100; i++ {
		if got := r.List(); !reflect.DeepEqual(got, first) {
			t.Fatalf("第 %d 次 List 与首次不一致", i)
		}
	}
}

// TestLoadExampleConfig 断言 config/gateway.example.json 能被 Load 成功加载
// （统一 SDD 9.2「对照 config/gateway.example.json 的实况」；本地端点走环回地址）。
func TestLoadExampleConfig(t *testing.T) {
	r, err := Load(filepath.Join("..", "..", "config", "gateway.example.json"))
	if err != nil {
		t.Fatalf("示例配置加载失败: %v", err)
	}
	list := r.List()
	if len(list) != 4 {
		t.Fatalf("示例配置端点数 = %d, want 4", len(list))
	}
	for _, ep := range list {
		if ep.UpstreamModel == "" || ep.Capabilities == nil || ep.CredentialRef == "" ||
			ep.ProviderName == "" || ep.MaxOutputTokens == 0 {
			t.Errorf("端点 %q 未带 9.2 完整字段: %+v", ep.Name, ep)
		}
	}
	var local *Endpoint
	for i := range list {
		if list[i].Name == "local" {
			local = &list[i]
		}
	}
	if local == nil {
		t.Fatalf("示例配置缺 local 端点")
	}
	if !strings.Contains(local.BaseURL, "127.0.0.1") {
		t.Errorf("本地端点 base_url = %q, want 含 127.0.0.1（环回地址，统一 SDD 9.2 出网一致性）", local.BaseURL)
	}
	ep, err := r.Lookup("gpt-6-luna")
	if err != nil {
		t.Fatalf("Lookup(gpt-6-luna) 报错: %v", err)
	}
	if ep.Name != "primary" {
		t.Errorf("Lookup(gpt-6-luna) = %q, want primary（priority 最小）", ep.Name)
	}
	if got := r.FallbackChains[routing.FailContextWindow]; !reflect.DeepEqual(got, []string{"large-context"}) {
		t.Errorf("FallbackChains[context_window] = %v, want [large-context]", got)
	}
}

// TestLoadGatewayConfig 断言 config/gateway.json（2026-11-17 全 key 实测 7/7 PASS 的注册表）
// 能被 Load 成功加载，且每个端点「端点名 = 模型名 = 上游模型名」三者逐字相同 ——
// 正式配置真名直出：不设别名（model != upstream_model 即别名），端点名也不另起；
// 别名与独立端点名只在 gateway.example.json 里演示（2026-11-21 口径）。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/registry/ -run TestLoadGatewayConfig -v
func TestLoadGatewayConfig(t *testing.T) {
	r, err := Load(filepath.Join("..", "..", "config", "gateway.json"))
	if err != nil {
		t.Fatalf("正式配置加载失败: %v", err)
	}
	list := r.List()
	if len(list) != 7 {
		t.Fatalf("正式配置端点数 = %d, want 7", len(list))
	}
	for _, ep := range list {
		if ep.Name != ep.Model {
			t.Errorf("端点名与模型名不同: name = %q, model = %q（端点名须与模型名相同）", ep.Name, ep.Model)
		}
		if ep.Model != ep.UpstreamModel {
			t.Errorf("端点 %q 用别名: model = %q, upstream_model = %q（正式配置要求同值）", ep.Name, ep.Model, ep.UpstreamModel)
		}
		got, err := r.Lookup(ep.Model)
		if err != nil {
			t.Errorf("Lookup(%q) 报错: %v", ep.Model, err)
			continue
		}
		if got.Name != ep.Name {
			t.Errorf("Lookup(%q) = 端点 %q, want %q", ep.Model, got.Name, ep.Name)
		}
	}
}

// names 取端点名列表（测试辅助）。
func names(eps []Endpoint) []string {
	out := make([]string, len(eps))
	for i, ep := range eps {
		out[i] = ep.Name
	}
	return out
}

// epJSON 是带配额声明的端点 JSON 片段模板（%s 依次：name / rate_limit 行 / 并发行 / 组行）。
func epJSON(name, extra string) string {
	return `{
      "name": "` + name + `",
      "base_url": "https://api.openai.com/v1",
      "model": "m-` + name + `",
      "upstream_model": "gpt-6-luna",
      "provider_name": "openai",
      "context_window": 128000,
      "max_output_tokens": 4096,
      "capabilities": {"supports_tools": true, "supports_stream": true, "supports_vision": false},
      "credential_ref": "OPENAI_API_KEY",
      "timeout_s": 30,
      "priority": 0` + extra + `
    }`
}

// TestLoadRateLimitAndConcurrency 断言 rate_limit / concurrency_limit / limits_group 正确解析。
func TestLoadRateLimitAndConcurrency(t *testing.T) {
	raw := `{"strategy": "priority", "endpoints": [` + epJSON("a",
		`, "rate_limit": {"tpm": 100000, "rpm": 600, "burst_s": 5},
      "concurrency_limit": 50, "limits_group": "openai-pool"`) + `]}`
	reg, err := Load(writeConfig(t, raw))
	if err != nil {
		t.Fatalf("合法配额声明不应报错: %v", err)
	}
	ep := reg.List()[0]
	if ep.RateLimit == nil || ep.RateLimit.TPM != 100000 || ep.RateLimit.RPM != 600 || ep.RateLimit.BurstS != 5 {
		t.Fatalf("rate_limit 解析失真: %+v", ep.RateLimit)
	}
	if ep.ConcurrencyLimit != 50 || ep.LimitsGroup != "openai-pool" {
		t.Fatalf("concurrency_limit/limits_group 解析失真: %d / %q", ep.ConcurrencyLimit, ep.LimitsGroup)
	}
}

// TestLoadRejectsBadRateLimit 断言非法配额形状被拒（burst_s 缺失、两维全 0、负值）。
func TestLoadRejectsBadRateLimit(t *testing.T) {
	for _, extra := range []string{
		`, "rate_limit": {"tpm": 600, "rpm": 0, "burst_s": 0}`,
		`, "rate_limit": {"tpm": 0, "rpm": 0, "burst_s": 5}`,
		`, "rate_limit": {"tpm": -1, "rpm": 0, "burst_s": 5}`,
	} {
		raw := `{"strategy": "priority", "endpoints": [` + epJSON("a", extra) + `]}`
		if _, err := Load(writeConfig(t, raw)); err == nil {
			t.Fatalf("非法 rate_limit 应报错：%s", extra)
		}
	}
}

// TestLoadRejectsNegativeConcurrency 断言 concurrency_limit 为负被拒。
func TestLoadRejectsNegativeConcurrency(t *testing.T) {
	raw := `{"strategy": "priority", "endpoints": [` + epJSON("a", `, "concurrency_limit": -1`) + `]}`
	if _, err := Load(writeConfig(t, raw)); err == nil {
		t.Fatal("负 concurrency_limit 应报错")
	}
}

// TestLoadLimitsGroupConsistency 断言同组声明逐字一致才放行（共享一份计数的组里
// 出现两份不同声明 = 超发/欠发的隐性错误 ⇒ 启动报错）。
func TestLoadLimitsGroupConsistency(t *testing.T) {
	okRaw := `{"strategy": "priority", "endpoints": [` +
		epJSON("a", `, "rate_limit": {"tpm": 600, "rpm": 0, "burst_s": 5}, "concurrency_limit": 8, "limits_group": "pool"`) + `,` +
		epJSON("b", `, "rate_limit": {"tpm": 600, "rpm": 0, "burst_s": 5}, "concurrency_limit": 8, "limits_group": "pool"`) + `]}`
	if _, err := Load(writeConfig(t, okRaw)); err != nil {
		t.Fatalf("同组一致声明不应报错: %v", err)
	}
	badRaw := `{"strategy": "priority", "endpoints": [` +
		epJSON("a", `, "rate_limit": {"tpm": 600, "rpm": 0, "burst_s": 5}, "limits_group": "pool"`) + `,` +
		epJSON("b", `, "rate_limit": {"tpm": 1200, "rpm": 0, "burst_s": 5}, "limits_group": "pool"`) + `]}`
	if _, err := Load(writeConfig(t, badRaw)); err == nil {
		t.Fatal("同组不同声明应报错")
	}
}

// TestLoadCallers 断言 callers 合同面（registry callers 单源，R-43 / F-36）解析与校验：
// 哈希存储（key_hash 形状不合法 = 明文混入 ⇒ 报错）、必有过期（缺失 / 已过期 ⇒ 报错）、
// scope 显式（模型集 / 端点集非空且引用存在）、轮换双 key 上限、配额形状与端点同口径。
func TestLoadCallers(t *testing.T) {
	const hashA = "1111111111111111111111111111111111111111111111111111111111111111"
	const hashB = "2222222222222222222222222222222222222222222222222222222222222222"
	// hashC 含字母（数字无大小写，纯数字哈希验不出大小写形态拒）。
	const hashC = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	key := func(hash, expires string) string {
		return `{"key_hash": "` + hash + `", "expires_at": "` + expires + `"}`
	}
	caller := func(name, keys, models, endpoints string, extra string) string {
		return `{"name": "` + name + `", "keys": [` + keys + `],
      "scope": {"models": [` + models + `], "endpoints": [` + endpoints + `]}` + extra + `}`
	}
	base := func(callers string) string {
		return `{"strategy": "priority", "endpoints": [` + epJSON("a", "") + `], "callers": [` + callers + `]}`
	}

	// 合法：单 key + scope + 契约配额。
	okRaw := base(caller("harness", key(hashA, future), `"m-a"`, `"a"`,
		`, "rate_limit": {"tpm": 60000, "rpm": 600, "burst_s": 5}`))
	reg, err := Load(writeConfig(t, okRaw))
	if err != nil {
		t.Fatalf("合法 callers 不应报错: %v", err)
	}
	if len(reg.Callers) != 1 || reg.Callers[0].Name != "harness" ||
		len(reg.Callers[0].Keys) != 1 || reg.Callers[0].Keys[0].KeyHash != hashA ||
		reg.Callers[0].Scope.TokenBudget != 0 || reg.Callers[0].RateLimit == nil {
		t.Fatalf("callers 解析失真: %+v", reg.Callers)
	}

	// 合法：轮换窗口双 key（新旧并存）。
	okDual := base(caller("harness", key(hashA, future)+","+key(hashB, future), `"m-a"`, `"a"`, ``))
	if _, err := Load(writeConfig(t, okDual)); err != nil {
		t.Fatalf("轮换双 key 不应报错: %v", err)
	}

	bads := map[string]string{
		"key_hash 明文形态（非 64 位小写 hex）": base(caller("harness", key("ck-plaintext-key", future), `"m-a"`, `"a"`, ``)),
		"key_hash 大写 hex":             base(caller("harness", key(strings.ToUpper(hashC), future), `"m-a"`, `"a"`, ``)),
		"expires_at 缺失（永不过期，F-36）":    base(caller("harness", `{"key_hash": "`+hashA+`"}`, `"m-a"`, `"a"`, ``)),
		"expires_at 已过期":              base(caller("harness", key(hashA, past), `"m-a"`, `"a"`, ``)),
		"keys 为空":                     base(caller("harness", ``, `"m-a"`, `"a"`, ``)),
		"keys 超过 2（⛔ 堆历史 key）":        base(caller("harness", key(hashA, future)+","+key(hashB, future)+","+key(strings.Repeat("3", 64), future), `"m-a"`, `"a"`, ``)),
		"scope.models 为空":             base(caller("harness", key(hashA, future), ``, `"a"`, ``)),
		"scope.endpoints 为空":          base(caller("harness", key(hashA, future), `"m-a"`, ``, ``)),
		"scope.models 引用不存在的模型":       base(caller("harness", key(hashA, future), `"nope"`, `"a"`, ``)),
		"scope.endpoints 引用不存在的端点":    base(caller("harness", key(hashA, future), `"m-a"`, `"nope"`, ``)),
		"token_budget 为负": `{"strategy": "priority", "endpoints": [` + epJSON("a", "") + `],
      "callers": [{"name": "harness", "keys": [` + key(hashA, future) + `],
      "scope": {"models": ["m-a"], "endpoints": ["a"], "token_budget": -1}}]}`,
		"name 缺失": base(caller("", key(hashA, future), `"m-a"`, `"a"`, ``)),
		"rate_limit 形状非法（两维全 0）": base(caller("harness", key(hashA, future), `"m-a"`, `"a"`, `, "rate_limit": {"tpm": 0, "rpm": 0, "burst_s": 5}`)),
	}
	for name, raw := range bads {
		if _, err := Load(writeConfig(t, raw)); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}

	// 重复 name / 重复 key_hash 全局唯一。
	dupName := base(caller("harness", key(hashA, future), `"m-a"`, `"a"`, ``) + "," +
		caller("harness", key(hashB, future), `"m-a"`, `"a"`, ``))
	if _, err := Load(writeConfig(t, dupName)); err == nil {
		t.Error("重复 name 应报错")
	}
	dupHash := base(caller("harness", key(hashA, future), `"m-a"`, `"a"`, ``) + "," +
		caller("other", key(hashA, future), `"m-a"`, `"a"`, ``))
	if _, err := Load(writeConfig(t, dupHash)); err == nil {
		t.Error("重复 key_hash 应报错")
	}

	// 反向锁：合法双 key + 预算 + 无配额（rate_limit 省略 = 不限流）也必须放行。
	okFull := `{"strategy": "priority", "endpoints": [` + epJSON("a", "") + `],
      "callers": [{"name": "harness", "keys": [` + key(hashA, future) + `,` + key(hashB, future) + `],
      "scope": {"models": ["m-a"], "endpoints": ["a"], "token_budget": 5000}}]}`
	if _, err := Load(writeConfig(t, okFull)); err != nil {
		t.Fatalf("合法双 key + 预算登记不应报错: %v", err)
	}
}

// TestLoadCredentialRateLimits 断言上游令牌池契约配额解析与校验（多 key：
// per_key / shared / key_limits 三层至少一层；形状与 rate_limit 同口径）。
func TestLoadCredentialRateLimits(t *testing.T) {
	okRaw := `{"strategy": "priority", "endpoints": [` + epJSON("a", "") + `],
      "credential_rate_limits": [{"key_ref": "OPENAI_API_KEY",
        "per_key": {"tpm": 1000000, "rpm": 10000, "burst_s": 10},
        "shared": {"tpm": 4000000, "rpm": 40000, "burst_s": 10},
        "key_limits": [{"tpm": 500000, "rpm": 5000, "burst_s": 10}, null]}]}`
	reg, err := Load(writeConfig(t, okRaw))
	if err != nil {
		t.Fatalf("合法 credential_rate_limits 不应报错: %v", err)
	}
	crl := reg.CredentialRateLimits[0]
	if crl.KeyRef != "OPENAI_API_KEY" || crl.PerKey == nil || crl.Shared == nil || len(crl.KeyLimits) != 2 {
		t.Fatalf("解析失真: %+v", crl)
	}
	for _, bad := range []string{
		`{"per_key": {"tpm": 600, "rpm": 0, "burst_s": 1}}`, // 缺 key_ref
		`{"key_ref": "K"}`, // 三层全空
		`{"key_ref": "K", "per_key": {"tpm": 0, "rpm": 0, "burst_s": 1}}`, // 形状非法
		`{"key_ref": "K", "key_limits": [null]}`,                          // null 项
	} {
		raw := `{"strategy": "priority", "endpoints": [` + epJSON("a", "") + `],
        "credential_rate_limits": [` + bad + `]}`
		if _, err := Load(writeConfig(t, raw)); err == nil {
			t.Fatalf("非法 credential_rate_limits 应报错：%s", bad)
		}
	}
}
