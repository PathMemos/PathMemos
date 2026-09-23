// Package redis provides Redis client initialization helpers.
package redis

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"papafeiji/backend/internal/pkg/safe"

	"github.com/redis/go-redis/v9"
)

func NewClient(addr string) (*redis.Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("redis addr is empty")
	}

	var opt *redis.Options
	if strings.HasPrefix(addr, "redis://") || strings.HasPrefix(addr, "rediss://") {
		parsed, err := redis.ParseURL(addr)
		if err != nil {
			return nil, fmt.Errorf("parse redis url: %w", err)
		}
		opt = parsed
	} else {
		opt = &redis.Options{
			Addr:     addr,
			Password: os.Getenv("REDIS_PASSWORD"),
		}
	}
	opt.PoolSize = 50
	if v := os.Getenv("REDIS_POOL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 10 {
				opt.PoolSize = 10
			} else {
				opt.PoolSize = n
			}
		}
	}
	opt.MinIdleConns = 5
	opt.DialTimeout = 5 * time.Second
	opt.ReadTimeout = 3 * time.Second
	opt.WriteTimeout = 3 * time.Second

	rdb := redis.NewClient(opt)

	// B6a-08：启动时异步 Ping，失败仅告警不阻断启动；连接池与请求级重试自愈。
	safe.Go(context.Background(), nil, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			slog.Warn("redis startup ping failed", slog.Any("error", err))
		}
	})

	return rdb, nil
}
