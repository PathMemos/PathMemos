// Package main provides related functionality.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	goredis "github.com/redis/go-redis/v9"

	"papafeiji/backend/internal/ai"
	"papafeiji/backend/internal/auth"
	"papafeiji/backend/internal/autorecord"
	"papafeiji/backend/internal/avatar"
	"papafeiji/backend/internal/bootstrap"
	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/diary"
	"papafeiji/backend/internal/family"
	"papafeiji/backend/internal/file"
	"papafeiji/backend/internal/invite"
	"papafeiji/backend/internal/jobs"
	"papafeiji/backend/internal/location"
	"papafeiji/backend/internal/mcp"
	mw "papafeiji/backend/internal/middleware"
	"papafeiji/backend/internal/migration"
	"papafeiji/backend/internal/opslog"
	"papafeiji/backend/internal/payment"
	"papafeiji/backend/internal/pkg/safe"
	"papafeiji/backend/internal/purge"
	"papafeiji/backend/internal/push"
	"papafeiji/backend/internal/redis"
	"papafeiji/backend/internal/system"
	"papafeiji/backend/internal/user"
	"papafeiji/backend/internal/vip"
	"papafeiji/backend/internal/wxmp"
	"papafeiji/backend/migratefs"
	"papafeiji/backend/pkg/limiter"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server failed to start", slog.Any("error", err))
		os.Exit(1)
	}
}

var shuttingDown atomic.Bool

func run() error {
	ctx := context.Background()
	baseCtx, baseCancel := context.WithCancel(ctx)
	defer baseCancel()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	// FP-P2-03：SaaS 公众号密钥不完整时不会启动失败，但公众号渠道会静默半可用；
	// 启动时显式告警便于排查（不 fail-closed，避免非核心渠道阻断主服务）。
	if cfg.DeploymentMode == "saas" {
		var missingMP []string
		if cfg.WechatMPAppID == "" {
			missingMP = append(missingMP, "WECHAT_MP_APPID")
		}
		if cfg.WechatMPSecret == "" {
			missingMP = append(missingMP, "WECHAT_MP_SECRET")
		}
		if cfg.WechatMPGhID == "" {
			missingMP = append(missingMP, "WECHAT_MP_GHID")
		}
		if cfg.WechatEncodingAESKey == "" {
			missingMP = append(missingMP, "WECHAT_ENCODING_AES_KEY")
		}
		if len(missingMP) > 0 {
			logger.Warn("wechat mp config incomplete, official-account channel may be partially unavailable", slog.Any("missing", missingMP))
		}
	}

	pgPool, err := db.NewPool(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pgPool.Close()

	pool := db.WrapPool(pgPool)

	// 开源版启动时自动执行未应用的 migration（幂等，不会重复执行已应用的）。
	// SaaS 部署由 deploy.sh 显式控制迁移时机，此处跳过。
	if cfg.DeploymentMode == "open" {
		ms, err := migratefs.LoadMigrations("backend/migrations")
		if err != nil {
			return fmt.Errorf("load migrations: %w", err)
		}
		if err := migration.Run(ctx, pgPool, ms); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}
	}

	bgPgPool, err := db.NewBackgroundPool(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect background database: %w", err)
	}
	defer bgPgPool.Close()

	bgPool := db.WrapPool(bgPgPool)

	rdb, err := redis.NewClient(cfg.RedisAddr)
	if err != nil {
		return fmt.Errorf("connect redis: %w", err)
	}
	defer rdb.Close() //nolint:errcheck

	sessions := mw.NewSessionManager(rdb)

	// 开源版启动时确保 OPEN_API_KEY 已写入 api_keys 表，供 Worker 路由鉴权使用。
	if cfg.DeploymentMode == "open" {
		if err := bootstrap.SeedOpenBackend(ctx, cfg, pool, sessions); err != nil {
			return fmt.Errorf("seed open backend: %w", err)
		}
	}

	sysCfg, err := config.BuildSysConfig(cfg)
	if err != nil {
		return fmt.Errorf("load sys config: %w", err)
	}

	// 开源版默认图片由 LoadSysConfig（sysconfig.go open 分支）基于 API_HOST 构造绝对 URL：
	// 轨迹图标会传给腾讯静态地图（icon: 参数），相对路径外部服务无法抓取，
	// 此处不再覆盖为相对路径。

	lock := db.NewAdvisoryLock(pgPool)
	sysCfgLoader := config.NewSysConfigLoader(cfg)

	vipService := vip.NewService(pool)

	aiService := ai.NewService(pool, rdb, vipService, sysCfgLoader, cfg)
	wxMPClient := wxmp.NewClient(cfg, rdb)
	familyService := family.NewService(pool, rdb, lock, sysCfg.DefaultAvatarURL)
	storage := file.NewStorage(cfg.StorageLocalPath).WithBaseURL(sysCfg.FileBaseURL)
	if cfg.OSSConfigured() {
		ossClient, err := oss.New(cfg.OSSEndpoint, cfg.OSSAccessKeyID, cfg.OSSAccessKeySecret)
		if err != nil {
			return fmt.Errorf("init oss client: %w", err)
		}
		ossClient.HTTPClient = config.HTTPClient()
		ossBucket, err := ossClient.Bucket(cfg.OSSBucket)
		if err != nil {
			return fmt.Errorf("init oss bucket: %w", err)
		}
		storage.WithOSS(file.NewOSSStore(ossBucket, cfg.OSSPublicURLBase()))
	}

	// ADR-0013：已删对象边缘缓存批量收敛。默认关闭；开启后 OSS 对象删除成功时把
	// 公开 URL 记入 Redis 队列，由后台任务定期分批调用阿里云 CDN 刷新接口。
	var purgeQueue *purge.Queue
	var cdnPurger *purge.Purger
	if cfg.CDNRefreshEnabled && rdb != nil && storage.OSSConfigured() {
		purgeQueue = purge.NewQueue(rdb)
		cdnPurger = purge.NewPurger(cfg.OSSAccessKeyID, cfg.OSSAccessKeySecret)
		storage.WithOSSDeleteHook(func(objectURL string) {
			hookCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := purgeQueue.Record(hookCtx, objectURL); err != nil {
				slog.Warn("record deleted url for purge failed", slog.Any("error", err))
			}
		})
	}
	avatarService := avatar.NewService(pool, storage)
	diaryService := diary.NewService(pool, rdb, lock, storage, sysCfg, cfg.TencentMapKeys)
	pushService := push.NewService(pool, cfg, rdb, wxMPClient, vipService)
	diaryService.SetNewPlaceAlerter(pushService)
	autoRecordService := autorecord.NewService(pool, rdb, cfg, diaryService, pushService)
	bgAutoRecordService := autorecord.NewService(bgPool, rdb, cfg, diaryService, pushService)
	jobRunner := jobs.NewRunner(pool, bgPool, bgAutoRecordService, storage, pushService, cfg, purgeQueue, cdnPurger, rdb)
	jobRunner.Start(ctx)

	httpRouter := chi.NewRouter()
	httpRouter.Use(middleware.RequestID)
	httpRouter.Use(mw.RecoveryMiddleware(logger))
	httpRouter.Use(mw.LoggerMiddleware(logger))

	healthLimiter := mw.NewIPRateLimiter(30, time.Minute, cfg.TrustedProxyCIDR)
	httpRouter.With(healthLimiter.Handler).Get("/health/live", newLivenessHandler(&shuttingDown))
	httpRouter.With(healthLimiter.Handler).Get("/health/ready", newHealthHandler(pool, rdb, &shuttingDown))
	httpRouter.With(healthLimiter.Handler).Get("/health", newHealthHandler(pool, rdb, &shuttingDown))

	apiRouter := chi.NewRouter()
	// Cloudflare Worker 共享密钥校验。未配置时直接放行；外部回调与 MCP 内部路由跳过。
	apiRouter.Use(mw.WorkerAuth(cfg.WorkerSecret, "/api/prod/payment/virtualPayNotify", "/wx/callback", "/internal/mcp"))

	publicLimiter := mw.NewIPRateLimiter(60, time.Minute, cfg.TrustedProxyCIDR)
	publicRouter := chi.NewRouter()
	publicRouter.Use(publicLimiter.Handler)
	// 公开路由（登录、支付/公众号回调、system/config）也输出访问日志，供 P95 统计。
	publicRouter.Use(mw.AccessLogMiddleware(logger))

	mcpHandler := mcp.NewHandler(pool, cfg, rdb)

	authHandler := auth.NewHandlerWithBackgroundPool(pool, bgPool, rdb, cfg, sessions, vipService, familyService, avatarService, storage, sysCfg.DefaultAvatarURL)
	authHandler.RegisterPublic(publicRouter)

	paymentHandler := payment.NewHandler(apiRouter, pool, cfg, vipService)
	paymentHandler.RegisterPublic(publicRouter)

	inviteHandler := invite.NewHandler(pool, rdb, authHandler.GetWechatClient(), storage, cfg)
	inviteHandler.RegisterPublic(publicRouter)

	wxmpHandler := wxmp.NewHandler(apiRouter, pool, rdb, cfg, sysCfgLoader, wxMPClient, aiService)
	wxmpHandler.RegisterPublic(publicRouter)

	system.NewHandler(publicRouter, cfg)

	apiRouter.Mount("/", publicRouter)

	// MCP 客户端流量唯一入口为 Cloudflare Worker：源站仅暴露 /internal/mcp/*
	// （X-Worker-Secret 保护），不再注册公开 MCP 协议端点。
	// 开源版直接暴露 /mcp/*，不依赖 Worker，也不注册内部端点以减少攻击面。
	if cfg.DeploymentMode == "open" {
		mcpHandler.RegisterPublic(apiRouter)
	} else {
		mcpHandler.RegisterInternal(apiRouter)
	}

	apiRouter.Group(func(r chi.Router) {
		if cfg.DeploymentMode == "open" {
			r.Use(mw.NewOpenAuthMiddleware(sessions, pool, cfg.OpenAPIKey).Handler)
		} else {
			r.Use(mw.NewSessionMiddleware(sessions).Handler)
		}
		// 鉴权后输出带 user_id 的访问日志，用于数据问题复盘（OPS-LOG）。
		r.Use(mw.AccessLogMiddleware(logger))

		authHandler.RegisterProtected(r)
		// 账号注销单独注册，加 IP 限流 5 次/小时（绕过 session 认证后仍有必要防护）
		accountDeleteLimiter := mw.NewIPRateLimiter(5, time.Hour, cfg.TrustedProxyCIDR)
		r.With(accountDeleteLimiter.Handler).Delete("/auth/account", authHandler.DeleteAccount)
		// A-FIX-04：手机号绑定每次都会调用微信 GetPhoneNumber（失败也计费/耗额度），
		// 单独加 IP 限流 10 次/分钟，避免高频消耗微信额度（日限只拦成功绑定）。
		bindPhoneLimiter := mw.NewIPRateLimiter(10, time.Minute, cfg.TrustedProxyCIDR)
		r.With(bindPhoneLimiter.Handler).Post("/auth/phone/bind", authHandler.BindPhone)

		userHandler := user.NewHandlerWithBackgroundPool(r, pool, bgPool, vipService, avatarService, storage, sysCfg.DefaultAvatarURL)
		userHandler.Register()

		familyHandler := family.NewHandler(r, pool, rdb, lock, sysCfg.DefaultAvatarURL, vipService)
		familyHandler.Register()

		fileHandler := file.NewHandler(r, pool, bgPool, rdb, storage, cfg, vipService)
		fileHandler.Register()
		// FP-P2-02：上传限流（60 次/分钟/IP）。配额只按字节计，恶意用户可反复上传小图
		// 在配额内制造海量 files 行，耗尽 DB 行/inode；限流先于超时中间件执行。
		uploadLimiter := mw.NewIPRateLimiter(60, time.Minute, cfg.TrustedProxyCIDR)
		fileHandler.RegisterUpload(
			uploadLimiter.Handler,
			func(next http.Handler) http.Handler {
				// 5 分钟 < httpServer.WriteTimeout(310s)，确保超时 JSON 能返回客户端。
				return http.TimeoutHandler(next, 5*time.Minute, `{"code":"5001","message":"upload timeout"}`)
			},
		)

		vipHandler := vip.NewHandler(r, pool, vipService)
		vipHandler.Register()

		paymentHandler := payment.NewHandler(r, pool, cfg, vipService)
		paymentHandler.Register()

		autoRecordHandler := autorecord.NewHandler(r, pool, autoRecordService)
		autoRecordHandler.Register()

		pushHandler := push.NewHandler(r, pushService)
		pushHandler.Register()

		locationHandler := location.NewHandler(r, cfg, rdb)
		locationHandler.Register()

		mcpHandler.Register(r)

		inviteHandler.Register(r)

		diaryHandler := diary.NewHandler(r, pool, vipService, diaryService)
		diaryHandler.Register()

		opslogHandler := opslog.NewHandler(r, pool)
		opslogHandler.Register()
	})

	httpRouter.Mount("/", apiRouter)

	sseRouter := chi.NewRouter()
	sseRouter.Use(middleware.RequestID)
	sseRouter.Use(mw.RecoveryMiddleware(logger))
	sseRouter.Use(mw.LoggerMiddleware(logger))
	sseRouter.With(healthLimiter.Handler).Get("/health/live", newLivenessHandler(&shuttingDown))
	sseRouter.With(healthLimiter.Handler).Get("/health/ready", newHealthHandler(pool, rdb, &shuttingDown))
	sseRouter.With(healthLimiter.Handler).Get("/health", newHealthHandler(pool, rdb, &shuttingDown))

	// AI chat 成本最高的端点，加应用层 IP 限流作为防御纵深（主防护为日配额制）
	aiChatLimiter := mw.NewIPRateLimiter(30, time.Minute, cfg.TrustedProxyCIDR)
	sseRouter.Group(func(r chi.Router) {
		// /health 直接暴露给负载均衡，WorkerAuth 仅加在 SSE 业务路由上。
		r.Use(mw.WorkerAuth(cfg.WorkerSecret))
		if cfg.DeploymentMode == "open" {
			r.Use(mw.NewOpenAuthMiddleware(sessions, pool, cfg.OpenAPIKey).Handler)
		} else {
			r.Use(mw.NewSessionMiddleware(sessions).Handler)
		}
		// 鉴权后输出带 user_id 的访问日志，用于数据问题复盘（OPS-LOG）。
		r.Use(mw.AccessLogMiddleware(logger))
		r.Use(aiChatLimiter.Handler)
		aiHandler := ai.NewHandler(r, aiService)
		aiHandler.Register()
	})

	httpAddr := cfg.HTTPBind + ":" + cfg.HTTPPort
	sseAddr := cfg.SSEBind + ":" + cfg.SSEPort

	httpServer := &http.Server{
		Addr:    httpAddr,
		Handler: httpRouter,
		BaseContext: func(_ net.Listener) context.Context {
			return baseCtx
		},

		ReadTimeout:       10 * time.Minute,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      310 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	sseServer := &http.Server{
		Addr:    sseAddr,
		Handler: sseRouter,
		BaseContext: func(_ net.Listener) context.Context {
			return baseCtx
		},

		ReadHeaderTimeout: 10 * time.Second,
		// 注意：ReadTimeout 必须保持 0——Go http.Server 在请求体读完后启动后台读，
		// 读 deadline 到期会关闭连接，中断进行中的 SSE 流（AI 对话 >30s 即被掐断）。
		// 慢发 body 的防护由 ReadHeaderTimeout 与上层限流承担。
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	srvErr := make(chan error, 2)

	safe.Go(ctx, nil, func() {
		defer wg.Done()
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	})

	safe.Go(ctx, nil, func() {
		defer wg.Done()
		if err := sseServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	})

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	var startErr error
	select {
	case <-quit:
	case startErr = <-srvErr:
		logger.Error("server failed to start", slog.Any("error", startErr))
	}

	shuttingDown.Store(true)
	baseCancel()

	jobRunner.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = httpServer.Shutdown(shutdownCtx) //nolint:errcheck
	_ = sseServer.Shutdown(shutdownCtx)  //nolint:errcheck

	healthLimiter.Stop()
	publicLimiter.Stop()
	inviteHandler.Stop()
	mcpHandler.Stop()
	limiter.StopGeoCoder()
	limiter.StopStaticMap()

	wg.Wait()

	return startErr
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "DEBUG":
		lv = slog.LevelDebug
	case "WARN":
		lv = slog.LevelWarn
	case "ERROR":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}

	// LOG_FILE 非空时把结构化日志同时落盘到该文件（compose 挂载的持久化目录），
	// stdout 保留以便 docker logs / 现有告警扫描继续可用；容器重建后宿主机日志不丢，
	// 便于多天排障与日志监控。未配置（如本地开发）时仅输出 stdout。
	var w io.Writer = os.Stdout
	if path := strings.TrimSpace(os.Getenv("LOG_FILE")); path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "log file dir unavailable, fallback to stdout: path=%s err=%v\n", path, err)
		} else if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "log file unavailable, fallback to stdout: path=%s err=%v\n", path, err)
		} else {
			w = io.MultiWriter(os.Stdout, f)
		}
	}

	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv})
	return slog.New(handler)
}

func newHealthHandler(pool *db.Pool, rdb *goredis.Client, shutdownFlag *atomic.Bool) http.HandlerFunc {
	// R4：Redis 降级节流告警——外部轮询 /health 返回体不是可靠的告警通道，
	// 由进程自己按每 5 分钟一条 ERROR 日志（alert=redis_down）接入现有日志监控。
	var lastRedisAlertAt atomic.Int64
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		status := map[string]string{
			"status": "ok",
		}
		code := http.StatusOK

		if shutdownFlag.Load() {
			status["status"] = "shutting_down"
			code = http.StatusServiceUnavailable
		}

		if err := pool.Pool().Ping(ctx); err != nil {
			status["status"] = "error"
			code = http.StatusServiceUnavailable
		}

		// R3：Redis 故障不再判死容器。AI 链路对 Redis 抖动已 fail-open，
		// 若健康检查仍 503 会被 watchdog 反复重启，雪上加霜。
		// 降级为 200 + "degraded"，由日志/监控告警兜底；数据库故障仍判死。
		if err := rdb.Ping(ctx).Err(); err != nil {
			status["redis"] = "down"
			if status["status"] == "ok" {
				status["status"] = "degraded"
			}
			now := time.Now().Unix()
			if now-lastRedisAlertAt.Load() >= 300 {
				lastRedisAlertAt.Store(now)
				slog.ErrorContext(ctx, "redis health degraded",
					slog.String("alert", "redis_down"),
					slog.Any("error", err))
			}
		} else {
			status["redis"] = "up"
			lastRedisAlertAt.Store(0)
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(status) //nolint:errcheck
	}
}

// newLivenessHandler 仅检查进程存活与关闭状态，不依赖 DB/Redis。
func newLivenessHandler(shutdownFlag *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := map[string]string{"status": "ok"}
		code := http.StatusOK
		if shutdownFlag.Load() {
			status["status"] = "shutting_down"
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(status) //nolint:errcheck
	}
}
