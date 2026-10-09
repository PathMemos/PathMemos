package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/vip"
	"papafeiji/backend/pkg/util"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrInvalidVIP = errors.New("invalid vip")
	// errNotifyRejected 表示回调的业务性拒绝（订单不存在/金额不符/事件不支持等）：
	// 重试无法改变结果，应回 200 终止微信重试。
	// 其余错误视为瞬时故障，回非 2xx 触发微信重试。
	errNotifyRejected = errors.New("notify business rejected")
)

type requestResponse struct {
	OutTradeNo  string `json:"outTradeNo,omitempty"`
	PrepayID    string `json:"prepayId,omitempty"`
	OfferID     string `json:"offerId,omitempty"`
	BuyQuantity int32  `json:"buyQuantity,omitempty"`
	NeedPay     bool   `json:"needPay"`
	SignData    string `json:"signData,omitempty"`
	PaySig      string `json:"paySig,omitempty"`
	Signature   string `json:"signature,omitempty"`
	Mode        string `json:"mode,omitempty"`
}

// createOrder 为指定用户创建一笔 VIP 订单。
// 注意：事务内关闭旧 pending 订单后立即新建，极端并发下仍可能产生多笔 pending 订单；
// 这是业务可接受的，由用户/微信最终完成其中一笔，其余由后台任务关闭（AGENTS.md §4.3）。
func (h *Handler) createOrder(ctx context.Context, userID, vipID string, env int32) (*requestResponse, error) {
	vipRecord, err := h.pool.Queries().GetVIPByID(ctx, vipID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidVIP
		}
		slog.ErrorContext(ctx, "get vip by id failed", slog.String("vip_id", vipID), slog.Any("error", err))
		return nil, fmt.Errorf("get vip by id: %w", err)
	}
	if !vipRecord.IsActive || (vipRecord.Type != "month" && vipRecord.Type != "year") {
		return nil, ErrInvalidVIP
	}

	amount, ok := MatchPrice(vipRecord.Prices, vipRecord.Type)
	if !ok || amount <= 0 {
		return nil, ErrInvalidVIP
	}

	if _, _, err := vip.ParseVIPDuration(vipRecord.TimeLimitMark, int(vipRecord.TimeLimitNumber)); err != nil {
		return nil, ErrInvalidVIP
	}

	var outTradeNo string
	var orderID string

	err = db.WithTx(ctx, h.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		if err := q.ClosePendingOrdersByUserAndVIP(ctx, sqlc.ClosePendingOrdersByUserAndVIPParams{
			UserID: pgtype.Text{String: userID, Valid: true},
			VipID:  vipID,
		}); err != nil {
			return fmt.Errorf("close pending orders: %w", err)
		}

		outTradeNo, err = GenerateOutTradeNo()
		if err != nil {
			return fmt.Errorf("generate out trade no: %w", err)
		}
		orderID, err = util.NewUUID()
		if err != nil {
			return fmt.Errorf("generate order id: %w", err)
		}
		_, err = q.CreateOrder(ctx, sqlc.CreateOrderParams{
			ID:         orderID,
			UserID:     pgtype.Text{String: userID, Valid: true},
			VipID:      vipID,
			OutTradeNo: outTradeNo,
			Amount:     amount,
		})
		if err != nil {
			return fmt.Errorf("create order: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	user, err := h.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user openid: %w", err)
	}

	sessionKeyRec, err := h.pool.Queries().GetUserSessionKeyByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user session key: %w", err)
	}
	sessionKey := ""
	if sessionKeyRec.Valid {
		sessionKey = sessionKeyRec.String
	}

	attach, err := json.Marshal(map[string]string{
		"userId": userID,
		"vipId":  vipID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal attach: %w", err)
	}

	wxResp, err := h.wechat.Request(ctx, VirtualPayRequest{
		OutTradeNo:  outTradeNo,
		OpenID:      user.OpenID,
		OfferID:     "",
		ProductID:   utilString(vipRecord.ProductID),
		GoodsPrice:  amount,
		Env:         env,
		BuyQuantity: 1,
		Attach:      string(attach),
	}, sessionKey)
	if err != nil {
		if _, cerr := h.pool.Queries().CloseOrder(ctx, sqlc.CloseOrderParams{
			OutTradeNo: outTradeNo,
			UserID:     pgtype.Text{String: userID, Valid: true},
		}); cerr != nil {
			slog.ErrorContext(ctx, "close order after wechat request failed", slog.String("out_trade_no", outTradeNo), slog.Any("error", cerr))
		}
		return nil, fmt.Errorf("wechat request failed: %w", err)
	}

	return &requestResponse{
		OutTradeNo:  outTradeNo,
		OfferID:     wxResp.OfferID,
		BuyQuantity: 1,
		NeedPay:     true,
		SignData:    wxResp.SignData,
		PaySig:      wxResp.PaySig,
		Signature:   wxResp.Signature,
		Mode:        wxResp.Mode,
	}, nil
}

func parseNotifyAmount(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int64(n), true
	case int64:
		return n, true
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

func (h *Handler) handleNotify(ctx context.Context, payload map[string]interface{}) error {
	// 数据回调 URL 同时承载微信消息推送（订阅消息弹窗/变更/发送结果与发货事件共用同一 URL）。
	// 订阅消息系列事件属正常用户触达，固定成功签收即可——不进入发货解析，
	// 更不打 payment_notify_business_rejected 资金告警（subscribe_msg_popup_event
	// 曾触发误报，见 DEPLOYMENT 告警白名单口径）。
	if evt := eventType(payload); evt != "xpay_goods_deliver_notify" {
		if strings.HasPrefix(evt, "subscribe_msg_") {
			slog.InfoContext(ctx, "payment notify ignored (subscribe event)", slog.String("event", evt))
			return nil
		}
		return fmt.Errorf("%w: unsupported payment event: %s", errNotifyRejected, evt)
	}

	outTradeNo := getStringAny(payload, "OutTradeNo", "out_trade_no")
	transactionID := getNestedStringAny(payload, "WeChatPayInfo", "", "TransactionId", "transaction_id")
	if transactionID == "" {
		transactionID = getStringAny(payload, "transaction_id")
	}
	if outTradeNo == "" {
		return fmt.Errorf("%w: missing out_trade_no", errNotifyRejected)
	}
	if transactionID == "" {
		slog.ErrorContext(ctx, "notify missing transaction_id", slog.String("out_trade_no", outTradeNo))
		return fmt.Errorf("%w: missing transaction_id", errNotifyRejected)
	}

	order, err := h.pool.Queries().GetOrderByOutTradeNo(ctx, outTradeNo)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "notify order not found", slog.String("out_trade_no", outTradeNo))
			return fmt.Errorf("%w: order not found: %v", errNotifyRejected, err)
		}
		return fmt.Errorf("get order by out trade no: %w", err)
	}

	userDeleted := !order.UserID.Valid || order.UserID.String == ""
	if userDeleted {
		slog.WarnContext(ctx, "notify for deleted user, will update order but skip delivery", slog.String("out_trade_no", outTradeNo))
	}

	notifyAmount, amountOK := parseNotifyAmount(getNestedAny(payload, []string{"GoodsInfo", "goodsInfo"}, []string{"ActualPrice", "actualPrice"}))
	if !amountOK {
		notifyAmount, amountOK = parseNotifyAmount(getNestedAny(payload, nil, []string{"ActualPrice", "actualPrice", "amount"}))
	}
	if !amountOK {
		slog.ErrorContext(ctx, "notify amount missing or unparsable",
			slog.String("alert", "payment_notify_amount_missing"),
			slog.String("out_trade_no", outTradeNo),
			slog.Int64("order_amount", int64(order.Amount)))
		return fmt.Errorf("%w: amount missing", errNotifyRejected)
	}
	// 回调已通过验签，金额不可解析才拒绝。金额与订单标价不一致多来自
	// 平台立减/优惠券，此时用户已实际扣款，拒绝会静默漏发；按「支付成功即到账」
	// 照常发货，仅打告警供对账。非正数金额视为异常拒绝。
	if notifyAmount <= 0 {
		slog.ErrorContext(ctx, "notify amount invalid, rejected",
			slog.String("alert", "payment_notify_amount_invalid"),
			slog.String("out_trade_no", outTradeNo),
			slog.Int64("order_amount", int64(order.Amount)),
			slog.Int64("notify_amount", notifyAmount))
		return fmt.Errorf("%w: invalid amount", errNotifyRejected)
	}
	if notifyAmount != int64(order.Amount) {
		slog.ErrorContext(ctx, "payment notify amount mismatch, delivering anyway",
			slog.String("alert", "payment_notify_amount_mismatch"),
			slog.String("out_trade_no", outTradeNo),
			slog.Int64("order_amount", int64(order.Amount)),
			slog.Int64("notify_amount", notifyAmount))
	}

	err = db.WithTx(ctx, h.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		// 在事务内重新确认订单归属，避免账号注销与支付回调的极窄竞态。
		currentOrder, err := q.GetOrderByOutTradeNo(ctx, outTradeNo)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("order not found: %w", err)
			}
			return fmt.Errorf("get order in tx: %w", err)
		}
		isUserDeleted := !currentOrder.UserID.Valid || currentOrder.UserID.String == ""

		rows, err := q.UpdateOrderPaid(ctx, sqlc.UpdateOrderPaidParams{
			OutTradeNo:    outTradeNo,
			TransactionID: pgtype.Text{String: transactionID, Valid: true},
		})
		if err != nil {
			// 仅 transaction_id 唯一冲突（23505，不同 out_trade_no 收到同一 transaction_id）
			// 属微信侧不应发生的异常，重试无法修复，按业务拒绝并依赖监控告警人工兜底。
			// 其余错误（40001 序列化失败、40P01 死锁、连接中断等）为瞬时故障，原样返回以触发微信重试。
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return fmt.Errorf("%w: update order paid: %v", errNotifyRejected, err)
			}
			return fmt.Errorf("update order paid: %w", err)
		}
		if rows == 0 {
			// currentOrder 是事务开头的快照；rows==0 说明订单已非 pending，可能被并发 CloseOrder
			// 在快照之后、本次 UPDATE 之前改为 closed（快照仍 pending）。重新读取最新状态，
			// 避免把"刚被关闭但已支付"误判为幂等成功而静默漏发货。
			freshOrder, ferr := q.GetOrderByOutTradeNo(ctx, outTradeNo)
			if ferr == nil {
				currentOrder = freshOrder
				// 重读后同步刷新用户注销快照：首读与重读之间注销可能已提交，
				// 沿用旧快照会对 userID 为空的订单调 ActivateVIPWithTx 而回滚 500。
				isUserDeleted = !currentOrder.UserID.Valid || currentOrder.UserID.String == ""
			} else if !errors.Is(ferr, pgx.ErrNoRows) {
				return fmt.Errorf("re-read order state: %w", ferr)
			}
			// 订单已非 pending：paid 为已发货的幂等重试，正常返回成功。
			// closed 表示订单被关闭后才完成支付（取消竞态/后台任务关闭超时 pending 后仍支付成功），
			// 微信已扣款，不能漏发：VP-P1-02 将 closed 补记为 paid 并自动发货。
			if currentOrder.State == "closed" && !isUserDeleted {
				rows2, err2 := q.MarkClosedOrderPaid(ctx, sqlc.MarkClosedOrderPaidParams{
					OutTradeNo:    outTradeNo,
					TransactionID: pgtype.Text{String: transactionID, Valid: true},
				})
				if err2 != nil {
					var pgErr *pgconn.PgError
					if errors.As(err2, &pgErr) && pgErr.Code == "23505" {
						return fmt.Errorf("%w: mark closed order paid: %v", errNotifyRejected, err2)
					}
					return fmt.Errorf("mark closed order paid: %w", err2)
				}
				if rows2 == 0 {
					slog.WarnContext(ctx, "closed order concurrently handled, skip",
						slog.String("out_trade_no", outTradeNo))
					return nil
				}
				slog.ErrorContext(ctx, "payment notify for closed order: reissued delivery",
					slog.String("alert", "payment_notify_closed_order_reissued"),
					slog.String("out_trade_no", outTradeNo),
					slog.String("vip_id", currentOrder.VipID),
					slog.String("transaction_id", transactionID))
				if err := h.vipService.ActivateVIPWithTx(ctx, currentOrder.UserID.String, currentOrder.VipID, q); err != nil {
					return fmt.Errorf("activate vip (closed order): %w", err)
				}
				return nil
			}
			slog.WarnContext(ctx, "notify for non-pending order", slog.String("out_trade_no", outTradeNo))
			return nil
		}

		if isUserDeleted {
			slog.InfoContext(ctx, "payment notify order updated for deleted user",
				slog.String("out_trade_no", outTradeNo),
				slog.String("vip_id", currentOrder.VipID),
				slog.String("transaction_id", transactionID))
			return nil
		}

		if err := h.vipService.ActivateVIPWithTx(ctx, currentOrder.UserID.String, currentOrder.VipID, q); err != nil {
			return fmt.Errorf("activate vip: %w", err)
		}

		slog.InfoContext(ctx, "payment notify delivered",
			slog.String("out_trade_no", outTradeNo),
			slog.String("user_id", currentOrder.UserID.String),
			slog.String("vip_id", currentOrder.VipID),
			slog.String("transaction_id", transactionID))
		return nil
	})
	return err
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getStringAny(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func getNestedStringAny(m map[string]interface{}, outer string, outerAlt string, keys ...string) string {
	for _, o := range []string{outer, outerAlt} {
		if o == "" {
			// outerAlt 为空表示直接从顶层读取
			for _, k := range keys {
				if v, ok := m[k].(string); ok && v != "" {
					return v
				}
			}
			continue
		}
		outerMap, ok := m[o].(map[string]interface{})
		if !ok {
			continue
		}
		for _, k := range keys {
			if v, ok := outerMap[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

func getNestedAny(m map[string]interface{}, outerKeys, innerKeys []string) interface{} {
	if len(outerKeys) == 0 {
		// 无外层 key 时直接从顶层读取
		for _, k := range innerKeys {
			if v, ok := m[k]; ok {
				return v
			}
		}
		return nil
	}
	for _, o := range outerKeys {
		outerMap, ok := m[o].(map[string]interface{})
		if !ok {
			continue
		}
		for _, k := range innerKeys {
			if v, ok := outerMap[k]; ok {
				return v
			}
		}
	}
	return nil
}

func utilString(t pgtype.Text) string {
	if t.Valid {
		return t.String
	}
	return ""
}
