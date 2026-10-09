package util

import (
	"context"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// dnsLookupTimeout 单次 DNS 解析超时。
	dnsLookupTimeout = 1500 * time.Millisecond
	// dnsCacheTTL 成功解析结果的缓存时间。
	dnsCacheTTL = 60 * time.Second
	// dnsFailureCacheTTL 解析失败（超时/出错）的缓存时间，避免热路径被反复阻塞。
	dnsFailureCacheTTL = 5 * time.Second
	// dnsCacheMaxEntries 进程内 DNS 缓存容量上限，防止用户可控 hostname 驱动无界内存增长。
	dnsCacheMaxEntries = 10000
)

// dnsCacheEntry 一次域名解析的缓存结果。ok=false 表示解析失败（短期缓存）。
type dnsCacheEntry struct {
	ips       []net.IP
	ok        bool
	expiresAt time.Time
}

// dnsCache 进程内 DNS 结果缓存：IsPublicHTTPSURL 处于保存头像/图片等热路径，
// 每次同步解析最多阻塞 1.5s；缓存后命中路径零延迟。多实例各自缓存，语义正确。
var dnsCache = struct {
	sync.Mutex
	m map[string]dnsCacheEntry
}{m: make(map[string]dnsCacheEntry)}

func cachedLookupIP(ctx context.Context, host string) ([]net.IP, bool) {
	dnsCache.Lock()
	e, ok := dnsCache.m[host]
	if ok && time.Now().Before(e.expiresAt) {
		dnsCache.Unlock()
		if !e.ok {
			return nil, false
		}
		return e.ips, true
	}
	dnsCache.Unlock()

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	ttl := dnsCacheTTL
	entry := dnsCacheEntry{ips: ips, ok: err == nil, expiresAt: time.Now().Add(ttl)}
	if err != nil {
		ttl = dnsFailureCacheTTL
		entry.expiresAt = time.Now().Add(ttl)
	}

	dnsCache.Lock()
	// 仅缓存新结果，避免覆盖并发请求刚写入的更新条目。
	if cur, exists := dnsCache.m[host]; !exists || time.Now().After(cur.expiresAt) {
		// 容量保护：超过阈值先清扫已过期条目；清扫后仍超限则放弃缓存本条目（不驱逐未过期条目），
		// 避免用户可控 hostname 使 map 无界增长。
		if len(dnsCache.m) >= dnsCacheMaxEntries {
			now := time.Now()
			for k, v := range dnsCache.m {
				if now.After(v.expiresAt) {
					delete(dnsCache.m, k)
				}
			}
		}
		if len(dnsCache.m) < dnsCacheMaxEntries {
			dnsCache.m[host] = entry
		}
	}
	dnsCache.Unlock()
	return entry.ips, entry.ok
}

func IsPublicHTTPSURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") {
		return false
	}
	if u.Port() != "" && u.Port() != "443" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !isForbiddenIP(ip)
	}

	// 域名形式需解析 DNS 并校验所有解析结果均非内网/回环，缓解 SSRF。
	// 解析结果进程内缓存（成功 60s / 失败 5s），热路径不被 DNS 抖动拖累。
	// 保存时校验；DNS 重绑定窗口由抓取侧超时与响应大小限制兜底。
	ctx, cancel := context.WithTimeout(context.Background(), dnsLookupTimeout)
	defer cancel()
	ips, ok := cachedLookupIP(ctx, host)
	if !ok {
		return false
	}
	for _, ip := range ips {
		if isForbiddenIP(ip) {
			return false
		}
	}
	return true
}

// isForbiddenIP 判断 IP 是否属于禁止访问的保留/内网网段（M5：补齐 CGNAT 与文档网段）。
func isForbiddenIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// CGNAT 100.64.0.0/10、文档网段 192.0.2.0/24 198.51.100.0/24 203.0.113.0/24、benchmark 198.18.0.0/15。
	for _, cidr := range []string{
		"100.64.0.0/10",
		"192.0.2.0/24",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"198.18.0.0/15",
	} {
		if _, n, err := net.ParseCIDR(cidr); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
