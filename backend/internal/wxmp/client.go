package wxmp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"mime/multipart"
	"net/http"
	"net/url"
	"time"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/pkg/util"

	goredis "github.com/redis/go-redis/v9"
)

const (
	accessTokenCacheKey = "wechat:mp:access_token:%s"
	accessTokenCacheTTL = 110 * time.Minute
)

// UserInfo holds selected fields from WeChat /cgi-bin/user/info.
type UserInfo struct {
	OpenID        string
	UnionID       string
	Nickname      string
	Avatar        string
	Subscribe     bool
	SubscribeTime int64
}

// Client is a WeChat official account (service account) API client.
type Client struct {
	appID      string
	secret     string
	httpClient *http.Client
	rdb        *goredis.Client
}

// NewClient creates a new service account client.
func NewClient(cfg *config.Config, rdb *goredis.Client) *Client {
	return &Client{
		appID:      cfg.WechatMPAppID,
		secret:     cfg.WechatMPSecret,
		httpClient: config.HTTPClient(),
		rdb:        rdb,
	}
}

// IsConfigured returns whether the client has credentials configured.
func (c *Client) IsConfigured() bool {
	return c.appID != "" && c.secret != ""
}

func (c *Client) GetAccessToken(ctx context.Context) (string, error) {
	if !c.IsConfigured() {
		return "", fmt.Errorf("wechat mp appid or secret not configured")
	}

	cacheKey := fmt.Sprintf(accessTokenCacheKey, c.appID)
	if c.rdb != nil {
		cached, err := c.rdb.Get(ctx, cacheKey).Result()
		if err == nil && cached != "" {
			return cached, nil
		}
		if err != nil && err != goredis.Nil {
			// 缓存读失败（非 miss）降级回源微信，但记 warn 与 getThumbMediaID 同型，避免排障缺一环。
			slog.WarnContext(ctx, "read wechat mp access token cache failed", slog.Any("error", err))
		}
	}

	// Use the stable access token endpoint to avoid token invalidation races.
	u := "https://api.weixin.qq.com/cgi-bin/stable_token"
	payload, err := json.Marshal(map[string]string{
		"grant_type": "client_credential",
		"appid":      c.appID,
		"secret":     c.secret,
	})
	if err != nil {
		return "", fmt.Errorf("marshal mp access token request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build mp access token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", util.SanitizeURLError(fmt.Errorf("request mp access token: %w", err))
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return "", fmt.Errorf("read mp access token response: %w", err)
	}

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse mp access token response: %w", err)
	}

	if result.ErrCode != 0 {
		return "", fmt.Errorf("wechat mp access token error: code=%d msg=%s", result.ErrCode, result.ErrMsg)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("empty mp access token")
	}

	if c.rdb != nil {
		_ = c.rdb.Set(ctx, cacheKey, result.AccessToken, accessTokenCacheTTL).Err() //nolint:errcheck // cache write is best-effort
	}

	return result.AccessToken, nil
}

// FetchUserInfo calls /cgi-bin/user/info to obtain unionid and profile.
func (c *Client) FetchUserInfo(ctx context.Context, openID string) (*UserInfo, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("wechat mp appid or secret not configured")
	}
	if openID == "" {
		return nil, fmt.Errorf("openid is empty")
	}

	token, err := c.GetAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	u := fmt.Sprintf(
		"https://api.weixin.qq.com/cgi-bin/user/info?access_token=%s&openid=%s&lang=zh_CN",
		url.QueryEscape(token),
		url.QueryEscape(openID),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build mp user info request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, util.SanitizeURLError(fmt.Errorf("request mp user info: %w", err))
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return nil, fmt.Errorf("read mp user info response: %w", err)
	}

	var result struct {
		OpenID        string `json:"openid"`
		UnionID       string `json:"unionid"`
		Nickname      string `json:"nickname"`
		Avatar        string `json:"headimgurl"`
		Subscribe     int    `json:"subscribe"`
		SubscribeTime int64  `json:"subscribe_time"`
		ErrCode       int    `json:"errcode"`
		ErrMsg        string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse mp user info response: %w", err)
	}

	if result.ErrCode != 0 {
		return nil, fmt.Errorf("wechat mp user info error: code=%d msg=%s", result.ErrCode, result.ErrMsg)
	}

	return &UserInfo{
		OpenID:        result.OpenID,
		UnionID:       result.UnionID,
		Nickname:      result.Nickname,
		Avatar:        result.Avatar,
		Subscribe:     result.Subscribe == 1,
		SubscribeTime: result.SubscribeTime,
	}, nil
}

// UploadTempMedia uploads a temporary media file to WeChat.
// mediaType can be "image", "voice", "video" or "thumb".
func (c *Client) UploadTempMedia(ctx context.Context, mediaType, filename string, data []byte) (string, error) {
	if !c.IsConfigured() {
		return "", fmt.Errorf("wechat mp appid or secret not configured")
	}
	if mediaType == "" {
		return "", fmt.Errorf("media type is empty")
	}
	if len(data) == 0 {
		return "", fmt.Errorf("media data is empty")
	}

	token, err := c.GetAccessToken(ctx)
	if err != nil {
		return "", err
	}

	u := fmt.Sprintf("https://api.weixin.qq.com/cgi-bin/media/upload?access_token=%s&type=%s", url.QueryEscape(token), url.QueryEscape(mediaType))

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("media", filename)
	if err != nil {
		return "", fmt.Errorf("create media form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("write media data: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return "", fmt.Errorf("build upload media request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", util.SanitizeURLError(fmt.Errorf("request upload media: %w", err))
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return "", fmt.Errorf("read upload media response: %w", err)
	}

	var result struct {
		MediaID      string `json:"media_id"`
		ThumbMediaID string `json:"thumb_media_id"`
		ErrCode      int    `json:"errcode"`
		ErrMsg       string `json:"errmsg"`
		CreatedAt    int64  `json:"created_at"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse upload media response: %w", err)
	}
	if result.ErrCode != 0 {
		return "", fmt.Errorf("wechat upload media error: code=%d msg=%s", result.ErrCode, result.ErrMsg)
	}
	if mediaType == "thumb" {
		if result.ThumbMediaID == "" {
			return "", fmt.Errorf("empty thumb media id from upload response")
		}
		return result.ThumbMediaID, nil
	}
	if result.MediaID == "" {
		return "", fmt.Errorf("empty media id from upload response")
	}

	return result.MediaID, nil
}

// SendMiniProgramPage sends a customer service mini-program page card.
func (c *Client) SendMiniProgramPage(ctx context.Context, openID, appID, pagePath, title, thumbMediaID string) error {
	if !c.IsConfigured() {
		return fmt.Errorf("wechat mp appid or secret not configured")
	}
	if openID == "" {
		return fmt.Errorf("openid is empty")
	}
	if appID == "" {
		return fmt.Errorf("appid is empty")
	}
	if thumbMediaID == "" {
		return fmt.Errorf("thumb media id is empty")
	}
	if pagePath == "" {
		pagePath = "pages/index/index"
	}
	if title == "" {
		title = "打开小程序"
	}

	token, err := c.GetAccessToken(ctx)
	if err != nil {
		return err
	}

	u := fmt.Sprintf("https://api.weixin.qq.com/cgi-bin/message/custom/send?access_token=%s", url.QueryEscape(token))

	payload, err := json.Marshal(map[string]interface{}{
		"touser":  openID,
		"msgtype": "miniprogrampage",
		"miniprogrampage": map[string]string{
			"title":          title,
			"appid":          appID,
			"pagepath":       pagePath,
			"thumb_media_id": thumbMediaID,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal mini program page request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build mini program page request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return util.SanitizeURLError(fmt.Errorf("request mini program page: %w", err))
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return fmt.Errorf("read mini program page response: %w", err)
	}

	var result struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse mini program page response: %w", err)
	}
	if result.ErrCode != 0 {
		return fmt.Errorf("wechat mini program page error: code=%d msg=%s", result.ErrCode, result.ErrMsg)
	}

	return nil
}

// SendKfMessage sends a customer service text message.
func (c *Client) SendKfMessage(ctx context.Context, openID, content string) error {
	if !c.IsConfigured() {
		return fmt.Errorf("wechat mp appid or secret not configured")
	}
	if openID == "" {
		return fmt.Errorf("openid is empty")
	}

	token, err := c.GetAccessToken(ctx)
	if err != nil {
		return err
	}

	u := fmt.Sprintf("https://api.weixin.qq.com/cgi-bin/message/custom/send?access_token=%s", url.QueryEscape(token))

	payload, err := json.Marshal(map[string]interface{}{
		"touser":  openID,
		"msgtype": "text",
		"text": map[string]string{
			"content": content,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal kf message request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build kf message request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return util.SanitizeURLError(fmt.Errorf("request kf message: %w", err))
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return fmt.Errorf("read kf message response: %w", err)
	}

	var result struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse kf message response: %w", err)
	}
	if result.ErrCode != 0 {
		return fmt.Errorf("wechat kf message error: code=%d msg=%s", result.ErrCode, result.ErrMsg)
	}

	return nil
}
