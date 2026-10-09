package autorecord

import (
	"fmt"
	"log/slog"

	"net/http"
	"strconv"
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
	maxBatchPoints    = 50
	maxUploadBodySize = 64 * 1024 // 64 KiB，足以覆盖 50 个轨迹点
)

type Handler struct {
	router  chi.Router
	pool    *db.Pool
	service *Service
}

func NewHandler(router chi.Router, pool *db.Pool, service *Service) *Handler {
	return &Handler{router: router, pool: pool, service: service}
}

func (h *Handler) Register() {
	h.router.Get("/auto-record/config", h.GetConfig)
	h.router.Put("/auto-record/config", h.UpdateConfig)
	h.router.Put("/auto-record/active", h.TouchActive)
	h.router.Post("/auto-record/trajectories", h.UploadTrajectories)
}

func (h *Handler) GetConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	isVip := h.service.HasActiveVIP(ctx, userID)
	if !isVip {
		middleware.JSON(w, r, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}

	user, err := h.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {

		middleware.JSON(w, r, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{"enabled": user.AutoRecordEnabled})
}

func (h *Handler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := middleware.ReadJSONBody(w, r, &req, 4096); err != nil {
		middleware.JSONBodyError(w, r, err)
		return
	}

	if req.Enabled && !h.service.HasActiveVIP(ctx, userID) {
		middleware.JSONBizError(w, r, errors.BizNotVip, "vip required")
		return
	}

	if err := h.pool.Queries().UpdateUserAutoRecord(ctx, sqlc.UpdateUserAutoRecordParams{
		ID:                userID,
		AutoRecordEnabled: req.Enabled,
	}); err != nil {

		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to update config")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

// TouchActive 记录用户活跃时间；异常告警候选 SQL 据此跳过最近活跃用户（last_active_at 条件），
// 并不直接重置 abnormal_alert_sent_at。
func (h *Handler) TouchActive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	if err := h.service.TouchActiveAt(ctx, userID); err != nil {

		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to record active")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) UploadTrajectories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	if !h.service.HasActiveVIP(ctx, userID) {
		middleware.JSONBizError(w, r, errors.BizNotVip, "vip required")
		return
	}

	// 每次上传都刷新 last_active_at，作为存活心跳
	_ = h.service.TouchActiveAt(ctx, userID) //nolint:errcheck // heartbeat update is best-effort

	var req struct {
		Points []struct {
			Lat        *float64 `json:"lat"`
			Lon        *float64 `json:"lon"`
			RecordedAt string   `json:"recordedAt"`
		} `json:"points"`
		// BatchSeq 由前端生成，仅用于观测；服务端幂等以自然键唯一索引为准。
		BatchSeq int `json:"batchSeq"`
	}
	if err := middleware.ReadJSONBody(w, r, &req, maxUploadBodySize); err != nil {
		middleware.JSONBodyError(w, r, err)
		return
	}

	if len(req.Points) == 0 {
		middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
		return
	}
	if len(req.Points) > maxBatchPoints {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, fmt.Sprintf("too many points, max %d", maxBatchPoints))
		return
	}

	ids := make([]string, 0, len(req.Points))
	userIDs := make([]string, 0, len(req.Points))
	lats := make([]string, 0, len(req.Points))
	lons := make([]string, 0, len(req.Points))
	recordedAts := make([]pgtype.Timestamptz, 0, len(req.Points))
	for _, p := range req.Points {
		if p.Lat == nil || p.Lon == nil {
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "lat and lon are required")
			return
		}
		if *p.Lat < -90 || *p.Lat > 90 || *p.Lon < -180 || *p.Lon > 180 {
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "invalid coordinates")
			return
		}
		if p.RecordedAt == "" {
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "recorded_at is required")
			return
		}
		t, err := time.Parse(time.RFC3339, p.RecordedAt)
		if err != nil {

			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "invalid recorded_at")
			return
		}
		id, err := util.NewUUID()
		if err != nil {

			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to generate trajectory id")
			return
		}
		ids = append(ids, id)
		userIDs = append(userIDs, userID)
		lats = append(lats, formatCoord(*p.Lat))
		lons = append(lons, formatCoord(*p.Lon))
		recordedAts = append(recordedAts, pgtype.Timestamptz{Time: t, Valid: true})
	}

	if err := h.pool.Queries().InsertTrajectories(ctx, sqlc.InsertTrajectoriesParams{
		Ids:         ids,
		UserIds:     userIDs,
		Lats:        lats,
		Lons:        lons,
		RecordedAts: recordedAts,
	}); err != nil {

		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to upload trajectory")
		return
	}

	// OPS-LOG：轨迹上报审计日志（用于数据问题复盘）。
	slog.InfoContext(ctx, "auto record trajectories uploaded",
		slog.String("user_id", userID),
		slog.Int("count", len(req.Points)),
		slog.Int("batch_seq", req.BatchSeq),
	)

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func formatCoord(v float64) string {
	return strconv.FormatFloat(v, 'f', 7, 64)
}
