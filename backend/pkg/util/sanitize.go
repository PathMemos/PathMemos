package util

import (
	"fmt"
	"regexp"
)

// urlCredentialParamRe 匹配 URL 查询串中可能携带凭据的参数。
var urlCredentialParamRe = regexp.MustCompile(`([?&](?:key|access_token|secret|appid|js_code)=)[^&\s]+`)

// SanitizeURLError 将错误信息中 URL 查询参数的值脱敏（key=***），
// 防止腾讯地图 key / 微信 access_token 等凭据随错误日志泄露。
func SanitizeURLError(err error) error {
	if err == nil {
		return nil
	}
	msg := urlCredentialParamRe.ReplaceAllString(err.Error(), "${1}***")
	return fmt.Errorf("%s", msg)
}

// SanitizeRequestID 规范化客户端传入的 X-Request-ID：限制长度并剔除控制字符。
func SanitizeRequestID(rid string) string {
	if rid == "" {
		return ""
	}
	runes := []rune(rid)
	if len(runes) > 64 {
		runes = runes[:64]
	}
	out := make([]rune, 0, len(runes))
	for _, r := range runes {
		if r < 0x20 || r == 0x7f {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
