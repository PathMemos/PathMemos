package ai

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// R-08：SSE 连接上限（单用户与全局）与释放复用。
func TestSSEConnLimiter(t *testing.T) {
	l := newSSEConnLimiter(3)
	if !l.acquire("u1") || !l.acquire("u1") {
		t.Fatal("per-user first two should pass")
	}
	if l.acquire("u1") {
		t.Fatal("per-user third should be rejected")
	}
	if !l.acquire("u2") {
		t.Fatal("global slot for u2 should pass")
	}
	if l.acquire("u3") {
		t.Fatal("global limit should reject u3")
	}
	l.release("u1")
	if !l.acquire("u3") {
		t.Fatal("released global slot should be reusable")
	}
}

// R-08：心跳写入 `: heartbeat`，且 done 之后不再发心跳。
func TestLockedSSEWriterHeartbeat(t *testing.T) {
	rec := httptest.NewRecorder()
	lw := &lockedSSEWriter{w: rec, f: rec}
	lw.heartbeat()
	if got := strings.Count(rec.Body.String(), ": heartbeat"); got != 1 {
		t.Fatalf("want 1 heartbeat, got %d (%q)", got, rec.Body.String())
	}
	lw.done()
	lw.heartbeat()
	if got := strings.Count(rec.Body.String(), ": heartbeat"); got != 1 {
		t.Fatalf("heartbeat after done must be suppressed, got %d", got)
	}
	if !strings.Contains(rec.Body.String(), "event: done") {
		t.Fatalf("done event missing: %q", rec.Body.String())
	}
}
