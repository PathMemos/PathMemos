package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"papafeiji/backend/internal/db/sqlc"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type TxFn func(ctx context.Context, q *sqlc.Queries) error

// TxBeginner 抽象能开启事务的数据库连接/连接池。
// *pgxpool.Pool 与 pgxmock（单元测试）均实现该接口，使事务流程可测试。
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

func WithTx(ctx context.Context, pool TxBeginner, fn TxFn) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			//nolint:errcheck
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
	}()

	if err := fn(ctx, sqlc.New(tx)); err != nil {
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil {
			return fmt.Errorf("rollback failed: %w (original: %w)", rbErr, err)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		// Commit 报错后事务状态未知，补一次 Rollback 收尾（幂等），避免遗留未结束事务。
		_ = tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
		return fmt.Errorf("commit tx: %w", err)
	}

	return nil
}

func WithTxDeferrable(ctx context.Context, pool TxBeginner, fn TxFn) error {
	const maxRetries = 3
	const retryBackoff = 50 * time.Millisecond
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		err := runTxDeferrableOnce(ctx, pool, fn)
		if err == nil {
			return nil
		}
		if !isSerializationFailure(err) {
			return err
		}
		lastErr = err
		time.Sleep(retryBackoff)
	}
	return fmt.Errorf("deferrable tx failed after %d attempts: %w", maxRetries, lastErr)
}

func runTxDeferrableOnce(ctx context.Context, pool TxBeginner, fn TxFn) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:       pgx.RepeatableRead,
		DeferrableMode: pgx.Deferrable,
	})
	if err != nil {
		return fmt.Errorf("begin deferrable tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			//nolint:errcheck
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
	}()

	if err := fn(ctx, sqlc.New(tx)); err != nil {
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil {
			return fmt.Errorf("rollback failed: %w (original: %w)", rbErr, err)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
		return fmt.Errorf("commit tx: %w", err)
	}

	return nil
}

func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001"
	}
	return false
}
