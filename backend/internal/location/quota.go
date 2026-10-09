package location

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"papafeiji/backend/pkg/timeutil"

	"github.com/redis/go-redis/v9"
)

const (
	// MaxReversePerUserPerDay 每用户每日逆地理编码配额（供 diary CreateAutoEntry 复用）。
	MaxReversePerUserPerDay = 200
	reverseQuotaKeyPrefix   = "location:reverse"
	reverseQuotaTTL         = 24 * time.Hour
)

// checkReverseQuotaScript atomically increments the per-user daily counter and
// sets its TTL on the first increment. Returns 1 if the request is within quota.
var checkReverseQuotaScript = `
local key = KEYS[1]
local ttl = tonumber(ARGV[1])
local maxCount = tonumber(ARGV[2])
local cur = redis.call('incr', key)
if cur == 1 then
    redis.call('expire', key, ttl)
end
return cur <= maxCount and 1 or 0
`

// CheckReverseQuota 按用户日配额原子计数。Redis 不可用或超限时返回 false（fail-closed，
// 与 location 主链路一致，保证外部地理 API 配额不被耗尽）。
func CheckReverseQuota(ctx context.Context, rdb *redis.Client, userID string) bool {
	if rdb == nil {
		slog.ErrorContext(ctx, "reverse geocode quota check failed: redis not available")
		return false
	}
	key := fmt.Sprintf("%s:%s:%s", reverseQuotaKeyPrefix, userID, timeutil.NowShanghai().Format("2006-01-02"))
	allowed, err := rdb.Eval(ctx, checkReverseQuotaScript,
		[]string{key},
		int64(reverseQuotaTTL.Seconds()),
		MaxReversePerUserPerDay,
	).Int()
	if err != nil {
		slog.ErrorContext(ctx, "reverse geocode quota check failed", slog.Any("error", err))
		return false
	}
	return allowed == 1
}
