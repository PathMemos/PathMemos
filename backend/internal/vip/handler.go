package vip

import (
	"encoding/json"

	"net/http"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/middleware"
	"papafeiji/backend/pkg/errors"

	"github.com/go-chi/chi/v5"
)

const maxVIPRequestBodySize = 8 << 10

type Handler struct {
	router  chi.Router
	pool    *db.Pool
	service *Service
	cfg     *config.Config
}

// NewHandler 创建 VIP 路由处理器（防重由 DB 唯一约束保证）。
func NewHandler(router chi.Router, pool *db.Pool, service *Service, cfg *config.Config) *Handler {
	return &Handler{
		router:  router,
		pool:    pool,
		service: service,
		cfg:     cfg,
	}
}

// freeVipDisabled 检查免费 VIP 是否已下线（FREE_VIP_ENABLED=0）。
// 下线语义为端点不可用（404）而非仅隐藏前端入口：直调 API 的旧客户端同样被拒
func (h *Handler) freeVipDisabled(w http.ResponseWriter, r *http.Request) bool {
	if h.cfg != nil && !h.cfg.FreeVipEnabled {
		middleware.JSONError(w, r, http.StatusNotFound, errors.CodeNotFound, "free vip disabled")
		return true
	}
	return false
}

func (h *Handler) Register() {
	h.router.Get("/vip", h.ListPaidVIP)
	h.router.Get("/vip/free", h.ListFreeVIP)
	h.router.Post("/vip/free/claim", h.ClaimFreeVIP)
	h.router.Get("/vip/free/check", h.CheckFreeVIP)
	h.router.Post("/vip/new-user", h.ClaimTrialVIP)
}

func (h *Handler) ListPaidVIP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := h.pool.Queries().ListActivePaidVIPs(ctx)
	if err != nil {

		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to list vips")
		return
	}

	data := make([]map[string]interface{}, 0, len(rows))
	for _, v := range rows {
		data = append(data, vipToMap(v))
	}

	middleware.JSON(w, r, http.StatusOK, data)
}

func (h *Handler) ListFreeVIP(w http.ResponseWriter, r *http.Request) {
	if h.freeVipDisabled(w, r) {
		return
	}
	ctx := r.Context()

	rows, err := h.pool.Queries().ListActiveFreeVIPs(ctx)
	if err != nil {

		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to list free vips")
		return
	}

	data := make([]map[string]interface{}, 0, len(rows))
	for _, v := range rows {
		data = append(data, map[string]interface{}{
			"id":   v.ID,
			"name": v.Name,
		})
	}

	middleware.JSON(w, r, http.StatusOK, data)
}

func (h *Handler) ClaimFreeVIP(w http.ResponseWriter, r *http.Request) {
	if h.freeVipDisabled(w, r) {
		return
	}
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		VipID string `json:"vipId"`
	}
	if err := middleware.ReadJSONBody(w, r, &req, maxVIPRequestBodySize); err != nil {
		middleware.JSONBodyError(w, r, err)
		return
	}
	if req.VipID == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "vipId is required")
		return
	}

	if err := h.service.ClaimFreeVIP(ctx, userID, req.VipID); err != nil {
		switch err {
		case ErrFreeVIPAlreadyClaimed:
			middleware.JSONBizError(w, r, errors.BizFreeVipAlreadyClaimed, err.Error())
		case ErrInvalidVIP:
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, err.Error())
		default:

			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to claim free vip")
		}
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) CheckFreeVIP(w http.ResponseWriter, r *http.Request) {
	if h.freeVipDisabled(w, r) {
		return
	}
	ctx := r.Context()
	userID := middleware.UserID(ctx)
	vipID := r.URL.Query().Get("vipId")

	if vipID == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "vipId is required")
		return
	}

	claimed, err := h.service.HasVIPClaim(ctx, userID, vipID)
	if err != nil {

		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to check vip claim")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{"claimed": claimed})
}

func (h *Handler) ClaimTrialVIP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	if err := h.service.ClaimTrialVIP(ctx, userID); err != nil {
		switch err {
		case ErrTrialVIPAlreadyClaimed:
			middleware.JSONBizError(w, r, errors.BizTrialVipAlreadyClaimed, err.Error())
		case ErrTrialVIPDisabled:
			// 对齐 free 档下线闸门（freeVipDisabled）：商品下线为端点不可用（404），
			// 直调 API 的旧客户端同样被拒；已领取的 409 语义不受影响。
			middleware.JSONError(w, r, http.StatusNotFound, errors.CodeNotFound, "trial vip disabled")
		default:

			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to claim trial vip")
		}
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func vipToMap(v sqlc.Vip) map[string]interface{} {
	var prices interface{}
	if len(v.Prices) > 0 {
		_ = json.Unmarshal(v.Prices, &prices) //nolint:errcheck // invalid JSON falls back to empty prices
	}
	if prices == nil {
		prices = []interface{}{}
	}

	return map[string]interface{}{
		"id":   v.ID,
		"name": v.Name,
		"type": v.Type,
		"timeLimit": map[string]interface{}{
			"mark":   v.TimeLimitMark,
			"number": v.TimeLimitNumber,
		},
		"prices": prices,
	}
}
