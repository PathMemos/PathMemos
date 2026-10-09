package jobs

import (
	"context"
	"testing"
	"time"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/file"
	"papafeiji/backend/internal/pkg/safe"

	"github.com/pashagolub/pgxmock/v4"
)

// 任务注册表必须完整、名字唯一、失联阈值 > 0，且与运维 CLI 的任务名清单一致。
func TestJobSpecsRegistry(t *testing.T) {
	r := &Runner{cfg: &config.Config{}, jobs: map[string]time.Duration{}}
	specs := r.specs()
	if len(specs) != len(KnownJobNames()) {
		t.Fatalf("specs=%d, KnownJobNames=%d", len(specs), len(KnownJobNames()))
	}
	seen := map[string]bool{}
	for _, s := range specs {
		if s.name == "" || s.lockKey == "" || s.run == nil {
			t.Fatalf("incomplete spec: %+v", s)
		}
		if seen[s.name] {
			t.Fatalf("duplicate job name %q", s.name)
		}
		seen[s.name] = true
		if s.staleAfter() <= 0 {
			t.Fatalf("job %s staleAfter must be > 0", s.name)
		}
	}
	for _, name := range KnownJobNames() {
		if !seen[name] {
			t.Fatalf("KnownJobNames has %q but specs does not", name)
		}
	}
	if got := jobName(lockAutoRecord); got != "auto_record" {
		t.Fatalf("jobName = %q, want auto_record", got)
	}
}

func TestGoWithRecover_PanicNotifiesChannel(t *testing.T) {
	ctx := context.Background()
	done := make(chan error, 1)

	safe.GoWithRecover(ctx, nil, func() error {
		panic("intentional panic for test")
	}, func(panicErr error) {
		done <- panicErr
	})

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil panic error")
		}
		if err.Error() == "" {
			t.Fatal("expected non-empty error message")
		}
		t.Logf("panic correctly notified: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for panic notification — goroutine deadlocked")
	}
}

func TestGoWithRecover_NormalCompletion(t *testing.T) {
	ctx := context.Background()
	done := make(chan error, 1)

	safe.GoWithRecover(ctx, nil, func() error {
		done <- nil
		return nil
	}, func(panicErr error) {
		done <- panicErr
	})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil error, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for normal completion")
	}
}

func TestGoWithRecover_ErrorReturned(t *testing.T) {
	ctx := context.Background()
	done := make(chan error, 1)

	testErr := context.DeadlineExceeded
	safe.GoWithRecover(ctx, nil, func() error {
		done <- testErr
		return testErr
	}, func(panicErr error) {
		done <- panicErr
	})

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}

func TestGoWithRecover_NoChannelWithoutPanicAndNoResult(t *testing.T) {
	ctx := context.Background()

	safe.GoWithRecover(ctx, nil, func() error {
		return nil
	}, nil)

	time.Sleep(100 * time.Millisecond)
	// Should not panic — onPanic is nil, fn returns nil, no channel needed
}

// TestRunCleanupOrphanTrajMaps_ScanExemptsInviteQRAssets 系统文件清理扫描必须携带
// invite 资产豁免条件（NOT metadata ? 'raw'）——邀请二维码/分享海报已分发到微信会话，
// 不得按 7 天窗口回收（否则图片 URL 变 404）；若 SQL 豁免条件被移除（sqlc 重生成后查询
// 文本变化），本测试的正则匹配即失败。
func TestRunCleanupOrphanTrajMaps_ScanExemptsInviteQRAssets(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	r := &Runner{bgPool: db.NewPoolWithDBTX(mock), storage: &file.Storage{}}

	// 首批即空行：任务直接返回；查询文本特征由正则断言（豁免条件在 WHERE 中）。
	mock.ExpectQuery(`NOT COALESCE\(metadata \? 'raw', false\)`).
		WithArgs("").
		WillReturnRows(pgxmock.NewRows([]string{"id", "path", "storage_type"}))

	if err := r.runCleanupOrphanTrajMaps(context.Background()); err != nil {
		t.Fatalf("runCleanupOrphanTrajMaps: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("scan query must carry invite QR exemption (NOT metadata ? 'raw'): %v", err)
	}
}
