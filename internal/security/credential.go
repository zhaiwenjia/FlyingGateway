// 日期: 2026-11-14 11:50:04.381
// 作者: Zhai Wenjia

package security

import (
	"fmt"
	"os"
	"regexp"
)

// ResolveCredential 按来源引用解析凭据（统一 SDD 8.1 / 8.4）。
// 当前只支持「环境变量名」一种引用形态：
//   - ref 必须是合法环境变量名（[A-Za-z_][A-Za-z0-9_]*）；
//   - 其他形态（密钥别名如 vault:// / keyring: / secret: 等）⇒ 显式错误，
//     ⛔ 不假装支持、⛔ 不猜语义（密钥管理服务接入另立规格）；
//   - 环境变量未设置或值为空 ⇒ 显式错误，⛔ 不返回空串当有效凭据。
//
// 凭据值只在出站请求构造处解析注入（8.4），⛔ 不在函数间层层传递明文（8.1）。
func ResolveCredential(ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("security: credential_ref 为空")
	}
	if !envNamePattern.MatchString(ref) {
		return "", fmt.Errorf("security: credential_ref %q 不是环境变量名形态（仅支持环境变量引用，密钥别名形态未实现）", ref)
	}
	v, ok := os.LookupEnv(ref)
	if !ok {
		return "", fmt.Errorf("security: 环境变量 %s 未设置，无法解析凭据", ref)
	}
	if v == "" {
		return "", fmt.Errorf("security: 环境变量 %s 已设置但值为空，拒绝作凭据使用", ref)
	}
	return v, nil
}

// envNamePattern 是 POSIX 环境变量名形态。
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
