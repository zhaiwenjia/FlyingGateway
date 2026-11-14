// 日期: 2026-11-14 15:16:06.596
// 作者: Zhai Wenjia

package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/version"
)

// discardLogger 返回丢弃输出的 logger（测试里不刷日志）。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestVersionFlag 断言 -version / -V 打印 internal/version.Version 后退出（0）。
func TestVersionFlag(t *testing.T) {
	for _, flagName := range []string{"-version", "-V", "--version"} {
		var out bytes.Buffer
		if code := run([]string{flagName}, &out); code != 0 {
			t.Errorf("run(%q) 退出码 = %d，期望 0", flagName, code)
		}
		if got := strings.TrimSpace(out.String()); got != version.Version {
			t.Errorf("run(%q) 输出 = %q，期望 version.Version=%q", flagName, got, version.Version)
		}
	}
}

// TestParseFlagsShortLongEquivalence 断言长选项与单字母缩写等价（CLI 双形式口径）。
func TestParseFlagsShortLongEquivalence(t *testing.T) {
	longForm, err := parseFlags([]string{
		"--config", "a.json", "--rates", "b.json", "--listen", ":9999",
		"--admin-listen", "127.0.0.1:9090", "--allow-loopback",
		"--cancel-grace", "2s", "--detect-bound", "800ms",
		"--rate-capacity", "10", "--rate-refill", "2.5", "--budget-tokens", "100",
		"--guardrail-pattern", "x", "--guardrail-webhook", "http://127.0.0.1:1/h",
		"--pprof-listen", "127.0.0.1:6060",
	})
	if err != nil {
		t.Fatalf("长选项解析失败: %v", err)
	}
	shortForm, err := parseFlags([]string{
		"-c", "a.json", "-r", "b.json", "-l", ":9999",
		"-a", "127.0.0.1:9090", "-L",
		"-g", "2s", "-d", "800ms",
		"-C", "10", "-R", "2.5", "-B", "100",
		"-P", "x", "-W", "http://127.0.0.1:1/h",
		"-p", "127.0.0.1:6060",
	})
	if err != nil {
		t.Fatalf("短选项解析失败: %v", err)
	}
	if longForm.configPath != shortForm.configPath || longForm.configPath != "a.json" {
		t.Errorf("config 长短不等价: %q vs %q", longForm.configPath, shortForm.configPath)
	}
	if longForm.ratesPath != shortForm.ratesPath || longForm.ratesPath != "b.json" {
		t.Errorf("rates 长短不等价: %q vs %q", longForm.ratesPath, shortForm.ratesPath)
	}
	if longForm.listenAddr != shortForm.listenAddr || longForm.listenAddr != ":9999" {
		t.Errorf("listen 长短不等价: %q vs %q", longForm.listenAddr, shortForm.listenAddr)
	}
	if longForm.adminListenAddr != shortForm.adminListenAddr {
		t.Errorf("admin-listen 长短不等价: %q vs %q", longForm.adminListenAddr, shortForm.adminListenAddr)
	}
	if longForm.pprofListen != shortForm.pprofListen || longForm.pprofListen != "127.0.0.1:6060" {
		t.Errorf("pprof-listen 长短不等价: %q vs %q", longForm.pprofListen, shortForm.pprofListen)
	}
	if !longForm.allowLoopback || !shortForm.allowLoopback {
		t.Error("allow-loopback 长短不等价（须都为 true）")
	}
	if longForm.cancelGrace != 2*time.Second || shortForm.cancelGrace != 2*time.Second {
		t.Errorf("cancel-grace 长短不等价: %v vs %v", longForm.cancelGrace, shortForm.cancelGrace)
	}
	if longForm.detectBound != 800*time.Millisecond || shortForm.detectBound != 800*time.Millisecond {
		t.Errorf("detect-bound 长短不等价: %v vs %v", longForm.detectBound, shortForm.detectBound)
	}
	if longForm.rateCapacity != 10 || shortForm.rateCapacity != 10 {
		t.Errorf("rate-capacity 长短不等价: %d vs %d", longForm.rateCapacity, shortForm.rateCapacity)
	}
	if longForm.budgetTokens != 100 || shortForm.budgetTokens != 100 {
		t.Errorf("budget-tokens 长短不等价: %d vs %d", longForm.budgetTokens, shortForm.budgetTokens)
	}
	if strings.Join(longForm.guardrailPatterns, ",") != strings.Join(shortForm.guardrailPatterns, ",") {
		t.Errorf("guardrail-pattern 长短不等价: %v vs %v", longForm.guardrailPatterns, shortForm.guardrailPatterns)
	}
}

// TestDetectPollFromBound 断言轮询兜底周期 = 检测上界的 1/4（统一 SDD 6.6 有界延迟）。
func TestDetectPollFromBound(t *testing.T) {
	if got := detectPollFromBound(time.Second); got != 250*time.Millisecond {
		t.Errorf("detectPollFromBound(1s) = %v，期望 250ms", got)
	}
	if got := detectPollFromBound(0); got != 250*time.Millisecond {
		t.Errorf("detectPollFromBound(0) = %v，期望回落默认 250ms", got)
	}
}

// TestPprofListenToggle 断言 pprof 端点可关可开：默认不给 -pprof-listen ⇒ 端点不启（关）；
// 显式给环回地址 ⇒ /debug/pprof/ 可访问（开）；收口后不可达（再关）。
func TestPprofListenToggle(t *testing.T) {
	// 关：默认不给 -pprof-listen ⇒ 地址为空，run() 不会起 pprof 服务。
	def, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("默认参数解析失败: %v", err)
	}
	if def.pprofListen != "" {
		t.Errorf("默认 pprof-listen = %q，期望空（⛔ 不默认开）", def.pprofListen)
	}

	// 开：显式 -p ⇒ 端点起在环回地址上，/debug/pprof/ 返回索引页。
	on, err := parseFlags([]string{"-p", "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("-p 解析失败: %v", err)
	}
	if err := validatePprofAddr(on.pprofListen); err != nil {
		t.Fatalf("环回地址被拒（应放行）: %v", err)
	}
	srv, ln, err := startPprof(on.pprofListen, discardLogger())
	if err != nil {
		t.Fatalf("pprof 启动失败: %v", err)
	}
	base := "http://" + ln.Addr().String()
	resp, err := http.Get(base + "/debug/pprof/")
	if err != nil {
		t.Fatalf("pprof 索引请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("pprof 索引状态 = %d，期望 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "profile") {
		t.Errorf("pprof 索引页缺少 profile 入口，body 前 200 字节 = %q", string(body[:min(len(body), 200)]))
	}

	// 再关：收口后端点不可达。
	_ = srv.Close()
	if _, err := http.Get(base + "/debug/pprof/"); err == nil {
		t.Error("pprof 收口后仍可访问，期望连接失败")
	}
}

// TestValidatePprofAddr 断言 pprof 监听地址只放行环回 IP（⛔ 不绑内网 / 公网 / 全接口 / 主机名）。
func TestValidatePprofAddr(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:6060", "127.0.0.2:1", "[::1]:6060"} {
		if err := validatePprofAddr(ok); err != nil {
			t.Errorf("validatePprofAddr(%q) = %v，期望放行", ok, err)
		}
	}
	for _, bad := range []string{":6060", "0.0.0.0:6060", "[::]:6060", "192.168.1.5:6060", "10.0.0.1:6060", "localhost:6060", "6060"} {
		if err := validatePprofAddr(bad); err == nil {
			t.Errorf("validatePprofAddr(%q) 通过，期望拒绝", bad)
		}
	}
}
