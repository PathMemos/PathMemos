package payment

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/vip"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

// mockVIPService 记录 ActivateVIPWithTx 调用，供 handleNotify 测试断言发货行为。
type mockVIPService struct {
	activateCalls int
	activateUID   string
	activateVipID string
}

func (m *mockVIPService) GetVIPInfo(_ context.Context, _ string) (vip.Info, error) {
	return vip.Info{IsVIP: false}, nil
}

func (m *mockVIPService) ActivateVIPWithTx(_ context.Context, userID, vipID string, _ *sqlc.Queries) error {
	m.activateCalls++
	m.activateUID = userID
	m.activateVipID = vipID
	return nil
}

// orderRows 构造 sqlc.Order 扫描所需的 11 列 mock 行（prepay_id 已随 000012 摘除）。
func orderRows(id, userID, vipID, outTradeNo, state string, amount int32) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "user_id", "vip_id", "out_trade_no", "channel", "state", "amount",
		"transaction_id", "paid_at", "created_at", "updated_at",
	}).AddRow(id, pgtype.Text{String: userID, Valid: true}, vipID, outTradeNo,
		"virtual_pay", state, amount, nil, nil, time.Now(), time.Now())
}

func notifyPayload(outTradeNo, transactionID string, amount int64) map[string]interface{} {
	return map[string]interface{}{
		"OutTradeNo": outTradeNo,
		"WeChatPayInfo": map[string]interface{}{
			"TransactionId": transactionID,
		},
		"GoodsInfo": map[string]interface{}{
			"ActualPrice": float64(amount),
		},
		"event": "xpay_goods_deliver_notify",
	}
}

func newNotifyHandler(mock pgxmock.PgxPoolIface, v *mockVIPService) *Handler {
	return &Handler{
		pool:       db.NewPoolWithDBTX(mock),
		vipService: v,
	}
}

// TestHandleNotify_Delivery 正常发货：pending 订单 → tx 内 UpdateOrderPaid 1 行 → ActivateVIPWithTx 被调用 → 无错误。
func TestHandleNotify_Delivery(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "pending", 100))
	mock.ExpectBegin()
	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "pending", 100))
	mock.ExpectExec("UPDATE orders SET").
		WithArgs("OT-1001", pgtype.Text{String: "WX-1001", Valid: true}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	v := &mockVIPService{}
	h := newNotifyHandler(mock, v)
	err = h.handleNotify(context.Background(), notifyPayload("OT-1001", "WX-1001", 100))
	if err != nil {
		t.Fatalf("handleNotify delivery failed: %v", err)
	}
	if v.activateCalls != 1 || v.activateUID != "u1" || v.activateVipID != "vip-month-0001" {
		t.Fatalf("ActivateVIPWithTx calls = %d (uid=%q vip=%q), want 1 (u1, vip-month-0001)",
			v.activateCalls, v.activateUID, v.activateVipID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestHandleNotify_DuplicateNotify 重复通知：UpdateOrderPaid 0 行（已支付）→ 幂等成功、不调用 Activate。
func TestHandleNotify_DuplicateNotify(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "paid", 100))
	mock.ExpectBegin()
	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "paid", 100))
	mock.ExpectExec("UPDATE orders SET").
		WithArgs("OT-1001", pgtype.Text{String: "WX-1001", Valid: true}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0)) // 已支付：WHERE state='pending' 不命中
	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "paid", 100))
	mock.ExpectCommit()

	v := &mockVIPService{}
	h := newNotifyHandler(mock, v)
	err = h.handleNotify(context.Background(), notifyPayload("OT-1001", "WX-1001", 100))
	if err != nil {
		t.Fatalf("duplicate notify should be idempotent success, got: %v", err)
	}
	if v.activateCalls != 0 {
		t.Fatalf("ActivateVIPWithTx calls = %d, want 0 (idempotent no-op)", v.activateCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestHandleNotify_AmountMismatchDelivers VP-P1-01：金额与标价不一致（平台立减/优惠）时，
// 回调已验签且用户已扣款，应照常发货而不是业务拒绝静默漏发。
func TestHandleNotify_AmountMismatchDelivers(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "pending", 100))
	mock.ExpectBegin()
	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "pending", 100))
	mock.ExpectExec("UPDATE orders SET").
		WithArgs("OT-1001", pgtype.Text{String: "WX-1001", Valid: true}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	v := &mockVIPService{}
	h := newNotifyHandler(mock, v)
	err = h.handleNotify(context.Background(), notifyPayload("OT-1001", "WX-1001", 99))
	if err != nil {
		t.Fatalf("amount mismatch should still deliver, got: %v", err)
	}
	if v.activateCalls != 1 {
		t.Fatalf("ActivateVIPWithTx calls = %d, want 1 (deliver despite discount)", v.activateCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestHandleNotify_AmountInvalid 金额为 0/负数 → errNotifyRejected，不进入事务。
func TestHandleNotify_AmountInvalid(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "pending", 100))

	v := &mockVIPService{}
	h := newNotifyHandler(mock, v)
	err = h.handleNotify(context.Background(), notifyPayload("OT-1001", "WX-1001", 0))
	if !errors.Is(err, errNotifyRejected) {
		t.Fatalf("want errNotifyRejected for non-positive amount, got %v", err)
	}
	if v.activateCalls != 0 {
		t.Fatalf("ActivateVIPWithTx calls = %d, want 0", v.activateCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestHandleNotify_ClosedOrderReissued VP-P1-02：订单已被本地关闭但用户完成支付，
// 补记为 paid 并自动发货，而非告警漏发。
func TestHandleNotify_ClosedOrderReissued(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "closed", 100))
	mock.ExpectBegin()
	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "closed", 100))
	mock.ExpectExec("UPDATE orders SET").
		WithArgs("OT-1001", pgtype.Text{String: "WX-1001", Valid: true}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0)) // state='pending' 不命中
	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnRows(orderRows("order-1", "u1", "vip-month-0001", "OT-1001", "closed", 100))
	mock.ExpectExec("UPDATE orders SET").
		WithArgs("OT-1001", pgtype.Text{String: "WX-1001", Valid: true}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1)) // closed -> paid 补记
	mock.ExpectCommit()

	v := &mockVIPService{}
	h := newNotifyHandler(mock, v)
	err = h.handleNotify(context.Background(), notifyPayload("OT-1001", "WX-1001", 100))
	if err != nil {
		t.Fatalf("closed order paid should be reissued, got: %v", err)
	}
	if v.activateCalls != 1 || v.activateUID != "u1" {
		t.Fatalf("ActivateVIPWithTx calls = %d (uid=%q), want 1 (u1)", v.activateCalls, v.activateUID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestHandleNotify_OrderNotFound 订单不存在（pgx.ErrNoRows）→ errNotifyRejected。
func TestHandleNotify_OrderNotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-404").
		WillReturnError(pgx.ErrNoRows)

	h := newNotifyHandler(mock, &mockVIPService{})
	err = h.handleNotify(context.Background(), notifyPayload("OT-404", "WX-1001", 100))
	if !errors.Is(err, errNotifyRejected) {
		t.Fatalf("want errNotifyRejected for missing order, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestHandleNotify_QueryDBError 查询订单遇到真实 DB 错误 → 返回可重试错误（非 errNotifyRejected）。
func TestHandleNotify_QueryDBError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("FROM orders WHERE out_trade_no = \\$1").
		WithArgs("OT-1001").
		WillReturnError(errors.New("db connection lost"))

	h := newNotifyHandler(mock, &mockVIPService{})
	err = h.handleNotify(context.Background(), notifyPayload("OT-1001", "WX-1001", 100))
	if err == nil {
		t.Fatal("expected error for real db failure")
	}
	if errors.Is(err, errNotifyRejected) {
		t.Fatalf("real db error must NOT be errNotifyRejected (retriable), got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestValidReceiveID VP-P2-03：receive_id 校验（未配置时跳过）。
func TestValidReceiveID(t *testing.T) {
	cases := []struct {
		expected string
		got      string
		want     bool
	}{
		{"wxappid", "wxappid", true},
		{"wxappid", "otherapp", false},
		{"", "anything", true},
		{"  ", "anything", true},
		{" wxappid ", "wxappid", true},
	}
	for _, c := range cases {
		if got := validReceiveID(c.expected, c.got); got != c.want {
			t.Fatalf("validReceiveID(%q, %q) = %v, want %v", c.expected, c.got, got, c.want)
		}
	}
}

// TestHandleNotify_SubscribeEventSilentAck 订阅消息系列事件（数据回调 URL 与发货事件共用）：
// 固定成功签收、不进入发货解析、不产生 payment_notify_business_rejected 资金告警。
func TestHandleNotify_SubscribeEventSilentAck(t *testing.T) {
	for _, evt := range []string{"subscribe_msg_popup_event", "subscribe_msg_change_event", "subscribe_msg_sent_event"} {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("new mock pool: %v", err)
		}
		payload := map[string]interface{}{
			"event":                  evt,
			"openid":                 "oENlX3TF45l_x0zqiO69Lj_9z_4A",
			"SubscribeMsgPopupEvent": map[string]interface{}{},
		}
		h := newNotifyHandler(mock, &mockVIPService{})
		if err := h.handleNotify(context.Background(), payload); err != nil {
			t.Fatalf("%s: want silent ack (nil), got %v", evt, err)
		}
		// 静默签收路径不得触达数据库。
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("%s: unexpected DB calls: %v", evt, err)
		}
		mock.Close()
	}
}

// TestHandleNotify_UnsupportedEvent 未知事件（含带完整发货字段者）仍业务拒绝，
// 保留 payment_notify_business_rejected 告警语义。
func TestHandleNotify_UnsupportedEvent(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	payload := notifyPayload("OT-1001", "WX-1001", 100)
	payload["event"] = "xpay_some_future_event"
	h := newNotifyHandler(mock, &mockVIPService{})
	err = h.handleNotify(context.Background(), payload)
	if !errors.Is(err, errNotifyRejected) {
		t.Fatalf("want errNotifyRejected for unsupported event, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// errReader 模拟网络层读失败（连接截断）的请求 body。
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("simulated connection reset") }

// TestNotify_ReadBodyFailureRetries（FR-3 / VP-9-AC2b）：读失败必须返回 500 + ErrCode:-1
// 触发微信补发——固定成功会让微信停止重试，已扣款订单静默滞留 pending。
func TestNotify_ReadBodyFailureRetries(t *testing.T) {
	h := newNotifyHandler(nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/prod/payment/virtualPayNotify", errReader{})
	h.Notify(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("read body failure: want 500, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ErrCode":-1`) {
		t.Fatalf("read body failure: want ErrCode:-1, got %s", rec.Body.String())
	}
}

// TestNotify_BodyTooLargeFixedSuccess（FR-3 / VP-9-AC2a）：超限属垃圾/攻击输入，
// 终止重试（固定成功 JSON）。
func TestNotify_BodyTooLargeFixedSuccess(t *testing.T) {
	h := newNotifyHandler(nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/prod/payment/virtualPayNotify",
		bytes.NewReader(make([]byte, 64*1024+1)))
	h.Notify(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("body too large: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ErrCode":0`) {
		t.Fatalf("body too large: want fixed success, got %s", rec.Body.String())
	}
}
