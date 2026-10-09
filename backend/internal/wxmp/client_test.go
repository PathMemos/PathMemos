package wxmp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	miniredis "github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

// errRoundTripper 让回源请求立即失败，测试不触网。
type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network in test")
}

// GetAccessToken：Redis 缓存读失败（非 Nil 错误）记 warn 后照常回源（与 getThumbMediaID 同型）。
func TestGetAccessToken_CacheReadErrorLogsWarn(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	rdb.Close() // 客户端已关：缓存读必然报非 Nil 错误
	mr.Close()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	c := &Client{
		appID:      "wx-test-appid",
		secret:     "test-secret",
		httpClient: &http.Client{Transport: errRoundTripper{}},
		rdb:        rdb,
	}
	if _, err := c.GetAccessToken(context.Background()); err == nil {
		t.Fatalf("expected fallback fetch error, got nil")
	}
	if !strings.Contains(buf.String(), "read wechat mp access token cache failed") {
		t.Fatalf("expected cache-read warn log, got: %s", buf.String())
	}
}
