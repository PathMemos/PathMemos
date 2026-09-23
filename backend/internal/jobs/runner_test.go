package jobs

import (
	"context"
	"testing"
	"time"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/pkg/safe"
)

// R-03：任务注册表必须完整、名字唯一、失联阈值 > 0，且与运维 CLI 的任务名清单一致。
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
