// Package middleware provides authentication helpers.
package middleware

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/pkg/errors"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// OpenAuthMiddleware authenticates requests for the open-source backend.
// 两条互斥通路：
//   - 携带 Authorization Bearer 时只做 session 鉴权（直连浏览器/小程序），
//     session 不存在直接 401，由前端走重新登录流程，不再回落 API Key；
//   - 完全无 Authorization 头时才校验 X-Private-Api-Key
//     （Cloudflare Worker 转发的 MCP/curl 请求）。
type OpenAuthMiddleware struct {
	sessions        *SessionManager
	pool            *db.Pool
	expectedKeyHash string
	// B6a-09：连续失败计数——达到阈值才延时，避免每次失败都占用请求 goroutine 500ms。
	failCount atomic.Int32
}

func NewOpenAuthMiddleware(sessions *SessionManager, pool *db.Pool, openAPIKey string) *OpenAuthMiddleware {
	h := sha256.Sum256([]byte(openAPIKey))
	return &OpenAuthMiddleware{
		sessions:        sessions,
		pool:            pool,
		expectedKeyHash: hex.EncodeToString(h[:]),
	}
}

func (m *OpenAuthMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionID := extractBearerToken(r)
		if sessionID != "" {
			// session 不存在直接 401（前端走 401→重新登录流程），不再回落 API Key，
			// 避免过期 session + 有效 API Key 的请求静默降级为默认用户身份。
			userID, err := m.sessions.Get(r.Context(), sessionID)
			if err != nil {
				if stderrors.Is(err, redis.Nil) {
					JSONBizError(w, r, errors.BizSessionInvalid, "invalid or expired session")
					return
				}

				JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to check session")
				return
			}

			r = r.WithContext(WithSessionID(WithUserID(r.Context(), userID), sessionID))
			next.ServeHTTP(w, r)
			return
		}

		// 仅在完全无 Authorization 头时尝试 API Key 鉴权（MCP/curl 通路）。
		if r.Header.Get("Authorization") == "" {
			apiKey := r.Header.Get("X-Private-Api-Key")
			if apiKey != "" {
				userID, err := m.lookupUserByAPIKey(r.Context(), apiKey)
				if err == nil && userID != "" {
					m.failCount.Store(0)
					r = r.WithContext(WithSessionID(WithUserID(r.Context(), userID), "apikey:"+userID))
					next.ServeHTTP(w, r)
					return
				}
				// DB 基础设施错误与「键不合法」同走 401，但需可区分：仅对非 ErrNoRows 的查询错误记日志，
				// 避免开源版 DB 故障期间全部表现为无效密钥、无从排查。
				if err != nil && !stderrors.Is(err, pgx.ErrNoRows) {
					slog.Warn("open api key lookup failed", slog.Any("error", err))
				}
				// B6a-09：连续失败达到阈值才延时，压低暴力穷举速率且不拖慢偶发错误请求。
				if m.failCount.Add(1) >= 3 {
					time.Sleep(500 * time.Millisecond)
					m.failCount.Store(0)
				}
			}
		}

		JSONBizError(w, r, errors.BizSessionInvalid, "unauthorized")
	})
}

func (m *OpenAuthMiddleware) lookupUserByAPIKey(ctx context.Context, rawKey string) (string, error) {
	// 仅接受开源版种子 OPEN_API_KEY：用户通过 /mcp/key 创建的 MCP Key 只能用于 MCP 端点，
	// 不能升级为完整 REST 凭证。
	h := sha256.Sum256([]byte(rawKey))
	hashHex := hex.EncodeToString(h[:])
	if subtle.ConstantTimeCompare([]byte(hashHex), []byte(m.expectedKeyHash)) != 1 {
		return "", fmt.Errorf("api key not allowed")
	}
	key, err := m.pool.Queries().GetAPIKeyByHash(ctx, hashHex)
	if err != nil {
		return "", err
	}
	if key.ApiKey == "" {
		return "", fmt.Errorf("empty api key")
	}
	return key.UserID, nil
}
