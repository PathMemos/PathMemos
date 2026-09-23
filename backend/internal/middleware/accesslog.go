package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// statusRecorder 包装 ResponseWriter，捕获实际写入的状态码供访问日志使用。
// 未显式调用 WriteHeader 时默认 200。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush 透传 http.Flusher。SSE 路由（/ai/chat）依赖该断言，
// 若包装 writer 丢失 Flush，流式接口会直接返回 500 "streaming not supported"。
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// AccessLogMiddleware 为每个请求输出一条 INFO 结构化访问日志（含路由模板 route 与耗时 duration_ms），
// 用于数据问题复盘与 SLO 统计（scripts/accesslog-p95.sh 读取）。
// 挂到鉴权中间件之后时 user_id 已写入 context；挂到公开路由时 user_id 为空。
func AccessLogMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			elapsed := time.Since(start)
			route := ""
			if rc := chi.RouteContext(r.Context()); rc != nil {
				route = rc.RoutePattern()
			}
			logger.Info("api access",
				slog.String("request_id", RequestID(r.Context())),
				slog.String("user_id", UserID(r.Context())),
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.String("path", r.URL.Path),
				slog.String("query", r.URL.RawQuery),
				slog.Int("status", rec.status),
				slog.Int64("duration_ms", elapsed.Milliseconds()),
				slog.Duration("duration", elapsed),
				slog.String("ip", r.RemoteAddr),
			)
		})
	}
}
