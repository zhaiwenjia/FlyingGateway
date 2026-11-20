// 日期: 2026-11-14 14:58:08.860
// 作者: Zhai Wenjia

// Command gateway 是 FlyingGateway 的进程入口（统一 SDD 2.1 ① 入口层 / 10.2）：
// 读配置（-config 注册表 + -rates 费率）→ 装配 registry / adapters / policy / telemetry →
// 起数据面与（配置开启的）管理面 → 优雅关闭（shutdown ⇒ 取消原因 shutdown）。
//
// 版本经 -ldflags 注入 internal/version.Version（⛔ 不硬编码版本号，见该包注释）。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/zhaiwenjia/FlyingGateway/internal/cost"
	"github.com/zhaiwenjia/FlyingGateway/internal/policy"
	"github.com/zhaiwenjia/FlyingGateway/internal/registry"
	"github.com/zhaiwenjia/FlyingGateway/internal/server"
	"github.com/zhaiwenjia/FlyingGateway/internal/share"
	"github.com/zhaiwenjia/FlyingGateway/internal/telemetry"
	"github.com/zhaiwenjia/FlyingGateway/internal/version"
)

// 配置默认值（统一 SDD 9.2：-config 默认 config/gateway.example.json、
// -rates 默认 config/rates.json；费率是费用数据唯一真源，配置只引用不复制价格）。
const (
	defaultConfigPath = "config/gateway.example.json"
	defaultRatesPath  = "config/rates.json"
	defaultListenAddr = ":8080"
	// adminTokenEnv 是管理面鉴权 token 的来源（统一 SDD 8.1：凭据只从环境变量读，
	// ⛔ 不给明文命令行参数 —— argv 对同机用户可见）。
	adminTokenEnv = "GATEWAY_ADMIN_TOKEN"
	// ⛔ 令牌桶无默认配额（统一 SDD 7.2 锚点分离）：契约配额走注册表 rate_limit /
	// callers.rate_limit（未登记 ⇒ 不限流），全局兜底桶默认关闭。拍脑袋默认值是负债 ——
	// 旧默认（容量 60000 / 补充 10000 tokens/s）实测在小请求形态下造出 ≈454 rps
	// 的隐藏吞吐天花板（P3 阶梯 c=256 档 429 实录，见 docs/20261114-限流过载-调研-*.md）。
)

// options 是命令行参数（长选项 + 单字母缩写双形式）。
type options struct {
	configPath string
	ratesPath  string
	listenAddr string

	adminListenAddr string
	allowLoopback   bool
	pprofListen     string

	cancelGrace           time.Duration
	cancelGraceByProvider map[string]time.Duration
	detectBound           time.Duration

	rateCapacity int64
	rateRefill   float64
	budgetTokens int64

	upstreamBucketCapacity int64
	upstreamBucketRefill   float64
	rateLimitCooldown      time.Duration
	maxTokenWait           time.Duration
	maxInFlight            int64

	upstreamProbeInterval time.Duration
	upstreamProbeTimeout  time.Duration
	redisAddr             string

	guardrailPatterns []string
	guardrailWebhook  string

	showVersion bool
}

// multiFlag 收集可重复的字符串参数（-P 护栏正则可给多条）。
type multiFlag []string

func (m *multiFlag) String() string { return fmt.Sprint([]string(*m)) }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// providerGraceFlag 收集按 provider 的延迟取消覆盖（-G provider=300ms 形态，可重复）。
type providerGraceFlag map[string]time.Duration

func (g *providerGraceFlag) String() string { return fmt.Sprint(map[string]time.Duration(*g)) }

func (g *providerGraceFlag) Set(v string) error {
	name, d, ok := splitKV(v)
	if !ok {
		return fmt.Errorf("须形如 provider=时长（如 openai=300ms），得到 %q", v)
	}
	dur, err := time.ParseDuration(d)
	if err != nil {
		return fmt.Errorf("时长解析失败: %w", err)
	}
	if *g == nil {
		*g = map[string]time.Duration{}
	}
	(*g)[name] = dur
	return nil
}

// splitKV 按第一个 = 切键值。
func splitKV(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

// parseFlags 解析命令行（长选项与单字母缩写双形式等价）。
func parseFlags(args []string) (*options, error) {
	opts := &options{}
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.StringVar(&opts.configPath, "config", defaultConfigPath, "端点注册表配置路径（JSON）")
	fs.StringVar(&opts.configPath, "c", defaultConfigPath, "同 --config")
	fs.StringVar(&opts.ratesPath, "rates", defaultRatesPath, "费率表路径（JSON）")
	fs.StringVar(&opts.ratesPath, "r", defaultRatesPath, "同 --rates")
	fs.StringVar(&opts.listenAddr, "listen", defaultListenAddr, "数据面监听地址（host:port）")
	fs.StringVar(&opts.listenAddr, "l", defaultListenAddr, "同 --listen")

	fs.StringVar(&opts.adminListenAddr, "admin-listen", "", "管理面监听地址（默认关闭；须内网 IP，⛔ 不绑公网）")
	fs.StringVar(&opts.adminListenAddr, "a", "", "同 --admin-listen")
	fs.BoolVar(&opts.allowLoopback, "allow-loopback", false, "出网校验放行环回地址（本地 Ollama 等，统一 SDD 8.2）")
	fs.BoolVar(&opts.allowLoopback, "L", false, "同 --allow-loopback")

	fs.StringVar(&opts.pprofListen, "pprof-listen", "", "pprof 排障端点监听地址（默认关闭；只绑环回 IP，⛔ 不绑公网）")
	fs.StringVar(&opts.pprofListen, "p", "", "同 --pprof-listen")

	fs.DurationVar(&opts.cancelGrace, "cancel-grace", 0, "延迟取消窗口（默认 0 = 立即取消，统一 SDD 3.7）")
	fs.DurationVar(&opts.cancelGrace, "g", 0, "同 --cancel-grace")
	fs.Var((*providerGraceFlag)(&opts.cancelGraceByProvider), "cancel-grace-provider", "按 provider 覆盖延迟窗口（provider=时长，可重复）")
	fs.Var((*providerGraceFlag)(&opts.cancelGraceByProvider), "G", "同 --cancel-grace-provider")
	fs.DurationVar(&opts.detectBound, "detect-bound", server.DefaultDetectBound, "断连检测延迟上界（默认 1s，统一 SDD 6.6）")
	fs.DurationVar(&opts.detectBound, "d", server.DefaultDetectBound, "同 --detect-bound")

	fs.Int64Var(&opts.rateCapacity, "rate-capacity", 0, "token 限流桶容量（0 = 不限流）")
	fs.Int64Var(&opts.rateCapacity, "C", 0, "同 --rate-capacity")
	fs.Float64Var(&opts.rateRefill, "rate-refill", 0, "token 限流补充速率（token/秒）")
	fs.Float64Var(&opts.rateRefill, "R", 0, "同 --rate-refill")
	fs.Int64Var(&opts.budgetTokens, "budget-tokens", 0, "每调用方 token 预算上限（0 = 不限预算）")
	fs.Int64Var(&opts.budgetTokens, "B", 0, "同 --budget-tokens")

	fs.Int64Var(&opts.upstreamBucketCapacity, "upstream-bucket-capacity", 0, "每自有令牌的本地令牌桶容量（契约配额走注册表 rate_limit；0 = 关闭）")
	fs.Int64Var(&opts.upstreamBucketCapacity, "U", 0, "同 --upstream-bucket-capacity")
	fs.Float64Var(&opts.upstreamBucketRefill, "upstream-bucket-refill", 0, "每自有令牌的本地令牌桶补充速率（token/秒）")
	fs.Float64Var(&opts.upstreamBucketRefill, "u", 0, "同 --upstream-bucket-refill")
	fs.DurationVar(&opts.rateLimitCooldown, "rate-limit-cooldown", 30*time.Second, "上游 429 无 Retry-After 时的令牌默认冷却")
	fs.DurationVar(&opts.rateLimitCooldown, "k", 30*time.Second, "同 --rate-limit-cooldown")
	fs.DurationVar(&opts.maxTokenWait, "max-token-wait", 3*time.Second, "全部自有令牌受限时的最长等待（超上限 ⇒ 429 + Retry-After）")
	fs.DurationVar(&opts.maxTokenWait, "m", 3*time.Second, "同 --max-token-wait")
	fs.Int64Var(&opts.maxInFlight, "max-in-flight", 0, "过载保护：在途请求数上限（0 = 不限；按实测容量标定，超限 ⇒ 429 + Retry-After）")
	fs.Int64Var(&opts.maxInFlight, "i", 0, "同 --max-in-flight")
	fs.DurationVar(&opts.upstreamProbeInterval, "upstream-probe-interval", 0, "上游周期巡检间隔（HTTP 可达性巡检、⛔ 不发模型调用；0 = 不启用；结果见 /admin/health probes）")
	fs.DurationVar(&opts.upstreamProbeInterval, "I", 0, "同 --upstream-probe-interval")
	fs.DurationVar(&opts.upstreamProbeTimeout, "upstream-probe-timeout", 3*time.Second, "单次上游巡检探测超时")
	fs.DurationVar(&opts.upstreamProbeTimeout, "T", 3*time.Second, "同 --upstream-probe-timeout")
	fs.StringVar(&opts.redisAddr, "redis-addr", "", "Redis 地址（host:port；空 = 内存单实例模式）—— 限流/预算/冷却计数跨实例共享；密码走环境变量 GATEWAY_REDIS_PASSWORD")
	fs.StringVar(&opts.redisAddr, "A", "", "同 --redis-addr")

	var pats multiFlag
	fs.Var(&pats, "guardrail-pattern", "护栏正则（可重复）")
	fs.Var(&pats, "P", "同 --guardrail-pattern")
	fs.StringVar(&opts.guardrailWebhook, "guardrail-webhook", "", "护栏 Webhook URL（空 = 只做正则）")
	fs.StringVar(&opts.guardrailWebhook, "W", "", "同 --guardrail-webhook")

	fs.BoolVar(&opts.showVersion, "version", false, "打印版本号后退出")
	fs.BoolVar(&opts.showVersion, "V", false, "同 --version")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	opts.guardrailPatterns = pats
	return opts, nil
}

// logExporter 把 span 元数据落结构化日志（统一 SDD 6.4：默认 NO_CONTENT ——
// ⛔ prompt / 响应全文不进 trace / 日志，只记形状、token、耗时、状态）。
type logExporter struct{ logger *slog.Logger }

// Export 实现 telemetry.Exporter。
func (e *logExporter) Export(spans []*telemetry.Span) {
	for _, sp := range spans {
		e.logger.Info("upstream span",
			"name", sp.Name,
			"status", spanStatusName(sp.Status),
			"ended", sp.Ended(),
			"attrs", sp.Attributes,
		)
	}
}

// spanStatusName 是 span 状态名。
func spanStatusName(st telemetry.SpanStatus) string {
	if st == telemetry.SpanError {
		return "ERROR"
	}
	return "UNSET"
}

// run 是可测试的主流程（返回退出码）。
func run(args []string, stdout io.Writer) int {
	opts, err := parseFlags(args)
	if err != nil {
		return 2
	}
	if opts.showVersion {
		fmt.Fprintln(stdout, version.Version)
		return 0
	}
	// 日志批量异步落盘（PERF-PROFILE 建议 2：span 日志同步 write 占 CPU 16–18%）：
	// 格式仍是默认 TextHandler（内容/字段语义不变），出口经 BatchHandler 有界队列 +
	// 后台批量 flush（BatchWriter 合并写）；defer Shutdown 收口排空 + flush（覆盖所有返回
	// 路径），溢出显式丢弃计数（⛔ 不静默丢，见 internal/telemetry/batch.go）。
	logBuf := telemetry.NewBatchWriter(os.Stderr, 0)
	logHandler := telemetry.NewBatchHandler(slog.NewTextHandler(logBuf, nil), logBuf, telemetry.BatchOptions{})
	defer logHandler.Shutdown() // 收口 flush；错误经 stderr 已无处可报，进程退出语义
	logger := slog.New(logHandler)

	reg, err := registry.Load(opts.configPath)
	if err != nil {
		logger.Error("注册表加载失败", "path", opts.configPath, "err", err)
		return 1
	}
	rates, err := cost.LoadRates(opts.ratesPath)
	if err != nil {
		logger.Error("费率表加载失败", "path", opts.ratesPath, "err", err)
		return 1
	}

	// 治理组件（显式配置才启用；⛔ 不静默按 0 处理）。
	var bucket *policy.TokenBucket
	if opts.rateCapacity > 0 && opts.rateRefill > 0 {
		bucket = policy.NewTokenBucket(opts.rateCapacity, opts.rateRefill)
	}
	var sharedStore share.Store
	if opts.redisAddr != "" {
		sharedStore = share.NewRedisStore(opts.redisAddr,
			os.Getenv("GATEWAY_REDIS_PASSWORD"), 3*time.Second, 8)
		logger.Info("共享计数存储已启用（Redis）", "addr", opts.redisAddr)
	}
	var budget *policy.Budget
	if opts.budgetTokens > 0 {
		if sharedStore != nil {
			budget = policy.NewSharedBudget(sharedStore, "fg:budget:",
				func(err error) { logger.Warn("共享计数存储故障（预算 fail-open 放行）", "err", err) },
				opts.budgetTokens)
		} else {
			budget = policy.NewBudget(opts.budgetTokens)
		}
	}
	var guardrail *policy.Guardrail
	if len(opts.guardrailPatterns) > 0 || opts.guardrailWebhook != "" {
		pats := make([]*regexp.Regexp, 0, len(opts.guardrailPatterns))
		for _, p := range opts.guardrailPatterns {
			re, err := regexp.Compile(p)
			if err != nil {
				logger.Error("护栏正则编译失败", "pattern", p, "err", err)
				return 1
			}
			pats = append(pats, re)
		}
		guardrail = policy.NewGuardrail(pats, opts.guardrailWebhook, nil)
	}

	srv, err := server.New(server.Options{
		Registry:               reg,
		Rates:                  rates,
		RegistryPath:           opts.configPath,
		RatesPath:              opts.ratesPath,
		Share:                  sharedStore,
		Tracer:                 telemetry.NewTracer(&logExporter{logger: logger}),
		Metrics:                telemetry.NewMetrics(),
		TokenBucket:            bucket,
		Budget:                 budget,
		Guardrail:              guardrail,
		UpstreamBucketCapacity: opts.upstreamBucketCapacity,
		UpstreamBucketRefill:   opts.upstreamBucketRefill,
		RateLimitCooldown:      opts.rateLimitCooldown,
		MaxTokenWait:           opts.maxTokenWait,
		MaxInFlight:            opts.maxInFlight,
		UpstreamProbeInterval:  opts.upstreamProbeInterval,
		UpstreamProbeTimeout:   opts.upstreamProbeTimeout,
		AllowLoopbackEgress:    opts.allowLoopback,
		CancelGracePeriod:      opts.cancelGrace,
		CancelGraceByProvider:  opts.cancelGraceByProvider,
		DisconnectPollInterval: detectPollFromBound(opts.detectBound),
		Logger:                 logger,
	})
	if err != nil {
		logger.Error("服务装配失败", "err", err)
		return 1
	}

	// 数据面。
	dataSrv := &http.Server{Addr: opts.listenAddr, Handler: srv.Handler()}
	dataErr := make(chan error, 1)
	go func() {
		logger.Info("数据面启动", "addr", opts.listenAddr)
		if err := dataSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			dataErr <- err
		}
	}()

	// 管理面（默认关闭；显式开启 + 强制鉴权 + 只绑内网，统一 SDD 2.3 域⑤ / 8.5）。
	var adminSrv *http.Server
	if opts.adminListenAddr != "" {
		if err := server.ValidateAdminAddr(opts.adminListenAddr); err != nil {
			logger.Error("管理面拒绝启动", "err", err)
			_ = dataSrv.Close()
			return 1
		}
		token := os.Getenv(adminTokenEnv)
		if token == "" {
			logger.Error("管理面强制鉴权：环境变量 " + adminTokenEnv + " 未设置，拒绝起管理面")
			_ = dataSrv.Close()
			return 1
		}
		adminHandler, err := srv.AdminHandler(token)
		if err != nil {
			logger.Error("管理面装配失败", "err", err)
			_ = dataSrv.Close()
			return 1
		}
		adminSrv = &http.Server{Addr: opts.adminListenAddr, Handler: adminHandler}
		go func() {
			logger.Info("管理面启动", "addr", opts.adminListenAddr)
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				dataErr <- err
			}
		}()
	}

	// pprof 排障端点（默认关闭；显式 -pprof-listen 才起，只绑环回，⛔ 不绑公网 ——
	// pprof 无鉴权，暴露面必须由监听地址收死）。
	var pprofSrv *http.Server
	if opts.pprofListen != "" {
		if err := validatePprofAddr(opts.pprofListen); err != nil {
			logger.Error("pprof 端点拒绝启动", "err", err)
			_ = dataSrv.Close()
			return 1
		}
		var err error
		pprofSrv, _, err = startPprof(opts.pprofListen, logger)
		if err != nil {
			logger.Error("pprof 端点启动失败", "addr", opts.pprofListen, "err", err)
			_ = dataSrv.Close()
			return 1
		}
	}

	// 配置热加载（统一 SDD 9.2）：SIGHUP 触发重读注册表/费率表，先校验后原子切换、
	// 失败保留旧表（与 POST /admin/reload 同一路径）。
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			out := srv.Reload()
			switch {
			case out.OK:
				logger.Info("配置热加载成功")
			case len(out.RestartRequired) > 0:
				logger.Warn("配置变更需重启（热加载拒绝，旧表继续生效）", "restart_required", out.RestartRequired)
			default:
				logger.Error("配置热加载失败（旧表继续生效）", "err", out.Error)
			}
		}
	}()

	// 优雅关闭：shutdown ⇒ 在途执行以 shutdown 原因立即收口（统一 SDD 6.6 要求 4）。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-dataErr:
		logger.Error("监听失败", "err", err)
		srv.Shutdown()
		return 1
	case s := <-sig:
		logger.Info("收到退出信号，优雅关闭", "signal", s.String())
	}
	srv.Shutdown()
	if adminSrv != nil {
		_ = adminSrv.Close()
	}
	if pprofSrv != nil {
		_ = pprofSrv.Close()
	}
	_ = dataSrv.Close()
	return 0
}

// detectPollFromBound 由检测延迟上界导出轮询兜底周期（上界的 1/4，统一 SDD 6.6）。
func detectPollFromBound(bound time.Duration) time.Duration {
	if bound <= 0 {
		bound = server.DefaultDetectBound
	}
	return bound / 4
}

// validatePprofAddr 校验 pprof 监听地址只绑环回（127.0.0.0/8、::1）：
// pprof 端点无鉴权且暴露运行时内部（堆、goroutine、trace）⇒ 比管理面更严，
// ⛔ 不绑内网 / 公网、⛔ 不用主机名（解析漂移不可复现，与管理面同口径）。
func validatePprofAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("pprof 监听地址 %q 非法（须 host:port）: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("pprof 监听地址 %q 必须是环回 IP 字面量（⛔ 不绑全接口 / 主机名）", addr)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("pprof 监听地址 %q 非环回（⛔ 不绑内网 / 公网，pprof 无鉴权）", addr)
	}
	return nil
}

// newPprofMux 构造 pprof 端点路由：只挂 net/http/pprof 标准库处理器，
// ⛔ 不动 http.DefaultServeMux（避免 import 副作用把 pprof 挂到数据面上）。
// 覆盖 cmdline / profile（CPU）/ symbol / trace 与 Index 兜底的具名 profile
// （heap / allocs / goroutine / block / mutex 等）。
func newPprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// startPprof 启动 pprof 端点（独立端口的 http.Server，随进程收口）。
// 调用前须过 validatePprofAddr；测试用 127.0.0.1:0 拿随机端口（返回 Listener 供取实际地址）。
func startPprof(addr string, logger *slog.Logger) (*http.Server, net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{Addr: addr, Handler: newPprofMux()}
	go func() {
		logger.Info("pprof 端点启动", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("pprof 端点监听失败", "err", err)
		}
	}()
	return srv, ln, nil
}

// main 薄壳：进程退出码由 run 决定。
func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}
