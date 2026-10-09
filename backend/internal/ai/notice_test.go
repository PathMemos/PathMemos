package ai

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// （02f AI-6）截断提示走 event: notice，终止性错误仍走 event: error，二者语义相反。
func TestWriteSSENotice_Format(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := writeSSENotice(rec, rec, `{"type":"truncated"}`); err != nil {
		t.Fatalf("writeSSENotice returned error: %v", err)
	}
	want := "event: notice\ndata: {\"type\":\"truncated\"}\n\n"
	if rec.Body.String() != want {
		t.Fatalf("unexpected SSE framing: %q, want %q", rec.Body.String(), want)
	}
}

func TestLockedSSEWriter_NoticeBeforeDone(t *testing.T) {
	rec := httptest.NewRecorder()
	lw := &lockedSSEWriter{w: rec, f: rec}
	lw.notice(`{"type":"truncated"}`)
	lw.done()
	body := rec.Body.String()
	if !strings.Contains(body, "event: notice\ndata: {\"type\":\"truncated\"}\n\n") {
		t.Fatalf("notice event missing: %q", body)
	}
	if !strings.Contains(body, "event: done") {
		t.Fatalf("done event missing: %q", body)
	}
	if strings.Index(body, "event: notice") > strings.Index(body, "event: done") {
		t.Fatalf("notice must precede done: %q", body)
	}
	// notice 不改变 closed 状态的语义体现在：done 之后再次 notice 被抑制。
	lw.notice(`{"type":"truncated"}`)
	if got := strings.Count(body, "event: notice"); got != 1 {
		t.Fatalf("notice after done must be suppressed, got %d", got)
	}
}
