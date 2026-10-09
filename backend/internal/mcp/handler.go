// Package mcp implements the MCP (Model Context Protocol) Streamable HTTP server.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/middleware"
	"papafeiji/backend/pkg/errors"
	"papafeiji/backend/pkg/timeutil"
	"papafeiji/backend/pkg/util"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

const (
	defaultMcpPageSize      = 500
	maxMcpEntries           = 1000
	mcpDiaryDefaultDaysBack = 90
	maxMcpDateRangeDays     = 180
	maxMcpMessageSize       = 64 << 10 // 64KB
	mcpCacheTTL             = 30 * time.Minute
)

// apiKeyNeverExpires 用于满足 api_keys.expires_at NOT NULL 约束，
// 同时实现「API Key 永不过期、永久展示」的产品需求（认证时不做过期校验）。
var apiKeyNeverExpires = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

type Handler struct {
	pool            *db.Pool
	cfg             *config.Config
	baseURL         string
	rateLimiter     *middleware.KeyRateLimiter
	publicIPLimiter *middleware.IPRateLimiter
	rdb             *redis.Client
}

func NewHandler(pool *db.Pool, cfg *config.Config, rdb *redis.Client) *Handler {
	return &Handler{
		pool:    pool,
		cfg:     cfg,
		baseURL: apiBaseURL(cfg.APIHost, cfg.HTTPBind, cfg.HTTPPort),
		rdb:     rdb,
		rateLimiter: middleware.NewKeyRateLimiter(30, time.Minute, 5000, func(r *http.Request) string {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				return "anon:" + middleware.ClientIP(r, cfg.TrustedProxyCIDR)
			}
			key := strings.TrimPrefix(auth, "Bearer ")
			if key == "" {
				return "anon:" + middleware.ClientIP(r, cfg.TrustedProxyCIDR)
			}
			h := sha256.Sum256([]byte(key))
			return hex.EncodeToString(h[:])
		}),
	}
}

// Stop 释放 handler 持有的资源。
func (h *Handler) Stop() {
	if h.rateLimiter != nil {
		h.rateLimiter.Stop()
	}
	if h.publicIPLimiter != nil {
		h.publicIPLimiter.Stop()
	}
}

func apiBaseURL(host string, bind string, port string) string {

	if host != "" {
		return strings.TrimSuffix(host, "/")
	}

	scheme := "https"
	addr := bind
	if addr == "" || addr == "0.0.0.0" {
		addr = "localhost"
	}
	baseURL := ""
	switch port {
	case "80", "443", "":
		baseURL = fmt.Sprintf("%s://%s", scheme, addr)
	default:
		baseURL = fmt.Sprintf("%s://%s:%s", scheme, addr, port)
	}
	return baseURL
}

func diaryURL(base string) string {
	return strings.TrimSuffix(base, "/") + "/mcp/diary"
}

func memoryURL(base string) string {
	return strings.TrimSuffix(base, "/") + "/mcp/memories"
}

func mcpServerURL(base string) string {
	return strings.TrimSuffix(base, "/") + "/mcp"
}

// publicBaseURL 返回 MCP 客户端应使用的公开入口：配置了 MCP_PUBLIC_URL
// （Cloudflare Worker 地址）时优先，否则回退源站地址。
func (h *Handler) publicBaseURL() string {
	if h.cfg.MCPPublicURL != "" {
		return strings.TrimSuffix(h.cfg.MCPPublicURL, "/")
	}
	return h.baseURL
}

// keyInfoResponse 构造 key 查询/创建/换发的统一响应。
// apiUrl 指向日记查询端点（GET），memoryUrl 指向记忆写入端点（POST），
// mcpConfig 的 url 与 authUrl（绑定页，仅 Worker 入口存在时返回）均指向公开入口。
func (h *Handler) keyInfoResponse(rawKey string, expiresAt time.Time) map[string]interface{} {
	base := h.publicBaseURL()
	resp := map[string]interface{}{
		"apiKey":    rawKey,
		"apiUrl":    diaryURL(base),
		"memoryUrl": memoryURL(base),
		"expiresAt": expiresAt.Format(time.RFC3339),
		"mcpConfig": buildMcpConfig(rawKey, base),
	}
	if h.cfg.MCPPublicURL != "" {
		resp["authUrl"] = base + "/auth"
	}
	return resp
}

func buildMcpConfig(apiKey, baseURL string) map[string]interface{} {
	return map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"memory": map[string]interface{}{
				"url": mcpServerURL(baseURL),
				"headers": map[string]string{
					"Authorization": "Bearer " + apiKey,
				},
			},
		},
	}
}

func (h *Handler) Register(r chi.Router) {
	r.Post("/mcp/key", h.CreateKey)
	r.Get("/mcp/key", h.GetKey)
	r.Delete("/mcp/key", h.DeleteKey)
	r.Post("/mcp/key/rotate", h.RotateKey)
}

// rejectAPIKeyIdentity 拒绝以 API Key 鉴权身份（X-Private-Api-Key 通路，sessionID 形如
// "apikey:<userID>"，见 open_auth.go）调用 key 管理接口。
// 原因：api_keys 有 UNIQUE(user_id)，open 模式种子 OPEN_API_KEY 占默认用户唯一一行，
// Rotate/Delete 会弄挂 Worker 鉴权；该身份的 key 即 OPEN_API_KEY，不可管理，
// key 管理必须使用登录会话。SaaS 模式 session 永无 "apikey:" 前缀，此守卫不会触发。
func rejectAPIKeyIdentity(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(middleware.SessionID(r.Context()), "apikey:") {
		middleware.JSONError(w, r, http.StatusForbidden, errors.CodeForbidden, "open 模式默认身份的 key 即 OPEN_API_KEY，不可管理；key 管理需登录会话")
		return true
	}
	return false
}

func (h *Handler) CreateKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if rejectAPIKeyIdentity(w, r) {
		return
	}
	userID := middleware.UserID(ctx)

	expiresAt := apiKeyNeverExpires

	var rawKey string
	err := db.WithTx(ctx, h.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		newKey, err := util.NewRandomToken(32)
		if err != nil {
			return fmt.Errorf("generate api key: %w", err)
		}
		keyID, err := util.NewUUID()
		if err != nil {
			return fmt.Errorf("generate api key id: %w", err)
		}

		created, err := q.CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
			ID:        keyID,
			UserID:    userID,
			KeyHash:   hashKey(newKey),
			ApiKey:    newKey,
			ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
		})
		if err != nil {
			if stderrors.Is(err, pgx.ErrNoRows) {
				existing, getErr := q.GetAPIKeyByUser(ctx, userID)
				if getErr != nil {
					return fmt.Errorf("get existing api key after conflict: %w", getErr)
				}
				rawKey = existing.ApiKey
				expiresAt = existing.ExpiresAt.Time
				return nil
			}
			return fmt.Errorf("create api key: %w", err)
		}
		rawKey = created.ApiKey
		return nil
	})
	if err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to create api key")
		return
	}

	h.respondWithAPIKey(w, r, rawKey, expiresAt)
}

func (h *Handler) respondWithAPIKey(w http.ResponseWriter, r *http.Request, rawKey string, expiresAt time.Time) {
	middleware.JSON(w, r, http.StatusOK, h.keyInfoResponse(rawKey, expiresAt))
}

func (h *Handler) GetKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if rejectAPIKeyIdentity(w, r) {
		return
	}
	userID := middleware.UserID(ctx)

	key, err := h.pool.Queries().GetAPIKeyByUser(ctx, userID)
	if err != nil {
		if stderrors.Is(err, pgx.ErrNoRows) {
			middleware.JSON(w, r, http.StatusOK, map[string]interface{}{"hasKey": false})
			return
		}
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to get api key")
		return
	}
	if key.ApiKey == "" {
		middleware.JSON(w, r, http.StatusOK, map[string]interface{}{"hasKey": false})
		return
	}

	resp := h.keyInfoResponse(key.ApiKey, key.ExpiresAt.Time)
	resp["hasKey"] = true
	middleware.JSON(w, r, http.StatusOK, resp)
}

func (h *Handler) DeleteKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if rejectAPIKeyIdentity(w, r) {
		return
	}
	userID := middleware.UserID(ctx)

	if err := h.pool.Queries().DeleteAPIKeyByUser(ctx, userID); err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to delete api key")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

// RotateKey 原子地作废旧 key 并签发新 key（事务内先删后建）。
// 与 CreateKey 的 get-or-create 幂等语义不同，用于「重新生成密钥」场景。
func (h *Handler) RotateKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if rejectAPIKeyIdentity(w, r) {
		return
	}
	userID := middleware.UserID(ctx)

	expiresAt := apiKeyNeverExpires

	var rawKey string
	err := db.WithTx(ctx, h.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		if err := q.DeleteAPIKeyByUser(ctx, userID); err != nil {
			return fmt.Errorf("delete old api key: %w", err)
		}
		newKey, err := util.NewRandomToken(32)
		if err != nil {
			return fmt.Errorf("generate api key: %w", err)
		}
		keyID, err := util.NewUUID()
		if err != nil {
			return fmt.Errorf("generate api key id: %w", err)
		}
		created, err := q.CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
			ID:        keyID,
			UserID:    userID,
			KeyHash:   hashKey(newKey),
			ApiKey:    newKey,
			ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("create api key: %w", err)
		}
		rawKey = created.ApiKey
		return nil
	})
	if err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to rotate api key")
		return
	}

	h.respondWithAPIKey(w, r, rawKey, expiresAt)
}

//nolint:dupl // 与 ListMemories 共用 authenticateMcpQuery 后的查询-响应尾部，结构重复可读性优先
func (h *Handler) GetDiary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	key, startDate, endDate, limit, ok := h.authenticateMcpQuery(w, r)
	if !ok {
		return
	}

	rows, err := h.listUserDiaryEntries(ctx, key.userID, startDate, endDate, limit)
	if err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to list diaries")
		return
	}

	data, truncated := buildMcpDiaryResponse(rows)
	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{"data": data, "truncated": truncated})
}

// authenticateMcpQuery 是查询类端点的公共前奏：限流 + API Key 鉴权 + 日期范围与 limit 解析。
func (h *Handler) authenticateMcpQuery(w http.ResponseWriter, r *http.Request) (key apiKeyInfo, startDate, endDate time.Time, limit int, ok bool) {
	if !h.rateLimiter.Allow(r) {
		middleware.JSONBizError(w, r, errors.BizRateLimited, "too many requests")
		return key, startDate, endDate, limit, false
	}
	var err error
	key, err = h.authenticateAPIKey(r.Context(), extractBearer(r.Header.Get("Authorization")))
	if err != nil {
		middleware.JSONError(w, r, http.StatusUnauthorized, errors.CodeUnauthorized, "invalid api key")
		return key, startDate, endDate, limit, false
	}
	startDate, endDate, err = parseMcpDateRange(r)
	if err != nil {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, err.Error())
		return key, startDate, endDate, limit, false
	}
	limit = parseMcpLimit(r)
	return key, startDate, endDate, limit, true
}

func (h *Handler) listUserDiaryEntries(ctx context.Context, userID string, startDate, endDate time.Time, limit int) ([]sqlc.ListUserMcpEntriesRow, error) {
	if h.rdb != nil {
		// 缓存 key 基于 userID；此函数由 MCP 鉴权流程调用，userID 来自 api_keys 表，
		// 与 OpenAuthMiddleware 的 session/API Key 分支无关，不会发生跨用户缓存污染。
		key := fmt.Sprintf("mcp:diary:%s:%s:%s:%d",
			userID, startDate.Format("2006-01-02"), endDate.Format("2006-01-02"), limit)
		if cached, err := h.rdb.Get(ctx, key).Bytes(); err == nil {
			var rows []sqlc.ListUserMcpEntriesRow
			if json.Unmarshal(cached, &rows) == nil {
				return rows, nil
			}
		}
		rows, err := h.pool.Queries().ListUserMcpEntries(ctx, sqlc.ListUserMcpEntriesParams{
			UserID:  userID,
			Column2: pgtype.Date{Time: startDate, Valid: true},
			Column3: pgtype.Date{Time: endDate, Valid: true},
			Limit:   int32(limit),
		})
		if err != nil {
			return nil, fmt.Errorf("list user mcp entries: %w", err)
		}
		if data, err := json.Marshal(rows); err == nil {
			// 超大结果不入缓存，避免 Redis 内存被大 payload 撑爆。
			if len(data) <= maxMcpMessageSize {
				_ = h.rdb.Set(ctx, key, data, mcpCacheTTL).Err() //nolint:errcheck
			}
		}
		return rows, nil
	}
	rows, err := h.pool.Queries().ListUserMcpEntries(ctx, sqlc.ListUserMcpEntriesParams{
		UserID:  userID,
		Column2: pgtype.Date{Time: startDate, Valid: true},
		Column3: pgtype.Date{Time: endDate, Valid: true},
		Limit:   int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list user mcp entries: %w", err)
	}
	return rows, nil
}

func parseMcpLimit(r *http.Request) int {
	limit := defaultMcpPageSize
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		// limit=0 视为合法（返回空结果），仅负值/非法值回落默认。
		if l, err := strconv.Atoi(limitStr); err == nil && l >= 0 {
			limit = l
		}
	}
	if limit > maxMcpEntries {
		limit = maxMcpEntries
	}
	return limit
}

func parseMcpDateRange(r *http.Request) (time.Time, time.Time, error) {
	now := timeutil.NowShanghai()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, timeutil.Shanghai)
	endDate := today.Add(24*time.Hour - time.Nanosecond)
	startDate := today.AddDate(0, 0, -mcpDiaryDefaultDaysBack)

	startStr := r.URL.Query().Get("start_date")
	if startStr != "" {
		parsed, err := time.ParseInLocation("2006-01-02", startStr, timeutil.Shanghai)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("start_date 格式不正确，应为 YYYY-MM-DD")
		}
		startDate = parsed
	}

	endStr := r.URL.Query().Get("end_date")
	if endStr != "" {
		parsed, err := time.ParseInLocation("2006-01-02", endStr, timeutil.Shanghai)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("end_date 格式不正确，应为 YYYY-MM-DD")
		}
		endDate = parsed.Add(24*time.Hour - time.Nanosecond)
	}

	if startDate.After(endDate) {
		return time.Time{}, time.Time{}, fmt.Errorf("开始日期不能晚于结束日期")
	}
	if endDate.Sub(startDate) > time.Hour*24*time.Duration(maxMcpDateRangeDays) {
		return time.Time{}, time.Time{}, fmt.Errorf("日期跨度超过限制，最大 %d 天", maxMcpDateRangeDays)
	}
	return startDate, endDate, nil
}

// buildMcpDiaryResponse 以字节预算增量构造响应：超过预算即停止追加条目并标记 truncated，
// 避免大文本日期范围先完整构造（每请求数十 MB 内存峰值）再整体拒绝。
func buildMcpDiaryResponse(rows []sqlc.ListUserMcpEntriesRow) ([]map[string]interface{}, bool) {
	const budgetBytes = maxMcpMessageSize * 4 / 5 // 预留外层 JSON 包装余量
	data := make([]map[string]interface{}, 0)     // 空结果返回 [] 而非 null，保证列表契约
	var currentDay map[string]interface{}
	var currentDate string
	var entries []map[string]interface{}
	used := 0
	truncated := false

	flushDay := func() {
		if currentDay != nil {
			currentDay["entryCount"] = len(entries)
			currentDay["entries"] = entries
			data = append(data, currentDay)
		}
	}

	for _, row := range rows {
		text := strings.TrimSpace(row.Text)
		used += len(text) + len(row.Location) + 128
		if used > budgetBytes {
			truncated = true
			break
		}
		dateKey := row.RecordDate.Time.Format("2006-01-02")
		if dateKey != currentDate {
			flushDay()
			currentDate = dateKey
			entries = nil
			currentDay = map[string]interface{}{
				"recordDate": dateKey,
			}
		}
		entries = append(entries, map[string]interface{}{
			"time":     row.RecordAt.Time.In(timeutil.Shanghai).Format("2006年01月02日15时"),
			"location": row.Location,
			"content":  text,
		})
	}
	flushDay()
	return data, truncated
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type createMemoryRequest struct {
	RecordTime time.Time `json:"record_time"`
	Title      string    `json:"title"`
	Content    string    `json:"content"`
}

type memoryItem struct {
	CreatedAt time.Time `json:"created_at"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Location  string    `json:"location"`
}

func (h *Handler) CreateMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if !h.rateLimiter.Allow(r) {
		middleware.JSONBizError(w, r, errors.BizRateLimited, "too many requests")
		return
	}
	key, err := h.authenticateAPIKey(ctx, extractBearer(r.Header.Get("Authorization")))
	if err != nil {
		middleware.JSONError(w, r, http.StatusUnauthorized, errors.CodeUnauthorized, "invalid api key")
		return
	}

	req, err := parseCreateMemoryRequest(w, r)
	if err != nil {
		if stderrors.Is(err, errMemoryBodyTooLarge) {
			middleware.JSONError(w, r, http.StatusRequestEntityTooLarge, errors.CodeRequestEntityTooLarge, err.Error())
			return
		}
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, err.Error())
		return
	}

	id, err := h.createMemoryForUser(ctx, key.userID, req)
	if err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to create memory")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{"memory_id": id})
}

// errMemoryBodyTooLarge：REST /memories 的 413 判定用 sentinel（02h MCP-5 要求 400 失败
// 带中文 message，413 亦以中文回显），替代此前丢失错误类型的字符串匹配。
var errMemoryBodyTooLarge = stderrors.New("请求体超过 64KB 限制")

func parseCreateMemoryRequest(w http.ResponseWriter, r *http.Request) (createMemoryRequest, error) {
	var req createMemoryRequest
	if err := middleware.ReadJSONBody(w, r, &req, maxMcpMessageSize); err != nil {
		var maxErr *http.MaxBytesError
		if stderrors.As(err, &maxErr) {
			return req, errMemoryBodyTooLarge
		}
		return req, fmt.Errorf("请求体格式非法")
	}
	if req.RecordTime.IsZero() {
		return req, fmt.Errorf("缺少 record_time 参数")
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" || utf8.RuneCountInString(req.Title) > 50 {
		return req, fmt.Errorf("标题最长 50 个字")
	}
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" || utf8.RuneCountInString(req.Content) > 10000 {
		return req, fmt.Errorf("内容需为 1-10000 个字")
	}
	return req, nil
}

func (h *Handler) createMemoryForUser(ctx context.Context, userID string, req createMemoryRequest) (string, error) {
	id, err := util.NewUUID()
	if err != nil {
		return "", fmt.Errorf("generate memory id: %w", err)
	}
	t := req.RecordTime.In(timeutil.Shanghai)
	recordDate := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	recordDateTime := pgtype.Date{Time: recordDate, Valid: true}
	err = db.WithTx(ctx, h.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		if _, err := q.CreateMemory(ctx, sqlc.CreateMemoryParams{
			ID:         id,
			UserID:     userID,
			RecordTime: pgtype.Timestamptz{Time: req.RecordTime, Valid: true},
			RecordDate: recordDateTime,
			Title:      req.Title,
			Content:    req.Content,
		}); err != nil {
			return fmt.Errorf("create memory: %w", err)
		}
		if _, err := q.UpsertDiary(ctx, sqlc.UpsertDiaryParams{
			ID:         id,
			UserID:     userID,
			RecordDate: recordDateTime,
		}); err != nil {
			return fmt.Errorf("upsert diary for memory: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	// 创建成功后尽力失效该用户的 MCP 查询缓存；失败由 TTL 兜底。
	h.invalidateMcpCache(ctx, userID)
	return id, nil
}

// InvalidateUserCache 删除指定用户的 MCP 查询缓存，按 pattern 小批量 Scan 删除。
// 失败时由 TTL 兜底；可被其他模块（如 diary）调用以保持 MCP 缓存一致性。
func InvalidateUserCache(ctx context.Context, rdb *redis.Client, userID string) {
	if rdb == nil || userID == "" {
		return
	}
	patterns := []string{
		fmt.Sprintf("mcp:diary:%s:*", userID),
		fmt.Sprintf("mcp:mem:%s:*", userID),
	}
	for _, pattern := range patterns {
		var cursor uint64
		for {
			keys, next, err := rdb.Scan(ctx, cursor, pattern, 100).Result()
			if err != nil {
				break
			}
			if len(keys) > 0 {
				_ = rdb.Del(ctx, keys...).Err() //nolint:errcheck
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
	}
}

// invalidateMcpCache 删除当前 handler 关联用户的 MCP 查询缓存。
func (h *Handler) invalidateMcpCache(ctx context.Context, userID string) {
	// 用 WithoutCancel 执行失效：客户端断开不应中断缓存清理，与 diary 模块策略一致。
	InvalidateUserCache(context.WithoutCancel(ctx), h.rdb, userID)
}

//nolint:dupl // 与 GetDiary 共用 authenticateMcpQuery 后的查询-响应尾部，结构重复可读性优先
func (h *Handler) ListMemories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	key, startDate, endDate, limit, ok := h.authenticateMcpQuery(w, r)
	if !ok {
		return
	}

	data, err := h.queryMemoryItems(ctx, key.userID, startDate, endDate, limit)
	if err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to list memories")
		return
	}

	data, truncated := truncateMemoryItems(data)
	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{"data": data, "truncated": truncated})
}

// truncateMemoryItems 以字节预算截断记忆列表（同 buildMcpDiaryResponse 的预算策略）。
func truncateMemoryItems(items []memoryItem) ([]memoryItem, bool) {
	if items == nil {
		return []memoryItem{}, false // 空结果返回 [] 而非 null，保证列表契约
	}
	const budgetBytes = maxMcpMessageSize * 4 / 5
	used := 0
	for i, item := range items {
		used += len(item.Title) + len(item.Content) + len(item.Location) + 128
		if used > budgetBytes {
			return items[:i], true
		}
	}
	return items, false
}

func (h *Handler) queryMemoryItems(ctx context.Context, userID string, startDate, endDate time.Time, limit int) ([]memoryItem, error) {
	if h.rdb != nil {
		key := fmt.Sprintf("mcp:mem:%s:%s:%s:%d",
			userID, startDate.Format("2006-01-02"), endDate.Format("2006-01-02"), limit)
		if cached, err := h.rdb.Get(ctx, key).Bytes(); err == nil {
			var items []memoryItem
			if json.Unmarshal(cached, &items) == nil {
				return items, nil
			}
		}
		items, err := h.queryMemoryItemsDB(ctx, userID, startDate, endDate, limit)
		if err != nil {
			return nil, err
		}
		// 与 listUserDiaryEntries 一致，仅小结果写缓存，防止大日期范围组合撑爆 Redis。
		if data, err := json.Marshal(items); err == nil && len(data) <= maxMcpMessageSize {
			_ = h.rdb.Set(ctx, key, data, mcpCacheTTL).Err() //nolint:errcheck
		}
		return items, nil
	}
	return h.queryMemoryItemsDB(ctx, userID, startDate, endDate, limit)
}

func (h *Handler) queryMemoryItemsDB(ctx context.Context, userID string, startDate, endDate time.Time, limit int) ([]memoryItem, error) {
	memories, err := h.pool.Queries().ListMemoriesByDateRange(ctx, sqlc.ListMemoriesByDateRangeParams{
		UserID:  userID,
		Column2: pgtype.Date{Time: startDate, Valid: true},
		Column3: pgtype.Date{Time: endDate, Valid: true},
		Limit:   int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}

	diaries, err := h.pool.Queries().ListDiaryEntriesByDateRange(ctx, sqlc.ListDiaryEntriesByDateRangeParams{
		UserID:  userID,
		Column2: pgtype.Date{Time: startDate, Valid: true},
		Column3: pgtype.Date{Time: endDate, Valid: true},
		Limit:   int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list diaries: %w", err)
	}

	return mergeMemoryItems(memories, diaries, limit), nil
}

func mergeMemoryItems(memories []sqlc.ListMemoriesByDateRangeRow, diaries []sqlc.ListDiaryEntriesByDateRangeRow, limit int) []memoryItem {
	result := make([]memoryItem, 0, limit)
	i, j := 0, 0
	for len(result) < limit && (i < len(memories) || j < len(diaries)) {
		if j >= len(diaries) || (i < len(memories) && memories[i].CreatedAt.Time.After(diaries[j].CreatedAt.Time)) {
			result = append(result, memoryItem{
				CreatedAt: memories[i].CreatedAt.Time,
				Title:     memories[i].Title,
				Content:   memories[i].Content,
				Location:  "",
			})
			i++
		} else {
			result = append(result, memoryItem{
				CreatedAt: diaries[j].CreatedAt.Time,
				Title:     "",
				Content:   diaries[j].Content,
				Location:  diaries[j].Location,
			})
			j++
		}
	}
	return result
}
