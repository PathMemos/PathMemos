// Package payment provides related functionality.
package payment

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
)

const outTradeNoAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// GenerateOutTradeNo 生成 32 位随机订单号。不携带 userID/vipID 语义（曾以弃用参数
// 暗示「仅日志用途」，实为误导性签名，已清理）；归属关系由 orders 行自身的
// user_id/vip_id 列承载。
func GenerateOutTradeNo() (string, error) {
	// 随机不可预测性保留纵深防御价值（回声/撞单探测面），
	// 当前防伪主边界为安全模式验签解密 + DB 幂等（02e D2/D3）。
	return randomString(32)
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random read: %w", err)
	}
	for i := range b {
		// %62 取字符存在轻微模偏差（256%62=8，前 8 个字母概率高约 3%）：对 32 位
		// 随机串的不可预测性/唯一性（~190 bit 熵 + out_trade_no 唯一约束兜底）影响
		// 可忽略，为有意取舍（02e D4），不引入拒绝采样复杂度。
		b[i] = outTradeNoAlphabet[int(b[i])%len(outTradeNoAlphabet)]
	}
	return string(b), nil
}

func MatchPrice(prices []byte, vipType string) (amount int32, ok bool) {
	if len(prices) == 0 {
		return 0, false
	}
	var list []struct {
		Type           string `json:"type"`
		Amount         int32  `json:"amount"`
		Duration       int32  `json:"duration"`
		Unit           string `json:"unit"`
		OriginalAmount *int32 `json:"originalAmount,omitempty"`
	}
	if err := json.Unmarshal(prices, &list); err != nil {
		return 0, false
	}
	for _, p := range list {
		if strings.EqualFold(p.Type, vipType) && p.Amount > 0 {
			return p.Amount, true
		}
	}
	return 0, false
}
