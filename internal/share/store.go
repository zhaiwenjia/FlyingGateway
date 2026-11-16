// 日期: 2026-11-16 23:39:48.270
// 作者: Zhai Wenjia

// Package share 实现跨实例共享计数存储（统一 SDD 7.2 多实例一致性）：
// 限流令牌桶、预算累计、冷却状态的计数外置，多实例共享一份配额（⛔ 不再是 N 实例 = N 倍配额）。
//
// 两个实现，同一语义（可插拔，不启用 Redis ⇒ MemoryStore 单实例零损失）：
//   - MemoryStore：进程内内存（单实例形态，语义与 RedisStore 逐条对齐）；
//   - RedisStore：Redis 共享（HASH 令牌桶 + Lua 原子扣减，官方 token bucket 口径：
//     状态 `{tokens, last_ms}` 存 HASH、refill + consume 单脚本原子、`redis.call('TIME')`
//     统一时钟 ⛔ 不信任各实例本地时钟）。
//
// 故障口径（显式降级，⛔ 不静默）：Redis 不可用（连接失败 / 超时 / 协议错）时
// RedisStore 返回 error，由调用方决定降级（回退本地内存 + warn 日志）或拒绝；
// ⛔ 不在本层悄悄吞错假装扣减成功。
package share

import "time"

// Take 是一组令牌桶扣减请求（统一 SDD 7.2 双维「同时可付才扣」的共享形态）。
type Take struct {
	Key          string  // 桶键（全局唯一，调用方按 scope 前缀组织）
	Capacity     int64   // 桶深（token 或请求）
	RefillPerSec float64 // 补充速率（单位/秒）
	Need         int64   // 本次扣减量（令牌维 = 预估 token；请求维 = 1）
}

// Store 是跨实例共享计数存储。所有操作并发安全。
//
// 原子性契约：
//   - TryTake：全部可付才扣（⛔ 不半扣），任一不足整体拒绝且不扣任何桶；
//   - IncrBy：累计 delta，limit > 0 时超限 ⇒ **回滚**并报超限（⛔ 不超发后补记）；
//   - 冷却键独立于计数键，读写均以绝对时刻（until）表达。
type Store interface {
	TryTake(now time.Time, takes []Take) (bool, error)
	Get(key string) (int64, error)
	IncrBy(key string, delta, limit int64) (int64, error)
	SetCooldown(key string, until time.Time) error
	CooldownUntil(key string) (time.Time, error)
}
