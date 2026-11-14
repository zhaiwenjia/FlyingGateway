// 日期: 2026-11-14 15:16:06.596
// 作者: Zhai Wenjia

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/version"
)

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
