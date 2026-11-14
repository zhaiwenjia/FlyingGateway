# 优化过程台账（FlyingGateway 性能与可靠性优化）

> **口径**（老宅 2026-11-14 指示「记录优化过程」）：每个优化项按六要素留痕——① **环节**（网关链路哪一段）② **现象与数据**（实测数字逐字）③ **根因** ④ **方案与取舍** ⑤ **前后对照**（同口径复测）⑥ **钉子/验证**（测试或复测命令）。⛔ 不只留结论不记过程；数字一律逐字来自实测输出。
> **关联**：`progress/2026-11-14.md`（事件流）· `20261114-realkeys-mimo-flash-bench/`（原始数据）· `docs/20261114-统一SDD实现-测试报告.md`（bug 台账 #12–#15）

## OPT-01 传输层连接池 churn（c=8 档瓶颈） —— ✅ 已修复

1. **环节**：上游执行/传输层（`internal/server/execute.go` 的 `installAuthTransport`）。
2. **现象与数据**：真实上游 3 轮中位 c=8 是唯一输档（网关 2.80 rps / P50 2017ms vs LiteLLM 3.16 / 1683ms）；单轮网关 c8 在 1.89–4.16 rps 荡。本地隔离实验（固定 300ms 假上游）：网关自身开销 +7~14ms/请求（≈4%）、LiteLLM +60~67ms（≈22%）。
3. **根因**：`http.DefaultTransport` 默认 `MaxIdleConnsPerHost=2`——中等并发下连接用完即关、下一波全量 TLS 重连；症状指纹「c=1 正常、c=8 塌、c=32 回升」吻合（c=1 无争用、c=32 全忙不回池）。
4. **方案与取舍**：传输层保活池 `MaxIdleConnsPerHost=100 / MaxIdleConns=200 / IdleConnTimeout=90s`（按最大并发档 32 留 3 倍余量）；单点修复，SDK 客户端同 `*http.Client` 全体受益；不改业务语义。
5. **前后对照**：网关 c8 单轮 2.80 → **4.16 rps**、P99 4709 → **2439ms**；真实上游 6 轮中位两侧打平（c8 2.72 vs 2.94、c32 6.39 vs 6.13），成功率恒 100%。
6. **钉子/验证**：`go test -race ./internal/server/` 全绿；隔离实验可复现（`scripts/fake_upstream.py` + `raw/gateway-local.json`）；commit 64b478e。

## OPT-02 超时误分类致重试被绕过（成功率口径） —— ✅ 已修复

1. **环节**：错误分类 → 可重试判定（`internal/server/execute.go` 的 `adapterDetail`）。
2. **现象与数据**：r2 轮 c=32 成功率 63/64——1 例上游单请求卡满 60s（`context deadline exceeded`），对外吐 `400 invalid_request/adapter_error`。
3. **根因**：兜底把传输超时归 `invalid_request`，而可重试集只认 `rate_limit/timeout/upstream_error` ⇒ 重试被静默绕过（分类是治理链的路由表，错一处全链错）。
4. **方案与取舍**：兜底改 `errors.FromTransportError` 传输语义三分（timeout⇒可重试 / cancelled⇒⛔ 不重试 / 其余⇒upstream_error）；修在分类层而非重试层打补丁。
5. **前后对照**：修复后 r4–r7 全档成功率 100%（64/64 亦然）。
6. **钉子/验证**：`TestAdapterDetailTimeoutRetryable`（分类）+ `TestTimeoutRetriedThenSucceeds`（端到端：首请求卡死→重试→200、上游恰 2 次）；commit af11d10。

## OPT-03 各环节 pprof 画像 + 并发阶梯基线 —— ✅ 已完成（见 PERF-PROFILE.md）

> 产出：`20261114-realkeys-mimo-flash-bench/PERF-PROFILE.md`（阶梯 1→256、CPU/alloc 各环节归因、优化建议排序）。画像出来后逐项追加 OPT-04 起的条目。

## OPT-04 优化技术调研（联网） —— ✅ 已完成（见 docs/20261114-性能优化-调研-Go网关.md）

> 产出：`docs/20261114-性能优化-调研-Go网关.md`（每条带来源 URL + 原话要点 + 落地清单）。



## OPT-05 SSE/NDJSON 扫描器缓冲池化 —— ✅ 已完成

1. **环节**：上游流解析（`internal/stream/{sse,ndjson}.go`，归还钩子挂 `internal/adapter/httpstream.go` 两迭代器 `close()`）。
2. **现象与数据**：画像 `NewSSEScanner` 占 alloc_space **54.21%**（每流 64KB 固定缓冲 ≈59KB/请求）；基线 SSE chat_1000 事件 450084 B/op · 2053 allocs。
3. **根因**：每流 64KB 初始缓冲一次性丢弃；逐行 Text() 拷贝 + `Data +=` 拼接。
4. **方案**：sync.Pool 借还（Put 前 Reset、cap>1MB 丢弃、幂等 Release；GOROOT 证实 EOF 后 Scan 惰性、归还安全）+ Bytes() 零拷贝取行 + parts 累积单次 Join。
5. **前后**：SSE chat_1000 −31% ns / −57% B / −51% allocs（450KB→192KB、2053→1004）；NDJSON 65664→**161** B/op。
6. **钉子**：阈值闸/Reset/别名安全/Release 幂等 5 测试；全量 -race 绿。证据 `20261114-realkeys-mimo-flash-bench/PERF-PROFILE.md` 相关 bench。

## OPT-06 帧编码器逐帧减分配 —— ✅ 已完成

1. **环节**：客户端侧帧编码（`internal/codec/{chat,responses}`）。
2. **数据**：ChunkEncoder 656 B · 5 allocs · 836ns；EventEncoder 445 B · 4 allocs · 458ns。
3. **根因**：装配结构装箱逃逸 + 切片字面量逐帧新建 + json.Marshal 中间 []byte。
4. **方案**：装配结构帧间复用（整值替换防脏）+ json.Encoder 直写复用缓冲；返回值仍独立拷贝（「可持有」契约不变）。
5. **前后**：两编码器 **5/4→1 allocs**、656/445→192/191 B、ns −13%/−19%。
6. **钉子**：既有逐字节载荷断言全绿（输出字节不变）。

## OPT-07 flush 策略实证（维持逐帧） —— ✅ 已实证

1. **环节**：帧泵客户端写出（`internal/server/respond.go`）。
2. **数据**：慢产者（LLM 帧率形态）下聚合 8/32/仅结尾帧把逐帧交付延迟恶化 **×92/×255/×600**，TTFB 却无差（首帧即刷）。
3. **结论**：**维持逐帧 flush（TTFB 优先）**，聚合不采；`flush()` 实测 0 B 0 allocs（逃逸分析内联）无需改。
4. **钉子**：`BenchmarkFlushCall`/`BenchmarkFlushPolicy` 留档可复现。

## OPT-08 流式 chunk 解码去全量反射 —— ✅ 已完成

1. **环节**：五家 chat chunk 解码器（qwen/moonshot/zhipu/deepseek/xiaomi）。
2. **数据**：画像 apijson+gjson 占 alloc 14.85%；基线每 chunk 8.5–10.4µs / 7.3–8KB / 52–55 allocs。
3. **根因**：每帧 openai-go typed 全量 Unmarshal，帧解码只消费 5 处字段。
4. **方案**：`internal/adapter/chatchunk.go` 轻量单趟按需解码 + **两段式兜底**（严格失败回落 typed，宽松容忍面全保留；唯一残差=RFC 未定义的重复键口径，留钉子）。
5. **前后**：ns **0.16–0.22×**、B/op **≈0.08×**、allocs **52–55 → 4–7**；探针 11 例对照 10 例逐字一致。
6. **钉子**：强制转换容错/畸形帧/usage 透传/重复键 4 测试。

## OPT-09 遥测日志批量异步落盘 —— ✅ 已完成

1. **环节**：遥测/日志出口（`internal/telemetry/batch.go` + `cmd/gateway` 装配）。
2. **数据**：画像 slog.Info cum **16–18% CPU**（每请求一次同步 write 落在请求 goroutine）；基线每条 8.74µs。
3. **方案**：BatchHandler（有界队列 4096 + 后台批量 + Shutdown 收口）+ BatchWriter 32KB 合并写；溢出=丢弃+**三通道显式计数**（⛔ 不静默丢、⛔ 不阻塞热路径）。
4. **前后**：每条 8740→**4671 ns**（≈47%↓）**且移出请求 goroutine**。
5. **钉子**：丢弃显式/收口排空/批量合并/io.Discard 口径 5 测试。

## OPT-10 Redact 出口层遮蔽快路径 —— ✅ 已完成

1. **环节**：出口层遮蔽（`internal/security/redact.go`）。
2. **数据**：基线 clean 4.4KB 文本 **703µs / 75.7KB / 21 allocs**（无凭据也全量跑 7 遍正则）。
3. **方案**：七模式触发必要条件预筛（无匹配 0 分配返回）+ 按需跑正则 + 单次扫描重建；Unicode case-fold 触发边界逐例实测（U+0130/Kelvin/长 s）。
4. **前后**：clean **40.9µs / 0 B / 0 allocs**（−94.2%）；大 token 2.0ms→179µs（−91.1%）。
5. **钉子**：真实凭据五形态既有断言全绿 + 触发覆盖 6 例。

## OPT-11 计量/错误热路径削减 —— ✅ 已完成

1. **环节**：`internal/cost`（SnapshotID/FromUSDTicks 去 fmt）+ `internal/errors`（二次 Unmarshal 合并）。
2. **前后**：CalculateCost 152→**60ns**（−60%、4→1 allocs）；FromUpstream 47µs→7.4µs（−84%，大头归 OPT-10 不贪功）。
3. **钉子**：输出逐字节一致断言 + 边界行为实测（json 形状分派与「先试后试」逐字等价）。

## OPT-12 无收益项（如实入账，⛔ 不硬凑） —— ✅ 已记

- usage cache_write_tokens 二次解码合并：**无可见收益**（保留，不回退）。
- collectDegradations 懒建 map：**无可见收益**（小 map 编译器栈上化）。
- Decoder.Token 流式按需读：**负结果**——比 mini-struct 慢 2.4×、分配多 168×，不采用（基准留档）。
- ParseChunk/ParseEvent：生产路径零调用（死代码），不投入优化。

## OPT-13 超高并发等价模拟（1k→100k vs LiteLLM） —— ✅ 已完成

1. **环节**：全链路容量（`20261114-ultra-concurrency-sim/`，长持在途风暴 30s）。
2. **数据（核心对比）**：FlyingGateway 成功在途封顶 **~16–17k**（优雅 502、进程存活）vs LiteLLM **~2k**（RST 丢连接）；TTFB **1.7–2.4ms vs 654–721ms（~300×）**；RSS **146KB vs 450KB**/连接；风暴后恢复 **1.5ms（100k 过载后 123ms）vs 267ms→4.9s**。
3. **决定性发现**：网关出站本地端口 **bind 排他**（Go Dialer 无 SO_REUSEADDR）⇒ 单源 IP 出站上限=临时端口池 **16384**，多上游端口**不能**乘算（实测 25k 档 `200=16288、502=5510`）；100k 到不了的卡点即它，非 CPU/内存/fd（fd 预算 400k<491520）。
4. **部署建议（开源文档素材）**：多源 IP / 多网关实例横向扩出站；fd 预算 ≈ 2×在途；`somaxconn=128` ⇒ 连接爬坡；溢出语义取「显式 502」优于 RST。
5. **等价性边界**：等价=在途状态/连接数/调度/GC/拒绝语义；不等价=真实上游方差、公网抖动、多机；100k 外推为线性外推（1k–10k 干净区斜率）非实测，如实标注。
6. **钉子**：压测器/编排/绘图脚本可重跑；raw/ 37 份 JSON + 5 图；`gofmt/vet/build` 全模块绿（CI 门禁未破）。
