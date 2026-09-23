package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

type recordCapturer struct{ records []slog.Record }

func (h *recordCapturer) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordCapturer) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *recordCapturer) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordCapturer) WithGroup(string) slog.Handler      { return h }

// 防回归：SSE 路由挂 AccessLogMiddleware 后，包装 writer 必须保留 http.Flusher，
// 否则 /ai/chat 的 http.Flusher 断言失败，直接返回 500 "streaming not supported"。
func TestAccessLogPreservesFlusher(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var gotFlusher bool
	h := AccessLogMiddleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, gotFlusher = w.(http.Flusher)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/ai/chat", nil))
	if !gotFlusher {
		t.Fatal("AccessLogMiddleware must preserve http.Flusher for SSE streaming")
	}
}

// 访问日志必须带路由模板 route 与毫秒耗时 duration_ms，供 scripts/accesslog-p95.sh 统计。
func TestAccessLogRouteAndDurationMS(t *testing.T) {
	var cap recordCapturer
	logger := slog.New(&cap)
	router := chi.NewRouter()
	router.Use(AccessLogMiddleware(logger))
	router.Get("/diary/{id}", func(http.ResponseWriter, *http.Request) {})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/diary/abc", nil))

	if len(cap.records) != 1 {
		t.Fatalf("want 1 access record, got %d", len(cap.records))
	}
	attrs := map[string]slog.Value{}
	cap.records[0].Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value; return true })
	if got := attrs["route"].String(); got != "/diary/{id}" {
		t.Fatalf("route = %q, want /diary/{id}", got)
	}
	if _, ok := attrs["duration_ms"]; !ok {
		t.Fatal("missing duration_ms attribute")
	}
}
