// 日期: 2026-11-16 23:13:43.700
// 作者: Zhai Wenjia

package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/registry"
	"github.com/zhaiwenjia/FlyingGateway/internal/telemetry"
)

// writeRegistryFile 把端点规格写成注册表 JSON 到指定路径（热加载测试用）。
func writeRegistryFile(t *testing.T, path string, eps []epSpec, chains map[string][]string) {
	t.Helper()
	epsWire := make([]map[string]any, 0, len(eps))
	for _, e := range eps {
		epsWire = append(epsWire, e.toWire())
	}
	cfg := map[string]any{"strategy": "priority", "endpoints": epsWire}
	if chains != nil {
		cfg["fallback_chains"] = chains
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写注册表失败: %v", err)
	}
}

// reloadEnv 装配一台带热加载数据源的网关（注册表文件可反复覆写触发 Reload）。
func reloadEnv(t *testing.T, eps []epSpec) *Server {
	t.Helper()
	t.Setenv("TEST_KEY_MIMO", "sk-gw-mimo-0003")
	dir := t.TempDir()
	regPath := filepath.Join(dir, "registry.json")
	writeRegistryFile(t, regPath, eps, nil)
	reg, err := registry.Load(regPath)
	if err != nil {
		t.Fatalf("注册表加载失败: %v", err)
	}
	srv, err := New(Options{
		Registry:            reg,
		RegistryPath:        regPath,
		Tracer:              telemetry.NewTracer(&telemetry.InMemoryExporter{}),
		Metrics:             telemetry.NewMetrics(),
		AllowLoopbackEgress: true,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("server.New 失败: %v", err)
	}
	return srv
}

// TestReloadSwapsConfig 钉住「可热更字段生效」：改 timeout_s + fallback_chains 后
// Reload ⇒ OK，/admin/endpoints 视图与注册表快照即刻反映新值（⛔ 不需要重启）。
func TestReloadSwapsConfig(t *testing.T) {
	srv := reloadEnv(t, []epSpec{{
		name: "primary", base: "http://127.0.0.1:18001/v1", model: "m", upstreamModel: "mimo-v2.6-flash",
		provider: "xiaomi", credRef: "TEST_KEY_MIMO",
		ctxWindow: 128000, maxOut: 1024, priority: 0, timeoutS: 5,
	}})
	if got := srv.curReg().List()[0].TimeoutS; got != 5 {
		t.Fatalf("初始 timeout_s = %v，期望 5", got)
	}
	// 覆写：timeout_s 5 → 9、加 fallback_chains。
	writeRegistryFile(t, srv.regPath, []epSpec{{
		name: "primary", base: "http://127.0.0.1:18001/v1", model: "m", upstreamModel: "mimo-v2.6-flash",
		provider: "xiaomi", credRef: "TEST_KEY_MIMO",
		ctxWindow: 128000, maxOut: 1024, priority: 0, timeoutS: 9,
	}}, map[string][]string{"general": {"primary"}})
	out := srv.Reload()
	if !out.OK {
		t.Fatalf("Reload 应成功：%+v", out)
	}
	if got := srv.curReg().List()[0].TimeoutS; got != 9 {
		t.Fatalf("热更后 timeout_s = %v，期望 9", got)
	}
	if _, ok := srv.curReg().FallbackChains["general"]; !ok {
		t.Fatal("fallback_chains 应随热加载生效")
	}
}

// TestReloadKeepsOldOnBadConfig 坏文件 ⇒ 显式报错、旧表原样保留（⛔ 不半应用）。
func TestReloadKeepsOldOnBadConfig(t *testing.T) {
	srv := reloadEnv(t, []epSpec{{
		name: "primary", base: "http://127.0.0.1:18001/v1", model: "m", upstreamModel: "mimo-v2.6-flash",
		provider: "xiaomi", credRef: "TEST_KEY_MIMO",
		ctxWindow: 128000, maxOut: 1024, priority: 0, timeoutS: 5,
	}})
	if err := os.WriteFile(srv.regPath, []byte("{不是合法 JSON"), 0o600); err != nil {
		t.Fatalf("写坏文件失败: %v", err)
	}
	out := srv.Reload()
	if out.OK || out.Error == "" {
		t.Fatalf("坏文件应显式报错：%+v", out)
	}
	if got := srv.curReg().List()[0].TimeoutS; got != 5 {
		t.Fatalf("坏文件后旧表应保留（timeout_s = %v，期望 5）", got)
	}
}

// TestReloadRestartRequired 需重启变更（端点增删 / rate_limit / breaker 等）⇒
// 显式拒绝并列出变更点、表不变（⛔ 不静默半生效）。
func TestReloadRestartRequired(t *testing.T) {
	srv := reloadEnv(t, []epSpec{{
		name: "primary", base: "http://127.0.0.1:18001/v1", model: "m", upstreamModel: "mimo-v2.6-flash",
		provider: "xiaomi", credRef: "TEST_KEY_MIMO",
		ctxWindow: 128000, maxOut: 1024, priority: 0, timeoutS: 5,
	}})
	// 变更 1：加一个端点 + 给 primary 加 rate_limit。
	writeRegistryFile(t, srv.regPath, []epSpec{
		{
			name: "primary", base: "http://127.0.0.1:18001/v1", model: "m", upstreamModel: "mimo-v2.6-flash",
			provider: "xiaomi", credRef: "TEST_KEY_MIMO",
			ctxWindow: 128000, maxOut: 1024, priority: 0, timeoutS: 5,
			rateLimit: map[string]any{"tpm": 1000, "rpm": 10, "burst_s": 5},
		},
		{
			name: "backup", base: "http://127.0.0.1:18002/v1", model: "m", upstreamModel: "mimo-v2.6-flash",
			provider: "xiaomi", credRef: "TEST_KEY_MIMO",
			ctxWindow: 128000, maxOut: 1024, priority: 1, timeoutS: 5,
		},
	}, nil)
	out := srv.Reload()
	if out.OK || len(out.RestartRequired) == 0 {
		t.Fatalf("端点增删 + rate_limit 变更应判需重启：%+v", out)
	}
	joined := ""
	for _, s := range out.RestartRequired {
		joined += s + ";"
	}
	for _, want := range []string{"新增端点 backup", "rate_limit"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("restart_required 缺 %q：%v", want, out.RestartRequired)
		}
	}
	if n := len(srv.curReg().List()); n != 1 {
		t.Fatalf("需重启拒绝后表不变（端点数 = %d，期望 1）", n)
	}
}

// TestReloadKeepsBreakerState 热加载不动熔断状态（⛔ reload 不是清熔断的后门）：
// 熔断 open ⇒ Reload（可热更变更）后仍 open。
func TestReloadKeepsBreakerState(t *testing.T) {
	srv := reloadEnv(t, []epSpec{{
		name: "primary", base: "http://127.0.0.1:18001/v1", model: "m", upstreamModel: "mimo-v2.6-flash",
		provider: "xiaomi", credRef: "TEST_KEY_MIMO",
		ctxWindow: 128000, maxOut: 1024, priority: 0, timeoutS: 5,
	}})
	brk := srv.breakerFor("primary")
	brk.RecordFailure(time.Now()) // threshold 默认 5 还差 4 次——改用 Trip 直接开闸
	brk.Trip(time.Now())
	if brk.State() != "open" {
		t.Fatalf("前置：熔断应 open，实际 %s", brk.State())
	}
	writeRegistryFile(t, srv.regPath, []epSpec{{
		name: "primary", base: "http://127.0.0.1:18001/v1", model: "m", upstreamModel: "mimo-v2.6-flash",
		provider: "xiaomi", credRef: "TEST_KEY_MIMO",
		ctxWindow: 128000, maxOut: 1024, priority: 0, timeoutS: 7,
	}}, nil)
	if out := srv.Reload(); !out.OK {
		t.Fatalf("可热更变更应成功：%+v", out)
	}
	if got := srv.breakerFor("primary").State(); got != "open" {
		t.Fatalf("热加载后熔断 = %s，期望 open（状态不被洗掉）", got)
	}
}

// TestAdminReloadEndpoint POST /admin/reload 鉴权与状态码口径。
func TestAdminReloadEndpoint(t *testing.T) {
	srv := reloadEnv(t, []epSpec{{
		name: "primary", base: "http://127.0.0.1:18001/v1", model: "m", upstreamModel: "mimo-v2.6-flash",
		provider: "xiaomi", credRef: "TEST_KEY_MIMO",
		ctxWindow: 128000, maxOut: 1024, priority: 0, timeoutS: 5,
	}})
	admin, err := srv.AdminHandler("adm-token")
	if err != nil {
		t.Fatalf("AdminHandler 报错: %v", err)
	}
	ts := httptest.NewServer(admin)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer adm-token")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /admin/reload = %d，期望 200（无变更重载成功）", resp.StatusCode)
	}
	readBody(t, resp)

	// 需重启变更 ⇒ 409（加端点 = 需重启变更）。
	writeRegistryFile(t, srv.regPath, []epSpec{
		{
			name: "primary", base: "http://127.0.0.1:18001/v1", model: "m", upstreamModel: "mimo-v2.6-flash",
			provider: "xiaomi", credRef: "TEST_KEY_MIMO",
			ctxWindow: 128000, maxOut: 1024, priority: 0, timeoutS: 5,
		},
		{
			name: "backup", base: "http://127.0.0.1:18002/v1", model: "m", upstreamModel: "mimo-v2.6-flash",
			provider: "xiaomi", credRef: "TEST_KEY_MIMO",
			ctxWindow: 128000, maxOut: 1024, priority: 1, timeoutS: 5,
		},
	}, nil)
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/reload", nil)
	req2.Header.Set("Authorization", "Bearer adm-token")
	resp2, _ := http.DefaultClient.Do(req2)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("需重启变更 = %d，期望 409", resp2.StatusCode)
	}
	readBody(t, resp2)

	// GET 不收。
	req3, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/reload", nil)
	req3.Header.Set("Authorization", "Bearer adm-token")
	resp3, _ := http.DefaultClient.Do(req3)
	if resp3.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d，期望 405", resp3.StatusCode)
	}
	resp3.Body.Close()
}
