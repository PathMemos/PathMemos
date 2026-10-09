package wxmp

import (
	"context"
	"encoding/xml"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"papafeiji/backend/internal/ai"
	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/pkg/safe"
	"papafeiji/backend/internal/wechatcrypto"
	"papafeiji/backend/pkg/util"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	goredis "github.com/redis/go-redis/v9"
)

const (
	wxmpReplyNoUser        = "您尚未在小程序中登录，请先打开小程序完成登录，即可在服务号使用 AI 助手。"
	wxmpReplyError         = "服务繁忙，请稍后再试。"
	wxmpReplyQuotaExceeded = "您当天的对话额度已用完，请明天再试。"
	wxmpReplyWelcome       = "你好，我是 PathMemo，你的 AI 私人助理。\n\n在小程序中授权后，我能自动为您记录生活足迹。随时向我提问即可唤醒回忆。"
	wxmpReplyUnsupported   = "暂只支持文字和语音消息，请发送文字或语音。"
	wxmpReplyVoiceEmpty    = "未能识别这段语音，请发送清晰的语音或文字。"
	wxmpReplyThinking      = "正在思考，请稍候…"

	wxmpKfTextByteLimit = 2000
	wxmpMsgIDCacheTTL   = 300 * time.Second

	wxmpMiniProgramPath = "pages/index/index"

	wxmpThumbCacheKey = "wxmp:thumb_media_id"
	wxmpThumbCacheTTL = 48 * time.Hour

	wxmpProfileRefreshKeyPrefix = "wxmp:profile:refresh:"
	wxmpProfileRefreshTTL       = 1 * time.Hour
)

// Handler handles WeChat official account callbacks.
type Handler struct {
	router       chi.Router
	pool         *db.Pool
	rdb          *goredis.Client
	cfg          *config.Config
	sysCfgLoader *config.SysConfigLoader
	wxClient     *Client
	aiService    *ai.Service
}

// NewHandler creates a new WeChat MP callback handler.
func NewHandler(router chi.Router, pool *db.Pool, rdb *goredis.Client, cfg *config.Config, sysCfgLoader *config.SysConfigLoader, wxClient *Client, aiService *ai.Service) *Handler {
	return &Handler{
		router:       router,
		pool:         pool,
		rdb:          rdb,
		cfg:          cfg,
		sysCfgLoader: sysCfgLoader,
		wxClient:     wxClient,
		aiService:    aiService,
	}
}

// RegisterPublic registers public callback endpoints (no session required).
func (h *Handler) RegisterPublic(router chi.Router) {
	router.HandleFunc("/wx/callback", h.HandleCallback)
}

// HandleCallback dispatches GET verification and POST message handling.
func (h *Handler) HandleCallback(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.verifyServer(w, r)
	case http.MethodPost:
		h.handleMessage(w, r)
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) verifyServer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	signature := q.Get("signature")
	timestamp := q.Get("timestamp")
	nonce := q.Get("nonce")
	echostr := q.Get("echostr")

	if wechatcrypto.CheckSignature(h.cfg.WechatMsgToken, signature, timestamp, nonce) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck
		_, _ = w.Write([]byte(echostr))
		return
	}
	slog.ErrorContext(ctx, "wx mp server verify failed", slog.String("signature", signature), slog.String("timestamp", timestamp), slog.String("nonce", nonce))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	//nolint:errcheck
	_, _ = w.Write([]byte("fail")) //nolint:errcheck
}

func (h *Handler) handleMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	timestamp := q.Get("timestamp")
	nonce := q.Get("nonce")

	const maxWXBodySize = 64 * 1024
	// 微信推送 body 超过 64KB 时直接返回 success，避免被重试和做无意义签名校验。
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWXBodySize+1))
	if err != nil {
		slog.ErrorContext(ctx, "read wx callback body failed", slog.Any("error", err))
		writeXMLError(w)
		return
	}
	if len(body) > maxWXBodySize {
		writeXMLError(w)
		return
	}
	xmlBody := string(body)

	encrypted := q.Get("msg_signature") != ""
	var msgXML MessageXML
	if encrypted {
		signature := q.Get("msg_signature")
		var enc EncryptedXML
		if err := xml.Unmarshal(body, &enc); err != nil {
			slog.ErrorContext(ctx, "parse wx encrypted xml failed", slog.Any("error", err))
			writeXMLError(w)
			return
		}
		// 加密模式必须校验 msg_signature，不允许回退到明文签名。
		if !wechatcrypto.CheckEncryptedSignature(h.cfg.WechatMsgToken, timestamp, nonce, enc.Encrypt, signature) {
			slog.WarnContext(ctx, "invalid wx mp encrypted signature")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("fail")) //nolint:errcheck
			return
		}
		plainXML, appID, err := wechatcrypto.DecryptMsg(enc.Encrypt, h.cfg.WechatEncodingAESKey)
		if err != nil {
			slog.ErrorContext(ctx, "decrypt wx mp message failed", slog.Any("error", err))
			writeXMLError(w)
			return
		}
		if h.cfg.WechatMPAppID != "" && appID != h.cfg.WechatMPAppID {
			slog.WarnContext(ctx, "wx mp decrypted appid mismatch", slog.String("expected", h.cfg.WechatMPAppID), slog.String("got", appID))
			writeXMLError(w)
			return
		}
		msgXML, err = ParseMessageXML(plainXML)
		if err != nil {
			slog.ErrorContext(ctx, "parse wx decrypted xml failed", slog.Any("error", err))
			writeXMLError(w)
			return
		}
	} else {
		// 明文模式收紧(与 payment 侧对齐):AES Key 已配置即强制安全模式,拒绝明文——
		// 明文签名 sha1(sort([token,timestamp,nonce])) 不绑定消息体,捕获一组合法
		// 签名三元组即可重放(伪造 FromUserName 触发 AI 对话/翻转订阅状态)。
		if h.cfg.WechatEncodingAESKey != "" {
			slog.WarnContext(ctx, "alert=wxmp_plaintext_rejected: secure mode is configured, plaintext callback not allowed")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("fail")) //nolint:errcheck
			return
		}
		// 无 AES Key 的明文部署:校验 timestamp 新鲜度(±5 分钟),压缩重放窗口。
		if ts, tsErr := strconv.ParseInt(timestamp, 10, 64); tsErr != nil || time.Now().Unix()-ts > 300 || time.Now().Unix()-ts < -300 {
			slog.WarnContext(ctx, "wx mp callback stale timestamp")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("fail")) //nolint:errcheck
			return
		}
		signature := q.Get("signature")
		if !wechatcrypto.CheckSignature(h.cfg.WechatMsgToken, signature, timestamp, nonce) {
			slog.WarnContext(ctx, "invalid wx mp callback signature")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("fail")) //nolint:errcheck
			return
		}
		var err error
		msgXML, err = ParseMessageXML(xmlBody)
		if err != nil {
			slog.ErrorContext(ctx, "parse wx callback xml failed", slog.Any("error", err))
			writeXMLError(w)
			return
		}
	}

	fromOpenID := msgXML.FromUserName
	toUserName := msgXML.ToUserName
	msgType := msgXML.MsgType
	content := msgXML.Content
	recognition := msgXML.Recognition
	event := msgXML.Event
	msgID := msgXML.MsgID

	if h.cfg.WechatMPGhID != "" && toUserName != h.cfg.WechatMPGhID {
		slog.WarnContext(ctx, "wx mp callback to_user_name mismatch", slog.String("expected", h.cfg.WechatMPGhID), slog.String("got", toUserName))
		writeXMLError(w)
		return
	}

	reply := func(text string) {
		h.writeReply(ctx, w, fromOpenID, toUserName, text, timestamp, nonce, encrypted)
	}

	if (msgType == "text" || msgType == "voice") && msgID != "" {
		duplicate, err := h.isDuplicateMessage(ctx, msgID)
		if err != nil {
			// 去重失败降级为继续处理（而非丢弃消息）：Redis 抖动不应导致用户消息被静默丢弃。
			slog.WarnContext(ctx, "check wx mp duplicate message failed, fall through to process", slog.String("msg_type", msgType), slog.String("msg_id", msgID), slog.String("openid", util.MaskID(fromOpenID)), slog.Any("error", err))
			duplicate = false
		}
		if duplicate {
			// 重复消息返回非空占位，避免微信因空内容重试导致 AI 被重复触发。
			reply(wxmpReplyThinking)
			return
		}
	}

	switch msgType {
	case "event":
		reply(h.handleEvent(ctx, fromOpenID, event))
	case "text":
		// Text reply is returned asynchronously; the immediate passive reply has already been written.
		h.handleTextMessage(ctx, w, fromOpenID, toUserName, content, timestamp, nonce, encrypted)
	case "voice":
		// Voice with recognition is handled the same way as text; the immediate passive reply has already been written.
		h.handleVoiceMessage(ctx, w, fromOpenID, toUserName, recognition, timestamp, nonce, encrypted)
	default:
		reply(wxmpReplyUnsupported)
	}
}

func (h *Handler) writeReply(ctx context.Context, w http.ResponseWriter, toUser, fromUser, content, timestamp, nonce string, encrypted bool) {
	xml := ReplyTextXML(toUser, fromUser, content)
	if !encrypted {
		writeXML(w, xml)
		return
	}
	encXML, err := EncryptReplyXML(toUser, xml, h.cfg.WechatMPAppID, h.cfg.WechatEncodingAESKey, h.cfg.WechatMsgToken, timestamp, nonce)
	if err != nil {
		slog.ErrorContext(ctx, "encrypt wx mp reply failed", slog.Any("error", err))
		writeXMLError(w)
		return
	}
	writeXML(w, encXML)
}

func (h *Handler) isDuplicateMessage(ctx context.Context, msgID string) (bool, error) {
	if msgID == "" {
		return false, nil
	}
	if h.rdb == nil {
		return false, nil
	}
	key := "wxmp:msgid:" + msgID
	set, err := h.rdb.SetNX(ctx, key, "1", wxmpMsgIDCacheTTL).Result()
	if err != nil {
		return false, err
	}
	return !set, nil
}

func (h *Handler) handleEvent(ctx context.Context, openID, event string) string {
	switch event {
	case "subscribe":
		// resolveUser 可能含微信 FetchUserInfo HTTP，同步执行会超过微信 5s 被动回复窗口；
		// 移到后台执行，先立即返回欢迎语，避免微信重试导致重复处理。
		safe.Go(context.WithoutCancel(ctx), nil, func() {
			bgCtx := context.WithoutCancel(ctx)
			if _, err := h.resolveUser(bgCtx, openID); err != nil {
				slog.WarnContext(bgCtx, "resolve user on subscribe failed", slog.String("openid", util.MaskID(openID)), slog.Any("error", err))
			}
		})
		// 显式标记订阅状态，避免 resolveUser 走本地缓存路径时不更新 subscribed
		if err := h.pool.Queries().UpdateWxMPAccountSubscribeStatus(ctx, sqlc.UpdateWxMPAccountSubscribeStatusParams{
			MpOpenid:   openID,
			Subscribed: true,
		}); err != nil {
			slog.ErrorContext(ctx, "update wx mp subscribe status on subscribe failed", slog.String("openid", util.MaskID(openID)), slog.Any("error", err))
		}
		// 异步推送小程序卡片，引导用户打开小程序完成注册绑定
		h.sendMiniProgramCardAsync(ctx, openID, "点击打开小程序")
		return wxmpReplyWelcome
	case "unsubscribe":
		if err := h.pool.Queries().UpdateWxMPAccountSubscribeStatus(ctx, sqlc.UpdateWxMPAccountSubscribeStatusParams{
			MpOpenid:   openID,
			Subscribed: false,
		}); err != nil {
			slog.ErrorContext(ctx, "update wx mp subscribe status failed", slog.String("openid", util.MaskID(openID)), slog.Any("error", err))
		}
		return ""
	default:
		return ""
	}
}

func (h *Handler) sendMiniProgramCardAsync(ctx context.Context, openID, title string) {
	safe.Go(ctx, nil, func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		thumbMediaID, thumbErr := h.getThumbMediaID(bgCtx)
		if thumbErr != nil {
			slog.ErrorContext(bgCtx, "get thumb media id failed", slog.String("openid", util.MaskID(openID)), slog.Any("error", thumbErr))
			return
		}
		if sendErr := h.wxClient.SendMiniProgramPage(bgCtx, openID, h.cfg.WechatAppID, wxmpMiniProgramPath, title, thumbMediaID); sendErr != nil {
			slog.ErrorContext(bgCtx, "send mini program card failed", slog.String("openid", util.MaskID(openID)), slog.Any("error", sendErr))
		}
	})
}

// handleTextMessage writes an empty passive reply immediately and streams the AI answer
// back in segments via WeChat customer service messages.
func (h *Handler) handleTextMessage(ctx context.Context, w http.ResponseWriter, fromOpenID, toUserName, content, timestamp, nonce string, encrypted bool) {
	msg := strings.TrimSpace(content)
	if msg == "" {
		h.writeReply(ctx, w, fromOpenID, toUserName, wxmpReplyUnsupported, timestamp, nonce, encrypted)
		return
	}

	// 先立即写非空被动回复（避免微信因空内容重试导致后台 AI 被多次触发），
	// 满足微信 5s 被动回复窗口；resolveUser 可能含微信 FetchUserInfo HTTP，放到后台执行。
	// 完整答案仍通过客服消息异步分段推送；未绑定用户改为异步发引导 + 小程序卡片。
	h.writeReply(ctx, w, fromOpenID, toUserName, wxmpReplyThinking, timestamp, nonce, encrypted)

	safe.Go(ctx, nil, func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), ai.AIStreamTimeout+30*time.Second)
		defer cancel()

		user, err := h.resolveUser(bgCtx, fromOpenID)
		if err != nil {
			slog.WarnContext(bgCtx, "resolve wx mp user failed", slog.String("openid", util.MaskID(fromOpenID)), slog.Any("error", err))
			//nolint:errcheck
			_ = h.wxClient.SendKfMessage(bgCtx, fromOpenID, wxmpReplyNoUser)
			h.sendMiniProgramCardAsync(bgCtx, fromOpenID, "点击小程序授权")
			return
		}

		sysCfg, err := h.sysCfgLoader.Load(bgCtx)
		if err != nil {
			slog.ErrorContext(bgCtx, "wx mp load sys config failed", slog.String("openid", util.MaskID(fromOpenID)), slog.Any("error", err))
			//nolint:errcheck
			_ = h.wxClient.SendKfMessage(bgCtx, fromOpenID, wxmpReplyError)
			return
		}

		var buf strings.Builder
		flushBuf := func(force bool) {
			text := buf.String()
			if text == "" {
				return
			}
			if !force && len(text) < wxmpKfTextByteLimit {
				return
			}
			buf.Reset()
			// 按微信客服消息字节上限拆分，避免单条消息超限被拒收。
			segments := splitWeChatText(stripMarkdown(text), wxmpKfTextByteLimit)
			var totalSegments, failedSegments int
			for _, seg := range segments {
				if seg == "" {
					continue
				}
				totalSegments++
				if err := h.wxClient.SendKfMessage(bgCtx, fromOpenID, seg); err != nil {
					failedSegments++
					slog.ErrorContext(bgCtx, "wx mp send kf segment failed", slog.String("openid", util.MaskID(fromOpenID)), slog.Int("len", utf8.RuneCountInString(seg)), slog.Int("bytes", len(seg)), slog.Any("error", err))
				}
			}
			if totalSegments > 0 && failedSegments == totalSegments {
				slog.ErrorContext(bgCtx, "[ALERT] wx mp all kf segments failed", slog.String("openid", util.MaskID(fromOpenID)), slog.Int("total_segments", totalSegments))
			}
		}

		_, err = h.aiService.ChatWithPromptUsingConfig(bgCtx, user.ID, msg, func(chunk string) error {
			buf.WriteString(chunk)
			flushBuf(false)
			return nil
		}, sysCfg.WechatMPPrompt, sysCfg)

		if err != nil {
			slog.ErrorContext(bgCtx, "wx mp ai chat failed", slog.String("user_id", user.ID), slog.String("openid", util.MaskID(fromOpenID)), slog.Any("error", err))
			if buf.Len() > 0 {
				flushBuf(true)
			}
			replyMsg := wxmpReplyError
			if stderrors.Is(err, ai.ErrAIDailyQuotaExceeded) {
				replyMsg = wxmpReplyQuotaExceeded
			}
			//nolint:errcheck
			_ = h.wxClient.SendKfMessage(bgCtx, fromOpenID, replyMsg)
			return
		}

		flushBuf(true)
	})
}

var (
	stripMarkdownCodeBlockRE  = regexp.MustCompile("(?s)```.*?```")
	stripMarkdownBoldAsterRE  = regexp.MustCompile(`\*\*(.*?)\*\*`)
	stripMarkdownBoldUnderRE  = regexp.MustCompile(`__(.*?)__`)
	stripMarkdownItalicAstRE  = regexp.MustCompile(`\*(.*?)\*`)
	stripMarkdownItalicUndRE  = regexp.MustCompile(`_(.*?)_`)
	stripMarkdownInlineCodeRE = regexp.MustCompile("`([^`]+)`")
	stripMarkdownHeadingRE    = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	stripMarkdownBlockquoteRE = regexp.MustCompile(`(?m)^>\s+`)
	stripMarkdownUnorderedRE  = regexp.MustCompile(`(?m)^[-*+]\s+`)
	stripMarkdownOrderedRE    = regexp.MustCompile(`(?m)^\d+\.\s+`)
	stripMarkdownLinkRE       = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	stripMarkdownImageRE      = regexp.MustCompile(`!\[([^\]]+)\]\([^)]+\)`)
)

// stripMarkdown removes common Markdown formatting so the text displays cleanly
// in WeChat official account chat bubbles.
func stripMarkdown(text string) string {
	text = stripMarkdownCodeBlockRE.ReplaceAllString(text, "")
	text = stripMarkdownBoldAsterRE.ReplaceAllString(text, "$1")
	text = stripMarkdownBoldUnderRE.ReplaceAllString(text, "$1")
	text = stripMarkdownItalicAstRE.ReplaceAllString(text, "$1")
	text = stripMarkdownItalicUndRE.ReplaceAllString(text, "$1")
	text = stripMarkdownInlineCodeRE.ReplaceAllString(text, "$1")
	text = stripMarkdownHeadingRE.ReplaceAllString(text, "")
	text = stripMarkdownBlockquoteRE.ReplaceAllString(text, "")
	text = stripMarkdownUnorderedRE.ReplaceAllString(text, "")
	text = stripMarkdownOrderedRE.ReplaceAllString(text, "")
	text = stripMarkdownLinkRE.ReplaceAllString(text, "$1")
	text = stripMarkdownImageRE.ReplaceAllString(text, "$1")
	return strings.TrimSpace(text)
}

// handleVoiceMessage handles voice messages by using WeChat's speech recognition result as input.
func (h *Handler) handleVoiceMessage(ctx context.Context, w http.ResponseWriter, fromOpenID, toUserName, recognition, timestamp, nonce string, encrypted bool) {
	text := strings.TrimSpace(recognition)
	if text == "" {
		h.writeReply(ctx, w, fromOpenID, toUserName, wxmpReplyVoiceEmpty, timestamp, nonce, encrypted)
		return
	}

	h.handleTextMessage(ctx, w, fromOpenID, toUserName, text, timestamp, nonce, encrypted)
}

func writeXML(w http.ResponseWriter, xml string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml)) //nolint:errcheck
}

func writeXMLError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("success")) //nolint:errcheck
}

// getThumbMediaID returns a cached WeChat temporary thumb media id for mini-program page cards.
// It uploads the default cover image and caches the media id for 48 hours.
func (h *Handler) getThumbMediaID(ctx context.Context) (string, error) {
	if h.rdb != nil {
		cached, err := h.rdb.Get(ctx, wxmpThumbCacheKey).Result()
		if err == nil && cached != "" {
			return cached, nil
		}
		if err != nil && err != goredis.Nil {
			slog.WarnContext(ctx, "read wx mp thumb media id cache failed", slog.Any("error", err))
		}
	}

	const thumbPath = "/app/assets/wxmp_thumb.jpg"
	data, err := os.ReadFile(thumbPath)
	if err != nil {
		return "", fmt.Errorf("read thumb file %s: %w", thumbPath, err)
	}

	mediaID, err := h.wxClient.UploadTempMedia(ctx, "thumb", "wxmp_thumb.jpg", data)
	if err != nil {
		return "", err
	}

	if h.rdb != nil {
		if err := h.rdb.Set(ctx, wxmpThumbCacheKey, mediaID, wxmpThumbCacheTTL).Err(); err != nil {
			slog.WarnContext(ctx, "cache wx mp thumb media id failed", slog.Any("error", err))
		}
	}

	return mediaID, nil
}

func (h *Handler) resolveUser(ctx context.Context, openID string) (*sqlc.GetUserByIDRow, error) {
	if openID == "" {
		return nil, pgx.ErrNoRows
	}

	// 1. Try local binding first.
	mpAcc, err := h.pool.Queries().GetWxMPAccountByOpenID(ctx, openID)
	if err == nil && mpAcc.UserID.Valid {
		if touchErr := h.pool.Queries().TouchWxMPAccountInteractTime(ctx, openID); touchErr != nil {
			slog.WarnContext(ctx, "touch wxmp interact time failed", slog.String("open_id", openID), slog.Any("error", touchErr))
		}
		user, userErr := h.pool.Queries().GetUserByID(ctx, mpAcc.UserID.String)
		if userErr == nil {
			h.refreshMPAccountAsync(ctx, openID)
			return &user, nil
		}
	}
	if err != nil && err != pgx.ErrNoRows {
		slog.WarnContext(ctx, "get wx mp account by openid failed", slog.String("openid", util.MaskID(openID)), slog.Any("error", err))
	}

	// 2. Fetch from WeChat to obtain unionid.
	if !h.wxClient.IsConfigured() {
		slog.WarnContext(ctx, "wx mp client not configured", slog.String("openid", util.MaskID(openID)))
		return nil, pgx.ErrNoRows
	}

	info, err := h.wxClient.FetchUserInfo(ctx, openID)
	if err != nil {
		slog.ErrorContext(ctx, "fetch wx mp user info failed", slog.String("openid", util.MaskID(openID)), slog.Any("error", err))
		return nil, err
	}

	if info.UnionID == "" {
		slog.WarnContext(ctx, "wx mp user info has no unionid", slog.String("openid", util.MaskID(openID)))
		return nil, pgx.ErrNoRows
	}

	// 3. Find global user by unionid.
	user, findErr := h.pool.Queries().GetUserByUnionID(ctx, pgtype.Text{String: info.UnionID, Valid: true})
	userFound := findErr == nil
	if findErr != nil && findErr != pgx.ErrNoRows {
		return nil, findErr
	}

	// 4. Upsert service account record (with or without user_id).
	id, idErr := util.NewUUID()
	if idErr != nil {
		return nil, idErr
	}

	subscribeTime := pgtype.Timestamptz{}
	if info.Subscribe && info.SubscribeTime > 0 {
		subscribeTime = pgtype.Timestamptz{Time: time.Unix(info.SubscribeTime, 0), Valid: true}
	}

	var userID pgtype.Text
	if userFound {
		userID = pgtype.Text{String: user.ID, Valid: true}
	}

	_, upsertErr := h.pool.Queries().UpsertWxMPAccount(ctx, sqlc.UpsertWxMPAccountParams{
		ID:               id,
		UserID:           userID,
		MpOpenid:         openID,
		Unionid:          pgtype.Text{String: info.UnionID, Valid: true},
		Nickname:         pgtype.Text{String: info.Nickname, Valid: info.Nickname != ""},
		Avatar:           pgtype.Text{String: info.Avatar, Valid: info.Avatar != ""},
		Subscribed:       info.Subscribe,
		SubscribeTime:    subscribeTime,
		LastInteractTime: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if upsertErr != nil {
		slog.ErrorContext(ctx, "upsert wx mp account failed", slog.String("openid", util.MaskID(openID)), slog.String("unionid", util.MaskID(info.UnionID)), slog.Any("error", upsertErr))
		if !userFound {
			return nil, upsertErr
		}
	}

	if !userFound {
		return nil, pgx.ErrNoRows
	}

	fullUser, getErr := h.pool.Queries().GetUserByID(ctx, user.ID)
	if getErr != nil {
		return nil, getErr
	}
	return &fullUser, nil
}

// refreshMPAccountAsync asynchronously refreshes subscribe status and profile from WeChat.
// This keeps the local wx_mp_accounts record in sync without blocking the current request.
func (h *Handler) refreshMPAccountAsync(ctx context.Context, openID string) {
	if !h.wxClient.IsConfigured() {
		return
	}
	// 节流：同一 openID 每 1 小时最多异步刷新一次微信资料，避免每条消息都打微信 FetchUserInfo。
	if h.rdb != nil {
		ok, err := h.rdb.SetNX(ctx, wxmpProfileRefreshKeyPrefix+openID, "1", wxmpProfileRefreshTTL).Result()
		if err != nil {
			slog.WarnContext(ctx, "wxmp profile refresh throttle check failed", slog.String("openid", util.MaskID(openID)), slog.Any("error", err))
		} else if !ok {
			return
		}
	}
	safe.Go(ctx, nil, func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		info, err := h.wxClient.FetchUserInfo(bgCtx, openID)
		if err != nil {
			slog.WarnContext(bgCtx, "async refresh wx mp user info failed", slog.String("openid", util.MaskID(openID)), slog.Any("error", err))
			return
		}
		subscribeTime := pgtype.Timestamptz{}
		if info.Subscribe && info.SubscribeTime > 0 {
			subscribeTime = pgtype.Timestamptz{Time: time.Unix(info.SubscribeTime, 0), Valid: true}
		}
		var userID pgtype.Text
		if info.UnionID != "" {
			mpAcc, accErr := h.pool.Queries().GetWxMPAccountByOpenID(bgCtx, openID)
			if accErr == nil && !mpAcc.UserID.Valid {
				user, findErr := h.pool.Queries().GetUserByUnionID(bgCtx, pgtype.Text{String: info.UnionID, Valid: true})
				if findErr == nil {
					userID = pgtype.Text{String: user.ID, Valid: true}
				}
			}
		}
		id, idErr := util.NewUUID()
		if idErr != nil {
			slog.ErrorContext(bgCtx, "generate wx mp account id failed", slog.Any("error", idErr))
			return
		}
		_, upsertErr := h.pool.Queries().UpsertWxMPAccount(bgCtx, sqlc.UpsertWxMPAccountParams{
			ID:               id,
			UserID:           userID,
			MpOpenid:         openID,
			Unionid:          pgtype.Text{String: info.UnionID, Valid: info.UnionID != ""},
			Nickname:         pgtype.Text{String: info.Nickname, Valid: info.Nickname != ""},
			Avatar:           pgtype.Text{String: info.Avatar, Valid: info.Avatar != ""},
			Subscribed:       info.Subscribe,
			SubscribeTime:    subscribeTime,
			LastInteractTime: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		})
		if upsertErr != nil {
			slog.WarnContext(bgCtx, "async refresh upsert wx mp account failed", slog.String("openid", util.MaskID(openID)), slog.Any("error", upsertErr))
		}
	})
}

// splitWeChatText splits text into parts where each part is within byteLimit bytes.
// It tries to split at newline boundaries and avoids breaking UTF-8 runes.
func splitWeChatText(text string, byteLimit int) []string {
	if len(text) <= byteLimit {
		return []string{text}
	}

	var parts []string
	remaining := text
	for len(remaining) > byteLimit {
		chunk := remaining[:byteLimit]
		// Try to find the last newline to keep messages readable.
		if idx := strings.LastIndex(chunk, "\n"); idx > 0 {
			chunk = chunk[:idx]
		} else {
			// Fallback: ensure we don't split a UTF-8 rune.
			for len(chunk) > 0 && !utf8.ValidString(chunk) {
				chunk = chunk[:len(chunk)-1]
			}
		}
		if len(chunk) == 0 {
			// Defensive: if chunk becomes empty, force cut at rune boundary.
			r, size := utf8.DecodeRuneInString(remaining)
			chunk = string(r)
			remaining = remaining[size:]
		} else {
			remaining = remaining[len(chunk):]
		}
		parts = append(parts, strings.TrimSpace(chunk))
	}
	if strings.TrimSpace(remaining) != "" {
		parts = append(parts, strings.TrimSpace(remaining))
	}
	return parts
}
