// 日期: 2026-11-14 11:50:04.434
// 作者: Zhai Wenjia

package security

import (
	"fmt"
	"net"
	"net/url"
)

// EgressError 是出网校验失败错误（统一 SDD 8.2）。
// Code 固定 egress_denied，上层映射 403 egress_denied；
// ⛔ 校验不通过只显式报错，不静默改投其他端点。
type EgressError struct {
	URL    string
	Reason string
}

func (e *EgressError) Error() string {
	return fmt.Sprintf("security: egress_denied: %s: %s", e.URL, e.Reason)
}

// deniedIPv4 是默认拒绝的私网 / 链路本地段（统一 SDD 8.2 表）。
var deniedIPv4 = []net.IPNet{
	mustCIDR("10.0.0.0/8"),
	mustCIDR("172.16.0.0/12"),
	mustCIDR("192.168.0.0/16"),
	mustCIDR("169.254.0.0/16"),
}

// loopbackIPv4 是可配置放行的环回段（127.0.0.0/8，不限定端口）。
var loopbackIPv4 = mustCIDR("127.0.0.0/8")

// lookupIP 是 DNS 解析注入点（防 DNS rebinding 测试用，默认 net.LookupIP）。
var lookupIP = net.LookupIP

// ValidateEgressURL 是出网 URL 的单一控制点（统一 SDD 8.2）：
//   - 协议白名单：仅 https；http 仅当 allowLoopback=true 且目标是环回地址时放行
//     （本地 Ollama 等，不限定端口）；
//   - 私网拒绝：10.0.0.0/8 / 172.16.0.0/12 / 192.168.0.0/16 / 169.254.0.0/16；
//   - 环回：127.0.0.0/8 与 ::1 按 allowLoopback 放行；
//   - ⭐ DNS 解析后校验：域名校验完必须把解析出的 IP 再过一遍私网校验，
//     防 DNS rebinding（首解析公网、重绑定私网）；
//   - 失败 ⇒ 返回 *EgressError（Code=egress_denied），⛔ 不静默改投。
func ValidateEgressURL(rawURL string, allowLoopback bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return &EgressError{URL: rawURL, Reason: "URL 解析失败: " + err.Error()}
	}
	switch u.Scheme {
	case "https":
		// 加密端点，继续校验地址。
	case "http":
		// http 仅环回放行时允许，且目标必须真是环回地址（下面统一判定）。
		if !allowLoopback {
			return &EgressError{URL: rawURL, Reason: "协议白名单只允许 https（http 仅环回放行时可用）"}
		}
	default:
		return &EgressError{URL: rawURL, Reason: "协议白名单只允许 https（http 仅环回放行时可用）"}
	}
	host := u.Hostname()
	if host == "" {
		return &EgressError{URL: rawURL, Reason: "URL 无主机名"}
	}

	// 先取要校验的 IP 列表：IP 字面量直接用；域名必须 DNS 解析后逐 IP 校验。
	ips := []net.IP{}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else {
		resolved, lerr := lookupIP(host)
		if lerr != nil {
			return &EgressError{URL: rawURL, Reason: "DNS 解析失败: " + lerr.Error()}
		}
		if len(resolved) == 0 {
			return &EgressError{URL: rawURL, Reason: "DNS 解析结果为空"}
		}
		ips = resolved
	}

	// http 只对环回放行 ⇒ 逐 IP 全部是环回才继续，否则拒。
	if u.Scheme == "http" {
		for _, ip := range ips {
			if !isLoopback(ip) {
				return &EgressError{URL: rawURL, Reason: "http 仅允许环回地址（本地推理服务）"}
			}
		}
	}

	// ⭐ 解析后 IP 也要过私网校验（防 DNS rebinding）：任一 IP 不通过即整体拒绝。
	for _, ip := range ips {
		if err := validateIP(ip, allowLoopback); err != nil {
			return &EgressError{URL: rawURL, Reason: err.Error()}
		}
	}
	return nil
}

// validateIP 对单个 IP 过私网 / 环回校验。
func validateIP(ip net.IP, allowLoopback bool) error {
	if isLoopback(ip) {
		if allowLoopback {
			return nil
		}
		return fmt.Errorf("环回地址 %s 未放行（allowLoopback=false）", ip)
	}
	for i := range deniedIPv4 {
		if deniedIPv4[i].Contains(ip) {
			return fmt.Errorf("私网 / 链路本地地址 %s 被拒绝", ip)
		}
	}
	return nil
}

func isLoopback(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	return ip.To4() != nil && loopbackIPv4.Contains(ip)
}

func mustCIDR(s string) net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return *n
}
