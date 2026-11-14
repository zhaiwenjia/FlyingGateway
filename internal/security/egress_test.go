// 日期: 2026-11-14 11:50:04.585
// 作者: Zhai Wenjia

package security

import (
	stderrors "errors"
	"net"
	"strings"
	"testing"
)

// withFakeDNS 注入 DNS 解析结果（防 DNS rebinding 测试用），测试后恢复。
func withFakeDNS(t *testing.T, m map[string][]net.IP) {
	t.Helper()
	old := lookupIP
	lookupIP = func(host string) ([]net.IP, error) {
		if ips, ok := m[host]; ok {
			return ips, nil
		}
		return nil, stderrors.New("no such host")
	}
	t.Cleanup(func() { lookupIP = old })
}

// TestValidateEgressURL_PrivateDenied 断言私网地址一律拒绝
// （统一 SDD 11.3「私网拒绝」，10.0.0.1:8000 ⇒ 403 egress_denied）。
// 断言有效性：私网放行的错误实现任一行会红；错误码非 egress_denied 也会红。
func TestValidateEgressURL_PrivateDenied(t *testing.T) {
	urls := []string{
		"https://10.0.0.1:8000/v1",
		"http://10.0.0.1:8000/v1",
		"https://172.16.0.1/",
		"https://192.168.1.10:8443/",
		"https://169.254.169.254/latest/meta-data/", // 云元数据 SSRF 经典目标
	}
	for _, u := range urls {
		err := ValidateEgressURL(u, true) // allowLoopback=true 也救不了私网
		if err == nil {
			t.Fatalf("%s 未被拒绝，期望 egress_denied", u)
		}
		var ee *EgressError
		if !stderrors.As(err, &ee) {
			t.Fatalf("%s 返回 %v，期望 *EgressError", u, err)
		}
		if !strings.Contains(ee.Error(), "egress_denied") {
			t.Fatalf("%s 错误不含 egress_denied：%v", u, err)
		}
	}
}

// TestValidateEgressURL_Loopback 按 allowLoopback 放行 / 拒绝环回
// （统一 SDD 11.3：127.0.0.1:11434（Ollama）⇒ allowLoopback=true 放行 / false 拒）。
// 断言有效性：忽略 allowLoopback 的实现（恒放行或恒拒）会红。
func TestValidateEgressURL_Loopback(t *testing.T) {
	cases := []struct {
		url           string
		allowLoopback bool
		wantErr       bool
	}{
		{"http://127.0.0.1:11434/api/chat", true, false}, // 本地 Ollama，http 环回放行
		{"http://127.0.0.1:11434/api/chat", true, false},
		{"http://127.0.0.1:11434/api/chat", false, true}, // 环回未放行 ⇒ 拒
		{"https://127.0.0.1:11434/", true, false},
		{"https://127.0.0.1:11434/", false, true},
		{"https://[::1]:8443/", true, false}, // ::1 同按 allowLoopback
		{"https://[::1]:8443/", false, true},
	}
	for _, c := range cases {
		err := ValidateEgressURL(c.url, c.allowLoopback)
		if c.wantErr && err == nil {
			t.Fatalf("%s allowLoopback=%v 未被拒绝", c.url, c.allowLoopback)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("%s allowLoopback=%v 被拒：%v", c.url, c.allowLoopback, err)
		}
	}
}

// TestValidateEgressURL_SchemeWhitelist 断言协议白名单（仅 https，http 仅环回）。
// 断言有效性：放行 ftp:// 的实现会红。
func TestValidateEgressURL_SchemeWhitelist(t *testing.T) {
	withFakeDNS(t, map[string][]net.IP{"example.com": {net.ParseIP("93.184.216.34")}})
	for _, u := range []string{"ftp://example.com/x", "ws://example.com/x", "example.com/x"} {
		if err := ValidateEgressURL(u, true); err == nil {
			t.Fatalf("%s 未被拒绝，期望协议白名单拦截", u)
		}
	}
}

// TestValidateEgressURL_DNSRebinding 断言 DNS 解析后 IP 仍过私网校验
// （统一 SDD 8.2「DNS 解析后校验」防 DNS rebinding）。
// 断言有效性：只校验 URL 字面 host、不校验解析结果的实现会红
// （域名公网、解析结果私网 ⇒ 必须拒）。
func TestValidateEgressURL_DNSRebinding(t *testing.T) {
	withFakeDNS(t, map[string][]net.IP{
		// rebinding 域名：URL 看着是域名，解析结果是私网。
		"rebind.example.test": {net.ParseIP("10.0.0.1")},
		// 公网域名：解析结果公网 ⇒ 放行。
		"public.example.test": {net.ParseIP("93.184.216.34")},
		// 双 A 记录夹私网：任一解析结果私网 ⇒ 整体拒。
		"mixed.example.test": {net.ParseIP("93.184.216.34"), net.ParseIP("192.168.0.5")},
		// 解析失败。
		"nxdomain.example.test": nil,
	})

	if err := ValidateEgressURL("https://rebind.example.test/v1", true); err == nil {
		t.Fatal("解析结果为私网的域名未被拒绝（DNS rebinding 未设防）")
	} else if !strings.Contains(err.Error(), "egress_denied") {
		t.Fatalf("错误不含 egress_denied：%v", err)
	}
	if err := ValidateEgressURL("https://public.example.test/v1", true); err != nil {
		t.Fatalf("公网域名被误拒：%v", err)
	}
	if err := ValidateEgressURL("https://mixed.example.test/v1", true); err == nil {
		t.Fatal("双 A 记录夹私网未被拒绝")
	}
	if err := ValidateEgressURL("https://nxdomain.example.test/v1", true); err == nil {
		t.Fatal("DNS 解析失败未报错")
	}
}

// TestValidateEgressURL_LoopbackViaDNS 断言环回判定对「解析后 IP」同样生效
// （域名解析到 127.0.0.1 时按 allowLoopback 判定）。
// 断言有效性：只看字面 host 的实现会红。
func TestValidateEgressURL_LoopbackViaDNS(t *testing.T) {
	withFakeDNS(t, map[string][]net.IP{"local.example.test": {net.ParseIP("127.0.0.1")}})
	if err := ValidateEgressURL("https://local.example.test/", true); err != nil {
		t.Fatalf("allowLoopback=true 时解析到环回被拒：%v", err)
	}
	if err := ValidateEgressURL("https://local.example.test/", false); err == nil {
		t.Fatal("allowLoopback=false 时解析到环回未被拒")
	}
}
