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

## OPT-03 各环节 pprof 画像 + 并发阶梯基线 —— ⏳ 进行中（W-perf1）

> 产出：`20261114-realkeys-mimo-flash-bench/PERF-PROFILE.md`（阶梯 1→256、CPU/alloc 各环节归因、优化建议排序）。画像出来后逐项追加 OPT-04 起的条目。

## OPT-04 优化技术调研（联网） —— ⏳ 进行中（W-perf2）

> 产出：`docs/20261114-性能优化-调研-Go网关.md`（每条带来源 URL + 原话要点 + 落地清单）。

<!-- 后续 OPT-NN 条目随实施逐条追加，每条六要素齐全；⛔ 不删旧条目（被推翻的加注不删）。 -->
