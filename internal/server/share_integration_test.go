// 日期: 2026-11-16 23:39:48.270
// 作者: Zhai Wenjia

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/telemetry"

	"github.com/zhaiwenjia/FlyingGateway/internal/share"
)

// redisStoreForTest 连本地 Redis（REDIS_ADDR 可覆盖）；不可达 ⇒ 显式 Skip。
func redisStoreForTest(t *testing.T) *share.RedisStore {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rs := share.NewRedisStore(addr, os.Getenv("REDIS_PASSWORD"), 2*time.Second, 4)
	if _, err := rs.Get("fgtest:ping"); err != nil {
		t.Skipf("redis 不可达（%s）：%v —— 共享集成测试显式跳过", addr, err)
	}
	return rs
}

// TestSharedCooldownAcrossInstances 钉住「429 冷却跨实例可见」：实例 A 标记冷却后
// 实例 B 的同 key 槽立即进入冷却（⛔ 不再各自记各自的冷却）。
func TestSharedCooldownAcrossInstances(t *testing.T) {
	rs := redisStoreForTest(t)
	coolKey := "fgtest:cool:" + time.Now().Format("150405.000000000")
	a := &tokenSlot{store: rs, coolKey: coolKey}
	b := &tokenSlot{store: rs, coolKey: coolKey}
	now := time.Now()
	a.markRateLimited(now, 30*time.Second)
	cooling, rem := b.cooling(now)
	if !cooling {
		t.Fatal("B 应感知 A 标记的冷却（跨实例共享）")
	}
	if rem < 29*time.Second {
		t.Fatalf("冷却剩余 = %v，期望 ≈30s", rem)
	}
	// 只增不减：B 再标 10s 不缩短。
	b.markRateLimited(now, 10*time.Second)
	_, rem2 := a.cooling(now)
	if rem2 < rem {
		t.Fatalf("冷却被缩短：%v → %v（⛔ 只增不减）", rem, rem2)
	}
}

// TestSharedCallerLimitAcrossServers 端到端钉住「两网关实例共享一份调用方配额」：
// 两台 Server 同注册表（callers.rate_limit 登记 rpm=60/burst=1 ⇒ 请求桶深 1）共享
// 同一 Redis ⇒ 实例 1 扣掉的配额实例 2 立即可见（实例 2 的请求被 429）。
// 错误实现对照：共享没接上 ⇒ 实例 2 放行 ⇒ 红。
func TestSharedCallerLimitAcrossServers(t *testing.T) {
	rs := redisStoreForTest(t)
	uniq := time.Now().Format("150405.000000000")
	t.Setenv("TEST_KEY_MIMO", "sk-gw-mimo-0003")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ""+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"},\"finish_reason\":null}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer up.Close()
	reg := writeRegistry(t, []epSpec{{
		name: "primary", base: up.URL, model: "m", upstreamModel: "mimo-v2.6-flash",
		provider: "xiaomi", credRef: "TEST_KEY_MIMO",
		ctxWindow: 128000, maxOut: 1024, priority: 0, timeoutS: 5,
	}}, nil, map[string]any{
		"callers": []map[string]any{callersWire("rl", "caller-key-rl-"+uniq, time.Now().Add(24*time.Hour),
			[]string{"m"}, []string{"primary"}, 0,
			map[string]any{"tpm": 0, "rpm": 60, "burst_s": 1})},
	})
	mk := func() *httptest.Server {
		srv, err := New(Options{
			Registry:            reg,
			Tracer:              telemetry.NewTracer(&telemetry.InMemoryExporter{}),
			Metrics:             telemetry.NewMetrics(),
			AllowLoopbackEgress: true,
			Logger:              testLogger(),
			Share:               rs,
		})
		if err != nil {
			t.Fatalf("server.New 失败: %v", err)
		}
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		return ts
	}
	e1 := mk()
	e2 := mk()
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`
	post := func(ts *httptest.Server) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer caller-key-rl-"+uniq)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		return resp
	}

	resp1 := post(e1)
	resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Fatalf("实例 1 = %d，期望 200", resp1.StatusCode)
	}
	resp2 := post(e2)
	resp2.Body.Close()
	if resp2.StatusCode != 429 {
		t.Fatalf("实例 2 = %d，期望 429（共享调用方配额已被实例 1 扣空）", resp2.StatusCode)
	}
}
