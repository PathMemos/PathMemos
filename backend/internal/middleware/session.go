// Package middleware provides related functionality.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"fmt"

	"net/http"
	"strings"
	"time"

	"papafeiji/backend/pkg/errors"

	"github.com/redis/go-redis/v9"
)

type ctxKeyUserID struct{}
type ctxKeySessionID struct{}

const (
	sessionKeyPrefix         = "session:"
	sessionAbsoluteKeyPrefix = "session:abs:"
	sessionsUserKeyPrefix    = "sessions:user:"
	sessionExpiry            = 30 * 24 * time.Hour
	sessionAbsoluteExpiry    = 90 * 24 * time.Hour
)

func UserID(ctx context.Context) string {
	if uid, ok := ctx.Value(ctxKeyUserID{}).(string); ok {
		return uid
	}
	return ""
}

func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKeyUserID{}, userID)
}

func SessionID(ctx context.Context) string {
	if sid, ok := ctx.Value(ctxKeySessionID{}).(string); ok {
		return sid
	}
	return ""
}

func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, ctxKeySessionID{}, sessionID)
}

// SessionManager manages server-side sessions stored in Redis.
type SessionManager struct {
	rdb *redis.Client
}

func NewSessionManager(rdb *redis.Client) *SessionManager {
	if rdb == nil {
		panic("session manager requires a non-nil redis client")
	}
	return &SessionManager{rdb: rdb}
}

func (s *SessionManager) newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// createSessionScript atomically stores the session key, adds the session ID to
// the user's session index set, sets an absolute expiry key, and expires all keys.
// KEYS[1] = session:{session_id}, KEYS[2] = session:abs:{session_id}, KEYS[3] = sessions:user:{user_id}
// ARGV[1] = user_id, ARGV[2] = session_id, ARGV[3] = sliding_expiry_seconds, ARGV[4] = absolute_expiry_seconds
var createSessionScript = `
local sessionKey = KEYS[1]
local sessionAbsKey = KEYS[2]
local userSessionsKey = KEYS[3]
local userID = ARGV[1]
local sessionID = ARGV[2]
local expiry = tonumber(ARGV[3])
local absExpiry = tonumber(ARGV[4])
if not expiry or expiry <= 0 or not absExpiry or absExpiry <= 0 then
    return redis.error_reply('ERR invalid expiry')
end
-- 登录时清理索引中已过期的 session（每次最多 50 个，避免长时间阻塞）。
-- 同时按迭代数 i>50 截断：否则集合前 50 个成员都有效时会 smembers 全量物化 + 逐成员 exists，
-- 阻塞 Redis 单线程、拖慢登录核心路径。
local members = redis.call('smembers', userSessionsKey)
local cleaned = 0
for i, id in ipairs(members) do
    if i > 50 or cleaned >= 50 then break end
    if redis.call('exists', 'session:' .. id) == 0 then
        redis.call('srem', userSessionsKey, id)
        cleaned = cleaned + 1
    end
end
redis.call('set', sessionKey, userID, 'ex', expiry)
redis.call('set', sessionAbsKey, '1', 'ex', absExpiry)
redis.call('sadd', userSessionsKey, sessionID)
redis.call('expire', userSessionsKey, expiry)
return 1
`

// Create creates a new session for the user and returns the session ID.
func (s *SessionManager) Create(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", fmt.Errorf("user id is required")
	}
	sessionID, err := s.newSessionID()
	if err != nil {
		return "", err
	}

	sessionKey := sessionKeyPrefix + sessionID
	sessionAbsKey := sessionAbsoluteKeyPrefix + sessionID
	userSessionsKey := sessionsUserKeyPrefix + userID

	_, err = s.rdb.Eval(ctx, createSessionScript,
		[]string{sessionKey, sessionAbsKey, userSessionsKey},
		userID, sessionID, int64(sessionExpiry.Seconds()), int64(sessionAbsoluteExpiry.Seconds()),
	).Result()
	if err != nil {
		return "", fmt.Errorf("store session: %w", err)
	}
	return sessionID, nil
}

// getSessionScript 原子地完成 session 存在性校验、绝对生命周期检查、TTL 刷新以及用户-session 索引维护。
// KEYS[1] = session:{session_id}, KEYS[2] = session:abs:{session_id}
// ARGV[1] = session_id, ARGV[2] = sliding_expiry_seconds
// 返回 session 对应的 user_id；session 不存在或绝对生命周期已过期时返回 nil。
var getSessionScript = `
local sessionKey = KEYS[1]
local sessionAbsKey = KEYS[2]
local sessionID = ARGV[1]
local expiry = tonumber(ARGV[2])
if not expiry or expiry <= 0 then
    return redis.error_reply('ERR invalid expiry')
end
if redis.call('exists', sessionAbsKey) == 0 then
    redis.call('del', sessionKey)
    return nil
end
local userID = redis.call('get', sessionKey)
if not userID then
    return nil
end
local userSessionsKey = 'sessions:user:' .. userID
redis.call('expire', sessionKey, expiry)
redis.call('sadd', userSessionsKey, sessionID)
redis.call('expire', userSessionsKey, expiry)
return userID
`

// Get returns the user ID associated with the session ID. It also refreshes
// the session TTL on every successful lookup (sliding expiration).
func (s *SessionManager) Get(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("session id is required")
	}
	sessionKey := sessionKeyPrefix + sessionID
	sessionAbsKey := sessionAbsoluteKeyPrefix + sessionID
	res, err := s.rdb.Eval(ctx, getSessionScript, []string{sessionKey, sessionAbsKey}, sessionID, int64(sessionExpiry.Seconds())).Result()
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}
	if res == nil {
		return "", redis.Nil
	}
	userID, ok := res.(string)
	if !ok || userID == "" {
		return "", fmt.Errorf("session has no user")
	}
	return userID, nil
}

// Delete removes a single session.
var deleteSessionScript = `
local sessionKey = KEYS[1]
local sessionAbsKey = KEYS[2]
local sessionID = ARGV[1]
local expiry = ARGV[2]
local userID = redis.call('get', sessionKey)
if not userID then
    return nil
end
local userSessionsKey = 'sessions:user:' .. userID
redis.call('srem', userSessionsKey, sessionID)
redis.call('del', sessionKey)
-- 同步删除绝对过期索引 key，避免 session:abs:* 残留。
redis.call('del', sessionAbsKey)
if tonumber(expiry) and tonumber(expiry) > 0 then
    redis.call('expire', userSessionsKey, expiry)
end
return userID
`

// deleteAllSessionsScript atomically lists all session IDs for a user,
// deletes the corresponding session keys, and then deletes the index set.
var deleteAllSessionsScript = `
local userSessionsKey = KEYS[1]
local sessionIDs = redis.call('smembers', userSessionsKey)
for i, id in ipairs(sessionIDs) do
    redis.call('del', 'session:' .. id)
    -- 同步删除绝对过期索引 key。
    redis.call('del', 'session:abs:' .. id)
end
redis.call('del', userSessionsKey)
return #sessionIDs
`

func (s *SessionManager) Delete(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	sessionKey := sessionKeyPrefix + sessionID
	sessionAbsKey := sessionAbsoluteKeyPrefix + sessionID
	_, err := s.rdb.Eval(ctx, deleteSessionScript, []string{sessionKey, sessionAbsKey}, sessionID, int64(sessionExpiry.Seconds())).Result()
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// flushAllSessionsScript 通过 SCAN 批量删除所有 session 相关 key。
// 用于开源版 OPEN_API_KEY 变更时强制所有用户退出登录。
// 每个 pattern 独立完整扫描（SCAN 游标是 keyspace 位置，跨 pattern 共用会漏删）；
// "session:*" 已覆盖 "session:abs:*" 前缀。
var flushAllSessionsScript = `
local count = 0
local patterns = {"session:*", "sessions:user:*"}
for _, pattern in ipairs(patterns) do
  local cursor = "0"
  repeat
    local result = redis.call("SCAN", cursor, "MATCH", pattern, "COUNT", 500)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
      redis.call("DEL", key)
      count = count + 1
    end
  until cursor == "0"
end
return count
`

func (s *SessionManager) FlushAllSessions(ctx context.Context) error {
	_, err := s.rdb.Eval(ctx, flushAllSessionsScript, []string{}).Result()
	if err != nil {
		return fmt.Errorf("flush all sessions: %w", err)
	}
	return nil
}

// DeleteAll removes all sessions for a user atomically using a Lua script.
func (s *SessionManager) DeleteAll(ctx context.Context, userID string) error {
	if userID == "" {
		return nil
	}
	userSessionsKey := sessionsUserKeyPrefix + userID
	_, err := s.rdb.Eval(ctx, deleteAllSessionsScript, []string{userSessionsKey}).Result()
	if err != nil {
		return fmt.Errorf("delete all user sessions: %w", err)
	}
	return nil
}

// SessionMiddleware authenticates requests using server-side sessions.
type SessionMiddleware struct {
	sessions *SessionManager
}

func NewSessionMiddleware(sessions *SessionManager) *SessionMiddleware {
	return &SessionMiddleware{sessions: sessions}
}

func (m *SessionMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionID := extractBearerToken(r)
		if sessionID == "" {
			JSONBizError(w, r, errors.BizSessionInvalid, "missing session")
			return
		}

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
	})
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
