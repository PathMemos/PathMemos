package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/middleware"
	"papafeiji/backend/pkg/timeutil"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

func userRowPhone(id, phone string, bindTime pgtype.Timestamptz) *pgxmock.Rows {
	now := time.Now()
	return pgxmock.NewRows([]string{
		"id", "open_id", "unionid", "phone_number", "avatar", "avatar_file_id",
		"nickname", "user_type", "phone_bind_time", "auto_record_enabled",
		"personal_family_id", "current_family_id", "invited_by", "lang",
		"created_at", "updated_at", "abnormal_subscribe_accepted", "last_active_at",
	}).AddRow(id, "openid-"+id, nil, pgtype.Text{String: phone, Valid: phone != ""}, nil, nil,
		pgtype.Text{String: "测试用户", Valid: true}, "wechat", bindTime, false,
		pgtype.Text{String: "fam-" + id, Valid: true}, pgtype.Text{String: "fam-" + id, Valid: true},
		nil, "zh", now, now, false, now)
}

func phoneRequest(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(middleware.WithUserID(req.Context(), "u1"))
}

// TestBindPhone_LockedToday 当日已绑定 → 400 且不调用微信、不写库。
func TestBindPhone_LockedToday(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()

	h := &Handler{pool: db.NewPoolWithDBTX(mock)}
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("u1").
		WillReturnRows(userRowPhone("u1", "13800000000", pgtype.Timestamptz{Time: timeutil.NowShanghai(), Valid: true}))

	rec := httptest.NewRecorder()
	h.BindPhone(rec, phoneRequest("/auth/phone/bind", `{"code":"x"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestLogin_DBErrorOnOpenIDLookup A-FIX-05：DB 抖动不得当作「无此用户」而误建新账号。
func TestLogin_DBErrorOnOpenIDLookup(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()
	wc, _ := newWechatTestServer(t)
	h, _, _ := newLoginHandler(mock, wc)

	mock.ExpectQuery("FROM users WHERE open_id = \\$1").
		WithArgs("openid-1").
		WillReturnError(errors.New("db down"))

	rec := httptest.NewRecorder()
	h.Login(rec, loginRequest("valid-code"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (must not create user on DB error)", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestBindPhone_DailyLimitRowsZero A-AC-19（handler 分支）：原子日限更新 0 行 → 400，
// 即使预检查通过（并发下他人已在本日绑定）。原子谓词本身已在真实库验证。
func TestBindPhone_DailyLimitRowsZero(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()
	wc, _ := newWechatTestServer(t)
	h := &Handler{pool: db.NewPoolWithDBTX(mock), wechat: wc}

	// 预检查：未在今天绑定过
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("u1").
		WillReturnRows(userRowPhone("u1", "", pgtype.Timestamptz{}))
	// 微信置换成功；原子更新影响 0 行 → 日限拦截
	mock.ExpectExec("UPDATE users SET").
		WithArgs("u1", pgtype.Text{String: "13800000000", Valid: true}, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	rec := httptest.NewRecorder()
	h.BindPhone(rec, phoneRequest("/auth/phone/bind", `{"code":"valid-phone"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (atomic daily limit)", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
