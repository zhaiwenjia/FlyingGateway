# 20261114 热路径基准（W-optB 编解码/计量优化）

## 一、问题

W-optB：先跑基准记基线，再只优化**测量证明是热点**的部分——热点 JSON 路径减少重复编解码
（RawMessage 直通、二次 Unmarshal 合并、Decoder.Token 按需读取舍）、[]byte↔string 削减、
Redact 分配、结构预分配。红线：归一化/费用口径零变化（testdata/real 断言零改动）、既有测试
全绿、⛔ 第三方库、⛔ 不动 internal/{server,stream,adapter,cmd}。

## 二、方法

- 基线/优化后同一命令（`run.sh`，`-bench=. -benchmem -count=5 -run='^$'`，6 包 38 项），
  原始输出 `bench_before.txt` / `bench_after.txt`；对比取每项 5 次中位数。
- 环境：darwin/arm64、go1.27.3、GOMAXPROCS=10；噪声带 ±1~2%（调研 3.1 口径）。
- ⚠️ bench_after 跑批期间有**并发会话在同一 worktree** 编译/测试（19:51、19:56 仍有写入），
  未改动路径整体 +2~3% 同向漂移，属机器负载差、非回归。
- ⚠️ 归属：19:45 起 codec/{chat,responses} 里的 `byteSink`（JSON 直写复用缓冲、返回值独立
  拷贝）是**并发写入者的改动**，不在本轮台账计功；bench_after 的两个 Encode 基准测到的是
  「本轮改动 + byteSink」合并态。

## 三、前后数据（中位数，完整 38 项见原始输出）

| 包 | 基准 | ns/op 前→后 | B/op 前→后 | allocs 前→后 | 归属 |
|---|---|---|---|---|---|
| security | Redact/clean-long-text | 703120→40948 (−94.2%) | 75728→0 | 21→0 | 本轮 |
| security | Redact/big-base64-token | 2005477→179227 (−91.1%) | 132802→24119 | 23→6 | 本轮 |
| security | Redact/json-form | 59118→61430 (+3.9%) | 5948→3697 | 37→32 | 本轮 |
| errors | FromUpstream/openai-429 | 47098→7381 (−84.3%) | 4715→1664 | 75→20 | 本轮（大头来自 Redact） |
| errors | FromUpstream/minimax-business | 31342→1687 (−94.6%) | 2977→376 | 70→7 | 本轮（同上） |
| cost | CalculateCost/flat | 152.0→60.1 (−60.4%) | 96→48 | 4→1 | 本轮 |
| cost | CalculateCost/tiered | 153.4→60.4 (−60.6%) | 96→48 | 4→1 | 本轮 |
| cost | CalculateCost/peak | 451.6→342.9 (−24.1%) | 128→80 | 6→3 | 本轮 |
| codec/chat | DecodeRequest/tools | 8467→8364 (±噪声) | 6069→4403 | 45→41 | 本轮（预分配） |
| codec/chat | DecodeRequest/small | 2204→2213 (±噪声) | 1417→1273 | 13→12 | 本轮（预分配） |
| codec/responses | DecodeRequest/item-array | 8328→8903 (漂移带内) | 4524→4091 | 32→30 | 本轮（预分配） |
| codec/chat | EncodeResponse | 1984→2030 (漂移带内) | 1072→1072 | 9→9 | 懒建 map 无可见收益 |
| codec/responses | EncodeResponse | 3231→3366 (漂移带内) | 2113→2113 | 19→19 | 懒建 map 无可见收益 |
| usage | Normalize ×16 项 | ±2~3% 同向漂移 | 不变 | 不变 | 形状分派合并无可见收益 |
| codec/chat | ChunkEncoderEncode | 841.6→729.2 (−13.4%) | 656→192 | 5→1 | ⚠️ byteSink（并发会话） |
| codec/responses | EventEncoderEncode | 459.3→379.8 (−17.3%) | 446→192 | 4→1 | ⚠️ byteSink（并发会话） |
| codec/{chat,responses} | ParseChunk / ParseEvent ×4 项 | 漂移带内 | 不变 | 不变 | 死代码，未优化 |

取舍数据（L5 决策，BenchmarkPickFields，同输入只摘 model/stream 两字段）：

| 做法 | ns/op | B/op | allocs/op |
|---|---|---|---|
| 整体 Unmarshal 进迷你结构体 | 3537 | 24 | 1 |
| json.Decoder.Token 流式按需读 | 6781 | 3704 | 168 |
| 整体 Unmarshal 进全量结构体（现状） | 8570 | 6071 | 45 |

## 四、结论

1. **Redact 预筛 + 按需跑正则 + 单次扫描重建**是本轮最大收益：无凭据文本 −94.2% 且零分配；
   凭据 json 形本体只省分配（allocs 37→32）、ns +3.9%——七模式全触发时是七条正则的固有成本。
2. **fmt.Sprintf → 字符串直拼**（快照标识 / minimax 码 / HTTP 消息）：计量主路径 −60%。
3. **Decoder.Token 按需读不划算**（168 allocs vs 1），mini-struct 按需解码 2.4× 快于全量——
   生产代码维持 RawMessage 直通 + 形状分派，⛔ 不引入 Token 流式读。
4. 二次 Unmarshal 合并（usage/errors 两处）行为逐字一致但**无可见收益**（失败解析不分配），
   保留不回退，如实记账。
5. 结构预分配削掉切片扩容（B −9~27%），ns 在漂移带内。
6. sync.Pool 池化输出缓冲放弃：与「返回 []byte 供调用方持有」的所有权契约冲突（五坑的
   双重归还/悬垂面）；编码器内复用缓冲已由并发会话的 byteSink 以「返回值独立拷贝」实现。

复现：`./run.sh`（输出即 before/after 同口径）。
