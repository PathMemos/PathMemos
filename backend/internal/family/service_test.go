package family

import (
	"context"
	"reflect"
	"testing"
	"time"

	"papafeiji/backend/internal/db/sqlc"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

// exactStrings 断言 SQL 参数等于给定 []string（pgxmock.Argument）。
type exactStrings struct {
	t    *testing.T
	want []string
}

func (m exactStrings) Match(v interface{}) bool {
	got, ok := v.([]string)
	if !ok || !reflect.DeepEqual(got, m.want) {
		m.t.Fatalf("string slice arg = %v (%T), want %v", v, v, m.want)
	}
	return true
}

// sameLenAs 断言 SQL 参数与给定切片等长（ids 与 userIds 一一对应的守卫，
// 不等长时 unnest 锁步补 NULL 违反 family_members.id NOT NULL）。
type sameLenAs struct {
	t    *testing.T
	want []string
}

func (m sameLenAs) Match(v interface{}) bool {
	got, ok := v.([]string)
	if !ok || len(got) != len(m.want) {
		m.t.Fatalf("string slice arg len = %d (%T), want %d", len(got), v, len(m.want))
	}
	return true
}

func TestRejoinBlocked(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	inCooldown := now.Add(-3 * 24 * time.Hour).Format(time.RFC3339)
	expired := now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)

	tests := []struct {
		name    string
		raw     string
		userIDs []string
		want    bool
	}{
		{"nil raw", "", []string{"u1"}, false},
		{"empty object", `{}`, []string{"u1"}, false},
		{"hit within cooldown", `{"u1":"` + inCooldown + `"}`, []string{"u1"}, true},
		{"hit exactly at edge", `{"u1":"` + now.Add(-removedRejoinCooldown).Format(time.RFC3339) + `"}`, []string{"u1"}, false},
		{"hit past cooldown", `{"u1":"` + expired + `"}`, []string{"u1"}, false},
		{"other user removed", `{"u2":"` + inCooldown + `"}`, []string{"u1"}, false},
		{"merge path any member hit", `{"u2":"` + expired + `","u3":"` + inCooldown + `"}`, []string{"u1", "u3"}, true},
		{"garbage timestamp tolerated", `{"u1":"not-a-time"}`, []string{"u1"}, false},
		{"garbage json tolerated", `not-json`, []string{"u1"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rejoinBlocked([]byte(tt.raw), tt.userIDs, now); got != tt.want {
				t.Fatalf("rejoinBlocked(%s, %v) = %v, want %v", tt.raw, tt.userIDs, got, tt.want)
			}
		})
	}
}

// TestJoinFamilyTx_MergeArraysAligned 断言 owner 整家合并路径
// BatchUpsertFamilyMembership 的 Ids 与 UserIds 等长且与成员一一对应
//（不等长时 unnest 锁步补 NULL，违反 family_members.id NOT NULL）。
func TestJoinFamilyTx_MergeArraysAligned(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	q := sqlc.New(mock)
	user := sqlc.GetUserByIDRow{ID: "u-owner"}
	user.CurrentFamilyID = pgtype.Text{String: "fam-src", Valid: true}

	mock.ExpectQuery("FROM family_members").
		WithArgs("fam-src").
		WillReturnRows(pgxmock.NewRows([]string{"user_id", "role", "joined_at", "avatar", "avatar_file_id", "nickname"}).
			AddRow("u-owner", "owner", now, nil, nil, nil).
			AddRow("u-b", "member", now, nil, nil, nil))
	mock.ExpectQuery("SELECT removed_members FROM families").
		WithArgs("fam-target").
		WillReturnRows(pgxmock.NewRows([]string{"removed_members"}).AddRow([]byte(`{}`)))
	mock.ExpectExec("INSERT INTO family_members").
		WithArgs(sameLenAs{t: t, want: []string{"u-owner", "u-b"}}, "fam-target", exactStrings{t: t, want: []string{"u-owner", "u-b"}}).
		WillReturnResult(pgxmock.NewResult("INSERT", 2))
	mock.ExpectExec("UPDATE users").
		WithArgs(exactStrings{t: t, want: []string{"u-owner", "u-b"}}, pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM family_daily_covers").
		WithArgs("fam-src").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectExec("DELETE FROM family_daily_covers").
		WithArgs("fam-src").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec("DELETE FROM family_members").
		WithArgs("fam-src").
		WillReturnResult(pgxmock.NewResult("DELETE", 2))
	mock.ExpectExec("DELETE FROM families").
		WithArgs("fam-src").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	s := &Service{}
	if err := s.joinFamilyTx(context.Background(), q, user, "fam-target", true); err != nil {
		t.Fatalf("joinFamilyTx: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestJoinFamilyTx_MergeMemberInCooldown 合并成员含冷却名单内成员 → 拒绝且不触达迁移写入。
func TestJoinFamilyTx_MergeMemberInCooldown(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	q := sqlc.New(mock)
	user := sqlc.GetUserByIDRow{ID: "u-owner"}
	user.CurrentFamilyID = pgtype.Text{String: "fam-src", Valid: true}

	mock.ExpectQuery("FROM family_members").
		WithArgs("fam-src").
		WillReturnRows(pgxmock.NewRows([]string{"user_id", "role", "joined_at", "avatar", "avatar_file_id", "nickname"}).
			AddRow("u-owner", "owner", now, nil, nil, nil).
			AddRow("u-b", "member", now, nil, nil, nil))
	recent := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	mock.ExpectQuery("SELECT removed_members FROM families").
		WithArgs("fam-target").
		WillReturnRows(pgxmock.NewRows([]string{"removed_members"}).AddRow([]byte(`{"u-b":"` + recent + `"}`)))

	s := &Service{}
	err = s.joinFamilyTx(context.Background(), q, user, "fam-target", true)
	if err != ErrRemovedRejoinCooldown {
		t.Fatalf("err = %v, want ErrRemovedRejoinCooldown", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
