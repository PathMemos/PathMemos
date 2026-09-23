package ai

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"papafeiji/backend/internal/middleware"
	"papafeiji/backend/internal/pkg/safe"
	"papafeiji/backend/pkg/errors"

	"github.com/go-chi/chi/v5"
)

const (
	maxMessageCodePoints = 2500
	AIStreamTimeout      = 180 * time.Second
	maxBackgroundEntries = 2000
	backgroundCacheTTL   = 30 * time.Minute
	dailyQuotaNonVIP     = 10
	dailyQuotaVIP        = 100
	dailyQuotaKeyPrefix  = "ai:daily_chat"
	maxChatBodySize      = 32 * 1024 // 32KB，足以覆盖 2500 code points 及 JSON 开销

	// R-08：SSE 心跳与并发连接上限（单轮本身受 AIStreamTimeout=180s 约束，无跨轮长连接）。
	sseHeartbeatInterval = 25 * time.Second
	sseMaxPerUser        = 2
	defaultSSEMaxConns   = 200
)

// requestIDPattern 限制客户端幂等标识格式：8~64 位字母数字/下划线/连字符。
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

type Handler struct {
	router  chi.Router
	service *Service
}

func NewHandler(router chi.Router, service *Service) *Handler {
	return &Handler{
		router:  router,
		service: service,
	}
}

func (h *Handler) Register() {
	h.router.Post("/ai/chat", h.Chat)
}

// --- R-08 SSE 连接治理 ---

type sseConnLimiter struct {
	mu      sync.Mutex
	global  int
	max     int
	perUser map[string]int
}

func newSSEConnLimiter(max int) *sseConnLimiter {
	return &sseConnLimiter{max: max, perUser: map[string]int{}}
}

func (l *sseConnLimiter) acquire(userID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 && l.global >= l.max {
		return false
	}
	if l.perUser[userID] >= sseMaxPerUser {
		return false
	}
	l.global++
	l.perUser[userID]++
	return true
}

func (l *sseConnLimiter) release(userID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.global > 0 {
		l.global--
	}
	if l.perUser[userID] > 1 {
		l.perUser[userID]--
	} else {
		delete(l.perUser, userID)
	}
}

func sseMaxConnsFromEnv() int {
	v := os.Getenv("SSE_MAX_CONNS")
	if v == "" {
		return defaultSSEMaxConns
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultSSEMaxConns
	}
	return n
}

var chatLimiter = newSSEConnLimiter(sseMaxConnsFromEnv())

// lockedSSEWriter 串行化数据/心跳/结束/错误写入，避免与心跳 goroutine 交错。
type lockedSSEWriter struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	f      http.Flusher
	closed bool
}

func (lw *lockedSSEWriter) data(chunk string) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return nil
	}
	return writeSSEData(lw.w, lw.f, chunk)
}

func (lw *lockedSSEWriter) heartbeat() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return
	}
	if _, err := fmt.Fprint(lw.w, ": heartbeat\n\n"); err == nil {
		lw.f.Flush()
	}
}

func (lw *lockedSSEWriter) done() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return
	}
	_ = writeSSEDone(lw.w, lw.f) //nolint:errcheck // stream close is best-effort
	lw.closed = true
}

func (lw *lockedSSEWriter) errorEvent(code, bizCode, message string) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return
	}
	_ = writeSSEError(lw.w, lw.f, code, bizCode, message) //nolint:errcheck // error already propagated as SSE event
	lw.closed = true
}

func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	if err := middleware.ReadJSONBody(w, r, &req, maxChatBodySize); err != nil {
		var maxBytesErr *http.MaxBytesError
		if stderrors.As(err, &maxBytesErr) {
			middleware.JSONError(w, r, http.StatusRequestEntityTooLarge, errors.CodeRequestEntityTooLarge, "request body too large")
		} else {
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "invalid request body")
		}
		return
	}

	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "message is required")
		return
	}
	if utf8.RuneCountInString(msg) > maxMessageCodePoints {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "message too long", errors.BizTextTooLong)
		return
	}
	// R4：request_id 为客户端幂等标识（断线重连复用），限定字符集与长度防止脏键/超大键。
	if req.RequestID != "" && !requestIDPattern.MatchString(req.RequestID) {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "invalid request_id")
		return
	}

	// R-08：超限在开流前返回 429；客户端断开时立即释放名额（上游仍消费到 EOF 写回放缓存）。
	if !chatLimiter.acquire(userID) {
		slog.WarnContext(ctx, "ai chat connection limit reached", slog.String("user_id", userID))
		middleware.JSONError(w, r, http.StatusTooManyRequests, errors.CodeTooManyRequests, "too many concurrent ai chat connections", errors.BizRateLimited)
		return
	}
	releaseOnce := sync.OnceFunc(func() { chatLimiter.release(userID) })
	defer releaseOnce()

	flusher, ok := w.(http.Flusher)
	if !ok {
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	lw := &lockedSSEWriter{w: w, f: flusher}
	stopHB := make(chan struct{})
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	safe.Go(ctx, nil, func() {
		defer hbWG.Done()
		t := time.NewTicker(sseHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-stopHB:
				return
			case <-t.C:
				lw.heartbeat()
			}
		}
	})
	defer func() { close(stopHB); hbWG.Wait() }()

	streamCtx, cancel := context.WithTimeout(ctx, AIStreamTimeout)
	defer cancel()

	_, err := h.service.Chat(streamCtx, userID, msg, req.RequestID, func(chunk string) error {
		if werr := lw.data(chunk); werr != nil {
			releaseOnce()
			return werr
		}
		return nil
	})
	if err != nil {
		if stderrors.Is(err, ErrAIDailyQuotaExceeded) {
			slog.WarnContext(ctx, "ai chat quota exceeded", slog.String("user_id", userID), slog.Any("error", err))
			lw.errorEvent(errors.CodeTooManyRequests, errors.BizAIDailyQuotaExceeded, "daily ai chat quota exceeded")
			return
		}
		if stderrors.Is(err, errAITurnInProgress) {
			slog.InfoContext(ctx, "ai turn in progress, reject reconnect", slog.String("user_id", userID))
			lw.errorEvent(errors.CodeTooManyRequests, errors.BizOperationInProgress, "ai turn in progress, retry later")
			return
		}
		if stderrors.Is(err, context.DeadlineExceeded) || stderrors.Is(err, errAIChatTimeout) {
			slog.ErrorContext(ctx, "ai chat timeout", slog.String("user_id", userID), slog.Any("error", err))
			lw.errorEvent(errors.CodeInternalError, "", "timeout")
			return
		}
		slog.ErrorContext(ctx, "ai chat upstream error", slog.String("user_id", userID), slog.Any("error", err))
		lw.errorEvent(errors.CodeInternalError, "", "upstream error")
		return
	}

	lw.done()
}
