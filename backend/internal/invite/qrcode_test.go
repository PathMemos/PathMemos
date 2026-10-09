package invite

import (
	"context"
	"testing"
	"time"

	"papafeiji/backend/internal/db/sqlc"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

func inviteCodeRows(shortCode string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"user_id", "short_code", "created_at", "expires_at"}).
		AddRow("u1", shortCode, time.Now(), nil)
}

func uniqueViolation() error {
	return &pgconn.PgError{Code: "23505"}
}

// TestEnsureShortCode_RetriesOnShortCodeCollision A-FIX-06：短码全局唯一，碰撞换码重试。
func TestEnsureShortCode_RetriesOnShortCodeCollision(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("SELECT short_code FROM user_invite_codes WHERE user_id = \\$1").
		WithArgs("u1").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("INSERT INTO user_invite_codes").
		WithArgs("u1", pgxmock.AnyArg()).WillReturnError(uniqueViolation())
	mock.ExpectQuery("SELECT short_code FROM user_invite_codes WHERE user_id = \\$1").
		WithArgs("u1").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("INSERT INTO user_invite_codes").
		WithArgs("u1", pgxmock.AnyArg()).WillReturnRows(inviteCodeRows("abcd1234"))

	g := &QRCodeGenerator{}
	code, err := g.ensureShortCode(context.Background(), sqlc.New(mock), "u1")
	if err != nil {
		t.Fatalf("ensureShortCode: %v", err)
	}
	if len(code) != inviteShortCodeLen {
		t.Fatalf("code = %q, want length %d", code, inviteShortCodeLen)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestEnsureShortCode_ReusesExistingOnUserConflict 冲突来自 user_id 唯一键 → 复用本人已有码。
func TestEnsureShortCode_ReusesExistingOnUserConflict(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("SELECT short_code FROM user_invite_codes WHERE user_id = \\$1").
		WithArgs("u1").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("INSERT INTO user_invite_codes").
		WithArgs("u1", pgxmock.AnyArg()).WillReturnError(uniqueViolation())
	mock.ExpectQuery("SELECT short_code FROM user_invite_codes WHERE user_id = \\$1").
		WithArgs("u1").WillReturnRows(pgxmock.NewRows([]string{"short_code"}).AddRow("existing1"))

	g := &QRCodeGenerator{}
	code, err := g.ensureShortCode(context.Background(), sqlc.New(mock), "u1")
	if err != nil {
		t.Fatalf("ensureShortCode: %v", err)
	}
	if code != "existing1" {
		t.Fatalf("code = %q, want existing1", code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestEnsureShortCode_ExhaustsRetries 连续碰撞 3 次 → 返回错误而非无限循环。
func TestEnsureShortCode_ExhaustsRetries(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("SELECT short_code FROM user_invite_codes WHERE user_id = \\$1").
		WithArgs("u1").WillReturnError(pgx.ErrNoRows)
	for i := 0; i < 3; i++ {
		mock.ExpectQuery("INSERT INTO user_invite_codes").
			WithArgs("u1", pgxmock.AnyArg()).WillReturnError(uniqueViolation())
		mock.ExpectQuery("SELECT short_code FROM user_invite_codes WHERE user_id = \\$1").
			WithArgs("u1").WillReturnError(pgx.ErrNoRows)
	}

	g := &QRCodeGenerator{}
	if _, err := g.ensureShortCode(context.Background(), sqlc.New(mock), "u1"); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
