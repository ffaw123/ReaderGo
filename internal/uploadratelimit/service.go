package uploadratelimit

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

const (
	keyVersion = "v1"
	// Lua 脚本只在请求到达时惰性补充令牌，不需要后台定时任务。
	tokenBucketScript = `
local capacity = tonumber(ARGV[1])
local refill_interval_ms = tonumber(ARGV[2])
local ttl_ms = tonumber(ARGV[3])

local now_ms
if ARGV[4] ~= nil then
  -- 第四个参数只供同包测试验证补充边界，生产调用始终使用 Redis TIME。
  now_ms = tonumber(ARGV[4])
else
  local redis_time = redis.call('TIME')
  now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
end

local tokens = tonumber(redis.call('HGET', KEYS[1], 'tokens'))
local last_refill_ms = tonumber(redis.call('HGET', KEYS[1], 'last_refill_ms'))

if tokens == nil or last_refill_ms == nil then
  tokens = capacity
  last_refill_ms = now_ms
else
  local elapsed_ms = math.max(0, now_ms - last_refill_ms)
  local refill_count = math.floor(elapsed_ms / refill_interval_ms)

  if refill_count > 0 then
    if tokens + refill_count >= capacity then
      tokens = capacity
      last_refill_ms = now_ms
    else
      tokens = tokens + refill_count
      last_refill_ms = last_refill_ms + refill_count * refill_interval_ms
    end
  end
end

local allowed = 0
local retry_after_ms = 0

if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry_after_ms = math.max(1, refill_interval_ms - (now_ms - last_refill_ms))
end

redis.call('HSET', KEYS[1],
  'tokens', tokens,
  'last_refill_ms', last_refill_ms)
redis.call('PEXPIRE', KEYS[1], ttl_ms)

return {allowed, tokens, retry_after_ms}
`
)

// Result 描述一次令牌消费结果，RetryAfter 只在拒绝时有意义。
type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

type scriptStore interface {
	Key(parts ...string) string
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
}

// Service 使用同一 Redis key 协调所有 API 实例的用户上传额度。
type Service struct {
	cache          scriptStore
	capacity       int
	refillInterval time.Duration
	keyTTL         time.Duration
}

func New(cacheStore scriptStore, capacity int, refillInterval, keyTTL time.Duration) *Service {
	return &Service{cache: cacheStore, capacity: capacity, refillInterval: refillInterval, keyTTL: keyTTL}
}

// Allow 原子补充并消耗一枚令牌；Redis 故障由 API 按失败关闭策略处理。
func (s *Service) Allow(ctx context.Context, tenantID string) (Result, error) {
	return s.allow(ctx, tenantID, nil)
}

func (s *Service) allow(ctx context.Context, tenantID string, testNowMS *int64) (Result, error) {
	if tenantID == "" {
		return Result{}, fmt.Errorf("tenant id must not be empty")
	}
	key := s.cache.Key("rate-limit", "upload", keyVersion, tenantID)
	args := []any{s.capacity, s.refillInterval.Milliseconds(), s.keyTTL.Milliseconds()}
	if testNowMS != nil {
		args = append(args, *testNowMS)
	}
	raw, err := s.cache.Eval(ctx, tokenBucketScript, []string{key}, args...)
	if err != nil {
		return Result{}, fmt.Errorf("consume upload rate limit token: %w", err)
	}
	values, ok := raw.([]any)
	if !ok || len(values) != 3 {
		return Result{}, fmt.Errorf("unexpected upload rate limit result: %#v", raw)
	}
	allowed, err := resultInt64(values[0])
	if err != nil {
		return Result{}, err
	}
	remaining, err := resultInt64(values[1])
	if err != nil {
		return Result{}, err
	}
	retryAfterMS, err := resultInt64(values[2])
	if err != nil {
		return Result{}, err
	}
	if (allowed != 0 && allowed != 1) || remaining < 0 || remaining > int64(s.capacity) || retryAfterMS < 0 || (allowed == 0 && retryAfterMS == 0) {
		return Result{}, fmt.Errorf("invalid upload rate limit values: %#v", values)
	}
	return Result{Allowed: allowed == 1, Remaining: int(remaining), RetryAfter: time.Duration(retryAfterMS) * time.Millisecond}, nil
}

func resultInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case int:
		return int64(typed), nil
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err == nil {
			return parsed, nil
		}
	case []byte:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err == nil {
			return parsed, nil
		}
	}
	return 0, fmt.Errorf("unexpected upload rate limit integer: %#v", value)
}
