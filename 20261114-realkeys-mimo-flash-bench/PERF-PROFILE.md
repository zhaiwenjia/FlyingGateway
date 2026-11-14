# PERF-PROFILE —— FlyingGateway 性能画像与热点定位（第一阶段）

> **日期**：2026-11-14 · **执行**：MCode 子任务（只测不改业务逻辑）
> **范围**：pprof 排障基建（`cmd/gateway`）+ 本地受控并发阶梯压测 + pprof 热点画像 + 优化建议（只建议不实施）
> **硬约束遵守**：⛔ 未改 `internal/` 任何业务代码；新增/修改仅 `cmd/gateway/main.go`、`cmd/gateway/main_test.go`、`scripts/local_sweep.py`、`scripts/plot_sweep.py` 与 `raw/local/` 产物。

## 一、pprof 端点用法

- **默认关闭**，显式 `-pprof-listen`（`-p`）才启动；**只允许绑环回 IP 字面量**（127.0.0.0/8、::1），`:6060` / `0.0.0.0:6060` / 内网 IP / 主机名一律拒绝启动（pprof 无鉴权，暴露面由监听地址收死；实现 `cmd/gateway/main.go` 的 `validatePprofAddr`）。
- 开启：

```bash
./gateway -c config/gateway.example.json -p 127.0.0.1:6060
curl 'http://127.0.0.1:6060/debug/pprof/'          # 索引（heap/allocs/goroutine/block/mutex…）
curl 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30' -o cpu.pprof   # CPU 画像
curl 'http://127.0.0.1:6060/debug/pprof/heap'  -o heap.pprof               # 堆快照（inuse）
curl 'http://127.0.0.1:6060/debug/pprof/allocs' -o allocs.pprof            # 累计分配
go tool pprof -top cpu.pprof
```

- 用例：`cmd/gateway/main_test.go` 的 `TestPprofListenToggle`（默认关 → 显式开、`/debug/pprof/` 返 200 含 `profile` 入口 → Close 后不可达）与 `TestValidatePprofAddr`（只放行环回）。实跑结果：`ok github.com/zhaiwenjia/FlyingGateway/cmd/gateway`（3 个用例 PASS，见第六节复现命令）。

## 二、测试环境与方法

- **链路**：loadgen（本机 Python/aiohttp）→ FlyingGateway（`127.0.0.1:18080`）→ 假上游 `scripts/fake_upstream.py`（`127.0.0.1:9099`，固定 300ms 延迟，OpenAI Chat 形响应）。对照组 loadgen → 假上游直连。
- **被测配置**：`raw/local/gateway-local.json`（单端点 `local-fake`，`upstream_model=qwen-bench-fake` ⇒ 识别为 Qwen 兼容层出站 `POST {base}/chat/completions`）；网关参数 `-L`（放行环回出站）+ `-p 127.0.0.1:6060`。
- **限流桶显式调大**（`-C/-R/-K/-E/-U/-u = 100000000`）：本压测测数据面吞吐与延迟，令牌桶默认补充 10000 token/s 会把高并发档吞吐钳住（c=256 时预估 token 消耗 ≈ 777 req/s × 每请求 30+ token），失真成限流器测试。限流检查仍在热路径执行（policy 环节归因不受影响）。
- **负载**：非流式 `POST /v1/chat/completions`，`max_tokens=16`、`temperature=0`、固定小 prompt；每档请求数 = 并发 × 8，**2 轮取中位**（`sweep_summary.json` 的 `median()`，偶数轮取中间两值均值）。
- **⚠️ 口径限制**：单机同跑 loadgen / 网关 / 假上游三进程（macOS arm64，Go 1.27.3），绝对值供画像与对照用，不代表生产容量；pprof 只采网关进程。
- **可重跑**：`/tmp/bench/bin/python scripts/local_sweep.py -m all`（构建网关 → 起假上游与网关 → 阶梯压测 → pprof 画像）；绘图 `/tmp/bench/bin/python scripts/plot_sweep.py`。

## 三、并发阶梯数据（2 轮中位；每轮成功率 100%，ok = 请求数、fail = 0）

| 并发 | 目标 | 请求数 | 吞吐 rps | P50 ms | P99 ms | max ms |
|---:|---|---:|---:|---:|---:|---:|
| 1 | 网关 | 8 | 3.22 | 310.75 | 313.25 | 313.25 |
| 1 | 直连 | 8 | 3.26 | 306.0 | 309.1 | 309.1 |
| 8 | 网关 | 64 | 25.2 | 315.75 | 328.45 | 328.45 |
| 8 | 直连 | 64 | 25.96 | 306.75 | 315.7 | 315.7 |
| 32 | 网关 | 256 | 98.365 | 321.05 | 351.75 | 354.0 |
| 32 | 直连 | 256 | 102.23 | 307.65 | 329.8 | 330.0 |
| 64 | 网关 | 512 | 195.215 | 323.95 | 348.75 | 354.45 |
| 64 | 直连 | 512 | 202.59 | 309.3 | 338.6 | 338.85 |
| 128 | 网关 | 1024 | 383.865 | 327.9 | 358.3 | 364.15 |
| 128 | 直连 | 1024 | 393.275 | 319.25 | 361.2 | 361.55 |
| 256 | 网关 | 2048 | 748.67 | 334.5 | 385.65 | 392.75 |
| 256 | 直连 | 2048 | 765.88 | 321.3 | 384.85 | 385.1 |

> 数值逐字取自 `raw/local/sweep_summary.json`（个别中位是两轮均值，JSON 存浮点全精度，表内按两位小数展示）；两轮逐请求明细在 `raw/local/sweep_<target>_c<并发>_r<轮>.json`。
> 阶梯曲线图：`raw/local/20261114-sweep-curve.png`（matplotlib ggplot + PingFang）。

**读数**（差值为表内两数直接相减，算式随附）：
- 网关 P50 固定开销从 c=1 的 +4.75ms（310.75 − 306.0）增至 c=256 的 +13.2ms（334.5 − 321.3），随并发缓慢增长；吞吐低 2%~4%（c=256：748.67 vs 765.88）。
- P99 在中档最差：c=32 网关 351.75 vs 直连 329.8（+21.95ms）；c=128/256 两档两者接近（358.3 vs 361.2 / 385.65 vs 384.85）。
- 所有档位成功率 100%，无 429/5xx（限流桶已调大，见第二节）。

## 四、pprof 热点画像（压测期间采集）

采集方式：c=64 与 c=256 各持续压 35s（c=64：6976 请求 / 0 失败 / 197.8 rps / P50 322.8ms；c=256：27440 请求 / 0 失败 / 776.76 rps / P50 328.6ms，`raw/local/sweep_profile_c*.json`），窗口内抓 `profile?seconds=30` + `heap` + `allocs`（`raw/local/pprof/c{64,256}/`）。

**CPU profile 摘录**（`go tool pprof -top`，flat）：

| c=64（总样本 4.81s / 30.01s 窗口） | | c=256（总样本 14.47s / 30.10s 窗口） | |
|---|---:|---|---:|
| syscall.rawsyscalln | 2.12s 44.07% | syscall.rawsyscalln | 6.97s 48.17% |
| runtime.usleep | 0.47s 9.77% | runtime.kevent | 1.37s 9.47% |
| runtime.kevent | 0.45s 9.36% | runtime.usleep | 1.19s 8.22% |
| runtime.pthread_cond_wait | 0.25s 5.20% | runtime.pthread_cond_wait | 0.87s 6.01% |
| runtime.madvise | 0.24s 4.78% | runtime.pthread_cond_signal | 0.83s 5.74% |
| runtime.pthread_cond_signal | 0.23s 4.57% | runtime.madvise | 0.60s 4.15% |
| runtime.pthread_kill | 0.19s 3.95% | runtime.pthread_kill | 0.54s 3.73% |

**alloc_space 摘录**（`go tool pprof -top -sample_index=alloc_space`，flat；c=64 累计 722.71MB，c=256 累计 3773.10MB，均为进程启动以来累计、覆盖预热 + 对应档位）：

| c=64 | | c=256 | |
|---|---:|---|---:|
| stream.NewSSEScanner | 373.89MB 51.73% | stream.NewSSEScanner | 2045.47MB 54.21% |
| gjson.Result.arrayOrMap | 37.53MB 5.19% | gjson.Result.arrayOrMap | 221.17MB 5.86% |
| adapter.(*qwenChatStreamDecoder).decodeOne（flat） | 29.04MB 4.02% | adapter.(*qwenChatStreamDecoder).decodeOne（flat） | 166.70MB 4.42% |
| adapter.(*qwenAdapter).BuildRequest（flat） | 26.52MB 3.67% | reflect.unsafe_New | 124.60MB 3.30% |
| reflect.unsafe_New | 23.52MB 3.25% | adapter.(*qwenAdapter).BuildRequest（flat） | 130.61MB 3.46% |
| telemetry.(*Span).SetAttr | 16.51MB 2.28% | encoding/json/v2.makeMethodArshaler.func3 | 84.10MB 2.23% |

（完整 top60 见 `raw/local/pprof/c*/\*.top.txt`：`cpu.cpu.top.txt`、`allocs.alloc_space.top.txt`、`allocs.alloc_objects.top.txt`、`heap.inuse_space.top.txt`。）

## 五、各环节归因表

**归类规则**：① 本仓函数按包路径归类（`internal/codec` → 入站解析/响应装配、`internal/stream` + 各家 stream decoder → 流式解析、`internal/adapter` BuildRequest 系 → 适配器请求构造、`internal/registry`+`internal/routing`+`internal/detect` → 路由注册、`internal/policy`+`internal/server/tokenpool.go`+`caller.go` → 治理检查、`internal/telemetry`+`main.(*logExporter)`+`log/slog` 链 → 遥测、`internal/server/cancel.go` 系 → 取消监听）；② net/http 标准库按 `pprof -peek` 调用者拆分到入站/出站（`net/http.(*persistConn).*` = 上游传输，`net/http.(*conn).serve`/`connReader`/`response.finishRequest` = 入站）；③ `runtime.*`/GC/调度不归环节，单列；④ 写/读系统调用内核时间按 `go tool pprof -peek syscall.write/syscall.read` 的调用者占比拆分（依据列给出被除数与除数）。

### CPU%（占总样本；c=64 总样本 4.81s、c=256 总样本 14.47s）

| 环节 | c=64 | c=256 | 依据（pprof 数字） |
|---|---:|---:|---|
| 遥测 telemetry（span 收口 + slog 日志生成/落盘） | ≈16% | ≈18% | `log/slog.(*Logger).Info` cum 780ms/16.22%（c64）、2.59s/17.90%（c256）；其中日志落盘 `os.(*File).Write` cum 730ms/15.18%、2.45s/16.93%（每请求一条 span 日志同步 write） |
| 上游执行与传输（请求写出 + 响应读 + 建连/关连） | ≈22% | ≈21% | write 拆分：`persistConn.writeLoop` 0.58s/12.06%（c64）、1.74s/12.02%（c256）；上游读（`bufio.fill` ← `persistConn.Read` 66.07%/66.18%）：0.37s/7.69%、0.90s/6.22%；`net/http.(*Transport).dial` cum 0.39s/2.70%（c256） |
| 入站解析 codec（请求读 + JSON 解码） | ≈4.5% | ≈4% | 入站读（`connReader.Read` 33.93%/33.82%）：0.19s/3.95%、0.46s/3.18%；`encoding/json.Unmarshal` cum 0.05s/1.04%、0.08s/0.55% |
| 响应装配序列化（帧累积 + 响应编码 + 入站响应写） | ≈4% | ≈4% | `response.finishRequest` write 0.17s/3.53%（两档同）+ `codec/chat.EncodeResponse`/`frameAccumulator` ≈0.5% |
| 流式解析 stream（SSE 扫描 + chunk 解码） | ≈1.5% | ≈1.5% | `qwenChatStreamDecoder.decode` cum 0.08s/0.55%、`sseIter.Next` 0.55%、`newSSEIter` 0.55%（c256；CPU 侧小，alloc 侧大） |
| 适配器请求构造 | ≈1.3% | ≈1.5% | `qwenAdapter.BuildRequest` cum 0.15s/1.04%（c256）+ `marshalBody`/openai-go 参数序列化 ≈0.5% |
| 治理检查 policy | <0.3% | <0.3% | `checkGovernance`/`TokenBucket.Allow`/`callerAuth` 低于 pprof 丢弃阈值（cum ≤ 0.07s），`bucketFor` 无独立样本 |
| 路由注册（Lookup/Detect/上下文判定） | ≈0.1% | ≈0.1% | `detect.Detect` cum 0.01s/0.069%（c256），registry.Lookup/routing.CheckContext 无独立样本 |
| 取消监听（cancel 注册表 + 断连监听） | ≈1% | ≈1% | `cancelRegistry.purgeExpiredLocked` flat 0.11s/0.76%（c256）+ `register`/watch 碎片 |
| 运行时/GC/调度（不可归环节） | ≈48% | ≈47% | `kevent`+`usleep`+`pthread_cond_*`+`madvise`+`pthread_kill` flat 已 ≈37%（c256 9.47+8.22+6.01+5.74+4.15+3.73），加 GC 标记/扫描（`gcDrain` cum 12.58%、`markroot` 7.39%）与调度（`findRunnable` 10.44%）残差 |

> 拆分算式示例（c256）：write 总量 `syscall.write` cum 4.70s = 日志 `os.(*File).Write` 2.45s + 上游 `persistConnWriter.Write` 1.74s + 入站 `checkConnErrorWriter.Write` 0.51s；read 总量 `syscall.read` cum 1.72s 中 `bufio.fill` 1.36s 按 Peek 调用者 66.18%/33.82% 拆上游 0.90s / 入站 0.46s，其余 0.36s 归杂项。各行「≈」为上述实测值的加总，环节间无重叠根（cum 根取互斥函数）。

### alloc%（占 alloc_space 累计）

| 环节 | c=64（722.71MB） | c=256（3773.10MB） | 依据（pprof 数字） |
|---|---:|---:|---|
| 流式解析 stream | ≈66% | ≈70% | `newSSEIter`→`NewSSEScanner` cum 54.61%（c256；flat 2045.47MB/54.21%）+ `qwenChatStreamDecoder.decode` cum 15.21%（含 openai-go `ChatCompletionChunk.UnmarshalJSON` → apijson 9.77% + gjson 5.86%） |
| 适配器请求构造 | ≈12.6% | ≈10.9% | `qwenAdapter.BuildRequest` cum 91.06MB/12.60%（c64）、409.32MB/10.85%（c256，含 openai-go `param.Marshal*` 82.01MB/2.17%） |
| 遥测 telemetry | ≈4% | ≈4.5% | `Span.SetAttr` 16.51MB/2.28%（c64）、63.55MB/1.68%（c256）+ `fmt.Sprintf`/`fmtsort.Sort` 2.85%（c256） |
| 入站解析 codec | ≈4.7% | ≈4.1% | `net/http.readRequestLimit` 15.50MB/2.15%、`textproto.readMIMEHeader` 35.01MB/0.93%、`MIMEHeader.Set` 41.01MB/1.09%、`codec/chat.DecodeRequest` 33.01MB/0.87%（c256） |
| 响应装配序列化 | ≈2% | ≈2% | `frameAccumulator.result` 26MB/0.69%、`codec/chat.EncodeResponse` 23.01MB/0.61%、`bytes.Clone` 24.51MB/0.65%（c256） |
| 上游执行与传输 | ≈4% | ≈4% | `Transport.dialConn` 43.12MB/1.14%、`net/http.readLoop` 31MB/0.82%、`Header.Clone` 19.01MB/0.5%、`bufio.NewWriterSize` 25.58MB/0.68%（c256） |
| 取消监听/上下文传播 | ≈1.5% | ≈1.7% | `context.(*cancelCtx).propagateCancel` 23.50MB/0.62%、`WithDeadlineCause` 21.5MB/0.57%（c256） |
| 共享/运行时（reflect、切片底座等） | ≈6% | ≈8% | `reflect.unsafe_New` 124.60MB/3.30%、`unsafe_NewArray` 55.55MB/1.47%、`MakeSlice` 58.55MB/1.55%（c256；多为上述环节的反射分配底座） |

## 六、优化建议清单（按「预期收益 × 实施成本」排序；⛔ 本轮只建议不实施）

| # | 建议 | 预期收益 | 实施成本 | 证据出处 |
|---:|---|---|---|---|
| 1 | **SSE 扫描器缓冲复用/按需增长**：`internal/stream.NewSSEScanner` 每个上游流 `make([]byte, 0, 64<<10)` 即每请求固定 64KB 分配（实测 ≈59KB/请求），改 `sync.Pool` 复用或让 `bufio.Scanner` 从默认小缓冲起按需增长（`maxSSELine` 防线保留） | 高：单点消掉 alloc_space 的 54%（2045.47MB/3773.10MB），直接减轻 GC（运行时/GC ≈47% CPU 里 madvise 4.15% + 标记扫描 ≈10% 与分配量正相关） | 低：单函数、无语义变化 | `raw/local/pprof/c256/allocs.alloc_space.top.txt`（NewSSEScanner 54.21%）；`internal/stream/sse.go:40` |
| 2 | **遥测日志改批量/异步/采样落盘**：每请求一条 span 日志走 `slog` 同步 `write`，日志链占 CPU 16~18%（其中内核 write 15~17%），比网关全部业务逻辑 CPU 还高 | 高：CPU 直降 ≈15%（c=256 口径），P50 固定开销（+4.75ms@c=1）中同步日志写盘是可测组成部分 | 中：需保关键事件不丢、有序可配 | `raw/local/pprof/c{64,256}/cpu.cpu.top.txt` + cum 链（`log/slog.(*Logger).Info` 16.22%/17.90%、`os.(*File).Write` 15.18%/16.93%） |
| 3 | **流式 chunk 解码降载**：`qwenChatStreamDecoder.decodeOne` 对每个 SSE chunk 全量反序列化 openai-go typed `ChatCompletionChunk`（反射 + apijson + gjson），占 alloc ≈15%；改按需取字段（gjson 直取 / 本仓 codec 既有解析）或复用解码缓冲 | 中高：alloc 再降 ≈15%，顺带减 GC 与 CPU（流式解析 CPU ≈1.5% 但分配放大） | 中：需保持各家 decoder 兼容面不变 | `raw/local/pprof/c256/allocs.alloc_space.top.txt`（decodeOne cum 14.85%、apijson 9.77%、gjson 5.86%）；`pprof -peek apijson.UnmarshalRoot` 调用链 |
| 4 | **上游连接池参数**：`internal/adapter` 用 `http.DefaultClient`（`defaultClient`），`MaxIdleConnsPerHost` 走 stdlib 默认 2，高并发下出站连接 churn（建连 `dial` cum 2.70% + `dialConn`/`readLoop`/`writeLoop` 分配与协程开销）；给适配器配共享 `http.Transport`（`MaxIdleConnsPerHost` 按并发档调） | 中：高并发档吞吐差 2~4%（c=256 748.67 vs 765.88 rps）与尾延迟的一部分 | 低：装配期一处配置 | `cpu.cpu.top.txt`（`net/http.(*Transport).dial` 0.39s/2.70%）；`allocs.alloc_space.top.txt`（`dialConn` 1.14%）；`internal/adapter/adapter.go:107` `defaultClient = http.DefaultClient` |
| 5 | **入站/出站 JSON 反射底座**：`reflect.unsafe_New`/`makeMethodArshaler` 合计 alloc ≈7%，如做 1/3 后仍热，可考虑请求/响应结构体复用或替换序列化路径 | 低中：单看占比不大 | 中高：涉及 codec 与 adapter 两侧契约 | `allocs.alloc_space.top.txt`（reflect.unsafe_New 3.30%、makeMethodArshaler.func3 2.23%） |
| 6 | **policy / 路由 / 取消监听：不建议动** | —（各 <1%，优化无收益） | — | CPU 归因表（checkGovernance/Lookup/Detect 低于 pprof 阈值；cancelRegistry ≈1%） |

## 附录：复现命令与原始数据索引

```bash
export PATH="/opt/homebrew/bin:$PATH"
cd 20261114-realkeys-mimo-flash-bench
/tmp/bench/bin/python scripts/local_sweep.py -m all          # 阶梯压测 + pprof 画像（可重跑）
/tmp/bench/bin/python scripts/plot_sweep.py                  # 阶梯曲线图
cd .. && gofmt -l . && go vet ./... && go build ./... && go test ./cmd/...   # pprof 用例
```

| 数据 | 路径 |
|---|---|
| 阶梯逐请求明细（24 份） | `raw/local/sweep_<target>_c{1,8,32,64,128,256}_r{1,2}.json` |
| 阶梯中位聚合 | `raw/local/sweep_summary.json` |
| 阶梯曲线图 | `raw/local/20261114-sweep-curve.png` |
| 画像期负载明细 | `raw/local/sweep_profile_c{64,256}.json` |
| pprof 原始画像 + top 表 | `raw/local/pprof/c{64,256}/{cpu,heap,allocs}.pprof` + `*.top.txt` |
| 网关日志（telemetry 归因佐证） | `raw/local/gateway_stderr.log` |
| 被测配置 | `raw/local/gateway-local.json` |
| 压测/绘图脚本 | `scripts/local_sweep.py`、`scripts/plot_sweep.py` |
