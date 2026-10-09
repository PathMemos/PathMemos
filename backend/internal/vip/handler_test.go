package vip

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/middleware"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

func newVIPHandler(mock pgxmock.PgxPoolIface) *Handler {
	p := db.NewPoolWithDBTX(mock)
	return &Handler{pool: p, service: &Service{pool: p}}
}

func trialClaimRequest(userID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/vip/new-user", nil)
	return req.WithContext(middleware.WithUserID(req.Context(), userID))
}

func decodeVIPResp(t *testing.T, rec *httptest.ResponseRecorder) (int, string, string, string) {
	t.Helper()
	var resp struct {
		Code    string `json:"code"`
		BizCode string `json:"biz_code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, resp.Code, resp.BizCode, resp.Message
}

// TestClaimTrialVIP_DisabledProduct_Returns404 下线闸门（端点形态）：trial 商品已下线
// （is_active=false）时 POST /vip/new-user 返回 404 `trial vip disabled`，对齐 free 档
// freeVipDisabled 的语义——下线为端点不可用，直调 API 的旧客户端同样被拒。
func TestClaimTrialVIP_DisabledProduct_Returns404(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("vips WHERE id = \\$1 AND is_active = true").
		WithArgs("vip-trial-0001").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	h := newVIPHandler(mock)
	rec := httptest.NewRecorder()
	h.ClaimTrialVIP(rec, trialClaimRequest("u1"))

	status, code, _, msg := decodeVIPResp(t, rec)
	if status != http.StatusNotFound || code != "4040" || msg != "trial vip disabled" {
		t.Fatalf("want 404/4040/trial vip disabled, got %d/%s/%s", status, code, msg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestClaimTrialVIP_AlreadyClaimed_Returns409 已领取语义不变：商品在售但撞领取唯一约束
// （openid 墓碑/user_id 维度）仍走原 409 TRIAL_VIP_ALREADY_CLAIMED，不受下线闸门影响。
func TestClaimTrialVIP_AlreadyClaimed_Returns409(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("vips WHERE id = \\$1 AND is_active = true").
		WithArgs("vip-trial-0001").
		WillReturnRows(vipRows(mock, "vip-trial-0001", "trial", "day", 7))
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("u1").
		WillReturnRows(claimantUserRow("openid-u1"))
	mock.ExpectExec("INSERT INTO user_vip_claims").
		WithArgs(pgxmock.AnyArg(), pgtype.Text{String: "u1", Valid: true}, "vip-trial-0001", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0)) // ON CONFLICT DO NOTHING → 0 行
	mock.ExpectRollback()

	h := newVIPHandler(mock)
	rec := httptest.NewRecorder()
	h.ClaimTrialVIP(rec, trialClaimRequest("u1"))

	status, code, bizCode, msg := decodeVIPResp(t, rec)
	if status != http.StatusConflict || code != "4090" || bizCode != "TRIAL_VIP_ALREADY_CLAIMED" || msg != "trial vip already claimed" {
		t.Fatalf("want 409/4090/TRIAL_VIP_ALREADY_CLAIMED, got %d/%s/%s/%s", status, code, bizCode, msg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
