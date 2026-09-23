package invite

import (
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"papafeiji/backend/internal/auth"
	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/file"
	"papafeiji/backend/internal/middleware"
	pkgerrors "papafeiji/backend/pkg/errors"
	"papafeiji/backend/pkg/util"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

const (
	inviteCodeCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

var inviteCodeRegex = regexp.MustCompile("^[" + inviteCodeCharset + "]{8}$")

type Handler struct {
	pool           *db.Pool
	rdb            *redis.Client
	qrGenerator    *QRCodeGenerator
	cfg            *config.Config
	resolveLimiter *middleware.IPRateLimiter
}

func NewHandler(pool *db.Pool, rdb *redis.Client, wechat *auth.WechatClient, storage *file.Storage, cfg *config.Config) *Handler {
	bgPath := defaultInviteBgPath()
	h := &Handler{
		pool:           pool,
		rdb:            rdb,
		qrGenerator:    NewQRCodeGenerator(wechat, pool, storage, bgPath),
		cfg:            cfg,
		resolveLimiter: middleware.NewIPRateLimiter(60, time.Hour, cfg.TrustedProxyCIDR),
	}
	return h
}

// Stop 为安全空操作；解析接口限流器无后台 goroutine，无需释放资源。
func (h *Handler) Stop() {
	if h.resolveLimiter != nil {
		h.resolveLimiter.Stop()
	}
}

func defaultInviteBgPath() string {
	return "/app/assets/invite-share-cover.png"
}

func (h *Handler) RegisterPublic(router chi.Router) {

	// 邀请码可被枚举扫描，/invite/resolve 公开接口需限流：同 IP 每小时最多 60 次。
	router.With(h.resolveLimiter.Handler).Get("/invite/resolve", h.Resolve)
}

func (h *Handler) Register(router chi.Router) {
	router.Get("/invite/list", h.List)
	router.Post("/invite/qrcode", h.QRCode)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	rows, err := h.pool.Queries().ListUserInvitesByInviter(ctx, pgtype.Text{String: userID, Valid: userID != ""})
	if err != nil {
		slog.ErrorContext(ctx, "failed to list invites", slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to list invites")
		return
	}

	items := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]interface{}{
			"userId":    row.UserID,
			"nickName":  util.ToInterface(row.Nickname),
			"avatarUrl": util.ToInterface(row.Avatar),
			"joined":    row.Joined.Valid && row.Joined.Bool,
		})
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"list": items,
	})
}

func (h *Handler) QRCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		Raw bool `json:"raw"`
	}
	if err := middleware.ReadJSONBodyAllowEmpty(w, r, &req, 64*1024); err != nil {
		middleware.JSONBodyError(w, r, err)
		return
	}

	var url string
	var err error
	if req.Raw {
		url, err = h.qrGenerator.GenerateRaw(ctx, h.rdb, userID)
	} else {
		url, err = h.qrGenerator.Generate(ctx, h.rdb, userID)
	}
	if err != nil {
		if ctx.Err() != nil {
			// 客户端在生成过程中断开（nginx 记录 499）：context 取消会使 OSS 上传后的
			// 事务回滚。属于客户端放弃，不记为内部错误，便于日志区分真实故障。
			slog.InfoContext(ctx, "invite qrcode generation aborted by client",
				slog.String("user_id", userID), slog.Bool("raw", req.Raw))
		} else {
			slog.ErrorContext(ctx, "failed to generate invite qrcode",
				slog.String("user_id", userID), slog.Bool("raw", req.Raw), slog.Any("error", err))
		}
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to generate invite qrcode")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"url": url,
	})
}

func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	shortCode := r.URL.Query().Get("code")

	if shortCode == "" || !inviteCodeRegex.MatchString(shortCode) {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "invalid invite code")
		return
	}

	userID, err := ResolveInviterFromCode(ctx, h.pool.Queries(), shortCode)
	if err != nil {
		slog.ErrorContext(ctx, "failed to resolve invite code", slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to resolve invite code")
		return
	}
	if userID == "" {
		middleware.JSONError(w, r, http.StatusNotFound, pkgerrors.CodeNotFound, "invite code not found")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"userId": userID,
	})
}
