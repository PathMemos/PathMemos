// Package ai provides related functionality.
package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"

	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/pkg/safe"
	"papafeiji/backend/internal/vip"
	"papafeiji/backend/pkg/timeutil"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

// dailyQuotaLua 对 Redis 辅助缓存做原子 check+incr，并在超出 limit 时自动回退。
// 用于 AI 配额消费的 Redis 前置闸门：Redis 异常时失败关闭，避免触碰数据库。
var dailyQuotaLua = redis.NewScript(`
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local cur = redis.call('incr', key)
if cur == 1 then
    redis.call('expire', key, ttl)
end
if cur > limit then
    redis.call('decr', key)
    return 0
end
return cur
`)

// refundDailyQuotaLua 对 Redis 辅助缓存做原子退费，仅在 key 存在且大于 0 时递减。
// 避免退费时将并发增量覆盖，或在 key 不存在时创建负值计数。
var refundDailyQuotaLua = redis.NewScript(`
local key = KEYS[1]
if redis.call('exists', key) == 0 then
    return -1
end
local cur = tonumber(redis.call('get', key))
if cur == nil then
    return -1
end
if cur > 0 then
    redis.call('decr', key)
    return redis.call('get', key)
end
return cur
`)

const (
	// aiTurnKeyTTL 同一轮消息「处理中」标记的存活时间；超过则视为遗留标记，允许重新处理。
	aiTurnKeyTTL = 240 * time.Second
	// aiReplyTTL 完整回复的缓存时间：覆盖小程序断线自动重连（秒级）与弱网重试场景。
	aiReplyTTL = 10 * time.Minute
	// aiTurnPollInterval 等待在途请求期间的轮询间隔。
	// （重连等待窗口已对齐 aiTurnKeyTTL，见 Chat 内重连幂等分支，不再使用独立短超时）
	aiTurnPollInterval = 200 * time.Millisecond
	// aiReplayChunkRunes 回放缓存回复时的分片大小（按字符数），保持流式体验。
	aiReplayChunkRunes = 120
)

// aiTurnKeys 生成同一轮消息的「处理中」标记键与完整回复缓存键。
// 客户端带 request_id 时（每次发送生成一次，重连重发复用同一值）以其为键——
// 精确覆盖「断线重连」场景；不带 request_id 的旧调用方（公众号回复等）回退到
// 消息内容哈希，仍能去重但存在「10 分钟内主动重复发相同消息被误判为回放」的边界。
func aiTurnKeys(userID, msg, requestID string) (turnKey, replyKey string) {
	base := msg
	if requestID != "" {
		base = requestID
	}
	h := sha256.Sum256([]byte(userID + "\n" + base))
	d := hex.EncodeToString(h[:])
	return "ai:turn:" + d, "ai:reply:" + d
}

// streamReplay 把缓存中的完整回复按片回放给 onChunk，任一回调失败即停止。
func streamReplay(reply string, onChunk func(string) error) error {
	if onChunk == nil {
		return nil
	}
	runes := []rune(reply)
	for i := 0; i < len(runes); i += aiReplayChunkRunes {
		end := i + aiReplayChunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		if err := onChunk(string(runes[i:end])); err != nil {
			return err
		}
	}
	return nil
}

// dailyQuotaCacheTTLFor 返回指定日期对应的 Redis 缓存 TTL：到次日 0 点上海时区的剩余时间 + 60 秒缓冲。
// 保证配额缓存按自然日过期，避免 24 小时固定 TTL 导致的跨天计数偏差。
func dailyQuotaCacheTTLFor(t time.Time) time.Duration {
	tz := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).Add(24 * time.Hour)
	return tz.Sub(t) + 60*time.Second
}

// Service holds AI chat business logic independent of transport (SSE / WeChat MP / etc.).
type Service struct {
	pool         *db.Pool
	rdb          *redis.Client
	vipService   vip.InfoProvider
	sysCfgLoader *config.SysConfigLoader
	cfg          *config.Config
	client       *UpstreamClient
}

// NewService creates an AI chat service.
func NewService(pool *db.Pool, rdb *redis.Client, vipService vip.InfoProvider, sysCfgLoader *config.SysConfigLoader, cfg *config.Config) *Service {
	return &Service{
		pool:         pool,
		rdb:          rdb,
		vipService:   vipService,
		sysCfgLoader: sysCfgLoader,
		cfg:          cfg,
		client:       NewUpstreamClient(cfg),
	}
}

func (s *Service) sysCfg(ctx context.Context) (*config.SysConfig, error) {
	return s.sysCfgLoader.Load(ctx)
}

// logUpstreamAlert 限频输出 AI 上游告警关键字（5 分钟一次），由 alert-watch 捕获推送。
// 上游故障在此前完全不可见（用户只看到对话失败），这是其唯一观测面（R-19）。
var lastUpstreamAlertUnixMilli atomic.Int64

func logUpstreamAlert(ctx context.Context, err error) {
	now := time.Now().UnixMilli()
	last := lastUpstreamAlertUnixMilli.Load()
	if last != 0 && now-last < 5*60*1000 {
		return
	}
	if !lastUpstreamAlertUnixMilli.CompareAndSwap(last, now) {
		return
	}
	slog.ErrorContext(ctx, "alert=ai_upstream_error", slog.Any("error", err))
}

// Chat runs a complete AI chat turn. If onChunk is non-nil it is called for each streamed chunk.
// It returns the full assistant reply. Quota is consumed at the start and refunded automatically
// if no reply could be produced due to timeout or upstream error.
// requestID 为客户端每轮生成的幂等标识（重连重发复用同一值），为空时回退消息内容哈希。
func (s *Service) Chat(ctx context.Context, userID, message, requestID string, onChunk func(string) error) (string, error) {
	sysCfg, err := s.sysCfg(ctx)
	if err != nil {
		return "", fmt.Errorf("load sys config: %w", err)
	}
	return s.chatWithPrompt(ctx, userID, message, onChunk, sysCfg.AIPrompt, sysCfg, requestID)
}

// ChatWithPromptUsingConfig runs ChatWithPrompt using an already loaded config to avoid duplicate DB loads.
func (s *Service) ChatWithPromptUsingConfig(ctx context.Context, userID, message string, onChunk func(string) error, systemPrompt string, sysCfg *config.SysConfig) (string, error) {
	if systemPrompt == "" {
		systemPrompt = sysCfg.AIPrompt
	}
	return s.chatWithPrompt(ctx, userID, message, onChunk, systemPrompt, sysCfg, "")
}

func (s *Service) chatWithPrompt(ctx context.Context, userID, message string, onChunk func(string) error, systemPrompt string, sysCfg *config.SysConfig, requestID string) (reply string, err error) {
	if userID == "" {
		return "", fmt.Errorf("user_id is empty")
	}

	msg := strings.TrimSpace(message)
	if msg == "" {
		return "", fmt.Errorf("message is empty")
	}
	if utf8.RuneCountInString(msg) > maxMessageCodePoints {
		return "", fmt.Errorf("message too long")
	}

	// —— 重连幂等（R3）：客户端断线自动重连会重发同一轮消息，
	// 此处避免重复调用上游、重复扣每日配额、重复落库——
	turnKey, replyKey := aiTurnKeys(userID, msg, requestID)
	complete := false
	acquired := false
	if s.rdb != nil {
		// 1) 完整回复已缓存（上一轮已生成但客户端没收到）：直接回放，零成本。
		if cached, gErr := s.rdb.Get(ctx, replyKey).Result(); gErr == nil && cached != "" {
			slog.InfoContext(ctx, "ai chat replay from cache", slog.String("user_id", userID))
			_ = streamReplay(cached, onChunk) //nolint:errcheck // 客户端停止接收时中止回放即可
			return cached, nil
		}
		// 2) 同一轮消息正在处理中（重连竞态）：短暂等待在途请求完成，尽量复用其回复。
		if ok, sErr := s.rdb.SetNX(ctx, turnKey, "1", aiTurnKeyTTL).Result(); sErr == nil && ok {
			acquired = true
		} else if sErr == nil && !ok {
			// 在途请求可能流式生成最长 AIStreamTimeout(180s)；等待窗口对齐 turnKey TTL(240s)：
			// 在途完成写 replyKey 即回放。R2：等待超时或在途失败绝不回落生成——
			// 否则同一 request_id 会二次调用上游、二次扣配额、二次落库；统一返回
			// OPERATION_IN_PROGRESS，由客户端稍后重试（残留标记由 TTL 兜底）。
			deadline := time.Now().Add(aiTurnKeyTTL)
			for time.Now().Before(deadline) {
				if cached, gErr := s.rdb.Get(ctx, replyKey).Result(); gErr == nil && cached != "" {
					slog.InfoContext(ctx, "ai chat replay after in-flight turn", slog.String("user_id", userID))
					_ = streamReplay(cached, onChunk) //nolint:errcheck
					return cached, nil
				}
				if _, gErr := s.rdb.Get(ctx, turnKey).Result(); gErr != nil {
					// 在途标记已释放但无回复缓存：在途请求失败（含空回复）。
					return "", errAITurnInProgress
				}
				select {
				case <-ctx.Done():
					// AI-P2-06：在途等待期间 ctx 到期属请求超时，返回 timeout 语义，
					// 避免 handler 误报为 upstream error；客户端主动取消仍为 cancelled。
					if stderrors.Is(ctx.Err(), context.DeadlineExceeded) {
						return "", errAIChatTimeout
					}
					return "", fmt.Errorf("stream context cancelled")
				case <-time.After(aiTurnPollInterval):
				}
			}
			return "", errAITurnInProgress
		}
		// Redis 异常（SetNX 报错）时按未获取处理，走正常路径，绝不阻断对话。
	}
	// 只有上游流完整结束（EOF）才缓存回放；客户端断线后上游流继续消费到 EOF（见 streamCtx
	// 的 WithoutCancel），完整回复写入 reply 缓存供重连回放，避免重复扣配额与重复落库。
	defer func() {
		if s.rdb == nil {
			return
		}
		// 先写 replyKey 再释放 turnKey：重连轮询先查 replyKey 再查 turnKey，
		// 若先释放 turnKey，重连可能在 replyKey 尚未写入的瞬间 break 走正常生成，
		// 造成重复扣配额+重复落库。
		if complete && reply != "" {
			s.rdb.Set(context.WithoutCancel(ctx), replyKey, reply, aiReplyTTL) //nolint:errcheck // 缓存失败仅失去回放能力
		}
		if acquired {
			s.rdb.Del(context.WithoutCancel(ctx), turnKey) //nolint:errcheck // 清理失败由 TTL 兜底
		}
	}()

	user, err := s.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("get user: %w", err)
	}

	familyID := ""
	if user.CurrentFamilyID.Valid {
		familyID = user.CurrentFamilyID.String
	}

	background, err := s.buildBackground(ctx, userID, familyID)
	if err != nil {
		return "", fmt.Errorf("build background: %w", err)
	}

	dialogLogs, err := s.pool.Queries().ListRecentDialogLogs(ctx, sqlcArg(userID, 20))
	if err != nil {
		return "", fmt.Errorf("list recent dialog logs: %w", err)
	}

	info, err := s.vipService.GetVIPInfo(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("get vip info for quota check: %w", err)
	}
	quota := dailyQuotaNonVIP
	if info.IsVIP {
		quota = dailyQuotaVIP
	}
	today := timeutil.NowShanghai()
	quotaDate := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	quotaOK, quotaErr := s.tryConsumeDailyQuota(ctx, userID, quota, quotaDate)
	if quotaErr != nil {
		return "", fmt.Errorf("consume quota: %w", quotaErr)
	}
	if !quotaOK {
		return "", ErrAIDailyQuotaExceeded
	}

	nickname := ""
	if user.Nickname.Valid && user.Nickname.String != "" {
		nickname = user.Nickname.String
	}

	messages := buildMessages(systemPrompt, nickname, background, dialogLogs, msg, user.Lang)

	// 流上下文独立于请求 ctx：客户端断线重连时，上游流继续消费到 EOF 并写入 reply 缓存，
	// 让重连请求命中回放，避免重复扣配额+重复落库；仍受 AIStreamTimeout 上限约束。
	streamCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), AIStreamTimeout)
	defer cancel()

	stream, err := s.client.Stream(streamCtx, sysCfg.AIBaseURL, sysCfg.AIModel, sysCfg.AIThinkingType, sysCfg.AIMaxTokens, messages)
	if err != nil {
		// 连接失败/非 200：上游不可用的第一现场，限频告警。
		logUpstreamAlert(ctx, err)
		safe.Go(ctx, nil, func() { s.refundDailyQuota(userID, quotaDate) })
		return "", fmt.Errorf("upstream stream: %w", err)
	}
	//nolint:errcheck
	defer stream.Close()

	var fullReply strings.Builder
	clientGone := false
	for {
		if err := streamCtx.Err(); err != nil {
			if fullReply.Len() == 0 {
				safe.Go(ctx, nil, func() { s.refundDailyQuota(userID, quotaDate) })
			}
			if fullReply.Len() > 0 {
				reply := fullReply.String()
				s.saveLogAsync(ctx, userID, msg, reply)
				return reply, nil
			}
			if stderrors.Is(err, context.DeadlineExceeded) {
				return "", errAIChatTimeout
			}
			return "", fmt.Errorf("stream context cancelled")
		}

		chunk, err := stream.Next()
		if err != nil {
			// R2-L02：流正常结束（[DONE] 或连接关闭）直接跳出，不再进入错误分支。
			if stderrors.Is(err, io.EOF) {
				complete = true
				break
			}
			if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
				if fullReply.Len() == 0 {
					safe.Go(ctx, nil, func() { s.refundDailyQuota(userID, quotaDate) })
				}
				if fullReply.Len() > 0 {
					reply := fullReply.String()
					s.saveLogAsync(ctx, userID, msg, reply)
					return reply, nil
				}
				// 区分超时与客户端主动断开：Canceled 不是超时，避免下发语义错误的 timeout SSE 事件。
				if stderrors.Is(err, context.DeadlineExceeded) {
					return "", errAIChatTimeout
				}
				return "", fmt.Errorf("stream context cancelled")
			}

			if fullReply.Len() == 0 {
				// 中途流错误且无任何回复：上游故障，限频告警（超时/取消已在上方分支返回）。
				logUpstreamAlert(ctx, err)
				safe.Go(ctx, nil, func() { s.refundDailyQuota(userID, quotaDate) })
			}
			if fullReply.Len() > 0 {
				reply := fullReply.String()
				s.saveLogAsync(ctx, userID, msg, reply)
				return reply, nil
			}
			return "", fmt.Errorf("upstream error: %w", err)
		}
		// R2-L02：空 chunk（上游心跳/占位）跳过，不再误判为流结束。
		if chunk == "" {
			continue
		}
		fullReply.WriteString(chunk)
		if onChunk != nil && !clientGone {
			if err := onChunk(chunk); err != nil {
				// 客户端已断开（或停止接收）：置 clientGone，继续把上游流消费到 EOF，
				// 由 defer 写入 reply 缓存供重连请求回放，不再向已断开的客户端写。
				clientGone = true
				slog.InfoContext(ctx, "ai chat client disconnected, continue consuming for replay cache", slog.String("user_id", userID))
			}
		}
	}

	reply = fullReply.String()
	if promptTokens, completionTokens := stream.Usage(); promptTokens > 0 || completionTokens > 0 {
		// B3 成本观测：上游回报的 token 用量（尽力而为的日志，不落库），供 ai-cost.sh 汇总。
		slog.InfoContext(ctx, "ai chat usage",
			slog.String("user_id", userID),
			slog.Int("prompt_tokens", promptTokens),
			slog.Int("completion_tokens", completionTokens))
	}
	if reply != "" {
		s.saveLogAsync(ctx, userID, msg, reply)
	} else {
		// 流以 [DONE]/EOF 干净结束但内容为空（thinking 模型仅输出 reasoning_content、或 max_tokens
		// 被推理阶段耗尽）：无可用回复，退配额（AI11「无回复必退配额」）。
		safe.Go(ctx, nil, func() { s.refundDailyQuota(userID, quotaDate) })
	}

	return reply, nil
}

func (s *Service) buildBackground(ctx context.Context, userID, familyID string) (string, error) {
	if familyID == "" {
		return "", nil
	}

	cacheKey := "ai:family_summary:" + familyID
	if cached, err := s.rdb.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
		return cached, nil
	}

	rows, err := s.pool.Queries().ListAIBackgroundEntries(ctx, sqlcListArg(familyID, maxBackgroundEntries))
	if err != nil {
		return "", fmt.Errorf("list ai background entries: %w", err)
	}

	if len(rows) == 0 {
		_ = s.rdb.Set(ctx, cacheKey, "暂无日记记录", backgroundCacheTTL).Err() //nolint:errcheck // cache write is best-effort
		return "暂无日记记录", nil
	}

	// PD-1：背景上下文按 rune 截断。原 10 万 runes 在活跃家庭可拼出 5~10 万 token，
	// 超过上游 DeepSeek 窗口导致 400/upstream error；收敛到与审计一致的 2 万 runes
	//（约 2 万 token），为 system prompt、历史与输出留足窗口；截断保留最新记录。
	const maxBackgroundLen = 20000

	var parts []string
	for _, r := range rows {
		if r.Content == "" {
			continue
		}
		parts = append(parts, r.Nickname+":\n"+r.Content)
	}

	background := strings.Join(parts, "\n\n")
	background = truncateRunes(background, maxBackgroundLen)
	if background == "" {
		background = "暂无日记记录"
	}
	_ = s.rdb.Set(ctx, cacheKey, background, backgroundCacheTTL).Err() //nolint:errcheck // cache write is best-effort
	return background, nil
}

// truncateRunes 按 rune 数截断字符串，避免在多字节字符中间切断。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// tryConsumeDailyQuota 以数据库为配额权威，Redis 仅作辅助快速闸门。
// Redis 补偿/退费失败时，缓存计数可能短暂领先于 DB，导致用户在缓存 TTL 内被误限流；
// 该不一致会在缓存自然过期后按 DB 权威值重建，业务上接受这一小概率窗口（AGENTS.md §4.2/§3.10）。
func (s *Service) tryConsumeDailyQuota(ctx context.Context, userID string, quota int, quotaDate time.Time) (bool, error) {
	dateStr := quotaDate.Format("2006-01-02")
	key := fmt.Sprintf("%s:%s:%s", dailyQuotaKeyPrefix, userID, dateStr)

	// Redis 前置快速闸门：已超限失败关闭，不触碰数据库。
	// 缓存 TTL 按自然日，避免跨天计数与 DB 权威值不一致。
	ttlSeconds := int(dailyQuotaCacheTTLFor(quotaDate).Seconds())
	redisCur, err := dailyQuotaLua.Run(ctx, s.rdb, []string{key}, quota, ttlSeconds).Int64()
	// 仅在闸门 Lua 成功 incr 后才允许回补 Decr：Redis 故障 fail-open 时 key 未创建，
	// 无条件 Decr 会造出无 TTL 的 -1 持久键（多得一次配额偏差且旧日期键永久残留）。
	gateIncr := false
	if err != nil {
		// Redis 辅助闸门故障时降级放行（fail-open）：DB 每日配额（IncrementAIDailyQuotaUsed 的
		// WHERE used<quota 原子条件扣减）仍是权威限制，配额不会因 Redis 抖动被绕过；
		// 仅让已超限用户在 Redis 故障期间多打一次 DB 才被拒（配合 30/min IP 限流，量级有界）。
		// 权衡：避免 Redis 抖动导致 AI 核心功能整体不可用（AGENTS.md 性能与稳定性优先）。
		slog.WarnContext(ctx, "ai daily quota redis gate unavailable, fall through to db authority",
			slog.String("user_id", userID),
			slog.Any("error", err))
	} else if redisCur <= 0 {
		return false, nil
	} else {
		gateIncr = true
	}

	// 数据库为根本：原子扣减并返回扣减后的使用量及是否实际发生扣减。
	incrResult, err := s.pool.Queries().IncrementAIDailyQuotaUsed(ctx, sqlc.IncrementAIDailyQuotaUsedParams{
		UserID:    userID,
		QuotaDate: pgtype.Date{Time: quotaDate, Valid: true},
		Used:      int32(quota),
	})
	if err != nil {
		// DB 异常时回补 Redis，避免 Redis 计数领先于 DB。
		if gateIncr {
			_ = s.rdb.Decr(ctx, key).Err() //nolint:errcheck // redis rollback is best-effort
		}
		return false, fmt.Errorf("increment ai daily quota used: %w", err)
	}
	if !incrResult.Incremented {
		// DB 未实际扣减（已超限），回补 Redis 辅助缓存。
		if gateIncr {
			_ = s.rdb.Decr(ctx, key).Err() //nolint:errcheck // redis rollback is best-effort
		}
		return false, nil
	}

	return true, nil
}

func (s *Service) refundDailyQuota(userID string, quotaDate time.Time) {
	var dbUsed int32
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond * time.Duration(attempt))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		dbUsed, lastErr = s.pool.Queries().DecrementAIDailyQuotaUsed(ctx, sqlc.DecrementAIDailyQuotaUsedParams{
			UserID:    userID,
			QuotaDate: pgtype.Date{Time: quotaDate, Valid: true},
		})
		cancel()
		if lastErr == nil {
			break
		}
		// 用户并发注销时配额行已随注销事务清理，Decrement 返回 ErrNoRows；
		// 这是良性场景，视为退款成功，不重试也不误告警（ai.md 已接受风险）。
		if stderrors.Is(lastErr, pgx.ErrNoRows) {
			slog.Info("ai quota refund skipped: quota row already removed (user deleted)", slog.String("user_id", userID))
			return
		}

	}
	if lastErr != nil {
		// DB 退款失败意味着用户可能被扣减配额但未获得有效回复，需要触发监控告警人工兜底。
		slog.Error("alert:ai_quota_refund_failed", slog.String("user_id", userID), slog.String("quota_date", quotaDate.Format("2006-01-02")), slog.Any("error", lastErr))
		return
	}

	// DB 退款成功后，对 Redis 辅助缓存做原子退费；若缓存已过期则按 DB 权威值重建。
	key := fmt.Sprintf("%s:%s:%s", dailyQuotaKeyPrefix, userID, quotaDate.Format("2006-01-02"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	redisCur, err := refundDailyQuotaLua.Run(ctx, s.rdb, []string{key}).Int64()
	if err != nil {
		slog.WarnContext(ctx, "ai quota refund redis decrement failed", slog.String("user_id", userID), slog.Any("error", err))
		return
	}
	if redisCur == -1 {
		_ = s.rdb.Set(ctx, key, dbUsed, dailyQuotaCacheTTLFor(quotaDate)).Err() //nolint:errcheck // cache rebuild is best-effort
	}
}

func (s *Service) saveLogAsync(ctx context.Context, userID, userMsg, assistantMsg string) {
	safe.Go(ctx, nil, func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.saveLog(bgCtx, userID, userMsg, assistantMsg) //nolint:errcheck // async log save is best-effort
	})
}

func (s *Service) saveLog(ctx context.Context, userID, userMsg, assistantMsg string) error {
	if userMsg == "" && assistantMsg == "" {
		return nil
	}
	now := timeutil.NowShanghai()
	return db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		userLogID, err := newID()
		if err != nil {
			return fmt.Errorf("generate user log id: %w", err)
		}
		if err := q.InsertAIDialogLog(ctx, sqlcInsertArg(userLogID, userID, "user", userMsg, now)); err != nil {
			return fmt.Errorf("insert user ai dialog log: %w", err)
		}
		if assistantMsg != "" {
			assistantLogID, err := newID()
			if err != nil {
				return fmt.Errorf("generate assistant log id: %w", err)
			}
			if err := q.InsertAIDialogLog(ctx, sqlcInsertArg(assistantLogID, userID, "assistant", assistantMsg, now)); err != nil {
				return fmt.Errorf("insert assistant ai dialog log: %w", err)
			}
		}
		return nil
	})
}

var (
	ErrAIDailyQuotaExceeded = fmt.Errorf("daily ai chat quota exceeded")
	errAIChatTimeout        = stderrors.New("ai chat timeout")
	errAITurnInProgress     = stderrors.New("ai turn in progress")
)
