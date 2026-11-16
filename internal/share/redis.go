// 日期: 2026-11-16 23:39:48.270
// 作者: Zhai Wenjia

package share

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// RedisStore 是跨实例共享计数存储（Redis 后端）。
//
// 口径（对齐 Redis 官方 token bucket / rate limiting 模式）：
//   - 令牌桶状态存 HASH（字段 tokens / last_ms），refill + consume 在**单个 Lua 脚本**里
//     原子完成（⛔ INCR 后 EXPIRE 的两步竞态）；
//   - 时钟统一用 `redis.call('TIME')` —— ⛔ 不信任各实例本地时钟（跨实例时钟漂移会让
//     refill 各算各的）；
//   - TryTake 的「全部可付才扣」在一个脚本内预检 + 统一写回（⛔ 不半扣）。
type RedisStore struct {
	c *Client
}

// NewRedisStore 构造 Redis 存储（addr 形如 host:port；password 空 = 无 AUTH）。
func NewRedisStore(addr, password string, timeout time.Duration, poolSize int) *RedisStore {
	return &RedisStore{c: NewClient(addr, password, timeout, poolSize)}
}

// ---- Lua 脚本（服务器端原子执行；注释即口径）----

// luaTryTake：N 桶「同时可付才扣」。KEYS = 桶键；ARGV 每桶三元组
// (capacity, refill_per_sec, need)。返回 1 = 已扣、0 = 某桶不足（整体未扣）。
const luaTryTake = `
local t = redis.call('TIME')
local now_ms = t[1] * 1000 + math.floor(t[2] / 1000)
local n = #KEYS
local pending = {}
for i = 1, n do
  local cap = tonumber(ARGV[i * 3 - 2])
  local refill = tonumber(ARGV[i * 3 - 1])
  local need = tonumber(ARGV[i * 3])
  local d = redis.call('HMGET', KEYS[i], 'tokens', 'last_ms')
  local tokens = tonumber(d[1])
  local last = tonumber(d[2])
  if tokens == nil then
    tokens = cap
    last = now_ms
  else
    local elapsed = (now_ms - last) / 1000.0
    if elapsed > 0 then
      tokens = math.min(cap, tokens + elapsed * refill)
    end
    last = now_ms
  end
  if tokens < need then
    return 0
  end
  pending[i] = {KEYS[i], tokens - need, last}
end
for i = 1, n do
  local p = pending[i]
  redis.call('HMSET', p[1], 'tokens', p[2], 'last_ms', p[3])
end
return 1
`

// luaIncrBy：预算原子累计，limit > 0 且超限 ⇒ 回滚。返回 {ok, value}：
// ok=1 已累计、ok=0 超限（value = 现值未动）。
const luaIncrBy = `
local v = redis.call('INCRBY', KEYS[1], ARGV[1])
local limit = tonumber(ARGV[2])
if limit > 0 and v > limit then
  redis.call('DECRBY', KEYS[1], ARGV[1])
  return {0, v - tonumber(ARGV[1])}
end
return {1, v}
`

// luaSetCooldown：冷却截止只增不减（⛔ 重复 429 缩短冷却）。KEYS=[cool键]，
// ARGV=[until_unix_nano]；now 取 TIME（毫秒）。返回 1 = 已写、0 = 保留旧值。
// ⚠️ 值存**纳秒**（与 MemoryStore 的 time.Time 精度对齐，⛔ 毫秒截断会让两实现
// 在同一断言下给出不同结果）；TTL 换算毫秒给 PX。
const luaSetCooldown = `
local t = redis.call('TIME')
local now_ms = t[1] * 1000 + math.floor(t[2] / 1000)
local new_until = tonumber(ARGV[1])
local old = redis.call('GET', KEYS[1])
if old and tonumber(old) > new_until then
  return 0
end
local ttl = math.max(1, math.floor((new_until / 1000000) - now_ms))
redis.call('SET', KEYS[1], ARGV[1], 'PX', ttl)
return 1
`

// TryTake 全部可付才扣（单脚本原子，⛔ 不半扣）。
func (r *RedisStore) TryTake(now time.Time, takes []Take) (bool, error) {
	if len(takes) == 0 {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := make([]string, 0, 2+len(takes)*3+3)
	args = append(args, "EVAL", luaTryTake, strconv.Itoa(len(takes)))
	for _, t := range takes {
		args = append(args, t.Key)
	}
	for _, t := range takes {
		args = append(args,
			strconv.FormatInt(t.Capacity, 10),
			strconv.FormatFloat(t.RefillPerSec, 'f', -1, 64),
			strconv.FormatInt(t.Need, 10))
	}
	rp, err := r.c.Do(ctx, args...)
	if err != nil {
		return false, err
	}
	return rp.kind == ':' && rp.num == 1, nil
}

// Get 返回预算累计值。
func (r *RedisStore) Get(key string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rp, err := r.c.Do(ctx, "GET", key)
	if err != nil {
		return 0, err
	}
	if rp.kind != '$' || rp.bulk == nil {
		return 0, nil
	}
	v, err := strconv.ParseInt(string(rp.bulk), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("share: 预算值解析失败 %q：%w", rp.bulk, err)
	}
	return v, nil
}

// IncrBy 原子累计；limit > 0 且超限 ⇒ 回滚并报超限。
func (r *RedisStore) IncrBy(key string, delta, limit int64) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rp, err := r.c.Do(ctx, "EVAL", luaIncrBy, "1", key,
		strconv.FormatInt(delta, 10), strconv.FormatInt(limit, 10))
	if err != nil {
		return 0, err
	}
	if rp.kind != '*' || len(rp.arr) != 2 {
		return 0, fmt.Errorf("share: IncrBy 回复形状非法")
	}
	ok, v := rp.arr[0].num, rp.arr[1].num
	if ok == 0 {
		return v, fmt.Errorf("share: 预算超限（key=%s 现值 %d + %d 超上限 %d，已回滚）", key, v, delta, limit)
	}
	return v, nil
}

// SetCooldown 记冷却截止（只增不减）。
func (r *RedisStore) SetCooldown(key string, until time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.c.Do(ctx, "EVAL", luaSetCooldown, "1", key,
		strconv.FormatInt(until.UnixNano(), 10))
	return err
}

// CooldownUntil 读冷却截止（无 ⇒ 零值）。
func (r *RedisStore) CooldownUntil(key string) (time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rp, err := r.c.Do(ctx, "GET", key)
	if err != nil {
		return time.Time{}, err
	}
	if rp.kind != '$' || rp.bulk == nil {
		return time.Time{}, nil
	}
	ns, err := strconv.ParseInt(string(rp.bulk), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("share: 冷却值解析失败 %q：%w", rp.bulk, err)
	}
	return time.Unix(0, ns), nil
}
