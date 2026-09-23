// Package config loads and exposes application configuration.
package config

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"papafeiji/backend/internal/wechatsecrets"
)

var (
	sharedHTTPClient     *http.Client
	sharedHTTPClientOnce sync.Once
	sharedTransport      *http.Transport
	sharedTransportOnce  sync.Once
)

func sharedHTTPClientTransport() *http.Transport {
	sharedTransportOnce.Do(func() {
		sharedTransport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	})
	return sharedTransport
}

type Config struct {
	DatabaseURL                string
	RedisAddr                  string
	WechatAppID                string
	WechatSecret               string
	WechatMPAppID              string
	WechatMPSecret             string
	WechatMPGhID               string
	WechatMiniLinkEnvVersion   string
	WechatVirtualOfferID       string
	WechatVirtualAppKeyProd    string
	WechatVirtualAppKeySandbox string
	WechatMsgToken             string
	WechatEncodingAESKey       string

	// WechatVirtualCallbackToken 微信虚拟支付发货回调的验签 Token（安全模式），
	// 与公众号消息 Token 相互独立，避免两类回调配置冲突。
	WechatVirtualCallbackToken string
	// WechatVirtualCallbackAESKey 虚拟支付回调安全模式的 EncodingAESKey（43 位）。
	WechatVirtualCallbackAESKey string
	// PaymentAllowSandbox 是否允许客户端指定 env=1 走沙箱虚拟支付（仅联调服务器开启，生产默认关闭）。
	PaymentAllowSandbox bool
	TencentMapKeys      []string
	AIAPIKey            string

	OSSAccessKeyID     string
	OSSAccessKeySecret string
	OSSEndpoint        string
	OSSBucket          string
	OSSPublicURL       string

	HTTPBind string
	HTTPPort string
	SSEBind  string
	SSEPort  string
	APIHost  string
	LogLevel string

	// DeploymentMode 区分 SaaS 与开源版："saas" | "open"。
	// 影响 /system/config 返回的功能开关。
	DeploymentMode string

	// AI 运行时配置（原 sys_configs.ai_config）。
	AIBaseURL      string
	AIModel        string
	AIThinkingType string
	AIMaxTokens    int
	// AIPrompt 小程序系统提示词（原 sys_configs.ai_prompt）；默认 DefaultAIPrompt。
	AIPrompt string
	// WechatMPPrompt 公众号系统提示词（原 sys_configs.sys_config.wechatMpPrompt）；空回退 AIPrompt。
	WechatMPPrompt string
	// 默认静态资源（原 sys_configs.sys_config）。
	DefaultCoverImage     string
	DefaultTrajectoryIcon string
	DefaultAvatarURL      string
	// StoragePublicBaseURL 文件对外基址（原 sys_configs.sys_config.fileBaseUrl）。
	StoragePublicBaseURL string

	TrustedProxyCIDR string

	// WorkerSecret 是 Cloudflare Worker 中转 API 请求的共享密钥。
	// 可选：未配置时 WorkerAuth 中间件直接放行，兼容直接访问与开发环境。
	WorkerSecret string

	// MCPWorkerSecret 是 Cloudflare Worker 调用 /internal/mcp/rpc 的共享密钥。
	// saas 模式必填（缺省启动校验失败）；open 模式不注册内部端点，留空。
	MCPWorkerSecret string

	// MCPPublicURL 是 MCP 客户端应使用的公开入口（Cloudflare Worker 地址）。
	// 可选：未配置时回退为源站地址（APIHost）。
	MCPPublicURL string

	// MCPEnabled 是否对外开启 MCP 功能（/system/config features.mcp，B6a-15）。
	// 默认开启，可用 MCP_ENABLED=0 显式关闭。
	MCPEnabled bool

	// FreeVipEnabled 控制 /system/config 的 features.freeVip（小程序 VIP 页免费领取入口）。
	// 默认开启；免费活动下线用 FREE_VIP_ENABLED=0，无需发版（VP-13）。
	FreeVipEnabled bool

	// OpenAPIKey 是开源版与 Cloudflare Worker 之间的共享密钥。
	// Worker 转发请求时通过 X-Private-Api-Key 头部携带，开源版据此识别合法请求。
	OpenAPIKey string

	// StorageLocalPath 是开源版本地文件存储根目录。
	StorageLocalPath string

	// UserImageStorageLimitBytes 普通用户图片存储上限（字节），0 表示不限制。
	UserImageStorageLimitBytes int64
	// UserImageStorageLimitBytesVIP VIP 用户图片存储上限（字节），0 表示不限制。
	UserImageStorageLimitBytesVIP int64

	JobIntervalAutoRecord            time.Duration
	JobIntervalAbnormalAlert         time.Duration
	JobIntervalOrderClose            time.Duration
	JobIntervalCleanupAILogs         time.Duration
	JobIntervalCleanupTrajectories   time.Duration
	JobIntervalCleanupOrphanFiles    time.Duration
	JobIntervalCleanupOrphanTrajMaps time.Duration
	JobIntervalCleanupClientOpsLogs  time.Duration
	JobIntervalPurgeDeletedObjects   time.Duration

	// CDNRefreshEnabled 开启已删对象边缘缓存批量收敛（ADR-0013）：删除 OSS 对象时
	// 记录公开 URL，后台任务定期调用阿里云 CDN 刷新接口。默认关闭。
	CDNRefreshEnabled bool
}

func Load() (*Config, error) {
	jobIntervalAutoRecord, err := defaultDurationEnv("JOB_INTERVAL_AUTO_RECORD", 5*time.Minute)
	if err != nil {
		return nil, err
	}
	jobIntervalAbnormalAlert, err := defaultDurationEnv("JOB_INTERVAL_ABNORMAL_ALERT", 5*time.Minute)
	if err != nil {
		return nil, err
	}
	jobIntervalOrderClose, err := defaultDurationEnv("JOB_INTERVAL_ORDER_CLOSE", time.Minute)
	if err != nil {
		return nil, err
	}
	jobIntervalCleanupAILogs, err := defaultDurationEnv("JOB_INTERVAL_CLEANUP_AI_LOGS", 24*time.Hour)
	if err != nil {
		return nil, err
	}
	jobIntervalCleanupTrajectories, err := defaultDurationEnv("JOB_INTERVAL_CLEANUP_TRAJECTORIES", 6*time.Hour)
	if err != nil {
		return nil, err
	}
	jobIntervalCleanupOrphanFiles, err := defaultDurationEnv("JOB_INTERVAL_CLEANUP_ORPHAN_FILES", 7*24*time.Hour)
	if err != nil {
		return nil, err
	}
	jobIntervalCleanupOrphanTrajMaps, err := defaultDurationEnv("JOB_INTERVAL_CLEANUP_ORPHAN_TRAJ_MAPS", 7*24*time.Hour)
	if err != nil {
		return nil, err
	}
	jobIntervalCleanupClientOpsLogs, err := defaultDurationEnv("JOB_INTERVAL_CLEANUP_CLIENT_OPS_LOGS", 24*time.Hour)
	if err != nil {
		return nil, err
	}
	jobIntervalPurgeDeletedObjects, err := defaultDurationEnv("JOB_INTERVAL_PURGE_DELETED_OBJECTS", 24*time.Hour)
	if err != nil {
		return nil, err
	}

	userImageStorageLimitBytes, err := defaultInt64Env("USER_IMAGE_STORAGE_LIMIT_BYTES", 1024*1024*1024)
	if err != nil {
		return nil, err
	}
	userImageStorageLimitBytesVIP, err := defaultInt64Env("USER_IMAGE_STORAGE_LIMIT_BYTES_VIP", 5*1024*1024*1024)
	if err != nil {
		return nil, err
	}

	aiMaxTokens, err := defaultInt64Env("AI_MAX_OUTPUT_TOKENS", 0)
	if err != nil {
		return nil, err
	}

	// 成对回退（CF001a）：微信 AppID/Secret 仅当两者同时为空时才双双回退到 wechatsecrets 内置值，
	// 避免半配置（只设其一）时产生 AppID 来自环境、Secret 来自内置的混合配对。
	wechatAppID := os.Getenv("WECHAT_APPID")
	wechatSecret := os.Getenv("WECHAT_SECRET")
	if wechatAppID == "" && wechatSecret == "" {
		wechatAppID = wechatsecrets.DefaultAppID()
		wechatSecret = wechatsecrets.DefaultSecret()
	}

	cfg := &Config{
		DatabaseURL:                 os.Getenv("DATABASE_URL"),
		RedisAddr:                   os.Getenv("REDIS_ADDR"),
		WechatAppID:                 wechatAppID,
		WechatSecret:                wechatSecret,
		WechatMPAppID:               os.Getenv("WECHAT_MP_APPID"),
		WechatMPSecret:              os.Getenv("WECHAT_MP_SECRET"),
		WechatMPGhID:                os.Getenv("WECHAT_MP_GHID"),
		WechatMiniLinkEnvVersion:    defaultEnv("WECHAT_MINI_LINK_ENV_VERSION", "release"),
		WechatVirtualOfferID:        os.Getenv("WECHAT_VIRTUAL_OFFER_ID"),
		WechatVirtualAppKeyProd:     os.Getenv("WECHAT_VIRTUAL_APP_KEY_PRODUCTION"),
		WechatVirtualAppKeySandbox:  os.Getenv("WECHAT_VIRTUAL_APP_KEY_SANDBOX"),
		WechatMsgToken:              os.Getenv("WECHAT_MSG_TOKEN"),
		WechatEncodingAESKey:        os.Getenv("WECHAT_ENCODING_AES_KEY"),
		WechatVirtualCallbackToken:  os.Getenv("WECHAT_VIRTUAL_CALLBACK_TOKEN"),
		WechatVirtualCallbackAESKey: os.Getenv("WECHAT_VIRTUAL_CALLBACK_AES_KEY"),
		PaymentAllowSandbox:         os.Getenv("PAYMENT_ALLOW_SANDBOX") == "1",

		TencentMapKeys: parseKeys(os.Getenv("TENCENT_MAP_KEY")),
		AIAPIKey:       os.Getenv("AI_API_KEY"),

		OSSAccessKeyID:     os.Getenv("OSS_ACCESS_KEY_ID"),
		OSSAccessKeySecret: os.Getenv("OSS_ACCESS_KEY_SECRET"),
		OSSEndpoint:        os.Getenv("OSS_ENDPOINT"),
		OSSBucket:          os.Getenv("OSS_BUCKET"),
		OSSPublicURL:       os.Getenv("OSS_PUBLIC_URL"),

		HTTPBind:              defaultEnv("HTTP_BIND", "127.0.0.1"),
		HTTPPort:              defaultEnv("HTTP_PORT", "8080"),
		SSEBind:               defaultEnv("SSE_BIND", "127.0.0.1"),
		SSEPort:               defaultEnv("SSE_PORT", "8081"),
		APIHost:               os.Getenv("API_HOST"),
		LogLevel:              defaultEnv("LOG_LEVEL", "INFO"),
		DeploymentMode:        defaultEnv("DEPLOYMENT_MODE", "saas"),
		AIBaseURL:             defaultEnv("AI_BASE_URL", ""),
		AIModel:               defaultEnv("AI_MODEL", ""),
		AIThinkingType:        defaultEnv("AI_THINKING_TYPE", ""),
		AIMaxTokens:           int(aiMaxTokens),
		AIPrompt:              defaultEnv("AI_PROMPT", DefaultAIPrompt),
		WechatMPPrompt:        defaultEnv("WECHAT_MP_PROMPT", ""),
		DefaultCoverImage:     os.Getenv("DEFAULT_COVER_IMAGE"),
		DefaultTrajectoryIcon: os.Getenv("DEFAULT_TRAJECTORY_ICON"),
		DefaultAvatarURL:      defaultEnv("DEFAULT_AVATAR_URL", "https://api.dicebear.com/7.x/bottts-neutral/png?seed="),
		StoragePublicBaseURL:  os.Getenv("STORAGE_PUBLIC_BASE_URL"),
		TrustedProxyCIDR:      os.Getenv("TRUSTED_PROXY_CIDR"),
		WorkerSecret:          os.Getenv("WORKER_SECRET"),
		MCPWorkerSecret:       os.Getenv("MCP_WORKER_SECRET"),
		MCPPublicURL:          os.Getenv("MCP_PUBLIC_URL"),
		MCPEnabled:            boolEnv("MCP_ENABLED", true),
		FreeVipEnabled:        boolEnv("FREE_VIP_ENABLED", true),
		OpenAPIKey:            os.Getenv("OPEN_API_KEY"),
		StorageLocalPath:      defaultEnv("STORAGE_LOCAL_PATH", "/opt/pathmemos/uploads"),

		UserImageStorageLimitBytes:    userImageStorageLimitBytes,
		UserImageStorageLimitBytesVIP: userImageStorageLimitBytesVIP,

		JobIntervalAutoRecord:            jobIntervalAutoRecord,
		JobIntervalAbnormalAlert:         jobIntervalAbnormalAlert,
		JobIntervalOrderClose:            jobIntervalOrderClose,
		JobIntervalCleanupAILogs:         jobIntervalCleanupAILogs,
		JobIntervalCleanupTrajectories:   jobIntervalCleanupTrajectories,
		JobIntervalCleanupOrphanFiles:    jobIntervalCleanupOrphanFiles,
		JobIntervalCleanupOrphanTrajMaps: jobIntervalCleanupOrphanTrajMaps,
		JobIntervalCleanupClientOpsLogs:  jobIntervalCleanupClientOpsLogs,
		JobIntervalPurgeDeletedObjects:   jobIntervalPurgeDeletedObjects,
		CDNRefreshEnabled:                boolEnv("CDN_REFRESH_ENABLED", false),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func defaultInt64Env(key string, defaultValue int64) (int64, error) {
	s := os.Getenv(key)
	if s == "" {
		return defaultValue, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("%s must be non-negative", key)
	}
	return v, nil
}

func parseKeys(s string) []string {
	var out []string
	for _, k := range strings.Split(s, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

func (c *Config) OSSConfigured() bool {
	return c.OSSAccessKeyID != "" && c.OSSAccessKeySecret != "" && c.OSSEndpoint != "" && c.OSSBucket != ""
}

func (c *Config) OSSPublicURLBase() string {
	if c.OSSPublicURL != "" {
		return strings.TrimSuffix(c.OSSPublicURL, "/")
	}
	return fmt.Sprintf("https://%s.%s", c.OSSBucket, c.OSSEndpoint)
}

func (c *Config) validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if c.RedisAddr == "" {
		return fmt.Errorf("REDIS_ADDR is required")
	}
	if c.WechatAppID == "" {
		return fmt.Errorf("WECHAT_APPID is required")
	}
	if c.WechatSecret == "" {
		return fmt.Errorf("WECHAT_SECRET is required")
	}
	if len(c.TencentMapKeys) == 0 {
		return fmt.Errorf("TENCENT_MAP_KEY is required")
	}
	if c.AIAPIKey == "" {
		return fmt.Errorf("AI_API_KEY is required")
	}
	if c.APIHost == "" {
		return fmt.Errorf("API_HOST is required")
	}

	// 开源版不强制要求微信支付、公众号回调与 OSS 配置。
	if c.DeploymentMode != "saas" && c.DeploymentMode != "open" {
		return fmt.Errorf("DEPLOYMENT_MODE must be saas or open")
	}
	isOpen := c.DeploymentMode == "open"
	if !isOpen {
		if c.WechatVirtualOfferID == "" {
			return fmt.Errorf("WECHAT_VIRTUAL_OFFER_ID is required")
		}
		if c.WechatVirtualAppKeyProd == "" {
			return fmt.Errorf("WECHAT_VIRTUAL_APP_KEY_PRODUCTION is required")
		}
		if c.WechatVirtualAppKeySandbox == "" {
			return fmt.Errorf("WECHAT_VIRTUAL_APP_KEY_SANDBOX is required")
		}
		if c.WechatMsgToken == "" {
			return fmt.Errorf("WECHAT_MSG_TOKEN is required")
		}
		// 虚拟支付发货回调已切换安全模式（fail-closed 验签 + 加密），两个密钥缺一不可。
		if c.WechatVirtualCallbackToken == "" {
			return fmt.Errorf("WECHAT_VIRTUAL_CALLBACK_TOKEN is required (虚拟支付回调验签 Token)")
		}
		if c.WechatVirtualCallbackAESKey == "" {
			return fmt.Errorf("WECHAT_VIRTUAL_CALLBACK_AES_KEY is required (虚拟支付回调加密密钥)")
		}
		if c.WorkerSecret == "" {
			return fmt.Errorf("WORKER_SECRET is required in saas mode")
		}
		// R-14：MCP Worker 共享密钥与 WORKER_SECRET 对齐，saas 缺省时启动即失败，避免运行时 500。
		if c.MCPWorkerSecret == "" {
			return fmt.Errorf("MCP_WORKER_SECRET is required in saas mode")
		}
		// SaaS 模式禁止静默回退内置微信凭据（B1-03）：必须显式配置 WECHAT_APPID/WECHAT_SECRET。
		// 开源版仍可使用 wechatsecrets 内置凭据，机制保留。
		if os.Getenv("WECHAT_APPID") == "" {
			return fmt.Errorf("WECHAT_APPID is required in saas mode (内置凭据仅限开源版)")
		}
		if os.Getenv("WECHAT_SECRET") == "" {
			return fmt.Errorf("WECHAT_SECRET is required in saas mode (内置凭据仅限开源版)")
		}
	} else if c.WorkerSecret != "" {
		return fmt.Errorf("WORKER_SECRET must be empty in open mode")
	}
	if c.TrustedProxyCIDR != "" {
		for _, cidr := range strings.Split(c.TrustedProxyCIDR, ",") {
			cidr = strings.TrimSpace(cidr)
			if cidr == "" {
				return fmt.Errorf("TRUSTED_PROXY_CIDR contains empty segment")
			}
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("TRUSTED_PROXY_CIDR is invalid: %q: %w", cidr, err)
			}
		}
	}

	if c.OSSAccessKeyID != "" || c.OSSAccessKeySecret != "" || c.OSSEndpoint != "" || c.OSSBucket != "" || c.OSSPublicURL != "" {
		if c.OSSAccessKeyID == "" {
			return fmt.Errorf("OSS_ACCESS_KEY_ID is required when OSS is configured")
		}
		if c.OSSAccessKeySecret == "" {
			return fmt.Errorf("OSS_ACCESS_KEY_SECRET is required when OSS is configured")
		}
		if c.OSSEndpoint == "" {
			return fmt.Errorf("OSS_ENDPOINT is required when OSS is configured")
		}
		if c.OSSBucket == "" {
			return fmt.Errorf("OSS_BUCKET is required when OSS is configured")
		}
	}

	return nil
}

func defaultEnv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

// boolEnv 解析布尔环境变量：接受 strconv.ParseBool 支持的全部形式（1/t/true/yes/on…），
// 空值回退默认值，非法值回退 "1" 语义（兼容历史配置）。
func boolEnv(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return v == "1"
	}
	return b
}

func defaultDurationEnv(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %w", key, err)
	}
	return d, nil
}

func HTTPClient() *http.Client {
	sharedHTTPClientOnce.Do(func() {
		sharedHTTPClient = &http.Client{
			Timeout:   30 * time.Second,
			Transport: sharedHTTPClientTransport(),
		}
	})
	return sharedHTTPClient
}

func SSEHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   5 * time.Minute,
		Transport: sharedHTTPClientTransport(),
	}
}
