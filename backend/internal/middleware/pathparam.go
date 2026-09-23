package middleware

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

// URLParam 返回已解码的 chi 路径参数。
//
// chi 在 r.URL.RawPath 非空时按 RawPath 匹配路由（chi v5 mux.go），此时
// chi.URLParam 返回的是原始路径片段，可能仍带百分号编码。触发该分支的典型场景是
// 前端 encodeURIComponent 不转义、而 Go 的默认转义会改写的字符：! * ' ( )。
// 例如 encodeURIComponent("源头日记(诺德财富中心A座店)") 会把中文编码、把括号保留为
// 字面量，形成「混编」路径：
//
//	/user/common-addresses/%E6%BA%90...(%E8%AF%BA...)
//
// Go 因该混编设置 RawPath != ""，chi 于是返回仍带编码的参数，按名字查库会 404。
//
// 反向约束：RawPath == "" 时 chi.URLParam 已经是解码值（如 "100%棉" 会得到
// "100%棉"），此时不能再做 PathUnescape，否则 "%20" 之类会被二次解码。
func URLParam(r *http.Request, key string) string {
	value := chi.URLParam(r, key)
	if r.URL.RawPath != "" {
		if decoded, err := url.PathUnescape(value); err == nil {
			return decoded
		}
	}
	return value
}
