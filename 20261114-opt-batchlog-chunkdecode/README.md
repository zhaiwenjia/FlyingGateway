# 20261114 遥测批量落盘 + chunk 解码去反射 · 基准与验证证据

## 问题

`20261114-realkeys-mimo-flash-bench/PERF-PROFILE.md` 定量的两个大头：

1. 遥测/日志同步落盘：`log/slog.(*Logger).Info` cum 16.22%/17.90%（c=64/c=256），其中 `os.(*File).Write` cum 15.18%/16.93%——每请求一条 span 日志同步 write。
2. 流式 chunk 解码全量反射：`decodeOne` 对每个 SSE chunk 全量 Unmarshal openai-go typed `ChatCompletionChunk`（apijson 9.77% + gjson 5.86%），alloc_space 占比 ≈15%。

## 方法

- **基准先行**：`internal/adapter/chatchunk_test.go`（decodeOne 热路径，真实样本 + 构造 chunk）与 `internal/telemetry/batch_test.go`（slog 落盘，-benchmem）。
- **真基线获取**：解码实现改完后才补跑基线（流程偏差）⇒ 采取「精确逆向回改前文本 → 跑基线 → cp 恢复新实现」取真基线（⛔ 不用复刻假基线；恢复后 `go test -count=1 ./internal/adapter/` 全绿 + 文件 diff 校验）。
- **前后同口径**：同一 bench 函数、同一负载集、同一机器（Apple M1 Max / darwin arm64 / go1.27.3），各 `-count=3`。
- **帧语义零变化对照**：畸形/异型 chunk 容忍口径用临时探针在改前、改后各跑一次逐字 diff（探针测完即删，输出留 `raw/probe_*.log`）。

## 原始数据（raw/）

| 文件 | 内容 |
| --- | --- |
| `bench_decode_before.log` | 五家 decodeOne 基线（改前 typed 全量解码），-benchmem ×3 |
| `bench_decode_after.log` | 五家 decodeOne 改后（轻量两段式），同口径 ×3 |
| `bench_logsync_before.log` | slog TextHandler 直写文件基线（每条一次 write）×3 |
| `bench_log_after.log` | sync（未变路径）+ BatchHandler 批量路径 同轮 ×3 |
| `probe_old_tolerance.log` | 改前 openai-go 解码容忍口径探针（11 例逐字输出） |
| `probe_new_tolerance.log` | 改后同探针输出（与旧 diff：仅 PROBE 7 指针地址与 PROBE 10 重复键两处不同） |
| `test_full_after.log` | `go test -race -count=1 ./...` 全量（15 包 ok） |

## 结论

- chunk 解码：五家 ns/op 降至约 1/4~1/6，B/op 降至约 1/12~1/15，allocs/op 52~55 → 4~7（逐字数字见 `bench_decode_*.log`）。
- 日志落盘：每条 slog 同步 8.552~8.839µs → 批量 4.625~4.985µs（amortized，含收口 Shutdown），且写盘移出请求 goroutine。
- 帧语义：11 例容忍探针 10 例逐字一致（PROBE 7 仅内存地址打印不同）；唯一语义残差=同一对象重复键（PROBE 10，旧 first-wins / 新 last-wins，RFC 8259 行为未定义），已留钉子测试。

## 复现

见 `run_bench.sh`（逐条命令同上，无隐藏步骤）。
