// Package invite provides related functionality.
package invite

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/file"
	dbx "papafeiji/backend/pkg/db"
	"papafeiji/backend/pkg/util"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
	xdraw "golang.org/x/image/draw"
)

// InviteQR 生成结果：合成海报与分享缩略图的存储 URL。
// thumbUrl 服务于微信 OpenSDK shareImageMessage 的 64KB 缩略图硬约束
// （超限报 sendOpenReq:fail:check args fail, thumbData.size: N）。
type InviteQR struct {
	URL      string `json:"url"`
	ThumbURL string `json:"thumbUrl"`
}

// parseInviteQRCache 解析 Redis 缓存值；解析失败视为过期，由调用方重新生成。
func parseInviteQRCache(cached string) (InviteQR, bool) {
	var qr InviteQR
	if err := json.Unmarshal([]byte(cached), &qr); err != nil || qr.URL == "" {
		return InviteQR{}, false
	}
	return qr, true
}

const (
	inviteQRCodeCacheKey    = "invite:qrcode:%s"
	inviteQRCodeCacheTTL    = 6 * 24 * time.Hour
	inviteQRCodeTargetRatio = 0.20
	inviteQRCodeMargin      = 40
	inviteQRCodeInnerOffset = 300 // 二维码距右下角的额外内缩像素
	inviteShortCodeLen      = 8
)

type WechatClient interface {
	GetAccessToken(ctx context.Context) (string, error)
	ClearAccessToken(ctx context.Context)
}

type QRCodeGenerator struct {
	wechat      WechatClient
	pool        *db.Pool
	storage     *file.Storage
	bgImagePath string
}

func NewQRCodeGenerator(wechat WechatClient, pool *db.Pool, storage *file.Storage, bgImagePath string) *QRCodeGenerator {
	return &QRCodeGenerator{
		wechat:      wechat,
		pool:        pool,
		storage:     storage,
		bgImagePath: bgImagePath,
	}
}

func (g *QRCodeGenerator) Generate(ctx context.Context, rdb *redis.Client, userID string) (InviteQR, error) {
	return g.generate(ctx, rdb, userID, false)
}

func (g *QRCodeGenerator) GenerateRaw(ctx context.Context, rdb *redis.Client, userID string) (InviteQR, error) {
	return g.generate(ctx, rdb, userID, true)
}

func (g *QRCodeGenerator) generate(ctx context.Context, rdb *redis.Client, userID string, raw bool) (InviteQR, error) {
	if userID == "" {
		return InviteQR{}, fmt.Errorf("user id is empty")
	}

	cacheKey := fmt.Sprintf(inviteQRCodeCacheKey, userID)
	if raw {
		cacheKey += ":raw"
	}
	if rdb != nil {
		cached, err := rdb.Get(ctx, cacheKey).Result()
		if err == nil && cached != "" {
			if qr, ok := parseInviteQRCache(cached); ok {
				return qr, nil
			}
		}
		if err != nil && err != redis.Nil {
			slog.WarnContext(ctx, "read invite qrcode cache failed", slog.String("user_id", userID), slog.Bool("raw", raw), slog.Any("error", err))
		}

		// 并发/快速重复调用会互删对方刚创建的文件（并缓存指向已删文件的 URL 6 天），
		// 用按用户的 Redis 锁串行化生成：未抢到锁时短暂等待缓存落盘后直接返回。
		lockKey := "invite:qrcode:gen:" + userID
		lockClient := db.NewAdvisoryLock(g.pool.PGX())
		ok, token, lockErr := lockClient.TryLock(ctx, lockKey)
		if lockErr == nil && ok {
			defer lockClient.Unlock(context.WithoutCancel(ctx), lockKey, token) //nolint:errcheck
		} else if lockErr == nil {
			for i := 0; i < 10; i++ {
				select {
				case <-ctx.Done():
					return InviteQR{}, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				if cached, cerr := rdb.Get(ctx, cacheKey).Result(); cerr == nil && cached != "" {
					if qr, ok := parseInviteQRCache(cached); ok {
						return qr, nil
					}
				}
			}
			slog.WarnContext(ctx, "invite qrcode generation lock not acquired, falling through", slog.String("user_id", userID), slog.Bool("raw", raw))
		} else {
			slog.WarnContext(ctx, "invite qrcode lock error, falling through", slog.String("user_id", userID), slog.Bool("raw", raw), slog.Any("error", lockErr))
		}
	}

	shortCode, err := g.ensureShortCode(ctx, g.pool.Queries(), userID)
	if err != nil {
		return InviteQR{}, fmt.Errorf("ensure short code: %w", err)
	}

	user, err := g.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		return InviteQR{}, fmt.Errorf("get user: %w", err)
	}
	familyID := util.ToString(user.CurrentFamilyID)
	if familyID == "" {
		familyID = util.ToString(user.PersonalFamilyID)
	}
	rawStr := "false"
	if raw {
		rawStr = "true"
	}
	metadata, err := json.Marshal(map[string]string{
		"family_id": familyID,
		"raw":       rawStr,
	})
	if err != nil {
		return InviteQR{}, fmt.Errorf("marshal qrcode metadata: %w", err)
	}

	qrBytes, err := g.fetchWxaCode(ctx, shortCode)
	if err != nil {
		return InviteQR{}, fmt.Errorf("fetch wxa code: %w", err)
	}

	var imageBytes []byte
	var suffix string
	if raw {
		imageBytes = qrBytes
		suffix = "png"
	} else {
		imageBytes, err = g.composite(qrBytes)
		if err != nil {
			return InviteQR{}, fmt.Errorf("composite invite image: %w", err)
		}
		suffix = "jpg"
	}

	fileName := shortCode + "." + suffix
	key, storageType, size, err := g.storage.SaveSystemWithName(bytes.NewReader(imageBytes), "."+suffix, shortCode)
	if err != nil {
		return InviteQR{}, fmt.Errorf("save invite image: %w", err)
	}

	fileID, err := util.NewUUID()
	if err != nil {
		return InviteQR{}, fmt.Errorf("generate invite file id: %w", err)
	}

	// 事务内查询旧文件、创建新文件记录并删除旧文件记录；物理文件删除在 DB 提交成功后进行。
	// 旧文件列表在事务内读取，避免并发生成时误删刚创建的新二维码。
	var oldFiles []sqlc.ListUserInviteQRCodeFilesRow
	if err := db.WithTx(ctx, g.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		oldFiles, err = q.ListUserInviteQRCodeFiles(ctx, sqlc.ListUserInviteQRCodeFilesParams{
			CreatedBy: pgtype.Text{String: userID, Valid: true},
			Metadata:  []byte(rawStr),
		})
		if err != nil {
			return fmt.Errorf("list old invite qrcode files: %w", err)
		}
		if _, err := q.CreateFile(ctx, sqlc.CreateFileParams{
			ID:          fileID,
			CreatedBy:   pgtype.Text{String: userID, Valid: true},
			Path:        key,
			Name:        fileName,
			Suffix:      suffix,
			SizeBytes:   size,
			FileType:    "system",
			StorageType: storageType,
			Metadata:    metadata,
		}); err != nil {
			return fmt.Errorf("record invite qrcode file: %w", err)
		}
		if len(oldFiles) > 0 {
			oldIDs := make([]string, 0, len(oldFiles))
			for _, f := range oldFiles {
				oldIDs = append(oldIDs, f.ID)
			}
			if _, err := q.BatchDeleteFiles(ctx, oldIDs); err != nil {
				return fmt.Errorf("delete old invite qrcode file records: %w", err)
			}
		}
		return nil
	}); err != nil {
		return InviteQR{}, err
	}

	// DB 提交成功后再清理旧物理文件；清理失败可接受少量孤儿文件。
	// 并发生成时新旧文件路径相同（SaveSystemWithName 按 月份+shortCode 确定性命名），
	// 删掉同路径"旧"文件会误删刚写入的新文件，需跳过。
	for _, f := range oldFiles {
		if f.Path == key {
			continue
		}
		if delErr := g.storage.DeleteFile(f.Path, f.StorageType); delErr != nil {
			slog.WarnContext(ctx, "delete old invite qrcode physical file failed", slog.String("path", f.Path), slog.String("user_id", userID), slog.Any("error", delErr))
		}
	}

	imageURL, urlErr := g.storage.URL(key, storageType)
	if urlErr != nil {
		return InviteQR{}, fmt.Errorf("get invite qrcode url: %w", urlErr)
	}

	// 分享缩略图：OpenSDK thumbData ≤64KB 硬约束，由服务端确定性产出
	// 240px 宽、≤60KB 的 JPEG。失败降级为 thumbUrl 为空（客户端回退压缩原图）。
	// 缩略图不建文件记录：路径按 月份+短码-thumb 确定性命名，当月重生成覆盖写。
	// 无 files 行即不在任何清理路径覆盖内（行扫描任务与注销按行收集均不可见），
	// 跨月遗留为纯存储孤儿（单个仅数十 KB，已接受）；海报行则被系统文件清理豁免
	//（ScanOldSystemFiles NOT metadata ? 'raw'，02d §8.3），回收走重生成替换与注销清理。
	thumbURL := ""
	if !raw {
		if thumbBytes, err := makeInviteThumb(imageBytes); err != nil {
			slog.WarnContext(ctx, "make invite thumb failed", slog.String("user_id", userID), slog.Any("error", err))
		} else if thumbKey, thumbStorageType, _, err := g.storage.SaveSystemWithName(bytes.NewReader(thumbBytes), ".jpg", shortCode+"-thumb"); err != nil {
			slog.WarnContext(ctx, "save invite thumb failed", slog.String("user_id", userID), slog.Any("error", err))
		} else if tu, urlErr := g.storage.URL(thumbKey, thumbStorageType); urlErr != nil {
			slog.WarnContext(ctx, "invite thumb url failed", slog.String("user_id", userID), slog.Any("error", urlErr))
		} else {
			thumbURL = tu
		}
	}

	if rdb != nil {
		if payload, mErr := json.Marshal(map[string]string{"url": imageURL, "thumbUrl": thumbURL}); mErr == nil {
			if err := rdb.Set(ctx, cacheKey, string(payload), inviteQRCodeCacheTTL).Err(); err != nil {
				slog.WarnContext(ctx, "cache invite qrcode url failed", slog.String("user_id", userID), slog.Bool("raw", raw), slog.Any("error", err))
			}
		} else {
			slog.WarnContext(ctx, "marshal invite qrcode cache failed", slog.String("user_id", userID), slog.Bool("raw", raw), slog.Any("error", mErr))
		}
	}

	return InviteQR{URL: imageURL, ThumbURL: thumbURL}, nil
}

// makeInviteThumb 将海报缩到 240px 宽并迭代降质到 ≤60KB（OpenSDK 64KB 上限留余量）。
func makeInviteThumb(poster []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(poster))
	if err != nil {
		return nil, fmt.Errorf("decode poster: %w", err)
	}
	bounds := src.Bounds()
	for _, width := range []int{240, 180, 140} {
		if width > bounds.Dx() {
			continue
		}
		height := bounds.Dy() * width / bounds.Dx()
		if height < 1 {
			height = 1
		}
		dst := image.NewRGBA(image.Rect(0, 0, width, height))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, xdraw.Over, nil)
		for _, quality := range []int{80, 65, 50, 40} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
				return nil, fmt.Errorf("encode thumb: %w", err)
			}
			if buf.Len() <= 60*1024 {
				return buf.Bytes(), nil
			}
		}
	}
	return nil, fmt.Errorf("thumb exceeds 60KB even at minimum size")
}

func (g *QRCodeGenerator) ensureShortCode(ctx context.Context, q *sqlc.Queries, userID string) (string, error) {
	existing, err := q.GetUserInviteCode(ctx, userID)
	if err == nil && existing != "" {
		return existing, nil
	}
	if err != nil && !isPgNoRows(err) {
		return "", fmt.Errorf("get user invite code: %w", err)
	}

	// 短码全局唯一，碰撞需换码重试（最长 3 次）；若冲突来自 user_id 唯一键，
	// 说明本人已有码，直接复用。
	const maxCodeAttempts = 3
	for attempt := 0; attempt < maxCodeAttempts; attempt++ {
		code, err := util.NewShortCode(inviteShortCodeLen)
		if err != nil {
			return "", fmt.Errorf("generate short code: %w", err)
		}
		if _, dbErr := q.CreateUserInviteCode(ctx, sqlc.CreateUserInviteCodeParams{
			UserID:    userID,
			ShortCode: code,
		}); dbErr != nil {
			if dbx.IsUniqueViolation(dbErr) {
				existing, getErr := q.GetUserInviteCode(ctx, userID)
				if getErr == nil && existing != "" {
					return existing, nil
				}
				if getErr != nil && !isPgNoRows(getErr) {
					return "", fmt.Errorf("get user invite code after conflict: %w", getErr)
				}
				// 冲突在 short_code：换一个短码重试。
				continue
			}
			return "", fmt.Errorf("create user invite code: %w", dbErr)
		}
		return code, nil
	}
	return "", fmt.Errorf("generate unique invite code: exhausted %d attempts", maxCodeAttempts)
}

func isPgNoRows(err error) bool {
	return stderrors.Is(err, pgx.ErrNoRows)
}

func (g *QRCodeGenerator) fetchWxaCode(ctx context.Context, scene string) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			g.wechat.ClearAccessToken(ctx)
		}

		token, err := g.wechat.GetAccessToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("get access token: %w", err)
		}

		u := fmt.Sprintf("https://api.weixin.qq.com/wxa/getwxacodeunlimit?access_token=%s", url.QueryEscape(token))
		payload, err := json.Marshal(map[string]interface{}{
			"scene":      scene,
			"page":       "pages/index/index",
			"is_hyaline": true,
			"width":      800,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal wxacode request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		httpClient := config.HTTPClient()
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request wxa code: %w", err)
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		resp.Body.Close() //nolint:errcheck
		if err != nil {
			return nil, fmt.Errorf("read wxa code response: %w", err)
		}

		if len(body) > 0 && body[0] == '{' {
			var result struct {
				ErrCode int    `json:"errcode"`
				ErrMsg  string `json:"errmsg"`
			}
			if err := json.Unmarshal(body, &result); err == nil && result.ErrCode != 0 {
				if (result.ErrCode == 40001 || result.ErrCode == 42001) && attempt == 0 {
					continue
				}
				return nil, fmt.Errorf("wxa code error: code=%d msg=%s", result.ErrCode, result.ErrMsg)
			}
		}

		if len(body) == 0 {
			return nil, fmt.Errorf("empty wxa code response")
		}
		return body, nil
	}
	return nil, fmt.Errorf("fetch wxa code failed after retry")
}

func (g *QRCodeGenerator) composite(qrBytes []byte) ([]byte, error) {
	bgFile, err := os.Open(g.bgImagePath)
	if err != nil {
		return nil, fmt.Errorf("open background image: %w", err)
	}
	//nolint:errcheck
	defer bgFile.Close()

	bgImg, _, err := image.Decode(bgFile)
	if err != nil {
		return nil, fmt.Errorf("decode background image: %w", err)
	}

	qrImg, _, err := image.Decode(bytes.NewReader(qrBytes))
	if err != nil {
		return nil, fmt.Errorf("decode qrcode image: %w", err)
	}

	bounds := bgImg.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	targetSize := int(float64(width) * inviteQRCodeTargetRatio)
	if targetSize < 120 {
		targetSize = 120
	}
	scaledQR := resizeNearest(qrImg, targetSize, targetSize)

	dst := image.NewRGBA(bounds)
	draw.Draw(dst, bounds, bgImg, bounds.Min, draw.Src)

	margin := inviteQRCodeMargin
	offset := inviteQRCodeInnerOffset
	x := width - scaledQR.Bounds().Dx() - margin - offset
	y := height - scaledQR.Bounds().Dy() - margin - offset
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	draw.Draw(dst, scaledQR.Bounds().Add(image.Pt(x, y)), scaledQR, image.Point{}, draw.Over)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90}); err != nil {
		return nil, fmt.Errorf("encode composite image: %w", err)
	}
	return buf.Bytes(), nil
}

func resizeNearest(src image.Image, w, h int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	bounds := src.Bounds()
	sw := bounds.Dx()
	sh := bounds.Dy()
	for y := 0; y < h; y++ {
		sy := bounds.Min.Y + y*sh/h
		for x := 0; x < w; x++ {
			sx := bounds.Min.X + x*sw/w
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

func ResolveInviterFromCode(ctx context.Context, q *sqlc.Queries, code string) (string, error) {
	if code == "" {
		return "", nil
	}
	code = strings.ToUpper(strings.TrimSpace(code))

	userID, err := q.ResolveInviterFromCode(ctx, code)
	if err != nil {
		if isPgNoRows(err) {
			return "", nil
		}
		return "", fmt.Errorf("resolve inviter from code: %w", err)
	}
	return userID, nil
}
