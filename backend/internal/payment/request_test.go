package payment

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/middleware"
)

func doRequest(t *testing.T, allowSandbox bool, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{cfg: &config.Config{PaymentAllowSandbox: allowSandbox}}
	req := httptest.NewRequest(http.MethodPost, "/payment/virtual/request", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(middleware.WithUserID(req.Context(), "u1"))
	rec := httptest.NewRecorder()
	h.Request(rec, req)
	return rec
}

// TestRequestSandboxGate 生产默认拒绝沙箱 env=1（闸门先于任何 DB 访问）。
func TestRequestSandboxGate(t *testing.T) {
	rec := doRequest(t, false, `{"vipId":"vip-month-0001","env":1}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("sandbox disabled status = %d, want 403", rec.Code)
	}
}

func TestRequestInvalidEnv(t *testing.T) {
	rec := doRequest(t, true, `{"vipId":"vip-month-0001","env":2}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid env status = %d, want 400", rec.Code)
	}
}

func TestRequestMissingVipID(t *testing.T) {
	rec := doRequest(t, false, `{"env":0}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing vipId status = %d, want 400", rec.Code)
	}
}

func TestRequestInvalidBody(t *testing.T) {
	rec := doRequest(t, false, `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body status = %d, want 400", rec.Code)
	}
}
