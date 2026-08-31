package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store 是业务缓存依赖的最小接口，便于功能模块测试时替换 Redis。
type Store interface {
	Key(parts ...string) string
	GetJSON(ctx context.Context, key string, dst any) (bool, error)
	SetJSON(ctx context.Context, key string, value any, ttl time.Duration) error
	Delete(ctx context.Context, keys ...string) error
	SetNX(ctx context.Context, key string, value any, ttl time.Duration) (bool, error)
}

// Client 复用 go-redis 连接池，并统一添加环境级 key 前缀。
type Client struct {
	raw       *redis.Client
	keyPrefix string
}

func NewRedis(ctx context.Context, rawURL, keyPrefix string) (*Client, error) {
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	keyPrefix = strings.Trim(keyPrefix, ":")
	if keyPrefix == "" {
		return nil, errors.New("redis key prefix must not be empty")
	}
	client := &Client{raw: redis.NewClient(options), keyPrefix: keyPrefix}
	// 启动阶段主动验证连接，避免首次上传时才发现 Redis 配置错误。
	if err := client.raw.Ping(ctx).Err(); err != nil {
		_ = client.raw.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}

func (c *Client) Key(parts ...string) string {
	items := make([]string, 0, len(parts)+1)
	items = append(items, c.keyPrefix)
	items = append(items, parts...)
	return strings.Join(items, ":")
}

func (c *Client) GetJSON(ctx context.Context, key string, dst any) (bool, error) {
	value, err := c.raw.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(value, dst); err != nil {
		return false, fmt.Errorf("decode redis value: %w", err)
	}
	return true, nil
}

func (c *Client) SetJSON(ctx context.Context, key string, value any, ttl time.Duration) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode redis value: %w", err)
	}
	return c.raw.Set(ctx, key, encoded, ttl).Err()
}

func (c *Client) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.raw.Del(ctx, keys...).Err()
}

func (c *Client) SetNX(ctx context.Context, key string, value any, ttl time.Duration) (bool, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, fmt.Errorf("encode redis value: %w", err)
	}
	return c.raw.SetNX(ctx, key, encoded, ttl).Result()
}

// Eval 原子执行由业务模块维护的 Lua 脚本；通用缓存层不解释脚本返回值。
func (c *Client) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return c.raw.Eval(ctx, script, keys, args...).Result()
}

func (c *Client) Close() error {
	return c.raw.Close()
}
