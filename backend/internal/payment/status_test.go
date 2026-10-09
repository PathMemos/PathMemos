package payment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"papafeiji/backend/internal/middleware"
	pkgerrors "papafeiji/backend/pkg/errors"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

type statusBody struct {
	Code    string `json:"code"`
	BizCode string `json:"biz_code"`
	Message string `json:"message"`
}

func doStatus(t *testing.T, mock pgxmock.PgxPoolIface, outTradeNo string) *httptest.ResponseRecorder {
	t.Helper()
	h := newNotifyHandler(mock, &mockVIPService{})
	req := httptest.NewRequest(http.MethodGet, "/payment/virtual/status?outTradeNo="+outTradeNo, nil)
	req = req.WithContext(middleware.WithUserID(req.Context(), "u1"))
	rec := httptest.NewRecorder()
	h.Status(rec, req)
	return rec
}

// TestStatus_OrderNotFound 订单不存在 → 404 且下发 biz_code=ORDER_NOT_FOUND。
func TestStatus_OrderNotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-404").
		WillReturnError(pgx.ErrNoRows)

	rec := doStatus(t, mock, "OT-404")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body statusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.BizCode != pkgerrors.BizOrderNotFound {
		t.Fatalf("biz_code = %q, want %q", body.BizCode, pkgerrors.BizOrderNotFound)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestStatus_DBError 真实 DB 错误 → 500（而非伪装 404），避免掩盖故障。
func TestStatus_DBError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-500").
		WillReturnError(context.DeadlineExceeded)

	rec := doStatus(t, mock, "OT-500")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (real db error must not be 404)", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestStatus_OK 本人订单 → 200 返回 state。
func TestStatus_OK(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "paid", 100))

	rec := doStatus(t, mock, "OT-1001")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload struct {
		Data struct {
			State string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Data.State != "paid" {
		t.Fatalf("state = %q, want paid", payload.Data.State)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestStatus_OtherUserOrder 他人订单 → 404，不泄露存在性。
func TestStatus_OtherUserOrder(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u2", "vip-month-0001", "OT-1001", "paid", 100))

	rec := doStatus(t, mock, "OT-1001")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
