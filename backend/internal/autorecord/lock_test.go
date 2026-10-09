package autorecord

import (
	"context"
	"errors"
	"testing"
)

// stubLocker 进程内桩锁，模拟 advisory lock 的 TryLock 语义（仅测试用）。
type stubLocker struct {
	ok    bool
	err   error
	calls int
}

func (s *stubLocker) TryLock(context.Context, string) (bool, string, error) {
	s.calls++
	return s.ok, "token", s.err
}

func (s *stubLocker) TryLocks(_ context.Context, _ []string) (bool, map[string]string, error) {
	return s.ok, map[string]string{}, s.err
}

func (s *stubLocker) Unlock(context.Context, string, string) error { return nil }

func (s *stubLocker) UnlockMany(context.Context, map[string]string) error { return nil }

// TestProcessUser_LockBusySkipsNotFails：用户级锁被占用（手动即时成文并发持有）时
// 跳过该用户返回 nil，不计入 failed——正常并发不应触发 alert=auto_record_failed
// 噪音告警；锁被占用路径不得触达 DB（pool 为 nil，触达即 panic）。
func TestProcessUser_LockBusySkipsNotFails(t *testing.T) {
	l := &stubLocker{ok: false}
	s := &Service{lock: l}
	if err := s.processUser(context.Background(), "user-1"); err != nil {
		t.Fatalf("lock busy must skip with nil error, got %v", err)
	}
	if l.calls != 1 {
		t.Fatalf("expected exactly 1 TryLock call, got %d", l.calls)
	}
}

// TestProcessUser_LockErrorReturnsError：锁获取本身出错（DB 故障）仍返回 error，
// 计入轮次失败告警（真实故障必须可见）。
func TestProcessUser_LockErrorReturnsError(t *testing.T) {
	s := &Service{lock: &stubLocker{ok: false, err: errors.New("db down")}}
	if err := s.processUser(context.Background(), "user-1"); err == nil {
		t.Fatal("lock acquisition error must return error")
	}
}
