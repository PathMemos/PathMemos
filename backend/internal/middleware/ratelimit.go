package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"papafeiji/backend/pkg/errors"
)

type rateBucket struct {
	requests []time.Time
}

type slidingWindowLimiter struct {
	limit      int
	window     time.Duration
	maxBuckets int
	buckets    map[string]*rateBucket
	mu         sync.Mutex
}

func newSlidingWindowLimiter(limit int, window time.Duration, maxBuckets int) *slidingWindowLimiter {
	return &slidingWindowLimiter{
		limit:      limit,
		window:     window,
		maxBuckets: maxBuckets,
		buckets:    make(map[string]*rateBucket),
	}
}

func (l *slidingWindowLimiter) allow(key string) bool {
	if l.limit <= 0 {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)

	b, ok := l.buckets[key]
	if !ok {
		b = &rateBucket{}
		l.buckets[key] = b
	}

	// 滑动窗口：只保留当前窗口内的请求时间戳。
	requests := b.requests[:0]
	for _, t := range b.requests {
		if t.After(cutoff) {
			requests = append(requests, t)
		}
	}
	b.requests = requests

	if len(b.requests) >= l.limit {
		return false
	}
	b.requests = append(b.requests, now)

	// 惰性清理过期桶：仅在桶数超限 20% 时触发（而非每请求满容量时全表扫描），
	// 每轮最多删除 maxEvictPerPass 个，避免攻击场景下 O(N) 扫描拖垮所有限流检查。
	if len(l.buckets) >= l.maxBuckets*12/10 {
		l.evictBuckets(cutoff)
	}

	return true
}

func (l *slidingWindowLimiter) evictBuckets(cutoff time.Time) {
	// 单次遍历完成淘汰——删除全部过期/空桶并顺带记录最久未活跃桶；
	// 仍超限时仅删该一个最旧桶。单次遍历完成淘汰，最坏 O(N)。
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, b := range l.buckets {
		if len(b.requests) == 0 || b.requests[len(b.requests)-1].Before(cutoff) {
			delete(l.buckets, k)
			continue
		}
		if first || b.requests[0].Before(oldestTime) {
			oldestKey = k
			oldestTime = b.requests[0]
			first = false
		}
	}
	if len(l.buckets) > l.maxBuckets && !first {
		delete(l.buckets, oldestKey)
	}
}

const defaultMaxBuckets = 10000

type IPRateLimiter struct {
	sw             *slidingWindowLimiter
	trustedProxies []net.IPNet
}

func NewIPRateLimiter(limit int, window time.Duration, trustedProxies ...string) *IPRateLimiter {
	parsed, invalid := parseTrustedProxies(trustedProxies)
	if len(invalid) > 0 {
		// 非法 CIDR 显式告警，避免配置错误时静默退化为直接信任 RemoteAddr。
		slog.Warn("ip rate limiter: invalid trusted proxy cidr ignored", slog.Any("invalid", invalid))
	}

	return &IPRateLimiter{
		sw:             newSlidingWindowLimiter(limit, window, defaultMaxBuckets),
		trustedProxies: parsed,
	}
}

func parseTrustedProxies(inputs []string) (valid []net.IPNet, invalid []string) {
	for _, input := range inputs {
		for _, cidr := range strings.Split(input, ",") {
			cidr = strings.TrimSpace(cidr)
			if cidr == "" {
				continue
			}
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				invalid = append(invalid, cidr)
				continue
			}
			valid = append(valid, *ipNet)
		}
	}
	return valid, invalid
}

// Stop 保留以兼容旧接口；当前实现已无后台 goroutine。
func (l *IPRateLimiter) Stop() {}

func (l *IPRateLimiter) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, l.trustedProxies)
		if !l.sw.allow(ip) {
			JSONBizError(w, r, errors.BizRateLimited, "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP 根据 RemoteAddr 与可信代理 CIDR 解析真实客户端 IP。
func ClientIP(r *http.Request, trustedProxies ...string) string {
	parsed, _ := parseTrustedProxies(trustedProxies)
	return clientIP(r, parsed)
}

func clientIP(r *http.Request, trusted []net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remoteIP := net.ParseIP(host)

	// 未配置可信代理时，直接信任 RemoteAddr，不解析 X-Forwarded-For 等头部，
	// 防止客户端伪造 IP 绕过限流。
	if remoteIP == nil || !isTrustedProxy(remoteIP, trusted) {
		return host
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := strings.TrimSpace(parts[i])
			if ip == "" {
				continue
			}
			parsed := net.ParseIP(ip)
			if parsed == nil || isPrivateIP(parsed) {
				continue
			}
			return ip
		}
	}

	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		ip := strings.TrimSpace(xri)
		if parsed := net.ParseIP(ip); parsed != nil && !isPrivateIP(parsed) {
			return ip
		}
	}

	return host
}

func isTrustedProxy(ip net.IP, trusted []net.IPNet) bool {
	for _, n := range trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

var privateIPv4Networks = func() []*net.IPNet {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		nets = append(nets, ipNet)
	}
	return nets
}()

var privateIPv6Networks = func() []*net.IPNet {
	//nolint:errcheck
	_, fc00, _ := net.ParseCIDR("fc00::/7")
	if fc00 == nil {
		return nil
	}
	return []*net.IPNet{fc00}
}()

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}

	for _, ipNet := range privateIPv4Networks {
		if ipNet.Contains(ip) {
			return true
		}
	}

	if ip.To4() == nil {
		for _, ipNet := range privateIPv6Networks {
			if ipNet.Contains(ip) {
				return true
			}
		}
	}

	return false
}
