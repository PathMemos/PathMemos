package auth

import (
	"context"
	"crypto/rand"
	stderrors "errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"papafeiji/backend/internal/avatar"
	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/family"
	"papafeiji/backend/internal/file"
	"papafeiji/backend/internal/middleware"
	"papafeiji/backend/internal/pkg/safe"
	"papafeiji/backend/internal/user"
	"papafeiji/backend/internal/userinfo"
	"papafeiji/backend/internal/vip"
	dbx "papafeiji/backend/pkg/db"
	"papafeiji/backend/pkg/errors"
	"papafeiji/backend/pkg/timeutil"
	"papafeiji/backend/pkg/util"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

// SessionStore 抽象会话的创建与删除（*middleware.SessionManager 为生产实现），
// 便于登录流程单元测试注入 mock。
type SessionStore interface {
	Create(ctx context.Context, userID string) (string, error)
	Delete(ctx context.Context, sessionID string) error
	DeleteAll(ctx context.Context, userID string) error
}

type Handler struct {
	pool          *db.Pool
	bgPool        *db.Pool
	sessions      SessionStore
	wechat        *WechatClient
	vipService    VIPService
	familyService FamilyService
	avatarService *avatar.Service
	storage       *file.Storage

	defaultAvatar string
}

type VIPService interface {
	vip.InfoProvider
	IssueTrialVIPWithTx(ctx context.Context, userID string, q *sqlc.Queries) error
	ExtendVIPDaysWithTx(ctx context.Context, userID string, days int, q *sqlc.Queries) error
}

type FamilyService interface {
	GetFamily(ctx context.Context, userID string) (*family.FamilyInfo, error)
	DeleteAccount(ctx context.Context, userID string) (*family.AccountCleanupInfo, error)
}

func NewHandlerWithBackgroundPool(pool *db.Pool, bgPool *db.Pool, rdb *redis.Client, cfg *config.Config, sessions *middleware.SessionManager, vipService VIPService, familyService FamilyService, avatarService *avatar.Service, storage *file.Storage, defaultAvatarURL string) *Handler {
	return &Handler{
		pool:          pool,
		bgPool:        bgPool,
		sessions:      sessions,
		wechat:        NewWechatClient(cfg, rdb),
		vipService:    vipService,
		familyService: familyService,
		avatarService: avatarService,
		storage:       storage,
		defaultAvatar: defaultAvatarURL,
	}
}

func (h *Handler) RegisterPublic(router chi.Router) {
	router.Post("/auth/login", h.Login)
}

func (h *Handler) RegisterProtected(router chi.Router) {
	router.Post("/auth/logout", h.Logout)
	router.Get("/auth/phone", h.GetPhone)
	router.Post("/auth/phone/unbind", h.UnbindPhone)
	router.Post("/auth/inviter", h.BindInviter)
	// POST /auth/phone/bind 由 main.go 单独注册（含 IP 限流，A-FIX-04）；DELETE /auth/account 同理。
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req struct {
		Code    string `json:"code"`
		Inviter string `json:"inviter"`
	}
	if err := middleware.ReadJSONBody(w, r, &req, 4096); err != nil {
		middleware.JSONBodyError(w, r, err)
		return
	}
	if req.Code == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "code is required")
		return
	}

	session, err := h.wechat.Jscode2session(ctx, req.Code)
	if err != nil {
		slog.ErrorContext(ctx, "wechat jscode2session failed", slog.Any("error", err))
		if stderrors.Is(err, ErrWechatInvalidCode) {
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "invalid wechat code")
		} else {
			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "wechat login failed")
		}
		return
	}

	user, isNew, err := h.findOrCreateUser(ctx, session, req.Inviter)
	if err != nil {
		slog.ErrorContext(ctx, "login findOrCreateUser failed", slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "login failed")
		return
	}

	// Reconcile orphan wx_mp_accounts records (e.g. user followed service account before
	// their mini-program account had a unionid set).
	if session.UnionID != "" {
		if linkErr := h.pool.Queries().LinkWxMPAccountByUnionID(ctx, sqlc.LinkWxMPAccountByUnionIDParams{
			Unionid: toNullText(session.UnionID),
			UserID:  toNullText(user.ID),
		}); linkErr != nil {
			slog.WarnContext(ctx, "link wx mp account by unionid failed", slog.String("user_id", user.ID), slog.String("unionid", util.MaskID(session.UnionID)), slog.Any("error", linkErr))
		}
	}

	sessionID, err := h.sessions.Create(ctx, user.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create session", slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to create session")
		return
	}

	mpSubscribed := false
	mpAccount, accErr := h.pool.Queries().GetWxMPAccountByUserID(ctx, pgtype.Text{String: user.ID, Valid: true})
	if accErr == nil {
		mpSubscribed = mpAccount.Subscribed
	} else if !stderrors.Is(accErr, pgx.ErrNoRows) {
		// 无公众号绑定记录（ErrNoRows）是正常情况，静默降级；仅真实 DB 故障才告警，
		// 避免每次登录都刷 WARN（与 user.GetProfile 口径一致）。
		slog.WarnContext(ctx, "get wx mp account failed, mpSubscribed defaults to false", slog.String("user_id", user.ID), slog.Any("error", accErr))
	}
	userInfo := userinfo.Build(ctx, user, h.vipService, h.defaultAvatar, mpSubscribed)
	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"sessionId": sessionID,
		"newUser":   isNew,
		"userInfo":  userInfo,
	})
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID := middleware.SessionID(ctx)

	if err := h.sessions.Delete(ctx, sessionID); err != nil {
		slog.ErrorContext(ctx, "delete session failed", slog.String("session_id", util.MaskID(sessionID)), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "logout failed")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) GetPhone(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	user, err := h.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "user not found")
		return
	}

	var phone interface{}
	if user.PhoneNumber.Valid {
		phone = user.PhoneNumber.String
	} else {
		phone = nil
	}

	// 与后端 BindPhone 的 phoneModificationLockedToday 对齐：只要当天绑定过（无论当前是否已解绑），
	// 当天都不可再绑定，避免客户端显示可改、服务端却拒绝。
	canModifyToday := true
	if user.PhoneBindTime.Valid {
		bindDate := user.PhoneBindTime.Time.In(timeutil.Shanghai).Format("2006-01-02")
		today := timeutil.NowShanghai().Format("2006-01-02")
		canModifyToday = bindDate != today
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"phoneNumber":    phone,
		"canModifyToday": canModifyToday,
	})
}

// phoneModificationLockedToday 判断用户今天是否已绑定过手机号：
// 服务端日限（B1-10），与客户端 canModifyToday 语义一致，防止绕过客户端限制频繁调用微信接口。
func phoneModificationLockedToday(user sqlc.GetUserByIDRow) bool {
	if !user.PhoneBindTime.Valid {
		return false
	}
	return user.PhoneBindTime.Time.In(timeutil.Shanghai).Format("2006-01-02") == timeutil.NowShanghai().Format("2006-01-02")
}

func (h *Handler) BindPhone(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		Code string `json:"code"`
	}
	if err := middleware.ReadJSONBody(w, r, &req, 4096); err != nil {
		middleware.JSONBodyError(w, r, err)
		return
	}

	// M1：日限检查先于微信接口调用——被限用户不再消耗微信 code 交换额度。
	user, err := h.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "bind phone get user failed", slog.String("user_id", userID), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to get user")
		return
	}
	if phoneModificationLockedToday(user) {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "phone can only be modified once per day")
		return
	}

	phone, err := h.wechat.GetPhoneNumber(ctx, req.Code)
	if err != nil {
		slog.ErrorContext(ctx, "bind phone getPhoneNumber failed", slog.String("user_id", userID), slog.Any("error", err))
		if stderrors.Is(err, ErrWechatInvalidCode) {
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "invalid phone code")
		} else {
			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to get phone number")
		}
		return
	}

	today := timeutil.NowShanghai()
	rows, err := h.pool.Queries().BindUserPhoneIfAllowed(ctx, sqlc.BindUserPhoneIfAllowedParams{
		ID:            userID,
		PhoneNumber:   pgtype.Text{String: phone, Valid: true},
		PhoneBindTime: nowPgxTimestamptz(),
		Today:         pgtype.Date{Time: time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location()), Valid: true},
	})
	if err != nil {
		if dbx.IsUniqueViolation(err) {
			middleware.JSONBizError(w, r, errors.BizPhoneAlreadyBound, "phone already bound")
			return
		}
		err = fmt.Errorf("bind phone: %w", err)
		slog.ErrorContext(ctx, "bind phone failed", slog.String("user_id", userID), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to bind phone")
		return
	}
	if rows == 0 {
		// A-FIX-03：并发下其他请求已在本日完成绑定；以原子条件的受影响行数为权威判定。
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "phone can only be modified once per day")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) UnbindPhone(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		Code string `json:"code"`
	}
	if err := middleware.ReadJSONBody(w, r, &req, 4096); err != nil {
		middleware.JSONBodyError(w, r, err)
		return
	}

	// R4：解绑不再调用微信 GetPhoneNumber（每次 code 交换都会产生认证费用）。
	// 安全边界：登录态持有者即账号所有者，解绑自己账号的手机号无需二次验证。
	// 日限口径：绑定受「每天最多绑定一次」约束；解绑保留 PhoneBindTime（不清空），
	// 否则「绑→解绑→当天再绑」可绕过日限并反复消耗微信手机号认证额度。
	user, err := h.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "unbind phone get user failed", slog.String("user_id", userID), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to get user")
		return
	}
	if !user.PhoneNumber.Valid || user.PhoneNumber.String == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "phone not bound")
		return
	}

	if err := h.pool.Queries().UpdateUserPhone(ctx, sqlc.UpdateUserPhoneParams{
		ID:          userID,
		PhoneNumber: pgtype.Text{},
		// 保留最后一次绑定时间：日限据此判定「当天是否绑定过」，解绑不清空。
		PhoneBindTime: user.PhoneBindTime,
	}); err != nil {
		err = fmt.Errorf("unbind phone: %w", err)
		slog.ErrorContext(ctx, "unbind phone failed", slog.String("user_id", userID), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to unbind phone")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

// BindInviter 登录后补绑邀请人（R4）。
// 极弱网下新用户可能在场景码解析完成前就完成登录，错过注册期绑定（index.ts 1.5s 竞态）；
// 客户端在解析完成后调用本接口补绑。幂等约束：
//   - 已有邀请人（invited_by 非空或存在 user_invites 记录）→ 幂等成功，不重复奖励；
//   - 注册超过 7 天 → 静默成功不绑定（邀请奖励面向新用户，防老用户扫码刷奖励）；
//   - 邀请人不存在/自邀 → 拒绝。
//
// 奖励逻辑与注册期绑定共用 applyInviteRewardsWithTx，保证一致。
func (h *Handler) BindInviter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		Inviter string `json:"inviter"`
	}
	if err := middleware.ReadJSONBody(w, r, &req, 4096); err != nil {
		middleware.JSONBodyError(w, r, err)
		return
	}
	if req.Inviter == "" || req.Inviter == userID {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "invalid inviter")
		return
	}

	if _, err := h.pool.Queries().GetUserByID(ctx, req.Inviter); err != nil {
		if stderrors.Is(err, pgx.ErrNoRows) {
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "inviter not found")
			return
		}
		slog.ErrorContext(ctx, "bind inviter check failed", slog.String("user_id", userID), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to check inviter")
		return
	}

	user, err := h.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "bind inviter get user failed", slog.String("user_id", userID), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to get user")
		return
	}
	alreadyBound := user.InvitedBy.Valid && user.InvitedBy.String != ""
	if !alreadyBound {
		if _, err := h.pool.Queries().GetUserInviteByUserID(ctx, toNullText(userID)); err == nil {
			alreadyBound = true
		} else if !stderrors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "bind inviter check invite record failed", slog.String("user_id", userID), slog.Any("error", err))
			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to check invite record")
			return
		}
	}
	if alreadyBound {
		middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
		return
	}
	// 注册超过 7 天不补绑：邀请奖励面向新用户，防止老用户扫码刷 3+7 天 VIP。
	if time.Since(user.CreatedAt.Time) > 7*24*time.Hour {
		middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
		return
	}

	err = db.WithTx(ctx, h.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		// 事务内行锁 + 幂等条件更新，杜绝并发双绑双奖励。
		current, err := q.GetUserByIDForUpdate(ctx, userID)
		if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		if current.InvitedBy.Valid && current.InvitedBy.String != "" {
			return nil
		}
		if _, err := q.GetUserInviteByUserID(ctx, toNullText(userID)); err == nil {
			return nil
		} else if !stderrors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check invite record in tx: %w", err)
		}
		if _, err := q.UpdateUserInvitedBy(ctx, sqlc.UpdateUserInvitedByParams{
			ID:        userID,
			InvitedBy: toNullText(req.Inviter),
		}); err != nil {
			return fmt.Errorf("update user invited by: %w", err)
		}
		return h.applyInviteRewardsWithTx(ctx, q, userID, req.Inviter)
	})
	if err != nil {
		slog.ErrorContext(ctx, "bind inviter failed", slog.String("user_id", userID), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to bind inviter")
		return
	}

	slog.InfoContext(ctx, "bind inviter success", slog.String("user_id", userID), slog.String("inviter", util.MaskID(req.Inviter)))
	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

// DeleteAccount 注销当前登录账号。
// 风险接受：DB 提交后的 session/物理文件清理失败均只记录日志并返回 200，
// 避免用户误以为注销未成功而重复操作；残留数据由后台补偿任务与 TTL 自然过期兜底（AGENTS.md §2.5/§4.4）。
func (h *Handler) DeleteAccount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		ConfirmName string `json:"confirmName"`
	}
	if err := middleware.ReadJSONBody(w, r, &req, 4096); err != nil {
		middleware.JSONBodyError(w, r, err)
		return
	}
	if req.ConfirmName == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "confirmName is required")
		return
	}

	currentUser, err := h.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		if stderrors.Is(err, pgx.ErrNoRows) {
			middleware.JSONError(w, r, http.StatusNotFound, errors.CodeNotFound, "user not found")
			return
		}
		slog.ErrorContext(ctx, "delete account get user failed", slog.String("user_id", userID), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to delete account")
		return
	}
	if !currentUser.Nickname.Valid || currentUser.Nickname.String == "" {
		slog.ErrorContext(ctx, "delete account user nickname missing", slog.String("user_id", userID))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to delete account")
		return
	}
	if req.ConfirmName != currentUser.Nickname.String {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "confirmName mismatch")
		return
	}

	// 1. 立即删除当前 session，使当前登录状态失效。
	sessionID := middleware.SessionID(ctx)
	if err := h.sessions.Delete(ctx, sessionID); err != nil {
		slog.ErrorContext(ctx, "delete current session before account deletion failed", slog.String("user_id", userID), slog.String("session_id", util.MaskID(sessionID)), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to delete account")
		return
	}

	// 2. 在事务内完成账号数据删除；不再在 beforeCommit 中处理 session。
	cleanup, err := h.familyService.DeleteAccount(ctx, userID)
	if err != nil {
		switch err {
		case family.ErrOperationInProgress:
			middleware.JSONBizError(w, r, errors.BizOperationInProgress, err.Error())
		default:
			middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to delete account")
		}
		return
	}

	// 3. DB 提交成功后，在后台 goroutine 中尽力清理受影响用户的 session 与物理文件。
	// MCP 模块已迁移至 stateless Streamable HTTP，无 in-memory session 需清理。
	// 账号数据已删除，此处清理失败不应再返回 500，避免用户认为操作未成功而重复注销；
	// 残留 session 由 TTL 自然过期兜底，不再额外启动后台补偿任务（AGENTS.md 简单优先）。
	safe.Go(ctx, nil, func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		user.CleanupAfterAccountDeletion(bgCtx, h.pool, h.bgPool, h.sessions, h.storage, cleanup, userID)
	})

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) findOrCreateUser(ctx context.Context, session *WechatSession, inviter string) (*sqlc.GetUserByIDRow, bool, error) {
	queries := h.pool.Queries()

	if session.UnionID != "" {
		user, err := queries.GetUserByUnionID(ctx, toNullText(session.UnionID))
		if err == nil {

			if err := queries.UpdateUserSessionKey(ctx, sqlc.UpdateUserSessionKeyParams{
				ID:         user.ID,
				SessionKey: toNullText(session.SessionKey),
			}); err != nil {
				return nil, false, fmt.Errorf("update session key: %w", err)
			}
			fullUser, err := queries.GetUserByID(ctx, user.ID)
			if err != nil {
				return nil, false, fmt.Errorf("get user after union login: %w", err)
			}
			return &fullUser, false, nil
		}
		if !stderrors.Is(err, pgx.ErrNoRows) {
			// A-FIX-05：DB 抖动不能当作「无此用户」，否则会误建新账号。
			return nil, false, fmt.Errorf("get user by union id: %w", err)
		}
	}

	user, err := queries.GetUserByOpenID(ctx, session.OpenID)
	if err == nil {

		if err := queries.UpdateUserSessionKey(ctx, sqlc.UpdateUserSessionKeyParams{
			ID:         user.ID,
			SessionKey: toNullText(session.SessionKey),
		}); err != nil {
			return nil, false, fmt.Errorf("update session key: %w", err)
		}

		if session.UnionID != "" && (!user.Unionid.Valid || user.Unionid.String == "") {
			if err := queries.UpdateUserUnionID(ctx, sqlc.UpdateUserUnionIDParams{
				ID:      user.ID,
				Unionid: toNullText(session.UnionID),
			}); err != nil {
				return nil, false, fmt.Errorf("update union id: %w", err)
			}
		}
		fullUser, err := queries.GetUserByID(ctx, user.ID)
		if err != nil {
			return nil, false, fmt.Errorf("get user after openid login: %w", err)
		}
		return &fullUser, false, nil
	}
	if !stderrors.Is(err, pgx.ErrNoRows) {
		// A-FIX-05：同上，真实 DB 错误向上返回 500。
		return nil, false, fmt.Errorf("get user by open id: %w", err)
	}

	userID, err := util.NewUUID()
	if err != nil {
		return nil, false, fmt.Errorf("generate user id: %w", err)
	}
	familyID, err := util.NewUUID()
	if err != nil {
		return nil, false, fmt.Errorf("generate family id: %w", err)
	}
	membershipID, err := util.NewUUID()
	if err != nil {
		return nil, false, fmt.Errorf("generate membership id: %w", err)
	}

	defaultNickname := randomNickname()
	var inviterID string
	if inviter != "" {
		_, err := h.pool.Queries().GetUserByID(ctx, inviter)
		if err == nil {
			inviterID = inviter
		} else if !stderrors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "check inviter failed, skipping invite reward", slog.String("inviter", util.MaskID(inviter)), slog.Any("error", err))
		}
	}

	if err := db.WithTx(ctx, h.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		if _, err := q.CreateFamily(ctx, sqlc.CreateFamilyParams{
			ID:         familyID,
			IsPersonal: true,
		}); err != nil {
			return fmt.Errorf("create family: %w", err)
		}

		if _, err := q.CreateUser(ctx, sqlc.CreateUserParams{
			ID:               userID,
			OpenID:           session.OpenID,
			Unionid:          toNullText(session.UnionID),
			Nickname:         toNullText(defaultNickname),
			UserType:         "wechat",
			SessionKey:       toNullText(session.SessionKey),
			PersonalFamilyID: toNullText(familyID),
			CurrentFamilyID:  toNullText(familyID),
			InvitedBy:        toNullText(inviterID),
		}); err != nil {
			return fmt.Errorf("create user: %w", err)
		}

		if _, err := q.UpsertFamilyMembership(ctx, sqlc.UpsertFamilyMembershipParams{
			ID:       membershipID,
			FamilyID: familyID,
			UserID:   userID,
			Role:     "owner",
		}); err != nil {
			return fmt.Errorf("upsert family membership: %w", err)
		}

		if err := h.vipService.IssueTrialVIPWithTx(ctx, userID, q); err != nil {
			return fmt.Errorf("issue trial vip: %w", err)
		}

		if err := h.applyInviteRewardsWithTx(ctx, q, userID, inviterID); err != nil {
			return fmt.Errorf("apply invite rewards: %w", err)
		}

		if _, err := createOrGetUserInviteCode(ctx, q, userID); err != nil {
			return fmt.Errorf("create invite code: %w", err)
		}

		return nil
	}); err != nil {
		if dbx.IsUniqueViolation(err) {

			var existingID string
			if existing, openErr := queries.GetUserByOpenID(ctx, session.OpenID); openErr == nil {
				existingID = existing.ID
			} else if session.UnionID != "" {
				if existingUnion, unionErr := queries.GetUserByUnionID(ctx, toNullText(session.UnionID)); unionErr == nil {
					existingID = existingUnion.ID
				}
			}
			if existingID != "" {
				if updateErr := queries.UpdateUserSessionKey(ctx, sqlc.UpdateUserSessionKeyParams{
					ID:         existingID,
					SessionKey: toNullText(session.SessionKey),
				}); updateErr != nil {
					return nil, false, fmt.Errorf("update session key after race: %w", updateErr)
				}
				if session.UnionID != "" {
					if unionErr := queries.UpdateUserUnionID(ctx, sqlc.UpdateUserUnionIDParams{
						ID:      existingID,
						Unionid: toNullText(session.UnionID),
					}); unionErr != nil {
						slog.ErrorContext(ctx, "update user union id after race failed", slog.String("user_id", existingID), slog.Any("error", unionErr))
					}
				}
				fullUser, getErr := queries.GetUserByID(ctx, existingID)
				if getErr != nil {
					return nil, false, fmt.Errorf("get user after race: %w", getErr)
				}
				return &fullUser, false, nil
			}
		}
		return nil, false, err
	}

	newUser, err := queries.GetUserByID(ctx, userID)
	if err != nil {
		return nil, false, fmt.Errorf("get user after create: %w", err)
	}

	if h.avatarService != nil && h.defaultAvatar != "" {
		avatarURL := h.defaultAvatarURLString(userID)
		if avatarURL != "" {
			safe.Go(ctx, nil, func() {
				bgCtx, cancel := context.WithTimeout(context.Background(), avatar.GenerateMarkerTimeout)
				defer cancel()
				if _, err := h.avatarService.GenerateMarker(bgCtx, userID, avatarURL); err != nil {
					slog.ErrorContext(bgCtx, "generate avatar marker for new user failed", slog.String("user_id", userID), slog.String("avatar", avatarURL), slog.Any("error", err))
					return
				}
				queries := h.pool.Queries()
				if h.bgPool != nil {
					queries = h.bgPool.Queries()
				}
				if err := queries.UpdateUserAvatar(bgCtx, sqlc.UpdateUserAvatarParams{
					ID:           userID,
					Avatar:       pgtype.Text{String: avatarURL, Valid: true},
					AvatarFileID: pgtype.Text{},
				}); err != nil {
					slog.ErrorContext(bgCtx, "update new user avatar failed", slog.String("user_id", userID), slog.Any("error", err))
				}
			})
		}
	}

	return &newUser, true, nil
}

// applyInviteRewardsWithTx 邀请奖励下发（R4 抽取）：注册期绑定与登录后补绑共用同一实现，
// 保证两条路径的奖励逻辑永不漂移。inviterID 为空时不做任何事。
func (h *Handler) applyInviteRewardsWithTx(ctx context.Context, q *sqlc.Queries, inviteeID, inviterID string) error {
	if inviterID == "" {
		return nil
	}
	// 事务内确认邀请人仍存在；若已被删除则跳过奖励，避免外键约束失败。
	// 瞬时 DB 错误不再静默吞掉（有日志可排查），但仍跳过奖励以保证注册主流程不被拖累。
	if _, checkErr := q.GetUserByID(ctx, inviterID); checkErr != nil {
		if !stderrors.Is(checkErr, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "check inviter failed, skip invite reward", slog.String("inviter_id", inviterID), slog.Any("error", checkErr))
		}
		return nil
	}
	inviteID, err := util.NewUUID()
	if err != nil {
		return fmt.Errorf("generate invite id: %w", err)
	}
	// R-21：冗余被邀请人 openid——注销后邀请行保留（user_id 置 NULL），作为被邀请奖励终身一次的判定依据。
	invitee, err := q.GetUserByID(ctx, inviteeID)
	if err != nil {
		return fmt.Errorf("get invitee user: %w", err)
	}
	inviteeOpenID := pgtype.Text{}
	if invitee.OpenID != "" {
		inviteeOpenID = pgtype.Text{String: invitee.OpenID, Valid: true}
	}
	if _, err := q.CreateUserInvite(ctx, sqlc.CreateUserInviteParams{
		ID:         inviteID,
		UserID:     toNullText(inviteeID),
		InviterID:  toNullText(inviterID),
		UserOpenID: inviteeOpenID,
	}); err != nil {
		return fmt.Errorf("create user invite: %w", err)
	}

	// R-21：先标记后发奖——同一微信主体（openid）已终身领取过被邀请奖励时跳过 +3
	//（部分唯一索引 uq_user_invites_user_open_id 在 DB 层兜底，I1/I11），不阻断注册/绑定主流程，
	// inviter 侧奖励不受影响（月度 14 天封顶即天花板）。
	markRows, err := q.MarkInviteeRewarded(ctx, inviteID)
	if err != nil {
		if !dbx.IsUniqueViolation(err) {
			return fmt.Errorf("mark invitee rewarded: %w", err)
		}
		slog.InfoContext(ctx, "invitee reward skipped: openid already rewarded", slog.String("invitee_id", inviteeID))
	}
	if markRows > 0 {
		if err := h.vipService.ExtendVIPDaysWithTx(ctx, inviteeID, 3, q); err != nil {
			return fmt.Errorf("extend invitee vip: %w", err)
		}
	}

	now := timeutil.NowShanghai()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, timeutil.Shanghai)
	monthEnd := monthStart.AddDate(0, 1, 0)
	if err := q.LockInviterReward(ctx, toNullText(inviterID)); err != nil {
		return fmt.Errorf("lock inviter reward: %w", err)
	}
	rewardedDays, err := q.CountInviterMonthlyRewardDays(ctx, sqlc.CountInviterMonthlyRewardDaysParams{
		InviterID:         toNullText(inviterID),
		RewardInviterAt:   pgtype.Timestamptz{Time: monthStart, Valid: true},
		RewardInviterAt_2: pgtype.Timestamptz{Time: monthEnd, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("count inviter monthly reward days: %w", err)
	}
	if rewardedDays < 14 {
		// Mark first, then extend, so concurrent logins cannot double-reward.
		rewardedRows, err := q.MarkInviterRewarded(ctx, inviteID)
		if err != nil {
			return fmt.Errorf("mark inviter rewarded: %w", err)
		}
		if rewardedRows > 0 {
			if err := h.vipService.ExtendVIPDaysWithTx(ctx, inviterID, 7, q); err != nil {
				return fmt.Errorf("extend inviter vip: %w", err)
			}
		}
	}
	return nil
}

func generateUserInviteCode() string {
	chars := []rune(inviteCodeChars)
	max := big.NewInt(int64(len(chars)))
	b := make([]rune, 8)
	for i := 0; i < 8; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			b[i] = chars[0]
			continue
		}
		b[i] = chars[n.Int64()]
	}
	return string(b)
}

func createOrGetUserInviteCode(ctx context.Context, q *sqlc.Queries, userID string) (string, error) {
	shortCode := generateUserInviteCode()
	if _, err := q.CreateUserInviteCode(ctx, sqlc.CreateUserInviteCodeParams{
		UserID:    userID,
		ShortCode: shortCode,
	}); err != nil {
		if dbx.IsUniqueViolation(err) {
			// 并发或冲突时复用该用户已存在的短码，与二维码生成侧 ensureShortCode 保持一致。
			if existing, getErr := q.GetUserInviteCode(ctx, userID); getErr == nil && existing != "" {
				return existing, nil
			}
		}
		return "", fmt.Errorf("create user invite code: %w", err)
	}
	return shortCode, nil
}

const inviteCodeChars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func (h *Handler) defaultAvatarURLString(userID string) string {
	if h.defaultAvatar == "" {
		return ""
	}
	return h.defaultAvatar + userID
}

func (h *Handler) GetWechatClient() *WechatClient {
	return h.wechat
}

func toNullText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func nowPgxTimestamptz() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now(), Valid: true}
}

var nicknameAdjectives = []string{
	"小熊", "小鹿", "小兔", "小猫", "小狗", "小熊猫", "小松鼠", "小狐狸",
	"向日葵", "蒲公英", "小雏菊", "四叶草", "樱花", "桂花", "梅花",
	"小星星", "小月亮", "小云朵", "小彩虹", "小太阳",
}

func randomNickname() string {
	adjIdx, err := rand.Int(rand.Reader, big.NewInt(int64(len(nicknameAdjectives))))
	if err != nil {
		// 系统熵源异常时不阻塞登录，回退到确定性昵称。
		return fmt.Sprintf("用户_%d", time.Now().Unix()%10000)
	}
	suffix, err := rand.Int(rand.Reader, big.NewInt(9000))
	if err != nil {
		return fmt.Sprintf("%s_%d", nicknameAdjectives[adjIdx.Int64()], time.Now().Unix()%9000)
	}
	return fmt.Sprintf("%s_%d", nicknameAdjectives[adjIdx.Int64()], 1000+suffix.Int64())
}
