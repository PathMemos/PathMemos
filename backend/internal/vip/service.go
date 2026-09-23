package vip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"time"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/pkg/timeutil"
	"papafeiji/backend/pkg/util"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	trialVIPID = "vip-trial-0001"
)

type Service struct {
	pool *db.Pool
}

func NewService(pool *db.Pool) *Service {
	return &Service{pool: pool}
}

type Info struct {
	IsVIP      bool
	ExpireTime interface{}
}

type InfoProvider interface {
	GetVIPInfo(ctx context.Context, userID string) (Info, error)
}

var _ InfoProvider = (*Service)(nil)

// IssueTrialVIPWithTx 注册事务内发放 trial。同一微信主体注销重注册时，openid 墓碑
// （user_vip_claims 部分唯一索引，R-21）使 UpsertVIPClaim rowsAffected==0；此处静默
// 跳过发放而不回滚注册（R-26）——否则注销用户将永久无法重新注册。
// /vip/new-user 端点的 409 语义由 ClaimTrialVIP→ActivateVIPWithTx 独立承载，不受影响。
func (s *Service) IssueTrialVIPWithTx(ctx context.Context, userID string, q *sqlc.Queries) error {
	err := s.ActivateVIPWithTx(ctx, userID, trialVIPID, q)
	if trialGrantSkipped(err) {
		slog.InfoContext(ctx, "trial already claimed (openid tombstone), skip grant on registration", "user_id", userID)
		return nil
	}
	return err
}

// trialGrantSkipped 报告注册路径的墓碑冲突是否应按「跳过发放」容忍。
func trialGrantSkipped(err error) bool {
	return errors.Is(err, ErrTrialVIPAlreadyClaimed)
}

func (s *Service) ClaimTrialVIP(ctx context.Context, userID string) error {
	return db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		return s.ActivateVIPWithTx(ctx, userID, trialVIPID, q)
	})
}

func (s *Service) ClaimFreeVIP(ctx context.Context, userID, vipID string) error {
	return db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		vipRecord, err := q.GetVIPByID(ctx, vipID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrInvalidVIP
			}

			return fmt.Errorf("get vip by id: %w", err)
		}
		if vipRecord.Type != "free" || !vipRecord.IsActive {
			return ErrInvalidVIP
		}
		return s.activateVIPWithTx(ctx, userID, vipRecord, q)
	})
}

func (s *Service) HasVIPClaim(ctx context.Context, userID, vipID string) (bool, error) {
	return s.hasVIPClaimWithQ(ctx, s.pool.Queries(), userID, vipID)
}

func (s *Service) hasVIPClaimWithQ(ctx context.Context, q *sqlc.Queries, userID, vipID string) (bool, error) {
	if userID == "" {
		return false, fmt.Errorf("empty user id")
	}
	// R-21：优先按微信主体（openid）判重——注销重注册得到新 user_id 后仍能识别已领取；
	// openid 缺失（异常数据）时回退 user_id 维度。
	claimant, err := q.GetUserByID(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("get claimant user: %w", err)
	}
	if claimant.OpenID != "" {
		exists, err := q.HasVIPClaimByOpenID(ctx, sqlc.HasVIPClaimByOpenIDParams{
			OpenID: pgtype.Text{String: claimant.OpenID, Valid: true},
			VipID:  vipID,
		})
		if err != nil {
			return false, err
		}
		return exists, nil
	}
	exists, err := q.HasVIPClaim(ctx, sqlc.HasVIPClaimParams{
		UserID: pgtype.Text{String: userID, Valid: userID != ""},
		VipID:  vipID,
	})
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *Service) ExtendVIPDaysWithTx(ctx context.Context, userID string, days int, q *sqlc.Queries) error {
	if userID == "" {
		return fmt.Errorf("empty user id")
	}
	if days <= 0 {
		return fmt.Errorf("days must be positive, got %d", days)
	}

	now := timeutil.NowShanghai()
	existing, err := q.GetUserVIPForUpdate(ctx, userID)
	var begin time.Time
	var expire time.Time
	var id string
	if err == nil {
		begin = existing.BeginTime.Time
		base := now
		if existing.ExpireTime.Time.After(now) {
			base = existing.ExpireTime.Time
		}
		expire = base.AddDate(0, 0, days)
		id = existing.ID
	} else if errors.Is(err, pgx.ErrNoRows) {
		// VP-P2-02：并发首次下发（无 user_vips 行）时 GetUserVIPForUpdate 锁不住不存在的行，
		// 两个并发调用会各自按 now 计算 expire，UpsertUserVIP 的 GREATEST 只保留较大者，
		// 较小档时长被吞。先 ON CONFLICT DO NOTHING 占位，再 FOR UPDATE 重读串行化创建。
		newID, err := util.NewUUID()
		if err != nil {
			return fmt.Errorf("generate user vip id: %w", err)
		}
		if err := q.ClaimUserVIPRow(ctx, sqlc.ClaimUserVIPRowParams{
			ID:         newID,
			UserID:     userID,
			BeginTime:  pgtype.Timestamptz{Time: now, Valid: true},
			ExpireTime: pgtype.Timestamptz{Time: now.Add(time.Second), Valid: true},
		}); err != nil {
			return fmt.Errorf("claim user vip row: %w", err)
		}
		claimed, err := q.GetUserVIPForUpdate(ctx, userID)
		if err != nil {
			return fmt.Errorf("re-read user vip for update: %w", err)
		}
		begin = claimed.BeginTime.Time
		base := now
		if claimed.ExpireTime.Time.After(now) {
			base = claimed.ExpireTime.Time
		}
		expire = base.AddDate(0, 0, days)
		id = claimed.ID
	} else {
		return fmt.Errorf("get user vip: %w", err)
	}

	_, err = q.UpsertUserVIP(ctx, sqlc.UpsertUserVIPParams{
		ID:         id,
		UserID:     userID,
		BeginTime:  pgtype.Timestamptz{Time: begin, Valid: true},
		ExpireTime: pgtype.Timestamptz{Time: expire, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("upsert user vip: %w", err)
	}
	return nil
}

func (s *Service) GetVIPInfo(ctx context.Context, userID string) (Info, error) {
	uv, err := s.pool.Queries().GetUserVIP(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Info{IsVIP: false, ExpireTime: nil}, nil
		}
		return Info{}, fmt.Errorf("get user vip: %w", err)
	}

	isVIP := uv.ExpireTime.Time.After(timeutil.NowShanghai())
	expire := uv.ExpireTime.Time.In(timeutil.Shanghai).Format("2006-01-02T15:04:05-07:00")

	return Info{IsVIP: isVIP, ExpireTime: expire}, nil
}

func (s *Service) ActivateVIPWithTx(ctx context.Context, userID, vipID string, q *sqlc.Queries) error {
	vipRecord, err := q.GetVIPByID(ctx, vipID)
	if err != nil {
		return fmt.Errorf("get vip by id: %w", err)
	}
	return s.activateVIPWithTx(ctx, userID, vipRecord, q)
}

func (s *Service) activateVIPWithTx(ctx context.Context, userID string, vipRecord sqlc.Vip, q *sqlc.Queries) error {
	duration, unit, err := ParseVIPDuration(vipRecord.TimeLimitMark, int(vipRecord.TimeLimitNumber))
	if err != nil {
		return fmt.Errorf("parse vip duration: %w", err)
	}

	now := timeutil.NowShanghai()

	// R-21：冗余记录发放主体 openid——注销后领取墓碑行（user_id 置 NULL）仍凭 open_id 防重。
	claimant, err := q.GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get claimant user: %w", err)
	}
	openID := pgtype.Text{}
	if claimant.OpenID != "" {
		openID = pgtype.Text{String: claimant.OpenID, Valid: true}
	}

	claimID, err := util.NewUUID()
	if err != nil {
		return fmt.Errorf("generate vip claim id: %w", err)
	}
	rowsAffected, err := q.UpsertVIPClaim(ctx, sqlc.UpsertVIPClaimParams{
		ID:     claimID,
		UserID: pgtype.Text{String: userID, Valid: userID != ""},
		VipID:  vipRecord.ID,
		OpenID: openID,
	})
	if err != nil {
		return fmt.Errorf("upsert vip claim: %w", err)
	}
	if rowsAffected == 0 {
		switch vipRecord.Type {
		case "trial":
			return ErrTrialVIPAlreadyClaimed
		case "free":
			return ErrFreeVIPAlreadyClaimed
		}
	}

	existing, err := q.GetUserVIPForUpdate(ctx, userID)
	if err == nil {
		begin := existing.BeginTime.Time
		base := now
		if existing.ExpireTime.Time.After(now) {
			base = existing.ExpireTime.Time
		}
		var added time.Time
		switch unit {
		case "day":
			added = base.AddDate(0, 0, duration)
		case "month":
			added = base.AddDate(0, duration, 0)
		case "year":
			added = base.AddDate(duration, 0, 0)
		}
		expire := added

		_, err = q.UpsertUserVIP(ctx, sqlc.UpsertUserVIPParams{
			ID:         existing.ID,
			UserID:     userID,
			BeginTime:  pgtype.Timestamptz{Time: begin, Valid: true},
			ExpireTime: pgtype.Timestamptz{Time: expire, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("upsert user vip (extend): %w", err)
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		// 并发首次激活竞态：先 ON CONFLICT DO NOTHING 占位行，再 FOR UPDATE 重读串行化创建，
		// 使后提交者从先提交者的 expire_time 叠加时长，避免 GREATEST 只保留较大者、较小档丢失。
		newUserVipID, err := util.NewUUID()
		if err != nil {
			return fmt.Errorf("generate user vip id: %w", err)
		}
		// 占位行必须满足 CHECK (expire_time > begin_time)：expire 取 now+1s（严格大于 begin），
		// 该值仅占位，紧接的 FOR UPDATE 重读 + UpsertUserVIP 会立即改写为真实时长。
		if err := q.ClaimUserVIPRow(ctx, sqlc.ClaimUserVIPRowParams{
			ID:         newUserVipID,
			UserID:     userID,
			BeginTime:  pgtype.Timestamptz{Time: now, Valid: true},
			ExpireTime: pgtype.Timestamptz{Time: now.Add(time.Second), Valid: true},
		}); err != nil {
			return fmt.Errorf("claim user vip row: %w", err)
		}
		claimed, err := q.GetUserVIPForUpdate(ctx, userID)
		if err != nil {
			return fmt.Errorf("re-read user vip for update: %w", err)
		}
		base := now
		if claimed.ExpireTime.Time.After(now) {
			base = claimed.ExpireTime.Time
		}
		var extended time.Time
		switch unit {
		case "day":
			extended = base.AddDate(0, 0, duration)
		case "month":
			extended = base.AddDate(0, duration, 0)
		case "year":
			extended = base.AddDate(duration, 0, 0)
		}
		_, err = q.UpsertUserVIP(ctx, sqlc.UpsertUserVIPParams{
			ID:         claimed.ID,
			UserID:     userID,
			BeginTime:  claimed.BeginTime,
			ExpireTime: pgtype.Timestamptz{Time: extended, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("upsert user vip (create): %w", err)
		}
	} else {
		return fmt.Errorf("get user vip: %w", err)
	}

	return nil
}

func ParseVIPDuration(mark string, number int) (int, string, error) {
	if number <= 0 {
		return 0, "", fmt.Errorf("invalid vip time_limit_number: %d (must be > 0)", number)
	}
	switch mark {
	case "day", "month", "year":
		return number, mark, nil
	default:
		return 0, "", fmt.Errorf("invalid vip time_limit_mark: %s", mark)
	}
}
