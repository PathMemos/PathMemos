package middleware

import (
	"net/http"
	"time"

	"papafeiji/backend/pkg/errors"
)

// KeyRateLimiter 基于自定义 key 的滑动窗口限流器。
type KeyRateLimiter struct {
	sw      *slidingWindowLimiter
	keyFunc func(r *http.Request) string
}

func NewKeyRateLimiter(limit int, window time.Duration, maxBuckets int, keyFunc func(r *http.Request) string) *KeyRateLimiter {
	if maxBuckets <= 0 {
		maxBuckets = defaultMaxBuckets
	}
	return &KeyRateLimiter{
		sw:      newSlidingWindowLimiter(limit, window, maxBuckets),
		keyFunc: keyFunc,
	}
}

// Stop 保留以兼容旧接口；当前实现已无后台 goroutine。
func (l *KeyRateLimiter) Stop() {}

func (l *KeyRateLimiter) allow(r *http.Request) bool {
	if l.keyFunc == nil {
		return true
	}
	key := l.keyFunc(r)
	if key == "" {
		// 空 key 直接拒绝（防御性）。当前调用方均保证非空 key。
		return false
	}
	return l.sw.allow(key)
}

// Allow 暴露给需要在认证后显式检查限流的 handler 使用。
func (l *KeyRateLimiter) Allow(r *http.Request) bool {
	return l.allow(r)
}

func (l *KeyRateLimiter) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(r) {
			JSONBizError(w, r, errors.BizRateLimited, "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}
