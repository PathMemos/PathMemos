// Package db provides database connection, transaction, and locking utilities.
package db

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool 创建 API 请求池：语句超时 30s；上限 DB_MAX_CONNS（默认 50），下限 DB_MIN_CONNS（默认 10）。
func NewPool(databaseURL string) (*pgxpool.Pool, error) {
	return newPool(databaseURL, true, intEnv("DB_MAX_CONNS", 50, 5), intEnv("DB_MIN_CONNS", 10, 1))
}

// NewBackgroundPool 创建后台任务池：语句不超时；使用独立上限 DB_BG_MAX_CONNS（默认 10）
// 与下限 DB_BG_MIN_CONNS（默认 2），不与请求池共用 DB_MAX_CONNS/DB_MIN_CONNS，
// 保证「请求池 + 后台池」的连接预算可被 scripts/check-conn-budget.sh 精确校验。
func NewBackgroundPool(databaseURL string) (*pgxpool.Pool, error) {
	return newPool(databaseURL, false, intEnv("DB_BG_MAX_CONNS", 10, 1), intEnv("DB_BG_MIN_CONNS", 2, 1))
}

// intEnv 读取正整数环境变量；缺失、非法或小于 min 时返回 def。
func intEnv(key string, def, min int32) int32 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < int(min) {
		return def
	}
	return int32(n)
}

func newPool(databaseURL string, enableStatementTimeout bool, maxConns, minConns int32) (*pgxpool.Pool, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("database URL is empty")
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}

	if minConns > maxConns {
		minConns = maxConns
	}
	config.MaxConns = maxConns
	config.MinConns = minConns
	config.MaxConnLifetime = time.Hour
	config.MaxConnIdleTime = 30 * time.Minute
	config.ConnConfig.RuntimeParams["timezone"] = "Asia/Shanghai"
	if enableStatementTimeout {
		config.ConnConfig.RuntimeParams["statement_timeout"] = "30000"
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
