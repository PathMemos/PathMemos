// Package opslog 提供客户端操作日志上报端点：小程序本地记录关键操作事件，
// 打开小程序时批量 POST /ops/client-log 落库，用于排查数据问题（如记录丢失）。
package opslog

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/middleware"
	"papafeiji/backend/pkg/errors"
	"papafeiji/backend/pkg/util"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	maxBodySize = 64 << 10 // 64 KiB，足够 200 条摘要事件
	maxEvents   = 200
	maxEventLen = 1024 // 单条事件 detail 超过此长度时整体丢弃（不截断保留），避免脏数据撑爆表
)

type Handler struct {
	router chi.Router
	pool   *db.Pool
}

func NewHandler(router chi.Router, pool *db.Pool) *Handler {
	return &Handler{router: router, pool: pool}
}

func (h *Handler) Register() {
	h.router.Post("/ops/client-log", h.Upload)
}

type clientEvent struct {
	T      string          `json:"t"`
	Type   string          `json:"type"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

type uploadRequest struct {
	Events     []clientEvent `json:"events"`
	Device     string        `json:"device,omitempty"`
	AppVersion string        `json:"appVersion,omitempty"`
}

// Upload 接收小程序批量上报的客户端操作日志。
// 宽容处理：JSON 合法即存储（事件超量/超长做截断），只拒绝超大 body 与非法 JSON，
// 避免客户端队列因 4xx 无法清空而反复重试。
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "invalid request body")
		return
	}
	if len(body) > maxBodySize {
		middleware.JSONError(w, r, http.StatusRequestEntityTooLarge, errors.CodeRequestEntityTooLarge, "request body too large")
		return
	}

	var req uploadRequest
	if err := json.Unmarshal(body, &req); err != nil {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "invalid json")
		return
	}

	// 事件条数上限 + 单条截断，防止恶意/异常客户端撑爆存储。
	if len(req.Events) > maxEvents {
		req.Events = req.Events[:maxEvents]
	}
	events := make([]map[string]interface{}, 0, len(req.Events))
	now := time.Now().UTC()
	for _, ev := range req.Events {
		if ev.T == "" || ev.Type == "" || len(ev.Type) > 128 {
			continue
		}
		entry := map[string]interface{}{
			"t":    ev.T,
			"type": ev.Type,
		}
		if len(ev.Detail) > 0 && len(ev.Detail) <= maxEventLen {
			var d interface{}
			if err := json.Unmarshal(ev.Detail, &d); err == nil {
				entry["detail"] = d
			}
		}
		events = append(events, entry)
	}
	if len(events) == 0 {
		middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
		return
	}

	raw, err := json.Marshal(events)
	if err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "marshal events failed")
		return
	}

	id, err := util.NewUUID()
	if err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "generate id failed")
		return
	}

	device := req.Device
	if device == "" {
		device = r.UserAgent()
	}
	device = truncateUTF8(device, 256)
	appVersion := truncateUTF8(req.AppVersion, 64)

	if _, err := h.pool.Queries().InsertClientOpsLog(ctx, sqlc.InsertClientOpsLogParams{
		ID:         id,
		UserID:     userID,
		Device:     device,
		AppVersion: appVersion,
		Events:     raw,
		ClientSentAt: pgtype.Timestamptz{
			Time:  now,
			Valid: true,
		},
	}); err != nil {
		// 重复上报/写入失败不阻断客户端；记录错误日志便于运维发现。
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "insert client ops log failed")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

// truncateUTF8 按字节上限截断并丢弃切断的多字节残片，避免写出非法 UTF-8 触发
// Postgres invalid byte sequence 导致上报接口 500（device 256 / appVersion 64）。
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "")
}
