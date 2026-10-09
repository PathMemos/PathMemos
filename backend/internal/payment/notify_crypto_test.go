package payment

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

var (
	testKey32          = []byte("0123456789abcdef0123456789abcdef")
	testEncodingAESKey = base64.StdEncoding.EncodeToString(testKey32)[:43] // 43 字符（无填充）
	testCallbackToken  = "token-test"
)

// encryptNotify 按微信虚拟支付安全模式（AES-256-CBC、IV 全零、PKCS7 按 32 字节对齐）
// 构造密文：random(16) + msgLen(4 大端) + msg + receiveID。
func encryptNotify(t *testing.T, msg, receiveID string) string {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(testEncodingAESKey + "=")
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	buf := make([]byte, 0, 16+4+len(msg)+len(receiveID)+32)
	randBytes := make([]byte, 16)
	for i := range randBytes {
		randBytes[i] = byte(i)
	}
	buf = append(buf, randBytes...)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(msg)))
	buf = append(buf, length[:]...)
	buf = append(buf, []byte(msg)...)
	buf = append(buf, []byte(receiveID)...)

	pad := 32 - len(buf)%32
	if pad == 0 {
		pad = 32
	}
	for i := 0; i < pad; i++ {
		buf = append(buf, byte(pad))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	out := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(out, buf)
	return base64.StdEncoding.EncodeToString(out)
}

func signEncrypted(token, timestamp, nonce, encrypt string) string {
	arr := []string{token, timestamp, nonce, encrypt}
	sort.Strings(arr)
	sum := sha1.Sum([]byte(strings.Join(arr, "")))
	return hex.EncodeToString(sum[:])
}

func notifyRequest(t *testing.T, encrypt, msgSignature string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]string{"encrypt": encrypt})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	url := "/api/prod/payment/virtualPayNotify?timestamp=1700000000&nonce=nonce1"
	if msgSignature != "" {
		url += "&msg_signature=" + msgSignature
	}
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func newCryptoNotifyHandler(mock pgxmock.PgxPoolIface, v *mockVIPService) *Handler {
	return &Handler{
		pool:       db.NewPoolWithDBTX(mock),
		cfg:        &config.Config{WechatVirtualCallbackToken: testCallbackToken, WechatVirtualCallbackAESKey: testEncodingAESKey, WechatAppID: "wxappid"},
		vipService: v,
	}
}

func deliverPlaintext(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{
		"OutTradeNo": "OT-9",
		"event":      "xpay_goods_deliver_notify",
		"WeChatPayInfo": map[string]interface{}{
			"TransactionId": "WX-9",
		},
		"GoodsInfo": map[string]interface{}{"ActualPrice": 100},
	})
	if err != nil {
		t.Fatalf("marshal plaintext: %v", err)
	}
	return string(b)
}

// TestNotify_DecryptsAndDelivers VP-9-AC5/6：合法加密回调 → 解密 → 发货。
func TestNotify_DecryptsAndDelivers(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()

	encrypt := encryptNotify(t, deliverPlaintext(t), "wxappid")

	mock.ExpectQuery("FROM orders WHERE out_trade_no").WithArgs("OT-9").
		WillReturnRows(orderRows("order-9", "u1", "vip-month-0001", "OT-9", "pending", 100))
	mock.ExpectBegin()
	mock.ExpectQuery("FROM orders WHERE out_trade_no").WithArgs("OT-9").
		WillReturnRows(orderRows("order-9", "u1", "vip-month-0001", "OT-9", "pending", 100))
	mock.ExpectExec("UPDATE orders SET").
		WithArgs("OT-9", pgtype.Text{String: "WX-9", Valid: true}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	v := &mockVIPService{}
	h := newCryptoNotifyHandler(mock, v)
	sig := signEncrypted(testCallbackToken, "1700000000", "nonce1", encrypt)
	rec := httptest.NewRecorder()
	h.Notify(rec, notifyRequest(t, encrypt, sig))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if v.activateCalls != 1 || v.activateUID != "u1" {
		t.Fatalf("activate calls = %d uid=%q, want 1/u1", v.activateCalls, v.activateUID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestNotify_BadEncryptedSignature 验签失败 → 不发货（固定 200 终止重试）。
func TestNotify_BadEncryptedSignature(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()

	encrypt := encryptNotify(t, deliverPlaintext(t), "wxappid")
	v := &mockVIPService{}
	h := newCryptoNotifyHandler(mock, v)
	rec := httptest.NewRecorder()
	h.Notify(rec, notifyRequest(t, encrypt, "deadbeef"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if v.activateCalls != 0 {
		t.Fatalf("activate calls = %d, want 0 on bad signature", v.activateCalls)
	}
}

// TestNotify_DecryptFailure 密文损坏（签名有效）→ 不发货。
func TestNotify_DecryptFailure(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()

	badCipher := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, 48))
	sig := signEncrypted(testCallbackToken, "1700000000", "nonce1", badCipher)
	v := &mockVIPService{}
	h := newCryptoNotifyHandler(mock, v)
	rec := httptest.NewRecorder()
	h.Notify(rec, notifyRequest(t, badCipher, sig))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if v.activateCalls != 0 {
		t.Fatalf("activate calls = %d, want 0 on decrypt failure", v.activateCalls)
	}
}

// TestNotify_ReceiveIDMismatch 解密成功但 receive_id 非本小程序 → 500 触发重试。
func TestNotify_ReceiveIDMismatch(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	defer mock.Close()

	encrypt := encryptNotify(t, deliverPlaintext(t), "otherapp")
	sig := signEncrypted(testCallbackToken, "1700000000", "nonce1", encrypt)
	v := &mockVIPService{}
	h := newCryptoNotifyHandler(mock, v)
	rec := httptest.NewRecorder()
	h.Notify(rec, notifyRequest(t, encrypt, sig))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on receive_id mismatch", rec.Code)
	}
	if v.activateCalls != 0 {
		t.Fatalf("activate calls = %d, want 0", v.activateCalls)
	}
}
