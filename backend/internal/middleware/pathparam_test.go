package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestURLParam 覆盖 chi 按 RawPath 路由时路径参数仍带编码的回归场景。
// 前端 encodeURIComponent 不转义 ! * ' ( )，而 Go 的默认转义会改写它们，
// 于是含括号（如商家名）的地址名会形成 RawPath != "" 的混编路径。
func TestURLParam(t *testing.T) {
	cases := []struct {
		name string
		path string // 线上原始请求路径
		want string
	}{
		{
			name: "混编编码_中文被编码_括号为字面量",
			path: "/user/common-addresses/%E6%BA%90%E5%A4%B4%E6%97%A5%E8%AE%B0(%E8%AF%BA%E5%BE%B7%E8%B4%A2%E5%AF%8C%E4%B8%AD%E5%BF%83A%E5%BA%A7%E5%BA%97)",
			want: "源头日记(诺德财富中心A座店)",
		},
		{
			name: "混编编码_感叹号与星号为字面量",
			path: "/user/common-addresses/a!b*c",
			want: "a!b*c",
		},
		{
			name: "全编码_RawPath 为空_不应二次解码",
			path: "/user/common-addresses/%E4%B8%8A%E5%AE%9E%C2%B7%E6%B5%B7%E4%B8%8A%E6%B5%B7",
			want: "上实·海上海",
		},
		{
			name: "字面百分号_不应二次解码",
			path: "/user/common-addresses/100%25%E6%A3%89",
			want: "100%棉",
		},
		{
			name: "编码斜杠_解码为斜杠",
			path: "/user/common-addresses/a%2Fb(%E5%BA%97)",
			want: "a/b(店)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "http://example.com"+tc.path, nil)
			router := chi.NewRouter()
			var got, chiRaw string
			router.Put("/user/common-addresses/{name}", func(w http.ResponseWriter, r *http.Request) {
				chiRaw = chi.URLParam(r, "name")
				got = URLParam(r, "name")
			})
			router.ServeHTTP(httptest.NewRecorder(), req)

			if got != tc.want {
				t.Fatalf("URLParam = %q, want %q (chi raw = %q, RawPath = %q)",
					got, tc.want, chiRaw, req.URL.RawPath)
			}
		})
	}
}
