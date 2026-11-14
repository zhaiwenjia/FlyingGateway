// 日期: 2026-11-14 11:50:04.536
// 作者: Zhai Wenjia

package security

import (
	"strings"
	"testing"
)

// TestResolveCredential_FromEnv 断言凭据只从环境变量读（统一 SDD 8.1）。
// 断言有效性：若实现从别处（文件 / 硬编码）取值，返回值断言会红。
func TestResolveCredential_FromEnv(t *testing.T) {
	t.Setenv("FG_TEST_CRED_OK", "sk-real-112233445566")
	got, err := ResolveCredential("FG_TEST_CRED_OK")
	if err != nil {
		t.Fatalf("ResolveCredential 报错：%v", err)
	}
	if got != "sk-real-112233445566" {
		t.Fatalf("凭据值 = %q, 期望环境变量原值", got)
	}
}

// TestResolveCredential_Errors 断言缺失 / 空值 / 非法名 / 密钥别名形态都显式报错
// （⛔ 不假装支持密钥别名）。
// 断言有效性：任何一项「静默返回空串 / 猜语义」的实现都会红。
func TestResolveCredential_Errors(t *testing.T) {
	cases := []struct {
		name string
		ref  string
	}{
		{"未设置的环境变量", "FG_TEST_CRED_UNSET_NEVER"},
		{"空 ref", ""},
		{"非法环境变量名", "FG-BAD-NAME"},
		{"密钥别名形态 vault", "vault://secret/fg-key"},
		{"密钥别名形态 keyring", "keyring:fg/key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := ResolveCredential(c.ref)
			if err == nil {
				t.Fatalf("ResolveCredential(%q) 未报错且返回 %q，期望显式错误", c.ref, v)
			}
			if v != "" {
				t.Fatalf("报错同时返回了值 %q", v)
			}
		})
	}

	// 已设置但值为空 ⇒ 显式错误。
	t.Setenv("FG_TEST_CRED_EMPTY", "")
	if v, err := ResolveCredential("FG_TEST_CRED_EMPTY"); err == nil {
		t.Fatalf("空值凭据未报错，返回 %q", v)
	} else if !strings.Contains(err.Error(), "FG_TEST_CRED_EMPTY") {
		t.Fatalf("错误信息未点名环境变量：%v", err)
	}
}
