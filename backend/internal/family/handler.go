package family

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/middleware"
	dbx "papafeiji/backend/pkg/db"
	"papafeiji/backend/pkg/errors"
	"papafeiji/backend/pkg/timeutil"
	"papafeiji/backend/pkg/util"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

type VipServicer interface {
	ExtendVIPDaysWithTx(ctx context.Context, userID string, days int, q *sqlc.Queries) error
}

type Handler struct {
	router        chi.Router
	service       *Service
	pool          *db.Pool
	vipService    VipServicer
	defaultAvatar string
}

func NewHandler(router chi.Router, pool *db.Pool, rdb *redis.Client, lock db.Locker, defaultAvatarURL string, vipService VipServicer) *Handler {
	svc := NewService(pool, rdb, lock, defaultAvatarURL)
	return &Handler{
		router:        router,
		service:       svc,
		pool:          pool,
		vipService:    vipService,
		defaultAvatar: defaultAvatarURL,
	}
}

func (h *Handler) Register() {
	h.router.Get("/family", h.GetFamily)
	h.router.Post("/family", h.CreateFamily)
	h.router.Post("/family/invite-link", h.CreateInviteLink)
	h.router.Post("/family/invite-link/join", h.JoinByInviteLink)
	h.router.Post("/family/leave", h.LeaveFamily)
	h.router.Delete("/family/members/{userId}", h.RemoveMember)
	h.router.Delete("/family", h.DissolveFamily)
}

func (h *Handler) GetFamily(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	info, err := h.service.GetFamily(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get family info", slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to get family info")
		return
	}

	members := make([]map[string]interface{}, 0, len(info.Members))
	for _, m := range info.Members {
		members = append(members, map[string]interface{}{
			"userId":    m.UserID,
			"avatarUrl": m.Avatar,
			"nickName":  m.Nickname,
			"role":      m.Role,
			"joinedAt":  m.JoinedAt.Format("2006-01-02T15:04:05-07:00"),
		})
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"familyId":   info.FamilyID,
		"ownerId":    info.OwnerID,
		"isPersonal": info.IsPersonal,
		"members":    members,
	})
}

func (h *Handler) CreateFamily(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	familyID, err := h.service.CreateFamily(ctx, userID)
	if err != nil {
		switch err {
		case ErrAlreadyInFamily:
			middleware.JSONBizError(w, r, errors.BizAlreadyInFamily, err.Error())
		default:
			slog.ErrorContext(ctx, "failed to create family", slog.Any("error", err))
			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to create family")
		}
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"familyId": familyID,
	})
}

func (h *Handler) LeaveFamily(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	if err := h.service.LeaveFamily(ctx, userID); err != nil {
		switch err {
		case ErrNotInNormalFamily:
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, err.Error())
		case ErrOwnerCannotLeave:
			middleware.JSONBizError(w, r, errors.BizOwnerCannotLeaveFamily, err.Error())
		case ErrOperationInProgress:
			middleware.JSONBizError(w, r, errors.BizOperationInProgress, err.Error())
		default:
			slog.ErrorContext(ctx, "failed to leave family", slog.Any("error", err))
			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to leave family")
		}
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)
	targetUserID := chi.URLParam(r, "userId")

	if targetUserID == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "userId is required")
		return
	}

	if err := h.service.RemoveMember(ctx, userID, targetUserID); err != nil {
		switch err {
		case ErrCannotRemoveSelf:
			middleware.JSONBizError(w, r, errors.BizCannotRemoveSelf, err.Error())
		case ErrCannotRemoveOwner:
			middleware.JSONBizError(w, r, errors.BizCannotRemoveOwner, err.Error())
		case ErrNotInNormalFamily, ErrNotOwner:
			middleware.JSONError(w, r, http.StatusForbidden, errors.CodeForbidden, err.Error())
		case ErrTargetNotInFamily:
			middleware.JSONError(w, r, http.StatusNotFound, errors.CodeNotFound, err.Error())
		case ErrOperationInProgress:
			middleware.JSONBizError(w, r, errors.BizOperationInProgress, err.Error())
		default:
			slog.ErrorContext(ctx, "failed to remove member", slog.Any("error", err))
			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to remove member")
		}
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) DissolveFamily(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	if err := h.service.DissolveFamily(ctx, userID); err != nil {
		switch err {
		case ErrCannotDissolvePersonal:
			middleware.JSONError(w, r, http.StatusForbidden, errors.CodeForbidden, err.Error())
		case ErrNotOwner:
			middleware.JSONError(w, r, http.StatusForbidden, errors.CodeForbidden, err.Error())
		case ErrOperationInProgress:
			middleware.JSONBizError(w, r, errors.BizOperationInProgress, err.Error())
		default:
			slog.ErrorContext(ctx, "failed to dissolve family", slog.Any("error", err))
			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to dissolve family")
		}
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) CreateInviteLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	info, err := h.service.GetFamily(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get family info", slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to get family info")
		return
	}
	if info.FamilyID == "" {
		middleware.JSONError(w, r, http.StatusNotFound, errors.CodeNotFound, "family not found")
		return
	}

	linkID := info.FamilyID
	if info.IsPersonal {
		newFamilyID, err := h.service.CreateFamily(ctx, userID)
		if err != nil {
			switch err {
			case ErrAlreadyInFamily:
				middleware.JSONBizError(w, r, errors.BizAlreadyInFamily, err.Error())
			default:
				slog.ErrorContext(ctx, "failed to create family", slog.Any("error", err))
				middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to create family")
			}
			return
		}
		linkID = newFamilyID
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"linkId": linkID,
	})
}

func (h *Handler) JoinByInviteLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		LinkID string `json:"linkId"`
	}
	if err := middleware.ReadJSONBody(w, r, &req, 64*1024); err != nil {
		middleware.JSONBodyError(w, r, err)
		return
	}
	if req.LinkID == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "linkId is required")
		return
	}

	if err := h.service.JoinFamily(ctx, userID, req.LinkID); err != nil {

		switch err {
		case ErrFamilyNotFound:
			middleware.JSONBizError(w, r, errors.BizFamilyNotFound, err.Error())
		case ErrTargetIsPersonalFamily:
			middleware.JSONBizError(w, r, errors.BizTargetIsPersonalFamily, err.Error())
		case ErrAlreadyInTargetFamily:
			middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
		case ErrFamilyFull:
			middleware.JSONBizError(w, r, errors.BizFamilyFull, err.Error())
		case ErrRemovedRejoinCooldown:
			middleware.JSONBizError(w, r, errors.BizRemovedRejoinCooldown, err.Error())
		case ErrOperationInProgress:
			middleware.JSONBizError(w, r, errors.BizOperationInProgress, err.Error())
		default:
			slog.ErrorContext(ctx, "failed to join family", slog.Any("error", err))
			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to join family")
		}
		return
	}

	h.grantJoinReward(ctx, userID, req.LinkID)
	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

// grantJoinReward 发放入家邀请奖励（被邀请人 +3 / 邀请人 +7 天 VIP，见 02d F-5）。
// 奖励属资金相关发放：任何失败分支记 Error 级结构化日志并带 alert=invite_reward_failed
// 关键字（scripts/alert-watch.sh 按关键字扫描触达告警通道，DEPLOYMENT §10），
// 但不向客户端报错、不影响 JoinByInviteLink 的 200。
func (h *Handler) grantJoinReward(ctx context.Context, userID, familyID string) {
	if _, err := h.pool.Queries().GetUserInviteByUserID(ctx, toNullText(userID)); err == nil {
		return
	} else if !stderrors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "alert=invite_reward_failed: check user invite failed", "user_id", userID, "error", err)
		return
	}

	u, err := h.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		// 奖励路径任何失败不向客户端报错；alert= 关键字保证告警触达，瞬时 DB 错误不静默吞掉。
		slog.ErrorContext(ctx, "alert=invite_reward_failed: grant join reward get user failed", "user_id", userID, "error", err)
		return
	}
	// 奖励窗口与 /auth/inviter 统一为注册后 7 天（原 5 分钟惩罚弱网/慢扫码新用户，
	// 且两入口窗口不一致无决策依据）；inviter 侧另有月度 14 天封顶。
	if time.Since(u.CreatedAt.Time) > 7*24*time.Hour {
		return
	}

	ownerID, err := h.pool.Queries().GetFamilyOwner(ctx, familyID)
	if err != nil {
		// F-5 第 5 步：任何失败仅记日志、不影响主流程 200——alert= 关键字走告警通道，不能无声跳过。
		slog.ErrorContext(ctx, "alert=invite_reward_failed: grant join reward get family owner failed", slog.String("user_id", userID), slog.String("family_id", familyID), slog.Any("error", err))
		return
	}
	if ownerID == "" {
		return
	}
	if ownerID == userID {
		return
	}

	if err := db.WithTxDeferrable(ctx, h.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		if _, innerErr := q.GetUserInviteByUserID(ctx, toNullText(userID)); innerErr == nil {
			return nil
		} else if !stderrors.Is(innerErr, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "alert=invite_reward_failed: check user invite failed", "user_id", userID, "error", innerErr)
			return nil
		}

		if _, checkErr := q.GetUserByID(ctx, ownerID); checkErr != nil {
			// ErrNoRows（owner 已不存在）为预期跳过；其余 DB 错误按 F-5 记 alert 告警日志后跳过奖励，不静默吞掉。
			if !stderrors.Is(checkErr, pgx.ErrNoRows) {
				slog.ErrorContext(ctx, "alert=invite_reward_failed: grant join reward check owner failed, skip reward", "user_id", userID, "owner_id", ownerID, "error", checkErr)
			}
			return nil
		}

		inviteID, err := util.NewUUID()
		if err != nil {
			return fmt.Errorf("generate invite id: %w", err)
		}
		// 冗余被邀请人 openid（注销后行保留，user_id 置 NULL），终身一次判定依据。
		if _, err := q.CreateUserInvite(ctx, sqlc.CreateUserInviteParams{
			ID:         inviteID,
			UserID:     toNullText(userID),
			InviterID:  toNullText(ownerID),
			UserOpenID: toNullText(u.OpenID),
		}); err != nil {
			if !dbx.IsUniqueViolation(err) {
				return fmt.Errorf("create user invite: %w", err)
			}
			// 与 BindInviter 补绑路径并发时撞 user_id 唯一索引：对端事务已建行并发奖，本路径幂等跳过（家庭加入不被 500 打断）。
			slog.InfoContext(ctx, "invite record created concurrently, skip join reward", slog.String("user_id", userID))
			return nil
		}

		// openid 曾领过被邀请奖励（墓碑行保留于 user_invites）则跳过全部奖励：
		// +3/+7 均不发，邀请行照常创建且对邀请人可见，家庭加入主流程不受影响。
		rewarded, rewardedErr := q.ExistsInviteeRewardByOpenID(ctx, toNullText(u.OpenID))
		if rewardedErr != nil {
			slog.ErrorContext(ctx, "alert=invite_reward_failed: check openid tombstone failed", slog.String("user_id", userID), slog.Any("error", rewardedErr))
			return nil
		}
		if rewarded {
			slog.InfoContext(ctx, "join rewards skipped: openid already rewarded (re-registration)", slog.String("user_id", userID))
			return nil
		}

		// 先标记后发奖：上方墓碑预检已排除重注册身份，reward_invitee_at 只会在此处由 NULL 翻转一次；
		// 并发双绑由行锁 + IS NULL 条件自然去重，唯一索引 uq_user_invites_user_open_id 仅作 DB 层兜底。
		markRows, err := q.MarkInviteeRewarded(ctx, inviteID)
		if err != nil {
			return fmt.Errorf("mark invitee rewarded: %w", err)
		}
		if markRows > 0 {
			if err := h.vipService.ExtendVIPDaysWithTx(ctx, userID, 3, q); err != nil {
				return fmt.Errorf("extend invitee vip: %w", err)
			}
		}

		now := timeutil.NowShanghai()
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, timeutil.Shanghai)
		monthEnd := monthStart.AddDate(0, 1, 0)
		if err := q.LockInviterReward(ctx, toNullText(ownerID)); err != nil {
			return fmt.Errorf("lock inviter reward: %w", err)
		}
		rewardedDays, err := q.CountInviterMonthlyRewardDays(ctx, sqlc.CountInviterMonthlyRewardDaysParams{
			InviterID:         toNullText(ownerID),
			RewardInviterAt:   pgtype.Timestamptz{Time: monthStart, Valid: true},
			RewardInviterAt_2: pgtype.Timestamptz{Time: monthEnd, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("count inviter monthly reward days: %w", err)
		}
		if rewardedDays < 14 {
			rewardedRows, err := q.MarkInviterRewarded(ctx, inviteID)
			if err != nil {
				return fmt.Errorf("mark inviter rewarded: %w", err)
			}
			if rewardedRows > 0 {
				if err := h.vipService.ExtendVIPDaysWithTx(ctx, ownerID, 7, q); err != nil {
					return fmt.Errorf("extend inviter vip: %w", err)
				}
			}
		}

		return nil
	}); err != nil {
		// 奖励事务失败不回滚家庭加入、不重试（02d F-5）；alert= 关键字保证发放失败经告警通道触达。
		slog.ErrorContext(ctx, "alert=invite_reward_failed: grant join reward failed", "user_id", userID, "family_id", familyID, "error", err)
	}
}

func toNullText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
