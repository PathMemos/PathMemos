package family

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

type stubVipService struct{ extendCalls int }

func (s *stubVipService) ExtendVIPDaysWithTx(ctx context.Context, userID string, days int, q *sqlc.Queries) error {
	s.extendCalls++
	return nil
}

// captureLogs 替换默认 slog logger 捕获输出（grantJoinReward 经默认 logger 落日志）。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// userRow 构造 sqlc.GetUserByIDRow 扫描所需的 18 列 mock 行（列序与 GetUserByID 投影一致）。
func familyUserRow(id, openID string) *pgxmock.Rows {
	now := time.Now()
	return pgxmock.NewRows([]string{
		"id", "open_id", "unionid", "phone_number", "avatar", "avatar_file_id",
		"nickname", "user_type", "phone_bind_time", "auto_record_enabled",
		"personal_family_id", "current_family_id", "invited_by", "lang",
		"created_at", "updated_at", "abnormal_subscribe_accepted", "last_active_at",
	}).AddRow(id, openID, nil, nil, nil, nil,
		pgtype.Text{String: "测试用户", Valid: true}, "wechat", nil, false,
		pgtype.Text{String: "fam-" + id, Valid: true}, pgtype.Text{String: "fam-" + id, Valid: true},
		nil, "zh", now, now, false, now)
}

// expectRewardPathUntilCreateInvite 铺设 grantJoinReward 到 CreateUserInvite 为止的全部 mock 期望。
func expectRewardPathUntilCreateInvite(mock pgxmock.PgxPoolIface, createInviteErr error) {
	// 事务外预检：无邀请记录、用户在 7 天窗口内、目标家庭有 owner。
	mock.ExpectQuery(`FROM user_invites WHERE user_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM users WHERE id = \$1`).
		WithArgs("invitee-1").
		WillReturnRows(familyUserRow("invitee-1", "openid-ee"))
	mock.ExpectQuery(`FROM family_members WHERE family_id = \$1 AND role = 'owner'`).
		WithArgs("fam-target").
		WillReturnRows(pgxmock.NewRows([]string{"user_id"}).AddRow("owner-1"))
	// WithTxDeferrable（RepeatableRead + Deferrable）；pgxmock 校验事务选项需显式给出。
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, DeferrableMode: pgx.Deferrable})
	mock.ExpectQuery(`FROM user_invites WHERE user_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM users WHERE id = \$1`).
		WithArgs("owner-1").
		WillReturnRows(familyUserRow("owner-1", "openid-owner"))
	// CreateUserInvite 为 sqlc :one（QueryRow 语义），mock 用 ExpectQuery。
	mock.ExpectQuery(`INSERT INTO user_invites`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(createInviteErr)
}

// TestGrantJoinReward_TxFailureEmitsAlert 奖励事务失败必须产出 alert=invite_reward_failed
// 告警日志（资金相关发放失败不能只留 Warn 级日志无声丢失）。
func TestGrantJoinReward_TxFailureEmitsAlert(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	buf := captureLogs(t)
	vip := &stubVipService{}
	h := &Handler{pool: db.NewPoolWithDBTX(mock), vipService: vip}

	expectRewardPathUntilCreateInvite(mock, errors.New("db down"))
	mock.ExpectRollback()

	h.grantJoinReward(context.Background(), "invitee-1", "fam-target")

	logs := buf.String()
	if !strings.Contains(logs, "alert=invite_reward_failed") {
		t.Fatalf("expected alert=invite_reward_failed in logs, got:\n%s", logs)
	}
	if !strings.Contains(logs, "grant join reward failed") {
		t.Fatalf("expected failure message preserved in logs, got:\n%s", logs)
	}
	if vip.extendCalls != 0 {
		t.Fatalf("expected 0 vip extensions on tx failure, got %d", vip.extendCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGrantJoinReward_ConcurrentUniqueViolationNoAlert 并发补绑撞 user_invites_user_id_key：
// 幂等跳过属预期路径（对端事务已发奖），不得触发 invite_reward_failed 告警。
func TestGrantJoinReward_ConcurrentUniqueViolationNoAlert(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	buf := captureLogs(t)
	vip := &stubVipService{}
	h := &Handler{pool: db.NewPoolWithDBTX(mock), vipService: vip}

	expectRewardPathUntilCreateInvite(mock, &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"})
	mock.ExpectCommit()

	h.grantJoinReward(context.Background(), "invitee-1", "fam-target")

	if logs := buf.String(); strings.Contains(logs, "alert=invite_reward_failed") {
		t.Fatalf("idempotent skip must not alert, got:\n%s", logs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGrantJoinReward_ReRegistrationSkipsRewards 注销重注册（openid 墓碑命中）：
// 邀请行照常创建（"我的邀请"列表可见），但整体不发奖——+3/+7 均不发，家庭加入照常成功且无 alert。
func TestGrantJoinReward_ReRegistrationSkipsRewards(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	buf := captureLogs(t)
	vip := &stubVipService{}
	h := &Handler{pool: db.NewPoolWithDBTX(mock), vipService: vip}

	// 事务外预检：无邀请记录、用户在 7 天窗口内、目标家庭有 owner。
	mock.ExpectQuery(`FROM user_invites WHERE user_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM users WHERE id = \$1`).
		WithArgs("invitee-1").
		WillReturnRows(familyUserRow("invitee-1", "openid-ee"))
	mock.ExpectQuery(`FROM family_members WHERE family_id = \$1 AND role = 'owner'`).
		WithArgs("fam-target").
		WillReturnRows(pgxmock.NewRows([]string{"user_id"}).AddRow("owner-1"))
	// WithTxDeferrable（RepeatableRead + Deferrable）；pgxmock 校验事务选项需显式给出。
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, DeferrableMode: pgx.Deferrable})
	mock.ExpectQuery(`FROM user_invites WHERE user_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`FROM users WHERE id = \$1`).
		WithArgs("owner-1").
		WillReturnRows(familyUserRow("owner-1", "openid-owner"))
	// 邀请行先建（关系记录进邀请列表）。
	mock.ExpectQuery(`INSERT INTO user_invites`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "user_id", "inviter_id", "entry_count", "reward_inviter_at", "reward_invitee_at", "created_at", "user_open_id"}).
			AddRow("invite-1", "invitee-1", "owner-1", 0, nil, nil, time.Now(), "openid-ee"))
	// 墓碑命中：openid 曾领过被邀请奖励 → 跳过全部奖励。
	mock.ExpectQuery(`FROM user_invites WHERE user_open_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectCommit()

	h.grantJoinReward(context.Background(), "invitee-1", "fam-target")

	if logs := buf.String(); strings.Contains(logs, "alert=invite_reward_failed") {
		t.Fatalf("re-registration skip must not alert, got:\n%s", logs)
	}
	if vip.extendCalls != 0 {
		t.Fatalf("expected 0 vip extensions on re-registration, got %d", vip.extendCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
