package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"papafeiji/backend/pkg/errors"
	"papafeiji/backend/pkg/util"

	"github.com/go-chi/chi/v5/middleware"
)

type responseEnvelope struct {
	Code       string      `json:"code"`
	BizCode    string      `json:"biz_code,omitempty"`
	Message    string      `json:"message"`
	Data       interface{} `json:"data,omitempty"`
	Extra      interface{} `json:"extra,omitempty"`
	Count      int         `json:"count,omitempty"`
	NextCursor string      `json:"nextCursor,omitempty"`
	RequestID  string      `json:"request_id"`
}

type ctxKeyRequestID struct{}

func RequestID(ctx context.Context) string {
	if rid, ok := ctx.Value(ctxKeyRequestID{}).(string); ok {
		return rid
	}
	if rid := middleware.GetReqID(ctx); rid != "" {
		return rid
	}
	return NewRequestID()
}

func WithRequestID(ctx context.Context, rid string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID{}, rid)
}

func NewRequestID() string {
	id, err := util.NewUUID()
	if err != nil {
		return ""
	}
	return id
}

func requestID(r *http.Request) string {
	rid := RequestID(r.Context())
	if rid == "" {
		// 优先复用客户端传入的 X-Request-ID，否则生成一次并注入 ctx，
		// 保证同一请求内多次取用稳定（避免每次生成新 UUID）。
		rid = util.SanitizeRequestID(r.Header.Get("X-Request-ID"))
		if rid == "" {
			rid = NewRequestID()
		}
		*r = *r.WithContext(WithRequestID(r.Context(), rid))
	}
	return rid
}

func JSON(w http.ResponseWriter, r *http.Request, status int, data interface{}) {
	JSONWithExtra(w, r, status, data, nil)
}

func JSONWithExtra(w http.ResponseWriter, r *http.Request, status int, data interface{}, extra interface{}) {
	resp := responseEnvelope{
		Code:      errors.CodeSuccess,
		Message:   "ok",
		Data:      data,
		Extra:     extra,
		RequestID: requestID(r),
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "json encode JSONWithExtra response failed", slog.Any("error", err))
	}
}

// JSONWithExtraAndCount 同时下发 extra 与 count（如 /diary/details 既需封面/记忆，
// 也需条目总数供前端分页判断）。
func JSONWithExtraAndCount(w http.ResponseWriter, r *http.Request, status int, data interface{}, extra interface{}, count int) {
	resp := responseEnvelope{
		Code:      errors.CodeSuccess,
		Message:   "ok",
		Data:      data,
		Extra:     extra,
		Count:     count,
		RequestID: requestID(r),
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "json encode JSONWithExtraAndCount response failed", slog.Any("error", err))
	}
}

func JSONWithPagination(w http.ResponseWriter, r *http.Request, status int, data interface{}, nextCursor string, count int) {
	resp := responseEnvelope{
		Code:       errors.CodeSuccess,
		Message:    "ok",
		Data:       data,
		Count:      count,
		NextCursor: nextCursor,
		RequestID:  requestID(r),
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "json encode pagination response failed", slog.Any("error", err))
	}
}

// JSONError 下发统一错误 envelope。若提供了 biz_code，则 biz_code 是 code 枚举与
// HTTP 状态的唯一权威：调用方显式传入的 code/status 会被其覆盖，从根本上保证
// 「同一 biz_code 全端点同一 HTTP 状态 + 同一 code」（ADR-0008）。
func JSONError(w http.ResponseWriter, r *http.Request, status int, code string, message string, bizCode ...string) {
	bc := ""
	if len(bizCode) > 0 {
		bc = bizCode[0]
	}
	if bc != "" {
		status = errors.HTTPStatus(bc)
		code = errors.CodeForBiz(bc)
	}
	resp := responseEnvelope{
		Code:      code,
		BizCode:   bc,
		Message:   message,
		RequestID: requestID(r),
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "json encode error response failed", slog.Any("error", err))
	}
}

// JSONBizError 以 biz_code 为唯一语义载体下发错误，HTTP 状态与 code 枚举自动推导。
func JSONBizError(w http.ResponseWriter, r *http.Request, bizCode string, message string) {
	JSONError(w, r, errors.HTTPStatus(bizCode), errors.CodeForBiz(bizCode), message, bizCode)
}
